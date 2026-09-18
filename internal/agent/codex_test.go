package agent

// codex_test.go - Codex 引擎单元测试。
//
// 覆盖：注册表、buildArgs 关键 flag 映射、NDJSON envelope 解析、
// Continue 走 resume --last、max-tokens/temperature 静默忽略、CLI 缺失报错。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCodexLookup(t *testing.T) {
	e := Lookup("codex")
	if e == nil {
		t.Fatal("Lookup(\"codex\") = nil")
	}
	if e.Name() != "codex" {
		t.Errorf("Name() = %q want codex", e.Name())
	}
}

func TestCodexBuildArgsNewSession(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codex", codexStubWithText("hi from codex"))
	e := &CodexEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := readFile(log)
	args := string(data)
	for _, want := range []string{
		"exec",
		"--skip-git-repo-check",
		"--json",
		// 不带 --ephemeral:codex 0.133 在该模式下不落盘,续接会报 "no rollout found"
		"-s read-only", // off 模式默认 read-only 沙箱
	} {
		if !strings.Contains(args, want) {
			t.Errorf("want %q in args, got %q", want, args)
		}
	}
}

func TestCodexBuildArgsModel(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codex", codexStubWithText("ok"))
	e := &CodexEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Model:    "codex/MiniMax-M3",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := readFile(log)
	if !strings.Contains(string(data), "-m MiniMax-M3") {
		t.Errorf("model flag missing: %q", string(data))
	}
}

func TestCodexSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codex", codexStubWithText("ok"))
	e := &CodexEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		SystemPrompt: "你是助手",
		Messages:     []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := readFile(log)
	args := string(data)
	// -c developer_instructions="<TOML 字符串>":json.Marshal 输出应包含转义。
	if !strings.Contains(args, "-c developer_instructions=") {
		t.Errorf("-c developer_instructions missing: %q", args)
	}
	if !strings.Contains(args, "你是助手") {
		t.Errorf("system prompt content missing: %q", args)
	}
}

func TestCodexResumeSession(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codex", codexStubWithText("续上"))
	e := &CodexEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		SessionID: "thr-1",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := readFile(log)
	args := string(data)
	if !strings.Contains(args, "exec resume thr-1") {
		t.Errorf("exec resume thr-1 not passed: %q", args)
	}
	// 续接时不应重新开新会话(--ephemeral 一律不带;见 buildArgs 注释)
	// 续接时也不带沙箱 flag(codex 0.133 `exec resume` 不接受 -s)
	if strings.Contains(args, "-s read-only") || strings.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("resume 不应带 sandbox flag: %q", args)
	}
}

func TestCodexContinueUsesLast(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codex", codexStubWithText("ok"))
	e := &CodexEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Continue: true,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := readFile(log)
	args := string(data)
	if !strings.Contains(args, "exec resume --last") {
		t.Errorf("exec resume --last not passed: %q", args)
	}
	// 同 TestCodexResumeSession:resume --last 也不带 sandbox flag
	if strings.Contains(args, "-s ") || strings.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("resume --last 不应带 sandbox flag: %q", args)
	}
}

func TestCodexMaxTokensIgnored(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codex", codexStubWithText("ok"))
	e := &CodexEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 4096,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := readFile(log)
	if strings.Contains(string(data), "max_tokens") || strings.Contains(string(data), "model_max_output_tokens") {
		t.Errorf("MaxTokens 应静默忽略: %q", string(data))
	}
}

func TestCodexTemperatureIgnored(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codex", codexStubWithText("ok"))
	e := &CodexEngine{BinPath: w.bin}
	temp := 0.5
	if _, err := e.Complete(context.Background(), Request{
		Messages:    []Message{{Role: "user", Content: "hi"}},
		Temperature: &temp,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := readFile(log)
	if strings.Contains(strings.ToLower(string(data)), "temperature") {
		t.Errorf("temperature 应静默忽略: %q", string(data))
	}
}

// codexStubWithText 构造一个"会落 -o 文件 + 输出 agent_message 事件"的 stub,
// 让 codex.Complete 完整跑通(从 NDJSON 事件流或落盘文件取到 Text)。
func codexStubWithText(text string) string {
	// 把 text 中的双引号/反斜杠做转义,避免破坏 shell 字符串
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "$", `\$`).Replace(text)
	return `#!/bin/sh
# 1) 落 -o 文件
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then
    printf '` + esc + `\n' > "$arg"
  fi
  prev="$arg"
done
# 2) 输出 NDJSON 事件流(item.completed.agent_message 让正文直接被解析到)
echo '{"type":"thread.started","thread_id":"stub-1"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"` + esc + `"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}'
`
}

func TestCodexParseStdoutExtractsAll(t *testing.T) {
	// codex --json 典型事件流
	stdout := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thr-xyz"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"这是答案"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":300,"output_tokens":50,"reasoning_output_tokens":20}}`,
	}, "\n")
	e := &CodexEngine{}
	resp := e.parseStdout(stdout)
	if resp.Text != "这是答案" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.SessionID != "thr-xyz" {
		t.Errorf("SessionID = %q", resp.SessionID)
	}
	// input(1200) + cached(300) = 1500; output(50) + reasoning(20) = 70
	if resp.InputTokens != 1500 {
		t.Errorf("InputTokens = %d want 1500", resp.InputTokens)
	}
	if resp.OutputTokens != 70 {
		t.Errorf("OutputTokens = %d want 70", resp.OutputTokens)
	}
}

func TestCodexCompleteWithOutputFile(t *testing.T) {
	// 走兜底:stdout 没 agent_message 事件但 -o 落盘文件有内容
	cli := writeFakeCLI(t, "codex", `#!/bin/sh
# 解析 -o 参数,落盘文件;同时 echo thread.started
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then
    printf '兜底正文\n' > "$arg"
  fi
  prev="$arg"
done
echo '{"type":"thread.started","thread_id":"thr-9"}'
`)
	e := &CodexEngine{BinPath: cli}
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(resp.Text, "兜底正文") {
		t.Errorf("Text 应从 -o 文件兜底读出: %q", resp.Text)
	}
	if resp.SessionID != "thr-9" {
		t.Errorf("SessionID = %q", resp.SessionID)
	}
}

func TestCodexNotFound(t *testing.T) {
	e := &CodexEngine{BinPath: "/nonexistent/codex"}
	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for missing CLI")
	}
}

func TestCodexTimeoutPassesProcessDeadline(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codex", codexStubWithText("ok"))
	e := &CodexEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Timeout:  2 * time.Minute,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// codex 不在 args 层暴露 timeout(由 ctx deadline 接管);只验证进程能跑通。
	_ = log
}

func TestCodexToMLBasicString(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello", `"hello"`},
		{`含"引号`, `"含\"引号"`},
		{"换\n行", `"换\n行"`},
		{"反\\斜杠", `"反\\斜杠"`},
	}
	for _, tc := range cases {
		if got := tomlBasicString(tc.in); got != tc.want {
			t.Errorf("tomlBasicString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// readFile 小工具,直接复用 os.ReadFile。
func readFile(p string) ([]byte, error) {
	return os.ReadFile(p)
}
