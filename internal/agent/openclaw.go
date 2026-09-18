package agent

// openclaw.go - OpenClaw CLI 引擎。
//
// 后端：openclaw（OpenClaw 2026.6.11，e085fa1，本机 /opt/homebrew/bin/openclaw）。
//
// 推荐集成模式：`--local` 嵌入式（embedded agent，无需 Gateway RPC，
// 调用方负责把 provider API key 装进 shell 环境；本包继承父进程 env 即满足）。
//
// 非交互命令：
//
//	openclaw agent --local --agent <id> --message <text> --json --timeout <sec>
//	openclaw agent --local --agent <id> --message-file <path> --json --timeout <sec>
//	openclaw agent --local --agent <id> --message <text> --session-id <id> --json
//	openclaw agent --local --agent <id> --message <text> --model <provider/model> --json
//
// 响应 envelope（顶层，不嵌套在 data 字段里）：
//
//	{
//	  "payloads": [{"text": "<助手正文>", "mediaUrl": null}],
//	  "meta": {
//	    "durationMs": 2658,
//	    "agentMeta": {
//	      "sessionId": "<uuid>",
//	      "provider": "papergames",
//	      "model": "deepseek-v4-pro",
//	      "usage": {"input":2974,"output":4,"cacheRead":49152,"total":52130},
//	      "lastCallUsage": {...}
//	    },
//	    "aborted": false
//	  }
//	}
//
// 注意：CLI 在 envelope 之前会输出一堆 plugin 日志到 stdout（"openviking: ..." 之类），
// 这些不是 NDJSON，混入 envelope。 解析策略：先在 stdout 全文里扫描 `{` 开头的
// 第一个 balance 平衡的 JSON 对象，再 unmarshal —— 与 firstJSONObject 同源。
//
// 续接：
//   - SessionID 非空 → `agent --session-id <id>` 直接续接
//   - Continue 真 → `openclaw sessions --json --limit 1` 拉最近一次会话的 sessionId
//     再当 SessionID 传
//   - 新会话 → 拿 Response.SessionID 留给下一轮
//
// SystemPrompt 注入：openclaw agent 没有 --system-prompt flag；把 system prompt
// 拼到 message 头（与 trae 相同路径）。
//
// MaxTokens / Temperature / JSONSchema：
//   - MaxTokens / Temperature: openclaw agent 无对应 flag，静默忽略（矩阵 ②）
//   - JSONSchema: 走和 llm 引擎一样的"输出后处理抽 JSON"路径(extractJSONObjectStrict)
//
// Tools 模式：openclaw 是"内嵌 agent"（自带工具循环），没有逐工具白名单/禁用能力。
//   整体忽略 Request.Tools 字段，由 openclaw 自身决定是否调用工具。
//
// CLI 路径解析顺序：显式 BinPath → MAGIC_AGENT_OPENCLAW_BIN → 常见安装位 → PATH
// （探测链统一收敛在 engine_base.go 的 cliBase）。
//
// 不实现 Stream：openclaw --json 是单次 envelope 协议；流式能力由后续 CLI
// 版本补齐。CLI 层面对 nil Streamer 有明确报错路径（see cli/ask.go）。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OpenClawEngine 通过 OpenClaw CLI --local 模式实现 Engine。
type OpenClawEngine struct {
	// BinPath 显式指定 CLI 路径（空 = 自动探测）。测试注入用。
	BinPath string

	// Agent 显式 agent id（默认 "main"）。Request 不承载 agent 字段，
	// 引擎构造时注入为集群级默认值。
	Agent string

	// Model 显式模型（与 Request.Model 效果一致；引擎构造时显式覆盖优先）。
	Model string
}

// Name 实现 Engine。
func (e *OpenClawEngine) Name() string { return "openclaw" }

// DefaultOpenClawTimeout 单次尝试默认超时（与 openclaw 自身 --timeout 默认 600s 对齐）。
const DefaultOpenClawTimeout = 10 * time.Minute

