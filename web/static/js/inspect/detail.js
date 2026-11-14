/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: detail.js
 * @time: 2026/10/28
 */
// 性能巡检报告详情：摘要、同实例趋势图、提示块、按分区展示的检查项（带“建议”列；SQL Server 的检查项可展开明细表）、删除报告。进行中的报告每 5 秒刷新。
import { api, confirmRun } from '../common.js';
import { t, tp } from '../i18n.js';
import { $, el, btn, setMsg, fmtFull, fmtElapsed, statusBadge, scoreEl, apiFull } from './util.js';
import { trendPoints, renderTrend, isScoreKind } from './trend.js';

const BASE = '/api/v1/inspection';
const ref = $('insp-detail').dataset.ref;
let canRun = false;
let canDelete = false;
let timer = null;
let trendFor = ''; // 已经画过趋势图的实例，避免进行中的报告每 5 秒刷新时反复请求

const LEVEL = [[t('正常'), 'passed'], [t('关注'), 'waiting'], [t('告警'), 'rejected'], [t('未采集|inspect'), 'aborted']];
const ROW_CLASS = ['', 'lv-warn', 'lv-err', ''];

function triggerText(r) {
  if (r.trigger === 'schedule') return r.plan_name ? t('定时 · {name}', { name: r.plan_name }) : t('定时');
  return r.operator ? t('手动 · {name}', { name: r.operator }) : t('手动');
}

function bignum(label, value, cls) {
  const d = el('div');
  d.append(el('b', 'n' + (cls ? ' ' + cls : ''), value), el('span', '', label));
  return d;
}

function renderSummary(d) {
  const r = d.report;
  $('d-title').textContent = `#${r.id} · ${r.instance_name}`;
  const st = $('d-status');
  st.replaceChildren(statusBadge(r));
  const parts = [`${d.engine}`, t('开始 {time}', { time: fmtFull(r.start_time) }), t('耗时 {time}', { time: fmtElapsed(r.elapsed) }), triggerText(r)];
  if (r.total) parts.push(t('{n} 项检查', { n: r.total }));
  $('d-sub').textContent = parts.join(' · ');
  const finished = r.status !== 'running';
  $('d-summary').hidden = !finished && !d.sections.length;
  const nums = $('d-nums');
  nums.replaceChildren();
  if (isScoreKind(r.kind)) {
    const s = r.health_score;
    nums.append(bignum(t('健康评分'), s === null || s === undefined ? '–' : s, s >= 90 ? 'ok' : s >= 70 ? 'warn' : 'crit'));
  }
  nums.append(bignum(t('告警'), r.alert, r.alert ? 'crit' : ''), bignum(t('关注'), r.attention, r.attention ? 'warn' : ''));
  if (r.kind === 'mssql') nums.append(bignum(t('正常'), r.normal, 'ok'));
  nums.append(bignum(t('未采集|inspect'), r.failed, ''));
  const facts = $('d-facts');
  facts.replaceChildren();
  for (const f of d.facts) facts.append(el('dt', '', f.label), el('dd', '', f.value));
  $('d-rerun').hidden = !(canRun && r.status !== 'running');
  $('d-delete').hidden = !(canDelete && r.status !== 'running'); // 进行中的报告要先取消才能删
  if (r.status === 'running') {
    setMsg($('d-msg'), t('巡检进行中：{done} / {total} 项', { done: r.done, total: r.total || '?' }), 'info');
  } else if (r.status === 'cancelled') {
    setMsg($('d-msg'), r.error ? t('该巡检已被取消（{reason}）；只显示取消前已采集的部分。', { reason: tp(r.error) }) : t('该巡检已被取消；只显示取消前已采集的部分。'), 'warn');
  } else if (r.status === 'failed') {
    setMsg($('d-msg'), t('巡检失败：{reason}', { reason: r.error ? tp(r.error) : t('未知原因') }));
  } else {
    setMsg($('d-msg'), '');
  }
}

function renderBlocks(d) {
  const box = $('d-blocks');
  box.replaceChildren();
  for (const b of d.blocks) {
    const div = el('div', 'alert ' + (b.kind === 'err' ? 'err' : b.kind === 'warn' ? 'warn' : 'info'));
    div.append(el('b', '', b.title));
    const ol = document.createElement('ol');
    for (const line of b.items) ol.append(el('li', '', line));
    div.append(ol);
    box.append(div);
  }
  $('d-note').hidden = !d.note;
  $('d-note').textContent = d.note || '';
}

function detailTable(tbl) {
  const wrap = el('div', 'item-detail-wrap');
  const table = el('table', 'list');
  const head = el('tr');
  for (const c of tbl.columns) head.append(el('th', '', tp(c)));
  const thead = el('thead');
  thead.append(head);
  table.append(thead);
  const body = el('tbody');
  const last = tbl.columns[tbl.columns.length - 1] === '结论'; // i18n-ignore 与后端返回的列名比较
  for (const row of tbl.rows) {
    const tr = el('tr', last ? (row[row.length - 1] === '告警' ? 'lv-err' : row[row.length - 1] === '关注' ? 'lv-warn' : '') : ''); // i18n-ignore 与后端返回的结论比较
    row.forEach((cell, i) => {
      const td = el('td', tbl.columns[i] === 'SQL' ? 'cell-sql' : '', cell === null || cell === undefined ? '' : tp(cell));
      if (typeof cell === 'number') td.classList.add('num');
      tr.append(td);
    });
    body.append(tr);
  }
  table.append(body);
  wrap.append(table);
  return wrap;
}

