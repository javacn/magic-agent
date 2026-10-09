package agent

/* codebuddy_gateway_stream.go - codebuddy-gateway 的收流通道（SSE）。
 *
 * 通道形态：GET <base>/api/v1/runs/{runId}/stream + `accept: text/event-stream`，
 * 响应是 SSE，逐帧 `data: {...}`，每帧是一个 **Gateway Protocol 出站消息**：
 *
 *	{"version":"1.0","replyTo":"<入站消息 id>","status":"<状态>", ...}
 *
 * 帧类型（对 CodeBuddy Code 2.147.0 读内核得到）：
 *
 *	status=accepted   — 受理帧。**没有正文**；只在投递后立刻发一次。
 *	                    ⚠️ 网关侧用的是普通 RxJS Subject（非 Replay），所以「投递完才
 *	                    去连流」的客户端**收不到这一帧** —— 别拿它当「连接成功」的判据。
 *	status=streaming  — 正文增量：content.chunk 是**增量**（不是全量），逐块拼接。
 *	status=completed  — 终帧：content.markdown 是**整段**正文（权威值），
 *	                    另带 agent.sessionId / agent.toolCalls。
 *	status=error      — 终止：error.code / error.message。
 *
 * ⚠️ **实测更正（2026-09-24，本机 WorkBuddy CLI 2.147.0 + 默认模型）**：这个网关目前
 * **并不发 streaming 帧**。裸探针（raw_probe，不经本引擎解析）一次 410 字生成的完整帧序列是：
 *
 *	+10.067s 流已建立（content-type=text/event-stream）
 *	+10.067s event: message
 *	+10.067s data: {"status":"completed","content":{"markdown":"<410 字整段>"}}
 *	+10.067s event: done
 *	+10.068s data: {}
 *
 * 也就是「**受理 + 整段正文**」两帧（与 arkclaw 那条通道同构），不是打字机。
 * 内核里 `convertEventToOutbound` 确实写了 text_delta → `status=streaming` 的映射，
 * 但它要求会话事件是 `type=model` 且带 `data.delta.type=text_delta`；这一条在本版本的
 * `--serve` 路径上没有出现（TUI 下是否出现未验）。所以：
 *   · 本文件照**协议**实现 streaming 帧的解析与去重（网关将来吐增量时客户端零改动即可接住）；
 *   · 但**不要对外宣称它是逐字流式** —— 当前成色与 arkclaw 一样是「整段到达」。
 *
 * 另一条实测结论（影响超时语义）：**网关在首帧到达前不 flush 响应头**。
 * 上面那次探针里，投递在 +0.014s 返回，而流响应头到 +10.067s 才出现（= 首帧生成完）。
 * 所以「等待」全都发生在 http.Client.Do 里面 —— 调用方给的超时必须覆盖**整个生成过程**，
 * 而不是只覆盖连接建立（默认 10 分钟正是按这个语义定的，见 DefaultCBGatewayTimeout）。
 *
 * 与非流式（Complete）的关系：同一条执行核（CodeBuddyGatewayEngine.run）+ 同一套帧解析。
 * Complete 也读 SSE —— 因为投递响应里没有正文，SSE 是唯一拿得到正文的通道（见
 * codebuddy_gateway.go 文件头）。差别只在 onEvent 是不是 nil。
 *
 * 不实现的东西：断线重连（网关没有 resubscribe 端点）、Request.Append（webhook 是
 * 「一次投递一个 run」的模型，没有持续喂消息的通道）。
 */

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// cbGatewaySSEMaxLine 单帧上限：completed 帧把**整段正文**放在一行 data 里，留足余量。
const cbGatewaySSEMaxLine = 16 << 20 // 16 MiB

