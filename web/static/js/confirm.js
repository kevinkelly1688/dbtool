/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: confirm.js
 * @time: 2026/10/28
 */
// 统一确认组件：高危确认 / 差异确认 / 带执行的确认 / 轻提示。
// 约定：所有传入文本一律用 textContent 渲染，不解析 HTML；不使用内联样式 / 内联脚本 / 内联事件。
// 样式由本模块在首次使用时动态插入 <link rel="stylesheet" href="/static/css/confirm.css">。

import { t, tp } from './i18n.js';

const CSS_HREF = '/static/css/confirm.css';
const FOCUSABLE = 'a[href],button:not([disabled]),input:not([disabled]):not([type=hidden]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

// ---- 样式加载 ----
let cssPromise = null;
function ensureCSS() {
  if (cssPromise) return cssPromise;
  const existing = document.querySelector('link[data-cf-css]');
  if (existing && existing.sheet) { cssPromise = Promise.resolve(); return cssPromise; }
  cssPromise = new Promise((resolve) => {
    const link = existing || document.createElement('link');
    let done = false;
    const fin = () => { if (!done) { done = true; resolve(); } };
    link.addEventListener('load', fin);
    link.addEventListener('error', fin); // 样式加载失败也不能让弹窗打不开
    setTimeout(fin, 1500);
    if (!existing) {
      link.rel = 'stylesheet';
      link.href = CSS_HREF;
      link.setAttribute('data-cf-css', '1');
      document.head.appendChild(link);
    }
  });
  return cssPromise;
}

// ---- 按钮元信息（权限码 -> 名称 / 是否高危）----
// 页面可调用 setButtonMeta({ 'sqlworkflow.execute': { name: '立即执行', kind: 'd' } })；
// 也会读全局 BTN（原型同名对象，若存在）。kind === 'd' 或 danger === true 视为高危。
const buttonMeta = new Map();
export function setButtonMeta(map) {
  if (!map) return;
  for (const [k, v] of Object.entries(map)) buttonMeta.set(k, typeof v === 'string' ? { name: v } : v);
}
function metaOf(code) {
  if (!code) return null;
  if (buttonMeta.has(code)) return buttonMeta.get(code);
  const g = globalThis.BTN;
  if (g && typeof g === 'object' && g[code]) return typeof g[code] === 'string' ? { name: g[code] } : g[code];
  return null;
}

// ---- 小工具 ----
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}
const str = (v) => (v === undefined || v === null ? '' : String(v));
const len = (s) => [...s].length; // 按码点计数，和用户眼里的“字数”一致
let seq = 0;

function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(text).then(() => true, () => fallbackCopy(text));
  }
  return Promise.resolve(fallbackCopy(text));
}
function fallbackCopy(text) {
  const ta = el('textarea');
  ta.value = text;
  ta.setAttribute('aria-hidden', 'true');
  ta.className = 'cf-offscreen';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  ta.remove();
  return ok;
}

// ---- 当前弹窗（同时只允许一个）----
let current = null;

function visibleFocusables(root) {
  return [...root.querySelectorAll(FOCUSABLE)].filter((e) => !e.hidden && e.getClientRects().length > 0);
}

function normOpts(o) {
  o = o || {};
  const level = o.level === 'high' ? 'high' : 'normal';
  const lines = Array.isArray(o.lines) ? o.lines : (o.lines ? [o.lines] : []);
  const meta = metaOf(o.code);
  return {
    title: str(o.title) || t('请确认'),
    lines: lines.map(str).filter((l) => l !== ''),
    details: Array.isArray(o.details) ? o.details : [],
    sql: Array.isArray(o.sql) ? o.sql.map(str).join('\n') : str(o.sql),
    code: str(o.code),
    codeName: tp(str(o.codeName) || (meta && meta.name ? String(meta.name) : '')),
    level,
    typed: o.typed === undefined || o.typed === null ? '' : String(o.typed),
    typedLabel: str(o.typedLabel),
    reason: o.reason && typeof o.reason === 'object' ? o.reason : null,
    okText: str(o.okText) || (level === 'high' ? t('确认执行') : t('确认')),
    cancelText: str(o.cancelText) || t('取消'),
    danger: o.danger === undefined ? level === 'high' : !!o.danger,
    focusOk: !!o.focusOk, // 初始焦点落在确认按钮（默认高危落在取消）；有键入框 / 原因框时仍先到输入框
    rows: o.__rows || null,
    high: level === 'high' || !!(meta && (meta.kind === 'd' || meta.danger === true)),
  };
}

