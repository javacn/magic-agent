/* ---------- 流式事件 → 画面（**唯一一份映射**）----------
   2026-09-17 用户定稿：「派发任务也要走流式，就走对话框提交 1 个方式」「要在任务中看到流式效果」。
   所以对话框提交（bindHomeAsk）与需求派发（qaReqStreamApply）共用这一个开关：
   thinking → 思考区、text → 逐字正文、tool_use/tool_result → 工具卡、result → 权威全文、
   error/end → 收尾行。调用方只管提供 ctx（手柄 + 工具登记表 + 收尾动作）。

   2026-09-17 追加（委派/子代理显示，对齐 Claude Code 原生 stream 形态）：
   · 归一化（qaEventNormalize）：assistant/user 的 message.content 块、system 生命周期
     事件、result 别名（result/duration_ms）统一翻译成扁平事件词表；parent_tool_use_id
     随事件带成 `parent` —— 引擎已扁平的事件原样通过，渲染层不写第二份判断；
   · ① tool_use.name 为 Agent/Task（主会话）→ 委派卡（.qa-agent），登记进 ctx.agents；
   · ③ 带 parent 的事件渲染进父委派卡的嵌套区（子工具卡/子正文/子注脚）；
   · ② system 生命周期（task_started/progress/notification）驱动父卡状态胶囊；
     父卡缺席 → 降级为 .qa-note 轻量行，不再静默丢弃；
   · ④ tool_result 配不到卡片 → 「未配对结果」单行卡兜底，不再静默丢弃。 */
/*   2026-09-18 追加（⑤ 提问卡）：tool_use.name = AskUserQuestion → 落成**可点选项**的提问卡
   （.qa-ask），不再把 questions/options 的 JSON 摊在普通工具卡的「输入」里；
   用户点选项 = 把答案作为追问发回这一问（见 qaAskPick / qaAskDeliver）。 */

/** content 块（string | [{type:'text',text},…] | {text}）→ 纯文本 */
function qaBlockText(c){
  if(c == null) return "";
  if(typeof c === "string") return c;
  if(Array.isArray(c)) return c.map(x=>(x && typeof x === "object") ? (x.text || "") : String(x)).join("");
  if(typeof c === "object") return c.text != null ? String(c.text) : JSON.stringify(c);
  return String(c);
}

/* ---------- tool_use 载荷 → 工具调用单元（按 description 分割统计）----------
   2026-09-18 用户：「当工具调用返回的信息中有 description 需要以文本显示出来。调用了多少个
   工具按照出现了 description 进行分割统计」+「分割成显示多次工具调用 中间插入一些 description」。
   一条 tool_use 事件里可能装着不止一次调用 —— 参数是数组、是几份 JSON 拼在一起、或者干脆不是
   JSON（命令行文本里逐条带 description）。原来整份载荷摊在参数摘要里 → 看着是**一次**调用、
   行里写着一大坨 JSON，统计行也只加 1。
   这里把它切成若干「调用单元」：一个单元画一张卡、卡上摘要显示自己的 description 文本，
   统计行按单元数计数 —— 于是「调用了多少个工具」= 出现了几处 description。
   单元形状 { name, desc, args }：desc 可能为空（没有 description 的载荷），此时回落原行为。 */
