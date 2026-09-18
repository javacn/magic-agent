package cli

// keepalive_test.go - CLI 侧「常驻会话 + 追加消息」的测试。
//
// 用户需求（2026-09-18）：「按路线 2 实现，先支持 claude 和 codebuddy 的追加功能」。
//
// 覆盖：--keep-alive 的前置校验（必须 --stream / 引擎必须支持 / --idle 必须为正）/
// --append 的参数校验与错误路径 / 端到端（起常驻会话 → --append 投递 → 空闲收工）。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darren/magic-agent/internal/agent"
	"github.com/darren/magic-agent/internal/session"
)

// kaEngine 假引擎：模拟 claude/codebuddy 的常驻会话 —— 消费 req.Append，
// 每收到一条就发一个 turn_end（真实引擎在 result 行时发）。
type kaEngine struct {
	name string
	mu   sync.Mutex
	got  []string
}

func (k *kaEngine) Name() string           { return k.name }
func (k *kaEngine) Detect() (bool, string) { return true, "fake://" + k.name }
func (k *kaEngine) Complete(context.Context, agent.Request) (agent.Response, error) {
	return agent.Response{Text: "complete", Model: "fake-model"}, nil
}
func (k *kaEngine) Stream(_ context.Context, req agent.Request, onEvent func(agent.StreamEvent)) (agent.StreamResult, error) {
	if req.Append == nil {
		return agent.StreamResult{}, errors.New("kaEngine 需要 Append 通道")
	}
	for msg := range req.Append {
		k.mu.Lock()
		k.got = append(k.got, msg)
		k.mu.Unlock()
		if onEvent != nil {
			onEvent(agent.StreamEvent{Kind: agent.KindTurnEnd, Text: "ok"})
		}
	}
	return agent.StreamResult{
		Response: agent.Response{Engine: k.name, Text: "done", SessionID: "sid-ka", Model: "fake-model"},
	}, nil
}
func (k *kaEngine) received() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.got...)
}
func (k *kaEngine) reset() {
	k.mu.Lock()
	k.got = nil
	k.mu.Unlock()
}

// ⚠️ 整包共用一个 "codebuddy" 假引擎：`agent.Lookup` 取注册表里**首个**同名匹配，
// 每个用例各注册一个的话，后注册的永远不被使用（我第一版就是这么挂的：
// 追加投递成功、断言却打在另一个实例上）。用前 reset 即可。
var (
	kaRegisterOnce sync.Once
	kaShared       = &kaEngine{name: "codebuddy"}
)

func kaEngineForTest(t *testing.T) *kaEngine {
	t.Helper()
	kaRegisterOnce.Do(func() { registerFake(kaShared) })
	kaShared.reset()
	return kaShared
}

// shortSessionsDir 给测试一个**短**的登记表目录（unix socket 路径有 104 字节上限）。
func shortSessionsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mas-cli-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// ── 前置校验 ─────────────────────────────────────────────────

