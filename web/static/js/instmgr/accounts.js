/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: accounts.js
 * @time: 2026/10/28
 */
// 账号管理：账号列表（未录入 / 高危 / 已锁定筛选与计数）、创建、录入（使用人 / 用途 / 到期日）、授权/回收（先预览语句，含 ALL PRIVILEGES）、改密、锁定/解锁、删除。
import { api, $, el, btn, setMsg, qs, emptyRow, cell, openModal, closeModal, confirmBox, loadCaps, can, loadInstanceSelect, busy } from './ui.js';
import { t, tp } from '../i18n.js';

const msg = $('ac-msg');
let choices = [];
let caps = {};
let canLock = false;
let regReady = false; // 旁路表 dbt_account_reg 已建：可以登记使用人 / 用途 / 到期日
let owners = null; // 使用人下拉：{inst, list}
let target = null; // 正在操作的账号

const inst = () => Number($('ac-inst').value);
const cur = () => choices.find((c) => c.id === inst());
const url = (path) => `/api/v1/instances/${inst()}/accounts${path}`;
const who = (a) => `'${a.user}'@'${a.host}'`;

// 状态筛选：saved 已录入 / unsaved 未录入 / risk 高危 / locked 已锁定
function statusParams() {
  const v = $('ac-saved').value;
  return { saved: v === 'saved' ? '1' : v === 'unsaved' ? '0' : '', risk: v === 'risk' ? '1' : '', locked: v === 'locked' ? '1' : '' };
}

async function load() {
  setMsg(msg, '');
  $('ac-count').textContent = '';
  const c = cur();
  if (!c) return emptyRow($('ac-rows'), 5, t('没有可用的实例'));
  if (!c.supported) { emptyRow($('ac-rows'), 5, '—'); return setMsg(msg, t('暂不支持 {type} 类型实例的账号管理', { type: tp(c.db_type_label) }), 'warn'); }
  emptyRow($('ac-rows'), 5, t('加载中…'));
  try {
    const res = await api('GET', url('') + qs({ ...statusParams(), q: $('ac-q').value.trim() }));
    canLock = res.supports_lock;
    regReady = !!res.reg_ready;
    $('ac-count').textContent = t('共 {total} 个 · 高危 {risky} · 已锁定 {locked} · 未录入 {unsaved}', { total: res.total, risky: res.risky, locked: res.locked, unsaved: res.unsaved });
    render(res.items);
  } catch (e) { emptyRow($('ac-rows'), 5, t('加载失败')); setMsg(msg, e.message); }
}

function render(items) {
  const tb = $('ac-rows');
  if (!items.length) return emptyRow(tb, 5, t('没有符合条件的账号'));
  tb.replaceChildren();
  const canEdit = can(caps, 'account.edit');
  const canGrant = can(caps, 'account.grant');
  const canPwd = can(caps, 'account.pwd');
  const canLockBtn = can(caps, 'account.lock');
  const canDrop = can(caps, 'account.delete');
  for (const a of items) {
    const tr = el('tr', a.risky ? 'lv-warn' : '');
    const name = el('td', 'mono');
    name.append(who(a) + ' ');
    name.append(el('span', 'badge' + (a.saved ? ' ok' : ''), a.saved ? t('已录入') : t('未录入')));
    if (a.is_locked) name.append(' ', el('span', 'badge warn', t('已锁定')));
    if (a.risky) { const b = el('span', 'badge crit', t('高危')); b.title = a.risk_note; name.append(' ', b); }
    if (a.protected) { const b = el('span', 'badge', t('受保护')); b.title = tp(a.protect_reason); name.append(' ', b); }
    const priv = el('td', 'mono wrap');
    priv.textContent = a.privileges.join('\n');
    const reg = el('td', 'im-reg');
    if (a.owner || a.purpose || a.expire_date) {
      if (a.owner) reg.append(el('div', '', t('{name}（{user}）', { name: a.owner_display || a.owner, user: a.owner })));
      if (a.purpose) reg.append(el('div', 'small', a.purpose));
      if (a.expire_date) {
        const d = el('div', 'small', t('到期：{date}', { date: a.expire_date }) + ' ');
        if (a.expire_state === 'expired') d.append(el('span', 'badge crit', t('已过期')));
        else if (a.expire_state === 'soon') d.append(el('span', 'badge warn', a.expire_days === 0 ? t('今天到期') : t('{n} 天后到期', { n: a.expire_days })));
        reg.append(d);
      }
    } else reg.append(el('span', 'muted', a.protected && !a.saved ? '—' : t('未登记')));
    const ops = el('td', 'row-actions');
    if (canEdit) ops.append(btn(a.saved ? t('编辑') : t('录入'), '', () => openEdit(a)), ' ');
    if (canGrant) {
      const grantBtn = btn(t('授权'), 'primary', () => openGrant(a));
      if (a.builtin) { grantBtn.disabled = true; grantBtn.title = tp(a.protect_reason); } // 内置账号（含 root 本机账号）不能授权 / 回收
      ops.append(grantBtn, ' ');
    }
    const prot = (b) => { if (a.protected) { b.disabled = true; b.title = tp(a.protect_reason); } return b; };
    if (canPwd) ops.append(prot(btn(t('改密'), '', () => openPwd(a))), ' ');
    if (canLock && canLockBtn) ops.append(prot(btn(a.is_locked ? t('解锁') : t('锁定'), '', () => lock(a))), ' ');
    if (canDrop) ops.append(prot(btn(t('删除'), 'ghost danger', () => drop(a))));
    tr.append(name, priv, reg, cell(a.remark), ops);
    tb.append(tr);
  }
}

