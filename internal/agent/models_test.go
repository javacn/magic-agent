package agent

// models_test.go - 模型清单动态探测的测试。
//
// 覆盖：
//   - 三个纯文本/配置解析器（codebuddy --help / llm models / claude settings.json）
//   - JSON 提取（顶层数组 vs {"models":[...]}，容忍前置日志噪声）
//   - 各引擎 ListModels 的**命令构造 + 解析**（假 CLI，不碰真实 CLI）
//   - 无动态来源的引擎（arkclaw）返回 ErrNoModelSource
//
// 真实 CLI 的真实输出另有真机验收（README「已验证」段），此处只保证契约。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── 纯函数解析器 ─────────────────────────────────────────────

func TestParseCodebuddyHelpModels(t *testing.T) {
	// 真实 --help 的排版（--model 描述内含括号清单）。
	help := `Options:
  --model <model>                                  Model for the current session. Please provide the model ID. Currently supported: (auto, hy4-preview, hy3, glm-5.3, custom-local:MiniMax-M3, custom-local:gpt-6-astra)
  --text-to-image-model <model>                    Model for text-to-image generation
`
	want := []string{"auto", "hy4-preview", "hy3", "glm-5.3", "custom-local:MiniMax-M3", "custom-local:gpt-6-astra"}
	if got := parseCodebuddyHelpModels(help); !equalStrings(got, want) {
		t.Errorf("got %v want %v", got, want)
	}

	// 清单跨行（终端宽度换行）也要能截到配对右括号。
	wrapped := "Currently supported: (auto, hy3,\n                     glm-5.3)\nNext: x\n"
	if got := parseCodebuddyHelpModels(wrapped); !equalStrings(got, []string{"auto", "hy3", "glm-5.3"}) {
		t.Errorf("wrapped list: got %v", got)
	}

	// 没有标记 → nil（不能让调用方误以为"零个模型"）。
	if got := parseCodebuddyHelpModels("no such marker"); got != nil {
		t.Errorf("no marker: got %v want nil", got)
	}

	// 去重 + 去空。
	dup := "Currently supported: (auto, hy3, hy3, , auto)"
	if got := parseCodebuddyHelpModels(dup); !equalStrings(got, []string{"auto", "hy3"}) {
		t.Errorf("dedupe: got %v", got)
	}
}

