/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: list.js
 * @time: 2026/10/28
 */
// 性能巡检列表：巡检报告（筛选、立即巡检、批量取消 / 删除、进行中自动刷新）与定时巡检计划（多计划）。
import { api, confirmRun } from '../common.js';
import { t, tp } from '../i18n.js';
import { $, el, btn, setMsg, fmtTime, fmtElapsed, statusBadge, countBadge, scoreEl, apiFull, openModal, closeModal, bindModalClose, SEP, SEMI } from './util.js';
import { applyRunFilter } from './runfilter.js';

const BASE = '/api/v1/inspection';
const SIZE = 20;
const WEEK = [t('日|inspect_weekday'), t('一|inspect_weekday'), t('二|inspect_weekday'), t('三|inspect_weekday'), t('四|inspect_weekday'), t('五|inspect_weekday'), t('六|inspect_weekday')];

let opts = null; // 服务端返回的选项（实例、标签、渠道、权限）
let page = 0;
let total = 0;
let rows = [];
const selected = new Set();
let pollTimer = null;
let linkFilter = { plan_id: '', run_id: '', latest: false }; // 从“运行记录”跳转过来时只看某次运行的报告；从首页跳转过来时只看每个实例最近一次的报告


// ---- 初始化 ----

async function init() {
  bindModalClose();
  const q = new URLSearchParams(location.search);
  linkFilter = { plan_id: q.get('plan_id') || '', run_id: q.get('run_id') || '', latest: q.get('latest') === '1' };
  try {
    opts = await api('GET', BASE + '/options');
  } catch (e) {
    setMsg($('msg'), e.message);
    return;
  }
  const sel = $('f-instance');
  for (const i of opts.instances) sel.append(new Option(`${i.label}：${i.name}`, String(i.id)));
  $('btn-run').hidden = !opts.can_run;
  $('btn-delete').hidden = !opts.can_delete;
  $('btn-cancel').hidden = !opts.can_cancel;
  $('btn-plan-new').hidden = !opts.can_plan;
  $('f-filter').addEventListener('submit', (e) => e.preventDefault());
  if (linkFilter.run_id || linkFilter.plan_id) {
    const box = el('div', 'alert info', linkFilter.run_id ? t('仅显示计划运行 #{id} 产生的报告。', { id: linkFilter.run_id }) : t('仅显示计划 #{id} 产生的报告。', { id: linkFilter.plan_id }));
    const all = el('a', '', t('查看全部报告'));
    all.href = '/inspection';
    box.append(document.createTextNode(' '), all);
    $('msg').append(box);
  }
  if (linkFilter.latest) {
    const box = el('div', 'alert info', t('仅显示每个实例最近一次巡检（成功或失败）的结果。'));
    const all = el('a', '', t('查看全部报告'));
    all.href = '/inspection';
    box.append(document.createTextNode(' '), all);
    $('msg').append(box);
  }
  const wantLevel = q.get('level') || ''; // 首页“我可见的实例”卡片带过来的级别（有告警 / 巡检失败）
  if (wantLevel && [...$('f-level').options].some((o) => o.value === wantLevel)) $('f-level').value = wantLevel;
  for (const id of ['f-instance', 'f-level', 'f-trigger', 'f-start', 'f-end']) {
    $(id).addEventListener('change', () => { page = 0; selected.clear(); loadReports(); });
  }
  $('pg-prev').addEventListener('click', () => { if (page > 0) { page--; loadReports(); } });
  $('pg-next').addEventListener('click', () => { if ((page + 1) * SIZE < total) { page++; loadReports(); } });
  $('chk-all').addEventListener('change', (e) => {
    for (const r of rows) { if (e.target.checked) selected.add(r.ref); else selected.delete(r.ref); }
    renderReports();
  });
  $('btn-run').addEventListener('click', openRun);
  $('btn-cancel').addEventListener('click', () => batch('cancel'));
  $('btn-delete').addEventListener('click', () => batch('delete'));
  $('tab-reports').addEventListener('click', () => switchTab('reports'));
  $('tab-plans').addEventListener('click', () => switchTab('plans'));
  $('btn-plan-new').addEventListener('click', () => editPlan(null));
  initPlanModal();
  initRunModal();
  loadReports();
  loadPlans();
}

