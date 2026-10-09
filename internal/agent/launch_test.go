package agent

// launch_test.go - 平台包装层的回归测试（2026-10-01）。
//
// 这组断言守的是两个**静默**退化：参数被 cmd.exe 吞掉、思维链混进正文。
// 两者都不报错、退出码都是 0，只能靠断言挡住。

import (
	"strings"
	"testing"
)

// TestEmptyArgAsFlagValue 「flag + 空串」必须改写成 flag=。
//
// 这是 Windows 上必需的一步：--tools "" 的空串是独立 argv 元素，
// 经 .cmd 传参时被丢弃，CLI 收到残缺参数表后表现是「认不出 prompt」。
func TestEmptyArgAsFlagValue(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"独立空串改写", []string{"-p", "--tools", "", "--model", "m"},
			[]string{"-p", "--tools=", "--model", "m"}},
		{"末尾空串", []string{"--tools", ""}, []string{"--tools="}},
		{"无空串不动", []string{"--tools", "Read,Bash"}, []string{"--tools", "Read,Bash"}},
		{"flag 不误伤其他值", []string{"--model", "", "--tools", "x"},
			[]string{"--model", "", "--tools", "x"}},
		{"空 flag 不匹配", []string{"--tool", ""}, []string{"--tool", ""}},
		{"空数组", []string{}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := emptyArgAsFlagValue(c.in, "--tools")
			if len(got) != len(c.want) {
				t.Fatalf("长度 = %d(%q)，want %d(%q)", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("第 %d 个 = %q，want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestPowershellQuoteLiteral 提示词里的元字符必须原样保留。
//
// PowerShell 单引号字符串里只有一种转义（”），所以 $ ` " 都自动安全。
// 这条断言是「不要偷懒改成双引号字符串」的护栏 —— 换成双引号后
// $var 会被 PowerShell 展开，提示词内容就变了。
func TestPowershellQuoteLiteral(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "'abc'"},
		{"", "''"},
		{"$env:PATH", "'$env:PATH'"},
		{"say \"hi\"", `'say "hi"'`},
		{"back`tick", "'back`tick'"},
		{"it's", "'it''s'"},
		{"a\nb", "'a\nb'"}, // 换行在单引号内是字面量，不需转义
	}
	for _, c := range cases {
		if got := powershellQuote(c.in); got != c.want {
			t.Errorf("powershellQuote(%q) = %s，want %s", c.in, got, c.want)
		}
	}
}

// TestPowerShellScriptShape 生成的调用脚本形状正确（& 调用运算符 + 逐参数单引号）。
func TestPowerShellScriptShape(t *testing.T) {
	got := powershellInvokeScript(`C:\a b\claude.cmd`, []string{"-p", "it's", "x\ny"})
	want := `& 'C:\a b\claude.cmd' '-p' 'it''s' 'x` + "\n" + `y'`
	if got != want {
		t.Fatalf("got  = %q\nwant = %q", got, want)
	}
	// 换行绝不能把脚本截断（PowerShell -Command 按行解析）。
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "'x\ny'") {
		t.Fatalf("换行未被安全包住：%q", got)
	}
}

// TestNeedsPowerShellWrap 后缀判定。
//
// 期望值按**运行平台**取值：unix 上 needsPowerShellWrap 一律 false
// （launchWrap 直接原样返回），Windows 上才看后缀。
// 写死 Windows 的期望会让本文件在 macOS/CI(linux) 上必然红。
func TestNeedsPowerShellWrap(t *testing.T) {
	wrap := platformIsWindows() // 平台总开关
	cases := []struct {
		bin      string
		wantWrap bool // 仅当 wrap == true 时才参与断言
	}{
		{`C:\npm\claude.cmd`, true},
		{`C:\npm\foo.BAT`, true},
		{`C:\npm\script.ps1`, true},
		{`C:\trae\trae-cli.exe`, false},
		{`/opt/homebrew/bin/claude`, false},
		{"claude", false},
		{"", false},
		{"weird.cmd.txt", false}, // 只看最后一个后缀
	}
	for _, c := range cases {
		want := c.wantWrap && wrap
		if got := needsPowerShellWrap(c.bin); got != want {
			t.Errorf("needsPowerShellWrap(%q) = %v，want %v（platformIsWindows=%v）",
				c.bin, got, want, wrap)
		}
	}
}

// TestLaunchWrapKeepsOriginalWhenNoWrap 明确「不该包装时绝不包装」。
//
// unix 上 launchWrap 必须原样返回：任何改写都会静默改变既有行为
// （尤其不能改写空串 flag —— 那是 Windows 专属改写）。
func TestLaunchWrapKeepsOriginalWhenNoWrap(t *testing.T) {
	if platformIsWindows() {
		t.Skip("Windows 上 .cmd 会被包装，本断言只对 unix 成立")
	}
	in := []string{"-p", "--tools", "", "prompt\nwith\nnewlines"}
	spec := launchWrap("/opt/homebrew/bin/claude", in)
	if spec.wrapped {
		t.Fatal("unix 上不应包装")
	}
	if spec.bin != "/opt/homebrew/bin/claude" {
		t.Fatalf("bin = %q，应原样保留", spec.bin)
	}
	if len(spec.args) != len(in) {
		t.Fatalf("args = %q，应原样保留（含空串）", spec.args)
	}
	// 逐项比对：空串必须还在原位（launchWrap 不许碰 unix 的 argv）。
	for i := range in {
		if spec.args[i] != in[i] {
			t.Fatalf("第 %d 个 = %q，want %q", i, spec.args[i], in[i])
		}
	}
}

// TestLaunchWrapNativeExeUntouched Windows 上：原生 .exe 不包装、但空串仍改写。
//
// 这条挡的是「把所有 Windows bin 都包一层 PowerShell」的偷懒改法 ——
// 那样会多出一个常驻 powershell 进程，进程组杀、超时、信号转发全部多一跳，
// 回归面极大。原生 exe 必须直连。
func TestLaunchWrapNativeExeUntouched(t *testing.T) {
	if !platformIsWindows() {
		t.Skip("本断言只对 Windows 成立")
	}
	spec := launchWrap(`C:\trae\trae-cli.exe`, []string{"-p", "--tools", "", "x"})
	if spec.wrapped {
		t.Fatal(".exe 不该被包装成 PowerShell")
	}
	if spec.bin != `C:\trae\trae-cli.exe` {
		t.Fatalf("bin = %q", spec.bin)
	}
	// 空串改写仍生效（它不依赖包装，是 argv 层面的修补）。
	joined := strings.Join(spec.args, "\x00")
	if strings.Contains(joined, "--tools\x00\x00") {
		t.Fatalf("空串未被改写：%q", spec.args)
	}
	if !strings.Contains(joined, "--tools=") {
		t.Fatalf("缺少 --tools=：%q", spec.args)
	}
}

// TestLaunchWrapWindowsScriptShape Windows 上：包装后的参数形状必须可 exec。
//
// 精确断言整个 argv（不是 contains 弱匹配）—— 参数顺序/个数错了
// PowerShell 会静默执行错的命令，而这正是本层要防的那类失败。
func TestLaunchWrapWindowsScriptShape(t *testing.T) {
	if !platformIsWindows() {
		t.Skip("本断言只对 Windows 成立")
	}
	spec := launchWrap(`C:\npm\claude.cmd`, []string{"-p", "--tools", "", "multi\nline"})
	if !spec.wrapped {
		t.Fatal(".cmd 应被包装成 PowerShell")
	}
	if spec.bin != powershellBin() {
		t.Fatalf("bin = %q，want %q", spec.bin, powershellBin())
	}
	wantArgs := []string{
		"-NoProfile",
		"-ExecutionPolicy",
		"Bypass",
		"-Command",
		`& 'C:\npm\claude.cmd' '-p' '--tools=' 'multi` + "\n" + `line'`,
	}
	if len(spec.args) != len(wantArgs) {
		t.Fatalf("args = %q\nwant = %q", spec.args, wantArgs)
	}
	for i := range wantArgs {
		if spec.args[i] != wantArgs[i] {
			t.Fatalf("第 %d 个 = %q，want %q", i, spec.args[i], wantArgs[i])
		}
	}
	// 独立空参数绝不能出现在脚本里（cmd.exe 会把它吞掉）。
	script := spec.args[len(spec.args)-1]
	if strings.Contains(script, "''") {
		t.Fatalf("脚本里出现独立空参数：%q", script)
	}
	// 多行提示词必须仍是**一个**引号包裹的参数，不能把 -Command 脚本截断。
	if !strings.Contains(script, "'multi\nline'") {
		t.Fatalf("多行提示词未完整保留：%q", script)
	}
	if strings.Count(script, "\n") != 1 {
		t.Fatalf("脚本被换行截断：%q", script)
	}
}
