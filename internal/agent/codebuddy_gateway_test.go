package agent

// codebuddy_gateway_test.go - codebuddy-gateway（CodeBuddy Code HTTP 网关，webhook + SSE）
// 引擎单元测试。
//
// 覆盖：注册表与接口契约、webhook 投递请求形状（路径 / 认证头 / 安全头 / body 字段）、
// SSE 逐帧解析（accepted / streaming / completed / error）、增量去重、终帧权威正文、
// JSONSchema 后处理、会话锚点续接、附件映射、非 SSE 回退、投递失败、能力表接线、
// 配置文件读取。
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
	"time"

	"github.com/darren/magic-agent/internal/config"
)

// cbGatewayRec 记录假网关收到的一次请求。
type cbGatewayRec struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

// cbGatewayTestServer 起一个假网关：POST 投递按 runID 回 202，GET 流按帧脚本回 SSE。
//
// frames 里每个元素是一行 data 的载荷（不含 "data: " 前缀）。
func cbGatewayTestServer(t *testing.T, runID string, frames []string) (*httptest.Server, *[]cbGatewayRec) {
	t.Helper()
	recs := &[]cbGatewayRec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*recs = append(*recs, cbGatewayRec{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: string(raw)})

		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/webhooks/"):
			w.Header().Set("content-type", "application/json;charset=UTF-8")
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, `{"data":{"runId":%q,"status":"accepted"}}`, runID)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/"+runID+"/stream":
			w.Header().Set("content-type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			for _, f := range frames {
				fmt.Fprintf(w, "data: %s\n\n", f)
				if fl != nil {
					fl.Flush()
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, recs
}

// cbGatewayFrameJSON 组一帧出站消息 JSON。
func cbGatewayFrameJSON(status, extra string) string {
	base := fmt.Sprintf(`{"version":"1.0","replyTo":"msg","status":%q`, status)
	if extra != "" {
		base += "," + extra
	}
	return base + "}"
}

// cbGatewayServerEngine 返回一个指向假网关的引擎（全部字段显式给，不读用户配置文件）。
func cbGatewayServerEngine(srv *httptest.Server) *CodeBuddyGatewayEngine {
	return &CodeBuddyGatewayEngine{
		URL:      srv.URL,
		Password: "test-token",
		Platform: "generic",
		Sender:   "tester",
		// ConfigPath 指向一个不存在的文件：保证不会读到开发机上的真实配置。
		ConfigPath: filepath.Join(os.TempDir(), "magic-agent-cbgw-absent.json"),
	}
}

// ---------- 注册表与接口契约 ----------

func TestCodeBuddyGatewayLookup(t *testing.T) {
	e := Lookup("codebuddy-gateway")
	if e == nil {
		t.Fatal(`Lookup("codebuddy-gateway") = nil`)
	}
	if e.Name() != "codebuddy-gateway" {
		t.Errorf("Name() = %q want codebuddy-gateway", e.Name())
	}
	if Lookup("CodeBuddy-Gateway") == nil {
		t.Error("Lookup 应大小写不敏感")
	}
	// 与 codebuddy / codebuddy-ai 是两个引擎，不能互相遮住。
	if Lookup("codebuddy").Name() != "codebuddy" {
		t.Error(`Lookup("codebuddy") 被 codebuddy-gateway 遮住了`)
	}
}

func TestCodeBuddyGatewayImplementsInterfaces(t *testing.T) {
	var e Engine = &CodeBuddyGatewayEngine{}
	if e.Name() != cbGatewayEngineName {
		t.Errorf("Name() = %q", e.Name())
	}
	if AsStreamer(e) == nil {
		t.Error("codebuddy-gateway 应实现 Streamer")
	}
	if !SupportsStream(e) {
		t.Error("SupportsStream(codebuddy-gateway) 应为 true")
	}
	if e.(CapabilityFamily).CapabilityFamily() != "codebuddy-gateway" {
		t.Errorf("CapabilityFamily() = %q", e.(CapabilityFamily).CapabilityFamily())
	}
}

// ---------- 非流式（Complete）：投递 + 收流 ----------

func TestCodeBuddyGatewayComplete(t *testing.T) {
	frames := []string{
		cbGatewayFrameJSON("accepted", ""),
		cbGatewayFrameJSON("streaming", `"content":{"chunk":"你好"}`),
		cbGatewayFrameJSON("streaming", `"content":{"chunk":"，世界"}`),
		cbGatewayFrameJSON("completed", `"content":{"text":"你好，世界","markdown":"你好，世界"},"agent":{"sessionId":"sess-1"}`),
	}
	srv, recs := cbGatewayTestServer(t, "run-abc", frames)
	e := cbGatewayServerEngine(srv)

	res, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "打个招呼"}},
	})
	if err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	if res.Text != "你好，世界" {
		t.Errorf("Text = %q want 你好，世界", res.Text)
	}
	if res.Engine != "codebuddy-gateway" {
		t.Errorf("Engine = %q", res.Engine)
	}
	// ⚠️ SessionID 必须是**会话锚点**（conversation id），不是终帧里的 agent.sessionId ——
	// 后者是网关内部 UUID，拿它当 --session 会静默新开会话（见 result 的注释）。
	if res.SessionID == "sess-1" {
		t.Error("SessionID 取了终帧的 agent.sessionId（内部 UUID），应为会话锚点")
	}
	if res.SessionID == "" || !strings.HasPrefix(res.SessionID, "magic-agent-") {
		t.Errorf("SessionID = %q want 自动生成的会话锚点", res.SessionID)
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d want 1", res.Attempts)
	}

	if len(*recs) != 2 {
		t.Fatalf("假网关收到 %d 次请求 want 2（投递 + 收流）", len(*recs))
	}
	post, get := (*recs)[0], (*recs)[1]

	// 投递：路径 / 方法 / 两个头 / body 形状。
	if post.Method != http.MethodPost {
		t.Errorf("投递方法 = %s", post.Method)
	}
	if post.Path != "/api/v1/webhooks/generic" {
		t.Errorf("投递路径 = %s want /api/v1/webhooks/generic", post.Path)
	}
	if got := post.Header.Get("authorization"); got != "Bearer test-token" {
		t.Errorf("authorization = %q want Bearer test-token", got)
	}
	if got := post.Header.Get(cbGatewaySecurityHeader); got != "1" {
		t.Errorf("%s = %q want 1（网关请求校验中间件要求）", cbGatewaySecurityHeader, got)
	}
	var inbound cbGatewayInbound
	if err := json.Unmarshal([]byte(post.Body), &inbound); err != nil {
		t.Fatalf("投递 body 不是合法 JSON: %v", err)
	}
	if inbound.Type != "message" || inbound.Version != "1.0" {
		t.Errorf("version/type = %q/%q want 1.0/message", inbound.Version, inbound.Type)
	}
	if inbound.Source.Platform != "generic" || inbound.Source.Sender.ID != "tester" {
		t.Errorf("source = %+v", inbound.Source)
	}
	if inbound.Source.Conversation.ID == "" {
		t.Error("conversation.id 不能为空（它是网关的会话锚点）")
	}
	if !strings.Contains(inbound.Payload.Text, "打个招呼") {
		t.Errorf("payload.text = %q，应含展平后的用户消息", inbound.Payload.Text)
	}

	// 收流：路径 + accept。
	if get.Path != "/api/v1/runs/run-abc/stream" {
		t.Errorf("收流路径 = %s", get.Path)
	}
	if got := get.Header.Get("accept"); got != "text/event-stream" {
		t.Errorf("accept = %q want text/event-stream", got)
	}
}

