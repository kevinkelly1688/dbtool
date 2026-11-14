/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: new.js
 * @time: 2026/10/28
 */
// 提交 SQL 工单：选资源组/实例/库 → 写 SQL → 检查 → 提交。支持 SQL Server 的批量上线（多实例）。
import { api, riskConfirm, setButtonMeta } from '../common.js';
import { t, tp } from '../i18n.js';
import { createEditor } from '../query/editor.js';
import { showFlow, loadCopySource } from './flowprev.js';
import { filterLabels } from '../labelfilter.js';

const $ = (id) => document.getElementById(id);
const msg = $('new-msg');

let groups = [];
let batchOpt = { allowed: false, types: [], max: 100 };
let checked = null; // 最近一次通过检查的快照：{ key, result }

function alertBox(text, kind = 'err') {
  msg.replaceChildren();
  if (!text) return;
  const d = document.createElement('div');
  d.className = 'alert ' + kind;
  d.setAttribute('role', 'alert');
  d.textContent = text;
  msg.append(d);
}

function option(value, label) {
  const o = document.createElement('option');
  o.value = value;
  o.textContent = label;
  return o;
}

const editor = createEditor($('wf-editor'), {
  onRun: () => runCheck(),
  onChange: () => invalidate(),
});

const skip = () => !isRedis() && !!($('wf-skip') && $('wf-skip').checked);
const batchOn = () => !!($('wf-batch') && $('wf-batch').checked);
const currentGroup = () => groups.find((x) => String(x.id) === $('wf-group').value);

// 批量模式下选中的实例 ID（按页面顺序）
function selectedIds() {
  return [...document.querySelectorAll('#wf-batch-checks input:checked')].map((i) => Number(i.value));
}

// 实例 ID 列表：批量取勾选的，单实例取下拉框的
function targetIds() {
  if (batchOn()) return selectedIds();
  const v = $('wf-inst').value;
  return v ? [Number(v)] : [];
}

function snapshot() {
  return JSON.stringify([targetIds(), $('wf-db').value, editor.value, skip(), batchOn()]);
}

// SQL、实例、库、是否跳过检查任何一项变化，之前的检查结果就作废，需要重新检查才能提交。
function invalidate() {
  if (checked && checked.key !== snapshot()) {
    checked = null;
    $('wf-submit').disabled = true;
  }
}

function selectedInstance() {
  const g = currentGroup();
  const id = targetIds()[0];
  return g ? g.instances.find((i) => i.id === id) : null;
}

// Redis 实例：提交的是命令（每行一条），由管理员定义的规则检查，不支持批量 / 跳过检查 / 备份。
function isRedis() {
  const inst = selectedInstance();
  return !!inst && inst.db_type === 'redis';
}

// 切换 Redis 命令模式的文案与控件（切回其他实例时完全恢复）。
function applyRedisMode() {
  const on = isRedis();
  $('wf-redis-note').hidden = !on;
  $('wf-sql-title').textContent = on ? t('Redis 命令') : t('SQL 内容');
  $('wf-check').textContent = on ? t('检查命令') : t('检查 SQL');
  const head = $('check-table').tHead.rows[0].cells;
  head[0].textContent = on ? t('行|line') : '#';
  head[4].textContent = on ? t('命令') : 'SQL';
  head[5].textContent = on ? t('涉及键数') : t('影响行数');
  const ed = window.ace && window.ace.edit ? window.ace.edit($('wf-editor')) : null;
  if (ed) ed.session.setMode(on ? 'ace/mode/text' : 'ace/mode/mysql');
}

const groupInstances = () => (currentGroup() ? currentGroup().instances : []);
const batchCapable = (list) => {
  const types = new Set(batchOpt.types);
  return list.filter((i) => types.has(i.db_type) && i.db_type !== 'redis');
};
// 用户有批量权限、且资源组里有可批量的实例
const batchAvailable = () => batchOpt.allowed && batchCapable(groupInstances()).length > 0;

// 提示里的“可批量的类型”：服务端 BATCH_DB_TYPES 决定，默认只有 SQL Server（写成 MSSQL）。
const batchTypeText = () => (batchOpt.types.length ? batchOpt.types : ['mssql'])
  .map((x) => (x === 'mssql' ? 'MSSQL' : x === 'mysql' ? 'MySQL' : x)).join(t('、|sep'));

