/* ===== 基础组件：融合输入框（askBox）=====
   用户 2026-09-17 定稿：「令对话框变成 1 个基础组件，与工作台首页的要一样共用」+
   「是要做成前端组件 改了大家都跟着变」。全应用**只此一份**标记与行为，
   两处实例共用（此前是手抄三份，改一处漏两处）：
     home2 → 工作台首页入口
     home  → 任务对话视图
   ＋ 菜单 = 5 个类型选项（需求 / 模拟测试 / 录屏 / 产品文档 / 测试用例），选中后以**胶囊**挂进对话框：
   挂「需求」胶囊 → 发送 = 新建需求（正文存入待处理，不发起对话）；
   挂「模拟测试」胶囊 → 发送 = 开一轮模拟测试（见 24-agent-test.js）；
   挂「录屏」胶囊 → 发送 = 录屏模式（见 25-record.js）；
   其余 / 没挂 = 普通对话。
   需求管理页的那个输入框已下线 —— 新建需求的唯一入口在对话框的 ＋ 菜单里。
   实例差异只走配置：id 前缀 p、占位文案。
   挂载：askBoxMountAll() —— 按 ASK_INSTANCES 逐个替换宿主 innerHTML（box + 三只菜单）。
   ⚠️ 改这里 = 改所有实例，不必再去各页面抄一遍；新增输入框请复用本组件而非复制标记。

   ⚠️ 2026-09-21 第一版（用户：「对话窗口不选择模型了 直接使用配置界面的模型列表自动处理」）：
   右下角只留一枚「引擎」选择器，模型按钮与它的菜单整体下线 —— 用哪个模型改由**配置界面**
   （设置窗口「引擎配置」）里该引擎的**有序降级链**决定（链首 = 主选，其后是兜底候选，
   执行侧按顺序自动试，见 src/js/04-task-list.js + desktop/chat-model.cjs）。
   → 第二版（「不能选择 但是要显示用的是哪个」）：补一枚**只读模型标签**如实显示。
   → 第三版（「工作台的对话框中可以选模型 在任务ui中只是显示」）：首页可选、任务对话只读。
   → **第四版（2026-09-21 定稿，用户：「还是都可以选择模型 但是自动切换模型的时候 对话框下拉的
     选中模型显示也要变」）：两个实例都恢复成可选的模型下拉**（按钮 + 菜单），
     并且「自动切换模型」时下拉的选中项/文案**必须跟着变成实际在跑的那个**。
   ⚠️ 别再按「只读标签」那两版改回去（只读标签 `-model-ro` / `.ag-mdl-ro` 已删干净）：
   现在两处控件**同源同形态**，实例差异回到只有占位文案一条。
   ⚠️ 与「两处真相」的关系：模型选择**是**允许的（用户要），但「这次到底下发哪个」仍只有一份
   算法（`qaModelFor()`，见 04-task-list.js）—— 界面只负责把选择写回去、把实际值画出来。 */
const ASK_ICON = {
  plus: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>',
  access: '<svg class="i" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"><circle cx="12" cy="12" r="9"/><path d="M12 8h.01M11 11.5h1v5h1" stroke-linecap="round"/></svg>',
  engine: '<svg class="logo" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"><rect x="7" y="7" width="10" height="10" rx="2"/><path d="M12 2.5v3M12 18.5v3M2.5 12h3M18.5 12h3M5.3 5.3l2.1 2.1M16.6 16.6l2.1 2.1M18.7 5.3l-2.1 2.1M7.4 16.6l-2.1 2.1"/></svg>',
  /* 模型选择器上那枚「地球」小标（用图标而不是文字前缀，免得按钮变成「模型：xxx」那种表单感）。 */
  model: '<svg class="logo" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" aria-hidden="true"><circle cx="12" cy="12" r="9.2"/><path d="M12 2.8c3.2 4.4 3.2 14 0 18.4M12 2.8c-3.2 4.4-3.2 14 0 18.4M3.4 9h17.2M3.4 15h17.2"/></svg>',
  chev: '<svg class="chev" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>',
  arrow: '<svg class="ico-arrow" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 19V5M5 12l7-7 7 7"/></svg>',
  stop: '<svg class="ico-stop" viewBox="0 0 24 24" fill="currentColor"><rect x="6.5" y="6.5" width="11" height="11" rx="2.4"/></svg>',
};