func TestCodeBuddyGatewayStreamEmitsIncrements(t *testing.T) {
	frames := []string{
		cbGatewayFrameJSON("streaming", `"content":{"chunk":"第一"}`),
		cbGatewayFrameJSON("streaming", `"content":{"chunk":"第二"}`),
		cbGatewayFrameJSON("completed", `"content":{"markdown":"第一第二"}`),
	}
	srv, _ := cbGatewayTestServer(t, "run-1", frames)
	e := cbGatewayServerEngine(srv)

	var got []string
	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "x"}},
	}, func(ev StreamEvent) {
		if ev.Kind == KindText {
			got = append(got, ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("Stream 失败: %v", err)
	}
	// 增量必须是**逐块**（不是整段一次性），终帧只补差量（此处无差量）。
	want := []string{"第一", "第二"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("增量 = %v want %v", got, want)
	}
	if res.Text != "第一第二" {
		t.Errorf("Text = %q", res.Text)
	}
}

// 终帧正文比已发增量长时，只补差量（不重复发整段）。
func TestCodeBuddyGatewayCompletedEmitsRemainderOnly(t *testing.T) {
	frames := []string{
		cbGatewayFrameJSON("streaming", `"content":{"chunk":"你好"}`),
		cbGatewayFrameJSON("completed", `"content":{"markdown":"你好，世界"}`),
	}
	srv, _ := cbGatewayTestServer(t, "run-1", frames)
	e := cbGatewayServerEngine(srv)

	var got []string
	if _, err := e.Stream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}},
		func(ev StreamEvent) { got = append(got, ev.Text) }); err != nil {
		t.Fatalf("Stream 失败: %v", err)
	}
	if len(got) != 2 || got[0] != "你好" || got[1] != "，世界" {
		t.Errorf("增量 = %v want [你好 ，世界]", got)
	}
}

