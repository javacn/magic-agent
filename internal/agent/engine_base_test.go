package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaxTokensSettingsHelper maxTokensSettings 的载荷与边界：
// n<=0 视为未设置（ok=false，不注入 --settings）。
func TestMaxTokensSettingsHelper(t *testing.T) {
	if payload, ok := maxTokensSettings(0); ok {
		t.Errorf("0 应视为未设置，got payload=%q", payload)
	}
	if payload, ok := maxTokensSettings(-5); ok {
		t.Errorf("负数应视为未设置，got payload=%q", payload)
	}
	payload, ok := maxTokensSettings(8000)
	if !ok {
		t.Fatal("8000 应产生载荷")
	}
	want := `{"env":{"CLAUDE_CODE_MAX_OUTPUT_TOKENS":"8000"}}`
	if payload != want {
		t.Errorf("payload = %q, want %q", payload, want)
	}
}

// TestExpandHome 基类路径展开：仅处理 ~ 与 ~/ 前缀。
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("无法获取主目录: %v", err)
	}
	cases := []struct {
		in   string
		want string
	}{
		{"~/bin/x", filepath.Join(home, "bin/x")},
		{"~", home},
		{"~user/x", "~user/x"}, // 不支持 ~user 形式
		{"/opt/homebrew/bin", "/opt/homebrew/bin"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := expandHome(tc.in); got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNpmGlobalBinDirs npm 全局前缀解析：env（NPM_CONFIG_PREFIX / npm_config_prefix）
// 给出的前缀排在前，其后是**标准全局前缀兜底**（GUI 进程 PATH 窄时要靠它，
// 见 engine_base.go 的 npmGlobalBinDirs 注释；2026-09-28 为 codebuddy 独立 CLI 加的）。
func TestNpmGlobalBinDirs(t *testing.T) {
	t.Setenv("NPM_CONFIG_PREFIX", "")
	t.Setenv("npm_config_prefix", "")

	// 未设 env：只剩标准前缀兜底 —— 不能为空（否则 GUI 侧探测不到 /opt/homebrew/bin）
	base := npmGlobalBinDirs()
	if len(base) == 0 {
		t.Fatal("标准全局前缀兜底不该为空")
	}
	if base[0] != "/opt/homebrew/bin" {
		t.Errorf("标准前缀应排在首位（无 env 前缀时），got %v", base)
	}

	// 设了 env：env 前缀排在标准前缀**之前**（优先级不因新增兜底而变）
	t.Setenv("NPM_CONFIG_PREFIX", "/p/upper")
	got := npmGlobalBinDirs()
	if len(got) == 0 || got[0] != "/p/upper/bin" {
		t.Errorf("env 前缀应排首位, got %v", got)
	}
	var hasStd bool
	var upperCount int
	for _, d := range got {
		switch d {
		case "/opt/homebrew/bin":
			hasStd = true
		case "/p/upper/bin":
			upperCount++
		}
	}
	if !hasStd {
		t.Errorf("标准前缀应始终包含（env 存在也不能丢兜底）, got %v", got)
	}
	if upperCount != 1 {
		t.Errorf("同前缀应去重（出现 %d 次）, got %v", upperCount, got)
	}

	// 大写为空时小写单独生效
	t.Setenv("NPM_CONFIG_PREFIX", "")
	t.Setenv("npm_config_prefix", "/p/lower")
	if got := npmGlobalBinDirs(); len(got) == 0 || got[0] != "/p/lower/bin" {
		t.Errorf("got %v, want 首位 /p/lower/bin", got)
	}
}

// TestResolveNpmGlobalPrefixFallback 探测链最后一跳：候选与 PATH 都落空时用 npm 全局
// 前缀兜底。回归背景：沙箱把 `npm i -g` 的落点设成不在 PATH 里的私有目录，dsh 装好了
// 却报 not found（2026-09-22 真机复现）。
func TestResolveNpmGlobalPrefixFallback(t *testing.T) {
	prefix := t.TempDir()
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 用唯一名，避免撞上真实二进制或各引擎的候选路径
	const name = "magic-agent-test-cli-xyz"
	fake := filepath.Join(binDir, name)
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	base := cliBase{binName: name, envVar: "MAGIC_AGENT_TEST_XYZ_BIN"}
	t.Setenv("MAGIC_AGENT_TEST_XYZ_BIN", "")
	t.Setenv("NPM_CONFIG_PREFIX", prefix)
	t.Setenv("npm_config_prefix", "")
	t.Setenv("PATH", "") // 排除 LookPath，只留 npm 前缀这一跳

	if got := base.resolve(""); got != fake {
		t.Errorf("resolve() = %q, want %q（应靠 npm 全局前缀兜底）", got, fake)
	}

	// 前缀也关掉 → 如实找不到（证明上面的命中确实来自该跳，而非误报）
	t.Setenv("NPM_CONFIG_PREFIX", "")
	if got := base.resolve(""); got != "" {
		t.Errorf("无任何来源时 resolve() = %q, want 空", got)
	}
}

// TestResolveOrder npm 兜底不得改变既有优先级：显式参数 > 环境变量 > 候选 > PATH > npm 前缀。
func TestResolveOrder(t *testing.T) {
	prefix := t.TempDir()
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const name = "magic-agent-test-cli-abc"
	npmPath := filepath.Join(binDir, name)
	if err := os.WriteFile(npmPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	candDir := t.TempDir()
	candPath := filepath.Join(candDir, name)
	if err := os.WriteFile(candPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	base := cliBase{
		binName:    name,
		envVar:     "MAGIC_AGENT_TEST_ABC_BIN",
		candidates: []string{candPath},
	}
	t.Setenv("NPM_CONFIG_PREFIX", prefix)
	t.Setenv("npm_config_prefix", "")
	t.Setenv("PATH", "")

	t.Setenv("MAGIC_AGENT_TEST_ABC_BIN", "/explicit/env/path")
	if got := base.resolve(""); got != "/explicit/env/path" {
		t.Errorf("环境变量应优先于候选/npm, got %q", got)
	}
	if got := base.resolve("/explicit/arg"); got != "/explicit/arg" {
		t.Errorf("显式参数应最优先, got %q", got)
	}

	t.Setenv("MAGIC_AGENT_TEST_ABC_BIN", "")
	if got := base.resolve(""); got != candPath {
		t.Errorf("候选应优先于 npm 前缀, got %q want %q", got, candPath)
	}

	base.candidates = []string{filepath.Join(candDir, "nope")}
	if got := base.resolve(""); got != npmPath {
		t.Errorf("候选落空应落 npm 前缀, got %q want %q", got, npmPath)
	}
}

// TestInstallCommandOf 一键安装命令能力表：有官方安装命令的引擎必须给出可执行的一行，
// 没有安装路径的（GUI 应用 / 配置文件类）如实留空 —— 不编跑不通的命令。
func TestInstallCommandOf(t *testing.T) {
	// 有安装命令：非空 + 形如一行 shell（不含换行）。
	for _, engine := range []string{"claude", "codebuddy", "codebuddy-ai", "codex", "openclaw", "dsh", "trae", "llm"} {
		got := InstallCommandOf(engine)
		if got == "" {
			t.Errorf("InstallCommandOf(%q) 为空，应给出一键安装命令", engine)
			continue
		}
		if strings.ContainsAny(got, "\n\r") {
			t.Errorf("InstallCommandOf(%q) = %q，必须是一行", engine, got)
		}
	}
	// 具体命令取自各 CLI 官方安装方式（回归保护：防止手滑改坏包名）。
	cases := map[string]string{
		"claude":       "npm install -g @anthropic-ai/claude-code",
		"codebuddy":    "npm install -g @tencent-ai/codebuddy-code",
		"codebuddy-ai": "npm install -g @tencent-ai/codebuddy-code", // 同一个独立 CLI、两个账号
		"codex":        "npm install -g @openai/codex",
		"openclaw":     "npm install -g openclaw@latest",
		"dsh":          "npm i -g @deepseek-ai/dsh",
		"trae":         `sh -c "$(curl -L https://trae.cn/trae-cli/install.sh)"`,
		"llm":          "python3 -m venv ~/.llm-venv && ~/.llm-venv/bin/pip install llm",
	}
	for engine, want := range cases {
		if got := InstallCommandOf(engine); got != want {
			t.Errorf("InstallCommandOf(%q) = %q want %q", engine, got, want)
		}
	}
	// 没有可执行安装路径的引擎：留空（原因在 note 里）。
	for _, engine := range []string{"arkclaw", "codebuddy-gateway", "unknown", ""} {
		if got := InstallCommandOf(engine); got != "" {
			t.Errorf("InstallCommandOf(%q) = %q，应为空（无安装命令）", engine, got)
		}
	}
}

// TestToolsSwitchableOf `--tools` 的落地通道能力表：只有真有 CLI 参数的引擎为 true
// （dsh / openclaw / arkclaw 自带工具循环，llm 无工具概念）。
func TestToolsSwitchableOf(t *testing.T) {
	for engine, want := range map[string]bool{
		"claude":       true,
		"codebuddy":    true,
		"codebuddy-ai": true,
		"trae":         true,
		"codex":        true,
		"llm":          false,
		"openclaw":     false,
		"dsh":          false,
		"arkclaw":      false,
		"unknown":      false,
	} {
		if got := ToolsSwitchableOf(engine); got != want {
			t.Errorf("ToolsSwitchableOf(%q) = %v want %v", engine, got, want)
		}
	}
}

// TestCLIResolveEnvVar 探测链优先级：显式 BinPath > 环境变量 > 候选路径。
func TestCLIResolveEnvVar(t *testing.T) {
	t.Setenv("MAGIC_AGENT_LLM_BIN", "/custom/llm-path")
	if got := llmBase.resolve(""); got != "/custom/llm-path" {
		t.Errorf("env 覆盖未生效: %q", got)
	}
	// 显式路径优先于环境变量。
	if got := llmBase.resolve("/explicit/bin"); got != "/explicit/bin" {
		t.Errorf("显式路径应最优先: %q", got)
	}
}

/* ── 引擎版本读取（2026-09-23）──
 * 用户：「引擎检测除了 a2a 的 其他也要支持有升级」—— 版本号是「升级」可验证的前提
 * （重探一次比对旧新，才能如实说「已升级」或「版本未变」）。 */

func TestFirstSemver(t *testing.T) {
	cases := []struct{ in, want string }{
		// 实测样例（各 CLI 的真实输出形态）
		{"2.1.146 (Claude Code)", "2.1.146"},
		{"codex-cli 0.154.0", "0.154.0"},
		{"OpenClaw 2026.6.11 (e085fa1)", "2026.6.11"},
		{"0.1.5-rc.2", "0.1.5-rc.2"},
		{"trae-cli 0.120.52\n", "0.120.52"},
		{"1.2.3+build.7", "1.2.3+build.7"},
		// 认不出来就空串（**不编**）
		{"no version here", ""},
		{"", ""},
		// ⚠️ 两段的**不算**：CLI 输出里的 `1.0` / `3.12` 常是协议版本、窗口尺寸这类东西
		{"protocol 1.0", ""},
		// ⚠️ **只看第一行**：后面的行可能是路径里的别的版本号
		//（实测 llm 的 venv python 坏掉时，输出里满是 /…/python3.12.13/… 这种路径）
		{"Python path configuration:\n  /…/python3.12.13/…", ""},
		{"llm, version 0.27.1\n  /…/python3.12.13/…", "0.27.1"},
	}
	for _, c := range cases {
		if got := firstSemver(c.in); got != c.want {
			t.Errorf("firstSemver(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestEngineVersionOfSkipsNonExecutables(t *testing.T) {
	// A2A 网关的 bin 是 URL（本机没二进制）→ 不该去 spawn 它；空 bin 同理。
	if got := EngineVersionOf(context.Background(), "https://h/a2a/jsonrpc"); got != "" {
		t.Errorf("HTTP 端点应返回空串, got %q", got)
	}
	if got := EngineVersionOf(context.Background(), "   "); got != "" {
		t.Errorf("空 bin 应返回空串, got %q", got)
	}
}

func TestEngineVersionOfReadsCLIOutput(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-cli")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'fake-cli 9.8.7 (build abc)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := EngineVersionOf(context.Background(), bin); got != "9.8.7" {
		t.Errorf("got %q want 9.8.7", got)
	}
	// 认不出 --version 的 CLI（退出码非 0、输出没有版本号）→ 空串，不编
	bad := filepath.Join(dir, "bad-cli")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho 'unknown flag: --version' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := EngineVersionOf(context.Background(), bad); got != "" {
		t.Errorf("认不出时应返回空串, got %q", got)
	}
}
