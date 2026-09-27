package cli

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lineWriter 收集每次写入的行，供异步断言使用（控制通道是后台 goroutine 在写）。
type lineWriter struct {
	mu    sync.Mutex
	lines []string
	ch    chan string
}

func newLineWriter() *lineWriter {
	return &lineWriter{ch: make(chan string, 64)}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	for _, ln := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if ln == "" {
			continue
		}
		w.mu.Lock()
		w.lines = append(w.lines, ln)
		w.mu.Unlock()
		w.ch <- ln
	}
	return len(p), nil
}

// wait 等够 n 行（超时即失败），返回收到的行。
func (w *lineWriter) wait(t *testing.T, n int) []string {
	t.Helper()
	got := make([]string, 0, n)
	deadline := time.After(3 * time.Second)
	for len(got) < n {
		select {
		case ln := <-w.ch:
			got = append(got, ln)
		case <-deadline:
			t.Fatalf("等待第 %d 行超时；已收到 %v", n, got)
		}
	}
	return got
}

// TestControlPingAndUnknownOp 未知 op 只回一条 control_error，**不中断读取** ——
// 一个拼错的 op 不该让整条控制通道失效。
func TestControlPingAndUnknownOp(t *testing.T) {
	w := newLineWriter()
	ctl := startControlReader(
		strings.NewReader(`{"op":"ping"}`+"\n"+`{"op":"nope"}`+"\n"+`{"op":"ping"}`+"\n"),
		newEventSink(w, "claude", true), controlHooks{})
	defer ctl.Close()

	lines := w.wait(t, 3)
	if !strings.Contains(lines[0], `"type":"pong"`) {
		t.Errorf("第 1 行应是 pong，实际 %s", lines[0])
	}
	if !strings.Contains(lines[1], `"type":"control_error"`) {
		t.Errorf("未知 op 应回 control_error，实际 %s", lines[1])
	}
	if !strings.Contains(lines[2], `"type":"pong"`) {
		t.Errorf("未知 op 之后应继续读命令，实际 %s", lines[2])
	}
}

// TestControlBadJSON 非 JSON 行回 control_error 并继续。
func TestControlBadJSON(t *testing.T) {
	w := newLineWriter()
	ctl := startControlReader(strings.NewReader("这不是 JSON\n"+`{"op":"ping"}`+"\n"),
		newEventSink(w, "claude", true), controlHooks{})
	defer ctl.Close()

	lines := w.wait(t, 2)
	if !strings.Contains(lines[0], "control_error") {
		t.Errorf("非 JSON 行应回 control_error，实际 %s", lines[0])
	}
	if !strings.Contains(lines[1], "pong") {
		t.Errorf("之后应继续读，实际 %s", lines[1])
	}
}

// TestControlAnswer 常驻不可用时 answer 要回**可读的** control_error（不静默丢）；
// 可用时回 answered 且文本原样送达。
func TestControlAnswer(t *testing.T) {
	t.Run("常驻不可用", func(t *testing.T) {
		w := newLineWriter()
		ctl := startControlReader(strings.NewReader(`{"op":"answer","text":"选 A"}`+"\n"),
			newEventSink(w, "claude", true), controlHooks{})
		defer ctl.Close()
		ln := w.wait(t, 1)[0]
		if !strings.Contains(ln, "control_error") {
			t.Errorf("应回 control_error，实际 %s", ln)
		}
		if !strings.Contains(ln, "常驻") {
			t.Errorf("错误文案应说明走的是常驻会话，实际 %s", ln)
		}
	})

	t.Run("空文本", func(t *testing.T) {
		w := newLineWriter()
		ctl := startControlReader(strings.NewReader(`{"op":"answer"}`+"\n"),
			newEventSink(w, "claude", true), controlHooks{answer: func(string) bool { return true }})
		defer ctl.Close()
		if ln := w.wait(t, 1)[0]; !strings.Contains(ln, "control_error") {
			t.Errorf("空 text 应回 control_error，实际 %s", ln)
		}
	})

	t.Run("可用", func(t *testing.T) {
		w := newLineWriter()
		var got atomic.Value
		ctl := startControlReader(strings.NewReader(`{"op":"answer","text":"选 A"}`+"\n"),
			newEventSink(w, "claude", true), controlHooks{
				answer: func(text string) bool { got.Store(text); return true },
			})
		defer ctl.Close()
		if ln := w.wait(t, 1)[0]; !strings.Contains(ln, `"type":"answered"`) {
			t.Errorf("应回 answered，实际 %s", ln)
		}
		if v, _ := got.Load().(string); v != "选 A" {
			t.Errorf("答案文本 = %q want 选 A", v)
		}
	})
}

// TestControlInterruptAndStop interrupt 不立即回应答（应答在本轮真的结束时发），
// stop 立即回；两个标志位都要落。
func TestControlInterruptAndStop(t *testing.T) {
	w := newLineWriter()
	var gotReason atomic.Value
	ctl := startControlReader(
		strings.NewReader(`{"op":"interrupt","reason":"用户点了停止"}`+"\n"+`{"op":"stop"}`+"\n"),
		newEventSink(w, "claude", true), controlHooks{
			interrupt: func(reason string) { gotReason.Store(reason) },
		})
	defer ctl.Close()

	// interrupt 不发应答，所以第一条应当是 stop 的 stopped。
	if ln := w.wait(t, 1)[0]; !strings.Contains(ln, `"type":"stopped"`) {
		t.Errorf("第一条应答应是 stopped，实际 %s", ln)
	}
	if v, _ := gotReason.Load().(string); v != "用户点了停止" {
		t.Errorf("interrupt reason = %q want 用户点了停止", v)
	}
	if !ctl.WasInterrupted() {
		t.Error("WasInterrupted 应为 true")
	}
	if !ctl.WasStopped() {
		t.Error("WasStopped 应为 true")
	}
}

// TestControlStopCallsHook stop 要真的触发收工动作。
func TestControlStopCallsHook(t *testing.T) {
	w := newLineWriter()
	var stopped atomic.Bool
	ctl := startControlReader(strings.NewReader(`{"op":"stop"}`+"\n"),
		newEventSink(w, "claude", true), controlHooks{stop: func() { stopped.Store(true) }})
	defer ctl.Close()
	w.wait(t, 1)
	if !stopped.Load() {
		t.Error("stop hook 未调用")
	}
}

// TestKeepAlivePushAfterShutdown Push 与 shutdown 的竞态防线：
// 收工之后必须返回 false，**不能**向已关闭通道发送（那会 panic）。
func TestKeepAlivePushAfterShutdown(t *testing.T) {
	ka := &keepAlive{in: make(chan string, 1), appendCh: make(chan string, 1)}

	if !ka.Push("hello") {
		t.Fatal("收工前 Push 应成功")
	}
	if got := <-ka.in; got != "hello" {
		t.Errorf("Push 内容 = %q want hello", got)
	}

	ka.shutdown()
	if ka.Push("again") {
		t.Error("收工后 Push 必须返回 false")
	}
	ka.shutdown() // 幂等：重复收工不该 panic
	if ka.Push("") {
		t.Error("空文本不该被接受")
	}
}
