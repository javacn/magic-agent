package agent

// arkclaw_stream_test.go - ArkClaw 流式通道（A2A message/stream 的 SSE）单元测试。
//
// 覆盖：接口契约（实现了 Streamer + sse 标记）、请求形状（method / accept / contextId
// 续接落点）、SSE 逐帧解析（心跳与分隔行跳过、working 帧不发事件、completed 发正文）、
// 整段重复去重、中间 artifact 增量转发、失败态 / JSON-RPC error / 401 / 无终态帧 /
// 空正文、非 SSE 响应的一次性回退、JSONSchema 后处理、超时中断。
//
// 全部走 httptest，不触网（真实网关的行为见 arkclaw_stream.go 文件头的实测记录）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- 假网关与帧构造 ----------

// arkClawSSEServer 起一个假网关，把 body 原样当 SSE 回（content-type: text/event-stream）。
func arkClawSSEServer(t *testing.T, body string) (*httptest.Server, *arkClawRec) {
	t.Helper()
	return arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		_, _ = io.WriteString(w, body)
	})
}

// arkClawSSEFrame 组一帧 SSE（`data: <json>` + 空行分隔）。
func arkClawSSEFrame(jsonBody string) string { return "data: " + jsonBody + "\n\n" }

// arkClawWorkingFrame 组一个受理帧（state=working，无正文）—— 真实网关的**第一帧**长这样。
func arkClawWorkingFrame(contextID string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":"req-1","result":{"kind":"task","id":"task-1",`+
		`"contextId":%q,"status":{"state":"working","timestamp":"2026-09-22T07:31:28.894Z"},`+
		`"history":[]}}`, contextID)
}

// arkClawArtifactFrame 组一个把正文挂在 artifacts 上的帧（网关将来吐增量时的形态）。
func arkClawArtifactFrame(state, contextID, text string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":"req-1","result":{"kind":"task","id":"task-1",`+
		`"contextId":%q,"status":{"state":%q},"artifacts":[{"artifactId":"a1",`+
		`"parts":[{"kind":"text","text":%q}]}]}}`, contextID, state, text)
}

// ---------- 接口契约 ----------

func TestArkClawIsStreamer(t *testing.T) {
	// 流式走 A2A 官方 SSE（message/stream）：**不是** WebSocket —— 实测网关不回 101
	// 升级响应，且 A2A 规范里 WS 属自定义绑定、火山网关也没有 WS API 类型。
	e := &ArkClawEngine{}
	if s := AsStreamer(e); s == nil {
		t.Fatal("arkclaw 应实现 Streamer（走 A2A 官方 SSE，见 arkclaw_stream.go）")
	}
	if !SupportsStream(e) {
		t.Error("SupportsStream(arkclaw) 应为 true（`--engines` 的 streaming 字段由它决定）")
	}
	if got := streamArgsFor(e); len(got) != 1 || got[0] != "sse" {
		t.Errorf("流式标记 = %v，期望 [sse]", got)
	}
}

// ---------- 正常路径 ----------

