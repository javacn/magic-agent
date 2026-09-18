// Package session - 会话登记表：让「停止指定会话」成为可能。
//
// 用户需求（2026-09-18）：「要支持停止指定会话」。
//
// 为什么需要一张登记表：magic-agent 把引擎 CLI 放在**独立进程组**里跑
// （Setpgid，见 agent/runcmd.go），所以它和 magic-agent 自己不在同一组 ——
// 调用方（观物台等）`kill(-magic-agent-pid)` 带不走引擎子进程，会出现
// 「界面已停止、引擎还在跑」。更麻烦的是进程句柄只活在调用方内存里：
// 应用重启后，之前 detach 出去的会话就再也停不掉了。
//
// 于是每次调用都落一条记录（引擎子进程 pid + 引擎/模型/工作目录 + 会话 id），
// 之后任何进程都能用 `magic-agent --stop <session_id|run_id>` 精确停掉它。
//
// 文件位置：$MAGIC_AGENT_SESSIONS > ~/.magic-agent/sessions/<run_id>.json
// 每条一个文件（无锁、无共享写），过期记录（默认 24h）在 Begin/List/Stop 时顺手清理。
//
// 记录里 session_id 可能一开始不知道：引擎的会话 id 只在**收尾行**里给
// （见 magic-agent 的 result envelope），所以：
//
//	开始时就已知（--session <id> 续接）→ 直接写进记录
//	开始时未知（新会话）              → 结束时回填（Finish），期间用 run_id 寻址
//
// 因此 --stop 接受两种 id：会话 id（session_id）或运行 id（run_id，`--sessions` 可见）。
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/darren/magic-agent/internal/agent"
)

// EnvDir 覆盖登记表目录的环境变量名。
const EnvDir = "MAGIC_AGENT_SESSIONS"

// Retention 记录保留时长：过期的（按 UpdatedAt）在每次读写时清理。
// 留着而不是用完即删，是为了让「停一条早就结束的会话」能答出「已结束」而不是「没找到」。
const Retention = 24 * time.Hour

// 记录状态。
const (
	StateRunning = "running" // 正在跑
	StateDone    = "done"    // 正常结束
	StateFailed  = "failed"  // 报错结束
	StateStopped = "stopped" // 被 --stop 停掉
	StateGone    = "gone"    // 记录里是 running，但进程已不在（自然退出/被别处杀掉）
)

