package agent

// stream_test.go - 流式调用测试。
//
// 用 fake CLI 脚本回放三家 CLI 的真实 NDJSON 样本（2026-09-15 实测抓包），
// 验证：
//	- claude/codebuddy：thinking_delta / text_delta 解析与转发
//	- trae：delta.content 直出增量解析
//	- result 收尾行：全文以 result.result 为准
//	- 流中断（无 result 行）→ 报错
//	- 超时杀进程组
//	- AsStreamer / SupportsStream

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// collectEvents 收集全部事件的辅助闭包。
func collectEvents() (*[]StreamEvent, func(StreamEvent)) {
	events := &[]StreamEvent{}
	return events, func(ev StreamEvent) { *events = append(*events, ev) }
}

// ── claude 流式 ───────────────────────────────────────────────

func TestClaudeStreamParsesThinkingAndText(t *testing.T) {
	// 样本精简自真实抓包：init → thinking 增量 → text 增量 → result。
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-1","model":"MiniMax-M3"}'
echo '{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"用户在做加法"}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"，答案是 2"}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_stop","index":0},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"2"}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"！"}},"session_id":"s-1"}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"2！","session_id":"s-1","model":"MiniMax-M3"}'
`)
	e := &ClaudeEngine{BinPath: cli}
	events, onEvent := collectEvents()

	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "1+1"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if res.Text != "2！" {
		t.Errorf("Text = %q, want 2！（以 result 行为准）", res.Text)
	}
	if res.Thinking != "用户在做加法，答案是 2" {
		t.Errorf("Thinking = %q", res.Thinking)
	}
	if res.SessionID != "s-1" {
		t.Errorf("SessionID = %q", res.SessionID)
	}
	if res.Model != "MiniMax-M3" {
		t.Errorf("Model = %q", res.Model)
	}

	// 事件顺序：2 thinking + 2 text
	var thinkCount, textCount int
	for _, ev := range *events {
		switch ev.Kind {
		case KindThinking:
			thinkCount++
		case KindText:
			textCount++
		}
	}
	if thinkCount != 2 || textCount != 2 {
		t.Errorf("events: thinking=%d text=%d, want 2/2", thinkCount, textCount)
	}
	if len(*events) > 0 && (*events)[0].Kind != KindThinking {
		t.Errorf("first event should be thinking, got %v", (*events)[0].Kind)
	}
}

// result.result 为空时回退到增量拼接的全文。
func TestClaudeStreamFallsBackToDeltas(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"增量全文"}}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":""}'
`)
	e := &ClaudeEngine{BinPath: cli}
	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "增量全文" {
		t.Errorf("Text = %q, want 增量全文", res.Text)
	}
}

// ── codebuddy 流式（同源协议）────────────────────────────────

func TestCodeBuddyStreamParsesDeltas(t *testing.T) {
	cli := writeFakeCLI(t, "codebuddy", `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"思考中"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"你好"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"世界"}}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"你好世界","model":"glm-5.3"}'
`)
	e := &CodeBuddyEngine{BinPath: cli}
	events, onEvent := collectEvents()

	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "你好世界" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Thinking != "思考中" {
		t.Errorf("Thinking = %q", res.Thinking)
	}
	if len(*events) != 3 {
		t.Errorf("events = %d, want 3", len(*events))
	}
}

// codebuddy 流式保留 user_query 回显剥离逻辑。
func TestCodeBuddyStreamStripsUserQueryEcho(t *testing.T) {
	cli := writeFakeCLI(t, "codebuddy", `#!/bin/sh
echo '{"type":"result","subtype":"success","is_error":false,"result":"<user_query>【用户】hi</user_query>真正回答"}'
`)
	e := &CodeBuddyEngine{BinPath: cli}
	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "真正回答" {
		t.Errorf("Text = %q, want 回显剥离", res.Text)
	}
}

// ── trae 流式 ─────────────────────────────────────────────────

