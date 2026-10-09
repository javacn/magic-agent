package agent

// stream_timeline_test.go - 状态收敛事件模型（2026-10-02）。
//
// 背景：magic-agent 原先的流式事件是**纯 delta 广播**（只说「又来了这几个字」），
// 消费方要自己拼、自己判断何时完整。对「终端直打」够用，但撑不起多端同步、
// 历史回放、断线重连 —— 那些场景里同一条消息被反复投递 / 乱序到达。
//
// 本文件把 agents-anywhere 的三个核心机制落地并钉住：
//  1. **稳定 item id**（引擎原生 message id 派生）—— 拿不到就不猜
//  2. **单调 revision + 整段 snapshot**（partial 是 running，final 沿用同 id 覆盖）
//  3. **终帧缺失必补 terminal 事件**（绝不让一轮悬空）
//
// 另外两条**不破坏既有行为**的护栏：
//   · Text 仍是增量片段（终端直打 / 老客户端逐行拼接不受影响）
//   · 拿不到原生 id 时降级到「本轮内稳定的序号 id」，不产生第二个 item

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// skipIfNoShCLI 端到端用例依赖 writeFakeCLI 写的 `#!/bin/sh` 脚本 ——
// Windows 上没有 shebang 解释器，子进程直接起不来（"executable file not
// found in %PATH%"）。整个 internal/agent 的 fake-CLI 测试在 Windows 上
// 都是这个下场（基线红的原因之一），所以这里显式跳过而不是让它红。
//
// ⚠️ 判断回归时**不能看失败数**，要跟 HEAD 基线做集合差 —— 跳过的用例在
// 两边都不产生失败行，看数字会以为"少测了"（见 MEMORY.md 的基线纪律）。
func skipIfNoShCLI(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("需要 #!/bin/sh 解释器；Windows 上 fake CLI 起不来（与本仓库其余 fake-CLI 测试同一限制）")
	}
}

// claudeRealSample 精简自 2026-10-02 真实抓包（claude stream-json）：
// message_start 带原生 id，该 id 贯穿后续 assistant 聚合行。
const claudeMsgStart = `{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_abc123"}}}`

// collector 挂一个 OnEvent 收集器，返回累积到的事件切片。
//
// 为什么要它（而不是直接用 ask_test.go 里的 feedNDJSON）：那个辅助函数
// 在调用**之后**才把事件返回，而下面几个用例要在 feed 之后**继续**调
// emitTurnEnd / emitTurnFailed 再看新产生的帧 —— 收集器必须贯穿始终。
type collector struct{ evs []StreamEvent }

func newCollector(acc *streamAccumulator) *collector {
	c := &collector{}
	acc.OnEvent = func(ev StreamEvent) { c.evs = append(c.evs, ev) }
	return c
}

// feedTo 把 NDJSON 行喂进累加器，并把产生的帧记进 c。
//
// ⚠️ 顺序很关键：必须**先** feed 再挂 collector —— feedNDJSON 自己会覆盖
// acc.OnEvent（它自己收集一份），先挂 collector 会被顶掉，
// 症状是 c.evs 恒为空（测试以「没事件」的形式失败，很容易误判成引擎没发）。
func feedTo(t *testing.T, acc *streamAccumulator, c *collector, lines []string) {
	t.Helper()
	got := feedNDJSON(t, acc, lines)
	acc.OnEvent = func(ev StreamEvent) { c.evs = append(c.evs, ev) }
	c.evs = append(c.evs, got...)
}

// TestStableItemIDFromNativeMessageID 原生 message id 派生出的 item id
// 必须贯穿本轮所有文本事件（partial 帧 + final 帧共用一个 id）。
func TestStableItemIDFromNativeMessageID(t *testing.T) {
	lines := []string{
		claudeMsgStart,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"你"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"好"}}}`,
		`{"type":"assistant","message":{"id":"msg_abc123","role":"assistant","content":[{"type":"text","text":"你好"}]}}`,
	}
	acc := &streamAccumulator{Engine: "claude"}
	evs := feedNDJSON(t, acc, lines)

	var ids []string
	for _, ev := range evs {
		if ev.Kind == KindText {
			ids = append(ids, ev.ItemID)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("text 事件数 = %d，want 2", len(ids))
	}
	if ids[0] != "claude_msg_msg_abc123" {
		t.Fatalf("item id = %q，want claude_msg_msg_abc123", ids[0])
	}
	if ids[0] != ids[1] {
		t.Fatalf("同一轮的 item id 必须稳定：%q vs %q", ids[0], ids[1])
	}
}

// TestItemIDFallbackIsStableNoGuessing 拿不到原生 id 时降级到「本轮内稳定的
// 序号 id」—— 关键是**不能**每帧一个新 id（否则一条消息被拆成 N 个 item，
// 前端会画出 N 个气泡）。
func TestItemIDFallbackIsStableNoGuessing(t *testing.T) {
	lines := []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"甲"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"乙"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"丙"}}}`,
	}
	acc := &streamAccumulator{Engine: "trae"}
	evs := feedNDJSON(t, acc, lines)

	first := ""
	for _, ev := range evs {
		if ev.ItemID == "" {
			t.Fatalf("降级也要给 id（否则消费方无法收敛）：%+v", ev)
		}
		if first == "" {
			first = ev.ItemID
		}
		if ev.ItemID != first {
			t.Fatalf("降级 id 不稳定：%q vs %q", first, ev.ItemID)
		}
	}
	if !strings.HasPrefix(first, "trae_msg_") {
		t.Errorf("降级 id 应带引擎前缀：%q", first)
	}
}

