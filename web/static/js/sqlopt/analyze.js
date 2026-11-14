/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: analyze.js
 * @time: 2026/10/28
 */
// SQL 分析：上传 .sql / Mapper XML / 慢日志 → 拆成逐条 SQL → 后台逐条分析（轮询进度）→ 查看每条的报告；
// 页面底部“最近报告”列出仍在保留期内的任务（含剩余保留倒计时），可切换查看。
import { api } from '../common.js';
import { t, locale } from '../i18n.js';
import { API, el, option, setMsg, loadOptions, loadDatabases, pollJob, levelBadge, renderReport, scoreClass } from './shared.js';

const $ = (id) => document.getElementById(id);
const root = $('sqlanalyze');
const canAnalyze = root.dataset.canAnalyze === 'true';
const maxBytes = Number(root.dataset.maxMb || 5) * 1024 * 1024;
const JOB_KEY = 'sqlopt_analyze_job';

// sessionStorage 在隐私模式等场景可能不可用，读写都不能影响页面功能
const mem = {
  get() { try { return sessionStorage.getItem(JOB_KEY); } catch (_) { return null; } },
  set(v) { try { sessionStorage.setItem(JOB_KEY, v); } catch (_) { /* 忽略 */ } },
  del() { try { sessionStorage.removeItem(JOB_KEY); } catch (_) { /* 忽略 */ } },
};

const els = {
  msg: $('sa-msg'), notice: $('sa-notice'), file: $('sa-file'), inst: $('sa-inst'), db: $('sa-db'), start: $('sa-start'),
  cancel: $('sa-cancel'), status: $('sa-status'), bar: $('sa-bar'), fill: $('sa-bar-fill'), rows: $('sa-rows'),
  summary: $('sa-summary'), modal: $('sa-modal'), jobCard: $('sa-job-card'), jobMeta: $('sa-job-meta'), jobKeep: $('sa-job-keep'),
  recent: $('sa-recent'),
};

let text = '';
let fileName = '';
let rowsEls = []; // { tr, fp, type, tables, score, st, lv, act, sig }
let job = null;
let poller = null;

const ST = {
  waiting: ['waiting', t('排队')], running: ['running', t('分析中')], done: ['passed', t('已完成')],
  failed: ['rejected', t('失败')], canceled: ['aborted', t('已取消')], queued: ['waiting', t('排队')],
};
const FORMAT_NAME = { sql: t('SQL 文本'), mapper: 'Mapper XML', slowlog: t('慢日志') };

const num = (n) => Number(n).toLocaleString(locale);
const fmtMin = (sec) => {
  if (sec <= 0) return t('即将清理');
  if (sec < 60) return t('不到 1 分钟');
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  return t('{m} 分 {s} 秒', { m, s: String(s).padStart(2, '0') });
};

function statsText(s) {
  if (!s) return '';
  const parts = [];
  if (s.time) parts.push(s.time);
  if (s.db) parts.push(t('库 {name}', { name: s.db }));
  parts.push(t('耗时 {v}s', { v: s.query_time }), t('扫描 {n} 行', { n: num(s.rows_examined) }), t('返回 {n} 行', { n: num(s.rows_sent) }));
  return parts.join(' · ');
}

function scoreCell(td, score) {
  td.replaceChildren();
  if (score === undefined || score === null) return;
  td.append(el('span', 'sa-score ' + scoreClass(score), String(score)));
}

function buildRow(r, i) {
  const tr = el('tr');
  const sqlTd = el('td', 'wrap mono so-sql', r.sql);
  if (r.stats) sqlTd.append(el('div', 'muted small', statsText(r.stats)));
  const fp = el('td');
  const type = el('td', '', r.type || '');
  const tables = el('td', 'sa-tables');
  const score = el('td');
  const st = el('td');
  const lv = el('td');
  const act = el('td', 'row-actions');
  tr.append(el('td', '', String(r.sql_id)), sqlTd, fp, type, tables, score, st, lv, act);
  const e = { tr, fp, type, tables, score, st, lv, act, sig: '' };
  rowsEls[i] = e;
  return e;
}

