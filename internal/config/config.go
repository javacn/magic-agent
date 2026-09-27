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
//	  },
//	  "codebuddyGateway": {
//	    "url": "http://127.0.0.1:8399"
//	  }
//	}
//
// 字段名做了宽松兼容（等价写法任选其一）：
//
//	url          ← url | endpoint
//	key          ← key | apikey | api_key
//	claw_id      ← claw_id | clawId | clawID
//	systemPrompt ← systemPrompt | system_prompt | system
//	password     ← password | token | key | secret（codebuddyGateway 节）
//
// 环境变量覆盖（优先级高于文件内容）：
//
//	MAGIC_AGENT_ARKCLAW_URL
//	MAGIC_AGENT_ARKCLAW_KEY
//	MAGIC_AGENT_ARKCLAW_CLAW_ID
//	MAGIC_AGENT_SYSTEM_PROMPT
//	MAGIC_AGENT_CBGW_URL
//	MAGIC_AGENT_CBGW_PASSWORD
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

// codebuddyGateway 一节支持的环境变量覆盖名。
const (
	EnvCBGWURL      = "MAGIC_AGENT_CBGW_URL"
	EnvCBGWPassword = "MAGIC_AGENT_CBGW_PASSWORD"
)

// EnvSystemPrompt 默认系统提示词的环境变量覆盖名。
const EnvSystemPrompt = "MAGIC_AGENT_SYSTEM_PROMPT"

// Config 是 magic-agent 的全部本地配置。
// arkclaw 一节给远端网关凭据；systemPrompt 是全局默认系统提示词；
// agents 是**具名 A2A agent 清单**（每个条目 = 一个独立引擎，见 AgentConfig）。
type Config struct {
	ArkClaw ArkClawConfig `json:"arkclaw"`

	// CodeBuddyGateway CodeBuddy Code HTTP 网关（`codebuddy --serve` / `/gateway`
	// 远程控制模式）的连接配置，见 CBGatewayConfig。
	CodeBuddyGateway CBGatewayConfig `json:"codebuddyGateway"`

	// SystemPrompt 默认系统提示词：调用方（CLI）未显式给 -s/--system 时注入，
	// 对所有引擎生效。空 = 不注入（保持原行为）。
	SystemPrompt string `json:"systemPrompt"`

	/* Agents 具名 A2A agent 清单（2026-09-23 加）。
	 *
	 * 为什么需要它：arkclaw 那个引擎**只有一个**（名字写死在注册表里），而模型是
	 * **由 claw_id 在网关侧绑死**的 —— 想同时用两个 claw（或想给同一个 claw 一个
	 * 好认的名字）就没有位置放。这一节就是那个位置：每个条目注册成一个独立引擎，
	 * 名字取条目里的 `name`，于是 `-e MagicAI` 与 `-e arkclaw` 可以并列存在。
	 *
	 * ⚠️ 与 `arkclaw` 节的分工：`arkclaw` 是**历史保留的单实例**（老配置照旧生效，
	 * 行为一个字不改）；`agents` 是新增的**多实例**通道。两者互不影响。 */
	Agents []AgentConfig `json:"agents"`
}