/** ＋ 菜单的**七种**「胶囊」类型（用户 2026-09-17 定稿 3 种；2026-09-21 依次加「模拟测试」「录屏」「截屏」；
 *  2026-09-23 加「短视频」：在工作台对话中增加新模式，通过集成调用 magic-shorts CLI 完成短视频制作）——
 *  ＋ 点开 = 7 个选项，选中哪类，哪类就以**胶囊**挂进对话框；
 *  发送语义由胶囊决定：**挂了「需求」胶囊 → 发送 = 新建需求**（正文存入待处理，不发起对话），
 *  挂「模拟测试」= 开测、挂「录屏」= 录屏模式、挂「截屏」= 截屏模式、挂「短视频」= 短视频流水线，
 *  否则 = 普通对话（胶囊随发送清掉）。
 *  ⚠️ 前五种（req / sim / rec / shot / shorts）是**改发送语义的模式**，后两种（doc / case）只是普通对话里挂个标记 ——
 *     排序不是随便排的，改发送语义的那几种必须靠前（见 19-attachments.js::askAddMenuBuild）。
 *  ⚠️ 键盘契约：**Enter = 发送 / 存入，Shift+Enter = 换行**。输入域是多行 textarea，
 *     所有「Enter 发送」处理器都必须判 `!e.shiftKey`（漏判会吃掉换行，多行输入形同虚设）。
 *     ⚠️ 且必须先过 `qaImeComposing`：**输入法合成中（候选词还没上屏）的那一下 Enter 归输入法，
 *        不是发送**（用户 2026-09-28：「输入时的 enter 不要发出去」）。
 *        合成态一律放行 —— 不 preventDefault、不提交，让候选词正常上屏；其余 Enter 一下即发，
 *        不做二次确认（用户 2026-09-29：「不需要两次 enter」）。 */
const ASK_CHIP_LABEL = { req: "需求", sim: "模拟测试", rec: "录屏", shot: "截屏", shorts: "短视频", doc: "产品文档", case: "测试用例" };
const ASK_CHIP_SIDE = { req: "发送即新建", sim: "发送即开测", rec: "发送即录屏", shot: "发送即截屏", shorts: "发送即短视频", doc: "普通对话", case: "普通对话" };

/** 组件标记：返回 box + **四只菜单**（＋ / 引擎 / 模型 / 权限）的 HTML；菜单挂 box 之后，交由 qaMenuPlace 定位。
 *  ⚠️ 站内实例**必须渲染成同一个样子**（用户 2026-09-17 复查截图后强调）：
 *     同一个 box 类、同样的 ＋/权限胶囊、同样的引擎 + 模型下拉、同样位置的提示行、同样只有「一个发送钮」。
 *     实例之间只允许不同：① 占位文案。（2026-09-21 中途曾按实例分过模型控件形态 ——
 *     首页可选 / 任务对话只读 —— 用户随后定稿「还是都可以选择模型」，已还原成同源同形态。）
 *     曾经踩过：给某个实例省掉 ＋/权限、另加「取消」与一段说明文字 →
 *     用户一眼看出「两个对话框明显不一样，怎么会是共用」。别再按「上下文是否需要」去裁剪。
 *  输入域统一 textarea（rows=1 + field-sizing 自增高）—— 多行能力归组件本身。 */
