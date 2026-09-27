package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

/* Service 是 magic-client 插件的本地服务：给它一个 profile，它提供
 * 「基础版 Agent UI + 九项扩展接口 + 壳能力」里**服务侧**的那一半。
 *
 * 三件事必须做对（见 magic-client 方案设计）：
 *  1) 契约校验：启动时读 core 的 --contract，版本或能力不匹配就**明确报错**，
 *     不带着不匹配的能力跑起来 —— 这类漂移的表现是界面空白，最难定位。
 *  2) 只走 core 的公开面：spawn `magic-agent` 子进程收 NDJSON 事件，**不解析引擎输出**。
 *  3) 默认只绑 127.0.0.1 且要令牌：本地服务对同机所有进程可见，不开鉴权等于把
 *     引擎执行权交给任意本机进程。
 */

// RequiredContractVersion 本插件能解析的 core 契约版本。
const RequiredContractVersion = 1

// requiredFeatures 插件依赖的 core 通道能力。缺任何一个都不能工作：
// 没有 events 就没有带游标的事件流，没有 control 就无法打断与回审批。
var requiredFeatures = []string{"feature.session.events", "feature.session.control"}

// Options 启动参数。
type Options struct {
	// MagicAgent core 可执行文件；空 = 从 PATH 找 magic-agent。
	MagicAgent string
	Profile    Profile
	// UIRoot 静态资源目录（开发态从磁盘加载；发布态由调用方指向内置资源解包目录）。
	UIRoot string
	// Token 访问令牌；空 = 启动时随机生成。
	Token  string
	Logger *log.Logger
}

// Service 本地服务。
type Service struct {
	opts     Options
	log      *log.Logger
	contract Contract
	// backend `/api/*` 的反向代理；profile 没配 backend.url 时为 nil。
	// 在 New() 里一次建好，避免在请求路径上惰性初始化（那是数据竞争）。
	backend *httputil.ReverseProxy

	mu   sync.Mutex
	runs map[string]*askRun
	seq  int64
}

// Contract core `--contract` 里本插件关心的部分。
type Contract struct {
	ContractVersion int      `json:"contractVersion"`
	Features        []string `json:"features"`
	Engines         []struct {
		Engine       string   `json:"engine"`
		OK           bool     `json:"ok"`
		Bin          string   `json:"bin"`
		Capabilities []string `json:"capabilities"`
	} `json:"engines"`
}

// New 建服务并做契约校验。
func New(opts Options) (*Service, error) {
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "magic-client: ", 0)
	}
	if strings.TrimSpace(opts.MagicAgent) == "" {
		opts.MagicAgent = "magic-agent"
	}
	s := &Service{opts: opts, log: opts.Logger, runs: map[string]*askRun{}}

	// 项目后端的反代一次建好：profile 合法就给代理，非法就启动即失败
	//（地址写错比运行时 502 更容易查——启动日志里说清楚）。
	if target := strings.TrimSpace(opts.Profile.Backend.URL); target != "" {
		rp, err := buildBackendProxy(opts.Profile.Backend, target)
		if err != nil {
			return nil, err
		}
		s.backend = rp
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, opts.MagicAgent, "--contract").Output()
	if err != nil {
		return nil, fmt.Errorf("读 core 契约失败（%s --contract）：%w", opts.MagicAgent, err)
	}
	if err := json.Unmarshal(out, &s.contract); err != nil {
		return nil, fmt.Errorf("解析 core 契约失败：%w", err)
	}
	if s.contract.ContractVersion != RequiredContractVersion {
		return nil, fmt.Errorf(
			"core 契约版本 %d 与本插件要求的 %d 不一致：请升级 magic-agent（或换用匹配的插件版本）",
			s.contract.ContractVersion, RequiredContractVersion)
	}
	for _, f := range requiredFeatures {
		if !hasStr(s.contract.Features, f) {
			return nil, fmt.Errorf("core 缺通道能力 %s（需要支持 --stream --events 与 --control 的版本）", f)
		}
	}
	return s, nil
}

