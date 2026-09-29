/* ---------- 左栏「任务 · 对话」渲染 ----------
   四条用户定稿的显示规则：
   ① **处理中的进度效果**（2026-09-17）：status=running 的行渲染一枚 12px 旋转进度环（QA_TK_SPIN），
      不再是那枚安静的脉冲圆点；done/error 仍走圆点（.tk-st）。
      派发需求产生的会话同样带 running 状态（REQ_TASK_STATUS），所以看板派发也能看到转圈。
   ② **每档永远最多显示 20 条**（2026-09-22 用户：「任务对话区域永远最多显示20个」）——
      原先是「默认 5 条 + 查看更多 (N) 展开全部」（2026-09-17 那条已下线，`.tk-more` 连同
      三份开合态一起删掉）：用户要的是**固定的上限**，不是「想点就能看全」。
      ⚠️ 上限内**当前会话 / 正在处理的那条 / 置顶的那条优先占名额**（挤掉的是最老的那些普通行）——
      正在跑的被挤出去等于进度效果白加，置顶被挤出去等于没置顶。
      ⚠️ 超出 20 条的**不再有展开入口**：老对话的找回路径是**搜索**（见 ④）或
      **在需求看板点卡片跳到它那条会话**（见 qaTaskReveal）——用户原话
      「历史的能通过搜索或者点击需求跳转找到就行了」。列表底部留一行如实说明（`.tk-note`），
      不让人以为「只有 20 条」。
   ③ **三档状态 = 胶囊 tab**（2026-09-20 用户原话：「对话列表中审查中的任务直接显示到了已归档里不对
      增加1个审查中的分组。同时3个状态的分组改成胶囊tab形式」+「已归档改成已完成」）：
      · 三档 = 「处理中 / 审查中 / 已完成」，渲染成一枚**分段控件**（`.tk-tabs` / `.tk-tab`，与需求看板
        页头的「列表 / 看板」、用户菜单里的「外观」同一套语汇）：**一次只显示一档的内容**，
        点哪个 tab 就看哪一组，每档带该组计数 `(N)`。tab 即分组 —— 旧的「可折叠帽子堆在一起」
        那种形态已下线（`.tk-group` / `.tk-group-head` 连同样式一起删了，别只删 JS 留死样式）。
      · **会话状态 = 需求状态，1 对 1**（2026-09-20 用户追加：「需求的状态为什么跟任务状态不一致
        应该1对1一致的啊」）：状态值、文案、分档**全部**出自唯一一份口径 `qaTaskStatusOf`
        （12-req-board.js）—— 需求派发的会话，status 就是需求 `status` 逐字
        （pending / running / verify / done / error / stopped），文案直接复用需求侧的
        `reqStatusLabel`（待处理 / 排队中 / 处理中 / 审查中 / 待验证 / 验证未通过 / 审查未定论 /
        已完成 / 失败 / 已停止）。以前会话侧自己抄了一张映射表（`stopped→error`、`pending→queued`），
        于是「卡片写已停止、这一行写上次调用失败」「卡片写待处理、这一行写排队中」——两边各说各话。
      · **三档都是「状态」，且互斥**（见 qaTaskGroupOf）：**状态优先，归档只对「不再跑」的行生效**：
        - 审查中 = 需求正走审查这条线（= 需求看板「待验证」那一列的会话，见 qaTaskReviewing）；
        - 处理中 = **还没结束**的（排队 / 在跑）—— 哪怕它被归档过也留在这里（状态说了算）；
        - 已完成 = 跑完的（`status='done'`：干活轮过审 / 人工确认完成）**或**用户归档了它
          （归档现在唯一的用途：把「失败 / 已停止」这类不再跑的会话收进已完成）。
      · **「审查中」优先于「已完成」**，而「已归档」这一档**整个改名成「已完成」并接住 status=done**
        —— 用户 2026-09-20 报的正是「审查中的任务显示到了（当时叫已归档的）那一档里」。
        行尾 ⋯ 的「归档 / 移出归档」只在**真会改变归属**时才出（见 qaTkMenuItems），不留空动作。
      · **三档状态只装「需求派发的会话」**（`qaTaskIsReqConv`），计数与需求看板的列一一对应：
        处理中档 = 需求「待处理」列 + 「处理中」列；审查中档 = 「待验证」列；已完成档 = 「已完成」列。
        **没绑需求（或绑的需求已被删）的普通对话单独进第 4 档「对话」**（空则不渲染那枚 tab）——
        用户 2026-09-20：「处理中的任务就应该跟需求数一致啊」：不分开的话左栏永远比需求多
        （实测 已完成 56 vs 需求 50，多出来的就是首页直接发起的普通对话）。
      · 归属只由**状态 + 用户动作**决定，且**不改 ts**（行尾时间与「最新 5 条」的基准原样保留）。
      · 默认停在「处理中」档；「审查中 / 已完成」不切过去就看不见（= 旧契约里「已归档默认收起」）。
      · 置顶行排在**本组最前**（置顶 = 钉在顶部，不另开第四个分组），行内带一枚朱砂图钉。
   ④ **搜索 = 找历史会话的唯一入口**（2026-09-22 用户：「支持一下搜索功能」）：
      搜索框在 `#nav-tasks` **之上**（`index.html` 里的静态节点，不随列表重绘消失，见 `.tk-search`）。
      关键词匹配 **会话标题 + 关联需求的标题 / 正文**（都来自内存里各自的清单，不额外发 IPC）。
      ① **跨档搜索**：命中结果不再按 tab 分组（tab 计数本来就不含搜索条件，摆在搜索结果上方只会误导），
         命中的行按同一套排序（置顶优先 → ts 倒序）平铺，**上限同为 20 条**（与 ② 同一条口径）；
      ② 清空关键词 = 退回 tab 视图（搜索是**临时态**，不做持久化，切工作目录一并复位）；
      ③ **一次命中不到就如实说**（`.tk-empty`）：换关键词，或去需求看板点卡片跳过去。
      ⚠️ 「点需求跳转」时**要清掉搜索词**（见 qaTaskReveal）：否则跳过去的那条会被搜索条件过滤掉，
         画布切了、左栏却看不到它（用户会认为没反应）。
   ⑤ **组内排序**：置顶在最前，其余按 `ts` 倒序（与行尾那个时间戳同源）。
      ⚠️ 归属哪些档由 ③ 的 `qaTaskGroupOf` 决定；本文件只负责「取哪一档的行、怎么排、列几条」。 */
/* 每档的**硬上限**（2026-09-22 用户：「任务对话区域永远最多显示20个」）——
   不是「默认值」、也没有「查看更多」能突破它（那条路径 2026-09-22 整体下线）。
   超出部分靠搜索 / 点需求卡片跳转找回，见文件头 ② ④。 */
const QA_TK_VISIBLE = 20;
let qaTaskTab = "active";              // 当前选中的胶囊 tab（active / review / done / chat）
/* 搜索关键词（**临时态**：不落 localStorage；清空 = 回 tab 视图；切工作目录一并复位）。
   非空时列表切进「搜索结果」形态：跨档平铺命中行、上限同样 20 条（见文件头 ④）。 */
let qaTkQuery = "";
/* 「这一次一定要露出来的那条」（点需求卡片 / 点列表跳转时由 qaTaskReveal 置位）——
   20 条上限生效时它优先占名额（见 qaTaskFold）；qaTaskSwitch 接管成 qaActiveId 后即清空。 */
let qaTaskRevealId = "";
const QA_TK_GROUPS = [["active", "处理中"], ["review", "审查中"], ["done", "已完成"], ["chat", "对话"]];
/** 处理中/审查中的转圈进度环（2026-09-17 用户：「处理中的进度效果」）——
 *  文案随**需求状态**走（跑审查轮时 aria-label 是「审查中」，不是「处理中」），见 qaTaskStatusOf。 */
