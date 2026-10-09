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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
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
	// KindTurnFailed 一轮**没有走到终帧**（引擎流被截断、没吐 result 行）。
	//
	// 为什么需要它（2026-10-02，对齐 agents-anywhere 的 failed_terminal_event）：
	// 此前这条路径只有 `return fmt.Errorf("... without result line")`，
	// 于是「已经流出去的半截正文」在事件流上永远停在 running —— 消费方
	// （多端同步 / 历史回放）看到的是一条**悬空**的 item：既不知道它完了没有，
	// 也不知道该显示什么。补一条终态事件，一轮就绝不会悬空。
	//
	// 与 KindTurnEnd 的分工：turn_end = 引擎给出了 result 行（成功收尾）；
	// turn_failed = 引擎没给（异常收尾，带 Error / Reason）。
	// 两者都终结「这一轮」，消费方可以统一当边界用。
	KindTurnFailed StreamEventKind = "turn_failed"
)

// StreamStatus 事件所描述的 item 状态 —— 对齐 agents-anywhere 的
// timeline.item_status（2026-10-02）。
//
// 为什么要它：delta 广播只能告诉消费方「又来了几个字」，没法告诉它
// 「这条消息已经完整了」。多端同步时前者要客户端自己判断何时停止拼接
// （易错：漏拼、重复拼、乱序），后者只需「覆盖到 final 为止」。
type StreamStatus string

