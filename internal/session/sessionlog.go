package session

// sessionlog.go - 「看历史」会话持久化的纯读写路径。
//
// 谁用：core CLI 在流式跑时 append 事件到 `<dir>/<session_id>.jsonl`；
// plugin (`magic-client`) 在 `GET /desk/session/{id}/messages` 路由上读这个文件。
// 双方都在 `~/.magic-agent/sessions/`（同一目录）。
//
// 与 `Begin/Finish` 的关系：这里只管「事件落盘」的内容（流式输出每条事件）；
// 登记表（`run-*.json`）只记元数据（提示词、状态、pid）。两条线相互独立。
//
// 读写路径都放在 `session` 包，而不是 `cli`：因为 plugin (`internal/client`) 也要读。
// 跨包依赖方向：`internal/client -> internal/session`，避免 `internal/client -> internal/cli`。
//
// 设计约束：
//   - 文件不存在 ≠ 错：返回空 slice + nil error（UI 上「空会话」也是合理状态）。
//   - 坏行不阻塞：NDJSON 解析失败的行 skip 一次（stderr 打 warning）；
//     文件里其它正常行仍能取到。
//   - 大文件：16MB 单行上限（result 行可能很大）；调用方按 seq 翻页可另写。

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DefaultLogDir 返回 ~/.magic-agent/sessions/（与 core CLI 用的目录一致）。
//
// 谁用：plugin (`internal/client`) 在 plugin 进程里直接读历史时 —— core CLI 是另一
// 个进程，得在 plugin 这边再做一次解析。
func DefaultLogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("找不到家目录：%w", err)
	}
	return filepath.Join(home, ".magic-agent", "sessions"), nil
}

// SeqOf 从**已解析**的一条事件里取出 seq。
//
// 为什么要回调而不是要求 T 带字段：本包被 CLI（`sessionEvent` 结构体）与 plugin
// （`map[string]any`，它不想依赖 cli 的 schema）两边共用，两者取 seq 的方式不同。
type SeqOf[T any] func(T) uint64

// ReadSessionLog 读整个 jsonl 到 `[]T`（每条事件一行 JSON）。
//
// dir 为空时报错（避免「默认进 home 写错了路径」这种难追的 bug，调用方必须显式传）。
// 这是**泛型**，因为 events 的具体类型由调用方决定（CLI 用 `sessionEvent`、
// plugin 也用同一类型 —— 所以 plugin 这边不需要再写一份）。
//
// 顺序：seq 升序；空文件返回 nil。损坏行跳过（warn 一行）。
func ReadSessionLog[T any](dir, sessionID string, parse func([]byte) (T, error)) ([]T, error) {
	evs, _, err := ReadSessionLogAfter(dir, sessionID, 0, nil, parse)
	return evs, err
}

// ReadSessionLogAfter 只读 seq > after 的事件（**断线续读**用）。
// 返回 (事件, 文件里的最大 seq, error)。after = 0 等价于全量。
//
// 语义对齐 agents-anywhere 的 `GET /sessions/{id}/events?after=seq:N`：
// 客户端报「我已经看到 seq=N」，服务端就从 N+1 开始给。
//
// ⚠️ after 是**排他**上界：不回 seq == after 的那条。重复回一条会让客户端的去重
//
//	逻辑从「优化」变成「必需」—— 而续读的全部意义就是让客户端不必再关心去重。
//
// ⚠️ 返回的 maxSeq 覆盖**整个文件**（含被 after 滤掉的部分）：调用方要用它判断
//
//	「客户端的游标是不是超前了」（after > maxSeq ⇒ 客户端状态比磁盘还新，
//	只可能是日志被重置过、或游标来自另一个会话）。只统计过滤后的事件得不到这个信息。
//
// ⚠️ 仍然顺序读完整文件后过滤，没有按 seq 做**定位**（文件是 NDJSON、seq 靠解析才拿到，
//
//	无法 seek）。代价是每次续读都要解析整个文件；收益是**网络只传增量**。
//	本项目的会话规模下这个取舍是划算的（本地解析毫秒级，跨网传全量是百 KB 级）。
func ReadSessionLogAfter[T any](dir, sessionID string, after uint64, seqOf SeqOf[T], parse func([]byte) (T, error)) ([]T, uint64, error) {
	if dir == "" {
		return nil, 0, fmt.Errorf("ReadSessionLog: dir 不能为空（调用方需用 DefaultLogDir() 解析）")
	}
	f, err := os.Open(filepath.Join(dir, sessionID+".jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil // 没历史 ≠ 错
		}
		return nil, 0, err
	}
	defer f.Close()
	return readNDJSON(f, after, seqOf, parse)
}

// readNDJSON 流式读 NDJSON。空行 / 坏行跳过（warn 一次）。
//
// after / seqOf 都可选：seqOf 为 nil 时不按 seq 过滤（此时 after 无意义），
// 返回的 maxSeq 也就恒为 0 —— 这两个参数是配套的，调用方别只给一半。
func readNDJSON[T any](r io.Reader, after uint64, seqOf SeqOf[T], parse func([]byte) (T, error)) ([]T, uint64, error) {
	var out []T
	var maxSeq uint64
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		v, err := parse(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "magic-agent: 会话历史行解析失败：%v（跳过该行）\n", err)
			continue
		}
		if seqOf == nil {
			out = append(out, v)
			continue
		}
		s := seqOf(v)
		// maxSeq 先更新、后过滤：它要反映整个文件，而不只是这一页。
		if s > maxSeq {
			maxSeq = s
		}
		if s <= after {
			continue
		}
		out = append(out, v)
	}
	return out, maxSeq, sc.Err()
}
