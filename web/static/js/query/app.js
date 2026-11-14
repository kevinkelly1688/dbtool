/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: app.js
 * @time: 2026/10/28
 */
// 在线查询工作台：实例/库选择、库表树（列展开 / 无权限 / 敏感）、多标签（各自保存结果与执行计划）、运行与取消、
// 限制策略提示条、格式化与示例 SQL、执行历史。选到 Redis 实例时改成“命令”模式：库=db 编号、左侧是 key 浏览、纯文本编辑器、没有格式化 / 执行计划。
// 选到 PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦实例时：编辑器模式、示例语句、库表树（PG 按 schema 分组、Mongo 是集合）、结果提示（脱敏、warnings / notices）按类型切换，没有格式化 / 执行计划页签。
import { api, fragment, riskConfirm } from '../common.js';
import { t, tp } from '../i18n.js';
import { loadButtons, can } from '../btnperm.js';
import { createEditor } from './editor.js';
import { renderResult } from './grid.js';
import { renderPlan } from './plan.js';
import { initMySQL } from './mysql.js';
import { createTree } from './tree.js';
import { renderPolicy } from './policybar.js';
import { formatSQL } from './format.js';
import { buildSamples, REDIS_COMMANDS } from './samples.js';
import { isMSSQL, isPG, isRedis, isMulti, isMongo, noPlanType, selectSQL, quoteId } from './sqlutil.js';
import { createProgrammables } from './programmable.js';
import { showObjectInfo } from './objinfo.js';
import { openMenu } from './ctxmenu.js';
import { createKeyBrowser, toMatch } from './redis_keys.js';
import { initSideResize } from './sidesize.js';
import { createBatch } from './batch.js';

const $ = (id) => document.getElementById(id);
const STORE_KEY = 'dbtool.query.tabs.v1';
// 没有 query.run 权限（loadButtons 取到且不含该码）时，运行 / 执行计划一直置灰。
const NO_RUN_TITLE = t('无权限：query.run');
let noRun = false;

const els = {
  inst: $('inst'), db: $('db'), tree: $('tree'), treeQ: $('tree-q'), tabs: $('tabs'), run: $('run'), explain: $('explain'),
  cancel: $('cancel'), elapsed: $('elapsed'), limit: $('limit'), policy: $('policy'), msg: $('msg'), result: $('result'),
  stripe: $('env-stripe'), bEnv: $('b-env'), bRole: $('b-role'), bType: $('b-type'), fmt: $('fmt'), sample: $('sample'),
  sideTabs: $('side-tabs'), progQ: $('prog-q'), progTree: $('prog-tree'),
  runAll: $('run-all'), runAllN: $('run-all-n'), batchOpts: $('batch-opts'),
  sideT: { tables: $('side-t-tables'), prog: $('side-t-prog') },
  sideP: { tables: $('side-p-tables'), prog: $('side-p-prog') },
};
// 切到 Redis / 新引擎时被替换的搜索框文案，切回 MySQL / SQL Server 时原样恢复。
const orig = {
  treePh: els.treeQ.placeholder, treeAria: els.treeQ.getAttribute('aria-label') || '', fmtTitle: els.fmt.title,
};
// 新引擎里“格式化 / 执行计划”不可用时的原因（按钮置灰时的 title），MongoDB 另换搜索框文案。
const NEW_ENGINE = {
  pgsql: { fmt: t('PostgreSQL 暂不提供格式化'), plan: t('PostgreSQL 没有执行计划页签，请直接运行 EXPLAIN'), planMsg: t('PostgreSQL 没有执行计划页签，请直接运行 EXPLAIN。') },
  clickhouse: { fmt: t('ClickHouse 暂不提供格式化'), plan: t('ClickHouse 没有执行计划页签，请直接运行 EXPLAIN'), planMsg: t('ClickHouse 没有执行计划页签，请直接运行 EXPLAIN。') },
  oracle: { fmt: t('Oracle 暂不提供格式化'), plan: t('Oracle 没有执行计划页签，请直接运行 EXPLAIN PLAN FOR'), planMsg: t('Oracle 没有执行计划页签，请直接运行 EXPLAIN PLAN FOR。') },
  dm: { fmt: t('达梦暂不提供格式化'), plan: t('达梦没有执行计划页签，请直接运行 EXPLAIN'), planMsg: t('达梦没有执行计划页签，请直接运行 EXPLAIN。') },
  mongo: { fmt: t('MongoDB 语句不做格式化'), plan: t('MongoDB 不支持 explain'), planMsg: t('MongoDB 不支持 explain。'), ph: t('搜索集合名'), aria: t('搜索集合名') },
};
const wb = $('wb');
const globalPolicy = {
  timeout: Number(wb.dataset.timeout) || 60,
  disableStar: wb.dataset.disableStar === 'true',
  masking: wb.dataset.masking === 'true',
};

