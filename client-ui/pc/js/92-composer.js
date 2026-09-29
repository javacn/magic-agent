/* 撰写区交互层：把「＋ / 引擎 / 模型 / 权限」四只菜单接上，并绑定发送主链。
 *
 *  ── 为什么不是直接搬 magic-test 的 bindHomeEntry ──
 *    上游那 1100 行把四只菜单、左下角工作目录切换器、需求派发拦截、键盘导航、
 *    Floating UI 定位全缠在一个闭包里，且依赖整个工作台。这一版只保「对话撰写区」这一层：
 *      · 标记 = 18-askbox.js 现画的（一字未改）
 *      · 菜单内容 = qaPermMenuBuild()（18）/ askAddMenuBuild()（19）现成的构造器，直接用
 *      · 弹出层 = 这里自己写的一份（position:fixed，贴着按钮翻上/翻下）
 *    上游的 Floating UI 精确定位、完整键盘导航、工作目录切换器 —— **未移植**（见增强点清单）。
 *
 *  ── 命名沿用上游 ──
 *    函数名（qaPickerSync / qaModelPickSync / qaEngHintSync / qaMenuPlace / qaCloseOne …）
 *    与上游一致，因为别的分片（05 的任务行尾菜单、07 的发送链）会按名字引用它们。
 */

/* ---------- 引擎行元素：弹出层与内容构造器要用的公共小件 ---------- */

/* 与上游 09-ask-event.js 一致的「支持流式」图标（只标正向，不支持什么都不画） */
const QA_ICON_STREAM = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M3 7h9M3 12h6M3 17h9"/><rect x="15.6" y="5" width="2.8" height="14" rx="1.4" fill="currentColor" stroke="none"/></svg>';
const qaStreamTag = (x)=>{
  if(!x || x.streaming !== true) return "";
  return `<span class="stm" data-streaming="true" role="img" aria-label="支持流式" title="支持 --stream：回答逐字流式返回">${QA_ICON_STREAM}</span>`;
};
/** 对每个已挂载的实例做一件事（实例清单唯一来源 = ASK_INSTANCES）。原样搬自上游。 */
const qaAskEach = (suffix, fn)=>{ ASK_INSTANCES.forEach(p=>{ const el = $("#" + p + suffix); if(el) fn(el, p); }); };

/* ---------- 弹出层（自研的极简版） ---------- */

let qaMenuBound = false;

/** 关掉一只菜单：隐藏 + 复位按钮的打开态。 */
function qaCloseOne(m){
  if(!m || m.hidden) return;
  m.hidden = true;
  m.classList.remove("dock-up","dock-down");
  const btn = m._qaBtn;
  if(btn){ btn.classList.remove("open"); btn.setAttribute("aria-expanded","false"); }
  m._qaBtn = null;
}
/** 关掉全部（除 except）。清单 = 登记在册的四只组件菜单。 */
function qaMenusClose(except){
  ASK_INSTANCES.forEach(p=>["-add-menu","-engine-menu","-model-menu","-perm-menu"].forEach(s=>{
    const m = $("#" + p + s);
    if(m && m !== except) qaCloseOne(m);
  }));
}
/** 定位：贴着触发按钮。下方放不下就翻到上方；左右夹持在视口内。
 *  ⚠️ 用 `position:fixed` + 视口坐标（不是 absolute + offsetParent）—— 宿主布局一变，
 *     absolute 的包含块就不再是 .home-entry，会整体错位。 */
