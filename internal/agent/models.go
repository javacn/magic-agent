package agent

// models.go - 引擎「支持的模型清单」的动态探测（--engines 使用）。
//
// 核心约束：模型清单**一律不硬编码**。每个引擎只用它自己的权威入口
// 现取现算，CLI 升级 / 用户注册新模型后 --engines 自动跟随：
//
//	引擎      动态来源
//	claude    ~/.claude/settings.json（顶层 model + env 里 ANTHROPIC_*_MODEL[*_NAME]）①
//	codebuddy `codebuddy --help` 里 --model 描述自带的 "Currently supported: (...)" 清单
//	trae      `trae-cli models --json`
//	llm       `llm models`（用户经 llm CLI 自注册的模型）
//	codex     `codex debug models`（raw model catalog）
//	openclaw  `openclaw models list --json`
//	arkclaw   无 —— 模型由网关按 claw_id 绑定，返回 ErrNoModelSource ②
//
// ① claude 没有 models 子命令（--help 只有 agents/auth/doctor/mcp/plugin/
//    project/... ），可用的模型标识只能从用户配置里读：settings.json 的
//    env 键是 claude 自己的契约（ANTHROPIC_DEFAULT_{SONNET,OPUS,HAIKU}_MODEL
//    与 *_MODEL_NAME 指向代理真实模型）。这里按「前缀 ANTHROPIC_ + 后缀
//    MODEL / MODEL_NAME」过滤键名，不写死具体模型名。
// ② 探测失败/无来源都不算致命：CLI 层把原因写进 models_note，models 字段
//    留空（见 cli/root.go runEngines）。
//
// 各引擎的 ListModels 都是**只读**子命令（不发起推理、不消耗额度），
// 但会真的启动 CLI 进程（openclaw ~2.5s 最慢），因此探测并发执行、
// 单引擎超时 DefaultModelProbeTimeout（见 cli/root.go）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultModelProbeTimeout 单个引擎模型探测的超时（CLI 启动 + 输出）。
// 实测最慢的是 openclaw models list --json（~2.5s），30s 足够宽裕。
const DefaultModelProbeTimeout = 30 * time.Second

// ErrNoModelSource 引擎没有「动态模型清单」来源（而非探测失败）。
// ListModels 返回的错误里用 %w 包住它，调用方可据此区分语义。
var ErrNoModelSource = errors.New("no dynamic model source")

// ModelLister 可选接口：引擎能动态列出当前可用模型标识。
//
// 只有「自家 CLI/配置里存在权威清单」的引擎才实现它；未实现的引擎
// （目前无）在 --engines 里只有 models_note 说明。返回值即 -m 可用的
// 模型标识（顺序 = CLI 自己的展示顺序，已去重去空）。
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}

// ModelListerOf 取引擎的模型探测能力；未实现返回 nil。
func ModelListerOf(e Engine) ModelLister {
	l, _ := e.(ModelLister)
	return l
}

// probeCLI 执行只读探测子命令并返回 stdout（stderr 只在失败时并入错误信息）。
func probeCLI(ctx context.Context, engine, bin string, args ...string) (string, error) {
	if bin == "" {
		return "", fmt.Errorf("%s CLI not found", engine)
	}
	stdout, stderr, err := runCLI(ctx, bin, args...)
	if err != nil {
		return "", wrapCliError(engine, stdout, stderr, err)
	}
	return stdout, nil
}

// dedupeModels 去空、去首尾空白、按首次出现顺序去重。
func dedupeModels(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// ── JSON 类输出的通用解析 ────────────────────────────────────────

// jsonModelsArray 从 CLI 的 JSON 输出里取「模型对象数组」。
// 兼容两种形态：顶层数组（trae）与 {"models":[...]} 包裹（codex / openclaw）。
// 输出头部的日志噪声由 firstJSONValue 自动跳过。
func jsonModelsArray(stdout string) ([]map[string]any, error) {
	raw, ok := firstJSONValue(stdout)
	if !ok {
		return nil, errors.New("no JSON object/array in output")
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(raw), &arr); err == nil {
		return arr, nil
	}
	var wrapped struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapped); err != nil {
		return nil, fmt.Errorf("unmarshal model list: %w", err)
	}
	return wrapped.Models, nil
}

