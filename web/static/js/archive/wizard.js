/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: wizard.js
 * @time: 2026/10/28
 */
// 新建 / 编辑归档任务向导：来源 → 条件 → 模式与备份 → 稳定性参数 → 调度 → 试运行并提交。
// 所有“算”的事（最终 WHERE、将执行的 SQL、调度预览、试运行）都调服务端接口；这里只做必填 / 范围等即时提示，以服务端为准。
// 约定：向导的工作状态在 W.t；函数里的局部变量一律叫 task，不叫 t（免得遮住翻译函数 t()）。
import { applyButtons } from '../btnperm.js';
import { t, tp, isZh } from '../i18n.js';
import { A, issuesOf } from './api.js';
import { $, el, add, nf, button, pbutton, fillSelect, getPath, setPath, go, notifyChanged, fmtDur, withNodes } from './util.js';
import { flash, notice, tag, kvList, h4, code, bold, sqlBox, small } from './ui.js';
import { S, MODES, can, limits, policy, modeTag, backupTags } from './consts.js';
import { STEPS, TIME_UNITS, blankTask, fromTask, toInput, keyHash, stepOfField, stepErrors, sleepMin, timeCandidates, defaultTimeColumn, colOf, isIntTime, probeNotes } from './wizard_model.js';
import { checkNode, sqlBlocks } from './check_view.js';
import { WD_FULL, inWin, minOf } from './sched.js';

const root = () => $('ar-view-wizard');
let W = null;
let pvTimer = 0;

const STORE_BAD = (s) => s.enabled === false || s.status === 'err' || s.status === 'off';

// ---------------------------------------------------------------- 小部件
function formGrid() {
  return el('div', 'ar-form');
}

function row(form, label, control, opt = {}) {
  const l = el('label', opt.req ? 'req' : '', label);
  const v = el('div', 'fv');
  add(v, control);
  form.append(l, v);
  return v;
}

function input(type, path, opt = {}) {
  const i = el('input');
  i.type = type;
  const cur = getPath(W.t, path);
  if (type === 'checkbox') i.checked = !!cur;
  else i.value = cur === undefined || cur === null ? '' : String(cur);
  if (opt.maxLength) i.maxLength = opt.maxLength;
  if (opt.placeholder) i.placeholder = opt.placeholder;
  if (opt.width) i.style.maxWidth = opt.width + 'px';
  if (opt.min !== undefined) i.min = String(opt.min);
  if (opt.max !== undefined) i.max = String(opt.max);
  if (type === 'number') i.step = '1';
  if (type === 'text') i.autocomplete = 'off';
  if (opt.disabled) i.disabled = true;
  i.addEventListener(type === 'checkbox' || opt.onChange ? 'change' : 'input', () => {
    setPath(W.t, path, type === 'checkbox' ? i.checked : i.value);
    if (opt.after) opt.after();
    else touched();
  });
  return i;
}

function select(path, items, textOf, valueOf, opt = {}) {
  const s = el('select');
  fillSelect(s, items, textOf, valueOf, opt.first, getPath(W.t, path));
  if (opt.disabled) s.disabled = true;
  s.setAttribute('aria-label', opt.label || path);
  s.addEventListener('change', () => {
    setPath(W.t, path, opt.num ? Number(s.value) || 0 : s.value);
    if (opt.after) opt.after(s.value);
    else touched();
  });
  return s;
}

// 任何字段变化：刷新动态区域，并（防抖）重新向服务端要预览
function touched() {
  updDyn();
  schedulePreview();
}

// ---------------------------------------------------------------- 预览（服务端计算）
function schedulePreview() {
  clearTimeout(pvTimer);
  pvTimer = setTimeout(runPreview, 350);
}

async function runPreview() {
  clearTimeout(pvTimer);
  pvTimer = 0;
  if (!W) return;
  const seq = ++W.pvSeq;
  try {
    const pv = await A.preview({ ...toInput(W.t), task_id: W.edit ? W.edit.id : undefined });
    if (!W || seq !== W.pvSeq) return;
    W.pv = pv;
    updDyn();
  } catch (e) {
    if (e.code === 'not_ready') return;
    if (W && seq === W.pvSeq) W.pvErr = e.message;
  }
}

async function flushPreview() {
  if (pvTimer) await runPreview();
}

const issuesFor = (step) => ((W.pv && W.pv.issues) || []).filter((i) => i.level === 'error' && stepOfField(i.field) === step).map((i) => tp(i.msg));

function errsOf(step) {
  const ctx = { edit: W.edit, cols: W.cols, instances: W.src.instances };
  const out = stepErrors(W.t, step, ctx);
  for (const m of issuesFor(step)) if (!out.includes(m)) out.push(m);
  return out;
}

const allErrs = () => [0, 1, 2, 3, 4].flatMap((s) => errsOf(s).map((m) => ({ step: s, m })));

function needsApproval() {
  if (!W.edit) return true;
  return !(W.pv && W.pv.needs_approval === false);
}

function checkValid() {
  const c = W.check;
  if (!c) return false;
  if (c.keyHash !== keyHash(W.t)) return false;
  const exp = Date.parse(String(c.expires_at || '').replace(' ', 'T'));
  return !(exp && exp < Date.now());
}

function blockReason() {
  const e = allErrs();
  if (e.length) return t('第 {n} 步有问题：{msg}', { n: e[0].step + 1, msg: e[0].m });
  if (!can('archive.apply')) return t('没有提交权限（archive.apply）');
  if (!needsApproval()) return '';
  const c = W.check;
  if (!c) return t('提交前必须先试运行');
  if (!checkValid()) return t('试运行结果已失效（条件 / 目标 / 来源有改动或超过 7 天），请重新试运行');
  if (c.blocked) return t('试运行有红色检查项，不能提交');
  if (c.need_ack_warn > 0 && !W.ack.warn) return t('试运行有 {n} 个黄色警告，请勾选“已知晓”', { n: c.need_ack_warn });
  if (c.need_ack_big && !W.ack.big) return t('命中比例超过 30%，请勾选“已确认大批量”');
  return '';
}

