/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: resource_groups.js
 * @time: 2026/10/28
 */
// 资源组管理：KPI、列表（负责人 / 备注 / 搜索）、新建/编辑（通知请到“通知渠道”配置；还配着旧版 Webhook 的组可以在这里清除）、停用（被拒时列出进行中的工单）/恢复/彻底删除、
// 关联用户与实例（穿梭框：暂存后统一保存，显示 +n −m 与审批流新断点）。仅超级用户。
import { api, $, el, button, msgBox, busy, emptyRow, modal, tabs, qs, status, badge, riskConfirm, confirmRun } from './ui.js';
import { t, tp } from '../i18n.js';

const API = '/api/v1/settings/resource-groups';
const setMsg = msgBox($('rg-msg'));
const setEditMsg = msgBox($('re-msg'));
const setRelMsg = msgBox($('rel-msg'));
const editModal = modal('m-res-edit');
const busyModal = modal('m-res-busy');

let editing = null; // 正在编辑的资源组详情；null 表示新建
let rel = null; // 当前关联对象数据（服务端状态）
let relKind = 'users';
let relGroup = null;
let stage = null; // 暂存的关联变更：{ users: {add:Set, remove:Set}, instances: {...} }
let leadersLoaded = false;
let previewTimer = 0;
let previewSeq = 0;

// ---- KPI ----

function kpi(label, value, sub, cls) {
  const d = el('div');
  d.append(el('b', cls || '', String(value)), el('span', '', label));
  if (sub) d.append(el('span', 'small', sub));
  return d;
}

async function loadKpis() {
  const s = await api('GET', API + '/summary');
  $('rg-kpis').replaceChildren(
    kpi(t('启用中的资源组'), s.enabled, t('另有 {n} 个已停用', { n: s.disabled }), 'ok'),
    kpi(t('没有实例的组'), s.no_instance, '', s.no_instance ? 'warn' : ''),
    kpi(t('审批流不完整'), s.flow_incomplete, '', s.flow_incomplete ? 'warn' : ''),
    kpi(t('未配置机器人'), s.no_bot, '', s.no_bot ? 'warn' : ''),
  );
}

// ---- 列表 ----

// 通知群：把资源组上的旧版 Webhook 和“通知渠道”里对该组生效的渠道（含全局渠道）合起来看。
// 有渠道的标个数（钉钉 ×2），只有旧版 Webhook 的标“旧版”，都没有的显示“未配”；企业微信 / Lark / Telegram / 邮件只在配了时才显示。
const CHAN_TYPES = [['dingtalk', 'ding_set'], ['wecom', ''], ['feishu', 'feishu_set'], ['lark', ''], ['slack', 'slack_set'], ['telegram', ''], ['email', '']];
const CHAN_NAME = { dingtalk: () => t('钉钉'), wecom: () => t('企业微信'), feishu: () => t('飞书'), lark: () => 'Lark', slack: () => 'Slack', telegram: () => 'Telegram', email: () => t('邮件') };

function chanBadge(typ, n, legacy) {
  const name = CHAN_NAME[typ]();
  const tips = [];
  if (n) tips.push(t('通知渠道 {n} 个（含全局渠道）', { n }));
  if (legacy) tips.push(t('资源组上的旧版 Webhook 已配置'));
  const b = badge(n ? t('{name} ×{n}', { name, n }) : legacy ? t('{name}（旧版）', { name }) : t('{name} 未配', { name }), n || legacy ? 'ok' : '');
  if (tips.length) b.title = tips.join(' · ');
  return b;
}

function hooks(g) {
  const box = el('span');
  const ch = g.channels || {};
  for (const [typ, key] of CHAN_TYPES) {
    const n = ch[typ] || 0;
    const legacy = key ? !!g[key] : false;
    if (!n && !legacy && (typ === 'wecom' || typ === 'lark' || typ === 'telegram' || typ === 'email')) continue;
    box.append(chanBadge(typ, n, legacy), document.createTextNode(' '));
  }
  return box;
}

// 编辑框里的一句话：这个组现在有哪些通知渠道（含全局渠道）
function chanNote(g) {
  const ch = g.channels || {};
  const parts = CHAN_TYPES.filter(([typ]) => ch[typ]).map(([typ]) => t('{name} ×{n}', { name: CHAN_NAME[typ](), n: ch[typ] }));
  return parts.length ? t('该资源组已有的通知渠道：{list}（含全局渠道）', { list: parts.join('、') }) : t('该资源组还没有配置通知渠道。');
}

