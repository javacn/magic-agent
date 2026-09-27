package cli

// attach_test.go - 「文件＋提示词」（-a/--attach 附件）的 CLI 层测试。
//
// 用户需求原文（2026-09-17）：「理解不对 是文件＋提示词 比如截图加提示词」。
//
// 覆盖：-a 的路径解析与校验（缺失 / 目录 / 逗号 / 去重 / ~ 展开）、附件透传到 Request、
// 无原生附件通道的引擎（trae / openclaw）降级为「路径写进提示词」时的 stderr 提示、
// 以及 `--engines` 的 attachments 能力字段。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAttachFile 写一个测试文件（内容随意 —— MIME 由魔数/扩展名嗅探，这里只关心路径链路）。
func writeAttachFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ── resolveAttachments ───────────────────────────────────────

func TestResolveAttachmentsSplitsAndDedupes(t *testing.T) {
	a := writeAttachFile(t, "a.png", "\x89PNG\r\n\x1a\n")
	b := writeAttachFile(t, "b.md", "hi")

	got, err := resolveAttachments([]string{a + "," + b, a}) // 逗号 + 重复
	if err != nil {
		t.Fatalf("resolveAttachments: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("附件数 = %d want 2（去重后）: %+v", len(got), got)
	}
	if got[0].Path != a || got[1].Path != b {
		t.Errorf("路径应转绝对且保持顺序: %+v", got)
	}
	if got[0].MIME != "image/png" {
		t.Errorf("a.png 的 MIME = %q want image/png（魔数）", got[0].MIME)
	}
	if got[1].MIME != "text/plain" {
		t.Errorf("b.md 的 MIME = %q want text/plain", got[1].MIME)
	}
}

func TestResolveAttachmentsErrors(t *testing.T) {
	if _, err := resolveAttachments([]string{"/no/such/shot.png"}); err == nil {
		t.Error("不存在的附件应报错")
	}
	if _, err := resolveAttachments([]string{t.TempDir()}); err == nil {
		t.Error("目录应报错（不是普通文件）")
	}
	// 全空白 → 视作没有附件（不报错）
	got, err := resolveAttachments([]string{"  ", ","})
	if err != nil || len(got) != 0 {
		t.Errorf("空白输入应得到 0 个附件: %+v err=%v", got, err)
	}
}

// ── -a/--attach 透传 ─────────────────────────────────────────

// 有原生附件通道的引擎（claude）：附件进 Request，且不打降级警告。
func TestAttachFlagReachesRequest(t *testing.T) {
	capEng := &capturingEngine{name: "claude"}
	registerFake(capEng)
	img := writeAttachFile(t, "shot.png", "\x89PNG\r\n\x1a\n")

	_, stderr, err := runAskCmd(t, "", "-e", "claude", "-p", "这张图什么颜色", "-a", img)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if len(capEng.lastAttach) != 1 {
		t.Fatalf("附件数 = %d want 1", len(capEng.lastAttach))
	}
	if capEng.lastAttach[0].Path != img {
		t.Errorf("附件路径 = %q want %q", capEng.lastAttach[0].Path, img)
	}
	if capEng.lastAttach[0].MIME != "image/png" {
		t.Errorf("附件 MIME = %q", capEng.lastAttach[0].MIME)
	}
	if strings.Contains(stderr, "没有附件输入通道") {
		t.Errorf("claude 有原生通道，不该打降级警告: %q", stderr)
	}
	// 提示词本身不受影响
	if capEng.lastPrompt != "这张图什么颜色" {
		t.Errorf("prompt = %q", capEng.lastPrompt)
	}
}

// 多个 -a 与逗号分隔等价。
func TestAttachFlagMultiple(t *testing.T) {
	// 注意：不能用 "claude" 这个名字 —— 上一个用例已注册同名假引擎，
	// Lookup 取注册表里首个匹配 → 断言会打到别的实例上（我第一版就这么挂的）。
	capEng := &capturingEngine{name: "fake-attmulti"}
	registerFake(capEng)
	a := writeAttachFile(t, "1.png", "\x89PNG\r\n\x1a\n")
	b := writeAttachFile(t, "2.png", "\x89PNG\r\n\x1a\n")
	c := writeAttachFile(t, "3.png", "\x89PNG\r\n\x1a\n")

	// -a 可重复，也接受逗号分隔
	_, _, err := runAskCmd(t, "", "-e", "fake-attmulti", "-p", "看图", "-a", a, "-a", b+","+c)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if len(capEng.lastAttach) != 3 {
		t.Errorf("附件数 = %d want 3（-a 重复 + 逗号分隔）", len(capEng.lastAttach))
	}
}

// 附件路径错误 → usage error（exit 2），不静默忽略。
func TestAttachFlagBadPathIsUsageError(t *testing.T) {
	registerFake(&stringEngine{name: "fake-attbad", text: "x"})

	_, _, err := runAskCmd(t, "", "-e", "fake-attbad", "-p", "hi", "-a", "/no/such/shot.png")
	if err == nil {
		t.Fatal("应报错")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("应是 usageError(exit 2), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "--attach") {
		t.Errorf("错误信息应点明 --attach: %v", err)
	}
}

// ── 无原生通道引擎的降级提示 ─────────────────────────────────

func TestAttachmentPromptFallbackWarnUnit(t *testing.T) {
	// 有原生通道 → 无提示
	for _, e := range []string{"claude", "codebuddy", "codebuddy-ai", "codex", "llm", "arkclaw"} {
		if got := attachmentPromptFallbackWarn(e, 1, true); got != "" {
			t.Errorf("%s 有原生通道，不该提示: %q", e, got)
		}
	}
	// 无附件 → 无提示
	if got := attachmentPromptFallbackWarn("trae", 0, true); got != "" {
		t.Errorf("无附件不该提示: %q", got)
	}
	// trae + 工具开 → 提示但不提 --tools
	got := attachmentPromptFallbackWarn("trae", 2, false)
	if !strings.Contains(got, "trae") || !strings.Contains(got, "2 个附件") || strings.Contains(got, "--tools off") {
		t.Errorf("trae/工具开 的提示不对: %q", got)
	}
	// trae + 工具关 → 必须点明读不到
	gotOff := attachmentPromptFallbackWarn("trae", 1, true)
	if !strings.Contains(gotOff, "--tools off") || !strings.Contains(gotOff, "--tools on") {
		t.Errorf("工具关时必须点明：%q", gotOff)
	}
	if !strings.Contains(gotOff, "--engines") {
		t.Errorf("提示应指引到 --engines 的 attachments 字段: %q", gotOff)
	}
	// dsh / openclaw：同样无原生通道，但 --tools 在它们身上没有落地通道
	//（自带工具循环）→ 提示里**不该**出现「请改用 --tools on」（传了也不改变行为）。
	for _, e := range []string{"dsh", "openclaw"} {
		got := attachmentPromptFallbackWarn(e, 1, true)
		if !strings.Contains(got, e) || !strings.Contains(got, "没有附件输入通道") {
			t.Errorf("%s 应提示降级: %q", e, got)
		}
		if strings.Contains(got, "--tools on") {
			t.Errorf("%s 的 --tools 无落地通道，不该提 --tools on: %q", e, got)
		}
	}
}

// 端到端：trae（无原生通道）带附件 + 默认 --tools off → stderr 出现降级提示。
func TestAttachFlagWarnsOnPromptFallbackEngine(t *testing.T) {
	registerFake(&stringEngine{name: "trae", text: "ok"})
	img := writeAttachFile(t, "shot.png", "\x89PNG\r\n\x1a\n")

	_, stderr, err := runAskCmd(t, "", "-e", "trae", "-p", "看图", "-a", img)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !strings.Contains(stderr, "没有附件输入通道") || !strings.Contains(stderr, "--tools off") {
		t.Errorf("stderr 应给出降级提示, got %q", stderr)
	}

	// --tools on → 只提示降级，不再提读不到
	_, stderrOn, err := runAskCmd(t, "", "-e", "trae", "-p", "看图", "--tools", "on", "-a", img)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !strings.Contains(stderrOn, "没有附件输入通道") {
		t.Errorf("仍应提示降级, got %q", stderrOn)
	}
	if strings.Contains(stderrOn, "--tools off") {
		t.Errorf("工具已开，不该再提 --tools off: %q", stderrOn)
	}
}

// ── --engines 的 attachments 字段 ────────────────────────────

func TestEnginesFlagCarriesAttachmentsCapability(t *testing.T) {
	registerFake(&stringEngine{name: "codex"})
	registerFake(&stringEngine{name: "claude"})
	registerFake(&stringEngine{name: "arkclaw"})
	registerFake(&stringEngine{name: "trae"})
	registerFake(&stringEngine{name: "dsh"})

	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	var rows []struct {
		Engine      string `json:"engine"`
		Attachments string `json:"attachments"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rows); err != nil {
		t.Fatalf("解析 --engines 输出失败: %v (%s)", err, stdout)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Engine] = r.Attachments
	}
	for engine, want := range map[string]string{
		"codex":   "flag:-i",
		"claude":  "stdin:stream-json",
		"arkclaw": "part:file",
		"trae":    "prompt",
		"dsh":     "prompt", // headless 无附件参数 → 路径写进提示词
	} {
		if got[engine] != want {
			t.Errorf("--engines 里 %s 的 attachments = %q want %q", engine, got[engine], want)
		}
	}
	// 每条都必须有该字段（omitempty 也不能漏 —— 调用方据此决定要不要发附件）
	for _, r := range rows {
		if r.Attachments == "" {
			t.Errorf("%s 缺 attachments 字段", r.Engine)
		}
	}
}
