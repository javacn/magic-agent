/* ===== 对话消息的「变体阶梯」（对齐 agents-anywhere 桌面端）=====
 *
 * 背景：anywhere 桌面端把时间线消息拆成一套**有层级的变体**，任何一条事件都能落到
 * 某个合适形态上，且**绝不会因为形态不认识就把内容丢掉**：
 *
 *   ① 气泡消息      用户 → 右对齐气泡；助手 → 无气泡通栏
 *   ② 单行标记行    Marker：16px 图标 + 等宽单行标题 + 状态徽章（+ 可折叠 JSON 明细）
 *   ③ 可折叠明细    有明细的标记行 → chevron + 展开一个代码面板
 *   ④ 分隔条        会话压缩这类「横切事件」→ 左右细线夹一行字
 *   ⑤ 工具卡        ToolCard：单行标题（人话）+ 展开 ToolDetailPanel（命令 / 文件变更 / 输出）
 *   ⑥ 代码面板帧    CodePanelFrame：label 头 + 复制按钮 + 代码体，工具明细与正文代码块共用
 *   ⑦ 文件变更行    FileChangeRow：动作徽章 + 路径 + Diff（4 列栅格 + 5 种行色）
 *
 * 本项目的现状是「只认识认识的那几种」：未知 type / 未配对结果 / 未配对 system 事件
 * 一律落到 `.qa-note` 一行纯文本，**JSON 明细与状态全丢**；工具明细是裸 `<pre>`，
 * 没有 label 头也没有复制；文件改动（Edit/Write）只能靠用户自己读参数 JSON。
 *
 * 本分片补的就是这套阶梯。三条纪律：
 *   1. **形态不认识也要留住内容** —— 兜底永远是可展开的 JSON，而不是一行字；
 *   2. **节点必须扛得住快照重建** —— 快照走 `innerHTML = snapshot`，所以状态一律落
 *      在 class / data-* 上，点击一律走 #qa-stream 事件委托（见 07 的 bindQaStreamDelegation），
 *      绝不在这里 addEventListener；
 *   3. **只有一处写这份标记** —— 工具卡、标记行、代码面板都从这里出，业务侧不手搓。
 *
 * ⚠️ 字号沿用本项目（密集工作台）：标签 9px、代码 10.5px、标题 10.5-11px。
 *    不照抄 anywhere 的 14px —— 那个尺度是给消费级 App 的，搬进工作台会立刻显松。
 */

