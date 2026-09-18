package agent

// codebuddy.go - CodeBuddy（WorkBuddy）CLI 引擎。
//
// 后端：WorkBuddy.app 内置的 codebuddy CLI。非交互模式与 claude 同源
// （同为 CodeBuddy Code 系），但输出 envelope 实测有两种形态，解析需兼容：
//
//	1. 单对象：{"type":"result","result":"<md>",...}
//	2. 数组：[{...stream messages...}]，扫描 type=="result" 或
//	   逐条拼 assistant text content
//
// 关键 flags：
//
//	--print --output-format json     单结果 envelope
//	--model <m>                      裸模型名（hy3 / glm-5.3 / ...）
//	--tools ""                       禁用全部工具
//	-y                               启用工具（--dangerously-skip-permissions）
//	-r, --resume <id>                续接指定会话（Request.SessionID 非空时传）。
//	-c, --continue                   续接最近一次会话（Request.Continue）。
//	                                 默认新会话不传 --no-session-persistence，
//	                                 否则首轮会话不落盘、session_id 无法续接
//	                                （与 claude 同款 "cannot be resumed" 语义）
//	--append-system-prompt <s>       注入 system prompt（仅 off 模式附 noToolSuffix）
//	--settings <json>                注入 {"env":{"CLAUDE_CODE_MAX_OUTPUT_TOKENS":"<n>"}}
//	                                 实现 MaxTokens（codebuddy 无原生 max-tokens flag，
//	                                 与 claude 同款 --settings 通道，见矩阵①）
//
// CLI 路径解析：显式 BinPath → MAGIC_AGENT_CODEBUDDY_BIN → WorkBuddy.app
// 内置路径 → PATH（探测链统一收敛在 engine_base.go 的 cliBase）。
//
// ⚠️ 在 WorkBuddy 会话内调用时，父进程注入的 SERVER__PORT 会让 CLI 抢
// 父会话已监听的端口 → EADDRINUSE → 永久挂起。子进程环境由 env.go
// 的 denylist 统一剔除该类变量。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// CodeBuddyEngine 通过 CodeBuddy CLI 实现 Engine。
type CodeBuddyEngine struct {
	// BinPath 显式指定 CLI 路径（空 = 自动探测）。测试注入用。
	BinPath string
}

// Name 实现 Engine。
func (e *CodeBuddyEngine) Name() string { return "codebuddy" }

// DefaultCodeBuddyTimeout 单次尝试默认超时。
const DefaultCodeBuddyTimeout = 5 * time.Minute

// DefaultCodeBuddyModel codebuddy 引擎的默认模型。
// 未显式 -m 指定时使用 hy3（与 magic-video 的 DefaultCreativeModel 一致）。
const DefaultCodeBuddyModel = "hy3"

// bin 探测 codebuddy CLI 路径（委托 cliBase 统一探测链）。
func (e *CodeBuddyEngine) bin() string {
	return codebuddyBase.resolve(e.BinPath)
}

// Detect 实现 Engine。
func (e *CodeBuddyEngine) Detect() (bool, string) {
	p := codebuddyBase.resolve(e.BinPath)
	if p == "" {
		return false, codebuddyBase.notFound
	}
	return true, p + " (default model: " + DefaultCodeBuddyModel + ")"
}

// ListModels 实现 ModelLister。
//
// codebuddy 没有 models 子命令，但 --help 里 --model 的描述自带权威清单
// （"Currently supported: (auto, hy3, ..., custom-local:*)"）—— 直接解析它，
// 不写死任何模型名；用户新注册的 custom-local 模型也会自动出现。
func (e *CodeBuddyEngine) ListModels(ctx context.Context) ([]string, error) {
	stdout, err := probeCLI(ctx, "codebuddy", e.bin(), "--help")
	if err != nil {
		return nil, err
	}
	models := parseCodebuddyHelpModels(stdout)
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: --help has no \"Currently supported\" list", ErrNoModelSource)
	}
	return models, nil
}

