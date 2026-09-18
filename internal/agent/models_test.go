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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	// env 里混有非字符串值（真实 settings.json 就有 1 这种整型），必须跳过而不是解析失败。
	data := []byte(`{
	  "model": "claude-sonnet-4-6",
	  "env": {
	    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-5[1M]",
	    "ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": "MiniMax-M3",
	    "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5[1M]",
	    "ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME": "MiniMax-M2.7-highspeed",
	    "ANTHROPIC_BASE_URL": "http://127.0.0.1:15721",
	    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": 1
	  }
	}`)
	want := []string{"claude-sonnet-4-6", "MiniMax-M3", "MiniMax-M2.7-highspeed"}

	// 顺序 = 键名排序（顶层 model 先），断言集合足够：ANTHROPIC_* 键按字母序
	// 取到的值与 -m 能收的标识一一对应。
	got, err := claudeModelsFromSettings(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, m := range append(want, "claude-sonnet-5[1M]", "claude-opus-5[1M]") {
		if !containsStr(got, m) {
			t.Errorf("missing %q in %v", m, got)
		}
	}
	if containsStr(got, "http://127.0.0.1:15721") || containsStr(got, "1") {
		t.Errorf("non-model env leaked into list: %v", got)
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
	// --help 里没有 "Currently supported" → ErrNoModelSource 语义的错误。
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

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
