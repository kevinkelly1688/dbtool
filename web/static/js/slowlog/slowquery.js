/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: slowquery.js
 * @time: 2026/10/28
 */
// 慢查日志（SQL 优化）：统计（按 SQL 指纹聚合，顶部 KPI、行内趋势，可展开看历史趋势，可汇总全部实例）、明细、获取优化建议。
// 数据源由服务端按实例类型选择（pt-query-digest 库 / 平台采集 dbt_slow_logs / 阿里云 RDS），页面只显示当前用的是哪一份。
import { t, tp } from '../i18n.js';
import { api, $, el, setMsg, fmtNum, fmtSec, lastDays, qs, renderPager, bindSort, emptyRow, trendChart } from './lib.js';
import { initRedis } from './redis.js';
import { emptyHint } from './hints.js';

const els = {
  root: $('slowquery'), msg: $('sq-msg'), form: $('sq-form'), inst: $('sq-inst'), db: $('sq-db'), source: $('sq-source'),
  range: $('sq-range'), start: $('sq-start'), end: $('sq-end'), min: $('sq-min'), q: $('sq-q'), note: $('sq-note'),
  tabs: [...document.querySelectorAll('#slowquery > .tabbar .tab')], paneStat: $('sq-pane-stat'), paneDet: $('sq-pane-det'),
  kpis: $('sq-kpis'), kfps: $('sq-k-fps'), kcnt: $('sq-k-cnt'), ksum: $('sq-k-sum'), kavg: $('sq-k-avg'), kmax: $('sq-k-max'),
  shead: $('sq-shead'), srows: $('sq-srows'), spager: $('sq-spager'),
  dhead: $('sq-dhead'), drows: $('sq-drows'), dpager: $('sq-dpager'), filter: $('sq-filter'), clear: $('sq-clear'), advise: $('sq-advise'),
  modal: $('sq-modal'), mtitle: $('sq-mtitle'), msql: $('sq-msql'), mbody: $('sq-mbody'), mclose: $('sq-mclose'), mopen: $('sq-mopen'),
};
const ALL = 'all'; // 实例下拉里“全部实例（汇总）”的取值
const CAN_ADVISE = els.root.dataset.canAdvise === '1';
const CAN_OPEN = els.root.dataset.canOpen === '1';

const stat = { offset: 0, limit: 30, sort: 'exec_count', order: 'desc' };
const det = { offset: 0, limit: 30, sort: 'parse_rows', order: 'desc' };
let instances = [];
let tab = 'stat';
let sqlId = ''; // 明细按该 SQL 指纹过滤
let selected = null; // 明细里勾选的行

const isAll = () => els.inst.value === ALL;
const curInst = () => instances.find((i) => String(i.id) === els.inst.value);
const isRedis = () => { const i = curInst(); return !!i && i.source === 'redis'; }; // 选中 Redis 实例：换成 Redis 的命令统计 / 慢命令明细（redis.js）
const redisUI = initRedis({
  root: els.root, msg: els.msg, minSel: els.min, canReset: els.root.dataset.canRedisReset === '1',
  getParams: () => ({ instance_id: els.inst.value, start: els.start.value, end: els.end.value, q: els.q.value.trim(), min_seconds: els.min.value }),
  instName: () => (curInst() ? curInst().name : ''),
});
redisUI.init();

function common() {
  const o = { db_name: els.db.value, source: els.source.value, start: els.start.value, end: els.end.value, min_seconds: els.min.value, q: els.q.value.trim() };
  if (isAll()) o.all = 1;
  else o.instance_id = els.inst.value;
  return o;
}

// 某一行所在实例的查询参数（汇总模式下用行自己的实例，不再带 all）。
function rowCommon(r) {
  const o = common();
  delete o.all;
  o.instance_id = r.instance_id || els.inst.value;
  return o;
}

function setNote(page) {
  els.note.textContent = page && page.source_label ? t('当前数据源：{label}', { label: tp(page.source_label) }) : '';
}

function clipSQL(s) {
  s = s || '';
  return s.length > 100 ? s.slice(0, 100) + '…' : s;
}

const fmtDur = (sec) => {
  const v = Number(sec) || 0;
  if (v >= 3600) return (v / 3600).toFixed(1) + ' h';
  if (v >= 60) return (v / 60).toFixed(1) + ' min';
  return fmtSec(v) + ' s';
};

const shortTime = (s) => (s ? s.slice(5, 16) : '—');

