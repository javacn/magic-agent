package cli

// ask.go - 提问主流程（根命令直用，无子命令）。
//
// 输入来源（优先级：--prompt > args > --file > stdin）：
//	1. -p/--prompt <text>：命令行直接给提示词
//	2. 位置参数拼接：magic-agent "问题"
//	3. --file <path>：文件全文（"-" = stdin）
//	4. stdin 管道（stdin 非 TTY 且无其他输入时自动读取）
// --file 与 -p/args 可组合：文件内容在前，-p/args 在后（附加上下文）。
//
// **提示词可以是文件路径**（用户 2026-09-17：「传了提示词需要支持传入文件路径」）：
// -p / 位置参数 / -s 的值若命中一个已存在的普通文件，就按文件内容用（并打一行 stderr 提示）；
// 想强制按文件读、读不到就报错，写 "@<path>"（-p、位置参数、-s、配置的 systemPrompt 都支持）。
// 判据保守（单行、≤4096 字节、stat 是普通文件），所以 `-p "解释一下 README.md"` 这类
// 正常提示词不受影响；详见 expandPromptSource。
//
// 工具开关（--tools）：
//	off（默认）  禁用全部工具 —— 纯 chat 一次成型，输出可解析
//	on           保持引擎默认工具集 + 权限旁路（agent 模式）
//	<白名单>     逗号分隔工具名（如 Bash,Read），仅允许这些工具
//	             + 权限旁路；引擎不支持的白名单成员忽略。
//
// 会话续接（--session）：
//	默认新会话；把上次输出 envelope 里的 session_id 传回 --session，
//	即可继续同一会话（引擎侧透传各自的 --resume 参数）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/darren/magic-agent/internal/agent"
	"github.com/darren/magic-agent/internal/config"
	"github.com/darren/magic-agent/internal/session"
)

// askOptions 根命令的全部参数。
type askOptions struct {
	engine       string
	model        string
	system       string
	prompt       string
	file         string
	attach       []string // 附件（截图/图片）路径；与提示词一起发给引擎
	tools        string
	timeout      time.Duration
	retries      int
	backoff      time.Duration
	output       string
	verbose      bool
	engines      bool
	contract     bool // 输出带版本的契约 envelope（--contract；客户端启动校验用）
	noModels     bool
	login        string // 拉起某引擎自己的交互式登录会话（--login <engine>）
	events       bool   // --stream 的事件流带契约版本与行号（--events；客户端消费用）
	control      bool   // 从 stdin 读 NDJSON 控制命令（--control；打断 / 收工 / 回审批）
	jsonOut      bool
	stream       bool
	noThinking   bool
	maxTokens    int
	temperature  float64
	tempSet      bool
	jsonSchema   string // 内联 JSON Schema（仅 llm 引擎走 --schema）
	session      string // 会话续接 id（空 = 新会话，默认）
	continueF    bool   // 续接最近一次会话（-c/--continue；不需要 id）
	workspace    string // 工作目录（-w/--workspace；空 = 用调用方 cwd）
	stop         string // 停止指定会话/运行（--stop <session_id|run_id>）
	listSessions bool   // 列出会话登记表（--sessions）
	// sessionLog 读一段会话的**事件历史**（--session-log <session_id|run_id>）。
	// 为什么要有它：会话的「查询」能力原先只在 HTTP 插件里（/desk/session/{id}/messages），
	// 而 magic-agent 自己不跑服务、由调用方（如掌天瓶）直接 exec CLI —— 所以读历史
	// 必须也是一条 CLI 命令，否则调用方拿不到历史，只能自己解析磁盘格式。
	sessionLog string
	// logAfter 增量续读游标（--after <seq>，仅配合 --session-log）：
	// 调用方报「我已看到 seq=N」，只回 seq > N 的部分。断线重连时不必重传整段历史。
	logAfter  uint64
	keepAlive bool          // 常驻会话：首轮结束后不退出，等 --append 追加（需配合 --stream）
	appendTo  string        // 向常驻会话追加消息（--append <session_id|run_id>）
	idle      time.Duration // 常驻会话空闲收工时长（--idle，仅 --keep-alive 有效）

	// 四档权限模型（见 internal/agent/permission.go）。仅 claude / codebuddy 接线。
	permission     string   // 档位：manual | accept-edits | auto | full
	sandboxExclude []string // 始终在沙箱外执行的命令（sandbox.excludedCommands）
	sandboxDomain  []string // 沙箱网络白名单（sandbox.network.allowedDomains）
	autoModeEnv    []string // 第 3 档分类器的受信边界描述（autoMode.environment）
	permDeny       []string // 追加 deny 规则（permissions.deny，所有档位生效）
	permAsk        []string // 追加 ask 规则（permissions.ask，强制人工审批）
}

// newAskOptions 返回带默认值的选项集。
//   - 默认输出 json：固定单行 envelope，程序解析最友好；人看用 -o text。
//   - 默认引擎 codebuddy：hy3 免费模型开箱即用。
//   - 默认超时 600s：10 分钟，覆盖 codebuddy 慢响应场景。
//   - 默认常驻（keepAlive）：claude/codebuddy 的流式调用自动成为常驻会话，
//     任务跑着的时候就能 `--append` 追加需求（用户 2026-09-18：「keep-alive要是默认的」）。
//     用 --keep-alive=false 关掉；非流式 / 不支持追加的引擎会自动忽略它。
func newAskOptions() *askOptions {
	return &askOptions{
		engine:    "codebuddy",
		output:    "json",
		timeout:   600 * time.Second,
		keepAlive: true,
	}
}

