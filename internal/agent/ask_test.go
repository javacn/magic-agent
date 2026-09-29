package agent

// ask_test.go - 「需要用户选择」统一映射层的单测。
//
// 用例里的 payload 分两类，注释里都标了出处：
//   真机抓包 —— 2026-09-18 用 claude 2.1.146 / 本机 WorkBuddy codebuddy 实跑抓到
//              （`printf … | claude -p --input-format stream-json --output-format stream-json --verbose`）；
//   文档形状 —— 官方 SDK 文档给的 control_request / updatedInput 例子。

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 测试素材

// askToolUseLineReal 真机抓包（claude 2.1.146）：模型调用 AskUserQuestion 的 assistant 行。
// 只保留本层关心的字段（type / message.content），其余（model / usage / uuid…）略。
const askToolUseLineReal = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call_01a0b32ddcb27515aa596902","name":"AskUserQuestion","input":{"questions":[{"header":"午餐选择","multiSelect":false,"options":[{"description":"选拉面","label":"拉面"},{"description":"选盖饭","label":"盖饭"}],"question":"午餐吃拉面还是盖饭？"}]}}]},"session_id":"ad0f2b93-7916-4d8a-8852-9756c179aff1"}`

// askAutoDenyLineReal 真机抓包：CLI 紧接着自己回的那条**自动拒绝**（模型拿不到用户答案）。
const askAutoDenyLineReal = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"Answer questions?","is_error":true,"tool_use_id":"call_01a0b32ddcb27515aa596902"}]},"parent_tool_use_id":null,"session_id":"ad0f2b93-7916-4d8a-8852-9756c179aff1","tool_use_result":"Error: Answer questions?"}`

// askControlRequestDoc 文档形状：SDK 层 can_use_tool 请求（宿主自己实现 canUseTool 时才有）。
const askControlRequestDoc = `{"type":"control_request","request_id":"perm-1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"toolu_01","input":{"questions":[{"question":"用哪个数据库？","header":"数据库","multiSelect":false,"options":[{"label":"PostgreSQL","description":"关系型数据库"},{"label":"MongoDB","description":"文档数据库"}]}]},"permission_suggestions":[]}}`

// askPermissionControlRequestDoc 文档形状：非提问类的待授权请求（工具不是 AskUserQuestion）。
const askPermissionControlRequestDoc = `{"type":"control_request","request_id":"perm-2","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"toolu_02","input":{"command":"npm test"}}}`

// ---------------------------------------------------------------- 解析

func TestParseAskLineRealToolUse(t *testing.T) {
	req, ok := ParseAskLine("claude", askToolUseLineReal)
	if !ok {
		t.Fatal("真机抓包的 AskUserQuestion tool_use 行没被识别")
	}
	if req.Kind != AskKindQuestion || req.Source != AskSourceToolUse {
		t.Errorf("Kind/Source = %q/%q，期望 question/tool_use", req.Kind, req.Source)
	}
	if req.Engine != "claude" {
		t.Errorf("Engine = %q，期望 claude", req.Engine)
	}
	if req.ToolUseID != "call_01a0b32ddcb27515aa596902" {
		t.Errorf("ToolUseID = %q", req.ToolUseID)
	}
	if len(req.Questions) != 1 {
		t.Fatalf("问题数 = %d，期望 1", len(req.Questions))
	}
	q := req.Questions[0]
	if q.ID != "q0" {
		t.Errorf("归一化 id = %q，期望 q0", q.ID)
	}
	if q.Text != "午餐吃拉面还是盖饭？" || q.Header != "午餐选择" || q.MultiSelect {
		t.Errorf("问题字段不对: %+v", q)
	}
	if len(q.Options) != 2 || q.Options[0].Label != "拉面" || q.Options[0].Description != "选拉面" || q.Options[1].Label != "盖饭" {
		t.Errorf("选项不对: %+v", q.Options)
	}
}

func TestParseAskLineControlRequest(t *testing.T) {
	req, ok := ParseAskLine("claude", askControlRequestDoc)
	if !ok {
		t.Fatal("control_request(can_use_tool) 没被识别")
	}
	if req.Source != AskSourceControlRequest || req.RequestID != "perm-1" || req.ToolUseID != "toolu_01" {
		t.Errorf("来源/request_id/tool_use_id 不对: %+v", req)
	}
	if len(req.Questions) != 1 || req.Questions[0].Text != "用哪个数据库？" {
		t.Errorf("问题没解析出来: %+v", req.Questions)
	}
}

func TestParseAskLinePermissionKind(t *testing.T) {
	req, ok := ParseAskLine("codebuddy", askPermissionControlRequestDoc)
	if !ok {
		t.Fatal("待授权的 control_request 没被识别")
	}
	if req.Kind != AskKindPermission {
		t.Errorf("Kind = %q，期望 permission", req.Kind)
	}
	if len(req.Questions) != 0 {
		t.Errorf("待授权请求不该有问题列表: %+v", req.Questions)
	}
	if !strings.Contains(string(req.ToolInput), "npm test") {
		t.Errorf("工具入参没保留: %s", req.ToolInput)
	}
}

func TestParseAskLineNotAsk(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"CLI 的自动拒绝 tool_result", askAutoDenyLineReal},
		{"普通正文", `{"type":"assistant","message":{"content":[{"type":"text","text":"你好"}]}}`},
		{"别的工具的 tool_use", `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"x","name":"Bash","input":{"command":"ls"}}]}}`},
		{"AskUserQuestion 但问题为空", `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"x","name":"AskUserQuestion","input":{"questions":[]}}]}}`},
		{"control_request 但子类型不是 can_use_tool", `{"type":"control_request","request_id":"r1","request":{"subtype":"rewind_files","tool_name":"AskUserQuestion"}}`},
		{"result 收尾行", `{"type":"result","subtype":"success","result":"done"}`},
		{"非 JSON 杂讯", `not json at all`},
		{"空行", ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if req, ok := ParseAskLine("claude", c.line); ok {
				t.Errorf("不该识别成「需要用户选择」，却得到 %+v", req)
			}
		})
	}
}

