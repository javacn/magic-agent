package agent

// stream.go - 流式调用抽象（含思考过程与工具调用）。
//
// Streamer 是 Engine 的流式形态：CLI 以 stream-json 逐行输出，
// magic-agent 实时解析并把四类增量转发给回调：
//
//	KindThinking   — 模型思考过程（reasoning / thinking delta）
//	KindText       — 正文增量
//	KindToolUse    — 模型发起工具调用（name + args；args 在收尾一次性发出，
//	                 由 input_json_delta 累积而成，最稳定）
//	KindToolResult — 工具调用产出（id 关联回 tool_use）
//
// 流式模式语义与 Complete 不同，刻意不共享 Runner：
//	- 超时仍然生效（每次尝试独立进程组，超时杀整组）；
//	- 不做自动重试——增量已实时发往 stdout，重放会造成重复消费；
//	  需要重试语义的调用方用非流式 Run。
//
// 三家 CLI 的流式协议（2026-09-15 实测）：
//
//	claude / codebuddy（同源 CodeBuddy Code 系）：
//	  --output-format stream-json --include-partial-messages --verbose
//	  NDJSON 行 {"type":"stream_event","event":{"type":"content_block_delta",
//	  "delta":{"type":"thinking_delta","thinking":"..."}}} / text_delta
//	  工具：content_block_start{type:tool_use} → content_block_delta{input_json_delta} → content_block_stop
//	       后续 content_block_start{type:tool_result} → text 字段即产出文本
//	  收尾 {"type":"result","subtype":"success","result":"全文",...}
//
//	trae：
//	  -p --output-format stream-json --include-partial-messages
//	  NDJSON 行 {"type":"stream_event","delta":{"role":"assistant",
//	  "content":"增量"}}；无 thinking 通道（模型侧不开 reasoning）；
//	  trae 的 stream-json 协议**不暴露独立工具事件通道**，KindToolUse/KindToolResult
//	  在 trae 下恒为空——这是协议限制，不是解析缺陷。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// StreamEventKind 增量类型。
type StreamEventKind string

const (
	// KindThinking 思考过程增量。
	KindThinking StreamEventKind = "thinking"
	// KindText 正文增量。
	KindText StreamEventKind = "text"
	// KindToolUse 模型发起工具调用。
	//
	// Name: 工具名（如 "Bash" / "Read" / "Edit"）。
	// ID: 引擎分配的工具调用 id（claude/codebuddy: tool_use_id；trae: 空串）。
	// Args: 完整参数 JSON 字符串（由 input_json_delta 累积而成；args 字段在
	//       content_block_stop 时一次性填入并 emit，消费方无需自己拼装）。
	// StreamEvent.Text 字段同步填入 Args，便于通用 NDJSON 序列化。
	KindToolUse StreamEventKind = "tool_use"
	// KindToolResult 工具调用产出。
	//
	// Name: 关联的 tool_use.id（用于反向索引）。
	// Text 字段即工具产出文本（可能是 JSON / 命令 stdout / 文件内容等）。
	KindToolResult StreamEventKind = "tool_result"
	// KindTurnEnd 一轮结束（引擎给出了 result 行）。
	//
	// 只有 claude / codebuddy 的 stream-json 路径会发（它们每轮收尾都有 result 行）。
	// 用途：常驻会话（Request.Append）里告诉调用方「这轮做完了，可以继续追加」——
	// CLI 的 keep-alive 空闲计时就挂在它上面（见 cli/ask.go）。
	// Text 填该轮的最终正文（与 result 行一致）。
	KindTurnEnd StreamEventKind = "turn_end"
	// KindAsk 「需要用户选择」（模型提问 / 工具待授权）。
	//
	// 归一化后的统一格式在 StreamEvent.Ask（跨引擎一致，见 ask.go），Text 是它的一行人话摘要。
	// 目前只有 claude / codebuddy 会发（其余引擎无 AskUserQuestion，也无 can_use_tool 协议）。
	//
	// ⚠️ 消费方注意：headless 下 claude 在模型发出提问后会**立刻自行拒绝**
	//（实测 tool_result "Answer questions?"，is_error=true），模型拿不到用户答案。
	// 要真正作答，得把答案作为**后续 user 消息**补进去（见 EncodeAskFollowUp），
	// 正好复用常驻会话的 --append 通道。
	KindAsk StreamEventKind = "ask"
)