func TestArkClawStreamSSECompleted(t *testing.T) {
	const text = "西湖，位于浙江省杭州市城西，是中国最著名的淡水湖泊之一。"
	body := arkClawSSEFrame(arkClawWorkingFrame("ctx-s1")) +
		arkClawSSEFrame(arkClawTextEnvelope(text, "ctx-s1"))
	srv, rec := arkClawSSEServer(t, body)
	e := arkClawServerEngine(srv)

	events, onEvent := collectEvents()
	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "介绍一下西湖"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// 请求形状：凭据仍走 query，method 换成 message/stream，并要求 SSE 响应。
	if rec.Path != "/a2a/jsonrpc" {
		t.Errorf("path = %q", rec.Path)
	}
	if rec.Query["apikey"] != "test-key" || rec.Query["clawId"] != "ci-test" {
		t.Errorf("query = %v", rec.Query)
	}
	if got := rec.Header.Get("accept"); got != "text/event-stream" {
		t.Errorf("accept = %q want text/event-stream", got)
	}
	var sent arkClawRequest
	if err := json.Unmarshal([]byte(rec.Body), &sent); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	if sent.Method != "message/stream" {
		t.Errorf("method = %q want message/stream", sent.Method)
	}

	// 事件：working 帧**不发事件**（网关没有增量可发），completed 帧发一条正文增量。
	if len(*events) != 1 {
		t.Fatalf("事件数 = %d want 1: %+v", len(*events), *events)
	}
	if ev := (*events)[0]; ev.Kind != KindText || ev.Text != text {
		t.Errorf("事件 = %+v want KindText/%q", ev, text)
	}

	if res.Text != text {
		t.Errorf("Text = %q", res.Text)
	}
	if res.SessionID != "ctx-s1" {
		t.Errorf("SessionID = %q want ctx-s1", res.SessionID)
	}
	if res.Engine != "arkclaw" || res.Attempts != 1 || res.Latency <= 0 {
		t.Errorf("res = %+v", res.Response)
	}
}

func TestArkClawStreamSkipsHeartbeatAndNonDataLines(t *testing.T) {
	// SSE 里的分隔空行、":" 注释（心跳）、event:/id:/retry: 字段与 [DONE] 都不是数据帧。
	body := ": keep-alive\n\n" +
		"event: task\nid: 7\nretry: 1000\n" +
		arkClawSSEFrame(arkClawTextEnvelope("正文", "c1")) +
		"data: [DONE]\n\n"
	srv, _ := arkClawSSEServer(t, body)

	events, onEvent := collectEvents()
	res, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(*events) != 1 || (*events)[0].Text != "正文" {
		t.Errorf("事件 = %+v want 单条正文", *events)
	}
	if res.Text != "正文" {
		t.Errorf("Text = %q", res.Text)
	}
}

func TestArkClawStreamDedupWhenArtifactsRepeat(t *testing.T) {
	// 实测：completed 帧把同一份正文**同时**放在 status.message 与 artifacts 里。
	// 两处都发会让调用方看到两份 → 必须只发一次。
	frame := `{"jsonrpc":"2.0","id":"req-1","result":{"kind":"task","id":"t","contextId":"c1",` +
		`"status":{"state":"completed","message":{"kind":"message","role":"agent",` +
		`"parts":[{"kind":"text","text":"同一份正文"}]}},` +
		`"artifacts":[{"artifactId":"a1","parts":[{"kind":"text","text":"同一份正文"}]}]}}`
	srv, _ := arkClawSSEServer(t, arkClawSSEFrame(frame))

	events, onEvent := collectEvents()
	res, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("事件数 = %d want 1（同一份正文不得发两次）: %+v", len(*events), *events)
	}
	if res.Text != "同一份正文" {
		t.Errorf("Text = %q", res.Text)
	}
}

func TestArkClawStreamForwardsArtifactIncrements(t *testing.T) {
	// 中间帧若挂了正文（网关当前不发，留着以防将来漏掉）：按增量转发，
	// completed 帧的整段正文只补发**剩下的部分**（前缀去重），不重复。
	body := arkClawSSEFrame(arkClawArtifactFrame("working", "c1", "第一段")) +
		arkClawSSEFrame(arkClawTextEnvelope("第一段第二段", "c1"))
	srv, _ := arkClawSSEServer(t, body)

	events, onEvent := collectEvents()
	res, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(*events) != 2 {
		t.Fatalf("事件数 = %d want 2: %+v", len(*events), *events)
	}
	if (*events)[0].Text != "第一段" || (*events)[1].Text != "第二段" {
		t.Errorf("增量 = %q / %q", (*events)[0].Text, (*events)[1].Text)
	}
	if res.Text != "第一段第二段" {
		t.Errorf("Text = %q", res.Text)
	}
}

