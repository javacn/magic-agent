package agent

// models.go - ~/.magic-agent/models.json 的加载与解析。
//
// 这是一个「模型清单」：**扁平 JSON 数组**，每条自带完整的 url + apiKey，
// 与 WorkBuddy / CodeBuddy CLI 的 LanguageModel 格式互为超集
// （本工具多出 timeout / extraBody / temperature 三个扩展字段，
// 对方的条目直接拿来也能读）。
//
// 加载顺序（先命中先用）：
//  1. $MAGIC_AGENT_MODELS           显式指定路径
//  2. ~/.magic-agent/models.json    本工具自己的配置
//
// 数组**首条即默认模型**，因此不需要 default 包装字段。
// 上层只需调用 LoadModels / ResolveModel。
//
// 每条恒为 OpenAI 兼容 HTTP 端点（本工具不再支持 api 字段路由与
// 委托其他引擎；ollama 用 http://localhost:11434/v1/chat/completions 表达）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ModelEntry 是 models.json 数组里的一条模型。
//
// 字段对齐 WorkBuddy / CodeBuddy CLI 的 LanguageModel，并追加三个扩展
// （Timeout / ExtraBody / Temperature）。
type ModelEntry struct {
	// ── 身份 ──

	// ID 调用方标识（-m 用）。必填，且在文件内唯一（忽略大小写）。
	ID string `json:"id"`
	// Model 发往 API 的真实模型名；空 = 用 ID。
	//
	// 用途：同一个真实模型要在配置里出现多次时（不同 key / 不同开关），
	// 用 ID 区分调用方视角，用 Model 指定线上模型名。例如：
	//
	//	{"id":"minimax-nothink","model":"MiniMax-M3","extraBody":{...}}
	Model string `json:"model,omitempty"`
	// Name 展示名（--engines 用）；空 = 用 ID。
	Name string `json:"name,omitempty"`
	// Vendor 供应商名（展示用）。
	Vendor string `json:"vendor,omitempty"`

	// ── 连接 ──

	// URL 完整 API 端点。必填。支持 ${ENV_VAR} 引用。
	URL string `json:"url"`
	// APIKey Bearer 认证密钥。支持 ${ENV_VAR} 引用。
	// 本地端点（如 ollama）可留空。
	APIKey string `json:"apiKey,omitempty"`
	// UseCustomProtocol 为 true 时 URL 原样透传，不补 /chat/completions。
	UseCustomProtocol bool `json:"useCustomProtocol,omitempty"`

	// ── 能力标记（仅展示/校验用，不参与请求构造）──

	SupportsToolCall  bool `json:"supportsToolCall,omitempty"`
	SupportsImages    bool `json:"supportsImages,omitempty"`
	SupportsReasoning bool `json:"supportsReasoning,omitempty"`
	MaxInputTokens    int  `json:"maxInputTokens,omitempty"`
	MaxOutputTokens   int  `json:"maxOutputTokens,omitempty"`

	// ── magic-agent 扩展 ──

	// Timeout 单次 HTTP 超时（秒）；0 = 调用方默认（300s）。
	Timeout int `json:"timeout,omitempty"`
	// ExtraBody 原样并入请求体顶层，用于厂商私有开关。
	//
	// 这是**必须保留**的字段：推理模型（如 MiniMax-M3）不关思维链时
	// 长文本生成会被推理吃光 token 预算、正文为空。
	// 典型值：{"thinking":{"type":"disabled"}}
	ExtraBody map[string]any `json:"extraBody,omitempty"`
	// Temperature 采样温度；nil = 不发该字段。
	Temperature *float64 `json:"temperature,omitempty"`
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
	if env := strings.TrimSpace(os.Getenv("MAGIC_AGENT_MODELS")); env != "" {
		out = append(out, env)
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".magic-agent", "models.json"))
	}
	return out
}

// ErrModelsNotFound 所有候选路径都不存在。
var ErrModelsNotFound = errors.New("models config not found")

// LoadModels 从候选路径读取模型清单，返回 (条目, 实际使用的路径)。
//
// 首个存在的文件生效；文件存在但解析失败时直接报错（不静默跳过），
// 避免用户改了 models.json 却以为生效了。加载后立即做 ${ENV_VAR}
// 展开与结构校验。
func LoadModels() ([]ModelEntry, string, error) {
	var tried []string
	for _, p := range modelsCandidates() {
		data, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				tried = append(tried, p)
				continue
			}
			return nil, p, fmt.Errorf("read models config %s: %w", p, err)
		}
		var entries []ModelEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, p, fmt.Errorf("parse models config %s: %w (want a JSON array of model entries)", p, err)
		}
		if len(entries) == 0 {
			return nil, p, fmt.Errorf("models config %s defines no models", p)
		}
		if err := validateEntries(entries); err != nil {
			return nil, p, fmt.Errorf("models config %s: %w", p, err)
		}
		// ${ENV_VAR} 展开：key 与 url 都支持，便于把密钥留在环境里。
		for i := range entries {
			entries[i].URL = expandEnv(entries[i].URL)
			entries[i].APIKey = expandEnv(entries[i].APIKey)
		}
		return entries, p, nil
	}
	return nil, "", fmt.Errorf("%w (tried: %s); create ~/.magic-agent/models.json",
		ErrModelsNotFound, strings.Join(tried, ", "))
}

