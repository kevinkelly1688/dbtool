/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: btnperm.js
 * @time: 2026/10/28
 */
// 权限组管理 › 按钮权限：按钮权限树（模块折叠、页面级全选 / 仅查看 / 清空、搜索、预设、页面预览、保存前变更差异）和“无权限按钮的显示方式”。
// 页面能不能打开、按钮能不能点都只看这里的按钮授权；仅超级用户的条目和对所有登录用户开放的基础按钮不进树，只在页面下方注明。
// 接口见 /api/v1/settings/button-perms/*，仅超级用户。
import { api, $, el, button, badge, busy, modal, msgBox, confirmRun } from './ui.js';
import { t, tp } from '../i18n.js';

const BP = '/api/v1/settings/button-perms';

let reg = null; // 登记表与设置
let index = new Map(); // 按钮码 → {b, page, module}
let gid = 0; // 当前权限组；0 表示没有（新建中）
let selected = new Set();
let baseline = new Set();
let members = 0;
let gname = '';
let setMsg = () => {};

export const buttonIndex = () => index;
export const registry = () => reg;

// 默认仅超级用户、但可以显式授予的按钮加一个提示（备注里写明原因）
function noteTag(b) {
  if (b.super) return el('em', 'lg su', b.note ? tp(b.note) : t('默认仅超管'));
  return null;
}

// 基础按钮：没有任何限制、对所有登录用户开放（终止自己的工单、回滚语句、申请查询权限），不进权限树
const fixed = (b) => !!b.open;
const grantable = (b) => !b.locked && !fixed(b);
const pageButtons = (p) => p.buttons.filter(grantable);

// ---- 登记表与设置 ----

export async function loadRegistry() {
  reg = await api('GET', BP + '/registry');
  index = new Map();
  for (const m of reg.modules) for (const p of m.pages) for (const b of p.buttons) index.set(b.code, { b, page: p, module: m.name });
  const sel = $('bp-preset');
  sel.replaceChildren(new Option(t('选择预设…'), ''), ...reg.preset_names.map((n) => new Option(tp(n), n)));
  showSetting();
  $('bp-tables').hidden = reg.tables_ready;
  return reg;
}

function showSetting() {
  const s = reg.setting;
  $('bp-deny').value = s.deny_display;
  $('bp-compat').hidden = s.mode === 'strict'; // 还没执行过 dbt-admin perm migrate（原权限码没迁移成按钮授权）时提醒
}

async function saveSetting() {
  try {
    const s = await api('PUT', BP + '/setting', { deny_display: $('bp-deny').value });
    reg.setting = s;
    showSetting();
    setMsg(t('已保存，并写入系统审计'), 'info');
  } catch (e) { setMsg(e.message); }
}

// ---- 树 ----

function counts(p) {
  const bs = pageButtons(p);
  return [bs.filter((b) => selected.has(b.code)).length, bs.length];
}

function updateCounts() {
  let tot = 0, all = 0, danger = 0;
  for (const m of reg.modules) {
    let mc = 0, mt = 0;
    for (const p of m.pages) {
      const [n, tn] = counts(p);
      mc += n; mt += tn;
      const pg = document.querySelector(`#bp-tree .bp-page[data-page="${p.key}"]`);
      if (pg) {
        const cnt = pg.querySelector('.bp-cnt');
        if (!cnt) continue; // 整页仅超级用户：没有计数和复选框
        cnt.textContent = `${n} / ${tn}`;
        pg.querySelector('[data-access]').checked = n > 0;
        for (const cb of pg.querySelectorAll('input[data-code]')) cb.checked = selected.has(cb.dataset.code);
      }
      for (const b of pageButtons(p)) if (selected.has(b.code) && b.kind === 'd') danger++;
    }
    tot += mc; all += mt;
    const det = document.querySelector(`#bp-tree details[data-mod="${CSS.escape(m.name)}"]`);
    if (det) {
      det.querySelector('summary small').textContent = `${mc} / ${mt}`;
      const all = det.querySelector('summary input[data-modall]');
      if (all) { all.checked = mt > 0 && mc === mt; all.indeterminate = mc > 0 && mc < mt; all.disabled = mt === 0; }
    }
  }
  $('bp-total').textContent = String(tot);
  $('bp-all').textContent = String(all);
  $('bp-danger').textContent = String(danger);
  const dirty = isDirty();
  $('bp-save').disabled = !gid || !dirty;
  $('bp-reset').disabled = !gid || !dirty;
  $('bp-save').textContent = dirty ? t('保存…（有未保存的变更）') : t('保存…');
}

