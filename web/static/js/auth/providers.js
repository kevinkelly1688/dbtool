/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: providers.js
 * @time: 2026/10/28
 */
// 登录方式管理（仅超级用户）：列表、排序、启停、删除、添加、配置、测试，以及通用规则。
// 往页面写内容一律用 textContent / createElement，不拼 innerHTML。
import { api, riskConfirm, confirmRun, toast, setButtonMeta } from '../common.js';
import { t, tp, isZh } from '../i18n.js';

const $ = (id) => document.getElementById(id);
const BASE = '/api/v1/settings/auth-providers';
const SEP = isZh ? '、' : ', '; // 列表项之间的分隔
const SEP2 = isZh ? '；' : '; ';

// 确认弹窗页脚显示的权限码名称（与 internal/btnperm/registry.go 的“登录方式”一致）
setButtonMeta({
  'authp.test': { name: t('测试连接 / 测试登录') },
  'authp.edit': { name: t('编辑配置 / 入组规则'), kind: 'd' },
  'authp.toggle': { name: t('启用 / 停用 / 排序 / 删除'), kind: 'd' },
});

let ctx = null; // GET 概览的返回：providers / kinds / rules / 组选项等
let cur = null; // 配置弹窗里正在编辑的登录方式
let testing = null; // 测试弹窗里的登录方式
let dirty = false; // 配置弹窗有未保存的修改
let pendingFocus = null; // 排序后要把焦点放回的按钮 {id, which}
let dragId = null;
const clearSecrets = new Set(); // 用户点了“清除已保存的密钥”的字段

const STATUS = { enabled: [t('启用|status'), 'passed'], disabled: [t('停用|status'), 'aborted'], error: [t('异常'), 'rejected'] };
const FIRST = { create: t('自动创建'), existing: t('仅已有用户') };
// 用户属性映射的默认值，只用作 placeholder 提示（留空时后端按同样的默认处理）
const DEFAULT_ATTRS = {
  ldap: [t('（留空=登录时输入的账号）'), 'displayName|cn|name', 'mail|email'],
  oidc: ['preferred_username|email_local', 'name|preferred_username', 'email'],
  dingtalk: ['org_email_local|email_local|userid', 'name', 'email|org_email'],
  cas: ['user', 'displayName|cn|name|user', 'mail|email'],
  feishu: ['user_id|email_local|enterprise_email_local', 'name|en_name', 'email|enterprise_email'],
  slack: ['email_local', 'name|real_name', 'email'],
  wecom: ['userid', 'name|userid', 'biz_mail|email'],
};
// 手机号属性的默认值，同样只作 placeholder
const DEFAULT_PHONE = { ldap: 'mobile|telephoneNumber', oidc: 'phone_number', dingtalk: 'mobile', cas: 'mobile|phone', feishu: 'mobile', slack: t('（无）'), wecom: 'mobile' };
const MATCH_TEXT = { username: t('用户名'), email: t('邮箱'), third_id: t('第三方 ID'), phone: t('手机号') };
const MATCH_ORDER = ['third_id', 'username', 'email', 'phone'];
// 只有这几种平台在用户资料里有对应的通知 ID 字段，企业微信没有，所以不显示回填
const BACKFILL_TEXT = {
  dingtalk: t('登录后回填钉钉 UserID'),
  feishu: t('登录后回填飞书 OpenID'),
  slack: t('登录后回填 Slack ID'),
};

// ---- 小工具 ----

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

function button(text, cls, onClick) {
  const b = el('button', 'btn' + (cls ? ' ' + cls : ''), text);
  b.type = 'button';
  if (onClick) b.addEventListener('click', onClick);
  return b;
}

function msgInto(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}
const setMsg = (text, kind) => msgInto($('ap-msg'), text, kind);

// 执行期间禁用按钮，防止重复提交
async function busy(btn, fn) {
  if (btn) { btn.disabled = true; btn.setAttribute('aria-busy', 'true'); }
  try { return await fn(); } finally { if (btn) { btn.disabled = false; btn.removeAttribute('aria-busy'); } }
}

const firstLine = (s) => String(s || '').split('\n')[0];

function namesOf(ids, list) {
  const m = new Map((list || []).map((g) => [g.id, g.name]));
  return (ids || []).map((id) => m.get(id) || '#' + id);
}

function fillMulti(sel, items, chosen) {
  const set = new Set((chosen || []).map(Number));
  sel.replaceChildren(...(items || []).map((g) => {
    const o = new Option(g.name, String(g.id));
    o.selected = set.has(g.id);
    return o;
  }));
}
const picked = (sel) => [...sel.selectedOptions].map((o) => Number(o.value));

// ---- 弹窗：可叠放（测试弹窗会盖在配置弹窗上）；Esc 关最上层；打开时聚焦第一个控件，关闭后还原焦点 ----

const stack = [];
// 配置弹窗有未保存的修改时，关闭前确认一下，免得一个 Esc 把半天的配置丢了
const guards = {
  'm-prov': async () => {
    if (!dirty) return true;
    const r = await riskConfirm({ title: t('放弃未保存的修改？'), lines: [t('配置弹窗里有未保存的修改，关闭后这些修改会丢失。')], okText: t('放弃并关闭'), cancelText: t('继续编辑'), danger: true });
    return r.ok;
  },
};
let guarding = false; // 守卫（会弹确认框）进行中，不重复触发

