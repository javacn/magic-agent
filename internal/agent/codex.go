package agent

// codex.go - Codex CLI 引擎。
//
// 后端：codex（@openai/codex，本机 /opt/homebrew/bin/codex）。
// 非交互模式：`codex exec`，协议面：
//
//	--json            NDJSON 事件流，stdout 逐行 {"type":"...","...":"..."}
//	-o, --output-last-message <FILE>
//	                  最终 assistant 正文落盘；也用于 --output-schema 时取
//	                  schema 化的 JSON（可能被 ```json fences 包裹）
//	--output-schema <FILE>
//	                  原生 JSONSchema 约束（首选）
//	--skip-git-repo-check
//	                  允许在非 git 目录跑（默认外层 cwd 不一定在 git 仓库内）
//	--ephemeral       不落盘 session（仅当不需要续接时使用 —— 与 SessionID 互斥）
//	-m, --model <m>   模型
//	-c, --config k=v  TOML 配置覆写，dotted path
//	-s, --sandbox <MODE>
//	                  read-only / workspace-write / danger-full-access
//	--dangerously-bypass-approvals-and-sandbox
//	                  全放行（off 模式不要用，会与「禁用工具」冲突）
//
// 关键事件：
//
//	{"type":"thread.started","thread_id":"<uuid>"}            — 拿到会话 id
//	{"type":"turn.started"}
//	{"type":"item.completed","item":{"type":"agent_message","text":"..."}}
//	{"type":"turn.completed","usage":{
//	   "input_tokens":...,"cached_input_tokens":...,
//	   "output_tokens":...,"reasoning_output_tokens":...}}
//
// 续接：`codex exec resume <thread_id>`（指定 id）或 `codex exec resume --last`
//（接最近一次）。 Continue → resume --last；SessionID 非空 → resume <id>。
//
// SystemPrompt 注入：`-c developer_instructions=<text>`。其值解析为 TOML，
// 因此双引号、反斜杠、换行须用 json.Marshal 转义（json 的转义集与 TOML basic
// string 完全兼容，已实测）。
//
// MaxTokens / Temperature 静默忽略 —— codex 无 `--max-tokens` / `--temperature`
// 原生 flag；`-c model_max_output_tokens=...` / `-c temperature=...` 在本版本
// 0.133.0 未生效（实测 CLI 静默接受但结果未改变），因此按矩阵 ② 直接忽略。
//
// CLI 路径解析顺序：显式 BinPath → MAGIC_AGENT_CODEX_BIN → 常见安装位 → PATH
// （探测链统一收敛在 engine_base.go 的 cliBase）。
//
// CODEX_HOME 隔离（codex_home.go，2026-09-16 实测 0.154.0）：
// ~/.codex/auth.json 的 ChatGPT 登录态会让 codex exec 启动时拉取
// cloud config bundle（auth.openai.com）；本机 DNS 解析该域名超时时
// 直接报 "timed out waiting for cloud config bundle after 15s" 退出。
// 引擎层注入镜像目录（无 auth.json）根治；外部显式设置的 CODEX_HOME /
// MAGIC_AGENT_CODEX_HOME 均被尊重。
//
// web_search 覆写：config.toml 常为 web_search="disabled"（无联网），
// 需要联网类查询时设 MAGIC_AGENT_CODEX_WEBSEARCH=live|cached|indexed
// 注入 -c web_search=<v>（仅非 resume 调用；值域 0.154 实测为
// disabled/cached/indexed/live）。
//
// 不实现 Stream：codex 的 `--output-last-message` 仅一次性产出最终
// message，且 `--json` 协议未稳定暴露正文 token 增量；流式能力按矩阵⑥在
// 后续版本补齐。CLI 层面对 nil Streamer 有明确报错路径（see cli/ask.go）。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CodexEngine 通过 Codex CLI 实现 Engine。
type CodexEngine struct {
	// BinPath 显式指定 CLI 路径（空 = 自动探测）。测试注入用。
	BinPath string

	// Model 显式模型（与 Request.Model 效果一致；引擎构造时显式覆盖优先）。
	Model string
}

