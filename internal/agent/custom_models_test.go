package agent

// custom_models_test.go - 自定义模型端点同步（见 custom_models.go）。
//
// 背景回归：桌面 App 配好的 custom-local 模型，在独立 CLI 下会报
// "Custom model <id> has no endpoint url configured" —— 端点必须落在
// <配置目录>/models.json。这里锁住四条性质：
//  1. 首次同步：建文件、含 url/apiKey、availableModels 与 CLI 自己的行为对齐、权限 600（含密钥）
//  2. 幂等：无新增时**不落盘**（内容逐字节不变）
//  3. 只增不改：既有条目与未识别字段原样保留；availableModels 追加不丢
//  4. 安全兜底：App 缓存缺失 / 条目 disabled → 不建文件、不报错

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeJSON 写一个 JSON 文件（测试夹具）。
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// appCacheFixture 造一份 App 合并配置缓存的等价结构（嵌套 models + disabled 条目 + 非模型字段）。
func appCacheFixture() map[string]any {
	return map[string]any{
		"authentication": map[string]any{"id": "codebuddy"},
		"models": []any{
			map[string]any{"id": "hy3", "credits": "x0.00"},
			map[string]any{
				"id": "custom-local:MiniMax-M3", "name": "MiniMax-M3",
				"url": "https://api.minimaxi.com/v1", "apiKey": "sk-test-key",
				"tags": []any{"custom"}, "supportsImages": true,
			},
			map[string]any{
				"id": "custom-local:MiniMax-M3.1-Flash-Preview", "name": "MiniMax-M3.1-Flash-Preview",
				"url": "https://api.minimaxi.com/v1", "apiKey": "sk-test-key-2",
			},
			// disabled 条目不得同步
			map[string]any{
				"id": "custom-local:disabled-one", "url": "https://x/v1",
				"apiKey": "sk-dead", "disabled": true,
			},
		},
	}
}

