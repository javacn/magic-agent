/* ---------- 任务 · 对话列表（左栏）：1 任务 = 1 次首页流式对话 ----------
   每次发送自动登记（标题 = prompt 首行截断）；点击切换 = 保存当前快照 →
   恢复目标快照到 #qa-stream。流式进行中禁止切换（流是增量写 DOM，切走会写进别的快照）。

   【2026-09-20 口径修正】左栏的**条数与计数 = 库内全量**（`tasks.db` 里该工作目录的全部
   会话），`QA_TK_MAX` 只约束「最多几条会话**带快照**」—— 那是内存 / localStorage 的容量
   保护，不该顺带决定「用户能看见几条」。
   旧行为（条目跟着一起淘汰）错在哪：用户 2026-09-20 报「任务列表跟需求个数为什么不一致 /
   已完成对话的跟已完成的需求就对不上」—— 实测左栏「已完成」只有 18 条（内存里就 20 条），
   而需求看板「已完成」48 条、`tasks.db` 里该目录已完成的会话 54 条：
   差的那些不是「没有」，是被 20 条上限**静默**挤掉了，界面上一点痕迹都没有。 */
const QA_TK_ICON = '<svg viewBox="0 0 24 24"><path d="M21 12a8 8 0 01-8 8H5l-2 2V12a8 8 0 018-8h2a8 8 0 018 8z"/></svg>';
/* 最多几条会话**带快照**（内存 / localStorage 容量上限）—— **不是**左栏可见条数，见文件头。 */
const QA_TK_MAX = 20;
/* ⚠️ qaSidClean 必须定义在下面的 localStorage 读入之前：
   读入代码会调用它，而后置 const 在同步执行时处于 TDZ → ReferenceError
   被 catch 静默吞掉 → qaTasks 永远空 → 快照从未恢复（「点击任务不显示历史」的根因之一，
   2026-09-16 排查确认；壳内能看见任务清单纯靠 SQLite 兜底，快照一直是空的）。 */
const qaSidClean = (s)=>{ s = String(s||""); return /^qa-\d+$/.test(s) ? "" : s; };
let qaTasks = [], qaActiveId = null;
try { qaTasks = JSON.parse(localStorage.getItem("qaTasks") || "[]").map(t=>({
  engine: "claude", model: "", sessionId: qaSidClean(t.sessionId || ""), status: "done", ...t })); } catch(e) { qaTasks = []; }
/* 僵尸 running 归正 + **补记 sidLost**（2026-09-18 用户：「任务要记录会话ID 重启的时候
   不然就丢失了」）：上次关窗/重启时那一轮被打断，result 行里的 session_id 永远拿不到；
   若这条本来也没有 id（首跑被打断），必须记一笔 sidLost → 下次追问下发 `-c` 续接该目录
   最近一次会话，否则用户看到的就是「重启之后这条会话失忆了」。
   （有 id 的那条不受影响：id 已由主进程 persistSid / qaSessionBindFor 落库，重启照旧续接。）
   ⚠️ 归正成 `error` 而不是 `done`（2026-09-20 改）：左栏自 2026-09-20 起按状态分三档
   （处理中 / 审查中 / 已完成，见 05-task-list-render.js），「被打断」**不是**「已完成」——
   归成 done 会让每次重启都把中断的会话塞进「已完成」档。`error` 与主进程那条
   `tasks-db::sweepRunning`（库里同样写 'error'）口径一致，红点 + 「上次调用失败」也说得出
   发生了什么；它留在「处理中」档，用户续一句就能接着跑。
   ⚠️ 同一趟里还要把老值 `queued` 归一成 `pending`（2026-09-20 起会话状态与**需求状态逐字一致**，
   见 12-req-board.js 的 qaTaskStatusOf；`queued` 是那之前把 pending 一律记成 queued 留下的存量值）。 */
qaTasks.forEach(t=>{
  if(t.status === "running"){ t.status = "error"; if(!t.sessionId) t.sidLost = 1; }
  else if(t.status === "queued") t.status = "pending";
});

/* ---------- 引擎 / 模型选择（输入框右下角）----------
   引擎清单动态来自 `magic-agent --engines`，两条取数路径：
     ① 桌面壳内 → window.desk.engines()（IPC，主进程 engines.cjs）
     ② 浏览器预览态 → GET /api/engines（内置后端，同一份 engines.cjs）
   两者都不可用时才回车内置兜底清单（QA_ENGINES_FALLBACK，仅为「不开天窗」）。
   ⚠️ 兜底清单是兜底，不是数据源：界面上必须能反映本机真实装了哪些引擎。

   ⚠️ 2026-09-21（用户：「对话窗口不选择模型了 直接使用配置界面的模型列表自动处理」）：
   对话框里**只剩引擎一枚选择器**，模型选择器整体下线（标记与菜单都删了）——
   模型唯一来自**配置界面**里该引擎的模型链（见下面「模型：只由配置界面决定」一段）。
   选中的引擎记入 localStorage（`qaEngine`）；模型**不再记**（它不再是用户选择，
   记一份就必然与配置漂移 —— 存量 `qaModel` 键在启动时清掉，见下）。 */
