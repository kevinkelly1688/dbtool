/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: sidesize.js
 * @time: 2026/10/28
 */
// 在线查询左侧对象栏（表 / 可编程对象 / Redis key）的宽度：拖动右边缘的分隔条调整，长的表名、存储过程名可以看全。
// 双击分隔条或按 Home 恢复默认宽度，聚焦后按 ← → 微调（Shift 加大步长）；宽度记在浏览器里，下次打开沿用。
// 记在浏览器里只是个人习惯：存取失败（隐私模式、被禁用）时照常使用，只是不记忆。窄屏（≤ 800px）上下排版，分隔条由样式隐藏。
const KEY = 'dbtool.query.sideWidth.v1';
const DEFAULT = 240; // 和 app.css 里 --wb-side-w 的默认值一致
const MIN = 180;
const MAX = 720;
const MIN_MAIN = 360; // 右边编辑器和结果区至少留这么宽
const STEP = 16;
const STEP_BIG = 64;

function load() {
  try {
    const n = parseInt(localStorage.getItem(KEY), 10);
    return Number.isFinite(n) ? n : null;
  } catch (_) {
    return null;
  }
}

function save(n) {
  try {
    if (n === null) localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, String(n));
  } catch (_) { /* 忽略 */ }
}

// wb 是 .wb-body（宽度写在它的 --wb-side-w 上），handle 是分隔条；onResize 在宽度变化后调用（编辑器要重新量宽度）。
export function initSideResize(wb, handle, onResize = () => {}) {
  const side = wb.querySelector('.wb-side');
  if (!side || !handle) return;
  let wanted = load() ?? DEFAULT; // 用户要的宽度；窗口太窄时实际显示的会小一些，窗口放大后再恢复
  let shown = DEFAULT;
  let raf = 0;

  const maxNow = () => Math.max(MIN, Math.min(MAX, wb.clientWidth - MIN_MAIN));
  const clamp = (n) => Math.round(Math.max(MIN, Math.min(maxNow(), n)));
  const notify = () => {
    if (raf) return;
    raf = requestAnimationFrame(() => { raf = 0; onResize(); });
  };
  function apply(n, remember = true) {
    shown = clamp(n);
    if (remember) wanted = shown;
    wb.style.setProperty('--wb-side-w', shown + 'px'); // CSSOM 写入，不算内联样式，严格 CSP 下也可用
    handle.setAttribute('aria-valuemin', String(MIN));
    handle.setAttribute('aria-valuemax', String(maxNow()));
    handle.setAttribute('aria-valuenow', String(shown));
    notify();
  }

  let startX = 0;
  let startW = 0;
  let dragging = false;
  handle.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    dragging = true;
    startX = e.clientX;
    startW = side.getBoundingClientRect().width;
    handle.setPointerCapture(e.pointerId);
    document.body.classList.add('wb-resizing');
  });
  handle.addEventListener('pointermove', (e) => { if (dragging) apply(startW + e.clientX - startX); });
  const end = () => {
    if (!dragging) return;
    dragging = false;
    document.body.classList.remove('wb-resizing');
    save(wanted);
    onResize();
  };
  handle.addEventListener('pointerup', end);
  handle.addEventListener('pointercancel', end);
  handle.addEventListener('lostpointercapture', end);

  const reset = () => { apply(DEFAULT); save(null); };
  handle.addEventListener('dblclick', reset);
  handle.addEventListener('keydown', (e) => {
    const step = e.shiftKey ? STEP_BIG : STEP;
    if (e.key === 'ArrowLeft') apply(shown - step);
    else if (e.key === 'ArrowRight') apply(shown + step);
    else if (e.key === 'Home') { reset(); e.preventDefault(); return; }
    else return;
    e.preventDefault();
    save(wanted);
  });
  window.addEventListener('resize', () => apply(wanted, false)); // 窗口变窄时别让侧栏把编辑器挤没了

  apply(wanted);
}
