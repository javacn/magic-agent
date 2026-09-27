package agent

// dsh_sdk_test.go - dsh **SDK 通道**（`dsh --profile sdk`）单元测试。
//
// 假 CLI 是一段 POSIX sh：按 `--profile` 分流 —— `sdk` 走 JSON-RPC 循环（读一行请求、
// 按方法名回一行响应/通知），其它 profile 走 headless 行为。这样同一条二进制既能验
// 「SDK 通道可用」，也能验「SDK 不可用 → 回退 headless」。
//
// 依据（协议形状来自官方 d.ts，不是从样本猜的）：
//
//	initialize → {"serverInfo":{"name":"deepseek-harness-sdk-runtime","version":"0.0.1"}}
//	session/prompt → {"messageId":…}
//	← session.event {sessionId, event:{type, data}}
//	     assistant/message → message.content[] 的 reasoning / text 块
//	     tool/call         → {turn, step, callId, name, arguments}
//	     tool/result       → {message:{source:{kind:"tool",callId}, content:[ToolResultBlock]}}
//	     turn/end          → {turn, reason:{kind:"completed"}}
//	← session.status {sessionId, status:"running"|"idle"}

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// dshSDKStubOpts 假 CLI 的可注入行为（零值 = 一条成功的一轮）。
type dshSDKStubOpts struct {
	// InitResponse initialize 的响应行（空 = 默认成功握手）。
	InitResponse string
	// PromptEvents session/prompt 之后推送的事件行（sh 片段，可用 $sid）。
	PromptEvents string
	// HeadlessBody 非 sdk profile 时的行为（空 = 打印一行 headless 正文）。
	HeadlessBody string
	// SilentSDK true = sdk profile 起来就退出（模拟旧版 dsh 没有该 profile）。
	SilentSDK bool
}

// dshSDKStub 造一个会说 SDK 协议的假 dsh，返回 (bin, argsLog, stdinLog)。
// argsLog 追加每次启动的参数（不截断，便于判断起了几次、走的哪条通道）；
// stdinLog 追加每条收到的 JSON-RPC 请求（便于断言 initialize 的载荷）。
func dshSDKStub(t *testing.T, dir string, opts dshSDKStubOpts) (bin, argsLog, stdinLog string) {
	t.Helper()
	argsLog = filepath.Join(dir, "args.log")
	stdinLog = filepath.Join(dir, "stdin.log")
	bin = filepath.Join(dir, "dsh")

	if opts.InitResponse == "" {
		opts.InitResponse = `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"deepseek-harness-sdk-runtime","version":"0.0.1"}}}`
	}
	if opts.PromptEvents == "" {
		opts.PromptEvents = defaultDshSDKEvents
	}
	if opts.HeadlessBody == "" {
		opts.HeadlessBody = "echo 'headless 正文'"
	}
	silent := ""
	if opts.SilentSDK {
		silent = "exit 0"
	}

	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + argsLog + "\n" +
		"if [ \"$1\" = \"--profile\" ] && [ \"$2\" = \"sdk\" ]; then\n" +
		silent + "\n" +
		"  turn=0\n" +
		"  while IFS= read -r line; do\n" +
		"    printf '%s\\n' \"$line\" >> " + stdinLog + "\n" +
		"    case \"$line\" in\n" +
		"      *initialize*)\n" +
		"        printf '%s\\n' '" + opts.InitResponse + "'\n" +
		"        ;;\n" +
		"      *session/prompt*)\n" +
		"        sid=$(printf '%s' \"$line\" | sed -n 's/.*\"sessionId\":\"\\([^\"]*\\)\".*/\\1/p')\n" +
		"        turn=$((turn+1))\n" +
		"        printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"messageId\":\"m1\"}}'\n" +
		opts.PromptEvents + "\n" +
		"        ;;\n" +
		"      *shutdown*)\n" +
		"        printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{}}'\n" +
		"        ;;\n" +
		"    esac\n" +
		"  done\n" +
		"  exit 0\n" +
		"fi\n" +
		opts.HeadlessBody + "\n"

	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsLog, stdinLog
}

