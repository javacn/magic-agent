package cli

// permission_test.go - CLI 层的四档权限模型测试（--permission）。
//
// 覆盖：
//   - resolvePermissionTier 的解析与两道校验（取值合法性 / 引擎是否接线）
//   - flag 默认值契约（默认 full = 保持既有行为）
//   - --engines 输出里的 permission 能力字段
//   - prepareAsk 把 --permission* 正确写进 agent.Request（flag → Request 的接线）
//
// ⚠️ 本包测试进程里的引擎注册表是**假的**（见 workspace_test.go 的说明：
// agent.Engines() 只在 registry 为空时才 initEngines()，而本包测试先 Register 了
// 假引擎 → 真实引擎不会出现）。所以这里一律按**名字**注册假引擎，并只断言
// 与引擎实现无关的东西（名字驱动的能力函数、prepareAsk 产出的 Request）。
// 「Request → claude argv → 子进程」那一段在 internal/agent/permission_test.go
// 里用真 ClaudeEngine + 假 CLI 覆盖（那才是能直接构造引擎的层）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/darren/magic-agent/internal/agent"
)

// TestResolvePermissionTier 解析 + 校验。
func TestResolvePermissionTier(t *testing.T) {
	cases := []struct {
		name     string
		engine   string
		raw      string
		explicit bool
		want     agent.PermissionTier
		wantErr  bool
		errHas   string
	}{
		{"四档规范名", "claude", "manual", true, agent.PermissionManual, false, ""},
		{"accept-edits", "claude", "accept-edits", true, agent.PermissionAcceptEdits, false, ""},
		{"auto", "codebuddy", "auto", true, agent.PermissionAuto, false, ""},
		{"full", "claude", "full", true, agent.PermissionFull, false, ""},
		{"别名 default", "claude", "default", true, agent.PermissionManual, false, ""},
		{"数字档位", "claude", "3", true, agent.PermissionAuto, false, ""},
		{"大小写不敏感", "claude", "AUTO", true, agent.PermissionAuto, false, ""},
		{"非法取值 plan", "claude", "plan", true, "", true, "invalid --permission"},
		{"非法取值 dontAsk", "claude", "dontAsk", true, "", true, "invalid --permission"},
		{"空取值", "claude", "", true, "", true, "invalid --permission"},
		// 未接线引擎：显式传才报错（默认值不能把 `-e codex` 这类既有调用打挂）
		{"未接线引擎显式传", "codex", "auto", true, "", true, "暂不支持"},
		{"未接线引擎 trae", "trae", "manual", true, "", true, "暂不支持"},
		{"未接线引擎默认值放行", "codex", "full", false, agent.PermissionFull, false, ""},
		{"已接线引擎默认值", "claude", "full", false, agent.PermissionFull, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolvePermissionTier(c.engine, c.raw, c.explicit)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 tier=%q", got)
				}
				if c.errHas != "" && !strings.Contains(err.Error(), c.errHas) {
					t.Errorf("错误信息 %q 不含 %q", err.Error(), c.errHas)
				}
				// 参数类错误必须是 usageError（exit 2）。
				if _, ok := err.(*usageError); !ok {
					t.Errorf("应为 *usageError（exit 2），得到 %T", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if got != c.want {
				t.Errorf("tier = %q, want %q", got, c.want)
			}
		})
	}
}

// TestPermissionFlagDefaults 默认值契约：--permission 默认 full。
//
// 改默认档等于静默变更既有调用方的行为（agent 会突然开始弹审批 / 被沙箱拦），
// 所以这个默认值必须被测试钉住。
func TestPermissionFlagDefaults(t *testing.T) {
	cmd := NewRootCommand()
	f := cmd.PersistentFlags().Lookup("permission")
	if f == nil {
		t.Fatal("--permission flag 未注册")
	}
	if f.DefValue != "full" {
		t.Errorf("--permission 默认值 = %q, want full（= agent.DefaultPermissionTier，保持既有行为）", f.DefValue)
	}
	if f.DefValue != string(agent.DefaultPermissionTier) {
		t.Errorf("--permission 默认值 = %q 与 agent.DefaultPermissionTier=%q 不一致",
			f.DefValue, agent.DefaultPermissionTier)
	}
	// 配套 flags 也要在（否则 --permission-deny 之类会被当成 prompt 发给引擎）。
	for _, name := range []string{
		"sandbox-exclude", "sandbox-domain", "auto-mode-env", "permission-deny", "permission-ask",
	} {
		if cmd.PersistentFlags().Lookup(name) == nil {
			t.Errorf("--%s 未注册", name)
		}
	}
}