// ---- 标签页（SQL / 实例 / 库仅存浏览器本地；各标签的结果、执行计划、提示只在内存里，刷新即丢，不影响正确性） ----
let state = loadState();
const views = new Map(); // 标签 id -> { res, plan, rtab, msg, req }

function loadState() {
  try {
    const s = JSON.parse(localStorage.getItem(STORE_KEY));
    if (s && Array.isArray(s.tabs) && s.tabs.length) return s;
  } catch (_) { /* 隐私模式或数据损坏时忽略 */ }
  return { tabs: [newTab(1)], cur: 0, seq: 1 };
}

function newTab(n) {
  return { id: 't' + Date.now().toString(36) + n, name: t('查询 {n}', { n }), sql: '', inst: '', db: '' };
}

let saveTimer = 0;
function saveState() {
  clearTimeout(saveTimer);
  saveTimer = setTimeout(() => {
    try { localStorage.setItem(STORE_KEY, JSON.stringify(state)); } catch (_) { /* 忽略 */ }
  }, 300);
}

const cur = () => state.tabs[state.cur];
const viewOf = (id) => { let v = views.get(id); if (!v) { v = { res: null, plan: null, rtab: 'res', msg: null, req: null }; views.set(id, v); } return v; };
const instType = () => { const o = els.inst.selectedOptions[0]; return o ? o.dataset.type || '' : ''; };

const editor = createEditor($('editor'), {
  onRun: () => execute(false),
  onRunAll: () => runAll(),
  onChange: () => { if (!switching) { cur().sql = editor.value; saveState(); } refreshRunAll(); },
});

initSideResize($('wb'), $('wb-resizer'), () => editor.resize()); // 左栏宽度可拖动，宽度变了编辑器要重新量

let switching = false;
let running = null; // { tabId, ctrl }

// 运行全部：脚本里每条语句各一个结果页签（js/query/batch.js）。这里只提供它要用的几个入口。
const batchUi = createBatch({
  editor,
  view: () => viewOf(cur().id),
  tab: () => cur(),
  busy: () => !!running,
  ctx: () => ({
    type: instType(), instId: Number(els.inst.value), db: els.db.value, limit: Number(els.limit.value),
    instName: els.inst.selectedOptions[0] ? els.inst.selectedOptions[0].textContent.trim() : '',
  }),
  begin: (tabId, ctrl) => { running = { tabId, ctrl }; setBusy(true); renderTabs(); drawMsg(); drawResult(); },
  end: () => { running = null; setBusy(false); renderTabs(); drawMsg(); drawResult(); },
  redraw: () => drawResult(),
  setMsg,
  askReason,
  noPrivExtra: (e) => {
    const target = e.data.table ? `${e.data.database}.${e.data.table}` : dbLabel(e.data.database);
    const href = applyHref(e.data.table);
    return href ? { href, text: t('申请 {target} 的查询权限', { target }) } : { plain: t('（需要申请：{target}）', { target }) };
  },
  resultOpts: (res, r) => {
    const multi = isMulti(r.ctx.type);
    return {
      dbType: r.ctx.type,
      exportHref: wb.dataset.exportEnabled === 'true' && !multi ? '/sqlworkflow/export/new' : '',
      estimate: res.truncated && !multi ? () => {
        if (res._est === undefined) {
          res._est = api('POST', '/api/v1/query/estimate', { instance_id: r.ctx.instId, db: r.ctx.db, sql: r.run }).then((x) => (x ? x.rows : -1));
        }
        return res._est;
      } : null,
    };
  },
});

function renderTabs() {
  els.tabs.replaceChildren();
  state.tabs.forEach((tb, i) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'tab' + (i === state.cur ? ' on' : '');
    b.setAttribute('role', 'tab');
    b.setAttribute('aria-selected', String(i === state.cur));
    const name = document.createElement('span');
    name.textContent = tb.name;
    b.append(name);
    if (running && running.tabId === tb.id) {
      const dot = document.createElement('span');
      dot.className = 'spinner';
      dot.title = t('运行中');
      b.append(dot);
    }
    if (state.tabs.length > 1) {
      const x = document.createElement('span');
      x.className = 'x';
      x.textContent = '×';
      x.title = t('关闭');
      x.setAttribute('aria-label', t('关闭标签 {name}', { name: tb.name }));
      x.addEventListener('click', (ev) => { ev.stopPropagation(); closeTab(i); });
      b.append(x);
    }
    b.addEventListener('click', () => switchTab(i));
    els.tabs.append(b);
  });
  const add = document.createElement('button');
  add.type = 'button';
  add.className = 'tab';
  add.title = t('新建查询标签');
  add.setAttribute('aria-label', t('新建查询标签'));
  add.textContent = '+';
  add.addEventListener('click', () => addTab('', cur().inst, cur().db));
  els.tabs.append(add);
}

