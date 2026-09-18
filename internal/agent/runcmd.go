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
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// spawnHookKey ctx 里携带「子进程已启动」回调的键（见 WithSpawnHook）。
type spawnHookKey struct{}

// WithSpawnHook 注册「引擎子进程刚启动」的回调（参数是子进程 pid）。
//
// 用途：会话登记表要记住**引擎子进程**的 pid —— 它自成进程组（Setpgid），
// 与 magic-agent 自己不在同一组，所以调用方杀 magic-agent 的进程组带不走它。
// 记录到 pid 后，`magic-agent --stop <会话>` 才能精确杀掉这条会话的引擎。
//
// 回调在 Start() 之后同步调用，只做写文件这类轻量事（不阻塞读流）。
func WithSpawnHook(ctx context.Context, fn func(pid int)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, spawnHookKey{}, fn)
}

// notifySpawn 通知 ctx 上注册的 spawn 回调（无回调时什么都不做）。
func notifySpawn(ctx context.Context, pid int) {
	if ctx == nil || pid <= 0 {
		return
	}
	if fn, ok := ctx.Value(spawnHookKey{}).(func(int)); ok && fn != nil {
		fn(pid)
	}
}

// runCLI 在独立进程组里执行 bin args，返回 (stdout, stderr, err)。
// 超时/取消时杀掉整个进程组（含 CLI 的全部后代进程）。
func runCLI(ctx context.Context, bin string, args ...string) (string, string, error) {
	return runCLIIn(ctx, "", bin, args...)
}

// runCLIIn 同 runCLI，但可指定工作目录 dir（空 = 继承调用方 cwd）。
// workspace 的主要落地手段就是它：claude / codebuddy / trae / openclaw 都没有
// 「工作目录」flag，**进程 cwd 就是它们的原生方式**（相对路径、CLAUDE.md 发现、
// git 上下文都跟着它走）；codex 另有原生 `-C/--cd`（见 codex.go buildArgs）。
func runCLIIn(ctx context.Context, dir string, bin string, args ...string) (string, string, error) {
	return runCLIEnvIn(ctx, dir, nil, bin, args...)
}

// runCLIEnv 在 runCLI 基础上为子进程追加环境变量（extra 为 K=V 切片，
// 追加在继承环境之后 —— Go exec 语义：重复 key 取最后一个，故 extra 覆盖继承值）。
func runCLIEnv(ctx context.Context, extra []string, bin string, args ...string) (string, string, error) {
	return runCLIEnvIn(ctx, "", extra, bin, args...)
}

// runCLIEnvIn = runCLIIn + runCLIEnv（工作目录 + 追加环境变量）。
func runCLIEnvIn(ctx context.Context, dir string, extra []string, bin string, args ...string) (string, string, error) {
	cmd := exec.Command(bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	configureProcAttr(cmd)

	/* stdout/stderr 用**自建管道 + 自己 drain**，不直挂 bytes.Buffer：
	   Go 只在目标是 *os.File 时才不做「内部拷贝 goroutine」，而 **cmd.Wait() 会等那些
	   goroutine 结束** —— 只要子进程还有后代握着管道写端（引擎 CLI 的 node worker 逃出
	   进程组时就是这样），Wait 就永远不返回：症状是「引擎已经死了，magic-agent 却挂着不退」。
	   （流式路径 2026-09-18 踩过一次，见 stream.go 的同类注释；这里按同一姿势一并修。） */
	stdoutR, stdoutW, perr := os.Pipe()
	if perr != nil {
		return "", "", perr
	}
	stderrR, stderrW, perr := os.Pipe()
	if perr != nil {
		stdoutR.Close()
		stdoutW.Close()
		return "", "", perr
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.Env = envWithDefaults()
	if len(extra) > 0 {
		cmd.Env = append(cmd.Env, extra...)
	}

	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return "", "", err
	}
	notifySpawn(ctx, cmd.Process.Pid) // 会话登记表据此记住引擎子进程 pid
	stdoutW.Close()
	stderrW.Close()
	drained := make(chan struct{}, 2)
	go func() { defer stdoutR.Close(); _, _ = io.Copy(&stdout, stdoutR); drained <- struct{}{} }()
	go func() { defer stderrR.Close(); _, _ = io.Copy(&stderr, stderrR); drained <- struct{}{} }()

	// 看门狗：ctx 结束 → 杀整组。
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case err := <-waitErr:
		waitDrained(drained, 2, 10*time.Second) // 进程已退：等 drain 读完余量（有限等待防后代握管道挂死）
		return stdout.String(), stderr.String(), err
	case <-ctx.Done():
		killProcessGroup(cmd)
		select {
		case <-waitErr:
		case <-time.After(10 * time.Second):
		}
		waitDrained(drained, 2, 5*time.Second)
		return stdout.String(), stderr.String(), fmt.Errorf("process group killed: %w", ctx.Err())
	}
}

// waitDrained 等 n 个 drain goroutine 收尾，最多等 d（防后代进程握着写端导致永久挂起）。
func waitDrained(ch <-chan struct{}, n int, d time.Duration) {
	deadline := time.After(d)
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-deadline:
			return
		}
	}
}

// envWithDefaults 继承当前环境（CLI 依赖 HOME/PATH 等基础变量）。
func envWithDefaults() []string {
	return environ()
}

// newStreamCmd 构造流式 CLI 进程（进程组 + 环境继承，stdout 留给调用方接管）。
func newStreamCmd(bin string, args []string) *exec.Cmd {
	return newStreamCmdIn("", bin, args)
}

// newStreamCmdIn 同 newStreamCmd，可指定工作目录（workspace，见 runCLIIn 注释）。
func newStreamCmdIn(dir string, bin string, args []string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	configureProcAttr(cmd)
	cmd.Env = envWithDefaults()
	return cmd
}

// streamStderrBuf 流式进程的 stderr 缓冲（保留尾部 32KB 供错误摘要）。
type streamStderrBuf struct {
	buf [32 * 1024]byte
	n   int
}

func (b *streamStderrBuf) Write(p []byte) (int, error) {
	if room := len(b.buf) - b.n; room > 0 {
		if len(p) <= room {
			copy(b.buf[b.n:], p)
			b.n += len(p)
			return len(p), nil
		}
		copy(b.buf[b.n:], p[:room])
		b.n = len(b.buf)
	}
	return len(p), nil
}

// String 尾部内容（去 ANSI）。
func (b *streamStderrBuf) String() string {
	return stripANSI(string(b.buf[:b.n]))
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
