/* ---------- #qa-stream 事件委托：工具卡 / 思考条的开合 / 提问卡点选 ----------
   为什么必须委托：快照恢复（qaTaskSwitch / qaDbLoad / 启动恢复）走的是
   `s.innerHTML = t.snapshot` —— DOM 节点被整份重建，绑在卡片上的监听随旧节点消失。
   表现就是「点侧栏历史任务后，WebSearch 这类工具卡点了没反应」（2026-09-16 用户报障）。
   委托挂在容器 #qa-stream 上（它从不被替换，只换 innerHTML），历史卡片照样可开合。
   ⚠️ 提问卡（⑤ .qa-ask）同理：恢复出来的历史提问卡也要能点选 —— 它比工具卡更怕丢监听。
   幂等：重复调用只绑一次。 */
let qaStreamDelegated = false;
function bindQaStreamDelegation(){
  const s = $("#qa-stream");
  if(!s || qaStreamDelegated) return;
  qaStreamDelegated = true;
  s.addEventListener("click", e=>{
    // ⑤ 提问卡：选项 / 提交 / 「其他」确认（先于工具卡判定 —— 卡里没有 .qa-tool-head，互不干扰）
    const askOpt = e.target.closest && e.target.closest(".qa-ask-opt");
    if(askOpt){ const c = askOpt.closest(".qa-ask"); if(c) qaAskPick(c, askOpt); return; }
    const askSub = e.target.closest && e.target.closest(".qa-ask-sub");
    if(askSub){
      const c = askSub.closest(".qa-ask");
      if(c && !qaAskAnswered(c) && qaAskAllDone(c)) qaAskMark(c, qaAskText(c));
      return;
    }
    const askGo = e.target.closest && e.target.closest(".qa-ask-go");
    if(askGo){
      const c = askGo.closest(".qa-ask");
      const inp = c && c.querySelector(".qa-ask-other input");
      if(c) qaAskFree(c, inp && inp.value);
      return;
    }
    const toolHead = e.target.closest && e.target.closest(".qa-tool-head");
    if(toolHead){
      const card = toolHead.closest(".qa-tool"); if(!card) return;
      card.dataset.touched = "1";               // 记「用户手动开合过」，自动收起让位
      card.classList.toggle("open");
      return;
    }
    /* ②③ 复制按钮（代码面板帧 / 工具明细段，2026-09-28）：
       要复制的原文落位是**固定的** —— 面板里是 `.qa-cf-bd pre`，明细段里是 `.qa-tool-sec pre`
       （见 29-msg-variants 的 qaCodeFrameNode / QA_COPY_BTN）。按这个约定取文本，
       所以按钮本身不带任何 data-*，快照重建后照样能复制。
       点完切成对勾 1.2s：原来没有反馈，用户会怀疑没复制上、连点好几次。 */
    const cpBtn = e.target.closest && e.target.closest(".qa-cf-copy");
    if(cpBtn){
      const host = cpBtn.closest(".qa-cf, .qa-tool-sec");
      const pre = host && host.querySelector("pre");
      if(typeof qaCopyText === "function") qaCopyText(pre ? pre.textContent : "");
      cpBtn.classList.add("copied");
      setTimeout(()=>cpBtn.classList.remove("copied"), 1200);
      return;
    }
    /* ② 单行标记行开合（图标 + 人话标题 + 状态徽章 + 可展开明细）。
       ⚠️ 没有明细（data-tog="0"）时不可点 —— 否则点了一下什么都没有，像坏了。 */
    const mkRow = e.target.closest && e.target.closest(".qa-mk-row");
    if(mkRow){
      if(mkRow.dataset.tog !== "1") return;
      const mk = mkRow.closest(".qa-mk"); if(!mk) return;
      mk.dataset.touched = "1";
      mk.classList.toggle("open");
      return;
    }
    const thinkTg = e.target.closest && e.target.closest(".qa-think-toggle");
    if(thinkTg){
      const th = thinkTg.closest(".qa-think"); if(!th) return;
      th.dataset.touched = "1";
      th.classList.toggle("open");
      return;
    }
    const toolsTg = e.target.closest && e.target.closest(".qa-tools-toggle");
    if(toolsTg){
      const ts = toolsTg.closest(".qa-tools"); if(!ts) return;
      ts.dataset.touched = "1";
      ts.classList.toggle("open");
    }
  });
  // 「其他」行的回车确认（input 的 Enter 不会走 click；同样挂在容器上才不怕快照重建）
  // ⚠️ 合成中（输入法选词那一敲）不算确认，见 qaImeComposing —— 这行输入框同样要吃中文。
  s.addEventListener("keydown", e=>{
    if(qaImeComposing(e)) return;
    if(e.key !== "Enter") return;
    const inp = e.target.closest && e.target.closest(".qa-ask-other input");
    if(!inp) return;
    e.preventDefault();
    const c = inp.closest(".qa-ask"); if(c) qaAskFree(c, inp.value);
  });
}
function bindNavTasks(){
  const box = $("#nav-tasks"); if(!box) return;
  box.addEventListener("click", e=>{
    /* 行尾 ⋯（下拉操作）：必须先于 .tk-item 判定 —— 它在这个行按钮**内部**，
       冒泡上来时 target 也在行里。stopPropagation 是给 document 那层「点菜单外关闭」
       用的：不停，刚开出来的菜单会被同一拍的事件立刻收掉。 */
    const dots = e.target.closest(".tk-dots");
    if(dots){
      e.stopPropagation();
      const id = dots.dataset.menu;
      if(qaTkMenuFor === id) qaTkMenuClose();      // 同一点再点一次 = 收起
      else qaTkMenuOpen(id, dots);
      return;
    }
    // 分组胶囊 tab（处理中 / 审查中 / 已完成）：点哪个 tab 就看哪一档
    //（用户 2026-09-20：「3个状态的分组改成胶囊tab形式」+「已归档改成已完成」）。
    // 点当前档 = 无操作（qaTaskTabSet 里判掉），不重画、不闪。
    const tab = e.target.closest(".tk-tab");
    if(tab){ qaTkMenuClose(); qaTaskTabSet(tab.dataset.group); return; }
    const item = e.target.closest(".tk-item");
    if(item){ qaTkMenuClose(); qaTaskSwitch(item.dataset.id); }
  });
  /* 搜索框（`#nav-tasks` **之上**的静态节点，不随列表重绘消失 —— 见 05-task-list-render.js 文件头 ④）：
     输入即搜（跨档平铺命中行，上限同为 20 条）；清空 / Esc = 回 tab 视图。
     ⚠️ 这里只负责「把值交给 qaTkQuerySet」，过滤与渲染口径全在那一个写入口里。 */
  const sInp = $("#tk-search-input");
  if(sInp && !sInp.dataset.bound){
    sInp.dataset.bound = "1";
    sInp.addEventListener("input", ()=>{ qaTkQuerySet(sInp.value); });
    sInp.addEventListener("keydown", (e)=>{ if(e.key === "Escape"){ e.preventDefault(); qaTkQuerySet(""); } });
  }
  const sClr = $("#tk-search-clear");
  if(sClr && !sClr.dataset.bound){
    sClr.dataset.bound = "1";
    sClr.addEventListener("click", ()=>{ qaTkQuerySet(""); const i = $("#tk-search-input"); if(i) i.focus(); });
  }
  /* 菜单内容点击：绑在菜单自己身上（它是 body 下的单例，不随列表重绘而消失），
     与行上的委托同理 —— 清单每次开合都由 innerHTML 重建。 */
  const menu = $(TK_MENU_SEL);
  if(menu && !menu.dataset.bound){
    menu.dataset.bound = "1";
    menu.addEventListener("click", e=>{
      const o = e.target.closest(".ag-model-opt"); if(!o) return;
      qaTkMenuAct(o.dataset.tkAct, o.dataset.tkId);
    });
  }
  /* 点菜单外 / Esc 关闭。⚠️ 用 composedPath 而不是 e.target.closest：
     菜单项的点选会 innerHTML 重绘（菜单内容重建），被点的节点在冒泡到 document 前
     就脱离了 DOM —— detached 节点的 closest() 恒为 null，会被误判成「点在菜单外」。 */
  document.addEventListener("click", e=>{
    if(qaTkMenuFor === null) return;
    const m = $(TK_MENU_SEL), path = e.composedPath();
    if(m && path.includes(m)) return;
    if(path.some(n=>n && n.classList && n.classList.contains("tk-dots"))) return;
    qaTkMenuClose();
  });
  document.addEventListener("keydown", e=>{ if(e.key === "Escape") qaTkMenuClose(); });
  qaTaskRender();
  // 启动恢复：激活最近一条任务（但不抢跳视图 —— 保持首页纯入口，点侧栏条目才进对话）
  if(qaTasks.length){
    qaActiveId = qaTasks[0].id;
    const t = qaTasks[0];
    if(t.snapshot){
      const s = $("#qa-stream");
      if(s){ s.innerHTML = t.snapshot; s.hidden = false; qaDropRunPills(s); }
    }
    qaSyncChatMode();
    qaTaskRender();
  }
  qaDbLoad();                                 // 桌面桥可用：以 SQLite 清单为准覆盖恢复
}
/** 停这一轮（提交钮处于「停止」态时点它走这里 —— 判据见 qaSendBtnSync：运行中 + 输入框空）。
 *  只停**当前显示的这条会话**在跑的那一轮，逐条理由照旧：
 *    · 按 rid 停，绝不用不带 rid 的 `askStop()`（那是「停全部」，并发下会把用户别的会话
 *      一起打断 —— 主进程侧每路一个 rid，见 desk:ask-stop）；
 *    · 收尾行与快照都落进**那一轮自己的画布**（生成中切走时，用户是在别的会话里点的停止，
 *      写到当前显示的这条上就是张冠李戴）；
 *    · 必须和 end/error 那几条收尾路径**一样**收口（用户 2026-09-18 报障：「不应该出现两个
 *      生成中」）—— 漏了 `h.finish(); h.run.remove()`，那枚「生成中」胶囊就永远钉在画布上，
 *      下一次发送再长出一枚 → 两枚并排（且被快照一起存下来）。停止 = 这一轮结束，胶囊必摘。
 *  ⚠️ 这段原先长在 qaSend 里（那时「运行中提交」的语义就是停止）。用户 2026-09-18 把
 *  「运行中提交」改成**排队**后，停止就不该再挂在提交路径上 —— 抽成这条独立路径，
 *  qaSend 那边只留入队（见 qaMsgQueue）。 */
