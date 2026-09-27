package agent

// openclaw_acp_test.go - openclaw 流式（ACP）通道的单测：**假 CLI 回放**，不碰真 Gateway。
//
// 假 openclaw 是一个交互式 shell 脚本：从 stdin 逐行读 JSON-RPC 帧、按 method 回响应，
// 并把收到的每一帧追加到 stdin.log 供断言（「客户端到底发了什么」也要钉住 ——
// 会话 key 是经 `_meta.sessionKey` 传的，这一条不测就等于没接线）。
//
// 覆盖：
//  ① 握手与事件映射（banner 噪声不打断 / 正文增量 / tool_call / tool_call_update / stopReason）
//  ② 会话锚点：新会话自造 key、已给的 key 原样透传、老 uuid 经 sessions 反查成 key
//  ③ exec 审批：execute 类拒绝、read 类放行（含 MAGIC_AGENT_OPENCLAW_ACP_APPROVE=all 覆盖）
//  ④ 桥不可用（Gateway 没起 / scope 未批）→ 自动回退内嵌一次性调用（正文补发成一条 text 增量）
//  ⑤ SupportsStream / --engines 的 streaming 标记

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// useTempBreaker 把冷却标记文件指到临时路径，返回该路径。
// 每个用例各自隔离：既不互相影响，也不受本机真实冷却文件影响。
func useTempBreaker(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "openclaw-acp-broken.json")
	t.Setenv("MAGIC_AGENT_OPENCLAW_ACP_BREAKER", p)
	return p
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// acpStubOpts 造桩时可调的几处。
type acpStubOpts struct {
	// permKind session/request_permission 里那条工具调用的 kind（决定我们放行还是拒绝）。
	permKind string
	// bridgeFail 为真时：只往 stderr 打「gateway connect failed…」并 exit 1（模拟桥连不上）。
	bridgeFail bool
	// sessionsJSON `openclaw sessions --json` 的返回（老 uuid → key 反查用）；空则不接管该子命令。
	sessionsJSON string
}

// acpStub 写一个假 openclaw 到临时目录，返回可执行路径与 stdin 日志路径。
func acpStub(t *testing.T, o acpStubOpts) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "openclaw")
	logPath = filepath.Join(dir, "stdin.log")

	perm := `printf '%s\n' '{"jsonrpc":"2.0","id":99,"method":"session/request_permission","params":{"sessionId":"acp-sid-1","toolCall":{"toolCallId":"tc-2","title":"Bash","kind":"` + o.permKind + `"},"options":[{"optionId":"allow-1","name":"允许","kind":"allow_once"},{"optionId":"reject-1","name":"拒绝","kind":"reject_once"}]}}'`

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("LOG=" + logPath + "\n")
	if o.sessionsJSON != "" {
		b.WriteString("case \"$*\" in\n  *sessions*) printf '%s\\n' '" + o.sessionsJSON + "'; exit 0 ;;\nesac\n")
	}
	// 内嵌回退路径（Complete）：`agent --local --agent main --json …`
	b.WriteString("case \"$*\" in\n  *agent*--json*) printf '%s\\n' '{\"payloads\":[{\"text\":\"内嵌回退的正文\",\"mediaUrl\":null}],\"meta\":{\"agentMeta\":{\"sessionId\":\"sid-fallback\",\"model\":\"m-fb\",\"usage\":{\"input\":1,\"output\":2,\"total\":3}}}}'; exit 0 ;;\nesac\n")
	if o.bridgeFail {
		b.WriteString("printf 'spawn\\n' >> " + filepath.Join(dir, "spawn.log") + "\n")
		b.WriteString("echo 'gateway connect failed: GatewayClientRequestError: scope upgrade pending approval' >&2\n")
		b.WriteString("echo 'ACP bridge failed: scope upgrade pending approval' >&2\n")
		b.WriteString("exit 1\n")
	}
	// ACP 会话：噪声 banner（非 JSON 帧）+ 逐帧应答
	b.WriteString("printf '%s\\n' '◇  Doctor warnings ───────────────────────────'\n")
	b.WriteString("while IFS= read -r line; do\n")
	b.WriteString("  printf '%s\\n' \"$line\" >> \"$LOG\"\n")
	b.WriteString("  case \"$line\" in\n")
	b.WriteString("    *'\"method\":\"initialize\"'*) printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":1,\"agentCapabilities\":{}}}' ;;\n")
	b.WriteString("    *'\"method\":\"session/new\"'*) printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"sessionId\":\"acp-sid-1\"}}' ;;\n")
	b.WriteString("    *'\"method\":\"session/prompt\"'*)\n")
	b.WriteString("      printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"sessionId\":\"acp-sid-1\",\"update\":{\"sessionUpdate\":\"agent_message_chunk\",\"content\":{\"type\":\"text\",\"text\":\"逐字\"}}}}'\n")
	b.WriteString("      printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"sessionId\":\"acp-sid-1\",\"update\":{\"sessionUpdate\":\"agent_message_chunk\",\"content\":{\"type\":\"text\",\"text\":\"增量\"}}}}'\n")
	b.WriteString("      printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"sessionId\":\"acp-sid-1\",\"update\":{\"sessionUpdate\":\"tool_call\",\"toolCallId\":\"tc-1\",\"title\":\"Bash\",\"kind\":\"read\",\"rawInput\":{\"command\":\"echo hi\"}}}}'\n")
	b.WriteString("      printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"sessionId\":\"acp-sid-1\",\"update\":{\"sessionUpdate\":\"tool_call_update\",\"toolCallId\":\"tc-1\",\"status\":\"completed\",\"rawOutput\":\"hi\"}}}'\n")
	// 同一次调用再推一条（真机实测会收到 content 与 rawOutput 两条）→ 客户端必须去重
	b.WriteString("      printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"sessionId\":\"acp-sid-1\",\"update\":{\"sessionUpdate\":\"tool_call_update\",\"toolCallId\":\"tc-1\",\"status\":\"completed\",\"content\":[{\"type\":\"content\",\"content\":{\"type\":\"text\",\"text\":\"hi(dup)\"}}]}}}'\n")
	b.WriteString("      not-json-noise\n")
	b.WriteString("      " + perm + "\n")
	b.WriteString("      printf '%s\\n' '{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"stopReason\":\"end_turn\"}}' ;;\n")
	b.WriteString("  esac\n")
	b.WriteString("done\n")
	if err := os.WriteFile(bin, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

// readStubLog 读回桩记录到的所有请求帧。
func readStubLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读桩日志失败: %v", err)
	}
	return string(b)
}

