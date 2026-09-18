package agent

// arkclaw_test.go - ArkClaw（A2A JSON-RPC 网关）引擎单元测试。
//
// 覆盖：注册表、非流式契约（不得实现 Streamer）、message/send 请求体形状、
// contextId 续接位置（必须在 message 内部）、401 纯文本鉴权失败、JSON-RPC
// error、任务失败态、artifacts 兜底取文、空正文、凭据缺失、Continue 显式
// 拒绝、JSONSchema 后处理、端点拼接与配置文件读取。
//
// 全部走 httptest，不触网。

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
)

// arkClawRec 记录假网关收到的一次请求。
type arkClawRec struct {
	Method string
	Path   string
	Query  map[string]string
	Header http.Header
	Body   string
}

// arkClawTestServer 起一个假 A2A 网关：先记录请求，再交给 handler 决定响应。
func arkClawTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body string)) (*httptest.Server, *arkClawRec) {
	t.Helper()
	rec := &arkClawRec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.Method = r.Method
		rec.Path = r.URL.Path
		rec.Query = map[string]string{}
		for k, vs := range r.URL.Query() {
			if len(vs) > 0 {
				rec.Query[k] = vs[0]
			}
		}
		rec.Header = r.Header.Clone()
		rec.Body = string(raw)
		handler(w, r, string(raw))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// arkClawTextEnvelope 组一个正常完成（completed）的 A2A 响应体。
func arkClawTextEnvelope(text, contextID string) string {
	return fmt.Sprintf(
		`{"jsonrpc":"2.0","id":"req-1","result":{"kind":"task","id":"task-1","contextId":%q,`+
			`"status":{"state":"completed","message":{"kind":"message","role":"agent",`+
			`"parts":[{"kind":"text","text":%q}]}},"history":[],"artifacts":[]}}`,
		contextID, text)
}

// arkClawServerEngine 返回一个指向假网关的引擎（凭据走显式字段，不读配置文件）。
func arkClawServerEngine(srv *httptest.Server) *ArkClawEngine {
	return &ArkClawEngine{
		URL:    srv.URL + "/a2a/jsonrpc",
		Key:    "test-key",
		ClawID: "ci-test",
	}
}

// ---------- 注册表与接口契约 ----------

func TestArkClawLookup(t *testing.T) {
	e := Lookup("arkclaw")
	if e == nil {
		t.Fatal(`Lookup("arkclaw") = nil`)
	}
	if e.Name() != "arkclaw" {
		t.Errorf("Name() = %q want arkclaw", e.Name())
	}
	// 大小写不敏感（与其余引擎一致）。
	if Lookup("ArkClaw") == nil {
		t.Error(`Lookup("ArkClaw") = nil, want case-insensitive match`)
	}
}

func TestArkClawIsEngine(t *testing.T) {
	var e Engine = &ArkClawEngine{}
	if e.Name() != "arkclaw" {
		t.Errorf("Name() = %q", e.Name())
	}
}

func TestArkClawNotStreamer(t *testing.T) {
	// 需求明确「不需要流式」：引擎不得实现 Streamer，
	// CLI 层因此走 "engine does not support streaming" 的报错路径。
	if s := AsStreamer(&ArkClawEngine{}); s != nil {
		t.Errorf("arkclaw 不应实现 Streamer, got %T", s)
	}
}

func TestArkClawDetectConfigured(t *testing.T) {
	e := &ArkClawEngine{URL: "https://h/a2a/jsonrpc", Key: "k", ClawID: "c"}
	ok, note := e.Detect()
	if !ok {
		t.Fatalf("Detect() ok=false, note=%q", note)
	}
	if note != "https://h/a2a/jsonrpc" {
		t.Errorf("Detect() note = %q want url", note)
	}
}

func TestArkClawDetectMissingCreds(t *testing.T) {
	clearArkClawEnv(t)
	e := &ArkClawEngine{ConfigPath: filepath.Join(t.TempDir(), "absent.json")}
	ok, note := e.Detect()
	if ok {
		t.Fatalf("Detect() ok=true, note=%q", note)
	}
	for _, want := range []string{"url", "key", "claw_id"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q 未提及缺字段 %q", note, want)
		}
	}
}

