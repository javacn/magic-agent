//go:build !windows

package agent

// launch_unix.go - Unix (darwin/linux) 侧的 launcher 解析。
//
// unix 上没有这一层：npm 装的 shim 是无扩展名的脚本（带 shebang），
// exec 可以直接执行；launchWrap 在 launch.go 里对非 Windows 直接原样返回。
// 本文件只提供两个平台分派用的最小实现。

func platformIsWindows() bool { return false }

// powershellBin 在 unix 上不会被调用（launchWrap 不走包装分支）。
// 保留实现只为让 launch.go 保持可编译，不返回真路径。
func powershellBin() string { return "" }
