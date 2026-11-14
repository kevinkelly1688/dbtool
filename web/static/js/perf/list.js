/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: list.js
 * @time: 2026/10/28
 */
// 性能工单列表：KPI 卡片、页签（我的待办 / 全部未关闭 / 待分派 / 待受理 / 处理中 / 待验证 / 审批中 / 已超时 / 已关闭 / 全部）、
// 筛选（级别 / 团队 / 处理方 / 类型 / 状态 / 来源 / 可见范围）、排序、分页、手工建单。
import { api, riskConfirm } from '../common.js';
import { t, tp } from '../i18n.js';
import { $, el, sevBadge, statusBadge, fmtNum, fmtHours, setMsg, fillSelect, openModal, closeModal, wireModals, qs, busy } from './util.js';

const PAGE = 20;
const st = { view: 'todo', offset: 0, sort: 'impact_score', desc: true, total: 0, opts: null };
const msg = $('pl-msg');
const canCreate = $('perf-list').dataset.create === 'true';

function slaText(r) {
  if (r.status === 'closed') return { text: tp(r.close_reason_label) || t('已关闭'), cls: '' };
  if (r.left_hours === null || r.left_hours === undefined) return { text: '-', cls: '' };
  const lv = r.escalation_level > 0 ? ' L' + r.escalation_level : '';
  if (r.overdue) return { text: t('超时 {time}', { time: fmtHours(r.left_hours) }) + lv, cls: 'crit' };
  return { text: t('剩余 {time}', { time: fmtHours(r.left_hours) }) + lv, cls: '' };
}

const EMPTY = { todo: t('没有待办的工单。'), new: t('没有待分派的工单。'), approve: t('没有审批中的工单。'), overdue: t('没有已超时的工单。') };

function renderRows(rows) {
  const body = $('pl-rows');
  body.replaceChildren();
  if (!rows.length) {
    const td = el('td', 'empty', EMPTY[st.view] || t('没有符合条件的工单。'));
    td.colSpan = 12;
    const tr = el('tr');
    tr.append(td);
    body.append(tr);
    return;
  }
  for (const r of rows) {
    const tr = el('tr', r.overdue ? 'overdue' : '');
    tr.tabIndex = 0;
    const go = () => { location.href = '/perf/' + r.id; };
    tr.addEventListener('click', go);
    tr.addEventListener('keydown', (e) => { if (e.key === 'Enter') go(); });
    const sql = el('td');
    sql.append(el('div', 'mono clip', r.fingerprint), el('small', 'muted', `${r.instance} / ${r.db_name || '-'} · ${tp(r.category_label)}`));
    sql.title = r.fingerprint;
    const sev = el('td');
    sev.append(sevBadge(r.severity));
    const stt = el('td');
    stt.append(statusBadge(r.status, tp(r.status_label)));
    const sla = slaText(r);
    const slaTd = el('td');
    slaTd.append(el(sla.cls ? 'b' : 'span', sla.cls, sla.text));
    tr.append(el('td', '', '#' + r.id), sev, stt, sql, el('td', 'num', fmtNum(r.impact_score) + ' s'), el('td', 'num', r.load_share.toFixed(1) + '%'),
      el('td', 'num', fmtNum(r.avg_time_ms) + ' ms'), el('td', 'num', fmtNum(r.per_day_count)), el('td', '', tp(r.owner_side_label)), el('td', '', r.team || '-'),
      el('td', '', r.assignee || t('待分派')), slaTd);
    body.append(tr);
  }
}

// 页签角标：有数量才显示（0 不显示，避免满屏的 0）。
function setBadge(b, n) {
  b.hidden = !n;
  b.textContent = String(n || 0);
}

// KPI 卡片与页签角标用同一份计数（服务端按当前可见范围一次算出）。
function applyCounts(c) {
  document.querySelectorAll('#pl-tabs [data-count]').forEach((b) => setBadge(b, c[b.dataset.count]));
  $('pl-k-todo').textContent = fmtNum(c.todo);
  $('pl-k-todo-sub').textContent = c.todo_overdue ? t('其中 {n} 张已超时', { n: c.todo_overdue }) : '';
  $('pl-k-overdue').textContent = fmtNum(c.overdue);
  $('pl-k-new').textContent = fmtNum(c.new);
  $('pl-k-fixed').textContent = fmtNum(c.fixed);
  $('pl-k-created').textContent = fmtNum(c.created_7d);
  $('pl-k-closed').textContent = fmtNum(c.closed_7d);
}

async function load() {
  setMsg(msg, '');
  const q = qs({
    view: st.view, severity: $('pl-sev').value, team_id: $('pl-team').value, instance_id: $('pl-inst').value, owner_side: $('pl-side').value, category: $('pl-cat').value,
    status: $('pl-status').value, source: $('pl-src').value, scope: $('pl-scope') ? $('pl-scope').value : '',
    search: $('pl-search').value.trim(), sort: st.sort, order: st.desc ? 'desc' : 'asc', offset: st.offset, limit: PAGE,
  });
  try {
    const res = await fetchList(q);
    st.total = res.total;
    renderRows(res.data);
    if (res.summary.counts) {
      applyCounts(res.summary.counts);
    } else { // 仓库不支持一次算出全部计数时，只有“我的待办”和“已超时”两个角标
      setBadge($('pl-b-todo'), res.summary.todo);
      setBadge($('pl-b-overdue'), res.summary.overdue);
    }
    const pages = Math.max(1, Math.ceil(res.total / PAGE));
    $('pl-info').textContent = t('共 {total} 条，第 {page} / {pages} 页', { total: res.total, page: Math.floor(st.offset / PAGE) + 1, pages });
    $('pl-prev').disabled = st.offset <= 0;
    $('pl-next').disabled = st.offset + PAGE >= res.total;
  } catch (e) {
    setMsg(msg, e.message);
  }
}

