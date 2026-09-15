package cli

// ask_test.go - CLI 层测试。
//
// 覆盖：
//	- collectPrompt 输入收集（args / --file / stdin 组合）
//	- 端到端：fake 引擎注册 + cobra 命令执行 + 默认 json 输出
//	- 根命令简写（-p / 位置参数）与 ask 子命令等价
//	- --tools 解析（off / on / 白名单 / 非法值）
//	- 引擎/格式参数校验错误 → exit 2
//
// 默认值契约（newAskOptions 与 bindAskFlags 双处维护，须一致）：
//	engine=codebuddy, output=json, timeout=600s, tools=off

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/darren/magic-agent/internal/agent"
)

// stringEngine 最简单的假引擎：固定返回文本。
type stringEngine struct {
	name string
	text string
	err  error
}

func (s *stringEngine) Name() string           { return s.name }
func (s *stringEngine) Detect() (bool, string) { return true, "fake://" + s.name }
func (s *stringEngine) Complete(ctx context.Context, req agent.Request) (agent.Response, error) {
	if s.err != nil {
		return agent.Response{}, s.err
	}
	return agent.Response{Text: s.text, Model: "fake-model", Latency: 5_000_000}, nil
}

// capturingEngine 捕获收到的 prompt 与 tools 模式，供组合输入断言。
type capturingEngine struct {
	name       string
	lastPrompt string
	lastTools  agent.ToolsMode
}

func (c *capturingEngine) Name() string           { return c.name }
func (c *capturingEngine) Detect() (bool, string) { return true, "fake://" + c.name }
func (c *capturingEngine) Complete(ctx context.Context, req agent.Request) (agent.Response, error) {
	if len(req.Messages) > 0 {
		c.lastPrompt = req.Messages[len(req.Messages)-1].Content
	}
	c.lastTools = req.Tools
	return agent.Response{Text: "captured", Model: "fake-model", Latency: 1_000_000}, nil
}

// ── collectPrompt ─────────────────────────────────────────────

