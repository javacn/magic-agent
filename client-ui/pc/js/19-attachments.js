/* =========================================================
   对话框附件：粘贴 / 拖入的截图（2026-09-17）
   ---------------------------------------------------------
   链路一句话：paste 事件取 Blob → canvas 压缩 → **IPC 直写本机** → 缩略图 + 绝对路径。

   ① 为什么不叫「上传」：这是本地应用。图片本来就要落到本机产物库
      （~/.magic-test/pastes —— 用户粘贴/飞书图独立目录，与浏览器截图 images/ 分离、
      不进侧边栏产物库，2026-09-23），主进程直接 fs 写盘
      是零跳转、零端口依赖的最短路径，全程没有网络环节。HTTP 侧只在**预览态**
      （无 preload 桥）兜底 —— POST /api/media/upload 存在的唯一理由就是预览与 Playwright。
      读取侧仍走内置后端（<img src> 需要真实 URL），即产物库「写走 IPC、读走后端」的
      既有分工，与 server.js 顶部那段注释讲的是同一件事。
   ② 为什么仍然要压缩：本机不限流量，但图是要**喂给引擎看图**的 —— Retina 截屏动辄
      3024×1964 / 3~6MB，既拖慢视觉理解也拖慢每一轮对话。压到长边 ≤1920、≤1.5MB 是
      「界面文字还看得清」与「不拖后腿」的平衡点。且只有超限才重编码：小图原样直传，
      绝不无条件重编码 —— PNG 转 JPEG 会把界面文字压花。
   ③ 为什么状态要显式可见：落盘是异步的。用户在图还没落盘时按发送，引擎拿到的是
      不存在的路径，而报错会出现在下游、指向别处，最难排查 —— 所以在发送入口拦
      （见 qaSend 的附件闸门），失败项给可点的「重试」而不是静默丢弃。
   ④ 内存：缩略图先用 URL.createObjectURL 立刻出图，拿到真实 URL 后**立即 revoke**。
      理由见 MDN：只要有一个 objectURL 没释放，底层 Blob 就永远无法被 GC ——
      SPA 里反复贴图几分钟就能把渲染进程顶爆。只有确实没有真实 URL 可用时
      （file:// 直开且无后端）才继续持有 objectURL，并在移除 / 发送时统一释放。
   ========================================================= */

/** 单次最多挂几张 / 单张体积上限 / 压缩目标
 *  ⚠️ ATT_MAX_BYTES 与 desktop/media-store.cjs 的 MAX_UPLOAD_BYTES 是**同一个数**：
 *     前端先拦一道只为给即时反馈，落盘那道才是权威校验。改一处必须改另一处。 */
const ATT_MAX = 6;
const ATT_MAX_BYTES = 12 * 1024 * 1024;
const ATT_EDGE = 1920;                    // 压缩后最长边（px）
const ATT_TARGET = 1.5 * 1024 * 1024;     // 压缩目标（字节）
const ATT_RE = /^image\/(png|jpeg|jpg|gif|webp|bmp)$/;

const askAtts = {};                       // p → [att, ...]（按实例分开，与胶囊同款）
let attSeq = 0;

/** 体积文案（B → KB → MB） */
function attSize(n){
  n = Number(n) || 0;
  if(n < 1024) return n + " B";
  if(n < 1024 * 1024) return (n / 1024).toFixed(0) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}
/** 失败原因的中文化。
 *  fetch 的网络类失败在 Chrome 里就是一句英文 `Failed to fetch`（TypeError），
 *  直接抛给用户读不通；这里把「连不上 / 被中断」归成一句人话，其余保留原文
 *  （落盘端返回的已是中文，如「仅支持 PNG / JPEG…」「超过 12MB 上限」）。 */
function attErrText(err){
  const raw = String((err && err.message) || err || "").trim();
  if(!raw) return "写入本机失败";
  if(/failed to fetch|networkerror|load failed|network error/i.test(raw)) return "写入本机失败 · 点重试";
  return raw.slice(0, 60);
}
/** 缩略图地址：落盘后用真实 URL，落盘途中用本地 objectURL */
function attThumbUrl(a){ return a.remoteUrl || a.objectUrl || ""; }

/** 落盘返回的 url → 浏览器可直接用的绝对地址。
 *  壳内与预览态都拿得到 prdBackendBase（同一个内置后端）；
 *  file:// 直开且后端未就绪时返回空 —— 那种情况继续用 objectURL，
 *  不生成一个必然 404 的地址（否则缩略图会当场裂成破图标）。 */
function attAbsUrl(item){
  const u = (item && item.url) || "";
  if(!u) return "";
  if(prdBackendBase) return prdBackendBase + u;
  return location.protocol === "file:" ? "" : u;      // 预览态同源，相对路径即正确
}
/** 释放 objectURL。只在与真实 URL 并存、或被明确移除时释放 —— 释放早了缩略图会裂。 */
function attRelease(a, force){
  if(!a.objectUrl) return;
  if(!force && !a.remoteUrl) return;
  try{ URL.revokeObjectURL(a.objectUrl); }catch(e){ /* 已释放过：忽略 */ }
  a.objectUrl = "";
}

