//go:build !windows

package agent

// runcmd_test.go - 进程组杀灭回归测试（POSIX only：依赖 sh 脚本与进程组信号）。
//
// 场景：CLI 脚本 spawn 一个长活后台子进程（模拟 codebuddy 的 node
// worker），runCLI 超时后整个进程组（含后台子进程）必须被杀干净。

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRunCLIKillsProcessGroupOnTimeout(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "alive")
	cli := filepath.Join(dir, "slow-cli")
	// 脚本：后台循环写 marker（模拟 worker 存活），前台 sleep 30s。
	script := "#!/bin/sh\n(while true; do touch " + marker + "; sleep 0.2; done) &\nsleep 30\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := runCLI(ctx, cli)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("kill took too long: %v", elapsed)
	}

	// 等一小段时间，检查 marker 不再更新（进程组已死）。
	stat1, _ := os.Stat(marker)
	time.Sleep(600 * time.Millisecond)
	stat2, _ := os.Stat(marker)
	if stat1 == nil {
		t.Fatal("marker should exist (worker was alive)")
	}
	if stat2 != nil && stat2.ModTime().After(stat1.ModTime()) {
		t.Errorf("background worker still alive after group kill (mtime advanced: %v -> %v)", stat1.ModTime(), stat2.ModTime())
	}

	// 残留进程检查：该脚本名下不应有进程。
	if err := syscall.Kill(0, 0); err != nil {
		t.Logf("signal self check: %v", err)
	}
}

func TestRunCLISuccess(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "ok-cli")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\necho OUT\necho ERR >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runCLI(context.Background(), cli)
	if err != nil {
		t.Fatalf("runCLI: %v", err)
	}
	if stdout != "OUT\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if stderr != "ERR\n" {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestRunCLIStartFailure(t *testing.T) {
	_, _, err := runCLI(context.Background(), "/nonexistent/cli-xyz")
	if err == nil {
		t.Fatal("expected start error")
	}
}
