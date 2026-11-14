/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: apply.js
 * @time: 2026/10/28
 */
// 提交查询权限申请：选择实例、资源组、库表，提交后跳转到申请详情。
import { api, confirmRun, setButtonMeta } from '../common.js';
import { t } from '../i18n.js';

const $ = (id) => document.getElementById(id);
const els = {
  panel: $('apply-panel'), msg: $('apply-msg'), inst: $('ap-inst'), group: $('ap-group'), db: $('ap-db'), dbOne: $('ap-db-one'),
  dbsMany: $('ap-dbs-many'), dbChecks: $('ap-db-checks'), tablesWrap: $('ap-tables-wrap'), tableChecks: $('ap-table-checks'),
  tableQ: $('ap-table-q'), date: $('ap-date'), limit: $('ap-limit'), title: $('ap-title'), submit: $('ap-submit'),
  reason: $('ap-reason'), reasonWrap: $('ap-reason-wrap'),
};

let options = null;
let tables = [];
const pickedTables = new Set();

function setMsg(text, kind = 'err') {
  els.msg.replaceChildren();
  if (!text) return;
  const d = document.createElement('div');
  d.className = 'alert ' + kind;
  d.setAttribute('role', 'alert');
  d.textContent = text;
  els.msg.append(d);
}

const privType = () => Number(document.querySelector('input[name="ap-type"]:checked').value);

function fillSelect(sel, items, textOf = (x) => x, valueOf = (x) => x) {
  sel.replaceChildren();
  for (const it of items) sel.append(new Option(textOf(it), valueOf(it)));
}

function checkList(container, items, picked, onChange) {
  container.replaceChildren();
  if (!items.length) {
    const h = document.createElement('div');
    h.className = 'hint';
    h.textContent = t('没有可选项');
    container.append(h);
    return;
  }
  for (const name of items) {
    const label = document.createElement('label');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = name;
    cb.checked = picked.has(name);
    cb.addEventListener('change', () => {
      if (cb.checked) picked.add(name); else picked.delete(name);
      if (onChange) onChange();
    });
    label.append(cb, document.createTextNode(name));
    container.append(label);
  }
}

function syncType() {
  const ptype = privType();
  els.dbOne.hidden = ptype === 1;
  els.tablesWrap.hidden = ptype === 1;
  els.dbsMany.hidden = ptype === 2;
}

const pickedDBs = new Set();

// Redis 没有表：选到 Redis 实例时只能申请库（db 编号）级权限，表权限单选项置灰。
function lockRedis() {
  const inst = options && options.instances.find((i) => String(i.id) === els.inst.value);
  const redis = !!inst && inst.db_type === 'redis';
  const tableRadio = document.querySelector('input[name="ap-type"][value="2"]');
  if (redis) document.querySelector('input[name="ap-type"][value="1"]').checked = true;
  tableRadio.disabled = redis;
  tableRadio.title = redis ? t('Redis 没有表，只能申请库权限') : '';
  syncType();
}

// MongoDB 的“表”是集合：选到 Mongo 实例时把表选择区的标签和过滤框占位符换成“集合”。
const tableLbl = els.tablesWrap.querySelector('.lbl');
const origTableLbl = tableLbl ? tableLbl.textContent : '';
const origTableQ = els.tableQ.placeholder;
function syncEngine() {
  const inst = options && options.instances.find((i) => String(i.id) === els.inst.value);
  const type = inst ? inst.db_type : '';
  if (tableLbl) tableLbl.textContent = type === 'mongo' ? t('集合（可多选）') : origTableLbl;
  els.tableQ.placeholder = type === 'mongo' ? t('过滤集合名') : origTableQ;
}

async function loadDatabases(wants = []) {
  lockRedis();
  syncEngine();
  const id = els.inst.value;
  els.db.replaceChildren();
  els.dbChecks.replaceChildren();
  tables = [];
  els.tableChecks.replaceChildren();
  if (!id) return;
  try {
    const dbs = await api('GET', `/api/v1/query/instances/${encodeURIComponent(id)}/databases`);
    fillSelect(els.db, dbs);
    const hit = wants.filter((w) => dbs.includes(w));
    if (hit.length) els.db.value = hit[0];
    pickedDBs.clear();
    hit.forEach((w) => pickedDBs.add(w));
    checkList(els.dbChecks, dbs, pickedDBs);
    await loadTables();
  } catch (e) {
    setMsg(e.message);
  }
}

async function loadTables(wantTables = []) {
  const id = els.inst.value;
  const db = els.db.value;
  tables = [];
  pickedTables.clear();
  if (!id || !db) return renderTables();
  try {
    tables = await api('GET', `/api/v1/query/instances/${encodeURIComponent(id)}/databases/${encodeURIComponent(db)}/tables`);
    wantTables.filter((tb) => tables.includes(tb)).forEach((tb) => pickedTables.add(tb));
  } catch (e) {
    setMsg(e.message);
  }
  renderTables();
}

function renderTables() {
  const q = els.tableQ.value.trim().toLowerCase();
  checkList(els.tableChecks, tables.filter((tb) => !q || tb.toLowerCase().includes(q)), pickedTables);
}

const splitList = (s) => (s ? String(s).split(',').map((x) => x.trim()).filter(Boolean) : []);

