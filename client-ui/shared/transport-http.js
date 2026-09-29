/* 传输层的一个**现成实现**：HTTP（打 magic-client 插件的 /desk/* 接口）。
 *
 *  ⚠️ 这是给「手上已经有一个 HTTP 端点」的对接方用的**可选件**，不是组件的一部分。
 *    组件本身与传输无关（见 conversation.js 头部的 transport 契约）。
 *    按定稿口径 magic-agent 自己不跑服务 —— 掌天瓶那种宿主应当自己实现一个走
 *    IPC → CLI 的 transport（引擎清单/MAGIC 的 ask 与 control/会话历史都由 CLI 承担），
 *    而不是让组件去连某个服务。
 *
 *  ── 这一份覆盖的接口（与 CLIENT-CONTRACT.md 第二章一致）──
 *    GET  /desk/engines?models=1                      引擎 + 模型 + 每引擎能力
 *    POST /desk/ask                                   SSE：open / 事件 / end
 *    POST /desk/ask/{id}/control                      打断 / 收工 / 回答案
 *    GET  /desk/session/{id}/messages                 全量历史
 *    GET  /desk/session/{id}/events?after=<seq>       增量续读
 *
 *  ── 用法 ──
 *    const transport = MagicTransportHttp({ base: '', token: '…' });   // 同源时 base 传空串
 *    MagicConversation.create({ el, transport, … });
 */
(function () {
  'use strict';

  function MagicTransportHttp(opts) {
    opts = opts || {};
    var BASE = String(opts.base || '').replace(/\/+$/, '');
    var TOKEN = opts.token || '';

    function hdr(extra) {
      var h = extra || {};
      if (TOKEN) h['X-Magic-Token'] = TOKEN;
      return h;
    }
    function api(path, o) {
      o = o || {};
      o.headers = hdr(o.headers);
      if (o.json !== undefined) {
        o.method = o.method || 'POST';
        o.headers['Content-Type'] = 'application/json';
        o.body = JSON.stringify(o.json);
      }
      return fetch(BASE + path, o).then(function (r) {
        if (!r.ok) throw new Error(r.status + ' ' + r.statusText);
        return r;
      });
    }

    return {
      /* 引擎清单：带模型（选择器要用），所以固定 ?models=1。 */
      listEngines: function () {
        return api('/desk/engines?models=1').then(function (r) { return r.json(); });
      },

      /* 一轮：把 SSE 拆成三个回调交给组件，组件不关心里面是 SSE 还是别的。 */
      ask: function (payload, h) {
        var aborted = false;
        var ctl = typeof AbortController !== 'undefined' ? new AbortController() : null;
        var ended = false;
        var finish = function (code) {
          if (ended) return;
          ended = true;
          if (h && h.onEnd) h.onEnd(code);
        };
        api('/desk/ask', { json: payload, signal: ctl ? ctl.signal : undefined })
          .then(function (resp) {
            var reader = resp.body.getReader(), dec = new TextDecoder(), buf = '';
            return (function pump() {
              return reader.read().then(function (chunk) {
                if (chunk.done) return finish(0);
                buf += dec.decode(chunk.value, { stream: true });
                var blocks = buf.split('\n\n');
                buf = blocks.pop();
                blocks.forEach(function (b) {
                  var name = '', data = '';
                  b.split('\n').forEach(function (ln) {
                    if (ln.indexOf('event: ') === 0) name = ln.slice(7).trim();
                    else if (ln.indexOf('data: ') === 0) data += ln.slice(6);
                  });
                  if (!data) return;
                  var ev; try { ev = JSON.parse(data); } catch (e) { return; }
                  if (name === 'open') {
                    if (h && h.onOpen) h.onOpen(ev && (ev.id || ev.ask || ev.run));
                    return;
                  }
                  if (name === 'end') { finish(ev && ev.code); return; }
                  if (h && h.onEvent) h.onEvent(ev);
                });
                if (ended) return;
                return pump();
              });
            })();
          })
          .catch(function (e) {
            /* 被中止不算错（用户点了打断），别把它报成发送失败。 */
            if (aborted) return finish(null);
            if (h && h.onError) h.onError(e);
            finish(null);
          });
        return { cancel: function () { aborted = true; if (ctl) ctl.abort(); } };
      },

      control: function (p) {
        var body = { op: p.op };
        if (p.text != null) body.text = p.text;
        return api('/desk/ask/' + encodeURIComponent(p.id) + '/control', { json: body })
          .then(function () { return true; });
      },

      readSession: function (id, o) {
        o = o || {};
        /* 有游标就走增量接口：长会话下差一个数量级。
           形状与全量接口不同（多 lastSeq / snapshotRequired），一并原样交给组件。 */
        if (o.after != null) {
          return api('/desk/session/' + encodeURIComponent(id) + '/events?after=' + o.after)
            .then(function (r) { return r.json(); });
        }
        return api('/desk/session/' + encodeURIComponent(id) + '/messages')
          .then(function (r) { return r.json(); });
      }
    };
  }

  window.MagicTransportHttp = MagicTransportHttp;
})();
