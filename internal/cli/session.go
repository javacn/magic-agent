package cli

// session.go - CLI 侧的会话登记与「停止指定会话」入口。
//
// 用户需求（2026-09-18）：「要支持停止指定会话」。
//
// 三件事：
//  ① 每次调用（非流式 / 流式）都落一条会话记录 —— 见 ask.go 的 beginSession/finishSession；
//  ② `magic-agent --stop <session_id|run_id>`：按记录精确杀掉该会话的引擎进程组；
//  ③ 进程收到 SIGINT/SIGTERM 时，先带走自己的引擎子进程再退出 —— 否则调用方
//     kill 掉 magic-agent 只会留下一个还在跑的引擎孤儿（子 CLI 自成进程组，杀不到）。
//
// 登记表本身在 internal/session（无锁、一条一个文件）。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/darren/magic-agent/internal/agent"
	"github.com/darren/magic-agent/internal/session"
)

// exitInterrupted 被 SIGINT/SIGTERM 终止时的退出码（128 + 2）。
const exitInterrupted = 130

// 当前活动会话（一次进程通常只有一个：CLI 是一次调用一次进程）。
var (
	activeMu      sync.Mutex
	activeSession *session.Handle
)

// setActiveSession 记下当前活动会话（供信号处理路径使用）。
func setActiveSession(h *session.Handle) {
	activeMu.Lock()
	activeSession = h
	activeMu.Unlock()
}

// clearActiveSession 清掉当前活动会话（只在仍是同一个句柄时清，避免误清后来者）。
func clearActiveSession(h *session.Handle) {
	activeMu.Lock()
	if activeSession == h {
		activeSession = nil
	}
	activeMu.Unlock()
}

// stopActiveSession 杀掉当前活动会话的引擎进程（信号处理路径用）。
func stopActiveSession() {
	activeMu.Lock()
	h := activeSession
	activeMu.Unlock()
	if h == nil {
		return
	}
	h.KillChild()
	h.Finish("", session.StateCancelled)
}

// installSignalStop 注册「被终止时先带走引擎子进程」的处理。
//
// 为什么必须做：引擎 CLI 在**独立进程组**里（Setpgid），调用方对 magic-agent
// 发 SIGTERM / kill(-magic-agent-pid) 时，引擎子进程不在那一组里 → 会变成孤儿继续跑
// （观物台「停止」按钮踩的就是这个）。这里在退出前补一刀。
//
// 覆盖三处孤儿来源：
//   - 当前活动会话（stopActiveSession）；
//   - 交互式登录子进程（stopLoginChild）—— 它必须待在前台组里读 tty，因而
//     比普通引擎更「自己管不了」，只能在这里记一笔带走（见 login.go）；
//   - 关终端（SIGHUP）：与 SIGTERM 同路。
func installSignalStop() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-ch
		stopLoginChild()
		stopActiveSession()
		os.Exit(exitInterrupted)
	}()
}

// keepAlive 常驻会话的运行时状态（追加入口 + 空闲收工）。
//
// 数据流：`--append` 客户端 → unix socket → Appender.Serve → in → 转发（touch 活动时间）
// → appendCh → 引擎（写进 claude/codebuddy 的 stdin，成为下一轮 user 消息）。
// 收工：关闭 in → 转发收尾并关闭 appendCh → 引擎 stdin EOF → 子进程收尾退出。
type keepAlive struct {
	appender *session.Appender
	appendCh chan string // 引擎读这条
	in       chan string // Appender 写这条

	closeOnce sync.Once
	mu        sync.Mutex
	last      time.Time
	idle      time.Duration // 0 = 本轮结束就收工（追加窗口只在任务运行期间）
	closed    bool          // 收工后为 true：Push 据此拒收（与 close(in) 同锁，避免向已关闭通道发送）
}

// kaEnabled 常驻会话是否生效：只在「流式 + 引擎支持追加」时才有意义 —— 不满足就静默
// 不启用（不影响原有调用形态）。显式传 `--keep-alive` 却不满足条件的报错在 prepareAsk
// 里（那里能拿到 cmd 与引擎）。
//
// 默认值语义**按引擎区分**（agent.AppendDefaultOn）：claude/codebuddy 默认常驻
// （用户 2026-09-18 的要求）；dsh 默认关、要显式 `--keep-alive` —— 它的 SDK 通道本来
// 就是一次一轮的形态，默认挂 5 分钟空闲窗口会让既有调用方以为命令卡住了。
func kaEnabled(cmd *cobra.Command, opts *askOptions, engine string) bool {
	if !opts.stream || !agent.AppendSupportOf(engine) {
		return false
	}
	if flagChanged(cmd, "keep-alive") {
		return opts.keepAlive // 显式开关优先于引擎默认值
	}
	return agent.AppendDefaultOn(engine)
}

