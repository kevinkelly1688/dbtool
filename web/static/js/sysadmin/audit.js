/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: audit.js
 * @time: 2026/10/28
 */
// 系统审计：通用审计（dbt_audit_log + 扩展信息，可导出 CSV）、SQL 上线审计、查询审计（含被拒绝的语句）。需要 audit.view 权限（或超级用户）。
import { api, $, el, msgBox, emptyRow, tabs, qs, pager, fillSelect, status, modal, confirmRun, toast } from './ui.js';
import { loadButtons, can } from '../btnperm.js';
import { t, tp } from '../i18n.js';

const setMsg = msgBox($('au-msg'));
const API = '/api/v1/audit';
const loaded = { wf: false, ql: false };
let opts = { actions: [], sql_statuses: [], resource_groups: [], categories: [], results: [], ext_ready: false, reject_ready: false };
const detail = modal('au-detail');
const EXPORT_MAX = 50000; // 与服务端一致：导出最多 5 万行
let lastTotal = null; // 当前筛选条件下的记录总数（来自最近一次列表）

const dateFrom = (days) => {
  const d = new Date(Date.now() - days * 86400000);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
};

const dash = (v) => (v === undefined || v === null || v === '' ? '—' : v);
const RESULT_CLS = { 成功: 'passed', 失败: 'rejected', 拒绝: 'waiting' };

function resultTag(r) {
  return r ? status(tp(r), RESULT_CLS[r] || 'aborted') : el('span', 'muted', '—');
}

function kpi(box, items) {
  box.replaceChildren();
  for (const [label, value, sub, cls] of items) {
    const d = el('div');
    const b = el('b', cls || '', value);
    d.append(el('span', '', label), b);
    if (sub) d.append(el('small', '', sub));
    box.append(d);
  }
  box.hidden = false;
}

const SVGNS = 'http://www.w3.org/2000/svg';
function svg(tag, attrs, text) {
  const e = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, String(v));
  if (text !== undefined) e.textContent = text;
  return e;
}

// ---- 概览：KPI、近 14 天柱图、最活跃用户 ----

