/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: searchselect.js
 * @time: 2026/10/28
 */
// 实例下拉的“输入即筛选”：把单选的实例 <select> 升级成可输入的组合框，用户键入关键字做模糊匹配并定位。
//
// - 原来的 <select> 保留在 DOM 里（隐藏），仍是取值 / 表单提交 / 事件的唯一来源：选中后写回它的 value 并触发 change；
//   页面脚本照旧读 select.value、往里加 option、设 select.value / disabled / hidden，这里会同步显示。
// - 识别规则（只升级单选）：带 data-fuzzy；aria-label 是“实例 / 目标实例 / 按实例过滤”；id 或 name 以 inst / instance 结尾。
//   不想升级的加 data-nofuzzy。选项再多也只渲染前 200 项，继续输入缩小范围。
// - 匹配：不区分大小写；空格分隔的每个词都要出现在选项文字里（子串）；没有子串命中时退到“按顺序出现的字符”（子序列）。
// - 键盘：↑ ↓ 移动、Enter 选中、Esc 放弃并还原、Tab 离开。所有文字都用 textContent 写入。

import { fuzzyFilter } from './fuzzy.js';
import { t } from './i18n.js';

const MAX_RENDER = 200;
const LABEL_RE = /^(实例|目标实例|按实例过滤)$/;
// 界面语言不是中文时 aria-label 已被翻译，识别规则要同时认译文
const LABELS = new Set([t('实例'), t('目标实例'), t('按实例过滤')]);
const ID_RE = /(^|[-_.])inst(ance)?$/i;

function isInstanceSelect(s) {
  if (s.multiple || s.size > 1 || s.hasAttribute('data-nofuzzy')) return false;
  if (s.hasAttribute('data-fuzzy')) return true;
  const label = (s.getAttribute('aria-label') || '').trim();
  if (LABEL_RE.test(label) || LABELS.has(label)) return true;
  return ID_RE.test(s.id || '') || ID_RE.test(s.getAttribute('name') || '');
}

let uid = 0;
const registry = new Set(); // 已升级的（wrap, list）；wrap 从页面上移除后，把挂在 body 上的 list 一起清掉

