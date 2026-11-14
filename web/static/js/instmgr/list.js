/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: list.js
 * @time: 2026/10/28
 */
// 实例列表：筛选、统计条、逐行异步健康状态、详情抽屉、页内新建/编辑（保存前字段对比确认）、测试连接、
// 实例停用/启用、删除（先检查引用）、Binlog 清理（核对从库进度）。
import { api, $, el, btn, setMsg, qs, emptyRow, cell, fmtBytes, fmtNum, openModal, closeModal, confirmBox, loadCaps, can, renderPager, busy } from './ui.js';
import { riskConfirm } from '../common.js';
import { t, tp } from '../i18n.js';

const msg = $('il-msg');
const state = { page: 1, size: 20, total: 0 };
let caps = {};
let opts = null; // 表单可选项：groups / tags / tunnels / db_types / ext_ready
let editing = null; // 正在编辑的实例；null 表示新建
let stats = null; // 服务端统计：total / disabled / collecting
let extReady = true;
let pageItems = [];
const selected = new Set(); // 勾选的实例 id（只在当前页内有效，换页 / 重新加载即清空）
const health = new Map(); // 实例 id -> 探测结果（当前页）
const rowOf = new Map(); // 实例 id -> {tr, status, ver, load}

const NO_AUTH = ['redis', 'memcached', 'mongo', 'elasticsearch', 'opensearch'];
const DEFAULT_PORT = { mysql: 3306, mssql: 1433, redis: 6379, pgsql: 5432, mongo: 27017, clickhouse: 8123, oracle: 1521, dm: 5236 }; // ClickHouse 走 HTTP 接口，默认 8123（TLS 常用 8443，勾选 SSL 不会自动改端口）
// PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦只支持在线查询（服务端给 features / features_label）；其它功能的按钮与入口对它们隐藏或标“暂不支持”。
const limited = (it) => Array.isArray(it.features) && it.features.length > 0;
const HEALTH_PARALLEL = 4; // 页面同时发出的探测请求数（服务端另有全局上限）
// 健康是逐个实例现场探测的，服务端分页接口不带它；选了“健康”筛选时改由页面读全部实例、逐个探测后再筛，结果跨页。
const G_PARALLEL = 6; // 跨页筛选的探测并发（服务端全局上限同为 6）
const HEALTH_REUSE_MS = 120000; // 跨页筛选里，2 分钟内的探测结果直接复用
let gMode = null; // 跨页健康筛选：{ all } 为当前条件下的全部实例；null 表示普通分页
let gToken = 0; // 每次加载 +1，旧的探测循环看到令牌变了就退出

function filters() {
  return { type: $('il-role').value, db_type: $('il-dbtype').value, tag: $('il-tag').value, status: $('il-status').value,
    state: $('il-state').value, env: $('il-env').value, q: $('il-q').value.trim() };
}

async function load() {
  setMsg(msg, '');
  if ($('il-health').value) return loadAllByHealth(false);
  gToken++;
  gMode = null;
  try {
    const res = await api('GET', '/api/v1/instances' + qs({ ...filters(), page: state.page, page_size: state.size }));
    state.total = res.total;
    stats = res.stats || null;
    extReady = res.ext_ready !== false;
    pageItems = res.items;
    render(res.items);
    renderPager($('il-pager'), state, (p) => { state.page = p; load(); });
    loadHealth(res.items, false);
  } catch (e) {
    emptyRow($('il-rows'), 12, t('加载失败'));
    setMsg(msg, e.message);
  }
}

function tagBadge(tag) {
  const b = el('span', 'badge' + (tag.active ? '' : ' warn'), tag.code);
  b.title = tag.active ? tag.name : t('{name}（已停用）', { name: tag.name });
  return b;
}

function envBadge(it) {
  if (!it.env) return el('span', 'muted', '—');
  const cls = it.env_prod ? 'prod' : it.env === 'test' ? 'test' : '';
  return el('span', ('badge ' + cls).trim(), tp(it.env_label) || it.env);
}

// 环境显示名：字典里的名称（内置的生产 / 测试有译文），没设置显示“未设置”。
const envName = (v) => (v ? tp(((opts && opts.envs) || []).find((o) => o.value === v)?.label || v) : t('未设置'));

// ---- 健康状态（逐行异步）----
// 列表里的状态只分“正常 / 异常”：告警（连接数接近上限）与未知（连不上、超时、暂不支持探测）都算异常，原因放在悬停提示里；
// 详情抽屉与“测试连接”仍显示完整原因。已停用不是健康状态，单独保留。
const DETAIL_TEXT = { ok: t('正常'), warn: t('告警'), unknown: t('未知'), disabled: t('已停用') };
const HEALTH_TEXT = { ok: [t('正常'), 'badge ok'], warn: [t('异常'), 'badge warn'], unknown: [t('异常'), 'badge warn'], disabled: [t('已停用'), 'badge'] };

function paintHealth(it, h) {
  const r = rowOf.get(it.id);
  if (!r) return;
  r.status.replaceChildren();
  r.ver.replaceChildren();
  r.load.replaceChildren();
  if (!h) {
    r.status.append(it.disabled ? el('span', 'badge', t('已停用')) : el('span', 'muted', t('探测中…')));
  } else {
    const [text, cls] = HEALTH_TEXT[h.status] || [t('异常'), 'badge warn'];
    const b = el('span', cls, text);
    if (h.message) b.title = h.message;
    r.status.append(b);
  }
  r.ver.append(el('span', '', it.db_type_label));
  if (h && h.version) r.ver.append(' ', el('small', 'muted', h.version));
  if (h && (h.status === 'ok' || h.status === 'warn') && h.qps_kind) {
    const q = el('span', 'mono', `${fmtNum(h.qps)} / ${fmtNum(h.connections)}`);
    q.title = h.max_connections
      ? t('连接上限 {max} · 延迟 {ms} ms · 探测于 {at}', { max: fmtNum(h.max_connections), ms: h.latency_ms, at: h.checked_at })
      : t('延迟 {ms} ms · 探测于 {at}', { ms: h.latency_ms, at: h.checked_at });
    r.load.append(q);
  } else {
    r.load.append(el('span', 'muted', '—'));
  }
  applyHealthFilter();
}

function healthOf(it) {
  if (it.disabled) return 'disabled';
  const h = health.get(it.id);
  return h ? h.status : '';
}

// 列表与筛选只分 ok / bad（告警、未知都归为异常）；还没探测出结果返回 ''
function groupOf(it) {
  const s = healthOf(it);
  if (s === 'ok' || s === 'disabled') return s;
  return s ? 'bad' : '';
}

