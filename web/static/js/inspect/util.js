/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: util.js
 * @time: 2026/10/28
 */
// 性能巡检页面共用的小工具。往 DOM 写内容一律用 textContent / createElement。
import { t } from '../i18n.js';

export const $ = (id) => document.getElementById(id);

// 列表连接符：中文用顿号 / 分号，英文换成逗号 / 分号加空格（译文在目录里）。
export const SEP = t('、');
export const SEMI = t('；');

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = String(text);
  return e;
}

export function btn(text, cls, onClick) {
  const b = el('button', 'btn' + (cls ? ' ' + cls : ''), text);
  b.type = 'button';
  if (onClick) b.addEventListener('click', onClick);
  return b;
}

export function setMsg(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

// 服务端返回带时区的时间（RFC3339，本地时区），页面按服务器本地时间显示。
export const fmtTime = (s) => (s ? s.slice(0, 16).replace('T', ' ') : '—');
export const fmtFull = (s) => (s ? s.slice(0, 19).replace('T', ' ') : '—');

export function fmtElapsed(sec) {
  sec = Math.max(0, sec | 0);
  if (sec >= 3600) return `${Math.floor(sec / 3600)}h${String(Math.floor((sec % 3600) / 60)).padStart(2, '0')}m`;
  return `${Math.floor(sec / 60)}m${String(sec % 60).padStart(2, '0')}s`;
}

export const STATUS = {
  running: [t('进行中'), 'running'], finish: [t('已完成'), 'passed'], failed: [t('失败'), 'rejected'], cancelled: [t('已取消'), 'aborted'],
};

export function statusBadge(r) {
  const [text, cls] = STATUS[r.status] || [r.status, ''];
  const wrap = el('span');
  wrap.append(el('span', 'st ' + cls, text));
  if ((r.status === 'running' || r.status === 'cancelled') && r.total > 0) {
    wrap.append(document.createTextNode(' '), el('small', 'muted', t('{done} / {total} 项', { done: r.done, total: r.total })));
  }
  return wrap;
}

export function countBadge(n, cls) {
  if (!n) return document.createTextNode('0');
  return el('span', 'badge ' + cls, n);
}

export function scoreEl(score) {
  if (score === null || score === undefined) return document.createTextNode('–');
  return el('span', 'score ' + (score >= 90 ? 'good' : score >= 70 ? 'mid' : 'bad'), score);
}

// 需要读取响应里 data 之外的字段（total）时用：与 common.js 的 api() 一致，但返回整个响应体。
export async function apiFull(url) {
  let res;
  try {
    res = await fetch(url, { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
  } catch (_) {
    throw new Error(t('无法连接服务器，请检查网络后重试'));
  }
  let payload = null;
  try { payload = await res.json(); } catch (_) { /* 非 JSON */ }
  if (!res.ok) throw new Error((payload && payload.message) || t('请求失败（HTTP {status}）', { status: res.status }));
  return payload || {};
}

export function openModal(id) { $(id).hidden = false; }
export function closeModal(id) { $(id).hidden = true; }

export function bindModalClose() {
  document.querySelectorAll('.modal-mask').forEach((m) => {
    m.addEventListener('click', (e) => { if (e.target === m || e.target.closest('[data-close]')) m.hidden = true; });
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') document.querySelectorAll('.modal-mask').forEach((m) => { m.hidden = true; });
  });
}