// 批量开关始终显示在选项组里；能不能用的条件和以前完全一样：单实例下拉里选中的是可批量的类型（默认只有 SQL Server），
// 并且有批量权限、资源组里有可批量的实例。批量打开后“目标实例”也一直在页面上，所以它的类型始终要满足：
// 换成不支持批量的类型（MySQL、Redis 等），批量会自动关闭并置灰，和还没打开批量时一样。
// 不能用时置灰、不能勾，鼠标停上去（title）提示原因：实例类型不对 → 仅支持 MSSQL；类型对但没有批量权限 → 没有批量提交的权限。
function updateBatchWrap() {
  const cur = groupInstances().find((i) => String(i.id) === $('wf-inst').value);
  const typeOK = !!cur && batchCapable([cur]).length > 0;
  const enabled = batchAvailable() && typeOK;
  const wrap = $('wf-batch-wrap');
  wrap.classList.toggle('off', !enabled);
  wrap.setAttribute('aria-disabled', String(!enabled));
  wrap.title = enabled ? '' : typeOK ? t('没有批量提交的权限') : t('仅支持 {types}', { types: batchTypeText() });
  $('wf-batch').disabled = !enabled;
  if (!enabled) $('wf-batch').checked = false;
  $('wf-batch-list').hidden = !batchOn();
  // 批量打开后“目标实例”也留在原位（版面不动）：提交的实例以“选择要上线的实例”里勾选的为准，单实例下拉在批量时只用来判断能不能批量
}

function updateOptionsVisibility() {
  updateBatchWrap();
  const inst = selectedInstance();
  const mssql = inst && inst.db_type === 'mssql';
  const redis = !!inst && inst.db_type === 'redis';
  $('wf-backup-wrap').hidden = !!mssql || redis || batchOn() || !window.__inception; // SQL Server、Redis 与未配 goInception 都没有备份
  if ($('wf-skip-wrap')) $('wf-skip-wrap').hidden = batchOn() || redis;
  applyRedisMode();
}

function fillInstances() {
  const list = groupInstances();
  const batchList = batchCapable(list);

  const sel = $('wf-inst');
  const keepInst = sel.value; // 打开 / 关闭批量都会重建下拉，保持原来选中的实例
  sel.replaceChildren();
  if (!list.length) sel.append(option('', t('该资源组下没有可上线的实例')));
  list.forEach((i) => sel.append(option(i.id, t('{name}（{type}）', { name: i.name, type: i.db_type }))));
  if (keepInst && list.some((i) => String(i.id) === keepInst)) sel.value = keepInst;

  const box = $('wf-batch-checks');
  box.replaceChildren();
  $('wf-batch-q').value = '';
  if (!batchList.length) {
    const h = document.createElement('span');
    h.className = 'hint';
    h.textContent = t('该资源组下没有可批量上线的实例');
    box.append(h);
  }
  batchList.forEach((i) => {
    const l = document.createElement('label');
    const c = document.createElement('input');
    c.type = 'checkbox';
    c.value = i.id;
    c.addEventListener('change', onSelectionChange);
    l.append(c, ' ' + i.name);
    box.append(l);
  });
  updateCount();
  updateOptionsVisibility();
  return loadDatabases();
}

function updateCount() {
  const q = $('wf-batch-q').value.trim();
  const all = document.querySelectorAll('#wf-batch-checks label').length;
  const shown = document.querySelectorAll('#wf-batch-checks label:not([hidden])').length;
  $('wf-batch-count').textContent = q
    ? t('已选 {n} / {max}，匹配 {shown} / {all}', { n: selectedIds().length, max: batchOpt.max, shown, all })
    : t('已选 {n} / {max}', { n: selectedIds().length, max: batchOpt.max });
}

// 搜索：输入即筛选（模糊匹配）；已勾选的不会因为被筛掉而取消，全选只选搜索结果
function filterBatch() {
  const box = $('wf-batch-checks');
  const f = filterLabels(box, $('wf-batch-q').value);
  box.querySelector('.no-match')?.remove();
  if (!f.shown && f.total) {
    const h = document.createElement('span');
    h.className = 'hint no-match';
    h.textContent = t('没有匹配的实例');
    box.append(h);
  }
  updateCount();
}

