// Package config - magic-agent 本地配置文件。
//
// 有些引擎不靠本机 CLI，而靠远端网关：凭据（url / key / id）既不适合写进
// 代码，也不适合每次调用都敲一遍 flag。本包提供统一的本地配置文件读取。
//
// 文件位置（按优先级）：
//
//  1. 环境变量 MAGIC_AGENT_CONFIG 指定的路径（~ 前缀会展开）
//  2. $XDG_CONFIG_HOME/magic-agent/config.json
//  3. ~/.config/magic-agent/config.json
//
// 文件不存在 / 为空 **不是错误**：返回零值配置 + nil。调用方按「必需字段
// 是否齐备」报错，这样纯环境变量或纯 flag 的用法同样能工作。
//
// 格式（JSON）：
//
//	{
//	  "systemPrompt": "你是一个中文助手，始终用中文回答所有问题。",
//	  "arkclaw": {
//	    "url": "https://<host>/a2a/jsonrpc",
//	    "key": "<apikey>",
//	    "claw_id": "ci-xxxxxxxx"
//	  }
//	}
//
// 字段名做了宽松兼容（等价写法任选其一）：
//
//	url          ← url | endpoint
//	key          ← key | apikey | api_key
//	claw_id      ← claw_id | clawId | clawID
//	systemPrompt ← systemPrompt | system_prompt | system
//
// 环境变量覆盖（优先级高于文件内容）：
//
//	MAGIC_AGENT_ARKCLAW_URL
//	MAGIC_AGENT_ARKCLAW_KEY
//	MAGIC_AGENT_ARKCLAW_CLAW_ID
//	MAGIC_AGENT_SYSTEM_PROMPT
//
// 本包只负责「读到值」，不做网络校验，也不缓存（每次调用重新读，便于
// 测试与运行期改配置）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvPath 指定配置文件路径的环境变量名。
const EnvPath = "MAGIC_AGENT_CONFIG"

// arkclaw 一节支持的环境变量覆盖名。
const (
	EnvArkClawURL    = "MAGIC_AGENT_ARKCLAW_URL"
	EnvArkClawKey    = "MAGIC_AGENT_ARKCLAW_KEY"
	EnvArkClawClawID = "MAGIC_AGENT_ARKCLAW_CLAW_ID"
)

// EnvSystemPrompt 默认系统提示词的环境变量覆盖名。
const EnvSystemPrompt = "MAGIC_AGENT_SYSTEM_PROMPT"

// Config 是 magic-agent 的全部本地配置。
// arkclaw 一节给远端网关凭据；systemPrompt 是全局默认系统提示词。
type Config struct {
	ArkClaw ArkClawConfig `json:"arkclaw"`

	// SystemPrompt 默认系统提示词：调用方（CLI）未显式给 -s/--system 时注入，
	// 对所有引擎生效。空 = 不注入（保持原行为）。
	SystemPrompt string `json:"systemPrompt"`
}

