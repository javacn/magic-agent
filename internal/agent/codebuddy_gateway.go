package agent

/* codebuddy_gateway.go - codebuddy-gateway 引擎（CodeBuddy Code HTTP 网关，
 * 走 webhook + SSE 的「远程连接模式」）。
 *
 * 与其余引擎最大的不同：它既不起本机 CLI（不是 claude / codebuddy 那种），
 * 也不走 A2A JSON-RPC（不是 arkclaw 那种），而是把 prompt **投递**给一个已经在跑的
 * CodeBuddy Code HTTP 网关 —— 也就是 `codebuddy --serve`（或交互会话里的 `/gateway`
 * 远程控制）暴露出来的那套服务，见官方文档「远程控制（Remote Control）」
 * https://www.codebuddy.ai/docs/zh/cli/remote-control
 *
 * 为什么要有这个引擎（2026-09-24）：用户要「支持这种远程连接模式接入引擎，走
 * webhook + SSE」。它解决的是 codebuddy 引擎解决不了的两件事：
 *   · 复用**已经登录、已经配好模型与 MCP** 的那个网关进程 —— 不必每次 spawn 一个 CLI；
 *   · 把执行放到网关那台机器上（Tunnel 形态下网关可以在另一台机器 / 容器里），
 *     本机只负责投递与收流。
 *
 * 两段式协议（2026-09-24 对 CodeBuddy Code 2.147.0 真机实测，别凭文档臆想）：
 *
 *  1) 投递（本文件）
 *     POST <base>/api/v1/webhooks/{platform}
 *     authorization: Bearer <password>      ← `--auth password` 时的口令
 *     x-codebuddy-request: 1                ← 网关的请求校验中间件要求（缺了 403）
 *     {"version":"1.0","id":"<msgId>","type":"message",
 *      "source":{"platform":"generic","sender":{"id":"…"},
 *                "conversation":{"id":"<会话锚点>","type":"direct"}},
 *      "payload":{"text":"<prompt>","attachments":[…]}}
 *     → 202 {"data":{"runId":"<uuid>","status":"accepted"}}      （实测 3~13ms 返回）
 *
 *  2) 收流（codebuddy_gateway_stream.go）
 *     GET <base>/api/v1/runs/{runId}/stream
 *     accept: text/event-stream
 *     → SSE，逐帧 data: {"version":"1.0","replyTo":"<msgId>","status":"…",…}
 *
 * ⚠️ 关键事实：**投递响应里没有正文**。网关是「受理即返回」的异步模型，正文只在
 * SSE 通道上出现（`status=streaming` 的 `content.chunk` 增量 + `status=completed`
 * 的 `content.markdown` 整段）。所以本引擎的 Complete 也必须读 SSE —— 这不是「为了
 * 流式而流式」，而是那条通道是唯一拿得到正文的路。
 * 附带结论：platform 只能选 generic —— wecom / wechat-kf 的适配器会把回复**推给平台**，
 * 而 generic 适配器根本没有实现 sendReply（实测回调一次都不会触发），正文只留在 run
 * 流里，正好由本引擎消费。
 *
 * 参数映射（统一参数矩阵的 codebuddy-gateway 列）：
 *   - Model：协议里没有模型字段，模型由网关进程自己的配置决定（`--model` / 会话设置）
 *     → 传 -m 不生效，打一行告警后忽略（与 arkclaw 的「模型被 claw_id 绑死」同构）
 *   - SessionID：写入 source.conversation.id —— 网关按它 getOrCreateSession，
 *     同一个 id = 同一个会话（**这是真正的多轮续接**，实测有效）
 *   - Continue：网关没有「查询最近会话」的接口 → **显式报错**（静默开新会话会让
 *     调用方以为上下文接上了）
 *   - SystemPrompt：协议无独立 system 角色 → 展平进 prompt 头部
 *   - MaxTokens / Temperature：协议无对应字段 → 静默忽略
 *   - Tools / Permission：会话的 agent 模式与权限档位由**网关进程启动参数**决定
 *     （`--agent` / `--permission-mode`），网关还会把远程任务的权限强制切到
 *     bypassPermissions（远程场景无法交互式审批）→ per-call 无法落地，能力表报 none
 *   - Workspace：网关在**它自己的 cwd** 里跑 → per-call 无法指定，能力表报 none
 *   - Attachments：走 payload.attachments 原生字段（urlType=local-path），
 *     由网关把它渲染成「附件清单 + 路径」塞进 prompt，模型再用 Read 工具去看 ——
 *     所以能力表如实报 "prompt"（图**不会**以图片形式进模型上下文，别报成 part:file）
 *   - JSONSchema：无原生约束 → 输出后处理抽 JSON（同 llm / openclaw / arkclaw）
 *   - Append（常驻会话）：webhook 是「一次投递一个 run」的模型，没有持续喂消息的通道
 *     → 不支持，能力表报 false
 */

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