// ---------- 正常路径 ----------

func TestArkClawCompleteSuccess(t *testing.T) {
	srv, rec := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, arkClawTextEnvelope("你好，我是 arkclaw", "ctx-1"))
	})
	e := arkClawServerEngine(srv)

	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "你好"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "你好，我是 arkclaw" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.SessionID != "ctx-1" {
		t.Errorf("SessionID = %q want ctx-1", resp.SessionID)
	}
	if resp.Engine != "arkclaw" {
		t.Errorf("Engine = %q want arkclaw", resp.Engine)
	}
	if resp.Latency <= 0 {
		t.Errorf("Latency = %v want > 0", resp.Latency)
	}

	// 凭据走 query，路径保持 /a2a/jsonrpc。
	if rec.Path != "/a2a/jsonrpc" {
		t.Errorf("path = %q", rec.Path)
	}
	if rec.Query["apikey"] != "test-key" || rec.Query["clawId"] != "ci-test" {
		t.Errorf("query = %v", rec.Query)
	}
	if ct := rec.Header.Get("content-type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}

	// 请求体必须是 message/send + 单条 text part。
	var got arkClawRequest
	if err := json.Unmarshal([]byte(rec.Body), &got); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v (%s)", err, rec.Body)
	}
	if got.JSONRPC != "2.0" || got.Method != "message/send" {
		t.Errorf("envelope = %+v", got)
	}
	if got.ID == "" || got.Params.Message.MessageID == "" {
		t.Errorf("id / messageId 不应为空: %+v", got)
	}
	if got.Params.Message.Kind != "message" || got.Params.Message.Role != "user" {
		t.Errorf("message = %+v", got.Params.Message)
	}
	if got.Params.Message.ContextID != "" {
		t.Errorf("新会话不应带 contextId, got %q", got.Params.Message.ContextID)
	}
	if len(got.Params.Message.Parts) != 1 || got.Params.Message.Parts[0].Kind != "text" {
		t.Fatalf("parts = %+v", got.Params.Message.Parts)
	}
	if !strings.Contains(got.Params.Message.Parts[0].Text, "你好") {
		t.Errorf("prompt 未含用户输入: %q", got.Params.Message.Parts[0].Text)
	}
}

func TestArkClawSystemPromptFlattened(t *testing.T) {
	srv, rec := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, arkClawTextEnvelope("ok", "ctx-1"))
	})
	e := arkClawServerEngine(srv)

	// 协议无独立 system 角色 → 应展平进 message 正文。
	if _, err := e.Complete(context.Background(), Request{
		SystemPrompt: "你是运维助手",
		Messages:     []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(rec.Body, "运维助手") {
		t.Errorf("system prompt 未展平进正文: %q", rec.Body)
	}
}

func TestArkClawResumeUsesMessageContextID(t *testing.T) {
	srv, rec := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, arkClawTextEnvelope("紫色大象", "ctx-1"))
	})
	e := arkClawServerEngine(srv)

	resp, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "我刚才说的暗号是什么"}},
		SessionID: "ctx-1",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.SessionID != "ctx-1" {
		t.Errorf("SessionID = %q want ctx-1", resp.SessionID)
	}

	// 关键回归点：续接 id 必须落在 message.contextId（消息对象内部）。
	// 放到外层 params.contextId 无效 —— 网关会另开上下文。
	var outer map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rec.Body), &outer); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(outer["params"], &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if raw, ok := params["contextId"]; ok {
		t.Errorf("外层 params.contextId 不应出现（会被网关忽略）: %s", raw)
	}
	var msg struct {
		ContextID string `json:"contextId"`
	}
	if err := json.Unmarshal(params["message"], &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if msg.ContextID != "ctx-1" {
		t.Errorf("message.contextId = %q want ctx-1", msg.ContextID)
	}
}

func TestArkClawSessionIDFallsBackToRequest(t *testing.T) {
	// 网关没回 contextId 时，沿用请求里的 id，保证调用方拿到的 session 可用。
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, arkClawTextEnvelope("ok", ""))
	})
	e := arkClawServerEngine(srv)

	resp, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		SessionID: "ctx-keep",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.SessionID != "ctx-keep" {
		t.Errorf("SessionID = %q want ctx-keep", resp.SessionID)
	}
}

