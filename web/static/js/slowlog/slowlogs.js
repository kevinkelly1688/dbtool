/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: slowlogs.js
 * @time: 2026/10/28
 */
// 查慢日志：平台采集的 dbt_slow_logs，按“实例 × 库 × SQL 指纹 × 天”聚合；顶部是 KPI 与每日折线；
// 点“详细”打开抽屉（摘要卡片 + SQL 指纹 + 去优化工具链接 + 当天最近 100 条）；底部是采集器状态。
import { t, tp } from '../i18n.js';
import { api, $, el, setMsg, fmtNum, fmtSec, lastDays, fmtTime, qs, renderPager, bindSort, emptyRow } from './lib.js';
import { lineChart } from './slowlogs_chart.js';

const COLS = 13;
const els = {
  page: $('slowlogs'),
  msg: $('sl-msg'), form: $('sl-form'), inst: $('sl-inst'), db: $('sl-db'), range: $('sl-range'), start: $('sl-start'), end: $('sl-end'), q: $('sl-q'),
  rows: $('sl-rows'), pager: $('sl-pager'), head: $('sl-head'),
  kTotal: $('sl-k-total'), kFps: $('sl-k-fps'), kTime: $('sl-k-time'), kMax: $('sl-k-max'), kMaxNote: $('sl-k-maxnote'),
  chartCount: $('sl-chart-count'), chartTime: $('sl-chart-time'), chartCountSub: $('sl-chart-count-sub'),
  modal: $('sl-modal'), mrows: $('sl-mrows'), mtitle: $('sl-mtitle'), mclose: $('sl-mclose'), msum: $('sl-msum'), mfp: $('sl-mfp'),
  mopt: $('sl-mopt'), moptWrap: $('sl-mopt-wrap'), mlistTitle: $('sl-mlist-title'),
  cstate: $('sl-cstate'), crows: $('sl-crows'), cnote: $('sl-cnote'),
};
const canOptimize = els.page.dataset.canOpt === '1';
const state = { offset: 0, limit: 30, sort: 'exec_date', order: 'desc' };
let statsSeq = 0; // 统计请求的序号：只采用最后一次发出的请求结果

// 过滤条件：列表与统计共用（统计与分页、排序无关）。
function filters() {
  return { instance_id: els.inst.value, db_name: els.db.value, start: els.start.value, end: els.end.value, q: els.q.value.trim() };
}

function params() {
  return { ...filters(), sort: state.sort, order: state.order, limit: state.limit, offset: state.offset };
}

const triple = (a, b, c) => `${fmtNum(a)} / ${fmtNum(b)} / ${fmtNum(c)}`;

// 秒 -> 易读时长：12.3s / 4分05秒 / 3时20分 / 2天5时。
function fmtDur(sec) {
  const s = Number(sec) || 0;
  if (s < 60) return fmtSec(s) + ' s';
  const pad = (n) => String(n).padStart(2, '0');
  if (s < 3600) return t('{m}分{s}秒', { m: Math.floor(s / 60), s: pad(Math.floor(s % 60)) });
  if (s < 86400) return t('{h}时{m}分', { h: Math.floor(s / 3600), m: pad(Math.floor((s % 3600) / 60)) });
  return t('{d}天{h}时', { d: Math.floor(s / 86400), h: Math.floor((s % 86400) / 3600) });
}

// 折线图纵轴刻度：小数值保留一位，大数缩写。
const tick = (v) => (v >= 10 ? fmtNum(v) : String(Math.round(v * 10) / 10));

function sqlRow(g, cols) {
  const tr = el('tr', 'sql-expand');
  tr.hidden = true;
  const td = el('td');
  td.colSpan = cols;
  const pre = el('pre', 'sqlbox mono', g.sql_text);
  const note = [t('代表 SQL')];
  if (g.table_name) note.push(t('主表：{name}', { name: g.table_name }));
  if (g.text_truncated) note.push(t('内容过长已截断'));
  td.append(pre, el('p', 'muted small', note.join(' · ')));
  tr.append(td);
  return tr;
}

function render(page) {
  els.rows.replaceChildren();
  if (!page.rows.length) {
    els.rows.append(emptyRow(COLS, t('该时间范围内没有慢日志（请确认实例已开启“慢日志采集”）')));
  }
  for (const g of page.rows) {
    const tr = el('tr', 'clickable');
    const ops = el('td', 'row-actions');
    const btn = el('button', 'btn', t('详细'));
    btn.type = 'button';
    btn.addEventListener('click', (e) => { e.stopPropagation(); openDetail(g); });
    ops.append(btn);
    const fp = el('td', 'fp mono', g.fingerprint || '—');
    fp.title = g.fingerprint || '';
    tr.append(el('td', '', g.instance_name), el('td', '', g.db_name || '—'), fp, el('td', 'num', g.count), el('td', '', g.exec_date),
      el('td', 'mono', g.user_host), el('td', 'num strong', fmtSec(g.query_time_sum)), el('td', 'num', fmtSec(g.query_time_avg)),
      el('td', 'num' + (g.query_time_max >= 30 ? ' hot' : ''), fmtSec(g.query_time_max)),
      el('td', 'num', fmtSec(g.query_time_min)), el('td', 'num', triple(g.rows_sent_avg, g.rows_sent_max, g.rows_sent_min)),
      el('td', 'num', triple(g.rows_examined_avg, g.rows_examined_max, g.rows_examined_min)), ops);
    const detail = sqlRow(g, COLS);
    tr.addEventListener('click', () => { detail.hidden = !detail.hidden; });
    els.rows.append(tr, detail);
  }
  renderPager(els.pager, { total: page.total, offset: state.offset, limit: state.limit }, (off) => { state.offset = off; load(); });
}