// ---------------------------------------------------------------- 来源数据加载
async function loadDbs(instId) {
  if (!instId) return [];
  if (!W.src.dbs[instId]) {
    try { W.src.dbs[instId] = await A.databases(instId); } catch (e) { flash(e.message, 'err'); W.src.dbs[instId] = []; }
  }
  return W.src.dbs[instId];
}

async function loadTables() {
  W.src.tables = [];
  if (W.t.instance_id && W.t.db) {
    try { W.src.tables = await A.tables(W.t.instance_id, W.t.db); } catch (e) { flash(e.message, 'err'); }
  }
}

async function loadCols() {
  W.cols = null;
  const t = W.t;
  if (t.instance_id && t.db && t.table) {
    try { W.cols = await A.columns(t.instance_id, t.db, t.table); } catch (e) { flash(e.message, 'err'); }
  }
}

async function loadFlow() {
  W.flow = null;
  if (W.t.resource_group_id) {
    try { W.flow = await A.flow(W.t.resource_group_id); } catch (_) { W.flow = null; }
  }
}

const instOf = (id) => W.src.instances.find((i) => i.id === Number(id));
const rgsOf = (id) => (instOf(id) ? instOf(id).resource_groups || [] : []);
const defaultArchiveDb = (dbs) => (dbs.find((x) => /archive/i.test(x.name)) || dbs[0] || { name: '' }).name;

// ---------------------------------------------------------------- 入口 / 退出
export async function show(editId) {
  const root_ = root();
  root_.hidden = false;
  if (W && (W.editId || 0) === (editId || 0) && root_.firstChild) {
    drawAll();
    return;
  }
  root_.replaceChildren(el('div', 'muted', t('加载中…')));
  W = {
    editId: editId || 0, edit: null, t: null, step: 0, src: { instances: [], dbs: {}, tables: [] }, cols: null, flow: null,
    pv: null, pvSeq: 0, pvErr: '', check: null, ack: { warn: false, big: false }, drying: false, serverErrs: [], busy: false,
    probe: null, // 整型时间列的探测：{ key, loading, data, err }
  };
  try {
    W.src.instances = await A.instances();
    if (editId) {
      W.edit = await A.task(editId);
      W.t = fromTask(W.edit);
      if (!instOf(W.edit.instance_id)) {
        W.src.instances.push({ id: W.edit.instance_id, name: W.edit.instance_name, resource_groups: [{ id: W.edit.resource_group_id, name: W.edit.resource_group_name, has_flow: true }] });
      }
      await Promise.all([loadDbs(W.t.instance_id), loadTables(), loadCols(), loadFlow()]);
      if (W.t.dt_on) await loadDbs(W.t.dt.instance_id);
    } else {
      W.t = blankTask();
    }
  } catch (e) {
    root_.replaceChildren();
    root_.append(notice(e.message, 'bad'), button(t('返回任务列表'), '', () => go('#tasks')));
    W = null;
    return;
  }
  drawAll();
  schedulePreview();
}

export function hide() {
  root().hidden = true;
}

function exit(toHash) {
  clearTimeout(pvTimer);
  W = null;
  root().replaceChildren();
  go(toHash || '#tasks');
}

// ---------------------------------------------------------------- 外框
function drawAll() {
  const r = root();
  r.replaceChildren();
  if (!can('archive.apply')) {
    r.append(notice(t('你没有新建 / 修改归档任务的权限（archive.apply）。'), 'warn'), button(t('返回任务列表'), '', () => exit()));
    return;
  }
  const bar = el('div', 'toolbar');
  bar.append(button(t('← 返回任务列表'), '', () => exit()), el('b', '', W.edit ? t('编辑归档任务「{name}」', { name: W.edit.name }) : t('新建归档任务')));
  const card = el('div', 'card');
  const steps = el('ol', 'ar-steps');
  steps.id = 'ar-w-steps';
  const body = el('div');
  body.id = 'ar-w-body';
  const err = el('div');
  err.id = 'ar-w-err';
  const nav = el('div', 'ar-wiz-nav');
  nav.id = 'ar-w-nav';
  card.append(steps, body, err, nav);
  r.append(bar, card);
  drawStep();
}

function drawSteps() {
  const ol = $('ar-w-steps');
  ol.replaceChildren();
  STEPS.forEach((n, i) => {
    const li = el('li', i < W.step ? 'done' : i === W.step ? 'cur' : '');
    li.append(el('b', '', i < W.step ? '✓' : String(i + 1)), el('span', '', n));
    if (i === W.step) li.setAttribute('aria-current', 'step');
    ol.append(li);
  });
  // 窄屏下步骤条可横向滑动：把当前步滚到可见区域（只动步骤条自己，不滚页面）
  const curLi = ol.querySelector('li.cur');
  if (curLi && ol.scrollWidth > ol.clientWidth) ol.scrollLeft = Math.max(0, curLi.offsetLeft - (ol.clientWidth - curLi.offsetWidth) / 2);
}

function drawStep() {
  drawSteps();
  const body = $('ar-w-body');
  body.replaceChildren();
  [step0, step1, step2, step3, step4, step5][W.step](body);
  updDyn();
  applyButtons(root());
}