func TestParseLLMModelsText(t *testing.T) {
	out := `OpenAI Chat: gpt-4o (aliases: 4o)
OpenAI Chat: gpt-4o-mini (aliases: 4o-mini)
OpenRouter: x-ai/grok-4

gpt-4o
`
	want := []string{"gpt-4o", "gpt-4o-mini", "x-ai/grok-4"}
	if got := parseLLMModelsText(out); !equalStrings(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestClaudeModelsFromSettings(t *testing.T) {
	// 真实 settings.json 的形态：每个档位一组**四个**变量，其中只有 `*_MODEL`（id 格）
	// 是模型；`*_MODEL_NAME` 是显示名。id 格分布在两个命名空间：
	// ANTHROPIC_DEFAULT_*_MODEL / ANTHROPIC_MODEL 与 CLAUDE_CODE_SUBAGENT_MODEL。
	// 回归保护（2026-09-28 用户报「claude 返回的模型列表不对」→「setting 中不是 5 个模型吗」）：
	//   ① 显示名（本机那份是 GLM-5.2 / kimi-k2.6 / deepseek-v4-pro）**不得**进清单；
	//   ② CLAUDE_CODE_SUBAGENT_MODEL 这个 id 格**不能漏**。
	// env 里还混有非字符串值（真实文件就有 1 这种整型），必须跳过而不是解析失败。
	data := []byte(`{
	  "model": "claude-sonnet-4-6",
	  "env": {
	    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-5[1M]",
	    "ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": "kimi-k2.6",
	    "ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION": "Kimi K2.6",
	    "ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES": "1m,vision",
	    "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5[1M]",
	    "ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME": "GLM-5.2",
	    "ANTHROPIC_MODEL": "claude-fable-5[1M]",
	    "CLAUDE_CODE_SUBAGENT_MODEL": "deepseek-v4-flash",
	    "ANTHROPIC_BASE_URL": "http://127.0.0.1:15721",
	    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": 1
	  }
	}`)
	// 顶层 model 先，其后 env 键按字母序 —— 共 5 个 id 格。
	want := []string{
		"claude-sonnet-4-6",
		"claude-opus-5[1M]",
		"claude-sonnet-5[1M]",
		"claude-fable-5[1M]",
		"deepseek-v4-flash",
	}

	got, err := claudeModelsFromSettings(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v（集合应完全一致）", got, want)
	}
	for _, m := range want {
		if !containsStr(got, m) {
			t.Errorf("missing %q in %v", m, got)
		}
	}

	// 显示名 / 描述 / 能力位 / 非模型 env 一律不得混入
	for _, bad := range []string{"kimi-k2.6", "GLM-5.2", "Kimi K2.6", "1m,vision", "http://127.0.0.1:15721", "1"} {
		if containsStr(got, bad) {
			t.Errorf("非模型 id 的值 %q 混进了清单: %v", bad, got)
		}
	}

	// 语法错 → 报错（不是静默空清单）。
	if _, err := claudeModelsFromSettings([]byte("{oops")); err == nil {
		t.Error("invalid JSON should error")
	}
}

func TestJSONModelsArray(t *testing.T) {
	// 顶层数组（trae）。
	arr := `[
	  {"name":"Doubao-Seed-Evolving","real_name":"Doubao-Seed-Evolving"},
	  {"name":"My-MiniMax-M3","real_name":"MiniMax-M3"}
	]`
	objs, err := jsonModelsArray(arr)
	if err != nil {
		t.Fatalf("array: %v", err)
	}
	if got := modelNamesFromObjects(objs, "name", "real_name"); !equalStrings(got, []string{"Doubao-Seed-Evolving", "My-MiniMax-M3"}) {
		t.Errorf("trae names: %v", got)
	}

	// 包裹对象（codex / openclaw）+ 前置日志噪声。
	wrapped := "[plugin] noise line\n" + `{"count":1,"models":[{"key":"minimax/MiniMax-M3","name":"MiniMax-M3"}]}`
	objs, err = jsonModelsArray(wrapped)
	if err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if got := modelNamesFromObjects(objs, "key", "name"); !equalStrings(got, []string{"minimax/MiniMax-M3"}) {
		t.Errorf("openclaw keys: %v", got)
	}

	if _, err := jsonModelsArray("no json at all"); err == nil {
		t.Error("no JSON should error")
	}
}

// ── 各引擎 ListModels（假 CLI）────────────────────────────────

func TestEngineListModelsWithFakeCLI(t *testing.T) {
	// 隔离 HOME：codebuddy-ai 的扩展来源会扫 ~/.codebuddy-ai 与 ~/.codebuddy 的
	// local_storage 缓存，不隔离会吃到真机缓存导致断言失败。
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		name    string
		cliName string
		canned  string
		wantArg string // 记进 args.log 的参数（顺序敏感）
		want    []string
		build   func(bin string) ModelLister
	}{
		{
			name:    "trae",
			cliName: "trae-cli",
			canned:  `echo '[{"name":"My-MiniMax-M3","real_name":"MiniMax-M3"},{"name":"GLM-5.3"}]'`,
			wantArg: "models --json",
			want:    []string{"My-MiniMax-M3", "GLM-5.3"},
			build:   func(bin string) ModelLister { return &TraeEngine{BinPath: bin} },
		},
		{
			name:    "codex",
			cliName: "codex",
			canned:  `echo '{"models":[{"slug":"MiniMax-M3","display_name":"MiniMax-M3"}]}'`,
			wantArg: "debug models",
			want:    []string{"MiniMax-M3"},
			build:   func(bin string) ModelLister { return &CodexEngine{BinPath: bin} },
		},
		{
			name:    "openclaw",
			cliName: "openclaw",
			canned:  `echo '{"count":2,"models":[{"key":"minimax/MiniMax-M3"},{"key":"papergames/deepseek-v4-pro"}]}'`,
			wantArg: "models list --json",
			want:    []string{"minimax/MiniMax-M3", "papergames/deepseek-v4-pro"},
			build:   func(bin string) ModelLister { return &OpenClawEngine{BinPath: bin} },
		},
		{
			name:    "llm",
			cliName: "llm",
			canned:  `printf '%s\n' 'OpenAI Chat: gpt-4o (aliases: 4o)' 'OpenRouter: x-ai/grok-4'`,
			wantArg: "models",
			want:    []string{"gpt-4o", "x-ai/grok-4"},
			build:   func(bin string) ModelLister { return &LLMEngine{BinPath: bin} },
		},
		{
			name:    "codebuddy",
			cliName: "codebuddy",
			canned:  `printf '%s\n' 'Options:' '  --model <model>  Model ID. Currently supported: (auto, hy3, custom-local:MiniMax-M3)'`,
			wantArg: "--help",
			want:    []string{"auto", "hy3", "custom-local:MiniMax-M3"},
			build:   func(bin string) ModelLister { return &CodeBuddyEngine{BinPath: bin} },
		},
		{
			// AI 后端的注册表是分层别名（国外模型网关侧映射），与 WorkBuddy 端完全不同；
			// listModels 必须经 extraEnv 隔离读到自己的清单（这里用假 CLI 验证解析面）。
			name:    "codebuddy-ai",
			cliName: "codebuddy-ai",
			canned:  `printf '%s\n' 'Options:' '  --model <model>  Model ID. Currently supported: (fast-model, balanced-model, primary-model, deep-model)'`,
			wantArg: "--help",
			want:    []string{"fast-model", "balanced-model", "primary-model", "deep-model"},
			build:   func(bin string) ModelLister { return &CodeBuddyAIEngine{BinPath: bin} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, log := argsCaptureCLI(t, t.TempDir(), tc.cliName, tc.canned)
			got, err := tc.build(w.bin).ListModels(context.Background())
			if err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			if !equalStrings(got, tc.want) {
				t.Errorf("models = %v want %v", got, tc.want)
			}
			data, _ := os.ReadFile(log)
			if args := strings.TrimSpace(string(data)); args != tc.wantArg {
				t.Errorf("args = %q want %q", args, tc.wantArg)
			}
		})
	}
}

// 探测失败（CLI 报错 / 输出里没有清单）必须报错而非返回空清单。
func TestEngineListModelsFailures(t *testing.T) {
	// 隔离 HOME 与配置目录：codebuddy 的清单现在先读扩展来源（acc 缓存 /
	// 远程配置缓存），不隔离会吃到真机的 ~/.workbuddy 缓存而"意外成功"。
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEBUDDY_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))

	// 三级来源全空、--help 里也没有 "Currently supported" → ErrNoModelSource 语义的错误。
	w, _ := argsCaptureCLI(t, t.TempDir(), "codebuddy", `echo 'no model list'`)
	if _, err := (&CodeBuddyEngine{BinPath: w.bin}).ListModels(context.Background()); err == nil {
		t.Error("codebuddy without list should error")
	}

	// CLI 非零退出 → 报错（stderr 摘要并入）。
	dir := t.TempDir()
	bin := filepath.Join(dir, "trae-cli")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (&TraeEngine{BinPath: bin}).ListModels(context.Background()); err == nil {
		t.Error("failing CLI should error")
	}

	// 找不到 CLI → 报错（不 panic）。
	if _, err := (&LLMEngine{BinPath: filepath.Join(dir, "nope")}).ListModels(context.Background()); err == nil {
		t.Error("missing CLI should error")
	}
}

