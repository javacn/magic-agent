package agent

// openclaw_acp.go - openclaw 的**流式**通道：以 ACP client 驱动 `openclaw acp`。
//
// 为什么不是 `agent --json`：那条是一次性 envelope —— 正文整段给出，既没有逐字增量，
// 也没有工具调用/思考的事件（见 openclaw.go 文件头）。openclaw 自带的官方流式出口是
// **ACP server**（`openclaw acp`：stdio + JSON-RPC 2.0，背后接本地 Gateway）。
// 它的兼容矩阵（openclaw 2026.6.11 文档 cli/acp.md）写着：
//
//	正文输出 + Tool streaming | Partial | tool_call / tool_call_update 事件带原始输入输出与文本
//	思考流（thought）        | Unsupported | bridge 只发 output text 与 tool status
//
// 所以这一版能拿到：**逐字正文增量 + 工具卡**；思考增量拿不到（协议侧就没有，不是解析缺陷）。
//
// 事件映射（session/update 通知 → 本包的 StreamEvent）：
//
//	agent_message_chunk → KindText          （content.text，逐字）
//	agent_thought_chunk → KindThinking      （桥当前不发；留着，将来支持时零改动接上）
//	tool_call           → KindToolUse       （name=title/kind，id=toolCallId，args=rawInput）
//	tool_call_update    → KindToolResult    （status=completed/failed 时；text=内容或 rawOutput）
//	其余（plan / usage_update / session_info_update …）→ 忽略
//
// 会话续接：锚点是 Gateway 的 **session key**（不是 ACP 的 sessionId —— 那个由桥每次随机生成，
// 出了进程就没意义）。做法：`session/new` 的 `_meta.sessionKey` 传我们自己的 key，
// 桥的 resolveSessionKey 逻辑（dist/acp-cli）见到 `meta.sessionKey` 就原样采用。
//   - 新会话：`agent:<agentID>:acp-<uuid>`（与桥的 fallback `acp-bridge:<uuid>` 同一形态）
//   - 续接  ：把上次返回的 key 原样再传一次 → 同一个 Gateway 会话，上下文延续
//   - ⚠️ 老版本返回的是 Gateway **session id**（裸 uuid，非流式路径给的）→ 不能当 key 用，
//     这里用 `openclaw sessions --json` 反查它对应的 key 再续接；查不到就新开并在 stderr 说明。
//
// 回退（很重要）：流式通道多一个「Gateway 必须在跑 + ACP 桥的 scope 已批」的外部依赖。
// 桥起不来时**自动回退到内嵌一次性调用**（= 原来的 Complete 行为），并在 stderr 留一行说明 ——
// 「装了 openclaw 就能说话」不该被流式通道的依赖打断；代价只是那一轮没有逐字增量。
//
// ⚠️ 两条**主动改走嵌套路**（不流式）的情形，理由都是「宁可少一个增量，也不要静默改变语义」：
//  1. 本轮**指定了模型**（`-m`）：ACP 不暴露模型选择（见下），嵌套路才能忠实生效；
//  2. 桥不可用（Gateway 没起 / scope 未批）—— 见上。
//
// 模型：ACP 桥当前**不支持按轮指定模型**（兼容矩阵：Model selection 尚未作为 ACP config 暴露），
// 会话用的是 Gateway 侧的默认/钉住模型。所以流式那一轮 `result.model` 只能是**主选兜底**（可能为空），
// 真正用的模型以会话侧为准 —— 这件事在 README 里也点明，别让界面把一个猜的值当真。
//
// exec 审批（session/request_permission）：桥会把「需要用户拍板」的请求转给 ACP client。
// 默认取**保守**策略（见 acpPermissionDecision）：只放行读/搜/取/思考类，改动与执行类一律拒绝
// 并在 stderr 说明；要全放行用 MAGIC_AGENT_OPENCLAW_ACP_APPROVE=all。
// ⚠️ openclaw 自己的 exec-policy / allowlist 仍是第一道闸门，这里只是 ACP 这一层的答复。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

/* ────────────── ACP 线格式（JSON-RPC 2.0，NDJSON 一行一帧） ────────────── */

