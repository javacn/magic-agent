package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/darren/magic-agent/internal/agent"
)

/* 事件契约 v1（客户端消费形态）：给流式事件加版本与游标。
 *
 * 为什么需要（2026-09-27）：`--stream` 的 NDJSON 事件流此前没有版本号、也没有
 * 序号，客户端（magic-client 桌面 / 移动插件）因此回答不了两个必须回答的问题：
 *   1) 这份输出我能不能解析？—— 字段改名时老客户端会静默解析失败，界面上表现为空白；
 *   2) 我收到的是否连续、有没有漏行？—— 断线重连要按游标续读。
 * 于是 `--events` 在**不改老形状**的前提下补三样：
 *   v            契约版本，与 `--contract` 的 contractVersion 同一个数
 *   seq          单次进程内单调递增的行号（从 1 开始）
 *   session_id   一旦拿到就每行都带（老形状只在 turn_end / result 带）
 * 沿用老形状已有的约定：**每轮一定以 result 或 error 收尾**。
 *
 * ⚠️ `--events` 与 `--stream` 的输出**形状不同**，别混用：
 *   老消费者继续用 `--stream`（逐字节不变）；新客户端用 `--stream --events`。
 *   两条路径共用 streamEventPayload()，靠测试钉住「字段集只差 v / seq」。
 */

// eventSink 事件出口。enabled=false 时输出与历史形状逐字节一致。
type eventSink struct {
	w       io.Writer
	engine  string
	enabled bool

	mu      sync.Mutex
	seq     int
	session string
}

func newEventSink(w io.Writer, engine string, enabled bool) *eventSink {
	return &eventSink{w: w, engine: engine, enabled: enabled}
}

// Enabled 是否处于 `--events` 形态。控制通道的应答只在它下面输出（老形状不认新事件类型）。
func (s *eventSink) Enabled() bool { return s.enabled }

// setSession 记住会话 id（拿到之后后续每行都带）。
func (s *eventSink) setSession(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	s.session = id
	s.mu.Unlock()
}

// nextSeq 取下一个行号（调用方不要再持锁调它）。
func (s *eventSink) nextSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq
}

// sessionID 当前已知的会话 id。
func (s *eventSink) sessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

func (s *eventSink) writeLine(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := fmt.Fprintln(s.w, string(b))
	return err
}

