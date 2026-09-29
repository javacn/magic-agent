/* ===== 运行态注册表（2026-09-18 重构）=====
   用户报障：「任务列表点击显示回答生成中 不能切换 为什么呢？肯定可以切换另1个会话去啊」。
   根因：流式是**增量写 DOM**（qaRenderAnswer / appendText / qaRenderDone 都往画布里写），
   而画布只有一份 #qa-stream —— 生成中一切换，这一轮的增量就会写进**另一个会话**的画布，
   所以老代码只能在切换处硬拦（`if(qaRunning) toast("回答生成中，稍候再切换")`）。
   现在改成「**画布跟着会话走**」：
     · 一次 run = 一个条目，带自己的渲染句柄 h / 工具卡表 / 委派卡表 / 耗时 / 后台容器 holder；
     · 切走 = 把这一轮的画布**整份摘到 holder**（节点不销毁，后台增量继续写进它们），
       切回来 = 再挂回 #qa-stream（内容比快照新，接得上正在跑的那一轮）；
     · 事件按 rid 路由到对应 run（不再有「全局 qaRid + 全局 _cur」那套单例）。
   ⚠️ 但「同时只跑一轮」这条**没有变**（用户 2026-09-18 定稿「对话中不允许上一条没结束
     直接输入下1个」+ 主进程 `desk:ask` 是单会话槽：新请求会先杀掉旧的）。
     所以撰写区仍然全局锁死，只是**切换与查看不再被拦**。 */
const qaRuns = new Map();                       // rid → run
function qaRunOfTask(id){ for(const r of qaRuns.values()) if(r.taskId === id) return r; return null; }
function qaTaskRunning(id){ return !!qaRunOfTask(id); }
/** 这一轮该往哪儿落笔：正常 = 可见画布 #qa-stream；被切到后台 = 自己的 holder */
function qaRunHost(run){ return (run && run.holder) ? run.holder : $("#qa-stream"); }
/** 摘掉画布里的「生成中」胶囊行（`.qa-run`）—— 一轮跑完 / 被停 / 快照恢复时都要摘。
 *  为什么必须显式摘：`.qa-run` 是**行**不是状态，它不会自己消失。收尾路径漏一次，
 *  这枚写着「生成中」的胶囊就永远钉在画布上，下一次发送再长出一枚 →
 *  用户看到的「两个生成中」（2026-09-18 报障）。收尾统一走 qaRunEnd 收口。 */
function qaDropRunPills(host){
  const s = host || $("#qa-stream"); if(!s || !s.querySelectorAll) return;
  s.querySelectorAll(".qa-run").forEach(el=>el.remove());
}
/** 一轮收尾（成功 / 失败 / 被停）的统一收口：关掉流式光标 + 摘掉「生成中」胶囊。
 *  ⚠️ 六个收尾点（end / turn_end / error / 调用失败 ×2 / 点停止）以前各写一遍
 *  `h.finish(); h.run.remove()`，**点停止那条漏写** → 胶囊留在画布上（见上）。
 *  现在只留这一个入口，漏不掉。入参是 qaRenderAnswer 返回的渲染句柄 `h`。 */
function qaRunEnd(h){
  if(!h) return;
  if(typeof h.finish === "function") h.finish();      // 尾部 pending 落盘 + 收掉 ▍光标
  if(h.run && h.run.parentNode) h.run.remove();       // 摘掉这一轮的「生成中」胶囊行
}
/* 正在为「后台 run」写画布（它不在屏幕上）→ 别去滚动可见画布：
   否则前台那条会话会被后台的增量不停拽到底部（用户会看到「我这条怎么自己跳」）。 */
let qaPaintBg = false;
function qaSetChatMode(on){
  const v = $("#view-chat"); if(v) v.classList.toggle("chat-mode", !!on);
}
function qaHasContent(){
  const s = $("#qa-stream");
  /* `.qa-hist-note`（2026-09-22）也算「有内容」：历史找不回来时画布上那条**说明**
     是这条对话唯一的落脚点 —— 不算它就会退回「描述你要做的事」那种新任务引导，
     看起来像「历史被清空了、这是一条新对话」（用户报障的观感就是这么来的）。 */
  return !!(s && !s.hidden && s.querySelector(".qa-msg, .qa-hist-note"));
}
function qaSyncChatMode(){ qaSetChatMode(qaHasContent()); }
function qaScrollIntoView(){
  if(qaPaintBg) return;                          // 后台 run 的增量不许拽动前台画布（见 qaPaintBg）
  const s = $("#qa-stream"); if(!s) return;
  s.scrollTop = s.scrollHeight;   // 卡片固定撑满、自身即滚动容器（滚动条已隐藏），永远贴底
}
/** 对话头像（用户 2026-09-17：「用户因为没用头像应该显示一个1个超，bot这边应该用现在的
 *  应用图标大喵标识」）：
 *    · 用户侧没头像 → 显示 1 个字 = 左下角显示名的首字（「超哥」→「超」，取不到就「超」）；
 *    · bot 侧 → 当前应用图标 assets/icon.png（大喵）；
 *    · **审核用户**（2026-09-20 加）→ 一枚「审」字头像（琥珀底），与作者、引擎都区分开 ——
 *      它代表的是「审查轮的结论说话人」，不是需求作者本人（用户原话：
 *      「验证不通过的返回建议 再代表审核用户发送到对话中」）。
 *  实时渲染（qaRenderMine / qaRenderAnswer）与派发快照（qaReqConvMsg）共用这一份标记，
 *  否则快照里的头像会跟实时那份走形。 */
function qaUserInitial(){
  const el = $("#nu-name");
  const nm = ((el && el.textContent) || "").trim();
  return (nm || "超").slice(0, 1);
}
/* **数字分身的头像图标**（2026-09-21 用户：「应该使用1个机器人图标头像吧」）：
   一枚简笔机器人头 —— 天线 + 圆角方脸 + 两只眼 + 一道嘴 + 两侧耳线；`currentColor` 上色，
   颜色与底由 CSS 给（`.qa-av-sim`，朱砂实底 + 白线）。
   为什么用机器人图标而不是「分」字：它是**一个引擎在演人**，机器人头一眼说明「这不是真人」；
   而「分」字第一眼看不出是什么，还得配说明。 */
const QA_AV_SIM_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">'
  + '<path d="M12 6.7V4.5"/><circle cx="12" cy="3.3" r="1.15" fill="currentColor" stroke="none"/>'
  + '<rect x="4.7" y="6.7" width="14.6" height="11.9" rx="3.5"/>'
  + '<path d="M3.4 11.6v2.6M20.6 11.6v2.6"/>'
  + '<circle cx="9.4" cy="11.9" r="1.25" fill="currentColor" stroke="none"/>'
  + '<circle cx="14.6" cy="11.9" r="1.25" fill="currentColor" stroke="none"/>'
  + '<path d="M9.7 15.6h4.6"/></svg>';

