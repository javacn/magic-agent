package agent

// models_test.go - models.json 加载与模型解析的测试（扁平数组格式）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeModels 把内容写到临时 models.json，返回路径。
func writeModels(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "models.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write models: %v", err)
	}
	return p
}

// withModels 临时把 MAGIC_AGENT_MODELS 指向给定内容，测试结束自动恢复。
func withModels(t *testing.T, body string) string {
	t.Helper()
	p := writeModels(t, body)
	t.Setenv("MAGIC_AGENT_MODELS", p)
	return p
}

// sampleModels 是扁平数组样本：三条条目，首条即默认。
const sampleModels = `[
  {
    "id": "MiniMax-M3",
    "name": "MiniMax-M3",
    "vendor": "MiniMax",
    "url": "https://api.minimaxi.com/v1/chat/completions",
    "apiKey": "k1",
    "supportsToolCall": true,
    "supportsImages": true,
    "supportsReasoning": true,
    "maxInputTokens": 1000000,
    "maxOutputTokens": 524288,
    "timeout": 600,
    "extraBody": {"thinking": {"type": "disabled"}}
  },
  {
    "id": "minimax-peter",
    "model": "MiniMax-M3",
    "url": "https://api.minimaxi.com/v1/chat/completions",
    "apiKey": "k2",
    "timeout": 600
  },
  {
    "id": "ark-glm",
    "url": "https://ark.cn-beijing.volces.com/api/coding/v3",
    "apiKey": "k3"
  }
]`