// DefaultCBGatewayTimeout 单次调用的默认超时。
//
// 10 分钟：网关侧的 runTimeoutMs 默认就是 1_800_000（30 分钟，实测值来自网关内核
// DEFAULT_RUN_TIMEOUT_MS），而 CLI 的 -t 默认也是 600s —— 取 10 分钟与 CLI 层一致，
// 避免「CLI 说 600s、引擎内部却更短」这种自相矛盾。要跑更久的任务显式给 -t。
const DefaultCBGatewayTimeout = 10 * time.Minute

// cbGatewayMaxBody 单次响应体读取上限（webhook 受理响应很小，留足余量即可）。
const cbGatewayMaxBody = 1 << 20 // 1 MiB

// cbGatewayEngineName 引擎标识（注册名 / -e 取值 / --engines 展示名）。
const cbGatewayEngineName = "codebuddy-gateway"

// cbGatewayDefaultSender 未配置 sender 时用的发送者标识（网关按它做限流与归属）。
const cbGatewayDefaultSender = "magic-agent"

// cbGatewayPlatformGeneric generic 平台：唯一「正文只留在 run 流里」的通道，
// 也是本引擎的默认（见文件头「关键事实」）。
const cbGatewayPlatformGeneric = "generic"

// cbGatewaySecurityHeader 网关请求校验中间件要求的头。
//
// 依据：内核里的 gateway_request_validation_middleware —— 非豁免路径缺这个头直接
// 403 {"error":"Missing required header: x-codebuddy-request"}。实测 GET /api/v1/runs/{id}
// 就命中过这个 403。webhook 的 POST 虽在豁免表里，一并带上没有副作用。
const cbGatewaySecurityHeader = "x-codebuddy-request"

// CodeBuddyGatewayEngine 通过 CodeBuddy Code HTTP 网关（webhook + SSE）实现 Engine。
type CodeBuddyGatewayEngine struct {
	// URL / Password / Platform / Sender / Conversation 显式覆盖配置与环境变量。
	// 生产路径留空、走配置文件；测试注入用。
	URL          string
	Password     string
	Platform     string
	Sender       string
	Conversation string

	// ConfigPath 显式配置文件路径（空 = config.Path() 默认探测链）。测试注入用。
	ConfigPath string

	// HTTPClient 显式 HTTP 客户端（空 = 每请求带 ctx 超时的默认客户端）。测试注入用。
	HTTPClient *http.Client

	// Timeout 单次调用超时（0 = DefaultCBGatewayTimeout）。
	Timeout time.Duration
}

// Name 实现 Engine。
func (e *CodeBuddyGatewayEngine) Name() string { return cbGatewayEngineName }

// CapabilityFamily 实现 CapabilityFamily：本引擎自成一族（协议与 arkclaw / codebuddy
// 都不同），故家族名就是自己的名字 —— 五张能力表按它查。
func (e *CodeBuddyGatewayEngine) CapabilityFamily() string { return cbGatewayEngineName }

// cbGatewayResolved 是一次调用生效的连接参数（显式字段 → 环境变量 → 配置文件）。
type cbGatewayResolved struct {
	baseURL      string
	password     string
	platform     string
	sender       string
	conversation string
}

