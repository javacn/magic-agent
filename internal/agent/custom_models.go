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
//
// ⚠️ 硬约束（2026-09-30 补）：目标文件**必须是顶层 JSON 对象**才动它。桌面端
// WorkBuddy 自己那份 `models.json` 顶层是**数组**（LanguageModel[]），与 CLI 的
// `{availableModels, models}` 不是同一个格式。若把数组当"解析失败的空文件"整体写回，
// 就是把对方的模型清单替换成只剩 custom-local 几条 —— 官方模型全丢且不可回滚。
// 撞上这种文件一律**跳过并在 stderr 说明**；覆盖写之前另留 `.magic-agent.bak` 原文。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// customModelsFileName CLI 读自定义模型端点的文件名（见 CLI 的 getModelsPath）。
const customModelsFileName = "models.json"

// customModelsBackupSuffix 覆盖写之前留的原文备份后缀。
// 一次误写就能把模型清单清空，留一份可回滚的东西。
const customModelsBackupSuffix = ".magic-agent.bak"

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
	var raw0 []byte // 改动前的原文（覆盖写之前留备份）
	root := map[string]any{}
	switch b, err := os.ReadFile(path); {
	case err == nil:
		raw0 = b
		/* ⚠️ 只有**顶层是 JSON 对象**才认作 CLI 的 models.json。
		   桌面端（WorkBuddy）自己那份 models.json 顶层是**数组**（LanguageModel[]），
		   数组解析不进 map[string]any。以前这里把"解析失败"当空文件继续写回
		   {models, availableModels} —— 对数组格式来说那等于把对方的模型清单**整体替换**掉：
		   官方模型全没了，且不可恢复（原注释里"解析失败时不覆盖未识别的字段"对数组不成立，
		   数组根本装不下未知字段）。这种文件不是 CLI 的格式，一律不动。 */
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			fmt.Fprintf(os.Stderr, "magic-agent: 跳过自定义模型端点同步：%s 不是合法 JSON（保持原文件不动）\n", path)
			return 0, nil
		}
		m, ok := doc.(map[string]any)
		if !ok {
			fmt.Fprintf(os.Stderr, "magic-agent: 跳过自定义模型端点同步：%s 顶层不是 JSON 对象"+
				"（该文件不是 CLI 的 models.json 格式，覆盖写会毁掉里面的模型清单；保持原文件不动）\n", path)
			return 0, nil
		}
		root = m
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
	// 覆盖写之前留一份原文备份：一次误写就能清空模型清单，必须可回滚。
	if len(raw0) > 0 {
		_ = os.WriteFile(path+customModelsBackupSuffix, raw0, 0o600)
	}
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

// ── 事故还原：把被覆盖写坏的桌面端 models.json 修回来 ──────────
//
// 背景（2026-09-30 Windows 事故）：Windows 上 CODEBUDDY_CONFIG_DIR 可能指向桌面端
// 数据目录，于是上面的同步把桌面端那份**数组格式**的 models.json 覆盖成了
// `{availableModels, models}` 对象格式。桌面端解析不了 → 模型清单整个为空，
// 用户看到「WorkBuddy 的模型都丢失了」。
//
// 还原依据（实测比对同一台机器上的两份文件）：
//
//	桌面端数组条目   {id:"MiniMax-M3",   name, vendor, url, apiKey, ...}
//	我们写的对象条目 {id:"custom-local:MiniMax-M3", name, vendor, url, apiKey, ...}
//
// 即**同一批条目、id 差一个 custom-local: 前缀**。所以还原 = 把对象里的 models[]
// 原样搬成数组，只去掉 id 前缀。官方模型清单不在这个文件里（那是 product.json /
// acc 缓存的事，桌面端自己拉），所以这里不需要、也不该造官方条目。

// DesktopAppHomeOf 返回引擎对应的桌面端数据目录名（空 = 该引擎没有桌面端）。
func DesktopAppHomeOf(engine string) string {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "codebuddy":
		return ".workbuddy"
	case "codebuddy-ai":
		return ".workbuddy-ai"
	}
	return ""
}

// DesktopEngines 列出有桌面端数据目录的引擎（`--repair-models` 的可用值，供提示与校验共用）。
func DesktopEngines() []string { return []string{"codebuddy", "codebuddy-ai"} }

// RepairDesktopModels 修复被反向同步写坏的桌面端 models.json。
// 返回（是否真的做了修复, 给人看的说明, 错误）。没有可修的东西时返回 (false, 说明, nil)。
func RepairDesktopModels(engine string) (bool, string, error) {
	home := DesktopAppHomeOf(engine)
	if home == "" {
		return false, "", fmt.Errorf(
			"引擎 %q 没有对应的桌面端数据目录，没有可修的 models.json（支持：%s）",
			strings.TrimSpace(engine), strings.Join(DesktopEngines(), " / "))
	}
	dir, err := os.UserHomeDir()
	if err != nil || dir == "" {
		dir = os.Getenv("HOME")
	}
	if dir == "" {
		return false, "", fmt.Errorf("定位用户目录失败，无法找到桌面端 models.json")
	}
	return repairModelsFile(filepath.Join(dir, home))
}

// repairModelsFile 就地把 appHome/models.json 从「被覆盖写的对象」还原成桌面端数组。
//
// 只认**明确的坏形状**（顶层对象 + models[] 全是 custom-local:* 条目）才动手：
// 已经是数组、不是合法 JSON、或形状不认识，一律原样不动并说明原因。
func repairModelsFile(appHome string) (bool, string, error) {
	path := filepath.Join(appHome, customModelsFileName)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, fmt.Sprintf("%s 不存在，无需修复", path), nil
	case err != nil:
		return false, "", err
	}

	var doc any
	if uerr := json.Unmarshal(b, &doc); uerr != nil {
		return false, fmt.Sprintf("%s 不是合法 JSON，形状对不上，未改动", path), nil
	}
	if _, isArr := doc.([]any); isArr {
		return false, fmt.Sprintf("%s 已是桌面端的数组格式，无需修复", path), nil
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return false, fmt.Sprintf("%s 顶层既不是数组也不是对象，未改动", path), nil
	}
	raw, _ := root["models"].([]any)
	if len(raw) == 0 {
		return false, fmt.Sprintf("%s 里没有 models 数组，不像被我们覆盖写的形状，未改动", path), nil
	}

	out := make([]any, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			return false, fmt.Sprintf("%s 的 models 里混有非对象条目，未改动", path), nil
		}
		id, _ := m["id"].(string)
		if !strings.HasPrefix(id, customModelIDPrefix) {
			return false, fmt.Sprintf("%s 的 models 里混有非 %s 条目（可能不是我们写坏的），未改动",
				path, customModelIDPrefix), nil
		}
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		cp["id"] = strings.TrimPrefix(id, customModelIDPrefix)
		out = append(out, cp)
	}

	nb, merr := json.MarshalIndent(out, "", "  ")
	if merr != nil {
		return false, "", merr
	}
	nb = append(nb, '\n')

	// 先把现场（被写坏的那份）留一份，再落盘。
	broken := path + ".broken.bak"
	if werr := os.WriteFile(broken, b, 0o600); werr != nil {
		return false, "", fmt.Errorf("写备份 %s 失败: %w", broken, werr)
	}
	if werr := os.WriteFile(path, nb, 0o600); werr != nil {
		return false, "", werr
	}
	return true, fmt.Sprintf(
		"已还原 %s：%d 条自定义模型改回桌面端的数组格式（id 去掉 %s 前缀）；原文件备份在 %s",
		path, len(out), customModelIDPrefix, broken), nil
}