function switchTab(name) {
  $('pane-reports').hidden = name !== 'reports';
  $('pane-plans').hidden = name !== 'plans';
  for (const [id, on] of [['tab-reports', name === 'reports'], ['tab-plans', name === 'plans']]) {
    $(id).classList.toggle('on', on);
    $(id).setAttribute('aria-selected', String(on));
  }
}

// ---- 报告列表 ----

function reportQuery() {
  const p = new URLSearchParams({ offset: String(page * SIZE), limit: String(SIZE) });
  for (const [k, id] of [['instance_id', 'f-instance'], ['level', 'f-level'], ['trigger', 'f-trigger'], ['start_date', 'f-start'], ['end_date', 'f-end']]) {
    if ($(id).value) p.set(k, $(id).value);
  }
  if (linkFilter.plan_id) p.set('plan_id', linkFilter.plan_id);
  if (linkFilter.run_id) p.set('run_id', linkFilter.run_id);
  if (linkFilter.latest) p.set('latest', '1');
  return p.toString();
}

async function loadReports(silent = false) {
  clearTimeout(pollTimer);
  try {
    const res = await apiFull(BASE + '/reports?' + reportQuery());
    rows = res.data || [];
    total = res.total || 0;
    renderReports();
  } catch (e) {
    if (!silent) setMsg($('msg'), e.message);
  }
  if (rows.some((r) => r.status === 'running')) pollTimer = setTimeout(() => loadReports(true), 5000);
}

function td(...children) {
  const cell = el('td');
  for (const c of children) cell.append(c instanceof Node ? c : document.createTextNode(String(c)));
  return cell;
}

function triggerText(r) {
  if (r.trigger === 'schedule') return r.plan_name ? t('定时 · {name}', { name: r.plan_name }) : t('定时');
  return r.operator ? t('手动 · {name}', { name: r.operator }) : t('手动');
}

function renderReports() {
  const body = $('rows');
  body.replaceChildren();
  if (!rows.length) {
    const tr = el('tr');
    const c = el('td', 'empty', t('没有符合条件的巡检报告。'));
    c.colSpan = 13;
    tr.append(c);
    body.append(tr);
  }
  for (const r of rows) {
    const tr = el('tr', r.status === 'failed' ? 'lv-err' : '');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = selected.has(r.ref);
    cb.setAttribute('aria-label', t('选择报告 {ref}', { ref: r.ref }));
    cb.addEventListener('change', () => { if (cb.checked) selected.add(r.ref); else selected.delete(r.ref); updateBatch(); });
    const running = r.status === 'running';
    const errText = r.error ? tp(r.error) : '';
    const failedMsg = r.status === 'failed' && r.error ? el('small', 'muted', errText.length > 40 ? errText.slice(0, 40) + '…' : errText) : null;
    const st = statusBadge(r);
    if (failedMsg) { st.append(document.createTextNode(' '), failedMsg); }
    tr.append(td(cb), td('#' + r.id), td(r.instance_name), td(r.type_label), td(fmtTime(r.start_time)), td(triggerText(r)), td(st),
      td((r.kind === 'mysql' || r.kind === 'redis') && r.status === 'finish' ? scoreEl(r.health_score) : '–'),
      td(running || r.status === 'failed' ? '–' : countBadge(r.alert, 'crit')), td(running || r.status === 'failed' ? '–' : countBadge(r.attention, 'warn')),
      td(running || r.status === 'failed' ? '–' : String(r.failed)), td(fmtElapsed(r.elapsed)));
    for (const i of [8, 9, 10]) tr.children[i].classList.add('num');
    const ops = el('td', 'row-actions');
    const view = el('a', 'btn', t('查看报告'));
    view.href = '/inspection/' + r.ref;
    if (running) { view.setAttribute('aria-disabled', 'true'); view.style.pointerEvents = 'none'; view.style.opacity = '.45'; }
    ops.append(view, document.createTextNode(' '));
    if (running && opts.can_cancel) ops.append(btn(t('取消'), 'ghost', () => act('cancel', [r.ref])), document.createTextNode(' '));
    if ((r.status === 'failed' || r.status === 'cancelled') && opts.can_run) ops.append(btn(t('重新巡检'), '', () => rerun(r)), document.createTextNode(' '));
    if (!running && opts.can_delete) ops.append(btn(t('删除'), 'ghost danger', () => act('delete', [r.ref])));
    tr.append(ops);
    body.append(tr);
  }
  const pages = Math.max(1, Math.ceil(total / SIZE));
  $('pager-text').textContent = t('共 {total} 条，第 {page} / {pages} 页', { total, page: page + 1, pages });
  $('pg-prev').disabled = page === 0;
  $('pg-next').disabled = (page + 1) * SIZE >= total;
  $('chk-all').checked = rows.length > 0 && rows.every((r) => selected.has(r.ref));
  updateBatch();
}

