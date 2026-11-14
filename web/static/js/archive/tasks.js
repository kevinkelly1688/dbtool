/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: tasks.js
 * @time: 2026/10/28
 */
// 归档任务页签：KPI、筛选、列表（运行中任务行内显示实时进度）、分页。
import { applyButtons } from '../btnperm.js';
import { t, tp } from '../i18n.js';
import { A } from './api.js';
import { $, el, add, button, pbutton, link, fillSelect, nf, clip, debounce, go, notifyChanged, labelCells } from './util.js';
import { flash } from './ui.js';
import { statusTag, modeTag, resultTag, backupTags, PAUSE_KIND } from './consts.js';
import { taskButtons } from './actions.js';
import { liveNode } from './live.js';
import { acquire, subscribe, byTask } from './poller.js';

const PAGE = 50;
const root = () => $('ar-view-tasks');

const st = {
  built: false,
  f: { status: '', mode: '', instance_id: '', q: '', mine: false },
  offset: 0,
  total: 0,
  items: [],
  loading: false,
};
let liveCells = new Map(); // task_id -> 行内进度容器
let runningNow = new Set(); // 最近一次加载时 running 非空的任务 id
let releasePoll = null;
let unsub = null;
let slowTimer = 0;
let lastReload = 0;
let shown = false;

function build() {
  if (st.built) return;
  st.built = true;
  const r = root();
  const kpis = el('div', 'ar-kpis');
  kpis.id = 'ar-t-kpis';
  r.append(kpis);

  const tb = el('div', 'toolbar');
  const selStatus = el('select');
  selStatus.id = 'ar-t-status';
  selStatus.setAttribute('aria-label', t('状态'));
  const selMode = el('select');
  selMode.id = 'ar-t-mode';
  selMode.setAttribute('aria-label', t('模式'));
  const selInst = el('select');
  selInst.id = 'ar-t-inst';
  selInst.setAttribute('aria-label', t('实例'));
  const q = el('input');
  q.type = 'search';
  q.id = 'ar-t-q';
  q.placeholder = t('搜索任务 / 库表 / 实例');
  q.setAttribute('aria-label', t('搜索'));
  q.style.minWidth = '220px';
  const mine = el('input');
  mine.type = 'checkbox';
  mine.id = 'ar-t-mine';
  const mineLab = el('label', 'chk');
  mineLab.append(mine, document.createTextNode(' ' + t('待我审批')));
  mineLab.id = 'ar-t-minelab';
  const newBtn = pbutton('archive.apply', t('＋ 新建归档任务'), 'primary', () => go('#wizard'));
  tb.append(selStatus, selMode, selInst, q, mineLab, el('span', 'spacer'), newBtn);
  r.append(tb);

  const card = el('section', 'card');
  const table = el('table', 'list ar-cards');
  const thead = el('thead');
  const hr = el('tr');
  [t('名称'), t('来源表'), t('模式'), t('备份去向'), t('调度'), t('状态'), t('上次运行'), t('下次运行'), t('累计归档行数'), t('操作')].forEach((title) => hr.append(el('th', '', title)));
  thead.append(hr);
  const body = el('tbody');
  body.id = 'ar-t-rows';
  table.append(thead, body);
  const pager = el('div', 'ar-pager');
  const total = el('span', 'muted');
  total.id = 'ar-t-total';
  pager.append(total, el('span', 'spacer'), button(t('上一页'), 'ghost', () => { st.offset = Math.max(0, st.offset - PAGE); load(); }), button(t('下一页'), 'ghost', () => { st.offset += PAGE; load(); }));
  pager.children[2].id = 'ar-t-prev';
  pager.children[3].id = 'ar-t-next';
  const wrap = el('div', 'ar-tbl-wrap');
  wrap.append(table);
  card.append(wrap, pager);
  r.append(card);

  fillSelect(selStatus, [['pending', t('待审批')], ['rejected', t('已驳回')], ['enabled', t('已启用')], ['paused', t('已暂停')], ['disabled', t('已停用')]], (x) => x[1], (x) => x[0], ['', t('状态：全部')]);
  fillSelect(selMode, [['move', t('归档并删除')], ['copy', t('仅备份')], ['purge', t('仅清理')]], (x) => x[1], (x) => x[0], ['', t('模式：全部')]);
  fillSelect(selInst, [], () => '', () => '', ['', t('实例：全部')]);

  const reload = () => { st.offset = 0; load(); };
  selStatus.addEventListener('change', () => { st.f.status = selStatus.value; reload(); });
  selMode.addEventListener('change', () => { st.f.mode = selMode.value; reload(); });
  selInst.addEventListener('change', () => { st.f.instance_id = selInst.value; reload(); });
  mine.addEventListener('change', () => { st.f.mine = mine.checked; reload(); });
  q.addEventListener('input', debounce(() => { st.f.q = q.value.trim(); reload(); }, 300));
  document.addEventListener('archive:changed', () => { if (shown) load(); });
}