function askBoxMarkup(cfg){
  const p = cfg.p;
  const boxCls = cfg.boxCls || "home-box";
  const ph = escHtml((cfg.ph && cfg.ph.ask) || "今天帮你做些什么？");
  /* 待发队列行（用户 2026-09-18：「新需求排列在会话框上面」）：**会话框上方**的一行，
     处理中提交的下一条排在这里等自动提交（见 qaMsgQueue 那段）。内容由 qaMsgQueueRender 现填。
     ⚠️ 只给**会话框**（home 实例）画 —— home2 是新建对话入口，它提交的永远是「新会话」，
        不排队，也不该挂一行永远空的 DOM。 */
  const queueRow = cfg.p === QA_QUEUE_INSTANCE
    ? `<div class="ag-queue" id="${p}-queue" data-testid="${p}-queue" hidden></div>` : "";
  return `
            <!-- 右上角吉祥物「大喵」= 应用图标抠图（用户 2026-09-17）；纯装饰，
                 绝对定位在 .home-entry 里、框顶之上，不占布局也不拦点击（见 .ag-mascot 注释） -->
            <img class="ag-mascot" id="${p}-mascot" src="assets/damiao-mascot.png"
                 alt="" aria-hidden="true" draggable="false">
            ${queueRow}
            <div class="ag-box ${boxCls}" id="${p}-box" data-testid="${p}-box">
              <div class="ag-atts" id="${p}-atts" data-testid="${p}-atts" hidden></div>
              <div class="ag-top">
                <!-- 胶囊在输入域**之前**、同一个 flex 行里 → 与文字同行（装不下才由 flex-wrap 折行） -->
                <div class="ag-refs" id="${p}-refs" data-testid="${p}-refs" hidden></div>
                <textarea id="${p}-input" data-testid="${p}-input" rows="1" placeholder="${ph}" autocomplete="off"></textarea>
                <div class="ag-hints" id="${p}-hints"><span class="ag-hint" id="${p}-hint-at"><b>@</b>引用对话文件</span><span class="ag-hint-sep">，</span><span class="ag-hint" id="${p}-hint-slash"><b>/</b>调用技能与指令</span></div>
              </div>
              <div class="ag-bot">
                <button class="ag-plus" id="${p}-plus" data-testid="${p}-plus" title="选择需求 / 模拟测试 / 录屏 / 产品 / 用例" aria-haspopup="listbox" aria-expanded="false" type="button">${ASK_ICON.plus}</button>
                <!-- 访问权限 = **四档选择器**（2026-09-18 用户：「这个地方要选择」）。
                     改造前是一枚二值胶囊（点一下在「允许完全访问 / 受限访问」间切换）——
                     现在点开四档菜单，按钮文案 = 当前档位，选中态由 qaPermSync() 统一刷。
                     ⚠️ 两个实例必须渲染成同一个样子：文案与选中态都来自**同一份** qaPerm。 -->
                <button class="ag-access" id="${p}-access" data-testid="${p}-access" title="选择访问权限" aria-haspopup="listbox" aria-expanded="false" type="button">${ASK_ICON.access}<span class="txt">自动审批</span>${ASK_ICON.chev}</button>
                <div class="ag-right">
                  <span class="ag-busy" aria-hidden="true"></span>
                  <button class="ag-model ag-engine" id="${p}-engine" data-testid="${p}-engine" type="button" title="选择引擎">${ASK_ICON.engine}<span class="txt">claude</span>${ASK_ICON.chev}</button>
                  <!-- 模型 = **可选**（两个实例同款；用户 2026-09-21 定稿「还是都可以选择模型
                       但是自动切换模型的时候 对话框下拉的选中模型显示也要变」）。
                       文案与选中态由 qaModelPickSync() 现填；首帧给一个不撒谎的占位。 -->
                  <button class="ag-model ag-mdl-pick" id="${p}-model" data-testid="${p}-model" type="button" title="选择模型">${ASK_ICON.model}<span class="txt">引擎默认</span>${ASK_ICON.chev}</button>
                  <button class="ag-send" id="${p}-send" data-testid="${p}-send" title="发送" type="button">${ASK_ICON.arrow}${ASK_ICON.stop}</button>
                </div>
              </div>
            </div>
            <div class="ag-menu scroll-y" id="${p}-engine-menu" hidden><span class="ag-menu-cap">引擎</span><button class="ag-model-opt on" data-engine="claude" type="button">claude</button></div>
            <div class="ag-menu scroll-y" id="${p}-model-menu" data-testid="${p}-model-menu" hidden></div>
            <!-- 访问权限菜单：4 档，内容由 qaPermMenuBuild() 现填（见该函数注释） -->
            <div class="ag-menu scroll-y ag-perm" id="${p}-perm-menu" data-testid="${p}-perm-menu" role="listbox" hidden></div>
            <!-- ＋ 菜单：5 个类型选项（需求 / 模拟测试 / 录屏 / 产品文档 / 测试用例）—— 内容由 askAddMenuBuild 现填 -->
            <div class="ag-menu scroll-y ag-add" id="${p}-add-menu" data-testid="${p}-add-menu" hidden></div>`;
}