func TestNormalizeAskToolUse(t *testing.T) {
	// 工具名容忍大小写与下划线差异；参数是流式累积出来的完整 JSON。
	input := json.RawMessage(`{"questions":[{"question":"Q","header":"H","multiSelect":true,"options":[{"label":"A"}]}]}`)
	req, ok := NormalizeAskToolUse("claude", "ask_user_question", input, "call_1")
	if !ok {
		t.Fatal("下划线写法的工具名没被认出来")
	}
	if req.Questions[0].MultiSelect != true {
		t.Errorf("multiSelect 没解析: %+v", req.Questions[0])
	}
	if _, ok := NormalizeAskToolUse("claude", "Bash", input, "call_1"); ok {
		t.Error("Bash 不该被识别成提问")
	}
	if _, ok := NormalizeAskToolUse("claude", "AskUserQuestion", nil, "call_1"); ok {
		t.Error("空入参不该被识别成提问")
	}
}

// ---------------------------------------------------------------- 渲染

// q0Req 造一个两问（单选 + 多选）的统一请求，供渲染用例复用。
func q0Req(t *testing.T) *AskRequest {
	t.Helper()
	req, ok := ParseAskLine("claude", `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call_9","name":"AskUserQuestion","input":{"questions":[
		{"question":"用什么格式？","header":"Format","multiSelect":false,"options":[{"label":"摘要","description":"简短"},{"label":"详细","description":"展开"}]},
		{"question":"包含哪些章节？","header":"Sections","multiSelect":true,"options":[{"label":"引言","description":"开头"},{"label":"结论","description":"收尾"}]}
	]}}]}}`)
	if !ok {
		t.Fatal("素材没解析出来")
	}
	return req
}

