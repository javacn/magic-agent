package client

import (
	"bufio"
	"bytes"
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

// fakeCore 写一个假的 magic-agent 脚本：既能回答 --contract / --engines，
// 又能在 ask 模式下先发两行事件、再把 stdin 收到的控制命令**回显**成事件。
// 回显是关键 —— 它让「控制命令有没有真的到 core 的 stdin」变成可断言的事实。
func fakeCore(t *testing.T, contract string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "magic-agent")
	script := `#!/bin/sh
case "$*" in
  *--contract*)
    echo '` + contract + `'
    exit 0
    ;;
  *--engines*)
    echo '[{"engine":"claude","ok":true,"capabilities":["session.stream"]}]'
    exit 0
    ;;
  *--sessions*)
    echo '[{"run_id":"run-1","session_id":"sess-1","engine":"claude","state":"done"}]'
    exit 0
    ;;
esac
# 把收到的参数也回显成一行事件：让「插件有没有把 --session 透传下去」变成可断言的事实。
printf '{"v":1,"seq":0,"type":"args","raw":"%s"}\n' "$*"
echo '{"v":1,"seq":1,"type":"ready","engine":"claude"}'
echo '{"v":1,"seq":2,"type":"text","text":"hi"}'
while IFS= read -r line; do
  # 转义后再回显：命令里含引号（{"op":"ping"}），不转义就不是合法 JSON。
  esc=$(printf '%s' "$line" | sed 's/\\/\\\\/g; s/"/\\"/g')
  printf '{"v":1,"seq":3,"type":"control_echo","raw":"%s"}\n' "$esc"
done
echo '{"v":1,"seq":4,"type":"result","text":"hi"}'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const goodContract = `{"contractVersion":1,"features":["feature.session.events","feature.session.control"],"engines":[{"engine":"claude","ok":true,"capabilities":["session.stream"]}]}`

func newTestService(t *testing.T, core string) *Service {
	t.Helper()
	svc, err := New(Options{MagicAgent: core, Profile: DefaultProfile(), Token: "tok"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

// TestNewRejectsContractMismatch 契约版本或通道能力不匹配时必须**启动即失败**，
// 而不是带着不匹配的能力跑起来（那类漂移在界面上表现为空白，最难定位）。
func TestNewRejectsContractMismatch(t *testing.T) {
	t.Run("版本不符", func(t *testing.T) {
		core := fakeCore(t, `{"contractVersion":99,"features":["feature.session.events"],"engines":[]}`)
		_, err := New(Options{MagicAgent: core, Profile: DefaultProfile()})
		if err == nil {
			t.Fatal("版本不符应当报错")
		}
		if !strings.Contains(err.Error(), "契约版本") {
			t.Errorf("报错应说明契约版本，实际: %v", err)
		}
	})

	t.Run("缺通道能力", func(t *testing.T) {
		core := fakeCore(t, `{"contractVersion":1,"features":["feature.session.events"],"engines":[]}`)
		_, err := New(Options{MagicAgent: core, Profile: DefaultProfile()})
		if err == nil {
			t.Fatal("缺 control 能力应当报错")
		}
		if !strings.Contains(err.Error(), "feature.session.control") {
			t.Errorf("报错应点名缺哪个能力，实际: %v", err)
		}
	})

	t.Run("正常", func(t *testing.T) {
		svc := newTestService(t, fakeCore(t, goodContract))
		if got := svc.Contract().ContractVersion; got != RequiredContractVersion {
			t.Errorf("contractVersion = %d want %d", got, RequiredContractVersion)
		}
	})
}

// TestAuthRequired 静态资源不鉴权，但 /desk/* 必须带令牌 ——
// 本地端口对同机所有进程可见，不鉴权等于把引擎执行权交出去。
func TestAuthRequired(t *testing.T) {
	svc := newTestService(t, fakeCore(t, goodContract))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/desk/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无令牌访问 /desk/health 应 401，实际 %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/desk/health", nil)
	req.Header.Set("X-Magic-Token", "tok")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("带令牌应 200，实际 %d", resp2.StatusCode)
	}
	var health struct {
		OK              bool     `json:"ok"`
		ContractVersion int      `json:"contractVersion"`
		Features        []string `json:"features"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if !health.OK || health.ContractVersion != 1 || len(health.Features) != 2 {
		t.Errorf("health 内容不对: %+v", health)
	}
}

