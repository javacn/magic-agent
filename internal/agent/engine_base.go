package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// 本文件是六个 CLI 引擎（claude / codebuddy / trae / llm / codex / openclaw）的公共基座：
// 统一可执行文件探测链、路径展开，以及跨引擎共享的参数构造辅助函数。
//
// 统一参数矩阵（Request 字段 × 引擎的实际映射）：
//
//	┌──────────────┬──────────────────────────┬───────────────────────────┬────────────────────────────┬──────────────────┬───────────────────────┬─────────────────────┐
//	│ Request 字段 │ claude                   │ codebuddy                 │ trae                       │ llm              │ codex                 │ openclaw            │
//	├──────────────┼──────────────────────────┼───────────────────────────┼────────────────────────────┼──────────────────┼───────────────────────┼─────────────────────┤
//	│ Model        │ --model <m>              │ --model <m>               │ -c model.name=<m>          │ -m <m>           │ -m <m>                │ --model <m>         │
//	│ SessionID    │ --resume <id>            │ --resume <id>             │ --resume=<id>              │ --cid <id>       │ exec resume <id>      │ --session-id <id>   │
//	│ Continue     │ --continue               │ --continue                │ --resume（自动选最近会话） │ -c               │ resume --last         │ sessions 查最新 id  │
//	│ MaxTokens    │ --settings（env 注入）①  │ --settings（env 注入）①   │ 不支持（静默忽略）②        │ -o max_tokens    │ 不支持（静默忽略）②   │ 静默忽略②           │
//	│ Temperature  │ 不支持                   │ 不支持                    │ 不支持（静默忽略）②        │ -o temperature   │ 不支持（静默忽略）②   │ 静默忽略②           │
//	│ Tools        │ --tools / 全放行③        │ --tools / 全放行③         │ --disallowed-tool / 全放行③│ 不支持           │ -s 沙箱档位④          │ 内嵌 agent（忽略）  │
//	│ SystemPrompt │ --append-system-prompt   │ --append-system-prompt    │ 展平进 prompt 头部         │ -s               │ -c developer_instr.⑤  │ 展平进 prompt 头部  │
//	│ Timeout      │ 进程级超时               │ 进程级超时                │ + --query-timeout          │ 进程级超时       │ 进程级超时            │ + --timeout <sec>   │
//	│ JSONSchema   │ 不支持                   │ 不支持                    │ 不支持                     │ 输出后处理抽 JSON│ --output-schema 原生  │ 输出后处理抽 JSON   │
//	└──────────────┴──────────────────────────┴───────────────────────────┴────────────────────────────┴──────────────────┴───────────────────────┴─────────────────────┘
//
// 注：
// ① claude / codebuddy 均支持 `--settings <file-or-json>`，通过 {"env":{"CLAUDE_CODE_MAX_OUTPUT_TOKENS":"<n>"}}
//    注入输出上限（已用真实 CLI --help 验证）；两者无 temperature 原生参数，也无对应 settings 键。
// ② trae 的 `-c <k>=<v>` 会整体覆盖模型配置块（实测设置 model.max_tokens 后模型名漂移并触发配额错误），
//    因此 MaxTokens / Temperature 在 trae 上明确不支持；codex 实测 `-c model_max_output_tokens=` /
//    `-c temperature=` 会被 CLI 静默忽略（无对应生效键）；openclaw agent 无相关 flag ——
//    三者构造参数时统一静默忽略。
// ③ Tools 语义：关闭 = claude/codebuddy `--tools ""`；trae `--disallowed-tool`
//    逐个列出内置实体工具（Bash/Edit/Write/Glob/Grep/Read）。
//    开启 = claude `--dangerously-skip-permissions`；codebuddy/trae `-y`。
//    白名单 = claude/codebuddy `--tools a,b`；trae **无收窄能力**（`--allowed-tool`
//    只做自动批准、不裁剪工具集，实测仍下发全部 18 个工具），故白名单降级为
//    「默认开启所有工具」= 全放行 `-y`，与 codex 的白名单降级同构。
//    llm 无工具概念。
// ④ codex 无逐工具白名单/禁用能力：off → `-s read-only`（只读沙箱，尽力压制写入与执行）；
//    on / 白名单 → `--dangerously-bypass-approvals-and-sandbox`（白名单降级为全放行）。
// ⑤ codex 的 system prompt 经 `-c developer_instructions=<TOML 字符串>` 原生注入；
//    任意文本用 json.Marshal 转义（其转义集与 TOML basic string 兼容）。

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
	codebuddyBase = cliBase{
		binName: "codebuddy",
		envVar:  "MAGIC_AGENT_CODEBUDDY_BIN",
		candidates: []string{
			"/Applications/WorkBuddy.app/Contents/Resources/app.asar.unpacked/cli/bin/codebuddy",
			"/Applications/WorkBuddy.app/Contents/Resources/app.asar.unpacked/cli/bin/cbc",
		},
		notFound: "codebuddy CLI not found (install WorkBuddy.app or set MAGIC_AGENT_CODEBUDDY_BIN)",
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
			// venv 优先：绕开 Homebrew Python 3.14 的 pip truststore 兼容问题
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
)

// resolve 解析引擎二进制路径：显式 BinPath → 环境变量 → 候选路径（os.Stat）→ PATH 查找。
// 显式路径与环境变量不做存在性校验（与原实现一致，llm 的 Detect 会自行 stat 校验显式路径）。
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
	return ""
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

// maxTokensSettings 构造 claude / codebuddy `--settings` 所需的 JSON 载荷，
// 通过 env 注入 CLAUDE_CODE_MAX_OUTPUT_TOKENS。n<=0 时返回 ("", false) 表示不注入。
func maxTokensSettings(n int) (string, bool) {
	if n <= 0 {
		return "", false
	}
	payload, err := json.Marshal(map[string]any{
		"env": map[string]string{"CLAUDE_CODE_MAX_OUTPUT_TOKENS": strconv.Itoa(n)},
	})
	if err != nil {
		return "", false
	}
	return string(payload), true
}