// validateEntries 结构校验：每条必须有 id 与 url，且 id 唯一。
//
// 直接回应「必须有完整的 baseurl 和 key 才行」：缺 url 直接拒绝加载。
// apiKey 允许为空（本地端点无需密钥），仅在 --engines 里提示无 key。
func validateEntries(entries []ModelEntry) error {
	seen := make(map[string]int, len(entries))
	for i, m := range entries {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			return fmt.Errorf("entry #%d: missing \"id\"", i+1)
		}
		if strings.TrimSpace(m.URL) == "" {
			return fmt.Errorf("entry %q: missing \"url\" (each model needs a full endpoint)", id)
		}
		key := strings.ToLower(id)
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("duplicate id %q (entries #%d and #%d); use distinct \"id\" values and set \"model\" to the wire model name if they point at the same model",
				id, prev+1, i+1)
		}
		seen[key] = i
	}
	return nil
}

// expandEnv 展开 ${VAR} / $VAR；未设置的变量**保持原样**（不报错、不置空）。
//
// 保留占位符而非静默置空：让问题在真正发请求时以 401/404 暴露，
// 比加载期就把 key 清空更好定位。与 WorkBuddy 行为一致。
func expandEnv(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return os.Expand(s, func(k string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return "${" + k + "}"
	})
}

// ResolveModel 按 query 找一条模型。
//
// 接受形式：
//
//	""      首条（数组顺序即优先级，首条即默认）
//	"id"    忽略大小写匹配 ID；命中后回填声明的规范大小写
//
// 未命中时返回错误并列出全部可用 id。
func ResolveModel(entries []ModelEntry, query string) (ModelEntry, error) {
	if len(entries) == 0 {
		return ModelEntry{}, fmt.Errorf("models config defines no models")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return entries[0], nil
	}
	for _, m := range entries {
		if equalFold(m.ID, query) {
			return m, nil
		}
	}
	return ModelEntry{}, fmt.Errorf("model %q not found (available: %s)", query, strings.Join(entryIDs(entries), ", "))
}

// entryIDs 返回全部 id（供错误提示）。
func entryIDs(entries []ModelEntry) []string {
	out := make([]string, 0, len(entries))
	for _, m := range entries {
		out = append(out, m.ID)
	}
	return out
}

// WireModel 返回发往 API 的模型名：配了 model 用 model，否则用 id。
func (m ModelEntry) WireModel() string {
	if s := strings.TrimSpace(m.Model); s != "" {
		return s
	}
	return m.ID
}

// Endpoint 产出最终请求 URL。
//
//	UseCustomProtocol = true      原样返回（仅去尾部斜杠）
//	已以 /chat/completions 结尾    原样返回（避免重复追加）
//	其余                          去尾部斜杠后补 /chat/completions
//
// 补全规则与 WorkBuddy 一致（日志实证：
// https://ark.cn-beijing.volces.com/api/coding/v3 →
// .../api/coding/v3/chat/completions）。
func (m ModelEntry) Endpoint() string {
	u := strings.TrimRight(strings.TrimSpace(m.URL), "/")
	if u == "" || m.UseCustomProtocol {
		return u
	}
	if strings.HasSuffix(u, "/chat/completions") {
		return u
	}
	return u + "/chat/completions"
}

// HTTPTimeout 返回配置的超时；0 = 调用方默认。
func (m ModelEntry) HTTPTimeout() time.Duration {
	if m.Timeout > 0 {
		return time.Duration(m.Timeout) * time.Second
	}
	return 0
}

// DisplayName 返回展示用名字：name → id → "<unnamed>"。
func (m ModelEntry) DisplayName() string {
	if s := strings.TrimSpace(m.Name); s != "" {
		return s
	}
	if s := strings.TrimSpace(m.ID); s != "" {
		return s
	}
	return "<unnamed>"
}

// HasAPIKey 是否配了密钥（本地端点可以没有）。
func (m ModelEntry) HasAPIKey() bool {
	return strings.TrimSpace(m.APIKey) != ""
}
