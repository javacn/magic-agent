package agent

// llmhttp_test.go - llm 引擎「配置文件模型」直连链路的单测（httptest 打桩，不碰真网络）。
//
// 覆盖：
//   - models.json 解析 / 查表（id 精确 + 忽略大小写；空 -m → 首条；未知 id → 不命中）
//   - 非流式直连：真实模型名（model 字段）、extraBody 落请求体顶层、role 消息、token 计数、
//     <think> 标签剥离
//   - 流式直连：SSE delta 分流 thinking/text、**标签被分块切开**也要正确剥离、[DONE] 收尾
//   - 错误：非 2xx → `HTTP <code>: 片段`（isRetryable 认这个前缀）
//   - 路由：命中配置文件时**不启动 llm CLI**（BinPath 指向不存在的文件也必须成功）

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeModelsJSON 造一份临时 models.json 并把它设为配置来源。
// 注意 URL 占位符由调用方替换成 httptest 的地址。
func writeModelsJSON(t *testing.T, entries []map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "models.json")
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal models.json: %v", err)
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write models.json: %v", err)
	}
	t.Setenv("MAGIC_AGENT_MODELS", p)
	return p
}

func TestLoadAndLookupLLMConfigModels(t *testing.T) {
	writeModelsJSON(t, []map[string]any{
		{"id": "MiniMax-M3", "name": "MiniMax-M3", "url": "http://127.0.0.1:1/v1/chat/completions", "apiKey": "k1"},
		{"id": "minimax-nothink", "model": "MiniMax-M3", "name": "MiniMax-M3 (no thinking)",
			"url": "http://127.0.0.1:1/v1", "apiKey": "k2",
			"extraBody": map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{"name": "没有 id 的行应被忽略", "url": "http://127.0.0.1:1/v1"},
	})

	list, err := LoadLLMConfigModels()
	if err != nil {
		t.Fatalf("LoadLLMConfigModels: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应跳过没有 id 的行，得到 %d 条", len(list))
	}

	// 精确 + 忽略大小写
	if m, ok := LookupLLMConfigModel("minimax-nothink"); !ok || m.WireModel() != "MiniMax-M3" {
		t.Errorf("minimax-nothink 应命中且真实模型名为 MiniMax-M3，得 %+v ok=%v", m, ok)
	}
	if m, ok := LookupLLMConfigModel("minimax-m3"); !ok || m.ID != "MiniMax-M3" {
		t.Errorf("大小写不同的 id 也应命中，得 %+v ok=%v", m, ok)
	}
	if _, ok := LookupLLMConfigModel("nope"); ok {
		t.Error("未知 id 不应命中")
	}
	// 空 -m → 首条
	if m, ok := ResolveLLMConfigModel(""); !ok || m.ID != "MiniMax-M3" {
		t.Errorf("空 -m 应取首条，得 %+v ok=%v", m, ok)
	}
	// 端点补全：只给 base 的要补 /chat/completions，写全的保持原样
	if got := list[1].Endpoint(); got != "http://127.0.0.1:1/v1/chat/completions" {
		t.Errorf("Endpoint 补全失败: %s", got)
	}
	if got := list[0].Endpoint(); got != "http://127.0.0.1:1/v1/chat/completions" {
		t.Errorf("已写全的端点不应被改写: %s", got)
	}
}

func TestResolveLLMConfigModelWithoutFile(t *testing.T) {
	t.Setenv("MAGIC_AGENT_MODELS", filepath.Join(t.TempDir(), "nope.json"))
	if _, ok := ResolveLLMConfigModel(""); ok {
		t.Error("没有配置文件时空 -m 不应命中（应回退 llm CLI 默认）")
	}
	if _, ok := ResolveLLMConfigModel("MiniMax-M3"); ok {
		t.Error("没有配置文件时任何 id 都不应命中")
	}
}

func TestCompleteConfiguredDirect(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"<think>内部推理</think>你好"}}],`+
			`"usage":{"prompt_tokens":11,"completion_tokens":3}}`)
	}))
	defer srv.Close()

	writeModelsJSON(t, []map[string]any{
		{"id": "MiniMax-M3", "name": "MiniMax-M3", "url": srv.URL + "/v1", "apiKey": "sk-test"},
		{"id": "minimax-nothink", "model": "MiniMax-M3", "name": "MiniMax-M3 (no thinking)",
			"url": srv.URL + "/v1/chat/completions", "apiKey": "sk-test",
			"extraBody": map[string]any{"thinking": map[string]any{"type": "disabled"}}},
	})

	e := &LLMEngine{BinPath: "/nonexistent/llm"} // 命中配置文件就不该去启动 CLI
	resp, err := e.Complete(context.Background(), Request{
		Engine:       "llm",
		Model:        "minimax-nothink",
		SystemPrompt: "你是助手",
		Messages:     []Message{{Role: "user", Content: "只回两个字：你好"}},
		MaxTokens:    64,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "你好" {
		t.Errorf("思维链剥离后正文应为「你好」，得 %q", resp.Text)
	}
	if resp.Model != "minimax-nothink" {
		t.Errorf("Model 应回填配置里的 id（唯一标识，真实名会重复），得 %q", resp.Model)
	}
	if resp.InputTokens != 11 || resp.OutputTokens != 3 || resp.TotalTokens != 14 {
		t.Errorf("token 计数应回填 11/3/14，得 %d/%d/%d", resp.InputTokens, resp.OutputTokens, resp.TotalTokens)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("鉴权头应带 Bearer apiKey，得 %q", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("端点补全错误: %s", gotPath)
	}
	// 关键契约：model = 真实模型名；extraBody 落到请求体顶层；messages 用原生 role
	if gotBody["model"] != "MiniMax-M3" {
		t.Errorf("请求体 model 应为真实模型名，得 %v", gotBody["model"])
	}
	if gotBody["stream"] != false {
		t.Errorf("非流式应 stream=false，得 %v", gotBody["stream"])
	}
	if gotBody["max_tokens"] != float64(64) {
		t.Errorf("调用方给的 max_tokens 应透传，得 %v", gotBody["max_tokens"])
	}
	thinking, _ := gotBody["thinking"].(map[string]any)
	if thinking == nil || thinking["type"] != "disabled" {
		t.Errorf("extraBody 未 merge 到请求体顶层: %v", gotBody["thinking"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 应有 system+user 两条，得 %v", gotBody["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是助手" {
		t.Errorf("system 消息不对: %v", first)
	}
}

func TestCompleteConfiguredHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "External\nauthentication failed.")
	}))
	defer srv.Close()
	writeModelsJSON(t, []map[string]any{{"id": "m1", "url": srv.URL + "/v1", "apiKey": "bad"}})

	_, err := (&LLMEngine{BinPath: "/nonexistent/llm"}).Complete(context.Background(),
		Request{Engine: "llm", Model: "m1", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("401 应报错")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("错误应带 `HTTP <code>:` 前缀，得 %q", err.Error())
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("错误正文应压成单行，得 %q", err.Error())
	}
}

func TestStreamConfiguredDirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(s string) {
			fmt.Fprint(w, s)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// reasoning 走独立通道；content 里带 <think> 标签，且**标签被切成两块**（严酷用例）
		send("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"先想\"}}]}\n\n")
		send("data: {\"choices\":[{\"delta\":{\"content\":\"<thi\"}}]}\n\n")
		send("data: {\"choices\":[{\"delta\":{\"content\":\"nk>秘密</think>你\"}}]}\n\n")
		send("data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n")
		send("data: [DONE]\n\n")
	}))
	defer srv.Close()
	writeModelsJSON(t, []map[string]any{{"id": "mini", "url": srv.URL + "/v1", "apiKey": "k"}})

	var kinds []string
	res, err := (&LLMEngine{BinPath: "/nonexistent/llm"}).Stream(context.Background(),
		Request{Engine: "llm", Model: "mini", Messages: []Message{{Role: "user", Content: "hi"}}},
		func(ev StreamEvent) { kinds = append(kinds, string(ev.Kind)+":"+ev.Text) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Response.Text != "你好" {
		t.Errorf("正文应为「你好」（标签跨块也要剥掉），得 %q", res.Response.Text)
	}
	if res.Response.Model != "mini" {
		t.Errorf("流式结果的 Model 也应是配置 id（唯一标识），得 %q", res.Response.Model)
	}
	if !strings.Contains(res.Thinking, "先想") || !strings.Contains(res.Thinking, "秘密") {
		t.Errorf("思考通道应同时收到 reasoning 通道与标签内的内容，得 %q", res.Thinking)
	}
	if res.Response.InputTokens != 5 || res.Response.OutputTokens != 2 {
		t.Errorf("流式 usage 应回填 5/2，得 %d/%d", res.Response.InputTokens, res.Response.OutputTokens)
	}
	joined := strings.Join(kinds, "|")
	if strings.Contains(joined, "<thi") || strings.Contains(joined, "think>") {
		t.Errorf("不该把标签碎片发给前端: %s", joined)
	}
}

func TestStreamConfiguredNoText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"只有思考\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	writeModelsJSON(t, []map[string]any{{"id": "mini", "url": srv.URL + "/v1"}})

	_, err := (&LLMEngine{BinPath: "/nonexistent/llm"}).Stream(context.Background(),
		Request{Engine: "llm", Model: "mini", Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "没有正文") {
		t.Errorf("只有思考没有正文时应报错，得 %v", err)
	}
}

func TestConfiguredModelRejectsSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	writeModelsJSON(t, []map[string]any{{"id": "mini", "url": srv.URL + "/v1"}})

	e := &LLMEngine{BinPath: "/nonexistent/llm"}
	if _, err := e.Complete(context.Background(), Request{Engine: "llm", Model: "mini",
		SessionID: "abc", Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Error("显式 --session 时应明确报错（直连无会话库），不能静默丢上下文")
	}
	if _, err := e.Complete(context.Background(), Request{Engine: "llm", Model: "mini",
		Continue: true, Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Error("显式 -c 时应明确报错")
	}
}

// 未知 id（不在配置文件里）必须仍走 llm CLI 路径 —— 用不存在的 BinPath 触发 CLI 探测失败，
// 错误里应出现 llm CLI 相关字样，而不是直连才有的报错。
func TestUnknownModelFallsBackToCLI(t *testing.T) {
	writeModelsJSON(t, []map[string]any{{"id": "mini", "url": "http://127.0.0.1:1/v1"}})
	_, err := (&LLMEngine{BinPath: "/nonexistent/llm"}).Complete(context.Background(),
		Request{Engine: "llm", Model: "not-in-config", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("BinPath 不存在时应失败")
	}
	if !strings.Contains(err.Error(), "llm CLI") {
		t.Errorf("未知 id 应回退 llm CLI 路径，得 %q", err.Error())
	}
}

func TestExpandEnvKeep(t *testing.T) {
	t.Setenv("LLM_TEST_KEY", "secret")
	cases := map[string]string{
		"${LLM_TEST_KEY}":         "secret",
		"$LLM_TEST_KEY":           "secret",
		"Bearer $LLM_TEST_KEY!":   "Bearer secret!",
		"${LLM_TEST_KEY_NOT_SET}": "${LLM_TEST_KEY_NOT_SET}", // 未设置 → 保留占位（暴露配置问题）
		"plain":                   "plain",
	}
	for in, want := range cases {
		if got := expandEnvKeep(in); got != want {
			t.Errorf("expandEnvKeep(%q) = %q, want %q", in, got, want)
		}
	}
}

// 默认超时：条目里给了 timeout（秒）就用它，调用方给了 Timeout 优先。
func TestConfiguredTimeout(t *testing.T) {
	if got := llmConfiguredTimeout(LLMConfigModel{Timeout: 30}, Request{}); got != 30*time.Second {
		t.Errorf("条目 timeout 应生效，得 %v", got)
	}
	if got := llmConfiguredTimeout(LLMConfigModel{Timeout: 30}, Request{Timeout: 5 * time.Second}); got != 5*time.Second {
		t.Errorf("调用方 Timeout 应优先，得 %v", got)
	}
	if got := llmConfiguredTimeout(LLMConfigModel{}, Request{}); got != DefaultLLMTimeout {
		t.Errorf("都没给时用引擎默认，得 %v", got)
	}
}