function updateBatch() {
  const cur = rows.filter((r) => selected.has(r.ref));
  const nRun = cur.filter((r) => r.status === 'running').length;
  const nDel = cur.length - nRun;
  $('btn-cancel').textContent = t('批量取消 ({n})', { n: nRun });
  $('btn-cancel').disabled = nRun === 0;
  $('btn-delete').textContent = t('批量删除 ({n})', { n: nDel });
  $('btn-delete').disabled = nDel === 0;
}

function batch(kind) {
  const cur = rows.filter((r) => selected.has(r.ref));
  const refs = cur.filter((r) => (kind === 'cancel') === (r.status === 'running')).map((r) => r.ref);
  if (refs.length) act(kind, refs);
}

async function act(kind, refs) {
  const targets = rows.filter((r) => refs.includes(r.ref));
  const names = targets.map((r) => '#' + r.id);
  const listText = names.length > 8 ? names.slice(0, 8).join(SEP) + t(' 等 {n} 份', { n: names.length }) : names.join(SEP);
  const isCancel = kind === 'cancel';
  const copts = isCancel ? {
    title: t('取消巡检'), code: 'inspect.cancel', codeName: t('取消进行中的巡检'),
    lines: [t('确定取消 {n} 份进行中的巡检吗？已采集的部分会保留。', { n: refs.length })],
    details: [{ k: t('报告'), v: listText }], okText: t('确认取消巡检'), cancelText: t('返回'),
  } : {
    title: t('删除 {n} 份巡检报告', { n: refs.length }), level: 'high', code: 'inspect.delete', codeName: t('删除报告'),
    lines: [t('删除后不可恢复（报告内容与检查项一并删除），操作会写入操作日志。')],
    details: [{ k: t('报告'), v: listText }],
    typed: refs.length === 1 && targets.length === 1 ? '#' + targets[0].id : String(refs.length),
    typedLabel: refs.length === 1 ? t('请输入报告标识确认删除：') : t('共删除 {n} 份，请输入份数确认：', { n: refs.length }), okText: t('确认删除'),
  };
  const cr = await confirmRun(copts, () => api('POST', `${BASE}/reports/${kind}`, { ids: refs }));
  if (!cr.ok) return;
  try {
    const res = cr.result;
    refs.forEach((r) => selected.delete(r));
    if (kind === 'cancel') setMsg($('msg'), t('已取消 {n} 个；正在采集的项结束后停止', { n: res.cancelled }) + (res.skipped ? t('，另 {n} 个已结束、跳过', { n: res.skipped }) : ''), 'info');
    else setMsg($('msg'), t('已删除 {n} 个', { n: res.deleted }) + (res.running ? t('，{n} 个进行中未删除，请先取消', { n: res.running }) : ''), 'info');
    await loadReports(true);
  } catch (e) {
    setMsg($('msg'), e.message);
  }
}

async function rerun(r) {
  await submitRun([r.instance_id]);
}