async function qaStopActive(){
  const run = qaRunOfTask(qaActiveId);
  if(!run) return false;
  /* 「模拟测试」那一轮（2026-09-21）：它没有 rid，停的是主进程那条测试（`desk:agentTestStop`）。
     收尾动作与普通对话**逐条对齐**（摘胶囊 / 落收尾行 / 存快照 / 重算锁 / 放行队列），
     只把「停谁」换掉 —— 用户看到的是同一枚停止钮、同一套行为。 */
  if(run.sim){
    try{ await window.desk.agentTestStop(run.simTid || ""); }catch(e){}
    qaRuns.delete(run.rid);
    if(typeof atcStopLocal === "function") atcStopLocal(run);   // 摘掉「生成中」胶囊（含被测引擎那条）
    else qaRunEnd(run.h);
    qaRenderDone(false, "已停止 ・ 这一轮已中断，可以发下一条了", qaRunHost(run));
    qaSaveSnapshotFor(run.taskId, null, { holder: run.holder });
    qaSyncBusy();
    qaAskFlush(run.taskId);
    qaMsgFlush(run.taskId);
    return true;
  }
  try{ await window.desk.askStop(run.rid); }catch(e){}
  qaRuns.delete(run.rid);                      // 上一轮的迟到事件（end/error）不再对号入座
  qaRunEnd(run.h);
  qaRenderDone(false, "已停止 ・ 这一轮已中断，可以发下一条了", qaRunHost(run));
  qaSaveSnapshotFor(run.taskId, null, { holder: run.holder });
  qaSyncBusy();                                // 按真实状态重算：当前这条空闲 → 解锁（别的会话在跑也不拦）
  qaAskFlush(run.taskId);                      // ⑤ 停止即「这一轮结束」→ 提问卡排队的答案可以发了
  qaMsgFlush(run.taskId);                      // 同理：排队等发的下一条现在可以提交了
  return true;
}
/* ---------- 发送总入口（首页 + 任务对话共用）----------
   qaSend(text, {followup, from}):
   - 首页发送（followup=false）：新建任务 → 跳「任务对话」视图 → 流式作答；
   - chat 视图继续追问（followup=true）：沿用当前任务，不清画布、不新建；
   - **运行中提交 = 排队**（用户 2026-09-18：「会话处理中时 改成支持输入 但是新需求排列在
     会话框上面默认等待上一个结束后自动提交」）：这一轮没跑完时，本条消息排进该会话的
     队列（qaMsgQueue），等它收尾由 qaMsgFlush 自动提交 —— **不打断**在跑的那一轮。
     ⚠️ 「停止」不在这条路径上：它挪去了 qaStopActive（提交钮在输入框为空时才是停止钮）。
   - from = 组件实例名（home2 / home）：发送后清掉该实例的胶囊（＋ 菜单选的类型标记）。
     ⚠️ 「挂需求胶囊 → 发送 = 新建需求」不经过这里：捕获拦截在 qaSend 之前就转给
     askReqSave 了，能走到这里的都是普通对话 —— 而普通对话**不建需求**（2026-09-18 定稿，
     见下面 followup=false 分支里的注释）。 */