// bindAskFlags 把全部 flags 注册为根命令的 persistent flags。
func bindAskFlags(cmd *cobra.Command, opts *askOptions) {
	f := cmd.PersistentFlags()
	f.StringVarP(&opts.engine, "engine", "e", "codebuddy", "引擎: codebuddy | codebuddy-ai | claude | trae | llm | codex | openclaw | dsh | arkclaw（默认 codebuddy）")
	f.StringVarP(&opts.model, "model", "m", "", "模型（空 = 引擎默认：codebuddy=hy3；codebuddy-ai=CLI 自选默认；llm 引擎传 llm CLI 注册名，如 minimax-m3；dsh/arkclaw 不透传）")
	f.StringVarP(&opts.system, "system", "s", "", "系统提示词（空 = 用配置文件 ~/.config/magic-agent/config.json 的 systemPrompt；两者都为空则不注入）。值若是文件路径或 @文件 → 读该文件内容")
	f.StringVarP(&opts.prompt, "prompt", "p", "", "提示词（值若是文件路径或 @文件 → 读该文件内容作为提示词）")
	f.StringVarP(&opts.workspace, "workspace", "w", "", "工作目录（workspace）：在该目录里执行引擎；codex 走原生 -C，claude/codebuddy/trae/dsh 用子进程 cwd，llm/arkclaw/openclaw 不支持（忽略并提示）")
	f.StringVarP(&opts.file, "file", "f", "", "从文件读 prompt（\"-\" = stdin）")
	f.StringArrayVarP(&opts.attach, "attach", "a", nil, "附件路径（截图/图片等），可重复或逗号分隔；与提示词一起发给引擎（各引擎落地方式见 --engines 的 attachments 字段）")
	f.StringVar(&opts.tools, "tools", "off", "工具开关: off | on | 逗号分隔白名单(如 Bash,Read)")
	f.StringVar(&opts.session, "session", "", "会话续接 id（空=新会话；传入上次输出里的 session_id 继续同一会话；arkclaw 传 contextId；dsh 不支持续接）")
	f.BoolVarP(&opts.continueF, "continue", "c", false, "续接当前目录最近一次会话（不需要 session id；与 --session 同时给时 --session 优先）")
	f.StringVar(&opts.stop, "stop", "", "停止指定会话/运行：传 session_id 或 run_id（--sessions 可见），杀掉它的引擎进程组")
	f.BoolVar(&opts.listSessions, "sessions", false, "列出会话登记表（JSON 数组：run_id / session_id / pid / engine / state / 起止时间）")
	/* 会话历史的**读取**命令。与 --sessions（列表）、--stop、--append 同一族：
	   调用方（掌天瓶等）exec CLI 就能拿到会话数据，不需要 magic-agent 跑任何服务。 */
	f.StringVar(&opts.sessionLog, "session-log", "", "读一段会话的事件历史（JSON：{session,events}）。传 session_id 或 run_id（--sessions 可见）")
	f.Uint64Var(&opts.logAfter, "after", 0, "配合 --session-log：增量续读，只回 seq > N 的事件（另附 count/after/lastSeq/snapshotRequired）")
	f.BoolVar(&opts.keepAlive, "keep-alive", true, "常驻会话：首轮结束后不退出、等 --append 追加（需 --stream）。claude/codebuddy 默认开；dsh 默认关（显式传 --keep-alive 开启）；其余引擎不支持")
	f.StringVar(&opts.appendTo, "append", "", "向常驻会话追加一条消息：传 session_id 或 run_id，内容用 -p/位置参数给")
	f.DurationVar(&opts.idle, "idle", 5*time.Minute, "常驻会话空闲收工时长（默认 5m；0 = 本轮结束就收工，追加窗口只在任务运行期间）")
	f.IntVar(&opts.maxTokens, "max-tokens", 0, "输出 token 上限（0=不指定；claude/codebuddy 经 --settings 注入，llm 透传，trae/dsh 忽略）")
	f.Float64Var(&opts.temperature, "temperature", -1, "采样温度（-1=不指定；仅 llm 引擎透传，其余引擎忽略）")
	f.StringVar(&opts.jsonSchema, "json-schema", "", "JSON Schema 内联字符串（llm / openclaw / dsh / arkclaw 引擎；启用结构化输出与 JSON 后处理）")
	f.DurationVarP(&opts.timeout, "timeout", "t", 600*time.Second, "单次尝试超时（默认 600s=10m）")
	f.IntVarP(&opts.retries, "retries", "r", 0, "失败重试次数（默认 0）")
	f.DurationVar(&opts.backoff, "backoff", 2*time.Second, "首次重试退避间隔（指数翻倍，上限 30s）")
	f.StringVarP(&opts.output, "output", "o", "json", "输出格式: json | text")
	f.BoolVarP(&opts.verbose, "verbose", "v", false, "重试过程打印到 stderr")
	f.BoolVar(&opts.engines, "engines", false, "列出支持的引擎、本机 CLI 可用性与各引擎当前支持的模型（JSON 数组；每行含 capabilities / models / workspace / streaming 能力字段；不可用的引擎带 install 一键安装命令）")
	f.BoolVar(&opts.contract, "contract", false, "输出桌面/移动客户端契约：{\"contractVersion\":N,\"engines\":[...]}（engines 与 --engines 同构，另含 capabilities 静态能力字段）。默认不探测模型（快）；要模型写 --no-models=false")
	f.BoolVar(&opts.noModels, "no-models", false, "配合 --engines / --contract：跳过各引擎的模型探测（只列引擎与可用性，不启动 CLI）")
	f.StringVar(&opts.login, "login", "", "拉起指定引擎自己的交互式登录会话（如 codebuddy / codebuddy-ai）：自动带上该引擎的账号环境，进去执行 /login 即可；登录态落在该引擎自己的票据上，两个账号互不顶号")
	f.BoolVar(&opts.jsonOut, "json", false, "兼容保留：--engines 已默认 JSON，本 flag 不再需要")
	f.BoolVar(&opts.stream, "stream", false, "流式输出：正文/思考增量实时打到 stdout（text 模式思考走 stderr）")
	f.BoolVar(&opts.events, "events", false, "配合 --stream：事件流带契约版本与行号（每行加 v / seq，并先发一行 ready），供客户端消费；老消费者不要开（形状与 --stream 不同）")
	f.BoolVar(&opts.control, "control", false, "配合 --stream：从 stdin 读 NDJSON 控制命令（ping / interrupt / stop / answer），让调用方能打断长任务、回审批")
	f.BoolVar(&opts.noThinking, "no-thinking", false, "流式模式下不转发思考过程增量")
	bindPermissionFlags(cmd, opts)
}

// bindPermissionFlags 注册四档权限模型的 flags（仅 claude / codebuddy 生效）。
//
// 默认档 full 是**刻意**的：改造前 claude/codebuddy 在 --tools 非 off 时恒传
// --dangerously-skip-permissions / -y，语义正是第 4 档。把默认值改成别的档位
// 会是一次静默的行为变更（既有调用方的 agent 会突然开始弹审批 / 被沙箱拦）。
// 推荐需要 agent 能力的调用方显式传 `--permission auto`。
func bindPermissionFlags(cmd *cobra.Command, opts *askOptions) {
	f := cmd.PersistentFlags()
	f.StringVar(&opts.permission, "permission", string(agent.DefaultPermissionTier),
		"权限档位: manual | accept-edits | auto | full（默认 full=保持既有行为；仅 claude/codebuddy/codebuddy-ai 生效）\n"+
			"  manual        沙箱开启，只读放行，其余逐项由用户确认\n"+
			"  accept-edits  沙箱开启，编辑放行，命令仍逐条确认\n"+
			"  auto          沙箱开启，越界由内置 LLM Guardian 判定（推荐）\n"+
			"  full          沙箱关闭，命令直接在宿主机执行，无审批")
	f.StringArrayVar(&opts.sandboxExclude, "sandbox-exclude", nil,
		"始终在沙箱外执行的命令（如 docker,watchman）；可重复或逗号分隔（manual/accept-edits/auto 有效）")
	f.StringArrayVar(&opts.sandboxDomain, "sandbox-domain", nil,
		"沙箱网络白名单域名；可重复或逗号分隔（留空 = 不改动引擎自身网络策略）")
	f.StringArrayVar(&opts.autoModeEnv, "auto-mode-env", nil,
		"第 3 档分类器的受信边界（自然语言，可重复）。如 \"Source control: github.example.com/acme-corp\"")
	f.StringArrayVar(&opts.permDeny, "permission-deny", nil,
		"追加 deny 规则，如 'Bash(rm -rf *)'；在所有档位（含 full）都生效且不可被白名单覆盖；可重复")
	f.StringArrayVar(&opts.permAsk, "permission-ask", nil,
		"追加 ask 规则，如 'Bash(git push *)'；命中即强制人工审批（第 3 档下分类器也无法自动放行）；可重复")
}