function openModal(id) {
  const m = $(id);
  if (!m.hidden) return;
  stack.push({ m, back: document.activeElement });
  m.hidden = false;
  const first = [...m.querySelectorAll('input, select, textarea, button')]
    .find((x) => !x.disabled && x.type !== 'hidden' && !x.closest('[hidden]'));
  if (first) first.focus();
}

function closeModal(id, force = false) {
  const m = $(id);
  if (m.hidden) return;
  if (!force && guards[id]) {
    if (guarding) return;
    guarding = true;
    guards[id]().then((ok) => { guarding = false; if (ok) closeModal(id, true); }, () => { guarding = false; });
    return;
  }
  m.hidden = true;
  const i = stack.findIndex((s) => s.m === m);
  if (i < 0) return;
  const [s] = stack.splice(i, 1);
  // 列表重绘后原按钮可能已不在页面上，退而求其次把焦点放到“添加”按钮
  if (s.back && s.back.isConnected) s.back.focus();
  else if (!stack.length) $('ap-new').focus();
}

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && stack.length) {
    e.preventDefault();
    closeModal(stack[stack.length - 1].m.id);
  }
});
for (const m of document.querySelectorAll('.modal-mask')) {
  for (const b of m.querySelectorAll('[data-close]')) b.addEventListener('click', () => closeModal(m.id));
}

// ---- 列表 ----

function emptyRow(text) {
  const tr = el('tr');
  const td = el('td', 'empty', text);
  td.colSpan = 9;
  tr.append(td);
  return tr;
}

// 本地账号密码是内置方式，不在 providers 里，固定画在第一行
function localRow() {
  const tr = el('tr', 'local-row');
  const name = el('td');
  name.append(el('b', '', t('本地账号密码')));
  const st = el('td');
  st.append(el('span', 'st passed', t('启用|status')));
  const ops = el('td', 'row-actions');
  ops.append(button(t('设置'), '', () => openModal('m-prov-local')), el('span', 'muted small', t('不可停用 / 删除')));
  tr.append(el('td'), name, el('td', '', t('内置')), st, el('td', '', t('账号 + 密码表单')), el('td', 'muted', t('不适用')),
    el('td', 'muted', '—'), el('td', 'small', ctx.local_last_login || '—'), ops);
  return tr;
}

function groupsText(p) {
  const a = namesOf(p.auth_groups, ctx.auth_groups);
  const r = namesOf(p.resource_groups, ctx.resource_groups);
  const parts = [];
  if (a.length) parts.push(t('权限组：') + a.join(SEP));
  if (r.length) parts.push(t('资源组：') + r.join(SEP));
  return parts.join(SEP2);
}

function providerRow(p, i, n) {
  const tr = el('tr');
  tr.dataset.id = String(p.id);
  if (p.status === 'error') tr.className = 'lv-warn';

  // 排序：上移 / 下移保证键盘可用；拖动手柄是给鼠标的便捷方式
  const sort = el('td', 'sort-cell');
  const handle = el('span', 'drag-handle', '⋮⋮');
  handle.setAttribute('aria-hidden', 'true');
  handle.title = t('拖动调整顺序');
  const up = button('↑', 'ghost mini', () => move(i, -1));
  up.dataset.move = 'up';
  up.title = t('上移');
  up.setAttribute('aria-label', t('上移「{name}」', { name: p.name }));
  up.disabled = i === 0;
  const down = button('↓', 'ghost mini', () => move(i, 1));
  down.dataset.move = 'down';
  down.title = t('下移');
  down.setAttribute('aria-label', t('下移「{name}」', { name: p.name }));
  down.disabled = i === n - 1;
  sort.append(handle, up, down);
  dnd(tr, handle);

  const name = el('td');
  name.append(el('b', '', p.name));
  const kind = el('td');
  kind.append(el('span', 'badge', tp(p.kind_label) || p.kind));
  const st = el('td');
  const [stText, stCls] = STATUS[p.status] || [p.status, 'aborted'];
  st.append(el('span', 'st ' + stCls, stText));
  if (p.test_stale) st.append(' ', el('span', 'badge warn', t('配置已变更，待重新测试')));

  const btnText = p.kind === 'ldap' ? t('账号密码表单（域账号）') : (p.btn_label || t('使用{name}登录', { name: p.name }));
  const groups = groupsText(p);
  const groupTd = groups ? el('td', 'small', groups) : el('td', 'muted', t('全局默认'));
  const last = p.status === 'error'
    ? el('td', 'small crit', firstLine(p.test_msg) || t('最近一次测试未通过'))
    : el('td', 'small', p.last_login || '—');

  const ops = el('td', 'row-actions');
  ops.append(button(t('配置'), '', () => openConfig(p)), button(t('测试'), '', () => openTest(p)));
  if (p.enabled) {
    ops.append(button(t('停用'), 'ghost danger', () => toggle(p, false)));
  } else {
    ops.append(button(t('启用'), '', () => toggle(p, true)),
      button(t('删除'), 'ghost danger', () => remove(p)));
  }

  tr.append(sort, name, kind, st, el('td', '', btnText), el('td', '', FIRST[p.first_login] || p.first_login), groupTd, last, ops);
  return tr;
}

