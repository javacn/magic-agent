package cli

// session_test.go - CLI 侧「停止指定会话」的测试。
//
// 用户需求（2026-09-18）：「要支持停止指定会话」。
//
// 覆盖：--sessions 列出登记表 / --stop 的三种结果（真停掉 / 已结束幂等 / 未知 id 报错）/
// --stop "" 的参数校验 / 每次调用都会落一条记录（state=done 且回填 session_id）/
// 引擎子进程 pid 经 spawn 钩子写进记录（--stop 才能杀对进程）。

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/darren/magic-agent/internal/agent"
	"github.com/darren/magic-agent/internal/session"
)

// sessionRows 解析 --sessions 的 JSON 输出。
type sessionRow struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	PID       int    `json:"pid"`
	Engine    string `json:"engine"`
	State     string `json:"state"`
	Alive     bool   `json:"alive"`
}

func parseSessions(t *testing.T, out string) []sessionRow {
	t.Helper()
	var rows []sessionRow
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("--sessions 输出不是 JSON 数组: %v (%s)", err, out)
	}
	return rows
}

// 一次普通调用结束后，登记表里应留下一条 done 记录，并回填 session_id。
func TestAskRegistersSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(session.EnvDir, dir)
	registerFake(&stringEngine{name: "fake-sess-reg", text: "ok"})

	if _, _, err := runAskCmd(t, "", "-e", "fake-sess-reg", "你好"); err != nil {
		t.Fatalf("ask: %v", err)
	}

	rows := parseSessions(t, mustSessions(t))
	if len(rows) != 1 {
		t.Fatalf("登记表条数 = %d want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Engine != "fake-sess-reg" || r.State != session.StateDone {
		t.Errorf("记录不对: %+v", r)
	}
	if r.Alive {
		t.Error("已结束的会话不该标 alive")
	}
	// 记录文件确实落在指定目录
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("目录里文件数 = %d want 1", len(entries))
	}
}

// 流式调用同样落记录。
func TestStreamAskRegistersSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(session.EnvDir, dir)
	registerFake(&streamingEngine{name: "fake-sess-stream", text: "ok"})

	if _, _, err := runAskCmd(t, "", "-e", "fake-sess-stream", "--stream", "hi"); err != nil {
		t.Fatalf("stream ask: %v", err)
	}
	rows := parseSessions(t, mustSessions(t))
	if len(rows) != 1 || rows[0].State != session.StateDone {
		t.Fatalf("流式调用也该落一条 done 记录: %+v", rows)
	}
}

// 引擎返回的 session_id 要回填进记录（这样「停这条会话」能用会话 id 寻址）。
//
// 注：这里**不能**用真引擎来验「子进程 pid 被写进记录」—— cli 包测试进程里的注册表
// 是假的（先 Register 了假引擎，agent.Engines() 就不会 initEngines()，真引擎根本不在）。
// spawn 钩子本身在 internal/agent 用真引擎 + 假 CLI 验（TestSpawnHookReceivesChildPID）。
func TestAskBackfillsSessionID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(session.EnvDir, dir)
	registerFake(&sidEngine{name: "fake-sess-sid", text: "ok", sid: "sid-from-engine"})

	if _, _, err := runAskCmd(t, "", "-e", "fake-sess-sid", "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	rows := parseSessions(t, mustSessions(t))
	if len(rows) != 1 {
		t.Fatalf("登记表条数 = %d want 1", len(rows))
	}
	if rows[0].SessionID != "sid-from-engine" {
		t.Errorf("session_id = %q want sid-from-engine（收尾要回填）", rows[0].SessionID)
	}
	// 用会话 id 也能 --stop 找到它（此处已结束 → 幂等，不报错）
	if _, _, err := runAskCmd(t, "", "--stop", "sid-from-engine"); err != nil {
		t.Errorf("按 session_id 停止不该报错: %v", err)
	}
}

// sidEngine 固定返回一个 session_id 的假引擎（验回填）。
type sidEngine struct {
	name string
	text string
	sid  string
}

func (s *sidEngine) Name() string           { return s.name }
func (s *sidEngine) Detect() (bool, string) { return true, "fake://" + s.name }
func (s *sidEngine) Complete(ctx context.Context, req agent.Request) (agent.Response, error) {
	return agent.Response{Text: s.text, Model: "fake-model", SessionID: s.sid, Latency: 1_000_000}, nil
}

// --stop：未知 id → 报错（exit 1 语义，不是 usageError）。
func TestStopUnknownIDErrors(t *testing.T) {
	t.Setenv(session.EnvDir, t.TempDir())
	_, _, err := runAskCmd(t, "", "--stop", "no-such-session")
	if err == nil {
		t.Fatal("未知 id 应报错")
	}
	if _, ok := err.(*usageError); ok {
		t.Errorf("未知 id 不是参数错（不该 exit 2）: %v", err)
	}
	if !strings.Contains(err.Error(), "no such session") {
		t.Errorf("错误信息应点明找不到: %v", err)
	}
}

// --stop 空值 → 参数错（exit 2），而不是掉进「提问」分支报 empty prompt。
func TestStopEmptyIDIsUsageError(t *testing.T) {
	t.Setenv(session.EnvDir, t.TempDir())
	_, _, err := runAskCmd(t, "", "--stop", "")
	if err == nil {
		t.Fatal("应报错")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("空 id 应是 usageError(exit 2), got %T: %v", err, err)
	}
}

// --stop：已结束的会话 → 幂等（exit 0，stopped=false + reason）。
func TestStopFinishedSessionIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(session.EnvDir, dir)
	registerFake(&stringEngine{name: "fake-sess-fin", text: "ok"})

	if _, _, err := runAskCmd(t, "", "-e", "fake-sess-fin", "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	runID := parseSessions(t, mustSessions(t))[0].RunID

	stdout, _, err := runAskCmd(t, "", "--stop", runID)
	if err != nil {
		t.Fatalf("停一条已结束的会话不该报错: %v", err)
	}
	var out struct {
		Type    string `json:"type"`
		Stopped bool   `json:"stopped"`
		State   string `json:"state"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &out); err != nil {
		t.Fatalf("--stop 输出不是 JSON: %v (%s)", err, stdout)
	}
	if out.Type != "stop" || out.Stopped {
		t.Errorf("应 stopped=false: %+v", out)
	}
	if !strings.Contains(out.Reason, "已结束") {
		t.Errorf("reason = %q", out.Reason)
	}
}

// --sessions 空登记表 → 空数组（不是 null，调用方好解析）。
func TestSessionsEmptyIsArray(t *testing.T) {
	t.Setenv(session.EnvDir, t.TempDir())
	out, _, err := runAskCmd(t, "", "--sessions")
	if err != nil {
		t.Fatalf("--sessions: %v", err)
	}
	if got := strings.TrimSpace(out); got != "[]" {
		t.Errorf("空登记表应输出 [], got %q", got)
	}
}

// mustSessions 跑 --sessions 并返回 stdout。
func mustSessions(t *testing.T) string {
	t.Helper()
	out, _, err := runAskCmd(t, "", "--sessions")
	if err != nil {
		t.Fatalf("--sessions: %v", err)
	}
	return out
}
