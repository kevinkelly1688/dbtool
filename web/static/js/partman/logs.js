/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: logs.js
 * @time: 2026/10/28
 */
// 分区维护：全部配置的维护日志（按配置 × 来源筛选）。
import { t, tp } from '../i18n.js';
import { el, setMsg, sourceTag, logStatusTag, logItemsCell, labelCells, roundCell, KIND_LABEL, colon } from './util.js';
import { logOutputRow } from './trace.js';

const $ = (id) => document.getElementById(id);
const PAGE = 20;
const state = { offset: 0 };
let configsFilled = false;

function query() {
  const p = new URLSearchParams({ offset: String(state.offset), limit: String(PAGE) });
  if ($('pg-config').value) p.set('config_id', $('pg-config').value);
  const src = $('pg-source').value;
  if (src === 'system') p.set('scope', 'system'); // 系统记录：定时维护轮次汇总 / 推送测试（不属于某个配置）
  else if (src) p.set('source', src);
  return p.toString();
}

// 接口同时返回 data、total 与筛选用的 configs，api() 只返回 data，所以这里直接取。
async function fetchLogs() {
  const res = await fetch('/api/v1/partman/logs?' + query(), { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
  const payload = await res.json().catch(() => null);
  if (!res.ok) throw new Error((payload && payload.message) || t('请求失败（HTTP {status}）', { status: res.status }));
  return { rows: payload.data || [], total: payload.total || 0, configs: payload.configs || [] };
}

function fillConfigs(configs, wanted) {
  const sel = $('pg-config');
  sel.replaceChildren(new Option(t('全部配置'), ''));
  for (const c of configs) sel.append(new Option(t('#{id} {table}（{inst}）', { id: c.cnf_id, table: c.table, inst: c.instance_name }), String(c.id)));
  sel.value = wanted && configs.some((c) => String(c.id) === wanted) ? wanted : '';
  configsFilled = true;
}

function cell(text, cls) { return el('td', cls || '', text === undefined || text === null || text === '' ? '—' : String(text)); }

function render(rows) {
  const body = $('pg-rows');
  body.replaceChildren();
  if (!rows.length) {
    const tr = el('tr');
    const c = el('td', 'empty', t('没有符合条件的维护日志'));
    c.colSpan = 8;
    tr.append(c);
    body.append(tr);
    return;
  }
  for (const l of rows) {
    const tr = el('tr');
    const src = el('td');
    src.append(sourceTag(l.source));
    const st = el('td');
    const system = l.scope === 'system';
    if (system) st.append(el('span', 'muted', '—'));
    else st.append(logStatusTag(l.status));
    const res = el('td');
    const out = logOutputRow(l, 8);
    if (l.round) res.append(roundCell(l.round));
    res.append(logItemsCell(l), out.btn);
    const tbl = system ? el('td') : cell(l.table, 'mono');
    if (system) tbl.append(el('span', 'st aborted', t('系统记录') + (KIND_LABEL[l.kind] ? colon + KIND_LABEL[l.kind] : '')), el('div', 'muted small', tp(l.table || '')));
    tr.append(cell(l.create_time, 'nowrap'), cell(l.id), cell(l.instance_name), tbl, src, cell(l.operator), st, res);
    labelCells(tr, [t('时间'), t('日志 ID'), t('实例'), t('库.表'), t('来源'), t('操作人'), t('状态'), t('语句与结果')]);
    body.append(tr);
    if (out.row) body.append(out.row);
  }
}

async function load() {
  try {
    const r = await fetchLogs();
    if (!configsFilled) fillConfigs(r.configs, new URLSearchParams(location.search).get('config_id') || '');
    render(r.rows);
    setMsg($('pg-msg'), '');
    $('pg-total').textContent = t('共 {total} 条，第 {page} / {pages} 页', { total: r.total, page: Math.floor(state.offset / PAGE) + 1, pages: Math.max(1, Math.ceil(r.total / PAGE)) });
    $('pg-prev').disabled = state.offset <= 0;
    $('pg-next').disabled = state.offset + PAGE >= r.total;
  } catch (e) {
    setMsg($('pg-msg'), e.message);
  }
}

// 页面带 ?config_id=（从配置列表的“日志”弹窗跳来）时先把下拉选好再查，避免先闪一遍全部。
async function init() {
  const wanted = new URLSearchParams(location.search).get('config_id') || '';
  const src = new URLSearchParams(location.search).get('source') || '';
  if (src) $('pg-source').value = src;
  if (wanted) {
    // 先把下拉填好才能按配置筛选：先不带筛选取一次配置清单。
    try {
      const res = await fetch('/api/v1/partman/logs?limit=1', { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
      const payload = await res.json().catch(() => null);
      if (res.ok && payload) fillConfigs(payload.configs || [], wanted);
    } catch (_) { /* 取不到就按全部配置显示 */ }
  }
  await load();
}

$('pg-filter').addEventListener('submit', (e) => { e.preventDefault(); state.offset = 0; load(); });
for (const id of ['pg-config', 'pg-source']) $(id).addEventListener('change', () => { state.offset = 0; load(); });
$('pg-prev').addEventListener('click', () => { state.offset = Math.max(0, state.offset - PAGE); load(); });
$('pg-next').addEventListener('click', () => { state.offset += PAGE; load(); });

init();
