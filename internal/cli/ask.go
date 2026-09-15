package cli

// ask.go - ask 主流程（root 简写与 ask 子命令共用）。
//
// 输入来源（优先级：--prompt > args > --file > stdin）：
//	1. -p/--prompt <text>：命令行直接给提示词
//	2. 位置参数拼接：magic-agent "问题"
//	3. --file <path>：文件全文（"-" = stdin）
//	4. stdin 管道（stdin 非 TTY 且无其他输入时自动读取）
// --file 与 -p/args 可组合：文件内容在前，-p/args 在后（附加上下文）。
//
// 工具开关（--tools）：
//	off（默认）  禁用全部工具 —— 纯 chat 一次成型，输出可解析
//	on           保持引擎默认工具集 + 权限旁路（agent 模式）
//	<白名单>     逗号分隔工具名（如 Bash,Read），仅允许这些工具
//	             + 权限旁路；引擎不支持的白名单成员忽略。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/darren/magic-agent/internal/agent"
)

// askOptions ask 的全部参数（root 与 ask 子命令共享同一份绑定）。
type askOptions struct {
	engine  string
	model   string
	system  string
	prompt  string
	file    string
	tools   string
	timeout time.Duration
	retries int
	backoff time.Duration
	output  string
	verbose bool
}

// newAskOptions 返回带默认值的选项集。
//   - 默认输出 json：固定单行 envelope，程序解析最友好；人看用 -o text。
//   - 默认引擎 codebuddy：hy3 免费模型开箱即用。
//   - 默认超时 600s：10 分钟，覆盖 codebuddy 慢响应场景。
func newAskOptions() *askOptions {
	return &askOptions{
		engine:  "codebuddy",
		output:  "json",
		timeout: 600 * time.Second,
	}
}

// bindAskFlags 把 ask 的 flags 注册为 cmd 的 persistent flags：
// root 上注册后 ask 子命令自动继承（子命令自身不再重复注册）。
func bindAskFlags(cmd *cobra.Command, opts *askOptions) {
	f := cmd.PersistentFlags()
	f.StringVarP(&opts.engine, "engine", "e", "codebuddy", "引擎: codebuddy | claude | trae（默认 codebuddy）")
	f.StringVarP(&opts.model, "model", "m", "", "模型（空 = 引擎默认：codebuddy=hy3）")
	f.StringVarP(&opts.system, "system", "s", "", "系统提示词")
	f.StringVarP(&opts.prompt, "prompt", "p", "", "提示词（根命令直接提问用）")
	f.StringVarP(&opts.file, "file", "f", "", "从文件读 prompt（\"-\" = stdin）")
	f.StringVar(&opts.tools, "tools", "off", "工具开关: off | on | 逗号分隔白名单(如 Bash,Read)")
	f.DurationVarP(&opts.timeout, "timeout", "t", 600*time.Second, "单次尝试超时（默认 600s=10m）")
	f.IntVarP(&opts.retries, "retries", "r", 0, "失败重试次数（默认 0）")
	f.DurationVar(&opts.backoff, "backoff", 2*time.Second, "首次重试退避间隔（指数翻倍，上限 30s）")
	f.StringVarP(&opts.output, "output", "o", "json", "输出格式: json | text")
	f.BoolVarP(&opts.verbose, "verbose", "v", false, "重试过程打印到 stderr")
}

// newAskCommand 构建 ask 子命令（与根命令简写等价；flags 继承 persistent）。
func newAskCommand(opts *askOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ask [flags] [prompt...]",
		Short: "向 agent 引擎提一个问题，输出固定格式结果（根命令的显式形式）",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAsk(cmd, args, opts)
		},
	}
	return cmd
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

// runAsk ask 主流程。
func runAsk(cmd *cobra.Command, args []string, opts *askOptions) error {
	// 1. 校验引擎与输出格式。
	engine := agent.Lookup(opts.engine)
	if engine == nil {
		names := make([]string, 0)
		for _, e := range agent.Engines() {
			names = append(names, e.Name())
		}
		return &usageError{fmt.Errorf("unknown engine %q (available: %s)", opts.engine, strings.Join(names, ", "))}
	}
	format, err := agent.ParseFormat(opts.output)
	if err != nil {
		return &usageError{err}
	}
	if opts.retries < 0 {
		return &usageError{fmt.Errorf("--retries must be >= 0, got %d", opts.retries)}
	}
	toolsMode, err := parseToolsMode(opts.tools)
	if err != nil {
		return err
	}

	// 2. 组装 prompt：-p > --file/stdin 内容 + 位置参数。
	promptParts, err := collectPrompt(cmd.InOrStdin(), args, opts.file)
	if err != nil {
		return &usageError{err}
	}
	if p := strings.TrimSpace(opts.prompt); p != "" {
		// --prompt 优先级最高：放到最前（同 --file 语义，附加内容跟后面）。
		promptParts = append([]string{p}, promptParts...)
	}
	prompt := strings.TrimSpace(strings.Join(promptParts, "\n\n"))
	if prompt == "" {
		return &usageError{fmt.Errorf("empty prompt: use -p, pass args, --file, or pipe stdin")}
	}

	// 3. 引擎 CLI 预检（快速失败，给出可操作提示）。
	if ok, note := engine.Detect(); !ok {
		return fmt.Errorf("engine %q unavailable: %s", engine.Name(), note)
	}

	// 4. 组装 Request + Runner，执行。
	req := agent.Request{
		Engine:       engine.Name(),
		Model:        opts.model,
		SystemPrompt: opts.system,
		Tools:        toolsMode,
		Messages: []agent.Message{
			{Role: "user", Content: prompt},
		},
	}
	runner := &agent.Runner{
		Engine:  engine,
		Timeout: opts.timeout,
		Retries: opts.retries,
		Backoff: opts.backoff,
		Verbose: opts.verbose,
	}
	resp, err := runner.Run(cmd.Context(), req)
	if err != nil {
		attempts := 1 + opts.retries
		_ = agent.WriteError(cmd.ErrOrStderr(), format, engine.Name(), attempts, err)
		return err
	}
	return agent.WriteOutput(cmd.OutOrStdout(), format, resp)
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