function applyHealthFilter() {
  const want = $('il-health').value;
  for (const it of pageItems) {
    const r = rowOf.get(it.id);
    if (!r) continue;
    r.tr.hidden = !!want && groupOf(it) !== want;
  }
  renderKpis();
  syncSel();
}

function renderKpis() {
  const box = $('il-kpis');
  box.replaceChildren();
  const base = gMode ? gMode.all : pageItems;
  const count = (s) => base.filter((it) => groupOf(it) === s).length;
  const pending = base.filter((it) => !it.disabled && !health.has(it.id)).length;
  const k = (label, value, cls) => {
    const d = el('div');
    d.append(el('b', cls || '', value), el('span', '', label));
    box.append(d);
  };
  k(t('实例（可见范围）'), stats ? fmtNum(stats.total) : fmtNum(state.total));
  k(gMode ? t('正常（全部）') : t('正常（本页）'), count('ok') + (pending ? '…' : ''), 'ok');
  k(gMode ? t('异常（全部）') : t('异常（本页）'), count('bad'), count('bad') ? 'warn' : '');
  k(gMode ? t('已停用（全部）') : t('已停用（本页）'), base.filter((it) => it.disabled).length);
  k(t('已停用'), stats ? fmtNum(stats.disabled) : '—');
  k(t('慢SQL采集中'), stats ? fmtNum(stats.collecting) : '—');
}

async function loadHealth(items, force) {
  const todo = items.filter((it) => !it.disabled);
  for (const it of items) {
    if (it.disabled) paintHealth(it, null);
    else if (force || !health.has(it.id)) paintHealth(it, null);
  }
  const pageNo = state.page;
  const token = (loadHealth.token = (loadHealth.token || 0) + 1);
  let next = 0;
  const worker = async () => {
    while (next < todo.length && token === loadHealth.token && pageNo === state.page) {
      const it = todo[next++];
      let h;
      try {
        h = await api('GET', `/api/v1/instances/${it.id}/health` + (force ? '?force=1' : ''));
      } catch (e) {
        h = { status: 'unknown', message: e.message };
      }
      if (token !== loadHealth.token) return;
      health.set(it.id, h);
      paintHealth(it, h);
    }
  };
  await Promise.all(Array.from({ length: Math.min(HEALTH_PARALLEL, todo.length) }, worker));
}

// 读取当前条件（不含健康）下的全部实例；服务端单页最多 200 条。
async function fetchAllInstances(token) {
  const f = filters();
  const out = [];
  let page = 1;
  let total = 0;
  do {
    const res = await api('GET', '/api/v1/instances' + qs({ ...f, page, page_size: 200 }));
    if (token !== gToken) return null;
    stats = res.stats || stats;
    extReady = res.ext_ready !== false;
    total = res.total;
    out.push(...res.items);
    page++;
  } while (out.length < total && page <= 50);
  return out;
}

// 健康筛选（跨页）：读全部实例 → 已停用的直接归类 → 其余逐个探测（并发有限、服务端有缓存）→ 边探测边重绘命中的行。
async function loadAllByHealth(force) {
  const token = ++gToken;
  loadHealth.token = (loadHealth.token || 0) + 1; // 作废当前页还在跑的逐行探测
  const want = $('il-health').value;
  const pager = $('il-pager');
  pager.replaceChildren(el('span', 'muted', t('正在读取实例列表…')));
  let all;
  try {
    all = await fetchAllInstances(token);
  } catch (e) {
    if (token !== gToken) return;
    pager.replaceChildren();
    emptyRow($('il-rows'), 12, t('加载失败'));
    setMsg(msg, e.message);
    return;
  }
  if (!all || token !== gToken) return;
  gMode = { all };
  state.total = all.length;
  const now = Date.now();
  for (const it of all) {
    const h = health.get(it.id);
    if (h && (force || now - (h.at || 0) > HEALTH_REUSE_MS)) health.delete(it.id);
  }
  const live = all.filter((it) => !it.disabled);
  const todo = live.filter((it) => !health.has(it.id));
  let done = live.length - todo.length;
  let lastPaint = 0;
  const paint = (final) => {
    const shown = all.filter((it) => groupOf(it) === want);
    pageItems = shown;
    render(shown, true);
    if (!shown.length && !final) emptyRow($('il-rows'), 10, t('探测中…'));
    pager.replaceChildren(el('span', '', final
      ? t('共 {n} 条符合', { n: shown.length })
      : t('共 {n} 条符合，已探测 {done} / {total}', { n: shown.length, done, total: live.length })));
  };
  paint(!todo.length);
  let next = 0;
  const worker = async () => {
    while (next < todo.length && token === gToken) {
      const it = todo[next++];
      let h;
      try {
        h = await api('GET', `/api/v1/instances/${it.id}/health` + (force ? '?force=1' : ''));
      } catch (e) {
        h = { status: 'unknown', message: e.message };
      }
      if (token !== gToken) return;
      h.at = Date.now();
      health.set(it.id, h);
      done++;
      if (Date.now() - lastPaint > 1500) { lastPaint = Date.now(); paint(false); }
    }
  };
  await Promise.all(Array.from({ length: Math.min(G_PARALLEL, todo.length) }, worker));
  if (token === gToken) paint(true);
}

// HOST:PORT 列：超过 HP_CLAMP 个字符就折成约一半长度（省略号），点一下展开看全部，再点收起；不长的地址原样显示。
const HP_CLAMP = 30;
function hostCell(it) {
  const text = `${it.host}:${it.port}`;
  const td = el('td', 'mono nw');
  if (text.length <= HP_CLAMP) {
    td.textContent = text;
    return td;
  }
  const box = el('span', 'im-hp');
  box.setAttribute('role', 'button');
  box.tabIndex = 0;
  const body = el('span', 'im-hp-t', text);
  const chev = el('span', 'im-hp-x', '▸');
  chev.setAttribute('aria-hidden', 'true');
  const sync = () => {
    const open = box.classList.contains('open');
    box.setAttribute('aria-expanded', String(open));
    box.title = open ? t('点击收起地址') : t('{addr}\n点击展开完整地址', { addr: text });
    chev.textContent = open ? '▾' : '▸';
  };
  const flip = () => { box.classList.toggle('open'); sync(); };
  box.addEventListener('click', flip);
  box.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); flip(); } });
  sync();
  box.append(body, chev);
  td.append(box);
  return td;
}

