/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: export_new.js
 * @time: 2026/10/28
 */
// 提交数据导出工单：选资源组/实例/库/格式 → 写查询 → 检查（统计行数）→ 提交。支持多实例批量导出。
import { api, riskConfirm, setButtonMeta } from '../common.js';
import { t, tp } from '../i18n.js';
import { createEditor } from '../query/editor.js';
import { showFlow, loadCopySource } from './flowprev.js';
import { filterLabels } from '../labelfilter.js';

const $ = (id) => document.getElementById(id);
const msg = $('new-msg');

let groups = [];
let batchTypes = new Set();
let expOpt = { allowed: false, batch_allowed: false, formats: [], max_rows: 10000, seconds: 60, can_unmask: false, can_merge: false };
let batchMax = 100;
let checked = null; // 最近一次通过检查的快照：{ key }

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

const editor = createEditor($('wf-editor'), { onRun: () => runCheck(), onChange: () => invalidate() });

const batchOn = () => !!$('wf-batch').checked;
// 合并成一个文件：只在批量导出里、且服务端允许时才生效
const mergeOn = () => batchOn() && !$('wf-merge-wrap').hidden && $('wf-merge').checked;
const currentGroup = () => groups.find((x) => String(x.id) === $('wf-group').value);
const selectedIds = () => [...document.querySelectorAll('#wf-batch-checks input:checked')].map((i) => Number(i.value));

function targetIds() {
  if (batchOn()) return selectedIds();
  const v = $('wf-inst').value;
  return v ? [Number(v)] : [];
}

function snapshot() {
  return JSON.stringify([targetIds(), $('wf-db').value, editor.value, batchOn(), mergeOn()]);
}

// 实例、库、查询任何一项变化，之前的检查结果就作废。
function invalidate() {
  if (checked && checked.key !== snapshot()) {
    checked = null;
    $('wf-submit').disabled = true;
  }
}

const groupInstances = () => (currentGroup() ? currentGroup().export_instances : []);
const batchCapable = (list) => list.filter((i) => batchTypes.has(i.db_type));
// 用户有批量导出权限、且资源组里有可批量的实例
const batchAvailable = () => expOpt.batch_allowed && batchCapable(groupInstances()).length > 0;

// 批量开关默认不显示：只有单实例下拉里选中的是可批量的类型（默认只有 SQL Server）才出现；
// 已经打开批量时一直显示（免得没法关闭）。
function updateBatchWrap() {
  const cur = groupInstances().find((i) => String(i.id) === $('wf-inst').value);
  const show = batchAvailable() && (batchOn() || (!!cur && batchCapable([cur]).length > 0));
  $('wf-batch-wrap').hidden = !show;
  if (!show) $('wf-batch').checked = false;
  $('wf-batch-list').hidden = !batchOn();
  $('wf-inst-label').hidden = batchOn();
}

