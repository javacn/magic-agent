package agent

// models.go - 引擎「支持的模型清单」的动态探测（--engines 使用）。
//
// 核心约束：模型清单**一律不硬编码**。每个引擎只用它自己的权威入口
// 现取现算，CLI 升级 / 用户注册新模型后 --engines 自动跟随：
//
//	引擎      动态来源
//	claude    ~/.claude/settings.json（顶层 model + env 里 ANTHROPIC_*MODEL /
//	          CLAUDE_CODE_*MODEL 这些 **id 格**）①
//	codebuddy 扩展来源链（两端同链 ③）：客户端合并配置缓存 acc-product-config
//	          → 远程配置缓存 ∪ App 包 product.json → `codebuddy --help` 的
//	          "Currently supported: (...)" 清单（末级兜底）
//	trae      `trae-cli models --json`
//	llm       `llm models`（用户经 llm CLI 自注册的模型）
//	codex     `codex debug models`（raw model catalog）
//	openclaw  `openclaw models list --json`
//	arkclaw   无 —— 模型由网关按 claw_id 绑定，返回 ErrNoModelSource ②
//	dsh       `$DSH_HOME/settings.yaml` 里已配置的模型（llm-pi-ai.providers.<route>.models，
//	          无则回落 agent-default-model）；输出 `route/model`，SDK 通道下就是 -m 的可取值
//
// ① claude 没有 models 子命令（--help 只有 agents/auth/doctor/mcp/plugin/
//    project/... ），可用的模型标识只能从用户配置里读：settings.json 的
//    env 键是 claude 自己的契约 —— 每个档位一组四个变量
//    （ANTHROPIC_DEFAULT_{SONNET,OPUS,HAIKU,FABLE...}_MODEL = 模型 id，
//    _MODEL_NAME = 显示名，另有 _MODEL_DESCRIPTION / _MODEL_SUPPORTED_CAPABILITIES），
//    同形态还有 ANTHROPIC_MODEL / ANTHROPIC_SMALL_FAST_MODEL /
//    CLAUDE_CODE_SUBAGENT_MODEL。这里按「命名空间 ANTHROPIC_ / CLAUDE_CODE_ +
//    后缀 MODEL」过滤键名（**只取 id 格**，显示名不算），不写死具体模型名。
// ② 探测失败/无来源都不算致命：CLI 层把原因写进 models_note，models 字段
//    留空（见 cli/root.go runEngines）。
// ③ codebuddy 两端的清单链（2026-09-21 定稿，2026-09-28 两端对齐）：客户端合并
//    配置缓存 <appHome>/cache/acc-product-config-v*.json（客户端模型选择器同源；
//    AI 端 27 条 = 23 预制 + 4 custom-local，WorkBuddy 端 61 条 = 53 预制 +
//    8 custom-local）
//    → 客户端未运行过时回退「远程配置缓存 ∪ App 包 product.json」超集近似
//    → 回退 --help。**两端都走这条链**：WorkBuddy 端的 --help 只有 17 条
//    （9 预制 + 8 custom-local），官方主力模型 hy3 / hy4-preview /
//    deepseek-v4.1-flash / glm-5.3 全不在内，只按 --help 出清单会大面积缺失
//    （且与同引擎的 model_credits 读自不同文件，倍率查得到、清单查不到）。
//
// 各引擎的 ListModels 都是**只读**子命令（不发起推理、不消耗额度），
// 但会真的启动 CLI 进程（openclaw ~2.5s 最慢），因此探测并发执行、
// 单引擎超时 DefaultModelProbeTimeout（见 cli/root.go）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// ModelCreditLister 可选接口：引擎能给出各模型的**积分倍率**（与客户端计费口径
// 一致）。返回 map[模型id]倍率，倍率是规范化后的数字字符串（客户端的
// "x0.34 credits" / "x0.77" → "0.34" / "0.77"）；客户端不给倍率的模型
// （如 custom-local）不出现在 map 里。探测失败 / 无来源返回 nil。
type ModelCreditLister interface {
	ModelCredits(ctx context.Context) map[string]string
}

// ModelCreditListOf 返回引擎的 ModelCreditLister 实现（未实现返回 nil）。
func ModelCreditListOf(e Engine) ModelCreditLister {
	l, _ := e.(ModelCreditLister)
	return l
}

// parseCreditMultiplier 把客户端计费字段规范化成纯倍率数字字符串：
//
//	"x2.20 credits" -> "2.20"    "x0.77" -> "0.77"    "  X1.5 CREDITS " -> "1.5"
//
// 其他形态（空 / 缺 x / 非数字）返回 ""（该模型不计入倍率表）。
func parseCreditMultiplier(raw string) string {
	s := strings.TrimSpace(strings.ToLower(raw))
	s = strings.TrimSuffix(strings.TrimSpace(s), "credits")
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "x")
	if s == "" {
		return ""
	}
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return ""
	}
	return s
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

