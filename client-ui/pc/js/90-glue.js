/* 适配层：让**逐字复制过来的** magic-test 对话框代码在两种宿主下都能跑起来。
 *
 *  ── 只做两件事 ──
 *   ① 保证 `window.desk.*` 存在（上游代码只认这个接口）：
 *        · 宿主已经注入了 desk（掌天瓶 Electron 壳的 preload）→ **直接用宿主的**；
 *        · 没有宿主桥（浏览器直开 / 静态预览）→ 这里装一个同名的适配，把通道接到 transport：
 *            desk.ask(payload)         → transport.ask，事件按 rid 回推给 onAskEvent 订阅者
 *            desk.askStop(rid)         → transport.control({op:'interrupt'})
 *            desk.askAppend({id,text}) → transport.control({op:'answer', text})
 *            desk.onAskEvent(cb)       → 订阅（返回退订函数）
 *            desk.engines()            → transport.listEngines()，并把 CLI 的 snake_case 归一成
 *                                        上游约定的 {ok, engines:[{modelsNote, …}]}
 *   ② 挂载：askBoxMountAll（输入框）→ qaComposerInit（菜单 + 发送 + 流）。
 *
 *  ── 关于「补丁函数」──
 *    凡是**复制文件里已经带了实现**的，这里绝不重复声明（会直接撞成
 *    "Identifier has already been declared"）；只有真的没人定义的，才补最小实现，
 *    且一律「如实说」——不假装成功、不静默失败。它们就是后面找增强点的清单。
 *
 *  ⚠️ 维护提醒：往这儿加符号前，先确认复制文件里没有同名顶层声明。
 */

/* ---------- 宿主桥优先 ----------
 *  ⚠️ 这一段是**集成时唯一要认的规矩**：
 *    · 页面被**宿主**打开（掌天瓶 Electron 壳就是），宿主 preload 已经注入了 `window.desk`
 *      → 直接用它。宿主那份是 IPC 直连 CLI，链路更短，而且有几个能力只有宿主给得了：
 *      附件落盘 `desk.pasteImage`、按会话 id 追加 `desk.askAppend`、SQLite 任务库
 *      `desk.tasksList`。自己再包一层 transport 会把这些**顶掉**，症状是「附件贴不上、
 *      作答变成新起一轮」。
 *    · 没有宿主桥（浏览器直接开 / 静态预览）→ 才自建：把 desk 的四个通道接到 transport。
 *  两种情形下，下面的渲染层代码一个字都不用改（它只认 window.desk）。 */
const HOST_DESK = (window.desk && typeof window.desk.ask === "function") ? window.desk : null;

/* ---------- 传输层（仅无宿主桥时用） ---------- */
const MAGIC_TOKEN = window.MAGIC_TOKEN
  || new URLSearchParams(location.search).get("token") || "";
const MAGIC_BASE = (window.MAGIC_BASE || "").replace(/\/+$/, "");
const transport = HOST_DESK ? null : window.MagicTransportHttp({ base: MAGIC_BASE, token: MAGIC_TOKEN });

/* ---------- 引擎行归一 ----------
   上游 magic-test 的 engines.cjs 会把 CLI 输出规范化；组件这一版直接吃 CLI 的原始行，
   所以在这里对齐字段名（`models_note` → `modelsNote` 等），其余原样透传。 */
function normalizeEngineRow(r){
  const models = Array.isArray(r.models) ? r.models.map((m)=> (typeof m === "string" ? m : (m && m.id) || "" )).filter(Boolean) : [];
  const labels = {};
  (Array.isArray(r.models) ? r.models : []).forEach((m)=>{
    if(m && typeof m === "object" && m.id) labels[m.id] = m.name || m.id;
  });
  return {
    engine: r.engine,
    ok: r.ok !== false,
    bin: r.bin || "",
    note: r.note || "",
    models: models,
    modelLabels: Object.assign(labels, r.modelLabels || {}),
    modelCredits: r.model_credits || r.modelCredits || {},
    modelsNote: r.models_note || r.modelsNote || "",
    workspace: r.workspace || "",
    streaming: r.streaming,
    attachments: r.attachments || "",
    permission: r.permission || "",
    capabilities: r.capabilities || [],
  };
}