export function isDirty() {
  if (selected.size !== baseline.size) return true;
  for (const c of selected) if (!baseline.has(c)) return true;
  return false;
}

function setPage(p, mode) {
  for (const b of pageButtons(p)) {
    const on = mode === 'all' || (mode === 'view' && b.kind === 'v');
    if (on) selected.add(b.code); else selected.delete(b.code);
  }
  updateCounts();
}

// 批量勾选：整个模块 / 全局（全选可授予、仅查看类、清空）。mode 为 all / view / none。
function setModule(m, on) {
  for (const p of m.pages) for (const b of pageButtons(p)) { if (on) selected.add(b.code); else selected.delete(b.code); }
  updateCounts();
}

function setAll(mode) {
  if (!gid || !reg) return;
  selected = new Set();
  if (mode !== 'none') {
    for (const m of reg.modules) for (const p of m.pages) for (const b of pageButtons(p)) if (mode === 'all' || b.kind === 'v') selected.add(b.code);
  }
  updateCounts();
}

// 不进树的条目只注明：仅超级用户的、对所有登录用户开放的
function appendNotes(wrap, p) {
  const su = p.buttons.filter((b) => b.locked).map((b) => tp(b.label));
  const open = p.buttons.filter(fixed).map((b) => tp(b.label));
  if (su.length) wrap.append(el('p', 'small muted bp-note', t('仅超级用户：{list}', { list: su.join(t('、|sep')) })));
  if (open.length) wrap.append(el('p', 'small muted bp-note', t('所有登录用户固定开放：{list}', { list: open.join(t('、|sep')) })));
}

function renderPage(p) {
  const wrap = el('div', 'bp-page');
  wrap.dataset.page = p.key;
  const head = el('div', 'bp-head');
  if (!p.buttons.some(grantable)) { // 整页都不能授予（仅超级用户）：只列页面名和下面的说明
    head.append(el('b', '', tp(p.name)), ' ', el('span', 'mono small muted', p.route), badge(t('仅超管'), 'warn'));
    wrap.dataset.q = `${tp(p.name)} ${p.route} ${p.buttons.map((b) => `${tp(b.label)} ${b.code}`).join(' ')}`.toLowerCase();
    wrap.append(head);
    appendNotes(wrap, p);
    return wrap;
  }
  const lb = el('label', 'check');
  const access = document.createElement('input');
  access.type = 'checkbox';
  access.dataset.access = '1';
  access.setAttribute('aria-label', t('{name} 页面访问', { name: tp(p.name) }));
  access.addEventListener('change', () => setPage(p, access.checked ? 'view' : 'none'));
  lb.append(access, ' ', el('b', '', tp(p.name)));
  head.append(lb, ' ', el('span', 'mono small muted', p.route));
  head.append(el('span', 'spacer'), el('small', 'muted bp-cnt', '0 / 0'));
  for (const [mode, text] of [['all', t('全选')], ['view', t('仅查看')], ['none', t('清空')]]) {
    const a = el('a', '', text);
    a.href = '#';
    a.addEventListener('click', (e) => { e.preventDefault(); setPage(p, mode); });
    head.append(' · ', a);
  }
  head.append(' ', button(t('预览'), '', () => preview(p)));
  wrap.append(head);

  const grid = el('div', 'bp-btns');
  for (const b of p.buttons.filter(grantable)) {
    const l = el('label', 'bp k' + b.kind);
    const label = tp(b.label);
    l.dataset.q = `${tp(p.name)} ${label} ${b.code}`.toLowerCase();
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.dataset.code = b.code;
    cb.addEventListener('change', () => { if (cb.checked) selected.add(b.code); else selected.delete(b.code); updateCounts(); });
    l.append(cb, ' ', label);
    if (b.kind === 'd') { const w = el('span', 'dg', '⚠'); w.title = t('高危：授予时二次确认并写审计'); l.append(' ', w); }
    l.append(' ', el('small', '', b.code));
    const tag = noteTag(b);
    if (tag) l.append(' ', tag);
    grid.append(l);
  }
  wrap.append(grid);
  appendNotes(wrap, p);
  return wrap;
}

