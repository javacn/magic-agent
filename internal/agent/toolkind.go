package agent

import "strings"

/* 工具调用的**子形态**（tool kind）—— 由归一化层判定，渲染层不再靠工具名猜。
 *
 * ── 为什么要有它 ──
 * 客户端要按形态给不同的卡：命令、文件变更、网页检索、MCP、子代理、提问授权、通用。
 * 此前只能在**渲染层按工具名猜**（见 magic-test 的 qaFileChangeOf(u.name, uo)），
 * 于是每接一个新引擎就漏一批名字，同一个名字在不同引擎里含义还会漂，
 * 两个界面（旧工作台 / PC 基础对话 UI）各猜一套，必然越走越远。
 * 放在归一化层 = **一处判定**，SSE 流与会话日志两条通道同时带上，两边共用同一份。
 *
 * ── 取值为什么是这几个词 ──
 * 刻意对齐 agents-anywhere 的 `ToolContentKind`（connector/connector/runtime_protocol/timeline.py）：
 * 同一样东西在两个界面上说同一个词，才有「统一」可言。
 */

const (
	// ToolKindCommand 执行命令（Bash / shell / exec 一类）。
	ToolKindCommand = "command"
	// ToolKindFileChange 改动文件（Edit / Write / apply_patch 一类）。
	ToolKindFileChange = "file_change"
	// ToolKindWebSearch 联网检索或抓取网页。
	ToolKindWebSearch = "web_search"
	// ToolKindMCP MCP 服务器提供的工具（名字通常带 `mcp__` 前缀）。
	ToolKindMCP = "mcp"
	// ToolKindAgentCall 派子代理（Task / Agent 一类，主会话里会渲染成委派卡）。
	ToolKindAgentCall = "agent_call"
	// ToolKindInputRequest 向用户提问、要输入（AskUserQuestion 一类，会渲染成提问卡）。
	ToolKindInputRequest = "input_request"
	// ToolKindPermission 请求授权（权限确认那一类，会渲染成审批卡）。
	ToolKindPermission = "permission"
	// ToolKindToolCall 其余**已知**工具（读文件 / 列目录 / 待办…）。
	//
	// ⚠️ 认不出来也归这里，**不归 unknown** —— 与 anywhere 的 claude projector 一致
	//（未知工具名 → 通用 ToolCallContent）：它是「有一次工具调用但形态未细分」，
	// 不是「不知道这是什么事件」。unknown 留给连工具名都没有的情况。
	ToolKindToolCall = "tool_call"
	// ToolKindUnknown 连名字都没有，无法判定形态。
	ToolKindUnknown = "unknown"
)

// 名字 → 形态。全部小写、无命名空间前缀。
// 覆盖本仓库已接的引擎：claude / codebuddy（Bash、Edit、Read、Task…）、
// codex（command_execution、apply_patch…）、openclaw / trae（namespace.tool 形状，已剥前缀）。
var toolKindByName = map[string]string{
	// 执行命令
	"bash": ToolKindCommand, "shell": ToolKindCommand, "sh": ToolKindCommand,
	"exec": ToolKindCommand, "execute": ToolKindCommand, "run": ToolKindCommand,
	"command": ToolKindCommand, "command_execution": ToolKindCommand,
	"bashoutput": ToolKindCommand, "killshell": ToolKindCommand, "terminal": ToolKindCommand,
	"local_shell": ToolKindCommand, "run_command": ToolKindCommand,
	// 改动文件
	"edit": ToolKindFileChange, "write": ToolKindFileChange, "multiedit": ToolKindFileChange,
	"notebookedit": ToolKindFileChange, "apply_patch": ToolKindFileChange,
	"str_replace_editor": ToolKindFileChange, "str_replace": ToolKindFileChange,
	"create_file": ToolKindFileChange, "write_file": ToolKindFileChange,
	"edit_file": ToolKindFileChange, "patch": ToolKindFileChange,
	// 联网
	"websearch": ToolKindWebSearch, "web_search": ToolKindWebSearch,
	"webfetch": ToolKindWebSearch, "web_fetch": ToolKindWebSearch,
	"fetch": ToolKindWebSearch, "browser": ToolKindWebSearch,
	// 子代理
	"task": ToolKindAgentCall, "agent": ToolKindAgentCall, "subagent": ToolKindAgentCall,
	"delegate": ToolKindAgentCall,
	// 提问 / 要输入
	"askuserquestion": ToolKindInputRequest, "ask_user_question": ToolKindInputRequest,
	"elicit": ToolKindInputRequest, "input_request": ToolKindInputRequest,
	// 授权
	"permission": ToolKindPermission, "can_use_tool": ToolKindPermission,
	"askpermission": ToolKindPermission, "approve": ToolKindPermission,
}

// ToolKindFor 判定一次工具调用的子形态。name 是引擎给的工具名；args 暂未参与判定
// （留作后续细分，例如按命令内容区分只读查询与写操作）。认不出来的名字归 `tool_call`。
func ToolKindFor(name, args string) string {
	_ = args // 预留：形态细分仍可能要看参数，先固定签名免得调用点来回改
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return ToolKindUnknown
	}
	// MCP 的名字带命名空间前缀（`mcp__github__create_issue` / `mcp.github.x`），先认它，
	// 否则会被下面剥前缀的动作毁掉线索。
	if strings.HasPrefix(n, "mcp__") || strings.HasPrefix(n, "mcp.") {
		return ToolKindMCP
	}
	// 其余命名空间形状（trae / openclaw 会给 `namespace.tool`）剥掉前缀再查表。
	if i := strings.LastIndex(n, "."); i >= 0 && i < len(n)-1 {
		n = n[i+1:]
	}
	if k, ok := toolKindByName[n]; ok {
		return k
	}
	return ToolKindToolCall
}