function errNode() {
  const box = el('div');
  const e = W.step < 5 ? errsOf(W.step).map((m) => m) : allErrs().map((x) => t('第 {n} 步：{msg}', { n: x.step + 1, msg: x.m }));
  const lines = [...e, ...W.serverErrs];
  if (!lines.length) return box;
  const ul = el('ul', 'ar-err-list');
  lines.forEach((m) => ul.append(el('li', '', m)));
  box.append(notice([bold(W.step < 5 ? t('本步还有问题') : t('提交前需要先修正')), ul], 'bad', '✕'));
  return box;
}

function navNode() {
  const nav = document.createDocumentFragment();
  const prev = button(t('上一步'), '', () => { W.step -= 1; W.serverErrs = []; drawStep(); window.scrollTo(0, 0); });
  prev.disabled = W.step === 0;
  nav.append(prev);
  if (W.step < 5) {
    nav.append(button(t('下一步'), 'primary', next));
  } else {
    const why = blockReason();
    const label = needsApproval() ? t('提交申请') : t('保存修改');
    const submitBtn = pbutton('archive.apply', label, 'primary', () => submit());
    if (why) {
      submitBtn.disabled = true;
      submitBtn.title = why;
    }
    nav.append(submitBtn);
    const canDraft = !W.edit || ['draft', 'rejected'].includes(W.edit.status);
    if (canDraft) nav.append(pbutton('archive.apply', W.edit ? t('保存修改（不提交）') : t('保存草稿'), '', () => submit(true)));
    if (why) {
      const s = el('span', 'small', why);
      s.style.color = 'var(--dt-crit)';
      nav.append(s);
    }
  }
  nav.append(el('span', 'spacer'), button(t('取消'), '', () => exit()));
  return nav;
}

// 只刷新动态区域，不重建输入框（避免输入时丢焦点）
function updDyn() {
  if (!W) return;
  const err = $('ar-w-err');
  if (err) err.replaceChildren(errNode());
  const nav = $('ar-w-nav');
  if (nav) {
    nav.replaceChildren(navNode());
    applyButtons(nav);
  }
  const fill = (id, node) => { const b = $(id); if (b) b.replaceChildren(node); };
  if ($('ar-w-where')) fill('ar-w-where', sqlBox((W.pv && W.pv.where) || t('（填写条件后由服务端生成）')));
  if ($('ar-w-sql')) fill('ar-w-sql', sqlBlocks(W.pv && W.pv.sql, W.t.mode));
  if ($('ar-w-tpl')) fill('ar-w-tpl', tplExample());
  if ($('ar-w-sched')) fill('ar-w-sched', schedPreview());
  if ($('ar-w-pest')) fill('ar-w-pest', document.createTextNode(paramEstimate()));
}

async function next() {
  await flushPreview();
  const e = errsOf(W.step);
  if (e.length) {
    flash(e[0], 'err');
    updDyn();
    return;
  }
  W.step += 1;
  W.serverErrs = [];
  drawStep();
  window.scrollTo(0, 0);
}

// ---------------------------------------------------------------- 第 1 步：来源
function flowNode() {
  const f = W.flow;
  const box = el('div');
  if (!W.t.resource_group_id) return box;
  if (!f || !f.exists) {
    box.append(notice(t('该资源组没有配置“数据归档申请”审批流，不能提交。'), 'bad', '✕'));
    return box;
  }
  const parts = (f.levels || []).map((l, i) => t('第 {n} 级 {group}（{users}）', { n: i + 1, group: l.group_name, users: (l.users || []).length ? l.users.join(isZh ? '、' : ', ') : t('本级无人可审，仅超级用户可审') }));
  box.append(notice([t('审批流：'), parts.join(' → ')], (f.levels || []).some((l) => !(l.users || []).length) ? 'warn' : 'info'));
  return box;
}

function tableInfoNode() {
  const c = W.cols;
  const box = el('div');
  if (!c) return box;
  box.append(h4(t('表信息')));
  const cand = timeCandidates(c);
  const colNote = (x) => [code(x.name), el('span', 'small muted', ` ${x.type}${x.indexed ? t(' · 有索引') : t(' · 无索引')}  `)];
  const tags = [];
  if (c.partitioned) tags.push(tag(t('分区表（按普通表处理）'), 'warn'));
  if ((c.fk_parents || []).length) tags.push(tag(t('被外键子表引用'), 'bad'));
  if (c.protected) tags.push(tag(t('受保护库表'), 'bad'));
  if (c.has_triggers) tags.push(tag(t('有触发器'), 'info'));
  box.append(kvList([
    [t('行数'), t('≈ {n} 行', { n: nf(c.rows) })],
    [t('主键'), [code((c.pk_cols || []).join(', ') || t('无')), ' ', c.has_pk ? '' : tag(t('无主键，不允许建任务'), 'bad')]],
    [t('可作时间列'), cand.dt.length ? cand.dt.map(colNote) : cand.ints.length ? el('span', 'small muted', t('没有 datetime / timestamp / date 列')) : tag(t('没有可作时间列的列'), 'bad')],
    ...(cand.ints.length ? [[t('整型列（可能是时间戳）'), cand.hinted.length
      ? [cand.hinted.slice(0, 8).map(colNote), cand.hinted.length > 8 || cand.rest.length ? el('span', 'small muted', t('另有 {n} 个整型列', { n: Math.max(0, cand.hinted.length - 8) + cand.rest.length })) : '']
      : el('span', 'small muted', t('{n} 个 int / bigint 列，没有名字像时间的；若其中存着 10 位秒 / 13 位毫秒时间戳，可在下一步选择', { n: cand.ints.length }))]] : []),
    [t('其它'), tags.length ? tags : '—'],
  ], true));
  return box;
}

