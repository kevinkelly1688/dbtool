/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: drawer.js
 * @time: 2026/10/28
 */
// 右侧抽屉：编辑规则 / 命中详情 / 添加例外共用。同一时间只有一个；Esc 或点遮罩（按下和松开都在遮罩上）关闭。
import { t } from '../i18n.js';
import { h, clear, $ } from './vocab.js';

let current = null;

export function closeDrawer() {
  if (!current) return;
  const c = current;
  current = null;
  const mask = $('fw-mask');
  mask.hidden = true;
  clear(mask);
  document.removeEventListener('keydown', c.onKey, true);
  if (c.opener && document.contains(c.opener)) c.opener.focus();
}

// opts: { title, badge?: Node, narrow?, body: Node[], footer: Node[], canClose?: () => boolean | Promise<boolean> }
// 返回 { close, root, body }。
export function openDrawer(opts) {
  if (current) closeDrawer();
  const mask = $('fw-mask');
  const opener = document.activeElement;
  const requestClose = async () => {
    if (opts.canClose && !(await opts.canClose())) return;
    closeDrawer();
  };
  const x = h('button', { class: 'x', type: 'button', 'aria-label': t('关闭'), text: '✕', onclick: requestClose });
  const body = h('div', { class: 'bd' }, opts.body || []);
  const footer = h('div', { class: 'ft' }, opts.footer || []);
  const titleId = 'fw-dr-title';
  const root = h('div', { class: 'fw-drawer' + (opts.narrow ? ' narrow' : ''), role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': titleId },
    h('h3', { id: titleId }, opts.title, opts.badge || null, x), body, footer);
  clear(mask).append(root);
  mask.hidden = false;
  let downOnMask = false;
  mask.onmousedown = (e) => { downOnMask = e.target === mask; };
  mask.onclick = (e) => { if (e.target === mask && downOnMask) requestClose(); };
  const onKey = (e) => {
    if (e.key !== 'Escape') return;
    // 确认弹窗（confirm.js）盖在抽屉上面时，Esc 只关它
    if (document.querySelector('.cf-mask')) return;
    e.stopPropagation();
    requestClose();
  };
  document.addEventListener('keydown', onKey, true);
  current = { onKey, opener };
  const first = body.querySelector('input:not([type=hidden]), select, textarea') || footer.querySelector('button');
  if (first) first.focus();
  return { close: closeDrawer, requestClose, root, body, footer };
}

export function isDrawerOpen() { return current !== null; }
