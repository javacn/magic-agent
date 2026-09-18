//go:build !windows

package agent

// runcmd_unix.go - Unix (darwin/linux) 进程组实现。
//
// Setpgid 让子进程自成进程组（pgid = 子进程 pid），超时时用负 pid
// 向整组发 SIGKILL，CLI 内部 spawn 的 node worker 一并清理。

import (
	"errors"
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

// killTarget 把 (pid, group) 换算成 syscall.Kill 的目标：
// 进程组用负 pid（子 CLI 自成一组，pgid = 子 pid），单进程用正 pid。
func killTarget(pid int, group bool) int {
	if group {
		return -pid
	}
	return pid
}

// TerminatePID 按 pid 发 SIGTERM（group=true → 整个进程组）。
func TerminatePID(pid int, group bool) error {
	return syscall.Kill(killTarget(pid, group), syscall.SIGTERM)
}

// KillPID 按 pid 发 SIGKILL（group=true → 整个进程组）。
func KillPID(pid int, group bool) error {
	return syscall.Kill(killTarget(pid, group), syscall.SIGKILL)
}

// PIDAlive 报告 pid 是否还活着（signal 0 只做权限/存在性检查）。
// EPERM 也算活着（进程在，只是不归我们管）。
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
