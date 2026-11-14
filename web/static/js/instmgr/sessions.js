/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: sessions.js
 * @time: 2026/10/28
 */
// 会话管理：进程列表（统计条、长时间运行标记、行级终止）、Top 表空间、事务、锁等待（含阻塞链）。
import { api, $, el, btn, setMsg, qs, emptyRow, cell, fmtNum, longCell, confirmBox, initTabs, loadCaps, can, loadInstanceSelect, renderPager, busy } from './ui.js';
import { t, tp } from '../i18n.js';

const msg = $('ss-msg');
const sel = new Map(); // 已勾选的会话：id -> {id, user, host}
const space = { page: 1, size: 20, total: 0 };
let caps = {};
let choices = [];
let tab = 'proc';
let preselect = 0; // 从锁信息 / 事务页跳转过来时要选中的线程号
let showTab = () => {};

const TAB_CAP = { proc: 'session.process', space: 'session.tablespace', trx: 'session.trx', lock: 'session.locks' };

const inst = () => Number($('ss-inst').value);
const cur = () => choices.find((c) => c.id === inst());

function unsupportedNote() {
  const c = cur();
  if (!c) { setMsg(msg, t('没有可用的实例')); return true; }
  if (!c.supported) { setMsg(msg, t('暂不支持 {type} 类型实例的会话管理', { type: tp(c.db_type_label) }), 'warn'); return true; }
  return false;
}

function resetTables() {
  for (const [id, cols] of [['p-rows', 10], ['s-rows', 9], ['t-rows', 10], ['l-rows', 8]]) emptyRow($(id), cols, '—');
  $('p-stats').replaceChildren();
  $('l-chains').replaceChildren();
}

async function loadTab(name = tab) {
  setMsg(msg, '');
  if (unsupportedNote()) return resetTables();
  const run = { proc: loadProc, space: loadSpace, trx: loadTrx, lock: loadLock }[name];
  try { await run(); } catch (e) { setMsg(msg, e.message); }
}

// ---- 进程状态 ----
function updateCount() {
  $('p-count').textContent = t('已选 {n} 个会话', { n: sel.size });
  $('p-kill').disabled = sel.size === 0;
}

function renderStats(st) {
  const box = $('p-stats');
  box.replaceChildren();
  const k = (label, value, cls) => {
    const d = el('div');
    d.append(el('b', cls || '', fmtNum(value)), el('span', '', label));
    box.append(d);
  };
  k(t('连接总数'), st.total);
  k(t('活跃（非 Sleep）'), st.active);
  k(t('长时间运行（≥300s）'), st.long, st.long ? 'warn' : '');
  k(t('阻塞源'), st.blockers, st.blockers ? 'crit' : '');
}

async function loadProc() {
  const box = $('p-rows');
  emptyRow(box, 10, t('加载中…'));
  const q = qs({ command: $('p-cmd').value, user: $('p-user').value.trim(), db: $('p-db').value.trim(), min_time: $('p-time').value.trim(), q: $('p-q').value.trim() });
  const res = await api('GET', `/api/v1/instances/${inst()}/processlist${q}`);
  // 已经结束的线程不再保留勾选
  const alive = new Set(res.rows.map((r) => r.id));
  for (const id of [...sel.keys()]) if (!alive.has(id)) sel.delete(id);
  const highlight = preselect;
  if (preselect) {
    const r = res.rows.find((x) => x.id === preselect && !x.protected);
    if (r) sel.set(r.id, { id: r.id, user: r.user, host: r.host });
    preselect = 0;
  }
  renderStats(res.stats);
  if (!res.rows.length) { emptyRow(box, 10, t('没有符合条件的会话')); updateCount(); return; }
  box.replaceChildren();
  let focus = null;
  for (const r of res.rows) {
    const tr = el('tr', r.severity === 'crit' ? 'lv-err' : r.severity === 'warn' ? 'lv-warn' : '');
    if (r.id === highlight) { tr.classList.add('im-hl'); focus = tr; }
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = sel.has(r.id);
    cb.disabled = r.protected || !can(caps, 'session.kill');
    cb.setAttribute('aria-label', t('选择线程 {id}', { id: r.id }));
    if (r.protected) cb.title = t('不允许终止：{reason}', { reason: tp(r.reason) });
    cb.addEventListener('change', () => {
      if (cb.checked) sel.set(r.id, { id: r.id, user: r.user, host: r.host }); else sel.delete(r.id);
      updateCount();
    });
    const first = el('td');
    first.append(cb);
    const timeTd = el('td', 'num nw', fmtNum(r.time));
    if (r.long_running) timeTd.append(' ', el('span', 'badge ' + (r.severity === 'crit' ? 'crit' : 'warn'), t('长时间运行')));
    if (r.blocker) timeTd.append(' ', el('span', 'badge crit', t('阻塞源')));
    const ops = el('td', 'row-actions');
    if (can(caps, 'session.kill')) {
      const kb = btn(t('终止'), 'ghost danger', () => kill([{ id: r.id, user: r.user, host: r.host }]));
      if (r.protected) { kb.disabled = true; kb.title = t('不允许终止：{reason}', { reason: tp(r.reason) }); }
      ops.append(kb);
    }
    tr.append(first, cell(r.id), cell(r.user), cell(r.host, 'mono'), cell(r.db || '—'), timeTd, cell(r.command), cell(r.state), longCell(r.info), ops);
    box.append(tr);
  }
  if (focus) focus.scrollIntoView({ block: 'center' });
  if (res.truncated) setMsg(msg, t('会话太多，只显示了前 1000 个，请用上面的条件缩小范围'), 'warn');
  updateCount();
}

