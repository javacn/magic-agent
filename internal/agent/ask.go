package agent

// ask.go - 「需要用户选择」的统一格式与跨引擎兼容映射层。
//
// 背景（2026-09-18 真机实测，claude 2.1.146 / 本机 WorkBuddy 内置 codebuddy）：
//
//	claude 在 -p（headless）下调用 AskUserQuestion 时**没有** control_request，
//	而是普通 assistant 消息里的 tool_use 块，紧接着 CLI 自己回一条 is_error 的
//	tool_result 把提问**自动拒绝**（实测内容 "Answer questions?"）：
//
//	  {"type":"assistant","message":{"content":[{"type":"tool_use",
//	     "id":"call_01a0…","name":"AskUserQuestion",
//	     "input":{"questions":[{"header":"午餐选择","multiSelect":false,
//	       "options":[{"description":"选拉面","label":"拉面"},{"description":"选盖饭","label":"盖饭"}],
//	       "question":"午餐吃拉面还是盖饭？"}]}}]}}
//	  {"type":"user","message":{"content":[{"type":"tool_result",
//	     "content":"Answer questions?","is_error":true,"tool_use_id":"call_01a0…"}]},
//	   "tool_use_result":"Error: Answer questions?"}
//
//	codebuddy（本机构建）连 AskUserQuestion 都不在工具表里（工具表走 ToolSearch /
//	DeferExecuteTool 的延迟工具集），所以当前不会出现该形态；官方 SDK 文档则与 claude
//	同构地定义了该工具，故两家共用同一套答案格式。
//
// 于是「需要用户选择」在 wire 上只有两种形状，本文件把它们归一化成一份统一格式：
//
//	tool_use         模型发起提问（claude / codebuddy 族）。宿主只能**观察**到：
//	                 CLI 已自行拒绝，想真正作答只能靠 EncodeAskFollowUp 把答案作为
//	                 后续 user 消息补进去（正好复用常驻会话的 --append 通道）。
//	control_request  SDK 层 can_use_tool 请求（宿主自己实现了 canUseTool 回调时才有）。
//	                 此时宿主是决策方，用 EncodeAskAnswer / EncodeAskControlResponse 作答。
//
// 两族的**答案格式**是同一套（官方文档 claude / codebuddy 一致）：
//
//	allow  {"behavior":"allow","updatedInput":{<原 input 原样>,"answers":{<问题原文>:<label>}}}
//	deny   {"behavior":"deny","message":"…"}
//
// 最容易写错的三点，本文件用代码把它们钉死（不靠调用方自觉）：
//
//	1. answers 的 key 是**问题原文**（不是 header、不是 id）—— 见 askAnswersMap；
//	   问题原文重复时直接报错（文本 key 无法区分，静默合并会答错题）。
//	2. 多选把多个 label 用 ", " 连接（官方示例的写法）。
//	3. allow 时 updatedInput **必填**，且必须原样回传 questions 数组 —— EncodeAskAnswer
//	   用「原始 input + answers」的方式保证这一点（不做字段级重建，免得漏字段）。
//
// 其余引擎（trae / llm / codex / openclaw / arkclaw）实测均无 AskUserQuestion 与
// can_use_tool 协议，能力表 AskSupportOf 返回 "none"（ACP 系的 session/request_permission
// 是另一族协议，本项目尚未接入，不在本文件冒充支持）。

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// askMultiSelectSep 多选答案的连接符。官方文档示例即 ", "（逗号 + 空格），
// 换成别的（如 "、" 或 ","）会让 CLI 侧解析不到已选项。
const askMultiSelectSep = ", "

// AskAnswerPrefix 兜底通道（EncodeAskFollowUp）输出的固定前缀。
//
// 带上它是为了让模型能一眼认出「这段是用户对刚才那个提问的选择」，
// 而不是把它当成新的自由文本需求。调用方（观物台等）也可用它做解析锚点。
const AskAnswerPrefix = "【用户选择】"

// AskSource 说明这次「需要用户选择」是从哪种 wire 形状识别出来的。
type AskSource string

const (
	// AskSourceToolUse assistant 消息里的 tool_use 块（claude / codebuddy 实测形态）。
	AskSourceToolUse AskSource = "tool_use"
	// AskSourceControlRequest SDK 层 control_request（subtype=can_use_tool）。
	AskSourceControlRequest AskSource = "control_request"
)

// AskKind 这次「需要用户选择」的性质。
type AskKind string

