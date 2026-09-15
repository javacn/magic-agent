// Package agent - 引擎无关的 agent CLI 代理抽象层。
//
// magic-agent 是一个专业的 agent CLI 代理工具：把 claude / codebuddy /
// trae 三家 CLI 的非交互调用统一成一个 Engine 接口，对外提供一致的
// 请求/响应结构、超时与重试语义、固定的 text / json 输出格式。
//
// 各关注点分文件维护：
//
//	engine.go    — Engine 接口、Request/Response 类型、注册表
//	prompt.go    — 多轮消息扁平化为单条 prompt 的公共逻辑
//	claude.go    — Claude Code CLI 引擎（--print --output-format json）
//	codebuddy.go — CodeBuddy（WorkBuddy）CLI 引擎
//	trae.go      — Trae CLI 引擎（-p，无 --model flag，-c model.name= 覆盖）
//	runner.go    — 超时 + 重试编排（可重试错误分类、指数退避）
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

	// Messages 对话消息；CLI 引擎会扁平化为单条 prompt。
	Messages []Message

	// Timeout 单次尝试的超时（含引擎 CLI 自身执行时间）。
	// 0 = 引擎默认。
	Timeout time.Duration
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
	// 返回 (可用, 说明)。说明用于 `engines` 子命令展示。
	Detect() (bool, string)

	// Complete 执行一次调用。实现只负责单次尝试；
	// 超时与重试由 Runner 在外层编排。
	Complete(ctx context.Context, req Request) (Response, error)
}

// registry 内置引擎注册表（顺序即 `engines` 列表展示顺序）。
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
}

// toolsOrDefault nil ToolsMode 视为 ToolsOff。
// 返回接口（而非具体实现）：判定一律走 ToolsMode 的导出方法。
func toolsOrDefault(t ToolsMode) ToolsMode {
	if t == nil {
		return ToolsOff
	}
	return t
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