const (
	// acpProtocolVersion 取 ACP SDK 0.15 的协议版本（initialize 回应里应回同一值）。
	acpProtocolVersion = 1
	// acpKeyPrefix 自造 session key 的前缀：形态与 openclaw 的 session key 一致
	//（`agent:<agentId>:<rest>`），agentId 用本引擎固定使用的那个（默认 main）。
	acpKeyPrefix = "agent:%s:acp-"
	// acpKeyHexLen 随机 key 的十六进制长度（16 字节 = 32 字符，与 uuid 同量级）。
	acpKeyHexLen = 32
	// acpMaxLine 单行上限（tool_call 的 rawInput/rawOutput 可能很大）。
	acpMaxLine = 16 * 1024 * 1024
)

// acpError 是 JSON-RPC 的错误对象。
type acpError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *acpError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("ACP 错误 %d: %s (%s)", e.Code, e.Message, truncateForErr(string(e.Data), 200))
	}
	return fmt.Sprintf("ACP 错误 %d: %s", e.Code, e.Message)
}

// acpFrame 一帧（请求 / 响应 / 通知共用）。
type acpFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpError       `json:"error,omitempty"`
}

// acpUpdate 是 session/update 通知里的 update 载荷（各类型字段的并集，按需取用）。
type acpUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	// agent_message_chunk / agent_thought_chunk / user_message_chunk：content 是 ContentBlock
	Content json.RawMessage `json:"content"`
	// tool_call / tool_call_update
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
}

// acpContentBlock 是 content 字段的正文形态（只关心 text）。
type acpContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

/* ────────────── Stream：ACP 主流程 ────────────── */

// openClawACPUnavailable 标记「流式通道本身不可用」（Gateway 没起 / scope 未批 / 命令缺失），
// 这一类失败可以安全回退到内嵌一次性调用；其余的协议/业务失败照实返回。
type openClawACPUnavailable struct{ why string }

func (e *openClawACPUnavailable) Error() string { return e.why }

// acpBreaker：桥不可用后的**跨进程冷却**（落一个标记文件）。
//
// 为什么必须跨进程：上层（观物台）是**一轮一个 CLI 进程**，进程内的冷却对下一轮毫无意义 ——
// 实测那样写两轮都会照旧 spawn 桥（各多花 ~7s，还在 Gateway 侧各留一条 pending 请求）。
// 所以把「到什么时候为止别再试」写进文件：冷却期内直接走嵌套路，到点自动再试一次
// （用户批完 scope 后最多等这么久就恢复流式；想立刻恢复删掉该文件即可）。
//
// 文件：`~/.magic-agent/openclaw-acp-broken.json`（`MAGIC_AGENT_OPENCLAW_ACP_BREAKER` 可改路径，
// 验收脚本用它指向临时文件）。写不进去就退化成「没有冷却」——这类优化不该让调用失败。
const acpBreakerCooldown = 5 * time.Minute

// StreamAvailable 报告某引擎的流式通道**此刻**是否可用。
//
// 与 AsStreamer 的区别：后者是编译期能力（有没有实现 Streamer），前者是运行期可用性。
// openclaw 的流式走 ACP 桥，而桥要连本机 Gateway、Gateway 对它的授权（scope）是**外部状态**
// —— 用户没批的时候桥连不上，`--stream` 只会白等一次失败再回退（实测 ~7s）。
// 所以把「已知不可用」的冷却状态算进来，让调用方（掌天瓶）直接不传 `--stream`、
// 界面也不显示流式标记；冷却到期自动恢复（真连上了就一直是 true）。
//
// 注意：判据是**已知失败**而不是「主动探测」—— 探测一次要 ~7s，放进 `--engines` 会拖慢每次
// 刷新。所以冷却是「失败后写文件、跨进程生效」的形态：没批时最多多试一次，之后就不再宣称可用。
func StreamAvailable(engineName string) bool {
	switch engineName {
	case "openclaw":
		return !acpBreakerCooling()
	}
	return true
}

// acpBreakerPath 冷却标记文件路径（空串 = 用不了文件，退化成不冷却）。
func acpBreakerPath() string {
	if p := strings.TrimSpace(os.Getenv("MAGIC_AGENT_OPENCLAW_ACP_BREAKER")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".magic-agent", "openclaw-acp-broken.json")
}

