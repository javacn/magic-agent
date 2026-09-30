package agent

// repair_models_test.go - 事故还原（--repair-models 的落地逻辑，见 custom_models.go）。
//
// 场景来自 2026-09-30 Windows 事故：桌面端 models.json 是**数组**，被反向同步覆盖成
// `{availableModels, models}` 对象后桌面端解析不了，模型清单整个为空。
// 这里锁住三件事：
//  1. 明确的坏形状 → 还原成数组，id 去掉 custom-local: 前缀，且先留 .broken.bak
//  2. 已经是数组 → 一个字都不动
//  3. 形状对不上（坏 JSON / 空对象 / models 里混着非 custom-local 条目）→ 一律不动

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clobberedFixture 造一份「被我们覆盖写坏」的 models.json：顶层对象 + 全 custom-local 条目。
func clobberedFixture() []byte {
	return []byte(`{
  "availableModels": [
    "custom-local:MiniMax-M3",
    "custom-local:glm-5.3"
  ],
  "models": [
    {
      "id": "custom-local:MiniMax-M3",
      "name": "MiniMax-M3",
      "vendor": "MiniMax",
      "url": "https://api.minimaxi.com/v1/chat/completions",
      "apiKey": "sk-user-own",
      "supportsImages": false
    },
    {
      "id": "custom-local:glm-5.3",
      "name": "glm-5.3",
      "vendor": "Zhipu",
      "url": "https://open.bigmodel.cn/api/paas/v4",
      "apiKey": "sk-user-own-2"
    }
  ]
}
`)
}

func TestRepairModelsFile_RestoresArrayFormat(t *testing.T) {
	appHome := t.TempDir()
	path := filepath.Join(appHome, customModelsFileName)
	if err := os.WriteFile(path, clobberedFixture(), 0o600); err != nil {
		t.Fatal(err)
	}

	repaired, note, err := repairModelsFile(appHome)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !repaired {
		t.Fatalf("坏形状应被还原，note=%q", note)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(b, &arr); err != nil {
		t.Fatalf("还原后应是数组: %v（内容 %s）", err, b)
	}
	if len(arr) != 2 {
		t.Fatalf("条数 = %d, want 2", len(arr))
	}
	for _, m := range arr {
		id, _ := m["id"].(string)
		if id == "" || strings.HasPrefix(id, customModelIDPrefix) {
			t.Errorf("id 必须去掉 %s 前缀，实际 %q", customModelIDPrefix, id)
		}
	}
	if arr[0]["id"] != "MiniMax-M3" {
		t.Errorf("首条 id = %v, want MiniMax-M3", arr[0]["id"])
	}
	// 端点与密钥必须原样保留（丢了就只能手抄）
	if arr[0]["url"] != "https://api.minimaxi.com/v1/chat/completions" || arr[0]["apiKey"] != "sk-user-own" {
		t.Errorf("端点/密钥没保留: %v", arr[0])
	}

	// 现场必须留备份，且备份是坏的那份原文
	bak, err := os.ReadFile(path + ".broken.bak")
	if err != nil {
		t.Fatalf("应留 .broken.bak: %v", err)
	}
	if !bytes.Equal(bak, clobberedFixture()) {
		t.Error(".broken.bak 应为被写坏的原文")
	}
}

func TestRepairModelsFile_LeavesGoodShapesAlone(t *testing.T) {
	// 桌面端原生数组格式
	native := []byte("[\n  {\n    \"id\": \"MiniMax-M3\",\n    \"url\": \"https://x/v1\"\n  }\n]\n")

	cases := []struct {
		name    string
		content []byte
	}{
		{"已是数组（桌面端原生格式）", native},
		{"坏 JSON", []byte("{oops")},
		{"空 JSON 对象", []byte("{}")},
		{"顶层是字符串", []byte(`"nope"`)},
		{"models 里混着非 custom-local 条目", []byte(`{"models":[{"id":"hy3","url":"https://x"}]}`)},
		{"models 是空数组", []byte(`{"models":[],"availableModels":[]}`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			appHome := t.TempDir()
			path := filepath.Join(appHome, customModelsFileName)
			if err := os.WriteFile(path, tc.content, 0o600); err != nil {
				t.Fatal(err)
			}

			repaired, _, err := repairModelsFile(appHome)
			if err != nil {
				t.Fatalf("形状对不上应静默不改，不该报错: %v", err)
			}
			if repaired {
				t.Errorf("形状对不上不该动手")
			}
			after, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if !bytes.Equal(after, tc.content) {
				t.Errorf("原文件被改写了\nbefore=%s\nafter =%s", tc.content, after)
			}
			if _, serr := os.Stat(path + ".broken.bak"); !os.IsNotExist(serr) {
				t.Errorf("没还原就不该留 .broken.bak")
			}
		})
	}
}

func TestRepairModelsFile_MissingFileIsNoop(t *testing.T) {
	appHome := t.TempDir()
	repaired, note, err := repairModelsFile(appHome)
	if err != nil {
		t.Fatalf("没有文件不该报错: %v", err)
	}
	if repaired {
		t.Error("没有文件时不谈还原")
	}
	if note == "" {
		t.Error("应给出「不存在、无需修复」的说明，不静默")
	}
}

// 幂等：还原过一次之后，再跑一次必须什么都不做（已经是数组）。
func TestRepairModelsFile_Idempotent(t *testing.T) {
	appHome := t.TempDir()
	path := filepath.Join(appHome, customModelsFileName)
	if err := os.WriteFile(path, clobberedFixture(), 0o600); err != nil {
		t.Fatal(err)
	}
	if repaired, _, err := repairModelsFile(appHome); err != nil || !repaired {
		t.Fatalf("首次应还原: repaired=%v err=%v", repaired, err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	repaired, _, err := repairModelsFile(appHome)
	if err != nil {
		t.Fatal(err)
	}
	if repaired {
		t.Error("第二次不该再动手")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("第二次不该改动文件")
	}
}

// 引擎名到桌面端数据目录的映射：不认识的名字必须报错，而不是猜一个目录去写。
func TestRepairDesktopModels_UnknownEngine(t *testing.T) {
	if _, _, err := RepairDesktopModels("no-such-engine"); err == nil {
		t.Fatal("没有桌面端目录的引擎应报错")
	}
}
