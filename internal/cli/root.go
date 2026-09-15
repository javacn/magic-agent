package cli

// root.go - magic-agent CLI 入口与全局 flags。
//
// 命令结构（无子命令，统一根命令直用）：
//
//	magic-agent [flags] [prompt...]   提问（唯一主流程）
//	magic-agent --engines             列出引擎与可用性（--json 可组合）
//	magic-agent --version             版本信息
//
// 关键 flags：
//
//	-e, --engine <name>      引擎：claude | codebuddy | trae | llm（默认 codebuddy）
//	-m, --model <name>       模型（空 = 引擎默认；llm 引擎读 models.json）
//	-s, --system <prompt>    系统提示词
//	-p, --prompt <text>      提示词
//	-f, --file <path>        从文件读 prompt（- 读 stdin）
//	-t, --timeout <dur>      单次尝试超时（如 3m；默认 600s）
//	-r, --retries <n>        失败重试次数（默认 0）
//	    --backoff <dur>      首次重试退避（默认 2s，指数翻倍，上限 30s）
//	-o, --output <format>    输出：json（默认）| text
//	    --tools <mode>       off（默认）| on | 逗号分隔白名单
//	    --engines            列出引擎与 CLI 探测结果（替代原 engines 子命令）
//	-v, --verbose            重试过程打到 stderr
//
// 退出码：0 成功；1 调用失败；2 参数/输入错误。

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/darren/magic-agent/internal/agent"
)

// Version 版本号。
var Version = "0.1.0"

// NewRootCommand 构建 CLI 根命令（无子命令，全部走根命令 flags）。
//
//	.magic-agent -p "问题"            （-p/--prompt 提示词）
//	magic-agent "问题"                （位置参数同样有效）
//	magic-agent -e codebuddy -m hy3 "问题"
//	magic-agent --engines             （列引擎；--json 可组合）
//
// 注意 -p 在 claude 语境里是 --print，这里统一让位给 --prompt。
func NewRootCommand() *cobra.Command {
	opts := newAskOptions()
	root := &cobra.Command{
		Use:     "magic-agent [flags] [prompt...]",
		Short:   "专业的 agent CLI 代理工具 - 统一 claude / codebuddy / trae 引擎",
		Version: Version,
		Long: `magic-agent - agent CLI 代理工具

把 claude / codebuddy（WorkBuddy）/ trae 三家 CLI 的非交互调用
统一成一条命令：支持切换引擎与模型、超时与重试、固定 text/json
输出格式、稳定退出码。适合脚本化编排与上层工具集成。

引擎：
  claude     Claude Code CLI（-p --output-format json）
  codebuddy  CodeBuddy / WorkBuddy 内置 CLI（默认 hy3，可切 glm-5.3 等）
  trae       Trae CLI（使用 trae 自身配置的默认模型）
  llm        按 ~/.magic-agent/models.json 直接调 LLM（HTTP / ollama / 委托其他引擎）

示例：
  magic-agent -p "用一句话解释什么是熵"            # 直接提问（默认 json 输出）
  magic-agent "问题"                               # 位置参数等价
  magic-agent -e codebuddy "写一首俳句"            # codebuddy 默认 hy3
  magic-agent -e codebuddy -m glm-5.3 "写一首俳句"
  magic-agent -e trae -t 10m "总结这篇文档"        # 默认关工具；--tools on 可开
  magic-agent -e claude --tools Bash,Read "看看这个目录"  # 工具白名单
  cat doc.md | magic-agent -e claude -f - "总结上文"
  magic-agent -e claude -r 2 "1+1=?"               # 失败重试 2 次
  magic-agent --engines                           # 列出引擎与可用性
  magic-agent --engines --json                    # JSON 形式（可被 jq 解析）

llm 引擎（读 ~/.magic-agent/models.json）：
  magic-agent -e llm -m minimax/MiniMax-M3 "问题"   # 显式 provider/model
  magic-agent -e llm -m MiniMax-M3 "问题"           # 裸模型名（按声明顺序匹配）
  magic-agent -e llm "问题"                         # 用配置里的 default
  magic-agent -e llm --engines                      # 看配置来源与 provider 数`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          rejectRemovedSubcommands,
		// 禁用 cobra 内置 completion 子命令：本 CLI 无子命令形态。
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.engines {
				return runEngines(cmd, opts)
			}
			if opts.stream {
				return runStreamAsk(cmd, args, opts)
			}
			return runAsk(cmd, args, opts)
		},
	}
	bindAskFlags(root, opts)
	// 无子命令设计：禁用 cobra 自动注入的 completion 命令，
	// 保证 "magic-agent completion" 也只是被当作 prompt 而非隐藏子命令。
	root.CompletionOptions.DisableDefaultCmd = true
	// --version 输出固定 "magic-agent <ver>"，不带 cobra 默认前缀。
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	return root
}