// ---------- 错误路径 ----------

func TestArkClawHTTP401PlainText(t *testing.T) {
	// 鉴权失败时网关回 text/plain 纯文本（非 JSON）。
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		w.Header().Set("content-type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "External authentication failed.")
	})
	e := arkClawServerEngine(srv)

	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected 401 error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("错误应含状态码 401: %v", err)
	}
	if !strings.Contains(msg, "External authentication failed") {
		t.Errorf("错误应含网关正文: %v", err)
	}
}

func TestArkClawRPCError(t *testing.T) {
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"req-1","error":{"code":-32602,"message":"invalid params"}}`)
	})
	e := arkClawServerEngine(srv)

	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected JSON-RPC error")
	}
	if !strings.Contains(err.Error(), "invalid params") {
		t.Errorf("err = %v", err)
	}
}

func TestArkClawTaskFailed(t *testing.T) {
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"req-1","result":{"kind":"task","id":"t","contextId":"ctx-9",`+
			`"status":{"state":"failed","message":{"kind":"message","parts":[{"kind":"text","text":"上游超时"}]}}}}`)
	})
	e := arkClawServerEngine(srv)

	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected failure for status.state=failed")
	}
	if !strings.Contains(err.Error(), "failed") || !strings.Contains(err.Error(), "上游超时") {
		t.Errorf("err = %v", err)
	}
	// 失败也要把 contextId 交回调用方，便于排查/续接。
	if resp.SessionID != "ctx-9" {
		t.Errorf("SessionID = %q want ctx-9", resp.SessionID)
	}
}

