package agent

// itemtracker.go - 状态收敛的可复用跟踪器（2026-10-02）。
//
// streamAccumulator 内嵌了一份状态收敛逻辑，但那只覆盖走
// `streamAccumulator` 的三个引擎（claude / codebuddy / trae）。
// 其余六个引擎组各有自己的累加器结构与 emit 路径：
//
//	codex                  codexStreamAcc
//	llm                    thinkSplitter
//	openclaw               acpStreamState
//	dsh / dsh_sdk          各自的 session.event 帧循环（同一引擎两条协议路径）
//	arkclaw                SSE 两帧（整段到达）
//	codebuddy-gateway      SSE 单帧（整段到达）
//
// 让它们各自造一套 id/revision 逻辑必然漂移，所以把**与引擎无关的部分**
// （item 身份、revision 计数、状态机）抽成本文件，各引擎持有一个实例。
//
// 分工边界（重要）：
//   itemTracker  —— 身份与版本：item id、revision 递增、status 语义
//   调用方        —— 内容累积：claude 要按 content block index 升序拼接，
//                    别的引擎直接 append 或整段替换
//
// 两个累积入口对应两种真实协议形态：
//   Append(delta)  增量协议（claude delta / trae delta.content / llm chunk）
//   Replace(full)  整段协议（arkclaw 与 gateway 一次给全文）——语义是
//                  **替换不是追加**，追加会得到「你好你好」

import "fmt"

// itemLane 同一轮内并行的内容通道。
//
// 为什么按 lane 拆开 item（2026-10-02 修正）：
// thinking 与 text 共享一个 item id 时，消费方按 (item_id, revision)
// 覆盖式 upsert 会**丢掉先到的那一半**——
//
//	thinking(snapshot=「我在想」, rev=1)
//	text    (snapshot=「答案」,   rev=2)   ← 覆盖，思考消失
//
// Text 字段是增量，所以终端直打完全看不出来；出问题的是走 snapshot
// 的消费方（历史回放、多端同步）—— 思考内容静默消失，且没有任何报错。
//
// agents-anywhere 面对同一问题选了同样的方向：reasoning 是**独立的
// system item**（runtime_protocol/timeline.py 的 SystemContentKind =
// "reasoning"），与 message 分属不同 TimelineItemType，压根不共用 item。
//
// 拆开不丢关联性：两个 item id 内嵌**同一个原生 message id**
// （claude_msg_<id> 与 claude_think_<id>），消费方要合并展示时
// 按原生 id 关联即可，不需要额外的映射表。
type itemLane string

const (
	// laneText 正文通道，item id 形如 claude_msg_<native>。
	laneText itemLane = ""

	// laneThink 思考/推理通道，item id 形如 claude_think_<native>。
	// 中段用 think_（不是 reason_）：与 wire 上的事件 type=thinking 对齐，
	// 读日志/grep 时不用在 reason / think / reasoning 之间猜。
	laneThink itemLane = "think_"
)

// itemAttrs 一条事件的收敛属性。apply 把它写回 StreamEvent。
type itemAttrs struct {
	ItemID   string
	Revision uint64
	Status   StreamStatus
	Snapshot string
}

// apply 把收敛属性挂到事件上。
func (a itemAttrs) apply(ev *StreamEvent) {
	ev.ItemID = a.ItemID
	ev.ItemRevision = a.Revision
	ev.Status = a.Status
	ev.Snapshot = a.Snapshot
}

// itemTracker 维护「本轮当前 item」的身份与版本。
//
// 零值不可用，必须用 newItemTracker 构造。生命周期与一轮流式调用一致
// （每轮新建），所以不需要显式释放。
type itemTracker struct {
	prefix   string
	itemID   string
	nativeID string
	revision uint64
	snapshot []byte
	// localSeq 无原生 id 时的降级序号，**只增不减**。
	//
	// 为什么不复用 revision：revision 会被 Begin 归零，于是同一跟踪器里的
	// 两条无 id 消息都会拿到 local_1 —— 后一条覆盖前一条（症状同
	// 「一轮多条 assistant message 只剩最后一段」）。序号必须跨 Begin 单调。
	localSeq uint64
}