function step0(body) {
  const task = W.t;
  const lock = !!W.edit;
  const f = formGrid();
  row(f, t('任务名称'), input('text', 'name', { maxLength: 50, placeholder: t('例：订单主表-已完成订单归档') }), { req: 1 });

  const instSel = select('instance_id', W.src.instances, (i) => t('{name}（{info}）', { name: i.name, info: `${i.db_type || 'mysql'}${i.env ? ' · ' + i.env : ''}` }) + (i.status && i.status !== 'ok' ? ' · ' + i.status : ''), (i) => i.id, { first: ['0', t('请选择实例')], disabled: lock, label: t('实例'), num: true, after: onInstance });
  row(f, t('实例'), instSel, { req: 1 });

  const dbs = (W.src.dbs[task.instance_id] || []);
  const dbSel = select('db', dbs, (d) => d.name + (d.protected ? t('（受保护库）') : ''), (d) => d.name, { first: ['', t('请选择库')], disabled: lock, label: t('库'), after: onDb });
  row(f, t('库'), dbSel, { req: 1 });

  const tbSel = select('table', W.src.tables, (x) => t('{name}（≈{rows} 行）', { name: x.name, rows: nf(x.rows) }) + (x.protected ? t(' · 受保护') : ''), (x) => x.name, { first: ['', t('请选择表')], disabled: lock, label: t('表'), after: onTable });
  [...tbSel.options].forEach((o) => { const x = W.src.tables.find((y) => y.name === o.value); if (x && x.protected) o.disabled = true; });
  row(f, t('表'), tbSel, { req: 1 });

  const rgs = rgsOf(task.instance_id);
  const rgSel = select('resource_group_id', rgs, (r) => r.name + (r.has_flow ? '' : t('（未配置“数据归档申请”审批流）')), (r) => r.id, { first: ['0', task.instance_id ? t('请选择资源组') : t('先选择实例')], label: t('资源组'), num: true, after: onRg });
  [...rgSel.options].forEach((o) => { const x = rgs.find((y) => String(y.id) === o.value); if (x && !x.has_flow) o.disabled = true; });
  row(f, t('所属资源组'), rgSel, { req: 1 });
  body.append(f);
  if (lock) body.append(notice(t('实例 / 库 / 表创建后不可修改。'), 'info'));
  const flow = el('div');
  flow.id = 'ar-w-flow';
  flow.append(flowNode());
  const info = el('div');
  info.id = 'ar-w-tinfo';
  info.append(tableInfoNode());
  body.append(flow, info);
}

async function onInstance(v) {
  const t = W.t;
  t.instance_id = Number(v) || 0;
  t.db = '';
  t.table = '';
  t.time_column = '';
  t.time_unit = '';
  W.probe = null;
  W.cols = null;
  W.src.tables = [];
  const rgs = rgsOf(t.instance_id);
  t.resource_group_id = ((rgs.find((r) => r.has_flow) || rgs[0] || {}).id) || 0;
  t.dt.instance_id = t.instance_id;
  if (t.instance_id) {
    const dbs = await loadDbs(t.instance_id);
    t.dt.db = defaultArchiveDb(dbs);
  }
  await loadFlow();
  W.check = null;
  drawStep();
  schedulePreview();
}

async function onDb() {
  W.t.table = '';
  W.t.time_column = '';
  W.t.time_unit = '';
  W.probe = null;
  W.cols = null;
  await loadTables();
  W.check = null;
  drawStep();
  schedulePreview();
}

async function onTable() {
  const t = W.t;
  t.time_column = '';
  t.time_unit = '';
  W.probe = null;
  await loadCols();
  if (W.cols) {
    const pick = defaultTimeColumn(W.cols);
    t.time_column = pick ? pick.name : '';
    if (isIntTime(pick)) runProbe();
  }
  W.check = null;
  drawStep();
  schedulePreview();
}

// ---- 整型时间列：探测它的取值，预填并核对“时间单位” ----
const probeKey = () => [W.t.instance_id, W.t.db, W.t.table, W.t.time_column].join('|');

function probeNode() {
  const box = el('div');
  if (!isIntTime(colOf(W.cols, W.t.time_column))) return box;
  const p = W.probe;
  if (!p || p.key !== probeKey() || p.loading) {
    box.append(el('div', 'small muted', t('正在核对该列的取值…')));
    return box;
  }
  if (p.err) {
    box.append(notice(t('无法核对该列的取值：{msg}', { msg: p.err }), 'warn'));
    return box;
  }
  for (const n of probeNotes(p.data, W.t.time_unit)) box.append(notice(n.text, n.level));
  return box;
}

function fillProbe() {
  const b = $('ar-w-probe');
  if (b) b.replaceChildren(probeNode());
}

async function runProbe() {
  const task = W.t;
  const key = probeKey();
  W.probe = { key, loading: true, data: null, err: '' };
  fillProbe();
  let res;
  try {
    res = { key, loading: false, data: await A.timeProbe(task.instance_id, task.db, task.table, task.time_column), err: '' };
  } catch (e) {
    res = { key, loading: false, data: null, err: e.message };
  }
  if (!W || !W.probe || W.probe.key !== key) return; // 期间换了列 / 表，丢弃过期结果
  W.probe = res;
  // 还没选单位而探测给出了判断：预填（仍可改）
  if (res.data && !W.t.time_unit && res.data.guess_unit) {
    W.t.time_unit = res.data.guess_unit;
    W.check = null;
    const s = $('ar-w-unit');
    if (s) s.value = W.t.time_unit;
    schedulePreview();
  }
  fillProbe();
  updDyn();
}