async function qaSend(v, opts){
  const followup = !!(opts && opts.followup);
  const from = (opts && opts.from) || "";
  /* mode = 这一轮是不是**某种模式**（目前只有 'rec' = 录屏模式，由 ＋ 菜单的「录屏」胶囊决定，
     见 25-record.js::recStart）。它一路透传到主进程 desk:ask —— 那边据此把用户原文换成
     **模式指令**再交给引擎（用户气泡里显示的仍是他的原话，指令只进 prompt）。
     空串 = 普通对话（一字不改）。 */
  const mode = (opts && opts.mode) || "";
  /* append = 把这条消息**追加进那条还活着的会话**（提问卡的答案走这条，见 qaAskSend）。
     传 { sid, rid } 时不再 `desk.ask` 起新进程，而是 `desk.askAppend` 往**同一个进程的 stdin**
     投一条 user 消息 —— 引擎把它当**下一轮**处理（这是唯一有效的作答通道：引擎在非交互模式下
     自己就把 AskUserQuestion 拒了，宿主没有回填 tool_result 的窗口，实测抢答无效，
     见 agent-cli.cjs 那一节的两次实测）。
     ⚠️ 追加**不产生新 rid**：这一轮的事件仍带 append.rid 回来，所以 run 要登记在那个 rid 上。 */
  const append = (opts && opts.append) || null;
  v = String(v || "").trim();
  /* 附件闸门（粘贴 / 拖入的截图）：必须都在发送前落盘完成。
     半途发出会让引擎读到一个还不存在的路径，而报错发生在下游、信息也指向别处 ——
     这类失败最难排查，所以在入口就拦住：
       · 还在保存 → 提示稍等（不吞掉用户已输入的内容）
       · 有失败项 → 要求先重试或移除（不静默丢掉用户的图）
     只贴图不发文字是允许的（贴一张截图直接发是很自然的动作），
     此时 prompt 由 askAttPrompt 补一句中性引导。 */
  const atts = askAttReady(from);
  if(askAttBusy(from)){ toast("截图还在保存，稍等一下再发"); return; }
  if(askAttFailed(from).length){ toast("有截图没保存成功，先重试或移除"); return; }
  // 无桌面桥（纯浏览器打开）：降级 = 创建流水线任务并跳转，两个输入框共用同一入口语义
  if(!(window.desk && typeof window.desk.ask === "function")){
    if(!v && !atts.length) return;
    if(from) askChipsClear(from);
    const t = createTask();
    toast("任务已创建：<br><b>" + escHtml(v || "（截图）") + "</b>", true);
    showView("pipeline");
    // 打开新任务的督造面板，与「点击等待列卡片」路径一致
    if(t) openPanel(t.id);
    const src = followup ? $("#home-input") : $("#home2-input");
    if(src) src.value = "";
    if(from) askAttClear(from);
    return;
  }
  /* 空输入直接 return（点停止钮时输入框是空的 → 那条路径已在 qaSendBtnSync 分流到
     qaStopActive，走不到这里；这里只挡「回车但什么都没写」这种空提交）。 */
  if(!v && !atts.length) return;
  /* 处理中提交的下一条 → **排队**，不打断在跑的那一轮（用户 2026-09-18：「会话处理中时
     改成支持输入 但是新需求排列在会话框上面默认等待上一个结束后自动提交」）。
     改造前这里是「停就是停」：撰写区锁死、提交钮就是停止钮，任何调进来的路径都只会停。
     现在**停止挪到了 qaStopActive**（提交钮空手点才走那条），这里换成入队：
       · 条目挂在**这条会话**的队列上（qaMsgQueue），渲染在会话框上方，标「等待中」；
       · 等它收尾（onDone / onError / 点停止）或用户切回这条会话时由 qaMsgFlush 自动提交。
     ⚠️ 只对 followup（任务对话实例）成立：home2 = 新建对话入口，它提交的永远是**新会话**，
        不排进别人的队（qaBusyEx 也永不锁它，两边口径一致）。
     ⚠️ append（提问卡答案）不走这里：答案有自己的队列与时机（qaAskQueue），更不该排队。 */
  if(!append && followup && qaTaskRunning(qaActiveId)){
    /* 模式轮（录屏 / 截屏 / 短视频）**不排队**：队列条目只带正文（qaMsgEnqueue({ text: v })），排进去就等于
       「用户以为在录屏，结果变成了一句普通对话」—— 静默降级比直接拒绝更糟。
       （recStart / shortsStart 发送前也拦了一道，这里是第二道：别指望调用方永远记得判。） */
    if(mode){
      /* 文案按模式给全称（2026-09-23 起模式轮不止「录屏」一种，别再硬编码）。 */
      const mn = { rec: "录屏", shot: "截屏", shorts: "做短视频", sim: "发模拟测试" }[mode] || "发送";
      toast("这条会话还在跑 —— 等它结束再" + mn); return;
    }
    if(from) askChipsClear(from);              // 与正常发送同约定：发出即清掉该实例的胶囊
    const srcInp = $("#home-input");
    if(srcInp) srcInp.value = "";              // 正文已挪进队列条目 → 输入框腾空
    qaMsgEnqueue(qaActiveId, { text: v });
    qaSendBtnSync(from || "home");             // 输入框空了 → 那枚钮切回「停止」
    return;
  }
  if(from) askChipsClear(from);
  const srcInput = followup ? $("#home-input") : $("#home2-input");
  /* 本次用哪个模型 = **唯一算法**（见 04-task-list.js 的 qaModelFor）：
       工作台首页（home2，新建任务）→ 用户在那枚下拉里选的（没选 = 配置链的链首）；
       任务对话（home，1 任务 = 1 会话）→ **该任务自己的模型**（任务对话那枚下拉可改它，
       自动切换时也会被改成实际跑通的那个）。
     ⚠️ 必须在 qaTaskBegin 之前算好：新建任务时它就是这条任务的模型，要随任务落库。 */
  const sendModel = (typeof qaModelFor === "function") ? qaModelFor(from, qaActiveId) : qaModel;
  if(!followup){
    // 首页发送 = 新建 1 个任务：先给旧任务存快照 → 清空画布 → 跳「任务对话」视图流式作答。
    // ⚠️ 顺序敏感（2026-09-16 踩坑）：此前先清画布，旧任务快照永远存的是空串，
    //    点侧栏切回旧任务时历史即丢（只剩标题，内容全无）。
    if(typeof showView === "function") showView("chat");   // 发送才跳转，聚焦/输入都不跳
    qaTaskSaveSnapshot();                   // 快照存的是旧任务此刻的画布（清空前）
    const s = $("#qa-stream"); if(s) s.innerHTML = "";
    qaTaskBegin(v, sendModel);              // 左栏登记新任务（1 任务 = 1 对话 = 1 模型）
    /* 【2026-09-18 用户定稿】对话框提交 = **普通对话**，不再默认往需求看板落条目。
       改前：首页每次发送都无条件 reqBoardAddFromChat(v) → 看板里长出一条来源「来自对话」
       的「待处理」需求。用户原话：「工作台对话框输入提交不要默认加入需求 只是普通对话
       必须加号选择了需求模式才加入待处理需求」。
       改后：建需求的**唯一入口** = ＋ 菜单选「需求」挂胶囊（askReqSave —— 捕获阶段就
       拦在 qaSend 之前，见 qaAddBind），不挂胶囊的提交一行都不碰需求表。
       ⚠️ 记忆流不受影响：desk.ask 照旧把输入写进当前 workspace 的记忆流（用于功能点文档
       归纳，见 requirements.cjs），这里只是**不再建需求条目**，两件事本来就不该混。 */
  } else if(!qaActiveId){
    // chat 视图但无激活任务（极端兜底）：按新建处理
    qaTaskBegin(v, sendModel);
  }
  qaSetChatMode(true);                      // 对话态：聊天上/输入下，气泡空态隐藏
  qaRenderMine(v, atts);                    // 截图随气泡一起留在对话里（用真实 URL，可安全清附件）
  if(srcInput) srcInput.value = "";
  /* ⚠️ 顺序敏感：附件路径必须在**清附件之前**取好（askAttClear 会清空 askAtts[p]）。
     取的是**绝对路径**，交给主进程转成 CLI 原生 `-a/--attach` —— 图片作为 content block
     真的送到模型眼前。**不要**退回「把路径写进提示词」：实测那样引擎只会回
     「我无法直接查看图片内容」，偶尔「看起来成功」其实是用 Bash 跑 OCR 兜的假象。
     只贴图不写字也允许：提示词补一句中性引导，图本身走附件。 */
  const attPaths = atts.map((a)=>a.path).filter(Boolean);
  const prompt = v || (attPaths.length ? "请看截图。" : "");
  if(from) askAttClear(from);               // 已进气泡 → 输入框上方的附件条功成身退
  const h = qaRenderAnswer();
  if(!h){ return; }
  /* ⚠️ 锁只落**这条会话**自己的撰写区：run 按 taskId=qaActiveId 登记 → qaTaskRunning 成立，
     qaBusyEx 就只锁当前显示的这条；别的会话（含 home2 的新建入口）照常可发、互不设卡。
     `userText` / `msgText`（2026-09-22）＝这一轮的**消息台账**：持久层只存消息（不存样式），
     收尾时由 qaSaveSnapshotFor 一并落进消息文件（见 27-conv-log.js）。
       · userText = 用户这一轮说的话（只贴图时记「（截图）」—— 与气泡里显示的一致）；
       · msgText  = 引擎这一轮的正文，由流式 text 事件逐段累加（见 09-ask-event.js 的 ctx.onText）。
     `logSteps`（2026-09-23）＝这一轮的**过程步骤**（思考块 / 工具卡，见 ctx.onStep）：
     运行中只攒着，收尾时随消息一起落进消息表 —— 历史重画才能看到「当时想了什么、干了什么」。 */
  const run = { rid: 0, taskId: qaActiveId, h, tools: new Map(), agents: new Map(), ms: null, holder: null,
                userText: v || (attPaths.length ? "（截图）" : ""), msgText: "", logSteps: [] };
  qaRuns.set(0, run);                          // 占位登记：end 事件按 rid 路由，0 = 暂未过
  qaRefreshRunning();                          // 锁**这条会话**的撰写区（其余会话不受影响）
  // 1 任务 = 1 会话：新任务首问不带 sessionId（引擎新建会话）；追问回传上次 result 的
  // session_id（--session 续接），引擎据此延续同一上下文。任务对象持久化该 id。
  const curTask = qaTasks.find(x=>x.id===qaActiveId) || null;
  try{
    // workspace 透传：主进程把输入落进该工作区记忆流（归纳按窗口/按天批量做，见 requirements.cjs）；
    // workspaceDir/Flag 让引擎**在该目录里干活**（cwd + `-w`，见 main.js 的 workspace 注释）。
    // prompt 是用户正文本身；截图走 attachments（引擎原生附件通道，图片真的送到模型眼前）。
    // 两者的分工不要混：「路径写进提示词」实测模型看不到图，见 qaSend 里的注释。
    /* ① 能追加就追加（提问卡的答案走这条）：不新建进程、无冷启动，答案落进**同一条会话**。
       ⚠️ 追加不产生新的 rid —— 事件仍带**原来那一轮的 rid** 回来，所以 run 必须登记在
       append.rid 上；否则 bindHomeAsk 的 `qaRuns.get(ev.rid)` 找不到它、整轮被当过期事件丢掉
       （界面表现：点了选项也显示「已选」，但引擎那轮的回答永远不出现）。 */
    if(append && append.sid && typeof window.desk.askAppend === "function"){
      let ar = null;
      try{ ar = await window.desk.askAppend({ id: append.sid, text: prompt }); }
      catch(e){ ar = { ok: false, error: String((e && e.message) || e) }; }
      if(ar && ar.ok){
        run.rid = append.rid;
        qaRuns.delete(0); qaRuns.set(run.rid, run);
        return;
      }
      /* 追加没送达（会话已结束 / keep-alive 空闲窗口过了 / 老版 CLI 不认 `--append`）→
         就地回落「新起一轮 + `--session` 续接」：答案一样送得到，只是多一次冷启动。
         不静默：留一条日志，排障时能看出这一轮走的是哪条路。 */
      console.warn("[qaSend] 追加未送达，回落新起一轮：", (ar && ar.error) || "");
    }
    /* ⚠️ **会话 id 优先 `qaContinueRecent(t)` 判定**：本会话**有 id**（哪怕是重启后从库读回来的）
       → 直接传 `--session`（用户原话：「不应该-c 应该传回话id」）；本会话**被打断**没拿到 id
       且 sidLost=1 → `-c` 兜底续接最近一次。仅这一条路径用 `-c`。 */
    /* ⚠️ `model` = **本次要用的那个**（`qaModelFor(from, ...)` 现算，见上）：
       工作台首页新建任务时是用户选的（没选 = 配置链的链首），任务对话追问时是**这条任务自己的模型**。
       主进程拿到它当**主选**，后面自动接上同一条链的其余候选逐个试（额度/限流/不可用时
       换下一个，见 desktop/chat-model.cjs）；空串 = 引擎自带默认。 */
    const r = await window.desk.ask({ prompt, engine: qaEngine, model: sendModel,
      /* 访问权限档位（manual / accept-edits / auto / full）→ 主进程拼 `--permission <档位>`。
         四档的语义见 QA_PERMS 上方注释；这里只透传 id，不认识的值由 qaPermSet/qaPermOf 挡掉。 */
      permission: qaPerm,
      attachments: attPaths,
      sessionId: (curTask && curTask.sessionId) || "",
      continueRecent: qaContinueRecent(curTask),
      workspace: (qaWorkspace || ""), taskId: (curTask && curTask.id) || "",
      workspaceDir: wsDirOf(), workspaceFlag: qaWorkspaceSupported(),
      streamFlag: qaStreamSupported(),
      /* mode = 模式轮标记（'' / 'rec'）。主进程据此把 prompt 换成**模式指令**
         （录屏模式：用户原文 + 壳探明的前置事实 + cdp-drive 固定写法 + 硬约束）。 */
      mode: mode,
      taskTitle: (curTask && curTask.title) || "" });
    if(!r || !r.ok){
      qaRunEnd(h);
      qaRenderDone(false, "调用失败 · " + ((r && r.error) || "桥不可用"), qaRunHost(run));
      /* 失败原因也进消息台账（2026-09-22）：持久层只存消息，若这一轮一个字都没吐、
         失败原因又不记，点开历史就是「只有我说了话、引擎没反应」—— 与需求派发那边
         把失败原因当一条回答渲染是同一条口径（见 qaReqConvSnapshot）。 */
      run.msgText = "运行失败\n" + String((r && r.error) || "桥不可用");
      qaSaveSnapshotFor(run.taskId, "error");
      qaRuns.delete(run.rid || 0);
      qaRefreshRunning();
      return;
    }
    run.rid = r.rid;
    qaRuns.delete(0); qaRuns.set(r.rid, run);  // 用真实 rid 换掉占位（end 事件按 rid 路由）
  }catch(e){
    qaRunEnd(h);
    qaRenderDone(false, "调用失败 · " + String(e && e.message || e).slice(0, 80), qaRunHost(run));
    run.msgText = "运行失败\n" + String((e && e.message) || e);
    qaSaveSnapshotFor(run.taskId, "error");
    qaRuns.delete(run.rid || 0);
    qaRefreshRunning();
  }
}
/** 对话进行中禁用引擎选择器时，按钮 title 上那句说明。
 *  **文案只写这一份**：判锁的是 qaBusyEx（落 disabled），写 title 的是 qaPickerSync
 *  （它是按钮文案的唯一出处）—— 两边共用这个常量，避免同一句话在两处各写一遍走形。
 *  ⚠️ 只有 engine 一条：**模型下拉不锁**（用户 2026-09-18 定稿「模型可以换」）——
 *  模型是每次调用随 `-m` 下发的参数，换了不会把会话接错，所以它不需要这条说明。 */