let dbOwner = null; // 当前库列表对应的实例
function onSelectionChange() {
  updateCount();
  updateOptionsVisibility();
  const first = targetIds()[0] || null;
  if (first !== dbOwner) loadDatabases();
  else invalidate();
}

async function loadDatabases() {
  const sel = $('wf-db');
  const keep = sel.value;
  sel.replaceChildren();
  const id = targetIds()[0];
  dbOwner = id || null;
  if (!id) {
    sel.append(option('', batchOn() ? t('先勾选实例') : ''));
    return;
  }
  sel.append(option('', t('加载中…')));
  try {
    const dbs = await api('GET', `/api/v1/sqlworkflow/instances/${id}/databases`);
    sel.replaceChildren();
    if (!dbs.length) sel.append(option('', t('没有可用的数据库')));
    const redis = isRedis(); // Redis 的库是编号：值是纯数字，显示成 db0、db1…
    dbs.forEach((d) => sel.append(option(d, redis ? 'db' + d : d)));
    if (dbs.includes(keep)) sel.value = keep;
  } catch (e) {
    sel.replaceChildren(option('', t('加载失败')));
    alertBox(e.message);
  }
  invalidate();
}

function cell(tr, text, cls) {
  const td = document.createElement('td');
  td.textContent = text === undefined || text === null ? '' : String(text);
  if (cls) td.className = cls;
  tr.append(td);
}

function renderCheck(res) {
  $('check-card').hidden = false;
  const tb = $('check-table').tBodies[0];
  tb.replaceChildren();
  res.rows.forEach((r) => {
    const tr = document.createElement('tr');
    if (r.errlevel === 2) tr.className = 'lv-err';
    else if (r.errlevel === 1) tr.className = 'lv-warn';
    cell(tr, r.id);
    cell(tr, tp(r.stagestatus));
    cell(tr, r.errlevel === 2 ? t('错误') : r.errlevel === 1 ? t('警告') : t('通过|level'));
    cell(tr, r.errormessage === 'None' ? '' : tp(r.errormessage), 'wrap');
    cell(tr, r.sql, 'mono wrap');
    cell(tr, r.affected_rows, 'num');
    tb.append(tr);
  });
  if (!res.rows.length) {
    const tr = document.createElement('tr');
    const td = document.createElement('td');
    td.colSpan = 6;
    td.className = 'empty';
    td.textContent = t('没有语句');
    tr.append(td);
    tb.append(tr);
  }
  $('check-summary').textContent = t('类型：{type}　警告 {w}　错误 {e}', { type: tp(res.syntax_label), w: res.warning_count, e: res.error_count });
  const n = $('check-notice');
  n.replaceChildren();
  if (res.notice) {
    const d = document.createElement('div');
    d.className = 'alert warn';
    d.textContent = tp(res.notice);
    n.append(d);
  }
  // 批量上线：每个实例的预检结果
  const ic = $('inst-check');
  ic.hidden = !(res.instances && res.instances.length);
  const ib = $('inst-check-table').tBodies[0];
  ib.replaceChildren();
  (res.instances || []).forEach((i) => {
    const tr = document.createElement('tr');
    if (!i.ok) tr.className = 'lv-err';
    cell(tr, i.name);
    cell(tr, i.ok ? t('可用') : t('不可用'));
    cell(tr, tp(i.error || ''));
    ib.append(tr);
  });
}

const SOURCE_NOTE = {
  goinception: t('已由 goInception 检查'),
  mssql: t('已由 SQL Server 语法解析与上线规则检查'),
  manual: t('已跳过检查'),
  redis: t('已按 Redis 规则检查'),
  create_table: t('已做语法检查（原生方式执行）'),
};