/* ---------- 变体图标（24 视框 / currentColor / 描边，与全站内联 SVG 同一形制）---------- */
const QA_V_ICONS = {
  // anywhere：command → SquareTerminal
  terminal: '<svg viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="16" rx="2.4"/><path d="m7 9.5 3 2.8-3 2.8M12.8 15.5h4"/></svg>',
  // anywhere：file_change / artifact → FilePenLine
  filepen: '<svg viewBox="0 0 24 24"><path d="M13 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h4"/><path d="M13 3l5 5v3M13 3v5h5"/><path d="M20.2 12.3a1.6 1.6 0 0 1 2.3 2.3l-5.2 5.2-3 .7.7-3z"/></svg>',
  // anywhere：其余工具 → Hammer
  hammer: '<svg viewBox="0 0 24 24"><path d="m15 12-8.4 8.4a2.1 2.1 0 1 1-3-3L12 9"/><path d="M17.6 15 22 10.6M20 5.5l-3 3"/><path d="m11.6 4.4 2.8-2.8 7.6 7.6-2.8 2.8a2 2 0 0 1-2.8 0l-4.8-4.8a2 2 0 0 1 0-2.8z"/></svg>',
  // anywhere：agent_call → Bot
  bot: '<svg viewBox="0 0 24 24"><path d="M12 8V4.6M9.4 4.6h5.2"/><rect x="4" y="8" width="16" height="12" rx="3"/><path d="M2.6 13.4v2.2M21.4 13.4v2.2M9.5 13v2M14.5 13v2"/></svg>',
  // anywhere：web_search → 放大镜
  search: '<svg viewBox="0 0 24 24"><circle cx="11" cy="11" r="6.2"/><path d="m15.8 15.8 3.8 3.8"/></svg>',
  // anywhere：mcp → 插头/节点
  mcp: '<svg viewBox="0 0 24 24"><path d="M9 3v6M15 3v6"/><path d="M6 9h12v3a6 6 0 0 1-6 6 6 6 0 0 1-6-6z"/><path d="M12 18v3"/></svg>',
  // anywhere：system / marker 兜底 → Clock（失败时换 CircleAlert）
  clock: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 7.2v4.8l3.2 1.9"/></svg>',
  alert: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 7.6v5M12 16.2h.01"/></svg>',
  sparkles: '<svg viewBox="0 0 24 24"><path d="M10 4.2 11.3 8 15 9.3 11.3 10.6 10 14.4 8.7 10.6 5 9.3 8.7 8z"/><path d="M18 14.6v3.2M19.6 16.2h-3.2"/></svg>',
  sep: '<svg viewBox="0 0 24 24"><path d="M4 12h16M7.5 8.6 12 4.1l4.5 4.5M16.5 15.4 12 19.9l-4.5-4.5"/></svg>',
  chev: '<svg class="chev" viewBox="0 0 24 24"><path d="M6 9l6 6 6-6"/></svg>',
  copy: '<svg viewBox="0 0 24 24"><rect x="9" y="9" width="11" height="11" rx="2.2"/><path d="M15 6.2V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h.2"/></svg>',
  ok: '<svg viewBox="0 0 24 24"><path d="M4 12.5l5 5L20 6.5"/></svg>',
};
/* 代码面板/明细段通用的复制按钮。**要复制的原文的落位是固定的**：
   面板里放 `.qa-cf-bd pre`，工具明细段里放 `.qa-tool-sec pre` ——
   委托侧按这个约定取文本，所以按钮本身不必带任何 data-*（快照重建也照样能复制）。 */
const QA_COPY_BTN = '<button type="button" class="qa-cf-copy" aria-label="复制" title="复制">'
  + '<span class="i-copy">' + QA_V_ICONS.copy + '</span><span class="i-ok">' + QA_V_ICONS.ok + '</span></button>';

/* ---------- 剪贴板 ----------
   本文件是全站唯一需要「复制节点文本」的地方（代码面板的复制按钮）。
   导航器 API 在 file:// 与无安全上下文下**不存在**（Electron 里也可能没授权），
   所以必须有回退：临时 textarea + execCommand。取不到就静默失败 —— 复制失败弹一堆错更烦。 */
function qaCopyText(text){
  const s = String(text == null ? "" : text);
  if(!s) return;
  if(navigator.clipboard && navigator.clipboard.writeText){
    navigator.clipboard.writeText(s).catch(()=>qaCopyFallback(s));
    return;
  }
  qaCopyFallback(s);
}
function qaCopyFallback(s){
  try{
    const ta = document.createElement("textarea");
    ta.value = s;
    ta.setAttribute("readonly", "");
    ta.style.cssText = "position:fixed;left:-9999px;top:0;opacity:0";
    document.body.appendChild(ta);
    ta.select();
    document.execCommand("copy");
    ta.remove();
  }catch(e){ /* 复制不可用：不打扰用户 */ }
}

/* ---------- ②⑦ 状态徽章（anywhere TimelineStatusBadge）----------
   `h-5 text-[11px]`，failed → destructive，其余 secondary。这里按本项目的字号收成 15px / 9px，
   色阶走令牌（玉=成、朱砂=败、琥珀=跑、灰=中性），与 .qa-agent-chip 同一套语言。 */
