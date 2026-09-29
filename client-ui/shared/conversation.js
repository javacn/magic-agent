/* 对话 UI 组件的公共逻辑（PC 与移动端共用这一份，是**唯一来源**）。
 *
 *  里面的 markdown 渲染、工具卡、思考块都是逐行对着 agents-anywhere 的 Compose 源码写的，
 *  最早由脚本从 client-ui-mobile 原样抽取而来；抽取完成后两个宿主都收敛成了瘦壳，
 *  这份文件就是权威实现 —— 改对话行为改这里，不要再去改宿主。
 *
 *  ── 组件不连后端 ──
 *    magic-agent **自己不跑服务**。数据从哪来由宿主决定，组件只认一个 transport：
 *      listEngines()                        -> Promise<[{engine, ok, models, capabilities}]>
 *      ask(payload, {onOpen,onEvent,onEnd}) -> 无返回；onEnd **必须**被调一次
 *                                              payload: {engine, model?, permission?, session?, prompt}
 *                                              onOpen(runID) / onEvent(coreEvent) / onEnd(exitCode)
 *      control({id, op, text?})             -> Promise   （op: interrupt | stop | answer）
 *      readSession(id, {after?})            -> Promise<{events, lastSeq?, snapshotRequired?}>
 *    掌天瓶走 IPC → CLI；别的对接方可能走 HTTP —— 组件对此一无所知。
 *    现成的 HTTP 实现见同目录 transport-http.js。
 *
 *  ── 用法 ──
 *    const conv = MagicConversation.create({
 *      el:            容器元素（组件会把对话区 + 输入区建进去）
 *      transport:     必填，见上
 *      session:       可选。传了就从这段会话开始（会拉历史），不传 = 新会话
 *      engine, model, permission: 可选初始值（空 = 不传，由 core 用自己的默认）
 *      engines:       可选。给了就用这份清单，不给则调 transport.listEngines()
 *      titleEl:       可选。宿主显示会话标题的元素（组件会写「新会话 · <引擎>」这类文案）
 *      onEvent:       可选。每个 core 事件透传一份给宿主（只读）
 *      onBusy:        可选。一轮开始/结束时回调 (busy:boolean)
 *    });
 *    conv.send('…');  conv.interrupt();  conv.stop();  conv.answer('…');
 *    conv.setSession(sid) / conv.newSession() / conv.refreshEngines() / conv.destroy();
 *
 *  ── 边界 ──
 *    组件**只管对话区**：会话列表、工作区切换、引擎探测的展示、业务视图都由对接方
 *    自己通过它那侧的接口实现，再把选中的会话交给组件（setSession）。
 */
