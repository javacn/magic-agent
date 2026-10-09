package agent

// codebuddy_stream_test.go - codebuddy 专属流式协议差异的回归测试（2026-10-01）。
//
// codebuddy 与 claude 同源但**协议有两处不同**，两处都只表现为
// 「输出为空 / 输出里混进英文推理」，不报错、退出码 0：
//
//  1. 不发 content_block_delta：正文只出现在 assistant 聚合行里。
//     不开 AggregateOnly → `-o text` 实时输出全程空白。
//  2. 思维链以 <think>…</think> 塞进正文 result 字符串。
//     不剥 → 用户看到的回答前面挂着一段英文推理。
//
// 两个开关都是"多发/少发"级别的错，所以必须有断言钉住。

import (
	"strings"
	"testing"
)

// TestAggregateOnlyEmitsFromAssistantLine AggregateOnly 引擎的正文必须实时出来。
func TestAggregateOnlyEmitsFromAssistantLine(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"cb-1","model":"hy3"}`,
		`{"type":"assistant","session_id":"cb-1","message":{"role":"assistant","content":[{"type":"text","text":"你好"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"你好","session_id":"cb-1"}`,
	}
	acc := &streamAccumulator{Engine: "codebuddy", AggregateOnly: true}
	evs := feedNDJSON(t, acc, lines)

	var texts []string
	for _, ev := range evs {
		if ev.Kind == KindText {
			texts = append(texts, ev.Text)
		}
	}
	if len(texts) != 1 || texts[0] != "你好" {
		t.Fatalf("正文事件 = %q，want 恰好一条「你好」", texts)
	}
	if acc.Text.String() != "你好" {
		t.Fatalf("acc.Text = %q", acc.Text.String())
	}
}

// TestAggregateOnlyOffDoesNotDuplicate 非 AggregateOnly 引擎（claude/trae）
// 绝不能从聚合行再发一遍正文 —— 增量已经在 stream_event 里发过了。
//
// 这条是「AggregateOnly 必须由引擎显式声明、不能全局打开」的护栏。
func TestAggregateOnlyOffDoesNotDuplicate(t *testing.T) {
	lines := []string{
		// 增量路径（claude 正常形态）
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}}`,
		// 同一轮的聚合行
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"你好"}]}}`,
	}
	acc := &streamAccumulator{Engine: "claude"} // AggregateOnly 缺省 false
	evs := feedNDJSON(t, acc, lines)

	n := 0
	for _, ev := range evs {
		if ev.Kind == KindText {
			n++
			if ev.Text != "你好" {
				t.Fatalf("正文事件 = %q", ev.Text)
			}
		}
	}
	if n != 1 {
		t.Fatalf("正文事件条数 = %d，want 1（开了就重复输出了）", n)
	}
	if acc.Text.String() != "你好" {
		t.Fatalf("acc.Text = %q，want 只有一份", acc.Text.String())
	}
}

// TestAggregateOnlyStripsThinkFromLiveOutput 实时输出里不能出现思维链。
func TestAggregateOnlyStripsThinkFromLiveOutput(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":"<think>模型在推理英文</think>\n最终答案是 42"}}`,
	}
	acc := &streamAccumulator{Engine: "codebuddy", AggregateOnly: true}
	evs := feedNDJSON(t, acc, lines)

	var joined string
	for _, ev := range evs {
		if ev.Kind == KindText {
			joined += ev.Text
		}
	}
	if strings.Contains(joined, "<think>") || strings.Contains(joined, "模型在推理英文") {
		t.Fatalf("实时输出里混进了思维链：%q", joined)
	}
	if !strings.Contains(joined, "最终答案是 42") {
		t.Fatalf("正文丢了：%q", joined)
	}
}