function qaMenuPlace(btn, menu){
  if(!btn || !menu || menu.hidden) return;
  const r = btn.getBoundingClientRect();
  const w = menu.offsetWidth, h = menu.offsetHeight;
  menu.style.position = "fixed";
  menu.style.right = "auto"; menu.style.bottom = "auto";
  menu.style.left = Math.max(8, Math.min(Math.round(r.left), window.innerWidth - w - 8)) + "px";
  const below = Math.round(r.bottom) + 4;
  const up = (below + h > window.innerHeight - 8) && (r.top - h - 4 > 8);
  menu.style.top = Math.round(up ? r.top - h - 4 : below) + "px";
  menu.classList.toggle("dock-up", up);
  menu.classList.toggle("dock-down", !up);
}
/** 清单异步变长后重排（模型清单可能几十项）。 */
function qaMenuReplace(){ ASK_INSTANCES.forEach(p=>["-add-menu","-engine-menu","-model-menu","-perm-menu"].forEach(s=>{
  const m = $("#" + p + s);
  if(m && !m.hidden && m._qaBtn) qaMenuPlace(m._qaBtn, m);
})); }
function qaMenuToggle(btn, menu, p){
  if(!menu) return;
  const willOpen = menu.hidden;
  qaMenusClose(menu);
  if(!willOpen){ qaCloseOne(menu); return; }
  menu.dataset.qaInstance = p;
  menu._qaBtn = btn;
  menu.hidden = false;
  btn.classList.add("open");
  btn.setAttribute("aria-expanded","true");
  qaMenuPlace(btn, menu);
  /* 打开时把键盘高亮落到当前选中项 */
  const cur = menu.querySelector(".ag-model-opt.on") || menu.querySelector(".ag-model-opt");
  menu.querySelectorAll(".ag-model-opt").forEach(o=>o.classList.remove("kb"));
  if(cur){ cur.classList.add("kb"); cur.scrollIntoView({ block:"nearest" }); }
}
/* 供别的函数作用域复用（05 的任务行尾菜单走这两个钩子，与上游同款） */
window.__qaMenuClose = qaCloseOne;
window.__qaMenuPlace = qaMenuPlace;
window.__qaMenusClose = qaMenusClose;
window.__qaMenuReplace = qaMenuReplace;

/* ---------- 键盘导航（↑↓ 移动 / Enter 选中 / Esc 关闭） ---------- */
function qaKbMove(menu, dir){
  const opts = [...menu.querySelectorAll(".ag-model-opt")];
  if(!opts.length) return -1;
  let i = opts.findIndex(o=>o.classList.contains("kb"));
  opts.forEach(o=>o.classList.remove("kb"));
  i = i < 0 ? 0 : (i + dir + opts.length) % opts.length;
  opts[i].classList.add("kb");
  opts[i].scrollIntoView({ block:"nearest" });
  return i;
}

/* ---------- 菜单内容构造 ---------- */

/** 引擎菜单：清单来自 `magic-agent --engines`（经 desk.engines 桥）。原样搬自上游。 */
function qaEngMenuBuild(){
  const html = '<span class="ag-menu-cap">引擎</span>' + qaEngines.map(x=>
    `<button class="ag-model-opt${x.engine===qaEngine?" on":""}" data-engine="${escHtml(x.engine)}" type="button" role="option" aria-selected="${x.engine===qaEngine}" title="${x.ok ? escHtml(x.engine) : escHtml(x.engine) + "（本机 CLI 不可用）"}">
      <span class="eng-opt${x.ok?"":" off"}"><span class="st"></span><span class="nm">${escHtml(x.engine)}</span>${qaStreamTag(x)}</span>
    </button>`).join("");
  qaAskEach("-engine-menu", m=>{ m.innerHTML = html; });
  qaMenuReplace();
}
/** 模型菜单：首项「自动」（按配置链的链首），其后是该引擎探测到的模型。原样搬自上游。 */
function qaMdlMenuBuild(){
  const auto = qaModelAuto();
  const e = qaEngines.find(x=>x.engine === qaEngine) || null;
  const labels = (e && e.modelLabels) || {};
  const credits = (e && e.modelCredits) || {};
  const crOf = (id)=>{ const v = String(credits[id] || "").trim(); return v ? "x" + v : ""; };
  const ids = (e && Array.isArray(e.models) ? e.models : []).map(m=>String(m||"")).filter(Boolean);
  const autoName = auto ? qaModelName(auto, qaEngine) : "引擎默认";
  const autoCr = auto ? crOf(auto) : "";
  const opts = [{ id:"", name:"自动", note: autoCr ? autoName + " · " + autoCr : autoName }]
    .concat(ids.map(id=>{
      const bits = [];
      if(auto === id) bits.push("链首");
      const cr = crOf(id);
      if(cr) bits.push(cr);
      return { id, name: labels[id] || id, note: bits.join(" · ") };
    }));
  const html = '<span class="ag-menu-cap">模型</span>' + opts.map(m=>
    `<button class="ag-model-opt" data-model="${escHtml(m.id||"")}" type="button" role="option" aria-selected="false" title="${escHtml(m.id ? ("指定模型 · 下发给 -m " + m.id) : ("自动 · 按设置里「" + qaEngine + "」的模型链（链首 " + autoName + "）"))}">
      <span class="mdl-opt"><span class="nm">${escHtml(m.name)}</span>${m.note ? `<span class="dm">${escHtml(m.note)}</span>` : ""}</span>
    </button>`).join("");
  qaAskEach("-model-menu", m=>{ m.innerHTML = html; });
  qaModelPickSync();
  qaMenuReplace();
}

