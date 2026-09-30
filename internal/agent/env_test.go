//go:build !windows

package agent

// env_test.go - 子进程环境剔除规则回归测试。
//
// 背景：WorkBuddy 父会话把 SERVER__PORT 注入给子进程，codebuddy CLI
// 会用该端口再起一个服务，撞上父进程已监听 → EADDRINUSE → 永久挂起。
// 本文件锁定「SERVER__ 前缀必须被剔除」这一行为，防止回归。

import (
	"os"
	"strings"
	"testing"
)

func TestEnvironStripsServerPrefix(t *testing.T) {
	t.Setenv("SERVER__PORT", "58311")
	t.Setenv("SERVER__HOST", "127.0.0.1")

	for _, kv := range environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "SERVER__") {
			t.Errorf("SERVER__* 不应传给子 CLI，但出现: %s", kv)
		}
	}
}

func TestEnvironStripsParentSessionIDs(t *testing.T) {
	denied := []string{
		"CODEBUDDY_SESSION_ID",
		"CODEBUDDY_CONVERSATION_REQUEST_ID",
		"CODEBUDDY_CONVERSATION_MESSAGE_ID",
		"CODEBUDDY_TOOL_CALL_ID",
		"CLAUDE_SESSION_ID",
	}
	for _, k := range denied {
		t.Setenv(k, "test-value")
	}

	got := map[string]bool{}
	for _, kv := range environ() {
		name, _, _ := strings.Cut(kv, "=")
		got[name] = true
	}
	for _, k := range denied {
		if got[k] {
			t.Errorf("父会话标识 %s 不应传子 CLI", k)
		}
	}
}

// 基础变量必须保留：CLI 依赖 HOME/PATH，剔多了会直接跑不起来。
func TestEnvironKeepsEssentials(t *testing.T) {
	t.Setenv("MAGIC_AGENT_TEST_KEEP", "yes")
	got := map[string]bool{}
	for _, kv := range environ() {
		name, _, _ := strings.Cut(kv, "=")
		got[name] = true
	}
	for _, k := range []string{"HOME", "PATH", "MAGIC_AGENT_TEST_KEEP"} {
		if !got[k] {
			t.Errorf("基础变量 %s 被误剔除", k)
		}
	}
	if _, ok := os.LookupEnv("HOME"); ok && !got["HOME"] {
		t.Error("HOME 必须传递")
	}
}

// 宿主注入的 Python 变量必须剔除：llm 引擎由 venv python 启动，
// PYTHONHOME / PYTHONPATH 会打乱其 sys.path（见 env.go 注释）。
func TestEnvironStripsPythonVars(t *testing.T) {
	t.Setenv("PYTHONHOME", "/some/host/python")
	t.Setenv("PYTHONPATH", "/some/host/site-packages")

	got := map[string]bool{}
	for _, kv := range environ() {
		name, _, _ := strings.Cut(kv, "=")
		got[name] = true
	}
	for _, k := range []string{"PYTHONHOME", "PYTHONPATH"} {
		if got[k] {
			t.Errorf("%s 不应传给子 CLI（会破坏 llm 的 venv 解释器）", k)
		}
	}
}

// Windows 的变量名不区分大小写：父会话若写成混合大小写，也必须被剔除。
// 2026-09-30 事故：漏剔除 → 子 CLI 仍读到父进程端口 → EADDRINUSE 永久挂起，
// 表现就是「一进登录界面就上不去」。
func TestEnvDeniedCaseInsensitive(t *testing.T) {
	cases := map[string]bool{
		"server__port":         true,
		"Server__Port":         true,
		"server__host":         true,
		"codebuddy_session_id": true,
		"Codebuddy_Session_Id": true,
		"pythonhome":           true,
		"PATH":                 false,
		"HOME":                 false,
		// 仍必须放行：引擎靠它选配置目录（见 codebuddyAccountEnv）
		"codebuddy_config_dir": false,
	}
	for name, want := range cases {
		if got := envDenied(name); got != want {
			t.Errorf("envDenied(%q) = %v, want %v", name, got, want)
		}
	}
}

// 混合大小写的父会话变量在 environ() 里也必须不见。
func TestEnvironStripsMixedCaseServerPrefix(t *testing.T) {
	t.Setenv("server__port", "58311")
	t.Setenv("Server__Host", "127.0.0.1")
	t.Setenv("codebuddy_session_id", "should-be-stripped")

	for _, kv := range environ() {
		name, _, _ := strings.Cut(kv, "=")
		if envDenied(name) {
			t.Errorf("被拒绝的变量仍出现在子 CLI 环境里: %s", kv)
		}
	}
}

func TestEnvDenied(t *testing.T) {
	cases := map[string]bool{
		"SERVER__PORT":                true,
		"SERVER__HOST":                true,
		"SERVER__ANYTHING":            true,
		"CODEBUDDY_SESSION_ID":        true,
		"CODEBUDDY_SERVICE_PROXY_URL": false, // 保留：CLI 可能依赖父会话服务
		"CODEBUDDY_MCP_CONFIG":        false,
		"HOME":                        false,
		"PATH":                        false,
		"CODEBUDDY_CONFIG_DIR":        false,
	}
	for name, want := range cases {
		if got := envDenied(name); got != want {
			t.Errorf("envDenied(%q) = %v, want %v", name, got, want)
		}
	}
}