// TestRevisionMonotonic revision 在同一 item 内严格递增，且从 1 起。
// TestRevisionMonotonic revision 必须在**各自 lane 内**单调。
//
// 2026-10-02 修正：原先断言「一条消息内所有事件共用一个递增计数器」，
// 那是在 thinking/text 共享同一个 item 的前提下成立的。拆成两条 item
// （见 itemtracker.go itemLane）之后，两条 lane 各自从 1 开始，
// 跨 lane 比较 revision 是没有意义的 —— item_id 不同就不是同一条 item。
func TestRevisionMonotonic(t *testing.T) {
	lines := []string{
		claudeMsgStart,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"甲"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"乙"}}}`,
	}
	acc := &streamAccumulator{Engine: "claude"}
	evs := feedNDJSON(t, acc, lines)

	// 按 item id 分组，每组内 revision 必须 1,2,3... 严格递增。
	lastByItem := map[string]uint64{}
	n := 0
	for _, ev := range evs {
		if ev.ItemRevision == 0 {
			t.Fatalf("事件缺 revision：%+v", ev)
		}
		if ev.ItemID == "" {
			t.Fatalf("事件缺 item id：%+v", ev)
		}
		if last := lastByItem[ev.ItemID]; ev.ItemRevision <= last {
			t.Fatalf("item %q 的 revision 不单调：%d 之后是 %d", ev.ItemID, last, ev.ItemRevision)
		}
		lastByItem[ev.ItemID] = ev.ItemRevision
		n++
	}
	if n != 3 {
		t.Fatalf("事件数 = %d，want 3", n)
	}
	for item, last := range lastByItem {
		if last != 1 && last != 2 {
			t.Errorf("item %q 的末版 revision = %d，want 1（思考单帧）或 2（正文两帧）", item, last)
		}
	}
}

// TestSnapshotAccumulates snapshot 必须是**累积整段**（状态收敛），
// 而不是又一个增量片段 —— 这是消费方能只靠 upsert 就拼出全文的前提。
func TestSnapshotAccumulates(t *testing.T) {
	lines := []string{
		claudeMsgStart,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"你"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"好"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"呀"}}}`,
	}
	acc := &streamAccumulator{Engine: "claude"}
	evs := feedNDJSON(t, acc, lines)

	var snaps []string
	var deltas []string
	for _, ev := range evs {
		if ev.Kind != KindText {
			continue
		}
		snaps = append(snaps, ev.Snapshot)
		deltas = append(deltas, ev.Text)
	}
	want := []string{"你", "你好", "你好呀"}
	for i, s := range snaps {
		if s != want[i] {
			t.Fatalf("snapshot[%d] = %q，want %q", i, s, want[i])
		}
	}
	// Text 必须仍是增量（终端直打依赖它）。
	wantDelta := []string{"你", "好", "呀"}
	for i, d := range deltas {
		if d != wantDelta[i] {
			t.Fatalf("text[%d] = %q，want 增量 %q", i, d, wantDelta[i])
		}
	}
}