function qaStatusTone(status){
  const s = String(status || "").toLowerCase();
  if(s === "failed" || s === "error" || s === "cancelled" || s === "interrupted" || s === "canceled") return "err";
  if(s === "running" || s === "pending" || s === "waiting_approval" || s === "waiting" || s === "in_progress") return "run";
  if(s === "ok" || s === "completed" || s === "complete" || s === "done" || s === "success") return "ok";
  return "";
}
function qaStatusLabel(status){
  const s = String(status || "").toLowerCase();
  return ({
    pending: "待处理", running: "进行中", waiting_approval: "待确认", in_progress: "进行中",
    failed: "失败", error: "失败", cancelled: "已取消", canceled: "已取消", interrupted: "已中断",
    completed: "已完成", complete: "已完成", done: "已完成", success: "已完成", ok: "已完成",
  })[s] || String(status || "");
}
function qaStatusBadgeHtml(status){
  const s = String(status || "").trim();
  if(!s) return "";
  const tone = qaStatusTone(s);
  return `<span class="qa-st${tone ? " " + tone : ""}">${escHtml(qaStatusLabel(s))}</span>`;
}

/* ---------- ⑥ 代码面板帧（anywhere CodePanelFrame）----------
   anywhere 的形制：外框 `rounded-xl border`；头 36px + 底边 + 浅灰底，label 在左、
   复制按钮在右（点后 1.2s 变勾）；体是横向可滚的等宽代码。
   ⚠️ 工具明细、JSON 兜底、正文代码块**共用这一个帧** —— 两处各写一套，观感必然分叉
     （本项目原来工具卡是裸 `<pre>` + 「输入/输出」小标题，正文代码块又是另一套样式）。
   ⚠️ 复制按钮不带监听：点击走 #qa-stream 事件委托（快照重建后照样能点）。
     要复制的原文**永远放在 `.qa-cf-bd pre` 里**（diff 形态放一份 hidden 的），
     委托侧只认这一个位置，不必给每个按钮挂 data-*。 */
function qaCodeFrameNode(label, code, opts){
  const o = opts || {};
  const wrap = document.createElement("div");
  wrap.className = "qa-cf" + (o.flush ? " qa-cf-flush" : "");
  wrap.innerHTML =
    '<div class="qa-cf-hd">'
    + '<span class="qa-cf-lb"></span>'
    + '<span class="qa-cf-hd-r">'
    + (o.action ? '<span class="qa-cf-act"></span>' : '')
    + '<button type="button" class="qa-cf-copy" aria-label="复制" title="复制">'
    + '<span class="i-copy">' + QA_V_ICONS.copy + '</span><span class="i-ok">' + QA_V_ICONS.ok + '</span>'
    + '</button></span></div>'
    + '<div class="qa-cf-bd"></div>';
  wrap.querySelector(".qa-cf-lb").textContent = String(label || "code");
  if(o.action) wrap.querySelector(".qa-cf-act").textContent = String(o.action);
  const bd = wrap.querySelector(".qa-cf-bd");
  if(o.rows && o.rows.length){
    const src = document.createElement("pre");
    src.hidden = true;
    src.textContent = o.rawText != null ? String(o.rawText) : o.rows.map(r=>r.text).join("\n");
    bd.appendChild(src);
    const d = document.createElement("div");
    d.className = "qa-diff" + (o.showNo ? "" : " qa-diff-nono");
    d.innerHTML = qaDiffRowsHtml(o.rows, !!o.showNo);
    bd.appendChild(d);
  }else{
    const pre = document.createElement("pre");
    pre.textContent = String(code == null ? "" : code);
    bd.appendChild(pre);
  }
  return wrap;
}

/* ---------- ⑦ Diff 行（anywhere DiffPanel / buildDiffRows）----------
   四列栅格「符号 / 行号 / 竖分隔 / 正文」，五种行色：add 玉绿、del 朱砂、hunk 靛、file 灰、ctx 正文。
   `showNo=false` 时不画行号 —— 那是**推导出来的 diff**（见 qaDerivedDiffRows），
   行号无从得知，画上去就是编数据。 */
