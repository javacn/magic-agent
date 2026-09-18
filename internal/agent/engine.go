// Package agent - 引擎无关的 agent CLI 代理抽象层。
//
// magic-agent 是一个专业的 agent CLI 代理工具：把 claude / codebuddy /
// trae / llm / codex / openclaw 六家 CLI 的非交互调用统一成一个 Engine 接口，
// 对外提供一致的请求/响应结构、超时与重试语义、固定的 text / json 输出格式。
//
// 各关注点分文件维护：
//
//	engine.go      — Engine 接口、Request/Response 类型、注册表
//	engine_base.go — 公共基座：统一探测链（cliBase）+ 统一参数矩阵
//	prompt.go      — 多轮消息扁平化为单条 prompt 的公共逻辑
//	claude.go      — Claude Code CLI 引擎（--print --output-format json）
//	codebuddy.go   — CodeBuddy（WorkBuddy）CLI 引擎
//	trae.go        — Trae CLI 引擎（-p，无 --model flag，-c model.name= 覆盖）
//	llmengine.go   — simonw/LLM CLI 引擎（-m / -s / -o 透传）
//	codex.go       — Codex CLI 引擎（exec / exec resume + --output-schema）
//	openclaw.go    — OpenClaw CLI 引擎（agent --local --json）
//	runner.go      — 超时 + 重试编排（可重试错误分类、指数退避）
package agent

import (
	"context"
	"time"
)

// Message 是一轮对话消息。
type Message struct {
	Role    string // "system" | "user" | "assistant"
	Content string
}

// ToolsMode 工具开关模式。
//
//	ToolsOff      禁用全部工具（默认）：纯 chat 一次成型，输出可解析。
//	ToolsOn       引擎默认工具集 + 权限旁路：真 agent 模式，模型可执行
//	              工具调用（读文件、跑命令等），代价是输出可能是工具痕迹。
//	ToolsAllowlist 白名单：仅允许列出的工具 + 权限旁路。
//
// 判定方法一律导出：Engine 实现与本包外的调用方（如 CLI 层测试）
// 都要能检视模式；非导出方法的接口无法被跨包断言。
type ToolsMode interface {
	// IsOff 是否为「禁用全部工具」模式。
	IsOff() bool
	// IsOn 是否为「引擎默认工具集 + 权限旁路」模式。
	IsOn() bool
	// Allowlist 白名单模式下的工具名；非白名单模式返回 nil。
	Allowlist() []string
}

// toolsModeImpl 统一实现载体。
type toolsModeImpl struct {
	off       bool
	on        bool
	allowlist []string
}

// ToolsOff / ToolsOn 预置模式。
var (
	ToolsOff = toolsModeImpl{off: true}
	ToolsOn  = toolsModeImpl{on: true}
)

// ToolsAllowlist 构造白名单模式。
func ToolsAllowlist(names []string) ToolsMode {
	return toolsModeImpl{allowlist: names}
}

// IsOff / IsOn / Allowlist 实现 ToolsMode。
func (t toolsModeImpl) IsOff() bool         { return t.off }
func (t toolsModeImpl) IsOn() bool          { return t.on }
func (t toolsModeImpl) Allowlist() []string { return t.allowlist }

