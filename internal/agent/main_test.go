package agent

// main_test.go - 包级测试隔离。
//
// ⚠️ 为什么需要：llm 引擎现在会读**用户真实机器**上的 `~/.magic-agent/models.json`
//（见 llmmodels.go）：`-m <id>` 命中即直连端点。若不隔离，单测会
//  ① 把本机注册表当成"假 CLI 的输出"（TestEngineListModelsWithFakeCLI 断言失败）
//  ② 更糟：请求真的打到线上端点（实测跑出过一次真实模型回答）
//
// 做法：把 MAGIC_AGENT_MODELS 指到一个不存在的临时路径 → 直连链路整体不生效，
// 所有 CLI 路径的单测回到"完全可控"的假 CLI 语义。
// 需要测直连链路的用例（llmhttp_test.go）自己用 t.Setenv 覆盖这个变量。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	os.Setenv("MAGIC_AGENT_MODELS", filepath.Join(os.TempDir(), "magic-agent-test-no-models.json"))
	os.Exit(m.Run())
}
