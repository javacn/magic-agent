package agent

// arkclaw.go - ArkClaw 引擎（A2A JSON-RPC 网关，HTTP 而非本机 CLI）。
//
// 与其余引擎最大的不同：arkclaw 不在本机，而是一个 A2A（Agent-to-Agent）
// 网关。magic-agent 把 prompt 组成一个 JSON-RPC `message/send` 请求 POST
// 过去，网关侧跑它自己的 agent，再把结果包成 A2A Task 返回来。
//
// 端点与凭据（url / key / claw_id）不写死在代码里，走本地配置文件
// ~/.config/magic-agent/config.json 的 "arkclaw" 节（见 internal/config）；
// 环境变量 MAGIC_AGENT_ARKCLAW_URL / _KEY / _CLAW_ID 可覆盖文件值，
// 引擎结构体上的显式字段又可覆盖前两者（测试注入用）。
//
// 请求（HTTP POST，凭据走 query 参数）：
//
//	POST <url>?apikey=<key>&clawId=<clawId>
//	content-type: application/json
//
//	{"jsonrpc":"2.0","id":"<uuid>","method":"message/send","params":{
//	   "message":{"kind":"message","role":"user","messageId":"<uuid>",
//	              "contextId":"<可选：续接上一轮>",
//	              "parts":[{"kind":"text","text":"<prompt>"}]}}}
//
// 响应（HTTP 200 + JSON-RPC 2.0 envelope）：
//
//	{"jsonrpc":"2.0","id":"<同上>","result":{
//	   "kind":"task","id":"<task-id>","contextId":"<上下文 id>",
//	   "status":{"state":"completed",
//	             "message":{"parts":[{"kind":"text","text":"<正文>"}]}},
//	   "history":[...],
//	   "artifacts":[{"artifactId":"...","parts":[{"kind":"text","text":"..."}]}]}}
//
// 取文优先级：result.status.message.parts[].text（任务的最终答复）→
// result.artifacts[].parts[].text（网关把产物挂在产物区时兜底）。
//
// 续接（实测结论，2026-09-16）：
// contextId 必须放在 **message 对象内部**。放到 params.contextId（外层）
// 会被网关忽略并另开一个上下文 —— 表现为 agent 完全记不起上一轮。
// 因此：
//   - Request.SessionID 非空 → 回填到 message.contextId（续接）
//   - Response.SessionID ← result.contextId（留给下一轮）
//   - Request.Continue 不支持：A2A 没有「查询最近上下文」的接口，
//     显式报错，引导调用方改用 --session <contextId>。
//
// 鉴权失败：HTTP 401 + text/plain 正文 "External authentication failed."
// 注意这不是 JSON —— 解析必须先看状态码再看 body，不能直接 json.Unmarshal
// 然后报含糊的 "invalid character"。
//
// 参数映射（统一参数矩阵的 arkclaw 列）：
//   - Model：实测 4 种透传方式（query ?model= / params.message.metadata.model /
//     params.message.metadata.model.name / params.configuration.model /
//     X-Model header）都被网关忽略 → 固定路由到 kimi-k2.6。模型是 clawId 绑定的，
//     切换必须改 config.json 的 claw_id（不是 -m）。-m 在 arkclaw 上静默忽略。
//   - MaxTokens / Temperature：协议无对应字段，claw 侧自带控制，-t 之外
//     的采样参数透传无意义 → 静默忽略（与矩阵注 ② trae / codex / openclaw 一致）
//   - Tools：claw 自带工具循环，-y / 白名单都无法逐工具控 → 静默忽略
//   - SystemPrompt：协议无独立 system 角色，展平进 message 头部
//   - JSONSchema：走 llm 引擎同一条「输出后处理抽 JSON」路径
//     (extractJSONObjectStrict)
//   - SessionID：写入 message.contextId（续接；实测放外层 params.contextId 无效）
//   - Continue：A2A 无「查询最近上下文」接口 → 显式拒绝（不放外层那种「静默开新会话」的坑）
//
// 不实现 Stream：A2A 的流式语义是独立的 message/stream 方法（SSE 事件流），
// 与本项目现有 Streamer 契约（逐行 NDJSON + 进程 stdout 解析）不是一回事；
// 按需求只接 message/send 单次请求-响应。CLI 层面对 nil Streamer 有明确
// 报错路径（see cli/ask.go）。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/darren/magic-agent/internal/config"
)

