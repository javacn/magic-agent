package agent

// llmengine_test.go - llm 引擎（simonw/LLM CLI 包装）测试。
//
// 用 fake CLI 脚本模拟 llm prompt 的行为，验证：
//	- buildArgs 参数构造（-n、-m、-s、--no-stream、prompt 位置）
//	- Complete：--no-stream + 标签剥离
//	- Stream：纯文本逐行流式 + thinkSplitter 标签路由
//	- 标签被行边界切开的边界情况
//	- CLI 失败报错
//
// 标签字面量同样分段拼接（见 tags.go 头注释）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// llmTag 拼接思维链标签（避免源码出现连续 token）。
func llmTag(parts ...string) string { return strings.Join(parts, "") }

// TestLLMBuildArgs 参数构造：-n 恒有；-m / -s 按需；prompt 收尾。
func TestLLMBuildArgs(t *testing.T) {
	e := &LLMEngine{}

	// 最小形式
	got := e.buildArgs(Request{Model: "minimax-m3"}, "你好")
	want := []string{"prompt", "-n", "-m", "minimax-m3", "你好"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("buildArgs = %q, want %q", got, want)
	}

	// 带 system + engine/model 前缀剥离
	got = e.buildArgs(Request{Model: "llm/minimax-m3", SystemPrompt: "你是助手"}, "问题")
	want = []string{"prompt", "-n", "-m", "minimax-m3", "-s", "你是助手", "问题"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("buildArgs = %q, want %q", got, want)
	}

	// 无模型：不传 -m（llm 用自己的默认模型）
	got = e.buildArgs(Request{}, "hi")
	want = []string{"prompt", "-n", "hi"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("buildArgs = %q, want %q", got, want)
	}
}

// TestLLMBuildArgsModelOptions 模型选项经 -o 透传：
//
//	MaxTokens > 0      → -o max_tokens N
//	Temperature != nil → -o temperature T
//
// 缺省（0 / nil）时两个都不出现，避免给模型塞无意义的默认值。
func TestLLMBuildArgsModelOptions(t *testing.T) {
	e := &LLMEngine{}

	// 两个都给
	temp := 0.3
	got := e.buildArgs(Request{Model: "minimax-m3", MaxTokens: 32000, Temperature: &temp}, "写一集")
	want := []string{"prompt", "-n", "-m", "minimax-m3", "-o", "max_tokens", "32000", "-o", "temperature", "0.3", "写一集"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("buildArgs = %q, want %q", got, want)
	}

	// 只给 max-tokens
	got = e.buildArgs(Request{MaxTokens: 8000}, "hi")
	want = []string{"prompt", "-n", "-o", "max_tokens", "8000", "hi"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("buildArgs = %q, want %q", got, want)
	}

	// 整数温度不留 ".0" 尾巴；0 是合法温度（必须出现）
	zero := 0.0
	got = e.buildArgs(Request{Temperature: &zero}, "hi")
	want = []string{"prompt", "-n", "-o", "temperature", "0", "hi"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("buildArgs = %q, want %q (温度 0 必须透传)", got, want)
	}

	// 都不给
	got = e.buildArgs(Request{Model: "minimax-m3"}, "hi")
	for _, a := range got {
		if a == "-o" {
			t.Errorf("buildArgs = %q, want no -o when unset", got)
		}
	}
}

