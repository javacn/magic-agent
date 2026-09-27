/* Package boundary 是「桌面是可选插件」这条约束的自动化守卫（P1 验收要求的 CI 断言）。
 *
 * 方案里的判据写的是「装与不装插件时主二进制 hash、体积、`--engines` 输出一致」。
 * 直接比 hash 要构建两次、还得人为造出「不装」的状态 —— 又慢又脆，而且真出事时
 * 它只告诉你「不一样」，不告诉你为什么。
 *
 * 这里改为断言**产生那些结果的源头**：主二进制是「源码 + 依赖闭包」的纯函数，
 * 所以只要下面两条成立，hash / 体积 / 输出就必然一致：
 *
 *   1) core 的依赖闭包里没有插件（`internal/client`）；
 *   2) 仓库里没有任何 `//go:embed` 把插件的 UI 资源嵌进 core。
 *
 * 任一条破了，都说明有人把插件塞进了 core —— 那是这条约束的实质违反。
 * 这两条断言比 hash 对比更早失败、且报错直接指向原因。
 */
package boundary

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// corePkg 主二进制的包路径。断言都围绕它展开。
const corePkg = "./cmd/magic-agent"

// pluginPkgs 只允许出现在插件侧、绝不允许进入 core 依赖闭包的包。
var pluginPkgs = []string{
	"github.com/darren/magic-agent/internal/client",
}

// repoRoot 从当前包目录向上找到含 go.mod 的那一层。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录失败: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("向上找不到 go.mod")
		}
		dir = parent
	}
}

// goListDeps 列出某个包的全部依赖（含标准库）。
func goListDeps(t *testing.T, root, pkg string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("本机没有 go，跳过边界断言（CI 里必有）")
	}
	cmd := exec.Command("go", "list", "-deps", pkg)
	cmd.Dir = root
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list -deps %s 失败: %v\n%s", pkg, err, errBuf.String())
	}
	var deps []string
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			deps = append(deps, line)
		}
	}
	if len(deps) == 0 {
		t.Fatal("go list -deps 输出为空，断言失去意义")
	}
	return deps
}

// TestCoreDoesNotDependOnPlugin core 的依赖闭包里不许出现插件包。
//
// 这是「可选插件」的前提：一旦 core 依赖了插件，不装插件就跑不起来，
// 而且插件的依赖（HTTP 服务、静态资源、profile）会被动变成 core 的负担。
func TestCoreDoesNotDependOnPlugin(t *testing.T) {
	root := repoRoot(t)
	deps := goListDeps(t, root, corePkg)

	// 先确认没有写错包路径：插件包本身必须存在于依赖解析里（用 go list 单查一次）。
	if _, err := exec.LookPath("go"); err == nil {
		cmd := exec.Command("go", "list", pluginPkgs[0])
		cmd.Dir = root
		if err := cmd.Run(); err != nil {
			t.Fatalf("插件包 %s 不存在或无法解析，本断言的包名需要更新: %v", pluginPkgs[0], err)
		}
	}

	for _, dep := range deps {
		for _, bad := range pluginPkgs {
			if dep == bad || strings.HasPrefix(dep, bad+"/") {
				t.Errorf("core 依赖了插件包 %s —— 违反了「桌面是可选插件」：\n"+
					"core（%s）只能依赖 CLI 集成相关的东西，插件要另起命令。", dep, corePkg)
			}
		}
	}
}

// TestCoreDoesNotEmbedPluginAssets 仓库里不许有 //go:embed 把插件的 UI 资源嵌进 core。
//
// 为什么单看 //go:embed：资源一旦被 embed，就会同时进入**所有** cmd 的二进制，
// 体积与 hash 都会变，而且 embed 的路径看不出「这是插件的」，最容易破线。
func TestCoreDoesNotEmbedPluginAssets(t *testing.T) {
	root := repoRoot(t)

	// 插件资源目录（相对仓库根）。任何 embed 指令都不许点到它们。
	forbiddenInEmbed := []string{"client-ui", "magic-client-mobile-ui"}

	var scanned int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "npm":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		scanned++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "//go:embed") {
				continue
			}
			for _, bad := range forbiddenInEmbed {
				if strings.Contains(line, bad) {
					t.Errorf("%s 里的 embed 指令指向了插件资源：\n  %s\n"+
						"插件资源（%s）只能由插件自己的命令持有。", rel, line, bad)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描仓库失败: %v", err)
	}
	if scanned == 0 {
		t.Fatal("一个 .go 文件都没扫到，断言失去意义（工作目录不对？）")
	}
}