async function submitRun(ids, bigkey = false) {
  try {
    const res = await api('POST', BASE + '/reports/run', { instance_ids: ids, redis_bigkey: bigkey });
    const parts = [t('已提交 {n} 个实例的巡检', { n: res.created.length })];
    if (res.busy.length) parts.push(t('{names} 已有进行中的巡检，跳过', { names: res.busy.join(SEP) }));
    if (res.unsupported.length) parts.push(t('{names} 的类型暂不支持巡检', { names: res.unsupported.join(SEP) }));
    if (res.disabled && res.disabled.length) parts.push(t('{names} 已在实例管理里停用，跳过', { names: res.disabled.join(SEP) }));
    if (res.not_found.length) parts.push(t('{n} 个实例不存在或无权限', { n: res.not_found.length }));
    setMsg($('msg'), parts.join(SEMI), res.created.length ? 'info' : 'warn');
    page = 0;
    await loadReports(true);
    return true;
  } catch (e) {
    setMsg($('msg'), e.message);
    return false;
  }
}

// ---- 立即巡检弹窗 ----

function groupedInstances(list) {
  const groups = [['mssql', 'SQL Server'], ['mysql', t('MySQL / 兼容类型')], ['redis', 'Redis']];
  return groups.map(([k, name]) => [name, list.filter((i) => i.kind === k)]).filter(([, l]) => l.length);
}

function instLabel(i) {
  return i.db_type === 'mysql' || i.db_type === 'mssql' || i.db_type === 'redis' ? i.name : t('{name}（{label}）', { name: i.name, label: i.label });
}

function initRunModal() {
  const list = $('run-list');
  // 搜索：输入即筛选；“全选”只选当前搜索结果里的，“清空”清掉全部已选；已选但被筛掉的实例仍然会被巡检（计数里含它们）
  const refresh = () => {
    const n = list.querySelectorAll('input:checked').length;
    const hidden = [...list.querySelectorAll('label[hidden] input:checked')].length;
    $('run-go').textContent = t('开始巡检（{n}）', { n });
    const f = { shown: list.querySelectorAll('label:not([hidden])').length, total: list.querySelectorAll('label').length };
    $('run-count').textContent = $('run-q').value.trim() ? t('匹配 {shown} / {total}', { shown: f.shown, total: f.total }) + (hidden ? t('，另有 {n} 个已选在结果之外', { n: hidden }) : '') : '';
  };
  const filter = () => {
    const f = applyRunFilter(list, $('run-q').value);
    list.querySelector('.no-match')?.remove();
    if (!f.shown && f.total) list.append(el('span', 'hint no-match', t('没有匹配的实例')));
    refresh();
  };
  $('run-q').addEventListener('input', filter);
  $('run-q').addEventListener('keydown', (e) => { if (e.key === 'Enter') e.preventDefault(); });
  list.addEventListener('change', refresh);
  $('run-all').addEventListener('click', () => { list.querySelectorAll('label:not([hidden]) input').forEach((c) => { c.checked = true; }); refresh(); });
  $('run-none').addEventListener('click', () => { list.querySelectorAll('input').forEach((c) => { c.checked = false; }); refresh(); });
  $('run-go').addEventListener('click', async () => {
    const ids = [...list.querySelectorAll('input:checked')].map((c) => Number(c.value));
    if (!ids.length) { setMsg($('msg'), t('请选择要巡检的实例')); closeModal('m-run'); return; }
    const bigkey = $('run-bigkey').checked;
    closeModal('m-run');
    await submitRun(ids, bigkey);
  });
}

function openRun() {
  const list = $('run-list');
  list.replaceChildren();
  for (const [name, items] of groupedInstances(opts.instances)) {
    list.append(el('b', 'grp', name));
    for (const i of items) {
      const lb = el('label');
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.value = String(i.id);
      lb.append(cb, document.createTextNode(instLabel(i)));
      list.append(lb);
    }
  }
  if (!opts.instances.length) list.append(el('span', 'hint', t('没有可巡检的实例')));
  $('run-bigkey-row').hidden = !opts.instances.some((i) => i.kind === 'redis');
  $('run-bigkey').checked = false;
  $('run-go').textContent = t('开始巡检（{n}）', { n: 0 });
  $('run-q').value = '';
  $('run-count').textContent = '';
  openModal('m-run');
  $('run-q').focus();
}

// ---- 定时计划列表 ----