function render(items, keep) {
  const tb = $('il-rows');
  rowOf.clear();
  if (keep) { // 跨页筛选的增量重绘：探测结果与仍在列表里的勾选保留
    for (const id of [...selected]) if (!items.some((it) => it.id === id)) selected.delete(id);
  } else {
    health.clear();
    selected.clear();
  }
  renderKpis();
  const canDisable = can(caps, 'instance.edit');
  const canDelete = can(caps, 'instance.delete');
  $('il-batch').hidden = !(canDisable || canDelete);
  $('il-b-enable').hidden = $('il-b-disable').hidden = $('il-b-collect-on').hidden = $('il-b-collect-off').hidden = !canDisable;
  $('il-b-env').hidden = $('il-b-env-go').hidden = !canDisable;
  $('il-b-delete').hidden = !canDelete;
  $('il-all').hidden = !(canDisable || canDelete);
  if (!items.length) { syncSel(); return emptyRow(tb, 10, t('没有符合条件的实例')); }
  tb.replaceChildren();
  for (const it of items) {
    const tr = el('tr', it.disabled ? 'im-off' : '');
    const sel = el('td', 'im-sel');
    if (canDisable || canDelete) {
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.setAttribute('aria-label', t('选择 {name}', { name: it.instance_name }));
      cb.addEventListener('change', () => {
        if (cb.checked) selected.add(it.id); else selected.delete(it.id);
        syncSel();
      });
      sel.append(cb);
    }
    const name = el('td', 'nw');
    const link = el('a', '', it.instance_name);
    link.href = '#';
    link.addEventListener('click', (e) => { e.preventDefault(); openDrawer(it); });
    name.append(link);
    if (it.disabled) name.append(' ', el('span', 'badge', t('已停用')));
    if (it.tunnel_id) name.append(' ', el('span', 'badge', t('隧道')));
    if (limited(it)) name.append(' ', el('span', 'badge', t('仅查询'))); // PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦：只支持在线查询
    const status = el('td', 'nw');
    const ver = el('td', 'il-ver');
    const load = el('td', 'nw');
    rowOf.set(it.id, { tr, status, ver, load });
    tr.append(sel, cell(it.id), name, cell(it.type === 'master' ? t('主库') : t('从库')), ver, hostCell(it));
    tr.append(status, load, limited(it) ? cell(t('暂不支持'), 'muted nw') : cell(it.is_enable ? t('采集中') : t('未采集'), it.is_enable ? 'ok-text nw' : 'muted nw')); // 慢SQL 一列不换行（“采集中”不拆成两行）
    const ops = el('td', 'row-actions ops');
    if (can(caps, 'instance.test') || can(caps, 'instance.add') || can(caps, 'instance.edit')) ops.append(btn(t('测试连接'), '', () => testSaved(it)), ' ');
    if (can(caps, 'instance.edit')) {
      ops.append(btn(t('编辑'), '', () => openEdit(it)), ' ');
    }
    if (canDisable) ops.append(btn(it.disabled ? t('启用') : t('停用'), it.disabled ? '' : 'ghost danger', () => toggleDisabled(it)), ' ');
    if (can(caps, 'instance.purge_binlog') && it.can_binlog && !it.disabled) ops.append(btn(t('Binlog 清理'), 'ghost danger', () => openBinlog(it)), ' ');
    if (can(caps, 'instance.delete')) ops.append(btn(t('删除'), 'ghost danger', () => remove(it)));
    tr.append(ops);
    tb.append(tr);
    paintHealth(it, keep ? (health.get(it.id) || null) : null);
  }
  syncSel();
}

// ---- 批量选择与批量操作 ----

// 当前真正会被操作的实例：勾选了、且没有被“健康”筛选藏起来的行。
function selItems() {
  return pageItems.filter((it) => selected.has(it.id) && rowOf.get(it.id) && !rowOf.get(it.id).tr.hidden);
}

function syncSel() {
  const vis = pageItems.filter((it) => rowOf.get(it.id) && !rowOf.get(it.id).tr.hidden);
  const picked = selItems();
  for (const it of pageItems) {
    const r = rowOf.get(it.id);
    if (!r) continue;
    const on = selected.has(it.id);
    r.tr.classList.toggle('im-picked', on);
    const cb = r.tr.querySelector('td.im-sel input');
    if (cb) cb.checked = on;
  }
  $('il-sel-n').textContent = String(picked.length);
  for (const id of ['il-b-enable', 'il-b-disable', 'il-b-collect-on', 'il-b-collect-off', 'il-b-env-go', 'il-b-delete', 'il-b-clear']) $(id).disabled = picked.length === 0;
  const all = $('il-all');
  all.checked = vis.length > 0 && picked.length === vis.length;
  all.indeterminate = picked.length > 0 && picked.length < vis.length;
}

// 实例名清单：最多列 n 个，超出的写“等 N 个”。
const namesText = (items, n = 12) => {
  const names = items.slice(0, n).map((it) => it.instance_name || it.name).join(t('、'));
  return items.length > n ? t('{names} 等 {n} 个', { names, n: items.length }) : names;
};

// 批量结果的提示：全部成功用普通提示；有失败的列出前几个及原因。kind 取 disable / enable / collectOn / collectOff / env / delete。
function batchSummary(res, kind, label) {
  const bad = res.items.filter((x) => !x.ok);
  const done = res.done;
  if (!bad.length) {
    const text = {
      disable: () => t('已停用 {n} 个实例', { n: done }),
      enable: () => t('已启用 {n} 个实例', { n: done }),
      collectOn: () => t('已开启采集 {n} 个实例', { n: done }),
      collectOff: () => t('已关闭采集 {n} 个实例', { n: done }),
      env: () => t('已设置环境为「{label}」 {n} 个实例', { label, n: done }),
      delete: () => t('已删除 {n} 个实例', { n: done }),
    }[kind]();
    return setMsg(msg, text, 'info');
  }
  const lines = bad.slice(0, 8).map((x) => t('{name}：{message}', { name: x.name, message: tp(x.message) }));
  if (bad.length > 8) lines.push(t('…… 还有 {n} 个失败', { n: bad.length - 8 }));
  const p = { done, bad: bad.length, label };
  const head = {
    disable: () => t('停用：成功 {done} 个，失败 {bad} 个。', p),
    enable: () => t('启用：成功 {done} 个，失败 {bad} 个。', p),
    collectOn: () => t('开启采集：成功 {done} 个，失败 {bad} 个。', p),
    collectOff: () => t('关闭采集：成功 {done} 个，失败 {bad} 个。', p),
    env: () => t('设置环境为「{label}」：成功 {done} 个，失败 {bad} 个。', p),
    delete: () => t('删除：成功 {done} 个，失败 {bad} 个。', p),
  }[kind]();
  setMsg(msg, head + '\n' + lines.join('\n'));
}