// acpBreakerCooling 现在是否处于冷却期（标记文件里的 until 还没到）。
func acpBreakerCooling() bool {
	p := acpBreakerPath()
	if p == "" {
		return false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	var st struct {
		Until int64 `json:"until"`
	}
	if json.Unmarshal(b, &st) != nil || st.Until == 0 {
		return false
	}
	return time.Now().Unix() < st.Until
}

// markACPBridgeDown 落一次冷却（写失败就静默放弃：只是优化，不是功能）。
func markACPBridgeDown() {
	p := acpBreakerPath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	b, _ := json.Marshal(map[string]any{
		"until": time.Now().Add(acpBreakerCooldown).Unix(),
		"why":   "openclaw ACP 桥不可用（Gateway 没起 / scope 未批 / 命令缺失）",
	})
	_ = os.WriteFile(p, b, 0o644)
}

// Stream 实现 Streamer：以 ACP client 驱动 `openclaw acp`（见文件头）。
func (e *OpenClawEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return StreamResult{}, fmt.Errorf("openclaw CLI not found; set MAGIC_AGENT_OPENCLAW_BIN")
	}
	// prompt 与非流式路径**同源**：同一次扁平化 + 同一段附件说明（image 走 ACP 原生块，见下）。
	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, toolsIsOff(req))
	if prompt == "" {
		return StreamResult{}, fmt.Errorf("openclaw: empty prompt")
	}

	/* ⚠️ ACP 通道**不能指定模型**：桥不暴露模型选择（兼容矩阵原文「Model selection … not yet
	   exposed as ACP config options」），会话用 Gateway 侧的默认/钉住模型。而 `-m` 在嵌套路里是
	   **忠实生效**的（`openclaw agent --model`，实测换成 minimax 就走 minimax 的鉴权）。
	   所以「本轮指定了模型」时宁可不流式，也不要把用户选的模型静默换掉 —— 直接走嵌套路，
	   并在 stderr 说明（与「指定模型」这件事的优先级一致：模型正确 > 逐字增量）。 */
	if m := stripModelPrefix(req.Model); m != "" {
		fmt.Fprintf(os.Stderr, "magic-agent: openclaw 指定了模型（%s），本轮改走非流式（ACP 通道不支持按轮指定模型）\n", m)
		return e.completeAsStream(ctx, req, onEvent, start)
	}

	textPart, imageBlocks, attNote := e.acpPromptParts(prompt, req)
	if attNote != "" {
		fmt.Fprintln(os.Stderr, "magic-agent: "+attNote)
	}

	// 桥刚被判过不可用 → 冷却期内直接走嵌套路（不再 spawn：省 7s，也不再刷 pending 请求）。
	if acpBreakerCooling() {
		fmt.Fprintf(os.Stderr, "magic-agent: openclaw 流式通道（ACP）本轮跳过（上次判定不可用，%s 内不再重试）→ 走内嵌一次性调用\n",
			acpBreakerCooldown)
		return e.completeAsStream(ctx, req, onEvent, start)
	}

	key, keyNote := e.resolveACPSessionKey(ctx, bin, req.SessionID)
	if keyNote != "" {
		fmt.Fprintln(os.Stderr, "magic-agent: "+keyNote)
	}

	st, err := e.streamACPSession(ctx, bin, key, textPart, imageBlocks, onEvent)
	if err == nil {
		return e.acpStreamResult(st, req, key, start), nil
	}

	// 收尾：ACP 通道不可用 → 回退内嵌一次性调用（老行为），并把正文补发成一条 text 增量。
	if _, ok := err.(*openClawACPUnavailable); !ok {
		return StreamResult{}, err
	}
	markACPBridgeDown()
	fmt.Fprintf(os.Stderr, "magic-agent: openclaw 流式通道（ACP）不可用，已回退内嵌一次性调用（本轮无逐字增量）：%v\n", err)
	return e.completeAsStream(ctx, req, onEvent, start)
}

// completeAsStream 走内嵌一次性调用（Complete），并把结果包装成「流式形态」：
// 正文补发成**一条 text 增量**，调用方（对话渲染）看到的仍是「有正文的一轮」，只是没有逐字。
// 用途：① ACP 桥不可用时的回退；② 本轮指定了模型（ACP 不支持下，见 Stream）。
func (e *OpenClawEngine) completeAsStream(
	ctx context.Context, req Request, onEvent func(StreamEvent), start time.Time,
) (StreamResult, error) {
	resp, err := e.Complete(ctx, req)
	if err != nil {
		return StreamResult{}, err
	}
	if onEvent != nil && resp.Text != "" {
		onEvent(StreamEvent{Kind: KindText, Text: resp.Text})
	}
	return StreamResult{
		Response: Response{
			Engine: e.Name(), Text: resp.Text, Model: resp.Model, SessionID: resp.SessionID,
			InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens, TotalTokens: resp.TotalTokens,
			Latency: time.Since(start), Attempts: 1,
		},
	}, nil
}