let plans = [];

async function loadPlans() {
  try {
    plans = await api('GET', BASE + '/plans');
    setMsg($('plan-msg'), '');
  } catch (e) {
    plans = [];
    setMsg($('plan-msg'), e.message, e.code === 'table_missing' ? 'warn' : 'err');
  }
  renderPlans();
}

function notifyText(p) {
  if (p.notify_mode === 'none') return t('不通知');
  const names = p.channel_names.length ? p.channel_names.join(SEP) : '—';
  return p.notify_mode === 'alert' ? t('有告警时 → {names}', { names }) : t('每次完成 → {names}', { names });
}

function lastRunCell(p) {
  const c = el('td', 'small');
  if (p.running) { c.append(el('span', 'st running', t('运行中'))); return c; }
  const r = p.last_run;
  if (!r) { c.classList.add('muted'); c.textContent = '—'; return c; }
  c.append(document.createTextNode(fmtTime(r.started_at) + ' · '));
  const [cls, text] = r.status === 'failed' ? ['rejected', t('失败')]
    : r.failed > 0 ? ['rejected', t('{ok} 成功 {failed} 失败', { ok: r.ok, failed: r.failed })]
      : r.status === 'skipped' ? ['aborted', t('已跳过')] : ['passed', t('{ok} 成功', { ok: r.ok })];
  c.append(el('span', 'st ' + cls, text));
  return c;
}

function renderPlans() {
  const body = $('plan-rows');
  body.replaceChildren();
  $('plan-count').textContent = t('{n} 个计划', { n: plans.length });
  for (const p of plans) {
    const tr = el('tr');
    const inst = el('td');
    inst.append(document.createTextNode(t('{n} 个实例', { n: p.instance_total })));
    const names = [...p.instance_names];
    if (p.tag_codes.length) names.push(t('标签：{names}', { names: p.tag_codes.join(SEP) }));
    if (names.length) inst.append(document.createTextNode(' '), el('small', 'muted', names.join(SEP) + (p.instance_ids.length > p.instance_names.length ? ' …' : '')));
    if (p.hidden) inst.append(document.createTextNode(' '), el('small', 'muted', t('（另有 {n} 个你没有权限看到）', { n: p.hidden })));
    const name = el('td');
    name.append(el('b', '', p.name));
    const sw = el('td');
    const lb = el('label', 'sw');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = p.enabled;
    cb.disabled = !opts.can_plan_toggle;
    cb.setAttribute('aria-label', t('启用计划 {name}', { name: p.name }));
    cb.addEventListener('change', async () => {
      try { await api('POST', `${BASE}/plans/${p.id}/enable`, { enabled: cb.checked }); await loadPlans(); } catch (e) { setMsg($('plan-msg'), e.message); cb.checked = !cb.checked; }
    });
    lb.append(cb, document.createElement('i'));
    sw.append(lb, document.createTextNode(' ' + (p.enabled ? t('启用|state') : t('已暂停'))));
    const next = el('td', 'small' + (p.enabled ? '' : ' muted'), p.enabled ? fmtTime(p.next_run_at) : t('已暂停'));
    const ops = el('td', 'row-actions');
    if (opts.can_plan) ops.append(btn(t('编辑'), '', () => editPlan(p)), document.createTextNode(' '));
    ops.append(btn(t('运行记录'), '', () => showRuns(p)), document.createTextNode(' '));
    if (opts.can_plan_run) ops.append(btn(t('立即运行'), '', () => runPlan(p)), document.createTextNode(' '));
    if (opts.can_plan_toggle) ops.append(btn(t('删除'), 'ghost danger', () => deletePlan(p)));
    const gap = p.stagger_seconds ? t('间隔 {n} 秒', { n: p.stagger_seconds }) : t('无间隔');
    tr.append(name, inst, td(tp(p.describe)), td(t('并发 {n} · {gap}', { n: p.concurrency, gap })), td(notifyText(p)),
      td(t('{n} 天', { n: p.retention_days })), sw, lastRunCell(p), next, ops);
    body.append(tr);
  }
  if (!body.children.length) {
    const tr = el('tr');
    const c = el('td', 'empty', opts.plans_ready ? t('还没有定时巡检计划。') : t('定时巡检的表尚未创建（需执行 deploy/sql/013_inspect.sql）'));
    c.colSpan = 10;
    tr.append(c);
    body.append(tr);
  }
}

