/* ---------- 提问卡 → 回答（AskUserQuestion 的落笔）----------
   2026-09-18 用户：「返回了 askuserquestion，需要 ui 在对话框中增加选择显示 而不是显示这个 json」。
   卡落在对话框里，答案也要回到对话框：点选项 = 把这一问的回答作为**追问**发给同一个会话
   （走 qaSend 的 followup 语义、沿用 sessionId 续接，不在左栏另开一条会话）。

   ⚠️ 发送时机 —— **不打断在跑的那一轮**（用户 2026-09-18 定稿「对话中不允许上一条没结束
   直接输入下1个」；这条会话的撰写区在它自己生成中锁死）：这一轮还在跑就先把答案排进
   qaAskQueue，等它收尾（onDone / onError / 用户点停止）或用户切回该会话时由 qaAskFlush
   自动发出。
   空闲时（引擎早就不等回答了 —— 非交互模式下很常见）点一下立即发出，不排队。
   排队态在卡上已经是「已答」（选中项高亮 + 标题变「已回答」），不会让用户以为点了没反应。 */
const qaAskQueue = new Map();                  // taskId → [等着发出去的答案]（一轮里可能问好几次）
function qaAskAnswered(card){ return !!card && card.dataset.answered === "1"; }
/** 收这一张卡上的答案，形状与 magic-agent 的 `EncodeAskFollowUp` **逐字对齐**：
 *      【用户选择】
 *      - <问题原文> → <答案>
 *  多选多个 label 用 `", "` 连接（Go 侧 askMultiSelectSep）。
 *  为什么必须有这个前缀：答案是以**新的一条 user 消息**追加进会话的（见 agent-cli.cjs 那一节），
 *  没有前缀模型会把它读成一条新需求；两端文案漂移就等于白答。
 *  单问单选也不再只回一个裸标签（原先回的就是「推送到远程」四个字，模型只能靠猜）。 */
function qaAskText(card){
  const qbs = [...card.querySelectorAll(".qa-ask-qb")];
  const lines = qbs.map(qb=>{
    const labels = [...qb.querySelectorAll(".qa-ask-opt.sel")]
      .map(o=>o.dataset.label || "").filter(Boolean);
    if(!labels.length) return "";
    return "- " + (qb.dataset.q || "") + " → " + labels.join(", ");
  }).filter(Boolean);
  return lines.length ? "【用户选择】\n" + lines.join("\n") + "\n" : "";
}
function qaAskAllDone(card){
  const qbs = [...card.querySelectorAll(".qa-ask-qb")];
  return qbs.length > 0 && qbs.every(qb=>qb.querySelector(".qa-ask-opt.sel"));
}
function qaAskSyncSub(card){
  const b = card.querySelector(".qa-ask-sub");
  if(b) b.disabled = !qaAskAllDone(card);
  card.querySelectorAll(".qa-ask-opt").forEach(o=>
    o.setAttribute("aria-checked", o.classList.contains("sel") ? "true" : "false"));
}
/** 标记「这一问已答」并把答案发出去。
 *  text = 真正发出去的内容（`【用户选择】…` 信封，见 qaAskText）；
 *  display = 卡上「你的回答」那一行显示什么（只有「其他」自由输入需要它：
 *  信封里的题面不适合当展示文案；不传就不显示这行，选项高亮本身已经是答案）。 */
function qaAskMark(card, text, display){
  const t = String(text || "").trim(); if(!card || !t) return;
  card.dataset.answered = "1";
  const ttl = card.querySelector(".qa-ask-ttl"); if(ttl) ttl.textContent = "提问 · 已回答";
  const shown = String(display == null ? "" : display).trim();
  if(shown){
    const ans = card.querySelector(".qa-ask-ans");
    if(ans){ ans.hidden = false; ans.querySelector(".v").textContent = shown; }
  }
  qaAskSyncSub(card);
  /* sid / rid 从**卡自己**身上取（dataset，见 askCard）：这样快照恢复出来的历史卡也答得回去。 */
  qaAskDeliver(qaActiveId, t, { sid: card.dataset.sid || "", rid: card.dataset.rid || "" });
}
/** 把答案交给会话：**这条会话**空闲立即发；它自己的那一轮还在跑就排队（收尾/切回时由
 *  qaAskFlush 发出）。别的会话在跑**不拦它**（2026-09-18 晚定稿：锁只跟会话走，跨会话不互锁）。
 *  一轮里被问了多次（多张卡）→ 按先后顺序逐条发：前一条发出后新一轮又跑起来，
 *  后一条继续排队，等那一轮收尾再发（同一把闸门，不绕开「生成中不发下一条」）。 */