function setRow(e, it, idx) {
  const sig = [it.status, it.findings, it.level, it.error, it.type, it.score, it.fp_id].join('|');
  if (sig === e.sig) return;
  e.sig = sig;
  e.type.textContent = it.type || '';
  e.fp.replaceChildren();
  if (it.fp_id) e.fp.append(el('span', 'sa-fpid', it.fp_id));
  e.tables.textContent = (it.tables || []).join(', ');
  scoreCell(e.score, it.score);
  const [cls, label] = ST[it.status] || ['aborted', it.status];
  e.st.replaceChildren(el('span', 'st ' + cls, label));
  if (it.error) e.st.append(el('div', 'muted small', it.error));
  e.lv.replaceChildren();
  if (it.status === 'done') {
    if (it.findings > 0) e.lv.append(levelBadge(it.level), document.createTextNode(' ' + t('{n} 条', { n: it.findings })));
    else e.lv.append(el('span', 'muted', t('无')));
  }
  e.act.replaceChildren();
  if (it.status === 'done') {
    const b = el('button', 'btn', t('查看'));
    b.type = 'button';
    b.addEventListener('click', () => openReport(idx));
    e.act.append(b);
  }
}

function showRows(list) {
  rowsEls = [];
  els.rows.replaceChildren();
  if (!list.length) {
    const tr = el('tr');
    const td = el('td', 'empty', t('没有解析出 SQL'));
    td.colSpan = 9;
    tr.append(td);
    els.rows.append(tr);
    return;
  }
  list.forEach((r, i) => {
    const e = buildRow(r, i);
    if (r.error) { e.st.append(el('span', 'st aborted', t('已跳过')), el('div', 'muted small', r.error)); e.sig = 'x'; }
    else e.st.append(el('span', 'st waiting', t('待分析')));
    els.rows.append(e.tr);
  });
}

function setBusy(busy) {
  els.file.disabled = busy;
  els.inst.disabled = busy;
  els.db.disabled = busy || !els.inst.value;
  els.cancel.hidden = !busy;
  els.start.hidden = busy;
}

function jobMeta(v) {
  const parts = [];
  if (v.file) parts.push(v.file);
  if (v.format) parts.push(FORMAT_NAME[v.format] || v.format);
  parts.push(v.instance ? `${v.instance} / ${v.db || t('全部库')}` : t('静态规则分析'));
  return parts.join(' · ');
}

function renderKeep(v) {
  const active = v.status === 'queued' || v.status === 'running';
  els.jobKeep.textContent = !active && v.expires_at ? t('{finished} 完成，约 {expires} 后清理', { finished: v.finished_at, expires: v.expires_at }) : '';
  els.jobKeep.hidden = !els.jobKeep.textContent;
}

function renderJob(v) {
  job = v;
  els.bar.hidden = false;
  els.jobCard.hidden = false;
  els.jobMeta.textContent = jobMeta(v);
  renderKeep(v);
  const pct = v.total ? Math.round((v.done / v.total) * 100) : 0;
  els.fill.style.width = pct + '%';
  if (v.notice) {
    els.notice.replaceChildren(Object.assign(el('div', 'alert info so-note', v.notice)));
  } else {
    els.notice.replaceChildren();
  }
  v.items.forEach((it, i) => {
    if (!rowsEls[i]) {
      els.rows.append(buildRow({ sql_id: it.sql_id, sql: it.sql, stats: it.stats, type: it.type }, i).tr);
    }
    setRow(rowsEls[i], it, i);
  });
  const active = v.status === 'queued' || v.status === 'running';
  if (active) els.status.textContent = t('已解析 {total} 条 SQL · 分析进度 {done} / {total}', { total: v.total, done: v.done });
  else if (v.status === 'done') els.status.textContent = t('分析完成：{n} 条', { n: v.total }) + (v.failed ? t('，其中 {n} 条失败', { n: v.failed }) : '');
  else els.status.textContent = v.status === 'canceled' ? t('分析已取消') : t('分析失败：{reason}', { reason: v.error || '' });
  if (v.error && v.status === 'failed') setMsg(els.msg, v.error);
  if (v.summary) renderSummary(v.summary);
  else els.summary.hidden = true;
  markRecent();
}

