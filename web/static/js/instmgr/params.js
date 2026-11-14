/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: params.js
 * @time: 2026/10/28
 */
// 参数配置：运行值（SHOW GLOBAL VARIABLES，含推荐值与“偏离推荐”）、参数模板总览、动态修改（SET GLOBAL）、修改历史（当前实例 / 全部实例）。
import { api, $, el, btn, setMsg, qs, emptyRow, cell, openModal, closeModal, confirmBox, initTabs, loadCaps, can, loadInstanceSelect, renderPager, busy } from './ui.js';
import { riskConfirm } from '../common.js';
import { myCnfSnippet } from './mycnf.js';
import { t, tp } from '../i18n.js';

const msg = $('pm-msg');
const hist = { page: 1, size: 20, total: 0 };
let choices = [];
let caps = {};
let tab = 'vars';
let editing = null; // 正在修改的参数行
let tpl = null; // 正在编辑的模板行（null 表示新建）

const inst = () => Number($('pm-inst').value);
const cur = () => choices.find((c) => c.id === inst());

function guard() {
  const c = cur();
  if (!c) { setMsg(msg, t('没有可用的实例')); return false; }
  if (!c.supported) { setMsg(msg, t('暂不支持 {type} 类型实例的参数配置', { type: tp(c.db_type_label) }), 'warn'); return false; }
  return true;
}

async function loadTab(name = tab) {
  setMsg(msg, '');
  if (!guard()) { emptyRow($('v-rows'), 7, '—'); emptyRow($('tpl-rows'), 7, '—'); emptyRow($('h-rows'), 7, '—'); return; }
  try { await ({ vars: loadVars, tpl: loadTpls, hist: loadHist }[name])(); } catch (e) { setMsg(msg, e.message); }
}

// ---- 参数列表 ----
async function loadVars() {
  emptyRow($('v-rows'), 7, t('加载中…'));
  const rows = await api('GET', `/api/v1/instances/${inst()}/params` + qs({ editable: $('v-editable').value, q: $('v-q').value.trim(), deviation: $('v-dev').checked ? '1' : '' }));
  const tb = $('v-rows');
  const nDev = rows.filter((r) => r.deviates).length;
  $('v-dev-count').textContent = nDev ? t('{n} 项偏离模板推荐值', { n: nDev }) : '';
  if (!rows.length) {
    emptyRow(tb, 7, $('v-dev').checked ? t('没有偏离推荐值的参数') : $('v-editable').value === 'yes' ? t('没有允许修改的参数') : t('没有符合条件的参数'));
    return;
  }
  tb.replaceChildren();
  const LIMIT = 300;
  for (const r of rows.slice(0, LIMIT)) {
    const tr = el('tr', r.deviates ? 'lv-warn' : '');
    const ops = el('td', 'row-actions');
    if (r.editable && can(caps, 'param.edit')) ops.append(btn(t('修改'), '', () => openSet(r)), ' ');
    if (can(caps, 'param.template')) ops.append(btn(r.template_id ? t('模板') : t('+模板'), 'ghost', () => openTpl(r)));
    const val = el('td', 'mono wrap', r.runtime_value);
    if (r.deviates) val.append(' ', el('span', 'badge warn', t('偏离推荐')));
    tr.append(cell(r.variable_name, 'mono'), val, cell(r.default_value, 'mono'), cell(r.recommended_value || '—', 'mono'), cell(r.valid_values, 'mono'), cell(r.description, 'wrap'), ops);
    tb.append(tr);
  }
  if (rows.length > LIMIT) setMsg(msg, t('共 {n} 个参数，只显示前 {limit} 个，请用搜索缩小范围', { n: rows.length, limit: LIMIT }), 'warn');
}

// ---- 修改参数值 ----
function openSet(r) {
  editing = r;
  setMsg($('pm-set-msg'), '');
  $('s-name').value = r.variable_name;
  $('s-old').value = r.runtime_value;
  $('s-new').value = r.runtime_value;
  $('s-valid').textContent = r.valid_values ? t('（允许 {values}）', { values: r.valid_values }) : '';
  $('s-rec-box').hidden = !r.recommended_value;
  $('s-rec').value = r.recommended_value || '';
  openModal('pm-m-set');
}

