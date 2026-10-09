package agent

// claude.go - Claude Code CLI 引擎。
//
// 后端：claude（Claude Code，本机 /opt/homebrew/bin/claude）。
//
// ## 提示词一律走 stdin（2026-10-01 改，Windows 实测）
//
// 以前用 `-p <prompt>` 把提示词当**位置参数**传。实测在 Windows 上这是
// 结构性不可靠的：npm 装的 claude 是 `claude.cmd` 批处理，含换行的位置参数
// 会被 cmd.exe 拆坏，子进程拿不到完整 prompt 就**静默退 0、零输出**
//（对照实验：同一命令单行 20 行 / 含换行 0 行；非流式同样整段丢内容）。
//
// 现在 Complete 与 Stream **都**走 `--input-format stream-json` 的 stdin
// 通道（streamjson.go），提示词永不作为 argv。unix 上协议完全相同，
// 故**不加平台分支** —— 一条路径两平台通吃。
//
// 收尾正文从 NDJSON 的 result 行取（这条协议要求 --output-format 也是
// stream-json），故连非流式的 Complete 也要归约一遍事件。
//
// ## 关键 flags：
//
//	--model <m>                    传裸模型名（sonnet / opus / claude-sonnet-4-6...）
//	--tools ""                     禁用全部内置工具，纯 chat 一次成型
//	                                 （Windows 上经 launch.go 改写成 --tools=，
//	                                  空串作独立参数会被 cmd.exe 丢弃）
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

	// 提示词走 stdin 的 stream-json 通道（**所有**调用，含无附件的纯文本）。
	//
	// 为什么不再用 `-p <prompt>` 位置参数（2026-10-01 改，Windows 实测）：
	// 位置参数在 Windows 上会被 .cmd 批处理拆坏 —— 含换行时整段内容丢失，
	// claude 收到空 prompt 后回一句 "How can I help you today?"（实测）。
	// unix 上位置参数本来能用，但统一走 stdin 可让两平台行为完全一致，
	// 省掉一类「只在某个平台复现」的问题。代价是要多解析一层 NDJSON，
	// 而 streamJSONArgs/runStreamJSONIn 已是附件路径的现成实现。
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
		// 补终态事件（2026-10-02）：非流式路径通常没有 onEvent，nil 时静默跳过。
		// 有 onEvent 的场景（观察者）才能看到「这一轮异常结束」而不是凭空消失。
		err := fmt.Errorf("claude CLI stream ended without result line")
		acc.emitTurnFailed(acc.SessionID, err)
		return Response{}, err
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

// buildArgs 构造 claude CLI 参数（含末尾的位置参数 prompt）。
//
// ⚠️ 2026-10-01 起**生产路径已不再用它**：Complete 与 Stream 都把提示词
// 走 stdin（见各自注释里的理由），只有测试与「显式要位置参数」的场合还可能用。
// 保留是因为 buildArgsBase 的语义（"不含位置参数"）仍被 streamJSONArgs 依赖。
func (e *ClaudeEngine) buildArgs(req Request, prompt string) []string {
	return append(e.buildArgsBase(req), prompt)
}

// buildArgsBase 构造参数（不含末尾的位置参数 prompt）——提示词走 stdin 的路径复用它。
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
		// ⚠️ 2026-10-01：必须显式 --tools default —— 只传 --permission-mode
		// 不传 --tools 时，claude CLI 在 stream-json 模式下不发 result 行
		// （实测）：表现为「stream ended without result line」，魔法端把整轮
		// 标 turn_failed、UI 一句话都渲染不出来。--tools default 才是「开工具」
		// 的正路（与 ""=关、name1,name2=白名单 同列；"default" 是「全部内置」）。
		args = append(args, "--tools", "default",
			"--permission-mode", claudePermissionMode(req.Permission))
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

	// 提示词一律走 stdin 的 stream-json 输入通道（不再当位置参数）。
	//
	// 为什么这是**默认**而不是「附件时才走」（2026-10-01 改，Windows 实测）：
	// 位置参数在 Windows 上是结构性不可靠的 —— npm 装的 claude 是 .cmd 批处理，
	// 含换行的位置参数会被 cmd.exe 拆坏，子进程拿不到完整 prompt 就
	// **静默退 0、零输出**（对照实验：同一命令单行 20 行 / 含换行 0 行）。
	// 不只是流式受损：非流式的 -p 同样整段丢内容。
	// agents-anywhere 对此的处理是提示词全走 SDK stdin（connector/runtimes/claude/
	// sdk/client.py:160 的 query(content)），从架构上就没有这条破路。
	//
	// unix 上走 stdin 同样正确（协议一致），故**不加平台分支** ——
	// 一条路径两平台通吃，避免两套行为漂移。
	//
	// 常驻会话（Request.Append 非 nil）的区别只在 stdin 是个**流**：
	// 追加消息源源不断写进去，通道关闭才 EOF（子进程随之收尾）。
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
	args := streamJSONArgs(e.buildArgsBase(req), prompt, true)
	acc := &streamAccumulator{Engine: e.Name(), OnEvent: onEvent, OnSessionID: req.OnSessionID}
	var fin streamJSONResult
	seen, err := runStreamJSONIn(ctx, req.Workspace, nil, bin, args, stdin, acc, &fin)
	if err != nil {
		return StreamResult{}, err
	}
	if !seen {
		// 补终态事件：流被截断（引擎崩 / 被杀 / 没吐 result）时，已流出去的半截正文
		// 在事件流上永远停在 running —— 消费方看到的是一条悬空 item。
		// 对齐 agents-anywhere 的 failed_terminal_event（见 emitTurnFailed）。
		err := fmt.Errorf("claude CLI stream ended without result line")
		acc.emitTurnFailed(acc.SessionID, err)
		return StreamResult{}, err
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

// truncateStr 截断到最多 n 字节。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
