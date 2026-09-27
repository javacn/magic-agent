package agent

// dsh 的 SDK 传输（`dsh --profile sdk`）：换行分帧的 JSON-RPC 2.0 over stdio。
//
// 为什么需要它：headless profile 没有工具调用通道 —— 官方定位就是「reasoning 走 stderr、
// 最终正文走 stdout、然后退出」，stdout 里只有最终助手消息、stderr 里只有推理增量，
// **工具调用不出现在任何 CLI 流上**（实测：模型用了 bash/glob/read，CLI 只字未提）。
// 工具调用只写进会话日志（$DSH_HOME/sessions/…/session.v3.jsonl.zstd，zstd 压缩）。
//
// 官方内置的 `sdk` profile 则把**运行时内每个会话事件**实时推给客户端：
//
//	initialize        {cwd, provider, model, maxTokens?} → {serverInfo}
//	session/prompt    {sessionId, contentBlocks}         → {messageId}（持久入队回执）
//	shutdown          {}                                 → {}
//	← session.event   {sessionId, event}                 每个会话事件，不过滤
//	← session.status  {sessionId, status: running|idle}
//
// session.event 的 event.type 就是会话日志里那些类型（@deepseek-ai/dsh-session 的
// SessionEventMap），其中：
//
//	assistant/message  {turn, step, message:{content:[…]}}  content 里混有
//	                                                        reasoning / text 块 → KindThinking / KindText
//	tool/call          {turn, step, callId, name, arguments} → KindToolUse
//	tool/result        {turn, step, message:{source:{kind:"tool",callId}, content:[…]}}
//	                                                        → KindToolResult
//	turn/end           {turn, reason}                       一轮结束
//
// 相比 headless 的净收益：① 工具调用可上报；② 正文按 step 实时到达（不再等 turn 结束）；
// ③ initialize 可指定 provider/model 与 maxTokens；④ 一轮有明确的 turn/end 边界；
// ⑤ **同一进程内可续接同一会话**（见下）。
//
// 会话续接（两种情形必须分清）：
//
//	同一进程内续接：**支持**。对同一个 sessionId 连续 session/prompt 就是官方推荐的续接
//	  方式 —— 官方 Python SDK 文档原文「reuse a harness, home, and id only to continue
//	  the same durable conversation」。本引擎把它接在 Request.Append 上：常驻会话
//	  （CLI 的 --keep-alive + --append）期间同一个 runtime 进程活着，每条追加都排进同一
//	  会话，于是拿到真正的多轮上下文（探针实测第二轮能复述第一轮让记的数字）。
//	跨进程续接：**做不到**。服务端的 getOrCreateSession 只在**本进程的** sessions 表里
//	  查，未命中就走 createSession → `ctx.agents.create`（源码 lib/index.js），拿一个已
//	  存在的持久 id 会撞 SessionAlreadyExistsError（实测 -32603
//	  `session "x" already exists`）。harness 核心其实有 `agents.resume`
//	  （dsh-agent 类型声明：「Load a persisted session and resume an agent on it」），
//	  但 SDK 协议只暴露 initialize / session/prompt / shutdown 三个方法，没把它放出来
//	  —— 只有 Web/TUI 那条 host API 用得上 resume。故 magic-agent 的 `-s <id>` 对 dsh
//	  仍是显式报错，并指向 `--append`（见 dsh.go 的 errDshNoSessionResume）。
//
// 依据：@deepseek-ai/dsh-sdk-protocol 的 README.zh.md / lib/types/types.d.ts，
// @deepseek-ai/dsh-sdk-jsonrpc-server 的 README.zh.md 与 lib/index.js，
// @deepseek-ai/dsh-llm 的 ContentBlock、@deepseek-ai/dsh-session 的 SessionEventMap，
// 以及官方 Python SDK 文档（docs/user/guide/python-sdk.md）。
//（serverInfo.name 固定为 deepseek-harness-sdk-runtime，实测 version 0.0.1）。
// 协议自述「无协议版本协商、预发布、无兼容承诺」，故本实现只依赖上面这几个具名帧，
// 未知 event.type 一律忽略（前向兼容）。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// dshSDKProfile sdk profile 名（启动器 app 参数）。
const dshSDKProfile = "sdk"

