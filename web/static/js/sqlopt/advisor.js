/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: advisor.js
 * @time: 2026/10/28
 */
// SQL 优化工具：内置规则建议 / SOAR / SQLTuning / 执行计划。
// 外部工具与 SQLTuning 是后台任务：提交后轮询，结果出来再取；内置建议、执行计划是同步接口。
import { api } from '../common.js';
import { t } from '../i18n.js';
import { createEditor } from '../query/editor.js';
import { API, el, option, setMsg, loadOptions, loadDatabases, pollJob, renderReport, renderTable, renderMarkdown, renderFindings } from './shared.js';

const $ = (id) => document.getElementById(id);
const els = {
  msg: $('ad-msg'), inst: $('ad-inst'), db: $('ad-db'), tool: $('ad-tool'),
  tuningWrap: $('ad-tuning-wrap'), format: $('ad-format'), explain: $('ad-explain'), run: $('ad-run'), cancel: $('ad-cancel'),
  status: $('ad-status'), tabs: $('ad-tabs'), result: $('ad-result'),
};

const editor = createEditor($('ad-editor'), { onRun: () => run(), onChange: () => {} });

const TOOLS = [
  { value: 'builtin', label: t('内置规则建议'), perm: 'explain' },
  { value: 'sqltuning', label: 'SQLTuning', perm: 'sqltuning' },
  { value: 'soar', label: 'SOAR', perm: 'soar' },
];
const TAB_LABEL = { builtin: t('内置建议'), plan: t('执行计划'), soar: t('SOAR 报告'), sqltuning: 'SQLTuning' };

let opts = { instances: [], tools: {}, perms: {} };
let panes = {}; // key → DOM
let poller = null;
let curJob = null;

const selInst = () => opts.instances.find((i) => String(i.id) === els.inst.value);

function setBusy(busy, text = '') {
  for (const b of [els.format, els.explain, els.run]) b.disabled = busy;
  els.cancel.hidden = !(busy && curJob);
  els.status.textContent = text;
}

// ---- 标签页 ----

function showTab(key) {
  for (const b of els.tabs.querySelectorAll('.tab')) b.classList.toggle('on', b.dataset.tab === key);
  els.result.replaceChildren(panes[key]);
}

function putPane(key, node) {
  panes[key] = node;
  let btn = els.tabs.querySelector(`[data-tab="${key}"]`);
  if (!btn) {
    btn = el('button', 'tab', TAB_LABEL[key]);
    btn.type = 'button';
    btn.dataset.tab = key;
    btn.addEventListener('click', () => showTab(key));
    els.tabs.append(btn);
  }
  showTab(key);
}

// ---- 选项联动 ----

function refreshTools() {
  const inst = selInst();
  const prev = els.tool.value;
  els.tool.replaceChildren();
  for (const tl of TOOLS) {
    if (!opts.perms[tl.perm]) continue;
    const o = option(tl.value, tl.label);
    // 内置规则建议不选实例也能用（只做静态规则）；其余方式必须选一个 MySQL 系的实例
    o.disabled = tl.value === 'builtin' ? !!inst && !inst.tools : !inst || !inst.tools;
    els.tool.append(o);
  }
  if (!els.tool.options.length) els.tool.append(option('', t('没有可用的优化方式（缺少权限）')));
  const keep = [...els.tool.options].find((o) => o.value === prev && !o.disabled);
  const first = [...els.tool.options].find((o) => !o.disabled);
  if (keep) els.tool.value = prev;
  else if (first) els.tool.value = first.value;
  syncToolUI();
}

function syncToolUI() {
  const inst = selInst();
  const tl = els.tool.value;
  els.tuningWrap.hidden = tl !== 'sqltuning';
  const picked = els.tool.selectedOptions[0];
  els.run.disabled = !tl || !picked || picked.disabled;
  els.explain.disabled = !inst || !inst.explain || !opts.perms.explain;
  if (inst && !inst.tools) els.status.textContent = t('该类型的实例只提供“执行计划”');
  else if (tl && opts.tools[tl] && !opts.tools[tl].ready) {
    els.status.textContent = opts.tools[tl].message;
    if (opts.can_configure) {
      const a = el('a', '', t('去配置'));
      a.href = '/settings/config#optimizer';
      els.status.append(' ', a);
    }
  } else els.status.textContent = '';
}

