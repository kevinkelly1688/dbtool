/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: util.js
 * @time: 2026/10/28
 */
// 数据归档页面共用的小工具：DOM 构造、格式化。
// 所有来自服务端 / 数据库的字符串一律用 textContent 写入，不拼 innerHTML。
import { fmtNum, isZh } from '../i18n.js';

// 句中的冒号：中文用全角“：”，其它语言用半角“: ”。
export const colon = isZh ? '：' : ': ';

export const $ = (id) => document.getElementById(id);

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = String(text);
  return e;
}

// 追加子节点：字符串变文本节点，null / false 忽略。
export function add(parent, ...kids) {
  for (const k of kids.flat(Infinity)) {
    if (k === null || k === undefined || k === false) continue;
    parent.append(typeof k === 'string' || typeof k === 'number' ? document.createTextNode(String(k)) : k);
  }
  return parent;
}

export function h(tag, cls, ...kids) {
  return add(el(tag, cls), kids);
}

// 译文里的 {名字} 占位换成节点（句子中间嵌 <b> / <code> 的场景）：返回数组，直接交给 add() / notice()。
// 用法：先用 t 取带 {now} {table} 等占位的整句译文，再把这些占位换成 bold / code 节点，占位名与译文里的一致。
export function withNodes(text, nodes) {
  return text.split(/(\{\w+\})/).map((s) => {
    const m = /^\{(\w+)\}$/.exec(s);
    return m && Object.prototype.hasOwnProperty.call(nodes, m[1]) ? nodes[m[1]] : s;
  }).filter((x) => x !== '' && x !== null && x !== undefined);
}

export function button(text, cls, fn, title) {
  const b = el('button', 'btn ' + (cls || ''), text);
  b.type = 'button';
  if (title) b.title = title;
  if (fn) b.addEventListener('click', fn);
  return b;
}

// 带权限码的按钮：data-btn 交给 btnperm.js（有权限 / 置灰 / 隐藏）处理。
export function pbutton(code, text, cls, fn, title) {
  const b = button(text, cls, fn, title);
  b.dataset.btn = code;
  return b;
}

export function link(text, fn, title) {
  const b = el('button', 'ar-link', text);
  b.type = 'button';
  if (title) b.title = title;
  b.addEventListener('click', fn);
  return b;
}

export function td(content, cls) {
  const c = el('td', cls || '');
  if (content !== undefined && content !== null) add(c, content);
  return c;
}

export function fillSelect(sel, items, textOf, valueOf, first, current) {
  sel.replaceChildren();
  if (first) sel.append(new Option(first[1], first[0]));
  for (const it of items) sel.append(new Option(textOf(it), valueOf(it)));
  if (current !== undefined && current !== null) sel.value = String(current);
}

// ---- 格式化 ----

export const nf = (n) => (n === undefined || n === null || Number.isNaN(Number(n)) ? '—' : fmtNum(n));

export function fmtBytes(n) {
  n = Number(n) || 0;
  if (n >= 1099511627776) return (n / 1099511627776).toFixed(2) + ' TB';
  if (n >= 1073741824) return (n / 1073741824).toFixed(2) + ' GB';
  if (n >= 1048576) return (n / 1048576).toFixed(1) + ' MB';
  if (n >= 1024) return (n / 1024).toFixed(1) + ' KB';
  return Math.round(n) + ' B';
}

const pad2 = (n) => String(n).padStart(2, '0');

export function fmtDur(ms) {
  const s = Math.round((Number(ms) || 0) / 1000);
  if (s < 60) return s + ' s';
  const m = Math.floor(s / 60);
  if (m < 60) return m + ' min ' + pad2(s % 60) + ' s';
  return Math.floor(m / 60) + ' h ' + pad2(m % 60) + ' min';
}

// 服务端时间统一按 "YYYY-MM-DD HH:MM:SS" 或 RFC3339 返回；页面只做截断显示。
export function fmtTime(v, len) {
  if (!v) return '—';
  let s = String(v);
  if (/^\d{4}-\d{2}-\d{2}T/.test(s)) {
    const d = new Date(s);
    if (!Number.isNaN(d.getTime())) s = `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
  }
  return len ? s.slice(0, len) : s;
}

export const clip = (s, n) => (String(s || '').length > n ? String(s).slice(0, n) + '…' : String(s || ''));

export const bq = (s) => '`' + s + '`';

// 解析 "YYYY-MM-DD HH:MM[:SS]" 或 RFC3339 为毫秒时间戳，失败返回 NaN。
export function parseTime(v) {
  if (!v) return NaN;
  return Date.parse(String(v).replace(' ', 'T'));
}

export function clone(o) {
  return JSON.parse(JSON.stringify(o));
}

export function debounce(fn, ms) {
  let t = 0;
  return (...a) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...a), ms);
  };
}

// 点路径读写：'p.batch_size'
export function getPath(o, path) {
  return path.split('.').reduce((a, k) => (a === undefined || a === null ? undefined : a[k]), o);
}
export function setPath(o, path, v) {
  const ks = path.split('.');
  const last = ks.pop();
  const tgt = ks.reduce((a, k) => a[k], o);
  tgt[last] = v;
}

// 窄屏卡片列表：把表头文字写进每个单元格的 data-label（CSS 用 attr() 显示成“标签：值”）。
// 宽屏下 data-label 不显示，对表格没有任何影响。
export function labelCells(table) {
  const heads = [...table.querySelectorAll('thead th')].map((t) => t.textContent.trim());
  for (const tr of table.querySelectorAll('tbody tr')) {
    const cs = tr.children;
    if (cs.length !== heads.length) continue;
    for (let i = 0; i < cs.length; i += 1) if (heads[i]) cs[i].dataset.label = heads[i];
  }
}

// 页面内导航（hash 路由）与“数据已变化”广播：列表在收到后重新加载。
export const go = (hash) => { location.hash = hash; };
export const notifyChanged = () => document.dispatchEvent(new CustomEvent('archive:changed'));