// Request 是一次 agent 调用的完整输入。
type Request struct {
	// Engine 引擎名（"claude" | "codebuddy" | "trae"），由 Runner 填充，
	// 引擎实现一般不需要读它。
	Engine string

	// Model 模型标识。空串 = 引擎自身配置的默认模型。
	// 引擎实现负责剥掉可能的 "engine/model" 前缀。
	Model string

	// SystemPrompt 可选系统提示词。各引擎自行决定注入方式
	//（claude/codebuddy用 --append-system-prompt，trae 拼进 prompt 头）。
	SystemPrompt string

	// Tools 工具开关模式（默认 ToolsOff）。
	Tools ToolsMode

	// Permission 四档权限档位（默认 DefaultPermissionTier = full，保持既有行为）。
	//
	// 四档语义与 Claude Code 参数映射见 permission.go。空串 = 用默认档。
	// 只有 PermissionSupported 为真的引擎（claude / codebuddy）会真正落地；
	// 其余引擎传非空值会在 CLI 层被**明确拒绝**（exit 2），不会静默忽略 ——
	// 静默忽略一个安全设置，等于让用户以为自己被保护着。
	Permission PermissionTier

	// PermissionOptions 四档模型之上的可选项（沙箱排除命令、网络白名单、
	// 第 3 档分类器的受信边界、deny/ask 规则）。零值即最严格。
	// 只在 PermissionSupported 的引擎上生效。
	PermissionOptions PermissionOptions

	// Messages 对话消息；CLI 引擎会扁平化为单条 prompt。
	Messages []Message

	// SessionID 会话续接 id。空 = 新会话（默认，上下文从零开始）；
	// 非空 = 续接该会话（引擎透传各自的续接参数），上一轮上下文继续生效。
	// 会话 id 通常取自上一次 Response.SessionID。各引擎映射（统一支持）：
	// claude/codebuddy --resume <id>；trae --resume=<id>；llm --cid <id>。
	// 注意：新会话不主动禁用落盘（--no-session-persistence 的会话无法续接），
	// 这样首轮返回的 session_id 才能用于下一轮 --session。
	SessionID string

	// Continue 续接"最近一次会话"（不需要 session id）。各引擎映射
	//（统一支持）：claude/codebuddy --continue；trae 裸 --resume
	//（AUTO = 自动接最近一次会话，实测 -p 非交互模式可用）；llm -c。
	// 与 SessionID 同时设置时 SessionID 优先（显式 id 比"最近一次"更精确）。
	Continue bool

	// MaxTokens 输出 token 上限。0 = 不指定（交给引擎/模型默认）。
	//
	// 各引擎映射（统一支持，trae 除外）：llm 经 `-o max_tokens` 透传；
	// claude/codebuddy 经 `--settings` 注入 env CLAUDE_CODE_MAX_OUTPUT_TOKENS
	//（两者无原生 flag）；trae 不支持 —— `-c <k>=<v>` 会整体覆盖模型配置块
	//（实测注入后模型名漂移并触发配额错误），静默忽略。
	// 推理模型（MiniMax-M3 等）的思考过程计入 output 配额，调用方
	// 需要给足够大的值，否则正文会被思考吃光。
	MaxTokens int

	// Temperature 采样温度。nil = 不提（交给引擎/模型默认）。
	// 仅 llm 支持（`-o temperature`）；claude/codebuddy/trae 无原生参数，
	// 忽略（不上报错误）。
	Temperature *float64

	// JSONSchema 是可选的结构化输出约束。
	//
	// 各引擎行为：
	//   llm 引擎：尝试把 Response.Text 解析回 JSON 对象。
	//             严格匹配 schema 的顶层 Required 字段集 + Type=object，
	//             解析成功则把 Text 替换为 json.Marshal 后的紧凑串。
	//             失败则原样返回（不阻断，让上层解析器兜底）。
	//   其他引擎：暂忽略；约束完全靠 system prompt 注入。
	//
	// 上游（magic-video）已经会把 schema 渲染进 system prompt 作为
	// 提示词约束；本字段只是 llm 引擎做"输出后处理"以稳定 JSON。
	JSONSchema *JSONSchema

	// Attachments 随提示词一起发送的附件（截图 / 图片为主）。
	//
	// 用户需求原文（2026-09-17）：「文件＋提示词 比如截图加提示词」——
	// 提示词与附件是**并列**关系，不是「提示词其实是个文件路径」。
	//
	// 各引擎落地方式（尽量用原生能力，见 AttachmentSupportOf）：
	//
	//	codex     原生 `-i/--image <FILE>...`（可重复）
	//	claude    原生 `--input-format stream-json`：附件作为 message content 的
	//	          image block 走 stdin（CLI 要求此时 output-format 也是 stream-json）
	//	codebuddy 同上（同族 CLI，同样有 --input-format）
	//	llm       直连：OpenAI 兼容 content parts（image_url + data URL）；
	//	          委托 llm CLI：原生 `-a/--attachment <path>`
	//	arkclaw   原生 A2A：message.parts 里追加 kind=file 的 file part（base64 inline）
	//	trae      无原生输入通道 → 只能把**绝对路径**写进提示词，靠它的读文件工具看
	//	openclaw  同上
	//
	// 路径由 CLI 层校验（存在 + 普通文件）后填成绝对路径；引擎不重复校验。
	Attachments []Attachment

	// Append 追加消息通道（常驻会话 / keep-alive）。
	//
	// 用户需求（2026-09-18）：「会话中追加需求」。非 nil 时本次调用是**常驻会话**：
	// 引擎先发提示词，之后每从通道收到一条文本就再追加一轮 user 消息（同一进程、同一会话），
	// 通道关闭 → stdin EOF → 引擎收尾退出。
	//
	// 只有 claude / codebuddy 支持（走 stream-json 输入：`--input-format stream-json` 的
	// stdin 可以持续喂 user 消息，官方文档明确「allows providing guidance to the model
	// while it [is working]」，本项目 2026-09-18 实测同一进程内两轮均被处理）。
	// 其它引擎拿到非 nil 会在 CLI 层被挡下（见 AppendSupportOf）。
	Append <-chan string

	// Timeout 单次尝试的超时（含引擎 CLI 自身执行时间）。
	// 0 = 引擎默认。
	Timeout time.Duration

	// Workspace 工作目录（绝对路径）。空 = 用调用方的 cwd（默认行为）。
	//
	// 各引擎落地方式（尽量用原生能力，见 WorkspaceSupportOf）：
	//
	//	codex     原生 `-C/--cd <dir>`（working root）**并**子进程 cwd
	//	claude    子进程 cwd（CLI 无工作目录 flag；--add-dir 只用于追加额外目录）
	//	codebuddy 同上（有 --add-dir，语义同 claude）
	//	trae      同上（有 --add-dir）
	//	openclaw  不支持：workspace 与 agent 绑定（`openclaw agents`），无 per-call flag，
	//	          实测子进程 cwd 被忽略 → 参数被忽略（CLI 层提示）
	//	llm       不支持：模型不直接读写文件，无文件系统语义 → 参数被忽略（CLI 层提示）
	//	arkclaw   不支持：远端网关按 claw_id 绑定，客户端无法指定
	//
	// 上面的"生效/忽略"与 WorkspaceSupportOf 的取值一一对应（flag:-C / cwd / none）。
	//
	// 目录必须存在且是目录（CLI 层校验）；不存在时 spawn 会以 "chdir ...: no such file
	// or directory" 失败，这里提前拦掉更清楚。
	Workspace string
}