async function load() {
  setMsg(els.msg, '');
  els.rows.replaceChildren(emptyRow(COLS, t('加载中…')));
  try {
    render(await api('GET', '/api/v1/slowlog/slowlogs?' + qs(params())));
  } catch (e) {
    els.rows.replaceChildren(emptyRow(COLS, t('加载失败')));
    els.pager.replaceChildren();
    setMsg(els.msg, e.message);
  }
}

// ---- 顶部 KPI、折线图、库下拉 ----

function resetStats(note) {
  for (const k of [els.kTotal, els.kFps, els.kTime, els.kMax]) k.textContent = '—';
  els.kMaxNote.textContent = note || '';
  els.chartCount.replaceChildren();
  els.chartTime.replaceChildren();
  els.chartCountSub.textContent = '';
}

// 库下拉：选项来自统计接口（范围内出现过的库）；已选的库即使这次范围里没有也保留。
function fillDatabases(list) {
  const cur = els.db.value;
  const names = [...list];
  if (cur && !names.includes(cur)) names.unshift(cur);
  els.db.replaceChildren(new Option(t('全部库'), ''));
  for (const n of names) els.db.append(new Option(n, n));
  els.db.value = cur;
}

function renderStats(st) {
  els.kTotal.textContent = fmtNum(st.total);
  els.kTotal.title = String(st.total);
  els.kFps.textContent = fmtNum(st.fingerprints);
  els.kTime.textContent = fmtDur(st.total_time);
  els.kTime.title = fmtSec(st.total_time) + ' s';
  if (st.slowest && st.max_time > 0) {
    const s = st.slowest;
    els.kMax.textContent = fmtSec(st.max_time) + ' s';
    els.kMaxNote.textContent = [s.instance_name, s.db_name, (s.start_time || '').slice(0, 10)].filter(Boolean).join(' · ');
    els.kMaxNote.title = (els.kMaxNote.textContent + '\n' + (s.fingerprint || '')).trim();
  } else {
    els.kMax.textContent = '—';
    els.kMaxNote.textContent = '';
    els.kMaxNote.title = '';
  }
  const days = st.days || [];
  els.chartCountSub.textContent = t('{n} 天', { n: days.length });
  els.chartCount.replaceChildren(lineChart(days.map((d) => ({ label: d.day, value: d.count })),
    { cls: 'c1', name: t('次数'), fmt: tick, ariaLabel: t('每日慢 SQL 次数折线图') }));
  els.chartTime.replaceChildren(lineChart(days.map((d) => ({ label: d.day, value: d.total_time })),
    { cls: 'c2', name: t('累计耗时(s)'), fmt: tick, ariaLabel: t('每日累计耗时折线图') }));
  fillDatabases(st.databases || []);
}

async function loadStats() {
  const seq = ++statsSeq;
  try {
    const st = await api('GET', '/api/v1/slowlog/slowlogs/stats?' + qs(filters()));
    if (seq === statsSeq) renderStats(st);
  } catch (e) {
    // 统计失败不影响列表：KPI 显示 —，并给出原因。
    if (seq === statsSeq) resetStats(t('统计加载失败：{msg}', { msg: e.message }));
  }
}

function reloadAll() {
  state.offset = 0;
  load();
  loadStats();
}

// ---- 明细抽屉 ----

// 去优化工具的链接格式（与“优化+性能”约定）：/sqladvisor?inst=<实例ID>&db=<库>&sql_id=<指纹>。
function optimizeHref(g) {
  return '/sqladvisor?' + qs({ inst: g.instance_id, db: g.db_name, sql_id: g.sql_md5 });
}

function sumCard(label, value, title) {
  const d = el('div');
  const b = el('b', '', value);
  if (title) b.title = title;
  d.append(b, el('span', '', label));
  return d;
}