// TestSnapshotJoinsBlocksByIndex 一条消息里正文可能分散在多个 text 块，
// snapshot 必须按 content block index 升序拼接，而不是按到达顺序。
// 挡的是「先 append 到全局 buffer」这种实现：块乱序到达时会拼错。
func TestSnapshotJoinsBlocksByIndex(t *testing.T) {
	// 故意让 index=1 的块**先**到：按到达顺序拼会得到「B然后A」。
	lines := []string{
		claudeMsgStart,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"B"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}}`,
	}
	acc := &streamAccumulator{Engine: "claude"}
	evs := feedNDJSON(t, acc, lines)

	last := evs[len(evs)-1]
	if !strings.HasSuffix(last.Snapshot, "AB") {
		t.Fatalf("snapshot 应按 index 拼成 AB（index1 先到），实际 %q", last.Snapshot)
	}
}

// TestPartialThenFinalConverge partial（running）之后 final 用**同一个 id**
// + 更高 revision + final 状态覆盖。消费方只做 upsert 即可，
// 不需要「先删 partial 再插 final」这种有顺序要求的逻辑。
func TestPartialThenFinalConverge(t *testing.T) {
	acc := &streamAccumulator{Engine: "claude"}
	c := newCollector(acc)
	feedTo(t, acc, c, []string{
		claudeMsgStart,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"你好"}}}`,
	})
	acc.emitTurnEnd("你好（引擎权威值）", "s-1")

	var texts, finals []StreamEvent
	for _, ev := range c.evs {
		if ev.Kind == KindText {
			texts = append(texts, ev)
		}
		if ev.Kind == KindTurnEnd {
			finals = append(finals, ev)
		}
	}
	if len(texts) != 1 || len(finals) != 1 {
		t.Fatalf("events: text=%d turn_end=%d，want 1/1", len(texts), len(finals))
	}
	p, f := texts[0], finals[0]
	if p.ItemID != f.ItemID {
		t.Fatalf("final 必须沿用 partial 的 id：%q vs %q", p.ItemID, f.ItemID)
	}
	if f.ItemRevision <= p.ItemRevision {
		t.Fatalf("final revision 必须更高：partial=%d final=%d", p.ItemRevision, f.ItemRevision)
	}
	if p.Status != StatusRunning {
		t.Errorf("partial 状态 = %q，want running", p.Status)
	}
	if f.Status != StatusFinal {
		t.Errorf("final 状态 = %q，want final", f.Status)
	}
	// final 的 snapshot 用 result 行的权威正文。
	if f.Snapshot != "你好（引擎权威值）" {
		t.Errorf("final snapshot = %q，want result 行正文", f.Snapshot)
	}
}

// TestSecondMessageStartsNewItem 一轮里的**多条** assistant message
// 必须是**不同的 item**。
//
// 为什么：claude 一次工具调用后继续回答，每个 tool_result 之后都是一次新的
// message_start。共用一个 item 的话后一条覆盖前一条 —— 症状是
// 「对话里只剩最后一段回复，前面的全没了」，而且没有任何报错。
//
// agents-anywhere 同样在 message_start 里清空块桶并把 revision 归零
// （runtimes/claude/timeline/stream.py:35-36），本测试钉住这个行为。
func TestSecondMessageStartsNewItem(t *testing.T) {
	acc := &streamAccumulator{Engine: "claude"}
	evs := feedNDJSON(t, acc, []string{
		claudeMsgStart, // msg_abc123
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"第一段"}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_xyz789"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"第二段"}}}`,
	})
	if len(evs) != 2 {
		t.Fatalf("事件数 = %d，want 2", len(evs))
	}
	first, second := evs[0], evs[1]
	if first.ItemID == second.ItemID {
		t.Fatalf("两条 message 不该共用 item：%q", first.ItemID)
	}
	// revision 按 message 重置（与 agents-anywhere 归零同义），
	// 所以每条 item 内部才是 1,2,3… 的干净序列。
	if first.ItemRevision != 1 || second.ItemRevision != 1 {
		t.Errorf("各 message 的首个 revision 应为 1：%d / %d", first.ItemRevision, second.ItemRevision)
	}
	// snapshot 不能串：第二条只含自己的正文。
	if second.Snapshot != "第二段" {
		t.Errorf("第二条 snapshot 串了前一条的内容：%q", second.Snapshot)
	}
	if !strings.HasSuffix(first.ItemID, "msg_abc123") || !strings.HasSuffix(second.ItemID, "msg_xyz789") {
		t.Errorf("item id 未对应各自的原生 id：%q / %q", first.ItemID, second.ItemID)
	}
}

// TestFinalDoesNotTouchThinkLane final 只收尾**正文** lane。
//
// result 行里是整条消息的正文，**不含思考链**。若也给思考 lane 发 final，
// 消费方会收到一条「快照从「我在想」回退成空」的 final ——
// 比不收尾更糟（内容先涨后掉）。思考 lane 的终止由轮级终态信号
// （turn_end / turn_failed）表达，见 stream.go emitTurnEnd 的注释。
func TestFinalDoesNotTouchThinkLane(t *testing.T) {
	acc := &streamAccumulator{Engine: "claude"}
	c := newCollector(acc)
	feedTo(t, acc, c, []string{
		claudeMsgStart,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"我在想"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案"}}}`,
	})
	acc.emitTurnEnd("答案", "s-1")

	thinkItem, textItem := "", ""
	for _, ev := range c.evs {
		switch ev.Kind {
		case KindThinking:
			thinkItem = ev.ItemID
			if ev.Status != StatusRunning {
				t.Errorf("思考事件状态 = %q，want running", ev.Status)
			}
		case KindTurnEnd:
			textItem = ev.ItemID
		}
	}
	if thinkItem == "" || textItem == "" {
		t.Fatalf("没拿到两个 lane 的 item：think=%q text=%q", thinkItem, textItem)
	}
	if thinkItem == textItem {
		t.Fatalf("final 不该落到思考 lane 上：%q", thinkItem)
	}
	// 只有一条 turn_end，且它挂在正文 lane 上。
	var ends int
	for _, ev := range c.evs {
		if ev.Kind == KindTurnEnd {
			ends++
		}
	}
	if ends != 1 {
		t.Errorf("turn_end 条数 = %d，want 1（不为思考 lane 额外发一条）", ends)
	}
}