// Complete 实现 Engine：单次调用 codebuddy CLI。
func (e *CodeBuddyEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return Response{}, fmt.Errorf("codebuddy CLI not found; set MAGIC_AGENT_CODEBUDDY_BIN")
	}

	// 附件（截图）：与 claude 同族，--print 文本模式收不了图，走
	// --input-format stream-json（图片 content block 经 stdin）。
	if hasImageAttachment(req.Attachments) {
		return e.completeWithAttachments(ctx, req, start)
	}

	args := e.buildArgs(req)

	// workspace：codebuddy 与 claude 同族（无工作目录 flag，--add-dir 只加额外目录）
	stdout, stderr, err := runCLIIn(ctx, req.Workspace, bin, args...)
	if err != nil {
		return Response{}, wrapCliError("codebuddy", stdout, stderr, err)
	}

	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return Response{}, fmt.Errorf("codebuddy CLI returned empty output")
	}

	text := extractCodeBuddyResult(raw)

	// 兜底：个别版本会把 user 请求回显（<user_query>…</user_query>）当正文。
	if cleaned := stripUserQueryEcho(text); cleaned != text {
		text = cleaned
	}
	if strings.TrimSpace(text) == "" {
		return Response{}, fmt.Errorf("codebuddy CLI 返回内容仅为请求回显（无模型正文）")
	}

	return Response{
		Text:      text,
		Model:     req.Model,
		SessionID: extractCodeBuddySession(raw),
		Latency:   time.Since(start),
	}, nil
}

// completeWithAttachments 走 stream-json 输入通道跑一次带附件的调用
// （codebuddy 与 claude 同族协议），把事件归约成单次 Response。
func (e *CodeBuddyEngine) completeWithAttachments(ctx context.Context, req Request, start time.Time) (Response, error) {
	bin := e.bin()
	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return Response{}, fmt.Errorf("codebuddy: empty prompt")
	}
	args := streamJSONArgs(e.buildArgsBase(req), prompt, false)
	stdin, err := streamJSONUserLine(prompt, req.Attachments)
	if err != nil {
		return Response{}, err
	}
	acc := &streamAccumulator{Engine: e.Name()}
	var fin streamJSONResult
	seen, err := runStreamJSONIn(ctx, req.Workspace, nil, bin, args, strings.NewReader(stdin), acc, &fin)
	if err != nil {
		return Response{}, wrapCliError("codebuddy", "", "", err)
	}
	if !seen {
		return Response{}, fmt.Errorf("codebuddy CLI stream ended without result line")
	}
	if fin.IsError {
		return Response{}, fmt.Errorf("codebuddy CLI error (subtype=%s): %s", fin.Subtype, truncateStr(fin.Result, 500))
	}
	text := stripUserQueryEcho(finalizeStreamText(fin, acc))
	if strings.TrimSpace(text) == "" {
		return Response{}, fmt.Errorf("codebuddy CLI 返回内容仅为请求回显（无模型正文）")
	}
	return Response{
		Text:      text,
		Model:     fin.Model,
		SessionID: fin.SessionID,
		Latency:   time.Since(start),
	}, nil
}

// buildArgs 构造 codebuddy CLI 参数（Complete 与 Stream 共用）。
func (e *CodeBuddyEngine) buildArgs(req Request) []string {
	return append(e.buildArgsBase(req), FlattenPrompt(req.SystemPrompt, req.Messages, false))
}

// buildArgsBase 构造参数（不含末尾的位置参数 prompt）——附件场景复用：
// 那条路提示词走 stdin，不能再作为命令行参数传。
func (e *CodeBuddyEngine) buildArgsBase(req Request) []string {
	args := []string{"--print", "--output-format", "json"}
	switch {
	case req.SessionID != "":
		// 续接指定会话（commander 风格空格传值）。默认新会话不传
		// --no-session-persistence：不落盘的会话无法续接。
		args = append(args, "--resume", req.SessionID)
	case req.Continue:
		// 续接最近一次会话（无需 id）。
		args = append(args, "--continue")
	}
	// 工具模式决定两件事：CLI 侧的工具白名单，以及 system prompt 是否
	// 追加 noToolSuffix。
	//
	// ⚠️ noToolSuffix 只在 off 模式注入。它明文写着「严禁使用任何工具」，
	// 与 on / 白名单模式的目标直接冲突 —— 之前无条件注入，导致
	// `--tools on` 表面开了工具、system prompt 却在压制模型调用，
	// 表现为「工具启用无效」。enable 模式下必须让 system prompt 干净。
	tools := toolsOrDefault(req.Tools)
	switch {
	case tools.IsOff():
		// 关工具：不仅 CLI 侧禁掉，system prompt 也要堵住模型「伪工具调用」
		// （实测 off 且不注入时，模型会输出 <tool_calls:xxxx> 这类假标签）。
		args = append(args, "--tools", "")
		args = append(args, "--append-system-prompt", noToolSuffix)
	case tools.IsOn():
		// 四档权限档位：codebuddy 与 claude 同族，--permission-mode 取值完全一致
		//（default / acceptEdits / auto / bypassPermissions），故复用同一映射。
		// 改造前这里恒传 -y（= --dangerously-skip-permissions，第 4 档）。
		args = append(args, "--permission-mode", claudePermissionMode(req.Permission))
	default:
		args = append(args, "--tools", strings.Join(tools.Allowlist(), ","),
			"--permission-mode", claudePermissionMode(req.Permission))
	}
	if m := stripModelPrefix(req.Model); m != "" {
		args = append(args, "--model", m)
	} else {
		// 默认模型 hy3；空串 = CLI 自身默认（几乎不用，保底语义）。
		if DefaultCodeBuddyModel != "" {
			args = append(args, "--model", DefaultCodeBuddyModel)
		}
	}
	// 统一参数矩阵：`--settings` 只接受一份载荷，故 MaxTokens 的 env 注入与
	// 四档模型的 sandbox / autoMode / permissions 合并进同一个 JSON。
	// 注意 codebuddy 的沙箱键集与 claude 略有差异（文档未列 failIfUnavailable），
	// 由 sandboxSettings 按引擎区分。
	if payload, ok := agentSettingsPayload(req, "codebuddy"); ok {
		args = append(args, "--settings", payload)
	}
	return args
}

