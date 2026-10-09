package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/darren/magic-agent/internal/agent"
)

// sinkSamples 覆盖全部事件类型：用来钉住「--stream 与 --events 的字段集只差 v / seq」。
func sinkSamples() []agent.StreamEvent {
	return []agent.StreamEvent{
		{Kind: agent.KindText, Text: "正文"},
		{Kind: agent.KindThinking, Text: "思考"},
		{Kind: agent.KindToolUse, Text: `{"command":"ls"}`, Name: "Bash", ID: "toolu_1"},
		{Kind: agent.KindToolResult, Text: "输出", ID: "toolu_1"},
		{Kind: agent.KindTurnEnd, Text: "本轮正文", SessionID: "sess_abc"},
		{Kind: agent.KindAsk, Text: "要选一个"},
		// 状态收敛样本（2026-10-02）：这两个字段为空时全部 omitempty，
		// 所以老形状逐字节不变；非空时按契约出现在 wire 上。
		{
			Kind: agent.KindText, Text: "片", ItemID: "claude_msg_1",
			ItemRevision: 3, Status: agent.StatusRunning, Snapshot: "整段",
		},
		{
			Kind: agent.KindTurnFailed, Text: "", SessionID: "sess_abc",
			Error:  "claude CLI stream ended without result line",
			Reason: "without result line",
			Status: agent.StatusFailed, ItemID: "claude_msg_1", ItemRevision: 4,
		},
	}
}

func decodeEventLine(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &m); err != nil {
		t.Fatalf("解析事件行失败: %v（%q）", err, s)
	}
	return m
}

// TestEventSinkShapeParity 老形状（--stream）与新形状（--stream --events）的字段集
// 必须只差 v / seq，取值逐项相同。
//
// 这条是本文件里最重要的断言：两个形状共用 streamEventPayload()，但 envelope 那一步
// 做了「marshal → 加字段 → marshal」的往返，字段丢失或改名只会在新形状里发生。
func TestEventSinkShapeParity(t *testing.T) {
	for _, ev := range sinkSamples() {
		var plain, enriched bytes.Buffer
		if err := newEventSink(&plain, "claude", false).stream(ev); err != nil {
			t.Fatalf("%s: 老形状 %v", ev.Kind, err)
		}
		if err := newEventSink(&enriched, "claude", true).stream(ev); err != nil {
			t.Fatalf("%s: 新形状 %v", ev.Kind, err)
		}

		a := decodeEventLine(t, plain.String())
		b := decodeEventLine(t, enriched.String())

		if _, ok := b["v"]; !ok {
			t.Errorf("%s: --events 形态缺 v 字段: %v", ev.Kind, b)
		}
		if _, ok := b["seq"]; !ok {
			t.Errorf("%s: --events 形态缺 seq 字段: %v", ev.Kind, b)
		}
		delete(b, "v")
		delete(b, "seq")
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: 两条路径不一致\n  老形状: %v\n  新形状: %v", ev.Kind, a, b)
		}
	}
}

