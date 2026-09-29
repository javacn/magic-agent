package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSessionWriterLazyOpenAndId 先 push 几条事件（无 session_id），再 setID，
// 验证：(a) 没 id 时进 pending；(b) 有 id 后开文件并把 pending 也写进去。
func TestSessionWriterLazyOpenAndId(t *testing.T) {
	dir := t.TempDir()
	sw := newSessionWriter(dir, 1)
	defer sw.close()

	sw.push(sessionEvent{Kind: "thinking", Text: "先想想"})
	sw.push(sessionEvent{Kind: "tool_use", Text: `{"file":"a.go"}`, Name: "Read", ID: "t1"})

	sw.setSessionID("sess-1")
	sw.push(sessionEvent{Kind: "text", Text: "你好"})

	drain(t, sw)

	lines := readLines(t, filepath.Join(dir, "sess-1.jsonl"))
	if len(lines) != 3 {
		t.Fatalf("期望 3 行历史，实际 %d：%v", len(lines), lines)
	}
	var first, second, third sessionEvent
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[2]), &third); err != nil {
		t.Fatal(err)
	}
	if first.Kind != "thinking" || second.Kind != "tool_use" || third.Kind != "text" {
		t.Fatalf("顺序不对：%s / %s / %s", first.Kind, second.Kind, third.Kind)
	}
	if third.SessionID != "sess-1" {
		t.Fatalf("第三行应带 session_id=sess-1，实得 %q", third.SessionID)
	}
	if first.SessionID != "sess-1" {
		t.Fatalf("pending 行回填 id 失败：%q", first.SessionID)
	}
	if first.Seq >= second.Seq || second.Seq >= third.Seq {
		t.Fatalf("Seq 不单调：%d/%d/%d", first.Seq, second.Seq, third.Seq)
	}
}

// TestSessionWriterFilePermissions 落盘文件权限 0600。
func TestSessionWriterFilePermissions(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 下 0600 检查无意义")
	}
	dir := t.TempDir()
	sw := newSessionWriter(dir, 1)
	sw.setSessionID("perm-1")
	sw.push(sessionEvent{Kind: "text", Text: "你好"})
	drain(t, sw)
	st, err := os.Stat(filepath.Join(dir, "perm-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("期望 0600 权限，实得 %v", perm)
	}
}