function addTab(sql, inst, db) {
  state.seq = (state.seq || state.tabs.length) + 1;
  const t = newTab(state.seq);
  t.sql = sql;
  t.inst = inst;
  t.db = db;
  state.tabs.push(t);
  return switchTab(state.tabs.length - 1);
}

async function closeTab(i) {
  if (state.tabs.length === 1) return;
  const tb = state.tabs[i];
  if (running && running.tabId === tb.id) { setMsg(t('该标签正在运行，请先取消。'), 'warn'); return; }
  if (tb.sql.trim()) {
    const r = await riskConfirm({
      title: t('关闭「{name}」？', { name: tb.name }), lines: [t('标签里的 SQL 将丢失，无法找回。')],
      okText: t('关闭标签'), cancelText: t('保留'), danger: true,
    });
    if (!r.ok) return;
    // 确认期间标签可能已变化：按对象重新定位，且不能关到正在运行的标签
    i = state.tabs.indexOf(tb);
    if (i < 0 || state.tabs.length === 1) return;
    if (running && running.tabId === tb.id) { setMsg(t('该标签正在运行，请先取消。'), 'warn'); return; }
  }
  views.delete(tb.id);
  state.tabs.splice(i, 1);
  if (state.cur >= state.tabs.length) state.cur = state.tabs.length - 1;
  else if (i < state.cur) state.cur--;
  switchTab(state.cur);
}

async function switchTab(i) {
  state.cur = i;
  switching = true;
  editor.value = cur().sql;
  switching = false;
  renderTabs();
  drawMsg();
  drawResult();
  await selectContext(cur().inst, cur().db);
  saveState();
}

// ---- 提示条（每个标签各自保存） ----
function setMsg(text, kind = 'err', extra) {
  viewOf(cur().id).msg = text ? { text, kind, extra } : null;
  drawMsg();
}

function drawMsg() {
  els.msg.replaceChildren();
  const m = viewOf(cur().id).msg;
  if (!m) return;
  const d = document.createElement('div');
  d.className = 'alert ' + m.kind;
  d.setAttribute('role', m.kind === 'err' ? 'alert' : 'status');
  d.textContent = m.text;
  if (m.extra && m.extra.href) {
    const a = document.createElement('a');
    a.className = 'alert-act';
    a.href = m.extra.href;
    a.target = '_blank';
    a.rel = 'noopener';
    a.textContent = m.extra.text;
    d.append(a);
  } else if (m.extra && m.extra.plain) {
    d.append(document.createTextNode(m.extra.plain));
  }
  els.msg.append(d);
}

// 申请查询权限的入口：本系统提供权限申请则带着实例、库、表跳转预填；否则没有入口。
function applyHref(table) {
  if (wb.dataset.applyEnabled === 'true') {
    const q = new URLSearchParams({ new: '1', instance: els.inst.value, db: els.db.value });
    if (table) q.set('table', table);
    return '/query/applies?' + q.toString();
  }
  return '';
}

// Redis 的库显示成 db3；其他类型原样。
const dbLabel = (name) => (isRedis(instType()) ? 'db' + name : name);

function noPrivMsg(message, database, table) {
  const target = table ? `${database}.${table}` : dbLabel(database);
  const href = applyHref(table);
  if (!href) return setMsg(message, 'err', { plain: t('（需要申请：{target}）', { target }) });
  setMsg(message, 'err', { href, text: t('申请 {target} 的查询权限', { target }) });
}

// ---- 实例 / 库 / 对象树 / 提示条 ----
function updateBadges() {
  const o = els.inst.selectedOptions[0];
  const env = o ? o.dataset.env : '';
  const role = o ? o.dataset.role : '';
  const type = o ? o.dataset.type : '';
  const envProd = o ? o.dataset.envProd === 'true' : false;
  const envLabel = o ? o.dataset.envLabel : '';
  const envCls = envProd ? 'prod' : env === 'test' ? 'test' : '';
  els.stripe.dataset.env = envCls;
  const set = (el, text, cls) => { el.hidden = !text; el.textContent = text || ''; el.className = 'badge' + (cls ? ' ' + cls : ''); };
  set(els.bEnv, env ? tp(envLabel || env) : '', envCls);
  set(els.bRole, role === 'master' ? t('主库') : role === 'slave' ? t('从库') : '');
  set(els.bType, type);
  editor.setMode(type);
  const redis = isRedis(type);
  const eng = NEW_ENGINE[type];
  els.runAll.hidden = els.batchOpts.hidden = redis || isMongo(type); // 一次只能一条命令 / 语句
  refreshRunAll();
  els.explain.disabled = noPlanType(type) || !!running || noRun;
  els.explain.title = noRun ? NO_RUN_TITLE : redis ? t('Redis 没有执行计划') : isMSSQL(type) ? t('SQL Server 暂不支持执行计划') : eng ? eng.plan : '';
  els.fmt.disabled = redis || isMulti(type);
  els.fmt.title = redis ? t('Redis 命令无需格式化') : eng ? eng.fmt : orig.fmtTitle;
  applyChrome(type);
  syncSideTabs();
}