function advisorURL(r) {
  return '/sqladvisor?' + qs({ inst: r.instance_id || els.inst.value, db: r.db_name, sql_id: r.sql_id });
}

// ---- 统计 ----

const SVG_NS = 'http://www.w3.org/2000/svg';

// 行内趋势：范围内每天的慢查次数（折线）。
function sparkline(vals) {
  const W = 84, H = 22, P = 2;
  const s = document.createElementNS(SVG_NS, 'svg');
  s.setAttribute('viewBox', `0 0 ${W} ${H}`);
  s.setAttribute('class', 'sq-spark');
  s.setAttribute('role', 'img');
  s.setAttribute('aria-label', t('范围内每天的慢查次数'));
  if (!vals || !vals.length) return s;
  const max = Math.max(...vals, 1);
  const n = vals.length;
  const pts = vals.map((v, i) => `${(P + (n === 1 ? (W - 2 * P) / 2 : (i * (W - 2 * P)) / (n - 1))).toFixed(1)},${(H - P - (v / max) * (H - 2 * P)).toFixed(1)}`);
  const line = document.createElementNS(SVG_NS, 'polyline');
  line.setAttribute('points', pts.join(' '));
  line.setAttribute('class', 'line');
  const ttl = document.createElementNS(SVG_NS, 'title');
  ttl.textContent = t('共 {total} 次；单日最多 {max} 次', { total: vals.reduce((a, b) => a + b, 0), max });
  s.append(line, ttl);
  return s;
}

function renderKpis(sum, page) {
  els.kpis.hidden = !sum;
  if (!sum) return;
  els.kfps.textContent = fmtNum(sum.fingerprints);
  els.kcnt.textContent = fmtNum(sum.exec_count);
  els.ksum.textContent = fmtDur(sum.exec_time_sum);
  els.kavg.textContent = sum.exec_count ? t('平均 {v} s', { v: fmtSec(sum.exec_time_sum / sum.exec_count) }) : '';
  els.kmax.textContent = fmtSec(sum.query_time_max) + ' s';
  if (page && page.notes && page.notes.length) setMsg(els.msg, page.notes.map(tp).join(' '), 'info');
}

function adviseBtn(r) {
  if (!CAN_ADVISE) return null;
  const b = el('button', 'btn sm', t('优化建议'));
  b.type = 'button';
  b.addEventListener('click', () => advise({
    instance_id: r.instance_id || Number(els.inst.value), db_name: r.db_name, sql: r.sample_sql || r.sql_text,
    rows_examined: r.parse_row_avg, rows_sent: r.return_row_avg, query_time: r.query_time_avg, lock_time: r.lock_time_avg,
  }, r));
  return b;
}

function openLink(r, sample, label = t('智能调优')) {
  if (!CAN_OPEN) return null;
  const a = el('a', 'btn sm', label);
  a.href = advisorURL(r);
  a.addEventListener('click', () => {
    if (!sample) return;
    try { sessionStorage.setItem('advisor_sql_content', sample); } catch (_) { /* 没有 sessionStorage 时由优化工具页按指纹取样例 */ }
  });
  return a;
}