/** 从 DataTransfer 取图片文件（paste 与 drop 共用这一份）
 *  ⚠️ 必须先判 item.kind === "file"：非 file 项调 getAsFile() 返回 null；
 *     只认 image/* —— 否则普通文本粘贴会被误吃（Word 复制同时带文本与图片时尤其明显）。 */
function attFromDataTransfer(dt){
  const out = [];
  if(!dt) return out;
  const items = dt.items;
  if(items && items.length){
    for(const it of items){
      if(it.kind !== "file") continue;
      if(!ATT_RE.test(it.type || "")) continue;
      const f = it.getAsFile();
      if(f && f.size) out.push(f);
    }
  }
  if(!out.length && dt.files){          // 拖拽进来的文件在 files 里；paste 侧它是回退
    for(const f of dt.files){ if(f && ATT_RE.test(f.type || "") && f.size) out.push(f); }
  }
  return out;
}

/** 解码：优先 createImageBitmap，失败回落 <img>。
 *  返回 { src, w, h, close() }（close 负责释放位图 / objectURL，必须调用）。 */
async function attDecode(file){
  if(typeof createImageBitmap === "function"){
    try{
      const bm = await createImageBitmap(file, { imageOrientation: "from-image" });
      return { src: bm, w: bm.width, h: bm.height, close: ()=>{ try{ bm.close(); }catch(e){} } };
    }catch(e){ /* 回落 <img> */ }
  }
  const url = URL.createObjectURL(file);
  try{
    const img = await new Promise((res, rej)=>{
      const im = new Image();
      im.onload = ()=>res(im);
      im.onerror = ()=>rej(new Error("这张图读不出来"));
      im.src = url;
    });
    return { src: img, w: img.naturalWidth, h: img.naturalHeight, close: ()=>URL.revokeObjectURL(url) };
  }catch(e){ URL.revokeObjectURL(url); throw e; }
}

/** 编码一次（canvas → Blob）。toBlob 不支持该类型时会返回 null 或**另一种类型**，
 *  类型不符一律视为这条路走不通（否则会拿 PNG 冒充 WebP，体积目标永远达不到）。 */
function attEncode(src, w, h, type, quality){
  return new Promise((res)=>{
    let cv;
    try{ cv = document.createElement("canvas"); }catch(e){ return res(null); }
    cv.width = w; cv.height = h;
    const ctx = cv.getContext("2d");
    if(!ctx) return res(null);
    try{
      ctx.drawImage(src, 0, 0, w, h);
      cv.toBlob((b)=>res(b && (b.type || "") === type ? b : null), type, quality);
    }catch(e){ res(null); }
  });
}

/** 压缩：只在「尺寸超限」或「体积超目标」时重编码，否则原样返回**同一个文件对象**。
 *  输出优先级 PNG（源是 PNG 时无损、界面文字最清晰）→ WebP（省体积且保字）→ JPEG（兜底）；
 *  取第一个达标的，都不达标就取最小的那个。
 *  GIF 直接放过：重编码会把动图压成静帧（已知坑）。
 *  只返回字节、不返回文件名：文件名归落盘端命名（media-store.cjs 的 paste-时间戳），
 *  前端编一个名字出来只会在「还没落盘」时显示成假名。 */
async function attCompress(file){
  if(file.type === "image/gif") return file;
  if(file.size <= ATT_TARGET){
    // 体积本来就小：再做一次尺寸探针，只有尺寸超限才值得压（避免小图被无谓重编码）
    try{
      const d = await attDecode(file);
      const big = Math.max(d.w, d.h) > ATT_EDGE;
      d.close();
      if(!big) return file;
    }catch(e){ return file; }            // 解不开就原样交给落盘侧判（那边按魔数验）
  }
  const d = await attDecode(file);
  try{
    const scale = Math.min(1, ATT_EDGE / Math.max(d.w, d.h, 1));
    const w = Math.max(1, Math.round(d.w * scale));
    const h = Math.max(1, Math.round(d.h * scale));
    const cands = [];
    if((file.type || "") === "image/png") cands.push(["image/png", undefined]);
    cands.push(["image/webp", 0.92], ["image/jpeg", 0.9]);
    let best = null;
    for(const [type, q] of cands){
      const b = await attEncode(d.src, w, h, type, q);
      if(!b) continue;
      if(!best || b.size < best.size) best = b;
      if(b.size <= ATT_TARGET) return b;
    }
    return best || file;                 // 全编码失败：宁可落原图，也不把图丢掉
  } finally { d.close(); }
}

/** 落盘（本机写文件）：壳内走 IPC 直写，预览态回落到内置后端。
 *  两条路径返回同一种形状（都有 url 与 path），所以上层不必知道走的哪条。
 *  ⚠️ 必须校验返回形状（有 name 有 path 才算数）：桥/端点若返回一个残缺的 {ok:true}，
 *     就会造出一张「显示成功、实则没有路径」的图 —— 用户看得见缩略图、引擎却什么都读不到，
 *     而且全程没有任何报错。这是最会蒙混过关的一类坏状态，宁可在源头报错、留出「重试」。 */
