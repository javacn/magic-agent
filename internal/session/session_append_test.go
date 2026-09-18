package session

// session_append_test.go - 常驻会话追加入口（unix socket）的单元测试。
//
// 覆盖：ListenAppend 把 socket 路径写进记录（权限 0600）/ 往返投递 / 空内容与未知 id 报错 /
// 没有追加入口时的可操作提示 / socket 已死时把记录标 gone / Close 删 socket 文件。

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// startAppender 起一条带追加入口的会话，返回句柄与消息通道。
func startAppender(t *testing.T) (*Handle, *Appender, chan string) {
	t.Helper()
	isolate(t)
	h, err := Begin(BeginOptions{Engine: "claude", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	ap, err := ListenAppend(h)
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows 无 unix socket（预期报错）: %v", err)
		}
		t.Fatalf("ListenAppend: %v", err)
	}
	t.Cleanup(ap.Close)
	ch := make(chan string, 4)
	go ap.Serve(ch)
	return h, ap, ch
}

func TestListenAppendRegistersSocket(t *testing.T) {
	h, ap, _ := startAppender(t)

	if ap.Path() == "" {
		t.Fatal("socket 路径为空")
	}
	if got := h.Record().Append; got != ap.Path() {
		t.Errorf("记录里的 append = %q want %q", got, ap.Path())
	}
	st, err := os.Stat(ap.Path())
	if err != nil {
		t.Fatalf("socket 文件不存在: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket 权限 = %o want 600（只有同用户能追加）", perm)
	}
	// 记录落盘后也能读到（--append 就是从磁盘读的）
	rec, found, err := Find(h.RunID())
	if err != nil || !found {
		t.Fatalf("Find: found=%v err=%v", found, err)
	}
	if rec.Append != ap.Path() {
		t.Errorf("磁盘记录的 append = %q", rec.Append)
	}
}

func TestAppendMessageDelivers(t *testing.T) {
	h, _, ch := startAppender(t)

	res, err := AppendMessage(h.RunID(), "追加：顺便把日志也整理了")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if !res.Queued {
		t.Errorf("应 queued=true: %+v", res)
	}
	select {
	case got := <-ch:
		if got != "追加：顺便把日志也整理了" {
			t.Errorf("收到的消息 = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("消息没投递到常驻进程")
	}
}

// 用会话 id 追加（观物台手里通常只有 session_id）。
func TestAppendMessageBySessionID(t *testing.T) {
	isolate(t)
	h, err := Begin(BeginOptions{Engine: "claude", SessionID: "sid-append", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	ap, err := ListenAppend(h)
	if err != nil {
		t.Skipf("Windows 无 unix socket: %v", err)
	}
	t.Cleanup(ap.Close)
	ch := make(chan string, 2)
	go ap.Serve(ch)

	if _, err := AppendMessage("sid-append", "按会话 id 追加"); err != nil {
		t.Fatalf("AppendMessage(session_id): %v", err)
	}
	select {
	case got := <-ch:
		if got != "按会话 id 追加" {
			t.Errorf("收到的消息 = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没投递")
	}
}

func TestAppendMessageErrors(t *testing.T) {
	isolate(t)

	if _, err := AppendMessage("whatever", "   "); err == nil {
		t.Error("空内容应报错")
	}
	if _, err := AppendMessage("no-such-session", "hi"); err == nil {
		t.Error("未知 id 应报错")
	}

	// 会话存在但没有追加入口（没用 --keep-alive 起）→ 提示要 --keep-alive
	h, _ := Begin(BeginOptions{Engine: "claude", PID: os.Getpid()})
	_, err := AppendMessage(h.RunID(), "hi")
	if err == nil {
		t.Fatal("没有追加入口应报错")
	}
	if !strings.Contains(err.Error(), "--keep-alive") {
		t.Errorf("错误信息应点明要用 --keep-alive 启动: %v", err)
	}
	if !strings.Contains(err.Error(), "--session") {
		t.Errorf("错误信息应给出「已结束用 --session 续接」的出路: %v", err)
	}
}

// 记录说 running、socket 却连不上（常驻进程崩了）→ 报错并把记录标 gone。
func TestAppendMessageDeadSocketMarksGone(t *testing.T) {
	h, ap, _ := startAppender(t)
	ap.Close() // 常驻进程「没了」

	res, err := AppendMessage(h.RunID(), "hi")
	if err == nil {
		t.Fatal("连不上应报错")
	}
	if res.Record.State != StateGone {
		t.Errorf("记录状态 = %q want %q", res.Record.State, StateGone)
	}
	got, _, _ := Find(h.RunID())
	if got.State != StateGone {
		t.Errorf("磁盘记录状态 = %q want %q", got.State, StateGone)
	}
}

func TestAppenderCloseRemovesSocket(t *testing.T) {
	_, ap, _ := startAppender(t)
	path := ap.Path()
	ap.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Close 应删掉 socket 文件: %v", err)
	}
	ap.Close() // 幂等
}
