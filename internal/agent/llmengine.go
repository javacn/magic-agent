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
//	Complete: llm prompt -n --no-stream [-m MODEL] [-s SYSTEM] PROMPT
//	Stream:   llm prompt -n            [-m MODEL] [-s SYSTEM] PROMPT
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
	"fmt"
	"os"
	"os/exec"
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
func (e *LLMEngine) buildArgs(req Request, prompt string) []string {
	args := []string{"prompt", "-n"}
	if m := stripModelPrefix(req.Model); m != "" {
		args = append(args, "-m", m)
	}
	if req.SystemPrompt != "" {
		args = append(args, "-s", req.SystemPrompt)
	}
	return append(args, prompt)
}

// Complete 实现 Engine：llm prompt --no-stream，一次性拿全文。
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

	args := append(e.buildArgs(req, prompt), "--no-stream")
	stdout, stderr, err := runCLI(ctx, bin, args...)
	if err != nil {
		return Response{}, wrapCliError("llm", stdout, stderr, err)
	}

	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return Response{}, fmt.Errorf("llm CLI returned empty output")
	}

	text, _ := splitThinkingTags(raw)
	if strings.TrimSpace(text) == "" {
		// 全是思维链、无正文：原样返回（可能模型把答案写在思维链里）。
		text = raw
	}
	return Response{
		Engine:  e.Name(),
		Text:    strings.TrimSpace(text),
		Model:   stripModelPrefix(req.Model),
		Latency: time.Since(start),
	}, nil
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
