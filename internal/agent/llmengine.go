package agent

// llmengine.go - llm 引擎：包装 simonw/LLM CLI（https://github.com/simonw/LLM）。
//
// 定位：用 llm 这一个 CLI 屏蔽背后全部模型差异（OpenAI/Anthropic/MiniMax/
// ollama/开源端点…），magic-agent 的 llm 引擎只做「转调 + 思维链剥离」，
// 模型注册、密钥、端点全部由 llm 自己管：
//
//	llm models                     列出可用模型（用户自己注册）
//	llm keys set <name>            存密钥
//	~/.llm-venv/bin/llm ...        本机 llm 安装位（Homebrew Python 3.14
//	                               pip truststore 坏，用 python3.12 venv 装）
//
// 用法：
//
//	magic-agent -e llm "问题"                # llm 的默认模型
//	magic-agent -e llm -m minimax-m3 "问题"  # 显式模型
//
// CLI 调用形式（两种模式共用 buildArgs）：
//
//	Complete: llm prompt -n --no-stream --json [-m MODEL] [-s SYSTEM] \
//	          [-o max_tokens N] [-o temperature T] PROMPT
//	Stream:   llm prompt -n            [-m MODEL] [-s SYSTEM] \
//	          [-o max_tokens N] [-o temperature T] PROMPT
//
// Complete 额外带 --json：同时拿正文与 token 计数（见下方 parseLLMJSONEntry）。
//
// 流式输出是**纯文本 stdout**（非 NDJSON）。MiniMax 等推理模型会把思维链
// 以标签形式直接混在正文里；本引擎用 thinkSplitter 状态机把标签块路由到
// thinking 通道。实测 llm 的 -R/--hide-reasoning 挡不住 MiniMax 的标签，
// 所以剥离逻辑必须自己做。
//
// 标签常量沿用 openai_client.go 的分段拼接手法：连续字面量会被写盘管线
// 当特殊 token 改写，导致源码与运行时值不一致。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// LLMEngine 包装 simonw/LLM CLI 实现 Engine + Streamer。
type LLMEngine struct {
	// BinPath 显式指定 CLI 路径（空 = 自动探测）。测试注入用。
	BinPath string
}

// Name 实现 Engine。
func (e *LLMEngine) Name() string { return "llm" }

// DefaultLLMTimeout llm 引擎单次尝试默认超时。
const DefaultLLMTimeout = 10 * time.Minute

// DefaultLLMModel 默认模型：空串 = llm 自己的默认模型（第一个注册的）。
const DefaultLLMModel = ""

