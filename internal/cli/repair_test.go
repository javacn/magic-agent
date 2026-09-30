package cli

// repair_test.go - `--repair-models` 的参数校验。
//
// 落地逻辑（形状识别 / 还原 / 备份 / 幂等）在 internal/agent/repair_models_test.go，
// 这里只锁 CLI 层的契约：引擎名缺失或不支持桌面端时是**参数错**（exit 2），
// 而不是被当成 prompt 发给引擎。

import (
	"strings"
	"testing"
)

func TestRepairModelsWithoutEngineNameIsUsageError(t *testing.T) {
	_, _, err := runAskCmd(t, "", "--repair-models")
	if err == nil {
		t.Fatal("--repair-models 缺引擎名应报错")
	}
	if !strings.Contains(err.Error(), "需要引擎名") {
		t.Errorf("应是中文用法提示，实际 %q", err.Error())
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("应是 usageError（exit 2），实际 %T", err)
	}
}

// 显式给空串同样走校验（与 --login 同一取舍：用「传过没有」而不是「值非空」判断）。
func TestRepairModelsEmptyEngineIsUsageError(t *testing.T) {
	_, _, err := runAskCmd(t, "", "--repair-models", "")
	if err == nil {
		t.Fatal("空引擎名应报错")
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("应是 usageError（exit 2），实际 %T", err)
	}
}

// 没有桌面端数据目录的引擎：参数错，并点名可用值。
func TestRepairModelsUnsupportedEngine(t *testing.T) {
	_, _, err := runAskCmd(t, "", "--repair-models", "claude")
	if err == nil {
		t.Fatal("claude 没有桌面端 models.json，应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "没有对应的桌面端数据目录") {
		t.Errorf("错误应说明该引擎没有桌面端目录，实际 %q", msg)
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("应是 usageError（exit 2），实际 %T", err)
	}
}