function qaSpinHtml(label, title){
  return `<span class="tk-spin" role="status" aria-label="${escHtml(label)}" title="${escHtml(title)}">`
    + '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9" pathLength="100" stroke-dasharray="57 100"/></svg></span>';
}
/* 行尾 ⋯（下拉操作入口）与置顶图钉（material push_pin，实心，走 fill:currentColor） */
const QA_TK_DOTS = '<svg viewBox="0 0 24 24" aria-hidden="true">'
  + '<circle cx="5" cy="12" r="1.9"/><circle cx="12" cy="12" r="1.9"/><circle cx="19" cy="12" r="1.9"/></svg>';
const QA_TK_PIN = '<span class="tk-pin" role="img" aria-label="已置顶" title="已置顶">'
  + '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M16 9V4h1a1 1 0 000-2H7a1 1 0 000 2h1v5a3 3 0 01-3 3v2h5.97v7l1 1 1-1v-7H19v-2a3 3 0 01-3-3z"/></svg></span>';
/** 行尾那枚状态标记（转圈 / 圆点）——**与需求状态 1 对 1**（用户 2026-09-20：「需求的状态为什么跟
 *  任务状态不一致 应该1对1一致的啊」）：
 *    文案与状态一律取自 `qaTaskStatusOf`（需求派发的会话直接复用需求侧的 `reqStatusLabel`），
 *    所以「需求卡片写已停止、这一行写上次调用失败」那种错位不会再出现；
 *    颜色只有一档映射（转圈=在跑 · 琥珀=审查这条线 · 靛蓝=排队/待处理 · 朱红=失败/已停止 · 灰=已完成）。
 *  ⚠️ `status` 逐字进 class（`tk-st <st>`），样式见 app.css —— 新增状态记得同时加一条颜色。 */
function qaTaskSt(t){
  const s = qaTaskStatusOf(t);
  const title = s.detail ? `${s.label} · ${s.detail}` : s.label;
  if(s.st === "running") return qaSpinHtml(s.label, title);
  return `<span class="tk-st ${escHtml(s.st)}" title="${escHtml(title)}"></span>`;
}
/** 组内排序：**置顶在前**，其余按 ts 倒序（ts 同时是行尾那个时间戳，见 qaSaveSnapshotFor）。 */
function qaTaskOrder(a, b){
  return ((b.pinned?1:0) - (a.pinned?1:0)) || ((b.ts||0) - (a.ts||0));
}
/* ---------- 归组：三档互斥，**状态优先**（用户 2026-09-20 报的就是「审查中的跑到已完成里」）---------- */
/** 这条会话是不是「审查中」—— 即需求看板「待验证」那一列里的会话。两个判据（任一成立即算）：
 *    ① 需求 `status='verify'`：干活轮跑完了、等审查结论（含审查未通过 / 审查未给结论）；
 *    ② 需求 `status='running'` 且 `phase='verify'`：审查轮**正在跑**（看板卡片标「审查中」）。
 *  ⚠️ 两个判据都走 `qaTaskStatusOf`（唯一口径）：② 的 phase 要按 **reqId 反查需求**，
 *     查不到（没这条需求 / 看板还没加载）就只按 ① 判，绝不猜 —— 猜错的代价是把一条普通会话
 *     搬进「审查中」，比漏判更糟。 */
function qaTaskReviewing(t){
  const s = qaTaskStatusOf(t);
  return s.st === "verify" || (s.st === "running" && s.phase === "verify");
}
/** 这条会话算不算「**需求派发的会话**」—— 三档状态只装它们（计数才能与需求看板的列对上）。
 *  判据：① 绑了需求（`t.reqId`，库里 `tasks.req_id`；老数据退回 id 前缀 `qa-req-<需求id>`）；
 *       ② 那条需求**还在**（看板已加载时按 reqId 查得到）。
 *  ⚠️ 「绑了但查不到」有两种情形，处理方式不同：
 *    · **看板还没加载**（启动那一瞬 / 切目录的中间态）→ **先信绑定关系**，别把整列会话瞬移进「对话」档
 *      （否则每次刷新都会看到计数先跳一下）；
 *    · **看板已加载、需求确实不在**（需求被删了）→ 这条只剩对话历史 → 归「对话」档
 *      （用户要的是「数字对得上」，留着它会让左栏比需求多一条）。
 *  ⚠️ 没绑需求的（首页直接发起的普通对话）→ 一律「对话」档。 */
function qaTaskIsReqConv(t){
  const reqId = (t && (t.reqId || qaReqOfId(t.id))) || "";
  if(!reqId) return false;
  if(typeof qaReqItemOf === "function" && qaReqItemOf(reqId)) return true;
  const loaded = (typeof qaReqBoardLoaded === "function") && qaReqBoardLoaded();
  return !loaded;
}
/** 会话 → 归属的 tab（**互斥**，判据全部来自 `qaTaskStatusOf`）：
 *    · 对话 = 不是需求派发的会话（没绑需求 / 绑的需求已删，见 qaTaskIsReqConv）—— 单独一档；
 *    · 审查中 = 审查这条线（见 qaTaskReviewing）；
 *    · 处理中 = **还没结束**的（排队 / 在跑）—— 状态说了算，**归档不许把在跑的行挪走**；
 *    · 已完成 = 跑完的（`status='done'`）或**用户归档了的**（只有「不再跑」的行才吃这一档：
 *      失败 / 已停止收进已完成，正是「归档」这枚动作现在唯一的用途）。
 *  ⚠️ 「在跑 + 已归档」这种组合（历史上真出现过：先归档、之后又被重新派发）必须落在「处理中」——
 *     需求卡写着「处理中」而左栏那行躺在「已完成」里，就是用户报的「状态不一致」。 */
function qaTaskGroupOf(t){
  if(!qaTaskIsReqConv(t)) return "chat";
  if(qaTaskReviewing(t)) return "review";
  const s = qaTaskStatusOf(t);
  if(s.live) return "active";
  if(s.st === "done" || t.archived) return "done";
  return "active";
}
/* ---------- 搜索：关键词匹配 +「哪一条算命中」（见文件头 ④）----------
   匹配面 = 会话标题 + **关联需求的标题 / 正文**（需求派发的会话标题就是 LLM 归纳的要点，
   而正文里往往才是用户真正记得的那句话；两个清单都在内存里，不额外发 IPC）。
   ⚠️ 大小写不敏感、前后空白不算内容（用户手输的空格不该让结果为空）。 */
function qaTkQueryTrim(){ return String(qaTkQuery || "").trim().toLowerCase(); }
function qaTkHit(t){
  const q = qaTkQueryTrim(); if(!q) return false;
  if(String(t.title || "").toLowerCase().includes(q)) return true;
  const reqId = (t && (t.reqId || qaReqOfId(t.id))) || "";
  if(!reqId) return false;
  const it = (typeof qaReqItemOf === "function") ? qaReqItemOf(reqId) : null;
  if(!it) return false;
  return String(it.title || "").toLowerCase().includes(q)
      || String(it.body || "").toLowerCase().includes(q);
}
/** 写关键词（**唯一写入口**）：值没变、且输入框内容也一致时什么都不做（避免每敲一个字都重画）。 */
function qaTkQuerySet(v){
  const next = String(v == null ? "" : v);
  const box = $("#tk-search-input");
  if(next === qaTkQuery){
    if(box && box.value !== next) box.value = next;      // 外部复位（切目录 / 跳转）时同步输入框
    return false;
  }
  qaTkQuery = next;
  if(box && box.value !== next) box.value = next;
  const clear = $("#tk-search-clear");
  if(clear) clear.hidden = !next;
  qaTaskRender();
  return true;
}
/** 清空搜索（跳转前 / 切工作目录时用）——顺带收起那枚「清空」按钮 */
function qaTkQueryReset(){ return qaTkQuerySet(""); }
/** 分组胶囊 tab（处理中 / 审查中 / 已完成 / 对话）——**一枚分段控件**，当前档是白底胶囊。
 *  用户 2026-09-20：「3个状态的分组改成胶囊tab形式」+「已归档改成已完成」+「处理中的任务就应该跟需求数一致啊」。
 *  **三档状态 = 需求派发的会话，计数与需求看板的列一一对应**（处理中档 = 待处理列 + 处理中列；
 *  审查中档 = 待验证列；已完成档 = 已完成列）——「数字对得上」是用户反复提的那条。
 *  为此多出第 4 档「对话」：**没绑需求（或绑的需求已被删）的普通对话**单独放，不混进那三档，
 *  否则左栏数永远比需求多（实测：已完成 56 vs 需求 50，多出来的 6 条就是首页直接发起的普通对话）。
 *  ⚠️ 三档状态**恒定渲染**（哪怕 0 条）——它是「状态筛选器」，计数 (0) 本身就是信息；
 *     「对话」档**只在非空时出现**（用户要的是三档状态，没普通对话时不该多一枚 tab）。
 *  ⚠️ `data-group` 必须留着：它不只是给样式用的选择器，也是**组件契约** ——
 *  CT-53 用它断言分组、并在 `#nav-tasks .tk-tab[data-group="done"]` 里定位「已完成」那一档。
 *  ⚠️ `role=tablist / role=tab / aria-selected` 与需求看板页头那枚 `.req-seg` 同一套语义。 */