/** 站内实例清单（唯一真相）：挂载、事件绑定、选择器数组都以它为准。
 *  ⚠️ 2026-09-17：需求管理页的「新建需求」输入框已下线（新建需求的唯一入口 = 对话框 ＋ 菜单）。 */
const ASK_INSTANCES = ["home2", "home"];
/** 各实例配置 —— 允许不同的只有占位文案；尺寸/控件/结构一律相同
 *  ⚠️ 2026-09-21 中途曾在这里加过 `modelCtl`（首页可选 / 任务对话只读），用户随后定稿
 *     「还是都可以选择模型」→ 已删除。模型选择器对**两个实例**都在，别再按实例裁剪。 */
const ASK_CFG = {
  home2: { p: "home2", boxCls: "home-box", ph: { ask: "今天帮你做些什么？" } },
  /* ⚠️ 2026-09-21 审美修订（文案自相矛盾）：home 实例原占位是「继续这个任务，或换个要求？」，
     但**空任务**下这句话不成立 —— 空态正上方写着「这个任务还没有对话」，下面却让用户「继续」。
     改成一个在两种状态下都成立的问法：没有历史时 = 描述要做的事，有历史时 = 接着追问。
     （组件实例之间允许不同的只有占位文案这一条，见文件头注释。） */
  home:  { p: "home",  boxCls: "home-box", ph: { ask: "描述你要做的事，或继续追问…" } },
};

/** 权限档位菜单内容（4 档）—— 两个实例灌**同一份** html。
 *  行内 = 图标 + 「档位名 / 一句人话说明」 + 当前档位的 ✓（与参考稿一致）。
 *  ⚠️ **文案规则（用户 2026-09-18：「显示的说明参考一下 比如 LLM Guardian 这样就不是人话」）**：
 *     `desc` 必须是**人话** —— 短句、口语、第二人称「你」、零术语。写「由 AI 审核，拿不准才问你」，
 *     不写「越界由 LLM Guardian 判定」；不写「沙箱」「宿主机」「只读放行」这类工程词。
 *     精确语义放 `detail`（只进 title 提示，不进可见文案）。
 *     改文案时**先读这一条**：菜单是给用户看「我的操作会怎么被处理」，不是给工程师看架构。
 *  选项用 `.ag-model-opt`（与引擎菜单同款）：键盘导航（qaKbMove 找 .ag-model-opt）、
 *  hover/kb 高亮、`.on` 选中态全部白拿，不必再写一套。 */
function qaPermMenuBuild(){
  const html = '<span class="ag-menu-cap">访问权限</span>' + QA_PERMS.map(x=>
    `<button class="ag-model-opt${x.id===qaPerm?" on":""}" data-perm="${escHtml(x.id)}" type="button" role="option" aria-selected="${x.id===qaPerm}" title="${escHtml(x.label + " · " + x.detail)}">
      <span class="pm-opt"><span class="pm-ic">${x.icon}</span><span class="pm-tx"><span class="nm">${escHtml(x.label)}</span><span class="ds">${escHtml(x.desc)}</span></span><span class="pm-ck">${QA_PERM_CHECK}</span></span>
    </button>`).join("") + qaPermNoteHtml();
  ASK_INSTANCES.forEach(p=>{ const m = $("#" + p + "-perm-menu"); if(m) m.innerHTML = html; });
}