async function runCheck() {
  alertBox('');
  const ids = targetIds();
  const db = $('wf-db').value;
  if (!ids.length) return alertBox(batchOn() ? t('请先勾选要上线的实例') : t('请先选择实例'));
  if (!db) return alertBox(t('请先选择数据库'));
  if (!editor.value.trim()) return alertBox(isRedis() ? t('请先填写 Redis 命令') : t('请先填写 SQL'));
  const btn = $('wf-check');
  btn.setAttribute('aria-busy', 'true');
  btn.disabled = true;
  try {
    const key = snapshot();
    const body = { db_name: db, sql: editor.value, skip_check: skip() };
    if (batchOn()) body.instance_ids = ids;
    else body.instance_id = ids[0];
    const res = await api('POST', '/api/v1/sqlworkflow/check', body);
    renderCheck(res);
    $('wf-engine-note').textContent = SOURCE_NOTE[res.source] || t('基础检查（未配置 goInception）');
    const bad = (res.instances || []).filter((i) => !i.ok);
    if (res.error_count > 0) {
      checked = null;
      $('wf-submit').disabled = true;
      alertBox(t('检查发现 {n} 个错误，请修改后重新检查。', { n: res.error_count }));
    } else if (bad.length) {
      checked = null;
      $('wf-submit').disabled = true;
      alertBox(t('有 {n} 个实例无法使用，请取消勾选或处理后重新检查。', { n: bad.length }));
    } else {
      checked = { key, result: res };
      $('wf-submit').disabled = false;
    }
  } catch (e) {
    alertBox(e.message);
  } finally {
    btn.removeAttribute('aria-busy');
    btn.disabled = false;
  }
}

async function submit() {
  alertBox('');
  if (!checked) return alertBox(t('请先检查 SQL，通过后才能提交'));
  const g = currentGroup();
  const name = $('wf-name').value.trim();
  if (!name) return alertBox(t('请填写工单名称'));
  const ids = targetIds();
  const btn = $('wf-submit');
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  try {
    const body = {
      name, demand_url: $('wf-url').value.trim(), db_name: $('wf-db').value,
      group_name: g ? g.name : '', sql: editor.value, is_backup: $('wf-backup').checked && !$('wf-backup-wrap').hidden, skip_check: skip(),
      run_date_start: $('wf-start').value, run_date_end: $('wf-end').value,
    };
    if (batchOn()) body.instance_ids = ids;
    else body.instance_id = ids[0];
    const res = await api('POST', '/api/v1/sqlworkflow', body);
    location.href = `/sqlworkflow/${res.workflow_id}`;
  } catch (e) {
    alertBox(e.message);
    btn.disabled = false;
    btn.removeAttribute('aria-busy');
  }
}

// 复制为新工单：按终态工单的内容预填资源组、实例、库、SQL（资源组或实例你已无权使用时给出提示，由你重新选择）
async function applyCopy(c) {
  const notes = [];
  const g = groups.find((x) => x.name === c.group_name);
  if (g) $('wf-group').value = String(g.id);
  else notes.push(t('原资源组「{name}」你已不在其中，请重新选择资源组和实例', { name: c.group_name }));
  $('wf-name').value = c.name || '';
  $('wf-url').value = c.demand_url || '';
  if ($('wf-backup')) $('wf-backup').checked = !!c.is_backup;
  await fillInstances();
  showFlow($('flow-prev'), $('wf-group').value, $('flow-note'));
  if (g) {
    const ids = (c.instance_ids && c.instance_ids.length ? c.instance_ids : [c.instance_id]).map(String);
    const useBatch = ids.length > 1 && $('wf-batch') && batchAvailable();
    if (useBatch) {
      // 批量打开后“目标实例”也在页面上，它必须是可批量的类型，否则批量会被关掉：先指到原工单里还可用的实例，没有就指到第一个可批量的
      const capable = batchCapable(groupInstances()).map((i) => String(i.id));
      const anchor = ids.find((id) => capable.includes(id)) || capable[0];
      if (anchor) $('wf-inst').value = anchor;
      $('wf-batch').checked = true;
      await fillInstances();
      const boxes = [...document.querySelectorAll('#wf-batch-checks input')];
      boxes.forEach((b) => (b.checked = ids.includes(b.value)));
      if (!boxes.some((b) => b.checked)) notes.push(t('原工单的实例你已无权使用，请重新勾选实例'));
      else if (boxes.filter((b) => b.checked).length < ids.length) notes.push(t('原工单的部分实例已不可选，已勾选其余的'));
      updateCount();
      updateOptionsVisibility();
      await loadDatabases();
    } else {
      if (ids.length > 1) notes.push(t('当前账号不能提交批量上线工单，已只带出第一个实例'));
      const sel = $('wf-inst');
      if ([...sel.options].some((o) => o.value === ids[0])) {
        sel.value = ids[0];
        updateOptionsVisibility();
        await loadDatabases();
      } else notes.push(t('原工单的实例已不可选，请重新选择实例'));
    }
    const db = $('wf-db'); // 上面已等库列表加载完
    if ([...db.options].some((o) => o.value === c.db_name)) db.value = c.db_name;
    else if (c.db_name) notes.push(t('原数据库「{name}」不在当前实例的库列表里，请重新选择', { name: c.db_name }));
  }
  editor.value = c.sql || '';
  alertBox(t('已从工单 #{id} 复制内容，请核对后重新检查并提交。', { id: c.source_id }) + (notes.length ? ' ' + notes.join(t('；')) + t('。') : ''), notes.length ? 'warn' : 'info');
}

