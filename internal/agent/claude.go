package agent

// claude.go - Claude Code CLI 引擎。
//
// 后端：claude（Claude Code，本机 /opt/homebrew/bin/claude）。
// 非交互模式：-p（--print）+ --output-format json，拿到稳定 envelope：
//
//	{"type":"result","subtype":"success","result":"<正文>",
//	 "session_id":"...","is_error":false, "total_cost_usd":...}
//
// 关键 flags：
//
//	--model <m>                    传裸模型名（sonnet / opus / claude-sonnet-4-6...）
//	--tools ""                     禁用全部内置工具，纯 chat 一次成型
//	--no-session-persistence       不落会话（--print 模式）
//	--append-system-prompt <s>     注入 system prompt
//
// CLI 路径解析顺序：MAGIC_AGENT_CLAUDE_BIN → PATH(claude) → 常见安装位。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ClaudeEngine 通过 Claude Code CLI 实现 Engine。
type ClaudeEngine struct {
	// BinPath 显式指定 CLI 路径（空 = 自动探测）。测试注入用。
	BinPath string
}

// Name 实现 Engine。
func (e *ClaudeEngine) Name() string { return "claude" }

// DefaultClaudeTimeout 单次尝试默认超时。
const DefaultClaudeTimeout = 5 * time.Minute

// defaultClaudeBin 探测 claude CLI 路径。
func (e *ClaudeEngine) bin() string {
	if e.BinPath != "" {
		return e.BinPath
	}
	if env := os.Getenv("MAGIC_AGENT_CLAUDE_BIN"); env != "" {
		return env
	}
	candidates := []string{
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return ""
}

// Detect 实现 Engine。
func (e *ClaudeEngine) Detect() (bool, string) {
	p := e.bin()
	if p == "" {
		return false, "claude CLI not found (install Claude Code or set MAGIC_AGENT_CLAUDE_BIN)"
	}
	return true, p
}

// claudeResult 对应 --output-format json 的 envelope。
type claudeResult struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	SessionID    string  `json:"session_id"`
	Model        string  `json:"model"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// Complete 实现 Engine：单次调用 claude CLI。
func (e *ClaudeEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return Response{}, fmt.Errorf("claude CLI not found; set MAGIC_AGENT_CLAUDE_BIN")
	}

	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return Response{}, fmt.Errorf("claude: empty prompt")
	}

	args := []string{
		"-p",
		"--output-format", "json",
		"--tools", "",
		"--no-session-persistence",
	}
	if m := stripModelPrefix(req.Model); m != "" {
		args = append(args, "--model", m)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	args = append(args, prompt)

	stdout, stderr, err := runCLI(ctx, bin, args...)
	if err != nil {
		errMsg := truncateStr(strings.TrimSpace(stderr), 500)
		if errMsg == "" {
			errMsg = err.Error()
		}
		return Response{}, fmt.Errorf("claude CLI: %w (stderr: %s)", err, errMsg)
	}

	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return Response{}, fmt.Errorf("claude CLI returned empty output")
	}

	var env claudeResult
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		// 个别版本对 --print + json 仍吐纯文本：原样返回。
		return Response{
			Text:    raw,
			Model:   req.Model,
			Latency: time.Since(start),
		}, nil
	}
	if env.IsError {
		return Response{}, fmt.Errorf("claude CLI error (subtype=%s): %s", env.Subtype, truncateStr(env.Result, 500))
	}
	if env.Result == "" {
		return Response{}, fmt.Errorf("claude CLI returned empty result")
	}
	return Response{
		Text:      env.Result,
		Model:     env.Model,
		SessionID: env.SessionID,
		Latency:   time.Since(start),
	}, nil
}

// truncateStr 截断到最多 n 字节。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
