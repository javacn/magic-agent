package agent

// stripthinking_tags_test.go - 思维链标签剥离的回归测试。
//
// 注意：这里同样用分段拼接构造标签。连续字面量会被写盘管线改写
// （见 openai_client.go 的说明），导致测试与实现不一致。

import (
	"strings"
	"testing"
)

// tag 拼接出思维链标签，避免在源码里出现可被改写的连续 token。
func tag(parts ...string) string { return strings.Join(parts, "") }

func TestStripThinkingTagsPairs(t *testing.T) {
	open := tag("<", "think", ">")
	closeTag := tag("<", "/", "think", ">")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"无标签原样返回", "答案是 2", "答案是 2"},
		{"标准成对标签", open + "一堆推理" + closeTag + "答案是 2", "答案是 2"},
		{"标签前有正文", "前言 " + open + "中间推理" + closeTag + " 后记", "前言  后记"},
		{"多段标签全部剥离", open + "a" + closeTag + "中" + open + "b" + closeTag + "尾", "中尾"},
		{"有开无闭丢弃尾部", "正文" + open + "未完成的推理", "正文"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripThinkingTags(c.in); got != c.want {
				t.Errorf("stripThinkingTags(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestThinkingTagLiteralsIntact 守卫源码里的标签字面量未被写盘管线改写。
//
// 一旦被改写（长度变化），这里的断言会立即失败，提示重新分段拼接。
func TestThinkingTagLiteralsIntact(t *testing.T) {
	if thinkOpenTag != "<think>" {
		t.Errorf("thinkOpenTag = %q (len %d), want %q (len 7)",
			thinkOpenTag, len(thinkOpenTag), "<think>")
	}
	if thinkCloseTag != "</think>" {
		t.Errorf("thinkCloseTag = %q (len %d), want %q (len 8)",
			thinkCloseTag, len(thinkCloseTag), "</think>")
	}
}