// 打开弹窗。runner 为空：点确认即 resolve；否则确认后执行 runner(value)，失败留在弹窗内可重试。
function openDialog(rawOpts, runner) {
  if (typeof document === 'undefined') return Promise.resolve({ ok: false });
  const o = normOpts(rawOpts);
  if (current) current.finish({ ok: false, replaced: true });

  return new Promise((resolve) => {
    const entry = {
      mounted: false, busy: false, closed: false, mask: null, onKey: null, onFocusIn: null, restore: null,
      finish(result) {
        if (entry.closed) return;
        entry.closed = true;
        if (current === entry) current = null;
        if (entry.onKey) document.removeEventListener('keydown', entry.onKey, true);
        if (entry.onFocusIn) document.removeEventListener('focusin', entry.onFocusIn, true);
        if (entry.mounted) {
          entry.mask.remove();
          document.documentElement.classList.remove('cf-lock');
          const back = entry.restore;
          if (back && typeof back.focus === 'function' && document.contains(back)) {
            try { back.focus(); } catch (_) { /* 元素已不可聚焦 */ }
          }
        }
        resolve(result);
      },
    };
    current = entry;
    entry.restore = document.activeElement;

    ensureCSS().then(() => {
      if (entry.closed) return; // 等样式期间已被替换 / 关闭
      mount(entry, o, runner);
    });
  });
}

