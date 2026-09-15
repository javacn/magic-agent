package agent

// llmengine_test.go - llm 引擎的路由与 HTTP 客户端测试。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLLMEngineOpenAIRoute 走 HTTP provider：验证请求构造与响应解析。
func TestLLMEngineOpenAIRoute(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"答案是 2"}}]}`))
	}))
	defer srv.Close()

	withModels(t, `{
	  "providers":[{
	    "name":"mock","baseUrl":"`+srv.URL+`","apiKey":"secret",
	    "models":[{"id":"test-model"}],
	    "extraBody":{"thinking":{"type":"disabled"}}
	  }]
	}`)

	e := &LLMEngine{}
	resp, err := e.Complete(context.Background(), Request{
		Model:        "mock/test-model",
		SystemPrompt: "你是严谨的助手",
		Messages:     []Message{{Role: "user", Content: "1+1=?"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "答案是 2" {
		t.Errorf("Text = %q, want 答案是 2", resp.Text)
	}
	if resp.Model != "mock/test-model" {
		t.Errorf("Model = %q, want mock/test-model", resp.Model)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want Bearer secret", gotAuth)
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
	// 模型名应剥掉 provider 前缀。
	if gotBody["model"] != "test-model" {
		t.Errorf("model = %v, want test-model", gotBody["model"])
	}
	// 非流式。
	if gotBody["stream"] != false {
		t.Errorf("stream = %v, want false", gotBody["stream"])
	}
}

// TestLLMEngineHTTPErrorStatus 非 2xx 时错误信息带状态码（便于重试分类）。
func TestLLMEngineHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer srv.Close()

	withModels(t, `{"providers":[{"name":"mock","baseUrl":"`+srv.URL+`","models":[{"id":"m"}]}]}`)

	_, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "mock/m",
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

	withModels(t, `{"providers":[{"name":"mock","baseUrl":"`+srv.URL+`","models":[{"id":"m"}]}]}`)

	_, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "mock/m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "bad model") {
		t.Fatalf("error = %v, want provider error surfaced", err)
	}
}

// TestLLMEngineMissingBaseURL HTTP provider 缺 baseUrl 时报错。
func TestLLMEngineMissingBaseURL(t *testing.T) {
	withModels(t, `{"providers":[{"name":"mock","api":"openai-completions","models":[{"id":"m"}]}]}`)

	_, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "mock/m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "baseUrl is required") {
		t.Fatalf("error = %v, want baseUrl required", err)
	}
}

// TestLLMEngineUnsupportedAPI 未知 api 类型报错并列出支持列表。
func TestLLMEngineUnsupportedAPI(t *testing.T) {
	withModels(t, `{"providers":[{"name":"weird","api":"quantum-telepathy","models":[{"id":"m"}]}]}`)

	_, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "weird/m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported api") {
		t.Fatalf("error = %v, want unsupported api", err)
	}
}

// TestLLMEngineUnknownModel 未知模型报错带配置路径。
func TestLLMEngineUnknownModel(t *testing.T) {
	p := withModels(t, `{"providers":[{"name":"mock","baseUrl":"http://x/v1","models":[{"id":"m"}]}]}`)

	_, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "mock/nope",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want error for unknown model")
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("error should include config path %s: %v", p, err)
	}
}

// TestLLMEngineDelegatesToCLI codebuddy-cli provider 委托给 CodeBuddyEngine。
// 用不存在的 CLI 让其预检失败，从而验证「走的是委托路径」。
func TestLLMEngineDelegatesToCLI(t *testing.T) {
	t.Setenv("MAGIC_AGENT_CODEBUDDY_BIN", "")
	t.Setenv("PATH", t.TempDir()) // 确保 PATH 里没有 codebuddy
	withModels(t, `{"providers":[{"name":"cb","api":"codebuddy-cli","models":[{"id":"hy3"}]}]}`)

	_, err := (&LLMEngine{}).Complete(context.Background(), Request{
		Model:    "cb/hy3",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want error when delegated CLI unavailable")
	}
	// 错误里应点明是委托到哪个引擎。
	if !strings.Contains(err.Error(), "codebuddy") {
		t.Errorf("error = %v, want it to name the delegated engine", err)
	}
}

// TestLLMEngineDetectReportsConfig Detect 成功时报告 provider 数量与默认模型。
func TestLLMEngineDetectReportsConfig(t *testing.T) {
	withModels(t, sampleModels)

	ok, note := (&LLMEngine{}).Detect()
	if !ok {
		t.Fatalf("Detect = false: %s", note)
	}
	if !strings.Contains(note, "3 providers") {
		t.Errorf("note = %q, want provider count", note)
	}
	if !strings.Contains(note, "minimax/MiniMax-M3") {
		t.Errorf("note = %q, want default model", note)
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

// TestStripOllamaSpinner braille spinner 去除。
func TestStripOllamaSpinner(t *testing.T) {
	in := "\u280b \u2809 \u28f4 \u28e7 你好。"
	if got := stripOllamaSpinner(in); got != "你好。" {
		t.Errorf("stripOllamaSpinner = %q, want 你好。", got)
	}
}

// TestBuildChatPayload 请求体构造：空 system 不产生 system 消息。
func TestBuildChatPayload(t *testing.T) {
	req := buildChatPayload(Provider{}, "m", "  ", []Message{{Role: "user", Content: "hi"}})
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

	// 空 role 兜底为 user。
	req2 := buildChatPayload(Provider{}, "m", "", []Message{{Content: "x"}})
	if req2.Messages[0].Role != "user" {
		t.Errorf("empty role should default to user, got %q", req2.Messages[0].Role)
	}
}
