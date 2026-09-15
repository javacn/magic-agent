package agent

// runcmd.go - 进程组感知的 CLI 执行辅助（平台无关部分）。
//
// 三家 CLI 都是 node 启动器，内部还会 spawn worker 子进程。
// exec.CommandContext 超时只杀直接子进程，CLI 的 worker 会变孤儿
// 继续跑（实测残留多个 node 进程）。这里统一用进程组：
//
//	1. configureProcAttr: 让 CLI 及其全部后代进独立进程组（组长 = CLI）；
//	2. ctx 结束/超时时 killProcessGroup 杀掉整组；
//	3. cmd.Wait() 自然回收。
//
// 平台差异（Setpgid/负 pid kill vs Windows taskkill /T）分别放在
// runcmd_unix.go 与 runcmd_windows.go。

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runCLI 在独立进程组里执行 bin args，返回 (stdout, stderr, err)。
// 超时/取消时杀掉整个进程组（含 CLI 的全部后代进程）。
func runCLI(ctx context.Context, bin string, args ...string) (string, string, error) {
	cmd := exec.Command(bin, args...)
	configureProcAttr(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = envWithDefaults()

	if err := cmd.Start(); err != nil {
		return "", "", err
	}

	// 看门狗：ctx 结束 → 杀整组。
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case err := <-waitErr:
		return stdout.String(), stderr.String(), err
	case <-ctx.Done():
		killProcessGroup(cmd)
		<-waitErr // 回收
		return stdout.String(), stderr.String(), fmt.Errorf("process group killed: %w", ctx.Err())
	}
}

// envWithDefaults 继承当前环境（CLI 依赖 HOME/PATH 等基础变量）。
func envWithDefaults() []string {
	return environ()
}

// stderrSummary 从 CLI 的 stderr 里提取人可读的失败摘要：
// 取最后一行非空输出（CLI 报错通常在末尾），去 ANSI 色码，截断。
func stderrSummary(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return truncateStr(stripANSI(s), 500)
		}
	}
	return ""
}

// wrapCliError 统一包装引擎 CLI 的执行失败。
//
// 外层文本 "<engine> CLI: <cliCauseError>"（cliCauseError.Error() 自带
// stderr 摘要），错误链经 %w 保持完整 —— WriteError 由此提取 reason。
//
// stderr 为空时（典型：超时被杀）不追加 stderr 摘要，避免拿 err 自身
// 回填造成重复。
func wrapCliError(engine, stdout, stderr string, err error) error {
	summary := stderrSummary(stderr)
	return fmt.Errorf("%s CLI: %w", engine, &cliCauseError{cause: err, summary: summary})
}

// cliCauseError CLI 失败的根因载体。
// Error() 输出格式：
//
//	有摘要  "<cause> (stderr: <摘要>)"
//	无摘要  "<cause>"（超时 / fork 失败类）
type cliCauseError struct {
	cause   error
	summary string
}

func (e *cliCauseError) Error() string {
	if e.summary != "" {
		return fmt.Sprintf("%v (stderr: %s)", e.cause, e.summary)
	}
	return e.cause.Error()
}

func (e *cliCauseError) Cause() error { return e.cause }

// Summary stderr 摘要；空表示无（超时/fork 失败类）。
func (e *cliCauseError) Summary() string { return e.summary }

// Unwrap 支持 errors.Is/As 沿链下钻。
func (e *cliCauseError) Unwrap() error { return e.cause }

// stripANSI 去除 ANSI 转义序列（色码），避免错误信息里混入 "\x1b[31m"。
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEsc = true
		case inEsc && (r == 'm' || r == 'K' || r == 'H'):
			inEsc = false // CSI 序列终结符
		case !inEsc:
			b.WriteRune(r)
		}
	}
	return b.String()
}