function mount(entry, o, runner) {
  const id = 'cf-' + (++seq);
  const mask = el('div', 'cf-mask');
  const dlg = el('div', 'cf-dialog' + (o.high ? ' cf-high' : ''));
  dlg.setAttribute('role', 'dialog');
  dlg.setAttribute('aria-modal', 'true');
  dlg.setAttribute('aria-labelledby', id + '-t');
  dlg.setAttribute('aria-describedby', id + '-b');
  mask.append(dlg);

  // 头部
  const head = el('header', 'cf-head');
  const h = el('h2', 'cf-title', o.title);
  h.id = id + '-t';
  head.append(h);
  if (o.high) head.append(el('span', 'cf-badge', t('高危')));
  dlg.append(head);

  // 主体
  const body = el('div', 'cf-body');
  body.id = id + '-b';
  dlg.append(body);
  if (o.lines.length) {
    const box = el('div', 'cf-lines' + (o.high ? ' cf-lines-high' : ''));
    for (const l of o.lines) box.append(el('p', '', l));
    body.append(box);
  }
  const details = o.details.filter((d) => d && d.k !== undefined && d.k !== null);
  if (details.length) {
    const dl = el('dl', 'cf-details');
    for (const d of details) {
      dl.append(el('dt', '', str(d.k)), el('dd', '', str(d.v) === '' ? '—' : str(d.v)));
    }
    body.append(dl);
  }
  if (o.rows) {
    const tbl = el('table', 'cf-diff');
    const cap = el('caption', 'cf-sr', t('变更对比'));
    const thead = el('thead');
    const hr = el('tr');
    for (const x of [t('项目'), t('原值'), t('新值')]) hr.append(el('th', '', x));
    thead.append(hr);
    const tb = el('tbody');
    for (const r of o.rows) {
      const tr = el('tr');
      const from = str(r.from), to = str(r.to);
      tr.append(
        el('th', 'cf-d-label', str(r.label)),
        el('td', 'cf-d-from' + (from === '' ? ' cf-d-empty' : ''), from === '' ? t('（空）') : from),
        el('td', 'cf-d-to' + (to === '' ? ' cf-d-empty' : ''), to === '' ? t('（空）') : to),
      );
      tb.append(tr);
    }
    tbl.append(cap, thead, tb);
    const wrap = el('div', 'cf-diff-wrap');
    wrap.append(tbl);
    body.append(wrap);
  }
  if (o.sql) {
    const w = el('div', 'cf-sqlwrap');
    const bar = el('div', 'cf-sqlbar');
    bar.append(el('span', 'cf-sqllabel', t('将执行：')));
    const cp = el('button', 'cf-btn cf-btn-sm', t('复制'));
    cp.type = 'button';
    cp.addEventListener('click', () => {
      Promise.resolve(copyText(o.sql)).then((okc) => {
        cp.textContent = okc ? t('已复制') : t('复制失败');
        setTimeout(() => { cp.textContent = t('复制'); }, 1500);
      });
    });
    bar.append(cp);
    const pre = el('pre', 'cf-sql', o.sql);
    pre.tabIndex = 0;
    pre.setAttribute('aria-label', t('将执行的 SQL'));
    w.append(bar, pre);
    body.append(w);
  }

  let typedInput = null;
  if (o.typed) {
    const row = el('div', 'cf-field');
    const lab = el('label', 'cf-label');
    lab.htmlFor = id + '-typed';
    lab.append(document.createTextNode(o.typedLabel || t('请原样输入以下文本以确认：')), document.createTextNode(' '));
    lab.append(el('code', 'cf-typed-code', o.typed));
    typedInput = el('input', 'cf-input');
    typedInput.id = id + '-typed';
    typedInput.type = 'text';
    typedInput.autocomplete = 'off';
    typedInput.spellcheck = false;
    typedInput.setAttribute('autocapitalize', 'off');
    row.append(lab, typedInput);
    body.append(row);
  }

  let reasonInput = null, counter = null;
  const rc = o.reason;
  const rMin = rc && Number.isFinite(+rc.min) ? Math.max(0, Math.floor(+rc.min)) : 0;
  const rMax = rc && Number.isFinite(+rc.max) && +rc.max > 0 ? Math.floor(+rc.max) : 0;
  const rReq = !!(rc && rc.required);
  if (rc) {
    const row = el('div', 'cf-field');
    const lab = el('label', 'cf-label' + (rReq ? ' cf-req' : ''), str(rc.label) || t('原因'));
    lab.htmlFor = id + '-reason';
    reasonInput = el('textarea', 'cf-input cf-textarea');
    reasonInput.id = id + '-reason';
    reasonInput.rows = 3;
    reasonInput.placeholder = str(rc.placeholder);
    if (rMax) reasonInput.maxLength = rMax;
    counter = el('div', 'cf-counter');
    counter.setAttribute('aria-live', 'polite');
    row.append(lab, reasonInput, counter);
    body.append(row);
  }

  const err = el('div', 'cf-err');
  err.setAttribute('role', 'alert');
  err.hidden = true;
  body.append(err);

  // 页脚
  const foot = el('footer', 'cf-foot');
  if (o.code) {
    const c = el('span', 'cf-code');
    c.append(document.createTextNode(t('权限码：')), el('code', '', o.code));
    if (o.codeName) c.append(document.createTextNode(' · ' + o.codeName));
    if (o.high) c.append(document.createTextNode(' · '), el('b', 'cf-high-tag', t('高危操作')));
    foot.append(c);
  } else if (o.high) {
    const c = el('span', 'cf-code');
    c.append(el('b', 'cf-high-tag', t('高危操作')));
    foot.append(c);
  }
  const btns = el('div', 'cf-btns');
  const cancel = el('button', 'cf-btn', o.cancelText);
  cancel.type = 'button';
  const ok = el('button', 'cf-btn ' + (o.danger ? 'cf-btn-danger' : 'cf-btn-primary'), o.okText);
  ok.type = 'button';
  btns.append(cancel, ok);
  foot.append(btns);
  dlg.append(foot);

  // 校验
  const reasonValue = () => (reasonInput ? reasonInput.value.trim() : '');
  const reasonOk = () => {
    if (!rc) return true;
    const n = len(reasonValue());
    if (n === 0) return !rReq;
    if (n < rMin) return false;
    if (rMax && n > rMax) return false;
    return true;
  };
  const refresh = () => {
    if (counter) {
      const n = len(reasonValue());
      const cnt = rMax ? t('{n} / {max} 字', { n, max: rMax }) : t('{n} 字', { n });
      if (rMin) counter.textContent = t('{count}，至少 {min} 字', { count: cnt, min: rMin });
      else if (rReq) counter.textContent = t('{count}，必填', { count: cnt });
      else counter.textContent = cnt;
      counter.classList.toggle('cf-bad', !reasonOk() && (n > 0 || rReq));
    }
    const typedOk = !typedInput || typedInput.value.trim() === o.typed;
    ok.disabled = entry.busy || !typedOk || !reasonOk();
  };
  if (typedInput) typedInput.addEventListener('input', refresh);
  if (reasonInput) reasonInput.addEventListener('input', refresh);

  const setBusy = (b) => {
    entry.busy = b;
    ok.classList.toggle('cf-spin', b);
    if (b) ok.setAttribute('aria-busy', 'true'); else ok.removeAttribute('aria-busy');
    cancel.disabled = b;
    if (typedInput) typedInput.disabled = b;
    if (reasonInput) reasonInput.disabled = b;
    refresh();
  };
  const showErr = (msg) => { err.textContent = msg || ''; err.hidden = !msg; };

  const submit = async () => {
    if (entry.closed || entry.busy || ok.disabled) return;
    const value = reasonInput ? reasonValue() : undefined;
    if (!runner) {
      entry.finish(value === undefined ? { ok: true } : { ok: true, value });
      return;
    }
    showErr('');
    setBusy(true);
    let result;
    try {
      result = await runner(value);
    } catch (e) {
      if (entry.closed) { toast((e && e.message) || t('操作失败'), 'err'); return; } // 弹窗已被替换，只能用提示告知
      setBusy(false);
      showErr((e && e.message) || String(e) || t('操作失败'));
      ok.focus();
      return;
    }
    const out = { ok: true, result };
    if (value !== undefined) out.value = value;
    entry.finish(out);
  };
  const cancelIt = () => { if (!entry.busy) entry.finish({ ok: false }); };

  ok.addEventListener('click', submit);
  cancel.addEventListener('click', cancelIt);

  // 遮罩点击：高危不允许；且必须按下和松开都在遮罩上（拖选文字滑出弹窗不误关）
  let downOnMask = false;
  mask.addEventListener('mousedown', (e) => { downOnMask = e.target === mask; });
  mask.addEventListener('click', (e) => {
    if (e.target === mask && downOnMask && !o.high && !o.typed && !rc) cancelIt();
    downOnMask = false;
  });

  entry.onKey = (e) => {
    if (entry.closed) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopImmediatePropagation();
      cancelIt();
      return;
    }
    if (e.key === 'Enter' && !e.isComposing && !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey) {
      const t = e.target;
      const tag = t && t.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'BUTTON' || tag === 'A' || tag === 'SELECT') return;
      e.preventDefault();
      e.stopImmediatePropagation();
      submit();
      return;
    }
    if (e.key === 'Tab') {
      const list = visibleFocusables(dlg);
      if (!list.length) { e.preventDefault(); return; }
      const first = list[0], last = list[list.length - 1];
      const act = document.activeElement;
      if (!dlg.contains(act)) { e.preventDefault(); first.focus(); }
      else if (e.shiftKey && act === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && act === last) { e.preventDefault(); first.focus(); }
    }
  };
  document.addEventListener('keydown', entry.onKey, true);
  // 焦点跑到弹窗外（如屏幕阅读器 / 后台脚本聚焦）时拉回
  entry.onFocusIn = (e) => {
    if (entry.closed || dlg.contains(e.target)) return;
    const l = visibleFocusables(dlg);
    if (l.length) l[0].focus();
  };
  document.addEventListener('focusin', entry.onFocusIn, true);

  entry.mask = mask;
  entry.mounted = true;
  document.body.appendChild(mask);
  document.documentElement.classList.add('cf-lock');
  refresh();
  // 初始焦点：有输入框先到输入框；高危默认落在“取消”，避免回车误确认（调用方传 focusOk 的除外）；否则落在确认
  const first = typedInput || reasonInput || ((o.danger || o.high) && !o.focusOk ? cancel : ok);
  first.focus();
}