function qaTaskTabsHtml(counts){
  const tabs = QA_TK_GROUPS
    .filter(([key])=> key !== "chat" || (counts.chat || 0) > 0)     // 「对话」空则不出现
    .map(([key, label])=>{
      const on = key === qaTaskTab;
      const n = counts[key] || 0;
      const tip = key === "chat" ? "没绑需求的普通对话（首页直接发起的）" : `${label}（需求侧同名状态的会话）`;
      return `<button class="tk-tab${on ? " on" : ""}" type="button" role="tab" data-group="${key}"
        data-n="${n}" data-testid="tk-tab-${key}" aria-selected="${on}"
        title="${on ? "当前显示「" : "切到「"}${label}」（${n} 条）· ${tip}">${label}<span class="gn">(${n})</span></button>`;
    }).join("");
  return `<div class="tk-tabs" role="tablist" aria-label="对话分组">${tabs}</div>`;
}
/** 某档空时的提示（tab 是筛选器，切过去没有内容要**说清楚为什么**，不能只剩一片空白） */
function qaTaskEmptyHtml(key){
  const txt = key === "review" ? "没有审查中的对话 —— 需求跑完干活轮、进入审查后会先落到这里"
    : key === "done" ? "还没有已完成的对话 —— 跑完或被归档的会话会落到这里"
    : key === "chat" ? "还没有普通对话 —— 首页直接发起的对话会落到这里"
    : (qaTasks.length ? "该工作目录下没有进行中的对话 —— 跑完的都在「已完成」里"
                      : "还没有对话 —— 在首页发送第一条任务");
  return `<div class="tk-empty">${txt}</div>`;
}
/** 切 tab（点当前档 = 无操作）：只改左栏显示归属，不动会话 / 画布。
 *  ⚠️ 不改 ts、不 touch —— 切 tab 是纯浏览动作（同「切会话只存快照」那条契约）。 */
function qaTaskTabSet(key){
  if(key === qaTaskTab) return false;
  if(!QA_TK_GROUPS.some(g=>g[0] === key)) return false;
  qaTaskTab = key;
  qaTaskRender();
  return true;
}
/** 超出 20 条时列表底部那行**说明**（2026-09-22 起取代「查看更多 / 收起」按钮）。
 *  为什么是「说明」而不是「按钮」：用户要的是**固定上限**（「永远最多显示20个」），
 *  并明确给了老对话的找回路径（「历史的能通过搜索或者点击需求跳转找到就行了」）——
 *  所以这里只如实说清「还有多少条、去哪找」，不再提供一枚能突破上限的入口。
 *  ⚠️ 只改可见条数，**一条都不删**：历史全在 tasks.db / requirements 表里，搜索与需求跳转都能到。 */
function qaTaskCapNote(total, hidden){
  if(total <= QA_TK_VISIBLE || hidden <= 0) return "";
  return `<div class="tk-note" data-testid="tk-cap-note">仅显示最近 ${QA_TK_VISIBLE} 条 · `
    + `还有 ${hidden} 条更早的（在上方搜索里找，或在需求看板点卡片跳过去）</div>`;
}
/** 搜索结果那头一行（跨档平铺时用，取代胶囊 tab —— tab 的计数不含搜索条件，摆在上方只会误导）。 */
function qaTaskHitNote(hits, shown){
  const q = String(qaTkQuery || "").trim();
  return `<div class="tk-note" data-testid="tk-search-note">搜索「${escHtml(q)}」· 命中 ${hits} 条`
    + (shown < hits ? ` · 只显示最近 ${QA_TK_VISIBLE} 条` : "") + `</div>`;
}
/** 一行任务：标题 / 图钉(置顶) / 状态 / 引擎 / 时间 / 行尾下拉 ⋯。
 *  ⚠️ 行尾 ⋯ 是**行按钮内部的 span**（与它取代的 × 同款）：嵌套 <button> 是非法 HTML，
 *  点击仍走 #nav-tasks 上的事件委托，先于 .tk-item 分支命中（见 bindNavTasks）。 */
function qaTaskRowHtml(t){
  return `<button class="tk-item${t.id===qaActiveId?" active":""}" data-id="${t.id}" type="button" title="${escHtml(t.title)}">
      <span class="tico">${QA_TK_ICON}</span>
      <span class="tt">${escHtml(t.title)}</span>
      ${t.pinned ? QA_TK_PIN : ""}
      ${qaTaskSt(t)}
      <span class="tk-eng">${escHtml(t.engine||"claude")}</span>
      <span class="tm">${qaTaskTime(t.ts)}</span>
      <span class="tk-dots" data-menu="${escHtml(t.id)}" data-testid="tk-dots"
        title="更多操作 · ${qaTkMenuItems(t).map(x=>x[1]).join(" / ")}">${QA_TK_DOTS}</span>
    </button>`;
}
/** 「每档永远最多 20 条」这条规则的应用（见文件头 ②）：返回本档本次要渲染的行 + 超出的条数。
 *  ① 先取排序后的前 `QA_TK_VISIBLE` 条；
 *  ② **保留规则**（当前会话 / 刚被跳转定位的那条 / 正在处理的 / 置顶的）优先占名额 ——
 *     被挤出 20 名的保留项**顶掉最老的普通行**（不是把上限撑破：上限是硬的，见 ②）。
 *  ⚠️ 返回的 list **仍按原顺序**（置顶在前 → ts 倒序），顶名额只换「谁留下」，不动排序。 */