// TestLLMCompletePassesModelOptions --max-tokens / --temperature 真的落到命令行。
func TestLLMCompletePassesModelOptions(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, "llm")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + log + "\necho 'ok'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &LLMEngine{BinPath: cli}

	temp := 0.3
	_, err := e.Complete(context.Background(), Request{
		Model:       "minimax-m3",
		MaxTokens:   32000,
		Temperature: &temp,
		Messages:    []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	for _, want := range []string{"-o max_tokens 32000", "-o temperature 0.3"} {
		if !strings.Contains(args, want) {
			t.Errorf("%q missing in args: %q", want, args)
		}
	}
}

// TestLLMCompleteStripsThinking Complete 模式：--no-stream 全量输出，
// 标签块被剥离、正文与思维链分离。
func TestLLMCompleteStripsThinking(t *testing.T) {
	open := llmTag("<", "think", ">")
	closeTag := llmTag("<", "/", "think", ">")
	cli := writeFakeCLI(t, "llm", "#!/bin/sh\ncat <<'EOF'\n"+open+"先想一下"+closeTag+"答案是 2\nEOF\n")
	e := &LLMEngine{BinPath: cli}

	resp, err := e.Complete(context.Background(), Request{
		Model:    "minimax-m3",
		Messages: []Message{{Role: "user", Content: "1+1"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "答案是 2" {
		t.Errorf("Text = %q, want 答案是 2", resp.Text)
	}
	if resp.Engine != "llm" {
		t.Errorf("Engine = %q, want llm", resp.Engine)
	}
	if resp.Model != "minimax-m3" {
		t.Errorf("Model = %q, want minimax-m3", resp.Model)
	}
}

// TestLLMCompletePlainText 无标签模型：正文原样返回。
func TestLLMCompletePlainText(t *testing.T) {
	cli := writeFakeCLI(t, "llm", "#!/bin/sh\necho '普通回答'\n")
	e := &LLMEngine{BinPath: cli}

	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "普通回答" {
		t.Errorf("Text = %q, want 普通回答", resp.Text)
	}
}

// TestLLMCompletePassesFlags --no-stream 与 -s 落到真实命令行。
func TestLLMCompletePassesFlags(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, "llm")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + log + "\necho 'ok'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &LLMEngine{BinPath: cli}

	_, err := e.Complete(context.Background(), Request{
		Model:        "minimax-m3",
		SystemPrompt: "你是助手",
		Messages:     []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	for _, want := range []string{"prompt", "-n", "-m minimax-m3", "-s 你是助手", "--no-stream"} {
		if !strings.Contains(args, want) {
			t.Errorf("%q missing in args: %q", want, args)
		}
	}
}

// TestLLMCompleteCliError CLI 非零退出 → 报错。
func TestLLMCompleteCliError(t *testing.T) {
	cli := writeFakeCLI(t, "llm", "#!/bin/sh\necho 'Error: no such model' >&2\nexit 1\n")
	e := &LLMEngine{BinPath: cli}

	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "llm") {
		t.Errorf("error should mention llm: %v", err)
	}
}

// TestLLMStreamSplitsThinkingTags 流式：标签块路由到 thinking，
// 标签外正文路由到 text；换行保留。
func TestLLMStreamSplitsThinkingTags(t *testing.T) {
	open := llmTag("<", "think", ">")
	closeTag := llmTag("<", "/", "think", ">")
	cli := writeFakeCLI(t, "llm", "#!/bin/sh\ncat <<'EOF'\n"+open+"\n用户在做加法。\n"+closeTag+"\n答案是 2。\nEOF\n")
	e := &LLMEngine{BinPath: cli}

	events, onEvent := collectEvents()
	res, err := e.Stream(context.Background(), Request{
		Model:    "minimax-m3",
		Messages: []Message{{Role: "user", Content: "1+1"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "答案是 2。" {
		t.Errorf("Text = %q, want 答案是 2。", res.Text)
	}
	// 思维链内容：开闭标签之间的文本（收尾 TrimSpace 去掉标签行的换行）。
	if res.Thinking != "用户在做加法。" {
		t.Errorf("Thinking = %q, want 用户在做加法。", res.Thinking)
	}
	// 至少各一条增量
	var hasThink, hasText bool
	for _, ev := range *events {
		switch ev.Kind {
		case KindThinking:
			hasThink = true
		case KindText:
			hasText = true
		}
	}
	if !hasThink || !hasText {
		t.Errorf("events missing kinds: thinking=%v text=%v (events=%v)", hasThink, hasText, *events)
	}
	// 第一条事件必须是 thinking（开标签在最前）
	if len(*events) > 0 && (*events)[0].Kind != KindThinking {
		t.Errorf("first event = %v, want thinking", (*events)[0].Kind)
	}
}

// TestLLMStreamFakeTagNotSplit 被换行打断的伪标签（如 "<th\nink>"）
// 不是标签——标签本身不含换行，不可能被 Scanner 的行边界切开。
// 行为与非流式 splitThinkingTags 一致：视为正文原样输出。
func TestLLMStreamFakeTagNotSplit(t *testing.T) {
	open := llmTag("<", "think", ">")
	closeTag := llmTag("<", "/", "think", ">")
	half1, half2 := open[:3], open[3:]
	chalf1, chalf2 := closeTag[:4], closeTag[4:]
	cli := writeFakeCLI(t, "llm", "#!/bin/sh\ncat <<'EOF'\n"+half1+"\n"+half2+"推理"+chalf1+"\n"+chalf2+"正文\nEOF\n")
	e := &LLMEngine{BinPath: cli}

	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// 切开的伪标签整体属于正文（换行保留，行首尾空白已在收尾 TrimSpace）。
	want := half1 + "\n" + half2 + "推理" + chalf1 + "\n" + chalf2 + "正文"
	if res.Text != want {
		t.Errorf("Text = %q, want %q（伪标签不是标签，原样输出）", res.Text, want)
	}
	if res.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", res.Thinking)
	}
}

// TestLLMStreamTagAcrossLines 真标签跨行结构：开标签后正文多行、
// 闭标签独立成行、闭标签后还有正文——思维链块跨行正确聚合。
func TestLLMStreamTagAcrossLines(t *testing.T) {
	open := llmTag("<", "think", ">")
	closeTag := llmTag("<", "/", "think", ">")
	cli := writeFakeCLI(t, "llm", "#!/bin/sh\ncat <<'EOF'\n"+open+"\n第一行推理\n第二行推理\n"+closeTag+"\n最终答案\nEOF\n")
	e := &LLMEngine{BinPath: cli}

	events, onEvent := collectEvents()
	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "最终答案" {
		t.Errorf("Text = %q, want 最终答案", res.Text)
	}
	if res.Thinking != "第一行推理\n第二行推理" {
		t.Errorf("Thinking = %q, want 跨行思维链完整聚合（首尾换行已 trim）", res.Thinking)
	}
	if len(*events) == 0 || (*events)[0].Kind != KindThinking {
		t.Errorf("first event should be thinking: %v", *events)
	}
}

// TestLLMStreamPlainText 无标签流式：全部走 text 通道，换行保留。
func TestLLMStreamPlainText(t *testing.T) {
	cli := writeFakeCLI(t, "llm", "#!/bin/sh\ncat <<'EOF'\n第一行\n第二行\nEOF\n")
	e := &LLMEngine{BinPath: cli}

	events, onEvent := collectEvents()
	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "第一行\n第二行" {
		t.Errorf("Text = %q, want 第一行\\n第二行", res.Text)
	}
	if res.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", res.Thinking)
	}
	for _, ev := range *events {
		if ev.Kind != KindText {
			t.Errorf("all events should be text, got %v", ev.Kind)
		}
	}
}

// TestLLMCompleteParsesJSONUsage --json 路径：正文取自 response（思维链已剥离），
// token 计数从 input_tokens / output_tokens 读出，model 用 llm 回报的实际模型名。
func TestLLMCompleteParsesJSONUsage(t *testing.T) {
	open := llmTag("<", "think", ">")
	closeTag := llmTag("<", "/", "think", ">")
	payload := `[{"model":"minimax-m3","input_tokens":192,"output_tokens":38,"response":"` +
		open + `想一下` + closeTag + `\n\n收到"}]`
	cli := writeFakeCLI(t, "llm", "#!/bin/sh\ncat <<'EOF'\n"+payload+"\nEOF\n")
	e := &LLMEngine{BinPath: cli}

	resp, err := e.Complete(context.Background(), Request{
		Model:    "llm/minimax-m3",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "收到" {
		t.Errorf("Text = %q, want 收到（思维链应被剥离）", resp.Text)
	}
	if resp.InputTokens != 192 || resp.OutputTokens != 38 {
		t.Errorf("tokens = in:%d out:%d, want in:192 out:38", resp.InputTokens, resp.OutputTokens)
	}
	if resp.TotalTokens != 230 {
		t.Errorf("TotalTokens = %d, want 230", resp.TotalTokens)
	}
	// llm 回报的 model 优先于请求里的 "llm/minimax-m3"（前缀已剥）。
	if resp.Model != "minimax-m3" {
		t.Errorf("Model = %q, want minimax-m3", resp.Model)
	}
}

// TestLLMCompleteJSONPicksLastEntry 工具链场景：数组多项时取最后一项的正文。
func TestLLMCompleteJSONPicksLastEntry(t *testing.T) {
	payload := `[{"model":"m","response":"中间结果","input_tokens":10,"output_tokens":1},` +
		`{"model":"m","response":"最终答复","input_tokens":20,"output_tokens":2}]`
	cli := writeFakeCLI(t, "llm", "#!/bin/sh\ncat <<'EOF'\n"+payload+"\nEOF\n")
	e := &LLMEngine{BinPath: cli}

	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "最终答复" {
		t.Errorf("Text = %q, want 最终答复", resp.Text)
	}
	if resp.InputTokens != 20 {
		t.Errorf("InputTokens = %d, want 20（取最终项）", resp.InputTokens)
	}
}

// TestParseLLMJSONEntryNonJSON 非 JSON 输出 → ok=false（走纯文本兜底）。
func TestParseLLMJSONEntryNonJSON(t *testing.T) {
	if _, ok := parseLLMJSONEntry("就是一段纯文本"); ok {
		t.Error("plain text should not parse as JSON entry")
	}
	if _, ok := parseLLMJSONEntry(`{"response":"对象不是数组"}`); ok {
		t.Error("JSON object should not parse as entry array")
	}
	if _, ok := parseLLMJSONEntry(`[{"model":"m","response":"   "}]`); ok {
		t.Error("entry with blank response should be skipped")
	}
}

// TestLLMCompleteDoesNotPassSchemaToCLI magic-agent 收到 JSONSchema 时**不传**
// 给 simonw llm CLI（实测 minimax-m3 等 OpenAI Chat 类模型不支持 schema，
// 会报 "does not support schemas"）。schema 走 system prompt 提示词约束，
// 引擎里只做 response 后处理（extractJSONObjectStrict）。
func TestLLMCompleteDoesNotPassSchemaToCLI(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, "llm")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + log + "\nprintf '%s\\n' '[{\"model\":\"m\",\"input_tokens\":1,\"output_tokens\":2,\"response\":\"ok\"}]'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &LLMEngine{BinPath: cli}
	if _, err := e.Complete(context.Background(), Request{
		Model:    "minimax-m3",
		Messages: []Message{{Role: "user", Content: "hi"}},
		JSONSchema: &JSONSchema{
			Type:     "object",
			Required: []string{"score"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	if strings.Contains(args, "--schema ") {
		t.Errorf("--schema should NOT be passed to llm CLI (would fail on OpenAI Chat models): %q", args)
	}
}

// TestLLMBuildArgsOmitsSchemaWhenUnset 没 JSONSchema 时不带 --schema。
func TestLLMBuildArgsOmitsSchemaWhenUnset(t *testing.T) {
	e := &LLMEngine{}
	got := e.buildArgs(Request{Model: "minimax-m3"}, "hi")
	for _, a := range got {
		if a == "--schema" {
			t.Errorf("--schema should not appear when JSONSchema unset: %q", got)
		}
	}
}

// TestLLMCompleteAppliesJSONSchema 当请求带 JSONSchema 且模型把对象字符串化在
// response 里时，Complete 路径应把它解析回紧凑 JSON。
//
// 模拟真实场景：llm CLI 返回 <response> 字段是「字符串化的 JSON」，
// 而非已经 parse 好的对象。这是 OpenAI chat/completions 端点的常见形态。
func TestLLMCompleteAppliesJSONSchema(t *testing.T) {
	// 用 string 模型构造 envelope，response 是被 json.Marshal 后的对象。
	obj := map[string]any{
		"setting": 12, "character": 13, "pacing": 14,
		"hook": 15, "prose": 12, "density": 13, "style": 12,
		"issues":      []string{"意象重复"},
		"suggestions": []string{"替换具体细节"},
	}
	objJSON, _ := json.Marshal(obj)
	payload, _ := json.Marshal([]map[string]any{{
		"model":        "minimax-m3",
		"input_tokens": 100,
		"output_tokens": len(objJSON),
		"response":     string(objJSON), // ← 这里是「字符串化的 JSON」
	}})
	cli := writeFakeCLI(t, "llm", "#!/bin/sh\ncat <<'EOF'\n"+string(payload)+"\nEOF\n")
	e := &LLMEngine{BinPath: cli}

	resp, err := e.Complete(context.Background(), Request{
		Model:    "llm/minimax-m3",
		Messages: []Message{{Role: "user", Content: "打分"}},
		JSONSchema: &JSONSchema{
			Type:     "object",
			Required: []string{"setting", "character", "pacing", "issues", "suggestions"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// resp.Text 必须是合法 JSON 字符串，且 Required 顶层字段都在
	var back map[string]any
	if err := json.Unmarshal([]byte(resp.Text), &back); err != nil {
		t.Fatalf("Text not valid JSON: %v (got=%q)", err, resp.Text)
	}
	for _, k := range []string{"setting", "issues", "suggestions"} {
		if _, ok := back[k]; !ok {
			t.Errorf("missing %q in extracted: %s", k, resp.Text)
		}
	}
	// 紧凑化：不能带多余空白
	if strings.Contains(resp.Text, "\n") || strings.Contains(resp.Text, "  ") {
		t.Errorf("extracted JSON should be compact: %q", resp.Text)
	}
}

// TestLLMCompleteMissingRequiredKeepsRaw 当 model 输出缺 Required 字段时，
// 不改写 Response.Text，让上游 base/score 的宽松解析兜底。
func TestLLMCompleteMissingRequiredKeepsRaw(t *testing.T) {
	// 注意：payload 的 response 字段直接是 JSON 字符串（这是 llm CLI
	// 真实行为），所以不需要再次 json.Marshal。
	payload := `[{"model":"m","input_tokens":1,"output_tokens":2,"response":"{\"setting\":12,\"character\":13}"}]`
	cli := writeFakeCLI(t, "llm", "#!/bin/sh\ncat <<'EOF'\n"+payload+"\nEOF\n")
	e := &LLMEngine{BinPath: cli}

	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "打分"}},
		JSONSchema: &JSONSchema{
			Type:     "object",
			Required: []string{"setting", "hook"}, // hook 缺失
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Text 保留原始字符串（带原始字段，未经过 extract 改写）
	if !strings.Contains(resp.Text, `"character":13`) {
		t.Errorf("Text should keep raw content, got %q", resp.Text)
	}
}

// TestLLMDetectMissingCli CLI 不存在 → Detect 失败并给出路径。
func TestLLMDetectMissingCli(t *testing.T) {
	e := &LLMEngine{BinPath: "/nonexistent/llm"}
	ok, note := e.Detect()
	if ok {
		t.Fatal("Detect should fail for missing CLI")
	}
	if !strings.Contains(note, "/nonexistent/llm") {
		t.Errorf("note = %q, want the missing path", note)
	}
}
