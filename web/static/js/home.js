/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: home.js
 * @time: 2026/10/28
 */
// 首页：待办页签、性能工单（KPI 与待办列表）、实例健康（最近一次巡检的缓存结果，不现场连库）。
// 性能工单、巡检由各自模块的接口提供：接口 403 / 404 / 失败（没有权限或模块没启用）时静默去掉对应的块，不影响首页其余部分。
// 往页面写内容一律用 textContent / createElement，不拼 innerHTML。
import { api } from './common.js';
import { t, tp } from './i18n.js';

const $ =(id) => document.getElementById(id);

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

// ---- 待办页签 ----

function selectTab(name) {
  for (const t of document.querySelectorAll('.home-tabs .tab')) {
    const on = t.dataset.tab === name;
    t.classList.toggle('on', on);
    t.setAttribute('aria-selected', String(on));
    const panel = $('todo-' + t.dataset.tab);
    if (panel) panel.hidden = !on;
  }
}

for (const t of document.querySelectorAll('.home-tabs .tab')) t.addEventListener('click', () => selectTab(t.dataset.tab));

// 去掉某个待办页签（及其内容）；一个都不剩时去掉整张卡片
function dropTab(name) {
  const tab = document.querySelector(`.home-tabs .tab[data-tab="${name}"]`);
  const panel = $('todo-' + name);
  const wasOn = tab && tab.classList.contains('on');
  if (tab) tab.remove();
  if (panel) panel.remove();
  const left = document.querySelectorAll('.home-tabs .tab');
  if (!left.length) {
    const card = $('todo');
    if (card) card.remove();
  } else if (wasOn) {
    selectTab(left[0].dataset.tab);
  }
}

// ---- 待办总数（标题旁的 todo-total）----
// 总数 = 待我审批（SQL 上线 / 查询权限 / 数据归档，服务端算好写在 cnt-approve）+ 性能工单待办（异步加载，写在 cnt-perf）。
// “我提交的”只是跟踪进度，不计入。性能工单还没加载出来（显示 …）时先只算已知部分，加载后再更新。

function countOf(id) {
  const e = $(id);
  const n = e ? Number(String(e.textContent).trim()) : NaN;
  return Number.isFinite(n) ? n : 0;
}

function updateTodoTotal() {
  const box = $('todo-total');
  if (!box) return;
  const perfLoading = !!$('cnt-perf') && String($('cnt-perf').textContent).trim() === '…';
  const n = countOf('cnt-approve') + countOf('cnt-perf');
  box.textContent = n > 0
    ? (perfLoading ? t('· 共 {n} 项待处理（性能工单加载中）', { n }) : t('· 共 {n} 项待处理', { n }))
    : (perfLoading ? '' : t('· 没有待处理事项'));
}

// ---- KPI 卡片 ----

function setKPI(id, value, hint, warn) {
  const k = $(id);
  if (!k) return;
  k.querySelector('[data-role="v"]').textContent = value;
  k.querySelector('[data-role="d"]').textContent = hint;
  k.classList.toggle('warn', !!warn);
}

// ---- 性能工单 ----

async function perf() {
  if (!$('todo-perf') && !$('k-perf')) return;
  let td;
  let list;
  try {
    [td, list] = await Promise.all([api('GET', '/api/v1/perf/my-todo'), api('GET', '/api/v1/perf/issues?view=todo&limit=8')]);
  } catch (_) {
    const k = $('k-perf');
    if (k) k.remove();
    dropTab('perf');
    updateTodoTotal();
    return;
  }
  td = td || { todo: 0, overdue: 0 };
  setKPI('k-perf', String(td.todo || 0), td.overdue > 0 ? t('其中 {n} 个已超时', { n: td.overdue }) : t('无超时'), td.overdue > 0);
  const cnt = $('cnt-perf');
  if (cnt) cnt.textContent = String(td.todo || 0);
  updateTodoTotal();
  const box = $('todo-perf');
  if (!box) return;
  box.replaceChildren();
  const rows = Array.isArray(list) ? list : [];
  if (!rows.length) {
    box.append(el('p', 'empty-note', t('没有需要你处理的性能工单')));
    return;
  }
  for (const r of rows) {
    const a = el('a', 'todo-row');
    a.href = '/perf/' + encodeURIComponent(r.id);
    const sev = el('span', 'sev ' + String(r.severity || '').toLowerCase(), r.severity || '—');
    const main = el('span', 'todo-main');
    const fp = String(r.fingerprint || '').replace(/\s+/g, ' ');
    main.append(el('b', '', `${r.instance || ''}${r.db_name ? ' / ' + r.db_name : ''}`),
      el('small', 'muted', `${tp(r.status_label) || r.status || ''}${r.overdue ? ' · ' + t('已超时') : ''}${fp ? ' · ' + (fp.length > 60 ? fp.slice(0, 60) + '…' : fp) : ''}`));
    const act = el('span', 'todo-act');
    if (r.overdue) act.append(el('span', 'st rejected', t('需处理')));
    else act.textContent = t('查看 →');
    a.append(sev, main, act);
    box.append(a);
  }
  if (td.todo > rows.length) {
    const more = el('p', 'more-note muted small');
    more.append(t('还有 {n} 项，见', { n: td.todo - rows.length }) + ' ');
    const l = el('a', '', t('性能工单'));
    l.href = '/perf';
    more.append(l, ' ' + t('列表|lists'));
    box.append(more);
  }
}