async function batchDisable(to) {
  const picked = selItems();
  const items = picked.filter((it) => !!it.disabled !== to); // 停用只处理未停用的，启用只处理已停用的
  if (!items.length) return setMsg(msg, to ? t('选中的实例都已经是停用状态，没有需要停用的。') : t('选中的实例都没有停用，没有需要启用的。'), 'info');
  const lines = to
    ? [t('停用后：平台不再连接这些实例；在线查询、SQL 上线、巡检、慢日志采集等页面的实例列表里不再出现它们（已有工单、报告、数据保留），随时可以重新启用。')]
    : [t('启用后这些实例会重新出现在各页面的实例列表里，平台恢复连接它们。')];
  const p = { n: items.length, names: namesText(items) };
  lines.push(to ? t('将停用 {n} 个：{names}', p) : t('将启用 {n} 个：{names}', p));
  if (picked.length > items.length) lines.push(t('另有 {n} 个已经是目标状态，会被跳过。', { n: picked.length - items.length }));
  const title = to ? t('批量停用 {n} 个实例？', { n: items.length }) : t('批量启用 {n} 个实例？', { n: items.length });
  if (!(await confirmBox({ title, lines, okText: to ? t('确定停用') : t('确定启用'), danger: to }))) return;
  try {
    const res = await api('POST', '/api/v1/instances/batch/disable', { ids: items.map((it) => it.id), disabled: to });
    await load();
    batchSummary(res, to ? 'disable' : 'enable');
  } catch (e) { setMsg(msg, e.message); }
}

// 批量开启 / 关闭慢日志采集：只改采集开关，不动实例本身（与上面的启用 / 停用是两回事）。
async function batchCollect(to) {
  const picked = selItems();
  const supported = picked.filter((it) => !limited(it)); // PostgreSQL / MongoDB / ClickHouse 没有慢日志采集
  const items = supported.filter((it) => !!it.is_enable !== to);
  if (!items.length) {
    let text;
    if (supported.length) text = to ? t('选中的实例慢日志采集都已经是开启状态，没有需要开启的。') : t('选中的实例慢日志采集都已经是关闭状态，没有需要关闭的。');
    else text = to ? t('选中的实例只支持在线查询，没有慢日志采集，没有需要开启的。') : t('选中的实例只支持在线查询，没有慢日志采集，没有需要关闭的。');
    return setMsg(msg, text, 'info');
  }
  const lines = to
    ? [t('开启后这些实例会纳入慢日志采集；已停用的实例要等重新启用后才开始采集。')]
    : [t('关闭后不再采集这些实例的慢日志，已经采集的数据保留，随时可以重新开启。')];
  const p = { n: items.length, names: namesText(items) };
  lines.push(to ? t('将开启 {n} 个：{names}', p) : t('将关闭 {n} 个：{names}', p));
  const skippedSame = supported.length - items.length;
  const skippedLimited = picked.length - supported.length;
  if (skippedSame) lines.push(to ? t('另有 {n} 个慢日志采集已经是开启状态，会被跳过。', { n: skippedSame }) : t('另有 {n} 个慢日志采集已经是关闭状态，会被跳过。', { n: skippedSame }));
  if (skippedLimited) lines.push(t('另有 {n} 个只支持在线查询（没有慢日志采集），会被跳过。', { n: skippedLimited }));
  const title = to ? t('批量开启采集 {n} 个实例？', { n: items.length }) : t('批量关闭采集 {n} 个实例？', { n: items.length });
  if (!(await confirmBox({ title, lines, okText: to ? t('确定开启采集') : t('确定关闭采集') }))) return;
  try {
    const res = await api('POST', '/api/v1/instances/batch/collect', { ids: items.map((it) => it.id), enable: to });
    await load();
    batchSummary(res, to ? 'collectOn' : 'collectOff');
  } catch (e) { setMsg(msg, e.message); }
}

// 批量设置环境：把选中的实例设为生产 / 测试 / 未设置（存旁路表，不改实例本身的其它设置）。
async function batchEnv() {
  if (!opts || !opts.ext_ready) return setMsg(msg, t('未启用：需先执行 070_instance_ext.sql'));
  const picked = selItems();
  const to = $('il-b-env').value;
  const label = envName(to);
  const items = picked.filter((it) => (it.env || '') !== to);
  if (!items.length) return setMsg(msg, t('选中的实例环境都已经是「{label}」，没有需要设置的。', { label }), 'info');
  const lines = [t('将把 {n} 个实例的环境设为「{label}」：{names}', { n: items.length, label, names: namesText(items) })];
  if (((opts && opts.envs) || []).some((o) => o.value === to && o.is_prod)) lines.push(t('目标是生产环境的 SQL 工单，执行、取消等高危操作会要求键入工单号确认。'));
  if (picked.length > items.length) lines.push(t('另有 {n} 个已经是「{label}」，会被跳过。', { n: picked.length - items.length, label }));
  if (!(await confirmBox({ title: t('批量设置环境 {n} 个实例？', { n: items.length }), lines, okText: t('确定设置'), danger: false }))) return;
  try {
    const res = await api('POST', '/api/v1/instances/batch/env', { ids: items.map((it) => it.id), env: to });
    await load();
    batchSummary(res, 'env', label);
  } catch (e) { setMsg(msg, e.message); }
}

async function batchDelete() {
  const picked = selItems();
  if (!picked.length) return;
  setMsg(msg, t('正在检查 {n} 个实例的引用…', { n: picked.length }), 'info');
  let pre;
  try {
    pre = await api('POST', '/api/v1/instances/batch/delete', { ids: picked.map((it) => it.id), dry_run: true });
  } catch (e) { return setMsg(msg, e.message); }
  setMsg(msg, '');
  const okList = pre.items.filter((x) => x.ok);
  const blocked = pre.items.filter((x) => !x.ok);
  const blockedLines = blocked.slice(0, 10).map((x) => t('{name}：{message}', { name: x.name, message: tp(x.message) }));
  if (blocked.length > 10) blockedLines.push(t('…… 还有 {n} 个', { n: blocked.length - 10 }));
  if (!okList.length) {
    await confirmBox({ title: t('选中的实例都不能删除'), lines: [t('它们仍被下面的记录引用，或当前没有权限：'), ...blockedLines, t('如果只是不再使用，可以改为“停用”，或取消资源组关联，让它不再出现在业务页面里。')], noOk: true, danger: true });
    return;
  }
  const lines = [t('删除后不可恢复；账号登记、数据库登记、参数修改历史等配置类数据会一并删除。'), t('将删除 {n} 个：{names}', { n: okList.length, names: namesText(okList) })];
  if (blocked.length) lines.push(t('下面 {n} 个不能删除，已跳过：', { n: blocked.length }), ...blockedLines);
  const typeText = t('删除{n}个实例', { n: okList.length });
  if (!(await confirmBox({ title: t('批量删除 {n} 个实例？', { n: okList.length }), lines, okText: t('确定删除'), typeText }))) return;
  try {
    const res = await api('POST', '/api/v1/instances/batch/delete', { ids: okList.map((x) => x.id), confirm: true });
    await load();
    batchSummary(res, 'delete');
  } catch (e) { setMsg(msg, e.message); }
}