// DefaultArkClawTimeout 单次 HTTP 请求默认超时。
// 实测网关单轮往返 8s ~ 23s（agent 侧带工具循环时更久），留足余量。
const DefaultArkClawTimeout = 3 * time.Minute

// maxArkClawBody 单次响应体读取上限，防止异常响应把内存吃满。
const maxArkClawBody = 8 << 20 // 8 MiB

// ArkClawEngine 通过 A2A JSON-RPC 网关实现 Engine。
type ArkClawEngine struct {
	// URL / Key / ClawID 显式覆盖配置文件与环境变量。三者同时给定时以此为准；
	// 生产路径留空、走配置文件。测试注入用。
	URL    string
	Key    string
	ClawID string

	// ConfigPath 显式配置文件路径（空 = config.Path() 默认探测链）。测试注入用。
	ConfigPath string

	// HTTPClient 显式 HTTP 客户端（空 = 每请求带 ctx 超时的默认客户端）。测试注入用。
	HTTPClient *http.Client

	// Timeout 单次 HTTP 超时（0 = DefaultArkClawTimeout）。
	Timeout time.Duration
}

// Name 实现 Engine。
func (e *ArkClawEngine) Name() string { return "arkclaw" }

// endpoint 解析生效的 (url, key, clawID)。
//
// 优先级：结构体显式字段 → 环境变量 → 配置文件。
// （环境变量覆盖文件值这一步由 config.LoadFrom 内部完成。）
func (e *ArkClawEngine) endpoint() (string, string, string, error) {
	path := e.ConfigPath
	if strings.TrimSpace(path) == "" {
		path = config.Path()
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		return "", "", "", err
	}
	return firstNonEmpty(e.URL, cfg.ArkClaw.URL),
		firstNonEmpty(e.Key, cfg.ArkClaw.Key),
		firstNonEmpty(e.ClawID, cfg.ArkClaw.ClawID),
		nil
}

// configHint 返回报错时提示的配置文件路径。
func (e *ArkClawEngine) configHint() string {
	if p := strings.TrimSpace(e.ConfigPath); p != "" {
		return p
	}
	if p := config.Path(); p != "" {
		return p
	}
	return "(配置文件路径未知)"
}

// Detect 实现 Engine。
//
// HTTP 引擎没有可执行文件，可用性 = 三项凭据齐备。返回的说明信息用端点
// URL（与 CLI 引擎返回二进制路径同构，供 --engines 展示）。
func (e *ArkClawEngine) Detect() (bool, string) {
	u, k, c, err := e.endpoint()
	if err != nil {
		return false, fmt.Sprintf("arkclaw config error: %v", err)
	}
	var missing []string
	if u == "" {
		missing = append(missing, "url")
	}
	if k == "" {
		missing = append(missing, "key")
	}
	if c == "" {
		missing = append(missing, "claw_id")
	}
	if len(missing) > 0 {
		return false, fmt.Sprintf("arkclaw not configured: missing %s in %s (or set %s / %s / %s)",
			strings.Join(missing, ", "), e.configHint(),
			config.EnvArkClawURL, config.EnvArkClawKey, config.EnvArkClawClawID)
	}
	return true, u
}

// arkClawPart 是 A2A 消息/产物里的内容块。
//
// 取值（A2A 规范）：text（文本）/ file（文件/图片）—— 本项目两者都用：
// 提示词走 text，附件走 file（inline base64，见 arkClawFileContent）。
type arkClawPart struct {
	Kind string              `json:"kind"`
	Text string              `json:"text,omitempty"`
	File *arkClawFileContent `json:"file,omitempty"`
}

