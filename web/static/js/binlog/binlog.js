/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: binlog.js
 * @time: 2026/10/28
 */
// Binlog 解析 / 回滚：选实例 → 选 binlog 范围与过滤条件 → 后台解析（轮询进度）→ 分页查看 / 复制 / 下载。
// 所有来自服务端或数据库的字符串都用 textContent 写入，不拼 innerHTML。
import { api } from '../common.js';
import { applyButtons } from '../btnperm.js';
import { el, btn, setMsg, confirmBox, fmtNum, fmtBytes } from '../instmgr/ui.js';
import { t, tp } from '../i18n.js';

const API = '/api/v1/binlog';
const $ = (id) => document.getElementById(id);
const root = $('bl');
const JOB_KEY = 'binlog_job';
const INST_KEY = 'binlog_instance';

// sessionStorage / localStorage 在隐私模式等场景可能不可用，读写都不能影响页面功能
const store = {
  get(s, k) { try { return s.getItem(k); } catch (_) { return null; } },
  set(s, k, v) { try { s.setItem(k, v); } catch (_) { /* 忽略 */ } },
  del(s, k) { try { s.removeItem(k); } catch (_) { /* 忽略 */ } },
};

const els = {
  msg: $('bl-msg'), inst: $('bl-inst'), info: $('bl-info'), modeRb: $('bl-mode-rb'), modeNote: $('bl-mode-note'),
  sfile: $('bl-sfile'), spos: $('bl-spos'), efile: $('bl-efile'), epos: $('bl-epos'), stime: $('bl-stime'), etime: $('bl-etime'),
  dbs: $('bl-dbs'), tables: $('bl-tables'), noprefix: $('bl-noprefix'), extra: $('bl-extra'), full: $('bl-full'),
  maxRows: $('bl-max-rows'), maxMb: $('bl-max-mb'), timeout: $('bl-timeout'), start: $('bl-start'), cancel: $('bl-cancel'),
  jobCard: $('bl-job-card'), jobMeta: $('bl-job-meta'), progress: $('bl-progress'), kpis: $('bl-kpis'), notes: $('bl-job-notes'),
  resCard: $('bl-result-card'), resMeta: $('bl-result-meta'), rows: $('bl-rows'), more: $('bl-more'), pagerInfo: $('bl-pager-info'),
  copyAll: $('bl-copy-all'), download: $('bl-download'), copyState: $('bl-copy-state'), jobs: $('bl-jobs'),
};

const S = {
  opts: null,
  info: null,
  job: null, // 当前查看的任务视图
  poller: null,
  rows: [], // 已加载的结果行
  loadingRows: false,
  nextSeg: null, // 点“继续下一段”填进表单的续接位置：{ file, pos, index }；表单里的起始位置没被改过时，下一个任务带上这个段号
};

const ST = {
  queued: ['waiting', t('排队')], running: ['running', t('解析中')], done: ['passed', t('已完成')],
  failed: ['rejected', t('失败')], canceled: ['aborted', t('已取消')],
};
const MODE_LABEL = { sql: t('还原 SQL'), rollback: t('回滚 SQL') };

const mode = () => (document.querySelector('input[name="bl-mode"]:checked') || {}).value || 'sql';
const instId = () => Number(els.inst.value) || 0;
const toInt = (s) => {
  s = String(s || '').trim();
  if (!s) return 0;
  if (!/^\d+$/.test(s)) return NaN;
  return Number(s);
};

function selectedValues(sel) {
  return [...sel.selectedOptions].map((o) => o.value);
}

function fmtRemain(sec) {
  if (sec <= 0) return t('即将清理');
  if (sec < 60) return t('不到 1 分钟');
  return t('{n} 分钟', { n: Math.floor(sec / 60) });
}

// ---------- 实例与 binlog 信息 ----------