const (
	// AskKindQuestion 模型主动提问（AskUserQuestion），答案是选项 label。
	AskKindQuestion AskKind = "question"
	// AskKindPermission 工具调用待授权（can_use_tool 且工具不是 AskUserQuestion），
	// 答案是 allow / deny。Questions 为空，待授权工具的入参在 ToolInput 里。
	AskKindPermission AskKind = "permission"
)

// AskOption 一个候选项。
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Preview 选项预览（claude TS SDK 专有：由 toolConfig.askUserQuestion.previewFormat
	// 决定是否生成，markdown / html 两种）。codebuddy 无此字段，归一化后留空。
	Preview string `json:"preview,omitempty"`
}

// AskQuestion 一个归一化后的问题。
type AskQuestion struct {
	// ID 规范化 id（"q0" / "q1" …，按出现顺序）。**不是** wire 上的字段：
	// 两家 CLI 都不给问题 id，answers 只能用问题原文做 key，所以本 id 只用于
	// 调用方（UI / 程序）稳定地回填答案 —— 见 EncodeAskAnswer 的匹配顺序。
	ID string `json:"id"`
	// Text 问题正文（answers 的 key，必须原样）。
	Text string `json:"text"`
	// Header 短标签。claude 侧要求 ≤12 字符（见 NormalizeAskHeader），codebuddy 无限制。
	Header string `json:"header,omitempty"`
	// Options 候选项（claude 侧 2–4 个）。
	Options []AskOption `json:"options,omitempty"`
	// MultiSelect 是否多选（wire 字段名就是 camelCase 的 multiSelect）。
	MultiSelect bool `json:"multi_select"`
}

// AskRequest 归一化后的「需要用户选择」请求（跨引擎统一格式）。
//
// 这是本层对外的**唯一**格式：调用方只认它，不必知道底下是 claude 还是 codebuddy、
// 也不必知道这次走的是 tool_use 还是 control_request。
type AskRequest struct {
	// Engine 产出该请求的引擎名（由调用方 / 流解析器填入，可能为空）。
	Engine string `json:"engine,omitempty"`
	// Kind 提问 / 待授权。
	Kind AskKind `json:"kind"`
	// Source 识别来源。
	Source AskSource `json:"source"`
	// ToolName 原始工具名（提问恒为 "AskUserQuestion"）。
	ToolName string `json:"tool_name,omitempty"`
	// ToolUseID 引擎分配的工具调用 id（tool_use 形态必有）。
	ToolUseID string `json:"tool_use_id,omitempty"`
	// RequestID control_request 的 request_id（回 control_response 时必须原样带回）。
	RequestID string `json:"request_id,omitempty"`
	// Questions 归一化后的问题列表（Kind=question 时非空）。
	Questions []AskQuestion `json:"questions,omitempty"`
	// ToolInput 原始工具入参 JSON（allow 时作为 updatedInput 的基底原样回传）。
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	// Raw 原始整行 NDJSON（排查用，不对外序列化）。
	Raw json.RawMessage `json:"-"`
}

// AskChoice 调用方对**一个问题**的选择。
type AskChoice struct {
	// QuestionID 优先按归一化 id 匹配（"q0"）。
	QuestionID string `json:"question_id,omitempty"`
	// Question 次选：按问题原文精确匹配（与 CLI 侧的 key 语义一致）。
	Question string `json:"question,omitempty"`
	// Labels 命中的选项 label（多选可多个）。
	Labels []string `json:"labels,omitempty"`
	// FreeText 自由文本（用户自填，wire 上直接把文本当答案，不要写 "Other"）。
	FreeText string `json:"free_text,omitempty"`
}

// AskAnswer 调用方给出的统一答案。
type AskAnswer struct {
	// Denied 拒绝（提问：拒绝回答；待授权：不允许执行）。
	Denied bool `json:"denied,omitempty"`
	// Message 拒绝理由 / 想回给模型的话。为空时回 "User declined"。
	Message string `json:"message,omitempty"`
	// Interrupt 拒绝的同时中断整个会话（阻止模型继续尝试）。
	// 只有声明支持的引擎会渲染该字段（见 AskInterruptSupportOf）。
	Interrupt bool `json:"interrupt,omitempty"`
	// Choices 每个问题的选择（Kind=question 时必填；Kind=permission 时忽略）。
	Choices []AskChoice `json:"choices,omitempty"`
}

// ---------------------------------------------------------------- 解析（引擎原生 → 统一）

