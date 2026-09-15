package agent

// models.go - ~/.magic-agent/models.json 的配置模型与加载/解析。
//
// 这是一个与引擎无关的「模型清单」：每个 provider 声明自己的 api 类型
// （openai-completions / ollama / codebuddy-cli / trae-cli / claude-cli）
// 与它下面的模型 id。llmengine.go 据此把 -m <model> 路由到对应后端。
//
// 加载顺序（先命中先用）：
//  1. $MAGIC_AGENT_MODELS           显式指定路径
//  2. ~/.magic-agent/models.json    本工具自己的配置
//  3. ~/.magic-video/config.json    兜底复用 magic-video 的 models.default
//
// 第 3 条让已有 magic-video apiKey 的用户开箱可用，无需重录密钥。
// 三种来源都接受两种形状：
//
//	本工具形状      {"default":"minimax/MiniMax-M3","providers":[...]}
//	magic-video 形状 {"models":{"default":[...]}}
//
// 兼容逻辑集中在 effectiveProviders()，上层只需调用 LoadModels/ResolveModel。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// api 类型常量。空值按 openai-completions 处理（最常见的默许语义）。
const (
	// APIOpenAI OpenAI 兼容的 /chat/completions HTTP 接口。
	APIOpenAI = "openai-completions"
	// APIOllama 本地 ollama：shell 调用 `ollama run <model>`。
	APIOllama = "ollama"
	// APICodeBuddy 委托 WorkBuddy 内置 codebuddy CLI（复用 CodeBuddyEngine）。
	APICodeBuddy = "codebuddy-cli"
	// APITrae 委托 trae-cli（复用 TraeEngine）。
	APITrae = "trae-cli"
	// APIClaude 委托 Claude Code CLI（复用 ClaudeEngine）。
	APIClaude = "claude-cli"
)

// ModelSpec 是 provider 下的一个模型。
type ModelSpec struct {
	ID            string   `json:"id"`
	Input         []string `json:"input,omitempty"`         // 支持的输入模态，空 = text
	ContextWindow int      `json:"contextWindow,omitempty"` // 可选上下文窗口
}

// Provider 是 models.json 中的一个后端。
//
// 字段与 magic-video 的 ProviderEntry 对齐（name/baseUrl/apiKey/api/timeout/
// models/extraBody），因此同一份配置两处通用。
type Provider struct {
	Name      string         `json:"name"`
	API       string         `json:"api,omitempty"`     // 空 = openai-completions
	BaseURL   string         `json:"baseUrl,omitempty"` // HTTP 类必填
	APIKey    string         `json:"apiKey,omitempty"`
	Timeout   int            `json:"timeout,omitempty"` // 秒；0 = 默认 300s
	Models    []ModelSpec    `json:"models,omitempty"`
	ExtraBody map[string]any `json:"extraBody,omitempty"` // 原样并入请求体（厂商私有开关）
}

// ModelsFile 是 models.json 的顶层结构。
type ModelsFile struct {
	// Default 默认模型标识，"provider/model" 或裸 "model"。
	// 空 = 取 magic-video 的 llm.textModels[0]，仍为空则取第一个 provider 的首个模型。
	Default string `json:"default,omitempty"`

	// Providers 按声明顺序排列（顺序即裸模型名的解析优先级）。
	Providers []Provider `json:"providers,omitempty"`

	// LLM 兼容 magic-video 的 {"llm":{"textModels":[...]}} 形状，
	// 其 textModels[0] 作为默认模型。
	LLM *struct {
		TextModels  []string `json:"textModels,omitempty"`
		ImageModels []string `json:"imageModels,omitempty"`
		VideoModels []string `json:"videoModels,omitempty"`
	} `json:"llm,omitempty"`

	// Models 兼容 magic-video 的 {"models":{"default":[...]}} 形状。
	Models *struct {
		Default []Provider `json:"default,omitempty"`
	} `json:"models,omitempty"`
}

// DefaultModel 返回生效的默认模型标识。
// 优先级：顶层 default > magic-video 的 llm.textModels[0] > 空。
func (m ModelsFile) DefaultModel() string {
	if s := strings.TrimSpace(m.Default); s != "" {
		return s
	}
	if m.LLM != nil {
		for _, s := range m.LLM.TextModels {
			if t := strings.TrimSpace(s); t != "" {
				return t
			}
		}
	}
	return ""
}

// effectiveProviders 返回实际参与解析的 provider 列表。
// 本工具形状（providers）优先；否则取 magic-video 形状（models.default）。
func (m ModelsFile) effectiveProviders() []Provider {
	if len(m.Providers) > 0 {
		return m.Providers
	}
	if m.Models != nil {
		return m.Models.Default
	}
	return nil
}

// HomeDir 返回 ~/.magic-agent 目录（不创建）。
func HomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".magic-agent")
	}
	return filepath.Join(home, ".magic-agent")
}

// modelsCandidates 返回按优先级排列的候选配置文件。
func modelsCandidates() []string {
	var out []string
	if env := os.Getenv("MAGIC_AGENT_MODELS"); env != "" {
		out = append(out, env)
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(home, ".magic-agent", "models.json"),
			filepath.Join(home, ".magic-video", "config.json"),
		)
	}
	return out
}

