package agent

// claude.go - Claude Code CLI 引擎。
//
// 后端：claude（Claude Code，本机 /opt/homebrew/bin/claude）。
// 非交互模式：-p（--print）+ --output-format json，拿到稳定 envelope：
//
//	{"type":"result","subtype":"success","result":"<正文>",
//	 "session_id":"...","is_error":false, "total_cost_usd":...}
//
// 关键 flags：
//
//	--model <m>                    传裸模型名（sonnet / opus / claude-sonnet-4-6...）
//	--tools ""                     禁用全部内置工具，纯 chat 一次成型
//	--permission-mode <mode>       四档权限档位（见 permission.go）：
//	                               manual → default；accept-edits → acceptEdits；
//	                               auto → auto；full → bypassPermissions（默认档）
//	-r, --resume <id>              续接指定会话（Request.SessionID 非空时传）。
//	-c, --continue                 续接当前目录最近一次会话（Request.Continue）。
//	                               注意：默认新会话不传 --no-session-persistence，
//	                               否则首轮会话不落盘，返回的 session_id 无法续接
//	                              （claude 帮助明确写着该 flag "cannot be resumed"）
//	--append-system-prompt <s>     注入 system prompt
//	--settings <json>              单份 JSON 载荷，合并注入三件事：
//	                                 ① MaxTokens → env.CLAUDE_CODE_MAX_OUTPUT_TOKENS
//	                                    （claude 无原生 max-tokens flag）
//	                                 ② 四档模型的 sandbox 配置（claude **没有**
//	                                    --sandbox flag，只能走 settings）
//	                                 ③ 第 3 档的 autoMode 受信边界 + deny/ask 规则
//	                               三者必须合并：--settings 不是可重复 flag。
//
// 档位为什么走 --permission-mode 而不是 settings.permissions.defaultMode：
// 官方明确 auto / bypassPermissions 在项目级 .claude/settings.json 与本地级
// .claude/settings.local.json 中**不生效**，只有 --permission-mode（与 --settings
// 的 flag 层）可靠。详见 permission.go。
//
// CLI 路径解析顺序：显式 BinPath → MAGIC_AGENT_CLAUDE_BIN → 常见安装位 → PATH
//（探测链统一收敛在 engine_base.go 的 cliBase）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClaudeEngine 通过 Claude Code CLI 实现 Engine。
type ClaudeEngine struct {
	// BinPath 显式指定 CLI 路径（空 = 自动探测）。测试注入用。
	BinPath string
}

// Name 实现 Engine。
func (e *ClaudeEngine) Name() string { return "claude" }

// DefaultClaudeTimeout 单次尝试默认超时。
const DefaultClaudeTimeout = 5 * time.Minute

// DefaultClaudeModel claude 引擎的默认模型（Claude Code CLI 默认即主模型，
// 显式传空串别名让 CLI 自己决定，不在代理层硬编码具体版本号）。
const DefaultClaudeModel = ""

// bin 探测 claude CLI 路径（委托 cliBase 统一探测链）。
func (e *ClaudeEngine) bin() string {
	return claudeBase.resolve(e.BinPath)
}

// Detect 实现 Engine。
func (e *ClaudeEngine) Detect() (bool, string) {
	p := claudeBase.resolve(e.BinPath)
	if p == "" {
		return false, claudeBase.notFound
	}
	return true, p
}

// claudeSettingsPath 用户级 settings.json 路径（CLAUDE_CONFIG_DIR 优先，
// 与 claude 自身的配置目录约定一致）。
func claudeSettingsPath() string {
	dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, "settings.json")
}

// ListModels 实现 ModelLister。
//
// claude 没有 models 子命令（--help 只有 agents/auth/doctor/mcp/plugin/...），
// 所以清单取自用户配置 ~/.claude/settings.json：顶层 model + env 里
// ANTHROPIC_*MODEL / CLAUDE_CODE_*MODEL（**id 那一格**）的值 —— 这正是 -m 能收的标识
// （别名由 CLI 解析，代理侧不硬编码任何模型名）。同组的 `*_MODEL_NAME` 是显示名，不算
// （见 models.go 的 claudeModelsFromSettings）。
func (e *ClaudeEngine) ListModels(_ context.Context) ([]string, error) {
	path := claudeSettingsPath()
	if path == "" {
		return nil, fmt.Errorf("%w: cannot resolve claude config dir", ErrNoModelSource)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s not found (claude has no models subcommand)", ErrNoModelSource, path)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	models, err := claudeModelsFromSettings(data)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: no model entries in %s", ErrNoModelSource, path)
	}
	return models, nil
}

