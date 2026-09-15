package agent

// env.go - 环境变量读取（darwin / linux / windows 通用）。

import "os"

// environ 返回当前进程环境变量（CLI 依赖 HOME/PATH 等基础变量）。
func environ() []string { return os.Environ() }
