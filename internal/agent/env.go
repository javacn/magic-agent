package agent

// env.go - 子进程环境构造（darwin / linux / windows 通用）。
//
// 除继承当前环境（CLI 依赖 HOME/PATH 等基础变量）外，还要剔除若干
// 「只能属于父进程」的变量。见 childEnvDenylist。

import (
	"os"
	"strings"
)

// serverEnvPrefix 剔除的前缀：父会话内置 HTTP 服务的监听地址。
//
// WorkBuddy 父会话用 SERVER__HOST / SERVER__PORT（双下划线）把自身
// 内置 HTTP 服务的监听端口注入给子进程。codebuddy CLI 读到这两个变量后
// 会在**同一个端口**再起一个服务，撞上父进程已监听 → EADDRINUSE →
// 启动流程里该错误是 unhandled rejection，CLI 既不退出也不产出任何
// stdout/stderr，表现为**永久挂起**。
//
// 实测（2026-09-15）：
//
//	继承 SERVER__PORT=<父进程端口>   → 100s+ 无输出，必挂
//	剔除 SERVER__PORT               → 4~9s 正常返回
//	换成任意空闲端口                 → 正常返回
//
// 这是「在 WorkBuddy 会话里嵌套调用 codebuddy 引擎永远没结果」的根因，
// 不是 CLI 本身的问题（干净环境下同一命令数秒即回）。因此子进程一律
// 不继承该前缀；CLI 不需要、也不应该绑定父进程的服务端口。
const serverEnvPrefix = "SERVER__"

// envDenylist 精确剔除的变量名（前缀规则之外的部分）。
var envDenylist = map[string]bool{
	// 父会话的会话/请求标识：子进程复用会让 CLI 误以为自己是既有会话
	// 的重入调用，可能串到父会话上下文，故一并剔除。
	"CODEBUDDY_SESSION_ID":              true,
	"CODEBUDDY_CONVERSATION_REQUEST_ID": true,
	"CODEBUDDY_CONVERSATION_MESSAGE_ID": true,
	"CODEBUDDY_TOOL_CALL_ID":            true,
	"CLAUDE_SESSION_ID":                 true,
}

// environ 返回供子 CLI 继承的环境变量（已剔除父进程专属项）。
func environ() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if envDenied(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// envDenied 判断某环境变量是否不允许传给子 CLI。
func envDenied(name string) bool {
	if strings.HasPrefix(name, serverEnvPrefix) {
		return true
	}
	return envDenylist[name]
}