function qaAskDeliver(taskId, text, opts){
  const id = taskId || qaActiveId || "";
  const o = opts || {};
  if(qaTaskRunning(id)){
    const q = qaAskQueue.get(id) || [];
    q.push({ text, sid: o.sid || "", rid: o.rid || "" });
    qaAskQueue.set(id, q);
    toast("已选：<b>" + escHtml(text) + "</b><br>这一轮结束后自动发出", true);
    return;
  }
  toast("已选：<b>" + escHtml(text) + "</b>", true);
  qaAskSend(text, o);
}
/** 答案的统一出口：**能追加就追加**（答案落进那条还活着的会话、引擎当下一轮处理），
 *  追加不了就新起一轮 + `--session` 续接（老版 CLI / 会话已结束 / 没有会话 id）。
 *  判据是「拿到 sid 且拿到 rid」：sid 来自统一 ask 事件（tool_use 那条老路没有它）——
 *  所以老引擎自动走回落路径，行为与改造前完全一致。 */
function qaAskSend(text, opts){
  const o = opts || {};
  const sid = String(o.sid || "").trim();
  /* ⚠️ rid 从 dataset 读回来是**字符串**，而事件路由（qaRuns / bindHomeAsk 的 `qaRuns.get(ev.rid)`）
     用的是**数字** —— 不归一化就会「qaRuns.get(1) 查不到 '1'」，追加回来的那一轮整轮被当过期
     事件丢掉（界面表现：追加成功、却永远看不到引擎的回答）。 */
  const ridNum = (o.rid == null || o.rid === "") ? NaN : Number(o.rid);
  if(sid && Number.isFinite(ridNum) && window.desk && typeof window.desk.askAppend === "function"){
    qaSend(text, { followup: true, from: "home", append: { sid, rid: ridNum } });
    return;
  }
  qaSend(text, { followup: true, from: "home" });
}
/** 排队的答案该不该现在发：**这条会话**没有一轮在跑 + 用户正看着这条会话
 *  （qaSend 是按 qaActiveId 落笔的，不能在别的会话的画布里替它发）。
 *  别的会话还在跑**不拦** —— 答案发给的是空闲的这条，并行是会话之间的事。 */
function qaAskFlush(taskId){
  const id = taskId || "";
  const q = qaAskQueue.get(id);
  if(!q || !q.length || qaTaskRunning(id)) return;
  if(id !== (qaActiveId || "")) return;
  const item = q.shift();
  if(!q.length) qaAskQueue.delete(id);
  qaAskSend(item.text, { sid: item.sid, rid: item.rid });
}

/* ---------- 会话排队：处理中提交的下一条 ----------
   用户 2026-09-18：「会话处理中时 改成支持输入 但是新需求排列在会话框上面默认等待上一个
   结束后自动提交」。
   改前：这一轮没跑完，撰写区整块锁死（输入域 disabled + 提交钮变停止钮）—— 想追问只能干等。
   改后：输入域照常可写（qaBusyEx 不再 disabled 它），处理中点发送 = **排进队列**：
     · 条目渲染在**会话框上方**（#home-queue，与附件条同在 .home-entry 里），标「等待中」；
     · 这一轮收尾（onDone / onError / 用户点停止）或用户切回这条会话时，由 qaMsgFlush
       **自动提交队首那一条** —— 这就是用户说的「默认等待上一个结束后自动提交」。
   ⚠️ 一次只放一条出去：每条消息都会起**新的一轮**，一次放多条 = 后一条把前一条杀掉
      （主进程 `desk:ask` 是单会话槽）。剩下的等那一轮收尾再放 —— 队列自己就串起来了。
   ⚠️ 提问卡答案优先：qaAskQueue 里还有没发出去的答案时先不发队列消息（那是引擎正等着的
      回答，抢在它前面发会把这一问的上下文冲掉）。
   ⚠️ 只在**用户正看着这条会话**时发：qaSend 是按 qaActiveId 落笔的（画布只有一份），
      在别的会话的画布里替它发就是张冠李戴 —— 与 qaAskFlush 同一条判据。切回来即发。
   ⚠️ 附件不进队列：处理中贴图仍被拦（见 askAttBind 的 busyGate），所以条目只有文字。 */
const qaMsgQueue = new Map();                  // taskId → [{ text }]
/** 队列条目画在**哪只实例**上：队列跟会话走，而「会话框」= home 实例。
 *  home2 是新建对话入口 —— 它提交的永远是**新会话**，不排队，也不该显示别人的队列。 */