// workspaceUnsupportedWarn 引擎没有文件系统语义（llm / arkclaw）时的提示文案；
// 支持的引擎返回空串（无需提示）。抽成函数便于单测，也把「不支持要说明白」变成可断言的行为。
func workspaceUnsupportedWarn(engine string) string {
	if agent.WorkspaceSupportOf(engine) != "none" {
		return ""
	}
	return fmt.Sprintf("magic-agent: 警告：%s 引擎不支持指定工作目录，-w/--workspace 已忽略"+
		"（各引擎能力见 --engines 的 workspace 字段）\n", engine)
}

// resolveWorkspaceDir 解析 -w/--workspace：展开 ~、转绝对路径、校验存在且是目录。
// 空串原样返回（= 不指定，用调用方 cwd）。
// 路径不存在时提前报错 —— 否则会在 spawn 阶段以 "chdir ...: no such file or directory"
// 的形式冒出来，指向性差（尤其 `-o json` 下只看到一句 fork/exec 错误）。
func resolveWorkspaceDir(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	abs, err := filepath.Abs(agent.ExpandHome(p))
	if err != nil {
		return "", fmt.Errorf("--workspace: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("--workspace: %s 不可访问: %w", abs, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("--workspace: %s 不是目录", abs)
	}
	return abs, nil
}

// resolveSystemPrompt 决定本轮生效的 system prompt：`-s/--system` 优先
// （其值已由调用方过过 expandPromptSource），未给时回落到配置文件里的全局默认值
// （`~/.config/magic-agent/config.json` 的 `systemPrompt`，路径可用 MAGIC_AGENT_CONFIG 覆盖，
// 也可用 MAGIC_AGENT_SYSTEM_PROMPT 覆盖取值）。两者都为空 → 空串（不注入）。
//
// 配置里的默认值同样支持「文件路径 / @路径」（系统提示词常单独存一个文件），
// 故这里也过一遍 expandPromptSource；返回的 note 非空时调用方应写到 stderr。
//
// 第二个返回值（旧签名的 err）只在「配置文件读不了 / JSON 语法错 / @路径读不到」时非空 ——
// 调用方按「不注入 + 提示」处理，绝不因为一份坏配置让整个调用失败。
func resolveSystemPrompt(flagValue string) (text, note string, err error) {
	if s := strings.TrimSpace(flagValue); s != "" {
		return s, "", nil
	}
	cfg, cerr := config.Load()
	if cerr != nil {
		return "", "", cerr
	}
	v := strings.TrimSpace(cfg.SystemPrompt)
	if v == "" {
		return "", "", nil
	}
	return expandPromptSource(v)
}

// maxPromptPathLen 自动识别「提示词其实是个文件路径」时的长度上限。
// 超过这个长度不可能是路径，直接按文本用（也免去一次 stat）。
const maxPromptPathLen = 4096

// expandPromptSource 把「可能是文件路径」的提示词输入展开成文本。
//
// 用户 2026-09-17 的要求：「传了提示词需要支持传入文件路径」。支持的写法：
//
//	"@<path>"   显式按文件读取 —— 读不到/不是普通文件直接报错（调用方明确要的就是文件）
//	"<path>"    值恰好命中一个**已存在的普通文件** → 按文件内容读取，并给一行 stderr 提示
//	其余        原样文本
//
// 自动识别刻意保守，避免把正常提示词误当路径：必须不含换行、长度 ≤ maxPromptPathLen、
// stat 出来是普通文件（目录不算）、可读。命中时**一定**在 stderr 说明「已按文件读取」，
// 不静默 —— 否则用户会奇怪「模型怎么收到了文件内容」。
//
// 返回的 note 非空时，调用方应把它原样写到 stderr。
func expandPromptSource(s string) (text, note string, err error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return s, "", nil
	}
	forced := strings.HasPrefix(raw, "@")
	p := raw
	if forced {
		p = strings.TrimSpace(strings.TrimPrefix(raw, "@"))
		if p == "" {
			return "", "", fmt.Errorf("@ 后缺少文件路径")
		}
	} else if strings.ContainsAny(raw, "\r\n") || len(raw) > maxPromptPathLen {
		return s, "", nil // 多行 / 超长：一定是提示词正文，不做路径识别
	}

	abs, aerr := filepath.Abs(agent.ExpandHome(p))
	if aerr != nil {
		if forced {
			return "", "", fmt.Errorf("解析提示词文件路径 %s: %w", p, aerr)
		}
		return s, "", nil
	}
	st, serr := os.Stat(abs)
	switch {
	case serr != nil:
		if forced {
			return "", "", fmt.Errorf("读取提示词文件 %s: %w", abs, serr)
		}
		return s, "", nil
	case !st.Mode().IsRegular():
		if forced {
			return "", "", fmt.Errorf("提示词文件 %s 不是普通文件（目录？）", abs)
		}
		return s, "", nil
	}
	data, rerr := os.ReadFile(abs)
	if rerr != nil {
		if forced {
			return "", "", fmt.Errorf("读取提示词文件 %s: %w", abs, rerr)
		}
		return s, "", nil
	}
	note = fmt.Sprintf("magic-agent: 提示：提示词输入命中文件，已按文件内容使用：%s（%d 字节）\n", abs, len(data))
	return strings.TrimSpace(string(data)), note, nil
}

// maxAttachBytes 单个附件大小上限 —— base64 之后还要胖 1/3，且要整条进 HTTP 请求体，
// 32MB 已远超「截图」量级，超过基本是误把大文件当附件。
const maxAttachBytes = 32 << 20

// resolveAttachments 展开 -a/--attach：拆逗号 → 展开 ~ → 绝对路径 → 校验（普通文件 + 体积）。
//
// 为什么在这里校验：路径错误要尽早以 exit 2 报出来（而不是等到引擎层变成
// 一句含糊的 "open ...: no such file"），且引擎实现因此可以信任 Path 是干净的绝对路径。
// 重复路径去重（同一张图手滑写两遍不必发两遍）。
func resolveAttachments(values []string) ([]agent.Attachment, error) {
	var out []agent.Attachment
	seen := make(map[string]bool)
	for _, v := range values {
		for _, raw := range strings.Split(v, ",") {
			p := strings.TrimSpace(raw)
			if p == "" {
				continue
			}
			abs, err := filepath.Abs(agent.ExpandHome(p))
			if err != nil {
				return nil, fmt.Errorf("--attach: %w", err)
			}
			st, err := os.Stat(abs)
			if err != nil {
				return nil, fmt.Errorf("--attach: %s 不可访问: %w", abs, err)
			}
			if !st.Mode().IsRegular() {
				return nil, fmt.Errorf("--attach: %s 不是普通文件", abs)
			}
			if st.Size() > maxAttachBytes {
				return nil, fmt.Errorf("--attach: %s 超过 %dMB 上限", abs, maxAttachBytes>>20)
			}
			if seen[abs] {
				continue
			}
			seen[abs] = true
			out = append(out, agent.NewAttachment(abs))
		}
	}
	return out, nil
}