// Name 实现 Engine。
func (e *CodexEngine) Name() string { return "codex" }

// DefaultCodexTimeout 单次尝试默认超时（codex 沙箱+网路，整体偏慢）。
const DefaultCodexTimeout = 8 * time.Minute

// bin 探测 codex CLI 路径（委托 cliBase 统一探测链）。
func (e *CodexEngine) bin() string {
	return codexBase.resolve(e.BinPath)
}

// Detect 实现 Engine。
func (e *CodexEngine) Detect() (bool, string) {
	p := codexBase.resolve(e.BinPath)
	if p == "" {
		return false, codexBase.notFound
	}
	return true, p
}

// codexTurnUsage 对应 turn.completed.usage（实测结构）。
type codexTurnUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// codexThreadStarted 对应 thread.started 事件。
type codexThreadStarted struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
}

// codexItemCompleted 对应 item.completed 事件，agent_message 即最终正文。
type codexItemCompleted struct {
	Type string         `json:"type"`
	Item map[string]any `json:"item"`
}

// codexTurnCompleted 对应 turn.completed 事件（带 usage）。
type codexTurnCompleted struct {
	Type  string         `json:"type"`
	Usage codexTurnUsage `json:"usage"`
}

// ListModels 实现 ModelLister：`codex debug models` 输出 CLI 当前生效的
// raw model catalog（JSON），取 slug（= -m 接受的标识）。
func (e *CodexEngine) ListModels(ctx context.Context) ([]string, error) {
	stdout, err := probeCLI(ctx, "codex", e.bin(), "debug", "models")
	if err != nil {
		return nil, err
	}
	objs, err := jsonModelsArray(stdout)
	if err != nil {
		return nil, err
	}
	models := modelNamesFromObjects(objs, "slug", "display_name")
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: codex debug models returned no entries", ErrNoModelSource)
	}
	return models, nil
}

