package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/darren/magic-agent/internal/agent"
)

// contractRow 是 --engines / --contract 每一行里本测试关心的字段。
type contractRow struct {
	Engine       string   `json:"engine"`
	OK           bool     `json:"ok"`
	Capabilities []string `json:"capabilities"`
	Models       []string `json:"models"`
}

// TestContractEnvelope -- `--contract` 的顶层形状与契约版本。
//
// 客户端启动时靠这两个东西做校验：contractVersion 决定"我能不能解析这份输出"，
// capabilities 决定"界面画成什么样"。
func TestContractEnvelope(t *testing.T) {
	stdout, _, err := runAskCmd(t, "", "--contract")
	if err != nil {
		t.Fatalf("--contract: %v", err)
	}
	var env struct {
		ContractVersion int           `json:"contractVersion"`
		Features        []string      `json:"features"`
		Engines         []contractRow `json:"engines"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("解析 --contract 输出失败: %v（原始输出：%s）", err, stdout)
	}
	if env.ContractVersion != agent.ContractVersion {
		t.Errorf("contractVersion = %d want %d", env.ContractVersion, agent.ContractVersion)
	}
	// 核心级通道能力：客户端据此判断这台 core 支不支持 --events / --control。
	for _, want := range []string{agent.FeatureSessionEvents, agent.FeatureSessionControl} {
		found := false
		for _, f := range env.Features {
			if f == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("features 缺 %s，实际 %v", want, env.Features)
		}
	}
	if got, want := len(env.Engines), len(agent.Engines()); got != want {
		t.Errorf("engines 行数 = %d want %d", got, want)
	}
	for _, row := range env.Engines {
		if row.Engine == "" {
			t.Errorf("有一行缺 engine 字段：%+v", row)
		}
		if len(row.Capabilities) == 0 {
			t.Errorf("%s: capabilities 为空（客户端会画不出这个引擎）", row.Engine)
		}
		/* ⚠️ --contract 默认不探测模型：每个引擎的模型探测都要起一次真 CLI 进程
		   （10 个引擎约 30s 级），客户端在启动路径上等不起。有人把默认改回探测时
		   这条会红，正好提醒他去看 runContract 的注释。 */
		if len(row.Models) != 0 {
			t.Errorf("%s: --contract 默认不应探测模型，却给出了 models=%v", row.Engine, row.Models)
		}
	}
}

// TestEnginesTopLevelStaysArray -- `--engines` 顶层必须仍是数组。
//
// 老调用方（含 magic-test 的 engines.cjs）按数组解析；契约信息走 --contract 的
// envelope，不能把老入口改成对象 —— 那是静默破坏。
func TestEnginesTopLevelStaysArray(t *testing.T) {
	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	trimmed := strings.TrimSpace(stdout)
	if !strings.HasPrefix(trimmed, "[") {
		t.Fatalf("--engines 顶层必须是 JSON 数组（老调用方按数组解析），实际开头是 %q", firstRunes(trimmed, 32))
	}
}

// TestEnginesCarriesCapabilities -- 老入口也要带 capabilities（纯加字段，向后兼容）。
func TestEnginesCarriesCapabilities(t *testing.T) {
	stdout, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}
	var rows []contractRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("解析 --engines 输出失败: %v（原始输出：%s）", err, stdout)
	}
	if len(rows) == 0 {
		t.Fatal("--engines 没有任何一行")
	}
	for _, row := range rows {
		if len(row.Capabilities) == 0 {
			t.Errorf("%s: --engines 每行都应带 capabilities，实际为空", row.Engine)
		}
	}
}

// TestContractAndEnginesRowsAgree -- 两个入口的行必须由同一份构建逻辑产出：
// 同名引擎的 **capabilities**（静态声明）应逐项一致。
//
// ⚠️ 刻意**不比对 ok / streaming / models**：它们是运行态，两次独立进程的探测结果
// 可以合法地不同（arkclaw 这类远端网关的 ok 取决于可达性与配置）。这也正是把
// capabilities 与运行态分开的原因 —— 静态的那部分必须稳定，动态的那部分本就会变。
func TestContractAndEnginesRowsAgree(t *testing.T) {
	contractOut, _, err := runAskCmd(t, "", "--contract")
	if err != nil {
		t.Fatalf("--contract: %v", err)
	}
	enginesOut, _, err := runAskCmd(t, "", "--engines", "--no-models")
	if err != nil {
		t.Fatalf("--engines: %v", err)
	}

	var env struct {
		Engines []contractRow `json:"engines"`
	}
	var rows []contractRow
	if err := json.Unmarshal([]byte(contractOut), &env); err != nil {
		t.Fatalf("解析 --contract 失败: %v", err)
	}
	if err := json.Unmarshal([]byte(enginesOut), &rows); err != nil {
		t.Fatalf("解析 --engines 失败: %v", err)
	}

	byName := map[string]contractRow{}
	for _, r := range rows {
		byName[r.Engine] = r
	}
	for _, c := range env.Engines {
		e, ok := byName[c.Engine]
		if !ok {
			t.Errorf("--contract 里的 %s 在 --engines 里没有", c.Engine)
			continue
		}
		if strings.Join(e.Capabilities, ",") != strings.Join(c.Capabilities, ",") {
			t.Errorf("%s: capabilities 不一致（--engines=%v / --contract=%v）",
				c.Engine, e.Capabilities, c.Capabilities)
		}
	}
}

// firstRunes 取前 n 个字符用于报错信息（避免把整段 JSON 打进日志）。
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
