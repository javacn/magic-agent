package agent

// streamjson.go - claude / codebuddy 共用的 stream-json 输入输出（附件走这条路）。
//
// 背景（2026-09-17 实测，claude / codebuddy 两家 CLI 同族）：
//
//	--input-format stream-json  可以喂「提示词 + 图片 content block」的 user 消息
//	                            （附件唯一的原生输入通道；两家的 -p 文本模式收不了图）
//	但它要求 --output-format 也必须是 stream-json，所以收尾正文要从
//	NDJSON 的 result 行里取 —— 于是非流式（Complete）在**有附件时**也得跑这条协议。
//
// 本文件把「喂 stdin + 归约 NDJSON」抽成一份，claude/codebuddy 各自复用，
// 免得两家各写一套解析（历史上两份实现漂移过一次，见 magic-test 的 agent-cli 注释）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// streamJSONResult 两家 CLI 收尾行（type=result）的公共形状。
type streamJSONResult struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
}

// runStreamJSONIn 跑一次 stream-json 调用：增量喂给 acc，收尾行填进 fin。
// 返回是否见到了 result 行（未见 = 协议没跑完，调用方按错误处理）。
//
// stdin 为 nil 时不喂 stdin（提示词走命令行位置参数）。每见到一次 result 行，
// 除填 fin 外还会向调用方发一个 KindTurnEnd 事件 —— 常驻会话（Request.Append）
// 靠它判断「这轮做完了，可以继续追加」。
func runStreamJSONIn(
	ctx context.Context, dir string, extraEnv []string, bin string, args []string,
	stdin io.Reader, acc *streamAccumulator, fin *streamJSONResult,
) (bool, error) {
	seen := false
	err := runStreamCLIStdinReaderIn(ctx, dir, extraEnv, bin, args, stdin, func(line string) error {
		isResult, perr := acc.handleNDJSONLine(line)
		if perr != nil {
			return perr
		}
		if isResult {
			_ = json.Unmarshal([]byte(line), fin)
			seen = true
			// 轮次边界事件（Text = 该轮最终正文，SessionID = 该轮会话 id）。
			// ⚠️ 它必须带 session_id：常驻会话的最终 result 信封要等会话结束才输出
			//（默认空闲 5m），调用方要靠这一条把会话锚点绑上（见 StreamEvent.SessionID）。
			acc.emitTurnEnd(fin.Result, fin.SessionID)
		}
		return nil
	})
	return seen, err
}

// streamJSONInputPipe 常驻会话的 stdin：先写首条 user 消息（提示词 + 附件），
// 之后每从 appendCh 收到一条就再写一轮；appendCh 关闭 → 关掉写端 → 子进程收尾。
//
// 返回的 reader 交给子进程当 stdin（不 EOF 就一直活着）。写失败（子进程已退）时
// 直接收摊，不让 goroutine 泄漏。
func streamJSONInputPipe(prompt string, atts []Attachment, appendCh <-chan string) (io.Reader, error) {
	first, err := streamJSONUserLine(prompt, atts)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		if _, err := pw.Write([]byte(first)); err != nil {
			return
		}
		for msg := range appendCh {
			line, lerr := streamJSONUserLine(msg, nil)
			if lerr != nil {
				continue // 追加内容构造失败（不该发生）：跳过这一条，别把整个会话带崩
			}
			if _, err := pw.Write([]byte(line)); err != nil {
				return
			}
		}
	}()
	return pr, nil
}

// streamJSONUserLine 把「提示词 + 附件」拼成一行 stream-json 输入（含结尾换行）。
//
// content 块顺序：图片附件（image block）在前、提示词文本在后 —— 与官方
// 多模态示例一致（图先给，问题随后）。**非图片**附件没有对应的 content 类型，
// 只能把绝对路径追加进文本（与 trae/openclaw 的兜底同一语义）。
func streamJSONUserLine(prompt string, atts []Attachment) (string, error) {
	blocks := make([]map[string]any, 0, len(atts)+1)
	var nonImage []Attachment
	for _, a := range atts {
		if !a.IsImage() {
			nonImage = append(nonImage, a)
			continue
		}
		data, err := readAttachmentBase64(a)
		if err != nil {
			return "", err
		}
		mime := a.MIME
		if mime == "" {
			mime = "image/png"
		}
		blocks = append(blocks, map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "base64", "media_type": mime, "data": data},
		})
	}
	text := appendAttachmentSection(prompt, nonImage)
	blocks = append(blocks, map[string]any{"type": "text", "text": text})

	msg := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": blocks},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	return string(raw) + "\n", nil
}

// hasImageAttachment 是否含图片附件（决定要不要走 stream-json 输入通道）。
func hasImageAttachment(atts []Attachment) bool {
	for _, a := range atts {
		if a.IsImage() {
			return true
		}
	}
	return false
}

// setArgValue 把 args 里 <flag> 的下一个值改成 want（找不到就追加 <flag> want）。
// claude/codebuddy 的 --output-format json → stream-json 就靠它。
func setArgValue(args []string, flag, want string) []string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			args[i+1] = want
			return args
		}
	}
	return append(args, flag, want)
}

// finalizeStreamText 取最终正文：优先收尾行的 result，其次增量拼接。
func finalizeStreamText(fin streamJSONResult, acc *streamAccumulator) string {
	if t := strings.TrimSpace(fin.Result); t != "" {
		return t
	}
	return strings.TrimSpace(acc.Text.String())
}

// streamJSONArgs 把「文本模式」的 args 改造成 stream-json 输入模式：
//
//	去掉末尾的位置参数（提示词走 stdin，不再作为命令行参数）
//	--output-format → stream-json
//	追加 --input-format stream-json --verbose（+ stream 模式下再加 --include-partial-messages）
func streamJSONArgs(baseArgs []string, prompt string, streaming bool) []string {
	args := make([]string, 0, len(baseArgs))
	for _, a := range baseArgs {
		if a == prompt { // 末尾的位置参数就是提示词，排除掉
			continue
		}
		args = append(args, a)
	}
	args = setArgValue(args, "--output-format", "stream-json")
	args = append(args, "--input-format", "stream-json", "--verbose")
	if streaming {
		args = append(args, "--include-partial-messages")
	}
	return args
}

// errEmptyStreamContent 两家在 stream-json 收尾为空时的公共错误。
func errEmptyStreamContent(label string) error {
	return fmt.Errorf("%s CLI returned empty result", label)
}
