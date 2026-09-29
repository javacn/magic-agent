/* ---------- 完整 Markdown 渲染器（专业栈：marked + DOMPurify + highlight.js）----------
   解析：marked（GFM）——表格对齐 / 任务列表 / 删除线 / 自动链接 / 围栏语言标签，
        远超手写正则的边界正确性（嵌套列表、松散列表、行内混排等）。
   净化：DOMPurify——文档正文多来自网页采集（不可信源），渲染后必须消毒，
        防脚本 / 事件属性注入；放行 title / target / align / class 等阅读必需属性。
   高亮：highlight.js——代码块按 ```lang 自动着色（hljs.highlightElement）。
        ⚠️ 它是**按需加载**的（2026-09-19）：129KB，只有真渲染出代码块时才注入，
        见下方 loadHljs()，不要在 index.html 里同步引它。
   保留契约：仍返回 { html, anchors }。anchors 为标题索引（level/text/id），
        供右侧目录（.md-toc）点击定位；标题锚点 id 稳定（h-1 / h-2 …），
        悬停显示「#」。与既有的 .md-body 版式 / .doc-reader 结构完全对接。 */

const MD_STACK_READY = (typeof marked !== "undefined") && (typeof DOMPurify !== "undefined");
let _mdAnchors = [], _mdHSeq = 0;

/** 取纯文本（去行内标记），用于目录标题 */
function mdPlainText(s){
  return String(s||"")
    .replace(/!\[([^\]]*)\]\([^)]*\)/g,"$1")
    .replace(/\[([^\]]*)\]\([^)]*\)/g,"$1")
    .replace(/`([^`]*)`/g,"$1")
    .replace(/\*\*([^*]+)\*\*/g,"$1").replace(/__([^_]+)__/g,"$1")
    .replace(/\*([^*]+)\*/g,"$1").replace(/_([^_]+)_/g,"$1")
    .replace(/~~([^~]+)~~/g,"$1")
    .replace(/\s+/g," ").trim();
}

/* 一次性配置 marked 渲染器：
   - heading  → 稳定锚点 id + 收集目录 + 「#」锚点（.h-anc，::before 出 # 号）
   - code     → 保留 code-lang 角标 + hljs 类，随后由 highlightElement 上色
   - listitem → GFM 任务项改写成既有 .task-li 结构；普通项回退默认渲染
   - link     → 外链加 target=_blank rel=noopener，防钓鱼落页 */
if(MD_STACK_READY){
  marked.use({
    gfm:true,
    breaks:false,
    renderer:{
      heading(tok){
        const inner = this.parser.parseInline(tok.tokens||[]);
        const id = "h-" + (++_mdHSeq);
        _mdAnchors.push({ level:tok.depth, text:mdPlainText(tok.text), id });
        return `<h${tok.depth} id="${id}">${inner}<a class="h-anc" href="#${id}" title="定位此标题" aria-hidden="true"></a></h${tok.depth}>\n`;
      },
      code(tok){
        const lang = String(tok.lang||"").split(/\s+/)[0];
        const cls = "hljs" + (lang ? " language-" + lang : "");
        // 转义代码正文：既保证 <tag> 原样显示，也避免消毒阶段误删
        const body = String(tok.text||"")
          .replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;");
        return `<pre>${lang?`<span class="code-lang">${lang}</span>`:""}<code class="${cls}">${body}</code></pre>\n`;
      },
      listitem(tok){
        if(tok.task){
          const body = this.parser.parse(tok.tokens||[], !!tok.loose);
          return `<li class="task-li${tok.checked?" done":""}"><span class="bx">✓</span><span class="tx">${body}</span></li>\n`;
        }
        return false;   // 回退 marked 默认（保留嵌套 <ul>/<ol>）
      },
      link(tok){
        const text = this.parser.parseInline(tok.tokens||[]);
        const href = String(tok.href||"");
        const ti = tok.title ? ` title="${tok.title}"` : "";
        const ext = /^https?:/i.test(href);
        return `<a href="${href}"${ext?' target="_blank" rel="noopener"':""}${ti}>${text}</a>`;
      },
    },
  });
}

/** 手写兜底：vendor 未加载时仍能阅读（仅线性块级，极端降级） */
function mdFallback(src){
  const esc = (s)=>String(s??"").replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;");
  let html = "", inCode = false, buf = [];
  const flushPara = ()=>{ if(buf.length){ html += "<p>" + esc(buf.join(" ")) + "</p>"; buf = []; } };
  String(src||"").split(/\r?\n/).forEach((ln)=>{
    if(/^\s*```/.test(ln)){ flushPara(); if(inCode){ html += `<pre><code>${esc(buf.join("\n"))}</code></pre>`; buf=[]; } inCode=!inCode; return; }
    if(inCode){ buf.push(ln); return; }
    const hm = /^(#{1,6})\s+(.*)$/.exec(ln);
    if(hm){ flushPara(); const lv=hm[1].length, id="h-"+(++_mdHSeq);
      _mdAnchors.push({level:lv,text:mdPlainText(hm[2]),id});
      html += `<h${lv} id="${id}">${esc(hm[2])}</h${lv}>`; return; }
    if(/^\s*$/.test(ln)){ flushPara(); return; }
    buf.push(ln.trim());
  });
  flushPara();
  return html;
}