function qaDiffRowsHtml(rows, showNo){
  return (rows || []).map(r=>{
    const k = r.kind === "add" ? "add" : r.kind === "del" ? "del" : r.kind === "hunk" ? "hunk" : r.kind === "file" ? "file" : "ctx";
    const sign = k === "add" ? "+" : k === "del" ? "-" : "";
    const no = showNo && r.no != null ? String(r.no) : "";
    return '<div class="qa-diff-row ' + k + '">'
      + '<span class="s">' + sign + '</span>'
      + '<span class="n">' + escHtml(no) + '</span>'
      + '<span class="b"></span>'
      + '<span class="t">' + escHtml(r.text) + '</span>'
      + '</div>';
  }).join("");
}
/** 引擎给了**真的** unified diff 才走这里（tool_result 里带 @@ 与 ---/+++ 时）。 */
function qaLooksLikeDiff(text){
  const t = String(text == null ? "" : text);
  return /(^|\n)@@ /.test(t) && (/(^|\n)\+\+\+ /.test(t) || /(^|\n)--- /.test(t));
}
function qaDiffTextToRows(text){
  const rows = [];
  let no = null;
  String(text == null ? "" : text).split("\n").forEach(line=>{
    if(/^@@ /.test(line)){
      const m = /^@@\s+-\d+(?:,\d+)?\s+\+(\d+)/.exec(line);
      no = m ? Number(m[1]) : null;
      rows.push({ kind:"hunk", text:line });
      return;
    }
    if(/^(diff |index |--- |\+\+\+ )/.test(line)){ rows.push({ kind:"file", text:line }); return; }
    if(line.charAt(0) === "+"){ rows.push({ kind:"add", no:no, text:line.slice(1) }); if(no != null) no++; return; }
    if(line.charAt(0) === "-"){ rows.push({ kind:"del", no:no, text:line.slice(1) }); return; }
    rows.push({ kind:"ctx", no:no, text:line.charAt(0) === " " ? line.slice(1) : line });
    if(no != null && line !== "") no++;
  });
  return rows;
}
/** 由工具参数的 old/new 文本**推导** diff（Edit / MultiEdit / Write 走这条）。
 *  与真 diff 的区别：没有上下文行、也不知道行号 —— 所以只画「- 旧 / + 新」两组，
 *  不做 hunk、不画行号。宁可少画，也不编行号。 */
function qaDerivedDiffRows(oldText, newText){
  const rows = [];
  const push = (kind, txt)=>{
    const s = String(txt == null ? "" : txt);
    if(!s.length) return;
    s.split("\n").forEach(t=>rows.push({ kind:kind, text:t }));
  };
  push("del", oldText);
  push("add", newText);
  return rows;
}

/* ---------- ②③ 单行标记行 + JSON 兜底（anywhere Marker / JsonMarker / ArtifactCard）----------
   anywhere 里所有「不是正文、也不是完整工具卡」的条目都落在这一个形态上：
   system / marker / artifact / diagnostic / unknown →
     16px 图标 + 等宽单行标题 + 状态徽章（+ 展开一份 JSON）。
   这是整套阶梯里最重要的一格：**形态不认识 ≠ 内容丢掉**。
   本项目原来只有 `.qa-note`（一行 10px 灰字，见 03-chat-message 的 note），
   JSON 与状态全丢 —— 引擎抛个没见过的生命周期事件，画面上只剩「系统 · xxx」，
   排障要的字段其实都在事件里，只是没人画。

   ⚠️ 明细一律走 qaCodeFrameNode 的 JSON 面板：与工具明细、正文代码块同一个帧，
     观感不会分叉；且 JSON 是**可复制的**（原来那行灰字只能靠肉眼抄）。 */
