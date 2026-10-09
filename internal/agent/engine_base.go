package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 本文件是七个 CLI 引擎（claude / codebuddy / trae / llm / codex / openclaw / dsh）
// 的公共基座：统一可执行文件探测链、路径展开，以及跨引擎共享的参数构造辅助函数。
// （arkclaw 是 HTTP 网关、不走本机 CLI，其参数映射见 arkclaw.go；dsh 的完整映射
// 与依据见 dsh.go。）
//
// 统一参数矩阵（Request 字段 × 引擎的实际映射）：
//
//	┌──────────────┬──────────────────────────┬───────────────────────────┬────────────────────────────┬──────────────────┬───────────────────────┬─────────────────────┬──────────────────────┐
//	│ Request 字段 │ claude                   │ codebuddy                 │ trae                       │ llm              │ codex                 │ openclaw            │ dsh                  │
//	├──────────────┼──────────────────────────┼───────────────────────────┼────────────────────────────┼──────────────────┼───────────────────────┼─────────────────────┼──────────────────────┤
//	│ Model        │ --model <m>              │ --model <m>               │ -c model.name=<m>          │ -m <m>           │ -m <m>                │ --model <m>         │ 不支持（配置层）⑨    │
//	│ SessionID    │ --resume <id>            │ --resume <id>             │ --resume=<id>              │ --cid <id>       │ exec resume <id>      │ --session-id <id>   │ 不支持（显式报错）⑨  │
//	│ Continue     │ --continue               │ --continue                │ --resume（自动选最近会话） │ -c               │ resume --last         │ sessions 查最新 id  │ 不支持（显式报错）⑨  │
//	│ MaxTokens    │ --settings（env 注入）①  │ --settings（env 注入）①   │ 不支持（静默忽略）②        │ -o max_tokens    │ 不支持（静默忽略）②   │ 静默忽略②           │ 静默忽略②            │
//	│ Temperature  │ 不支持                   │ 不支持                    │ 不支持（静默忽略）②        │ -o temperature   │ 不支持（静默忽略）②   │ 静默忽略②           │ 静默忽略②            │
//	│ Tools        │ --tools / 档位③         │ --tools / 档位③          │ --disallowed-tool / 全放行③│ 不支持           │ -s 沙箱档位④          │ 内嵌 agent（忽略）  │ 内嵌 agent（忽略）⑨  │
//	│ Permission   │ --permission-mode⑧      │ --permission-mode⑧        │ 未接线（yaml 配置）        │ 不支持           │ 未接线（沙箱×审批两轴）│ 未接线（exec.mode） │ 未接线（配置层预设）⑨│
//	│ SystemPrompt │ --append-system-prompt   │ --append-system-prompt    │ 展平进 prompt 头部         │ -s               │ -c developer_instr.⑤  │ 展平进 prompt 头部  │ 展平进 prompt 头部   │
//	│ Timeout      │ 进程级超时               │ 进程级超时                │ + --query-timeout          │ 进程级超时       │ 进程级超时            │ + --timeout <sec>   │ 进程级超时           │
//	│ JSONSchema   │ 不支持                   │ 不支持                    │ 不支持                     │ 输出后处理抽 JSON│ --output-schema 原生  │ 输出后处理抽 JSON   │ 输出后处理抽 JSON    │
//	│ Workspace    │ 子进程 cwd⑥              │ 子进程 cwd⑥               │ 子进程 cwd⑥                │ 不支持⑦          │ -C/--cd + cwd⑥        │ 不支持⑦             │ 子进程 cwd⑥          │
//	└──────────────┴──────────────────────────┴───────────────────────────┴────────────────────────────┴──────────────────┴───────────────────────┴─────────────────────┴──────────────────────┘
//
// 注：
// ① claude / codebuddy 均支持 `--settings <file-or-json>`，通过 {"env":{"CLAUDE_CODE_MAX_OUTPUT_TOKENS":"<n>"}}
//
//	注入输出上限（已用真实 CLI --help 验证）；两者无 temperature 原生参数，也无对应 settings 键。
//
// ② trae 的 `-c <k>=<v>` 会整体覆盖模型配置块（实测设置 model.max_tokens 后模型名漂移并触发配额错误），
//
//	因此 MaxTokens / Temperature 在 trae 上明确不支持；codex 实测 `-c model_max_output_tokens=` /
//	`-c temperature=` 会被 CLI 静默忽略（无对应生效键）；openclaw agent 无相关 flag ——
//	三者构造参数时统一静默忽略。
//
// ③ Tools 语义：关闭 = claude/codebuddy `--tools ""`；trae `--disallowed-tool`
//
//	逐个列出内置实体工具（Bash/Edit/Write/Glob/Grep/Read）。
//	开启 = claude/codebuddy `--permission-mode <档位>`（四档模型，见 ⑧ 与 permission.go；
//	改造前恒传 `--dangerously-skip-permissions` / `-y` = 第 4 档）；trae `-y`。
//	白名单 = claude/codebuddy `--tools a,b` + `--permission-mode <档位>`；trae **无收窄能力**
//	（`--allowed-tool` 只做自动批准、不裁剪工具集，实测仍下发全部 18 个工具），
//	故白名单降级为「默认开启所有工具」= 全放行 `-y`，与 codex 的白名单降级同构。
//	llm 无工具概念。
//
// ④ codex 无逐工具白名单/禁用能力：off → `-s read-only`（只读沙箱，尽力压制写入与执行）；
//
//	on / 白名单 → `--dangerously-bypass-approvals-and-sandbox`（白名单降级为全放行）。
//
// ⑤ codex 的 system prompt 经 `-c developer_instructions=<TOML 字符串>` 原生注入；
//
//	任意文本用 json.Marshal 转义（其转义集与 TOML basic string 兼容）。
//
// ⑥ Workspace（工作目录）落地方式 —— 能用 CLI 原生能力的就用原生：
//
//	codex 原生 `-C/--cd <dir>`（"working root"，同时决定 sandbox 可写根）+ 子进程 cwd；
//	claude / codebuddy / trae **没有**工作目录 flag（`--add-dir` 是「追加额外可访问目录」），
//	其原生方式就是**在目标目录里启动进程**（cwd）——相对路径解析、CLAUDE.md/AGENTS.md
//	发现、git 上下文都跟着 cwd，故一律用子进程 cwd 实现（2026-09-17 真机验证：
//	codex/claude/codebuddy/trae 跑 `pwd` 都落在指定目录）。
//	dsh 也走这条路：官方语义就是「调用时所在目录即默认 workspace 根」，
//	子进程 cwd 正是它的原生方式。
//
// ⑦ **不支持**（-w 会被忽略，CLI 层打一行提示）：
//
//	llm       chat CLI，模型不直接读写文件，无文件系统语义；
//	arkclaw   远端网关，workspace 由 claw_id 绑定，客户端无法指定；
//	openclaw  workspace 与 agent 绑定（`openclaw agents add` 时分配），无 per-call flag ——
//	          **实测子进程 cwd 被忽略**（agent 报的仍是自己的 ~/.openclaw/workspace），
//	          要换目录得配一个绑定该目录的 agent。
//	机器可读的能力表见 WorkspaceSupportOf（--engines 每行的 workspace 字段）。
//
// ⑧ Permission（四档权限模型，2026-09-18 新增，详见 permission.go）：
//
//	档位 manual / accept-edits / auto / full 映射到 claude、codebuddy 的
//	`--permission-mode default / acceptEdits / auto / bypassPermissions`。
//	沙箱没有 CLI flag（claude 的 flags 表里没有 --sandbox），只能经 `--settings`
//	的 sandbox.* 注入；而 `--settings` 不是可重复 flag，故与 MaxTokens 的 env 注入
//	合并进同一份 JSON（agentSettingsPayload）。第 3 档还会注入 settings.autoMode。
//	默认档 full = 保持改造前的行为（恒传 --dangerously-skip-permissions / -y）。
//	未接线的引擎（trae/llm/codex/openclaw/dsh/arkclaw）传 --permission 会在 CLI 层
//	exit 2 明确报错 —— 安全设置不做静默忽略。
//	机器可读的能力表见 PermissionSupportOf（--engines 每行的 permission 字段）。
//
// ⑨ dsh（DeepSeek Harness，headless profile）—— 依据其官方 CLI 行为参考，完整映射与理由见 dsh.go：
// Model：headless **没有 --model 参数**，模型由配置层决定（$DSH_HOME/settings.yaml 的 agent-default-model 段 / Settings → Models），故 -m 不透传、构造参数时忽略并打一行告警（同 arkclaw）。
// SessionID / Continue：headless 一次调用 = 一个**全新的持久化 Agent**，官方 app 参数表里只有任务文本、没有续接参数 → **显式报错**（静默开新会话会让调用方以为上下文接上了）。
// MaxTokens / Temperature：无对应 flag（超时是 provider 级 settings 的 streamIdleTimeoutMs，不是 CLI flag）→ 静默忽略②。
// Tools：dsh 自带 agent 工具循环（base bundle 的 read/write/edit + Bash），增删工具只能改 profile bundle / cordis.patch.yml，**无命令行级逐工具开关** → 整体忽略（与 openclaw 的「内嵌 agent」同构）。
// Permission：新会话默认 workspace-write 预设（写入限工作区 + 平台临时目录），另有 read-only 预设，进程回退由环境变量 DSH_PERMISSION_MODE 决定；两个预设无法表达四档模型里的「沙箱关闭 / 无审批」（第 4 档）→ 不接线（none），显式传 --permission 在 CLI 层 exit 2。
// SystemPrompt：无独立 system 注入 flag → 展平进 prompt 头部（同 trae/openclaw）。
// Workspace：官方语义就是「调用时所在目录即默认 workspace 根」→ 子进程 cwd⑥。
// JSONSchema：无原生约束 → 输出后处理抽 JSON（同 llm/openclaw/arkclaw）。
// Attachments：无原生输入通道 → 绝对路径写进提示词（同 trae/openclaw）。
// Stream：实现了，但**只流推理** —— 推理增量走 stderr（"dsh: reasoning:" 起头）→ 逐条 thinking 事件；正文在 turn 结束时一次性给出（一条 text 事件）。正文级流式需 --profile sdk 的 JSON-RPC，不在本引擎范围。
//
// cliBase 描述一个 CLI 引擎的可执行文件探测配置。
// 各引擎保留顶层 BinPath 字段与一行式 Name()，以保证测试中零值/复合字面量构造的兼容性；
// 本结构体仅承载探测链配置，由各引擎的 bin()/Detect() 委托调用。
type cliBase struct {
	binName    string   // CLI 名称（LookPath 兜底用）
	envVar     string   // 覆盖二进制路径的环境变量
	candidates []string // 常见安装路径（支持 ~ 前缀，按序 os.Stat 探测）
	notFound   string   // 全部探测失败时的提示文案
}