// 非 MySQL / SQL Server 类型的搜索框：切换时清空（SQL 的表名过滤和 Redis 的 key 模式不能互用），切回时原样恢复。
let chromeType = '';
function applyChrome(type) {
  const mode = isRedis(type) ? 'redis' : NEW_ENGINE[type] ? type : '';
  if (mode === chromeType) return;
  chromeType = mode;
  const redis = mode === 'redis';
  const eng = NEW_ENGINE[mode];
  els.treeQ.value = '';
  els.treeQ.placeholder = redis ? t('key 匹配，如 user:*（回车）') : eng && eng.ph ? eng.ph : orig.treePh;
  els.treeQ.setAttribute('aria-label', redis ? t('按模式搜索 key') : eng && eng.aria ? eng.aria : orig.treeAria);
  if (!redis) tree.setFilter('');
}

let sampleCtx = { table: '', cols: [], pk: '' };
let samples = [];

function drawPolicy(meta) {
  const info = renderPolicy(els.policy, { meta, db: els.db.value, dbType: instType(), global: globalPolicy, applyHref: () => applyHref() });
  if (!noRun) els.run.title = info; // 行数上限、超时、只读范围不再占一排，悬停在“运行”上看
}

function drawSamples() {
  const groups = [];
  samples = buildSamples({ dbType: instType(), disableStar: globalPolicy.disableStar, ...sampleCtx });
  els.sample.replaceChildren(new Option(isRedis(instType()) ? t('示例命令…') : isMongo(instType()) ? t('示例语句…') : t('示例 SQL…'), ''));
  samples.forEach((s, i) => {
    let g = groups.find((x) => x.label === s.group);
    if (!g) { g = document.createElement('optgroup'); g.label = s.group; groups.push(g); els.sample.append(g); }
    g.append(new Option(s.label, String(i)));
  });
}

const tree = createTree({
  container: els.tree,
  onTable: async (t) => {
    try {
      if (isMongo(instType())) { // 集合：直接生成 find，不用先取字段；一次只能一条语句，所以编辑器非空时放进新标签
        const stmt = selectSQL(t.name, [], 'mongo', 10);
        if (editor.value.trim()) await addTab(stmt, cur().inst, cur().db); else { editor.value = stmt; setMsg(''); }
        return;
      }
      const cols = await tree.columnsOf(t.name);
      editor.append(selectSQL(t.name, cols.map((c) => c.name), instType(), 100));
      setMsg('');
    } catch (e) {
      setMsg(e.message);
    }
  },
  onColumn: (name) => editor.insert(quoteId(name, instType())),
  // 右键表名 / 视图名：「对象信息」（基本信息 + 创建语句，只读）
  onContext: (t, e, c) => objectMenu(e, t.name, () => showObjectInfo({ inst: c.inst, db: c.db, type: c.type, ...objectRef(t, c.type) })),
  onLocked: (tbl) => noPrivMsg(t('你没有 {db}.{table} 的查询权限。', { db: els.db.value, table: tbl.name }), els.db.value, tbl.name),
  onLoaded: (meta) => {
    drawPolicy(meta);
    editor.setTables(meta ? meta.tables.map((tbl) => tbl.name) : [], isMongo(instType()) ? t('集合') : t('表'));
    sampleCtx = { table: '', cols: [], pk: '' };
    drawSamples();
    if (!meta) return;
    const plain = (tbl) => !tbl.view && !tbl.locked && (!tbl.kind || tbl.kind === 'table' || tbl.kind === 'collection' || tbl.kind === 'partitioned'); // 新引擎的字典 / 外部表 / 物化视图不当示例表
    const first = meta.tables.find(plain) || meta.tables.find((tbl) => !tbl.locked);
    if (!first) return;
    const dbAtLoad = els.db.value;
    sampleCtx.table = first.name;
    drawSamples();
    tree.columnsOf(first.name).then((cols) => {
      if (els.db.value !== dbAtLoad) return;
      sampleCtx = { table: first.name, cols: cols.map((c) => c.name), pk: (cols.find((c) => c.key === 'PRI') || {}).name || '' };
      drawSamples();
    }).catch(() => { /* 取不到字段时示例只用表名 */ });
  },
});