function qaTaskFold(rows){
  const keepRule = (t)=> t.id === qaActiveId || t.id === qaTaskRevealId
    || t.status === "running" || !!t.pinned;
  if(rows.length <= QA_TK_VISIBLE) return { list: rows, hidden: 0 };
  const take = new Set(rows.slice(0, QA_TK_VISIBLE).map(t=>t.id));
  rows.filter(t=>keepRule(t) && !take.has(t.id)).forEach((t)=>{
    for(let i = rows.length - 1; i >= 0; i--){       // 从最老的一头找可顶掉的行
      const c = rows[i];
      if(take.has(c.id) && !keepRule(c)){ take.delete(c.id); take.add(t.id); break; }
    }
  });
  const list = rows.filter(t=>take.has(t.id));
  return { list, hidden: rows.length - list.length };
}
function qaTaskRender(){
  const box = $("#nav-tasks"); if(!box) return;
  // workspace 过滤：左栏「任务 · 对话」只列当前工作目录下的会话
  //（过滤只影响显示 —— qaTasks 仍持全量，避免切目录时丢掉其他目录的 DOM 快照）
  // 排序按 ts 倒序（置顶优先）：「最新的 20 个」以时间为准，不依赖数组插入顺序
  const all = qaTasks.filter(t=>wsMatch(t.workspace)).slice().sort(qaTaskOrder);
  if(!all.length){
    htmlWrite(box, `<div class="tk-empty">${qaTasks.length
      ? "该工作目录下还没有对话 —— 在首页发送第一条任务"
      : "还没有对话 —— 在首页发送第一条任务"}</div>`);
    return;
  }
  /* 搜索态（关键词非空）：**跨档平铺命中行**（见文件头 ④）——
     此刻「当前是哪一档」不参与显示（tab 栏整体让位给那行命中计数：
     tab 上的计数不含搜索条件，摆在上方会让人以为「搜索也按档筛选」）。 */
  const searchKey = String(qaTkQuery || "").trim();
  let html;
  if(searchKey){
    const hits = all.filter(qaTkHit);
    const fold = qaTaskFold(hits);
    html = qaTaskHitNote(hits.length, fold.list.length);
    html += hits.length
      ? fold.list.map(qaTaskRowHtml).join("") + qaTaskCapNote(hits.length, fold.hidden)
      : `<div class="tk-empty" data-testid="tk-search-empty">没有匹配「${escHtml(searchKey)}」的对话 —— 换个关键词，或去需求看板点卡片跳到那条对话</div>`;
  } else {
    /* 一次分组、各档一份：**归组互斥**（qaTaskGroupOf），所以「审查中」的那几条
       不会同时出现在「已完成」或「处理中」里 —— 这正是用户 2026-09-20 报的那件事。
       计数与各档实际条数同源（同一个 groups 对象），不会出现「tab 上写 3 条、点进去只有 2 行」。
       三档状态只装**需求派发的会话**（计数与需求看板的列对得上），普通对话进「对话」档。 */
    const groups = { active: [], review: [], done: [], chat: [] };
    all.forEach(t=>{ groups[qaTaskGroupOf(t)].push(t); });
    const counts = { active: groups.active.length, review: groups.review.length,
      done: groups.done.length, chat: groups.chat.length };
    /* 「对话」档空时不渲染那枚 tab —— 用户正停在这一档（最后一条普通对话被删 / 被归档走了）时
       要退回「处理中」，否则会停在一档已经不存在的视图上（tab 栏里没有它、列表也不该显示它）。 */
    if(!counts.chat && qaTaskTab === "chat") qaTaskTab = "active";
    const rows = groups[qaTaskTab] || [];
    const fold = qaTaskFold(rows);
    html = qaTaskTabsHtml(counts);
    html += rows.length
      ? fold.list.map(qaTaskRowHtml).join("") + qaTaskCapNote(rows.length, fold.hidden)
      : qaTaskEmptyHtml(qaTaskTab);
  }
  /* 幂等写（见 01-core.js 的 htmlWrite）：**渲染结果没变就不碰 DOM**。
     运行中每 700ms 一次的看板同步（qaReqConvSync → qaTaskRender）都会调到这里，
     而正在跑的那条的**行内容**其实没变（变的是会话快照，快照不上行）——
     整份重建纯属白做，还会打散 hover 态与行尾菜单锚点、让条目在「最近 20 条」的边界线上
     反复进出（用户 2026-09-19：「处理中的任务显示一直在变 一会消失一会又显示在第一条」）。
     写了才做下面那步锚点补偿：没重建就不存在「⋯ 是新节点」的问题。 */
  if(!htmlWrite(box, html)) return;
  /* 列表被整份重建后，行尾菜单的锚点（那枚 ⋯）是**新节点** —— 不补这一步会出现
     「菜单还浮在原位、但行尾已经没有 ⋯ 是热的」这种脱钩态。锚点还在就补回打开态，
     锚点随这次重绘消失（归档/切目录/被折叠）就顺手收掉菜单。 */
  if(qaTkMenuFor !== null){
    const dots = box.querySelector(`.tk-dots[data-menu="${qaTkMenuFor}"]`);
    if(dots) dots.dataset.open = "1"; else qaTkMenuClose();
  }
}
/** 新对话开始：登记任务（标题 = prompt 首行压缩截断；引擎/模型/会话id/状态随任务落库。
 *  sessionId 此时为空 —— 首问由引擎新建会话，result 事件回传 id 后经 qaSessionBind 写回）
 *  @param {string} model 这条任务用哪个模型（调用方按 `qaModelFor(from, ...)` 算好传来，
 *    见 04-task-list.js）。**1 任务 = 1 模型**：它从此就是这条任务的模型，任务对话里只显示不改。
 *    缺省回落 qaModel（老调用方 / 极端兜底），不编值。 */
function qaTaskBegin(title, model){
  const clean = String(title || "").replace(/\s+/g, " ").trim().slice(0, 30);
  const m = String(model == null ? qaModel : model || "");
  const id = "qa-" + Date.now();
  const t = { id, title: clean || "新对话", snapshot: "", ts: Date.now(),
              engine: qaEngine, model: m, sessionId: "", status: "running",
              workspace: wsDirOf() };      // 归属当前工作目录（切换 workspace 时按它过滤）
  qaTasks.unshift(t);
  qaTasksTrim();                       // 上限 = 淘汰**最老的**（见 04-task-list.js 的 qaTasksTrim）
  qaActiveId = id;
  /* 左栏跟着切到「对话」档（2026-09-23 用户：「跳转对话框的时候 队列列表的tab也要自动切换到正确状态的tab」）。
     新会话不是需求派发的（无 reqId → qaTaskGroupOf 恒为 "chat"），若此刻 tab 停在
     「处理中 / 审查中 / 已完成」档，刚开的这条在左栏里**看不见**，观感是「发出去就没了」。
     统一走 qaTaskReveal（切档 + 露行 + 顺带清搜索），与看板跳转/派发自动进入同一套机制；
     前面刚 unshift 的这条 ts 最新、必在名额内，reveal 只是保个险。 */
  try{ qaTaskReveal(id); }catch(e){ qaTaskTab = "chat"; qaTaskRender(); }
  /* 显示值跟上这条任务（生成中胶囊 / ＋菜单提示 / 只读标签都读 qaModel）——
     任务对话那枚只读标签显示的正是「这条任务用的模型」。 */
  if(typeof qaModelSync === "function") qaModelSync();
  qaTaskRender(); qaTaskPersist(); qaDbCreate(t);
}
/* opts.touch === false：**只存快照，不刷新「最近活动时间」**。
   为什么必须区分（2026-09-18 用户：「点任务对话的任务时顺序会变化，不应该变化」）：
   左栏顺序 = ts 倒序（qaTaskRender），而 ts 同时是行尾那个时间戳。切会话若顺手刷新 ts，
   刚被切走的那条会瞬间跳到列表最上面 —— 顺序随每次点击乱跳，落库还会连 updated_at 一起污染。
   所以「纯浏览（切会话）」不 touch，「真的有新内容（发送 / 流式收尾 / 失败）」才 touch。 */
function qaTaskSaveSnapshot(status, opts){ qaSaveSnapshotFor(qaActiveId, status, opts); }
/** 按**指定会话**存快照（流式 end / 失败 / 切换前 / 后台那条跑完时都用它）。
 *  ⚠️ 两个坑：
 *   ① 画布可能被摘到后台 run 的 holder 里（用户切走了）→ 必须从 holder 取 HTML，
 *      否则存下来的是**当前显示的那个会话**的画面（张冠李戴）；
 *   ② 这条会话**还在跑**时不许把状态从 running 改写成 done（切走时就是这么调的）——
 *      判据是「它有没有在飞的 run」，而不是调用方传没传 status。 */
