package agent

// llmmodels.go - `~/.magic-agent/models.json`：llm 引擎的「配置文件模型」注册表。
//
// 用户定稿（2026-09-17）原话：
//
//	「llm模式下可选的是配置文件中的」+「配置文件里不是有模型真实名字吗 参数传id
//	 否则无法区分用一个名字的模型」
//
// 由此确定的契约：
//
//	id     —— 调用标识（`-m <id>`、界面下拉里的值）。**唯一**，用来区分真实名相同的变体
//	model  —— 线上「真实模型名」（缺省 = id）。同一个真实名可以挂多条配置
//	name   —— 展示名（只给人看，不参与执行）
//	其余键 —— 端点 url / apiKey / timeout / extraBody 等，执行时按 id 查表直连
//
// 为什么不能只把 id 原样交给 simonw/LLM CLI：llm CLI 只认它自己注册的 model_id，
// 且无法表达每条配置自带的 extraBody —— 实测 `minimax-nothink` 与 `MiniMax-M3`
// 真实模型名相同，只靠 `{"thinking":{"type":"disabled"}}` 区分（直连实测：默认变体
// 正文里混 `<think>`，disabled 变体直接出正文）。所以必须由 magic-agent 直连端点。
//
// 文件形态（扁平数组，**首条即默认**）：
//
//	[
//	  { "id": "MiniMax-M3", "name": "MiniMax-M3", "vendor": "MiniMax",
//	    "url": "https://api.minimaxi.com/v1/chat/completions", "apiKey": "sk-…", "timeout": 600 },
//	  { "id": "minimax-nothink", "model": "MiniMax-M3", "name": "MiniMax-M3 (no thinking)",
//	    "url": "https://api.minimaxi.com/v1/chat/completions", "apiKey": "sk-…",
//	    "extraBody": { "thinking": { "type": "disabled" } } }
//	]
//
// 路径：$MAGIC_AGENT_MODELS > ~/.magic-agent/models.json。
// 文件缺失 / 非法 JSON → 整个直连链路不生效（Lookup 返回 false），llm 引擎退回原有行为
//（把 -m 原样透传给 llm CLI），不会因为配置写坏而让 llm 引擎整体不可用。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LLMConfigModel 是 models.json 的一条配置。
type LLMConfigModel struct {
	ID              string         `json:"id"`              // 调用标识（-m 的值）
	Model           string         `json:"model"`           // 线上真实模型名（空 = 用 ID）
	Name            string         `json:"name"`            // 展示名（不参与执行）
	Vendor          string         `json:"vendor"`          // 供应商标注（不参与执行）
	URL             string         `json:"url"`             // OpenAI 兼容端点
	APIKey          string         `json:"apiKey"`          // 密钥（支持 ${VAR} 占位）
	Timeout         int            `json:"timeout"`         // 单次调用超时（秒）
	Temperature     *float64       `json:"temperature"`     // 该模型的默认温度
	MaxOutputTokens int            `json:"maxOutputTokens"` // 该模型的默认输出上限
	ExtraBody       map[string]any `json:"extraBody"`       // 透传到请求体顶层（变体差异常在这）
}

// WireModel 返回实际发给端点的模型名：有 model 用 model，否则用 id。
func (m LLMConfigModel) WireModel() string {
	if s := strings.TrimSpace(m.Model); s != "" {
		return s
	}
	return strings.TrimSpace(m.ID)
}

// Endpoint 返回最终请求地址。配置里已写明 /chat/completions 就原样用；
// 只给了 base（如 https://api.minimaxi.com/v1）则补全 —— 实测两种写法都有人用。
func (m LLMConfigModel) Endpoint() string {
	u := strings.TrimSpace(m.URL)
	if u == "" {
		return ""
	}
	if strings.HasSuffix(u, "/chat/completions") {
		return u
	}
	return strings.TrimRight(u, "/") + "/chat/completions"
}

// LLMModelsPath 返回 models.json 的路径（$MAGIC_AGENT_MODELS 优先）。
func LLMModelsPath() string {
	if p := strings.TrimSpace(os.Getenv("MAGIC_AGENT_MODELS")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".magic-agent", "models.json")
}

// LoadLLMConfigModels 读取并校验配置文件。文件缺失 / 非法 → 空切片 + 错误
// （调用方一律按「不生效」处理，不向上抛）。
func LoadLLMConfigModels() ([]LLMConfigModel, error) {
	p := LLMModelsPath()
	if p == "" {
		return nil, fmt.Errorf("cannot resolve models.json path")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var list []LLMConfigModel
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("invalid models.json (%s): %w", p, err)
	}
	out := make([]LLMConfigModel, 0, len(list))
	for _, m := range list {
		if strings.TrimSpace(m.ID) == "" {
			continue // 没有 id 的行无法被 -m 寻址，跳过
		}
		m.URL = expandEnvKeep(m.URL)
		m.APIKey = expandEnvKeep(m.APIKey)
		out = append(out, m)
	}
	return out, nil
}

// LookupLLMConfigModel 按 id 查配置（精确优先，其次忽略大小写）。
func LookupLLMConfigModel(id string) (LLMConfigModel, bool) {
	want := strings.TrimSpace(id)
	if want == "" {
		return LLMConfigModel{}, false
	}
	list, err := LoadLLMConfigModels()
	if err != nil {
		return LLMConfigModel{}, false
	}
	for _, m := range list {
		if m.ID == want {
			return m, true
		}
	}
	for _, m := range list {
		if strings.EqualFold(m.ID, want) {
			return m, true
		}
	}
	return LLMConfigModel{}, false
}

// ResolveLLMConfigModel 解析本次调用要用的配置条目：
//
//	给了 -m → 按 id 查（查不到 → ok=false，回退 llm CLI 路径）
//	没给 -m → 取**首条**（「首条即默认」，配置文件就是 llm 引擎的模型注册表）
func ResolveLLMConfigModel(model string) (LLMConfigModel, bool) {
	if strings.TrimSpace(model) != "" {
		return LookupLLMConfigModel(stripModelPrefix(model))
	}
	list, err := LoadLLMConfigModels()
	if err != nil || len(list) == 0 {
		return LLMConfigModel{}, false
	}
	return list[0], true
}

// expandEnvKeep 展开 ${VAR} / $VAR；**未设置的变量保留原文**（不置空），
// 让 401/404 直接把「占位没填」这件事暴露出来，而不是静默用空密钥。
func expandEnvKeep(s string) string {
	if s == "" || !strings.Contains(s, "$") {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			sb.WriteByte(s[i])
			i++
			continue
		}
		// ${VAR}
		if i+1 < len(s) && s[i+1] == '{' {
			if end := strings.IndexByte(s[i+2:], '}'); end >= 0 {
				name := s[i+2 : i+2+end]
				if v, ok := os.LookupEnv(name); ok {
					sb.WriteString(v)
				} else {
					sb.WriteString(s[i : i+2+end+1]) // 原样保留
				}
				i += 2 + end + 1
				continue
			}
		}
		// $VAR（字母/数字/下划线）
		j := i + 1
		for j < len(s) && (isEnvNameChar(s[j])) {
			j++
		}
		if j > i+1 {
			name := s[i+1 : j]
			if v, ok := os.LookupEnv(name); ok {
				sb.WriteString(v)
			} else {
				sb.WriteString(s[i:j])
			}
			i = j
			continue
		}
		sb.WriteByte('$')
		i++
	}
	return sb.String()
}

func isEnvNameChar(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