function renderStat(page) {
  els.srows.replaceChildren();
  setNote(page);
  renderKpis(page.summary, page);
  if (!page.rows.length) els.srows.append(emptyRow(13, emptyHint('stat', page, isAll() ? null : curInst(), t)));
  for (const r of page.rows) {
    const tr = el('tr');
    const tg = el('td');
    const toggle = el('button', 'btn ghost', '▸');
    toggle.type = 'button';
    toggle.setAttribute('aria-expanded', 'false');
    toggle.setAttribute('aria-label', t('展开 SQL 与趋势'));
    tg.append(toggle);
    const where = el('td', 'nowrap');
    where.append(el('div', '', r.instance_name || (curInst() ? curInst().name : '')), el('small', 'muted', r.db_name || '—'));
    const link = el('a', 'mono wrap', clipSQL(r.sql_text));
    link.href = '#';
    link.title = r.sql_text;
    link.addEventListener('click', async (e) => {
      e.preventDefault();
      if (isAll() && r.instance_id) { els.inst.value = String(r.instance_id); await onInstanceChange(false); }
      sqlId = r.sql_id;
      det.offset = 0;
      showTab('det');
    });
    const sqlTd = el('td', 'mono wrap');
    sqlTd.append(link, el('div', 'muted small mono', r.sql_id));
    const num = (total, avg) => {
      const td = el('td', 'num', fmtNum(total));
      td.append(el('small', 'muted sq-avg', t('均 {v}', { v: fmtNum(avg) })));
      return td;
    };
    const sp = el('td');
    sp.append(sparkline(r.spark));
    const ops = el('td', 'sq-ops');
    const ab = adviseBtn(r);
    const ol = openLink(r, r.sample_sql);
    if (ab) ops.append(ab);
    if (ol) ops.append(' ', ol);
    tr.append(tg, where, sqlTd, el('td', 'num', r.exec_count), el('td', 'num', fmtSec(r.exec_time_sum)), el('td', 'num', fmtSec(r.query_time_avg)),
      el('td', 'num', fmtSec(r.query_time_max)), num(r.parse_total_rows, r.parse_row_avg), num(r.return_total_rows, r.return_row_avg),
      el('td', 'small nowrap', shortTime(r.first_seen)), el('td', 'small nowrap', shortTime(r.last_seen || r.create_time)), sp, ops);
    const dtr = el('tr');
    dtr.hidden = true;
    const dtd = el('td');
    dtd.colSpan = 13;
    dtr.append(dtd);
    let loaded = false;
    toggle.addEventListener('click', () => {
      const open = dtr.hidden;
      dtr.hidden = !open;
      toggle.textContent = open ? '▾' : '▸';
      toggle.setAttribute('aria-expanded', String(open));
      if (open && !loaded) { loaded = true; fillExpand(dtd, r); }
    });
    els.srows.append(tr, dtr);
  }
  renderPager(els.spager, { total: page.total, offset: stat.offset, limit: stat.limit }, (off) => { stat.offset = off; loadStat(); });
}

// 展开行：SQL 全文 + 历史趋势。
async function fillExpand(td, r) {
  const grid = el('div', 'dgrid');
  grid.style.margin = '0';
  grid.append(el('pre', 'sqlbox mono', r.sample_sql || r.sql_text));
  const right = el('div');
  right.append(el('b', 'small', t('SQL 历史趋势')), document.createTextNode(' '), el('span', 'muted small', t('次数（柱）· 95% 耗时（线）')));
  const holder = el('div', 'muted small', t('加载中…'));
  right.append(holder);
  grid.append(right);
  td.append(grid);
  try {
    const tr = await api('GET', '/api/v1/slowlog/review/trend?' + qs({ ...rowCommon(r), sql_id: r.sql_id, bucket: spanDays() <= 3 ? 'hour' : 'day' }));
    holder.replaceChildren(trendChart(tr.points));
    if (tr.approx) right.append(el('p', 'muted small', t('明细行数过多，95% 耗时基于部分明细估算。')));
  } catch (e) {
    holder.textContent = t('趋势加载失败：{msg}', { msg: e.message });
  }
}

function spanDays() {
  const a = Date.parse(els.start.value), b = Date.parse(els.end.value);
  return Number.isNaN(a) || Number.isNaN(b) ? 7 : Math.round((b - a) / 86400000) + 1;
}

async function loadStat() {
  setMsg(els.msg, '');
  els.srows.replaceChildren(emptyRow(13, t('加载中…')));
  try {
    renderStat(await api('GET', '/api/v1/slowlog/review?' + qs({ ...common(), sort: stat.sort, order: stat.order, limit: stat.limit, offset: stat.offset })));
  } catch (e) {
    els.srows.replaceChildren(emptyRow(13, t('加载失败')));
    els.spager.replaceChildren();
    els.kpis.hidden = true;
    setMsg(els.msg, e.message);
  }
}

// ---- 明细 ----

function renderDet(page) {
  els.drows.replaceChildren();
  selected = null;
  setNote(page);
  if (!page.rows.length) els.drows.append(emptyRow(11, emptyHint('det', page, isAll() ? null : curInst(), t)));
  for (const r of page.rows) {
    const tr = el('tr');
    const pick = el('td');
    const radio = document.createElement('input');
    radio.type = 'radio';
    radio.name = 'sq-pick';
    radio.setAttribute('aria-label', t('选择这条 SQL'));
    radio.addEventListener('change', () => { selected = r; });
    pick.append(radio);
    const sqlTd = el('td', 'mono wrap', clipSQL(r.sql_text));
    sqlTd.title = r.sql_text;
    tr.append(pick, el('td', '', r.exec_start), el('td', '', r.db_name || '—'), el('td', 'mono', r.host_address), sqlTd, el('td', 'num', r.exec_count),
      el('td', 'num', fmtSec(r.query_time_pct95)), el('td', 'num', fmtSec(r.query_time_sum)), el('td', 'num', fmtSec(r.lock_time_sum)),
      el('td', 'num', fmtNum(r.parse_rows)), el('td', 'num', fmtNum(r.return_rows)));
    tr.addEventListener('click', (e) => { if (e.target !== radio) { radio.checked = true; selected = r; } });
    els.drows.append(tr);
  }
  renderPager(els.dpager, { total: page.total, offset: det.offset, limit: det.limit }, (off) => { det.offset = off; loadDet(); });
}