// resolve 解析生效的连接参数。
//
// 优先级：结构体显式字段 → 环境变量 → 配置文件
// （环境变量覆盖文件值这一步由 config.LoadFrom 内部完成）。
func (e *CodeBuddyGatewayEngine) resolve() (cbGatewayResolved, error) {
	path := e.ConfigPath
	if strings.TrimSpace(path) == "" {
		path = config.Path()
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		return cbGatewayResolved{}, err
	}
	g := cfg.CodeBuddyGateway
	return cbGatewayResolved{
		baseURL:      strings.TrimRight(firstNonEmpty(e.URL, g.URL), "/"),
		password:     firstNonEmpty(e.Password, g.Password),
		platform:     firstNonEmpty(e.Platform, g.Platform, cbGatewayPlatformGeneric),
		sender:       firstNonEmpty(e.Sender, g.Sender, cbGatewayDefaultSender),
		conversation: firstNonEmpty(e.Conversation, g.Conversation),
	}, nil
}

// configHint 返回报错时提示的配置文件路径。
func (e *CodeBuddyGatewayEngine) configHint() string {
	p := strings.TrimSpace(e.ConfigPath)
	if p == "" {
		p = config.Path()
	}
	if p == "" {
		return "(配置文件路径未知)"
	}
	return p + ` 的 "codebuddyGateway" 节`
}

// Detect 实现 Engine。
//
// 与 arkclaw 同一取舍：**只查配置齐备，不发网络请求**。
// 理由：`--engines` 会对每个引擎调一次 Detect，加一次网络探测会让列表慢上一个 RTT，
// 而「网关此刻在不在跑」是运行期状态 —— 真连不上时 Complete/Stream 会给出准确报错。
func (e *CodeBuddyGatewayEngine) Detect() (bool, string) {
	r, err := e.resolve()
	if err != nil {
		return false, fmt.Sprintf("%s config error: %v", e.Name(), err)
	}
	if r.baseURL == "" {
		return false, fmt.Sprintf("%s not configured: missing url in %s (or set %s)",
			e.Name(), e.configHint(), config.EnvCBGWURL)
	}
	return true, r.baseURL
}

// ListModels 实现 ModelLister：网关**不提供**模型清单。
//
// 模型由网关进程自己的配置决定（`--model` / 会话里的模型选择），webhook 协议里没有
// 模型字段 → 不编造列表，只返回 ErrNoModelSource + 原因。
func (e *CodeBuddyGatewayEngine) ListModels(_ context.Context) ([]string, error) {
	return nil, fmt.Errorf("%w: codebuddy-gateway uses the model configured in the gateway process (start it with --model or switch in the session)", ErrNoModelSource)
}

// cbGatewayInbound 是投递给网关的入站消息（Gateway Protocol 格式）。
//
// 字段名与网关内核的 generic 适配器解析一一对应：
// id / type / source / payload 是**必需**的（缺 id 或 type 直接 400
// "Invalid generic message format: missing required fields (id, type)"，实测过）。
type cbGatewayInbound struct {
	Version string           `json:"version"`
	ID      string           `json:"id"`
	Type    string           `json:"type"`
	Source  cbGatewaySource  `json:"source"`
	Payload cbGatewayPayload `json:"payload"`
	Action  string           `json:"action,omitempty"`
}

// cbGatewaySource 是消息来源（平台 + 发送者 + 会话锚点）。
type cbGatewaySource struct {
	Platform     string          `json:"platform"`
	Sender       cbGatewaySender `json:"sender"`
	Conversation cbGatewayConv   `json:"conversation"`
}

// cbGatewaySender 是发送者。
type cbGatewaySender struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// cbGatewayConv 是会话（网关按 Conversation.ID 做 getOrCreateSession，即续接锚点）。
type cbGatewayConv struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// cbGatewayPayload 是消息体（正文 + 可选附件）。
type cbGatewayPayload struct {
	Text        string                `json:"text,omitempty"`
	Attachments []cbGatewayAttachment `json:"attachments,omitempty"`
}

// cbGatewayAttachment 是附件条目。
//
// 网关侧把它渲染成「附件清单（请按需使用 Read / WebFetch 等工具查看）：- [类型] 名称
// （本地路径，可用 Read）: <路径>」拼进 prompt —— 所以本引擎报的附件能力是 "prompt"
// 而不是图片直传（见文件头）。
type cbGatewayAttachment struct {
	Type    string `json:"type"`
	Name    string `json:"name,omitempty"`
	URL     string `json:"url"`
	URLType string `json:"urlType"`
}

