package cli

import (
	"encoding/json"
	"testing"

	"github.com/darren/magic-agent/internal/agent"
)

/* 工具子形态上 wire 的证据（2026-09-29）。
 *
 * 为什么值得单独钉一条：这个字段是**两个界面**（旧工作台 / PC 基础对话 UI）选卡的唯一依据，
 * 而它只在 tool_use 事件上出现。没有这条断言，某天有人在 payload 里漏带它，
 * 表现是「工具卡悄悄退回按名字猜」—— 界面上不报错，只是慢慢和新引擎对不上。 */
func TestStreamPayloadCarriesToolKind(t *testing.T) {
	// ① 工具调用：带上判定结果
	p := streamEventPayload(agent.StreamEvent{Kind: agent.KindToolUse, Name: "Bash", Text: `{"cmd":"ls"}`})
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["tool_kind"] != agent.ToolKindCommand {
		t.Errorf("tool_use 的 tool_kind = %v, want %q（payload: %s）", got["tool_kind"], agent.ToolKindCommand, b)
	}

	// ② MCP 的名字带命名空间前缀，仍然要判成 mcp
	b2, _ := json.Marshal(streamEventPayload(agent.StreamEvent{Kind: agent.KindToolUse, Name: "mcp__github__create_issue"}))
	var got2 map[string]any
	_ = json.Unmarshal(b2, &got2)
	if got2["tool_kind"] != agent.ToolKindMCP {
		t.Errorf("mcp 工具的 tool_kind = %v, want %q", got2["tool_kind"], agent.ToolKindMCP)
	}

	// ③ 工具**结果**不带：它的 Name 放的是关联的 tool_use.id，拿来判形态会把 id 当工具名
	b3, _ := json.Marshal(streamEventPayload(agent.StreamEvent{Kind: agent.KindToolResult, Name: "toolu_01abc", Text: "ok"}))
	var got3 map[string]any
	_ = json.Unmarshal(b3, &got3)
	if _, has := got3["tool_kind"]; has {
		t.Errorf("tool_result 不该带 tool_kind：%s", b3)
	}

	// ④ 非工具事件：字段整个省掉（老消费者的字段集不受影响）
	b4, _ := json.Marshal(streamEventPayload(agent.StreamEvent{Kind: agent.KindText, Text: "你好"}))
	var got4 map[string]any
	_ = json.Unmarshal(b4, &got4)
	if _, has := got4["tool_kind"]; has {
		t.Errorf("text 事件不该带 tool_kind：%s", b4)
	}
}

/* 会话日志与 SSE 用**同一个**判定：两边画出的卡才会是同一张。
 * 这里钉住日志事件的字段（history 回放走的就是它）。 */
func TestSessionEventCarriesToolKind(t *testing.T) {
	ev := agent.StreamEvent{Kind: agent.KindToolUse, Name: "Edit", Text: `{"file":"a.go"}`}
	se := sessionEvent{
		Kind: string(ev.Kind), Text: ev.Text, Name: ev.Name, ID: ev.ID,
		ToolKind: toolKindOfEvent(ev),
	}
	if se.ToolKind != agent.ToolKindFileChange {
		t.Errorf("日志里的 tool_kind = %q, want %q", se.ToolKind, agent.ToolKindFileChange)
	}
	b, err := json.Marshal(se)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["tool_kind"] != agent.ToolKindFileChange {
		t.Errorf("日志事件的 tool_kind = %v, want %q（%s）", got["tool_kind"], agent.ToolKindFileChange, b)
	}
	// 非工具事件不带这个键（omitempty）
	b2, _ := json.Marshal(sessionEvent{Kind: "text", Text: "hi"})
	var got2 map[string]any
	_ = json.Unmarshal(b2, &got2)
	if _, has := got2["tool_kind"]; has {
		t.Errorf("text 日志事件不该带 tool_kind：%s", b2)
	}
}
