package cli

// login.go - `--login <engine>`：拉起某引擎自己的交互式登录会话。
//
// 为什么需要它：codebuddy / codebuddy-ai 跑的是**同一个 CLI 的两个账号**，
// 登录时必须带上各自的账号环境（配置目录 + ACC_PRODUCT_CONFIG_V3 里的
// authentication.id），否则登录结果会写进**另一个账号**的票据文件。
// 这几个变量只有引擎自己知道（见 internal/agent/login.go 的 LoginRunner），
// 所以这里只负责 exec 与 stdio 直通，不重复拼环境。

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/darren/magic-agent/internal/agent"
	"github.com/spf13/cobra"
)

// runLogin 处理 `--login <engine>`。
func runLogin(cmd *cobra.Command, opts *askOptions) error {
	name := strings.TrimSpace(opts.login)
	if name == "" {
		return &usageError{fmt.Errorf("--login 需要引擎名：magic-agent --login <engine>（可用：%s）", loginCapableEngines())}
	}
	eng := agent.Lookup(name)
	if eng == nil {
		return &usageError{fmt.Errorf("未知引擎 %q；可用：%s", name, loginCapableEngines())}
	}
	lr, ok := eng.(agent.LoginRunner)
	if !ok {
		return &usageError{fmt.Errorf(
			"引擎 %s 不需要 --login：它的登录态由自己的 CLI 管理，直接运行该 CLI 登录即可", eng.Name())}
	}

	bin, args, extraEnv, err := lr.LoginCommand()
	if err != nil {
		return err
	}

	/* stdio 直通：交互式界面必须拿到真的 TTY。
	   ⚠️ 这里**刻意不设进程组**（普通引擎调用走 configureProcAttr 自成一组）：
	   交互式 TUI 必须在**前台进程组**里，否则它从 tty 读输入会被 SIGTTIN 停住 ——
	   表现就是「一进登录界面就卡住」。代之以「登记子进程 + 退出前带走」的清理
	   （见 loginChild / stopLoginChild），保证它不会变成占着账号的孤儿。 */
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = agent.ChildEnvWith(extraEnv)
	if wd, werr := os.Getwd(); werr == nil {
		c.Dir = wd
	}

	// 提示走 stderr：stdout 留给子进程的交互界面，不被污染。
	// 第三行是给「上一次登录没成功」的情形：残留的登录态得先清掉，否则会一直登不上。
	fmt.Fprintf(cmd.ErrOrStderr(),
		"magic-agent: 已按 %s 的账号环境启动 %s\n"+
			"            在里面执行 /login 完成登录（退出后本命令结束）\n"+
			"            若上一次登录没成功、这次进去仍登不上：先执行 /logout 清掉残留登录态，再执行 /login\n",
		eng.Name(), bin)

	if startErr := c.Start(); startErr != nil {
		return fmt.Errorf("启动 %s 失败: %w", bin, startErr)
	}
	setLoginChild(c)
	defer clearLoginChild()

	if runErr := c.Wait(); runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return fmt.Errorf("%s 会话退出码 %d", eng.Name(), ee.ExitCode())
		}
		return fmt.Errorf("%s 会话异常: %w", eng.Name(), runErr)
	}
	return nil
}

/* ── 交互式登录子进程的清理登记 ──────────────────────────────
 *
 * 为什么单独记一笔：登录是**唯一**不走进程组管理的子进程（理由见上面 runLogin 的
 * 注释 —— 交互式 TUI 必须留在前台组里）。因此它既不在会话登记表（--stop 管不到），
 * 也不会被 installSignalStop 的「杀会话」路径带走。一旦 magic-agent 被 SIGTERM
 * （桌面壳的「停止」按钮、`kill <pid>`）终止，这个 CLI 就成孤儿，继续占着该账号的
 * 配置目录 / 端口，下一次 --login 起来会撞上残留状态 —— 用户看到的就是
 * 「登录失败后再也登不上」。所以退出前必须补一刀。
 */
var (
	loginMu    sync.Mutex
	loginChild *exec.Cmd
)

// setLoginChild 登记正在跑的登录子进程。
func setLoginChild(c *exec.Cmd) {
	loginMu.Lock()
	loginChild = c
	loginMu.Unlock()
}

// clearLoginChild 注销（登录会话正常结束 / 启动失败时调用）。
func clearLoginChild() {
	setLoginChild(nil)
}

/* stopLoginChild 杀掉正在跑的登录子进程（若在），返回是否真的动了手。
 *
 * 用**单进程**信号（group=false）：它本来就与 magic-agent 同组，负 pid 会把
 * magic-agent 自己一起杀掉。
 */
func stopLoginChild() bool {
	loginMu.Lock()
	c := loginChild
	loginChild = nil
	loginMu.Unlock()
	if c == nil || c.Process == nil {
		return false
	}
	_ = agent.KillPID(c.Process.Pid, false)
	return true
}

// loginCapableEngines 列出实现了 LoginRunner 的引擎名（报错提示用）。
func loginCapableEngines() string {
	var names []string
	for _, e := range agent.Engines() {
		if _, ok := e.(agent.LoginRunner); ok {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "(无)"
	}
	return strings.Join(names, " / ")
}
