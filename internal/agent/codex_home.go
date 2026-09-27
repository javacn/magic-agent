package agent

// codex_home.go - codex 引擎的 CODEX_HOME 隔离。
//
// 根因（2026-09-16 实测，codex-cli 0.154.0）：
//
//	~/.codex/auth.json 存在 ChatGPT 登录态（auth_mode="chatgpt"）时，
//	codex exec 启动会拉取 cloud config bundle（企业云端配置），目标域名
//	auth.openai.com。若系统 DNS 解析该域名失败/超时（本机实测 curl 8s
//	无响应，但 --resolve 指定 Cloudflare IP 后 0.9s 200 OK，纯 DNS 路径
//	问题），codex 在 15s 后报错退出：
//
//	  Error: timed out waiting for cloud config bundle after 15s
//
//	且 -c cloud_config.enabled=false / -c cloud_config=false 均无法绕过
//	（0.154 无此配置键）。config.toml 用的 custom provider（本地代理
//	127.0.0.1:15721）根本不需要 ChatGPT 登录态 —— auth.json 是唯一触发点。
//
// 修复：magic-agent 为 codex 子进程注入独立的 CODEX_HOME（镜像目录），
// 只同步 config.toml、AGENTS.md 及 config 引用的相对路径数据文件
// （如 cc-switch 的 model_catalog_json），**绝不同步 auth.json**。
// 无登录态 → 不拉 cloud config → exec 直连 custom provider 正常返回。
//
// 行为矩阵：
//
//	外部已设 CODEX_HOME                 → 尊重用户环境，不做隔离/同步
//	外部已设 MAGIC_AGENT_CODEX_HOME     → 直接用该目录（跳过自动同步）
//	~/.codex 不存在                     → 不启用隔离（保持原行为）
//	默认                                → 镜像 ~/.magic-agent/codex-home/
//
// 同步策略：源 config.toml 的 size+mtime 与镜像不一致时全量重同步
//（文件均为 KB 级，开销可忽略）。镜像内 sessions 由 codex 自行落盘，
// 续接（resume）天然工作在同一镜像内，自洽。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// magicAgentHomeDir 引擎私有数据的根目录（~/.magic-agent）。
func magicAgentHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".magic-agent")
}

// defaultCodexSourceHome 用户原始 codex 配置目录。
func defaultCodexSourceHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// codexHomeEnvKey 引擎级 CODEX_HOME 覆盖开关。
const codexHomeEnvKey = "MAGIC_AGENT_CODEX_HOME"

// ensureCodexHome 决定 codex 子进程的 CODEX_HOME，必要时构建镜像目录。
//
// 返回 (home, injected, error)：injected=true 表示调用方须将该 home 以
// CODEX_HOME 注入子进程环境。
func ensureCodexHome() (string, bool, error) {
	// 用户显式指定引擎 CODEX_HOME：直接用，不同步。
	if v := os.Getenv(codexHomeEnvKey); v != "" {
		return v, true, nil
	}
	// 用户已在环境中管理 CODEX_HOME：尊重，不覆盖。
	if os.Getenv("CODEX_HOME") != "" {
		return "", false, nil
	}
	src := defaultCodexSourceHome()
	if src == "" {
		return "", false, nil
	}
	srcConfig := filepath.Join(src, "config.toml")
	if _, err := os.Stat(srcConfig); err != nil {
		// 没有用户级 codex 配置：无需隔离（auth.json 若存在也无法
		// 触发 cloud config —— 没有自定义 provider 时 codex 行为不变）。
		return "", false, nil
	}
	root := magicAgentHomeDir()
	if root == "" {
		return "", false, nil
	}
	dst := filepath.Join(root, "codex-home")
	if err := syncCodexHome(src, dst); err != nil {
		// 同步失败不阻断调用：回退到无隔离（旧行为）。
		return "", false, fmt.Errorf("codex: sync CODEX_HOME: %w", err)
	}
	return dst, true, nil
}

// syncCodexHome 把源 CODEX_HOME 的必要文件镜像到 dst。
// 镜像文件写入后回写源的 mtime，使 size+mtime 比对成为有效的幂等判据。
func syncCodexHome(src, dst string) error {
	srcConfig := filepath.Join(src, "config.toml")
	dstConfig := filepath.Join(dst, "config.toml")
	if si, ei, err := statBoth(srcConfig, dstConfig); err == nil && sameStat(si, ei) {
		return nil // 已同步
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	// 1) config.toml 本体
	if err := copyPreservingMtime(srcConfig, dstConfig); err != nil {
		return err
	}
	// 2) config.toml 引用的相对路径数据文件（如 model_catalog_json）
	//    + 全局 AGENTS.md（codex exec 每轮都会读取的全局指令）。
	data, err := os.ReadFile(srcConfig)
	if err != nil {
		return err
	}
	names := relativeFileRefs(string(data))
	if _, err := os.Stat(filepath.Join(src, "AGENTS.md")); err == nil {
		names = append(names, "AGENTS.md")
	}
	for _, name := range names {
		s := filepath.Join(src, name)
		d := filepath.Join(dst, name)
		if err := copyPreservingMtime(s, d); err != nil {
			if os.IsNotExist(err) {
				continue // 引用了但缺失：跳过，让 codex 自己报
			}
			return err
		}
	}
	return nil
}

// copyPreservingMtime 拷贝单个文件并回写源 mtime（幂等同步的关键）。
func copyPreservingMtime(src, dst string) error {
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		return err
	}
	return os.Chtimes(dst, si.ModTime(), si.ModTime())
}

// relativeFileRefs 提取 config.toml 中「数据文件名」形态的字符串值。
// 规则：basic string 值不含 '/' 与 '\\'（非路径）、带文件扩展名
// （过滤 model 名 / token / 枚举值等纯 token）、非空；auth.json 永不
// 同步（cloud config 触发点，隔离的意义所在）。引用了但源不存在的文件
// 由 syncCodexHome 阶段自然跳过。
var tomlStringValRe = regexp.MustCompile(`"([^"\n\\]+)"`)

func relativeFileRefs(config string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range tomlStringValRe.FindAllStringSubmatch(config, -1) {
		v := m[1]
		switch {
		case v == "" || v == "auth.json":
			continue
		case containsAny(v, '/', '\\'):
			continue // 路径形态（多为绝对路径或跨目录引用），不处理
		case filepath.Ext(v) == "" || v == ".":
			continue // 无扩展名：model 名/枚举值/token，非文件名
		case seen[v]:
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func containsAny(s string, chars ...byte) bool {
	for i := 0; i < len(s); i++ {
		for _, c := range chars {
			if s[i] == c {
				return true
			}
		}
	}
	return false
}

// statBoth / sameStat 供同步比对用。
func statBoth(a, b string) (os.FileInfo, os.FileInfo, error) {
	si, err := os.Stat(a)
	if err != nil {
		return nil, nil, err
	}
	ei, err := os.Stat(b)
	if err != nil {
		return nil, nil, err
	}
	return si, ei, nil
}

func sameStat(a, b os.FileInfo) bool {
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}
