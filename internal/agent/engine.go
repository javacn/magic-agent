// Package agent - 引擎无关的 agent CLI 代理抽象层。
//
// magic-agent 是一个专业的 agent CLI 代理工具：把 claude / codebuddy /
// trae / llm / codex / openclaw / dsh 七家 CLI 与 arkclaw 网关的非交互调用
// 统一成一个 Engine 接口，对外提供一致的请求/响应结构、超时与重试语义、
// 固定的 text / json 输出格式。
//
// 各关注点分文件维护：
//
//	engine.go      — Engine 接口、Request/Response 类型、注册表
//	engine_base.go — 公共基座：统一探测链（cliBase）+ 统一参数矩阵
//	prompt.go      — 多轮消息扁平化为单条 prompt 的公共逻辑
//	claude.go      — Claude Code CLI 引擎（--print --output-format json）
//	codebuddy.go   — CodeBuddy Code CLI 引擎（一个独立 CLI、两个账号）
//	trae.go        — Trae CLI 引擎（-p，无 --model flag，-c model.name= 覆盖）
//	llmengine.go   — simonw/LLM CLI 引擎（-m / -s / -o 透传）
//	codex.go       — Codex CLI 引擎（exec / exec resume + --output-schema）
//	openclaw.go    — OpenClaw CLI 引擎（agent --local --json）
//	dsh.go         — DeepSeek Harness 引擎（--profile headless "<任务>"，纯文本 stdout）
//	arkclaw.go     — ArkClaw A2A 网关引擎（HTTP JSON-RPC message/send）
//	arkclaw_stream.go — ArkClaw 流式通道（A2A message/stream 的 SSE 事件流）
//	codebuddy_gateway.go — CodeBuddy Code HTTP 网关引擎（webhook 投递 + SSE 收流）
//	codebuddy_gateway_stream.go — 上面那个的 SSE 通道（逐字增量）
//	runner.go      — 超时 + 重试编排（可重试错误分类、指数退避）
package agent