// askQuestionWire 是 wire 上单个问题的原始形状（两家一致）。
type askQuestionWire struct {
	Question    string `json:"question"`
	Header      string `json:"header"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
		Preview     string `json:"preview"`
	} `json:"options"`
}

// ParseAskLine 从一行 stream-json NDJSON 里识别「需要用户选择」并归一化。
//
// 命中返回 (统一请求, true)；非该形态（普通正文 / 工具事件 / 杂讯）返回 (nil, false)。
// 只认两种形状，与真机实测一致：
//
//	{"type":"assistant", …content[].type=tool_use,name=AskUserQuestion…}
//	{"type":"control_request","request":{"subtype":"can_use_tool",…}}
//
// engine 只用于回填 AskRequest.Engine（可为空）；解析本身与引擎无关。
func ParseAskLine(engine, line string) (*AskRequest, bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return nil, false
	}
	switch probe.Type {
	case "control_request":
		return parseAskControlRequest(engine, line)
	case "assistant":
		return parseAskAssistantLine(engine, line)
	}
	return nil, false
}

// parseAskControlRequest 解析 SDK 层 control_request（宿主自己实现 canUseTool 时才会出现）。
func parseAskControlRequest(engine, line string) (*AskRequest, bool) {
	var cr struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype   string          `json:"subtype"`
			ToolName  string          `json:"tool_name"`
			ToolUseID string          `json:"tool_use_id"`
			Input     json.RawMessage `json:"input"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(line), &cr); err != nil {
		return nil, false
	}
	// 只有 can_use_tool 才是「需要用户选择」；同层的 rewind / interrupt 等子类型不是。
	if cr.Request.Subtype != "can_use_tool" {
		return nil, false
	}
	return normalizeAsk(engine, AskSourceControlRequest, cr.Request.ToolName,
		cr.Request.Input, cr.Request.ToolUseID, cr.RequestID, json.RawMessage(line))
}

// parseAskAssistantLine 解析 assistant 消息里的 AskUserQuestion tool_use 块。
//
// 一条 assistant 消息可能带多个 content 块，这里只取**第一条** AskUserQuestion ——
// 同一条消息里出现多个提问块的情形在真机上未观察到（模型一轮只问一次）。
func parseAskAssistantLine(engine, line string) (*AskRequest, bool) {
	var am struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &am); err != nil {
		return nil, false
	}
	for _, b := range am.Message.Content {
		if b.Type != "tool_use" || !isAskUserQuestion(b.Name) {
			continue
		}
		if req, ok := normalizeAsk(engine, AskSourceToolUse, b.Name, b.Input, b.ID, "", json.RawMessage(line)); ok {
			return req, true
		}
	}
	return nil, false
}

// NormalizeAskToolUse 把一个已知的 tool_use 块归一化成统一格式（流解析器累积完参数后调用）。
//
// 与 ParseAskLine 的区别：这里不重新解析整行，直接吃「工具名 + 入参 JSON」——
// claude/codebuddy 的流式协议里参数是 input_json_delta 拼出来的，收尾时才有完整 JSON。
//
// 工具名不是 AskUserQuestion（或 questions 为空）时返回 (nil, false)：
// 普通工具调用的授权不在这里冒充成用户选择（待授权请走 control_request 那条路）。
func NormalizeAskToolUse(engine, toolName string, input json.RawMessage, toolUseID string) (*AskRequest, bool) {
	return normalizeAsk(engine, AskSourceToolUse, toolName, input, toolUseID, "", nil)
}