async function attSave(a){
  let item = null;
  if(window.desk && typeof window.desk.pasteImage === "function"){
    const buf = await a.blob.arrayBuffer();
    const r = await window.desk.pasteImage({ buf, label: "paste" });
    if(!r || !r.ok) throw new Error((r && r.error) || "写入本机失败");
    item = r.item;
  }else{
    const resp = await fetch((prdBackendBase || "") + "/api/media/upload?name=paste", {
      method: "POST",
      headers: { "Content-Type": a.blob.type || "image/png" },
      body: a.blob,
    });
    const j = await resp.json().catch(()=>null);
    if(!resp.ok || !j || !j.ok) throw new Error((j && j.error) || ("HTTP " + resp.status));
    item = j.item;
  }
  if(!item || !item.name || !item.path) throw new Error("落盘返回不完整（没拿到图片路径）");
  return item;
}

/** 一个附件的完整生命周期：压缩 → 落盘 → 换真实 URL（拿到后立刻释放本地 objectURL） */
async function attProcess(p, a){
  try{
    const blob = await attCompress(a.blob);
    a.blob = blob; a.size = blob.size;
    askAttSync(p);                                   // 压完的体积先出来，不必等落盘
    if(blob.size > ATT_MAX_BYTES) throw new Error("超过 " + Math.round(ATT_MAX_BYTES / 1024 / 1024) + "MB 上限");
    const item = await attSave(a);
    a.remoteUrl = attAbsUrl(item);
    a.path = (item && item.path) || "";
    a.name = (item && item.name) || a.name;           // 真实文件名只在落盘成功后才有
    a.size = (item && item.bytes) || a.size;
    a.error = ""; a.status = "done";
    attRelease(a);                                   // 有真实 URL 了 → 立刻放掉本地 Blob 引用
  }catch(err){
    a.status = "error";
    a.error = attErrText(err);
  }
  askAttSync(p);
}

/** 当前引擎能不能**真的看到**图（按 `--engines` 的 attachments 能力判定）。
 *  'prompt' = 收附件只能退化成「提示词里的路径」——实测模型会直接回「我无法查看图片」，
 *  也就是说「图挂上了、也发出去了，但模型没看见」；其余取值
 *  （stdin:stream-json / flag:-i / flag:-a / part:file）都有原生图片通道。
 *  字段缺失（老版 CLI 不报该能力）= 不判定、不打扰用户。 */
function qaEngineSeesImages(){
  const row = qaEngines.find(x=>x.engine === qaEngine) || null;
  const cap = String((row && row.attachments) || "");
  if(!cap) return true;
  return cap !== "prompt";
}

/** 「哪些引擎真的能看到图」—— 降级提示里那条「换成 X 才能看图」的建议，**从清单算**。
 *  为什么不再写死「换成 claude / codebuddy」（2026-09-24 加 codebuddy-gateway 那轮改的）：
 *  写死的名单会随引擎增减漂移，而且它本来就漏了（codebuddy-ai / codex / llm / arkclaw 都有
 *  原生图片通道）—— 给用户的「换法」应当是他这台机器上**此刻真的可用**的那几个。
 *  判据与 qaEngineSeesImages 同源：attachments 能力非空且不是 'prompt'。
 *  只取前 4 个：一行提示塞不下更多，且这只是一句举例。 */
function qaImageEngines(){
  return (Array.isArray(qaEngines) ? qaEngines : [])
    .filter(x=>x && x.ok && String(x.attachments || "") && String(x.attachments) !== "prompt")
    .map(x=>String(x.engine))
    .slice(0, 4);
}

/** 挂上若干图片（粘贴 / 拖拽共用入口）。多张并发各推各的，谁先落盘谁先亮。 */
function askAttAdd(p, files){
  const list = askAtts[p] || (askAtts[p] = []);
  const room = ATT_MAX - list.length;
  if(room <= 0){ toast("一个对话框最多挂 " + ATT_MAX + " 张图"); return; }
  const take = files.slice(0, room);
  const dropped = files.length - take.length;
  take.forEach((f)=>{
    // name 先留空：落盘成功前不编名字，UI 会显示「截图」（假文件名如 paste.png 只会让人困惑）
    const a = { id: "att-" + (++attSeq), blob: f, name: "", size: f.size,
                objectUrl: URL.createObjectURL(f), remoteUrl: "", path: "",
                status: "saving", error: "" };
    list.push(a);
    attProcess(p, a);
  });
  askAttSync(p);
  /* 静默降级必须在这里点破：引擎收附件的方式若只是「把路径写进提示词」，
     模型根本看不到图 —— 用户会以为贴上了就等于看见了，然后在那边纳闷「它怎么答非所问」。 */
  if(!qaEngineSeesImages()){
    /* 「换成谁」从探测清单算（qaImageEngines）：本机此刻真能看图的引擎有哪些，
       就列哪些 —— 写死的名单会漂，而且列出一个本机不可用的引擎等于把人送进死胡同。
       一个都没有时（全是 prompt 通道 / 老版 CLI 不报该能力）只留前半句，不编建议。 */
    const can = qaImageEngines();
    toast("已挂上 " + take.length + " 张截图 · 但 <b>" + escHtml(qaEngine)
      + "</b> 看不到图（只能拿到路径）"
      + (can.length ? "<br>换成 " + can.map(escHtml).join(" / ") + " 才能让模型真的看图" : ""), true);
    return;
  }
  toast(dropped
    ? "一个对话框最多挂 " + ATT_MAX + " 张图，多余的 " + dropped + " 张已忽略"
    : "已挂上 " + take.length + " 张截图 · 发送后引擎可直接读取");
}

