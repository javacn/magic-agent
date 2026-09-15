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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	for _, name := range []string{"claude", "codebuddy", "trae", "llm"} {
		if !SupportsStream(Lookup(name)) {
			t.Errorf("SupportsStream(%s) should be true", name)
		}
	}
}

// ── llm 引擎（HTTP SSE）流式 ────────────────────────────────

// sseServer 起一个回放给定 SSE 数据行的 httptest 服务，并把 models.json
// 指向它（单条 id=m 的条目）。
func sseServer(t *testing.T, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	withModels(t, `[{"id":"m","url":"`+srv.URL+`/chat/completions","apiKey":"k"}]`)
}

// reasoning_content 增量 → thinking 通道；content 增量 → text 通道。
func TestLLMStreamSplitsReasoningAndText(t *testing.T) {
	sseServer(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"先想一下"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"，答案是 2"}}]}`,
		`data: {"choices":[{"delta":{"content":"答案"}}]}`,
		`data: {"choices":[{"delta":{"content":"是 2"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	events, onEvent := collectEvents()
	res, err := (&LLMEngine{}).Stream(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "1+1"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "答案是 2" {
		t.Errorf("Text = %q, want 答案是 2", res.Text)
	}
	if res.Thinking != "先想一下，答案是 2" {
		t.Errorf("Thinking = %q", res.Thinking)
	}
	if res.Model != "m" {
		t.Errorf("Model = %q, want m", res.Model)
	}
	if res.Engine != "llm" {
		t.Errorf("Engine = %q, want llm", res.Engine)
	}
	if len(*events) != 4 {
		t.Fatalf("events = %d, want 4", len(*events))
	}
	if (*events)[0].Kind != KindThinking || (*events)[2].Kind != KindText {
		t.Errorf("event kinds = %v, want thinking…text…", *events)
	}
}

// 无思维链的普通模型：全部走 text 通道。
func TestLLMStreamPlainText(t *testing.T) {
	sseServer(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"普通回答第一行\n"}}]}`,
		`data: {"choices":[{"delta":{"content":"第二行"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n"))

	events, onEvent := collectEvents()
	res, err := (&LLMEngine{}).Stream(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "普通回答第一行\n第二行" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", res.Thinking)
	}
	if len(*events) != 2 {
		t.Errorf("events = %d, want 2", len(*events))
	}
}

// 正文通道为空、思维链里混着 think 标签 → 兜底剥离后作为正文。
func TestLLMStreamThinkTagFallback(t *testing.T) {
	open := tagLT + tagWord + tagGT
	closeTag := tagLT + tagSlash + tagWord + tagGT
	sseServer(t, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\""+
		open+"推理过程"+closeTag+"兜底答案\"}}]}\n"+
		"data: [DONE]\n\n")

	res, err := (&LLMEngine{}).Stream(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "兜底答案" {
		t.Errorf("Text = %q, want think 块被剥离后的正文", res.Text)
	}
}

// 流式 4xx 错误带状态码。
func TestLLMStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	t.Cleanup(srv.Close)
	withModels(t, `[{"id":"m","url":"`+srv.URL+`/chat/completions","apiKey":"k"}]`)

	_, err := (&LLMEngine{}).Stream(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want it to mention 401", err)
	}
}

// Complete：正文里残留的 think 标签被剥离。
func TestLLMCompleteStripsThinking(t *testing.T) {
	open := tagLT + tagWord + tagGT
	closeTag := tagLT + tagSlash + tagWord + tagGT
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + open + `思考过程` + closeTag + `正文答案"}}]}`))
	}))
	t.Cleanup(srv.Close)
	withModels(t, `[{"id":"m","url":"`+srv.URL+`/chat/completions","apiKey":"k"}]`)

	resp, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "正文答案" {
		t.Errorf("Text = %q, want think 块被剥离", resp.Text)
	}
}