// errDshSDKUnavailable 表示 SDK 通道不可用（profile 起不来 / 握手握手不上），
// 调用方据此回退到 headless，而不是把整次调用判死。
var errDshSDKUnavailable = errors.New("dsh SDK profile unavailable")

// errDshSDKClosed SDK 子进程在响应到达前就退出了（profile 不存在 / 启动即崩）。
var errDshSDKClosed = errors.New("SDK 进程提前退出")

// dshRPCErrFrame JSON-RPC 错误响应（服务端明确拒绝：模型没配、参数非法…）。
//
// 与 errDshSDKUnavailable 严格区分：这是**这一轮真的失败了**（例如 initialize 报
// `provider "x" has no configured model "y"`），回退 headless 只会把错误吞掉、
// 让用户拿到一个「看起来成功但换了模型」的结果。
type dshRPCErrFrame struct {
	Code    int
	Message string
}

func (e *dshRPCErrFrame) Error() string {
	return fmt.Sprintf("JSON-RPC %d: %s", e.Code, e.Message)
}

// dshSDKInitTimeout 握手超时：initialize 要解析 provider/model 路由，比普通请求慢。
const dshSDKInitTimeout = 30 * time.Second

// dshSDKTurnResult 一轮 SDK turn 的汇总。
type dshSDKTurnResult struct {
	Text     string
	Thinking string
	Tools    []ToolCall
	// Provider / Model 本轮实际用的路由与模型（initialize 的回显，非猜测）。
	Provider string
	Model    string
	// SessionID 本轮的 SDK 会话 id（客户端持有）。
	//
	// 用途：常驻会话里 `magic-agent --append <session_id>` 能用它定位到这条会话
	//（会话登记表按 run_id 或 session_id 查找）。
	// ⚠️ 它**不能**当 `-s` 用：跨进程续接拿不到（见本文件头部的「会话续接」说明）。
	SessionID string
}

// dshResolveRoute 决定 initialize 的 (provider, model)。
//
// 取值优先级：req.Model（非空）→ settings.yaml 的 agent-default-model。
// 形态兼容三种写法（ListModels 报的就是第一种）：
//
//	route/model       → (route, model)                     ← ListModels 的输出形态
//	dsh/route/model   → 先剥引擎前缀再按上面拆
//	model             → (配置里的 provider, model)          裸 id 只能在配置路由内解释
//
// 两者都拿不到时返回空串，由调用方判为「SDK 通道不可用」并回退 headless
// （initialize 的 provider/model 是必填项，没有默认值）。
func dshResolveRoute(reqModel string) (string, string) {
	defProvider, defModel := DshConfiguredRoute()
	m := strings.TrimSpace(reqModel)
	if m == "" {
		return defProvider, defModel
	}
	m = strings.TrimPrefix(m, "dsh/") // 先剥可能的引擎前缀
	if p, id, ok := strings.Cut(m, "/"); ok && p != "" && id != "" {
		return p, id
	}
	// 裸 id：只能在配置层的路由内解释（同名模型可能出现在多条路由上）。
	return defProvider, m
}

// ── 协议载荷（只声明本实现用到的字段）────────────────────────────
//
// 字段名与 dsh-sdk-protocol / dsh-llm / dsh-session 的 d.ts 逐一对齐，
// 不按实测样本猜：tool/call 的 callId、tool/result 的 message.source.callId、
// assistant/message 的 message.content[] 都是官方具名结构。

type dshRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type dshRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type dshRPCInbound struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Error  *dshRPCError    `json:"error"`
}