function qaAvatarHtml(role){
  if(role === "mine") return `<span class="qa-av qa-av-txt" aria-hidden="true">${escHtml(qaUserInitial())}</span>`;
  if(role === "review") return `<span class="qa-av qa-av-txt qa-av-rev" aria-hidden="true">审</span>`;
  /* **数字分身**（2026-09-21 用户：「数字分身在右边显示专门的头像即可 不需要标注1个模拟用户的标签」→
     「应该使用1个机器人图标头像吧」）：它演的是用户，但不是用户本人 —— 所以既不复用用户头像
     （那会让人以为是我自己发的），也不只挂一枚「模拟用户」小标（标签挡在气泡里，读起来像注释）；
     给它一枚**自己的**机器人头像（同上 QA_AV_SIM_SVG）。
     ⚠️ 与「审」同一条做法：头像由这里统一给，别在业务代码里手搓 span。 */
  if(role === "sim") return `<span class="qa-av qa-av-sim" aria-hidden="true">${QA_AV_SIM_SVG}</span>`;
  return `<span class="qa-av"><img class="qa-av-img" src="assets/icon.png" alt="掌天瓶" draggable="false"></span>`;
}
/* **引擎名小标**（2026-09-22 用户：「头像旁边显示引擎名字」；同日追加「名字显示的大一点
   再加上模型显示」→ 文案 = 「引擎 · 模型」，字号见 app.css M8）：左侧消息头像独占一行（见文末 M7）
   之后，头像那一行右侧空着 —— 把「这一条是哪个引擎、哪个模型说的」挂在头像右边。
   这里只负责吐节点；**挂不挂、挂哪一档**由渲染层决定（右侧三档不挂：它们靠头像字 / 抬头点明身份，
   见 app.css 的 `.qa-who` 只认左侧形状）。空引擎名 → 空串：宁可没有，也不写「未知」占位。
   模型存的是 **id**，显示走 qaModelName（与「生成中」胶囊同一套分工：名字只活在展示层）。 */
function qaWhoLabel(engine, model){
  const nm = String(engine == null ? "" : engine).trim();
  if(!nm) return "";
  const md = String(model == null ? "" : model).trim();
  return md ? `${nm} · ${qaModelName(md, nm)}` : nm;
}
function qaWhoHtml(engine, model){
  const lb = qaWhoLabel(engine, model);
  return lb ? `<span class="qa-who">${escHtml(lb)}</span>` : "";
}
function qaRenderMine(text, atts){
  const s = $("#qa-stream"); if(!s) return;
  s.hidden = false;
  const div = document.createElement("div");
  div.className = "qa-msg qa-mine";
  div.innerHTML = `<div class="qa-ico">${qaAvatarHtml("mine")}</div><div class="qa-bd"><div class="qa-t md-body"></div></div>`;
  // 用户消息按 markdown 渲染：marked + DOMPurify（沿用产品文档同一管线，输入是用户可控，
  // 但消毒仍保留以防恶意粘贴）。
  const host = div.querySelector(".qa-t");
  /* 正文 + 正文里那些截图路径一起落笔：需求正文（`[粘贴的截图]` + 绝对路径）与飞书带图消息
     （`[图片] <路径>`）的图**走不了附件通道**，只能把路径写进正文 —— 渲染时把它们摘出来
     画成缩略图，别让用户对着一串 /Users/… 猜自己发了什么（见 19-attachments 的 qaShotsMount）。
     代码块的高亮补扫也在它里面。 */
  qaShotsMount(host, text);
  // 随消息发出的截图（只有图没文字时 host 为空，此时这一行就是气泡的全部内容）。
  // 用 DOM 拼节点而不是拼 HTML 字符串：缩略图地址来自落盘返回值，
  // 走 setAttribute 天然免去一次转义，也让「没拿到地址就跳过」这种判断更直白。
  if(atts && atts.length){
    const row = document.createElement("div");
    row.className = "qa-atts";
    atts.forEach((a)=>{
      const u = attThumbUrl(a);
      if(!u) return;
      row.appendChild(qaThumbNode(u, (a.name || "截图") + " · " + attSize(a.size), a.name || "截图"));
    });
    if(row.children.length) host.appendChild(row);
  }
  /* 长消息折叠（2026-09-28，对齐 anywhere 的 CollapsibleUserMessage）：用户在这个工作台里
     最常干的事就是把整份需求/上下文粘进来，一条消息能把画布顶掉两屏 —— anywhere 给用户消息
     的解法是「超阈值就截断 + 底部渐隐 + 一枚展开全文（内容一个字不删，只改可见高度）」。
     ⚠️ 必须放在**附件之后**：判据看的是渲染后的 DOM（host 里有没有 `.qa-atts`），
        带图的消息不折 —— 否则「我特意要你看的图」会被一起折掉（见 29-msg-variants 的 qaFoldMaybe）。
     ⚠️ 折叠态是个**类**（`.qa-fold`），落在 HTML 上 → 快照恢复后高度不会弹回去。 */
  qaFoldMaybe(host, text);
  s.appendChild(div);
  /* ⚠️ 落笔即贴底（2026-09-23 用户报障：「追问问题 但是用户的问题不会触发滚动到最新
     需要等 agent 回复才会触发」）。
     根因：**自己这条气泡是唯一不滚的一笔** —— 引擎侧每一笔（appendText / toolCall /
     setResult / 收尾行，见本文件各处 qaScrollIntoView）与数字分身那枚气泡
     （24-agent-test 的 atcTwinMsg）落笔后都会贴底，只有这里没有。
     于是「上一轮答得长、画布停在中间」时，刚发出去的那条整条落在折线以下 —— 用户看不见
     自己刚问的话，得等引擎吐出第一段正文（09-ask-event 每次事件末尾那次滚动）才被动拽到底。
     排队那条路径（qaMsgQueue）表现一样：轮到它自动提交时同样只有这条不滚，观感还是
     「我的问题没滚过去、等回复才滚」。
     ⚠️ 这一行是**唯一**该滚的地方：qaRenderMine 就是「自己说的话落进画布」的唯一出口，
     放在这里，实时发送 / 排队提交 / 派发快照三条路径一并贴底；放到调用方（qaSend）去补，
     下一次多一条调用路径就又会漏。 */
  qaScrollIntoView();
}
/** 「引擎 · 模型 · 生成中」= 1 枚胶囊（用户 2026-09-17）：
 *  转圈 + 文字同装一枚圆角胶囊（.qa-run-pill），实时生成行与派发快照的「生成中」状态行
 *  共用这一份标记 —— 两处各写一份必然走形（一处改了另一处忘）。
 *  文字仍走 qaModelName（显示名），`-m` 用的是 id，与全链路的分工一致。 */
