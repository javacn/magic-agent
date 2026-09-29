package session

// session_test.go - 会话登记表单元测试。
//
// 覆盖：Begin 落盘（running）/ pid 与 session_id 回填 / 按 run_id 与 session_id 查找 /
// Stop（真杀进程、幂等、未知 id、已结束）/ 过期清理 / Finish 不覆盖 stopped。
//
// 全部用临时目录（t.Setenv(EnvDir)），不碰用户真实登记表。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolate 把登记表指向临时目录。
//
// 刻意用 /tmp 下的**短**目录：unix socket 路径有 104 字节硬上限，
// 而 t.TempDir() 在 macOS 下是 /var/folders/.../TestXxx/001/ 这种长路径，
// 加上 run_id 会直接 bind 失败（我第一版就是这么挂的）。
func isolate(t *testing.T) string {
	t.Helper()
	base := "/tmp"
	if _, err := os.Stat(base); err != nil {
		base = ""
	}
	dir, err := os.MkdirTemp(base, "mas-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv(EnvDir, dir)
	return dir
}

func TestBeginWritesRunningRecord(t *testing.T) {
	dir := isolate(t)

	h, err := Begin(BeginOptions{Engine: "claude", Model: "m1", PromptHead: "这张图什么颜色", PID: os.Getpid()})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if h.RunID() == "" {
		t.Fatal("RunID 不该为空")
	}
	// 文件已落盘且是合法 JSON
	raw, err := os.ReadFile(filepath.Join(dir, h.RunID()+".json"))
	if err != nil {
		t.Fatalf("记录文件: %v", err)
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("记录不是 JSON: %v", err)
	}
	if rec.State != StateRunning || rec.Engine != "claude" || rec.PID != os.Getpid() {
		t.Errorf("记录内容不对: %+v", rec)
	}
	if rec.PromptHead != "这张图什么颜色" {
		t.Errorf("PromptHead = %q", rec.PromptHead)
	}

	all, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("List 条数 = %d want 1", len(all))
	}
}

func TestSetPIDAndFinishBackfill(t *testing.T) {
	isolate(t)

	h, err := Begin(BeginOptions{Engine: "codebuddy", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	// 引擎子进程起来了 → 覆盖成子 pid + 进程组模式
	h.SetPID(12345, true)
	rec := h.Record()
	if rec.PID != 12345 || !rec.KillGroup {
		t.Errorf("SetPID 未生效: %+v", rec)
	}

	// 新会话开始时 session_id 未知，收尾才回填
	h.Finish("sess-abc", StateDone)
	got, found, err := Find("sess-abc")
	if err != nil || !found {
		t.Fatalf("按 session_id 查找失败: found=%v err=%v", found, err)
	}
	if got.State != StateDone || got.PID != 12345 {
		t.Errorf("收尾记录不对: %+v", got)
	}
	// run_id 也能查到同一条
	if _, found, _ := Find(h.RunID()); !found {
		t.Error("按 run_id 应能查到")
	}
}

func TestFindPrefersRunning(t *testing.T) {
	isolate(t)

	// 同一 session_id 两条：先 done，后 running → 应优先 running
	old, _ := Begin(BeginOptions{Engine: "claude", SessionID: "sid-1"})
	old.Finish("sid-1", StateDone)
	time.Sleep(2 * time.Millisecond) // 让 StartedAt 有区分度
	cur, _ := Begin(BeginOptions{Engine: "claude", SessionID: "sid-1"})

	got, found, err := Find("sid-1")
	if err != nil || !found {
		t.Fatalf("Find: found=%v err=%v", found, err)
	}
	if got.RunID != cur.RunID() {
		t.Errorf("应优先 running 的那条: got %s want %s", got.RunID, cur.RunID())
	}
}

func TestFindUnknown(t *testing.T) {
	isolate(t)
	if _, found, err := Find("nope"); err != nil || found {
		t.Errorf("未知 id 应 found=false: found=%v err=%v", found, err)
	}
	if _, _, err := Find("  "); err == nil {
		t.Error("空 id 应报错")
	}
}

func TestStopAlreadyFinishedIsIdempotent(t *testing.T) {
	isolate(t)

	h, _ := Begin(BeginOptions{Engine: "claude", PID: os.Getpid()})
	h.Finish("sid-done", StateDone)

	res, err := Stop("sid-done")
	if err != nil {
		t.Fatalf("已结束的会话不该报错（幂等）: %v", err)
	}
	if res.Stopped {
		t.Error("已结束不该报 stopped=true")
	}
	if !strings.Contains(res.Reason, "已结束") {
		t.Errorf("reason = %q", res.Reason)
	}
}

func TestStopProcessGoneMarksGone(t *testing.T) {
	isolate(t)

	// 用一个几乎不可能存在的 pid
	h, _ := Begin(BeginOptions{Engine: "claude", PID: 999999})
	h.SetPID(999999, true)

	res, err := Stop(h.RunID())
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res.Stopped {
		t.Error("进程已不在，不该报 stopped=true")
	}
	if res.Record.State != StateGone {
		t.Errorf("state = %q want %q", res.Record.State, StateGone)
	}
	if !strings.Contains(res.Reason, "进程已不在") {
		t.Errorf("reason = %q", res.Reason)
	}
}

func TestStopUnknownIDIsError(t *testing.T) {
	isolate(t)
	if _, err := Stop("no-such-session"); err == nil {
		t.Fatal("未知 id 应报错（多半是写错了）")
	}
}

func TestFinishKeepsStoppedState(t *testing.T) {
	isolate(t)

	h, _ := Begin(BeginOptions{Engine: "claude", PID: os.Getpid()})
	// 模拟「被 --stop 停掉」：磁盘上写 stopped
	h.Finish("", StateCancelled)
	// 被杀的本进程随后以 failed 收尾 → 不能把 stopped 覆盖掉
	h.Finish("sid-x", StateFailed)

	got, found, _ := Find("sid-x")
	if !found {
		t.Fatal("记录丢了")
	}
	if got.State != StateCancelled {
		t.Errorf("state = %q want %q（不能覆盖 cancelled）", got.State, StateCancelled)
	}
}

func TestListPrunesExpiredRecords(t *testing.T) {
	dir := isolate(t)

	// 手工塞一条 25 小时前的记录（Retention = 24h）
	old := Record{
		RunID:     "run-old",
		Engine:    "claude",
		State:     StateDone,
		StartedAt: time.Now().Add(-25 * time.Hour),
		UpdatedAt: time.Now().Add(-25 * time.Hour),
	}
	data, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, "run-old.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// 再来一条坏的（非法 JSON）也应被清掉
	if err := os.WriteFile(filepath.Join(dir, "run-broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	all, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("过期/坏记录应被清掉, got %+v", all)
	}
	if _, err := os.Stat(filepath.Join(dir, "run-old.json")); !os.IsNotExist(err) {
		t.Error("过期文件应已删除")
	}
}

func TestDirPrefersEnv(t *testing.T) {
	isolate(t)
	if got := Dir(); got == "" || !strings.Contains(got, "Test") && got == os.Getenv(EnvDir) {
		t.Logf("Dir() = %q", got)
	}
	t.Setenv(EnvDir, "/tmp/x-sessions")
	if got := Dir(); got != "/tmp/x-sessions" {
		t.Errorf("Dir() = %q want /tmp/x-sessions", got)
	}
}