func TestArkClawEmptyText(t *testing.T) {
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"req-1","result":{"kind":"task","id":"t","contextId":"c",`+
			`"status":{"state":"completed","message":{"parts":[]}}}}`)
	})
	e := arkClawServerEngine(srv)

	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "空正文") {
		t.Errorf("err = %v want 空正文", err)
	}
}

func TestArkClawNotConfigured(t *testing.T) {
	clearArkClawEnv(t)
	e := &ArkClawEngine{ConfigPath: filepath.Join(t.TempDir(), "absent.json")}

	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error when creds missing")
	}
	if !strings.Contains(err.Error(), "未配置") {
		t.Errorf("err = %v want 未配置", err)
	}
}

func TestArkClawContinueUnsupported(t *testing.T) {
	// A2A 无「查询最近上下文」接口 → Continue 必须显式报错，而不是静默新开会话。
	e := &ArkClawEngine{URL: "https://h/a2a/jsonrpc", Key: "k", ClawID: "c"}
	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Continue: true,
	})
	if err == nil {
		t.Fatal("expected error for Continue")
	}
	if !strings.Contains(err.Error(), "continue") && !strings.Contains(err.Error(), "续接") {
		t.Errorf("err = %v", err)
	}
}

func TestArkClawEmptyPrompt(t *testing.T) {
	e := &ArkClawEngine{URL: "https://h/a2a/jsonrpc", Key: "k", ClawID: "c"}
	_, err := e.Complete(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "empty prompt") {
		t.Errorf("err = %v want empty prompt", err)
	}
}

// ---------- 解析细节 ----------

func TestArkClawArtifactsFallback(t *testing.T) {
	// status.message 缺正文时退回 artifacts[].parts[].text。
	raw := `{"jsonrpc":"2.0","id":"1","result":{"kind":"task","id":"t","contextId":"c",` +
		`"status":{"state":"completed"},"artifacts":[{"artifactId":"a1","name":"out",` +
		`"parts":[{"kind":"text","text":"来自产物"}]}]}}`
	resp, err := arkClawParseResponse(http.StatusOK, []byte(raw))
	if err != nil {
		t.Fatalf("arkClawParseResponse: %v", err)
	}
	if resp.Text != "来自产物" {
		t.Errorf("Text = %q", resp.Text)
	}
}

func TestArkClawJoinParts(t *testing.T) {
	got := arkClawJoinParts([]arkClawPart{
		{Kind: "text", Text: "第一段"},
		{Kind: "text", Text: "   "}, // 空白块跳过
		{Kind: "text", Text: "第二段"},
	})
	if got != "第一段\n第二段" {
		t.Errorf("arkClawJoinParts = %q", got)
	}
}

func TestArkClawParseNonJSONBody(t *testing.T) {
	_, err := arkClawParseResponse(http.StatusOK, []byte("<html>bad gateway</html>"))
	if err == nil || !strings.Contains(err.Error(), "不是 JSON") {
		t.Errorf("err = %v", err)
	}
}

func TestArkClawParseHandlesLeadingLogLine(t *testing.T) {
	// 少量网关会在 JSON 前混入日志行 → 退回括号计数法抽首个对象。
	body := "[gw] request accepted\n" + arkClawTextEnvelope("带日志的正文", "ctx-7")
	resp, err := arkClawParseResponse(http.StatusOK, []byte(body))
	if err != nil {
		t.Fatalf("arkClawParseResponse: %v", err)
	}
	if resp.Text != "带日志的正文" || resp.SessionID != "ctx-7" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestArkClawEndpointURL(t *testing.T) {
	got, err := arkClawEndpointURL("https://h/a2a/jsonrpc", "k 1", "c/2")
	if err != nil {
		t.Fatalf("arkClawEndpointURL: %v", err)
	}
	// 凭据必须被 URL 编码。
	if !strings.Contains(got, "apikey=k+1") && !strings.Contains(got, "apikey=k%201") {
		t.Errorf("apikey 未正确编码: %q", got)
	}
	if !strings.Contains(got, "clawId=c%2F2") {
		t.Errorf("clawId 未正确编码: %q", got)
	}

	// 原 URL 自带的 query 参数应保留。
	got, err = arkClawEndpointURL("https://h/a2a/jsonrpc?trace=1", "k", "c")
	if err != nil {
		t.Fatalf("arkClawEndpointURL: %v", err)
	}
	if !strings.Contains(got, "trace=1") {
		t.Errorf("原有 query 丢失: %q", got)
	}
}

func TestArkClawEndpointURLRejectsRelative(t *testing.T) {
	for _, bad := range []string{"", "/a2a/jsonrpc", "h/a2a/jsonrpc", "://x"} {
		if _, err := arkClawEndpointURL(bad, "k", "c"); err == nil {
			t.Errorf("arkClawEndpointURL(%q) 应报错", bad)
		}
	}
}

func TestArkClawRandomIDUniqueAndShaped(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := arkClawRandomID()
		if seen[id] {
			t.Fatalf("重复 id: %q", id)
		}
		seen[id] = true
		if len(id) != 36 || strings.Count(id, "-") != 4 {
			t.Fatalf("id 不是 UUIDv4 形态: %q", id)
		}
	}
}

// ---------- 配置来源 ----------

func TestArkClawCredsFromConfigFile(t *testing.T) {
	clearArkClawEnv(t)
	srv, rec := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, arkClawTextEnvelope("ok", "ctx-1"))
	})

	// 配置文件里的 url 指向假网关；key/claw_id 也来自文件。
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	body := fmt.Sprintf(`{"arkclaw":{"url":%q,"key":"file-key","claw_id":"file-claw"}}`,
		srv.URL+"/a2a/jsonrpc")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	e := &ArkClawEngine{ConfigPath: cfgPath}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if rec.Query["apikey"] != "file-key" || rec.Query["clawId"] != "file-claw" {
		t.Errorf("文件凭据未生效: %v", rec.Query)
	}
}

func TestArkClawExplicitOverridesEnvAndFile(t *testing.T) {
	// 优先级：显式字段 > 环境变量 > 配置文件。
	t.Setenv("MAGIC_AGENT_ARKCLAW_URL", "https://env.invalid/a2a/jsonrpc")
	t.Setenv("MAGIC_AGENT_ARKCLAW_KEY", "env-key")
	t.Setenv("MAGIC_AGENT_ARKCLAW_CLAW_ID", "env-claw")

	srv, rec := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, arkClawTextEnvelope("ok", "ctx-1"))
	})

	e := &ArkClawEngine{
		URL:    srv.URL + "/a2a/jsonrpc", // 显式覆盖环境变量
		Key:    "explicit-key",
		ClawID: "explicit-claw",
	}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if rec.Query["apikey"] != "explicit-key" || rec.Query["clawId"] != "explicit-claw" {
		t.Errorf("显式字段未覆盖环境变量: %v", rec.Query)
	}
}

func TestArkClawMalformedConfigReported(t *testing.T) {
	clearArkClawEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &ArkClawEngine{ConfigPath: cfgPath}
	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected parse error to surface")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("err = %v", err)
	}
}

// ---------- JSONSchema 后处理 ----------

func TestArkClawJSONSchemaPostProcess(t *testing.T) {
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		// 网关爱说废话：正文里包了 markdown 围栏 + 前后解释。
		_, _ = io.WriteString(w, arkClawTextEnvelope(
			"好的，结果如下：\n```json\n{\"city\":\"北京\",\"temp\":21}\n```\n需要我继续吗？", "ctx-1"))
	})
	e := arkClawServerEngine(srv)

	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "北京天气"}},
		JSONSchema: &JSONSchema{
			Type: "object",
			Properties: map[string]map[string]any{
				"city": {"type": "string"},
				"temp": {"type": "number"},
			},
			Required: []string{"city", "temp"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resp.Text), &out); err != nil {
		t.Fatalf("resp.Text 不是纯 JSON: %v (%q)", err, resp.Text)
	}
	if out["city"] != "北京" {
		t.Errorf("out = %v", out)
	}
}