function render(list) {
  const body = $('rg-rows');
  if (!list.length) return emptyRow(body, 9, t('没有符合条件的资源组。'));
  body.replaceChildren();
  for (const g of list) {
    const tr = el('tr');
    if (!g.deleted && (g.instances === 0 || g.flows_configured < g.flows_total)) tr.className = 'lv-warn';
    const ops = el('td', 'row-actions');
    if (g.deleted) {
      ops.append(button(t('恢复'), '', () => restore(g)), ' ',
        button(t('彻底删除'), 'ghost danger', () => openPurge(g)));
    } else {
      ops.append(button(t('编辑'), '', () => openEdit(g.id)), ' ', button(t('关联对象'), '', () => openRel(g)), ' ',
        button(t('停用'), 'ghost danger', () => disable(g)));
    }
    const nm = el('td', g.deleted ? 'muted' : '');
    nm.append(el('b', '', g.name));
    if (g.remark) nm.append(el('div', 'small muted', g.remark));
    const lead = el('td', g.leader ? '' : 'muted');
    if (g.leader) { lead.append(document.createTextNode(g.leader_display || g.leader)); if (g.leader_display) lead.append(el('div', 'small muted mono', g.leader)); } else lead.textContent = '—';
    tr.append(el('td', 'num', String(g.id)), nm, lead, el('td', 'num', String(g.users)), el('td', 'num', String(g.instances)),
      el('td', g.deleted ? 'muted' : '', g.deleted ? '—' : t('{done} / {total} 已配置', { done: g.flows_configured, total: g.flows_total })));
    const hk = el('td');
    if (g.deleted) hk.textContent = '—'; else hk.append(hooks(g));
    const st = el('td');
    st.append(g.deleted ? status(t('已停用'), 'aborted') : status(t('正常'), 'passed'));
    tr.append(hk, st, ops);
    body.append(tr);
  }
}

async function load() {
  const list = await api('GET', API + qs({ search: $('rg-search').value.trim(), deleted: $('rg-deleted').checked ? '1' : '' }));
  render(list);
  loadKpis().catch(() => {});
}

async function restore(g) {
  const r = await confirmRun({
    title: t('恢复资源组'), code: 'rg.delete',
    lines: [t('恢复后该组的成员重新看到其实例，审批流重新生效，可以再提交工单。')],
    details: [{ k: t('资源组'), v: g.name }, { k: t('编号'), v: `#${g.id}` }],
    okText: t('确认恢复'),
  }, () => api('POST', `${API}/${g.id}/restore`));
  if (!r.ok) return;
  setMsg('');
  try {
    await load();
    setMsg(t('已恢复'), 'info');
  } catch (e) { setMsg(e.message); }
}

// ---- 停用：被进行中的工单拦下时列出明细 ----

async function disable(g) {
  const r = await confirmRun({
    title: t('停用资源组'), code: 'rg.delete', level: 'high',
    lines: [t('停用后用户通过该组获得的实例可见范围会失效，审批流不再生效，也不能再提交工单。'), t('有进行中的工单时不允许停用。这是软删除，可以随时恢复。')],
    details: [{ k: t('资源组'), v: g.name }, { k: t('编号'), v: `#${g.id}` }, { k: t('成员 / 实例'), v: t('{users} 个用户 / {instances} 个实例', { users: g.users, instances: g.instances }) }],
    okText: t('确认停用'),
  }, async () => {
    try {
      await api('DELETE', `${API}/${g.id}`);
    } catch (e) {
      // 被进行中的工单拦下：关闭确认框，改弹明细窗口（不是“失败后重试”能解决的）
      const list = e.data && e.data.open_workflows;
      if (e.code === 'conflict' && Array.isArray(list) && list.length) return { blocked: { e, list } };
      throw e;
    }
    return {};
  });
  if (!r.ok) return;
  setMsg('');
  if (r.result.blocked) { showBusy(g, r.result.blocked.e, r.result.blocked.list); return; }
  try {
    await load();
    setMsg(t('已停用'), 'info');
  } catch (e) { setMsg(e.message); }
}