// startKeepAlive 启动常驻会话：校验能力 → 开追加入口（写进会话记录）→ 起转发与看门狗。
func startKeepAlive(cmd *cobra.Command, opts *askOptions, engine string, h *session.Handle) (*keepAlive, error) {
	if !agent.AppendSupportOf(engine) {
		return nil, &usageError{fmt.Errorf(
			"--keep-alive 暂不支持 %s 引擎（当前支持 claude、codebuddy、codebuddy-ai：靠 stream-json 输入持续收 user 消息；"+
				"dsh：靠 SDK 通道对同一会话继续 prompt）", engine)}
	}
	if h == nil {
		return nil, &usageError{fmt.Errorf("--keep-alive 需要会话登记表可用（检查目录：%s）", session.Dir())}
	}
	if opts.idle < 0 {
		return nil, &usageError{fmt.Errorf("--idle 不能为负（收到 %v；0 = 本轮结束就收工）", opts.idle)}
	}
	ap, err := session.ListenAppend(h)
	if err != nil {
		return nil, fmt.Errorf("--keep-alive: %w", err)
	}
	ka := &keepAlive{
		appender: ap,
		appendCh: make(chan string, 16),
		in:       make(chan string, 16),
		last:     time.Now(),
		idle:     opts.idle,
	}
	go ap.Serve(ka.in)
	go func() {
		for msg := range ka.in {
			ka.touch()
			ka.appendCh <- msg
		}
		close(ka.appendCh)
	}()
	if opts.idle > 0 {
		// 空闲看门狗：最后一轮结束后 idle 内没人追加 → 收工。
		tick := opts.idle / 5
		if tick < time.Second {
			tick = time.Second
		}
		go func() {
			t := time.NewTicker(tick)
			defer t.Stop()
			for range t.C {
				if time.Since(ka.activity()) >= opts.idle {
					ka.shutdown()
					return
				}
			}
		}()
	}
	// 提示只在「人看的场景（-o text）」或显式传了 --keep-alive 时打：
	// 默认常驻时保持 stderr 干净，机器调用方（-o json）不受影响。
	if strings.EqualFold(strings.TrimSpace(opts.output), "text") || flagChanged(cmd, "keep-alive") {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"magic-agent: 常驻会话已开启（run_id=%s）；可用 `magic-agent --append %s -p \"追加内容\"` 追加，空闲 %v 后自动收工\n",
			h.RunID(), h.RunID(), opts.idle)
	}
	return ka, nil
}

// wrapOnEvent 包一层 onEvent：任何引擎事件都算「有活动」，避免看门狗把在跑的一轮掐掉。
// idle == 0 时语义是「追加窗口只在任务运行期间」——所以**本轮一结束就收工**。
func (k *keepAlive) wrapOnEvent(next func(agent.StreamEvent)) func(agent.StreamEvent) {
	return func(ev agent.StreamEvent) {
		k.touch()
		if next != nil {
			next(ev)
		}
		if k.idle == 0 && ev.Kind == agent.KindTurnEnd {
			k.shutdown()
		}
	}
}

// touch 记录活动时间。
func (k *keepAlive) touch() {
	k.mu.Lock()
	k.last = time.Now()
	k.mu.Unlock()
}

// activity 最近一次活动时间。
func (k *keepAlive) activity() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.last
}

// shutdown 收工：关闭输入通道（幂等）→ 引擎 stdin 随之 EOF。
//
// ⚠️ closed 与 close(k.in) 必须在**同一把锁**里完成：Push 持锁发送，
// 否则「Push 判定未关闭 → shutdown 关通道 → Push 发送」会 panic（向已关闭通道发送）。
func (k *keepAlive) shutdown() {
	k.closeOnce.Do(func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.closed = true
		close(k.in)
	})
}

// Push 把一条 user 消息推进常驻会话（与 --append 走同一条入口）。
//
// 用途：`--control` 的 answer 命令 —— 审批 / 提问的答案只能作为**下一轮 user 消息**
// 喂给模型（headless 下原地作答拿不到答案，见 agent.KindAsk）。
// 返回 false = 常驻已收工或缓冲区已满；调用方据此回一条 control_error，不静默丢。
func (k *keepAlive) Push(msg string) bool {
	if k == nil || strings.TrimSpace(msg) == "" {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return false
	}
	select {
	case k.in <- msg:
		k.last = time.Now() // 与 touch() 同义；这里已持锁，不能再调 touch()
		return true
	default:
		return false
	}
}