/** 把「独占一行的图片路径」变成 markdown 图片，让对话里**直接看到图**（2026-09-19）。
 *
 *  为什么需要：引擎产出/引用的截图通常写成**一行绝对路径**（或本项目入库时的 `[图片] <路径>`），
 *  不是 markdown —— 不转的话对话里只有一行字，看不到图；而飞书那边的完成回执也会把同一批图
 *  上传成内联图（见 feishu-bridge.cjs::extractImageRefs），两边口径对齐。
 *
 *  ⚠️ 只认**独占一行**的绝对路径 + 常见图片扩展名：
 *    · 不碰行内路径（正文里提到 `src/js/xxx.png` 不该变成图）；
 *    · 路径里不含空格 / 引号 / 括号（含了就不转，免得把 markdown 语法拼坏）；
 *    · alt 也填路径 —— 图加载不出来时还能看见它指向哪。 */
function mdLocalizeImagePaths(src){
  const EX = "png|jpe?g|gif|webp|bmp";
  return String(src||"")
    // 本项目入库时给飞书图片的写法：`[图片] /abs/x.png`
    .replace(new RegExp("^[ \\t]*\\[图片\\][ \\t]*(/\\S+\\.(?:" + EX + "))[ \\t]*$", "gim"),
      (_a, p) => `![${p}](${p})`)
    // 裸路径独占一行
    .replace(new RegExp("^[ \\t]*(/[^\\s`'\"()<>]+\\.(?:" + EX + "))[ \\t]*$", "gim"),
      (_a, p) => `![${p}](${p})`);
}

/** 渲染 Markdown → { html, anchors } */
function mdToHtml(src){
  _mdAnchors = []; _mdHSeq = 0;
  src = mdLocalizeImagePaths(src);
  if(!MD_STACK_READY){ return { html: mdFallback(src), anchors: _mdAnchors }; }
  let raw;
  try{
    raw = marked.parse(String(src||""));
  }catch(e){
    console.warn("[md] marked 解析失败，回退简易渲染：", e && e.message);
    return { html: mdFallback(src), anchors: _mdAnchors };
  }
  // 消毒：放行阅读必需的属性 / 标签；禁 script、事件属性、javascript: 协议
  const html = DOMPurify.sanitize(raw, {
    ADD_ATTR: ["target", "rel", "align", "class", "id", "title", "disabled", "checked", "type"],
    FORBID_TAGS: ["script", "style", "iframe", "form", "input"],
    FORBID_ATTR: ["onerror", "onload", "onclick", "style"],
  });
  return { html, anchors: _mdAnchors };
}

/** 只取 HTML（兼容旧调用） */
function mdToHtmlOnly(src){ return mdToHtml(src).html; }