// cbGatewayFrame 是一帧出站消息（Gateway Protocol）。
type cbGatewayFrame struct {
	Version string `json:"version"`
	ReplyTo string `json:"replyTo"`
	Status  string `json:"status"`

	Content *struct {
		// Chunk 增量正文（status=streaming）。
		Chunk string `json:"chunk"`
		// Text / Markdown 整段正文（status=completed；内核两个字段都填，
		// markdown 是完整版，text 在部分路径上被截到 4096）。
		Text     string `json:"text"`
		Markdown string `json:"markdown"`
	} `json:"content"`

	Agent *struct {
		SessionID string              `json:"sessionId"`
		ToolCalls []cbGatewayToolCall `json:"toolCalls"`
	} `json:"agent"`

	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// cbGatewayToolCall 是终帧里带的工具调用记录（尽力解析，缺失不影响正文）。
//
// 本版本内核的 convertEventToOutbound 并不填这个数组，所以实测通常为空 ——
// 解析写成宽松的、字段可缺省，将来网关开始填时零改动即可接住。
type cbGatewayToolCall struct {
	Name   string          `json:"name"`
	ID     string          `json:"id"`
	Args   json.RawMessage `json:"args"`
	Result string          `json:"result"`
}

// cbGatewayStreamState 一条 run 流的累积状态。
type cbGatewayStreamState struct {
	onEvent func(StreamEvent)

	msgID  string // 入站消息 id（replyTo，用于过滤串台帧）
	runID  string
	convID string

	// emitted 已作为增量发出的正文（去重用：终帧给的是整段）。
	emitted strings.Builder
	// final 权威正文（终帧给的整段）；缺省回落到 emitted。
	final string

	completed bool   // 是否见过终帧
	status    string // 最近一次 status（错误信息用）
	sessionID string // 会话锚点（网关回 sessionId 时以它为准）
	tools     []ToolCall

	// tracker 状态收敛的身份与版本（见 itemtracker.go）。
	// 原生 id = 网关分配的 runId（一个 run 恒定，见 codebuddy_gateway.go 的 deliver）。
	textTrack *itemTracker
	// textTrack 是否已经发过事件（发过就不能再换 item id）。
	textStarted bool
}

// trackers 惰性建立跟踪器。
func (st *cbGatewayStreamState) trackers() *itemTracker {
	if st.textTrack == nil {
		st.textTrack = newItemTracker(cbGatewayEngineName, laneText)
		st.textTrack.Begin(st.runID)
	}
	return st.textTrack
}

// Stream 实现 Streamer：走 webhook 投递 + SSE 收流（见文件头）。
func (e *CodeBuddyGatewayEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	return e.run(ctx, req, onEvent)
}

// consumeStream 打开 run 流并逐帧消费，直到终帧 / 流结束 / 出错。
//
// schema 非 nil 时对终帧正文做严格 JSON 抽取（与 llm / openclaw / arkclaw 同一后处理），
// 且在**发出之前**做，保证调用方拿到的增量与收尾正文是同一份。
func (e *CodeBuddyGatewayEngine) consumeStream(ctx context.Context, client *http.Client, r cbGatewayResolved, runID string, st *cbGatewayStreamState, schema *JSONSchema) error {
	endpoint := r.baseURL + "/api/v1/runs/" + url.PathEscape(runID) + "/stream"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("%s: 构造流请求失败: %w", e.Name(), err)
	}
	httpReq.Header.Set("accept", "text/event-stream")
	cbGatewaySetAuth(httpReq, r.password)

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%s: 连接 run 流失败（网关 %s 是否在跑？）: %w", e.Name(), r.baseURL, err)
	}
	defer httpResp.Body.Close()

	// 网关没按 SSE 回（老版本 / 路由被改）：如实退回「一次性解析」，不假装有增量。
	if !cbGatewayIsEventStream(httpResp.Header.Get("content-type")) {
		raw, rerr := io.ReadAll(io.LimitReader(httpResp.Body, cbGatewayMaxBody))
		if rerr != nil {
			return fmt.Errorf("%s: 读取响应失败: %w", e.Name(), rerr)
		}
		if httpResp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: 网关拒绝流请求（HTTP %d）: %s", e.Name(), httpResp.StatusCode, cbGatewayErrorDetail(raw))
		}
		if _, herr := st.handleFrame(raw, schema); herr != nil {
			return herr
		}
		if !st.completed {
			return fmt.Errorf("%s: 网关未按 SSE 回（content-type=%q），且响应里没有终帧 —— 无法取得正文",
				e.Name(), httpResp.Header.Get("content-type"))
		}
		return nil
	}

	sc := bufio.NewScanner(httpResp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), cbGatewaySSEMaxLine)
	for sc.Scan() {
		payload, ok := cbGatewaySSEPayload(sc.Text())
		if !ok {
			continue // 空行 / ":" 心跳 / event: 等字段
		}
		done, herr := st.handleFrame([]byte(payload), schema)
		if herr != nil {
			return herr
		}
		if done {
			break // 终帧：网关随后即关流，不必再等 EOF
		}
	}
	if serr := sc.Err(); serr != nil {
		// 超时 / 取消会在读 body 时爆出来（响应头早就到了，卡的是流）。
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("%s: 流式读取中断: %w", e.Name(), cerr)
		}
		return fmt.Errorf("%s: 读取流式响应失败: %w", e.Name(), serr)
	}

	if !st.completed {
		return fmt.Errorf("%s: run 流结束但没有终帧（最后状态 %q）—— 网关侧任务可能仍在跑或已异常退出",
			e.Name(), st.status)
	}
	return nil
}