// dshSDKInitParams = InitializeParams。provider/model 必填，maxTokens 可选
// （「限制 SDK 创建的 agent 及其进程内后代的每次对话模型输出」—— 这是 headless
// 完全没有的能力，见 dsh.go 的参数矩阵）。
type dshSDKInitParams struct {
	Cwd       string `json:"cwd"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	MaxTokens int    `json:"maxTokens,omitempty"`
}

// dshSDKTextBlock = TextBlock（ContentBlock 的 text 成员）。
type dshSDKTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type dshSDKPromptParams struct {
	SessionID     string            `json:"sessionId"`
	ContentBlocks []dshSDKTextBlock `json:"contentBlocks"`
}

// dshSDKSessionEvent = SessionEventNotification。
type dshSDKSessionEvent struct {
	SessionID string `json:"sessionId"`
	Event     struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	} `json:"event"`
}

// dshSDKStatusEvent = SessionStatusNotification。
type dshSDKStatusEvent struct {
	SessionID string `json:"sessionId"`
	Status    string `json:"status"`
}

// assistant/message：message.content[] 里混有 reasoning / text / tool-call 块。
// turn / step 用来把正文**按轮分桶**（常驻会话里多轮的事件可能交错到达）。
type dshSDKAssistantMessage struct {
	Turn    int `json:"turn"`
	Step    int `json:"step"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// turn/end：一轮结束（reason.kind 如 completed / max-tokens / error）。
type dshSDKTurnEnd struct {
	Turn   int `json:"turn"`
	Reason struct {
		Kind string `json:"kind"`
	} `json:"reason"`
}

// tool/call：arguments 是模型原样产出的 JSON **字符串**（不是对象）。
type dshSDKToolCall struct {
	CallID    string `json:"callId"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// tool/result：ToolResultMessage = {source:{kind:"tool",callId}, content:[…]}，
// 其中 content[0] 是 ToolResultBlock{type:"tool-result", toolCallId, content:[TextBlock…]}。
// 这里按「两级都可能带文本」来取，避免把结构差异变成空结果。
type dshSDKToolResult struct {
	Message struct {
		Source struct {
			CallID string `json:"callId"`
		} `json:"source"`
		Content []dshSDKToolResultBlock `json:"content"`
	} `json:"message"`
}

type dshSDKToolResultBlock struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	IsError bool   `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// dshSDKTurn 跑一轮 SDK turn。
//
// onEvent 为 nil 时只收集不回调（非流式路径复用同一实现）。返回 errDshSDKUnavailable
// 表示 SDK 通道本身不可用（调用方回退 headless）；其余错误是这一轮真的失败了。
func dshSDKTurn(ctx context.Context, bin, workspace string, req Request, prompt string, onEvent func(StreamEvent)) (dshSDKTurnResult, error) {
	provider, model := dshResolveRoute(req.Model)
	if provider == "" || model == "" {
		return dshSDKTurnResult{}, fmt.Errorf(
			"%w: 读不到 $DSH_HOME/settings.yaml 的 agent-default-model（provider/model 是 initialize 的必填项）",
			errDshSDKUnavailable)
	}

	cmd := newStreamCmdIn(workspace, bin, []string{"--profile", dshSDKProfile})

	/* 三条管道都自建（不直挂 bytes.Buffer / StdinPipe 的 stdout 侧）：
	   沿用 runcmd.go / stream.go 的同一条经验 —— Go 只在目标是 *os.File 时才不做内部
	   拷贝 goroutine，而 cmd.Wait() 会等那些 goroutine 结束；只要 dsh 的 node worker
	   还握着写端，Wait 就永远不返回（症状：引擎已死、magic-agent 挂着不退）。 */
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return dshSDKTurnResult{}, fmt.Errorf("%w: stdout pipe: %v", errDshSDKUnavailable, err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return dshSDKTurnResult{}, fmt.Errorf("%w: stderr pipe: %v", errDshSDKUnavailable, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return dshSDKTurnResult{}, fmt.Errorf("%w: stdin pipe: %v", errDshSDKUnavailable, err)
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		stdin.Close()
		return dshSDKTurnResult{}, fmt.Errorf("%w: start: %v", errDshSDKUnavailable, err)
	}
	notifySpawn(ctx, cmd.Process.Pid) // 会话登记表据此记住引擎子进程 pid（--stop 靠它）
	stdoutW.Close()                   // 父进程不再持有写端；EOF 只取决于子进程一侧
	stderrW.Close()

	// stderr 尾部留 32KB 做错误摘要（与其它流式路径同一个缓冲类型）。
	var stderrBuf streamStderrBuf
	go func() { defer stderrR.Close(); _, _ = io.Copy(&stderrBuf, stderrR) }()

	// turnDone 本轮收尾信号：收尾后读线程**继续把 stdout 读干净**（否则子进程写满管道
	// 会卡住、只能等 stop 的超时杀组），但不再往 inbound 投递。
	// 幂等 —— turn/end、session.status:idle、以及 stop()（ctx 取消/报错退出）都可能先到。
	turnDone := make(chan struct{})
	var closeTurn sync.Once
	markTurnDone := func() { closeTurn.Do(func() { close(turnDone) }) }

	// 读线程：逐行解 JSON-RPC 帧投进 channel；主循环只做 select，避免与 ctx 取消竞争。
	inbound := make(chan dshRPCInbound, 64)
	go func() {
		defer close(inbound)
		defer stdoutR.Close()
		sc := bufio.NewScanner(stdoutR)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tool/result 可能很大
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var msg dshRPCInbound
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue // 格式错误的行按协议要求忽略
			}
			select {
			case inbound <- msg:
			case <-turnDone:
			}
		}
	}()

	// procExit 是进程退出的广播（close 后所有接收方立即返回），供 stop 与主循环共用。
	procExit := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		close(procExit)
	}()

	// stop 幂等收尾：先让读线程转入「只读不投递」（否则它会卡在投递上、stdout 不再被
	// drain），再关 stdin 让 runtime 自己退（协议唯一的「结束」手段），5s 不退就杀整个
	// 进程组（dsh 会起 node worker，只杀父进程会留孤儿）。
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			markTurnDone()
			_ = stdin.Close()
			select {
			case <-procExit:
			case <-time.After(5 * time.Second):
				killProcessGroup(cmd)
				select {
				case <-procExit:
				case <-time.After(5 * time.Second):
				}
			}
		})
	}
	defer stop()

	nextID := 0
	send := func(method string, params any) (int, error) {
		nextID++
		b, err := json.Marshal(dshRPCRequest{JSONRPC: "2.0", ID: nextID, Method: method, Params: params})
		if err != nil {
			return nextID, err
		}
		_, err = stdin.Write(append(b, '\n'))
		return nextID, err
	}

	// ① initialize：握手失败 = SDK 通道不可用（旧版 dsh 没有该 profile 时会走到这里）。
	initParams := dshSDKInitParams{Cwd: workspace, Provider: provider, Model: model, MaxTokens: req.MaxTokens}
	initID, err := send("initialize", initParams)
	if err != nil {
		return dshSDKTurnResult{}, fmt.Errorf("%w: 写 initialize: %v", errDshSDKUnavailable, err)
	}
	initCtx, cancelInit := context.WithTimeout(ctx, dshSDKInitTimeout)
	defer cancelInit()
	if err := waitForResponse(initCtx, inbound, procExit, initID); err != nil {
		var rpcErr *dshRPCErrFrame
		if errors.As(err, &rpcErr) {
			// 服务端明确拒绝（模型没配 / 参数非法）：这是真失败，不回退。
			return dshSDKTurnResult{}, fmt.Errorf(
				"dsh: initialize 被拒绝（provider=%s model=%s）：%s；"+
					"可用的 provider/model 见 --engines 的 models 字段（只有 settings.yaml 里声明过的模型才会被路由接受）",
				provider, model, rpcErr.Message)
		}
		return dshSDKTurnResult{}, fmt.Errorf("%w: initialize: %v%s", errDshSDKUnavailable, err, dshStderrTail(stderrBuf.String()))
	}

	// ② session/prompt：入队即返回 messageId，事件随后异步到达。
	//
	// 常驻会话（req.Append 非 nil）：同一进程内对同一 sessionId 继续 prompt 就是官方
	// 支持的续接方式 —— 官方 Python SDK 文档原文「reuse a harness, home, and id only to
	// continue the same durable conversation」，本引擎探针也验证了第二轮能复述第一轮让
	// 记的数字。所以收到一条追加就**立刻**再排一条（服务器明确支持排队：「同一会话上的
	// 独立请求可以继续排入更多工作」），任务跑着的时候追加也能被接住，不必等当前轮结束。
	//
	// ⚠️ 跨进程续接做不到：服务端 createSession 走的是 `ctx.agents.create`（源码
	// lib/index.js 的 getOrCreateSession / createSession），拿一个已存在的持久 id 会撞
	// SessionAlreadyExistsError（实测 -32603 `session "x" already exists`）。harness 核心
	// 有 `agents.resume`（dsh-agent 类型声明：「Load a persisted session and resume an
	// agent on it」），但 SDK 协议只有 initialize / session/prompt / shutdown 三个方法，
	// 没有暴露它 —— 只有 Web/TUI 那条 host API 用得上 resume。
	sessionID := newDshSessionID()
	out := dshSDKTurnResult{Provider: provider, Model: model, SessionID: sessionID}
	callIndex := map[string]int{}
	promptIDs := map[int]bool{} // 已发出的 prompt 请求 id（用来把它的错误响应挑出来）
	// 正文按 **turn 号分桶**：常驻会话里多轮的事件可能交错到达（第二轮的 prompt 刚排进去，
	// 第一轮的事件还在路上），用单一缓冲区会把两轮正文粘成一段。
	textByTurn := map[int]string{}
	lastTurn := 0
	roundsPending := 0  // 已排入但还没收到 turn/end 的轮数
	roundEnded := false // 当前轮是否已经收到 turn/end（session.status:idle 兜底要用）
	appendCh := req.Append
	appendClosed := appendCh == nil

	emit := func(ev StreamEvent) {
		if onEvent != nil {
			onEvent(ev)
		}
	}
	sendPrompt := func(text string) error {
		id, serr := send("session/prompt", dshSDKPromptParams{
			SessionID:     sessionID,
			ContentBlocks: []dshSDKTextBlock{{Type: "text", Text: text}},
		})
		if serr != nil {
			return serr
		}
		promptIDs[id] = true
		roundsPending++
		roundEnded = false
		return nil
	}
	// finish 收尾：先让读线程转入「只读不投递」，再发 shutdown（协议唯一的「结束」
	// 手段，没有 cancel 方法）。幂等 —— turn/end、session.status:idle、追加通道关闭
	// 三条收尾路径都可能走到。
	finish := func() (dshSDKTurnResult, error) {
		markTurnDone()
		_, _ = send("shutdown", nil)
		return out, nil
	}
	// roundDone 一轮结束：取该轮（turn 号）的正文、报一条 turn_end，并在「追加窗口已关
	// 且没有在跑的轮次」时收尾（常驻会话里 turn/end 只是轮次边界，不是会话终点）。
	roundDone := func(turn int) (dshSDKTurnResult, bool, error) {
		if roundsPending > 0 {
			roundsPending--
		}
		roundEnded = true
		if turn == 0 {
			turn = lastTurn // 载荷没给 turn 号时的兜底
		}
		roundText := textByTurn[turn]
		delete(textByTurn, turn)
		out.Text = roundText
		// 带 SessionID：常驻会话里宿主需要在「这一轮刚做完」时就把会话锚点绑上
		//（与 claude/codebuddy 的 turn_end 同一取舍，见 stream.go 的 StreamEvent.SessionID）。
		emit(StreamEvent{Kind: KindTurnEnd, Text: roundText, SessionID: sessionID})
		if appendClosed && roundsPending == 0 {
			res, err := finish()
			return res, true, err
		}
		return dshSDKTurnResult{}, false, nil
	}
	if err := sendPrompt(prompt); err != nil {
		return dshSDKTurnResult{}, fmt.Errorf("dsh sdk: 写 session/prompt: %v", err)
	}

	// ③ 读事件直到收尾（或进程退出 / ctx 取消）。
	for {
		select {
		case <-ctx.Done():
			return out, fmt.Errorf("dsh sdk: %w", ctx.Err())
		case <-procExit:
			// 进程结束：已经拿到正文就正常收尾，否则报错（stderr 尾部通常就是原因）。
			if out.Text == "" {
				return out, fmt.Errorf("dsh sdk: 进程提前退出（%v）%s", exitErr, dshStderrTail(stderrBuf.String()))
			}
			return out, nil
		case text, ok := <-appendCh:
			// 追加通道（常驻会话）。通道关闭 = 调用方不再追加 → 等在跑的轮次收完就收尾。
			if !ok {
				appendClosed = true
				appendCh = nil // nil channel 在 select 里永久阻塞，不再进这个分支
				if roundsPending == 0 {
					return finish()
				}
				continue
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			if err := sendPrompt(text); err != nil {
				return out, fmt.Errorf("dsh sdk: 写 session/prompt（追加）: %v", err)
			}
		case msg, ok := <-inbound:
			if !ok {
				if out.Text == "" {
					return out, fmt.Errorf("dsh sdk: 进程提前退出%s", dshStderrTail(stderrBuf.String()))
				}
				return out, nil
			}
			// 响应帧：prompt 被拒（内容非法等）时立刻失败，别干等到 ctx 超时。
			if msg.ID != nil {
				if promptIDs[*msg.ID] && msg.Error != nil {
					return out, fmt.Errorf("dsh sdk: session/prompt 被拒绝：%s",
						(&dshRPCErrFrame{Code: msg.Error.Code, Message: msg.Error.Message}).Error())
				}
				continue
			}
			switch msg.Method {
			case "session.event":
				var n dshSDKSessionEvent
				if err := json.Unmarshal(msg.Params, &n); err != nil {
					continue
				}
				// session.event 覆盖「运行时内每个会话」，不止 SDK 建的这一个
				//（协议原文）→ 必须按 sessionId 过滤，否则并行的 TUI/Web 会话
				// 会把它们的工具调用混进本轮结果。
				if n.SessionID != sessionID {
					continue
				}
				switch n.Event.Type {
				case "assistant/message":
					var am dshSDKAssistantMessage
					if err := json.Unmarshal(n.Event.Data, &am); err != nil {
						continue
					}
					for _, blk := range am.Message.Content {
						if blk.Text == "" {
							continue
						}
						turn := am.Turn
						if turn == 0 {
							turn = lastTurn // 载荷没给 turn 号时的兜底
						}
						lastTurn = turn
						switch blk.Type {
						case "text":
							textByTurn[turn] += blk.Text
							out.Text = textByTurn[turn]
							emit(StreamEvent{Kind: KindText, Text: blk.Text})
						case "reasoning":
							// 推理跨轮累加（与 claude/codebuddy 的 acc.Thinking 同语义）。
							out.Thinking += blk.Text
							emit(StreamEvent{Kind: KindThinking, Text: blk.Text})
						}
					}
				case "tool/call":
					var tc dshSDKToolCall
					if err := json.Unmarshal(n.Event.Data, &tc); err != nil {
						continue
					}
					if tc.Name == "" && tc.CallID == "" {
						continue
					}
					emit(StreamEvent{Kind: KindToolUse, Text: tc.Arguments, Name: tc.Name, ID: tc.CallID})
					callIndex[tc.CallID] = len(out.Tools)
					out.Tools = append(out.Tools, ToolCall{Name: tc.Name, ID: tc.CallID, Args: tc.Arguments})
				case "tool/result":
					var tr dshSDKToolResult
					if err := json.Unmarshal(n.Event.Data, &tr); err != nil {
						continue
					}
					text := dshToolResultText(tr)
					emit(StreamEvent{Kind: KindToolResult, Text: text, ID: tr.Message.Source.CallID})
					if i, ok := callIndex[tr.Message.Source.CallID]; ok && out.Tools[i].Result == "" {
						out.Tools[i].Result = text
					}
				case "turn/end":
					var te dshSDKTurnEnd
					_ = json.Unmarshal(n.Event.Data, &te)
					res, done, err := roundDone(te.Turn)
					if done {
						return res, err
					}
				}
				// 其余 event.type（turn/start、step/*、request/*、session/title…）忽略。
			case "session.status":
				var st dshSDKStatusEvent
				// 兜底：某些轮次可能只给 status 不给 turn/end（同样按 sessionId 过滤）。
				//
				// 三个条件缺一不可：
				//   ① 本轮还没收到 turn/end（roundEnded=false）—— 否则这条 idle 只是
				//      一轮结束后的状态回落，当收尾信号会把常驻会话在轮次之间掐掉
				//      （实测踩过：追加通道已关 + 第二轮已排入时，第一轮尾部的 idle
				//      把整个会话结束了，第二轮的事件再也没被消费）；
				//   ② 已经拿到正文；
				//   ③ 追加窗口已关 —— 窗口开着时的 idle 只是「两轮之间」。
				if err := json.Unmarshal(msg.Params, &st); err == nil &&
					st.SessionID == sessionID && st.Status == "idle" &&
					!roundEnded && out.Text != "" && appendClosed {
					res, done, err := roundDone(lastTurn)
					if done {
						return res, err
					}
				}
			}
		}
	}
}