const QA_ENGINES_FALLBACK = [
  { engine:"claude",    ok:true, defaultModel:"",    models:[], modelsNote:"", workspace:"" },
  { engine:"codebuddy", ok:true, defaultModel:"hy3", models:[], modelsNote:"", workspace:"" },
  { engine:"trae",      ok:true, defaultModel:"",    models:[], modelsNote:"", workspace:"" },
  { engine:"llm",       ok:true, defaultModel:"",    models:[], modelsNote:"", workspace:"" },
];
let qaEngines = QA_ENGINES_FALLBACK.slice();
let qaEngine = "claude";
/* ── 模型：两个值，各有明确归属（2026-09-21 用户第四次定稿：
   「还是都可以选择模型 但是自动切换模型的时候 对话框下拉的选中模型显示也要变」）──────
     · `qaModelPick` —— **工作台首页**（home2，新建任务入口）那枚选择器选的值；
       `""` = 自动（按配置界面里该引擎模型链的链首）。记 `localStorage["qaModelPick"]`。
       它的含义是「**新建任务默认用哪个**」。
     · `qaModel`     —— 当前**任务对话**那条上下文会下发的模型 = 该任务自己的模型
       （`t.model`；新建时定下、之后可在任务对话里改、被自动切换时会被主进程改掉）。
       它同时是显示值：生成中胶囊 / ＋菜单提示都读它。
   ⚠️ 发送时**不直接读这两个值**，读 `qaModelFor(from, taskId)` —— 唯一算法，见该函数注释。
   ⚠️ 链的读取仍只有一处（qaModelChainLoad）：链首就是「自动」时用的那个。 */
let qaModelPick = "";
let qaModel = "";
/* ── 四档权限模型（用户 2026-09-18：「这个地方要选择」）──────────────────────
   对话框左槽原先是一枚**二值**胶囊「允许完全访问」（点一下在「完全访问 / 受限访问」之间
   切换）—— 用户要的是**可选的四档**。四档的定义与 magic-agent 的 `--permission` 逐字对应
   （magic-agent 的 permission.go 是唯一权威）：
     manual        沙箱开启，只读放行，其余逐项由用户确认
     accept-edits  沙箱开启，工作区内编辑放行，命令仍逐条确认
     auto          沙箱开启，沙箱内放行，越界交内置 LLM Guardian 判定
     full          沙箱关闭，命令直接在宿主机执行，无审批
   ⚠️ id 必须与 CLI 取值逐字一致（`manual` / `accept-edits` / `auto` / `full`）：
      这里拼错一个字母，CLI 侧会 exit 2「invalid --permission」，整轮对话发不出去。
   ⚠️ 默认档 = **auto（自动审批）**（用户 2026-09-18：「默认模式是自动审批」）。
      这是一次**有意的默认行为变更**：改造前那枚二值胶囊默认在「允许完全访问」，
      等于默认跑在无沙箱无审批的第 4 档；现在默认落到第 3 档（沙箱内放行 + 越界交 AI 审核）。
      magic-agent 的 CLI 自身默认仍是 full，所以**必须显式下发** —— 见下方 QA_PERM_DEFAULT 注释。 */