func TestOpenClawStreamACP_MapsEventsAndCarriesSessionKey(t *testing.T) {
	useTempBreaker(t)
	bin, logPath := acpStub(t, acpStubOpts{permKind: "read"})

	var got []StreamEvent
	res, err := (&OpenClawEngine{BinPath: bin}).Stream(context.Background(),
		Request{Messages: []Message{{Role: "user", Content: "看看目录"}}},
		func(ev StreamEvent) { got = append(got, ev) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// ① 事件映射：两条正文增量 → tool_use → tool_result（噪声行与 banner 都被跳过）
	kinds := []StreamEventKind{}
	for _, ev := range got {
		kinds = append(kinds, ev.Kind)
	}
	want := []StreamEventKind{KindText, KindText, KindToolUse, KindToolResult}
	if len(kinds) != len(want) {
		t.Fatalf("事件数 = %d（%v），期望 %d：%v", len(kinds), kinds, len(want), kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("第 %d 个事件类型 = %s，期望 %s（全部：%v）", i, kinds[i], want[i], kinds)
		}
	}
	if got[0].Text != "逐字" || got[1].Text != "增量" {
		t.Errorf("正文增量不对: %q %q", got[0].Text, got[1].Text)
	}
	if got[2].Name != "read" || got[2].ID != "tc-1" || !strings.Contains(got[2].Text, "echo hi") {
		t.Errorf("tool_use 事件不对: %+v", got[2])
	}
	if got[3].ID != "tc-1" || got[3].Text != "hi" {
		t.Errorf("tool_result 事件不对: %+v", got[3])
	}

	// ② 收尾：正文拼接 / 工具记录 / 会话锚点是**我们造的 key**（不是桥的 acp-sid-1）
	if res.Text != "逐字增量" {
		t.Errorf("Text = %q", res.Text)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "read" || res.Tools[0].Result != "hi" {
		t.Errorf("Tools = %+v（同一次调用只该有一条记录）", res.Tools)
	}
	if !strings.HasPrefix(res.SessionID, "agent:main:acp-") {
		t.Errorf("SessionID = %q，期望自造的 session key（前缀 agent:main:acp-）", res.SessionID)
	}
	if res.SessionID == "acp-sid-1" {
		t.Errorf("SessionID 不该用桥随机生成的 acp sessionId（出了进程就没意义）")
	}
	if res.Model != "" {
		t.Errorf("Model = %q（ACP 侧给不出模型；没指定主选时就是空，界面按「引擎默认」显示）", res.Model)
	}

	// ③ 客户端到底发了什么：协议版本 / 会话 key 经 _meta 传 / 提示词进了 prompt 块
	log := readStubLog(t, logPath)
	if !strings.Contains(log, `"protocolVersion":1`) {
		t.Errorf("initialize 未带协议版本：%s", log)
	}
	if !strings.Contains(log, `"_meta":{"sessionKey":"agent:main:acp-`) {
		t.Errorf("session/new 未把自造 key 经 _meta.sessionKey 传给桥：%s", log)
	}
	if !strings.Contains(log, `"sessionId":"acp-sid-1"`) || !strings.Contains(log, `看看目录`) {
		t.Errorf("session/prompt 载荷不对：%s", log)
	}
	// ④ 审批：read 类 → 必须回「选 allow-1」（不是拒绝、也不是不回）
	if !strings.Contains(log, `"optionId":"allow-1"`) {
		t.Errorf("read 类审批未放行：%s", log)
	}
}

func TestOpenClawStreamACP_KeepsGivenSessionKey(t *testing.T) {
	useTempBreaker(t)
	bin, logPath := acpStub(t, acpStubOpts{permKind: "read"})
	const key = "agent:main:acp-existing-key"

	res, err := (&OpenClawEngine{BinPath: bin}).Stream(context.Background(),
		Request{SessionID: key, Messages: []Message{{Role: "user", Content: "接着说"}}},
		func(StreamEvent) {})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.SessionID != key {
		t.Errorf("SessionID = %q，期望原样回传 %q（续接靠它）", res.SessionID, key)
	}
	if !strings.Contains(readStubLog(t, logPath), `"_meta":{"sessionKey":"`+key+`"`) {
		t.Errorf("已给的 key 未原样透传")
	}
}

func TestOpenClawStreamACP_MapsLegacyIDToSessionKey(t *testing.T) {
	useTempBreaker(t)
	// 老的非流式路径返回的是 Gateway **session id**（裸 uuid）：必须用 sessions 反查成 key，
	// 否则同一轮对话的上下文会在升级后静默丢掉。
	const legacy = "97579a21-58f0-4574-a8fd-93fb95d8711b"
	bin, logPath := acpStub(t, acpStubOpts{
		permKind:     "read",
		sessionsJSON: `{"sessions":[{"sessionId":"` + legacy + `","key":"agent:main:keep","model":"m","updatedAt":1}]}`,
	})

	res, err := (&OpenClawEngine{BinPath: bin}).Stream(context.Background(),
		Request{SessionID: legacy, Messages: []Message{{Role: "user", Content: "接着说"}}},
		func(StreamEvent) {})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.SessionID != "agent:main:keep" {
		t.Errorf("SessionID = %q，期望反查到的 key agent:main:keep", res.SessionID)
	}
	if !strings.Contains(readStubLog(t, logPath), `"_meta":{"sessionKey":"agent:main:keep"`) {
		t.Errorf("反查到的 key 未透传给桥")
	}
}

func TestOpenClawStreamACP_DeniesExecuteByDefault(t *testing.T) {
	useTempBreaker(t)
	bin, logPath := acpStub(t, acpStubOpts{permKind: "execute"})
	if _, err := (&OpenClawEngine{BinPath: bin}).Stream(context.Background(),
		Request{Messages: []Message{{Role: "user", Content: "删掉临时文件"}}},
		func(StreamEvent) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	log := readStubLog(t, logPath)
	if !strings.Contains(log, `"optionId":"reject-1"`) {
		t.Errorf("execute 类审批**必须**默认拒绝（不许替用户扩大权限）：%s", log)
	}
	if strings.Contains(log, `"optionId":"allow-1"`) {
		t.Errorf("execute 类不该出现放行：%s", log)
	}
}

func TestOpenClawStreamACP_ApproveAllOverride(t *testing.T) {
	useTempBreaker(t)
	t.Setenv("MAGIC_AGENT_OPENCLAW_ACP_APPROVE", "all")
	bin, logPath := acpStub(t, acpStubOpts{permKind: "execute"})
	if _, err := (&OpenClawEngine{BinPath: bin}).Stream(context.Background(),
		Request{Messages: []Message{{Role: "user", Content: "删掉临时文件"}}},
		func(StreamEvent) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !strings.Contains(readStubLog(t, logPath), `"optionId":"allow-1"`) {
		t.Errorf("设了 MAGIC_AGENT_OPENCLAW_ACP_APPROVE=all 就该全放行")
	}
}

func TestOpenClawStreamACP_FallsBackWhenBridgeUnavailable(t *testing.T) {
	useTempBreaker(t)
	bin, _ := acpStub(t, acpStubOpts{bridgeFail: true})

	var got []StreamEvent
	res, err := (&OpenClawEngine{BinPath: bin}).Stream(context.Background(),
		Request{Messages: []Message{{Role: "user", Content: "你好"}}},
		func(ev StreamEvent) { got = append(got, ev) })
	if err != nil {
		t.Fatalf("桥不可用时应回退而不是报错：%v", err)
	}
	// 回退走 Complete（内嵌一次性调用）：正文来自那份 envelope，并以**一条 text 增量**补发出去，
	// 这样调用方（对话渲染）拿到的仍是「有正文的一轮」，只是没有逐字。
	if res.Text != "内嵌回退的正文" {
		t.Errorf("Text = %q（应来自内嵌 envelope）", res.Text)
	}
	if res.SessionID != "sid-fallback" {
		t.Errorf("SessionID = %q（回退路径用内嵌的 session_id）", res.SessionID)
	}
	if len(got) != 1 || got[0].Kind != KindText || got[0].Text != "内嵌回退的正文" {
		t.Errorf("回退未把正文补发成一条 text 增量：%+v", got)
	}
}

func TestOpenClawStreamACP_ModelForcesNonStream(t *testing.T) {
	useTempBreaker(t)
	// ACP 桥不暴露模型选择：指定了模型就必须走嵌套路（`agent --model` 忠实生效），
	// 否则用户选的模型被静默换掉 —— 比没有逐字增量糟糕得多。
	bin, logPath := acpStub(t, acpStubOpts{permKind: "read"})

	var got []StreamEvent
	res, err := (&OpenClawEngine{BinPath: bin}).Stream(context.Background(),
		Request{Model: "minimax/MiniMax-M3", Messages: []Message{{Role: "user", Content: "你好"}}},
		func(ev StreamEvent) { got = append(got, ev) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "内嵌回退的正文" {
		t.Errorf("Text = %q（指定模型时应走内嵌 envelope）", res.Text)
	}
	if len(got) != 1 || got[0].Kind != KindText {
		t.Errorf("应把正文补发成一条 text 增量：%+v", got)
	}
	if _, statErr := os.Stat(logPath); statErr == nil {
		t.Errorf("指定模型时不该起 ACP 桥（桩不该收到任何 ACP 帧）")
	}
}

func TestOpenClawStreamACP_BreakerSkipsRespawnAcrossRounds(t *testing.T) {
	// 为什么是**文件**而不是进程内变量：上层一轮一个 CLI 进程（实测进程内冷却两轮都照样 spawn）。
	breaker := useTempBreaker(t)
	bin, _ := acpStub(t, acpStubOpts{bridgeFail: true})
	spawnLog := filepath.Join(filepath.Dir(bin), "spawn.log") // 桩自己记的「被起过几次」

	e := &OpenClawEngine{BinPath: bin}
	req := Request{Messages: []Message{{Role: "user", Content: "你好"}}}
	for i := 0; i < 2; i++ {
		if _, err := e.Stream(context.Background(), req, func(StreamEvent) {}); err != nil {
			t.Fatalf("第 %d 轮失败：%v", i+1, err)
		}
	}
	raw, rerr := os.ReadFile(spawnLog)
	if rerr != nil {
		t.Fatalf("桩没被起过？: %v", rerr)
	}
	if n := strings.Count(string(raw), "spawn"); n != 1 {
		t.Errorf("两轮里桥被起了 %d 次，期望 1 次（第二轮应被冷却挡住、直接走嵌套路）", n)
	}
	if _, statErr := os.Stat(breaker); statErr != nil {
		t.Errorf("冷却标记文件没落盘：%v", statErr)
	}
	// 冷却过期（把 until 拨到过去）→ 应重新尝试桥
	_ = os.WriteFile(breaker, []byte(`{"until":1}`), 0o644)
	if _, err := e.Stream(context.Background(), req, func(StreamEvent) {}); err != nil {
		t.Fatalf("冷却过期后的一轮失败：%v", err)
	}
	raw2, _ := os.ReadFile(spawnLog)
	if n := strings.Count(string(raw2), "spawn"); n != 2 {
		t.Errorf("冷却过期后应重试一次（累计 2 次），实测 %d 次", n)
	}
}

func TestOpenClawStreamACP_BreakerSkipsEvenWhenBridgeWouldWork(t *testing.T) {
	// 冷却期内**即便桥其实能跑通**也不再 spawn（跨进程省时间；到点自动重试，见上一个用例）。
	breaker := useTempBreaker(t)
	_ = os.WriteFile(breaker, []byte(`{"until":`+itoa(time.Now().Add(time.Hour).Unix())+`}`), 0o644)
	bin, logPath := acpStub(t, acpStubOpts{permKind: "read"})

	res, err := (&OpenClawEngine{BinPath: bin}).Stream(context.Background(),
		Request{Messages: []Message{{Role: "user", Content: "你好"}}},
		func(StreamEvent) {})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "内嵌回退的正文" {
		t.Errorf("Text = %q（冷却期内应直接走嵌套路）", res.Text)
	}
	if _, statErr := os.Stat(logPath); statErr == nil {
		t.Errorf("冷却期内不该驱动 ACP 桥（桩不该收到帧）")
	}
}

func TestOpenClawSupportsStream(t *testing.T) {
	e := &OpenClawEngine{}
	if !SupportsStream(e) {
		t.Fatal("openclaw 应报告支持流式（`--engines` 的 streaming 字段由它决定）")
	}
	if got := streamArgsFor(e); len(got) != 1 || got[0] != "acp" {
		t.Errorf("流式标记 = %v，期望 [acp]", got)
	}
}

func TestACPPermissionPolicy(t *testing.T) {
	t.Setenv("MAGIC_AGENT_OPENCLAW_ACP_APPROVE", "")
	cases := map[string]string{
		"read": "allow", "search": "allow", "fetch": "allow", "think": "allow", "other": "allow", "": "allow",
		"execute": "reject", "edit": "reject", "delete": "reject", "move": "reject",
		"EXECUTE": "reject", // 大小写不敏感
	}
	for kind, want := range cases {
		if got := acpPermissionPolicy(kind); got != want {
			t.Errorf("acpPermissionPolicy(%q) = %q，期望 %q", kind, got, want)
		}
	}
	t.Setenv("MAGIC_AGENT_OPENCLAW_ACP_APPROVE", "all")
	if got := acpPermissionPolicy("execute"); got != "allow" {
		t.Errorf("APPROVE=all 时 execute 应放行，实测 %q", got)
	}
}

// TestStreamAvailableBreaker 流式可用性要跟着「已知失败」的冷却走：
// 冷却中 → false（调用方不传 --stream、界面不画流式标记）；过期 / 无文件 / 其他引擎 → true。
func TestStreamAvailableBreaker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "breaker.json")
	t.Setenv("MAGIC_AGENT_OPENCLAW_ACP_BREAKER", path)

	if !StreamAvailable("openclaw") {
		t.Fatalf("无标记文件时 openclaw 应可用")
	}
	// 冷却中（until 在未来）
	if err := os.WriteFile(path, []byte(`{"until":`+strconv.FormatInt(time.Now().Add(3*time.Minute).Unix(), 10)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if StreamAvailable("openclaw") {
		t.Errorf("冷却中 openclaw 不该报可用（否则界面会画流式标记、还会白等一次桥失败）")
	}
	// 过期（until 在过去）
	if err := os.WriteFile(path, []byte(`{"until":`+strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !StreamAvailable("openclaw") {
		t.Errorf("冷却过期后应恢复可用")
	}
	// 其他引擎不受这个文件影响
	if !StreamAvailable("claude") || !StreamAvailable("arkclaw") {
		t.Errorf("别的引擎不该被 openclaw 的冷却波及")
	}
}