// newItemTracker 构造某条 lane 的跟踪器。engine 决定 item id 前缀，
// lane 决定中段（不同引擎的原生 id 空间不互通，混用会让跨引擎回放撞 id）。
func newItemTracker(engine string, lane itemLane) *itemTracker {
	return &itemTracker{prefix: itemTrackerPrefix(engine, lane)}
}

// itemTrackerPrefix 该引擎 + lane 的 item id 前缀。
func itemTrackerPrefix(engine string, lane itemLane) string {
	base := "msg_"
	if engine != "" {
		base = engine + "_" + base
	}
	if lane == laneText {
		return base
	}
	// 非正文通道插在中段：claude_msg_ → claude_think_
	return engine + "_" + string(lane) + "msg_"
}

// Begin 声明「一条新消息开始了」。
//
// 什么时候必须调：claude 一轮里可能有**多条** assistant message
// （工具调用后继续对话，每次 tool_result 之后都是新的 message_start）。
// 若不切，一轮里的多条消息会共用一个 item id —— 前一条被后一条覆盖，
// 症状是「对话里只有最后一段回复，前面的不见了」。
//
// agents-anywhere 在 message_start 里 clear() 块桶并把 revision 归零
// （runtimes/claude/timeline/stream.py:35-36），本方法是对同一动作的封装。
//
// nativeID 为空时按序号降级（协议没给 message id）。
//
// 重复调用是安全的：带**同一个** native id 再 Begin 视为无操作，
// 不会清掉已累积的内容。某些实现在一条消息里会重复吐 message_start
// （codebuddy 实测有），照单重开会凭空截断正文。
func (t *itemTracker) Begin(nativeID string) {
	if nativeID != "" && nativeID == t.nativeID {
		return
	}
	t.itemID = ""
	t.nativeID = nativeID
	t.revision = 0
	t.snapshot = t.snapshot[:0]
	if nativeID != "" {
		t.itemID = t.prefix + nativeID
	}
}

// Note 记住引擎原生 id（拿不到就不给 item id——**不猜**）。
//
// 必须在第一个内容事件**之前**调：id 是先于内容出现的
// （claude 的 message_start、codex 的 item.started）。
// 之后仍可补记，届时会把已建立的降级 id 升级为原生形式。
func (t *itemTracker) Note(nativeID string) {
	if nativeID == "" || t.nativeID == nativeID {
		return
	}
	t.nativeID = nativeID
	// 只在本轮还没发出任何事件时升级 item id：已经发过的帧挂在
	// 降级 id 上，中途换 id 看起来像丢内容（消费方按 id 建表）。
	if t.revision == 0 {
		t.itemID = t.prefix + nativeID
	}
}

// ensure 建立 item id（降级链见上：原生 id → 本轮内稳定序号）。
//
// 序号取自 localSeq（**不**取自 revision）：revision 会被 Begin 归零，
// 拿它当序号会让同一跟踪器里的两条无 id 消息撞成同一个 local_1，
// 后一条把前一条覆盖掉。
func (t *itemTracker) ensure() string {
	if t.itemID != "" {
		return t.itemID
	}
	t.localSeq++
	t.itemID = fmt.Sprintf("%slocal_%d", t.prefix, t.localSeq)
	return t.itemID
}

// next 取下一个 revision 并返回收敛属性。
func (t *itemTracker) next(status StreamStatus) itemAttrs {
	return t.nextWith(status, string(t.snapshot))
}