// 顶部 4 个 KPI：启用中 / 已停用 / 测试通过 / 本地账号密码是否开放
function renderKpis() {
  const list = ctx.providers || [];
  const on = list.filter((p) => p.status === 'enabled').length;
  const off = list.filter((p) => !p.enabled).length;
  const passed = list.filter((p) => p.test_ok === true && !p.test_stale).length;
  const stale = list.filter((p) => p.test_stale).length;
  const bad = list.filter((p) => p.status === 'error').length;
  $('apk-on').textContent = String(on);
  $('apk-off').textContent = String(off);
  $('apk-test').textContent = `${passed} / ${list.length}`;
  $('apk-test-d').textContent = bad ? t('{n} 个异常', { n: bad }) : stale ? t('{n} 个配置已变更，待重新测试', { n: stale }) : '';
  const mode = (ctx.rules && ctx.rules.local_form_mode) || 'collapse';
  $('apk-local').textContent = mode === 'hidden' ? t('仅后门') : t('开放');
  $('apk-local').className = 'kv' + (mode === 'hidden' ? ' warn' : ' ok');
  $('apk-local-d').textContent = mode === 'hidden' ? t('表单已隐藏') : mode === 'always' ? t('始终显示') : t('折叠显示');
}

// 登录页预览：只画“启用且测试通过”的方式，顺序同上表
function renderPreview() {
  const box = $('ap-preview');
  const on = (ctx.providers || []).filter((p) => p.status === 'enabled');
  const ldap = on.some((p) => p.kind === 'ldap');
  const btns = on.filter((p) => p.kind !== 'ldap');
  const mode = (ctx.rules && ctx.rules.local_form_mode) || 'collapse';
  const showForm = ldap || !btns.length || mode !== 'hidden';
  const card = el('div', 'lp-card');
  card.append(el('div', 'lp-title', t('登录控制台')));
  for (const p of btns) card.append(el('div', 'btn lp-btn' + (p === btns[0] ? ' primary' : ''), p.btn_label || t('使用{name}登录', { name: p.name })));
  if (showForm) {
    const f = el('div', 'lp-form' + (btns.length && mode === 'collapse' && !ldap ? ' collapsed' : ''));
    if (btns.length && mode === 'collapse' && !ldap) {
      f.append(el('div', 'small muted', t('▸ 或使用平台账号')));
    } else {
      f.append(el('div', 'lp-input', t('用户名')), el('div', 'lp-input', t('密码')), el('div', 'btn primary lp-btn', t('登录')));
    }
    card.append(f);
  } else {
    card.append(el('div', 'small muted', t('本地账号表单已隐藏')));
  }
  box.replaceChildren(card);
}

function renderList() {
  renderKpis();
  renderPreview();
  const rows = $('ap-rows');
  rows.replaceChildren(localRow());
  const list = ctx.providers;
  if (!list.length) {
    rows.append(emptyRow(t('还没有定义外部登录方式')));
    return;
  }
  list.forEach((p, i) => rows.append(providerRow(p, i, list.length)));
  if (pendingFocus) {
    const tr = rows.querySelector(`tr[data-id="${pendingFocus.id}"]`);
    const other = pendingFocus.which === 'up' ? 'down' : 'up';
    // 移到顶 / 底后本按钮会被禁用，焦点改放到另一个方向的按钮
    const b = tr && (tr.querySelector(`[data-move="${pendingFocus.which}"]:not(:disabled)`) || tr.querySelector(`[data-move="${other}"]`));
    if (b) b.focus();
    pendingFocus = null;
  }
}

async function load() {
  ctx = await api('GET', BASE);
  ctx.providers = ctx.providers || [];
  renderList();
}

// ---- 排序 ----

function move(i, d) {
  const list = ctx.providers;
  const j = i + d;
  if (j < 0 || j >= list.length) return;
  [list[i], list[j]] = [list[j], list[i]];
  pendingFocus = { id: list[j].id, which: d < 0 ? 'up' : 'down' };
  saveOrder();
}

async function saveOrder() {
  renderList();
  try {
    await api('PUT', '/api/v1/settings/auth-order', { ids: ctx.providers.map((p) => p.id) });
    setMsg(t('显示顺序已保存'), 'info');
  } catch (e) {
    setMsg(e.message);
    await load().catch(() => {});
  }
}

function dnd(tr, handle) {
  // 只有按住手柄时整行才可拖，避免影响行内文字选择
  handle.addEventListener('mousedown', () => { tr.draggable = true; });
  handle.addEventListener('mouseup', () => { tr.draggable = false; });
  tr.addEventListener('dragstart', (e) => {
    dragId = Number(tr.dataset.id);
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', tr.dataset.id);
    tr.classList.add('dragging');
  });
  tr.addEventListener('dragend', () => {
    tr.draggable = false;
    tr.classList.remove('dragging');
    dragId = null;
  });
  tr.addEventListener('dragover', (e) => {
    if (dragId === null) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
  });
  tr.addEventListener('drop', (e) => {
    e.preventDefault();
    const target = Number(tr.dataset.id);
    if (dragId === null || dragId === target) return;
    const list = ctx.providers;
    const from = list.findIndex((p) => p.id === dragId);
    const to = list.findIndex((p) => p.id === target);
    if (from < 0 || to < 0) return;
    const [it] = list.splice(from, 1);
    list.splice(to, 0, it);
    saveOrder();
  });
}

// ---- 启停 / 删除 ----

const lastTestText = (p) => {
  if (!p.test_time) return t('从未测试');
  const time = p.test_time;
  if (p.test_ok) return p.test_stale ? t('{time} 通过（配置已变更，待重新测试）', { time }) : t('{time} 通过', { time });
  return p.test_stale ? t('{time} 失败（配置已变更，待重新测试）', { time }) : t('{time} 失败', { time });
};
const providerDetails = (p) => [
  { k: t('名称'), v: p.name }, { k: t('类型'), v: tp(p.kind_label) || p.kind },
  { k: t('最近测试'), v: lastTestText(p) },
];