// ---- 左侧页签：表 / 可编程对象（MySQL 系、SQL Server；Redis、PostgreSQL、MongoDB、ClickHouse、Oracle、达梦没有这个页签）----
let noObjInfo = false; // 没有 query.objinfo 按钮：隐藏“可编程对象”页签与右键“对象信息”（loadButtons 取到后设置；服务端仍逐接口校验）
const progSupported = (type) => !noObjInfo && !!type && !isRedis(type) && !isMulti(type);
let sideTab = 'tables';
let progDirty = true; // 实例 / 库变了，可编程对象列表要重取（页签打开时才取）
const prog = createProgrammables({
  container: els.progTree,
  onOpen: (kind, it, c) => showObjectInfo({ inst: c.inst, db: c.db, type: c.type, kind, name: it.name, schema: it.schema }),
  onContext: (kind, it, e, c) => {
    const label = (it.schema ? it.schema + '.' : '') + it.name;
    objectMenu(e, label, () => showObjectInfo({ inst: c.inst, db: c.db, type: c.type, kind, name: it.name, schema: it.schema }));
  },
});

// 库表树节点 -> 对象信息的定位参数：PostgreSQL 的 schema.table、SQL Server 的 架构.表 拆成 schema + name。
function objectRef(t, type) {
  const kind = t.view ? 'view' : 'table';
  if (isPG(type)) {
    const schema = t.schema || (t.name.includes('.') ? t.name.slice(0, t.name.indexOf('.')) : 'public');
    return { kind, schema, name: t.name.startsWith(schema + '.') ? t.name.slice(schema.length + 1) : t.name };
  }
  if (isMSSQL(type)) {
    const i = t.name.lastIndexOf('.');
    return i > 0 ? { kind, schema: t.name.slice(0, i), name: t.name.slice(i + 1) } : { kind, schema: 'dbo', name: t.name };
  }
  return { kind, schema: '', name: t.name };
}

// 右键菜单（目前只有「对象信息」）。键盘菜单键 / Shift+F10 触发时没有鼠标坐标，菜单贴在目标元素下面。
function objectMenu(e, label, show) {
  if (noObjInfo) return;
  const at = !e.clientX && !e.clientY ? { el: e.currentTarget || e.target } : { x: e.clientX, y: e.clientY };
  openMenu(at, [{ label: t('对象信息'), run: show }], { label: t('{label} 的操作', { label }), scrollRoots: [els.tree, els.progTree], compact: true });
}

function loadProg() {
  progDirty = false;
  return prog.load({ inst: els.inst.value, db: els.db.value, type: instType() });
}

function setSideTab(name) {
  sideTab = name === 'prog' && progSupported(instType()) ? 'prog' : 'tables';
  for (const k of ['tables', 'prog']) {
    const on = k === sideTab;
    els.sideT[k].classList.toggle('on', on);
    els.sideT[k].setAttribute('aria-selected', String(on));
    els.sideP[k].hidden = !on;
  }
  if (sideTab === 'prog' && progDirty) loadProg();
}

// 实例类型变了：支持的类型显示页签，其余隐藏并回到“表”。
function syncSideTabs() {
  const ok = progSupported(instType());
  els.sideTabs.hidden = !ok;
  if (!ok && sideTab === 'prog') setSideTab('tables');
}

els.sideT.tables.addEventListener('click', () => setSideTab('tables'));
els.sideT.prog.addEventListener('click', () => setSideTab('prog'));
els.sideTabs.addEventListener('keydown', (e) => {
  if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
  e.preventDefault();
  const to = e.key === 'ArrowRight' ? 'prog' : 'tables';
  setSideTab(to);
  els.sideT[to].focus();
});
els.progQ.addEventListener('input', () => prog.setFilter(els.progQ.value));

const keyBrowser = createKeyBrowser({
  container: els.tree,
  onPick: (cmd) => {
    if (editor.value.trim()) addTab(cmd, cur().inst, cur().db); else { editor.value = cmd; setMsg(''); }
  },
});

let redisSeq = 0;
// Redis：库提示条取 /meta 的 policy，左侧换成 key 浏览，编辑器补全换成命令名，示例换成 Redis 示例。
async function loadRedisPanel() {
  const my = ++redisSeq;
  const inst = els.inst.value;
  const db = els.db.value;
  await tree.load({ inst: '', db: '' }); // 作废 MySQL 对象树的在途请求（会触发 onLoaded(null)）
  if (my !== redisSeq) return;
  editor.setTables(REDIS_COMMANDS, t('命令'));
  sampleCtx = { table: '', cols: [], pk: '' };
  drawSamples();
  if (!inst || !db) { drawPolicy(null); return keyBrowser.load({ inst: '', db: '' }); }
  api('GET', `/api/v1/query/instances/${encodeURIComponent(inst)}/databases/${encodeURIComponent(db)}/meta`).then((m) => {
    if (my === redisSeq) drawPolicy(m);
  }).catch(() => { if (my === redisSeq) drawPolicy(null); });
  return keyBrowser.load({ inst, db, match: toMatch(els.treeQ.value) });
}

function loadTree() {
  progDirty = true; // 库变了：可编程对象列表作废，页签正开着就立刻重取
  prog.clear();
  if (sideTab === 'prog' && progSupported(instType())) loadProg();
  if (isRedis(instType())) return loadRedisPanel();
  redisSeq++;
  keyBrowser.cancel();
  return tree.load({ inst: els.inst.value, db: els.db.value, type: instType() });
}