// attachmentPromptFallbackWarn 引擎没有附件输入通道（AttachmentSupportOf == "prompt"）时的提示：
// 附件改成「把路径写进提示词」，需要引擎自己能读文件 —— 因此 --tools off 时必须点明。
// 抽成函数便于单测，也把「不静默降级」变成可断言的行为。
//
// 「请改用 --tools on」这句只对**真的有 --tools 落地通道**的引擎说（ToolsSwitchableOf）：
// dsh / openclaw 这类自带工具循环的引擎传 --tools 也不改变行为，说了等于误导。
func attachmentPromptFallbackWarn(engine string, count int, toolsOff bool) string {
	if count == 0 || agent.AttachmentSupportOf(engine) != "prompt" {
		return ""
	}
	msg := fmt.Sprintf("magic-agent: 警告：%s 引擎没有附件输入通道，%d 个附件已改为「把路径写进提示词」", engine, count)
	if toolsOff && agent.ToolsSwitchableOf(engine) {
		msg += "；当前 --tools off，引擎读不到这些文件，请改用 --tools on"
	}
	return msg + "（各引擎能力见 --engines 的 attachments 字段）\n"
}

// parseToolsMode 解析 --tools 参数为结构化模式。
func parseToolsMode(s string) (agent.ToolsMode, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "off", "none", "false":
		return agent.ToolsOff, nil
	case "on", "true", "default":
		return agent.ToolsOn, nil
	default:
		// 白名单：逗号/空格分隔。
		names := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		if len(names) == 0 {
			return agent.ToolsOff, &usageError{fmt.Errorf("invalid --tools value %q (want off | on | comma-separated tool names)", s)}
		}
		clean := make([]string, 0, len(names))
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" {
				clean = append(clean, n)
			}
		}
		if len(clean) == 0 {
			return agent.ToolsOff, &usageError{fmt.Errorf("invalid --tools value %q (no tool names parsed)", s)}
		}
		return agent.ToolsAllowlist(clean), nil
	}
}

// resolvePermissionTier 解析并校验 --permission（四档权限模型，见 agent/permission.go）。
//
// 两道校验：
//  1. 取值必须是四档之一（含别名与数字档位）；
//  2. 引擎必须已接线 —— 未接线时**报错**（exit 2），不静默忽略。
//     静默忽略一个安全设置是最坏的结果：用户以为自己被保护着，实际没有。
//     注意只在「显式传过 --permission」时才做第 2 道校验，这样默认值不会
//     让 `-e codex` 这类既有调用突然失败。
func resolvePermissionTier(engineName, raw string, explicit bool) (agent.PermissionTier, error) {
	tier, ok := agent.ParsePermissionTier(raw)
	if !ok {
		return "", &usageError{fmt.Errorf("invalid --permission %q (want one of: %s)",
			raw, strings.Join(agent.PermissionTiers(), " | "))}
	}
	if explicit && !agent.PermissionSupported(engineName) {
		return "", &usageError{fmt.Errorf(
			"--permission 暂不支持 %s 引擎（当前仅 claude、codebuddy、codebuddy-ai；各引擎能力见 --engines 的 permission 字段）",
			engineName)}
	}
	return tier, nil
}

// runAsk ask 主流程（非流式）。
func runAsk(cmd *cobra.Command, args []string, opts *askOptions) error {
	engine, format, req, err := prepareAsk(cmd, args, opts)
	if err != nil {
		return err
	}

	// 会话登记 + spawn 钩子：把引擎子进程 pid 记下来，之后任何进程都能
	// `magic-agent --stop <session_id>` 精确停掉它（见 internal/session）。
	h := beginSession(engine.Name(), req)
	ctx := withSessionHook(cmd.Context(), h)

	runner := &agent.Runner{
		Engine:  engine,
		Timeout: opts.timeout,
		Retries: opts.retries,
		Backoff: opts.backoff,
		Verbose: opts.verbose,
	}
	resp, err := runner.Run(ctx, req)
	if err != nil {
		finishSession(h, "", session.StateFailed)
		attempts := 1 + opts.retries
		// WriteError 已把失败信息（json envelope / text 行）写到 stderr，
		// 标记已输出，Execute 不再重复打印。
		if werr := agent.WriteError(cmd.ErrOrStderr(), format, engine.Name(), attempts, err); werr == nil {
			return &reportedError{err}
		}
		return err
	}
	finishSession(h, resp.SessionID, session.StateDone)
	return agent.WriteOutput(cmd.OutOrStdout(), format, resp)
}

// beginSession 落一条 running 记录（失败不阻断调用：登记表只是可运维性）。
// PID 先填自身 pid 兜底 —— HTTP 直连这类没有子进程的引擎也能被 --stop 停掉
// （杀 magic-agent 自己即中止该次调用）；子进程起来后 spawn 钩子会覆盖成引擎 pid。
func beginSession(engine string, req agent.Request) *session.Handle {
	h, err := session.Begin(session.BeginOptions{
		Engine:     engine,
		Model:      req.Model,
		Workspace:  req.Workspace,
		SessionID:  req.SessionID,
		PromptHead: firstUserText(req.Messages),
		PID:        os.Getpid(),
	})
	if err != nil {
		return nil
	}
	setActiveSession(h)
	return h
}

// withSessionHook 给 ctx 挂上「子进程已启动」回调（h 为空时原样返回）。
func withSessionHook(ctx context.Context, h *session.Handle) context.Context {
	if h == nil {
		return ctx
	}
	return agent.WithSpawnHook(ctx, func(pid int) { h.SetPID(pid, true) })
}

// finishSession 收尾：写最终状态 + 回填会话 id，并清掉「当前活动会话」。
func finishSession(h *session.Handle, sessionID, state string) {
	if h == nil {
		return
	}
	h.Finish(sessionID, state)
	clearActiveSession(h)
}

// firstUserText 取第一条 user 消息的开头（记录里给人看）。
func firstUserText(msgs []agent.Message) string {
	for _, m := range msgs {
		if m.Role == "user" || m.Role == "" {
			return m.Content
		}
	}
	return ""
}