const (
	// StatusRunning 还在增长中（partial）。消费方应持续覆盖同一 item。
	StatusRunning StreamStatus = "running"
	// StatusFinal 终态：这条 item 的内容已经定型，不会再变。
	StatusFinal StreamStatus = "final"
	// StatusFailed 异常终态：引擎没走完就被掐断（见 KindTurnFailed）。
	StatusFailed StreamStatus = "failed"
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

	/* ── 状态收敛字段（2026-10-02，对齐 agents-anywhere 的 timeline item）──
	 *
	 * 此前的事件模型是**纯 delta 广播**：只说「又来了这几个字」，
	 * 消费方要自己拼、自己判断何时完整。对「终端直打」够用，但撑不起
	 * 多端同步 / 历史回放 / 断线重连 —— 那些场景里同一条消息会被
	 * 反复投递、乱序到达，前端不得不自己去重、排序、猜终态。
	 *
	 * agents-anywhere 的做法（claude/timeline/stream.py + core/timeline.py）：
	 * 事件语义是**状态收敛**而非增量广播：
	 *   · 每个可更新 item 有**稳定 id**（引擎原生 message id 派生）
	 *   · 每次更新带**单调递增的 revision**
	 *   · partial 与 final 用**同一个 id**，final 只是 revision 更高的覆盖
	 *   · 幂等公式 revision = max(item.revision, existing.revision+1)
	 *     ⇒ 重复投递不产生新版本
	 * 消费方于是只需要「按 (id, revision) 覆盖式 upsert」，天然幂等。
	 *
	 * ⚠️ **Text 的语义保持不变（仍是增量片段）** —— 这是本设计最重要的一条
	 * 取舍。`--stream -o text` 的正文直打、sessionlog 落盘、老客户端的
	 * 逐行拼接全都依赖它，改成整段快照会静默打断所有既有消费方。
	 * 想做 upsert 的消费方读 Snapshot；想增量拼接的继续读 Text。
	 * 两个字段并存，引擎层一次投影同时喂饱两类消费方。 */

	// ItemID 稳定的 item 标识（timeline item id）。空串 = 本事件不参与状态收敛
	//（老引擎 / 无法归一化身份时）。取值形如 "msg_<native message id>"。
	//
	// 同一轮里的 thinking / text / 工具事件共享同一个 message item id，
	// revision 各自递增 —— 消费方按 (ItemID, Revision) upsert 即得最终全文。
	ItemID string

	// Revision 该 item 的第几个版本，从 1 起单调递增。
	// 同一个 ItemID 下：partial 是 1..N，final 是 N+1（覆盖，不是追加）。
	ItemRevision uint64

	// Status 本事件描述的 item 状态（见 StreamStatus）。空串 = 老语义
	//（纯增量，既无终态也无状态机），消费方可按「非空才启用收敛」处理。
	Status StreamStatus

	// Snapshot 该 item 的**当前完整内容**（累积到此刻为止），与 Text（增量）并存。
	//
	// 为什么已经累积了还要发增量：两类消费方要的东西不同 ——
	// 终端直打要增量（否则重打一遍全文）；多端同步要快照（否则要自己拼，
	// 且拼不回去的历史/乱序场景无解）。发了快照，消费方可以完全忽略 Text。
	//
	// 仅在 ItemID 非空时携带；Snapshot 不可靠的引擎留空（消费方回退到 delta 拼接）。
	Snapshot string

	// Error 本轮异常收尾的原因（仅 KindTurnFailed 携带）。
	Error string
	// Reason 根因摘要（仅 KindTurnFailed 携带），与 -o json 的失败 envelope 同源
	//（同走 agent.ReasonOf），同一次失败在事件流与 stderr 两处的说法必然一致。
	Reason string
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
	case *CodeBuddyAIEngine:
		return []string{"stream-json"}
	case *TraeEngine:
		return []string{"stream-json"}
	case *LLMEngine:
		return []string{"stream"} // llm prompt 默认流式（纯文本 stdout）
	case *DshEngine:
		// dsh 默认走 SDK profile 的 stdio JSON-RPC（session.event 帧）：推理、正文
		// 逐 step、工具调用都有；该 profile 不可用时回退 headless（只有推理增量走
		// stderr、正文一次性走 stdout）。标 sdk-jsonrpc 便于排查时一眼看出这一族。
		return []string{"sdk-jsonrpc"}
	case *OpenClawEngine:
		// openclaw 的流式走 **ACP**（`openclaw acp`，stdio + JSON-RPC，背后接本地 Gateway）：
		// 正文逐字增量 + tool_call/tool_call_update（思考流协议侧不支持）。
		// ⚠️ 它比别的引擎多一个外部依赖（Gateway 在跑 + ACP 桥的 scope 已批）：
		// 桥不可用时 Stream 会**自动回退**内嵌一次性调用（见 openclaw_acp.go 文件头）。
		return []string{"acp"}
	case *ArkClawEngine:
		// arkclaw 的流式走 A2A 官方的 **SSE** 通道（`message/stream`，text/event-stream）。
		// ⚠️ 与「逐字」不是一回事：实测本网关只发「受理帧（working，空正文）+ 终帧
		//（completed，整段正文）」两帧 —— 正文仍是一次性到达，见 arkclaw_stream.go 文件头。
		// 标 sse 便于排查时一眼看出这一族（也是 WebSocket 走不通后的唯一正路）。
		return []string{"sse"}
	case *CodeBuddyGatewayEngine:
		// codebuddy-gateway 也走 SSE（`GET /api/v1/runs/{runId}/stream`），但它同时是
		// **非流式路径的唯一取文通道**（投递响应里没有正文），所以 --stream 与非 --stream
		// 走的都是这条路。⚠️ 实测（2026-09-24）该网关目前只推终帧、**不发增量帧** ——
		// 「流式」的成色与 arkclaw 一样是「整段到达」，别当成打字机；
		// 增量解析已按协议实现，网关将来吐 chunk 时零改动即可接住。
		// 见 codebuddy_gateway_stream.go 文件头。
		return []string{"sse"}
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

// runStreamStderrIn 启动 CLI 并把它的 **stderr 逐行回调**，收尾返回 stdout 全文与 stderr 全文。
//
// 为什么需要它（与上面几个 helper 正好相反）：其余引擎的增量走 stdout，
// 而 dsh 的 headless 把**推理增量写到 stderr**（"dsh: reasoning:" 起头），
// 最终正文才一次性打到 stdout。要把它接成 Streamer，就必须扫 stderr 而不是 stdout。
//
// 管道与 Wait 的处理沿用上面同一条经验（自建管道 + 自己 drain，不让 cmd.Wait 等
// Go 的内部拷贝 goroutine，否则后代进程握着写端时 Wait 永不返回）。
func runStreamStderrIn(ctx context.Context, dir string, bin string, args []string, onStderrLine func(line string) error) (string, string, error) {
	cmd := newStreamCmdIn(dir, bin, args)

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return "", "", fmt.Errorf("stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return "", "", fmt.Errorf("stderr pipe: %w", err)
	}
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return "", "", err
	}
	notifySpawn(ctx, cmd.Process.Pid) // 会话登记表据此记住引擎子进程 pid
	stdoutW.Close()                   // 父进程不再持有写端；EOF 只取决于子进程一侧
	stderrW.Close()

	// stdout 只做收集（最终正文），不逐行回调。
	stdoutDone := make(chan error, 1)
	go func() {
		defer stdoutR.Close()
		_, cerr := io.Copy(&stdoutBuf, stdoutR)
		stdoutDone <- cerr
	}()

	// stderr 逐行扫 + 回调（推理增量）。
	stderrDone := make(chan error, 1)
	go func() {
		defer stderrR.Close()
		sc := bufio.NewScanner(stderrR)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			stderrBuf.WriteString(line)
			stderrBuf.WriteByte('\n')
			if onStderrLine == nil {
				continue
			}
			if herr := onStderrLine(line); herr != nil {
				killProcessGroup(cmd)
				stderrDone <- herr
				return
			}
		}
		stderrDone <- sc.Err()
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	// result 收尾：把两条管道读干净（都限时，防后代进程握着写端永久挂起），
	// 再把缓冲里的内容交出去。
	result := func(err error) (string, string, error) {
		waitDrainedBounded(stderrDone, 5*time.Second)
		waitDrainedBounded(stdoutDone, 5*time.Second)
		return stdoutBuf.String(), stderrBuf.String(), err
	}

	select {
	case werr := <-waitErr:
		return result(werr)
	case serr := <-stderrDone:
		if serr == nil {
			// stderr 先 EOF（罕见）：仍要等进程退出才算收尾。
			select {
			case werr := <-waitErr:
				return result(werr)
			case <-ctx.Done():
				killProcessGroup(cmd)
				return result(fmt.Errorf("process group killed: %w", ctx.Err()))
			}
		}
		// 回调报错（或读流失败）：杀组后收尾。
		killProcessGroup(cmd)
		select {
		case <-waitErr:
		case <-time.After(10 * time.Second):
		}
		return result(serr)
	case <-ctx.Done():
		killProcessGroup(cmd)
		select {
		case <-waitErr:
		case <-time.After(10 * time.Second):
		}
		return result(fmt.Errorf("process group killed: %w", ctx.Err()))
	}
}

