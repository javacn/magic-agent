package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/darren/magic-agent/internal/agent"
)

// 用户提问落盘（kind:"user"）的端到端验证。
//
// 为什么值得一条端到端测试：这个 push 发生在 **session_id 还不知道**的时刻
//（引擎要到流里才报 id），所以它落盘的正确性完全依赖 sessionWriter 的 pending 缓冲
//「拿到 id 后先刷 pending、再写当前事件」这个顺序。单看代码容易以为要手动等 id ——
// 一旦谁把它改成「等 id 再 push」，就会把流路径堵住（违反「绝不断流」），或者顺序错乱。
//
// 这一条钉住三件事：
//  1. 用户提问**确实**落盘了（在此之前历史里只有 agent 的单方面发言）；
//  2. 它是**第一条**（排在引擎事件之前 —— 证明 pending 被先刷出）；
//  3. seq 从 1 起且严格递增（历史接口靠 seq 做游标，断了就续读不成）。

// userEchoStreamEngine 模拟真实引擎：在流**开始时**报出 session_id（真实引擎在 init 行里报），
// 然后发 thinking / text。这样 userEvent 的 push 必然早于 id 的出现。
type userEchoStreamEngine struct {
	name      string
	sessionID string
}

func (e *userEchoStreamEngine) Name() string           { return e.name }
func (e *userEchoStreamEngine) Detect() (bool, string) { return true, "fake://" + e.name }

func (e *userEchoStreamEngine) Complete(_ context.Context, _ agent.Request) (agent.Response, error) {
	return agent.Response{Text: "收到", Model: "fake-model"}, nil
}

func (e *userEchoStreamEngine) Stream(_ context.Context, req agent.Request, onEvent func(agent.StreamEvent)) (agent.StreamResult, error) {
	// 引擎流里见到 session_id 的那一刻 —— 这是 writer 开文件的触发点。
	if req.OnSessionID != nil {
		req.OnSessionID(e.sessionID)
	}
	onEvent(agent.StreamEvent{Kind: agent.KindThinking, Text: "想一下"})
	onEvent(agent.StreamEvent{Kind: agent.KindText, Text: "收到"})
	return agent.StreamResult{
		Response: agent.Response{Engine: e.name, Text: "收到", Model: "fake-model", SessionID: e.sessionID},
	}, nil
}

func TestStreamAskPersistsUserMessage(t *testing.T) {
	dir := t.TempDir()
	prev := SessionLogDirOverride
	SessionLogDirOverride = dir
	defer func() { SessionLogDirOverride = prev }()

	const sid = "sess-user-log"
	const prompt = "帮我把 guard 的令牌比较改成常数时间，避免时序侧信道"
	registerFake(&userEchoStreamEngine{name: "fake-user-log", sessionID: sid})

	if _, _, err := runAskCmd(t, "", "-e", "fake-user-log", "--stream", prompt); err != nil {
		t.Fatalf("--stream: %v", err)
	}

	// runStreamAsk 用 defer sessionLog.close() 收尾，所以命令返回后文件已 flush。
	evs, err := ReadSessionLog(dir, sid)
	if err != nil {
		t.Fatalf("读回会话日志失败：%v", err)
	}
	if len(evs) == 0 {
		t.Fatal("会话日志是空的 —— 用户提问没落盘")
	}

	// ① 首条必须是 user，且文本原样。
	if evs[0].Kind != "user" {
		t.Fatalf("首条期望 kind=user（用户提问应先于引擎事件落盘），实得 %q；全部：%s", evs[0].Kind, dumpKinds(evs))
	}
	if evs[0].Text != prompt {
		t.Fatalf("首条 user 文本被改动：\n期望 %q\n实得 %q", prompt, evs[0].Text)
	}
	if evs[0].SessionID != sid {
		t.Fatalf("首条应带上 session_id（pending 刷盘时回填），实得 %q", evs[0].SessionID)
	}

	// ② 引擎事件也都在，且排在 user 之后。
	sawText := false
	for i := 1; i < len(evs); i++ {
		if evs[i].Kind == "text" && evs[i].Text == "收到" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("引擎的 text 事件没落盘；全部：%s", dumpKinds(evs))
	}

	// ③ seq 从 1 起、严格递增（历史接口靠它做游标）。
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("第 %d 条 seq 期望 %d，实得 %d（seq 必须从 1 起连续递增）", i, i+1, ev.Seq)
		}
	}

	// ④ 落盘文件与 CLI 的 schema 一致：kind 字段（不是 type），这也是移动端渲染器
	//    必须两套都认的原因。
	raw := readLines(t, filepath.Join(dir, sid+".jsonl"))
	if len(raw) == 0 {
		t.Fatal("落盘文件是空的")
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(raw[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["kind"] != "user" {
		t.Fatalf("落盘首行的字段应是 kind=user，实得 %v", first)
	}
	if _, hasType := first["type"]; hasType {
		t.Fatal("落盘格式用的是 kind，不该出现 type 字段")
	}
}

func dumpKinds(evs []sessionEvent) string {
	out := ""
	for i, ev := range evs {
		if i > 0 {
			out += ", "
		}
		out += ev.Kind
	}
	return "[" + out + "]"
}