const QA_QUEUE_INSTANCE = "home";
function qaMsgQueueRender(taskId){
  const el = $("#" + QA_QUEUE_INSTANCE + "-queue");
  if(!el) return;
  const id = taskId == null ? (qaActiveId || "") : taskId;
  // 只画「当前显示的这条会话」的队列：切到别的会话时不该看见上一条会话的待发条目
  const list = (id && id === (qaActiveId || "")) ? (qaMsgQueue.get(id) || []) : [];
  el.hidden = !list.length;
  el.innerHTML = "";
  list.forEach((it, i)=>{
    const row = document.createElement("div");
    row.className = "ag-q";
    row.dataset.i = String(i);
    row.innerHTML = `<span class="no"></span><span class="tx"></span>`
      + `<span class="st">等待中 · 上一条结束后自动发送</span>`
      + `<button class="x" type="button" aria-label="取消这条待发消息">×</button>`;
    row.querySelector(".no").textContent = String(i + 1);
    row.querySelector(".tx").textContent = it.text || "";
    /* 直接绑监听（不走 #qa-stream 事件委托）：这一行在画布**之外**，不参与快照重建，
       每次渲染都是新节点 —— 与附件条的 × 同一套做法。 */
    row.querySelector(".x").addEventListener("click", (e)=>{ e.stopPropagation(); qaMsgQueueDrop(id, i); });
    el.appendChild(row);
  });
}
/** 取消一条待发消息（点条目尾的 ×）：列表空了就把这条会话的队列键删掉，不留空数组。 */
function qaMsgQueueDrop(taskId, idx){
  const id = taskId || ""; if(!id) return;
  const q = qaMsgQueue.get(id); if(!q || idx < 0 || idx >= q.length) return;
  q.splice(idx, 1);
  if(!q.length) qaMsgQueue.delete(id);
  qaMsgQueueRender(id);
  toast("已取消这条待发消息");
}
/** 处理中提交 → 入队（**不打断**在跑的那一轮）。 */
function qaMsgEnqueue(taskId, item){
  const id = taskId || ""; if(!id) return;
  const q = qaMsgQueue.get(id) || [];
  q.push({ text: String((item && item.text) || "") });
  qaMsgQueue.set(id, q);
  qaMsgQueueRender(id);
  toast("已排队 · 这一轮结束后自动发送<br><b>" + escHtml((item && item.text) || "") + "</b>", true);
}
/** 排队的下一条该不该现在发：这条会话没有一轮在跑 + 用户正看着它 + 没有待发的提问卡答案
 *  （见上面三条 ⚠️）。发一条即返回 —— 剩下的等这一轮收尾再来。 */
function qaMsgFlush(taskId){
  const id = taskId || "";
  const q = qaMsgQueue.get(id);
  if(!q || !q.length || qaTaskRunning(id)) return;
  if(id !== (qaActiveId || "")) return;
  if((qaAskQueue.get(id) || []).length) return;
  const item = q.shift();
  if(!q.length) qaMsgQueue.delete(id);
  qaMsgQueueRender(id);
  qaSend(item.text, { followup: true, from: QA_QUEUE_INSTANCE });
}
/** 会话删了 → 它排的队也没意义了（不留悬挂条目）。 */
function qaMsgQueueDropFor(taskId){ qaMsgQueue.delete(taskId || ""); qaMsgQueueRender(); }
/** 点选项：多选只勾（等「提交」）；单选点一下即答 */
function qaAskPick(card, optEl){
  if(!card || !optEl || qaAskAnswered(card)) return;
  const qb = optEl.closest(".qa-ask-qb"); if(!qb) return;
  if(qb.dataset.multi === "1"){
    optEl.classList.toggle("sel");
    qaAskSyncSub(card);
    return;
  }
  qb.querySelectorAll(".qa-ask-opt").forEach(o=>o.classList.remove("sel"));
  optEl.classList.add("sel");
  qaAskSyncSub(card);
  if(qaAskAllDone(card)) qaAskMark(card, qaAskText(card));
}
/** 「其他」自由输入：不在清单里的答案直接写（回车 / 点确认都走这里）。
 *  发出去的是**同一份信封**（引擎只认 `【用户选择】` 前缀，见 qaAskText）；卡上「你的回答」
 *  那一行显示用户写的原文（信封里的题面当展示文案太啰嗦）。 */
function qaAskFree(card, val){
  const t = String(val || "").trim();
  if(!card || !t || qaAskAnswered(card)) return;
  const qbs = [...card.querySelectorAll(".qa-ask-qb")];
  const q = qbs.length === 1 ? (qbs[0].dataset.q || "") : "";
  qaAskMark(card, "【用户选择】\n" + (q ? "- " + q + " → " + t : "- " + t) + "\n", t);
}

