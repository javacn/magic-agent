package agent

// llmhttp.go - llm 引擎「配置文件模型」的直连执行（models.json 条目 → OpenAI 兼容端点）。
//
// 为什么需要它：见 llmmodels.go 的文件头 —— 配置里的 id 是唯一标识，真实模型名会重复，
// 变体差异（extraBody）只有直连才表达得出来。llm CLI 那条路只服务「llm 自己注册的
// model_id」，两者并存：命中配置文件 → 直连；否则原样委托 llm CLI。
//
// 与 llm CLI 路径的差异（都是刻意的）：
//
//	会话续接：直连没有会话库 → 不支持 --session / -c。显式要求续接时报错而不是静默丢上下文。
//	思考过程：非流式走 splitThinkingTags、流式走 thinkSplitter（与 CLI 路径同一套剥离逻辑）；
//	          MiniMax 这类把思维链以标签混进 content 的模型，两端都能分流。
//	参数透传：max_tokens / temperature 优先用调用方（req）给的值，其次用条目自带的默认；
//	          extraBody 整体 merge 到请求体顶层，但 model / messages / stream 由本文件最终设定，
//	          避免配置写错把这三个关键键覆盖掉。
//	错误：统一 `HTTP <code>: <片段>`（runner.isRetryable 靠 "429/503/timeout" 这类子串判重试）。
//	Token 计数：端点给了 usage 就回填（llm CLI 路径靠 --json，直连靠响应体）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// llmConfiguredClient 直连使用的 HTTP 客户端。超时按条目/调用方取，这里只兜底。
func llmConfiguredClient() *http.Client {
	return &http.Client{Timeout: 0} // 超时统一走 ctx（条目 timeout / CLI -t），避免两套超时打架
}

// llmConfiguredTimeout 解析本次调用的超时：调用方 req.Timeout > 条目 timeout（秒）> 引擎默认。
func llmConfiguredTimeout(m LLMConfigModel, req Request) time.Duration {
	if req.Timeout > 0 {
		return req.Timeout
	}
	if m.Timeout > 0 {
		return time.Duration(m.Timeout) * time.Second
	}
	return DefaultLLMTimeout
}

// buildConfiguredBody 组装请求体。stream 由调用方决定。
func buildConfiguredBody(m LLMConfigModel, req Request, stream bool) (map[string]any, error) {
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if s := strings.TrimSpace(req.SystemPrompt); s != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": s})
	}
	for _, msg := range req.Messages {
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "user"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": msg.Content})
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("llm: empty prompt")
	}

	// 附件（截图）→ OpenAI 兼容的多模态 content parts，挂到**最后一条 user 消息**上
	//（就是本次提问）。实测：MiniMax-M3 直连收 image_url(data URL) 能正确识图
	//（2026-09-17：纯红 64x64 PNG → 答「红色」）。
	if len(req.Attachments) > 0 {
		if err := attachToLastUserMessage(msgs, req.Attachments); err != nil {
			return nil, err
		}
	}

	body := map[string]any{}
	for k, v := range m.ExtraBody { // extraBody 先铺（变体差异常在这）
		body[k] = v
	}
	body["model"] = m.WireModel()
	body["messages"] = msgs
	body["stream"] = stream

	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	} else if m.MaxOutputTokens > 0 {
		body["max_tokens"] = m.MaxOutputTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	} else if m.Temperature != nil {
		body["temperature"] = *m.Temperature
	}
	return body, nil
}

// attachToLastUserMessage 把附件塞进最后一条 user 消息的 content：
//
//	图片  → {"type":"image_url","image_url":{"url":"data:<mime>;base64,<...>"}}（在前）
//	其他  → 路径清单并入文本（协议里没有通用文件类型）
//
// 找不到 user 消息（理论上不会：CLI 层总要给提示词）时返回错误，不静默丢附件。
func attachToLastUserMessage(msgs []map[string]any, atts []Attachment) error {
	idx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i]["role"] == "user" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("llm: 附件需要一条 user 消息承载，本次没有")
	}
	text, _ := msgs[idx]["content"].(string)

	parts := make([]map[string]any, 0, len(atts)+1)
	var rest []Attachment
	for _, a := range atts {
		if !a.IsImage() {
			rest = append(rest, a)
			continue
		}
		data, err := readAttachmentBase64(a)
		if err != nil {
			return err
		}
		mime := a.MIME
		if mime == "" {
			mime = "image/png"
		}
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + mime + ";base64," + data},
		})
	}
	parts = append(parts, map[string]any{"type": "text", "text": appendAttachmentSection(text, rest)})
	msgs[idx]["content"] = parts
	return nil
}