/** 档位菜单底部那行说明 —— **只有当前引擎不接受档位时才画**。
 *  为什么必须有：CLI 只对 claude / codebuddy / codebuddy-ai 实现了 `--permission`，
 *  其余引擎（trae / llm / codex / openclaw / arkclaw / codebuddy-gateway）收到它会被 `exit 2` 打回、整轮一个字
 *  都拿不到（2026-09-21 用户报障「openclaw 对话没对接好」的根因）。所以主进程对这几种引擎
 *  **一个都不下发**（desktop/main.js / reqboard.cjs / requirements.cjs 三处同判据）。
 *  那界面就不能还写着「自动审批」装作生效了 —— 静默忽略是本仓库明确不许的做法。
 *  ⚠️ 文案是**人话**（与档位说明同一口径）：说清「这一档在这台引擎上不生效、走引擎自己的
 *     默认档」，不写 flag 名、不写「permission 字段」。 */
function qaPermNoteHtml(){
  if(typeof qaPermSupported !== "function" || qaPermSupported()) return "";
  return `<span class="ag-menu-note" data-perm-note="unsupported" data-testid="perm-note">「${escHtml(qaEngine)}」不接受权限档位，这一轮按它自己的默认方式执行</span>`;
}

/** 按钮文案 / 选中态 / 菜单勾选三处同步 —— **唯一**的档位渲染出口。
 *  按钮高亮（`.on`）只在「完全访问」上：它是唯一不隔离、不询问的档，需要一眼可辨；
 *  其余三档都是受限态，统一走灰字（与改造前 `.ag-access:not(.on)` 同调）。
 *  菜单里则相反 —— **完全访问永远带朱砂**（哪怕没选中），因为它是危险档，
 *  要像参考稿那样在列表里就能看出「这条不一样」；选中态另由 ✓ 表达。
 *  ⚠️ 当前引擎不接受档位时（qaPermSupported 假），按钮 **title 如实改口** ——
 *     按钮文案仍是那一档（它记的是用户的选择，换回 claude 就照样生效），
 *     但要说清「这台引擎上不生效」，不然就是静默忽略。 */
function qaPermSync(){
  const cur = qaPermOf(qaPerm);
  const unsup = (typeof qaPermSupported === "function") && !qaPermSupported();
  ASK_INSTANCES.forEach(p=>{
    const b = $("#" + p + "-access");
    if(b){
      b.classList.toggle("on", cur.id === QA_PERM_ALERT);
      b.dataset.perm = cur.id;
      b.title = `访问权限 · ${cur.label}（${cur.detail}）`
        + (unsup ? ` · 「${qaEngine}」不接受权限档位，这一轮按它自己的默认方式执行` : "");
      const t = b.querySelector(".txt");
      if(t) t.textContent = cur.label;
    }
    const m = $("#" + p + "-perm-menu");
    if(m) [...m.querySelectorAll(".ag-model-opt")].forEach(o=>{
      const on = o.dataset.perm === cur.id;
      o.classList.toggle("on", on);
      o.setAttribute("aria-selected", String(on));
    });
  });
}

/** 切换档位（写 localStorage + 三处同步 + 反馈）。
 *  反馈里带上**那句人话说明** —— 档位差别是行为差别，只报个名字用户记不住。 */
function qaPermSet(id){
  if(!QA_PERM_IDS.includes(id)) return false;
  qaPerm = id;
  try{ localStorage.setItem("qaPerm", qaPerm); }catch(e){}
  qaPermSync();
  const cur = qaPermOf(qaPerm);
  toast(`已切换为「${cur.label}」· ${cur.desc}`);
  return true;
}

