package agent

// codex_stream_test.go - codex 引擎的流式单测（假 CLI 回放实测 NDJSON，不碰真网络/真 CLI）。
//
// 事件样本取自 2026-09-17 真机抓包（codex 0.133，`--json --dangerously-bypass-approvals-and-sandbox`）：
// 这一版**没有逐字文本增量**，正文在 item.completed(agent_message) 里整段给出，
// 工具调用以 item.started/item.completed(command_execution) 成对出现。
// 覆盖：text/tool_use/tool_result 事件映射、会话 id、usage 计数、非 JSON 行不打断、空正文报错。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexStreamStub 造一个回放固定 NDJSON 的假 codex。
func codexStreamStub(t *testing.T, dir string, lines []string) string {
	t.Helper()
	bin := filepath.Join(dir, "codex")
	script := "#!/bin/sh\n"
	for _, ln := range lines {
		// 单引号包裹，内部单引号用 '\'' 转义
		script += "printf '%s\\n' '" + strings.ReplaceAll(ln, "'", `'\''`) + "'\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestCodexStreamMapsEvents(t *testing.T) {
	dir := t.TempDir()
	bin := codexStreamStub(t, dir, []string{
		`{"type":"thread.started","thread_id":"th-123"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"ls -a"}}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"ls -a","aggregated_output":"a.txt\nb.txt","exit_code":0}}`,
		`garbage-not-json`, // CLI 偶尔混日志：不该打断整条流
		`{"type":"item.completed","item":{"type":"agent_message","text":"目录里有两个文件"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":7,"reasoning_output_tokens":3}}`,
	})

	var got []StreamEvent
	res, err := (&CodexEngine{BinPath: bin}).Stream(context.Background(),
		Request{Tools: ToolsOn, Messages: []Message{{Role: "user", Content: "看看目录"}}},
		func(ev StreamEvent) { got = append(got, ev) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// ① 事件映射：tool_use → tool_result → text
	if len(got) != 3 {
		t.Fatalf("事件数 = %d，期望 3（tool_use/tool_result/text）：%+v", len(got), got)
	}
	if got[0].Kind != KindToolUse || got[0].ID != "item_0" || got[0].Text != "ls -a" {
		t.Errorf("tool_use 事件不对: %+v", got[0])
	}
	if got[1].Kind != KindToolResult || got[1].ID != "item_0" || !strings.Contains(got[1].Text, "a.txt") {
		t.Errorf("tool_result 事件不对: %+v", got[1])
	}
	if got[2].Kind != KindText || got[2].Text != "目录里有两个文件" {
		t.Errorf("text 事件不对: %+v", got[2])
	}

	// ② 收尾：正文 / 会话 id / token 计数（input 含 cached，output 含 reasoning）
	if res.Text != "目录里有两个文件" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.SessionID != "th-123" {
		t.Errorf("SessionID = %q，期望 thread_id", res.SessionID)
	}
	if res.InputTokens != 120 || res.OutputTokens != 10 || res.TotalTokens != 130 {
		t.Errorf("token 计数 = %d/%d/%d，期望 120/10/130", res.InputTokens, res.OutputTokens, res.TotalTokens)
	}
}

func TestCodexStreamEmptyTextFails(t *testing.T) {
	dir := t.TempDir()
	bin := codexStreamStub(t, dir, []string{
		`{"type":"thread.started","thread_id":"th-1"}`,
		`{"type":"turn.completed","usage":{"input_tokens":1}}`,
	})
	_, err := (&CodexEngine{BinPath: bin}).Stream(context.Background(),
		Request{Tools: ToolsOn, Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty result") {
		t.Errorf("没有正文时应报错，得 %v", err)
	}
}

// Streamer 能力表：codex 现在实现了 Stream（其余不支持的引擎由 --engines 的 streaming 字段暴露）。
func TestCodexImplementsStreamer(t *testing.T) {
	if AsStreamer(&CodexEngine{}) == nil {
		t.Error("CodexEngine 应实现 Streamer（2026-09-17 补齐）")
	}
}