var (
	claudeBase = cliBase{
		binName: "claude",
		envVar:  "MAGIC_AGENT_CLAUDE_BIN",
		candidates: []string{
			"/opt/homebrew/bin/claude",
			"/usr/local/bin/claude",
		},
		notFound: "claude CLI not found (install Claude Code or set MAGIC_AGENT_CLAUDE_BIN)",
	}
	// codebuddyBase / codebuddyAIBase 都指向**独立安装**的 CodeBuddy Code CLI
	// （`npm i -g @tencent-ai/codebuddy-code`），不再用桌面 App 内置的那一份。
	//
	// 为什么换掉 App 内置 CLI（2026-09-28 实测根因）：
	//   桌面 App（WorkBuddy / WorkBuddy AI）的凭据由 App 通过 sidecar 通道用
	//   **受管密钥**做静态加密（CODEBUDDY_SIDECAR_CREDENTIAL_BOOTSTRAP_SOCKET），
	//   密钥只发给 App 自己的子进程；独立起的 CLI 解不开，
	//   at-rest 登记表里 `auth/<id>.info` 的 read/write 全是 `missing-key`
	//   → 引擎每次调用只拿到 "Authentication required" 的空输出。
	//   而独立安装的 CLI 自带 TUI（可 `/login`）、自带可读写的凭据库，
	//   并且能用 ACC_PRODUCT_CONFIG_V3 切 authentication.id —— 一个 CLI 并存多个账号。
	//
	// 探测链（见 cliBase.resolve）：MAGIC_AGENT_CODEBUDDY_BIN →
	// PATH 上的 `codebuddy` → npm 全局 bin 目录。candidates 留空 =
	// 不再探测 App 包内路径（那是导致 missing-key 的那一份）。
	codebuddyBase = cliBase{
		binName:  "codebuddy",
		envVar:   "MAGIC_AGENT_CODEBUDDY_BIN",
		notFound: "codebuddy CLI not found (npm install -g @tencent-ai/codebuddy-code, or set MAGIC_AGENT_CODEBUDDY_BIN)",
	}
	// codebuddyAIBase 与 codebuddyBase 用**同一个二进制**，差异只在账号：
	// 两个引擎各自注入不同的 ACC_PRODUCT_CONFIG_V3（authentication.id），
	// CLI 据此读不同的票据文件 sharedDataPath/auth/<id>.info
	//（2026-09-28 实测验证）。账号标识见 codebuddy.go 的 codebuddyAuthID / codebuddyAIAuthID。
	codebuddyAIBase = cliBase{
		binName:  "codebuddy",
		envVar:   "MAGIC_AGENT_CODEBUDDY_AI_BIN",
		notFound: "codebuddy CLI not found for codebuddy-ai (npm install -g @tencent-ai/codebuddy-code, or set MAGIC_AGENT_CODEBUDDY_AI_BIN)",
	}
	traeBase = cliBase{
		binName: "trae-cli",
		envVar:  "MAGIC_AGENT_TRAE_BIN",
		candidates: []string{
			"~/.local/bin/trae-cli",
			"~/bin/trae-cli",
		},
		notFound: "trae-cli not found (install trae-cli or set MAGIC_AGENT_TRAE_BIN)",
	}
	llmBase = cliBase{
		binName: "llm",
		envVar:  "MAGIC_AGENT_LLM_BIN",
		candidates: []string{
			// venv 优先：绕开 Homebrew Python 3.14 的 pip truststore 兼容问题。
			// ⚠️ **两个平台的 venv 布局不同，两条都要列**（2026-10-01 实测踩过）：
			// Windows 的 venv 可执行文件在 `Scripts\`，Unix 在 `bin/`。只列 Unix 那条时，
			// `npm i -g magic-agent` 的 postinstall 明明把 llm 装好了（install.js 自己
			// 按 win32 选了 Scripts），`--engines` 却仍报 "llm CLI not found" ——
			// 安装器与探测器对「llm 装在哪」各说各话，且两边都不报错，很难查。
			"~/.llm-venv/Scripts/llm.exe",
			"~/.llm-venv/bin/llm",
			"/opt/homebrew/bin/llm",
			"/usr/local/bin/llm",
			"~/.local/bin/llm",
		},
		notFound: "llm CLI not found (pip install llm / brew, or set MAGIC_AGENT_LLM_BIN)",
	}
	codexBase = cliBase{
		binName: "codex",
		envVar:  "MAGIC_AGENT_CODEX_BIN",
		candidates: []string{
			"/opt/homebrew/bin/codex",
			"/usr/local/bin/codex",
		},
		notFound: "codex CLI not found (npm install -g @openai/codex or set MAGIC_AGENT_CODEX_BIN)",
	}
	openclawBase = cliBase{
		binName: "openclaw",
		envVar:  "MAGIC_AGENT_OPENCLAW_BIN",
		candidates: []string{
			"/opt/homebrew/bin/openclaw",
			"/usr/local/bin/openclaw",
			"~/.local/bin/openclaw",
		},
		notFound: "openclaw CLI not found (install openclaw or set MAGIC_AGENT_OPENCLAW_BIN)",
	}
	// dsh 是 Node 启动器（npm 包 @deepseek-ai/dsh），常见落点是 npm 全局 bin 目录；
	// PATH 查找（exec.LookPath）才是主路径，下面的候选只是「PATH 不干净」时的兜底。
	dshBase = cliBase{
		binName: "dsh",
		envVar:  "MAGIC_AGENT_DSH_BIN",
		candidates: []string{
			"~/.local/bin/dsh",
			"~/.npm-global/bin/dsh",
			"/opt/homebrew/bin/dsh",
			"/usr/local/bin/dsh",
			"~/.bun/bin/dsh",
		},
		notFound: "dsh CLI not found (npm i -g @deepseek-ai/dsh, or set MAGIC_AGENT_DSH_BIN)",
	}
)

