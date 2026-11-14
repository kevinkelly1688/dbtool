/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: ui.js
 * @time: 2026/10/28
 */
// 数据归档：弹窗、抽屉、提示条、标签、键值表、时间线等通用 UI 部件。
import { t } from '../i18n.js';
import { $, el, add } from './util.js';

// ---- 页面顶部提示 ----
let flashTimer = 0;
export function flash(text, kind = 'info', keep = false) {
  const box = $('ar-msg');
  box.replaceChildren();
  clearTimeout(flashTimer);
  if (!text) return;
  const d = el('div', 'alert ' + (kind === 'ok' ? 'info' : kind), text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
  if (!keep && kind !== 'err') flashTimer = setTimeout(() => box.replaceChildren(), 6000);
}

export function setMsg(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

// ---- 提示条 / 标签 ----
const ICON = { ok: '✓', warn: '!', bad: '✕', info: 'ℹ', '': 'ℹ' };
export function notice(content, kind = '', icon) {
  const d = el('div', 'ar-notice ' + kind);
  d.append(el('i', '', icon || ICON[kind] || 'ℹ'));
  const body = el('div');
  add(body, content);
  d.append(body);
  return d;
}

export function tag(text, kind, title) {
  const t = el('span', 'ar-tag ' + (kind || ''), text);
  if (title) t.title = title;
  return t;
}

// 简短的 <b>文字</b>
export const bold = (text) => el('b', '', text);
export const code = (text) => el('code', '', text);
export const small = (text, cls) => el('span', 'small muted' + (cls ? ' ' + cls : ''), text);

export function sqlBox(text) {
  return el('pre', 'ar-code', text);
}

export function kvList(rows, c2) {
  const dl = el('dl', 'ar-kv' + (c2 ? ' c2' : ''));
  for (const [k, v] of rows) {
    dl.append(el('dt', '', k));
    const dd = el('dd');
    add(dd, v);
    dl.append(dd);
  }
  return dl;
}

export function h4(text, hint) {
  const e = el('h4', 'ar-h4', text);
  if (hint) e.append(el('small', '', hint));
  return e;
}

// 时间线：items = [{cls, h, m}]
export function timeline(items) {
  const ul = el('ul', 'ar-tline');
  for (const it of items) {
    const li = el('li', it.cls || '');
    const head = el('div');
    add(head, it.h);
    li.append(head);
    if (it.m) li.append(el('div', 'm', it.m));
    ul.append(li);
  }
  return ul;
}

export function empty(text) {
  return el('div', 'muted small', text);
}

// 文本差异：每行 "- 旧 / + 新"（简单逐行比对，用于策略保存前的差异预览）
export function diffBox(oldText, newText) {
  const a = String(oldText).split('\n');
  const b = String(newText).split('\n');
  const box = el('div', 'ar-diff');
  const n = Math.max(a.length, b.length);
  for (let i = 0; i < n; i++) {
    if (a[i] === b[i]) continue;
    if (a[i] !== undefined) box.append(el('span', 'del', '- ' + a[i]));
    if (b[i] !== undefined) box.append(el('span', 'add', '+ ' + b[i]));
  }
  if (!box.childNodes.length) box.append(el('span', '', t('（无变化）')));
  return box;
}

// ---- 弹窗 ----
let modalState = null;

export function closeModal() {
  $('ar-modal-mask').hidden = true;
  $('ar-modal').className = 'modal';
  $('ar-modal-body').replaceChildren();
  $('ar-modal-msg').replaceChildren();
  modalState = null;
}

// openModal({title, body(Node), ok, okCls, cancel, size:'wide'|'xwide', onOk: async () => (false 表示保持打开)})
export function openModal(opt) {
  const mask = $('ar-modal-mask');
  $('ar-modal-title').textContent = opt.title || '';
  $('ar-modal-title').className = opt.crit ? 'crit' : '';
  $('ar-modal').className = 'modal' + (opt.size ? ' ' + opt.size : '');
  $('ar-modal-msg').replaceChildren();
  const body = $('ar-modal-body');
  body.replaceChildren();
  if (opt.body) add(body, opt.body);
  const ok = $('ar-modal-ok');
  const cancel = $('ar-modal-cancel');
  ok.hidden = opt.ok === '';
  ok.textContent = opt.ok || t('确定');
  ok.className = 'btn ' + (opt.okCls || 'primary');
  ok.disabled = false;
  ok.removeAttribute('aria-busy');
  cancel.textContent = opt.cancel || t('取消');
  modalState = { onOk: opt.onOk, onCancel: opt.onCancel };
  mask.hidden = false;
  const first = body.querySelector('input:not([type=checkbox]):not([type=radio]),textarea,select');
  if (first) first.focus();
  else ok.focus();
}

export function modalMsg(text, kind = 'err') {
  setMsg($('ar-modal-msg'), text, kind);
}

async function modalOk() {
  if (!modalState || !modalState.onOk) return closeModal();
  const ok = $('ar-modal-ok');
  ok.disabled = true;
  ok.setAttribute('aria-busy', 'true');
  try {
    const r = await modalState.onOk();
    if (r !== false) closeModal();
  } catch (e) {
    modalMsg(e.message || t('操作失败'));
  } finally {
    ok.disabled = false;
    ok.removeAttribute('aria-busy');
  }
}

// 高危操作确认：可要求输入名称（nameToType）。onOk 抛错则显示在弹窗里并保持打开。
export function riskConfirm({ title, body, ok, nameToType, sql, onOk }) {
  const wrap = el('div');
  add(wrap, body);
  if (sql) wrap.append(sqlBox(sql));
  let input = null;
  if (nameToType) {
    const lab = el('label', 'fld ar-confirm-name');
    const [pre, post] = t('请输入 {name} 以确认', { name: '\u0001' }).split('\u0001'); // 名称是 <code> 节点，按译文语序拆成前后两段
    lab.append(document.createTextNode(pre), el('code', 'crit', nameToType), document.createTextNode(post));
    input = el('input');
    input.type = 'text';
    input.autocomplete = 'off';
    input.setAttribute('aria-label', t('输入名称确认'));
    lab.append(input);
    wrap.append(lab);
  }
  openModal({
    title, body: wrap, ok: ok || t('确认'), okCls: 'danger', crit: true,
    onOk: async () => {
      if (input && input.value.trim() !== nameToType) {
        modalMsg(t('输入的名称与「{name}」不一致', { name: nameToType }));
        return false;
      }
      return onOk();
    },
  });
}

// ---- 抽屉 ----
let drawerOnClose = null;

export function openDrawer(title, body, onClose) {
  $('ar-drawer-title').textContent = title;
  const b = $('ar-drawer-body');
  b.replaceChildren();
  add(b, body);
  $('ar-drawer-mask').hidden = false;
  drawerOnClose = onClose || null;
}

export function setDrawerBody(body, title) {
  if (title) $('ar-drawer-title').textContent = title;
  const b = $('ar-drawer-body');
  const top = b.scrollTop;
  b.replaceChildren();
  add(b, body);
  b.scrollTop = top;
}

export function drawerOpen() {
  return !$('ar-drawer-mask').hidden;
}

export function closeDrawer() {
  $('ar-drawer-mask').hidden = true;
  $('ar-drawer-body').replaceChildren();
  const f = drawerOnClose;
  drawerOnClose = null;
  if (f) f();
}

export function initUi() {
  $('ar-modal-ok').addEventListener('click', modalOk);
  $('ar-modal-cancel').addEventListener('click', () => {
    const f = modalState && modalState.onCancel;
    closeModal();
    if (f) f();
  });
  $('ar-modal-mask').addEventListener('mousedown', (e) => { if (e.target === $('ar-modal-mask')) $('ar-modal-cancel').click(); });
  $('ar-drawer-close').addEventListener('click', closeDrawer);
  $('ar-drawer-mask').addEventListener('mousedown', (e) => { if (e.target === $('ar-drawer-mask')) closeDrawer(); });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (!$('ar-modal-mask').hidden) $('ar-modal-cancel').click();
    else if (!$('ar-drawer-mask').hidden) closeDrawer();
  });
}
