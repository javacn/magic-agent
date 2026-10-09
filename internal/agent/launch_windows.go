//go:build windows

package agent

// launch_windows.go - Windows 侧的 launcher 解析。
//
// PowerShell 可执行名按 powershell.exe → powershell → 固定名 顺序找。
// 优先找实际存在的那个，避免在只有 pwsh 的精简环境里退化成无效命令。

import "os/exec"

func platformIsWindows() bool { return true }

// powershellBin 找可用的 PowerShell。找不到时返回 "powershell.exe"
// 作为最后的兜底（让 exec 报「找不到文件」这种能看懂的错，
// 而不是静默失败）。
func powershellBin() string {
	for _, name := range []string{"powershell.exe", "powershell", "pwsh.exe", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil && p != "" {
			return p
		}
	}
	return "powershell.exe"
}
