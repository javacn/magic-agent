package client

import (
	"fmt"
	"net"
	"strings"
)

/* 局域网接入（P4 的第一级：让手机能连上）。
 *
 * 默认只绑 127.0.0.1 是刻意的：本地端口对同机所有进程可见，开了鉴权才敢往外露。
 * 手机要连就得绑到局域网，而这一开，**令牌就成了唯一的门锁** —— 所以这里的两条
 * 守卫是配套的：`--lan` 必须给足强度的令牌，短令牌直接拒绝启动。
 *
 * 三级接入里这一级是 L1（局域网）；L2 的跨网中继需要服务端，不在本文件范围。
 */

// MinLanTokenLen 绑到局域网时令牌的最小长度（字符）。
// 为什么是 24：局域网内能被扫到，短令牌（哪怕 6 位）在本地网络里是可以暴力猜的。
// 生成的默认令牌是 16 字节 = 32 个十六进制字符，远超这条线。
const MinLanTokenLen = 24

// ValidateLanToken 检查绑到局域网时的令牌强度。
func ValidateLanToken(token string) error {
	tok := strings.TrimSpace(token)
	if tok == "" {
		return fmt.Errorf("绑到局域网不允许空令牌：空令牌会让鉴权整体失效（等于没有门锁）")
	}
	if len(tok) < MinLanTokenLen {
		return fmt.Errorf("绑到局域网的令牌至少要 %d 个字符（当前 %d 个）：局域网内能被扫到，短令牌不安全",
			MinLanTokenLen, len(tok))
	}
	return nil
}

// LanAddresses 列出本机可用于局域网访问的 IPv4 地址（形如 `192.168.1.5:50022`）。
//
// 过滤掉回环、链路本地（169.254.x）与非 IPv4：这三类给手机都没用。
// 一台机器可能有多个（有线 + 无线 + 虚拟网卡），所以返回切片让调用方全都打印出来 ——
// 猜错网卡比多打两行地址麻烦得多。
func LanAddresses(port int) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, fmt.Sprintf("%s:%d", ip4.String(), port))
		}
	}
	return out
}