// Execute 运行 CLI，返回进程退出码。
//
// 错误输出约定（stderr，与 -o 联动）：
//   - 引擎执行失败：runAsk 已用 WriteError 输出（reportedError 标记），此处跳过
//   - 其余错误（参数错 / 引擎预检失败 / 子命令形态）：此处统一输出 ——
//     -o json 时输出同结构 envelope（attempts=0，engine 为 -e 原值），
//     text 时一行 "magic-agent: <err>"。stdout 恒不产生半截内容。
func Execute() int {
	root := NewRootCommand()
	err := root.Execute()
	if err == nil {
		return exitOK
	}
	// 已由 WriteError 格式化输出过（json envelope / text 行）。
	if errors.Is(err, errReportedSentinel) {
		return exitCodeOf(err)
	}

	// 按 -o 的实际生效值（含默认）决定 stderr 格式。
	format := agent.FormatJSON
	if f := root.PersistentFlags().Lookup("output"); f != nil && f.Value.String() != "" {
		if v, perr := agent.ParseFormat(f.Value.String()); perr == nil {
			format = v
		}
	}
	engineName := ""
	if f := root.PersistentFlags().Lookup("engine"); f != nil {
		engineName = f.Value.String()
	}
	// 参数类错误尚未执行任何尝试；attempts=0 便于调用方区分阶段。
	_ = agent.WriteError(os.Stderr, format, engineName, 0, err)
	return exitCodeOf(err)
}

// errReportedSentinel reportedError 的内部标记（errors.Is 用）。
var errReportedSentinel = errors.New("already reported")

// reportedError 包装"已输出过失败信息"的错误：
// WriteError 已把 envelope/文本写到 stderr，Execute 不再打印第二遍。
type reportedError struct{ err error }

func (r *reportedError) Error() string { return r.err.Error() }

// Is 让 errors.Is(err, errReportedSentinel) 命中。
func (r *reportedError) Is(target error) bool { return target == errReportedSentinel }

// Unwrap 保留退出码推断（usageError 判定）沿链下钻。
func (r *reportedError) Unwrap() error { return r.err }

// 退出码约定。
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// usageError 标记参数/输入错误（exit 2）。
type usageError struct{ err error }

func (u *usageError) Error() string { return u.err.Error() }

// removedSubcommands 已删除的子命令名（v0.1 曾有 ask/engines/version
// 子命令形态；现统一为根命令 flags）。出现在首位参数时直接拒绝，
// 避免被误当成 prompt 发给引擎。
var removedSubcommands = map[string]string{
	"ask":        "--prompt / 位置参数",
	"engines":    "--engines",
	"version":    "--version",
	"completion": "(no shell completion)",
}

// rejectRemovedSubcommands 首个位置参数命中已删子命令名时报 usage 错误。
func rejectRemovedSubcommands(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		if hint, ok := removedSubcommands[strings.ToLower(args[0])]; ok {
			return &usageError{fmt.Errorf("unknown command %q: subcommands were removed, use %s instead", args[0], hint)}
		}
	}
	return cobra.ArbitraryArgs(cmd, args)
}

// exitCodeOf 从错误推断退出码。
func exitCodeOf(err error) int {
	if _, ok := err.(*usageError); ok {
		return exitUsage
	}
	return exitFail
}

// runEngines 列出引擎与 CLI 探测结果（原 engines 子命令的 flag 形态）。
// text 表格输出；--json 输出单行 JSON 数组（jq 友好）。
func runEngines(cmd *cobra.Command, opts *askOptions) error {
	engines := agent.Engines()
	if opts.jsonOut {
		type row struct {
			Engine string `json:"engine"`
			OK     bool   `json:"ok"`
			Bin    string `json:"bin"`
			Note   string `json:"note,omitempty"`
		}
		rows := make([]row, 0, len(engines))
		for _, e := range engines {
			ok, note := e.Detect()
			r := row{Engine: e.Name(), OK: ok, Note: note}
			if ok {
				r.Bin = note
				r.Note = ""
			}
			rows = append(rows, r)
		}
		return printJSON(cmd.OutOrStdout(), rows)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "ENGINE     STATUS  CLI")
	for _, e := range engines {
		ok, note := e.Detect()
		status := "✗"
		if ok {
			status = "✓"
		}
		fmt.Fprintf(w, "%-10s %-7s %s\n", e.Name(), status, note)
	}
	return nil
}