async function loadOptions() {
  S.opts = await api('GET', `${API}/options`);
  const lim = S.opts.limits;
  $('bl-hard-rows').textContent = fmtNum(lim.hard_rows);
  $('bl-hard-mb').textContent = fmtNum(lim.hard_mb);
  $('bl-max-timeout').textContent = lim.max_timeout;
  els.maxRows.value = lim.default_rows;
  els.maxMb.value = lim.default_mb;
  els.timeout.value = lim.default_timeout;

  els.inst.replaceChildren();
  if (!S.opts.instances.length) {
    els.inst.append(new Option(t('（没有可用的 MySQL 实例）'), ''));
    return;
  }
  els.inst.append(new Option(t('请选择实例'), ''));
  for (const i of S.opts.instances) {
    const base = i.role ? t('{name}（{role}）', { name: i.name, role: i.role }) : i.name;
    const o = new Option(base + (i.unsupported ? ' · ' + tp(i.unsupported) : ''), String(i.id));
    o.disabled = !!i.unsupported;
    els.inst.append(o);
  }
  const want = new URLSearchParams(location.search).get('inst') || store.get(localStorage, INST_KEY);
  if (want && S.opts.instances.some((i) => String(i.id) === want && !i.unsupported)) els.inst.value = want;
}

function resetFileSelects() {
  els.sfile.replaceChildren();
  els.efile.replaceChildren(new Option(t('最后一个文件末尾'), ''));
  els.dbs.replaceChildren();
  els.epos.value = '';
  els.epos.disabled = true;
}

function renderInfo(info) {
  els.info.replaceChildren();
  const chips = el('div', 'chips');
  chips.append(
    el('span', 'chip', info.version || t('版本未知')),
    el('span', 'chip' + (info.binlog_format === 'ROW' ? ' ok' : ' warn'), `binlog_format=${info.binlog_format || '?'}`),
    el('span', 'chip' + (info.binlog_row_image === 'FULL' ? ' ok' : ' warn'), `binlog_row_image=${info.binlog_row_image || '?'}`),
    el('span', 'chip', t('{n} 个 binlog 文件', { n: info.files.length })),
  );
  els.info.append(chips);
  const ok = el('div', 'small ' + (info.can_rollback ? 'bl-ok' : 'bl-warn'),
    info.can_rollback ? t('可以生成回滚 SQL（ROW + FULL）') : t('不能生成回滚 SQL：{reason}', { reason: info.rollback_reason }));
  els.info.append(ok);
}

async function loadInstance() {
  stopPolling();
  S.info = null;
  resetFileSelects();
  els.info.replaceChildren(el('span', 'muted', t('请选择实例')));
  els.start.disabled = true;
  setMsg(els.msg, '');
  const id = instId();
  if (!id) return;
  store.set(localStorage, INST_KEY, String(id));
  els.info.replaceChildren(el('span', 'muted', t('正在读取 binlog 列表…')));
  try {
    const [info, dbs] = await Promise.all([api('GET', `${API}/instances/${id}/info`), api('GET', `${API}/instances/${id}/databases`).catch(() => [])]);
    if (id !== instId()) return; // 期间切换了实例
    S.info = info;
    renderInfo(info);
    if (!info.files.length) {
      setMsg(els.msg, info.binlog_format ? t('该实例没有 binlog 文件（可能未开启 binlog）') : t('读取不到 binlog 信息'), 'warn');
    }
    const last = info.files.length - 1;
    info.files.forEach((f, i) => {
      const label = t('{name}（{size}）', { name: f.name, size: fmtBytes(f.size) });
      els.sfile.append(new Option(label, f.name));
      els.efile.append(new Option(label, f.name));
      if (i === last) els.sfile.value = f.name; // 默认从最后一个文件开始：最常见的是找刚刚发生的误操作
    });
    for (const d of dbs) els.dbs.append(new Option(d, d));
    syncMode();
  } catch (e) {
    if (id !== instId()) return;
    els.info.replaceChildren(el('span', 'muted', t('读取失败')));
    setMsg(els.msg, e.message);
  }
}

// 能否开始：有文件可读，且当前模式有对应权限（只有回滚权限的人只能用回滚模式）。
function refreshStart() {
  const perms = S.opts.perms;
  const canRb = perms.rollback && S.info && S.info.can_rollback;
  const ok = S.info && S.info.files.length && (mode() === 'rollback' ? canRb : perms.parse);
  els.start.disabled = !ok;
}