func TestCodeBuddyGatewayErrorFrame(t *testing.T) {
	frames := []string{
		cbGatewayFrameJSON("streaming", `"content":{"chunk":"半句"}`),
		cbGatewayFrameJSON("error", `"error":{"code":"EXECUTION_ERROR","message":"模型调用失败"}`),
	}
	srv, _ := cbGatewayTestServer(t, "run-1", frames)
	e := cbGatewayServerEngine(srv)

	_, err := e.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("error 帧应报错")
	}
	if !strings.Contains(err.Error(), "EXECUTION_ERROR") || !strings.Contains(err.Error(), "模型调用失败") {
		t.Errorf("错误信息 = %q，应带上 code 与 message", err.Error())
	}
}

func TestCodeBuddyGatewayEmptyBody(t *testing.T) {
	frames := []string{cbGatewayFrameJSON("completed", `"content":{"markdown":"   "}`)}
	srv, _ := cbGatewayTestServer(t, "run-1", frames)
	e := cbGatewayServerEngine(srv)

	_, err := e.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "空正文") {
		t.Errorf("err = %v want 空正文", err)
	}
}

// 流结束但没见到终帧 → 如实报错，不把半句当结果。
func TestCodeBuddyGatewayMissingTerminalFrame(t *testing.T) {
	frames := []string{cbGatewayFrameJSON("streaming", `"content":{"chunk":"半句"}`)}
	srv, _ := cbGatewayTestServer(t, "run-1", frames)
	e := cbGatewayServerEngine(srv)

	_, err := e.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "没有终帧") {
		t.Errorf("err = %v want 没有终帧", err)
	}
}