// Complete 实现 Engine：单次调用 codex exec。
//
// 执行模型：
//  1. 构造参数（codex exec [resume [id|--last]] ... --json -o <tmpfile> ...）
//  2. runCLI 拉取 stdout（NDJSON 事件流）+ stderr
//  3. 逐行解析事件：thread.started.thread_id → SessionID；
//     最后一个 item.completed.item.type=="agent_message" → Text（兜底用 -o 文件）；
//     turn.completed.usage → Tokens
//  4. --output-last-message 落盘文件作 fallback 兜底（--output-schema 时也用它读 JSON）
func (e *CodexEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return Response{}, fmt.Errorf("codex CLI not found; set MAGIC_AGENT_CODEX_BIN")
	}

	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, toolsIsOff(req))
	if prompt == "" {
		return Response{}, fmt.Errorf("codex: empty prompt")
	}
	if req.SystemPrompt == "" && toolsIsOff(req) {
		prompt += noToolSuffix
	}
	prompt = codexPromptWithAttachments(prompt, req)

	// 临时文件 -o:codex 把最终正文写到该路径。--output-schema 同样用它取结果。
	tmpDir, err := os.MkdirTemp("", "magic-agent-codex-")
	if err != nil {
		return Response{}, fmt.Errorf("codex: mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	outFile := filepath.Join(tmpDir, "last.txt")
	// JSONSchema 约束落到独立文件,与 -o 不冲突;最终回包做 fence 剥离。
	var schemaFile string
	if req.JSONSchema != nil {
		schemaFile = filepath.Join(tmpDir, "schema.json")
		if err := writeJSONSchemaFile(schemaFile, req.JSONSchema); err != nil {
			return Response{}, fmt.Errorf("codex: write schema: %w", err)
		}
	}

	args := e.buildArgs(req, prompt, outFile, schemaFile)
	// CODEX_HOME 隔离：镜像目录无 auth.json → codex 不拉 cloud config
	//（auth.openai.com DNS 不可达时的 15s 超时根因，见 codex_home.go）。
	// 同步失败静默回退旧行为（ensureCodexHome 内部已兜底）。
	codexHome, injectHome, _ := ensureCodexHome()
	var extraEnv []string
	if injectHome {
		extraEnv = append(extraEnv, "CODEX_HOME="+codexHome)
	}
	stdout, stderr, runErr := runCLIEnvIn(ctx, req.Workspace, extraEnv, bin, args...)
	// 即使 runErr 非空（如非零退出），仍尝试从落盘文件抢救文本；NDJSON
	// 事件流部分事件已写入 stdout，--output-last-message 通常也已落盘。
	resp := e.parseStdout(stdout)
	if resp.Latency == 0 {
		resp.Latency = time.Since(start)
	}
	resp.Engine = e.Name()

	// 兜底正文:agent_message 事件可能没拿到(text 为空),从 -o 落盘文件读。
	if strings.TrimSpace(resp.Text) == "" {
		if data, rerr := os.ReadFile(outFile); rerr == nil {
			resp.Text = stripJSONFence(strings.TrimSpace(string(data)))
		}
	}
	// JSONSchema 模式:进一步尝试严格抽取顶层 JSON 对象替换 Text。
	if req.JSONSchema != nil {
		if m, ok := extractJSONObjectStrict(resp.Text, req.JSONSchema); ok {
			if b, jerr := json.Marshal(m); jerr == nil {
				resp.Text = string(b)
			}
		}
	}
	if runErr != nil {
		return resp, wrapCliError("codex", stdout, stderr, runErr)
	}
	if strings.TrimSpace(resp.Text) == "" {
		return resp, fmt.Errorf("codex CLI returned empty result")
	}
	return resp, nil
}

// parseStdout 逐行解析 --json NDJSON,提取 Text / SessionID / Token 计数。
// 行可能为空(JSON 行间无空行)或非法 JSON(警告类输出),失败跳过。
func (e *CodexEngine) parseStdout(stdout string) Response {
	var (
		text      string
		sessionID string
		usage     codexTurnUsage
		gotUsage  bool
		gotItem   bool
	)
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 16MB 行上限
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// 尝试多事件类型,逐个 unmarshal,失败说明不是该事件,继续。
		var ts codexThreadStarted
		if json.Unmarshal([]byte(line), &ts) == nil && ts.Type == "thread.started" && ts.ThreadID != "" {
			sessionID = ts.ThreadID
			continue
		}
		var ic codexItemCompleted
		if json.Unmarshal([]byte(line), &ic) == nil && ic.Type == "item.completed" {
			if t, _ := ic.Item["type"].(string); t == "agent_message" {
				if s, _ := ic.Item["text"].(string); s != "" {
					text = s
					gotItem = true
				}
			}
			continue
		}
		var tc codexTurnCompleted
		if json.Unmarshal([]byte(line), &tc) == nil && tc.Type == "turn.completed" {
			usage = tc.Usage
			gotUsage = true
			continue
		}
		// 非预期事件:忽略(其他事件如 message.created/command_execution...)。
	}
	_ = gotItem
	_ = gotUsage
	resp := Response{
		Text:      text,
		SessionID: sessionID,
		Model:     stripModelPrefix(reqModelForCodex(e, "")),
	}
	// codex usage 不区分 input/output 思路:这里只赋值已知键,zero-value 自然兜底。
	resp.InputTokens = usage.InputTokens + usage.CachedInputTokens
	resp.OutputTokens = usage.OutputTokens + usage.ReasoningOutputTokens
	resp.TotalTokens = resp.InputTokens + resp.OutputTokens
	return resp
}

