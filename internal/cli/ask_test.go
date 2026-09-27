package cli

// ask_test.go - CLI 层测试。
//
// 覆盖：
//	- collectPrompt 输入收集（args / --file / stdin 组合）
//	- 端到端：fake 引擎注册 + cobra 命令执行 + 默认 json 输出
//	- 根命令提问（-p / 位置参数）
//	- --tools 解析（off / on / 白名单 / 非法值）
//	- --engines flag（默认 JSON 数组；--json 兼容）
//	- 子命令已删除（ask / engines / version / completion 均不存在）
//	- 引擎/格式参数校验错误 → exit 2
//
// 默认值契约（newAskOptions 与 bindAskFlags 双处维护，须一致）：
//	engine=codebuddy, output=json, timeout=600s, tools=off

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/darren/magic-agent/internal/agent"
	"github.com/darren/magic-agent/internal/config"
	"github.com/darren/magic-agent/internal/session"
)

// TestMain 隔离用户真实配置与会话登记表。
//
// prepareAsk 会读配置文件取「默认系统提示词」（~/.config/magic-agent/config.json），
// 每次调用还会往会话登记表（~/.magic-agent/sessions/）落一条记录 —— 不隔离的话
// 本机那份配置会渗进断言，测试还会往用户真实登记表里写垃圾。
// 需要真配置/真登记表的用例自己用 t.Setenv 覆盖。
func TestMain(m *testing.M) {
	_ = os.Setenv(config.EnvPath, filepath.Join(os.TempDir(), "magic-agent-test-absent", "config.json"))
	// 登记表目录用 /tmp 下的**短**路径：常驻会话的追加入口是 unix socket，
	// 路径有 104 字节上限，而 macOS 的 TMPDIR 是 /var/folders/... 那种长路径。
	sessDir, err := os.MkdirTemp("/tmp", "mas-cli-test-")
	if err != nil {
		sessDir, err = os.MkdirTemp("", "mas-cli-test-")
	}
	if err == nil {
		_ = os.Setenv(session.EnvDir, sessDir)
	}
	code := m.Run()
	if err == nil {
		_ = os.RemoveAll(sessDir)
	}
	os.Exit(code)
}

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
	name          string
	lastPrompt    string
	lastTools     agent.ToolsMode
	lastSchema    *agent.JSONSchema
	lastMaxTok    int
	lastTemp      *float64
	lastSessionID string
	lastContinue  bool
	lastSystem    string
	lastAttach    []agent.Attachment
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
	c.lastSessionID = req.SessionID
	c.lastContinue = req.Continue
	c.lastSystem = req.SystemPrompt
	c.lastAttach = req.Attachments
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

// --session 会话续接 id 透传到引擎 Request；缺省为空 = 新会话。
func TestSessionPassthrough(t *testing.T) {
	capEng := &capturingEngine{name: "fake-sess"}
	registerFake(capEng)

	// 显式 --session abc → Request.SessionID == "abc"
	if _, _, err := runAskCmd(t, "", "-e", "fake-sess", "--session", "abc", "接着说"); err != nil {
		t.Fatal(err)
	}
	if capEng.lastSessionID != "abc" {
		t.Errorf("lastSessionID = %q, want abc", capEng.lastSessionID)
	}

	// 缺省 → 空串（新会话）
	capEng.lastSessionID = "sentinel"
	if _, _, err := runAskCmd(t, "", "-e", "fake-sess", "hi"); err != nil {
		t.Fatal(err)
	}
	if capEng.lastSessionID != "" {
		t.Errorf("default lastSessionID = %q, want empty (new session)", capEng.lastSessionID)
	}

	// 纯空白值视同缺省 → 空串
	capEng.lastSessionID = "sentinel"
	if _, _, err := runAskCmd(t, "", "-e", "fake-sess", "--session", "  ", "hi"); err != nil {
		t.Fatal(err)
	}
	if capEng.lastSessionID != "" {
		t.Errorf("blank lastSessionID = %q, want empty (trimmed)", capEng.lastSessionID)
	}
}

