package client

import (
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

/* 二维码配对：把「桌面地址 + 令牌」变成手机能扫的一串东西。
 *
 * ── 为什么由插件出这张码 ──
 * 插件是**唯一**知道真实监听地址与端口的地方（端口从 0 起、由系统分配，profile 里没有）。
 * 手机端只负责「扫到什么就连什么」，不承担任何拼装责任 —— 拼装一旦分散到两端，
 * 就会出现「地址看着对、其实少一段路径」这类最难查的问题。
 *
 * ── 地址从哪来：请求本身 ──
 * 配对地址用 `r.Host` 拼（即「你是怎么打开这个页面的」），端口自然就对。
 * 所以启动时会把 `/pair` 页面**按每个网卡地址各打印一条链接**：在桌面上点局域网那条，
 * 码里就是局域网地址；点回环那条，码里就是回环地址。不需要再往 Options 里塞端口。
 *
 * ── 安全口径：用令牌本身把门 ──
 * 这张码的内容就是令牌，所以 `/pair*` 走**普通鉴权**（要带 token）。
 * 拿到令牌的人本来就拥有令牌，看这个页面没有任何提权 —— 而没令牌的人看到的只是 401。
 *
 * ⚠️ 我最初写的是「只允许 remote addr 是回环」，那是错的：桌面浏览器点局域网链接打开
 * `/pair` 时，源地址就是本机的局域网 IP（macOS 连自己也是走网卡地址），会被判 403，
 * 而那条链接正是我打印给用户点的。**守卫拦住的必须是攻击者，不能是正确用法**。
 */

// pairURL 拼出配对地址：用请求里的 host（含端口），带上令牌。
func (s *Service) pairURL(r *http.Request) string {
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	u := "http://" + host + "/"
	if s.opts.Token != "" {
		u += "?token=" + s.opts.Token
	}
	return u
}

// handlePairInfo 机器读：给壳 / 自动化用。
func (s *Service) handlePairInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"url":  s.pairURL(r),
		"host": r.Host,
		"hint": "手机端扫 /pair 页面上的二维码；地址与令牌都在这个 url 里。",
	})
}

// handlePairQR 二维码 PNG。
func (s *Service) handlePairQR(w http.ResponseWriter, r *http.Request) {
	png, err := qrcode.Encode(s.pairURL(r), qrcode.Medium, 512)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成二维码失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store") // 令牌可能在变，别缓存
	_, _ = w.Write(png)
}

// handlePairPage 人看的页面：桌面浏览器打开，手机扫屏幕。
func (s *Service) handlePairPage(w http.ResponseWriter, r *http.Request) {
	url := s.pairURL(r)

	/* 只显示「地址」和「令牌」两段，方便手抄兜底；整条 url 也一并给出便于复制。
	   页面本身不含任何脚本 —— 它就一张图加几行字，没有可被注入的面。 */
	addr, token := url, ""
	if i := strings.Index(url, "?token="); i >= 0 {
		addr, token = url[:i], url[i+len("?token="):]
	}

	page := `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>掌天瓶 · 配对</title>
<style>
 body{margin:0;background:#0A0A0B;color:#FAFAFA;padding:30px 24px 44px;
      font:15px/1.65 -apple-system,BlinkMacSystemFont,"PingFang SC","Noto Sans CJK SC",sans-serif}
 h1{font-size:20px;margin:0 0 6px} .sub{color:#8A8A8F;font-size:13px;margin:0 0 24px}
 .qr{background:#fff;padding:14px;border-radius:18px;width:max-content;margin:0 auto 22px}
 .qr img{display:block;width:272px;height:272px;image-rendering:pixelated}
 .row{background:#141416;border:1px solid #232327;border-radius:14px;padding:14px 16px;margin-bottom:10px}
 .k{color:#8A8A8F;font-size:12px;margin-bottom:4px}
 .v{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:14px;word-break:break-all}
 ol{color:#8A8A8F;font-size:13px;padding-left:20px;margin:22px 0 0}
 li{margin:5px 0}
</style></head><body>
 <h1>掌天瓶 · 配对</h1>
 <p class="sub">用手机上的掌天瓶 App 扫下面这张码。手机要和这台电脑在同一个网络里。</p>
 <div class="qr"><img src="/pair/qr.png?token=__TOKENRAW__" alt="配对二维码"></div>
 <div class="row"><div class="k">桌面地址</div><div class="v">__ADDR__</div></div>
 <div class="row"><div class="k">令牌</div><div class="v">__TOKEN__</div></div>
 <ol>
  <li><b>推荐</b>：把上面「桌面地址」那一整行复制到手机（微信、隔空投送都行），
      在 App 的配对页粘进地址栏 —— 它会自动拆出令牌，不用手抄。</li>
  <li>或者用手机<b>系统相机</b>扫上面这张码：会在浏览器里打开同一套界面（走网页版，不是 App）。</li>
  <li>App 内的相机扫码还没做。连不上时先确认桌面是以局域网方式起的，且两边在同一个 Wi-Fi。</li>
 </ol>
</body></html>`

	page = strings.ReplaceAll(page, "__ADDR__", htmlEscape(addr))
	page = strings.ReplaceAll(page, "__TOKEN__", htmlEscape(token))
	/* ⚠️ 二维码图必须**自带令牌**：`<img>` 是浏览器自己发起的子资源请求，带不上
	   `X-Magic-Token` 头，而 /pair/qr.png 走的是普通鉴权 —— 第一版我写成 src="/pair/qr.png"，
	   结果页面上永远只有一张 401 的碎图，也就是「没看到二维码」。
	   同一类坑在模块那边也踩过（产物缩略图），凡是子资源都要把令牌放进 URL。 */
	page = strings.ReplaceAll(page, "__TOKENRAW__", neturl.QueryEscape(token))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprint(w, page)
}

// htmlEscape 最小转义：这个页面里只插两段我们自己生成的值，但仍然不信任地转义 ——
// 一旦哪天有人把 Host 头指过来，未转义就是一个反射点。
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
