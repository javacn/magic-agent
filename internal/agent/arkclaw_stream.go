package agent

// arkclaw_stream.go - ArkClaw 的**流式**通道：A2A 官方的 `message/stream`（SSE 事件流）。
//
// 为什么不是 WebSocket（2026-09-22 实测结论，别再走一遍）：
//   - 向同一端点发 `Upgrade: websocket` 握手，网关**不回 101**，照常当普通 POST 处理
//     （HTTP 200 + application/json；server: istio-envoy / x-powered-by: Express）；
//   - A2A 核心规范只定义三种传输（JSON-RPC over HTTP(S) / gRPC / HTTP+JSON），流式统一走
//     SSE，WebSocket 属「自定义协议绑定」，要**服务端**另行实现；
//   - 火山引擎 API 网关（*.volceapi.com）的协议枚举里没有 WebSocket API 类型，也没有
//     「把已有 HTTP API 升级成 WS」的能力 —— 这条链路本身托不住 WS。
//   结论：客户端单方面把 URL 换成 wss:// 什么都拿不到，流式只能走 SSE。
//
// 通道形态：POST message/stream + `accept: text/event-stream`，响应是 SSE，逐帧 `data: {...}`，
// 每帧是一个完整的 JSON-RPC 2.0 Response（见 arkclaw.go 文件头的 envelope 说明）。
//
// 事件映射（实测 2026-09-22，真实网关）：
//
//	status.state=working/submitted   → 不发事件（网关没有增量可发），只刷新 contextId
//	status.state=completed           → KindText（正文）
//	status.state=failed/canceled/…   → 直接报错（与 Complete 同一条错误路径）
//	中间帧若挂 artifact 正文          → 按增量发（本网关当前不发，留着以免将来漏掉）
//	`:` 注释行 / 空行 / event: 等字段  → 跳过（SSE 分隔与心跳）
//
// ⚠️ 本网关的流式**不是逐字增量**，这是协议事实不是解析缺陷：实测一次 200 字生成只收到 2 帧
// —— 0.20s 的 working（空正文）与 12.42s 的 completed（654 字**一次性**到达）。
// 所以这里的语义是「任务受理帧 + 正文整段到达」，不是打字机式流。要逐字得 claw 侧在生成
// 过程中多发中间 status-update / artifact-update，属服务端改动（与传输协议无关）。
// 收益是实的：SSE 连接由服务端持续持有并回帧，长任务不再靠单次 HTTP 空等（也就不会被
// 中间代理的空闲超时掐断），且 claw 侧将来吐增量时客户端零改动即可接住。
//
// 与非流式（Complete）的关系：同一条请求构造路径（arkClawBuildRequest）+ 同一套错误判定
// （arkClawParseResponse），差别只在 method（message/stream vs message/send）与响应读取方式
// （SSE 逐帧 vs 一次性 body）。网关没按 SSE 回时**如实退回一次性响应**（见 Stream），
// 不拿「没有 completed 帧」去糊弄调用方。
//
// 不实现的东西：tasks/resubscribe（断线重连）、Request.Append（A2A 无「持续喂消息」语义，
// 见 AppendSupportOf）、-r 重试（增量已实时发出，重放会重复消费，与其余引擎一致）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/darren/magic-agent/internal/config"
)

// arkClawSSEMaxLine 单帧上限：completed 帧把**整段正文**放在一行 data 里，留足余量。
const arkClawSSEMaxLine = 16 << 20 // 16 MiB

// arkClawStreamState 一次 message/stream 的累积状态。
type arkClawStreamState struct {
	onEvent func(StreamEvent)

	// emitted 已作为增量发出的正文。completed 帧会把整段正文再给一次（实测 status.message
	// 与 artifacts 都带同一份），靠它做前缀去重，避免调用方看到两份。
	emitted strings.Builder
	// final 权威正文（completed 帧给的整段）；缺省回落到 emitted。
	final string

	completed bool   // 是否见过 completed 帧
	state     string // 最近一次 status.state（错误信息用）
	sessionID string // contextId（会话锚点，逐帧刷新）

	// tracker 状态收敛的身份与版本（见 itemtracker.go）。
	textTrack *itemTracker
	// textTrack 是否已经发过事件（发过就不能再换 item id）。
	textStarted bool
}

// trackers 惰性建立跟踪器。
func (st *arkClawStreamState) trackers() *itemTracker {
	if st.textTrack == nil {
		st.textTrack = newItemTracker("arkclaw", laneText)
	}
	return st.textTrack
}

