/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: modal.js
 * @time: 2026/10/28
 */
// 弹窗外壳（内容预览、对象信息共用）：遮罩 + 对话框 + 焦点陷阱 + Esc / 点遮罩关闭 + 锁页面滚动 + 关闭后焦点回到原处。
// 同时只开一个：再开一个会先关掉上一个。内容由调用方放进返回的 dlg。
import { el } from './ui.js';

const FOCUSABLE = 'button:not([disabled]),textarea:not([disabled]),input:not([disabled]),select:not([disabled]),a[href],[tabindex]:not([tabindex="-1"])';
let current = null;

export function closeModal() {
  if (current) current.close();
}

// opts: { label: 无障碍名称, wide: 更宽的对话框 }。
// 返回 { dlg, close(), onClose(fn) }：onClose 登记的清理函数在关闭时依次执行（如销毁编辑器）。
export function openModal(opts) {
  closeModal();
  const mask = el('div', 'cv-mask');
  const dlg = el('div', 'cv-dialog' + (opts.wide ? ' cv-wide' : ''));
  dlg.setAttribute('role', 'dialog');
  dlg.setAttribute('aria-modal', 'true');
  dlg.setAttribute('aria-label', opts.label || '');
  mask.append(dlg);

  const prev = document.activeElement;
  const cleanups = [];
  let closed = false;
  const focusables = () => [...dlg.querySelectorAll(FOCUSABLE)].filter((e) => e.getClientRects().length > 0);

  const onKey = (e) => {
    if (closed) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopImmediatePropagation();
      api.close();
      return;
    }
    if (e.key === 'Tab') {
      const list = focusables();
      if (!list.length) { e.preventDefault(); return; }
      const first = list[0];
      const last = list[list.length - 1];
      const act = document.activeElement;
      if (!dlg.contains(act)) { e.preventDefault(); first.focus(); }
      else if (e.shiftKey && act === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && act === last) { e.preventDefault(); first.focus(); }
    }
  };
  const onFocusIn = (e) => {
    if (closed || dlg.contains(e.target)) return;
    const list = focusables();
    if (list.length) list[0].focus();
  };

  const api = {
    dlg,
    onClose(fn) { cleanups.push(fn); },
    close() {
      if (closed) return;
      closed = true;
      if (current === api) current = null;
      document.removeEventListener('keydown', onKey, true);
      document.removeEventListener('focusin', onFocusIn, true);
      for (const f of cleanups) {
        try { f(); } catch (_) { /* 清理失败不影响关闭 */ }
      }
      mask.remove();
      document.documentElement.classList.remove('cv-lock');
      if (prev && typeof prev.focus === 'function' && document.contains(prev)) {
        try { prev.focus(); } catch (_) { /* 元素已不可聚焦 */ }
      }
    },
  };

  // 按下和松开都在遮罩上才关闭（在弹窗里拖选文字滑出去不误关）
  let downOnMask = false;
  mask.addEventListener('mousedown', (e) => { downOnMask = e.target === mask; });
  mask.addEventListener('click', (e) => {
    if (e.target === mask && downOnMask) api.close();
    downOnMask = false;
  });
  document.addEventListener('keydown', onKey, true);
  document.addEventListener('focusin', onFocusIn, true);
  document.body.appendChild(mask);
  document.documentElement.classList.add('cv-lock');
  current = api;
  return api;
}