/* ---------- highlight.js 按需加载（2026-09-19）----------
   129KB，比 marked + purify + floating-ui 加起来还大 —— 但只有真渲染出代码块时才用得到。
   原来它在 index.html 里同步加载，每次启动都白解析一遍。现在首次需要时才注入；
   注入完成前代码块以纯文本呈现（下面的 typeof hljs 分支本来就兜住了，不影响功能）。
   流式渲染每闭合一段都会调一次 highlightIn → 加载期间可能登记多个根节点，
   用 Set 去重，加载完统一补扫一遍。 */
let _hljsLoading = false;
const _hljsPending = new Set();
function loadHljs(){
  if(typeof hljs !== "undefined" || _hljsLoading) return;
  _hljsLoading = true;
  const s = document.createElement("script");
  s.src = "vendor/highlight.min.js";
  s.onload = () => {
    _hljsLoading = false;
    _hljsPending.forEach((r)=>{ try{ highlightIn(r); }catch(e){ /* 节点可能已卸载，忽略 */ } });
    _hljsPending.clear();
  };
  s.onerror = () => { _hljsLoading = false; };   // 加载失败就一直纯文本，不影响功能
  document.head.appendChild(s);
}

/** 对已插入 DOM 的代码块执行语法高亮；hljs 还没就绪就先登记，加载完补扫。
 *  ⚠️ 先查有没有代码块再决定要不要惊动 129KB —— 渲染普通文档（无 ``` 块）时
 *  highlightIn 照样会被调一次，不能让它把 highlight.js 白拉下来。 */
function highlightIn(root){
  if(!root) return;
  const blocks = root.querySelectorAll("pre code");
  if(!blocks.length) return;
  if(typeof hljs === "undefined"){ _hljsPending.add(root); loadHljs(); return; }
  blocks.forEach((el)=>{
    if(el.dataset.hl) return;
    try{ hljs.highlightElement(el); el.dataset.hl = "1"; }catch(e){ /* 未知语言静默跳过 */ }
  });
}

/* ---------- 右栏多标签系统 ----------
   tab 形态：{ id, kind:'browser'|'doc', docId?, el, paneEl }
   - 浏览器工具标签常驻、不可关闭
   - 每份产品文档直接就是一个标签页（不经任何索引层、无弹窗），内容即完整 Markdown 阅读器 */
let dockTabs = [];          // 全部标签（含常驻浏览器）
let activeDockTab = null;   // 当前激活 tab id
let docTabSeq = 0;

const DOC_ICON = `<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M6 3.5h8l4 4v13a1 1 0 01-1 1H6a1 1 0 01-1-1v-16a1 1 0 011-1z"/><path d="M14 3.5v4h4M8.5 12h7M8.5 15h7M8.5 18h4"/></svg>`;

function findTab(id){ return dockTabs.find(t=>t.id===id); }

/** 初始化常驻「浏览器工具」标签 */
function initDockTabs(){
  const browser = { id:'tab-browser', kind:'browser', el:$("#tab-browser"), paneEl:null };
  const features = { id:'tab-req-features', kind:'features', el:$("#tab-req-features"), paneEl:$("#dock-pane-tab-req-features") };
  /* 审查截图（2026-09-20）：与「文档阅读」同族 —— 默认收起、点卡片才亮
     （见 reqShotsShowTab 与 app.css 的 #tab-req-shots[hidden]）。 */
  const shots = { id:'tab-req-shots', kind:'shots', el:$("#tab-req-shots"), paneEl:$("#dock-pane-tab-req-shots") };
  dockTabs = [browser, features, shots];
  browser.el.addEventListener("click", ()=>activateDockTab('tab-browser'));
  features.el.addEventListener("click", ()=>{
    activateDockTab('tab-req-features');
    // 2026-09-18 二次调整：列表在主视图，本面板只是「单篇阅读器」。
    // 直接点本 tab 时，若主视图已经在累计档且有缓存的当前文档 → 不动；否则给个引导。
    if(!$("#rf-ws-name") || $("#rf-ws-name").textContent.trim() === "—"){
      reqFeatPaneHint();
    }
  });
  /* 直接点这枚 tab = 「回到我上次看的那条需求的截图」（面板内容由 reqShotsOpen 填）。
     内容为空时给一句引导，而不是一片空白 —— 与 features tab 的 reqFeatPaneHint 同一考虑。 */
  shots.el.addEventListener("click", ()=>{
    activateDockTab('tab-req-shots');
    if(!$("#rs-body") || !$("#rs-body").childElementCount) reqShotsPaneHint();
  });
  activateDockTab('tab-browser');
}