async function loadDet() {
  setMsg(els.msg, '');
  els.filter.textContent = sqlId ? t('按 SQL 指纹过滤：{id}', { id: sqlId }) : '';
  els.clear.hidden = !sqlId;
  if (isAll()) {
    els.drows.replaceChildren(emptyRow(11, t('汇总模式没有明细，请先选择具体实例')));
    els.dpager.replaceChildren();
    return;
  }
  els.drows.replaceChildren(emptyRow(11, t('加载中…')));
  try {
    renderDet(await api('GET', '/api/v1/slowlog/review/history?' + qs({ ...common(), sql_id: sqlId, sort: det.sort, order: det.order, limit: det.limit, offset: det.offset })));
  } catch (e) {
    els.drows.replaceChildren(emptyRow(11, t('加载失败')));
    els.dpager.replaceChildren();
    setMsg(els.msg, e.message);
  }
}

// ---- 优化建议 ----

const LEVEL = { crit: ['badge crit', t('严重')], warn: ['badge warn', t('注意')], info: ['badge', t('提示')] };
const scoreClass = (s) => (s >= 80 ? 'good' : s >= 60 ? 'mid' : 'bad');

// 获取一条 SQL 的优化建议。body 是 /slowlog/advise 的请求；src 是来源行（用来生成“在优化工具中打开”的链接）。
async function advise(body, src) {
  setMsg(els.msg, '');
  const busyBtns = [els.advise, ...document.querySelectorAll('.sq-ops button')].filter(Boolean);
  for (const b of busyBtns) b.disabled = true;
  try {
    const adv = await api('POST', '/api/v1/slowlog/advise', body);
    els.msql.textContent = body.sql;
    els.mbody.replaceChildren();
    const sc = el('div', 'sq-score');
    sc.append(el('span', 'muted', t('综合评分')), el('b', scoreClass(adv.score), String(adv.score)), el('span', 'muted', t('命中 {n} 条建议', { n: adv.items.length })));
    els.mbody.append(sc);
    for (const it of adv.items) {
      const box = el('div', 'advice');
      const head = el('div', 'advice-head');
      const [cls, label] = LEVEL[it.level] || LEVEL.info;
      head.append(el('span', cls, label), el('b', '', ' ' + it.title));
      box.append(head, el('p', 'muted', it.detail));
      els.mbody.append(box);
    }
    if (adv.index_ddl && adv.index_ddl.length) {
      els.mbody.append(el('p', 'muted small sq-sec', t('候选索引（仅供参考）')), el('pre', 'sqlbox mono', adv.index_ddl.join('\n')));
    }
    els.mbody.append(el('p', 'muted small', t('来源：{label}', { label: tp(adv.source_label) })));
    if (CAN_OPEN && src) {
      els.mopen.hidden = false;
      els.mopen.href = advisorURL(src);
      els.mopen.onclick = () => {
        try { sessionStorage.setItem('advisor_sql_content', body.sql); } catch (_) { /* 由优化工具页按指纹取样例 */ }
      };
    } else {
      els.mopen.hidden = true;
    }
    els.modal.hidden = false;
    els.mclose.focus();
  } catch (e) {
    setMsg(els.msg, e.message);
  } finally {
    for (const b of busyBtns) b.disabled = false;
  }
}

async function adviseSelected() {
  if (!selected) return setMsg(els.msg, t('请先选择需要优化的 SQL'));
  const n = Math.max(selected.exec_count, 1); // 明细行可能是多次执行的汇总：换算成单次平均
  await advise({
    instance_id: Number(els.inst.value), db_name: selected.db_name, sql: selected.sql_text,
    rows_examined: Math.round(selected.parse_rows / n), rows_sent: Math.round(selected.return_rows / n),
    query_time: selected.query_time_sum / n, lock_time: selected.lock_time_sum / n,
  }, { instance_id: Number(els.inst.value), db_name: selected.db_name, sql_id: selected.sql_id });
}

// ---- 页签与筛选 ----

