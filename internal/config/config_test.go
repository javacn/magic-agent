package config

// config_test.go - 配置文件加载器单元测试。
//
// 覆盖：路径解析优先级（MAGIC_AGENT_CONFIG / XDG_CONFIG_HOME / ~/.config）、
// 文件缺失与空文件视为零值、语法错必须上报、宽松键名兼容、环境变量覆盖
// 文件值、Ready/Missing 判定。
//
// 全部用 t.TempDir()，不触碰用户真实配置。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv 清空本包关心的全部环境变量，保证测试不受外部环境影响。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvPath, "XDG_CONFIG_HOME", EnvArkClawURL, EnvArkClawKey, EnvArkClawClawID, EnvSystemPrompt} {
		t.Setenv(k, "")
	}
}

// writeConfig 把内容落到临时文件，返回路径。
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFromFile(t *testing.T) {
	clearEnv(t)
	p := writeConfig(t, `{"arkclaw":{"url":"https://h/a2a/jsonrpc","key":"k1","claw_id":"ci-1"}}`)

	c, err := LoadFrom(p)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.ArkClaw.URL != "https://h/a2a/jsonrpc" || c.ArkClaw.Key != "k1" || c.ArkClaw.ClawID != "ci-1" {
		t.Errorf("ArkClaw = %+v", c.ArkClaw)
	}
	if !c.ArkClaw.Ready() {
		t.Errorf("Ready() = false, Missing = %v", c.ArkClaw.Missing())
	}
}

func TestLoadFromAcceptsLooseKeys(t *testing.T) {
	clearEnv(t)
	// 等价写法：endpoint / apikey / clawId。
	p := writeConfig(t, `{"arkclaw":{"endpoint":"https://h/a2a/jsonrpc","apikey":"k2","clawId":"ci-2"}}`)

	c, err := LoadFrom(p)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.ArkClaw.URL != "https://h/a2a/jsonrpc" || c.ArkClaw.Key != "k2" || c.ArkClaw.ClawID != "ci-2" {
		t.Errorf("宽松键名未生效: %+v", c.ArkClaw)
	}

	// api_key 也应被接受。
	p2 := writeConfig(t, `{"arkclaw":{"url":"u","api_key":"k3","clawID":"ci-3"}}`)
	c2, err := LoadFrom(p2)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c2.ArkClaw.Key != "k3" || c2.ArkClaw.ClawID != "ci-3" {
		t.Errorf("api_key / clawID 未生效: %+v", c2.ArkClaw)
	}
}

func TestLoadFromMissingFileIsNotAnError(t *testing.T) {
	clearEnv(t)
	c, err := LoadFrom(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("文件不存在不应报错: %v", err)
	}
	if c.ArkClaw.URL != "" || c.ArkClaw.Key != "" || c.ArkClaw.ClawID != "" {
		t.Errorf("期望零值配置, got %+v", c.ArkClaw)
	}
	if c.ArkClaw.Ready() {
		t.Error("零值配置 Ready() 应为 false")
	}
}

func TestLoadFromEmptyPathIsNotAnError(t *testing.T) {
	clearEnv(t)
	if _, err := LoadFrom(""); err != nil {
		t.Fatalf("空路径不应报错: %v", err)
	}
}

func TestLoadFromBlankFileIsNotAnError(t *testing.T) {
	clearEnv(t)
	p := writeConfig(t, "  \n\t\n")
	c, err := LoadFrom(p)
	if err != nil {
		t.Fatalf("空白文件不应报错: %v", err)
	}
	if c.ArkClaw.URL != "" {
		t.Errorf("期望零值, got %+v", c.ArkClaw)
	}
}