// 换了时间列：单位重新判断（日期时间列没有单位；整型列要先探测）
function onTimeColumn() {
  W.t.time_unit = '';
  W.probe = null;
  W.check = null;
  if (isIntTime(colOf(W.cols, W.t.time_column))) runProbe();
  drawStep();
  schedulePreview();
}

async function onRg() {
  await loadFlow();
  const f = $('ar-w-flow');
  if (f) f.replaceChildren(flowNode());
  touched();
}

// ---------------------------------------------------------------- 第 2 步：条件
function step1(body) {
  const task = W.t;
  const f = formGrid();
  const tcs = timeCandidates(W.cols).all;
  const tsel = select('time_column', tcs, (c) => t('{name}（{type}{idx}）', { name: c.name, type: c.is_time ? c.type : `${c.type} · ${t('整型')}`, idx: c.indexed ? t(' · 有索引') : t(' · 无索引') }), (c) => c.name, { first: tcs.length ? undefined : ['', t('（请先选择表）')], label: t('时间列'), after: onTimeColumn });
  row(f, t('时间列'), tsel, { req: 1 });
  if (isIntTime(colOf(W.cols, task.time_column))) {
    // 整型列里存的是 10 位秒 / 13 位毫秒的 Unix 时间戳：必须告诉系统单位，条件才会按同一单位换算
    const usel = select('time_unit', TIME_UNITS, (u) => u[1], (u) => u[0], { first: ['', t('请选择时间单位')], label: t('时间单位'), after: () => { W.check = null; fillProbe(); touched(); } });
    usel.id = 'ar-w-unit';
    row(f, t('时间单位'), usel, { req: 1 });
    const pbox = el('div');
    pbox.id = 'ar-w-probe';
    pbox.append(probeNode());
    row(f, t('时间戳核对'), pbox);
    if (!W.probe || W.probe.key !== probeKey()) runProbe();
  }
  const min = policy().min_retention_days || 1;
  const ret = input('number', 'retention_days', { min, max: limits().retention_max_days || 3650, width: 160 });
  row(f, t('保留天数（≥ {min}）', { min }), [ret, ' ' + t('天')], { req: 1 });
  const ex = el('textarea');
  ex.rows = 3;
  ex.maxLength = limits().extra_where_max || 500;
  ex.placeholder = t('例：status IN (3,4)');
  ex.value = task.extra_where || '';
  ex.setAttribute('aria-label', t('附加条件'));
  ex.addEventListener('input', () => { task.extra_where = ex.value; touched(); });
  row(f, t('附加条件（可选）'), ex);
  body.append(f);
  body.append(h4(t('系统生成的 WHERE')));
  const wh = el('div');
  wh.id = 'ar-w-where';
  body.append(wh);
}

// ---------------------------------------------------------------- 第 3 步：模式与备份
function tplExample() {
  const box = el('div');
  const pv = W.pv;
  if (!pv || W.t.mode === 'purge') return box;
  const parts = [];
  if (W.t.dt_on && pv.dest_table_name) parts.push(withNodes(t('库表 {name}'), { name: code(`${W.t.dt.db}.${pv.dest_table_name}`) }));
  if (W.t.df_on && pv.dest_file_path) parts.push(withNodes(t('文件 {name}'), { name: code(pv.dest_file_path) }));
  if (!parts.length) return box;
  const d = el('div', 'small muted');
  d.append(t('示例（按今天展开）：'));
  parts.forEach((p, i) => { if (i) d.append(isZh ? '；' : '; '); add(d, p); });
  box.append(d);
  return box;
}

function step2(body) {
  const task = W.t;
  const cards = el('div', 'ar-modes');
  for (const m of Object.keys(MODES)) {
    const dis = m === 'purge' && !policy().allow_purge_no_backup;
    const lab = el('label', 'ar-mode' + (task.mode === m ? ' on' : '') + (dis ? ' dis' : ''));
    const r = el('input');
    r.type = 'radio';
    r.name = 'ar-mode';
    r.value = m;
    r.checked = task.mode === m;
    r.disabled = dis;
    r.addEventListener('change', () => { task.mode = m; W.check = null; drawStep(); schedulePreview(); });
    const d = el('span', '', MODES[m][2]);
    if (dis) d.append(el('br'), bold(t('安全策略未允许')));
    lab.append(r, el('b', '', MODES[m][0]), d);
    cards.append(lab);
  }
  body.append(h4(t('模式')), cards);

  if (task.mode === 'purge') {
    body.append(notice(bold(t('仅清理：不备份，直接删除，删除后无法恢复。')), 'warn', '!'));
    const f = formGrid();
    const ta = el('textarea');
    ta.rows = 3;
    ta.maxLength = 200;
    ta.placeholder = t('例：压测临时流水，无保留价值');
    ta.value = task.purge_reason;
    ta.setAttribute('aria-label', t('不备份理由'));
    ta.addEventListener('input', () => { task.purge_reason = ta.value; touched(); });
    row(f, t('不备份理由（至少 5 字）'), ta, { req: 1 });
    body.append(f);
  } else {
    body.append(h4(t('备份去向')));
    body.append(dtPanel(), dfPanel());
    const tpl = el('div');
    tpl.id = 'ar-w-tpl';
    body.append(tpl);
  }
  body.append(h4(t('将执行的 SQL')));
  const sq = el('div');
  sq.id = 'ar-w-sql';
  body.append(sq);
}