/* ---------- 三处同步出口（与上游同名同义） ---------- */

/** 某实例「当前选中了什么模型」（`""` = 自动）。首页看全局选择、任务对话看这条任务。 */
function qaModelPickOf(p){
  if(p === "home2") return String(qaModelPick || "");
  const t = Array.isArray(qaTasks) ? (qaTasks.find(x=>x.id === qaActiveId) || null) : null;
  return (t && t.model) ? String(t.model) : String(qaModelPick || "");
}
/** 模型下拉按钮文案 + 菜单选中态 —— **唯一**同步出口（两个实例各算一次）。原样搬自上游。 */
function qaModelPickSync(){
  const auto = qaModelAuto();
  qaAskEach("-model", (b, p)=>{
    const picked = qaModelPickOf(p);
    const show = (typeof qaModelFor === "function") ? qaModelFor(p, qaActiveId) : picked;
    const t = b.querySelector(".txt");
    if(t) t.textContent = show ? qaModelName(show, qaEngine) : "引擎默认";
    b.dataset.model = picked;
    b.title = picked
      ? `本任务用「${qaModelName(picked, qaEngine)}」（下发给 -m ${picked}）· 点开可改，或切回自动`
      : `自动 · 按设置里「${qaEngine}」的模型链（链首 ${auto ? qaModelName(auto, qaEngine) : "—"}）`;
  });
  qaAskEach("-model-menu", (m, p)=>{
    const picked = qaModelPickOf(p);
    m.querySelectorAll(".ag-model-opt").forEach(x=>{
      const on = (x.dataset.model||"") === picked;
      x.classList.toggle("on", on);
      x.setAttribute("aria-selected", String(on));
    });
  });
}
window.qaModelPickSync = qaModelPickSync;

/** 引擎按钮文案 + 菜单选中态 + 引擎提示行 + 模型下拉 + 权限胶囊 —— **唯一**同步出口。
 *  上游还在这里管「对话进行中禁用引擎」的 title，本版没做那层锁（见增强点）。 */
function qaPickerSync(){
  qaAskEach("-engine", b=>{ const t = b.querySelector(".txt"); if(t) t.textContent = qaEngine; });
  qaAskEach("-engine-menu", m=>{ m.querySelectorAll(".ag-model-opt").forEach(x=>{
    const on = x.dataset.engine === qaEngine;
    x.classList.toggle("on", on);
    x.setAttribute("aria-selected", String(on));
  }); });
  qaEngHintSync();
  qaModelPickSync();
  if(typeof qaPermSync === "function") qaPermSync();
}
window.qaPickerSync = qaPickerSync;

/* ---------- 引擎清单取数 ---------- */

