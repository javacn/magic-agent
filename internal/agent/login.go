package agent

// login.go - 交互式登录入口的抽象（`magic-agent --login <engine>`）。
//
// 为什么要有这一层：登录态由各引擎的 CLI 自己管，而**登录时必须带上哪个账号**
// 只有引擎自己知道 —— codebuddy / codebuddy-ai 跑的是同一个 CLI，靠
// ACC_PRODUCT_CONFIG_V3 切 authentication.id（见 codebuddy.go 的账号隔离一节）。
// 手工拼这几个环境变量太容易错，错了就会把登录结果写进别的账号的票据文件。
//
// 于是约定：引擎把「怎么拉起来」讲清楚（bin / args / extraEnv），
// CLI 层只负责 exec 并把 stdio 直通给用户。

// LoginRunner 由「支持交互式登录」的引擎实现。
//
// 只有需要额外账号环境的引擎才实现它；其余引擎的登录态就是 CLI 自己的事
// （直接运行那个 CLI 即可），CLI 层会给出明确提示而不是硬造一条命令。
type LoginRunner interface {
	// LoginCommand 返回拉起交互式登录会话所需的三元组：
	//
	//	bin      可执行文件路径
	//	args     启动参数（通常为空 —— 裸启动即交互式界面，在里面执行 /login）
	//	extraEnv 追加到当前环境之上的环境变量（KEY=VALUE；重复 key 取后者）
	//
	// 返回的 extraEnv 会由 ChildEnvWith 与本进程环境合并（同样按 env.go 的规则
	// 剔除父进程专属变量）。
	LoginCommand() (bin string, args []string, extraEnv []string, err error)
}