function syncMode() {
  const perms = S.opts.perms;
  const canRb = perms.rollback && S.info && S.info.can_rollback;
  els.modeRb.disabled = !canRb;
  const sqlRadio = document.querySelector('input[name="bl-mode"][value="sql"]');
  sqlRadio.disabled = !perms.parse;
  if (!perms.parse && canRb) els.modeRb.checked = true;
  els.modeNote.textContent = perms.rollback ? '' : t('没有生成回滚 SQL 的权限');
  if (!canRb && mode() === 'rollback') document.querySelector('input[name="bl-mode"][value="sql"]').checked = true;
  els.start.textContent = mode() === 'rollback' ? t('开始生成回滚 SQL') : t('开始解析');
  refreshStart();
}

// ---------- 发起任务 ----------

function collect() {
  const spos = toInt(els.spos.value);
  const epos = toInt(els.epos.value);
  const rows = toInt(els.maxRows.value);
  const mb = toInt(els.maxMb.value);
  const tmo = toInt(els.timeout.value);
  if ([spos, epos, rows, mb, tmo].some(Number.isNaN)) throw new Error(t('位置、最多条数、体积上限、超时必须是数字'));
  return {
    instance_id: instId(),
    mode: mode(),
    start_file: els.sfile.value,
    start_pos: spos,
    end_file: els.efile.value,
    end_pos: els.efile.value ? epos : 0,
    start_time: els.stime.value.trim(),
    stop_time: els.etime.value.trim(),
    databases: selectedValues(els.dbs),
    tables: els.tables.value.split(/[\s,，;]+/).filter(Boolean),
    sql_types: [...document.querySelectorAll('input[name="bl-type"]:checked')].map((c) => c.value),
    max_rows: rows,
    max_mb: mb,
    timeout_sec: tmo,
    no_db_prefix: els.noprefix.checked,
    extra_info: els.extra.checked,
    full_columns: els.full.checked,
    segment: segmentNo(),
  };
}

// 回滚分段的段号：起始位置还是“继续下一段”填进去的那个，就是它的下一段；改过起始位置（或换了模式）就从第 1 段重新算。
function segmentNo() {
  const n = S.nextSeg;
  return n && mode() === 'rollback' && els.sfile.value === n.file && toInt(els.spos.value) === n.pos ? n.index : 1;
}

// 把上一段的续接位置填进起始位置：结束位置、时间和过滤条件都保留，确认后点“开始生成回滚 SQL”继续。
function continueSegment(job) {
  const sg = job.segment;
  if (!sg || !sg.more) return;
  if (![...els.sfile.options].some((o) => o.value === sg.next_file)) {
    setMsg(els.msg, t('续接位置所在的 binlog 文件 {file} 已不在实例的文件列表里（可能已被清理），无法继续', { file: sg.next_file }));
    return;
  }
  els.sfile.value = sg.next_file;
  els.spos.value = String(sg.next_pos);
  S.nextSeg = { file: sg.next_file, pos: sg.next_pos, index: sg.index + 1 };
  setMsg(els.msg, t('已把续接位置 {pos} 填进起始位置，确认其它条件不变后点“{start}”，生成第 {n} 段。', { pos: `${sg.next_file}:${sg.next_pos}`, start: els.start.textContent, n: sg.index + 1 }), 'info');
  els.start.scrollIntoView({ block: 'center', behavior: 'smooth' });
  els.start.focus();
}

async function start() {
  setMsg(els.msg, '');
  let body;
  try { body = collect(); } catch (e) { setMsg(els.msg, e.message); return; }
  if (!body.instance_id) { setMsg(els.msg, t('请选择实例')); return; }
  if (body.mode === 'rollback') {
    const ok = await confirmBox({
      title: t('生成回滚 SQL'),
      lines: [
        t('平台只会生成回滚语句，不会在目标库上执行任何语句；是否执行、何时执行由你决定。'),
        t('回滚 SQL 按“后发生的先撤销”的顺序给出，执行前务必在测试环境验证；范围内有 DDL 时无法回滚。'),
        t('这次操作会写入系统审计，结果里可能含有业务敏感数据。'),
      ],
      okText: t('确认生成'), danger: true,
    });
    if (!ok) return;
  }
  els.start.disabled = true;
  try {
    const job = await api('POST', `${API}/parse`, body);
    showJob(job);
    watch(job.id);
    loadJobs();
  } catch (e) {
    setMsg(els.msg, e.message);
  } finally {
    refreshStart();
  }
}

// ---------- 任务进度 ----------

function stopPolling() {
  if (S.poller) { clearTimeout(S.poller); S.poller = null; }
}