// TestUpsertIdempotent 同一 (id, revision) 重复投递不产生新版本 ——
// agents-anywhere 的幂等公式：revision = max(item, existing+1)。
// 我们在引擎侧就保证 revision 单调，消费方按 id 覆盖即可幂等；
// 这条测试钉住「重复消费同一条事件得到同样结果」这个可观察性质。
func TestUpsertIdempotent(t *testing.T) {
	acc := &streamAccumulator{Engine: "claude"}
	evs := feedNDJSON(t, acc, []string{
		claudeMsgStart,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"你好"}}}`,
	})

	// 模拟消费方的 upsert 表。
	type ver struct {
		rev  uint64
		body string
	}
	table := map[string]ver{}
	apply := func(ev StreamEvent) {
		old, ok := table[ev.ItemID]
		if ok && old.rev >= ev.ItemRevision {
			return // 重复或乱序的旧版本，直接丢弃
		}
		table[ev.ItemID] = ver{rev: ev.ItemRevision, body: ev.Snapshot}
	}
	for _, ev := range evs {
		if ev.Kind == KindText {
			apply(ev)
			apply(ev) // 重放同一帧
			apply(ev)
		}
	}
	if len(table) != 1 {
		t.Fatalf("upsert 表应只有 1 个 item，实际 %d", len(table))
	}
	for _, v := range table {
		if v.body != "你好" {
			t.Fatalf("重复 upsert 后正文 = %q，want 你好", v.body)
		}
	}
}

// TestTurnFailedTerminalFallback 终帧缺失必须补终态事件（对齐
// agents-anywhere 的 failed_terminal_event）。此前这条路径只有 return error，
// 于是已经流出去的半截正文在事件流上永远停在 running —— 一轮悬空。
func TestTurnFailedTerminalFallback(t *testing.T) {
	acc := &streamAccumulator{Engine: "claude"}
	c := newCollector(acc)
	feedTo(t, acc, c, []string{
		claudeMsgStart,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"半截"}}}`,
	})
	acc.emitTurnFailed("s-1", errNoResultLine("claude"))

	var fail *StreamEvent
	for i := range c.evs {
		if c.evs[i].Kind == KindTurnFailed {
			fail = &c.evs[i]
		}
	}
	if fail == nil {
		t.Fatal("终帧缺失时必须发 turn_failed")
	}
	if fail.Status != StatusFailed {
		t.Errorf("状态 = %q，want failed", fail.Status)
	}
	if fail.Error == "" || fail.Reason == "" {
		t.Errorf("异常收尾必须带原因：%+v", fail)
	}
	// 已建立 item 时，失败也挂到同一个 item 上：内容还在，只是不再增长。
	if fail.ItemID == "" {
		t.Fatal("异常收尾应挂到已有 item 上")
	}
	if fail.ItemRevision <= 1 {
		t.Errorf("异常收尾的 revision 必须高于既有 partial：%d", fail.ItemRevision)
	}
	if !strings.Contains(fail.Snapshot, "半截") {
		t.Errorf("异常收尾的 snapshot 应保留已流出的内容：%q", fail.Snapshot)
	}
}

// TestTurnFailedWithoutAnyText 引擎连一个字都没吐就断了：仍然必须有终态。
func TestTurnFailedWithoutAnyText(t *testing.T) {
	acc := &streamAccumulator{Engine: "claude"}
	c := newCollector(acc)
	feedTo(t, acc, c, []string{
		`{"type":"system","subtype":"init","session_id":"s-9"}`,
	})
	acc.emitTurnFailed("s-9", errNoResultLine("claude"))

	if len(c.evs) != 1 || c.evs[0].Kind != KindTurnFailed {
		t.Fatalf("events = %+v，want 恰好一条 turn_failed", c.evs)
	}
	if c.evs[0].SessionID != "s-9" {
		t.Errorf("异常收尾要带 session_id：%q", c.evs[0].SessionID)
	}
	// 没建立过 item 时不硬造 id（宁缺勿猜：空 id 表示「本事件不参与状态收敛」）。
	if c.evs[0].ItemID != "" {
		t.Errorf("没有内容时不该硬造 item id：%q", c.evs[0].ItemID)
	}
}

