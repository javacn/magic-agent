package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

/* 跨源这一层是移动端实测逼出来的：容器页面的源是 https://localhost，插件在局域网 IP，
 * 天生跨源。没有 CORS 头时浏览器直接掐掉响应 —— 现象是 APK 装得上、闸门页打得开，
 * 但探活永远停在「连接中…」。**curl 不主动带 Origin，所以接口级验证永远看不见这条**。
 */
func TestCORSForLocalOrigins(t *testing.T) {
	svc := newTestService(t, fakeCore(t, goodContract))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	do := func(method, path, origin string, withToken bool, preflight bool) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if preflight {
			req.Header.Set("Access-Control-Request-Method", "GET")
			req.Header.Set("Access-Control-Request-Headers", "x-magic-token")
		}
		if withToken {
			req.Header.Set("X-Magic-Token", "tok")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	t.Run("预检不带令牌也必须放行（否则浏览器把真请求判死）", func(t *testing.T) {
		res := do(http.MethodOptions, "/desk/health", "https://localhost", false, true)
		defer res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("预检期望 204，得到 %d", res.StatusCode)
		}
		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "https://localhost" {
			t.Fatalf("期望回显 Origin，得到 %q", got)
		}
		if h := res.Header.Get("Access-Control-Allow-Headers"); !containsFold(h, "x-magic-token") {
			t.Fatalf("允许头里必须有点名 X-Magic-Token，得到 %q", h)
		}
	})

	t.Run("容器源的正常请求要带 ACAO", func(t *testing.T) {
		res := do(http.MethodGet, "/desk/health", "https://localhost", true, false)
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("期望 200，得到 %d", res.StatusCode)
		}
		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "https://localhost" {
			t.Fatalf("期望 ACAO=https://localhost，得到 %q", got)
		}
		if v := res.Header.Values("Vary"); !containsFold(join(v), "origin") {
			t.Fatalf("必须 Vary: Origin，否则缓存会把一个源的响应喂给另一个源：%v", v)
		}
	})

	t.Run("本机别的源（桌面壳的 loopback 端口）也放行", func(t *testing.T) {
		res := do(http.MethodGet, "/desk/health", "http://127.0.0.1:5173", true, false)
		defer res.Body.Close()
		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:5173" {
			t.Fatalf("期望回显 loopback 源，得到 %q", got)
		}
	})

	t.Run("iOS 容器的自定义 scheme 放行", func(t *testing.T) {
		res := do(http.MethodGet, "/desk/health", "capacitor://localhost", true, false)
		defer res.Body.Close()
		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "capacitor://localhost" {
			t.Fatalf("期望回显 capacitor://，得到 %q", got)
		}
	})

	t.Run("外站不给 CORS 头（后面是执行面，不能给任意网页开门）", func(t *testing.T) {
		res := do(http.MethodGet, "/desk/health", "https://evil.example", true, false)
		defer res.Body.Close()
		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("外站不该拿到 ACAO，得到 %q", got)
		}
		// 令牌仍然是唯一门锁：没令牌时即便源合法也是 401
		res2 := do(http.MethodGet, "/desk/health", "https://localhost", false, false)
		defer res2.Body.Close()
		if res2.StatusCode != http.StatusUnauthorized {
			t.Fatalf("没令牌期望 401，得到 %d", res2.StatusCode)
		}
	})
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (indexFold(s, sub) >= 0)
}

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func join(v []string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