/** 亮出「文档阅读」标签（2026-09-20 用户：「有侧边栏不需要默认显示文档阅读 点击文档才显示」）。
 *  旧行为：这枚 tab 与「浏览器」并列**常驻**，打开侧栏就先看见一个空阅读器的入口 ——
 *  用户不想默认看到它。现在它由 index.html 的 hidden 属性直出收起，
 *  首次真去读某篇文档时才（由 reqFeatOpenInDock / rdocOvOpenInDock）调这里摘掉 hidden。
 *  幂等：重复调用无副作用；亮出来之后就常驻（不做「读完又收回去」——
 *  关闭动作与「点了没反应」是最容易让人以为功能坏了的两种交互）。
 *  ⚠️ 改的是 el.hidden（属性），不是样式；.dock-tab-btn 的 display 会盖掉 UA 的
 *     [hidden]{display:none}，所以 app.css 里配了 #tab-req-features[hidden]。 */
function reqFeatShowTab(){
  const tab = findTab('tab-req-features') || null;
  const el = (tab && tab.el) || $("#tab-req-features");
  if(el) el.hidden = false;
}

/** 亮出「审查截图」标签（2026-09-20）—— 与 reqFeatShowTab 同一套口径（hidden 属性，
 *  不是样式；幂等；亮出来就常驻）。调用方只有 reqShotsOpen（点卡片那枚「截图 N」）。 */
function reqShotsShowTab(){
  const tab = findTab('tab-req-shots') || null;
  const el = (tab && tab.el) || $("#tab-req-shots");
  if(el) el.hidden = false;
}

/** 打开某份产品文档 → 直接新建/聚焦其标签页（完整 Markdown 阅读器） */
function openDocTab(docId){
  const exist = dockTabs.find(t=>t.kind==='doc' && t.docId===docId);
  if(exist){ activateDockTab(exist.id); return exist; }
  const tab = { id:'doc-' + (++docTabSeq), kind:'doc', docId };
  const pill = document.createElement('button');
  pill.className = 'dock-tab-btn';
  pill.dataset.tab = tab.id;
  pill.setAttribute('role','tab');
  pill.innerHTML = `<span class="ti">${DOC_ICON}</span><span class="tt"></span><span class="tx" title="关闭">✕</span>`;
  pill.addEventListener('click', (e)=>{
    if(e.target.classList.contains('tx')){ closeDockTab(tab.id); return; }
    activateDockTab(tab.id);
  });
  const pane = document.createElement('div');
  pane.className = 'panel-body br-dock-body dock-pane';
  pane.id = 'dock-pane-' + tab.id;
  pane.setAttribute('role','tabpanel');
  tab.el = pill; tab.paneEl = pane;
  dockTabs.push(tab);
  $("#dock-tab-scroll").appendChild(pill);
  $("#dock-panes").appendChild(pane);
  renderDocPane(tab);
  activateDockTab(tab.id);
  return tab;
}

function closeDockTab(id){
  const i = dockTabs.findIndex(t=>t.id===id);
  if(i < 0) return;
  const tab = dockTabs[i];
  if(tab.kind === 'browser') return;       // 浏览器常驻不可关
  tab.el.remove(); tab.paneEl.remove();
  dockTabs.splice(i, 1);
  if(activeDockTab === id){
    const next = dockTabs[i] || dockTabs[i-1] || dockTabs[0];
    activateDockTab(next ? next.id : null);
  }
}