// TestAggregateOnlySnapshotReplacesNotAppends AggregateOnly 引擎（codebuddy）
// 只发聚合行，聚合行给的是**整条消息的全文**。snapshot 必须是整体替换，
// 不是追加 —— 追加会得到「你好你好」这种重复正文（同一份内容数两遍）。
func TestAggregateOnlySnapshotReplacesNotAppends(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"cb-1"}`,
		`{"type":"assistant","message":{"id":"cb_msg_1","content":[{"type":"text","text":"第一段"}]}}`,
		`{"type":"assistant","message":{"id":"cb_msg_1","content":[{"type":"text","text":"第二段"}]}}`,
	}
	acc := &streamAccumulator{Engine: "codebuddy", AggregateOnly: true}
	evs := feedNDJSON(t, acc, lines)

	if len(evs) != 2 {
		t.Fatalf("事件数 = %d，want 2", len(evs))
	}
	for i, ev := range evs {
		if ev.Snapshot == "" {
			t.Fatalf("聚合行也要带 snapshot：%+v", ev)
		}
		if strings.Contains(ev.Snapshot, "第一段第一段") {
			t.Fatalf("第 %d 帧 snapshot 重复累加了：%q", i, ev.Snapshot)
		}
	}
	if evs[0].ItemID != evs[1].ItemID {
		t.Fatalf("同一轮两条聚合行的 item id 应一致：%q vs %q", evs[0].ItemID, evs[1].ItemID)
	}
	if evs[1].ItemRevision <= evs[0].ItemRevision {
		t.Fatalf("revision 必须递增：%d vs %d", evs[0].ItemRevision, evs[1].ItemRevision)
	}
}

// TestThinkingAndTextShareItem 思考与正文属于**同一条消息 item**（共享 id、
// 各自递增 revision），但 snapshot 分开：thinking 的快照不含正文。
//
// 为什么分开：消费方要能分别渲染「思考气泡」和「正文气泡」，
// 混在一个 snapshot 里就没法分开画。
// TestThinkingAndTextAreSeparateItems 思考与正文是**两条独立 item**。
//
// 2026-10-02 修正：原先二者共享一个 item id，只靠 snapshot 内容区分。
// 那样在「按 (item_id, revision) 覆盖式 upsert」下会静默丢内容：
//
//	thinking(snapshot=「我在想」, rev=1)
//	text    (snapshot=「答案」,   rev=2)  ← 覆盖前者，思考彻底消失
//
// Text 是增量所以终端直打看不出问题；出问题的是走 snapshot 的消费方
// （历史回放 / 多端同步）。agents-anywhere 同样把 reasoning 归为独立的
// system item（runtime_protocol/timeline.py），压根不与 message 共用 item。
//
// 断言两条 item id 不同，但**内嵌同一个原生 message id** —— 消费方要
// 合并展示时按原生 id 关联即可，不需要额外映射表。
func TestThinkingAndTextAreSeparateItems(t *testing.T) {
	acc := &streamAccumulator{Engine: "claude"}
	evs := feedNDJSON(t, acc, []string{
		claudeMsgStart,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"我在想"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案"}}}`,
	})
	if len(evs) != 2 {
		t.Fatalf("事件数 = %d，want 2", len(evs))
	}
	think, text := evs[0], evs[1]
	if think.Kind != KindThinking || text.Kind != KindText {
		t.Fatalf("kind 顺序不对：%v / %v", think.Kind, text.Kind)
	}
	if think.ItemID == text.ItemID {
		t.Fatalf("思考与正文不该共用 item id：%q（共用会让 upsert 丢掉先到的一半）", think.ItemID)
	}
	// 两条 item 都要能追回同一个原生 message id，否则消费方无法关联。
	for _, ev := range []StreamEvent{think, text} {
		if !strings.HasSuffix(ev.ItemID, "msg_abc123") {
			t.Errorf("item id 未内嵌原生 message id：%q", ev.ItemID)
		}
	}
	// 命名钉死：正文 msg_ / 思考 think_（与 wire 上 type=text / type=thinking 对齐）。
	// 改名会让按 item_id 前缀分类的消费方（"只看思考"之类）静默失效。
	if !strings.HasPrefix(text.ItemID, "claude_msg_") {
		t.Errorf("正文 item id 前缀 = %q，want claude_msg_", text.ItemID)
	}
	if !strings.HasPrefix(think.ItemID, "claude_think_") {
		t.Errorf("思考 item id 前缀 = %q，want claude_think_", think.ItemID)
	}
	if strings.Contains(think.Snapshot, "答案") {
		t.Errorf("思考的 snapshot 不该含正文：%q", think.Snapshot)
	}
	if strings.Contains(text.Snapshot, "我在想") {
		t.Errorf("正文的 snapshot 不该含思考：%q", text.Snapshot)
	}
}