function chip(label, n) {
  const c = el('span', 'sa-chip', label);
  c.append(el('b', '', String(n)));
  return c;
}

function renderSummary(s) {
  els.summary.hidden = false;
  const nums = $('sa-sum-nums');
  nums.replaceChildren();
  const box = (n, label) => { const d = el('div'); d.append(el('b', '', String(n)), el('span', '', label)); nums.append(d); };
  box(s.total, t('SQL 总数'));
  box(s.unique, t('不同指纹'));
  box(s.scored ? s.avg_score : '—', t('平均评分'));
  const by = (k) => s.by_level[k] || 0;
  box(by('high'), t('高级别（最高建议）语句'));
  box(by('medium'), t('中级别语句'));
  box(by('low'), t('低级别语句'));
  box(by('none'), t('没有建议的语句'));
  const types = $('sa-sum-types');
  types.replaceChildren();
  Object.entries(s.by_type || {}).sort((a, b) => b[1] - a[1]).forEach(([k, n]) => types.append(chip((k || '?').toUpperCase(), n)));
  const fp = $('sa-sum-fp');
  fp.replaceChildren();
  document.querySelectorAll('.sa-th-stat').forEach((th) => { th.hidden = !s.has_stats; });
  for (const f of s.top_fingerprints) {
    const tr = el('tr');
    tr.append(el('td', 'sa-fpid', f.fp_id || ''), el('td', 'wrap mono so-sql', f.fingerprint), el('td', '', f.type), el('td', 'num', String(f.count)));
    const lv = el('td');
    if (f.level) lv.append(levelBadge(f.level));
    tr.append(lv);
    if (s.has_stats) tr.append(el('td', 'num', (f.total_time || 0).toFixed(2)), el('td', 'num', num(f.rows_examined || 0)));
    fp.append(tr);
  }
  const tb = $('sa-sum-tables');
  tb.replaceChildren();
  if (!s.tables.length) {
    const tr = el('tr'); const td = el('td', 'empty', t('没有识别到表')); td.colSpan = 2; tr.append(td); tb.append(tr);
  }
  for (const tb2 of s.tables) {
    const tr = el('tr');
    tr.append(el('td', 'mono', tb2.name), el('td', 'num', String(tb2.count)));
    tb.append(tr);
  }
}

// ---- 报告弹窗 ----

function closeModal() { els.modal.hidden = true; }
$('sa-modal-close').addEventListener('click', closeModal);
els.modal.addEventListener('click', (e) => { if (e.target === els.modal) closeModal(); });
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeModal(); });

async function openReport(idx) {
  try {
    const d = await api('GET', `${API}/jobs/${job.id}/items/${idx}`);
    $('sa-modal-title').textContent = t('分析报告 · SQLID {id}', { id: d.sql_id });
    $('sa-modal-sql').textContent = d.full_sql;
    const body = $('sa-modal-body');
    body.replaceChildren();
    const dl = el('dl', 'so-kv');
    if (d.fp_id) dl.append(el('dt', '', t('指纹 ID')), el('dd', 'mono', d.fp_id));
    if (d.type) dl.append(el('dt', '', t('语句类型')), el('dd', '', d.type.toUpperCase()));
    if (d.tables && d.tables.length) dl.append(el('dt', '', t('涉及的表')), el('dd', 'mono', d.tables.join(', ')));
    if (d.fingerprint) dl.append(el('dt', '', t('指纹')), el('dd', 'mono', d.fingerprint));
    if (d.stats) dl.append(el('dt', '', t('日志统计')), el('dd', '', statsText(d.stats)));
    if (dl.childNodes.length) body.append(dl);
    if (d.report) renderReport(body, d.report);
    else body.append(el('div', 'alert err', d.error || t('没有报告')));
    els.modal.hidden = false;
  } catch (e) {
    setMsg(els.msg, e.message);
  }
}