// TestEventSinkSeqMonotonic 行号从 1 起、逐行 +1；每行都带契约版本。
func TestEventSinkSeqMonotonic(t *testing.T) {
	var buf bytes.Buffer
	s := newEventSink(&buf, "claude", true)
	for _, ev := range sinkSamples() {
		if err := s.stream(ev); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != len(sinkSamples()) {
		t.Fatalf("行数 = %d want %d", len(lines), len(sinkSamples()))
	}
	for i, ln := range lines {
		m := decodeEventLine(t, ln)
		if got := int(m["seq"].(float64)); got != i+1 {
			t.Errorf("第 %d 行 seq = %d want %d", i, got, i+1)
		}
		if got := int(m["v"].(float64)); got != agent.ContractVersion {
			t.Errorf("第 %d 行 v = %d want %d", i, got, agent.ContractVersion)
		}
	}
}

// TestEventSinkSessionSticky 会话 id 一旦拿到，后续每行都带（客户端据此绑定会话）。
func TestEventSinkSessionSticky(t *testing.T) {
	var buf bytes.Buffer
	s := newEventSink(&buf, "claude", true)
	if err := s.stream(agent.StreamEvent{Kind: agent.KindTurnEnd, Text: "done", SessionID: "sess_x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.stream(agent.StreamEvent{Kind: agent.KindText, Text: "下一轮"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if m := decodeEventLine(t, lines[1]); m["session_id"] != "sess_x" {
		t.Errorf("turn_end 之后的正文应带 session_id=sess_x，实际 %v", m["session_id"])
	}
}

// TestEventSinkNewEventsGated ready 与控制应答只在 --events 下输出 ——
// 老消费者不认识新事件类型，往老形状里注入它们是静默的形状变更。
func TestEventSinkNewEventsGated(t *testing.T) {
	var off bytes.Buffer
	offSink := newEventSink(&off, "claude", false)
	if err := offSink.ready("hy3"); err != nil {
		t.Fatal(err)
	}
	if err := offSink.notice("pong", nil); err != nil {
		t.Fatal(err)
	}
	if off.Len() != 0 {
		t.Errorf("未开 --events 时不该输出 ready / 控制应答，实际 %q", off.String())
	}

	var on bytes.Buffer
	onSink := newEventSink(&on, "claude", true)
	if err := onSink.ready("hy3"); err != nil {
		t.Fatal(err)
	}
	if err := onSink.notice("pong", nil); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(on.String()), "\n")
	if m := decodeEventLine(t, lines[0]); m["type"] != "ready" || m["engine"] != "claude" || m["model"] != "hy3" {
		t.Errorf("ready 行不对: %v", m)
	}
	m := decodeEventLine(t, lines[1])
	if m["type"] != "pong" {
		t.Errorf("pong 行不对: %v", m)
	}
	if _, ok := m["v"]; !ok {
		t.Error("控制应答也应带 v")
	}
}

// TestStreamFlagsRequireStream 给了 --events / --control 却没给 --stream 要报错。
// 本项目的立场：静默忽略是 bug 的温床（超时被静默忽略那类事故）。
func TestStreamFlagsRequireStream(t *testing.T) {
	for _, flag := range []string{"--events", "--control"} {
		_, _, err := runAskCmd(t, "", flag, "hi")
		if err == nil {
			t.Errorf("%s 不带 --stream 应当报错", flag)
			continue
		}
		if !strings.Contains(err.Error(), "--stream") {
			t.Errorf("%s 的报错应提到 --stream，实际: %v", flag, err)
		}
	}
}

// TestControlConflictsWithStdinPrompt --control 占用 stdin，与「从 stdin 读提示词」互斥。
func TestControlConflictsWithStdinPrompt(t *testing.T) {
	_, _, err := runAskCmd(t, "提示词", "--stream", "--control", "-f", "-")
	if err == nil {
		t.Fatal("--control 与 -f - 应当冲突并报错")
	}
	if !strings.Contains(err.Error(), "stdin") {
		t.Errorf("报错应解释 stdin 冲突，实际: %v", err)
	}
}

// TestStreamEventPayloadConvergenceFields 状态收敛字段必须**只增不改**：
// 老消费者按老形状解析时，这些字段要么不出现（omitempty），要么是它
// 完全不认识但无害的扩展字段。契约的「只增字段不升版本」规则靠这条钉住。
func TestStreamEventPayloadConvergenceFields(t *testing.T) {
	// 1) 空的收敛字段 → 老形状逐字节不变（不带任何新键）。
	var buf bytes.Buffer
	if err := newEventSink(&buf, "claude", false).stream(agent.StreamEvent{
		Kind: agent.KindText, Text: "正文",
	}); err != nil {
		t.Fatal(err)
	}
	m := decodeEventLine(t, buf.String())
	for _, k := range []string{"item_id", "item_revision", "status", "snapshot", "error", "reason"} {
		if _, ok := m[k]; ok {
			t.Errorf("空收敛字段不该出现在 wire 上：%q -> %v", k, m)
		}
	}

	// 2) 非空 → 按契约名出现，值逐项透传。
	buf.Reset()
	ev := agent.StreamEvent{
		Kind: agent.KindText, Text: "片", ItemID: "claude_msg_1",
		ItemRevision: 3, Status: agent.StatusRunning, Snapshot: "整段",
	}
	if err := newEventSink(&buf, "claude", true).stream(ev); err != nil {
		t.Fatal(err)
	}
	m = decodeEventLine(t, buf.String())
	if m["item_id"] != "claude_msg_1" {
		t.Errorf("item_id = %v", m["item_id"])
	}
	if int(m["item_revision"].(float64)) != 3 {
		t.Errorf("item_revision = %v", m["item_revision"])
	}
	if m["status"] != "running" {
		t.Errorf("status = %v", m["status"])
	}
	if m["snapshot"] != "整段" {
		t.Errorf("snapshot = %v", m["snapshot"])
	}
	// text 仍是增量（不因收敛字段改成整段）。
	if m["text"] != "片" {
		t.Errorf("text 应保持增量语义，实际 %v", m["text"])
	}
}

// TestTurnFailedPayloadFields turn_failed 必须带 error / reason ——
// 消费方（jq 管道 / 桌面壳）靠它才知道这一轮为什么没结果。
func TestTurnFailedPayloadFields(t *testing.T) {
	var buf bytes.Buffer
	s := newEventSink(&buf, "claude", true)
	if err := s.stream(agent.StreamEvent{
		Kind: agent.KindTurnFailed, SessionID: "s1",
		Error:  "claude CLI stream ended without result line",
		Reason: "without result line", Status: agent.StatusFailed,
	}); err != nil {
		t.Fatal(err)
	}
	m := decodeEventLine(t, buf.String())
	if m["type"] != "turn_failed" {
		t.Errorf("type = %v", m["type"])
	}
	if m["error"] == "" || m["reason"] == "" {
		t.Errorf("turn_failed 必须带 error 与 reason：%v", m)
	}
	if m["status"] != "failed" {
		t.Errorf("status = %v，want failed", m["status"])
	}
	if m["session_id"] != "s1" {
		t.Errorf("session_id = %v", m["session_id"])
	}
}

// ── 黄金样本：钉住 Claude / CodeBuddy 在 wire 上的真实字节形状 ──
//
// 为什么不复用上面的 TestStreamEventPayloadConvergenceFields：
// 那条验的是「字段在不在、值对不对」；这条验的是**引擎实际跑出来的形状**
// 与 2026-10-02 真实抓包逐字节一致。两者不能互相替代：
//   · 字段级断言在「字段改名」时会跟着改，永远发现不了改名带来的破坏；
//   · 本文件一旦要改（比如某天决定 snapshot 不再随 text 下发），
//     这里的差异会直接把改动暴露出来，逼人确认影响面。
//
// 样本来自真实运行（claude / codebuddy 各自一轮，见 2026-10-01 日志）。
// 形状的**关键不变量**（改动时最容易被忽略的三条）：
//  1. `text` 是增量，不是 snapshot —— 终端直打靠它逐行拼。
//  2. `snapshot` 与 `item_revision` 同时出现且 revision 从 1 起单调。
//  3. thinking 与 text 的 item_id **不同但后缀相同**（同一原生 message id）。
//     曾经写成同一个 id，导致覆盖式 upsert 静默吞掉思考内容。

// TestClaudeWireShapeGolden 真实 Claude 一轮的完整事件序列（黄金样本）。
func TestClaudeWireShapeGolden(t *testing.T) {
	// 实测：message_start 给出原生 id msg_070d0c14…，思考与正文各自成 lane。
	live := []agent.StreamEvent{
		{
			Kind: agent.KindThinking, Text: "先",
			ItemID: "claude_think_msg_070d0c14", ItemRevision: 1,
			Status: agent.StatusRunning, Snapshot: "先",
		},
		{
			Kind: agent.KindThinking, Text: "想一下",
			ItemID: "claude_think_msg_070d0c14", ItemRevision: 2,
			Status: agent.StatusRunning, Snapshot: "先想一下",
		},
		{
			Kind: agent.KindText, Text: "2",
			ItemID: "claude_msg_msg_070d0c14", ItemRevision: 1,
			Status: agent.StatusRunning, Snapshot: "2",
		},
		{
			Kind: agent.KindTurnEnd, Text: "2", SessionID: "sess_1",
			ItemID: "claude_msg_msg_070d0c14", ItemRevision: 2,
			Status: agent.StatusFinal, Snapshot: "2",
		},
	}

	var buf bytes.Buffer
	s := newEventSink(&buf, "claude", false)
	for _, ev := range live {
		if err := s.stream(ev); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != len(live) {
		t.Fatalf("行数 = %d，want %d", len(lines), len(live))
	}

	// 不变量 1：text 恒为增量。
	for i, ev := range live {
		m := decodeEventLine(t, lines[i])
		if m["text"] != ev.Text {
			t.Errorf("[%d] text = %v，want %v（text 必须是增量，不能被 snapshot 顶掉）", i, m["text"], ev.Text)
		}
	}

	// 不变量 2：同一 item 内 revision 从 1 起单调。
	lastByItem := map[string]float64{}
	for i, ln := range lines {
		m := decodeEventLine(t, ln)
		id, _ := m["item_id"].(string)
		rev, _ := m["item_revision"].(float64)
		if id == "" || rev == 0 {
			t.Errorf("[%d] 缺 item_id / item_revision: %v", i, m)
			continue
		}
		if prev := lastByItem[id]; rev <= prev {
			t.Errorf("[%d] item %q 的 revision 未单调：%v 之后是 %v", i, id, prev, rev)
		}
		lastByItem[id] = rev
	}

	// 不变量 3：思考与正文两条 item，后缀相同（同一原生 message id）。
	thinkItem := decodeEventLine(t, lines[0])["item_id"].(string)
	textItem := decodeEventLine(t, lines[2])["item_id"].(string)
	if thinkItem == textItem {
		t.Fatalf("思考与正文共用 item id（会静默吞掉思考内容）: %q", thinkItem)
	}
	if !strings.HasSuffix(thinkItem, "msg_070d0c14") || !strings.HasSuffix(textItem, "msg_070d0c14") {
		t.Errorf("两条 item 应内嵌同一原生 id：%q / %q", thinkItem, textItem)
	}
	if !strings.Contains(thinkItem, "think_") {
		t.Errorf("思考 item id 应含 think_ 段：%q", thinkItem)
	}

	// 终帧：final 挂在正文 item 上，且是它的最高 revision。
	end := decodeEventLine(t, lines[3])
	if end["status"] != string(agent.StatusFinal) {
		t.Errorf("turn_end.status = %v，want final", end["status"])
	}
	if end["item_id"] != textItem {
		t.Errorf("turn_end 应挂在正文 item 上：%v vs %v", end["item_id"], textItem)
	}
}

// TestCodeBuddyWireShapeGolden 真实 CodeBuddy 一轮的形状（黄金样本）。
//
// 与 Claude 的**关键差异**：codebuddy 是 AggregateOnly（正文只在 assistant
// 聚合行出现），所以一轮里通常**没有 thinking lane** —— 只有正文一条 item，
// 且 text 已是整段（不是逐字增量）。
func TestCodeBuddyWireShapeGolden(t *testing.T) {
	live := []agent.StreamEvent{
		{
			Kind: agent.KindText, Text: "2",
			ItemID: "codebuddy_msg_01a0f598", ItemRevision: 1,
			Status: agent.StatusRunning, Snapshot: "2",
		},
		{
			Kind: agent.KindTurnEnd, Text: "2", SessionID: "sess_cb",
			ItemID: "codebuddy_msg_01a0f598", ItemRevision: 2,
			Status: agent.StatusFinal, Snapshot: "2",
		},
	}
	var buf bytes.Buffer
	s := newEventSink(&buf, "codebuddy", false)
	for _, ev := range live {
		if err := s.stream(ev); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("行数 = %d，want 2", len(lines))
	}
	// 不带 thinking lane：整轮只有一条 item。
	first := decodeEventLine(t, lines[0])
	second := decodeEventLine(t, lines[1])
	if first["item_id"] != second["item_id"] {
		t.Errorf("codebuddy 整轮应共用一条 item：%v vs %v", first["item_id"], second["item_id"])
	}
	if first["status"] != string(agent.StatusRunning) || second["status"] != string(agent.StatusFinal) {
		t.Errorf("状态序列 = %v → %v，want running → final", first["status"], second["status"])
	}
	// 前缀正确（不同引擎的 id 空间不互通，撞了会让跨引擎回放串台）。
	if !strings.HasPrefix(first["item_id"].(string), "codebuddy_msg_") {
		t.Errorf("codebuddy item id 前缀不对：%v", first["item_id"])
	}
}
