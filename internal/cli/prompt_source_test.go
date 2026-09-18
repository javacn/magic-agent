package cli

// prompt_source_test.go - 「提示词可以是文件路径」的单元测试。
//
// 用户 2026-09-17 的要求：「传了提示词需要支持传入文件路径」。
// 覆盖：-p / 位置参数 / -s / 配置里的 systemPrompt 四条入口 + @ 强制形式 +
// 保守判据（多行、目录、超长、不存在的路径都按普通文本处理，不能被误判）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darren/magic-agent/internal/config"
)

// writePromptFile 在临时目录写一个提示词文件，返回其路径。
func writePromptFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ── expandPromptSource 单测 ──────────────────────────────────

func TestExpandPromptSourcePlainText(t *testing.T) {
	// 普通提示词原样返回，且不应产生提示（否则每次调用都刷一行 stderr）。
	for _, s := range []string{"你好", "解释一下 README.md", "多行\n提示词\n不受影响", ""} {
		text, note, err := expandPromptSource(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if text != s || note != "" {
			t.Errorf("%q → text=%q note=%q（应原样、无提示）", s, text, note)
		}
	}
}

func TestExpandPromptSourceExistingFile(t *testing.T) {
	f := writePromptFile(t, "p.md", "  文件里的提示词\n")
	text, note, err := expandPromptSource(f)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if text != "文件里的提示词" {
		t.Errorf("text = %q want 文件内容（去首尾空白）", text)
	}
	if !strings.Contains(note, "命中文件") || !strings.Contains(note, f) {
		t.Errorf("note 应说明命中文件并给出路径, got %q", note)
	}
}

func TestExpandPromptSourceForcedAt(t *testing.T) {
	f := writePromptFile(t, "sys.md", "你是审稿人")
	text, note, err := expandPromptSource("@" + f)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if text != "你是审稿人" || note == "" {
		t.Errorf("text = %q note = %q", text, note)
	}

	// @ 后为空 / 文件不存在 / 是目录 → 报错（强制语义下不猜测）
	if _, _, err := expandPromptSource("@"); err == nil {
		t.Error("@ 空路径应报错")
	}
	if _, _, err := expandPromptSource("@/no/such/file.md"); err == nil {
		t.Error("不存在的文件（强制）应报错")
	}
	if _, _, err := expandPromptSource("@" + t.TempDir()); err == nil {
		t.Error("目录（强制）应报错")
	}
}

func TestExpandPromptSourceConservativeGuards(t *testing.T) {
	// 目录：非强制时按普通文本处理（不能把 -p <目录> 读崩）
	dir := t.TempDir()
	if text, note, err := expandPromptSource(dir); err != nil || text != dir || note != "" {
		t.Errorf("目录应原样当文本: text=%q note=%q err=%v", text, note, err)
	}

	// 不存在的路径：原样当文本
	if text, note, err := expandPromptSource("/no/such/file.md"); err != nil || text != "/no/such/file.md" || note != "" {
		t.Errorf("不存在的路径应原样: text=%q note=%q err=%v", text, note, err)
	}

	// 多行文本即使含真实文件路径也不识别（按正文用）
	f := writePromptFile(t, "p.md", "内容")
	multi := "看看这个文件\n" + f
	if text, note, err := expandPromptSource(multi); err != nil || text != multi || note != "" {
		t.Errorf("多行应原样: text=%q note=%q err=%v", text, note, err)
	}

	// 超长单行也不例外（超过 maxPromptPathLen 直接按文本）
	long := f + strings.Repeat("x", maxPromptPathLen)
	if text, note, err := expandPromptSource(long); err != nil || text != long || note != "" {
		t.Errorf("超长应原样: note=%q err=%v", note, err)
	}
}

// ── 端到端：四条入口 ────────────────────────────────────────

// -p 给文件路径 → 用文件内容（并向 stderr 说明）。
func TestPromptFlagAcceptsFilePath(t *testing.T) {
	capEng := &capturingEngine{name: "fake-pfile"}
	registerFake(capEng)
	f := writePromptFile(t, "ask.md", "总结这份文档")

	stdout, stderr, err := runAskCmd(t, "", "-e", "fake-pfile", "-p", f)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if capEng.lastPrompt != "总结这份文档" {
		t.Errorf("prompt = %q want 文件内容", capEng.lastPrompt)
	}
	if !strings.Contains(stderr, "命中文件") {
		t.Errorf("stderr 应说明按文件读取, got %q", stderr)
	}
	if strings.Contains(stdout, "命中文件") {
		t.Errorf("提示不该污染 stdout, got %q", stdout)
	}
}

// -p 给文件路径 + 追加文本：文件内容在前、追加在后（与 -f 语义一致）。
func TestPromptFlagFilePlusText(t *testing.T) {
	capEng := &capturingEngine{name: "fake-pfile2"}
	registerFake(capEng)
	f := writePromptFile(t, "ask.md", "上下文内容")

	if _, _, err := runAskCmd(t, "", "-e", "fake-pfile2", "-p", f, "补充一句"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if want := "上下文内容\n\n补充一句"; capEng.lastPrompt != want {
		t.Errorf("prompt = %q want %q", capEng.lastPrompt, want)
	}
}

// 位置参数给文件路径 → 同样按文件读。
func TestPositionalArgAcceptsFilePath(t *testing.T) {
	capEng := &capturingEngine{name: "fake-argfile"}
	registerFake(capEng)
	f := writePromptFile(t, "task.md", "任务描述")

	if _, _, err := runAskCmd(t, "", "-e", "fake-argfile", f); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if capEng.lastPrompt != "任务描述" {
		t.Errorf("prompt = %q want 文件内容", capEng.lastPrompt)
	}
}

// -p @不存在的文件 → usage error（exit 2），不猜测、不静默当文本。
func TestPromptFlagForcedAtMissingIsUsageError(t *testing.T) {
	registerFake(&stringEngine{name: "fake-atmiss", text: "x"})

	_, _, err := runAskCmd(t, "", "-e", "fake-atmiss", "-p", "@/no/such/prompt.md")
	if err == nil {
		t.Fatal("应报错")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("应是 usageError(exit 2), got %T: %v", err, err)
	}
}

// -s 给文件路径 → 用文件内容作为 system prompt。
func TestSystemFlagAcceptsFilePath(t *testing.T) {
	capEng := &capturingEngine{name: "fake-sfile"}
	registerFake(capEng)
	f := writePromptFile(t, "reviewer.md", "你是严格的审稿人")

	_, stderr, err := runAskCmd(t, "", "-e", "fake-sfile", "-s", f, "看这段")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if capEng.lastSystem != "你是严格的审稿人" {
		t.Errorf("SystemPrompt = %q want 文件内容", capEng.lastSystem)
	}
	if !strings.Contains(stderr, "命中文件") {
		t.Errorf("stderr 应说明按文件读取, got %q", stderr)
	}
}

// 配置里的默认 systemPrompt 指向文件 → 也按文件读；@ 不存在的文件 → 不注入（不阻断调用）。
func TestConfigSystemPromptAcceptsFilePath(t *testing.T) {
	capEng := &capturingEngine{name: "fake-cfgsys"}
	registerFake(capEng)
	f := writePromptFile(t, "cn.md", "你是一个中文助手，始终用中文回答所有问题。")

	// ① 值就是文件路径（自动识别）
	t.Setenv(config.EnvPath, writeCLIConfig(t, `{"systemPrompt":`+jsonQuote(t, f)+`}`))
	if _, _, err := runAskCmd(t, "", "-e", "fake-cfgsys", "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if capEng.lastSystem != "你是一个中文助手，始终用中文回答所有问题。" {
		t.Errorf("SystemPrompt = %q want 文件内容", capEng.lastSystem)
	}

	// ② @ 强制形式指向不存在的文件 → 本次不注入，但调用照常成功
	capEng.lastSystem = ""
	t.Setenv(config.EnvPath, writeCLIConfig(t, `{"systemPrompt":"@/no/such/system.md"}`))
	if _, _, err := runAskCmd(t, "", "-e", "fake-cfgsys", "hi"); err != nil {
		t.Fatalf("坏 @ 路径不应让调用失败: %v", err)
	}
	if capEng.lastSystem != "" {
		t.Errorf("应不注入, got %q", capEng.lastSystem)
	}
}

// jsonQuote 把路径安全地塞进测试用 JSON 字符串（路径可能含中文/反斜杠）。
func jsonQuote(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
