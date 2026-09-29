package agent

// custom_models.go - 把桌面 App 里配好的**自定义模型端点**同步给独立 CLI。
//
// 背景（2026-09-28）：codebuddy 系引擎改用**独立安装**的 CLI 后，用户在 App 里配好的
// 自定义模型（`custom-local:*`）在 `-m` 里一律失败：
//
//	Custom model <id> has no endpoint url configured. Set the "url" field for
//	this model in your model settings (models.json) and try again.
//
// 根因：CLI 的自定义模型端点只从 `<配置目录>/models.json` 读，而桌面 App 是在
// **启动 CLI 时把端点（url / apiKey）注入子进程**的 —— 独立进程没有这份注入。
// 我们在 App 的合并配置缓存里能读到这些端点（与模型清单同源），补写一次即可。
//
// 为什么不选"把 custom-local 从清单里过滤掉"：那是把可用模型砍掉；补端点是让它们
// 真正可用。清单与端点同源（都取该引擎的 acc 缓存），不会出现"列了却没端点"。
//
// 归谁所有：models.json 是 **CLI 自己的**配置文件（CLI 的 models 控制器会读写它）。
// 所以这里只做**合并**：只补缺失的 id，不删、不改、不动其它字段；无新增时不落盘
//（避免无谓写盘与 mtime 抖动）。App 缓存一律只读。

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// customModelsFileName CLI 读自定义模型端点的文件名（见 CLI 的 getModelsPath）。
const customModelsFileName = "models.json"

// customModelIDPrefix 自定义模型 id 前缀（App 与 CLI 共用这个约定）。
const customModelIDPrefix = "custom-local:"

// syncCustomModelsFromApp 把 App 合并配置缓存（acc-product-config）里的
// `custom-local:*` 模型合并进 `<configDir>/models.json`，返回新增条数。
//
// accConfigPath 为空 / 文件不存在 / 里面没有自定义模型 → 返回 (0, nil)：这是常态，
// 不是错误（没装桌面 App、或用户没配自定义模型）。
//
// 失败语义：读 App 缓存失败视为"没得同步"（返回 0, nil，不阻断模型探测）；
// 只有**写** models.json 失败才返回错误 —— 调用方按 best-effort 处理即可。
func syncCustomModelsFromApp(accConfigPath, configDir string) (int, error) {
	if strings.TrimSpace(accConfigPath) == "" || strings.TrimSpace(configDir) == "" {
		return 0, nil
	}
	models := readAppCustomModels(accConfigPath)
	if len(models) == 0 {
		return 0, nil
	}

	path := filepath.Join(configDir, customModelsFileName)
	root := map[string]any{}
	switch b, err := os.ReadFile(path); {
	case err == nil:
		// 解析失败就当空文件：CLI 自己也会这么兜（readModelsJson 失败返回 {models:[]}）。
		// 但**不覆盖**未识别的字段 —— 解析失败时下面只会写回 models/availableModels。
		_ = json.Unmarshal(b, &root)
		if root == nil {
			root = map[string]any{}
		}
	case errors.Is(err, fs.ErrNotExist):
		// 首次同步：新建
	default:
		return 0, err
	}

	existing := map[string]bool{}
	raw, _ := root["models"].([]any)
	for _, it := range raw {
		if m, ok := it.(map[string]any); ok {
			if id, _ := m["id"].(string); id != "" {
				existing[id] = true
			}
		}
	}

	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 固定顺序，便于 diff

	added := 0
	for _, id := range ids {
		if existing[id] {
			continue
		}
		raw = append(raw, models[id])
		existing[id] = true
		added++
	}
	if added == 0 {
		return 0, nil // 已在位：不落盘
	}
	root["models"] = raw

	// availableModels 与 CLI 自己的 saveCustomModel 行为对齐（它每次都会维护这一项）。
	// 实测（2026-09-28）：只写 custom-local id 不会挤掉官方模型的 -m 可用性。
	avail := make([]string, 0, len(raw))
	seen := map[string]bool{}
	if old, ok := root["availableModels"].([]any); ok {
		for _, v := range old {
			if s, ok := v.(string); ok && s != "" && !seen[s] {
				seen[s] = true
				avail = append(avail, s)
			}
		}
	}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			avail = append(avail, id)
		}
	}
	root["availableModels"] = avail

	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return 0, err
	}
	// 含第三方 apiKey：与 CLI 自身凭据同级（仅属主可读）。
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return 0, err
	}
	return added, nil
}

// readAppCustomModels 从 App 的合并配置缓存里收集自定义模型条目（id → 原样条目）。
//
// 原样保留整个对象：CLI 的 models.json 条目就是它自己那套字段
// （id / name / url / apiKey / supportsImages / supportsReasoning / supportsToolCall /
// reasoning / maxInputTokens / maxOutputTokens / useCustomProtocol / vendor / tags），
// 逐字段挑写容易漏（实测同一条目在不同缓存里字段数不同）。
//
// 跳过 disabled 条目；解析失败返回 nil（视作没有）。
func readAppCustomModels(accConfigPath string) map[string]map[string]any {
	b, err := os.ReadFile(accConfigPath)
	if err != nil {
		return nil
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil
	}
	out := map[string]map[string]any{}
	collectCustomModels(doc, out)
	return out
}

// collectCustomModels 深度遍历合并配置缓存，挑出 custom-local:* 条目（先到先得）。
func collectCustomModels(node any, out map[string]map[string]any) {
	switch v := node.(type) {
	case map[string]any:
		if id, ok := v["id"].(string); ok && strings.HasPrefix(id, customModelIDPrefix) {
			if disabled, _ := v["disabled"].(bool); !disabled {
				if _, dup := out[id]; !dup {
					out[id] = v
				}
			}
		}
		for _, child := range v {
			collectCustomModels(child, out)
		}
	case []any:
		for _, child := range v {
			collectCustomModels(child, out)
		}
	}
}

// ensureCustomModelEndpoint 仅当本次请求用的是一个自定义模型时，才补一次端点。
//
// 为什么不止在 listModels 里同步：用户可能在 App 里刚加了一个自定义模型，就直接
// `-m custom-local:<新>` 调用（没打开过模型选择器）。这条兜底让那种路径也能用上。
// best-effort：不同步失败也不影响正常模型（官方模型根本不读 models.json）。
func (c codebuddyCore) ensureCustomModelEndpoint(model string) {
	if !strings.HasPrefix(model, customModelIDPrefix) {
		return
	}
	_, _ = syncCustomModelsFromApp(c.accConfigPath, c.effectiveConfigDir())
}

// effectiveConfigDir 本次子进程实际使用的配置目录：用户显式设了 CODEBUDDY_CONFIG_DIR
// 就跟着走（CLI 也这么读，见 codebuddyAccountEnv），否则用引擎自带的那个。
func (c codebuddyCore) effectiveConfigDir() string {
	if v := strings.TrimSpace(os.Getenv("CODEBUDDY_CONFIG_DIR")); v != "" {
		return v
	}
	return c.configDir
}