// Stream 实现 Streamer：走 A2A 官方 SSE 通道（见文件头）。
func (e *ArkClawEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()

	endpoint, key, clawID, err := e.endpoint()
	if err != nil {
		return StreamResult{}, fmt.Errorf("arkclaw: %w", err)
	}
	if endpoint == "" || key == "" || clawID == "" {
		return StreamResult{}, fmt.Errorf("arkclaw: 未配置（需要 url / key / claw_id）；请在 %s 的 \"arkclaw\" 节补齐，或设置 %s / %s / %s",
			e.configHint(), config.EnvArkClawURL, config.EnvArkClawKey, config.EnvArkClawClawID)
	}

	// 与非流式同一条契约：A2A 只提供「按 contextId 续接」，没有「查询最近上下文」的接口。
	if req.Continue && req.SessionID == "" {
		return StreamResult{}, fmt.Errorf("arkclaw: 不支持 continue（A2A 无「查询最近上下文」接口），请用 --session <context_id> 显式续接")
	}
	// 模型由 clawId 绑定，-m 在 arkclaw 上无作用（与 Complete 同一告警）。
	if req.Model != "" {
		fmt.Fprintf(stderr, "  ⚠ arkclaw: --model %s 不生效（实测网关按 clawId 绑死为 kimi-k2.6），要换模型请改配置 claw_id\n", req.Model)
	}

	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return StreamResult{}, fmt.Errorf("arkclaw: empty prompt")
	}

	target, err := arkClawEndpointURL(endpoint, key, clawID)
	if err != nil {
		return StreamResult{}, fmt.Errorf("arkclaw: 端点无效: %w", err)
	}

	body, err := func() ([]byte, error) {
		reqObj, berr := arkClawBuildRequest(arkClawMethodStream, prompt, req.SessionID, req.Attachments)
		if berr != nil {
			return nil, berr
		}
		return json.Marshal(reqObj)
	}()
	if err != nil {
		return StreamResult{}, fmt.Errorf("arkclaw: 构造请求失败: %w", err)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = e.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultArkClawTimeout
	}
	httpCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(httpCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return StreamResult{}, fmt.Errorf("arkclaw: 构造 HTTP 请求失败: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")

	client := e.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return StreamResult{}, fmt.Errorf("arkclaw: %w", err)
	}
	defer httpResp.Body.Close()

	// 网关没按 SSE 回（老网关 / message/stream 不认）：退回一次性响应，
	// 把正文补发成一条 text 增量并在 stderr 说明 —— 「装了就能用」不该被流式通道打断。
	if !arkClawIsEventStream(httpResp.Header.Get("content-type")) {
		raw, rerr := io.ReadAll(io.LimitReader(httpResp.Body, maxArkClawBody))
		if rerr != nil {
			return StreamResult{}, fmt.Errorf("arkclaw: 读取响应失败: %w", rerr)
		}
		resp, perr := arkClawParseResponse(httpResp.StatusCode, raw)
		resp.Latency, resp.Engine = time.Since(start), e.Name()
		if resp.SessionID == "" {
			resp.SessionID = req.SessionID
		}
		if perr != nil {
			return StreamResult{Response: resp}, perr
		}
		fmt.Fprintln(stderr, "magic-agent: arkclaw 网关未按 SSE 回（message/stream 不可用？），本轮按一次性响应处理（无增量）")
		// 一次性响应也走 tracker：一次性只是**没有增量帧**，不是**没有状态收敛**。
		// 收尾发 turn_end（final）而不是又一条 text，与 SSE 路径同一约定。
		if onEvent != nil && resp.Text != "" {
			fst := &arkClawStreamState{onEvent: onEvent, sessionID: resp.SessionID}
			// 原生 id 与 SSE 路径同源：一次性 body 里也有 result.id（task id），
			// 用 arkClawParseResponse 解析出的响应拿不到它，所以这里按帧重解一次。
			if id := arkClawTaskID(raw); id != "" {
				fst.trackers().Begin(id)
			}
			ev := StreamEvent{Kind: KindText, Text: resp.Text, SessionID: resp.SessionID}
			// 一次性响应是整段到达，没有前序增量：snapshot 就是全文本身。
			fst.trackers().nextWith(StatusRunning, resp.Text).apply(&ev)
			onEvent(ev)
			end := StreamEvent{Kind: KindTurnEnd, Text: resp.Text, SessionID: resp.SessionID}
			if attrs, ok := fst.trackers().Finalize(resp.Text); ok {
				attrs.apply(&end)
			}
			onEvent(end)
		}
		return StreamResult{Response: resp}, nil
	}

	st := &arkClawStreamState{onEvent: onEvent}
	sc := bufio.NewScanner(httpResp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), arkClawSSEMaxLine)
	for sc.Scan() {
		payload, ok := arkClawSSEPayload(sc.Text())
		if !ok {
			continue // 空行 / ":" 心跳 / event: 等字段
		}
		done, herr := st.handleFrame([]byte(payload), req.JSONSchema)
		if herr != nil {
			st.fail(herr)
			return st.result(e, req, start), herr
		}
		if done {
			break // 终态帧（completed / failed）：网关随后即关流，不必再等 EOF
		}
	}
	if serr := sc.Err(); serr != nil {
		// 超时/取消会在读 body 时爆出来（请求头早就到了，卡的是流）。
		if cerr := httpCtx.Err(); cerr != nil {
			err := fmt.Errorf("arkclaw: 流式读取中断: %w", cerr)
			st.fail(err)
			return st.result(e, req, start), err
		}
		err := fmt.Errorf("arkclaw: 读取流式响应失败: %w", serr)
		st.fail(err)
		return st.result(e, req, start), err
	}

	if !st.completed {
		err := fmt.Errorf("arkclaw: 流式响应结束但没有 completed 帧（最后状态 %q）", st.state)
		st.fail(err)
		return st.result(e, req, start), err
	}
	res := st.result(e, req, start)
	if strings.TrimSpace(res.Text) == "" {
		// 正文空 → 整轮没有可用输出。item 可能已建立（网关发过 artifact），
		// 必须收成 failed，否则那条 item 永远停在 running。
		err := fmt.Errorf("arkclaw: 网关返回空正文（流式）")
		st.fail(err)
		return res, err
	}
	return res, nil
}