function drawBars(box, days) {
  const W = 560, H = 150, top = 16, base = 128;
  const max = Math.max(1, ...days.map((d) => d.count));
  const slot = W / days.length, bw = Math.min(30, slot * 0.62);
  const s = svg('svg', { class: 'au-bars', viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': t('近 14 天审计事件按天条数') });
  days.forEach((d, i) => {
    const h = Math.round((d.count / max) * (base - top));
    const x = i * slot + (slot - bw) / 2;
    const bar = svg('rect', { x, y: base - h, width: bw, height: Math.max(h, d.count ? 1 : 0), rx: 2 });
    bar.append(svg('title', {}, t('{date}：{count} 条', { date: d.date, count: d.count })));
    s.append(bar);
    if (d.count) s.append(svg('text', { class: 'v', x: x + bw / 2, y: base - h - 3 }, String(d.count)));
    if (i % 2 === 0 || days.length <= 7) s.append(svg('text', { x: x + bw / 2, y: 144 }, d.date.slice(5)));
  });
  box.replaceChildren(s);
}

function drawTop(box, users) {
  box.replaceChildren();
  if (!users.length) { box.append(el('p', 'muted', t('近 7 天没有审计事件'))); return; }
  const list = el('div', 'au-top');
  const max = Math.max(1, ...users.map((u) => u.count));
  for (const u of users) {
    const row = el('div', 'row');
    const name = el('span', '', u.user_name);
    if (u.user_display) name.append(el('small', 'muted', ' ' + u.user_display));
    const bar = el('span', 'bar');
    const fill = el('i');
    fill.style.width = Math.max(2, Math.round((u.count / max) * 100)) + '%';
    bar.append(fill);
    row.append(name, bar, el('span', 'n', String(u.count)));
    list.append(row);
  }
  box.append(list);
}

async function loadOverview() {
  const o = await api('GET', API + '/overview');
  if (!o.days.length && !o.events_7d) return; // 仓库不支持概览：不显示
  const num = (v) => (v === null || v === undefined ? '—' : String(v));
  kpi($('lg-kpis'), [
    [t('近 7 天审计事件'), String(o.events_7d), ''],
    [t('登录失败'), String(o.login_failed_7d), t('其中账号锁定 {n} 次', { n: o.lockouts_7d }), o.login_failed_7d > 3 ? 'warn' : ''],
    [t('高危操作'), num(o.risky_7d), ''],
    [t('被拒绝'), num(o.denied_7d), '', o.denied_7d ? 'warn' : ''],
  ]);
  drawBars($('lg-bars'), o.days);
  drawTop($('lg-top'), o.top_users);
  $('lg-charts').hidden = false;
}

// ---- 通用审计 ----

function logParams(page) {
  return { action: $('lg-action').value, category: $('lg-category').value, result: $('lg-result').value, risky: $('lg-risky').checked ? 1 : '',
    user: $('lg-user').value.trim(), search: $('lg-search').value.trim(), start_date: $('lg-start').value, end_date: $('lg-end').value, page, page_size: 20 };
}

function categoryCell(r) {
  const td = el('td');
  if (!r.category_label && !r.module) { td.append(el('span', 'muted', '—')); return td; }
  td.append(el('span', 'badge', tp(r.category_label) || '—'));
  if (r.module && r.module !== '—') td.append(el('span', 'small muted', ' ' + tp(r.module)));
  if (r.risky) td.append(document.createTextNode(' '), el('span', 'badge au-risk', t('高危')));
  return td;
}

async function openDetail(id) {
  setMsg('');
  const d = await api('GET', `${API}/log/${id}`);
  const it = d.item;
  const body = $('au-detail-body');
  body.replaceChildren();
  const kv = el('dl', 'au-kv');
  const add = (k, v) => { kv.append(el('dt', '', k)); const dd = el('dd'); if (v instanceof Node) dd.append(v); else dd.textContent = dash(v); kv.append(dd); };
  add(t('时间'), it.action_time);
  add(t('用户'), it.user_display ? t('{name}（{display}）', { name: it.user_name, display: it.user_display }) : it.user_name);
  add(t('来源 IP'), it.ip);
  add(t('类别 / 模块'), [tp(it.category_label), it.module && it.module !== '—' ? tp(it.module) : ''].filter(Boolean).join(' · ') || '—');
  const code = el('span');
  if (it.code) {
    code.append(el('code', '', it.code));
    if (it.code_label) code.append(document.createTextNode(' ' + tp(it.code_label)));
    if (it.risky) code.append(document.createTextNode(' '), el('span', 'badge au-risk', t('高危')));
  } else code.textContent = '—';
  add(t('权限码'), code);
  add(t('动作'), tp(it.action));
  add(t('结果'), resultTag(it.result));
  body.append(kv, el('h3', '', t('额外信息')), el('pre', 'au-extra', it.extra_info || t('（无）')));
  if (d.recent.length) {
    body.append(el('h3', '', t('该用户的其他近期操作')));
    const tbl = el('table', 'list au-recent');
    const head = el('tr');
    for (const h of [t('时间'), t('动作'), t('结果')]) head.append(el('th', '', h));
    const thead = el('thead');
    thead.append(head);
    tbl.append(thead);
    const tb = el('tbody');
    for (const r of d.recent) {
      const tr = el('tr');
      const a = el('td', '', tp(r.action));
      a.title = r.extra_info || '';
      const rt = el('td');
      rt.append(resultTag(r.result));
      tr.append(el('td', 'small', r.action_time), a, rt);
      tb.append(tr);
    }
    tbl.append(tb);
    body.append(tbl);
  }
  detail.open();
}

async function loadLog(page = 1) {
  setMsg('');
  const data = await api('GET', API + '/log' + qs(logParams(page)));
  const body = $('lg-rows');
  if (!data.rows.length) emptyRow(body, 7, t('没有符合条件的审计记录。'));
  else {
    body.replaceChildren();
    for (const r of data.rows) {
      const tr = el('tr', 'au-click' + (r.result === '失败' || r.action === '登入失败' ? ' lv-err' : '')); // i18n-ignore 与后端存库的取值比较
      tr.tabIndex = 0;
      const open = () => openDetail(r.id).catch((e) => setMsg(e.message));
      tr.addEventListener('click', open);
      tr.addEventListener('keydown', (e) => { if (e.key === 'Enter') open(); });
      const user = el('td');
      user.append(el('span', 'mono', r.user_name));
      if (r.user_display) user.append(el('div', 'small muted', r.user_display));
      const act = el('td', '', tp(r.action));
      const res = el('td');
      res.append(resultTag(r.result));
      const extra = el('td', 'extra small', (r.extra_info || '').length > 120 ? r.extra_info.slice(0, 120) + '…' : r.extra_info || '');
      tr.append(el('td', 'small', r.action_time), user, categoryCell(r), act, res, el('td', 'mono small', dash(r.ip)), extra);
      body.append(tr);
    }
  }
  lastTotal = data.total;
  pager($('lg-pager'), data, (n) => loadLog(n).catch((e) => setMsg(e.message)));
}

// ---- SQL 上线审计 ----

function wfParams(page) {
  return { status: $('wf-status').value, group_id: $('wf-group').value, search: $('wf-search').value.trim(), start_date: $('wf-start').value, end_date: $('wf-end').value, page, page_size: 20 };
}

async function loadWfDist() {
  const p = wfParams(0);
  delete p.page; delete p.page_size;
  const list = await api('GET', API + '/sql-workflows/stats' + qs(p));
  const box = $('wf-dist');
  const total = list.reduce((a, x) => a + x.count, 0);
  if (!total) { box.hidden = true; return; }
  const stack = $('wf-stack'), legend = $('wf-legend');
  stack.replaceChildren(); legend.replaceChildren();
  list.forEach((x, i) => {
    const seg = el('i', 'au-c' + (i % 7));
    seg.style.width = (x.count / total) * 100 + '%';
    seg.title = t('{label}：{count}', { label: tp(x.label), count: x.count });
    stack.append(seg);
    const lg = el('span');
    lg.append(el('i', 'au-c' + (i % 7)), document.createTextNode(`${tp(x.label)} ${x.count}`));
    legend.append(lg);
  });
  $('wf-dist-sub').textContent = t('共 {total} 个', { total });
  box.hidden = false;
}

async function loadWf(page = 1) {
  setMsg('');
  loadWfDist().catch(() => { $('wf-dist').hidden = true; });
  const data = await api('GET', API + '/sql-workflows' + qs(wfParams(page)));
  const body = $('wf-rows');
  if (!data.rows.length) emptyRow(body, 8, t('没有符合条件的 SQL 上线工单。'));
  else {
    body.replaceChildren();
    for (const w of data.rows) {
      const tr = el('tr');
      const name = el('td');
      const a = el('a', '', `#${w.id} ${w.name}`);
      a.href = '/sqlworkflow/' + w.id;
      name.append(a);
      const st = el('td');
      st.append(status(tp(w.status_label), w.status === 'workflow_finish' ? 'passed' : (w.status === 'workflow_exception' || w.status === 'workflow_autoreviewwrong') ? 'rejected' : w.status === 'workflow_abort' ? 'aborted' : 'waiting'));
      tr.append(name, el('td', '', w.engineer_display || w.engineer), el('td', '', w.group_name), el('td', 'mono small', [w.instance_name, w.db_name].filter(Boolean).join(' / ')),
        el('td', '', tp(w.syntax_type)), st, el('td', 'small', w.create_time), el('td', 'small', w.finish_time || '—'));
      body.append(tr);
    }
  }
  pager($('wf-pager'), data, (n) => loadWf(n).catch((e) => setMsg(e.message)));
}

// ---- 查询审计 ----

function qlParams(page) {
  return { user: $('ql-user').value.trim(), instance: $('ql-instance').value.trim(), search: $('ql-search').value.trim(),
    start_date: $('ql-start').value, end_date: $('ql-end').value, only_rejected: $('ql-rejected').checked ? 1 : '', page, page_size: 20 };
}

const fmtSec = (v) => (v ? (v < 10 ? v.toFixed(3) : v.toFixed(1)) + ' s' : '—');

async function loadQlStats() {
  const p = qlParams(0);
  delete p.page; delete p.page_size; delete p.only_rejected;
  const s = await api('GET', API + '/query-logs/stats' + qs(p));
  kpi($('ql-kpis'), [
    [t('查询语句'), String(s.total), t('{n} 位用户', { n: s.users })],
    [t('被拒绝'), s.reject_ready ? String(s.rejected) : '—', '', s.rejected ? 'warn' : ''],
    [t('平均耗时'), fmtSec(s.avg_cost), ''],
    [t('最慢一条'), fmtSec(s.max_cost), s.max_user || ''],
  ]);
}

function sqlCell(r) {
  const td = el('td', 'sqlcell clip expand', r.sql + (r.sql_truncated ? ' ' + t('…（已截断）') : ''));
  let full = false;
  td.addEventListener('click', async () => {
    if (td.classList.contains('clip') && r.sql_truncated && !full) {
      try {
        const d = await api('GET', `${API}/query-logs/${r.kind}/${r.id}`);
        td.textContent = d.sql_full;
        full = true;
      } catch (e) { setMsg(e.message); }
    }
    td.classList.toggle('clip');
  });
  return td;
}

async function loadQl(page = 1) {
  setMsg('');
  loadQlStats().catch(() => { $('ql-kpis').hidden = true; });
  const data = await api('GET', API + '/query-logs' + qs(qlParams(page)));
  const note = $('ql-note');
  note.replaceChildren();
  if (!data.reject_ready) note.append(el('div', 'alert warn', t('被拒绝语句记录表未创建，请先执行 deploy/sql/050_query_reject.sql。')));
  const body = $('ql-rows');
  if (!data.rows.length) emptyRow(body, 8, t('没有符合条件的查询记录。'));
  else {
    body.replaceChildren();
    for (const r of data.rows) {
      const tr = el('tr', r.kind === 'rejected' ? 'lv-err' : '');
      const res = el('td');
      if (r.kind === 'rejected') {
        res.append(status(t('已拒绝'), 'rejected'));
        if (r.reason) res.append(el('div', 'au-reason', tp(r.reason)));
      } else res.append(r.status === 'masked' ? status(t('已脱敏'), 'running') : status(t('正常'), 'passed'));
      tr.append(el('td', 'small', r.create_time), el('td', '', `${r.username} ${r.user_display}`), el('td', 'mono small', dash(r.instance_name)), el('td', 'mono small', dash(r.db_name)),
        sqlCell(r), el('td', 'num', r.kind === 'rejected' ? '—' : String(r.effect_row)), el('td', 'num', r.kind === 'rejected' ? '—' : dash(r.cost_time)), res);
      body.append(tr);
    }
  }
  pager($('ql-pager'), data, (n) => loadQl(n).catch((e) => setMsg(e.message)));
}

function guard(fn) { return () => fn().catch((e) => setMsg(e.message)); }

tabs($('au-tabs'), (name) => {
  if (name === 'wf' && !loaded.wf) { loaded.wf = true; guard(() => loadWf(1))(); }
  if (name === 'ql' && !loaded.ql) { loaded.ql = true; guard(() => loadQl(1))(); }
});

for (const [form, fn] of [['lg-form', loadLog], ['wf-form', loadWf], ['ql-form', loadQl]]) {
  $(form).addEventListener('submit', (e) => { e.preventDefault(); guard(() => fn(1))(); });
}
$('lg-reset').addEventListener('click', () => {
  $('lg-form').reset();
  $('lg-start').value = dateFrom(7);
  guard(() => loadLog(1))();
});
// 当前筛选条件的文字描述（导出确认框里展示）
function filterDesc() {
  const sel = (id, all) => { const e = $(id); return e.value ? e.options[e.selectedIndex].textContent : all; };
  const parts = [];
  if ($('lg-action').value) parts.push(t('动作={v}', { v: sel('lg-action', '') }));
  if ($('lg-category').value) parts.push(t('类别={v}', { v: sel('lg-category', '') }));
  if ($('lg-result').value) parts.push(t('结果={v}', { v: sel('lg-result', '') }));
  if ($('lg-risky').checked) parts.push(t('仅高危'));
  if ($('lg-user').value.trim()) parts.push(t('用户={v}', { v: $('lg-user').value.trim() }));
  if ($('lg-search').value.trim()) parts.push(t('关键词={v}', { v: $('lg-search').value.trim() }));
  if ($('lg-start').value || $('lg-end').value) parts.push(t('日期 {start} ~ {end}', { start: $('lg-start').value || t('不限'), end: $('lg-end').value || t('不限') }));
  return parts.length ? parts.join(t('；|sep')) : t('无（全部记录）');
}

// 用 fetch 取回 CSV，才能读到响应头里的“已截断”标记；成功后再触发浏览器下载
async function fetchExport(params) {
  const res = await fetch(API + '/log/export' + qs(params), { credentials: 'same-origin' });
  if (!res.ok) {
    let msg = '';
    try { msg = (await res.json()).message; } catch (_) { /* 非 JSON 错误页 */ }
    throw new Error(msg || t('导出失败（HTTP {status}）', { status: res.status }));
  }
  const m = /filename="([^"]+)"/.exec(res.headers.get('Content-Disposition') || '');
  return { blob: await res.blob(), name: m ? m[1] : 'audit-log.csv', truncated: res.headers.get('X-Export-Truncated') === '1' };
}

