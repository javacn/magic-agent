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
