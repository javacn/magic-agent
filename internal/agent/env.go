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

	// Python 解释器路径注入：宿主环境（如 TRAE 会话）会把 PYTHONHOME 指到
	// 自带的 Python.framework、PYTHONPATH 指到跨版本 site-packages。
	// llm CLI 由 venv python 启动，读到这两个变量后 sys.path 错乱，
	// 直接以 path 配置 dump / "<no Python frame>" 退出（实测 2026-09-16）。
	// venv 自带隔离，不需要宿主注入；node 系 CLI（claude/codebuddy/trae）不受影响。
	"PYTHONHOME": true,
	"PYTHONPATH": true,
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

// ChildEnvWith 返回「子进程应继承的环境（environ()，已剔除父进程专属项）」
// 追加 extraEnv 后的结果。
//
// exec 对重复 key 取**后出现**的那份，所以 extraEnv 可以覆盖继承来的同名变量
// —— 这正是账号隔离需要的语义（见 codebuddy.go 的 codebuddyAccountEnv）。
// 交互式登录入口（--login）也用它构造环境，保证与普通调用同源。
func ChildEnvWith(extraEnv []string) []string {
	if len(extraEnv) == 0 {
		return environ()
	}
	out := environ()
	return append(out, extraEnv...)
}

// envDenied 判断某环境变量是否不允许传给子 CLI。
//
// ⚠️ 大小写不敏感（2026-09-30 修，Windows 事故）：环境变量名在 **Windows 上本身
// 不区分大小写**，父会话用 `server__port` / `Server__Port` 这类大小写写出来时，
// 原来的逐字符比较会**漏剔除** —— 子 CLI 于是仍读到父进程的监听端口，再起一个服务
// 撞上 EADDRINUSE，表现为「一进登录界面就卡死 / 嵌套调用永远没结果」。
// Unix 上变量名区分大小写，折叠大小写只会**更保守**（多剔除几个父会话风格的名字），
// 不会少剔除，故这里两个平台统一按大写比较。
func envDenied(name string) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, serverEnvPrefix) {
		return true
	}
	return envDenylist[upper]
}