els.tool.addEventListener('change', syncToolUI);

els.inst.addEventListener('change', async () => {
  els.db.replaceChildren(option('', t('请选择数据库')));
  els.db.disabled = !els.inst.value;
  refreshTools();
  setMsg(els.msg, '');
  if (!els.inst.value) return;
  try {
    for (const d of await loadDatabases(els.inst.value, els.msg)) els.db.append(option(d, d));
  } catch (e) {
    setMsg(els.msg, e.message);
  }
});

// ---- 取 SQL ----

function sqlText() {
  const s = editor.sqlToRun().trim();
  if (!s) throw new Error(t('请输入 SQL'));
  return s;
}

function needInst(needDB) {
  if (!els.inst.value) throw new Error(t('请选择实例'));
  if (needDB && !els.db.value) throw new Error(t('请选择数据库'));
  return Number(els.inst.value);
}

// ---- 按钮 ----

els.format.addEventListener('click', async () => {
  setMsg(els.msg, '');
  const text = editor.value;
  if (!text.trim()) return;
  try {
    const r = await api('POST', `${API}/format`, { sql: text });
    editor.value = r.sql;
  } catch (e) {
    setMsg(els.msg, e.message);
  }
});

async function explain(format = 'traditional') {
  setMsg(els.msg, '');
  let body;
  try {
    body = { instance_id: needInst(false), db: els.db.value, sql: sqlText(), format };
  } catch (e) {
    setMsg(els.msg, e.message);
    return;
  }
  setBusy(true, t('正在获取执行计划…'));
  try {
    const r = await api('POST', `${API}/explain`, body);
    const root = el('div');
    const bar = el('div', 'actions');
    const sw = el('button', 'btn ghost', format === 'json' ? t('切换为表格') : t('切换为 JSON 格式'));
    sw.type = 'button';
    sw.addEventListener('click', () => explain(format === 'json' ? 'traditional' : 'json'));
    bar.append(el('span', 'muted small', 'EXPLAIN ' + (format === 'json' ? 'FORMAT=JSON ' : '') + r.statement), el('span', 'spacer'), sw);
    root.append(bar);
    if (r.plan) root.append(renderTable(r.plan));
    if (r.json) root.append(el('pre', 'sqlbox mono so-plan-json', r.json));
    if (r.findings && r.findings.length) {
      root.append(el('div', 'so-sec', t('执行计划解读')), renderFindings(r.findings));
    } else if (r.plan) {
      root.append(el('div', 'so-empty', t('执行计划里没有发现明显问题')));
    }
    putPane('plan', root);
  } catch (e) {
    setMsg(els.msg, e.message);
  } finally {
    setBusy(false);
  }
}
els.explain.addEventListener('click', () => explain());

async function run() {
  if (els.run.disabled) return;
  setMsg(els.msg, '');
  const tool = els.tool.value;
  try {
    if (tool === 'builtin') return await runBuiltin();
    return await runTool(tool);
  } catch (e) {
    setBusy(false);
    setMsg(els.msg, e.message);
  }
}
els.run.addEventListener('click', run);

async function runBuiltin() {
  const body = { instance_id: els.inst.value ? Number(els.inst.value) : 0, db: els.db.value, sql: sqlText() };
  setBusy(true, t('正在分析…'));
  try {
    const rep = await api('POST', `${API}/advise`, body);
    const root = el('div');
    renderReport(root, rep);
    putPane('builtin', root);
  } finally {
    setBusy(false);
  }
}