function renderSections(d) {
  const box = $('d-sections');
  box.replaceChildren();
  const mssql = d.report.kind === 'mssql';
  for (const s of d.sections) {
    const det = el('details', 'acc');
    det.open = s.expanded;
    const sum = el('summary', '', s.name);
    if (s.alert) sum.append(el('span', 'badge crit', t('告警 {n}', { n: s.alert })));
    if (s.attention) sum.append(el('span', 'badge warn', t('关注 {n}', { n: s.attention })));
    if (s.failed) sum.append(el('span', 'badge', t('未采集 {n}', { n: s.failed })));
    if (!s.alert && !s.attention && !s.failed) sum.append(el('span', 'badge ok', t('正常')));
    sum.append(document.createTextNode(' '), el('small', 'muted', t('{n} 项', { n: s.items.filter((i) => !i.detail).length })));
    det.append(sum);
    const table = el('table', 'list');
    const head = el('tr');
    for (const c of mssql ? [t('检查项'), t('范围'), t('当前值'), t('阈值'), t('结论'), t('建议')] : [t('检查项'), t('当前值'), t('阈值'), t('结论'), t('建议')]) head.append(el('th', '', c));
    const thead = el('thead');
    thead.append(head);
    table.append(thead);
    const body = el('tbody');
    for (const it of s.items) {
      const tr = el('tr', it.detail ? 'detail-row' : ROW_CLASS[it.level] || '');
      const name = el('td', '', it.name);
      if (it.table) {
        const x = btn(t('明细'), 'expander');
        name.append(x);
        const dr = el('tr', 'item-detail');
        dr.hidden = true;
        const dc = el('td');
        dc.colSpan = mssql ? 6 : 5;
        dc.append(detailTable(it.table));
        dr.append(dc);
        x.addEventListener('click', () => { dr.hidden = !dr.hidden; x.textContent = dr.hidden ? t('明细') : t('收起'); });
        tr.append(name);
        appendCells(tr, it, mssql);
        body.append(tr, dr);
        continue;
      }
      tr.append(name);
      appendCells(tr, it, mssql);
      body.append(tr);
    }
    table.append(body);
    det.append(table);
    box.append(det);
  }
}

function appendCells(tr, it, mssql) {
  if (mssql) tr.append(el('td', '', it.db || t('实例')));
  tr.append(el('td', 'val', it.value), el('td', 'small', it.threshold || '—'));
  const verdict = el('td');
  if (!it.detail) {
    const [text, cls] = LEVEL[it.level] || ['', ''];
    verdict.append(el('span', 'st ' + cls, text));
  }
  tr.append(verdict);
  // 建议：关注 / 告警 / 未采集的检查项才显示（正常项留空，避免满屏文字）；明细行不显示
  const advice = el('td', 'advice');
  if (!it.detail && it.level > 0 && it.advice) advice.textContent = it.advice;
  else advice.append(el('span', 'muted', '—'));
  tr.append(advice);
}

// ---- 同实例趋势 ----
async function loadTrend(d) {
  const r = d.report;
  if (trendFor === r.instance_id + ':' + r.ref) return;
  trendFor = r.instance_id + ':' + r.ref;
  if (r.status === 'running') { $('d-trend').hidden = true; return; }
  try {
    const res = await apiFull('/api/v1/inspection/reports?instance_id=' + r.instance_id + '&limit=60');
    const pts = trendPoints(res.data || [], r.kind, 30);
    $('d-trend').hidden = false;
    $('d-trend-sub').textContent = isScoreKind(r.kind) ? t('健康评分') : t('告警（红）/ 关注（黄）项数');
    renderTrend($('d-trend-chart'), pts, r.kind, r.ref);
  } catch (_) {
    $('d-trend').hidden = true; // 趋势拿不到不影响看报告
  }
}

// ---- 删除报告（二次确认）----
async function deleteReport(d) {
  const r = d.report;
  const res = await confirmRun({
    title: t('删除巡检报告'), level: 'high', code: 'inspect.delete', codeName: t('删除报告'),
    lines: [t('删除后不可恢复（报告内容与检查项一并删除），操作会写入操作日志。')],
    details: [{ k: t('报告'), v: '#' + r.id }, { k: t('实例'), v: r.instance_name }, { k: t('开始时间'), v: fmtFull(r.start_time) }],
    typed: '#' + r.id, typedLabel: t('请输入报告标识确认删除：'), okText: t('确认删除'),
  }, () => api('POST', BASE + '/reports/delete', { ids: [r.ref] }));
  if (res.ok) location.href = '/inspection';
}

async function load() {
  clearTimeout(timer);
  try {
    const d = await api('GET', `${BASE}/reports/${ref}`);
    renderSummary(d);
    renderBlocks(d);
    renderSections(d);
    loadTrend(d);
    $('d-delete').onclick = () => deleteReport(d);
    if (d.report.status === 'running') timer = setTimeout(load, 5000);
    $('d-rerun').onclick = async () => {
      const res = await confirmRun({
        title: t('重新巡检'), code: 'inspect.run', codeName: t('立即 / 重新巡检'),
        lines: [t('将对该实例重新生成一份巡检报告（只读采集）；该实例已有进行中的巡检时会被跳过。')],
        details: [{ k: t('实例'), v: d.report.instance_name }],
        okText: t('开始巡检'),
      }, () => api('POST', BASE + '/reports/run', { instance_ids: [d.report.instance_id] }));
      if (res.ok) location.href = '/inspection';
    };
  } catch (e) {
    setMsg($('d-msg'), e.message);
    $('d-sub').textContent = '';
  }
}

(async () => {
  try {
    const o = await api('GET', BASE + '/options');
    canRun = !!o.can_run;
    canDelete = !!o.can_delete;
  } catch (_) { /* 没有选项也能看报告 */ }
  load();
})();