function qaSaveSnapshotFor(taskId, status, opts){
  if(!taskId) return;
  const t = qaTasks.find(x=>x.id===taskId); if(!t) return;
  const run = qaRunOfTask(taskId);
  // 落笔处：优先用调用方给的 holder（**收尾**时 run 可能刚被摘出注册表，但画布还留在 holder 里，
  // 不显式传就会去读「当前显示的那条会话」的画布 —— 张冠李戴）。
  const holder = (opts && opts.holder) || (run && run.holder) || null;
  const off = !!holder;
  const s = off ? holder : $("#qa-stream");
  t.snapshot = (s && (off || !s.hidden)) ? s.innerHTML : "";
  if(status) t.status = status;
  /* 「这一轮跑完但状态还是 running」→ 落 done：**只对普通对话**成立。
     需求派发出来的会话（`t.reqId` 非空）的状态**由需求说了算**（1 对 1，见 qaTaskStatusOf）——
     需求还在跑（`status='running'`）时，这里把会话改成 done 就等于「需求卡写着处理中、左栏那行
     写着已完成」，正是用户 2026-09-20 报的那种不一致；而且这条会话此刻**可能压根没有 qaRun**
     （壳重载过 / 事件对不上号 / 是另一个 workspace 的派发），`!run` 这个判据会误判成「跑完了」。
     需求侧的状态变化由看板同步刷（`qaReqConvSync`），这里绝不越权改写。 */
  else if(!t.reqId && t.status === "running" && !run) t.status = "done";
  const touch = !(opts && opts.touch === false);
  if(touch) t.ts = Date.now();
  qaTaskPersist(); qaDbUpdate(t, touch ? null : { touch:false });
  /* 消息台账（2026-09-22）：**持久层存的是消息，不是上面那份 HTML 快照**（见 27-conv-log.js）。
     收尾的五个点（end / error / 停止 / 调用失败 / 切走）全都汇到这里，所以在这里收口 ——
     只写一处，漏不掉。push 自带幂等（同人同文不重复记、正文变长改写末尾那条），
     所以「中途切走只拿到半截正文」与「收尾被调用多次」两种情形都不会写脏。 */
  if(typeof qaConvTurnSync === "function"){
    const r = run || qaRunOfTask(taskId);
    /* 过程步骤（2026-09-23）：随消息一起落库。steps 用 run.stepsSaved 记「已保存到第几条」——
       收尾点可能被调多次（end 之后又 stop、中途切走后跑完），只传没落过库的增量，
       重复收尾不会把同一段思考/工具写两遍。 */
    let steps = null;
    if(r && Array.isArray(r.logSteps)){
      steps = r.logSteps.slice(r.stepsSaved || 0);
      r.stepsSaved = r.logSteps.length;
    }
    qaConvTurnSync(taskId, r ? r.userText : "", r ? r.msgText : "", steps);
  }
}
function qaSessionBind(sessionId){ qaSessionBindFor(qaActiveId, sessionId); }
/** 把 magic-agent 返回的会话 id 写回**指定会话**（生成中切走、那条后台跑完时也要写对，
 *  绝不能写「当前显示的那条」—— 否则会话锚点会挂到别的会话上）。 */
function qaSessionBindFor(taskId, sessionId){
  // 把 magic-agent 返回的会话id 写回当前任务：localStorage 快照 + SQLite 同步落库，
  // 追问时经 ask({ sessionId }) 续接同一会话（1 任务 = 1 会话的核心链路）。
  if(!sessionId || !taskId) return;
  const t = qaTasks.find(x=>x.id===taskId); if(!t || t.sessionId === sessionId) return;
  t.sessionId = sessionId;
  if(t.sidLost) delete t.sidLost;           // 拿到 id → 不再需要 `-c` 兜底
  qaTaskPersist(); qaDbUpdate(t, { sessionId });
}
/* 本轮结束（成功/失败/被停）后仍没有会话 id → 记一笔 sidLost。
   为什么要记：`session_id` 只在 CLI 的 result 行里给，中途被停/超时的那轮永远拿不到它；
   没有这个标记，下一轮追问就只能从零开一个新会话（引擎失忆 = 用户看到的「没了 session」）。
   有这个标记 → 追问时下发 `-c`，续接该目录最近一次会话（引擎自己那条），至少接得回去。 */
function qaTaskMarkSidLost(){ qaTaskMarkSidLostFor(qaActiveId); }
function qaTaskMarkSidLostFor(taskId){
  const t = qaTasks.find(x=>x.id===taskId); if(!t) return;
  if(t.sessionId){ if(t.sidLost) delete t.sidLost; } else t.sidLost = 1;
  qaTaskPersist();
}
/* 没有会话 id 时要不要带 `-c`（续接「该目录最近一次会话」）：
   ① 必须**真的跑过一轮且没拿到 id**（sidLost）—— 新建会话的第一轮绝不带，
      否则会把同目录里别的会话接过来（串台）；
   ② 引擎门槛：magic-agent 的 `-c` 在本机 CLI 系引擎（claude / codebuddy / trae / codex / llm）
      成立；arkclaw 明确不支持（CLI 帮助原文：「A2A 无『查询最近上下文』接口，请显式传 --session」），
      openclaw 的会话与 agent 绑定、连 cwd 都被忽略 → 这两个不下发，维持原行为；
      codebuddy-gateway 同样不支持（网关没有「查询最近会话」的接口，CLI 会显式报错）→ 也不下发。
   ⚠️ 若上游 CLI 以后在 `--engines` 里给出这类能力字段，这张表应换成读字段（别长期硬编码）。 */
const QA_CONTINUE_ENGINES = ["claude", "codebuddy", "trae", "codex", "llm"];
function qaContinueRecent(t){
  if(!t || t.sessionId || !t.sidLost) return false;
  return QA_CONTINUE_ENGINES.indexOf(String(qaEngine || "")) >= 0;
}
/* 切入任务对话界面时，把该任务自己的 **engine / model** 还原回当前上下文，
   并同步两处下拉（home2=工作台首页 / home=任务对话）。
   为什么模型也要还原（2026-09-21 用户定稿：「还是都可以选择模型 但是自动切换模型的时候
   对话框下拉的选中模型显示也要变」）：**1 任务 = 1 会话 = 1 模型** —— 模型在新建任务那一刻
   定下（工作台首页那枚下拉 / 或「自动」），进了这条任务的对话可以在那枚下拉里改（改的是
   **这条任务**），自动切换时也会被主进程改掉。
   不还原的话：点了 A 任务、下拉却显示着 B 任务（或首页）的模型 —— 界面与行为对不上。 */
function qaTaskApplyModel(t){
  if(!t) return;
  const eng = t.engine || "claude";
  if(eng !== qaEngine){
    qaEngine = eng;
    try{ localStorage.setItem("qaEngine", qaEngine); }catch(err){}
    /* 引擎变了 → 该引擎的模型链必须重取（「自动」档用链首，且链是跟着引擎走的）。
       ⚠️ 这里**不 await**：本函数在「点侧栏那条会话」的链上是同步调用的，阻塞一次 IPC
       会让切会话卡一下；链取回后显示值落定，随后的发送自然用新值。 */
    if(typeof qaModelChainLoad === "function") qaModelChainLoad(qaEngine, true);
  }
  /* 显示值 = 这条任务自己的模型（`qaModelSync()` 走 qaModelFor("home", qaActiveId)） */
  if(typeof qaModelSync === "function") qaModelSync();
  /* ⚠️ qaPickerSync 是**绑定函数内部的局部 const**（在下面定义），全局作用域的
     qaTaskApplyModel 直接取会抛 ReferenceError —— 后果是「点侧栏任务毫无反应」：
     异常在切视图/还原画布/重画左栏那几行之前就把整条链打断了。
     实测（2026-09-18 用户：「点击任务对话的任务时 现在顺序会变化 不应该变化」）：
     点一条**引擎与当前不同**的会话（例：当前 codebuddy，点一条 claude 的）时抛
     「qaMdlMenuBuild is not defined」—— qaActiveId 已经改了、qaEngine 也切了，但画布没还原、
     视图没切、左栏也没重画 → 列表里那条行尾时间/高亮与真实状态错位，看起来像「点了顺序变了」。
     按本项目既有惯例（见 showView / __qaMenuClose）守卫 + 定义处补一次全局暴露，
     两头都做 → 无论谁先执行都不会抛。更彻底的根治仍是把它提到全局作用域。 */
  if(typeof qaPickerSync === "function") qaPickerSync();   // 同步两处按钮文案 + 菜单 .on 高亮
}
/** 把**某条任务**的模型改成主进程实际在跑的那个（2026-09-21）。
 *  什么时候会发生：① 配置里的模型链自动降级了 —— 主进程换了下一个候选并跑通
 *  （`start` 事件带回实际模型）；② 用户在那条任务的对话里手选了一个（`qaModelSet` 走这里）。
 *  必须把任务上记的模型改掉，否则：
 *    · 任务对话那枚下拉还写着那个已经跑不通的模型（用户看到的与真跑的对不上）；
 *    · 下次追问又先试那个死掉的模型，白烧一次失败尝试。
 *  与需求派发侧 `reqboard::promoteWorkingModel` 是同一条口径（那边同步的是需求自己的 model）。
 *  ⚠️ 只在**真的不同**时才写库（每轮都写一次是纯噪声，还会把 ts 搅动）。
 *  ⚠️ `model === ""` 是**合法值**（用户在那条任务的下拉里选了「自动」）→ 把任务上的模型清空，
 *     之后按 `qaModelFor` 回落「自动」（配置链的链首）。别拿「空串 = 没传」把它挡掉。 */