// acpStreamResult 把 ACP 会话的累积状态收成 StreamResult。
// 会话锚点用 **key**（不是桥随机生成的 ACP sessionId）：调用方下一轮把它传回 `--session`
// 即续接同一 Gateway 会话（见文件头「会话续接」）。
func (e *OpenClawEngine) acpStreamResult(st *acpStreamState, req Request, key string, start time.Time) StreamResult {
	text := strings.TrimSpace(st.text.String())
	if text == "" {
		text = strings.TrimSpace(st.lastText)
	}
	model := strings.TrimSpace(st.model)
	if model == "" {
		model = stripModelPrefix(req.Model)
	}
	in, out := st.usage.Input+st.usage.CacheRead, st.usage.Output
	return StreamResult{
		Response: Response{
			Engine: e.Name(), Text: text, Model: model, SessionID: key,
			InputTokens: in, OutputTokens: out, TotalTokens: in + out,
			Latency: time.Since(start), Attempts: 1,
		},
		Thinking: strings.TrimSpace(st.thinking.String()),
		Tools:    st.tools,
	}
}

/* ────────────── 会话锚点 ────────────── */

// resolveACPSessionKey 决定本轮用哪个 Gateway session key，以及要额外说明什么（可为空）。
func (e *OpenClawEngine) resolveACPSessionKey(ctx context.Context, bin, sid string) (key, note string) {
	s := strings.TrimSpace(sid)
	if s == "" {
		return e.newACPSessionKey(), ""
	}
	if strings.Contains(s, ":") {
		return s, "" // 已经是 key 形态（本引擎流式路径返回的就是它）
	}
	// 裸 uuid = 老的非流式路径返回的 Gateway **session id**：反查它对应的 key 再续接，
	// 这样升级前后的同一轮对话上下文不会丢。
	if k, err := e.lookupSessionKey(ctx, bin, s); err == nil && k != "" {
		return k, ""
	}
	return e.newACPSessionKey(), fmt.Sprintf("上一轮的会话 id %q 是内嵌路径给的（映射不到 ACP 会话 key），本轮新开会话", s)
}

// newACPSessionKey 造一个新的 session key。
func (e *OpenClawEngine) newACPSessionKey() string {
	buf := make([]byte, acpKeyHexLen/2)
	if _, err := rand.Read(buf); err != nil {
		// 熵源异常极罕见；退回时间戳，保证 key 仍然唯一（宁可继续，也不要因为拿不到随机数就失败）。
		return fmt.Sprintf(acpKeyPrefix+fmt.Sprintf("%x", time.Now().UnixNano()), e.agentID())
	}
	return fmt.Sprintf(acpKeyPrefix+hex.EncodeToString(buf), e.agentID())
}

// lookupSessionKey 用 `openclaw sessions --json` 把 Gateway session id 反查成 session key。
// 复用与 lookupLatestSessionID 同一套解析（抗 banner / 日志混入）。
func (e *OpenClawEngine) lookupSessionKey(ctx context.Context, bin, sessionID string) (string, error) {
	args := []string{"sessions", "--json", "--limit", "200"}
	if a := e.agentID(); a != "" {
		args = append(args, "--agent", a)
	}
	stdout, _, err := runCLI(ctx, bin, args...)
	if err != nil {
		return "", err
	}
	var env openclawSessionsEnvelope
	if jerr := json.Unmarshal([]byte(stdout), &env); jerr != nil {
		obj, ok := firstJSONObject(stdout)
		if !ok {
			return "", fmt.Errorf("openclaw sessions: 未找到 JSON envelope")
		}
		_ = json.Unmarshal(mustMarshal(obj), &env)
	}
	for _, s := range env.Sessions {
		if s.SessionID == sessionID && s.Key != "" {
			return s.Key, nil
		}
	}
	return "", nil
}

/* ────────────── ACP 会话：spawn + 握手 + 事件循环 ────────────── */

// acpStreamState 一次 ACP 流式调用的累积状态。
type acpStreamState struct {
	onEvent  func(StreamEvent)
	text     strings.Builder
	thinking strings.Builder
	lastText string // result 行（若有）给的权威正文
	model    string
	usage    openclawUsage

	tools      []ToolCall
	toolIndex  map[string]int  // toolCallId → tools 下标
	resultSeen map[string]bool // 已发过 tool_result 的 toolCallId（桥会推多条，见 handleNotification）

	sessionID  string // 桥给的 ACP sessionId（仅本进程内用于 prompt/cancel）
	stopReason string

	emitted  bool // 是否已经吐出过增量（回退判定用）
	acpReady bool // 桥是否已连上（handshake 有响应）
}