func TestEncodeAskAnswerAllow(t *testing.T) {
	req := q0Req(t)
	out, err := EncodeAskAnswer("claude", req, AskAnswer{Choices: []AskChoice{
		{QuestionID: "q0", Labels: []string{"摘要"}},
		{Question: "包含哪些章节？", Labels: []string{"引言", "结论"}},
	}})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	var got struct {
		Behavior     string `json:"behavior"`
		UpdatedInput struct {
			Questions json.RawMessage   `json:"questions"`
			Answers   map[string]string `json:"answers"`
		} `json:"updatedInput"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, out)
	}
	if got.Behavior != "allow" {
		t.Errorf("behavior = %q，期望 allow", got.Behavior)
	}
	// answers 的 key 必须是**问题原文**（最容易写错的一点）。
	want := map[string]string{
		"用什么格式？":  "摘要",
		"包含哪些章节？": "引言, 结论", // 多选按 ", " 连接
	}
	if len(got.UpdatedInput.Answers) != len(want) {
		t.Fatalf("answers = %v，期望 %v", got.UpdatedInput.Answers, want)
	}
	for k, v := range want {
		if got.UpdatedInput.Answers[k] != v {
			t.Errorf("answers[%q] = %q，期望 %q", k, got.UpdatedInput.Answers[k], v)
		}
	}
	// questions 必须**原样回传**（allow 时 updatedInput 是必填，且要带上原问题数组）。
	if !strings.Contains(string(got.UpdatedInput.Questions), "用什么格式？") {
		t.Errorf("questions 没原样回传: %s", got.UpdatedInput.Questions)
	}
}

func TestEncodeAskAnswerFreeText(t *testing.T) {
	req := q0Req(t)
	out, err := EncodeAskAnswer("claude", req, AskAnswer{Choices: []AskChoice{
		{QuestionID: "q0", FreeText: "Markdown"},
		{QuestionID: "q1", Labels: []string{"引言"}},
	}})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	// 自由文本直接当答案值（不要写 "Other"）。
	if !strings.Contains(string(out), `"用什么格式？":"Markdown"`) {
		t.Errorf("自由文本没直接落到 answers 里: %s", out)
	}
}

func TestEncodeAskAnswerDeny(t *testing.T) {
	req := q0Req(t)
	out, err := EncodeAskAnswer("claude", req, AskAnswer{Denied: true, Message: "用户拒绝了", Interrupt: true})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if got["behavior"] != "deny" || got["message"] != "用户拒绝了" {
		t.Errorf("拒绝结构不对: %v", got)
	}
	// claude 的官方 SDK 文档没有 interrupt 字段 → 不渲染（codebuddy 才渲染）。
	if _, exists := got["interrupt"]; exists {
		t.Errorf("claude 不该带 interrupt: %v", got)
	}

	out2, err := EncodeAskAnswer("codebuddy", req, AskAnswer{Denied: true, Message: "用户拒绝了", Interrupt: true})
	if err != nil {
		t.Fatalf("codebuddy 渲染失败: %v", err)
	}
	var got2 map[string]any
	if err := json.Unmarshal(out2, &got2); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if got2["interrupt"] != true {
		t.Errorf("codebuddy 应带 interrupt: %v", got2)
	}
}

func TestEncodeAskAnswerDenyDefaultMessage(t *testing.T) {
	out, err := EncodeAskAnswer("claude", q0Req(t), AskAnswer{Denied: true})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if !strings.Contains(string(out), `"message":"User declined"`) {
		t.Errorf("拒绝时 message 为空应回默认文案: %s", out)
	}
}

func TestEncodeAskAnswerPermissionKind(t *testing.T) {
	req, ok := ParseAskLine("claude", askPermissionControlRequestDoc)
	if !ok {
		t.Fatal("素材没解析出来")
	}
	out, err := EncodeAskAnswer("claude", req, AskAnswer{})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	var got struct {
		Behavior     string         `json:"behavior"`
		UpdatedInput map[string]any `json:"updatedInput"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if got.Behavior != "allow" || got.UpdatedInput["command"] != "npm test" {
		t.Errorf("待授权放行应原样回传工具入参: %s", out)
	}
}

func TestEncodeAskAnswerErrors(t *testing.T) {
	dup := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c","name":"AskUserQuestion","input":{"questions":[
		{"question":"同一个问题？","header":"A","multiSelect":false,"options":[{"label":"是"},{"label":"否"}]},
		{"question":"同一个问题？","header":"B","multiSelect":false,"options":[{"label":"是"},{"label":"否"}]}
	]}}]}}`
	dupReq, ok := ParseAskLine("claude", dup)
	if !ok {
		t.Fatal("重复问题的素材没解析出来")
	}

	cases := []struct {
		name   string
		engine string
		req    *AskRequest
		ans    AskAnswer
		want   string
	}{
		{"引擎不支持（无已知答案形状）", "trae", q0Req(t), AskAnswer{}, "has no known ask-answer protocol shape"},
		{"问题原文重复（文本 key 无法区分）", "claude", dupReq, AskAnswer{Choices: []AskChoice{
			{QuestionID: "q0", Labels: []string{"是"}}, {QuestionID: "q1", Labels: []string{"否"}},
		}}, "appears more than once"},
		{"漏答一个问题", "claude", q0Req(t), AskAnswer{Choices: []AskChoice{{QuestionID: "q0", Labels: []string{"摘要"}}}}, "has no answer"},
		{"选项不在候选里", "claude", q0Req(t), AskAnswer{Choices: []AskChoice{
			{QuestionID: "q0", Labels: []string{"幻觉选项"}}, {QuestionID: "q1", Labels: []string{"引言"}},
		}}, "is not one of its options"},
		{"单选给了多个 label", "claude", q0Req(t), AskAnswer{Choices: []AskChoice{
			{QuestionID: "q0", Labels: []string{"摘要", "详细"}}, {QuestionID: "q1", Labels: []string{"引言"}},
		}}, "single-select"},
		{"问题 id 不存在", "claude", q0Req(t), AskAnswer{Choices: []AskChoice{
			{QuestionID: "q9", Labels: []string{"摘要"}}, {QuestionID: "q1", Labels: []string{"引言"}},
		}}, "unknown question id"},
		{"空答案", "claude", q0Req(t), AskAnswer{Choices: []AskChoice{
			{QuestionID: "q0"}, {QuestionID: "q1", Labels: []string{"引言"}},
		}}, "empty answer"},
		{"labels 与 free_text 同时给", "claude", q0Req(t), AskAnswer{Choices: []AskChoice{
			{QuestionID: "q0", Labels: []string{"摘要"}, FreeText: "X"}, {QuestionID: "q1", Labels: []string{"引言"}},
		}}, "not both"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := EncodeAskAnswer(c.engine, c.req, c.ans)
			if err == nil {
				t.Fatal("期望报错，却成功了")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息 = %q，期望含 %q", err.Error(), c.want)
			}
		})
	}
}

func TestEncodeAskControlResponse(t *testing.T) {
	req, _ := ParseAskLine("claude", askControlRequestDoc)
	out, err := EncodeAskControlResponse("claude", req, AskAnswer{Choices: []AskChoice{{QuestionID: "q0", Labels: []string{"PostgreSQL"}}}})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	var got struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior string `json:"behavior"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, out)
	}
	if got.Type != "control_response" || got.Response.Subtype != "success" {
		t.Errorf("信封不对: %s", out)
	}
	// request_id 必须原样带回（否则 CLI 对不上是哪条请求）。
	if got.Response.RequestID != "perm-1" {
		t.Errorf("request_id = %q，期望 perm-1", got.Response.RequestID)
	}
	if got.Response.Response.Behavior != "allow" {
		t.Errorf("内层结果不对: %s", out)
	}

	// tool_use 形态没有 request_id → 明确报错（不静默造一个）。
	toolUseReq, _ := ParseAskLine("claude", askToolUseLineReal)
	if _, err := EncodeAskControlResponse("claude", toolUseReq, AskAnswer{}); err == nil {
		t.Error("没有 request_id 时应报错")
	}
}