function qaTaskSetModel(taskId, model){
  const t = Array.isArray(qaTasks) ? (qaTasks.find(x=>x.id === taskId) || null) : null;
  const m = String(model || "");
  if(!t || String(t.model || "") === m) return false;
  t.model = m;
  qaTaskPersist(); qaDbUpdate(t, { touch:false });   // 改模型不算「一次新活动」→ 不动 updated_at（左栏顺序不跳）
  if(t.id === qaActiveId) qaModelSync();   // 正在显示的这条 → 下拉文案与选中项当场跟着改
  return true;
}
/** 无快照时的兜底画面：按这条会话所属的**需求**（`reqId`）现拼一份简版。
 *  为什么需要（2026-09-20）：左栏的条数与计数自本次起 = **库内全量**（见 04-task-list.js
 *  的文件头），而 `QA_TK_MAX` 只决定「几条会话**带快照**」—— 被摘掉快照的历史会话
 *  （以及刚重启还没从 localStorage 取回快照的那几条）点开时快照是空的，
 *  没有这层兜底就是一片空白，看着像「会话内容丢了」。
 *  兜底只对**需求派发**的会话成立（`qa-req-<需求id>`，需求正文与逐轮答复都在 reqItems 里）；
 *  手动对话没有需求可依 → 返回空串，维持原来的空态画布（那是真的没有历史可还原）。 */
function qaTaskRebuildSnap(t){
  if(!t || !t.reqId) return "";
  try{
    const it = ((typeof reqItems !== "undefined" && reqItems) || []).find(x=>x.id === t.reqId);
    return it ? qaReqConvSnapshot(it) : "";
  }catch(e){ return ""; }
}
/** 把**某条会话的历史**画进画布（点当前会话 / 切到别的会话共用这一份）。
 *  三级找回，逐级降级（口径与实现在 27-conv-log.js 的文件头）：
 *    ① `t.snapshot` —— **本会话内存里的画面**：最快，且**保真**（工具卡 / 思考区 / 截图 / 提问卡都在）。
 *       它是**会话内的画面缓存**，不是历史来源（历史来源只有一处：SQLite 的消息表）。
 *    ② `qaTaskHistoryRecover` —— **SQLite 消息表**（按 task_id 读）→ 重画；
 *       需求派发的会话则由需求表现拼（`qaTaskRebuildSnap`，那份数据同样在 SQLite 里）。
 *    ③ 都没有 → 画一条**说明**（`qaConvLostText`），说清为什么看不到。
 *  ⚠️ **没有第四级兜底**（用户 2026-09-22：「不需要兜底 就是历史读 sqllite」）：
 *     曾经读过引擎自己的会话文件（`<会话id>.jsonl`）当最后一级兜底，已按要求删除。
 *
 *  ⚠️ ②是异步的：先画一条「正在恢复…」占位，结果回来时**只在用户还停在这条会话上**才落笔
 *     （`qaActiveId` 变了就丢弃 —— 否则会把 A 的历史画到 B 的会话里）。
 *  ⚠️ 调用方负责传对 `replace`：切到别的会话 = 换掉画布（true）；
 *     点当前会话 = 画布已有东西就别清（false，与旧行为一致：非空不动）。
 *  ⚠️ 恢复出来的画面**不写回 `t.snapshot`**：那是「这一次运行里真实渲染过的东西」的缓存，
 *     而恢复的是「按消息重画的近似画面」—— 混进缓存里会让「哪份是真的」说不清。
 *  @returns {boolean} 画布上现在是否有内容 */
function qaTaskPaint(t, s, opts){
  if(!t || !s) return false;
  const replace = !!(opts && opts.replace);
  if(!replace && s.innerHTML) return true;              // 点当前会话：画布非空就不动（旧口径）
  if(t.snapshot){ s.innerHTML = t.snapshot; s.hidden = false; qaDropRunPills(s); return true; }
  const reqSnap = qaTaskRebuildSnap(t);                 // ② 需求派发：按需求现拼（数据在 SQLite 需求表里）
  if(reqSnap){ t.snapshot = reqSnap; s.innerHTML = reqSnap; s.hidden = false; qaDropRunPills(s); return true; }
  /* ② 异步读 SQLite 的消息表：台账先读起来（顺带把这条会话的历史种进内存台账，用户接着追问时接得上） */
  if(typeof qaMsgLogLoad === "function") qaMsgLogLoad(t.id);
  const id = t.id;
  s.innerHTML = qaConvNoticeHtml("正在读取这条对话的历史…", "wait");
  s.hidden = false;
  qaTaskHistoryRecover(t).then((html)=>{
    if(qaActiveId !== id) return;                       // 用户已经切走 → 丢弃这次结果
    const box = $("#qa-stream"); if(!box) return;
    if(html){
      /* 直接把消息画出来 —— **不挂任何来源声明**（用户 2026-09-22：「以下内容来自本地消息库
         （按当前界面重新绘制） 这个声明不需要」）：历史本来就是这条会话自己的，读者不需要
         被告知它是从哪儿读出来的；只有**真的没有**历史时才说话（见下面的 lost 分支）。 */
      box.innerHTML = html;
      box.hidden = false; qaDropRunPills(box);
    }else{
      box.innerHTML = qaConvNoticeHtml(qaConvLostText(t), "lost");
      box.hidden = false;
    }
    qaSyncChatMode(); qaScrollIntoView();
  }).catch((e)=>{ console.warn("[conv] 历史读取失败：", (e && e.message) || e); });
  return true;
}
function qaTaskSwitch(id){
  if(id === qaActiveId){
    qaTaskRevealId = "";                            // 定位的临时名额已无用（它本来就是当前会话）
    qaTaskApplyModel(qaTasks.find(x=>x.id===id));   // 当前任务也确保选择器显示其模型
    // 点击当前活动任务 ≠ 无操作：仍要保证「看见对话」——切视图 + 快照就地恢复。
    // （2026-09-16 用户反馈：仅一条任务时点侧栏毫无反应，历史看不见 —— 早退太激进）
    const t = qaTasks.find(x=>x.id===id);
    const s = $("#qa-stream");
    if(t && s) qaTaskPaint(t, s, { replace:false });
    if(typeof showView === "function" && state && state.view !== "chat") showView("chat");
    qaSyncChatMode();
    qaScrollIntoView();
    qaAskFlush(id);                             // ⑤ 切回这条会话：排队等发的提问卡答案现在可以发了
    qaMsgQueueRender(id);                       // 队列条目跟着会话走：画回这一条的待发清单
    qaMsgFlush(id);                             // 排队的下一条：这条已空闲就自动提交
    return;
  }
  /* 生成中**也允许切换**（2026-09-18 用户：「任务列表点击显示回答生成中 不能切换 为什么呢？
     肯定可以切换另1个会话去啊」）。老代码在这里硬拦，因为画布只有一份、增量会写串。
     现在把「正在跑的那条」的画布**整份摘到它自己的 holder**：节点不销毁 →
     后台增量继续写进这些节点，切回来直接挂回 #qa-stream，内容比快照还新。
     ⚠️ 不许在这里把「还在跑的那条」状态改写成 done —— qaSaveSnapshotFor 按「有没有在飞的 run」判断。 */
  const prevRun = qaRunOfTask(qaActiveId);
  qaTaskSaveSnapshot(null, { touch:false });   // 先存旧的（纯切会话：不刷新 ts → 左栏顺序不动）
  if(prevRun){
    if(!prevRun.holder) prevRun.holder = document.createElement("div");
    const prev = $("#qa-stream");
    if(prev) while(prev.firstChild) prevRun.holder.appendChild(prev.firstChild);   // 画布跟着会话走
  }
  const t = qaTasks.find(x=>x.id===id); if(!t) return;
  qaActiveId = id;
  qaTaskRevealId = "";                         // 定位的临时名额交还给 qaActiveId（保留规则里的同一档待遇）
  qaTaskApplyModel(t);                         // 还原该任务的 engine/model 到选择器
  const s = $("#qa-stream");
  const run = qaRunOfTask(id);
  if(s){
    if(run && run.holder){                     // 切回一条**还在跑**的会话：把它的画布挂回来
      while(run.holder.firstChild) s.appendChild(run.holder.firstChild);
      run.holder = null;                       // 回到可见画布 → holder 交还（qaRunHost 自动改回落笔处）
      s.hidden = false;
    } else qaTaskPaint(t, s, { replace:true });
  }
  qaSyncBusy();                                // 撰写区文案跟着「当前显示的是哪条」重算
  // 切到「任务对话」视图（用户在别的视图里点会话也要看见聊天窗口）
  if(typeof showView === "function" && state && state.view !== "chat") showView("chat");
  // 按快照有无内容决定两态（空态气泡引导 / 对话态撑满）
  qaSyncChatMode();
  qaScrollIntoView();
  qaTaskRender();
  /* ⑤ 提问卡排队等发的答案：**放在画布切换之后**（早于它，qaSend 画的气泡会被
     紧接着的 `s.innerHTML = t.snapshot` 整份抹掉）。此刻 qaActiveId 已是这条会话，
     qaAskFlush 的「正看着这条吗」判据成立。
     ⚠️ 待发队列同一条道理，也放在这里：先重画「这一条的待发清单」，空闲才自动提交下一条。 */
  qaMsgQueueRender(id);
  qaAskFlush(id);
  qaMsgFlush(id);
}
/* ---------- 左栏定位（需求看板点卡片 → 跳到那条对话，2026-09-20）----------
   用户原话：「点击需求看板中的任务可以定位跳到对话」。
   「定位」不只是 qaTaskSwitch（它负责切视图 + 还原画布 + 给行挂 .active）——
   还得**把那一行真的露出来**：左栏有两层（tab 切档 / 20 条上限），
   一条旧会话完全可能既不在「处理中」的前 20 条里、又躺在默认不显示的那两档里
   （审查中 / 已完成）。只说「切过去了」而那一行看不见，用户会认为没反应。
   ⚠️ 归属哪一档**必须问 qaTaskGroupOf**，别自己按 archived 判 —— 「已完成」现在是
   「跑完的 ∪ 归档的」，「审查中」还优先于它（见该函数）。
   ⚠️ 20 条上限是**硬的**（2026-09-22 起没有「查看更多」能突破它）→ 露出来的办法是
   ① 切到它所在的档、② 让 qaTaskRevealId 在本次渲染里优先占一个名额（qaTaskFold 的保留规则）。
   不做「临时把上限撑大」这种事：那会让「永远最多 20 个」这条契约当场失效。
   ⚠️ 顺带**清掉搜索词**：带着搜索条件跳过去，目标行会被过滤掉（左栏空、画布却切了）。 */