// ---- 对外 API ----

// riskConfirm(opts) -> Promise<{ok, value?}>
export function riskConfirm(opts) {
  return openDialog(opts, null);
}

function diffRows(rows) {
  const out = [];
  for (const r of Array.isArray(rows) ? rows : []) {
    if (!r) continue;
    if (str(r.from) !== str(r.to)) out.push({ label: r.label, from: r.from, to: r.to });
  }
  return out;
}

// diffConfirm(opts) -> Promise<{ok, value?, noChange?}>；rows 无变化时不弹窗，返回 {ok:false, noChange:true}
export function diffConfirm(opts) {
  const o = opts || {};
  const rows = diffRows(o.rows);
  if (!rows.length) return Promise.resolve({ ok: false, noChange: true });
  return openDialog({ ...o, __rows: rows }, null);
}

// confirmRun(opts, asyncFn) -> Promise<{ok, value?, result?, noChange?}>
// 确认后调用 asyncFn(value)；执行期间确认按钮转圈禁用，抛错则错误显示在弹窗内（可重试），成功关闭并把返回值放在 result。
// opts 带 rows 时按 diffConfirm 处理（无变化不弹窗不执行）。
export function confirmRun(opts, asyncFn) {
  const o = opts || {};
  if (typeof asyncFn !== 'function') return Promise.reject(new TypeError('confirmRun 需要 asyncFn')); // i18n-ignore 开发期的调用错误，不面向用户
  if (o.rows !== undefined) {
    const rows = diffRows(o.rows);
    if (!rows.length) return Promise.resolve({ ok: false, noChange: true });
    return openDialog({ ...o, __rows: rows }, asyncFn);
  }
  return openDialog(o, asyncFn);
}

// 关闭当前确认弹窗（视为取消）。页面跳转 / 路由切换时可调用。
export function closeConfirm() {
  if (current) current.finish({ ok: false, closed: true });
}

// ---- toast ----
let toastBox = null;
const TOAST_MAX = 4;
export function toast(msg, kind = 'ok') {
  if (typeof document === 'undefined') return;
  if (kind !== 'ok' && kind !== 'err' && kind !== 'warn') kind = 'ok';
  ensureCSS();
  if (!toastBox || !document.body.contains(toastBox)) {
    toastBox = el('div', 'cf-toasts');
    toastBox.setAttribute('role', 'status');
    toastBox.setAttribute('aria-live', 'polite');
    document.body.appendChild(toastBox);
  }
  const t = el('div', 'cf-toast cf-toast-' + kind, str(msg));
  if (kind === 'err') t.setAttribute('role', 'alert');
  const kill = () => { clearTimeout(timer); t.remove(); };
  const timer = setTimeout(kill, kind === 'err' ? 6000 : kind === 'warn' ? 4000 : 2400);
  t.addEventListener('click', kill);
  toastBox.append(t);
  while (toastBox.children.length > TOAST_MAX) toastBox.firstChild.remove();
}
