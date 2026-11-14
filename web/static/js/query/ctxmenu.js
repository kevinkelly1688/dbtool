/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: ctxmenu.js
 * @time: 2026/10/28
 */
// 弹出菜单（结果表列头“复制整列”、库表树右键“对象信息”共用）。同时只开一个；
// 点别处 / Esc / 滚动 / 窗口缩放关闭，上下方向键切换，Tab 关闭；关闭后焦点回到打开前的位置。
import { el } from './ui.js';

let closeCurrent = null;

export function closeMenu() {
  if (closeCurrent) closeCurrent();
}

// at: { x, y } 鼠标位置，或 { el } 锚点元素（菜单贴在它下面，放不下时翻到上面）。
// items: [{ label, run }]：点选后先关菜单再调用 run()。
// opts: { label: 无障碍名称, compact: 宽度贴合内容（单项菜单用，不撑到最小宽度）, scrollRoots: 这些元素滚动时关闭菜单, ignore: 点它不算“点别处”（由它自己的点击处理收起）, onClose: 关闭回调 }。返回关闭函数。
export function openMenu(at, items, opts = {}) {
  closeMenu();
  const prev = document.activeElement;
  const menu = el('div', opts.compact ? 'pop-menu compact' : 'pop-menu');
  menu.setAttribute('role', 'menu');
  if (opts.label) menu.setAttribute('aria-label', opts.label);
  const buttons = items.map((it) => {
    const b = el('button');
    b.type = 'button';
    b.setAttribute('role', 'menuitem');
    b.append(document.createTextNode(it.label));
    b.addEventListener('click', () => {
      close(true);
      Promise.resolve(it.run()).catch(() => { /* run 自己负责提示错误 */ });
    });
    menu.append(b);
    return b;
  });
  document.body.append(menu);

  const mw = menu.offsetWidth;
  const mh = menu.offsetHeight;
  let x;
  let y;
  if (at.el) {
    const r = at.el.getBoundingClientRect();
    x = r.left;
    y = r.bottom + 4 + mh > window.innerHeight ? Math.max(8, r.top - 4 - mh) : r.bottom + 4;
  } else {
    x = at.x;
    y = at.y + mh > window.innerHeight ? Math.max(8, at.y - mh) : at.y;
  }
  menu.style.left = Math.max(8, Math.min(x, window.innerWidth - mw - 8)) + 'px';
  menu.style.top = Math.max(8, y) + 'px';
  if (buttons.length) buttons[0].focus();

  const onDown = (e) => { if (!menu.contains(e.target) && !(opts.ignore && opts.ignore.contains(e.target))) close(true); };
  const onKey = (e) => {
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); close(true); return; }
    if (e.key === 'Tab') { close(false); return; }
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      const i = buttons.indexOf(document.activeElement);
      const n = buttons.length;
      buttons[(i + (e.key === 'ArrowDown' ? 1 : n - 1)) % n].focus();
    }
  };
  const off = () => close(false);
  const roots = opts.scrollRoots || [];
  document.addEventListener('mousedown', onDown, true);
  document.addEventListener('contextmenu', onDown, true); // 在别处右键：先关掉这一个（新的菜单随后由那处的处理函数打开）
  menu.addEventListener('keydown', onKey);
  for (const r of roots) r.addEventListener('scroll', off, { passive: true });
  window.addEventListener('resize', off);

  function close(restoreFocus) {
    if (closeCurrent !== close) return;
    closeCurrent = null;
    document.removeEventListener('mousedown', onDown, true);
    document.removeEventListener('contextmenu', onDown, true);
    for (const r of roots) r.removeEventListener('scroll', off);
    window.removeEventListener('resize', off);
    menu.remove();
    if (restoreFocus && prev && typeof prev.focus === 'function' && document.contains(prev)) {
      try { prev.focus(); } catch (_) { /* 元素已不可聚焦 */ }
    }
    if (opts.onClose) opts.onClose();
  }
  closeCurrent = close;
  return () => close(false);
}