// waitDrainedBounded 等一个 drain goroutine 收尾，最多等 d（超时即放弃，防挂死）。
func waitDrainedBounded(ch <-chan error, d time.Duration) {
	select {
	case <-ch:
	case <-time.After(d):
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

	// AggregateOnly 标记本引擎的正文**只**出现在 assistant 聚合行里、
	// 不会走 stream_event 的 content_block_delta（2026-10-01，codebuddy 实测）。
	//
	// 为什么需要它：handleNDJSONLine 的 assistant 分支历史上只做兜底、不 emit
	// （因为 claude/trae 的增量已经在 stream_event 里发过了，再 emit 会重复）。
	// 但 codebuddy 不发 delta —— 不开这个开关，它的 `-o text` 实时输出全程空白。
	// 由引擎显式声明，而不是「猜」（猜错就是重复输出，两种错法都很难查）。
	AggregateOnly bool

	// SanitizeTurnText 该引擎的「轮次正文清洗器」：把 result 行的**原始**
	// result 字段整理成可以直接当正文交出去的样子。
	//
	// 为什么需要它（2026-10-01，codebuddy 实测）：result 行里的 result 字段是
	// 未经处理的引擎原始输出 —— codebuddy 把思维链以 <think>…</think>
	// 塞在同一个字符串里。于是同一轮里三个来源的"干净程度"并不一致：
	//
	//	acc.Text    已清洗（emit 时过 stripThinkBlock）
	//	fin.Result  未清洗（← turn_end 直接拿它发，于是漏了）
	//	最终正文     由调用方 stripThinkBlock(finalizeStreamText(...)) 得到，已清洗
	//
	// 于是 `-o json` 的 NDJSON 里会出现「text 干净、turn_end 脏」的自相矛盾。
	// 症状极隐蔽：内容本身是对的，只是 turn_end 那一条夹着一段英文推理，
	// 消费方（观物台把 turn_end 当轮次边界）就会把推理当成正文收进去。
	//
	// 由引擎声明（claude 留 nil = 原样不动），而不是在 emitTurnEnd 里对所有引擎
	// 无脑套 think 剥离 —— 那会把合法输出里恰好出现的 <think> 字面量也切掉。
	SanitizeTurnText func(string) string

	// SessionID 流里最近见到的 session_id（system/init、assistant、result 行都带）。
	//
	// 为什么要它：KindAsk 事件必须能**直接接住** —— 宿主看到提问后要把用户的答案
	// 送回同一个会话（`--append <session_id>`），而 json 模式下 stderr 保持干净、
	// 拿不到启动提示里的 run_id。init 行在流的最开头就给了 session_id，
	// 所以提问出现时这里一定已经有值（与 KindTurnEnd 带 SessionID 同一取舍）。
	//
	// 顺带也用它给 `OnSessionID` 兜底（见 stream.go handleNDJSONLine 里 OnSessionID 回调）——
	// 在 handleNDJSONLine 收到第一条含 session_id 的 NDJSON 行时就触发，开文件早于第一条事件。
	SessionID string

	// OnSessionID 见 Request.OnSessionID：可选回调，nil 跳过。
	// CLI 在 runStreamAsk 里 wire 到 sessionWriter 的开文件动作上。
	OnSessionID func(id string)

	// askSeen 已发过 KindAsk 的请求 key（tool_use_id / request_id / 工具名+入参），用于去重。
	//
	// 为什么必须去重：claude 在 --include-partial-messages 下，同一个提问会**走两条路**
	// 被识别到 —— 一次是 content_block_start/stop 累积出的 tool_use（含完整 questions），
	// 一次是随后那份聚合 assistant 消息（也带完整 input）。只该报一次。
	askSeen map[string]bool

	/* ── 状态收敛状态机（2026-10-02，见 StreamEvent 的状态收敛字段注释）──
	 *
	 * agents-anywhere 的 ClaudeStreamAccumulator（runtimes/claude/timeline/stream.py）
	 * 维护三样东西，本文件照抄同一套：
	 *   partial_message_id  稳定 item id（message_start 记原生 id 派生）
	 *   partial_*_blocks    按 content block index 的累积文本
	 *   partial_revision    当前 revision 计数
	 * 终帧沿用同 id + revision+1 覆盖。 */

	/* 正文与思考各一条 item（2026-10-02 修正，理由见 itemtracker.go
	   itemLane 的注释）：共享一个 item id 时消费方覆盖式 upsert 会丢掉
	   先到的那一半。两条 item 内嵌同一个原生 message id，合并展示时
	   按原生 id 关联即可。 */
	textTrack  *itemTracker
	thinkTrack *itemTracker

	// textBlocks 按 content block index 累积的正文片段。
	textBlocks map[int]string
	// thinkBlocks 按 content block index 累积的思考片段。
	thinkBlocks map[int]string
}

// trackers 惰性建立两条 lane 的跟踪器（prefix 依赖 Engine，
// 累加器是零值构造的，不能在字段声明处建）。
func (a *streamAccumulator) trackers() trackerPair {
	if a.textTrack == nil {
		a.textTrack = newItemTracker(a.Engine, laneText)
	}
	if a.thinkTrack == nil {
		a.thinkTrack = newItemTracker(a.Engine, laneThink)
	}
	return trackerPair{text: a.textTrack, think: a.thinkTrack}
}

/* ── 稳定 item id 的建立与降级链 ──────────────────────────────────
 *
 * agents-anywhere 的规则（claude/timeline/messages.py stable_message_item_id）：
 * 从 message_start 记下的原生 id 派生 `claude_msg_<hash>`，**跨事件稳定**。
 * 拿不到 message_start.id 时它「丢弃并 warning，不猜」。
 *
 * 我们同理，但降级链多两级（引擎协议不保证每次都有原生 id）：
 *
 *	1. 原生 message id（claude/codebuddy 实测必有，贯穿整条消息）
 *	2. 本轮内稳定的**序号**（首个 text/thinking 块出现时定下，
 *	   之后本轮不再变）—— 同一轮内稳定，重放时靠 seq 也能推回同一个
 *
 * ⚠️ 降级到 2 时**绝不能**每帧新造一个 id（否则一条消息被拆成 N 个 item），
 * 所以序号在首次建立后写进 tracker.itemID，之后只读不重算。 */

// beginMessage 声明一条新 assistant message 开始，清空块桶。
//
// 为什么必须切：一轮里可能有**多条** assistant message（工具调用后
// 继续对话，每次 tool_result 之后都是新的 message_start）。不切的话它们
// 共用一个 item id，前一条被后一条覆盖 —— 症状是「只看到最后一段回复」。
//
// 与 agents-anywhere 同动作（runtimes/claude/timeline/stream.py:35-36 在
// message_start 里 clear() 块桶并把 revision 归零），这里是封装。
func (a *streamAccumulator) beginMessage(nativeID string) {
	p := a.trackers()
	p.text.Begin(nativeID)
	p.think.Begin(nativeID)
	a.textBlocks = nil
	a.thinkBlocks = nil
}

// observeNativeMessageID 从任意 NDJSON 行里捞出原生 assistant message id。
//
// 2026-10-02 实测（claude stream-json 抓包）：message_start 事件里
// event.message.id 与后续 assistant 聚合行 message.id **完全一致**，
// 且整条消息期间不变 —— 所以两条路任一先到都能定下同一个 id。
func (a *streamAccumulator) observeNativeMessageID(line string) {
	var probe struct {
		Type    string `json:"type"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		Event struct {
			Type    string `json:"type"`
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
		} `json:"event"`
	}
	if json.Unmarshal([]byte(line), &probe) != nil {
		return
	}
	// message_start = 一条新 assistant message 开始：切 item、记原生 id。
	// 必须在事件路由**之前**做（message_start 早于所有 content_block_delta），
	// 否则新消息的第一个 delta 会挂到上一条消息的 item 上。
	if probe.Event.Type == "message_start" {
		nativeID := probe.Event.Message.ID
		if nativeID == "" && probe.Type == "assistant" {
			nativeID = probe.Message.ID
		}
		a.beginMessage(nativeID)
		return
	}
	// 聚合 assistant 行也带 message.id（实测与 message_start 的 id 一致），
	// 作为 message_start 缺失时定下 id 的第二条路。
	if probe.Type == "assistant" && probe.Message.ID != "" {
		p := a.trackers()
		p.text.Note(probe.Message.ID)
		p.think.Note(probe.Message.ID)
	}
}

// appendBlock 往某个 index 累积文本块。
func (a *streamAccumulator) appendBlock(store map[int]string, index int, text string) {
	if text == "" || store == nil {
		return
	}
	store[index] += text
}

// joinBlocks 按 index 升序拼接所有块（与到达顺序无关）。
func (a *streamAccumulator) joinBlocks(store map[int]string) string {
	if len(store) == 0 {
		return ""
	}
	idx := make([]int, 0, len(store))
	for i := range store {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var sb strings.Builder
	for _, i := range idx {
		sb.WriteString(store[i])
	}
	return sb.String()
}

// emit 聚合并转发一条增量（基础版本：thinking / text）。
//
// 两种模式并存（2026-10-02）：
//   - OnEvent == nil：不发事件，只累积到 Text / Thinking（老行为）。
//   - 已建立 item：Text 仍带增量（**不破坏**终端直打与老客户端），
//     同时带 ItemID / ItemRevision / Status / Snapshot 四元组，
//     消费方按 (ItemID, Revision) upsert 即得状态收敛。
func (a *streamAccumulator) emit(kind StreamEventKind, text string) {
	switch kind {
	case KindThinking:
		a.Thinking.WriteString(text)
	case KindText:
		a.Text.WriteString(text)
	}
	if a.OnEvent == nil {
		return
	}
	ev := StreamEvent{Kind: kind, Text: text}
	if kind == KindThinking || kind == KindText {
		a.decorateTextEvent(&ev, kind)
	}
	a.OnEvent(ev)
}

// decorateTextEvent 给 thinking/text 事件补上状态收敛四元组。
//
// 累积策略：**整段 snapshot**（不是增量片段）—— 与 agents-anywhere 一致。
// 消费方覆盖式 upsert，天然去重，不需要「先删 partial 再插 final」的顺序逻辑。
func (a *streamAccumulator) decorateTextEvent(ev *StreamEvent, kind StreamEventKind) {
	/* snapshot 从「按 content block index 升序拼接的块」取，而不是从
	   到达顺序累积的 builder 取。区别是实质性的：一条消息里正文可能分散
	   在多个 text 块，而块的**到达顺序**不保证等于 index 顺序
	   （实测：index=1 的块先到、index=0 的块后到是可能的）。
	   按到达顺序拼会给消费方一份顺序错乱的正文，而且错得很隐蔽 ——
	   内容都在，只是读起来不对。 */
	if kind == KindThinking {
		a.trackers().think.nextWith(StatusRunning, a.joinBlocks(a.thinkBlocks)).apply(ev)
		return
	}
	a.trackers().text.nextWith(StatusRunning, a.joinBlocks(a.textBlocks)).apply(ev)
}

// emitTextDelta 累积一个内容块的增量并发出带状态收敛元数据的事件。
//
// index 来自 claude 的 content_block index（-1 = 协议没给，如 trae /
// codebuddy 聚合行）。按 index 分桶累积而不是一路 append，是因为
// 一条 assistant 消息里正文可能分散在多个 text 块，而块的**到达顺序**
// 不保证等于 index 顺序（实测 thinking=0 先到、text=1 后到，但反过来也可能）。
func (a *streamAccumulator) emitTextDelta(index int, kind StreamEventKind, text string) {
	if text == "" {
		return
	}
	if a.textBlocks == nil {
		a.textBlocks = make(map[int]string)
	}
	if a.thinkBlocks == nil {
		a.thinkBlocks = make(map[int]string)
	}
	store := a.textBlocks
	if kind == KindThinking {
		store = a.thinkBlocks
	}
	a.appendBlock(store, index, text)
	a.emit(kind, text)
}

// emitAggregateText 从 assistant 聚合行发正文（AggregateOnly 引擎专用）。
//
// ⚠️ 与 emitTextDelta 的**关键区别**：聚合行给的是**整条消息的全文**，
// 不是增量。所以 snapshot 是「整体替换」而不是「追加」——
// 追加会得到 "你好你好"（同一份内容被数了两遍），这正是 codebuddy 路径
// 最容易踩的坑（它只发聚合行，delta 路径一次都不走）。
//
// Text 字段仍然原样带整段（保持 codebuddy 历史行为：终端直打逐行拼出来
// 恰好就是全文），不做差分 —— 对聚合引擎而言两者的结果本来就相同。
func (a *streamAccumulator) emitAggregateText(full string) {
	if full == "" {
		return
	}
	if a.textBlocks == nil {
		a.textBlocks = make(map[int]string)
	}
	// 单桶：整段覆盖（协议给不了 index，用 0 桶并始终覆盖）。
	a.textBlocks[0] = full
	a.emit(KindText, full)
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
		/* 通知 CLI 「会话 id 已出现」：让 sessionWriter 在写第一条事件之前就能
		   开好文件，避免「打开文件前的那条 init 行（带 thinking 起点）全丢」。
		   OnSessionID 是可选字段（nil 跳过），老调用方无需改。 */
		if a.OnSessionID != nil {
			a.OnSessionID(probe.SessionID)
		}
	}
	/* 原生 assistant message id（状态收敛的锚点，2026-10-02）。
	   必须**在事件路由之前**观察：message_start 早于所有 content_block_delta，
	   在它之后才建立 item id 的话，前几个 delta 会落到「序号降级」id 上。
	   实测（claude 抓包）：message_start.event.message.id 与 assistant 聚合行的
	   message.id 一致，所以两条路都能定下同一个 id。 */
	a.observeNativeMessageID(line)

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
				Type    string `json:"type"`
				Message struct {
					ID string `json:"id"`
				} `json:"message"`
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
					// 按 content block index 累积：思考与正文可能分处不同块，
					// snapshot 必须按 index 拼，不能按到达顺序 append。
					a.emitTextDelta(se.Event.Index, KindThinking, d.Thinking)
				}
			case d.Type == "text_delta" || d.Text != "":
				if d.Text != "" {
					if cbType == "" || cbType == "text" {
						a.emitTextDelta(se.Event.Index, KindText, d.Text)
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
		// index 恒为 -1（协议不给），单桶累积 = 整段拼接，与到达顺序一致。
		if c := se.Delta2.Content; c != "" && se.Delta2.Role == "assistant" {
			a.emitTextDelta(-1, KindText, c)
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
		/* AggregateOnly 引擎（2026-10-01，codebuddy 实测）：它**不发**
		   content_block_delta，正文只出现在这份聚合行里。若不在这里 emit，
		   `-o text` 的实时 stdout 会全程空（用户看到「转圈然后什么都没有」），
		   只有 json 模式收尾的 result 行才有正文。
		   claude/trae 走 stream_event 增量，这里再 emit 会**重复输出**，
		   所以由引擎显式声明（见 streamAccumulator.AggregateOnly）。 */
		if a.AggregateOnly {
			if txt := assistantLineText(line); txt != "" {
				// 与收尾路径同一个过滤器：codebuddy 会把推理以 <think>…</think>
				// 的形式塞进正文，不剥的话实时输出里会混进英文推理。
				a.emitAggregateText(stripThinkBlock(txt))
			}
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

// assistantLineText 从 assistant 聚合行里取出纯文本正文。
//
// 行形状（claude / codebuddy 同族）：
//
//	{"type":"assistant","message":{"content":[{"type":"text","text":"…"}]}}
//
// content 可能是字符串（少见）或块数组。tool_use / tool_result 块跳过 ——
// 它们由 content_block_* 与 user 消息两条路负责，不在这里重复发。
// 嵌套的 thinking 块也跳过（codebuddy 会把推理塞在 result 字符串里，
// 由 codebuddy.go 的 stripThinkBlock 在收尾时整段剥离）。
func assistantLineText(line string) string {
	var msg struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return ""
	}
	raw := msg.Message.Content
	if len(raw) == 0 {
		return ""
	}
	// 形态 1：content 直接是字符串。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// 形态 2：块数组，只取 type=="text" 的 text 拼接。
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// emitTurnEnd 发一轮结束事件：text = 该轮正文（引擎 result 行的 result），
// sessionID = 该轮的会话 id（见 StreamEvent.SessionID 的说明）。
//
// text 会先过一遍 SanitizeTurnText（由引擎声明的清洗器）——
// 不然这里发出去的是引擎**原始** result，与同一轮的 text / result 事件不一致
// （codebuddy 的 <think> 泄漏正是从这条路出去的，见 SanitizeTurnText 的注释）。
func (a *streamAccumulator) emitTurnEnd(text, sessionID string) {
	if a.OnEvent == nil {
		return
	}
	if a.SanitizeTurnText != nil {
		text = a.SanitizeTurnText(text)
	}
	ev := StreamEvent{Kind: KindTurnEnd, Text: text, SessionID: sessionID}
	/* 终帧 = 状态收敛的**最后一次 upsert**：沿用与 partial 同一个 ItemID、
	   revision 再 +1、Status 标 final。消费方按 (ItemID, Revision) 覆盖即可，
	   不需要「先删 partial 再插 final」这种有顺序要求的逻辑（agents-anywhere
	   的做法，见 claude/timeline/stream.py next_final_revision）。
	   Snapshot 用 result 行的正文而非累积分片 —— 引擎给出的 result 是权威值，
	   与增量拼接有出入时以它为准（claude 的 result 行 = 完整正文；codebuddy 的
	   result 行还要过 stripThinkBlock，上面已过）。

	   ⚠️ 只收尾**正文** lane：result 行是整条消息的正文，不含思考链。
	   思考 lane 已经在最后一个 thinking_delta 上收到过内容，此后不会再增长；
	   若也给它发 final，消费方会看到一条「内容中途消失」的 final（快照回退成空），
	   反而制造出比不收尾更糟的状态。想标记思考已结束，靠消费方见到 result
	   事件后停止等待即可 —— 它拿得到 turn 级的终态信号。 */
	if attrs, ok := a.trackers().finalize(text); ok {
		attrs.apply(&ev)
	}
	a.OnEvent(ev)
}

// emitTurnFailed 补一条「本轮异常收尾」事件（引擎没给 result 行）。
//
// 对齐 agents-anywhere 的 failed_terminal_event（claude/turns/lifecycle.py）：
// 流被截断也**必须**发终态事件，绝不让一轮悬空。
//
// 为什么这条不能省（2026-10-02）：此前这条路径只有 return error，
// 于是「已经流出去、正打到一半」的正文在事件流上永远停在 running ——
// 多端同步的客户端看到的是一个既不增长也不结束的 item，
// 只能靠超时猜；历史回放更糟：那一条永远显示「生成中」。
//
// 语义与 KindTurnEnd 的分工：
//
//	turn_end    = 引擎给出了 result 行（成功收尾）
//	turn_failed = 引擎没给（异常收尾），带 Error / Reason
func (a *streamAccumulator) emitTurnFailed(sessionID string, err error) {
	if a.OnEvent == nil || err == nil {
		return
	}
	ev := StreamEvent{
		Kind:      KindTurnFailed,
		SessionID: sessionID,
		Error:     err.Error(),
		Reason:    ReasonOf(err),
		Status:    StatusFailed,
	}
	// 已建立 item 时把失败也挂到同一个 item 上：partial 的内容仍然存在，
	// 只是这条 item 不会再更新了（Status=failed 明确终止增长）。
	if attrs, ok := a.trackers().failText(a.joinBlocks(a.textBlocks)); ok {
		attrs.apply(&ev)
	}
	a.OnEvent(ev)
}
