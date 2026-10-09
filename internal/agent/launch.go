package agent

// launch.go - 跨平台的「怎么把引擎进程拉起来」包装层（平台无关部分）。
//
// ## 为什么需要这一层
//
// 引擎 CLI 有两种落地形态：
//
//	npm 全局装的 shim：Windows 上是 claude.cmd / codebuddy.cmd（批处理），
//	                  macOS/Linux 上是 claude / codebuddy（无扩展名的脚本）
//	独立安装的原生二进制：trae-cli.exe / dsh 等
//
// **批处理不能被直接 exec**。Windows 的 exec.Command 遇到 `.cmd` 会走
// cmd.exe 解析层，参数里的非 ASCII 字符会被代码页转换吃掉（中文 → 乱码），
// `&` `|` `^` `%` 会被当命令语法解析，子进程拿不到原始输入。
//
// 实测的真凶（2026-10-01，对照矩阵，提示词含换行+中文+%&^+引号）：
//
//	裸 claude.exe 直连        → 位置参数、stdin 两条路都完好无损
//	claude.cmd 直连 + 位置参数 → 中文乱码、`&` 触发「'香蕉' 不是内部或外部命令」
//	claude.cmd 直连 + stdin    → 完好（stdin 走字节流，不过参数解析层）
//
// ⚠️ 换行**不是**元凶（.cmd 下 LF/CRLF 都正常）。换行只是最容易被肉眼
// 发现的触发点，真正破坏内容的是编码转换与元字符解析。
//
// agents-anywhere 对同一问题的处理见
// `anywhere-labs/Agents-Anywhere` 的 `connector/connector/launch.py:48-56`：
// `.cmd`/`.bat` 一律不直接 exec，改包成
// `powershell -NoProfile -ExecutionPolicy Bypass -Command "& 'path' 'arg1' …"`，
// 参数用**单引号**转义（单引号写两个）。本文件是它的 Go 等价物。
//
// ## 本层解决什么、不解决什么
//
// 解决：**引号层面的**参数传递（launchWrap 只在 Windows 上生效，
// unix 直连，行为零变化）。
//
// 不解决：**长度/结构层面**的提示词传递 —— 提示词不该当位置参数传。
// 那是引擎各自的选择，见 claude.go / codebuddy.go 的「提示词走 stdin」。
// 两层是互补的：stdin 解决内容，launchWrap 解决剩下的 flag 传递。

import (
	"path/filepath"
	"strings"
)

// launchSpec 描述一次「最终怎么 exec」的决策结果。
type launchSpec struct {
	// bin 真正要交给 exec.Command 的可执行文件。
	bin string
	// args 真正要交给 exec.Command 的参数（已按 launcher 变形）。
	args []string
	// wrapped 标记是否经过了 PowerShell 包装（诊断用；false = 原样直连）。
	wrapped bool
}

// windowsBatchExt 是 Windows 上**不能直接 exec** 的脚本后缀。
//
// `.ps1` 需要 ExecutionPolicy Bypass 才能跑，`.cmd`/`.bat` 需要 cmd.exe
// 解析层 —— 两者都要包一层 PowerShell 才安全。
var windowsBatchExt = map[string]bool{
	".cmd": true,
	".bat": true,
	".ps1": true,
}

// needsPowerShellWrap 报告这个可执行文件在当前平台是否需要 PowerShell 包装。
//
// 判定依据是**后缀**而非「能不能执行」：npm 装的 shim 在 Windows 上
// 一定是 .cmd，即便 PATH 里 shim 指向的可能是别的东西。
func needsPowerShellWrap(bin string) bool {
	if !platformIsWindows() {
		return false
	}
	return windowsBatchExt[strings.ToLower(filepath.Ext(bin))]
}

// launchWrap 按平台把「引擎可执行文件 + 它的参数」变成可直接 exec 的形式。
//
// unix：原样返回（零行为变化）。
// Windows + 原生 exe：原样返回。
// Windows + .cmd/.bat/.ps1：包成
//
//	powershell -NoProfile -ExecutionPolicy Bypass -Command
//	  "& 'C:\path\claude.cmd' 'arg1' 'arg2'"
//
// 空串 flag 由调用方通过 emptyStringFlags 声明（见 emptyArgAsFlagValue 的说明）。
//
// ⚠️ 为什么不用 cmd /c 包一层（更"原生"的做法）：cmd 的引号规则在参数里
// 出现时无法可靠转义 —— % 是变量展开、^ 是转义符、& 是命令分隔符，
// 提示词里任何一个都可能踩到。PowerShell 的单引号字符串是字面量，
// 唯一的转义就是把单引号写成两个，语义上没有二义性。这与 agents-anywhere 的
// launch.py 选 powershell 而非 cmd 是同一个理由。
func launchWrap(bin string, args []string) launchSpec {
	if !platformIsWindows() {
		return launchSpec{bin: bin, args: args}
	}
	// 先把「flag + 空串」改写成 flag=，再进包装 —— 顺序不能反：
	// 包装后参数已成 PowerShell 脚本文本，空串已无处安放。
	if len(emptyStringFlags) > 0 {
		for _, f := range emptyStringFlags {
			args = emptyArgAsFlagValue(args, f)
		}
	}
	if !needsPowerShellWrap(bin) {
		return launchSpec{bin: bin, args: args}
	}
	script := powershellInvokeScript(bin, args)
	return launchSpec{
		bin:     powershellBin(),
		args:    []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script},
		wrapped: true,
	}
}

