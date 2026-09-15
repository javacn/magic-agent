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