// Contract 已校验过的 core 契约（给启动日志与 /desk/health 用）。
func (s *Service) Contract() Contract { return s.contract }

// MagicAgentPath 实际使用的 core 可执行文件。
func (s *Service) MagicAgentPath() string { return s.opts.MagicAgent }

// Handler 路由。
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/desk/health", s.guard(s.handleHealth))
	mux.HandleFunc("/desk/contract", s.guard(s.handleContract))
	mux.HandleFunc("/desk/engines", s.guard(s.handleEngines))
	mux.HandleFunc("/desk/profile", s.guard(s.handleProfile))
	mux.HandleFunc("/desk/modules", s.guard(s.handleModules))
	mux.HandleFunc("/desk/sessions", s.guard(s.handleSessions))
	mux.HandleFunc("/desk/asks", s.guard(s.handleAsks))
	mux.HandleFunc("/desk/ask", s.guard(s.handleAsk))
	mux.HandleFunc("/desk/ask/", s.guard(s.handleAskSub))
	// 业务模块的路由：原样转发到项目自己的后端（见 Profile.Backend 的注释）。
	mux.HandleFunc("/api/", s.guard(s.handleProjectAPI))
	// 业务模块的静态资源：优先项目自己的模块目录（profile 的 ui.modulesDir）。
	mux.HandleFunc("/modules/", s.handleModulesStatic)
	// 二维码配对：把「桌面地址 + 令牌」给手机扫。内容等同于令牌，所以用普通鉴权把门
	// （拿到令牌的人本来就拥有它；没令牌的人只会看到 401）。
	mux.HandleFunc("/pair", s.guard(s.handlePairPage))
	mux.HandleFunc("/pair/qr.png", s.guard(s.handlePairQR))
	mux.HandleFunc("/desk/pair", s.guard(s.handlePairInfo))
	mux.HandleFunc("/", s.handleStatic)
	return mux
}

// guard 鉴权：`X-Magic-Token` 头或 `token` 查询参数。静态资源不鉴权（它不含密钥，
// 真正的执行面都在 /desk/* 后面）。
func (s *Service) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := s.opts.Token
		got := r.Header.Get("X-Magic-Token")
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if want != "" && got != want {
			writeErr(w, http.StatusUnauthorized, "缺少或错误的访问令牌（X-Magic-Token / ?token=）")
			return
		}
		next(w, r)
	}
}

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"contractVersion": s.contract.ContractVersion,
		"features":        s.contract.Features,
		"app":             s.opts.Profile.App.Name,
		"engines":         len(s.contract.Engines),
	})
}

// handleContract 原样转发 core 的契约（客户端拿它做能力降级，不需要经过插件再解释一遍）。
func (s *Service) handleContract(w http.ResponseWriter, r *http.Request) {
	s.proxyCore(w, r, "--contract")
}

// handleEngines 引擎清单（含模型；走 --no-models 的快路径由调用方决定）。
func (s *Service) handleEngines(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("models") == "1" {
		s.proxyCore(w, r, "--engines")
		return
	}
	s.proxyCore(w, r, "--engines", "--no-models")
}

func (s *Service) handleProfile(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.opts.Profile)
}

// handleSessions 会话登记表（core 的 `--sessions`）：列表页的「最近会话」用它。
// 与 /desk/asks 的区别：asks 是**本进程正在跑的轮次**（内存里），sessions 是 core 的
// 持久登记表（跨进程、含已结束的）。
func (s *Service) handleSessions(w http.ResponseWriter, r *http.Request) {
	s.proxyCore(w, r, "--sessions")
}

// moduleInfo 模块清单的一项：基础版 UI 按它决定加载谁。
type moduleInfo struct {
	ID         string   `json:"id"`
	Entry      string   `json:"entry,omitempty"`
	URL        string   `json:"url,omitempty"`
	DataTables []string `json:"dataTables,omitempty"`
	// Loadable 前端能否加载（有 entry 且 UIRoot 已配置）。
	Loadable bool `json:"loadable"`
}

