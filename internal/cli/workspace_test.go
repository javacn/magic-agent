package cli

// workspace_test.go - `-w/--workspace` 的 CLI 侧单测。
//
// 覆盖：
//  ① 值解析：展开 ~、转绝对路径、必须存在且是目录（不存在/是文件 → 参数错误 exit 2 语义）
//  ② 透传：进入 Request.Workspace（假引擎回收）
//  ③ 不支持要说明白：llm / arkclaw 给出 stderr 提示文案，其余引擎不提示
//  ④ --engines 每行带 workspace 能力字段（机器可读）

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darren/magic-agent/internal/agent"
)

// wsCaptureEngine 记录收到的 Request.Workspace。
type wsCaptureEngine struct {
	name string
	got  string
}

func (e *wsCaptureEngine) Name() string           { return e.name }
func (e *wsCaptureEngine) Detect() (bool, string) { return true, "fake://" + e.name }
func (e *wsCaptureEngine) Complete(_ context.Context, req agent.Request) (agent.Response, error) {
	e.got = req.Workspace
	return agent.Response{Text: "ok", Model: req.Model}, nil
}

func TestWorkspaceFlagPassedToEngine(t *testing.T) {
	ws := t.TempDir()
	e := &wsCaptureEngine{name: "ws-echo"}
	registerFake(e)

	stdout, stderr, err := runAskCmd(t, "", "-e", "ws-echo", "-w", ws, "-o", "text", "hi")
	if err != nil {
		t.Fatalf("ask: %v (stderr=%s)", err, stderr)
	}
	if !strings.Contains(stdout, "ok") {
		t.Errorf("stdout = %q", stdout)
	}
	// 路径会转绝对 + 规整（t.TempDir 在 macOS 上是 /var → /private/var 的软链）
	want, _ := filepath.EvalSymlinks(ws)
	got, _ := filepath.EvalSymlinks(e.got)
	if got != want {
		t.Errorf("Request.Workspace = %q, want %q", got, want)
	}
	// 支持的引擎不该有「不支持」提示
	if strings.Contains(stderr, "不支持指定工作目录") {
		t.Errorf("ws-echo 不该被提示不支持：%s", stderr)
	}
}

func TestWorkspaceFlagExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if _, err := resolveWorkspaceDir("~"); err != nil {
		t.Fatalf(`resolveWorkspaceDir("~") = %v`, err)
	}
	got, err := resolveWorkspaceDir("~")
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(home); func() bool { g, _ := filepath.EvalSymlinks(got); return g != want }() {
		t.Errorf(`resolveWorkspaceDir("~") = %q, want %q`, got, home)
	}
}

func TestWorkspaceFlagRejectsBadPath(t *testing.T) {
	registerFake(&wsCaptureEngine{name: "ws-echo-bad"})

	// 不存在
	_, _, err := runAskCmd(t, "", "-e", "ws-echo-bad", "-w", filepath.Join(t.TempDir(), "nope"), "hi")
	if err == nil || !strings.Contains(err.Error(), "--workspace") {
		t.Errorf("不存在的目录应报参数错误并点明 --workspace，实得 %v", err)
	}

	// 是文件不是目录
	f := filepath.Join(t.TempDir(), "afile")
	if werr := os.WriteFile(f, []byte("x"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	_, _, err = runAskCmd(t, "", "-e", "ws-echo-bad", "-w", f, "hi")
	if err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Errorf("指向文件应报「不是目录」，实得 %v", err)
	}
}

func TestWorkspaceUnsupportedWarnText(t *testing.T) {
	for _, engine := range []string{"llm", "arkclaw", "openclaw"} {
		warn := workspaceUnsupportedWarn(engine)
		if !strings.Contains(warn, engine) || !strings.Contains(warn, "-w/--workspace 已忽略") {
			t.Errorf("workspaceUnsupportedWarn(%q) = %q，应说明被忽略", engine, warn)
		}
	}
	for _, engine := range []string{"claude", "codebuddy", "trae", "codex"} {
		if warn := workspaceUnsupportedWarn(engine); warn != "" {
			t.Errorf("workspaceUnsupportedWarn(%q) = %q，支持 workspace 的引擎不该提示", engine, warn)
		}
	}
}

func TestEnginesFlagCarriesWorkspaceCapability(t *testing.T) {
	// ⚠️ 测试进程里的引擎注册表是**假的**：agent.Engines() 只在 registry 为空时才
	// initEngines()，而本包测试先 Register 了假引擎 → 真实引擎不会出现。故这里按名字
	// 注册几个具有代表性的假引擎（codex=flag:-C / llm=arkclaw=none / claude=cwd）。
	registerFake(&wsCaptureEngine{name: "codex"})
	registerFake(&wsCaptureEngine{name: "llm"})
	registerFake(&wsCaptureEngine{name: "arkclaw"})
	registerFake(&wsCaptureEngine{name: "claude"})
	registerFake(&wsCaptureEngine{name: "ws-cap-unlisted"})

	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	var rows []struct {
		Engine    string `json:"engine"`
		Workspace string `json:"workspace"`
		Streaming bool   `json:"streaming"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rows); err != nil {
		t.Fatalf("解析 --engines 输出失败: %v (%s)", err, stdout)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Engine] = r.Workspace
	}
	for engine, want := range map[string]string{
		"codex":           "flag:-C",
		"llm":             "none",
		"arkclaw":         "none",
		"claude":          "cwd",
		"ws-cap-unlisted": "cwd", // 未识别的引擎按默认（子进程 cwd）处理
	} {
		if got[engine] != want {
			t.Errorf("--engines 里 %s 的 workspace = %q, want %q", engine, got[engine], want)
		}
	}
	// streaming：假引擎只实现 Complete → 一律 false；真实 codex 引擎实现了 Stream → true
	streaming := map[string]bool{}
	for _, r := range rows {
		streaming[r.Engine] = r.Streaming
	}
	if streaming["ws-cap-unlisted"] {
		t.Error("只实现 Complete 的假引擎 streaming 应为 false")
	}
	registerFake(&fakeStreamEngine{name: "stream-cap"})
	stdout2, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines(2): %v", err)
	}
	var rows2 []struct {
		Engine    string `json:"engine"`
		Streaming bool   `json:"streaming"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout2)), &rows2); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, r := range rows2 {
		if r.Engine == "stream-cap" && !r.Streaming {
			t.Error("实现了 Streamer 的引擎 streaming 应为 true")
		}
	}
}

// fakeStreamEngine 同时实现 Engine + Streamer（用于 streaming 能力字段断言）。
type fakeStreamEngine struct {
	name string
}

func (e *fakeStreamEngine) Name() string           { return e.name }
func (e *fakeStreamEngine) Detect() (bool, string) { return true, "fake://" + e.name }
func (e *fakeStreamEngine) Complete(_ context.Context, _ agent.Request) (agent.Response, error) {
	return agent.Response{Text: "ok"}, nil
}
func (e *fakeStreamEngine) Stream(_ context.Context, _ agent.Request, _ func(agent.StreamEvent)) (agent.StreamResult, error) {
	return agent.StreamResult{Response: agent.Response{Text: "ok"}}, nil
}