// StreamEvent 一条流式增量。
type StreamEvent struct {
	Kind StreamEventKind
	Text string
	// Name 工具名（仅 KindToolUse / KindToolResult 携带；其余为空）。
	Name string
	// ID 工具调用 id（仅 KindToolUse / KindToolResult 携带；其余为空）。
	// KindToolResult 的 ID 与之关联的 KindToolUse.ID 相同。
	ID string
	// SessionID 会话 id（目前只有 KindTurnEnd 携带）。
	//
	// 为什么放在轮次事件上：常驻会话（keep-alive）的**最终 result 信封要等会话结束**
	// 才输出（默认空闲 5m），而调用方（观物台）需要在「这一轮刚做完」时就把会话 id
	// 落库/绑定 —— 否则下一轮追问会找不到锚点，只能退化成 `-c` 续接最近会话。
	// 引擎在每轮 result 行就能拿到 session_id（claude/codebuddy 的收尾行自带）。
	SessionID string
	// Ask 归一化后的「需要用户选择」请求（仅 KindAsk 携带；其余为 nil）。
	//
	// 有了它，调用方不必再分辨这次是 claude 的 tool_use 形态还是 control_request 形态
	//（两种形态在 wire 上字段完全不同，见 ask.go 的 ParseAskLine）。
	Ask *AskRequest
}

// ToolCall 一次完整的工具调用记录（用于收尾汇总）。
type ToolCall struct {
	// Name 工具名。
	Name string
	// ID 引擎分配的工具调用 id（可用于关联 KindToolResult）。
	ID string
	// Args 完整参数 JSON 字符串。
	Args string
	// Result 工具产出文本（可能为空——某些工具无文本产出）。
	Result string
}

// StreamResult 流式调用的收尾汇总。
type StreamResult struct {
	Response
	// Thinking 完整思考过程（各增量拼接）。
	Thinking string
	// Tools 全部工具调用记录（按发生顺序）。
	// 仅 claude/codebuddy 在 --tools on / 白名单模式下非空；
	// trae 协议不暴露工具事件通道，恒为 nil。
	Tools []ToolCall
}

// Streamer 流式引擎接口。实现负责单次流式尝试；
// 超时由 StreamRunner 外层控制。
type Streamer interface {
	// Stream 发起一次流式调用，实时把增量写入 onEvent，
	// 返回收尾结果（含全文与思考过程）。
	Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error)
}

// AsStreamer 把 Engine 升级为 Streamer（不支持流式的引擎返回 nil）。
func AsStreamer(e Engine) Streamer {
	s, _ := e.(Streamer)
	return s
}

// streamArgsFor 返回引擎的流式支持标记（用于 SupportsStream 快速判定）。
// 返回 nil 表示该引擎不支持流式。
func streamArgsFor(e Engine) []string {
	switch e.(type) {
	case *ClaudeEngine:
		return []string{"stream-json"}
	case *CodeBuddyEngine:
		return []string{"stream-json"}
	case *TraeEngine:
		return []string{"stream-json"}
	case *LLMEngine:
		return []string{"stream"} // llm prompt 默认流式（纯文本 stdout）
	}
	return nil
}

// SupportsStream 报告引擎是否支持流式。
func SupportsStream(e Engine) bool {
	return streamArgsFor(e) != nil && AsStreamer(e) != nil
}

// runStreamCLI 启动 CLI 流式进程，逐行回调，收尾返回全文与思考。
// lineHandler 返回非 nil error 时立即杀进程组并中止（解析致命错误）。
//
// 管道为何自建而不用 StdoutPipe：StdoutPipe 的读端会在 cmd.Wait() 里被
// 提前关闭——进程先退而 reader 尚有管道积压时，余量被内核丢弃、reader
// 读到 "file already closed"，把一次成功的流式调用变成报错（实测复现）。
// 自建管道的读端归 reader 独有、Wait 不触碰；父进程写端在 Start 后立即
// 关闭，EOF 语义 = 进程退出（含所有继承写端的后代）。
func runStreamCLI(ctx context.Context, bin string, args []string, lineHandler func(line string) error) error {
	return runStreamCLIIn(ctx, "", bin, args, lineHandler)
}