async function toggle(p, on) {
  setMsg('');
  const r = await confirmRun(on
    ? {
      title: t('启用登录方式「{name}」', { name: p.name }), code: 'authp.toggle',
      lines: [p.kind === 'ldap'
        ? t('启用后登录页的账号密码表单支持用域账号登录，立即生效，无需重启。')
        : t('启用后登录页立即出现「{label}」按钮，点击会跳转到身份提供方的授权页，无需重启。', { label: p.btn_label || t('使用{name}登录', { name: p.name }) }),
      t('首次登录策略：{name}。', { name: FIRST[p.first_login] || p.first_login }),
      ...(p.test_ok === true && !p.test_stale ? [] : [t('该登录方式还没有测试通过（或配置已变更待重新测试），服务端会拒绝启用，请先测试。')])],
      details: providerDetails(p), okText: t('确认启用'),
    }
    : {
      title: t('停用登录方式「{name}」', { name: p.name }), code: 'authp.toggle', level: 'high',
      lines: [t('停用后登录页不再显示「{name}」，通过它登录的用户将无法再用这种方式登录，需改用其他方式，或由管理员重置为本地密码。', { name: p.name }),
        t('本地账号密码登录不受影响。可以随时重新启用。')],
      details: providerDetails(p), okText: t('确认停用'),
    }, () => api('POST', `${BASE}/${p.id}/${on ? 'enable' : 'disable'}`));
  if (!r.ok) return;
  try {
    await load();
    setMsg(on ? t('已启用「{name}」', { name: p.name }) : t('已停用「{name}」', { name: p.name }), 'info');
  } catch (e) {
    setMsg(e.message);
  }
}

async function remove(p) {
  setMsg('');
  let r = await confirmRun({
    title: t('删除登录方式「{name}」', { name: p.name }), code: 'authp.toggle', level: 'high',
    lines: [t('删除后配置与密钥一并清除，不可恢复。')],
    details: providerDetails(p), okText: t('确认删除'),
  }, async () => {
    try {
      await api('DELETE', `${BASE}/${p.id}`);
    } catch (e) {
      // 有用户通过它登录过：关闭本框，把后端给的说明交给管理员再确认一次
      if (e.status === 409 && e.code === 'in_use') return { inUse: e.message };
      throw e;
    }
    return {};
  });
  if (!r.ok) return;
  if (r.result.inUse) {
    r = await confirmRun({
      title: t('仍然删除「{name}」？', { name: p.name }), code: 'authp.toggle', level: 'high',
      lines: [r.result.inUse], details: providerDetails(p), okText: t('仍然删除'),
    }, () => api('DELETE', `${BASE}/${p.id}?confirm=1`));
    if (!r.ok) return;
  }
  try {
    await load();
    setMsg(t('已删除「{name}」', { name: p.name }), 'info');
  } catch (e) {
    setMsg(e.message);
  }
}

// ---- 添加 ----

$('ap-new').addEventListener('click', () => {
  if (!ctx) return;
  $('pa-kind').replaceChildren(...(ctx.kinds || []).map((k) => new Option(tp(k.label), k.value)));
  $('pa-name').value = '';
  msgInto($('pa-msg'), '');
  openModal('m-prov-add');
});

async function create(btn) {
  const name = $('pa-name').value.trim();
  if (!name) {
    msgInto($('pa-msg'), t('请填写名称'));
    $('pa-name').focus();
    return;
  }
  await busy(btn, async () => {
    try {
      const v = await api('POST', BASE, { kind: $('pa-kind').value, name });
      closeModal('m-prov-add');
      await load();
      openConfig(v);
    } catch (e) {
      msgInto($('pa-msg'), e.message);
    }
  });
}
$('pa-next').addEventListener('click', (e) => create(e.currentTarget));
$('pa-name').addEventListener('keydown', (e) => { if (e.key === 'Enter') create($('pa-next')); });

// ---- 配置弹窗 ----

const dlg = $('m-prov');
const cfgInputs = () => [...dlg.querySelectorAll('[data-cfg]')];

function fillSecret(inp, has) {
  inp.value = '';
  inp.disabled = false;
  inp.placeholder = has ? t('已配置，留空不修改') : t('加密存储，不回显');
  const label = inp.closest('label');
  let btn = label.querySelector('.secret-clear');
  if (!btn) {
    // 按钮放在 label 里，输入框的可访问名称单独指定，免得把按钮文字也读进去
    inp.setAttribute('aria-label', label.firstChild.textContent.trim());
    btn = button(t('清除已保存的密钥'), 'ghost secret-clear');
    btn.addEventListener('click', () => toggleClear(inp, btn));
    label.append(btn);
  }
  btn.textContent = t('清除已保存的密钥');
  btn.hidden = !has;
}

function toggleClear(inp, btn) {
  const k = inp.dataset.cfg;
  dirty = true;
  if (clearSecrets.has(k)) {
    clearSecrets.delete(k);
    inp.disabled = false;
    inp.placeholder = t('已配置，留空不修改');
    btn.textContent = t('清除已保存的密钥');
  } else {
    clearSecrets.add(k);
    inp.value = '';
    inp.disabled = true;
    inp.placeholder = t('保存后清除');
    btn.textContent = t('撤销清除');
  }
}

