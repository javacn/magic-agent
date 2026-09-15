package agent

// models_test.go - models.json 加载与模型解析的测试。

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

const sampleModels = `{
  "default": "minimax/MiniMax-M3",
  "providers": [
    {"name":"minimax","baseUrl":"https://api.minimaxi.com/v1","apiKey":"k1",
     "timeout":600,"models":[{"id":"MiniMax-M3","input":["text","image"]}],
     "extraBody":{"thinking":{"type":"disabled"}}},
    {"name":"codebuddy","api":"codebuddy-cli","models":[{"id":"hy3"},{"id":"hy3-x"}]},
    {"name":"local","api":"ollama","models":[{"id":"qwen2.5:0.5b"}]}
  ]
}`

// TestLoadModelsOwnShape 验证本工具形状（providers[]）可加载。
func TestLoadModelsOwnShape(t *testing.T) {
	p := withModels(t, sampleModels)

	mf, path, err := LoadModels()
	if err != nil {
		t.Fatalf("LoadModels: %v", err)
	}
	if path != p {
		t.Errorf("path = %q, want %q", path, p)
	}
	provs := mf.effectiveProviders()
	if len(provs) != 3 {
		t.Fatalf("providers = %d, want 3", len(provs))
	}
	if provs[0].Name != "minimax" {
		t.Errorf("first provider = %q, want minimax", provs[0].Name)
	}
}

// TestLoadModelsMagicVideoShape 验证兼容 magic-video 的 models.default 形状。
func TestLoadModelsMagicVideoShape(t *testing.T) {
	withModels(t, `{
	  "llm": {"textModels":["MiniMax-M3"]},
	  "models": {"default":[
	    {"name":"minimax","baseUrl":"https://api.minimaxi.com/v1","apiKey":"k1",
	     "timeout":600,"models":[{"id":"MiniMax-M3"}]}
	  ]}
	}`)

	mf, _, err := LoadModels()
	if err != nil {
		t.Fatalf("LoadModels: %v", err)
	}
	provs := mf.effectiveProviders()
	if len(provs) != 1 || provs[0].Name != "minimax" {
		t.Fatalf("providers = %+v, want one minimax", provs)
	}
	// magic-video 形状下 default 落在 llm.textModels，解析需回退到首模型。
	prov, bare, err := ResolveModel(mf, "")
	if err != nil {
		t.Fatalf("ResolveModel(\"\"): %v", err)
	}
	if prov.Name != "minimax" || bare != "MiniMax-M3" {
		t.Errorf("got %s/%s, want minimax/MiniMax-M3", prov.Name, bare)
	}
}

// TestResolveModelProviderQualified 显式 provider/model 形式。
func TestResolveModelProviderQualified(t *testing.T) {
	withModels(t, sampleModels)
	mf, _, _ := LoadModels()

	prov, bare, err := ResolveModel(mf, "codebuddy/hy3-x")
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if prov.Name != "codebuddy" || bare != "hy3-x" {
		t.Errorf("got %s/%s, want codebuddy/hy3-x", prov.Name, bare)
	}
	if api := prov.EffectiveAPI(); api != APICodeBuddy {
		t.Errorf("api = %q, want %q", api, APICodeBuddy)
	}
}

// TestResolveModelBareName 裸模型名按声明顺序取首个命中 provider。
func TestResolveModelBareName(t *testing.T) {
	withModels(t, sampleModels)
	mf, _, _ := LoadModels()

	prov, bare, err := ResolveModel(mf, "MiniMax-M3")
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	if prov.Name != "minimax" || bare != "MiniMax-M3" {
		t.Errorf("got %s/%s, want minimax/MiniMax-M3", prov.Name, bare)
	}
	// ollama provider 的模型也应能按裸名命中。
	prov, _, err = ResolveModel(mf, "qwen2.5:0.5b")
	if err != nil {
		t.Fatalf("ResolveModel(qwen): %v", err)
	}
	if prov.EffectiveAPI() != APIOllama {
		t.Errorf("api = %q, want %q", prov.EffectiveAPI(), APIOllama)
	}
}

// TestResolveModelDefault 空模型名走配置的 default。
func TestResolveModelDefault(t *testing.T) {
	withModels(t, sampleModels)
	mf, _, _ := LoadModels()

	prov, bare, err := ResolveModel(mf, "")
	if err != nil {
		t.Fatalf("ResolveModel(\"\"): %v", err)
	}
	if prov.Name != "minimax" || bare != "MiniMax-M3" {
		t.Errorf("got %s/%s, want default minimax/MiniMax-M3", prov.Name, bare)
	}
}