function fillInstances() {
  const list = groupInstances();
  const batchList = batchCapable(list);

  const sel = $('wf-inst');
  const keepInst = sel.value; // 打开 / 关闭批量都会重建下拉，保持原来选中的实例
  sel.replaceChildren();
  if (!list.length) sel.append(option('', t('该资源组下没有可导出的实例')));
  list.forEach((i) => sel.append(option(i.id, t('{name}（{type}）', { name: i.name, type: i.db_type }))));
  if (keepInst && list.some((i) => String(i.id) === keepInst)) sel.value = keepInst;
  updateBatchWrap();

  const box = $('wf-batch-checks');
  box.replaceChildren();
  $('wf-batch-q').value = '';
  if (!batchList.length) {
    const h = document.createElement('span');
    h.className = 'hint';
    h.textContent = t('该资源组下没有可批量导出的实例');
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
  return loadDatabases();
}

function updateCount() {
  const q = $('wf-batch-q').value.trim();
  const all = document.querySelectorAll('#wf-batch-checks label').length;
  const shown = document.querySelectorAll('#wf-batch-checks label:not([hidden])').length;
  $('wf-batch-count').textContent = q
    ? t('已选 {n} / {max}，匹配 {shown} / {all}', { n: selectedIds().length, max: batchMax, shown, all })
    : t('已选 {n} / {max}', { n: selectedIds().length, max: batchMax });
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
    const dbs = await api('GET', `/api/v1/sqlworkflow/instances/${id}/databases?export=1`); // 只列出有查询权限的库
    sel.replaceChildren();
    if (!dbs.length) sel.append(option('', t('没有可用的数据库')));
    dbs.forEach((d) => sel.append(option(d, d)));
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
    cell(tr, tp(r.instance_name || ''));
    cell(tr, tp(r.stagestatus));
    cell(tr, r.errlevel === 2 ? t('错误') : r.errlevel === 1 ? t('警告') : t('通过|level'));
    cell(tr, r.errormessage === 'None' ? '' : tp(r.errormessage), 'wrap');
    cell(tr, r.sql, 'mono wrap');
    cell(tr, r.affected_rows, 'num');
    tb.append(tr);
  });
  $('check-summary').textContent = t('警告 {w}　错误 {e}', { w: res.warning_count, e: res.error_count });
  const n = $('check-notice');
  n.replaceChildren();
  if (res.notice) {
    const d = document.createElement('div');
    d.className = 'alert warn';
    d.textContent = tp(res.notice);
    n.append(d);
  }
  const ic = $('inst-check');
  ic.hidden = !(res.instances && res.instances.length);
  const ib = $('inst-check-table').tBodies[0];
  ib.replaceChildren();
  (res.instances || []).forEach((i) => {
    const tr = document.createElement('tr');
    if (!i.ok) tr.className = 'lv-err';
    cell(tr, i.name);
    cell(tr, i.ok ? t('可导出') : t('不可导出'));
    cell(tr, tp(i.error || ''));
    ib.append(tr);
  });
}

async function runCheck() {
  alertBox('');
  const ids = targetIds();
  const db = $('wf-db').value;
  if (!ids.length) return alertBox(batchOn() ? t('请先勾选要导出的实例') : t('请先选择实例'));
  if (!db) return alertBox(t('请先选择数据库'));
  if (!editor.value.trim()) return alertBox(t('请先填写查询语句'));
  const btn = $('wf-check');
  btn.setAttribute('aria-busy', 'true');
  btn.disabled = true;
  try {
    const key = snapshot();
    const body = { export: true, db_name: db, sql: editor.value };
    if (batchOn()) body.instance_ids = ids;
    else body.instance_id = ids[0];
    if (mergeOn()) body.export_merge = true; // 服务端会核对各实例返回的列是否一致
    const res = await api('POST', '/api/v1/sqlworkflow/check', body);
    renderCheck(res);
    const bad = (res.instances || []).filter((i) => !i.ok);
    if (res.error_count > 0) {
      checked = null;
      $('wf-submit').disabled = true;
      alertBox(t('检查发现 {n} 个错误，请修改后重新检查。', { n: res.error_count }));
    } else {
      checked = { key };
      $('wf-submit').disabled = bad.length > 0;
      if (bad.length) alertBox(t('有 {n} 个实例无法导出，请取消勾选后重新检查。', { n: bad.length }));
    }
  } catch (e) {
    alertBox(e.message);
  } finally {
    btn.removeAttribute('aria-busy');
    btn.disabled = false;
  }
}

// 导出原值（仅超级用户，服务端强制）：勾选要先过高危确认，必须写理由；SQL 防火墙强制脱敏的列照样脱敏。
const unmaskOn = () => !!$('wf-unmask') && !$('wf-unmask-wrap').hidden && $('wf-unmask').checked;