// Stream 实现 Streamer：流式调用 codebuddy CLI。
// 协议与 claude 同源（CodeBuddy Code 系 stream-json）。
func (e *CodeBuddyEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return StreamResult{}, fmt.Errorf("codebuddy CLI not found; set MAGIC_AGENT_CODEBUDDY_BIN")
	}

	args := e.buildArgs(req)
	if req.Append != nil || hasImageAttachment(req.Attachments) {
		// 附件 / 常驻会话 → stream-json 输入通道：提示词改走 stdin（见 streamjson.go）。
		prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
		var stdin io.Reader
		if req.Append != nil {
			pipe, perr := streamJSONInputPipe(prompt, req.Attachments, req.Append)
			if perr != nil {
				return StreamResult{}, perr
			}
			stdin = pipe
		} else {
			line, serr := streamJSONUserLine(prompt, req.Attachments)
			if serr != nil {
				return StreamResult{}, serr
			}
			stdin = strings.NewReader(line)
		}
		args = streamJSONArgs(e.buildArgsBase(req), prompt, true)
		acc := &streamAccumulator{Engine: e.Name(), OnEvent: onEvent}
		var fin streamJSONResult
		seen, err := runStreamJSONIn(ctx, req.Workspace, nil, bin, args, stdin, acc, &fin)
		if err != nil {
			return StreamResult{}, err
		}
		if !seen {
			return StreamResult{}, fmt.Errorf("codebuddy CLI stream ended without result line")
		}
		if fin.IsError {
			return StreamResult{}, fmt.Errorf("codebuddy CLI error (subtype=%s): %s", fin.Subtype, truncateStr(fin.Result, 500))
		}
		text := stripUserQueryEcho(finalizeStreamText(fin, acc))
		if strings.TrimSpace(text) == "" {
			return StreamResult{}, fmt.Errorf("codebuddy CLI 返回内容仅为请求回显（无模型正文）")
		}
		return StreamResult{
			Response: Response{
				Engine:    e.Name(),
				Text:      text,
				Model:     fin.Model,
				SessionID: fin.SessionID,
				Latency:   time.Since(start),
			},
			Thinking: acc.Thinking.String(),
			Tools:    acc.Tools,
		}, nil
	}
	for i := range args {
		if args[i] == "--output-format" {
			args[i+1] = "stream-json"
			break
		}
	}
	args = append(args, "--include-partial-messages", "--verbose")

	acc := &streamAccumulator{Engine: e.Name(), OnEvent: onEvent}
	var fin struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		IsError   bool   `json:"is_error"`
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
		Model     string `json:"model"`
	}
	seenResult := false

	err := runStreamCLIIn(ctx, req.Workspace, bin, args, func(line string) error {
		isResult, perr := acc.handleNDJSONLine(line)
		if perr != nil {
			return perr
		}
		if isResult {
			_ = json.Unmarshal([]byte(line), &fin)
			seenResult = true
		}
		return nil
	})
	if err != nil {
		return StreamResult{}, err
	}
	if !seenResult {
		return StreamResult{}, fmt.Errorf("codebuddy CLI stream ended without result line")
	}
	if fin.IsError {
		return StreamResult{}, fmt.Errorf("codebuddy CLI error (subtype=%s): %s", fin.Subtype, truncateStr(fin.Result, 500))
	}

	text := stripUserQueryEcho(fin.Result)
	if strings.TrimSpace(text) == "" {
		text = strings.TrimSpace(acc.Text.String())
	}
	if strings.TrimSpace(text) == "" {
		return StreamResult{}, fmt.Errorf("codebuddy CLI 返回内容仅为请求回显（无模型正文）")
	}
	return StreamResult{
		Response: Response{
			Engine:    e.Name(),
			Text:      text,
			Model:     fin.Model,
			SessionID: fin.SessionID,
			Latency:   time.Since(start),
		},
		Thinking: acc.Thinking.String(),
		Tools:    acc.Tools,
	}, nil
}

