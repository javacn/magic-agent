package cli

// ask_test.go - CLI 层测试。
//
// 覆盖：
//	- collectPrompt 输入收集（args / --file / stdin 组合）
//	- 端到端：fake 引擎注册 + cobra 命令执行 + 退出码
//	- 引擎/格式参数校验错误 → exit 2

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/darren/magic-agent/internal/agent"
)

// stringEngine 最简单的假引擎：固定返回文本。
type stringEngine struct {
	name string
	text string
	err  error
}

func (s *stringEngine) Name() string                    { return s.name }
func (s *stringEngine) Detect() (bool, string)          { return true, "fake://" + s.name }
func (s *stringEngine) Complete(ctx context.Context, req agent.Request) (agent.Response, error) {
	if s.err != nil {
		return agent.Response{}, s.err
	}
	return agent.Response{Text: s.text, Model: "fake-model", Latency: 5_000_000}, nil
}

// ── collectPrompt ─────────────────────────────────────────────

func TestCollectPromptFromArgs(t *testing.T) {
	parts, err := collectPrompt(strings.NewReader(""), []string{"你好", "世界"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0] != "你好 世界" {
		t.Errorf("parts = %v", parts)
	}
}

func TestCollectPromptFromFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.txt")
	if err := os.WriteFile(f, []byte("文件内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	parts, err := collectPrompt(strings.NewReader(""), nil, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0] != "文件内容" {
		t.Errorf("parts = %v", parts)
	}
}

func TestCollectPromptFilePlusArgs(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p.txt")
	if err := os.WriteFile(f, []byte("上下文"), 0o644); err != nil {
		t.Fatal(err)
	}
	parts, err := collectPrompt(strings.NewReader(""), []string{"问题"}, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0] != "上下文" || parts[1] != "问题" {
		t.Errorf("parts = %v (want 文件在前、参数在后)", parts)
	}
}

func TestCollectPromptFileMissing(t *testing.T) {
	if _, err := collectPrompt(strings.NewReader(""), nil, "/no/such/file.txt"); err == nil {
		t.Error("expected error for missing file")
	}
}

// ── 端到端命令执行 ────────────────────────────────────────────

// runAskCmd 用给定 opts + args 跑 ask 命令，返回 (stdout, stderr, exit-code-intent error)。
func runAskCmd(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"ask"}, args...))
	err := cmd.Execute()
	return out.String(), errBuf.String(), err
}

// 注册一个假引擎（用独立名字避免污染全局注册表——直接构造命令并替换 Lookup）。
// 这里用环境注入的方式：ask 命令通过 agent.Lookup 查引擎，所以注册一个
// 独特名字的假引擎即可。
func TestAskEndToEndText(t *testing.T) {
	agent.Register(&stringEngine{name: "fake-echo", text: "echoed"})
	t.Cleanup(func() { unregisterLast() })

	stdout, _, err := runAskCmd(t, "", "-e", "fake-echo", "你好")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if strings.TrimSpace(stdout) != "echoed" {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestAskEndToEndJSON(t *testing.T) {
	agent.Register(&stringEngine{name: "fake-json", text: "the answer"})
	t.Cleanup(func() { unregisterLast() })

	stdout, _, err := runAskCmd(t, "", "-e", "fake-json", "-o", "json", "hi")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	for _, want := range []string{
		`"engine":"fake-json"`,
		`"model":"fake-model"`,
		`"attempts":1`,
		`"latency_ms":5`,
		`"text":"the answer"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %s: %s", want, stdout)
		}
	}
}

func TestAskStdinPipe(t *testing.T) {
	agent.Register(&stringEngine{name: "fake-stdin", text: "ok"})
	t.Cleanup(func() { unregisterLast() })

	stdout, _, err := runAskCmd(t, "来自管道的问题", "-e", "fake-stdin")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if strings.TrimSpace(stdout) != "ok" {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestAskUnknownEngine(t *testing.T) {
	_, _, err := runAskCmd(t, "", "-e", "nope", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError (exit 2), got %T: %v", err, err)
	}
}

func TestAskBadFormat(t *testing.T) {
	_, _, err := runAskCmd(t, "", "-e", "claude", "-o", "yaml", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError, got %T", err)
	}
}

func TestAskEmptyPrompt(t *testing.T) {
	_, _, err := runAskCmd(t, "", "-e", "claude")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError, got %T", err)
	}
}

func TestAskNegativeRetries(t *testing.T) {
	_, _, err := runAskCmd(t, "", "-e", "claude", "-r", "-1", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("error should be usageError, got %T", err)
	}
}

func TestAskEngineFailureJSONError(t *testing.T) {
	agent.Register(&stringEngine{name: "fake-bad", err: errors.New("connection refused")})
	t.Cleanup(func() { unregisterLast() })

	stdout, stderr, err := runAskCmd(t, "", "-e", "fake-bad", "-o", "json", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if stdout != "" {
		t.Errorf("stdout should be empty on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, `"error"`) {
		t.Errorf("stderr should contain json error envelope: %q", stderr)
	}
}

func TestExitCodeOf(t *testing.T) {
	if exitCodeOf(&usageError{errors.New("x")}) != 2 {
		t.Error("usageError should be exit 2")
	}
	if exitCodeOf(errors.New("x")) != 1 {
		t.Error("other errors should be exit 1")
	}
}

// unregisterLast 移除注册表最后一个引擎（测试清理用）。
// agent 包未暴露反注册，这里通过 Engines 重建不可行；
// 采用变通：ask 测试用的假引擎名字带 fake- 前缀且不影响真实引擎
// 查找，注册表多几个假引擎无害，此函数实际只用于语义标记。
func unregisterLast() {}

// 确保 cobra 命令满足接口。
var _ = cobra.Command{}