func TestTraeStreamParsesDeltaContent(t *testing.T) {
	// 样本精简自真实抓包：delta.content 直出，无 thinking 通道。
	cli := writeFakeCLI(t, "trae-cli", `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"t-1"}'
echo '{"type":"stream_event","session_id":"t-1","delta":{"role":"assistant","content":"2"}}'
echo '{"type":"stream_event","session_id":"t-1","delta":{"role":"assistant","content":"就是这个"}}'
echo '{"type":"assistant","session_id":"t-1","message":{"role":"assistant","content":"2就是这个"}}'
echo '{"type":"result","subtype":"success","session_id":"t-1","result":"2就是这个","is_error":false}'
`)
	e := &TraeEngine{BinPath: cli}
	events, onEvent := collectEvents()

	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "1+1"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "2就是这个" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Thinking != "" {
		t.Errorf("trae 无 thinking 通道，Thinking 应为空，got %q", res.Thinking)
	}
	if len(*events) != 2 {
		t.Errorf("events = %d, want 2（assistant 聚合行不重复 emit）", len(*events))
	}
}

// ── 异常路径 ──────────────────────────────────────────────────

// 流中断（CLI 退出但无 result 行）→ 明确报错。
func TestStreamMissingResultLine(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半截"}}}'
`)
	e := &ClaudeEngine{BinPath: cli}
	_, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil {
		t.Fatal("expected error for missing result line")
	}
	if !strings.Contains(err.Error(), "without result") {
		t.Errorf("error should mention missing result: %v", err)
	}
}

// result is_error → 报错。
func TestStreamErrorResult(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"result","subtype":"error_max_turns","is_error":true,"result":"超出轮次"}'
`)
	e := &ClaudeEngine{BinPath: cli}
	_, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil {
		t.Fatal("expected error for is_error result")
	}
	if !strings.Contains(err.Error(), "error_max_turns") {
		t.Errorf("error should mention subtype: %v", err)
	}
}

// CLI 非零退出码 → 报错。
func TestStreamCliFailure(t *testing.T) {
	cli := writeFakeCLI(t, "claude", "#!/bin/sh\necho 'some error' >&2\nexit 7\n")
	e := &ClaudeEngine{BinPath: cli}
	_, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
}

// 超时 → 杀进程组并报错（子进程不残留）。
func TestStreamTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	cli := writeFakeCLI(t, "claude", "#!/bin/sh\nsleep 30 && touch "+marker+"\n")
	e := &ClaudeEngine{BinPath: cli}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := e.Stream(ctx, Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
	// marker 不应出现（sleep 30 未跑完就被杀）
	time.Sleep(400 * time.Millisecond)
	if _, merr := os.Stat(marker); merr == nil {
		t.Error("child process survived timeout (marker exists)")
	}
}

// 非流式引擎 → AsStreamer 返回 nil（防呆：counterEngine 不是 Streamer）。
func TestAsStreamerNonStreamer(t *testing.T) {
	e := &counterEngine{name: "x"}
	if s := AsStreamer(e); s != nil {
		t.Errorf("AsStreamer(non-streamer) = %v, want nil", s)
	}
	if SupportsStream(e) {
		t.Error("SupportsStream(non-streamer) should be false")
	}
	for _, name := range []string{"claude", "codebuddy", "codebuddy-ai", "trae", "llm"} {
		if !SupportsStream(Lookup(name)) {
			t.Errorf("SupportsStream(%s) should be true", name)
		}
	}
}

// ── 工具调用事件（claude / codebuddy stream-json 协议）──