$('p-all').addEventListener('change', (e) => {
  for (const cb of $('p-rows').querySelectorAll('input[type=checkbox]:not(:disabled)')) {
    if (cb.checked !== e.target.checked) { cb.checked = e.target.checked; cb.dispatchEvent(new Event('change')); }
  }
});

// 终止会话：先预览服务端生成的 KILL 语句，再二次确认；given 不为空时只终止这几个（行级“终止”按钮），否则终止勾选的。
async function kill(given) {
  const threads = Array.isArray(given) ? given : [...sel.values()];
  if (!threads.length) return;
  let plan;
  try { plan = await api('POST', `/api/v1/instances/${inst()}/kill/preview`, { threads }); } catch (e) { return setMsg(msg, e.message); }
  const ok = plan.items.filter((i) => i.ok);
  const refused = plan.items.filter((i) => !i.ok);
  const lines = [];
  if (ok.length) {
    lines.push(t('实例 {name}：将终止以下 {n} 个会话', { name: cur().instance_name, n: ok.length }));
    for (const i of ok) {
      const one = t('线程 {id} · {user}@{host} · 库 {db} · {command} · 已运行 {time}s', { id: i.id, user: i.user, host: i.host, db: i.db || '—', command: i.command, time: i.time });
      lines.push(one + (i.info ? ' · ' + i.info.slice(0, 80) : ''));
    }
  }
  for (const i of refused) lines.push(t('不会终止 线程 {id}：{reason}', { id: i.id, reason: tp(i.reason) }));
  if (!ok.length) return confirmBox({ title: t('没有可以终止的会话'), lines, noOk: true });
  if (!(await confirmBox({ title: t('确定要终止所选会话吗？'), lines, sql: plan.statements, okText: t('确定终止') }))) return;
  try {
    const res = await busy($('p-kill'), () => api('POST', `/api/v1/instances/${inst()}/kill`, { threads: ok.map((i) => ({ id: i.id, user: i.user, host: i.host })), confirm: true }));
    for (const i of ok) sel.delete(i.id);
    const bad = res.filter((r) => !r.ok);
    if (bad.length) {
      const detail = bad.map((r) => t('线程 {id}（{message}）', { id: r.id, message: tp(r.message) })).join(t('；'));
      setMsg(msg, t('已终止 {ok} 个，{bad} 个未成功：', { ok: res.length - bad.length, bad: bad.length }) + detail, 'warn');
    } else {
      setMsg(msg, t('已终止 {n} 个会话', { n: res.length }), 'info');
    }
  } catch (e) { setMsg(msg, e.message); }
  await loadProc().catch((e) => setMsg(msg, e.message));
}

// ---- Top 表空间 ----
async function loadSpace() {
  emptyRow($('s-rows'), 9, t('加载中…'));
  const res = await api('GET', `/api/v1/instances/${inst()}/tablespace` + qs({ offset: (space.page - 1) * space.size, limit: space.size }));
  space.total = res.total;
  const tb = $('s-rows');
  if (!res.rows.length) emptyRow(tb, 9, t('没有用户表'));
  else {
    tb.replaceChildren();
    for (const r of res.rows) {
      const tr = el('tr');
      tr.append(cell(r.schema), cell(r.table), cell(r.engine), cell(r.total_mb, 'num'), cell(fmtNum(r.rows), 'num'), cell(r.data_mb, 'num'), cell(r.index_mb, 'num'), cell(r.free_mb, 'num'), cell(r.free_pct, 'num'));
      if (r.free_pct >= 20 && r.free_mb >= 1024) tr.className = 'lv-warn';
      tb.append(tr);
    }
  }
  renderPager($('s-pager'), space, (p) => { space.page = p; loadTab('space'); });
}

// ---- 事务 ----
// 线程号：有“进程状态”权限时是链接，点击跳到进程状态页签并选中该会话。
function threadLink(id) {
  const td = el('td');
  if (!can(caps, 'session.process')) { td.textContent = String(id); return td; }
  const a = el('a', 'mono', String(id));
  a.href = '#';
  a.addEventListener('click', (e) => { e.preventDefault(); preselect = id; showTab('proc'); });
  td.append(a);
  return td;
}

