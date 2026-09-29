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

	// stdio 直通：交互式界面必须拿到真的 TTY。
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = agent.ChildEnvWith(extraEnv)
	if wd, werr := os.Getwd(); werr == nil {
		c.Dir = wd
	}

	// 提示走 stderr：stdout 留给子进程的交互界面，不被污染。
	fmt.Fprintf(cmd.ErrOrStderr(),
		"magic-agent: 已按 %s 的账号环境启动 %s\n"+
			"            在里面执行 /login 完成登录（退出后本命令结束）\n", eng.Name(), bin)

	if runErr := c.Run(); runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return fmt.Errorf("%s 会话退出码 %d", eng.Name(), ee.ExitCode())
		}
		return fmt.Errorf("启动 %s 失败: %w", bin, runErr)
	}
	return nil
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