// defaultDshSDKEvents 一轮标准事件：推理 → 工具调用 → 工具结果 → 正文 → turn/end。
//
// 注意 printf 的 %s 不解释参数里的反斜杠（只有格式串才解释），所以 JSON 里要表达的
// `\n` 原样写 `\n`、要表达的 `\"` 在 sh 双引号里写 `\\\"`。
const defaultDshSDKEvents = `        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.event\",\"params\":{\"sessionId\":\"$sid\",\"event\":{\"type\":\"assistant/message\",\"data\":{\"turn\":$turn,\"step\":1,\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"reasoning\",\"text\":\"先看看仓库\\n\"}]}}}}}"
        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.event\",\"params\":{\"sessionId\":\"$sid\",\"event\":{\"type\":\"tool/call\",\"data\":{\"turn\":$turn,\"step\":1,\"callId\":\"c1\",\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}}}"
        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.event\",\"params\":{\"sessionId\":\"$sid\",\"event\":{\"type\":\"tool/result\",\"data\":{\"turn\":$turn,\"step\":1,\"message\":{\"source\":{\"kind\":\"tool\",\"callId\":\"c1\"},\"content\":[{\"type\":\"tool-result\",\"toolCallId\":\"c1\",\"content\":[{\"type\":\"text\",\"text\":\"a.txt\\n\"}],\"isError\":false}]}}}}}"
        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.event\",\"params\":{\"sessionId\":\"$sid\",\"event\":{\"type\":\"assistant/message\",\"data\":{\"turn\":$turn,\"step\":2,\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"目录里有 a.txt\"}]}}}}}"
        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.event\",\"params\":{\"sessionId\":\"$sid\",\"event\":{\"type\":\"turn/end\",\"data\":{\"turn\":$turn,\"reason\":{\"kind\":\"completed\"}}}}}"
        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.status\",\"params\":{\"sessionId\":\"$sid\",\"status\":\"idle\"}}"`