function dtPanel() {
  const task = W.t;
  const box = el('div', 'ar-bk');
  const lab = el('label', 'chk');
  const cb = input('checkbox', 'dt_on', { after: async () => { W.check = null; if (task.dt_on && !task.dt.instance_id) task.dt.instance_id = task.instance_id; if (task.dt_on) { const dbs = await loadDbs(task.dt.instance_id); if (!task.dt.db) task.dt.db = defaultArchiveDb(dbs); } drawStep(); schedulePreview(); } });
  lab.append(cb, document.createTextNode(' '), bold(t('归档库表')));
  box.append(lab);
  if (task.dt_on) {
    const f = formGrid();
    const insts = W.src.instances;
    const is = select('dt.instance_id', insts, (i) => i.name + (i.id === task.instance_id ? t('（同实例）') : ''), (i) => i.id, { first: ['0', t('请选择实例')], label: t('目标实例'), num: true, after: async (v) => { task.dt.db = ''; const dbs = await loadDbs(Number(v)); task.dt.db = defaultArchiveDb(dbs); W.check = null; drawStep(); schedulePreview(); } });
    row(f, t('目标实例'), is);
    const dbs = W.src.dbs[task.dt.instance_id] || [];
    row(f, t('归档库'), select('dt.db', dbs, (x) => x.name + (x.protected ? t('（受保护库）') : ''), (x) => x.name, { first: ['', t('请选择库')], label: t('归档库'), after: () => { W.check = null; touched(); } }));
    row(f, t('表名模板'), input('text', 'dt.template', { placeholder: '{table}_{yyyy}', after: () => { W.check = null; touched(); } }));
    box.append(f);
  }
  return box;
}

function dfPanel() {
  const task = W.t;
  const box = el('div', 'ar-bk');
  const lab = el('label', 'chk');
  const cb = input('checkbox', 'df_on', { after: () => { W.check = null; if (task.df_on && !task.df.store_id) task.df.store_id = ((S.stores.find((s) => !STORE_BAD(s)) || {}).id) || 0; drawStep(); schedulePreview(); } });
  lab.append(cb, document.createTextNode(' '), bold(t('文件存储')));
  box.append(lab);
  if (task.df_on) {
    const f = formGrid();
    const ss = select('df.store_id', S.stores, (s) => t('{name}（{type}）', { name: s.name, type: s.type }) + (STORE_BAD(s) ? (s.enabled === false || s.status === 'off' ? t(' · 已停用') : t(' · 连接异常')) : ''), (s) => s.id, { first: ['0', t('请选择')], label: t('备份存储'), num: true, after: () => { W.check = null; touched(); } });
    [...ss.options].forEach((o) => { const x = S.stores.find((y) => String(y.id) === o.value); if (x && (x.enabled === false || x.status === 'off')) o.disabled = true; });
    row(f, t('备份存储'), ss);
    row(f, t('格式'), select('df.format', [['csv', 'CSV'], ['sql', t('SQL（INSERT 语句）')]], (x) => x[1], (x) => x[0], { label: t('格式'), after: () => { W.check = null; touched(); } }));
    const zl = el('label', 'chk');
    zl.append(input('checkbox', 'df.gzip', { after: () => { W.check = null; touched(); } }), document.createTextNode(' ' + t('gzip 压缩')));
    const el2 = el('label', 'chk');
    el2.style.marginLeft = '14px';
    el2.append(input('checkbox', 'df.encrypt', { after: () => { W.check = null; touched(); } }), document.createTextNode(' ' + t('AES-256-GCM 加密')));
    row(f, t('压缩 / 加密'), [zl, el2]);
    row(f, t('路径模板'), input('text', 'df.path_template', { placeholder: '{db}/{table}/{yyyy}{mm}{dd}', after: () => { W.check = null; touched(); } }));
    const pm = limits().part_mb || [16, 4096];
    row(f, t('单文件上限（MB）'), input('number', 'df.part_mb', { min: pm[0], max: pm[1], width: 140, after: () => { W.check = null; touched(); } }));
    box.append(f);
  }
  return box;
}

// ---------------------------------------------------------------- 第 4 步：稳定性参数
function paramEstimate() {
  const p = W.t.p;
  const b = Number(p.batch_size);
  const sl = Number(p.sleep_ms);
  if (!(b > 0) || !(sl >= 0)) return '';
  const per = 120 + b * 0.22 + sl;
  return t('约每批 {rw} ms 读写 + 休眠 {sleep} ms，吞吐 ≈ {rate} 行/秒；单次上限 {max} 行约需 {dur}（粗略估算）。', {
    rw: Math.round(120 + b * 0.22), sleep: sl, rate: nf(Math.round(b / (per / 1000))), max: nf(Number(p.max_rows)), dur: fmtDur(Math.ceil(Number(p.max_rows) / b) * per),
  });
}

function step3(body) {
  const task = W.t;
  const lim = limits();
  const f = formGrid();
  const N = (k, opt) => input('number', 'p.' + k, { width: 150, ...opt });
  const keyed = (k, opt) => N(k, { ...opt, after: () => { if (k === 'max_rows') W.check = null; touched(); } });
  const slMin = sleepMin(task.mode);
  const lagR = lim.max_lag_seconds || [1, 3600];
  row(f, t('批大小（行）'), keyed('batch_size'), { req: 1 });
  row(f, t('批间休眠（ms）'), keyed('sleep_ms', { min: slMin }), { req: 1 });
  row(f, t('Threads_running 阈值'), keyed('max_threads_running'), { req: 1 });
  row(f, t('复制延迟阈值（秒）'), keyed('max_lag_seconds', { min: lagR[0] }), { req: 1 });
  row(f, t('磁盘剩余阈值（%）'), keyed('min_disk_free_pct'));
  row(f, t('最长等待（分钟）'), keyed('max_wait_minutes'));
  row(f, t('批耗时目标（ms）'), keyed('target_batch_ms'));
  row(f, t('连续失败暂停（批）'), keyed('max_consecutive_failures'));
  row(f, t('单次运行最多处理（行）'), keyed('max_rows', { width: 200 }), { req: 1 });
  body.append(f);
  const bar = el('div', 'toolbar');
  bar.style.marginTop = '8px';
  const est = el('span', 'small muted');
  est.id = 'ar-w-pest';
  bar.append(button(t('恢复为策略默认值'), '', () => { W.t.p = { ...W.t.p, ...defaultFromPolicy() }; W.check = null; drawStep(); schedulePreview(); }), est);
  body.append(bar);
}

