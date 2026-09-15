package agent

// env_unix.go - Unix (darwin/linux) 环境变量读取。

import "os"

func environ() []string { return os.Environ() }