// ---- 详情抽屉 ----
function kv(dl, k, v) {
  dl.append(el('dt', '', k));
  const dd = el('dd');
  if (v instanceof Node) dd.append(v); else dd.textContent = v === undefined || v === null || v === '' ? '—' : String(v);
  dl.append(dd);
}

async function openDrawer(it) {
  $('il-drawer-t').textContent = it.instance_name;
  const body = $('il-drawer-body');
  body.replaceChildren();
  const head = el('div', 'im-drawer-badges');
  head.append(el('span', 'badge', it.db_type_label), ' ', el('span', 'badge' + (it.type === 'master' ? ' ok' : ''), it.type === 'master' ? t('主库') : t('从库')));
  if (it.env) head.append(' ', envBadge(it));
  if (it.disabled) head.append(' ', el('span', 'badge warn', t('已停用')));
  body.append(head);
  const dl = el('dl', 'kv');
  kv(dl, t('地址'), `${it.host}:${it.port}`);
  const hv = el('span', '', t('探测中…'));
  kv(dl, t('版本 / 状态'), hv);
  const hl = el('span', '', '—');
  kv(dl, t('当前负载'), hl);
  kv(dl, t('连接账号'), it.user);
  kv(dl, t('环境'), tp(it.env_label) || t('未设置'));
  kv(dl, t('SSH 隧道'), it.tunnel_name || (it.tunnel_id ? `#${it.tunnel_id}` : t('无')));
  kv(dl, t('默认数据库 / 字符集'), [it.db_name, it.charset].filter(Boolean).join(' / ') || '—');
  kv(dl, t('资源组'), it.groups.map((g) => g.name).join(t('、')) || '—');
  const tg = el('span');
  it.tags.forEach((tag, i) => { if (i) tg.append(' '); tg.append(tagBadge(tag)); });
  kv(dl, t('标签'), it.tags.length ? tg : '—');
  if (limited(it)) kv(dl, t('支持范围'), tp(it.features_label) || it.features.join(t('、')));
  kv(dl, t('慢日志采集'), limited(it) ? t('暂不支持') : it.is_enable ? t('采集中') : t('未开启'));
  if (it.disabled) kv(dl, t('停用|state'), t('{by} 于 {at}', { by: it.disabled_by || '—', at: it.disabled_at || '—' }));
  if (it.show_db_name_regex) kv(dl, t('显示的库'), it.show_db_name_regex);
  if (it.denied_db_name_regex) kv(dl, t('隐藏的库'), it.denied_db_name_regex);
  body.append(dl);
  const links = el('div', 'im-drawer-links');
  for (const [text, href, code] of [[t('会话诊断'), '/instances/sessions', 'session.open'], [t('数据库'), '/instances/databases', 'database.view'],
    [t('帐号维护'), '/instances/accounts', 'account.view'], [t('参数管理'), '/instances/params', 'param.open']]) {
    if (!can(caps, code) || limited(it)) continue;
    const a = el('a', 'btn', text);
    a.href = `${href}?instance=${it.id}`;
    links.append(a, ' ');
  }
  body.append(links);
  openModal('il-drawer');
  if (it.disabled) { hv.textContent = t('已停用'); hl.textContent = '—'; return; }
  let h = health.get(it.id);
  if (!h) {
    try { h = await api('GET', `/api/v1/instances/${it.id}/health`); } catch (e) { h = { status: 'unknown', message: e.message }; }
  }
  const text = DETAIL_TEXT[h.status] || h.status; // 抽屉里保留“告警 / 未知”的区分和原因
  const hp = { version: h.version || '—', text, message: h.message };
  hv.textContent = h.message ? t('{version} · {text}（{message}）', hp) : t('{version} · {text}', hp);
  hl.textContent = (h.status === 'ok' || h.status === 'warn') && h.qps_kind
    ? t('QPS {qps}（{kind}）· 连接 {conn} · 活跃线程 {run} · 延迟 {ms} ms', {
      qps: fmtNum(h.qps),
      kind: h.qps_kind === 'delta' ? t('两次探测差值') : t('启动以来平均'),
      conn: fmtNum(h.connections) + (h.max_connections ? ' / ' + fmtNum(h.max_connections) : ''),
      run: fmtNum(h.threads_running),
      ms: h.latency_ms,
    })
    : '—';
}

// ---- 测试连接 ----
function showTest(r, into) {
  setMsg(into, t('连接成功：{endpoint} · {version} · 延迟 {ms} ms', { endpoint: r.endpoint, version: r.version || t('未知版本'), ms: r.latency_ms }), 'info');
}

async function testSaved(it) {
  setMsg(msg, t('正在测试 {name} …', { name: it.instance_name }), 'info');
  try {
    const r = await api('POST', `/api/v1/instances/${it.id}/test`);
    setMsg(msg, t('{name}：连接成功 · {endpoint} · {version} · 延迟 {ms} ms', { name: it.instance_name, endpoint: r.endpoint, version: r.version || t('未知版本'), ms: r.latency_ms }), 'info');
  } catch (e) { setMsg(msg, t('{name}：{message}', { name: it.instance_name, message: e.message })); }
}

// ---- 停用 / 启用实例 ----
async function toggleDisabled(it) {
  const to = !it.disabled;
  const lines = to
    ? [t('实例 {name} 停用后：平台不再连接它；在线查询、SQL 上线、巡检、慢日志采集等页面的实例列表里不再出现它（已有工单、报告、数据保留）。', { name: it.instance_name }),
      t('已关联资源组：{groups}。随时可以重新启用。', { groups: it.groups.map((g) => g.name).join(t('、')) || t('无') })]
    : [t('启用后 {name} 会重新出现在各页面的实例列表里，平台恢复连接它。', { name: it.instance_name })];
  const title = to ? t('停用实例 {name}？', { name: it.instance_name }) : t('启用实例 {name}？', { name: it.instance_name });
  if (!(await confirmBox({ title, lines, okText: to ? t('确定停用') : t('确定启用'), danger: to }))) return;
  try {
    await api('POST', `/api/v1/instances/${it.id}/disable`, { disabled: to });
    await load();
    setMsg(msg, to ? t('实例已停用') : t('实例已启用'), 'info');
  } catch (e) { setMsg(msg, e.message); }
}