// 网关没按 SSE 回（老版本 / 路由被改）→ 如实报错，不假装有增量。
func TestCodeBuddyGatewayNonSSEFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"data":{"runId":"run-1","status":"accepted"}}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"data":{"runId":"run-1","active":false}}`)
	}))
	t.Cleanup(srv.Close)
	e := cbGatewayServerEngine(srv)

	_, err := e.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "未按 SSE 回") {
		t.Errorf("err = %v want 未按 SSE 回", err)
	}
}

// 投递被拒（401 纯文本）→ 报错里带上状态码与正文，不能只说 "invalid character"。
func TestCodeBuddyGatewayDeliverRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "Authentication required")
	}))
	t.Cleanup(srv.Close)
	e := cbGatewayServerEngine(srv)

	_, err := e.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("401 应报错")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Authentication required") {
		t.Errorf("err = %q，应带状态码与正文", err.Error())
	}
}

// ---------- 会话续接 / 附件 / 后处理 ----------

func TestCodeBuddyGatewaySessionContinuation(t *testing.T) {
	frames := []string{cbGatewayFrameJSON("completed", `"content":{"markdown":"ok"}`)}
	srv, recs := cbGatewayTestServer(t, "run-1", frames)
	e := cbGatewayServerEngine(srv)

	res, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "接着说"}},
		SessionID: "conv-42",
	})
	if err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	var inbound cbGatewayInbound
	if err := json.Unmarshal([]byte((*recs)[0].Body), &inbound); err != nil {
		t.Fatalf("body: %v", err)
	}
	if inbound.Source.Conversation.ID != "conv-42" {
		t.Errorf("conversation.id = %q want conv-42（--session 应落到会话锚点）", inbound.Source.Conversation.ID)
	}
	if res.SessionID != "conv-42" {
		t.Errorf("SessionID = %q want conv-42（终帧没给 sessionId 时沿用请求里的）", res.SessionID)
	}
}

// 终帧带 agent.sessionId 时，也不能拿它当 session_id 返回（那是网关内部 UUID）。
func TestCodeBuddyGatewaySessionIDIsConversationAnchor(t *testing.T) {
	frames := []string{
		cbGatewayFrameJSON("completed", `"content":{"markdown":"ok"},"agent":{"sessionId":"6a58b80e-2750-432f-9cea-a37424ea9533"}`),
	}
	srv, _ := cbGatewayTestServer(t, "run-1", frames)
	e := cbGatewayServerEngine(srv)

	res, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "接着说"}},
		SessionID: "conv-42",
	})
	if err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	if res.SessionID != "conv-42" {
		t.Errorf("SessionID = %q want conv-42（会话锚点，不是终帧的 agent.sessionId）", res.SessionID)
	}
}

// 每次调用都不给 --session 时，会话锚点必须各不相同（否则会串上下文）。
func TestCodeBuddyGatewayFreshConversationPerCall(t *testing.T) {
	frames := []string{cbGatewayFrameJSON("completed", `"content":{"markdown":"ok"}`)}
	srv, recs := cbGatewayTestServer(t, "run-1", frames)
	e := cbGatewayServerEngine(srv)

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		if _, err := e.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
			t.Fatalf("第 %d 次失败: %v", i, err)
		}
	}
	for _, r := range *recs {
		if r.Method != http.MethodPost {
			continue
		}
		var inbound cbGatewayInbound
		if err := json.Unmarshal([]byte(r.Body), &inbound); err != nil {
			t.Fatalf("body: %v", err)
		}
		if seen[inbound.Source.Conversation.ID] {
			t.Errorf("conversation.id %q 重复了（会串上下文）", inbound.Source.Conversation.ID)
		}
		seen[inbound.Source.Conversation.ID] = true
	}
	if len(seen) != 3 {
		t.Errorf("收到 %d 个不同的会话锚点 want 3", len(seen))
	}
}

func TestCodeBuddyGatewayContinueRejected(t *testing.T) {
	srv, _ := cbGatewayTestServer(t, "run-1", nil)
	e := cbGatewayServerEngine(srv)

	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "x"}},
		Continue: true,
	})
	if err == nil || !strings.Contains(err.Error(), "continue") {
		t.Errorf("err = %v want 明确拒绝 continue", err)
	}
}

func TestCodeBuddyGatewayAttachmentsMapped(t *testing.T) {
	frames := []string{cbGatewayFrameJSON("completed", `"content":{"markdown":"ok"}`)}
	srv, recs := cbGatewayTestServer(t, "run-1", frames)
	e := cbGatewayServerEngine(srv)

	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "看这张图"}},
		Attachments: []Attachment{
			{Path: "/tmp/a.png", MIME: "image/png"},
			{Path: "/tmp/b.pdf", MIME: "application/pdf"},
		},
	})
	if err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	var inbound cbGatewayInbound
	if err := json.Unmarshal([]byte((*recs)[0].Body), &inbound); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(inbound.Payload.Attachments) != 2 {
		t.Fatalf("attachments = %d want 2", len(inbound.Payload.Attachments))
	}
	a := inbound.Payload.Attachments[0]
	if a.Type != "image" || a.URL != "/tmp/a.png" || a.URLType != "local-path" {
		t.Errorf("附件[0] = %+v", a)
	}
	if inbound.Payload.Attachments[1].Type != "file" {
		t.Errorf("附件[1].Type = %q want file", inbound.Payload.Attachments[1].Type)
	}
}

func TestCodeBuddyGatewayJSONSchemaPostProcess(t *testing.T) {
	// 模型把 JSON 裹在 markdown 围栏里（最常见形态），后处理要抽得出来。
	markdown := "```json\n{\"name\":\"n\",\"age\":1}\n```"
	quoted, err := json.Marshal(markdown)
	if err != nil {
		t.Fatal(err)
	}
	frames := []string{
		fmt.Sprintf(`{"version":"1.0","replyTo":"msg","status":"completed","content":{"markdown":%s}}`, quoted),
	}
	srv, _ := cbGatewayTestServer(t, "run-1", frames)
	e := cbGatewayServerEngine(srv)

	schema := &JSONSchema{Type: "object", Required: []string{"name", "age"}}
	res, err := e.Complete(context.Background(), Request{
		Messages:   []Message{{Role: "user", Content: "x"}},
		JSONSchema: schema,
	})
	if err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	if !strings.HasPrefix(res.Text, "{") {
		t.Errorf("Text = %q，JSONSchema 模式下应抽出纯 JSON", res.Text)
	}
}