function showBusy(g, e, list) {
  $('m-busy-title').textContent = t('无法停用资源组 · {name}', { name: g.name });
  const total = (e.data && e.data.total) || list.length;
  $('busy-sum').textContent = t('该资源组还有 {n} 个进行中的工单，需先结束或终止后才能停用。', { n: total }) + (total > list.length ? t('下面列出前 {n} 个。', { n: list.length }) : '');
  const body = $('busy-rows');
  body.replaceChildren();
  for (const w of list) {
    const tr = el('tr');
    const name = el('td');
    if (w.href) {
      const a = el('a', '', `#${w.id}`);
      a.href = w.href;
      a.target = '_blank';
      a.rel = 'noopener';
      name.append(a, document.createTextNode(' ' + (w.name || '')));
    } else name.textContent = `#${w.id} ${w.name || ''}`;
    const st = el('td');
    st.append(status(tp(w.status), 'waiting'));
    tr.append(el('td', '', tp(w.label)), name, st);
    body.append(tr);
  }
  busyModal.open();
}

// ---- 彻底删除 ----

// 彻底删除：必须原样键入资源组名称（服务端同样校验 confirm_name）
async function openPurge(g) {
  const r = await confirmRun({
    title: t('彻底删除资源组'), code: 'rg.delete', level: 'high',
    lines: [t('彻底删除会从系统中永久删除该资源组及其审批流配置，无法恢复。'),
      t('只有已停用且没有任何引用的组才能删除。')],
    details: [{ k: t('资源组'), v: g.name }, { k: t('编号'), v: `#${g.id}` }],
    typed: g.name, typedLabel: t('请输入资源组名称确认：'),
    okText: t('彻底删除'),
  }, () => api('DELETE', `${API}/${g.id}/purge`, { confirm_name: g.name }));
  if (!r.ok) return;
  setMsg('');
  try {
    await load();
    setMsg(t('已彻底删除资源组「{name}」，并写入系统审计', { name: g.name }), 'info');
  } catch (e) { setMsg(e.message); }
}

// ---- 编辑 ----

const HOOKS = [['ding', t('钉钉')], ['feishu', t('飞书')], ['slack', 'Slack']];

async function loadLeaders() {
  if (leadersLoaded) return;
  leadersLoaded = true;
  try {
    const data = await api('GET', '/api/v1/settings/users' + qs({ status: 'active', sort: 'username', page_size: 200 }));
    const dl = $('re-leader-list');
    dl.replaceChildren();
    for (const u of data.rows) {
      const o = document.createElement('option');
      o.value = u.username;
      o.label = u.display;
      dl.append(o);
    }
  } catch (_) { leadersLoaded = false; }
}

async function openEdit(id) {
  setEditMsg('');
  editing = null;
  $('re-name').value = '';
  $('re-leader').value = '';
  $('re-remark').value = '';
  $('m-res-title').textContent = t('新建资源组');
  $('re-flows').hidden = true;
  loadLeaders();
  if (id) {
    try { editing = await api('GET', `${API}/${id}`); } catch (e) { setMsg(e.message); return; }
    $('m-res-title').textContent = t('编辑资源组：{name}', { name: editing.name });
    $('re-name').value = editing.name;
    $('re-leader').value = editing.leader || '';
    $('re-remark').value = editing.remark || '';
    const body = $('re-flows-body');
    body.replaceChildren();
    for (const f of editing.flows) {
      const tr = el('tr');
      tr.append(el('td', '', tp(f.label)), el('td', f.configured ? '' : 'muted', f.configured ? tp(f.text) : t('未配置')));
      body.append(tr);
    }
    $('re-flow-link').href = '/settings/audit-flow#g=' + editing.id;
    $('re-flows').hidden = false;
  }
  $('re-chan-now').textContent = editing ? chanNote(editing) : '';
  $('re-chan-now').hidden = !editing;
  // 编辑框里不再有 Webhook 输入框（通知请到“通知渠道”配置）；这个组还配着旧版 Webhook 时才出现“清除”入口
  let anyLegacy = false;
  for (const [k] of HOOKS) {
    $('re-' + k + '-cb').checked = false;
    const set = !!(editing && editing[k + '_set']);
    $('re-' + k + '-clear').hidden = !set;
    anyLegacy = anyLegacy || set;
  }
  $('re-legacy').hidden = !anyLegacy;
  editModal.open();
  $('re-name').focus();
}