// claude 流式应识别 tool_use / tool_result：args 由 input_json_delta 累积，
// 收尾 stop 时一次性 emit；result 的 text 字段作为 KindToolResult 透出。
//
// 注：fake CLI 走 shell 单引号 heredoc，反斜杠不被 shell 解释，但 Go 的
// JSON 解析器会把 \" 解析为字面的 \"（反斜杠 + 引号），不是真 JSON。
// 这正好覆盖「引擎吐出的是 JSON-escape 字符串」的真实场景。
func TestClaudeStreamParsesToolUseAndResult(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","name":"Bash","id":"toolu_01"}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls -la\"}"}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_stop","index":2},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_start","index":3,"content_block":{"type":"tool_result","tool_use_id":"toolu_01"}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"total 12\\ndrwxr-xr-x 3 user user 96 Sep 15 21:00 ."}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_stop","index":3},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_start","index":4,"content_block":{"type":"text","text":""}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"完成"},"session_id":"s-1"}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"完成","session_id":"s-1","model":"MiniMax-M3"}'
`)
	e := &ClaudeEngine{BinPath: cli}
	events, onEvent := collectEvents()

	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "看看目录"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "完成" {
		t.Errorf("Text = %q, want 完成", res.Text)
	}

	// 工具事件计数：tool_use 1 + tool_result 1。
	var toolUseCount, toolResultCount, textCount int
	for _, ev := range *events {
		switch ev.Kind {
		case KindToolUse:
			toolUseCount++
			if ev.Name != "Bash" {
				t.Errorf("tool_use.Name = %q, want Bash", ev.Name)
			}
			if ev.ID != "toolu_01" {
				t.Errorf("tool_use.ID = %q, want toolu_01", ev.ID)
			}
			if ev.Text != `{"command":"ls -la"}` {
				t.Errorf("tool_use.Text (args) = %q", ev.Text)
			}
		case KindToolResult:
			toolResultCount++
			if ev.ID != "toolu_01" {
				t.Errorf("tool_result.ID = %q, want toolu_01", ev.ID)
			}
			if !strings.Contains(ev.Text, "total 12") {
				t.Errorf("tool_result.Text = %q, want ls output", ev.Text)
			}
		case KindText:
			textCount++
		}
	}
	if toolUseCount != 1 {
		t.Errorf("toolUseCount = %d, want 1", toolUseCount)
	}
	if toolResultCount != 1 {
		t.Errorf("toolResultCount = %d, want 1", toolResultCount)
	}
	if textCount != 1 {
		t.Errorf("textCount = %d, want 1", textCount)
	}

	// 收尾汇总应带回工具调用。
	if len(res.Tools) != 1 {
		t.Fatalf("res.Tools = %d, want 1", len(res.Tools))
	}
	tc := res.Tools[0]
	if tc.Name != "Bash" || tc.ID != "toolu_01" {
		t.Errorf("Tools[0] = {Name:%q ID:%q}", tc.Name, tc.ID)
	}
	if tc.Args != `{"command":"ls -la"}` {
		t.Errorf("Tools[0].Args = %q", tc.Args)
	}
	if !strings.Contains(tc.Result, "total 12") {
		t.Errorf("Tools[0].Result = %q, want ls output", tc.Result)
	}
}

// 多次连续工具调用：args 累积在 stop 时一次性 emit；Tools 列表保持调用顺序。
func TestClaudeStreamMultipleToolCalls(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","name":"Read","id":"t1"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/a\"}"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_stop","index":2}}'
echo '{"type":"stream_event","event":{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","name":"Read","id":"t2"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\""}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"/b\"}"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_stop","index":3}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	e := &ClaudeEngine{BinPath: cli}
	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "读两个文件"}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("Tools = %d, want 2", len(res.Tools))
	}
	if res.Tools[0].ID != "t1" || res.Tools[0].Args != `{"path":"/a"}` {
		t.Errorf("Tools[0] = %+v", res.Tools[0])
	}
	if res.Tools[1].ID != "t2" || res.Tools[1].Args != `{"path":"/b"}` {
		t.Errorf("Tools[1] = %+v (args 应由两段 partial_json 拼接)", res.Tools[1])
	}
}

// trae 协议不暴露工具事件：即使用户开了 --tools on，流式输出也不带工具事件。
// 这是协议限制，不是解析缺陷。
func TestTraeStreamNoToolEvents(t *testing.T) {
	cli := writeFakeCLI(t, "trae-cli", `#!/bin/sh
echo '{"type":"stream_event","delta":{"role":"assistant","content":"正文"}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"正文"}'
`)
	e := &TraeEngine{BinPath: cli}
	events, onEvent := collectEvents()

	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	for _, ev := range *events {
		if ev.Kind == KindToolUse || ev.Kind == KindToolResult {
			t.Errorf("trae 不应发射工具事件, got %v", ev.Kind)
		}
	}
	if len(res.Tools) != 0 {
		t.Errorf("trae res.Tools 应为 nil/空, got %d", len(res.Tools))
	}
}

// 协议异常：open tool_use 没收到 stop 直接来 result → 兜底回收，不丢事件。
func TestClaudeStreamToolUseOrphanRecovery(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","name":"Edit","id":"t1"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}}'
# 没有 content_block_stop，直接 result → 触发 toolUseOrphanGuard 兜底。
echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'
`)
	e := &ClaudeEngine{BinPath: cli}
	events, onEvent := collectEvents()

	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var toolUseCount int
	for _, ev := range *events {
		if ev.Kind == KindToolUse {
			toolUseCount++
			if ev.Name != "Edit" || ev.Text != `{"x":1}` {
				t.Errorf("orphan tool_use = %+v", ev)
			}
		}
	}
	if toolUseCount != 1 {
		t.Errorf("toolUseCount = %d, want 1（孤儿回收）", toolUseCount)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "Edit" {
		t.Errorf("res.Tools = %+v", res.Tools)
	}
}