// ---------- Detect / 配置 ----------

func TestCodeBuddyGatewayDetect(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.json")

	// 未配置 url → 不可用，提示里带上配置文件位置与环境变量名。
	e := &CodeBuddyGatewayEngine{ConfigPath: absent}
	ok, note := e.Detect()
	if ok {
		t.Error("未配置 url 时 Detect 应为 false")
	}
	if !strings.Contains(note, "codebuddyGateway") || !strings.Contains(note, config.EnvCBGWURL) {
		t.Errorf("note = %q，应点明配置节与环境变量", note)
	}

	// 配了 url → 可用，说明信息是 URL（与 CLI 引擎返回二进制路径同构）。
	e2 := &CodeBuddyGatewayEngine{URL: "http://127.0.0.1:8399", ConfigPath: absent}
	ok2, note2 := e2.Detect()
	if !ok2 {
		t.Fatalf("配了 url 时 Detect 应为 true，note=%q", note2)
	}
	if note2 != "http://127.0.0.1:8399" {
		t.Errorf("note = %q want 端点 URL", note2)
	}
}

func TestCodeBuddyGatewayConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{"codebuddyGateway":{"endpoint":"http://127.0.0.1:9000/","token":"tok","platform":"generic","sender":"s1","conversation":"c1"}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(p)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	g := cfg.CodeBuddyGateway
	// endpoint → url、token → password 的宽松别名要生效。
	if g.URL != "http://127.0.0.1:9000/" || g.Password != "tok" {
		t.Errorf("url/password = %q/%q（别名未生效）", g.URL, g.Password)
	}
	if g.Platform != "generic" || g.Sender != "s1" || g.Conversation != "c1" {
		t.Errorf("其余字段 = %+v", g)
	}
	if !g.Ready() {
		t.Errorf("Ready() = false，Missing=%v", g.Missing())
	}

	// 节名别名（codebuddy_gateway）也要认。
	cfg2, err := config.LoadFrom(writeCBGWConfig(t, `{"codebuddy_gateway":{"url":"http://h:1"}}`))
	if err != nil {
		t.Fatalf("LoadFrom(alias): %v", err)
	}
	if cfg2.CodeBuddyGateway.URL != "http://h:1" {
		t.Errorf("节名别名未生效: %+v", cfg2.CodeBuddyGateway)
	}
}