func (st *acpStreamState) emit(ev StreamEvent) {
	switch ev.Kind {
	case KindText:
		st.text.WriteString(ev.Text)
	case KindThinking:
		st.thinking.WriteString(ev.Text)
	}
	if ev.Kind == KindText || ev.Kind == KindThinking || ev.Kind == KindToolUse || ev.Kind == KindToolResult {
		st.emitted = true
	}
	if st.onEvent != nil {
		st.onEvent(ev)
	}
}

// acpPermissionPolicy 返回对一次 exec 审批请求的答复策略：
//
//	"allow"  选第一个 allow_* 选项
//	"reject" 选第一个 reject_* 选项（拿不到就回 cancelled）
//
// 默认保守：只放行读/搜/取/思考/其它类（对应 ACP ToolKind: read/search/fetch/think/other），
// edit/delete/move/execute 一律拒绝 —— 与「不替用户扩大权限」一致。
// MAGIC_AGENT_OPENCLAW_ACP_APPROVE=all 时全放行（等价 `openclaw acp client --approve-all`）。
func acpPermissionPolicy(kind string) string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("MAGIC_AGENT_OPENCLAW_ACP_APPROVE")), "all") {
		return "allow"
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "read", "search", "fetch", "think", "other", "":
		return "allow"
	default: // edit / delete / move / execute
		return "reject"
	}
}