function initUnmask() {
  if (!expOpt.can_unmask) return;
  setButtonMeta({ 'export.unmasked': t('导出原值（不脱敏）') });
  $('wf-unmask-wrap').hidden = false;
  $('wf-unmask').addEventListener('change', async () => {
    if ($('wf-unmask').checked) {
      const r = await riskConfirm({
        title: t('导出原值（不脱敏）'), level: 'high', code: 'export.unmasked', okText: t('确认导出原值'),
        lines: [
          t('导出的文件里是未脱敏的原值，文件流出后无法再控制。'),
          t('提交时必须写明理由，审批人会在工单里看到“导出原值”和你的理由，系统审计会留痕。'),
          t('SQL 防火墙要求强制脱敏的列仍会脱敏；执行时如果你已不是超级用户，会按脱敏设置导出。'),
        ],
      });
      if (!r.ok) $('wf-unmask').checked = false;
    }
    $('wf-unmask-reason-wrap').hidden = !$('wf-unmask').checked;
    if ($('wf-unmask').checked) {
      // 理由默认带上常用的一句，可以直接改；已经写过的不覆盖
      if (!$('wf-unmask-reason').value.trim()) $('wf-unmask-reason').value = t('业务需要查看原值');
      showCount();
      $('wf-unmask-reason').focus();
    }
  });
  // 理由框下面的字数：够 5 个字（提交时的下限）变绿
  const showCount = () => {
    const n = [...$('wf-unmask-reason').value].length;
    $('wf-unmask-count').textContent = `${n} / 200`;
    $('wf-unmask-count').classList.toggle('ok', n >= 5);
  };
  $('wf-unmask-reason').addEventListener('input', showCount);
}

// 合并成一个文件（批量导出）：各实例导出的数据依次写进同一个文件；改选项后之前的检查作废（要重新核对各实例的列）。
function initMerge() {
  if (!expOpt.can_merge) return;
  $('wf-merge-wrap').hidden = false;
  $('wf-merge').addEventListener('change', invalidate);
}

async function submit() {
  alertBox('');
  if (!checked) return alertBox(t('请先检查查询，通过后才能提交'));
  const g = currentGroup();
  const name = $('wf-name').value.trim();
  if (!name) return alertBox(t('请填写工单名称'));
  const ids = targetIds();
  const btn = $('wf-submit');
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  try {
    const body = {
      name, demand_url: $('wf-url').value.trim(), db_name: $('wf-db').value, group_name: g ? g.name : '', sql: editor.value,
      export: true, export_format: $('wf-format').value, run_date_start: $('wf-start').value, run_date_end: $('wf-end').value,
      plan_run_at: $('wf-plan').value,
    };
    if (batchOn()) body.instance_ids = ids;
    else body.instance_id = ids[0];
    if (unmaskOn()) {
      const reason = $('wf-unmask-reason').value.trim();
      if ([...reason].length < 5) {
        btn.disabled = false;
        btn.removeAttribute('aria-busy');
        $('wf-unmask-reason').focus();
        return alertBox(t('导出原值必须填写理由（至少 5 个字），审批人会看到'));
      }
      body.export_unmasked = true;
      body.export_unmask_reason = reason;
    }
    if (mergeOn()) body.export_merge = true;
    const res = await api('POST', '/api/v1/sqlworkflow', body);
    location.href = `/sqlworkflow/${res.workflow_id}`;
  } catch (e) {
    alertBox(e.message);
    btn.disabled = false;
    btn.removeAttribute('aria-busy');
  }
}

async function init() {
  try {
    const o = await api('GET', '/api/v1/sqlworkflow/options');
    groups = o.groups;
    expOpt = o.export || expOpt;
    batchTypes = new Set((o.batch && o.batch.types) || []);
    batchMax = (o.batch && o.batch.max) || batchMax;
    const sel = $('wf-group');
    if (!groups.length) sel.append(option('', t('你还没有加入任何资源组')));
    groups.forEach((g) => sel.append(option(g.id, g.name)));
    (expOpt.formats || []).forEach((f) => $('wf-format').append(option(f.value, tp(f.label))));
    if (!expOpt.allowed && !expOpt.batch_allowed) alertBox(t('你没有提交数据导出工单的权限（缺少 export.submit）'));
    initUnmask();
    initMerge();
    await fillInstances();
    showFlow($('flow-prev'), $('wf-group').value, $('flow-note'));
    const c = await loadCopySource();
    if (c) await applyCopy(c);
  } catch (e) {
    alertBox(e.message);
  }
}