/* ---------- desk 适配（只在没有宿主桥时安装） ---------- */
(function installDeskAdapter() {
  if (HOST_DESK) return;      // 宿主已经给了 desk，别覆盖它
  const subs = [];
  let ridSeq = 0;
  /* ⚠️ 事件**延后一个微任务**再推给订阅者 —— 这不是洁癖，是修一个真实的竞态：
     SSE 常把 `open` 与紧随其后的若干事件**放在同一个网络块**里回来（一次 write 合并成一段），
     transport 会在一个 forEach 里把它们连着派给 onEvent。若在此**同步**转发，
     渲染层此时还在 `await desk.ask(...)` 里没恢复（run 尚未按 rid 登记），
     `qaRuns.get(ev.rid)` 一律取不到 → **整轮事件被当过期事件丢光**：画面只剩用户气泡。
     推到微任务队列后，onOpen 触发的那个 Promise 续体（它才会 `qaRuns.set(rid, run)`）
     排在前面先跑，事件随后必被认领。顺序仍严格保持（逐个入队，FIFO）。 */
  const emit = (rid, ev) => {
    Promise.resolve().then(() => {
      /* ⚠️ 订阅者抛错**要吭声**：静默 catch 会让「整轮画面不动」变成没有任何线索的哑故障
         （事件明明推到了、渲染层却什么都没画 —— 排查时最费时间的一类）。 */
      subs.forEach((fn) => {
        try { fn(Object.assign({ rid: rid }, ev)); }
        catch (e) { console.error("[desk] 事件订阅者处理失败", ev.type, e); }
      });
    });
  };

  window.desk = {
    /* 起一轮。上游约定：**立即**回 {ok, rid}（渲染层据此登记 run），事件随后经 onAskEvent 推进来。
       所以这里等到 transport 的 onOpen（或失败）才 resolve —— 但**不阻塞**事件回推。 */
    ask(payload) {
      /* ⚠️ rid 必须是**数字**，不能图省事用 "r1" 这种字符串：上游代码拿它做 `Number.isFinite(rid)`
         判断（提问卡的答案要走 `--append` 追加回同一条会话，见 06-ask-answer.js 的 qaAskSend），
         字符串会判 false → 答案被降级成「排队，等这一轮结束再发」，而不是当场追加。
         `0` 是上游的「占位 rid（尚未到手）」，所以从 1 起编号。 */
      const rid = ++ridSeq;
      return new Promise((resolve) => {
        let settled = false;
        const done = (r) => { if (!settled) { settled = true; resolve(r); } };
        try {
          transport.ask(payload, {
            onOpen() { done({ ok: true, rid: rid }); },
            onEvent(ev) { emit(rid, ev); },
            onEnd(code) { emit(rid, { type: "end", code: code }); done({ ok: true, rid: rid }); },
            onError(e) {
              const msg = String((e && e.message) || e);
              emit(rid, { type: "error", reason: msg });
              done({ ok: false, error: msg });
            },
          });
        } catch (e) {
          const msg = String((e && e.message) || e);
          emit(rid, { type: "error", reason: msg });
          emit(rid, { type: "end", code: 1 });
          done({ ok: false, error: msg });
        }
        /* 兜底：有些 transport 不回调 onOpen（一轮没有 open 帧）。给个上限，
           别让 qaSend 卡在 `await desk.ask` 上 —— 事件照旧按 rid 路由。 */
        setTimeout(() => done({ ok: true, rid: rid }), 2000);
      });
    },
    askStop(rid) { return transport.control({ id: rid, op: "interrupt" }); },
    /* append = 往这条还活着的会话追加一条用户消息（引擎把它当下一轮）。
       在 core 那边就是 --control 的 answer。
       ⚠️ 必须回 `{ok}` 形状：上游 qaSend 拿的是 `ar.ok`（见 07-chat-stream-render.js 的
       `if(ar && ar.ok)`）。而 transport 的 control 只回一个裸 true —— 不归一的话，
       提问卡的答案会被判成「追加未送达」，白白回落成「新起一轮 + --session」，
       多一次冷启动（答案还是送得到，只是慢且费）。 */
    askAppend(p) {
      return Promise.resolve(transport.control({ id: p.id, op: "answer", text: p.text }))
        .then(() => ({ ok: true }))
        .catch((e) => ({ ok: false, error: String((e && e.message) || e) }));
    },
    onAskEvent(cb) { subs.push(cb); return () => { const i = subs.indexOf(cb); if (i >= 0) subs.splice(i, 1); }; },

    /* 引擎清单：transport 给的是 CLI 原始行（可能带 snake_case）→ 归一成上游形状。 */
    engines() {
      return Promise.resolve()
        .then(() => transport.listEngines())
        .then((r) => {
          const list = Array.isArray(r) ? r : (r && Array.isArray(r.engines) ? r.engines : null);
          if (!list || !list.length) return { ok: false, engines: [] };
          return { ok: true, engines: list.map(normalizeEngineRow) };
        })
        .catch(() => ({ ok: false, engines: [] }));
    },

    /* 附件落盘：上游是 Electron 的 pasteImage（把图写进磁盘并回路径）。
       这里没有本地壳，**不假装成功** —— 明说这一版还不支持粘贴落盘。 */
    pasteImage() {
      return Promise.reject(new Error("这一版（浏览器宿主）还不支持粘贴截图落盘：需要宿主提供 pasteImage 通道"));
    },
    /* 任务本地库（SQLite）：magic-agent 没有本地库 —— 空清单 + 写入如实回「跳过」。
       04-task-list.js 的 qaDb* 会把这些结果当「桥不可用」，于是只走 localStorage。 */
    tasksList() { return Promise.resolve({ ok: true, tasks: [] }); },
    tasksCreate(t) { return Promise.resolve({ ok: true, task: t }); },
    tasksUpdate() { return Promise.resolve({ ok: true }); },
    tasksDelete() { return Promise.resolve({ ok: true }); },
    convList() { return Promise.resolve({ ok: true, messages: [] }); },
    convAppend() { return Promise.resolve({ ok: true }); },
    settingsGet() { return Promise.resolve({ ok: true, config: {} }); },
    /* 模型链（配置界面那份「主选 + 兜底」的有序链）：这一版没有配置界面 → 空链。
       ⚠️ 必须给这个通道：04-task-list.js 的 qaModelChainLoad 在没有它时会去 fetch
       `/api/settings/model-chain`（那是 magic-test 内置后端的路），本版没有那个端点 →
       每次都留一条 404 噪音。空链的语义是「用引擎自带默认」，与 04 的注释一致，不编数据。 */
    settingsModelChain() { return Promise.resolve({ ok: true, chain: [] }); },
  };
})();