let dbSeq = 0;
async function loadDatabases(wantDb) {
  const seq = ++dbSeq;
  els.db.disabled = true;
  els.db.innerHTML = '<option value="">' + t('加载中…') + '</option>';
  if (!els.inst.value) {
    els.db.innerHTML = '<option value="">' + t('（先选择实例）') + '</option>';
    els.db.disabled = false;
    return;
  }
  if (isRedis(instType())) { // Redis 的“库”是 db 编号，下拉里带键数（取不到不显示）
    try {
      const list = await api('GET', `/api/v1/query/instances/${encodeURIComponent(els.inst.value)}/redis/databases`);
      if (seq !== dbSeq) return;
      els.db.disabled = false;
      els.db.replaceChildren(...list.map((d) => new Option(d.keys >= 0 ? `db${d.db} (${d.keys.toLocaleString('en-US')})` : `db${d.db}`, d.db)));
      const want = wantDb ? String(wantDb).replace(/^db/i, '') : '';
      if (want && list.some((d) => d.db === want)) els.db.value = want;
    } catch (e) {
      if (seq !== dbSeq) return;
      els.db.disabled = false;
      els.db.innerHTML = '<option value="">' + t('（加载失败）') + '</option>';
      const d = document.createElement('div');
      d.className = 'alert err';
      d.textContent = e.message;
      els.tree.replaceChildren(d);
    }
    return;
  }
  const r = await fragment(`/ui/query/databases?instance=${encodeURIComponent(els.inst.value)}`);
  if (seq !== dbSeq) return;
  els.db.disabled = false;
  if (!r.ok) {
    els.db.innerHTML = '<option value="">' + t('（加载失败）') + '</option>';
    els.tree.innerHTML = r.html; // 错误提示放在对象树区域
    return;
  }
  els.db.innerHTML = r.html;
  if (wantDb && [...els.db.options].some((o) => o.value === wantDb)) els.db.value = wantDb;
}

async function selectContext(instId, dbName) {
  if (instId && [...els.inst.options].some((o) => o.value === instId)) els.inst.value = instId;
  updateBadges();
  await loadDatabases(dbName);
  cur().inst = els.inst.value;
  cur().db = els.db.value;
  await loadTree();
}

els.inst.addEventListener('change', async () => { await selectContext(els.inst.value, ''); saveState(); });
els.db.addEventListener('change', () => { cur().db = els.db.value; saveState(); loadTree(); });
els.treeQ.addEventListener('input', () => { if (!isRedis(instType())) tree.setFilter(els.treeQ.value); });
els.treeQ.addEventListener('keydown', (ev) => { // Redis：回车才按模式重新扫描（避免每敲一个字就对线上 Redis 发 SCAN）
  if (ev.key === 'Enter' && isRedis(instType())) { ev.preventDefault(); keyBrowser.search(toMatch(els.treeQ.value)); }
});

// ---- 格式化 / 示例 ----
els.fmt.addEventListener('click', () => {
  const v = editor.value;
  if (!v.trim()) return setMsg(t('编辑器是空的'), 'warn');
  editor.value = formatSQL(v);
  setMsg('');
});

els.sample.addEventListener('change', () => {
  const s = samples[Number(els.sample.value)];
  els.sample.value = '';
  if (!s) return;
  if (editor.value.trim()) addTab(s.sql, cur().inst, cur().db);
  else { editor.value = s.sql; setMsg(''); }
});

// ---- 运行 / 取消 ----
let timer = 0;

function setBusy(on) {
  els.run.disabled = on || noRun;
  els.runAll.disabled = on || noRun;
  els.explain.disabled = on || noRun || noPlanType(instType());
  els.run.setAttribute('aria-busy', String(on));
  els.cancel.hidden = !on;
  clearInterval(timer);
  if (on) {
    const t0 = Date.now();
    els.elapsed.textContent = '0.0s';
    timer = setInterval(() => (els.elapsed.textContent = ((Date.now() - t0) / 1000).toFixed(1) + 's'), 100);
  }
}

function emptyBox(text) {
  const d = document.createElement('div');
  d.className = 'res-empty';
  d.textContent = text;
  return d;
}