// -c/--continue 透传到引擎 Request；缺省 false = 新会话；
// --session 与 -c 同时给时两者都透传（引擎侧 SessionID 优先）。
func TestContinuePassthrough(t *testing.T) {
	capEng := &capturingEngine{name: "fake-cont"}
	registerFake(capEng)

	// -c → Continue == true
	if _, _, err := runAskCmd(t, "", "-e", "fake-cont", "-c", "接着说"); err != nil {
		t.Fatal(err)
	}
	if !capEng.lastContinue {
		t.Errorf("lastContinue = false, want true (-c)")
	}

	// 长形式 --continue 等价
	capEng.lastContinue = false
	if _, _, err := runAskCmd(t, "", "-e", "fake-cont", "--continue", "hi"); err != nil {
		t.Fatal(err)
	}
	if !capEng.lastContinue {
		t.Errorf("--continue long form not captured")
	}

	// 缺省 → false（新会话）
	capEng.lastContinue = true
	if _, _, err := runAskCmd(t, "", "-e", "fake-cont", "hi"); err != nil {
		t.Fatal(err)
	}
	if capEng.lastContinue {
		t.Errorf("default lastContinue = true, want false (new session)")
	}

	// --session 与 -c 同时给：CLI 层两者都透传，优先级由引擎侧保证
	if _, _, err := runAskCmd(t, "", "-e", "fake-cont", "--session", "abc", "-c", "hi"); err != nil {
		t.Fatal(err)
	}
	if capEng.lastSessionID != "abc" || !capEng.lastContinue {
		t.Errorf("session=%q continue=%v, want abc/true", capEng.lastSessionID, capEng.lastContinue)
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

// streamTimeoutEngine 记录流式调用收到的 req.Timeout（回归：-t 必须透传到引擎）。
type streamTimeoutEngine struct {
	name        string
	lastTimeout time.Duration
}

func (s *streamTimeoutEngine) Name() string           { return s.name }
func (s *streamTimeoutEngine) Detect() (bool, string) { return true, "fake://" + s.name }
func (s *streamTimeoutEngine) Complete(context.Context, agent.Request) (agent.Response, error) {
	return agent.Response{Text: "ok"}, nil
}
func (s *streamTimeoutEngine) Stream(_ context.Context, req agent.Request, onEvent func(agent.StreamEvent)) (agent.StreamResult, error) {
	s.lastTimeout = req.Timeout
	onEvent(agent.StreamEvent{Kind: agent.KindText, Text: "ok"})
	return agent.StreamResult{Response: agent.Response{Engine: s.name, Text: "ok"}}, nil
}

// 回归（2026-09-24）：流式路径必须把调用方**显式**给的 -t 透传给引擎。
//
// 以前流式路径从不设 req.Timeout → 引擎只能用自己的默认值（arkclaw 是 3 分钟），
// 于是桌面壳传的 `-t 600s` 被静默忽略；而网关侧带工具循环的任务要 5 分钟
// （实测「生成周报」304s 才回）→ 3 分钟必被砍，界面上就是「什么都没显示 · 调用失败」。
func TestStreamPassesExplicitTimeoutToEngine(t *testing.T) {
	eng := &streamTimeoutEngine{name: "fake-stream-timeout"}
	registerFake(eng)

	if _, _, err := runAskCmd(t, "", "-e", "fake-stream-timeout", "--stream", "-t", "900s", "hi"); err != nil {
		t.Fatalf("--stream -t 900s: %v", err)
	}
	if eng.lastTimeout != 900*time.Second {
		t.Errorf("引擎收到的 req.Timeout = %v want 15m（显式 -t 未透传）", eng.lastTimeout)
	}

	// 没给 -t → 不透传：别去覆盖引擎自己的默认（如 llm 的条目级 timeout、openclaw 的 600s）。
	eng.lastTimeout = -1
	if _, _, err := runAskCmd(t, "", "-e", "fake-stream-timeout", "--stream", "hi"); err != nil {
		t.Fatalf("--stream: %v", err)
	}
	if eng.lastTimeout != 0 {
		t.Errorf("未给 -t 时 req.Timeout = %v want 0（不该覆盖引擎默认）", eng.lastTimeout)
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
//
// 注意：以下用例一律带 --no-models —— 模型探测会真的启动本机 CLI
//（trae-cli / llm / codex / openclaw / codebuddy），单元测试不该依赖它们。
// models 字段本身的行为由 TestEnginesFlagIncludesModels 用假引擎 + 假探测覆盖。

// --engines 默认即输出 JSON 数组（含引擎名与是否可用），无需 --json。
func TestEnginesFlagJSONByDefault(t *testing.T) {
	registerFake(&stringEngine{name: "fake-list", text: "x"})

	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	if !strings.Contains(stdout, `"engine":"fake-list"`) || !strings.Contains(stdout, `"ok":true`) {
		t.Errorf("stdout = %q, want json array with engine name and ok", stdout)
	}
	if strings.Contains(stdout, "ENGINE") {
		t.Errorf("stdout = %q, want json output only (no text table)", stdout)
	}
}

// --engines --json 旧调用形态仍然兼容（同样输出 JSON）。
func TestEnginesFlagJSON(t *testing.T) {
	registerFake(&stringEngine{name: "fake-json-list", text: "x"})

	stdout, _, err := runAskCmd(t, "", "--engines", "--json", "--no-models")
	if err != nil {
		t.Fatalf("--engines --json: %v", err)
	}
	if !strings.Contains(stdout, `"engine":"fake-json-list"`) || !strings.Contains(stdout, `"ok":true`) {
		t.Errorf("stdout = %q, want json array with engine entries", stdout)
	}
}

// --engines 时不应触发提问（无 prompt 也不报错）。
func TestEnginesFlagSkipsPrompt(t *testing.T) {
	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines should not require prompt: %v", err)
	}
	if !strings.Contains(stdout, `"engine"`) {
		t.Errorf("stdout = %q, want json engine list", stdout)
	}
}

// modelListEngine 假引擎：在 stringEngine 之上实现 agent.ModelLister。
type modelListEngine struct {
	stringEngine
	models  []string
	err     error
	credits map[string]string // 非 nil 时实现 agent.ModelCreditLister
}

func (m *modelListEngine) ListModels(context.Context) ([]string, error) {
	return m.models, m.err
}

func (m *modelListEngine) ModelCredits(context.Context) map[string]string {
	return m.credits
}

// stubModelProbe 把 CLI 层的模型探测限制在假引擎上：
// 假引擎（*modelListEngine）走它自己的 ListModels，其余引擎（真实 CLI 引擎）
// 直接返回"无动态来源"。t.Cleanup 自动还原。
func stubModelProbe(t *testing.T) {
	t.Helper()
	prev := probeEngineModels
	probeEngineModels = func(ctx context.Context, l agent.ModelLister) ([]string, error) {
		if _, ok := l.(*modelListEngine); ok {
			return l.ListModels(ctx)
		}
		return nil, agent.ErrNoModelSource
	}
	t.Cleanup(func() { probeEngineModels = prev })
	// 倍率探测与模型探测同链：只放行假引擎，防止真实引擎读真机缓存。
	prevC := probeEngineCredits
	probeEngineCredits = func(_ context.Context, l agent.ModelCreditLister) map[string]string {
		if _, ok := l.(*modelListEngine); ok {
			return l.ModelCredits(context.Background())
		}
		return nil
	}
	t.Cleanup(func() { probeEngineCredits = prevC })
}

// enginesRow --engines 输出的一行（只看本用例关心的字段）。
type enginesRow struct {
	Engine     string   `json:"engine"`
	OK         bool     `json:"ok"`
	Models     []string `json:"models"`
	ModelsNote string   `json:"models_note"`
}

// findEnginesRow 按引擎名取行。
func findEnginesRow(t *testing.T, rows []enginesRow, name string) enginesRow {
	t.Helper()
	for _, r := range rows {
		if r.Engine == name {
			return r
		}
	}
	t.Fatalf("engine %q not found in %+v", name, rows)
	return enginesRow{}
}

// --engines 每行带 models（动态探测结果）；探测失败的引擎给 models_note。
func TestEnginesFlagIncludesModels(t *testing.T) {
	okEngine := &modelListEngine{
		stringEngine: stringEngine{name: "fake-models-ok"},
		models:       []string{"m-a", "m-b"},
	}
	errEngine := &modelListEngine{
		stringEngine: stringEngine{name: "fake-models-err"},
		err:          errors.New("probe failed: boom"),
	}
	registerFake(okEngine)
	registerFake(errEngine)
	stubModelProbe(t)

	stdout, _, err := runAskCmd(t, "", "--engines")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	var rows []enginesRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout, err)
	}

	ok := findEnginesRow(t, rows, "fake-models-ok")
	if len(ok.Models) != 2 || ok.Models[0] != "m-a" || ok.Models[1] != "m-b" {
		t.Errorf("models = %v want [m-a m-b]", ok.Models)
	}
	if ok.ModelsNote != "" {
		t.Errorf("models_note should be empty on success, got %q", ok.ModelsNote)
	}

	bad := findEnginesRow(t, rows, "fake-models-err")
	if len(bad.Models) != 0 {
		t.Errorf("failed probe should not report models, got %v", bad.Models)
	}
	if !strings.Contains(bad.ModelsNote, "probe failed: boom") {
		t.Errorf("models_note = %q, want probe error", bad.ModelsNote)
	}
}

// --engines 对实现 ModelCreditLister 的引擎带 model_credits（积分倍率表）；
// 未给倍率的引擎省略该字段。
func TestEnginesFlagIncludesModelCredits(t *testing.T) {
	withCredits := &modelListEngine{
		stringEngine: stringEngine{name: "fake-credits"},
		models:       []string{"fast-model", "deepseek-v4.1-flash", "custom-local:X"},
		credits:      map[string]string{"fast-model": "0.34", "deepseek-v4.1-flash": "0.00"},
	}
	withoutCredits := &modelListEngine{
		stringEngine: stringEngine{name: "fake-no-credits"},
		models:       []string{"m-a"},
	}
	registerFake(withCredits)
	registerFake(withoutCredits)
	stubModelProbe(t)

	stdout, _, err := runAskCmd(t, "", "--engines")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	var rows []struct {
		Engine       string            `json:"engine"`
		Models       []string          `json:"models"`
		ModelCredits map[string]string `json:"model_credits"`
	}
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout, err)
	}
	for _, r := range rows {
		switch r.Engine {
		case "fake-credits":
			if !equalStrMap(r.ModelCredits, map[string]string{"fast-model": "0.34", "deepseek-v4.1-flash": "0.00"}) {
				t.Errorf("model_credits = %v", r.ModelCredits)
			}
		case "fake-no-credits":
			if r.ModelCredits != nil {
				t.Errorf("engine without credits should omit model_credits, got %v", r.ModelCredits)
			}
		}
	}
}