// bin 探测 openclaw CLI 路径（委托 cliBase 统一探测链）。
func (e *OpenClawEngine) bin() string {
	return openclawBase.resolve(e.BinPath)
}

// Detect 实现 Engine。
func (e *OpenClawEngine) Detect() (bool, string) {
	p := openclawBase.resolve(e.BinPath)
	if p == "" {
		return false, openclawBase.notFound
	}
	return true, p
}

// openclawAgentMeta 对应 meta.agentMeta（顶层 envelope 的 meta 嵌套）。
type openclawAgentMeta struct {
	SessionID     string        `json:"sessionId"`
	Provider      string        `json:"provider"`
	Model         string        `json:"model"`
	Usage         openclawUsage `json:"usage"`
	LastCallUsage openclawUsage `json:"lastCallUsage"`
}

// openclawUsage 对应 agentMeta.usage / lastCallUsage。
type openclawUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	Total      int `json:"total"`
}

// openclawEnvelope 对应 --json 顶层响应。
type openclawEnvelope struct {
	Payloads []struct {
		Text     string  `json:"text"`
		MediaURL *string `json:"mediaUrl"`
	} `json:"payloads"`
	Meta struct {
		DurationMs int               `json:"durationMs"`
		AgentMeta  openclawAgentMeta `json:"agentMeta"`
		Aborted    bool              `json:"aborted"`
	} `json:"meta"`
}

// openclawSession 对应 openclaw sessions --json 单条会话。
type openclawSession struct {
	SessionID string `json:"sessionId"`
	Key       string `json:"key"`
	Model     string `json:"model"`
	UpdatedAt int64  `json:"updatedAt"`
}

// openclawSessionsEnvelope 对应 openclaw sessions --json 顶层。
type openclawSessionsEnvelope struct {
	Sessions []openclawSession `json:"sessions"`
}

// ListModels 实现 ModelLister：`openclaw models list --json` 列出本机
// openclaw 当前配置可用的模型，取 key（"<provider>/<model>"，
// 即 --model 接受的形态）。
func (e *OpenClawEngine) ListModels(ctx context.Context) ([]string, error) {
	stdout, err := probeCLI(ctx, "openclaw", e.bin(), "models", "list", "--json")
	if err != nil {
		return nil, err
	}
	objs, err := jsonModelsArray(stdout)
	if err != nil {
		return nil, err
	}
	models := modelNamesFromObjects(objs, "key", "name")
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: openclaw models list returned no entries", ErrNoModelSource)
	}
	return models, nil
}

