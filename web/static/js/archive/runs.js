/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: runs.js
 * @time: 2026/10/28
 */
// 运行记录页签：每次运行一行（定时 / 手动 / 恢复 / 试运行），筛选、分页；运行中的行定时刷新。
import { t } from '../i18n.js';
import { A } from './api.js';
import { $, el, button, link, fillSelect, nf, fmtDur, fmtTime, debounce, go, labelCells } from './util.js';
import { flash } from './ui.js';
import { resultTag, RUN_RESULT, TRIGGER } from './consts.js';

const PAGE = 50;
const root = () => $('ar-view-runs');
const st = { built: false, f: { task_id: '', trigger: '', result: '', q: '' }, offset: 0, total: 0, items: [], loading: false, taskOptsLoaded: false };
let shown = false;
let timer = 0;

function build() {
  if (st.built) return;
  st.built = true;
  const r = root();
  const tb = el('div', 'toolbar');
  const selTask = el('select');
  selTask.id = 'ar-r-task';
  selTask.setAttribute('aria-label', t('任务'));
  const selTrg = el('select');
  selTrg.id = 'ar-r-trg';
  selTrg.setAttribute('aria-label', t('触发方式'));
  const selRes = el('select');
  selRes.id = 'ar-r-res';
  selRes.setAttribute('aria-label', t('结果'));
  const q = el('input');
  q.type = 'search';
  q.id = 'ar-r-q';
  q.placeholder = t('搜索任务 / 库表 / 操作人');
  q.setAttribute('aria-label', t('搜索'));
  q.style.minWidth = '220px';
  const cnt = el('span', 'small muted');
  cnt.id = 'ar-r-count';
  tb.append(selTask, selTrg, selRes, q, el('span', 'spacer'), cnt);
  r.append(tb);

  fillSelect(selTask, [], () => '', () => '', ['', t('任务：全部')]);
  fillSelect(selTrg, Object.entries(TRIGGER), (x) => x[1], (x) => x[0], ['', t('触发方式：全部')]);
  fillSelect(selRes, Object.entries(RUN_RESULT), (x) => x[1][0], (x) => x[0], ['', t('结果：全部')]);

  const card = el('section', 'card');
  const table = el('table', 'list ar-cards');
  const th = el('thead');
  const hr = el('tr');
  [t('运行'), t('任务'), t('开始 / 结束'), t('触发'), t('结果'), t('扫描 / 备份 / 删除'), t('批数'), t('耗时'), t('峰值 Threads'), t('限流'), ''].forEach((title) => hr.append(el('th', '', title)));
  th.append(hr);
  const body = el('tbody');
  body.id = 'ar-r-rows';
  table.append(th, body);
  const pager = el('div', 'ar-pager');
  const prev = button(t('上一页'), 'ghost', () => { st.offset = Math.max(0, st.offset - PAGE); load(); });
  prev.id = 'ar-r-prev';
  const next = button(t('下一页'), 'ghost', () => { st.offset += PAGE; load(); });
  next.id = 'ar-r-next';
  pager.append(el('span', 'spacer'), prev, next);
  const wrap = el('div', 'ar-tbl-wrap');
  wrap.append(table);
  card.append(wrap, pager);
  r.append(card);

  const reload = () => { st.offset = 0; load(); };
  selTask.addEventListener('change', () => { st.f.task_id = selTask.value; reload(); });
  selTrg.addEventListener('change', () => { st.f.trigger = selTrg.value; reload(); });
  selRes.addEventListener('change', () => { st.f.result = selRes.value; reload(); });
  q.addEventListener('input', debounce(() => { st.f.q = q.value.trim(); reload(); }, 300));
  document.addEventListener('archive:changed', () => { if (shown) load(); });
}