async function saveEdit() {
  setEditMsg('');
  const body = { name: $('re-name').value, leader: $('re-leader').value.trim(), remark: $('re-remark').value.trim() };
  for (const [k] of HOOKS) body['clear_' + k] = $('re-' + k + '-cb').checked; // Webhook 本身不再从这里填，只能清除旧的
  await busy($('re-save'), async () => {
    try {
      if (editing) await api('PUT', `${API}/${editing.id}`, body);
      else await api('POST', API, body);
      editModal.close();
      await load();
      setMsg(t('已保存'), 'info');
    } catch (e) { setEditMsg(e.message); }
  });
}

// ---- 关联对象（暂存后统一保存） ----

const emptyStage = () => ({ users: { add: new Set(), remove: new Set() }, instances: { add: new Set(), remove: new Set() } });
const relData = (kind) => (kind === 'users' ? rel.users : rel.instances);

function dirtyCounts(kind) {
  const s = stage[kind];
  return [s.add.size, s.remove.size];
}

const relDirty = () => !!stage && ['users', 'instances'].some((k) => dirtyCounts(k).some((n) => n > 0));

// 暂存之后的有效视图：已关联 = 原已关联 − 待移除 + 待添加
function effective(kind) {
  const d = relData(kind);
  const s = stage[kind];
  return {
    linked: d.linked.filter((x) => !s.remove.has(x.id)).concat(d.unlinked.filter((x) => s.add.has(x.id))),
    unlinked: d.unlinked.filter((x) => !s.add.has(x.id)).concat(d.linked.filter((x) => s.remove.has(x.id))),
  };
}

function stageMove(kind, ids, toLinked) {
  const d = relData(kind);
  const s = stage[kind];
  const origLinked = new Set(d.linked.map((x) => x.id));
  for (const id of ids) {
    if (toLinked) {
      if (origLinked.has(id)) s.remove.delete(id); else s.add.add(id);
    } else if (origLinked.has(id)) s.remove.add(id); else s.add.delete(id);
  }
  renderRel();
  schedulePreview();
}

function checkList(items, textOf) {
  const box = el('div', 'check-list');
  for (const it of items) {
    const lb = el('label');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = String(it.id);
    const txt = textOf(it);
    lb.dataset.q = txt.toLowerCase();
    lb.append(cb, document.createTextNode(txt));
    box.append(lb);
  }
  if (!items.length) box.append(el('span', 'hint', t('（无）')));
  return box;
}

function side(title, items, textOf, actionText, cls, onAction) {
  const card = el('div', 'card inner');
  card.append(el('h3', '', t('{title}（{n}）', { title, n: items.length })));
  const search = document.createElement('input');
  search.type = 'search';
  search.placeholder = t('搜索');
  search.setAttribute('aria-label', t('搜索{title}', { title }));
  const list = checkList(items, textOf);
  search.addEventListener('input', () => {
    const q = search.value.trim().toLowerCase();
    for (const lb of list.querySelectorAll('label')) lb.hidden = q !== '' && !lb.dataset.q.includes(q);
  });
  const btn = button(actionText, cls);
  btn.addEventListener('click', () => {
    const ids = [...list.querySelectorAll('input:checked')].map((c) => Number(c.value));
    if (!ids.length) { setRelMsg(t('请先勾选对象')); return; }
    setRelMsg('');
    onAction(ids);
  });
  const act = el('div', 'actions');
  act.append(btn);
  card.append(search, list, act);
  return card;
}

function renderRel() {
  const body = $('rel-body');
  body.replaceChildren();
  const userText = (u) => `${u.username} ${u.display}${u.is_active ? '' : t('（已停用）')}`;
  const instText = (i) => t('{name}（{code}）', { name: i.name, code: `${i.db_type}${i.type ? ' / ' + i.type : ''}` });
  const isUser = relKind === 'users';
  const textOf = isUser ? userText : instText;
  const ev = effective(relKind);
  $('rel-nu').textContent = String(effective('users').linked.length);
  $('rel-ni').textContent = String(effective('instances').linked.length);
  body.append(
    side(t('已关联'), ev.linked, textOf, t('移除所选 →'), 'danger', (ids) => stageMove(relKind, ids, false)),
    side(t('未关联'), ev.unlinked, textOf, t('← 添加所选'), 'primary', (ids) => stageMove(relKind, ids, true)),
  );
  const [ua, ur] = dirtyCounts('users');
  const [ia, ir] = dirtyCounts('instances');
  $('rel-sum').textContent = t('本次变更（未保存）：用户 +{ua} −{ur}；实例 +{ia} −{ir}', { ua, ur, ia, ir });
  $('rel-save').disabled = !relDirty();
  $('rel-cancel').textContent = relDirty() ? t('放弃并关闭') : t('关闭');
}