func TestEncodeAskFollowUp(t *testing.T) {
	req := q0Req(t)
	got := EncodeAskFollowUp(req, AskAnswer{Choices: []AskChoice{
		{QuestionID: "q0", Labels: []string{"摘要"}},
		{QuestionID: "q1", Labels: []string{"引言", "结论"}},
	}})
	for _, want := range []string{AskAnswerPrefix, "用什么格式？ → 摘要", "包含哪些章节？ → 引言, 结论"} {
		if !strings.Contains(got, want) {
			t.Errorf("兜底文本缺 %q:\n%s", want, got)
		}
	}

	// 未回答的问题显式标出来（不静默省略）。
	partial := EncodeAskFollowUp(req, AskAnswer{Choices: []AskChoice{{QuestionID: "q0", Labels: []string{"摘要"}}}})
	if !strings.Contains(partial, "（未回答）") {
		t.Errorf("漏答的问题应显式标注:\n%s", partial)
	}

	// 拒绝
	denied := EncodeAskFollowUp(req, AskAnswer{Denied: true, Message: "不想回答"})
	if !strings.Contains(denied, "用户拒绝回答") || !strings.Contains(denied, "不想回答") {
		t.Errorf("拒绝文案不对:\n%s", denied)
	}

	// 待授权
	perm, _ := ParseAskLine("claude", askPermissionControlRequestDoc)
	follow := EncodeAskFollowUp(perm, AskAnswer{})
	if !strings.Contains(follow, "Bash → 允许") {
		t.Errorf("待授权文案不对:\n%s", follow)
	}
	followDeny := EncodeAskFollowUp(perm, AskAnswer{Denied: true, Message: "命令有风险"})
	if !strings.Contains(followDeny, "Bash → 拒绝") || !strings.Contains(followDeny, "命令有风险") {
		t.Errorf("待授权拒绝文案不对:\n%s", followDeny)
	}

	// nil 请求不 panic
	if s := EncodeAskFollowUp(nil, AskAnswer{}); !strings.Contains(s, AskPrefixForTest) {
		t.Errorf("nil 请求也应给出前缀:\n%s", s)
	}
}