func TestLoadFromMalformedReportsError(t *testing.T) {
	clearEnv(t)
	p := writeConfig(t, `{"arkclaw":{"url":`)
	_, err := LoadFrom(p)
	if err == nil {
		t.Fatal("语法错必须上报，不能静默吞掉")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("错误信息应含路径 %q: %v", p, err)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	clearEnv(t)
	p := writeConfig(t, `{"arkclaw":{"url":"https://file/a2a/jsonrpc","key":"file-key","claw_id":"file-claw"}}`)
	t.Setenv(EnvArkClawURL, "https://env/a2a/jsonrpc")
	t.Setenv(EnvArkClawKey, "env-key")

	c, err := LoadFrom(p)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.ArkClaw.URL != "https://env/a2a/jsonrpc" {
		t.Errorf("URL = %q want env 值", c.ArkClaw.URL)
	}
	if c.ArkClaw.Key != "env-key" {
		t.Errorf("Key = %q want env 值", c.ArkClaw.Key)
	}
	// 未被环境变量覆盖的字段应保留文件值。
	if c.ArkClaw.ClawID != "file-claw" {
		t.Errorf("ClawID = %q want 文件值", c.ArkClaw.ClawID)
	}
}

func TestEnvOnlyWithoutFile(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvArkClawURL, "https://env/a2a/jsonrpc")
	t.Setenv(EnvArkClawKey, "env-key")
	t.Setenv(EnvArkClawClawID, "env-claw")

	c, err := LoadFrom(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !c.ArkClaw.Ready() {
		t.Errorf("纯环境变量应可用: %+v", c.ArkClaw)
	}
}

// ── 默认系统提示词（systemPrompt）─────────────────────────────

// 顶层 systemPrompt 读取，且与 arkclaw 一节互不影响。
func TestLoadSystemPrompt(t *testing.T) {
	clearEnv(t)
	p := writeConfig(t, `{
	  "systemPrompt": "你是一个中文助手，始终用中文回答所有问题。",
	  "arkclaw": {"url":"u","key":"k","claw_id":"c"}
	}`)

	c, err := LoadFrom(p)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.SystemPrompt != "你是一个中文助手，始终用中文回答所有问题。" {
		t.Errorf("SystemPrompt = %q", c.SystemPrompt)
	}
	if !c.ArkClaw.Ready() {
		t.Errorf("arkclaw 一节应同时生效: %+v", c.ArkClaw)
	}
}

// 键名宽松兼容：system_prompt / system 任选其一。
func TestLoadSystemPromptLooseKeys(t *testing.T) {
	clearEnv(t)
	for _, tc := range []struct{ name, body string }{
		{"system_prompt", `{"system_prompt":"用中文回答"}`},
		{"system", `{"system":"用中文回答"}`},
		{"camelCase", `{"systemPrompt":"用中文回答"}`},
	} {
		c, err := LoadFrom(writeConfig(t, tc.body))
		if err != nil {
			t.Fatalf("%s: LoadFrom: %v", tc.name, err)
		}
		if c.SystemPrompt != "用中文回答" {
			t.Errorf("%s: SystemPrompt = %q want 用中文回答", tc.name, c.SystemPrompt)
		}
	}
}

// 未配置 / 空白 → 空串（= 不注入，保持原行为）。
func TestSystemPromptAbsentIsEmpty(t *testing.T) {
	clearEnv(t)
	c, err := LoadFrom(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.SystemPrompt != "" {
		t.Errorf("未配置时应为空串, got %q", c.SystemPrompt)
	}

	c2, err := LoadFrom(writeConfig(t, `{"systemPrompt":"   "}`))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c2.SystemPrompt != "" {
		t.Errorf("纯空白应视为未配置, got %q", c2.SystemPrompt)
	}
}

// 环境变量覆盖文件值（文件里有别的值也不生效）。
func TestSystemPromptEnvOverridesFile(t *testing.T) {
	clearEnv(t)
	p := writeConfig(t, `{"systemPrompt":"文件里的"}`)
	t.Setenv(EnvSystemPrompt, "环境变量里的")

	c, err := LoadFrom(p)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.SystemPrompt != "环境变量里的" {
		t.Errorf("SystemPrompt = %q want 环境变量值", c.SystemPrompt)
	}
}

// 纯环境变量、无配置文件同样可用。
func TestSystemPromptEnvOnlyWithoutFile(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvSystemPrompt, "只靠环境变量")

	c, err := LoadFrom(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.SystemPrompt != "只靠环境变量" {
		t.Errorf("SystemPrompt = %q", c.SystemPrompt)
	}
}

func TestPathPrefersConfigEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvPath, "/tmp/magic-agent-explicit.json")
	if got := Path(); got != "/tmp/magic-agent-explicit.json" {
		t.Errorf("Path() = %q", got)
	}
}

func TestPathExpandsTilde(t *testing.T) {
	clearEnv(t)
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("无法取得 home 目录")
	}
	t.Setenv(EnvPath, "~/custom/config.json")
	if got, want := Path(), filepath.Join(home, "custom", "config.json"); got != want {
		t.Errorf("Path() = %q want %q", got, want)
	}
}

func TestPathUsesXDGConfigHome(t *testing.T) {
	clearEnv(t)
	// ⚠️ 必须把 HOME 指到空临时目录：探测链第 2 档是 `~/.magic-agent/config.json`，
	// 而**开发机上这个文件是真实存在的**（用户 2026-09-23 定稿的新位置）——
	// 不隔离 HOME，本用例就会拿真机文件当输入，在别人的机器上假红/假绿。
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got, want := Path(), filepath.Join("/tmp/xdg", "magic-agent", "config.json"); got != want {
		t.Errorf("Path() = %q want %q", got, want)
	}
}

func TestPathFallsBackToHomeConfig(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := Path(), filepath.Join(home, ".config", "magic-agent", "config.json"); got != want {
		t.Errorf("Path() = %q want %q", got, want)
	}
}

/* ~/.magic-agent/config.json 的优先级（2026-09-23 加，用户：「应该放在 ~/.magic-agent/ 下」）。
 * 两条一起钉才有意义：**存在时用它**、**不存在时不顶掉老位置** ——
 * 只钉前者会把「老配置被静默忽略」这个最坏的回归放过去。 */