// 复制为新工单：按终态导出工单的内容预填资源组、实例、库、格式和查询
async function applyCopy(c) {
  const notes = [];
  if (!c.is_export) notes.push(t('原工单不是数据导出工单'));
  const g = groups.find((x) => x.name === c.group_name);
  if (g) $('wf-group').value = String(g.id);
  else notes.push(t('原资源组「{name}」你已不在其中，请重新选择资源组和实例', { name: c.group_name }));
  $('wf-name').value = c.name || '';
  $('wf-url').value = c.demand_url || '';
  if (c.export_format && [...$('wf-format').options].some((o) => o.value === c.export_format)) $('wf-format').value = c.export_format;
  await fillInstances();
  showFlow($('flow-prev'), $('wf-group').value, $('flow-note'));
  if (g) {
    const ids = (c.instance_ids && c.instance_ids.length ? c.instance_ids : [c.instance_id]).map(String);
    const useBatch = ids.length > 1 && batchAvailable();
    if (useBatch) {
      $('wf-batch').checked = true;
      await fillInstances();
      const boxes = [...document.querySelectorAll('#wf-batch-checks input')];
      boxes.forEach((b) => (b.checked = ids.includes(b.value)));
      if (!boxes.some((b) => b.checked)) notes.push(t('原工单的实例你已无权使用，请重新勾选实例'));
      else if (boxes.filter((b) => b.checked).length < ids.length) notes.push(t('原工单的部分实例已不可选，已勾选其余的'));
      updateCount();
      await loadDatabases();
    } else {
      if (ids.length > 1) notes.push(t('当前账号不能提交批量数据导出工单，已只带出第一个实例'));
      const sel = $('wf-inst');
      if ([...sel.options].some((o) => o.value === ids[0])) {
        sel.value = ids[0];
        updateBatchWrap();
        await loadDatabases();
      } else notes.push(t('原工单的实例已不可选，请重新选择实例'));
    }
    const db = $('wf-db');
    if ([...db.options].some((o) => o.value === c.db_name)) db.value = c.db_name;
    else if (c.db_name) notes.push(t('原数据库「{name}」不在当前实例的库列表里，请重新选择', { name: c.db_name }));
  }
  editor.value = c.sql || '';
  alertBox(t('已从工单 #{id} 复制内容，请核对后重新检查并提交。', { id: c.source_id }) + (notes.length ? ' ' + notes.join(t('；')) + t('。') : ''), notes.length ? 'warn' : 'info');
}

$('wf-group').addEventListener('change', () => { $('wf-batch').checked = false; fillInstances(); showFlow($('flow-prev'), $('wf-group').value, $('flow-note')); });
$('wf-inst').addEventListener('change', () => { updateBatchWrap(); loadDatabases(); });
$('wf-db').addEventListener('change', invalidate);
$('wf-batch').addEventListener('change', fillInstances);
$('wf-batch-q').addEventListener('input', filterBatch);
$('wf-batch-q').addEventListener('keydown', (e) => { if (e.key === 'Enter') e.preventDefault(); });
$('wf-batch-all').addEventListener('click', () => {
  [...document.querySelectorAll('#wf-batch-checks label:not([hidden]) input')].slice(0, batchMax).forEach((b) => (b.checked = true));
  onSelectionChange();
});
$('wf-batch-none').addEventListener('click', () => {
  document.querySelectorAll('#wf-batch-checks input').forEach((b) => (b.checked = false));
  onSelectionChange();
});
$('wf-check').addEventListener('click', runCheck);
$('wf-submit').addEventListener('click', submit);
init();