const QA_PERMS = [
  { id:"manual", label:"手动审批",
    desc:"改动和命令都由你确认",
    detail:"沙箱开启，只读放行，改动与命令逐项由你确认",
    icon:'<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M5 3l13.6 7.9-5.9 1.6-2.5 5.7z"/></svg>' },
  { id:"accept-edits", label:"自动接受编辑",
    desc:"改文件不用问，跑命令才问你",
    detail:"沙箱开启，工作区内编辑自动放行，命令仍逐条确认",
    icon:'<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M4 20h4L19 9a2.1 2.1 0 0 0-3-3L5 17v3z"/></svg>' },
  { id:"auto", label:"自动审批",
    desc:"由 AI 审核，拿不准才问你",
    detail:"沙箱开启，沙箱内自动放行，越界请求交内置 AI 审核（原 LLM Guardian）",
    icon:'<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3l7.5 2.8v5.7c0 4.3-3 7.9-7.5 9.2-4.5-1.3-7.5-4.9-7.5-9.2V5.8z"/><path d="M9 12.2l2.2 2.2 4-4.4"/></svg>' },
  { id:"full", label:"完全访问",
    desc:"不经审批，直接在本机运行",
    detail:"沙箱关闭，命令直接在本机执行，所有检查与审批都已关闭",
    icon:'<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3.8l8.7 15.2H3.3z"/><path d="M12 9.6v4.2M12 16.6h.01"/></svg>' },
];
/** 菜单里那一枚「✓」（只在当前档位显示，与参考稿一致）。 */
const QA_PERM_CHECK = '<svg class="ck" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12.8l4.4 4.4L19 7"/></svg>';
const QA_PERM_IDS = QA_PERMS.map(x=>x.id);
/** 默认档（用户 2026-09-18：「默认模式是自动审批」）。
 *  ⚠️ magic-agent 的 CLI 自身默认是 **full**，与本默认档不同 —— 所以「不传就是 auto」这件事
 *     **不成立**，必须把这个值显式拼进 argv（`--permission auto`）。调用链上三处都要兜：
 *     `desk:ask`（渲染层总是带档位，兜旧渲染层 / 预览态）、`reqboard.startRun`（派发）、
 *     `agent-cli.buildAgentArgs` 的 DEFAULT_PERMISSION 常量。少兜一处就退回无沙箱的第 4 档。 */
const QA_PERM_DEFAULT = "auto";
/** 唯一「不隔离、不询问」的档 → **按钮侧**给它朱砂高亮，其余三档灰字。
 *  ⚠️ 别和 QA_PERM_DEFAULT 混用：高亮标的是「危险档」，不是「当前默认档」——
 *     默认档改成 auto 之后若还拿 QA_PERM_DEFAULT 判高亮，「完全访问」就不红了，
 *     安全信号正好反了。菜单侧则由 CSS 的 `[data-perm="full"]` 常亮。 */
const QA_PERM_ALERT = "full";
let qaPerm = QA_PERM_DEFAULT;
/** 按 id 取档位定义；未知 id 一律回退默认档（不抛、不静默发一个空档位给 CLI）。 */
function qaPermOf(id){ return QA_PERMS.find(x=>x.id === id) || QA_PERMS.find(x=>x.id === QA_PERM_DEFAULT); }
/* 模型 id → 可读模型名：**只用于显示**（模型按钮 / 对话「生成中」行 / 需求看板卡片 / 提示行 / toast）。
   查法 = 按 id 在该引擎的 modelLabels 里找（llm 配置文件里的 name，或 llm 配置的 model_name）；
   当前引擎查不到时扫全部引擎的标签（看板里的需求可能属于别的引擎），再查不到就原样返回 id。
   ⚠️ 透传给 CLI 的 `-m`、任务 / 需求落库、派发参数一律仍用 id —— 名字只活在展示层。 */
function qaModelName(id, engine){
  const key = String(id || "");
  if(!key) return "";
  const hit = (e)=> (e && e.modelLabels && e.modelLabels[key]) || "";
  return hit(qaEngines.find(x=>x.engine === (engine || qaEngine)))
      || qaEngines.map(hit).find(Boolean)
      || key;
}
/* ---------- 模型：链来自配置界面，选不选**两个实例都可以** ----------
   用户 2026-09-21 四次改口，**这一版是定稿**（前面几版都写在这里，免得下次又来回改）：
     ① 「对话窗口不选择模型了 直接使用配置界面的模型列表自动处理」→ 删掉模型选择器；
     ② 「不能选择 但是要显示用的是哪个」→ 补只读标签；
     ③ 「工作台的对话框中可以选模型 在任务ui中只是显示」→ 首页可选、任务对话只读；
     ④ 「还是都可以选择模型 但是自动切换模型的时候 对话框下拉的选中模型显示也要变」
        → **两个实例都恢复成可选下拉**，且自动切换后下拉的选中项/文案必须跟着变。
   所以现在：模型选择**允许**（两个实例都能选），但「这次到底下发哪个」仍然只有一份算法
   （下面的 `qaModelFor`）—— 界面只负责把选择写回去、把实际值画出来。
   配置界面里那条**有序降级链**依然生效：链首是「自动」档用的主选，其后是兜底候选，
   执行侧按顺序自动试（见 desktop/chat-model.cjs + main.js::desk:ask）。
   为什么要这样切：选择可以有多个入口，但**算法只能有一份**。界面再算一遍「该试哪个」，
   就必然出现「界面说 A、主进程先试 B」的漂移。 */