// ---- 最近报告 ----

let recentList = [];
let recentAt = 0;

function remainOf(b) {
  if (b.remain_seconds < 0) return -1;
  return Math.max(0, b.remain_seconds - Math.floor((Date.now() - recentAt) / 1000));
}

function renderRecent() {
  els.recent.replaceChildren();
  if (!recentList.length) {
    const tr = el('tr');
    const td = el('td', 'empty', t('还没有保留中的分析任务'));
    td.colSpan = 8;
    tr.append(td);
    els.recent.append(tr);
    return;
  }
  for (const b of recentList) {
    const tr = el('tr');
    tr.dataset.jobId = b.id;
    if (job && job.id === b.id) tr.className = 'sa-recent-cur';
    const jid = el('td');
    jid.append(el('span', 'mono', b.id.slice(0, 8)));
    if (!b.mine) jid.append(document.createTextNode(' '), el('span', 'badge', t('他人')));
    const file = el('td', 'wrap', b.file || b.title);
    if (b.format) file.append(document.createTextNode(' '), el('span', 'badge', FORMAT_NAME[b.format] || b.format));
    const where = el('td', '', b.instance ? `${b.instance} / ${b.db || t('全部库')}` : t('静态分析'));
    const prog = el('td', 'num', `${b.done} / ${b.total}`);
    const [cls, label] = ST[b.status] || ['aborted', b.status];
    const st = el('td');
    st.append(el('span', 'st ' + cls, label));
    const rs = remainOf(b);
    const keep = el('td', rs >= 0 && rs < 300 ? 'sa-keep-warn' : '', rs < 0 ? '—' : t('{time}后清理', { time: fmtMin(rs) }));
    keep.dataset.remain = String(rs);
    const act = el('td', 'row-actions');
    const view = el('button', 'btn', t('查看'));
    view.type = 'button';
    view.addEventListener('click', () => openJob(b.id));
    act.append(view);
    tr.append(jid, file, where, el('td', '', b.owner), prog, st, keep, act);
    els.recent.append(tr);
  }
}

function markRecent() {
  els.recent.querySelectorAll('tr[data-job-id]').forEach((tr) => {
    tr.classList.toggle('sa-recent-cur', !!job && tr.dataset.jobId === job.id);
  });
}

async function loadRecent() {
  try {
    const r = await api('GET', `${API}/jobs`);
    recentList = r.jobs || [];
    recentAt = Date.now();
    renderRecent();
  } catch (e) {
    els.recent.replaceChildren();
    const tr = el('tr');
    const td = el('td', 'empty', t('最近报告加载失败：{msg}', { msg: e.message }));
    td.colSpan = 8;
    tr.append(td);
    els.recent.append(tr);
  }
}

// 倒计时：每 15 秒按本地时间重画；每分钟向服务端同步一次（已被清理的任务从列表消失）。
let tickN = 0;
setInterval(() => {
  if (document.hidden) return;
  tickN++;
  if (tickN % 4 === 0) loadRecent();
  else if (recentList.length) renderRecent();
}, 15000);
$('sa-recent-refresh').addEventListener('click', loadRecent);

async function openJob(id) {
  setMsg(els.msg, '');
  if (poller) { poller.stop(); poller = null; }
  setBusy(false);
  try {
    const v = await api('GET', `${API}/jobs/${id}`);
    mem.set(id);
    rowsEls = [];
    els.rows.replaceChildren();
    renderJob(v);
    if (v.status === 'queued' || v.status === 'running') follow(id);
    window.scrollTo({ top: 0, behavior: 'smooth' });
  } catch (e) {
    setMsg(els.msg, e.status === 404 ? t('这个任务已过期被清理（或服务已重启），请重新分析') : e.message);
    loadRecent();
  }
}

