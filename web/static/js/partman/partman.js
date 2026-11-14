/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: partman.js
 * @time: 2026/10/28
 */
// 分区维护：配置列表、编辑（含预览与时区输入）、预览并执行（删除分区 / 初始化分区需二次确认）、推送设置、维护日志。
// 所有来自服务端 / 数据库的字符串一律用 textContent 写入，不拼 innerHTML。
import { api, confirmRun, toast } from '../common.js';
import { t, tp } from '../i18n.js';
import { fuzzyFilter } from '../fuzzy.js';
import { sourceTag, logStatusTag, logItemsCell, labelCells, listSep, clauseSep } from './util.js';
import { initWebhooks } from './webhooks.js';
import { logOutputRow, inlineTrace, copyText } from './trace.js';

const $ = (id) => document.getElementById(id);
const PAGE = 20;
const STATUS_CONCURRENCY = 4; // 浏览器端同时在途的状态请求数（服务端另有全局 / 单实例并发闸）

let ov = null; // 概览：实例、枚举、权限、定时维护设置
let state = { offset: 0, total: 0 };
let editing = null; // 正在编辑的配置；null 表示新建
let running = null; // 预览 / 执行弹窗对应的配置与计划

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function setMsg(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

function td(text, cls) { return el('td', cls || '', text === undefined || text === null ? '' : String(text)); }

function fillSelect(sel, items, textOf, valueOf, first) {
  sel.replaceChildren();
  if (first) sel.append(new Option(first[1], first[0]));
  for (const it of items) sel.append(new Option(textOf(it), valueOf(it)));
}

function show(modal) { modal.hidden = false; }
function hide(modal) { modal.hidden = true; }

// ---- 列表 ----

function filterQuery() {
  const p = new URLSearchParams({ offset: String(state.offset), limit: String(PAGE) });
  const add = (k, v) => { if (v) p.set(k, v); };
  add('instance_id', $('pm-inst').value);
  add('isenable', $('pm-enable').value);
  add('col_type', $('pm-coltype').value);
  add('period', $('pm-period').value);
  add('search', $('pm-search').value.trim());
  return p.toString();
}

function button(text, cls, fn) {
  const b = el('button', 'btn ' + cls, text);
  b.type = 'button';
  b.addEventListener('click', fn);
  return b;
}

function renderRows(rows) {
  const body = $('pm-rows');
  body.replaceChildren();
  statusCells.clear();
  availCells.clear();
  alertCells.clear();
  rowOf.clear();
  if (!rows.length) {
    const tr = el('tr');
    const c = el('td', 'empty', t('没有符合条件的分区配置'));
    c.colSpan = 10;
    tr.append(c);
    body.append(tr);
    return;
  }
  for (const r of rows) {
    const tr = el('tr');
    const ops = el('td', 'row-actions');
    if (!r.supported) {
      ops.append(el('span', 'muted', t('暂不支持（{type}）', { type: r.db_type })));
    } else {
      if (ov.can_edit) ops.append(button(t('编辑'), 'ghost', () => openEdit(r)));
      ops.append(button(ov.can_run ? t('预览 / 执行') : t('预览'), 'ghost', () => openRun(r)));
    }
    ops.append(button(t('日志'), 'ghost', () => openLog(r)));
    if (ov.can_delete) ops.append(button(t('删除'), 'ghost danger', () => remove(r)));
    const stat = el('td', 'pm-stat');
    statusCells.set(r.id, stat);
    if (!r.supported) renderStatusCell(stat, { state: 'unsupported', message: t('实例类型 {type} 暂不支持', { type: r.db_type }) });
    else if (statusOf.has(r.id)) renderStatusCell(stat, statusOf.get(r.id));
    else stat.append(el('span', 'muted small', t('检测中…')));
    const av = el('td', 'pm-avail'), al = el('td');
    availCells.set(r.id, av);
    alertCells.set(r.id, al);
    rowOf.set(r.id, r);
    if (!r.supported) { av.append(el('span', 'muted', '—')); al.append(el('span', 'muted', '—')); }
    else if (statusOf.has(r.id)) { renderAvail(av, r, statusOf.get(r.id)); renderAlert(al, r, statusOf.get(r.id)); }
    else { av.append(el('span', 'muted small', t('检测中…'))); renderAlert(al, r, undefined); }
    const inst = td(r.instance_name);
    if (r.dialect === 'doris') inst.append(el('span', 'pm-dialect', r.db_type === 'doris' ? 'Doris' : 'SelectDB'));
    // 列表只放“定位 + 最近维护 + 状态 + 操作”；分区字段 / 类型 / 周期 / 跨度 / 保留 / 预创 / 时区 / 启用 / 说明 在“编辑”与“预览”里看
    tr.append(enableCell(r), td(r.cnf_id), inst, td(r.dbname, 'mono'), td(r.tbname, 'mono'), av, td(r.last_run), stat, al, ops);
    body.append(tr);
  }
}

// ---- 状态与 KPI ----
// 状态由 /configs/:id/status 单独提供（服务端读目标表，慢实例只拖慢它自己那一行）：每行异步加载，浏览器端限并发；
// KPI 的“配置数 / 自动开”来自 /summary（不连目标库），“待续建 / 待清理 / 异常”对可见范围内全部配置逐个检测后累计。

const statusOf = new Map(); // 配置 id -> 最近一次状态
const statusCells = new Map(); // 配置 id -> 当前页该行的状态单元格
const availCells = new Map(); // 配置 id -> 当前页该行的“可用分区”单元格（可用分区告警，见下面的“可用分区告警”一节）
const alertCells = new Map(); // 配置 id -> 当前页该行的“告警”单元格
const rowOf = new Map(); // 配置 id -> 当前页该行的配置（带每张表自己的告警阈值 / 静默期）
const inflight = new Map(); // 配置 id -> 在途请求
let sum = null; // /summary 结果
let sweepToken = 0;
let kpiTimer = 0;
let active = 0;
const waiting = [];

async function limited(fn) {
  if (active >= STATUS_CONCURRENCY) await new Promise((res) => waiting.push(res));
  else active += 1;
  try {
    return await fn();
  } finally {
    const next = waiting.shift();
    if (next) next();
    else active -= 1;
  }
}

function fetchStatus(id, refresh) {
  if (inflight.has(id)) return inflight.get(id);
  const p = api('GET', `/api/v1/partman/configs/${id}/status${refresh ? '?refresh=1' : ''}`)
    .catch((e) => ({ id, state: 'unknown', message: e.message, adds: 0, drops: 0 }))
    .finally(() => inflight.delete(id));
  inflight.set(id, p);
  return p;
}

function statusLabel(v) {
  switch (v.state) {
    case 'ok': return [t('正常'), 'passed'];
    case 'pending': {
      const parts = [];
      if (v.adds > 0) parts.push(t('待续建 {n}', { n: v.adds }));
      if (v.drops > 0) parts.push(t('待清理 {n}', { n: v.drops }));
      return [parts.join(' + ') || t('待维护'), 'waiting'];
    }
    case 'unpartitioned': return [t('未分区'), 'waiting'];
    case 'error': return [t('异常'), 'rejected'];
    case 'unsupported': return [t('暂不支持'), 'aborted'];
    default: return [t('未知'), 'aborted'];
  }
}

function renderStatusCell(cell, v) {
  cell.replaceChildren();
  const [text, cls] = statusLabel(v);
  const tag = el('span', 'st ' + cls, text);
  const msg = tp(v.message);
  const tips = [];
  if (msg) tips.push(msg);
  if (v.reorganize) tips.push(t('含 MAXVALUE 分区，续建需要 REORGANIZE'));
  if (v.checked_at) tips.push(t('检测于 {time}', { time: v.checked_at }));
  if (tips.length) tag.title = tips.join('\n');
  cell.append(tag);
  if (v.reorganize) cell.append(el('span', 'muted small', ' ' + t('需重组')));
  if (v.state === 'unpartitioned') cell.append(el('span', 'muted small', ' ' + (v.can_init ? t('可初始化') : t('不可初始化'))));
  if (msg && (v.state === 'error' || v.state === 'unknown' || (v.state === 'unpartitioned' && !v.can_init))) {
    const m = msg.length > 36 ? msg.slice(0, 36) + '…' : msg;
    cell.append(el('div', 'pm-st-msg', m));
  }
}

function applyStatus(v) {
  statusOf.set(v.id, v);
  const cell = statusCells.get(v.id);
  if (cell && cell.isConnected) renderStatusCell(cell, v);
  refreshAlertCells(v.id);
  scheduleKpi();
}

// 异步加载一批配置的状态：已在排队 / 在途的跳过；refresh 为 false 时已有结果的也跳过。
const queued = new Set();
function loadStatuses(ids, refresh) {
  for (const id of ids) {
    if (queued.has(id) || (!refresh && statusOf.has(id))) continue;
    queued.add(id);
    limited(() => fetchStatus(id, refresh)).then(applyStatus).finally(() => queued.delete(id));
  }
}

function scheduleKpi() {
  if (kpiTimer) return;
  kpiTimer = setTimeout(() => { kpiTimer = 0; drawKpi(); }, 120);
}

function kpi(label, value, cls) {
  const d = el('div');
  d.append(el('b', cls || '', String(value)), el('span', '', label));
  return d;
}

function drawKpi() {
  const box = $('pm-kpis');
  box.replaceChildren();
  if (!sum) return;
  let add = 0, drop = 0, err = 0, unk = 0, unp = 0, low = 0, checked = 0;
  const checkable = sum.items.filter((it) => it.supported);
  for (const it of checkable) {
    const v = statusOf.get(it.id);
    if (!v) continue;
    checked += 1;
    if (alertOf({ alert_threshold: it.alert_threshold, alert_silence_until: it.alert_silenced ? '1' : '' }, v).k === 'alert') low += 1;
    if (v.state === 'error') err += 1;
    else if (v.state === 'unknown') unk += 1;
    else if (v.state === 'unpartitioned') unp += 1;
    else if (v.state === 'pending') {
      if (v.adds > 0) add += 1;
      if (v.drops > 0) drop += 1;
    }
  }
  box.append(kpi(t('配置数'), sum.total), kpi(t('自动开'), sum.enabled, 'ok'),
    kpi(t('待续建'), add, add ? 'warn' : ''), kpi(t('待清理'), drop, drop ? 'warn' : ''),
    kpi(t('未分区'), unp, unp ? 'warn' : ''), kpi(t('异常'), err, err ? 'crit' : ''));
  if (ov.alert && ov.alert.enabled) box.append(kpi(t('可用分区不足'), low, low ? 'crit' : ''));
  const note = $('pm-kpi-note');
  const total = checkable.length;
  let text = t('已检测 {n} / {total} 个配置', { n: checked, total });
  if (unk) text += t('，{n} 个读不到目标库（未计入）', { n: unk });
  if (sum.total > total) text += t('，{n} 个实例类型暂不支持', { n: sum.total - total });
  if (sum.truncated) text += t('；配置过多，只统计前 1000 个');
  note.textContent = text + (checked < total ? t('（检测中…）') : '');
}

async function refreshSummary(refresh) {
  const token = ++sweepToken;
  try {
    const s = await api('GET', '/api/v1/partman/summary');
    if (token !== sweepToken) return;
    sum = s;
    drawKpi();
    loadStatuses(s.items.filter((it) => it.supported).map((it) => it.id), refresh);
  } catch (e) {
    $('pm-kpi-note').textContent = t('统计读取失败：{msg}', { msg: e.message });
  }
}

async function refreshAll() {
  statusOf.clear();
  for (const cell of [...statusCells.values(), ...availCells.values()]) { cell.replaceChildren(el('span', 'muted small', t('检测中…'))); }
  for (const cell of alertCells.values()) cell.replaceChildren();
  await load(true);
  await refreshSummary(true);
}

// 第一列：启停开关（只改“开启维护”这一个开关）。没有编辑权限时只读；实例类型暂不支持的不显示。
function enableCell(r) {
  const c = el('td');
  if (!r.supported) { c.append(el('span', 'muted', '—')); return c; }
  const lab = el('label', 'sw');
  const inp = document.createElement('input');
  inp.type = 'checkbox';
  inp.checked = !!r.isenable;
  inp.disabled = !ov.can_edit;
  inp.setAttribute('aria-label', t('开启维护'));
  inp.addEventListener('change', () => toggle(r, inp));
  lab.append(inp, el('i'));
  c.append(lab);
  return c;
}

async function toggle(r, inp) {
  if (inp) inp.disabled = true; // 请求期间不让连点
  try {
    await api('POST', `/api/v1/partman/configs/${r.id}/enable`, { enable: !r.isenable });
    statusOf.delete(r.id);
    setMsg($('pm-msg'), r.isenable ? t('已停用 {name} 的维护', { name: `${r.dbname}.${r.tbname}` }) : t('已启用 {name} 的维护', { name: `${r.dbname}.${r.tbname}` }), 'info');
    await load();
    await refreshSummary();
  } catch (e) {
    if (inp) { inp.checked = !!r.isenable; inp.disabled = false; } // 失败：开关回到原来的状态
    setMsg($('pm-msg'), e.message);
  }
}

async function load(refresh) {
  try {
    const res = await fetchList();
    state.total = res.total;
    renderRows(res.rows);
    loadStatuses(res.rows.filter((r) => r.supported).map((r) => r.id), refresh);
    $('pm-total').textContent = t('共 {total} 条，第 {page} / {pages} 页', { total: res.total, page: Math.floor(state.offset / PAGE) + 1, pages: Math.max(1, Math.ceil(res.total / PAGE)) });
    $('pm-prev').disabled = state.offset <= 0;
    $('pm-next').disabled = state.offset + PAGE >= res.total;
  } catch (e) {
    setMsg($('pm-msg'), e.message);
  }
}

// 列表接口同时返回 data 与 total，api() 只返回 data，所以这里直接取。
async function fetchList() {
  const res = await fetch('/api/v1/partman/configs?' + filterQuery(), { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
  const payload = await res.json().catch(() => null);
  if (!res.ok) throw new Error((payload && payload.message) || t('请求失败（HTTP {status}）', { status: res.status }));
  return { rows: payload.data || [], total: payload.total || 0 };
}

async function remove(r) {
  const cr = await confirmRun({
    title: t('删除分区维护配置'), level: 'high', code: 'partman.delete', codeName: t('删除配置'),
    lines: [t('将删除该配置及其全部维护日志。'), t('目标表上已有的分区不受影响。')],
    details: [{ k: t('配置'), v: '#' + r.cnf_id }, { k: t('实例'), v: r.instance_name }, { k: t('库.表'), v: `${r.dbname}.${r.tbname}` }],
    typed: `${r.dbname}.${r.tbname}`, typedLabel: t('请输入「库.表」确认删除：'), okText: t('确认删除'),
  }, () => api('DELETE', `/api/v1/partman/configs/${r.id}`));
  if (!cr.ok) return;
  try {
    setMsg($('pm-msg'), t('已删除'), 'info');
    cat.tbs.clear(); // 同上：这张表又可以新增配置了
    statusOf.delete(r.id);
    await load();
    await refreshSummary();
  } catch (e) {
    setMsg($('pm-msg'), e.message);
  }
}

// ---- 可用分区告警 ----
// 口径：可用分区数 = 目标表里上界晚于当前时间的分区个数（含当前分区，不含 MAXVALUE），小于阈值就告警。
// 全局设置（总开关、默认阈值、重复提醒、恢复通知）在“系统配置 › 分区维护”；每张表可以在这里的弹窗里单独设阈值 / 静默。

// 告警级别：与服务端 alertLevelOf 同一套规则（阈值 0 = 不告警；静默期内只标记不推送；读不准的不判断）。
function alertOf(r, v) {
  const a = ov && ov.alert;
  if (!a || !a.enabled) return { k: 'off', thr: 0 };
  if (!v || v.available == null) return { k: 'na', thr: 0 };
  const thr = r.alert_threshold == null ? a.min_available : r.alert_threshold;
  if (thr <= 0) return { k: 'off', thr: 0 };
  if (v.available >= thr) return { k: 'ok', thr };
  return { k: r.alert_silence_until ? 'silent' : 'alert', thr };
}

// 秒数写成“1 天 14 小时”“3 小时 20 分钟”。
function dur(sec) {
  const m = Math.max(0, Math.floor(sec / 60));
  const d = Math.floor(m / 1440);
  const h = Math.floor((m % 1440) / 60);
  if (d >= 1 && d < 10 && h > 0) return t('{d} 天 {h} 小时', { d, h });
  if (d >= 1) return t('{d} 天', { d });
  if (h >= 1 && m % 60 > 0) return t('{h} 小时 {m} 分钟', { h, m: m % 60 });
  if (h >= 1) return t('{h} 小时', { h });
  return t('{m} 分钟', { m });
}

function runwayText(v) {
  if (v.available === 0) return v.has_maxvalue ? t('已用尽，数据正落入 MAXVALUE 分区') : t('已用尽，新数据写入会报错');
  return t('预计 {time} 用尽（约 {left}后）', { time: v.available_end, left: dur(v.available_secs) }) + (v.has_maxvalue ? t('，之后落入 MAXVALUE') : '');
}

function renderAvail(cell, r, v) {
  cell.replaceChildren();
  if (!v || v.available == null) { cell.append(el('span', 'muted', v ? '—' : t('检测中…'))); return; }
  const L = alertOf(r, v);
  const hot = L.k === 'alert' || L.k === 'silent';
  const lvl = hot ? (v.available === 0 ? 'crit' : 'warn') : '';
  const total = Math.min(12, Math.max(v.available, r.pre_create + 1, L.thr));
  const bar = el('span', 'pm-slots' + (lvl ? ' ' + lvl : ''));
  for (let i = 0; i < total; i++) bar.append(el('i', i < v.available ? 'on' : (i < L.thr ? 'need' : '')));
  cell.append(bar, el('b', 'pm-av' + (lvl ? ' ' + lvl : ''), String(v.available)));
  if (L.thr > 0) cell.append(el('span', 'muted small', ' ' + t('/ 阈值 {n}', { n: L.thr })));
  if (r.alert_threshold != null) cell.append(el('span', 'pm-tag', t('自定义')));
  if (L.thr > r.pre_create + 1) {
    const g = el('span', 'pm-tag warn', t('阈值过高'));
    g.title = t('阈值大于“预创建数 + 1”（{n}）：续建之后也达不到，会一直告警', { n: r.pre_create + 1 });
    cell.append(g);
  }
  if (v.has_maxvalue) {
    const m = el('span', 'pm-tag', 'MAXVALUE');
    m.title = t('含 MAXVALUE 兜底分区：用尽后写入不会失败，但续建需要 REORGANIZE');
    cell.append(m);
  }
  cell.append(el('div', 'pm-runway' + (v.available === 0 ? ' crit' : ''), runwayText(v)));
}

function renderAlert(cell, r, v) {
  cell.replaceChildren();
  const L = alertOf(r, v);
  switch (L.k) {
    case 'alert': cell.append(el('span', 'st ' + (v.available === 0 ? 'rejected' : 'waiting'), v.available === 0 ? t('已用尽') : t('告警中'))); break;
    case 'silent': cell.append(el('span', 'st aborted', t('静默至 {time}', { time: r.alert_silence_until }))); break;
    case 'ok': cell.append(el('span', 'st passed', t('正常'))); break;
    case 'off': cell.append(el('span', 'muted', v ? t('已关闭') : '')); break;
    default: cell.append(el('span', 'muted', v ? t('无法判断') : ''));
  }
  if (r.supported && ov.can_edit) { // 每张表自己的阈值 / 静默：放在告警列里，操作列不再变宽
    const w = el('div', 'pm-alert-ops');
    w.append(alertButton(r));
    cell.append(w);
  }
}

function refreshAlertCells(id) {
  const r = rowOf.get(id);
  if (!r) return;
  const v = statusOf.get(id);
  const a = availCells.get(id);
  const b = alertCells.get(id);
  if (a && a.isConnected) renderAvail(a, r, v);
  if (b && b.isConnected) renderAlert(b, r, v);
}

function alertButton(r) {
  const b = button(t('告警设置'), 'ghost', () => openAlert(r));
  if (!ov.alert || !ov.alert.ready) { b.disabled = true; b.title = t('需要先在元数据库执行 deploy/sql/280_partman_alert.sql'); }
  return b;
}

// 页面上方的提示：扩展表没建 / 告警未启用 / 当前全局设置。
function showAlertNotes() {
  const al = ov.alert;
  if (!al) return;
  if (!al.ready) {
    const box = $('pm-alert-warn');
    box.hidden = false;
    box.textContent = t('可用分区告警的扩展表还没有建：列表只做标记，不会推送，也不能给每张表单独设阈值。请先在元数据库执行 deploy/sql/280_partman_alert.sql（升级时 install.sh 会自动执行），然后刷新页面。');
    return;
  }
  $('pm-alert-note').textContent = al.enabled
    ? t('可用分区告警：全局阈值 {n} 个分区，持续不足时{repeat}；检查间隔同自动维护间隔。全局设置在“平台设置 › 系统配置 › 分区维护”，每张表可以单独设阈值。', { n: al.min_available, repeat: al.repeat_hours > 0 ? t('每 {h} 小时提醒一次', { h: al.repeat_hours }) : t('不重复提醒') })
    : t('可用分区告警未启用（“平台设置 › 系统配置 › 分区维护”里的“启用可用分区告警”）。');
}

// ---- 每张表的告警设置 ----

let alertCtx = null; // 弹窗对应的配置

function draftThreshold() {
  const mode = (document.querySelector('input[name="pa-mode"]:checked') || {}).value;
  if (mode === 'follow') return { ok: true, value: null, eff: ov.alert.min_available };
  if (mode === 'off') return { ok: true, value: 0, eff: 0 };
  const n = Number($('pa-n').value);
  const ok = Number.isInteger(n) && n >= 1 && n <= ov.alert.max_threshold;
  return { ok, value: n, eff: ok ? n : 0 };
}

function drawAlertNow() {
  const box = $('pa-now');
  box.replaceChildren();
  const v = statusOf.get(alertCtx.id);
  if (!v) { box.append(el('span', 'muted', t('检测中…'))); return; }
  if (v.available == null) { box.append(el('span', 'muted', t('无法判断：读不到目标表，或这张表不适用（不是 RANGE 分区、配置与表对不上……）'))); return; }
  const line = el('div');
  line.append(el('b', 'pm-big' + (v.available === 0 ? ' crit' : ''), String(v.available)), el('span', 'muted', ' ' + t('个可用')));
  box.append(line, el('div', 'pm-runway' + (v.available === 0 ? ' crit' : ''), runwayText(v)));
}

function drawSilence() {
  const box = $('pa-silence');
  box.replaceChildren();
  const r = alertCtx;
  if (r.alert_silence_until) {
    box.append(el('span', 'st aborted', t('静默至 {time}', { time: r.alert_silence_until })), ' ', button(t('取消静默'), 'ghost', () => silenceAlert(0)));
  } else {
    box.append(button(t('静默 24 小时'), 'ghost', () => silenceAlert(24)), ' ', button(t('静默 7 天'), 'ghost', () => silenceAlert(168)));
  }
  box.append(el('p', 'muted small', t('静默期间只在列表里标记，不推送；到期后自动解除。')));
}

function updateVerdict() {
  const box = $('pa-verdict');
  box.replaceChildren();
  const r = alertCtx;
  const v = statusOf.get(r.id);
  const d = draftThreshold();
  let text = '';
  let cls = 'mid';
  if (!ov.alert.enabled) text = t('系统配置里没有启用可用分区告警，不会推送，列表里也不标告警。');
  else if (!d.ok) { text = t('自定义阈值需要是 1 到 {max} 之间的整数', { max: ov.alert.max_threshold }); cls = 'warn'; }
  else if (d.eff === 0) text = t('这张表不告警（阈值 0）。');
  else if (!v || v.available == null) text = t('读不到可用分区数，暂时无法判断。');
  else if (v.available < d.eff) {
    text = t('当前可用 {n} 个 < 阈值 {thr} → 告警', { n: v.available, thr: d.eff }) + (r.alert_silence_until ? t('（静默中，不推送）') : '');
    cls = v.available === 0 ? 'crit' : 'warn';
  } else { text = t('当前可用 {n} 个 ≥ 阈值 {thr} → 正常', { n: v.available, thr: d.eff }); cls = 'ok'; }
  box.append(el('div', 'pm-verdict ' + cls, text));
  if (d.ok && d.eff > r.pre_create + 1) box.append(el('p', 'pm-warn-t small', t('阈值 {thr} 大于“预创建数 + 1”（{n}）：定时维护续建之后也达不到，会一直告警。请调小阈值，或把预创建数加大。', { thr: d.eff, n: r.pre_create + 1 })));
}

function openAlert(r) {
  alertCtx = r;
  setMsg($('pa-msg'), '');
  $('pa-title').textContent = t('告警设置：{name}', { name: `${r.dbname}.${r.tbname}` });
  $('pa-follow').textContent = t('跟随全局默认（当前 {n} 个分区）', { n: ov.alert.min_available });
  const mode = r.alert_threshold == null ? 'follow' : r.alert_threshold === 0 ? 'off' : 'custom';
  for (const x of document.querySelectorAll('input[name="pa-mode"]')) x.checked = x.value === mode;
  $('pa-n').max = String(ov.alert.max_threshold);
  $('pa-n').value = r.alert_threshold > 0 ? r.alert_threshold : ov.alert.min_available;
  drawAlertNow();
  drawSilence();
  updateVerdict();
  show($('pm-alert'));
}

// 服务端返回的最新阈值 / 静默期并回这一行，刷新列表单元格与 KPI。
function applyAlertView(r, res) {
  r.alert_threshold = res.alert_threshold == null ? null : res.alert_threshold;
  r.alert_silence_until = res.alert_silence_until || '';
  refreshAlertCells(r.id);
  refreshSummary();
}

async function saveAlert() {
  const d = draftThreshold();
  if (!d.ok) { setMsg($('pa-msg'), t('自定义阈值需要是 1 到 {max} 之间的整数', { max: ov.alert.max_threshold })); return; }
  try {
    applyAlertView(alertCtx, await api('PUT', `/api/v1/partman/configs/${alertCtx.id}/alert`, { threshold: d.value }));
    hide($('pm-alert'));
    setMsg($('pm-msg'), t('已保存 {name} 的告警阈值', { name: `${alertCtx.dbname}.${alertCtx.tbname}` }), 'info');
  } catch (e) {
    setMsg($('pa-msg'), e.message);
  }
}

async function silenceAlert(hours) {
  try {
    applyAlertView(alertCtx, await api('POST', `/api/v1/partman/configs/${alertCtx.id}/alert/silence`, { hours }));
    drawSilence();
    updateVerdict();
  } catch (e) {
    setMsg($('pa-msg'), e.message);
  }
}

// ---- 告警记录 ----

const alertLogs = { offset: 0, total: 0 };
const ALERT_PAGE = 20;

async function loadAlertLogs() {
  const body = $('pas-rows');
  setMsg($('pas-msg'), '');
  try {
    const res = await fetch(`/api/v1/partman/alerts?offset=${alertLogs.offset}&limit=${ALERT_PAGE}`, { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
    const payload = await res.json().catch(() => null);
    if (!res.ok) throw new Error((payload && payload.message) || t('请求失败（HTTP {status}）', { status: res.status }));
    alertLogs.total = payload.total || 0;
    body.replaceChildren();
    const rows = payload.data || [];
    if (!rows.length) {
      const tr = el('tr');
      const c = el('td', 'empty', t('还没有告警记录'));
      c.colSpan = 7;
      tr.append(c);
      body.append(tr);
    }
    const kinds = { alert: [t('告警'), 'rejected'], repeat: [t('重复提醒'), 'waiting'], recover: [t('已恢复'), 'passed'] };
    for (const x of rows) {
      const k = kinds[x.kind] || [x.kind, 'aborted'];
      const kind = el('td');
      kind.append(el('span', 'st ' + k[1], k[0]));
      const result = el('td');
      result.append(el('span', 'st ' + (x.ok ? 'passed' : 'rejected'), x.ok ? t('已推送') : t('失败')));
      if (x.message) result.append(el('div', 'pm-st-msg', x.message));
      const tr = el('tr');
      tr.append(td(x.create_time), kind, td(x.instance), td(x.dbname ? `${x.dbname}.${x.tbname}` : `#${x.cnf_id}`, 'mono'), td(`${x.available} / ${x.threshold}`), td(x.channels), result);
      body.append(tr);
    }
    $('pas-total').textContent = t('共 {total} 条，第 {page} / {pages} 页', { total: alertLogs.total, page: Math.floor(alertLogs.offset / ALERT_PAGE) + 1, pages: Math.max(1, Math.ceil(alertLogs.total / ALERT_PAGE)) });
    $('pas-prev').disabled = alertLogs.offset <= 0;
    $('pas-next').disabled = alertLogs.offset + ALERT_PAGE >= alertLogs.total;
  } catch (e) {
    setMsg($('pas-msg'), e.message);
  }
}

function openAlertLogs() {
  alertLogs.offset = 0;
  $('pas-rows').replaceChildren();
  show($('pm-alerts-log'));
  loadAlertLogs();
}

function bindAlert() {
  $('pm-alerts').addEventListener('click', openAlertLogs);
  $('pas-close').addEventListener('click', () => hide($('pm-alerts-log')));
  $('pas-prev').addEventListener('click', () => { alertLogs.offset = Math.max(0, alertLogs.offset - ALERT_PAGE); loadAlertLogs(); });
  $('pas-next').addEventListener('click', () => { alertLogs.offset += ALERT_PAGE; loadAlertLogs(); });
  $('pa-close').addEventListener('click', () => hide($('pm-alert')));
  $('pa-save').addEventListener('click', saveAlert);
  for (const x of document.querySelectorAll('input[name="pa-mode"]')) x.addEventListener('change', updateVerdict);
  $('pa-n').addEventListener('input', () => {
    document.querySelector('input[name="pa-mode"][value="custom"]').checked = true;
    updateVerdict();
  });
}

// ---- 计划展示（编辑弹窗与执行弹窗共用） ----

function newTable(headers) {
  const tbl = el('table', 'list');
  const th = el('thead');
  const hr = el('tr');
  for (const h of headers) hr.append(el('th', '', h));
  th.append(hr);
  const tb = el('tbody');
  tbl.append(th, tb);
  return { tbl, tb };
}


// 长语句（初始化一次建几百上千个分区）缩略显示，可展开 / 复制完整语句。
const STMT_SHORT = 600;
function stmtCell(sql) {
  const c = el('td', 'pm-stmt');
  if (sql.length <= STMT_SHORT) { c.textContent = sql; return c; }
  const text = el('div', 'pm-stmt-text', shortSql(sql));
  let full = false;
  const more = button(t('展开完整语句（{n} 字符）', { n: sql.length }), 'ghost', () => {
    full = !full;
    text.textContent = full ? sql : shortSql(sql);
    more.textContent = full ? t('收起') : t('展开完整语句（{n} 字符）', { n: sql.length });
  });
  const copy = button(t('复制完整语句'), 'ghost', async () => {
    toast((await copyText(sql)) ? t('已复制完整语句') : t('复制失败，请展开后手工选中复制'), 'ok');
  });
  const ops = el('div', 'pm-stmt-ops');
  ops.append(more, copy);
  c.append(text, ops);
  return c;
}
function shortSql(sql) { return sql.slice(0, 420) + t(' …（省略 {n} 字符）… ', { n: sql.length - 560 }) + sql.slice(-140); }

const START_LABEL = { current: t('从当前窗口开始'), earliest: t('从最早数据所在窗口开始'), date: t('指定起始日期') };
const CHECK_LABEL = { ok: [t('通过|check'), 'passed'], warn: [t('注意'), 'waiting'], error: [t('不满足'), 'rejected'] };
const KIND_LABEL = { add: t('新增'), drop: t('删除'), reorganize: t('重组'), init: t('初始化') };

// 初始化计划的摘要：起点、将创建的分区个数与范围、前置检查结果、说明。
function renderInitSummary(box, plan) {
  const ini = plan.init;
  const dl = el('dl', 'pm-kv');
  const add = (k, v) => { if (v !== undefined && v !== null && v !== '') dl.append(el('dt', '', k), el('dd', 'mono', String(v))); };
  const mode = START_LABEL[ini.start_mode] || ini.start_mode;
  add(t('起点'), ini.start_mode === 'date' && ini.start_date ? t('{mode}（{date}）', { mode, date: ini.start_date }) : mode);
  add(t('起始窗口'), ini.start);
  add(t('最早数据时间'), ini.earliest_data);
  if (ini.partition_count > 0) {
    add(t('将创建分区'), t('{n} 个：{first} ~ {last}', { n: ini.partition_count, first: ini.first_partition, last: ini.last_partition }));
    add(t('最后一个分区上界（不含）'), ini.last_bound);
  }
  box.append(dl);
  const checks = ini.checks || [];
  if (checks.length) {
    box.append(el('div', 'muted small', t('前置检查：')));
    const ul = el('ul', 'pm-checks');
    for (const c of checks) {
      const [text, cls] = CHECK_LABEL[c.level] || [c.level, 'aborted'];
      const li = el('li');
      li.append(el('span', 'st ' + cls, text), el('span', '', ' ' + tp(c.message)));
      ul.append(li);
    }
    box.append(ul);
  }
  for (const n of ini.notes || []) box.append(el('div', 'alert info', tp(n)));
}

function renderPlan(box, plan) {
  box.replaceChildren();
  const init = plan.kind === 'init' && plan.init;
  const head = [t('目标表 {name}', { name: plan.table })];
  if (init) head.push(t('未分区的表（将初始化为 RANGE 分区表）'));
  else head.push(t('分区方式 {method}（{expr}）', { method: plan.method, expr: plan.expression }));
  head.push(t('按{tz}的当前时间 {now}', { tz: plan.timezone ? t('时区 {name}', { name: plan.timezone }) : t('配置时区'), now: plan.now }));
  box.append(el('p', 'muted small', head.join(clauseSep)));
  if (init) {
    renderInitSummary(box, plan);
  } else {
    const parts = el('div', 'pm-parts');
    const ex = plan.existing || [];
    const shown = ex.length > 12 ? [...ex.slice(0, 3), null, ...ex.slice(-8)] : ex;
    for (const p of shown) {
      if (!p) parts.append(el('span', '', '…'));
      else if (p.unknown) parts.append(el('span', 'pm-part-unknown', t('{name} < 上界未知（不会删除）', { name: p.name })));
      else parts.append(el('span', '', `${p.name} < ${p.bound}`));
    }
    box.append(el('div', 'muted small', t('现有 {n} 个分区：', { n: ex.length })), parts);
  }
  for (const w of plan.warnings || []) box.append(el('div', 'alert warn', tp(w)));
  const checkTrace = () => {
    if (!(plan.output && plan.output.lines && plan.output.lines.length)) return;
    const tb = el('div', 'pm-trace-box');
    tb.append(inlineTrace({ label: t('检查过程'), output: plan.output, open: false }));
    box.append(tb);
  };
  if (!plan.statements.length) {
    if (init) box.append(el('div', 'alert err', t('前置检查未通过，不能初始化。')));
    else box.append(el('div', 'alert info', t('分区已经是最新的，没有需要执行的语句。')));
    checkTrace();
    return;
  }
  const { tbl, tb } = newTable([t('类型'), t('分区'), t('边界（不含）'), t('语句')]);
  for (const s of plan.statements) {
    const tr = el('tr');
    const k = el('td');
    k.append(el('span', 'pm-kind ' + s.kind, KIND_LABEL[s.kind] || s.kind));
    const parts = s.partitions || [];
    const names = parts.length > 6 ? t('{head} … {tail}（共 {n} 个）', { head: parts.slice(0, 3).join(listSep), tail: parts.slice(-2).join(listSep), n: parts.length }) : parts.join(listSep);
    tr.append(k, td(names, 'mono'), td(s.bound, 'mono'), stmtCell(s.sql));
    tb.append(tr);
  }
  box.append(tbl);
  checkTrace();
}

// ---- 时区输入：常用项 + 自由输入 + 说明（提示只是辅助，是否合法以服务端校验为准） ----

const TZ_COMMON = [
  ['', t('跟随目标库')],
  ['Asia/Shanghai', 'Asia/Shanghai'],
  ['UTC', 'UTC'],
  ['+08:00', '+08:00'],
  ['Asia/Tokyo', 'Asia/Tokyo'],
  ['Asia/Singapore', 'Asia/Singapore'],
  ['Europe/London', 'Europe/London'],
  ['America/New_York', 'America/New_York'],
  ['America/Los_Angeles', 'America/Los_Angeles'],
];
const TZ_MAX = 20; // dbt_partman_config.timezone 是 varchar(20)
const TZ_FIXED = /^(?:(?:UTC|GMT)\s*)?[+-]\d{1,2}(?::?\d{2})?$|^(?:UTC|GMT)$/i;

function fillTzList() {
  const names = new Set(TZ_COMMON.map((x) => x[0]).filter(Boolean));
  for (const x of (ov && ov.timezones) || []) if (x) names.add(x);
  for (const x of ['GMT+08:00', 'UTC+8', '+0800', 'Asia/Hong_Kong', 'Asia/Seoul', 'Asia/Dubai', 'Europe/Berlin', 'Australia/Sydney']) names.add(x);
  fillList('pe-tzs', [...names], (x) => x);
  const box = $('pe-tz-chips');
  box.replaceChildren();
  for (const [value, label] of TZ_COMMON) {
    const b = el('button', 'pm-chip', label);
    b.type = 'button';
    b.dataset.tz = value;
    b.addEventListener('click', () => { $('pe-tz').value = value; tzHint(); });
    box.append(b);
  }
}

// 浏览器能识别该 IANA 名字时，比较一月与七月的偏移判断有没有夏令时；不认识返回 null。
function browserHasDst(name) {
  try {
    const y = new Date().getFullYear();
    const off = (m) => {
      const parts = new Intl.DateTimeFormat('en-US', { timeZone: name, hourCycle: 'h23', year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric' })
        .formatToParts(new Date(Date.UTC(y, m, 1, 12)));
      const g = (t) => Number(parts.find((p) => p.type === t).value);
      return Date.UTC(g('year'), g('month') - 1, g('day'), g('hour'), g('minute'), g('second')) - Date.UTC(y, m, 1, 12);
    };
    return off(0) !== off(6);
  } catch (_) {
    return null;
  }
}

function tzHint() {
  const v = $('pe-tz').value.trim();
  const note = $('pe-tz-note');
  for (const b of $('pe-tz-chips').children) b.setAttribute('aria-pressed', String(b.dataset.tz === v));
  note.className = 'muted small';
  note.textContent = '';
  if (!v) return;
  if ([...v].length > TZ_MAX) {
    note.className = 'pm-tz-warn small';
    note.textContent = t('时区最多 {max} 个字符，当前 {n} 个，无法保存。', { max: TZ_MAX, n: [...v].length });
    return;
  }
  if (TZ_FIXED.test(v)) return;
  if (/^[A-Za-z_]+(?:\/[A-Za-z0-9_+-]+)+$/.test(v) && browserHasDst(v) === true) {
    note.className = 'pm-tz-warn small';
    note.textContent = t('{name} 有夏令时，换季日附近分区边界会漂移一小时，建议改用固定偏移。', { name: v });
  }
}

// ---- 编辑 ----

function draft() {
  return {
    instance_id: Number($('pe-inst').value), dbname: $('pe-db').value.trim(), tbname: $('pe-tb').value.trim(), column: $('pe-col').value.trim(),
    col_type: $('pe-coltype').value, period: $('pe-period').value, interval: Number($('pe-interval').value), retain: Number($('pe-retain').value),
    pre_create: Number($('pe-pre').value), timezone: $('pe-tz').value.trim(), isenable: $('pe-enable').checked, env: $('pe-env').value.trim(),
  };
}

function openEdit(r) {
  editing = r || null;
  $('pm-edit-title').textContent = r ? t('编辑分区配置：{name}', { name: `${r.dbname}.${r.tbname}` }) : t('新增分区配置');
  setMsg($('pm-edit-msg'), '');
  $('pe-plan').replaceChildren();
  const supported = ov.instances.filter((i) => i.supported);
  fillSelect($('pe-inst'), supported, (i) => t('{name}（{type}）', { name: i.name, type: i.db_type }), (i) => String(i.id));
  fillSelect($('pe-coltype'), ov.col_types, (x) => x, (x) => x);
  fillSelect($('pe-period'), ov.periods, (x) => x, (x) => x);
  fillTzList();
  const v = r || { instance_id: supported[0] ? supported[0].id : 0, dbname: '', tbname: '', column: '', col_type: 'datetime', period: 'day', interval: 1, retain: 30, pre_create: 3, timezone: 'Asia/Shanghai', isenable: true, env: '' };
  $('pe-inst').value = String(v.instance_id);
  $('pe-db').value = v.dbname;
  $('pe-tb').value = v.tbname;
  $('pe-col').value = v.column;
  $('pe-coltype').value = v.col_type;
  $('pe-period').value = v.period;
  $('pe-interval').value = v.interval || 1;
  $('pe-retain').value = v.retain;
  $('pe-pre').value = v.pre_create;
  $('pe-tz').value = v.timezone || '';
  $('pe-enable').checked = v.isenable;
  $('pe-env').value = v.env;
  show($('pm-edit'));
  catErr = '';
  tzHint();
  updateHint();
  loadDbs().then(loadTables).then(loadCols);
}

// ---- 库 / 表 / 分区字段 级联（候选来自目标实例的 information_schema；读取失败仍可手工输入） ----

const cat = { dbs: new Map(), tbs: new Map(), cols: new Map() }; // 已读取过的结果，键见各 load 函数
let catErr = ''; // 最近一次读取候选失败的原因
const seq = { db: 0, tb: 0, col: 0 }; // 丢弃过期响应（用户已经改选了上一级）

async function catalog(kind, key, url) {
  if (cat[kind].has(key)) return cat[kind].get(key);
  const v = await api('GET', url);
  cat[kind].set(key, v);
  return v;
}

function fillList(id, items, valueOf, labelOf) {
  const dl = $(id);
  dl.replaceChildren();
  for (const it of items) {
    const o = document.createElement('option');
    o.value = valueOf(it);
    const lab = labelOf ? labelOf(it) : '';
    if (lab) { o.label = lab; o.textContent = lab; }
    dl.append(o);
  }
}

const dbKey = () => `${$('pe-inst').value}/${$('pe-db').value.trim()}`;
const tbKey = () => `${dbKey()}/${$('pe-tb').value.trim()}`;
const IDENT = /^[0-9A-Za-z_$]{1,64}$/;

async function loadDbs() {
  const inst = $('pe-inst').value;
  const mine = ++seq.db;
  fillList('pe-dbs', []);
  if (!inst) return;
  try {
    const dbs = await catalog('dbs', inst, `/api/v1/partman/instances/${inst}/databases`);
    if (mine !== seq.db) return;
    catErr = '';
    fillList('pe-dbs', dbs, (x) => x);
  } catch (e) {
    if (mine !== seq.db) return;
    catErr = t('读取库列表失败：{msg}', { msg: e.message });
  }
  updateHint();
}

async function loadTables() {
  const inst = $('pe-inst').value;
  const db = $('pe-db').value.trim();
  const mine = ++seq.tb;
  tbClose();
  if (!inst || !IDENT.test(db)) { updateHint(); return; }
  try {
    await catalog('tbs', `${inst}/${db}`, `/api/v1/partman/instances/${inst}/tables?db=${encodeURIComponent(db)}`);
    if (mine !== seq.tb) return;
    catErr = '';
    tbRefresh();
  } catch (e) {
    if (mine !== seq.tb) return;
    catErr = t('读取库 {db} 的表列表失败：{msg}', { db, msg: e.message });
  }
  updateHint();
}

// ---- 表名下拉：只列分区表，并按状态着色 ----
// 淡红 = 这张表已经有分区配置（同一实例的同库同表只能有一条）；浅绿 = 是 RANGE 分区表且还没有配置，可以新增；其余不上色。
// 原生 <datalist> 的弹窗由浏览器自己画，选项没法上色，所以表名这一项用自己的列表（挂在页面里、fixed 定位，弹窗的 overflow 裁剪不到它）。
// 只改下拉里显示什么：cat.tbs 仍是库里的全部表（updateHint 要靠它判断“还不是分区表”），手工输入照旧可用。
// Doris / SelectDB 的表没有读分区信息（partition_unknown），无法判断是不是分区表，所以不过滤。

const TB_MAX = 200; // 最多渲染的条数，继续输入缩小范围
const tbBox = { shown: [], active: -1, open: false, typed: false };

function tbCandidates() {
  return (cat.tbs.get(dbKey()) || []).filter((x) => x.partitioned || x.partition_unknown);
}

function tbState(x) {
  if (x.configured) return 'taken';
  return x.partitioned && /^RANGE/.test(x.method || '') ? 'free' : '';
}

function tbSub(x) {
  const parts = [];
  if (x.partitioned) parts.push(t('分区表 {method}', { method: x.method || '' }).trim());
  if (x.configured) parts.push(t('已有分区配置'));
  return parts.join(' · ');
}

function tbRender() {
  const list = $('pe-tb-list');
  const all = tbCandidates();
  const q = tbBox.typed ? $('pe-tb').value : '';
  tbBox.shown = fuzzyFilter(all.map((x) => x.name), q).map((i) => all[i]);
  list.replaceChildren();
  if (!tbBox.shown.length) {
    list.append(el('li', 'pm-tb-empty', q.trim() ? t('没有匹配的选项') : t('该库里没有分区表（非分区表不显示在列表里）')));
    tbBox.active = -1;
    $('pe-tb').removeAttribute('aria-activedescendant');
    return;
  }
  const cur = $('pe-tb').value.trim();
  tbBox.shown.slice(0, TB_MAX).forEach((x, n) => {
    const st = tbState(x);
    const li = el('li', 'pm-tb-opt' + (st ? ' ' + st : ''));
    li.id = `pe-tb-o${n}`;
    li.setAttribute('role', 'option');
    li.dataset.i = String(n);
    li.append(el('span', 'pm-tb-name', x.name));
    const sub = tbSub(x);
    if (sub) li.append(el('small', 'pm-tb-sub', sub));
    list.append(li);
  });
  if (tbBox.shown.length > TB_MAX) list.append(el('li', 'pm-tb-empty', t('还有 {n} 项，继续输入缩小范围', { n: tbBox.shown.length - TB_MAX })));
  const at = tbBox.shown.findIndex((x) => x.name === cur);
  tbActive(at >= 0 && at < TB_MAX ? at : 0, false);
}

function tbActive(n, scroll = true) {
  const items = $('pe-tb-list').querySelectorAll('.pm-tb-opt');
  items.forEach((li, i) => li.classList.toggle('on', i === n));
  tbBox.active = n;
  if (items[n]) {
    $('pe-tb').setAttribute('aria-activedescendant', items[n].id);
    if (scroll) items[n].scrollIntoView({ block: 'nearest' });
  } else {
    $('pe-tb').removeAttribute('aria-activedescendant');
  }
}

// 列表放在输入框下面；下面放不下（比上面小）就翻到上面
function tbPlace() {
  const r = $('pe-tb').getBoundingClientRect();
  const vh = window.innerHeight;
  const below = vh - r.bottom - 8;
  const above = r.top - 8;
  const st = $('pe-tb-list').style;
  st.left = Math.max(4, Math.round(r.left)) + 'px';
  st.minWidth = Math.round(r.width) + 'px';
  if (below >= 180 || below >= above) {
    st.top = Math.round(r.bottom + 2) + 'px';
    st.bottom = 'auto';
    st.maxHeight = Math.max(120, Math.min(300, below)) + 'px';
  } else {
    st.top = 'auto';
    st.bottom = Math.round(vh - r.top + 2) + 'px';
    st.maxHeight = Math.max(120, Math.min(300, above)) + 'px';
  }
}

function tbOpen() {
  const input = $('pe-tb');
  if (tbBox.open || input.disabled || !cat.tbs.has(dbKey())) return; // 还没读到表列表就先不开，读到后 tbRefresh 会补开
  tbBox.open = true;
  tbBox.typed = false;
  $('pe-tb-list').hidden = false;
  input.setAttribute('aria-expanded', 'true');
  tbRender();
  tbPlace();
  window.addEventListener('scroll', tbPlace, true);
  window.addEventListener('resize', tbPlace);
}

function tbClose() {
  if (!tbBox.open) return;
  tbBox.open = false;
  $('pe-tb-list').hidden = true;
  $('pe-tb').setAttribute('aria-expanded', 'false');
  window.removeEventListener('scroll', tbPlace, true);
  window.removeEventListener('resize', tbPlace);
}

// 表列表读完（或重新读完）后：输入框正聚焦就开 / 刷新列表
function tbRefresh() {
  if (document.activeElement !== $('pe-tb')) return;
  if (tbBox.open) { tbRender(); tbPlace(); } else tbOpen();
}

function tbChoose(x) {
  const input = $('pe-tb');
  const changed = input.value !== x.name;
  tbClose();
  input.value = x.name;
  if (changed) input.dispatchEvent(new Event('change', { bubbles: true })); // 与原生 datalist 选中一致：走原有的 change（清分区字段、重读字段列表）
}

function bindTbList() {
  const input = $('pe-tb');
  input.addEventListener('focus', tbOpen);
  input.addEventListener('click', tbOpen);
  input.addEventListener('input', () => {
    if (!tbBox.open) tbOpen();
    if (!tbBox.open) return;
    tbBox.typed = true;
    tbRender();
    tbPlace();
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (!tbBox.open) { tbOpen(); return; }
      const n = $('pe-tb-list').querySelectorAll('.pm-tb-opt').length;
      if (n) tbActive(e.key === 'ArrowDown' ? (tbBox.active + 1) % n : (tbBox.active - 1 + n) % n);
    } else if (e.key === 'Enter') {
      const x = tbBox.open && tbBox.shown[tbBox.active];
      if (x) { e.preventDefault(); tbChoose(x); }
    } else if (e.key === 'Escape') {
      if (tbBox.open) { e.preventDefault(); e.stopPropagation(); tbClose(); } // 只收起列表，不关整个弹窗
    } else if (e.key === 'Tab') {
      tbClose();
    }
  });
  // mousedown 先于 blur：在这里选中，并阻止输入框失焦（拖滚动条时列表也不会收起）
  $('pe-tb-list').addEventListener('mousedown', (e) => {
    e.preventDefault();
    const li = e.target.closest('.pm-tb-opt');
    if (li && tbBox.shown[Number(li.dataset.i)]) tbChoose(tbBox.shown[Number(li.dataset.i)]);
  });
  input.addEventListener('blur', tbClose);
}

async function loadCols() {
  const inst = $('pe-inst').value;
  const db = $('pe-db').value.trim();
  const tb = $('pe-tb').value.trim();
  const mine = ++seq.col;
  fillList('pe-cols', []);
  if (!inst || !IDENT.test(db) || !IDENT.test(tb)) { updateHint(); return; }
  try {
    const cols = await catalog('cols', `${inst}/${db}/${tb}`, `/api/v1/partman/instances/${inst}/columns?db=${encodeURIComponent(db)}&table=${encodeURIComponent(tb)}`);
    if (mine !== seq.col) return;
    catErr = '';
    fillList('pe-cols', cols, (c) => c.name, (c) => c.column_type + (c.comment ? ' · ' + c.comment : ''));
    if (!$('pe-col').value.trim() && cols.length) {
      const pref = cols.find((c) => /created|paid|time|date/i.test(c.name)) || cols[0];
      $('pe-col').value = pref.name;
      autoType();
    }
  } catch (e) {
    if (mine !== seq.col) return;
    catErr = t('读取表 {db}.{tb} 的字段列表失败：{msg}', { db, tb, msg: e.message });
  }
  updateHint();
}

// 选中分区字段后自动带出字段类型（int / bigint / datetime / date）。
function autoType() {
  const cols = cat.cols.get(tbKey());
  const c = cols && cols.find((x) => x.name === $('pe-col').value.trim());
  if (c) $('pe-coltype').value = c.col_type;
}

function instInfo() {
  const id = Number($('pe-inst').value);
  return ((ov && ov.instances) || []).find((i) => i.id === id) || null;
}

function updateHint() {
  const box = $('pe-hint');
  if (catErr) { setMsg(box, catErr, 'warn'); return; }
  const tbs = cat.tbs.get(dbKey());
  const name = $('pe-tb').value.trim();
  if (!tbs || !name) { setMsg(box, ''); return; }
  const found = tbs.find((x) => x.name === name);
  if (!found) { setMsg(box, t('库 {db} 里没有表 {name}（可能是视图或拼写有误）。', { db: $('pe-db').value.trim(), name }), 'warn'); return; }
  if (!found.partitioned) {
    const inst = instInfo();
    if (inst && (inst.dialect === 'doris' || inst.can_init === false)) {
      setMsg(box, t('{name} 还不是分区表，需先在目标库建为 RANGE 分区表。', { name }), 'warn');
    } else {
      setMsg(box, t('{name} 还不是分区表。', { name }), 'warn');
    }
    return;
  }
  if (!/^RANGE/.test(found.method || '')) { setMsg(box, t('{name} 的分区方式是 {method}，仅支持 RANGE / RANGE COLUMNS。', { name, method: found.method || t('未知') }), 'warn'); return; }
  const cols = cat.cols.get(tbKey());
  if (cols && !cols.length) { setMsg(box, t('{name} 里没有 datetime / date / int / bigint 字段，无法作为分区字段。', { name }), 'warn'); return; }
  setMsg(box, t('{name} 是分区表（{method}）。', { name, method: found.method }), 'info');
}

function bindCascade() {
  $('pe-inst').addEventListener('change', () => {
    for (const id of ['pe-db', 'pe-tb', 'pe-col']) $(id).value = '';
    tbClose();
    fillList('pe-cols', []);
    loadDbs();
  });
  $('pe-db').addEventListener('change', () => {
    $('pe-tb').value = '';
    $('pe-col').value = '';
    fillList('pe-cols', []);
    loadTables();
  });
  $('pe-tb').addEventListener('change', () => {
    $('pe-col').value = '';
    updateHint();
    loadCols();
  });
  $('pe-col').addEventListener('change', autoType);
  $('pe-tz').addEventListener('input', tzHint);
  bindTbList();
}

async function previewDraft() {
  setMsg($('pm-edit-msg'), '');
  const btn = $('pe-preview');
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  try {
    const plan = await api('POST', '/api/v1/partman/preview', { draft: draft() });
    renderPlan($('pe-plan'), plan);
  } catch (e) {
    $('pe-plan').replaceChildren();
    setMsg($('pm-edit-msg'), e.message);
  } finally {
    btn.disabled = false;
    btn.removeAttribute('aria-busy');
  }
}

async function saveEdit() {
  setMsg($('pm-edit-msg'), '');
  try {
    let res;
    if (editing) res = await api('PUT', `/api/v1/partman/configs/${editing.id}`, draft());
    else res = await api('POST', '/api/v1/partman/configs', draft());
    hide($('pm-edit'));
    cat.tbs.clear(); // 表列表里的“已有分区配置”标记变了，下次打开重新读
    setMsg($('pm-msg'), t('已保存'), 'info');
    for (const w of (res && res.warnings) || []) $('pm-msg').append(el('div', 'alert warn', tp(w)));
    if (editing) statusOf.delete(editing.id);
    await load();
    await refreshSummary();
  } catch (e) {
    setMsg($('pm-edit-msg'), e.message);
  }
}

// ---- 预览并执行 ----

const isInit = (plan) => !!plan && plan.kind === 'init' && !!plan.init;

function syncGo() {
  if (!running || !running.plan) return;
  const plan = running.plan;
  let ok = ov.can_run && plan.statements.length > 0 && !running.done && !!running.cfg.isenable;
  if (isInit(plan)) {
    ok = ok && plan.init.can_execute && ov.can_delete !== false;
  } else {
    if (plan.drops.length && $('pr-confirm').value.trim() !== `${running.cfg.dbname}.${running.cfg.tbname}`) ok = false;
    if (plan.reorganize && !$('pr-reorg-ok').checked) ok = false;
  }
  $('pr-go').disabled = !ok;
}

function startOpt() {
  const mode = (document.querySelector('input[name="pr-start"]:checked') || {}).value || 'current';
  return { start_mode: mode, start_date: mode === 'date' ? $('pr-start-date').value.trim() : '' };
}

function showStartControls(opt) {
  for (const r of document.querySelectorAll('input[name="pr-start"]')) r.checked = r.value === opt.start_mode;
  $('pr-start-date').value = opt.start_date || '';
  $('pr-start-date-box').hidden = opt.start_mode !== 'date';
}

async function openRun(r) {
  running = { cfg: r, plan: null, done: false, sawInit: false, opt: { start_mode: 'current', start_date: '' }, seq: 0 };
  $('pr-title').textContent = t('分区维护：{name}（{inst}）', { name: `${r.dbname}.${r.tbname}`, inst: r.instance_name });
  setMsg($('pr-msg'), '');
  $('pr-plan').replaceChildren(el('p', 'muted', t('正在读取目标表的分区信息…')));
  $('pr-danger').hidden = true;
  $('pr-reorg').hidden = true;
  $('pr-init').hidden = true;
  $('pr-go').hidden = true;
  $('pr-confirm').value = '';
  $('pr-reorg-ok').checked = false;
  showStartControls(running.opt);
  show($('pm-run'));
  await refreshRun();
}

// 把一份计划画到弹窗里（预览结果，或 409 带回的重算计划）。
function applyPlan(plan) {
  const r = running.cfg;
  running.plan = plan;
  running.done = false;
  const init = isInit(plan);
  if (init) running.sawInit = true;
  renderPlan($('pr-plan'), plan);
  const ul = $('pr-drops');
  ul.replaceChildren();
  for (const d of plan.drops || []) ul.append(el('li', '', d));
  $('pr-name').textContent = `${r.dbname}.${r.tbname}`;
  $('pr-danger').hidden = !(plan.drops || []).length;
  $('pr-reorg').hidden = !plan.reorganize;
  $('pr-init').hidden = !init;
  const perm = $('pr-init-perm');
  perm.hidden = !(init && ov.can_delete === false);
  perm.textContent = t('你没有初始化权限，只能预览。');
  const go = $('pr-go');
  go.textContent = init ? t('初始化为分区表…') : t('确认执行');
  go.hidden = !(ov.can_run && plan.statements.length);
  if (!r.isenable && ov.can_run) setMsg($('pr-msg'), t('该配置已停用，只能预览。'), 'warn');
  else setMsg($('pr-msg'), '');
  syncGo();
}

async function refreshRun() {
  const r = running.cfg;
  const mine = ++running.seq;
  const body = { id: r.id };
  if (running.sawInit) body.init = { ...running.opt };
  try {
    const plan = await api('POST', '/api/v1/partman/preview', body);
    if (mine !== running.seq) return;
    applyPlan(plan);
  } catch (e) {
    if (mine !== running.seq) return;
    $('pr-plan').replaceChildren();
    $('pr-go').hidden = true;
    $('pr-danger').hidden = true;
    $('pr-reorg').hidden = true;
    if (!running.sawInit) $('pr-init').hidden = true;
    let text = e.message;
    if (e.code === 'earliest_unavailable') text += t('。请改用“指定起始日期”，或“从当前窗口开始”。');
    setMsg($('pr-msg'), text);
  }
}

function renderResult(res) {
  const box = $('pr-plan');
  box.replaceChildren();
  if (res.notice) box.append(el('div', 'alert info', tp(res.notice)));
  const { tbl, tb } = newTable([t('结果'), t('语句'), t('耗时')]);
  const label = { success: t('成功'), failed: t('失败'), skipped: t('未执行') };
  for (const s of res.statements) {
    const tr = el('tr');
    const k = el('td');
    k.append(el('span', 'st ' + (s.status === 'success' ? 'passed' : s.status === 'failed' ? 'rejected' : 'aborted'), label[s.status] || s.status));
    const sql = el('td', 'pm-stmt', s.sql);
    if (s.error) sql.append(el('div', 'pm-log-err', tp(s.error)));
    tr.append(k, sql, td(s.elapsed_ms + ' ms', 'num'));
    tb.append(tr);
  }
  box.append(tbl);
  if (res.output && res.output.lines && res.output.lines.length) {
    const tb = el('div', 'pm-trace-box');
    tb.append(inlineTrace({ label: t('运行输出'), output: res.output, logId: res.log_id, open: true }));
    box.append(tb);
  }
}

function finishRun(res) {
  running.done = true;
  $('pr-danger').hidden = true;
  $('pr-reorg').hidden = true;
  $('pr-init').hidden = true;
  $('pr-go').hidden = true;
  renderResult(res);
  setMsg($('pr-msg'), res.failed ? t('有语句执行失败，后续语句未执行。') : t('执行完成。'), res.failed ? 'err' : 'info');
  statusOf.delete(running.cfg.id);
  return Promise.all([load(), refreshSummary()]);
}

async function go() {
  const plan = running.plan;
  if (isInit(plan)) { await goInit(); return; }
  const btn = $('pr-go');
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  setMsg($('pr-msg'), '');
  try {
    const res = await api('POST', `/api/v1/partman/configs/${running.cfg.id}/run`, { confirm_drops: plan.drops, confirm_reorganize: $('pr-reorg-ok').checked });
    await finishRun(res);
  } catch (e) {
    setMsg($('pr-msg'), e.message);
    if (e.code === 'confirm_required') await refreshRun(); // 表的分区在预览后变了：重新预览
  } finally {
    btn.removeAttribute('aria-busy');
    syncGo();
  }
}

// 初始化：统一确认组件（高危、键入「库.表」）。服务端会重新读取目标表并重算计划，
// 确认值（表名、分区个数、首末分区）与重算结果不一致就拒绝并带回新计划。
async function goInit() {
  const r = running.cfg;
  const plan = running.plan;
  const ini = plan.init;
  const name = `${r.dbname}.${r.tbname}`;
  const opt = { ...running.opt };
  let stale = false;
  const cr = await confirmRun({
    title: t('初始化为分区表'), level: 'high', code: 'partman.run', codeName: t('初始化分区'),
    lines: [
      t('将把 {name} 改造成 RANGE 分区表：整表重建，期间持有元数据锁、可能阻塞写入，执行后无法一键还原。', { name }),
      ...(plan.statements.some((x) => x.sql.length > 2000) ? [t('语句较长，只显示开头和结尾。')] : []),
    ],
    details: [
      { k: t('实例'), v: r.instance_name }, { k: t('库.表'), v: name },
      { k: t('起点'), v: `${START_LABEL[ini.start_mode] || ini.start_mode}${ini.start ? t('（{start}）', { start: ini.start }) : ''}` },
      { k: t('将创建分区'), v: t('{n} 个：{first} ~ {last}', { n: ini.partition_count, first: ini.first_partition, last: ini.last_partition }) },
      { k: t('最后上界（不含）'), v: ini.last_bound },
    ],
    sql: plan.statements.map((x) => (x.sql.length > 2000 ? shortSql(x.sql) : x.sql)),
    typed: name, typedLabel: t('请输入「库.表」确认初始化：'), okText: t('确认初始化'),
  }, async () => {
    if (stale) throw new Error(t('目标表已发生变化，页面上的计划已更新。请取消本窗口，核对新的计划后重新执行。'));
    try {
      return await api('POST', `/api/v1/partman/configs/${r.id}/run`, { init: opt, confirm_init: { ...ini.confirm, table: name } });
    } catch (e) {
      // 409 confirm_required / init_precheck_failed 带回重算后的计划：先更新页面，不自动用新值重试
      if (e.data && e.data.kind && running && running.cfg === r) { stale = true; applyPlan(e.data); }
      throw e;
    }
  });
  if (!cr.ok) return;
  await finishRun(cr.result);
  toast(cr.result.failed ? t('初始化失败，详情见弹窗与维护日志') : t('已初始化为分区表'), cr.result.failed ? 'err' : 'ok');
}

// 起点选择变化：重新预览（服务端按新起点重算分区个数与范围）。
function onStartChange() {
  if (!running || running.done) return;
  const opt = startOpt();
  $('pr-start-date-box').hidden = opt.start_mode !== 'date';
  if (opt.start_mode === 'date' && !opt.start_date) {
    setMsg($('pr-msg'), t('请填写起始日期。'), 'warn');
    $('pr-go').disabled = true;
    return;
  }
  running.opt = opt;
  setMsg($('pr-msg'), '');
  $('pr-go').disabled = true;
  $('pr-plan').replaceChildren(el('p', 'muted', opt.start_mode === 'earliest' ? t('正在读取最早数据时间…') : t('正在重新计算…')));
  refreshRun();
}

// ---- 日志 ----

async function openLog(r) {
  const body = $('pl-rows');
  body.replaceChildren();
  $('pl-title').textContent = t('维护日志：{name}', { name: `${r.dbname}.${r.tbname}` });
  $('pl-all').href = `/partman/logs?config_id=${encodeURIComponent(r.id)}`;
  show($('pm-log'));
  const note = (text) => {
    const tr = el('tr');
    const c = el('td', 'empty', text);
    c.colSpan = 6;
    tr.append(c);
    body.append(tr);
  };
  try {
    const rows = await api('GET', `/api/v1/partman/configs/${r.id}/logs?limit=10`);
    if (!rows.length) note(t('还没有维护日志'));
    for (const l of rows) {
      const tr = el('tr');
      const src = el('td');
      src.append(sourceTag(l.source));
      const st = el('td');
      st.append(logStatusTag(l.status));
      const res = el('td');
      const out = logOutputRow(l, 6);
      res.append(logItemsCell(l), out.btn);
      tr.append(td(l.id), src, td(l.operator || '—'), st, res, td(l.create_time));
      labelCells(tr, [t('日志 ID'), t('来源'), t('操作人'), t('状态'), t('执行结果'), t('时间')]);
      body.append(tr);
      if (out.row) body.append(out.row);
    }
  } catch (e) {
    note(e.message);
  }
}

// ---- 初始化 ----

async function init() {
  try {
    ov = await api('GET', '/api/v1/partman/overview');
  } catch (e) {
    setMsg($('pm-msg'), e.message);
    return;
  }
  fillSelect($('pm-inst'), ov.instances, (i) => i.name, (i) => String(i.id), ['', t('全部实例')]);
  fillSelect($('pm-coltype'), ov.col_types, (x) => x, (x) => x, ['', t('字段类型：全部')]);
  fillSelect($('pm-period'), ov.periods, (x) => x, (x) => x, ['', t('周期：全部')]);
  $('pm-new').hidden = !ov.can_add;
  showAlertNotes();
  initWebhooks(ov);
  const a = ov.auto;
  if (a.enabled) {
    const box = $('pm-auto');
    box.hidden = false;
    box.textContent = t('定时维护已开启：每 {n} 分钟一轮，{action}。', { n: a.interval_minutes, action: a.drop ? t('续建并清理过期分区') : t('只续建分区（不自动删除）') });
  }
  await load();
  await refreshSummary();
}

$('pm-filter').addEventListener('submit', (e) => { e.preventDefault(); state.offset = 0; load(); });
for (const id of ['pm-inst', 'pm-enable', 'pm-coltype', 'pm-period']) $(id).addEventListener('change', () => { state.offset = 0; load(); });
$('pm-prev').addEventListener('click', () => { state.offset = Math.max(0, state.offset - PAGE); load(); });
$('pm-next').addEventListener('click', () => { state.offset += PAGE; load(); });
$('pm-new').addEventListener('click', () => openEdit(null));
$('pm-refresh').addEventListener('click', refreshAll);
bindCascade();
$('pe-cancel').addEventListener('click', () => hide($('pm-edit')));
$('pe-preview').addEventListener('click', previewDraft);
$('pe-save').addEventListener('click', saveEdit);
$('pr-close').addEventListener('click', () => hide($('pm-run')));
$('pr-go').addEventListener('click', go);
$('pr-confirm').addEventListener('input', syncGo);
$('pr-reorg-ok').addEventListener('change', syncGo);
for (const r of document.querySelectorAll('input[name="pr-start"]')) r.addEventListener('change', onStartChange);
$('pr-start-apply').addEventListener('click', onStartChange);
$('pr-start-date').addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); onStartChange(); } });
$('pl-close').addEventListener('click', () => hide($('pm-log')));
bindAlert();
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  // 只关最上面的一个弹窗（推送通道编辑 > 推送设置 > 其它）
  for (const id of ['pm-hook-edit', 'pm-hook', 'pm-alert', 'pm-alerts-log', 'pm-edit', 'pm-run', 'pm-log']) {
    if (!$(id).hidden) { hide($(id)); return; }
  }
});

init();