// handleFrame 处理一帧出站消息。返回 done=true 表示这帧是终态（可以停读）。
func (st *cbGatewayStreamState) handleFrame(raw []byte, schema *JSONSchema) (bool, error) {
	var f cbGatewayFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return false, nil // 非 JSON 帧（心跳 / 日志混入）：忽略，不打断整轮
	}
	/* 刻意**不按 replyTo 过滤**（虽然它理论上等于本次入站消息 id）。
	 *
	 * 理由：run 流本来就是按 runId 分流的（网关侧 taskStreams.get(runId) 是一个 per-run
	 * 的 Subject，只推这一条 run 的帧），不存在串台；而「严格比对 replyTo」一旦遇上
	 * 字段语义漂移（改成 run id / 出站回复 id），就会**静默丢掉全部帧** —— 症状是
	 * 「跑完了但正文是空的」，排查成本极高。宁可不做这道无收益的过滤。
	 * f.ReplyTo 仍解析出来，供排查时对照日志。 */
	if f.Agent != nil && strings.TrimSpace(f.Agent.SessionID) != "" {
		st.sessionID = strings.TrimSpace(f.Agent.SessionID)
	}
	if f.Agent != nil && len(f.Agent.ToolCalls) > 0 {
		st.tools = cbGatewayToolCalls(f.Agent.ToolCalls)
	}

	status := strings.ToLower(strings.TrimSpace(f.Status))
	st.status = status

	switch status {
	case "error", "failed", "canceled", "cancelled", "rejected":
		code, msg := "", ""
		if f.Error != nil {
			code, msg = f.Error.Code, f.Error.Message
		}
		if strings.TrimSpace(msg) == "" {
			msg = "(网关未给出错误详情)"
		}
		if code != "" {
			return true, fmt.Errorf("%s: run %s 失败（%s）: %s", cbGatewayEngineName, st.runID, code, truncateStr(msg, 300))
		}
		return true, fmt.Errorf("%s: run %s 失败（status=%s）: %s", cbGatewayEngineName, st.runID, f.Status, truncateStr(msg, 300))

	case "completed":
		text := ""
		if f.Content != nil {
			text = firstNonEmpty(f.Content.Markdown, f.Content.Text)
		}
		// JSONSchema 后处理：在发出之前做，保证增量与收尾正文是同一份（同 arkclaw）。
		if schema != nil {
			if extracted, ok := extractJSONObjectStrict(text, schema); ok {
				text = extracted
			}
		}
		st.final = text
		st.completed = true
		st.emitText(text)
		// completed = 轮次终点：发 turn_end 把 item 收成 final（见 finish 的注释）。
		st.finish(firstNonEmpty(st.convID, st.sessionID))
		return true, nil

	case "streaming":
		if f.Content != nil && f.Content.Chunk != "" {
			st.emitText(st.emitted.String() + f.Content.Chunk)
		}
		return false, nil

	case "accepted":
		return false, nil // 受理帧：没有正文（且晚连的客户端本来就收不到）
	}

	// 未知状态：网关将来加状态时不要打断整轮，但若它挂了正文也照发。
	if f.Content != nil && f.Content.Chunk != "" {
		st.emitText(st.emitted.String() + f.Content.Chunk)
	}
	return false, nil
}

// emitText 发一条正文增量。
//
// 去重规则（与 arkclaw 同一套）：入参是**当前应显示的全量**。已发出的部分与它同前缀时
// 只发剩下的那截；对不上（既非空又不是前缀）宁可不发 —— 正文以 StreamResult.Text 为准，
// 重复发会让调用方看到两份内容。
func (st *cbGatewayStreamState) emitText(full string) {
	if full == "" {
		return
	}
	if prev := st.emitted.String(); prev != "" {
		if !strings.HasPrefix(full, prev) {
			return
		}
		full = full[len(prev):]
	}
	if full == "" {
		return
	}
	st.emitted.WriteString(full)
	if st.onEvent == nil {
		return
	}
	// ⚠️ snapshot 直接取 st.emitted（累积到此刻的全文），**不能**喂给
	// tracker 的 Append —— Append 的语义是「把这段**增量**拼到已有 snapshot
	// 后面」，而这里传进去的是「累积到此刻的全文」，两者混用会逐帧翻倍
	// （第一帧"你好"、第二帧"你好世界"，累加后得到"你好你好世界"）。
	//
	// 正确分工见 itemtracker.go 文件头：tracker 管 id / revision / status，
	// 内容累积由调用方独家负责。nextWith 就是为此准备的入口。
	ev := StreamEvent{Kind: KindText, Text: full}
	st.trackers().nextWith(StatusRunning, st.emitted.String()).apply(&ev)
	st.textStarted = true
	st.onEvent(ev)
}

