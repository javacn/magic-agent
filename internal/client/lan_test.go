package client

import (
	"strings"
	"testing"
)

// TestValidateLanToken 绑到局域网时令牌就是唯一门锁，短的一律拒绝。
func TestValidateLanToken(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  bool // true = 通过
	}{
		{"空令牌", "", false},
		{"纯空格", "    ", false},
		{"短令牌", "abc123", false},
		{"差一字符", strings.Repeat("a", MinLanTokenLen-1), false},
		{"刚够", strings.Repeat("a", MinLanTokenLen), true},
		{"生成的 32 位十六进制", "0123456789abcdef0123456789abcdef", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateLanToken(c.token)
			if c.want && err != nil {
				t.Errorf("应通过，却报错：%v", err)
			}
			if !c.want && err == nil {
				t.Error("应拒绝，却通过了")
			}
		})
	}
}

// TestLanAddresses 返回的地址里不能有回环或链路本地 —— 那两类给手机都没用。
func TestLanAddresses(t *testing.T) {
	got := LanAddresses(50022)
	for _, a := range got {
		if strings.HasPrefix(a, "127.") || strings.HasPrefix(a, "169.254.") {
			t.Errorf("不该出现回环/链路本地地址：%s", a)
		}
		if !strings.HasSuffix(a, ":50022") {
			t.Errorf("应带上端口：%s", a)
		}
		if i := strings.Index(a, ":"); i > 0 {
			if strings.Contains(a[:i], ":") {
				t.Errorf("IPv6 不该出现在这个列表里：%s", a)
			}
		}
	}
	// 本机可能只连着回环（离线环境），所以不要求非空，只在有结果时做上面的形状断言。
	t.Logf("局域网地址：%v", got)
}