function qaTaskReveal(id){
  const t = qaTasks.find(x=>x.id===id); if(!t) return false;
  qaTkQueryReset();
  qaTaskTab = qaTaskGroupOf(t);       // ① 切到它所在的那一档
  /* ② 让它在本次渲染里**优先占一个名额**（qaTaskFold 的保留规则）——本档 >20 条时，
     它可能落在上限之外，不置位就是「切过去了但那一行看不见」。 */
  qaTaskRevealId = id;
  qaTaskRender();
  return true;
}
let qaTkFlashTimer = null;
/** 把定位到的那一行滚进视口并闪一下（1.5s 后自动褪去）。
 *  为什么要闪：跳转后左栏可能只是多了一行 `.active`（几十行里很难一眼认出是哪条）。
 *  ⚠️ 每次先摘类再强制重排，否则连点两条时第二条不会重播动画（动画只在加类的那一刻触发）。 */
function qaTaskFlash(id){
  const el = document.querySelector(`#nav-tasks .tk-item[data-id="${id}"]`);
  if(!el) return false;
  try{ el.scrollIntoView({ block:"nearest", behavior:"smooth" }); }
  catch(e){ try{ el.scrollIntoView(); }catch(err){ /* 老引擎没有这个 API */ } }
  el.classList.remove("tk-flash");
  void el.offsetWidth;
  el.classList.add("tk-flash");
  clearTimeout(qaTkFlashTimer);
  qaTkFlashTimer = setTimeout(()=>{
    const n = document.querySelector("#nav-tasks .tk-item.tk-flash");
    if(n) n.classList.remove("tk-flash");
  }, 1500);
  return true;
}
function qaTaskDelete(id){
  const i = qaTasks.findIndex(x=>x.id===id); if(i < 0) return;
  const gone = qaTasks[i];
  qaTasks.splice(i, 1);
  qaDbDelete(id);
  if(gone && gone.reqId) qaReqConvDropMark(gone.reqId);   // 需求派发出来的会话：删了别再自动建回来
  qaMsgQueueDropFor(id);                     // 这条会话排的待发条目一并丢掉（不留悬挂队列）
  if(qaActiveId === id){
    qaActiveId = null;
    const s = $("#qa-stream"); if(s){ s.innerHTML = ""; s.hidden = true; }
    qaSyncChatMode();
    // 自动切到最近一条（若有）
    if(qaTasks.length) qaTaskSwitch(qaTasks[0].id);
  }
  qaTaskRender(); qaTaskPersist();
  toast("已删除该对话");
}
/* ---------- 置顶 / 归档（用户 2026-09-18：「也支持置顶」+「列表支持分组 处理中和已归档」）----------
   两者都**只改显示归属，不动 ts**：行尾时间与「最新 5 条」的基准必须原样保留，
   所以落库走 patch.touch:false（同「切会话只存快照」那条契约）—— 动了 updated_at，
   重启后这条会话的时间戳就变成了「你点置顶的那一刻」，明显是错的。
   归档也不改 status：它表达的是「用户把这条收起来了」，不是「这条跑成什么样了」。
   ⚠️ 2026-09-20 起那一档改叫「已完成」（用户：「已归档改成已完成」）：**跑完的（status=done）
   本来就在里面**，归档是给「还没跑完但我不想再看见它」的那几条留的出路（失败 / 被停 / 排队中）
   —— 所以这枚动作依旧有用，不是个空按钮。归档过的会话归属见 qaTaskGroupOf。

   ⚠️ 落库**必须验结果**（用户 2026-09-20 报障：「点击归档无效果 重启又丢失了
   没更新sqllite中状态吗」）：这一支以前不接 qaDbUpdate 的返回值，而主进程回 {ok:false}
   （典型原因：tasks.db 里没有这一行 —— 老库没同步 / 上次 create 失败 / 库被清过）
   时界面照样归了档、库里还是 0，重启时 qaDbLoad 以库为准一覆盖，归档就**静默没了**。
   现在的三步：① 照常 update；② 回 {ok:false} → 用 create 把这一行**补建**出来再 update 一次；
   ③ 还写不进去 → 内存与 localStorage 一起**回滚**（不留「界面归了档、库里没有」的错位），
   并明说这次没写进库 —— 宁可让用户看见失败，也不假装成功。 */