// 当前标签的结果区：有结果也有执行计划时出“结果 / 执行计划”页签。
function drawResult() {
  const v = viewOf(cur().id);
  if (batchUi.draw(els.result, v)) return; // 上一次是“运行全部”：一条语句一个结果页签
  els.result.replaceChildren();
  if (running && running.tabId === cur().id) { els.result.append(emptyBox(t('正在执行…'))); return; }
  if (!v.res && !v.plan) { els.result.append(emptyBox(t('运行查询后，结果显示在这里'))); return; }
  if (v.res && v.plan) {
    const bar = document.createElement('div');
    bar.className = 'rtabs';
    [['res', t('结果')], ['plan', t('执行计划')]].forEach(([k, label]) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = k === v.rtab ? 'on' : '';
      b.textContent = label;
      b.addEventListener('click', () => { v.rtab = k; drawResult(); });
      bar.append(b);
    });
    els.result.append(bar);
  }
  const body = document.createElement('div');
  body.className = 'res-body'; // 样式见 app.css：占满结果框剩余的高度，结果表在框里滚动
  els.result.append(body);
  const showPlan = (v.rtab === 'plan' && v.plan) || !v.res;
  const redisRes = !!(v.req && isRedis(v.req.type));
  const multiRes = !!(v.req && isMulti(v.req.type)); // PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦：不显示“去导出”，不估算总行数
  if (showPlan) {
    renderPlan(body, v.plan);
  } else {
    const res = v.res;
    renderResult(body, res, {
      title: t('结果'),
      dbType: v.req ? v.req.type : '',
      exportHref: wb.dataset.exportEnabled === 'true' && !redisRes && !multiRes ? '/sqlworkflow/export/new' : '',
      onNext: redisRes ? (cmd) => {
        if (!editor.replaceText(v.req.sql, cmd)) editor.value = cmd;
        execute(false);
      } : null,
      estimate: res.truncated && v.req && !redisRes && !multiRes ? () => {
        if (res._est === undefined) {
          res._est = api('POST', '/api/v1/query/estimate', { instance_id: v.req.instance_id, db: v.req.db, sql: v.req.sql }).then((r) => (r ? r.rows : -1));
        }
        return res._est;
      } : null,
    });
  }
}

// SQL 防火墙要求说明原因：原因随语句进入审计（命中记录）。
function askReason(message) {
  return riskConfirm({
    title: t('需要说明查询原因'),
    lines: String(message || t('该语句命中了需要说明原因的规则。')).split('\n').filter(Boolean),
    okText: t('提交并执行'), cancelText: t('取消'),
    reason: { label: t('原因'), required: true, min: 4, max: 200, placeholder: t('查询原因（会进入审计）') },
  });
}

async function execute(isExplain) {
  if (running) return;
  if (noRun) return setMsg(t('当前账号没有 query.run 权限，不能运行查询或查看执行计划。'), 'warn');
  if (!els.inst.value) return setMsg(t('请先选择实例'));
  const type = instType();
  const redis = isRedis(type);
  const multi = isMulti(type);
  // MySQL 系（不是 Redis / MongoDB / PostgreSQL / ClickHouse / Oracle / 达梦 / SQL Server）：脚本开头的 SET 语句自动随“光标所在语句”一起提交
  const parts = !redis && !isMongo(type) && !multi && !isMSSQL(type) ? editor.partsToRun() : null;
  let sql = (parts ? parts.stmt : redis ? editor.lineToRun() : isMongo(type) ? editor.allToRun() : editor.sqlToRun()).trim();
  if (!sql) return setMsg(redis ? t('先输入一条命令') : isMongo(type) ? t('先输入一条语句') : t('先输入一条 SQL'), 'warn');
  if (isExplain && multi) return setMsg(NEW_ENGINE[type].planMsg, 'warn');
  if (isExplain && !redis) {
    if (isMSSQL(type)) return setMsg(t('SQL Server 暂不支持执行计划。'), 'warn');
    if (!/^\s*(explain|desc|describe)\b/i.test(sql)) sql = 'EXPLAIN ' + sql;
  }
  if (parts && parts.prelude) sql = parts.prelude + '\n;\n' + sql; // 前置 SET 放在最前面（EXPLAIN 只加在查询上）

  const tab = cur();
  const v = viewOf(tab.id);
  if (v.batch && !isExplain) { // 上一次是“运行全部”：有固定的页签就把这条并进同一排页签，否则回到单个结果
    if (!redis && !isMongo(type) && batchUi.keepsPins(v)) return batchUi.runSingle(v, tab, { sql: (parts ? parts.stmt : sql).trim(), run: sql });
    v.batch = null;
    v.res = v.plan = null;
  }
  const req = { instance_id: Number(els.inst.value), db: els.db.value, sql, limit: Number(els.limit.value) };
  v.msg = null;
  drawMsg();
  running = { tabId: tab.id, ctrl: new AbortController() };
  setBusy(true);
  renderTabs();
  drawResult();
  const instName = els.inst.selectedOptions[0] ? els.inst.selectedOptions[0].textContent.trim() : '';
  try {
    let res;
    for (;;) {
      try {
        res = await api('POST', '/api/v1/query/execute', req, { signal: running.ctrl.signal });
        break;
      } catch (e) {
        // SQL 防火墙的“要求填写原因”规则命中：弹窗收集原因，带上 reason 原样重发一次
        if (e.code !== 'reason_required' || req.reason) throw e;
        const r = await askReason(e.message);
        if (!r.ok) {
          const c = new Error(t('已取消：没有填写原因，语句没有执行。'));
          c.code = 'canceled';
          throw c;
        }
        req.reason = r.value;
      }
    }
    v.req = { ...req, instName, type };
    if (res.kind === 'explain') { v.plan = res; v.rtab = 'plan'; } else { v.res = res; v.rtab = 'res'; }
  } catch (e) {
    if (!multi && (isExplain || /^\s*(explain|desc|describe)\b/i.test(sql))) v.plan = null; else v.res = null; // 失败时不保留上一次的结果，避免被误认为是这次的输出
    if (!v.res && !v.plan) v.rtab = 'res';
    if (v.plan && !v.res) v.rtab = 'plan';
    if (e.name === 'AbortError') {
      v.msg = { text: t('已取消。'), kind: 'info' };
    } else if (e.status === 401) {
      v.msg = { text: t('登录已过期，请刷新页面重新登录。'), kind: 'err' };
    } else if (e.code === 'no_privilege' && e.data && e.data.database) {
      const target = e.data.table ? `${e.data.database}.${e.data.table}` : dbLabel(e.data.database);
      const href = applyHref(e.data.table);
      v.msg = { text: e.message, kind: 'err', extra: href ? { href, text: t('申请 {target} 的查询权限', { target }) } : { plain: t('（需要申请：{target}）', { target }) } };
    } else {
      v.msg = { text: e.message, kind: e.code === 'timeout' || e.code === 'busy' ? 'warn' : e.code === 'canceled' ? 'info' : 'err' };
    }
  } finally {
    running = null;
    setBusy(false);
    renderTabs();
    drawMsg();
    drawResult();
  }
}