// TestSessionWriterAppendMode 第二次开启相同 id 应在文件末尾续写，
// 而不是覆盖 —— 续接同一会话时尤其重要（避免重复第一条事件）。
func TestSessionWriterAppendMode(t *testing.T) {
	dir := t.TempDir()
	sw1 := newSessionWriter(dir, 1)
	sw1.setSessionID("sess-X")
	sw1.push(sessionEvent{Kind: "text", Text: "first"})
	drain(t, sw1)
	sw1.close()

	sw2 := newSessionWriter(dir, 2)
	sw2.setSessionID("sess-X")
	sw2.push(sessionEvent{Kind: "text", Text: "second"})
	drain(t, sw2)
	sw2.close()

	lines := readLines(t, filepath.Join(dir, "sess-X.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("期望 2 行（不覆盖），实得 %d", len(lines))
	}
	var a, b sessionEvent
	if err := json.Unmarshal([]byte(lines[0]), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &b); err != nil {
		t.Fatal(err)
	}
	if a.Text != "first" || b.Text != "second" {
		t.Fatalf("顺序或内容错：%q / %q", a.Text, b.Text)
	}
	if a.Seq < 1 || b.Seq < 1 {
		t.Fatalf("Seq 至少从 1 起：a=%d b=%d", a.Seq, b.Seq)
	}
}

// TestReadSessionLogMissing 没历史时不应 500 / 应返回 nil。
func TestReadSessionLogMissing(t *testing.T) {
	dir := t.TempDir()
	cli := SessionLogDirOverride
	defer func() { SessionLogDirOverride = cli }()
	SessionLogDirOverride = dir
	evs, err := ReadSessionLog("", "no-such-session")
	if err != nil {
		t.Fatalf("不存在的会话应该静默 OK：%v", err)
	}
	if evs != nil {
		t.Fatalf("不存在的会话应该返回 nil：%v", evs)
	}
}

// TestReadSessionLogSkipsCorruptLines 坏行不阻塞整体读取。
func TestReadSessionLogSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"seq":1,"kind":"text","text":"a","at":"2026-01-01T00:00:00Z","session_id":"s"}
{not json
{"seq":2,"kind":"text","text":"b","at":"2026-01-01T00:00:01Z","session_id":"s"}
`)
	_ = f.Close()
	cli := SessionLogDirOverride
	defer func() { SessionLogDirOverride = cli }()
	SessionLogDirOverride = dir
	evs, err := ReadSessionLog("", "s")
	if err != nil {
		t.Fatalf("读历史失败：%v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("坏行应跳过，剩 2 行，实际 %d", len(evs))
	}
	if evs[0].Text != "a" || evs[1].Text != "b" {
		t.Fatalf("顺序错：%v", evs)
	}
}

// TestSessionWriterRealisticStream 模拟「真实流」的写入模式：
//   - 首条事件（带 init session_id）→ writer 拿到 id → 开文件 + 把 pending 写盘
//   - 后续 thinking/text/tool_use/turn_end 都按时间顺序落盘
//   - 文件按行追加；可被 ReadSessionLog 完整读回
//
// 关键不变量：流路径断掉（写盘慢）时 pending 不丢 id，open 完就立即 flush；
// seq 单调递增；session_id 在 id 出现前后的行都能拿到。
func TestSessionWriterRealisticStream(t *testing.T) {
	dir := t.TempDir()
	cli := SessionLogDirOverride
	defer func() { SessionLogDirOverride = cli }()
	SessionLogDirOverride = dir

	sw, err := newSessionWriterAuto(sessionLogDir)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟 handleNDJSONLine 的边写边喂：先 3 条无 id，再设 id（init），再 4 条。
	sw.push(sessionEvent{Kind: "thinking", Text: "我应该"})
	sw.push(sessionEvent{Kind: "thinking", Text: "回「收到」"})
	sw.push(sessionEvent{Kind: "text", Text: "用户让我只回「收到」"})
	sw.setSessionID("sess-real")
	sw.push(sessionEvent{Kind: "text", Text: "收到"})
	sw.push(sessionEvent{Kind: "tool_use", Name: "Read", ID: "t1", Text: `{"file":"x"}`})
	sw.push(sessionEvent{Kind: "tool_result", ID: "t1", Text: "ok"})
	sw.push(sessionEvent{Kind: "turn_end", Text: "收到"})

	drain(t, sw)

	lines := readLines(t, filepath.Join(dir, "sess-real.jsonl"))
	if len(lines) != 7 {
		t.Fatalf("期望 7 行，实得 %d：\n%v", len(lines), lines)
	}

	want := []string{"thinking", "thinking", "text", "text", "tool_use", "tool_result", "turn_end"}
	for i, ln := range lines {
		var ev sessionEvent
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("line %d 解析失败: %v", i, err)
		}
		if ev.Kind != want[i] {
			t.Fatalf("line %d: 期望 kind=%s 实得 %s", i, want[i], ev.Kind)
		}
		if ev.SessionID != "sess-real" {
			t.Fatalf("line %d: 所有行都应带 session_id，实得 %q", i, ev.SessionID)
		}
		if i > 0 {
			var prev sessionEvent
			_ = json.Unmarshal([]byte(lines[i-1]), &prev)
			if ev.Seq <= prev.Seq {
				t.Fatalf("line %d: Seq 必须严格递增 a=%d b=%d", i, prev.Seq, ev.Seq)
			}
		}
	}

	// 反向验证 —— ReadSessionLog 能完整读回 7 条。
	evs, err := ReadSessionLog("", "sess-real")
	if err != nil {
		t.Fatalf("ReadSessionLog 失败：%v", err)
	}
	if len(evs) != 7 {
		t.Fatalf("ReadSessionLog 期望 7 条，实得 %d", len(evs))
	}
}

// drain 等 writer 的 in 通道全部写完。close 之后再等它把所有 pending 全 flush。
func drain(t *testing.T, sw *sessionWriter) {
	t.Helper()
	sw.close()
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// 保留一个简单的并发 sanity：setID 与 push 并发调用，writer 不能崩。
func TestSessionWriterConcurrentSetIDAndPush(t *testing.T) {
	dir := t.TempDir()
	sw := newSessionWriter(dir, 1)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			sw.setSessionID("id-" + string(rune('a'+i%26)))
		}(i)
		go func(i int) {
			defer wg.Done()
			sw.push(sessionEvent{Kind: "text", Text: "msg"})
			time.Sleep(time.Millisecond)
		}(i)
	}
	wg.Wait()
	sw.close()
}