/** 附件条渲染：**增量**更新，不整体重绘。
 *  为什么：重绘会重建 <img>，而产物读取端是 Cache-Control: no-cache（server.js serveMedia），
 *  每次重建都会真的打回后端 —— 状态一变缩略图就闪一下。故只补新节点、只改文字、只摘删除项。 */
function askAttSync(p){
  const el = $("#" + p + "-atts"); if(!el) return;
  const list = askAtts[p] || [];
  el.hidden = !list.length;
  const alive = new Set();
  list.forEach((a, i)=>{
    alive.add(a.id);
    let node = el.querySelector('.ag-att[data-id="' + a.id + '"]');
    if(!node){
      node = document.createElement("span");
      node.className = "ag-att";
      node.dataset.id = a.id;
      node.innerHTML = '<img class="thumb" alt="" draggable="false">' +
        '<span class="meta"><b class="nm"></b><span class="sz"></span></span>' +
        '<button class="retry" data-act="retry" type="button">重试</button>' +
        '<button class="x" data-act="del" type="button" aria-label="移除这张图">×</button>';
    }
    if(el.children[i] !== node) el.insertBefore(node, el.children[i] || null);
    const u = attThumbUrl(a);
    const img = node.querySelector(".thumb");
    if(img.getAttribute("src") !== u) img.setAttribute("src", u);
    node.classList.toggle("is-saving", a.status === "saving");
    node.classList.toggle("is-error", a.status === "error");
    node.classList.toggle("is-done", a.status === "done");     // 三态对称：CSS 目前不依赖它，
                                                              // 但给测试与后续状态钩子留一个稳定锚点
    node.title = (a.name || "截图") + (a.error ? " · " + a.error : "");
    node.querySelector(".nm").textContent = a.name || "截图";
    node.querySelector(".sz").textContent = a.status === "saving" ? "保存中…"
      : a.status === "error" ? (a.error || "保存失败") : attSize(a.size);
  });
  [].slice.call(el.querySelectorAll(".ag-att")).forEach((n)=>{
    if(!alive.has(n.dataset.id)) n.remove();
  });
}

function askAttDel(p, id){
  const list = askAtts[p] || [];
  const i = list.findIndex((a)=>a.id === id);
  if(i < 0) return;
  attRelease(list[i], true);                 // 移除即释放（哪怕还没落盘）
  list.splice(i, 1);
  askAttSync(p);
}
function askAttRetry(p, id){
  const a = (askAtts[p] || []).find((x)=>x.id === id);
  if(!a || a.status !== "error") return;
  a.status = "saving"; a.error = "";
  askAttSync(p);
  attProcess(p, a);
}
/** 清空（发送后调用）：连带释放所有 objectURL */
function askAttClear(p){
  (askAtts[p] || []).forEach((a)=>attRelease(a, true));
  askAtts[p] = [];
  askAttSync(p);
}
const askAttReady  = (p)=> (askAtts[p] || []).filter((a)=>a.status === "done");
const askAttBusy   = (p)=> (askAtts[p] || []).some((a)=>a.status === "saving");
const askAttFailed = (p)=> (askAtts[p] || []).filter((a)=>a.status === "error");

/** 附件拼进**纯文本通道**（目前只有「挂需求胶囊 → 存入待处理」这一条）。
 *  ⚠️ 对话/派发**不要**用这个：那边的图走引擎原生附件通道（见 qaSend 的 attachments），
 *     把路径写进提示词实测模型看不到图（引擎回「我无法查看图片」）。
 *  需求条目只有一个正文字段（text-only），所以这里退化为「正文 + 绝对路径列表」，
 *  至少让后续派发时人或引擎能顺着路径找到图。 */
function askAttBody(p, text){
  const list = askAttReady(p);
  if(!list.length) return text;
  const lines = list.map((a)=>"- " + (a.path || a.name));
  return (text ? text + "\n\n" : "") + "[粘贴的截图]\n" + lines.join("\n");
}

/* ── 正文里的截图路径 → 气泡里的缩略图（2026-09-20）────────────────────────
   用户原话：「在对话中带截图或者附件的用户消息没有正确显示截图的缩略图」。
   图走不了附件通道的那两条路，只能把**绝对路径写进正文**（引擎靠路径读图）：
     · 对话框挂「需求」胶囊存入待处理（上面 askAttBody）：`[粘贴的截图]` + 若干 `- <路径>`
     · 飞书带图消息（desktop/feishu-bridge.cjs）：每行一条 `[图片] <路径>`
   路径给引擎读没问题，**给人看就是天书** —— 于是对话里那条用户消息只剩一串
   `/Users/…/paste-2026…png`，截图本身一眼都看不到。
   所以渲染用户气泡时把这些行**摘出来变成缩略图**（复用随消息发出的附件那套 `.qa-atts`
   形制与点开放大），正文里只留人话。
   ⚠️ 只在**渲染层**做：正文原文（库里的 body / 下发给引擎的 prompt）一个字都不动 ——
      摘掉的只是屏幕上那几行，别把 askAttBody / 飞书桥写进去的东西改了。
   ⚠️ 摘不干净宁可**不摘**：标记后面没有能识别的图片路径时（历史数据 / 人手写的），
      原样保留 —— 静默吃掉用户写的字比显示得难看严重得多。 */