func TestCollectPromptFromArgs(t *testing.T) {
	parts, err := collectPrompt(strings.NewReader(""), []string{"你好", "世界"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0] != "你好 世界" {
		t.Errorf("parts = %v", parts)
	}
}

func TestCollectPromptFromFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.txt")
	if err := os.WriteFile(f, []byte("文件内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	parts, err := collectPrompt(strings.NewReader(""), nil, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0] != "文件内容" {
		t.Errorf("parts = %v", parts)
	}
}

func TestCollectPromptFilePlusArgs(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.txt")
	if err := os.WriteFile(f, []byte("上下文"), 0o644); err != nil {
		t.Fatal(err)
	}
	parts, err := collectPrompt(strings.NewReader(""), []string{"问题"}, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0] != "上下文" || parts[1] != "问题" {
		t.Errorf("parts = %v (want 文件在前、参数在后)", parts)
	}
}

func TestCollectPromptFileMissing(t *testing.T) {
	if _, err := collectPrompt(strings.NewReader(""), nil, "/no/such/file.txt"); err == nil {
		t.Error("expected error for missing file")
	}
}

// ── 端到端命令执行 ────────────────────────────────────────────

// runAskCmd 用给定 args 跑根命令，返回 (stdout, stderr, error)。
func runAskCmd(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errBuf.String(), err
}

// registerFake 注册假引擎并登记清理。
func registerFake(e agent.Engine) {
	agent.Register(e)
}

func TestAskEndToEndText(t *testing.T) {
	registerFake(&stringEngine{name: "fake-echo", text: "echoed"})

	// 默认输出 json；-o text 显式要纯文本。
	stdout, _, err := runAskCmd(t, "", "ask", "-e", "fake-echo", "-o", "text", "你好")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if strings.TrimSpace(stdout) != "echoed" {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestAskEndToEndJSON(t *testing.T) {
	registerFake(&stringEngine{name: "fake-json", text: "the answer"})

	// 默认（不传 -o）就是 json。
	stdout, _, err := runAskCmd(t, "", "ask", "-e", "fake-json", "hi")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	for _, want := range []string{
		`"engine":"fake-json"`,
		`"model":"fake-model"`,
		`"attempts":1`,
		`"latency_ms":5`,
		`"text":"the answer"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %s: %s", want, stdout)
		}
	}
}

// 根命令直接提问 = ask 的简写（flags 提升 persistent + RunE 直连）。
func TestRootShorthandAsk(t *testing.T) {
	registerFake(&stringEngine{name: "fake-root", text: "root answer"})

	// 位置参数形态
	stdout, _, err := runAskCmd(t, "", "-e", "fake-root", "你好")
	if err != nil {
		t.Fatalf("root ask: %v", err)
	}
	if !strings.Contains(stdout, `"text":"root answer"`) {
		t.Errorf("root positional stdout = %q", stdout)
	}

	// -p 提示词形态（-p = --prompt）
	stdout2, _, err := runAskCmd(t, "", "-e", "fake-root", "-p", "pipe问题")
	if err != nil {
		t.Fatalf("root -p ask: %v", err)
	}
	if !strings.Contains(stdout2, `"text":"root answer"`) {
		t.Errorf("root -p stdout = %q", stdout2)
	}
}

// -p 与位置参数组合：-p 在前、args 在后；--tools 透传到 Request。
func TestPromptFlagCombinedWithArgs(t *testing.T) {
	capEng := &capturingEngine{name: "fake-cap"}
	registerFake(capEng)

	_, _, err := runAskCmd(t, "", "-e", "fake-cap", "-p", "主问题", "附加", "上下文")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	got := capEng.lastPrompt
	if !strings.HasPrefix(got, "主问题") || !strings.Contains(got, "附加 上下文") {
		t.Errorf("prompt = %q, want -p 在前 + args 在后", got)
	}
	// 默认 tools=off 透传
	if !agent.ToolsIsOff(capEng.lastTools) {
		t.Errorf("default tools should be off, got %#v", capEng.lastTools)
	}
}

// --tools on / 白名单透传到引擎。
func TestToolsPassthrough(t *testing.T) {
	capEng := &capturingEngine{name: "fake-tools"}
	registerFake(capEng)

	if _, _, err := runAskCmd(t, "", "-e", "fake-tools", "--tools", "Bash,Read", "hi"); err != nil {
		t.Fatal(err)
	}
	al := agent.ToolsAllowlistOf(capEng.lastTools)
	if len(al) != 2 || al[0] != "Bash" || al[1] != "Read" {
		t.Errorf("allowlist = %v", al)
	}

	capEng.lastTools = nil
	if _, _, err := runAskCmd(t, "", "-e", "fake-tools", "--tools", "on", "hi"); err != nil {
		t.Fatal(err)
	}
	if !agent.ToolsIsOn(capEng.lastTools) {
		t.Errorf("tools=on should pass through, got %#v", capEng.lastTools)
	}
}

// --tools 参数解析：off / on / 白名单 / 非法值。
// 断言走 agent 包导出的 ToolsIsOff/ToolsIsOn/ToolsAllowlistOf
//（toolsModeImpl 未导出，跨包接口断言其小写方法不可行）。
func TestParseToolsMode(t *testing.T) {
	m1, err1 := parseToolsMode("off")
	if err1 != nil {
		t.Fatalf("off: %v", err1)
	}
	if !agent.ToolsIsOff(m1) {
		t.Errorf("off should parse to off mode, got %#v", m1)
	}

	m2, err2 := parseToolsMode("on")
	if err2 != nil {
		t.Fatalf("on: %v", err2)
	}
	if agent.ToolsIsOff(m2) || !agent.ToolsIsOn(m2) {
		t.Errorf("on should parse to on mode, got %#v", m2)
	}

	m3, err3 := parseToolsMode("Bash, Read")
	if err3 != nil {
		t.Fatalf("allowlist: %v", err3)
	}
	if agent.ToolsIsOff(m3) || agent.ToolsIsOn(m3) {
		t.Fatalf("allowlist mode wrong: %#v", m3)
	}
	if al := agent.ToolsAllowlistOf(m3); len(al) != 2 || al[0] != "Bash" || al[1] != "Read" {
		t.Errorf("allowlist = %v", al)
	}

	if _, err := parseToolsMode(" , "); err == nil {
		t.Error("comma-only value should error")
	}
}

// 默认值契约：engine=codebuddy / output=json / timeout=600s。
func TestDefaults(t *testing.T) {
	opts := newAskOptions()
	if opts.engine != "codebuddy" {
		t.Errorf("default engine = %q, want codebuddy", opts.engine)
	}
	if opts.output != "json" {
		t.Errorf("default output = %q, want json", opts.output)
	}
	if opts.timeout != 600_000_000_000 { // 600s
		t.Errorf("default timeout = %v, want 600s", opts.timeout)
	}
}

func TestAskStdinPipe(t *testing.T) {
	registerFake(&stringEngine{name: "fake-stdin", text: "ok"})

	stdout, _, err := runAskCmd(t, "来自管道的问题", "ask", "-e", "fake-stdin", "-o", "text")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if strings.TrimSpace(stdout) != "ok" {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestAskUnknownEngine(t *testing.T) {
	_, _, err := runAskCmd(t, "", "ask", "-e", "nope", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError (exit 2), got %T: %v", err, err)
	}
}

func TestAskBadFormat(t *testing.T) {
	_, _, err := runAskCmd(t, "", "ask", "-e", "claude", "-o", "yaml", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError, got %T", err)
	}
}

func TestAskEmptyPrompt(t *testing.T) {
	_, _, err := runAskCmd(t, "", "ask", "-e", "claude")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError, got %T", err)
	}
}

func TestAskNegativeRetries(t *testing.T) {
	_, _, err := runAskCmd(t, "", "ask", "-e", "claude", "-r", "-1", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError, got %T", err)
	}
}

func TestAskEngineFailureJSONError(t *testing.T) {
	registerFake(&stringEngine{name: "fake-bad", err: errors.New("connection refused")})

	stdout, stderr, err := runAskCmd(t, "", "ask", "-e", "fake-bad", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if stdout != "" {
		t.Errorf("stdout should be empty on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, `"error"`) {
		t.Errorf("stderr should contain json error envelope: %q", stderr)
	}
}

func TestExitCodeOf(t *testing.T) {
	if exitCodeOf(&usageError{errors.New("x")}) != 2 {
		t.Error("usageError should be exit 2")
	}
	if exitCodeOf(errors.New("x")) != 1 {
		t.Error("other errors should be exit 1")
	}
}

// unregisterLast：agent 包未暴露反注册；fake 引擎名字带 fake- 前缀、
// 不影响真实引擎查找，残留无害。
func unregisterLast() {}

// 确保 cobra 命令满足接口。
var _ = cobra.Command{}