// stripUserQueryEcho 剥离 <user_query>…</user_query> 请求回显。
func stripUserQueryEcho(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "<user_query>") {
		return s
	}
	end := strings.Index(t, "</user_query>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(t[end+len("</user_query>"):])
}

// extractCodeBuddyResult 解析 codebuddy 输出，兼容三种形态：
//
//  1. 单对象 envelope {"type":"result","result":"..."}
//  2. 数组：优先 type=="result"，否则拼接全部 assistant text
//  3. 解析失败原样返回
func extractCodeBuddyResult(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}

	// Shape 1: 单对象。
	var single struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if trimmed[0] == '{' {
		if err := json.Unmarshal([]byte(trimmed), &single); err == nil {
			if single.Result != "" {
				return single.Result
			}
		}
	}

	// Shape 2: stream 消息数组。
	if trimmed[0] == '[' {
		var msgs []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &msgs); err == nil {
			for _, raw := range msgs {
				var m struct {
					Type   string `json:"type"`
					Result string `json:"result"`
				}
				if json.Unmarshal(raw, &m) == nil && m.Type == "result" && m.Result != "" {
					return m.Result
				}
			}
			var sb strings.Builder
			for _, raw := range msgs {
				var probe map[string]json.RawMessage
				if json.Unmarshal(raw, &probe) != nil {
					continue
				}
				if r, ok := probe["role"]; ok && string(r) == `"assistant"` {
					if text := extractAssistantText(probe); text != "" {
						if sb.Len() > 0 {
							sb.WriteString("\n")
						}
						sb.WriteString(text)
					}
				}
				if r, ok := probe["type"]; ok && string(r) == `"assistant"` {
					if text := extractAssistantText(probe); text != "" {
						if sb.Len() > 0 {
							sb.WriteString("\n")
						}
						sb.WriteString(text)
					}
				}
			}
			if sb.Len() > 0 {
				return sb.String()
			}
		}
	}

	// Shape 3: 原样返回。
	return raw
}

// extractCodeBuddySession 从 codebuddy 输出里挖 session_id（尽力而为），
// 兼容单对象 envelope 与 stream 消息数组两种形态；找不到返回空串。
func extractCodeBuddySession(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	// Shape 1: 单对象 envelope。
	if trimmed[0] == '{' {
		var single struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(trimmed), &single) == nil {
			return single.SessionID
		}
		return ""
	}
	// Shape 2: stream 消息数组 —— 优先 type=="result" 行，其次任意带
	// session_id 的行。
	if trimmed[0] == '[' {
		var msgs []json.RawMessage
		if json.Unmarshal([]byte(trimmed), &msgs) != nil {
			return ""
		}
		fallback := ""
		for _, m := range msgs {
			var probe struct {
				Type      string `json:"type"`
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(m, &probe) != nil || probe.SessionID == "" {
				continue
			}
			if probe.Type == "result" {
				return probe.SessionID
			}
			if fallback == "" {
				fallback = probe.SessionID
			}
		}
		return fallback
	}
	return ""
}

// extractAssistantText 从单条 stream 消息里挖 assistant 可见文本。
func extractAssistantText(msg map[string]json.RawMessage) string {
	var sb strings.Builder
	var containers []json.RawMessage
	// "message" 可能是 {"content":[...]}（包一层），也可能直接是 [...]。
	if v, ok := msg["message"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(v, &inner); err == nil {
			if c, ok := inner["content"]; ok {
				containers = append(containers, c)
			}
		} else {
			containers = append(containers, v)
		}
	}
	if v, ok := msg["content"]; ok {
		containers = append(containers, v)
	}
	for _, c := range containers {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(c, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			t := b.Type
			if t == "" || t == "text" || t == "output_text" {
				if b.Text != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(b.Text)
				}
			}
		}
	}
	return sb.String()
}