async function loadTrx() {
  emptyRow($('t-rows'), 10, t('加载中…'));
  const rows = await api('GET', `/api/v1/instances/${inst()}/transactions` + qs({ min_seconds: $('t-min').value, idle_only: $('t-idle').checked ? '1' : '' }));
  const tb = $('t-rows');
  if (!rows.length) return emptyRow(tb, 10, t('没有符合条件的事务'));
  tb.replaceChildren();
  for (const r of rows) {
    const tr = el('tr');
    tr.append(cell(r.trx_started), cell(fmtNum(r.trx_secs), 'num'), cell(r.trx_state), cell(r.user || '—'), cell(r.host, 'mono'), cell(r.db || '—'), threadLink(r.thread_id),
      cell(fmtNum(r.rows_locked), 'num'), cell(fmtNum(r.rows_modified), 'num'), longCell(r.info));
    if (r.trx_secs >= 60) tr.className = 'lv-warn';
    tb.append(tr);
  }
}

// ---- 锁等待 ----
// 阻塞链：根因在最上面，被它阻塞的线程缩进挂在下面。
function renderChains(chains) {
  const box = $('l-chains');
  box.replaceChildren();
  if (!chains || !chains.length) return;
  const wrap = el('div', 'alert err im-chains');
  wrap.append(el('b', '', t('阻塞链（{n} 条，根因在最上面）', { n: chains.length })));
  for (const ch of chains) {
    const one = el('div', 'im-chain');
    for (const n of ch.nodes) {
      const line = el('div', 'im-chain-node');
      line.style.marginLeft = Math.min(n.depth, 8) * 22 + 'px';
      line.append(el('span', 'badge' + (n.role === 'root' ? ' crit' : ''), n.role === 'root' ? t('根因') : t('被阻塞')), ' ');
      if (can(caps, 'session.process')) {
        const a = el('a', 'mono', '#' + n.thread);
        a.href = '#';
        a.addEventListener('click', (e) => { e.preventDefault(); preselect = n.thread; showTab('proc'); });
        line.append(a);
      } else line.append(el('span', 'mono', '#' + n.thread));
      line.append(' ', el('small', 'muted', n.role === 'root' ? t('事务已持续 {secs} s', { secs: fmtNum(n.secs) }) : t('已等待 {secs} s', { secs: fmtNum(n.secs) }) + (n.object ? ' · ' + n.object : '')));
      if (n.sql) line.append(' ', el('code', 'im-chain-sql', n.sql.length > 100 ? n.sql.slice(0, 100) + ' …' : n.sql));
      one.append(line);
    }
    wrap.append(one);
  }
  box.append(wrap);
}

async function loadLock() {
  emptyRow($('l-rows'), 8, t('加载中…'));
  const view = await api('GET', `/api/v1/instances/${inst()}/lock-waits?chains=1`);
  const rows = view.rows;
  renderChains(view.chains);
  const tb = $('l-rows');
  if (!rows.length) return emptyRow(tb, 8, t('当前没有锁等待'));
  tb.replaceChildren();
  for (const r of rows) {
    const tr = el('tr', 'lv-err');
    const link = el('td');
    if (can(caps, 'session.process')) {
      const a = el('a', '', String(r.block_thread));
      a.href = '#';
      a.addEventListener('click', (e) => { e.preventDefault(); preselect = r.block_thread; showTab('proc'); });
      link.append(a);
    } else link.textContent = String(r.block_thread);
    tr.append(cell(r.wait_state), cell(fmtNum(r.wait_secs), 'num'), cell(r.wait_thread), longCell(r.wait_sql), cell(r.wait_object, 'mono'), link, longCell(r.block_sql), cell(fmtNum(r.block_secs) + ' s', 'num'));
    tb.append(tr);
  }
}

// ---- 初始化 ----
(async () => {
  try {
    caps = await loadCaps();
    $('p-kill').hidden = !can(caps, 'session.kill');
    let first = '';
    for (const [name, code] of Object.entries(TAB_CAP)) {
      const tabEl = $('tab-' + name);
      tabEl.hidden = !can(caps, code);
      if (can(caps, code) && !first) first = name;
    }
    if (!first) return setMsg(msg, t('你没有查看会话、表空间、事务或锁信息的权限'));
    const sw = initTabs($('ss'), (name) => { tab = name; space.page = 1; if (choices.length) loadTab(name); });
    showTab = sw;
    choices = await loadInstanceSelect($('ss-inst'), 'sessions');
    $('ss-inst').addEventListener('change', () => { sel.clear(); space.page = 1; loadTab(); });
    sw(first);
    if (!choices.length) setMsg(msg, t('没有可用的实例'), 'warn');
  } catch (e) { setMsg(msg, e.message); }
})();

$('p-filter').addEventListener('submit', (e) => { e.preventDefault(); loadTab('proc'); });
$('t-filter').addEventListener('submit', (e) => { e.preventDefault(); loadTab('trx'); });
$('l-refresh').addEventListener('click', () => loadTab('lock'));
$('p-kill').addEventListener('click', () => kill());
