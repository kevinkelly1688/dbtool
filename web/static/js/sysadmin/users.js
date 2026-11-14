/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: users.js
 * @time: 2026/10/28
 */
// 用户管理：KPI、列表筛选 / 排序、新建/编辑（部门、认证方式）、启停、解锁、重置密码（手输或系统生成临时密码）、重置 2FA、详情抽屉。仅超级用户。
// 密码只在输入框里，提交后立即清空；接口除“系统生成的临时密码”（仅返回一次）外从不返回密码。
import { api, $, el, button, msgBox, busy, emptyRow, modal, qs, status, badge, pager, fillSelect, riskConfirm, confirmRun } from './ui.js';
import * as drawer from './user_drawer.js';
import { t, tp } from '../i18n.js';

const setMsg = msgBox($('us-msg'));
const setEditMsg = msgBox($('ue-msg'));
const setPwMsg = msgBox($('up-msg'));
const editModal = modal('m-user-edit');
const pwModal = modal('m-user-pw', { beforeClose: () => { $('up-pw-text').textContent = ''; return true; } });
const API = '/api/v1/settings/users';

let opts = { auth_groups: [], resource_groups: [] };
let editing = null; // 正在编辑的用户详情；null 表示新建
let pwTarget = null;
let page = 1;

const TFA = { totp: 'TOTP', sms: t('短信') };

function fmtMinutes(sec) { return sec >= 60 ? t('{n} 分', { n: Math.ceil(sec / 60) }) : t('{n} 秒', { n: sec }); }

function notifyText(u) {
  const parts = [];
  if (u.has_ding) parts.push(t('钉钉') + ' ✓');
  if (u.has_feishu) parts.push(t('飞书') + ' ✓');
  if (u.has_slack) parts.push('Slack ✓');
  return parts.length ? parts.join(' ') : '—';
}

// ---- KPI ----

function kpi(label, value, sub, cls) {
  const d = el('div');
  d.append(el('b', cls || '', String(value)), el('span', '', label));
  if (sub) d.append(el('span', 'small', sub));
  return d;
}

async function loadKpis() {
  const s = await api('GET', API + '/summary');
  $('us-kpis').replaceChildren(
    kpi(t('用户总数'), s.total, t('{active} 人正常 · {inactive} 人停用', { active: s.active, inactive: s.inactive }), ''),
    kpi(t('超级用户'), s.superuser, '', ''),
    kpi(t('已锁定'), s.locked, '', s.locked ? 'crit' : ''),
    kpi(t('未绑定 2FA'), s.no_2fa, '', s.no_2fa ? 'warn' : ''),
  );
}

// ---- 列表 ----

function row(u) {
  const tr = el('tr');
  if (u.locked) tr.className = 'lv-warn';
  const groupCell = (list) => {
    const td = el('td', 'small');
    if (!list.length) { td.className = 'muted'; td.textContent = '—'; return td; }
    for (const g of list) td.append(badge(g.name), document.createTextNode(' '));
    return td;
  };
  const st = el('td');
  if (!u.is_active) st.append(status(t('已停用'), 'aborted'));
  else if (u.locked) st.append(status(t('已锁定'), 'rejected'), el('small', 'muted', ' ' + t('失败 {n} 次 · 剩余 {left}', { n: u.failed_login_count, left: fmtMinutes(u.lock_remaining) })));
  else st.append(status(t('正常'), 'passed'));
  if (u.is_superuser) st.append(document.createTextNode(' '), badge(t('超级用户'), 'warn'));
  if (u.must_change) st.append(document.createTextNode(' '), badge(t('须改密'), 'warn'));
  const last = el('td', 'small', u.last_login || '—');
  if (u.last_login_failed_at && (!u.last_login || u.last_login_failed_at > u.last_login)) last.textContent = t('{time} 失败', { time: u.last_login_failed_at });
  const ops = el('td', 'row-actions');
  if (u.locked || (u.is_active && u.failed_login_count > 0)) ops.append(button(t('解锁'), u.locked ? 'primary' : '', () => unlock(u)), ' ');
  ops.append(button(t('详情'), '', () => openDrawer(u.id)), ' ', button(t('编辑'), '', () => openEdit(u.id)), ' ');
  const rp = button(t('重置密码'), '', () => openPw(u));
  if (!u.can_reset) { rp.disabled = true; rp.title = t('{auth} 认证，密码由外部系统管理，不能在这里重置', { auth: tp(u.auth_label) }); }
  ops.append(rp, ' ');
  if (u.two_fa.length) ops.append(button(t('重置 2FA'), '', () => reset2fa(u)), ' ');
  ops.append(u.is_active ? button(t('停用'), 'ghost danger', () => toggle(u, false)) : button(t('启用'), '', () => toggle(u, true)));
  const nameCell = el('td', 'small');
  nameCell.append(document.createTextNode(u.display));
  nameCell.append(el('div', 'muted', u.email || '—'));
  tr.append(el('td', 'mono', u.username), nameCell, el('td', 'small', u.dept || '—'), groupCell(u.auth_groups), groupCell(u.resource_groups),
    el('td', 'small', tp(u.auth_label) || t('本地')), st,
    el('td', 'small', u.two_fa.length ? u.two_fa.map((k) => TFA[k] || k).join(' + ') : '—'), last, el('td', 'small muted', notifyText(u)), ops);
  return tr;
}