func TestArkClawStreamResumeUsesMessageContextID(t *testing.T) {
	srv, rec := arkClawSSEServer(t, arkClawSSEFrame(arkClawTextEnvelope("紫色大象", "ctx-prev")))
	e := arkClawServerEngine(srv)

	res, err := e.Stream(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "暗号是什么"}},
		SessionID: "ctx-prev",
	}, func(StreamEvent) {})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.SessionID != "ctx-prev" {
		t.Errorf("SessionID = %q want ctx-prev", res.SessionID)
	}

	// 续接 id 必须落在 message.contextId（外层 params.contextId 会被网关忽略）。
	var outer map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rec.Body), &outer); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(outer["params"], &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if raw, ok := params["contextId"]; ok {
		t.Errorf("外层 params.contextId 不应出现: %s", raw)
	}
	var msg struct {
		ContextID string `json:"contextId"`
	}
	if err := json.Unmarshal(params["message"], &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if msg.ContextID != "ctx-prev" {
		t.Errorf("message.contextId = %q want ctx-prev", msg.ContextID)
	}
}

func TestArkClawStreamJSONSchemaPostProcess(t *testing.T) {
	// JSONSchema 后处理要在**发出增量之前**做，保证事件里的正文与收尾正文是同一份。
	body := arkClawSSEFrame(arkClawTextEnvelope(
		"好的，结果如下：\n```json\n{\"city\":\"北京\",\"temp\":21}\n```\n需要我继续吗？", "c1"))
	srv, _ := arkClawSSEServer(t, body)

	events, onEvent := collectEvents()
	res, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "北京天气"}},
		JSONSchema: &JSONSchema{
			Type: "object",
			Properties: map[string]map[string]any{
				"city": {"type": "string"},
				"temp": {"type": "number"},
			},
			Required: []string{"city", "temp"},
		},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("Text 不是纯 JSON: %v (%q)", err, res.Text)
	}
	if len(*events) != 1 || (*events)[0].Text != res.Text {
		t.Errorf("增量应与收尾正文一致: events=%+v text=%q", *events, res.Text)
	}
}

// ---------- 错误路径 ----------

func TestArkClawStreamTaskFailed(t *testing.T) {
	frame := `{"jsonrpc":"2.0","id":"req-1","result":{"kind":"task","id":"t","contextId":"ctx-9",` +
		`"status":{"state":"failed","message":{"kind":"message","parts":[{"kind":"text","text":"上游超时"}]}}}}`
	srv, _ := arkClawSSEServer(t, arkClawSSEFrame(arkClawWorkingFrame("ctx-9"))+arkClawSSEFrame(frame))

	res, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(StreamEvent) {})
	if err == nil {
		t.Fatal("expected error for status.state=failed")
	}
	if !strings.Contains(err.Error(), "failed") || !strings.Contains(err.Error(), "上游超时") {
		t.Errorf("err = %v", err)
	}
	// 失败也要把 contextId 交回调用方（排查 / 续接都要它）。
	if res.SessionID != "ctx-9" {
		t.Errorf("SessionID = %q want ctx-9", res.SessionID)
	}
}

func TestArkClawStreamRPCError(t *testing.T) {
	srv, _ := arkClawSSEServer(t, arkClawSSEFrame(
		`{"jsonrpc":"2.0","id":"req-1","error":{"code":-32602,"message":"invalid params"}}`))

	_, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(StreamEvent) {})
	if err == nil || !strings.Contains(err.Error(), "invalid params") {
		t.Errorf("err = %v", err)
	}
}

func TestArkClawStreamHTTP401PlainText(t *testing.T) {
	// 鉴权失败：网关回 401 + text/plain（非 SSE）→ 走一次性响应那条路，错误必须带状态码与正文。
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		w.Header().Set("content-type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "External authentication failed.")
	})

	_, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(StreamEvent) {})
	if err == nil {
		t.Fatal("expected 401 error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "External authentication failed") {
		t.Errorf("err = %v", err)
	}
}