// arkClawFileContent 是 A2A file part 的内容载体。
//
// 两种形态二选一：bytes（inline base64，本机文件用它）或 uri（远端 URL）。
// 附件是本机截图 → 只能 inline。
type arkClawFileContent struct {
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Bytes    string `json:"bytes,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// arkClawMessage 是 A2A 的 message 结构（请求与响应共用）。
type arkClawMessage struct {
	Kind      string        `json:"kind"`
	Role      string        `json:"role,omitempty"`
	MessageID string        `json:"messageId,omitempty"`
	ContextID string        `json:"contextId,omitempty"`
	Parts     []arkClawPart `json:"parts,omitempty"`
}

// arkClawRequest 是 message/send 的 JSON-RPC 2.0 请求体。
type arkClawRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  struct {
		Message arkClawMessage `json:"message"`
	} `json:"params"`
}

// arkClawTaskStatus 对应 result.status。
type arkClawTaskStatus struct {
	State   string          `json:"state"`
	Message *arkClawMessage `json:"message"`
}

// arkClawArtifact 对应 result.artifacts[] 的一项。
type arkClawArtifact struct {
	ArtifactID string        `json:"artifactId"`
	Name       string        `json:"name"`
	Parts      []arkClawPart `json:"parts"`
}

// arkClawResult 对应 JSON-RPC envelope 的 result。
type arkClawResult struct {
	Kind      string            `json:"kind"`
	ID        string            `json:"id"`
	ContextID string            `json:"contextId"`
	Status    arkClawTaskStatus `json:"status"`
	Artifacts []arkClawArtifact `json:"artifacts"`
}

// arkClawRPCError 对应 JSON-RPC envelope 的 error。
type arkClawRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// arkClawEnvelope 是网关顶层响应。
type arkClawEnvelope struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      string           `json:"id"`
	Result  *arkClawResult   `json:"result"`
	Error   *arkClawRPCError `json:"error"`
}

// ListModels 实现 ModelLister：A2A 网关**不提供**模型清单。
//
// 模型由网关按 claw_id 绑定（实测 -m 不生效，网关固定用 clawId 对应的模型），
// 因此这里不编造列表，只返回 ErrNoModelSource + 原因，由 --engines 写进
// models_note 告知调用方"换模型得改 claw_id"。
func (e *ArkClawEngine) ListModels(_ context.Context) ([]string, error) {
	return nil, fmt.Errorf("%w: arkclaw binds the model by claw_id at the gateway (change claw_id to switch)", ErrNoModelSource)
}

// Complete 实现 Engine：单次 message/send 往返。
//
// 执行流程：
//  1. 解析端点与凭据（配置文件 / 环境变量 / 显式字段）
//  2. 展平 prompt，组成 JSON-RPC 请求（续接 id 放 message.contextId）
//  3. POST（超时由 ctx 控制）
//  4. 先看状态码、再解 envelope，取 status.message.parts[].text
//  5. JSONSchema 模式下再做严格抽取替换 Text
func (e *ArkClawEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()

	endpoint, key, clawID, err := e.endpoint()
	if err != nil {
		return Response{}, fmt.Errorf("arkclaw: %w", err)
	}
	if endpoint == "" || key == "" || clawID == "" {
		return Response{}, fmt.Errorf("arkclaw: 未配置（需要 url / key / claw_id）；请在 %s 的 \"arkclaw\" 节补齐，或设置 %s / %s / %s",
			e.configHint(), config.EnvArkClawURL, config.EnvArkClawKey, config.EnvArkClawClawID)
	}

	// A2A 只提供「按 contextId 续接」，没有「查询最近上下文」的接口，
	// 因此 Continue 无法实现，直接报错而不是静默新开会话。
	if req.Continue && req.SessionID == "" {
		return Response{}, fmt.Errorf("arkclaw: 不支持 continue（A2A 无「查询最近上下文」接口），请用 --session <context_id> 显式续接")
	}

	// 实测：网关拒绝 4 种 model 透传（query ?model= / params.message.metadata.model /
	// params.message.metadata.model.name / params.configuration.model / X-Model header），
	// 模型由 clawId 绑定。-m 在 arkclaw 上不是透传是「无作用」：告警一次，
	// 让用户知道「-m 没用，想换模型得改 config.json 的 claw_id」。
	// max-tokens / temperature / tools 与 trae/codex/openclaw 一致静默忽略
	//（矩阵注 ②/③ 已有先例，不在 arkclaw 上做差异化告警）。
	if req.Model != "" {
		fmt.Fprintf(stderr, "  ⚠ arkclaw: --model %s 不生效（实测网关按 clawId 绑死为 kimi-k2.6），要换模型请改配置 claw_id\n", req.Model)
	}

	// 协议没有独立 system 角色：与 trae / openclaw 同路，展平进 message 头部。
	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return Response{}, fmt.Errorf("arkclaw: empty prompt")
	}

	target, err := arkClawEndpointURL(endpoint, key, clawID)
	if err != nil {
		return Response{}, fmt.Errorf("arkclaw: 端点无效: %w", err)
	}

	body, err := func() ([]byte, error) {
		reqObj, berr := arkClawBuildRequest(prompt, req.SessionID, req.Attachments)
		if berr != nil {
			return nil, berr
		}
		return json.Marshal(reqObj)
	}()
	if err != nil {
		return Response{}, fmt.Errorf("arkclaw: 构造请求失败: %w", err)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = e.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultArkClawTimeout
	}
	httpCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(httpCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("arkclaw: 构造 HTTP 请求失败: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")

	client := e.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("arkclaw: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, maxArkClawBody))
	if err != nil {
		return Response{}, fmt.Errorf("arkclaw: 读取响应失败: %w", err)
	}

	resp, perr := arkClawParseResponse(httpResp.StatusCode, raw)
	resp.Latency = time.Since(start)
	resp.Engine = e.Name()
	// 网关没回 contextId 时沿用请求里的 id，保证调用方拿到的 session 可用。
	if resp.SessionID == "" {
		resp.SessionID = req.SessionID
	}
	// JSONSchema 后处理（与 llm / openclaw 同路径）。
	if req.JSONSchema != nil {
		if extracted, ok := extractJSONObjectStrict(resp.Text, req.JSONSchema); ok {
			resp.Text = extracted
		}
	}
	if perr != nil {
		return resp, perr
	}
	if strings.TrimSpace(resp.Text) == "" {
		return resp, fmt.Errorf("arkclaw: 网关返回空正文")
	}
	return resp, nil
}

// arkClawBuildRequest 组装 message/send 请求体。
//
// contextID 非空时写入 message.contextId（续接；放外层 params.contextId 无效）。
func arkClawBuildRequest(prompt, contextID string, atts []Attachment) (arkClawRequest, error) {
	r := arkClawRequest{
		JSONRPC: "2.0",
		ID:      arkClawRandomID(),
		Method:  "message/send",
	}
	// 提示词走 text part；附件走 A2A 原生 file part（inline base64）。
	// 远端 agent 看不到本机路径，所以**必须**inline —— 传路径等于没传。
	parts := []arkClawPart{{Kind: "text", Text: prompt}}
	for _, a := range atts {
		data, err := readAttachmentBase64(a)
		if err != nil {
			return arkClawRequest{}, err
		}
		mime := a.MIME
		if mime == "" {
			mime = "application/octet-stream"
		}
		parts = append(parts, arkClawPart{
			Kind: "file",
			File: &arkClawFileContent{Name: a.Name(), MimeType: mime, Bytes: data},
		})
	}
	r.Params.Message = arkClawMessage{
		Kind:      "message",
		Role:      "user",
		MessageID: arkClawRandomID(),
		ContextID: strings.TrimSpace(contextID),
		Parts:     parts,
	}
	return r, nil
}

// arkClawParseResponse 解析网关响应。
//
// 状态码优先：鉴权失败时网关回 text/plain 纯文本（非 JSON），必须把状态码
// 与 body 一起当错误上报，而不是让 json.Unmarshal 报含糊的语法错。
func arkClawParseResponse(status int, raw []byte) (Response, error) {
	body := strings.TrimSpace(string(raw))

	if status < 200 || status >= 300 {
		if body == "" {
			return Response{}, fmt.Errorf("arkclaw: HTTP %d（空响应体）", status)
		}
		return Response{}, fmt.Errorf("arkclaw: HTTP %d: %s", status, truncateStr(arkClawCollapse(body), 300))
	}
	if body == "" {
		return Response{}, fmt.Errorf("arkclaw: 网关返回空响应体（HTTP %d）", status)
	}

	env := arkClawEnvelope{}
	if err := json.Unmarshal(raw, &env); err != nil {
		// 少量网关会在 JSON 前混入日志行；退回括号计数法抽首个对象。
		obj, ok := firstJSONObject(body)
		if !ok {
			return Response{}, fmt.Errorf("arkclaw: 响应不是 JSON: %s", truncateStr(arkClawCollapse(body), 300))
		}
		if jerr := json.Unmarshal(mustMarshal(obj), &env); jerr != nil {
			return Response{}, fmt.Errorf("arkclaw: 解析响应失败: %w", jerr)
		}
	}

	if env.Error != nil {
		return Response{}, fmt.Errorf("arkclaw: JSON-RPC error %d: %s", env.Error.Code, arkClawCollapse(env.Error.Message))
	}
	if env.Result == nil {
		return Response{}, fmt.Errorf("arkclaw: 响应缺少 result 字段: %s", truncateStr(arkClawCollapse(body), 300))
	}

	res := *env.Result
	state := strings.ToLower(strings.TrimSpace(res.Status.State))
	switch state {
	case "failed", "canceled", "cancelled", "rejected", "unknown":
		detail := arkClawPickText(res)
		if detail == "" {
			detail = "(网关未给出正文)"
		}
		return Response{SessionID: res.ContextID},
			fmt.Errorf("arkclaw: 任务终止（status.state=%s）: %s", res.Status.State, truncateStr(arkClawCollapse(detail), 300))
	}

	return Response{
		Text:      arkClawPickText(res),
		SessionID: res.ContextID,
	}, nil
}

// arkClawPickText 按优先级取正文。
// status.message.parts[].text 是 A2A 里任务的最终答复；
// artifacts[].parts[].text 是网关把产物挂在产物区时的兜底。
func arkClawPickText(res arkClawResult) string {
	if res.Status.Message != nil {
		if s := arkClawJoinParts(res.Status.Message.Parts); s != "" {
			return s
		}
	}
	for _, a := range res.Artifacts {
		if s := arkClawJoinParts(a.Parts); s != "" {
			return s
		}
	}
	return ""
}

// arkClawJoinParts 把多个文本块拼成一段正文（换行分隔，去首尾空白）。
func arkClawJoinParts(parts []arkClawPart) string {
	var sb strings.Builder
	for _, p := range parts {
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(p.Text)
	}
	return strings.TrimSpace(sb.String())
}

// arkClawEndpointURL 把凭据拼进 URL query（apikey / clawId）。
// 原 URL 已带的 query 参数保留；同名参数以配置值为准。
func arkClawEndpointURL(endpoint, key, clawID string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("必须是绝对 URL（含 scheme 与 host）：%q", endpoint)
	}
	q := u.Query()
	if key != "" {
		q.Set("apikey", key)
	}
	if clawID != "" {
		q.Set("clawId", clawID)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// arkClawRandomID 生成 JSON-RPC id / messageId 用的随机标识（UUIDv4 形态，
// 仅用标准库；网关只需要「每轮唯一」）。
func arkClawRandomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况下 crypto/rand 不可用：时间戳兜底（同进程内仍唯一）。
		return fmt.Sprintf("magic-agent-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// arkClawCollapse 把连续空白压成单空格（错误摘要保持单行）。
func arkClawCollapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// firstNonEmpty 返回第一个去空白后非空的实参。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