/** 实例运行态：已选的胶囊类型（按实例分开 —— 两个输入框互不干扰） */
const askChips = {};                      // p → ["req" | "sim" | "rec" | "doc" | "case", ...]
const askInputOf = (p)=> $("#" + p + "-input");
const askHasReq = (p)=> (askChips[p] || []).includes("req");
/** 挂了「模拟测试」胶囊吗（2026-09-21 新增的模式，发送即开测 —— 见 24-agent-test.js）。
 *  与「需求」胶囊同一套机制：语义由胶囊决定，拦截点在 qaAddBind 的捕获阶段。 */
const askHasSim = (p)=> (askChips[p] || []).includes("sim");
/** 挂了「录屏」胶囊吗（2026-09-21 新增的模式，发送即录屏 —— 见 25-record.js）。
 *  与前三枚同一套机制；⚠️ 拦截顺序是 rec → shot → sim → req（改发送语义的四枚里，越"重"的越先判，
 *  见 09-ask-event.js 的注释）。 */
const askHasRec = (p)=> (askChips[p] || []).includes("rec");
/** 挂了「截屏」胶囊吗（2026-09-21 新增的模式，发送即截屏 —— 见 25-record.js）。
 *  与「录屏」是**兄弟**（同一套前置体检 / MCP 工具 / 兜底），只差「要一帧还是要一段」。 */
const askHasShot = (p)=> (askChips[p] || []).includes("shot");
/** 挂了「短视频」胶囊吗（2026-09-23 新增的模式，发送即跑短视频流水线 —— 见 28-shorts.js）。
 *  与「录屏 / 截屏」同一套拦截机制（捕获阶段截发送 → desk.ask{ mode }），但它**不碰浏览器**：
 *  引擎只产任务单（```shorts-task 围栏块），壳逐阶段执行 magic-shorts CLI（选题 → 脚本 → 配音 → 成片）。 */
const askHasShorts = (p)=> (askChips[p] || []).includes("shorts");

/** 胶囊行渲染（＋ 菜单选中后出现在对话框里的胶囊，可单个摘掉） */
function askChipsSync(p){
  const el = $("#" + p + "-refs"); if(!el) return;
  const list = askChips[p] || [];
  el.hidden = !list.length;
  el.innerHTML = list.map(kind=>
    `<span class="ag-ref" title="${escHtml(ASK_CHIP_LABEL[kind] || kind)}">` +
      `<span class="rk">${escHtml(ASK_CHIP_LABEL[kind] || kind)}</span>` +
      `<button class="rx" data-kind="${kind}" type="button" aria-label="移除${escHtml(ASK_CHIP_LABEL[kind] || kind)}胶囊">×</button>` +
    `</span>`).join("");
}
function askChipToggle(p, kind){
  if(!ASK_CHIP_LABEL[kind]) return;
  const list = askChips[p] || (askChips[p] = []);
  const i = list.indexOf(kind);
  if(i >= 0){ list.splice(i, 1); toast(`已移除${ASK_CHIP_LABEL[kind]}胶囊`); }
  else {
    list.push(kind);
    toast(kind === "req" ? "已挂需求胶囊 · 发送即新建需求"
      : kind === "sim" ? "已挂模拟测试胶囊 · 发送即开测（数字分身 ↔ 默认引擎，按判据打分）"
      : kind === "rec" ? "已挂录屏胶囊 · 发送即录屏（引擎驱动内嵌浏览器录一段，产物落「浏览器工具 → 产物」）"
      : kind === "shorts" ? "已挂短视频胶囊 · 发送即跑流水线（选题 → 脚本 → 配音 → 成片，几分钟）"
      : `已挂${ASK_CHIP_LABEL[kind]}胶囊`);
  }
  askChipsSync(p);
}
function askChipDrop(p, kind){
  const list = askChips[p] || [];
  const i = list.indexOf(kind);
  if(i < 0) return;
  list.splice(i, 1);
  askChipsSync(p);
  toast(`已移除${ASK_CHIP_LABEL[kind] || kind}胶囊`);
}
/** 清空胶囊（发送后调用） */
function askChipsClear(p){
  if(!(askChips[p] || []).length) return;
  askChips[p] = [];
  askChipsSync(p);
}