// AskPrefixForTest 只是把常量暴露给上面那条断言，避免硬编码文案。
const AskPrefixForTest = AskAnswerPrefix

func TestAskSupportOf(t *testing.T) {
	for _, e := range []string{"claude", "CLAUDE"} {
		if got := AskSupportOf(e); got != "tool:AskUserQuestion" {
			t.Errorf("AskSupportOf(%q) = %q", e, got)
		}
		if !AskSupportsEngine(e) {
			t.Errorf("AskSupportsEngine(%q) 应为 true", e)
		}
	}
	// codebuddy 族：模型看不到 AskUserQuestion（2026-09-28 七种配置实测，见 AskSupportOf 注释）。
	// 回归保护：曾按"同族协议"推断成 tool:AskUserQuestion，导致上层 UI 承诺永不出现的决策卡。
	for _, e := range []string{"codebuddy", "codebuddy-ai", "trae", "llm", "codex", "openclaw", "dsh", "arkclaw", "", "nope"} {
		if got := AskSupportOf(e); got != "none" {
			t.Errorf("AskSupportOf(%q) = %q，期望 none", e, got)
		}
	}
	// interrupt 是 codebuddy 的**协议形状**（当前真机走不到，见 AskInterruptSupportOf 注释）。
	if !AskInterruptSupportOf("codebuddy") || AskInterruptSupportOf("claude") {
		t.Error("interrupt 支持判定不对（只有 codebuddy 的官方文档写了该字段）")
	}
	if !AskInterruptSupportOf("codebuddy-ai") {
		t.Error("codebuddy-ai 与 codebuddy 同族同协议，应支持 interrupt")
	}
}