// dshSDKHome 把 $DSH_HOME 指向临时目录并写入 agent-default-model（initialize 的必填项）。
func dshSDKHome(t *testing.T, provider, model string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	cfg := "agent-default-model:\n  provider: " + provider + "\n  model: " + model + "\n"
	if err := os.WriteFile(filepath.Join(home, "settings.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 默认（Profile 空）走 SDK 通道：启动参数是 `--profile sdk`，正文来自 session.event。
func TestDshSDKIsDefaultTransport(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, argsLog, _ := dshSDKStub(t, dir, dshSDKStubOpts{})

	resp, err := (&DshEngine{BinPath: bin}).Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "看看目录"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "目录里有 a.txt" {
		t.Errorf("Text = %q want 目录里有 a.txt", resp.Text)
	}
	// 模型标签用 initialize 实际用的路由，不是配置层猜的。
	if resp.Model != "modelverse/deepseek-v4.1-flash" {
		t.Errorf("Model = %q want modelverse/deepseek-v4.1-flash", resp.Model)
	}
	// dsh 续不上会话 → 不回可续接的 id（见 errDshNoSessionResume）。
	if resp.SessionID != "" {
		t.Errorf("SessionID = %q want 空（dsh 不支持续接）", resp.SessionID)
	}
	args := readFileString(t, argsLog)
	if !strings.Contains(args, "--profile sdk") {
		t.Errorf("默认应走 SDK 通道: %q", args)
	}
	if strings.Contains(args, "--profile headless") {
		t.Errorf("SDK 可用时不该回退 headless: %q", args)
	}
}

// 流式：推理 / 工具调用 / 工具结果 / 正文 / turn 收尾，全部按顺序上报。
func TestDshSDKStreamEmitsAllEventKinds(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, _, _ := dshSDKStub(t, dir, dshSDKStubOpts{})

	var mu sync.Mutex
	var events []StreamEvent
	res, err := (&DshEngine{BinPath: bin}).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "看看目录"}},
	}, func(ev StreamEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "目录里有 a.txt" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Thinking != "先看看仓库\n" {
		t.Errorf("Thinking = %q want %q", res.Thinking, "先看看仓库\n")
	}
	// 工具调用：name + arguments（原样 JSON 字符串）+ callId，结果按 callId 回填。
	if len(res.Tools) != 1 {
		t.Fatalf("Tools = %v want 1 条", res.Tools)
	}
	tc := res.Tools[0]
	if tc.Name != "bash" || tc.ID != "c1" {
		t.Errorf("ToolCall = %+v want name=bash id=c1", tc)
	}
	if tc.Args != `{"command":"ls"}` {
		t.Errorf("ToolCall.Args = %q want {\"command\":\"ls\"}", tc.Args)
	}
	if tc.Result != "a.txt\n" {
		t.Errorf("ToolCall.Result = %q want a.txt\\n", tc.Result)
	}

	mu.Lock()
	defer mu.Unlock()
	var kinds []StreamEventKind
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	want := []StreamEventKind{KindThinking, KindToolUse, KindToolResult, KindText, KindTurnEnd}
	if strings.Join(kindsToStrings(kinds), ",") != strings.Join(kindsToStrings(want), ",") {
		t.Errorf("事件序列 = %v want %v", kinds, want)
	}
	// KindToolUse 必须带 name/id/args（消费方靠它渲染工具卡片）。
	for _, ev := range events {
		if ev.Kind == KindToolUse {
			if ev.Name != "bash" || ev.ID != "c1" || ev.Text != `{"command":"ls"}` {
				t.Errorf("KindToolUse 载荷不全: %+v", ev)
			}
		}
		if ev.Kind == KindToolResult && ev.ID != "c1" {
			t.Errorf("KindToolResult 应带关联 callId: %+v", ev)
		}
		if ev.Kind == KindTurnEnd && ev.SessionID != res.SessionID {
			t.Errorf("KindTurnEnd 应带会话锚点: got %q want %q", ev.SessionID, res.SessionID)
		}
	}
}

// 别的会话的事件不混进本轮（协议原文：session.event 覆盖运行时内**每个**会话）。
func TestDshSDKIgnoresOtherSessions(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	events := `        printf '%s\n' '{"jsonrpc":"2.0","method":"session.event","params":{"sessionId":"别人的会话","event":{"type":"assistant/message","data":{"turn":1,"step":1,"message":{"role":"assistant","content":[{"type":"text","text":"外来正文"}]}}}}}'
        printf '%s\n' '{"jsonrpc":"2.0","method":"session.event","params":{"sessionId":"别人的会话","event":{"type":"tool/call","data":{"turn":1,"step":1,"callId":"x9","name":"bash","arguments":"{}"}}}}'
        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.event\",\"params\":{\"sessionId\":\"$sid\",\"event\":{\"type\":\"assistant/message\",\"data\":{\"turn\":$turn,\"step\":1,\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"我的正文\"}]}}}}}"
        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.event\",\"params\":{\"sessionId\":\"$sid\",\"event\":{\"type\":\"turn/end\",\"data\":{\"turn\":$turn,\"reason\":{\"kind\":\"completed\"}}}}}"`
	bin, _, _ := dshSDKStub(t, dir, dshSDKStubOpts{PromptEvents: events})

	res, err := (&DshEngine{BinPath: bin}).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "我的正文" {
		t.Errorf("Text = %q want 我的正文（外来会话的正文不该混入）", res.Text)
	}
	if len(res.Tools) != 0 {
		t.Errorf("Tools = %v want 空（外来会话的工具调用不该混入）", res.Tools)
	}
}

// initialize 被服务端明确拒绝（模型没配）→ **真失败**，不静默回退 headless。
func TestDshSDKInitializeRejectedIsFatal(t *testing.T) {
	dshSDKHome(t, "modelverse", "no-such-model")
	dir := t.TempDir()
	bin, argsLog, _ := dshSDKStub(t, dir, dshSDKStubOpts{
		InitResponse: `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"pi-ai provider \"modelverse\" has no configured model \"no-such-model\""}}`,
	})

	_, err := (&DshEngine{BinPath: bin}).Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("initialize 被拒应报错")
	}
	for _, want := range []string{"initialize 被拒绝", "no configured model", "modelverse"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应包含 %q: %v", want, err)
		}
	}
	// 关键：不能换个通道把错误吞掉（换了就是静默改模型）。
	if args := readFileString(t, argsLog); strings.Contains(args, "headless") {
		t.Errorf("服务端拒绝时不该回退 headless: %q", args)
	}
}

