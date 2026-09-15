package agent

// openai_client.go - OpenAI 兼容 /chat/completions HTTP 客户端。
//
// 移植自 magic-video 的 base/llm/openai.go（已被 minimax 生产验证），
// 适配 magic-agent 的 Engine 语义。关键保留点：
//
//   - extraBody 原样并入请求体：厂商私有开关透传。典型用途是推理模型
//     关思维链 —— MiniMax-M3 的 {"thinking":{"type":"disabled"}}，不加
//     开关时长文本生成会被思维链吃光 token 预算、正文直接为空。
//   - reasoning_split=true：MiniMax 把 thinking 拆到 reasoning_content，
//     content 只含实际回答。
//   - content 里残留的思维链标签做兜底剥离。
//
// 单次调用；超时与重试由 Runner 在外层编排（与 CLI 引擎一致）。

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

// httpClientFor 按 provider 超时构造 HTTP 客户端。
func httpClientFor(p Provider) *http.Client {
	timeout := p.HTTPTimeout()
	if timeout == 0 {
		timeout = 300 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// chatComplete 向 provider 发一次 chat completion，返回正文。
//
// system 非空时作为首条 system 消息；messages 依序跟随。
func chatComplete(ctx context.Context, hc *http.Client, p Provider, model, system string, messages []Message) (string, error) {
	payload := buildChatPayload(p, model, system, messages)

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	// extraBody 透传：merge 到顶层，覆盖同名键。
	if len(p.ExtraBody) > 0 {
		var merged map[string]any
		if err := json.Unmarshal(body, &merged); err != nil {
			return "", fmt.Errorf("merge extraBody (unmarshal): %w", err)
		}
		for k, v := range p.ExtraBody {
			merged[k] = v
		}
		if body, err = json.Marshal(merged); err != nil {
			return "", fmt.Errorf("merge extraBody (marshal): %w", err)
		}
	}

	url := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 带上状态码与响应片段，方便上层分类（429/5xx 可重试）。
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(strings.TrimSpace(string(respBody)), 500))
	}

	text, err := parseChatResponse(respBody)
	if err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	return text, nil
}

// chatRequest 是 /chat/completions 的请求体（HTTP 线格式）。
type chatRequest struct {
	Model          string    `json:"model"`
	Messages       []chatMsg `json:"messages"`
	MaxTokens      int       `json:"max_tokens,omitempty"`
	Temperature    *float64  `json:"temperature,omitempty"`
	ReasoningSplit bool      `json:"reasoning_split,omitempty"` // MiniMax: thinking 拆到 reasoning_content
	Stream         bool      `json:"stream"`
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// buildChatPayload 组装请求体（system 在前，随后按输入顺序）。
func buildChatPayload(p Provider, model, system string, messages []Message) chatRequest {
	msgs := make([]chatMsg, 0, len(messages)+1)
	if s := strings.TrimSpace(system); s != "" {
		msgs = append(msgs, chatMsg{Role: "system", Content: s})
	}
	for _, m := range messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		msgs = append(msgs, chatMsg{Role: role, Content: m.Content})
	}
	return chatRequest{
		Model:          model,
		Messages:       msgs,
		ReasoningSplit: true,
		// 显式 false：magic-agent 只要一次性结果，不要 SSE 流。
		Stream: false,
	}
}

