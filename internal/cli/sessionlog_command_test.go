package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `--session-log`（会话历史的读取命令）的端到端验证。
//
// 为什么值得这些断言：这条命令是 magic-agent **不跑服务** 时，调用方（掌天瓶等）
// 拿会话历史的**唯一**通道。它坏掉的表现是「界面历史整段空白」，而那属于静默失败 ——
// 所以下面既钉住正常路径，也钉住几个容易做错的边界：
//   - 追平（after == lastSeq）必须是**正常空增量**，不能报 snapshotRequired；
//   - 游标超前（after > lastSeq）才要求重拉全量 —— 判据是**严格大于**；
//   - `--after` 单用必须**报错**，不能静默忽略（本项目反复踩过静默忽略的坑）；
//   - 没有历史的会话是**空数组 + 成功**，不是错误。

// writeSessionLog 造一份会话历史文件，返回日志目录。
func writeSessionLog(t *testing.T, sessionID, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := SessionLogDirOverride
	t.Cleanup(func() { SessionLogDirOverride = prev })
	SessionLogDirOverride = dir
	return dir
}

const sampleLog = `{"seq":1,"at":"2026-01-01T00:00:00Z","kind":"user","text":"帮我改登录页","session_id":"s1"}
{"seq":2,"at":"2026-01-01T00:00:01Z","kind":"thinking","text":"先看目录","session_id":"s1"}
{"seq":3,"at":"2026-01-01T00:00:02Z","kind":"text","text":"改好了","session_id":"s1"}
`

// logEnvelope 与插件 /desk/session/{id}/messages 的同形状：{session, events}。
type logEnvelope struct {
	Session string            `json:"session"`
	Events  []json.RawMessage `json:"events"`
}

// logDelta 与插件 /desk/session/{id}/events 的同形状。
type logDelta struct {
	Session          string            `json:"session"`
	Events           []json.RawMessage `json:"events"`
	Count            int               `json:"count"`
	After            uint64            `json:"after"`
	LastSeq          uint64            `json:"lastSeq"`
	SnapshotRequired bool              `json:"snapshotRequired"`
}

// TestSessionLogFull 全量读：形状与插件一致、顺序按 seq。
func TestSessionLogFull(t *testing.T) {
	writeSessionLog(t, "s1", sampleLog)

	out, _, err := runAskCmd(t, "", "--session-log", "s1")
	if err != nil {
		t.Fatalf("读历史不该报错：%v", err)
	}
	var got logEnvelope
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("输出不是 JSON：%v\n%s", err, out)
	}
	if got.Session != "s1" {
		t.Errorf("session = %q，想要 s1", got.Session)
	}
	if len(got.Events) != 3 {
		t.Fatalf("事件数 = %d，想要 3", len(got.Events))
	}
	// 顺序必须是 seq 升序（历史接口靠 seq 做游标，乱了续读就废了）
	var first struct {
		Seq  uint64 `json:"seq"`
		Kind string `json:"kind"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(got.Events[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || first.Kind != "user" || first.Text != "帮我改登录页" {
		t.Errorf("首条不对：%+v", first)
	}
}

// TestSessionLogAfter 增量：只回 seq > after 的部分。
func TestSessionLogAfter(t *testing.T) {
	writeSessionLog(t, "s1", sampleLog)

	out, _, err := runAskCmd(t, "", "--session-log", "s1", "--after", "2")
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	var got logDelta
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("输出不是 JSON：%v\n%s", err, out)
	}
	if got.Count != 1 || len(got.Events) != 1 {
		t.Fatalf("after=2 应只剩 1 条，实际 %d", len(got.Events))
	}
	if got.After != 2 || got.LastSeq != 3 {
		t.Errorf("after/lastSeq = %d/%d，想要 2/3", got.After, got.LastSeq)
	}
	if got.SnapshotRequired {
		t.Error("还有新事件时不该要求重拉全量")
	}
}

// TestSessionLogAfterCaughtUp 追平（after == lastSeq）：正常空增量，**不**要求重拉。
//
// ⚠️ 这条最容易写错：判据若写成 `>=`，每次追平都会让客户端白拉一遍全量历史。
func TestSessionLogAfterCaughtUp(t *testing.T) {
	writeSessionLog(t, "s1", sampleLog)

	out, _, err := runAskCmd(t, "", "--session-log", "s1", "--after", "3")
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	var got logDelta
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 0 {
		t.Errorf("追平时应是空增量，实际 %d 条", got.Count)
	}
	if got.LastSeq != 3 {
		t.Errorf("lastSeq = %d，想要 3", got.LastSeq)
	}
	if got.SnapshotRequired {
		t.Error("追平（after == lastSeq）不是异常，不该要求重拉全量")
	}
}

// TestSessionLogAfterAhead 游标超前磁盘：增量不可信，要求重拉全量。
func TestSessionLogAfterAhead(t *testing.T) {
	writeSessionLog(t, "s1", sampleLog)

	out, _, err := runAskCmd(t, "", "--session-log", "s1", "--after", "9")
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	var got logDelta
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !got.SnapshotRequired {
		t.Error("游标比磁盘还新时必须要求重拉全量，否则客户端会一直以为没有新事件")
	}
	if got.Count != 0 {
		t.Errorf("超前时应无事件，实际 %d 条", got.Count)
	}
}

// TestSessionLogMissing 没有历史的会话：空数组 + 成功（空历史不是错）。
func TestSessionLogMissing(t *testing.T) {
	writeSessionLog(t, "s1", sampleLog)

	out, _, err := runAskCmd(t, "", "--session-log", "no-such-session")
	if err != nil {
		t.Fatalf("不存在的会话不该报错：%v", err)
	}
	var got logEnvelope
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 0 {
		t.Errorf("应为空数组，实际 %d 条", len(got.Events))
	}
	// 必须是 []，不能是 null —— 调用方写 `for (ev of events)` 时 null 会直接抛。
	if !strings.Contains(out, `"events":[]`) {
		t.Errorf("空历史要输出 []，实际：%s", out)
	}
}

// TestSessionLogNeedsID 空 id 要报错（走参数校验，掉进提问分支的话会报 empty prompt）。
func TestSessionLogNeedsID(t *testing.T) {
	writeSessionLog(t, "s1", sampleLog)

	_, _, err := runAskCmd(t, "", "--session-log", "")
	if err == nil {
		t.Fatal("空 id 应该报错")
	}
	if !strings.Contains(err.Error(), "需要会话 id") {
		t.Errorf("错误信息该说清要 id，实际：%v", err)
	}
}

// TestAfterAloneIsRejected `--after` 不给 `--session-log` 时必须报错。
//
// 为什么单列一条：本项目对「传了却被静默忽略」的开关有过多次事故记录
// （README 的「-t 600s 被静默忽略」）。`--after` 单独出现是典型的误用，
// 静默忽略会让调用方以为增量读生效了、其实拿到的是全量。
func TestAfterAloneIsRejected(t *testing.T) {
	writeSessionLog(t, "s1", sampleLog)

	_, _, err := runAskCmd(t, "", "--after", "5")
	if err == nil {
		t.Fatal("--after 单用应该报错，不能被静默忽略")
	}
	if !strings.Contains(err.Error(), "--session-log") {
		t.Errorf("错误信息该点出要配合 --session-log，实际：%v", err)
	}
}