async function load(p = page) {
  page = p;
  const data = await api('GET', API + qs({ search: $('us-search').value.trim(), auth_group: $('us-auth').value, resource_group: $('us-res').value,
    status: $('us-status').value, sort: $('us-sort').value, page, page_size: 20 }));
  const body = $('us-rows');
  if (!data.rows.length) emptyRow(body, 11, t('没有符合条件的用户。'));
  else { body.replaceChildren(); for (const u of data.rows) body.append(row(u)); }
  pager($('us-pager'), data, (n) => load(n).catch((e) => setMsg(e.message)));
  loadKpis().catch(() => {});
}

// 确认（权限码、高危标记、失败留在弹窗里重试）后调接口；取消不发请求。
async function act(confirmOpts, method, url, done, body) {
  const r = await confirmRun(confirmOpts, () => api(method, url, body));
  if (!r.ok) return;
  setMsg('');
  try {
    await load();
    await drawer.refresh();
    setMsg(done, 'info');
  } catch (e) { setMsg(e.message); }
}

const userDetails = (u) => [{ k: t('账号'), v: u.username }, { k: t('姓名'), v: u.display }];

const unlock = (u) => act({
  title: t('解锁账号'), code: 'user.unlock',
  lines: [t('解锁后 {name} 的登录失败计数清零，可以立即再次登录。', { name: u.username })],
  details: [...userDetails(u), { k: t('连续失败'), v: t('{n} 次', { n: u.failed_login_count }) }],
  okText: t('确认解锁'),
}, 'POST', `${API}/${u.id}/unlock`, t('已解锁 {name}', { name: u.username }));

const reset2fa = (u) => act({
  title: t('重置两步验证'), code: 'user.reset', level: 'high',
  lines: [t('其已绑定的验证方式会被全部删除，下次登录需要重新绑定（如果系统开启了强制两步验证）。'), t('该操作会写入系统审计。')],
  details: [...userDetails(u), { k: t('已绑定'), v: (u.two_fa || []).map((k) => TFA[k] || k).join(' + ') }],
  okText: t('确认重置'),
}, 'POST', `${API}/${u.id}/reset-2fa`, t('已重置 {name} 的 2FA', { name: u.username }));

const toggle = (u, on) => (on
  ? act({
    title: t('启用账号'), code: 'user.disable',
    lines: [t('启用后 {name} 可以重新登录。', { name: u.username })],
    details: userDetails(u), okText: t('确认启用'),
  }, 'POST', `${API}/${u.id}/enable`, t('已启用 {name}', { name: u.username }))
  : act({
    title: t('停用账号'), code: 'user.disable', level: 'high',
    lines: [t('停用后该账号不能再登录；已登录的会话可能保持到过期。'), t('历史审计与工单保留，可随时重新启用。')],
    details: userDetails(u), okText: t('确认停用'),
  }, 'POST', `${API}/${u.id}/disable`, t('已停用 {name}', { name: u.username })));

async function openDrawer(id) {
  try { await drawer.open(id); } catch (e) { setMsg(e.message); }
}

// ---- 新建 / 编辑 ----

function groupChecks(box, items, selected) {
  box.replaceChildren();
  for (const g of items) {
    const lb = el('label');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = String(g.id);
    cb.checked = selected.includes(g.id);
    lb.append(cb, document.createTextNode(g.name));
    box.append(lb);
  }
  if (!items.length) box.append(el('span', 'hint', t('（还没有）')));
}