function active(job) { return job && (job.status === 'queued' || job.status === 'running'); }

function kpi(label, value, cls) {
  const d = el('div');
  d.append(el('b', cls || '', String(value)), el('span', '', label));
  return d;
}

function showJob(job) {
  const prev = S.job;
  S.job = job;
  els.jobCard.hidden = false;
  const [cls, label] = ST[job.status] || ['aborted', job.status];
  els.jobMeta.replaceChildren(el('span', 'st ' + cls, label), document.createTextNode(` ${job.instance} · ${MODE_LABEL[job.mode] || job.mode} · ${job.range}`));
  els.progress.hidden = !active(job);
  els.cancel.hidden = !active(job);
  els.kpis.replaceChildren(
    kpi(t('已扫描事件'), fmtNum(job.scanned)),
    kpi(t('当前位置'), `${job.file || '—'}:${job.pos || 0}`),
    kpi(t('已匹配语句'), fmtNum(job.matched)),
    kpi(t('结果体积'), fmtBytes(job.bytes)),
    kpi(t('事务数'), fmtNum(job.trx || 0)),
  );
  const types = Object.entries(job.by_type || {}).map(([k, n]) => `${k} ${fmtNum(n)}`).join(' · ');
  els.notes.replaceChildren();
  if (types) els.notes.append(el('div', 'muted small', t('类型分布：{types}', { types })));
  if (job.status === 'failed') els.notes.append(el('div', 'alert err', job.error || t('任务失败')));
  if (job.status === 'canceled') els.notes.append(el('div', 'alert warn', t('任务已取消')));
  if (job.truncated) els.notes.append(el('div', 'alert warn', job.truncated));
  const sg = job.segment;
  if (sg && job.status === 'done') {
    // 回滚分段：本段在事务边界结束，回滚 SQL 是完整可用的；完整回滚要按段号从大到小执行。
    // 后面还有时，原因、续接位置和执行顺序已经在上面的提示（job.truncated）里写了，这里只放段号和按钮，不重复。
    const box = el('div', 'alert info bl-seg');
    box.append(el('b', '', t('第 {n} 段', { n: sg.index })), document.createTextNode(' '));
    if (sg.more) box.append(btn(t('继续下一段'), 'primary', () => continueSegment(job)));
    else box.append(document.createTextNode(t('这是最后一段。完整回滚要按段号从大到小执行：先执行最后一段，最后执行第 1 段。')));
    els.notes.append(box);
  }
  for (const w of job.warnings || []) els.notes.append(el('div', 'alert warn', w));
  if (job.status === 'done') {
    els.notes.append(el('div', 'muted small', t('结果保留到 {at}', { at: job.expires_at })));
  }
  if (!prev || prev.id !== job.id) {
    S.rows = [];
    els.resCard.hidden = true;
    els.rows.replaceChildren();
  }
  const finished = job.status === 'done';
  els.resCard.hidden = !finished && S.rows.length === 0;
  els.download.disabled = !job.can_download;
  els.download.title = job.can_download ? '' : t('没有下载权限，或任务没有结果');
  els.copyAll.disabled = !finished;
  if (finished && (!prev || prev.id !== job.id || prev.status !== 'done')) loadRows(true);
  if (job.status !== 'queued' && job.status !== 'running') store.del(sessionStorage, JOB_KEY);
  else store.set(sessionStorage, JOB_KEY, job.id);
}

function watch(id) {
  stopPolling();
  const tick = async () => {
    try {
      const v = await api('GET', `${API}/jobs/${id}`);
      if (S.job && S.job.id !== id) return; // 用户切到了别的任务
      showJob(v);
      if (active(v)) S.poller = setTimeout(tick, 1000);
      else loadJobs();
    } catch (e) {
      if (e.status === 404 || e.status === 403) {
        store.del(sessionStorage, JOB_KEY);
        els.progress.hidden = true;
        els.cancel.hidden = true;
        setMsg(els.msg, t('任务不存在或已过期，请重新解析'), 'warn');
      } else {
        S.poller = setTimeout(tick, 3000);
      }
    }
  };
  tick();
}

async function cancelJob(id) {
  try {
    await api('POST', `${API}/jobs/${id}/cancel`);
  } catch (e) {
    setMsg(els.msg, e.message);
  }
  if (S.job && S.job.id === id) watch(id);
  loadJobs();
}

