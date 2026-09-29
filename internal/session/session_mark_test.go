package session

import (
	"os"
	"testing"
)

/* Handle.Mark：唯一的中途写状态入口（2026-09-29 统一会话状态词表时新增）。
 *
 * 它承担的是「引擎停下来等用户」这件事 —— 会话还活着，但**没在跑**，
 * 所以界面上该显示「等审批」而不是「生成中」。三条契约：
 *   ① running ↔ waiting_approval 能来回切，并且**落盘**（不是只改内存）；
 *   ② 终态不许被翻回去（一个迟到的流事件不能把已结束的会话写成还在跑）；
 *   ③ 不在这一对里的值一律不写（防止它被当成万能的改状态口子）。
 *
 * 为什么值得单独钉：这条路径在真机上不容易稳定复现（要引擎恰好发出一次
 * 提问 / 待授权），而它一旦写错，错的是「这条会话结没结束」这种结论性信息。 */
func TestHandleMarkWaitingApproval(t *testing.T) {
	t.Setenv(EnvDir, t.TempDir())
	h, err := Begin(BeginOptions{Engine: "claude", PID: os.Getpid(), PromptHead: "等审批"})
	if err != nil {
		t.Fatal(err)
	}
	runID := h.RunID()

	// ① 进「等审批」，且必须落盘
	h.Mark(StateWaitingApproval)
	if got := h.Record().State; got != StateWaitingApproval {
		t.Fatalf("内存状态 = %q, want %q", got, StateWaitingApproval)
	}
	rec, found, err := Find(runID)
	if err != nil || !found {
		t.Fatalf("读回记录失败: err=%v found=%v", err, found)
	}
	if rec.State != StateWaitingApproval {
		t.Errorf("落盘状态 = %q, want %q", rec.State, StateWaitingApproval)
	}

	// 用户作答（或这一轮的任何后续事件）→ 回到「正在跑」
	h.Mark(StateRunning)
	if got := h.Record().State; got != StateRunning {
		t.Errorf("作答后状态 = %q, want %q", got, StateRunning)
	}
	rec2, _, _ := Find(runID)
	if rec2.State != StateRunning {
		t.Errorf("回到 running 也要落盘，实际 %q", rec2.State)
	}
}

func TestHandleMarkDoesNotTouchTerminalState(t *testing.T) {
	t.Setenv(EnvDir, t.TempDir())

	// 正常收尾之后，迟到的「等审批」不许把结论翻掉
	h, _ := Begin(BeginOptions{Engine: "claude", PID: os.Getpid()})
	h.Finish("sid-1", StateDone)
	h.Mark(StateWaitingApproval)
	if got := h.Record().State; got != StateDone {
		t.Errorf("终态被改写了：%q, want %q", got, StateDone)
	}

	// 被打断的会话同样不许被翻回 running（那会让「已经断了」变成「还在跑」）
	h2, _ := Begin(BeginOptions{Engine: "claude", PID: os.Getpid()})
	h2.Finish("", StateInterrupted)
	h2.Mark(StateRunning)
	if got := h2.Record().State; got != StateInterrupted {
		t.Errorf("被打断的会话被翻回 running 了：%q", got)
	}
}

func TestHandleMarkRejectsOtherStates(t *testing.T) {
	t.Setenv(EnvDir, t.TempDir())
	h, _ := Begin(BeginOptions{Engine: "claude", PID: os.Getpid()})
	h.Mark(StateFailed) // 不在允许的一对里
	h.Mark(StateDone)
	h.Mark("")
	if got := h.Record().State; got != StateRunning {
		t.Errorf("Mark 不该把它改成 %q（只允许 running / waiting_approval）", got)
	}
}

/* 旧记录里的 `stopped` 读出来必须是 `cancelled`：磁盘上有别的版本写下的老值，
 * 读的时候不归一，界面上就会冒出一个谁也不认识的词。 */
func TestNormalizeLegacyState(t *testing.T) {
	if got := normalizeState("stopped"); got != StateCancelled {
		t.Errorf("normalizeState(stopped) = %q, want %q", got, StateCancelled)
	}
	for _, s := range []string{StateRunning, StateWaitingApproval, StateDone, StateFailed, StateCancelled, StateInterrupted, StateGone} {
		if got := normalizeState(s); got != s {
			t.Errorf("normalizeState(%q) 不该改它，得到 %q", s, got)
		}
	}
	if got := normalizeState("who_knows"); got != "who_knows" {
		t.Errorf("不认识的词原样返回，得到 %q", got)
	}
}