// streamACPSession 起桥、跑完一轮，返回累积状态。失败时若判定为「通道不可用」，
// 返回 *openClawACPUnavailable（见 Stream 的回退）。
func (e *OpenClawEngine) streamACPSession(
	ctx context.Context, bin, key, text string, images []acpImageBlock, onEvent func(StreamEvent),
) (*acpStreamState, error) {
	args := []string{"--log-level", "silent", "acp", "--no-prefix-cwd"}
	cmd := newStreamCmdIn("", bin, args)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("openclaw acp: stdin pipe: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("openclaw acp: stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("openclaw acp: stderr pipe: %w", err)
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		// 连可执行文件都起不来（ENOENT / 权限）→ 属于通道不可用，回退。
		return nil, &openClawACPUnavailable{why: fmt.Sprintf("启动 %s 失败: %v", bin, err)}
	}
	notifySpawn(ctx, cmd.Process.Pid)
	stdoutW.Close() // 父进程不持写端：EOF 只取决于子进程一侧
	stderrW.Close()

	stderrTail := &streamStderrBuf{}
	go func() { defer stderrR.Close(); _, _ = io.Copy(stderrTail, stderrR) }()

	st := &acpStreamState{onEvent: onEvent, toolIndex: map[string]int{}, resultSeen: map[string]bool{}}

	/* 写端串行化：ACP 的每一条请求/应答都必须整帧写出（不能交错），
	   所以所有写都走这一条通道，由唯一一个 goroutine 落笔。
	   ⚠️ 关闭用 stopped 通道**而不是 close(writes)**：取消那条 goroutine 也可能在写，
	   往已关闭的通道发会 panic（第一版就是这么写的，编译过了但一取消就崩）。 */
	writes := make(chan []byte, 32)
	stopped := make(chan struct{})
	var stopOnce sync.Once
	stopWriter := func() { stopOnce.Do(func() { close(stopped) }) }
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopped:
				return
			case b := <-writes:
				if _, werr := stdin.Write(b); werr != nil {
					return
				}
			}
		}
	}()
	send := func(v any) {
		b, merr := json.Marshal(v)
		if merr != nil {
			return
		}
		select {
		case <-stopped:
			return
		case writes <- append(b, '\n'):
		default: // 通道满 = 下游堵住（罕见）；丢掉比死锁好
		}
	}
	sendReq := func(id int, method string, params any) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	}

	// 取消：ACP 有 session/cancel，先礼后兵（发完再杀整组，与其他引擎的杀组语义一致）。
	go func() {
		<-ctx.Done()
		if st.sessionID != "" {
			send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel",
				"params": map[string]any{"sessionId": st.sessionID}})
			time.Sleep(200 * time.Millisecond) // 给桥一点时间优雅收尾
		}
		killProcessGroup(cmd)
	}()

	// 握手：initialize → session/new(_meta.sessionKey) → session/prompt。
	const (
		idInit   = 1
		idNew    = 2
		idPrompt = 3
	)
	promptSent := false
	promptDone := make(chan error, 1)
	finish := func(err error) {
		select {
		case promptDone <- err:
		default:
		}
	}

	scanErr := make(chan error, 1)
	go func() {
		defer stdoutR.Close()
		sc := bufio.NewScanner(stdoutR)
		sc.Buffer(make([]byte, 0, 64*1024), acpMaxLine)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || line[0] != '{' {
				continue // 桥的 banner / 日志（`◇ Doctor warnings` 那类）会混进 stdout：跳过
			}
			var f acpFrame
			if jerr := json.Unmarshal([]byte(line), &f); jerr != nil {
				continue
			}
			if f.Method != "" && len(f.ID) == 0 { // 通知
				st.handleNotification(f)
				continue
			}
			if f.Method != "" { // server→client 请求（要应答，否则桥会等）
				st.handleServerRequest(f, send)
				continue
			}
			// 响应
			if f.Error != nil {
				finish(f.Error)
				return
			}
			switch asInt(f.ID) {
			case idInit:
				st.acpReady = true
				if !promptSent {
					params := map[string]any{
						"cwd":        e.acpCwd(),
						"mcpServers": []any{},
						"_meta":      map[string]any{"sessionKey": key},
					}
					// cwd 交给会话（桥按它为 session 记工作目录）；prompt 不再重复前缀（--no-prefix-cwd）。
					sendReq(idNew, "session/new", params)
				}
			case idNew:
				var r struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(f.Result, &r)
				if r.SessionID == "" {
					finish(fmt.Errorf("openclaw acp: session/new 未返回 sessionId"))
					return
				}
				st.sessionID = r.SessionID
				blocks := []any{map[string]any{"type": "text", "text": text}}
				for _, im := range images {
					blocks = append(blocks, map[string]any{
						"type": "image", "data": im.Data, "mimeType": im.MIME,
					})
				}
				sendReq(idPrompt, "session/prompt", map[string]any{
					"sessionId": r.SessionID, "prompt": blocks,
				})
				promptSent = true
			case idPrompt:
				var r struct {
					StopReason string `json:"stopReason"`
				}
				_ = json.Unmarshal(f.Result, &r)
				st.stopReason = r.StopReason
				finish(nil)
				return
			default:
				// 其余响应（我们没发过的 id）忽略。
			}
		}
		if serr := sc.Err(); serr != nil && serr != io.ErrClosedPipe {
			scanErr <- serr
		}
	}()

	// 发 initialize（协议版本取 SDK 0.15 同一值；clientCapabilities 里把不支持的都声明 false，
	// 桥的兼容矩阵也写明它不调用 client 的 fs/* 与 terminal/*）。
	sendReq(idInit, "initialize", map[string]any{
		"protocolVersion": acpProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false},
		},
	})

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	var runErr error
	select {
	case err := <-promptDone:
		runErr = err
	case err := <-scanErr:
		runErr = fmt.Errorf("openclaw acp: 读 stdout 失败: %w", err)
	case werr := <-waitErr:
		// 进程先退（桥连不上 / 握手失败都会走这里）：把 stderr 的原文带出来。
		runErr = fmt.Errorf("openclaw acp 进程退出: %v（stderr: %s）", werr, truncateForErr(stderrTail.String(), 400))
	case <-ctx.Done():
		runErr = fmt.Errorf("process group killed: %w", ctx.Err())
	}

	// 收尾：停写端（让桥收尾）→ 杀组兜底 → 等写协程与进程回收。
	stopWriter()
	if runErr == nil {
		// 正常收尾：桥在一轮结束后通常自己退出（一次性调用）；给一小段时间，超时再杀。
		select {
		case <-waitErr:
		case <-time.After(5 * time.Second):
			_ = stdin.Close()
			killProcessGroup(cmd)
		}
	} else {
		_ = stdin.Close()
		killProcessGroup(cmd)
		select {
		case <-waitErr:
		case <-time.After(5 * time.Second):
		}
	}
	wg.Wait()

	if runErr != nil {
		// 通道不可用（Gateway 没起 / scope 未批 / 二进制缺失）→ 交给 Stream 回退。
		if !st.emitted && isACPBridgeUnavailable(stderrTail.String(), runErr) {
			return nil, &openClawACPUnavailable{why: firstLine(runErr.Error())}
		}
		return nil, runErr
	}
	if st.stopReason == "cancelled" {
		return nil, fmt.Errorf("本轮被取消（stopReason=cancelled）")
	}
	if strings.TrimSpace(st.text.String()) == "" && strings.TrimSpace(st.lastText) == "" {
		return nil, fmt.Errorf("openclaw acp 返回空正文（stopReason=%s）", st.stopReason)
	}
	return st, nil
}