// runStreamAsk 流式主流程。
//
// 输出分流（增量实时、无缓冲）：
//
//	text 模式（-o text）：
//	  正文增量 → stdout（拼成连续正文）
//	  思考增量 → stderr（前置 "… " 每行，供 2>/dev/null 静音或 tee 保留）
//	  结尾 stdout 不再重复全文
//
//	json 模式（默认）：
//	  每条增量一行 NDJSON：{"type":"text","text":"…"} / {"type":"thinking","text":"…"}
//	  结尾一行汇总 envelope（含 thinking 全文 + attempts + latency），
//	  jq 逐事件消费或整体 tail 取 result。
//
// 语义差异（相对非流式）：
//   - 不做自动重试（增量已实时发出，重放会重复消费）；-r 被忽略并提示。
//   - 超时照常生效（杀整个 CLI 进程组）。
func runStreamAsk(cmd *cobra.Command, args []string, opts *askOptions) error {
	engine, format, req, err := prepareAsk(cmd, args, opts)
	if err != nil {
		return err
	}
	streamer := agent.AsStreamer(engine)
	if streamer == nil {
		return &usageError{fmt.Errorf("engine %q does not support streaming", engine.Name())}
	}

	if opts.retries > 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "magic-agent: --stream 不支持自动重试，-r 已忽略")
	}

	/* 把调用方**显式**给的 -t 透传给引擎（2026-09-24 修）。
	   以前流式路径从不设 req.Timeout，引擎只能用自己的默认值 —— arkclaw 那个默认是 3 分钟，
	   于是 `-t 600s` 被静默忽略：网关侧要跑 5 分钟的任务（实测「生成周报」304s 才回）必在
	   3 分钟被砍，桌面壳上就是「什么都没显示 · 调用失败 context deadline exceeded」。
	   只在 flagChanged 时透传：没给 -t 就别去覆盖引擎自己的默认（如 llm 的条目级 timeout）。 */
	if flagChanged(cmd, "timeout") {
		req.Timeout = opts.timeout
	}

	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	sink := newEventSink(stdout, engine.Name(), opts.events)

	// 「看历史」持久化：每条事件落盘到 <dir>/<session_id>.jsonl，
	// 移动端进入会话详情时先读这个文件渲染历史，再开 SSE 接续。
	// 不写盘也不会让流失败；所有写盘错误都进 stderr 警告、流继续（写盘是个独立 goroutine）。
	sessionLog, err := newSessionWriterAuto(sessionLogDir)
	if err != nil {
		fmt.Fprintf(stderr, "magic-agent: 准备会话持久化目录失败：%v（流继续）\n", err)
		sessionLog = nil
	} else {
		defer func() { sessionLog.close() }()
	}
	// 把 session_id 推到 writer：CLI 这边的 OnSessionID 回调（引擎流里见到 init 行就调）
	// 写新 id，writer 自己的写盘 goroutine 读出来开文件。
	req.OnSessionID = func(id string) {
		if sessionLog != nil {
			sessionLog.setSessionID(id)
		}
	}

	/* 用户自己的提问也落盘（2026-09-28）。
	   在此之前落盘的**只有引擎事件** —— 于是从列表点进会话，历史里只有 agent 的回复，
	   没有「你问的那句话」（实时发消息时气泡正常，因为那条是客户端自己本地渲染的，
	   不依赖落盘）。移动端渲染器早就预留了 `kind:"user"` 分支，这里一补历史就完整。
	   ⚠️ 位置很关键：放在 OnSessionID **之后**、流开始**之前**。此刻还不知道 session_id，
	      writer 会把它收进 pending；等 id 出现时 pending 会**先于**当前事件被写出 ——
	     所以顺序天然正确，不必在这里自己等 id（等就是把流路径堵住，违反「绝不断流」）。
	   ⚠️ 只落 `user` 角色：system prompt 是调用参数、不是会话内容，落了会让历史里出现
	     用户从没说过的话；assistant 正文已由引擎的 text / turn_end 事件覆盖。
	   ⚠️ 一条 user 消息一条事件：UI 侧一个气泡对应一条，合并成一坨就没法分开渲染。 */
	if sessionLog != nil {
		for _, m := range req.Messages {
			if m.Role != "user" || strings.TrimSpace(m.Content) == "" {
				continue
			}
			sessionLog.push(sessionEvent{Kind: "user", Text: m.Content})
		}
	}

	var onEvent func(agent.StreamEvent)
	wireLogger := func(ev agent.StreamEvent) {
		if sessionLog == nil {
			return
		}
		// 全部 StreamEventKind 都该落盘：thinking / text / tool_use / tool_result /
		// ask / turn_end。turn_end 是「这一轮到底了」的最重要标记（flush 到 UI 的
		// 「一轮结束」线）；历史视图上少一条 turn_end 就看不出「这轮已完」。
		// ⚠️ 这里只走**引擎事件**。用户提问（kind:"user"）与失败（kind:"error"）
		//    不由流事件触发，见上面 runStreamAsk 里的 push 与下面的 error 分支。
		/* ToolKind 与 SSE 流同源（同一个 toolKindOfEvent）：历史回放与实时渲染
		   因此画出同一种卡，而不是各按工具名猜一套（见 agent/toolkind.go）。 */
		sessionLog.push(sessionEvent{
			Kind: string(ev.Kind), Text: ev.Text, Name: ev.Name, ID: ev.ID,
			ToolKind: toolKindOfEvent(ev), Ask: ev.Ask,
		})
	}
	if format == agent.FormatJSON {
		onEvent = func(ev agent.StreamEvent) {
			// --no-thinking 只压制思考过程；工具事件保持透传。
			if ev.Kind == agent.KindThinking && opts.noThinking {
				return
			}
			_ = sink.stream(ev)
			wireLogger(ev)
		}
	} else {
		onEvent = func(ev agent.StreamEvent) {
			switch ev.Kind {
			case agent.KindThinking:
				if opts.noThinking {
					return
				}
				fmt.Fprint(stderr, "… "+ev.Text)
			case agent.KindText:
				fmt.Fprint(stdout, ev.Text)
			case agent.KindToolUse:
				// 与 thinking 对称：工具事件走 stderr，前缀 🔧。
				// 2>/dev/null 可静音；stdout 正文保持干净。
				prefix := "🔧 " + ev.Name + "(" + ev.ID + ") "
				fmt.Fprint(stderr, prefix+ev.Text+"\n")
			case agent.KindToolResult:
				fmt.Fprint(stderr, "   ↳ "+ev.Text+"\n")
			case agent.KindAsk:
				// 「需要用户选择」：一行摘要打到 stderr（与工具事件同一条通道，
				// 2>/dev/null 可静音），stdout 正文保持干净。
				//
				// ⚠️ headless 下 claude 会在模型提问后**自行拒绝**（模型拿不到答案），
				// 要真正作答得把答案作为后续 user 消息补进去（agent.EncodeAskFollowUp）。
				fmt.Fprint(stderr, "❓ "+ev.Text+"\n")
			}
			wireLogger(ev)
		}
	}

	// 超时：常驻会话的存活由 --idle（空闲收工）决定，所以常驻生效时只有调用方**显式**给 -t
	// 才套总超时；否则不设上限（长任务 + 追加不受 600s 默认值限制）。
	ctx := cmd.Context()
	/* --control 要能主动打断：在（可能的）超时之上再套一层可取消的 ctx。
	   打断与超时走同一条路径 —— 引擎进程组由 Stream 侧按 ctx 收拾。 */
	var cancelAll context.CancelFunc
	if opts.control {
		ctx, cancelAll = context.WithCancel(ctx)
		defer cancelAll()
	}
	if !kaEnabled(cmd, opts, engine.Name()) || flagChanged(cmd, "timeout") {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}

	// 会话登记 + spawn 钩子（与 runAsk 同一套：--stop 靠它拿到引擎 pid）。
	h := beginSession(engine.Name(), req)
	ctx = withSessionHook(ctx, h)

	// 常驻会话（默认开，claude/codebuddy 的流式调用）：开追加入口等 --append，
	// 空闲 --idle 后优雅收工（关闭通道 → 引擎 stdin EOF → 正常收尾）。
	var ka *keepAlive
	if kaEnabled(cmd, opts, engine.Name()) {
		var kerr error
		ka, kerr = startKeepAlive(cmd, opts, engine.Name(), h)
		if kerr != nil {
			finishSession(h, "", session.StateFailed)
			return kerr
		}
		defer ka.Close()
		req.Append = ka.appendCh
		onEvent = ka.wrapOnEvent(onEvent)
	}

	/* 控制通道：给调用方一条回传通道（打断 / 收工 / 回审批）。
	   起在常驻会话之后 —— answer 依赖 ka。 */
	var ctl *controlSession
	if opts.control {
		ctl = startControlReader(cmd.InOrStdin(), sink, controlHooks{
			interrupt: func(reason string) { cancelAll() },
			stop: func() {
				if ka != nil {
					ka.Close() // 关追加入口 → 引擎 stdin EOF → 本轮优雅收尾
					return
				}
				cancelAll()
			},
			answer: func(text string) bool { return ka != nil && ka.Push(text) },
		})
		defer ctl.Close()
	}

	// ready 握手（仅 --events）：客户端据此确认「谁在跑、契约版本是多少」。
	_ = sink.ready(req.Model)

	/* ── 会话状态的中途写入（2026-09-29，对齐 anywhere 的 TimelineStatus）──
	   引擎**停下来等用户**（提问 / 工具待授权）时，这条会话仍然活着，但没在跑 ——
	   界面上该显示「等审批」而不是「生成中」。用户作答、或这一轮的任何后续事件到达时
	   回到 running。落点选在这里，是因为它同时罩住两条 onEvent（--stream 与 --events），
	   不必在两个分支里各写一遍（漏一个就会有一条通道的状态是假的）。
	   写入很快（状态没变时 Mark 直接返回，一轮最多两次落盘），见 session.Handle.Mark。 */
	if h != nil {
		inner := onEvent
		onEvent = func(ev agent.StreamEvent) {
			if ev.Kind == agent.KindAsk {
				h.Mark(session.StateWaitingApproval)
			} else {
				h.Mark(session.StateRunning)
			}
			inner(ev)
		}
	}

	res, err := streamer.Stream(ctx, req, onEvent)
	interrupted := ctl != nil && ctl.WasInterrupted()
	if interrupted {
		/* 打断的应答放在这里、而不是命令到达的那一刻：interrupt 的真实含义是
		   「这一轮真的结束了」。失败链本身就是 context canceled，不必再包一层。 */
		_ = sink.notice("interrupted", map[string]any{"reason": "control"})
	}
	if err != nil {
		/* 被打断 ≠ 失败（2026-09-29）：这里的 err 就是 context canceled，但「用户喊停」
		   与「引擎报错」是两件事 —— 混成 failed 之后左栏只能显示「失败」，
		   用户会以为是自己哪里没接好。 */
		if interrupted {
			finishSession(h, "", session.StateInterrupted)
		} else {
			finishSession(h, "", session.StateFailed)
		}
		/* ⚠️ 失败必须留下**调用方读得到**的说明（2026-09-22 修）。
		   以前这里 json 模式一个字都不写：stdout 空、stderr 只剩前面那些提示行
		   （如 openclaw 的「指定了模型，本轮改走非流式」）—— 调用方（桌面壳）看到的就是
		   「退出码 1 + 一行事件都没有」，界面上画成「空白 + 已完成」，看起来像
		   「引擎没对接好 / 不显示」（用户 2026-09-22 报障）。
		   两处都写，各有分工（与 output.go 的契约、runAsk 的做法对齐）：
		     · stdout 一条 `{"type":"error",...}` 事件 —— 流式消费者不必回头解析 stderr，
		       事件流自己就是完整的（每轮以 result 或 error 收尾，不会两样都没有）；
		     · stderr 一份 WriteError —— json = 带 reason 根因的 envelope，text = 一行；
		       老调用方（只读 stderr / 只读 envelope）照旧能拿到原因。
		   ⚠️ 别把这条只写进 stderr：桌面壳的**换模型重试**判据吃的是事件流与 stderr 两处，
		     但只认事件的消费方（第三方 jq 管道）会因此永远看不到失败。 */
		if interrupted {
			/* 打断这一支**不发 error 事件**（2026-09-29）：事件流上已经发过 `interrupted`，
			   再叠一条 error 会让界面把「用户喊停」画成「运行失败」（消费方的 onError 分支），
			   词表就白分了。stderr 仍留一份 WriteError，排障时看清是哪一个 signal 结束的。
			   ⚠️ 退出码不动（这里照旧返回 reportedError）—— 改退出码是另一件事，
			      会连带所有调用方的判错逻辑，不夹带在这次改动里。 */
			_ = agent.WriteError(stderr, format, engine.Name(), 1, err)
		} else if format == agent.FormatJSON {
			_ = sink.event(streamErrorPayload(engine.Name(), err))
			_ = agent.WriteError(stderr, format, engine.Name(), 1, err)
		} else {
			fmt.Fprintf(stderr, "\nmagic-agent: %v\n", err)
		}
		return &reportedError{err}
	}
	finishSession(h, res.SessionID, session.StateDone)

	if format == agent.FormatJSON {
		// 结尾汇总行（jq 可 tail -1 取全文 + 思考过程 + 工具调用列表）。
		out := struct {
			Type      string           `json:"type"`
			Engine    string           `json:"engine"`
			Model     string           `json:"model"`
			SessionID string           `json:"session_id,omitempty"`
			Attempts  int              `json:"attempts"`
			LatencyMS int64            `json:"latency_ms"`
			Thinking  string           `json:"thinking,omitempty"`
			Text      string           `json:"text"`
			Tools     []agent.ToolCall `json:"tools,omitempty"`
		}{
			Type:      "result",
			Engine:    res.Engine,
			Model:     res.Model,
			SessionID: res.SessionID,
			Attempts:  1,
			LatencyMS: res.Latency.Milliseconds(),
			Thinking:  res.Thinking,
			Text:      res.Text,
			Tools:     res.Tools,
		}
		if opts.noThinking {
			out.Thinking = ""
		}
		if err := sink.event(out); err != nil {
			return err
		}
	} else {
		// text 模式：正文已实时打完，补尾换行即可。
		fmt.Fprintln(stdout)
	}
	return nil
}