// doConfiguredRequest 发一次请求；非 2xx 统一报 `HTTP <code>: <片段>`。
func doConfiguredRequest(ctx context.Context, m LLMConfigModel, req Request, body map[string]any) (*http.Response, error) {
	ep := m.Endpoint()
	if ep == "" {
		return nil, fmt.Errorf("llm: model %q has no url in models.json", m.ID)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ep, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if m.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+m.APIKey)
	}
	if stream, _ := body["stream"].(bool); stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := llmConfiguredClient().Do(httpReq)
	if err != nil {
		return nil, err // 网络类错误原样返回：isRetryable 靠 "connection refused" 等子串判定
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, compactSnippet(string(snippet)))
	}
	return resp, nil
}

// compactSnippet 把错误正文压成单行短串（日志/envelope 里可读）。
func compactSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// checkConfiguredSession 直连没有会话库：显式要求续接时明确报错，不静默丢上下文。
func checkConfiguredSession(m LLMConfigModel, req Request) error {
	if strings.TrimSpace(req.SessionID) != "" || req.Continue {
		return fmt.Errorf("llm 配置文件模型 %q 不支持会话续接（直连无会话库）：请去掉 --session/-c，或改用 llm CLI 注册的模型", m.ID)
	}
	return nil
}

// completeConfigured 非流式直连。返回体与 llm CLI 路径同形（含 token 计数）。
func (e *LLMEngine) completeConfigured(ctx context.Context, m LLMConfigModel, req Request) (Response, error) {
	start := time.Now()
	if err := checkConfiguredSession(m, req); err != nil {
		return Response{}, err
	}
	body, err := buildConfiguredBody(m, req, false)
	if err != nil {
		return Response{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, llmConfiguredTimeout(m, req))
	defer cancel()
	resp, err := doConfiguredRequest(cctx, m, req, body)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}
	out, err := parseConfiguredChat(raw)
	if err != nil {
		return Response{}, err
	}

	text, thinking := splitThinkingTags(out.Content)
	if strings.TrimSpace(text) == "" {
		// 全是思维链（模型把答案写进了 reasoning）→ 原样返回，别丢内容
		text = out.Content
	}
	if strings.TrimSpace(text) == "" && strings.TrimSpace(out.Reasoning) != "" {
		text = out.Reasoning
	}
	if req.JSONSchema != nil {
		if extracted, ok := extractJSONObjectStrict(text, req.JSONSchema); ok {
			text = extracted
		}
	}
	_ = thinking // 非流式路径不单独回传思考（与 llm CLI 路径一致）
	return Response{
		Engine: e.Name(),
		Text:   strings.TrimSpace(text),
		// Model 回填 **配置里的 id**（调用标识），不是线上真实名：
		// 真实模型名会重复（MiniMax-M3 与 minimax-nothink 同名），只有 id 能唯一指认
		// 「这一轮到底用的哪条配置」；真实名按 id 查配置文件即可。
		Model:        m.ID,
		Latency:      time.Since(start),
		InputTokens:  out.PromptTokens,
		OutputTokens: out.CompletionTokens,
		TotalTokens:  out.PromptTokens + out.CompletionTokens,
	}, nil
}

// configuredChat 非流式响应里我们需要的部分。
type configuredChat struct {
	Content          string
	Reasoning        string
	PromptTokens     int
	CompletionTokens int
}