const QA_LOCK_TITLE = { engine: "对话进行中 · 不能换引擎" };
/** 运行态开关（用户 2026-09-18 晚定稿：「都不需要锁 只是回话在处理中锁死当前回话的输入
 *  就行 不需要锁死其他的回话」）：锁**只落正在处理中的这条会话自己的撰写区** ——
 *  当前显示的这条在跑 → home 撰写区进入**运行态**：＋ 菜单禁用、引擎选择器禁用，
 *  提交钮按输入框有无内容切成「停止 / 发送（排队）」两态（见 qaSendBtnSync）；
 *  别的会话在跑 → **完全不影响**：home2（新建对话入口）永远可发，home 在显示空闲会话
 *  时也照常可发 —— 会话之间互不设卡，并行是会话自己的事。
 *  老的「qaRuns.size > 0 全局锁」与「另一条会话正在生成中」占位一并下线。
 *  实例清单只有 ASK_INSTANCES 一份（别再手写 ["#home-…","#home2-…"] 这样的数组：
 *  加/删实例必然漏改）。
 *
 *  2026-09-18 追加（用户：「对话状态时引擎选择要禁用 不能换引擎」）：**引擎选择器并入同一把锁**。
 *  理由与撰写区同源 —— 会话是按当前引擎建的（sessionId 与引擎绑定，追问靠它接回同一会话），
 *  生成中把引擎换掉，下一轮就接到别的引擎上去了。
 *  锁的口径也照抄撰写区：只在「当前显示的这条在跑」时禁用；home2（新建对话入口）永不锁，
 *  别的会话在跑也不影响它 —— 并行仍是会话自己的事。
 *  ⚠️ **只锁引擎**：会话归属只跟引擎绑定（sessionId 靠它续接），所以只有引擎那一枚要禁。
 *  （**模型下拉不锁** —— 用户 2026-09-18 定稿「模型可以换」：模型是每次调用随 `-m` 下发的
 *  参数，换了不会把会话接错；2026-09-21 定稿两个实例都可以选模型，那枚下拉照旧不进这把锁。）
 *
 *  2026-09-18 再改（用户：「会话处理中时 改成支持输入 但是新需求排列在会话框上面默认等待
 *  上一个结束后自动提交」）：**输入域不再锁**。这一轮没跑完也能照常写、照常提交，
 *  提交即排队（见 qaMsgQueue），不再需要「等它结束再打字」。
 *  ⚠️ 于是本函数不再动 `inp.disabled` —— 输入域**任何时候都可用**；占位文案也从
 *  「正在生成中… 点右侧按钮可停止」改成讲清排队规则的那句。 */