// modelNamesFromObjects 按候选字段名依次取值（第一个非空命中），保持原顺序。
func modelNamesFromObjects(objs []map[string]any, fields ...string) []string {
	out := make([]string, 0, len(objs))
	for _, obj := range objs {
		for _, f := range fields {
			if v, ok := obj[f].(string); ok && strings.TrimSpace(v) != "" {
				out = append(out, v)
				break
			}
		}
	}
	return dedupeModels(out)
}

// firstJSONValue 返回 s 中第一个**能被 json.Valid 接受**的对象/数组字面量。
//
// 不能用"首个 '[' 或 '{'"直接定起点：CLI 会把日志混进 stdout，比如
// openclaw 的 "[飞书插件] 已注册…"（首个 '[' 属于日志，不是 JSON）。
// 因此逐个候选起点做括号配对 + json.Valid 校验，取第一个真正合法的。
func firstJSONValue(s string) (string, bool) {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '{' || c == '[' {
			if raw, ok := scanBalanced(s[i:], c); ok && json.Valid([]byte(raw)) {
				return raw, true
			}
		}
	}
	return "", false
}

// scanBalanced 从 s[0]（= open 括号）起做括号配对，返回配平的字面量。
// 字符串内的括号通过 inStr 状态跳过（含反斜杠转义）。
func scanBalanced(s string, open byte) (string, bool) {
	closeCh := byte('}')
	if open == '[' {
		closeCh = ']'
	}
	depth, inStr, escape := 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch c {
			case '\\':
				escape = !escape
				continue
			case '"':
				if !escape {
					inStr = false
				}
				escape = false
				continue
			default:
				escape = false
				continue
			}
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}
	return "", false
}

// ── 各引擎私有格式的解析（纯函数，便于单测）──────────────────────

// parseCodebuddyHelpModels 从 `codebuddy --help` 的 --model 描述里抽清单：
//
//	--model <model>  Model for the current session. Please provide the model ID.
//	                 Currently supported: (auto, hy4-preview, hy3, ...)
//
// 清单由 CLI 自己维护（含用户自定义的 custom-local:* 项），随版本自动更新。
func parseCodebuddyHelpModels(help string) []string {
	const marker = "Currently supported:"
	i := strings.Index(help, marker)
	if i < 0 {
		return nil
	}
	rest := help[i+len(marker):]
	if j := strings.IndexByte(rest, '('); j >= 0 {
		// 括号内的清单；跨行时也截到配对 ')'。
		if k := strings.IndexByte(rest[j:], ')'); k >= 0 {
			rest = rest[j+1 : j+k]
		} else {
			rest = rest[j+1:]
		}
	} else if k := strings.IndexByte(rest, '\n'); k >= 0 {
		rest = rest[:k]
	}
	return dedupeModels(strings.Split(rest, ","))
}

// parseLLMModelsText 解析 `llm models` 的纯文本输出：
//
//	OpenAI Chat: gpt-4o (aliases: 4o)
//	OpenRouter: x-ai/grok-4
//
// 取「模型 id」：先剥 " (aliases: ...)" 尾巴，再取最后一个 ": " 之后的部分。
func parseLLMModelsText(stdout string) []string {
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		ln := strings.TrimSpace(line)
		if ln == "" {
			continue
		}
		if i := strings.Index(ln, " (aliases: "); i >= 0 {
			ln = ln[:i]
		}
		if i := strings.LastIndex(ln, ": "); i >= 0 {
			ln = ln[i+2:]
		}
		out = append(out, ln)
	}
	return dedupeModels(out)
}

// claudeModelsFromSettings 从 Claude Code 的 settings.json 抽模型标识：
// 顶层 model + env 里所有「ANTHROPIC_ 开头、MODEL / MODEL_NAME 结尾」的字符串值
// （env 里混有非字符串值，如 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1，须跳过）。
func claudeModelsFromSettings(data []byte) ([]string, error) {
	var s struct {
		Model string         `json:"model"`
		Env   map[string]any `json:"env"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse claude settings: %w", err)
	}
	out := make([]string, 0, len(s.Env)+1)
	if v := strings.TrimSpace(s.Model); v != "" {
		out = append(out, v)
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 固定顺序，便于 diff / 测试
	for _, k := range keys {
		if !strings.HasPrefix(k, "ANTHROPIC_") {
			continue
		}
		if !strings.HasSuffix(k, "MODEL") && !strings.HasSuffix(k, "MODEL_NAME") {
			continue
		}
		if v, ok := s.Env[k].(string); ok && strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return dedupeModels(out), nil
}