// ---------- 结果行 ----------

function typeBadge(r) {
  const cls = r.type === 'DELETE' ? 'badge crit' : r.type === 'UPDATE' ? 'badge warn' : r.type === 'INSERT' ? 'badge ok' : 'badge';
  const b = el('span', cls, r.type);
  return b;
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (_) {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.append(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch (_e) { ok = false; }
    ta.remove();
    return ok;
  }
}

function flash(text, ok = true) {
  els.copyState.textContent = text;
  els.copyState.className = 'small ' + (ok ? 'bl-ok' : 'bl-warn');
  setTimeout(() => { if (els.copyState.textContent === text) els.copyState.textContent = ''; }, 3000);
}

function appendRows(list) {
  if (!list.length) return;
  for (const r of list) {
    const tr = el('tr');
    tr.append(el('td', 'nw', String(r.seq)), el('td', 'nw', r.trx ? String(r.trx) : '—'), el('td', 'nw', r.time),
      el('td', 'nw mono', `${r.file}:${r.pos}`), el('td', 'nw', r.table ? `${r.schema}.${r.table}` : r.schema || '—'));
    const tt = el('td', 'nw');
    tt.append(typeBadge(r));
    if (r.no_rollback) tt.append(document.createTextNode(' '), el('span', 'badge warn', t('不可回滚')));
    tr.append(tt);
    tr.append(el('td', 'mono wrap bl-sql', r.sql));
    const act = el('td', 'nw');
    act.append(btn(t('复制'), '', async () => flash((await copyText(r.sql)) ? t('已复制第 {n} 条', { n: r.seq }) : t('复制失败，请手动选中复制'), true)));
    tr.append(act);
    els.rows.append(tr);
  }
}

async function loadRows(first) {
  if (!S.job || S.loadingRows) return;
  S.loadingRows = true;
  const id = S.job.id;
  try {
    if (first) { S.rows = []; els.rows.replaceChildren(); }
    const page = await api('GET', `${API}/jobs/${id}/rows?offset=${S.rows.length}&limit=${S.opts.limits.page_rows}`);
    if (!S.job || S.job.id !== id) return;
    S.rows.push(...page.rows);
    if (first && !page.rows.length) {
      const tr = el('tr');
      const td = el('td', 'empty', t('范围内没有符合条件的语句'));
      td.colSpan = 8;
      tr.append(td);
      els.rows.append(tr);
    }
    appendRows(page.rows);
    els.resCard.hidden = false;
    els.resMeta.textContent = t('{mode} · 共 {n} 条', { mode: MODE_LABEL[S.job.mode] || '', n: fmtNum(page.total) });
    els.pagerInfo.textContent = page.total ? t('已显示 {shown} / {total}', { shown: fmtNum(S.rows.length), total: fmtNum(page.total) }) : '';
    els.more.hidden = S.rows.length >= page.total;
  } catch (e) {
    setMsg(els.msg, e.message);
  } finally {
    S.loadingRows = false;
  }
}

async function copyAll() {
  if (!S.job) return;
  els.copyAll.disabled = true;
  try {
    // 已加载的不够就把剩下的页取完
    for (;;) {
      const page = await api('GET', `${API}/jobs/${S.job.id}/rows?offset=${S.rows.length}&limit=${S.opts.limits.page_rows}`);
      if (!page.rows.length) break;
      S.rows.push(...page.rows);
      appendRows(page.rows);
      if (S.rows.length >= page.total) break;
    }
    els.more.hidden = true;
    const ok = await copyText(S.rows.map((r) => r.sql).join('\n'));
    flash(ok ? t('已复制全部 {n} 条', { n: fmtNum(S.rows.length) }) : t('复制失败（浏览器拒绝访问剪贴板或内容过大），请改用下载'), ok);
  } catch (e) {
    setMsg(els.msg, e.message);
  } finally {
    els.copyAll.disabled = false;
  }
}

async function download() {
  if (!S.job || !S.job.can_download) return;
  const ok = await confirmBox({
    title: t('下载结果文件'),
    lines: [t('文件里是解析出的完整 SQL，可能含有业务敏感数据；每次下载都会写入系统审计。'), t('请妥善保管，用后删除。')],
    okText: t('确认下载'), danger: true,
  });
  if (!ok) return;
  els.download.disabled = true;
  try {
    // 结果可能有几百 MB 到几 GB：让浏览器直接下载（边收边写盘），不用 fetch + blob 把整个文件装进页面内存。
    // 直接下载出错时浏览器只会显示“下载失败”，所以先确认任务还在、有结果、有下载权限，那样的错误能在页面上说清楚。
    const v = await api('GET', `${API}/jobs/${S.job.id}`);
    if (v.status !== 'done' || !v.can_download) throw new Error(t('没有下载权限，或任务没有结果'));
    const a = document.createElement('a');
    a.href = `${API}/jobs/${S.job.id}/download`;
    a.download = '';
    document.body.append(a);
    a.click();
    a.remove();
  } catch (e) {
    setMsg(els.msg, e.message);
  } finally {
    els.download.disabled = !(S.job && S.job.can_download);
  }
}

// ---------- 任务列表 ----------

async function loadJobs() {
  let list;
  try {
    list = await api('GET', `${API}/jobs`);
  } catch (e) {
    els.jobs.replaceChildren();
    const tr = el('tr');
    const td = el('td', 'empty', e.status === 403 ? t('没有查看任务的权限') : e.message);
    td.colSpan = 9;
    tr.append(td);
    els.jobs.append(tr);
    return;
  }
  els.jobs.replaceChildren();
  if (!list.length) {
    const tr = el('tr');
    const td = el('td', 'empty', t('还没有任务'));
    td.colSpan = 9;
    tr.append(td);
    els.jobs.append(tr);
    return;
  }
  for (const j of list) {
    const tr = el('tr');
    const [cls, label] = ST[j.status] || ['aborted', j.status];
    const st = el('td');
    st.append(el('span', 'st ' + cls, label));
    if (j.error) st.append(el('div', 'muted small', j.error));
    const remain = j.remain_seconds >= 0 ? fmtRemain(j.remain_seconds) : '—';
    tr.append(el('td', 'nw', j.created_at), el('td', 'nw', j.mine ? t('{name}（我）', { name: j.owner }) : j.owner), el('td', 'nw', j.instance),
      el('td', 'nw', MODE_LABEL[j.mode] || j.mode), el('td', 'wrap bl-range', j.range), st,
      el('td', 'nw', j.status === 'done' ? (j.truncated ? t('{n} 条（已截断）', { n: fmtNum(j.matched) }) : t('{n} 条', { n: fmtNum(j.matched) })) : j.status === 'running' ? t('已扫描 {n}', { n: fmtNum(j.scanned) }) : '—'),
      el('td', 'nw', remain));
    const act = el('td', 'nw');
    act.append(btn(t('查看'), '', () => { showJob(j); if (active(j)) watch(j.id); else if (j.status === 'done') loadRows(true); window.scrollTo({ top: 0, behavior: 'smooth' }); }));
    if (active(j)) act.append(document.createTextNode(' '), btn(t('取消'), 'danger', () => cancelJob(j.id)));
    tr.append(act);
    els.jobs.append(tr);
  }
}

// ---------- 事件 ----------

els.inst.addEventListener('change', loadInstance);
els.efile.addEventListener('change', () => {
  els.epos.disabled = !els.efile.value;
  if (!els.efile.value) els.epos.value = '';
});
for (const r of document.querySelectorAll('input[name="bl-mode"]')) r.addEventListener('change', syncMode);
els.start.addEventListener('click', start);
els.cancel.addEventListener('click', () => S.job && cancelJob(S.job.id));
els.more.addEventListener('click', () => loadRows(false));
els.copyAll.addEventListener('click', copyAll);
els.download.addEventListener('click', download);
$('bl-jobs-refresh').addEventListener('click', loadJobs);

(async function init() {
  try {
    await loadOptions();
    await applyButtons(root);
    await loadInstance();
  } catch (e) {
    setMsg(els.msg, e.message);
  }
  loadJobs();
  const saved = store.get(sessionStorage, JOB_KEY);
  if (saved) {
    try {
      const v = await api('GET', `${API}/jobs/${saved}`);
      showJob(v);
      if (active(v)) watch(saved);
    } catch (_) {
      store.del(sessionStorage, JOB_KEY);
    }
  }
})();
