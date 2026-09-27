package agent

// workspace_test.go - 「工作目录（-w/--workspace）」的引擎侧单测。
//
// 覆盖（需求原话：「参数还要支持指定workspace 尽量使用原生cli支持的方式 不支持告诉我」）：
//  ① WorkspaceSupportOf 能力表：codex=flag:-C / claude·codebuddy·trae·openclaw=cwd / llm·arkclaw=none
//  ② 子进程 cwd 真的换了目录（假 CLI 跑 pwd 回读）—— claude/codebuddy/trae/openclaw 的原生方式
//  ③ codex 走原生 `-C/--cd <dir>`；没给 workspace 时不加该 flag
//  ④ 声明为 none 的引擎不会因为 workspace 报错（参数被忽略，提示由 CLI 层负责）

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWorkspaceSupportOfMapping(t *testing.T) {
	cases := map[string]string{
		"codex":        "flag:-C",
		"claude":       "cwd",
		"codebuddy":    "cwd",
		"codebuddy-ai": "cwd",
		"trae":         "cwd",
		"dsh":          "cwd",  // 官方语义：调用时所在目录即默认 workspace 根
		"openclaw":     "none", // 实测子进程 cwd 被忽略（workspace 与 agent 绑定）
		"llm":          "none",
		"arkclaw":      "none",
	}
	for engine, want := range cases {
		if got := WorkspaceSupportOf(engine); got != want {
			t.Errorf("WorkspaceSupportOf(%q) = %q, want %q", engine, got, want)
		}
	}
}

// pwdStub 造一个假 CLI：把 `pwd` 与收到的参数写进 log，再输出一行合法 result JSON。
// （claude 的 Complete 要能解析出正文，故带上 {"type":"result",...}。）
func pwdStub(t *testing.T, dir, name string) (bin, log string) {
	t.Helper()
	log = filepath.Join(dir, "ws.log")
	bin = filepath.Join(dir, name)
	script := "#!/bin/sh\n" +
		"{ pwd; printf 'args: %s\\n' \"$*\"; } > " + log + "\n" +
		`echo '{"type":"result","result":"ok","session_id":"s-1"}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// samePath 比较两个路径是否指向同一目录（macOS 上 /var 与 /private/var 是同一处）。
func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	if ea != nil || eb != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

func TestRunCLIInChangesChildCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX sh/pwd")
	}
	ws := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	bin, log := pwdStub(t, filepath.Dir(ws), "pwd-cli")

	if _, _, err := runCLIIn(context.Background(), ws, bin); err != nil {
		t.Fatalf("runCLIIn: %v", err)
	}
	out, _ := os.ReadFile(log)
	got := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if !samePath(t, got, ws) {
		t.Errorf("子进程 cwd = %q，期望 workspace %q", got, ws)
	}
}

func TestRunCLIInWithoutWorkspaceKeepsCallerCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX sh/pwd")
	}
	bin, log := pwdStub(t, t.TempDir(), "pwd-cli2")
	if _, _, err := runCLIIn(context.Background(), "", bin); err != nil {
		t.Fatalf("runCLIIn: %v", err)
	}
	out, _ := os.ReadFile(log)
	got := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	wd, _ := os.Getwd()
	if !samePath(t, got, wd) {
		t.Errorf("不给 workspace 时应继承调用方 cwd（%q），实得 %q", wd, got)
	}
}

// claude 这类没有工作目录 flag 的引擎：靠子进程 cwd 生效（引擎内部把 req.Workspace 交给 runCLIIn）。
func TestClaudeCompleteRunsInWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX sh/pwd")
	}
	ws := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	bin, log := pwdStub(t, filepath.Dir(ws), "claude")
	e := &ClaudeEngine{BinPath: bin}
	resp, err := e.Complete(context.Background(), Request{
		Workspace: ws,
		Messages:  []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "ok" {
		t.Fatalf("Text = %q want ok", resp.Text)
	}
	out, _ := os.ReadFile(log)
	lines := strings.SplitN(string(out), "\n", 2)
	if len(lines) == 0 || !samePath(t, strings.TrimSpace(lines[0]), ws) {
		t.Errorf("claude 子进程 cwd = %q，期望 %q", strings.TrimSpace(lines[0]), ws)
	}
	// 无原生 flag：参数里不该凭空多出目录类开关
	if strings.Contains(string(out), "--add-dir") {
		t.Errorf("claude 不该自动加 --add-dir（那是追加额外目录，不是主 workspace）: %s", out)
	}
}

func TestCodexWorkspaceUsesNativeCD(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	w, log := argsCaptureCLI(t, dir, "codex", codexStubWithText("ok"))
	e := &CodexEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Workspace: ws,
		Messages:  []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	if args := string(data); !strings.Contains(args, "-C "+ws) {
		t.Errorf("codex 应传原生 -C <workspace>，实得 args: %s", args)
	}
}

func TestCodexOmitsCDWithoutWorkspace(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "codex", codexStubWithText("ok"))
	e := &CodexEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	if args := string(data); strings.Contains(args, "-C ") {
		t.Errorf("未指定 workspace 时不该有 -C：%s", args)
	}
}