func equalStrMap(a, b map[string]string) bool {
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

// --engines --no-models 完全不触发探测（不启动任何 CLI）。
func TestEnginesFlagNoModelsSkipsProbe(t *testing.T) {
	registerFake(&modelListEngine{
		stringEngine: stringEngine{name: "fake-no-models"},
		models:       []string{"m-a"},
	})

	prev := probeEngineModels
	calls := 0
	probeEngineModels = func(context.Context, agent.ModelLister) ([]string, error) {
		calls++
		return []string{"should-not-appear"}, nil
	}
	t.Cleanup(func() { probeEngineModels = prev })

	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines --no-models: %v", err)
	}
	if calls != 0 {
		t.Errorf("probe called %d times, want 0", calls)
	}
	if strings.Contains(stdout, `"models":`) || strings.Contains(stdout, `"models_note":`) {
		t.Errorf("stdout = %q, want no models fields", stdout)
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

// ── 默认系统提示词（配置文件 systemPrompt）────────────────────

// writeCLIConfig 把配置内容落到临时文件并返回路径（供 t.Setenv(config.EnvPath, ...)）。
func writeCLIConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// 没给 -s → 用配置文件里的 systemPrompt（全局默认）。
func TestSystemPromptDefaultsFromConfig(t *testing.T) {
	capEng := &capturingEngine{name: "fake-syscfg"}
	registerFake(capEng)
	t.Setenv(config.EnvPath, writeCLIConfig(t, `{"systemPrompt":"你是一个中文助手，始终用中文回答所有问题。"}`))

	if _, _, err := runAskCmd(t, "", "-e", "fake-syscfg", "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	want := "你是一个中文助手，始终用中文回答所有问题。"
	if capEng.lastSystem != want {
		t.Errorf("SystemPrompt = %q want %q", capEng.lastSystem, want)
	}
}

// -s 优先于配置里的默认值（给了就完全替换，不叠加）。
func TestSystemFlagOverridesConfigDefault(t *testing.T) {
	capEng := &capturingEngine{name: "fake-sysover"}
	registerFake(capEng)
	t.Setenv(config.EnvPath, writeCLIConfig(t, `{"systemPrompt":"配置里的默认"}`))

	if _, _, err := runAskCmd(t, "", "-e", "fake-sysover", "-s", "命令行给的", "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if capEng.lastSystem != "命令行给的" {
		t.Errorf("SystemPrompt = %q want 命令行值", capEng.lastSystem)
	}
}

// 配置里没有 systemPrompt（或文件不存在）→ 不注入，行为与以前一致。
func TestSystemPromptAbsentNotInjected(t *testing.T) {
	capEng := &capturingEngine{name: "fake-sysnone"}
	registerFake(capEng)

	// ① 配置文件存在但无该键
	t.Setenv(config.EnvPath, writeCLIConfig(t, `{"arkclaw":{"url":"u","key":"k","claw_id":"c"}}`))
	if _, _, err := runAskCmd(t, "", "-e", "fake-sysnone", "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if capEng.lastSystem != "" {
		t.Errorf("无该键时应为空, got %q", capEng.lastSystem)
	}

	// ② 配置文件不存在
	t.Setenv(config.EnvPath, filepath.Join(t.TempDir(), "absent.json"))
	if _, _, err := runAskCmd(t, "", "-e", "fake-sysnone", "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if capEng.lastSystem != "" {
		t.Errorf("文件缺失时应为空, got %q", capEng.lastSystem)
	}
}

// 环境变量 MAGIC_AGENT_SYSTEM_PROMPT 覆盖文件值。
func TestSystemPromptEnvOverride(t *testing.T) {
	capEng := &capturingEngine{name: "fake-sysenv"}
	registerFake(capEng)
	t.Setenv(config.EnvPath, writeCLIConfig(t, `{"systemPrompt":"文件里的"}`))
	t.Setenv(config.EnvSystemPrompt, "环境变量里的")

	if _, _, err := runAskCmd(t, "", "-e", "fake-sysenv", "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if capEng.lastSystem != "环境变量里的" {
		t.Errorf("SystemPrompt = %q want 环境变量值", capEng.lastSystem)
	}
}

// 配置文件语法错 → 不阻断调用（不注入默认值）；-v 时在 stderr 说明原因。
func TestBrokenConfigDoesNotBreakAsk(t *testing.T) {
	capEng := &capturingEngine{name: "fake-sysbad"}
	registerFake(capEng)
	t.Setenv(config.EnvPath, writeCLIConfig(t, `{"systemPrompt":`))

	if _, _, err := runAskCmd(t, "", "-e", "fake-sysbad", "hi"); err != nil {
		t.Fatalf("坏配置不应让调用失败: %v", err)
	}
	if capEng.lastSystem != "" {
		t.Errorf("坏配置时不注入, got %q", capEng.lastSystem)
	}

	// -v：stderr 出现提示（不静默）
	_, stderr, err := runAskCmd(t, "", "-e", "fake-sysbad", "-v", "hi")
	if err != nil {
		t.Fatalf("ask -v: %v", err)
	}
	if !strings.Contains(stderr, "读取默认系统提示词失败") {
		t.Errorf("-v 应提示配置读取失败, stderr = %q", stderr)
	}
}