function qaHasDetail(v){
  if(v == null) return false;
  if(typeof v === "string") return v.trim().length > 0;
  if(Array.isArray(v)) return v.length > 0;
  if(typeof v === "object") return Object.keys(v).length > 0;
  return true;
}
function qaMarkerNode(opts){
  const o = opts || {};
  const det = o.detail;
  const hasDet = qaHasDetail(det);
  const wrap = document.createElement("div");
  wrap.className = "qa-mk" + (o.destructive ? " err" : "");
  const row = document.createElement("div");
  row.className = "qa-mk-row";
  row.dataset.tog = hasDet ? "1" : "0";
  row.title = String(o.hint || "");
  row.innerHTML = (hasDet ? '<span class="qa-mk-chev">' + QA_V_ICONS.chev + '</span>'
                          : '<span class="qa-mk-gap"></span>')
    + '<span class="qa-mk-ico">' + (o.icon || (o.destructive ? QA_V_ICONS.alert : QA_V_ICONS.clock)) + '</span>'
    + '<span class="qa-mk-tt"></span>'
    + (o.status ? qaStatusBadgeHtml(o.status) : '');
  row.querySelector(".qa-mk-tt").textContent = String(o.title || "");
  wrap.appendChild(row);
  if(hasDet){
    const bd = document.createElement("div");
    bd.className = "qa-mk-bd";
    const isStr = typeof det === "string";
    bd.appendChild(qaCodeFrameNode(
      String(o.detailLabel || (isStr ? "detail" : "json")),
      isStr ? String(det) : JSON.stringify(det, null, 2)));
    wrap.appendChild(bd);
  }
  return wrap;
}

/* ---------- ④ 分隔条（anywhere Marker variant="separator"）----------
   用在「横切整条会话」的事件上 —— anywhere 那边是**会话压缩**（`MarkerContentKind="compact"`，
   进行中/已完成两种文案）。分隔条不是谁说的话，形状本身就在说「这里是一条分界」。
   ⚠️ 它**不承载明细**（没有可展开体），所以原始事件信息只能挂在 title 上（悬停可见）——
      这也是为什么「拿不准的 system 事件」不该走它，而该走标记行（有 JSON 兜底）。 */
function qaSepNode(text, opts){
  const o = opts || {};
  const d = document.createElement("div");
  d.className = "qa-sep";
  d.innerHTML = '<span class="qa-sep-t"></span>';
  d.querySelector(".qa-sep-t").textContent = String(text || "");
  if(o.title) d.title = String(o.title);
  return d;
}

/* ---------- ⑤ 工具归类与人话标题（anywhere timelineToolKind / timelineToolTitle）----------
   anywhere 不给用户看内部工具名与参数 JSON，而是按类别写一句人话：
     command →「运行了 <命令>」；file_change →「修改了 <文件>」；web_search →「搜索了 <词>」；
     agent_call →「<动作>：<描述>」；mcp →「<server> / <tool>」。
   本项目原来行里是「工具名 + 参数串前 60 字」—— 那是给排障的人看的，不是给用的人看的。
   ⚠️ 工具名与参数原文**不丢**：留在行 title（悬停可见）与明细段里。 */
const QA_TOOL_RULES = [
  { kind: "file_change", re: /^(edit|multiedit|multi_edit|write|notebookedit|notebook_edit|str_replace_editor|str_replace_based_edit_tool|apply_patch|create_file|delete_file)$/i },
  { kind: "agent_call", re: /^(agent|task|subagent)$/i },
  { kind: "web_search", re: /^(websearch|web_search|search_query)$/i },
  { kind: "web_fetch", re: /^(webfetch|web_fetch|fetch)$/i },
  { kind: "read", re: /^(read|notebookread|notebook_read|view|cat)$/i },
  { kind: "search", re: /^(grep|glob|search|find|ls|list_dir|listdir)$/i },
  { kind: "command", re: /^(bash|shell|zsh|sh|exec|execute|run|terminal|command|cmd|powershell)$/i },
];
function qaToolKindOf(name, argsObj){
  const n = String(name || "").trim();
  if(/^mcp__/i.test(n) || /^mcp[_.]/.test(n)) return "mcp";
  for(const r of QA_TOOL_RULES) if(r.re.test(n)) return r.kind;
  const o = argsObj && typeof argsObj === "object" ? argsObj : null;
  if(o){
    if(o.command != null || o.cmd != null) return "command";
    if(o.file_path != null || o.notebook_path != null) return "file_change";
    if(o.old_string != null || o.new_string != null || Array.isArray(o.edits)) return "file_change";
    if(o.query != null) return "web_search";
    if(o.url != null) return "web_fetch";
    if(o.pattern != null) return "search";
  }
  return "tool";
}

