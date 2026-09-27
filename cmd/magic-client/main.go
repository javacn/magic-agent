// magic-client 是 magic-agent 的**可选**桌面 / 移动端插件：提供基础版 Agent UI
// 与一组扩展接口，让业务项目（magic-test 等）只写 profile 与业务模块。
//
// 它不替代 core：引擎调用、事件归一、会话与控制通道全在 magic-agent 里。
// 本进程只做三件事 —— 启动时校验 core 契约、把 core 的 NDJSON 事件原样中继给客户端、
// 提供静态资源与扩展接口的落点。
//
// 用法：
//
//	magic-client serve --profile ./profiles/zhantianping.json --ui-dir ./client-ui
//	magic-client serve --print-url          # 只打印带令牌的地址，不启动（脚本用）
//
// 默认**只绑 127.0.0.1**，并需要 X-Magic-Token：同机任意进程都能访问本地端口，
// 不开鉴权等于把引擎执行权交给它们。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/darren/magic-agent/internal/client"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "用法: magic-client serve [--profile <path>] [--ui-dir <dir>] [--addr 127.0.0.1:0] [--token <t>]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var (
		profilePath = fs.String("profile", "", "profile JSON 路径（空 = 内置默认）")
		uiDir       = fs.String("ui-dir", "", "静态资源目录（基础版 UI 与业务模块）")
		addr        = fs.String("addr", "", "监听地址（默认 127.0.0.1:0；带 --lan 时是 0.0.0.0:0）")
		lan         = fs.Bool("lan", false, "绑到局域网（0.0.0.0），让手机 / 平板连得上。⚠️ 开了之后令牌是唯一门锁")
		token       = fs.String("token", "", "访问令牌（空 = 随机生成并打印；--lan 时至少要 24 个字符）")
		magicAgent  = fs.String("magic-agent", os.Getenv("MAGIC_AGENT_BIN"), "core 可执行文件（空 = 从 PATH 找 magic-agent）")
		printURL    = fs.Bool("print-url", false, "只打印带令牌的访问地址后退出（不启动服务）")
	)
	_ = fs.Parse(os.Args[2:])

	prof, err := client.LoadProfile(*profilePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "magic-client:", err)
		os.Exit(1)
	}
	tok := strings.TrimSpace(*token)
	if tok == "" {
		tok = randToken() // 16 字节 = 32 位十六进制，远强于局域网那条线
	}
	/* 绑到局域网时令牌是**唯一**门锁：内网能被扫到，短令牌等于没有。
	   默认生成的那个够强，所以这条只在用户显式给了弱令牌时报错。 */
	if *lan {
		if err := client.ValidateLanToken(tok); err != nil {
			fmt.Fprintln(os.Stderr, "magic-client:", err)
			os.Exit(2)
		}
	}
	svc, err := client.New(client.Options{
		MagicAgent: *magicAgent,
		Profile:    prof,
		UIRoot:     *uiDir,
		Token:      tok,
	})
	if err != nil {
		// 契约不匹配是**要立刻停**的错误：带着不匹配的能力跑起来只会让人以为是界面 bug。
		fmt.Fprintln(os.Stderr, "magic-client: 启动前检查失败:", err)
		os.Exit(1)
	}

	listenAddr := strings.TrimSpace(*addr)
	if listenAddr == "" {
		if *lan {
			listenAddr = "0.0.0.0:0"
		} else {
			listenAddr = "127.0.0.1:0"
		}
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "magic-client: 监听失败:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://127.0.0.1:%d/?token=%s", port, tok)
	if *printURL {
		// 只打印本机地址：脚本拿它做探活，局域网地址在 --lan 下另有输出。
		fmt.Println(url)
		_ = ln.Close()
		return
	}

	c := svc.Contract()
	fmt.Fprintf(os.Stderr, "magic-client: %s | core=%s 契约 v%d | 引擎 %d 个 | 数据目录 %s\n",
		prof.App.Name, svc.MagicAgentPath(), c.ContractVersion, len(c.Engines), prof.DataDir())
	fmt.Fprintf(os.Stderr, "magic-client: 打开 %s\n", url)
	fmt.Fprintf(os.Stderr, "magic-client: 接口 /desk/{health,contract,engines,modules,sessions,profile,asks,ask,ask/{id}/control} + 业务转发 /api/*（都要 X-Magic-Token）\n")
	if *lan {
		// 绑到局域网就把可用地址全打出来：一台机器常有多个网卡，猜错比多打两行麻烦。
		fmt.Fprintln(os.Stderr, "magic-client: ⚠️ 已绑到局域网（0.0.0.0）：同网络内的设备都能访问，令牌是唯一门锁")
		addrs := client.LanAddresses(port)
		if len(addrs) == 0 {
			fmt.Fprintln(os.Stderr, "magic-client: 没找到局域网地址（只连着回环？用 ipconfig getifaddr en0 查一下本机 IP）")
		}
		for _, a := range addrs {
			fmt.Fprintf(os.Stderr, "magic-client: 手机 / 平板打开  http://%s/?token=%s\n", a, tok)
			// 配对二维码页：要在**电脑上**打开这个链接（手机扫屏幕上的码）。
			// 用某个网卡地址打开，码里就是那个地址 —— 所以每个地址各给一条。
			fmt.Fprintf(os.Stderr, "magic-client:   配对二维码（在电脑上开，手机扫屏幕）  http://%s/pair?token=%s\n", a, tok)
		}
	}

	srv := &http.Server{
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		<-stop
		fmt.Fprintln(os.Stderr, "\nmagic-client: 收到退出信号，关闭中")
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "magic-client: 服务退出:", err)
		os.Exit(1)
	}
}

func randToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
