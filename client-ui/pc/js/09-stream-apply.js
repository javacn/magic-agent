/* ---------- 统一提问事件（magic-agent 2026-09-18 起 `{"type":"ask", …}`）----------
   与 qaAskParse 同一个出口结构（askCard 只认那一种），但**不用猜形状** —— magic-agent 已经把
   两种 wire 形态（tool_use 的三种编码 / SDK 层 control_request）归一化过，且额外带回两样东西：
     · `session_id`  —— 追加通道的寻址 id（答案要靠它 `--append` 回同一条会话）；
     · `kind`        —— question（模型提问）/ permission（工具待授权）。
   两种事件都要认：老版 CLI 只发 tool_use（走 qaAskParse），新版两条都发（tool_use 先到、
   ask 后到 → 由 qaStreamApplyOne 去重，不会画两张卡）。 */
function qaAskFromEvent(ask){
  if(!ask || typeof ask !== "object") return null;
  if(ask.kind && ask.kind !== "question") return null;     // 待授权（permission）暂不走提问卡
  const qs = Array.isArray(ask.questions) ? ask.questions : [];
  const out = qs.map(q=>({
    header: String(q.header || ""),
    question: String(q.text || q.question || ""),           // 统一格式用 text；老形状用 question
    multiSelect: !!(q.multi_select || q.multiSelect),
    options: (Array.isArray(q.options) ? q.options : [])
      .map(o=>({ label: String(o.label || ""), description: String(o.description || "") }))
      .filter(o=>o.label),
  })).filter(q=>q.question || q.options.length);
  return out.length ? out : null;
}

function qaStreamApply(ev, ctx){
  for(const e of qaEventNormalize(ev)) qaStreamApplyOne(e, ctx);
}

/* 「这一轮里主进程**自动换过模型**」的 rid 集合（2026-09-21）。
   为什么需要记：换完模型主进程会把跑通的那个**升级成链首**（promoteModel），
   但那只在 close 前做 —— 所以 `start` 那一刻链首还是旧的。到 `end` 时再重取一次链，
   「自动」档显示的名字才会跟上新链首（否则首页那枚下拉还写着那个已经跑不通的模型）。
   用 Set 按 rid 记（不是全局布尔）：同一刻可能有多条会话在跑，布尔会被互相踩掉。 */
const qaModelSwitched = new Set();

