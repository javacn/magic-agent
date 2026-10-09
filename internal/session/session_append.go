package session

// session_append.go - 常驻会话的「追加消息」通道（unix domain socket）。
//
// 用户需求（2026-09-18）：「按路线 2 实现，先支持 claude 和 codebuddy 的追加功能」。
//
// 机制：claude/codebuddy 的 `--input-format stream-json` 允许在**同一个进程**里持续
// 接收 user 消息（= 会话中追加需求）。所以常驻会话（`--keep-alive`）会在自己的
// 会话记录里登记一个 unix socket 路径，任何进程都能：
//
//	magic-agent --append <session_id> -p "追加内容"
//
// 由 `AppendMessage` 连上去把消息交给常驻进程，常驻进程再写进引擎的 stdin。
//
// 为什么用 unix socket 而不是 TCP/FIFO：
//   - 权限天然受控：socket 文件 0600 + 目录 0700 → 只有同用户能追加，不需要额外 token；
//   - FIFO 的「最后一个写者关闭 → 读端 EOF」语义会把引擎的 stdin 关掉，不能用；
//   - TCP 需要暴露端口 + token 校验，在单机场景纯属多余。
//
// Windows 没有 unix socket → `ListenAppend` 明确报错（`--keep-alive` 会被挡下并说明原因），
// 不做静默降级。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// AppendProtocolVersion 追加协议版本（客户端/服务端握手用，便于以后演进）。
const AppendProtocolVersion = 1

// maxUnixSocketPath unix socket 路径的保守上限：macOS 的 sun_path 是 104 字节
// （Linux 108），留点余量取 100。
const maxUnixSocketPath = 100

// appendRequest 客户端 → 常驻进程 的一条消息。
type appendRequest struct {
	Version int    `json:"version"`
	Text    string `json:"text"`
}

// appendResponse 常驻进程 → 客户端的应答。
type appendResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Appender 常驻会话的追加入口（一个 unix socket）。
type Appender struct {
	ln   net.Listener
	path string
}

// Path 返回 socket 路径（写进会话记录，客户端据此连接）。
func (a *Appender) Path() string { return a.path }

// Serve 把收到的消息推给 ch（阻塞直到 ln 关闭或 ch 关闭）。
//
// 不关闭 ch —— 通道的生命周期归调用方（CLI）：空闲超时/收到停止信号时由它关闭，
// 从而让引擎 stdin EOF、常驻会话优雅收尾。
func (a *Appender) Serve(ch chan<- string) {
	for {
		conn, err := a.ln.Accept()
		if err != nil {
			return // ln 已关闭
		}
		go a.handle(conn, ch)
	}
}