// runStreamCLIIn 同 runStreamCLI，可指定工作目录（workspace，见 runcmd.go runCLIIn 注释）。
func runStreamCLIIn(ctx context.Context, dir string, bin string, args []string, lineHandler func(line string) error) error {
	return runStreamCLIEnvIn(ctx, dir, nil, bin, args, lineHandler)
}

// runStreamCLIEnvIn = runStreamCLIIn + 追加环境变量（如 codex 的 CODEX_HOME 隔离）。
func runStreamCLIEnvIn(ctx context.Context, dir string, extraEnv []string, bin string, args []string, lineHandler func(line string) error) error {
	return runStreamCLIStdinIn(ctx, dir, extraEnv, bin, args, "", lineHandler)
}

// runStreamCLIStdinIn = runStreamCLIEnvIn + 可给子进程喂 stdin 内容（一次性字符串）。
func runStreamCLIStdinIn(ctx context.Context, dir string, extraEnv []string, bin string, args []string, stdin string, lineHandler func(line string) error) error {
	if stdin == "" {
		return runStreamCLIStdinReaderIn(ctx, dir, extraEnv, bin, args, nil, lineHandler)
	}
	return runStreamCLIStdinReaderIn(ctx, dir, extraEnv, bin, args, strings.NewReader(stdin), lineHandler)
}

// runStreamCLIStdinReaderIn = 上面那个 + stdin 可以是**流**（常驻会话靠它持续喂消息）。
//
// 为什么需要流：claude/codebuddy 的 `--input-format stream-json` 允许在一个进程里
// 持续接收 user 消息（「会话中追加需求」）。调用方给的 reader 不 EOF，子进程就一直活着；
// reader EOF（或关闭）→ 子进程收尾退出。
func runStreamCLIStdinReaderIn(ctx context.Context, dir string, extraEnv []string, bin string, args []string, stdin io.Reader, lineHandler func(line string) error) error {
	cmd := newStreamCmdIn(dir, bin, args)
	if len(extraEnv) > 0 {
		// Go exec 语义：重复 key 取最后一个 → extraEnv 覆盖继承值
		cmd.Env = append(cmd.Env, extraEnv...)
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stdout = stdoutW
	/* stderr 也自建管道 + 自己 drain（不能用 &streamStderrBuf{} 直挂）：
	   Go 只在 Stderr 是 *os.File 时才不做「内部拷贝 goroutine」，而 **cmd.Wait() 会等那个
	   goroutine 结束** —— 只要引擎还有后代进程（逃出进程组的 node worker 之类）握着 stderr
	   写端，Wait 就永远不返回。实测症状（2026-09-18，常驻会话 + `--stop`）：引擎已被杀、
	   magic-agent 却一直挂着不退（goroutine dump 显示主 goroutine 卡在 `<-waitErr`），
	   观物台那边就是「点了停止，界面一直转圈」。自建管道后 Wait 只等进程本身。 */
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return fmt.Errorf("stderr pipe: %w", err)
	}
	stderrBuf := &streamStderrBuf{}
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return err
	}
	notifySpawn(ctx, cmd.Process.Pid) // 会话登记表据此记住引擎子进程 pid
	stdoutW.Close()                   // 父进程不再持有写端；EOF 只取决于子进程一侧
	stderrW.Close()
	go func() { defer stderrR.Close(); _, _ = io.Copy(stderrBuf, stderrR) }()

	// 看门狗：ctx 结束 → 杀整组；否则等进程自然退出。
	done := make(chan error, 1)
	go func() {
		defer stdoutR.Close()
		sc := bufio.NewScanner(stdoutR)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 16MB 行上限（result 行可能很大）
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				if err := lineHandler(line); err != nil {
					killProcessGroup(cmd)
					done <- err // 让主 select 把 handler 错误带回（reader 是唯一发送方，不能在此收）
					return
				}
			}
		}
		if err := sc.Err(); err != nil {
			done <- fmt.Errorf("read stream: %w", err)
			return
		}
		done <- nil
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case err := <-done:
		// 读端已 EOF（引擎 stdout 关闭 = 引擎这一侧结束了）→ **不再死等 Wait**：
		// 后代进程可能还握着 stderr 写端，硬等会把「引擎已死」变成「magic-agent 挂着不退」
		//（见上面 stderr 管道的注释）。让 Wait 在后台自行回收即可 —— 本进程随后就退出，
		// 内核会把子进程交给 init 收尸。
		go func() { <-waitErr }()
		return err
	case err := <-waitErr:
		// 进程先退（正常退出时 reader 也会很快结束；先等 reader 把余量读完）。
		// 注意：waitErr 已在本 case 消费过，下方任何路径都不得再收，
		// 否则永久阻塞（waitErr 只有一个发送方，第二次接收必然死锁）。
		if err == nil {
			// 正常退出 → 等 reader 收尾（有限等待防挂死）。
			select {
			case rerr := <-done:
				return rerr
			case <-time.After(10 * time.Second):
				killProcessGroup(cmd)
				return fmt.Errorf("stream reader did not finish after process exit")
			}
		}
		killProcessGroup(cmd)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("cli exited: %w（reader 未收尾：后代进程可能仍持有 stdout）", err)
		}
		return fmt.Errorf("cli exited: %w", err)
	case <-ctx.Done():
		killProcessGroup(cmd)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			// 同 done 分支：后代进程可能还握着管道，不能让「杀组」变成永久挂起。
			return fmt.Errorf("process group killed: %w（reader 未收尾）", ctx.Err())
		}
		select {
		case <-waitErr:
		case <-time.After(10 * time.Second):
		}
		return fmt.Errorf("process group killed: %w", ctx.Err())
	}
}