els.run.addEventListener('click', () => execute(false));
els.explain.addEventListener('click', () => execute(true));
els.runAll.addEventListener('click', () => runAll());
els.batchOpts.addEventListener('click', () => batchUi.togglePop(els.batchOpts));
editor.onCursor(() => refreshRunAll());

function runAll() {
  if (running) return;
  if (noRun) return setMsg(t('当前账号没有 query.run 权限，不能运行查询或查看执行计划。'), 'warn');
  if (!els.inst.value) return setMsg(t('请先选择实例'));
  const type = instType();
  if (isRedis(type) || isMongo(type)) return;
  batchUi.runAll();
}

// “运行全部”按钮上的条数（有选区时是选中的条数），以及编辑器里的状态圆点跟着文字改动。
let rcTimer = 0;
function refreshRunAll() {
  clearTimeout(rcTimer);
  rcTimer = setTimeout(() => {
    const type = instType();
    const n = isRedis(type) || isMongo(type) ? 0 : batchUi.count();
    els.runAllN.textContent = n > 1 ? t('（{n} 条）', { n }) : '';
    batchUi.sync(viewOf(cur().id));
  }, 120);
}
els.cancel.addEventListener('click', () => running && running.ctrl.abort());

// ---- 我的 SQL：收藏 + 执行历史 ----
initMySQL({
  // 要收藏的 SQL：选中的文本，否则光标所在语句
  context: () => {
    const o = els.inst.options[els.inst.selectedIndex];
    return { sql: (editor.sqlToRun() || '').trim(), instance: o ? o.textContent.trim() : '', db: els.db.value || '' };
  },
  paste: (sql) => editor.insert(sql),
  newTab: (sql, instance, db) => {
    const opt = [...els.inst.options].find((o) => o.textContent.trim() === instance);
    addTab(sql, opt ? opt.value : cur().inst, opt ? db : cur().db);
  },
});

// ---- 启动 ----
// /query?inst=<实例ID或名称>&db=<库> 预选实例与库（作用于当前标签）。
(function preselect() {
  const qp = new URLSearchParams(location.search);
  const want = qp.get('inst');
  if (!want) return;
  const opt = [...els.inst.options].find((o) => o.value === want) || [...els.inst.options].find((o) => o.textContent.trim() === want);
  if (!opt) return;
  cur().inst = opt.value;
  cur().db = qp.get('db') || '';
})();

renderTabs();
switching = true;
editor.value = cur().sql;
switching = false;
drawMsg();
drawResult();
selectContext(cur().inst, cur().db).then(saveState);

// 按钮权限只是显示层（服务端仍逐接口校验）：取不到权限时保持原样。
loadButtons().then((b) => {
  if (b && !can(b, 'query.objinfo')) {
    noObjInfo = true;
    syncSideTabs();
  }
  if (!b || can(b, 'query.run')) return;
  noRun = true;
  els.run.disabled = true;
  els.run.title = NO_RUN_TITLE;
  els.runAll.disabled = true;
  els.runAll.title = NO_RUN_TITLE;
  els.explain.disabled = true;
  els.explain.title = NO_RUN_TITLE;
  setMsg(t('当前账号没有 query.run 权限，不能运行查询。'), 'warn');
});