function kpiBox(label, value, sub, cls) {
  const d = el('div');
  d.append(el('span', '', label), el('b', cls || '', value), el('small', '', sub || ' '));
  return d;
}

function drawKpi(k) {
  const box = $('ar-t-kpis');
  box.replaceChildren();
  if (!k) return;
  box.append(
    kpiBox(t('任务总数'), nf(k.total)),
    kpiBox(t('已启用'), nf(k.enabled), k.running ? t('其中 {n} 个正在运行', { n: k.running }) : ''),
    kpiBox(t('待审批'), nf(k.pending), k.pending_mine ? t('其中 {n} 个等我审批', { n: k.pending_mine }) : '', k.pending_mine ? 'warn' : ''),
    kpiBox(t('已暂停'), nf(k.paused), '', k.paused ? 'warn' : ''),
    kpiBox(t('累计归档行数'), nf(k.cum_rows)),
  );
  const n = $('ar-tabn-tasks');
  n.hidden = !k.pending_mine;
  n.textContent = k.pending_mine || '';
  n.title = t('等我审批的归档任务');
  $('ar-t-minelab').lastChild.textContent = ' ' + (k.pending_mine ? t('待我审批（{n}）', { n: k.pending_mine }) : t('待我审批'));
}

function lastRunCell(task) {
  const c = el('td');
  if (!task.last_run) {
    c.append(el('span', 'muted', '—'));
    return c;
  }
  c.append(resultTag(task.last_run.result), el('div', 'small muted ar-nowrap', String(task.last_run.started_at || '').slice(5, 16)));
  return c;
}

function statusCell(task) {
  const c = el('td');
  c.append(statusTag(task.status));
  if (task.status === 'pending' && task.approval) c.append(el('div', 'small muted', tp(task.approval.pending_text || '')));
  if (task.status === 'paused' && task.pause) {
    const d = el('div', 'small muted', t('{kind}：{reason}', { kind: PAUSE_KIND[task.pause.kind] || t('已暂停'), reason: clip(tp(task.pause.reason), 30) }));
    d.title = tp(task.pause.reason) || '';
    c.append(d);
  }
  if (task.running) {
    const live = el('div');
    live.dataset.taskId = String(task.id);
    live.append(el('div', 'small muted', t('运行中…')));
    liveCells.set(task.id, live);
    c.append(live);
  }
  return c;
}