async function runTool(tool) {
  const body = { tool, instance_id: needInst(true), db: els.db.value, sql: sqlText() };
  if (tool === 'sqltuning') {
    body.options = ['sys_parm', 'sql_plan', 'obj_stat', 'sql_profile'].filter((k) => $('ad-opt-' + k).checked);
    if (!body.options.length) throw new Error(t('请至少选择一项 SQLTuning 要显示的内容'));
  }
  const v = await api('POST', `${API}/optimize`, body);
  curJob = v;
  setBusy(true, t('{name} 正在运行，请稍候…', { name: TAB_LABEL[tool] }));
  poller = pollJob(v.id, (j) => { curJob = j; });
  try {
    const done = await poller.promise;
    if (done.status === 'canceled') { setMsg(els.msg, t('已取消'), 'info'); return; }
    if (done.status === 'failed') throw new Error(done.error || t('任务失败'));
    const item = await api('GET', `${API}/jobs/${v.id}/items/0`);
    if (item.error || !item.result) throw new Error(item.error || t('没有取到结果'));
    showOptimize(item.result);
  } finally {
    curJob = null;
    poller = null;
    setBusy(false);
  }
}

els.cancel.addEventListener('click', async () => {
  if (!curJob) return;
  try { await api('POST', `${API}/jobs/${curJob.id}/cancel`); } catch (e) { setMsg(els.msg, e.message); }
});

// ---- 结果渲染 ----

function showOptimize(res) {
  const root = el('div');
  if (res.truncated) root.append(el('div', 'alert warn', t('输出过长，已截断')));
  if (res.tool === 'soar') {
    root.append(renderMarkdown(res.output || t('（SOAR 没有输出）')));
    putPane('soar', root);
  } else {
    root.append(renderTuning(res.tuning));
    putPane('sqltuning', root);
  }
}

function kvTable(tbl) {
  return tbl ? renderTable(tbl) : el('div', 'so-empty', t('没有数据'));
}

function details(title, open = false) {
  const d = el('details', 'acc');
  d.open = open;
  d.append(el('summary', '', title));
  const body = el('div');
  body.style.padding = '8px 12px';
  d.append(body);
  return [d, body];
}

function renderTuning(tn) {
  const root = el('div');
  if (tn.errors && tn.errors.length) for (const e of tn.errors) root.append(el('div', 'alert warn', e));
  if (tn.basic && tn.basic.rows.length) {
    root.append(el('div', 'alert', 'BASIC INFORMATION · MySQL SQL Tuning · ' + tn.basic.rows[0][0]));
  }
  if (tn.sys_params) {
    const [d, b] = details('SYSTEM PARAMETER');
    b.append(kvTable(tn.sys_params));
    if (tn.optimizer_switch) { b.append(el('div', 'so-sec', 'optimizer_switch'), kvTable(tn.optimizer_switch)); }
    root.append(d);
  }
  if (tn.plan) {
    const [d, b] = details('SQL PLAN', true);
    b.append(kvTable(tn.plan));
    if (tn.rewrite) b.append(el('div', 'so-sec', t('OPTIMIZER REWRITE SQL（SHOW WARNINGS）')), kvTable(tn.rewrite));
    root.append(d);
  }
  if (tn.objects && tn.objects.length) {
    const [d, b] = details('OBJECT STATISTICS', true);
    for (const o of tn.objects) {
      b.append(el('div', 'so-sec', o.table));
      if (o.error) { b.append(el('div', 'alert warn', o.error)); continue; }
      if (o.structure) b.append(el('pre', 'sqlbox mono', o.structure));
      if (o.info) b.append(el('div', 'so-sec', 'TABLE INFO'), kvTable(o.info));
      if (o.indexes) b.append(el('div', 'so-sec', 'INDEX INFO'), kvTable(o.indexes));
    }
    root.append(d);
  }
  if (tn.profile) {
    const p = tn.profile;
    const [d, b] = details('STATUS & PROFILING', true);
    if (p.error) b.append(el('div', 'alert warn', p.error));
    b.append(el('div', 'alert', t('EXECUTE TIME：{sec} s · 读取 {rows} 行', { sec: p.execute_seconds.toFixed(4), rows: p.rows })));
    if (p.status_diff) b.append(el('div', 'so-sec', 'SESSION STATUS (DIFFERENT)'), kvTable(p.status_diff));
    if (p.detail) b.append(el('div', 'so-sec', 'SQL PROFILING (DETAIL)'), kvTable(p.detail));
    if (p.summary) b.append(el('div', 'so-sec', 'SQL PROFILING (SUMMARY)'), kvTable(p.summary));
    root.append(d);
  }
  return root;
}