function saveBlob(blob, name) {
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = name;
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 10000);
}

async function doExport() {
  const p = logParams(1);
  delete p.page; delete p.page_size;
  const over = lastTotal !== null && lastTotal > EXPORT_MAX;
  const r = await confirmRun({
    title: t('导出审计日志（CSV）'), code: 'audit.export', level: 'high',
    lines: [
      over ? t('最多导出 {max} 行；当前筛选共 {total} 条，超出部分会被截断，请先缩小日期范围或筛选条件。', { max: EXPORT_MAX, total: lastTotal }) : t('最多导出 {max} 行。', { max: EXPORT_MAX }),
      t('文件包含用户、来源 IP 与操作详情，请按数据安全规范保管。这次导出本身会写入系统审计。'),
    ],
    details: [{ k: t('筛选条件'), v: filterDesc() }, { k: t('当前命中'), v: lastTotal === null ? '—' : t('{total} 条', { total: lastTotal }) }],
    okText: t('确认导出'),
  }, () => fetchExport(p));
  if (!r.ok) return;
  saveBlob(r.result.blob, r.result.name);
  if (r.result.truncated) {
    const tip = t('结果超过 {max} 行，导出文件已截断为前 {max} 行。请缩小日期范围或筛选条件后分批导出。', { max: EXPORT_MAX });
    setMsg(tip, 'warn');
    toast(tip, 'warn');
  } else {
    toast(t('已导出，并写入系统审计'));
  }
}

