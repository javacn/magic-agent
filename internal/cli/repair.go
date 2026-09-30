package cli

// repair.go - `--repair-models <engine>`：还原被反向同步写坏的桌面端 models.json。
//
// 为什么要有它：2026-09-30 的 Windows 事故里，magic-agent 把桌面端（WorkBuddy）那份
// **数组格式**的 models.json 覆盖成了 `{availableModels, models}` 对象格式，桌面端
// 解析不了 → 模型清单整个为空。同步那一侧已经加了「陌生形状一律不动」的硬约束，
// 但**已经坏掉的现场**需要一条能自己走回来的路 —— 否则用户只能手抄 apiKey。
//
// 还原逻辑在 agent.RepairDesktopModels（见 custom_models.go 的事故还原一节）：
// 只认明确的坏形状才动手，动手前先留 .broken.bak。

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/darren/magic-agent/internal/agent"
	"github.com/spf13/cobra"
)

// runRepairModels 处理 `--repair-models <engine>`。
func runRepairModels(cmd *cobra.Command, opts *askOptions) error {
	name := strings.TrimSpace(opts.repairModels)
	if name == "" {
		return &usageError{fmt.Errorf(
			"--repair-models 需要引擎名：magic-agent --repair-models <engine>（可用：%s）",
			strings.Join(agent.DesktopEngines(), " / "))}
	}

	// 引擎名先在这里校验：这是**参数错**（exit 2），而不是运行期失败（exit 1）。
	// agent 层还会再兜一次，但那属于「不该发生」的安全网。
	if agent.DesktopAppHomeOf(name) == "" {
		return &usageError{fmt.Errorf("引擎 %q 没有对应的桌面端数据目录，没有可修的 models.json（可用：%s）",
			name, strings.Join(agent.DesktopEngines(), " / "))}
	}

	repaired, note, err := agent.RepairDesktopModels(name)
	if err != nil {
		return err
	}

	// 机器可读的一行结果走 stdout（与 --sessions / --stop 同一族）；人看的说明走 stderr。
	if eerr := json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
		"engine":   name,
		"repaired": repaired,
		"note":     note,
	}); eerr != nil {
		return eerr
	}
	out := cmd.ErrOrStderr()
	fmt.Fprintf(out, "magic-agent: %s\n", note)
	if !repaired {
		// 没改动不是失败，但必须说清「没动」，不静默（本项目铁律）。
		fmt.Fprintf(out, "magic-agent: 未改动任何文件。形状对不上就不动，避免二次破坏。\n")
		return nil
	}
	fmt.Fprintf(out, "magic-agent: 已还原。请重开桌面端确认模型清单；官方模型由桌面端自己拉取，不在这个文件里。\n")
	return nil
}
