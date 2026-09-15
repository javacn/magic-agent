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
//
// 第 4 条针对「伪造工具返回」：实测（2026-09-15）off 模式下约 1/6 概率，
// 模型被要求「输出工具抓取到的内容」但没有工具时，会凭空编造一份看似
// 真实的返回体（如 httpbin JSON），甚至把 prompt 里 URL 携带的随机串填
// 进去冒充真实请求结果。仅禁「调用工具」堵不住这种编造，必须显式禁止。
const noToolSuffix = "\n\n[约束]\n- 严禁使用任何工具(function call / tool call / agent loop)。\n- 严禁伪造工具返回结果:不得编造网页/搜索/命令/文件的输出内容,不得假装调用过工具或声称已获取外部数据。若没有工具可用,就直接说明无法获取,不要用回忆或猜测填充成「工具返回」。\n- 直接以纯文本 markdown 一次性输出最终答案,不要输出思考过程、不要解释、不要分析。\n- 不要以 \"I'll ...\" / \"Let me ...\" / \"I need to ...\" 等执行意图开头。\n- 第一行必须是答案正文。"

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