(function () {
  'use strict';

  var SVG_NS = 'http://www.w3.org/2000/svg';

  /* 组件自带的图标集：宿主**不必**自己准备这些 symbol。
     前 7 个与 client-ui-mobile 里的定义逐字相同（保持两端图标一致），
     i-cpu / i-set 是选择器要用的，移动端原先没有，按其同一套线宽补画。 */
  var SPRITE =
    '<symbol id="i-chev" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m6.5 9.5 5.5 5.5 5.5-5.5"/></symbol>' +
    '<symbol id="i-copy" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2.2"/><path d="M15 6.2V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h.2"/></symbol>' +
    '<symbol id="i-share" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M21 3h-6M21 3v6M21 3l-8 8"/><path d="M20 14v5a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h5"/></symbol>' +
    '<symbol id="i-spark" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M9.9 4.3a.6.6 0 0 1 1.1 0l1.3 3.7a.6.6 0 0 0 .4.4l3.7 1.3a.6.6 0 0 1 0 1.1l-3.7 1.3a.6.6 0 0 0-.4.4l-1.3 3.7a.6.6 0 0 1-1.1 0l-1.3-3.7a.6.6 0 0 0-.4-.4L4.5 10.8a.6.6 0 0 1 0-1.1l3.7-1.3a.6.6 0 0 0 .4-.4z"/><path d="M18 5.5v4M20 7.5h-4M17 16v3M18.5 17.5h-3"/></symbol>' +
    '<symbol id="i-alert" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 8v5M12 16.5h.01"/></symbol>' +
    '<symbol id="i-clock" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></symbol>' +
    '<symbol id="i-up" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 19.5V5M5.8 11.2 12 5l6.2 6.2"/></symbol>' +
    '<symbol id="i-cpu" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"><rect x="7" y="7" width="10" height="10" rx="2"/><path d="M10 4v3M14 4v3M10 17v3M14 17v3M4 10h3M4 14h3M17 10h3M17 14h3"/></symbol>' +
    '<symbol id="i-set" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"><circle cx="12" cy="12" r="3"/><path d="M12 3.5v2.2M12 18.3v2.2M20.5 12h-2.2M5.7 12H3.5M18 6l-1.6 1.6M7.6 16.4 6 18M18 18l-1.6-1.6M7.6 7.6 6 6"/></symbol>';

  var spriteDone = false;
  function ensureSprite() {
    if (spriteDone) return;
    spriteDone = true;
    var s = document.createElementNS(SVG_NS, 'svg');
    s.setAttribute('width', '0'); s.setAttribute('height', '0');
    s.setAttribute('aria-hidden', 'true');
    s.style.position = 'absolute';
    s.innerHTML = SPRITE;
    document.body.appendChild(s);
  }

  function create(opts) {
    opts = opts || {};
    var root = opts.el;
    if (!root) throw new Error('MagicConversation.create: 缺 el（组件要挂的容器）');
    /* 组件**不自己连后端**：magic-agent 不跑服务，数据从哪来由宿主决定
       （掌天瓶走 IPC/CLI，别的对接方可能走 HTTP）。所以 transport 是必填的，
       缺了就当场报错，而不是回退到某个默认地址 —— 那种回退会把
       「宿主没接」伪装成「连不上」，最难查。 */
    var transport = opts.transport;
    if (!transport || typeof transport.ask !== 'function') {
      throw new Error('MagicConversation.create: 缺 transport（需实现 listEngines / ask / control / readSession）');
    }
    ensureSprite();

    /* ── 组件自有的 DOM 引用 ──
       保留段落里的代码是按 $('thread') / $('input') 这类 id 写的，下面这张映射表
       把它们指到组件自己的元素上 —— 于是那段代码**一字不用改**就能复用。 */
    var ctx = { thread: null, inner: null, input: null, sendBtn: null, permBtn: null, bottomBtn: null,
                titleEl: opts.titleEl || document.createElement('span') };
    function $(id) {
      if (id === 'thread') return ctx.thread;
      if (id === 'input') return ctx.input;
      if (id === 'btnSend') return ctx.sendBtn;
      if (id === 'btnPerm') return ctx.permBtn;
      if (id === 'btnBottom') return ctx.bottomBtn;
      if (id === 'chatTitle') return ctx.titleEl;
      return document.getElementById(id);
    }
    function el(tag, cls, text) {
      var n = document.createElement(tag);
      if (cls) n.className = cls;
      if (text != null) n.textContent = text;
      return n;
    }
    function svgIcon(id, size) {
      var s = document.createElementNS(SVG_NS, 'svg');
      s.setAttribute('width', size || 16); s.setAttribute('height', size || 16);
      var u = document.createElementNS(SVG_NS, 'use');
      u.setAttribute('href', '#' + id);
      s.appendChild(u);
      return s;
    }
    function box(cls, iconId, size) {
      var d = el('span', cls);
      d.appendChild(svgIcon(iconId, size));
      return d;
    }

    var state = {
      engines: opts.engines || [], engine: opts.engine || '', model: opts.model || '',
      permission: opts.permission || '', session: opts.session || '', busy: false, askId: null,
      renderedSeq: {}, cur: null, agentRaw: '', rsn: null, rsnRaw: '', lastTool: null,
      working: null, autoFollow: true, resultSeen: false
    };

  /* ══════════ markdown 渲染 ══════════
     对齐 AgentMarkdownText.kt：commonmark + autolink + strikethrough + tables + task list。
     不引第三方库 —— 只覆盖 agent 回复里实际会出现的子集，够用且零依赖。 */
  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }
  // :937-942 parseFileRef —— 形如 src/app.ts:12 的路径引用
  function fileRef(t) {
    if (!t || /\s/.test(t) || t.indexOf('://') >= 0) return null;
    if (t.indexOf('/') < 0) return null;
    if (!/\.[a-zA-Z0-9]+(?::\d+(?::\d+)?)?$/.test(t)) return null;
    return t.replace(/:\d+(?::\d+)?$/, '');
  }
  var COPY_TOKEN = '\u0000';
  var HARD_BREAK = '\u0001';
  var copyBin = [];
  function copyAttr(text) {
    copyBin.push(String(text == null ? '' : text));
    return ' data-copy-idx="' + (copyBin.length - 1) + '"';
  }
  var COPY_BTN_HTML = '<button type="button" class="copybtn copybtn--sm" aria-label="复制"' + '\u0002' +
    '><svg width="15" height="15"><use href="#i-copy"/></svg></button>';

  function inlineMd(src) {
    var tokens = [];
    var s = String(src == null ? '' : src);
    // 硬换行（行尾两空格 或 反斜杠）
    s = s.replace(/\\\n/g, HARD_BREAK).replace(/ {2,}\n/g, HARD_BREAK);
    // 行内码先摘出来，避免被转义与强调规则动到
    s = s.replace(/`([^`]+)`/g, function (_, code) {
      var isFile = fileRef(code);
      tokens.push('<code class="md-code' + (isFile ? ' md-code--file' : '') +
        (code.length > 30 || /\s/.test(code) ? ' md-code--wrap' : '') + '">' + esc(code) + '</code>');
      return COPY_TOKEN + (tokens.length - 1) + COPY_TOKEN;
    });
    // markdown 链接也先摘出来
    s = s.replace(/\[([^\]]*)\]\(([^)\s]+)\)/g, function (_, text, href) {
      tokens.push('<a href="' + esc(href) + '" target="_blank" rel="noreferrer noopener">' + esc(text) + '</a>');
      return COPY_TOKEN + (tokens.length - 1) + COPY_TOKEN;
    });
    // 图片 → 只保留 alt 文字（:428）
    s = s.replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1');
    s = esc(s);
    // 尖括号自动链接
    s = s.replace(/&lt;((?:https?:\/\/|mailto:)[^\s&]+)&gt;/g, function (_, url) {
      return '<a href="' + url + '" target="_blank" rel="noreferrer noopener">' + url + '</a>';
    });
    // 裸 URL
    s = s.replace(/(^|[\s(（])((?:https?:\/\/)[^\s<>"'）)]+)/g, function (_, pre, url) {
      var tail = '';
      while (/[.,;:!?]$/.test(url)) { tail = url.slice(-1) + tail; url = url.slice(0, -1); }
      if (!url) return pre;
      return pre + '<a href="' + url + '" target="_blank" rel="noreferrer noopener">' + url + '</a>' + tail;
    });
    s = s.replace(/\*\*([^\s*](?:[\s\S]*?[^\s*])?)\*\*/g, '<strong>$1</strong>');
    s = s.replace(/__([^\s_](?:[\s\S]*?[^\s_])?)__/g, '<strong>$1</strong>');
    s = s.replace(/~~([^\s~](?:[\s\S]*?[^\s~])?)~~/g, '<del>$1</del>');
    s = s.replace(/(^|[^*\w])\*([^\s*](?:[^*]*?[^\s*])?)\*/g, '$1<em>$2</em>');
    s = s.replace(/(^|[^_\w])_([^\s_](?:[^_]*?[^\s_])?)_/g, '$1<em>$2</em>');
    s = s.split(HARD_BREAK).join('<br>').split('\n').join(' ');
    s = s.replace(new RegExp(COPY_TOKEN + '(\\d+)' + COPY_TOKEN, 'g'), function (_, i) { return tokens[+i]; });
    return s;
  }

  function splitCells(line) {
    return line.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map(function (c) { return c.trim(); });
  }
  function isTableDelim(line) {
    var c = splitCells(line);
    return c.length >= 2 && c.every(function (x) { return /^:?-{1,}:?$/.test(x); });
  }
  function isTableHeader(line) {
    if (isTableDelim(line)) return false;
    var c = splitCells(line);
    return c.length >= 2 && c.some(function (x) { return x !== ''; });
  }
  function readTable(lines, i) {
    var header = splitCells(lines[i]);
    var aligns = splitCells(lines[i + 1]).map(function (x) {
      if (/^:-+:$/.test(x)) return 'center';
      if (/^-+:$/.test(x)) return 'right';
      return 'left';
    });
    i += 2;
    var body = [];
    while (i < lines.length && lines[i].trim() && lines[i].indexOf('|') >= 0) { body.push(splitCells(lines[i])); i++; }
    var h = '<div class="md-table-wrap"><table><thead><tr>';
    header.forEach(function (c, k) {
      h += '<th style="text-align:' + (aligns[k] || 'left') + '">' + inlineMd(c) + '</th>';
    });
    h += '</tr></thead><tbody>';
    body.forEach(function (r) {
      h += '<tr>';
      for (var k = 0; k < header.length; k++) {
        h += '<td style="text-align:' + (aligns[k] || 'left') + '">' + inlineMd(r[k] || '') + '</td>';
      }
      h += '</tr>';
    });
    return { html: h + '</tbody></table></div>', next: i };
  }
  function readList(lines, i) {
    var ordered = /^\s*\d+[.)]\s+/.test(lines[i]);
    var m = lines[i].match(/^\s*(\d+)[.)]\s+/);
    var start = m ? parseInt(m[1], 10) : 1;
    var items = [], idx = 0;
    while (i < lines.length) {
      var mm = lines[i].match(/^\s*(?:[-*+]|\d+[.)])\s+(.*)$/);
      if (!mm) break;
      var body = mm[1], task = null;
      var tm = body.match(/^\[([ xX])\]\s+(.*)$/);
      if (tm) { task = tm[1].toLowerCase() === 'x'; body = tm[2]; }
      // 续行：缩进的普通行并到上一条
      i++;
      while (i < lines.length && lines[i].trim() && /^\s{2,}/.test(lines[i]) &&
             !/^\s*(?:[-*+]|\d+[.)])\s+/.test(lines[i])) {
        body += ' ' + lines[i].trim(); i++;
      }
      items.push({ task: task, text: body, num: ordered ? start + idx : null });
      idx++;
    }
    var tag = ordered ? 'ol' : 'ul';
    var h = '<' + tag + '>';
    items.forEach(function (it) {
      // :222-227 anywhere 的 bullet 前缀就是字面量 "-"（不是圆点）；任务列表走 mono 的 [ ] / [x]
      var mk = it.task === null ? (it.num != null ? it.num + '.' : '-') : (it.task ? '[x]' : '[ ]');
      h += '<li><span class="mk' + (it.task !== null ? ' mk--mono' : '') + '">' + esc(mk) + '</span>' +
           '<span class="li-b">' + inlineMd(it.text) + '</span></li>';
    });
    return { html: h + '</' + tag + '>', next: i };
  }
  var BASH_LABELS = { bash: 1, sh: 1, shell: 1, zsh: 1, console: 1, terminal: 1 };
  function codePanelHtml(label, code) {
    var isBash = !!BASH_LABELS[String(label || '').toLowerCase()];
    var body;
    if (isBash) {
      // MarkdownCodePanel(:515-544) 走 BashCommandCard：按行拆，首个 token 上 prompt 色
      var cmds = String(code).split('\n').map(function (x) { return x.replace(/\s+$/, ''); })
        .filter(function (x) { return x.trim() !== ''; });
      if (!cmds.length) cmds = [code];
      body = cmds.map(function (c) {
        var t = c.trim(), sp = t.indexOf(' ');
        var first = sp < 0 ? t : t.slice(0, sp);
        var rest = sp < 0 ? '' : t.slice(sp + 1).replace(/^\s+/, '');
        return '<div class="cmdline"><span class="cmdline__p">' + esc(first) + '</span>' +
               (rest ? '<span class="cmdline__r">' + esc(rest) + '</span>' : '') + '</div>';
      }).join('');
    } else {
      body = '<pre class="codepanel__pre">' + esc(code) + '</pre>';
    }
    return '<div class="codepanel"><div class="codepanel__head">' +
      '<span class="codepanel__label">' + esc(isBash ? 'Bash' : (label || 'code')) + '</span>' +
      COPY_BTN_HTML.split('\u0002').join(copyAttr(code)) +
      '</div><div class="codepanel__code">' + body + '</div></div>';
  }
  function mdToHtml(src) {
    var lines = String(src == null ? '' : src).replace(/\r\n?/g, '\n').split('\n');
    // :883-911 normalizeMarkdownTables：表格前一行不是空行时补一个空行
    var norm = [], inFence = false, k;
    for (k = 0; k < lines.length; k++) {
      if (!inFence && k + 1 < lines.length && isTableHeader(lines[k]) && isTableDelim(lines[k + 1]) &&
          norm.length && norm[norm.length - 1].trim() !== '') norm.push('');
      norm.push(lines[k]);
      if (/^\s*(```|~~~)/.test(lines[k])) inFence = !inFence;
    }
    var n = norm.length, i = 0, out = [];
    while (i < n) {
      var line = norm[i];
      if (!line.trim()) { i++; continue; }
      var fence = line.match(/^\s*(```|~~~)\s*(.*)$/);
      if (fence) {
        var mark = fence[1];
        var label = (fence[2] || '').trim().split(/\s+/)[0] || 'code';
        var buf = []; i++;
        while (i < n && !new RegExp('^\\s*' + mark).test(norm[i])) { buf.push(norm[i]); i++; }
        i++;
        out.push(codePanelHtml(label, buf.join('\n')));
        continue;
      }
      var h = line.match(/^(#{1,6})\s+(.*)$/);
      if (h) { out.push('<h' + h[1].length + '>' + inlineMd(h[2].trim()) + '</h' + h[1].length + '>'); i++; continue; }
      if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) { out.push('<hr>'); i++; continue; }
      if (/^\s*>/.test(line)) {
        var qb = [];
        while (i < n && /^\s*>/.test(norm[i])) { qb.push(norm[i].replace(/^\s*>\s?/, '')); i++; }
        out.push('<blockquote><span class="bar"></span><div class="q-b">' + mdToHtml(qb.join('\n')) + '</div></blockquote>');
        continue;
      }
      if (/^\s*(?:[-*+]|\d+[.)])\s+/.test(line)) {
        var rl = readList(norm, i); out.push(rl.html); i = rl.next; continue;
      }
      if (isTableHeader(line) && i + 1 < n && isTableDelim(norm[i + 1])) {
        var rt = readTable(norm, i); out.push(rt.html); i = rt.next; continue;
      }
      var pb = [];
      while (i < n && norm[i].trim() &&
             !/^\s*(```|~~~)/.test(norm[i]) && !/^#{1,6}\s/.test(norm[i]) &&
             !/^\s*>/.test(norm[i]) && !/^\s*(?:[-*+]|\d+[.)])\s+/.test(norm[i]) &&
             !/^\s*([-*_])(\s*\1){2,}\s*$/.test(norm[i]) &&
             !(isTableHeader(norm[i]) && i + 1 < n && isTableDelim(norm[i + 1]))) {
        pb.push(norm[i]); i++;
      }
      if (pb.length) out.push('<p>' + inlineMd(pb.join('\n')) + '</p>');
      else i++;
    }
    return out.join('');
  }

  /* ══════════ 对话区组件 ══════════ */
  function thread() { return $('thread'); }
  function scrollBottom() { var t = thread(); t.scrollTop = t.scrollHeight; }
  function nearBottom() { var t = thread(); return t.scrollHeight - t.scrollTop - t.clientHeight < 48; }
  function pushItem(node, force) {
    thread().appendChild(node);
    if (force || state.autoFollow) scrollBottom();
  }
  function tgroup() { return el('div', 'tgroup'); }
  function doCopy(text) {
    var s = String(text == null ? '' : text);
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(s);
      else {
        var ta = el('textarea');
        ta.value = s; ta.style.cssText = 'position:fixed;left:-9999px';
        document.body.appendChild(ta); ta.select();
        try { document.execCommand('copy'); } catch (e) {}
        document.body.removeChild(ta);
      }
    } catch (e) {}
  }
  function copyBtn(text) {
    var b = el('button', 'copybtn');
    b.type = 'button';
    b.setAttribute('aria-label', '复制');
    b.appendChild(svgIcon('i-copy', 17));
    b.onclick = function () { doCopy(text); };
    return b;
  }

  /* UserBubble(:1286-1394)  22dp 圆角 · 17/13dp 内边距 · 16.5sp/24sp · 最多 8 行 */
  function userBubble(text, failed) {
    var wrap = tgroup();
    var row = el('div', 'urow');
    row.appendChild(copyBtn(text));                 // :1332 左侧 30dp 圆复制按钮
    var col = el('div', 'urow__col');
    var bub = el('div', 'ubub');
    var t = el('div', 'ubub__t', text);
    bub.appendChild(t);
    col.appendChild(bub);
    if (failed) col.appendChild(el('div', 'ubub__meta', '发送失败'));
    row.appendChild(col);
    wrap.appendChild(row);
    pushItem(wrap, true);
    // :1352-1356 maxLines = 8，溢出时给「展开 / 收起」
    var w = t.clientWidth;
    if (w > 0) {
      var probe = el('div', 'ubub__t');
      probe.style.cssText = 'position:absolute;left:-99999px;top:0;visibility:hidden;' +
                            'display:block;max-height:none;overflow:visible;width:' + w + 'px';
      probe.textContent = text;
      document.body.appendChild(probe);
      var full = probe.scrollHeight;
      document.body.removeChild(probe);
      if (full > 8 * 24 + 2) {
        var btn = el('button', 'ubub__more', '展开');
        btn.type = 'button';
        btn.onclick = function () { btn.textContent = bub.classList.toggle('is-open') ? '收起' : '展开'; };
        bub.appendChild(btn);
      }
    }
  }

  /* AgentMessageContent(:1238-1260) → AgentMarkdownText */
  function ensureAgent() {
    if (state.cur) return state.cur;
    var wrap = tgroup();
    var md = el('div', 'md');
    wrap.appendChild(md);
    pushItem(wrap);
    state.cur = { wrap: wrap, md: md };
    state.agentRaw = '';
    return state.cur;
  }
  var mdTimer = 0;
  function flushMd() {
    mdTimer = 0;
    if (!state.cur) return;
    state.cur.md.innerHTML = mdToHtml(state.agentRaw);
    if (state.autoFollow) scrollBottom();
  }
  function scheduleMd() {
    if (mdTimer) return;
    mdTimer = setTimeout(flushMd, 40);   // 流式期间合并重绘，避免每个 chunk 都跑一遍 markdown
  }
  function finalizeAgent(withActions) {
    if (!state.cur) return;
    if (mdTimer) { clearTimeout(mdTimer); mdTimer = 0; }
    flushMd();
    if (withActions && state.agentRaw.trim()) state.cur.wrap.appendChild(replyActions(state.agentRaw));
    state.cur = null; state.agentRaw = '';
  }

  /* AgentReplyActions(:554-587)  分割线 + 复制 / 分享（各 30dp 圆） */
  function replyActions(text) {
    var d = el('div', 'acts');
    d.appendChild(el('div', 'acts__line'));
    var row = el('div', 'acts__row');
    row.appendChild(copyBtn(text));
    var share = el('button', 'copybtn');
    share.type = 'button';
    share.setAttribute('aria-label', '分享');
    share.appendChild(svgIcon('i-share', 17));
    share.onclick = function () {
      var s = String(text || '');
      if (navigator.share) { try { navigator.share({ text: s }); return; } catch (e) {} }
      doCopy(s);
    };
    row.appendChild(share);
    d.appendChild(row);
    return d;
  }

  /* ReasoningSection(:1530-1608)  Sparkles + 13sp mono 标题 + 可折叠 */
  function ensureRsn() {
    if (state.rsn) return state.rsn;
    var d = el('div', 'rsn');
    var head = el('button', 'rsn__head');
    head.type = 'button';
    head.setAttribute('aria-expanded', 'false');
    var chev = box('chev', 'i-chev', 16);
    head.appendChild(chev);
    head.appendChild(box('ico', 'i-spark', 16));
    var title = el('span', 'rsn__title', '思考');
    head.appendChild(title);
    var body = el('div', 'rsn__body');
    var md = el('div', 'md');
    body.appendChild(md);
    body.style.display = 'none';
    head.onclick = function () {
      // :1572 只有可展开时才挂点击；单段短摘要那种（plain）是纯展示，点不动
      if (head.getAttribute('data-plain') === '1') return;
      var open = head.getAttribute('aria-expanded') !== 'true';
      head.setAttribute('aria-expanded', String(open));
      body.style.display = open ? '' : 'none';
    };
    d.appendChild(head); d.appendChild(body);
    pushItem(d);
    state.rsn = { el: d, head: head, chev: chev, title: title, body: body, md: md, plain: false };
    state.rsnRaw = '';
    return state.rsn;
  }
  function plainSummary(s) {
    return String(s || '')
      .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
      .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
      .replace(/`([^`]+)`/g, '$1')
      .replace(/[*_~#>]+/g, '')
      .replace(/\s+/g, ' ')
      .trim();
  }
  function updateRsnTitle() {
    var m = state.rsn;
    if (!m) return;
    var segs = String(state.rsnRaw).split(/\n{2,}/).filter(function (x) { return x.trim(); });
    var one = segs.length === 1 ? plainSummary(segs[0]) : null;
    if (one && one.length <= 80) {
      // :1539-1545 单段且能压成一行 → 直接内联成摘要，不给折叠
      m.title.textContent = '思考：' + one;
      m.plain = true;
      m.chev.style.display = 'none';
      m.head.setAttribute('data-plain', '1');
      m.head.setAttribute('aria-expanded', 'false');
      m.body.style.display = 'none';
    } else {
      m.title.textContent = segs.length ? '思考 · ' + segs.length + ' 段' : '思考';
      m.plain = false;
      m.chev.style.display = '';
      m.head.removeAttribute('data-plain');
    }
    // 不可展开时去掉「像能点」的反馈
    m.head.style.cursor = m.plain ? 'default' : 'pointer';
  }
  function finalizeRsn() {
    if (!state.rsn) return;
    updateRsnTitle();
    if (state.rsn.plain) state.rsn.body.style.display = 'none';
    state.rsn = null; state.rsnRaw = '';
  }

  /* WorkingIndicator(:1072-1120) */
  function setWorking(label) {
    clearWorking();
    var d = el('div', 'working');
    d.appendChild(el('div', 'working__sp'));
    d.appendChild(el('span', 'working__t', label));
    state.working = d;
    pushItem(d);
  }
  function clearWorking() {
    if (state.working) { state.working.remove(); state.working = null; }
  }

  /* ToolPlaceholder(:2199-2270)  Clock / CircleAlert + 13sp Bold + 状态胶囊 */
  function phItem(text, kind, status) {
    var wrap = tgroup();
    var d = el('div', 'ph' + (kind === 'err' ? ' ph--err' : ''));
    var row = el('div', 'ph__row');
    row.appendChild(box('ph__ico', kind === 'err' ? 'i-alert' : 'i-clock', 16));
    row.appendChild(el('span', 'ph__t', text));
    if (status) row.appendChild(el('span', 'pill' + (kind === 'err' ? ' pill--err' : ''), status));
    d.appendChild(row);
    wrap.appendChild(d);
    return wrap;
  }
  /* CompactTimelineSeparator(:2272-2315) */
  function sepItem(text) {
    var wrap = tgroup();
    var d = el('div', 'sep');
    d.appendChild(el('div', 'sep__line'));
    d.appendChild(el('span', 'sep__t', text));
    d.appendChild(el('div', 'sep__line'));
    wrap.appendChild(d);
    return wrap;
  }
  /* AuthErrorNotice(Components.kt:88-117) */
  function noticeItem(text) {
    var d = el('div', 'notice');
    d.appendChild(box('notice__ico', 'i-alert', 20));
    d.appendChild(el('div', 'notice__t', text));
    pushItem(d, true);
  }

  /* ToolActivityCard(:1625-1748)
     折叠行 8dp 圆角 + activitySurface + chevron + 16dp 图标 + 13sp mono 摘要
     展开体 14dp 圆角 + subtle + 1dp 描边；Command 走 CommandLineBar，其余走 输入/输出 段落 */
  function toolIconId(name) {
    var n = String(name || '').toLowerCase();
    if (/bash|shell|zsh|command|exec|run|terminal|\bcmd\b/.test(n)) return 'i-terminal';
    if (/write|edit|read|file|glob|grep|patch|diff|apply|notebook/.test(n)) return 'i-filepen';
    if (/agent|task|subagent|delegate/.test(n)) return 'i-bot';
    return 'i-hammer';
  }
  function isCmdTool(name) {
    return /bash|shell|zsh|command|exec|run|terminal|\bcmd\b/i.test(String(name || ''));
  }
  function toolCard(name, opts) {
    opts = opts || {};
    var cmd = isCmdTool(name);
    var d = el('div', 'tool' + (cmd ? ' tool--cmd' : '') + (opts.failed ? ' tool--err' : ''));
    var head = el('button', 'tool__head');
    head.type = 'button';
    head.setAttribute('aria-expanded', 'false');
    head.appendChild(box('chev', 'i-chev', 16));
    head.appendChild(box('tool__ico', toolIconId(name), 16));
    var sum = el('span', 'tool__sum', String(opts.summary || name || '工具'));
    head.appendChild(sum);
    if (opts.pill) head.appendChild(el('span', 'pill' + (opts.failed ? ' pill--err' : ''), opts.pill));
    d.appendChild(head);
    var detail = el('div', 'tool__detail');
    detail.style.display = 'none';
    d.appendChild(detail);
    head.onclick = function () {
      var open = head.getAttribute('aria-expanded') !== 'true';
      head.setAttribute('aria-expanded', String(open));
      detail.style.display = open ? '' : 'none';
      if (open) { try { if (navigator.vibrate) navigator.vibrate(12); } catch (e) {} }  // :1656 触感
    };
    pushItem(d);
    return { el: d, head: head, sum: sum, detail: detail, cmd: cmd };
  }
  /* CommandLineBar(:1969-2019)  14dp 圆角 · sessionCodeSurface · "$" + 13sp mono */
  function cmdBar(label, command) {
    var w = el('div', 'cmdbar');
    var h = el('div', 'cmdbar__head');
    h.appendChild(el('span', 'cmdbar__label', label || '命令'));
    h.appendChild(copyBtn(command));
    w.appendChild(h);
    var b = el('div', 'cmdbar__body');
    b.appendChild(el('span', 'cmdbar__p', '$'));
    b.appendChild(el('span', 'cmdbar__t', command));
    w.appendChild(b);
    return w;
  }
  /* CommandPreviewSection(:2021-2052)  11sp Bold 标签 + 12dp 圆角代码块（min 64dp） */
  function toolSection(label, text, isErr) {
    var s = el('div', 'sect');
    var h = el('div', 'sect__head');
    h.appendChild(el('span', 'sect__label', label));
    h.appendChild(copyBtn(text));
    s.appendChild(h);
    var c = el('div', 'sect__code');
    c.appendChild(el('pre', 'sect__pre' + (isErr ? ' sect__pre--err' : ''), String(text == null ? '' : text)));
    s.appendChild(c);
    return s;
  }
  function stopPrevTool() {
    if (state.lastTool) {
      state.lastTool.sum.classList.remove('shimmer');
      state.lastTool = null;
    }
  }
  function onToolUse(ev) {
    finalizeRsn();
    finalizeAgent(false);
    stopPrevTool();
    var name = ev.name || '工具';
    var input = String(ev.text == null ? '' : ev.text);
    var head = input.split('\n')[0].trim();
    var summary = head ? (name + ' · ' + head) : name;
    if (summary.length > 120) summary = summary.slice(0, 120) + '…';
    var card = toolCard(name, { summary: summary });
    if (input) {
      card.detail.appendChild(card.cmd ? cmdBar('命令', input) : toolSection('输入', input));
    }
    card.sum.classList.add('shimmer');   // :1704 运行中做扫光
    state.lastTool = card;
  }
  function onToolResult(ev) {
    var text = String(ev.text == null ? '' : ev.text);
    var card = state.lastTool;
    if (!card) card = toolCard('结果', { summary: '结果' });
    var err = /^\s*(error|错误|失败)/i.test(text) || /\bexit code [1-9]/.test(text);
    if (card.cmd && !card.detail.firstChild && !err) {
      // 命令卡还没展开过命令本体时，把结果当输出段
      card.detail.appendChild(cmdBar('命令', text));
    } else {
      card.detail.appendChild(toolSection('输出', text, err));
    }
    if (err) {
      card.el.classList.add('tool--err');
      card.head.appendChild(el('span', 'pill pill--err', 'failed'));
    }
    stopPrevTool();
  }

  /* ── 事件 → 界面（历史与实时共用同一条路径，靠 seq 去重） ── */
  function render(ev) {
    if (ev.seq != null && state.renderedSeq[ev.seq]) return;
    if (ev.seq != null) state.renderedSeq[ev.seq] = true;
    /* ⚠️ 两套字段名都要认：
       实时 SSE（magic-agent --stream --events）事件用 `type`；
       落盘的 <session_id>.jsonl 用 `kind`（internal/cli/sessionlog.go 的 sessionEvent.Kind）。
       只认 type 的后果是「从列表点进会话」历史整段空白 —— 静默失败，最难查。 */
    var t = ev.type || ev.kind;
    if (t === 'ready') {
      setWorking('Agent 运行中');
    } else if (t === 'thinking') {
      var m = ensureRsn();
      state.rsnRaw += ev.text || '';
      m.md.innerHTML = mdToHtml(state.rsnRaw);
      updateRsnTitle();
      if (state.autoFollow) scrollBottom();
    } else if (t === 'text') {
      finalizeRsn();
      ensureAgent();
      state.agentRaw += ev.text || '';
      scheduleMd();
    } else if (t === 'tool_use') {
      onToolUse(ev);
    } else if (t === 'tool_result') {
      onToolResult(ev);
    } else if (t === 'user') {
      // 历史里「用户自己那条」。core 目前只落引擎事件，这条是给落盘后预留的；
      // 认它不花成本，落盘一上线历史就自然有气泡。
      finalizeRsn(); finalizeAgent(true); stopPrevTool(); clearWorking();
      userBubble(String(ev.text || ''), false);
    } else if (t === 'turn_end') {
      if (ev.session_id) state.session = ev.session_id;
      clearWorking();
      finalizeRsn();
      // 兜底：个别引擎只在 turn_end 带整轮正文（正文增量一条都没到时用它）
      if (!state.cur && !state.agentRaw && ev.text) { ensureAgent(); state.agentRaw = ev.text; }
      finalizeAgent(true);
      stopPrevTool();
    } else if (t === 'result') {
      clearWorking();
      finalizeRsn();
      finalizeAgent(true);
      stopPrevTool();
      state.resultSeen = true;
      pushItem(sepItem('完成 · ' + (ev.latency_ms || 0) + 'ms'));
    } else if (t === 'error') {
      clearWorking();
      finalizeRsn();
      finalizeAgent(true);
      stopPrevTool();
      pushItem(phItem('本轮失败：' + (ev.reason || ev.error || ''), 'err', 'failed'), true);
    } else if (t === 'ask') {
      finalizeRsn(); finalizeAgent(true); stopPrevTool();
      /* 审批 / 提问卡。原先这里只弹一行 notice —— 而 ask 是 headless 下**唯一**能
         喂答案的通道（core 的 --control answer），只弹一行等于把这条路掐断了：
         引擎卡在等待批准，手机上却没有任何办法推进它。
         结构与桌面版 approval() 对齐（同一份 events 契约），样式按 AA 的状态胶囊走。 */
      approvalCard(ev);
    } else if (t === 'interrupted' || t === 'stopped') {
      clearWorking();
      finalizeRsn();
      finalizeAgent(true);
      stopPrevTool();
      pushItem(phItem(t === 'interrupted' ? '已中断' : '已停止', 'ok', null));
    }
  }

  function clearThread() {
    thread().innerHTML = '';
    state.cur = null; state.agentRaw = '';
    state.rsn = null; state.rsnRaw = '';
    state.lastTool = null; state.working = null;
    state.autoFollow = true;
    copyBin.length = 0;
    if (mdTimer) { clearTimeout(mdTimer); mdTimer = 0; }
    nearBottom(); syncBottomBtn();
  }
  function syncBottomBtn() {
    $('btnBottom').classList.toggle('tobottom--on', !state.autoFollow && thread().scrollHeight > thread().clientHeight + 80);
  }


  /* 控制通道：打断 / 收工 / 回答案。op 的语义与 core 的 --control 同协议；
     具体怎么送到 core 由 transport.control 决定（HTTP / IPC / CLI 都可能）。
     ⚠️ 没有 askId 就如实说，不要静默失败 —— 一个按下去没反应的按钮比报错更难查。
     返回 Promise<boolean>，给审批卡判断「答没答上」用。 */
  async function control(op, text) {
    if (!state.askId) {
      noticeItem('这一轮还没拿到轮次 id，稍等 open 事件回来再试');
      return false;
    }
    try {
      var payload = { op: op, id: state.askId };
      if (text != null) payload.text = text;
      await transport.control(payload);
      if (op !== 'answer') noticeItem(op === 'interrupt' ? '已请求打断' : '已请求收工');
      return true;
    } catch (e) {
      noticeItem('控制失败：' + (e.message || e));
      return false;
    }
  }

  /* ── 审批 / 提问卡 ──
     与桌面版 approval() 同一套 events 契约（CLIENT-CONTRACT.md 的 ask 事件）：
     ev.ask.questions[] 每题带 options[]，multi_select 为真时是多选。
     答复走 control 的 op:'answer' —— 与 core 的 --control answer 同协议。 */
  function approvalCard(ev) {
    var ask = ev.ask || {};
    var card = el('div', 'askcard');
    var head = el('div', 'askcard__head');
    head.appendChild(el('span', 'askcard__dot'));
    head.appendChild(el('span', null, ask.kind === 'approval' ? '需要授权' : '需要确认'));
    card.appendChild(head);

    var qs = ask.questions || [];
    if (!qs.length) {
      card.appendChild(el('p', 'askcard__q', ev.text || '模型需要你的选择'));
      var fb = el('button', 'askcard__btn askcard__btn--primary', '用输入框内容作答');
      fb.onclick = function () { answerAsk(card, $('input').value.trim() || '是'); };
      card.appendChild(fb);
      pushItem(card, true);
      return;
    }

    qs.forEach(function (q) {
      card.appendChild(el('p', 'askcard__q', (q.header ? '【' + q.header + '】' : '') + (q.text || '')));
      var opts = q.options || [];
      var chosen = [];
      var row = el('div', 'askcard__opts');
      if (q.multi_select) {
        opts.forEach(function (o) {
          var b = el('button', 'askcard__btn', o.label || '');
          if (o.description) b.title = o.description;
          b.onclick = function () {
            var i = chosen.indexOf(o.label);
            if (i >= 0) { chosen.splice(i, 1); b.classList.remove('askcard__btn--primary'); }
            else { chosen.push(o.label); b.classList.add('askcard__btn--primary'); }
          };
          row.appendChild(b);
        });
        var sub = el('button', 'askcard__btn askcard__btn--primary', '提交');
        sub.onclick = function () { if (chosen.length) answerAsk(card, chosen.join('、')); };
        row.appendChild(sub);
      } else {
        opts.forEach(function (o) {
          var b = el('button', 'askcard__opt');
          b.appendChild(el('b', null, o.label || ''));
          if (o.description) b.appendChild(el('span', null, o.description));
          b.onclick = function () { answerAsk(card, o.label || ''); };
          row.appendChild(b);
        });
        if (!opts.length) {
          var fb2 = el('button', 'askcard__btn askcard__btn--primary', '用输入框内容作答');
          fb2.onclick = function () { answerAsk(card, $('input').value.trim() || '是'); };
          row.appendChild(fb2);
        }
      }
      card.appendChild(row);
    });
    pushItem(card, true);
  }

  function answerAsk(card, text) {
    control('answer', text).then(function (ok) {
      if (!ok) return;
      card.innerHTML = '';
      var head = el('div', 'askcard__head');
      head.appendChild(el('span', 'askcard__dot askcard__dot--ok'));
      head.appendChild(el('span', null, '已答复：' + text));
      card.appendChild(head);
    });
  }

  /* ── 三项基础能力：引擎 / 模型 / 授权 ──
     移动端原先只有引擎选择，模型与授权都是空的，而这两样恰恰是「能不能安全用」的
     关键。所以这里补齐三件事，并把授权做成发送键旁的一枚胶囊：
     四个档位要能一眼分清，full 更是必须看起来「危险」。 */

  /* 四档授权模型，取值与文案对齐 internal/agent/permission.go（唯一真相），
     顺序照 PermissionTiers() 的声明顺序。首项「沿用默认」= 空值：不传
     --permission，由 core 用默认档 —— 刻意不给它预设某一档。 */
  var PERMISSION_TIERS = [
    { v: '', label: '沿用默认', hint: '不传 --permission，由 core 用默认档' },
    { v: 'manual', label: '手动审批', hint: '每个写操作都要你点头（manual）' },
    { v: 'accept-edits', label: '自动接受编辑', hint: '编辑自动过，其余仍问（accept-edits）' },
    { v: 'auto', label: '自动审批', hint: '自动审批常规操作（auto）' },
    { v: 'full', label: '完全访问', hint: '等同 --dangerously-skip-permissions（full）' }
  ];

  /* 当前引擎支不支持显式选档。core 只在**显式传过** --permission 时才校验
     （internal/cli/ask.go::resolvePermissionTier），不支持就别传。
     优先读 capabilities 里的 permission 字段，读不到再退回 core 当前接线的引擎名单。 */
  function engineSupportsPermission(name) {
    var e = (state.engines || []).filter(function (x) { return x.engine === name; })[0];
    if (!e) return false;
    if ((e.capabilities || []).indexOf('permission') >= 0) return true;
    return ['claude', 'codebuddy', 'codebuddy-ai'].indexOf(name) >= 0;
  }

  function tierOf(v) {
    for (var i = 0; i < PERMISSION_TIERS.length; i++) if (PERMISSION_TIERS[i].v === v) return PERMISSION_TIERS[i];
    return PERMISSION_TIERS[0];
  }

  function renderPermission() {
    var b = $('btnPerm');
    if (!b) return;
    var t = tierOf(state.permission || '');
    b.textContent = t.label;
    b.setAttribute('data-tier', state.permission || '');
    b.title = t.hint;
    b.disabled = !engineSupportsPermission(state.engine);
  }

  /* 引擎选完，模型与授权的可选范围都要跟着变（模型属于引擎，授权取决于引擎支不支持）。 */
  function onEnginePicked(name) {
    state.engine = name;
    var models = (state.engines || []).filter(function (x) { return x.engine === name; })[0];
    var list = (models && models.models) || [];
    if (list.indexOf(state.model) < 0) state.model = '';
    if (!engineSupportsPermission(name)) state.permission = '';
    if ($('chatTitle').textContent.indexOf('新会话') === 0) $('chatTitle').textContent = '新会话 · ' + name;
    renderPermission();
  }

    /* ══════════ 组件外壳：对话区 + 输入区 ══════════
       下面这些是宿主原先自己写的那部分（会话详情里的发送、底部面板、接线），
       抽到组件里由组件负责 —— 这样两端不必各写一遍。 */

    function autoGrow() {
      var t = ctx.input;
      t.style.height = 'auto';
      t.style.height = Math.min(t.scrollHeight, 200) + 'px';
    }

    async function send() {
      var text = ctx.input.value.trim();
      if (!text || state.busy) return;
      if (!state.engine) { noticeItem('还没有可用引擎：请先选一个引擎'); return; }
      userBubble(text, false);
      ctx.input.value = ''; autoGrow();
      state.busy = true; setSendMode('busy');
      finalizeRsn(); finalizeAgent(true); stopPrevTool();
      setWorking('Agent 运行中');
      state.resultSeen = false;
      state.askId = null;
      if (opts.onBusy) { try { opts.onBusy(true); } catch (e) {} }
      try {
        var payload = { engine: state.engine, prompt: text };
        if (state.model) payload.model = state.model;
        /* 空值就不传：让 core 用它自己的默认档，不替用户预设（安全语义）。
           引擎不支持显式选档时同样不传 —— core 只在显式传过时才校验。 */
        if (state.permission && engineSupportsPermission(state.engine)) payload.permission = state.permission;
        if (state.session) payload.session = state.session;
        /* 一轮的完整生命周期交给 transport：
             onOpen  → 拿到轮次 id（控制通道的唯一落点）
             onEvent → 逐条 core 事件
             onEnd   → 本轮收尾（含 core 退出码）
           transport 负责保证 onEnd 一定会被调到一次 —— 否则界面会永远卡在 busy。 */
        await new Promise(function (resolve) {
          var settled = false;
          var finish = function (code) {
            if (settled) return;
            settled = true;
            if (code !== 0 && code != null) noticeItem('core 退出码 ' + code);
            resolve();
          };
          try {
            transport.ask(payload, {
              onOpen: function (runID) { state.askId = runID || state.askId; },
              onEvent: function (ev) { try { render(ev); } catch (e) {} },
              onEnd: finish
            });
          } catch (e) {
            noticeItem('发送失败：' + (e.message || e));
            finish(null);
          }
        });
      } catch (e) {
        noticeItem('发送失败：' + (e.message || e));
      } finally {
        clearWorking(); finalizeRsn(); finalizeAgent(true); stopPrevTool();
        if (!state.resultSeen) pushItem(sepItem('完成'));
        state.busy = false; state.askId = null; setSendMode('idle');
        if (opts.onBusy) { try { opts.onBusy(false); } catch (e) {} }
      }
    }

    function setSendMode(mode) {
      var b = ctx.sendBtn;
      /* ⚠️ busy 时**不能**把它 disabled：那样点它不会派发 click，
         它就从「中断」退化成一枚按不动的图标。改指中断后，
         重复发送由 send() 自己的 state.busy 守卫拦住，不需要靠 disabled 防。 */
      b.disabled = false;
      b.classList.toggle('sendbtn--stop', mode === 'busy');
      b.innerHTML = '';
      b.appendChild(svgIcon(mode === 'busy' ? 'i-clock' : 'i-up', 17));
      b.setAttribute('aria-label', mode === 'busy' ? '中断' : '发送');
      b.title = mode === 'busy' ? '中断这一轮' : '发送';
    }

    /* ── 选择器浮层 ──
       fixed + 挂 body，用视口坐标定位：不受任何祖先的 overflow / transform 影响。
       这一条踩过坑 —— 菜单 append 进 flex 行里做 absolute 时宽高会塌成 0×0。 */
    var openPick = null;
    function closePick() {
      if (openPick) { openPick.remove(); openPick = null; }
      document.removeEventListener('mousedown', onPickDown, true);
      document.removeEventListener('keydown', onPickKey, true);
      window.removeEventListener('resize', closePick);
      window.removeEventListener('scroll', closePick, true);
    }
    function onPickDown(e) { if (!(e.target.closest && e.target.closest('.pick'))) closePick(); }
    function onPickKey(e) { if (e.key === 'Escape') { e.preventDefault(); closePick(); } }
    function placePick(p, anchor) {
      var a = anchor.getBoundingClientRect(), w = p.offsetWidth, h = p.offsetHeight;
      var left = a.left;
      if (left + w > window.innerWidth - 8) left = Math.max(8, window.innerWidth - w - 8);
      var top = a.bottom + 6;
      if (top + h > window.innerHeight - 8) top = Math.max(8, a.top - h - 6);
      p.style.left = Math.round(left) + 'px';
      p.style.top = Math.round(top) + 'px';
    }
    function showPick(anchor, build) {
      if (openPick && openPick._anchor === anchor) { closePick(); return; }
      closePick();
      var p = el('div', 'pick pick--on');
      p._anchor = anchor;
      build(p);
      document.body.appendChild(p);
      placePick(p, anchor);
      openPick = p;
      document.addEventListener('mousedown', onPickDown, true);
      document.addEventListener('keydown', onPickKey, true);
      window.addEventListener('resize', closePick);
      window.addEventListener('scroll', closePick, true);
    }
    function pickRow(label, desc, selected, onPick, danger) {
      var b = el('button', 'pick__row' + (danger ? ' pick__danger' : ''));
      b.type = 'button';
      b.setAttribute('aria-selected', String(!!selected));
      b.appendChild(el('span', 'pick__ck', selected ? '✓' : ''));
      var col = el('span');
      col.appendChild(el('span', 'pick__l', label));
      if (desc) col.appendChild(el('span', 'pick__d', desc));
      b.appendChild(col);
      b.onclick = function () { closePick(); onPick(); };
      return b;
    }

    /* 引擎 · 模型：一级列引擎，点开二级是该引擎的模型 */
    function openEnginePicker(anchor) {
      showPick(anchor, function (p) {
        if (!state.engines.length) {
          p.appendChild(el('div', 'pick__cap', '没有可用引擎'));
          p.appendChild(el('div', 'pick__row', 'magic-agent --engines 会给每个引擎的一键安装命令'));
          return;
        }
        p.appendChild(el('div', 'pick__cap', '选择引擎'));
        state.engines.forEach(function (en) {
          var models = en.models || [];
          var row = pickRow(en.engine + (en.ok ? '' : '（未安装）'),
            en.ok ? (models.length + ' 个模型') : '未安装', state.engine === en.engine, function () {
              if (!en.ok) return;
              state.engine = en.engine;
              if (models.indexOf(state.model) < 0) state.model = '';
              if (!engineSupportsPermission(en.engine)) state.permission = '';
              syncPickers();
            });
          if (!en.ok) { row.disabled = true; p.appendChild(row); return; }
          p.appendChild(row);
          if (models.length) {
            p.appendChild(el('div', 'pick__cap', '　└ ' + en.engine + ' 的模型'));
            p.appendChild(pickRow('默认模型', '不传 -m，由引擎自己挑',
              state.engine === en.engine && !state.model, function () {
                state.engine = en.engine; state.model = ''; syncPickers();
              }));
            models.forEach(function (n) {
              p.appendChild(pickRow(n, '', state.engine === en.engine && state.model === n, function () {
                state.engine = en.engine; state.model = n; syncPickers();
              }));
            });
          }
        });
      });
    }

    function openPermissionPicker(anchor) {
      showPick(anchor, function (p) {
        p.appendChild(el('div', 'pick__cap', '授权档位'));
        if (!engineSupportsPermission(state.engine)) {
          p.appendChild(el('div', 'pick__row', (state.engine || '当前引擎') + ' 不支持显式选档'));
          p.appendChild(el('div', 'pick__d', '仅 claude / codebuddy / codebuddy-ai，会沿用 core 默认'));
          return;
        }
        PERMISSION_TIERS.forEach(function (t) {
          p.appendChild(pickRow(t.label, t.hint, (state.permission || '') === t.v, function () {
            state.permission = t.v; syncPickers();
          }, t.v === 'full'));
        });
      });
    }

    function loadHistory(sid, after) {
      if (!sid) return Promise.resolve(0);
      return Promise.resolve(transport.readSession(sid, after == null ? {} : { after: after }))
        .then(function (r) {
          var events = (r && r.events) || [];
          events.sort(function (a, b) { return Number(a.seq || 0) - Number(b.seq || 0); });
          events.forEach(function (ev) { try { render(ev); } catch (e) { /* 单条坏事件吞掉 */ } });
          var t = thread(); t.scrollTop = t.scrollHeight;
          return events.length;
        });
    }

    function syncPickers() {
      var casc = root.querySelector('.casc');
      if (casc) {
        casc.querySelector('.casc__p').textContent = state.engine || '选择引擎';
        casc.querySelector('.casc__s').textContent = state.model || '默认模型';
      }
      var chip = ctx.permBtn;
      if (chip) {
        var t = tierOf(state.permission || '');
        chip.querySelector('span').textContent = '授权 · ' + t.label;
        chip.setAttribute('data-tier', state.permission || '');
        chip.disabled = !engineSupportsPermission(state.engine);
        chip.title = engineSupportsPermission(state.engine) ? t.hint
          : (state.engine || '当前引擎') + ' 不支持显式选授权档位（仅 claude / codebuddy / codebuddy-ai），会沿用 core 默认';
      }
    }

    /* ══════════ 搭 DOM ══════════
       thread 同时是滚动容器与条目容器（保留段落里 pushItem / clearThread / scrollTop
       都指向它）—— 所以**不再套内层 wrapper**，宽屏居中靠 CSS 的对称 padding。 */
    var wrap = el('div', 'conv');
    var threadEl = el('div', 'conv__thread');
    ctx.thread = threadEl;
    wrap.appendChild(threadEl);
    /* 组件内不需要「回到底部」按钮，留个游离节点承接保留段落里的赋值，不让它报错 */
    ctx.bottomBtn = el('span', 'tobottom');

    var form = el('form', 'composer');
    var cbox = el('div', 'composer__box');
    ctx.input = el('textarea'); ctx.input.rows = 1; ctx.input.placeholder = '回复 Agent';
    cbox.appendChild(ctx.input);
    var crow = el('div', 'composer__row');
    var cleft = el('div', 'composer__left');
    var casc = el('button', 'casc');
    casc.type = 'button';
    casc.appendChild(svgIcon('i-cpu', 14));
    casc.appendChild(el('span', 'casc__p', '选择引擎'));
    casc.appendChild(el('span', 'casc__sep', '·'));
    casc.appendChild(el('span', 'casc__s', '默认模型'));
    casc.appendChild(svgIcon('i-chev', 13));
    casc.className = 'casc';
    ctx.permBtn = el('button', 'permchip');
    ctx.permBtn.type = 'button';
    ctx.permBtn.appendChild(svgIcon('i-set', 14));
    ctx.permBtn.appendChild(el('span', null, '授权 · 沿用默认'));
    cleft.appendChild(casc); cleft.appendChild(ctx.permBtn);
    ctx.sendBtn = el('button', 'sendbtn');
    ctx.sendBtn.type = 'submit';
    crow.appendChild(cleft); crow.appendChild(ctx.sendBtn);
    cbox.appendChild(crow);
    form.appendChild(cbox);
    wrap.appendChild(form);
    root.appendChild(wrap);

    /* 空态：一句话说明这个组件等什么，别让人对着一片空白猜 */
    var empty = el('div', 'conv__empty', '还没有事件。写一句任务，点右下角发送。');
    threadEl.appendChild(empty);

    /* ══════════ 接线 ══════════ */
    threadEl.addEventListener('click', function (ev) {
      var t = ev.target && ev.target.closest ? ev.target.closest('[data-copy-idx]') : null;
      if (t) doCopy(copyBin[Number(t.getAttribute('data-copy-idx'))]);
    });
    threadEl.addEventListener('scroll', function () { state.autoFollow = nearBottom(); syncBottomBtn(); });
    ctx.input.addEventListener('input', autoGrow);
    form.addEventListener('submit', function (e) { e.preventDefault(); send(); });
    ctx.input.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); send(); }
    });
    casc.addEventListener('click', function (e) { e.stopPropagation(); openEnginePicker(casc); });
    ctx.permBtn.addEventListener('click', function (e) { e.stopPropagation(); openPermissionPicker(ctx.permBtn); });
    ctx.sendBtn.addEventListener('click', function (e) {
      if (!state.busy) return;          // 空闲时照常 submit
      e.preventDefault();
      control('interrupt');
    });

    /* 首个事件进来就把空态拿走 */
    var _render = render;
    render = function (ev) {
      if (empty.parentNode) empty.remove();
      if (opts.onEvent) { try { opts.onEvent(ev); } catch (e) {} }
      return _render(ev);
    };

    function newSession() {
      state.session = null; state.askId = null;
      clearThread();
      threadEl.appendChild(empty);
      if (ctx.titleEl) ctx.titleEl.textContent = state.engine ? ('新会话 · ' + state.engine) : '新会话';
      setTimeout(function () { ctx.input.focus(); }, 0);
    }
    function setSession(sid, s) {
      state.session = sid || null;
      if (s && s.engine) { state.engine = s.engine; }
      clearThread();
      threadEl.appendChild(empty);
      if (ctx.titleEl) ctx.titleEl.textContent =
        (s && (s.prompt_head || s.title)) || (sid ? String(sid).slice(0, 8) : '会话');
      syncPickers();
      if (sid) {
        loadHistory(sid).catch(function (e) { noticeItem('历史加载失败：' + (e.message || e)); });
      }
      setTimeout(function () { ctx.input.focus(); }, 0);
    }
    function refreshEngines() {
      return Promise.resolve(transport.listEngines()).then(function (list) {
        state.engines = Array.isArray(list) ? list : [];
        if (!state.engine) {
          var first = state.engines.filter(function (x) { return x.ok; })[0];
          if (first) state.engine = first.engine;
        }
        syncPickers();
        return state.engines;
      });
    }
    function destroy() {
      closePick();
      if (wrap.parentNode) wrap.parentNode.removeChild(wrap);
    }

    if (state.engines.length) syncPickers();
    else refreshEngines().catch(function () { /* 拉不到就保持空清单，选择器会如实说没有可用引擎 */ });
    setSendMode('idle');
    syncPickers();
    if (state.session) setSession(state.session);

    return {
      version: 1,
      el: root,
      state: state,
      send: send,
      interrupt: function () { return control('interrupt'); },
      stop: function () { return control('stop'); },
      answer: function (text) { return control('answer', text); },
      setEngine: function (n) { state.engine = n; syncPickers(); },
      setModel: function (n) { state.model = n; syncPickers(); },
      setPermission: function (v) { state.permission = v; syncPickers(); },
      setEngines: function (list) { state.engines = list || []; syncPickers(); },
      refreshEngines: refreshEngines,
      setSession: setSession,
      newSession: newSession,
      loadHistory: loadHistory,
      render: function (ev) { render(ev); },
      syncPickers: syncPickers,
      destroy: destroy
    };
  }

  window.MagicConversation = { version: 1, create: create };
})();