// resolve 解析引擎二进制路径：显式 BinPath → 环境变量 → 候选路径（os.Stat）→
// PATH 查找 → npm 全局前缀（见 npmGlobalBinDirs）。
// 显式路径与环境变量不做存在性校验（与原实现一致，llm 的 Detect 会自行 stat 校验显式路径）。
//
// npm 全局前缀放在**最后**是刻意的：它只做「补漏」，不改变既有环境的解析结果
// （有 PATH 命中的仍按 PATH 走），只让「PATH 被裁剪、但 CLI 装在 npm 全局前缀里」
// 的环境也能探测到 —— 例如沙箱把 `npm i -g` 的落点设成私有目录时。
func (b cliBase) resolve(binPath string) string {
	if binPath != "" {
		return binPath
	}
	if v := os.Getenv(b.envVar); v != "" {
		return v
	}
	for _, cand := range b.candidates {
		p := expandHome(cand)
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if path, err := exec.LookPath(b.binName); err == nil {
		return path
	}
	for _, dir := range npmGlobalBinDirs() {
		p := filepath.Join(dir, b.binName)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// npmGlobalBinDirs 返回 npm 全局 bin 目录，用于发现 `npm i -g` 装出来的 CLI。
//
// 两段，按优先级：
//
//  1. 环境变量 `NPM_CONFIG_PREFIX`（或小写 `npm_config_prefix`）给出的前缀
//     —— 依据：npm 把可执行文件放在 `<prefix>/bin`。本机实测沙箱就把它指向一个
//     **不在 PATH 里**的私有前缀，导致 `npm i -g @deepseek-ai/dsh` 明明成功、探测却报 not found。
//  2. 标准全局前缀兜底 `/opt/homebrew/bin`、`/usr/local/bin`。
//     为什么需要兜底（2026-09-28）：GUI 启动的进程（magic-test 等）PATH 往往很窄，
//     既没有 `npm i -g` 的落点、也可能没有 Homebrew 的 bin；而 codebuddy 系现在
//     依赖**独立安装**的 CodeBuddy Code CLI（`npm i -g @tencent-ai/codebuddy-code`
//     → 默认落在 `/opt/homebrew/bin`），探测不能把这件事寄托在调用方的 PATH 上。
//
// 只读环境变量、不起子进程：探测链会被 `--engines` 对每个引擎调用一次，跑 `npm prefix -g`
// 会让列表慢上秒级；而「npm 在 PATH 上」的场景本来就能被 LookPath 命中，无需兜底。
func npmGlobalBinDirs() []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	for _, k := range []string{"NPM_CONFIG_PREFIX", "npm_config_prefix"} {
		if p := strings.TrimSpace(os.Getenv(k)); p != "" {
			add(filepath.Join(p, "bin"))
		}
	}
	add("/opt/homebrew/bin") // Apple Silicon Homebrew
	add("/usr/local/bin")    // Intel Homebrew / Node 官方安装包
	return dirs
}

// detect 返回（是否可用，提示信息）。可用时提示信息为空字符串。
func (b cliBase) detect(binPath string) (bool, string) {
	if b.resolve(binPath) == "" {
		return false, b.notFound
	}
	return true, ""
}

// expandHome 将路径开头的 ~ 展开为用户主目录。
func expandHome(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	if len(p) > 1 && p[1] != '/' && p[1] != filepath.Separator {
		// 仅支持 ~ 或 ~/ 开头，不支持 ~user 形式
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if len(p) == 1 {
		return home
	}
	return filepath.Join(home, p[2:])
}

// ExpandHome 导出给 CLI 层做参数路径展开（-w/--workspace 等）。
func ExpandHome(p string) string { return expandHome(p) }

// WorkspaceSupportOf 返回某引擎「指定工作目录（workspace）」的落地方式（机器可读）：
//
//	"flag:-C"  CLI 有原生工作目录 flag（codex 的 -C/--cd）
//	"cwd"      没有 flag，但**子进程 cwd 就是它的原生方式**（相对路径 / CLAUDE.md 发现 /
//	           git 上下文都跟着 cwd）—— 工作目录真的会生效
//	           （claude / codebuddy / trae / dsh：dsh 官方语义即「调用目录 = workspace 根」）
//	"none"     改不动：llm / arkclaw 没有文件系统语义；openclaw 的 workspace 与 agent
//	           绑定（`openclaw agents`），**实测子进程 cwd 被忽略**（它的 agent 在自己的
//	           workspace 里跑）→ 参数被忽略，CLI 层会打一行提示
//
// 该值同时出现在 `--engines` 每行的 workspace 字段里，供调用方（如观物台）决定是否下发。
func WorkspaceSupportOf(engine string) string {
	// 具名 agent（如 MagicAI）先归到它协议的家族名，再查表 —— 见 CapabilityFamilyOf。
	engine = CapabilityFamilyOf(engine)
	switch engine {
	case "codex":
		return "flag:-C"
	case "llm", "arkclaw", "openclaw", "codebuddy-gateway":
		// codebuddy-gateway 也在这里：网关在**它自己的 cwd** 里跑 agent，
		// webhook 协议没有工作目录字段 → per-call 无法指定（要换目录得改网关进程的启动目录）。
		return "none"
	default:
		return "cwd" // claude / codebuddy / trae / dsh（以及测试用假引擎）
	}
}

// InstallCommandOf 返回某引擎的「一键安装命令」（shell 一行，可直接执行）。
//
// 为什么需要它：`--engines` 报某个引擎 `ok:false` 时，调用方（观物台等）需要一条
// **可执行的自救路径**，而不是只有一句 "not found"。命令一律取自各 CLI 的官方安装方式
// （括号里是官方另提供的等价方式，不塞进字段以免命令过长）：
//
//	claude    npm install -g @anthropic-ai/claude-code
//	          （官方另有 curl -fsSL https://claude.ai/install.sh | bash、brew install --cask claude-code）
//	codebuddy npm install -g @tencent-ai/codebuddy-code
//	codebuddy-ai  同上 —— 两个引擎跑的是**同一个独立 CLI**，只是账号不同
//	          （靠 ACC_PRODUCT_CONFIG_V3 切 authentication.id，见 codebuddy.go）。
//	          2026-09-28 起不再使用桌面 App（WorkBuddy.app / WorkBuddy AI.app）内置的那份：
//	          那份的凭据由 App 用受管密钥静态加密，独立进程解不开（missing-key），
//	          只会拿到空输出 —— 见 codebuddyBase 注释。
//	codex     npm install -g @openai/codex
//	openclaw  npm install -g openclaw@latest（官方文档；装完通常还要 openclaw onboard --install-daemon）
//	dsh       npm i -g @deepseek-ai/dsh
//	trae      sh -c "$(curl -L https://trae.cn/trae-cli/install.sh)"
//	          （官方 TraeCode CLI 安装脚本，落在 ~/.local/bin —— 正是本包探测链的候选位，
//	           故不必在同一行里 export PATH）
//	llm       python3 -m venv ~/.llm-venv && ~/.llm-venv/bin/pip install llm
//	          （与 npm postinstall 同一条隔离安装路径，避开系统 Python；另有 pip / pipx / brew）
//
// 返回空串 = **没有可执行的一键安装命令**（而不是「不知道」）：
//
//	arkclaw                   没有二进制也没有 CLI —— 凭据与端点在配置文件里
//	codebuddy-gateway         同上：它要的不是安装，而是**把网关跑起来**
//	                          （`codebuddy --serve` 或交互会话里的 `/gateway`），
//	                          那是一句启动命令、不是安装命令，写进 install 字段会误导
//
// 这些引擎「怎么才能用」的原因仍写在 --engines 的 note 字段（人读），install 字段如实留空，
// 不编一条跑不通的命令。
func InstallCommandOf(engine string) string {
	// 具名 agent 先归到它协议的家族名，再查表 —— 见 CapabilityFamilyOf。
	engine = CapabilityFamilyOf(engine)
	switch engine {
	case "claude":
		return "npm install -g @anthropic-ai/claude-code"
	case "codebuddy", "codebuddy-ai":
		return "npm install -g @tencent-ai/codebuddy-code"
	case "codex":
		return "npm install -g @openai/codex"
	case "openclaw":
		return "npm install -g openclaw@latest"
	case "dsh":
		return "npm i -g @deepseek-ai/dsh"
	case "trae":
		return `sh -c "$(curl -L https://trae.cn/trae-cli/install.sh)"`
	case "llm":
		return "python3 -m venv ~/.llm-venv && ~/.llm-venv/bin/pip install llm"
	}
	return ""
}

/* EngineVersionOf 尽力读出某引擎 CLI 的**当前版本**（读不到就空串 —— 绝不编）。
 *
 * 为什么要它（2026-09-23 用户：「引擎检测除了 a2a 的 其他也要支持有升级」）：
 *   有了版本号，「升级」这件事才**可验证** —— 点完升级重探一次，调用方才能如实说
 *   「已升级：旧 → 新」或「版本未变（多半已是最新）」。没有它就只能说「命令跑完了」，
 *   而「跑完了」既可能真升级了、也可能什么都没发生 —— 用户侧看起来一模一样
 *   （2026-09-23 在 magic-agent 那一区就踩过这个：用户报「升级完也没变化」）。
 *
 * 判据（都取自 CLI 自己的输出，本工程**不维护任何版本表** —— 抄一份必然漂移）：
 *   1. 跑 `<bin> --version`（5s 超时），取输出里**第一个 x.y.z 形态**的串；
 *   2. 拿不到（老 CLI 不认 --version / 输出里没有版本号 / 进程起不来）→ 空串。
 * 实测样例（2026-09-23 本机）：claude → `2.1.146 (Claude Code)`；codex → `codex-cli 0.154.0`；
 *   openclaw → `OpenClaw 2026.6.11 (e085fa1)`；dsh → `0.1.5-rc.2`；trae → `0.120.52`。
 * 读不到的三类，都**如实留空**（不编）：
 *   · arkclaw / 具名 A2A agent —— HTTP 网关，本机没有二进制（bin 是 URL，这里直接跳过）；
 *   · codebuddy / codebuddy-ai —— 桌面端 GUI 应用，其 CLI 不提供 --version 语义
 *     （⚠️ 实测 codebuddy-ai 偶尔能吐一个版本号，能读就读、读不到就空，不做特判）；
 *   · llm —— 它的 `--version` 要跑自己 venv 里的 python；venv 的 python 不可用
 *     （如环境里带了 PYTHONHOME / 解释器被换过）时读不到 → 空串。 */
func EngineVersionOf(ctx context.Context, bin string) string {
	b := strings.TrimSpace(bin)
	if b == "" || strings.Contains(b, "://") {
		return ""
	}
	vctx, cancel := context.WithTimeout(ctx, engineVersionTimeout)
	defer cancel()
	out, errOut, _ := runCLI(vctx, b, "--version")
	if v := firstSemver(out); v != "" {
		return v
	}
	/* 少数 CLI 把版本打到 stderr（stdout 只剩日志）→ 兜一次。
	   「只看第一行」这条同样适用，所以不会从报错堆栈里挑出无关数字。 */
	return firstSemver(errOut)
}

// engineVersionTimeout 单次 `--version` 的超时。比模型探测（30s）短得多：
// 它只打印一行，正常在 100ms 内返回；给 5s 是留给冷启动 / 慢盘。
const engineVersionTimeout = 5 * time.Second

// semverRe 匹配 x.y.z 形态（数字段），允许 `-rc.1` / `+build` 这类后缀。
// ⚠️ 只认**三段数字**：两段的（如 `1.0`）在 CLI 输出里常是别的东西（协议版本、窗口尺寸）。
var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.\-]+)?`)

// firstSemver 取文本里第一个版本号形态的串；没有则空串。
//
// ⚠️ **只看第一行**：版本输出都在第一行，后面的行可能是路径或别的程序的版本 ——
// 实测 llm 的 venv python 坏掉时，输出里满是 `/…/python3.12.13/…`、`/…/3.10.20_3/…`
// 这类路径，按「全文第一个 x.y.z」会挑中 `3.12.13` 这种**根本不是引擎版本**的数字。
// 只认三段数字的理由同 semverRe 的注释（两段的常是协议版本 / 窗口尺寸）。
func firstSemver(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return semverRe.FindString(s)
}

// ToolsSwitchableOf 报告某引擎的 `--tools` **是否真的有落地通道**（见矩阵 ③④⑨）：
//
//	true   claude / codebuddy / codebuddy-ai（`--tools ""` / `--tools a,b`）
//	       trae（`--disallowed-tool` 真减法 / `-y` 全放行）
//	       codex（`-s read-only` ↔ 旁路，无逐工具白名单但 off/on 确实有区别）
//	false  llm（无工具概念）；openclaw / dsh / arkclaw（自带工具循环或网关侧决定，
//	       命令行层没有逐工具开关 → 参数被整体忽略）
//	       codebuddy-gateway（工具集由**网关进程**的 `--agent` 模式决定，
//	       webhook 协议里没有工具字段 → per-call 传了也不改变行为）
//
// 用途：CLI 层据此决定「附件降级提示」里要不要提 `--tools on` —— 对没有落地通道的
// 引擎说「请改用 --tools on」是误导（传了也不改变行为）。
func ToolsSwitchableOf(engine string) bool {
	// 具名 agent 先归到它协议的家族名，再查表 —— 见 CapabilityFamilyOf。
	engine = CapabilityFamilyOf(engine)
	switch engine {
	case "claude", "codebuddy", "codebuddy-ai", "trae", "codex":
		return true
	default:
		return false
	}
}

// maxTokensEnv 返回 claude / codebuddy `--settings` 里 env 段的取值，
// 通过 CLAUDE_CODE_MAX_OUTPUT_TOKENS 注入输出上限。n<=0 时返回 nil 表示不注入。
func maxTokensEnv(n int) map[string]string {
	if n <= 0 {
		return nil
	}
	return map[string]string{"CLAUDE_CODE_MAX_OUTPUT_TOKENS": strconv.Itoa(n)}
}

// maxTokensSettings 构造只含 env 的 `--settings` JSON 载荷（历史入口，保留兼容）。
//
// 注意：claude / codebuddy 实际走 agentSettingsPayload —— 因为 `--settings` 只接受
// 一份载荷，MaxTokens 的 env、沙箱配置、autoMode、权限规则必须合并进同一个 JSON
// （见 permission.go）。本函数保留给「只要 env」的调用方与既有测试。
func maxTokensSettings(n int) (string, bool) {
	env := maxTokensEnv(n)
	if env == nil {
		return "", false
	}
	payload, err := json.Marshal(map[string]any{"env": env})
	if err != nil {
		return "", false
	}
	return string(payload), true
}