// ── product.json 模型清单（codebuddy-ai 专用来源）────────────────

func TestParseCreditMultiplier(t *testing.T) {
	cases := []struct{ in, want string }{
		{"x2.20 credits", "2.20"},
		{"x0.77", "0.77"},
		{"x0.00", "0.00"},
		{"  X1.5 CREDITS ", "1.5"},
		{"", ""},        // 空
		{"credits", ""}, // 缺倍率
		{"abc", ""},     // 非数字
		{"x", ""},       // 只有前缀
	}
	for _, tc := range cases {
		if got := parseCreditMultiplier(tc.in); got != tc.want {
			t.Errorf("parseCreditMultiplier(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseProductJSONModels(t *testing.T) {
	// 真实 AI 端 product.json 的裁剪形态：条目带 name / credits 等无关字段。
	data := []byte(`{
	  "$schema": "x",
	  "models": [
	    {"id":"default-model","name":"Default","credits":"x2.20 credits"},
	    {"id":"gpt-5.5","name":"GPT-5.5","credits":"x3.31"},
	    {"id":"gemini-3.1-pro","name":"Gemini-3.1-Pro"},
	    {"id":"fast-model","name":"Fast","credits":"x0.34 credits"}
	  ]
	}`)
	want := []string{"default-model", "gpt-5.5", "gemini-3.1-pro", "fast-model"}
	if got := parseProductJSONModels(data); !equalStrings(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	// 积分倍率：规范化数字；无 credits 的模型不进表。
	wantCredits := map[string]string{"default-model": "2.20", "gpt-5.5": "3.31", "fast-model": "0.34"}
	if _, gotCredits := parseProductJSONCatalog(data); !equalMaps(gotCredits, wantCredits) {
		t.Errorf("credits = %v want %v", gotCredits, wantCredits)
	}

	// 缺 id / 空 id 的条目跳过；重复项去重。
	partial := []byte(`{"models":[{"id":"a"},{"name":"no-id"},{"id":"  "},{"id":"b"},{"id":"a"}]}`)
	if got := parseProductJSONModels(partial); !equalStrings(got, []string{"a", "b"}) {
		t.Errorf("partial: got %v", got)
	}

	// 坏 JSON / models 类型不对 / 无 models / 空清单 → nil（调用方据此回退 --help）。
	for _, bad := range []string{`not json`, `{"models":"nope"}`, `{}`, `{"models":[]}`} {
		if got := parseProductJSONModels([]byte(bad)); got != nil {
			t.Errorf("bad %q: got %v want nil", bad, got)
		}
	}
}

func TestProductJSONPath(t *testing.T) {
	cases := []struct{ bin, want string }{
		{
			// 独立安装（npm）布局：product.json 在包根（bin/ 的上一级）
			bin:  "/opt/npm-global/lib/node_modules/@tencent-ai/codebuddy-code/bin/codebuddy",
			want: "/opt/npm-global/lib/node_modules/@tencent-ai/codebuddy-code/product.json",
		},
		{bin: "/usr/local/bin/codebuddy", want: "/usr/local/product.json"},
		{bin: "", want: ""},
	}
	for _, tc := range cases {
		if got := productJSONPath(tc.bin); got != tc.want {
			t.Errorf("productJSONPath(%q) = %q want %q", tc.bin, got, tc.want)
		}
	}
}

// ── 远程配置缓存（codebuddy-ai 扩展来源之一）────────────────────

func TestReadRemoteConfigCacheModels(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local_storage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 正常条目：两个账号的清单取并集、去重、保序。
	write("entry_a.info", `[
	  {"userId":"u1","data":{"models":[{"id":"auto"},{"id":"deepseek-v4.1-flash"}]}},
	  {"userId":"u2","data":{"models":[{"id":"deepseek-v4.1-flash"},{"id":"hy3"}]}}
	]`)
	// 其他形态：对象 / 字符串数组（base64+gzip 大对象）/ 坏 JSON → 都跳过。
	write("entry_flags.info", `{"productFeatures":{"Billing":true}}`)
	write("entry_blob.info", `["H4sIAAAAAAAAE+y9e28kWXYf"]`)
	write("entry_broken.info", `not json`)
	write("ignore.txt", `[{"data":{"models":[{"id":"nope"}]}}]`)

	want := []string{"auto", "deepseek-v4.1-flash", "hy3"}
	if got := readRemoteConfigCacheModels(dir); !equalStrings(got, want) {
		t.Errorf("got %v want %v", got, want)
	}

	// 目录缺失 / 空目录 / 空参 → nil。
	if got := readRemoteConfigCacheModels(filepath.Join(t.TempDir(), "nope")); got != nil {
		t.Errorf("missing dir: got %v want nil", got)
	}
	empty := filepath.Join(t.TempDir(), "local_storage")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := readRemoteConfigCacheModels(empty); got != nil {
		t.Errorf("empty dir: got %v want nil", got)
	}
	if got := readRemoteConfigCacheModels(""); got != nil {
		t.Errorf("empty arg: got %v want nil", got)
	}
}

// ── 客户端合并配置缓存（codebuddy-ai 首选来源）──────────────────

// codebuddy-ai 首选 ~/.workbuddy-ai/cache/acc-product-config-v*.json（客户端模型
// 选择器同源）；多版本取 mtime 最新；缺失时回退「远程配置缓存 ∪ product.json」。
func TestCodeBuddyListModelsAccConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cacheDir := filepath.Join(os.Getenv("HOME"), ".workbuddy-ai", "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAcc := func(name, body string, modTime time.Time) {
		t.Helper()
		p := filepath.Join(cacheDir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, modTime, modTime); err != nil {
			t.Fatal(err)
		}
	}

	accV3 := `{"endpoint":"https://www.workbuddy.ai","models":[
	  {"id":"fast-model","credits":"x0.34 credits"},{"id":"deepseek-v4.1-flash","credits":"x0.00"},
	  {"id":"gpt-5.5","credits":"x3.31"},{"id":"gpt-6-astra","credits":"x6.67"},{"id":"custom-local:MiniMax-M3"}
	]}`
	// 旧版本的清单不同，用于验证「取 mtime 最新」。
	accV2 := `{"models":[{"id":"stale-model","credits":"x1.11"}]}`

	// 客户端选择器清单（agents[].models）：acc 清单按它过滤，只留客户端展示的
	// 具名模型 + custom-local:*。stale-model 也列进来，供 B) 用。
	writeSelectorList(t, ".codebuddy-ai", "fast-model", "deepseek-v4.1-flash", "gpt-5.5", "gpt-6-astra", "stale-model")

	// A) acc 缓存命中 → 精确返回客户端清单（哪怕远程配置缓存 / product.json
	//    里有别的模型也不并入）；ModelCredits 给出规范化倍率，custom-local 不进表。
	writeAcc("acc-product-config-v2.json", accV2, time.Now().Add(-time.Hour))
	writeAcc("acc-product-config-v3.json", accV3, time.Now())
	bin, argsLog := appPkgCLI(t, "", `{"models":[{"id":"gpt-5.5"}]}`)
	ai := &CodeBuddyAIEngine{BinPath: bin}
	got, err := ai.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"fast-model", "deepseek-v4.1-flash", "gpt-5.5", "gpt-6-astra", "custom-local:MiniMax-M3"}
	if !equalStrings(got, want) {
		t.Errorf("acc got %v want %v", got, want)
	}
	wantCredits := map[string]string{
		"fast-model": "0.34", "deepseek-v4.1-flash": "0.00", "gpt-5.5": "3.31", "gpt-6-astra": "6.67",
	}
	if gotC := ai.ModelCredits(context.Background()); !equalMaps(gotC, wantCredits) {
		t.Errorf("ModelCredits = %v want %v", gotC, wantCredits)
	}
	if _, err := os.Stat(argsLog); err == nil {
		t.Error("acc 缓存命中时不应启动 CLI 取 --help")
	}

	// B) 只有旧版本 → 一样生效（glob 不写死版本号）。
	if err := os.Remove(filepath.Join(cacheDir, "acc-product-config-v3.json")); err != nil {
		t.Fatal(err)
	}
	got, err = ai.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels(v2): %v", err)
	}
	if !equalStrings(got, []string{"stale-model"}) {
		t.Errorf("v2-only got %v", got)
	}

	// C) acc 缓存按**客户端选择器清单**过滤：清单外的条目（补全 codewise-* /
	//    图像 hunyuan-image-* / 历史别名 default-1.* …）不进清单 —— 全量注册表
	//    铺给 -m 会比客户端多太多。过滤后一条不剩按「无来源」处理，回退下一级
	//    来源（这里是 product.json）。
	accNoise := `{"models":[{"id":"codewise-completions"},{"id":"hunyuan-image-alpha"},
	  {"id":"default-1.1"},{"id":"kimi-k2-instruct-taiji"}]}`
	writeAcc("acc-product-config-v3.json", accNoise, time.Now())
	got, err = ai.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels(noise): %v", err)
	}
	if !equalStrings(got, []string{"gpt-5.5"}) {
		t.Errorf("清单外的补全/图像/别名应被剔除，实际 got %v", got)
	}

	// D) 混合：选择器内的具名模型 + custom-local 保留，清单外的剔除，倍率表同步收窄。
	writeSelectorList(t, ".codebuddy-ai", "hy3", "glm-5.3")
	accMixed := `{"models":[{"id":"hy3","credits":"x0.00"},{"id":"codewise-jump"},
	  {"id":"custom-local:MiniMax-M3"},{"id":"glm-5.3","credits":"x0.79"}]}`
	writeAcc("acc-product-config-v3.json", accMixed, time.Now())
	got, err = ai.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels(mixed): %v", err)
	}
	if !equalStrings(got, []string{"hy3", "custom-local:MiniMax-M3", "glm-5.3"}) {
		t.Errorf("mixed got %v", got)
	}
	if gotC := ai.ModelCredits(context.Background()); !equalMaps(gotC, map[string]string{"hy3": "0.00", "glm-5.3": "0.79"}) {
		t.Errorf("mixed ModelCredits = %v", gotC)
	}
}