function activateDockTab(id){
  activeDockTab = id;
  const tab = findTab(id);
  // 浏览器标签的 pane 复用静态 #dock-pane-browser；其余用各自 paneEl
  dockTabs.forEach(t=>{
    const on = t.id === id;
    t.el.classList.toggle('active', on);
    t.el.setAttribute('aria-selected', on);
    const pane = t.paneEl || (t.kind==='browser' ? $("#dock-pane-browser") : null);
    if(pane) pane.classList.toggle('active', on);
  });
  if(tab && tab.kind === 'doc') renderDocPane(tab);
  if(tab && tab.kind === 'browser') syncWebviewViewport();
  /* 需要「展开右栏才看得见」的两枚标签（文档阅读 · 2026-09-18 / 审查截图 · 2026-09-20）：
     切到它们时把右栏打开 —— 收合态下整个 dock 列宽归 0、面板被 overflow 裁掉，看不见。
     （浏览器 tab 不用管：右栏本就是被它打开的。） */
  if(tab && (tab.kind === 'features' || tab.kind === 'shots') && !isDockOpen()) openBrowserDock();
}

/* ---------- 文档元信息解析 ----------
   优先取明细（含 markdown / detail / url）；数据源未连接时明细为空，
   回落到列表行 prdData[ id, title, module, version, status, date ]，
   保证标签页标题与工具条始终显示可读标题，而不是裸编号。
   正文再回落一层到内置种子正文 PRD_SEED_MD（00-seed-docs.js）：
   预览态没有后端明细，没有这层兜底就只剩「暂无 Markdown 正文」空壳
   （2026-09-20 用户：「在产品列表上要能看到文档」）。 */
function docMeta(id){
  const d = prdDetailIndex[id] || null;
  // 取元信息用**搜索前**的全量行（prdRowsAll）：搜索把某行筛掉后，打开着的阅读器不该跟着掉标题
  const row = prdRowsAll.find(r => r[0] === id) || prdData.find(r => r[0] === id) || null;
  if(!d && !row) return { id };
  const seed = (typeof PRD_SEED_MD !== "undefined") ? PRD_SEED_MD[id] : null;
  return {
    id,
    title:   (d && d.title)   || (row && row[1]) || id,
    module:  (d && d.module)  || (row && row[2]) || "",
    version: (d && d.version) || (row && row[3]) || "",
    status:  (d && d.status)  || (row && row[4]) || "",
    date:    (d && d.date)    || (row && row[5]) || "",
    url:     d && d.url,
    markdown: (d && d.markdown) || seed || undefined,
    detail:  d && d.detail,
    workspace: d && d.workspace,          // 供用例继承归属（工作台按 workspace 过滤）
  };
}

/* ---------- 文档标签页 = 完整 Markdown 阅读器 ----------
   渲染内容：
     .md-bar   工具条（文档编号 / 版本 / 状态 / 目录 / 导出 / 来源 / 置顶）
     .md-toc   目录（由正文标题自动生成，点击平滑定位）
     .md-body  正文（完整 Markdown 版式）
   无 Markdown 正文时给出可操作的空态说明（引导去浏览器标签沉淀）。 */