function fillInput(inp, cfg, has) {
  const k = inp.dataset.cfg;
  const v = cfg[k];
  if (inp.type === 'checkbox') {
    // skip_verify 在界面上显示成“校验证书”，勾选含义相反
    inp.checked = 'invert' in inp.dataset ? !v : !!v;
  } else if ('secret' in inp.dataset) {
    fillSecret(inp, !!has[k]);
  } else if (inp.tagName === 'SELECT') {
    inp.value = v || '';
    if (inp.selectedIndex < 0) inp.selectedIndex = 0; // 未配置时取第一项（即默认值）
  } else {
    inp.value = v === undefined || v === null || v === 0 ? '' : String(v);
  }
}

function addRule(r = {}) {
  const tr = el('tr');
  const attr = el('input', 'mono');
  attr.type = 'text';
  attr.value = r.attr || '';
  attr.placeholder = 'groups / memberOf / department';
  attr.dataset.r = 'attr';
  attr.setAttribute('aria-label', t('外部属性名'));
  const op = el('select');
  op.dataset.r = 'op';
  op.setAttribute('aria-label', t('比较方式'));
  op.append(new Option(t('等于'), 'eq'), new Option(t('包含'), 'contains'));
  op.value = r.op === 'contains' ? 'contains' : 'eq';
  const val = el('input');
  val.type = 'text';
  val.value = r.value || '';
  val.placeholder = t('如 dba-team');
  val.dataset.r = 'value';
  val.setAttribute('aria-label', t('比较值'));
  const ag = el('select');
  ag.multiple = true;
  ag.size = 3;
  ag.dataset.r = 'ag';
  ag.setAttribute('aria-label', t('加入的权限组'));
  fillMulti(ag, ctx.auth_groups, r.auth_groups);
  const rg = el('select');
  rg.multiple = true;
  rg.size = 3;
  rg.dataset.r = 'rg';
  rg.setAttribute('aria-label', t('加入的资源组'));
  fillMulti(rg, ctx.resource_groups, r.resource_groups);
  const rm = button(t('删除'), 'ghost danger', () => { tr.remove(); dirty = true; });
  rm.setAttribute('aria-label', t('删除这条入组规则'));
  for (const c of [attr, op, val, ag, rg, rm]) {
    const td = el('td');
    td.append(c);
    tr.append(td);
  }
  $('pv-rules').append(tr);
  return attr;
}

function collectRules() {
  return [...$('pv-rules').children].map((tr) => ({
    attr: tr.querySelector('[data-r="attr"]').value.trim(),
    op: tr.querySelector('[data-r="op"]').value,
    value: tr.querySelector('[data-r="value"]').value.trim(),
    auth_groups: picked(tr.querySelector('[data-r="ag"]')),
    resource_groups: picked(tr.querySelector('[data-r="rg"]')),
  })).filter((r) => r.attr || r.value); // 整行空着的当作没加
}

function collectConfig() {
  // 以服务端返回的配置为底，保留界面上没有展示的字段（如 CAS 的 global_logout）
  const cfg = { ...(cur.config || {}) };
  for (const inp of cfgInputs()) {
    if (inp.closest('[hidden]')) continue; // 其他类型的字段
    const k = inp.dataset.cfg;
    if (inp.type === 'checkbox') {
      cfg[k] = 'invert' in inp.dataset ? !inp.checked : inp.checked;
    } else if ('secret' in inp.dataset) {
      if (inp.value) cfg[k] = inp.value; else delete cfg[k]; // 留空 = 不修改
    } else if ('num' in inp.dataset) {
      const s = inp.value.trim();
      if (s && !/^\d+$/.test(s)) throw new Error(t('超时必须是正整数（秒），留空表示默认 10 秒'));
      cfg[k] = s ? Number(s) : 0;
    } else {
      cfg[k] = inp.value.trim();
    }
  }
  cfg.group_rules = collectRules();
  // 匹配依据：沿用全局时不带 match_by；自己设置时至少选一种
  if ($('pv-match-own').checked) {
    const by = MATCH_ORDER.filter((v) => $('pv-match').querySelector(`input[value="${v}"]`).checked);
    if (!by.length) throw new Error(t('请至少选择一种匹配依据，或改为“沿用通用规则里的全局设置”'));
    cfg.match_by = by;
  } else {
    delete cfg.match_by;
  }
  return cfg;
}

// 匹配依据区：config.match_by 为空 = 沿用全局
function fillMatch(cfg) {
  const own = Array.isArray(cfg.match_by) && cfg.match_by.length > 0;
  $('pv-match-own').checked = own;
  $('pv-match-global').checked = !own;
  const glob = ((ctx.rules && ctx.rules.match_by) || []).map((m) => MATCH_TEXT[m] || m);
  $('pv-match-global-text').textContent = glob.length ? t('（当前：{list}）', { list: glob.join(SEP) }) : t('（当前：不按任何依据匹配，只认已绑定的外部身份）');
  const set = new Set(own ? cfg.match_by : (ctx.rules && ctx.rules.match_by) || []);
  for (const cb of $('pv-match').querySelectorAll('input[type="checkbox"]')) cb.checked = set.has(cb.value);
  syncMatch();
}

function syncMatch() {
  const own = $('pv-match-own').checked;
  for (const cb of $('pv-match').querySelectorAll('input[type="checkbox"]')) cb.disabled = !own;
}

