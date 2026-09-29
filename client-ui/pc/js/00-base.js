/* 基础助手（从 magic-test 的 01-core.js / 15-layout.js 原样搬来）。
 *
 *  这一份是为了让**逐字复制过来的对话框代码**能跑起来 —— 它们按 magic-test 的全局函数风格写的
 *  （`$("#home-input")` 这类），所以这里保持同名同义，不去改那些文件里的调用点。
 *  改动请优先改上游（magic-test 的 src/js/），再同步到这里，避免两份漂移。
 */

/* −− 选择器 −−
   ⚠️ 注意是 **CSS 选择器**版，不是 id 版：调用点写的是 `$("#home-input")`。 */
const $  = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];

/** 幂等写 innerHTML：渲染结果字符串没变就不碰 DOM。
 *  上游为「运行中列表每 700ms 重建一次导致闪动」而写；对话框的流式重绘也吃这条。 */
function htmlWrite(el, html) {
  if (!el) return false;
  if (el.__html === html) return false;
  el.__html = html;
  el.innerHTML = html;
  return true;
}

function escHtml(s) {
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
}

/** 输入法合成守卫：中文输入法里「选词那一下按的 Enter」归输入法，不是提交。
 *  对话框的回车提交必须先过它，否则拼音打到一半敲回车会把半截消息发出去。 */
function qaImeComposing(e) {
  return !!e && (e.isComposing === true || e.keyCode === 229);
}

let toastTimer = null;
function toast(msg, asHtml) {
  const el = $("#toast");
  if (!el) return;
  const box = $("#toast-msg") || el;
  if (asHtml) box.innerHTML = msg;
  else box.textContent = msg;
  el.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove("show"), 2600);
}

/* 全局状态：上游 01-core.js 的那一份里混了工作台的东西（view/tasks/cases/prds），
   这里只保留对话框真正会读的字段；其余留空对象，避免别处读到 undefined。 */
const state = {
  view: "chat",
  tasks: [], cases: [], prds: [], timer: null, activeTask: null,
};
