package agent

// tags.go - 思维链标签常量与剥离逻辑。
//
// 部分推理模型（MiniMax-M3 等）经 simonw/llm CLI 输出时，思维链以
// 标签形式直接混在正文里。本文件提供：
//
//	thinkOpenTag / thinkCloseTag  标签常量（运行时值）
//	stripThinkingTags             非流式剥离（正文里删掉标签块）
//	splitThinkingTags             非流式拆分（正文、思维链都要）— 在 llmengine.go
//
// ⚠️ 标签常量必须分段拼接，不能写成连续字面量 —— 该标签 token 序列会被
// 某些分词器当作特殊 token 并在写盘时改写（实测被替换成全角竖线形式，
// 长度从 8 变 19），导致源码里的标签与实际字符串不符、Index 偏移错位。
// 分段拼接后运行时值不变，但源码中不出现该 token 序列。

import "strings"

// 思维链标签的构造片段（分段拼接，见文件头说明）。
const (
	tagLT    = "<"
	tagGT    = ">"
	tagSlash = "/"
	tagWord  = "think"
)

// thinkOpenTag / thinkCloseTag 运行时值为思维链的开始/结束标签。
var (
	thinkOpenTag  = tagLT + tagWord + tagGT
	thinkCloseTag = tagLT + tagSlash + tagWord + tagGT
)

// stripThinkingTags 剥离正文中残留的思维链标签块，返回剩余正文。
// 有开标签无闭标签时，开标签之后的残余整段丢弃（视为未完成的思考）。
func stripThinkingTags(s string) string {
	for {
		i := strings.Index(s, thinkOpenTag)
		if i < 0 {
			break
		}
		rest := s[i+len(thinkOpenTag):]
		j := strings.Index(rest, thinkCloseTag)
		if j < 0 {
			// 有开无闭：整段视为未完成的思考，丢弃。
			s = s[:i]
			break
		}
		// 相对 rest 切片，避免绝对/相对偏移混算。
		s = s[:i] + rest[j+len(thinkCloseTag):]
	}
	return strings.TrimSpace(s)
}