// handleNotification 处理桥推来的会话通知（session/update）。
func (st *acpStreamState) handleNotification(f acpFrame) {
	if f.Method != "session/update" {
		return
	}
	var p struct {
		Update acpUpdate `json:"update"`
	}
	if err := json.Unmarshal(f.Params, &p); err != nil {
		return
	}
	u := p.Update
	switch u.SessionUpdate {
	case "agent_message_chunk":
		if txt := acpTextOf(u.Content); txt != "" {
			st.emit(StreamEvent{Kind: KindText, Text: txt})
		}
	case "agent_thought_chunk":
		// 桥当前不发（兼容矩阵：thought streaming unsupported）；留着以备将来支持。
		if txt := acpTextOf(u.Content); txt != "" {
			st.emit(StreamEvent{Kind: KindThinking, Text: txt})
		}
	case "tool_call":
		/* 工具名：优先 `kind`（read / execute / edit …，短且稳定），其次 title。
		   ⚠️ 别直接用 title —— openclaw 的 title 常是整条命令（实测「exec: command: echo hi」），
		   当卡片标题会很长且每次都变；命令本身已经在 rawInput（= 事件里的 args）里了。 */
		name := firstNonEmpty(u.Kind, u.Title, "tool")
		args := jsonText(u.RawInput)
		st.emit(StreamEvent{Kind: KindToolUse, Text: args, Name: name, ID: u.ToolCallID})
		if u.ToolCallID != "" {
			st.toolIndex[u.ToolCallID] = len(st.tools)
			st.tools = append(st.tools, ToolCall{Name: name, ID: u.ToolCallID, Args: args})
		}
	case "tool_call_update":
		/* 只把「有产出」的更新当结果（pending / in_progress 是状态变化，不是一次产出），
		   并且**每个 toolCallId 只发一次**：桥对同一次调用会推多条带内容的更新
		   （实测同一次 exec 收到 content 的「hi\n」与 rawOutput 的「hi」两条）——
		   不去重就会在界面上出两张重复的工具卡。 */
		if u.ToolCallID != "" && st.resultSeen[u.ToolCallID] {
			return
		}
		text := acpToolResultText(u.Content, u.RawOutput)
		if text == "" {
			return
		}
		if u.ToolCallID != "" {
			st.resultSeen[u.ToolCallID] = true
		}
		st.emit(StreamEvent{Kind: KindToolResult, Text: text, ID: u.ToolCallID})
		if i, ok := st.toolIndex[u.ToolCallID]; ok {
			if st.tools[i].Result == "" {
				st.tools[i].Result = text
			}
		} else if u.ToolCallID != "" {
			st.tools = append(st.tools, ToolCall{Name: firstNonEmpty(u.Kind, u.Title), ID: u.ToolCallID, Result: text})
		}
	default:
		// plan / available_commands_update / current_mode_update / usage_update / user_message_chunk …
		// 一律忽略：本引擎不消费它们（usage 从收尾 result 里也没有，token 计数保持 0）。
	}
}

// handleServerRequest 应答 server→client 请求。
// 目前只有 session/request_permission（exec 审批）；其余一律回空 result，避免桥等我们。
func (st *acpStreamState) handleServerRequest(f acpFrame, send func(any)) {
	var id any
	if len(f.ID) > 0 {
		_ = json.Unmarshal(f.ID, &id)
	}
	if f.Method != "session/request_permission" {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		return
	}
	var p struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(f.Params, &p)
	want := acpPermissionPolicy(p.ToolCall.Kind)
	pick := ""
	for _, o := range p.Options {
		if want == "allow" && strings.HasPrefix(o.Kind, "allow") {
			pick = o.OptionID
			break
		}
		if want == "reject" && strings.HasPrefix(o.Kind, "reject") {
			pick = o.OptionID
			break
		}
	}
	if pick == "" {
		send(map[string]any{"jsonrpc": "2.0", "id": id,
			"result": map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}})
		return
	}
	// 审批是**安全相关**的一件事：拒绝的原因必须让人看得见（不许静默）。
	if want == "reject" {
		fmt.Fprintf(os.Stderr, "magic-agent: openclaw 请求执行「%s」(kind=%s)，按保守策略已拒绝（要放行设 MAGIC_AGENT_OPENCLAW_ACP_APPROVE=all）\n",
			firstNonEmpty(p.ToolCall.Title, "(未命名)"), firstNonEmpty(p.ToolCall.Kind, "?"))
	}
	send(map[string]any{"jsonrpc": "2.0", "id": id,
		"result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": pick}}})
}

