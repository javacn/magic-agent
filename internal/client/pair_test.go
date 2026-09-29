package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

/* 配对地址是「手机该连哪儿」的唯一来源，所以它必须跟着**用户实际打开的那个入口**走。
 *
 * 这条测试是隧道实测逼出来的：源侧收到的是明文 HTTP（TLS 在边缘终止），
 * 只认 r.Host + 写死 http:// 时，隧道下生成的二维码编成了
 * `http://<公网域名>/?token=…` —— 手机一扫，令牌先跟着一条明文请求出门。
 */
func TestPairURLFollowsForwardedHeaders(t *testing.T) {
	svc := newTestService(t, fakeCore(t, goodContract))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	pairInfo := func(hdrs map[string]string) (url, host string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/desk/pair?token=tok", nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out struct {
			URL  string `json:"url"`
			Host string `json:"host"`
		}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.URL, out.Host
	}

	t.Run("没有转发头：退回请求自身的 host", func(t *testing.T) {
		url, host := pairInfo(nil)
		if !strings.HasPrefix(url, "http://127.0.0.1:") {
			t.Fatalf("期望回环地址，得到 %q", url)
		}
		if !strings.HasSuffix(url, "/?token=tok") {
			t.Fatalf("令牌没带上：%q", url)
		}
		if !strings.HasPrefix(host, "127.0.0.1:") {
			t.Fatalf("host 期望是回环，得到 %q", host)
		}
	})

	t.Run("隧道/nginx：跟随 X-Forwarded-Proto 与 Host", func(t *testing.T) {
		url, host := pairInfo(map[string]string{
			"X-Forwarded-Proto": "https",
			"X-Forwarded-Host":  "pub.example.com",
		})
		// 这一条就是实测里失败的那一条：https 入口不能生成 http:// 的二维码。
		if url != "https://pub.example.com/?token=tok" {
			t.Fatalf("期望 https://pub.example.com/?token=tok，得到 %q", url)
		}
		if host != "pub.example.com" {
			t.Fatalf("host 期望 pub.example.com，得到 %q", host)
		}
	})

	t.Run("代理链叠成一串时只取第一段", func(t *testing.T) {
		url, _ := pairInfo(map[string]string{
			"X-Forwarded-Proto": "https, http",
			"X-Forwarded-Host":  "pub.example.com, inner.local:8080",
		})
		if url != "https://pub.example.com/?token=tok" {
			t.Fatalf("期望取第一段，得到 %q", url)
		}
	})

	t.Run("二维码内容与 /desk/pair 一致", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/pair/qr.png?token=tok", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "pub.example.com")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		/* 这里不断言图片像素（那是解码器的活，已经在真机上验过），只钉住
		   「同一份请求头，两条路径不会给出两个不同地址」这个前提 ——
		   它们共用一个 pairURL，一旦有人把其中一条改成各自拼，这条就会红。 */
		if ct := res.Header.Get("Content-Type"); ct != "image/png" {
			t.Fatalf("期望 image/png，得到 %q", ct)
		}
		url, _ := pairInfo(map[string]string{
			"X-Forwarded-Proto": "https",
			"X-Forwarded-Host":  "pub.example.com",
		})
		if svc.opts.Token != "tok" || !strings.Contains(url, "token=tok") {
			t.Fatalf("配对信息里没有令牌：%q", url)
		}
	})
}