// TestAggregateOnlySkipsToolBlocks 聚合行里的工具块不能被当正文发出来。
func TestAggregateOnlySkipsToolBlocks(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"内部推理"},` +
		`{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}},` +
		`{"type":"text","text":"命令已执行"}` +
		`]}}`
	acc := &streamAccumulator{Engine: "codebuddy", AggregateOnly: true}
	evs := feedNDJSON(t, acc, []string{line})

	var joined string
	for _, ev := range evs {
		if ev.Kind == KindText {
			joined += ev.Text
		}
	}
	if joined != "命令已执行" {
		t.Fatalf("正文 = %q，want 只有「命令已执行」", joined)
	}
}

// TestTurnEndSanitized 回归（2026-10-01 真实 E2E 抓到）：
// `-o json` 的 NDJSON 里 text 干净、turn_end 却夹着 <think>…</think>。
//
// 根因：turn_end 的 Text 直接取 result 行的**原始** result 字段
// （runStreamJSONIn 里 acc.emitTurnEnd(fin.Result, ...)），而同一轮的
// text / result 都过了 stripThinkBlock。三个来源干净程度不一致。
// 消费方把 turn_end 当轮次边界，推理就会被当成正文收进去。
func TestTurnEndSanitized(t *testing.T) {
	raw := `<think>The user asks… reasoning…</think>` + "\n\n最终答案"
	acc := &streamAccumulator{Engine: "codebuddy", SanitizeTurnText: stripThinkBlock}
	acc.OnEvent = func(ev StreamEvent) {
		if ev.Kind != KindTurnEnd {
			t.Fatalf("不该有别的 kind：%v", ev.Kind)
		}
		if strings.Contains(ev.Text, "<think>") || strings.Contains(ev.Text, "reasoning") {
			t.Fatalf("turn_end 仍带思维链：%q", ev.Text)
		}
		if ev.Text != "最终答案" {
			t.Fatalf("turn_end = %q，want「最终答案」", ev.Text)
		}
	}
	acc.emitTurnEnd(raw, "s-1")
}

// TestTurnEndUntouchedWhenNoSanitizer 没声明清洗器的引擎（claude）必须原样透传。
//
// 挡的是「在 emitTurnEnd 里对所有引擎无脑套 think 剥离」：
// 那样会切掉合法输出里恰好出现的 <think> 字面量。
func TestTurnEndUntouchedWhenNoSanitizer(t *testing.T) {
	const raw = "正常正文，含 <think> 字面量"
	acc := &streamAccumulator{Engine: "claude"} // SanitizeTurnText 为 nil
	got := ""
	acc.OnEvent = func(ev StreamEvent) { got = ev.Text }
	acc.emitTurnEnd(raw, "s-1")
	if got != raw {
		t.Fatalf("claude 的 turn_end 被改了：%q，want %q", got, raw)
	}
}

// TestAssistantLineTextShapes assistantLineText 的两种 content 形态。
func TestAssistantLineTextShapes(t *testing.T) {
	if got := assistantLineText(`{"message":{"content":"纯字符串"}}`); got != "纯字符串" {
		t.Errorf("字符串形态 = %q", got)
	}
	if got := assistantLineText(`{"message":{"content":[]}}`); got != "" {
		t.Errorf("空数组 = %q", got)
	}
	if got := assistantLineText(`{}`); got != "" {
		t.Errorf("无 content = %q", got)
	}
	if got := assistantLineText(`not json`); got != "" {
		t.Errorf("非 JSON = %q", got)
	}
	// 多段 text 用换行连接（不用空格：正文里空格可能有语义）。
	if got := assistantLineText(`{"message":{"content":[{"type":"text","text":"A"},{"type":"text","text":"B"}]}}`); got != "A\nB" {
		t.Errorf("多段拼接 = %q，want A\\nB", got)
	}
}

// TestStripThinkBlock 思维链剥离的边界行为。
func TestStripThinkBlock(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"无标签原样", "普通正文", "普通正文"},
		{"前置思维链被剥", "<think>abc</think>正文", "正文"},
		{"多段只留最后一个之后", "<think>a</think>中<think>b</think>尾", "尾"},
		{"只有思维链返回空", "<think>只有推理</think>", ""},
		{"未闭合不动", "<think>没闭合", "<think>没闭合"},
		{"空串", "", ""},
		{"闭标签无开标签", "</think>孤立", "</think>孤立"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripThinkBlock(c.in); got != c.want {
				t.Errorf("stripThinkBlock(%q) = %q，want %q", c.in, got, c.want)
			}
		})
	}
}
