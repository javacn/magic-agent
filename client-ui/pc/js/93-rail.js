/* 93-rail.js —— 左侧会话栏：**数据全部来自 magic-agent 自己的命令**（经宿主 desk 桥）。
 *
 *  ── 数据来自哪两条命令 ──
 *    `--sessions`     会话登记表：run_id / session_id / engine / model / state / workspace /
 *                     prompt_head / 起止时间 → 本文件的行
 *    `--session-log`  一条会话的**事件历史**：`{seq, at, kind, text, ...}`，
 *                     kind ∈ user / thinking / text / tool_use / tool_result / ask /
 *                     turn_end / error —— 与渲染层的扁平事件**同名词表**，所以点开一条会话
 *                     就是把历史事件喂进**与实时同一条**渲染管线（见 qaRailReplay）。
 *    操作面只有 CLI 真有据可依的那几个：新建对话 / 打开（读历史）/ 停止（`--stop`）/
 *    复制会话 id。「置顶 / 归档 / 删除」在 magic-agent 里没有对应概念 —— 本版**不做**，
 *    免得长出点了没反应的按钮（旧工作台那三样建在掌天瓶自己的 tasks.db 上）。
 *
 *  ── 为什么不用 05-task-list-render.js 那份左栏 ──
 *    那份是给旧工作台的**本地任务表**写的（置顶 / 归档 / 需求绑定 / 画布快照全在 SQLite）。
 *    这一版数据源换成了 CLI，硬套只会得到一半按钮是摆设。外观仍用 app.css 里那套 `.tk-*`
 *    类，所以看上去与其余界面同调。
 */

const QA_RAIL_MAX = 60;             // 一屏最多画多少行（CLI 的登记表可能有几百条）
const QA_RAIL_POLL_MS = 30000;      // 有会话在跑时的兜底轮询（其余时候靠「一轮结束」触发）

let qaRailAll = [];                 // 最近一次拉到的会话（宿主归一后的行）
let qaRailSkipped = 0;              // 被略掉的一次性调用条数（如实写在列表下方）
let qaRailNoLog = 0;                // 列出来的会话里，有多少条没有可回放的事件日志
let qaRailQuery = "";
let qaRailTab = "all";
let qaRailErr = "";
let qaRailLoaded = false;
let qaRailTimer = null;
let qaRailMenuEl = null;

/** 会话落到哪一档（左栏分组）—— 判据全部来自 CLI 给的 state / alive。
 *  ⚠️ 状态词与 magic-agent 的记录、agents-anywhere 的 TimelineStatus 对齐（2026-09-29）：
 *    running / waiting_approval / done / failed / cancelled / interrupted / gone。
 *    `waiting_approval` 必须算**进行中** —— 它没在跑，但也没结束；归到「已完成」的话，
 *    用户会看到「我还在等它回话，它却已完成」。 */
function qaRailGroup(s){
  if(!s) return "done";
  const st = String(s.state || "");
  if(s.alive || st === "running") return "live";
  if(st === "waiting_approval") return "live";
  if(st === "failed" || st === "gone") return "failed";
  return "done";
}
function qaRailGroupLabel(s){
  const st = String((s && s.state) || "");
  const g = qaRailGroup(s);
  if(g === "live") return st === "waiting_approval" ? "等审批" : "进行中";
  if(g === "failed") return "失败";
  if(st === "cancelled" || st === "stopped") return "已取消";
  if(st === "interrupted") return "已打断";
  return "已完成";
}
/** 时间列：今天只给 时:分，其余给 月/日（与旧左栏同一口径）。 */
function qaRailTime(ts){
  const n = Number(ts);
  if(!Number.isFinite(n) || !n) return "";
  const d = new Date(n), now = new Date();
  const hm = String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
  return d.toDateString() === now.toDateString() ? hm : (d.getMonth() + 1) + "/" + d.getDate();
}
function qaRailHit(s){
  if(qaRailTab !== "all" && qaRailGroup(s) !== qaRailTab) return false;
  const q = qaRailQuery.trim().toLowerCase();
  if(!q) return true;
  return [s.title, s.engine, s.model, s.workspace, s.sessionId, s.runId]
    .some((v) => String(v || "").toLowerCase().includes(q));
}