function renderRows() {
  const body = $('ar-t-rows');
  body.replaceChildren();
  liveCells = new Map();
  if (!st.items.length) {
    const tr = el('tr');
    const c = el('td', 'empty', t('没有符合条件的归档任务'));
    c.colSpan = 10;
    tr.append(c);
    body.append(tr);
    return;
  }
  for (const task of st.items) {
    const tr = el('tr');
    const name = el('td');
    const nameLink = link(task.name, () => go('#task/' + task.id));
    nameLink.style.fontWeight = '600';
    name.append(nameLink, el('div', 'small muted', `${task.resource_group_name || ''} · ${task.creator || '—'}`));
    const src = el('td');
    src.append(el('div', 'small muted ar-nowrap', task.instance_name), el('code', '', `${task.db}.${task.table}`));
    const mode = el('td');
    mode.append(modeTag(task.mode));
    const bk = el('td');
    add(bk, backupTags(task));
    if (!bk.childNodes.length) bk.append(el('span', 'muted', '—'));
    const sch = el('td');
    sch.append(el('span', 'ar-nowrap', tp(task.schedule_text) || '—'), el('div', 'small muted ar-nowrap', t('窗口 {window}', { window: tp(task.window_text) || '—' })));
    const nxt = el('td');
    nxt.append(task.next_run_at ? el('span', 'ar-nowrap', String(task.next_run_at).slice(5, 16)) : el('span', 'muted', '—'));
    const cum = el('td');
    cum.append(el('span', 'ar-nowrap', nf(task.cum_rows)));
    if (task.cum_runs) cum.append(el('div', 'small muted', t('{n} 次运行', { n: task.cum_runs })));
    const ops = el('td');
    const wrap = el('div', 'ar-ops');
    wrap.append(button(t('详情'), 'ghost', () => go('#task/' + task.id)));
    add(wrap, taskButtons(task, { compact: true, done: () => { notifyChanged(); } }));
    ops.append(wrap);
    tr.append(name, src, mode, bk, sch, statusCell(task), lastRunCell(task), nxt, cum, ops);
    body.append(tr);
  }
  labelCells(body.parentNode);
}

function syncPolling() {
  runningNow = new Set(st.items.filter((t) => t.running).map((t) => t.id));
  if (runningNow.size && !releasePoll && shown) releasePoll = acquire();
  if (!runningNow.size && releasePoll) {
    releasePoll();
    releasePoll = null;
  }
}

function onProgress(list) {
  if (!shown) return;
  for (const [id, cell] of liveCells) {
    const p = byTask(list, id);
    if (p) {
      cell.replaceChildren(liveNode(p, false));
      if (!runningNow.has(id)) maybeReload();
    }
  }
  // 运行结束（之前在跑、现在进度里没有了）或别处启动了新运行：重新加载列表
  for (const id of runningNow) if (!byTask(list, id)) maybeReload();
  for (const p of list || []) if (!runningNow.has(p.task_id) && st.items.some((t) => t.id === p.task_id)) maybeReload();
}

function maybeReload() {
  if (Date.now() - lastReload < 5000) return;
  load();
}

export async function load() {
  if (st.loading) {
    st.again = true;
    return;
  }
  st.loading = true;
  lastReload = Date.now();
  try {
    const res = await A.tasks({ ...st.f, mine: st.f.mine, offset: st.offset, limit: PAGE });
    st.items = res.data || [];
    st.total = res.total || 0;
    drawKpi(res.kpi);
    if (res.instances) {
      const sel = $('ar-t-inst');
      const cur = st.f.instance_id;
      fillSelect(sel, res.instances, (i) => i.name, (i) => i.id, ['', t('实例：全部')], cur);
    }
    renderRows();
    $('ar-t-total').textContent = t('共 {n} 个任务', { n: st.total });
    $('ar-t-prev').disabled = st.offset <= 0;
    $('ar-t-next').disabled = st.offset + PAGE >= st.total;
    syncPolling();
    applyButtons(root());
  } catch (e) {
    if (e.code === 'not_ready') return;
    flash(e.message, 'err');
  } finally {
    st.loading = false;
    if (st.again) {
      st.again = false;
      load();
    }
  }
}

export function show() {
  build();
  shown = true;
  root().hidden = false;
  if (!unsub) unsub = subscribe(onProgress);
  load();
  clearInterval(slowTimer);
  slowTimer = setInterval(() => { if (shown && !document.hidden) load(); }, 30000);
}

export function hide() {
  shown = false;
  root().hidden = true;
  clearInterval(slowTimer);
  if (releasePoll) {
    releasePoll();
    releasePoll = null;
  }
  if (unsub) {
    unsub();
    unsub = null;
  }
}

export const refresh = () => load();
