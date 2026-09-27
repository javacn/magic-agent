package cli

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
)

/* stdin 控制通道（--control）：给流式会话一条**回传**通道。
 *
 * 为什么需要（2026-09-27）：`--stream` 此前是单向的 —— 调用方只能看，不能打断长任务、
 * 也不能回审批。而桌面 / 移动端最核心的两个动作恰好就是这两件（手机负责控制、
 * 工作设备负责执行）。
 *
 * 协议：stdin 一行一条 NDJSON 命令（与 stdout 的事件流同构，便于同一套解析器处理）。
 * 应答只在 `--events` 形态下输出（老形状不注入新事件类型）：
 *
 *   {"op":"ping"}                       → {"type":"pong"}
 *   {"op":"interrupt","reason":"..."}   → 打断当前轮；**等这一轮真的结束**再发
 *                                         {"type":"interrupted","reason":"..."}
 *   {"op":"stop"}                       → 收工（关闭常驻会话的追加入口）→ {"type":"stopped"}
 *   {"op":"answer","text":"..."}        → 把答案作为后续 user 消息追进常驻会话
 *                                         → {"type":"answered"}；通道不可用 →
 *                                           {"type":"control_error","error":"..."}
 *   其它                                 → {"type":"control_error","op":"..."}
 *
 * ⚠️ answer 为什么走「后续 user 消息」而不是原地作答：headless 下 claude 在模型提问后
 * 会**立刻自行拒绝**（见 agent.KindAsk 的注释），唯一能把答案真正喂给模型的通道就是
 * 下一轮 user 消息 —— 也就是常驻会话的追加通道，与 `--append` 是同一条路。
 * 因此 answer 只在常驻会话生效时可用；不可用时明确回一条 control_error，不静默丢弃。
 */

// controlCommand 一条控制命令。
type controlCommand struct {
	Op     string `json:"op"`
	Reason string `json:"reason,omitempty"`
	Text   string `json:"text,omitempty"`
}

// controlHooks 控制命令的落地动作。
type controlHooks struct {
	// interrupt 打断当前轮（取消 ctx → 引擎进程组随之被收拾）。
	interrupt func(reason string)
	// stop 收工：关掉常驻会话的追加入口，让引擎正常收尾。
	stop func()
	// answer 把答案追进常驻会话；返回 false 表示通道此刻不可用。
	answer func(text string) bool
}

// controlSession 一次控制通道的生命周期。
type controlSession struct {
	sink    *eventSink
	hooks   controlHooks
	stopFlg atomic.Bool
	intrFlg atomic.Bool
	closed  chan struct{}
	once    sync.Once
}

// startControlReader 起一个后台 goroutine 读 NDJSON 命令。调用方负责 Close。
func startControlReader(r io.Reader, sink *eventSink, hooks controlHooks) *controlSession {
	s := &controlSession{sink: sink, hooks: hooks, closed: make(chan struct{})}
	go s.loop(r)
	return s
}

func (s *controlSession) loop(r io.Reader) {
	sc := bufio.NewScanner(r)
	// 单条命令上限 4MiB：answer 可能带一段很长的说明（截图说明、报错粘贴）。
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		select {
		case <-s.closed:
			return
		default:
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c controlCommand
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			_ = s.sink.notice("control_error", map[string]any{"error": "不是合法 NDJSON 命令：" + err.Error()})
			continue
		}
		s.dispatch(c)
	}
}

// dispatch 执行一条命令。未知 op 只回一条 control_error，**不中断读取** ——
// 一个拼错的 op 不该让整条控制通道失效。
func (s *controlSession) dispatch(c controlCommand) {
	switch strings.ToLower(strings.TrimSpace(c.Op)) {
	case "ping":
		_ = s.sink.notice("pong", nil)

	case "interrupt":
		// 不在这里回应答：interrupt 的真实含义是「这一轮真的结束了」，
		// 由 runStreamAsk 在 Stream 返回后发 interrupted。
		s.intrFlg.Store(true)
		if s.hooks.interrupt != nil {
			s.hooks.interrupt(c.Reason)
		}

	case "stop":
		s.stopFlg.Store(true)
		if s.hooks.stop != nil {
			s.hooks.stop()
		}
		_ = s.sink.notice("stopped", nil)

	case "answer":
		text := strings.TrimSpace(c.Text)
		if text == "" {
			_ = s.sink.notice("control_error", map[string]any{"op": "answer", "error": "answer 需要 text 字段"})
			return
		}
		if s.hooks.answer == nil || !s.hooks.answer(text) {
			_ = s.sink.notice("control_error", map[string]any{
				"op": "answer",
				"error": "常驻会话未生效，answer 无处可去。" +
					"答案要走常驻会话的追加通道（headless 下模型拿不到原地作答）；" +
					"请确认引擎支持 append（见 --contract 的 capabilities）且 --keep-alive 生效",
			})
			return
		}
		_ = s.sink.notice("answered", nil)

	default:
		_ = s.sink.notice("control_error", map[string]any{
			"op":    c.Op,
			"error": "未知 op：支持 ping / interrupt / stop / answer",
		})
	}
}

// WasStopped 收到过 stop。
func (s *controlSession) WasStopped() bool { return s.stopFlg.Load() }

// WasInterrupted 收到过 interrupt（应答由调用方在本轮结束时发出）。
func (s *controlSession) WasInterrupted() bool { return s.intrFlg.Load() }

// Close 停止读取（幂等）。已经在阻塞读 stdin 的 goroutine 会在下一次 Scan 返回后退出。
func (s *controlSession) Close() {
	s.once.Do(func() { close(s.closed) })
}