// cbGatewayAccepted 是 webhook 受理响应的信封。
type cbGatewayAccepted struct {
	Data struct {
		RunID  string `json:"runId"`
		Status string `json:"status"`
	} `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete 实现 Engine：一次 webhook 投递 + 一条 SSE 收流。
//
// 单次尝试；超时与重试由 Runner 在外层编排。
func (e *CodeBuddyGatewayEngine) Complete(ctx context.Context, req Request) (Response, error) {
	res, err := e.run(ctx, req, nil)
	return res.Response, err
}

// run 是 webhook + SSE 的公共执行核（Complete 与 Stream 共用）。
//
// onEvent 为 nil = 非流式（只在收尾返回全文）；非 nil = 实时把正文增量转出去。
// 无论哪种，正文都必须经 SSE 拿 —— 这是协议决定的（见文件头）。
func (e *CodeBuddyGatewayEngine) run(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()

	r, err := e.resolve()
	if err != nil {
		return StreamResult{}, fmt.Errorf("%s: %w", e.Name(), err)
	}
	if r.baseURL == "" {
		return StreamResult{}, fmt.Errorf("%s: 未配置（需要 url）；请在 %s 补齐，或设置 %s",
			e.Name(), e.configHint(), config.EnvCBGWURL)
	}

	// 网关没有「查询最近会话」的接口：续接只能靠显式 conversation id。
	if req.Continue && req.SessionID == "" {
		return StreamResult{}, fmt.Errorf("%s: 不支持 continue（网关无「查询最近会话」接口），请用 --session <conversation_id> 显式续接", e.Name())
	}
	// 模型由网关进程绑定，-m 在这里没有落地通道 —— 如实告警，不静默吞掉。
	if strings.TrimSpace(req.Model) != "" {
		fmt.Fprintf(stderr, "  ⚠ %s: --model %s 不生效（模型由网关进程自己的配置决定），要换模型请在网关侧改\n", e.Name(), req.Model)
	}

	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if strings.TrimSpace(prompt) == "" {
		return StreamResult{}, fmt.Errorf("%s: empty prompt", e.Name())
	}

	// 会话锚点：显式 --session 优先（= 续接同一会话）；否则用配置里的固定 conversation；
	// 都没有就每次生成一个新的（= 每次新会话，不串上下文）。
	convID := firstNonEmpty(req.SessionID, r.conversation)
	if convID == "" {
		convID = "magic-agent-" + cbGatewayRandomHex(8)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = e.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultCBGatewayTimeout
	}
	httpCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := e.HTTPClient
	if client == nil {
		client = &http.Client{}
	}

	msgID := "magic-agent-" + cbGatewayRandomHex(12)
	runID, err := e.deliver(httpCtx, client, r, msgID, convID, prompt, req.Attachments)
	if err != nil {
		return StreamResult{Response: e.baseResponse(start, convID)}, err
	}

	st := &cbGatewayStreamState{
		onEvent:   onEvent,
		msgID:     msgID,
		runID:     runID,
		convID:    convID,
		sessionID: convID,
	}
	if err := e.consumeStream(httpCtx, client, r, runID, st, req.JSONSchema); err != nil {
		// 流被截断 / 任务 failed / HTTP 拒绝都走这一条：已发出的正文 item
		// 必须收成 failed，不能让它永远停在 running（消费方只能靠超时猜，
		// 历史回放里那一条会永远显示「生成中」）。语义见 cbGatewayStreamState.fail。
		st.fail(err)
		return st.result(e, req, start), err
	}
	res := st.result(e, req, start)
	if strings.TrimSpace(res.Text) == "" {
		return res, fmt.Errorf("%s: 网关返回空正文（run %s）", e.Name(), runID)
	}
	return res, nil
}

// deliver 投递一条入站消息，返回网关分配的 runId。
func (e *CodeBuddyGatewayEngine) deliver(ctx context.Context, client *http.Client, r cbGatewayResolved, msgID, convID, prompt string, atts []Attachment) (string, error) {
	inbound := cbGatewayInbound{
		Version: "1.0",
		ID:      msgID,
		Type:    "message",
		Source: cbGatewaySource{
			Platform:     r.platform,
			Sender:       cbGatewaySender{ID: r.sender, Name: r.sender},
			Conversation: cbGatewayConv{ID: convID, Type: "direct"},
		},
		Payload: cbGatewayPayload{
			Text:        prompt,
			Attachments: cbGatewayAttachments(atts),
		},
	}
	body, err := json.Marshal(inbound)
	if err != nil {
		return "", fmt.Errorf("%s: 构造投递请求失败: %w", e.Name(), err)
	}

	endpoint := r.baseURL + "/api/v1/webhooks/" + url.PathEscape(r.platform)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%s: 构造 HTTP 请求失败: %w", e.Name(), err)
	}
	httpReq.Header.Set("content-type", "application/json")
	cbGatewaySetAuth(httpReq, r.password)

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("%s: 投递失败（网关 %s 是否在跑？）: %w", e.Name(), r.baseURL, err)
	}
	defer httpResp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, cbGatewayMaxBody))
	if err != nil {
		return "", fmt.Errorf("%s: 读取投递响应失败: %w", e.Name(), err)
	}

	var acc cbGatewayAccepted
	_ = json.Unmarshal(raw, &acc) // 非 JSON 正文（401 的 text/plain 等）留给下面的状态码分支解释
	if httpResp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("%s: 网关拒绝投递（HTTP %d）: %s", e.Name(), httpResp.StatusCode, cbGatewayErrorDetail(raw))
	}
	if acc.Data.RunID == "" {
		return "", fmt.Errorf("%s: 网关未返回 runId（HTTP %d）: %s", e.Name(), httpResp.StatusCode, cbGatewayErrorDetail(raw))
	}
	return acc.Data.RunID, nil
}

// baseResponse 构造「只有引擎/会话信息」的响应骨架（错误路径用）。
func (e *CodeBuddyGatewayEngine) baseResponse(start time.Time, convID string) Response {
	return Response{
		Engine:    e.Name(),
		SessionID: convID,
		Latency:   time.Since(start),
		Attempts:  1,
	}
}

// cbGatewaySetAuth 给请求带上网关认证（`--auth password` 时的 Bearer 口令）。
//
// 网关的 authenticate() 支持四种：?password= / gateway_session cookie /
// authorization: Bearer / x-access-token（实测读内核得到）。这里用最标准的 Bearer。
// 口令为空（`--auth none`）时不加任何头 —— 加了反而可能被当成无效凭据。
func cbGatewaySetAuth(req *http.Request, password string) {
	req.Header.Set(cbGatewaySecurityHeader, "1")
	if strings.TrimSpace(password) != "" {
		req.Header.Set("authorization", "Bearer "+strings.TrimSpace(password))
	}
}

// cbGatewayAttachments 把 CLI 层校验过的附件映射成网关协议里的附件条目。
//
// urlType 一律 "local-path"：网关与本机同机（或同容器）时路径才有效；
// 跨机（Tunnel 形态）时路径对网关侧无意义 —— 这一点在 README 里写明，不在这里猜。
func cbGatewayAttachments(atts []Attachment) []cbGatewayAttachment {
	if len(atts) == 0 {
		return nil
	}
	out := make([]cbGatewayAttachment, 0, len(atts))
	for _, a := range atts {
		out = append(out, cbGatewayAttachment{
			Type:    cbGatewayAttachmentType(a.MIME),
			URL:     a.Path,
			URLType: "local-path",
		})
	}
	return out
}

// cbGatewayAttachmentType 按 MIME 归类附件类型（网关用它渲染清单里的中文标签）。
func cbGatewayAttachmentType(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.HasPrefix(m, "image/"):
		return "image"
	case strings.HasPrefix(m, "audio/"):
		return "voice"
	case strings.HasPrefix(m, "video/"):
		return "video"
	}
	return "file"
}

// cbGatewayErrorDetail 从错误响应体里抽一句可读说明（JSON 优先，否则原文截断）。
func cbGatewayErrorDetail(raw []byte) string {
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error != nil && env.Error.Message != "" {
		if env.Error.Code != "" {
			return env.Error.Code + ": " + env.Error.Message
		}
		return env.Error.Message
	}
	return truncateStr(strings.TrimSpace(string(raw)), 300)
}

// cbGatewayRandomHex 生成 n 字节随机数的十六进制串（消息 id / 会话 id 用）。
func cbGatewayRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 随机源不可用时退化成时间戳：id 只要在本机唯一即可，不需要密码学强度。
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