function defaultFromPolicy() {
  const b = blankTask();
  return b.p;
}

// ---------------------------------------------------------------- 第 5 步：调度
function schedPreview() {
  const box = el('div');
  const runs = (W.pv && W.pv.next_runs) || [];
  if (!runs.length) {
    box.append(small(t('暂无触发时间预览')));
    return box;
  }
  const ol = el('ol');
  ol.style.cssText = 'margin:4px 0;padding-left:20px;line-height:2';
  for (const s of runs) {
    const ms = Date.parse(String(s).replace(' ', 'T'));
    const li = el('li');
    li.append(`${s} `);
    if (!Number.isNaN(ms)) {
      li.append(`${WD_FULL[new Date(ms).getDay()]} `);
      li.append(inWin(minOf(ms), W.t.win) ? tag(t('在窗口内'), 'ok') : tag(t('在窗口外，将被跳过'), 'bad'));
    }
    ol.append(li);
  }
  box.append(ol);
  return box;
}

function step4(body) {
  const task = W.t;
  const s = task.sch;
  const f = formGrid();
  const redrawing = () => { drawStep(); schedulePreview(); };
  row(f, t('调度方式'), select('sch.kind', [['day', t('每天')], ['week', t('每周')], ['month', t('每月')], ['cron', t('高级 cron')]], (x) => x[1], (x) => x[0], { label: t('调度方式'), after: redrawing }), { req: 1 });
  if (s.kind === 'cron') {
    row(f, t('cron 表达式（分 时 日 月 周）'), input('text', 'sch.cron', { placeholder: '0 2 * * *', width: 260 }), { req: 1 });
  } else {
    row(f, t('开始时间'), input('time', 'sch.time', { width: 160 }), { req: 1 });
  }
  if (s.kind === 'week') {
    const box = el('div');
    WD_FULL.forEach((n, i) => {
      const lab = el('label', 'chk');
      lab.style.marginRight = '10px';
      const cb = el('input');
      cb.type = 'checkbox';
      cb.checked = (s.weekdays || []).includes(i);
      cb.addEventListener('change', () => {
        s.weekdays = (s.weekdays || []).filter((x) => x !== i);
        if (cb.checked) s.weekdays.push(i);
        touched();
      });
      lab.append(cb, document.createTextNode(' ' + n));
      box.append(lab);
    });
    row(f, t('星期'), box);
  }
  if (s.kind === 'month') row(f, t('每月几日（1–28）'), input('number', 'sch.month_day', { min: 1, max: 28, width: 120 }));
  const win = el('span');
  win.append(input('time', 'win.start', { width: 150 }), ' – ', input('time', 'win.end', { width: 150 }));
  row(f, t('执行窗口'), win, { req: 1 });
  body.append(f);
  body.append(h4(t('未来 5 次触发')));
  const sp = el('div');
  sp.id = 'ar-w-sched';
  body.append(sp);
}

// ---------------------------------------------------------------- 第 6 步：试运行并提交
async function doCheck() {
  const e = allErrs();
  if (e.length) {
    flash(t('第 {n} 步：{msg}', { n: e[0].step + 1, msg: e[0].m }), 'err');
    return;
  }
  if (W.drying) return;
  W.drying = true;
  W.serverErrs = [];
  drawStep();
  try {
    const chk = await A.check({ ...toInput(W.t), task_id: W.edit ? W.edit.id : undefined });
    W.check = { ...chk, keyHash: keyHash(W.t) };
    W.ack = { warn: false, big: false };
  } catch (err) {
    const is = issuesOf(err);
    W.serverErrs = is && is.length ? is.map((x) => tp(x.msg)) : [err.message];
  } finally {
    W.drying = false;
  }
  if (W) drawStep();
}