/* 形态合并（2026-09-29）：magic-agent 的归一化层会随 tool_use 事件给出 `tool_kind`
 *（command / file_change / web_search / mcp / agent_call / input_request / permission /
 * tool_call / unknown，词表对齐 agents-anywhere 的 ToolContentKind）。
 *
 * 但**不能**直接让服务端那份覆盖本地判定：本地这套按名字与参数分得更细
 *（read / search / web_fetch 是本地才有的更细形态），盲目覆盖会让本来认识的工具
 * 掉回通用卡（实测 Read 会从「文件」图标变回通用锤子）。
 *
 * 规则：**本地认出来就听本地的；本地只认得通用 `tool` 时，采用服务端给的形态**。
 * 这样既不丢精度，又能覆盖本地名字表没见过的引擎（那是这份字段真正的价值所在）。
 * ⚠️ 与 magic-test 仓库的同名函数逐字一致：两个界面共用一份词表，别各改各的。 */
function qaToolKindMerge(name, argsObj, serverKind){
  const local = qaToolKindOf(name, argsObj);
  if(local !== "tool") return local;
  const m = { command:"command", file_change:"file_change", web_search:"web_search",
              mcp:"mcp", agent_call:"agent_call",
              /* 服务端更粗的那几档在本地没有对应形态 → 落到通用卡（不硬造新形态） */
              tool_call:"tool", input_request:"tool", permission:"tool", unknown:"tool" };
  return m[String(serverKind || "")] || "tool";
}
function qaToolIconFor(kind){
  if(kind === "command") return QA_V_ICONS.terminal;
  if(kind === "file_change") return QA_V_ICONS.filepen;
  if(kind === "agent_call") return QA_V_ICONS.bot;
  if(kind === "web_search") return QA_V_ICONS.search;
  if(kind === "web_fetch") return QA_V_ICONS.search;
  if(kind === "mcp") return QA_V_ICONS.mcp;
  if(kind === "read" || kind === "search") return QA_V_ICONS.filepen;
  return QA_V_ICONS.hammer;
}
/** 参数里的「目标」：路径 / 查询词 / URL / 模式 —— 人话标题的宾语 */
function qaToolTargetOf(argsObj){
  const o = argsObj && typeof argsObj === "object" ? argsObj : null;
  if(!o) return "";
  return String(o.file_path || o.notebook_path || o.path || o.query || o.url
    || o.pattern || o.command || o.cmd || "").trim();
}
function qaClip(s, n){
  const t = String(s == null ? "" : s).replace(/\s+/g, " ").trim();
  const max = n || 72;
  return t.length > max ? t.slice(0, max) + "…" : t;
}
function qaBaseName(p){
  const t = String(p || "").replace(/\\/g, "/").replace(/\/+$/, "");
  const i = t.lastIndexOf("/");
  return i >= 0 ? t.slice(i + 1) || t : t;
}
/** 工具行标题（人话）+ 供 title 属性用的原文（排障时悬停可见） */
function qaToolHumanTitle(name, argsObj, argsText){
  const kind = qaToolKindOf(name, argsObj);
  const o = argsObj && typeof argsObj === "object" ? argsObj : null;
  const path = o ? String(o.file_path || o.notebook_path || o.path || "").trim() : "";
  if(kind === "file_change"){
    const many = o && Array.isArray(o.edits) ? o.edits.length : 0;
    if(path) return (qaFileChangeAction(name, o) === "add" ? "写入 " : "编辑 ") + qaClip(path, 64);
    return many > 1 ? "修改了 " + many + " 处" : "修改文件";
  }
  if(kind === "command")    return "运行了 " + (qaClip(o && (o.command || o.cmd), 80) || qaClip(argsText, 80) || "命令");
  if(kind === "web_search") return "搜索了 " + (qaClip(o && (o.query || o.q), 64) || "关键词");
  if(kind === "web_fetch")  return "抓取 " + (qaClip(o && (o.url || o.uri), 64) || "网页");
  if(kind === "read")       return "读取 " + (qaClip(path, 64) || "文件");
  if(kind === "search")     return "查找 " + (qaClip(o && (o.pattern || o.query), 48) || "匹配项")
                                   + (path ? " · " + qaClip(qaBaseName(path) || path, 32) : "");
  if(kind === "agent_call"){
    const d = qaClip(o && (o.description || o.subagent_type), 64);
    return d ? "委派：" + d : "委派子代理";
  }
  if(kind === "mcp"){
    const parts = String(name || "").split(/__|_/).filter(Boolean);
    return parts.length >= 2 ? parts[0] + " / " + parts.slice(1).join(" ") : String(name || "mcp");
  }
  const tgt = qaClip(qaToolTargetOf(o), 56);
  return tgt ? String(name || "tool") + " " + tgt : String(name || "tool");
}

