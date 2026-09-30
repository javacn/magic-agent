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
//  5. 陌生文件一律不碰：顶层不是 JSON 对象（桌面端是数组）/ 坏 JSON → 逐字节保持原样，
//     且覆盖写之前一定留 .magic-agent.bak（见 2026-09-30 Windows 事故用例）

import (
	"bytes"
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

// 目标文件顶层不是 JSON 对象（桌面端 WorkBuddy 的 models.json 就是**数组**）时，
// 必须**一个字都不动**、也不留备份（因为压根没写）。
//
// 为什么是关键回归：以前把"解析不进 map"当空文件继续写回 {models, availableModels}，
// 等于把对方的模型清单整体替换成只剩 custom-local 几条 —— 官方模型全丢且不可回滚。
func TestSyncCustomModelsFromApp_RefusesNonObjectFile(t *testing.T) {
	// 桌面端 models.json 的真实形状：顶层数组，条目是 LanguageModel。
	desktop := []byte(`[
  {
    "id": "MiniMax-M3",
    "name": "MiniMax-M3",
    "vendor": "MiniMax",
    "url": "https://api.minimaxi.com/v1/chat/completions",
    "apiKey": "sk-user-own"
  },
  {
    "id": "glm-5.3",
    "name": "glm-5.3",
    "url": "https://open.bigmodel.cn/api/paas/v4",
    "apiKey": "sk-user-own-2"
  }
]`)

	cases := []struct {
		name    string
		content []byte
	}{
		{"顶层是数组（桌面端格式）", desktop},
		{"坏 JSON", []byte("{oops")},
		{"顶层是字符串", []byte(`"just a string"`)},
		{"顶层是数字", []byte(`42`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			acc := filepath.Join(dir, "acc.json")
			writeJSON(t, acc, appCacheFixture())
			cfg := filepath.Join(dir, "config")
			if err := os.MkdirAll(cfg, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(cfg, customModelsFileName)
			if err := os.WriteFile(path, tc.content, 0o600); err != nil {
				t.Fatal(err)
			}

			added, err := syncCustomModelsFromApp(acc, cfg)
			if err != nil {
				t.Errorf("应静默跳过，不该报错: %v", err)
			}
			if added != 0 {
				t.Errorf("added = %d, want 0", added)
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("原文件必须还在: %v", err)
			}
			if !bytes.Equal(after, tc.content) {
				t.Errorf("原文件被改写了！\nbefore=%s\nafter =%s", tc.content, after)
			}
			if _, err := os.Stat(path + customModelsBackupSuffix); !os.IsNotExist(err) {
				t.Errorf("没写就不该留备份")
			}
		})
	}
}

// 真的要覆盖写时，必须先把原文留成 <models.json>.magic-agent.bak（可回滚）。
func TestSyncCustomModelsFromApp_BacksUpBeforeOverwrite(t *testing.T) {
	dir := t.TempDir()
	acc := filepath.Join(dir, "acc.json")
	writeJSON(t, acc, appCacheFixture())
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, customModelsFileName)

	before := []byte("{\n  \"models\": [],\n  \"availableModels\": [\"hy3\"]\n}\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}

	added, err := syncCustomModelsFromApp(acc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if added == 0 {
		t.Fatal("本次应有新增（触发落盘）")
	}

	bak, err := os.ReadFile(path + customModelsBackupSuffix)
	if err != nil {
		t.Fatalf("覆盖写之前应留备份: %v", err)
	}
	if !bytes.Equal(bak, before) {
		t.Errorf("备份内容应为改动前的原文\nwant=%s\ngot =%s", before, bak)
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

/* 既有 models.json 无法确认为「CLI 自己的格式」时，一律**不碰**，而不是重建。
 *
 * 这条契约是 2026-09-30 改的（Windows 事故）。原行为是「解析失败当空文件，重建」，
 * 理由看起来无害（CLI 自己也这么兜底）。实际后果是把**别人的**文件整体替换掉：
 * 桌面端 WorkBuddy 的 models.json 顶层是数组，数组解析不进 map[string]any，
 * 于是被当成「坏文件」重建 → 里面 61 条官方模型全没了，只剩 8 条 custom-local，
 * 而且没有备份，不可回滚。
 *
 * 宁可少同步一次端点（CLI 会自己报 "has no endpoint url configured"），
 * 也不要毁掉一份不是我们管的配置。 */
func TestSyncCustomModelsFromApp_CorruptExistingFileLeftUntouched(t *testing.T) {
	dir := t.TempDir()
	acc := filepath.Join(dir, "acc.json")
	writeJSON(t, acc, appCacheFixture())
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, customModelsFileName)
	broken := []byte("{oops")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}

	added, err := syncCustomModelsFromApp(acc, cfg)
	if err != nil {
		t.Fatalf("不认识的文件应静默跳过，不该报错: %v", err)
	}
	if added != 0 {
		t.Errorf("added = %d, want 0（不碰未知格式）", added)
	}
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("原文件必须还在: %v", rerr)
	}
	if !bytes.Equal(after, broken) {
		t.Errorf("原文件被改写了：%s", after)
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