// handleModules 模块清单：profile 里的 modules 派生而来。
//
// 插件只回答「有哪些模块、入口在哪、它声明了哪些表」——**不含任何领域假设**，
// 也不解释 dataTables 的含义（那是模块与它自己后端之间的事）。
func (s *Service) handleModules(w http.ResponseWriter, r *http.Request) {
	hasUI := strings.TrimSpace(s.opts.UIRoot) != ""
	out := make([]moduleInfo, 0, len(s.opts.Profile.Modules))
	for _, m := range s.opts.Profile.Modules {
		mi := moduleInfo{ID: m.ID, Entry: m.Entry, DataTables: m.DataTables}
		if m.Entry != "" {
			/* ⚠️ URL 必须带 `/modules/` 前缀 —— 那是本服务挂载模块静态资源的路径。
			   漏了前缀的表现是「清单里明明有、点开 404」，而且单测（只断言字段）抓不到，
			   第一次真机点模块才发现。 */
			mi.URL = "/modules/" + strings.TrimPrefix(m.Entry, "/")
		}
		mi.Loadable = hasUI && m.Entry != ""
		out = append(out, mi)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"modules": out,
		"backend": s.opts.Profile.Backend.URL, // 空串 = 未配置，/api/* 会回 501
	})
}

// handleProjectAPI 把 `/api/*` 原样转发给项目自己的后端。
//
// 三条设计约束：
//  1. **不另建存储**：业务数据的真相在项目那边，插件转发而不是复制（否则两个真相源）。
//  2. **凭据服务端注入**：profile 里的 backend.token 由插件加进请求头，不进浏览器。
//  3. **支持流式**：项目的后端可能用 SSE 推进度（magic-test 的派发流就是），
//     所以用 ReverseProxy + 立即 flush，不缓冲整段响应。
func (s *Service) handleProjectAPI(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSpace(s.opts.Profile.Backend.URL)
	if target == "" || s.backend == nil {
		writeErr(w, http.StatusNotImplemented,
			"没有配置项目后端：profile 里补 \"backend\": {\"url\": \"http://127.0.0.1:<port>\"}。"+
				"业务数据（需求 / 用例 / 产物 / 设置）的真相在项目侧，插件只转发不另建存储。")
		return
	}
	s.backend.ServeHTTP(w, r)
}

// buildBackendProxy 建 `/api/*` 的反向代理。
//
// 三条设计约束：
//  1. **不另建存储**：业务数据的真相在项目那边，插件转发而不是复制（否则两个真相源）；
//  2. **凭据服务端注入**：backend.token 由插件加进请求头，不进浏览器；
//  3. **支持流式**：项目后端可能用 SSE 推进度（magic-test 的派发流就是），
//     FlushInterval=-1 让分片立即到达，不缓冲整段响应。
func buildBackendProxy(cfg BackendConfig, target string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("profile 里的 backend.url 不是合法地址：%q", target)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.FlushInterval = -1
	orig := rp.Director
	rp.Director = func(req *http.Request) {
		orig(req)
		req.Host = u.Host
		if tok := strings.TrimSpace(cfg.Token); tok != "" {
			if h := strings.TrimSpace(cfg.TokenHeader); h != "" {
				req.Header.Set(h, tok)
			} else {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		// 插件自己的令牌不带去后端：那是两套凭据，混着传会让后端误以为
		// 它需要校验我们的令牌。
		req.Header.Del("X-Magic-Token")
	}
	rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("转发到项目后端 %s 失败：%v", target, err))
	}
	return rp, nil
}

func (s *Service) handleAsks(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID      string `json:"id"`
		Engine  string `json:"engine"`
		Started string `json:"started"`
	}
	s.mu.Lock()
	items := make([]item, 0, len(s.runs))
	for _, run := range s.runs {
		items = append(items, item{ID: run.id, Engine: run.engine, Started: run.started.Format(time.RFC3339)})
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, items)
}

