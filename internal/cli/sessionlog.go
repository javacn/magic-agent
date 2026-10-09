package cli

// sessionlog.go - 把流式会话的每条事件落盘到 <dir>/<session_id>.jsonl。
//
// 目的：把会话从「一次性消费流」升级为「可重读」。
// 之前 magic-agent 的会话登记表（~/.magic-agent/sessions/run-*.json）只记元数据
// （提示词首行 / 状态 / 时间），不存消息内容 —— 移动端没法看历史。这次落地之后：
//   · 流式消费者同时收到 NDJSON（实时）与 jsonl 文件（持久化）
//   · 移动端进入会话详情时先拉历史 → 已经有内容可渲染；再开 SSE 接续
//
// 三条不能违反的约束：
//   1. **绝不能断流**：写盘失败、慢、磁盘满 —— 只打一行 warning，不抛错、不阻塞 onEvent。
//      否则一次 EIO 就把流式变成报错（结果链就是 magic-agent 退 = 插件 SSE 收尾 = 用户界面卡死）。
//   2. **最多一次重发**：每条事件写一次；不重试，不重读。不重试是因为重写会污染 seq 序列
//      （历史接口靠 seq 游标），且消息内容重复展示 = 体验差。
//   3. **session_id 未知前也能暂存**：不同引擎在流的不同位置暴露 session_id（init / assistant / result），
//      而最早的 thinking / tool_use 事件可能先到。开文件必须 lazy —— 拿到 id 再开。
//
// 落盘动作走 goroutine + buffered channel：调用方 (onEvent) 只做「塞进 channel 即返回」，
// IO 慢的副作用不会传到流路径。

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darren/magic-agent/internal/agent"
	"github.com/darren/magic-agent/internal/session"
)

// SessionLogDirOverride 给测试用：override 默认的 ~/.magic-agent/sessions/。
var SessionLogDirOverride string

// sessionLogDir 返回本会话日志的目录路径：
//   - 显式 override（SessionLogDirOverride）优先；
//   - 默认是 ~/.magic-agent/sessions（与 session 登记表同一目录）。
func sessionLogDir() (string, error) {
	if SessionLogDirOverride != "" {
		return SessionLogDirOverride, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("找不到家目录：%w", err)
	}
	return filepath.Join(home, ".magic-agent", "sessions"), nil
}