// Record 一条会话记录（也是磁盘上的 JSON 形状）。
type Record struct {
	RunID      string    `json:"run_id"`
	SessionID  string    `json:"session_id,omitempty"`
	PID        int       `json:"pid"`
	KillGroup  bool      `json:"kill_group"` // true = 按进程组杀（引擎子进程自成组）
	Engine     string    `json:"engine"`
	Model      string    `json:"model,omitempty"`
	Workspace  string    `json:"workspace,omitempty"`
	PromptHead string    `json:"prompt_head,omitempty"`
	State      string    `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// Append 追加入口的 unix socket 路径（常驻会话 / --keep-alive 才有）。
	// 空 = 该会话不可追加（--append 会明确告知原因，不静默）。
	Append string `json:"append,omitempty"`
}

// Dir 返回登记表目录（$MAGIC_AGENT_SESSIONS 优先）。
func Dir() string {
	if p := strings.TrimSpace(os.Getenv(EnvDir)); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".magic-agent", "sessions")
}

// BeginOptions 开始一条会话记录所需的输入。
type BeginOptions struct {
	Engine     string
	Model      string
	Workspace  string
	SessionID  string // 续接时已知；新会话为空
	PromptHead string // 提示词开头（便于人辨认）
	PID        int    // 可先填自身 pid 兜底（引擎子进程起来后会被 SetPID 覆盖）
}

// Handle 一条运行中会话的句柄（负责回写 pid 与收尾状态）。
type Handle struct {
	rec  Record
	path string
}

// RunID 本次运行的 id。
func (h *Handle) RunID() string { return h.rec.RunID }

// Record 当前记录快照。
func (h *Handle) Record() Record { return h.rec }

// SetPID 记录引擎子进程 pid（由 agent.WithSpawnHook 在 Start 之后回调）。
// group=true 表示该 pid 自成进程组（引擎 CLI 的正常情况）。
func (h *Handle) SetPID(pid int, group bool) {
	if h == nil || pid <= 0 {
		return
	}
	h.rec.PID = pid
	h.rec.KillGroup = group
	h.write()
}

// SetSessionID 回填会话 id（续接时开始就知道；新会话在收尾时才知道）。
func (h *Handle) SetSessionID(id string) {
	if h == nil || strings.TrimSpace(id) == "" || h.rec.SessionID == id {
		return
	}
	h.rec.SessionID = strings.TrimSpace(id)
	h.write()
}

// Finish 收尾：写最终状态（done / failed / stopped）+ 回填会话 id。
//
// 一个刻意的小保护：被别人 `--stop` 停掉时，磁盘上已经是 stopped；本进程随后
// 因被杀而以 failed 收尾 —— 不能把 stopped 覆盖成 failed，否则「是谁停的」就丢了。
func (h *Handle) Finish(sessionID, state string) {
	if h == nil {
		return
	}
	if state == StateFailed {
		if cur, err := readRecord(h.path); err == nil && cur.State == StateStopped {
			state = StateStopped
		}
	}
	if s := strings.TrimSpace(sessionID); s != "" {
		h.rec.SessionID = s
	}
	if state != "" {
		h.rec.State = state
	}
	h.write()
}

// KillChild 立即杀掉本条会话的进程（进程组 / 单进程），用于信号处理路径。
func (h *Handle) KillChild() {
	if h == nil || h.rec.PID <= 0 {
		return
	}
	_ = agent.KillPID(h.rec.PID, h.rec.KillGroup)
}

// write 落盘（失败静默：登记表只是可运维性，不能因为它写不动就让调用失败）。
func (h *Handle) write() {
	h.rec.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(h.rec, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(h.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	// 先写临时文件再改名：避免读到写了一半的 JSON。
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, h.path)
}

// Begin 新建一条 running 记录。
func Begin(o BeginOptions) (*Handle, error) {
	dir := Dir()
	if dir == "" {
		return nil, fmt.Errorf("cannot resolve sessions dir")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	prune(dir)

	now := time.Now()
	rec := Record{
		// run_id 用 base36 压短：它同时决定 socket 文件名，而 unix socket 路径有
		// 硬上限（macOS sun_path 104 字节），长 id + 长 HOME 会直接 bind 失败。
		RunID:      "run-" + strconv.FormatInt(now.UnixNano(), 36) + "-" + strconv.FormatInt(int64(os.Getpid()), 36),
		SessionID:  strings.TrimSpace(o.SessionID),
		PID:        o.PID,
		KillGroup:  false, // 自身 pid 兜底时按单进程杀（不能杀调用方的进程组）
		Engine:     o.Engine,
		Model:      o.Model,
		Workspace:  o.Workspace,
		PromptHead: head(o.PromptHead, 80),
		State:      StateRunning,
		StartedAt:  now,
		UpdatedAt:  now,
	}
	h := &Handle{rec: rec, path: filepath.Join(dir, rec.RunID+".json")}
	h.write()
	return h, nil
}

// head 截取开头 n 个字符（按 rune，避免把中文切坏）。
func head(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// List 返回全部记录（新的在前）；顺带清理过期记录。
func List() ([]Record, error) {
	dir := Dir()
	if dir == "" {
		return nil, fmt.Errorf("cannot resolve sessions dir")
	}
	prune(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Record, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if rec, err := readRecord(filepath.Join(dir, e.Name())); err == nil {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// Find 按 run_id 或 session_id 精确匹配一条记录。
// 同 id 有多条时优先「running」的那条，其次取最新。
func Find(id string) (Record, bool, error) {
	want := strings.TrimSpace(id)
	if want == "" {
		return Record{}, false, fmt.Errorf("empty session id")
	}
	all, err := List()
	if err != nil {
		return Record{}, false, err
	}
	var best Record
	found := false
	for _, r := range all {
		if r.RunID != want && r.SessionID != want {
			continue
		}
		switch {
		case !found:
			best, found = r, true
		case r.State == StateRunning && best.State != StateRunning:
			best = r
		case r.State == best.State && r.StartedAt.After(best.StartedAt):
			best = r
		}
	}
	return best, found, nil
}

// StopResult 停止一条会话的结果。
type StopResult struct {
	Record  Record `json:"record"`
	Stopped bool   `json:"stopped"` // 是否真的发出了终止并确认进程消失
	Reason  string `json:"reason,omitempty"`
}

// Stop 停止指定会话：先 SIGTERM，宽限期内不退再 SIGKILL，最后回写状态。
//
// 幂等：进程已经不在了 → Stopped=false + reason（不报错），并把状态改成 gone/stopped。
// 找不到 id → 返回错误（多半是 id 写错了，要让调用方知道）。
func Stop(id string) (StopResult, error) {
	rec, found, err := Find(id)
	if err != nil {
		return StopResult{}, err
	}
	if !found {
		return StopResult{}, fmt.Errorf("no such session %q（--sessions 可列出全部）", id)
	}

	if rec.State != StateRunning {
		return StopResult{Record: rec, Reason: "会话已结束（state=" + rec.State + "）"}, nil
	}
	if rec.PID <= 0 || !agent.PIDAlive(rec.PID) {
		rec.State = StateGone
		rec.UpdatedAt = time.Now()
		writeRecord(rec)
		return StopResult{Record: rec, Reason: "进程已不在（可能已自然结束）"}, nil
	}

	_ = agent.TerminatePID(rec.PID, rec.KillGroup)
	if !waitGone(rec.PID, 2*time.Second) {
		_ = agent.KillPID(rec.PID, rec.KillGroup)
		if !waitGone(rec.PID, 2*time.Second) {
			return StopResult{Record: rec, Reason: "已发信号但进程仍在（可能不属于当前用户）"},
				fmt.Errorf("session %q (pid %d) 未能终止", id, rec.PID)
		}
	}
	rec.State = StateStopped
	rec.UpdatedAt = time.Now()
	writeRecord(rec)
	return StopResult{Record: rec, Stopped: true}, nil
}

// waitGone 轮询等进程消失（每 50ms 一次）。
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !agent.PIDAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readRecord 读一条记录（坏文件当作不存在）。
func readRecord(path string) (Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// writeRecord 覆盖写一条记录（按 RunID 定位文件名）。
func writeRecord(rec Record) {
	dir := Dir()
	if dir == "" || rec.RunID == "" {
		return
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, rec.RunID+".json"), data, 0o600)
}

// prune 删除过期记录（按 UpdatedAt；读不动的坏文件一并清掉）。
func prune(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-Retention)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		rec, err := readRecord(p)
		if err != nil || rec.UpdatedAt.Before(cutoff) {
			_ = os.Remove(p)
		}
	}
}
