/* 工作台侧符号 —— 对话渲染层会引用，但它们属于**工作台**（需求看板 / 文档阅读器 /
 * 消息库 / 截图相册），magic-agent 这一版只有对话。
 *
 *  ── 两类，别混 ──
 *   ① **纯函数**：与工作台无关、只跟对话状态有关的（wsMatch / qaReqOfId / qaTaskStatusOf /
 *      qaEngHintSync / qaRefreshRunning）—— 从 magic-test 原样搬来，语义一字不改。
 *   ② **诚实桩**：这条能力这一版没有 —— 一律**明说**（toast / console.warn），
 *      不假装成功、不静默吞掉。这些桩就是「还没接上」的清单，也是后续增强点的来源。
 */

/* ================= ① 纯函数（原样搬自 magic-test） ================= */

/* 02-docs-data.js：qaRunning 是派生量（由 qaRefreshRunning 重算），qaUnsub 是事件订阅句柄 */
let qaRunning = false, qaUnsub = null;
/* 引擎清单是否已从 CLI 取回（true = qaEngines 是真实清单；false = 还是内置兜底表）。
   跨文件共享状态：92-composer.js 的 qaEnginesFetch 写它，04-task-list.js 的 qaPermSupported 读它。
   两侧都只在**运行时**访问（不在脚本加载期），所以归到这份共享状态里最清楚。 */
let qaEnginesReal = false;
/** 07/03 都读它：当前有没有在跑的轮次。**唯一算法**，别各处自己数 qaRuns。 */
function qaRefreshRunning(){ qaRunning = qaRuns.size > 0; qaSyncBusy(); }

/* 02-docs-data.js：消息图标常量。**纯常量**（没有一条与工作台相关）—— 原样搬来。
   03-chat-message.js 的提问卡（askCard）读 QA_ICONS.ask；漏了它 → 审批卡整张抛
   ReferenceError（画面表现：引擎问了、界面什么都没出现）。 */
const QA_ICONS = {
  quote: '<svg viewBox="0 0 24 24"><path d="M4 6h16M4 11h10M4 16h13"/></svg>',
  reply: '<svg viewBox="0 0 24 24"><path d="M21 12a8 8 0 01-8 8H5l-2 2V12a8 8 0 018-8h2a8 8 0 018 8z"/></svg>',
  tool: '<svg viewBox="0 0 24 24"><path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/></svg>',
  agent: '<svg viewBox="0 0 24 24"><path d="M12 3v3M8 6h8a3 3 0 0 1 3 3v6a3 3 0 0 1-3 3H8a3 3 0 0 1-3-3V9a3 3 0 0 1 3-3z"/><path d="M9.5 11.5h.01M14.5 11.5h.01M9.5 15h5"/></svg>',
  ask: '<svg viewBox="0 0 24 24"><path d="M21 11.5a8 8 0 0 1-8 8H7l-4 3v-9a8 8 0 0 1 8-8h2a8 8 0 0 1 8 6z"/><path d="M9.6 9.3a2.5 2.5 0 1 1 3.1 2.4c-.5.2-.8.6-.8 1.1v.5"/><path d="M12 16.2h.01"/></svg>',
};

/* 12-req-board.js：工作目录归属判定。本版没有「工作目录切换器」，wsDirOf() 恒为 ""
   → 第一行 `if(!d) return true` 直接放行（= 不按目录过滤）。整段保留，语义与上游一致。 */
const WS_DEMO_PLACEHOLDER = "__DEFAULT__";
let qaWsDefaultDir = "";
function wsMatch(recWs){
  const d = wsDirOf();
  if(!d) return true;                                 // 「不记录需求（仅浏览）」= 不按目录过滤
  let rec = String(recWs || "");
  if(rec === WS_DEMO_PLACEHOLDER) rec = qaWsDefaultDir || "";
  if(rec) return rec === d;
  return !!(qaWsDefaultDir && d === qaWsDefaultDir);
}

/* 12-req-board.js：会话 id ↔ 需求 id。本版不派发需求，但 id 前缀解析是通用的，原样保留。 */
const REQ_TASK_PREFIX = "qa-req-";
function qaReqOfId(id){
  const s = String(id || "");
  return s.startsWith(REQ_TASK_PREFIX) ? s.slice(REQ_TASK_PREFIX.length) : "";
}
/* 需求**侧**那条 —— 需要需求看板。本版没有看板 → 查不到（如实返回 null，
   于是 qaTaskStatusOf 走「普通对话」兜底分支，显示会话自己的 status）。 */