const QA_SHOT_MARK = /^\[粘贴的截图\]\s*$/;        // 对话侧：标记独占一行
const QA_SHOT_ITEM = /^[-*]\s*(\S.*?)\s*$/;        // 紧随其后的 `- <路径>`
const QA_IMG_LINE  = /^\[图片\]\s*(\S.*?)\s*$/;     // 飞书侧：一行一条
const QA_IMG_EXT   = /\.(png|jpe?g|gif|webp|bmp)$/i;

/* ── 行内形态（2026-09-20 二次）─────────────────────────────────────────────
   用户原话：「用户的截图显示对了 bot回复中显示图片还不对」。
   上一条只治了**行式**（标记独占一行 + 紧随的 `- 路径`），而引擎回话是**一句话里**带着
   它看到的引用：
     我看到的引用是：[粘贴的截图] - /Users/…/paste-20260920-163930.png
   行式规则一条都匹配不到 → bot 气泡里就只剩一串路径，截图一眼看不到。
   所以再补一遍**行内**：
     ① 标记（`[粘贴的截图]` / `[图片]`）+ 紧随的路径 → **整段**（标记 + 分隔符 + 路径）摘掉，
        标记本身是机器话，摘掉才像人话；
     ② 光秃秃的图片路径（绝对路径 / `~/` / `file://`）单独出现也摘。
   ⚠️ 三处**不碰**：代码块（整段跳过，见 qaShotSplit）、行内代码 `…`（先占位、摘完还原）、
      markdown 图片/链接的地址（`![]()` / `[]()` —— 那条管线自己有渲染与尺寸口径）。
   ⚠️ 拼不出可显示地址的路径（预览态拿不到本机文件）**一个字都不动** —— 与 qaShotsMount
      末尾那条「摘不干净宁可不摘」同一口径。 */
const QA_SHOT_PATH_RE = /(?:file:\/\/|\/|~\/)[^\s"'`<>()\[\]，。；：、）】]+?\.(?:png|jpe?g|gif|webp|bmp)(?![A-Za-z0-9])/gi;

/** 一行里的「标记 + 路径」/「光秃秃的路径」→ 摘成缩略图。
 *  返回剩下的文字（shots 里收集被摘掉的路径，顺序与正文一致）。 */
function qaShotInline(line, shots){
  const raw = String(line || "");
  if(!raw || raw.indexOf("/") < 0) return raw;        // 一行里连 `/` 都没有 → 没什么可摘的
  const codes = [];                                   // 行内代码先占位（里面的路径不是截图）
  let s = raw.replace(/`[^`]*`/g, (m)=>{ codes.push(m); return "\u0000" + (codes.length - 1) + "\u0000"; });
  const hits = [];
  QA_SHOT_PATH_RE.lastIndex = 0;
  let m;
  while((m = QA_SHOT_PATH_RE.exec(s))){
    const p = m[0];
    if(/[(!]\s*$/.test(s.slice(0, m.index))) continue;  // markdown 图片/链接的地址不归这里管
    /* 路径**必须**是独立的一段：紧贴在字母/数字/`/`/`=`/`:` 后面的一律不算 ——
       `https://x.com/a.png`（远端图）与 `data:image/png;base64,iVBOR…/a.png`（base64 流）
       都靠这一条挡掉，否则会被拼成 `file://` 的裂图，还把原文吃了。 */
    if(/[A-Za-z0-9_+/=:.-]$/.test(s.slice(0, m.index))) continue;
    if(!qaShotSrc(p)) continue;                        // 拼不出可显示地址 → 不动它（见上）
    let from = m.index;
    const mk = /\[(?:粘贴的截图|图片)\]\s*(?:[-–—:：]\s*)?$/.exec(s.slice(0, m.index));
    if(mk) from = mk.index;                            // 标记 + 分隔符紧贴在前面 → 一起摘
    hits.push({ from, to: m.index + p.length, p });
  }
  if(!hits.length) return raw;                         // 一个字都没摘 → 原样返回
  hits.forEach((h)=> shots.push(h.p));
  for(let i = hits.length - 1; i >= 0; i--){
    const h = hits[i];
    let to = h.to;
    if(s[h.from - 1] === " " && s[to] === " ") to++;    // 摘点两侧都有空格 → 连一个空格收掉，不留双空格
    s = s.slice(0, h.from) + s.slice(to);
  }
  s = s.replace(/[ \t]+$/, "");                        // 摘完留下的行尾空格 / 悬空分隔符（「…引用是：-」）
  s = s.replace(/[ \t]*[-–—:：]+[ \t]*$/, "");
  return s.replace(/\u0000(\d+)\u0000/g, (_m, i)=> codes[+i]);
}

/** 把正文拆成「给人看的文字」+「要画成缩略图的路径」。
 *  ⚠️ 代码块（``` / ~~~）里的一切原样保留：那是引擎/用户贴的代码与清单，
 *     里面的 `.png` 路径是给人看的字，不是要画出来的图。 */
