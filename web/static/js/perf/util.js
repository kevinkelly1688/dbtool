/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: util.js
 * @time: 2026/10/28
 */
// 性能工单各页面共用的小工具：建元素、数字/时长格式、级别/状态徽标、消息条、弹窗开关。
// 往 DOM 写内容一律用 textContent / createElement，不拼 innerHTML。
import { t, locale } from '../i18n.js';

export const $ = (id) => document.getElementById(id);

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

export function svg(tag, attrs = {}, text) {
  const e = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, String(v));
  if (text !== undefined) e.textContent = text;
  return e;
}

export function btn(text, cls, onClick) {
  const b = el('button', 'btn' + (cls ? ' ' + cls : ''), text);
  b.type = 'button';
  if (onClick) b.addEventListener('click', onClick);
  return b;
}

export const fmtNum = (n, digits = 0) => (n === null || n === undefined || Number.isNaN(Number(n)) ? '-' : Number(n).toLocaleString(locale, { maximumFractionDigits: digits }));

// 紧凑数字：1.2K / 3.4M
export function fmtShort(n) {
  const v = Number(n);
  if (!Number.isFinite(v)) return '-';
  if (v >= 1e9) return (v / 1e9).toFixed(1) + 'G';
  if (v >= 1e6) return (v / 1e6).toFixed(1) + 'M';
  if (v >= 1e3) return (v / 1e3).toFixed(1) + 'K';
  return String(Math.round(v));
}

// 小时数 → “3 小时”“2 天 5 小时”
export function fmtHours(h) {
  const a = Math.abs(h);
  if (a < 1) return t('{n} 分钟', { n: Math.max(1, Math.round(a * 60)) });
  if (a < 48) return t('{n} 小时', { n: Math.round(a) });
  return t('{n} 天', { n: Math.floor(a / 24) }) + (Math.round(a % 24) ? ' ' + t('{n} 小时', { n: Math.round(a % 24) }) : '');
}

export function fmtSeconds(s) {
  const v = Number(s) || 0;
  if (v >= 3600) return t('{n} 小时', { n: (v / 3600).toFixed(1) });
  if (v >= 60) return t('{n} 分钟', { n: Math.round(v / 60) });
  return Math.round(v) + ' s';
}

export function sevBadge(sev, label) {
  return el('span', 'sev ' + String(sev).toLowerCase(), label || sev);
}

// 状态徽标样式：待分派/待受理/豁免审批中 → waiting；处理中/待验证 → running；已关闭 → aborted
export function statusBadge(status, label) {
  const cls = { new: 'waiting', assigned: 'waiting', waive_pending: 'waiting', accepted: 'running', fixed: 'running', closed: 'aborted' }[status] || '';
  return el('span', 'st ' + cls, label);
}

export function setMsg(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

export function openModal(id) { $(id).hidden = false; }
export function closeModal(id) { $(id).hidden = true; }

// 弹窗里带 data-close 的按钮关闭所在弹窗；点遮罩空白处、按 Esc 也关闭
export function wireModals() {
  document.querySelectorAll('.modal-mask').forEach((m) => {
    m.querySelectorAll('[data-close]').forEach((b) => b.addEventListener('click', () => { m.hidden = true; }));
    m.addEventListener('mousedown', (e) => { if (e.target === m) m.hidden = true; });
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') document.querySelectorAll('.modal-mask:not([hidden])').forEach((m) => { m.hidden = true; });
  });
}

export function fillSelect(sel, items, textOf, valueOf, first) {
  sel.replaceChildren();
  if (first) sel.append(new Option(first[1], first[0]));
  for (const it of items) sel.append(new Option(textOf(it), String(valueOf(it))));
}

// 带忙碌状态的异步按钮
export async function busy(button, fn) {
  if (button) { button.disabled = true; button.setAttribute('aria-busy', 'true'); }
  try { return await fn(); } finally { if (button) { button.disabled = false; button.removeAttribute('aria-busy'); } }
}

export function qs(params) {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== '' && v !== null && v !== undefined && v !== 0) u.set(k, String(v));
  return u.toString();
}

// 按钮级权限（给配置页的“仅查看”只读模式用）：带 data-btn 的元素，无权限时置灰（或按系统设置隐藏），不是点了才 403。
export { applyButtons, loadButtons, can } from '../btnperm.js';

// 把弹窗里的所有输入框置为只读 / 恢复（只读模式下“查看”配置用）。
export function setFormReadOnly(modalId, ro) {
  document.querySelectorAll(`#${modalId} input, #${modalId} select, #${modalId} textarea`).forEach((e) => { e.disabled = ro; });
}

// 在容器顶部显示“只读”提示条。
export function showReadOnlyNotice(box, what) {
  box.replaceChildren(el('div', 'alert info', t('你只有“查看配置”权限，{what}为只读。', { what })));
}