// Stream 实现 Streamer：`codex exec --json` 的 NDJSON 事件流逐行转发。
//
// ⚠️ 这一版 codex（0.133，2026-09-17 实测）**没有逐字文本增量** —— 正文在
// `item.completed{item.type=agent_message,text}` 里整段给出。实测事件形状（--tools on）：
//
//	{"type":"thread.started","thread_id":"…"}                     → 会话 id
//	{"type":"turn.started"}
//	{"type":"item.started","item":{"type":"command_execution","id":"item_0","command":"…"}}
//	{"type":"item.completed","item":{"type":"command_execution","id":"item_0","command":"…","aggregated_output":"…","exit_code":0}}
//	{"type":"item.completed","item":{"type":"agent_message","text":"…"}}   → 正文（整段）
//	{"type":"turn.completed","usage":{"input_tokens":…,"cached_input_tokens":…,"output_tokens":…,"reasoning_output_tokens":…}}
//
// 所以 codex 的「流式」是**事件级**的：工具调用/正文一到就发事件，调用方仍能实时看到
// 进展（只是没有逐字打字效果）。未知 item 类型忽略，不猜。
func (e *CodexEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return StreamResult{}, fmt.Errorf("codex CLI not found; set MAGIC_AGENT_CODEX_BIN")
	}
	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, toolsIsOff(req))
	if prompt == "" {
		return StreamResult{}, fmt.Errorf("codex: empty prompt")
	}
	if req.SystemPrompt == "" && toolsIsOff(req) {
		prompt += noToolSuffix
	}
	prompt = codexPromptWithAttachments(prompt, req)

	// 流式不走 --output-last-message（正文直接从事件流拿），也不写 --output-schema 文件
	//（schema 仍走调用方的输出后处理，与 Complete 的兜底一致）；传空路径即自动跳过这两个 flag。
	args := e.buildArgs(req, prompt, "", "")
	codexHome, injectHome, _ := ensureCodexHome()
	var extraEnv []string
	if injectHome {
		extraEnv = append(extraEnv, "CODEX_HOME="+codexHome)
	}

	acc := &codexStreamAcc{OnEvent: onEvent}
	if err := runStreamCLIEnvIn(ctx, req.Workspace, extraEnv, bin, args, acc.handleLine); err != nil {
		return StreamResult{}, err
	}
	text := acc.Text.String()
	if strings.TrimSpace(text) == "" {
		return StreamResult{}, fmt.Errorf("codex CLI returned empty result")
	}
	in := acc.Usage.InputTokens + acc.Usage.CachedInputTokens
	out := acc.Usage.OutputTokens + acc.Usage.ReasoningOutputTokens
	return StreamResult{
		Response: Response{
			Engine:       e.Name(),
			Text:         strings.TrimSpace(text),
			Model:        stripModelPrefix(reqModelForCodex(e, req.Model)),
			SessionID:    acc.SessionID,
			InputTokens:  in,
			OutputTokens: out,
			TotalTokens:  in + out,
			Latency:      time.Since(start),
		},
		Thinking: strings.TrimSpace(acc.Thinking.String()),
	}, nil
}

// codexStreamAcc 把 codex 的 NDJSON 事件流映射成 StreamEvent（映射表见 Stream 注释）。
type codexStreamAcc struct {
	OnEvent   func(StreamEvent)
	Text      strings.Builder
	Thinking  strings.Builder
	SessionID string
	Usage     codexTurnUsage
}

func (a *codexStreamAcc) emit(ev StreamEvent) {
	if a.OnEvent != nil {
		a.OnEvent(ev)
	}
}

func (a *codexStreamAcc) handleLine(line string) error {
	s := strings.TrimSpace(line)
	if s == "" {
		return nil
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(s), &head); err != nil {
		return nil // 非 JSON 行（CLI 的警告输出）跳过，不打断整条流
	}
	switch head.Type {
	case "thread.started":
		var ts codexThreadStarted
		if json.Unmarshal([]byte(s), &ts) == nil && ts.ThreadID != "" {
			a.SessionID = ts.ThreadID
		}
	case "item.started", "item.completed":
		a.handleItem(s, head.Type)
	case "turn.completed":
		var tc codexTurnCompleted
		if json.Unmarshal([]byte(s), &tc) == nil {
			a.Usage = tc.Usage
		}
	}
	return nil
}