// 暂存变更后向服务端预览：移除（或加入）用户会不会让审批流出现新的断点
function schedulePreview() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(runPreview, 250);
}

async function runPreview() {
  const seq = ++previewSeq;
  const box = $('rel-warn');
  const s = stage.users;
  if (!s.add.size && !s.remove.size) { box.replaceChildren(); return; }
  try {
    const p = await api('POST', `${API}/${relGroup.id}/relations/preview`, { kind: 'users', add: [...s.add], remove: [...s.remove] });
    if (seq !== previewSeq) return;
    box.replaceChildren();
    if (p.new_breaks.length) {
      const d = el('div', 'alert warn');
      d.append(el('b', '', t('此调整会让审批流出现新的断点：')));
      const ul = el('ul', 'plain');
      for (const nb of p.new_breaks) ul.append(el('li', '', tp(nb)));
      d.append(ul, el('span', 'small', t('保存后这些级别的工单会卡住。')));
      box.append(d);
    }
  } catch (e) {
    if (seq === previewSeq) box.replaceChildren(el('div', 'alert warn', t('无法预览审批流影响：{msg}', { msg: e.message })));
  }
}

async function saveRel() {
  setRelMsg('');
  await busy($('rel-save'), async () => {
    try {
      for (const kind of ['users', 'instances']) {
        const s = stage[kind];
        if (!s.add.size && !s.remove.size) continue;
        await api('PUT', `${API}/${relGroup.id}/${kind}`, { add: [...s.add], remove: [...s.remove] });
        stage[kind] = { add: new Set(), remove: new Set() };
      }
      rel = await api('GET', `${API}/${relGroup.id}/relations`);
      renderRel();
      $('rel-warn').replaceChildren();
      setRelMsg(t('已保存，并写入系统审计'), 'info');
    } catch (e) {
      // 可能已保存了一部分：以服务端状态为准重新载入，暂存里没成功的保留
      try { rel = await api('GET', `${API}/${relGroup.id}/relations`); renderRel(); } catch (_) { /* 保留原提示 */ }
      setRelMsg(e.message);
    }
  });
}

async function openRel(g) {
  setRelMsg('');
  relGroup = g;
  try { rel = await api('GET', `${API}/${g.id}/relations`); } catch (e) { setMsg(e.message); return; }
  stage = emptyStage();
  $('rel-warn').replaceChildren();
  $('m-rel-title').textContent = t('关联对象 · {name}', { name: g.name });
  relKind = 'users';
  showRelTab('users');
  relModal.open();
}

// 有暂存但未保存的关联变更时，关闭前弹确认框（异步：modal() 会等它结束）
const confirmDiscard = async () => {
  if (!relDirty()) return true;
  const r = await riskConfirm({ title: t('放弃未保存的关联变更？'), lines: [t('有暂存但未保存的关联变更，放弃后这些调整不会生效。')], okText: t('放弃并关闭'), cancelText: t('继续编辑'), danger: true });
  return r.ok;
};
const relModal = modal('m-res-rel', { beforeClose: confirmDiscard, onClose: () => { stage = emptyStage(); load().catch((e) => setMsg(e.message)); } });
const showRelTab = tabs($('m-res-rel'), (name) => { relKind = name; if (rel) renderRel(); });

$('rel-cancel').addEventListener('click', () => relModal.close());
$('rel-save').addEventListener('click', saveRel);
$('rg-new').addEventListener('click', () => openEdit(0));
$('re-save').addEventListener('click', saveEdit);
$('rg-filter').addEventListener('submit', (e) => e.preventDefault());
$('rg-search').addEventListener('input', () => load().catch((e) => setMsg(e.message)));
$('rg-deleted').addEventListener('change', () => load().catch((e) => setMsg(e.message)));

load().catch((e) => { emptyRow($('rg-rows'), 9, t('加载失败')); setMsg(e.message); });