// claudeResult 对应 --output-format json 的 envelope。
type claudeResult struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	SessionID    string  `json:"session_id"`
	Model        string  `json:"model"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// Complete 实现 Engine：单次调用 claude CLI。
func (e *ClaudeEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return Response{}, fmt.Errorf("claude CLI not found; set MAGIC_AGENT_CLAUDE_BIN")
	}

	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return Response{}, fmt.Errorf("claude: empty prompt")
	}

	// 附件（截图）：claude 的 -p 文本模式收不了图，唯一的原生通道是
	// --input-format stream-json（图片作为 content block 走 stdin）。该模式下
	// --output-format 也必须是 stream-json，所以这里跑一遍流式协议、把事件归约成
	// 单次结果返回（Complete 的语义就是「一次性拿最终答案」，不给调用方发事件）。
	if hasImageAttachment(req.Attachments) {
		return e.completeWithAttachments(ctx, req, prompt, start)
	}

	args := e.buildArgs(req, prompt)

	// workspace：claude 没有工作目录 flag，子进程 cwd 就是原生方式（见 Request.Workspace）
	stdout, stderr, err := runCLIIn(ctx, req.Workspace, bin, args...)
	if err != nil {
		return Response{}, wrapCliError("claude", stdout, stderr, err)
	}

	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return Response{}, fmt.Errorf("claude CLI returned empty output")
	}

	var env claudeResult
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		// 个别版本对 --print + json 仍吐纯文本：原样返回。
		return Response{
			Text:    raw,
			Model:   req.Model,
			Latency: time.Since(start),
		}, nil
	}
	if env.IsError {
		return Response{}, fmt.Errorf("claude CLI error (subtype=%s): %s", env.Subtype, truncateStr(env.Result, 500))
	}
	if env.Result == "" {
		return Response{}, fmt.Errorf("claude CLI returned empty result")
	}
	return Response{
		Text:      env.Result,
		Model:     env.Model,
		SessionID: env.SessionID,
		Latency:   time.Since(start),
	}, nil
}

// completeWithAttachments 走 stream-json 输入通道跑一次带附件的调用，
// 把事件归约成单次 Response（不向调用方发增量事件）。
func (e *ClaudeEngine) completeWithAttachments(ctx context.Context, req Request, prompt string, start time.Time) (Response, error) {
	bin := e.bin()
	args := streamJSONArgs(e.buildArgsBase(req), prompt, false)
	stdin, err := streamJSONUserLine(prompt, req.Attachments)
	if err != nil {
		return Response{}, err
	}
	acc := &streamAccumulator{Engine: e.Name(), OnSessionID: req.OnSessionID}
	var fin streamJSONResult
	seen, err := runStreamJSONIn(ctx, req.Workspace, nil, bin, args, strings.NewReader(stdin), acc, &fin)
	if err != nil {
		return Response{}, wrapCliError("claude", "", "", err)
	}
	if !seen {
		return Response{}, fmt.Errorf("claude CLI stream ended without result line")
	}
	if fin.IsError {
		return Response{}, fmt.Errorf("claude CLI error (subtype=%s): %s", fin.Subtype, truncateStr(fin.Result, 500))
	}
	text := finalizeStreamText(fin, acc)
	if text == "" {
		return Response{}, errEmptyStreamContent("claude")
	}
	return Response{
		Text:      text,
		Model:     fin.Model,
		SessionID: fin.SessionID,
		Latency:   time.Since(start),
	}, nil
}

// buildArgs 构造 claude CLI 参数（Complete 与 Stream 共用）。
// outputFormat："json"（单结果 envelope）或 "stream-json"（流式 NDJSON）。
func (e *ClaudeEngine) buildArgs(req Request, prompt string) []string {
	return append(e.buildArgsBase(req), prompt)
}

// buildArgsBase 构造参数（不含末尾的位置参数 prompt）——给附件场景复用：
// 那条路提示词走 stdin，不能再作为命令行参数传（streamJSONArgs 负责剔除）。
func (e *ClaudeEngine) buildArgsBase(req Request) []string {
	// 工具模式 × 权限档位（四档模型见 permission.go）：
	//	off        --tools ""（无工具调用，权限档位无意义，不传）
	//	on         不传 --tools（引擎默认全工具）+ --permission-mode <档位>
	//	allowlist  --tools <names> + --permission-mode <档位>
	// 改造前这里在 on/白名单下恒传 --dangerously-skip-permissions（= 第 4 档），
	// 现在由 Request.Permission 决定；默认档仍是 full，故既有调用方行为不变。
	tools := toolsOrDefault(req.Tools)
	args := []string{"-p", "--output-format", "json"}
	switch {
	case req.SessionID != "":
		// 续接指定会话：首轮必须落盘才有 id 可续，因此默认新会话不传
		// --no-session-persistence（该 flag 的会话 "cannot be resumed"）。
		args = append(args, "--resume", req.SessionID)
	case req.Continue:
		// 续接最近一次会话（无需 id）。
		args = append(args, "--continue")
	}
	switch {
	case tools.IsOff():
		args = append(args, "--tools", "")
	case tools.IsOn():
		args = append(args, "--permission-mode", claudePermissionMode(req.Permission))
	default: // allowlist
		args = append(args, "--tools", strings.Join(tools.Allowlist(), ","),
			"--permission-mode", claudePermissionMode(req.Permission))
	}
	if m := stripModelPrefix(req.Model); m != "" {
		args = append(args, "--model", m)
	} else {
		args = append(args, "--model", DefaultClaudeModel)
	}
	// 统一参数矩阵：`--settings` 只接受**一份**载荷（文件路径或内联 JSON），
	// 所以 MaxTokens 的 env 注入、四档模型的 sandbox / autoMode / permissions
	// 必须合并进同一个 JSON —— 见 permission.go 的 agentSettingsPayload。
	// 沙箱没有 CLI flag（claude 的 flags 表里没有 --sandbox），只能从这里进。
	if payload, ok := agentSettingsPayload(req, "claude"); ok {
		args = append(args, "--settings", payload)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	return args
}

// Stream 实现 Streamer：流式调用 claude CLI（stream-json NDJSON）。
func (e *ClaudeEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return StreamResult{}, fmt.Errorf("claude CLI not found; set MAGIC_AGENT_CLAUDE_BIN")
	}
	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return StreamResult{}, fmt.Errorf("claude: empty prompt")
	}

	// 附件（截图）→ stream-json 输入通道：提示词不再当位置参数，改走 stdin。
	// 常驻会话（Request.Append 非 nil）走同一条通道 —— 区别只在 stdin 是个**流**：
	// 追加消息源源不断写进去，通道关闭才 EOF（子进程随之收尾）。
	args := e.buildArgs(req, prompt)
	if req.Append != nil || hasImageAttachment(req.Attachments) {
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
		acc := &streamAccumulator{Engine: e.Name(), OnEvent: onEvent, OnSessionID: req.OnSessionID}
		var fin streamJSONResult
		seen, err := runStreamJSONIn(ctx, req.Workspace, nil, bin, args, stdin, acc, &fin)
		if err != nil {
			return StreamResult{}, err
		}
		if !seen {
			return StreamResult{}, fmt.Errorf("claude CLI stream ended without result line")
		}
		if fin.IsError {
			return StreamResult{}, fmt.Errorf("claude CLI error (subtype=%s): %s", fin.Subtype, truncateStr(fin.Result, 500))
		}
		text := finalizeStreamText(fin, acc)
		if text == "" {
			return StreamResult{}, errEmptyStreamContent("claude")
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

	// json → stream-json：替换 --output-format 值并追加流式 flags。
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
		return StreamResult{}, fmt.Errorf("claude CLI stream ended without result line")
	}
	if fin.IsError {
		return StreamResult{}, fmt.Errorf("claude CLI error (subtype=%s): %s", fin.Subtype, truncateStr(fin.Result, 500))
	}

	text := fin.Result
	if text == "" {
		text = strings.TrimSpace(acc.Text.String())
	}
	if text == "" {
		return StreamResult{}, fmt.Errorf("claude CLI returned empty result")
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

// truncateStr 截断到最多 n 字节。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