async function init() {
  try {
    const o = await api('GET', '/api/v1/sqlworkflow/options');
    groups = o.groups;
    batchOpt = o.batch || batchOpt;
    window.__inception = !!o.inception;
    const sel = $('wf-group');
    if (!groups.length) sel.append(option('', t('你还没有加入任何资源组')));
    groups.forEach((g) => sel.append(option(g.id, g.name)));
    if (!o.inception) {
      $('wf-engine-note').textContent = o.native_allowed ? t('未配置 goInception，MySQL 实例使用基础检查') : t('未配置 goInception，MySQL 实例暂时无法提交');
    }
    await fillInstances();
    showFlow($('flow-prev'), $('wf-group').value, $('flow-note'));
    const c = await loadCopySource();
    if (c) await applyCopy(c);
  } catch (e) {
    alertBox(e.message);
  }
}

$('wf-group').addEventListener('change', () => { if ($('wf-batch')) $('wf-batch').checked = false; fillInstances(); showFlow($('flow-prev'), $('wf-group').value, $('flow-note')); });
$('wf-inst').addEventListener('change', () => { updateOptionsVisibility(); loadDatabases(); });
$('wf-db').addEventListener('change', invalidate);
if ($('wf-batch')) $('wf-batch').addEventListener('change', fillInstances);
$('wf-batch-q').addEventListener('input', filterBatch);
$('wf-batch-q').addEventListener('keydown', (e) => { if (e.key === 'Enter') e.preventDefault(); }); // 回车不触发表单提交 / 检查
$('wf-batch-all').addEventListener('click', () => {
  const boxes = [...document.querySelectorAll('#wf-batch-checks label:not([hidden]) input')].slice(0, batchOpt.max);
  boxes.forEach((b) => (b.checked = true));
  onSelectionChange();
});
$('wf-batch-none').addEventListener('click', () => {
  document.querySelectorAll('#wf-batch-checks input').forEach((b) => (b.checked = false));
  onSelectionChange();
});
// 勾选“跳过语法检查”要先过高危确认（取消则取消勾选）；提交时带 skip_check，服务端会把它写进审计。
if ($('wf-skip')) {
  setButtonMeta({ 'sqlworkflow.skip_check': t('跳过语法检查') });
  $('wf-skip').addEventListener('change', async () => {
    invalidate();
    if (!$('wf-skip').checked) return;
    const r = await riskConfirm({
      title: t('跳过语法检查'), level: 'high', code: 'sqlworkflow.skip_check', okText: t('确认跳过'),
      lines: [
        t('跳过检查后，系统不会发现缺主键、无 WHERE、表不存在等问题。'),
        t('该工单只能由目标库原生执行：不备份、没有回滚语句、不支持在线改表。'),
        t('仅在 goInception 无法处理的特殊语句时使用。提交时会在审计里记录“跳过语法检查”。'),
      ],
    });
    if (!r.ok) { $('wf-skip').checked = false; invalidate(); $('wf-skip').focus(); }
  });
}
$('wf-check').addEventListener('click', runCheck);
$('wf-submit').addEventListener('click', submit);
init();