// TestToolEventsKeepNoItemID 工具事件**不带** item id —— 它们有自己的
// tool_use.id 关联语义，塞进消息 item 会让工具卡和消息卡混成一个 item。
func TestToolEventsKeepNoItemID(t *testing.T) {
	acc := &streamAccumulator{Engine: "claude"}
	evs := feedNDJSON(t, acc, []string{
		claudeMsgStart,
		`{"type":"stream_event","event":{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","name":"Bash","id":"toolu_1"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":2}}`,
	})
	for _, ev := range evs {
		if ev.Kind == KindToolUse {
			if ev.ItemID != "" {
				t.Errorf("tool_use 不该带 item id：%q", ev.ItemID)
			}
			if ev.ID != "toolu_1" {
				t.Errorf("tool_use.ID = %q，want toolu_01 系列的 toolu_1", ev.ID)
			}
		}
	}
}

// errNoResultLine 复刻各引擎的「流结束但没有 result 行」错误文案。
func errNoResultLine(engine string) error {
	return &noResultError{engine: engine}
}

type noResultError struct{ engine string }

func (e *noResultError) Error() string {
	return e.engine + " CLI stream ended without result line"
}

// ── 端到端：走真实 Stream 路径（fake CLI 回放真实 NDJSON）──

// TestClaudeStreamEndToEndConverges 端到端：整条链（Stream → 累加器 → 事件）
// 必须给出「同 id、单调 revision、running…→ final」的完整收敛序列。
//
// 这是本文件最重要的一条：前面几条测的是累加器的单个方法，
// 挡不住「引擎 Stream 忘了把 item id 传下去」这类接线错误。
func TestClaudeStreamEndToEndConverges(t *testing.T) {
	skipIfNoShCLI(t)
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s-1","model":"MiniMax-M3"}'
echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_e2e"}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想一下"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"你"}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"好"}},"session_id":"s-1"}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"你好","session_id":"s-1"}'
`)
	e := &ClaudeEngine{BinPath: cli}
	events, onEvent := collectEvents()
	if _, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// 同 id + revision 单调 + 至少一条 final。
	var ids = map[string]bool{}
	var lastRev uint64
	sawFinal := false
	for _, ev := range *events {
		if ev.ItemID == "" {
			continue
		}
		ids[ev.ItemID] = true
		if ev.ItemRevision <= lastRev {
			t.Fatalf("revision 不单调：%d 之后 %d", lastRev, ev.ItemRevision)
		}
		lastRev = ev.ItemRevision
		if ev.Status == StatusFinal {
			sawFinal = true
			if ev.Snapshot != "你好" {
				t.Errorf("final snapshot = %q，want result 行正文「你好」", ev.Snapshot)
			}
		}
	}
	if len(ids) != 1 {
		t.Fatalf("一轮应只有一个 item id，实际 %v", ids)
	}
	if !sawFinal {
		t.Fatal("整条流必须有 final 事件（turn_end 携带）")
	}
}

// TestClaudeStreamMissingResultEmitsTurnFailed 端到端：流被截断时，
// 已经流出去的正文不会停在 running —— 必须补一条 turn_failed。
func TestClaudeStreamMissingResultEmitsTurnFailed(t *testing.T) {
	skipIfNoShCLI(t)
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_cut"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半截"}}}'
`)
	e := &ClaudeEngine{BinPath: cli}
	events, onEvent := collectEvents()
	if _, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent); err == nil {
		t.Fatal("缺 result 行必须报错")
	}

	var fail *StreamEvent
	for i := range *events {
		if (*events)[i].Kind == KindTurnFailed {
			fail = &(*events)[i]
		}
	}
	if fail == nil {
		t.Fatalf("必须补 turn_failed 终态事件，实际事件：%+v", events)
	}
	if fail.Status != StatusFailed || fail.Reason == "" {
		t.Errorf("turn_failed 缺状态或原因：%+v", fail)
	}
	// 半截正文仍在同一条 item 上，且不再增长。
	if !strings.Contains(fail.Snapshot, "半截") {
		t.Errorf("turn_failed 的 snapshot 应保留已流出的内容：%q", fail.Snapshot)
	}
}

// TestCodeBuddyStreamEndToEndConverges codebuddy（AggregateOnly）端到端收敛。
// 它的正文只来自 assistant 聚合行，snapshot 必须是**替换**语义。
func TestCodeBuddyStreamEndToEndConverges(t *testing.T) {
	skipIfNoShCLI(t)
	cli := writeFakeCLI(t, "codebuddy", `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"cb-1"}'
echo '{"type":"assistant","message":{"id":"cb_msg_e2e","content":[{"type":"text","text":"<think>思考</think>最终答案"}]}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"<think>思考</think>最终答案","session_id":"cb-1"}'
`)
	e := &CodeBuddyEngine{BinPath: cli}
	events, onEvent := collectEvents()
	if _, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var itemID string
	sawFinal := false
	for _, ev := range *events {
		if ev.ItemID == "" {
			continue
		}
		if itemID == "" {
			itemID = ev.ItemID
		}
		if ev.ItemID != itemID {
			t.Fatalf("item id 漂移：%q vs %q", itemID, ev.ItemID)
		}
		if strings.Contains(ev.Snapshot, "<think>") {
			t.Fatalf("snapshot 混进了思维链：%q", ev.Snapshot)
		}
		if ev.Status == StatusFinal {
			sawFinal = true
		}
	}
	if itemID == "" {
		t.Fatal("codebuddy 也应给出稳定 item id")
	}
	if !sawFinal {
		t.Fatal("必须有 final 事件")
	}
}