// handleItem 处理 item.started / item.completed：
//
//	agent_message      正文（实测只在 completed 出现，整段）
//	reasoning          思考（该版本未观测到，实测兜底）
//	command_execution  started → tool_use（Name=command、Args=命令行）；
//	                   completed → tool_result（Name/ID 关联，Text=聚合输出）
func (a *codexStreamAcc) handleItem(raw, evType string) {
	var ic codexItemCompleted
	if json.Unmarshal([]byte(raw), &ic) != nil {
		return
	}
	it := ic.Item
	itType, _ := it["type"].(string)
	text, _ := it["text"].(string)
	id, _ := it["id"].(string)
	switch itType {
	case "agent_message":
		if evType == "item.completed" && text != "" {
			a.Text.WriteString(text)
			a.emit(StreamEvent{Kind: KindText, Text: text})
		}
	case "reasoning":
		if evType == "item.completed" && text != "" {
			a.Thinking.WriteString(text)
			a.emit(StreamEvent{Kind: KindThinking, Text: text})
		}
	case "command_execution":
		cmd, _ := it["command"].(string)
		if evType == "item.started" {
			a.emit(StreamEvent{Kind: KindToolUse, Name: "command", ID: id, Text: cmd})
			return
		}
		out, _ := it["aggregated_output"].(string)
		if out == "" {
			out = cmd
		}
		a.emit(StreamEvent{Kind: KindToolResult, Name: id, ID: id, Text: out})
	}
}

// reqModelForCodex 统一决定模型来源:引擎构造注入 > Request.Model > 留空(用 CLI 默认)。
func reqModelForCodex(e *CodexEngine, fallback string) string {
	if m := e.Model; m != "" {
		return m
	}
	return fallback
}

