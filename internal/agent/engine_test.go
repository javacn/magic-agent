package agent

// engine_test.go - 引擎层单元测试。
//
// 用 fake CLI 脚本（写入临时目录的 shell 脚本）模拟三家 CLI 的行为，
// 验证：
//	- 参数构造（--model、--tools ""、--output-format、-c model.name、
//	  --query-timeout 映射）
//	- 输出解析（claude JSON envelope、codebuddy 三种 envelope、
//	  user_query 回显剥离、trae 纯文本）
//	- FlattenPrompt 扁平化逻辑
//	- 引擎注册表 / Lookup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFakeCLI 写一个假 CLI 脚本并返回路径。脚本内容直接是 shell 代码。
func writeFakeCLI(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// ── FlattenPrompt ─────────────────────────────────────────────

func TestFlattenPrompt(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "你好！"},
	}

	got := FlattenPrompt("你是助手", msgs, false)
	if !strings.Contains(got, "【系统指令】\n你是助手") {
		t.Errorf("system prompt missing: %q", got)
	}
	if !strings.Contains(got, "【用户】\n你好") {
		t.Errorf("user turn missing: %q", got)
	}
	if !strings.Contains(got, "【助手】\n你好！") {
		t.Errorf("assistant turn missing: %q", got)
	}
	// 末尾 assistant → 自动补「请继续」
	if !strings.HasSuffix(got, "【用户】\n请继续。") {
		t.Errorf("continue suffix missing: %q", got)
	}

	// noToolSuffix
	got2 := FlattenPrompt("s", nil, true)
	if !strings.Contains(got2, "[约束]") {
		t.Errorf("noToolSuffix missing: %q", got2)
	}

	// 空
	if got3 := FlattenPrompt("", nil, false); got3 != "" {
		t.Errorf("empty input should give empty prompt, got %q", got3)
	}
}

// ── 注册表 ────────────────────────────────────────────────────

func TestLookup(t *testing.T) {
	for _, name := range []string{"claude", "codebuddy", "trae"} {
		e := Lookup(name)
		if e == nil {
			t.Fatalf("Lookup(%q) = nil", name)
		}
		if e.Name() != name {
			t.Errorf("Name() = %q want %q", e.Name(), name)
		}
	}
	// 大小写不敏感
	if Lookup("CLAUDE") == nil {
		t.Error("Lookup should be case-insensitive")
	}
	if Lookup("nope") != nil {
		t.Error("Lookup(nope) should be nil")
	}
	if Lookup("") != nil {
		t.Error("Lookup(\"\") should be nil")
	}
}

// ── Claude 引擎 ───────────────────────────────────────────────

func TestClaudeCompleteParsesEnvelope(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
# 校验关键 flag
for a in "$@"; do
  case "$a" in
    -p|--print|--no-session-persistence) ;;
  esac
done
echo '{"type":"result","subtype":"success","is_error":false,"result":"答案是2","session_id":"s-123","model":"claude-sonnet-4-6"}'
`)
	e := &ClaudeEngine{BinPath: cli}
	if ok, _ := e.Detect(); !ok {
		t.Fatal("Detect should succeed with fake CLI")
	}
	resp, err := e.Complete(context.Background(), Request{
		SystemPrompt: "你是一个计算器",
		Messages:     []Message{{Role: "user", Content: "1+1=?"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "答案是2" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.SessionID != "s-123" {
		t.Errorf("SessionID = %q", resp.SessionID)
	}
	if resp.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q", resp.Model)
	}
}

func TestClaudePassesModelFlag(t *testing.T) {
	var captured string
	_ = captured
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + log + "\necho '{\"type\":\"result\",\"result\":\"ok\"}'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &ClaudeEngine{BinPath: cli}
	_, err := e.Complete(context.Background(), Request{
		Model:    "claude/sonnet",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	if !strings.Contains(args, "--model sonnet") {
		t.Errorf("--model sonnet not passed: %q", args)
	}
	if !strings.Contains(args, "--tools ") {
		t.Errorf("--tools not passed: %q", args)
	}
	if !strings.Contains(args, "--output-format json") {
		t.Errorf("--output-format json not passed: %q", args)
	}
}

func TestClaudeErrorEnvelope(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"result","subtype":"error_max_turns","is_error":true,"result":"超出轮次"}'
`)
	e := &ClaudeEngine{BinPath: cli}
	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for is_error=true")
	}
	if !strings.Contains(err.Error(), "error_max_turns") {
		t.Errorf("error should mention subtype: %v", err)
	}
}

func TestClaudeFallbackToRawText(t *testing.T) {
	cli := writeFakeCLI(t, "claude", "#!/bin/sh\necho 'plain text answer'\n")
	e := &ClaudeEngine{BinPath: cli}
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "plain text answer" {
		t.Errorf("Text = %q", resp.Text)
	}
}

func TestClaudeNotFound(t *testing.T) {
	e := &ClaudeEngine{BinPath: "/nonexistent/claude"}
	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for missing CLI")
	}
}

// ── CodeBuddy 引擎 ────────────────────────────────────────────