// TestLoadModelsFlatArray 扁平数组可加载，首条即默认。
func TestLoadModelsFlatArray(t *testing.T) {
	p := withModels(t, sampleModels)

	entries, path, err := LoadModels()
	if err != nil {
		t.Fatalf("LoadModels: %v", err)
	}
	if path != p {
		t.Errorf("path = %q, want %q", path, p)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if entries[0].ID != "MiniMax-M3" {
		t.Errorf("first entry id = %q, want MiniMax-M3", entries[0].ID)
	}
	if entries[0].Vendor != "MiniMax" {
		t.Errorf("vendor = %q, want MiniMax", entries[0].Vendor)
	}
	if entries[0].MaxInputTokens != 1000000 {
		t.Errorf("maxInputTokens = %d, want 1000000", entries[0].MaxInputTokens)
	}
}

// TestResolveModelByID 按 id 精确匹配。
func TestResolveModelByID(t *testing.T) {
	withModels(t, sampleModels)
	entries, _, _ := LoadModels()

	m, err := ResolveModel(entries, "minimax-peter")
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if m.ID != "minimax-peter" || m.WireModel() != "MiniMax-M3" {
		t.Errorf("got id=%q wire=%q, want minimax-peter/MiniMax-M3", m.ID, m.WireModel())
	}
}

// TestResolveModelDefaultIsFirst 空 query → 首条。
func TestResolveModelDefaultIsFirst(t *testing.T) {
	withModels(t, sampleModels)
	entries, _, _ := LoadModels()

	m, err := ResolveModel(entries, "")
	if err != nil {
		t.Fatalf("ResolveModel(\"\"): %v", err)
	}
	if m.ID != "MiniMax-M3" {
		t.Errorf("default id = %q, want MiniMax-M3 (first entry)", m.ID)
	}
}

// TestResolveModelCaseInsensitive id 匹配忽略大小写，并回填规范大小写。
func TestResolveModelCaseInsensitive(t *testing.T) {
	withModels(t, sampleModels)
	entries, _, _ := LoadModels()

	m, err := ResolveModel(entries, "MINIMAX-m3")
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if m.ID != "MiniMax-M3" {
		t.Errorf("id = %q, want canonical MiniMax-M3", m.ID)
	}
}

// TestResolveModelUnknownListsIDs 未知 id 报错并列出可用 id。
func TestResolveModelUnknownListsIDs(t *testing.T) {
	withModels(t, sampleModels)
	entries, _, _ := LoadModels()

	_, err := ResolveModel(entries, "gpt-9")
	if err == nil {
		t.Fatal("want error for unknown model, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
	for _, id := range []string{"MiniMax-M3", "minimax-peter", "ark-glm"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error should list available id %q: %v", id, err)
		}
	}
}

// TestLoadModelsInvalidJSON 文件存在但 JSON 非法时直接报错（不静默跳过）。
func TestLoadModelsInvalidJSON(t *testing.T) {
	withModels(t, `[{ this is not json }]`)

	_, path, err := LoadModels()
	if err == nil {
		t.Fatal("want parse error")
	}
	if !strings.Contains(err.Error(), "parse models config") {
		t.Errorf("error = %v, want parse error", err)
	}
	if path == "" {
		t.Error("path should be reported for a malformed file")
	}
}

// TestLoadModelsEmptyArray 空数组报错。
func TestLoadModelsEmptyArray(t *testing.T) {
	withModels(t, `[]`)

	_, _, err := LoadModels()
	if err == nil {
		t.Fatal("want error for empty array")
	}
	if !strings.Contains(err.Error(), "defines no models") {
		t.Errorf("error = %v, want 'defines no models'", err)
	}
}

// TestLoadModelsMissing 全部候选缺失时返回 ErrModelsNotFound。
func TestLoadModelsMissing(t *testing.T) {
	// 指向不存在路径，并让 home 候选也落空（用 t.Setenv 影响 HOME）。
	t.Setenv("MAGIC_AGENT_MODELS", filepath.Join(t.TempDir(), "nope.json"))
	t.Setenv("HOME", t.TempDir())

	_, _, err := LoadModels()
	if err == nil {
		t.Fatal("want error when no models config exists")
	}
	if !strings.Contains(err.Error(), "models config not found") {
		t.Errorf("error = %v, want ErrModelsNotFound text", err)
	}
}

// TestDuplicateIDRejected 重复 id 拒绝加载，并提示用 model 字段区分。
func TestDuplicateIDRejected(t *testing.T) {
	withModels(t, `[
	  {"id":"MiniMax-M3","url":"https://a/v1","apiKey":"k"},
	  {"id":"minimax-m3","url":"https://b/v1","apiKey":"k2"}
	]`)

	_, _, err := LoadModels()
	if err == nil {
		t.Fatal("want error for duplicate id")
	}
	if !strings.Contains(err.Error(), "duplicate id") {
		t.Errorf("error = %v, want 'duplicate id'", err)
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("error should suggest the \"model\" field: %v", err)
	}
}

// TestMissingURLRejected 缺 url 拒绝加载。
func TestMissingURLRejected(t *testing.T) {
	withModels(t, `[{"id":"no-url","apiKey":"k"}]`)

	_, _, err := LoadModels()
	if err == nil {
		t.Fatal("want error for missing url")
	}
	if !strings.Contains(err.Error(), "missing \"url\"") {
		t.Errorf("error = %v, want missing url", err)
	}
}

// TestMissingIDRejected 缺 id 拒绝加载。
func TestMissingIDRejected(t *testing.T) {
	withModels(t, `[{"url":"https://a/v1","apiKey":"k"}]`)

	_, _, err := LoadModels()
	if err == nil {
		t.Fatal("want error for missing id")
	}
	if !strings.Contains(err.Error(), "missing \"id\"") {
		t.Errorf("error = %v, want missing id", err)
	}
}

// TestExtraBodyPreserved extraBody 必须原样保留（MiniMax 关思维链依赖它）。
func TestExtraBodyPreserved(t *testing.T) {
	withModels(t, sampleModels)
	entries, _, _ := LoadModels()

	thinking, ok := entries[0].ExtraBody["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("extraBody.thinking = %#v, want object", entries[0].ExtraBody["thinking"])
	}
	if thinking["type"] != "disabled" {
		t.Errorf("thinking.type = %v, want disabled", thinking["type"])
	}
}

// TestWireModelFallback model 为空时回退到 id。
func TestWireModelFallback(t *testing.T) {
	withModels(t, sampleModels)
	entries, _, _ := LoadModels()

	if got := entries[0].WireModel(); got != "MiniMax-M3" {
		t.Errorf("WireModel = %q, want MiniMax-M3 (model empty → id)", got)
	}
	if got := entries[1].WireModel(); got != "MiniMax-M3" {
		t.Errorf("WireModel = %q, want MiniMax-M3 (explicit model)", got)
	}
}

// TestHTTPTimeout 超时字段解析。
func TestHTTPTimeout(t *testing.T) {
	var m ModelEntry
	if err := json.Unmarshal([]byte(`{"id":"x","url":"https://a/v1","timeout":600}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := m.HTTPTimeout().Seconds(); got != 600 {
		t.Errorf("HTTPTimeout = %v, want 600s", got)
	}
	var zero ModelEntry
	if zero.HTTPTimeout() != 0 {
		t.Error("zero timeout should stay 0 (caller default)")
	}
}

// ── Endpoint 三态 ─────────────────────────────────────────────

// TestEndpointAlreadySuffixed 已以 /chat/completions 结尾 → 不重复追加。
func TestEndpointAlreadySuffixed(t *testing.T) {
	m := ModelEntry{URL: "https://api.minimaxi.com/v1/chat/completions"}
	if got := m.Endpoint(); got != "https://api.minimaxi.com/v1/chat/completions" {
		t.Errorf("Endpoint = %q, want unchanged", got)
	}
	// 尾部斜杠也应被规整。
	m2 := ModelEntry{URL: "https://api.minimaxi.com/v1/chat/completions/"}
	if got := m2.Endpoint(); got != "https://api.minimaxi.com/v1/chat/completions" {
		t.Errorf("Endpoint = %q, want trailing slash trimmed", got)
	}
}

// TestEndpointNeedsAppend base 地址 → 补 /chat/completions。
func TestEndpointNeedsAppend(t *testing.T) {
	cases := map[string]string{
		"https://ark.cn-beijing.volces.com/api/coding/v3": "https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions",
		"https://api.minimaxi.com/v1":                     "https://api.minimaxi.com/v1/chat/completions",
		"http://localhost:11434/v1":                       "http://localhost:11434/v1/chat/completions",
		"https://api.minimaxi.com/v1/":                    "https://api.minimaxi.com/v1/chat/completions",
	}
	for in, want := range cases {
		if got := (ModelEntry{URL: in}).Endpoint(); got != want {
			t.Errorf("Endpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEndpointCustomProtocolVerbatim useCustomProtocol → 原样透传。
func TestEndpointCustomProtocolVerbatim(t *testing.T) {
	m := ModelEntry{URL: "https://proxy.example.com/custom/path", UseCustomProtocol: true}
	if got := m.Endpoint(); got != "https://proxy.example.com/custom/path" {
		t.Errorf("Endpoint = %q, want verbatim", got)
	}
	// 空 URL 保持空（由加载期校验拦截）。
	if got := (ModelEntry{}).Endpoint(); got != "" {
		t.Errorf("Endpoint of empty url = %q, want empty", got)
	}
}

// ── ${ENV_VAR} 展开 ───────────────────────────────────────────

// TestEnvVarExpansion 已设置的变量被展开。
func TestEnvVarExpansion(t *testing.T) {
	withModels(t, `[
	  {"id":"a","url":"https://${MAGIC_AGENT_TEST_HOST}/v1","apiKey":"${MAGIC_AGENT_TEST_KEY}"}
	]`)
	t.Setenv("MAGIC_AGENT_TEST_KEY", "sk-from-env")
	t.Setenv("MAGIC_AGENT_TEST_HOST", "api.example.com")

	entries, _, err := LoadModels()
	if err != nil {
		t.Fatalf("LoadModels: %v", err)
	}
	if entries[0].APIKey != "sk-from-env" {
		t.Errorf("apiKey = %q, want expanded", entries[0].APIKey)
	}
	if entries[0].URL != "https://api.example.com/v1" {
		t.Errorf("url = %q, want expanded", entries[0].URL)
	}
}

// TestEnvVarUnsetKeepsPlaceholder 未设置的变量保留占位符（不置空）。
func TestEnvVarUnsetKeepsPlaceholder(t *testing.T) {
	withModels(t, `[{"id":"a","url":"https://a/v1","apiKey":"${MAGIC_AGENT_DEFINITELY_UNSET_VAR}"}]`)

	entries, _, err := LoadModels()
	if err != nil {
		t.Fatalf("LoadModels: %v", err)
	}
	if entries[0].APIKey != "${MAGIC_AGENT_DEFINITELY_UNSET_VAR}" {
		t.Errorf("apiKey = %q, want placeholder preserved", entries[0].APIKey)
	}
}

// ── 展示与密钥判定 ────────────────────────────────────────────

// TestDisplayNameAndHasAPIKey 展示名回退链与密钥判定。
func TestDisplayNameAndHasAPIKey(t *testing.T) {
	if got := (ModelEntry{ID: "x", Name: "Pretty"}).DisplayName(); got != "Pretty" {
		t.Errorf("DisplayName = %q, want Pretty", got)
	}
	if got := (ModelEntry{ID: "x"}).DisplayName(); got != "x" {
		t.Errorf("DisplayName = %q, want x", got)
	}
	if got := (ModelEntry{}).DisplayName(); got != "<unnamed>" {
		t.Errorf("DisplayName = %q, want <unnamed>", got)
	}
	if (ModelEntry{APIKey: "  "}).HasAPIKey() {
		t.Error("blank apiKey should report false")
	}
	if !(ModelEntry{APIKey: "k"}).HasAPIKey() {
		t.Error("non-blank apiKey should report true")
	}
}