let qaModelChain = [];        // 当前引擎的模型链（配置界面那份，有序；[] = 引擎自带默认）
let qaModelChainEng = "";     // 上面这份链属于哪个引擎（换引擎必须重取，否则拿的是别人的链）
/** 「自动」用的模型 = 配置链的链首（空 = 引擎自带默认）。 */
function qaModelAuto(){ return qaModelChain[0] || ""; }
/** **本次会下发哪个模型**（唯一算法；发送、＋菜单提示、显示值三处都走它）。
 *   · `from === "home2"`（工作台首页，新建任务）→ 用户在那枚选择器上选的（`qaModelPick`）；
 *     没选（""）= 自动 → 配置链的链首。
 *   · `from === "home"`（任务对话，1 任务 = 1 会话）→ **该任务自己的模型**（`t.model`）；
 *     任务没定过（老数据 / 选了「自动」）或压根没有任务 → 回落 `qaModelPick || 自动`。
 *  ⚠️ 上下文由**调用方显式给**（from），不靠「当前有没有 qaActiveId」猜 ——
 *     activeId 在工作台首页也会残留着上一次的任务，猜必错。 */
function qaModelFor(from, taskId){
  const auto = qaModelAuto();
  if(from === "home2") return qaModelPick || auto;
  const t = Array.isArray(qaTasks) ? (qaTasks.find(x=>x.id === taskId) || null) : null;
  return (t && t.model) ? String(t.model) : (qaModelPick || auto);
}
/** 用户在某个实例的下拉里选了一个模型 → **写回唯一的那个状态位**（唯一写入口）。
 *   · `from === "home2"`：改 `qaModelPick`（= 新建任务默认用哪个）+ 落 localStorage；
 *   · `from === "home"` 且当前有任务：改**这条任务**的 `t.model`（只影响这条任务）；
 *     没有任务时（任务对话的空态）退化成改 `qaModelPick`，与首页一致。
 *  `v === ""` = 选「自动」（按配置链），允许 —— 会把任务上的模型清空。
 *  ⚠️ 只写状态与显示，**不重算「该试哪个」**（那是 qaModelFor 的事）。 */
function qaModelSet(v, from, taskId){
  const m = String(v == null ? "" : v).trim();
  if(from === "home" && taskId && typeof qaTaskSetModel === "function"){
    qaTaskSetModel(taskId, m);        // 任务自己的模型（它内部会 persist + 刷显示）
    return m;
  }
  qaModelPick = m;
  try{ localStorage.setItem("qaModelPick", qaModelPick); }catch(e){ /* 忽略：记忆失败不影响本次选择 */ }
  qaModelSync();
  return m;
}
/** 把显示值 qaModel 重算一遍（任务对话那条上下文会下发的模型）并刷两处下拉。
 *  两个实例显示的值**可能不同**（首页 = 新建任务的默认；任务对话 = 这条任务的），
 *  所以同步函数按实例各算一次（`qaModelPickSync` 内部走 qaModelFor(p, qaActiveId)）。 */
function qaModelSync(){
  qaModel = qaModelFor("home", qaActiveId);
  if(typeof window.qaModelPickSync === "function") window.qaModelPickSync();
}
/** 读某引擎的模型链并重算显示值（**qaModelChain 的唯一写入口**）。
 *  取数两条路与引擎清单同源：桌面壳走 IPC `settingsModelChain`，浏览器预览态走内置后端
 *  `GET /api/settings/model-chain`（同一份 settings-store 实现）；桥缺专用通道时回落
 *  `settingsGet()` + `settingsChainOf()`（同一套取链判据，不另写一份）。
 *  读不到（无桥 / 无后端 / 引擎名空）→ 空链 → 自动档 = 引擎自带默认，**不编数据**。
 *  @param {string} engine  引擎名（省略 = 当前 qaEngine）
 *  @param {boolean} force  true = 忽略缓存重取（配置改了 / 引擎清单刷新后）
 *  @returns {Promise<string[]>} 该引擎的链（有序） */