// writeStreamEventJSON 输出一行流式事件 NDJSON。
//
// 输出 schema（所有事件共享 {type,text}）：
//
//	{"type":"thinking","text":"..."}                模型思考增量
//	{"type":"text","text":"..."}                    正文增量
//	{"type":"tool_use","text":"<args JSON>","name":"Bash","id":"toolu_xxx"}
//	{"type":"tool_result","text":"<output>","name":"","id":"toolu_xxx"}
//	{"type":"turn_end","text":"<该轮正文>","session_id":"..."}   一轮结束（常驻会话）
//	{"type":"error","error":"<完整错误链>","reason":"<根因>","engine":"..."}  本轮失败（收尾）
//
// 工具事件多带 name / id 字段；turn_end 带 session_id（调用方据此绑定会话锚点）；
// thinking / text 事件的 name / id / session_id 省略（取零值）。
//
// ⚠️ **每轮一定以 result 或 error 收尾**（2026-09-22 起）：失败那条由 writeStreamErrorJSON
// 发出（见 runStreamAsk 的失败分支）。以前失败什么都不发，消费方只能靠退出码猜 ——
// 「一行事件都没有」被读成「引擎没输出」，界面上就是空白。
func writeStreamEventJSON(w io.Writer, ev agent.StreamEvent) error {
	data, err := json.Marshal(streamEventPayload(ev))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

/* writeStreamErrorJSON 输出一行「本轮失败」事件（流式形态的失败收尾）。
 *
 *	schema: {"type":"error","engine":"openclaw","attempts":1,
 *	         "error":"<完整错误链>","reason":"<最内层根因>"}
 *
 * 字段与 `-o json` 的失败 envelope（agent.WriteError）**逐字同源**（都走 agent.ReasonOf），
 * 只是多一个 `type` 便于与事件流混排 —— 同一次失败在事件流与 stderr 里写出的原因必然一致。
 * 为什么要它：流式调用方按行读 stdout，**失败不发声**就等于这一轮凭空消失（见 runStreamAsk）。 */
func writeStreamErrorJSON(w io.Writer, engine string, err error) error {
	data, jerr := json.Marshal(streamErrorPayload(engine, err))
	if jerr != nil {
		return jerr
	}
	_, werr := fmt.Fprintln(w, string(data))
	return werr
}

// streamErrorPayload 「本轮失败」事件的 wire 形状。
// 与 `-o json` 的失败 envelope（agent.WriteError）**逐字同源**（都走 agent.ReasonOf），
// 只是多一个 `type` 便于与事件流混排 —— 同一次失败在事件流与 stderr 里写出的原因必然一致。
func streamErrorPayload(engine string, err error) any {
	return struct {
		Type     string `json:"type"`
		Engine   string `json:"engine"`
		Attempts int    `json:"attempts"`
		Error    string `json:"error"`
		Reason   string `json:"reason"`
	}{Type: "error", Engine: engine, Attempts: 1, Error: err.Error(), Reason: agent.ReasonOf(err)}
}

// prepareAsk 校验参数并组装 Request（流式 / 非流式共用）。
func prepareAsk(cmd *cobra.Command, args []string, opts *askOptions) (agent.Engine, agent.OutputFormat, agent.Request, error) {
	// 1. 校验引擎与输出格式。
	engine := agent.Lookup(opts.engine)
	if engine == nil {
		names := make([]string, 0)
		for _, e := range agent.Engines() {
			names = append(names, e.Name())
		}
		return nil, "", agent.Request{}, &usageError{fmt.Errorf("unknown engine %q (available: %s)", opts.engine, strings.Join(names, ", "))}
	}
	format, err := agent.ParseFormat(opts.output)
	if err != nil {
		return nil, "", agent.Request{}, &usageError{err}
	}
	if opts.retries < 0 {
		return nil, "", agent.Request{}, &usageError{fmt.Errorf("--retries must be >= 0, got %d", opts.retries)}
	}
	if opts.maxTokens < 0 {
		return nil, "", agent.Request{}, &usageError{fmt.Errorf("--max-tokens must be >= 0, got %d", opts.maxTokens)}
	}
	if opts.temperature < -1 {
		return nil, "", agent.Request{}, &usageError{fmt.Errorf("--temperature must be >= 0 or omitted, got %v", opts.temperature)}
	}
	toolsMode, err := parseToolsMode(opts.tools)
	if err != nil {
		return nil, "", agent.Request{}, err
	}
	// 四档权限档位：解析 + 能力校验。
	tier, err := resolvePermissionTier(engine.Name(), opts.permission, flagChanged(cmd, "permission"))
	if err != nil {
		return nil, "", agent.Request{}, err
	}
	// --tools off 下不调用任何工具，档位无处生效 —— 显式传了非默认档就说明白（不静默）。
	if flagChanged(cmd, "permission") && toolsMode.IsOff() && tier != agent.DefaultPermissionTier {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"magic-agent: 提示：当前 --tools off（不调用任何工具），--permission %s 不会生效；如需工具请加 --tools on\n", tier)
	}
	// 常驻会话默认值按引擎区分（见 kaEnabled / agent.AppendDefaultOn），但只在
	// 「流式 + 引擎支持追加」时才有意义。显式 `--keep-alive` 却没满足条件 → 明确报错；
	// 默认值不满足条件 → 静默忽略（不影响原有调用）。
	if flagChanged(cmd, "keep-alive") && opts.keepAlive {
		if !opts.stream {
			return nil, "", agent.Request{}, &usageError{fmt.Errorf(
				"--keep-alive 是常驻会话（首轮结束后等 --append 追加），必须配合 --stream：追加轮次的输出要靠事件流送出")}
		}
		if !agent.AppendSupportOf(engine.Name()) {
			return nil, "", agent.Request{}, &usageError{fmt.Errorf(
				"--keep-alive 暂不支持 %s 引擎（当前支持 claude、codebuddy、codebuddy-ai：靠 stream-json 输入持续收 user 消息；"+
					"dsh：靠 SDK 通道对同一会话继续 prompt）", engine.Name())}
		}
	}
	if opts.idle < 0 {
		return nil, "", agent.Request{}, &usageError{fmt.Errorf("--idle 不能为负（收到 %v；0 = 本轮结束就收工）", opts.idle)}
	}
	// 工作目录（workspace）：展开 ~、转绝对路径、校验存在且是目录。
	workspace, err := resolveWorkspaceDir(opts.workspace)
	if err != nil {
		return nil, "", agent.Request{}, &usageError{err}
	}
	if workspace != "" {
		// 引擎不支持时**明确提示**（参数会被忽略），而不是静默不生效。
		if warn := workspaceUnsupportedWarn(engine.Name()); warn != "" {
			fmt.Fprint(cmd.ErrOrStderr(), warn)
		}
	}
	// 附件（截图/图片等）：与提示词并列的第二份输入，校验后交给引擎按各自原生方式落地。
	attachments, err := resolveAttachments(opts.attach)
	if err != nil {
		return nil, "", agent.Request{}, &usageError{err}
	}
	if len(attachments) > 0 {
		// 没有原生附件通道的引擎会降级成「路径写进提示词」——明确说清楚（不静默）。
		if warn := attachmentPromptFallbackWarn(engine.Name(), len(attachments), toolsMode.IsOff()); warn != "" {
			fmt.Fprint(cmd.ErrOrStderr(), warn)
		}
	}

	// 2. 组装 prompt：-p > --file/stdin 内容 + 位置参数。
	// 每个输入先过 expandPromptSource：值是文件路径（或 @路径）时按文件内容用，
	// 命中提示写 stderr（不静默）。
	var notes []string
	expandArg := func(label, v string) (string, error) {
		text, note, e := expandPromptSource(v)
		if e != nil {
			return "", fmt.Errorf("%s: %w", label, e)
		}
		if note != "" {
			notes = append(notes, note)
		}
		return text, nil
	}
	expandedArgs := make([]string, 0, len(args))
	for _, a := range args {
		text, e := expandArg("参数", a)
		if e != nil {
			return nil, "", agent.Request{}, &usageError{e}
		}
		expandedArgs = append(expandedArgs, text)
	}
	/* ⚠️ --control 时 stdin 归控制通道，**不能**再让 collectPrompt 去读它：
	   它会把控制命令当成提示词读走（真机实测：`-p "..." --control` 下 ping / stop
	   全被吞掉，控制通道形同虚设，而 stdout 上一条异常都没有 —— 极难定位）。
	   传 nil 让 collectPrompt 跳过 stdin 兜底；提示词必须由 -p / 位置参数 / --file 给。 */
	promptStdin := cmd.InOrStdin()
	if opts.control {
		promptStdin = nil
	}
	promptParts, err := collectPrompt(promptStdin, expandedArgs, opts.file)
	if err != nil {
		return nil, "", agent.Request{}, &usageError{err}
	}
	if opts.control && opts.file == "" && len(promptParts) == 0 && strings.TrimSpace(opts.prompt) == "" {
		return nil, "", agent.Request{}, &usageError{fmt.Errorf(
			"--control 占用了 stdin（控制命令走它），所以提示词不能从 stdin 取：请用 -p \"...\" 或位置参数给出")}
	}
	if p := strings.TrimSpace(opts.prompt); p != "" {
		text, e := expandArg("-p/--prompt", p)
		if e != nil {
			return nil, "", agent.Request{}, &usageError{e}
		}
		// --prompt 优先级最高：放到最前（同 --file 语义，附加内容跟后面）。
		if text != "" {
			promptParts = append([]string{text}, promptParts...)
		}
	}
	for _, n := range notes {
		fmt.Fprint(cmd.ErrOrStderr(), n)
	}
	prompt := strings.TrimSpace(strings.Join(promptParts, "\n\n"))
	if prompt == "" {
		return nil, "", agent.Request{}, &usageError{fmt.Errorf("empty prompt: use -p, pass args, --file, or pipe stdin")}
	}

	// 3. 引擎 CLI 预检（快速失败，给出可操作提示）。
	if ok, note := engine.Detect(); !ok {
		return nil, "", agent.Request{}, fmt.Errorf("engine %q unavailable: %s", engine.Name(), note)
	}

	// 4. 组装 Request。
	// system prompt：-s 优先，其次配置文件里的默认值（空 = 不注入）。
	// -s 的值同样支持「文件路径 / @路径」（系统提示词常存在单独的文件里）。
	systemFlag, sysNote, sysFlagErr := expandPromptSource(opts.system)
	if sysFlagErr != nil {
		return nil, "", agent.Request{}, &usageError{fmt.Errorf("-s/--system: %w", sysFlagErr)}
	}
	if sysNote != "" {
		fmt.Fprint(cmd.ErrOrStderr(), sysNote)
	}
	systemPrompt, cfgNote, sysErr := resolveSystemPrompt(systemFlag)
	if cfgNote != "" {
		// 配置里的默认值指向文件 → 同样明说一句（不静默）。
		fmt.Fprint(cmd.ErrOrStderr(), cfgNote)
	}
	if sysErr != nil && opts.verbose {
		// 配置坏掉不阻断调用（默认值只是「锦上添花」），但要让 -v 用户看见原因。
		fmt.Fprintf(cmd.ErrOrStderr(), "magic-agent: 警告：读取默认系统提示词失败（%v），本次不注入\n", sysErr)
	}
	req := agent.Request{
		Engine:       engine.Name(),
		Model:        opts.model,
		SystemPrompt: systemPrompt,
		Tools:        toolsMode,
		Attachments:  attachments,
		MaxTokens:    opts.maxTokens,
		SessionID:    strings.TrimSpace(opts.session),
		Continue:     opts.continueF,
		Workspace:    workspace,
		Permission:   tier,
		PermissionOptions: agent.PermissionOptions{
			ExcludedCommands:    opts.sandboxExclude,
			AllowedDomains:      opts.sandboxDomain,
			AutoModeEnvironment: opts.autoModeEnv,
			Deny:                opts.permDeny,
			Ask:                 opts.permAsk,
		},
		Messages: []agent.Message{
			{Role: "user", Content: prompt},
		},
	}
	if opts.temperature >= 0 {
		t := opts.temperature
		req.Temperature = &t
	}
	if s := strings.TrimSpace(opts.jsonSchema); s != "" {
		schema, err := parseJSONSchemaInline(s)
		if err != nil {
			return nil, "", agent.Request{}, &usageError{fmt.Errorf("--json-schema: %w", err)}
		}
		req.JSONSchema = schema
	}
	return engine, format, req, nil
}