/* ---------- 取数 ---------- */

async function qaRailLoad(quiet){
  const d = window.desk;
  if(!d || typeof d.agentSessions !== "function"){
    qaRailErr = "这一版没有会话清单通道（需要宿主提供 desk.agentSessions）";
    qaRailLoaded = true; qaRailRender(); return;
  }
  try{
    const r = await d.agentSessions();
    if(!r || !r.ok){ qaRailErr = (r && r.error) || "会话清单读不到"; }
    else {
      const all = Array.isArray(r.sessions) ? r.sessions : [];
      /* ⚠️ 判据只有一条：**有没有 `session_id`**（有 = 一条真会话，能续接、能回放）。
         · 没有 `session_id` 的是一次性调用（归纳 / 判定 / 派发里的一轮），不是会话，不列；
         · 有 `session_id` 但 `hasLog=false` 的**照样列** —— 引擎当时是非流式跑的，没留下事件
           日志，可它仍然是一条 bot 会话（**审查轮就是这一类**：用户 2026-09-29「是审查也是
           bot，所以应该显示在对话 ui 的左边」）。早先我按 hasLog 把它们筛掉了，结果是审查
           在列表里整个消失。这类行会带「无历史」标记，点开也如实说清为什么没有。 */
      qaRailAll = all.filter((s) => String(s.sessionId || "").trim());
      qaRailSkipped = all.length - qaRailAll.length;
      qaRailNoLog = qaRailAll.filter((s) => !s.alive && !s.hasLog).length;
      qaRailErr = "";
    }
  }catch(e){ qaRailErr = String((e && e.message) || e); }
  qaRailLoaded = true;
  qaRailRender();
  qaRailSchedule();
  if(!quiet && qaRailErr) console.warn("[rail] " + qaRailErr);
}

/** 有会话在跑时轻度轮询；没在跑就不轮询（一次 CLI 调用不贵，但没必要空转）。 */
function qaRailSchedule(){
  const live = qaRailAll.some((s) => qaRailGroup(s) === "live");
  if(live && !qaRailTimer){ qaRailTimer = setInterval(() => qaRailLoad(true), QA_RAIL_POLL_MS); }
  if(!live && qaRailTimer){ clearInterval(qaRailTimer); qaRailTimer = null; }
}
/** 一轮结束后尽快刷新（由 92-composer 的收尾分支调）。 */
function qaRailRefreshSoon(){
  setTimeout(() => qaRailLoad(true), 400);
}

/* ---------- 渲染 ---------- */