func TestArkClawTimeoutFromRequest(t *testing.T) {
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, arkClawTextEnvelope("ok", "c"))
	})
	e := arkClawServerEngine(srv)

	// req.Timeout 应生效（此处给足，验证不因超时配置而失败）。
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Timeout:  30_000_000_000, // 30s
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// TestArkClawModelNotForwarded 验证 -m 在 arkclaw 上不能透传：
// 实测网关按 clawId 绑死模型，magic-agent 收到 -m 也要明确告警。
// 同时确认告警「不影响调用」——网关响应照样被解析、调用照样成功。
func TestArkClawModelNotForwarded(t *testing.T) {
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, arkClawTextEnvelope("ok", "c"))
	})
	e := arkClawServerEngine(srv)

	// 临时把 stderr 接到 buffer，验证告警文本。
	old := stderr
	defer func() { stderr = old }()
	var buf strings.Builder
	stderr = &buf

	resp, err := e.Complete(context.Background(), Request{
		Model:    "minimax-m3",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q", resp.Text)
	}
	warn := buf.String()
	if !strings.Contains(warn, "arkclaw") || !strings.Contains(warn, "minimax-m3") {
		t.Errorf("告警文案未提及 arkclaw/minimax-m3: %q", warn)
	}
	if !strings.Contains(warn, "claw_id") {
		t.Errorf("告警应引导用户改 claw_id: %q", warn)
	}
}

// TestArkClawNoModelNoWarning 验证未传 -m 时不要无脑打告警。
func TestArkClawNoModelNoWarning(t *testing.T) {
	srv, _ := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, arkClawTextEnvelope("ok", "c"))
	})
	e := arkClawServerEngine(srv)

	old := stderr
	defer func() { stderr = old }()
	var buf strings.Builder
	stderr = &buf

	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("未传 -m 时不应有 stderr 告警, got %q", buf.String())
	}
}

func TestDefaultArkClawTimeoutCoversObservedLatency(t *testing.T) {
	// 实测单轮 8~23s；默认超时必须留出充分余量。
	if DefaultArkClawTimeout < 2*60*1e9 {
		t.Errorf("DefaultArkClawTimeout = %v, 实测延迟最大 ~23s，应留足余量", DefaultArkClawTimeout)
	}
}

// clearArkClawEnv 清掉可能干扰测试的 arkclaw 相关环境变量。
func clearArkClawEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MAGIC_AGENT_CONFIG",
		"MAGIC_AGENT_ARKCLAW_URL",
		"MAGIC_AGENT_ARKCLAW_KEY",
		"MAGIC_AGENT_ARKCLAW_CLAW_ID",
	} {
		t.Setenv(k, "")
	}
}