// handle 处理一条连接：读一行 JSON，推入通道，回一行应答。
func (a *Appender) handle(conn net.Conn, ch chan<- string) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return
	}
	var req appendRequest
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &req); err != nil {
		writeAppendResponse(conn, appendResponse{Error: "请求不是合法 JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeAppendResponse(conn, appendResponse{Error: "追加内容为空"})
		return
	}
	select {
	case ch <- req.Text:
		writeAppendResponse(conn, appendResponse{OK: true})
	case <-time.After(5 * time.Second):
		// 通道满（追加太快）：明确告知，不假装成功
		writeAppendResponse(conn, appendResponse{Error: "追加队列已满，稍后再试"})
	}
}

// writeAppendResponse 回一行 JSON 应答（尽力而为，写不出去就算了）。
func writeAppendResponse(w io.Writer, resp appendResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = w.Write(append(data, '\n'))
}

// Close 关掉监听并删掉 socket 文件（幂等）。
func (a *Appender) Close() {
	if a == nil {
		return
	}
	if a.ln != nil {
		_ = a.ln.Close()
	}
	if a.path != "" {
		_ = os.Remove(a.path)
	}
}

// AppendTransportAvailable 报告本平台是否具备「追加入口」的传输能力。
//
// 存在的意义（2026-10-01）：Windows 没有 unix domain socket，
// ListenAppend 在那里必然失败。调用方（cli 层 kaEnabled）据此**提前**判定，
// 让「默认值触发的常驻」在 Windows 上静默不启用 —— 而不是让用户撞上
// 「--keep-alive: 常驻会话目前只支持 macOS/Linux」这种
// 「我没要求它开、它却报错」的错。
//
// 显式传 --keep-alive 时仍应报错（用户明确要了这件事，要说清为什么不行），
// 那个判断在 cli 层的 prepareAsk，不在这里。
func AppendTransportAvailable() bool {
	return runtime.GOOS != "windows"
}

// ListenAppend 为一条会话开追加入口，并把路径写进会话记录（客户端据此找到它）。
//
// 目录/文件权限：sessions 目录 0700、socket 文件 0600 → 仅同用户可连。
func ListenAppend(h *Handle) (*Appender, error) {
	if h == nil {
		return nil, fmt.Errorf("no session handle")
	}
	if !AppendTransportAvailable() {
		return nil, fmt.Errorf("常驻会话（--keep-alive）与 --append 目前只支持 macOS/Linux：Windows 没有 unix domain socket")
	}
	dir := Dir()
	if dir == "" {
		return nil, fmt.Errorf("cannot resolve sessions dir")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, h.RunID()+".sock")
	// unix socket 路径有硬上限（macOS sun_path 104 / Linux 108 字节），超了 bind 会报
	// 一句看不出原因的 "invalid argument"。提前拦住并给出可操作建议。
	if len(path) > maxUnixSocketPath {
		return nil, fmt.Errorf("追加入口路径过长（%d 字节，上限约 %d）：%s；"+
			"请把登记表目录换到短路径，例如 MAGIC_AGENT_SESSIONS=/tmp/magic-agent-sessions",
			len(path), maxUnixSocketPath, path)
	}
	_ = os.Remove(path) // 清掉可能残留的同名 socket
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	h.SetAppendPath(path)
	return &Appender{ln: ln, path: path}, nil
}

// AppendResult 一次追加的结果。
type AppendResult struct {
	Record Record `json:"record"`
	Queued bool   `json:"queued"`
	Reason string `json:"reason,omitempty"`
}

// AppendMessage 把一条消息追加进指定会话（session_id 或 run_id 都收）。
//
// 失败分两种，调用方按退出码区分：
//   - 找不到 id / 记录里没有追加入口 → 返回 error（exit 1，附可操作提示）
//   - 常驻进程已经退出（连接被拒）→ 返回 error（exit 1，提示会话已结束）
func AppendMessage(id, text string) (AppendResult, error) {
	msg := strings.TrimSpace(text)
	if msg == "" {
		return AppendResult{}, fmt.Errorf("追加内容为空")
	}
	rec, found, err := Find(id)
	if err != nil {
		return AppendResult{}, err
	}
	if !found {
		return AppendResult{}, fmt.Errorf("no such session %q（--sessions 可列出全部）", id)
	}
	if strings.TrimSpace(rec.Append) == "" {
		reason := "该会话没有追加入口（未用 --keep-alive 启动）"
		if rec.State != StateRunning {
			reason = "会话已结束（state=" + rec.State + "）"
		}
		return AppendResult{Record: rec, Reason: reason},
			fmt.Errorf("%s；常驻会话要用 --stream --keep-alive 启动，已结束的会话请用 --session <id> 续接", reason)
	}
	if rec.State != StateRunning {
		return AppendResult{Record: rec, Reason: "会话已结束（state=" + rec.State + "）"},
			fmt.Errorf("会话已结束（state=%s）；请用 --session <id> 续接", rec.State)
	}

	conn, derr := net.DialTimeout("unix", rec.Append, 5*time.Second)
	if derr != nil {
		// 记录说在跑、socket 却连不上：常驻进程多半刚退出（或崩了）
		rec.State = StateGone
		rec.UpdatedAt = time.Now()
		writeRecord(rec)
		return AppendResult{Record: rec, Reason: "常驻进程已不在（连接失败）"},
			fmt.Errorf("连不上会话 %s 的追加入口（%s）：常驻进程可能已退出；请用 --sessions 复核", id, rec.Append)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	req, _ := json.Marshal(appendRequest{Version: AppendProtocolVersion, Text: msg})
	if _, werr := conn.Write(append(req, '\n')); werr != nil {
		return AppendResult{Record: rec, Reason: "写入失败"}, fmt.Errorf("写入追加入口失败: %w", werr)
	}
	line, rerr := bufio.NewReader(conn).ReadString('\n')
	if rerr != nil && strings.TrimSpace(line) == "" {
		return AppendResult{Record: rec, Reason: "没收到应答"}, fmt.Errorf("追加入口没回应: %w", rerr)
	}
	var resp appendResponse
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); jerr != nil {
		return AppendResult{Record: rec, Reason: "应答不是 JSON"}, fmt.Errorf("追加入口应答异常: %s", strings.TrimSpace(line))
	}
	if !resp.OK {
		return AppendResult{Record: rec, Reason: resp.Error}, fmt.Errorf("追加被拒: %s", resp.Error)
	}
	rec.UpdatedAt = time.Now()
	writeRecord(rec)
	return AppendResult{Record: rec, Queued: true}, nil
}

// SetAppendPath 把追加入口路径写进记录（由 ListenAppend 调用）。
func (h *Handle) SetAppendPath(path string) {
	if h == nil {
		return
	}
	h.rec.Append = path
	h.write()
}