func TestNormalizeAskHeader(t *testing.T) {
	// claude 侧 header ≤12 字符；按**字符**（rune）截断，不能按字节。
	long := "午餐选择问题标题很长超过十二个字符"
	got := NormalizeAskHeader("claude", long)
	if n := len([]rune(got)); n != 12 {
		t.Errorf("截断后 = %d 个字符（%q），期望 12", n, got)
	}
	if got == long[:12] {
		t.Error("按字节截断会把中文切碎，必须按 rune")
	}
	// codebuddy 无该限制 → 原样。
	if NormalizeAskHeader("codebuddy", long) != long {
		t.Error("codebuddy 不该截断 header")
	}
	// 未超长原样返回。
	if NormalizeAskHeader("claude", "午餐") != "午餐" {
		t.Error("未超长应原样返回")
	}
}

// ---------------------------------------------------------------- 流接入

// feedNDJSON 跑一遍 handleNDJSONLine，收集所有事件。
func feedNDJSON(t *testing.T, acc *streamAccumulator, lines []string) []StreamEvent {
	t.Helper()
	var got []StreamEvent
	acc.OnEvent = func(ev StreamEvent) { got = append(got, ev) }
	for i, l := range lines {
		if _, err := acc.handleNDJSONLine(l); err != nil {
			t.Fatalf("第 %d 行解析失败: %v", i+1, err)
		}
	}
	return got
}

// onlyAskEvents 过滤出 KindAsk 事件。
func onlyAskEvents(evs []StreamEvent) []StreamEvent {
	var out []StreamEvent
	for _, ev := range evs {
		if ev.Kind == KindAsk {
			out = append(out, ev)
		}
	}
	return out
}

// TestStreamEmitsAskFromPartialAndAggregatedOnce 覆盖 claude 的真实双路：
// --include-partial-messages 下，同一个提问既走 content_block 增量（tool_use 累积），
// 又走聚合 assistant 行 —— 两条路都必须能识别，且**只报一次**。
func TestStreamEmitsAskFromPartialAndAggregatedOnce(t *testing.T) {
	partialJSON := `{\"questions\":[{\"question\":\"午餐吃拉面还是盖饭？\",\"header\":\"午餐选择\",\"multiSelect\":false,\"options\":[{\"label\":\"拉面\",\"description\":\"选拉面\"},{\"label\":\"盖饭\",\"description\":\"选盖饭\"}]}]}`
	lines := []string{
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_01a0b32ddcb27515aa596902","name":"AskUserQuestion"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"` + partialJSON + `"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
		askToolUseLineReal, // 聚合行（同 id）
		`{"type":"result","subtype":"success","result":"done","session_id":"s1"}`,
	}
	acc := &streamAccumulator{Engine: "claude"}
	evs := feedNDJSON(t, acc, lines)

	asks := onlyAskEvents(evs)
	if len(asks) != 1 {
		t.Fatalf("KindAsk 事件数 = %d，期望 1（去重失败）：%+v", len(asks), asks)
	}
	ev := asks[0]
	if ev.Ask == nil {
		t.Fatal("KindAsk 必须带归一化后的 Ask")
	}
	if ev.Ask.Engine != "claude" || ev.Ask.Kind != AskKindQuestion {
		t.Errorf("Ask 元信息不对: %+v", ev.Ask)
	}
	if len(ev.Ask.Questions) != 1 || ev.Ask.Questions[0].Options[1].Label != "盖饭" {
		t.Errorf("增量累积出的问题不对: %+v", ev.Ask.Questions)
	}
	// Text 是一行人话摘要（text 模式打到 stderr 用）。
	if !strings.Contains(ev.Text, "需要用户选择") || !strings.Contains(ev.Text, "拉面 | 盖饭") {
		t.Errorf("摘要文本不对: %q", ev.Text)
	}
	// 原有工具事件不受影响（提问仍是一条 tool_use）。
	var toolUse int
	for _, e := range evs {
		if e.Kind == KindToolUse {
			toolUse++
		}
	}
	if toolUse != 1 {
		t.Errorf("KindToolUse 事件数 = %d，期望 1", toolUse)
	}
}