function qaRailRender(){
  const box = $("#rail-list"), tabs = $("#rail-tabs"), note = $("#rail-note");
  if(!box) return;
  if(tabs){
    const counts = { all: qaRailAll.length, live: 0, done: 0, failed: 0 };
    qaRailAll.forEach((s) => { counts[qaRailGroup(s)] += 1; });
    const tab = (k, label) =>
      `<button class="tk-tab${qaRailTab === k ? " on" : ""}" data-rail-tab="${k}" type="button"` +
      ` data-n="${counts[k]}" data-testid="rail-tab-${k}"><span>${label}</span>` +
      `<span class="n">${counts[k]}</span></button>`;
    tabs.innerHTML = tab("all", "全部") + tab("live", "进行中") + tab("done", "已完成") + tab("failed", "失败");
  }
  const rows = qaRailAll.filter(qaRailHit).slice(0, QA_RAIL_MAX);
  if(!qaRailAll.length){
    box.innerHTML = `<div class="tk-empty" data-testid="rail-empty">${qaRailLoaded
      ? (qaRailErr ? escHtml(qaRailErr) : "还没有会话 —— 在右边直接提问就会开一条")
      : "正在读会话清单…"}</div>`;
  }else if(!rows.length){
    box.innerHTML = `<div class="tk-empty" data-testid="rail-empty">没有匹配的会话</div>`;
  }else{
    // ── 会话行 ──
    // ⚠️ 「无历史」标记：有会话 id 但没有事件日志（引擎当时非流式跑）—— 这类行照样列，
    //    但得**在点之前**告诉人一声，否则点开只看到一句说明会以为坏了。
    const noLog = (s) => !s.alive && !s.hasLog;
    box.innerHTML = rows.map((s) => {
      const g = qaRailGroup(s);
      const title = s.title || "(无标题会话)";
      const sub = [s.engine, s.model].filter(Boolean).join(" · ");
      const tip = [title, sub, s.workspace, s.sessionId || s.runId, s.updatedAt,
        noLog(s) ? "这条会话没有留下事件日志（引擎当时是非流式跑的），点开看不到历史" : ""]
        .filter(Boolean).join("\n");
      return `<button class="tk-item${s.id === qaActiveId ? " active" : ""}" data-sid="${escHtml(s.id)}" type="button" title="${escHtml(tip)}" data-testid="rail-item">
        <span class="tico"><i class="rail-dot ${g}" aria-hidden="true"></i></span>
        <span class="tt">${escHtml(title)}</span>
        ${noLog(s) ? `<span class="rail-nolog" title="没有可回放的历史">无历史</span>` : ""}
        <span class="tm">${escHtml(qaRailTime(s.ts))}</span>
        <span class="tk-dots" data-rail-menu="${escHtml(s.id)}" role="button" tabindex="-1" data-testid="rail-dots"
          title="更多">${'<svg viewBox="0 0 24 24"><circle cx="5" cy="12" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="19" cy="12" r="1.6"/></svg>'}</span>
      </button>`;
    }).join("");
  }
  if(note){
    const hit = qaRailAll.filter(qaRailHit).length;
    const bits = [];
    if(hit > QA_RAIL_MAX) bits.push(`仅显示最近 ${QA_RAIL_MAX} 条（共 ${hit} 条 · 可用搜索缩小范围）`);
    if(qaRailNoLog) bits.push(`标「无历史」的 ${qaRailNoLog} 条是引擎非流式跑的，没留下可回放的事件日志（会话本身还能继续）`);
    if(qaRailSkipped) bits.push(`另有 ${qaRailSkipped} 条一次性调用（没有会话 id）未列出`);
    note.textContent = bits.join(" · ");
  }
  const cnt = $("#rail-count");
  if(cnt) cnt.textContent = qaRailAll.length ? String(qaRailAll.filter((s) => qaRailGroup(s) === "live").length || "") : "";
}

/* ---------- 打开一条会话（读历史 + 回放） ---------- */

/** 把「正在跑的那一轮」的画面整份挪进它自己的 holder：节点不销毁 →
 *  后台增量继续写进这些节点，切回来直接挂回 #qa-stream（与 05 的切会话同一套做法）。 */
function qaRailStowRun(){
  const run = (typeof qaRunOfTask === "function") ? qaRunOfTask(qaActiveId) : null;
  if(!run) return;
  if(!run.holder) run.holder = document.createElement("div");
  const prev = $("#qa-stream");
  if(prev) while(prev.firstChild) run.holder.appendChild(prev.firstChild);
}

/** 让 qaSend 认得这条会话：它按 `qaTasks.find(qaActiveId)` 取 sessionId，
 *  取到了才会带上 `--session` 续接（否则引擎会新起一段上下文）。 */
function qaRailEnsureTask(s){
  if(!s || !s.id) return;
  let t = (Array.isArray(qaTasks) ? qaTasks : []).find((x) => x.id === s.id);
  if(!t){ t = { id: s.id, ts: s.ts || Date.now() }; qaTasks.unshift(t); }
  t.title = t.title || s.title || "新对话";
  t.engine = s.engine || t.engine || qaEngine;
  t.model = s.model || "";
  t.sessionId = s.sessionId || "";
  t.status = qaRailGroup(s) === "live" ? "running" : (s.state === "failed" ? "error" : "done");
}

/** 历史回放：kind 与渲染层事件**同名词表**，所以逐条喂进 qaStreamApply 即可。
 *  `user` 那条不是渲染层事件（它是 core 补记的用户提问），走 qaRenderMine 画成用户气泡。 */