// proxyCore 跑一条只读的 core 命令（--contract / --engines）并把 stdout 原样转发。
func (s *Service) proxyCore(w http.ResponseWriter, r *http.Request, args ...string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.opts.MagicAgent, args...).Output()
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("%s %s 失败：%v", s.opts.MagicAgent, strings.Join(args, " "), err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(out)
}

// askRequest POST /desk/ask 的请求体。
type askRequest struct {
	Engine    string `json:"engine"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// Session 续接一个已有会话（传 /desk/sessions 里的 session_id 或 run_id）。
	// 空 = 新会话。
	Session    string `json:"session,omitempty"`
	NoThinking bool   `json:"noThinking,omitempty"`
	// Idle 常驻会话空闲收工时长；默认 0 = 本轮结束就收工（客户端驱动的轮次语义）。
	// >0 才留常驻窗口（此时 answer 命令在轮次结束后仍可用）。
	Idle string `json:"idle,omitempty"`
}

// askRun 一次正在跑的 ask。
type askRun struct {
	id      string
	engine  string
	started time.Time
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	done    chan struct{}
	code    int
	stderr  *cappedBuffer
}

// handleAsk 发起一轮并**把 core 的 NDJSON 事件原样中继**给调用方（SSE）。
//
// 中继而不是解释：事件契约的解析权在客户端（桌面 / 移动端各一份实现），
// 插件只保证「顺序不变、不丢行、结束有明确的收尾事件」。
func (s *Service) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req askRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, "prompt 不能为空")
		return
	}
	engine := strings.TrimSpace(req.Engine)
	if engine == "" {
		writeErr(w, http.StatusBadRequest, "engine 不能为空（清单见 /desk/engines）")
		return
	}
	if !s.opts.Profile.AllowsEngine(engine) {
		writeErr(w, http.StatusForbidden, fmt.Sprintf("profile 白名单不允许引擎 %q", engine))
		return
	}

	idle := strings.TrimSpace(req.Idle)
	if idle == "" {
		idle = "0"
	}
	args := []string{
		"-e", engine,
		"--stream", "--events", "--control",
		"--idle", idle,
	}
	if req.Model != "" {
		args = append(args, "-m", req.Model)
	}
	if req.Workspace != "" {
		args = append(args, "-w", req.Workspace)
	}
	if req.Session != "" {
		args = append(args, "--session", req.Session)
	}
	if req.NoThinking {
		args = append(args, "--no-thinking")
	}
	args = append(args, "-p", req.Prompt)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd := exec.CommandContext(ctx, s.opts.MagicAgent, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stdin pipe: "+err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stdout pipe: "+err.Error())
		return
	}
	run := &askRun{
		id:      s.nextID(),
		engine:  engine,
		started: time.Now(),
		cmd:     cmd,
		stdin:   stdin,
		done:    make(chan struct{}),
		stderr:  newCappedBuffer(8 << 10),
	}
	cmd.Stderr = run.stderr
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusBadGateway, "启动 core 失败："+err.Error())
		return
	}
	s.mu.Lock()
	s.runs[run.id] = run
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.runs, run.id)
		s.mu.Unlock()
	}()

	// SSE 头必须在**第一行事件之前**发出，否则客户端拿到的是 application/json。
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	send := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send("open", map[string]any{"id": run.id, "engine": engine, "core": s.opts.MagicAgent})

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// 原样中继：核心事件流自己就是 NDJSON，不再包一层。
		fmt.Fprintf(w, "data: %s\n\n", line)
		if flusher != nil {
			flusher.Flush()
		}
	}
	werr := cmd.Wait()
	code := 0
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	run.code = code
	close(run.done)
	send("end", map[string]any{"id": run.id, "code": code, "stderr": run.stderr.String()})
}

// handleAskSub 处理 `/desk/ask/{id}/control`：把一条控制命令写进 core 的 stdin。
//
// 这就是「手机回审批」的服务侧路径 —— 命令格式与 core 的 --control 契约一致，
// 插件不解释 op，只转发（解析权在 core）。
func (s *Service) handleAskSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/desk/ask/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[1] != "control" {
		writeErr(w, http.StatusNotFound, "用法：POST /desk/ask/{id}/control")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	s.mu.Lock()
	run := s.runs[parts[0]]
	s.mu.Unlock()
	if run == nil {
		writeErr(w, http.StatusNotFound, "没有这个 ask（可能已结束）："+parts[0])
		return
	}
	var cmd struct {
		Op     string `json:"op"`
		Text   string `json:"text,omitempty"`
		Reason string `json:"reason,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&cmd); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	if strings.TrimSpace(cmd.Op) == "" {
		writeErr(w, http.StatusBadRequest, "op 不能为空（ping / interrupt / stop / answer）")
		return
	}
	b, _ := json.Marshal(cmd)
	if _, err := run.stdin.Write(append(b, '\n')); err != nil {
		writeErr(w, http.StatusConflict, "写 core stdin 失败（本轮可能已结束）："+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "op": cmd.Op, "id": run.id})
}