// normalizeAsk 是两条解析路径的公共归一点。
//
// AskUserQuestion → Kind=question（解析 questions；**一个都没有就不算命中**，
// 免得把空入参的工具调用误报成提问）。
// 其它工具 → 仅 control_request 形态算命中：Kind=permission（待授权；Questions 为空，
// 入参留在 ToolInput）；tool_use 形态下非 AskUserQuestion 一律不命中（见下）。
func normalizeAsk(engine string, src AskSource, toolName string, input json.RawMessage, toolUseID, requestID string, raw json.RawMessage) (*AskRequest, bool) {
	req := &AskRequest{
		Engine:    engine,
		Source:    src,
		ToolName:  toolName,
		ToolUseID: toolUseID,
		RequestID: requestID,
		ToolInput: input,
		Raw:       raw,
	}
	if !isAskUserQuestion(toolName) {
		// tool_use 形态下**只有** AskUserQuestion 才算「需要用户选择」：普通工具调用
		//（Bash / Read…）的授权是 CLI 自己的事，不该在这里冒充成一次用户选择。
		// control_request 形态则相反 —— can_use_tool 本身就是「这条工具调用待授权」。
		if src == AskSourceToolUse {
			return nil, false
		}
		req.Kind = AskKindPermission
		return req, true
	}
	req.Kind = AskKindQuestion

	var wire struct {
		Questions []askQuestionWire `json:"questions"`
	}
	if len(input) > 0 {
		_ = json.Unmarshal(input, &wire) // 形状不合就落到下面的「空问题」分支
	}
	for i, q := range wire.Questions {
		nq := AskQuestion{
			ID:          fmt.Sprintf("q%d", i),
			Text:        q.Question,
			Header:      q.Header,
			MultiSelect: q.MultiSelect,
		}
		for _, o := range q.Options {
			nq.Options = append(nq.Options, AskOption{
				Label:       o.Label,
				Description: o.Description,
				Preview:     o.Preview,
			})
		}
		req.Questions = append(req.Questions, nq)
	}
	if len(req.Questions) == 0 {
		return nil, false
	}
	return req, true
}

// isAskUserQuestion 工具名判定。容忍大小写与下划线差异
// （"AskUserQuestion" / "ask_user_question" / "askuserquestion" 都认）。
func isAskUserQuestion(name string) bool {
	if name == "" {
		return false
	}
	var b strings.Builder
	for _, r := range name {
		if r == '_' || r == '-' || r == ' ' {
			continue
		}
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String() == "askuserquestion"
}

// ---------------------------------------------------------------- 渲染（统一 → 引擎原生）

// EncodeAskAnswer 把统一答案渲染成引擎的**原生返回格式**（SDK 层 PermissionResult）：
//
//	允许  {"behavior":"allow","updatedInput":{…原 input…,"answers":{"<问题原文>":"<label>"}}}
//	拒绝  {"behavior":"deny","message":"…"[,"interrupt":true]}
//
// 适用场景：宿主自己实现了 canUseTool 回调（即这次请求来自 control_request）。
// 若这次请求是 tool_use 形态（CLI 已自行拒绝），请改用 EncodeAskFollowUp。
func EncodeAskAnswer(engine string, req *AskRequest, ans AskAnswer) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("ask: nil request")
	}
	if !AskSupportsEngine(engine) {
		return nil, fmt.Errorf("ask: engine %q does not expose AskUserQuestion (see AskSupportOf)", engine)
	}

	if ans.Denied {
		out := map[string]any{"behavior": "deny"}
		msg := ans.Message
		if msg == "" {
			msg = "User declined"
		}
		out["message"] = msg
		// interrupt 只有声明支持的引擎才渲染：claude 的官方 SDK 文档没有该字段，
		// 多传可能被严格校验的实现拒绝（不静默降级，也不冒险多塞）。
		if ans.Interrupt && AskInterruptSupportOf(engine) {
			out["interrupt"] = true
		}
		return json.Marshal(out)
	}

	// allow：updatedInput 必填（claude 侧省略会因校验失败被拒），且必须原样回传
	// questions —— 所以拿**原始入参**当基底，只加/改 answers 一个键。
	input := map[string]any{}
	if len(req.ToolInput) > 0 {
		if err := json.Unmarshal(req.ToolInput, &input); err != nil {
			return nil, fmt.Errorf("ask: decode tool input: %w", err)
		}
	}
	if req.Kind == AskKindQuestion {
		answers, err := askAnswersMap(req, ans)
		if err != nil {
			return nil, err
		}
		input["answers"] = answers
	}
	return json.Marshal(map[string]any{"behavior": "allow", "updatedInput": input})
}

// EncodeAskControlResponse 在 EncodeAskAnswer 外面再包一层 wire 信封（control_response）。
//
// ⚠️ 信封的嵌套层级**官方未公开逐字段规范**（社区逆向对层级描述也不一致，见
// WEBSOCKET_PROTOCOL_REVERSED 一类资料）。这里按最常见的三层写法输出：
//
//	{"type":"control_response","response":{"subtype":"success","request_id":"…",
//	 "response":{<EncodeAskAnswer 的结果>}}}
//
// 真要对接某个宿主前，务必对该宿主的真实 CLI 抓包核对一次。
func EncodeAskControlResponse(engine string, req *AskRequest, ans AskAnswer) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("ask: nil request")
	}
	if req.RequestID == "" {
		return nil, fmt.Errorf("ask: request has no request_id (source=%s); control_response cannot be built", req.Source)
	}
	result, err := EncodeAskAnswer(engine, req, ans)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": req.RequestID,
			"response":   json.RawMessage(result),
		},
	})
}

