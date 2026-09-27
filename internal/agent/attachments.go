package agent

// attachments.go - 「文件＋提示词」的公共件：附件类型、MIME 嗅探、能力表、兜底文案。
//
// 用户需求原文（2026-09-17）：
//
//	「传了提示词需要支持传入文件路径」→ 更正为「理解不对 是文件＋提示词 比如截图加提示词」
//
// 也就是：提示词**照旧是提示词**，附件（截图等）作为并列的第二份输入一起送进去。
// 本文件只放公共逻辑；各引擎怎么把附件交给 CLI 见 engine.go 的 Request.Attachments 注释。
//
// 能力表（AttachmentSupportOf）是**机器可读**的，`--engines` 每行会带上它，
// 调用方（如观物台）据此决定「要不要发附件 / 要不要提示用户」：
//
//	flag:-i            codex 原生 --image
//	stdin:stream-json  claude/codebuddy 原生 stream-json input（图片走 content block）
//	flag:-a            llm CLI 原生 --attachment
//	part:file          arkclaw 走 A2A 原生 file part（base64 inline）
//	prompt             无原生输入通道：把绝对路径写进提示词，靠引擎的读文件工具看
//	                   （trae / openclaw / dsh —— 需要工具可用，CLI 层会提示）
//	                   codebuddy-gateway 也归这一档：它走 payload.attachments 原生字段，
//	                   但网关把它渲染成路径清单塞进 prompt（图不进模型上下文）

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Attachment 一个随提示词一起发送的附件。
type Attachment struct {
	// Path 附件绝对路径（CLI 层校验过「存在 + 是普通文件」并转成绝对路径）。
	Path string `json:"path"`
	// MIME 嗅探到的媒体类型（按魔数优先，其次扩展名；识别不出为空串）。
	MIME string `json:"mime,omitempty"`
}

// Name 附件文件名（提示词里展示用）。
func (a Attachment) Name() string { return filepath.Base(a.Path) }

// IsImage 是否是图片（MIME 前缀判定）。
func (a Attachment) IsImage() bool { return strings.HasPrefix(a.MIME, "image/") }