// sessionEvent 是写盘格式：每条一行 JSON、自带 seq。
// seq 用来做"上一页"（cursor）—— anywhere 那条 timeline 也是这么干的。
//
// Kind 取自两处，合并成一份词表：
//   - 引擎事件（internal/agent/stream.go 的 StreamEventKind）：thinking / text /
//     tool_use / tool_result / ask / turn_end；
//   - core 自己补的：user（用户提问，见 ask.go runStreamAsk —— 引擎事件里没有它，
//     但历史缺了它就只剩 agent 单方面说话）、error（失败兜底）。
//
// 注意：Thinking 文本可能很大（claude 长思考），Text 可能有工具调用摘要（tool_use），
// 都直接 JSON 序列化。文件按行追加，断电最坏丢最后一行（不是大问题，流仍在跑）。
type sessionEvent struct {
	Seq  uint64 `json:"seq"`
	At   string `json:"at"`
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
	// ToolKind 工具调用的**子形态**（command / file_change / web_search / mcp /
	// agent_call / input_request / permission / tool_call），只有 tool_use 事件有，
	// 与 SSE 流同源（见 agent/toolkind.go 与 events.go 的 toolKindOfEvent）。
	// 有了它，历史回放和实时渲染才会画出同一种卡 —— 否则回放那侧只能按名字猜。
	ToolKind  string            `json:"tool_kind,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Ask       *agent.AskRequest `json:"ask,omitempty"`
	/* 状态收敛四元组（2026-10-02）。落盘它们是为了让**历史回放与实时渲染
	   画出同一种东西**：回放侧不必自己猜「这条消息到哪算完」，
	   按 (item_id, item_revision) 覆盖即可。缺了它们，历史里每条 text
	   都只是孤立的增量片段（与实时流同源但无法收敛）。 */
	ItemID       string `json:"item_id,omitempty"`
	ItemRevision uint64 `json:"item_revision,omitempty"`
	Status       string `json:"status,omitempty"`
	Snapshot     string `json:"snapshot,omitempty"`
	// Error / Reason 异常收尾的原因（仅 turn_failed），与事件流同源。
	Error  string `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// sessionWriter 每条流式调用一个实例；开着就一直 append 到
// `<dir>/<session_id>.jsonl`，session_id 还没出来时事件先**丢进 buffer**等 id。
//
// ⚠️ **不重发**：buffer 等 id 时最多暂存 ~64 条事件；满了就丢（按 round-robin），
// 并 emit 一行 warning。我们接受这个损失 —— 历史只对「点进去看历史」这件事有用，
// 极少数早期事件丢了用户察觉不到（call 号还在 SSE 流里）；但「突然不写盘」绝对不行。
type sessionWriter struct {
	dir      string
	runID    uint64
	stop     chan struct{}
	done     chan struct{}
	seq      atomic.Uint64
	in       chan sessionEvent
	mu       sync.Mutex
	pending  []sessionEvent
	file     *os.File
	w        *bufio.Writer
	filePath string
	overflow atomic.Uint64 // 被 ring-buffer 丢弃的事件计数（诊断用）

	// sessionID 跨 goroutine 共享：CLI 的 OnSessionID 写入（流路径），
	// sw.write 读（写盘 goroutine）。atomic.Pointer[string] 保证可见性。
	sessionID atomic.Pointer[string]
}

// newSessionWriter 创建并启动一个 sessionWriter（仅包内测试用，单测不依赖 onSessionID）。
func newSessionWriter(dir string, runID uint64) *sessionWriter {
	sw := &sessionWriter{
		dir:   dir,
		runID: runID,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
		in:    make(chan sessionEvent, 128),
	}
	initID := ""
	sw.sessionID.Store(&initID)
	go sw.loop()
	return sw
}

// setSessionID 由 CLI 端 OnSessionID 调用 —— 流里见到 session_id 就推给 writer。
func (sw *sessionWriter) setSessionID(id string) {
	s := id
	sw.sessionID.Store(&s)
}

// currentSessionID 在 sw.write 里读；不可与 OnSessionID 配对交换字段（race）。
func (sw *sessionWriter) currentSessionID() string {
	return *sw.sessionID.Load()
}

// newSessionWriterAuto 解析目录 + 起 writer。
// 解析失败：返回 nil + error，让调用方决定「不写盘继续」还是「中断流」。
func newSessionWriterAuto(dirFn func() (string, error)) (*sessionWriter, error) {
	dir, err := dirFn()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var counter atomic.Uint64
	runID := counter.Add(1)
	sw := newSessionWriter(dir, runID)
	return sw, nil
}

// push 是 onEvent 的最后一行：把一条事件塞进 channel；通道满了就丢 + 计数。
// 返回 nil；不出错（**绝不能断流**）。
func (sw *sessionWriter) push(ev sessionEvent) {
	select {
	case sw.in <- ev:
	default:
		// 满了：写盘跟不上。先按 ring-buffer 的思路存到 pending；如果 pending 也满就丢。
		sw.mu.Lock()
		if len(sw.pending) < 64 {
			sw.pending = append(sw.pending, ev)
		} else {
			sw.overflow.Add(1)
		}
		sw.mu.Unlock()
	}
}

// close 让 writer 把 pending + channel 全部 flush 后退出。
// 多次调用安全；后续的 push 仍然能被丢弃（不应发生：调用方应该在 cmd.Wait 之后才结束）。
func (sw *sessionWriter) close() {
	select {
	case <-sw.stop:
		// already closed
	default:
		close(sw.stop)
	}
	<-sw.done
}

// loop 是单 goroutine 的 IO 循环 —— 拉事件、拿会话 id、写盘、刷文件。
//
// 顺序的简化：把「拿 session_id」和「打开文件」放在事件处理里。第一条事件没 id
// → 进 pending；有 id 之后**只**开一次文件并把 pending 也写进去。之后每条事件
// 直接落盘，不再有"没 id"的问题。
func (sw *sessionWriter) loop() {
	defer close(sw.done)
	defer func() {
		if sw.w != nil {
			_ = sw.w.Flush()
			_ = sw.file.Close()
		}
	}()
	for {
		select {
		case <-sw.stop:
			// drain：把 in 里剩的都写出去（直到通道空）。
			for {
				select {
				case ev := <-sw.in:
					sw.write(ev)
				default:
					return
				}
			}
		case ev := <-sw.in:
			sw.write(ev)
		}
	}
}

// write 是单条事件的落盘。id 还没出现就暂存；id 出现后开文件并把暂存的也写进去。
func (sw *sessionWriter) write(ev sessionEvent) {
	// 1. 拿到当前 session_id（来自 accumulator；流早期可能为空）。
	if ev.SessionID == "" {
		ev.SessionID = sw.currentSessionID()
	}
	if ev.SessionID == "" {
		// 还没 id：进 pending，等后续事件触发「打开文件」的动作。
		sw.mu.Lock()
		if len(sw.pending) < 64 {
			sw.pending = append(sw.pending, ev)
		} else {
			sw.overflow.Add(1)
		}
		sw.mu.Unlock()
		return
	}
	// 2. 还没开文件：开 + 刷 pending。文件路径 = dir/<session_id>.jsonl。
	if sw.file == nil {
		if err := sw.openFile(ev.SessionID); err != nil {
			// 开不开得出来（磁盘满 / 权限）：本条丢弃，**不抛错**。下一条再来还试。
			fmt.Fprintf(os.Stderr, "magic-agent: 持久化会话失败（%s）：%v（流继续）\n", ev.SessionID, err)
			return
		}
		// pending 也写进去（首次不丢早期事件）。
		sw.mu.Lock()
		pend := sw.pending
		sw.pending = nil
		sw.mu.Unlock()
		for _, pev := range pend {
			sw.appendLine(pev)
		}
	}
	sw.appendLine(ev)
}

// openFile 在 <dir> 下创建/追加 <session_id>.jsonl。权限 0600（与 session 登记表一致）。
//
// ⚠️ 用 append 模式 + 0600：不截断已有文件（续接同一会话不会重复第一条事件），
// 权限只有同用户能读 —— 跨用户读自己用 read-only 调用就好了。
func (sw *sessionWriter) openFile(id string) error {
	sw.filePath = filepath.Join(sw.dir, id+".jsonl")
	f, err := os.OpenFile(sw.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	sw.file = f
	sw.w = bufio.NewWriterSize(f, 32*1024) // 32KB 缓冲；写完一行 Flush，由 timer 兜底
	return nil
}

// appendLine 写一行 JSON。错（写满 / 设备断开）只 log、不 panic、不影响流路径。
func (sw *sessionWriter) appendLine(ev sessionEvent) {
	ev.Seq = sw.seq.Add(1)
	if ev.At == "" {
		ev.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if ev.SessionID == "" {
		ev.SessionID = sw.currentSessionID()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintf(os.Stderr, "magic-agent: 会话事件序列化失败：%v（流继续）\n", err)
		return
	}
	b = append(b, '\n')
	if _, err := sw.w.Write(b); err != nil {
		fmt.Fprintf(os.Stderr, "magic-agent: 写会话失败：%v（流继续）\n", err)
		return
	}
	// 不每行都 Flush：bufio 攒一批，close 时一次性刷。代价：断电最坏丢一批；
	// 流路径完全不受影响。disk 也减少 90%+ 的 fsync。
}

// sessionEventFromJSON 把 jsonl 的一行解析成 sessionEvent。
// 与 session.ReadSessionLog 配对使用：CLI 知道自己的 schema，调用方把"怎么解析"
// 当成回调传过去，避免 session 包反向依赖 cli。
func sessionEventFromJSON(line []byte) (sessionEvent, error) {
	var ev sessionEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return sessionEvent{}, err
	}
	return ev, nil
}

// ReadSessionLog 读整个 jsonl 到 []sessionEvent，调用方拿去做 UI 渲染或 diff。
//
// 顺序：seq 升序；空文件返回 nil。损坏行跳过（warn 一行）—— 历史不能 100% 保证，
// 但绝大多数行都是单行 JSON + 换行结尾，单条坏掉的概率极低。
func ReadSessionLog(dir, sessionID string) ([]sessionEvent, error) {
	if dir == "" {
		var err error
		dir, err = sessionLogDir()
		if err != nil {
			return nil, err
		}
	}
	return session.ReadSessionLog(dir, sessionID, sessionEventFromJSON)
}

// ReadSessionLogAfter 增量读：只取 seq > after 的事件，并回磁盘上的最大 seq。
//
// 存在的理由与全量读不同：断线重连 / 长会话续看时不必重传整段历史（差一个数量级）。
// dir 传空 = 用默认历史目录（与 ReadSessionLog 同一口径）。
func ReadSessionLogAfter(dir, sessionID string, after uint64) ([]sessionEvent, uint64, error) {
	if dir == "" {
		var err error
		dir, err = sessionLogDir()
		if err != nil {
			return nil, 0, err
		}
	}
	return session.ReadSessionLogAfter(dir, sessionID, after,
		func(e sessionEvent) uint64 { return e.Seq }, sessionEventFromJSON)
}

// errClosed 表示 writer 已关。push 收到这个就吞掉 —— 不该发生，但保险。
var errClosed = errors.New("session writer closed")