// SDK profile 起不来（旧版 dsh 没这个 profile）→ 回退 headless，并把代价写进 stderr。
func TestDshSDKUnavailableFallsBackToHeadless(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, argsLog, _ := dshSDKStub(t, dir, dshSDKStubOpts{
		SilentSDK:    true,
		HeadlessBody: "echo 'headless 正文'",
	})

	old := stderr
	var buf strings.Builder
	stderr = &buf
	defer func() { stderr = old }()

	resp, err := (&DshEngine{BinPath: bin}).Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "headless 正文" {
		t.Errorf("Text = %q want headless 正文（应回退 headless）", resp.Text)
	}
	args := readFileString(t, argsLog)
	if !strings.Contains(args, "--profile sdk") || !strings.Contains(args, "--profile headless") {
		t.Errorf("应先试 sdk 再回退 headless: %q", args)
	}
	if warn := buf.String(); !strings.Contains(warn, "SDK 通道不可用") {
		t.Errorf("回退应有明确告警: %q", warn)
	}
}

// MaxTokens 在 SDK 通道上真的生效（initialize 的 maxTokens），headless 上没有这个能力。
func TestDshSDKMaxTokensForwarded(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, _, stdinLog := dshSDKStub(t, dir, dshSDKStubOpts{})

	if _, err := (&DshEngine{BinPath: bin}).Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 4096,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	init := readFileString(t, stdinLog)
	if !strings.Contains(init, `"maxTokens":4096`) {
		t.Errorf("initialize 应带 maxTokens:4096: %q", init)
	}
	if !strings.Contains(init, `"provider":"modelverse"`) || !strings.Contains(init, `"model":"deepseek-v4.1-flash"`) {
		t.Errorf("initialize 应带配置层的 provider/model: %q", init)
	}
}