async function qaModelChainLoad(engine, force){
  const eng = String(engine || qaEngine || "");
  if(!force && eng && eng === qaModelChainEng) return qaModelChain;
  let chain = [];
  const norm = (arr)=> (Array.isArray(arr) ? arr : []).map(x=>String(x || "").trim()).filter(Boolean);
  try{
    if(window.desk && typeof window.desk.settingsModelChain === "function"){
      const r = await window.desk.settingsModelChain(eng);
      if(r && r.ok) chain = norm(r.chain);
    }else if(window.desk && typeof window.desk.settingsGet === "function" && typeof settingsChainOf === "function"){
      /* 桥没有专用通道（老壳 / 只桩了 settingsGet 的测试桥）：读整份配置，用**同一套判据**
         取链 —— settingsChainOf 就是设置窗口那份（链为空时回落两个派生标量），不另写一份。 */
      const r = await window.desk.settingsGet();
      if(r && r.ok) chain = norm(settingsChainOf(r.config || {}, eng));
    }else{
      /* 无桌面壳（浏览器直接打开 / 静态预览）：同源 /api/settings/model-chain。
         与 qaEnginesPull 同一套「内置后端优先、同源兜底」的取数写法。 */
      const tries = [];
      if(typeof prdBackendBase !== "undefined" && prdBackendBase) tries.push(prdBackendBase + "/api/settings/model-chain?engine=" + encodeURIComponent(eng));
      tries.push("/api/settings/model-chain?engine=" + encodeURIComponent(eng));
      for(const url of tries){
        try{
          const res = await fetch(url);
          if(!res.ok) continue;
          const j = await res.json();
          if(j && j.ok){ chain = norm(j.chain); break; }
        }catch(e){ /* 这条取数路径不可用 → 试下一条 */ }
      }
    }
  }catch(e){ /* 读不到就是空链：宁可「用引擎自带默认」，也不假装配过一个模型 */ }
  qaModelChain = chain; qaModelChainEng = eng;
  /* 链变了 → 两处下拉都要重算：「自动」档用的是链首（首页那枚在没手选时显示它；
     任务对话那枚在没有任务模型时也回落它）。出口在 09-ask-event.js
     （window.qaModelPickSync），加守卫是因为本函数可能在选择器接线之前就被调到
     （启动顺序 / 预览态）。 */
  qaModelSync();
  return chain;
}
/* 当前引擎能否「指定工作目录（-w）」：判据 = `magic-agent --engines` 给的 workspace 字段
   （flag:-C / cwd / none）。**字段缺失（老版 CLI）一律当不支持** —— 下发了会把老 CLI
   打成 unknown flag（exit 2）。不支持时对话框仍会 cd 到工作目录（子进程 cwd），
   只是那些引擎（llm / arkclaw / openclaw）本来就不按 cwd 干活。 */
function qaWorkspaceSupported(engine){
  const row = qaEngines.find(x=>x.engine === (engine || qaEngine)) || null;
  const v = String((row && row.workspace) || "");
  return !!v && v !== "none";
}
/* 当前引擎是否支持 `--stream`（判据 = `--engines` 的 streaming 字段）。
   ⚠️ 默认**支持**（字段缺失 = 老版 CLI，保持原行为照旧加 --stream）——与 workspace 的
   默认相反：那边缺失=不下发（下发会报错），这边缺失=照旧下发（不加会丢增量渲染）。
   不支持时主进程去掉 --stream，并在收尾时把一次性 envelope 合成 text/result 事件
   喂给同一条渲染链路（见 main.js / ask-envelope.cjs）。
   ⚠️ 2026-09-21 起 **openclaw 也报 true**（magic-agent 侧新接了 ACP 流式通道）——
   这张表里现在只剩 arkclaw 是 false；别按老印象把 openclaw 写成不支持。 */
function qaStreamSupported(engine){
  const row = qaEngines.find(x=>x.engine === (engine || qaEngine)) || null;
  return !(row && row.streaming === false);
}
/* 当前引擎认不认 `--permission <档>`（判据 = `--engines` 的 permission 字段：
   `flag:--permission-mode` 认；`none` / 字段缺失（老版 CLI）不认）。
   ⚠️ 这条与上面两条的**代价不一样**，别按同一个直觉写：不认这个 flag 的引擎
   （trae / llm / codex / openclaw / arkclaw / codebuddy-gateway）一旦收到它，CLI 会 `exit 2` 把整轮打死、
   stdout 一个字都不返回 —— 用户看到的就是「openclaw 对话没对接好 / 没有返回任何内容」
   （2026-09-21 报障的根因）。所以默认取**保守**：探测不到 = 不认。
   主进程按同一条判据收口（desktop/agent-cli.cjs::engineCapsFor().permission，
   对话 / 派发 / 归纳三处都不再下发）；这里只用来说话：档位菜单里如实写明
   「该引擎不接受档位」，免得胶囊还显示「自动审批」而实际没生效（静默忽略）。
   ⚠️ 两侧在「拿不准」时的取向**刻意不同**：主进程未知 → 不下发（防把整轮打死）；
   界面未知（清单还没取到 / 名字不在清单里）→ **不宣称**不支持（防启动瞬间闪一行假说明）。
   只有「确实读到这一行」时才照字段说话。 */
