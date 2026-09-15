package cli

// ask_test.go - CLI 层测试。
//
// 覆盖：
//	- collectPrompt 输入收集（args / --file / stdin 组合）
//	- 端到端：fake 引擎注册 + cobra 命令执行 + 默认 json 输出
//	- 根命令提问（-p / 位置参数）
//	- --tools 解析（off / on / 白名单 / 非法值）
//	- --engines flag（text 表格 + --json）
//	- 子命令已删除（ask / engines / version / completion 均不存在）
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

// streamingEngine 同时实现 Engine + Streamer 的假引擎。
type streamingEngine struct {
	name     string
	text     string
	thinking string
	err      error
}

func (s *streamingEngine) Name() string           { return s.name }
func (s *streamingEngine) Detect() (bool, string) { return true, "fake://" + s.name }
func (s *streamingEngine) Complete(ctx context.Context, req agent.Request) (agent.Response, error) {
	if s.err != nil {
		return agent.Response{}, s.err
	}
	return agent.Response{Text: s.text, Model: "fake-model", Latency: 5_000_000}, nil
}

func (s *streamingEngine) Stream(ctx context.Context, req agent.Request, onEvent func(agent.StreamEvent)) (agent.StreamResult, error) {
	if s.err != nil {
		return agent.StreamResult{}, s.err
	}
	if s.thinking != "" {
		onEvent(agent.StreamEvent{Kind: agent.KindThinking, Text: s.thinking})
	}
	onEvent(agent.StreamEvent{Kind: agent.KindText, Text: s.text})
	return agent.StreamResult{
		Response: agent.Response{Engine: s.name, Text: s.text, Model: "fake-model", Latency: 5_000_000},
		Thinking: s.thinking,
	}, nil
}

// capturingEngine 捕获收到的 prompt 与 tools 模式，供组合输入断言。
type capturingEngine struct {
	name         string
	lastPrompt   string
	lastTools    agent.ToolsMode
	lastSchema   *agent.JSONSchema
	lastMaxTok   int
	lastTemp     *float64
}

