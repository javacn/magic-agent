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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darren/magic-agent/internal/session"
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
	// /desk/session/{id}/messages 是**前缀匹配**路由 —— 标准 ServeMux 把它挂上后，
	// 该前缀下任何 path 都先进这里（其它挂载顺序靠后也拦不住）。
	// 它同时承担 /desk/session/{id}/events（断线续读）的分派，见 handleSessionMessages。
	mux.HandleFunc("/desk/session/", s.guard(s.handleSessionMessages))
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
	return s.withCORS(mux)
}

/* withCORS 让**本机页面**能跨源访问本服务。
 *
 * ── 为什么必须有这一层 ──
 * 移动容器里页面是打包在 App 里的本地资源（Android + androidScheme=https ⇒ 源是
 * `https://localhost`），而插件跑在局域网 IP 上 —— 天生跨源。跨源就被 CORS 管，
 * 没有 CORS 头时浏览器**直接掐掉**响应：现象是闸门页探活永远停在「连接中…」，
 * APK 装得上、打得开、就是用不了。
 *
 * ⚠️ 这一层在 curl 里永远看不见：curl 不主动带 Origin，服务端也就没有跨源这回事。
 * 前几轮接口验证全绿，恰恰是因为它们没有浏览器这一层 —— 真浏览器一开就露。
 *
 * ── 为什么是白名单而不是 `*` ──
 * 本服务后面是能跑 claude/codebuddy 的**执行面**，`*` 等于允许用户访问的任意网页
 * 来敲这个本地端口。白名单只放行「本机页面」（容器 scheme 与 loopback 源），
 * 外站拿不到 CORS 头，浏览器照旧拦住它。
 *
 * ── 预检为什么必须绕过鉴权 ──
 * `X-Magic-Token` 是自定义头，浏览器会先发一条 OPTIONS 预检，而**预检请求不带业务头**
 * （它不可能带令牌）。若让预检走 guard，它必然 401，浏览器就把真正的请求判死。
 * 所以 OPTIONS 在 guard 之前就地答完，且它只是能力协商、不碰任何执行面。
 */