func TestSyncCustomModelsFromApp_FirstSync(t *testing.T) {
	dir := t.TempDir()
	acc := filepath.Join(dir, "acc-product-config-v3.json")
	writeJSON(t, acc, appCacheFixture())
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}

	added, err := syncCustomModelsFromApp(acc, cfg)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2（两个 enabled 的 custom-local）", added)
	}

	path := filepath.Join(cfg, customModelsFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("models.json 应被创建: %v", err)
	}
	var doc struct {
		Models          []map[string]any `json:"models"`
		AvailableModels []string         `json:"availableModels"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse models.json: %v", err)
	}
	if len(doc.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(doc.Models))
	}
	// 端点与密钥字段必须原样带过去（CLI 就是靠 url 才能路由）
	var found bool
	for _, m := range doc.Models {
		if m["id"] == "custom-local:MiniMax-M3" {
			found = true
			if m["url"] != "https://api.minimaxi.com/v1" || m["apiKey"] != "sk-test-key" {
				t.Errorf("端点/密钥没带过去: %v", m)
			}
			if m["supportsImages"] != true {
				t.Errorf("其它字段应原样保留: %v", m)
			}
		}
	}
	if !found {
		t.Errorf("缺条目 custom-local:MiniMax-M3: %v", doc.Models)
	}
	// availableModels 与 CLI 自己的 saveCustomModel 行为对齐
	for _, id := range []string{"custom-local:MiniMax-M3", "custom-local:MiniMax-M3.1-Flash-Preview"} {
		if !containsStr(doc.AvailableModels, id) {
			t.Errorf("availableModels 缺 %q: %v", id, doc.AvailableModels)
		}
	}
	// 含第三方密钥 → 仅属主可读写
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("权限 = %v, want 0600（文件含 apiKey）", fi.Mode().Perm())
	}
}

func TestSyncCustomModelsFromApp_IdempotentAndMergeOnly(t *testing.T) {
	dir := t.TempDir()
	acc := filepath.Join(dir, "acc.json")
	writeJSON(t, acc, appCacheFixture())
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := syncCustomModelsFromApp(acc, cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, customModelsFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 第二次：无新增 → 不落盘（内容逐字节不变）
	added, err := syncCustomModelsFromApp(acc, cfg)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if added != 0 {
		t.Errorf("第二次应新增 0，得到 %d", added)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("无新增时不应改写 models.json")
	}

	// 用户手工加过条目 / 别的字段 / availableModels → 必须保留，只追加
	user := map[string]any{
		"models":          []any{map[string]any{"id": "custom-local:user-added", "url": "https://u/v1", "apiKey": "sk-u"}},
		"availableModels": []string{"custom-local:user-added", "hy3"},
		"unknownField":    "keep-me",
	}
	writeJSON(t, path, user)
	if _, err := syncCustomModelsFromApp(acc, cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["unknownField"] != "keep-me" {
		t.Errorf("未识别字段被改动了: %v", doc["unknownField"])
	}
	ids := map[string]bool{}
	for _, m := range doc["models"].([]any) {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	if !ids["custom-local:user-added"] || !ids["custom-local:MiniMax-M3"] {
		t.Errorf("应同时保留用户条目与同步条目: %v", ids)
	}
	avail, _ := doc["availableModels"].([]any)
	if !hasStringInList(avail, "custom-local:user-added") || !hasStringInList(avail, "hy3") {
		t.Errorf("既有 availableModels 不得丢失: %v", avail)
	}
}

func TestSyncCustomModelsFromApp_NoSourceNoFile(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	path := filepath.Join(cfg, customModelsFileName)

	cases := []struct{ name, acc string }{
		{"acc 路径为空", ""},
		{"acc 文件不存在", filepath.Join(dir, "nope.json")},
		{"acc 是坏 JSON", filepath.Join(dir, "broken.json")},
		{"acc 里没有自定义模型", filepath.Join(dir, "plain.json")},
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "plain.json"), map[string]any{
		"models": []any{map[string]any{"id": "hy3"}},
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			added, err := syncCustomModelsFromApp(tc.acc, cfg)
			if err != nil {
				t.Errorf("应静默返回，不该报错: %v", err)
			}
			if added != 0 {
				t.Errorf("added = %d, want 0", added)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("没有可同步的内容时不该建文件")
			}
		})
	}

	// configDir 为空同样静默
	if added, err := syncCustomModelsFromApp(filepath.Join(dir, "plain.json"), ""); added != 0 || err != nil {
		t.Errorf("configDir 为空应静默, got (%d, %v)", added, err)
	}
}

func TestSyncCustomModelsFromApp_CorruptExistingFileRebuilt(t *testing.T) {
	dir := t.TempDir()
	acc := filepath.Join(dir, "acc.json")
	writeJSON(t, acc, appCacheFixture())
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	// 既有 models.json 坏了：应重建（CLI 自己也这么兜：读失败当空），且不崩
	if err := os.WriteFile(filepath.Join(cfg, customModelsFileName), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := syncCustomModelsFromApp(acc, cfg)
	if err != nil {
		t.Fatalf("坏文件应可恢复: %v", err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2", added)
	}
}

func TestEnsureCustomModelEndpoint_OnlyCustomLocal(t *testing.T) {
	dir := t.TempDir()
	acc := filepath.Join(dir, "acc.json")
	writeJSON(t, acc, appCacheFixture())
	cfg := filepath.Join(dir, "config")

	c := codebuddyCore{name: "codebuddy", accConfigPath: acc, configDir: cfg}
	path := filepath.Join(cfg, customModelsFileName)

	// 官方模型：不该碰 models.json（避免给每条请求加无谓的文件读）
	c.ensureCustomModelEndpoint("hy3")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("官方模型不该触发同步")
	}

	// 自定义模型：补端点
	c.ensureCustomModelEndpoint("custom-local:MiniMax-M3")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("自定义模型应触发同步: %v", err)
	}

	// 用户显式指定 CODEBUDDY_CONFIG_DIR 时，落点跟着走（与 CLI 的实际读取一致）
	alt := filepath.Join(dir, "alt-config")
	t.Setenv("CODEBUDDY_CONFIG_DIR", alt)
	c.ensureCustomModelEndpoint("custom-local:MiniMax-M3")
	if _, err := os.Stat(filepath.Join(alt, customModelsFileName)); err != nil {
		t.Errorf("应写到 CODEBUDDY_CONFIG_DIR: %v", err)
	}
}

// hasStringInList 判断 []any 里是否有该字符串（availableModels 反序列化成 []any）。
func hasStringInList(list []any, want string) bool {
	for _, v := range list {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}