// writeCBGWConfig 写一份临时配置文件并返回路径。
func writeCBGWConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// 引擎从配置文件读连接参数（显式字段留空时）。
func TestCodeBuddyGatewayEngineReadsConfig(t *testing.T) {
	frames := []string{cbGatewayFrameJSON("completed", `"content":{"markdown":"ok"}`)}
	srv, recs := cbGatewayTestServer(t, "run-1", frames)
	p := writeCBGWConfig(t, fmt.Sprintf(`{"codebuddyGateway":{"url":%q,"password":"cfg-tok"}}`, srv.URL))

	e := &CodeBuddyGatewayEngine{ConfigPath: p}
	if _, err := e.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	if got := (*recs)[0].Header.Get("authorization"); got != "Bearer cfg-tok" {
		t.Errorf("authorization = %q want Bearer cfg-tok（口令应来自配置文件）", got)
	}
}

// ---------- 能力表接线 ----------

func TestCodeBuddyGatewayCapabilities(t *testing.T) {
	const name = "codebuddy-gateway"

	// workspace：网关在它自己的 cwd 里跑 → per-call 改不动。
	if got := WorkspaceSupportOf(name); got != "none" {
		t.Errorf("WorkspaceSupportOf = %q want none", got)
	}
	// attachments：走原生字段但网关把它渲染成路径清单 → 如实报 prompt。
	if got := AttachmentSupportOf(name); got != "prompt" {
		t.Errorf("AttachmentSupportOf = %q want prompt", got)
	}
	// permission：档位在网关进程启动参数上，且网关强制 bypass → 未接线。
	if got := PermissionSupportOf(name); got != "none" {
		t.Errorf("PermissionSupportOf = %q want none", got)
	}
	if PermissionSupported(name) {
		t.Error("PermissionSupported 应为 false（传 --permission 要在 CLI 层报错）")
	}
	// ask：协议里没有作答通道。
	if got := AskSupportOf(name); got != "none" {
		t.Errorf("AskSupportOf = %q want none", got)
	}
	// append：webhook 是「一次投递一个 run」，没有持续喂消息的通道。
	if AppendSupportOf(name) {
		t.Error("AppendSupportOf 应为 false")
	}
	if AppendDefaultOn(name) {
		t.Error("AppendDefaultOn 应为 false")
	}
	// tools：工具集由网关进程的 --agent 决定，命令行层无逐工具开关。
	if ToolsSwitchableOf(name) {
		t.Error("ToolsSwitchableOf 应为 false")
	}
	// install：要的是「把网关跑起来」，不是安装命令 → 如实留空。
	if got := InstallCommandOf(name); got != "" {
		t.Errorf("InstallCommandOf = %q want 空串", got)
	}
}

// ---------- SSE 行解析 ----------

func TestCodeBuddyGatewaySSEPayload(t *testing.T) {
	cases := []struct {
		line string
		want string
		ok   bool
	}{
		{`data: {"status":"completed"}`, `{"status":"completed"}`, true},
		{`data:{"a":1}`, `{"a":1}`, true},
		{``, "", false},
		{`: keep-alive`, "", false},
		{`event: message`, "", false},
		{`id: 3`, "", false},
		{`data: [DONE]`, "", false},
	}
	for _, c := range cases {
		got, ok := cbGatewaySSEPayload(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("cbGatewaySSEPayload(%q) = (%q,%v) want (%q,%v)", c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestCodeBuddyGatewayIsEventStream(t *testing.T) {
	for _, ct := range []string{"text/event-stream", "text/event-stream; charset=utf-8", "TEXT/EVENT-STREAM"} {
		if !cbGatewayIsEventStream(ct) {
			t.Errorf("cbGatewayIsEventStream(%q) = false want true", ct)
		}
	}
	for _, ct := range []string{"application/json", "", "text/plain"} {
		if cbGatewayIsEventStream(ct) {
			t.Errorf("cbGatewayIsEventStream(%q) = true want false", ct)
		}
	}
}

// 超时/取消要在读流时如实报出来（不能静默返回半句）。
func TestCodeBuddyGatewayStreamTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"data":{"runId":"run-1","status":"accepted"}}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", cbGatewayFrameJSON("streaming", `"content":{"chunk":"半句"}`))
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(3 * time.Second) // 迟迟不给终帧
	}))
	t.Cleanup(srv.Close)
	e := cbGatewayServerEngine(srv)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := e.Complete(ctx, Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("超时应报错")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("超时没生效，耗时 %v", time.Since(start))
	}
}
