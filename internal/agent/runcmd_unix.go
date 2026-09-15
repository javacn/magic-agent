//go:build !windows

package agent

// runcmd_unix.go - Unix (darwin/linux) 进程组实现。
//
// Setpgid 让子进程自成进程组（pgid = 子进程 pid），超时时用负 pid
// 向整组发 SIGKILL，CLI 内部 spawn 的 node worker 一并清理。

import (
	"os/exec"
	"syscall"
)

// configureProcAttr 让子进程进入独立进程组。
func configureProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 杀掉整个进程组（负 pid = 进程组）。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