// ---- 新建 / 编辑 ----
function checks(box, list, selected, label) {
  box.replaceChildren();
  if (!list.length) box.append(el('span', 'hint', t('（没有可选项）')));
  for (const o of list) {
    const lb = el('label');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = String(o.id);
    cb.checked = selected.has(o.id);
    lb.append(cb, document.createTextNode(label(o)));
    box.append(lb);
  }
}

const picked = (box) => [...box.querySelectorAll('input:checked')].map((c) => Number(c.value));

function syncType() {
  const ty = $('f-dbtype').value;
  $('f-oracle').hidden = ty !== 'oracle';
  const noSSL = ty === 'dm'; // 达梦驱动的 SSL 需要证书文件，暂不支持：不显示，也不带进保存的内容
  $('f-ssl').closest('label').hidden = noSSL;
  $('f-verify').closest('label').hidden = noSSL;
  if (noSSL) $('f-ssl').checked = false;
  if (!editing && DEFAULT_PORT[ty] && (!$('f-port').value || Object.values(DEFAULT_PORT).includes(Number($('f-port').value)))) $('f-port').value = DEFAULT_PORT[ty]; // 新建时切换类型，端口跟着变（只在端口还是某个默认值时）
  $('f-db').placeholder = ty === 'redis' ? t('库编号，如 0') : '';
  $('f-pass').placeholder = editing ? t('留空表示不修改') : (NO_AUTH.includes(ty) ? t('可不填') : '');
}

function openEdit(it) {
  editing = it || null;
  $('il-edit-t').textContent = it ? t('编辑实例：{name}', { name: it.instance_name }) : t('添加实例');
  setMsg($('il-edit-msg'), '');
  const dbt = $('f-dbtype');
  dbt.replaceChildren(...opts.db_types.map((o) => new Option(o.label, o.value)));
  $('f-name').value = it ? it.instance_name : '';
  $('f-role').value = it ? it.type : 'master';
  dbt.value = it ? it.db_type : 'mysql';
  $('f-env').value = it ? it.env || '' : '';
  $('f-env').disabled = !opts.ext_ready;
  $('f-env-hint').textContent = opts.ext_ready ? '' : t('未启用：需先执行 070_instance_ext.sql');
  $('f-mode').value = it ? it.mode : '';
  $('f-host').value = it ? it.host : '';
  $('f-port').value = it ? it.port : 3306;
  $('f-user').value = it ? it.user : '';
  $('f-pass').value = '';
  $('f-db').value = it ? it.db_name : '';
  $('f-charset').value = it ? it.charset : '';
  $('f-show').value = it ? it.show_db_name_regex : '';
  $('f-denied').value = it ? it.denied_db_name_regex : '';
  $('f-service').value = it ? it.service_name : '';
  $('f-sid').value = it ? it.sid : '';
  checks($('f-groups'), opts.groups, new Set(it ? it.groups.map((g) => g.id) : []), (g) => g.name);
  // 标签：启用的 + 该实例已带上的（即使已停用，也要能看到并取消）
  const have = new Set(it ? it.tags.map((tag) => tag.id) : []);
  checks($('f-tags'), opts.tags.filter((tag) => tag.active || have.has(tag.id)), have,
    (tag) => (tag.active ? `${tag.code} ${tag.name}` : t('{code} {name}（已停用）', { code: tag.code, name: tag.name })));
  const tun = $('f-tunnel');
  tun.replaceChildren(new Option('—', '0'), ...opts.tunnels.map((tn) => new Option(tn.name, String(tn.id))));
  tun.value = String(it ? it.tunnel_id : 0);
  $('f-ssl').checked = it ? it.is_ssl : false;
  $('f-verify').checked = it ? it.verify_ssl : true;
  $('f-enable').checked = it ? it.is_enable : false;
  syncType();
  openModal('il-edit');
}

function formBody() {
  const b = {
    instance_name: $('f-name').value, type: $('f-role').value, db_type: $('f-dbtype').value, mode: $('f-mode').value,
    host: $('f-host').value, port: Number($('f-port').value), user: $('f-user').value, password: $('f-pass').value,
    is_ssl: $('f-ssl').checked, verify_ssl: $('f-verify').checked, is_enable: $('f-enable').checked,
    db_name: $('f-db').value, show_db_name_regex: $('f-show').value, denied_db_name_regex: $('f-denied').value, charset: $('f-charset').value,
    service_name: $('f-service').value, sid: $('f-sid').value, tunnel_id: Number($('f-tunnel').value),
    group_ids: picked($('f-groups')), tag_ids: picked($('f-tags')),
  };
  if (opts.ext_ready) b.env = $('f-env').value; // 旁路表没建时不发，服务端当作“不修改”
  return b;
}

// 保存前的字段对比：[标签, 旧值, 新值]
const nameOf = (list, ids) => list.filter((x) => ids.includes(x.id)).map((x) => x.name || x.code).join(t('、')) || t('无');
function diffOf(old, body) {
  const out = [];
  const add = (label, a, b) => { if (String(a ?? '') !== String(b ?? '')) out.push([label, a === '' || a === undefined || a === null ? t('（空）') : a, b === '' || b === undefined || b === null ? t('（空）') : b]); };
  const typeLabel = (v) => (opts.db_types.find((o) => o.value === v) || { label: v }).label;
  const envLabel = envName;
  const roleLabel = (v) => (v === 'master' ? t('主库') : t('从库'));
  const yn = (v) => (v ? t('是') : t('否'));
  const onOff = (v) => (v ? t('开启') : t('关闭|off'));
  const tunnelName = (id) => (opts.tunnels.find((x) => x.id === id) || { name: t('无') }).name;
  add(t('实例名称'), old.instance_name, body.instance_name.trim());
  add(t('角色'), roleLabel(old.type), roleLabel(body.type));
  add(t('数据库类型'), typeLabel(old.db_type), typeLabel(body.db_type));
  if (opts.ext_ready) add(t('环境'), envLabel(old.env || ''), envLabel(body.env));
  add(t('模式'), old.mode, body.mode);
  add('HOST', old.host, body.host.trim());
  add('PORT', old.port, body.port);
  add(t('用户名'), old.user, body.user.trim());
  add(t('默认数据库'), old.db_name, body.db_name.trim());
  add(t('字符集'), old.charset, body.charset.trim());
  add(t('显示的库（正则）'), old.show_db_name_regex, body.show_db_name_regex);
  add(t('隐藏的库（正则）'), old.denied_db_name_regex, body.denied_db_name_regex);
  if (body.db_type === 'oracle' || old.db_type === 'oracle') { add('Oracle service_name', old.service_name, body.service_name); add('Oracle sid', old.sid, body.sid); }
  add(t('资源组'), nameOf(opts.groups, old.groups.map((g) => g.id)), nameOf(opts.groups, body.group_ids));
  add(t('标签'), nameOf(opts.tags, old.tags.map((x) => x.id)), nameOf(opts.tags, body.tag_ids));
  add(t('SSH 隧道'), old.tunnel_name || (old.tunnel_id ? `#${old.tunnel_id}` : t('无')), tunnelName(body.tunnel_id));
  add(t('启用 SSL'), yn(old.is_ssl), yn(body.is_ssl));
  add(t('验证服务端证书'), yn(old.verify_ssl), yn(body.verify_ssl));
  add(t('慢日志采集'), onOff(old.is_enable), onOff(body.is_enable));
  if (body.password) out.push([t('连接密码'), t('（已保存，不回显）'), t('已更改（加密保存，不进入审计）')]);
  return out;
}