// nextWith 同 next，但调用方自己提供 snapshot 内容。
//
// 存在的理由：claude 的 snapshot 必须按 content block index 升序拼接
// （到达顺序不等于 index 顺序），由 streamAccumulator 算好后传进来 ——
// 身份与版本仍由跟踪器统一管理，内容拼接留给调用方（见文件头分工边界）。
func (t *itemTracker) nextWith(status StreamStatus, snapshot string) itemAttrs {
	t.revision++
	return itemAttrs{
		ItemID:   t.ensure(),
		Revision: t.revision,
		Status:   status,
		Snapshot: snapshot,
	}
}

// Append 累积一段增量，返回该 item 的新版本属性（status=running）。
//
// 用于**增量协议**引擎：每帧把 delta 拼上去，snapshot 始终是累积整段。
func (t *itemTracker) Append(delta string) itemAttrs {
	if delta != "" {
		t.snapshot = append(t.snapshot, delta...)
	}
	return t.next(StatusRunning)
}

// Replace 用整段替换累积内容，返回新版本属性（status=running）。
//
// 用于**整段协议**引擎（arkclaw / codebuddy-gateway 一次给全文）。
// 语义必须是替换：这些引擎每帧给的是全文，追加会让同一份内容数两遍。
func (t *itemTracker) Replace(full string) itemAttrs {
	t.snapshot = append(t.snapshot[:0], full...)
	return t.next(StatusRunning)
}

// Finalize 收尾：把权威正文（引擎的 result 行）作为最后版本（status=final）。
//
// snapshot 用引擎给的权威值而非累积值 —— 增量拼接与 result 偶有出入
// （codebuddy 还要过 think 剥离）时以 result 为准。
//
// 没有建立过 item 时返回 ok=false：不该为「一句正文都没流出来」的轮次硬造 item。
func (t *itemTracker) Finalize(final string) (itemAttrs, bool) {
	if t.itemID == "" {
		return itemAttrs{}, false
	}
	if final != "" {
		t.snapshot = append(t.snapshot[:0], final...)
		return t.next(StatusFinal), true
	}
	// 没给权威值就沿用累积值，别把已有内容清空。
	return t.next(StatusFinal), true
}

// Failed 异常收尾（status=failed）：引擎流被截断，没走到终帧。
//
// 挂到已有 item 上：内容还在，只是这条 item 不会再增长。消费方看到
// failed 就知道不必再等。没建立过 item 时返回 ok=false。
//
// snapshot 形参用于 claude 那类「内容由调用方按 block 拼」的引擎 ——
// 跟踪器自己累积的那份是空的，传空串即可回退到它。
func (t *itemTracker) Failed(snapshot string) (itemAttrs, bool) {
	if t.itemID == "" {
		return itemAttrs{}, false
	}
	return t.nextWith(StatusFailed, snapshot), true
}

// trackerPair 是同一轮内并行的两条 lane 跟踪器。
//
// 单独这个类型是为了让「收尾」这类**跨 lane 操作**有一个明确归属。
// 注意这里只提供 text lane 的终态 —— 思考 lane **不单独发终态**：
//
//   - final   ：result 行只含正文，给思考发 final 会让快照回退成空
//   - failed  ：turn_failed 本身已是轮级终态信号
//
// 两条 lane 的终止由消费方「见到轮级终态后不再等思考」来表达，
// 详见 stream.go emitTurnEnd / emitTurnFailed 的注释。
type trackerPair struct {
	text  *itemTracker
	think *itemTracker
}

// finalize 收尾正文 lane（final 状态，权威值 = result 行的正文）。
func (p trackerPair) finalize(final string) (itemAttrs, bool) {
	return p.text.Finalize(final)
}

// failText 异常收尾正文 lane（failed 状态）。
//
// snapshot 形参由调用方提供：claude 的内容按 content block index 分桶
// 累积在累加器里，跟踪器自己那份是空的。
func (p trackerPair) failText(snapshot string) (itemAttrs, bool) {
	return p.text.Failed(snapshot)
}