// 列表接口返回 {data, total, summary}，common.js 的 api() 只取 data，所以这里直接用 fetch 取完整响应。
async function fetchList(q) {
  const r = await fetch('/api/v1/perf/issues?' + q, { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
  let p = null;
  try { p = await r.json(); } catch (_) { /* 非 JSON */ }
  if (!r.ok) throw new Error((p && p.message) || t('请求失败（HTTP {status}）', { status: r.status }));
  return p;
}

function reload() { st.offset = 0; load(); }

async function init() {
  st.opts = await api('GET', '/api/v1/perf/options');
  const o = st.opts;
  fillSelect($('pl-sev'), o.severities, (x) => tp(x.label), (x) => x.value, ['', t('全部级别')]);
  fillSelect($('pl-team'), o.teams, (x) => x.name, (x) => x.id, ['', t('全部团队')]);
  fillSelect($('pl-inst'), o.filter_instances || [], (x) => x.name, (x) => x.id, ['', t('全部实例')]);
  fillSelect($('pl-side'), o.owner_sides, (x) => tp(x.label), (x) => x.value, ['', t('全部处理方')]);
  fillSelect($('pl-cat'), o.categories, (x) => tp(x.label), (x) => x.value, ['', t('全部类型')]);
  fillSelect($('pl-status'), o.statuses, (x) => tp(x.label), (x) => x.value, ['', t('全部状态')]);
  fillSelect($('pl-src'), o.sources || [], (x) => tp(x.label), (x) => x.value, ['', t('全部来源')]);
  ['pl-sev', 'pl-team', 'pl-inst', 'pl-side', 'pl-cat', 'pl-status', 'pl-src', 'pl-scope'].forEach((id) => { if ($(id)) $(id).addEventListener('change', reload); });
  let timer = null;
  $('pl-search').addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(reload, 300); });
  $('pl-filters').addEventListener('submit', (e) => { e.preventDefault(); reload(); });
  document.querySelectorAll('#pl-tabs .tab').forEach((tab) => tab.addEventListener('click', () => {
    document.querySelectorAll('#pl-tabs .tab').forEach((x) => x.classList.toggle('on', x === tab));
    st.view = tab.dataset.view;
    reload();
  }));
  document.querySelectorAll('button.sort').forEach((b) => b.addEventListener('click', () => {
    st.desc = st.sort === b.dataset.sort ? !st.desc : true;
    st.sort = b.dataset.sort;
    reload();
  }));
  $('pl-prev').addEventListener('click', () => { st.offset = Math.max(0, st.offset - PAGE); load(); });
  $('pl-next').addEventListener('click', () => { st.offset += PAGE; load(); });
  if (canCreate) initCreate();
  await load();
}

// ---- 手工建单（仅 DBA） ----

function initCreate() {
  wireModals();
  const o = st.opts;
  $('pl-new').addEventListener('click', () => {
    setMsg($('pc-msg'), '');
    fillSelect($('pc-inst'), o.instances, (x) => t('{name}（{type}）', { name: x.name, type: x.db_type }), (x) => x.id);
    fillSelect($('pc-sev'), o.severities, (x) => tp(x.label), (x) => x.value, ['', t('自动')]);
    fillSelect($('pc-user'), o.users, (x) => t('{display}（{username}）', { display: x.display || x.username, username: x.username }), (x) => x.id, ['0', t('自动')]);
    $('pc-sql').value = '';
    $('pc-remark').value = '';
    $('pc-db').value = '';
    openModal('m-perf-create');
  });
  $('pc-ok').addEventListener('click', (ev) => busy(ev.currentTarget, async () => {
    setMsg($('pc-msg'), '');
    try {
      const r = await api('POST', '/api/v1/perf/issues', {
        instance_id: Number($('pc-inst').value), db_name: $('pc-db').value.trim(), sql: $('pc-sql').value, severity: $('pc-sev').value,
        assignee_id: Number($('pc-user').value), remark: $('pc-remark').value.trim(),
      });
      closeModal('m-perf-create');
      location.href = '/perf/' + r.id;
    } catch (e) {
      if (e.code === 'exists' && e.data && e.data.id) {
        const r = await riskConfirm({ title: t('已存在相同的工单'), lines: [e.message], details: [{ k: t('已有工单'), v: '#' + e.data.id }], okText: t('打开已有工单'), cancelText: t('留在这里') });
        if (r.ok) location.href = '/perf/' + e.data.id;
        return;
      }
      setMsg($('pc-msg'), e.message);
    }
  }));
}

init().catch((e) => setMsg(msg, e.message));