// TestTraeStreamStillDeltaOnly trae 协议没有 message 概念，
// 但降级 id 仍要**本轮内稳定**（否则一条消息被拆成多段）。
func TestTraeStreamStillDeltaOnly(t *testing.T) {
	skipIfNoShCLI(t)
	cli := writeFakeCLI(t, "trae-cli", `#!/bin/sh
echo '{"type":"stream_event","delta":{"role":"assistant","content":"你"}}'
echo '{"type":"stream_event","delta":{"role":"assistant","content":"好"}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"你好"}'
`)
	e := &TraeEngine{BinPath: cli}
	events, onEvent := collectEvents()
	if _, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var id string
	var lastSnap string
	for _, ev := range *events {
		if ev.Kind != KindText {
			continue
		}
		if ev.ItemID == "" {
			t.Fatal("trae 的正文也应有 item id（降级序号）")
		}
		if id == "" {
			id = ev.ItemID
		}
		if ev.ItemID != id {
			t.Fatalf("trae 降级 id 不稳定：%q vs %q", id, ev.ItemID)
		}
		lastSnap = ev.Snapshot
	}
	if lastSnap != "你好" {
		t.Errorf("最终 snapshot = %q，want 你好", lastSnap)
	}
}

// ── arkclaw 专项（A2A message/stream）─────────────────────────────────
//
// 与 claude 的关键差异，决定了这里要钉的东西不一样：
//   - **没有 message_start**，所以拿不到「一轮一条新消息」的事件；
//     一轮就是一个 task，原生 id 直接取 result.id（task id）。
//   - **不能拿 contextId 当 item id**：它是跨轮不变的**会话**锚点，
//     拿它当 item id 的话同一会话每一轮都是同一个 item，后一轮覆盖前一轮
//     （症状：多轮对话只剩最后一次回复）。
//   - 网关**不是逐字增量**：实测 200 字生成只回 2 帧（working 空帧 +
//     completed 整段）。所以内容累积由 arkclaw 自己做前缀去重，tracker
//     只管 id/revision/status（分工见 itemtracker.go 文件头）。

func TestArkClawItemIDFromTaskID(t *testing.T) {
	body := arkClawSSEFrame(arkClawWorkingFrame("ctx-s1")) +
		arkClawSSEFrame(arkClawTextEnvelope("正文", "ctx-s1"))
	srv, _ := arkClawSSEServer(t, body)

	events, onEvent := collectEvents()
	if _, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	texts, _ := arkClawSplit(events)
	if len(texts) == 0 {
		t.Fatal("缺少正文事件")
	}
	// result.id = "task-1"（见 arkClawTextEnvelope 的帧构造）。
	if want := "arkclaw_msg_task-1"; texts[0].ItemID != want {
		t.Errorf("item_id = %q want %q（必须派生自 task id）", texts[0].ItemID, want)
	}
	// contextId 是会话锚点，只能进 SessionID，**绝不能**成为 item id。
	for _, ev := range *events {
		if strings.Contains(ev.ItemID, "ctx-s1") {
			t.Errorf("item_id 混入了 contextId: %q", ev.ItemID)
		}
	}
	if (*events)[len(*events)-1].SessionID != "ctx-s1" {
		t.Errorf("turn_end.SessionID = %q want ctx-s1", (*events)[len(*events)-1].SessionID)
	}
}

func TestArkClawRevisionMonotonicAndSnapshotCumulative(t *testing.T) {
	// 三帧增量（网关将来的形态）→ 逐帧校验：text 是增量，snapshot 是累积。
	body := arkClawSSEFrame(arkClawArtifactFrame("working", "c1", "第一段")) +
		arkClawSSEFrame(arkClawArtifactFrame("working", "c1", "第一段第二段")) +
		arkClawSSEFrame(arkClawTextEnvelope("第一段第二段第三段", "c1"))
	srv, _ := arkClawSSEServer(t, body)

	events, onEvent := collectEvents()
	if _, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	texts, ends := arkClawSplit(events)
	wantDelta := []string{"第一段", "第二段", "第三段"}
	wantSnap := []string{"第一段", "第一段第二段", "第一段第二段第三段"}
	if len(texts) != len(wantDelta) {
		t.Fatalf("text 事件数 = %d，want %d：%+v", len(texts), len(wantDelta), texts)
	}
	for i, ev := range texts {
		if ev.Text != wantDelta[i] {
			t.Errorf("[%d] text = %q，want 增量 %q", i, ev.Text, wantDelta[i])
		}
		if ev.Snapshot != wantSnap[i] {
			t.Errorf("[%d] snapshot = %q，want 累积 %q", i, ev.Snapshot, wantSnap[i])
		}
		if ev.Status != StatusRunning {
			t.Errorf("[%d] status = %q want running（partial 不是终态）", i, ev.Status)
		}
		if ev.ItemRevision != uint64(i+1) {
			t.Errorf("[%d] revision = %d，want %d（严格递增）", i, ev.ItemRevision, i+1)
		}
	}
	assertArkClawFinal(t, texts, ends, "第一段第二段第三段")
}