// EncodeAskFollowUp 把统一答案渲染成一段**普通用户消息文本**（兜底通道）。
//
// 为什么需要它：headless 下 claude 在模型调用 AskUserQuestion 后**立刻自行拒绝**
// （实测 tool_result 内容 "Answer questions?"，is_error=true），宿主没有回填
// tool_result 的窗口。此时唯一还能把用户选择送进模型的办法，就是把它作为
// **后续一条 user 消息**追加进去 —— 正好复用常驻会话的 --append 通道。
//
// 输出是纯文本（不依赖任何引擎的 JSON 协议），对全部引擎可用。
func EncodeAskFollowUp(req *AskRequest, ans AskAnswer) string {
	var b strings.Builder
	b.WriteString(AskAnswerPrefix)
	b.WriteString("\n")
	if req == nil {
		b.WriteString("- （请求缺失）\n")
		return b.String()
	}
	if req.Kind == AskKindPermission {
		verdict := "允许"
		if ans.Denied {
			verdict = "拒绝"
		}
		name := req.ToolName
		if name == "" {
			name = "工具调用"
		}
		b.WriteString("- " + name + " → " + verdict)
		if ans.Message != "" {
			b.WriteString("：" + ans.Message)
		}
		b.WriteString("\n")
		return b.String()
	}
	if ans.Denied {
		b.WriteString("- （用户拒绝回答本次提问）")
		if ans.Message != "" {
			b.WriteString("：" + ans.Message)
		}
		b.WriteString("\n")
		return b.String()
	}
	for _, q := range req.Questions {
		v := "（未回答）"
		for _, c := range ans.Choices {
			if !choiceMatches(c, q) {
				continue
			}
			if c.FreeText != "" {
				v = c.FreeText
			} else {
				v = strings.Join(c.Labels, askMultiSelectSep)
			}
			break
		}
		b.WriteString("- " + q.Text + " → " + v + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------- 能力表

// AskSupportOf 返回某引擎「需要用户选择」的落地方式（机器可读）：
//
//	"tool:AskUserQuestion"  有该工具 + can_use_tool 协议（claude / codebuddy 族）
//	"none"                  无该能力（其余引擎实测均无 AskUserQuestion，也无 canUseTool）
//
// 与 WorkspaceSupportOf / AttachmentSupportOf 同一约定：能力值同时可进 `--engines` 输出，
// 供调用方决定要不要走 Ask 流程。
//
// 注：trae / cursor / iflow / qwen 等走的是 ACP 的 session/request_permission（另一族
// 协议：选项带 optionId + kind=allow_once/reject_once…），本项目尚未接入，因此如实报 none。
func AskSupportOf(engine string) string {
	if equalFold(engine, "claude") || equalFold(engine, "codebuddy") {
		return "tool:AskUserQuestion"
	}
	return "none"
}

// AskSupportsEngine 引擎是否支持 AskUserQuestion 族协议。
func AskSupportsEngine(engine string) bool { return AskSupportOf(engine) != "none" }

// AskInterruptSupportOf 拒绝时是否支持 interrupt（拒绝并中断整个会话）。
//
// 只有 codebuddy 的官方文档写了该字段；claude 的 SDK 文档只给 behavior/message，
// 故对 claude 不渲染（见 EncodeAskAnswer）。
func AskInterruptSupportOf(engine string) bool { return equalFold(engine, "codebuddy") }

// askHeaderLimit claude 侧 header 的字符上限（官方 SDK 文档：最多 12 字符）。
// codebuddy 无此限制，返回 0 表示不限。
func askHeaderLimit(engine string) int {
	if equalFold(engine, "claude") {
		return 12
	}
	return 0
}

// NormalizeAskHeader 按目标引擎的限制归一化 header（超长按**字符**截断，不是字节）。
//
// 给「自己生成问题」的调用方用（本层的解析方向只做原样透传，不改模型给的内容）。
func NormalizeAskHeader(engine, header string) string {
	limit := askHeaderLimit(engine)
	if limit <= 0 || utf8.RuneCountInString(header) <= limit {
		return header
	}
	runes := []rune(header)
	return string(runes[:limit])
}

// ---------------------------------------------------------------- 内部工具

// askAnswersMap 把统一答案编译成 wire 需要的 answers 映射。
//
// 三条硬约束（对应文件头注释里「最容易写错的三点」）：
//  1. key 必须是**问题原文**；
//  2. 每个问题都必须有答案（CLI 侧按「全部问题都答了」处理 updatedInput）；
//  3. 问题原文不能重复 —— 重复时文本 key 无法区分，直接报错而不是静默合并。
func askAnswersMap(req *AskRequest, ans AskAnswer) (map[string]string, error) {
	if len(req.Questions) == 0 {
		return nil, fmt.Errorf("ask: no questions to answer")
	}
	byText := map[string]int{}
	for i, q := range req.Questions {
		if _, dup := byText[q.Text]; dup {
			return nil, fmt.Errorf("ask: question text %q appears more than once; the wire format keys answers by question text, so duplicates cannot be answered reliably", q.Text)
		}
		byText[q.Text] = i
	}

	out := make(map[string]string, len(req.Questions))
	for _, c := range ans.Choices {
		idx, err := matchChoice(req, byText, c)
		if err != nil {
			return nil, err
		}
		if _, dup := out[req.Questions[idx].Text]; dup {
			return nil, fmt.Errorf("ask: question %q answered twice", req.Questions[idx].Text)
		}
		v, err := askChoiceValue(req.Questions[idx], c)
		if err != nil {
			return nil, err
		}
		out[req.Questions[idx].Text] = v
	}
	for _, q := range req.Questions {
		if _, ok := out[q.Text]; !ok {
			return nil, fmt.Errorf("ask: question %q has no answer", q.Text)
		}
	}
	return out, nil
}

// matchChoice 定位一个选择对应的问题下标：先按归一化 id，再按问题原文。
func matchChoice(req *AskRequest, byText map[string]int, c AskChoice) (int, error) {
	if c.QuestionID != "" {
		for i, q := range req.Questions {
			if q.ID == c.QuestionID {
				return i, nil
			}
		}
		return -1, fmt.Errorf("ask: unknown question id %q", c.QuestionID)
	}
	if c.Question != "" {
		if i, ok := byText[c.Question]; ok {
			return i, nil
		}
		return -1, fmt.Errorf("ask: unknown question %q", c.Question)
	}
	return -1, fmt.Errorf("ask: choice needs question_id or question")
}

// askChoiceValue 校验并渲染单个问题的答案值。
//
// 不静默接受「不在候选里的 label」：wire 上 answers 的值必须是所选项的 label，
// 幻觉 label 到了模型侧会被当成无效答案，不如在这里就报错。
func askChoiceValue(q AskQuestion, c AskChoice) (string, error) {
	if c.FreeText != "" {
		if len(c.Labels) > 0 {
			return "", fmt.Errorf("ask: question %q: give either labels or free_text, not both", q.Text)
		}
		return c.FreeText, nil
	}
	if len(c.Labels) == 0 {
		return "", fmt.Errorf("ask: question %q: empty answer", q.Text)
	}
	if len(c.Labels) > 1 && !q.MultiSelect {
		return "", fmt.Errorf("ask: question %q is single-select but got %d labels", q.Text, len(c.Labels))
	}
	known := make(map[string]bool, len(q.Options))
	for _, o := range q.Options {
		known[o.Label] = true
	}
	for _, l := range c.Labels {
		if !known[l] {
			return "", fmt.Errorf("ask: question %q: label %q is not one of its options", q.Text, l)
		}
	}
	return strings.Join(c.Labels, askMultiSelectSep), nil
}

// choiceMatches 宽松匹配（仅用于兜底文本渲染，不做校验）：
// 按 id 或问题原文任一命中即可。
func choiceMatches(c AskChoice, q AskQuestion) bool {
	if c.QuestionID != "" {
		return c.QuestionID == q.ID
	}
	return c.Question != "" && c.Question == q.Text
}

// askSummaryText 把一次「需要用户选择」压成一行人类可读文本
// （流事件的 Text 字段用它，text 模式打到 stderr 即可看懂）。
func askSummaryText(req *AskRequest) string {
	if req == nil {
		return ""
	}
	if req.Kind == AskKindPermission {
		return fmt.Sprintf("需要授权：%s（等待用户选择）", req.ToolName)
	}
	parts := make([]string, 0, len(req.Questions))
	for _, q := range req.Questions {
		labels := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			labels = append(labels, o.Label)
		}
		parts = append(parts, fmt.Sprintf("%s [%s]", q.Text, strings.Join(labels, " | ")))
	}
	return "需要用户选择：" + strings.Join(parts, "；")
}