func TestArkClawStreamMissingCompletedFrame(t *testing.T) {
	// 流断在没有终态帧的地方（网关半途掉线）→ 必须明确报「没有 completed 帧」，
	// 不能拿空正文糊弄过去。
	srv, _ := arkClawSSEServer(t, arkClawSSEFrame(arkClawWorkingFrame("c1")))

	_, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(StreamEvent) {})
	if err == nil || !strings.Contains(err.Error(), "completed 帧") {
		t.Errorf("err = %v want 缺 completed 帧", err)
	}
}

func TestArkClawStreamEmptyText(t *testing.T) {
	frame := `{"jsonrpc":"2.0","id":"req-1","result":{"kind":"task","id":"t","contextId":"c",` +
		`"status":{"state":"completed","message":{"parts":[]}}}}`
	srv, _ := arkClawSSEServer(t, arkClawSSEFrame(frame))

	_, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(StreamEvent) {})
	if err == nil || !strings.Contains(err.Error(), "空正文") {
		t.Errorf("err = %v want 空正文", err)
	}
}

func TestArkClawStreamContinueUnsupported(t *testing.T) {
	e := &ArkClawEngine{URL: "https://h/a2a/jsonrpc", Key: "k", ClawID: "c"}
	_, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Continue: true,
	}, func(StreamEvent) {})
	if err == nil || (!strings.Contains(err.Error(), "continue") && !strings.Contains(err.Error(), "续接")) {
		t.Errorf("err = %v", err)
	}
}

func TestArkClawStreamNotConfigured(t *testing.T) {
	clearArkClawEnv(t)
	e := &ArkClawEngine{ConfigPath: t.TempDir() + "/absent.json"}
	_, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(StreamEvent) {})
	if err == nil || !strings.Contains(err.Error(), "未配置") {
		t.Errorf("err = %v want 未配置", err)
	}
}

func TestArkClawStreamEmptyPrompt(t *testing.T) {
	e := &ArkClawEngine{URL: "https://h/a2a/jsonrpc", Key: "k", ClawID: "c"}
	_, err := e.Stream(context.Background(), Request{}, func(StreamEvent) {})
	if err == nil || !strings.Contains(err.Error(), "empty prompt") {
		t.Errorf("err = %v want empty prompt", err)
	}
}

// ---------- 回退与超时 ----------

func TestArkClawStreamNonSSEFallback(t *testing.T) {
	// 网关没按 SSE 回（老网关 / 不认 message/stream）→ 按一次性响应处理并把正文补发成一条增量，
	// 而不是拿「没有 completed 帧」报错。
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, arkClawTextEnvelope("一次性正文", "ctx-f1"))
	})

	old := stderr
	defer func() { stderr = old }()
	var buf strings.Builder
	stderr = &buf

	events, onEvent := collectEvents()
	res, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "一次性正文" || res.SessionID != "ctx-f1" {
		t.Errorf("res = %+v", res.Response)
	}
	if len(*events) != 1 || (*events)[0].Text != "一次性正文" {
		t.Errorf("事件 = %+v want 单条正文", *events)
	}
	if !strings.Contains(buf.String(), "SSE") {
		t.Errorf("回退应在 stderr 说明原因: %q", buf.String())
	}
}

func TestArkClawStreamTimeoutInterruptsRead(t *testing.T) {
	// 长任务中网关只回了受理帧就「卡住」：超时必须能打断读流（请求头早到了，卡的是 body）。
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, arkClawSSEFrame(arkClawWorkingFrame("c1")))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done() // 客户端取消（超时）后立刻收工，不拖慢测试
	})

	start := time.Now()
	_, err := arkClawServerEngine(srv).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Timeout:  300 * time.Millisecond,
	}, func(StreamEvent) {})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("超时未生效: %v", elapsed)
	}
}