func TestArkClawMissingTaskIDDegradesToStableLocal(t *testing.T) {
	// 网关不给 result.id 时降级到**本轮内稳定**的序号 id（不是每帧新造）。
	frame := `{"jsonrpc":"2.0","id":"req-1","result":{"kind":"task","contextId":"c1",` +
		`"status":{"state":"completed","message":{"parts":[{"kind":"text","text":"无 task id"}]}}}}`
	srv, _ := arkClawSSEServer(t, arkClawSSEFrame(frame))

	events, onEvent := collectEvents()
	if _, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	texts, ends := arkClawSplit(events)
	// 降级序号从 1 起（ensure 用 localSeq 递增，不取 revision —— 后者会被
	// Begin 归零，让同一跟踪器里的两条无 id 消息撞成同一个 local_1）。
	if want := "arkclaw_msg_local_1"; texts[0].ItemID != want {
		t.Errorf("降级 item_id = %q want %q", texts[0].ItemID, want)
	}
	assertArkClawFinal(t, texts, ends, "无 task id")
}

func TestArkClawTurnFailedOnTruncatedStream(t *testing.T) {
	// 已发出增量但流被截断（没有 completed 帧）→ 必须补 turn_failed，
	// 否则那条 item 永远停在 running，消费方只能靠超时猜。
	// 对照：TestArkClawStreamMissingCompletedFrame 用 func(StreamEvent){}
	// 收事件，从没建立过 item，所以**不该**凭空造出 failed 帧。
	body := arkClawSSEFrame(arkClawWorkingFrame("c1")) +
		arkClawSSEFrame(arkClawArtifactFrame("working", "c1", "半句正文"))
	srv, _ := arkClawSSEServer(t, body)

	events, onEvent := collectEvents()
	_, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err == nil {
		t.Fatal("截断流应报错")
	}
	texts, failed := arkClawSplitFailed(events)
	if len(texts) != 1 || len(failed) != 1 {
		t.Fatalf("事件形状 = text %d / turn_failed %d，want 1/1：%+v", len(texts), len(failed), *events)
	}
	// failed 挂在**同一个** item 上：内容还在，只是这条 item 不再增长。
	if failed[0].ItemID != texts[0].ItemID {
		t.Errorf("turn_failed 应挂在正文 item 上：%q vs %q", failed[0].ItemID, texts[0].ItemID)
	}
	if failed[0].Status != StatusFailed {
		t.Errorf("status = %q want %q", failed[0].Status, StatusFailed)
	}
	if failed[0].ItemRevision <= texts[0].ItemRevision {
		t.Errorf("turn_failed revision 必须更高：%d vs %d", failed[0].ItemRevision, texts[0].ItemRevision)
	}
	if failed[0].Error == "" || failed[0].Reason == "" {
		t.Errorf("turn_failed 必须带 error / reason：%+v", failed[0])
	}
	// snapshot 保留已发出去的 partial（不回退成空）。
	if failed[0].Snapshot != "半句正文" {
		t.Errorf("snapshot = %q，want 保留 partial「半句正文」", failed[0].Snapshot)
	}
}

func TestArkClawTurnFailedOnTaskFailedState(t *testing.T) {
	// status.state=failed 同样要收成 failed 终态。
	frame := `{"jsonrpc":"2.0","id":"req-1","result":{"kind":"task","id":"t","contextId":"ctx-9",` +
		`"status":{"state":"failed","message":{"parts":[{"kind":"text","text":"上游超时"}]}}}}`
	srv, _ := arkClawSSEServer(t, arkClawSSEFrame(arkClawWorkingFrame("ctx-9"))+
		arkClawSSEFrame(arkClawArtifactFrame("working", "ctx-9", "生成了一部分"))+
		arkClawSSEFrame(frame))

	events, onEvent := collectEvents()
	_, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err == nil {
		t.Fatal("failed 状态应报错")
	}
	_, failed := arkClawSplitFailed(events)
	if len(failed) != 1 {
		t.Fatalf("turn_failed 事件数 = %d want 1：%+v", len(failed), *events)
	}
	if failed[0].Status != StatusFailed || failed[0].Snapshot != "生成了一部分" {
		t.Errorf("turn_failed = %+v", failed[0])
	}
}