func TestCodeBuddySingleEnvelope(t *testing.T) {
	cli := writeFakeCLI(t, "codebuddy", `#!/bin/sh
echo '{"type":"result","is_error":false,"result":"你好，世界"}'
`)
	e := &CodeBuddyEngine{BinPath: cli}
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "打个招呼"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "你好，世界" {
		t.Errorf("Text = %q", resp.Text)
	}
}

func TestCodeBuddyArrayEnvelope(t *testing.T) {
	cli := writeFakeCLI(t, "codebuddy", `#!/bin/sh
echo '[{"type":"system","subtype":"init"},{"type":"assistant","message":{"content":[{"type":"text","text":"部分1"}]}},{"type":"result","result":"最终答案"}]'
`)
	e := &CodeBuddyEngine{BinPath: cli}
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "最终答案" {
		t.Errorf("Text = %q want 最终答案", resp.Text)
	}
}

func TestCodeBuddyArrayAssistantFallback(t *testing.T) {
	cli := writeFakeCLI(t, "codebuddy", `#!/bin/sh
echo '[{"type":"assistant","message":{"content":[{"type":"text","text":"第一段"}]}},{"role":"assistant","content":[{"type":"output_text","text":"第二段"}]}]'
`)
	e := &CodeBuddyEngine{BinPath: cli}
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "第一段\n第二段" {
		t.Errorf("Text = %q want 两段拼接", resp.Text)
	}
}

func TestCodeBuddyUserQueryEcho(t *testing.T) {
	cli := writeFakeCLI(t, "codebuddy", `#!/bin/sh
echo '<user_query>【系统指令】你是助手</user_query>真正的回答'
`)
	e := &CodeBuddyEngine{BinPath: cli}
	resp, err := e.Complete(context.Background(), Request{
		SystemPrompt: "你是助手",
		Messages:     []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "真正的回答" {
		t.Errorf("Text = %q want 回显被剥离", resp.Text)
	}

	// 全回显（无闭标签后正文）→ 报错
	cli2 := writeFakeCLI(t, "codebuddy", "#!/bin/sh\necho '<user_query>只有回显</user_query>'\n")
	e2 := &CodeBuddyEngine{BinPath: cli2}
	if _, err := e2.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Error("expected error for echo-only output")
	}
}

func TestCodeBuddyPassesModel(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, "codebuddy")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + log + "\necho '{\"type\":\"result\",\"result\":\"ok\"}'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &CodeBuddyEngine{BinPath: cli}
	_, err := e.Complete(context.Background(), Request{
		Model:    "codebuddy/hy3",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	if !strings.Contains(args, "--model hy3") {
		t.Errorf("--model hy3 not passed: %q", args)
	}
	if !strings.Contains(args, "--append-system-prompt") {
		t.Errorf("--append-system-prompt not passed: %q", args)
	}
}

// ── Trae 引擎 ─────────────────────────────────────────────────

func TestTraeCompletePlainText(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, "trae-cli")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + log + "\necho 'trae 的回答'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &TraeEngine{BinPath: cli}
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Timeout:  90 * time.Second,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "trae 的回答" {
		t.Errorf("Text = %q", resp.Text)
	}

	data, _ := os.ReadFile(log)
	args := string(data)
	// 默认模型：不传 -c model.name
	if strings.Contains(args, "model.name") {
		t.Errorf("default model should not pass -c model.name: %q", args)
	}
	// 超时映射到 --query-timeout
	if !strings.Contains(args, "--query-timeout 1m30s") {
		t.Errorf("--query-timeout not mapped: %q", args)
	}
	// 工具禁用
	if !strings.Contains(args, "--disallowed-tool Bash") {
		t.Errorf("--disallowed-tool Bash missing: %q", args)
	}
	// system prompt 带 noToolSuffix
	if !strings.Contains(args, "[约束]") {
		t.Errorf("noToolSuffix missing in prompt: %q", args)
	}
}

func TestTraeExplicitModel(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, "trae-cli")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + log + "\necho 'ok'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &TraeEngine{BinPath: cli}
	resp, err := e.Complete(context.Background(), Request{
		Model:    "My-MiniMax-M3",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Model != "My-MiniMax-M3" {
		t.Errorf("Model = %q", resp.Model)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	if !strings.Contains(args, "-c model.name=My-MiniMax-M3") {
		t.Errorf("-c model.name not passed: %q", args)
	}
}

func TestTraeTimeoutCappedAt600s(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, "trae-cli")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + log + "\necho 'ok'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &TraeEngine{BinPath: cli}
	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Timeout:  30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "--query-timeout 10m0s") {
		t.Errorf("query-timeout should cap at 600s: %q", string(data))
	}
}

// ── TraeDefaultModel 配置探测 ─────────────────────────────────

func TestTraeDefaultModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// 无配置文件 → 空串
	if got := TraeDefaultModel(); got != "" {
		t.Errorf("no config: got %q want empty", got)
	}

	// 有配置 → 读 model.name
	cfgDir := filepath.Join(home, ".trae")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "model:\n    name: My-MiniMax-M3\nmodels:\n-  name: \"My-MiniMax-M3\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "trae_cli.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := TraeDefaultModel(); got != "My-MiniMax-M3" {
		t.Errorf("got %q want My-MiniMax-M3", got)
	}
}