// productJSONPath 从 CLI 二进制位置推导同 App 包内的 product.json：
//
//	<App>.app/Contents/Resources/app.asar.unpacked/cli/bin/codebuddy
//	  -> <App>.app/Contents/Resources/app.asar.unpacked/cli/product.json
//
// 即上溯两层（bin/codebuddy → bin → cli）再拼 product.json。
// 路径不匹配时返回的路径不存在，readProductJSONModels 会静默返回 nil。
func productJSONPath(bin string) string {
	if bin == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(filepath.Dir(bin)), "product.json")
}

// readProductJSONModels 读取并解析 App 包 product.json 的模型清单。
// 任何失败（文件不存在 / 权限 / 格式变了）都返回 nil，由调用方回退 --help
// —— product.json 是可选增强来源，不是硬依赖。
func readProductJSONModels(path string) []string {
	ids, _ := readProductJSONCatalog(path)
	return ids
}

// readProductJSONCatalog 同上，但同时返回积分倍率表（parseProductJSONCatalog）。
func readProductJSONCatalog(path string) ([]string, map[string]string) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	return parseProductJSONCatalog(data)
}

// parseProductJSONCatalog 抽取 product.json 形态配置的模型 id 与积分倍率：
//
//	{"models":[{"id":"gpt-5.5","name":"GPT-5.5","credits":"x3.31 credits",...},...]}
//
// 只取 id 与 credits（其余元数据 --engines 用不上）；保留文件内顺序；
// id 去重；倍率经 parseCreditMultiplier 规范化，缺省/非法的不进倍率表。
//
// ⚠️ 这里**不过滤**：product.json 的 credits 覆盖不完整（WorkBuddy 端实测 48 条里
// 只有 9 条带 credits，hy3 / minimax-m3 / glm-5.1 等主力模型都在另外 39 条里），
// 按「有倍率」筛会把真模型全砍掉。acc 缓存那份才够格过滤，见 readAccCatalog。
func parseProductJSONCatalog(data []byte) ([]string, map[string]string) {
	var doc struct {
		Models []struct {
			ID      string `json:"id"`
			Credits string `json:"credits"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil
	}
	var ids []string
	credits := map[string]string{}
	for _, m := range doc.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		ids = append(ids, id)
		if c := parseCreditMultiplier(m.Credits); c != "" {
			credits[id] = c
		}
	}
	ids = dedupeModels(ids)
	if len(ids) == 0 {
		// 空清单按「无来源」处理，调用方据此回退 --help（而非当成零个模型）。
		return nil, nil
	}
	return ids, credits
}

// readAccCatalog 读 acc-product-config 缓存，返回清单与倍率表。
//
// acc 缓存是客户端的**全量注册表**（WorkBuddy 端实测 61 条），不是模型选择器 ——
// 直接铺给 -m 会比客户端多太多。allowlist 是客户端显式下发的选择器清单
// （agents[].models，见 agentSelectorModels），为 nil 时**不过滤**（宁可多不可少）。
//
// 三类条目才进 -m 清单：
//
//  1. 在 allowlist 里（客户端展示的具名模型）→ 保留，倍率取注册表；
//  2. custom-local:*（用户自己加的，客户端同样展示、只是不计倍率）→ 保留；
//  3. 其余（补全 codewise-* / 图像 hunyuan-image-* / 历史别名 default-1.* 等）→ 剔除。
//
// ⚠️ "auto" 特例：客户端选择器首条，但**不在注册表里**（注册表只列具名模型）——
// CLI 的 --help 认、客户端确实展示，故按 allowlist 命中处理并补进清单首位。
//
// 返回空清单 = 当无来源，调用方回退 ②（远程配置缓存 ∪ product.json）。
func readAccCatalog(path string, allowlist []string) ([]string, map[string]string) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var doc struct {
		Models []struct {
			ID      string `json:"id"`
			Credits string `json:"credits"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil
	}
	on := map[string]bool{}
	for _, id := range allowlist {
		on[id] = true
	}
	var ids []string
	credits := map[string]string{}
	seen := map[string]bool{}
	// ① auto 等「在 allowlist 但不在注册表」的条目，按清单顺序补在前面。
	//    仅当该 id 是客户端合成项（auto 等少数特例，客户端用、注册表不列）才补；
	//    注册表为空时更不能拿选择器里随便一条顶替（那相当于把注册表的选择器
	//    当 ② 用，违反「注册表权威」语义 —— 应当回退 ② 拿真实数据）。
	for _, id := range allowlist {
		if seen[id] {
			continue
		}
		if !inAccRegistry(doc.Models, id) && isSyntheticSelectorID(id) {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, m := range doc.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		// ② 用户自定义模型：客户端同样展示，只是不计倍率。
		custom := strings.HasPrefix(id, "custom-local:")
		// ③ 具名模型：只在客户端选择器清单里才展示。
		if !custom && !(len(allowlist) > 0 && on[id]) {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		if rate := parseCreditMultiplier(m.Credits); rate != "" {
			credits[id] = rate
		}
	}
	ids = dedupeModels(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	return ids, credits
}

// isSyntheticSelectorID 判断某个不在 acc 注册表里的 id 是否是客户端的合成项（始终保留）。
//
// 实测 acc 注册表**只列具名模型**，不列客户端用作"自动选择"的合成项。最常见的就是
// `auto` —— 客户端用它表示「由后端挑模型」，CLI 的 --help 也认。
//
// ⚠️ 此名单必须保守：宁可漏掉一两个真合成项（表现为客户端能选、-m 取不到），
// 也不要把具名模型 id 误判进合成清单（那会让它绕过过滤、无限兜底进清单）。
func isSyntheticSelectorID(id string) bool {
	return id == "auto"
}

// inAccRegistry 判断某 id 是否在 acc 注册表里（区分「客户端列了但注册表没有」）。
func inAccRegistry(models []struct {
	ID      string `json:"id"`
	Credits string `json:"credits"`
}, id string) bool {
	for _, m := range models {
		if strings.TrimSpace(m.ID) == id {
			return true
		}
	}
	return false
}

// parseProductJSONModels 只取清单（积分倍率见 parseProductJSONCatalog）。
func parseProductJSONModels(data []byte) []string {
	ids, _ := parseProductJSONCatalog(data)
	return ids
}

// readRemoteConfigCacheModels 读取 CodeBuddy 远程配置的本地缓存，返回其中
// models[].id 的并集（积分倍率见 remoteConfigCacheCatalog）。
func readRemoteConfigCacheModels(localStorageDir string) []string {
	ids, _ := remoteConfigCacheCatalog(localStorageDir)
	return ids
}

// remoteConfigCacheCatalog 同上，但同时返回各模型的积分倍率表。
//
// 客户端模型选择器的真实数据源是网关（CloudProductManager 经 /v3/config）下发的
// 远程配置，落地在 <config_dir>/local_storage/entry_*.info：
//
//	[{"userId":"...","data":{"models":[{"id":"deepseek-v4.1-flash","credits":"x0.03",...},...],...}}, ...]
//
// 多账号条目取并集；同一模型多账号倍率不同时取先出现者。同目录还有其他形态的
// 条目（feature-flags 对象、base64+gzip 大对象、坏文件），解析不进上面的结构
// 就跳过。目录不存在 / 无可用条目 → (nil, nil)（调用方回退下一来源）。
func remoteConfigCacheCatalog(localStorageDir string) ([]string, map[string]string) {
	if localStorageDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(localStorageDir)
	if err != nil {
		return nil, nil
	}
	var ids []string
	credits := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".info") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(localStorageDir, e.Name()))
		if err != nil {
			continue
		}
		var docs []struct {
			Data struct {
				Models []struct {
					ID      string `json:"id"`
					Credits string `json:"credits"`
				} `json:"models"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &docs) != nil {
			continue
		}
		for _, doc := range docs {
			for _, m := range doc.Data.Models {
				id := strings.TrimSpace(m.ID)
				if id == "" {
					continue
				}
				ids = append(ids, id)
				if c := parseCreditMultiplier(m.Credits); c != "" {
					if _, dup := credits[id]; !dup {
						credits[id] = c
					}
				}
			}
		}
	}
	ids = dedupeModels(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	return ids, credits
}

// agentSelectorModels 从远程配置缓存里抽**客户端模型选择器真正展示的清单**
// —— agents[].models 数组：
//
//	[{"userId":"...","data":{"agents":[{"modelTags":["craft"],
//	   "models":["auto","hy4-preview","hy3","deepseek-v4.1-flash",...]}]}}]
//
// 这份清单是**显式下发**的，比「按倍率猜」准得多：倍率只说明客户端能计费，
// 不等于会展示 —— hy3-b / hy3-c / hy4-preview-dev / minimax-m2.5 / kimi-k2.5 /
// glm-4.6 都有倍率也都在注册表里，但客户端选择器里没有（它们是灰度 / 按
// modelTags 分档放的）。反之补全类（supportsExtra=true）连倍率都没有。
//
// ⚠️ **只取 mtime 最新的那一个条目**，不跨条目/跨账号取并集：共享 ~/.codebuddy 里
// 可能同时躺着多个账号的缓存（实测 WorkBuddy 端 d43e… 缓存里 userId=f231e9af 的
// 清单用的是 hy4-preview-f，而 ed6c16d4 的是 hy4-preview —— 并集会把上一账号的
// 型号混进来）。最新条目就是当前登录账号刚下发的。
//
// 目录不存在 / 无 agents 字段 / 解析失败 → (nil, false)：**没有权威清单**，
// 调用方据此跳过交集过滤、保留 acc 缓存全量（宁可多不可少，见 extendedModelCatalog）。
//
// ⚠️ auto：客户端清单里首条是 "auto"（自动选模型），它**不在 acc 缓存注册表里**
// （注册表只列具名模型）但 CLI 的 --help 认、且客户端确实展示，故调用方须放行。
func agentSelectorModels(localStorageDir string) ([]string, bool) {
	if localStorageDir == "" {
		return nil, false
	}
	entries, err := os.ReadDir(localStorageDir)
	if err != nil {
		return nil, false
	}
	type hit struct {
		mod time.Time
		ids []string
	}
	var best hit
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".info") {
			continue
		}
		p := filepath.Join(localStorageDir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var docs []struct {
			Data struct {
				Agents []struct {
					Models []string `json:"models"`
				} `json:"agents"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &docs) != nil {
			continue
		}
		var ids []string
		for _, doc := range docs {
			for _, a := range doc.Data.Agents {
				for _, m := range a.Models {
					if id := strings.TrimSpace(m); id != "" {
						ids = append(ids, id)
					}
				}
			}
		}
		ids = dedupeModels(ids)
		if len(ids) == 0 {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if best.ids == nil || fi.ModTime().After(best.mod) {
			best = hit{mod: fi.ModTime(), ids: ids}
		}
	}
	if best.ids == nil {
		return nil, false
	}
	return best.ids, true
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

// claudeModelEnvPrefixes Claude Code 里「模型 id」变量所在的命名空间（两个）：
//
//	ANTHROPIC_    ANTHROPIC_MODEL / ANTHROPIC_SMALL_FAST_MODEL /
//	              ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS}_MODEL …
//	CLAUDE_CODE_  CLAUDE_CODE_SUBAGENT_MODEL（子代理用的模型 id）
//
// 为什么不写死具体变量名：档位是可扩展的（本机就出现了 ANTHROPIC_DEFAULT_FABLE_MODEL，
// 二进制里只有 HAIKU/SONNET/OPUS），按「命名空间 + MODEL 结尾」过滤更耐用。
// ⚠️ 同一组的 `*_MODEL_NAME` / `*_MODEL_DESCRIPTION` / `*_MODEL_SUPPORTED_CAPABILITIES`
// 是**显示名/描述/能力位**，不是 id，被「MODEL 结尾」这条挡掉（见下）。
var claudeModelEnvPrefixes = []string{"ANTHROPIC_", "CLAUDE_CODE_"}

// claudeModelsFromSettings 从 Claude Code 的 settings.json 抽**模型标识**（-m 可取值）：
// 顶层 model + env 里「ANTHROPIC_ / CLAUDE_CODE_ 开头、且以 MODEL 结尾」的字符串值。
//
// 为什么只认 **MODEL 结尾**（2026-09-28 更正）：Claude Code 的 model 类 env 是
// **一组四个**变量（键名取自 CLI 二进制内的字符串）：
//
//	ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS}_MODEL                       ← 模型 id
//	ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS}_MODEL_NAME                  ← 显示名（界面标签）
//	ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS}_MODEL_DESCRIPTION           ← 描述
//	ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS}_MODEL_SUPPORTED_CAPABILITIES ← 能力位
//
// 只有 id 那一格算「模型」；`*_MODEL_NAME` 是给人看的标签
// （本机那份里是 GLM-5.2 / kimi-k2.6 / deepseek-v4-pro —— 实测「能跑通」不等于
// 「是模型 id」，路由可能只是回落到默认模型，所以只按命名契约取 id 格）。
//
// env 里混有非字符串值（如 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1），须跳过。
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
		if !hasAnyPrefix(k, claudeModelEnvPrefixes) {
			continue
		}
		// 只取 id 格：MODEL 结尾。`*_MODEL_NAME` 是显示名，不是可取值。
		if !strings.HasSuffix(k, "MODEL") {
			continue
		}
		if v, ok := s.Env[k].(string); ok && strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return dedupeModels(out), nil
}

// hasAnyPrefix 判断 s 是否以 prefixes 中任一项开头。
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
