package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMaxTokensSettingsHelper maxTokensSettings 的载荷与边界：
// n<=0 视为未设置（ok=false，不注入 --settings）。
func TestMaxTokensSettingsHelper(t *testing.T) {
	if payload, ok := maxTokensSettings(0); ok {
		t.Errorf("0 应视为未设置，got payload=%q", payload)
	}
	if payload, ok := maxTokensSettings(-5); ok {
		t.Errorf("负数应视为未设置，got payload=%q", payload)
	}
	payload, ok := maxTokensSettings(8000)
	if !ok {
		t.Fatal("8000 应产生载荷")
	}
	want := `{"env":{"CLAUDE_CODE_MAX_OUTPUT_TOKENS":"8000"}}`
	if payload != want {
		t.Errorf("payload = %q, want %q", payload, want)
	}
}

// TestExpandHome 基类路径展开：仅处理 ~ 与 ~/ 前缀。
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("无法获取主目录: %v", err)
	}
	cases := []struct {
		in   string
		want string
	}{
		{"~/bin/x", filepath.Join(home, "bin/x")},
		{"~", home},
		{"~user/x", "~user/x"},        // 不支持 ~user 形式
		{"/opt/homebrew/bin", "/opt/homebrew/bin"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := expandHome(tc.in); got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCLIResolveEnvVar 探测链优先级：显式 BinPath > 环境变量 > 候选路径。
func TestCLIResolveEnvVar(t *testing.T) {
	t.Setenv("MAGIC_AGENT_LLM_BIN", "/custom/llm-path")
	if got := llmBase.resolve(""); got != "/custom/llm-path" {
		t.Errorf("env 覆盖未生效: %q", got)
	}
	// 显式路径优先于环境变量。
	if got := llmBase.resolve("/explicit/bin"); got != "/explicit/bin" {
		t.Errorf("显式路径应最优先: %q", got)
	}
}