// Complete 实现 Engine：单次调用 openclaw agent。
//
// 执行流程：
//  1. 解析续接 id（SessionID 或 Continue → sessions 查最新）
//  2. 构造参数（openclaw agent --local --agent <id> ... --json --timeout <sec>）
//  3. runCLI 拉 stdout/stderr，stdout 含 plugin 日志 + 末尾 JSON envelope
//  4. firstJSONObject 抽取 envelope → unmarshal → 提取 payloads[0].text / agentMeta
//  5. JSONSchema 模式下再做严格抽取替换 Text
func (e *OpenClawEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return Response{}, fmt.Errorf("openclaw CLI not found; set MAGIC_AGENT_OPENCLAW_BIN")
	}

	// 续接解析:SessionID 优先;否则 Continue → sessions --limit 1 查最新。
	sessionID := req.SessionID
	if sessionID == "" && req.Continue {
		id, lerr := e.lookupLatestSessionID(ctx, bin)
		if lerr != nil {
			return Response{}, fmt.Errorf("openclaw: continue 解析最近会话失败: %w", lerr)
		}
		if id == "" {
			return Response{}, fmt.Errorf("openclaw: continue 模式未找到任何已存在会话")
		}
		sessionID = id
	}

	// 把 system prompt 拼到 message 头(openclaw agent 无 --system-prompt flag)
	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return Response{}, fmt.Errorf("openclaw: empty prompt")
	}
	// 附件：openclaw 无 per-call 附件参数，只能把绝对路径写进提示词
	//（与 trae 同一兜底；需要 openclaw 侧有读文件能力）。
	prompt = appendAttachmentSection(prompt, req.Attachments)

	// prompt 较长时走 --message-file 临时文件,避免命令行长度越界
	tmpDir, terr := os.MkdirTemp("", "magic-agent-openclaw-")
	if terr != nil {
		return Response{}, fmt.Errorf("openclaw: mktemp: %w", terr)
	}
	defer os.RemoveAll(tmpDir)
	msgArgs, cleanup, err := writeMessageArg(tmpDir, prompt)
	if err != nil {
		return Response{}, fmt.Errorf("openclaw: write message: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	args := e.buildArgs(req, sessionID, msgArgs)

	// workspace：openclaw 无 per-call 工作目录 flag（workspace 由 openclaw agents 绑定），
	// 子进程 cwd 是唯一可用的方式
	stdout, stderr, runErr := runCLIIn(ctx, req.Workspace, bin, args...)
	// 即便非零退出也尝试解析 envelope(plugin 错误后仍可能输出有效 JSON)。
	resp, perr := e.parseStdout(stdout)
	if resp.Latency == 0 {
		resp.Latency = time.Since(start)
	}
	resp.Engine = e.Name()
	// SessionID: 优先 envelope.agentMeta.sessionId;为空时回传请求里的 id
	// (Continue 路径我们已经查到了 sessionId,Envelope 通常也会回;以 envelope 为准)
	if resp.SessionID == "" {
		resp.SessionID = sessionID
	}
	// JSONSchema 后处理
	if req.JSONSchema != nil {
		if m, ok := extractJSONObjectStrict(resp.Text, req.JSONSchema); ok {
			if b, jerr := json.Marshal(m); jerr == nil {
				resp.Text = string(b)
			}
		}
	}
	if runErr != nil {
		return resp, wrapCliError("openclaw", stdout, stderr, runErr)
	}
	if perr != nil {
		return resp, perr
	}
	if strings.TrimSpace(resp.Text) == "" {
		return resp, fmt.Errorf("openclaw CLI returned empty result")
	}
	return resp, nil
}

// parseStdout 从 stdout(可能含 plugin 日志)中抽出顶层 JSON envelope 并解析。
// 找不到 envelope 时返回 ("", false)。
func (e *OpenClawEngine) parseStdout(stdout string) (Response, error) {
	env := openclawEnvelope{}
	// 方案 A:整段就是 JSON(短输出)
	if err := json.Unmarshal([]byte(stdout), &env); err == nil && len(env.Payloads) > 0 {
		return envelopeToResponse(env), nil
	}
	// 方案 B:stdout 含 plugin 日志,逐行扫描
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var collected strings.Builder
	// 我们不知道 JSON 跨多少行,先按整段重试
	for sc.Scan() {
		collected.WriteString(sc.Text())
		collected.WriteByte('\n')
		obj, ok := firstJSONObject(collected.String())
		if !ok {
			continue
		}
		// 拿到第一个顶层 JSON 对象后再 unmarshal(map → json → envelope)
		b, _ := json.Marshal(obj)
		var env2 openclawEnvelope
		if err := json.Unmarshal(b, &env2); err == nil && len(env2.Payloads) > 0 {
			return envelopeToResponse(env2), nil
		}
	}
	return Response{}, fmt.Errorf("openclaw: 未在 stdout 中找到有效 JSON envelope")
}

// envelopeToResponse 从 envelope 构造 Response。
func envelopeToResponse(env openclawEnvelope) Response {
	text := ""
	if len(env.Payloads) > 0 {
		text = env.Payloads[0].Text
	}
	meta := env.Meta.AgentMeta
	return Response{
		Text:         text,
		Model:        meta.Model,
		SessionID:    meta.SessionID,
		InputTokens:  meta.Usage.Input + meta.Usage.CacheRead,
		OutputTokens: meta.Usage.Output,
		TotalTokens:  meta.Usage.Total,
	}
}

// lookupLatestSessionID 调用 `openclaw sessions --json --limit 1` 解析最近一条会话 id。
// 失败/无会话时返回 ("", err)。
func (e *OpenClawEngine) lookupLatestSessionID(ctx context.Context, bin string) (string, error) {
	agent := e.agentID()
	args := []string{"sessions", "--json", "--limit", "1"}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	stdout, _, err := runCLI(ctx, bin, args...)
	if err != nil {
		return "", err
	}
	var env openclawSessionsEnvelope
	if jerr := json.Unmarshal([]byte(stdout), &env); jerr == nil {
		if len(env.Sessions) > 0 && env.Sessions[0].SessionID != "" {
			return env.Sessions[0].SessionID, nil
		}
	}
	// 兜底:同样走 firstJSONObject 抗 log 混入
	obj, ok := firstJSONObject(stdout)
	if !ok {
		return "", fmt.Errorf("openclaw sessions: 未找到 JSON envelope")
	}
	if jerr := json.Unmarshal(mustMarshal(obj), &env); jerr == nil && len(env.Sessions) > 0 {
		return env.Sessions[0].SessionID, nil
	}
	return "", nil
}

// mustMarshal 是 json.Marshal 的零值兜底(marshal map[string]any 不会失败)。
func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// agentID 返回构造时显式注入的 agent id;为空则用 "main" 作默认。
func (e *OpenClawEngine) agentID() string {
	if e.Agent != "" {
		return e.Agent
	}
	return "main"
}

// buildArgs 构造 `openclaw agent --local ...` 参数。
//
// 形态:
//
//	openclaw agent --local --agent <id> --json
//	  [--model <provider/model>] [--session-id <id>]
//	  [--message <text> | --message-file <path>]
//	  [--timeout <sec>]
func (e *OpenClawEngine) buildArgs(req Request, sessionID string, msgArgs []string) []string {
	args := []string{"agent", "--local", "--agent", e.agentID(), "--json"}
	if m := stripModelPrefix(req.Model); m != "" {
		args = append(args, "--model", m)
	} else if e.Model != "" {
		args = append(args, "--model", stripModelPrefix(e.Model))
	}
	if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	}
	args = append(args, msgArgs...)
	// 超时(秒):外层 Request.Timeout 是 Duration;openclaw --timeout 是 int 秒数
	if req.Timeout > 0 {
		secs := int(req.Timeout / time.Second)
		if secs < 1 {
			secs = 1
		}
		args = append(args, "--timeout", fmt.Sprintf("%d", secs))
	} else {
		args = append(args, "--timeout", "600")
	}
	// 统一参数矩阵:MaxTokens / Temperature / Tools / JSONSchema 静默忽略(JSONSchema 走
	// 输出后处理,Tools 是 openclaw 内嵌能力,调用方无法逐工具控)
	_ = req.MaxTokens
	_ = req.Temperature
	_ = req.Tools
	_ = req.JSONSchema
	return args
}

// writeMessageArg 决定如何把 prompt 传给 openclaw agent。
//
// 策略:统一走 --message-file <tmpfile>(避免长 prompt 撞 ARG_MAX,
// 也避免 shell 引号转义问题)。 仅在 prompt 极短(<=4KB)时走 --message
// 内联(节省一次落盘)。
//
// 返回 []string 为待 append 的 args 段(已含 flag 自身);cleanup 非 nil
// 时调用方需 defer 执行(只在走 file 分支时使用)。
func writeMessageArg(tmpDir, prompt string) ([]string, func(), error) {
	const inlineMax = 4 * 1024
	if len(prompt) <= inlineMax {
		return []string{"--message", prompt}, nil, nil
	}
	fp := filepath.Join(tmpDir, "message.txt")
	if err := os.WriteFile(fp, []byte(prompt), 0o600); err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.Remove(fp) }
	return []string{"--message-file", fp}, cleanup, nil
}