const checked = (box) => [...box.querySelectorAll('input:checked')].map((c) => Number(c.value));

function syncAuthType() {
  // 新建 LDAP 用户不需要本地密码
  const isNew = !editing;
  $('ue-pw-box').hidden = !isNew || $('ue-authtype').value === 'ldap';
}

async function openEdit(id, keepDrawer) {
  setEditMsg('');
  editing = null;
  if (id) {
    try { editing = await api('GET', `${API}/${id}`); } catch (e) { setMsg(e.message); return; }
  }
  if (!keepDrawer) drawer.close();
  const u = editing;
  $('ue-title').textContent = u ? t('编辑用户：{name}', { name: u.username }) : t('新建用户');
  $('ue-username').value = u ? u.username : '';
  $('ue-username').disabled = !!u;
  $('ue-display').value = u ? u.display : '';
  $('ue-email').value = u ? u.email : '';
  $('ue-dept').value = u ? u.dept || '' : '';
  $('ue-authtype').value = 'local';
  $('ue-auth-box').hidden = !!u;
  $('ue-auth-ro').hidden = !u;
  $('ue-auth-text').value = u ? tp(u.auth_label) || t('本地') : '';
  $('ue-password').value = '';
  syncAuthType();
  $('ue-ding').value = u ? u.ding_user_id : '';
  $('ue-feishu').value = u ? u.feishu_open_id : '';
  $('ue-slack').value = u ? u.slack_id : '';
  $('ue-active').checked = u ? u.is_active : true;
  $('ue-super').checked = u ? u.is_superuser : false;
  groupChecks($('ue-auth'), opts.auth_groups, u ? u.auth_group_ids : []);
  groupChecks($('ue-res'), opts.resource_groups, u ? u.resource_group_ids : []);
  editModal.open();
  (u ? $('ue-display') : $('ue-username')).focus();
}

async function saveEdit() {
  setEditMsg('');
  const body = {
    username: $('ue-username').value.trim(), display: $('ue-display').value, email: $('ue-email').value, password: $('ue-password').value,
    dept: $('ue-dept').value.trim(),
    ding_user_id: $('ue-ding').value, feishu_open_id: $('ue-feishu').value, slack_id: $('ue-slack').value,
    is_active: $('ue-active').checked, is_superuser: $('ue-super').checked,
    auth_group_ids: checked($('ue-auth')), resource_group_ids: checked($('ue-res')),
  };
  if (!editing) body.auth_type = $('ue-authtype').value;
  // 敏感变更二次确认
  const warns = [];
  if (body.is_superuser && !(editing && editing.is_superuser)) warns.push(t('授予超级用户（绕过全部权限检查）'));
  if (editing && editing.is_superuser && !body.is_superuser) warns.push(t('取消超级用户身份'));
  if (editing) {
    const before = [...editing.auth_group_ids].sort().join();
    if (before !== [...body.auth_group_ids].sort().join()) warns.push(t('调整权限组'));
  }
  if (warns.length) {
    const grantSuper = body.is_superuser && !(editing && editing.is_superuser);
    const r = await riskConfirm({
      title: grantSuper ? t('授予超级用户') : t('敏感变更确认'), level: 'high', code: grantSuper ? 'user.grant_super' : 'user.edit',
      lines: [...warns.map((w) => '· ' + w), t('该变更会写入系统审计。')],
      details: [{ k: t('账号'), v: body.username }, { k: t('姓名'), v: body.display }],
      okText: t('确认保存'),
    });
    if (!r.ok) return;
  }
  await busy($('ue-save'), async () => {
    try {
      if (editing) await api('PUT', `${API}/${editing.id}`, body);
      else await api('POST', API, body);
      $('ue-password').value = '';
      editModal.close();
      await load();
      await drawer.refresh();
      setMsg(t('已保存'), 'info');
    } catch (e) { setEditMsg(e.message); }
  });
}

// ---- 重置密码 ----

function syncPwMode() {
  $('up-manual').hidden = !$('up-mode-man').checked;
}