// ---- 使用人 / 用途 / 到期日（旁路表）----
async function fillOwners(select, selected) {
  if (!owners || owners.inst !== inst()) {
    try { owners = { inst: inst(), list: await api('GET', url('/owners')) }; } catch (_) { owners = { inst: inst(), list: [] }; }
  }
  select.replaceChildren(new Option(t('（不填）'), ''), ...owners.list.map((u) => new Option(t('{name}（{user}）', { name: u.display || u.username, user: u.username }), u.username)));
  select.value = selected || '';
}

function regInputs(prefix, hintId) {
  const ids = [prefix + '-owner', prefix + '-purpose', prefix + '-expire'];
  for (const id of ids) $(id).disabled = !regReady;
  $(hintId).textContent = regReady ? '' : t('使用人 / 用途 / 到期日未启用：需先执行 071_account_reg.sql');
}

// ---- 创建 ----
async function openCreate() {
  if (!cur() || !cur().supported) return setMsg(msg, t('请先选择一个支持的实例'));
  setMsg($('ac-create-msg'), '');
  for (const id of ['c-user', 'c-host', 'c-pw', 'c-pw2', 'c-remark', 'c-purpose', 'c-expire']) $(id).value = '';
  regInputs('c', 'c-reg-hint');
  await fillOwners($('c-owner'), '');
  openModal('ac-m-create');
}

async function submitCreate() {
  const body = { user: $('c-user').value.trim(), host: $('c-host').value.trim(), password: $('c-pw').value, password2: $('c-pw2').value, remark: $('c-remark').value };
  if (regReady) Object.assign(body, { owner: $('c-owner').value, purpose: $('c-purpose').value, expire_date: $('c-expire').value });
  if (!body.user || !body.host || !body.password) return setMsg($('ac-create-msg'), t('账号、主机、密码都要填写'));
  if (body.password !== body.password2) return setMsg($('ac-create-msg'), t('两次输入的密码不一致'));
  const hosts = body.host.split('|').map((h) => h.trim()).filter(Boolean);
  const ok = await confirmBox({ title: t('在 {name} 上创建账号？', { name: cur().instance_name }), lines: [t('新账号没有任何权限，创建后用“授权”功能授予。操作写入操作日志（不记录密码）。')],
    sql: hosts.map((h) => `CREATE USER '${body.user}'@'${h}' IDENTIFIED BY '******';`), okText: t('确定创建'), danger: false });
  if (!ok) return;
  try {
    const r = await busy($('c-submit'), () => api('POST', url(''), { ...body, confirm: true }));
    closeModal('ac-m-create');
    await load();
    const created = t('已创建 {n} 个账号', { n: r.created.length });
    setMsg(msg, r.failed.length ? t('{created}；有问题：{list}', { created, list: r.failed.join(t('；')) }) : created, r.failed.length ? 'warn' : 'info');
  } catch (e) { setMsg($('ac-create-msg'), e.message); }
}