async function runPlan(p) {
  const cr = await confirmRun({
    title: t('立即运行计划「{name}」', { name: p.name }), level: 'high', code: 'inspect.plan_run', codeName: t('立即运行定时计划'),
    lines: [t('将立即按计划的设置对 {n} 个实例开始巡检（巡检只读）；已有进行中巡检的实例会被跳过。', { n: p.instance_total }), t('通知：{text}', { text: notifyText(p) })],
    details: [{ k: t('计划'), v: p.name }, { k: t('实例数'), v: p.instance_total }], okText: t('确认运行'),
  }, () => api('POST', `${BASE}/plans/${p.id}/run`));
  if (!cr.ok) return;
  setMsg($('plan-msg'), t('计划「{name}」已开始运行，报告会陆续出现在“巡检报告”里。', { name: p.name }), 'info');
  setTimeout(loadPlans, 1500);
}

async function deletePlan(p) {
  const cr = await confirmRun({
    title: t('删除计划「{name}」', { name: p.name }), level: 'high', code: 'inspect.plan_toggle', codeName: t('暂停 / 启用 / 删除定时计划'),
    lines: [t('删除计划及其运行记录；已产生的报告保留。此操作不可恢复。')],
    details: [{ k: t('计划'), v: p.name }, { k: t('实例数'), v: p.instance_total }],
    typed: p.name, typedLabel: t('请输入计划名称确认删除：'), okText: t('确认删除'),
  }, () => api('DELETE', `${BASE}/plans/${p.id}`));
  if (!cr.ok) return;
  setMsg($('plan-msg'), t('已删除'), 'info');
  await loadPlans();
}

// ---- 运行记录 ----

async function showRuns(p) {
  $('m-runs-title').textContent = t('运行记录 · {name}', { name: p.name });
  const body = $('runs-rows');
  body.replaceChildren();
  let runs = [];
  try { runs = await api('GET', `${BASE}/plans/${p.id}/runs`); } catch (e) { setMsg($('plan-msg'), e.message); return; }
  if (!runs.length) {
    const tr = el('tr');
    const c = el('td', 'empty', t('还没有运行记录。'));
    c.colSpan = 10;
    tr.append(c);
    body.append(tr);
  }
  for (const r of runs) {
    const tr = el('tr', r.status === 'failed' || r.failed ? 'lv-err' : r.skipped || r.status === 'skipped' ? 'lv-warn' : '');
    const al = el('td', 'num');
    al.append(countBadge(r.alert, 'crit'), document.createTextNode(' / '), countBadge(r.attention, 'warn'));
    const link = el('td');
    if (r.status !== 'skipped') {
      const a = el('a', '', t('{n} 份报告', { n: r.ok + r.failed }));
      a.href = `/inspection?plan_id=${r.plan_id}&run_id=${r.id}`;
      link.append(a);
    }
    const dur = r.finished_at ? fmtElapsed((Date.parse(r.finished_at) - Date.parse(r.started_at)) / 1000) : r.status === 'running' ? t('运行中') : '—';
    tr.append(td(fmtTime(r.scheduled_at)), td(r.trigger === 'manual' ? t('手动 · {name}', { name: r.operator }) : t('定时')), td(String(r.total)), td(String(r.ok)), td(String(r.failed)), td(String(r.skipped)), al, td(dur), td(tp(r.note || '')), link);
    for (const i of [2, 3, 4, 5]) tr.children[i].classList.add('num');
    tr.children[8].classList.add('small');
    body.append(tr);
  }
  openModal('m-runs');
}

// ---- 计划编辑弹窗 ----

let editing = null; // 正在编辑的计划；null 表示新建
let previewTimer = null;

const val = (id) => $(id).value;
const num = (id) => Number($(id).value);

