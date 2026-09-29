//go:build !windows

package session

// session_kill_unix_test.go - 「真的把进程停掉」的取证（unix）。
//
// 用一个真的 `sleep 30`（Setpgid 自成进程组，与引擎 CLI 同姿势）当靶子：
// Stop 之后必须 Stopped=true 且进程真的没了；重复 Stop 必须幂等。

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/darren/magic-agent/internal/agent"
)

func TestStopKillsRealProcessGroup(t *testing.T) {
	isolate(t)

	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 引擎 CLI 就是这么起的
	if err := cmd.Start(); err != nil {
		t.Fatalf("起测试进程: %v", err)
	}
	// 必须及时 reap：僵尸进程 kill(pid,0) 仍返回成功，会让 waitGone 白等到超时。
	// （真实链路里 magic-agent 自己会 cmd.Wait()，等价于这里。）
	go func() { _ = cmd.Wait() }()

	pid := cmd.Process.Pid
	if !agent.PIDAlive(pid) {
		t.Fatal("测试进程没起来")
	}

	h, err := Begin(BeginOptions{Engine: "claude", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	h.SetPID(pid, true) // spawn 钩子在真实链路里做的事

	res, err := Stop(h.RunID())
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.Stopped {
		t.Fatalf("应 stopped=true: %+v", res)
	}
	if res.Record.State != StateCancelled {
		t.Errorf("state = %q want %q", res.Record.State, StateCancelled)
	}
	if agent.PIDAlive(pid) {
		t.Errorf("pid %d 还活着 —— 没真停掉", pid)
	}

	// 幂等：再停一次不报错、stopped=false
	res2, err2 := Stop(h.RunID())
	if err2 != nil {
		t.Fatalf("重复停止应幂等: %v", err2)
	}
	if res2.Stopped {
		t.Errorf("第二次不该报 stopped: %+v", res2)
	}
}

// 用 session_id 也能停（观物台/看板手里通常只有会话 id）。
func TestStopBySessionID(t *testing.T) {
	isolate(t)

	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("起测试进程: %v", err)
	}
	go func() { _ = cmd.Wait() }()

	h, _ := Begin(BeginOptions{Engine: "claude", SessionID: "sess-xyz"})
	h.SetPID(cmd.Process.Pid, true)

	res, err := Stop("sess-xyz")
	if err != nil {
		t.Fatalf("Stop(session_id): %v", err)
	}
	if !res.Stopped || res.Record.SessionID != "sess-xyz" {
		t.Errorf("按 session_id 停失败: %+v", res)
	}
	if agent.PIDAlive(cmd.Process.Pid) {
		t.Error("进程还在")
	}
}