function showTab(name) {
  tab = name;
  for (const tb of els.tabs) {
    const on = tb.dataset.tab === name;
    tb.classList.toggle('on', on);
    tb.setAttribute('aria-selected', String(on));
  }
  els.paneStat.hidden = name !== 'stat';
  els.paneDet.hidden = name !== 'det';
  reload();
}

function reload() {
  if (!els.inst.value) {
    setMsg(els.msg, t('请先选择实例'), 'info');
    return;
  }
  if (isRedis()) { redisUI.reload(); return; }
  const inst = curInst();
  if (inst && inst.hint) {
    setMsg(els.msg, tp(inst.hint), 'warn');
    (tab === 'stat' ? els.srows : els.drows).replaceChildren(emptyRow(tab === 'stat' ? 13 : 11, tp(inst.hint)));
    return;
  }
  if (tab === 'stat') loadStat(); else loadDet();
}

async function loadDatabases() {
  els.db.replaceChildren(new Option(t('全部数据库'), ''));
  if (isRedis()) return; // Redis 没有“库”维度的慢日志
  if (!els.inst.value || isAll()) return;
  try {
    const list = await api('GET', '/api/v1/slowlog/databases?' + qs({ instance_id: els.inst.value, source: els.source.value, start: els.start.value, end: els.end.value }));
    for (const d of list) els.db.append(new Option(d, d));
  } catch (_) { /* 库列表只是便利功能，失败时仍可不选库查询 */ }
}

function applyPreset() {
  if (els.range.value === 'custom') return;
  [els.start.value, els.end.value] = lastDays(Number(els.range.value));
}

function resetPaging() { stat.offset = 0; det.offset = 0; sqlId = ''; redisUI.resetPaging(); }

async function onInstanceChange(doReload = true) {
  const inst = curInst();
  const redisMode = isRedis();
  redisUI.setActive(redisMode);
  document.querySelector('#slowquery > .tabbar').hidden = redisMode;
  els.paneStat.hidden = redisMode || tab !== 'stat';
  els.paneDet.hidden = redisMode || tab !== 'det';
  els.db.hidden = redisMode;
  els.source.hidden = !(inst && inst.db_type === 'mysql');
  els.source.value = '';
  els.db.disabled = isAll(); // 汇总模式不能按库过滤（各实例的库不同），要按库看请选具体实例
  if (doReload) { resetPaging(); redisUI.resetPaging(); }
  await loadDatabases();
  if (doReload) reload();
}

els.inst.addEventListener('change', () => onInstanceChange(true));
els.db.addEventListener('change', () => { resetPaging(); reload(); });
els.min.addEventListener('change', () => { resetPaging(); reload(); });
els.source.addEventListener('change', async () => { resetPaging(); await loadDatabases(); reload(); });
els.range.addEventListener('change', async () => { applyPreset(); resetPaging(); await loadDatabases(); reload(); });
for (const i of [els.start, els.end]) i.addEventListener('change', () => { els.range.value = 'custom'; });
els.form.addEventListener('submit', async (e) => { e.preventDefault(); resetPaging(); await loadDatabases(); reload(); });
for (const tb of els.tabs) tb.addEventListener('click', () => showTab(tb.dataset.tab));
els.clear.addEventListener('click', () => { sqlId = ''; det.offset = 0; loadDet(); });
if (els.advise) els.advise.addEventListener('click', adviseSelected);
const closeModal = () => { els.modal.hidden = true; };
els.mclose.addEventListener('click', closeModal);
els.modal.addEventListener('click', (e) => { if (e.target === els.modal) closeModal(); });
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !els.modal.hidden) closeModal(); });
bindSort(els.shead, stat, reload);
bindSort(els.dhead, det, reload);

(async () => {
  applyPreset();
  try {
    instances = await api('GET', '/api/v1/slowlog/instances?page=query');
    els.inst.append(new Option(t('全部实例（汇总）'), ALL));
    for (const i of instances) {
      const o = new Option(t('{name}（{label}）', { name: i.name, label: i.db_type }) + (i.hint ? ' · ' + tp(i.hint) : ''), String(i.id));
      els.inst.append(o);
    }
    const want = new URLSearchParams(location.search).get('instance_id'); // 从别处带实例跳转过来时预选
    if (want && instances.some((i) => String(i.id) === want)) {
      els.inst.value = want;
      els.inst.dispatchEvent(new Event('change'));
    }
  } catch (e) {
    setMsg(els.msg, e.message);
  }
})();
