/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: ui.js
 * @time: 2026/10/28
 */
// goInception 配置页的公共小工具。所有往 DOM 写的内容都用 textContent，不拼 innerHTML。
import { t } from '../i18n.js';

export const $ = (id) => document.getElementById(id);

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

export function clear(node) { node.replaceChildren(); return node; }

// 页面顶部的提示条。kind: err / info / warn
export function setMsg(text, kind = 'err') {
  const box = $('gi-msg');
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
  d.scrollIntoView({ block: 'nearest' });
}

const pad = (n) => String(n).padStart(2, '0');
export function fmtTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// ---- 弹窗 ----
const openStack = [];

export function openModal(id) {
  const m = $(id);
  m.hidden = false;
  openStack.push(id);
  const focus = m.querySelector('input:not([type=hidden]), textarea, button.primary, button.danger');
  if (focus) focus.focus();
}

export function closeModal(id) {
  const m = $(id);
  if (!m) return;
  m.hidden = true;
  const i = openStack.lastIndexOf(id);
  if (i >= 0) openStack.splice(i, 1);
}

document.addEventListener('click', (e) => {
  const btn = e.target.closest('[data-close]');
  if (!btn) return;
  const mask = btn.closest('.modal-mask');
  if (mask && !btn.disabled) closeModal(mask.id);
});
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape' || !openStack.length) return;
  const id = openStack[openStack.length - 1];
  if (id === 'm-gi-progress' && $('m-gi-progress-close').disabled) return; // 重启进行中不允许 Esc 关掉
  closeModal(id);
});

// 通用确认弹窗：返回 Promise<boolean>。list 是要列出的条目。同一时刻只会有一个。
let pendingConfirm = null;
export function confirmDialog({ title, text, list = [], okText = t('仍然继续') }) {
  if (pendingConfirm) pendingConfirm(false);
  return new Promise((resolve) => {
    $('m-gi-warn-title').textContent = title;
    $('m-gi-warn-text').textContent = text;
    const ul = clear($('m-gi-warn-list'));
    for (const it of list) ul.append(el('li', '', it));
    const ok = $('m-gi-warn-ok');
    ok.textContent = okText;
    const mask = $('m-gi-warn');
    const finish = (v) => {
      if (pendingConfirm !== finish) return;
      pendingConfirm = null;
      ok.onclick = null;
      mask.removeEventListener('click', onMask);
      closeModal('m-gi-warn');
      resolve(v);
    };
    const onMask = (e) => { if (e.target.closest('[data-close]')) finish(false); };
    pendingConfirm = finish;
    mask.addEventListener('click', onMask);
    ok.onclick = () => finish(true);
    openModal('m-gi-warn');
  });
}

// ---- 渲染 ----

// 把服务端返回的行级对比画进 pre.diff。
export function renderDiff(pre, diff) {
  clear(pre);
  if (!diff || diff.same) {
    pre.append(el('span', 'ctx', t('（没有变化）')));
    return;
  }
  for (const l of diff.lines) {
    const row = el('span', l.op === '+' ? 'add' : l.op === '-' ? 'del' : l.op === '~' ? 'hunk' : 'ctx');
    row.textContent = l.op === '~' ? l.text : l.op + ' ' + l.text;
    pre.append(row);
  }
}

// 校验报告：错误和警告列表；全部通过时给一行绿色提示。
export function renderReport(box, report, extra) {
  clear(box);
  if (!report) return;
  if (!report.issues || report.issues.length === 0) {
    box.append(el('div', 'alert info', t('✔ 语法正确　✔ 参数类型与取值范围合法') + (extra ? '　ℹ ' + extra : '')));
    return;
  }
  const wrap = el('div', 'alert ' + (report.errors ? 'err' : 'warn'));
  wrap.append(el('div', '', report.errors
    ? (report.warnings ? t('✘ 有 {n} 个错误必须修正，{w} 条警告', { n: report.errors, w: report.warnings }) : t('✘ 有 {n} 个错误必须修正', { n: report.errors }))
    : t('⚠ 有 {n} 条警告，请确认', { n: report.warnings })));
  const ul = el('ul');
  for (const i of report.issues) ul.append(el('li', i.level, i.line ? t('第 {line} 行：{msg}', { line: i.line, msg: i.message }) : i.message));
  wrap.append(ul);
  box.append(wrap);
}

export function busy(btn, on) {
  btn.disabled = on;
  if (on) btn.setAttribute('aria-busy', 'true'); else btn.removeAttribute('aria-busy');
}
