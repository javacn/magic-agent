package agent

// attachments_test.go - 「文件＋提示词」（附件）链路的单元测试。
//
// 用户需求原文（2026-09-17）：「传了提示词需要支持传入文件路径」→
// 「理解不对 是文件＋提示词 比如截图加提示词」。
//
// 覆盖：MIME 嗅探 / 能力表 / 兜底路径清单 / stream-json 输入构造（claude、codebuddy）
// / codex -i（含 resume 退化为路径）/ llm 直连 content parts / llm CLI -a /
// arkclaw A2A file part / trae 路径兜底。
//
// 图片用**真 PNG 字节**（Go 现造），不靠扩展名 —— 嗅探走的是魔数，测试要覆盖这一点。

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngChunk 组一个 PNG 块（长度 + 类型 + 数据 + CRC）。
func pngChunk(tag string, data []byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{byte(len(data) >> 24), byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data))})
	b.WriteString(tag)
	b.Write(data)
	crc := crc32.ChecksumIEEE(append([]byte(tag), data...))
	b.Write([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})
	return b.Bytes()
}

// writeTestPNG 现造一张纯红 PNG（8x8 RGB），返回路径。
func writeTestPNG(t *testing.T, name string) string {
	t.Helper()
	w, h := 8, 8
	raw := make([]byte, 0, h*(1+w*3))
	for y := 0; y < h; y++ {
		raw = append(raw, 0) // filter type 0
		for x := 0; x < w; x++ {
			raw = append(raw, 0xff, 0x00, 0x00)
		}
	}
	var idat bytes.Buffer
	zw := zlib.NewWriter(&idat)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	ihdr := []byte{0, 0, 0, byte(w), 0, 0, 0, byte(h), 8, 2, 0, 0, 0}
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, pngChunk("IHDR", ihdr)...)
	png = append(png, pngChunk("IDAT", idat.Bytes())...)
	png = append(png, pngChunk("IEND", nil)...)

	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, png, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeTestFile 写一个任意内容的小文件（非图片附件用）。
func writeTestFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ── 嗅探与能力表 ──────────────────────────────────────────────

func TestNewAttachmentSniffsByMagicBytes(t *testing.T) {
	// 故意用 .dat 后缀：魔数优先，不能被扩展名带偏。
	p := writeTestPNG(t, "shot.dat")
	a := NewAttachment(p)
	if a.MIME != "image/png" {
		t.Errorf("MIME = %q want image/png（魔数优先于扩展名）", a.MIME)
	}
	if !a.IsImage() {
		t.Error("IsImage() 应为 true")
	}
	if a.Name() != "shot.dat" {
		t.Errorf("Name() = %q", a.Name())
	}
}

func TestNewAttachmentFallsBackToExtension(t *testing.T) {
	// 非图片内容 + 认识的扩展名 → 按扩展名兜底；完全不认识 → octet-stream。
	txt := writeTestFile(t, "notes.txt", "hello")
	if got := NewAttachment(txt).MIME; got != "text/plain" {
		t.Errorf("MIME = %q want text/plain", got)
	}
	bin := writeTestFile(t, "blob.xyz", "\x00\x01\x02")
	if got := NewAttachment(bin).MIME; got != "application/octet-stream" {
		t.Errorf("MIME = %q want application/octet-stream", got)
	}
}

func TestAttachmentSupportOf(t *testing.T) {
	want := map[string]string{
		"codex":        "flag:-i",
		"claude":       "stdin:stream-json",
		"codebuddy":    "stdin:stream-json",
		"codebuddy-ai": "stdin:stream-json",
		"llm":          "flag:-a",
		"arkclaw":      "part:file",
		"trae":         "prompt",
		"openclaw":     "prompt",
		"dsh":          "prompt",
	}
	for engine, w := range want {
		if got := AttachmentSupportOf(engine); got != w {
			t.Errorf("AttachmentSupportOf(%q) = %q want %q", engine, got, w)
		}
	}
	// 未知引擎保守兜底
	if got := AttachmentSupportOf("nope"); got != "prompt" {
		t.Errorf("未知引擎应为 prompt, got %q", got)
	}
}