/* 引擎清单取数。`qaEnginesReal` 声明在 91-workbench-stubs.js（04 的 qaPermSupported 要读它）。 */
/** 取引擎清单并重建两只菜单。返回 Promise<boolean>（true = 拿到真实清单）。 */
function qaEnginesFetch(fresh){
  return window.desk.engines(fresh ? { fresh:true } : undefined)
    .then(r=> (r && r.ok && Array.isArray(r.engines) && r.engines.length) ? r.engines : null)
    .catch(()=>null)
    .then(list=>{
      if(!list) return false;
      qaEngines = list;
      qaEnginesReal = true;
      if(!qaEngines.some(x=>x.engine === qaEngine)){        // 记住的引擎已不在清单 → 回首个可用
        qaEngine = qaEngines[0].engine;
        try{ localStorage.setItem("qaEngine", qaEngine); }catch(e){}
      }
      /* 引擎定下来就重取该引擎的模型链（「自动」档用的就是链首）并重建模型菜单。 */
      qaModelChainLoad(qaEngine, true);
      qaEngMenuBuild();
      qaMdlMenuBuild();
      /* ⚠️ 权限菜单也要重建：底部那行「该引擎不接受权限档位」是按**当前引擎**拼的
         （qaPermNoteHtml），只在 qaPermMenuBuild 里现填。清单是异步到的 —— 不重建的话，
         首帧（还拿着兜底表、不发这行）那次构建会一直留在 DOM 里，引擎换成不支持档位的
         那个也看不到说明。上游把清单更早拿到手，所以没这一处。 */
      qaPermMenuBuild();
      qaPickerSync();
      return true;
    });
}
window.__qaEnginesRefresh = ()=>qaEnginesFetch(true);

/* ---------- 交互绑定 ---------- */

/** 引擎被选中：写状态 + 落 localStorage + 重取链 + 全部重刷。 */
function qaEnginePick(name){
  if(!name || name === qaEngine) return;
  qaEngine = name;
  try{ localStorage.setItem("qaEngine", qaEngine); }catch(e){}
  qaModelChainLoad(qaEngine, true);
  qaEngMenuBuild();
  qaMdlMenuBuild();
  qaPermMenuBuild();          // 同上：说明行按当前引擎拼，换引擎必须重建
  qaPickerSync();
}

/** 撰写区总装：挂好四只菜单的开关与点选，绑发送主链。`askBoxMountAll` 之后调一次。 */
function qaComposerBind(){
  ASK_INSTANCES.forEach(p=>{
    const box = $("#" + p + "-box");
    if(!box) return;                              // 该实例没挂（如只有 #ask-home）
    const pairs = [
      ["-plus",   "-add-menu",    "add"],
      ["-access", "-perm-menu",   "perm"],
      ["-engine", "-engine-menu", "engine"],
      ["-model",  "-model-menu",  "model"],
    ];
    pairs.forEach(([bs, ms, kind])=>{
      const btn = $("#" + p + bs), menu = $("#" + p + ms);
      if(!btn || !menu) return;
      btn.addEventListener("click", (e)=>{ e.stopPropagation(); qaMenuToggle(btn, menu, p); });
      /* 菜单项点选：菜单内容会被 innerHTML 重建，所以委托在菜单自己身上。 */
      menu.addEventListener("click", (e)=>{
        const o = e.target.closest(".ag-model-opt"); if(!o) return;
        e.stopPropagation();
        if(kind === "add"){ askAddPick(p, o); return; }       // 19：挂/摘胶囊（内部自己关菜单）
        if(kind === "perm"){ qaPermSet(o.dataset.perm); qaCloseOne(menu); return; }
        if(kind === "engine"){ qaEnginePick(o.dataset.engine); qaCloseOne(menu); return; }
        /* 模型：写回唯一状态位（首页 = qaModelPick，任务对话 = 这条任务的 model） */
        qaModelSet(o.dataset.model, p, qaActiveId);
        qaModelPickSync();
        qaCloseOne(menu);
      });
    });
    /* 胶囊行上的 × —— 摘掉这枚胶囊 */
    const refs = $("#" + p + "-refs");
    if(refs) refs.addEventListener("click", (e)=>{
      const x = e.target.closest(".rx"); if(!x) return;
      e.stopPropagation();
      askChipDrop(p, x.dataset.kind);
    });
    /* 点菜单外 / Esc 关闭（composedPath：菜单项点选会重绘，closest 判不出「点在菜单外」） */
    if(!qaMenuBound){
      document.addEventListener("click", (e)=>{
        const path = e.composedPath();
        if(path.some(n=>n && n.classList && n.classList.contains("ag-menu"))) return;
        qaMenusClose(null);
      });
    }
  });
  if(!qaMenuBound){
    qaMenuBound = true;
    document.addEventListener("keydown", (e)=>{
      /* Esc：关菜单并回焦输入框 */
      if(e.key === "Escape"){ qaMenusClose(null); const i = $("#home-input"); if(i) i.focus(); return; }
      const open = [...document.querySelectorAll(".ag-menu")].find(m=>!m.hidden);
      if(!open) return;
      if(e.key === "ArrowDown" || e.key === "ArrowUp"){ e.preventDefault(); qaKbMove(open, e.key === "ArrowDown" ? 1 : -1); return; }
      if(e.key === "Enter"){
        const cur = open.querySelector(".ag-model-opt.kb");
        if(cur){ e.preventDefault(); cur.click(); }
      }
    });
    /* 滚动 / 缩放后重排（自研版没有 autoUpdate，退化成一次性重算） */
    window.addEventListener("resize", ()=>qaMenuReplace());
    window.addEventListener("scroll", ()=>qaMenuReplace(), true);
  }
  bindComposerSend();
}

