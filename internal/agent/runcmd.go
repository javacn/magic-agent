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