// bin 探测 llm CLI：BinPath → MAGIC_AGENT_LLM_BIN → 常见安装位 → PATH。
// venv 位（~/.llm-venv）排在 Homebrew 之前：Homebrew Python 3.14 的
// pip truststore 有 bug，用户专门建的 venv 是能用的那份。
func (e *LLMEngine) bin() string {
	if e.BinPath != "" {
		return e.BinPath
	}
	if env := os.Getenv("MAGIC_AGENT_LLM_BIN"); env != "" {
		return env
	}
	candidates := []string{
		"/opt/homebrew/bin/llm",
		"/usr/local/bin/llm",
	}
	if home, err := os.UserHomeDir(); err == nil {
		// venv 优先（插到最前）：能跑的版本比 brew 装的半残版本重要。
		candidates = append([]string{
			home + "/.llm-venv/bin/llm",
		}, candidates...)
		candidates = append(candidates, home+"/.local/bin/llm")
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("llm"); err == nil {
		return p
	}
	return ""
}

// Detect 实现 Engine：找到 CLI 即可用（模型/密钥由 llm 自管，不在
// magic-agent 侧校验；模型错了运行时 llm 会报错）。
// 显式指定的路径（BinPath / MAGIC_AGENT_LLM_BIN）必须真实存在，
// 自动探测链中的候选路径本身就经过 os.Stat 过滤。
func (e *LLMEngine) Detect() (bool, string) {
	p := e.bin()
	if p == "" {
		return false, "llm CLI not found (pip install llm / brew, or set MAGIC_AGENT_LLM_BIN)"
	}
	if e.BinPath != "" || os.Getenv("MAGIC_AGENT_LLM_BIN") != "" {
		if _, err := os.Stat(p); err != nil {
			return false, fmt.Sprintf("llm CLI not found at %s", p)
		}
	}
	return true, p
}

// buildArgs 构造 llm prompt 参数（Complete 与 Stream 共用）。
// Complete 模式额外追加 --no-stream。
//
// 模型选项经 simonw llm 的 `-o key value` 透传：
//
//	--max-tokens N   → -o max_tokens N
//	--temperature T  → -o temperature T
//
// 具体选项名由模型插件决定；未注册该选项时 llm 会报错，属于调用方
// 传错参数，不在这里静默吞掉。
func (e *LLMEngine) buildArgs(req Request, prompt string) []string {
	args := []string{"prompt", "-n"}
	if m := stripModelPrefix(req.Model); m != "" {
		args = append(args, "-m", m)
	}
	if req.SystemPrompt != "" {
		args = append(args, "-s", req.SystemPrompt)
	}
	if req.MaxTokens > 0 {
		args = append(args, "-o", "max_tokens", strconv.Itoa(req.MaxTokens))
	}
	if req.Temperature != nil {
		args = append(args, "-o", "temperature", formatTemperature(*req.Temperature))
	}
	return append(args, prompt)
}

// formatTemperature 把温度格式化为最短十进制表示（去掉多余的尾零）。
func formatTemperature(t float64) string {
	return strconv.FormatFloat(t, 'f', -1, 64)
}

// encodeJSONSchemaInline 把 JSONSchema 序列化成给 simonw llm 的
// `--schema` 接受的 JSON Schema DSL。
//
// simonw llm 的 --schema 既支持 DSL（如 "name str, age int, bio: 简介"）
// 也支持完整 JSON Schema。我们直接传 JSON Schema 字符串，让 llm 解析。
func encodeJSONSchemaInline(s *JSONSchema) (string, bool) {
	if s == nil || !strings.EqualFold(s.Type, "object") {
		return "", false
	}
	// 顶层仅保留 type / properties / required 三键，剔除冗余字段。
	doc := map[string]any{
		"type":       "object",
		"properties": s.Properties,
	}
	if len(s.Required) > 0 {
		doc["required"] = s.Required
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// Complete 实现 Engine：llm prompt --json --no-stream，一次性拿全文 + token 计数。
//
// 为什么加 --json：纯文本模式下 llm 不吐 token 数，上游（如 magic-video）
// 靠 output_tokens 判断「输出撞了 max_tokens 上限、正文可能被截断」。
// --json 输出同 `llm logs --json` 的结构（数组，每项含 response /
// input_tokens / output_tokens），解析失败时退回纯文本路径，不引入新的失败点。
func (e *LLMEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return Response{}, fmt.Errorf("llm CLI not found; set MAGIC_AGENT_LLM_BIN")
	}

	prompt := FlattenPrompt("", req.Messages, false)
	if prompt == "" {
		return Response{}, fmt.Errorf("llm: empty prompt")
	}

	args := append(e.buildArgs(req, prompt), "--no-stream", "--json")
	// 注意：simonw llm CLI 不为 OpenAI Chat 类模型提供原生 schema 约束
	//（实测 minimax-m3 报 "does not support schemas"）。所以 magic-agent
	// **不在 CLI 层透传** schema —— 上游（magic-video）已经渲染进
	// system prompt 作为提示词约束，llm 引擎这里只做 response 后处理
	//（见下方 extractJSONObjectStrict）。其他引擎（codebuddy）有自己的
	// --json-schema flag，由 magic-agent 各自的 Complete 实现处理。
	_ = req.JSONSchema
	stdout, stderr, err := runCLI(ctx, bin, args...)
	if err != nil {
		return Response{}, wrapCliError("llm", stdout, stderr, err)
	}

	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return Response{}, fmt.Errorf("llm CLI returned empty output")
	}

	if entry, ok := parseLLMJSONEntry(raw); ok {
		text, _ := splitThinkingTags(entry.Response)
		if strings.TrimSpace(text) == "" {
			// 全是思维链、无正文：原样返回（可能模型把答案写在思维链里）。
			text = entry.Response
		}
		// schema 严格匹配后处理：把 model 字符串化的 JSON 对象压平为
		// 紧凑串，便于上游 json.Unmarshal 一次成功。
		if req.JSONSchema != nil {
			if extracted, ok := extractJSONObjectStrict(text, req.JSONSchema); ok {
				text = extracted
			}
		}
		model := entry.Model
		if model == "" {
			model = stripModelPrefix(req.Model)
		}
		total := entry.InputTokens + entry.OutputTokens
		return Response{
			Engine:       e.Name(),
			Text:         strings.TrimSpace(text),
			Model:        model,
			Latency:      time.Since(start),
			InputTokens:  entry.InputTokens,
			OutputTokens: entry.OutputTokens,
			TotalTokens:  total,
		}, nil
	}

	// 兜底：--json 解析不出来（llm 版本差异 / 输出被污染）时按纯文本处理。
	text, _ := splitThinkingTags(raw)
	if strings.TrimSpace(text) == "" {
		text = raw
	}
	return Response{
		Engine:  e.Name(),
		Text:    strings.TrimSpace(text),
		Model:   stripModelPrefix(req.Model),
		Latency: time.Since(start),
	}, nil
}

// llmJSONEntry 是 `llm prompt --json` 数组里一项的所需子集。
type llmJSONEntry struct {
	Model        string `json:"model"`
	Response     string `json:"response"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// parseLLMJSONEntry 解析 `llm prompt --json` 输出，取最后一项有正文的条目。
//
// 单次 prompt 通常只有一项；带工具链（tools chain）时会有多项，最终答复
// 在最后一项，所以从后往前找第一个 response 非空的。
// 输出不是 JSON 数组时返回 ok=false，调用方走纯文本兜底。
func parseLLMJSONEntry(raw string) (llmJSONEntry, bool) {
	var entries []llmJSONEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return llmJSONEntry{}, false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if strings.TrimSpace(entries[i].Response) != "" {
			return entries[i], true
		}
	}
	return llmJSONEntry{}, false
}

// Stream 实现 Streamer：llm prompt 默认流式，逐行读纯文本 stdout，
// thinkSplitter 把标签块路由到 thinking 通道。
func (e *LLMEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return StreamResult{}, fmt.Errorf("llm CLI not found; set MAGIC_AGENT_LLM_BIN")
	}

	prompt := FlattenPrompt("", req.Messages, false)
	if prompt == "" {
		return StreamResult{}, fmt.Errorf("llm: empty prompt")
	}

	args := e.buildArgs(req, prompt)
	sp := &thinkSplitter{OnEvent: onEvent}

	err := runStreamCLI(ctx, bin, args, func(line string) error {
		sp.feed(line + "\n")
		return nil
	})
	if err != nil {
		return StreamResult{}, err
	}

	text, thinking := sp.finish()
	if text == "" {
		return StreamResult{}, fmt.Errorf("llm CLI returned empty result")
	}
	return StreamResult{
		Response: Response{
			Engine:  e.Name(),
			Text:    text,
			Model:   stripModelPrefix(req.Model),
			Latency: time.Since(start),
		},
		Thinking: thinking,
	}, nil
}

// splitThinkingTags 把带标签的混合文本拆为 (正文, 思维链)。
// 供 Complete 模式使用；Stream 模式用 thinkSplitter 状态机增量处理。
func splitThinkingTags(s string) (text, thinking string) {
	var tb, kb strings.Builder
	rest := s
	for {
		i := strings.Index(rest, thinkOpenTag)
		if i < 0 {
			tb.WriteString(rest)
			break
		}
		tb.WriteString(rest[:i])
		after := rest[i+len(thinkOpenTag):]
		j := strings.Index(after, thinkCloseTag)
		if j < 0 {
			// 有开无闭：残余整段视为思维链。
			kb.WriteString(after)
			break
		}
		kb.WriteString(after[:j])
		rest = after[j+len(thinkCloseTag):]
	}
	return tb.String(), kb.String()
}

// thinkSplitter 流式思维链分离状态机。
//
// llm 的流式输出是逐行纯文本。关键事实：思维链标签本身不含换行符，
// 而 Scanner 按行交付完整行，因此标签永远完整落在某一行内、不会被
// 行边界切开——无需前缀保持（holdback）。行内出现被换行打断的伪标签
//（如 "<th\nink>"）在原始字节流里本来就不是标签，与非流式的
// splitThinkingTags 判定一致：视为正文。
//
// 状态：
//
//	inThink   当前是否处于思维链块内（跨行保持，直至闭标签或 EOF）
//	sawText   已发出过实质正文（首个实质内容前的纯空白段不转发，
//	          消化标签独立成行时残留的换行）
//	sawThink  同上，针对思维链通道
type thinkSplitter struct {
	OnEvent   func(StreamEvent)
	Text      strings.Builder
	Thinking  strings.Builder
	inThink   bool
	sawText   bool
	sawThink  bool
}

// feed 处理一行流式输出（line 应含行尾换行符，以保留多行结构）。
func (sp *thinkSplitter) feed(line string) {
	s := line
	for {
		if sp.inThink {
			j := strings.Index(s, thinkCloseTag)
			if j < 0 {
				sp.emitThinking(s)
				return
			}
			sp.emitThinking(s[:j])
			s = s[j+len(thinkCloseTag):]
			sp.inThink = false
			continue
		}
		i := strings.Index(s, thinkOpenTag)
		if i < 0 {
			sp.emitText(s)
			return
		}
		sp.emitText(s[:i])
		s = s[i+len(thinkOpenTag):]
		sp.inThink = true
	}
}

// finish 收尾。未闭合的思维链块保持到 EOF（inThink 态下的全部内容
// 已在 feed 中计入 thinking），无额外缓冲需要冲刷。
func (sp *thinkSplitter) finish() (text, thinking string) {
	return strings.TrimSpace(sp.Text.String()), strings.TrimSpace(sp.Thinking.String())
}

// emitText 发出一段正文增量。首个实质内容前的纯空白段静默吞掉
//（builder 不写、事件不发），避免标签行的换行污染输出。
func (sp *thinkSplitter) emitText(s string) {
	if s == "" {
		return
	}
	if !sp.sawText && strings.TrimSpace(s) == "" {
		return
	}
	sp.sawText = true
	sp.Text.WriteString(s)
	if sp.OnEvent != nil {
		sp.OnEvent(StreamEvent{Kind: KindText, Text: s})
	}
}

// emitThinking 发出一段思维链增量（同样的首段空白吞并规则）。
func (sp *thinkSplitter) emitThinking(s string) {
	if s == "" {
		return
	}
	if !sp.sawThink && strings.TrimSpace(s) == "" {
		return
	}
	sp.sawThink = true
	sp.Thinking.WriteString(s)
	if sp.OnEvent != nil {
		sp.OnEvent(StreamEvent{Kind: KindThinking, Text: s})
	}
}