// TestResolveModelCaseInsensitive provider 名与模型 id 匹配忽略大小写。
func TestResolveModelCaseInsensitive(t *testing.T) {
	withModels(t, sampleModels)
	mf, _, _ := LoadModels()

	prov, bare, err := ResolveModel(mf, "MINIMAX/minimax-m3")
	if err != nil {
		t.Fatalf("ResolveModel: %v", err)
	}
	// 裸 id 应回填为 provider 中声明的规范大小写。
	if bare != "MiniMax-M3" {
		t.Errorf("bare = %q, want canonical MiniMax-M3", bare)
	}
	if prov.Name != "minimax" {
		t.Errorf("provider = %q, want minimax", prov.Name)
	}
}

// TestResolveModelUnknownModel 未知模型给出可用清单。
func TestResolveModelUnknownModel(t *testing.T) {
	withModels(t, sampleModels)
	mf, _, _ := LoadModels()

	_, _, err := ResolveModel(mf, "gpt-9")
	if err == nil {
		t.Fatal("want error for unknown model, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want 'not found'", err)
	}
	if !strings.Contains(err.Error(), "minimax/MiniMax-M3") {
		t.Errorf("error should list available models: %v", err)
	}
}

// TestResolveModelUnknownProvider 未知 provider 报错。
func TestResolveModelUnknownProvider(t *testing.T) {
	withModels(t, sampleModels)
	mf, _, _ := LoadModels()

	_, _, err := ResolveModel(mf, "nope/MiniMax-M3")
	if err == nil {
		t.Fatal("want error for unknown provider, got nil")
	}
	if !strings.Contains(err.Error(), "provider \"nope\" not found") {
		t.Errorf("error = %v", err)
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

// TestLoadModelsInvalidJSON 文件存在但 JSON 非法时直接报错（不静默跳过）。
func TestLoadModelsInvalidJSON(t *testing.T) {
	withModels(t, `{"providers": [ this is not json }`)

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

// TestLoadModelsNoProviders 无任何 provider 时报错。
func TestLoadModelsNoProviders(t *testing.T) {
	withModels(t, `{"default":"x"}`)

	_, _, err := LoadModels()
	if err == nil {
		t.Fatal("want error when providers empty")
	}
	if !strings.Contains(err.Error(), "defines no providers") {
		t.Errorf("error = %v", err)
	}
}

// TestExtraBodyPreserved extraBody 必须原样保留（MiniMax 关思维链依赖它）。
func TestExtraBodyPreserved(t *testing.T) {
	withModels(t, sampleModels)
	mf, _, _ := LoadModels()

	prov, _, _ := ResolveModel(mf, "minimax/MiniMax-M3")
	if len(prov.ExtraBody) == 0 {
		t.Fatal("extraBody lost during load")
	}
	thinking, ok := prov.ExtraBody["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("extraBody.thinking = %#v, want object", prov.ExtraBody["thinking"])
	}
	if thinking["type"] != "disabled" {
		t.Errorf("thinking.type = %v, want disabled", thinking["type"])
	}
}

// TestProviderHasModelEmptyModelsDeclared 未声明 models 的 provider 接受任意模型（透传）。
func TestProviderHasModelEmptyModelsDeclared(t *testing.T) {
	withModels(t, `{"providers":[{"name":"wild","baseUrl":"http://x/v1"}]}`)
	mf, _, _ := LoadModels()

	prov, bare, err := ResolveModel(mf, "anything-goes")
	if err != nil {
		t.Fatalf("provider without models should accept any model: %v", err)
	}
	if prov.Name != "wild" || bare != "anything-goes" {
		t.Errorf("got %s/%s", prov.Name, bare)
	}
}

// TestHTTPTimeout 超时字段解析。
func TestHTTPTimeout(t *testing.T) {
	var p Provider
	if err := json.Unmarshal([]byte(`{"name":"x","timeout":600}`), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := p.HTTPTimeout().Seconds(); got != 600 {
		t.Errorf("HTTPTimeout = %v, want 600s", got)
	}
	var zero Provider
	if zero.HTTPTimeout() != 0 {
		t.Error("zero timeout should stay 0 (caller default)")
	}
}