// handleFrame 处理一帧 SSE 数据。返回 done=true 表示这帧是终态（可以停读）。
func (st *arkClawStreamState) handleFrame(raw []byte, schema *JSONSchema) (bool, error) {
	env := arkClawEnvelope{}
	if err := json.Unmarshal(raw, &env); err != nil {
		return false, nil // 非 JSON 帧（心跳/日志混入）：忽略，不打断整轮
	}
	if env.Error != nil {
		return true, fmt.Errorf("arkclaw: JSON-RPC error %d: %s", env.Error.Code, arkClawCollapse(env.Error.Message))
	}
	if env.Result == nil {
		return false, nil // 没有 result 的帧（通知/心跳）：忽略
	}

	res := *env.Result
	if id := strings.TrimSpace(res.ContextID); id != "" {
		st.sessionID = id
	}
	/* 原生 item id 用 **taskId（res.ID）** 而不是 contextId —— 两者语义不同，
	   用错会让两轮共用一个 item：
	   - contextId 是**会话**锚点，跨轮不变（下一轮带着它续接，见
	     arkClawBuildRequest 的 contextID 参数），拿它当 item id 的话
	     一个会话里的每一轮都是同一个 item，后一轮覆盖前一轮
	     （症状：多轮对话只剩最后一次回复）。
	   - taskId 是**单轮**任务 id，每个 message/stream 一次，天然是一轮一个 item。
	   两者都拿不到时降级到本轮内稳定的 local_<n>（见 itemTracker.ensure）。 */
	if id := strings.TrimSpace(res.ID); id != "" && !st.textStarted {
		st.trackers().Note(id)
	}
	state := strings.ToLower(strings.TrimSpace(res.Status.State))
	st.state = state

	switch state {
	case "failed", "canceled", "cancelled", "rejected", "unknown":
		detail := arkClawPickText(res)
		if detail == "" {
			detail = "(网关未给出正文)"
		}
		return true, fmt.Errorf("arkclaw: 任务终止（status.state=%s）: %s",
			res.Status.State, truncateStr(arkClawCollapse(detail), 300))

	case "completed":
		text := arkClawPickText(res)
		// JSONSchema 后处理（与 Complete / llm / openclaw 同路径）：在**发出之前**做，
		// 保证调用方拿到的增量与收尾正文是同一份。
		if schema != nil {
			if extracted, ok := extractJSONObjectStrict(text, schema); ok {
				text = extracted
			}
		}
		st.final = text
		st.completed = true
		st.emitText(text)
		// completed = 轮次终点：发 turn_end 把 item 收成 final（见 finish 的注释）。
		st.finish()
		return true, nil
	}

	// 中间态（working / submitted / 其它）：本网关没有正文可发；若某些 claw 把增量挂在
	// 中间帧的 artifact 上，这里如实按增量转发（completed 帧再来时靠前缀去重，不会重复）。
	st.emitText(arkClawPickText(res))
	return false, nil
}