function qaReqItemOf(){ return null; }
function qaReqBoardLoaded(){ return false; }

/* 12-req-board.js：会话状态口径。因 qaReqItemOf 恒 null，实际走的是下面这段兜底
   （与上游逐字一致 —— 兜底词表与需求侧同一批状态名）。 */
function qaTaskStatusOf(t){
  const raw = String((t && t.status) || "done");
  const st = raw === "queued" ? "pending" : raw;            // 老值兼容
  const it = qaReqItemOf(t && t.reqId);
  if(it && typeof qaReqStatusOf === "function") return qaReqStatusOf(it);
  const label = st === "running" ? "处理中"
    : st === "verify" ? "待验证"
    : st === "pending" ? "排队中"
    : st === "stopped" ? "已停止"
    : st === "error" ? "失败"
    : "已完成";
  const detail = st === "running" ? "流式输出，命令行退出即完成"
    : st === "verify" ? "等审查结论"
    : st === "pending" ? "等前一条结束（同一工作目录串行执行）"
    : st === "stopped" ? "你自己喊的停"
    : st === "error" ? "上次调用失败"
    : "";
  return { st, phase: "", label, detail, live: st === "pending" || st === "running", manual: false };
}

/* 12-req-board.js：＋ 菜单底部「派发引擎 / 模型 / 工作目录」那行说明。原样保留。 */
function qaEngHintSync(){
  const dir = wsDirOf();
  const ws = !dir ? ""
    : (qaWorkspaceSupported() ? ` · 工作目录 ${dir}` : " · 该引擎不支持指定工作目录");
  ASK_INSTANCES.forEach(p=>{
    const el = $("#" + p + "-eng-hint"); if(!el) return;
    const mdl = (typeof qaModelFor === "function") ? qaModelFor(p, qaActiveId) : qaModel;
    el.textContent = `派发引擎 ${qaEngine}${mdl ? " · " + qaModelName(mdl, qaEngine) : ""}${ws}`;
  });
}

/* 27-conv-log.js：找不到历史时那句说明（如实）。原样保留。 */
function qaConvNoticeHtml(text, kind){
  return `<div class="qa-hist-note${kind ? " " + kind : ""}">${escHtml(text)}</div>`;
}
function qaConvLostText(t){
  const hasSid = !!(t && t.sessionId);
  return "这条对话的历史没有留存下来（这一版还没有本地消息库）。"
    + (hasSid ? "会话本身还在 —— 直接在这里追问，引擎会接着原来的上下文继续。" : "这一轮没有拿到会话 id，追问会新起一段上下文。");
}

/* ================= ② 诚实桩（这条能力这一版没有） ================= */

/* 内置后端基址：magic-test 靠它取引擎清单 / 上传截图。magic-agent 走 transport，
   不需要它 —— 声明成空串，所有 `if(prdBackendBase)` 分支自然短路到相对路径 / 直接失败。 */
var prdBackendBase = "";

/* 需求看板（12-req-board.js）—— 本版无看板 */
function reqHas(){ return false; }
function reqBoardFetch(){ return Promise.resolve(null); }
function reqBoardAddFromChat(){ return Promise.resolve(null); }
function qaReqConvMsg(){ return ""; }
function qaReqEngineMsg(){ return ""; }
function qaReqStreamApply(){ return false; }
function qaReqConvSnapshot(){ return ""; }
function qaReqConvSync(){ /* 无看板可同步 */ }
function qaReqConvDropMark(){ /* 无看板可标记 */ }
function qaTaskHistoryRecover(){ return Promise.resolve(""); }

/* 消息库 / 对话归档（27-conv-log.js）—— 本版无持久层。
   ⚠️ 这意味着**刷新页面历史就没了**：左栏条目来自 localStorage 的 qaTasks（04-task-list.js），
   点开时靠 t.snapshot 还原当前会话画面；其余（逐轮消息表）没有。持久化 = 增强点之一。 */
function qaMsgLogLoad(){ return Promise.resolve([]); }
function qaConvTurnSync(){ /* 无消息库可写 */ }

/* 截图相册 / 文档阅读器（右栏）—— 本版无右栏 */
function reqShotUrl(){ return ""; }
function reqFeatOpenInDock(){ notHere("打开功能点文档")(); }
function reqShotsOpen(){ notHere("截图相册")(); }

/* 产品文档页的数据源（10-markdown.js 的文档阅读器读它们）—— 本版无文档页，留空即死路径。 */
let prdData = [];
let prdDetailIndex = {};
let prdRowsAll = [];
const prdStateText = {};