function openConfig(p) {
  cur = p;
  clearSecrets.clear();
  const kind = p.kind;
  $('pv-title').textContent = `${p.name} · ${tp(p.kind_label) || kind}`;
  $('pv-name').value = p.name;
  $('pv-btn').value = p.btn_label || '';
  $('pv-btn').placeholder = t('使用{name}登录', { name: p.name });
  $('pv-btn-box').hidden = kind === 'ldap'; // LDAP 走账号密码表单，没有单独按钮
  $('pv-first').value = p.first_login === 'create' ? 'create' : 'existing';
  fillMulti($('pv-ag'), ctx.auth_groups, p.auth_groups);
  fillMulti($('pv-rg'), ctx.resource_groups, p.resource_groups);
  for (const sec of dlg.querySelectorAll('.prov-fields')) sec.hidden = sec.dataset.kind !== kind;
  const cfg = p.config || {};
  const has = p.has_secrets || {};
  for (const inp of cfgInputs()) fillInput(inp, cfg, has);
  $('pv-backfill-box').hidden = !BACKFILL_TEXT[kind];
  $('pv-backfill-text').textContent = BACKFILL_TEXT[kind] || '';
  $('pv-cb-box').hidden = !p.callback_url;
  $('pv-cb').value = p.callback_url || '';
  const def = DEFAULT_ATTRS[kind] || ['', '', ''];
  $('pv-attr-user').placeholder = def[0];
  $('pv-attr-display').placeholder = def[1];
  $('pv-attr-email').placeholder = def[2];
  $('pv-attr-phone').placeholder = DEFAULT_PHONE[kind] || '';
  fillMatch(cfg);
  $('pv-rules').replaceChildren();
  for (const r of cfg.group_rules || []) addRule(r);
  $('pv-test').textContent = kind === 'ldap' ? t('测试连接') : t('测试登录');
  msgInto($('pv-msg'), '');
  dirty = false;
  openModal('m-prov');
}

for (const id of ['pv-match-global', 'pv-match-own']) $(id).addEventListener('change', syncMatch);
dlg.addEventListener('input', () => { dirty = true; });
dlg.addEventListener('change', () => { dirty = true; });
$('pv-add-rule').addEventListener('click', () => { addRule().focus(); dirty = true; });

$('pv-cb-copy').addEventListener('click', async () => {
  const inp = $('pv-cb');
  try {
    await navigator.clipboard.writeText(inp.value);
    msgInto($('pv-msg'), t('回调地址已复制'), 'info');
  } catch (_) {
    // 非 https 页面没有 clipboard 接口：选中文字让用户自己复制
    inp.select();
    msgInto($('pv-msg'), t('浏览器不允许自动复制，已选中地址，请按 Ctrl+C 复制'), 'warn');
  }
});

// ---- 保存前的差异 ----

const onOff = (v) => (v ? t('开启') : t('关闭|off'));

// 输入框对应的显示名：优先 <label for>，其次所在 label 自己的文字（不含里面的选项 / 按钮文字）
function inputLabel(inp) {
  const lb = (inp.id && document.querySelector(`label[for="${inp.id}"]`)) || inp.closest('label');
  if (!lb) return inp.dataset.cfg;
  const own = [...lb.childNodes].filter((n) => n.nodeType === 3).map((n) => n.textContent).join('').trim();
  return own || lb.textContent.trim() || inp.dataset.cfg;
}

const matchText = (list, none = t('沿用通用规则的全局设置')) => (list && list.length ? MATCH_ORDER.filter((v) => list.includes(v)).map((v) => MATCH_TEXT[v]).join(SEP) : none);
const ruleText = (r) => t('{attr} {op} {value} → 权限组[{ag}] 资源组[{rg}]', {
  attr: r.attr, op: r.op === 'contains' ? t('包含') : t('等于'), value: r.value,
  ag: namesOf(r.auth_groups, ctx.auth_groups).join(SEP), rg: namesOf(r.resource_groups, ctx.resource_groups).join(SEP),
});
const rulesText = (list) => (list && list.length ? list.map(ruleText).join('\n') : t('（无）'));

// 对比服务端当前配置与表单：返回 [{label, from, to}]（相同的由 diffConfirm 自动过滤）；密钥只写“已修改 / 清除”，不含值
function configRows(body) {
  const rows = [];
  const add = (label, from, to) => rows.push({ label, from, to });
  add(t('名称'), cur.name, body.name);
  add(t('登录页按钮文字'), cur.btn_label || '', body.btn_label);
  add(t('首次登录策略'), FIRST[cur.first_login] || cur.first_login, FIRST[body.first_login] || body.first_login);
  add(t('默认权限组'), namesOf(cur.auth_groups, ctx.auth_groups).join(SEP), namesOf(body.auth_groups, ctx.auth_groups).join(SEP));
  add(t('默认资源组'), namesOf(cur.resource_groups, ctx.resource_groups).join(SEP), namesOf(body.resource_groups, ctx.resource_groups).join(SEP));
  const old = cur.config || {};
  const has = cur.has_secrets || {};
  for (const inp of cfgInputs()) {
    if (inp.closest('[hidden]')) continue; // 其他类型的字段
    const k = inp.dataset.cfg;
    const label = inputLabel(inp);
    if (inp.type === 'checkbox') {
      add(label, onOff('invert' in inp.dataset ? !old[k] : !!old[k]), onOff(inp.checked));
    } else if ('secret' in inp.dataset) {
      if (clearSecrets.has(k)) add(label, t('已配置'), t('清除'));
      else if (inp.value) add(label, has[k] ? t('已配置') : t('（未设置）'), t('已修改'));
    } else if (inp.tagName === 'SELECT') {
      const was = [...inp.options].find((o) => o.value === String(old[k] || '')) || inp.options[0];
      add(label, was ? was.textContent : '', inp.selectedOptions[0] ? inp.selectedOptions[0].textContent : '');
    } else {
      const was = old[k] === undefined || old[k] === null || old[k] === 0 ? '' : String(old[k]);
      add(label, was, body.config[k] === 0 ? '' : String(body.config[k] ?? inp.value.trim()));
    }
  }
  add(t('入组规则'), rulesText(old.group_rules), rulesText(body.config.group_rules));
  add(t('匹配依据'), matchText(old.match_by), matchText(body.config.match_by));
  return rows;
}