function step5(body) {
  const task = W.t;
  const lag = task.p.max_lag_seconds + 's';
  const pv = W.pv || {};
  body.append(h4(t('配置确认')));
  body.append(kvList([
    [t('名称'), task.name],
    [t('来源'), [(instOf(task.instance_id) || {}).name || '—', ' · ', code(`${task.db}.${task.table}`), ' · ', (rgsOf(task.instance_id).find((r) => r.id === Number(task.resource_group_id)) || {}).name || '']],
    [t('条件'), code(pv.where || '')],
    [t('模式'), modeTag(task.mode)],
    [t('备份去向'), backupDesc()],
    [t('调度 / 窗口'), t('{schedule} · 窗口 {window}', { schedule: tp(pv.schedule_text || ''), window: tp(pv.window_text || '') })],
    [t('稳定性'), t('批 {batch} 行 · 休眠 {sleep} ms · 单次上限 {max} 行 · Threads ≤ {threads} · 延迟 ≤ {lag}', {
      batch: task.p.batch_size, sleep: task.p.sleep_ms, max: nf(Number(task.p.max_rows)), threads: task.p.max_threads_running, lag,
    })],
  ], true));
  const errs = allErrs();
  if (errs.length) body.append(notice(t('前面的步骤还有未修正的问题，修正前不能试运行 / 提交。'), 'bad', '✕'));
  if (task.mode === 'purge') body.append(notice([bold(t('仅清理：不备份。')), t('理由：{reason}', { reason: task.purge_reason })], 'warn', '!'));
  if (W.flow && W.flow.exists) body.append(flowNode());

  const sec = el('div');
  sec.style.marginTop = '10px';
  if (!needsApproval()) {
    sec.append(notice(withNodes(t('本次修改{a}，{b}，可直接保存。'), { a: bold(t('只更保守')), b: bold(t('无需重新审批')) }), 'ok', '✓'));
  } else {
    if (W.edit && ['rejected', 'disabled', 'draft', 'pending'].includes(W.edit.status)) {
      const status = ({ rejected: t('已驳回'), disabled: t('已停用'), draft: t('草稿'), pending: t('待审批') })[W.edit.status];
      sec.append(notice(t('该任务当前为「{status}」，重新提交将开启新一轮审批{note}。', { status, note: W.edit.status === 'pending' ? t('（原审批作废）') : '' }), 'info'));
    }
    if (W.edit && W.edit.status === 'enabled') sec.append(notice(t('修改了关键字段或放宽了参数，审批通过前任务暂停调度。'), 'warn', '!'));
    const tb = el('div', 'toolbar');
    tb.style.margin = '10px 0';
    tb.append(bold(t('试运行')), el('span', 'spacer'));
    if (W.drying) tb.append(tag(t('试运行中…'), 'brand'));
    tb.append(pbutton('archive.dryrun', W.check ? t('重新试运行') : t('开始试运行'), 'primary', doCheck));
    if (W.drying) tb.lastChild.disabled = true;
    sec.append(tb);
    if (W.drying) {
      const bar = el('div', 'ar-prog busy');
      bar.append(el('b'));
      sec.append(bar);
    } else if (!W.check) {
      sec.append(notice(t('还没有试运行结果。'), 'info', '▶'));
    } else {
      const valid = checkValid();
      if (!valid) sec.append(notice(W.check.keyHash !== keyHash(task) ? t('试运行结果已失效：条件 / 目标 / 来源有改动，请重新试运行。') : t('试运行结果已超过 7 天有效期，请重新试运行。'), 'warn', '!'));
      const wrap = el('div', valid ? '' : 'ar-stale');
      wrap.append(checkNode(W.check, task.mode));
      sec.append(wrap);
      if (valid) {
        const c = W.check;
        if (c.blocked) sec.append(notice(bold(t('有红色检查项未通过，不能提交。')), 'bad', '✕'));
        if (c.need_ack_warn > 0) sec.append(ackBox('warn', withNodes(t('我已阅读以上 {n} 个黄色警告并确认继续'), { n: bold(String(c.need_ack_warn)) })));
        if (c.need_ack_big) sec.append(ackBox('big', [bold(t('已确认大批量')), t('：预估命中 {cap}{rows} 行（占表 {pct}%，超过 30%）', { cap: c.report.capped ? t('至少 ') : '', rows: nf(c.report.est_rows), pct: Number(c.report.percent || 0).toFixed(1) })]));
      }
    }
  }
  body.append(sec);
}

function backupDesc() {
  const t = W.t;
  const pseudo = {
    mode: t.mode,
    dest_table: t.mode !== 'purge' && t.dt_on ? { instance_id: t.dt.instance_id, db: t.dt.db, template: t.dt.template } : null,
    dest_file: t.mode !== 'purge' && t.df_on ? { store_id: t.df.store_id, gzip: t.df.gzip, encrypt: t.df.encrypt } : null,
  };
  const out = backupTags(pseudo);
  return out.length ? out : '—';
}

function ackBox(key, content) {
  const lab = el('label', 'chk');
  lab.style.cssText = 'display:flex;margin:6px 0';
  const cb = el('input');
  cb.type = 'checkbox';
  cb.checked = !!W.ack[key];
  cb.addEventListener('change', () => { W.ack[key] = cb.checked; updDyn(); });
  lab.append(cb, document.createTextNode(' '));
  add(lab, content);
  return lab;
}

async function submit(draft) {
  if (W.busy) return;
  await flushPreview();
  const input_ = toInput(W.t);
  let body;
  if (draft) {
    if (!W.t.name.trim()) { flash(t('请先填写任务名称'), 'err'); return; }
    body = { ...input_, submit: false };
  } else {
    const why = blockReason();
    if (why) { flash(why, 'err'); return; }
    body = { ...input_, submit: true };
    if (needsApproval()) Object.assign(body, { check_token: W.check.check_token, ack_warn: !!W.ack.warn, ack_big: !!W.ack.big });
  }
  W.busy = true;
  W.serverErrs = [];
  try {
    let res;
    if (W.edit) res = await A.updateTask(W.edit.id, { ...body, version: W.edit.version });
    else res = await A.createTask(body);
    const tv = res.data;
    const outcome = res.outcome;
    const msg = outcome === 'submitted' ? t('已提交申请，进入待审批：{text}', { text: tp((tv.approval && tv.approval.pending_text) || '') }) : outcome === 'saved' ? t('已保存修改') : t('已保存');
    const sep = isZh ? '；' : '; ';
    flash(msg + ((res.changes || []).length ? sep + res.changes.map((x) => tp(x)).join(sep) : ''), 'ok');
    notifyChanged();
    const id = tv && tv.id;
    exit(id ? '#task/' + id : '#tasks');
  } catch (err) {
    W.busy = false;
    const is = issuesOf(err);
    W.serverErrs = is && is.length ? is.map((x) => tp(x.msg)) : [err.message];
    if (err.code === 'check_expired' || err.code === 'check_mismatch') W.check = null;
    drawStep();
    return;
  }
}