// handleModulesStatic 业务模块的静态资源：**先**项目的模块目录（profile 的 ui.modulesDir），
// 再退回插件的 UI 目录（--ui-dir）。
//
// 为什么要有单独的挂载点：模块文件属于项目（在项目仓库里跟着一起改），而基础版 UI 属于插件。
// 混在一个目录里会让「插件升级」和「模块改动」互相踩。两个根都支持是为了让简单项目
// （模块就一两个文件）也能直接把文件放 UI 目录下，不强迫多配一个字段。
func (s *Service) handleModulesStatic(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/modules/")
	roots := make([]string, 0, 2)
	if d := strings.TrimSpace(s.opts.Profile.UI.ModulesDir); d != "" {
		roots = append(roots, d)
	}
	if d := strings.TrimSpace(s.opts.UIRoot); d != "" {
		roots = append(roots, d)
	}
	if len(roots) == 0 {
		writeErr(w, http.StatusNotFound, "没有配置模块目录：profile 的 ui.modulesDir 或 --ui-dir")
		return
	}
	for _, d := range roots {
		root, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		// Clean("/"+rel) 会把 `..` 收进根内，因此拼接后不可能逃出 root；HasPrefix 只作双保险。
		full := filepath.Join(root, filepath.Clean("/"+rel))
		if !strings.HasPrefix(full, root) {
			continue
		}
		/* ⚠️ 这里**不能**用 http.ServeFile：请求路径以 `/index.html` 结尾时它会 301 重定向
		   （`/modules/index.html` → `/modules/`），于是那个文件永远读不到 —— 现象是
		   「模块清单里明明有，一点就 404」。ServeContent 不做这个重定向。 */
		f, err := os.Open(full)
		if err != nil {
			continue
		}
		st, err := f.Stat()
		if err != nil || st.IsDir() {
			_ = f.Close()
			continue
		}
		http.ServeContent(w, r, st.Name(), st.ModTime(), f)
		_ = f.Close()
		return
	}
	writeErr(w, http.StatusNotFound, "模块文件不存在："+rel)
}

// handleStatic 基础版 UI 与业务模块的静态资源。
func (s *Service) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.opts.UIRoot) == "" {
		writeErr(w, http.StatusNotFound, "没有配置 UI 目录（--ui-dir），本服务只提供 /desk/* 接口")
		return
	}
	root, err := filepath.Abs(s.opts.UIRoot)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = s.opts.Profile.UI.Entry
		if p == "" {
			p = "index.html"
		}
	}
	full := filepath.Join(root, filepath.Clean(p))
	if !strings.HasPrefix(full, root) {
		writeErr(w, http.StatusForbidden, "路径越界")
		return
	}
	http.ServeFile(w, r, full)
}

func (s *Service) nextID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return fmt.Sprintf("ask-%d-%d", time.Now().UnixNano()/1e6, s.seq)
}

// cappedBuffer 只留最后 N 字节（stderr 排障用，不无限增长）。
type cappedBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newCappedBuffer(max int) *cappedBuffer { return &cappedBuffer{max: max} }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	if len(c.buf) > c.max {
		c.buf = c.buf[len(c.buf)-c.max:]
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

func hasStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}