// streamAccumulator 聚合增量与收尾 result 行。
type streamAccumulator struct {
	Text     strings.Builder
	Thinking strings.Builder
	OnEvent  func(StreamEvent)

	// Tools 已完成（收到 content_block_stop）的工具调用记录，按发生顺序追加。
	// 仅 claude/codebuddy 在 --tools on / 白名单模式下非空；
	// trae 协议不暴露工具事件通道，恒为 nil。
	Tools []ToolCall

	// 工具调用累积状态（仅 claude/codebuddy 用到；trae 协议不暴露这些字段）。
	//
	// 累积策略：
	//   content_block_start{type:tool_use, name, id} → 记录当前 open 的 tool_use
	//   content_block_delta{input_json_delta.partial_json} → 累积到 argsBuilder
	//   content_block_stop → 把累积结果 emit 为 KindToolUse，加入 Tools 列表
	//   content_block_start{type:tool_result} → 切换到结果累积模式
	//   （后续 text_delta → emit 为 KindToolResult，并把产出回填到对应 ToolCall.Result）
	//
	// 同一时刻只有一个 open tool_use；trae 下此状态恒为空。
	openToolName      string // 当前 open tool_use 的工具名
	openToolID        string // 当前 open tool_use 的 id
	openToolArgs      strings.Builder
	expectingToolStop bool // 下一个 stop 应触发 KindToolUse emit

	openResultID         string // 当前 open tool_result 关联的 id
	expectingResultStart bool   // 等待下一个 content_block_start{type:tool_result}

	// Engine 引擎名（各引擎构造累加器时填入），归一化「需要用户选择」时回填 AskRequest.Engine。
	// 留空也能跑：解析本身与引擎无关，只有 header 上限之类的归一化需要它。
	Engine string

	// SessionID 流里最近见到的 session_id（system/init、assistant、result 行都带）。
	//
	// 为什么要它：KindAsk 事件必须能**直接接住** —— 宿主看到提问后要把用户的答案
	// 送回同一个会话（`--append <session_id>`），而 json 模式下 stderr 保持干净、
	// 拿不到启动提示里的 run_id。init 行在流的最开头就给了 session_id，
	// 所以提问出现时这里一定已经有值（与 KindTurnEnd 带 SessionID 同一取舍）。
	SessionID string

	// askSeen 已发过 KindAsk 的请求 key（tool_use_id / request_id / 工具名+入参），用于去重。
	//
	// 为什么必须去重：claude 在 --include-partial-messages 下，同一个提问会**走两条路**
	// 被识别到 —— 一次是 content_block_start/stop 累积出的 tool_use（含完整 questions），
	// 一次是随后那份聚合 assistant 消息（也带完整 input）。只该报一次。
	askSeen map[string]bool
}

