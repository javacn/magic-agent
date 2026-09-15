package cli

// ask.go - ask 子命令：单次提问，核心命令。
//
// 输入来源（三选一，优先级：args > --file > stdin）：
//	1. 位置参数拼接：magic-agent ask "问题"
//	2. --file <path>：文件全文（"-" = stdin）
//	3. stdin 管道（stdin 非 TTY 且无参数时自动读取）
// --file/- 与位置参数可组合：文件内容在前，参数在后（便于附加上下文）。

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

// askOptions ask 的全部参数。
type askOptions struct {
	engine  string
	model   string
	system  string
	file    string
	timeout time.Duration
	retries int
	backoff time.Duration
	output  string
	verbose bool
}

// newAskCommand 构建 ask 子命令。
func newAskCommand() *cobra.Command {
	opts := &askOptions{}
	cmd := &cobra.Command{
		Use:   "ask [flags] [prompt...]",
		Short: "向 agent 引擎提一个问题，输出固定格式结果",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAsk(cmd, args, opts)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&opts.engine, "engine", "e", "claude", "引擎: claude | codebuddy | trae")
	f.StringVarP(&opts.model, "model", "m", "", "模型（空 = 引擎默认）")
	f.StringVarP(&opts.system, "system", "s", "", "系统提示词")
	f.StringVarP(&opts.file, "file", "f", "", "从文件读 prompt（\"-\" = stdin）")
	f.DurationVarP(&opts.timeout, "timeout", "t", 0, "单次尝试超时（如 3m；默认按引擎）")
	f.IntVarP(&opts.retries, "retries", "r", 0, "失败重试次数（默认 0）")
	f.DurationVar(&opts.backoff, "backoff", 2*time.Second, "首次重试退避间隔（指数翻倍，上限 30s）")
	f.StringVarP(&opts.output, "output", "o", "text", "输出格式: text | json")
	f.BoolVarP(&opts.verbose, "verbose", "v", false, "重试过程打印到 stderr")
	return cmd
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

	// 2. 组装 prompt：--file/stdin 内容 + 位置参数。
	promptParts, err := collectPrompt(cmd.InOrStdin(), args, opts.file)
	if err != nil {
		return &usageError{err}
	}
	prompt := strings.TrimSpace(strings.Join(promptParts, "\n\n"))
	if prompt == "" {
		return &usageError{fmt.Errorf("empty prompt: pass args, --file, or pipe stdin")}
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