function qaStreamApplyOne(ev, ctx){
  const h = ctx && ctx.h; if(!h || !ev) return;
  const agents = ctx.agents || (ctx.agents = new Map());   // tool_use_id → 委派卡（①③②共用）
  // parent 双拼写兼容：归一化事件带 `parent`；引擎自己扁平化但保留原字段的事件带 parent_tool_use_id
  const evParent = ev.parent != null ? String(ev.parent)
    : (ev.parent_tool_use_id != null ? String(ev.parent_tool_use_id) : null);
  const agOf = (id)=> id != null ? agents.get(id) : null;
  const finishTools = (ok)=>{                 // 流结束/失败时兜底：还在转圈的工具卡一律收账
    ctx.tools.forEach(tc=>{ if(tc.state === "running") tc.done("", ok); });
    ctx.tools.clear();
    agents.forEach(a=>{                       // 漏登记的委派卡也落定胶囊（正常路径 done 已处理）
      if(a.state === "running" && typeof a.notify === "function") a.notify("已结束", ok);
    });
    if(typeof h.endTools === "function") h.endTools();  // 收工具调用统计行
  };
  switch(ev.type){
    /* 一轮开始：主进程把**这一轮真正会用的模型**带回来了。三件事都要做（2026-09-21）：
       ① 「生成中」胶囊写成实际模型 —— 配置里的模型链会自动降级（前一个没额度就换下一个），
          换完还写着原来那个就是「界面说 A、实际跑 B」；
       ② 把这个实际模型**记回这条任务**（`qaTaskSetModel`）→ 任务对话那枚下拉的文案与
          选中项当场变成实际在跑的那个（用户要的「自动切换模型的时候 对话框下拉的选中
          模型显示也要变」），下次追问也不会先试那个死掉的；
       ③ 若用户**手选过**模型（`qaModelPick` 非空）而它正是被换掉的那个 → 一并改掉
          （别再拿一个刚被证明跑不通的模型当「新建任务默认」）；
          `ev.attempt > 0` = 这是换过模型之后的那一轮（主进程只在降级重试时才带它）。
       取不到出口（旧手柄 / 没有 taskId）就只做能做的，不抛。 */
    case "start": {
      const eng = String(ev.engine || qaEngine || "");
      const mdl = String(ev.model || "");
      const retry = Number(ev.attempt || 0) > 0;
      if(typeof h.setRunLabel === "function"){
        h.setRunLabel(`${eng}${mdl ? " · " + qaModelName(mdl, eng) : ""} · 生成中`);
      }
      /* 头像右边的「引擎 · 模型」小标同一时机跟成实际值（与胶囊同一条口径：
         界面说 A、实际跑 B 不行）。机器侧 `.qa-trace`（审查轮 / 数字分身的活动行）
         是 qaTraceToLeft 另建的节点，句柄管不到它 —— 走 `__traceWrap` 直接改文字。 */
      if(typeof h.setWho === "function") h.setWho(eng, mdl);
      const tw = (h.el && h.el.__traceWrap) ? h.el.__traceWrap.querySelector(".qa-who") : null;
      if(tw) tw.textContent = qaWhoLabel(eng, mdl);
      if(mdl && retry) qaModelSwitched.add(ev.rid);   // 记一笔：这轮换过模型（end 时重取链，见 Set 注释）
      if(mdl && ctx && ctx.taskId && typeof qaTaskSetModel === "function") qaTaskSetModel(ctx.taskId, mdl);
      if(mdl && retry && qaModelPick && qaModelPick !== mdl && typeof qaModelSet === "function"){
        qaModelSet(mdl, "home2", null);   // 手选的那个跑不通 → 换成实际跑通的（首页下拉跟着变）
      }
      /* ⚠️ 降级**落到「引擎默认」**（不带 -m）时也要如实回写（2026-09-21）：
         主进程的候选链在链尾多一格「引擎默认」（见 desktop/chat-model.cjs::allowEngineDefault）——
         那一格跑通时 `model` 就是空串。空串是**合法态**，但上面几条 `mdl &&` 守卫会整体跳过，
         于是任务卡与下拉会继续写着那个已经跑不通的模型（实测：openclaw 指定模型 → 401 认证失败
         → 已改用引擎默认答完，界面却还显示那个模型）。这里把它清成「自动」= 与实际相符。 */
      if(retry && !mdl){
        qaModelSwitched.add(ev.rid);      // 同样要记一笔：end 时重取链，下拉跟上新状态
        if(ctx && ctx.taskId && typeof qaTaskSetModel === "function") qaTaskSetModel(ctx.taskId, "");
        if(qaModelPick && typeof qaModelSet === "function") qaModelSet("", "home2", null);
      }
      break;
    }
    case "thinking":
      // ③ 子代理思考 → 父卡嵌套区注脚；主会话思考 → 思考区
      if(evParent && agOf(evParent)) agOf(evParent).subNote(ev.text || "");
      else {
        h.appendThinking(ev.text || "");
        /* 过程步骤（2026-09-23）：主会话思考也记进台账 —— 收尾时随消息一起落库，
           历史重画时按 kind 分流成折叠块（见 27-conv-log.js）。只记**主会话**：
           与 run.msgText 同一口径，子代理内部的过程属于委派卡，不摊进对话流。 */
        if(ctx && typeof ctx.onStep === "function") ctx.onStep({ type: "thinking", text: String(ev.text || "") });
      }
      break;
    case "text":
      // ③ 子代理正文 → 父卡嵌套区；主会话正文 → 逐字正文
      if(evParent && agOf(evParent)) agOf(evParent).subText(ev.text || "");
      else {
        h.appendText(ev.text || "");
        /* 消息台账（2026-09-22）：主会话正文逐段累加进 run.msgText —— 收尾时它就是要落进
           消息文件的那条「引擎说了什么」（持久层只存消息，不存 HTML，见 27-conv-log.js）。
           ⚠️ 只累计**主会话**正文：子代理（委派卡）里的话不属于这条对话的正文。 */
        if(ctx && typeof ctx.onText === "function") ctx.onText(ev.text || "");
      }
      break;
    /* ⑤ 统一提问事件（magic-agent 2026-09-18 起）：卡面直接由它渲染 —— 形状稳定，不必再猜
       tool_use 参数里的三种形态；它还把 session_id 带回来了（追加通道的寻址 id，见 qaAskSend）。 */
    case "ask": {
      const sid = ev.session_id ? String(ev.session_id) : "";
      /* 顺手把会话 id 绑到会话上：它来自 init 行，比 result 行早得多 —— 这一轮就算中途被打断，
         会话锚点也不会丢（README「会话 id 闭环」那条要求）。 */
      if(sid && typeof ctx.onSession === "function") ctx.onSession(sid);
      const tid = (ev.ask && ev.ask.tool_use_id) ? String(ev.ask.tool_use_id) : "";
      /* 同一问会走两条路各报一次（tool_use 事件先到、这条后到）→ 已经画过卡的只**补寻址信息**，
         不再画第二张。补的是卡上的 dataset.sid/rid：快照恢复后照样答得回去（见 askCard）。 */
      const prev = tid ? ctx.tools.get(tid) : null;
      if(prev && typeof prev.bindAsk === "function"){
        prev.bindAsk({ sid, rid: ctx.rid });
        break;
      }
      const qs = qaAskFromEvent(ev.ask);
      if(qs && typeof h.askCard === "function"){
        const tc = h.askCard((ev.ask && ev.ask.tool_name) || "AskUserQuestion", qs,
          { title: ev.text || "", sid, rid: ctx.rid });
        if(tid) ctx.tools.set(tid, tc);
      }
      break;
    }
    case "tool_call":
    case "tool_use": {
      // magic-agent 实测事件名 tool_use（旧约定 tool_call）：载荷 { id, name, text(参数JSON) }
      /* ⚠️ 这里**不再**预先 h.beginTools()（2026-09-18）：带 description 的调用走独立卡、不进组，
         组头改由第一张**无 description** 的卡到达时点亮（见 toolCall）—— 预先点亮会让
         「这一轮工具全带 description」时留下一个「已调用工具 · 0个」的空组。 */
      const tid = ev.tid != null ? ev.tid : ev.id;
      const argsText = ev.text || ev.args || "";
      const ag = agOf(evParent);
      /* 提问卡判定：qaAskParse 只跑一次（形状不稳、解析不便宜，下面还要用结果） */
      const askQs = (typeof h.askCard === "function" && qaIsAskTool(ev.name)) ? qaAskParse(argsText) : null;
      if(/^agent$|^task$/i.test(String(ev.name || "")) && !ag){
        // ① 主会话委派：专属卡（任务词 + 状态胶囊 + 嵌套区），按 id 登记
        let o = ev.inputObj || null;
        if(!o && argsText){ try { o = JSON.parse(argsText); } catch(e2){ /* 非 JSON 参数只显示原文 */ } }
        const desc = (o && (o.description || o.subagent_type)) || ev.preview || "";
        const prompt = (o && (o.prompt || o.task || o.promptText)) || "";
        const card = typeof h.agentCall === "function"
          ? h.agentCall(ev.name || "Agent", String(desc || ""), argsText, String(prompt))
          : h.toolCall(ev.name || "Agent", ev.preview || "", argsText);   // 旧手柄无 agentCall → 退普通卡
        if(tid != null){ ctx.tools.set(tid, card); agents.set(String(tid), card); }
      }else if(ag && typeof ag.subTool === "function"){
        // ③ 子代理内部工具（含子代理再委派）→ 父卡嵌套区，结果仍按 id 配对
        // 参数里有 description 就把它当摘要显示（人话优先于参数 JSON）；嵌套区不按它拆卡
        const u = qaToolUnits(ev.name, argsText)[0];
        const tc = ag.subTool(u.name, u.desc || ev.preview || "", u.args, !!u.desc);
        if(tid != null) ctx.tools.set(tid, tc);
      }else if(typeof h.askCard === "function" && qaIsAskTool(ev.name) && askQs){
        /* ⑤ 提问（AskUserQuestion）→ 可点选的提问卡，而不是把 questions JSON 摊在工具卡里
           （2026-09-18 用户：「需要 ui 在对话框中增加选择显示 而不是显示这个 json」）。
           ⚠️ 解析失败（qaAskParse → null）时不走这里 → 回落下面的普通工具卡：宁可不好看，
           也不能把引擎问的话丢掉。
           ⚠️ 新版 magic-agent 会为同一问**再发一条统一 ask 事件**（带 session_id）→ 那条到达时
           只给这张卡补 sid/rid，不再画第二张（见上面 case "ask" 的去重）。 */
        const tc = h.askCard(ev.name || "AskUserQuestion", askQs, { title: argsText });
        if(tid != null) ctx.tools.set(tid, tc);
      }else{
        /* 普通工具：按 qaToolUnits 切分 —— 一条事件里装了几次调用就画几张卡（中间就是各自的
           description 文本）。切出来的单元按**有没有 description** 分流（见 toolCall）：
           带 description 的独立显示在正文流里，不带的归进折叠组 —— 组计数因此只数组内卡。
           ⚠️ 结果回执（tool_result）只带一个 id，配不齐多张卡 → 只把 id 挂给第一张，
           其余由流收尾（finishTools）兜底落定，不会留下常驻转圈。 */
        /* 参数对象：人话标题与「文件变更」推导都要用（ev.inputObj 优先，其次自己解一次 JSON）。
           解不动 = 非 JSON 参数（命令行文本等）→ 保持 null，回落原来的参数摘要在行里。 */
        let argsObj = ev.inputObj || null;
        if(!argsObj && argsText){ try{ argsObj = JSON.parse(String(argsText).trim()); }catch(e2){} }
        const units = qaToolUnits(ev.name, argsText);
        units.forEach((u, i)=>{
          /* 人话标题（2026-09-28，对齐 anywhere 的 timelineToolTitle）：行里不再顶
             「内部工具名 + 参数 JSON」，改写「运行了 <命令>」「编辑 <文件>」「搜索了 <词>」
             这类句子；原始名与参数降级进行 title 与明细「输入」段，排障照旧拿得到。
             多单元（一条事件装多次调用）时每个单元按**自己的**参数生成标题 ——
             否则三张卡会写着同一句话。 */
          let uo = argsObj;
          if(units.length > 1 && u.args){ try{ uo = JSON.parse(String(u.args).trim()); }catch(e3){ uo = null; } }
          /* 形态：本地判定与服务端归一化层给的 `tool_kind` 合并（见 qaToolKindMerge）——
             两个界面由此共用一份词表，本地名字表没见过的引擎也不至于掉进通用卡。 */
          const uKind = qaToolKindMerge(u.name, uo, ev.tool_kind);
          const tc = h.toolCall(u.name, u.desc || ev.preview || "", u.args, !!u.desc,
            { kind: uKind, title: qaToolHumanTitle(u.name, uo, u.args || argsText) });
          /* 文件变更（2026-09-28）：magic-agent 不发 file_change 事件，但**改动就在参数里**
             （Edit 的 old/new_string、MultiEdit 的 edits[]、Write 的 content）——
             从参数推导一条「文件变更行 + Diff」贴进明细，用户不必再读参数 JSON 才知道改了哪几行。
             ⚠️ 推导的 diff 没有上下文行与行号（见 qaDerivedDiffRows），所以不画 hunk、不编行号。 */
          const fc = uKind === "file_change" ? qaFileChangeOf(u.name, uo) : null;
          if(fc && tc && typeof tc.addSection === "function") tc.addSection(qaFileChangeNode(fc));
          if(tid != null && i === 0) ctx.tools.set(tid, tc);
        });
      }
      /* 过程步骤（2026-09-23）：无论哪个分支，工具调用本身都记进台账（收尾时落库）。
         同一问的重复事件在上面 bindAsk 处已提前 break，不会记两遍；tid 随条目带上，
         tool_result 到达时按它配对补结果（见 bindHomeAsk 的 onStep）。 */
      if(ctx && typeof ctx.onStep === "function")
        ctx.onStep({ type: "tool", name: String(ev.name || "tool"), args: String(argsText || ""), tid: tid != null ? String(tid) : "" });
      break;
    }
    case "tool_result": {
      // 期望载荷: { tid|id, text(结果串,可缺省), ok(布尔,缺省 true) }
      const tid = ev.tid != null ? ev.tid : ev.id;
      const tc = tid != null ? ctx.tools.get(tid) : null;
      if(tc) tc.done(ev.text || "", ev.ok !== false);
      else if(evParent && agOf(evParent) && typeof agOf(evParent).subNote === "function")
        agOf(evParent).subNote("结果回执：" + String(ev.text || "").slice(0, 400)); // ③ 无卡可填 → 父卡注脚
      else if(typeof h.marker === "function"){
        /* ④ 未配对结果 → **单行标记行** + 可展开明细（2026-09-28）。
           原来是「未配对结果」工具卡：一张带「输入 / 输出」两段的卡，而输入永远是空的 ——
           形态比内容还重。对齐 anywhere：配不到卡片的结果落成一条 marker，
           正文留在可展开的明细里，且状态用徽章说清（不再只有一个「叉」）。
           ⚠️ 仍然**不丢内容**：这条路径存在的意义就是「配不上对也不静默丢弃」。 */
        const fail = ev.ok === false;
        h.marker({
          icon: QA_V_ICONS.terminal,
          title: "未配对结果" + (tid != null && tid !== "" ? " · " + String(tid).slice(0, 14) : ""),
          status: fail ? "failed" : "completed",
          destructive: fail,
          detail: ev.text ? String(ev.text) : null,
          detailLabel: "output",
          hint: "这条结果配不到对应的工具卡（工具事件缺 id，或结果晚于收尾到达）",
        });
      }
      /* 过程步骤（2026-09-23）：结果也进台账 —— 配不上对的由 onStep 内部兜成独立条目 */
      if(ctx && typeof ctx.onStep === "function")
        ctx.onStep({ type: "tool_result", tid: tid != null ? String(tid) : "", text: String(ev.text || ""), ok: ev.ok !== false });
      break;
    }
    case "system": {
      /* ② 生命周期：task_started / task_progress / task_notification → 驱动父委派卡胶囊。
         父卡缺席、以及**其余 subtype** → 单行标记行（2026-09-28）。
         原来是 h.note 打一行 10px 灰字：事件里的 task_id / usage / tool_use_id / status
         这些排障要的字段**全丢**，只留下「系统 · xxx」三个字。
         现在统一成 anywhere 的 Marker 形态：图标 + 人话标题 + 状态徽章 + 可展开的原始载荷。 */
      const st = String(ev.subtype || "");
      const ag = ev.tool_use_id != null ? agents.get(String(ev.tool_use_id)) : null;
      const sysDetail = (label)=>({
        kind: "system", subtype: st, label: label,
        task_id: ev.task_id, tool_use_id: ev.tool_use_id, status: ev.status, usage: ev.usage,
      });
      const sysMarker = (title, opts)=> (typeof h.marker === "function")
        ? h.marker(Object.assign({ icon: QA_V_ICONS.clock, title: title, detail: sysDetail(title) }, opts || {}))
        : (typeof h.note === "function" ? h.note(title) : null);
      /* ④ 上下文压缩边界 → **分隔条**（anywhere 的 `compact` kind 就是这一格，2026-09-28）。
         为什么走分隔条而不是标记行：压缩是**横切整条会话**的分界，不是「某件事发生了」，
         左右细线夹一行字正好说清「上下文在这儿被截断重排过」—— 后续回答忽然「忘了前面」时，
         用户得有这条凭据。
         ⚠️ 触发面：magic-agent 自己**不转发** system 事件（它只读 result / system:init 的 subtype），
            所以这条路实际只会在**被测引擎 / 原生流**上到达（claude 自动压缩时发的 system 事件）。
         ⚠️ 子类型名用 `/compact/i` 宽松匹配：本仓库没有固化它的 fixture，**不硬编**一个具体
            字符串（编错了的表现是静默不触发，比不写还糟）。
         ⚠️ 「进行中 / 已完成」按**事件自己的字段**判（subtype 里的字样 + status），不猜。 */
      if(/compact/i.test(st)){
        const active = /start|begin|progress/i.test(st)
          || ev.status === "running" || ev.status === "pending";
        if(typeof h.sep === "function"){
          h.sep(active ? "会话上下文压缩中" : "会话上下文已压缩", {
            title: "上下文压缩边界（system/" + st + "）—— 之后的内容可能不再包含更早的上下文",
          });
          break;
        }
      }
      if(st === "task_started"){
        if(ag && typeof ag.start === "function") ag.start();
        else sysMarker("子代理已启动" + (ev.task_id ? " · " + String(ev.task_id).slice(0, 8) : ""), { status: "running" });
      }else if(st === "task_progress"){
        const n = ev.usage && ev.usage.tool_uses;
        const label = "子代理" + (n != null ? ` · ${n} 次工具调用` : " · 进行中");
        if(ag && typeof ag.progress === "function") ag.progress(label);
        else sysMarker(label, { status: "running" });
      }else if(st === "task_notification"){
        const u = ev.usage || {};
        const dur = u.duration_ms != null ? (u.duration_ms / 1000).toFixed(1) + "s" : "";
        const label = (ev.status === "completed" ? "完成" : String(ev.status || "已结束"))
          + (dur ? ` · ${dur}` : "") + (u.tool_uses != null ? ` · ${u.tool_uses} 步` : "");
        if(ag && typeof ag.notify === "function") ag.notify(label, ev.status === "completed");
        else sysMarker("子代理" + label, { status: String(ev.status || "completed") });
      }else{
        sysMarker("系统 · " + (st || ev.status || "事件"), { status: ev.status || "" });
      }
      break;
    }
    case "result":
      // result 行是权威全文：以它覆盖正文（流式增量可能丢字符），并按 markdown 完整解析
      if(ev.text) h.setResult(ev.text);
      if(ev.latency_ms != null) ctx.setMs(ev.latency_ms);
      // 会话 id 写回任务（追问续接同一会话）；sessionId / session_id 双拼写兼容
      if(ev.sessionId || ev.session_id) ctx.onSession(ev.sessionId || ev.session_id);
      break;
    case "error":
      /* ⚠️ 失败那轮**也可能带着会话 id**（2026-09-23）：主进程把「引擎一个字都没说」的
         turn_end 改判成失败时（见 main.js 的空回答兜底），会话 id 是随 error 一起回来的 ——
         绑上它，下一轮追问仍接同一条会话（不然会退化成 `-c` 续接最近一次会话）。
         与 result / turn_end 两条一样，sessionId / session_id 双拼写兼容。 */
      if(ev.sessionId || ev.session_id) ctx.onSession(ev.sessionId || ev.session_id);
      h.endThinking(); finishTools(false);
      qaRunEnd(h);
      ctx.onError(ev);
      break;
    /* 过程提示（**不是**终态事件）：主进程用它说「这一轮正在自动换模型重试」这类事
       （2026-09-21 起对话侧的模型来自配置界面的降级链，前一个没额度就自动换下一个）。
       落成一行轻量注脚（h.note，与 system 事件同一处渲染），**不收尾**——
       重试那一轮仍带同一个 rid 继续推事件，画面照常往下走。 */
    case "notice":
      /* 过程提示：落成**标记行** + 可展开的原始载荷（2026-09-28）。
         「这一轮正在自动换模型重试」这类事原来只有一行文字，重试次数 / 原因 / 目标模型
         这些字段看不到 —— 而它们正是用户会追问的（「为什么答得慢了」）。 */
      if(typeof h.marker === "function")
        h.marker({ icon: QA_V_ICONS.clock, title: String(ev.text || ev.notice || "提示"),
                   detail: Object.assign({}, ev, { kind: "notice" }), status: ev.status || "" });
      else if(typeof h.note === "function") h.note(String(ev.text || ev.notice || ""));
      break;
    case "end":
      h.endThinking(); finishTools(true);
      qaRunEnd(h);
      /* 这一轮若**自动换过模型** → 此刻重取一次链（2026-09-21）：
         主进程在 close 前把跑通的那个升级成了链首，所以只有到这时「自动」档显示的
         名字才是新的。不重取的话，首页那枚下拉会一直写着那个已经跑不通的模型。
         ⚠️ 放在 onDone 之前：onDone 会落快照 / 改任务状态，重取链是纯读操作，先做完更稳。 */
      if(qaModelSwitched.has(ev.rid)){
        qaModelSwitched.delete(ev.rid);
        if(typeof qaModelChainLoad === "function") qaModelChainLoad(qaEngine, true);
      }
      if(ev.sessionId) ctx.onSession(ev.sessionId);   // result 行缺失时的兜底会话 id
      ctx.onDone(ev);
      break;
    case "turn_end":
      /* 一轮结束（magic-agent 2026-09-18 起；claude/codebuddy 的常驻会话每轮收尾发一次）。
         ⚠️ **这一轮到此就完成了**，别等 "end"：keep-alive 默认开之后，magic-agent 在最后一轮
         结束后还会留着会话等追加（默认空闲 5m 才收工），"end"（进程 close）要等到那时候才来。
         ⚠️ 顺序上 turn_end 在**最终 result 信封之前**（那个信封要等会话结束才输出），所以
         权威正文与会话 id 都得从这条事件上取，不能等 result：
           · ev.text        = 该轮权威正文（引擎 result 行的 result）→ setResult 覆盖增量
           · ev.session_id  = 该轮会话 id → 绑到任务上（下一轮追问续接同一会话）
         其余收尾动作与 "end" 完全一致（收思考区/工具卡/运行胶囊 + 快照 + 状态落库）。 */
      if(ev.text) h.setResult(ev.text);
      if(ev.session_id) ctx.onSession(ev.session_id);
      h.endThinking(); finishTools(true);
      qaRunEnd(h);
      ctx.onDone({ ok: true, code: 0, text: ev.text || "" });
      break;
    default:
      /* 形态不认识 ≠ 内容丢掉（2026-09-28，对齐 anywhere 的 UnknownTimelineItem）。
         原来是静默 break：引擎上了新事件类型，画面上「什么都没发生」，
         排障只能去翻日志 —— 而事件本身就在手里。
         现在落成一条可展开的标记行，原始载荷完整留在明细里。
         ⚠️ 心跳/保活这类**刻意的噪音**除外：每一跳都画一条会让画布全是垃圾。 */
      if(ev.type && !/^(ping|pong|heartbeat|keepalive|open|log)$/i.test(String(ev.type))
         && typeof h.marker === "function"){
        h.marker({ icon: QA_V_ICONS.clock, title: "未识别事件 · " + String(ev.type),
                   detail: ev, hint: "这次的事件类型本版渲染器还没有对应形态，原始载荷见明细" });
      }
      break;                                   // 心跳 / 日志等刻意噪音：忽略
  }
}