// Close 关掉追加入口（幂等）。
func (k *keepAlive) Close() {
	if k == nil {
		return
	}
	k.shutdown()
	k.appender.Close()
}

// runAppend 向常驻会话追加一条消息（内容用 -p 或位置参数给）。
//
// 退出码：0 已投递；1 找不到会话 / 会话没有追加入口 / 常驻进程已退出；2 参数错。
func runAppend(cmd *cobra.Command, args []string, opts *askOptions) error {
	id := strings.TrimSpace(opts.appendTo)
	if id == "" {
		return &usageError{fmt.Errorf("--append 需要一个 session_id 或 run_id（--sessions 可列出全部）")}
	}
	text := strings.TrimSpace(opts.prompt)
	if text == "" {
		text = strings.TrimSpace(strings.Join(args, " "))
	}
	if text == "" {
		return &usageError{fmt.Errorf("--append 需要追加内容：用 -p \"...\" 或位置参数给")}
	}

	res, err := session.AppendMessage(id, text)
	if err != nil {
		return fmt.Errorf("--append %s: %w", id, err)
	}
	if opts.output == "text" {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "magic-agent: 已追加到会话 %s（%s）\n", pickSessionName(res.Record), res.Record.Engine)
		return nil
	}
	out := struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Queued    bool   `json:"queued"`
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id,omitempty"`
		Engine    string `json:"engine"`
		State     string `json:"state"`
		Reason    string `json:"reason,omitempty"`
	}{
		Type:      "append",
		ID:        id,
		Queued:    res.Queued,
		RunID:     res.Record.RunID,
		SessionID: res.Record.SessionID,
		Engine:    res.Record.Engine,
		State:     res.Record.State,
		Reason:    res.Reason,
	}
	return printJSON(cmd.OutOrStdout(), out)
}

// pickSessionName 记录的可读名字（优先会话 id）。
func pickSessionName(rec session.Record) string {
	if rec.SessionID != "" {
		return rec.SessionID
	}
	return rec.RunID
}

// runSessions 列出会话登记表（JSON 数组，新→旧）。
func runSessions(cmd *cobra.Command, opts *askOptions) error {
	records, err := session.List()
	if err != nil {
		return err
	}
	if records == nil {
		records = []session.Record{}
	}
	type row struct {
		RunID      string `json:"run_id"`
		SessionID  string `json:"session_id,omitempty"`
		PID        int    `json:"pid"`
		Engine     string `json:"engine"`
		Model      string `json:"model,omitempty"`
		State      string `json:"state"`
		Alive      bool   `json:"alive"`
		Workspace  string `json:"workspace,omitempty"`
		PromptHead string `json:"prompt_head,omitempty"`
		StartedAt  string `json:"started_at"`
		UpdatedAt  string `json:"updated_at"`
		// HasLog 这条会话有没有**可回放的事件日志**（`--session-log` 读的就是它）。
		// 为什么让 CLI 来说这件事：日志**只在流式调用时写**（见 sessionlog.go 的 sessionWriter），
		// 所以非流式跑出来的会话没有历史可回放 —— 客户端要拿它判断「这条点开有东西看吗」，
		// 而让客户端自己去拼日志路径就是把本仓库的内部布局当接口用。
		HasLog bool `json:"has_log"`
	}
	logDir, _ := sessionLogDir()
	rows := make([]row, 0, len(records))
	for _, r := range records {
		rows = append(rows, row{
			RunID:      r.RunID,
			SessionID:  r.SessionID,
			PID:        r.PID,
			Engine:     r.Engine,
			Model:      r.Model,
			State:      r.State,
			Alive:      r.State == session.StateRunning,
			Workspace:  r.Workspace,
			PromptHead: r.PromptHead,
			StartedAt:  r.StartedAt.Format("2006-01-02 15:04:05"),
			UpdatedAt:  r.UpdatedAt.Format("2006-01-02 15:04:05"),
			HasLog:     hasSessionLog(logDir, r.SessionID),
		})
	}
	return printJSON(cmd.OutOrStdout(), rows)
}

// hasSessionLog 这条记录有没有事件日志：日志按 `session_id` 命名，所以没有 id 的
// （一次性调用）自然没有；还要看文件在不在、非空 —— 空文件等于没有可回放的内容。
func hasSessionLog(dir, sessionID string) bool {
	if dir == "" || sessionID == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, sessionID+".jsonl"))
	return err == nil && st.Size() > 0
}