// ── user 块里的 tool_result（claude / codebuddy 真实协议）──

// claude 实测：tool_result 不走独立 content_block，而是嵌在 user 消息里。
// 本测试验证 handleUserToolResult 能补全 Result 字段，并 emit KindToolResult。
func TestClaudeStreamToolResultInUserBlock(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"WebSearch","id":"call_001"}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"x\"}"}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_stop","index":1},"session_id":"s-1"}'
echo '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"call_001","type":"tool_result","content":"搜索结果：14.05亿"}]},"session_id":"s-1"}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"正文"}'
`)
	e := &ClaudeEngine{BinPath: cli}
	events, onEvent := collectEvents()

	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "test"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// tool_use 事件（args 累积）。
	var toolUseCount, toolResultCount int
	for _, ev := range *events {
		switch ev.Kind {
		case KindToolUse:
			toolUseCount++
			if ev.Name != "WebSearch" || ev.ID != "call_001" {
				t.Errorf("tool_use = %+v", ev)
			}
		case KindToolResult:
			toolResultCount++
			if ev.ID != "call_001" {
				t.Errorf("tool_result.ID = %q", ev.ID)
			}
			if ev.Text != "搜索结果：14.05亿" {
				t.Errorf("tool_result.Text = %q", ev.Text)
			}
		}
	}
	if toolUseCount != 1 || toolResultCount != 1 {
		t.Errorf("counts: toolUse=%d toolResult=%d, want 1/1", toolUseCount, toolResultCount)
	}

	// Result 字段必须回填。
	if len(res.Tools) != 1 {
		t.Fatalf("res.Tools = %d, want 1", len(res.Tools))
	}
	if res.Tools[0].Result != "搜索结果：14.05亿" {
		t.Errorf("Tools[0].Result = %q, want 搜索结果：14.05亿", res.Tools[0].Result)
	}
}

// tool_result 的 content 是数组（嵌套 text/image 块）时，文本字段拼接。
func TestClaudeStreamToolResultArrayContent(t *testing.T) {
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"Read","id":"r1"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_stop","index":1}}'
echo '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"r1","type":"tool_result","content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}]}}'
echo '{"type":"stream_event","event":{"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"D"}}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"D"}'
`)
	e := &ClaudeEngine{BinPath: cli}
	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "x"}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(res.Tools) != 1 {
		t.Fatalf("Tools = %d", len(res.Tools))
	}
	if res.Tools[0].Result != "line1\nline2" {
		t.Errorf("Tools[0].Result = %q", res.Tools[0].Result)
	}
}

// ── runStreamCLI 进程退出与 reader 收尾的竞态回归 ──────────────
//
// 曾有的两类故障（同根因家族）：
//  1. select 走到 waitErr 分支（进程先于 reader 收尾退出）时若对该通道
//     二次接收——waitErr 只有一个发送方，二次接收必然永久阻塞（runtime
//     报 all goroutines are asleep）。
//  2. StdoutPipe 的读端被 cmd.Wait() 提前关闭：进程退出而 reader 尚有
//     管道积压时，余量被丢弃并报 "file already closed"，把成功的一次
//     流式调用变成失败。已改为自建管道（读端归 reader、Wait 不触碰）。
//
// 复现手段：用 yes|head 瞬时灌入 5 万行增量（写端远快于读端），脚本随后
// 立即退出——cmd.Wait() 稳定先于 reader 排空积压返回，确定性地走进
// waitErr 分支；修复后必须完整读到全部增量与收尾 result。
func TestStreamProcessExitBeforeReaderFinish(t *testing.T) {
	const bursts = 50000
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString(`yes '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}}' | head -n ` + strconv.Itoa(bursts) + "\n")
	script.WriteString(`echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'` + "\n")
	cli := writeFakeCLI(t, "claude", script.String())
	e := &ClaudeEngine{BinPath: cli}

	var events int
	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "x"}},
	}, func(StreamEvent) { events++ })
	if err != nil {
		t.Fatalf("Stream: %v（修复前此路径死锁或丢失尾部输出）", err)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q", res.Text)
	}
	if events < bursts {
		t.Errorf("events = %d, want >= %d（尾部增量被丢弃？）", events, bursts)
	}
}

// ── llm 引擎的流式测试见 llmengine_test.go（fake CLI 包装）──