// TestEnginesReportsPermission --engines 每行要带 permission 能力字段。
//
// 按 workspace_test.go 的既有约定按名字注册假引擎（本包真实引擎不会出现）。
func TestEnginesReportsPermission(t *testing.T) {
	for _, n := range []string{"claude", "codebuddy", "trae", "llm", "codex", "openclaw", "arkclaw"} {
		registerFake(&stringEngine{name: n, text: "x"})
	}

	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	var rows []struct {
		Engine     string `json:"engine"`
		Permission string `json:"permission"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rows); err != nil {
		t.Fatalf("--engines 输出不是 JSON 数组: %v\n%s", err, stdout)
	}
	seen := map[string]string{}
	for _, r := range rows {
		seen[r.Engine] = r.Permission
	}
	if seen["claude"] != "flag:--permission-mode" {
		t.Errorf("claude 的 permission 字段 = %q, want flag:--permission-mode", seen["claude"])
	}
	if seen["codebuddy"] != "flag:--permission-mode" {
		t.Errorf("codebuddy 的 permission 字段 = %q, want flag:--permission-mode", seen["codebuddy"])
	}
	for _, e := range []string{"trae", "llm", "codex", "openclaw", "arkclaw"} {
		if v, ok := seen[e]; !ok {
			t.Errorf("--engines 缺 %s 行", e)
		} else if v != "none" {
			t.Errorf("%s 的 permission 字段 = %q, want none", e, v)
		}
	}
}

// TestPermissionUnsupportedEngineExitsUsage 未接线引擎 + 显式 --permission → usage 错误。
func TestPermissionUnsupportedEngineExitsUsage(t *testing.T) {
	registerFake(&stringEngine{name: "fake-perm-none", text: "x"})
	_, _, err := runAskCmd(t, "", "-e", "fake-perm-none", "--permission", "auto", "-p", "hi")
	if err == nil {
		t.Fatal("期望报错（引擎未接线）")
	}
	if !strings.Contains(err.Error(), "暂不支持") {
		t.Errorf("错误信息 = %q，应说明引擎未接线", err.Error())
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("应为 *usageError（exit 2），得到 %T", err)
	}
}

// TestPermissionInvalidValueExitsUsage 非法取值 → usage 错误。
func TestPermissionInvalidValueExitsUsage(t *testing.T) {
	registerFake(&stringEngine{name: "fake-perm-bad", text: "x"})
	_, _, err := runAskCmd(t, "", "-e", "fake-perm-bad", "--permission", "yolo-nope", "-p", "hi")
	if err == nil {
		t.Fatal("期望报错（取值非法）")
	}
	if !strings.Contains(err.Error(), "invalid --permission") {
		t.Errorf("错误信息 = %q", err.Error())
	}
}

// TestPermissionToolsOffHint --tools off 下显式传非默认档 → stderr 提示（不静默）。
//
// 该提示由 prepareAsk 依 flag 是否显式传过决定，与引擎实现无关，故对假引擎同样成立。
func TestPermissionToolsOffHint(t *testing.T) {
	registerFake(&stringEngine{name: "fake-perm-hint", text: "x"})
	// 假引擎不在支持列表里，所以这里只用「默认档 + 显式传」的组合验证不提示；
	// 非默认档的提示路径用 claude 这个名字（名字即被支持）验证。
	_, stderr, err := runAskCmd(t, "", "-e", "fake-perm-hint", "-p", "hi")
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if strings.Contains(stderr, "不会生效") {
		t.Errorf("默认档不该提示档位不生效，得到 %q", stderr)
	}

	registerFake(&stringEngine{name: "claude", text: "ok"})
	_, stderr2, err := runAskCmd(t, "", "-e", "claude", "--tools", "off",
		"--permission", "manual", "-o", "text", "-p", "hi")
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if !strings.Contains(stderr2, "不会生效") {
		t.Errorf("stderr 应提示档位在 --tools off 下不生效，得到 %q", stderr2)
	}
}

// ── flag → agent.Request 的接线 ───────────────────────────────

// TestPrepareAskWiresPermission prepareAsk 必须把 --permission* 原样写进 Request。
//
// 这是四档模型在 CLI 层的接线证据。断言只针对 Request 的字段（与哪个引擎实现
// 被 Lookup 命中无关），所以对本包的假引擎注册表也稳定。
func TestPrepareAskWiresPermission(t *testing.T) {
	// 让引擎预检无论如何都能过：真 ClaudeEngine 走这个假 CLI，假引擎自带 Detect=true。
	dummy := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(dummy, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGIC_AGENT_CLAUDE_BIN", dummy)

	opts := newAskOptions()
	cmd := &cobra.Command{Use: "t", SilenceUsage: true, SilenceErrors: true}
	bindAskFlags(cmd, opts)
	var req agent.Request
	cmd.RunE = func(c *cobra.Command, args []string) error {
		_, _, r, err := prepareAsk(c, args, opts)
		req = r
		return err
	}
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{
		"-e", "claude", "--tools", "on",
		"--permission", "accept-edits",
		"--sandbox-exclude", "docker,watchman",
		"--sandbox-domain", "api.example.com",
		"--auto-mode-env", "Source control: github.example.com/acme-corp",
		"--permission-deny", "Bash(rm -rf *)",
		"--permission-ask", "Bash(git push *)",
		"-p", "hi",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prepareAsk 失败: %v", err)
	}

	if req.Permission != agent.PermissionAcceptEdits {
		t.Errorf("Request.Permission = %q, want %q", req.Permission, agent.PermissionAcceptEdits)
	}
	po := req.PermissionOptions
	if strings.Join(po.ExcludedCommands, ",") != "docker,watchman" {
		t.Errorf("ExcludedCommands = %v", po.ExcludedCommands)
	}
	if len(po.AllowedDomains) != 1 || po.AllowedDomains[0] != "api.example.com" {
		t.Errorf("AllowedDomains = %v", po.AllowedDomains)
	}
	if len(po.AutoModeEnvironment) != 1 || po.AutoModeEnvironment[0] != "Source control: github.example.com/acme-corp" {
		t.Errorf("AutoModeEnvironment = %v", po.AutoModeEnvironment)
	}
	if len(po.Deny) != 1 || po.Deny[0] != "Bash(rm -rf *)" {
		t.Errorf("Deny = %v", po.Deny)
	}
	if len(po.Ask) != 1 || po.Ask[0] != "Bash(git push *)" {
		t.Errorf("Ask = %v", po.Ask)
	}
	if !agent.ToolsIsOn(req.Tools) {
		t.Errorf("Tools 应为 on，得到 %v", req.Tools)
	}
}

// TestPrepareAskPermissionDefaults Request.Permission 在未传 flag 时是默认档 full。
func TestPrepareAskPermissionDefaults(t *testing.T) {
	dummy := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(dummy, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGIC_AGENT_CLAUDE_BIN", dummy)

	opts := newAskOptions()
	cmd := &cobra.Command{Use: "t", SilenceUsage: true, SilenceErrors: true}
	bindAskFlags(cmd, opts)
	var req agent.Request
	cmd.RunE = func(c *cobra.Command, args []string) error {
		_, _, r, err := prepareAsk(c, args, opts)
		req = r
		return err
	}
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"-e", "claude", "-p", "hi"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prepareAsk 失败: %v", err)
	}
	if req.Permission != agent.PermissionFull {
		t.Errorf("Request.Permission = %q, want full（默认档）", req.Permission)
	}
	// 未传可选项时应当是零值（零值即最严格）。
	if len(req.PermissionOptions.Deny) != 0 || len(req.PermissionOptions.Ask) != 0 {
		t.Errorf("未传规则时不该有值: %+v", req.PermissionOptions)
	}
}