/** 发送主链：与上游 bindHomeAsk 的「提交 / 回车 / 停止」逐条对齐。
 *  ⚠️ 上游还订阅 desk.onAskEvent 把事件喂给渲染管线 —— 那一层在 90-glue.js 的 boot 里挂。 */
function bindComposerSend(){
  const input = $("#home-input"), sendBtn = $("#home-send"), box = $("#home-box");
  if(!input || !sendBtn){ throw new Error("输入框标记没注入（#home-input / #home-send 找不到）"); }
  const submit = ()=>qaSend(input.value, { followup: true, from: "home" });
  sendBtn.addEventListener("click", async ()=>{
    /* 同一枚钮两态（形态由 qaSendBtnSync 按「输入框有没有内容」落类）：运行中 + 空输入 = 停止 */
    if(box && box.classList.contains("is-busy") && !input.value.trim()){ await qaStopActive(); return; }
    await submit();
  });
  input.addEventListener("keydown", (e)=>{
    if(qaImeComposing(e)) return;                 // 输入法合成中，那一下回车归输入法
    if(e.key === "Enter" && !e.shiftKey){ e.preventDefault(); submit(); }
  });
  input.addEventListener("input", ()=>qaSendBtnSync("home"));
  qaSendBtnSync("home");                          // 首帧也对一次
}

/* ---------- 流式事件 → 画面 ---------- */

/** 订阅一轮里推进来的事件，按 rid 路由到**它那一轮**的 run，再交给 qaStreamApply。
 *  这是「事件 → 画面」的唯一入口 —— 逐字搬自上游 bindHomeAsk 的 handle（只去掉工作台
 *  无关的越权分支）。上游主进程会把同一轮事件**批量合并**成 { rid, batch:[...] }，
 *  这里两种形态都收（我们的 desk 适配是逐条推，但别假设宿主一定不批量）。 */