func (s *Service) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" {
			// Origin 会随请求方变，不加 Vary 会把一个源的响应喂给另一个源。
			w.Header().Add("Vary", "Origin")
			if localOrigin(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "X-Magic-Token, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			// 预检：只回能力协商，不进业务路由（也就不会因为没令牌而 401）。
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// localOrigin 判定「这是本机页面」：容器自定义 scheme，或 loopback 上的任意源。
//
// 不限端口与 http/https：桌面壳的开发服务器、容器、插件自己同源页面各不相同，
// 但它们的共同点是**都在这台机器上**。真正把门的是令牌，这一层只回答「浏览器让不让读」。
func localOrigin(origin string) bool {
	switch origin {
	case "capacitor://localhost", "ionic://localhost":
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
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

// handleAsks 会话登记表（core 的 `--sessions`）：列表页的「最近会话」用它。
// 与 /desk/asks 的区别：asks 是**本进程正在跑的轮次**（内存里），sessions 是 core 的
// 持久登记表（跨进程、含已结束的）。
func (s *Service) handleSessions(w http.ResponseWriter, r *http.Request) {
	s.proxyCore(w, r, "--sessions")
}

// handleSessionMessages 「看历史」持久化落盘的会话事件（`GET /desk/session/{id}/messages`）。
//
// 用途：移动端进会话详情时先拉历史 → 已经有内容可渲染；再开 SSE 接续。落盘由
// core CLI 在跑流时一行一行 append 到 ~/.magic-agent/sessions/<id>.jsonl
// （与本服务的转发/代理是两条独立路径，写盘独立于此服务 —— 即便用户没装本插件，
// 直接跑 magic-agent --stream 也会落盘）。
//
// 边界：
//   - 没历史 → 200 []，不是 404。UI 上看到「空会话」也是合理状态（不该报错）。
//   - 文件不在 → 200 []，read 端按 os.IsNotExist 静默返回。
//   - 写盘存在但损坏行 → 跳过损坏行；同 session 的其它正常行仍能拿到。
//   - 必须带 X-Magic-Token：这是会话数据（包含历史消息文本），没令牌读不到。
//
// 本函数同时是 `/desk/session/{id}/{action}` 这条前缀路由的**唯一入口**：
// `action` 为空或 `messages` 走全量（下面这段），`events` 走增量续读（见 handleSessionEvents）。
// 为什么在一个 handler 里分派而不再注册一条更具体的 pattern：`/desk/session/` 是**子树匹配**，
// 后注册的 pattern 并不会自动赢；而带 `{id}` 通配的新式 pattern 需要 Go 1.22+。
// 在入口处显式分派既不看 Go 版本脸色，也让「有哪些子路径」集中在一处可读。
func (s *Service) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	id, action, ok := parseSessionPath(r.URL.Path)
	if !ok {
		writeErr(w, http.StatusBadRequest, "session id 不能为空或路径分量")
		return
	}
	switch action {
	case "", "messages":
		// 全量：落到下面的读盘逻辑。
	case "events":
		s.handleSessionEvents(w, r, id)
		return
	default:
		// 未知子路径要**明说**，不能默默当全量返回 —— 否则客户端拼错路径时会拿到
		// 一份看起来正常的全量历史，于是「增量没生效」被当成「增量没实现」。
		writeErr(w, http.StatusNotFound, "未知子路径："+action+"（可用：messages / events）")
		return
	}
	dir, derr := session.DefaultLogDir()
	if derr != nil {
		writeErr(w, http.StatusInternalServerError, "读历史失败：找不到历史目录："+derr.Error())
		return
	}
	evs, err := session.ReadSessionLog(dir, id, sessionEventMapFromJSON)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读历史失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": id, "events": evs})
}

// sessionEventMapFromJSON 把 jsonl 的一行解析成 map（plugin 不依赖 cli 的 schema）。
func sessionEventMapFromJSON(line []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// parseSessionPath 从 `/desk/session/{id}/{action}` 拆出 id 与 action，并做**路径安全校验**。
//
// 返回 ok=false 的唯一情形是 id 为空或等于 `.` / `..`（调用方回 400）。
//
// ⚠️ id 是**文件名**的一部分（`<id>.jsonl`），所以校验必须在这里、且必须一次做全：
//   - 拒绝 `.` / `..` 整段（否则 id=".." 会指到上一级目录）
//   - 拒绝任何路径分隔符与控制字符（否则 "../../etc/passwd" 能探文件系统）
//
// action 允许为空（`/desk/session/{id}` 与 `/desk/session/{id}/messages` 等价）。
func parseSessionPath(path string) (id, action string, ok bool) {
	rest := strings.Trim(strings.TrimPrefix(path, "/desk/session/"), "/")
	if i := strings.Index(rest, "/"); i >= 0 {
		id, action = rest[:i], strings.Trim(rest[i+1:], "/")
	} else {
		id = rest
	}
	if id == "" || id == "." || id == ".." {
		return "", "", false
	}
	for _, c := range id {
		if c < 0x20 || c == 0x7f || c == '/' || c == '\\' {
			return "", "", false
		}
	}
	return id, action, true
}

// handleSessionEvents 增量续读：`GET /desk/session/{id}/events?after=<seq>`
// （`action` 已在 handleSessionMessages 里判过，这里只管读）。
//
// 语义对齐 agents-anywhere 的 `GET /sessions/{id}/events?after=seq:N`：客户端报
// 「我已经看到 seq=N」，服务端只回 seq > N 的部分。全量接口是 `/messages`；
// 这条存在的理由是**断线重连后不必重传整段历史**（长会话下差一个数量级）。
//
// 与 anywhere 的一处**刻意简化**：anywhere 在 `after == current` 时仍要回一条
// （`session.meta.updated` / `runtime.capability.updated`），因为它的「在线状态 / 能力」
// 是**不推进持久化 seq** 的独立维度 —— 客户端只靠 seq 对账会漏掉这两类变化。
// 我们没有这类旁路状态（在线与否由连接本身表达，能力是静态契约），所以
// 「没有新事件」就是真的什么都没有，回空数组即可。
//
// 三种结果，调用方按 `snapshotRequired` 分流：
//  1. 无新事件（含 after 正好等于最后一条）→ 200 + 空数组，snapshotRequired=false
//  2. 有新事件 → 200 + 增量数组，snapshotRequired=false
//  3. after **超前**于磁盘上的最大 seq → 200 + 空数组，snapshotRequired=**true**
//     超前只可能是「日志被重置/截断」或「游标来自另一个会话」；两种情况下
//     增量都不可信，客户端应改拉 `/messages` 全量重建。
//
// 边界：after 省略或为 0 = 全量（等价 /messages，但多带 lastSeq/after 元信息）；
// after 非数字或超 uint64 → 400（**不静默当 0** —— 那样客户端会以为拿到了增量，
// 实际每次都收全量，性能问题会被伪装成「没问题」）。
func (s *Service) handleSessionEvents(w http.ResponseWriter, r *http.Request, id string) {
	var after uint64
	afterRaw := strings.TrimSpace(r.URL.Query().Get("after"))
	if afterRaw != "" {
		n, err := strconv.ParseUint(afterRaw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "after 必须是非负整数（收到 "+strconv.Quote(afterRaw)+"）")
			return
		}
		after = n
	}
	dir, derr := session.DefaultLogDir()
	if derr != nil {
		writeErr(w, http.StatusInternalServerError, "读历史失败：找不到历史目录："+derr.Error())
		return
	}
	evs, maxSeq, err := session.ReadSessionLogAfter(dir, id, after, mapSeqOf, sessionEventMapFromJSON)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读历史失败："+err.Error())
		return
	}
	// 保证是数组而不是 null：客户端对这条接口的用法是「往已有画布上追加」，
	// 拿到 null 得处处判空，而空数组天然可迭代。（/messages 保持原行为不动 —— 已有测试钉着。）
	if evs == nil {
		evs = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session": id,
		"events":  evs,
		"count":   len(evs),
		"after":   after,
		"lastSeq": maxSeq,
		// 客户端游标比磁盘还新 ⇒ 增量不可信，去拉全量。
		// ⚠️ 判据是**严格大于**：after == maxSeq 是「已经追平」，那是正常的空增量。
		"snapshotRequired": after > maxSeq,
	})
}