function initPlanModal() {
  const wk = $('p-week');
  WEEK.forEach((name, i) => {
    const lb = el('label');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = String(i);
    lb.append(cb, document.createTextNode(name));
    wk.append(lb);
  });
  $('p-kind').addEventListener('change', () => { syncKind(); schedulePreview(); });
  for (const id of ['p-time', 'p-monthday', 'p-cron']) $(id).addEventListener('input', schedulePreview);
  wk.addEventListener('change', schedulePreview);
  $('p-notify').addEventListener('change', syncNotify);
  $('p-search').addEventListener('input', filterInstances);
  $('p-list').addEventListener('change', updateCount);
  $('p-all').addEventListener('click', () => { visibleChecks().forEach((c) => { c.checked = true; }); updateCount(); });
  $('p-none').addEventListener('click', () => { $('p-list').querySelectorAll('input').forEach((c) => { c.checked = false; }); updateCount(); });
  $('p-save').addEventListener('click', savePlan);
}

const visibleChecks = () => [...$('p-list').querySelectorAll('label')].filter((l) => !l.hidden).map((l) => l.querySelector('input'));

function syncKind() {
  const k = val('p-kind');
  $('row-time').hidden = k === 'cron';
  $('row-week').hidden = k !== 'weekly';
  $('row-month').hidden = k !== 'monthly';
  $('row-cron').hidden = k !== 'cron';
}

function syncNotify() {
  $('row-channels').hidden = val('p-notify') === 'none';
}

function updateCount() {
  $('p-count').textContent = String($('p-list').querySelectorAll('input:checked').length);
}

function filterInstances() {
  const q = val('p-search').trim().toLowerCase();
  $('p-list').querySelectorAll('label').forEach((l) => { l.hidden = q !== '' && !l.dataset.name.includes(q); });
}

function quickLink(text, fn) {
  const a = el('a', '', text);
  a.href = '#';
  a.addEventListener('click', (e) => { e.preventDefault(); fn(); updateCount(); });
  return a;
}

function pick(pred) {
  $('p-list').querySelectorAll('label').forEach((l) => {
    const i = opts.instances.find((x) => String(x.id) === l.querySelector('input').value);
    if (i && pred(i)) l.querySelector('input').checked = true;
  });
}

function buildInstanceList(selectedIDs) {
  const list = $('p-list');
  list.replaceChildren();
  for (const [name, items] of groupedInstances(opts.instances)) {
    list.append(el('b', 'grp', name));
    for (const i of items) {
      const lb = el('label');
      lb.dataset.name = i.name.toLowerCase();
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.value = String(i.id);
      cb.checked = selectedIDs.has(i.id);
      lb.append(cb, document.createTextNode(instLabel(i)));
      list.append(lb);
    }
  }
  $('p-total').textContent = String(opts.instances.length);
  const quick = $('p-quick');
  quick.replaceChildren();
  quick.className = 'quick';
  const tags = new Set();
  opts.instances.forEach((i) => i.tags.forEach((tg) => tags.add(tg)));
  const groups = new Set();
  opts.instances.forEach((i) => i.groups.forEach((g) => groups.add(g)));
  quick.append(quickLink(t('全部 MySQL 系'), () => pick((i) => i.kind === 'mysql')), quickLink(t('全部 SQL Server'), () => pick((i) => i.kind === 'mssql')));
  if (opts.instances.some((i) => i.kind === 'redis')) quick.append(quickLink(t('全部 Redis'), () => pick((i) => i.kind === 'redis')));
  for (const tg of [...tags].slice(0, 6)) quick.append(quickLink(t('标签 {name}', { name: tg }), () => pick((i) => i.tags.includes(tg))));
  for (const g of [...groups].slice(0, 6)) quick.append(quickLink(t('资源组 {name}', { name: g }), () => pick((i) => i.groups.includes(g))));
  updateCount();
}

function checkList(box, items, checked, labelOf, valueOf) {
  box.replaceChildren();
  for (const it of items) {
    const lb = el('label');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = String(valueOf(it));
    cb.checked = checked.has(String(valueOf(it)));
    lb.append(cb, document.createTextNode(labelOf(it)));
    box.append(lb);
  }
  if (!items.length) box.append(el('span', 'hint', t('暂无')));
}