// ErrModelsNotFound 所有候选路径都不存在。
var ErrModelsNotFound = errors.New("models config not found")

// LoadModels 从候选路径读取模型配置，返回 (配置, 实际使用的路径)。
//
// 首个存在的文件生效；文件存在但解析失败时直接报错（不静默跳过），
// 避免用户改了 models.json 却以为生效了。
func LoadModels() (ModelsFile, string, error) {
	var tried []string
	for _, p := range modelsCandidates() {
		data, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				tried = append(tried, p)
				continue
			}
			return ModelsFile{}, p, fmt.Errorf("read models config %s: %w", p, err)
		}
		var mf ModelsFile
		if err := json.Unmarshal(data, &mf); err != nil {
			return ModelsFile{}, p, fmt.Errorf("parse models config %s: %w", p, err)
		}
		if len(mf.effectiveProviders()) == 0 {
			return ModelsFile{}, p, fmt.Errorf("models config %s defines no providers", p)
		}
		return mf, p, nil
	}
	return ModelsFile{}, "", fmt.Errorf("%w (tried: %s); create ~/.magic-agent/models.json", ErrModelsNotFound, strings.Join(tried, ", "))
}

// ResolveModel 把模型标识解析为 (provider, 裸模型 id)。
//
// 接受形式：
//
//	"provider/model"  显式指定 provider（精确名匹配）
//	"model"           裸名，按 providers 声明顺序取首个包含它的
//	""                取配置的 default；default 为空则取第一个 provider 的首个模型
//
// provider 名与模型 id 的匹配均忽略大小写。
func ResolveModel(mf ModelsFile, model string) (Provider, string, error) {
	provs := mf.effectiveProviders()
	if len(provs) == 0 {
		return Provider{}, "", fmt.Errorf("models config defines no providers")
	}

	model = strings.TrimSpace(model)

	// 空 → 配置的 default（含 magic-video 的 llm.textModels 兜底）。
	if model == "" {
		model = mf.DefaultModel()
	}
	// default 也为空 → 第一个 provider 的首个模型。
	if model == "" {
		for _, p := range provs {
			if len(p.Models) > 0 {
				return p, p.Models[0].ID, nil
			}
		}
		return Provider{}, "", fmt.Errorf("no model specified and no provider declares any model")
	}

	// "provider/model" 显式形式。
	if i := strings.Index(model, "/"); i > 0 {
		name, bare := model[:i], model[i+1:]
		for _, p := range provs {
			if equalFold(p.Name, name) {
				if !providerHasModel(p, bare) {
					return Provider{}, "", fmt.Errorf("provider %q has no model %q (available: %s)",
						name, bare, strings.Join(modelIDs(p), ", "))
				}
				// 回填 provider 声明的规范大小写。
				return p, canonicalID(p, bare), nil
			}
		}
		return Provider{}, "", fmt.Errorf("provider %q not found (available: %s)", name, strings.Join(providerNames(provs), ", "))
	}

	// 裸模型名 → 按声明顺序取首个命中的 provider。
	// 命中后回填 provider 声明的规范大小写（用户可能传 "minimax-m3"）。
	for _, p := range provs {
		if providerHasModel(p, model) {
			return p, canonicalID(p, model), nil
		}
	}
	return Provider{}, "", fmt.Errorf("model %q not found in any provider (available: %s)",
		model, strings.Join(allModelIDs(provs), ", "))
}

// providerHasModel 判断 provider 是否声明了该模型（忽略大小写）。
// provider 未声明 models 时视为「接受任意模型」（透传语义）。
func providerHasModel(p Provider, bare string) bool {
	if len(p.Models) == 0 {
		return true
	}
	for _, m := range p.Models {
		if equalFold(m.ID, bare) {
			return true
		}
	}
	return false
}

// canonicalID 返回 provider 中该模型的规范大小写形式；未声明时原样返回。
func canonicalID(p Provider, bare string) string {
	for _, m := range p.Models {
		if equalFold(m.ID, bare) {
			return m.ID
		}
	}
	return bare
}

// EffectiveAPI 返回 provider 的 api 类型（空 = openai-completions）。
func (p Provider) EffectiveAPI() string {
	api := strings.TrimSpace(p.API)
	if api == "" {
		return APIOpenAI
	}
	return api
}

// HTTPTimeout 返回 provider 配置的超时；0 = 调用方默认。
func (p Provider) HTTPTimeout() time.Duration {
	if p.Timeout > 0 {
		return time.Duration(p.Timeout) * time.Second
	}
	return 0
}

// DisplayName 返回展示用的 provider 名（空名回退 "<unnamed>"）。
func (p Provider) DisplayName() string {
	if s := strings.TrimSpace(p.Name); s != "" {
		return s
	}
	return "<unnamed>"
}

func modelIDs(p Provider) []string {
	out := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		out = append(out, m.ID)
	}
	return out
}

func providerNames(ps []Provider) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.DisplayName())
	}
	return out
}

// allModelIDs 返回 "provider/model" 形式的全量清单（供错误提示）。
func allModelIDs(ps []Provider) []string {
	var out []string
	for _, p := range ps {
		for _, m := range p.Models {
			out = append(out, p.DisplayName()+"/"+m.ID)
		}
	}
	if len(out) == 0 {
		return providerNames(ps)
	}
	return out
}
