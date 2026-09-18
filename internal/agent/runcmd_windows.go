//go:build windows

package agent

// runcmd_windows.go - Windows 进程树实现。
//
// Windows 没有 POSIX 进程组语义，等价做法：
//   - 启动时置 CREATE_NEW_PROCESS_GROUP 让 CLI 自成一组（隔离 Ctrl-C、
//     便于整组信号）；
//   - 超时时用 `taskkill /T /F /PID <pid>` 强制杀掉整棵进程树
//     （/T 含所有后代），避免 node worker 变孤儿。
//
// 注释里刻意不写裸反斜杠，避免转义歧义。

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// createNewProcessGroup 对应 Windows CREATE_NEW_PROCESS_GROUP 标志。
const createNewProcessGroup = 0x00000200

// configureProcAttr 让子进程自成进程组。
func configureProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

// killProcessGroup 用 taskkill 杀掉进程树（/T 后代 + /F 强制）。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	// 优先整树杀；失败再退回单进程 Kill。
	if err := exec.Command("taskkill", "/T", "/F", "/PID", pid).Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}

// TerminatePID Windows 无 SIGTERM 语义 → 与 KillPID 同路（taskkill /T /F 杀整棵树）。
func TerminatePID(pid int, group bool) error {
	return KillPID(pid, group)
}

// KillPID 按 pid 杀进程树（group 参数在 Windows 无意义：taskkill /T 总是带后代）。
func KillPID(pid int, group bool) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// PIDAlive 用 tasklist 查该 pid 是否还在。
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), strconv.Itoa(pid))
}
