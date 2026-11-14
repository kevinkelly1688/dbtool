/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: ui.js
 * @time: 2026/10/28
 */
// 系统管理各页面共用的小工具：DOM 构造、消息条、弹窗、页签、分页。
// 往页面写内容一律用 textContent / createElement，不拼 innerHTML。
import { api, riskConfirm, diffConfirm, confirmRun, toast, setButtonMeta } from '../common.js';
import { t } from '../i18n.js';

export { api, riskConfirm, diffConfirm, confirmRun, toast };

// 系统管理各页确认弹窗页脚要显示的权限码名称（与 internal/btnperm/registry.go 的“系统管理”模块一致；kind:'d' = 高危）
setButtonMeta({
  'flow.edit': { name: t('变更审批流'), kind: 'd' },
  'rg.edit': { name: t('新建 / 编辑资源组与通知群') },
  'rg.relations': { name: t('调整成员与实例') },
  'rg.delete': { name: t('删除 / 恢复资源组'), kind: 'd' },
  'user.edit': { name: t('新建 / 编辑用户') },
  'user.unlock': { name: t('解锁账号') },
  'user.reset': { name: t('重置密码 / 重置 2FA'), kind: 'd' },
  'user.disable': { name: t('停用 / 启用'), kind: 'd' },
  'user.grant_super': { name: t('授予超级用户'), kind: 'd' },
  'ag.edit': { name: t('新建 / 编辑权限与成员'), kind: 'd' },
  'ag.delete': { name: t('删除权限组'), kind: 'd' },
  'ag.btnperm': { name: t('按钮权限与显示方式'), kind: 'd' },
  'audit.export': { name: t('导出审计日志'), kind: 'd' },
});

export const $ = (id) => document.getElementById(id);

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

export function button(text, cls, onClick) {
  const b = el('button', 'btn' + (cls ? ' ' + cls : ''), text);
  b.type = 'button';
  if (onClick) b.addEventListener('click', onClick);
  return b;
}

// 消息条：返回 set(text, kind)，kind 为 err / info / warn；text 为空则清除。
export function msgBox(box) {
  return (text, kind = 'err') => {
    box.replaceChildren();
    if (!text) return;
    const d = el('div', 'alert ' + kind, text);
    d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
    box.append(d);
  };
}

// 执行异步操作期间禁用按钮，防止重复提交。
export async function busy(btn, fn) {
  if (btn) { btn.disabled = true; btn.setAttribute('aria-busy', 'true'); }
  try { return await fn(); } finally { if (btn) { btn.disabled = false; btn.removeAttribute('aria-busy'); } }
}

export function fillSelect(sel, items, textOf, valueOf) {
  sel.replaceChildren();
  for (const it of items) sel.append(new Option(textOf(it), String(valueOf(it))));
}

// 把对象里的非空值拼成查询串（含前导 ?）。
export function qs(params) {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== '' && v !== null && v !== undefined) u.set(k, String(v));
  const s = u.toString();
  return s ? '?' + s : '';
}

// 页签：root 内 [data-tab] 按钮切换 [data-pane] 区块。onChange(name) 在切换后回调。
export function tabs(root, onChange) {
  const btns = [...root.querySelectorAll('[data-tab]')];
  const panes = [...root.querySelectorAll('[data-pane]')];
  const show = (name) => {
    for (const b of btns) b.classList.toggle('on', b.dataset.tab === name);
    for (const p of panes) p.hidden = p.dataset.pane !== name;
    if (onChange) onChange(name);
  };
  for (const b of btns) if (!b.disabled) b.addEventListener('click', () => show(b.dataset.tab));
  return show;
}

// 弹窗：依赖 admin.css 的 .modal-mask / .modal。点遮罩或按 Esc 关闭。
// opts.beforeClose：关闭前的确认回调，返回 false（或 resolve 为 false 的 Promise）则不关闭（用于有暂存的未保存变更）；opts.onClose：关闭之后的回调。
export function modal(id, opts = {}) {
  const m = $(id);
  let closing = false; // beforeClose 是异步确认（会弹确认框）时，等它结束前不重复触发
  const finish = (wasOpen) => {
    m.hidden = true;
    if (wasOpen && opts.onClose) opts.onClose();
  };
  const close = () => {
    if (closing) return;
    const wasOpen = !m.hidden;
    const r = opts.beforeClose ? opts.beforeClose() : true;
    if (r && typeof r.then === 'function') {
      closing = true;
      r.then((ok) => { if (ok !== false) finish(wasOpen); }, () => {}).finally(() => { closing = false; });
      return;
    }
    if (r === false) return;
    finish(wasOpen);
  };
  const open = () => { m.hidden = false; };
  m.addEventListener('mousedown', (e) => { if (e.target === m) close(); });
  m.addEventListener('keydown', (e) => { if (e.key === 'Escape') close(); });
  for (const b of m.querySelectorAll('[data-close]')) b.addEventListener('click', close);
  return { el: m, open, close };
}

export function emptyRow(tbody, cols, text) {
  const tr = el('tr');
  const td = el('td', 'empty', text);
  td.colSpan = cols;
  tr.append(td);
  tbody.replaceChildren(tr);
}

// 分页条：data 为服务端返回的 {total,page,pages}；go(page) 跳页。
export function pager(box, data, go) {
  box.replaceChildren(el('span', '', t('共 {total} 条，第 {page} / {pages} 页', { total: data.total, page: data.page, pages: data.pages })), el('span', 'spacer'));
  if (data.page > 1) box.append(button(t('上一页'), '', () => go(data.page - 1)));
  if (data.page < data.pages) box.append(button(t('下一页'), '', () => go(data.page + 1)));
}

export function status(text, cls) {
  return el('span', 'st ' + cls, text);
}

export function badge(text, cls) {
  return el('span', 'badge' + (cls ? ' ' + cls : ''), text);
}

// 把 URL 片段（#a=1&b=2）解析成对象。
export function hashParams() {
  const out = {};
  for (const [k, v] of new URLSearchParams(location.hash.replace(/^#/, ''))) out[k] = v;
  return out;
}
