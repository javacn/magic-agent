// Command magic-agent - 专业的 agent CLI 代理工具。
package main

import (
	"os"

	"github.com/darren/magic-agent/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
