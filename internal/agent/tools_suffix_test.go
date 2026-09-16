package agent

// tools_suffix_test.go - 「工具启用无效」回归测试。
//
// 背景：noToolSuffix 明文写着「严禁使用任何工具」。此前 codebuddy / trae
// 无条件把它注入 system prompt，导致 --tools on / 白名单模式下 CLI 侧
// 工具已开、system prompt 却在压制模型调用，表现为「启用了工具但没有
// 网络搜索」。
//
// 正确语义：off 模式注入（防「伪工具调用」），on / 白名单模式**不注入**。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// argLogger 造一个记录参数的假 CLI，返回 (引擎可用的 cli 路径, 读取参数函数)。
func argLogger(t *testing.T) (string, func() string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, "fake")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + log +
		"\necho '{\"type\":\"result\",\"result\":\"ok\"}'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli, func() string {
		b, _ := os.ReadFile(log)
		return string(b)
	}
}

func TestCodeBuddyToolModeSuffixMatrix(t *testing.T) {
	cases := []struct {
		name        string
		tools       ToolsMode
		wantSuffix  bool
		wantToolArg []string // 必须出现的参数对
		absentArg   []string // 必须不出现的参数
	}{
		{
			name:        "off 注入约束且禁工具",
			tools:       ToolsOff,
			wantSuffix:  true,
			wantToolArg: []string{"--tools", ""},
		},
		{
			name:       "on 不注入约束（否则工具被压制）",
			tools:      ToolsOn,
			wantSuffix: false,
			absentArg:  []string{"[约束]"},
		},
		{
			name:       "allowlist 不注入约束且带上白名单",
			tools:      ToolsAllowlist([]string{"WebSearch", "WebFetch"}),
			wantSuffix: false,
			absentArg:  []string{"[约束]"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli, argsOf := argLogger(t)
			e := &CodeBuddyEngine{BinPath: cli}
			if _, err := e.Complete(context.Background(), Request{
				Tools:    tc.tools,
				Messages: []Message{{Role: "user", Content: "hi"}},
			}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			args := argsOf()

			if got := strings.Contains(args, "[约束]"); got != tc.wantSuffix {
				t.Errorf("noToolSuffix 注入 = %v, want %v\nargs=%q", got, tc.wantSuffix, args)
			}
			for _, a := range tc.wantToolArg {
				if a == "" {
					if !strings.Contains(args, "--tools ") {
						t.Errorf("缺少 --tools 参数: %q", args)
					}
					continue
				}
				if !strings.Contains(args, a) {
					t.Errorf("缺少参数 %q: %q", a, args)
				}
			}
			for _, a := range tc.absentArg {
				if strings.Contains(args, a) {
					t.Errorf("不应包含 %q: %q", a, args)
				}
			}
			// 白名单模式下白名单工具名必须出现
			if tc.name == "allowlist 不注入约束且带上白名单" {
				if !strings.Contains(args, "WebSearch") {
					t.Errorf("白名单工具名未传入: %q", args)
				}
			}
		})
	}
}

func TestTraeToolModeSuffix(t *testing.T) {
	cases := []struct {
		name       string
		tools      ToolsMode
		wantSuffix bool
	}{
		{"off 注入约束", ToolsOff, true},
		{"on 不注入约束", ToolsOn, false},
		{"allowlist 不注入约束", ToolsAllowlist([]string{"Bash"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli, argsOf := argLogger(t)
			e := &TraeEngine{BinPath: cli}
			if _, err := e.Complete(context.Background(), Request{
				Tools:    tc.tools,
				Messages: []Message{{Role: "user", Content: "hi"}},
			}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			args := argsOf()
			if got := strings.Contains(args, "[约束]"); got != tc.wantSuffix {
				t.Errorf("noToolSuffix = %v, want %v\nargs=%q", got, tc.wantSuffix, args)
			}
		})
	}
}

// TestTraeToolModeMapping 固化 trae 的工具模式契约（2026-09-16 真机验证）。
//
// trae-cli 的权限开关实测语义：
//   - --allowed-tool 只做「自动批准」，不裁剪工具集（--allowed-tool WebFetch
//     仍下发全部 18 个工具）；
//   - --disallowed-tool 才真正做减法；
//   - -y 跳过全部权限检查 ⇒ 全工具可用。
//
// 因此白名单模式在 trae 上定义为「默认开启所有工具」：走 -y 全放行，
// 不再输出 --allowed-tool（它既不能收窄，全放行后也毫无作用，只会造成
// 「allowlist 被 -y 架空」的错觉）。
func TestTraeToolModeMapping(t *testing.T) {
	cases := []struct {
		name         string
		tools        ToolsMode
		wantArgs     []string
		absentArgs   []string
		wantDisallow bool // 是否出现任何 --disallowed-tool
	}{
		{
			name:         "off 逐个真名禁用且不全放行",
			tools:        ToolsOff,
			wantArgs:     []string{"Bash", "Edit", "Write", "Glob", "Grep", "Read"},
			absentArgs:   []string{"-y", "Replace"},
			wantDisallow: true,
		},
		{
			name:         "on 全放行不做减法",
			tools:        ToolsOn,
			wantArgs:     []string{"-y"},
			wantDisallow: false,
		},
		{
			name:         "allowlist 默认开启所有工具（全放行、不收窄）",
			tools:        ToolsAllowlist([]string{"WebFetch", "Bash"}),
			wantArgs:     []string{"-y"},
			absentArgs:   []string{"--allowed-tool"},
			wantDisallow: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli, argsOf := argLogger(t)
			e := &TraeEngine{BinPath: cli}
			if _, err := e.Complete(context.Background(), Request{
				Tools:    tc.tools,
				Messages: []Message{{Role: "user", Content: "hi"}},
			}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			args := argsOf()

			for _, a := range tc.wantArgs {
				if !hasArgToken(args, a) {
					t.Errorf("缺少参数 %q: %q", a, args)
				}
			}
			for _, a := range tc.absentArgs {
				if strings.Contains(args, a) {
					t.Errorf("不应包含 %q: %q", a, args)
				}
			}
			// off 必须逐个用 --disallowed-tool 列出真名；on/白名单必须是纯全放行。
			gotDisallow := strings.Contains(args, "--disallowed-tool")
			if gotDisallow != tc.wantDisallow {
				t.Errorf("--disallowed-tool 出现 = %v, want %v\nargs=%q", gotDisallow, tc.wantDisallow, args)
			}
			if tc.wantDisallow {
				for _, a := range traeOffDisableTools {
					if !strings.Contains(args, "--disallowed-tool "+a) {
						t.Errorf("off 模式漏禁 %q: %q", a, args)
					}
				}
			}
		})
	}
}

// hasArgToken 判断命令行里是否出现某个完整参数（避免 "-y" 被别的串误命中）。
func hasArgToken(args, want string) bool {
	for _, f := range strings.Fields(args) {
		if f == want {
			return true
		}
	}
	return false
}

// nil ToolsMode 必须按 off 处理（默认关工具，向后兼容）。
func TestToolsIsOffDefaultsToOff(t *testing.T) {
	if !toolsIsOff(Request{}) {
		t.Error("空 Tools 应视为 off")
	}
	if toolsIsOff(Request{Tools: ToolsOn}) {
		t.Error("ToolsOn 不应判定为 off")
	}
	if toolsIsOff(Request{Tools: ToolsAllowlist([]string{"Read"})}) {
		t.Error("白名单不应判定为 off")
	}
}

// noToolSuffix 必须显式禁止「伪造工具返回」。
//
// 背景：off 模式下模型被要求「输出工具抓到的内容」时会编造返回体
// （实测约 1/6），仅禁「调用工具」堵不住，必须有一条明文禁止伪造。
func TestNoToolSuffixForbidsFabricatedToolOutput(t *testing.T) {
	if !strings.Contains(noToolSuffix, "伪造工具返回") {
		t.Errorf("noToolSuffix 缺少「禁止伪造工具返回」约束: %q", noToolSuffix)
	}
	// 该约束要能经 FlattenPrompt 落到 system prompt 里。
	got := FlattenPrompt("你是助手", nil, true)
	if !strings.Contains(got, "伪造工具返回") {
		t.Errorf("伪造工具返回约束未进入 prompt: %q", got)
	}
	// 且 off 模式下 codebuddy 参数里能看到。
	cli, argsOf := argLogger(t)
	e := &CodeBuddyEngine{BinPath: cli}
	if _, err := e.Complete(context.Background(), Request{
		Tools:    ToolsOff,
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(argsOf(), "伪造工具返回") {
		t.Errorf("codebuddy off 模式 args 未带伪造约束: %q", argsOf())
	}
}