async function submitSet() {
  const body = { variable_name: editing.variable_name, value: $('s-new').value };
  const id = inst();
  let plan;
  try { plan = await busy($('s-submit'), () => api('POST', `/api/v1/instances/${id}/params/preview`, body)); } catch (e) { return setMsg($('pm-set-msg'), e.message); }
  const ok = await confirmBox({ title: t('确定修改 {name} 的参数？', { name: cur().instance_name }),
    lines: [t('{name}：{old} → {new}', { name: plan.variable_name, old: plan.old_value, new: plan.new_value }), tp(plan.note)], sql: [plan.sql + ';'], okText: t('确定执行') });
  if (!ok) return;
  try {
    const r = await busy($('s-submit'), () => api('PUT', `/api/v1/instances/${id}/params`, { ...body, confirm: true }));
    closeModal('pm-m-set');
    await loadVars();
    // 预览里的 note 是固定说明；执行结果的 note 与它不同时，是“写入修改历史失败”之类的附加提示，一并展示
    setMsg(msg, t('修改成功，请手动持久化到配置文件！') + (r.note && r.note !== plan.note ? r.note : ''), 'info');
    await persistHint(cur(), plan);
  } catch (e) { setMsg($('pm-set-msg'), e.message); }
}

// 修改成功后给出配置文件片段：在线 SET GLOBAL 重启后会丢失。只有 MySQL 才有 my.cnf，其它类型只提示手工持久化。
async function persistHint(instRow, p) {
  const snippet = instRow && instRow.db_type === 'mysql' ? myCnfSnippet(p.variable_name, p.new_value) : '';
  const who = { param: p.variable_name, name: instRow ? instRow.instance_name : t('该实例') };
  await riskConfirm({
    title: t('参数已修改，请同步到配置文件'),
    lines: [
      snippet
        ? t('{param} 已在 {name} 上动态生效，但不会写入 my.cnf，实例重启后会恢复原值。', who)
        : t('{param} 已在 {name} 上动态生效，但不会写入配置文件，实例重启后会恢复原值。', who),
      snippet ? t('请在实例的 my.cnf（[mysqld] 段）里加入下面的配置，并保持各节点配置一致：') : t('请按该实例类型的方式，把新值同步到它的配置文件或云控制台参数模板。'),
    ],
    details: [{ k: t('参数'), v: p.variable_name }, { k: t('旧值'), v: p.old_value }, { k: t('新值'), v: p.new_value }, ...(snippet ? [{ k: t('配置片段'), v: snippet }] : [])],
    okText: t('知道了'), cancelText: t('关闭'),
  });
}

// ---- 参数模板 ----
const TPL_TYPES = [['mysql', 'MySQL'], ['polardb', 'PolarDB'], ['polardb-x', 'PolarDB-X'], ['doris', 'Doris'], ['selectdb', 'SelectDB'], ['adb', 'ADB']];

// 参数模板总览：当前实例所属类型的全部模板
async function loadTpls() {
  emptyRow($('tpl-rows'), 7, t('加载中…'));
  const list = await api('GET', '/api/v1/params/templates' + qs({ db_type: cur().db_type }));
  const tb = $('tpl-rows');
  $('tpl-count').textContent = t('{type} 共 {n} 个参数模板', { type: tp(cur().db_type_label), n: list.length });
  if (!list.length) return emptyRow(tb, 7, t('这个类型还没有参数模板'));
  tb.replaceChildren();
  for (const tm of list) {
    const tr = el('tr');
    const flags = el('td');
    flags.append(el('span', 'badge ' + (tm.editable && !tm.blacklisted ? 'ok' : ''), tm.editable && !tm.blacklisted ? t('可修改') : t('不可修改')));
    if (tm.blacklisted) flags.append(' ', el('span', 'badge crit', t('黑名单')));
    if (tm.needs_restart) flags.append(' ', el('span', 'badge warn', t('需重启')));
    const ops = el('td', 'row-actions');
    if (can(caps, 'param.template')) ops.append(btn(t('编辑'), '', () => openTpl({ template_id: tm.id, variable_name: tm.variable_name, default_value: tm.default_value, valid_values: tm.valid_values, editable: tm.editable, description: tm.description, recommended_value: tm.recommended_value })));
    tr.append(cell(tm.variable_name, 'mono'), cell(tm.default_value, 'mono'), cell(tm.recommended_value || '—', 'mono'), cell(tm.valid_values || t('（自由文本）'), 'mono'), flags, cell(tm.description, 'wrap'), ops);
    tb.append(tr);
  }
}