// chatStream 发起 SSE 流式 chat completion，增量实时回调。
//
// 事件分流（OpenAI 兼容 + MiniMax reasoning_split 扩展）：
//
//	delta.content            → KindText
//	delta.reasoning_content  → KindThinking（MiniMax 思维链通道）
//
// 返回收尾汇总（全文 = 正文增量拼接，兜底 stripThinkingTags）。
func chatStream(ctx context.Context, hc *http.Client, p Provider, model, system string, messages []Message, onEvent func(StreamEvent)) (StreamResult, error) {
	payload := buildChatPayload(p, model, system, messages)
	payload.Stream = true // 覆盖非流式默认

	body, err := json.Marshal(payload)
	if err != nil {
		return StreamResult{}, fmt.Errorf("marshal payload: %w", err)
	}
	if len(p.ExtraBody) > 0 {
		var merged map[string]any
		if err := json.Unmarshal(body, &merged); err != nil {
			return StreamResult{}, fmt.Errorf("merge extraBody (unmarshal): %w", err)
		}
		for k, v := range p.ExtraBody {
			merged[k] = v
		}
		if body, err = json.Marshal(merged); err != nil {
			return StreamResult{}, fmt.Errorf("merge extraBody (marshal): %w", err)
		}
	}

	url := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return StreamResult{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return StreamResult{}, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return StreamResult{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(strings.TrimSpace(string(respBody)), 500))
	}

	var textBuf, thinkBuf strings.Builder
	emit := func(kind StreamEventKind, s string) {
		switch kind {
		case KindThinking:
			thinkBuf.WriteString(s)
		case KindText:
			textBuf.WriteString(s)
		}
		if onEvent != nil {
			onEvent(StreamEvent{Kind: kind, Text: s})
		}
	}

	// SSE 解析：data: {...} 行；data: [DONE] 收尾。
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawDone := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue // 心跳注释行
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // 非 JSON data 行忽略
		}
		if ev.Error != nil && ev.Error.Message != "" {
			return StreamResult{}, fmt.Errorf("provider error (%s): %s", ev.Error.Type, ev.Error.Message)
		}
		if len(ev.Choices) == 0 {
			continue
		}
		d := ev.Choices[0].Delta
		if d.ReasoningContent != "" {
			emit(KindThinking, d.ReasoningContent)
		}
		if d.Content != "" {
			emit(KindText, d.Content)
		}
	}
	if err := sc.Err(); err != nil {
		return StreamResult{}, fmt.Errorf("read sse: %w", err)
	}
	_ = sawDone // 部分厂商不发 [DONE] 直接断流，不作为硬性要求

	text := strings.TrimSpace(textBuf.String())
	if text == "" {
		// 正文增量缺失时兜底：也许全部混在 thinking 通道（reasoning_split 未生效）。
		text = stripThinkingTags(thinkBuf.String())
		if text == "" {
			return StreamResult{}, fmt.Errorf("stream produced no content")
		}
	}
	return StreamResult{
		Response: Response{Text: stripThinkingTags(text)},
		Thinking: thinkBuf.String(),
	}, nil
}

// parseChatResponse 从响应体里取第一条 choice 的正文。
func parseChatResponse(body []byte) (string, error) {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("unmarshal: %w", err)
	}
	// 部分厂商用 200 + error 体报错。
	if r.Error != nil && r.Error.Message != "" {
		return "", fmt.Errorf("provider error (%s): %s", r.Error.Type, r.Error.Message)
	}
	if len(r.Choices) == 0 {
		return "", fmt.Errorf("response has no choices")
	}
	return stripThinkingTags(r.Choices[0].Message.Content), nil
}

// 思维链标签的构造片段。
//
// ⚠️ 必须分段拼接，不能写成连续字面量 —— 该标签序列会被某些分词器当作
// 特殊 token 并在写盘时改写（实测被替换成全角竖线形式，长度从 8 变 19），
// 导致源码里的标签与实际字符串不符、Index 偏移全部错位。
// 分段拼接后运行时值不变，但源码中不出现该 token 序列。
const (
	tagLT    = "<"
	tagGT    = ">"
	tagSlash = "/"
	tagWord  = "think"
)

// thinkOpenTag / thinkCloseTag 运行时值为思维链的开始/结束标签。
var (
	thinkOpenTag  = tagLT + tagWord + tagGT
	thinkCloseTag = tagLT + tagSlash + tagWord + tagGT
)

// stripThinkingTags 兜底剥离 content 中残留的思维链标签块。
// reasoning_split=true 时上游已拆分，这里是兼容个别不遵守的厂商。
func stripThinkingTags(s string) string {
	for {
		i := strings.Index(s, thinkOpenTag)
		if i < 0 {
			break
		}
		rest := s[i+len(thinkOpenTag):]
		j := strings.Index(rest, thinkCloseTag)
		if j < 0 {
			// 有开无闭：整段视为未完成的思考，丢弃。
			s = s[:i]
			break
		}
		// 相对 rest 切片，避免绝对/相对偏移混算。
		s = s[:i] + rest[j+len(thinkCloseTag):]
	}
	return strings.TrimSpace(s)
}
