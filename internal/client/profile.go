package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

/* Profile 是「一台机器上装了几个 magic-* 应用」之间**唯一的差异来源**。
 *
 * 设计约束（见 magic-client 方案设计）：profile **只描述差异，不描述结构** ——
 * 一旦里面开始出现布局或流程描述，它就退化成第二套代码。
 * 因此这里只留四类字段：应用身份（app）、界面入口（ui）、启用能力（capabilities / engines）、
 * 数据位置（storage），外加业务模块清单（modules）。
 */

// Profile 一个应用的集成配置。
type Profile struct {
	App struct {
		Name     string `json:"name"`
		BundleID string `json:"bundleId,omitempty"`
		Icon     string `json:"icon,omitempty"`
	} `json:"app"`
	UI struct {
		// Entry 界面入口（相对 UIRoot）。
		Entry string `json:"entry"`
		// Theme 主题令牌名（paper-ink 等），由基础版 UI 解释。
		Theme string `json:"theme,omitempty"`
		// DefaultView 启动视图。
		DefaultView string `json:"defaultView,omitempty"`
		// ThemeVars 主题令牌覆盖（键值对；键写 `--page-brand` 或 `page-brand` 都行）。
		// 插件把它注入页面根元素的 style，模块因此不必改基础版 UI 的样式文件。
		ThemeVars map[string]string `json:"themeVars,omitempty"`
		// ModulesDir 项目自己的模块资源目录（绝对路径，或相对 profile 文件所在目录）。
		// 插件把它挂到 `/modules/` 下 —— 于是业务模块的文件**留在项目仓库里**，
		// 不必塞进插件的 UI 目录。LoadProfile 会把它解析成绝对路径。
		ModulesDir string `json:"modulesDir,omitempty"`
	} `json:"ui"`
	// Backend 项目自己的本地后端：插件的 `/api/*` 原样转发到这里。
	//
	// 为什么是代理而不是插件内置存储：业务数据（需求、用例、产物、设置）的**真相在项目那边**，
	// 插件再建一份存储就是第二个真相源。有了它，业务视图可以逐块迁到插件侧而**不必重写业务逻辑**
	//（前端搬过去，后端照旧）。没有配置时 `/api/*` 回 501 并点名这个字段，不静默 404。
	Backend BackendConfig `json:"backend"`
	// Capabilities 启用的通用能力（chat / capture / browser / files / terminal）。
	// 空 = 只启用基础版 UI 自带的 chat。
	Capabilities []string `json:"capabilities,omitempty"`
	// Engines 允许出现的引擎白名单；空 = 全部（以 core 的 --contract 为准）。
	Engines []string `json:"engines,omitempty"`
	Storage struct {
		// Dir 数据目录（默认 ~/.magic-client/<app-id>）。
		Dir string `json:"dir,omitempty"`
		// DB 业务库文件名。
		DB string `json:"db,omitempty"`
	} `json:"storage"`
	// Modules 业务模块清单：插件只负责按需加载与提供接口，不含任何领域假设。
	Modules []Module `json:"modules,omitempty"`
}

// Module 一个业务模块的声明。
type Module struct {
	ID string `json:"id"`
	// Entry 模块前端入口（相对 UIRoot），由基础版 UI 按视图激活时机动态加载。
	Entry string `json:"entry,omitempty"`
	// DataTables 该模块自己的表；插件只提供存储接口，不解释它们的含义。
	DataTables []string `json:"dataTables,omitempty"`
}

// BackendConfig 项目自己的本地后端（`/api/*` 的转发目标）。
type BackendConfig struct {
	// URL 后端地址，如 http://127.0.0.1:8931。
	URL string `json:"url,omitempty"`
	// Token 转发时由**服务端注入**的凭据（不暴露给浏览器）。
	Token string `json:"token,omitempty"`
	// TokenHeader 注入用的头名；空 = Authorization: Bearer <token>。
	TokenHeader string `json:"tokenHeader,omitempty"`
}

// DefaultProfile 返回最小可用 profile（只有基础版 UI，没有业务模块）。
func DefaultProfile() Profile {
	var p Profile
	p.App.Name = "magic-client"
	p.UI.Entry = "index.html"
	p.UI.DefaultView = "chat"
	p.Capabilities = []string{"chat"}
	return p
}

// LoadProfile 读 profile。path 为空时返回 DefaultProfile。
//
// 取值优先级（与 core 的 MAGIC_AGENT_BIN 同风格）：显式 --profile > 用户目录覆盖文件 > 内置默认。
// 用户覆盖文件的命名是 profiles/<app-id>.json，app-id 由文件名决定。
func LoadProfile(path string) (Profile, error) {
	p := DefaultProfile()
	if strings.TrimSpace(path) == "" {
		return p, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return p, fmt.Errorf("读 profile 失败: %w", err)
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("解析 profile 失败（%s）: %w", path, err)
	}
	if strings.TrimSpace(p.UI.Entry) == "" {
		p.UI.Entry = "index.html"
	}
	if strings.TrimSpace(p.App.Name) == "" {
		p.App.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	// modulesDir 允许写成相对 profile 的路径 —— 这样 profile 可以跟着项目仓库走，
	// 换台机器/换个 checkout 位置都不用改。
	if d := strings.TrimSpace(p.UI.ModulesDir); d != "" && !filepath.IsAbs(d) {
		p.UI.ModulesDir = filepath.Join(filepath.Dir(absOr(path)), d)
	}
	return p, nil
}

// absOr 取绝对路径；失败时原样返回（调用方只用来拼相对路径）。
func absOr(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// AppID 应用标识：优先 bundleId，其次名字小写，用于数据目录与用户覆盖文件命名。
func (p Profile) AppID() string {
	if s := strings.TrimSpace(p.App.BundleID); s != "" {
		return s
	}
	name := strings.ToLower(strings.TrimSpace(p.App.Name))
	name = strings.NewReplacer(" ", "-", "/", "-", "\\", "-").Replace(name)
	if name == "" {
		return "magic-client"
	}
	return name
}

// DataDir 数据目录（~/.magic-client/<app-id>；profile 里给了就用给的）。
func (p Profile) DataDir() string {
	if s := strings.TrimSpace(p.Storage.Dir); s != "" {
		return expandHome(s)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "magic-client", p.AppID())
	}
	return filepath.Join(home, ".magic-client", p.AppID())
}

// HasCapability 该 profile 是否启用了某个通用能力。
func (p Profile) HasCapability(name string) bool {
	for _, c := range p.Capabilities {
		if strings.EqualFold(strings.TrimSpace(c), name) {
			return true
		}
	}
	return false
}

// AllowsEngine 引擎白名单判定（空名单 = 全放行）。
func (p Profile) AllowsEngine(name string) bool {
	if len(p.Engines) == 0 {
		return true
	}
	for _, e := range p.Engines {
		if strings.EqualFold(strings.TrimSpace(e), name) {
			return true
		}
	}
	return false
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