function openPw(u) {
  pwTarget = u;
  setPwMsg('');
  $('up-title').textContent = t('重置密码：{name}', { name: u.username });
  $('up-mode-gen').checked = true;
  $('up-password').value = '';
  $('up-password2').value = '';
  $('up-force').checked = false;
  $('up-pw-text').textContent = '';
  $('up-form').hidden = false;
  $('up-result').hidden = true;
  $('up-save').hidden = false;
  $('up-cancel').textContent = t('取消');
  syncPwMode();
  pwModal.open();
}

async function savePw() {
  setPwMsg('');
  const gen = $('up-mode-gen').checked;
  const body = gen ? { generate: true } : { password: $('up-password').value, force_change: $('up-force').checked };
  if (!gen && body.password !== $('up-password2').value) { setPwMsg(t('两次输入的密码不一致')); return; }
  const target = pwTarget;
  // 密码只在提交的那一刻进入请求体；确认弹窗里不显示、不回显
  const r = await confirmRun({
    title: t('重置密码'), code: 'user.reset', level: 'high',
    lines: gen
      ? [t('将为该账号生成一个符合密码策略的临时密码。新密码只显示一次，关闭后无法再查看，也不会写入审计；用户首次登录须修改。')]
      : [body.force_change ? t('将把该账号的密码改为你刚输入的新密码，用户下次登录须先修改密码。') : t('将把该账号的密码改为你刚输入的新密码。'), t('密码不会写入审计。')],
    details: [{ k: t('账号'), v: target.username }, { k: t('姓名'), v: target.display }, { k: t('方式'), v: gen ? t('系统生成临时密码') : t('手输新密码') }],
    okText: t('确认重置'),
  }, () => api('POST', `${API}/${target.id}/reset-password`, body));
  if (!r.ok) return;
  const res = r.result;
  try {
    $('up-password').value = '';
    $('up-password2').value = '';
    if (gen) {
      // 临时密码只显示这一次：关闭窗口即清除
      $('up-pw-text').textContent = res.password;
      $('up-form').hidden = true;
      $('up-result').hidden = false;
      $('up-save').hidden = true;
      $('up-cancel').textContent = t('我已记录，关闭');
      await load();
      await drawer.refresh();
    } else {
      pwModal.close();
      await load();
      await drawer.refresh();
      setMsg(res.force_change ? t('已重置 {name} 的密码，用户下次登录须先修改密码', { name: target.username }) : t('已重置 {name} 的密码', { name: target.username }), 'info');
    }
  } catch (e) { setPwMsg(e.message); }
}

async function copyPw() {
  const text = $('up-pw-text').textContent;
  try {
    await navigator.clipboard.writeText(text);
    setPwMsg(t('已复制到剪贴板'), 'info');
  } catch (_) {
    const sel = window.getSelection();
    const range = document.createRange();
    range.selectNodeContents($('up-pw-text'));
    sel.removeAllRanges();
    sel.addRange(range);
    setPwMsg(t('浏览器不允许自动复制，已选中密码，请按 Ctrl+C 复制'), 'warn');
  }
}

async function init() {
  opts = await api('GET', API + '/options');
  fillSelect($('us-auth'), [{ id: '', name: t('全部权限组') }, ...opts.auth_groups], (g) => g.name, (g) => g.id);
  fillSelect($('us-res'), [{ id: '', name: t('全部资源组') }, ...opts.resource_groups], (g) => g.name, (g) => g.id);
  await load(1);
}

drawer.init({
  edit: (u) => openEdit(u.id, true),
  unlock, reset2fa, toggle,
  resetPw: (u) => openPw(u),
  changed: () => load().catch((e) => setMsg(e.message)),
});
$('us-filter').addEventListener('submit', (e) => { e.preventDefault(); load(1).catch((err) => setMsg(err.message)); });
for (const id of ['us-auth', 'us-res', 'us-status', 'us-sort']) $(id).addEventListener('change', () => load(1).catch((err) => setMsg(err.message)));
$('us-new').addEventListener('click', () => openEdit(0));
$('ue-save').addEventListener('click', saveEdit);
$('ue-authtype').addEventListener('change', syncAuthType);
$('up-save').addEventListener('click', savePw);
$('up-copy').addEventListener('click', copyPw);
$('up-mode-gen').addEventListener('change', syncPwMode);
$('up-mode-man').addEventListener('change', syncPwMode);

init().catch((e) => { emptyRow($('us-rows'), 11, t('加载失败')); setMsg(e.message); });