// TestProfileGateEngine profile 白名单之外的引擎必须被拦住（403），
// 否则「按需个性化配置」就是一句空话。
func TestProfileGateEngine(t *testing.T) {
	core := fakeCore(t, goodContract)
	prof := DefaultProfile()
	prof.Engines = []string{"claude"}
	svc, err := New(Options{MagicAgent: core, Profile: prof, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	body, _ := json.Marshal(askRequest{Engine: "codex", Prompt: "x"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/desk/ask", bytes.NewReader(body))
	req.Header.Set("X-Magic-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("白名单外的引擎应 403，实际 %d", resp.StatusCode)
	}
}

// sseReader 逐行读 SSE（data: 后面的 JSON）。
type sseReader struct {
	sc   *bufio.Scanner
	resp *http.Response
}

func openSSE(t *testing.T, url, token string, body any) *sseReader {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url+"/desk/ask", bytes.NewReader(b))
	req.Header.Set("X-Magic-Token", token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("/desk/ask 应 200，实际 %d（%s）", resp.StatusCode, msg)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &sseReader{sc: sc, resp: resp}
}

// next 读到下一条 data: 事件（跳过 event: 行与空行）。
func (r *sseReader) next(t *testing.T) map[string]any {
	t.Helper()
	for r.sc.Scan() {
		line := strings.TrimSpace(r.sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
			t.Fatalf("SSE 的 data 不是 JSON: %q", line)
		}
		return m
	}
	t.Fatal("SSE 结束但还没读到期望的事件")
	return nil
}

func (r *sseReader) close() { _ = r.resp.Body.Close() }

// TestAskRelaysAndControl 中继与控制通道的端到端：
//  1. core 的每一行原样到达客户端（顺序与内容都不改）；
//  2. `POST /desk/ask/{id}/control` 写进 core 的 stdin（假 core 会回显，构成可断言证据）。
func TestAskRelaysAndControl(t *testing.T) {
	svc := newTestService(t, fakeCore(t, goodContract))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	sse := openSSE(t, srv.URL, "tok", askRequest{Engine: "claude", Prompt: "只回复 hi"})
	defer sse.close()

	// 第一条是插件自己的 open 事件（带 ask id）；之后才是 core 的事件，原样中继。
	opened := sse.next(t)
	askID, _ := opened["id"].(string)
	if askID == "" {
		t.Fatalf("open 事件应带 ask id，实际 %v", opened)
	}
	_ = sse.next(t) // 假 core 的 args 回显行（另一条用例专门断言它）
	first := sse.next(t)
	if first["type"] != "ready" || first["v"] != float64(1) {
		t.Fatalf("第二条应是 core 的 ready（原样中继），实际 %v", first)
	}
	second := sse.next(t)
	if second["type"] != "text" || second["text"] != "hi" {
		t.Fatalf("第三条应是 text，实际 %v", second)
	}

	// 控制命令走 /desk/ask/{id}/control：id 从 /desk/asks 取（运行中唯一那条）。
	ctrlBody, _ := json.Marshal(map[string]string{"op": "ping"})
	reqList, _ := http.NewRequest(http.MethodGet, srv.URL+"/desk/asks", nil)
	reqList.Header.Set("X-Magic-Token", "tok")
	respList, err := http.DefaultClient.Do(reqList)
	if err != nil {
		t.Fatal(err)
	}
	var asks []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(respList.Body).Decode(&asks); err != nil {
		t.Fatal(err)
	}
	respList.Body.Close()
	if len(asks) != 1 {
		t.Fatalf("/desk/asks 应有 1 条运行中的 ask，实际 %d", len(asks))
	}
	if asks[0].ID != askID {
		t.Errorf("/desk/asks 的 id (%s) 应与 open 事件的 id (%s) 一致", asks[0].ID, askID)
	}

	reqC, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/desk/ask/%s/control", srv.URL, asks[0].ID), bytes.NewReader(ctrlBody))
	reqC.Header.Set("X-Magic-Token", "tok")
	respC, err := http.DefaultClient.Do(reqC)
	if err != nil {
		t.Fatal(err)
	}
	defer respC.Body.Close()
	if respC.StatusCode != http.StatusAccepted {
		msg, _ := io.ReadAll(respC.Body)
		t.Fatalf("控制接口应 202，实际 %d（%s）", respC.StatusCode, msg)
	}

	// 假 core 会把 stdin 收到的命令回显成 control_echo —— 这就是「命令真的到了 core」的证据。
	echo := sse.next(t)
	if echo["type"] != "control_echo" {
		t.Fatalf("应收到假 core 的回显事件，实际 %v", echo)
	}
	raw, _ := echo["raw"].(string)
	if !strings.Contains(raw, `"op":"ping"`) {
		t.Errorf("回显内容应含 op=ping，实际 %q", raw)
	}
}

// TestSessionsProxy 列表页要的是 core 的**持久会话登记表**（`--sessions`）。
func TestSessionsProxy(t *testing.T) {
	svc := newTestService(t, fakeCore(t, goodContract))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/desk/sessions", nil)
	req.Header.Set("X-Magic-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/desk/sessions 应 200，实际 %d", resp.StatusCode)
	}
	var rows []struct {
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SessionID != "sess-1" {
		t.Errorf("应原样转发 core 的会话表，实际 %+v", rows)
	}
}

// TestAskPassesSession 续接参数必须透传到 core 的 `--session` ——
// 否则「点历史会话继续」会静默变成新会话（用户看到的是「它不记得上一轮」）。
func TestAskPassesSession(t *testing.T) {
	svc := newTestService(t, fakeCore(t, goodContract))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	sse := openSSE(t, srv.URL, "tok", askRequest{Engine: "claude", Prompt: "接着上一轮", Session: "sess-1"})
	defer sse.close()
	_ = sse.next(t) // open

	args := sse.next(t)
	raw, _ := args["raw"].(string)
	if !strings.Contains(raw, "--session sess-1") {
		t.Errorf("core 收到的参数里应有 --session sess-1，实际 %q", raw)
	}
	if !strings.Contains(raw, "--events") || !strings.Contains(raw, "--control") {
		t.Errorf("core 收到的参数里应有 --events 与 --control，实际 %q", raw)
	}
}

// TestAskPassesPermission 授权档位要透传给 core 的 --permission。
// 反面同样重要：**没选档位时不能替 core 补一个默认值** —— core 只在显式传过
// --permission 时才校验引擎支不支持（internal/cli/ask.go::resolvePermissionTier），
// 我们替它补默认值会把「显式指定」与「沿用默认」混成一种，让不支持的引擎凭空报错。
func TestAskPassesPermission(t *testing.T) {
	svc := newTestService(t, fakeCore(t, goodContract))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	cases := []struct {
		name string
		req  askRequest
		want string
	}{
		{"显式选档", askRequest{Engine: "claude", Prompt: "改代码", Permission: "manual"}, "--permission manual"},
		{"accept-edits", askRequest{Engine: "claude", Prompt: "改代码", Permission: "accept-edits"}, "--permission accept-edits"},
		{"full 危险档", askRequest{Engine: "codebuddy", Prompt: "跑起来", Permission: "full"}, "--permission full"},
		{"未选档位就不传", askRequest{Engine: "claude", Prompt: "只读"}, ""},
		{"空白按未选处理", askRequest{Engine: "claude", Prompt: "只读", Permission: "   "}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sse := openSSE(t, srv.URL, "tok", c.req)
			defer sse.close()
			_ = sse.next(t) // open

			raw, _ := sse.next(t)["raw"].(string)
			if c.want == "" {
				if strings.Contains(raw, "--permission") {
					t.Errorf("未选档位时不该传 --permission，实际 %q", raw)
				}
				return
			}
			if !strings.Contains(raw, c.want) {
				t.Errorf("core 收到的参数里应有 %q，实际 %q", c.want, raw)
			}
		})
	}
}

// TestProjectAPINotConfigured 没配项目后端时 `/api/*` 回 501 并**点名 profile 字段**，
// 不静默 404（静默 404 会让人以为是模块前端写错了）。
func TestProjectAPINotConfigured(t *testing.T) {
	svc := newTestService(t, fakeCore(t, goodContract))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/docs", nil)
	req.Header.Set("X-Magic-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("/api/* 未配后端应 501，实际 %d", resp.StatusCode)
	}
	var m map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m["error"], "backend") {
		t.Errorf("错误信息应点名 profile 的 backend 字段，实际 %q", m["error"])
	}
}

// TestProjectAPIForwards `/api/*` 原样转发；凭据由服务端注入，插件自己的令牌不带去后端。
//
// 这三条一起构成「业务视图能搬但业务逻辑不用重写」的前提：路径 / 方法 / 查询不变，
// 凭据不泄漏到浏览器，两套令牌不互相污染。
func TestProjectAPIForwards(t *testing.T) {
	type seen struct {
		path, query, auth, magic string
	}
	var got seen
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"), magic: r.Header.Get("X-Magic-Token")}
		writeJSON(w, http.StatusOK, map[string]any{"表格": "docs", "行数": 3})
	}))
	defer backend.Close()

	prof := DefaultProfile()
	prof.Backend = BackendConfig{URL: backend.URL, Token: "backend-secret"}
	svc, err := New(Options{MagicAgent: fakeCore(t, goodContract), Profile: prof, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/docs?page=2", nil)
	req.Header.Set("X-Magic-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/docs 应 200，实际 %d", resp.StatusCode)
	}

	if got.path != "/api/docs" {
		t.Errorf("后端收到的路径应为 /api/docs，实际 %q", got.path)
	}
	if got.query != "page=2" {
		t.Errorf("查询串应原样转发，实际 %q", got.query)
	}
	if got.auth != "Bearer backend-secret" {
		t.Errorf("应注入后端凭据，实际 %q", got.auth)
	}
	if got.magic != "" {
		t.Errorf("插件的令牌不该带去后端，实际 %q", got.magic)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["表格"] != "docs" {
		t.Errorf("后端响应应原样回来，实际 %v", body)
	}
}

// TestProjectAPIRejectsBadURL 地址写错时启动即失败（比运行时 502 好查）。
func TestProjectAPIRejectsBadURL(t *testing.T) {
	prof := DefaultProfile()
	prof.Backend = BackendConfig{URL: "不是地址"}
	if _, err := New(Options{MagicAgent: fakeCore(t, goodContract), Profile: prof}); err == nil {
		t.Fatal("非法 backend.url 应当启动即失败")
	}
}

// TestModulesEndpoint 模块清单从 profile 派生：只回答「有哪些模块、入口在哪、声明了哪些表」。
func TestModulesEndpoint(t *testing.T) {
	prof := DefaultProfile()
	prof.Modules = []Module{
		{ID: "cases", Entry: "cases.js"},
		{ID: "reqboard", Entry: "reqboard.js", DataTables: []string{"workspaces", "requirements"}},
	}
	svc, err := New(Options{MagicAgent: fakeCore(t, goodContract), Profile: prof, Token: "tok", UIRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/desk/modules", nil)
	req.Header.Set("X-Magic-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Modules []struct {
			ID         string   `json:"id"`
			URL        string   `json:"url"`
			DataTables []string `json:"dataTables"`
			Loadable   bool     `json:"loadable"`
		} `json:"modules"`
		Backend string `json:"backend"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Modules) != 2 {
		t.Fatalf("应有 2 个模块，实际 %d", len(out.Modules))
	}
	// URL 必须带上挂载点前缀 `/modules/`（漏了就是「清单里有点开 404」）。
	if out.Modules[0].ID != "cases" || out.Modules[0].URL != "/modules/cases.js" || !out.Modules[0].Loadable {
		t.Errorf("第一个模块不对: %+v", out.Modules[0])
	}
	if len(out.Modules[1].DataTables) != 2 {
		t.Errorf("模块声明的表应原样回，实际 %+v", out.Modules[1])
	}
	if out.Backend != "" {
		t.Errorf("未配后端时 backend 应为空串，实际 %q", out.Backend)
	}
}

// TestModulesStatic 模块静态资源的两个根与优先级：
// 先项目自己的 `ui.modulesDir`，再退回插件的 `--ui-dir`；越界读不到根外的东西。
//
// 为什么要有独立的模块目录：模块文件属于项目（跟项目仓库一起改），基础版 UI 属于插件。
// 混在一个目录里，插件升级与模块改动会互相踩。
func TestModulesStatic(t *testing.T) {
	uiDir := t.TempDir()
	modDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte("from-ui"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "shared.js"), []byte("from-ui"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "shared.js"), []byte("from-project"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "cases.js"), []byte("cases-module"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(filepath.Dir(modDir), "secret-"+filepath.Base(modDir)+".txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	prof := DefaultProfile()
	prof.UI.ModulesDir = modDir
	svc, err := New(Options{MagicAgent: fakeCore(t, goodContract), Profile: prof, Token: "tok", UIRoot: uiDir})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	get := func(p string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+p, nil)
		req.Header.Set("X-Magic-Token", "tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 同名文件：项目目录优先
	if code, body := get("/modules/shared.js"); code != 200 || body != "from-project" {
		t.Errorf("/modules/shared.js 应取项目目录的，实际 %d %q", code, body)
	}
	// 项目目录里没有的，退回 UI 目录
	if code, body := get("/modules/index.html"); code != 200 || body != "from-ui" {
		t.Errorf("/modules/index.html 应退回 UI 目录，实际 %d %q", code, body)
	}
	// 项目模块能读到（这是「业务模块留在项目仓库里」的关键）
	if code, body := get("/modules/cases.js"); code != 200 || body != "cases-module" {
		t.Errorf("/modules/cases.js 应读到项目模块，实际 %d %q", code, body)
	}
	// 都不存在 → 404
	if code, _ := get("/modules/nope.js"); code != http.StatusNotFound {
		t.Errorf("不存在的模块应 404，实际 %d", code)
	}
	// 越界：Clean 把 `..` 收进根内，因此只能命中根内文件或 404，绝不可能读到根外
	if _, body := get("/modules/../" + filepath.Base(secret)); strings.Contains(body, "TOP-SECRET") {
		t.Error("越界路径读到了根外的文件")
	}
}

// TestLoadProfileDefaultsAndOverrides profile 只描述差异：省略的字段要有稳定默认值。
func TestLoadProfileDefaultsAndOverrides(t *testing.T) {
	t.Run("空路径给默认", func(t *testing.T) {
		p, err := LoadProfile("")
		if err != nil {
			t.Fatal(err)
		}
		if p.UI.Entry != "index.html" || p.AppID() != "magic-client" {
			t.Errorf("默认 profile 不对: entry=%q appID=%q", p.UI.Entry, p.AppID())
		}
		if len(p.Modules) != 0 {
			t.Errorf("默认 profile 不该有业务模块，实际 %v", p.Modules)
		}
	})

	t.Run("读文件并补默认", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "掌天瓶.json")
		content := `{"app":{"name":"掌天瓶","bundleId":"com.magic.test"},"modules":[{"id":"cases","entry":"modules/cases.js"}]}`
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := LoadProfile(path)
		if err != nil {
			t.Fatal(err)
		}
		if p.App.Name != "掌天瓶" || p.AppID() != "com.magic.test" {
			t.Errorf("app 字段没读到: %+v", p.App)
		}
		if p.UI.Entry != "index.html" {
			t.Errorf("省略的 ui.entry 应补默认，实际 %q", p.UI.Entry)
		}
		if len(p.Modules) != 1 || p.Modules[0].ID != "cases" {
			t.Errorf("业务模块清单没读到: %+v", p.Modules)
		}
		if !strings.HasSuffix(p.DataDir(), filepath.Join(".magic-client", "com.magic.test")) {
			t.Errorf("数据目录应按 appID 派生，实际 %q", p.DataDir())
		}
	})

	t.Run("坏文件报错", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte("{不是 JSON"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadProfile(path); err == nil {
			t.Fatal("坏 profile 应当报错")
		}
	})
}

// TestWriteErrShape 错误一律以 {"error": "..."} 回，客户端只认这一种形状。
func TestWriteErrShape(t *testing.T) {
	svc := newTestService(t, fakeCore(t, goodContract))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/desk/ask", strings.NewReader(`{"engine":"claude"}`))
	req.Header.Set("X-Magic-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 prompt 应 400，实际 %d", resp.StatusCode)
	}
	var m map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m["error"], "prompt") {
		t.Errorf("错误信息应提到 prompt，实际 %q", m["error"])
	}
}