function renderTree() {
  const root = $('bp-tree');
  root.replaceChildren();
  for (const m of reg.modules) {
    const d = el('details', 'acc bp-mod');
    d.open = true;
    d.dataset.mod = m.name;
    const sum = el('summary', '');
    // 模块级复选框：整个模块的可授予按钮一起勾选 / 取消（点复选框不会折叠模块）
    const all = document.createElement('input');
    all.type = 'checkbox';
    all.dataset.modall = '1';
    all.setAttribute('aria-label', t('{name} 全选', { name: tp(m.name) }));
    all.addEventListener('click', (e) => e.stopPropagation());
    all.addEventListener('change', () => setModule(m, all.checked));
    sum.append(all, document.createTextNode(' ' + tp(m.name) + ' '), el('small', 'muted', ''));
    d.append(sum);
    for (const p of m.pages) d.append(renderPage(p));
    root.append(d);
  }
  filterTree();
  updateCounts();
}

function filterTree() {
  const q = $('bp-q').value.trim().toLowerCase();
  for (const d of document.querySelectorAll('#bp-tree details')) {
    let anyPage = false;
    for (const pg of d.querySelectorAll('.bp-page')) {
      let any = false;
      const labels = pg.querySelectorAll('label.bp');
      for (const l of labels) {
        const hit = q === '' || l.dataset.q.includes(q);
        l.hidden = !hit;
        any = any || hit;
      }
      if (!labels.length) any = q === '' || (pg.dataset.q || '').includes(q); // 整页仅超级用户：没有按钮可搜，按页面名 / 路由 / 说明里的按钮名匹配
      pg.hidden = !any;
      anyPage = anyPage || any;
    }
    d.hidden = !anyPage;
    if (q !== '' && anyPage) d.open = true;
  }
}

// ---- 页面预览 ----

function preview(p) {
  $('bp-ptitle').textContent = t('{name}（{route}）', { name: tp(p.name), route: p.route });
  const body = $('bp-pbody');
  body.replaceChildren();
  body.classList.remove('muted');
  const hasView = p.buttons.some((b) => b.kind === 'v' && selected.has(b.code));
  const anySel = p.buttons.some((b) => selected.has(b.code));
  if (!anySel) body.append(el('div', 'alert warn', t('该权限组在此页面没有任何按钮：菜单里看不到，也打不开这个页面。')));
  else if (!hasView) body.append(el('div', 'alert warn', t('只勾了操作按钮、没有任何查看类按钮：用户打不开这个页面。')));
  const mode = reg.setting.deny_display;
  const bar = el('div', 'pv-bar');
  const hidden = [];
  for (const b of p.buttons) {
    const label = tp(b.label);
    if (b.locked) { hidden.push(label); continue; }
    const on = selected.has(b.code) || fixed(b);
    if (!on && mode !== 'disable') { hidden.push(label); continue; }
    const x = button(label + (b.kind === 'd' ? ' ⚠' : ''), b.kind === 'd' ? 'danger' : '');
    if (!on) { x.disabled = true; x.title = t('无权限：需要「{label}」（{code}）', { label, code: b.code }); }
    bar.append(x);
  }
  body.append(bar);
  if (hidden.length) {
    const list = hidden.join(t('、|sep'));
    body.append(el('p', 'small muted', mode === 'disable' ? t('按“无权限的按钮：置灰”，仅超管的按钮不显示：{list}', { list }) : t('按“无权限的按钮：隐藏”，看不到：{list}', { list })));
  }
}

// ---- 保存 ----

let diffM = null;

const labelOf = (code) => (index.get(code) ? tp(index.get(code).b.label) : code);

function diffList(title, codes, cls) {
  const box = el('div');
  box.append(el('h4', cls, t('{title}（{n}）', { title, n: codes.length })));
  if (!codes.length) { box.append(el('p', 'small muted', t('无'))); return box; }
  const ul = el('ul', 'plain');
  for (const c of codes) {
    const it = index.get(c);
    ul.append(el('li', '', `${it ? tp(it.page.name) + ' › ' : ''}${labelOf(c)}${it && it.b.kind === 'd' ? ' ⚠' : ''}　${c}`));
  }
  box.append(ul);
  return box;
}