func TestAppendAttachmentSectionFallback(t *testing.T) {
	atts := []Attachment{{Path: "/tmp/a.png", MIME: "image/png"}}

	got := appendAttachmentSection("看看这张图", atts)
	if !strings.Contains(got, "看看这张图") || !strings.Contains(got, "/tmp/a.png") || !strings.Contains(got, "image/png") {
		t.Errorf("兜底文案应含提示词与路径: %q", got)
	}
	// 空附件不动提示词
	if got := appendAttachmentSection("原样", nil); got != "原样" {
		t.Errorf("无附件时应原样返回, got %q", got)
	}
	// 提示词为空时只剩附件清单（不能拼出前导空行）
	if got := appendAttachmentSection("", atts); strings.HasPrefix(got, "\n") {
		t.Errorf("空提示词时不应有前导空行: %q", got)
	}
}

// ── stream-json 输入（claude / codebuddy）──────────────────────

func TestStreamJSONUserLineCarriesImageBlock(t *testing.T) {
	img := writeTestPNG(t, "shot.png")
	txt := writeTestFile(t, "readme.md", "文件里的说明")
	stdin, err := streamJSONUserLine("这张图什么颜色？", []Attachment{
		NewAttachment(img), NewAttachment(txt),
	})
	if err != nil {
		t.Fatalf("streamJSONUserLine: %v", err)
	}
	if !strings.HasSuffix(stdin, "\n") {
		t.Error("应是一整行（尾部换行）")
	}

	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(stdin), &msg); err != nil {
		t.Fatalf("stdin 不是合法 JSON: %v\n%s", err, stdin)
	}
	if msg.Type != "user" || msg.Message.Role != "user" {
		t.Errorf("消息形状不对: type=%q role=%q", msg.Type, msg.Message.Role)
	}
	if len(msg.Message.Content) != 2 {
		t.Fatalf("content 块数 = %d want 2（图 + 文本）", len(msg.Message.Content))
	}
	// 图在前、文本在后
	if msg.Message.Content[0].Type != "image" || msg.Message.Content[0].Source.Type != "base64" {
		t.Errorf("第一块应是 image/base64: %+v", msg.Message.Content[0])
	}
	if msg.Message.Content[0].Source.MediaType != "image/png" {
		t.Errorf("media_type = %q", msg.Message.Content[0].Source.MediaType)
	}
	raw, _ := base64.StdEncoding.DecodeString(msg.Message.Content[0].Source.Data)
	if len(raw) == 0 || !bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G'}) {
		t.Error("image block 的 base64 解回来不是 PNG —— 附件内容没真带上")
	}
	if msg.Message.Content[1].Type != "text" || !strings.Contains(msg.Message.Content[1].Text, "这张图什么颜色？") {
		t.Errorf("第二块应是带提示词的 text: %+v", msg.Message.Content[1])
	}
	// 非图片附件退化为路径文本
	if !strings.Contains(msg.Message.Content[1].Text, "readme.md") {
		t.Errorf("非图片附件应写进文本: %q", msg.Message.Content[1].Text)
	}
}

func TestStreamJSONArgsSwapsPromptForStdin(t *testing.T) {
	base := []string{"-p", "--output-format", "json", "--tools", "", "原始提示词"}
	got := streamJSONArgs(base, "原始提示词", false)

	if strings.Contains(strings.Join(got, " "), "原始提示词") {
		t.Errorf("提示词不能再作为位置参数（改走 stdin）: %v", got)
	}
	if !hasFlagValue(got, "--output-format", "stream-json") {
		t.Errorf("--output-format 应改成 stream-json: %v", got)
	}
	if !hasFlagValue(got, "--input-format", "stream-json") {
		t.Errorf("应追加 --input-format stream-json: %v", got)
	}
	streaming := streamJSONArgs(base, "原始提示词", true)
	if !contains(streaming, "--include-partial-messages") {
		t.Errorf("流式模式应带 --include-partial-messages: %v", streaming)
	}
	if contains(got, "--include-partial-messages") {
		t.Errorf("非流式模式不该带 --include-partial-messages: %v", got)
	}
}