function fillSummary(g) {
  els.msum.replaceChildren(
    sumCard(t('实例'), g.instance_name || '—'),
    sumCard(t('库'), g.db_name || '—'),
    sumCard(t('日期'), g.exec_date),
    sumCard(t('当天次数'), String(g.count)),
    sumCard(t('累计耗时'), fmtDur(g.query_time_sum), fmtSec(g.query_time_sum) + ' s'),
    sumCard(t('平均 / 最大 / 最小(s)'), `${fmtSec(g.query_time_avg)} / ${fmtSec(g.query_time_max)} / ${fmtSec(g.query_time_min)}`),
    sumCard(t('扫描行（均）'), fmtNum(g.rows_examined_avg), String(g.rows_examined_avg)),
    sumCard(t('返回行（均）'), fmtNum(g.rows_sent_avg), String(g.rows_sent_avg)),
    sumCard(t('主表'), g.table_name || '—'),
  );
  els.mfp.textContent = g.fingerprint || t('（无法计算指纹）');
  const show = canOptimize && g.instance_id && g.sql_md5;
  els.moptWrap.hidden = !show;
  els.mopt.href = show ? optimizeHref(g) : '/sqladvisor';
}

async function openDetail(g) {
  els.mtitle.textContent = t('慢 SQL 明细 · {inst} · {db} · {date}', { inst: g.instance_name, db: g.db_name || '—', date: g.exec_date });
  fillSummary(g);
  els.mlistTitle.textContent = t('最近 {n} 条', { n: Math.min(g.count, 100) });
  els.mrows.replaceChildren(emptyRow(8, t('加载中…')));
  els.modal.hidden = false;
  els.mclose.focus();
  try {
    const d = await api('GET', '/api/v1/slowlog/slowlogs/detail?' + qs({ instance_id: g.instance_id, db_name: g.db_name, sql_md5: g.sql_md5, exec_date: g.exec_date }));
    els.mrows.replaceChildren();
    if (!d.rows.length) els.mrows.append(emptyRow(8, t('没有明细')));
    for (const r of d.rows) {
      const tr = el('tr');
      tr.append(el('td', '', r.start_time), el('td', 'mono', r.user_host), el('td', '', r.db_name || '—'), el('td', 'num', fmtSec(r.query_time_sec)),
        el('td', 'num', fmtSec(r.lock_time_sec)), el('td', 'num', r.rows_sent), el('td', 'num', r.rows_examined), el('td', 'mono wrap', r.sql_text));
      els.mrows.append(tr);
    }
  } catch (e) {
    els.mrows.replaceChildren(emptyRow(8, e.message));
  }
}

function closeDetail() { els.modal.hidden = true; }

// ---- 采集器状态 ----

const STATUS = { ok: ['passed', t('正常')], error: ['rejected', t('失败')], skipped: ['aborted', t('已跳过')], none: ['aborted', t('无记录')] };

async function loadCollector() {
  try {
    const ov = await api('GET', '/api/v1/slowlog/collector');
    els.cstate.textContent = ov.enabled ? t('已启用 · 每 {sec} 秒一轮 · 并行 {n} 个实例', { sec: ov.interval_sec, n: ov.workers }) : t('已被环境变量关闭');
    const notes = [];
    if (!ov.enabled) notes.push(t('采集器被环境变量 SLOWLOG_COLLECT_ENABLE 关闭。'));
    if (!ov.state_table) notes.push(t('采集状态表 dbt_slowlog_collect_state 未创建。'));
    els.cnote.textContent = notes.join(' ');
    els.crows.replaceChildren();
    if (!ov.items.length) els.crows.append(emptyRow(8, t('没有开启“慢日志采集”的实例')));
    for (const s of ov.items) {
      const tr = el('tr');
      const st = el('td');
      const [cls, label] = STATUS[s.status] || STATUS.none;
      st.append(el('span', 'st ' + cls, label));
      tr.append(el('td', '', s.instance_name), st, el('td', '', fmtTime(s.last_run_at)), el('td', '', fmtTime(s.last_ok_at)),
        el('td', 'num', s.last_rows), el('td', 'num', s.total_rows), el('td', 'num', s.duration_ms), el('td', 'wrap', tp(s.error || '')));
      els.crows.append(tr);
    }
  } catch (e) {
    els.cstate.textContent = t('采集状态加载失败：{msg}', { msg: e.message });
  }
}

// ---- 初始化 ----

function applyPreset() {
  const v = els.range.value;
  if (v === 'custom') return;
  [els.start.value, els.end.value] = lastDays(Number(v));
}

els.range.addEventListener('change', () => { applyPreset(); reloadAll(); });
for (const i of [els.start, els.end]) i.addEventListener('change', () => { els.range.value = 'custom'; });
els.inst.addEventListener('change', () => { els.db.value = ''; reloadAll(); }); // 换实例后库名不再适用
els.db.addEventListener('change', reloadAll);
els.form.addEventListener('submit', (e) => { e.preventDefault(); reloadAll(); });
els.mclose.addEventListener('click', closeDetail);
els.modal.addEventListener('click', (e) => { if (e.target === els.modal) closeDetail(); });
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !els.modal.hidden) closeDetail(); });
bindSort(els.head, state, load);

(async () => {
  applyPreset();
  try {
    const list = await api('GET', '/api/v1/slowlog/instances?page=logs');
    for (const i of list) els.inst.append(new Option(t('{name}（{label}）', { name: i.name, label: i.db_type }), String(i.id)));
  } catch (e) {
    setMsg(els.msg, e.message);
  }
  load();
  loadStats();
  loadCollector();
})();