func TestKeepAliveRequiresStream(t *testing.T) {
	kaEngineForTest(t)
	_, _, err := runAskCmd(t, "", "-e", "codebuddy", "--keep-alive", "-p", "hi")
	if err == nil {
		t.Fatal("--keep-alive 不配 --stream 应报错")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("应是 usageError(exit 2), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "--stream") {
		t.Errorf("错误信息应点明要 --stream: %v", err)
	}
}

// 不支持的引擎：直接测能力分支（不走注册表 —— cli 包测试里的引擎是假的，
// 且同名只认首个注册，用 -e trae 跑真流程会先撞上别的假引擎的「不支持流式」）。
func TestKeepAliveUnsupportedEngine(t *testing.T) {
	t.Setenv(session.EnvDir, shortSessionsDir(t))
	cmd := NewRootCommand()
	opts := newAskOptions()
	opts.keepAlive, opts.stream, opts.idle = true, true, time.Second

	_, err := startKeepAlive(cmd, opts, "trae", nil)
	if err == nil {
		t.Fatal("不支持的引擎应报错")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("应是 usageError(exit 2), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), "codebuddy") {
		t.Errorf("错误信息应说明当前支持 claude/codebuddy: %v", err)
	}
}

func TestKeepAliveRejectsNegativeIdle(t *testing.T) {
	kaEngineForTest(t)
	_, _, err := runAskCmd(t, "", "-e", "codebuddy", "--stream", "--idle", "-1s", "-p", "hi")
	if err == nil {
		t.Fatal("--idle 负值应报错")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("应是 usageError, got %T: %v", err, err)
	}
}

// 默认就是常驻：claude/codebuddy 的 --stream 调用不传 --keep-alive 也会开出追加入口。
func TestKeepAliveIsDefaultForStreaming(t *testing.T) {
	dir := shortSessionsDir(t)
	t.Setenv(session.EnvDir, dir)
	eng := kaEngineForTest(t)

	done := make(chan error, 1)
	go func() {
		_, _, err := runAskCmd(t, "", "-e", "codebuddy", "--stream", "--idle", "1s", "-p", "首轮")
		done <- err
	}()

	var runID string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && runID == "" {
		for _, rec := range mustListSessions(t, dir) {
			if rec.Append != "" {
				runID = rec.RunID
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("默认（不传 --keep-alive）也该开出追加入口")
	}
	// 追加可用
	if _, _, err := runAskCmd(t, "", "--append", runID, "-p", "默认模式下的追加"); err != nil {
		t.Fatalf("--append: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("收尾报错: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("没收工")
	}
	if got := eng.received(); len(got) != 1 || got[0] != "默认模式下的追加" {
		t.Errorf("引擎收到的追加 = %v", got)
	}
}

// --keep-alive=false 关掉常驻：不建 socket，turn 结束即退出（老行为）。
func TestKeepAliveCanBeDisabled(t *testing.T) {
	dir := shortSessionsDir(t)
	t.Setenv(session.EnvDir, dir)
	registerFake(&streamingEngine{name: "fake-ka-off", text: "ok"})

	if _, _, err := runAskCmd(t, "", "-e", "fake-ka-off", "--stream", "--keep-alive=false", "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	recs := mustListSessions(t, dir)
	if len(recs) != 1 {
		t.Fatalf("登记表条数 = %d", len(recs))
	}
	if recs[0].Append != "" {
		t.Errorf("--keep-alive=false 不该开追加入口: %q", recs[0].Append)
	}
}

// 非流式调用默认不常驻（否则普通提问会挂住等追加）。
func TestKeepAliveNotAppliedToNonStream(t *testing.T) {
	dir := shortSessionsDir(t)
	t.Setenv(session.EnvDir, dir)
	registerFake(&stringEngine{name: "fake-ka-nostream", text: "ok"})

	if _, _, err := runAskCmd(t, "", "-e", "fake-ka-nostream", "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	recs := mustListSessions(t, dir)
	if len(recs) != 1 || recs[0].Append != "" {
		t.Errorf("非流式调用不该开追加入口: %+v", recs)
	}
}

// ── --append 错误路径 ────────────────────────────────────────

func TestAppendRequiresIDAndText(t *testing.T) {
	t.Setenv(session.EnvDir, shortSessionsDir(t))

	_, _, err := runAskCmd(t, "", "--append", "")
	if err == nil {
		t.Fatal("空 id 应报错")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("空 id 应是 usageError, got %T: %v", err, err)
	}

	_, _, err = runAskCmd(t, "", "--append", "some-id")
	if err == nil {
		t.Fatal("没有内容应报错")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("空内容应是 usageError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "-p") {
		t.Errorf("错误信息应说明怎么给内容: %v", err)
	}
}

func TestAppendUnknownSession(t *testing.T) {
	t.Setenv(session.EnvDir, shortSessionsDir(t))

	_, _, err := runAskCmd(t, "", "--append", "no-such-session", "-p", "追加")
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

// 普通（非常驻）会话不能追加 —— 错误要指出用 --keep-alive 启动、已结束用 --session 续接。
func TestAppendToNonKeepAliveSession(t *testing.T) {
	t.Setenv(session.EnvDir, shortSessionsDir(t))
	registerFake(&stringEngine{name: "fake-ka-none", text: "ok"})

	if _, _, err := runAskCmd(t, "", "-e", "fake-ka-none", "普通调用"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	rows := parseSessions(t, mustSessions(t))
	if len(rows) != 1 {
		t.Fatalf("登记表条数 = %d", len(rows))
	}

	_, _, err := runAskCmd(t, "", "--append", rows[0].RunID, "-p", "追加")
	if err == nil {
		t.Fatal("没有追加入口应报错")
	}
	if !strings.Contains(err.Error(), "--keep-alive") {
		t.Errorf("错误信息应点明要用 --keep-alive 启动: %v", err)
	}
}

// ── 端到端：常驻会话 + 追加 + 空闲收工 ────────────────────────

func TestKeepAliveAppendEndToEnd(t *testing.T) {
	dir := shortSessionsDir(t)
	t.Setenv(session.EnvDir, dir)

	eng := kaEngineForTest(t)

	// ① 起常驻会话（后台跑，空闲 1s 自动收工）
	done := make(chan error, 1)
	go func() {
		_, _, err := runAskCmd(t, "", "-e", "codebuddy", "--stream", "--keep-alive",
			"--idle", "1s", "-p", "首轮任务")
		done <- err
	}()

	// ② 等登记表里出现追加入口
	var runID string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, rec := range mustListSessions(t, dir) {
			if rec.Append != "" && rec.State == session.StateRunning {
				runID = rec.RunID
			}
		}
		if runID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("常驻会话没在登记表里开出追加入口")
	}

	// ③ 追加一条（走真实的 unix socket 通道）
	stdout, _, err := runAskCmd(t, "", "--append", runID, "-p", "追加：顺便跑一下测试")
	if err != nil {
		t.Fatalf("--append: %v", err)
	}
	var out struct {
		Type   string `json:"type"`
		Queued bool   `json:"queued"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &out); err != nil {
		t.Fatalf("--append 输出不是 JSON: %v (%s)", err, stdout)
	}
	if out.Type != "append" || !out.Queued {
		t.Errorf("--append 结果不对: %+v", out)
	}

	// ④ 空闲 1s 后应自动收工（不需要 --stop）
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("常驻会话收尾报错: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("空闲超时后常驻会话没收工")
	}

	// ⑤ 引擎确实收到了追加内容
	if got := eng.received(); len(got) != 1 || got[0] != "追加：顺便跑一下测试" {
		t.Errorf("引擎收到的追加 = %v", got)
	}
	// ⑥ 会话记录收尾为 done，socket 文件被清掉
	recs := mustListSessions(t, dir)
	if len(recs) != 1 {
		t.Fatalf("登记表条数 = %d", len(recs))
	}
	if recs[0].State != session.StateDone {
		t.Errorf("会话状态 = %q want done", recs[0].State)
	}
	if _, err := os.Stat(filepath.Join(dir, runID+".sock")); !os.IsNotExist(err) {
		t.Errorf("收工后 socket 文件应被删掉: %v", err)
	}
}

// mustListSessions 直接读登记表（比 --sessions 的 JSON 更直接，且能看到 append 字段）。
func mustListSessions(t *testing.T, dir string) []session.Record {
	t.Helper()
	t.Setenv(session.EnvDir, dir)
	recs, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return recs
}

// --engines 要暴露「能不能追加」这个能力（观物台据此决定是否提供追加入口）。
func TestEnginesFlagCarriesAppendCapability(t *testing.T) {
	kaEngineForTest(t)                        // codebuddy（支持）
	registerFake(&stringEngine{name: "trae"}) // 不支持
	registerFake(&stringEngine{name: "claude"})

	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	var rows []struct {
		Engine string `json:"engine"`
		Append bool   `json:"append"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rows); err != nil {
		t.Fatalf("解析 --engines 输出失败: %v (%s)", err, stdout)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Engine] = r.Append
	}
	if !got["codebuddy"] {
		t.Error("codebuddy 的 append 应为 true")
	}
	if got["trae"] {
		t.Error("trae 的 append 应为 false")
	}
}