// ---- 录入 / 编辑平台登记 ----
async function openEdit(a) {
  target = a;
  setMsg($('ac-edit-msg'), '');
  $('e-user').value = a.user;
  $('e-host').value = a.host;
  $('e-pw').value = '';
  $('e-remark').value = a.remark || '';
  $('e-purpose').value = a.purpose || '';
  $('e-expire').value = a.expire_date || '';
  regInputs('e', 'e-reg-hint');
  await fillOwners($('e-owner'), a.owner);
  openModal('ac-m-edit');
}

async function submitEdit() {
  try {
    const body = { user: target.user, host: target.host, password: $('e-pw').value, remark: $('e-remark').value };
    if (regReady) Object.assign(body, { owner: $('e-owner').value, purpose: $('e-purpose').value, expire_date: $('e-expire').value });
    await busy($('e-submit'), () => api('PUT', url('/info'), body));
    closeModal('ac-m-edit');
    await load();
    setMsg(msg, t('已保存'), 'info');
  } catch (e) { setMsg($('ac-edit-msg'), e.message); }
}

// ---- 改密 ----
function openPwd(a) {
  target = a;
  setMsg($('ac-pwd-msg'), '');
  $('p-title').textContent = t('实例 {inst} · 账号 {account}', { inst: cur().instance_name, account: `${a.user}@${a.host}` });
  $('p-pw').value = '';
  $('p-pw2').value = '';
  openModal('ac-m-pwd');
}

async function submitPwd() {
  const pw = $('p-pw').value;
  if (!pw) return setMsg($('ac-pwd-msg'), t('请输入新密码'));
  if (pw !== $('p-pw2').value) return setMsg($('ac-pwd-msg'), t('两次输入的密码不一致'));
  const ok = await confirmBox({ title: t('确定修改该账号的密码？'), lines: [t('{who} 的密码会在数据库里被修改，并同步更新平台登记信息。使用旧密码的应用会连不上。', { who: who(target) })], sql: [`ALTER USER ${who(target)} IDENTIFIED BY '******';`], okText: t('确定修改') });
  if (!ok) return;
  try {
    await busy($('p-submit'), () => api('POST', url('/password'), { user: target.user, host: target.host, password: pw, password2: $('p-pw2').value, confirm: true }));
    closeModal('ac-m-pwd');
    await load();
    setMsg(msg, t('密码已修改'), 'info');
  } catch (e) { setMsg($('ac-pwd-msg'), e.message); }
}

// ---- 锁定 / 删除 ----
async function lock(a) {
  const to = !a.is_locked;
  const ok = await confirmBox({ title: to ? t('确定锁定账号 {who}？', { who: who(a) }) : t('确定解锁账号 {who}？', { who: who(a) }), lines: [to ? t('锁定后该账号无法登录，已有连接不受影响。') : t('解锁后该账号可以重新登录。')],
    sql: [`ALTER USER ${who(a)} ACCOUNT ${to ? 'LOCK' : 'UNLOCK'};`], okText: to ? t('确定锁定') : t('确定解锁'), danger: to });
  if (!ok) return;
  try { await api('POST', url('/lock'), { user: a.user, host: a.host, lock: to, confirm: true }); await load(); setMsg(msg, to ? t('已锁定') : t('已解锁'), 'info'); } catch (e) { setMsg(msg, e.message); }
}

async function drop(a) {
  const ok = await confirmBox({ title: t('确定删除账号 {who}？', { who: who(a) }), lines: [t('删除后使用该账号的应用将无法连接，且不可恢复。同时会清除平台登记的信息。')], sql: [`DROP USER ${who(a)};`], okText: t('确定删除'), typeText: `${a.user}@${a.host}` });
  if (!ok) return;
  try { await api('POST', url('/drop'), { user: a.user, host: a.host, confirm: true }); await load(); setMsg(msg, t('已删除'), 'info'); } catch (e) { setMsg(msg, e.message); }
}

