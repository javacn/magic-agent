package agent

// llmengine.go - llm 引擎：按 ~/.magic-agent/models.json 直连 OpenAI 兼容端点。
//
// 这是「第 4 个引擎」，与 claude / codebuddy / trae 并列，共用同一条
// Runner 超时重试 + output.go envelope 路径。用法：
//
//	magic-agent -e llm -m MiniMax-M3 "问题"       # 按配置里的 id 指定
//	magic-agent -e llm -m minimax-nothink "问题"  # 同模型不同变体（关思维链）
//	magic-agent -e llm "问题"                     # 用数组首条
//
// 配置是扁平数组，每条自带完整 url + apiKey（见 models.go）。本引擎只走
// HTTP，不再有「按 api 字段路由」或「委托其他引擎」的分支 —— 需要
// codebuddy / trae / claude CLI 时直接用各自的 -e，不绕 llm。

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// LLMEngine 实现 Engine + Streamer：按 models.json 直连 HTTP 端点。
type LLMEngine struct{}

// Name 实现 Engine。
func (e *LLMEngine) Name() string { return "llm" }

// DefaultLLMTimeout llm 引擎单次尝试默认超时。
const DefaultLLMTimeout = 10 * time.Minute

// Detect 实现 Engine：只要有可用的模型配置就算可用。
func (e *LLMEngine) Detect() (bool, string) {
	entries, path, err := LoadModels()
	if err != nil {
		return false, err.Error()
	}
	note := fmt.Sprintf("%s (%d models, default=%s)", path, len(entries), entries[0].ID)
	if !entries[0].HasAPIKey() {
		note += " [default has no apiKey]"
	}
	return true, note
}

// Complete 实现 Engine：解析模型 → 发一次 HTTP chat completion。
func (e *LLMEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()

	entries, path, err := LoadModels()
	if err != nil {
		return Response{}, err
	}
	entry, err := ResolveModel(entries, req.Model)
	if err != nil {
		return Response{}, fmt.Errorf("%w (config: %s)", err, path)
	}

	text, err := chatComplete(ctx, httpClientFor(entry), entry, entry.WireModel(), req.SystemPrompt, req.Messages)
	if err != nil {
		return Response{}, fmt.Errorf("%s: %w", entry.DisplayName(), err)
	}
	if strings.TrimSpace(text) == "" {
		return Response{}, fmt.Errorf("model %q returned empty content", entry.ID)
	}
	return Response{
		Text:    text,
		Model:   entry.ID, // 调用方视角：便于区分同模型的不同变体
		Latency: time.Since(start),
	}, nil
}

// Stream 实现 Streamer：解析模型 → SSE 流式
// （reasoning_content → thinking 增量，正文 → text 增量）。
func (e *LLMEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()

	entries, path, err := LoadModels()
	if err != nil {
		return StreamResult{}, err
	}
	entry, err := ResolveModel(entries, req.Model)
	if err != nil {
		return StreamResult{}, fmt.Errorf("%w (config: %s)", err, path)
	}

	res, err := chatStream(ctx, httpClientFor(entry), entry, entry.WireModel(), req.SystemPrompt, req.Messages, onEvent)
	if err != nil {
		return StreamResult{}, fmt.Errorf("%s: %w", entry.DisplayName(), err)
	}
	if strings.TrimSpace(res.Text) == "" {
		return StreamResult{}, fmt.Errorf("model %q returned empty content", entry.ID)
	}
	res.Response.Engine = e.Name()
	res.Response.Model = entry.ID
	res.Response.Latency = time.Since(start)
	return res, nil
}
