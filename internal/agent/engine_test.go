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
	for _, name := range []string{"claude", "codebuddy", "codebuddy-ai", "trae"} {
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

// 未指定模型时 codebuddy 引擎默认 --model hy3；显式指定时用显式值。
func TestCodeBuddyDefaultModel(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, "codebuddy")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + log + "\necho '{\"type\":\"result\",\"result\":\"ok\"}'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &CodeBuddyEngine{BinPath: cli}

	// 默认：hy3
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "--model hy3") {
		t.Errorf("default model hy3 not passed: %q", string(data))
	}

	// 显式覆盖：glm-5.3
	if err := os.WriteFile(log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Complete(context.Background(), Request{
		Model:    "glm-5.3",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ = os.ReadFile(log)
	if !strings.Contains(string(data), "--model glm-5.3") {
		t.Errorf("explicit model not passed: %q", string(data))
	}
	if strings.Count(string(data), "--model") != 1 {
		t.Errorf("model flag should appear exactly once: %q", string(data))
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

// ── 会话续接（SessionID → --resume 映射）──────────────────────

// argsCaptureCLI 写一个把 "$*" 逐行记入 args.log 的假 CLI，返回 (引擎, log 路径)。
func argsCaptureCLI(t *testing.T, dir, name, cannedOutput string) (*engineWithLog, string) {
	t.Helper()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, name)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + log + "\n" + cannedOutput + "\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &engineWithLog{log: log, bin: cli}, log
}

type engineWithLog struct {
	log string
	bin string
}

// claudeArgs 构造带 args.log 的 ClaudeEngine。
func (w engineWithLog) claude() *ClaudeEngine { return &ClaudeEngine{BinPath: w.bin} }

// codebuddyArgs 构造带 args.log 的 CodeBuddyEngine。
func (w engineWithLog) codebuddy() *CodeBuddyEngine { return &CodeBuddyEngine{BinPath: w.bin} }

// traeArgs 构造带 args.log 的 TraeEngine。
func (w engineWithLog) trae() *TraeEngine { return &TraeEngine{BinPath: w.bin} }

func TestClaudeResumeSession(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "claude", `echo '{"type":"result","result":"续接成功","session_id":"s-abc"}'`)
	e := w.claude()
	resp, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		SessionID: "s-abc",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.SessionID != "s-abc" {
		t.Errorf("Response.SessionID = %q want s-abc", resp.SessionID)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	if !strings.Contains(args, "--resume s-abc") {
		t.Errorf("--resume s-abc not passed: %q", args)
	}
	if strings.Contains(args, "--no-session-persistence") {
		t.Errorf("--resume 与 --no-session-persistence 互斥，不应同时出现: %q", args)
	}
}

func TestClaudeNewSessionDefault(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "claude", `echo '{"type":"result","result":"ok"}'`)
	e := w.claude()
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	if strings.Contains(args, "--resume") {
		t.Errorf("默认新会话不应带 --resume: %q", args)
	}
	// 新会话必须可落盘：--no-session-persistence 的会话 "cannot be resumed"，
	// 传了会导致首轮 session_id 无法用于续接。
	if strings.Contains(args, "--no-session-persistence") {
		t.Errorf("默认新会话不应带 --no-session-persistence（会无法续接）: %q", args)
	}
}

func TestCodeBuddyResumeSession(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codebuddy", `echo '{"type":"result","result":"ok"}'`)
	e := w.codebuddy()
	if _, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		SessionID: "cb-77",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	if !strings.Contains(args, "--resume cb-77") {
		t.Errorf("--resume cb-77 not passed: %q", args)
	}
	if strings.Contains(args, "--no-session-persistence") {
		t.Errorf("两个 flag 互斥，不应同时出现: %q", args)
	}
}

// extractCodeBuddySession 兼容单对象 envelope 与 stream 消息数组两种形态。
func TestCodeBuddySessionExtract(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"single object", `{"type":"result","result":"ok","session_id":"s-1"}`, "s-1"},
		{"array with result", `[{"type":"system","session_id":"s-2"},{"type":"result","session_id":"s-3"}]`, "s-3"},
		{"array fallback first", `[{"type":"system","session_id":"s-4"},{"type":"assistant"}]`, "s-4"},
		{"array no session", `[{"type":"assistant"},{"type":"result"}]`, ""},
		{"empty", "", ""},
		{"plain text", "随便一段文本", ""},
		{"no session field", `{"type":"result","result":"ok"}`, ""},
	}
	for _, tc := range cases {
		if got := extractCodeBuddySession(tc.raw); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// codebuddy 数组形态下 Complete 也要把 session_id 挖出来回传。
func TestCodeBuddyCompleteExtractsSessionFromArray(t *testing.T) {
	cli := writeFakeCLI(t, "codebuddy", `#!/bin/sh
echo '[{"type":"system","session_id":"cb-init"},{"type":"result","result":"好"}]'
`)
	e := &CodeBuddyEngine{BinPath: cli}
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.SessionID != "cb-init" {
		t.Errorf("SessionID = %q want cb-init", resp.SessionID)
	}
}

func TestTraeResumeSessionEqualsForm(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "trae-cli", `echo '续上了'`)
	e := w.trae()
	resp, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		SessionID: "tr-9",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// trae 纯文本模式 CLI 不回 session_id → 回显请求里的 id，保证逐轮透传。
	if resp.SessionID != "tr-9" {
		t.Errorf("Response.SessionID = %q want tr-9（请求回显）", resp.SessionID)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	// pflag 可选值风格：必须等号形式，空格形式下裸 id 会被吞成 AUTO。
	if !strings.Contains(args, "--resume=tr-9") {
		t.Errorf("--resume=tr-9 (equals form) not passed: %q", args)
	}
}

func TestTraeNewSessionDefault(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "trae-cli", `echo 'ok'`)
	e := w.trae()
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// 新会话：请求里没有 id，回显也是空。
	if resp.SessionID != "" {
		t.Errorf("Response.SessionID = %q want empty", resp.SessionID)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "--resume") {
		t.Errorf("默认新会话不应带 --resume: %q", string(data))
	}
}

// ---------- Continue：续接最近一次会话（-c/--continue 语义） ----------

func TestClaudeContinueSession(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "claude", `echo '{"type":"result","result":"ok"}'`)
	e := w.claude()
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Continue: true,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	if !strings.Contains(args, "--continue") {
		t.Errorf("--continue not passed: %q", args)
	}
	if strings.Contains(args, "--resume") {
		t.Errorf("Continue 不应带 --resume: %q", args)
	}
}