// mapSeqOf 从解析后的 map 里取 seq。
//
// ⚠️ json.Unmarshal 进 `map[string]any` 时数字一律变 float64（除非开了 UseNumber），
// 所以这里只认 float64 —— 写一个 json.Number 分支是**没有调用方能到**的死代码。
// 非数字 / 负数 / 缺失一律返回 0：seq 从 1 起（见 cli/sessionlog.go 的 appendLine），
// 所以 0 天然被 `seq > after` 滤掉，不需要额外处理。
func mapSeqOf(m map[string]any) uint64 {
	f, ok := m["seq"].(float64)
	if !ok || f < 0 {
		return 0
	}
	return uint64(f)
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
	// Permission 四档权限模型（manual | accept-edits | auto | full），透传给 core 的
	// --permission。空 = 不传，由 core 用它自己的默认值（agent.DefaultPermissionTier）。
	// ⚠️ core 只在**显式传过** --permission 时才校验引擎是否支持（当前 claude / codebuddy /
	// codebuddy-ai）；所以这里只在客户端真的要选档位时才加这个参数，不要替 core 猜默认值，
	// 否则会把「显式指定」与「沿用默认」两种语义混成一种。
	Permission string `json:"permission,omitempty"`
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
	// 权限档位：只在客户端显式选了一档时才传，让 core 去校验该引擎支不支持。
	if p := strings.TrimSpace(req.Permission); p != "" {
		args = append(args, "--permission", p)
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