// UnmarshalJSON 宽松解析顶层键：systemPrompt / system_prompt / system 任选其一。
func (c *Config) UnmarshalJSON(b []byte) error {
	var raw struct {
		ArkClaw          ArkClawConfig   `json:"arkclaw"`
		CBGW             CBGatewayConfig `json:"codebuddyGateway"`
		CBGWAlt          CBGatewayConfig `json:"codebuddy_gateway"`
		CBGWAlt2         CBGatewayConfig `json:"codebuddy-gateway"`
		CBGWAlt3         CBGatewayConfig `json:"cbgw"`
		SystemPrompt     string          `json:"systemPrompt"`
		SystemPromptAlt  string          `json:"system_prompt"`
		SystemPromptAlt2 string          `json:"system"`
		Agents           []AgentConfig   `json:"agents"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	c.ArkClaw = raw.ArkClaw
	// 四个键名任选其一（同一语义的多种写法；都写时按上面的优先级取第一个非空节）。
	c.CodeBuddyGateway = firstNonEmptySection(raw.CBGW, raw.CBGWAlt, raw.CBGWAlt2, raw.CBGWAlt3)
	c.SystemPrompt = firstNonEmpty(raw.SystemPrompt, raw.SystemPromptAlt, raw.SystemPromptAlt2)
	c.Agents = raw.Agents
	return nil
}

// firstNonEmptySection 返回第一个「有内容」的配置节（全空则返回零值）。
//
// 为什么不像字符串那样简单 firstNonEmpty：节是结构体，多个别名键同时出现时
// 要挑**配了东西**的那个，而不是「第一个出现的」。判据用 URL 非空 ——
// 它是这一节唯一的必需字段（见 CBGatewayConfig.Missing）。
func firstNonEmptySection(sections ...CBGatewayConfig) CBGatewayConfig {
	for _, s := range sections {
		if strings.TrimSpace(s.URL) != "" {
			return s
		}
	}
	return CBGatewayConfig{}
}

/* AgentConfig 是 agents 数组里的一条：一个**具名 A2A agent**（走 arkclaw 同一条
 * JSON-RPC 协议，见 internal/agent/arkclaw.go）。
 *
 * 两种写法都支持（凭据可以内嵌在 URL 里，也可以分开写）：
 *
 *	{ "name": "MagicAI", "url": "https://host/a2a/jsonrpc?apikey=…&clawId=…" }
 *	{ "name": "MagicAI", "url": "https://host/a2a/jsonrpc", "key": "…", "claw_id": "ci-…" }
 *
 * ⚠️ URL 里已经带了 `apikey`/`clawId` 时**不必**再写 key/claw_id —— 拼接时会用
 * `q.Set` 保留既有 query（同值覆盖，不重复追加），所以两种写法等价、也不怕写重。
 * 键名与 arkclaw 节同一套宽松别名（url/endpoint、key/apikey/api_key、claw_id/clawId/clawID）。 */
type AgentConfig struct {
	// Name 引擎名（`-e <Name>` 用它，`--engines` 也用它）。空 → 该条目被跳过。
	Name string `json:"name"`

	// URL A2A JSON-RPC 端点（可自带 apikey / clawId query 参数）。
	URL string `json:"url"`

	// Key 网关鉴权 apikey（URL 里已带时可留空）。
	Key string `json:"key"`

	// ClawID 目标 agent 标识（URL 里已带时可留空）。
	ClawID string `json:"claw_id"`
}

// UnmarshalJSON 宽松解析（键名别名与 ArkClawConfig 完全一致，两处别各写一套）。
func (a *AgentConfig) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name       string `json:"name"`
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
	a.Name = strings.TrimSpace(raw.Name)
	a.URL = firstNonEmpty(raw.URL, raw.Endpoint)
	a.Key = firstNonEmpty(raw.Key, raw.APIKey, raw.APIKeyAlt)
	a.ClawID = firstNonEmpty(raw.ClawID, raw.ClawIDAlt, raw.ClawIDAlt2)
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

/* CBGatewayConfig 是 codebuddy-gateway 引擎的连接配置。
 *
 * 这个引擎不跑本机 CLI，也不走远端 A2A 网关，而是接**本机（或可达的）CodeBuddy Code
 * HTTP 网关** —— 也就是 `codebuddy --serve` / 交互会话里的 `/gateway` 那一套远程控制
 * 服务：magic-agent 把 prompt 以 `POST /api/v1/webhooks/{platform}` 投递进去（网关
 * 立即回 202 + runId），再用 `GET /api/v1/runs/{runId}/stream` 收 SSE 增量。
 *
 * 为什么凭据放配置文件而不是 flag：网关地址与访问 token 是**环境相关**的（本机端口
 * 每次都可能不同、公网 Tunnel 域名更是每次重启都换），写进命令行既难记也容易进 shell
 * 历史；与 arkclaw 同一取舍。
 *
 * 两种等价写法（token 也可以内嵌在 URL 的 query 里，见 CBGatewayConfig.Token）：
 *
 *	{ "codebuddyGateway": { "url": "http://127.0.0.1:8399" } }
 *	{ "codebuddyGateway": { "url": "https://xxx.trycloudflare.com", "password": "<token>" } }
 *
 * 节名别名：codebuddyGateway | codebuddy_gateway | codebuddy-gateway | cbgw。
 * 字段别名：url | endpoint；password | token | key | secret。 */
type CBGatewayConfig struct {
	// URL 网关根地址（不带 /api/v1 前缀），如 http://127.0.0.1:8399。
	URL string `json:"url"`

	// Password 网关访问口令。`--serve` 默认开密码认证（`--auth password`），
	// 口令在启动日志里打印；`--auth none` 时留空即可。
	Password string `json:"password"`

	// Platform 平台标识（webhooks 路径段）：generic | wecom | wechat-kf。
	// 空 = generic（本引擎唯一能自洽收流的通道，见 codebuddy_gateway.go 文件头）。
	Platform string `json:"platform"`

	// Sender 发送者标识（网关按它做限流与来源归属）。空 = "magic-agent"。
	Sender string `json:"sender"`

	// Conversation 会话标识（**续接的锚点**：网关按它 getOrCreateSession）。
	// 空 = 每次调用新开一个（即无上下文续接）。非空时同一 id 复用同一会话。
	Conversation string `json:"conversation"`
}

// UnmarshalJSON 宽松解析：同一语义的多种键名任选其一。
// 与 ArkClawConfig 同一理由 —— encoding/json 的大小写不敏感回退跨不过下划线。
func (c *CBGatewayConfig) UnmarshalJSON(b []byte) error {
	var raw struct {
		URL       string `json:"url"`
		Endpoint  string `json:"endpoint"`
		Password  string `json:"password"`
		Token     string `json:"token"`
		Key       string `json:"key"`
		Secret    string `json:"secret"`
		Platform  string `json:"platform"`
		Sender    string `json:"sender"`
		SenderAlt string `json:"sender_id"`
		Conv      string `json:"conversation"`
		ConvAlt   string `json:"conversation_id"`
		ConvAlt2  string `json:"conversationId"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	c.URL = firstNonEmpty(raw.URL, raw.Endpoint)
	c.Password = firstNonEmpty(raw.Password, raw.Token, raw.Key, raw.Secret)
	c.Platform = firstNonEmpty(raw.Platform)
	c.Sender = firstNonEmpty(raw.Sender, raw.SenderAlt)
	c.Conversation = firstNonEmpty(raw.Conv, raw.ConvAlt, raw.ConvAlt2)
	return nil
}

// Ready 报告必需配置是否齐备（只有 url 是必需的：`--auth none` 的网关没有口令）。
func (c CBGatewayConfig) Ready() bool { return len(c.Missing()) == 0 }

// Missing 返回缺失的字段名（用于报错文案）。
func (c CBGatewayConfig) Missing() []string {
	if strings.TrimSpace(c.URL) == "" {
		return []string{"url"}
	}
	return nil
}

// Path 返回生效的配置文件路径（无法确定时返回空串）。
//
// 探测顺序（2026-09-23 起 `~/.magic-agent/config.json` 提到最前）：
//
//  1. MAGIC_AGENT_CONFIG          —— 显式指定，永远最高优先
//  2. ~/.magic-agent/config.json  —— **存在时**用它（用户 2026-09-23 定稿：
//     「应该放在 ~/.magic-agent/ 下」；与 llm 的 models.json 同目录）
//  3. $XDG_CONFIG_HOME/magic-agent/config.json
//  4. ~/.config/magic-agent/config.json —— 历史默认位置
//
// ⚠️ 第 2 条**带存在性判断**是刻意的：新位置没建文件时不能把老位置顶掉，
// 否则老配置会被静默忽略 —— 那正是「配置改了没生效」这类报障的来源。
func Path() string {
	if v := strings.TrimSpace(os.Getenv(EnvPath)); v != "" {
		return expandHome(v)
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if p := filepath.Join(home, ".magic-agent", "config.json"); fileExists(p) {
			return p
		}
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "magic-agent", "config.json")
	}
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "magic-agent", "config.json")
}

// fileExists 报告路径是否是一个存在的普通文件（探测链用）。
// 权限错误 / 是个目录都按「不是配置文件」处理 —— 探测链要的是「能不能读它」。
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
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
	if v := strings.TrimSpace(os.Getenv(EnvCBGWURL)); v != "" {
		c.CodeBuddyGateway.URL = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvCBGWPassword)); v != "" {
		c.CodeBuddyGateway.Password = v
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