// hasFlagValue 断言 flag 的下一个值等于 want。
func hasFlagValue(args []string, flag, want string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == want {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// countFlag 数某个 flag 在 args 里出现的次数。
func countFlag(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

// claude 带附件：Complete 也必须走 stream-json 输入（提示词 + 图走 stdin），
// 且不向调用方发增量事件。
func TestClaudeCompleteWithAttachmentUsesStdin(t *testing.T) {
	img := writeTestPNG(t, "shot.png")
	// 假 CLI：把 stdin 长度与收到的参数回显进 result 行。
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
n=$(wc -c | tr -d ' ')
echo "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"stdin=$n args=$*\",\"session_id\":\"s-att\",\"model\":\"m\"}"
`)
	e := &ClaudeEngine{BinPath: cli}
	events, onEvent := collectEvents()

	res, err := e.Complete(context.Background(), Request{
		Messages:    []Message{{Role: "user", Content: "这张图什么颜色？"}},
		Attachments: []Attachment{NewAttachment(img)},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(*events) != 0 {
		t.Errorf("Complete 不该发事件, got %d", len(*events))
	}
	_ = onEvent
	if !strings.Contains(res.Text, "--input-format stream-json") {
		t.Errorf("参数里应有 --input-format stream-json: %q", res.Text)
	}
	if !strings.Contains(res.Text, "--output-format stream-json") {
		t.Errorf("参数里应有 --output-format stream-json: %q", res.Text)
	}
	if strings.Contains(res.Text, "这张图什么颜色？") {
		t.Errorf("提示词不该再出现在命令行参数里: %q", res.Text)
	}
	// stdin 至少要有图片 base64 的量级（几 KB 级）；为 0 说明附件没喂进去
	if strings.Contains(res.Text, "stdin=0 ") {
		t.Errorf("stdin 为空 —— 附件没经 stdin 发送: %q", res.Text)
	}
	if res.SessionID != "s-att" {
		t.Errorf("SessionID = %q want s-att", res.SessionID)
	}
}

// claude 流式带附件：增量照常转发，正文以 result 行为准。
func TestClaudeStreamWithAttachment(t *testing.T) {
	img := writeTestPNG(t, "shot.png")
	cli := writeFakeCLI(t, "claude", `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}},"session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"红色"}},"session_id":"s-1"}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"红色","session_id":"s-1","model":"m"}'
`)
	e := &ClaudeEngine{BinPath: cli}
	events, onEvent := collectEvents()

	res, err := e.Stream(context.Background(), Request{
		Messages:    []Message{{Role: "user", Content: "什么颜色"}},
		Attachments: []Attachment{NewAttachment(img)},
	}, onEvent)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Text != "红色" {
		t.Errorf("Text = %q want 红色", res.Text)
	}
	if len(*events) == 0 {
		t.Error("流式应转发增量事件")
	}
}

// ── codex / llm / trae / arkclaw ─────────────────────────────

func TestCodexArgsCarryImages(t *testing.T) {
	e := &CodexEngine{}
	img := writeTestPNG(t, "shot.png")
	txt := writeTestFile(t, "notes.txt", "hi")
	req := Request{
		Messages:    []Message{{Role: "user", Content: "看图说话"}},
		Attachments: []Attachment{NewAttachment(img), NewAttachment(txt)},
	}
	// 调用顺序与 Complete/Stream 一致：先把「没走 -i 的附件」补进提示词，再拼参数。
	prompt := codexPromptWithAttachments("看图说话", req)
	args := e.buildArgs(req, prompt, "", "")

	if !hasFlagValue(args, "-i", img) {
		t.Errorf("图片附件应走原生 -i: %v", args)
	}
	if hasFlagValue(args, "-i", txt) {
		t.Errorf("非图片附件不该给 -i（-i 只收图片）: %v", args)
	}
	if n := countFlag(args, "-i"); n != 1 {
		t.Errorf("-i 出现 %d 次 want 1（只有那张图）: %v", n, args)
	}
	if !strings.Contains(args[len(args)-1], txt) {
		t.Errorf("非图片附件应写进提示词: %q", args[len(args)-1])
	}

	// resume 轮：codex exec resume 不接受 -i → 退化为路径写进提示词
	resumeReq := Request{
		Messages:    []Message{{Role: "user", Content: "接着看"}},
		SessionID:   "sid-1",
		Attachments: []Attachment{NewAttachment(img)},
	}
	resumePrompt := codexPromptWithAttachments("接着看", resumeReq)
	argsResume := e.buildArgs(resumeReq, resumePrompt, "", "")
	if contains(argsResume, "-i") {
		t.Errorf("resume 轮不该注入 -i: %v", argsResume)
	}
	if !strings.Contains(argsResume[len(argsResume)-1], img) {
		t.Errorf("resume 轮附件应退化为提示词里的路径: %q", argsResume[len(argsResume)-1])
	}
}

func TestLLMConfiguredBodyAttachments(t *testing.T) {
	img := writeTestPNG(t, "shot.png")
	body, err := buildConfiguredBody(LLMConfigModel{ID: "m1", URL: "https://x/v1"},
		Request{
			Messages:    []Message{{Role: "user", Content: "什么颜色"}},
			Attachments: []Attachment{NewAttachment(img)},
		}, false)
	if err != nil {
		t.Fatalf("buildConfiguredBody: %v", err)
	}
	msgs, _ := body["messages"].([]map[string]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 数 = %d want 1", len(msgs))
	}
	parts, ok := msgs[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("带附件时 content 应是 content parts 数组, got %T", msgs[0]["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("parts 数 = %d want 2（图 + 文本）", len(parts))
	}
	if parts[0]["type"] != "image_url" {
		t.Errorf("第一块应是 image_url: %+v", parts[0])
	}
	u, _ := parts[0]["image_url"].(map[string]any)
	urlStr, _ := u["url"].(string)
	if !strings.HasPrefix(urlStr, "data:image/png;base64,") {
		t.Errorf("应是 data URL, got %q", urlStr[:min(40, len(urlStr))])
	}
	if parts[1]["type"] != "text" || parts[1]["text"] != "什么颜色" {
		t.Errorf("第二块应是原提示词: %+v", parts[1])
	}
}

func TestLLMCLIArgsUseAttachmentFlag(t *testing.T) {
	img := writeTestPNG(t, "shot.png")
	e := &LLMEngine{}
	args := e.buildArgs(Request{
		Messages:    []Message{{Role: "user", Content: "看图"}},
		Attachments: []Attachment{NewAttachment(img)},
	}, "看图")
	if !hasFlagValue(args, "-a", img) {
		t.Errorf("图片附件应走 llm CLI 原生 -a: %v", args)
	}
}

func TestTraePromptCarriesAttachmentPath(t *testing.T) {
	img := writeTestPNG(t, "shot.png")
	// 假 trae-cli：把收到的参数原样回显（纯文本输出）。
	cli := writeFakeCLI(t, "trae-cli", "#!/bin/sh\necho \"$*\"\n")
	e := &TraeEngine{BinPath: cli}
	res, err := e.Complete(context.Background(), Request{
		Messages:    []Message{{Role: "user", Content: "看图"}},
		Attachments: []Attachment{NewAttachment(img)},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(res.Text, img) {
		t.Errorf("trae 无原生附件通道 → 应把路径写进提示词: %q", res.Text)
	}
}

func TestArkClawSendsFilePart(t *testing.T) {
	img := writeTestPNG(t, "shot.png")
	srv, rec := arkClawTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = w.Write([]byte(arkClawTextEnvelope("看到了红色", "ctx-1")))
	})
	e := &ArkClawEngine{URL: srv.URL, Key: "k", ClawID: "c"}

	res, err := e.Complete(context.Background(), Request{
		Messages:    []Message{{Role: "user", Content: "什么颜色"}},
		Attachments: []Attachment{NewAttachment(img)},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Text != "看到了红色" {
		t.Errorf("Text = %q", res.Text)
	}

	// 请求体里应有 A2A 原生 file part（inline base64）
	var reqBody struct {
		Params struct {
			Message struct {
				Parts []struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
					File *struct {
						Name     string `json:"name"`
						MimeType string `json:"mimeType"`
						Bytes    string `json:"bytes"`
					} `json:"file"`
				} `json:"parts"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &reqBody); err != nil {
		t.Fatalf("请求体不是 JSON: %v\n%s", err, rec.Body)
	}
	parts := reqBody.Params.Message.Parts
	if len(parts) != 2 || parts[0].Kind != "text" || parts[1].Kind != "file" {
		t.Fatalf("parts = %+v want [text file]", parts)
	}
	if parts[1].File == nil || parts[1].File.MimeType != "image/png" {
		t.Fatalf("file part 形状不对: %+v", parts[1].File)
	}
	raw, derr := base64.StdEncoding.DecodeString(parts[1].File.Bytes)
	if derr != nil || !bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G'}) {
		t.Error("file part 的 bytes 不是 PNG base64 —— 附件没真带上")
	}
}