async function open(prefill = {}) {
  els.panel.hidden = false;
  els.panel.scrollIntoView({ block: 'nearest' });
  setMsg('');
  try {
    if (!options) options = await api('GET', '/api/v1/query/apply/options');
  } catch (e) {
    return setMsg(e.message);
  }
  if (!options.instances.length) {
    return setMsg(t('你当前没有可以申请权限的实例，请联系管理员。'));
  }
  els.reasonWrap.hidden = !options.reason_required;
  fillSelect(els.inst, options.instances, (i) => i.name, (i) => String(i.id));
  if (prefill.instance && options.instances.some((i) => String(i.id) === String(prefill.instance))) els.inst.value = String(prefill.instance);
  fillSelect(els.group, options.groups);
  if (prefill.group && options.groups.includes(prefill.group)) els.group.value = prefill.group;
  if (!els.date.value) {
    const d = new Date();
    d.setDate(d.getDate() + 30);
    els.date.value = d.toISOString().slice(0, 10);
  }
  if (prefill.limit) els.limit.value = prefill.limit;
  if (prefill.reason) els.reason.value = prefill.reason;
  // 权限类型：显式指定优先；否则有表名按表权限，只有库名按库权限
  const type = prefill.type ? String(prefill.type) : prefill.table ? '2' : prefill.db ? '1' : '2';
  document.querySelector(`input[name="ap-type"][value="${type === '1' ? '1' : '2'}"]`).checked = true;
  syncType();
  await loadDatabases(splitList(prefill.db));
  if (type === '2' && prefill.table) await loadTables(splitList(prefill.table));
}

async function submit() {
  setMsg('');
  const ptype = privType();
  const body = {
    instance_id: Number(els.inst.value), group_name: els.group.value, priv_type: ptype, valid_date: els.date.value,
    limit_num: Number(els.limit.value), title: els.title.value.trim(),
  };
  if (!els.reasonWrap.hidden) {
    body.reason = els.reason.value.trim();
    if ([...body.reason].length < 5) return setMsg(t('请填写申请理由（至少 5 个字）'));
  }
  if (ptype === 1) {
    body.db_list = [...pickedDBs];
    if (!body.db_list.length) return setMsg(t('请至少选择一个数据库'));
  } else {
    body.db_name = els.db.value;
    body.table_list = [...pickedTables];
    if (!body.db_name || !body.table_list.length) return setMsg(t('请选择数据库，并至少勾选一张表'));
  }
  els.submit.disabled = true;
  els.submit.setAttribute('aria-busy', 'true');
  try {
    const res = await api('POST', '/api/v1/query/privilege-applies', body);
    location.href = '/query/applies/' + res.apply_id;
  } catch (e) {
    setMsg(e.message);
    els.submit.disabled = false;
    els.submit.removeAttribute('aria-busy');
  }
}

$('new-apply').addEventListener('click', () => open());
$('ap-cancel').addEventListener('click', () => { els.panel.hidden = true; });
els.submit.addEventListener('click', submit);
els.inst.addEventListener('change', () => loadDatabases());
els.db.addEventListener('change', () => loadTables());
els.tableQ.addEventListener('input', renderTables);
document.querySelectorAll('input[name="ap-type"]').forEach((r) => r.addEventListener('change', syncType));

// 从在线查询页（实例、库、表）或申请详情的“重新申请”（另带类型、行数、理由、资源组）跳转过来时预填。
const params = new URLSearchParams(location.search);
if (params.get('new') === '1') {
  open({
    instance: params.get('instance'), db: params.get('db'), table: params.get('table'), type: params.get('type'),
    limit: params.get('limit'), reason: params.get('reason'), group: params.get('group'),
  });
}

// 状态筛选改变即刷新列表
const filterForm = document.getElementById('ap-filter');
if (filterForm) filterForm.querySelector('select[name="status"]').addEventListener('change', () => filterForm.submit());

// 列表行内撤回（申请人本人、待审批）
setButtonMeta({ 'qpriv.apply': t('申请 / 撤回查询权限') });
document.querySelectorAll('[data-abort-id]').forEach((btn) => {
  btn.addEventListener('click', async () => {
    const aid = btn.dataset.abortId;
    const tr = btn.closest('tr');
    const cell = tr && tr.children[1] && tr.children[1].firstChild; // 标题单元格里后面还跟着“理由”小字，只取第一段文字
    const title = cell ? cell.textContent.trim() : '';
    // 失败（如别人刚审批完）时错误留在弹窗里，可重试；取消不发请求
    const r = await confirmRun({
      title: t('撤回权限申请 #{id}', { id: aid }), code: 'qpriv.apply', okText: t('确认撤回'),
      lines: [t('撤回后流程终止，不能恢复；需要时可重新提交申请。')],
      details: [{ k: t('申请号'), v: '#' + aid }, ...(title ? [{ k: t('标题'), v: title }] : [])],
    }, () => api('POST', `/api/v1/query/privilege-applies/${encodeURIComponent(aid)}/abort`, { remark: '申请人主动撤回' })); // i18n-ignore 撤回备注存库（审批日志）
    if (r.ok) { btn.disabled = true; location.reload(); }
  });
});