function qaStreamBind(){
  if(qaUnsub || !(window.desk && typeof window.desk.onAskEvent === "function")) return;
  const handle = (ev)=>{
    const run = qaRuns.get(ev.rid); if(!run) return;    // 过期 / 未知会话事件丢弃
    const h = run.h; if(!h) return;
    /* 一轮刚开跑 → 让左栏尽快出现这条会话（新会话此时还没 session_id，收尾时会再刷一次） */
    if (ev.type === "start" && typeof qaRailRefreshSoon === "function") qaRailRefreshSoon();
    const visible = (run.taskId === qaActiveId) && !run.holder;   // 这一轮此刻在屏幕上吗
    qaPaintBg = !visible;
    try{
      qaStreamApply(ev, {
        h, tools: run.tools, agents: run.agents,
        rid: run.rid, taskId: run.taskId,
        setMs: (ms)=>{ run.ms = ms; },
        /* 消息台账：主会话正文逐段累加（收尾时落进快照用） */
        onText: (t)=>{ run.msgText = (run.msgText || "") + String(t || ""); },
        /* 过程步骤：思考块 / 工具卡攒着 */
        onStep: (st)=>{
          if(!st) return;
          if(st.type === "tool_result"){
            for(let i = run.logSteps.length - 1; i >= 0; i--){
              const s = run.logSteps[i];
              if(s.type === "tool" && String(s.tid) === String(st.tid)){
                s.result = String(st.text || ""); s.ok = st.ok !== false;
                return;
              }
            }
            run.logSteps.push({ type: "tool", name: "结果回执", args: "", result: String(st.text || ""), ok: st.ok !== false });
            return;
          }
          run.logSteps.push(st);
        },
        onSession: (sid)=>{ qaSessionBindFor(run.taskId, sid); },
        onError: (e)=>{
          /* 显示哪一句：**优先根因**（reason），退回完整错误链（error）——理由同上游 */
          qaRenderDone(false, "调用失败 · " + String(e.reason || e.error || "未知错误").slice(0, 80), qaRunHost(run));
          qaTaskMarkSidLostFor(run.taskId);
          qaSaveSnapshotFor(run.taskId, "error", { holder: run.holder });
          qaRuns.delete(run.rid);
          qaRefreshRunning();
          qaTaskRender();
          qaAskFlush(run.taskId);
          qaMsgFlush(run.taskId);
          /* 左栏跟着刷新：这一轮的会话（含刚拿到 session_id 的新会话）该出现在列表里了 */
          if (typeof qaRailRefreshSoon === "function") qaRailRefreshSoon();
        },
        onDone: (e)=>{
          const ms = run.ms;
          qaRenderDone(!e.stopped, e.stopped ? "已停止" : "已完成" + (ms != null ? ` · ${(ms/1000).toFixed(1)}s` : ""), qaRunHost(run));
          run.ms = null;
          qaTaskMarkSidLostFor(run.taskId);
          qaSaveSnapshotFor(run.taskId, "done", { holder: run.holder });
          qaRuns.delete(run.rid);
          qaRefreshRunning();
          qaTaskRender();
          qaAskFlush(run.taskId);
          qaMsgFlush(run.taskId);
          /* 左栏跟着刷新：这一轮的会话（含刚拿到 session_id 的新会话）该出现在列表里了 */
          if (typeof qaRailRefreshSoon === "function") qaRailRefreshSoon();
        },
      });
    } finally { qaPaintBg = false; }
    if(visible) qaScrollIntoView();
  };
  qaUnsub = window.desk.onAskEvent((p)=>{
    if(p && Array.isArray(p.batch)){ p.batch.forEach(handle); return; }
    handle(p);
  });
}

/** 启动总装（由 90-glue.js 的 boot 调用，在 askBoxMountAll 之后）。 */
function qaComposerInit(){
  qaPermMenuBuild();
  qaPermSync();
  qaEngMenuBuild();
  qaMdlMenuBuild();
  qaComposerBind();
  qaStreamBind();
  /* ⚠️ 画布上的**事件委托**必须挂（上游在 17-bind.js 的 bind() 里挂，本版没有那个文件）：
     工具卡 / 思考条的展开收起、提问卡的选项点选与「其他」确认全靠它 ——
     漏了它的表现是「卡片画出来了但点不动」，而且不报错（绑定函数自己只是加监听）。
     `bindNavTasks()` 是左栏那一套（本版没有左栏），刻意不调。 */
  if(typeof bindQaStreamDelegation === "function") bindQaStreamDelegation();
  /* 引擎清单异步取回后再重刷一次（拿不到就保持兜底清单，界面不开天窗） */
  qaEnginesFetch(false);
}
