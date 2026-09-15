package agent

// llmengine.go - llm 引擎：按 ~/.magic-agent/models.json 路由到具体后端。
//
// 这是「第 4 个引擎」，与 claude / codebuddy / trae 并列，共用同一条
// Runner 超时重试 + output.go envelope 路径。用法：
//
//	magic-agent -e llm -m minimax/MiniMax-M3 "问题"
//	magic-agent -e llm "问题"                    # 用 models.json 的 default
//
// 路由规则（按 provider.api 字段）：
//
//	openai-completions（默认）  internal/agent 内置 HTTP 客户端
//	ollama                    shell 调用 `ollama run <model>`
//	codebuddy-cli             委托 CodeBuddyEngine（零改动复用）
//	trae-cli                  委托 TraeEngine（零改动复用）
//	claude-cli                委托 ClaudeEngine（零改动复用）
//
// 委托而非重写的原因：这三家 CLI 的参数构造、envelope 解析、回显剥离
// 已在对应引擎里验证过；重写会产生两份会漂移的实现，还会引入
// internal/llm ↔ internal/agent 的依赖回环。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// LLMEngine 实现 Engine：按 models.json 路由到 HTTP / ollama / 已有 CLI 引擎。
type LLMEngine struct{}

// Name 实现 Engine。
func (e *LLMEngine) Name() string { return "llm" }

// DefaultLLMTimeout llm 引擎单次尝试默认超时。
const DefaultLLMTimeout = 10 * time.Minute

// Detect 实现 Engine：只要有可用的模型配置就算可用。
func (e *LLMEngine) Detect() (bool, string) {
	mf, path, err := LoadModels()
	if err != nil {
		return false, err.Error()
	}
	provs := mf.effectiveProviders()
	def := mf.DefaultModel()
	if def == "" {
		def = "(第一个模型)"
	}
	return true, fmt.Sprintf("%s (%d providers, default=%s)", path, len(provs), def)
}

// Complete 实现 Engine：解析模型 → 路由到对应后端。
func (e *LLMEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()

	mf, path, err := LoadModels()
	if err != nil {
		return Response{}, err
	}
	provider, bareModel, err := ResolveModel(mf, req.Model)
	if err != nil {
		return Response{}, fmt.Errorf("%w (config: %s)", err, path)
	}

	switch provider.EffectiveAPI() {
	case APIOllama:
		text, err := completeOllama(ctx, provider, bareModel, req)
		if err != nil {
			return Response{}, err
		}
		return Response{
			Text:    text,
			Model:   provider.DisplayName() + "/" + bareModel,
			Latency: time.Since(start),
		}, nil

	case APICodeBuddy, APITrae, APIClaude:
		return completeViaCLIEngine(ctx, provider, bareModel, req)

	case APIOpenAI:
		if strings.TrimSpace(provider.BaseURL) == "" {
			return Response{}, fmt.Errorf("provider %q: baseUrl is required for api=%s",
				provider.DisplayName(), APIOpenAI)
		}
		text, err := chatComplete(ctx, httpClientFor(provider), provider, bareModel, req.SystemPrompt, req.Messages)
		if err != nil {
			return Response{}, fmt.Errorf("%s: %w", provider.DisplayName(), err)
		}
		if strings.TrimSpace(text) == "" {
			return Response{}, fmt.Errorf("provider %q returned empty content", provider.DisplayName())
		}
		return Response{
			Text:    text,
			Model:   provider.DisplayName() + "/" + bareModel,
			Latency: time.Since(start),
		}, nil

	default:
		return Response{}, fmt.Errorf("provider %q: unsupported api %q (want %s | %s | %s | %s | %s)",
			provider.DisplayName(), provider.API,
			APIOpenAI, APIOllama, APICodeBuddy, APITrae, APIClaude)
	}
}

// completeViaCLIEngine 把请求委托给已有的 CLI 引擎实例。
//
// 直接构造引擎并调其 Complete：这里不经过 Runner（外层已经有一次
// Runner 编排），因此不需要超时/重试的二次叠加。
func completeViaCLIEngine(ctx context.Context, p Provider, model string, req Request) (Response, error) {
	var inner Engine
	switch p.EffectiveAPI() {
	case APICodeBuddy:
		inner = &CodeBuddyEngine{}
	case APITrae:
		inner = &TraeEngine{}
	case APIClaude:
		inner = &ClaudeEngine{}
	default:
		return Response{}, fmt.Errorf("provider %q: cannot delegate api %q", p.DisplayName(), p.API)
	}

	// 子引擎可用性预检：给出可操作提示，而不是让 CLI 调用裸失败。
	if ok, note := inner.Detect(); !ok {
		return Response{}, fmt.Errorf("provider %q delegates to %s engine which is unavailable: %s",
			p.DisplayName(), inner.Name(), note)
	}

	sub := Request{
		Engine:       inner.Name(),
		Model:        model,
		SystemPrompt: req.SystemPrompt,
		Tools:        req.Tools,
		Messages:     req.Messages,
		Timeout:      req.Timeout,
	}
	resp, err := inner.Complete(ctx, sub)
	if err != nil {
		return Response{}, fmt.Errorf("provider %q (%s): %w", p.DisplayName(), inner.Name(), err)
	}
	// 覆盖 model 标识为配置里的全名，便于调用方区分路由来源。
	resp.Model = p.DisplayName() + "/" + model
	return resp, nil
}