// ---- 流程 ----

async function init() {
  try {
    const o = await loadOptions();
    for (const i of o.instances.filter((x) => x.tools)) els.inst.append(option(i.id, t('{name}（{label}）', { name: i.name, label: i.db_type })));
  } catch (e) {
    setMsg(els.msg, e.message);
  }
  if (!canAnalyze) setMsg(els.msg, t('没有 sqlanalyze.run 权限，只能查看最近的分析报告。'), 'warn');
  loadRecent();
  const saved = mem.get();
  if (saved) resume(saved);
}

async function resume(id) {
  try {
    const v = await api('GET', `${API}/jobs/${id}`);
    showRows([]);
    els.rows.replaceChildren();
    renderJob(v);
    if (v.status === 'queued' || v.status === 'running') follow(id);
  } catch (_) {
    mem.del();
  }
}

els.inst.addEventListener('change', async () => {
  els.db.replaceChildren(option('', t('不指定')));
  els.db.disabled = !els.inst.value;
  if (!els.inst.value) return;
  try {
    for (const d of await loadDatabases(els.inst.value)) els.db.append(option(d, d));
  } catch (e) {
    setMsg(els.msg, e.message);
  }
});

els.file.addEventListener('change', async () => {
  setMsg(els.msg, '');
  els.notice.replaceChildren();
  els.summary.hidden = true;
  els.bar.hidden = true;
  els.jobCard.hidden = true;
  els.status.textContent = '';
  els.start.disabled = true;
  job = null;
  markRecent();
  const f = els.file.files[0];
  fileName = f ? f.name : '';
  if (!f) { showRows([]); return; }
  if (f.size > maxBytes) {
    els.file.value = '';
    setMsg(els.msg, t('文件超过 {n}MB，请拆分后再上传', { n: root.dataset.maxMb }));
    return;
  }
  try {
    text = await f.text();
    const r = await api('POST', `${API}/parse`, { text });
    showRows(r.rows);
    const fmt = { mapper: 'MyBatis Mapper XML', slowlog: t('MySQL 慢日志'), sql: t('SQL 文本') }[r.format] || r.format;
    els.status.textContent = t('已解析 {n} 条 SQL（{fmt}）', { n: r.rows.length, fmt });
    els.start.disabled = !canAnalyze || !r.rows.some((x) => !x.error);
  } catch (e) {
    showRows([]);
    setMsg(els.msg, e.message);
  }
});

function follow(id) {
  setBusy(true);
  poller = pollJob(id, renderJob);
  poller.promise.then((v) => {
    setBusy(false);
    renderJob(v);
    loadRecent();
  }).catch((e) => {
    setBusy(false);
    mem.del();
    setMsg(els.msg, e.status === 404 ? t('任务已过期或服务已重启，请重新开始分析') : e.message);
    loadRecent();
  });
}

els.start.addEventListener('click', async () => {
  setMsg(els.msg, '');
  els.summary.hidden = true;
  els.notice.replaceChildren();
  try {
    const body = { text, file_name: fileName, instance_id: els.inst.value ? Number(els.inst.value) : 0, db: els.db.value };
    const v = await api('POST', `${API}/analyze`, body);
    mem.set(v.id);
    rowsEls = [];
    els.rows.replaceChildren();
    renderJob(v);
    follow(v.id);
    loadRecent();
  } catch (e) {
    setMsg(els.msg, e.message);
  }
});

els.cancel.addEventListener('click', async () => {
  if (!job) return;
  try {
    await api('POST', `${API}/jobs/${job.id}/cancel`);
    els.status.textContent = t('正在取消…');
  } catch (e) {
    setMsg(els.msg, e.message);
  }
});

init();
