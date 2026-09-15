package cli

// root.go - magic-agent CLI 入口与全局 flags。
//
// 命令结构：
//
//	magic-agent                  显示帮助
//	magic-agent ask <prompt>     单次提问（核心命令）
//	magic-agent engines          列出引擎与可用性
//	magic-agent version          版本信息（等价于 --version）
//
// ask 的关键 flags：
//
//	-e, --engine <name>      引擎：claude | codebuddy | trae（默认 claude）
//	-m, --model <name>       模型（空 = 引擎默认）
//	-s, --system <prompt>    系统提示词
//	-f, --file <path>        从文件读 prompt（- 读 stdin）
//	-t, --timeout <dur>      单次尝试超时（如 3m；默认按引擎）
//	-r, --retries <n>        失败重试次数（默认 0）
//	    --backoff <dur>      首次重试退避（默认 2s，指数翻倍，上限 30s）
//	-o, --output <format>    输出：text（默认）| json
//	-v, --verbose            重试过程打到 stderr
//
// 退出码：0 成功；1 调用失败；2 参数/输入错误。

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/darren/magic-agent/internal/agent"
)

// Version 版本号。
var Version = "0.1.0"

// NewRootCommand 构建 CLI 根命令。
//
// 根命令本身可直接提问（ask 的简写形态）：
//
//	magic-agent -p "问题"          （-p/--prompt 提示词）
//	magic-agent "问题"             （位置参数同样有效）
//	magic-agent -e codebuddy -m hy3 "问题"
//
// 实现方式：ask 的全部 flags 注册为 root 的 persistent flags，
// 子命令共享同一份绑定变量；root 无子命令名时直接执行 ask 主流程。
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

示例：
  magic-agent -p "用一句话解释什么是熵"            # 根命令直接提问（默认 json 输出）
  magic-agent ask "问题"                           # ask 子命令等价
  magic-agent -e codebuddy "写一首俳句"            # codebuddy 默认 hy3
  magic-agent -e codebuddy -m glm-5.3 "写一首俳句"
  magic-agent -e trae -t 10m "总结这篇文档"        # 默认关工具；--tools on 可开
  magic-agent -e claude --tools Bash,Read "看看这个目录"  # 工具白名单
  cat doc.md | magic-agent -e claude -f - "总结上文"
  magic-agent -e claude -r 2 "1+1=?"               # 失败重试 2 次`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAsk(cmd, args, opts)
		},
	}
	bindAskFlags(root, opts)
	root.AddCommand(newAskCommand(opts))
	root.AddCommand(newEnginesCommand())
	root.AddCommand(newVersionCommand())
	// --version 输出与 version 子命令一致（"magic-agent <ver>"，不带 "version " 前缀）。
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	return root
}

// Execute 运行 CLI，返回进程退出码。
func Execute() int {
	root := NewRootCommand()
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "magic-agent: %v\n", err)
		return exitCodeOf(err)
	}
	return 0
}

// 退出码约定。
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// usageError 标记参数/输入错误（exit 2）。
type usageError struct{ err error }

func (u *usageError) Error() string { return u.err.Error() }

// exitCodeOf 从错误推断退出码。
func exitCodeOf(err error) int {
	if _, ok := err.(*usageError); ok {
		return exitUsage
	}
	return exitFail
}

// newVersionCommand version 子命令。
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "显示版本信息",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "magic-agent %s\n", Version)
			return nil
		},
	}
}

// newEnginesCommand engines 子命令：列出引擎与 CLI 探测结果。
func newEnginesCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "engines",
		Short: "列出支持的引擎与本机 CLI 可用性",
		RunE: func(cmd *cobra.Command, args []string) error {
			engines := agent.Engines()
			if asJSON {
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
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出")
	return cmd
}