// 保存配置（含确认）。返回：null = 用户取消；{noChange:true} = 没有变更；否则是服务端返回（含 provider / notice）。校验错误以异常抛出。
async function saveConfig() {
  const name = $('pv-name').value.trim();
  if (!name) throw new Error(t('请填写名称'));
  const body = {
    name,
    btn_label: $('pv-btn').value.trim(),
    first_login: $('pv-first').value,
    auth_groups: picked($('pv-ag')),
    resource_groups: picked($('pv-rg')),
    config: collectConfig(),
  };
  if (clearSecrets.size) body.clear_secrets = [...clearSecrets];
  const target = cur;
  const lines = [t('配置保存即生效，无需重启。密钥只显示“已修改”，审计里也不记录密钥内容。')];
  if (target.enabled) lines.push(t('若修改了连接相关的参数，该登录方式会被自动停用，之前的测试结果作废，需重新测试通过后才能启用。'));
  const r = await confirmRun({
    title: t('保存登录方式「{name}」配置', { name: target.name }), code: 'authp.edit', level: 'high', lines,
    details: [{ k: t('类型'), v: tp(target.kind_label) || target.kind }, { k: t('当前状态'), v: (STATUS[target.status] || [target.status])[0] }],
    rows: configRows(body), okText: t('确认保存'),
  }, () => api('PUT', `${BASE}/${target.id}`, body));
  if (r.noChange) return { noChange: true };
  if (!r.ok) return null;
  const d = r.result;
  cur = d.provider;
  dirty = false;
  clearSecrets.clear();
  return d;
}

$('pv-save').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  msgInto($('pv-msg'), '');
  let d;
  try {
    d = await saveConfig();
  } catch (err) {
    msgInto($('pv-msg'), err.message);
    return;
  }
  if (!d) return; // 在确认框里取消：什么都不发，弹窗保持原样
  closeModal('m-prov', true);
  if (d.noChange) { dirty = false; setMsg(t('没有变更'), 'info'); return; }
  if (d.notice) setMsg(d.notice, 'warn');
  else setMsg(t('已保存「{name}」', { name: d.provider.name }), 'info');
  await load().catch((err) => setMsg(err.message));
}));

// 测试用的是已保存的配置，所以有改动时先保存再测
$('pv-test').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  msgInto($('pv-msg'), '');
  try {
    let notice = '';
    if (dirty) {
      const d = await saveConfig();
      if (!d) return; // 取消保存：不测试
      if (d.noChange) { dirty = false; } else {
        notice = d.notice || '';
        fillSecretsAfterSave();
        load().catch((er) => setMsg(er.message));
      }
    }
    openTest(cur, notice);
  } catch (err) {
    msgInto($('pv-msg'), err.message);
  }
}));

// 保存后密钥输入框要清空并按新状态显示占位，否则再次保存会把同一个密钥重复提交
function fillSecretsAfterSave() {
  const has = cur.has_secrets || {};
  for (const inp of dlg.querySelectorAll('[data-secret]')) fillSecret(inp, !!has[inp.dataset.cfg]);
}

// ---- 测试弹窗 ----

function showLines(box, lines, kind) {
  const d = el('div', 'alert ' + kind + ' test-lines');
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  for (const line of lines) d.append(el('div', '', line));
  box.replaceChildren(d);
}

function showLast(p) {
  const box = $('pt-last');
  box.replaceChildren();
  if (!p.test_time) {
    box.textContent = t('最近一次测试：从未测试');
    return;
  }
  box.append(t('最近一次测试：{time}', { time: p.test_time }) + ' ');
  box.append(p.test_ok ? el('span', 'st passed', t('通过')) : el('span', 'st rejected', t('失败')));
  if (p.test_ok === false && p.test_msg) box.append(' ', el('span', 'crit', firstLine(p.test_msg)));
  if (p.test_stale) box.append(' ', el('span', 'badge warn', t('配置已变更，待重新测试')));
}

function openTest(p, notice) {
  testing = p;
  const ldap = p.kind === 'ldap';
  $('pt-title').textContent = `${p.name} · ${tp(p.kind_label) || p.kind}`;
  $('pt-ldap').hidden = !ldap;
  $('pt-open').hidden = ldap;
  $('pt-refresh').hidden = ldap;
  $('pt-run').textContent = ldap ? t('开始测试') : t('测试连接');
  $('pt-user').value = '';
  $('pt-pass').value = '';
  showLast(p);
  msgInto($('pt-note'), notice, 'warn');
  $('pt-result').replaceChildren();
  openModal('m-prov-test');
  if (!ldap) runTest(); // 非 LDAP 没有要填的东西，直接做一次元数据 / 凭据探测
}

