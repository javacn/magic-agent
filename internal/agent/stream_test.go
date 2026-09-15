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

// ── llm 引擎 openai-completions SSE 流式 ─────────────────────

func TestLLMStreamOpenAISSE(t *testing.T) {
	// 起 SSE 假服务器：reasoning_content（thinking）+ content（text）增量。
	srv := httptest.NewServer(httptestSSEHandler(`data: {"choices":[{"delta":{"reasoning_content":"先想"}}]}

data: {"choices":[{"delta":{"reasoning_content":"清楚"}}]}

data: {"choices":[{"delta":{"content":"答案"}}]}

data: {"choices":[{"delta":{"content":"是2"}}]}

data: [DONE]

`))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MAGIC_AGENT_MODELS", writeModelsJSON(t, home, srv.URL))

	e := &LLMEngine{}
	events, onEvent := collectEvents()
	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "1+1"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "答案是2" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Thinking != "先想清楚" {
		t.Errorf("Thinking = %q", res.Thinking)
	}
	if len(*events) != 4 {
		t.Errorf("events = %d, want 4", len(*events))
	}
}

// SSE 中途 HTTP 错误 → 报错。
func TestLLMStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MAGIC_AGENT_MODELS", writeModelsJSON(t, home, srv.URL))

	e := &LLMEngine{}
	_, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil {
		t.Fatal("expected error for HTTP 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should mention status: %v", err)
	}
}

// ollama 后端 → 流式明确报错。
func TestLLMStreamOllamaUnsupported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := `{"default":"local/qwen","providers":[{"name":"local","api":"ollama","models":[{"id":"qwen"}]}]}`
	path := filepath.Join(home, "models.json")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGIC_AGENT_MODELS", path)

	e := &LLMEngine{}
	_, err := e.Stream(context.Background(), Request{
		Model:    "local/qwen",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil {
		t.Fatal("expected error for ollama streaming")
	}
	if !strings.Contains(err.Error(), "ollama") {
		t.Errorf("error should mention ollama: %v", err)
	}
}

// httptestSSEHandler 返回固定 SSE body 的 handler。
func httptestSSEHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}
}

// writeModelsJSON 写一个指向 baseURL 的 openai-completions 配置。
func writeModelsJSON(t *testing.T, home, baseURL string) string {
	t.Helper()
	cfg := `{"default":"mini/MiniMax-M3","providers":[{"name":"mini","api":"openai-completions","baseUrl":"` + baseURL + `","apiKey":"test-key","models":[{"id":"MiniMax-M3"}]}]}`
	path := filepath.Join(home, "models.json")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