func parseConfiguredChat(raw []byte) (configuredChat, error) {
	var doc struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			Text string `json:"text"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return configuredChat{}, fmt.Errorf("llm: 响应不是 JSON（%w）: %s", err, compactSnippet(string(raw)))
	}
	if doc.Error != nil && doc.Error.Message != "" {
		return configuredChat{}, fmt.Errorf("llm: %s", doc.Error.Message)
	}
	if len(doc.Choices) == 0 {
		return configuredChat{}, fmt.Errorf("llm: 响应没有 choices: %s", compactSnippet(string(raw)))
	}
	c := doc.Choices[0]
	content := c.Message.Content
	if content == "" {
		content = c.Text // 少数端点（completion 风格）用 text
	}
	return configuredChat{
		Content:          content,
		Reasoning:        c.Message.ReasoningContent,
		PromptTokens:     doc.Usage.PromptTokens,
		CompletionTokens: doc.Usage.CompletionTokens,
	}, nil
}

// streamConfigured 流式直连（SSE）。逐块 delta 经 tagGuard 防标签被切块后,
// 交给与 CLI 路径同一个 thinkSplitter 分流 thinking/text。
func (e *LLMEngine) streamConfigured(ctx context.Context, m LLMConfigModel, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()
	if err := checkConfiguredSession(m, req); err != nil {
		return StreamResult{}, err
	}
	body, err := buildConfiguredBody(m, req, true)
	if err != nil {
		return StreamResult{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, llmConfiguredTimeout(m, req))
	defer cancel()
	resp, err := doConfiguredRequest(cctx, m, req, body)
	if err != nil {
		return StreamResult{}, err
	}
	defer resp.Body.Close()

	sp := &thinkSplitter{OnEvent: onEvent}
	guard := &tagGuard{}
	var usage struct {
		PromptTokens     int
		CompletionTokens int
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20) // 单行上限 16MB（与 runStreamCLI 同口径）
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue // 心跳/注释行
		}
		if !strings.HasPrefix(line, "data:") {
			continue // event:/id: 等其它 SSE 字段
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // 非 JSON 片段（有些网关会插日志行）→ 跳过，不打断整条流
		}
		if chunk.Usage != nil {
			usage.PromptTokens = chunk.Usage.PromptTokens
			usage.CompletionTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.ReasoningContent != "" {
			sp.feedThinking(d.ReasoningContent) // 独立 reasoning 通道：无标签，直接进 thinking
		}
		if d.Content != "" {
			sp.feed(guard.push(d.Content))
		}
	}
	if err := sc.Err(); err != nil {
		return StreamResult{}, err
	}
	sp.feed(guard.flush())

	text, thinking := sp.finish()
	if text == "" {
		return StreamResult{}, fmt.Errorf("llm: 流式响应没有正文（配置条目 %s → %s）", m.ID, m.WireModel())
	}
	return StreamResult{
		Response: Response{
			Engine:       e.Name(),
			Text:         text,
			Model:        m.ID, // id（调用标识）—— 见 completeConfigured 里的说明
			Latency:      time.Since(start),
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
			TotalTokens:  usage.PromptTokens + usage.CompletionTokens,
		},
		Thinking: thinking,
	}, nil
}

// tagGuard 防「思维链标签被 SSE 分块切开」：把尾部可能是 `<think>` / `</think>`
// 前缀的那几个字符暂扣，等下一块补齐再一起投喂状态机。
// （llm CLI 路径靠「Scanner 按行交付、标签不含换行」天然免疫，直连没有这层保证。）
type tagGuard struct {
	buf string
}

// feedThinking 把一段**已知属于思维链**的文本送进 thinking 通道。
// 独立 reasoning 通道（如 MiniMax 的 delta.reasoning_content）没有标签，
// 不能走 feed（那会被当正文）—— 见 streamConfigured。
func (sp *thinkSplitter) feedThinking(s string) {
	sp.emitThinking(s)
}

func (g *tagGuard) push(s string) string {
	if s == "" {
		return ""
	}
	g.buf += s
	keep := prefixKeep(g.buf, thinkOpenTag)
	if k := prefixKeep(g.buf, thinkCloseTag); k > keep {
		keep = k
	}
	if keep == 0 {
		out := g.buf
		g.buf = ""
		return out
	}
	out := g.buf[:len(g.buf)-keep]
	g.buf = g.buf[len(g.buf)-keep:]
	return out
}

func (g *tagGuard) flush() string {
	out := g.buf
	g.buf = ""
	return out
}

// prefixKeep 返回 buf 尾部最多保留多少个字符才「可能是 tag 的前缀」。
func prefixKeep(buf, tag string) int {
	max := len(tag) - 1
	if max > len(buf) {
		max = len(buf)
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(tag, buf[len(buf)-n:]) {
			return n
		}
	}
	return 0
}
