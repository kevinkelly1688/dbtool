/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: ui.js
 * @time: 2026/10/28
 */
// 在线查询页几个小组件共用的 DOM 小工具。文本一律用 textContent，不解析 HTML。
const NS = 'http://www.w3.org/2000/svg';

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// /static/img/icons.svg 里的图标（<svg class="ico"><use href=…#id>）。
export function icon(id) {
  const s = document.createElementNS(NS, 'svg');
  s.setAttribute('class', 'ico');
  s.setAttribute('aria-hidden', 'true');
  const u = document.createElementNS(NS, 'use');
  u.setAttribute('href', `/static/img/icons.svg#${id}`);
  s.append(u);
  return s;
}

export function iconButton(cls, title, iconId) {
  const b = el('button', cls);
  b.type = 'button';
  b.title = title;
  b.setAttribute('aria-label', title);
  b.append(icon(iconId));
  return b;
}

export const nf = (n) => Number(n).toLocaleString('en-US');