function qaPermSupported(engine){
  const row = qaEngines.find(x=>x.engine === (engine || qaEngine)) || null;
  if(!row) return true;
  /* ⚠️ 本仓库补的一处（2026-09-29，其余逐字未改）：上面 263 行写着「界面未知（清单还没取到）
     → 不宣称不支持」，但原实现只判了「名字不在清单里」—— 清单**没取回来**时用的是内置兜底表，
     那张表**没有** capability 字段，于是照字段一判就得出「claude 不接受权限档位」这种**假说明**，
     正好是这条注释要防的事。加一句：兜底表还没被真实清单替换掉之前，一律不宣称不支持。
     （qaEnginesReal 由 91-workbench-stubs.js 声明、92-composer.js 的 qaEnginesFetch 置真。） */
  if(!qaEnginesReal) return true;
  const v = String((row && row.permission) || "");
  return !!v && v !== "none";
}
/* ---------- 需求 workspace（记忆式需求归纳）----------
   选中 workspace 后，qaSend 会把 workspace/taskId/taskTitle 随 desk.ask 透传给主进程，
   用户输入先进该工作区的记忆流（~/.magic-test/requirements/<id>/memory.jsonl）。
   归纳不在单条消息上做，而是记忆式批量整理，三个触发源：
     ① 按窗口：对话流正常收尾（主进程 close 回调）→ 12s 防抖归纳；
     ② 按天：壳启动每日兜底扫描（lastDay ≠ 今天且有未归纳记忆）；
     ③ 手动：workspace 菜单「立即整理」。
   归纳由需求分析 LLM（magic-agent -e llm，--json-schema 结构化输出）完成：
   优先复用已有功能点文档，确属新能力才新建（标题 ≤10 字，产品经理视角）。 */
let qaWorkspace = "";                 // 当前 workspace id（全局：左下角切换器设定，"" = 仅浏览不过滤）
let qaWsList = [];                    // [{id,name,dir,pending,featureCount,featureNames,...}]
try{ qaWorkspace = localStorage.getItem("guanwu.ws") || ""; }catch(e){}
/* 旧数据兼容：早期版本把任务 id（qa-<ts>）当 sessionId 存库 —— 那不是 magic-agent
   的会话 id，续接必然失败。载入时统一清掉，真 id 是引擎返回的 UUID。
   （qaSidClean 定义已上移到任务清单读入之前，见 CT-37 TDZ 根因注释。） */
/* 启动恢复上一次选择的**引擎**（用户 2026-09-18：对话框记住上一次选择）与
   **工作台首页选过的模型**（用户 2026-09-18 原话：「对话框中要记住上一次选择的引擎和模型」
   —— 2026-09-21 模型选择器只留在工作台首页，记忆的口径跟着它走）。
   localStorage 是唯一持久源；读到的引擎已不在本机清单时，由 qaEnginesFetch 回退逻辑兜底回默认。
   ⚠️ 老键 `qaModel`（2026-09-21 那版「对话框不选模型」的遗留 / 更早那版的选择）一律**清掉**：
   本版的选择写在 `qaModelPick`，留着老键只会让「哪来的这个模型」变成下一次排障的谜题。 */
try {
  qaEngine = localStorage.getItem("qaEngine") || "claude";
  qaModelPick = localStorage.getItem("qaModelPick") || "";
  localStorage.removeItem("qaModel");
  /* 权限档位同样记住上一次选择（与引擎一套约定，两个输入框共享同一份 qaPerm）。
     存量值是失效 id（老版本写过别的取值 / 手改过）→ 回退默认档，绝不把未知值透传给 CLI。 */
  const p = localStorage.getItem("qaPerm");
  if(QA_PERM_IDS.includes(p)) qaPerm = p;
} catch(e) {}
/* 桌面桥可用时任务清单以 SQLite（~/.magic-test/tasks.db）为准，
   localStorage 只留 DOM 快照并作浏览器模式兜底。

   ⚠️ 落库一律**回传结果**（用户 2026-09-20 报障：「点击归档无效果 重启又丢失了
   没更新sqllite中状态吗」）。原先这两支是 fire-and-forget —— `.catch(()=>{})` 只兜住
   「IPC 直接抛异常」，主进程**正常回 `{ok:false}`**（典型：tasks.db 里没有这一行）也照样
   当成功：界面归了档、库里还是 0，重启时 qaDbLoad 以库为准一覆盖，归档就没了。
   现在统一 `Promise<{ok, error?}>`；桥不可用（浏览器 / 静态预览）回 `{ok:true, skipped:true}`
   —— 那种模式本来就只落 localStorage（既定兜底），不该被当成失败。
   调用点里「存快照 / 切会话」这类仍可 fire-and-forget（传 status/title 的那些），
   而**用户动作**（置顶 / 归档）必须看结果，见 qaTaskFlag。 */
