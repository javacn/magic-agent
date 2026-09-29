package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandleSessionMessagesReadAndValidate 验「看历史」路由的边界：
//
//   - 正常：写出 jsonl 后能读回（保证 plugin 这条线通）
//   - 不存在的会话 → 200 + 空 events，不是 404（空会话是合法状态）
//   - 缺令牌 → 401
//   - POST → 405
//   - 路径注入 `../../etc/passwd` → 400（拒绝 `.` 与 `..` 整段 id）
//   - 空 id（HTTP 客户端归一化后落到 "/desk/session/messages"）→ 200 + events=null：
//     此时 "messages" 成了 id —— 路由不挡。像普通会话一样查，不存在就 200+空 events。
//     这是合理的：路由没定义「纯 /messages」这条，前缀归一化后当成 id 处理不越权。
func TestHandleSessionMessagesReadAndValidate(t *testing.T) {
	// DefaultLogDir 用 HOME 解析成 `~/.magic-agent/sessions`。
	// 临时把 HOME 指到 t.TempDir()，fixture 写到 `<temp>/.magic-agent/sessions/<id>.jsonl`，
	// 不然 UserHomeDir 解析出来的目录不是 fixture 所在的地方。
	home := t.TempDir()
	dir := filepath.Join(home, ".magic-agent", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	id := "sess-good"
	path := filepath.Join(dir, id+".jsonl")
	want := []string{
		`{"seq":1,"kind":"thinking","text":"思考","session_id":"` + id + `","at":"2026-09-27T20:00:00Z"}`,
		`{"seq":2,"kind":"text","text":"正文","session_id":"` + id + `","at":"2026-09-27T20:00:01Z"}`,
		`{"seq":3,"kind":"turn_end","text":"完成","session_id":"` + id + `","at":"2026-09-27T20:00:02Z"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(want, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := newTestService(t, fakeCore(t, goodContract))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	// --- 正常：3 条 events，按 seq 升序
	resp := doGET(t, srv.URL+"/desk/session/"+id+"/messages?token=tok", true)
	if resp.statusCode != http.StatusOK {
		t.Fatalf("正常请求期望 200，实得 %d", resp.statusCode)
	}
	var body struct {
		Session string           `json:"session"`
		Events  []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(resp.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Session != id {
		t.Fatalf("session=%q 实得 %q", id, body.Session)
	}
	if len(body.Events) != 3 {
		t.Fatalf("期望 3 条 events，实得 %d：%v", len(body.Events), body.Events)
	}
	for i, e := range body.Events {
		// want 行是 jsonl 字串原样；Events 经 json.Unmarshal 后是 map —— 这里只验 seq 与 kind。
		var w map[string]any
		_ = json.Unmarshal([]byte(want[i]), &w)
		if e["seq"] != w["seq"] {
			t.Fatalf("event %d seq=%v 期望 %v", i, e["seq"], w["seq"])
		}
		if e["kind"] != w["kind"] {
			t.Fatalf("event %d kind=%v 期望 %v", i, e["kind"], w["kind"])
		}
	}

	// --- 不存在：200 + events=null
	resp = doGET(t, srv.URL+"/desk/session/no-such/messages?token=tok", true)
	if resp.statusCode != http.StatusOK {
		t.Fatalf("不存在期望 200，实得 %d", resp.statusCode)
	}
	var body2 struct {
		Events []map[string]any `json:"events"`
	}
	_ = json.Unmarshal(resp.body, &body2)
	if body2.Events != nil {
		t.Fatalf("不存在期望 events=null，实得 %v", body2.Events)
	}

	// --- 缺令牌 → 401
	resp = doGET(t, srv.URL+"/desk/session/"+id+"/messages", false)
	if resp.statusCode != http.StatusUnauthorized {
		t.Fatalf("缺令牌期望 401，实得 %d", resp.statusCode)
	}

	// --- POST → 405
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/desk/session/"+id+"/messages?token=tok", nil)
	req.Header.Set("Origin", "https://localhost")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST 期望 405，实得 %d", res.StatusCode)
	}

	// --- 路径注入 `../../etc/passwd` → 400（拒绝 `.` 与 `..` 整段 id）
	resp = doGET(t, srv.URL+"/desk/session/..%2F..%2Fetc%2Fpasswd/messages?token=tok", true)
	if resp.statusCode != http.StatusBadRequest {
		t.Fatalf("路径注入期望 400，实得 %d", resp.statusCode)
	}

	// --- 空 id（仅 `/messages`） → 实际变 /desk/session/messages（HTTP 客户端会归一化 `//`），
	//     此时 id 落到 "messages" —— 像普通会话一样查（不存在 → 200+空 events）。
	//     这是合理的：路由没有定义"纯 /messages"这条，前缀归一化后当成 id 处理不会越权。
	resp = doGET(t, srv.URL+"/desk/session//messages?token=tok", true)
	if resp.statusCode != http.StatusOK {
		t.Fatalf("前缀归一化后空 id 期望 200，实得 %d", resp.statusCode)
	}
}

// doGET 简单 GET 一把：带 origin、断言状态码、读 body 全文。
func doGET(t *testing.T, rawURL string, withOrigin bool) (resp struct {
	statusCode int
	body       []byte
}) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	if withOrigin {
		req.Header.Set("Origin", "https://localhost")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// 全量读取 body（流式响应可能被自动关流）。
	buf, _ := io.ReadAll(res.Body)
	resp.statusCode = res.StatusCode
	resp.body = buf
	return
}