async function openSave() {
  if (!gid) return;
  setMsg('');
  const next = [...selected];
  const dr = await api('POST', `${BP}/groups/${gid}/diff`, { codes: next });
  if (!dr.added.length && !dr.removed.length) { setMsg(t('没有变更'), 'info'); return; }
  const danger = dr.added.filter((c) => index.get(c) && index.get(c).b.kind === 'd');
  $('bd-sum').textContent = members
    ? t('权限组「{name}」有 {members} 个成员，新增 {add} 个、移除 {remove} 个按钮；保存后立即对成员生效。', { name: gname, members, add: dr.added.length, remove: dr.removed.length })
    : t('权限组「{name}」新增 {add} 个、移除 {remove} 个按钮；保存后立即对成员生效。', { name: gname, add: dr.added.length, remove: dr.removed.length });
  const body = $('bd-body');
  body.replaceChildren(diffList(t('新增'), dr.added, 'crit'), diffList(t('移除'), dr.removed, ''));
  for (const w of dr.warnings || []) body.append(el('div', 'alert warn', tp(w)));
  $('bd-danger-box').hidden = danger.length === 0;
  $('bd-danger').checked = false;
  $('bd-reason').value = '';
  $('bd-msg').replaceChildren();
  diffM.open();
}

async function confirmSave() {
  const msg = msgBox($('bd-msg'));
  if (!$('bd-danger-box').hidden && !$('bd-danger').checked) { msg(t('请先确认高危按钮的授予')); return; }
  await busy($('bd-ok'), async () => {
    try {
      const r = await api('PUT', `${BP}/groups/${gid}`, { codes: [...selected], reason: $('bd-reason').value.trim() });
      baseline = new Set(selected);
      diffM.close();
      updateCounts();
      const warns = r.warnings || [];
      setMsg(t('已保存：新增 {add}、移除 {remove}，并写入系统审计', { add: r.added.length, remove: r.removed.length }) + (warns.length ? t('。注意：{warnings}', { warnings: warns.map((w) => tp(w)).join(t('；|sep')) }) : ''), warns.length ? 'warn' : 'info');
    } catch (e) { msg(e.message); }
  });
}

// ---- 对外 ----

// load 载入某个权限组的按钮授权；id 为 0 表示新建中（提示先保存）。
export async function load(id, name, memberCount) {
  gid = id || 0;
  gname = name || '';
  members = memberCount || 0;
  $('bp-new-hint').hidden = gid !== 0;
  $('bp-cols').hidden = gid === 0;
  $('bp-members').textContent = gid ? t('成员 {n} 人', { n: members }) : '';
  if (!gid) { selected = new Set(); baseline = new Set(); return; }
  const g = await api('GET', `${BP}/groups/${gid}`);
  selected = new Set(g.codes.filter((c) => index.has(c) && grantable(index.get(c).b)));
  baseline = new Set(selected);
  renderTree();
}

export function init(msgFn) {
  setMsg = msgFn;
  diffM = modal('m-bp-diff');
  $('bp-setting-save').addEventListener('click', () => busy($('bp-setting-save'), () => saveSetting().catch((e) => setMsg(e.message))));
  $('bp-q').addEventListener('input', filterTree);
  $('bp-bulk-view').addEventListener('click', () => setAll('view'));
  $('bp-bulk-all').addEventListener('click', () => setAll('all'));
  $('bp-bulk-none').addEventListener('click', () => setAll('none'));
  $('bp-open').addEventListener('click', (e) => { e.preventDefault(); document.querySelectorAll('#bp-tree details').forEach((d) => { d.open = true; }); });
  $('bp-close').addEventListener('click', (e) => { e.preventDefault(); document.querySelectorAll('#bp-tree details').forEach((d) => { d.open = false; }); });
  $('bp-apply-preset').addEventListener('click', () => {
    const n = $('bp-preset').value;
    if (!n || !gid) return;
    selected = new Set(reg.presets[n].filter((c) => index.has(c)));
    renderTree();
    setMsg(t('已套用预设 {name}（尚未保存）', { name: tp(n) }), 'info');
  });
  $('bp-reset').addEventListener('click', () => { selected = new Set(baseline); renderTree(); });
  $('bp-save').addEventListener('click', () => busy($('bp-save'), () => openSave().catch((e) => setMsg(e.message))));
  $('bd-ok').addEventListener('click', confirmSave);
  window.addEventListener('beforeunload', (e) => { if (isDirty()) { e.preventDefault(); e.returnValue = ''; } });
}