// toolUseOrphanGuard 安全释放：若 content_block_stop 到来但 openToolName 为空，
// 不发事件也不入 Tools 列表（防御协议异常）。
func (a *streamAccumulator) toolUseOrphanGuard() {
	if !a.expectingToolStop || a.openToolName == "" {
		return
	}
	args := a.openToolArgs.String()
	a.emitKindWithExtras(KindToolUse, args, a.openToolName, a.openToolID)
	a.Tools = append(a.Tools, ToolCall{
		Name: a.openToolName,
		ID:   a.openToolID,
		Args: args,
	})
	// 「需要用户选择」：claude/codebuddy 的提问（AskUserQuestion）在 wire 上就是一个
	// tool_use 块，参数（questions）到 stop 时才完整 —— 就在这里归一化并发 KindAsk。
	a.maybeEmitAsk(a.openToolName, json.RawMessage(args), a.openToolID)
	a.openToolName = ""
	a.openToolID = ""
	a.openToolArgs.Reset()
	a.expectingToolStop = false
}

// flushToolResult 把累积的 result 文本 emit 为 KindToolResult 并回填 ToolCall。
func (a *streamAccumulator) flushToolResult(text string) {
	if a.openResultID == "" {
		return
	}
	a.emitKindWithExtras(KindToolResult, text, "", a.openResultID)
	// 回填到最近一条匹配的 ToolCall。
	for i := len(a.Tools) - 1; i >= 0; i-- {
		if a.Tools[i].ID == a.openResultID && a.Tools[i].Result == "" {
			a.Tools[i].Result = text
			break
		}
	}
	a.openResultID = ""
	a.expectingResultStart = false
}

// emitKindWithExtras 带 Name / ID 字段的 emit 入口（工具事件专用）。
func (a *streamAccumulator) emitKindWithExtras(kind StreamEventKind, text, name, id string) {
	if a.OnEvent != nil {
		a.OnEvent(StreamEvent{Kind: kind, Text: text, Name: name, ID: id})
	}
}

// maybeEmitAsk 若刚完成的工具调用就是「需要用户选择」（AskUserQuestion），
// 归一化后发一条 KindAsk。非该工具直接返回（绝大多数工具走这里）。
func (a *streamAccumulator) maybeEmitAsk(toolName string, input json.RawMessage, toolUseID string) {
	if !isAskUserQuestion(toolName) {
		return
	}
	if req, ok := NormalizeAskToolUse(a.Engine, toolName, input, toolUseID); ok {
		a.emitAsk(req)
	}
}

// emitAsk 去重后发出 KindAsk 事件（统一格式在 StreamEvent.Ask）。
//
// 去重键取 tool_use_id（claude/codebuddy 必有）；没有 id 时退到 request_id，
// 再退到「工具名 + 入参」。同一个提问在 partial 与聚合两条路上各出现一次，只报一次。
func (a *streamAccumulator) emitAsk(req *AskRequest) {
	if req == nil {
		return
	}
	key := req.ToolUseID
	if key == "" {
		key = req.RequestID
	}
	if key == "" {
		key = req.ToolName + "|" + string(req.ToolInput)
	}
	if a.askSeen == nil {
		a.askSeen = make(map[string]bool)
	}
	if a.askSeen[key] {
		return
	}
	a.askSeen[key] = true
	if a.OnEvent != nil {
		a.OnEvent(StreamEvent{
			Kind: KindAsk,
			Text: askSummaryText(req),
			Name: req.ToolName,
			ID:   req.ToolUseID,
			// 带上会话 id：宿主据此把用户的答案 `--append` 回同一会话（唯一的作答通道，
			// 见 EncodeAskFollowUp 与 README「需要用户选择」一节）。
			SessionID: a.SessionID,
			Ask:       req,
		})
	}
}