function qaBusyEx(){
  qaRunning = qaRuns.size > 0;                 // 全局派生量照旧（需求会话同步等仍在用）
  const on = qaTaskRunning(qaActiveId);        // 撰写区只看「当前显示的这条」在不在跑
  ASK_INSTANCES.forEach((p)=>{
    const lock = on && p !== "home2";          // home2 = 新建对话入口，永不锁
    const box = $("#" + p + "-box");
    if(box) box.classList.toggle("is-busy", lock);
    const inp = $("#" + p + "-input");
    if(inp){
      // 常驻占位文案只缓存一次：两个实例占位不同，别在任何地方写死文案
      if(inp.dataset.phIdle == null) inp.dataset.phIdle = inp.placeholder || "";
      // 输入域**不 disabled**（见上面 2026-09-18 再改那段）—— 只换占位文案讲清「提交即排队」
      inp.placeholder = lock ? "上一条处理中 · 回车排队，结束后自动发送" : (inp.dataset.phIdle || "");
    }
    const plus = $("#" + p + "-plus");
    if(plus) plus.disabled = lock;             // ＋ 菜单仍锁：处理中贴图/挂胶囊没有意义
    /* 提交钮的形态（停止 / 发送）随「输入框有没有内容」变 —— 由 qaSendBtnSync 落类与 title。
       放在这里调：qaBusyEx 是「这条会话在不在跑」的唯一重算点，锁态一变它必被调到。 */
    qaSendBtnSync(p);
    /* 引擎选择器：与撰写区同一把锁（见上面 2026-09-18 追加那段）。
       这里**只落 disabled**：title 上那句「为什么点不动」由 qaPickerSync 统一写
       （见 QA_LOCK_TITLE）—— 锁态一变就喊它一次，文案与状态永远同源。
       ⚠️ 要锁的只有引擎这一枚：模型下拉不锁（「模型可以换」，见 QA_LOCK_TITLE 上方那段）。 */
    const engSel = $("#" + p + "-engine");
    let lockFlipped = false;
    if(engSel){
      if(engSel.disabled !== lock) lockFlipped = true;
      engSel.disabled = lock;
      /* 锁上的那一刻把**已经开着**的菜单收掉：否则按钮虽禁用，菜单还浮在上面，
         用户照样能从菜单里把引擎点掉（禁用只挡触发钮，不挡已展开的浮层）。
         关闭统一走 __qaMenuClose（撤 autoUpdate 监听 + 复位按钮，见 qaCloseOne）。 */
      if(lock){
        const m = $("#" + p + "-engine-menu");
        if(m && !m.hidden && typeof window.__qaMenuClose === "function") window.__qaMenuClose(m);
      }
    }
    // 只在真的翻转过时才同步：qaBusyEx 被频繁调用，没必要每次都去重建菜单高亮
    if(lockFlipped && typeof window.qaPickerSync === "function") window.qaPickerSync();
    /* ⚠️ 提交钮的 title 不在这里写 —— 它随「输入框有没有内容」变（停止 / 排队发送），
       统一由 qaSendBtnSync 落（上面已按实例调过一次）。这里再写一遍必然与它打架。 */
  });
}
/** 提交钮的两种形态（用户 2026-09-18：「会话处理中时 改成支持输入 但是新需求排列在会话框上面
 *  默认等待上一个结束后自动提交」）：
 *    · 空闲 → 发送（常态）；
 *    · 运行中 + 输入框空 → **停止**（点它停这一轮，原来那套语义一字不改）；
 *    · 运行中 + 输入框有内容 → **发送**（点它**排队**，不打断在跑的那一轮，见 qaMsgQueue）。
 *  判据用「输入框有没有内容」而不是并排两枚按钮：一枚钮在固定位置只表达一件事 ——
 *  用户按下之前就知道会发生什么（图标与 title 都跟着变，见 .ag-box.is-busy.can-send）。
 *  调用点：qaBusyEx（锁态变化）+ 输入框的 input 事件（内容变化）。 */
function qaSendBtnSync(p){
  const box = $("#" + p + "-box"), inp = $("#" + p + "-input"), btn = $("#" + p + "-send");
  if(!box || !btn) return;
  const busy = box.classList.contains("is-busy");
  const canSend = busy && !!(inp && inp.value.trim());
  box.classList.toggle("can-send", canSend);
  btn.title = !busy ? "发送" : (canSend ? "排队发送 · 上一条处理中，结束后自动提交" : "停止生成");
}
/** 撰写区状态重算：**切会话 / 起跑 / 收尾**都要调一次 —— 锁依赖「当前显示的是哪条、
 *  它在不在跑」：切到在跑的那条 → 锁上；切到空闲的 → 解锁（哪怕别的会话还在跑）。 */
/** 老名兼容 —— 参数废弃（历史上是全局开关 / mineHint），一律按当前会话重算。
 *  ⚠️ onDone/onError 收尾时**不要**再 `qaBusy(false)`（已无意义），改调 qaRefreshRunning()。 */
function qaBusy(_ignored){ qaBusyEx(); }
function qaSyncBusy(){ qaBusyEx(); }
