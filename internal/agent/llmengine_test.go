package agent

// llmengine_test.go - llm 引擎的 HTTP 路由测试（扁平 models.json）。
//
// 本引擎只走 HTTP：不再有委托其他引擎或 ollama 分支，因此测试围绕
// 请求构造、响应解析、错误透传与 Detect 报告展开。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLLMEngineOpenAIRoute 走 HTTP：验证请求构造与响应解析。
func TestLLMEngineOpenAIRoute(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"答案是 2"}}]}`))
	}))
	defer srv.Close()

	withModels(t, `[
	  {
	    "id":"test-model",
	    "url":"`+srv.URL+`/chat/completions",
	    "apiKey":"secret",
	    "extraBody":{"thinking":{"type":"disabled"}}
	  }
	]`)

	e := &LLMEngine{}
	resp, err := e.Complete(context.Background(), Request{
		Model:        "test-model",
		SystemPrompt: "你是严谨的助手",
		Messages:     []Message{{Role: "user", Content: "1+1=?"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "答案是 2" {
		t.Errorf("Text = %q, want 答案是 2", resp.Text)
	}
	if resp.Model != "test-model" {
		t.Errorf("Model = %q, want test-model", resp.Model)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want Bearer secret", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}

	// system 作为首条消息，user 跟随。
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %#v, want 2 entries", gotBody["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是严谨的助手" {
		t.Errorf("first message = %#v, want system prompt", first)
	}
	// extraBody 必须并入请求体顶层。
	thinking, ok := gotBody["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Errorf("extraBody not merged into request: thinking=%#v", gotBody["thinking"])
	}
	// 发往 API 的模型名 = id（未配 model）。
	if gotBody["model"] != "test-model" {
		t.Errorf("model = %v, want test-model", gotBody["model"])
	}
	// 非流式。
	if gotBody["stream"] != false {
		t.Errorf("stream = %v, want false", gotBody["stream"])
	}
}

// TestLLMEngineWireModelUsed 配了 model 时发往 API 用 model，响应 Model 用 id。
func TestLLMEngineWireModelUsed(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		gotModel, _ = body["model"].(string)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	withModels(t, `[{"id":"minimax-nothink","model":"MiniMax-M3","url":"`+srv.URL+`/chat/completions","apiKey":"k"}]`)

	resp, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "minimax-nothink",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotModel != "MiniMax-M3" {
		t.Errorf("wire model = %q, want MiniMax-M3", gotModel)
	}
	if resp.Model != "minimax-nothink" {
		t.Errorf("response Model = %q, want the caller-facing id", resp.Model)
	}
}

// TestLLMEngineEndpointAppended base 地址（无 /chat/completions）被自动补全。
func TestLLMEngineEndpointAppended(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	// 注意：url 只给到 /v1，不带 /chat/completions
	withModels(t, `[{"id":"m","url":"`+srv.URL+`/v1","apiKey":"k"}]`)

	if _, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions (auto-appended)", gotPath)
	}
}

// TestLLMEngineHTTPErrorStatus 非 2xx 时错误信息带状态码（便于重试分类）。
func TestLLMEngineHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer srv.Close()

	withModels(t, `[{"id":"m","url":"`+srv.URL+`/chat/completions","apiKey":"k"}]`)

	_, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want error for HTTP 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error = %v, want it to mention 429", err)
	}
}

// TestLLMEngineProviderErrorInBody 200 + error 体也应报错。
func TestLLMEngineProviderErrorInBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request","message":"bad model"}}`))
	}))
	defer srv.Close()

	withModels(t, `[{"id":"m","url":"`+srv.URL+`/chat/completions","apiKey":"k"}]`)

	_, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "bad model") {
		t.Fatalf("error = %v, want provider error surfaced", err)
	}
}

// TestLLMEngineUnknownModel 未知模型报错带配置路径。
func TestLLMEngineUnknownModel(t *testing.T) {
	p := withModels(t, `[{"id":"m","url":"http://x/v1/chat/completions","apiKey":"k"}]`)

	_, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "nope",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want error for unknown model")
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("error should include config path %s: %v", p, err)
	}
}

// TestLLMEngineEmptyContent 200 但正文为空时报错。
func TestLLMEngineEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
	}))
	defer srv.Close()

	withModels(t, `[{"id":"m","url":"`+srv.URL+`/chat/completions","apiKey":"k"}]`)

	_, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "empty content") {
		t.Fatalf("error = %v, want empty content", err)
	}
}

// TestLLMEngineDetectReportsConfig Detect 成功时报告条目数与默认 id。
func TestLLMEngineDetectReportsConfig(t *testing.T) {
	withModels(t, sampleModels)

	ok, note := (&LLMEngine{}).Detect()
	if !ok {
		t.Fatalf("Detect = false: %s", note)
	}
	if !strings.Contains(note, "3 models") {
		t.Errorf("note = %q, want model count", note)
	}
	if !strings.Contains(note, "default=MiniMax-M3") {
		t.Errorf("note = %q, want default id", note)
	}
}

// TestLLMEngineDetectMissingConfig 配置缺失时 Detect 报不可用并给出补救提示。
func TestLLMEngineDetectMissingConfig(t *testing.T) {
	t.Setenv("MAGIC_AGENT_MODELS", "/nonexistent/models.json")
	t.Setenv("HOME", t.TempDir())

	ok, note := (&LLMEngine{}).Detect()
	if ok {
		t.Fatal("Detect should fail without config")
	}
	if !strings.Contains(note, "models.json") {
		t.Errorf("note = %q, want hint to create models.json", note)
	}
}

// TestLLMEngineDetectNoKey 默认条目无 key 时给出提示（本地端点场景）。
func TestLLMEngineDetectNoKey(t *testing.T) {
	withModels(t, `[{"id":"local","url":"http://localhost:11434/v1/chat/completions"}]`)

	ok, note := (&LLMEngine{}).Detect()
	if !ok {
		t.Fatalf("Detect = false: %s", note)
	}
	if !strings.Contains(note, "no apiKey") {
		t.Errorf("note = %q, want a no-apiKey hint", note)
	}
}

// TestBuildChatPayload 请求体构造：空 system 不产生 system 消息。
func TestBuildChatPayload(t *testing.T) {
	req := buildChatPayload(ModelEntry{}, "m", "  ", []Message{{Role: "user", Content: "hi"}})
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (blank system skipped)", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		t.Errorf("role = %q, want user", req.Messages[0].Role)
	}
	if !req.ReasoningSplit {
		t.Error("ReasoningSplit should be true (MiniMax thinking split)")
	}
	if req.Stream {
		t.Error("Stream should be false")
	}
	if req.Temperature != nil {
		t.Error("Temperature should be nil when unset (omitempty)")
	}

	// 空 role 兜底为 user。
	req2 := buildChatPayload(ModelEntry{}, "m", "", []Message{{Content: "x"}})
	if req2.Messages[0].Role != "user" {
		t.Errorf("empty role should default to user, got %q", req2.Messages[0].Role)
	}
}

// TestBuildChatPayloadTemperature temperature 透传。
func TestBuildChatPayloadTemperature(t *testing.T) {
	temp := 0.3
	req := buildChatPayload(ModelEntry{Temperature: &temp}, "m", "", []Message{{Role: "user", Content: "hi"}})
	if req.Temperature == nil || *req.Temperature != 0.3 {
		t.Errorf("Temperature = %v, want 0.3", req.Temperature)
	}
}