/* ---------- 工作台侧符号的最小实现 ----------
   只补「复制文件里确实没有」的那些。真被走到时如实说，不静默。 */
function notHere(what) {
  return function () {
    toast(what + "：这一版（magic-agent 基础对话 UI）还没接上");
    console.warn("[conv] 未实现：" + what);
  };
}
/* 视图切换：这里只有对话视图，所以是恒等操作（上游发送时会调它跳转到 chat） */
function showView(v) { state.view = v || "chat"; }
function createTask() { return { id: "conv-" + Date.now() }; }
function openPanel() { /* 上游：打开流水线督造面板；这一版没有 */ }
function syncWebviewViewport() { /* 上游：内嵌浏览器等比缩放；这一版没有 */ }
function isDockOpen() { return false; }
function openBrowserDock() { notHere("浏览器侧栏")(); }
function wsDirOf() { return ""; }
function genCasesFromDoc() { notHere("生成测试用例")(); }
function reqFeatPaneHint() { /* 上游：右栏文档阅读器的提示；无右栏 */ }
function reqShotsPaneHint() { /* 同上 */ }
function bindAsk() { /* 上游：提问卡选项的事件绑定；askCard 内部已自带 */ }
function agentCall() { notHere("模拟测试")(); }
function notify() { /* 上游：系统通知；无壳，忽略 */ }

/* ---------- 挂载 ---------- */
(function boot() {
  try {
    /* 输入框：上游的挂载入口，注入标记到 #ask-home */
    if (typeof askBoxMountAll === "function") askBoxMountAll();
    else throw new Error("askBoxMountAll 不存在（19 是否加载？）");
    /* 菜单 + 发送 + 流：撰写区总装（92-composer.js） */
    if (typeof qaComposerInit === "function") qaComposerInit();
    else throw new Error("qaComposerInit 不存在（92 是否加载？）");
    /* 左栏会话清单（93-rail.js）：数据来自 magic-agent 的 --sessions / --session-log */
    if (typeof qaRailInit === "function") qaRailInit();
  } catch (e) {
    document.body.insertAdjacentHTML("beforeend",
      '<div style="position:fixed;left:16px;bottom:16px;padding:12px 14px;border:1px solid #F4C7C7;'
      + 'background:#FFF4F4;color:#B42318;border-radius:10px;font:13px/1.5 system-ui">'
      + "对话界面挂载失败：" + escHtml((e && e.message) || e) + "</div>");
    throw e;
  }
})();