// Stream 实现 Streamer：按 provider 路由。
//
//	openai-completions  SSE 流（reasoning_content → thinking 增量）
//	codebuddy/trae/claude-cli  委托子引擎 Stream（零改动复用）
//	ollama              不支持（CLI 无流式协议）→ 明确报错
func (e *LLMEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()

	mf, path, err := LoadModels()
	if err != nil {
		return StreamResult{}, err
	}
	provider, bareModel, err := ResolveModel(mf, req.Model)
	if err != nil {
		return StreamResult{}, fmt.Errorf("%w (config: %s)", err, path)
	}

	switch provider.EffectiveAPI() {
	case APIOllama:
		return StreamResult{}, fmt.Errorf("provider %q: ollama 后端暂不支持流式（api=ollama），请去掉 --stream 或换 openai-completions 后端",
			provider.DisplayName())

	case APICodeBuddy, APITrae, APIClaude:
		return streamViaCLIEngine(ctx, provider, bareModel, req, onEvent)

	case APIOpenAI:
		if strings.TrimSpace(provider.BaseURL) == "" {
			return StreamResult{}, fmt.Errorf("provider %q: baseUrl is required for api=%s",
				provider.DisplayName(), APIOpenAI)
		}
		res, err := chatStream(ctx, httpClientFor(provider), provider, bareModel, req.SystemPrompt, req.Messages, onEvent)
		if err != nil {
			return StreamResult{}, fmt.Errorf("%s: %w", provider.DisplayName(), err)
		}
		if strings.TrimSpace(res.Text) == "" {
			return StreamResult{}, fmt.Errorf("provider %q returned empty content", provider.DisplayName())
		}
		res.Response.Engine = e.Name()
		res.Response.Model = provider.DisplayName() + "/" + bareModel
		res.Response.Latency = time.Since(start)
		return res, nil

	default:
		return StreamResult{}, fmt.Errorf("provider %q: unsupported api %q for streaming",
			provider.DisplayName(), provider.API)
	}
}

// streamViaCLIEngine 流式委托给子引擎。
func streamViaCLIEngine(ctx context.Context, p Provider, model string, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	var inner Engine
	switch p.EffectiveAPI() {
	case APICodeBuddy:
		inner = &CodeBuddyEngine{}
	case APITrae:
		inner = &TraeEngine{}
	case APIClaude:
		inner = &ClaudeEngine{}
	default:
		return StreamResult{}, fmt.Errorf("provider %q: cannot delegate api %q", p.DisplayName(), p.API)
	}
	streamer, ok := inner.(Streamer)
	if !ok {
		return StreamResult{}, fmt.Errorf("provider %q: engine %s does not support streaming", p.DisplayName(), inner.Name())
	}
	if ok, note := inner.Detect(); !ok {
		return StreamResult{}, fmt.Errorf("provider %q delegates to %s engine which is unavailable: %s",
			p.DisplayName(), inner.Name(), note)
	}
	sub := Request{
		Engine:       inner.Name(),
		Model:        model,
		SystemPrompt: req.SystemPrompt,
		Tools:        req.Tools,
		Messages:     req.Messages,
		Timeout:      req.Timeout,
	}
	res, err := streamer.Stream(ctx, sub, onEvent)
	if err != nil {
		return StreamResult{}, fmt.Errorf("provider %q (%s): %w", p.DisplayName(), inner.Name(), err)
	}
	res.Response.Engine = e2Name(p, inner)
	res.Response.Model = p.DisplayName() + "/" + model
	return res, nil
}

// e2Name 返回 llm 引擎名（streamViaCLIEngine 里统一标识）。
func e2Name(_ Provider, _ Engine) string { return "llm" }

// completeOllama 走本地 ollama：`ollama run <model> <prompt>`。
//
// ollama 无 system/多轮参数（除 /api/chat 外），因此把 system 与消息
// 扁平化进 prompt，复用 FlattenPrompt 的标签约定。
func completeOllama(ctx context.Context, p Provider, model string, req Request) (string, error) {
	bin := ollamaBin()
	if bin == "" {
		return "", fmt.Errorf("provider %q: ollama not found (install ollama or set MAGIC_AGENT_OLLAMA_BIN)",
			p.DisplayName())
	}

	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return "", fmt.Errorf("provider %q: empty prompt", p.DisplayName())
	}

	stdout, stderr, err := runCLI(ctx, bin, "run", model, prompt)
	if err != nil {
		return "", wrapCliError("ollama", stdout, stderr, err)
	}
	text := stripOllamaSpinner(stdout)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("ollama returned empty output")
	}
	return text, nil
}

// stripOllamaSpinner 去掉 ollama CLI 输出的 braille 旋转指示符残留
// （非 TTY 下通常不出现，但部分版本仍会带出来）。
func stripOllamaSpinner(s string) string {
	var b strings.Builder
	for _, r := range s {
		// U+2800–U+28FF 是 braille patterns，ollama 用它画 spinner。
		if r >= 0x2800 && r <= 0x28FF {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// ollamaBin 探测 ollama 可执行文件路径。
func ollamaBin() string {
	if env := os.Getenv("MAGIC_AGENT_OLLAMA_BIN"); env != "" {
		return env
	}
	for _, p := range []string{"/opt/homebrew/bin/ollama", "/usr/local/bin/ollama"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("ollama"); err == nil {
		return p
	}
	return ""
}
