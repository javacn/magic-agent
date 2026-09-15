package agent

// codebuddy.go - CodeBuddy（WorkBuddy）CLI 引擎。
//
// 后端：WorkBuddy.app 内置的 codebuddy CLI。非交互模式与 claude 同源
// （同为 CodeBuddy Code 系），但输出 envelope 实测有两种形态，解析需兼容：
//
//	1. 单对象：{"type":"result","result":"<md>",...}
//	2. 数组：[{...stream messages...}]，扫描 type=="result" 或
//	   逐条拼 assistant text content
//
// 关键 flags：
//
//	--print --output-format json     单结果 envelope
//	--model <m>                      裸模型名（hy3 / glm-5.3 / ...）
//	--tools ""                       禁用全部工具
//	--no-session-persistence         不落会话
//	--append-system-prompt <s>       注入 system prompt + noToolSuffix
//
// CLI 路径解析：MAGIC_AGENT_CODEBUDDY_BIN → WorkBuddy.app 内置路径。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CodeBuddyEngine 通过 CodeBuddy CLI 实现 Engine。
type CodeBuddyEngine struct {
	// BinPath 显式指定 CLI 路径（空 = 自动探测）。测试注入用。
	BinPath string
}

// Name 实现 Engine。
func (e *CodeBuddyEngine) Name() string { return "codebuddy" }

// DefaultCodeBuddyTimeout 单次尝试默认超时。
const DefaultCodeBuddyTimeout = 5 * time.Minute

// DefaultCodeBuddyModel codebuddy 引擎的默认模型。
// 未显式 -m 指定时使用 hy3（与 magic-video 的 DefaultCreativeModel 一致）。
const DefaultCodeBuddyModel = "hy3"

// defaultCodeBuddyBin 探测 codebuddy CLI 路径。
func (e *CodeBuddyEngine) bin() string {
	if e.BinPath != "" {
		return e.BinPath
	}
	if env := os.Getenv("MAGIC_AGENT_CODEBUDDY_BIN"); env != "" {
		return env
	}
	candidates := []string{
		"/Applications/WorkBuddy.app/Contents/Resources/app.asar.unpacked/cli/bin/codebuddy",
		"/Applications/WorkBuddy.app/Contents/Resources/app.asar.unpacked/cli/bin/cbc",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("codebuddy"); err == nil {
		return p
	}
	return ""
}

// Detect 实现 Engine。
func (e *CodeBuddyEngine) Detect() (bool, string) {
	p := e.bin()
	if p == "" {
		return false, "codebuddy CLI not found (install WorkBuddy.app or set MAGIC_AGENT_CODEBUDDY_BIN)"
	}
	return true, p + " (default model: " + DefaultCodeBuddyModel + ")"
}

// Complete 实现 Engine：单次调用 codebuddy CLI。
func (e *CodeBuddyEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return Response{}, fmt.Errorf("codebuddy CLI not found; set MAGIC_AGENT_CODEBUDDY_BIN")
	}

	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return Response{}, fmt.Errorf("codebuddy: empty prompt")
	}

	args := []string{
		"--print",
		"--output-format", "json",
		"--no-session-persistence",
		"--append-system-prompt", noToolSuffix,
	}
	// 工具模式映射（同 claude）。
	tools := toolsOrDefault(req.Tools)
	switch {
	case tools.IsOff():
		args = append(args, "--tools", "")
	case tools.IsOn():
		args = append(args, "-y") // --dangerously-skip-permissions
	default:
		args = append(args, "--tools", strings.Join(tools.Allowlist(), ","), "-y")
	}
	if m := stripModelPrefix(req.Model); m != "" {
		args = append(args, "--model", m)
	} else {
		// 默认模型 hy3；空串 = CLI 自身默认（几乎不用，保底语义）。
		if DefaultCodeBuddyModel != "" {
			args = append(args, "--model", DefaultCodeBuddyModel)
		}
	}
	args = append(args, prompt)

	stdout, stderr, err := runCLI(ctx, bin, args...)
	if err != nil {
		return Response{}, wrapCliError("codebuddy", stdout, stderr, err)
	}

	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return Response{}, fmt.Errorf("codebuddy CLI returned empty output")
	}

	text := extractCodeBuddyResult(raw)

	// 兜底：个别版本会把 user 请求回显（<user_query>…</user_query>）当正文。
	if cleaned := stripUserQueryEcho(text); cleaned != text {
		text = cleaned
	}
	if strings.TrimSpace(text) == "" {
		return Response{}, fmt.Errorf("codebuddy CLI 返回内容仅为请求回显（无模型正文）")
	}

	return Response{
		Text:    text,
		Model:   req.Model,
		Latency: time.Since(start),
	}, nil
}

// stripUserQueryEcho 剥离 <user_query>…</user_query> 请求回显。
func stripUserQueryEcho(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "<user_query>") {
		return s
	}
	end := strings.Index(t, "</user_query>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(t[end+len("</user_query>"):])
}

// extractCodeBuddyResult 解析 codebuddy 输出，兼容三种形态：
//
//  1. 单对象 envelope {"type":"result","result":"..."}
//  2. 数组：优先 type=="result"，否则拼接全部 assistant text
//  3. 解析失败原样返回
func extractCodeBuddyResult(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}

	// Shape 1: 单对象。
	var single struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if trimmed[0] == '{' {
		if err := json.Unmarshal([]byte(trimmed), &single); err == nil {
			if single.Result != "" {
				return single.Result
			}
		}
	}

	// Shape 2: stream 消息数组。
	if trimmed[0] == '[' {
		var msgs []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &msgs); err == nil {
			for _, raw := range msgs {
				var m struct {
					Type   string `json:"type"`
					Result string `json:"result"`
				}
				if json.Unmarshal(raw, &m) == nil && m.Type == "result" && m.Result != "" {
					return m.Result
				}
			}
			var sb strings.Builder
			for _, raw := range msgs {
				var probe map[string]json.RawMessage
				if json.Unmarshal(raw, &probe) != nil {
					continue
				}
				if r, ok := probe["role"]; ok && string(r) == `"assistant"` {
					if text := extractAssistantText(probe); text != "" {
						if sb.Len() > 0 {
							sb.WriteString("\n")
						}
						sb.WriteString(text)
					}
				}
				if r, ok := probe["type"]; ok && string(r) == `"assistant"` {
					if text := extractAssistantText(probe); text != "" {
						if sb.Len() > 0 {
							sb.WriteString("\n")
						}
						sb.WriteString(text)
					}
				}
			}
			if sb.Len() > 0 {
				return sb.String()
			}
		}
	}

	// Shape 3: 原样返回。
	return raw
}

// extractAssistantText 从单条 stream 消息里挖 assistant 可见文本。
func extractAssistantText(msg map[string]json.RawMessage) string {
	var sb strings.Builder
	var containers []json.RawMessage
	// "message" 可能是 {"content":[...]}（包一层），也可能直接是 [...]。
	if v, ok := msg["message"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(v, &inner); err == nil {
			if c, ok := inner["content"]; ok {
				containers = append(containers, c)
			}
		} else {
			containers = append(containers, v)
		}
	}
	if v, ok := msg["content"]; ok {
		containers = append(containers, v)
	}
	for _, c := range containers {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(c, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			t := b.Type
			if t == "" || t == "text" || t == "output_text" {
				if b.Text != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(b.Text)
				}
			}
		}
	}
	return sb.String()
}