/* ---------- ⑦ 文件变更的推导与渲染（anywhere FileChangeRow）----------
   magic-agent 的事件里没有 file_change 这一类，但**改动本身就在工具参数里**：
   Edit 带 old_string/new_string、MultiEdit 带 edits[]、Write 带 content。
   所以这一格是「从参数推导」而不是「等引擎给」—— 用户不必再读参数 JSON 才知道改了哪一行。
   ⚠️ 推导出的 diff 没有上下文行与行号（见 qaDerivedDiffRows），不画 hunk、不编行号。 */
function qaFileChangeAction(name, argsObj){
  const n = String(name || "").toLowerCase();
  const o = argsObj && typeof argsObj === "object" ? argsObj : {};
  if(/^(write|create_file)$/.test(n)) return "add";
  if(/delete_file/.test(n)) return "delete";
  if(o.old_string == null && o.new_string == null && !Array.isArray(o.edits) && o.content != null) return "add";
  return "modify";
}
function qaFileChangeOf(name, argsObj){
  const o = argsObj && typeof argsObj === "object" ? argsObj : null;
  if(!o) return null;
  const path = String(o.file_path || o.notebook_path || o.path || "").trim();
  if(!path) return null;
  const action = qaFileChangeAction(name, o);
  let rows = [];
  if(Array.isArray(o.edits) && o.edits.length){
    o.edits.forEach(e=>{ if(e) rows = rows.concat(qaDerivedDiffRows(e.old_string, e.new_string)); });
  }else if(o.old_string != null || o.new_string != null){
    rows = qaDerivedDiffRows(o.old_string, o.new_string);
  }else{
    rows = qaDerivedDiffRows("", o.content != null ? o.content : o.new_source);
  }
  if(!rows.length && !path) return null;
  return { action: action, path: path, rows: rows };
}
/** 文件变更行：动作徽章 + 路径 + Diff 面板（内部复用代码面板帧，与工具明细同一帧） */
function qaFileChangeNode(change){
  const wrap = document.createElement("div");
  wrap.className = "qa-fc";
  const hd = document.createElement("div");
  hd.className = "qa-fc-hd";
  hd.innerHTML = '<span class="qa-fc-ico">' + QA_V_ICONS.filepen + '</span>'
    + '<span class="qa-fc-tag"></span><span class="qa-fc-p"></span>';
  const tag = hd.querySelector(".qa-fc-tag");
  const act = change && change.action;
  tag.className = "qa-fc-tag " + (act === "add" ? "add" : act === "delete" ? "del" : "mod");
  tag.textContent = act === "add" ? "新建" : act === "delete" ? "删除" : "修改";
  hd.querySelector(".qa-fc-p").textContent = String((change && change.path) || "");
  hd.title = String((change && change.path) || "");
  wrap.appendChild(hd);
  if(change && change.rows && change.rows.length){
    wrap.appendChild(qaCodeFrameNode("diff", "", { rows: change.rows, showNo: !!change.showNo, flush: true }));
  }
  return wrap;
}