function enhance(sel) {
  if (sel.dataset.ssDone) return;
  sel.dataset.ssDone = '1';
  const id = 'ss' + (++uid);

  const wrap = document.createElement('div');
  wrap.className = 'ss';
  const input = document.createElement('input');
  input.type = 'text';
  input.className = 'ss-input';
  input.setAttribute('role', 'combobox');
  input.setAttribute('aria-autocomplete', 'list');
  input.setAttribute('aria-expanded', 'false');
  input.setAttribute('aria-controls', id + '-list');
  input.setAttribute('aria-label', sel.getAttribute('aria-label') || t('实例'));
  input.autocomplete = 'off';
  input.spellcheck = false;
  const list = document.createElement('ul');
  list.className = 'ss-list';
  list.id = id + '-list';
  list.setAttribute('role', 'listbox');
  list.hidden = true;
  wrap.append(input);
  // 列表挂在 body 上、position: fixed：卡片 / 弹窗的 overflow 裁剪不到它，也不会被相邻元素挡住
  document.body.append(list);
  registry.add({ wrap, list });
  // 保持原来下拉的宽度（可见时才量得到）；窄屏由 max-width: 100% 兜底
  const w = sel.getBoundingClientRect().width;
  if (w > 0) wrap.style.minWidth = Math.round(w) + 'px';
  // 放在 select 之前：<label> 里第一个可聚焦控件就是输入框，点 label 文字会聚焦到它
  sel.before(wrap);
  sel.classList.add('ss-native');
  sel.tabIndex = -1;

  let shown = []; // 当前列表对应的 option 下标
  let active = -1;
  let open = false;

  const options = () => Array.from(sel.options);
  const selectedText = () => { const o = sel.options[sel.selectedIndex]; return o ? o.textContent : ''; };

  function syncDisplay() {
    if (!open) input.value = selectedText();
    input.disabled = sel.disabled;
    wrap.hidden = sel.hidden;
    input.placeholder = sel.options.length > 8 ? t('输入关键字筛选（共 {n} 项）', { n: sel.options.length }) : '';
  }

  function render() {
    const opts = options();
    const texts = opts.map((o) => o.textContent);
    // 刚打开（还没键入）时显示全部；键入之后按输入框里的内容筛
    shown = fuzzyFilter(texts, input.dataset.typed ? input.value : '').filter((i) => !opts[i].disabled);
    list.replaceChildren();
    if (!shown.length) {
      const li = document.createElement('li');
      li.className = 'ss-empty';
      li.textContent = t('没有匹配的选项');
      list.append(li);
      active = -1;
      return;
    }
    const total = shown.length;
    const cur = sel.selectedIndex;
    shown.slice(0, MAX_RENDER).forEach((oi, n) => {
      const li = document.createElement('li');
      li.className = 'ss-opt' + (oi === cur ? ' sel' : '');
      li.id = `${id}-o${n}`;
      li.setAttribute('role', 'option');
      li.dataset.i = String(oi);
      li.textContent = opts[oi].textContent;
      li.title = opts[oi].textContent; // 名字长到被省略号截断时，悬停看全名
      list.append(li);
    });
    if (total > MAX_RENDER) {
      const li = document.createElement('li');
      li.className = 'ss-more';
      li.textContent = t('还有 {n} 项，继续输入缩小范围', { n: total - MAX_RENDER });
      list.append(li);
    }
    const curPos = shown.indexOf(cur);
    setActive(curPos >= 0 && curPos < MAX_RENDER ? curPos : 0, false);
  }

  function setActive(n, scroll = true) {
    const items = list.querySelectorAll('.ss-opt');
    items.forEach((li, i) => li.classList.toggle('on', i === n));
    active = n;
    if (items[n]) {
      input.setAttribute('aria-activedescendant', items[n].id);
      if (scroll) items[n].scrollIntoView({ block: 'nearest' });
    } else {
      input.removeAttribute('aria-activedescendant');
    }
  }

  // 按输入框的位置摆放列表：优先放下面，下面放不下（空间小于上面）就翻到上面
  function place() {
    const r = input.getBoundingClientRect();
    const vh = window.innerHeight;
    const below = vh - r.bottom - 8;
    const above = r.top - 8;
    const st = list.style;
    st.minWidth = Math.round(r.width) + 'px';
    // 选项比输入框宽、输入框又靠近窗口右侧时，列表会被窗口边缘切掉后半截名字：往左挪，保证整条名字看得到
    const lw = list.offsetWidth;
    st.left = Math.max(4, Math.min(Math.round(r.left), Math.round(window.innerWidth - lw - 8))) + 'px';
    if (below >= 180 || below >= above) {
      st.top = Math.round(r.bottom + 2) + 'px';
      st.bottom = 'auto';
      st.maxHeight = Math.max(120, Math.min(300, below)) + 'px';
    } else {
      st.top = 'auto';
      st.bottom = Math.round(vh - r.top + 2) + 'px';
      st.maxHeight = Math.max(120, Math.min(300, above)) + 'px';
    }
  }

  function openList() {
    if (open || input.disabled) return;
    open = true;
    delete input.dataset.typed;
    list.hidden = false;
    input.setAttribute('aria-expanded', 'true');
    render();
    place();
    window.addEventListener('scroll', place, true);
    window.addEventListener('resize', place);
    input.select();
  }

  function closeList(restore = true) {
    if (!open) return;
    open = false;
    list.hidden = true;
    window.removeEventListener('scroll', place, true);
    window.removeEventListener('resize', place);
    input.setAttribute('aria-expanded', 'false');
    delete input.dataset.typed;
    if (restore) input.value = selectedText();
  }

  function choose(oi) {
    const changed = sel.selectedIndex !== oi;
    sel.selectedIndex = oi;
    closeList(false);
    input.value = selectedText();
    if (changed) {
      sel.dispatchEvent(new Event('input', { bubbles: true }));
      sel.dispatchEvent(new Event('change', { bubbles: true }));
    }
  }

  input.addEventListener('focus', openList);
  input.addEventListener('click', openList);
  input.addEventListener('input', () => {
    if (!open) openList();
    input.dataset.typed = '1';
    render();
    place();
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (!open) { openList(); return; }
      const n = list.querySelectorAll('.ss-opt').length;
      if (!n) return;
      setActive(e.key === 'ArrowDown' ? (active + 1) % n : (active - 1 + n) % n);
    } else if (e.key === 'Enter') {
      if (open) {
        e.preventDefault();
        const li = list.querySelectorAll('.ss-opt')[active];
        if (li) choose(Number(li.dataset.i));
      }
    } else if (e.key === 'Escape') {
      if (open) { e.preventDefault(); closeList(true); }
    } else if (e.key === 'Tab') {
      closeList(true);
    }
  });
  // mousedown 先于 blur：用它选中，避免输入框先失焦把列表收掉
  list.addEventListener('mousedown', (e) => {
    const li = e.target.closest('.ss-opt');
    if (!li) return;
    e.preventDefault();
    choose(Number(li.dataset.i));
  });
  input.addEventListener('blur', () => closeList(true));

  // 与原 select 保持同步：选项增减、disabled / hidden、脚本里直接赋 value / selectedIndex
  new MutationObserver(() => { syncDisplay(); if (open) render(); }).observe(sel, { childList: true, subtree: true, attributes: true, attributeFilter: ['disabled', 'hidden'] });
  sel.addEventListener('change', syncDisplay);
  for (const prop of ['value', 'selectedIndex']) {
    const d = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, prop);
    if (!d || !d.set) continue;
    Object.defineProperty(sel, prop, {
      configurable: true,
      get() { return d.get.call(this); },
      set(v) { d.set.call(this, v); syncDisplay(); },
    });
  }
  syncDisplay();
}

function scan(root) {
  if (root.nodeType !== 1) return;
  if (root.tagName === 'SELECT' && isInstanceSelect(root)) enhance(root);
  root.querySelectorAll && root.querySelectorAll('select').forEach((s) => { if (isInstanceSelect(s)) enhance(s); });
}

function start() {
  scan(document.body);
  new MutationObserver((muts) => {
    for (const m of muts) m.addedNodes.forEach(scan);
    for (const r of registry) {
      if (!r.wrap.isConnected) { r.list.remove(); registry.delete(r); }
    }
  }).observe(document.body, { childList: true, subtree: true });
}

if (document.body) start();
else document.addEventListener('DOMContentLoaded', start);