// 添加实例的二次确认：列出将登记的字段（密码只说明已填写，不回显）。
async function confirmAdd(body) {
  const typeLabel = (opts.db_types.find((o) => o.value === body.db_type) || { label: body.db_type }).label;
  const envLabel = envName(body.env);
  const yn = (v) => (v ? t('是') : t('否'));
  const details = [
    { k: t('实例名称'), v: body.instance_name.trim() },
    { k: t('角色 / 类型'), v: `${body.type === 'master' ? t('主库') : t('从库')} / ${typeLabel}` },
    ...(opts.ext_ready ? [{ k: t('环境'), v: envLabel }] : []),
    { k: t('连接地址'), v: `${body.host.trim()}:${body.port}` },
    { k: t('用户名'), v: body.user.trim() },
    { k: t('连接密码'), v: body.password ? t('已填写（加密保存，不进入审计）') : t('未填写') },
    { k: t('默认数据库'), v: body.db_name.trim() },
    { k: t('资源组'), v: nameOf(opts.groups, body.group_ids) },
    { k: t('标签'), v: nameOf(opts.tags, body.tag_ids) },
    { k: t('SSH 隧道'), v: (opts.tunnels.find((x) => x.id === body.tunnel_id) || { name: t('无') }).name },
    { k: 'SSL', v: body.is_ssl ? t('启用（验证服务端证书：{v}）', { v: yn(body.verify_ssl) }) : t('未启用|ssl') },
    { k: t('慢日志采集'), v: body.is_enable ? t('开启') : t('关闭|off') },
  ];
  const r = await riskConfirm({
    title: t('添加实例 {name}？', { name: body.instance_name.trim() }), level: 'high', code: 'instance.add', codeName: t('添加实例'),
    lines: [body.group_ids.length
      ? t('请核对下面的信息。添加后该实例对所属资源组的成员可见，并开始被平台连接；操作写入操作日志（不记录密码）。')
      : t('请核对下面的信息。没有选择资源组：普通用户暂时看不到该实例（超级用户和有“查询所有实例”权限的用户可以），之后可以再编辑实例补上；添加后开始被平台连接，操作写入操作日志（不记录密码）。')],
    details, okText: t('确认添加'),
  });
  return r.ok;
}

async function save() {
  const b = $('il-edit-save');
  setMsg($('il-edit-msg'), '');
  const body = formBody();
  if (editing) {
    const changes = diffOf(editing, body);
    if (!changes.length) return setMsg($('il-edit-msg'), t('没有修改任何内容'), 'info');
    const ok = await confirmBox({ title: t('保存实例 {name}？', { name: editing.instance_name }),
      lines: [t('将更新以下内容：'), ...changes.map(([k, a, c]) => t('{k}：{old} → {new}', { k, old: a, new: c }))], okText: t('确定保存'), danger: false });
    if (!ok) return;
  } else if (!(await confirmAdd(body))) return;
  try {
    await busy(b, () => (editing ? api('PUT', `/api/v1/instances/${editing.id}`, body) : api('POST', '/api/v1/instances', body)));
    closeModal('il-edit');
    await load();
    setMsg(msg, t('已保存'), 'info');
  } catch (e) { setMsg($('il-edit-msg'), e.message); }
}

async function testForm() {
  const b = $('il-edit-test');
  setMsg($('il-edit-msg'), t('正在测试连接…'), 'info');
  try {
    const r = await busy(b, () => api('POST', '/api/v1/instances/test', { id: editing ? editing.id : 0, ...formBody() }));
    showTest(r, $('il-edit-msg'));
  } catch (e) { setMsg($('il-edit-msg'), e.message); }
}

// ---- 删除 ----
async function remove(it) {
  let refs;
  try { refs = await api('GET', `/api/v1/instances/${it.id}/refs`); } catch (e) { return setMsg(msg, e.message); }
  const refText = (r) => {
    const p = { label: tp(r.label), n: r.count + (r.capped ? '+' : ''), hint: tp(r.hint) };
    return r.hint ? t('{label}：{n} 条（{hint}）', p) : t('{label}：{n} 条', p);
  };
  const blocking = refs.filter((r) => r.blocking);
  if (blocking.length) {
    await confirmBox({ title: t('不能删除实例 {name}', { name: it.instance_name }), lines: [t('它仍被下面的记录引用：'), ...blocking.map(refText), t('如果只是不再使用，可以改为“停用”，或取消资源组关联，让它不再出现在业务页面里。')], noOk: true, danger: true });
    return;
  }
  const cascade = refs.filter((r) => !r.blocking);
  const ok = await confirmBox({
    title: t('确定删除实例 {name}？', { name: it.instance_name }),
    lines: [t('删除后不可恢复。'), ...(cascade.length ? [t('下面的配置类数据会一并删除：'), ...cascade.map(refText)] : [])],
    okText: t('确定删除'), typeText: it.instance_name,
  });
  if (!ok) return;
  try { await api('DELETE', `/api/v1/instances/${it.id}`, { confirm: true }); await load(); setMsg(msg, t('已删除'), 'info'); } catch (e) { setMsg(msg, e.message); }
}

// ---- Binlog 清理 ----
let binlogInst = null;
let plan = null;

async function openBinlog(it) {
  binlogInst = it;
  plan = null;
  $('bl-name').value = it.instance_name;
  $('bl-plan').hidden = true;
  $('bl-purge').disabled = true;
  $('bl-confirm').value = '';
  setMsg($('il-binlog-msg'), t('正在读取 binlog 列表…'), 'info');
  const sel = $('bl-file');
  sel.replaceChildren();
  openModal('il-binlog');
  try {
    const list = await api('GET', `/api/v1/instances/${it.id}/binlogs`);
    setMsg($('il-binlog-msg'), '');
    if (!list.length) return setMsg($('il-binlog-msg'), t('这个实例没有 binlog（可能没有开启）'), 'warn');
    sel.append(new Option(t('请选择 Binlog'), ''));
    for (const f of list) sel.append(new Option(`${f.name}　Size: ${fmtBytes(f.size)}`, f.name));
  } catch (e) { setMsg($('il-binlog-msg'), e.message); }
}