// codebuddy 两端的清单链：acc 缓存 → 远程配置缓存 ∪ product.json → 回退 --help。
// 2026-09-28 起 codebuddy（WorkBuddy 端）也启用扩展链，故 D 段与 AI 端同构。
func TestCodeBuddyListModelsProductJSON(t *testing.T) {
	// 隔离 HOME：扩展来源扫 ~/.codebuddy-ai 与 ~/.codebuddy 的缓存，不隔离会吃到真机缓存。
	t.Setenv("HOME", t.TempDir())

	const aiHelp = `Options:
  --model <model>  Model ID. Currently supported: (fast-model, balanced-model, primary-model, deep-model)
`
	const aiProduct = `{"models":[{"id":"gpt-5.5","credits":"x3.31 credits"},{"id":"gemini-3.1-pro"},{"id":"deepseek-v3-2-volc"},{"id":"fast-model"}]}`
	cacheBody := `[
	  {"userId":"u1","data":{"models":[{"id":"deepseek-v4.1-flash","credits":"x0.03"},{"id":"auto"},{"id":"hy3"}]}},
	  {"userId":"u2","data":{"models":[{"id":"deepseek-v4.1-flash"},{"id":"fast-model","credits":"x0.34 credits"}]}}
	]`
	sharedCache := filepath.Join(os.Getenv("HOME"), ".codebuddy", "local_storage")
	writeCache := func(dir, body string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "entry_cache.info"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A) 缓存 + product.json → 并集（缓存序在前、去重），且**不启动 CLI**。
	//    倍率同链合并：缓存优先（deepseek-v4.1-flash 取 u1 的 x0.03），product.json 补 gpt-5.5。
	writeCache(sharedCache, cacheBody)
	bin, argsLog := appPkgCLI(t, aiHelp, aiProduct)
	ai := &CodeBuddyAIEngine{BinPath: bin}
	got, err := ai.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"deepseek-v4.1-flash", "auto", "hy3", "fast-model", "gpt-5.5", "gemini-3.1-pro", "deepseek-v3-2-volc"}
	if !equalStrings(got, want) {
		t.Errorf("ai got %v want %v", got, want)
	}
	wantCredits := map[string]string{"deepseek-v4.1-flash": "0.03", "fast-model": "0.34", "gpt-5.5": "3.31"}
	if gotC := ai.ModelCredits(context.Background()); !equalMaps(gotC, wantCredits) {
		t.Errorf("ModelCredits = %v want %v", gotC, wantCredits)
	}
	if _, err := os.Stat(argsLog); err == nil {
		t.Error("扩展来源命中时不应启动 CLI 取 --help")
	}

	// B) 无缓存 + product.json → 仅 product.json，仍不启动 CLI。
	if err := os.RemoveAll(sharedCache); err != nil {
		t.Fatal(err)
	}
	got, err = ai.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels(product): %v", err)
	}
	if !equalStrings(got, []string{"gpt-5.5", "gemini-3.1-pro", "deepseek-v3-2-volc", "fast-model"}) {
		t.Errorf("product-only got %v", got)
	}
	if gotC := ai.ModelCredits(context.Background()); !equalMaps(gotC, map[string]string{"gpt-5.5": "3.31"}) {
		t.Errorf("product-only ModelCredits = %v", gotC)
	}

	// C) 两者皆无 → 回退 --help 的四个分层别名。
	bin3, _ := appPkgCLI(t, aiHelp, "")
	got, err = (&CodeBuddyAIEngine{BinPath: bin3}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels(fallback): %v", err)
	}
	if !equalStrings(got, []string{"fast-model", "balanced-model", "primary-model", "deep-model"}) {
		t.Errorf("fallback got %v", got)
	}

	// D) WorkBuddy 端（2026-09-28 起与 AI 端同链）：先 acc 缓存（客户端同源），
	//    按选择器清单过滤，命中即独占、不并入 ②、也不启动 CLI —— 官方预制模型
	//    （hy3 / deepseek-v4.1-flash）只能从这里来，--help 的清单里并没有。
	wbAccBody := `{"models":[
	  {"id":"hy3","credits":"x0.00"},{"id":"hy4-preview","credits":"x0.29"},
	  {"id":"deepseek-v4.1-flash","credits":"x0.11"},{"id":"codewise-jump"},
	  {"id":"custom-local:MiniMax-M3.1-Flash-Preview"}
	]}`
	wbAccDir := filepath.Join(os.Getenv("HOME"), ".workbuddy", "cache")
	if err := os.MkdirAll(wbAccDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wbAccDir, "acc-product-config-v3.json"), []byte(wbAccBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// 选择器清单里没有 codewise-jump（行内补全）→ 应被剔除。
	writeSelectorList(t, ".workbuddy", "hy3", "hy4-preview", "deepseek-v4.1-flash")
	const wbHelp = `Options:
  --model <model>  Model ID. Currently supported: (auto, hy3, glm-5.3)
`
	binWB, wbArgsLog := appPkgCLI(t, wbHelp, aiProduct)
	cbWB := &CodeBuddyEngine{BinPath: binWB}
	got, err = cbWB.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels(workbuddy acc): %v", err)
	}
	wantWB := []string{"hy3", "hy4-preview", "deepseek-v4.1-flash", "custom-local:MiniMax-M3.1-Flash-Preview"}
	if !equalStrings(got, wantWB) {
		t.Errorf("workbuddy acc got %v want %v", got, wantWB)
	}
	// 倍率与清单同源同链：acc 里的 custom-local 无倍率则不进表，官方四条都在。
	if gotC := cbWB.ModelCredits(context.Background()); !equalMaps(gotC, map[string]string{
		"hy3": "0.00", "hy4-preview": "0.29", "deepseek-v4.1-flash": "0.11",
	}) {
		t.Errorf("workbuddy ModelCredits = %v", gotC)
	}
	if _, err := os.Stat(wbArgsLog); err == nil {
		t.Error("workbuddy 命中 acc 缓存时不应启动 CLI 取 --help")
	}

	// D2) workbuddy 的 acc 缺席 → 回退 ② 远程配置缓存 ∪ product.json（仍不启 CLI）。
	//    sharedCache 在 B) 已被删掉，这里先写回来（② 的远程配置缓存那一路）。
	if err := os.RemoveAll(wbAccDir); err != nil {
		t.Fatal(err)
	}
	writeCache(sharedCache, cacheBody)
	got, err = cbWB.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels(workbuddy ②): %v", err)
	}
	if !equalStrings(got, []string{"deepseek-v4.1-flash", "auto", "hy3", "fast-model", "gpt-5.5", "gemini-3.1-pro", "deepseek-v3-2-volc"}) {
		t.Errorf("workbuddy ② got %v", got)
	}

	// D3) acc 与 ② 都没有 → 末级兜底 --help（此时才启动 CLI）。
	binWB3, _ := appPkgCLI(t, wbHelp, "")
	if err := os.RemoveAll(sharedCache); err != nil {
		t.Fatal(err)
	}
	got, err = (&CodeBuddyEngine{BinPath: binWB3}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels(workbuddy --help): %v", err)
	}
	if !equalStrings(got, []string{"auto", "hy3", "glm-5.3"}) {
		t.Errorf("workbuddy --help got %v", got)
	}

	// E) 用户显式设置 CODEBUDDY_CONFIG_DIR：只信该目录，共享缓存不兜底。
	t.Setenv("CODEBUDDY_CONFIG_DIR", t.TempDir())
	bin5, _ := appPkgCLI(t, aiHelp, "")
	got, err = (&CodeBuddyAIEngine{BinPath: bin5}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels(custom dir): %v", err)
	}
	if !equalStrings(got, []string{"fast-model", "balanced-model", "primary-model", "deep-model"}) {
		t.Errorf("custom-dir got %v", got)
	}
	t.Setenv("CODEBUDDY_CONFIG_DIR", "")
}