// JSONSchema 是传给 Engine 的结构化输出契约（精简版）。
//
// 故意保持极简：只覆盖本项目实际用到（type=object + 顶层 Required）。
// 需要更多字段时再按需扩展。
type JSONSchema struct {
	Type       string                    // "object"
	Properties map[string]map[string]any // field name -> 字段描述 {type, ...}
	Required   []string                  // 顶层必填字段名
}

// Response 是一次成功调用的结果。
type Response struct {
	// Engine 产出该响应的引擎名。
	Engine string

	// Text 助手正文（不含思考过程）。
	Text string

	// Model 实际使用的模型标识（探测或透传，尽力而为）。
	Model string

	// SessionID 引擎返回的会话 id（可能为空）。
	SessionID string

	// 以下三个 token 计数尽力而为：引擎/CLI 不上报时为 0。
	// 目前只有 llm 引擎能拿到（llm prompt --json 的 input_tokens /
	// output_tokens）；其余 CLI 引擎没有该信息。
	InputTokens  int
	OutputTokens int
	TotalTokens  int

	// Latency 一次成功尝试的耗时（不含重试等待）。
	Latency time.Duration

	// Attempts 本次调用总共尝试次数（含成功那次，>=1）。
	Attempts int
}

// Engine 是一个 agent CLI 后端的统一抽象。
type Engine interface {
	// Name 引擎标识（"claude" / "codebuddy" / "trae"）。
	Name() string

	// Detect 探测引擎 CLI 是否可用（可执行文件存在等）。
	// 返回 (可用, 说明)。说明用于 `--engines` 展示。
	Detect() (bool, string)

	// Complete 执行一次调用。实现只负责单次尝试；
	// 超时与重试由 Runner 在外层编排。
	Complete(ctx context.Context, req Request) (Response, error)
}

// registry 内置引擎注册表（顺序即 `--engines` 列表展示顺序）。
var registry []Engine

// Register 注册一个引擎（通常在各引擎文件的 init() 中调用）。
func Register(e Engine) {
	registry = append(registry, e)
}

// Engines 返回全部已注册引擎。
func Engines() []Engine {
	if len(registry) == 0 {
		initEngines()
	}
	out := make([]Engine, len(registry))
	copy(out, registry)
	return out
}

// Lookup 按名字查找引擎（大小写不敏感）。未找到返回 nil。
func Lookup(name string) Engine {
	if name == "" {
		return nil
	}
	for _, e := range Engines() {
		if equalFold(e.Name(), name) {
			return e
		}
	}
	return nil
}

// initEngines 惰性初始化，避免 import 顺序影响注册。
func initEngines() {
	Register(&ClaudeEngine{})
	Register(&CodeBuddyEngine{})
	Register(&TraeEngine{})
	Register(&LLMEngine{})
	Register(&CodexEngine{})
	Register(&OpenClawEngine{})
	Register(&ArkClawEngine{})
}

// toolsOrDefault nil ToolsMode 视为 ToolsOff。
// 返回接口（而非具体实现）：判定一律走 ToolsMode 的导出方法。
func toolsOrDefault(t ToolsMode) ToolsMode {
	if t == nil {
		return ToolsOff
	}
	return t
}

// toolsIsOff 是否「禁用工具」模式（nil 视为 off）。
// 各引擎用它决定是否注入 noToolSuffix —— 该后缀明文禁止工具调用，
// 只能在 off 模式出现，否则会和 on / 白名单模式互相打架。
func toolsIsOff(req Request) bool {
	return toolsOrDefault(req.Tools).IsOff()
}

// ToolsIsOff / ToolsIsOn / ToolsAllowlistOf 跨包检视 ToolsMode 的便捷包装。
// 与 ToolsMode 的导出方法等价，保留为语义化入口。
func ToolsIsOff(t ToolsMode) bool           { return toolsOrDefault(t).IsOff() }
func ToolsIsOn(t ToolsMode) bool            { return toolsOrDefault(t).IsOn() }
func ToolsAllowlistOf(t ToolsMode) []string { return toolsOrDefault(t).Allowlist() }

// equalFold 简易 ASCII 大小写不敏感比较。
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