// TestStreamEmitsAskOnlyFromAggregated 未开 --include-partial-messages 时
// （带附件的非流式路径就是这种）只有聚合行，也必须识别出来。
func TestStreamEmitsAskOnlyFromAggregated(t *testing.T) {
	acc := &streamAccumulator{Engine: "claude"}
	evs := feedNDJSON(t, acc, []string{askToolUseLineReal, askAutoDenyLineReal})
	asks := onlyAskEvents(evs)
	if len(asks) != 1 {
		t.Fatalf("KindAsk 事件数 = %d，期望 1", len(asks))
	}
	if asks[0].Ask.Source != AskSourceToolUse {
		t.Errorf("Source = %q，期望 tool_use", asks[0].Ask.Source)
	}
}

func TestStreamEmitsAskFromControlRequest(t *testing.T) {
	acc := &streamAccumulator{Engine: "codebuddy"}
	evs := feedNDJSON(t, acc, []string{askControlRequestDoc})
	asks := onlyAskEvents(evs)
	if len(asks) != 1 {
		t.Fatalf("KindAsk 事件数 = %d，期望 1", len(asks))
	}
	if asks[0].Ask.Source != AskSourceControlRequest || asks[0].Ask.RequestID != "perm-1" {
		t.Errorf("control_request 归一化不对: %+v", asks[0].Ask)
	}
}

// askToolUseLineNoSession 同上的提问行，但**不带** session_id —— 用来验证
// 「init 行给过的会话 id 能兜住后面不带 id 的提问行」。
const askToolUseLineNoSession = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call_ns_1","name":"AskUserQuestion","input":{"questions":[{"question":"午餐？","header":"午餐","multiSelect":false,"options":[{"label":"拉面"},{"label":"盖饭"}]}]}}]}}`

// TestStreamAskCarriesSessionID ask 事件必须带会话 id —— 宿主靠它把用户的答案
// `--append` 回同一会话（json 模式下 stderr 保持干净，拿不到启动提示里的 run_id）。
// 取「流里最近见到的」那个：init 行给过、提问行自己没带时，用 init 的。
func TestStreamAskCarriesSessionID(t *testing.T) {
	lines := []string{
		// init 行：流的最开头就带 session_id（提问出现时它已经在了）
		`{"type":"system","subtype":"init","session_id":"s-ask-1","model":"m"}`,
		askToolUseLineNoSession,
	}
	acc := &streamAccumulator{Engine: "claude"}
	asks := onlyAskEvents(feedNDJSON(t, acc, lines))
	if len(asks) != 1 {
		t.Fatalf("KindAsk 事件数 = %d，期望 1", len(asks))
	}
	if asks[0].SessionID != "s-ask-1" {
		t.Errorf("ask 事件没带 init 行的会话 id: %q", asks[0].SessionID)
	}
}

// TestStreamAskSessionIDFromAssistantLine 即使没有 init 行，也要能从 assistant 行
// 自带的 session_id 兜住（真机抓包的那一行就带）。
func TestStreamAskSessionIDFromAssistantLine(t *testing.T) {
	acc := &streamAccumulator{Engine: "claude"}
	asks := onlyAskEvents(feedNDJSON(t, acc, []string{askToolUseLineReal}))
	if len(asks) != 1 {
		t.Fatalf("KindAsk 事件数 = %d，期望 1", len(asks))
	}
	if asks[0].SessionID != "ad0f2b93-7916-4d8a-8852-9756c179aff1" {
		t.Errorf("应从 assistant 行的 session_id 兜住，got %q", asks[0].SessionID)
	}
}

func TestStreamNoAskForOtherTools(t *testing.T) {
	lines := []string{
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"Bash"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/tmp/x"}}]}}`,
		askAutoDenyLineReal,
	}
	acc := &streamAccumulator{Engine: "claude"}
	if asks := onlyAskEvents(feedNDJSON(t, acc, lines)); len(asks) != 0 {
		t.Errorf("非提问工具不该发 KindAsk: %+v", asks)
	}
}
