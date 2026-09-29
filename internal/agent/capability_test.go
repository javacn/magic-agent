package agent

import (
	"sort"
	"strings"
	"testing"
)

// hasCap 判断能力列表里有没有某个 id。
func hasCap(caps []string, id string) bool {
	for _, c := range caps {
		if c == id {
			return true
		}
	}
	return false
}

// TestCapabilitiesOfAllEngines 注册表里每个引擎的能力列表都要**结构合法**：
// 有序、无重复、id 都在已知集合里。
//
// 刻意不要求「非空」，也不检查注册表里有多少个引擎：注册表里可能出现用例注册的
// 探针引擎（它什么都没声明），而且它有个已知脆弱点会让内置引擎在某些运行方式下
// 整体缺席（见 engine.go 的 registry 注释）。「真实引擎至少有一项能力」由
// TestCapabilitiesDocumentedEngines 用**直接构造的实例**断言，与注册表状态无关。
func TestCapabilitiesOfAllEngines(t *testing.T) {
	known := map[string]bool{}
	for _, id := range CapabilityIDs() {
		known[id] = true
	}
	engines := Engines()
	if len(engines) == 0 {
		t.Fatal("引擎注册表为空，能力推导无从校验")
	}
	for _, e := range engines {
		caps := CapabilitiesOf(e.Name())
		if !sort.StringsAreSorted(caps) {
			t.Errorf("%s: 能力列表未排序: %v", e.Name(), caps)
		}
		seen := map[string]bool{}
		for _, id := range caps {
			if seen[id] {
				t.Errorf("%s: 能力重复: %s", e.Name(), id)
			}
			seen[id] = true
			if !known[id] {
				t.Errorf("%s: 未知能力 id %q（新增能力要同时进 CapabilityIDs）", e.Name(), id)
			}
		}
	}
}

// TestCapabilitiesOfUnknownEngine 未注册的引擎名返回 nil —— 调用方据此区分
// 「没这个引擎」与「这个引擎什么都不会」。
func TestCapabilitiesOfUnknownEngine(t *testing.T) {
	if caps := CapabilitiesOf("no-such-engine-xyz"); caps != nil {
		t.Errorf("未注册引擎应返回 nil，得到 %v", caps)
	}
	if caps := CapabilitiesOf(""); caps != nil {
		t.Errorf("空名字应返回 nil，得到 %v", caps)
	}
}

// TestCapabilitiesStable 多次调用结果逐项一致：capabilities 是**静态声明**，
// 不应受运行态（是否冷却、模型探测成功与否）影响。
func TestCapabilitiesStable(t *testing.T) {
	for _, e := range Engines() {
		a := CapabilitiesOf(e.Name())
		b := CapabilitiesOf(e.Name())
		if len(a) != len(b) {
			t.Errorf("%s: 两次调用长度不同 %d vs %d", e.Name(), len(a), len(b))
			continue
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("%s: 第 %d 项不稳定 %q vs %q", e.Name(), i, a[i], b[i])
			}
		}
	}
}