function openTpl(row) {
  tpl = row && row.template_id ? row : null;
  setMsg($('pm-tpl-msg'), '');
  $('pm-m-tpl-t').textContent = tpl ? t('修改参数模板') : t('添加参数模板');
  const dt = $('t-dbtype');
  dt.replaceChildren(...TPL_TYPES.map(([v, l]) => new Option(l, v)));
  dt.value = cur() ? cur().db_type : 'mysql';
  $('t-name').value = row ? row.variable_name : '';
  $('t-default').value = row ? row.default_value : '';
  $('t-rec').value = row && row.recommended_value ? row.recommended_value : '';
  $('t-valid').value = row ? row.valid_values : '';
  $('t-editable').checked = row && row.template_id ? row.editable : true;
  $('t-desc').value = row ? row.description : '';
  dt.disabled = $('t-name').readOnly = !!tpl;
  openModal('pm-m-tpl');
}

async function submitTpl() {
  const body = { db_type: $('t-dbtype').value, variable_name: $('t-name').value, default_value: $('t-default').value, valid_values: $('t-valid').value,
    editable: $('t-editable').checked, description: $('t-desc').value, recommended_value: $('t-rec').value };
  try {
    await busy($('t-submit'), () => (tpl ? api('PUT', `/api/v1/params/templates/${tpl.template_id}`, body) : api('POST', '/api/v1/params/templates', body)));
    closeModal('pm-m-tpl');
    await loadTab(tab);
    setMsg(msg, t('模板已保存'), 'info');
  } catch (e) { setMsg($('pm-tpl-msg'), e.message); }
}

// ---- 修改历史 ----
async function loadHist() {
  emptyRow($('h-rows'), 7, t('加载中…'));
  const all = $('h-scope').value === 'all';
  const q = qs({ q: $('h-q').value.trim(), page: hist.page, page_size: hist.size });
  const res = await api('GET', all ? `/api/v1/params/history${q}` : `/api/v1/instances/${inst()}/params/history${q}`);
  hist.total = res.total;
  const tb = $('h-rows');
  if (!res.rows.length) emptyRow(tb, 7, t('还没有修改记录'));
  else {
    tb.replaceChildren();
    for (const r of res.rows) {
      const tr = el('tr');
      tr.append(cell(r.instance_name), cell(r.variable_name, 'mono'), cell(r.old_var, 'mono'), cell(r.new_var, 'mono'), cell(r.set_sql, 'mono wrap'), cell(r.user_display || r.user_name), cell(r.create_time));
      tb.append(tr);
    }
  }
  renderPager($('h-pager'), hist, (p) => { hist.page = p; loadTab('hist'); });
}

// ---- 初始化 ----
$('v-filter').addEventListener('submit', (e) => { e.preventDefault(); loadTab('vars'); });
$('v-editable').addEventListener('change', () => loadTab('vars'));
$('v-dev').addEventListener('change', () => loadTab('vars'));
$('h-filter').addEventListener('submit', (e) => { e.preventDefault(); hist.page = 1; loadTab('hist'); });
$('h-scope').addEventListener('change', () => { hist.page = 1; loadTab('hist'); });
$('pm-add-tpl').addEventListener('click', () => openTpl(null));
$('s-submit').addEventListener('click', submitSet);
$('t-submit').addEventListener('click', submitTpl);

(async () => {
  try {
    caps = await loadCaps();
    $('pm-add-tpl').hidden = !can(caps, 'param.template');
    if (!can(caps, 'param.view')) return setMsg(msg, t('你没有查看实例参数的权限'));
    initTabs($('pm'), (name) => { tab = name; hist.page = 1; if (choices.length) loadTab(name); });
    choices = await loadInstanceSelect($('pm-inst'), 'params');
    $('pm-inst').addEventListener('change', () => { hist.page = 1; loadTab(); });
    await loadTab();
  } catch (e) { setMsg(msg, e.message); }
})();