// waitForResponse 等到 id 对应的响应帧；错误响应转成 *dshRPCErrFrame。
func waitForResponse(ctx context.Context, inbound <-chan dshRPCInbound, procExit <-chan struct{}, id int) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-procExit:
			return errDshSDKClosed
		case msg, ok := <-inbound:
			if !ok {
				return errDshSDKClosed
			}
			if msg.ID == nil || *msg.ID != id {
				continue // 握手期间的通知先丢弃（此时还没有会话）
			}
			if msg.Error != nil {
				return &dshRPCErrFrame{Code: msg.Error.Code, Message: msg.Error.Message}
			}
			return nil
		}
	}
}

// dshToolResultText 从 tool/result 载荷里取结果正文。
//
// 取两级：ToolResultBlock 自带的 text（少见）与它嵌套的 content[]（TextBlock，常态）。
// 没有任何文本产出的工具（例如只改了文件）返回空串。
func dshToolResultText(tr dshSDKToolResult) string {
	var b strings.Builder
	for _, blk := range tr.Message.Content {
		if blk.Type == "text" && blk.Text != "" {
			b.WriteString(blk.Text)
		}
		for _, inner := range blk.Content {
			if inner.Type == "text" && inner.Text != "" {
				b.WriteString(inner.Text)
			}
		}
	}
	return b.String()
}

// dshStderrTail 摘 stderr 末尾一行做错误补充（进程起不来时这里就是原因）。
func dshStderrTail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	return ": " + strings.TrimSpace(lines[len(lines)-1])
}

// DshConfiguredRoute 读 $DSH_HOME/settings.yaml 里 agent-default-model 的 (provider, model)。
// SDK 的 initialize 两者都是必填；读不到就只能回退 headless。
func DshConfiguredRoute() (string, string) {
	home := DshHome()
	if home == "" {
		return "", ""
	}
	data, err := os.ReadFile(filepath.Join(home, "settings.yaml"))
	if err != nil {
		return "", ""
	}
	return parseDshDefaultModelRoute(string(data))
}

// newDshSessionID 生成 SDK 侧会话 id。
//
// 必须**每次都是新 id**：session/prompt 的语义是「未知 id 惰性建 agent+session 对」，
// 已存在的 id 会被 -32603 `session "x" already exists` 拒掉（协议无 resume 方法）。
func newDshSessionID() string {
	return fmt.Sprintf("magic-agent-%d", time.Now().UnixNano())
}