const qaDesk = (typeof window !== "undefined" && window.desk && typeof window.desk.tasksList === "function") ? window.desk : null;
const qaDbOk = (r)=> (r && r.ok) ? { ok:true, task:r.task } : { ok:false, error:(r && r.error) || "落库失败" };
const qaDbErr = (e)=> ({ ok:false, error:String((e && e.message) || e) });
function qaDbCreate(t){
  if(!qaDesk) return Promise.resolve({ ok:true, skipped:true });
  if(!t) return Promise.resolve({ ok:false, error:"没有这条会话" });
  return qaDesk.tasksCreate({ id:t.id, title:t.title, engine:t.engine || "claude", model:t.model || "",
    sessionId:t.sessionId || "", status:t.status || "running",
    /* reqId = **任务表绑定需求id**（用户 2026-09-20）：需求派发的会话把绑定关系落到 `tasks.req_id` 列，
      不再只靠 id 前缀反推；普通对话为空（库里 NULL）。 */
    reqId: t.reqId || "",
    pinned: t.pinned ? 1 : 0, archived: t.archived ? 1 : 0,
    workspace: t.workspace || wsDirOf(), createdAt:t.ts, updatedAt:t.ts }).then(qaDbOk).catch(qaDbErr);
}
function qaDbUpdate(t, patch){
  if(!qaDesk) return Promise.resolve({ ok:true, skipped:true });
  if(!t) return Promise.resolve({ ok:false, error:"没有这条会话" });
  // patch.touch === false：库里也**别动 updated_at**（列表按它倒序，动了顺序就会变）。
  // 见 qaTaskSaveSnapshot 的说明 —— 仅切会话这类「无新内容」的落库走这条路。
  // reqId 每次都带上：绑定关系一旦建立就不该被后来的更新抹掉（主进程侧缺省时原样保留）。
  return qaDesk.tasksUpdate(t.id, Object.assign({ title:t.title, engine:t.engine, model:t.model,
    status:t.status, reqId:t.reqId || "", updatedAt:Date.now() }, patch || {})).then(qaDbOk).catch(qaDbErr);
}
function qaDbDelete(id){ if(qaDesk) qaDesk.tasksDelete(id).catch(()=>{}); }
/* 启动时以 SQLite 清单覆盖本地列表（快照仍从 localStorage 按 id 拾取；
   空库 = 首次启动，保留 localStorage 内容）。 */
async function qaDbLoad(){
  if(!qaDesk) return;
  try{
    // ⚠️ 取**全量**任务（不按 workspace 过滤）：qaTasks 同时承载各目录会话的 DOM 快照，
    //    服务端过滤会让其他目录的快照在这一步丢失。目录过滤只在 qaTaskRender 里做显示层。
    const r = await qaDesk.tasksList();
    if(!r || !r.ok || !Array.isArray(r.tasks) || !r.tasks.length) return;
    const snaps = new Map(qaTasks.map(t=>[t.id, t.snapshot || ""]));
    const prev = new Map(qaTasks.map(t=>[t.id, t]));      // sidLost 只在内存/localStorage 里，重建列表时带上
    qaTasks = r.tasks.map(d=>({
      id: d.id, title: d.title || "新对话", engine: d.engine || "claude", model: d.model || "",
      sessionId: qaSidClean(d.sessionId), status: d.status || "done",
      workspace: d.workspace || "",      // 归属目录（老数据为空 → 视作默认 workspace）
      ts: d.updatedAt || d.createdAt || Date.now(), snapshot: snaps.get(d.id) || "",
      pinned: d.pinned ? 1 : undefined,  // 置顶 / 归档（用户 2026-09-18）：随库回读，重启不丢
      archived: d.archived ? 1 : undefined,
      /* 绑定的需求 id（用户 2026-09-20：「任务表绑定需求id」）：**优先读库里的 `req_id` 列**，
         老行（迁移前落的、或库被手改过）退回按 id 前缀认 —— 两条路都通，重启后仍能对上。 */
      reqId: d.reqId || qaReqOfId(d.id),
      sidLost: (prev.get(d.id) || {}).sidLost || undefined,
    }));
    /* ⚠️ 这里**不许**再 `.slice(0, QA_TK_MAX)`（2026-09-20 删）：那会把库里的历史会话
       挡在内存之外 → 左栏 tab 计数与列表跟着少（实测「已完成」18 ↔ 需求看板 48）。
       上限只约束**快照**：`snaps` 来自 localStorage（最多 QA_TK_MAX 条），其余条目
       snapshot 为空串 —— 点开时由 qaTaskSwitch 按需求现拼简版画面兜底。 */
    // SQLite 里的 running 同样是上次会话的残影（流已随关窗消亡），一并归正；
    // 同时补记 sidLost（与上面 localStorage 那条同理）：被打断的那一轮没拿到 session_id，
    // 下次追问要用 `-c` 接回该目录最近一次会话，不能让这条会话「重启即失忆」。
    // ⚠️ 归正成 `error`（不是 done）—— 理由见文件上方那条注释（三档状态里的「已完成」不许收残影）；
    //    老值 `queued` 也在这里归一成 `pending`（同上，会话状态与需求状态逐字一致）。
    qaTasks.forEach(t=>{
      if(t.status === "running"){ t.status = "error"; if(!t.sessionId) t.sidLost = 1; }
      else if(t.status === "queued") t.status = "pending";
    });
    qaTaskRender(); qaTaskPersist();
    if(!qaActiveId || !qaTasks.some(t=>t.id===qaActiveId)){
      // 优先落到当前工作目录下的最近一条会话
      const first = qaTasks.find(t=>wsMatch(t.workspace)) || qaTasks[0];
      qaActiveId = first.id;
      if(first.snapshot){ const s = $("#qa-stream"); if(s){ s.innerHTML = first.snapshot; s.hidden = false; qaDropRunPills(s); } }
      qaSyncChatMode(); qaTaskRender();
    }
  }catch(e){}
}