// handleNDJSONLine 解析一行 NDJSON 流事件，返回是否为 result 收尾行。
// 兼容 claude/codebuddy（content_block_delta）与 trae（delta.content）两种族谱。
// claude/codebuddy 同时识别工具事件（content_block_start{type:tool_use|tool_result} +
// input_json_delta + content_block_stop）；trae 协议不暴露这些字段，本分支天然空转。
func (a *streamAccumulator) handleNDJSONLine(line string) (isResult bool, err error) {
	var probe struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
	}
	if jerr := json.Unmarshal([]byte(line), &probe); jerr != nil {
		return false, nil // 非 JSON 行（杂讯）忽略
	}
	// 顺手记住 session_id（init / assistant / result 行都带）：KindAsk 要靠它让宿主
	// 直接把答案追加回同一会话，见 streamAccumulator.SessionID 的说明。
	if probe.SessionID != "" {
		a.SessionID = probe.SessionID
	}

	switch probe.Type {
	case "result":
		// 收尾前若有残留 open tool_use（协议异常：没收到 stop），做兜底回收。
		a.toolUseOrphanGuard()
		return true, nil // 全文在 result.result，由调用方自行解析

	case "stream_event":
		// claude / codebuddy: event.content_block_delta.delta.{thinking_delta,text_delta}
		//                  + event.content_block_start.content_block.{type,name,id}
		//                  + event.content_block_stop
		// trae: delta.content 直出增量
		var se struct {
			Event struct {
				Type         string `json:"type"`
				ContentBlock struct {
					Type      string `json:"type"`
					Name      string `json:"name"`
					ID        string `json:"id"`
					ToolUseID string `json:"tool_use_id"`
				} `json:"content_block"`
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Thinking    string `json:"thinking"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			} `json:"event"`
			// trae: delta.content 直出增量
			Delta2 struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"delta"`
		}
		if jerr := json.Unmarshal([]byte(line), &se); jerr != nil {
			return false, nil
		}

		evtType := se.Event.Type
		cbType := se.Event.ContentBlock.Type

		switch evtType {
		case "content_block_start":
			switch cbType {
			case "tool_use":
				// 防御性：上一个 tool_use 没收到 stop 就来了新的 → 兜底收尾。
				a.toolUseOrphanGuard()
				a.openToolName = se.Event.ContentBlock.Name
				a.openToolID = se.Event.ContentBlock.ID
				a.openToolArgs.Reset()
				a.expectingToolStop = true
			case "tool_result":
				// 收尾当前 tool_use（标准顺序：use → stop → result → stop）。
				a.toolUseOrphanGuard()
				// tool_result 块 ID 字段是 tool_use_id（关联前一个 tool_use）。
				a.openResultID = se.Event.ContentBlock.ToolUseID
				if a.openResultID == "" {
					a.openResultID = se.Event.ContentBlock.ID
				}
				a.expectingResultStart = true
			}

		case "content_block_delta":
			// 注意：content_block_delta 这一行本身只通过 delta.type 区分
			// （thinking_delta / text_delta / input_json_delta），不会重复
			// 带上 content_block.type。所以路由既要 cbType 也要 d.Type。
			d := se.Event.Delta
			switch {
			case cbType == "tool_use" || d.Type == "input_json_delta":
				// input_json_delta：累积 partial_json 到 argsBuilder。
				if d.PartialJSON != "" {
					a.openToolArgs.WriteString(d.PartialJSON)
				}
			case cbType == "tool_result":
				// tool_result 的 text 字段即工具产出文本。
				if d.Text != "" {
					a.flushToolResult(d.Text)
				}
			case a.openResultID != "" && d.Type == "text_delta" && d.Text != "":
				// tool_result 块由 start/stop 框定，start 后到 stop 前的所有
				// text_delta 都属于 result 产出（cbType 在 delta 行里通常为空，
				// 必须靠 openResultID 状态识别）。
				a.flushToolResult(d.Text)
			case d.Type == "thinking_delta" || d.Thinking != "":
				if d.Thinking != "" {
					a.emit(KindThinking, d.Thinking)
				}
			case d.Type == "text_delta" || d.Text != "":
				if d.Text != "" {
					if cbType == "" || cbType == "text" {
						a.emit(KindText, d.Text)
					}
				}
			}

		case "content_block_stop":
			switch {
			case a.expectingToolStop:
				a.toolUseOrphanGuard()
			case a.expectingResultStart:
				// result 块没有 text_delta（某些实现下）→ 用空串兜底。
				a.flushToolResult("")
			}
		}

		// trae delta.content 直出增量（无 thinking / tool 通道）。
		if c := se.Delta2.Content; c != "" && se.Delta2.Role == "assistant" {
			a.emit(KindText, c)
		}

	case "assistant":
		// 非 partial 的整条 assistant 消息（trae 在 stream_event 之外还会
		// 发一份聚合行；claude 也会）。增量已在 stream_event 覆盖，
		// 这里不重复 emit，只做兜底：全文以 result 行为准。
		//
		// 兜底：assistant 聚合行有时是 tool_use 块（input 已完整）。如果
		// 累积器里还残留 openToolArgs（input_json_delta 累积到 stop 才发），
		// 这里也能补上 id/name（start 已经填了），不会丢工具事件。
		//
		// 另一件只有这里能做的事：**识别「需要用户选择」**。未开
		// --include-partial-messages 时（带附件的非流式路径就是这种）没有
		// content_block 增量，提问只出现在这份聚合行里。两条路都识别 → 靠
		// emitAsk 的 askSeen 去重，不会重复上报。
		if req, ok := ParseAskLine(a.Engine, line); ok {
			a.emitAsk(req)
		}

	case "control_request":
		// SDK 层请求（宿主自己实现了 canUseTool 回调时才会出现）：
		// subtype=can_use_tool 即「需要用户选择」——提问或工具待授权，都归一化。
		// 应答要原样带回 request_id（见 EncodeAskControlResponse）。
		// 同层的 rewind / interrupt 等子类型由 ParseAskLine 判掉。
		if req, ok := ParseAskLine(a.Engine, line); ok {
			a.emitAsk(req)
		}

	case "user":
		// claude / codebuddy 的 tool_result 不走独立 content_block，而是包在
		// 一个 user 消息里：
		//
		//   {"type":"user","message":{"role":"user","content":[
		//     {"tool_use_id":"<id>","type":"tool_result","content":"<text或数组>"}
		//   ]}}
		//
		// 这里把 tool_result 文本回填到对应的 ToolCall.Result，并 emit 一条
		// KindToolResult 事件（id 与之关联的 tool_use 相同）。
		a.handleUserToolResult(line)
	}
	return false, nil
}