// runSessionLog 读一段会话的**事件历史**：`--session-log <id> [--after <seq>]`。
//
// 为什么这条命令必须存在：会话的「查询」原先只有 HTTP 插件能做（`/desk/session/{id}/messages`），
// 而 magic-agent **自己不跑服务** —— 调用方（掌天瓶等）是 exec 本 CLI 来拿数据的。
// 缺了它，调用方就只能自己去解析 <session_id>.jsonl 的磁盘格式，那是把内部格式当接口用。
//
// 输出形状与插件的同名接口**逐字段对齐**（全量 {session,events}；增量另带
// count / after / lastSeq / snapshotRequired），这样两条路可以互换，调用方只写一份解析。
//
// 退出码：0 = 读到了（没有事件也是 0，空历史不是错）；1 = 读失败；2 = 参数错（空 id）。
func runSessionLog(cmd *cobra.Command, opts *askOptions) error {
	id := strings.TrimSpace(opts.sessionLog)
	if id == "" {
		return &usageError{fmt.Errorf("--session-log 需要会话 id：传 session_id 或 run_id（--sessions 可列出全部）")}
	}
	dir, err := sessionLogDir()
	if err != nil {
		return fmt.Errorf("--session-log %s: 找不到历史目录: %w", id, err)
	}

	if flagChanged(cmd, "after") {
		evs, maxSeq, err := ReadSessionLogAfter(dir, id, opts.logAfter)
		if err != nil {
			return fmt.Errorf("--session-log %s: %w", id, err)
		}
		if evs == nil {
			evs = []sessionEvent{}
		}
		return printJSON(cmd.OutOrStdout(), struct {
			Session string         `json:"session"`
			Events  []sessionEvent `json:"events"`
			Count   int            `json:"count"`
			After   uint64         `json:"after"`
			LastSeq uint64         `json:"lastSeq"`
			// 调用方游标比磁盘还新 ⇒ 增量不可信，去拉全量。
			// ⚠️ 判据是**严格大于**：after == lastSeq 是「已经追平」，那是正常的空增量。
			SnapshotRequired bool `json:"snapshotRequired"`
		}{Session: id, Events: evs, Count: len(evs), After: opts.logAfter,
			LastSeq: maxSeq, SnapshotRequired: opts.logAfter > maxSeq})
	}

	evs, err := ReadSessionLog(dir, id)
	if err != nil {
		return fmt.Errorf("--session-log %s: %w", id, err)
	}
	if evs == nil {
		evs = []sessionEvent{}
	}
	return printJSON(cmd.OutOrStdout(), struct {
		Session string         `json:"session"`
		Events  []sessionEvent `json:"events"`
	}{Session: id, Events: evs})
}

// runStop 停止指定会话：session_id 或 run_id 都收。
//
// 退出码约定：
//
//	0  已停掉，或本来就没事（进程已不在 / 会话已结束）—— 幂等，重复点「停止」不算错
//	1  找不到该 id（多半是写错了；--sessions 可列全部）
//	2  参数错（空 id 等）
func runStop(cmd *cobra.Command, opts *askOptions) error {
	id := strings.TrimSpace(opts.stop)
	if id == "" {
		return &usageError{fmt.Errorf("--stop 需要一个 session_id 或 run_id（--sessions 可列出全部）")}
	}
	res, err := session.Stop(id)
	if err != nil {
		return fmt.Errorf("--stop %s: %w", id, err)
	}

	if opts.output == "text" {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", stopText(res))
		return nil
	}
	out := struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Stopped   bool   `json:"stopped"`
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id,omitempty"`
		PID       int    `json:"pid"`
		Engine    string `json:"engine"`
		State     string `json:"state"`
		Reason    string `json:"reason,omitempty"`
	}{
		Type:      "stop",
		ID:        id,
		Stopped:   res.Stopped,
		RunID:     res.Record.RunID,
		SessionID: res.Record.SessionID,
		PID:       res.Record.PID,
		Engine:    res.Record.Engine,
		State:     res.Record.State,
		Reason:    res.Reason,
	}
	return writeJSONLine(cmd.OutOrStdout(), out)
}

// stopText 人看的停止结果。
func stopText(res session.StopResult) string {
	who := res.Record.SessionID
	if who == "" {
		who = res.Record.RunID
	}
	if res.Stopped {
		return fmt.Sprintf("magic-agent: 已停止会话 %s（%s pid=%d）", who, res.Record.Engine, res.Record.PID)
	}
	reason := res.Reason
	if reason == "" {
		reason = "无需停止"
	}
	return fmt.Sprintf("magic-agent: 会话 %s 未在运行：%s（state=%s）", who, reason, res.Record.State)
}

// writeJSONLine 写一行 JSON（复用 printJSON 的编码方式，单独留个名便于阅读）。
func writeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