function qaShotSplit(text){
  const lines = String(text || "").split("\n");
  const keep = [], shots = [];
  let fence = false;
  for(let i = 0; i < lines.length; i++){
    const ln = lines[i].trim();
    if(/^(?:```|~~~)/.test(ln)){ fence = !fence; keep.push(lines[i]); continue; }
    if(fence){ keep.push(lines[i]); continue; }
    const one = QA_IMG_LINE.exec(ln);
    if(one && QA_IMG_EXT.test(one[1])){ shots.push(one[1]); continue; }
    if(QA_SHOT_MARK.test(ln)){
      const take = [];
      let j = i + 1;
      for(; j < lines.length; j++){
        const m = QA_SHOT_ITEM.exec(lines[j].trim());
        if(!m || !QA_IMG_EXT.test(m[1])) break;
        take.push(m[1]);
      }
      if(take.length){ shots.push(...take); i = j - 1; continue; }   // 标记行与那些条目一起摘掉
    }
    keep.push(qaShotInline(lines[i], shots));
  }
  return { text: keep.join("\n").replace(/\n{3,}/g, "\n\n").trim(), shots };
}

/** 本地绝对路径 → 能塞进 `<img src>` 的地址（口径与 attAbsUrl 一致，别再各写一份）：
 *  ① 已经是 URL 的原样用；
 *  ② 产物库里的图（`~/.magic-test/images/…` 浏览器截图、`~/.magic-test/pastes/…`
 *     用户粘贴/飞书图）走内置后端 `/api/media/file/<kind>/<名字>` —— 壳内与预览态都通
 *     （后端按 KIND_DIR 放行三类目录下的平铺文件，所以只对产物库的路径这么拼）；
 *  ③ 其余本地文件：壳里页面是 `file://` → 拼 `file://`（与审查截图墙 reqShotUrl 同一招）；
 *     预览态（http）拿不到这种文件 → 返回空，上层退回文字，**不画一个必然裂掉的框**。 */
function qaShotSrc(p){
  const s = String(p || "").trim();
  if(!s) return "";
  if(/^(https?|data|blob|file):/i.test(s)) return s;
  const fileUrl = "file://" + encodeURI(s).replace(/#/g, "%23");
  const m = s.match(/\/\.magic-test\/(images|pastes)\//);
  if(m){
    let kind = m[1] === "pastes" ? "paste" : "image";        // 目录名 → kind（对应 KIND_DIR）
    const base = s.split("/").pop();
    // 历史粘贴图纠偏：2026-09-23 之前的粘贴/飞书图落 images/（paste-*、feishu- 前缀 +
    // 时间戳形状），迁移已把它们搬进 pastes/，但历史消息正文里存的还是 …/images/paste-*.png
    // 旧路径 —— 按文件名特征直接走 paste kind，不依赖后端 serveMedia 的旧地址兜底
    //（双保险：即使前端没走到这段，旧 URL 到后端也能被 resolveMediaPath 接住）。
    // ⚠️ 正则与 media-store.cjs 的 PASTE_LEGACY_NAME_RE 是同一份判定，两处改要同步。
    if(kind === "image" && base && /^(?:paste|feishu)-\d{8}-\d{6}(?:-\d+)?\.[a-z0-9]+$/i.test(base)) kind = "paste";
    if(base){
      const rel = "/api/media/file/" + kind + "/" + encodeURIComponent(base);
      if(prdBackendBase) return prdBackendBase + rel;
      return location.protocol === "file:" ? fileUrl : rel;      // 预览态同源
    }
  }
  return location.protocol === "file:" ? fileUrl : "";
}

/** 一枚缩略图节点（随消息发出的附件 / 正文里解析出来的截图，共用同一套形制） */
function qaThumbNode(src, title, alt){
  const im = document.createElement("img");
  im.src = src;
  im.alt = alt || "截图";
  if(title) im.title = title;
  im.setAttribute("draggable", "false");
  return im;
}

/** 气泡正文的统一落笔处：markdown（摘掉截图路径那几行/那几段）+ 下面一排截图缩略图。
 *  三条渲染器都走它 —— 实时那条用户气泡（qaRenderMine）、实时那条 bot 答复
 *  （qaRenderAnswer 的 flushClosed / finish / setResult）与派发快照那条（qaReqConvMsg）：
 *  少一处就会出现「刚发出去看得见、点开需求对话又变回路径」这种半修状态。
 *  ⚠️ 2026-09-20 二次：bot / 审核引擎的答复也走这里了（用户：「用户的截图显示对了
 *     bot回复中显示图片还不对」）—— 引擎回话里带的是**它看到的引用**，同一串路径
 *     在用户气泡里是缩略图、在 bot 气泡里是文字，读起来就像「bot 没看见图」。
 *     摘的仍然只是**屏幕上那几个字符**：库里的正文 / 下发给引擎的 prompt 一个字不动。 */
function qaShotsMount(host, text){
  if(!host) return;
  const sp = qaShotSplit(text);
  const srcs = [], lost = [];
  sp.shots.forEach((p)=>{ const u = qaShotSrc(p); if(u) srcs.push({ u, p }); else lost.push(p); });
  /* 地址拼不出来的（预览态拿不到本机文件）→ 把路径还原成文字，别静默吞掉 */
  const rest = lost.length
    ? (sp.text ? sp.text + "\n\n" : "") + lost.map((p)=>"[图片] " + p).join("\n")
    : sp.text;
  try{ host.innerHTML = mdToHtmlOnly(rest); highlightIn(host); }
  catch(e){ host.textContent = rest; }
  if(!srcs.length) return;
  const row = document.createElement("div");
  row.className = "qa-atts";
  srcs.forEach((x)=> row.appendChild(qaThumbNode(x.u, x.p.split("/").pop() + " · 点开看原图")));
  host.appendChild(row);
}

/** 附件条 + 粘贴 / 拖拽接线（逐实例各绑一次，与 ＋ 菜单同款） */
function askAttBind(p){
  const box = $("#" + p + "-box"); if(!box) return;
  const row = $("#" + p + "-atts");
  if(row){
    row.addEventListener("click", (e)=>{
      const btn = e.target.closest("[data-act]"); if(!btn) return;
      const host = btn.closest(".ag-att"); if(!host) return;
      if(btn.dataset.act === "retry") askAttRetry(p, host.dataset.id);
      else askAttDel(p, host.dataset.id);
    });
  }
  /* 生成中（.is-busy）整块撰写区锁死（用户 2026-09-18）：图也不许再往里加 ——
     加了也只有等这一轮结束才发得出去，中间那段时间缩略图挂在锁死的框上只会让人困惑。
     一律拦在图片分支里，纯文本粘贴的默认行为照旧不碰。 */
  const busyGate = (e)=>{
    if(!box.classList.contains("is-busy")) return false;
    e.preventDefault(); e.stopPropagation();
    toast("正在生成中 · 等这一轮结束再加截图");
    return true;
  };
  /* 粘贴：只认「确实带图片文件」的剪贴板。没有图片时一行不拦 ——
     纯文本 / 富文本粘贴继续走浏览器默认行为（无差别 preventDefault 会吃掉文字粘贴，
     这是这类实现最常见的一处自伤）。 */
  box.addEventListener("paste", (e)=>{
    const files = attFromDataTransfer(e.clipboardData);
    if(!files.length) return;
    if(busyGate(e)) return;
    e.preventDefault();
    askAttAdd(p, files);
  });
  /* 拖拽：与粘贴共用同一条管线（DataTransfer 是同一个抽象）。
     ⚠️ dragenter / dragover 必须 preventDefault：否则浏览器默认行为是**直接打开这个文件**，
        整个工作台会被替换成一张图片 —— 比粘贴更容易踩、也更难排查的一类故障。 */
  ["dragenter", "dragover"].forEach((t)=>box.addEventListener(t, (e)=>{
    if(!attFromDataTransfer(e.dataTransfer).length) return;
    e.preventDefault(); e.stopPropagation();
    if(box.classList.contains("is-busy")) return;      // 生成中不给落点反馈
    box.classList.add("is-drop");
  }));
  box.addEventListener("dragleave", (e)=>{
    if(!box.contains(e.relatedTarget)) box.classList.remove("is-drop");
  });
  box.addEventListener("drop", (e)=>{
    const files = attFromDataTransfer(e.dataTransfer);
    const anyFile = !!(e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length);
    box.classList.remove("is-drop");
    if(!files.length){
      if(anyFile){ e.preventDefault(); e.stopPropagation(); toast("只支持图片（PNG / JPEG / WebP / GIF / BMP）"); }
      return;
    }
    if(busyGate(e)) return;
    e.preventDefault(); e.stopPropagation();
    askAttAdd(p, files);
  });
}

function askAddClose(p){
  const menu = $("#" + p + "-add-menu"), plus = $("#" + p + "-plus");
  if(menu){
    if(typeof window.__qaMenuClose === "function") window.__qaMenuClose(menu);
    else menu.hidden = true;
  }
  if(plus){ plus.classList.remove("open"); plus.setAttribute("aria-expanded","false"); }
}

/** ＋ 菜单内容：**6 个选项**（需求 / 模拟测试 / 录屏 / 截屏 / 产品文档 / 测试用例），已选的打勾。
 *  用户 2026-09-17 定稿：不是列站内对象清单 —— 选中的类型以胶囊形式挂进对话框，
 *  由胶囊决定发送语义（挂「需求」= 发送即新建需求；挂「模拟测试」= 发送即开测；
 *  挂「录屏」= 发送即录屏；挂「截屏」= 发送即截屏；其余 = 普通对话）。
 *  ⚠️ 2026-09-21 新增「模拟测试」（用户：「只是在对话框中+号弹出的菜单中新增一种模式 模拟测试」）：
 *     它与「需求」并排放在最前两位 —— 这两个都是**改发送语义的模式**，
 *     而「产品文档 / 测试用例」只是普通对话里挂个标记。
 *  ⚠️ 2026-09-21 再加「录屏」（用户：「需求是工作台对话框中+号弹出的模型里新增录屏模式
 *     选择后调用这个cli去录屏」）与「截屏」（用户：「在对话框用截屏模式测试一下」）：
 *     四枚改发送语义的模式排在前四位，录屏与截屏**挨着**（同一族：都驱动内嵌浏览器），
 *     与「产品文档 / 测试用例」之间用这个顺序区分开。
 *  ⚠️ 2026-09-23 加「短视频」（用户：「在工作台对话中增加新模式 短视频 通过集成调用
 *     magic-shorts cli的能力完成短视频制作」）：五枚改发送语义的模式排在前五位；
 *     短视频排在截屏之后 —— 它与「录屏 / 截屏」不是一族（不碰浏览器：引擎只产任务单，
 *     壳逐阶段执行 magic-shorts CLI，见 28-shorts.js），位置只反映「同为重模式」的排序。 */
function askAddMenuBuild(p){
  const menu = $("#" + p + "-add-menu"); if(!menu) return;
  const list = askChips[p] || [];
  const opt = (kind)=>{
    const on = list.includes(kind);
    return `<button class="ag-model-opt${on ? " on" : ""}" data-add="${kind}" type="button" role="option"` +
      ` aria-selected="${on}" title="${escHtml(ASK_CHIP_LABEL[kind] || kind)}">` +
      `<span class="eng-opt"><span class="st"></span><span class="nm">${escHtml(ASK_CHIP_LABEL[kind] || kind)}</span>` +
      `<span class="dm">${escHtml(ASK_CHIP_SIDE[kind] || "")}</span></span></button>`;
  };
  const engHint = `<span class="req-eng-hint" id="${p}-eng-hint" data-testid="${p}-eng-hint"></span>`;
  menu.innerHTML = opt("req") + opt("sim") + opt("rec") + opt("shot") + opt("shorts") + opt("doc") + opt("case") +
    `<span class="ag-menu-note req-eng-note">${engHint}</span>`;
  qaEngHintSync();                       // 底部那行「派发引擎 / 工作目录」说明
}
/** 选中 ＋ 菜单里的一项 = 挂/摘对应胶囊 */
function askAddPick(p, opt){
  if(!opt) return;
  const kind = opt.dataset.add || "";
  askAddClose(p);
  askChipToggle(p, kind);
}
/** 「发送」时挂了需求胶囊 → 不发对话，正文直接存进「待处理」。
 *  需求管理页已无新建入口，这里是**新建需求的唯一路径**。 */
function askReqSave(p){
  const input = askInputOf(p);
  const text = String((input && input.value) || "").trim();
  const atts = askAttReady(p);
  // 与 qaSend 同一道附件闸门（理由同）：需求正文里带的是截图**绝对路径**，
  // 没落盘完就存 → 存进一条谁都读不到的路径。
  if(askAttBusy(p)){ toast("截图还在保存，稍等一下再存"); return; }
  if(askAttFailed(p).length){ toast("有截图没保存成功，先重试或移除"); return; }
  if(!qaWorkspace){ toast("先在左下角选一个工作目录"); return; }
  if(!text && !atts.length){ toast("先写点需求内容，再发送"); return; }
  if(!reqHas("boardAdd")){ toast("新建需求需要桌面壳（预览态不支持）"); return; }
  // 正文带上截图路径（需求条目是纯文本字段，走不了附件通道，见 askAttBody）
  const body = askAttBody(p, text);
  /* 模型**按实例算**（2026-09-21，与 qaSend 同一份算法）：工作台首页 = 那枚选择器选的
     （没选 = 配置链链首）；任务对话 = 该任务自己的模型。派发侧再按需求自己的模型 + 配置链兜底。 */
  const mdl = (typeof qaModelFor === "function") ? qaModelFor(p, qaActiveId) : qaModel;
  window.desk.boardAdd(qaWorkspace, { body, engine: qaEngine, model: mdl, source: "board", skipMemory: true })
    .then(r=>{
      if(!r || !r.ok){ toast("新建失败 · " + ((r && r.error) || "未知错误")); return; }
      if(input) input.value = "";
      askChipsClear(p);                  // 需求胶囊用掉即摘，别的胶囊一并清掉
      askAttClear(p);                    // 截图已写进正文 → 附件条功成身退
      toast("已存入待处理 · " + r.item.title);
      try{ reqBoardFetch(); }catch(e){ /* 看板未就绪 */ }
    })
    .catch(()=>toast("新建失败 · 通道不可用"));
}

/** 实例统一挂载 —— 必须早于 bind()（那边按 id 取输入框/发送钮/菜单来绑事件）。
 *  宿主 `<div class="home-entry" id="ask-<p>">`：.home-entry 是菜单的定位包含块 + z-index 兜底。 */
function askBoxMountAll(){
  ASK_INSTANCES.forEach((p)=>{
    const host = $("#ask-" + p);
    if(!host){ console.log("[askBox] 挂载点缺失：#ask-" + p); return; }
    host.innerHTML = askBoxMarkup(ASK_CFG[p]);
    askAddMenuBuild(p);                  // 先填一次，保证 DOM 里始终有菜单（含引擎提示行）
    askChipsSync(p);
  });
}

const USER_FALLBACK = { name: "超哥" };

function renderUser(){
  const el = $("#nav-user"); if(!el) return;
  const set = (u)=>{
    const cur = (u && u.ok !== false && u.name) ? u : USER_FALLBACK;
    const nm = $("#nu-name");
    if(nm) nm.textContent = cur.name || USER_FALLBACK.name;
  };
  set(USER_FALLBACK);
  if(window.desk && typeof window.desk.userInfo === "function"){
    window.desk.userInfo().then(u=>{ if(u && u.ok) set(u); }).catch(()=>{ /* 保持兜底 */ });
  }
}

/* ===== （已移除）名言跑马灯 =====
   原「三层结构 · 竖切轮换 · 长句横滚」的名言跑马灯已按用户 2026-09-16 要求
   整体替换为左下角的 workspace 切换器（#nu-ws，见 bindHomeEntry 内的 qaWsBind）。
   相应地 QUOTES / QUOTE_OF_DAY / startQuoteMarquee 一并删除，避免留死代码。 */

