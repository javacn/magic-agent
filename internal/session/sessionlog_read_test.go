package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 会话日志的增量读（断线续读）。这一组钉住三件最容易写错的事：
//
//  1. after 是**排他**上界（不回 seq == after 那条）——写成 >= 会让客户端每次续读
//     都重收一条，去重逻辑就被迫成为必需；
//  2. 返回的 maxSeq 覆盖**整个文件**而不只是这一页 —— 它是「客户端游标是否超前」的
//     唯一判据，只统计过滤后的事件就永远判不出超前；
//  3. 文件不存在时不是错误（新会话没有历史是正常状态）。

// seqEvent 测试用的事件类型：带 seq 与 text 两个字段。
type seqEvent struct {
	Seq  uint64 `json:"seq"`
	Text string `json:"text"`
}

func parseSeqEvent(line []byte) (seqEvent, error) {
	var ev seqEvent
	err := json.Unmarshal(line, &ev)
	return ev, err
}

// writeLog 写一份 fixture 会话日志，返回目录。
func writeLog(t *testing.T, id string, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	if len(lines) > 0 {
		body := ""
		for _, l := range lines {
			body += l + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestReadSessionLogAfterFiltersBySeq(t *testing.T) {
	dir := writeLog(t, "s", `{"seq":1,"text":"a"}`, `{"seq":2,"text":"b"}`, `{"seq":3,"text":"c"}`)

	// after=1 → 只回 2、3（排他）。
	evs, maxSeq, err := ReadSessionLogAfter(dir, "s", 1, func(e seqEvent) uint64 { return e.Seq }, parseSeqEvent)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Seq != 2 || evs[1].Seq != 3 {
		t.Fatalf("after=1 期望 [2 3]，实得 %+v", evs)
	}
	if maxSeq != 3 {
		t.Fatalf("maxSeq 期望 3，实得 %d", maxSeq)
	}

	// after=0 → 全量（seq 从 1 起，所以 0 等价于「没过滤」）。
	evs, _, err = ReadSessionLogAfter(dir, "s", 0, func(e seqEvent) uint64 { return e.Seq }, parseSeqEvent)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("after=0 期望 3 条，实得 %d", len(evs))
	}
}

// TestReadSessionLogAfterMaxSeqCoversWholeFile 是这一组里最要紧的一条：
// maxSeq 必须反映**整个文件**，不能只统计返回的那一页。
//
// 为什么：调用方用 `after > maxSeq` 判「客户端游标超前了（日志被重置 / 游标来自
// 别的会话）」，超时时得以全量重建。若 maxSeq 只统计过滤结果，after=99 这类超前
// 情形会得到 maxSeq=0，判据恒为真 —— 客户端会被永久判成「超前」，每次进会话都
// 重拉全量，增量能力形同虚设。
func TestReadSessionLogAfterMaxSeqCoversWholeFile(t *testing.T) {
	dir := writeLog(t, "s", `{"seq":1,"text":"a"}`, `{"seq":7,"text":"b"}`)

	// after 超前（99 > 7）：这一页是空的，但 maxSeq 仍须是 7。
	evs, maxSeq, err := ReadSessionLogAfter(dir, "s", 99, func(e seqEvent) uint64 { return e.Seq }, parseSeqEvent)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("after=99 期望空页，实得 %+v", evs)
	}
	if maxSeq != 7 {
		t.Fatalf("超前时 maxSeq 仍须覆盖整个文件（期望 7，实得 %d）", maxSeq)
	}
	if !(99 > maxSeq) {
		t.Fatalf("判据 `after > maxSeq` 应为 true（客户端超前），maxSeq=%d", maxSeq)
	}

	// 追平（after == maxSeq）：空页，且判据为 false（这是正常的「没有新事件」）。
	evs, maxSeq, err = ReadSessionLogAfter(dir, "s", 7, func(e seqEvent) uint64 { return e.Seq }, parseSeqEvent)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("已追平期望空页，实得 %+v", evs)
	}
	if 7 > maxSeq {
		t.Fatalf("追平时不该判成超前（maxSeq=%d）", maxSeq)
	}
}

// TestReadSessionLogAfterMissingFile 没历史不是错（新会话 / 从未落盘）。
func TestReadSessionLogAfterMissingFile(t *testing.T) {
	dir := t.TempDir()
	evs, maxSeq, err := ReadSessionLogAfter(dir, "nope", 3, func(e seqEvent) uint64 { return e.Seq }, parseSeqEvent)
	if err != nil {
		t.Fatalf("文件不存在不该报错，实得 %v", err)
	}
	if evs != nil || maxSeq != 0 {
		t.Fatalf("期望 (nil, 0)，实得 (%+v, %d)", evs, maxSeq)
	}
}

// TestReadSessionLogAfterNilSeqOf 不给 seqOf 时退化成全量（且 maxSeq 恒 0）。
//
// 这两个参数是配套的：seqOf 为 nil 就没法过滤，after 无意义。
func TestReadSessionLogAfterNilSeqOf(t *testing.T) {
	dir := writeLog(t, "s", `{"seq":1,"text":"a"}`, `{"seq":2,"text":"b"}`)
	evs, maxSeq, err := ReadSessionLogAfter(dir, "s", 5, nil, parseSeqEvent)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || maxSeq != 0 {
		t.Fatalf("seqOf=nil 期望全量 2 条 + maxSeq=0，实得 %d 条 / maxSeq=%d", len(evs), maxSeq)
	}
}

// TestReadSessionLogAfterSkipsCorruptLines 坏行不该让整次续读失败。
func TestReadSessionLogAfterSkipsCorruptLines(t *testing.T) {
	dir := writeLog(t, "s", `{"seq":1,"text":"a"}`, `{ this is not json`, `{"seq":3,"text":"c"}`)
	evs, maxSeq, err := ReadSessionLogAfter(dir, "s", 1, func(e seqEvent) uint64 { return e.Seq }, parseSeqEvent)
	if err != nil {
		t.Fatalf("坏行不该报错，实得 %v", err)
	}
	if len(evs) != 1 || evs[0].Seq != 3 {
		t.Fatalf("期望只回 seq=3，实得 %+v", evs)
	}
	if maxSeq != 3 {
		t.Fatalf("maxSeq 期望 3（坏行跳过但正常行仍算），实得 %d", maxSeq)
	}
}

// TestReadSessionLogAfterEmptyDirRejected 空 dir 必须报错。
//
// 这是防「默认进 home 写/读错路径」那类难追的 bug：调用方必须显式给目录。
func TestReadSessionLogAfterEmptyDirRejected(t *testing.T) {
	if _, _, err := ReadSessionLogAfter("", "s", 0, nil, parseSeqEvent); err == nil {
		t.Fatal("空 dir 期望报错，实得 nil")
	}
}