function editPlan(p) {
  editing = p;
  setMsg($('plan-err'), '');
  $('m-plan-title').textContent = p ? t('编辑定时巡检计划：{name}', { name: p.name }) : t('新建定时巡检计划');
  $('p-name').value = p ? p.name : '';
  $('p-search').value = '';
  buildInstanceList(new Set(p ? p.instance_ids : []));
  checkList($('p-tags'), opts.tags, new Set(p ? p.tag_codes : []), (tag) => `${tag.code}${tag.name ? ' ' + tag.name : ''}`, (tag) => tag.code);
  $('p-dbs').value = p ? p.databases.join(', ') : '';
  $('p-kind').value = p ? p.kind : 'daily';
  $('p-time').value = p && p.time ? p.time : '02:00';
  $('p-monthday').value = p && p.month_day ? p.month_day : 1;
  $('p-cron').value = p && p.kind === 'cron' ? p.cron : '';
  const days = new Set((p && p.weekdays ? p.weekdays : [1]).map(String));
  $('p-week').querySelectorAll('input').forEach((c) => { c.checked = days.has(c.value); });
  $('p-conc').value = p ? p.concurrency : 2;
  $('p-stagger').value = p ? p.stagger_seconds : 0;
  $('p-skip').checked = p ? p.skip_busy : true;
  $('p-retry').checked = p ? p.retry_once : false;
  $('p-timeout').value = p ? p.timeout_minutes : 15;
  $('p-notify').value = p ? p.notify_mode : 'none';
  checkList($('p-channels'), opts.channels, new Set(p ? p.notify_channels.map(String) : []), (c) => (c.enabled ? t('{name}（{label}）', { name: c.name, label: tp(c.label) }) : t('{name}（{label}） · 已停用', { name: c.name, label: tp(c.label) })), (c) => c.id);
  $('p-retention').value = p ? p.retention_days : 90;
  $('p-enabled').checked = p ? p.enabled : true;
  syncKind();
  syncNotify();
  openModal('m-plan');
  schedulePreview();
}

function collectPlan() {
  return {
    name: val('p-name').trim(),
    kind: val('p-kind'), time: val('p-time'), weekdays: [...$('p-week').querySelectorAll('input:checked')].map((c) => Number(c.value)),
    month_day: num('p-monthday'), cron: val('p-cron').trim(),
    instance_ids: [...$('p-list').querySelectorAll('input:checked')].map((c) => Number(c.value)),
    tag_codes: [...$('p-tags').querySelectorAll('input:checked')].map((c) => c.value),
    databases: val('p-dbs').split(',').map((s) => s.trim()).filter(Boolean),
    concurrency: num('p-conc'), stagger_seconds: num('p-stagger'), skip_busy: $('p-skip').checked, retry_once: $('p-retry').checked, timeout_minutes: num('p-timeout'),
    notify_mode: val('p-notify'), notify_channels: [...$('p-channels').querySelectorAll('input:checked')].map((c) => Number(c.value)),
    retention_days: num('p-retention'), enabled: $('p-enabled').checked,
  };
}

function schedulePreview() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(async () => {
    const b = collectPlan();
    try {
      const res = await api('POST', BASE + '/plans/preview', { kind: b.kind, time: b.time, weekdays: b.weekdays, month_day: b.month_day, cron: b.cron });
      $('p-next').className = 'muted small';
      $('p-next').textContent = t('下次运行：{times} …', { times: res.next_runs.map(fmtTime).join(SEP) });
    } catch (e) {
      $('p-next').className = 'err-text small';
      $('p-next').textContent = e.message;
    }
  }, 350);
}

async function savePlan() {
  const body = collectPlan();
  try {
    if (editing) await api('PUT', `${BASE}/plans/${editing.id}`, body);
    else await api('POST', BASE + '/plans', body);
  } catch (e) {
    setMsg($('plan-err'), e.message);
    return;
  }
  closeModal('m-plan');
  setMsg($('plan-msg'), editing ? t('计划已保存') : t('计划已创建'), 'info');
  await loadPlans();
}

init();
