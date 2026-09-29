package agent

import "testing"

/* 这份表就是「形态判定」的契约。改它等于改两个界面的渲染分支，别顺手改。 */
func TestToolKindFor(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		// 执行命令（claude / codebuddy / codex / openclaw 各自的名字）
		{"Bash", ToolKindCommand},
		{"bash", ToolKindCommand},
		{"command", ToolKindCommand},
		{"command_execution", ToolKindCommand},
		{"BashOutput", ToolKindCommand},
		{"KillShell", ToolKindCommand},
		{"terminal", ToolKindCommand},
		// 改动文件
		{"Edit", ToolKindFileChange},
		{"Write", ToolKindFileChange},
		{"MultiEdit", ToolKindFileChange},
		{"NotebookEdit", ToolKindFileChange},
		{"apply_patch", ToolKindFileChange},
		{"str_replace_editor", ToolKindFileChange},
		// 联网
		{"WebSearch", ToolKindWebSearch},
		{"WebFetch", ToolKindWebSearch},
		{"web_search", ToolKindWebSearch},
		// MCP：带命名空间前缀，且**不能**被剥前缀那一步吃掉
		{"mcp__github__create_issue", ToolKindMCP},
		{"mcp.github.create_issue", ToolKindMCP},
		{"MCP__filesystem__read", ToolKindMCP},
		// 子代理
		{"Task", ToolKindAgentCall},
		{"Agent", ToolKindAgentCall},
		// 提问 / 授权
		{"AskUserQuestion", ToolKindInputRequest},
		{"can_use_tool", ToolKindPermission},
		// 命名空间形状（trae / openclaw）：剥前缀后仍要查得出来
		{"builtin.Bash", ToolKindCommand},
		{"tools.Edit", ToolKindFileChange},
		{"acp.Task", ToolKindAgentCall},
		// 已知但未细分的工具 → tool_call（对齐 anywhere 的通用 ToolCallContent）
		{"Read", ToolKindToolCall},
		{"Glob", ToolKindToolCall},
		{"Grep", ToolKindToolCall},
		{"TodoWrite", ToolKindToolCall},
		{"exit_plan_mode", ToolKindToolCall},
		// 从没见过的名字也归 tool_call，**不是** unknown
		{"some_brand_new_tool", ToolKindToolCall},
		// 连名字都没有 → unknown
		{"", ToolKindUnknown},
		{"   ", ToolKindUnknown},
	}
	for _, c := range cases {
		if got := ToolKindFor(c.name, ""); got != c.want {
			t.Errorf("ToolKindFor(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

/* 名字大小写与前后空格不该影响判定（引擎给的名字大小写并不统一）。 */
func TestToolKindForNormalizesName(t *testing.T) {
	for _, n := range []string{"  Bash  ", "BASH", "bAsH"} {
		if got := ToolKindFor(n, ""); got != ToolKindCommand {
			t.Errorf("ToolKindFor(%q) = %q, want %q", n, got, ToolKindCommand)
		}
	}
}