// codebuddy（非 ai）的清单 / 倍率链：acc 缓存（~/.workbuddy）→ 远程配置缓存 ∪
// product.json → --help。**两端同链**（2026-09-28 起 codebuddy 也启用扩展清单链：
// 此前它只按 --help 出清单、倍率却按 acc 缓存取，同一引擎两个字段读自两个文件）。
// 同时验证清单与倍率全程不启动 CLI（acc 命中时只读缓存文件）。
func TestCodeBuddyModelCredits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEBUDDY_CONFIG_DIR", "")

	const help = `Options:
  --model <model>  Model ID. Currently supported: (hy3, fast-model, deep-model)
`
	// product.json 给 hy3/fast-model 两条倍率：acc 命中时不得混入（7.77/1.23）；
	// 兜底链里它们分别被缓存值顶掉 / 并入。
	product := `{"models":[{"id":"hy3","credits":"x7.77"},{"id":"fast-model","credits":"x1.23"}]}`
	accBody := `{"models":[
	  {"id":"hy3","credits":"x0.00"},{"id":"fast-model","credits":"x0.34"},
	  {"id":"balanced-model","credits":"x0.65"},{"id":"only-in-acc","credits":"x1.00"}
	]}`
	cacheBody := `[{"userId":"u1","data":{"models":[{"id":"hy3","credits":"x9.99"}]}}]`
	accDir := filepath.Join(os.Getenv("HOME"), ".workbuddy", "cache")
	sharedCache := filepath.Join(os.Getenv("HOME"), ".codebuddy", "local_storage")
	writeFile := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A) acc 缓存命中 → 清单与倍率**同源**（2026-09-28 起两端一致，都按 acc）：
	//    倍率只来自 acc（hy3=0.00，而非缓存 9.99 / product 7.77）；acc 独占、
	//    不并入 ②，且不启动 CLI。清单按选择器清单过滤：only-in-acc 不在客户端
	//    选择器里 → 剔除（这正是「比客户端多太多」的根因）。
	writeFile(filepath.Join(accDir, "acc-product-config-v3.json"), accBody)
	writeFile(filepath.Join(sharedCache, "entry_cache.info"), cacheBody)
	writeSelectorList(t, ".workbuddy", "hy3", "fast-model", "balanced-model")
	bin, argsLog := appPkgCLI(t, help, product)
	cb := &CodeBuddyEngine{BinPath: bin}
	got, err := cb.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if !equalStrings(got, []string{"hy3", "fast-model", "balanced-model"}) {
		t.Errorf("清单应按 acc 缓存 ∩ 选择器清单，got %v", got)
	}
	wantA := map[string]string{"hy3": "0.00", "fast-model": "0.34", "balanced-model": "0.65"}
	if gotC := cb.ModelCredits(context.Background()); !equalMaps(gotC, wantA) {
		t.Errorf("ModelCredits = %v want %v", gotC, wantA)
	}
	// 清单与倍率都不应启动 CLI（acc 命中，压根用不到 --help）。
	if _, err := os.Stat(argsLog); err == nil {
		t.Error("acc 命中时不应启动 CLI")
	}

	// B) acc 缺席 → 远程配置缓存 ∪ product.json 合并（缓存优先：hy3 取 9.99）。
	//    选择器目录也删掉：避免 A 段的选择器清单继续命中（acc 已删，readAccCatalog
	//    应直接回退 ②，否则选择器变成 ①→② 之间的伪来源）。
	if err := os.RemoveAll(accDir); err != nil {
		t.Fatal(err)
	}
	selectorDir := filepath.Join(os.Getenv("HOME"), ".workbuddy", "local_storage")
	if err := os.RemoveAll(selectorDir); err != nil {
		t.Fatal(err)
	}
	wantB := map[string]string{"hy3": "9.99", "fast-model": "1.23"}
	if gotC := cb.ModelCredits(context.Background()); !equalMaps(gotC, wantB) {
		t.Errorf("ModelCredits(fallback) = %v want %v", gotC, wantB)
	}

	// C) 缓存也没有 → 仅 product.json。
	if err := os.RemoveAll(sharedCache); err != nil {
		t.Fatal(err)
	}
	wantC := map[string]string{"hy3": "7.77", "fast-model": "1.23"}
	if gotC := cb.ModelCredits(context.Background()); !equalMaps(gotC, wantC) {
		t.Errorf("ModelCredits(product) = %v want %v", gotC, wantC)
	}

	// D) 什么都没有 → nil（CLI 端省略 model_credits）。
	bin2, _ := appPkgCLI(t, help, "")
	if gotC := (&CodeBuddyEngine{BinPath: bin2}).ModelCredits(context.Background()); gotC != nil {
		t.Errorf("ModelCredits(empty) = %v want nil", gotC)
	}
}

