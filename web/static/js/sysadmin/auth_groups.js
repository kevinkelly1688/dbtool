/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: auth_groups.js
 * @time: 2026/10/28
 */
// 权限组管理：权限组列表、基本信息（名称 / 说明）、成员列表、按用户查看有效权限（含单独授权 / 禁用）、按钮权限。仅超级用户。
// 权限只有按钮这一套：按钮权限树和“无权限按钮的显示方式”在 btnperm.js；按钮级单独授权 / 禁用在 btnperm_user.js。
import { api, $, el, busy, msgBox, emptyRow, tabs, qs, status, hashParams, fillSelect, badge, riskConfirm, confirmRun } from './ui.js';
import * as bp from './btnperm.js';
import * as bu from './btnperm_user.js';
import { t, tp } from '../i18n.js';

const setMsg = msgBox($('ag-msg'));
const API = '/api/v1/settings/auth-groups';
const BP = '/api/v1/settings/button-perms';

let groups = [];
let cur = null; // 当前权限组详情；null 表示正在新建
let creating = false;
let copySource = null; // 复制新建时的来源权限组：创建成功后把它的按钮授权一并复制过来
let showTab = () => {};

// ---- 左侧列表 ----

function renderList() {
  const box = $('ag-list');
  box.replaceChildren();
  for (const g of groups) {
    const b = el('button', cur && !creating && cur.id === g.id ? 'on' : '');
    b.type = 'button';
    b.setAttribute('role', 'option');
    const nm = el('span', '', g.name);
    if (g.builtin) nm.append(' ', badge(t('内置')));
    b.append(nm, el('small', '', t('{members} 人', { members: g.members })));
    if (g.description) b.title = g.description;
    b.addEventListener('click', () => select(g.id).catch((e) => setMsg(e.message)));
    box.append(b);
  }
  if (!groups.length) box.append(el('span', 'muted', t('还没有权限组')));
}

async function loadGroups() {
  groups = await api('GET', API);
  renderList();
}

// ---- 当前权限组 ----

function renderRefs() {
  const box = $('ag-refs');
  box.replaceChildren();
  if (!cur) { box.textContent = '—'; return; }
  box.append(document.createTextNode(t('成员 {members} 人；被 {flows} 条审批流引用', { members: cur.members.length, flows: cur.flows.length })));
  for (const f of cur.flows) box.append(el('div', 'muted', t('{group} · {type}（第 {level} 级）', { group: f.group_name, type: tp(f.type_label), level: f.level })));
  if (cur.flows.length) {
    const a = el('a', '', t('去审批流程'));
    a.href = '/settings/audit-flow';
    box.append(a);
  }
}

function renderMembers() {
  const body = $('ag-members');
  if (!cur) return emptyRow(body, 3, t('请先选择一个权限组'));
  if (!cur.members.length) return emptyRow(body, 3, t('这个权限组还没有成员'));
  body.replaceChildren();
  for (const u of cur.members) {
    const tr = el('tr');
    tr.append(el('td', 'mono', u.username), el('td', '', u.display), el('td'));
    tr.lastChild.append(u.is_active ? status(t('正常'), 'passed') : status(t('已停用'), 'aborted'));
    body.append(tr);
  }
}

function show() {
  $('ag-name').value = cur ? cur.name : '';
  $('ag-desc').value = cur ? cur.description || '' : '';
  // 内置权限组：不可改名、不可删除（服务端同样校验），说明可以改
  const builtin = !creating && !!cur && !!cur.builtin;
  $('ag-name').disabled = builtin;
  $('ag-name-hint').hidden = !builtin;
  $('ag-delete').hidden = creating || !cur || builtin;
  $('ag-copy').hidden = creating || !cur;
  $('ag-save').textContent = creating ? t('创建') : t('保存');
  renderRefs();
  renderMembers();
  renderList();
}

async function select(id) {
  if (bp.isDirty()) {
    const r = await riskConfirm({ title: t('放弃未保存的变更？'), lines: [t('“按钮权限”页签里有未保存的变更，切换权限组会丢弃它们。')], okText: t('放弃并切换') });
    if (!r.ok) return;
  }
  setMsg('');
  cur = await api('GET', `${API}/${id}`);
  creating = false;
  copySource = null;
  show();
  await bp.load(cur.id, cur.name, cur.members.length);
  showTab('basic');
}