// UnmarshalJSON 宽松解析顶层键：systemPrompt / system_prompt / system 任选其一。
func (c *Config) UnmarshalJSON(b []byte) error {
	var raw struct {
		ArkClaw          ArkClawConfig `json:"arkclaw"`
		SystemPrompt     string        `json:"systemPrompt"`
		SystemPromptAlt  string        `json:"system_prompt"`
		SystemPromptAlt2 string        `json:"system"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	c.ArkClaw = raw.ArkClaw
	c.SystemPrompt = firstNonEmpty(raw.SystemPrompt, raw.SystemPromptAlt, raw.SystemPromptAlt2)
	return nil
}

// ArkClawConfig 是 arkclaw 引擎（A2A JSON-RPC 网关）的连接配置。
type ArkClawConfig struct {
	// URL A2A JSON-RPC 端点，如 https://<host>/a2a/jsonrpc
	URL string `json:"url"`

	// Key 网关鉴权 apikey（作为 query 参数 apikey 发送）
	Key string `json:"key"`

	// ClawID 目标 agent 标识（作为 query 参数 clawId 发送）
	ClawID string `json:"claw_id"`
}

// UnmarshalJSON 宽松解析：同一语义的多种键名任选其一。
// 之所以不用单靠 struct tag —— encoding/json 的大小写不敏感回退无法跨过
// 下划线（"claw_id" 与 "clawId" 视为不同键）。
func (a *ArkClawConfig) UnmarshalJSON(b []byte) error {
	var raw struct {
		URL        string `json:"url"`
		Endpoint   string `json:"endpoint"`
		Key        string `json:"key"`
		APIKey     string `json:"apikey"`
		APIKeyAlt  string `json:"api_key"`
		ClawID     string `json:"claw_id"`
		ClawIDAlt  string `json:"clawId"`
		ClawIDAlt2 string `json:"clawID"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	a.URL = firstNonEmpty(raw.URL, raw.Endpoint)
	a.Key = firstNonEmpty(raw.Key, raw.APIKey, raw.APIKeyAlt)
	a.ClawID = firstNonEmpty(raw.ClawID, raw.ClawIDAlt, raw.ClawIDAlt2)
	return nil
}

// Ready 报告三项必需配置是否都非空。
func (a ArkClawConfig) Ready() bool {
	return len(a.Missing()) == 0
}

// Missing 返回缺失的字段名（用于报错文案）。
func (a ArkClawConfig) Missing() []string {
	var out []string
	if strings.TrimSpace(a.URL) == "" {
		out = append(out, "url")
	}
	if strings.TrimSpace(a.Key) == "" {
		out = append(out, "key")
	}
	if strings.TrimSpace(a.ClawID) == "" {
		out = append(out, "claw_id")
	}
	return out
}

// Path 返回生效的配置文件路径（不检查存在性；无法确定时返回空串）。
func Path() string {
	if v := strings.TrimSpace(os.Getenv(EnvPath)); v != "" {
		return expandHome(v)
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "magic-agent", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "magic-agent", "config.json")
}

// Load 读取默认位置的配置文件，并叠加环境变量覆盖。
func Load() (Config, error) {
	return LoadFrom(Path())
}

// LoadFrom 读取指定路径的配置文件，并叠加环境变量覆盖。
//
// path 为空、文件不存在、或文件内容全为空白时，返回零值配置 + nil
// （「没配置」是合法状态，由调用方决定缺字段是否致命）。
// 其它错误（读失败 / JSON 语法错）原样上报 —— 静默吞掉畸形配置会让
// 使用者排查不到根因。
func LoadFrom(path string) (Config, error) {
	var c Config
	if strings.TrimSpace(path) != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if len(strings.TrimSpace(string(b))) > 0 {
				if jerr := json.Unmarshal(b, &c); jerr != nil {
					return Config{}, fmt.Errorf("parse config %s: %w", path, jerr)
				}
			}
		case os.IsNotExist(err):
			// 未创建配置文件：走零值 + 环境变量。
		default:
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
	}
	return applyEnv(c), nil
}

// applyEnv 用环境变量覆盖配置值（环境变量优先级高于文件）。
func applyEnv(c Config) Config {
	if v := strings.TrimSpace(os.Getenv(EnvArkClawURL)); v != "" {
		c.ArkClaw.URL = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvArkClawKey)); v != "" {
		c.ArkClaw.Key = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvArkClawClawID)); v != "" {
		c.ArkClaw.ClawID = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvSystemPrompt)); v != "" {
		c.SystemPrompt = v
	}
	return c
}

// firstNonEmpty 返回第一个去空白后非空的实参。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// expandHome 展开路径开头的 ~（仅支持 ~ 与 ~/ 形式）。
func expandHome(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	if len(p) > 1 && p[1] != '/' && p[1] != filepath.Separator {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if len(p) == 1 {
		return home
	}
	return filepath.Join(home, p[2:])
}