function qaRunPillHtml(text){
  return `<span class="qa-run-pill"><span class="qa-spinner"></span>`
       + `<span class="qa-run-t">${escHtml(text)}</span></span>`;
}
function qaRenderAnswer(host){
  // 创建一条 AI 应答消息（思考区 + 工具卡区 + 正文区 + 运行态行），返回操作句柄
  // host = 这一轮的落笔处（可见画布 #qa-stream，或后台 run 的 holder，见 qaRunHost）——
  // 绝不能写死 #qa-stream：生成中切到别的会话时，这一轮的增量必须落在**自己**的画布里。
  const s = host || $("#qa-stream"); if(!s) return null;
  if(s.hidden) s.hidden = false;
  const div = document.createElement("div");
  div.className = "qa-msg";
  div.innerHTML = `<div class="qa-ico">${qaAvatarHtml("ai")}</div>
    <div class="qa-bd">
      <div class="qa-think" hidden>
        <span class="qa-think-toggle"><span class="tt">深度思考</span><span class="qa-think-dur" hidden></span><svg class="chev" viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg></span>
        <div class="qa-think-bd"></div>
      </div>
      <div class="qa-tools" hidden>
        <span class="qa-tools-toggle"><span class="tt">工具调用</span><span class="qa-tools-count"></span><span class="qa-tools-dur"></span><svg class="chev" viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg></span>
        <div class="qa-tools-bd"></div>
      </div>
      <div class="qa-t streaming md-body"></div>
    </div>`;
  /* 头像右边默认挂**当前引擎 · 当前模型**（用户 2026-09-22：「头像旁边显示引擎名字」+
     「再加上模型显示」）。模型初值与「生成中」胶囊同源（`qaModel` = 链首 / 任务自定）。
     ⚠️ 这是默认值：说话人不是当前引擎时由调用方用句柄的 `setWho` 改写
     （派发轮 = 该需求的引擎 / 模型，见 13-dispatch-stream；被测引擎 = 事件里的 p.engine，见 24-agent-test）；
     主进程降级换模型时由 `start` 事件改写（09-ask-event，与胶囊同一时机）。
     ⚠️ 右侧三档不显示它：审查轮会把 `.qa-ico` 整个换成「审」头像（这枚小标随之被替换掉），
     且 `.qa-who` 的样式只认左侧形状（见 app.css）。 */
  const icoEl = div.querySelector(".qa-ico");
  if(icoEl) icoEl.insertAdjacentHTML("beforeend", qaWhoHtml(qaEngine, qaModel));
  s.appendChild(div);

  /* ===== 工具调用统计行（Codex 体验）=====
     折叠条显示「工具调用 · N个 · X.Xs」，点击展开看组里的工具卡。
     ⚠️ 组里放**没有 description** 的调用（2026-09-18），外加**超编的**带 description 的独立卡
     （正文只留最近 5 张，更早的收编进组，见 ALONE_KEEP / groupAdd）；所以这里的 N 数的是
     「组里实际有几张卡」，与正文里那几张独立卡无关（2026-09-20）。
     appendTool 首次调用即进入 live：绿点 + 调用数实时跳动，
     工具默认收起（live 也只亮指示，不自动展开；用户手动开合过则尊重用户）；
     endTools 定格为「已调用 · N个 · X.Xs」并维持收起 —— 之后用户仍可点开回看。 */
  const tools = div.querySelector(".qa-tools");
  const toolsTt = tools.querySelector(".tt");
  const toolsCountEl = tools.querySelector(".qa-tools-count");
  const toolsDur = tools.querySelector(".qa-tools-dur");
  const toolsBd = tools.querySelector(".qa-tools-bd");
  let toolsStart = 0, toolsTimer = null, toolsNum = 0;
  const toolsTouched = ()=> tools.dataset.touched === "1";
  function toolsBegin(){
    tools.hidden = false;
    tools.classList.add("live");
    toolsTt.textContent = "工具调用中";
    /* 机器内部活动首次显形 → 把左边那条「机器侧」消息也显出来（见 qaTraceToLeft）。
       没做过拆分时 __traceWrap 是 undefined，这一行就是空操作。 */
    if(div.__traceWrap) div.__traceWrap.hidden = false;
    if(!toolsStart){
      toolsStart = Date.now();
    }
    if(!toolsTimer) toolsTimer = setInterval(()=>{
      toolsDur.hidden = false;
      toolsDur.textContent = ((Date.now() - toolsStart) / 1000).toFixed(1) + "s";
    }, 100);
  }
  function setToolsCount(n){
    if(!toolsStart) toolsBegin();
    toolsNum = n;
    toolsCountEl.textContent = " · " + n + "个";
    qaScrollIntoView();
  }
  function toolsEnd(){
    if(!toolsStart) return;
    clearInterval(toolsTimer); toolsTimer = null;
    tools.classList.remove("live");
    toolsTt.textContent = "已调用工具";
    toolsDur.hidden = false;
    toolsDur.textContent = ((Date.now() - toolsStart) / 1000).toFixed(1) + "s";
    if(!toolsTouched()) tools.classList.remove("open");
  }
  /* 收尾兜底（2026-09-18）：这一轮结束时，画布上**还在转圈**的工具卡一律落定。
     为什么需要它：一条事件里装了多次调用时，tool_result 只带一个 id，只有第一张卡配得上，
     其余几张（现在都作为独立卡摆在正文流里，转圈格外扎眼）原来会永远转 —— 连同
     「点停止」那条收尾路径（qaRunEnd 不经过 finishTools）也一起被这里收掉。
     钩子 `__qaDone` 由 mkCard / agentCall 挂在节点上（委派卡的是带胶囊的那版）；
     快照恢复出来的历史卡没有钩子，但它们本来就不是 running，扫不到。 */
  function settleRunningTools(ok){
    div.querySelectorAll('.qa-tool[data-state="running"]').forEach(el=>{
      if(typeof el.__qaDone === "function"){ try{ el.__qaDone("", ok); }catch(e){} }
    });
  }

  /* ===== 深度思考状态机（Codex 体验）=====
     appendThinking 首次调用即进入 live：朱砂呼吸点 + 「思考中」+ 秒数实时跳动，
     思考默认收起（live 也只亮指示，不自动展开；用户手动开合过则尊重用户）；
     endThinking 定格为「已深度思考 · X.Xs」并维持收起 —— 之后用户仍可点开回看。 */
  const think = div.querySelector(".qa-think");
  const thinkTt = think.querySelector(".tt");
  const thinkDur = think.querySelector(".qa-think-dur");
  const thinkBd = think.querySelector(".qa-think-bd");
  /* ⚠️ 开合点击统一走 #qa-stream 上的**事件委托**（见 bindQaStreamDelegation）：
     快照恢复（`s.innerHTML = t.snapshot`）会把节点整个重建 —— 直接 addEventListener
     绑在卡片上的监听随旧节点一起消失，「恢复历史对话后点工具卡/思考条没反应」
     （2026-09-16 用户报障：WebSearch 点击无法展开）。委托挂在不会被重建的容器上，
     恢复后的卡片照样可开合。用户手动开合过 → 记 data-touched，自动展开/收起让位。 */
  let thinkStart = 0, thinkTimer = null;
  const thinkTouched = ()=> think.dataset.touched === "1";
  function thinkBegin(){
    think.hidden = false;
    think.classList.add("live");
    thinkTt.textContent = "思考中";
    /* 同 toolsBegin：内部活动首次显形 → 显出左边那条「机器侧」消息（见 qaTraceToLeft） */
    if(div.__traceWrap) div.__traceWrap.hidden = false;
    if(!thinkStart){
      thinkStart = Date.now();
      // 思考默认收起：live 态只亮「思考中」指示（灰标签 + 朱砂呼吸点），不自动展开；
      // 用户点开回看 → 记 data-touched，之后尊重用户开合（见 bindQaStreamDelegation）。
    }
    if(!thinkTimer) thinkTimer = setInterval(()=>{
      thinkDur.hidden = false;
      thinkDur.textContent = ((Date.now() - thinkStart) / 1000).toFixed(1) + "s";
    }, 100);
  }
  function thinkEnd(){
    if(!thinkStart) return;                            // 从未思考过则什么都不显示
    clearInterval(thinkTimer); thinkTimer = null;
    think.classList.remove("live");
    thinkTt.textContent = "已深度思考";
    thinkDur.hidden = false;
    thinkDur.textContent = ((Date.now() - thinkStart) / 1000).toFixed(1) + "s";
    if(!thinkTouched()) think.classList.remove("open"); // 用户没手动开过 → 维持收起（用户点开过则保留展开）
  }

  const run = document.createElement("div");
  run.className = "qa-run";
  run.innerHTML = qaRunPillHtml(`${qaEngine}${qaModel ? " · " + qaModelName(qaModel, qaEngine) : ""} · 生成中`);
  s.appendChild(run);
  /* ⚠️ 落笔即贴底（2026-09-23，与 qaRenderMine / qaRenderDone 同一约定）。
     为什么必须在**这一行之后**：这条应答的立身有两块 —— 气泡节点（上面那个 div）+ 这枚
     「生成中」胶囊行，两块加起来约 96px。它挂在**用户刚发的那条气泡之后**，用户那条贴了底、
     这一条不贴，画布底下就留着一条空白（实测 slack=96px）→ 用户看到的仍是「没滚到最新」，
     下一段正文到达时画面还会再往下跳一次。
     ⚠️ 闸门在 qaScrollIntoView 里（qaPaintBg：后台 run 在画时不拽动前台画布）—— 本函数的
     `host` 可指向后台画布，但滚动对象始终是可见画布，所以这里不必自己判。 */
  qaScrollIntoView();

  /* ===== 助手消息：流式段落缓冲（Claude / Codex 风格）=====
     模型逐 token 推送会撞碎 markdown 标签（** 加粗被切成 3 段）。
     策略：累积整段纯文本 → 按 \n\n 切块 → 已闭合段落立即走 marked 解析 →
     落到 DOM 的 <p>/<ul>/<pre> 节点上。**未闭合的尾部段落**用纯文本预览
     兜底（.qa-tail 类，便于区分"已渲染"vs"打字中"）。
     流式结束 / result 全文到达时,再把尾部段落也走一次解析。 */
  const body = div.querySelector(".qa-t");
  const segs = [];                       // 已渲染的 DOM 段落节点数组
  let pending = "";                      // 尚未到达 \n\n 的尾部累积文本

  function flushClosed(){
    // 把累积文本里已闭合的段落(以 \n\n 切分)逐段解析渲染,留尾部未闭合部分在 pending
    if(!pending) return;
    const idx = pending.lastIndexOf("\n\n");
    if(idx < 0) return;
    const ready = pending.slice(0, idx);
    pending = pending.slice(idx + 2);
    ready.split(/\n{2,}/).forEach(chunk=>{
      if(!chunk.trim()) return;
      /* 一段落成：走**统一落笔处**（19-attachments::qaShotsMount）而不是直接 mdToHtmlOnly ——
         bot 回复里带着**它看到的引用 / 它自己截的图**时，那些 /Users/…png 要变成缩略图，
         不能当文字念（2026-09-20 用户：「用户的截图显示对了 bot回复中显示图片还不对」）。
         没有截图路径时 qaShotsMount 内部就是 mdToHtmlOnly，语义与原来逐字相同。 */
      const tmp = document.createElement("div");
      qaShotsMount(tmp, chunk);
      // 把 tmp 下的每个块级节点追加到 body（而不是覆盖 body，因为已存在历史段落）
      [...tmp.childNodes].forEach(node=>{
        // 去掉 streaming 类的 cursor,实际只加在最后一个未闭合段落
        if(node.classList) node.classList.remove("streaming");
        body.appendChild(node);
        segs.push(node);
      });
    });
    // 重新上色本轮新加的代码块（避免重复:marked 已产出 .hljs,highlightIn 会检查 dataset.hl）
    highlightIn(body);
    // 同步给 streaming 类（光标）只在最后一个未闭合 tail 上
    const tail = body.querySelector(".qa-tail");
    if(tail) tail.classList.add("streaming");
  }
  function renderTail(){
    // 渲染尾部未闭合段落：纯文本预览（div 块级容器,避免被 inline 上下文挤压）
    const old = body.querySelector(".qa-tail");
    if(old) old.remove();
    if(!pending) return;
    const div = document.createElement("div");
    div.className = "qa-tail streaming";
    div.textContent = pending;
    body.appendChild(div);
  }
  function rerenderAll(text){
    // result/全文覆盖:重置整段 + 一次性解析（此时已知完整 markdown,无未闭合）
    // 走统一落笔处：正文里那些截图路径（引擎回话里常见）一并变成缩略图
    body.innerHTML = "";
    qaShotsMount(body, text);
    highlightIn(body);
    pending = "";
  }

  /* ===== 工具调用卡（Codex 体验）=====
     toolCall(name, argsPreview, argsFull) → { done(resultText, ok), state }
     · 运行中：实线卡片、spinner、秒数跳动，默认收起（点头部展开看输入/输出）
     · done()：状态切勾/叉、秒数定格、维持收起降调为静默单行（用户点开过则保持）
     · 卡片插在正文(.qa-t)之前 —— 顺序即 思考 → 工具 → 正文；正文已在流式时
       也允许工具穿插（insertBefore 保证工具行始终在正文块上方）
     2026-09-17 重构：卡片本体抽成 mkCard（只造不插）—— toolCall（插主流）、
     agentCall（委派卡）、subTool（嵌套进父委派卡）共用同一份结构，两处各写一份必然走形。
     2026-09-18：toolCall 按 description 分流 —— 带 description 的独立显示、不带 description 的归组
     （用户：「调用工具有 description 时要独立出来显示，没有的才在一起一个组显示」）。 */
  const bd = div.querySelector(".qa-bd");
  /* 卡片到达序号（2026-09-20）：折叠组里的卡片按**到达顺序**排 —— 独立卡超编被收编进组时
     要插回它该在的位置（组里还夹着无 description 的卡，直接 append 会把它排到整组之后）。 */
  let toolSeq = 0;
  /* isDesc：摘要是不是「人话」（description / command）—— 是则换正文字体渲染（见 .qa-tool-args.qa-desc）。
     传布尔而不是靠调用方自己加类：mkCard 是唯一一处写这份标记的地方（委派卡/子工具卡都复用）。 */
  function mkCard(name, argsPreview, argsFull, isDesc, meta){
    const card = document.createElement("div");
    card.className = "qa-tool";
    card.dataset.state = "running";
    card.dataset.seq = String(++toolSeq);
    const started = Date.now();
    /* 明细段统一成「代码面板帧」的形制（label 行 + 复制按钮 + 代码体，见 29-msg-variants
       的 qaCodeFrameNode 注释）：这里原来是裸 `<pre>` 加一枚 9px 小标题 —— 没有复制、
       也不横向滚动，长输出只能靠拖选，与 anywhere 的 CodePanelFrame 差一档。
       ⚠️ 复制按钮**不带监听**：点击走 #qa-stream 事件委托（07 的 bindQaStreamDelegation），
          否则快照恢复（`innerHTML = snapshot`）之后按钮就成了死的。 */
    card.innerHTML = `
      <div class="qa-tool-head">
        <span class="qa-tool-ico">${QA_ICONS.tool}</span>
        <span class="qa-tool-name"></span>
        <span class="qa-tool-args"></span>
        <span class="qa-tool-meta">
          <span class="qa-tool-dur">0.0s</span>
          <span class="qa-tool-st">
            <svg class="i-ok" viewBox="0 0 24 24"><path d="M4 12.5l5 5L20 6.5"/></svg>
            <svg class="i-err" viewBox="0 0 24 24"><path d="M6 6l12 12M18 6L6 18"/></svg>
          </span>
          <svg class="chev" viewBox="0 0 24 24"><path d="M6 9l6 6 6-6"/></svg>
        </span>
      </div>
      <div class="qa-tool-bd">
        <div class="qa-tool-sec v-in-sec">
          <div class="qa-sec-hd"><span class="k">输入</span>${QA_COPY_BTN}</div>
          <pre class="v-in"></pre>
        </div>
        <div class="qa-tool-sec v-sec" hidden>
          <div class="qa-sec-hd"><span class="k">输出</span>${QA_COPY_BTN}</div>
          <pre class="v-out"></pre>
        </div>
      </div>`;
    /* 人话标题（2026-09-28，对齐 anywhere 的 timelineToolTitle）：调用方给了 meta.title 时
       行里不再顶「内部工具名 + 参数 JSON」，而是写一句人话（「运行了 ls -la」「编辑 a.ts」）；
       原始名与参数降级进 title 属性（悬停可见）与明细的「输入」段（排障照旧拿得到）。
       ⚠️ 没给 meta 的老调用方（委派卡 / 子代理卡 / 未配对结果）行为逐字不变 —— 这是新增的
         可选形参，不是替换。 */
    const nameEl = card.querySelector(".qa-tool-name");
    const argsEl = card.querySelector(".qa-tool-args");
    if(meta && meta.title){
      nameEl.textContent = "";
      nameEl.hidden = true;
      argsEl.textContent = String(meta.title);
      argsEl.classList.add("qa-desc");
      argsEl.title = String(name || "tool")
        + (argsPreview || argsFull ? " · " + String(argsPreview || argsFull).slice(0, 300) : "");
      const kind = String((meta && meta.kind) || "tool").replace(/[^a-z_]/gi, "") || "tool";
      card.classList.add("qa-kind-" + kind);
      card.querySelector(".qa-tool-ico").innerHTML = qaToolIconFor(kind);
    }else{
      nameEl.textContent = String(name || "tool");
      argsEl.textContent = String(argsPreview || argsFull || "");
      if(isDesc) argsEl.classList.add("qa-desc");
    }
    const vInText = String(argsFull || argsPreview || "");
    if(vInText) card.querySelector(".v-in").textContent = vInText;
    else card.querySelector(".v-in-sec").hidden = true;
    const durEl = card.querySelector(".qa-tool-dur");
    const timer = setInterval(()=>{
      durEl.textContent = ((Date.now() - started) / 1000).toFixed(1) + "s";
    }, 100);
    // 开合点击走 #qa-stream 委托（见 bindQaStreamDelegation），此处不直接绑监听：
    // 直接绑的监听在快照恢复（innerHTML 重建）后会丢，卡片就点不开了。
    const api = {
      card,
      /* 追加一个明细段（文件变更行走这条，见 09 的 tool_use 分支）。
         插在「输出」段之前 → 段落顺序与 anywhere 的 ToolDetailPanel 一致：输入 / 变更 / 输出。 */
      addSection(node){
        if(!node) return null;
        const bdEl = card.querySelector(".qa-tool-bd");
        bdEl.insertBefore(node, card.querySelector(".v-sec"));
        card.classList.remove("qa-tool-bare");
        qaScrollIntoView();
        return node;
      },
      done(resultText, ok = true){
        clearInterval(timer);
        card.dataset.state = ok ? "ok" : "err";
        durEl.textContent = ((Date.now() - started) / 1000).toFixed(1) + "s";
        if(resultText != null && String(resultText).length){
          const outSec = card.querySelector(".v-sec");
          const outPre = card.querySelector(".v-out");
          const rt = String(resultText);
          outSec.hidden = false;
          /* 结果本身是 unified diff 时走 **Diff 面板**（2026-09-28）。
             引擎回执里常见「改完了 + 一段 diff 原文」；当普通文本摊开的话，
             +/- 混在正文里既看不出增删也不好读。对齐 anywhere：diff 一律走 DiffPanel
             （四列栅格 + 增删配色）。
             ⚠️ 只在**确实像** diff 时才替换：判定要同时有 `@@` 与 `---`/`+++`（见 qaLooksLikeDiff），
                否则普通输出里以减号开头的行会被误判成一整块 diff。
             ⚠️ 原文仍留在 pre 里（只是 hidden）：复制按钮取的就是 `.qa-tool-sec pre` 的文本，
                挪走它复制按钮就瞎了。 */
          outPre.textContent = rt;
          const asDiff = typeof qaLooksLikeDiff === "function" && qaLooksLikeDiff(rt) && !outSec.querySelector(".qa-diff");
          if(asDiff){
            outPre.hidden = true;
            const d = document.createElement("div");
            d.className = "qa-diff";
            d.innerHTML = qaDiffRowsHtml(qaDiffTextToRows(rt), true);
            outSec.appendChild(d);
          }
        }
        /* 「无明细」退化形态（对齐 anywhere：工具没有 command/output/changes 时退化成一条
           单行 marker，不带边框与底纹）。
           判据取**实际渲染后**的段内容而不是入参：多段拼装（文件变更行、追加段）之后，
           只有这时候才知道这条到底有没有东西可看。class 落在 HTML 上 → 快照可还原。 */
        const secs = card.querySelectorAll(".qa-tool-bd > .qa-tool-sec");
        let hasDetail = false;
        for(const s of secs){
          if(s.hidden) continue;
          const pre = s.querySelector("pre");
          if(pre ? pre.textContent.trim().length > 0 : s.childElementCount > 0){ hasDetail = true; break; }
        }
        card.classList.toggle("qa-tool-bare", !hasDetail);
        if(card.dataset.touched !== "1") card.classList.remove("open");
        qaScrollIntoView();
      },
      get state(){ return card.dataset.state; },
    };
    /* 落定钩子挂在**节点**上（2026-09-18）：流收尾要能把「配不到 tool_result id 的追加卡」
       也一起收掉 —— 一条事件装多次调用时，只有第一张挂得上 id，其余原来会永远转圈
       （卡片从折叠组挪成独立卡后更显眼，所以这里补上）。挂 DOM 而不是闭包数组，
       是因为收尾那一侧只有节点可拿；快照恢复出来的历史卡没有钩子 → 收尾自然跳过
       （它们本来就不是 running，扫不到）。 */
    card.__qaDone = api.done;
    return api;
  }
  /* 每调一次 = 一次调用（「调用了多少个工具」= 画了几张卡；一条事件里装了多次调用时，
     调用方会按 description 切成多个单元逐个调进来，见 qaToolUnits）。
     2026-09-18 用户：「调用工具有 description 时要独立出来显示，没有的才在一起一个组显示」
     → 按 isDesc 分流：
       · isDesc=true  → **独立卡**（.qa-tool-alone）直接落在正文流里（组容器之外）——
                        摘要是人话（description），本来就该直接看见，没必要跟一堆参数卡一起
                        藏进「已调用工具 · N个」里；但正文只留**最近 5 张**，超编的照样收编进组
                        （2026-09-20 追加，见下 ALONE_KEEP）。
       · isDesc=false → 并入折叠组（.qa-tools-bd），组计数 +1。
     组头也因此改成**懒启动**：第一张无 description 的卡到达时才点亮（见 qaStreamApplyOne
     的 tool_use 分支 —— 那里不再预先 beginTools），否则「全是 description」的一轮会留下一个 0个 的空组。
     2026-09-20 追加「正文只留最近 5 张独立卡」：见下面 ALONE_KEEP / groupAdd。 */
  /* ===== 正文里独立卡的保留数（2026-09-20 用户：「只显示最近的5个有 description 的，
     其他的在工具调用中显示」）=====
     带 description 的卡原先是**全部**独立落在正文流里，一轮长活跑下来正文被十几张卡顶得老长
     （截图实证：折叠组「19个」下面还挂着一长串独立卡，正文反而成了主角）。现在正文只留
     **最近 5 张**，更早的按到达顺序收编进「工具调用 · N个」折叠组 —— 组计数一并把它们算进去
     （它们确实已经在组里了，计数不跟着加就会出现「组里 6 张、写着 1个」的假账）。
     ⚠️ 只收编 toolCall 出来的独立卡：委派卡（.qa-agent）是里程碑、卡里还挂着子代理的**实时活动**，
     收进默认收起的组里等于把正在跑的委派藏起来 —— 不在此列。 */
  const ALONE_KEEP = 5;
  const aloneCards = [];                 // 已独立显示的 desc 卡，队首最老
  /* 进组：按到达序号插回该在的位置（组里夹着无 description 的卡，append 会打乱先后），
     顺手把组头点亮 / 计数 +1 —— 原先是 toolCall 里各写一份，现在只有这一处。 */
  function groupAdd(card){
    const seq = Number(card.dataset.seq || 0);
    const sib = [...toolsBd.children].find(el=> Number(el.dataset.seq || 0) > seq);
    if(sib) toolsBd.insertBefore(card, sib); else toolsBd.appendChild(card);
    if(!toolsStart) toolsBegin();                             // 启动工具调用统计行
    toolsCountEl.textContent = " · " + (++toolsNum) + "个";   // 实时更新计数
  }
  function toolCall(name, argsPreview, argsFull, isDesc, meta){
    const t = mkCard(name, argsPreview, argsFull, isDesc, meta);
    if(isDesc){
      t.card.classList.add("qa-tool-alone");
      bd.insertBefore(t.card, body);              // 独立显示：正文流里的一张卡
      aloneCards.push(t.card);
      while(aloneCards.length > ALONE_KEEP){      // 超出保留数：最老的那张收编进折叠组
        const old = aloneCards.shift();
        old.classList.remove("qa-tool-alone");    // 左细条是「不在组里」的标记，进组就摘掉
        groupAdd(old);
      }
    }else{
      groupAdd(t.card);                           // 归组显示：折叠容器内
    }
    qaScrollIntoView();
    return t;
  }
  /* ===== ⑤ 提问卡（AskUserQuestion）=====
     2026-09-18 用户：「返回了 askuserquestion，需要 ui 在对话框中增加选择显示 而不是显示这个 json」。
     引擎问「你想执行哪个操作」时，原来只落成一张普通工具卡：questions/options 的 JSON 摊在
     「输入」里，用户得自己读 JSON 再手打答案。这里换成**可点选的卡**：
       · 单问单选 → 点一下即答（不必再点提交）；
       · 多问 / 多选 → 勾完点「提交」，答案按「题 → 选项」拼成一段回给引擎；
       · 「其他」行 → 想选不在清单里的答案就直接写，回车/点确认即答。
     参数 JSON 原样留在卡的 title 上（要排障时鼠标停一下就能看到），但不再摊在卡面上。
     解析失败时调用方不会走到这里（回落普通工具卡），所以这里 qs 至少有 1 问。 */
  /* qs  = 归一化后的问题数组。两种来源共用这一种结构：
           · tool_use 参数 → qaAskParse（要猜三种不稳定形状，老版 CLI 只有这条路）
           · 统一 ask 事件 → qaAskFromEvent（magic-agent 2026-09-18 起归一化过的稳定形状）
     opts = { title, sid, rid }：title 是排障用的原始载荷；sid / rid = **答案要回到哪条会话**
          （追加通道的寻址信息，落 dataset，见下）。 */
  function askCard(name, qs, opts){
    const list = Array.isArray(qs) ? qs : [];
    const o = opts || {};
    const multi = list.length > 1 || list.some(q=>q.multiSelect);
    const card = document.createElement("div");
    card.className = "qa-ask";
    card.title = String(o.title || "").slice(0, 600);
    /* 答案要回到哪条会话 —— 落在 **dataset** 上而不是闭包里：
       快照恢复走 `innerHTML = snapshot`，闭包随旧节点一起丢，dataset 跟着 HTML 活下来，
       所以恢复出来的历史提问卡照样答得回去（REQ-22 锁的就是「已答态随快照恢复」）。 */
    if(o.sid) card.dataset.sid = String(o.sid);
    if(o.rid != null && o.rid !== "") card.dataset.rid = String(o.rid);
    card.innerHTML = `
      <div class="qa-ask-hd">
        <span class="qa-ask-ico">${QA_ICONS.ask}</span>
        <span class="qa-ask-tag"></span>
        <span class="qa-ask-ttl">提问 · 待回答</span>
      </div>
      <div class="qa-ask-list"></div>
      <div class="qa-ask-other">
        <input type="text" placeholder="其他：不在上面选项里就直接写，回车确认">
        <button type="button" class="qa-ask-go">确认</button>
      </div>
      <div class="qa-ask-ft"${multi ? "" : " hidden"}>
        <button type="button" class="qa-ask-sub" disabled>提交</button>
        <span class="qa-ask-hint">${list.length > 1 ? "共 " + list.length + " 问，都选完再提交" : "可多选 · 选完点提交"}</span>
      </div>
      <div class="qa-ask-ans" hidden><span class="k">你的回答</span><span class="v"></span></div>`;
    /* ⚠️ 2026-09-22 审美修订：这枚标签原来**直接把引擎的工具名画在卡头上**
       （`AskUserQuestion`，9px 等宽 + 蓝框）—— 用户看到的是英文实现细节，
       读不出「这张卡是在等我回答」。现在标签写人话「需要你确认」，
       工具名降级进 title（排障时鼠标一悬停仍能看到是哪个工具）。 */
    const tagEl = card.querySelector(".qa-ask-tag");
    if(tagEl){
      tagEl.textContent = "需要你确认";
      tagEl.title = "引擎提问工具：" + String(name || "AskUserQuestion");
    }
    const listEl = card.querySelector(".qa-ask-list");
    list.forEach((q, qi)=>{
      const qb = document.createElement("div");
      qb.className = "qa-ask-qb";
      qb.dataset.qi = String(qi);
      qb.dataset.multi = q.multiSelect ? "1" : "0";
      qb.dataset.q = q.question;
      const head = document.createElement("div");
      head.className = "qa-ask-q";
      if(q.header){
        const chip = document.createElement("span");
        chip.className = "qa-ask-chip";
        chip.textContent = q.header;
        head.appendChild(chip);
      }
      head.appendChild(document.createTextNode(q.question));
      qb.appendChild(head);
      const opts = document.createElement("div");
      opts.className = "qa-ask-opts";
      q.options.forEach((o)=>{
        const b = document.createElement("button");
        b.type = "button";
        b.className = "qa-ask-opt";
        b.dataset.label = o.label;
        b.setAttribute("role", q.multiSelect ? "checkbox" : "radio");
        b.setAttribute("aria-checked", "false");
        b.innerHTML = `<span class="mk"><svg viewBox="0 0 24 24"><path d="M4 12.5l5 5L20 6.5"/></svg></span>
          <span class="tx"><span class="lb"></span><span class="ds"></span></span>`;
        b.querySelector(".lb").textContent = o.label;
        b.querySelector(".ds").textContent = o.description;
        if(!o.description) b.querySelector(".ds").remove();
        opts.appendChild(b);
      });
      qb.appendChild(opts);
      listEl.appendChild(qb);
    });
    bd.insertBefore(card, body);
    qaScrollIntoView();
    let settled = false;                      // 这一问是否已被引擎收尾（见 done）
    return {
      card,
      /* 引擎这一问的收尾（tool_result / 流结束兜底）。**卡不会因此失效**：
         多数引擎在非交互模式下收不到回答就自己结束了，此时用户点选项仍然是把答案
         作为追问发回去 —— 那正是这张卡存在的意义，所以这里只更新标题、不禁用。 */
      done(){
        if(!qaAskAnswered(card)){
          const ttl = card.querySelector(".qa-ask-ttl");
          if(ttl) ttl.textContent = "提问 · 待回答";
        }
        settled = true;
      qaScrollIntoView();
    },
    /* 统一 ask 事件后到（magic-agent 会为同一问先发 tool_use、再发 ask）→ 不重画第二张卡，
       只把「答回去要用的东西」补到这张已有的卡上（sid/rid，见 qaStreamApplyOne 的 ask 分支）。 */
    bindAsk(o2){
      const s2 = (o2 && o2.sid) || "";
      if(s2) card.dataset.sid = String(s2);
      if(o2 && o2.rid != null && o2.rid !== "") card.dataset.rid = String(o2.rid);
    },
    get state(){ return settled ? "ok" : "running"; },
    };
  }
  /* ① 子代理委派卡（.qa-tool.qa-agent）：工具卡变体 + 状态胶囊 + 嵌套活动区。
     生命周期由 ② 的 system 事件驱动（start/progress/notify 只改胶囊）；
     最终结果仍按 tool_use id 配对写进「输出」（done）—— 没等到 task_notification
     就收账时，胶囊在 done 里一并落定，不会留下常驻转圈。 */
  function agentCall(name, argsPreview, argsFull, promptText){
    // 委派卡的摘要本来就是 description（见调用处的 desc 取值）→ 按人话渲染
    const t = mkCard(name, argsPreview, argsFull, true);
    const card = t.card;
    card.classList.add("qa-agent");
    card.querySelector(".qa-tool-ico").innerHTML = QA_ICONS.agent;
    const chip = document.createElement("span");
    chip.className = "qa-agent-chip";
    chip.textContent = "等待启动";
    card.querySelector(".qa-tool-meta").prepend(chip);
    const bdEl = card.querySelector(".qa-tool-bd");
    if(promptText != null && String(promptText).length){
      const sec = document.createElement("div");
      sec.className = "qa-tool-sec";
      /* 段头与「输入 / 输出」同一形制（label + 复制按钮）：任务词是这条委派里最该被复制走的
         东西（用户常要拿它去别处复用），而它原来连复制入口都没有。 */
      sec.innerHTML = `<div class="qa-sec-hd"><span class="k">任务词</span>${QA_COPY_BTN}</div><pre></pre>`;
      sec.querySelector("pre").textContent = String(promptText);
      bdEl.insertBefore(sec, bdEl.firstChild);    // 任务词置顶（比原始参数 JSON 更可读）
    }
    const sub = document.createElement("div");
    sub.className = "qa-sub";
    sub.innerHTML = `<div class="qa-sub-h">子代理活动</div>`;
    bdEl.appendChild(sub);
    bd.insertBefore(card, body);
    qaScrollIntoView();
    const api = {
      done(resultText, ok = true){
        t.done(resultText, ok);
        if(chip.classList.contains("run")){       // 通知缺失时兜底落定（完成 → jade 绿）
          chip.classList.remove("run");
          if(ok) chip.classList.add("ok");
          chip.textContent = ok ? "完成" : "未完成";
        }
      },
      get state(){ return t.state; },
      start(){ chip.textContent = "运行中"; chip.classList.add("run"); },
      progress(label){ if(chip.classList.contains("run")) chip.textContent = String(label || "进行中"); },
      notify(label, ok = true){
        chip.classList.remove("run");
        if(ok) chip.classList.add("ok");
        chip.textContent = String(label || (ok ? "完成" : "已结束"));
      },
      subTool(nm, pv, full, isDesc){              // ③ 子代理内部工具 → 嵌套卡（样式更紧凑）
        const st = mkCard(nm, pv, full, isDesc);
        st.card.classList.add("qa-tool-sub");
        sub.appendChild(st.card);
        qaScrollIntoView();
        return st;
      },
      subText(txt){                               // ③ 子代理正文 → 嵌套区文本行
        const el = document.createElement("div");
        el.className = "qa-sub-t";
        el.textContent = String(txt || "");
        sub.appendChild(el);
        qaScrollIntoView();
      },
      subNote(txt){                               // ③ 子代理思考/回执 → 嵌套区注脚
        const el = document.createElement("div");
        el.className = "qa-sub-n";
        el.textContent = String(txt || "");
        sub.appendChild(el);
        qaScrollIntoView();
      },
    };
    /* 委派卡也要能被流收尾兜底落定（胶囊 + 卡状态一起）→ 覆盖 mkCard 挂的那个钩子 */
    card.__qaDone = api.done;
    return api;
  }
  /* ② 无主系统事件的降级行（父委派卡缺席时）。
     2026-09-28 起改走**单行标记行**（29-msg-variants 的 qaMarkerNode）：原来只是一行 10px
     灰字（`.qa-note`），事件里带的 JSON 与状态**全丢**；现在同一格既能给纯文本注脚，
     也能给「图标 + 标题 + 状态徽章 + 可展开明细」的完整形态（对齐 anywhere 的 Marker/JsonMarker）。
     ⚠️ 保留 note(text) 这个出口且签名不变：13-dispatch-stream / 24-agent-test 等旧调用方还在用，
        它们自动升格成标记行（信息只多不少）。 */
  function markerNode(opts){
    const n = qaMarkerNode(opts);
    bd.insertBefore(n, body);
    qaScrollIntoView();
    return n;
  }
  function note(text, opts){
    return markerNode(Object.assign({ title: String(text || "") }, opts || {}));
  }
  /* ④ 分隔条（横切整条会话的事件，如上下文压缩边界） */
  function sepNode(text, opts){
    const n = qaSepNode(text, opts);
    bd.insertBefore(n, body);
    qaScrollIntoView();
    return n;
  }

  return {
    el: div,                                   // 这条消息的根节点（消费方要给整条消息加形态时用：
                                               //   审查轮那条「审核引擎的回复」贴右 = 加个 qa-engine，见 13-dispatch-stream）
    think,
    body,
    beginThinking: thinkBegin,
    endThinking: thinkEnd,
    beginTools: toolsBegin,
    endTools: toolsEnd,
    appendThinking(t){ thinkBegin(); thinkBd.textContent += t; },
    appendText(t){
      pending += t;
      flushClosed();
      renderTail();
    },
    setResult(text){ rerenderAll(text); },
    toolCall,
    agentCall,
    askCard,                                   // ⑤ 提问卡（AskUserQuestion → 可点选项）
    note,
    marker: markerNode,                        // ② 单行标记行（图标 + 人话标题 + 状态 + 可展开明细）
    sep: sepNode,                              // ④ 分隔条（横切整条会话的事件）
    finish(){
      // 流式结束:把尾部 pending 也按 markdown 解析一次（行内标记如 ** / ` / []() 此时闭合完整）
      toolsEnd();                              // 收工具调用统计行
      settleRunningTools(true);                // 还在转圈的卡一律落定（配不到回执的追加卡 / 点停止那条路）
      if(pending){
        /* 尾部那段也走统一落笔处（理由同 flushClosed）：截屏路径 → 缩略图 */
        const tmp = document.createElement("div");
        qaShotsMount(tmp, pending);
        [...tmp.childNodes].forEach(node=> body.appendChild(node));
        highlightIn(body);
        const old = body.querySelector(".qa-tail");
        if(old) old.remove();
        pending = "";
      }
      body.classList.remove("streaming");
      const t = body.querySelector(".qa-tail"); if(t) t.classList.remove("streaming");
    },
    run,
    /** 改「生成中」胶囊上的文字。
     *  为什么要它（2026-09-21）：胶囊初值是渲染层按**链首**拼的（`qaModel`，见 04-task-list.js），
     *  而这一轮真正用哪个模型是**主进程**按配置界面的降级链定的 —— 前一个没额度就换下一个重试
     *  （同一个 rid 会再推一条 `start`，带上实际模型）。没有这个出口，界面就会写着 A、实际跑的是 B。
     *  ⚠️ 只动胶囊文字，不重建节点（重建会打断 `.qa-run` 的转圈与裙边，见 verify-dark-chat 的断言）。 */
    setRunLabel(t){
      const el = run.querySelector(".qa-run-t");
      if(el) el.textContent = String(t || "");
    },
    /** 改头像右边的**引擎 · 模型小标**（默认是渲染时按当前引擎挂的，见 qaRenderAnswer）。
     *  说话人不是当前引擎时用（派发轮 = 该需求的引擎 / 模型；被测引擎 = 事件里的引擎）。
     *  引擎传空串 = 摘掉小标（宁可没有，不写「未知」）；模型空 = 只显示引擎名。 */
    setWho(engine, model){
      const ico = div.querySelector(".qa-ico"); if(!ico) return;
      const lb = qaWhoLabel(engine, model);
      let w = ico.querySelector(".qa-who");
      if(!lb){ if(w) w.remove(); return; }
      if(!w){ w = document.createElement("span"); w.className = "qa-who"; ico.appendChild(w); }
      w.textContent = lb;
    },
  };
}
/** 把「机器内部活动」搬到**左边那条独立消息**里 —— 审核引擎与数字分身的「深度思考 / 工具调用」。
 *
 *  用户 2026-09-22 原话：「审核和数字分身自己的思考和工具调用要显示在左边 但是代表用户身份追问的
 *  消息要显示在右边」。上一版只在**右边那条消息内部**把这两块靠到左边（`app.css` 的 M4），
 *  用户看过后仍要「显示在左边」—— 所以这里升级成**真的搬到左边一条消息**：左边这半边的语义是
 *  「机器在动」（与 bot 答复那侧同一条左边线、头像也在左），右边那半边才是「谁在说话」。
 *
 *  ⚠️ **搬节点，不重建**：`qaRenderAnswer` 的句柄里攥着 `thinkBd` / `toolsBd` / 计数 / 计时那些元素引用，
 *     重建会让流式期间的「思考中 · X.Xs」「工具调用 · N个」全部失联（表现为数字不再跳、正文不再进）。
 *     搬完引用依旧有效 —— 这正是「思考 / 工具」能跟着引擎实时更新、而外层换了个容器也不受影响的原因。
 *  ⚠️ 新消息**先 hidden**：这两块在首条思考 / 首次工具调用之前是隐藏的，空着的一条消息会白占高度。
 *     显形由 `thinkBegin` / `toolsBegin` 里那两行 `__traceWrap.hidden = false` 负责（同文件上方）。
 *  ⚠️ 头像与右边那条**同一个**（`分` / `审`）：机器的活动属于**它自己**，只是「机器的事在左、人的话在右」；
 *     两枚头像的样式都是自包含的（`.qa-av.qa-av-sim` / `.qa-av-txt.qa-av-rev` 自带底色与字色），
 *     所以直接拷 ico 的 innerHTML 就与右边那条长得一模一样。
 *  ⚠️ **历史快照**里这两块仍在右边那条消息里（存的是当时的 HTML）—— 那条路由 `app.css` 的 M4（`:has()`）兜住：
 *     内部块回到左边（没有独立头像，但仍在左半边的左边线上）。 */