func TestPathPrefersMagicAgentHomeConfig(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	newPath := filepath.Join(home, ".magic-agent", "config.json")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte(`{"systemPrompt":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 老位置**同时存在**：新位置必须赢（否则「文件放对了却不生效」）。
	oldDir := filepath.Join(home, ".config", "magic-agent")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "config.json"), []byte(`{"systemPrompt":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Path(); got != newPath {
		t.Errorf("Path() = %q want %q（~/.magic-agent/config.json 存在时应优先）", got, newPath)
	}
}

func TestPathKeepsOldLocationWhenNewMissing(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	old := filepath.Join(home, ".config", "magic-agent", "config.json")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte(`{"systemPrompt":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 新位置**不存在** → 老位置必须照旧生效（不能因为「新位置优先」就把它顶掉）。
	if got := Path(); got != old {
		t.Errorf("Path() = %q want %q（新位置不存在时不许顶掉老位置）", got, old)
	}
}

/* agents 数组的解析（2026-09-23）：两种写法都要认 —— 凭据内嵌在 URL 里 / 分开写字段；
 * 键名别名与 arkclaw 节同一套（url/endpoint、key/apikey/api_key、claw_id/clawId/clawID）。 */
func TestAgentsParsedWithBothCredentialStyles(t *testing.T) {
	p := writeConfig(t, `{
	  "agents": [
	    { "name": "MagicAI", "url": "https://h/a2a/jsonrpc?apikey=K&clawId=C" },
	    { "name": "Second", "endpoint": "https://h2/a2a/jsonrpc", "apikey": "K2", "clawId": "C2" }
	  ]
	}`)
	c, err := LoadFrom(p)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if len(c.Agents) != 2 {
		t.Fatalf("Agents = %d 条，want 2", len(c.Agents))
	}
	a := c.Agents[0]
	if a.Name != "MagicAI" || a.URL != "https://h/a2a/jsonrpc?apikey=K&clawId=C" {
		t.Errorf("第 1 条 = %+v（URL 里内嵌凭据的写法）", a)
	}
	b := c.Agents[1]
	if b.Name != "Second" || b.URL != "https://h2/a2a/jsonrpc" || b.Key != "K2" || b.ClawID != "C2" {
		t.Errorf("第 2 条 = %+v（字段分开写 + 别名 endpoint/apikey/clawId）", b)
	}
}

func TestAgentsAbsentIsNotAnError(t *testing.T) {
	// 老配置（没有 agents 节）必须照旧可用 —— 新增字段不许把存量配置打成「解析失败」。
	p := writeConfig(t, `{"arkclaw":{"url":"u","key":"k","claw_id":"c"}}`)
	c, err := LoadFrom(p)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if len(c.Agents) != 0 {
		t.Errorf("Agents = %v，want 空", c.Agents)
	}
	if !c.ArkClaw.Ready() {
		t.Error("arkclaw 节应照旧解析出来")
	}
}

func TestMissingListsFieldNames(t *testing.T) {
	got := ArkClawConfig{URL: "u"}.Missing()
	if len(got) != 2 || got[0] != "key" || got[1] != "claw_id" {
		t.Errorf("Missing() = %v want [key claw_id]", got)
	}
	if (ArkClawConfig{}).Ready() {
		t.Error("空配置 Ready() 应为 false")
	}
	if !(ArkClawConfig{URL: "u", Key: "k", ClawID: "c"}).Ready() {
		t.Error("三项齐备 Ready() 应为 true")
	}
}

func TestWhitespaceOnlyValuesTreatedAsMissing(t *testing.T) {
	// 只有空白的值等同于未配置，避免把 " " 当成合法 apikey 发出去。
	c := ArkClawConfig{URL: "  ", Key: "k", ClawID: "c"}
	if c.Ready() {
		t.Error("纯空白 URL 应视为缺失")
	}
	if got := c.Missing(); len(got) != 1 || got[0] != "url" {
		t.Errorf("Missing() = %v", got)
	}
}

func TestLoadUsesPath(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"arkclaw":{"url":"u","key":"k","claw_id":"c"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPath, p)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ArkClaw.ClawID != "c" {
		t.Errorf("Load() 未走 EnvPath: %+v", c.ArkClaw)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("无法取得 home 目录")
	}
	cases := map[string]string{
		"":          "",
		"/abs/path": "/abs/path",
		"rel/path":  "rel/path",
		"~":         home,
		"~/a/b":     filepath.Join(home, "a", "b"),
		"~user/x":   "~user/x", // 不支持 ~user 形式，原样返回
	}
	for in, want := range cases {
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q) = %q want %q", in, got, want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "b", "c"); got != "b" {
		t.Errorf("firstNonEmpty = %q want b", got)
	}
	if got := firstNonEmpty("", " "); got != "" {
		t.Errorf("firstNonEmpty = %q want empty", got)
	}
	if got := firstNonEmpty(" a "); got != "a" {
		t.Errorf("firstNonEmpty 应去空白, got %q", got)
	}
}