/* ---------- 长消息折叠（anywhere CollapsibleUserMessage）----------
 *
 * 这套机制本项目**早就有了**，但它只长在一条路径上：审核引擎的报告（12-req-board 的
 * qaReqEngineMsg 里内联写死）。而**用户自己的气泡没有** —— 偏偏用户在这个工作台里最常干的事
 * 就是把一整份需求正文粘进来，一条消息能把画布顶掉两屏。
 * anywhere 的 `CollapsibleUserMessage` 正是给**用户消息**用的：超过 10 行就截断 + 底部渐隐 +
 * 一枚展开按钮（内容一个字不删，只改可见高度）。
 *
 * 这里把机制收敛成**一处**：判据、按钮、类名都由 qaFoldMaybe 给，谁要折叠就调它一次。
 * ⚠️ 类名沿用既有的 `.qa-fold-btn` / `data-qa-fold`（引擎那份的 `.qa-engine-fold` 作为别名保留）：
 *     13-dispatch-stream 的**事件委托**与 tools/verify-review-ui.mjs 都按这套名字断言，
 *     改名会同时打断这两处。新调用方用 `.qa-fold`，CSS 里与 `.qa-engine-fold` 并列成一组。
 * ⚠️ 折叠态靠**类**表达（不靠 inline style）：快照恢复走 `innerHTML = snapshot`，
 *     类落在 HTML 上会跟着还原，高度不会「恢复后又弹开」。
 * ⚠️ 判据与审核报告同一口径（>500 字符 或 >12 行）—— 阈值只写这一处。
 *     anywhere 用的是 10 行；这里沿用本项目已有的数字，免得同一屏里两套折叠阈值。 */
const QA_FOLD_MAX_CHARS = 500;
const QA_FOLD_MAX_LINES = 12;
/** 给 host 按需挂折叠（返回是否挂了）。cls 默认 "qa-fold"；审核报告传 "qa-engine-fold"。 */
function qaFoldMaybe(host, text, cls){
  if(!host) return false;
  /* 带附件的消息**不折**（2026-09-28）：附件缩略图就在同一个 host 里
     （qaShotsMount 把 `.qa-atts` append 进 host），折起来会把「我特意要你看的图」
     一起折掉 —— 而折叠的目的是让**一长段文字**不把对话顶满。
     判据取**渲染后**的 DOM（`.qa-atts` 真在才跳过），不看入参。 */
  if(host.querySelector(".qa-atts")) return false;
  const body = String(text == null ? "" : text);
  const lines = body.split("\n").filter((x)=>x.trim()).length;
  if(!(body.length > QA_FOLD_MAX_CHARS || lines > QA_FOLD_MAX_LINES)) return false;
  host.classList.add(cls || "qa-fold");
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "qa-fold-btn";
  btn.setAttribute("data-qa-fold", "1");        // 事件委托认它（对话画布会被整份重建）
  btn.setAttribute("data-qa-lines", String(lines));
  btn.textContent = "展开全文（共 " + lines + " 行）";
  if(host.parentNode) host.parentNode.insertBefore(btn, host.nextSibling);
  return true;
}