// event 输出一行事件。
//
// 老形态（未开 --events）：payload 原样一行，与历史输出逐字节一致。
// 新形态：payload 的字段集不变，外层补 v / seq（以及已知的 session_id）。
// 只在新形态下走一次「marshal → 加字段 → marshal」的往返 —— 它是客户端路径，
// 每行多一次解析换「两个形状永不漂」是划算的。
func (s *eventSink) event(payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if !s.enabled {
		return s.writeLine(b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	return s.eventAttrs(m)
}

// eventAttrs 输出一行带 envelope 的新形态事件（attrs 至少要有 type）。
func (s *eventSink) eventAttrs(attrs map[string]any) error {
	if !s.enabled {
		return nil
	}
	seq := s.nextSeq()
	env := make(map[string]any, len(attrs)+3)
	for k, v := range attrs {
		env[k] = v
	}
	env["v"] = agent.ContractVersion
	env["seq"] = seq
	if sess := s.sessionID(); sess != "" {
		if _, ok := env["session_id"]; !ok {
			env["session_id"] = sess
		}
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return s.writeLine(b)
}

// stream 输出一条引擎流式事件。
//
// 未开 --events 时直接走老写入函数（逐字节不变）；开了才补 v / seq。
// 两条路径共用 streamEventPayload()，字段集因此不可能漂。
func (s *eventSink) stream(ev agent.StreamEvent) error {
	s.setSession(ev.SessionID)
	if !s.enabled {
		return writeStreamEventJSON(s.w, ev)
	}
	return s.event(streamEventPayload(ev))
}

// ready 握手行：新形态的第一行，告诉客户端「谁在跑、契约版本是多少」。
// 只在 `--events` 下输出 —— 老消费者不认识它。
func (s *eventSink) ready(model string) error {
	if !s.enabled {
		return nil
	}
	attrs := map[string]any{"type": "ready", "engine": s.engine}
	if model != "" {
		attrs["model"] = model
	}
	return s.eventAttrs(attrs)
}

// notice 输出一条 core 自己产生的事件（不是引擎增量）：
// pong / interrupted / stopped / answered / answer_queued / control_error。
// 只在 `--events` 下输出，老形状不注入新类型。
func (s *eventSink) notice(kind string, attrs map[string]any) error {
	if !s.enabled {
		return nil
	}
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrs["type"] = kind
	return s.eventAttrs(attrs)
}

// streamEventPayload 流式事件的 wire 形状 —— `--stream` 与 `--stream --events` 共用。
// 两条边的字段集必须只差 v / seq，test 里有专门的防漂断言。
//
// ⚠️ 状态收敛字段（item_id / item_revision / status / snapshot / error / reason）
// 只在**非空时**出现（全部 omitempty）—— 老引擎（trae 走 delta 但无 item 概念）、
// 老事件（turn_end 之外的轮次事件）不带这些字段时与历史逐字节一致。
func streamEventPayload(ev agent.StreamEvent) any {
	return struct {
		Type      string            `json:"type"`
		Text      string            `json:"text"`
		Name      string            `json:"name,omitempty"`
		ID        string            `json:"id,omitempty"`
		ToolKind  string            `json:"tool_kind,omitempty"`
		SessionID string            `json:"session_id,omitempty"`
		Ask       *agent.AskRequest `json:"ask,omitempty"`
		/* 状态收敛四元组（2026-10-02，对齐 agents-anywhere 的 timeline item）。
		   消费方按 (item_id, item_revision) 覆盖式 upsert 即可：
		   同一 item 的多个 partial 帧 + 最后一个 final 帧，
		   乱序到达也能收敛到正确结果（重复投递同 revision 天然幂等）。 */
		ItemID       string `json:"item_id,omitempty"`
		ItemRevision uint64 `json:"item_revision,omitempty"`
		Status       string `json:"status,omitempty"`
		Snapshot     string `json:"snapshot,omitempty"`
		/* 异常收尾（仅 turn_failed）：Error 是完整错误链，Reason 是最内层根因，
		   与 -o json 的失败 envelope 同源（都走 agent.ReasonOf）。 */
		Error  string `json:"error,omitempty"`
		Reason string `json:"reason,omitempty"`
	}{
		Type:         string(ev.Kind),
		Text:         ev.Text,
		Name:         ev.Name,
		ID:           ev.ID,
		ToolKind:     toolKindOfEvent(ev),
		SessionID:    ev.SessionID,
		Ask:          ev.Ask,
		ItemID:       ev.ItemID,
		ItemRevision: ev.ItemRevision,
		Status:       string(ev.Status),
		Snapshot:     ev.Snapshot,
		Error:        ev.Error,
		Reason:       ev.Reason,
	}
}

/* toolKindOfEvent 事件携带的工具子形态（2026-09-29 新增，见 agent/toolkind.go）。
 *
 * 只有**工具调用**事件带它：渲染层据此选卡（命令 / 文件变更 / 网页检索 / MCP /
 * 子代理 / 提问 / 通用），不再靠工具名猜 —— 一处判定、两个界面共用。
 *
 * ⚠️ 工具**结果**事件不给：它的 Name 字段放的是「关联的 tool_use.id」（见 StreamEvent
 * 的注释），拿它去判形态只会把 id 当成工具名。结果的形态由它配上的那张卡承担。
 * ⚠️ 非工具事件一律空串 → `omitempty` 把它从 wire 上省掉，老消费者的字段集不受影响。 */
func toolKindOfEvent(ev agent.StreamEvent) string {
	if ev.Kind != agent.KindToolUse {
		return ""
	}
	return agent.ToolKindFor(ev.Name, ev.Text)
}