// attachmentMagic 按魔数嗅探：只认图片这几类（与 magic-test 的 media-store 同源判据）。
// 不信任扩展名 —— 它由调用方随便填。
var attachmentMagic = []struct {
	mime string
	test func(b []byte) bool
}{
	{"image/png", func(b []byte) bool { return len(b) > 8 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G' }},
	{"image/jpeg", func(b []byte) bool { return len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF }},
	{"image/gif", func(b []byte) bool { return len(b) > 6 && string(b[:4]) == "GIF8" }},
	{"image/webp", func(b []byte) bool {
		return len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
	}},
	{"image/bmp", func(b []byte) bool { return len(b) > 2 && b[0] == 'B' && b[1] == 'M' }},
	// PDF 也认一下：部分引擎（A2A file part / llm 附件）对它有明确支持。
	{"application/pdf", func(b []byte) bool { return len(b) > 4 && string(b[:4]) == "%PDF" }},
}

// sniffAttachment 读文件头判类型；魔数不认时按扩展名兜底，最后给
// application/octet-stream（不返回错误 —— 附件类型识别不了不该阻断调用）。
func sniffAttachment(path string) string {
	if f, err := os.Open(path); err == nil {
		head := make([]byte, 512)
		n, _ := f.Read(head)
		f.Close()
		for _, m := range attachmentMagic {
			if m.test(head[:n]) {
				return m.mime
			}
		}
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".pdf":
		return "application/pdf"
	case ".txt", ".md", ".json", ".csv", ".log", ".yaml", ".yml", ".xml", ".html":
		return "text/plain"
	}
	return "application/octet-stream"
}

// NewAttachment 构造附件：嗅探 MIME（路径不存在也不报错 —— 校验在 CLI 层做）。
func NewAttachment(path string) Attachment {
	return Attachment{Path: path, MIME: sniffAttachment(path)}
}

// AttachmentSupportOf 返回该引擎「收附件」的落地方式（见文件头的能力表）。
// 未知引擎一律 "prompt"（最保守的兜底：把路径写进提示词）。
func AttachmentSupportOf(engine string) string {
	// 具名 agent（如 MagicAI）先归到它协议的家族名，再查表 —— 见 CapabilityFamilyOf。
	engine = CapabilityFamilyOf(engine)
	switch engine {
	case "codex":
		return "flag:-i"
	case "claude", "codebuddy", "codebuddy-ai":
		return "stdin:stream-json"
	case "llm":
		return "flag:-a"
	case "arkclaw":
		return "part:file"
	case "trae", "openclaw", "dsh", "codebuddy-gateway":
		// codebuddy-gateway 走 payload.attachments 原生字段（urlType=local-path），
		// 但网关把它渲染成「附件清单 + 路径」塞进 prompt —— 图**不会**以图片形式
		// 进模型上下文，故如实报 prompt 而不是 part:file。
		return "prompt"
	}
	return "prompt"
}

// AppendSupportOf 报告该引擎是否支持「常驻会话 + 追加消息」（Request.Append）。
//
//	claude / codebuddy / codebuddy-ai  --input-format stream-json 能持续从 stdin 收 user 消息
//	dsh                              SDK 通道下对同一 sessionId 连续 session/prompt 即可续接
//	                                 （官方 Python SDK 文档：「reuse a harness, home, and id」）
//
// 其余引擎要么一次性（trae/llm），要么走协议级 follow-up 但不在本次范围（codex queue /
// openclaw agent --session-id / arkclaw A2A taskId）。
//
// ⚠️ dsh 的这条能力**依赖 SDK 通道**：显式 Profile/环境变量把通道切成 headless 后，
// 追加没有落地通道 —— 引擎会明确报错而不是静默丢消息（见 dsh.go 的回退分支）。
func AppendSupportOf(engine string) bool {
	// 具名 agent（如 MagicAI）先归到它协议的家族名，再查表 —— 见 CapabilityFamilyOf。
	engine = CapabilityFamilyOf(engine)
	switch engine {
	case "claude", "codebuddy", "codebuddy-ai", "dsh":
		return true
	}
	return false
}

// AppendDefaultOn 报告「常驻会话」在该引擎上是否**默认开**（--keep-alive 的默认值语义）。
//
// claude / codebuddy 默认开（用户 2026-09-18 的要求：「keep-alive 要是默认的」）。
// dsh 默认**关**、需要显式 `--keep-alive`：它的 SDK 通道本来就是「一次调用一轮」的形态，
// 默认挂一个 5 分钟空闲窗口会让既有调用方（脚本 / 观物台）以为命令卡住了。
func AppendDefaultOn(engine string) bool {
	// 具名 agent 先归到它协议的家族名，再查表 —— 见 CapabilityFamilyOf。
	engine = CapabilityFamilyOf(engine)
	switch engine {
	case "claude", "codebuddy", "codebuddy-ai":
		return true
	}
	return false
}

// AttachmentPromptSection 把附件以「路径清单」的形式拼进提示词正文。
//
// 这是**没有原生附件输入通道**的引擎（trae / openclaw / dsh）的兜底：把绝对路径说给模型，
// 由它自己的读文件工具去看。因此只有在工具可用时才有意义 —— CLI 层会在
// --tools off 时给出明确提示（不静默丢附件）。
func AttachmentPromptSection(atts []Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("【附件】以下文件已随本次请求提供，请用你的文件读取工具查看后再回答：\n")
	for _, a := range atts {
		fmt.Fprintf(&sb, "- %s", a.Path)
		if a.MIME != "" {
			fmt.Fprintf(&sb, "（%s）", a.MIME)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// appendAttachmentSection 把附件清单追加到 prompt 末尾（空附件原样返回）。
func appendAttachmentSection(prompt string, atts []Attachment) string {
	section := AttachmentPromptSection(atts)
	if section == "" {
		return prompt
	}
	if strings.TrimSpace(prompt) == "" {
		return strings.TrimSpace(section)
	}
	return strings.TrimSpace(prompt) + "\n\n" + section
}

// readAttachmentBase64 读附件并做 base64（llm 直连 / A2A file part 共用）。
// 读不动直接报错 —— 附件是调用方点名要发的东西，静默丢掉比失败更糟。
func readAttachmentBase64(a Attachment) (string, error) {
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", fmt.Errorf("读取附件 %s: %w", a.Path, err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}
