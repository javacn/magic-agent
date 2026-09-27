package cli

// engines_test.go - `--engines` 的「一键安装 / 升级命令」（install 字段）与版本号（version）装配测试。
//
// 需求（2026-09-21）：「引擎列表返回不可用的引擎时包括一键安装引擎的命令 这样检测到
// 无法安装的可以直接安装」；（2026-09-23）「引擎检测除了 a2a 的 其他也要支持有升级」。
// 约定（**当前契约**）：
//
//	有官方安装方式的引擎 → **一律给 install**（不管 ok 是 true 还是 false）：
//	    ok:false → 调用方当「安装」用；ok:true → 当「升级」用（重跑一遍 = 装最新版）
//	没有可执行安装路径的引擎（codebuddy 是 GUI 应用、arkclaw / 具名 A2A agent 是远端网关）
//	    → 不给该字段，原因在同行的 note 里（不编一条跑不通的命令）
//	version → 可用引擎尽力而为地给（`<bin> --version` 的第一个 semver）；`--no-models`
//	    跳过探测 → 一个都不给（本文件据此断言「该档零探测」）
//
// ⚠️ 注册假引擎的名字必须避开「别的测试行为上依赖」的名字：
// `agent.Lookup` 取注册表里**首个**同名匹配，而 Go 按文件名字母序跑测试
// （engines_test.go 在 keepalive_test.go / permission_test.go 之前）。两个坑：
//   - name:"codebuddy" 会挡住 keepalive_test.go 需要的**流式** codebuddy 假引擎
//     → TestKeepAliveIsDefaultForStreaming / TestKeepAliveAppendEndToEnd 稳定超时；
//   - name:"dsh" 会被 attach_test.go（先跑）注册的可用假引擎挡住 → ok 报 true。
// 故这里只用 openclaw（install 用例）、codebuddy-ai（无 install 用例）与 llm（可用 + 有命令用例）：
// 整包测试里没有别的文件在它们之前注册过这三个名字。
//
// 注：codebuddy / arkclaw 确实没有一键安装命令，但那个断言放在
// internal/agent/engine_base_test.go 的 TestInstallCommandOf 里做（纯函数、零副作用），
// 不必在这个会改全局注册表的测试里再注册一次它们的同名假引擎。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/darren/magic-agent/internal/agent"
)

// unavailableEngine 探测失败的假引擎（ok:false 路径）。
type unavailableEngine struct{ name string }

func (u *unavailableEngine) Name() string { return u.name }

func (u *unavailableEngine) Detect() (bool, string) {
	return false, u.name + " CLI not found (install it or set the BIN env)"
}

func (u *unavailableEngine) Complete(context.Context, agent.Request) (agent.Response, error) {
	return agent.Response{}, errors.New("unavailable")
}

func TestEnginesInstallCommandByAvailability(t *testing.T) {
	// 三个假引擎：不可用但有一键安装命令（openclaw）、不可用且没有（codebuddy-ai 是 GUI 应用）、
	// 可用且有一键安装命令（llm，用来钉「可用时也给 = 当升级用」）。
	registerFake(&unavailableEngine{name: "openclaw"})
	registerFake(&unavailableEngine{name: "codebuddy-ai"})
	registerFake(&stringEngine{name: "llm", text: "x"})

	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	var rows []struct {
		Engine  string `json:"engine"`
		OK      bool   `json:"ok"`
		Note    string `json:"note"`
		Install string `json:"install"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rows); err != nil {
		t.Fatalf("--engines 输出不是 JSON 数组: %v\n%s", err, stdout)
	}

	// 1) 不可用 + 有安装命令：install 非空、note 也保留（人读原因）。
	const wantInstall = "npm install -g openclaw@latest"
	foundWithInstall := false
	for _, r := range rows {
		if r.Engine != "openclaw" {
			continue
		}
		if r.OK {
			t.Fatal("openclaw 假引擎应报 ok:false")
		}
		foundWithInstall = true
		if r.Install != wantInstall {
			t.Errorf("openclaw 的 install = %q want %q", r.Install, wantInstall)
		}
		if r.Note == "" {
			t.Error("openclaw 不可用却没给 note")
		}
	}
	if !foundWithInstall {
		t.Fatal("--engines 缺 openclaw 行")
	}

	// 2) 不可用但没有可执行安装命令：install 必须为空（原因在 note）。
	foundAI := false
	for _, r := range rows {
		if r.Engine != "codebuddy-ai" {
			continue
		}
		if r.OK {
			t.Fatal("codebuddy-ai 假引擎应报 ok:false")
		}
		foundAI = true
		if r.Install != "" {
			t.Errorf("codebuddy-ai（GUI 应用）不该给 install，got %q", r.Install)
		}
		if r.Note == "" {
			t.Error("codebuddy-ai 不可用却没给 note（怎么才能用要靠它说明）")
		}
	}
	if !foundAI {
		t.Fatal("--engines 缺 codebuddy-ai 行")
	}

	// 3) **可用**引擎只要该引擎有官方安装方式，照样给 install（2026-09-23 新契约：
	//    同一条命令对已装好的引擎重跑一遍 = 升级到最新版，调用方据此把按钮写成「升级」）。
	foundOKWithInstall := false
	for _, r := range rows {
		if r.Engine != "llm" || !r.OK {
			continue
		}
		foundOKWithInstall = true
		if r.Install != agent.InstallCommandOf("llm") {
			t.Errorf("llm（可用）的 install = %q want %q", r.Install, agent.InstallCommandOf("llm"))
		}
	}
	if !foundOKWithInstall {
		t.Fatal("--engines 缺可用的 llm 行（本用例要钉「可用时也给 install」）")
	}

	// 4) 全局不变式：install 非空 ⟺ 该引擎确实有官方安装命令（与 ok 无关）。
	for _, r := range rows {
		want := agent.InstallCommandOf(r.Engine)
		if want == "" && r.Install != "" {
			t.Errorf("%s 没有官方安装方式却带了 install = %q", r.Engine, r.Install)
		}
		if want != "" && r.Install != want {
			t.Errorf("%s 的 install = %q want %q（不管 ok 是 %v）", r.Engine, r.Install, want, r.OK)
		}
	}
}

// `--no-models` 档跳过版本探测：一个 version 都不给（该档的卖点就是「不启动任何 CLI」）。
func TestEnginesVersionSkippedWithNoModels(t *testing.T) {
	registerFake(&stringEngine{name: "fake-eng-ver", text: "x"})

	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rows); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, stdout)
	}
	for _, r := range rows {
		if _, ok := r["version"]; ok {
			t.Errorf("--no-models 不该给 version（零探测）: %v", r)
		}
	}
}

// install 字段的 omitempty：没有官方安装方式的引擎，JSON 里**不应出现**该 key（不是空串）。
func TestEnginesInstallKeyOmittedWhenNoCommand(t *testing.T) {
	registerFake(&stringEngine{name: "install-fake-ok-2", text: "x"})

	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rows); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, stdout)
	}
	for _, r := range rows {
		if r["engine"] == "install-fake-ok-2" {
			if _, ok := r["install"]; ok {
				t.Errorf("没有官方安装方式的引擎不该有 install key: %v", r)
			}
			return
		}
	}
	t.Fatal("--engines 输出里没找到 install-fake-ok-2")
}