// handleUserToolResult 处理 claude / codebuddy 把 tool_result 嵌入 user 消息的协议路径。
//
// 解析 user.message.content 数组，每个元素形如：
//
//	{"tool_use_id":"<id>","type":"tool_result","content":"<text>"}
//
// 或 content 是数组（嵌套 text/image 块）；取首段文本作为 result 文本。
//
// 命中条件宽松：只要元素里有 tool_use_id + type=tool_result 即视为工具产出。
// 该路径与 stream_event 里的 content_block{type:tool_result} 互斥（claude 不会
// 同时走两条路径），故不会重复 emit。
func (a *streamAccumulator) handleUserToolResult(line string) {
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if jerr := json.Unmarshal([]byte(line), &msg); jerr != nil {
		return
	}
	if msg.Type != "user" || msg.Message.Role != "user" {
		return
	}
	for _, c := range msg.Message.Content {
		if c.Type != "tool_result" || c.ToolUseID == "" {
			continue
		}
		text := extractToolResultText(c.Content)
		// 回填最近一条匹配 id 的 ToolCall.Result。
		for i := len(a.Tools) - 1; i >= 0; i-- {
			if a.Tools[i].ID == c.ToolUseID && a.Tools[i].Result == "" {
				a.Tools[i].Result = text
				break
			}
		}
		// emit KindToolResult 事件（不走基础 emit，必须带 id）。
		a.emitKindWithExtras(KindToolResult, text, "", c.ToolUseID)
	}
}

// extractToolResultText 从 tool_result.content 字段抽出可读文本。
//
// content 可能是：
//   - 字符串：直接返回
//   - 数组：取每个块的 text 字段拼接
//   - null/空：返回空串
func extractToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// emit 聚合并转发一条增量（基础版本：thinking / text）。
func (a *streamAccumulator) emit(kind StreamEventKind, text string) {
	switch kind {
	case KindThinking:
		a.Thinking.WriteString(text)
	case KindText:
		a.Text.WriteString(text)
	}
	if a.OnEvent != nil {
		a.OnEvent(StreamEvent{Kind: kind, Text: text})
	}
}

// emitTurnEnd 发一轮结束事件：text = 该轮正文（引擎 result 行的 result），
// sessionID = 该轮的会话 id（见 StreamEvent.SessionID 的说明）。
func (a *streamAccumulator) emitTurnEnd(text, sessionID string) {
	if a.OnEvent == nil {
		return
	}
	a.OnEvent(StreamEvent{Kind: KindTurnEnd, Text: text, SessionID: sessionID})
}