// -m 在 SDK 通道上**生效**（initialize 的 provider/model）→ 不该再打「不生效」告警。
func TestDshSDKModelTakesEffectWithoutWarning(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, _, stdinLog := dshSDKStub(t, dir, dshSDKStubOpts{})

	old := stderr
	var buf strings.Builder
	stderr = &buf
	defer func() { stderr = old }()

	resp, err := (&DshEngine{BinPath: bin}).Complete(context.Background(), Request{
		Model:    "modelverse/deepseek-v4-pro-0813",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Model != "modelverse/deepseek-v4-pro-0813" {
		t.Errorf("Model = %q want 本轮实际用的 modelverse/deepseek-v4-pro-0813", resp.Model)
	}
	if !strings.Contains(readFileString(t, stdinLog), `"model":"deepseek-v4-pro-0813"`) {
		t.Errorf("initialize 应带上 -m 指定的模型")
	}
	if buf.Len() != 0 {
		t.Errorf("SDK 通道下 -m 生效，不该有告警: %q", buf.String())
	}
}

// 一轮只有推理、没有正文 → 明确报错（不返回空 Text 让上层猜）。
func TestDshSDKEmptyTextIsError(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	events := `        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.event\",\"params\":{\"sessionId\":\"$sid\",\"event\":{\"type\":\"assistant/message\",\"data\":{\"turn\":$turn,\"step\":1,\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"reasoning\",\"text\":\"只想不写\"}]}}}}}"
        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.event\",\"params\":{\"sessionId\":\"$sid\",\"event\":{\"type\":\"turn/end\",\"data\":{\"turn\":$turn,\"reason\":{\"kind\":\"completed\"}}}}}"`
	bin, _, _ := dshSDKStub(t, dir, dshSDKStubOpts{PromptEvents: events})

	_, err := (&DshEngine{BinPath: bin}).Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "empty result") {
		t.Fatalf("无正文应报错, got %v", err)
	}
}

// 显式 Profile（自定义 profile / headless）→ 尊重调用方选择，不走 SDK。
func TestDshExplicitProfileSkipsSDK(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, argsLog, stdinLog := dshSDKStub(t, dir, dshSDKStubOpts{})

	for _, profile := range []string{dshDefaultProfile, "headless-cn"} {
		if _, err := (&DshEngine{BinPath: bin, Profile: profile}).Complete(context.Background(), Request{
			Messages: []Message{{Role: "user", Content: "hi"}},
		}); err != nil {
			t.Fatalf("profile=%s Complete: %v", profile, err)
		}
	}
	args := readFileString(t, argsLog)
	if strings.Contains(args, "--profile sdk") {
		t.Errorf("显式 profile 时不该起 SDK 通道: %q", args)
	}
	if !strings.Contains(args, "--profile headless") || !strings.Contains(args, "--profile headless-cn") {
		t.Errorf("应按显式 profile 启动: %q", args)
	}
	if _, err := os.Stat(stdinLog); err == nil {
		t.Error("没走 SDK 通道时不该有 JSON-RPC 请求")
	}
}

// sdkEnabled：默认与 "sdk" 走 SDK；其余（含 headless）走单 profile 路径。
func TestDshSDKEnabled(t *testing.T) {
	t.Setenv(DshProfileEnv, "")
	cases := []struct {
		profile string
		want    bool
	}{
		{"", true},
		{"sdk", true},
		{" sdk ", true},
		{"headless", false},
		{"headless-cn", false},
	}
	for _, c := range cases {
		if got := (&DshEngine{Profile: c.profile}).sdkEnabled(); got != c.want {
			t.Errorf("sdkEnabled(profile=%q) = %v want %v", c.profile, got, c.want)
		}
	}
}

// MAGIC_AGENT_DSH_PROFILE：不改代码就能切回 headless（SDK 协议是预发布，留的退路）。
func TestDshProfileEnvOverride(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, argsLog, stdinLog := dshSDKStub(t, dir, dshSDKStubOpts{})

	// 环境变量 = headless → 不走 SDK。
	t.Setenv(DshProfileEnv, "headless")
	resp, err := (&DshEngine{BinPath: bin}).Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "headless 正文" {
		t.Errorf("Text = %q want headless 正文", resp.Text)
	}
	if args := readFileString(t, argsLog); strings.Contains(args, "--profile sdk") {
		t.Errorf("env=headless 时不该起 SDK 通道: %q", args)
	}
	if _, err := os.Stat(stdinLog); err == nil {
		t.Error("没走 SDK 通道时不该有 JSON-RPC 请求")
	}

	// 显式字段优先于环境变量。
	if _, err := (&DshEngine{BinPath: bin, Profile: dshSDKProfile}).Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if args := readFileString(t, argsLog); !strings.Contains(args, "--profile sdk") {
		t.Errorf("显式 Profile=sdk 应压过 env: %q", args)
	}
}

// Profile 显式给 "sdk" 但 SDK 起不来 → 回退 `--profile headless`（不是把 sdk 再喂一遍）。
func TestDshSDKExplicitProfileFallsBackToHeadless(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, argsLog, _ := dshSDKStub(t, dir, dshSDKStubOpts{SilentSDK: true, HeadlessBody: "echo '回退正文'"})

	resp, err := (&DshEngine{BinPath: bin, Profile: dshSDKProfile}).Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "回退正文" {
		t.Errorf("Text = %q want 回退正文", resp.Text)
	}
	args := readFileString(t, argsLog)
	if !strings.Contains(args, "--profile headless") {
		t.Errorf("回退应用 headless（不是 sdk）: %q", args)
	}
	if strings.Count(args, "--profile sdk") != 1 {
		t.Errorf("sdk 只该试一次: %q", args)
	}
}

// dshResolveRoute：`route/model` / `dsh/route/model` / 裸 id / 空（回落配置层）。
func TestDshResolveRoute(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	cases := []struct {
		name      string
		reqModel  string
		wantProv  string
		wantModel string
	}{
		{"空 → 配置层", "", "modelverse", "deepseek-v4.1-flash"},
		{"ListModels 形态", "modelverse/deepseek-v4-pro-0813", "modelverse", "deepseek-v4-pro-0813"},
		{"带引擎前缀", "dsh/modelverse/deepseek-v4-pro-0813", "modelverse", "deepseek-v4-pro-0813"},
		{"裸 id → 用配置层的 provider", "deepseek-v4-pro-0813", "modelverse", "deepseek-v4-pro-0813"},
		{"引擎前缀 + 裸 id", "dsh/deepseek-v4-pro-0813", "modelverse", "deepseek-v4-pro-0813"},
	}
	for _, c := range cases {
		p, m := dshResolveRoute(c.reqModel)
		if p != c.wantProv || m != c.wantModel {
			t.Errorf("%s: dshResolveRoute(%q) = (%q,%q) want (%q,%q)",
				c.name, c.reqModel, p, m, c.wantProv, c.wantModel)
		}
	}
}

// 读不到 agent-default-model（provider/model 是 initialize 的必填项）→ 判为通道不可用 → 回退。
//
// 注意这条路上**不会**起 SDK 进程：provider/model 是 initialize 的必填项，拼不出请求
// 就没必要 spawn（也避免了「先起进程再立刻杀」）。
func TestDshSDKNoConfiguredRouteFallsBack(t *testing.T) {
	t.Setenv("DSH_HOME", t.TempDir()) // 没有 settings.yaml
	dir := t.TempDir()
	bin, argsLog, _ := dshSDKStub(t, dir, dshSDKStubOpts{HeadlessBody: "echo 'headless 兜底'"})

	old := stderr
	var buf strings.Builder
	stderr = &buf
	defer func() { stderr = old }()

	resp, err := (&DshEngine{BinPath: bin}).Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "headless 兜底" {
		t.Errorf("Text = %q want headless 兜底", resp.Text)
	}
	if args := readFileString(t, argsLog); strings.Contains(args, "--profile sdk") {
		t.Errorf("拼不出 initialize 参数时不该起 SDK 进程: %q", args)
	}
	if warn := buf.String(); !strings.Contains(warn, "SDK 通道不可用") {
		t.Errorf("回退应有明确告警: %q", warn)
	}
}

// 没有 turn/end 时用 session.status:idle 兜底收尾（协议里两条都可能只来一条）。
func TestDshSDKStatusIdleFallback(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	events := `        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.event\",\"params\":{\"sessionId\":\"$sid\",\"event\":{\"type\":\"assistant/message\",\"data\":{\"turn\":$turn,\"step\":1,\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"兜底正文\"}]}}}}}"
        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.status\",\"params\":{\"sessionId\":\"$sid\",\"status\":\"running\"}}"
        printf '%s\n' "{\"jsonrpc\":\"2.0\",\"method\":\"session.status\",\"params\":{\"sessionId\":\"$sid\",\"status\":\"idle\"}}"` // 故意不给 turn/end
	bin, _, _ := dshSDKStub(t, dir, dshSDKStubOpts{PromptEvents: events})

	resp, err := (&DshEngine{BinPath: bin}).Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "兜底正文" {
		t.Errorf("Text = %q want 兜底正文", resp.Text)
	}
}

// ctx 取消：立刻返回 ctx 错误，且收尾不卡（读线程不能卡在投递上）。
func TestDshSDKContextCancel(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	// PromptEvents = ":" 即「只回 prompt 响应，之后什么都不推」→ 模拟一直不结束的一轮。
	bin, _, _ := dshSDKStub(t, dir, dshSDKStubOpts{PromptEvents: ":"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := (&DshEngine{BinPath: bin}).Stream(ctx, Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil {
		t.Fatal("ctx 超时应报错")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("错误应体现 ctx 超时: %v", err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("收尾过慢（%v），读线程/进程收尾可能卡住", d)
	}
}

// 常驻会话（Request.Append）：同一进程内对同一 sessionId 继续 prompt 即续接同一会话。
//
// 这是 dsh 上唯一真正可用的续接形态 —— 官方 Python SDK 文档：「reuse a harness, home,
// and id only to continue the same durable conversation」。
func TestDshSDKAppendContinuesSameSession(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, _, stdinLog := dshSDKStub(t, dir, dshSDKStubOpts{})

	appendCh := make(chan string, 4)
	appendCh <- "第一轮追加"
	close(appendCh)

	var mu sync.Mutex
	var events []StreamEvent
	res, err := (&DshEngine{BinPath: bin}).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "看看目录"}},
		Append:   appendCh,
	}, func(ev StreamEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// 两次 session/prompt，且**同一个 sessionId**（换了 id 就是两个独立会话了）。
	lines := strings.Split(strings.TrimSpace(readFileString(t, stdinLog)), "\n")
	var promptSIDs []string
	for _, ln := range lines {
		if !strings.Contains(ln, "session/prompt") {
			continue
		}
		m := regexp.MustCompile(`"sessionId":"([^"]*)"`).FindStringSubmatch(ln)
		if len(m) != 2 {
			t.Fatalf("解析 sessionId 失败: %s", ln)
		}
		promptSIDs = append(promptSIDs, m[1])
	}
	if len(promptSIDs) != 2 {
		t.Fatalf("session/prompt 次数 = %d want 2（首轮 + 一条追加）", len(promptSIDs))
	}
	if promptSIDs[0] != promptSIDs[1] {
		t.Errorf("追加轮换了 sessionId（%q → %q）—— 那就不是同一会话了", promptSIDs[0], promptSIDs[1])
	}
	if res.SessionID != promptSIDs[0] {
		t.Errorf("Response.SessionID = %q want %q（--append <session_id> 要靠它定位）", res.SessionID, promptSIDs[0])
	}

	// 每轮一条 turn_end（常驻看门狗靠它判断轮次边界），共 2 条。
	mu.Lock()
	defer mu.Unlock()
	var turnEnds int
	for _, ev := range events {
		if ev.Kind == KindTurnEnd {
			turnEnds++
		}
	}
	if turnEnds != 2 {
		t.Errorf("KindTurnEnd = %d want 2（每轮一条）", turnEnds)
	}
	// 工具调用跨轮累加（与 claude/codebuddy 的 acc.Tools 同语义）。
	if len(res.Tools) != 2 {
		t.Errorf("Tools = %d want 2（每轮一次 bash）", len(res.Tools))
	}
	// 正文取**最后一轮**（与 claude/codebuddy 的 finalizeStreamText 同语义）。
	if res.Text != "目录里有 a.txt" {
		t.Errorf("Text = %q want 目录里有 a.txt", res.Text)
	}
	if strings.Count(res.Thinking, "先看看仓库") != 2 {
		t.Errorf("Thinking 应跨轮累加: %q", res.Text)
	}
}

// 两轮之间的 session.status:idle **不能**把常驻会话掐掉（那是轮次边界，不是会话终点）。
func TestDshSDKAppendSurvivesIdleBetweenRounds(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, _, stdinLog := dshSDKStub(t, dir, dshSDKStubOpts{}) // 默认事件里 turn/end 之后带 status:idle

	appendCh := make(chan string, 4)
	appendCh <- "再来一轮"
	close(appendCh)

	if _, err := (&DshEngine{BinPath: bin}).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Append:   appendCh,
	}, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := strings.Count(readFileString(t, stdinLog), "session/prompt"); got != 2 {
		t.Errorf("session/prompt = %d want 2（idle 不该让会话在第一轮后就收尾）", got)
	}
}

// 常驻会话需要 SDK 通道：通道不可用时**明确报错**，不静默把追加消息丢掉。
func TestDshSDKAppendRequiresSDKChannel(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, argsLog, _ := dshSDKStub(t, dir, dshSDKStubOpts{SilentSDK: true})

	appendCh := make(chan string, 1)
	close(appendCh)
	_, err := (&DshEngine{BinPath: bin}).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Append:   appendCh,
	}, nil)
	if err == nil {
		t.Fatal("SDK 通道不可用时，常驻会话应明确报错")
	}
	if !strings.Contains(err.Error(), "常驻会话") || !strings.Contains(err.Error(), "SDK 通道") {
		t.Errorf("错误应说明常驻会话需要 SDK 通道: %v", err)
	}
	// 关键：不能拿 headless 顶替（那会把追加消息静默丢掉）。
	if args := readFileString(t, argsLog); strings.Contains(args, "headless") {
		t.Errorf("常驻会话不该回退 headless: %q", args)
	}
}

// 追加消息为空/纯空白 → 不排新轮（避免空 prompt 被服务端拒）。
func TestDshSDKAppendSkipsBlank(t *testing.T) {
	dshSDKHome(t, "modelverse", "deepseek-v4.1-flash")
	dir := t.TempDir()
	bin, _, stdinLog := dshSDKStub(t, dir, dshSDKStubOpts{})

	appendCh := make(chan string, 4)
	appendCh <- "   "
	close(appendCh)

	if _, err := (&DshEngine{BinPath: bin}).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Append:   appendCh,
	}, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := strings.Count(readFileString(t, stdinLog), "session/prompt"); got != 1 {
		t.Errorf("session/prompt = %d want 1（空白追加不排新轮）", got)
	}
}

// ── 小工具 ────────────────────────────────────────────────────

func readFileString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	return string(b)
}

func kindsToStrings(kinds []StreamEventKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}
