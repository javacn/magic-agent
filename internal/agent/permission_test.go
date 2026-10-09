package agent

// permission_test.go - 四档权限模型（permission.go）的单元测试。
//
// 覆盖三件事：
//  1. 档位解析（含别名与数字档位）；
//  2. settings 载荷组装（sandbox / autoMode / permissions / env 的合并与取值）；
//  3. 各档位落到 claude / codebuddy 的 argv 上到底长什么样（参数映射的最终证据）。
//
// 最后再补一个假 CLI 端到端：证明这些参数真的被送进了子进程，而不只是
// buildArgsBase 的返回值好看。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── 档位解析 ──────────────────────────────────────────────────

func TestParsePermissionTier(t *testing.T) {
	cases := []struct {
		in   string
		want PermissionTier
		ok   bool
	}{
		// 规范名
		{"manual", PermissionManual, true},
		{"accept-edits", PermissionAcceptEdits, true},
		{"auto", PermissionAuto, true},
		{"full", PermissionFull, true},
		// 大小写 / 分隔符不敏感
		{"MANUAL", PermissionManual, true},
		{"Accept_Edits", PermissionAcceptEdits, true},
		{"acceptEdits", PermissionAcceptEdits, true},
		{" accept-edits ", PermissionAcceptEdits, true},
		// 别名
		{"default", PermissionManual, true}, // 与 claude 自己的命名一致
		{"ask", PermissionManual, true},
		{"edits", PermissionAcceptEdits, true},
		{"guardian", PermissionAuto, true},
		{"bypass", PermissionFull, true},
		{"bypassPermissions", PermissionFull, true},
		{"yolo", PermissionFull, true},
		// 数字档位
		{"1", PermissionManual, true},
		{"2", PermissionAcceptEdits, true},
		{"3", PermissionAuto, true},
		{"4", PermissionFull, true},
		// 非法
		{"", "", false},
		{"plan", "", false},    // plan 是正交的「阶段档」，不是四档之一
		{"dontAsk", "", false}, // dontAsk 是正交的「锁定档」
		{"nope", "", false},
	}
	for _, c := range cases {
		got, ok := ParsePermissionTier(c.in)
		if ok != c.ok {
			t.Errorf("ParsePermissionTier(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("ParsePermissionTier(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDefaultPermissionTierIsFull 默认档必须是 full。
//
// 这是**刻意**的设计：改造前 claude/codebuddy 在 --tools 非 off 时恒传
// --dangerously-skip-permissions / -y（= 第 4 档）。默认值一旦改动，既有调用方
// 的 agent 会突然开始弹审批 / 被沙箱拦 —— 那是静默的行为变更，不能发生。
func TestDefaultPermissionTierIsFull(t *testing.T) {
	if DefaultPermissionTier != PermissionFull {
		t.Fatalf("DefaultPermissionTier = %q, want %q（改默认档等于静默变更既有行为）",
			DefaultPermissionTier, PermissionFull)
	}
	if got := permissionOrDefault(""); got != PermissionFull {
		t.Errorf("permissionOrDefault(\"\") = %q, want %q", got, PermissionFull)
	}
}

// TestClaudePermissionModeMapping 档位 → --permission-mode 取值的映射。
// 取值必须是 CLI 认的字面量（camelCase 的 acceptEdits 尤其容易写错）。
func TestClaudePermissionModeMapping(t *testing.T) {
	cases := map[PermissionTier]string{
		PermissionManual:      "default",
		PermissionAcceptEdits: "acceptEdits",
		PermissionAuto:        "auto",
		PermissionFull:        "bypassPermissions",
	}
	for tier, want := range cases {
		if got := claudePermissionMode(tier); got != want {
			t.Errorf("claudePermissionMode(%q) = %q, want %q", tier, got, want)
		}
	}
	// 空串 → 默认档
	if got := claudePermissionMode(""); got != "bypassPermissions" {
		t.Errorf("claudePermissionMode(\"\") = %q, want bypassPermissions", got)
	}
}

// TestPermissionSupportOf 能力表：只有 claude / codebuddy / codebuddy-ai 接线。
func TestPermissionSupportOf(t *testing.T) {
	for _, e := range []string{"claude", "codebuddy", "codebuddy-ai"} {
		if !PermissionSupported(e) {
			t.Errorf("%s 应支持四档模型", e)
		}
		if got := PermissionSupportOf(e); got != "flag:--permission-mode" {
			t.Errorf("PermissionSupportOf(%q) = %q", e, got)
		}
	}
	// 未接线的引擎必须报 none —— CLI 层据此 exit 2，而不是静默忽略。
	for _, e := range []string{"trae", "llm", "codex", "openclaw", "dsh", "arkclaw"} {
		if PermissionSupported(e) {
			t.Errorf("%s 不应报告支持四档模型", e)
		}
		if got := PermissionSupportOf(e); got != "none" {
			t.Errorf("PermissionSupportOf(%q) = %q, want none", e, got)
		}
	}
}

// ── settings 载荷 ─────────────────────────────────────────────

// decodeSettings 把 --settings 的内联 JSON 解成嵌套 map 便于断言。
func decodeSettings(t *testing.T, payload string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("settings payload 不是合法 JSON: %v\n%s", err, payload)
	}
	return out
}

// dig 按路径取值（不存在返回 nil）。
func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

// TestSandboxSettingsPerTier 逐档断言 sandbox 子对象。
//
// 关键点：autoAllowBashIfSandboxed 是第 1/2 档与第 3 档的分水岭。
// 该键在 claude 侧的默认值是 true，所以第 1/2 档必须显式写 false ——
// 否则沙箱内的 Bash 会被自动放行，第 2 档就退化成了「编辑与命令都不问」。
//
// ⚠️ 2026-10-01 反转：第 4 档（PermissionFull / bypassPermissions）下，
// sandboxSettings **返回 nil** 而不是 `{"enabled": false}`。原因是 claude CLI
// 在 tools=default + bypassPermissions + `--settings {"sandbox":{"enabled":false}}`
// 组合下报「Invalid JSON provided to --settings」、exit 0 零输出（实测）。
// bypassPermissions 与 sandbox 互斥：开了也是空跑、显式写 enabled=false 反而触发 bug。
// 让外层（agentSettingsPayload）整段不下发即可。
func TestSandboxSettingsPerTier(t *testing.T) {
	cases := []struct {
		tier       PermissionTier
		enabled    bool
		autoAllow  bool
		wantKeySet bool // 非 full 档应带 failIfUnavailable
	}{
		{PermissionManual, true, false, true},
		{PermissionAcceptEdits, true, false, true},
		{PermissionAuto, true, true, true},
	}
	for _, c := range cases {
		sb := sandboxSettings(c.tier, PermissionOptions{}, "claude")
		if sb == nil {
			t.Errorf("%s: sandboxSettings 不应返回 nil", c.tier)
			continue
		}
		if got := sb["enabled"]; got != c.enabled {
			t.Errorf("%s: sandbox.enabled = %v, want %v", c.tier, got, c.enabled)
		}
		if got := sb["autoAllowBashIfSandboxed"]; got != c.autoAllow {
			t.Errorf("%s: sandbox.autoAllowBashIfSandboxed = %v, want %v（这是第1/2档与第3档的分水岭）",
				c.tier, got, c.autoAllow)
		}
		// 严格沙箱：默认关闭逃逸舱口。
		if got := sb["allowUnsandboxedCommands"]; got != false {
			t.Errorf("%s: sandbox.allowUnsandboxedCommands = %v, want false", c.tier, got)
		}
		// 沙箱起不来就报错，不静默降级成不沙箱运行。
		if got := sb["failIfUnavailable"]; got != true {
			t.Errorf("%s: sandbox.failIfUnavailable = %v, want true", c.tier, got)
		}
	}

	// codebuddy 文档未列 failIfUnavailable，不能注入未知键。
	sb := sandboxSettings(PermissionAuto, PermissionOptions{}, "codebuddy")
	if _, has := sb["failIfUnavailable"]; has {
		t.Error("codebuddy 不该注入 failIfUnavailable（其文档未列该键）")
	}
	if got := sb["enabled"]; got != true {
		t.Errorf("codebuddy auto 档 sandbox.enabled = %v, want true", got)
	}

	// 第 4 档：返回 nil，整段 sandbox 不下发。
	if got := sandboxSettings(PermissionFull, PermissionOptions{}, "claude"); got != nil {
		t.Errorf("full 档应返回 nil（实测 2026-10-01：写 enabled=false 触发 claude CLI bug）")
	}
}

// TestSandboxSettingsOptions 可选项（排除命令 / 网络白名单 / 放宽逃逸舱口）。
func TestSandboxSettingsOptions(t *testing.T) {
	opts := PermissionOptions{
		ExcludedCommands:         []string{" docker ", "watchman", "docker", ""}, // 含重复与空白
		AllowedDomains:           []string{"api.example.com", "*.corp.example.com"},
		AllowUnsandboxedCommands: true,
	}
	sb := sandboxSettings(PermissionAuto, opts, "claude")

	ex, _ := sb["excludedCommands"].([]string)
	if strings.Join(ex, ",") != "docker,watchman" {
		t.Errorf("excludedCommands = %v, want [docker watchman]（去重去空白）", ex)
	}
	if got := dig(map[string]any{"s": sb}, "s", "network", "allowedDomains"); got == nil {
		t.Error("allowedDomains 未注入")
	}
	if got := sb["allowUnsandboxedCommands"]; got != true {
		t.Errorf("allowUnsandboxedCommands = %v, want true（显式放宽时）", got)
	}

	// 第 4 档：返回 nil，所有可选项一律不生效。
	if sbFull := sandboxSettings(PermissionFull, opts, "claude"); sbFull != nil {
		t.Errorf("full 档 sandbox 应返回 nil（整段不下发），得到 %v", sbFull)
	}
}

// TestAutoModeSettingsOnlyForAutoTier 只有第 3 档注入 autoMode，且必须带 $defaults。
func TestAutoModeSettingsOnlyForAutoTier(t *testing.T) {
	opts := PermissionOptions{AutoModeEnvironment: []string{"Source control: github.example.com/acme-corp"}}

	if got := autoModeSettings(PermissionManual, opts); got != nil {
		t.Errorf("manual 档不该有 autoMode，得到 %v", got)
	}
	if got := autoModeSettings(PermissionAcceptEdits, opts); got != nil {
		t.Errorf("accept-edits 档不该有 autoMode，得到 %v", got)
	}
	if got := autoModeSettings(PermissionFull, opts); got != nil {
		t.Errorf("full 档不该有 autoMode，得到 %v", got)
	}
	// 第 3 档但没给自定义边界 → 不注入（用引擎内建默认即可）。
	if got := autoModeSettings(PermissionAuto, PermissionOptions{}); got != nil {
		t.Errorf("auto 档无自定义边界时不该注入 autoMode，得到 %v", got)
	}

	am := autoModeSettings(PermissionAuto, opts)
	env, _ := am["environment"].([]string)
	if len(env) != 2 || env[0] != "$defaults" {
		t.Fatalf("autoMode.environment = %v, want [$defaults, <自定义>]；"+
			"漏掉 $defaults 等于**完整替换**内建规则，会丢掉内建安全规则", env)
	}
	if env[1] != "Source control: github.example.com/acme-corp" {
		t.Errorf("自定义条目丢失: %v", env)
	}
}

// TestAgentSettingsPayload 端到端组装：env / sandbox / autoMode / permissions
// 必须合并进**同一份** JSON（--settings 不是可重复 flag，分两次传会互相覆盖）。
func TestAgentSettingsPayload(t *testing.T) {
	// ① 工具关闭 + 无 MaxTokens → 不注入（纯 chat 不需要沙箱，
	//    而 failIfUnavailable 反而可能让一次纯 chat 因沙箱起不来而失败）。
	if _, ok := agentSettingsPayload(Request{Tools: ToolsOff}, "claude"); ok {
		t.Error("tools off 且无 MaxTokens 时不该注入 --settings")
	}

	// ② 工具关闭 + MaxTokens → 只注入 env，不带 sandbox。
	payload, ok := agentSettingsPayload(Request{Tools: ToolsOff, MaxTokens: 8000}, "claude")
	if !ok {
		t.Fatal("tools off + MaxTokens 应注入 --settings")
	}
	s := decodeSettings(t, payload)
	if got := dig(s, "env", "CLAUDE_CODE_MAX_OUTPUT_TOKENS"); got != "8000" {
		t.Errorf("env 注入错误: %v", got)
	}
	if _, has := s["sandbox"]; has {
		t.Error("tools off 时不该注入 sandbox")
	}

	// ③ 第 3 档 + MaxTokens + 全部可选项 → 四段齐全，且共用一份 JSON。
	req := Request{
		Tools:      ToolsOn,
		MaxTokens:  16000,
		Permission: PermissionAuto,
		PermissionOptions: PermissionOptions{
			ExcludedCommands:    []string{"docker"},
			AutoModeEnvironment: []string{"Org: acme"},
			Deny:                []string{"Bash(rm -rf *)"},
			Ask:                 []string{"Bash(git push *)"},
		},
	}
	payload, ok = agentSettingsPayload(req, "claude")
	if !ok {
		t.Fatal("auto 档应注入 --settings")
	}
	s = decodeSettings(t, payload)
	if got := dig(s, "env", "CLAUDE_CODE_MAX_OUTPUT_TOKENS"); got != "16000" {
		t.Errorf("env 丢失（说明被 sandbox 覆盖了？）: %v", got)
	}
	if got := dig(s, "sandbox", "enabled"); got != true {
		t.Errorf("sandbox.enabled = %v, want true", got)
	}
	if got := dig(s, "sandbox", "autoAllowBashIfSandboxed"); got != true {
		t.Errorf("auto 档 autoAllowBashIfSandboxed = %v, want true", got)
	}
	if got := dig(s, "autoMode", "environment"); got == nil {
		t.Error("autoMode 丢失")
	}
	if got := dig(s, "permissions", "deny"); got == nil {
		t.Error("permissions.deny 丢失")
	}
	if got := dig(s, "permissions", "ask"); got == nil {
		t.Error("permissions.ask 丢失")
	}

	// ④ 第 4 档：**不下发 sandbox 整段**（实测 2026-10-01：claude CLI 在
	//   tools=default + bypassPermissions + `--settings {"sandbox":{"enabled":false}}`
	//   组合下报「Invalid JSON provided to --settings」、exit 0 零输出）。
	//   bypassPermissions 与 sandbox 互斥：开了也是空跑、显式写 enabled=false
	//   反而触发那个 bug。让外层 sandboxSettings 返 nil、外层整段不下发即可。
	payload, ok = agentSettingsPayload(Request{Tools: ToolsOn, Permission: PermissionFull}, "claude")
	if ok {
		t.Error("full 档应**不**注入 --settings（沙箱整段不下发）")
	}
	if payload != "" {
		t.Errorf("full 档 payload 应为空，实际 %q", payload)
	}
}

// TestAgentSettingsPayloadDeterministic 同一组选项必须产出同一份 JSON。
// （否则参数断言类测试会随机翻车，调用方的 diff 也会无意义地抖。）
func TestAgentSettingsPayloadDeterministic(t *testing.T) {
	req := Request{
		Tools:      ToolsOn,
		Permission: PermissionManual,
		PermissionOptions: PermissionOptions{
			ExcludedCommands: []string{"docker", "git", "docker"},
			AllowedDomains:   []string{"b.example.com", "a.example.com"},
			Deny:             []string{"Bash(rm -rf *)", "Bash(rm -rf *)"},
		},
	}
	first, _ := agentSettingsPayload(req, "claude")
	for i := 0; i < 5; i++ {
		got, _ := agentSettingsPayload(req, "claude")
		if got != first {
			t.Fatalf("载荷不稳定：\n1: %s\n%d: %s", first, i, got)
		}
	}
}

// ── argv 映射（四档的最终证据） ────────────────────────────────

// argValue 取 argv 中某个 flag 的取值；flag 不存在时返回 ("", false)。
func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// TestClaudeBuildArgsPermissionTiers 逐档断言 claude 的真实 argv。
func TestClaudeBuildArgsPermissionTiers(t *testing.T) {
	e := &ClaudeEngine{}
	cases := []struct {
		tier     PermissionTier
		wantMode string
	}{
		{PermissionManual, "default"},
		{PermissionAcceptEdits, "acceptEdits"},
		{PermissionAuto, "auto"},
		{PermissionFull, "bypassPermissions"},
	}
	for _, c := range cases {
		// --tools on：2026-10-01 必须显式 --tools default（不然 claude CLI 在
		// stream-json 模式下不发 result 行）；同时仍带 --permission-mode。
		args := e.buildArgsBase(Request{Tools: ToolsOn, Permission: c.tier})
		got, ok := argValue(args, "--permission-mode")
		if !ok {
			t.Fatalf("%s(--tools on): argv 里没有 --permission-mode: %v", c.tier, args)
		}
		if got != c.wantMode {
			t.Errorf("%s(--tools on): --permission-mode = %q, want %q", c.tier, got, c.wantMode)
		}
		if got, _ := argValue(args, "--tools"); got != "default" {
			t.Errorf("%s(--tools on): --tools = %q, want default（显式开全部内置）", c.tier, got)
		}
		// 改造后不该再出现 --dangerously-skip-permissions（档位统一走 --permission-mode）。
		for _, a := range args {
			if a == "--dangerously-skip-permissions" {
				t.Errorf("%s: 仍在传 --dangerously-skip-permissions，应统一走 --permission-mode", c.tier)
			}
		}
		// 第 4 档**不**带 --settings（实测 2026-10-01：claude CLI 在
		// tools=default + bypassPermissions + `{"sandbox":{"enabled":false}}`
		// 组合下报「Invalid JSON provided to --settings」、exit 0 零输出）。
		// 其他档：沙箱只能经 --settings 进（claude 没有 --sandbox flag）。
		hasSettings := false
		if _, has := argValue(args, "--settings"); has {
			hasSettings = true
		}
		if c.tier == PermissionFull {
			if hasSettings {
				t.Errorf("%s(--tools on): full 档不应带 --settings（实测 2026-10-01 触发 claude CLI bug）", c.tier)
			}
		} else {
			if !hasSettings {
				t.Errorf("%s(--tools on): 缺少 --settings（沙箱配置没有 CLI flag，只能从这里进）", c.tier)
			}
		}

		// --tools 白名单
		wargs := e.buildArgsBase(Request{Tools: ToolsAllowlist([]string{"Bash", "Read"}), Permission: c.tier})
		if got, _ := argValue(wargs, "--tools"); got != "Bash,Read" {
			t.Errorf("%s(白名单): --tools = %q, want Bash,Read", c.tier, got)
		}
		if got, _ := argValue(wargs, "--permission-mode"); got != c.wantMode {
			t.Errorf("%s(白名单): --permission-mode = %q, want %q", c.tier, got, c.wantMode)
		}
	}

	// --tools off：不传档位、不传 settings（无工具调用，沙箱无意义）。
	offArgs := e.buildArgsBase(Request{Tools: ToolsOff, Permission: PermissionManual})
	if _, has := argValue(offArgs, "--permission-mode"); has {
		t.Errorf("--tools off 不该传 --permission-mode: %v", offArgs)
	}
	if _, has := argValue(offArgs, "--settings"); has {
		t.Errorf("--tools off 且无 MaxTokens 不该传 --settings: %v", offArgs)
	}
	if got, _ := argValue(offArgs, "--tools"); got != "" {
		t.Errorf("--tools off 应传空串，得到 %q", got)
	}
}

// TestClaudeBuildArgsDefaultTierUnchanged 默认档的 argv 与改造前的语义一致。
//
// 2026-10-01 修订：默认档（Permission 零值 = PermissionFull / bypassPermissions）
// **不**带 --settings。原因是 claude CLI 在 tools=default + bypassPermissions +
// `{"sandbox":{"enabled":false}}` 组合下报「Invalid JSON provided to --settings」、
// exit 0 零输出（实测）。bypassPermissions 与 sandbox 互斥，写 enabled=false 反而
// 触发那个 bug。让 sandboxSettings 在 full 档返 nil、外层整段不下发即可。
func TestClaudeBuildArgsDefaultTierUnchanged(t *testing.T) {
	e := &ClaudeEngine{}
	args := e.buildArgsBase(Request{Tools: ToolsOn}) // Permission 零值
	if got, _ := argValue(args, "--permission-mode"); got != "bypassPermissions" {
		t.Errorf("默认档 --permission-mode = %q, want bypassPermissions", got)
	}
	if got, _ := argValue(args, "--tools"); got != "default" {
		t.Errorf("默认档 --tools = %q, want default（2026-10-01 必须显式 --tools default）", got)
	}
	if _, has := argValue(args, "--settings"); has {
		t.Errorf("默认档（full）不应带 --settings（实测触发 claude CLI bug）")
	}
}

// TestCodeBuddyBuildArgsPermissionTiers codebuddy 与 claude 同族，档位取值一致。
func TestCodeBuddyBuildArgsPermissionTiers(t *testing.T) {
	e := &CodeBuddyEngine{}
	args := e.buildArgsBase(Request{Tools: ToolsOn, Permission: PermissionAuto})
	if got, ok := argValue(args, "--permission-mode"); !ok || got != "auto" {
		t.Errorf("codebuddy --permission-mode = %q (ok=%v), want auto", got, ok)
	}
	// 改造后不该再出现 -y。
	for _, a := range args {
		if a == "-y" {
			t.Error("codebuddy 仍在传 -y，应统一走 --permission-mode")
		}
	}
	payload, ok := argValue(args, "--settings")
	if !ok {
		t.Fatal("codebuddy auto 档应带 --settings")
	}
	s := decodeSettings(t, payload)
	if got := dig(s, "sandbox", "enabled"); got != true {
		t.Errorf("codebuddy auto 档 sandbox.enabled = %v, want true", got)
	}
	if got := dig(s, "sandbox", "autoAllowBashIfSandboxed"); got != true {
		t.Errorf("codebuddy auto 档 autoAllowBashIfSandboxed = %v, want true", got)
	}
}

// ── 端到端：参数真的进了子进程 ────────────────────────────────

// TestClaudePermissionEndToEnd 假 CLI 落 argv 取证。
//
// buildArgsBase 的返回值好看不等于子进程真的收到 —— spawn 路径上还有
// runCLIIn / streamJSONArgs 等环节会改参数。这里让假 CLI 把 "$@" 原样落盘，
// 再断言落盘内容里确实有档位与沙箱载荷。
func TestClaudePermissionEndToEnd(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "argv.txt")

	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"" + out + "\"; done\n" +
		"echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\",\"session_id\":\"s-1\",\"model\":\"m\"}'\n"
	cli := writeFakeCLI(t, "claude", script)

	e := &ClaudeEngine{BinPath: cli}
	req := Request{
		Engine:     "claude",
		Tools:      ToolsOn,
		Permission: PermissionAcceptEdits,
		PermissionOptions: PermissionOptions{
			Ask: []string{"Bash(git push *)"},
		},
		Messages: []Message{{Role: "user", Content: "hi"}},
	}
	if _, err := e.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("假 CLI 未落盘 argv: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	got, ok := argValue(lines, "--permission-mode")
	if !ok {
		t.Fatalf("子进程 argv 里没有 --permission-mode:\n%s", raw)
	}
	if got != "acceptEdits" {
		t.Errorf("子进程收到 --permission-mode %q, want acceptEdits", got)
	}
	payload, ok := argValue(lines, "--settings")
	if !ok {
		t.Fatalf("子进程 argv 里没有 --settings:\n%s", raw)
	}
	s := decodeSettings(t, payload)
	if v := dig(s, "sandbox", "autoAllowBashIfSandboxed"); v != false {
		t.Errorf("第 2 档 autoAllowBashIfSandboxed = %v, want false（命令仍须确认）", v)
	}
	if v := dig(s, "sandbox", "enabled"); v != true {
		t.Errorf("第 2 档 sandbox.enabled = %v, want true", v)
	}
	if v := dig(s, "permissions", "ask"); v == nil {
		t.Error("permissions.ask 未进子进程")
	}
}