import (
	"context"
	"strings"
	"time"

	"github.com/darren/magic-agent/internal/config"
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
	// claude / codebuddy / codebuddy-ai 走 stream-json 输入（`--input-format stream-json` 的
	// stdin 可以持续喂 user 消息，官方文档明确「allows providing guidance to the model
	// while it [is working]」，本项目 2026-09-18 实测同一进程内两轮均被处理）；
	// dsh 走 SDK 通道（同一进程内对同一 sessionId 继续 session/prompt，官方推荐的续接方式）。
	// 其它引擎拿到非 nil 会在 CLI 层被挡下（见 AppendSupportOf）。
	// 默认值是否开启按引擎区分，见 AppendDefaultOn（dsh 默认关）。
	Append <-chan string

	// OnSessionID 引擎发现 session_id 时回调（用于「看历史」持久化）。
	//
	// 谁需要它：CLI 在 runStreamAsk 里包一个 sessionWriter，每条事件落盘到
	// `<dir>/<session_id>.jsonl`。但 session_id 出现得早（claude 的 init 行就有），
	// 而引擎先发完初始帧 → 调用方再开文件 → 早期帧全丢；这个回调让 writer
	// **在收到第一条 init 行时就开好文件**，避免丢前几条事件。
	//
	// 调用方保证：id 非空时回调；多次回调以最后一次为准；与 onEvent 并发调用，
	// 调用方需要同步。引擎实现里一般一行 `req.OnSessionID(extract(line))` 即可。
	OnSessionID func(id string)

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

/* registry 引擎注册表（顺序即 `--engines` / `--contract` 的列表展示顺序）。
 *
 * ⚠️ 已知脆弱点（2026-09-27 记录，**行为未改**）：内置引擎的注册挂在
 * 「registry 为空才 initEngines」这个守卫上。任何在 Engines() 之前调 Register 的代码
 * 都会让 registry 非空，于是**内置引擎一个都不注册、且没有任何报错** —— 现象是
 * 「claude 的能力全是空的」「--engines 少了十个引擎」这类极难定位的问题。
 * 生产路径不会触发（Register 只被 initEngines 与 registerConfiguredAgents 调用），
 * 但用例里的探针引擎会触发，使**按名筛选**的测试结果随运行方式而变化
 * （arkclaw_test.go 的 __capfam_probe__ 曾在跑 `-run 'Capabilit'` 时让内置引擎整体消失）。
 * 要修就得连同一批「用假引擎遮住同名内置」的用例一起改：试过把内置改为无条件注册
 *（前置或追加），都会打断 TestEnginesInstallCommandByAvailability 这类用例，故暂不动。
 * 在那之前：**别在 Engines() 之前调 Register。** */
var registry []Engine

// Register 注册一个引擎（具名 A2A agent 走这里）。
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

/* CapabilityFamily 可选接口：引擎声明「我该走能力表的哪一行」。
 *
 * 为什么需要（2026-09-23）：能力表（workspace / attachments / permission / ask / append）
 * 全是**按引擎名写死的 switch**，而配置里的具名 agent 是**同一协议的新名字**
 * （MagicAI = 另一个 ArkClawEngine）—— 新名字会落进 default 分支、拿到错的能力。
 * 实测踩到：MagicAI 被报成 `workspace=cwd` / `attachments=prompt`，
 * 而它实际是 `none` / `part:file`（后者会让界面白白警告「图贴了但模型看不到」）。
 * 有了这个接口，具名实例只要声明家族，五张表一起就对。
 * 未实现该接口的引擎 = 家族名就是自己的 Name()（内置引擎行为一字不变）。 */
type CapabilityFamily interface {
	CapabilityFamily() string
}

// CapabilityFamily 实现 CapabilityFamily：A2A JSON-RPC 网关这一族（含具名 agent）。
func (e *ArkClawEngine) CapabilityFamily() string { return "arkclaw" }

/* CapabilityFamilyOf 把引擎名解析成「能力家族名」。
 *
 * 先按注册表找到那个实例：它实现了 CapabilityFamily 就用它的答案；
 * 否则（内置引擎 / 未注册的名字）返回原名 —— 于是内置引擎行为完全不变，
 * 而具名 agent 自动继承其协议那一整行能力。
 * ⚠️ 会（幂等地）触发注册表初始化：`MagicAI` 这种名字只有建完注册表才认得出来。 */
func CapabilityFamilyOf(engine string) string {
	name := strings.TrimSpace(engine)
	if name == "" {
		return ""
	}
	if e := Lookup(name); e != nil {
		if cf, ok := e.(CapabilityFamily); ok {
			if fam := strings.TrimSpace(cf.CapabilityFamily()); fam != "" {
				return fam
			}
		}
	}
	return name
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

// initEngines 惰性初始化，避免 import 顺序影响注册（脆弱点见 registry 的注释）。
func initEngines() {
	Register(&ClaudeEngine{})
	Register(&CodeBuddyEngine{})
	// codebuddy-ai 与 codebuddy 用**同一个**独立 CLI，只是账号不同
	//（ACC_PRODUCT_CONFIG_V3 切 authentication.id，见 codebuddy.go）。
	// 必须注册在 codebuddy 之后 —— Lookup 取第一个同名匹配，两者名字不同不冲突。
	Register(&CodeBuddyAIEngine{})
	// codebuddy-gateway = CodeBuddy 的**第三种接入方式**：不起本机 CLI，而是把 prompt
	// 投给已在跑的 CodeBuddy Code HTTP 网关（`codebuddy --serve` / `/gateway`），
	// 走 webhook 投递 + SSE 收流（见 codebuddy_gateway.go 文件头）。
	Register(&CodeBuddyGatewayEngine{})
	Register(&TraeEngine{})
	Register(&LLMEngine{})
	Register(&CodexEngine{})
	Register(&OpenClawEngine{})
	// dsh = DeepSeek Harness 的 headless profile（一次性任务入口）。
	Register(&DshEngine{})
	Register(&ArkClawEngine{})
	// 配置里 agents 数组的**具名 A2A agent**（放在内置引擎之后，重名会被跳过）。
	registerConfiguredAgents()
}

/* registerConfiguredAgents 把配置里 `agents` 数组的每个条目注册成一个独立引擎
 * （2026-09-23 加，用户：「新增 <url> 名字叫 MagicAI」）。
 *
 * 为什么在这里做、而不另起一个「引擎工厂」：注册表本来就是「有哪些引擎」的唯一真相，
 * 而 agents 只是**同一协议（A2A JSON-RPC）的多个实例** —— 复用 ArkClawEngine、
 * 只换 DisplayName / URL / Key / ClawID，于是 `--engines` 与 `-e <name>` 自动就能用，
 * 上层（magic-test 的引擎下拉）**一行都不用改**（它读的就是 --engines）。
 *
 * 边界（三条都不致命 —— 宁可少一个引擎，也不要把 CLI 打死）：
 *   · 读配置失败 / 没有 agents 节 → 什么都不注册（老行为一字不变）；
 *   · 条目 name 为空 → 跳过（没有名字就没法 `-e` 它）；
 *   · name 与已注册引擎重名 → 跳过（Lookup 取第一个匹配，注册了也只会被遮住，
 *     却会在 `--engines` 里多出一条同名行 —— 那是纯粹的误导）。
 */
func registerConfiguredAgents() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	for _, e := range agentEnginesFrom(cfg.Agents, registry) {
		Register(e)
	}
}

// agentEnginesFrom 把配置里的 agents 条目转成待注册的引擎（纯函数，便于直接测）。
//
// 两条过滤规则见 registerConfiguredAgents 的注释：**空名跳过**、**与已有引擎重名跳过**。
// `existing` 传入而不是直接读包级 registry —— 注册表只在首次 `Engines()` 时建一次，
// 拿它当输入会让测试依赖「谁先跑」，那是假红/假绿的常见来源。
func agentEnginesFrom(agents []config.AgentConfig, existing []Engine) []Engine {
	if len(agents) == 0 {
		return nil
	}
	taken := make(map[string]bool, len(existing)+len(agents))
	for _, e := range existing {
		taken[strings.ToLower(e.Name())] = true
	}
	var out []Engine
	for _, a := range agents {
		name := strings.TrimSpace(a.Name)
		if name == "" || taken[strings.ToLower(name)] {
			continue
		}
		taken[strings.ToLower(name)] = true
		out = append(out, &ArkClawEngine{DisplayName: name, URL: a.URL, Key: a.Key, ClawID: a.ClawID})
	}
	return out
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
