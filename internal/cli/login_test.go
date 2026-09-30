package cli

// login_test.go - `--login` 的参数校验与「退出前带走登录子进程」的清理契约。
//
// 这两件事都是「登录失败后再也登不上」那个用户问题的直接成因：
//   - 参数写错时给一句看得懂的中文（而不是 cobra 的英文裸报错，且退出码要对）；
//   - 登录 TUI 必须留在前台进程组里（否则读 tty 被 SIGTTIN 停住），代价是
//     magic-agent 被 SIGTERM / 关终端时必须主动把它带走，不能留成占着账号的孤儿。

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// `--login` 后面没跟引擎名：必须给出中文 usage（点名可用引擎），且算参数错（exit 2）。
// cobra 在解析阶段就报错（走不到 runLogin），所以这条靠 SetFlagErrorFunc 兜住。
func TestLoginWithoutEngineNameIsUsageError(t *testing.T) {
	_, _, err := runAskCmd(t, "", "--login")
	if err == nil {
		t.Fatal("--login 缺引擎名应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "需要引擎名") {
		t.Errorf("应是中文用法提示，实际 %q", msg)
	}
	/* ⚠️ 不断言「提示里点名了 codebuddy」：包内用例会在 Engines() 首次调用前
	   agent.Register 假引擎，触发 registry 的已知脆弱点（内置引擎整体不注册，
	   见 agent/engine.go 的守卫注释），那时可用引擎列表本来就是空的。
	   这里只锚定稳定契约：中文提示 + usage（exit 2）。 */
	if _, ok := err.(*usageError); !ok {
		t.Errorf("应是 usageError（exit 2），实际 %T", err)
	}
}

// 未知引擎名：usage 错误 + 列出可用引擎，不静默启一个空 bin。
func TestLoginUnknownEngine(t *testing.T) {
	_, _, err := runAskCmd(t, "", "--login", "no-such-engine")
	if err == nil {
		t.Fatal("未知引擎应报错")
	}
	if !strings.Contains(err.Error(), "未知引擎") {
		t.Errorf("错误应点名未知引擎，实际 %q", err.Error())
	}
	if _, ok := err.(*usageError); !ok {
		t.Errorf("应是 usageError（exit 2），实际 %T", err)
	}
}

// 不支持交互式登录的引擎：明确说不必用 --login，而不是硬造一条命令。
func TestLoginEngineWithoutLoginRunner(t *testing.T) {
	_, _, err := runAskCmd(t, "", "--login", "claude")
	if err == nil {
		t.Fatal("claude 不支持 --login，应报错")
	}
	if !strings.Contains(err.Error(), "不需要 --login") {
		t.Errorf("错误应说明该引擎不需要 --login，实际 %q", err.Error())
	}
}

// 登录子进程必须能被 stopLoginChild 收掉（进程真死）。
// 为什么值得测：登录是唯一不进进程组的子进程，孤儿化正是「登录失败后再也登不上」的成因。
//
// 判据用 c.Wait()（真回收）而不是 PIDAlive —— 被 SIGKILL 的子进程在没人 Wait 之前
// 是僵尸，仍然会被 kill(pid, 0) 判成「活着」，拿它当判据会假红。
func TestStopLoginChildKillsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 sleep 当靶子，Windows 另论")
	}
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Fatalf("起靶子进程失败: %v", err)
	}
	setLoginChild(c)

	if !stopLoginChild() {
		t.Fatal("stopLoginChild 应报告动了手")
	}

	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case werr := <-done:
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			t.Fatalf("靶子应被信号杀掉，实际 err=%v", werr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("登录子进程未被收掉（5s 内没退出）")
	}

	// 已经清空：再调一次不应再动手（幂等，避免误杀复用的 pid）。
	if stopLoginChild() {
		t.Error("注销后再调 stopLoginChild 不应再动手")
	}
}