// TestCapabilitiesDocumentedEngines 钉住 README 里已经写明的几条推导，防止把
// 「原生通道」与「降级通道」写反。
//
// 用**直接构造的引擎实例**（capabilitiesOfEngine），不走按名字查表 —— 注册表有全局状态，
// 会随测试筛选方式变化（见 engine.go 的 registry 注释），而这几条事实与注册表无关。
func TestCapabilitiesDocumentedEngines(t *testing.T) {
	// claude：stream / ask（AskUserQuestion + can_use_tool）/ 四档权限 / 原生附件 / npm 安装命令
	claude := capabilitiesOfEngine(&ClaudeEngine{})
	if len(claude) == 0 {
		t.Fatal("claude 的能力列表为空（真实引擎至少应有一项）")
	}
	for _, id := range []string{
		CapSessionStream, CapSessionAsk, CapSessionPermission,
		CapAttachmentNative, CapEngineInstall,
	} {
		if !hasCap(claude, id) {
			t.Errorf("claude 应有能力 %s，实际 %v", id, claude)
		}
	}

	// llm：README 明确「其余引擎」没有 ask / permission 通道
	llm := capabilitiesOfEngine(&LLMEngine{})
	for _, id := range []string{CapSessionAsk, CapSessionPermission} {
		if hasCap(llm, id) {
			t.Errorf("llm 不应有能力 %s，实际 %v", id, llm)
		}
	}

	// codebuddy 系 2026-09-28 起改成**独立安装**的 CodeBuddy Code CLI
	//（不再是桌面 GUI 应用）→ 有可执行安装路径，因此有 engine.install。
	for _, e := range []Engine{&CodeBuddyEngine{}, &CodeBuddyAIEngine{}} {
		if cb := capabilitiesOfEngine(e); !hasCap(cb, CapEngineInstall) {
			t.Errorf("%s 应有能力 %s（npm 一键安装命令），实际 %v", e.Name(), CapEngineInstall, cb)
		}
	}
	// 反例：没有可执行安装路径的引擎不给 engine.install
	//（codebuddy-gateway 要的不是安装，而是「把网关跑起来」）。
	if gw := capabilitiesOfEngine(&CodeBuddyGatewayEngine{}); hasCap(gw, CapEngineInstall) {
		t.Errorf("codebuddy-gateway 不应有能力 %s（无安装命令），实际 %v", CapEngineInstall, gw)
	}

	// 走路径降级的引擎：给 attachment.prompt 而不是 attachment.native
	for _, tc := range []struct {
		name string
		e    Engine
	}{
		{"trae", &TraeEngine{}},
		{"openclaw", &OpenClawEngine{}},
		{"dsh", &DshEngine{}},
	} {
		caps := capabilitiesOfEngine(tc.e)
		if !hasCap(caps, CapAttachmentPrompt) {
			t.Errorf("%s 应有能力 %s（附件走提示词路径），实际 %v", tc.name, CapAttachmentPrompt, caps)
		}
		if hasCap(caps, CapAttachmentNative) {
			t.Errorf("%s 不应有能力 %s，实际 %v", tc.name, CapAttachmentNative, caps)
		}
	}

	// arkclaw 的附件走 A2A 原生 file part → 应给出 attachment.native。
	// 这条直接查表：ArkClawEngine 的名字来自配置（不是常量），不适合直接构造。
	if got := AttachmentSupportOf("arkclaw"); got != "part:file" {
		t.Errorf("arkclaw attachments = %q want part:file（原生 file part → attachment.native）", got)
	}
}

// TestCoreFeaturesSorted 核心级通道能力要干净可用（客户端拿它判断 core 支不支持某条通道）。
func TestCoreFeaturesSorted(t *testing.T) {
	f := CoreFeatures()
	if len(f) == 0 {
		t.Fatal("CoreFeatures 为空")
	}
	if !sort.StringsAreSorted(f) {
		t.Errorf("CoreFeatures 未排序: %v", f)
	}
	for i := 1; i < len(f); i++ {
		if f[i] == f[i-1] {
			t.Errorf("CoreFeatures 有重复项: %s", f[i])
		}
	}
	for _, want := range []string{FeatureSessionEvents, FeatureSessionControl} {
		if !hasCap(f, want) {
			t.Errorf("CoreFeatures 缺 %s，实际 %v", want, f)
		}
	}
	// 核心级能力不能混进引擎能力白名单（两者是两个问题）。
	for _, id := range CapabilityIDs() {
		if strings.HasPrefix(id, "feature.") {
			t.Errorf("CapabilityIDs 不应含核心级能力 %q", id)
		}
	}
}

// TestCapabilityIDsDedupedAndSorted 能力全集本身要干净：无重复、已排序
// （客户端与服务端都可能拿它做校验白名单）。
func TestCapabilityIDsDedupedAndSorted(t *testing.T) {
	ids := CapabilityIDs()
	if len(ids) == 0 {
		t.Fatal("CapabilityIDs 为空")
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("CapabilityIDs 未排序: %v", ids)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			t.Errorf("CapabilityIDs 有重复项: %s", ids[i])
		}
	}
}