function startCreate(copyFrom) {
  setMsg('');
  creating = true;
  copySource = copyFrom ? { id: copyFrom.id, name: copyFrom.name } : null;
  cur = copyFrom ? { ...copyFrom, name: t('{name} 副本', { name: copyFrom.name }), builtin: false, members: [], flows: [] } : { id: 0, name: '', description: '', members: [], flows: [] };
  show();
  bp.load(0).catch((e) => setMsg(e.message));
  showTab('basic');
  $('ag-name').focus();
}

// 只保存名称和说明（权限在“按钮权限”页签里单独保存）；复制新建时，创建成功后再把来源组的按钮授权复制过去。
async function save() {
  setMsg('');
  const body = { name: creating || !cur.builtin ? $('ag-name').value : cur.name, description: $('ag-desc').value.trim() };
  await busy($('ag-save'), async () => {
    try {
      let id;
      let copied = 0;
      if (creating) {
        id = (await api('POST', API, body)).id;
        if (copySource) copied = await copyButtons(copySource, id);
      } else { await api('PUT', `${API}/${cur.id}`, body); id = cur.id; }
      await loadGroups();
      await select(id);
      setMsg(copied ? t('已创建，复制了 {n} 个按钮授权，并写入系统审计', { n: copied }) : t('已保存，并写入系统审计'), 'info');
    } catch (e) { setMsg(e.message); }
  });
}

async function copyButtons(src, newId) {
  const g = await api('GET', `${BP}/groups/${src.id}`);
  const codes = (g.codes || []).filter((c) => bp.buttonIndex().has(c) && !bp.buttonIndex().get(c).b.locked);
  if (!codes.length) return 0;
  await api('PUT', `${BP}/groups/${newId}`, { codes, reason: t('复制自权限组「{name}」', { name: src.name }) });
  return codes.length;
}

async function remove() {
  const g = cur;
  const rr = await confirmRun({
    title: t('删除权限组'), code: 'ag.delete', level: 'high',
    lines: [t('{n} 个成员会失去这个权限组（及它带来的权限）。', { n: g.members.length }), t('被审批流引用或有待审批工单正在使用时不允许删除。')],
    details: [{ k: t('权限组'), v: g.name }, { k: t('成员'), v: t('{n} 人', { n: g.members.length }) }, { k: t('被审批流引用'), v: t('{n} 条', { n: g.flows.length }) }],
    okText: t('确认删除'),
  }, () => api('DELETE', `${API}/${g.id}`));
  if (!rr.ok) return;
  await busy($('ag-delete'), async () => {
    try {
      cur = null;
      creating = false;
      await loadGroups();
      show();
      setMsg(t('已删除'), 'info');
    } catch (e) { setMsg(e.message); }
  });
}

// ---- 按用户查看有效权限 ----

async function searchUsers() {
  const data = await api('GET', '/api/v1/settings/users' + qs({ search: $('au-q').value.trim(), page_size: 50 }));
  const sel = $('au-user');
  fillSelect(sel, [{ id: '', label: data.rows.length ? t('选择用户…') : t('没有匹配的用户') }, ...data.rows.map((u) => ({ id: u.id, label: `${u.username} ${u.display}${u.is_active ? '' : t('（已停用）')}` }))], (u) => u.label, (u) => u.id);
  return data.rows;
}

async function loadUserPerms(id) {
  await bu.load(id || 0);
}

async function openUserFromHash(id) {
  showTab('user');
  await searchUsers();
  // 目标用户可能不在前 50 个里：按用户名精确补进下拉
  if (![...$('au-user').options].some((o) => o.value === String(id))) {
    const u = await api('GET', `/api/v1/settings/users/${id}`);
    $('au-user').append(new Option(`${u.username} ${u.display}`, String(u.id)));
  }
  $('au-user').value = String(id);
  await loadUserPerms(id);
}

async function init() {
  await Promise.all([loadGroups(), bp.loadRegistry()]);
  const h = hashParams();
  if (h.user) { show(); await openUserFromHash(Number(h.user)); return; }
  if (groups.length) await select(groups[0].id);
  else show();
}

showTab = tabs($('ag-main'));
bp.init(setMsg);
bu.init(setMsg);
$('ag-new').addEventListener('click', () => startCreate(null));
$('ag-copy').addEventListener('click', () => startCreate(cur));
$('ag-save').addEventListener('click', save);
$('ag-delete').addEventListener('click', remove);
$('au-search').addEventListener('click', () => searchUsers().catch((e) => setMsg(e.message)));
$('au-q').addEventListener('keydown', (e) => { if (e.key === 'Enter') searchUsers().catch((err) => setMsg(err.message)); });
$('au-user').addEventListener('change', () => loadUserPerms(Number($('au-user').value)).catch((e) => setMsg(e.message)));

init().catch((e) => setMsg(e.message));