async function qaTaskFlag(id, field, on, msg){
  const t = qaTasks.find(x=>x.id===id); if(!t) return;
  const prev = t[field];
  if(on) t[field] = 1; else delete t[field];
  qaTaskPersist();
  qaTaskRender();
  const patch = { touch:false }; patch[field] = on ? 1 : 0;
  let r = await qaDbUpdate(t, patch);
  if(!r.ok && !r.skipped){
    await qaDbCreate(t);                       // 库里没这一行 → 补建（已存在则它自己回 ok:false，无害）
    r = await qaDbUpdate(t, patch);
  }
  if(!r.ok && !r.skipped){
    if(prev === undefined) delete t[field]; else t[field] = prev;
    qaTaskPersist();
    qaTaskRender();
    toast(actionLabel(field, on) + "未写入本地数据库，已撤销");
    return;
  }
  if(msg) toast(msg);
}
/** 动作文案（成功 toast 与失败提示同一份口径，别两处各写一遍） */
function actionLabel(field, on){
  return field === "pinned" ? (on ? "置顶" : "取消置顶") : (on ? "归档" : "移出归档");
}
function qaTaskPin(id, on){ qaTaskFlag(id, "pinned", on, on ? "已置顶该对话" : "已取消置顶"); }
function qaTaskArchive(id, on){ qaTaskFlag(id, "archived", on, on ? "已归档该对话" : "已移出归档"); }
/* ---------- 行尾下拉菜单（#tk-menu，挂 body 的单例）----------
   2026-09-18 用户：「每一个对话行尾有下拉操作选择」—— 行尾 ⋯ 点开 = 置顶 / 归档 / 删除。
   ⚠️ 单例而不是「每行一只菜单」：菜单必须挂 body（侧栏 #nav 是 overflow:hidden，absolute 层
   会被整块裁掉），一屏几十行就几十个浮动节点；单例在开合之间只换内容 + 重定位。
   ⚠️ 定位必须走 qaMenuPlace（在 bindHomeEntry 里定义，经 window.__qaMenuPlace 取用），
   不在这里手写 left/top：翻转 / 视口夹持 / 滚动与尺寸变化的 autoUpdate 都在那儿统一实现。 */
const TK_MENU_SEL = "#tk-menu";
const TK_MENU_PLACE = { placement:"bottom-start", strategy:"fixed" };
let qaTkMenuFor = null;            // 当前开着的行尾菜单属于哪条会话（null = 没开）
/** 菜单条目 [动作, 文案, 是否危险色]：按该行当前状态给「反向」文案
 *  （已置顶 → 取消置顶 / 已归档 → 移出归档），删除恒为危险色。
 *  ⚠️ 「归档」那一项**只在这枚动作真会改变归属时才出**（2026-09-20 起左栏分档是**状态优先**，
 *  归档只能把「不再跑、又不在已完成档」的那两类（失败 / 已停止）收进「已完成」）：
 *    · `done` 的行本来就在「已完成」档里 —— 再给它一枚「归档」= 点了什么都不动
 *      （用户 2026-09-20 报过的那种「点击归档无效果」）；
 *    · 排队中 / 在跑 / 审查中的行由**状态**说了算（见 qaTaskGroupOf），归档挪不动它。
 *  所以只有 `error` / `stopped` 且未归档的行才出这一项。 */
function qaTkMenuItems(t){
  const s = qaTaskStatusOf(t);
  const items = [[t.pinned ? "unpin" : "pin", t.pinned ? "取消置顶" : "置顶", false]];
  if(t.archived) items.push(["unarchive", "移出归档", false]);
  else if(!s.live && s.st !== "done" && s.st !== "verify") items.push(["archive", "归档", false]);
  items.push(["del", "删除", true]);
  return items;
}
function qaTkMenuOpen(id, dots){
  const menu = $(TK_MENU_SEL), t = qaTasks.find(x=>x.id===id);
  if(!menu || !t) return;
  qaTkMenuClose();
  if(typeof window.__qaMenusClose === "function") window.__qaMenusClose(null);  // 别的弹出菜单先收掉
  menu.innerHTML = qaTkMenuItems(t).map(([a, label, danger])=>
    `<button class="ag-model-opt${danger?" danger":""}" type="button" role="menuitem"
      data-tk-act="${a}" data-tk-id="${escHtml(id)}">${label}</button>`).join("");
  menu.hidden = false;
  qaTkMenuFor = id;
  if(dots) dots.dataset.open = "1";
  if(typeof window.__qaMenuPlace === "function") window.__qaMenuPlace(dots, menu, TK_MENU_PLACE);
}
function qaTkMenuClose(){
  const menu = $(TK_MENU_SEL);
  const id = qaTkMenuFor;
  qaTkMenuFor = null;
  if(id){
    const dots = $(`#nav-tasks .tk-dots[data-menu="${id}"]`);
    if(dots) delete dots.dataset.open;
  }
  if(!menu || menu.hidden) return;
  // 关闭统一走 __qaMenuClose（隐藏 + 撤 autoUpdate 监听）；库缺失时退化为自己隐藏
  if(typeof window.__qaMenuClose === "function") window.__qaMenuClose(menu);
  else menu.hidden = true;
  menu.innerHTML = "";
}
function qaTkMenuAct(act, id){
  qaTkMenuClose();
  if(act === "pin") return qaTaskPin(id, true);
  if(act === "unpin") return qaTaskPin(id, false);
  if(act === "archive") return qaTaskArchive(id, true);
  if(act === "unarchive") return qaTaskArchive(id, false);
  if(act === "del") return qaTaskDelete(id);
}
/* ---------- 截图放大查看（lightbox · 单例）----------
   2026-09-18 用户：「对话框中发送的截图的显示要支持」—— 气泡里 168×120 的缩略图看不清内容，
   而截图恰恰是「要看细节」的东西，点一下看原图（对话框附件条上的 40px 缩略图同理）。
   挂 body 的单例，任何视图里都能用；媒体库那个 #bm-viewer 是**视图内部**的（`.bm-viewer{flex:1}`
   顶掉网格），切到对话视图就不可见，不能复用。
   ⚠️ 绑定必须走**事件委托**：#qa-stream 的 innerHTML 会被快照整份重建（同工具卡那条注释）。 */
function openShotViewer(url, name){
  const v = $("#shot-viewer"), im = $("#shot-viewer-img");
  if(!v || !im || !url) return;
  im.src = url;
  im.alt = name || "截图";
  const nm = $("#shot-viewer-name"); if(nm) nm.textContent = name || "";
  v.hidden = false;
}
function closeShotViewer(){
  const v = $("#shot-viewer"), im = $("#shot-viewer-img");
  if(!v || v.hidden) return;
  v.hidden = true;
  if(im) im.removeAttribute("src");     // 大图别一直占着内存
}
let qaShotViewerBound = false;
function bindShotViewer(){
  if(qaShotViewerBound) return;
  const v = $("#shot-viewer"); if(!v) return;
  qaShotViewerBound = true;
  v.addEventListener("click", (e)=>{
    if(e.target.closest && e.target.closest("#shot-viewer-close")){ closeShotViewer(); return; }
    if(e.target === v) closeShotViewer();          // 点背景关；点图本身不关
  });
  document.addEventListener("keydown", (e)=>{
    if(e.key === "Escape" && !v.hidden) closeShotViewer();
  });
  // ① 气泡里发出的截图 + **助手回复正文里的截图**（#qa-stream 会被整份重建 → 只能委托）。
  //    `.qa-t img` 本身已涵盖 `.qa-atts img`（附件条就挂在 .qa-t 里），两个选择器都写是为了
  //    语义自明 —— 将来任一处结构调整时，不必回头重新推导覆盖关系。
  //    2026-09-20：正文图加了尺寸封顶（app.css 的 .qa-t.md-body img），「看原图」就落在这条链上。
  const s = $("#qa-stream");
  if(s) s.addEventListener("click", (e)=>{
    const im = e.target.closest && e.target.closest(".qa-atts img, .qa-t img");
    if(im) openShotViewer(im.getAttribute("src"), im.getAttribute("alt"));
  });
  // ② 对话框附件条上的缩略图（同一个组件两个实例 → 实例清单只有 ASK_INSTANCES 一份）。
  //    绑**宿主挂载点** `#ask-<p>`（它从不被替换，只换 innerHTML）—— 绑 `#<p>-atts` 会在
  //    组件重挂时随旧节点一起丢掉。
  ASK_INSTANCES.forEach((p)=>{
    const host = $("#ask-" + p); if(!host) return;
    host.addEventListener("click", (e)=>{
      const im = e.target.closest && e.target.closest(".ag-att .thumb");
      if(!im) return;
      const nm = im.closest(".ag-att").querySelector(".nm");
      openShotViewer(im.getAttribute("src"), (nm && nm.textContent) || "");
    });
  });
}

