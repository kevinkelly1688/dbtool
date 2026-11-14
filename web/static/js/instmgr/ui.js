/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: ui.js
 * @time: 2026/10/28
 */
// 实例管理各页面共用的小工具：DOM 构造、提示条、弹窗、二次确认、页签、实例下拉、分页。
// 往页面写内容一律用 textContent / createElement，不拼 innerHTML。
import { api } from '../common.js';
import { t, tp } from '../i18n.js';

export { api };
export const $ = (id) => document.getElementById(id);

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

export function btn(text, cls, onClick) {
  const b = el('button', 'btn' + (cls ? ' ' + cls : ''), text);
  b.type = 'button';
  if (onClick) b.addEventListener('click', onClick);
  return b;
}

// 提示条：box 是容器元素。kind 取 err / info / warn。
export function setMsg(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

export const qs = (params) => {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== '') u.set(k, String(v));
  const s = u.toString();
  return s ? '?' + s : '';
};

export function emptyRow(tbody, cols, text) {
  tbody.replaceChildren();
  const tr = el('tr');
  const td = el('td', 'empty', text);
  td.colSpan = cols;
  tr.append(td);
  tbody.append(tr);
}

export function cell(text, cls) {
  return el('td', cls || '', text === null || text === undefined ? '' : String(text));
}

export function fmtNum(n) {
  return Number(n || 0).toLocaleString('en-US');
}

export function fmtBytes(n) {
  let v = Number(n || 0);
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + u[i];
}

// 带“展开/收起”的长文本单元格（SQL 等）。
export function longCell(text, limit = 120) {
  const td = el('td', 'mono wrap');
  text = text || '';
  if (text.length <= limit) { td.textContent = text; return td; }
  const span = el('span', '', text.slice(0, limit) + '…');
  const a = el('a', '', ' ' + t('展开'));
  a.href = '#';
  let open = false;
  a.addEventListener('click', (e) => {
    e.preventDefault();
    open = !open;
    span.textContent = open ? text : text.slice(0, limit) + '…';
    a.textContent = ' ' + (open ? t('收起') : t('展开'));
  });
  td.append(span, a);
  return td;
}

// ---- 弹窗 ----
const focusBack = new WeakMap();

export function openModal(id) {
  const m = typeof id === 'string' ? $(id) : id;
  focusBack.set(m, document.activeElement);
  m.hidden = false;
  const first = m.querySelector('input:not([readonly]):not([type=checkbox]), select, textarea, button');
  if (first) first.focus();
}

export function closeModal(m) {
  if (typeof m === 'string') m = $(m);
  if (!m || m.hidden) return;
  m.hidden = true;
  const back = focusBack.get(m);
  if (back && back.focus) back.focus();
}

document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-close]');
  if (b) closeModal(b.closest('.modal-mask'));
});
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  const open = [...document.querySelectorAll('.modal-mask')].filter((m) => !m.hidden);
  if (open.length) closeModal(open[open.length - 1]);
});

// 二次确认：返回 Promise<boolean>。lines 是说明段落，sql 是将要执行的语句（可选）；
// typeText 非空时要求用户输入该文本才能点确定。
export function confirmBox({ title, lines = [], sql = [], okText = t('确定'), danger = true, typeText = '', noOk = false }) {
  return new Promise((resolve) => {
    const mask = el('div', 'modal-mask');
    const box = el('div', 'modal');
    box.setAttribute('role', 'dialog');
    box.setAttribute('aria-modal', 'true');
    const h = el('h2', danger ? 'crit' : '', title);
    h.id = 'cf-title-' + Math.random().toString(36).slice(2);
    box.setAttribute('aria-labelledby', h.id);
    box.append(h);
    for (const l of lines) box.append(el('p', '', l));
    if (sql.length) {
      box.append(el('p', 'muted small', t('将执行的语句：')));
      box.append(el('pre', 'sqlbox mono', sql.join('\n')));
    }
    let input = null;
    if (typeText) {
      // 要求输入的文字用红色加粗显示，避免把“确认”之类的提示词当成要输入的内容
      // 整句翻译：占位符的位置放要输入的文字，前后各一段普通文本
      const parts = t('请输入 {text} 以确认：', { text: '\u0000' }).split('\u0000');
      const lb = el('label', 'muted small', parts[0]);
      lb.append(el('b', 'crit mono', typeText), document.createTextNode(parts[1] || ''));
      input = el('input');
      input.type = 'text';
      input.autocomplete = 'off';
      input.style.display = 'block';
      input.style.width = '100%';
      input.style.marginTop = '4px';
      lb.append(input);
      box.append(lb);
    }
    const actions = el('div', 'actions');
    actions.style.display = 'flex';
    actions.style.gap = '8px';
    actions.style.marginTop = '12px';
    actions.append(el('span', 'spacer'));
    const done = (v) => { document.removeEventListener('keydown', onKey); mask.remove(); resolve(v); };
    const onKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); done(false); } };
    document.addEventListener('keydown', onKey);
    const cancel = btn(noOk ? t('关闭') : t('取消'), '', () => done(false));
    actions.append(cancel);
    let ok = null;
    if (!noOk) {
      ok = btn(okText, danger ? 'danger' : 'primary', () => done(true));
      if (input) {
        ok.disabled = true;
        input.addEventListener('input', () => { ok.disabled = input.value.trim() !== typeText; });
      }
      actions.append(ok);
    }
    box.append(actions);
    mask.append(box);
    document.body.append(mask);
    (input || cancel).focus();
  });
}