$('lg-export').addEventListener('click', () => { doExport().catch((e) => setMsg(e.message)); });

// 没有 audit.export 按钮权限时置灰（只是显示层，接口仍按权限码校验）
async function gateExport() {
  const b = await loadButtons();
  if (b && !can(b, 'audit.export')) {
    const btn = $('lg-export');
    btn.disabled = true;
    btn.setAttribute('aria-disabled', 'true');
    btn.title = t('无权限：需要 audit.export（导出审计日志）');
  }
}

function extNote() {
  const box = $('lg-extnote');
  box.replaceChildren();
  if (!opts.ext_ready) {
    box.append(el('div', 'alert warn', t('审计扩展表未创建，请先执行 deploy/sql/090_audit_ext.sql。')));
  }
}

async function init() {
  $('lg-start').value = dateFrom(7); // 默认最近 7 天，避免一上来就扫全表
  opts = await api('GET', API + '/options');
  fillSelect($('lg-action'), [{ v: '', t: t('全部动作') }, ...opts.actions.map((a) => ({ v: a, t: tp(a) }))], (x) => x.t, (x) => x.v);
  fillSelect($('lg-category'), [{ v: '', t: t('全部类别') }, ...opts.categories.map((c) => ({ v: c.value, t: tp(c.label) }))], (x) => x.t, (x) => x.v);
  fillSelect($('lg-result'), [{ v: '', t: t('全部结果') }, ...opts.results.map((r) => ({ v: r, t: tp(r) }))], (x) => x.t, (x) => x.v);
  for (const id of ['lg-category', 'lg-result', 'lg-risky']) $(id).disabled = !opts.ext_ready;
  fillSelect($('wf-status'), [{ v: '', t: t('全部状态') }, ...opts.sql_statuses.map((s) => ({ v: s.value, t: tp(s.label) }))], (x) => x.t, (x) => x.v);
  fillSelect($('wf-group'), [{ v: '', t: t('全部资源组') }, ...opts.resource_groups.map((g) => ({ v: g.id, t: g.name }))], (x) => x.t, (x) => x.v);
  extNote();
  gateExport().catch(() => {});
  loadOverview().catch(() => { /* 概览是附加信息，失败不影响列表 */ });
  await loadLog(1);
}

init().catch((e) => { emptyRow($('lg-rows'), 7, t('加载失败')); setMsg(e.message); });