function qaRailReplay(events, s){
  const list = Array.isArray(events) ? events : [];
  let h = null;
  for(const ev of list){
    const kind = String((ev && ev.kind) || "");
    if(!kind) continue;
    if(kind === "user"){ qaRenderMine(String(ev.text || ""), []); continue; }
    if(!h){ h = qaRenderAnswer(); if(!h) break; }
    try{
      qaStreamApply(Object.assign({}, ev, { type: kind }), {
        h, tools: new Map(), agents: new Map(), rid: 0, taskId: qaActiveId,
        /* 回放是**只读**的：台账 / 会话绑定 / 收尾动作一律不落笔（历史不该改写现在） */
        setMs(){}, onText(){}, onStep(){}, onSession(){}, onError(){}, onDone(){},
      });
    }catch(e){ console.warn("[rail] 回放事件失败 " + kind + "：", (e && e.message) || e); }
  }
  if(!h && !list.some((e) => String(e.kind || "") === "user")){
    qaRenderDone(true, "这条会话没有留下事件", $("#qa-stream"));
    return;
  }
  if(h){
    /* 收尾行按会话自己的状态给一句（历史是只读的，不显示「生成中」那种活状态） */
    const g = qaRailGroup(s);
    qaRenderDone(g !== "failed", g === "failed" ? "上次调用失败" : (g === "live" ? "这条会话当时还在跑" : "历史回放"), $("#qa-stream"));
  }
}

async function qaRailOpen(id){
  const sid = String(id || "").trim();
  if(!sid) return;
  const s = qaRailAll.find((x) => x.id === sid) || null;
  const d = window.desk;
  if(!d || typeof d.agentSessionLog !== "function"){ toast("这一版没有会话历史通道"); return; }
  const box = $("#qa-stream"); if(!box) return;

  qaRailStowRun();
  qaActiveId = sid;
  qaRailEnsureTask(s);
  if(typeof showView === "function") showView("chat");
  box.innerHTML = qaConvNoticeHtml("正在读取这条会话的历史…", "wait");
  box.hidden = false;
  qaSetChatMode(true);
  qaRailRender();

  let r = null;
  try{ r = await d.agentSessionLog(sid); }
  catch(e){ r = { ok: false, error: String((e && e.message) || e) }; }
  if(qaActiveId !== sid) return;                     // 用户已经切走 → 丢弃这次结果
  box.innerHTML = "";
  if(!r || !r.ok){
    box.innerHTML = qaConvNoticeHtml("读会话历史失败：" + ((r && r.error) || "未知原因") +
      "（老版 magic-agent 不认识 `--session-log`，设置里升级一下就好）", "lost");
    return;
  }
  /* 命令通了但一条事件都没有 —— 这**不是**「命令不认识」，别混成同一句话：
     引擎当时是**非流式**跑的，magic-agent 的事件日志只在流式调用时写。行上那个
     「无历史」标记说的就是这件事，这里把原因说得更细一点（会话本身还能继续追问）。 */
  if(!Array.isArray(r.events) || !r.events.length){
    box.innerHTML = qaConvNoticeHtml(
      "这条会话没有留下事件日志（引擎当时是非流式跑的），所以看不到历史。" +
      "会话本身还在 —— 直接在这里追问，引擎会接着原来的上下文继续。", "lost");
    return;
  }
  qaRailReplay(r.events, s);
  qaScrollIntoView();
}

/** 新建对话：把当前上下文清掉，下一句话就是**全新会话**（不带给引擎任何 --session）。 */
function qaRailNew(){
  const box = $("#qa-stream");
  qaRailStowRun();
  qaActiveId = null;
  if(box){ box.innerHTML = ""; box.hidden = true; }
  if(typeof qaSetChatMode === "function") qaSetChatMode(false);
  qaRailRender();
  const inp = $("#home-input"); if(inp) inp.focus();
}

/* ---------- 行尾菜单 ---------- */