func (c *capturingEngine) Name() string           { return c.name }
func (c *capturingEngine) Detect() (bool, string) { return true, "fake://" + c.name }
func (c *capturingEngine) Complete(ctx context.Context, req agent.Request) (agent.Response, error) {
	if len(req.Messages) > 0 {
		c.lastPrompt = req.Messages[len(req.Messages)-1].Content
	}
	c.lastTools = req.Tools
	c.lastSchema = req.JSONSchema
	c.lastMaxTok = req.MaxTokens
	c.lastTemp = req.Temperature
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
	stdout, _, err := runAskCmd(t, "", "-e", "fake-echo", "-o", "text", "你好")
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
	stdout, _, err := runAskCmd(t, "", "-e", "fake-json", "hi")
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

// 根命令直接提问（唯一形态，子命令已删除）。
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
// （toolsModeImpl 未导出，跨包接口断言其小写方法不可行）。
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

	stdout, _, err := runAskCmd(t, "来自管道的问题", "-e", "fake-stdin", "-o", "text")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if strings.TrimSpace(stdout) != "ok" {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestAskUnknownEngine(t *testing.T) {
	_, _, err := runAskCmd(t, "", "-e", "nope", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError (exit 2), got %T: %v", err, err)
	}
}

func TestAskBadFormat(t *testing.T) {
	_, _, err := runAskCmd(t, "", "-e", "claude", "-o", "yaml", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError, got %T", err)
	}
}

func TestAskEmptyPrompt(t *testing.T) {
	_, _, err := runAskCmd(t, "", "-e", "claude")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError, got %T", err)
	}
}

// --json-schema 解析 + 透传到 Request.JSONSchema。
func TestJSONSchemaPassesThrough(t *testing.T) {
	capEng := &capturingEngine{name: "fake-schema"}
	registerFake(capEng)

	schema := `{"type":"object","properties":{"score":{"type":"integer"}},"required":["score","issues"]}`
	_, _, err := runAskCmd(t, "", "-e", "fake-schema", "--json-schema", schema, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if capEng.lastSchema == nil {
		t.Fatal("lastSchema is nil")
	}
	if !strings.EqualFold(capEng.lastSchema.Type, "object") {
		t.Errorf("Type = %q", capEng.lastSchema.Type)
	}
	if len(capEng.lastSchema.Required) != 2 ||
		capEng.lastSchema.Required[0] != "score" || capEng.lastSchema.Required[1] != "issues" {
		t.Errorf("Required = %v", capEng.lastSchema.Required)
	}
}

// --json-schema 非合法 JSON → usage 错误。
func TestJSONSchemaRejectsBadJSON(t *testing.T) {
	_, _, err := runAskCmd(t, "", "-e", "claude", "--json-schema", "not json", "hi")
	if _, ok := err.(*usageError); !ok {
		t.Errorf("expected usageError, got %T: %v", err, err)
	}
}

// --json-schema type!=object → 拒绝。
func TestJSONSchemaRejectsNonObject(t *testing.T) {
	_, _, err := runAskCmd(t, "", "-e", "claude", "--json-schema", `{"type":"array","items":{"type":"string"}}`, "hi")
	if _, ok := err.(*usageError); !ok {
		t.Errorf("expected usageError, got %T: %v", err, err)
	}
}

// --max-tokens / --temperature 透传到 Request。
func TestModelOptionsPassThrough(t *testing.T) {
	capEng := &capturingEngine{name: "fake-opts"}
	registerFake(capEng)

	_, _, err := runAskCmd(t, "", "-e", "fake-opts", "--max-tokens", "32000", "--temperature", "0.3", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if capEng.lastMaxTok != 32000 {
		t.Errorf("MaxTokens = %d want 32000", capEng.lastMaxTok)
	}
	if capEng.lastTemp == nil || *capEng.lastTemp != 0.3 {
		t.Errorf("Temperature = %v want 0.3", capEng.lastTemp)
	}
}

func TestAskNegativeRetries(t *testing.T) {
	_, _, err := runAskCmd(t, "", "-e", "claude", "-r", "-1", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError, got %T", err)
	}
}

func TestAskEngineFailureJSONError(t *testing.T) {
	registerFake(&stringEngine{name: "fake-bad", err: errors.New("connection refused")})

	stdout, stderr, err := runAskCmd(t, "", "-e", "fake-bad", "hi")
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

// ── json 错误 envelope：error + reason 双字段 ────────────────

// 引擎失败时 json envelope 必须带 reason（根因），且 reason 不含外层包装前缀。
func TestErrorEnvelopeReasonField(t *testing.T) {
	registerFake(&stringEngine{name: "fake-reason", err: errors.New("root cause xyz")})

	stdout, stderr, err := runAskCmd(t, "", "-e", "fake-reason", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	_ = stdout
	if !strings.Contains(stderr, `"reason":"root cause xyz"`) {
		t.Errorf("stderr should contain reason field with root cause: %q", stderr)
	}
	// error 字段保留完整链（含引擎名），reason 字段是纯根因。
	if !strings.Contains(stderr, `"error":"`) {
		t.Errorf("stderr should contain error field: %q", stderr)
	}
}

// text 模式失败输出保持单行纯文本（不带 json）。
func TestErrorTextMode(t *testing.T) {
	registerFake(&stringEngine{name: "fake-text-err", err: errors.New("boom text")})

	stdout, stderr, err := runAskCmd(t, "", "-e", "fake-text-err", "-o", "text", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if stdout != "" {
		t.Errorf("stdout should be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, "boom text") || strings.Contains(stderr, `"error"`) {
		t.Errorf("text mode stderr should be plain text line: %q", stderr)
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

// ── --engines flag ───────────────────────────────────────────

func TestEnginesFlagText(t *testing.T) {
	registerFake(&stringEngine{name: "fake-list", text: "x"})

	stdout, _, err := runAskCmd(t, "", "--engines")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	if !strings.Contains(stdout, "ENGINE") || !strings.Contains(stdout, "fake-list") {
		t.Errorf("stdout = %q, want engine table with fake-list", stdout)
	}
}

func TestEnginesFlagJSON(t *testing.T) {
	registerFake(&stringEngine{name: "fake-json-list", text: "x"})

	stdout, _, err := runAskCmd(t, "", "--engines", "--json")
	if err != nil {
		t.Fatalf("--engines --json: %v", err)
	}
	if !strings.Contains(stdout, `"engine":"fake-json-list"`) || !strings.Contains(stdout, `"ok":true`) {
		t.Errorf("stdout = %q, want json array with engine entries", stdout)
	}
}

// --engines 时不应触发提问（无 prompt 也不报错）。
func TestEnginesFlagSkipsPrompt(t *testing.T) {
	stdout, _, err := runAskCmd(t, "", "--engines")
	if err != nil {
		t.Fatalf("--engines should not require prompt: %v", err)
	}
	if !strings.Contains(stdout, "ENGINE") {
		t.Errorf("stdout = %q, want engine table", stdout)
	}
}

// ── 子命令已删除 ──────────────────────────────────────────────

// ask / engines / version / completion 子命令均已删除：
// 这些词不再被识别为子命令——作为位置参数它们就是普通 prompt。
// 真实执行会走引擎调用（测试环境无 codebuddy CLI，报 unknown engine），
// 这里用 cobra 命令树直接断言不存在任何子命令。
func TestSubcommandsRemoved(t *testing.T) {
	cmd := NewRootCommand()
	if subs := cmd.Commands(); len(subs) != 0 {
		names := make([]string, 0, len(subs))
		for _, s := range subs {
			names = append(names, s.Name())
		}
		t.Errorf("root should have no subcommands, got %v", names)
	}
}

// ── --stream 流式 ─────────────────────────────────────────────

// json 模式：NDJSON 事件流 + result 收尾行。
func TestStreamJSONMode(t *testing.T) {
	registerFake(&streamingEngine{name: "fake-stream", text: "答案", thinking: "思考"})

	stdout, stderr, err := runAskCmd(t, "", "-e", "fake-stream", "--stream", "1+1")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout lines = %d, want 3 (thinking + text + result): %q", len(lines), stdout)
	}
	if !strings.Contains(lines[0], `"type":"thinking"`) || !strings.Contains(lines[0], "思考") {
		t.Errorf("line0 = %q, want thinking event", lines[0])
	}
	if !strings.Contains(lines[1], `"type":"text"`) || !strings.Contains(lines[1], "答案") {
		t.Errorf("line1 = %q, want text event", lines[1])
	}
	if !strings.Contains(lines[2], `"type":"result"`) || !strings.Contains(lines[2], `"text":"答案"`) {
		t.Errorf("line2 = %q, want result envelope", lines[2])
	}
	if !strings.Contains(lines[2], `"thinking":"思考"`) {
		t.Errorf("result envelope missing thinking: %q", lines[2])
	}
	if stderr != "" {
		t.Errorf("json 模式 stderr 应为空, got %q", stderr)
	}
}

// text 模式：正文 → stdout，思考 → stderr。
func TestStreamTextMode(t *testing.T) {
	registerFake(&streamingEngine{name: "fake-stream-t", text: "正文", thinking: "思考过程"})

	stdout, stderr, err := runAskCmd(t, "", "-e", "fake-stream-t", "--stream", "-o", "text", "hi")
	if err != nil {
		t.Fatalf("stream text: %v", err)
	}
	if strings.TrimSpace(stdout) != "正文" {
		t.Errorf("stdout = %q, want 正文", stdout)
	}
	if !strings.Contains(stderr, "思考过程") {
		t.Errorf("stderr missing thinking: %q", stderr)
	}
}

// --no-thinking：思考增量不转发。
func TestStreamNoThinking(t *testing.T) {
	registerFake(&streamingEngine{name: "fake-stream-nt", text: "答案", thinking: "思考"})

	// json 模式：无 thinking 事件行，result envelope 不含 thinking 字段
	stdout, _, err := runAskCmd(t, "", "-e", "fake-stream-nt", "--stream", "--no-thinking", "hi")
	if err != nil {
		t.Fatalf("stream no-thinking: %v", err)
	}
	if strings.Contains(stdout, `"type":"thinking"`) {
		t.Errorf("stdout should not contain thinking events: %q", stdout)
	}
	if strings.Contains(stdout, `"thinking":"思考"`) {
		t.Errorf("result envelope should omit thinking: %q", stdout)
	}

	// text 模式：stderr 无思考
	_, stderr, err := runAskCmd(t, "", "-e", "fake-stream-nt", "--stream", "--no-thinking", "-o", "text", "hi")
	if err != nil {
		t.Fatalf("stream no-thinking text: %v", err)
	}
	if strings.Contains(stderr, "思考") {
		t.Errorf("stderr should not contain thinking: %q", stderr)
	}
}

// 不支持流式的引擎 → usage error（exit 2）。
func TestStreamUnsupportedEngine(t *testing.T) {
	registerFake(&stringEngine{name: "fake-nostream", text: "x"})

	_, _, err := runAskCmd(t, "", "-e", "fake-nostream", "--stream", "hi")
	if err == nil {
		t.Fatal("expected error for non-streaming engine")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError (exit 2), got %T: %v", err, err)
	}
}

// 流式失败：json 模式 stdout 只含已发出的增量事件，无 result 行；
// 错误 envelope 不重复打印（reportedError）。
func TestStreamFailureJSON(t *testing.T) {
	registerFake(&streamingEngine{name: "fake-stream-bad", err: errors.New("connection refused")})

	stdout, _, err := runAskCmd(t, "", "-e", "fake-stream-bad", "--stream", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(stdout, `"type":"result"`) {
		t.Errorf("failed stream should not emit result line: %q", stdout)
	}
}

// 确保 cobra 命令满足接口。
var _ = cobra.Command{}
