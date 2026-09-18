package agent

// codex_home_test.go - CODEX_HOME 隔离逻辑测试。
//
// 硬约束：auth.json 永不进入镜像目录（cloud config 15s 超时根因的触发点）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelativeFileRefs(t *testing.T) {
	config := `
model_provider = "custom"
model_catalog_json = "cc-switch-model-catalog.json"
web_search = "disabled"
experimental_bearer_token = "PROXY_MANAGED"
base_url = "http://127.0.0.1:15721/v1"
source = "/abs/path/openai-bundled"
`
	got := relativeFileRefs(config)
	joined := strings.Join(got, ",")
	// 期望：纯文件名进入列表（去重后），路径形态/纯 token 不进入。
	if !strings.Contains(joined, "cc-switch-model-catalog.json") {
		t.Fatalf("model_catalog_json 未被识别: %v", got)
	}
	for _, banned := range []string{"custom", "PROXY_MANAGED", "disabled", "/abs/path/openai-bundled", "127.0.0.1"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("非文件名值 %q 混入引用列表: %v", banned, got)
		}
	}
}

func TestRelativeFileRefsNeverAuthJSON(t *testing.T) {
	config := "auth_json_note = \"auth.json\"\n"
	got := relativeFileRefs(config)
	for _, v := range got {
		if v == "auth.json" {
			t.Fatalf("auth.json 不允许进入同步列表: %v", got)
		}
	}
}

func TestSyncCodexHome(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	write := func(p, c string) {
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(src, "config.toml"),
		"model_catalog_json = \"catalog.json\"\n")
	write(filepath.Join(src, "catalog.json"), "{}")
	write(filepath.Join(src, "AGENTS.md"), "global rules")
	write(filepath.Join(src, "auth.json"), `{"auth_mode":"chatgpt"}`)

	if err := syncCodexHome(src, dst); err != nil {
		t.Fatalf("sync: %v", err)
	}
	for _, name := range []string{"config.toml", "catalog.json", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Fatalf("%s 未同步: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "auth.json")); err == nil {
		t.Fatal("auth.json 被同步进镜像目录（硬约束违反）")
	}

	// 二次同步：mtime/size 未变 → 幂等（不重写）。
	fi1, err := os.Stat(filepath.Join(dst, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := syncCodexHome(src, dst); err != nil {
		t.Fatal(err)
	}
	fi2, err := os.Stat(filepath.Join(dst, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("未变更时不应重写镜像文件")
	}

	// 源 config 变更 → 重新同步。
	write(filepath.Join(src, "config.toml"),
		"model_catalog_json = \"catalog2.json\"\n")
	write(filepath.Join(src, "catalog2.json"), "[]")
	if err := syncCodexHome(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "catalog2.json")); err != nil {
		t.Fatal("源变更后未重新同步")
	}
}

func TestEnsureCodexHomeRespectsExternalEnv(t *testing.T) {
	t.Setenv(codexHomeEnvKey, "/custom/codex-home")
	home, injected, err := ensureCodexHome()
	if err != nil || !injected || home != "/custom/codex-home" {
		t.Fatalf("MAGIC_AGENT_CODEX_HOME 应直接生效: home=%s injected=%v err=%v", home, injected, err)
	}

	t.Setenv(codexHomeEnvKey, "")
	t.Setenv("CODEX_HOME", "/user/managed")
	home, injected, err = ensureCodexHome()
	if err != nil || injected || home != "" {
		t.Fatalf("外部 CODEX_HOME 应被尊重（不隔离）: home=%s injected=%v err=%v", home, injected, err)
	}
}

func TestCodexWebSearchOverride(t *testing.T) {
	for _, v := range []string{"live", "cached", "indexed", "disabled"} {
		t.Setenv("MAGIC_AGENT_CODEX_WEBSEARCH", v)
		if got := codexWebSearchOverride(); got != v {
			t.Fatalf("websearch override = %q, want %q", got, v)
		}
	}
	t.Setenv("MAGIC_AGENT_CODEX_WEBSEARCH", "enabled") // 非法值域
	if got := codexWebSearchOverride(); got != "" {
		t.Fatalf("非法值应被忽略, got %q", got)
	}
	t.Setenv("MAGIC_AGENT_CODEX_WEBSEARCH", "")
	if got := codexWebSearchOverride(); got != "" {
		t.Fatalf("未设置时应为空, got %q", got)
	}
}