// emitText 发一条正文增量。
//
// 去重规则：completed 帧给的是**整段**正文，而已发出的增量是它的前缀时只发剩下的部分；
// 若对不上（既非空、又不是前缀），宁可不发 —— 正文以 StreamResult.Text 为准，
// 重复发会让调用方看到两份内容。
func (st *arkClawStreamState) emitText(text string) {
	if text == "" {
		return
	}
	if prev := st.emitted.String(); prev != "" {
		if !strings.HasPrefix(text, prev) {
			return
		}
		text = text[len(prev):]
	}
	if text == "" {
		return
	}
	st.emitted.WriteString(text)
	if st.onEvent == nil {
		return
	}
	/* snapshot 直接取 st.emitted（累积到此刻的全文），**不能**喂给 tracker 的
	   Append —— Append 的语义是「把这段**增量**拼到已有 snapshot 后面」，
	   而这里传的是「累积到此刻的全文」，两者混用会逐帧翻倍
	   （「你好」→「你好你好」，与 codebuddy-gateway 踩过的同一个坑）。
	   正确分工见 itemtracker.go 文件头：tracker 管 id/revision/status，
	   内容累积由调用方独家负责。 */
	ev := StreamEvent{Kind: KindText, Text: text}
	st.trackers().nextWith(StatusRunning, st.emitted.String()).apply(&ev)
	st.textStarted = true
	st.onEvent(ev)
}

// finish 收尾：把 item 标成 final（completed 帧已到，正文不会再变）。
//
// 发 **KindTurnEnd** 而不是又一条 KindText，理由同 codebuddy-gateway：
// completed 是**轮次边界**而非「又一段正文」；补一条空 Text 的 KindText
// 会打破既有按 ev.Text 收集的消费者（既有测试就是这么写的）。
// arkclaw 不在 AppendSupportOf 名单里，不会启用常驻会话，
// 所以不会被误触发 keep-alive 空闲收工。
func (st *arkClawStreamState) finish() {
	if st.onEvent == nil || !st.textStarted {
		return
	}
	ev := StreamEvent{Kind: KindTurnEnd, Text: st.final, SessionID: st.sessionID}
	if attrs, ok := st.trackers().Finalize(st.emitted.String()); ok {
		attrs.apply(&ev)
	}
	st.onEvent(ev)
}

// fail 收尾异常路径：流被截断 / 任务 failed / JSON-RPC error / 空正文时，把已经发出去的
// 正文 item 收成 failed，而不是让它永远停在 running（消费方只能靠超时猜，
// 历史回放更糟：那一条永远显示「生成中」）。
//
// 语义与 finish 的分工：turn_end = 网关给了 completed 帧（成功收尾），
// turn_failed = 没有（异常收尾），带 Error / Reason。partial 的内容仍然存在，
// 只是这条 item 不会再更新（Status=failed 明确终止增长）。
func (st *arkClawStreamState) fail(err error) {
	if st.onEvent == nil || err == nil || !st.textStarted {
		return
	}
	ev := StreamEvent{
		Kind:      KindTurnFailed,
		SessionID: st.sessionID,
		Error:     err.Error(),
		Reason:    ReasonOf(err),
		Status:    StatusFailed,
	}
	if attrs, ok := st.trackers().Failed(st.emitted.String()); ok {
		attrs.apply(&ev)
	}
	st.onEvent(ev)
}

// result 把累积状态收成 StreamResult（终态与错误路径共用）。
func (st *arkClawStreamState) result(e *ArkClawEngine, req Request, start time.Time) StreamResult {
	text := st.final
	if strings.TrimSpace(text) == "" {
		text = st.emitted.String()
	}
	sid := st.sessionID
	if sid == "" {
		sid = req.SessionID // 网关没回 contextId 时沿用请求里的 id（与 Complete 一致）
	}
	return StreamResult{Response: Response{
		Engine:    e.Name(),
		Text:      text,
		SessionID: sid,
		Latency:   time.Since(start),
		Attempts:  1,
	}}
}

// arkClawSSEPayload 从一行 SSE 文本里取出 data 载荷。
// ok=false 表示这行不是数据帧：空行（帧分隔）、":xxx"（注释/心跳）、event:/id:/retry: 等字段。
func arkClawSSEPayload(line string) (string, bool) {
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

// arkClawTaskID 从一次性响应体里取 task id（result.id）。
//
// 非 SSE 回退路径专用：那里走 arkClawParseResponse 拿不到 result 结构
// （它只返回拼好的 Response），但原生 id 不该因此降级到 local_<n> ——
// 同一个 task 在 SSE 与一次性两条路径下必须得到**同一个** item id，
// 否则同一轮回复会因走不同通道而在消费方眼里裂成两条 item。
func arkClawTaskID(raw []byte) string {
	env := arkClawEnvelope{}
	if err := json.Unmarshal(raw, &env); err != nil || env.Result == nil {
		return ""
	}
	return strings.TrimSpace(env.Result.ID)
}

// arkClawIsEventStream 响应是否为 SSE（content-type 判断，容忍参数与大小写）。
func arkClawIsEventStream(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
}