func TestCodeBuddyContinueSession(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codebuddy", `echo '{"type":"result","result":"ok"}'`)
	e := w.codebuddy()
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Continue: true,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	if !strings.Contains(args, "--continue") {
		t.Errorf("--continue not passed: %q", args)
	}
	if strings.Contains(args, "--resume") {
		t.Errorf("Continue 不应带 --resume: %q", args)
	}
}

// trae 无 --continue：裸 --resume 触发 AUTO（续接最近一次会话），
// 且不能是等号形式（等号形式是显式 id 续接）。
func TestTraeContinueBareResume(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "trae-cli", `echo 'ok'`)
	e := w.trae()
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Continue: true,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	if !strings.Contains(args, "--resume") {
		t.Errorf("bare --resume not passed: %q", args)
	}
	if strings.Contains(args, "--resume=") {
		t.Errorf("Continue 应为裸 --resume，不带 =id: %q", args)
	}
}

// SessionID 优先于 Continue：两者同时设置时走显式 id 续接，
// 不应出现 --continue（trae 不应退化为裸 --resume）。
func TestSessionPriorityOverContinue(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
		want      string // 必须出现的参数片段
		forbid    string // 不应出现的参数片段
		fakeName  string
		canned    string
	}{
		{"claude", "s-1", "--resume s-1", "--continue", "claude", `echo '{"type":"result","result":"ok"}'`},
		{"codebuddy", "cb-1", "--resume cb-1", "--continue", "codebuddy", `echo '{"type":"result","result":"ok"}'`},
		{"trae", "tr-1", "--resume=tr-1", "--continue", "trae-cli", `echo 'ok'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			w, log := argsCaptureCLI(t, dir, tc.fakeName, tc.canned)
			var err error
			req := Request{
				Messages:  []Message{{Role: "user", Content: "hi"}},
				SessionID: tc.sessionID,
				Continue:  true,
			}
			switch tc.name {
			case "claude":
				_, err = w.claude().Complete(context.Background(), req)
			case "codebuddy":
				_, err = w.codebuddy().Complete(context.Background(), req)
			case "trae":
				_, err = w.trae().Complete(context.Background(), req)
			}
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			data, _ := os.ReadFile(log)
			args := string(data)
			if !strings.Contains(args, tc.want) {
				t.Errorf("%s not passed: %q", tc.want, args)
			}
			if strings.Contains(args, tc.forbid) {
				t.Errorf("%s 不应与 %s 同时出现: %q", tc.forbid, tc.want, args)
			}
		})
	}
}

// TestClaudeMaxTokensSettings 统一参数矩阵：claude 经 --settings 注入
// env CLAUDE_CODE_MAX_OUTPUT_TOKENS（无原生 max-tokens flag）。
func TestClaudeMaxTokensSettings(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "claude", `echo '{"type":"result","result":"ok"}'`)
	if _, err := w.claude().Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 8000,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	want := `--settings {"env":{"CLAUDE_CODE_MAX_OUTPUT_TOKENS":"8000"}}`
	if !strings.Contains(args, want) {
		t.Errorf("--settings 载荷未透传: %q", args)
	}
}

// TestCodeBuddyMaxTokensSettings 同上，作用于 codebuddy。
func TestCodeBuddyMaxTokensSettings(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codebuddy", `echo '{"type":"result","result":"ok"}'`)
	if _, err := w.codebuddy().Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 4096,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	want := `--settings {"env":{"CLAUDE_CODE_MAX_OUTPUT_TOKENS":"4096"}}`
	if !strings.Contains(args, want) {
		t.Errorf("--settings 载荷未透传: %q", args)
	}
}

// TestNoSettingsWhenMaxTokensUnset MaxTokens 未设置时不注入 --settings。
func TestNoSettingsWhenMaxTokensUnset(t *testing.T) {
	cases := []struct {
		name     string
		bin      string
		canned   string
		complete func(*engineWithLog, Request) error
	}{
		{"claude", "claude", `echo '{"type":"result","result":"ok"}'`,
			func(w *engineWithLog, req Request) error {
				_, err := w.claude().Complete(context.Background(), req)
				return err
			}},
		{"codebuddy", "codebuddy", `echo '{"type":"result","result":"ok"}'`,
			func(w *engineWithLog, req Request) error {
				_, err := w.codebuddy().Complete(context.Background(), req)
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			w, log := argsCaptureCLI(t, dir, tc.bin, tc.canned)
			if err := tc.complete(w, Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			data, _ := os.ReadFile(log)
			if strings.Contains(string(data), "--settings") {
				t.Errorf("MaxTokens 未设置时不应出现 --settings: %q", string(data))
			}
		})
	}
}

// TestTemperatureIgnoredByCLIs 统一参数矩阵：Temperature 仅 llm 支持，
// 其余引擎必须静默忽略（任何形式都不应出现温度参数）。
func TestTemperatureIgnoredByCLIs(t *testing.T) {
	temp := 0.7
	cases := []struct {
		name     string
		bin      string
		canned   string
		complete func(*engineWithLog, Request) error
	}{
		{"claude", "claude", `echo '{"type":"result","result":"ok"}'`,
			func(w *engineWithLog, req Request) error {
				_, err := w.claude().Complete(context.Background(), req)
				return err
			}},
		{"codebuddy", "codebuddy", `echo '{"type":"result","result":"ok"}'`,
			func(w *engineWithLog, req Request) error {
				_, err := w.codebuddy().Complete(context.Background(), req)
				return err
			}},
		{"trae", "trae-cli", `echo 'ok'`,
			func(w *engineWithLog, req Request) error {
				_, err := w.trae().Complete(context.Background(), req)
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			w, log := argsCaptureCLI(t, dir, tc.bin, tc.canned)
			req := Request{
				Messages:    []Message{{Role: "user", Content: "hi"}},
				Temperature: &temp,
			}
			if err := tc.complete(w, req); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			data, _ := os.ReadFile(log)
			args := string(data)
			if strings.Contains(strings.ToLower(args), "temperature") {
				t.Errorf("温度参数应被静默忽略: %q", args)
			}
		})
	}
}