// ---- 页签 ----
export function initTabs(root, onSwitch) {
  const tabs = [...root.querySelectorAll('[data-tab]')];
  const panes = [...root.querySelectorAll('[data-pane]')];
  const show = (name) => {
    tabs.forEach((tb) => tb.classList.toggle('on', tb.dataset.tab === name));
    panes.forEach((p) => { p.hidden = p.dataset.pane !== name; });
    if (onSwitch) onSwitch(name);
  };
  tabs.forEach((tb) => tb.addEventListener('click', () => show(tb.dataset.tab)));
  return show;
}

// ---- 权限与实例下拉 ----
let capsCache = null;
export async function loadCaps() {
  if (!capsCache) capsCache = await api('GET', '/api/v1/instmgr/caps');
  return capsCache;
}

// 按钮显隐：caps 以按钮码（登记表 internal/btnperm）为键，值是当前用户是否拥有该按钮，与服务端校验一致。
export function can(caps, button) {
  return caps[button] === true;
}

const KEY = 'instmgr.instance';
function remembered() {
  try { return localStorage.getItem(KEY) || ''; } catch (_) { return ''; }
}
function remember(v) {
  try { localStorage.setItem(KEY, v); } catch (_) { /* 无痕模式等：忽略 */ }
}

// 填充实例下拉：feature 取 sessions / databases / accounts / params。优先 ?instance=ID，其次上次选择。
// 不支持该功能的实例也放进列表（标注“暂不支持”），选中后由页面提示。
export async function loadInstanceSelect(select, feature) {
  const list = await api('GET', '/api/v1/instmgr/choices' + qs({ feature }));
  select.replaceChildren();
  if (!list.length) {
    select.append(new Option(t('（没有可用的实例）'), ''));
    return list;
  }
  for (const it of list) {
    const p = { name: it.instance_name, type: tp(it.db_type_label) };
    const o = new Option(it.supported ? t('{name}（{type}）', p) : t('{name}（{type}） · 暂不支持', p), String(it.id));
    select.append(o);
  }
  const want = new URLSearchParams(location.search).get('instance') || remembered();
  if (want && list.some((i) => String(i.id) === want)) select.value = want;
  select.addEventListener('change', () => remember(select.value));
  return list;
}

// ---- 分页 ----
export function renderPager(box, { page, size, total }, onGo) {
  box.replaceChildren();
  const pages = Math.max(1, Math.ceil(total / size));
  box.append(el('span', '', t('共 {total} 条，第 {page} / {pages} 页', { total, page, pages })), el('span', 'spacer'));
  const prev = btn(t('上一页'), '', () => onGo(page - 1));
  const next = btn(t('下一页'), '', () => onGo(page + 1));
  prev.disabled = page <= 1;
  next.disabled = page >= pages;
  box.append(prev, next);
}

export function debounce(fn, ms = 300) {
  let timer = null;
  return (...a) => { clearTimeout(timer); timer = setTimeout(() => fn(...a), ms); };
}

// 请求期间禁用按钮并标记忙碌。
export async function busy(button, fn) {
  button.disabled = true;
  button.setAttribute('aria-busy', 'true');
  try { return await fn(); } finally { button.disabled = false; button.removeAttribute('aria-busy'); }
}
