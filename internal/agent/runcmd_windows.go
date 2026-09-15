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
	"os/exec"
	"strconv"
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