// parseJSONSchemaInline 把 --json-schema 字符串解析成 agent.JSONSchema。
//
// 支持的形态（按宽松顺序）：
//
//  1. 完整 JSON Schema 对象：{"type":"object","properties":{...},"required":[...]}
//  2. 已经预解析的 JSON（任意合法 JSON Schema 顶层对象）
//
// 仅当 type=="object" 时返回；其他顶层 type 一律视为不适用并报错。
func parseJSONSchemaInline(s string) (*agent.JSONSchema, error) {
	var raw struct {
		Type       string                    `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if !strings.EqualFold(raw.Type, "object") {
		return nil, fmt.Errorf("only type=object supported (got %q)", raw.Type)
	}
	if len(raw.Required) == 0 && len(raw.Properties) == 0 {
		return nil, fmt.Errorf("schema must declare at least properties or required")
	}
	return &agent.JSONSchema{
		Type:       "object",
		Properties: raw.Properties,
		Required:   raw.Required,
	}, nil
}

// collectPrompt 收集 prompt 输入。返回 (parts)：
// --file 内容在前（若有），位置参数拼接在后（若有）。
func collectPrompt(stdin io.Reader, args []string, file string) ([]string, error) {
	var parts []string
	if file != "" {
		var (
			data []byte
			err  error
		)
		if file == "-" {
			data, err = io.ReadAll(stdin)
		} else {
			data, err = os.ReadFile(file)
		}
		if err != nil {
			return nil, fmt.Errorf("read prompt file: %w", err)
		}
		if s := strings.TrimSpace(string(data)); s != "" {
			parts = append(parts, s)
		}
	}
	if len(args) > 0 {
		parts = append(parts, strings.Join(args, " "))
	}
	// 无参数无文件但 stdin 有内容 → 读 stdin。
	// *os.File 且是 TTY 时跳过（交互式误用保护）；其余 reader
	//（真实管道 / 测试注入）在无其他输入时直接读取。
	if len(parts) == 0 && file == "" && stdin != nil {
		if f, ok := stdin.(*os.File); ok {
			if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
				return parts, nil // TTY，不读
			}
		}
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		if s := strings.TrimSpace(string(data)); s != "" {
			parts = append(parts, s)
		}
	}
	return parts, nil
}

// printJSON 单行 JSON 输出辅助。
func printJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