function renderDocPane(tab){
  const d = docMeta(tab.docId);
  const label = d.title || tab.docId;
  tab.el.querySelector('.tt').textContent = label;
  tab.el.title = `${tab.docId} · ${label}`;

  const stText = prdStateText[d.status] || d.status || "—";
  const bar = `<div class="md-bar">
      <span class="no">${tab.docId}</span>
      <span class="st ${d.status||""}">${stText}</span>
      <span class="sp"></span>
      <button class="mb" data-md="toc" title="显示 / 隐藏目录">目录</button>
      <button class="mb" data-md="cases" title="按本文档正文推导测试用例初稿，写入测试用例库（按验证方式分界面类 / 脚本类）">生成用例</button>
      <button class="mb" data-md="export" title="导出为 .md 文件">导出</button>
      ${d.url ? `<a class="mb" href="${String(d.url).replace(/"/g,"&quot;")}" target="_blank" rel="noopener" title="打开来源页面">来源 ↗</a>` : ""}
      <button class="mb" data-md="top" title="回到顶部">↑ 顶部</button>
    </div>`;

  let bodyHtml, anchors = [];
  if(d.markdown){
    const r = mdToHtml(d.markdown);
    bodyHtml = r.html; anchors = r.anchors;
  }else if(d.detail){
    bodyHtml = `<div class="doc-reader-empty">此文档仅有<b>结构快照</b>，还没有 Markdown 正文。</div>`;
  }else{
    bodyHtml = `<div class="doc-reader-empty">这份演示种子文档暂无 Markdown 正文。</div>
      <div class="doc-meta-note">产品模块：<code>${d.module||"—"}</code> · 版本：<code>${d.version||"—"}</code> · 更新：<code>${d.date||"—"}</code></div>`;
  }

  const toc = anchors.length
    ? `<div class="md-toc" id="toc-${tab.id}"><div class="tc-h">目 录</div>${
        anchors.map(a=>`<a class="lv${Math.min(4,a.level)}" data-h="${a.id}" title="${String(a.text).replace(/"/g,"&quot;")}">${String(a.text).replace(/</g,"&lt;")}</a>`).join("")
      }</div>`
    : `<div class="md-toc" id="toc-${tab.id}"><div class="tc-h">目 录</div><div class="tc-e">（正文无标题，无目录）</div></div>`;

  tab.paneEl.innerHTML = `<div class="doc-reader">
      <div class="md-prog" id="prog-${tab.id}"></div>
      ${bar}
      ${toc}
      <div class="doc-pane-body md-body" id="mdb-${tab.id}">${bodyHtml}</div>
    </div>`;

  const root = tab.paneEl.querySelector(".doc-reader");
  const body = tab.paneEl.querySelector(".doc-pane-body");
  const tocEl = tab.paneEl.querySelector(".md-toc");
  const prog = tab.paneEl.querySelector(".md-prog");
  tab.bodyEl = body;

  // 代码块语法高亮（highlight.js）——DOM 就位后再上色
  highlightIn(body);

  // 阅读进度条
  body.addEventListener("scroll", ()=>{
    const max = body.scrollHeight - body.clientHeight;
    prog.style.width = (max > 0 ? Math.min(100, (body.scrollTop / max) * 100) : 0) + "%";
  });

  // 工具条动作
  bar && root.addEventListener("click", (e)=>{
    const b = e.target.closest("[data-md]"); if(!b) return;
    const act = b.dataset.md;
    if(act === "toc"){
      tocEl.classList.toggle("show");
      b.classList.toggle("on", tocEl.classList.contains("show"));
    }else if(act === "cases"){
      // 文档 → 用例（2026-09-20）：推导逻辑与幂等口径都在 11-cases-reports.js::genCasesFromDoc
      genCasesFromDoc(tab.docId);
    }else if(act === "export"){
      exportDocMarkdown(tab.docId, d);
    }else if(act === "top"){
      body.scrollTo({ top:0, behavior:"smooth" });
    }
  });

  // 目录点击 → 平滑定位
  tocEl.addEventListener("click", (e)=>{
    const a = e.target.closest("a[data-h]"); if(!a) return;
    const h = body.querySelector("#" + a.dataset.h);
    if(h) h.scrollIntoView({ behavior:"smooth", block:"start" });
    tocEl.querySelectorAll("a").forEach(x=>x.style.color="");
    a.style.color = "var(--cinnabar-hi)";
  });
  // 正文内锚点（# 号）→ 平滑定位
  body.addEventListener("click", (e)=>{
    const a = e.target.closest("a.h-anc"); if(!a) return;
    e.preventDefault();
    const h = body.querySelector(a.getAttribute("href"));
    if(h) h.scrollIntoView({ behavior:"smooth", block:"start" });
  });
}

/** 导出当前文档为 .md 文件 */
function exportDocMarkdown(docId, d){
  const src = (d && d.markdown) || "";
  if(!src){ toast("此文档暂无 Markdown 正文可导出"); return; }
  const blob = new Blob([src], { type:"text/markdown;charset=utf-8" });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = `${docId}${d && d.title ? "-" + String(d.title).replace(/[\\/:*?"<>|\s]+/g,"_").slice(0,40) : ""}.md`;
  document.body.appendChild(a); a.click();
  setTimeout(()=>{ URL.revokeObjectURL(a.href); a.remove(); }, 0);
  toast("已导出 " + docId + " 为 Markdown 文件");
}

/* 数据源变化后刷新所有已打开的文档标签页 */
function refreshDocTabs(){
  dockTabs.filter(t=>t.kind==='doc').forEach(renderDocPane);
}