function qaRailMenuClose(){
  if(qaRailMenuEl){ qaRailMenuEl.remove(); qaRailMenuEl = null; }
}
function qaRailMenuOpen(id, anchor){
  qaRailMenuClose();
  const s = qaRailAll.find((x) => x.id === id) || null;
  if(!s || !anchor) return;
  const live = qaRailGroup(s) === "live";
  const el = document.createElement("div");
  el.className = "rail-menu";
  el.innerHTML =
    (live ? `<button type="button" data-act="stop">停止这条会话</button>` : "") +
    `<button type="button" data-act="open">打开（读历史）</button>` +
    `<button type="button" data-act="copy">复制会话 id</button>`;
  document.body.appendChild(el);
  const r = anchor.getBoundingClientRect();
  el.style.left = Math.max(8, Math.min(r.left, window.innerWidth - el.offsetWidth - 8)) + "px";
  el.style.top = Math.round(r.bottom + 4) + "px";
  el.addEventListener("click", async (e) => {
    const b = e.target.closest("[data-act]"); if(!b) return;
    const act = b.dataset.act;
    qaRailMenuClose();
    if(act === "open") return qaRailOpen(id);
    if(act === "copy"){
      try{ await navigator.clipboard.writeText(s.sessionId || s.runId || id); toast("已复制会话 id"); }
      catch(err){ toast("复制失败：" + ((err && err.message) || err)); }
      return;
    }
    if(act === "stop") return qaRailStop(s);
  });
  qaRailMenuEl = el;
}

async function qaRailStop(s){
  const d = window.desk;
  if(!d || typeof d.agentStop !== "function"){ toast("这一版没有停止会话的通道"); return; }
  const id = s.sessionId || s.runId || s.id;
  try{
    const r = await d.agentStop(id);
    if(r && r.ok) toast("已发出停止：" + id.slice(0, 8));
    else toast("停止失败：" + ((r && r.error) || "未知原因"));
  }catch(e){ toast("停止失败：" + ((e && e.message) || e)); }
  qaRailRefreshSoon();
}

/* ---------- 装配 ---------- */

function qaRailInit(){
  const box = $("#rail-list");
  if(!box) return;                                   // 宿主没给左栏（例如窄屏）→ 不装
  const tabs = $("#rail-tabs");
  if(tabs) tabs.addEventListener("click", (e) => {
    const b = e.target.closest("[data-rail-tab]"); if(!b) return;
    qaRailTab = b.dataset.railTab || "all";
    qaRailRender();
  });
  box.addEventListener("click", (e) => {
    const dots = e.target.closest("[data-rail-menu]");
    if(dots){ e.stopPropagation(); qaRailMenuOpen(dots.dataset.railMenu, dots); return; }
    const row = e.target.closest("[data-sid]");
    if(row) qaRailOpen(row.dataset.sid);
  });
  const sInp = $("#rail-search-input");
  if(sInp){
    sInp.addEventListener("input", () => { qaRailQuery = sInp.value; qaRailRender(); });
    sInp.addEventListener("keydown", (e) => { if(e.key === "Escape"){ sInp.value = ""; qaRailQuery = ""; qaRailRender(); } });
  }
  const sClr = $("#rail-search-clear");
  if(sClr) sClr.addEventListener("click", () => {
    if(sInp) sInp.value = ""; qaRailQuery = "";
    const c = $("#rail-search-clear"); if(c) c.hidden = true;
    qaRailRender();
  });
  if(sInp) sInp.addEventListener("input", () => { const c = $("#rail-search-clear"); if(c) c.hidden = !sInp.value; });
  const nw = $("#rail-new");
  if(nw) nw.addEventListener("click", () => qaRailNew());
  const rf = $("#rail-refresh");
  if(rf) rf.addEventListener("click", () => qaRailLoad(false));
  document.addEventListener("click", (e) => {
    if(!qaRailMenuEl) return;
    if(e.composedPath().some((n) => n === qaRailMenuEl)) return;
    qaRailMenuClose();
  });
  document.addEventListener("keydown", (e) => { if(e.key === "Escape") qaRailMenuClose(); });
  window.addEventListener("resize", () => qaRailMenuClose());
  qaRailLoad(true);
  qaRailRender();
}