// ---- 实例健康：取最近一次巡检结果（缓存，不连库）----

function healthOf(r) {
  if (r.status === 'failed') return ['bad', t('巡检失败')];
  if (r.alert > 0) return ['bad', t('告警 {n} 项', { n: r.alert })];
  if (r.attention > 0 || r.failed > 0) return ['warn', t('关注 {n} 项', { n: r.attention || r.failed })];
  return ['ok', r.health_score !== null && r.health_score !== undefined ? t('健康分 {n}', { n: r.health_score }) : t('正常')];
}

async function inspect() {
  const list = $('inst-list');
  if (!list) return;
  let rows;
  try {
    // latest=1：服务端按实例取最近一次结束（成功或失败）的巡检，每个实例一份，不会被条数上限截断
    rows = await api('GET', '/api/v1/inspection/reports?latest=1&limit=2000');
  } catch (_) {
    return; // 没有巡检权限或模块没启用：保持灰点
  }
  if (!Array.isArray(rows)) return;
  const latest = new Map();
  for (const r of rows) latest.set(r.instance_id, r);
  const link = $('insp-link');
  if (link) link.hidden = false;
  for (const li of list.querySelectorAll('li[data-inst]')) {
    const r = latest.get(Number(li.dataset.inst));
    if (!r) continue;
    const [cls, text] = healthOf(r);
    li.querySelector('.dot').className = 'dot ' + cls;
    li.querySelector('[data-role="st"]').textContent = text;
    li.querySelector('[data-role="st"]').className = 'in-st small ' + (cls === 'bad' ? 'crit' : 'muted');
  }
  // KPI：我可见的实例里，最近一次巡检有告警 / 巡检失败的个数（这是巡检结果，不是实例当前的连通状态）
  const sec = $('home-insts');
  const ids = String((sec && sec.dataset.ids) || '').split(',').filter(Boolean).map(Number);
  let seen = 0;
  let alert = 0;
  let failed = 0;
  for (const id of ids) {
    const r = latest.get(id);
    if (!r) continue;
    seen += 1;
    if (r.status === 'failed') failed += 1;
    else if (r.alert > 0) alert += 1;
  }
  const k = $('k-inst');
  const total = k ? k.querySelector('[data-role="v"]').textContent : '';
  if (!total) return;
  let hint;
  if (!seen) hint = t('尚无巡检数据');
  else if (alert && failed) hint = t('{a} 个有巡检告警，{f} 个巡检失败', { a: alert, f: failed });
  else if (alert) hint = t('{n} 个实例有巡检告警', { n: alert });
  else if (failed) hint = t('{n} 个实例巡检失败', { n: failed });
  else if (seen < ids.length) hint = t('已巡检的 {n} 个全部正常', { n: seen });
  else hint = t('巡检全部正常');
  setKPI('k-inst', total, hint, alert + failed > 0);
  k.title = t('按每个实例最近一次巡检统计：{seen} / {total} 个实例有巡检数据', { seen, total: ids.length });
  // 有告警 / 失败时，点进去直接看这些实例最近一次的巡检结果
  if (alert || failed) k.href = '/inspection?latest=1&level=' + (alert ? 'alert' : 'failed');
}

updateTodoTotal();
perf();
inspect();