function renderReplicas(p) {
  const box = $('bl-replicas');
  box.replaceChildren();
  if (!p.replicas || !p.replicas.length) return;
  box.append(el('p', 'muted small', t('当前连着 {n} 个从库：', { n: p.replicas.length })));
  const table = el('table', 'list');
  const head = el('tr');
  for (const h of [t('从库'), 'HOST', t('正在读取'), t('位置'), t('IO / SQL 线程'), t('结论')]) head.append(el('th', '', h));
  const thead = el('thead');
  thead.append(head);
  table.append(thead);
  const tb = el('tbody');
  for (const r of p.replicas) {
    const tr = el('tr', r.behind ? 'lv-err' : (!r.checked ? 'lv-warn' : ''));
    const concl = r.behind ? t('清理目标越过它的进度，会中断复制') : r.checked ? t('进度足够，可清理') : (r.registered ? (r.error || t('读不到进度')) : t('平台里没有登记，无法核对'));
    tr.append(cell(r.name || t('（未登记）')), cell(r.host, 'mono'), cell(r.read_file || '—', 'mono'), cell(r.checked ? fmtNum(r.read_pos) : '—', 'num'),
      cell(r.checked ? `${r.io_running || '—'} / ${r.sql_running || '—'}` : '—'), cell(concl));
    tb.append(tr);
  }
  table.append(tb);
  box.append(table);
  for (const w of p.warnings || []) box.append(el('div', 'alert warn', w));
}

async function previewPurge() {
  const to = $('bl-file').value;
  if (!to) return setMsg($('il-binlog-msg'), t('请先选择 binlog'));
  setMsg($('il-binlog-msg'), '');
  try {
    plan = await busy($('bl-preview'), () => api('POST', `/api/v1/instances/${binlogInst.id}/binlogs/preview`, { to }));
  } catch (e) { plan = null; $('bl-plan').hidden = true; $('bl-purge').disabled = true; return setMsg($('il-binlog-msg'), e.message); }
  const first = plan.purge[0];
  const last = plan.purge[plan.purge.length - 1];
  let text = t('将清理 {n} 个 binlog（{range}，共 {size}），保留 {to} 及之后的 {keep} 个。', {
    n: plan.purge.length, range: first + (plan.purge.length > 1 ? ' ～ ' + last : ''), size: fmtBytes(plan.purge_size), to: plan.to, keep: plan.keep,
  });
  if (plan.blocked) text = t('已拒绝：{reason}。', { reason: plan.block_reason });
  else if (plan.dump_threads > 0) text += ' ' + t('当前有 {n} 个从库连着，请先确认它们都已读取到 {to} 之后，否则会造成复制中断。', { n: plan.dump_threads, to: plan.to });
  else text += ' ' + t('当前没有从库连接，仍请确认下游（备份、同步工具）不再需要这些日志。');
  $('bl-plan-text').textContent = text;
  renderReplicas(plan);
  $('bl-plan-sql').textContent = plan.sql + ';';
  $('bl-plan-name').textContent = plan.to;
  $('bl-confirm').value = '';
  $('bl-confirm').disabled = !!plan.blocked;
  $('bl-plan').hidden = false;
  $('bl-purge').disabled = true;
}

async function purge() {
  if (!plan || plan.blocked) return;
  try {
    await busy($('bl-purge'), () => api('POST', `/api/v1/instances/${binlogInst.id}/binlogs/purge`, { to: plan.to, confirm: $('bl-confirm').value.trim() }));
    closeModal('il-binlog');
    setMsg(msg, t('已清理 {name} 的 binlog（保留 {to} 及之后）', { name: binlogInst.instance_name, to: plan.to }), 'info');
  } catch (e) { setMsg($('il-binlog-msg'), e.message); }
}

// ---- 初始化 ----
$('il-filter').addEventListener('submit', (e) => { e.preventDefault(); state.page = 1; load(); });
for (const id of ['il-role', 'il-dbtype', 'il-tag', 'il-status', 'il-state', 'il-env']) $(id).addEventListener('change', () => { state.page = 1; load(); });
$('il-health').addEventListener('change', () => { state.page = 1; load(); });
$('il-refresh').addEventListener('click', () => {
  if (gMode) { loadAllByHealth(true); return; }
  health.clear();
  loadHealth(pageItems, true);
});
$('il-add').addEventListener('click', () => openEdit(null));
$('il-all').addEventListener('change', (e) => {
  for (const it of pageItems) {
    const r = rowOf.get(it.id);
    if (!r || r.tr.hidden) continue;
    if (e.target.checked) selected.add(it.id); else selected.delete(it.id);
  }
  syncSel();
});
$('il-b-enable').addEventListener('click', () => batchDisable(false));
$('il-b-disable').addEventListener('click', () => batchDisable(true));
$('il-b-collect-on').addEventListener('click', () => batchCollect(true));
$('il-b-collect-off').addEventListener('click', () => batchCollect(false));
$('il-b-env-go').addEventListener('click', batchEnv);
$('il-b-delete').addEventListener('click', batchDelete);
$('il-b-clear').addEventListener('click', () => { selected.clear(); syncSel(); });
$('il-edit-save').addEventListener('click', save);
$('il-edit-test').addEventListener('click', testForm);
$('f-dbtype').addEventListener('change', syncType);
$('bl-file').addEventListener('change', () => { plan = null; $('bl-plan').hidden = true; $('bl-purge').disabled = true; });
$('bl-preview').addEventListener('click', previewPurge);
$('bl-purge').addEventListener('click', purge);
$('bl-confirm').addEventListener('input', () => { $('bl-purge').disabled = !plan || !!plan.blocked || $('bl-confirm').value.trim() !== plan.to; });

(async () => {
  try {
    caps = await loadCaps();
    opts = await api('GET', '/api/v1/instances/form-options');
    $('il-dbtype').append(...opts.db_types.map((o) => new Option(o.label, o.value)));
    $('il-tag').append(...opts.tags.filter((tag) => tag.active).map((tag) => new Option(`${tag.code} ${tag.name}`, tag.code)));
    // 环境来自环境字典（平台设置 › 环境设置）
    const envOpts = () => (opts.envs || []).map((o) => new Option(tp(o.label), o.value));
    $('il-env').append(...envOpts());
    $('f-env').append(...envOpts());
    $('il-b-env').prepend(...envOpts());
    $('il-add').hidden = !can(caps, 'instance.add');
    await load();
  } catch (e) { setMsg(msg, e.message); emptyRow($('il-rows'), 12, t('加载失败')); }
})();