// ---- 授权 / 回收 ----
const ALL_PRIV = 'ALL PRIVILEGES';
const PRIV_GROUPS = {
  global: [[t('全部权限（高危）'), [ALL_PRIV]], [t('数据'), ['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'FILE']],
    [t('结构'), ['CREATE', 'ALTER', 'INDEX', 'DROP', 'CREATE TEMPORARY TABLES', 'SHOW VIEW', 'CREATE ROUTINE', 'ALTER ROUTINE', 'EXECUTE', 'CREATE VIEW', 'EVENT', 'TRIGGER']],
    [t('管理'), ['GRANT OPTION', 'SUPER', 'PROCESS', 'RELOAD', 'SHUTDOWN', 'SHOW DATABASES', 'LOCK TABLES', 'REFERENCES', 'REPLICATION CLIENT', 'REPLICATION SLAVE', 'CREATE USER']]],
  db: [[t('全部权限（高危）'), [ALL_PRIV]], [t('数据'), ['SELECT', 'INSERT', 'UPDATE', 'DELETE']],
    [t('结构'), ['CREATE', 'ALTER', 'INDEX', 'DROP', 'CREATE TEMPORARY TABLES', 'SHOW VIEW', 'CREATE ROUTINE', 'ALTER ROUTINE', 'EXECUTE', 'CREATE VIEW', 'EVENT', 'TRIGGER']],
    [t('管理'), ['GRANT OPTION', 'LOCK TABLES', 'REFERENCES']]],
  table: [[t('数据'), ['SELECT', 'INSERT', 'UPDATE', 'DELETE']], [t('结构'), ['CREATE', 'ALTER', 'INDEX', 'DROP', 'SHOW VIEW', 'CREATE VIEW', 'TRIGGER']], [t('管理'), ['GRANT OPTION', 'REFERENCES']]],
  column: [[t('数据'), ['SELECT', 'INSERT', 'UPDATE', 'REFERENCES']]],
};
const DANGER = new Set(['FILE', 'SUPER', 'PROCESS', 'RELOAD', 'SHUTDOWN', 'CREATE USER', 'REPLICATION CLIENT', 'REPLICATION SLAVE', 'GRANT OPTION', ALL_PRIV]);
let previewed = false;

function invalidate() {
  previewed = false;
  $('g-submit').disabled = true;
  $('g-sql').textContent = t('（选项已变更，需重新预览）');
  $('g-warn').replaceChildren();
}

function drawPrivs() {
  const scope = $('g-scope').value;
  const keep = new Set([...$('g-privs').querySelectorAll('input:checked')].map((c) => c.value));
  const box = $('g-privs');
  box.replaceChildren();
  for (const [title, names] of PRIV_GROUPS[scope]) {
    const grp = el('div', 'privgrp');
    const head = el('label', 'check');
    const all = document.createElement('input');
    all.type = 'checkbox';
    head.append(all, ' ', el('b', '', title));
    const list = el('div', 'check-list');
    const cbs = [];
    for (const n of names) {
      const lb = el('label');
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.value = n;
      cb.checked = keep.has(n);
      cbs.push(cb);
      lb.append(cb, ' ', DANGER.has(n) ? el('span', 'crit', n) : document.createTextNode(n));
      list.append(lb);
    }
    all.addEventListener('change', () => { cbs.forEach((c) => { c.checked = all.checked; }); syncAll(); invalidate(); });
    // “全部权限”这一组没有“整组全选”的意义：隐藏组头的复选框，只留标题
    if (names.length === 1 && names[0] === ALL_PRIV) all.hidden = true;
    grp.append(head, list);
    box.append(grp);
  }
}

// 勾了 ALL PRIVILEGES，其他权限（除 GRANT OPTION，它不包含在 ALL 里）就不用再选了：取消并置灰，避免产生歧义。
function syncAll() {
  const boxes = [...$('g-privs').querySelectorAll('.check-list input')];
  const on = boxes.some((c) => c.value === ALL_PRIV && c.checked);
  for (const c of boxes) {
    if (c.value === ALL_PRIV || c.value === 'GRANT OPTION') continue;
    if (on) c.checked = false;
    c.disabled = on;
  }
}

function setMulti(sel, multi) {
  sel.multiple = multi;
  sel.size = 6;
}

function selected(sel) {
  return [...sel.selectedOptions].map((o) => o.value).filter(Boolean);
}

async function fillObjects(sel, params, keepNone) {
  sel.replaceChildren();
  try {
    const list = await api('GET', url('/objects') + qs(params));
    if (!list.length && !keepNone) sel.append(new Option(t('（没有）'), ''));
    for (const n of list) sel.append(new Option(n, n));
  } catch (e) { setMsg($('ac-grant-msg'), e.message); }
}

function syncScope() {
  const scope = $('g-scope').value;
  $('g-db-box').hidden = scope === 'global';
  $('g-tb-box').hidden = !(scope === 'table' || scope === 'column');
  $('g-col-box').hidden = scope !== 'column';
  $('g-db-lbl').textContent = scope === 'db' ? t('数据库（可多选）') : t('数据库');
  setMulti($('g-db'), scope === 'db');
  setMulti($('g-table'), scope === 'table');
  setMulti($('g-column'), true);
  drawPrivs();
  invalidate();
  $('g-table').replaceChildren();
  $('g-column').replaceChildren();
}

async function openGrant(a) {
  target = a;
  setMsg($('ac-grant-msg'), '');
  $('g-title').textContent = t('实例 {inst} · 账号 {account}', { inst: cur().instance_name, account: `${a.user}@${a.host}` });
  $('g-op').value = 'grant';
  $('g-scope').value = 'db';
  syncScope();
  openModal('ac-m-grant');
  await fillObjects($('g-db'), {});
}

function grantBody(confirm) {
  const scope = $('g-scope').value;
  const b = { user: target.user, host: target.host, op: $('g-op').value, scope, privs: [...$('g-privs').querySelectorAll('.check-list input:checked')].map((c) => c.value), confirm };
  b.dbs = scope === 'global' ? [] : selected($('g-db'));
  b.tables = scope === 'table' ? selected($('g-table')) : scope === 'column' ? selected($('g-table')).slice(0, 1) : [];
  b.columns = scope === 'column' ? selected($('g-column')) : [];
  return b;
}

async function previewGrant() {
  setMsg($('ac-grant-msg'), '');
  try {
    const plan = await busy($('g-preview'), () => api('POST', url('/grant/preview'), grantBody(false)));
    $('g-sql').textContent = plan.statements.join('\n');
    const w = $('g-warn');
    w.replaceChildren();
    for (const warn of plan.warnings) w.append(el('div', 'alert warn', warn));
    previewed = true;
    $('g-submit').disabled = false;
  } catch (e) { invalidate(); setMsg($('ac-grant-msg'), e.message); }
}

async function submitGrant() {
  if (!previewed) return;
  const body = grantBody(true);
  const revoke = body.op === 'revoke';
  const ok = await confirmBox({ title: revoke ? t('确定回收权限？') : t('确定授予权限？'), lines: [t('实例 {inst}，账号 {who}。操作写入操作日志。', { inst: cur().instance_name, who: who(target) }), ...[...$('g-warn').children].map((d) => d.textContent)],
    sql: $('g-sql').textContent.split('\n'), okText: t('确定执行') });
  if (!ok) return;
  try {
    await busy($('g-submit'), () => api('POST', url('/grant'), body));
    closeModal('ac-m-grant');
    await load();
    setMsg(msg, revoke ? t('权限已回收') : t('权限已授予'), 'info');
  } catch (e) { setMsg($('ac-grant-msg'), e.message); }
}

// ---- 初始化 ----
$('ac-filter').addEventListener('submit', (e) => { e.preventDefault(); load(); });
$('ac-inst').addEventListener('change', load);
$('ac-saved').addEventListener('change', load);
$('ac-create').addEventListener('click', openCreate);
$('c-submit').addEventListener('click', submitCreate);
$('e-submit').addEventListener('click', submitEdit);
$('p-submit').addEventListener('click', submitPwd);
$('g-op').addEventListener('change', invalidate);
$('g-privs').addEventListener('change', () => { syncAll(); invalidate(); });
$('g-scope').addEventListener('change', async () => { syncScope(); await fillObjects($('g-db'), {}); });
$('g-db').addEventListener('change', async () => {
  invalidate();
  const scope = $('g-scope').value;
  if (scope === 'table' || scope === 'column') {
    const db = selected($('g-db'))[0];
    $('g-column').replaceChildren();
    if (db) await fillObjects($('g-table'), { db }, true);
  }
});
$('g-table').addEventListener('change', async () => {
  invalidate();
  if ($('g-scope').value === 'column') {
    const db = selected($('g-db'))[0];
    const table = selected($('g-table'))[0];
    if (db && table) await fillObjects($('g-column'), { db, table }, true);
  }
});
$('g-column').addEventListener('change', invalidate);
$('g-preview').addEventListener('click', previewGrant);
$('g-submit').addEventListener('click', submitGrant);

(async () => {
  try {
    caps = await loadCaps();
    $('ac-create').hidden = !can(caps, 'account.create');
    choices = await loadInstanceSelect($('ac-inst'), 'accounts');
    await load();
  } catch (e) { setMsg(msg, e.message); }
})();