/* ---------- 快照条数上限：最多 QA_TK_MAX 条**带快照**，超出的只摘快照、不删条目 ----------
   用户 2026-09-20 报障：「处理中的需求对应的会话现在不显示了」（左栏找不到、点卡片也跳不过去）。
   根因 = 淘汰判据用的是**数组位置**而不是**会话年龄**：
   `qaReqConvSyncAll` 一趟里按 `reqItems`（created_at DESC，最新在前）给每条已派发需求建会话，
   每建一条 `qaTasks.unshift(t)` 到队首、紧接着 `qaTasks.length = QA_TK_MAX` 截断 ——
   一趟跑完被砍掉的恰是**最先建的那批 = 最新的需求**（实测：41 条已派发需求跑完一趟，
   内存里只剩 9/17–9/19 的老会话，9/20 那批连正在处理中的一起从内存里消失）。
   按 ts 淘汰最老的才符合既有契约（需求文档：「最多保留 20 条任务记录，超出后自动淘汰最早的」）。
   三类**带快照的名额**永不淘汰（与左栏折叠的保留规则同口径，见 05-task-list-render.js 的 qaTaskFold）：
     · 正在处理的（status=running）—— 「处理中的那条一定看得见」，这正是本次报障的那条；
     · 用户置顶的 —— 置顶被淘汰等于没置顶；
     · 当前打开的（qaActiveId）—— 否则用户正看着的会话会从列表里凭空消失。
   保护项多于上限时按软上限处理（全留）—— 上限管的是「快照的堆积」，不该反噬当下这几条。
   【2026-09-20 再修】被挤出上限的**只清 snapshot，条目一律留在 `qaTasks` 里**：
   条目就是左栏的 tab 计数与列表，删了它数字立刻变小、列表也少行，而且删得**无声无息** ——
   用户看到的就是「已完成对话 (18)」对不上「已完成需求 48」。清掉快照的会话点开时由
   qaTaskSwitch 按需求现拼简版画面（见 05-task-list-render.js），不会是一片空白。 */
let qaTasksKeepId = "";   // 显式要求「这次别裁掉」的那条（点卡片定位时用，见 qaReqOpenConv）
function qaTasksTrim(){
  const keep = new Set(qaTasks
    .filter(t=>t.status === "running" || t.pinned || t.id === qaActiveId || t.id === qaTasksKeepId)
    .map(t=>t.id));
  qaTasks.slice().sort((a,b)=>(b.ts||0)-(a.ts||0)).forEach(t=>{ if(keep.size < QA_TK_MAX) keep.add(t.id); });
  for(let i = 0; i < qaTasks.length; i++){
    const t = qaTasks[i];
    if(!keep.has(t.id) && t.snapshot) t.snapshot = "";   // 只摘快照，条目留着（左栏计数/列表要它）
  }
}
function qaTaskPersist(){
  /* 落 localStorage 的按**年龄**取前 QA_TK_MAX 条（不是数组位置）：位置法在「一趟批量建会话」
     之后会把最新的那批存丢（与 qaTasksTrim 同因）。数组本身不动 —— 左栏顺序由 qaTaskRender 自己排。
     ⚠️ 落盘只存**带快照**的最近 QA_TK_MAX 条（快照是大头，localStorage 有容量上限）；
     更早的会话条目由 `tasks.db` 兜底（启动时 qaDbLoad 全量读回），不靠这份缓存。 */
  try { localStorage.setItem("qaTasks", JSON.stringify(
    qaTasks.slice().sort((a,b)=>(b.ts||0)-(a.ts||0)).slice(0, QA_TK_MAX))); } catch(e) {}
}
function qaTaskTime(ts){
  const d = new Date(ts), now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  const hm = d.getHours().toString().padStart(2,"0") + ":" + d.getMinutes().toString().padStart(2,"0");
  return sameDay ? hm : (d.getMonth()+1) + "/" + d.getDate();
}