function qaJsonSeq(text){
  // 扫出字符串里并列的多份顶层 JSON 值（对象 / 数组）→ [{ obj, text }]；扫不动就回落
  const out = [];
  let i = 0;
  while(i < text.length){
    while(i < text.length && /[\s,]/.test(text[i])) i++;
    if(text[i] !== "{" && text[i] !== "[") break;
    let depth = 0, j = i, inStr = false, esc = false;
    for(; j < text.length; j++){
      const c = text[j];
      if(inStr){
        if(esc) esc = false;
        else if(c === "\\") esc = true;
        else if(c === '"') inStr = false;
        continue;
      }
      if(c === '"'){ inStr = true; continue; }
      if(c === "{" || c === "[") depth++;
      else if((c === "}" || c === "]") && --depth === 0){ j++; break; }
    }
    const slice = text.slice(i, j);
    try{ out.push({ obj: JSON.parse(slice), text: slice }); }catch(e){ break; }
    i = j;
  }
  return out;
}
function qaDescTexts(text){
  // 兜底：载荷不是 JSON 时，按 `"description": "…"` 出现处取描述文本（出现几次 = 几次调用）
  const out = [];
  const re = /"description"\s*:\s*"((?:[^"\\]|\\.)*)"/g;
  let m;
  while((m = re.exec(String(text || "")))) out.push(m[1].replace(/\\(["\\/nrt])/g, "$1"));
  return out;
}
function qaToolUnits(name, argsText){
  const nm = String(name || "tool");
  const raw = String(argsText == null ? "" : argsText);
  const mk = (desc, args, obj)=> ({ name: nm, args: String(args == null ? "" : args),
    desc: String(desc || (obj && (obj.description || obj.subagent_type || obj.command)) || "") });
  const trim = raw.trim();
  if(!trim) return [mk("", raw)];
  let root;
  try{ root = JSON.parse(trim); }catch(e){ root = undefined; }
  if(Array.isArray(root)) return root.map(o=>mk("", JSON.stringify(o, null, 2), o));  // 参数是数组 → 逐个
  if(root && typeof root === "object") return [mk("", trim, root)];                   // 单份对象 → 一次调用
  const seq = qaJsonSeq(trim);                                                       // JSON 拼接 → 逐个
  if(seq.length > 1) return seq.map(s=>mk("", s.text, s.obj));
  const ds = qaDescTexts(raw);                                                       // 非 JSON → 按 description 切
  if(ds.length > 1) return ds.map(d=>mk(d, ""));
  return [mk("", raw)];
}

/** 原生 stream 形态 → 渲染层扁平事件（返回数组；扁平事件原样单元素通过）。
 *  claude 原生：assistant/user 的 content 在 message.content[] 里（tool_use 的参数在
 *  input 对象、tool_result 的正文在 content 块里）—— 翻译成 {id,name,text} / {id,text,ok}。 */
function qaEventNormalize(ev){
  if(!ev || typeof ev !== "object") return [];
  const t = ev.type;
  const parent = ev.parent_tool_use_id || null;
  if(t === "assistant" || t === "user"){
    const blocks = ev.message && Array.isArray(ev.message.content) ? ev.message.content : null;
    if(!blocks) return [];
    const out = [];
    for(const b of blocks){
      if(!b || typeof b !== "object") continue;
      if(b.type === "thinking"){
        const txt = b.thinking || b.text || "";
        if(txt) out.push({ type:"thinking", text:txt, parent });
      }else if(b.type === "text"){
        // user 消息里的 text 块（任务 prompt 等）不上屏：委派卡「任务词」里已有
        if(t === "assistant" && b.text) out.push({ type:"text", text:b.text, parent });
      }else if(b.type === "tool_use"){
        out.push({ type:"tool_use", id:b.id, name:b.name,
          text: b.input != null ? JSON.stringify(b.input, null, 2) : (b.text || ""),
          inputObj: b.input || null, parent });
      }else if(b.type === "tool_result"){
        out.push({ type:"tool_result", id:b.tool_use_id, text:qaBlockText(b.content),
          ok:!b.is_error, parent });
      }
      // 其余块（image / document 等）不进对话流
    }
    return out;
  }
  if(t === "system"){
    // ② 生命周期事件原样放行（字段保留），渲染层按 subtype 分发
    return [{ type:"system", subtype:ev.subtype || "", task_id:ev.task_id,
      tool_use_id:ev.tool_use_id, status:ev.status, usage:ev.usage, parent }];
  }
  if(t === "result"){
    // claude 原生 result：正文叫 result、耗时叫 duration_ms —— 补别名对齐现有词表
    const e2 = Object.assign({}, ev);
    if(e2.text == null && e2.result != null) e2.text = String(e2.result);
    if(e2.latency_ms == null && e2.duration_ms != null) e2.latency_ms = e2.duration_ms;
    return [e2];
  }
  return [ev];   // thinking/text/tool_use/tool_result/error/end/start 等扁平事件
}

/* ---------- 提问（AskUserQuestion）解析 ----------
   2026-09-18 用户：「返回了 askuserquestion，需要 ui 在对话框中增加选择显示 而不是显示这个 json」。
   引擎把这一问塞在 tool_use 的参数里，形状**不稳定**（实测三种都出现过）：
     ① {questions:"[{…}]"}     —— questions 是**再编码一次的 JSON 字符串**（真机截图就是这个形状）
     ② {questions:[{…}]}       —— 正常数组
     ③ {question:"…",options:[…]} —— 没有 questions 包裹的单问
   解析失败一律返回 null → 调用方回落普通工具卡（信息不丢，只是不好看）。 */
function qaIsAskTool(name){
  return /ask[_\s-]?user[_\s-]?question/i.test(String(name || ""));
}
function qaAskParse(argsText){
  let o = argsText;
  if(typeof o === "string"){
    const s = o.trim(); if(!s) return null;
    try{ o = JSON.parse(s); }catch(e){ return null; }
  }
  if(!o || typeof o !== "object") return null;
  let qs = o.questions;
  if(typeof qs === "string"){ try{ qs = JSON.parse(qs); }catch(e){ return null; } }
  if(qs && !Array.isArray(qs) && typeof qs === "object") qs = [qs];
  if(!Array.isArray(qs)) qs = (o.question || o.options) ? [o] : [];   // ③ 无包裹的单问
  const out = qs.map(q=>{
    if(!q || typeof q !== "object") return null;
    const opts = Array.isArray(q.options) ? q.options : [];
    return {
      header: String(q.header || ""),
      question: String(q.question || ""),
      multiSelect: !!q.multiSelect,
      options: opts.filter(x=>x && typeof x === "object")
        .map(x=>({ label: String(x.label || ""), description: String(x.description || "") }))
        .filter(x=>x.label),
    };
  }).filter(q=>q && (q.question || q.options.length));
  return out.length ? out : null;
}