// ---- 初始化 ----

async function init() {
  try {
    opts = await loadOptions();
  } catch (e) {
    setMsg(els.msg, e.message);
    return;
  }
  for (const i of opts.instances) els.inst.append(option(i.id, t('{name}（{label}）', { name: i.name, label: i.db_type })));
  refreshTools();
  await handoff();
}

const ymd = (d) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;

// 按慢日志的 SQL 指纹取一条样例 SQL（最近 31 天里耗时最长的一条）。没有“慢查日志”的权限或取不到时返回 null。
async function sampleFor(instId, db, fp) {
  const end = new Date();
  const start = new Date(Date.now() - 30 * 86400000);
  const q = new URLSearchParams({ instance_id: String(instId), sql_id: fp, start: ymd(start), end: ymd(end), sort: 'query_time_sum', order: 'desc', limit: '1' });
  if (db) q.set('db_name', db);
  try {
    const page = await api('GET', `/api/v1/slowlog/review/history?${q}`);
    const row = (page.rows || [])[0];
    return row && row.sql_text ? row.sql_text : null;
  } catch (_) {
    return null;
  }
}

// 从“慢查日志”页跳转过来，两种方式都支持：
//   1. URL 参数 /sqladvisor?inst=<实例ID>&db=<库>&sql_id=<指纹>：预选实例和库，按指纹到慢日志里取一条样例 SQL 填入；
//   2. sessionStorage：直接带着 SQL、实例名、库名。
// 取到 SQL 就自动运行；只带了实例 / 库时只预选，等用户自己填 SQL。
async function handoff() {
  const qs = new URLSearchParams(location.search);
  let sql, dbName, instName;
  try {
    sql = sessionStorage.getItem('advisor_sql_content');
    dbName = sessionStorage.getItem('advisor_db_name');
    instName = sessionStorage.getItem('advisor_instance_name');
    ['advisor_sql_content', 'advisor_db_name', 'advisor_instance_name'].forEach((k) => sessionStorage.removeItem(k));
  } catch (_) { /* 没有 sessionStorage 时只看 URL 参数 */ }
  const instQ = qs.get('inst');
  const fp = qs.get('sql_id');
  dbName = qs.get('db') || dbName;
  let inst = null;
  if (instQ) inst = opts.instances.find((i) => String(i.id) === instQ);
  else if (instName) inst = opts.instances.find((i) => i.name === instName);
  if (instQ && !inst) setMsg(els.msg, t('无法预选实例：不存在、无权限或类型不支持。'), 'warn');
  if (!inst) {
    if (sql) editor.value = sql;
    return;
  }
  els.inst.value = String(inst.id);
  els.db.disabled = false;
  refreshTools();
  try {
    for (const d of await loadDatabases(inst.id, els.msg)) els.db.append(option(d, d));
  } catch (e) {
    setMsg(els.msg, e.message);
    return;
  }
  if (dbName && [...els.db.options].some((o) => o.value === dbName)) els.db.value = dbName;
  // 从慢日志过来默认用 SQLTuning（要先选好库）；没有这项权限或没选到库时退回内置规则建议
  const usable = (v) => [...els.tool.options].find((o) => o.value === v && !o.disabled && (v !== 'sqltuning' || els.db.value));
  const pick = usable('sqltuning') || usable('builtin');
  if (pick) { els.tool.value = pick.value; syncToolUI(); }
  if (!sql && fp) {
    sql = await sampleFor(inst.id, dbName, fp);
    if (!sql) setMsg(els.msg, t('没有取到该慢 SQL 的样例，请手动粘贴 SQL。'), 'info');
  }
  if (!sql) return;
  editor.value = sql;
  if (pick) run();
}

init();