// fail 异常收尾（status=failed）：流被截断 / 任务 failed / HTTP 拒绝 / 读出错。
//
// 与 finish 的分工：turn_end = 网关给了 completed 帧（成功收尾），
// turn_failed = 没有（异常收尾），带 Error / Reason。
// 挂到**已有**正文 item 上：partial 的内容仍然存在，只是这条 item 不再增长。
// 从没建立过 item 时直接返回 —— 不该为「一句正文都没流出来」的轮次硬造 item。
//
// 只有一个调用点（run 里的 consumeStream 错误分支）：它已经覆盖了
// handleFrame 报错、读流中断、无终帧、非 SSE 缺终帧等全部异常出口，
// 在这里收口比在每个 return err 处各写一遍可靠。
func (st *cbGatewayStreamState) fail(err error) {
	if st.onEvent == nil || err == nil || !st.textStarted {
		return
	}
	ev := StreamEvent{
		Kind:      KindTurnFailed,
		SessionID: firstNonEmpty(st.convID, st.sessionID),
		Error:     err.Error(),
		Reason:    ReasonOf(err),
		Status:    StatusFailed,
	}
	if attrs, ok := st.trackers().Failed(st.emitted.String()); ok {
		attrs.apply(&ev)
	}
	st.onEvent(ev)
}

// finish 收尾：把 item 标成 final（completed 帧已到，正文不会再变）。
//
// 发的是 **KindTurnEnd** 而不是又一条 KindText，原因有三个：
//  1. 语义对：completed 帧是**轮次边界**，不是「又来了一段正文」。
//     之前试过补一条空 Text 的 KindText 帧，能过测试但语义是错的 ——
//     消费方按 Text 拼正文的实现会收到一个空增量。
//  2. 不破坏既有收集器：现有测试与调用方都按「KindText 就是正文增量」收集，
//     多一条空帧会让它们数错条数。
//  3. 轮次边界有独立信号后，思考 lane（将来若有）也能靠它收尾 ——
//     与 claude / dsh 引擎的处理方式一致。
//
// ⚠️ keep-alive 的空闲收工挂在 KindTurnEnd 上（见 keepAlive 的调用点），
// 但 codebuddy-gateway 不在 AppendSupportOf 名单里，不会启用常驻会话，
// 所以这里发 turn_end 是安全的。
func (st *cbGatewayStreamState) finish(sessionID string) {
	if st.onEvent == nil || !st.textStarted {
		return
	}
	ev := StreamEvent{Kind: KindTurnEnd, Text: st.final, SessionID: sessionID}
	if attrs, ok := st.trackers().Finalize(st.emitted.String()); ok {
		attrs.apply(&ev)
	}
	st.onEvent(ev)
}

// result 把累积状态收成 StreamResult（终态与错误路径共用）。
//
// ⚠️ 返回的 SessionID 必须是**会话锚点（conversation id）**，不是网关内部的 session UUID：
// 调用方会把它原样交给下一轮的 --session，而网关的 getOrCreateSession 只认 conversation.id。
// 实测过一次踩坑：终帧里 agent.sessionId 是 `6a58b80e-…` 这种内部 UUID，若拿它当 session_id
// 返回，下一轮就会以这个 UUID 为锚点**新建一个会话** —— 上下文静默丢失，看起来像「模型失忆」。
// 所以这里只认 convID / req.SessionID；网关给的内部 UUID 仅作诊断（留在 st.sessionID）。
func (st *cbGatewayStreamState) result(e *CodeBuddyGatewayEngine, req Request, start time.Time) StreamResult {
	text := st.final
	if strings.TrimSpace(text) == "" {
		text = st.emitted.String()
	}
	sid := firstNonEmpty(req.SessionID, st.convID)
	return StreamResult{
		Response: Response{
			Engine:    e.Name(),
			Text:      text,
			SessionID: sid,
			Latency:   time.Since(start),
			Attempts:  1,
		},
		Tools: st.tools,
	}
}

// cbGatewayToolCalls 把终帧里的工具记录转成统一的 ToolCall（Args 尽力转成 JSON 串）。
func cbGatewayToolCalls(in []cbGatewayToolCall) []ToolCall {
	out := make([]ToolCall, 0, len(in))
	for _, t := range in {
		out = append(out, ToolCall{
			Name:   t.Name,
			ID:     t.ID,
			Args:   cbGatewayRawJSONToString(t.Args),
			Result: t.Result,
		})
	}
	return out
}

// cbGatewayRawJSONToString 把 args 原样转成字符串（是 JSON 字符串就解出内容，其余保持原文）。
func cbGatewayRawJSONToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// cbGatewaySSEPayload 从一行 SSE 文本里取出 data 载荷。
// ok=false 表示这行不是数据帧：空行（帧分隔）、":xxx"（注释/心跳）、event:/id:/retry: 等字段。
func cbGatewaySSEPayload(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, ":") {
		return "", false
	}
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return "", false
	}
	return payload, true
}

// cbGatewayIsEventStream 响应是否为 SSE（content-type 判断，容忍参数与大小写）。
func cbGatewayIsEventStream(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
}