// writeSelectorList 在 <home>/<appHome>/local_storage/entry_selector.info 写下客户端
// 模型选择器清单（agents[].models，acc 清单按它过滤 —— 见 agentSelectorModels）。
// appHome 传 ".workbuddy" 或 ".workbuddy-ai"。
func writeSelectorList(t *testing.T, appHome string, models ...string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), appHome, "local_storage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ids, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	body := `[{"userId":"u1","data":{"agents":[{"modelTags":["craft"],"models":` + string(ids) + `}]}}]`
	if err := os.WriteFile(filepath.Join(dir, "entry_selector.info"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// appPkgCLI 搭出 App 包目录结构（<tmp>/cli/bin/<name> + <tmp>/cli/product.json），
// 用于验证 productJSONPath 的上溯推导。productJSON 为空则不放 product.json。
func appPkgCLI(t *testing.T, helpOut, productJSON string) (bin, argsLog string) {
	t.Helper()
	cliDir := filepath.Join(t.TempDir(), "cli")
	binDir := filepath.Join(cliDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	argsLog = filepath.Join(cliDir, "args.log")
	bin = filepath.Join(binDir, "codebuddy")
	// --help 文案是多行文本，用 heredoc 原样输出（直接拼接会被 shell 当命令执行）。
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + argsLog + "\n" +
		"cat <<'HELPEOF'\n" + strings.TrimRight(helpOut, "\n") + "\nHELPEOF\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if productJSON != "" {
		if err := os.WriteFile(filepath.Join(cliDir, "product.json"), []byte(productJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return bin, argsLog
}

// claude 的清单来自 settings.json：HOME 下 .claude/settings.json，
// CLAUDE_CONFIG_DIR 优先；缺失时是 ErrNoModelSource（不是硬编码兜底）。
func TestClaudeListModelsFromConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"model":"","env":{"ANTHROPIC_DEFAULT_SONNET_MODEL":"claude-sonnet-5[1M]"}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := (&ClaudeEngine{}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if !equalStrings(got, []string{"claude-sonnet-5[1M]"}) {
		t.Errorf("got %v", got)
	}

	// CLAUDE_CONFIG_DIR 覆盖目录。
	alt := t.TempDir()
	if err := os.WriteFile(filepath.Join(alt, "settings.json"),
		[]byte(`{"model":"claude-opus-5[1M]"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", alt)
	got, err = (&ClaudeEngine{}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels(alt): %v", err)
	}
	if !equalStrings(got, []string{"claude-opus-5[1M]"}) {
		t.Errorf("alt got %v", got)
	}

	// 文件不存在 → ErrNoModelSource。
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := (&ClaudeEngine{}).ListModels(context.Background()); !errors.Is(err, ErrNoModelSource) {
		t.Errorf("missing settings: want ErrNoModelSource, got %v", err)
	}
}

// 无动态来源 / 清单为空的引擎：返回 ErrNoModelSource 包裹的错误（附原因）。
func TestArkClawListModelsNoSource(t *testing.T) {
	_, err := (&ArkClawEngine{}).ListModels(context.Background())
	if !errors.Is(err, ErrNoModelSource) {
		t.Fatalf("want ErrNoModelSource, got %v", err)
	}
	if !strings.Contains(err.Error(), "claw_id") {
		t.Errorf("reason should mention claw_id: %v", err)
	}
}

// 全部内置引擎都实现了 ModelLister（否则 --engines 会缺 models 字段）；
// ModelListerOf 对未实现者返回 nil。
func TestAllEnginesImplementModelLister(t *testing.T) {
	for _, e := range Engines() {
		if ModelListerOf(e) == nil {
			t.Errorf("engine %q does not implement ModelLister", e.Name())
		}
	}
	if ModelListerOf(&stringEngineStub{}) != nil {
		t.Error("ModelListerOf should return nil for non-lister engines")
	}
}

// stringEngineStub 未实现 ModelLister 的最小引擎。
type stringEngineStub struct{}

func (s *stringEngineStub) Name() string           { return "stub" }
func (s *stringEngineStub) Detect() (bool, string) { return true, "" }
func (s *stringEngineStub) Complete(context.Context, Request) (Response, error) {
	return Response{}, nil
}

// ── 辅助 ────────────────────────────────────────────────────

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