function cells(r) {
  const tr = el('tr');
  const id = el('td');
  const a = link('#' + r.id, () => go('#run/' + r.id));
  a.style.fontWeight = '600';
  id.append(a);
  const task = el('td');
  if (r.task_id) task.append(link(r.task_name, () => go('#task/' + r.task_id)));
  else task.append(r.task_name || t('（未提交的草稿）'));
  task.append(el('div', 'small muted'));
  task.lastChild.append(el('code', '', r.table));
  const time = el('td');
  time.append(el('span', 'ar-nowrap small', fmtTime(r.started_at, 16)), el('div', 'small muted ar-nowrap', r.result === 'running' || r.result === 'paused' ? t('进行中') : fmtTime(r.finished_at, 16).slice(5)));
  const trg = el('td');
  trg.append(document.createTextNode(TRIGGER[r.trigger] || r.trigger), el('div', 'small muted', r.by || ''));
  const res = el('td');
  res.append(resultTag(r.result));
  const dry = r.trigger === 'dryrun';
  const noLoad = dry || r.trigger === 'restore';
  const rows = el('td');
  if (dry) rows.append(el('span', 'muted', t('预估 {n}', { n: nf(r.scanned) })));
  else rows.append(el('span', 'ar-nowrap', `${nf(r.scanned)} / ${nf(r.backed)} / ${nf(r.deleted)}`));
  const thr = el('td');
  if (noLoad) thr.append('—');
  else thr.append(String(r.peak_threads));
  const thc = el('td');
  if (noLoad) thc.append('—');
  else thc.append(r.throttle_count ? el('span', 'ar-tag warn', r.throttle_count) : '0');
  const ops = el('td');
  ops.append(button(t('详情'), 'ghost', () => go('#run/' + r.id)));
  tr.append(id, task, time, trg, res, rows, el('td', '', dry ? '—' : nf(r.batches)), el('td', '', r.result === 'running' || r.result === 'paused' ? '—' : fmtDur(r.duration_ms)), thr, thc, ops);
  return tr;
}

function render() {
  const body = $('ar-r-rows');
  body.replaceChildren();
  if (!st.items.length) {
    const tr = el('tr');
    const c = el('td', 'empty', t('没有符合条件的运行记录'));
    c.colSpan = 11;
    tr.append(c);
    body.append(tr);
  } else for (const r of st.items) body.append(cells(r));
  labelCells(body.parentNode);
  $('ar-r-count').textContent = t('共 {n} 条', { n: st.total });
  $('ar-r-prev').disabled = st.offset <= 0;
  $('ar-r-next').disabled = st.offset + PAGE >= st.total;
}

async function loadTaskOptions() {
  if (st.taskOptsLoaded) return;
  st.taskOptsLoaded = true;
  try {
    const res = await A.tasks({ limit: 200 });
    fillSelect($('ar-r-task'), res.data || [], (x) => x.name, (x) => x.id, ['', t('任务：全部')], st.f.task_id);
  } catch (_) {
    st.taskOptsLoaded = false;
  }
}

export async function load() {
  if (st.loading) {
    st.again = true;
    return;
  }
  st.loading = true;
  try {
    const res = await A.runs({ ...st.f, offset: st.offset, limit: PAGE });
    st.items = res.data || [];
    st.total = res.total || 0;
    render();
    clearTimeout(timer);
    if (shown && st.items.some((r) => r.result === 'running' || r.result === 'paused')) timer = setTimeout(() => { if (shown && !document.hidden) load(); else if (shown) timer = setTimeout(load, 3000); }, 3000);
  } catch (e) {
    if (e.code !== 'not_ready') flash(e.message, 'err');
  } finally {
    st.loading = false;
    if (st.again) {
      st.again = false;
      load();
    }
  }
}

// query 里的 task=ID 用于“查看全部”跳转
export async function show(query) {
  build();
  shown = true;
  root().hidden = false;
  const tid = query && query.task ? String(query.task) : '';
  if (tid !== st.f.task_id) {
    st.f.task_id = tid;
    st.offset = 0;
  }
  await loadTaskOptions();
  $('ar-r-task').value = st.f.task_id;
  load();
}

export function hide() {
  shown = false;
  clearTimeout(timer);
  root().hidden = true;
}

export const refresh = () => load();