function qaTraceToLeft(h, engine, model){
  const el = h && h.el;
  if(!el || !el.parentNode) return null;
  const parts = ["qa-think", "qa-tools"].map((c) => el.querySelector("." + c)).filter(Boolean);
  if(!parts.length) return null;
  const wrap = document.createElement("div");
  wrap.className = "qa-msg qa-trace";
  wrap.hidden = true;                              // 两块都还藏着 → 整条不占位
  wrap.innerHTML = `<div class="qa-ico"></div><div class="qa-bd"></div>`;
  const tico = wrap.querySelector(".qa-ico");
  const ico = el.querySelector(".qa-ico");
  if(ico) tico.innerHTML = ico.innerHTML;
  /* 头像右边挂**机器侧自己的引擎 · 模型**：先摘掉从右边那条拷来的旧 `.qa-who`
     （右边那条若是左侧形状会自带一枚，那是说话人的，不是这台机器的），再按 engine/model 重挂。 */
  const oldWho = tico.querySelector(".qa-who"); if(oldWho) oldWho.remove();
  tico.insertAdjacentHTML("beforeend", qaWhoHtml(engine, model));
  const bd = wrap.querySelector(".qa-bd");
  parts.forEach((p) => bd.appendChild(p));         // 搬节点（引用不断，见上）
  el.parentNode.insertBefore(wrap, el);            // 排在右边那条**之前** = 机器在左、话在右
  el.__traceWrap = wrap;                           // 首次显形时由 thinkBegin / toolsBegin 摘 hidden
  return wrap;
}
function qaRenderDone(ok, label, host){
  // host = 这一轮的落笔处（见 qaRunHost）：后台跑的收尾行要落进它自己的画布，
  // 不能写死 #qa-stream —— 否则「切走那条跑完了」会在**当前显示的**会话里凭空多出一行收尾。
  const s = host || $("#qa-stream"); if(!s) return;
  const d = document.createElement("div");
  d.className = "qa-done" + (ok ? "" : " err") + " qa-pin";
  d.innerHTML = `<span class="d"></span><span></span>`;
  d.querySelector("span:last-child").textContent = label;
  s.appendChild(d);
  qaScrollIntoView();
}