// writeJSONSchemaFile 序列化 JSONSchema 到磁盘(codex 读 TOML 格式,但
// --output-schema 接受任意 JSON Schema,这里走 JSON 序列化即可)。
func writeJSONSchemaFile(path string, s *JSONSchema) error {
	doc := map[string]any{
		"type":                 s.Type,
		"properties":           s.Properties,
		"required":             s.Required,
		"additionalProperties": false,
	}
	if s.Type == "" {
		doc["type"] = "object"
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// codexPromptWithAttachments 把「没走 -i 的附件」补进提示词正文：
//
//	非图片附件   -i 只收图片，只能给路径（靠 codex 的读文件能力）
//	resume 轮     `codex exec resume` 不接受 -i，图片也退回路径
//
// 新会话的图片附件走原生 -i（见 buildArgs），不进提示词。
func codexPromptWithAttachments(prompt string, req Request) string {
	if len(req.Attachments) == 0 {
		return prompt
	}
	if req.SessionID != "" || req.Continue {
		return appendAttachmentSection(prompt, req.Attachments)
	}
	var rest []Attachment
	for _, a := range req.Attachments {
		if !a.IsImage() {
			rest = append(rest, a)
		}
	}
	return appendAttachmentSection(prompt, rest)
}

// buildArgs 构造 codex exec 参数(Complete 与可能的后续 Stream 复用)。
//
//	args 形态(典型 new session):
//	  codex exec --skip-git-repo-check --json -m <model>
//	    [-c developer_instructions="<text>"]
//	    [-s read-only | --dangerously-bypass-approvals-and-sandbox]
//	    [-o <tmpfile>] [--output-schema <tmpfile>] [--ephemeral]
//	    <prompt>
//
//	续接变体:codex exec resume <id>|--last ...
func (e *CodexEngine) buildArgs(req Request, prompt, outFile, schemaFile string) []string {
	// off 模式:不要 full-access(与「禁工具」冲突),用 read-only 沙箱压制
	// 写入与命令执行;on / 白名单 模式:全放行,让 agent 真能跑。
	tools := toolsOrDefault(req.Tools)

	var args []string
	// 子命令前缀(续接路径:codex exec resume [id|--last] ...)
	switch {
	case req.SessionID != "":
		args = append(args, "exec", "resume", req.SessionID)
	case req.Continue:
		args = append(args, "exec", "resume", "--last")
	default:
		args = append(args, "exec")
	}
	// 通用 flags(放在子命令之后,prompt 之前)
	args = append(args, "--skip-git-repo-check", "--json")
	// 工作目录（workspace）：codex 有原生 `-C/--cd`（working root），优先用它 ——
	// 它同时决定 sandbox 的可写根与相对路径解析；子进程 cwd 也一并设置（见调用点）。
	if ws := strings.TrimSpace(req.Workspace); ws != "" {
		args = append(args, "-C", ws)
	}
	// 注意:不能加 --ephemeral。codex 0.133 在 --ephemeral 模式下不会把
	// session rollout 落盘,后续 --session 续接时报 "no rollout found for
	// thread id"(实测 2026-09-16)。为保持与 claude 一致的"默认新会话可续接"策略,
	// 始终不传 --ephemeral。
	// 模型
	if m := stripModelPrefix(req.Model); m != "" {
		args = append(args, "-m", m)
	} else if e.Model != "" {
		args = append(args, "-m", stripModelPrefix(e.Model))
	}
	// SystemPrompt → -c developer_instructions=<TOML 字符串>
	if req.SystemPrompt != "" {
		args = append(args, "-c", "developer_instructions="+tomlBasicString(req.SystemPrompt))
	}
	// 附件 → codex 原生 `-i/--image <FILE>...`（用户「文件＋提示词 比如截图加提示词」）。
	// 只对图片生效（-i 的语义就是 image），且**只在非 resume 轮**注入 ——
	// `codex exec resume` 只接受有限的覆写参数（实测 -s / bypass 会被 exit 2 拒绝），
	// 续接轮的附件改由 codexPromptWithAttachments 写进提示词。
	if req.SessionID == "" && !req.Continue {
		for _, a := range req.Attachments {
			if a.IsImage() {
				args = append(args, "-i", a.Path)
			}
		}
	}
	// 工具/沙箱:off 模式不调用工具,read-only 沙箱最契合(on/白名单需要工具,放行)。
	// 仅在非 resume 子命令下注入;codex 0.133 的 `exec resume` 子命令不接受
	// -s/--dangerously-bypass-approvals-and-sandbox(--help 列表无此 flag),
	// 传了直接 exit 2 "unexpected argument '-s' found"(实测 2026-09-16)。
	// 续接时延用首次会话的 sandbox 设置。
	if req.SessionID == "" && !req.Continue {
		if tools.IsOff() {
			args = append(args, "-s", "read-only")
		} else {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
		// web_search 覆写（MAGIC_AGENT_CODEX_WEBSEARCH=live|cached|indexed）：
		// 用户 config 常配 disabled；联网类查询需在此放开。resume 不注入。
		if v := codexWebSearchOverride(); v != "" {
			args = append(args, "-c", "web_search="+v)
		}
	}
	// -o 与 --output-schema 落盘文件
	if outFile != "" {
		args = append(args, "-o", outFile)
	}
	if schemaFile != "" {
		args = append(args, "--output-schema", schemaFile)
	}
	// 统一参数矩阵:MaxTokens / Temperature 静默忽略(见 engine_base.go 矩阵②)
	_ = req.MaxTokens
	_ = req.Temperature

	// prompt 必须放最后(codex exec [PROMPT] 位置参数)
	args = append(args, prompt)
	return args
}

// codexWebSearchOverride 读 MAGIC_AGENT_CODEX_WEBSEARCH 覆写值。
// 仅接受 0.154 实测合法值域（disabled/cached/indexed/live），其余忽略。
func codexWebSearchOverride() string {
	switch v := os.Getenv("MAGIC_AGENT_CODEX_WEBSEARCH"); v {
	case "disabled", "cached", "indexed", "live":
		return v
	}
	return ""
}

// tomlBasicString 把任意字符串编码为 TOML basic string 的等价形式。
// TOML basic string 与 JSON 字符串的转义集完全一致(\b \t \n \f \r \" \\ \uXXXX),
// 直接走 json.Marshal + 去掉外层引号即可。
func tomlBasicString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// 兜底:无法 marshal 时(基本不可能,因为 json 接受任意字符串)用双引号包裹
		// 并手动转义引号/反斜杠。
		var sb strings.Builder
		sb.WriteByte('"')
		for _, r := range s {
			if r == '"' || r == '\\' {
				sb.WriteByte('\\')
			}
			sb.WriteRune(r)
		}
		sb.WriteByte('"')
		return sb.String()
	}
	return string(b)
}