// emptyStringFlags 全局登记：本项目里哪些 flag 会以**空串**作为值。
//
// 为什么要全局而不是按引擎传：改写发生在 launchWrap 这一层，
// 而引擎各自的 buildArgs 只知道「我要传 --tools 空串」，
// 不该知道「Windows 上这么传会丢」。分层就在这里 —— 引擎管语义，
// launch 管平台怎么落地。
//
// ⚠️ 这是一张**白名单**，不是「所有空串」的通用规则：
// 未来新增「用空串表达含义」的 flag 时必须登记进来，否则在 Windows 上
// 会静默丢参数（症状：该 flag 被 CLI 忽略，行为退回默认）。
var emptyStringFlags = []string{"--tools"}

// powershellInvokeScript 拼出 PowerShell 的调用脚本：
// `& '脚本路径' '参数1' '参数2' …`
//
// & 是 PowerShell 的调用运算符（"执行这个"），必须保留；
// 路径与每个参数都用单引号包起来，单引号自身写成两个。
func powershellInvokeScript(bin string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "&", powershellQuote(bin))
	for _, a := range args {
		parts = append(parts, powershellQuote(a))
	}
	return strings.Join(parts, " ")
}

// powershellQuote PowerShell 单引号字符串字面量。
//
// 单引号字符串内**只有两种转义**：” 表示一个字面单引号，
// 其余（含 $ ` " ）全部按字面量处理 —— 这正是我们要的：提示词里的
// 美元符号、反引号、双引号都不用操心。
func powershellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// emptyArgAsFlagValue 把「flag + 空串值」这一对参数改写成 `flag=` 形式。
//
// ## 为什么必须改写（2026-10-01 实测，本文件存在的第二个理由）
//
// 引擎 CLI 里有一批「用空串表达含义」的 flag，claude 的 `--tools ""` 就是
// 典型（空串 = 禁用全部内置工具，见 claude.go buildArgsBase 的 toolsIsOff 分支）。
//
// 传法有两种，**只有一种在 Windows 上活着**：
//
//	--tools ""     ❌ 0 行输出、~320ms 挂
//	--tools=       ✅ 正常输出
//
// ⚠️ 机制不是「cmd.exe 吞空串」—— 2026-10-01 的对照实测（裸 claude.exe
// 直连，绕开一切 shim）里 `--tools ""` 一样挂，所以**这跟 .cmd/cmd.exe
// 无关**。真实机制是：**Windows 命令行本身无法可靠传递空串参数**。
// Go 的 `syscall.EscapeArg` 把空串编成 `""`，可命令行解析层不保证把它
// 当成一个实参传下去（本机实测就是被丢弃），于是 CLI 收到的 argv 表
// 少了一个元素 → 后续位置参数**整体错位** → 表现为「认不出 prompt」：
//
//	Input must be provided either through stdin or as a prompt argument
//
// 2×2 交叉实测（单行 / 多行 / 单换行 × split / merged）结论一致：
// 只要是独立空串 argv 就 ~320ms 挂，**与换行、与中文都无关**。
// `-p ""`（flag 自己吃空串）同样挂；而 `--model ""` + `--tools=` 正常 ——
// 说明触发条件是「flag 与空串相邻」这一形状，不是某个具体 flag。
//
// `--tools=` 只有一个 argv 元素、不含空串，绕开了这个坑，且对 CLI 等价
// （所有这些 CLI 都同时接受 `--flag value` 与 `--flag=value` 两种写法）。
//
// unix 上不做改写：argv 原样传，空串本来就是合法的空参数。
func emptyArgAsFlagValue(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		// 命中 `flag` + 空串值这一对 → 合并成 `flag=`
		if args[i] == flag && i+1 < len(args) && args[i+1] == "" {
			out = append(out, flag+"=")
			i++ // 跳过被合并的空串
			continue
		}
		out = append(out, args[i])
	}
	return out
}