async function runTest() {
  const p = testing;
  if (!p) return;
  const body = p.kind === 'ldap' ? { username: $('pt-user').value.trim(), password: $('pt-pass').value } : {};
  $('pt-pass').value = ''; // 密码只用这一次，不留在页面上
  showLines($('pt-result'), [t('测试中…')], 'info');
  await busy($('pt-run'), async () => {
    try {
      const d = await api('POST', `${BASE}/${p.id}/test`, body);
      const rep = d.report || { ok: false, lines: [] };
      const lines = rep.lines && rep.lines.length ? rep.lines : [rep.ok ? t('✔ 测试通过') : t('✖ 测试未通过')];
      showLines($('pt-result'), lines, rep.ok ? 'info' : 'err');
      if (d.provider) {
        testing = d.provider;
        showLast(d.provider);
        if (cur && cur.id === d.provider.id) cur = d.provider;
      }
      load().catch((e) => setMsg(e.message));
    } catch (e) {
      showLines($('pt-result'), [e.message], 'err');
    }
  });
}

$('pt-run').addEventListener('click', runTest);
$('pt-pass').addEventListener('keydown', (e) => { if (e.key === 'Enter') runTest(); });

$('pt-open').addEventListener('click', () => {
  if (!testing) return;
  window.open('/login/sso/' + testing.id + '?test=1', '_blank', 'noopener');
  showLines($('pt-result'), [t('已在新窗口打开授权页。在新窗口完成授权后回到这里点“刷新结果”。')], 'info');
});

$('pt-refresh').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  if (!testing) return;
  try {
    const p = await api('GET', `${BASE}/${testing.id}`);
    testing = p;
    showLast(p);
    if (p.test_time) {
      const lines = String(p.test_msg || '').split('\n').filter((s) => s.trim());
      showLines($('pt-result'), lines.length ? lines : [p.test_ok ? t('✔ 测试通过') : t('✖ 测试未通过')], p.test_ok ? 'info' : 'err');
    } else {
      showLines($('pt-result'), [t('还没有测试结果。')], 'warn');
    }
    load().catch((er) => setMsg(er.message));
  } catch (err) {
    showLines($('pt-result'), [err.message], 'err');
  }
}));

// ---- 通用规则 ----

function fillRules(r) {
  $('ap-local-mode').value = r.local_form_mode || 'collapse';
  if ($('ap-local-mode').selectedIndex < 0) $('ap-local-mode').value = 'collapse';
  const set = new Set(r.match_by || []);
  for (const cb of $('ap-match').querySelectorAll('input[type="checkbox"]')) cb.checked = set.has(cb.value);
  $('ap-su-local').checked = !!r.superuser_local_only;
  $('ap-ext-2fa').checked = !!r.external_2fa;
  $('ap-backfill').checked = !!r.backfill_ids;
  $('ap-self-bind').checked = !!r.self_bind;
  $('ap-rules-save').disabled = false;
}

const LOCAL_MODE_TEXT = { always: t('始终显示'), collapse: t('折叠'), hidden: t('隐藏（仅 /login?local=1 可用）') };

$('ap-rules-save').addEventListener('click', async (e) => {
  setMsg('');
  const body = {
    local_form_mode: $('ap-local-mode').value,
    match_by: [...$('ap-match').querySelectorAll('input:checked')].map((c) => c.value),
    superuser_local_only: $('ap-su-local').checked,
    external_2fa: $('ap-ext-2fa').checked,
    backfill_ids: $('ap-backfill').checked,
    self_bind: $('ap-self-bind').checked,
  };
  const old = ctx.rules || {};
  const lines = [t('通用规则影响所有登录方式，保存后立即生效（无需重启）。')];
  if (old.superuser_local_only && !body.superuser_local_only) lines.push(t('已取消“超级用户只能本地登录”：外部身份匹配到超级用户时将允许登录，请确认身份提供方的账号安全。'));
  if (old.external_2fa && !body.external_2fa) lines.push(t('关闭“外部登录后仍要求两步验证”：外部登录成功后不再校验动态码。'));
  const r = await confirmRun({
    title: t('保存登录通用规则'), code: 'authp.edit', level: 'high', lines,
    rows: [
      { label: t('本地账号密码表单'), from: LOCAL_MODE_TEXT[old.local_form_mode] || old.local_form_mode, to: LOCAL_MODE_TEXT[body.local_form_mode] || body.local_form_mode },
      { label: t('匹配依据'), from: matchText(old.match_by, t('（无）')), to: matchText(body.match_by, t('（无）')) },
      { label: t('超级用户只能本地登录'), from: onOff(old.superuser_local_only), to: onOff(body.superuser_local_only) },
      { label: t('外部登录后仍要求两步验证'), from: onOff(old.external_2fa), to: onOff(body.external_2fa) },
      { label: t('回填第三方 ID'), from: onOff(old.backfill_ids), to: onOff(body.backfill_ids) },
      { label: t('允许用户自行绑定'), from: onOff(old.self_bind), to: onOff(body.self_bind) },
    ],
    okText: t('确认保存'),
  }, () => api('PUT', '/api/v1/settings/auth-rules', body));
  if (r.noChange) { toast(t('没有变更'), 'warn'); return; }
  if (!r.ok) return;
  ctx.rules = r.result;
  fillRules(r.result);
  renderKpis();
  renderPreview();
  setMsg(t('通用规则已保存'), 'info');
});

// 通用规则只在首次加载时填，之后列表刷新不覆盖管理员正在改的勾选
load()
  .then(() => fillRules(ctx.rules || {}))
  .catch((e) => {
    setMsg(e.message);
    $('ap-rows').replaceChildren(emptyRow(t('加载失败：') + e.message));
  });