/* ────────────── 小工具 ────────────── */

// acpCwd 本轮会话的工作目录（`session/new` 的 cwd）：桥按它把会话记到对应工作目录。
// 取当前进程 cwd —— CLI 层已按调用方的 workspace 切过目录（与本包其它引擎的子进程 cwd 语义一致）；
// 拿不到（极少见）退到 "/"：cwd 只是给会话做标记，写错不该让整轮失败。
func (e *OpenClawEngine) acpCwd() string {
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd
	}
	return "/"
}

// acpImageBlock 一张随提示词发出的图片（ACP 原生 image 内容块）。
type acpImageBlock struct {
	Data string
	MIME string
}

// acpPromptParts 拆出提示词的文本部分与图片块。
// 图片能读成 ACP 原生块就原生发（模型真能看到图）；读不了的附件退化成提示词里的绝对路径
// （= 今天的行为），并把原因回给调用方打一行 stderr。
func (e *OpenClawEngine) acpPromptParts(prompt string, req Request) (string, []acpImageBlock, string) {
	if len(req.Attachments) == 0 {
		return prompt, nil, ""
	}
	var images []acpImageBlock
	var asPath []Attachment
	for _, a := range req.Attachments {
		if !a.IsImage() {
			asPath = append(asPath, a)
			continue
		}
		data, err := os.ReadFile(a.Path)
		if err != nil {
			asPath = append(asPath, a)
			continue
		}
		images = append(images, acpImageBlock{
			Data: base64.StdEncoding.EncodeToString(data),
			MIME: a.MIME,
		})
	}
	text := prompt
	note := ""
	if len(asPath) > 0 {
		text = appendAttachmentSection(prompt, asPath)
		note = fmt.Sprintf("%d 个附件无法作为图片发出，已退化为提示词里的路径（模型看不到图）", len(asPath))
	}
	return text, images, note
}

// acpTextOf 从 ContentBlock 里取文本（非 text 类型返回空串）。
func acpTextOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var b acpContentBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		return ""
	}
	if b.Type != "text" && b.Type != "" {
		return ""
	}
	return b.Text
}

// acpToolResultText 从 tool_call_update 的 content / rawOutput 里取可读产出。
// content 是 ToolCallContent 数组（每项 {type, content:{text}}），rawOutput 是工具原始输出。
func acpToolResultText(content json.RawMessage, rawOutput json.RawMessage) string {
	var sb strings.Builder
	if len(content) > 0 {
		var arr []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(content, &arr); err == nil {
			for _, c := range arr {
				if t := acpTextOf(c.Content); t != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(t)
				}
			}
		}
	}
	if sb.Len() == 0 && len(rawOutput) > 0 {
		return jsonText(rawOutput)
	}
	return sb.String()
}

// jsonText 把任意 JSON 值压成可读文本（字符串原样，其余紧凑 JSON）。
func jsonText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return buf.String()
	}
	return string(raw)
}

// asInt 把 JSON-RPC 的 id（可能是数字或字符串）转成 int（认不出返回 -1）。
func asInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return -1
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var v int
		if _, err := fmt.Sscanf(s, "%d", &v); err == nil {
			return v
		}
	}
	return -1
}

// isACPBridgeUnavailable 判定「流式通道本身不可用」（可安全回退内嵌路径）。
// 依据：桥把 Gateway 连接错误原样打到 stderr —— 实测原文
// 「gateway connect failed: GatewayClientRequestError: scope upgrade pending approval」/
// 「ACP bridge failed: …」；另外 ACP 命令不存在（老版本 openclaw）也在这类。
func isACPBridgeUnavailable(stderr string, err error) bool {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	hay := strings.ToLower(stderr + "\n" + msg)
	for _, s := range []string{
		"gateway connect failed",
		"acp bridge failed",
		"scope upgrade pending approval",
		"not paired",
		"unknown command",
		"unknown option",
		"no such file or directory",
		"executable file not found",
	} {
		if strings.Contains(hay, s) {
			return true
		}
	}
	return false
}

// firstLine 取多行文本的第一行（stderr 摘要用）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// truncateForErr 截断长文本（错误信息里只留一段，避免刷屏）。
func truncateForErr(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " ⏎ "))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// (streamStderrBuf.Tail 见 runcmd.go：这里只用到它的尾部摘要能力)
