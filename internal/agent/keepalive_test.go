package agent

// keepalive_test.go - 常驻会话（Request.Append）在 claude/codebuddy 上的落地。
//
// 用户需求（2026-09-18）：「按路线 2 实现，先支持 claude 和 codebuddy 的追加功能」。
//
// 做法：假 CLI 用 `while read line` 模拟「stdin 每来一行就当作一轮」，
// 把收到的每行长度回显成 result —— 于是可以断言：
//   - 追加的消息**真的进了子进程的 stdin**（行数 = 1 + 追加数）；
//   - 每轮都发 KindTurnEnd（CLI 的空闲收工挂在它上面）；
//   - 关闭追加通道 → 子进程 EOF 收尾 → Stream 正常返回（不需要 --stop）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// multiTurnCLI 假 CLI：stdin 每来一行就回一个 result，并把该行追加写进 seenFile。
func multiTurnCLI(t *testing.T, seenFile string) string {
	t.Helper()
	return writeFakeCLI(t, "claude", `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' "$line" >> `+seenFile+`
  echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s-ka","model":"m"}'
done
`)
}

// readLines 读 seenFile 的行数（文件可能还没建）。
func readLines(t *testing.T, p string) []string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestKeepAliveAppendsReachChildStdin(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen.txt")
	cli := multiTurnCLI(t, seen)
	e := &ClaudeEngine{BinPath: cli}

	appendCh := make(chan string, 4)
	events, onEvent := collectEvents()

	type res struct {
		out StreamResult
		err error
	}
	done := make(chan res, 1)
	go func() {
		out, err := e.Stream(context.Background(), Request{
			Messages: []Message{{Role: "user", Content: "首轮任务"}},
			Append:   appendCh,
		}, onEvent)
		done <- res{out, err}
	}()

	// 等首轮进了子进程
	waitFor(t, func() bool { return len(readLines(t, seen)) >= 1 }, "首轮没进 stdin")
	appendCh <- "追加需求 A"
	waitFor(t, func() bool { return len(readLines(t, seen)) >= 2 }, "追加 A 没进 stdin")
	appendCh <- "追加需求 B"
	waitFor(t, func() bool { return len(readLines(t, seen)) >= 3 }, "追加 B 没进 stdin")

	// 关闭通道 → 子进程 EOF → 优雅收尾
	close(appendCh)

	var got res
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("关闭追加通道后 Stream 没返回（常驻会话没能优雅收尾）")
	}
	if got.err != nil {
		t.Fatalf("Stream: %v", got.err)
	}
	if got.out.Text != "ok" {
		t.Errorf("Text = %q", got.out.Text)
	}

	lines := readLines(t, seen)
	if len(lines) != 3 {
		t.Fatalf("子进程收到 %d 行 want 3（首轮 + 两条追加）", len(lines))
	}
	if !strings.Contains(lines[0], "首轮任务") {
		t.Errorf("第 1 行应是首轮提示词: %q", lines[0])
	}
	if !strings.Contains(lines[1], "追加需求 A") || !strings.Contains(lines[2], "追加需求 B") {
		t.Errorf("追加内容没按顺序进 stdin: %v", lines)
	}
	// 追加是**流式输入**：每行都应是 user 消息 JSON
	for i, l := range lines {
		if !strings.Contains(l, `"type":"user"`) || !strings.Contains(l, `"role":"user"`) {
			t.Errorf("第 %d 行不是 user 消息 JSON: %q", i+1, l)
		}
	}

	// 每轮一个 KindTurnEnd（CLI 的空闲收工 / 应用侧「本轮完成」都靠它），且必须带会话 id
	turnEnds := 0
	for _, ev := range *events {
		if ev.Kind == KindTurnEnd {
			turnEnds++
			if ev.SessionID != "s-ka" {
				t.Errorf("turn_end 应带会话 id（应用侧靠它绑锚点），got %q", ev.SessionID)
			}
			if ev.Text == "" {
				t.Error("turn_end 应带该轮正文")
			}
		}
	}
	if turnEnds != 3 {
		t.Errorf("KindTurnEnd 事件数 = %d want 3（每轮一个）", turnEnds)
	}
}

// codebuddy 同族，走同一条 stream-json 输入通道。
func TestKeepAliveWorksForCodeBuddy(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen.txt")
	cli := writeFakeCLI(t, "codebuddy", `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' "$line" >> `+seen+`
  echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s-ka","model":"m"}'
done
`)
	e := &CodeBuddyEngine{BinPath: cli}
	appendCh := make(chan string, 2)

	done := make(chan error, 1)
	go func() {
		_, err := e.Stream(context.Background(), Request{
			Messages: []Message{{Role: "user", Content: "首轮"}},
			Append:   appendCh,
		}, nil)
		done <- err
	}()
	waitFor(t, func() bool { return len(readLines(t, seen)) >= 1 }, "首轮没进 stdin")
	appendCh <- "追加到 codebuddy"
	waitFor(t, func() bool { return len(readLines(t, seen)) >= 2 }, "追加没进 stdin")
	close(appendCh)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("codebuddy 常驻会话没优雅收尾")
	}
	if lines := readLines(t, seen); !strings.Contains(strings.Join(lines, "\n"), "追加到 codebuddy") {
		t.Errorf("追加内容没进 codebuddy 的 stdin: %v", lines)
	}
}

func TestAppendSupportOf(t *testing.T) {
	for engine, want := range map[string]bool{
		"claude":    true,
		"codebuddy": true,
		"trae":      false,
		"codex":     false,
		"llm":       false,
		"openclaw":  false,
		"arkclaw":   false,
		"unknown":   false,
	} {
		if got := AppendSupportOf(engine); got != want {
			t.Errorf("AppendSupportOf(%q) = %v want %v", engine, got, want)
		}
	}
}

// waitFor 轮询等待条件成立（最多 10s）。
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("超时：%s", msg)
}
