package agent

// prompt.go - 多轮消息扁平化为单条 prompt 的公共逻辑。
//
// CLI 后端（claude -p / codebuddy --print / trae -p）本质上只接受一条
// prompt 字符串。当调用方给出 system prompt + 多轮 messages 时，按各角色
// 打标签拼接；末尾若是 assistant 消息，补一条「请继续」的 user 轮，
// 保证最终话语权交给模型。

import (
	"fmt"
	"strings"
)

// noToolSuffix 追加到 system prompt 末尾的公共约束：禁止 agent loop /
// 工具调用，强制一次性输出最终答案。codebuddy/trae 实测不加这个会输出
// agent trace（"I'll explore..." + <tool_calls>...），下游无法解析。
// claude 引擎默认用 --tools "" 关工具，不追加该后缀。
const noToolSuffix = "\n\n[约束]\n- 严禁使用任何工具(function call / tool call / agent loop)。\n- 直接以纯文本 markdown 一次性输出最终答案,不要输出思考过程、不要解释、不要分析。\n- 不要以 \"I'll ...\" / \"Let me ...\" / \"I need to ...\" 等执行意图开头。\n- 第一行必须是答案正文。"

// FlattenPrompt 把 system prompt + messages 扁平化为一条 prompt。
// appendNoTool 为 true 时在 system 部分后追加 noToolSuffix。
func FlattenPrompt(systemPrompt string, messages []Message, appendNoTool bool) string {
	var sb strings.Builder
	if systemPrompt != "" {
		sb.WriteString("【系统指令】\n")
		sb.WriteString(systemPrompt)
		if appendNoTool {
			sb.WriteString(noToolSuffix)
		}
		sb.WriteString("\n\n")
	}
	for _, m := range messages {
		switch m.Role {
		case "system":
			sb.WriteString("【系统】\n")
		case "user":
			sb.WriteString("【用户】\n")
		case "assistant":
			sb.WriteString("【助手】\n")
		default:
			sb.WriteString(fmt.Sprintf("【%s】\n", m.Role))
		}
		sb.WriteString(m.Content)
		sb.WriteString("\n\n")
	}
	// 末尾是 assistant 时补 user 轮，让 CLI 收到明确的「最终输入」。
	if len(messages) > 0 && messages[len(messages)-1].Role == "assistant" {
		sb.WriteString("【用户】\n请继续。\n")
	}
	return strings.TrimSpace(sb.String())
}

// stripModelPrefix 剥掉 "engine/model" 形式的前缀，返回裸模型名。
func stripModelPrefix(model string) string {
	if i := strings.Index(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}
