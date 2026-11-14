/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_multi_engine.mjs
 * @time: 2026/10/28
 */
// PostgreSQL / MongoDB / ClickHouse 接入在线查询 / 权限申请 / 实例列表的页面级测试：
// 自起假后端（multi_engine_fake.mjs，接口形状按 docs/multi-engine-query.md）+ 无头 Chrome（CDP），不依赖第三方包。
// 用法：node scripts/frontend-test/run_multi_engine.mjs [chrome路径]    环境变量 ME_HTTP_PORT（默认 8906）/ ME_CDP_PORT（默认 9243）。
// 结束时只按 PID 结束自己启动的 Chrome。
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { startServer, calls } from './multi_engine_fake.mjs';

const HTTP = +(process.env.ME_HTTP_PORT || 8906);
const CDP = +(process.env.ME_CDP_PORT || 9243);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const server = await startServer(HTTP);
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'me-chrome-'));
const chrome = spawn(chromePath, ['--headless=new', `--remote-debugging-port=${CDP}`, `--user-data-dir=${profile}`, '--no-first-run', '--disable-gpu', 'about:blank'], { stdio: 'ignore' });
let cleaned = false;
function cleanup() {
  if (cleaned) return;
  cleaned = true;
  try { if (process.platform === 'win32') spawnSync('taskkill', ['/PID', String(chrome.pid), '/T', '/F'], { stdio: 'ignore' }); else chrome.kill(); } catch (_) { /* 已退出 */ }
  server.close();
  setTimeout(() => { try { fs.rmSync(profile, { recursive: true, force: true }); } catch (_) { /* 占用中则留给系统清理 */ } }, 1500).unref();
}
process.on('exit', cleanup);

async function getWs() {
  for (let i = 0; i < 80; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${CDP}/json`)).json();
      const p = list.find((x) => x.type === 'page');
      if (p) return p.webSocketDebuggerUrl;
    } catch (_) { /* 等 Chrome 起来 */ }
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error('Chrome 未启动');
}
const ws = new WebSocket(await getWs());
await new Promise((r) => ws.addEventListener('open', r));
let id = 0;
const pend = new Map();
let consoleErrors = [];
ws.addEventListener('message', (m) => {
  const d = JSON.parse(m.data);
  if (d.id && pend.has(d.id)) {
    const { res, rej } = pend.get(d.id);
    pend.delete(d.id);
    if (d.error) rej(new Error(d.error.message)); else res(d.result);
  } else if (d.method === 'Runtime.exceptionThrown') {
    consoleErrors.push(d.params.exceptionDetails.text + ' ' + ((d.params.exceptionDetails.exception || {}).description || ''));
  } else if (d.method === 'Log.entryAdded' && d.params.entry.level === 'error') {
    consoleErrors.push(d.params.entry.text + ' ' + (d.params.entry.url || ''));
  }
});
const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id; pend.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
await send('Runtime.enable');
await send('Page.enable');
await send('Log.enable');

async function ev(expr) {
  const r = await send('Runtime.evaluate', { expression: `(async()=>{${expr}})()`, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error('页面异常：' + ((r.exceptionDetails.exception || {}).description || r.exceptionDetails.text));
  return r.result.value;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const DESKTOP = { width: 1366, height: 900, deviceScaleFactor: 1, mobile: false };
await send('Emulation.setDeviceMetricsOverride', DESKTOP);
// 设置 ME_SHOT=目录 时在关键位置截图，便于人工看版面
async function shot(name) {
  if (!process.env.ME_SHOT) return;
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.mkdirSync(process.env.ME_SHOT, { recursive: true });
  fs.writeFileSync(path.join(process.env.ME_SHOT, name + '.png'), Buffer.from(r.data, 'base64'));
}
async function waitFor(expr, what, ms = 6000) {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) {
    if (await ev(`return !!(${expr});`).catch(() => false)) return;
    await sleep(60);
  }
  throw new Error('等待超时：' + what);
}
let pass = 0, fail = 0;
async function t(name, fn) {
  try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; console.log('  FAIL ' + name + '\n       ' + e.message); }
}
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b)} 实得 ${JSON.stringify(a)}`); };
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };
const has = (s, sub, m) => { if (!String(s).includes(sub)) throw new Error(`${m || ''} 应包含 ${JSON.stringify(sub)}，实得 ${JSON.stringify(String(s).slice(0, 300))}`); };
const q = (sel, prop = 'textContent') => ev(`const e=document.querySelector(${JSON.stringify(sel)}); return e ? e.${prop} : null;`);
const qa = (sel, prop = 'textContent') => ev(`return [...document.querySelectorAll(${JSON.stringify(sel)})].map(e=>e.${prop});`);
const exists = (sel) => ev(`return !!document.querySelector(${JSON.stringify(sel)});`);
const click = (sel) => ev(`document.querySelector(${JSON.stringify(sel)}).click();`);

async function nav(url) {
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}${url}` });
  await sleep(200);
  await waitFor("document.readyState === 'complete'", '页面加载');
}
// ---- 在线查询页的操作 ----
const edVal = () => ev("return ace.edit(document.getElementById('editor')).getValue();");
const setEd = (v) => ev(`ace.edit(document.getElementById('editor')).setValue(${JSON.stringify(v)}, 1);`);
const edMode = () => ev("return ace.edit(document.getElementById('editor')).session.getMode().$id;");
async function openQuery(inst, db) {
  await nav('/query');
  await ev('localStorage.clear();');
  await nav(`/query?inst=${inst}&db=${db}`);
  await waitFor("document.querySelector('#tree .tree-head, #tree .alert, #tree .tree-hint') && document.getElementById('db').value", '库表树加载');
  await sleep(150);
}
async function pickInst(instId, expectDb) {
  await ev(`const s=document.getElementById('inst'); s.value=${JSON.stringify(String(instId))}; s.dispatchEvent(new Event('change'));`);
  await waitFor(`document.getElementById('db').value === ${JSON.stringify(expectDb)} && document.querySelector('#tree .tree-head, #tree .alert')`, '切换实例后库表树');
  await sleep(150);
}
async function runQuery() {
  const n = calls.filter((c) => c.url === '/api/v1/query/execute').length;
  await click('#run');
  for (let i = 0; i < 100 && calls.filter((c) => c.url === '/api/v1/query/execute').length <= n; i++) await sleep(50);
  await waitFor("document.getElementById('run').getAttribute('aria-busy') === 'false' && !document.querySelector('#result .res-empty')?.textContent.includes('正在执行')", '执行完成');
  await sleep(60);
}
const lastExec = () => calls.filter((c) => c.url === '/api/v1/query/execute').slice(-1)[0];
const treeRows = () => qa('#tree .tr-row .name .t');
const clickTable = (name) => ev(`const b=[...document.querySelectorAll('#tree .tr-row .name')].find(e=>e.dataset.table===${JSON.stringify(name)}); if(!b) throw new Error('没有表 ${name}'); b.click();`);
const sampleLabels = () => qa('#sample option', 'textContent');
const chipTexts = () => qa('#policy .chip');
const alertTexts = () => qa('#result .alert');
const hrefOf = (sel) => q(sel, 'href');

console.log(`多引擎页面测试（http://127.0.0.1:${HTTP}/query）`);

// =============== PostgreSQL ===============
await openQuery(4, 'shop');
await t('PG：徽标、编辑器模式、格式化/执行计划置灰并说明', async () => {
  eq(await q('#b-type'), 'pgsql');
  eq(await edMode(), 'ace/mode/pgsql');
  eq(await q('#fmt', 'disabled'), true);
  eq(await q('#explain', 'disabled'), true);
  has(await q('#explain', 'title'), 'EXPLAIN');
  has(await q('#fmt', 'title'), '暂不提供格式化');
});
await t('PG：左侧树按 schema 分组（public 第一）、种类标注', async () => {
  eq(await qa('#tree .tr-group .gn'), ['public', 'audit']);
  eq(await treeRows(), ['orders', 'v_orders', 'mv_stats', 'events', 'ext_log']);
  eq(await qa('#tree .tr-row .meta'), ['12k', '视图', '物化视图', '900k', '外部表']);
  eq(await q('#tree .tr-row.locked .name', 'dataset.table'), 'public.v_orders');
});
await t('PG：schema 分组可折叠，再点展开', async () => {
  await click('#tree .tr-group:nth-of-type(1)');
  eq(await treeRows(), ['events', 'ext_log']);
  await click('#tree .tr-group');
  eq((await treeRows()).length, 5);
});
await t('PG：搜索框只按表名过滤，不匹配字段名 / 注释，也不发按字段搜索请求', async () => {
  const typeQ = (v) => ev(`const i=document.getElementById('tree-q'); i.value=${JSON.stringify(v)}; i.dispatchEvent(new Event('input'));`);
  const before = calls.length;
  await typeQ('ORDERS');
  eq(await treeRows(), ['orders', 'v_orders'], '不区分大小写的表名子串');
  await typeQ('audit.ev');
  eq(await treeRows(), ['events'], 'schema.表名 也能搜');
  await typeQ('user_id');
  eq(await treeRows(), [], '字段名不匹配');
  has(await q('#tree .tree-hint'), '没有匹配的表名');
  await typeQ('订单');
  eq(await treeRows(), [], '表注释不匹配');
  await sleep(700);
  eq(calls.slice(before).filter((c) => c.url.includes('/search')).length, 0, '不调用 search');
  await typeQ('');
  eq((await treeRows()).length, 5);
});
await t('PG：点表名生成带 schema 且列名正确加引号的 SELECT', async () => {
  await clickTable('public.orders');
  await waitFor("ace.edit(document.getElementById('editor')).getValue().length > 0", '生成语句');
  eq(await edVal(), 'SELECT id, user_id, "update", "Status", created_at\nFROM public.orders\nLIMIT 100;');
});
await t('PG：展开字段并点字段插入（带引号）', async () => {
  await click('#tree .tr-row .arrow');
  await waitFor("document.querySelector('#tree .tr-col')", '字段列表');
  eq((await qa('#tree .tr-col .cn')).length, 5);
  await setEd('select ');
  await ev("ace.edit(document.getElementById('editor')).navigateFileEnd();");
  await ev("[...document.querySelectorAll('#tree .tr-col')].find(b=>b.textContent.includes('Status')).click();");
  eq(await edVal(), 'select "Status"');
});
await t('PG：提示条只剩要处理的（其余表无权限）；脱敏已启用、权限范围、只读范围、超时都在“运行”的悬停提示里', async () => {
  const chips = (await chipTexts()).join('|');
  truthy(!chips.includes('数据脱敏已启用'), '正常状态的说明不占提示条');
  has(await q('#run', 'title'), '数据脱敏已启用');
  truthy(!chips.includes('不做脱敏'), '脱敏总开关开着时不应提示不做脱敏');
  truthy(!chips.includes('只读：') && !chips.includes('超时'), '只读范围与超时不再占提示条');
  has(await q('#run', 'title'), 'EXPLAIN（不含 ANALYZE）'); // 改放进“运行”按钮的悬停提示
  has(await q('#run', 'title'), '超时');
  has(await q('#run', 'title'), 'public.orders');
  has(chips, '其余 1 张表无权限');
});
await t('按键行与结果区之间：提示条隐藏、没有消息时只隔 4px；有提示条时它紧跟在按键行下面 4px', async () => {
  const r = await ev(`const pol = document.getElementById('policy'); const msg = document.getElementById('msg'); const was = { h: pol.hidden, m: msg.innerHTML };
    const top = (s) => document.querySelector(s).getBoundingClientRect();
    msg.replaceChildren();
    pol.hidden = true;
    const hiddenGap = Math.round(top('#result').top - top('.wb-main > .toolbar').bottom);
    pol.hidden = false;
    const chipGap = Math.round(top('#policy').top - top('.wb-main > .toolbar').bottom);
    const msgDisplay = getComputedStyle(msg).display;
    pol.hidden = was.h; msg.innerHTML = was.m;
    return { hiddenGap, chipGap, msgDisplay, btnH: Math.round(top('#run').height) };`);
  eq(r.hiddenGap, 4, '提示条隐藏时按键行到结果区 4px');
  eq(r.chipGap, 4, '有提示条时按键行到提示条 4px');
  eq(r.msgDisplay, 'none', '空消息区不占位');
  eq(r.btnH, 26);
});
await t('PG：示例下拉含 PG 示例和拦截反例，且 schema 限定', async () => {
  const labels = (await sampleLabels()).join('|');
  has(labels, 'EXPLAIN（普通语句，不带 ANALYZE）');
  has(labels, 'pg_sleep()（危险函数）');
  has(labels, 'EXPLAIN ANALYZE');
  has(labels, 'pg_authid');
  eq(await q('#sample option', 'textContent'), '示例 SQL…');
  await ev("const s=document.getElementById('sample'); s.value=String([...s.options].findIndex(o=>o.textContent.startsWith('前 50 行')) - 1); s.dispatchEvent(new Event('change'));");
  await sleep(300);
  has(await edVal(), 'from public.orders');
});
await t('PG：运行 -> 结果表头带类型、warnings/notices、无“不做脱敏”条、截断提示无导出与估算', async () => {
  await setEd('select id, name from public.orders big');
  await runQuery();
  eq(lastExec().body, { instance_id: 4, db: 'shop', sql: 'select id, name from public.orders big', limit: 500 });
  eq(await qa('#result table.grid th .ctype'), [], '表头下面不再显示字段类型');
  eq(await qa('#result table.grid th:not(.rn)', 'title'), ['int4', 'text'], '类型只在悬停提示里');
  const al = await alertTexts();
  truthy(!al.join('|').includes('不做脱敏'), '脱敏已开启时结果上方不应再提示不做脱敏');
  has(al.join('|'), '警告一');
  has(al.join('|'), '提示一');
  has(al.join('|'), '提示二');
  has(al.join('|'), '结果已按 1000 行上限截断，可加过滤条件或分页读取。');
  truthy(!al.join('|').includes('旧提示'), '不应有旧 notice');
  eq(await exists('#result a[href*="export"]'), false, '无导出链接');
  eq(calls.some((c) => c.url === '/api/v1/query/estimate'), false, '不估算总行数');
  eq(await q('#result td.null'), 'NULL');
  eq(await exists('#result .sql-fold'), false, '不再有“实际执行的 SQL”折叠');
  eq((await qa('#result .res-meta .chip')).includes('select'), true, 'stmt_kind 芯片');
  await shot('pg_result');
});
await t('PG：EXPLAIN 当普通语句运行，没有执行计划页签', async () => {
  await setEd('explain select 1 from public.orders');
  await runQuery();
  eq(await exists('#result .rtabs'), false);
  eq(await exists('#result table.grid'), true);
});
await t('PG：引擎拒绝 -> 错误条原文；失败不保留上次结果', async () => {
  await setEd('select forbidden from public.orders');
  await runQuery();
  has(await q('#msg .alert'), '只允许 SELECT 等只读语句（测试拒绝）');
  eq(await exists('#result table.grid'), false);
});
await t('PG：无权限 -> 申请链接预填 schema.table；busy -> 警告条', async () => {
  await setEd('select * from public.secret');
  await runQuery();
  has(await q('#msg .alert'), '你无 shop.public.secret 表的查询权限');
  const href = await hrefOf('#msg .alert a');
  has(href, 'instance=4');
  has(href, 'db=shop');
  has(href, 'table=public.secret');
  await setEd('select busyq');
  await runQuery();
  has(await q('#msg .alert'), '已达 3 条');
  has(await q('#msg .alert', 'className'), 'warn');
});
await t('PG：EXPLAIN 失败时不保留旧结果（EXPLAIN 不走执行计划页签）', async () => {
  await setEd('select 1');
  await runQuery();
  eq(await exists('#result table.grid'), true);
  await setEd('explain select forbidden');
  await runQuery();
  eq(await exists('#result table.grid'), false);
});

// =============== MongoDB ===============
await t('Mongo：切换后纯文本模式、集合树、搜索框', async () => {
  await pickInst(5, 'appdb');
  eq(await q('#b-type'), 'mongo');
  eq(await edMode(), 'ace/mode/text');
  eq(await treeRows(), ['orders', 'order-items', 'v_active', 'metrics']);
  eq(await q('#tree-q', 'placeholder'), '搜索集合名');
  eq(await q('#fmt', 'disabled'), true);
  eq(await q('#explain', 'disabled'), true);
  has(await q('#explain', 'title'), '不支持 explain');
  eq(await q('#sample option', 'textContent'), '示例语句…');
  has(await q('#run', 'title'), '数据脱敏已启用');
  has(await q('#run', 'title'), 'find / findOne');
  eq(await qa('#tree .tr-group'), []);
  has(await q('#tree .tree-head .n'), '4 个集合');
  await shot('mongo_tree');
});
await t('Mongo：点集合生成 find，编辑器非空时放进新标签；非标识符集合名用 getCollection', async () => {
  await setEd('');
  await clickTable('orders');
  await waitFor("ace.edit(document.getElementById('editor')).getValue()", '生成语句');
  eq(await edVal(), 'db.orders.find({}).limit(10)');
  const tabsBefore = await ev("return document.querySelectorAll('#tabs .tab').length;");
  await clickTable('order-items');
  await waitFor(`document.querySelectorAll('#tabs .tab').length === ${tabsBefore + 1}`, '新标签');
  await sleep(300);
  eq(await edVal(), 'db.getCollection("order-items").find({}).limit(10)');
});
await t('Mongo：展开集合字段；没有按字段搜索请求', async () => {
  await click('#tree .tr-row .arrow');
  await waitFor("document.querySelector('#tree .tr-col')", '字段');
  const before = calls.length;
  await ev("const i=document.getElementById('tree-q'); i.value='order-'; i.dispatchEvent(new Event('input'));");
  await sleep(700);
  eq(calls.slice(before).filter((c) => c.url.includes('/search')).length, 0, '不调用 search');
  eq(await treeRows(), ['order-items']);
  await ev("const i=document.getElementById('tree-q'); i.value=''; i.dispatchEvent(new Event('input'));");
});
await t('Mongo：运行取整个编辑器内容（含分号与空行），不按分号切分', async () => {
  const stmt = 'db.orders.find({note: "a;b"},\n\n  {status: 1}).limit(5)';
  await setEd(stmt);
  await ev("ace.edit(document.getElementById('editor')).moveCursorTo(0, 3); ace.edit(document.getElementById('editor')).clearSelection();");
  await runQuery();
  eq(lastExec().body.sql, stmt);
  eq(await qa('#result table.grid th .ctype'), [], 'mongo 没有列类型');
  eq(await exists('#result .sql-fold'), false, '不再有“实际执行的语句”折叠');
  eq(await q('#result td:nth-child(4)'), '[{"sku":"a;b"}]');
  truthy(!(await alertTexts()).join('|').includes('不做脱敏'), '脱敏已开启时不应提示不做脱敏');
});
await t('Mongo：示例含库级语句、反例；选中文本只运行选中部分', async () => {
  const labels = (await sampleLabels()).join('|');
  has(labels, 'getCollectionNames（需库级权限）');
  has(labels, '$where');
  has(labels, 'getSiblingDB');
  await setEd('db.orders.find({}).limit(1)\ndb.users.find({}).limit(2)');
  await ev("const e=ace.edit(document.getElementById('editor')); e.selection.setRange(new (ace.require('ace/range').Range)(1,0,1,30));");
  await runQuery();
  eq(lastExec().body.sql, 'db.users.find({}).limit(2)');
});
await t('Mongo：引擎拒绝显示原文', async () => {
  await setEd('db.orders.insertOne({forbidden: 1})');
  await runQuery();
  has(await q('#msg .alert'), '不允许的方法');
});

// =============== ClickHouse ===============
await t('ClickHouse：sql 模式、种类图标/标注、示例表避开字典', async () => {
  await pickInst(6, 'logs');
  eq(await q('#b-type'), 'clickhouse');
  eq(await edMode(), 'ace/mode/sql');
  eq(await treeRows(), ['dict_country', 'events', 'events_mv', 'v_recent']);
  eq(await qa('#tree .tr-row .meta'), ['字典', '8M', '物化视图', '视图']);
  eq(await qa('#tree .tr-group'), []);
  eq(await q('#explain', 'disabled'), true);
  has(await q('#explain', 'title'), 'ClickHouse');
  await sleep(300);
  has((await sampleLabels()).join('|'), '前 50 行：events');
  await shot('ch_tree');
});
await t('ClickHouse：点表生成 select（format 列加反引号）；运行显示 notices 与列类型', async () => {
  await setEd('');
  await clickTable('events');
  await waitFor("ace.edit(document.getElementById('editor')).getValue()", '生成语句');
  eq(await edVal(), 'SELECT dt, user_id, `format`, msg\nFROM events\nLIMIT 100;');
  await setEd('select 1 format JSON');
  await runQuery();
  eq(await qa('#result table.grid th .ctype'), [], '表头下面不再显示字段类型');
  eq(await qa('#result table.grid th:not(.rn)', 'title'), ['UInt8']);
  const al = (await alertTexts()).join('|');
  has(al, '已忽略 FORMAT 子句');
  truthy(!al.includes('不做脱敏'), '脱敏已开启时不应提示不做脱敏');
  await setEd('select 1 big');
  await runQuery();
  has((await alertTexts()).join('|'), '结果超过 4 MiB 字节上限，已截断。');
  eq(await exists('#result a[href*="export"]'), false);
});
await t('ClickHouse：示例含 system.users/SETTINGS/file() 反例', async () => {
  const labels = (await sampleLabels()).join('|');
  has(labels, 'system.users');
  has(labels, 'SETTINGS');
  has(labels, 'file()');
  has(labels, 'merge()');
});

// =============== 切回 MySQL / SQL Server / Redis 完全恢复 ===============
await t('切回 MySQL：模式、按钮、文案、示例、提示条、树结构全部恢复原样', async () => {
  await pickInst(1, 'shop_my');
  eq(await q('#b-type'), 'mysql');
  eq(await edMode(), 'ace/mode/mysql');
  eq(await q('#fmt', 'disabled'), false);
  eq(await q('#explain', 'disabled'), false);
  eq(await q('#explain', 'title'), '');
  eq(await q('#fmt', 'title'), '');
  eq(await q('#tree-q', 'placeholder'), '搜索表名');
  eq(await q('#sample option', 'textContent'), '示例 SQL…');
  const chips = (await chipTexts()).join('|');
  truthy(!chips.includes('该类型的查询结果不做脱敏'), 'MySQL 不应有不做脱敏提示');
  has(await q('#run', 'title'), '数据脱敏');
  has(await q('#run', 'title'), '只读：SELECT / WITH / SHOW / DESC / EXPLAIN');
  eq(await treeRows(), ['orders', 'v_orders']);
  eq(await qa('#tree .tr-group'), []);
});
await t('MySQL 结果：脱敏标记、旧 notice、导出链接、总行数估算保持', async () => {
  await setEd('select id, phone from orders big');
  await runQuery();
  has(await q('#result .res-meta'), '已脱敏：phone');
  eq(await qa('#result table.grid th.masked'), ['phone'], '脱敏列表头标 masked，没有盾牌小图标');
  eq(await exists('#result table.grid th .shield'), false);
  const green = await ev("const c = (s) => getComputedStyle(document.querySelector(s)).color; return [c('#result th.masked'), c('#result th:not(.masked):not(.rn)')];");
  truthy(green[0] !== green[1], '脱敏列表头颜色和普通列不同：' + green.join(' / '));
  has((await alertTexts()).join('|'), '旧提示：已改写 LIMIT');
  eq((await alertTexts()).join('|').includes('不做脱敏'), false);
  eq(await exists('#result a[href*="export"]'), true, 'MySQL 保留导出链接');
  await waitFor("[...document.querySelectorAll('#result .res-note')].some(e=>e.textContent.includes('估算约 4,321 行'))", '估算总行数');
  eq(await qa('#result table.grid th .ctype'), []);
});
await t('MySQL 执行计划页签仍可用', async () => {
  await click('#explain');
  await waitFor("document.querySelector('#result .rtabs')", '计划页签');
  has(await q('#result .rtabs'), '执行计划');
});
await t('SQL 防火墙：要求填写原因 → 弹窗收集 → 带 reason 重发一次，原因太短不能提交', async () => {
  await setEd('select id from needsreason');
  const execs = () => calls.filter((c) => c.url === '/api/v1/query/execute');
  const n = execs().length;
  await click('#run');
  await waitFor("document.querySelector('.cf-dialog')", '原因弹窗');
  has(await q('.cf-dialog'), '需要说明查询原因');
  has(await q('.cf-dialog'), '该表含敏感数据，请说明查询用途。');
  eq(execs().length, n + 1, '先发出一次（没带原因）');
  eq(execs().slice(-1)[0].body.reason, undefined, '第一次不带 reason');
  const okBtn = "[...document.querySelectorAll('.cf-dialog button')].find(b => b.textContent.includes('提交并执行'))";
  await ev("const t=document.querySelector('.cf-dialog textarea'); t.value='ab'; t.dispatchEvent(new Event('input'));");
  eq(await ev(`return ${okBtn}.disabled;`), true, '原因不足 4 个字符时不能提交');
  await ev("const t=document.querySelector('.cf-dialog textarea'); t.value='  月度对账  '; t.dispatchEvent(new Event('input'));");
  eq(await ev(`return ${okBtn}.disabled;`), false);
  await ev(`${okBtn}.click();`);
  await waitFor("document.getElementById('run').getAttribute('aria-busy') === 'false' && !document.querySelector('.cf-dialog')", '重发完成');
  await sleep(80);
  eq(execs().length, n + 2, '只重发一次');
  eq(execs().slice(-1)[0].body.reason, '月度对账', '带上去掉首尾空白的原因');
  has(await q('#result .res-meta'), '已脱敏：phone');
});
await t('SQL 防火墙：取消原因弹窗 → 不重发，提示已取消', async () => {
  await setEd('select id from needsreason');
  const n = calls.filter((c) => c.url === '/api/v1/query/execute').length;
  await click('#run');
  await waitFor("document.querySelector('.cf-dialog')", '原因弹窗');
  await ev("[...document.querySelectorAll('.cf-dialog button')].find(b => b.textContent.includes('取消')).click();");
  await waitFor("document.getElementById('run').getAttribute('aria-busy') === 'false' && !document.querySelector('.cf-dialog')", '已取消');
  await sleep(80);
  eq(calls.filter((c) => c.url === '/api/v1/query/execute').length, n + 1, '取消后不重发');
  has(await q('#msg'), '没有填写原因');
});
await t('Redis：命令模式与 key 浏览不受影响，切回再恢复', async () => {
  await pickInst(3, '0');
  eq(await q('#b-type'), 'redis');
  eq(await edMode(), 'ace/mode/text');
  eq(await q('#tree-q', 'placeholder'), 'key 匹配，如 user:*（回车）');
  eq(await qa('#tree .rk-row .t'), ['user:1', 'k2']);
  eq(await q('#sample option', 'textContent'), '示例命令…');
  await setEd('GET k2');
  await runQuery();
  has((await chipTexts()).join('|'), 'Redis 查询结果不做脱敏');
  eq(await exists('#result .res-unmasked'), false, 'Redis 不出新类型的 unmasked 条');
  await pickInst(4, 'shop');
  eq(await q('#tree-q', 'placeholder'), '搜索表名');
  eq(await qa('#tree .tr-group .gn'), ['public', 'audit']);
  await pickInst(2, 'master');
  eq(await q('#b-type'), 'mssql');
  eq(await edMode(), 'ace/mode/sqlserver');
  eq(await q('#explain', 'disabled'), true);
  has(await q('#explain', 'title'), 'SQL Server 暂不支持执行计划');
  eq(await q('#fmt', 'disabled'), false);
});

// =============== 手机 / 平板宽度：不横向溢出 ===============
const overflow = () => ev('return [document.documentElement.scrollWidth, window.innerWidth];');
for (const [label, width, height] of [['手机宽度 375', 375, 812], ['平板宽度 768', 768, 1024]]) {
  await t(`${label}：在线查询页（PG / Mongo / ClickHouse 结果）无横向溢出`, async () => {
    await send('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: true });
    for (const [inst, db, stmt] of [[4, 'shop', 'select id, name from public.orders big'], [5, 'appdb', 'db.orders.find({})'], [6, 'logs', 'select 1 big']]) {
      await openQuery(inst, db);
      await setEd(stmt);
      await runQuery();
      const w = await overflow();
      truthy(w[0] <= w[1] + 1, `实例 ${inst} 页面横向溢出 ${w}`);
      if (width === 375 && inst === 4) await shot('mobile_pg');
    }
    await send('Emulation.setDeviceMetricsOverride', DESKTOP);
  });
  await t(`${label}：权限申请无横向溢出`, async () => {
    await send('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: true });
    await nav('/applies?new=1&instance=4&db=shop');
    await waitFor("!document.getElementById('apply-panel').hidden && document.querySelector('#ap-db-checks label')", '申请面板');
    const w = await overflow();
    truthy(w[0] <= w[1] + 1, `申请页横向溢出 ${w}`);
    await send('Emulation.setDeviceMetricsOverride', DESKTOP);
  });
}

// =============== 权限申请 ===============
await nav('/applies?new=1&instance=4&db=shop');
await waitFor("!document.getElementById('apply-panel').hidden && document.querySelector('#ap-table-checks label')", '申请面板');
await t('申请：PG 库表级都可选，表名 schema.table', async () => {
  eq(await q('input[name="ap-type"][value="2"]', 'disabled'), false);
  eq(await qa('#ap-table-checks label'), ['public.orders', 'public.v_orders', 'public.mv_stats', 'audit.events', 'audit.ext_log']);
  await shot('apply_pg');
});
await t('申请：勾选 schema.table 提交表级申请', async () => {
  await ev("const r=document.querySelector('input[name=ap-type][value=\"2\"]'); r.checked=true; r.dispatchEvent(new Event('change'));");
  await ev("[...document.querySelectorAll('#ap-table-checks label')].find(l=>l.textContent==='audit.events').querySelector('input').click();");
  await ev("document.getElementById('ap-reason').value='需要看审计数据，申请表级权限';");
  await click('#ap-submit');
  await sleep(300);
  const c = calls.filter((x) => x.url === '/api/v1/query/privilege-applies').slice(-1)[0];
  eq(c.body.priv_type, 2);
  eq(c.body.db_name, 'shop');
  eq(c.body.table_list, ['audit.events']);
  eq(c.body.instance_id, 4);
});
await nav('/applies');
await ev("document.getElementById('new-apply').click();");
await waitFor("document.querySelector('#ap-inst option')", '实例下拉');
await t('申请：Mongo 标签变“集合”，Redis 仍只能库级，MySQL 仍是表', async () => {
  await ev("const s=document.getElementById('ap-inst'); s.value='5'; s.dispatchEvent(new Event('change'));");
  await waitFor("document.querySelector('#ap-table-checks label')", 'mongo 集合');
  has(await q('#ap-tables-wrap .lbl'), '集合（可多选）');
  eq(await q('#ap-table-q', 'placeholder'), '过滤集合名');
  await ev("const s=document.getElementById('ap-inst'); s.value='3'; s.dispatchEvent(new Event('change'));");
  await sleep(200);
  eq(await q('input[name="ap-type"][value="2"]', 'disabled'), true);
  await ev("const s=document.getElementById('ap-inst'); s.value='1'; s.dispatchEvent(new Event('change'));");
  await sleep(200);
  eq(await q('input[name="ap-type"][value="2"]', 'disabled'), false);
  has(await q('#ap-tables-wrap .lbl'), '表（可多选）');
  eq(await q('#ap-table-q', 'placeholder'), '过滤表名');
});

// =============== 实例列表 ===============
await nav('/instances');
await waitFor("document.querySelectorAll('#il-rows tr').length >= 6", '实例行');
await t('实例列表：新类型标“仅查询”，不出现开启采集按钮；MySQL 不变', async () => {
  const rows = await ev("return [...document.querySelectorAll('#il-rows tr')].map(tr=>({n:tr.children[2].textContent.trim(), cap:tr.children[8].textContent.trim(), ops:[...tr.querySelectorAll('.ops .btn')].map(b=>b.textContent)}));");
  const by = (n) => rows.find((r) => r.n.startsWith(n));
  eq(by('pg-main').cap, '暂不支持');
  has(by('pg-main').n, '仅查询');
  eq(by('mysql-prod').n.includes('仅查询'), false);
  eq(by('pg-main').ops.includes('关闭采集') || by('pg-main').ops.includes('开启采集'), false);
  eq(by('mongo-main').cap, '暂不支持');
  eq(by('mysql-prod').cap, '未采集');
  eq(by('mysql-prod').ops.includes('开启采集') || by('mysql-prod').ops.includes('关闭采集'), false); // 列表里不再有开启 / 关闭采集按钮（在“编辑”里改）
  await shot('inst_list');
});
// 慢SQL 列的“采集中 / 未采集 / 暂不支持”和操作列的按钮都不换行；放不下时表格自己横向滚动，页面不出现横向滚动条。
const noWrapProbe = `
  const lines = (el) => { const r = document.createRange(); r.selectNodeContents(el); return new Set([...r.getClientRects()].map((x) => Math.round(x.top))).size; };
  return [...document.querySelectorAll('#il-rows tr')].map((tr) => ({
    n: tr.children[2].textContent.trim(),
    capLines: lines(tr.children[8]),
    opTops: [...new Set([...tr.querySelectorAll('.ops .btn')].map((b) => Math.round(b.getBoundingClientRect().top)))].length,
    btns: tr.querySelectorAll('.ops .btn').length,
  }));`;
await t('实例列表：慢SQL 状态与操作按钮各占一行，不换行（1366 宽）', async () => {
  const rows = await ev(noWrapProbe);
  truthy(rows.length >= 6, '至少 6 行');
  for (const r of rows) {
    eq(r.capLines, 1, `${r.n} 的慢SQL 列行数`);
    eq(r.opTops, 1, `${r.n} 的操作按钮占了几行（共 ${r.btns} 个按钮）`);
  }
  const w = await ev('return [document.documentElement.scrollWidth, window.innerWidth];');
  truthy(w[0] <= w[1] + 1, `页面不应出现横向滚动 ${w[0]} > ${w[1]}`);
});
await t('实例列表：窗口窄到放不下时，表格在自己的容器里横向滚动，按钮仍不换行，页面没有横向滚动条', async () => {
  await send('Emulation.setDeviceMetricsOverride', { width: 900, height: 900, deviceScaleFactor: 1, mobile: false });
  await sleep(300);
  const rows = await ev(noWrapProbe);
  for (const r of rows) {
    eq(r.capLines, 1, `${r.n} 的慢SQL 列行数`);
    eq(r.opTops, 1, `${r.n} 的操作按钮行数`);
  }
  const m = await ev("const s=document.querySelector('.il-scroll'); return [document.documentElement.scrollWidth, window.innerWidth, s.scrollWidth, s.clientWidth];");
  truthy(m[0] <= m[1] + 1, `页面出现横向滚动 ${m[0]} > ${m[1]}`);
  truthy(m[2] >= m[3], '容器内部可以横向滚动');
  await send('Emulation.setDeviceMetricsOverride', DESKTOP);
  await sleep(200);
});
await t('实例列表：详情抽屉写“支持范围”，隐藏会话/账号/参数入口；MySQL 保留', async () => {
  await ev("[...document.querySelectorAll('#il-rows tr')].find(tr=>tr.children[2].textContent.startsWith('ch-main')).children[2].querySelector('a').click();");
  await waitFor("document.querySelector('#il-drawer-body dl')", '抽屉');
  has(await q('#il-drawer-body'), '支持范围');
  has(await q('#il-drawer-body'), '在线查询');
  eq(await exists('#il-drawer-body .im-drawer-links a'), false);
  await ev("document.querySelector('#il-drawer .actions .btn, #il-drawer button')?.click();");
  await ev("[...document.querySelectorAll('#il-rows tr')].find(tr=>tr.children[2].textContent.startsWith('mysql-prod')).children[2].querySelector('a').click();");
  await waitFor("document.querySelector('#il-drawer-body dl')", '抽屉');
  eq((await qa('#il-drawer-body .im-drawer-links a')).length > 0, true);
  eq((await q('#il-drawer-body')).includes('支持范围'), false);
});
await t('实例列表：行首勾选与批量启用 / 停用 / 开启关闭采集 / 删除', async () => {
  const names = () => ev("return [...document.querySelectorAll('#il-rows tr')].map(tr=>tr.children[2].textContent.trim());");
  const rows = await names();
  eq(rows.length >= 6, true);
  eq(await exists('#il-rows tr td.im-sel input'), true, '每行有勾选框');
  eq(await q('#il-batch', 'hidden'), false, '有权限时显示批量操作条');
  eq(await ev("return ['il-b-enable','il-b-disable','il-b-delete','il-b-clear'].every(id=>document.getElementById(id).disabled);"), true, '没勾选时按钮不可用');
  // 勾选前两行：计数 2、按钮可用、行高亮
  await ev("document.querySelectorAll('#il-rows tr td.im-sel input')[0].click(); document.querySelectorAll('#il-rows tr td.im-sel input')[1].click();");
  eq(await q('#il-sel-n'), '2');
  eq(await ev("return ['il-b-enable','il-b-disable','il-b-delete','il-b-clear'].some(id=>document.getElementById(id).disabled);"), false);
  eq(await ev("return document.querySelectorAll('#il-rows tr.im-picked').length;"), 2);
  // 全选当前页，再取消
  await ev("document.getElementById('il-all').click();");
  eq(Number(await q('#il-sel-n')), rows.length);
  await ev("document.getElementById('il-all').click();");
  eq(await q('#il-sel-n'), '0');

  // 批量停用：确认弹窗 → 请求带上勾选的 id
  await ev("document.querySelectorAll('#il-rows tr td.im-sel input')[0].click(); document.querySelectorAll('#il-rows tr td.im-sel input')[1].click();");
  await ev("document.getElementById('il-b-disable').click();");
  await waitFor("document.querySelector('body > .modal-mask:not([id]) .modal h2')", '确认弹窗');
  has(await q('body > .modal-mask:not([id]) .modal h2'), '批量停用 2 个实例');
  has(await q('body > .modal-mask:not([id]) .modal'), rows[0]);
  await ev("[...document.querySelectorAll('body > .modal-mask:not([id]) .modal .actions button')].find(b=>b.textContent==='确定停用').click();");
  await waitFor("calls.length >= 0 && document.querySelector('#il-msg .alert')", '结果提示').catch(() => {});
  const dis = calls.filter((x) => x.url === '/api/v1/instances/batch/disable').slice(-1)[0];
  eq(dis.body.disabled, true);
  eq(dis.body.ids.length, 2);
  has(await q('#il-msg'), '已停用 2 个实例');

  // 批量开启 / 关闭采集：只提交支持慢日志采集且状态不同的；只支持在线查询的（pg-main）被跳过并说明
  await waitFor("document.querySelectorAll('#il-rows tr td.im-sel input').length >= 6", '列表刷新');
  eq(await q('#il-b-collect-on'), '批量开启采集');
  eq(await q('#il-b-collect-off'), '批量关闭采集');
  eq(await ev("return ['il-b-collect-on','il-b-collect-off'].every(id=>document.getElementById(id).disabled);"), true, '没勾选时不可用');
  await ev("document.querySelectorAll('#il-rows tr td.im-sel input').forEach((c, i) => { if (i === 0 || i === 3) c.click(); });"); // mysql-prod + pg-main
  eq(await q('#il-sel-n'), '2');
  await ev("document.getElementById('il-b-collect-on').click();");
  await waitFor("document.querySelector('body > .modal-mask:not([id]) .modal h2')", '确认弹窗');
  has(await q('body > .modal-mask:not([id]) .modal h2'), '批量开启采集 1 个实例');
  has(await q('body > .modal-mask:not([id]) .modal'), 'mysql-prod');
  has(await q('body > .modal-mask:not([id]) .modal'), '只支持在线查询');
  await ev("[...document.querySelectorAll('body > .modal-mask:not([id]) .modal .actions button')].find(b=>b.textContent==='确定开启采集').click();");
  await sleep(300);
  const col = calls.filter((x) => x.url === '/api/v1/instances/batch/collect').slice(-1)[0];
  eq(col.body.enable, true);
  eq(col.body.ids, [1], '只提交 mysql-prod');
  has(await q('#il-msg'), '已开启采集 1 个实例');
  // 只选了只支持在线查询的实例：不弹确认、不发请求，直接说明
  await waitFor("document.querySelectorAll('#il-rows tr td.im-sel input').length >= 6", '列表刷新');
  const nCol = calls.filter((x) => x.url === '/api/v1/instances/batch/collect').length;
  await ev("document.querySelectorAll('#il-rows tr td.im-sel input')[3].click();");
  await ev("document.getElementById('il-b-collect-off').click();");
  await sleep(200);
  has(await q('#il-msg'), '没有慢日志采集');
  eq(calls.filter((x) => x.url === '/api/v1/instances/batch/collect').length, nCol, '没有可操作的实例不应发请求');
  // 已经是关闭状态的 mysql-prod：同样直接说明
  await ev("document.querySelectorAll('#il-rows tr td.im-sel input')[3].click(); document.querySelectorAll('#il-rows tr td.im-sel input')[0].click();");
  await ev("document.getElementById('il-b-collect-off').click();");
  await sleep(200);
  has(await q('#il-msg'), '都已经是关闭状态');
  eq(calls.filter((x) => x.url === '/api/v1/instances/batch/collect').length, nCol, '已经是目标状态的不应发请求');
  await ev("document.getElementById('il-b-clear').click();");

  // 批量删除：先 dry_run，被引用的跳过；要键入“删除N个实例”才能确认
  await waitFor("document.querySelectorAll('#il-rows tr td.im-sel input').length >= 6", '列表刷新');
  await ev("document.querySelectorAll('#il-rows tr td.im-sel input').forEach((c, i) => { if (i === 0 || i === 5) c.click(); });");
  eq(await q('#il-sel-n'), '2');
  await ev("document.getElementById('il-b-delete').click();");
  await waitFor("document.querySelector('body > .modal-mask:not([id]) .modal input[type=text]')", '键入确认');
  const dry = calls.filter((x) => x.url === '/api/v1/instances/batch/delete').slice(-1)[0];
  eq(dry.body.dry_run, true);
  const dlg = await q('body > .modal-mask:not([id]) .modal');
  has(dlg, '将删除 1 个');
  has(dlg, '仍被引用：SQL 工单 3 条');
  has(dlg, '删除1个实例');
  eq(await ev("return [...document.querySelectorAll('body > .modal-mask:not([id]) .modal .actions button')].find(b=>b.textContent==='确定删除').disabled;"), true, '没键入前不能确认');
  await ev("const i=document.querySelector('body > .modal-mask:not([id]) .modal input[type=text]'); i.value='删除1个实例'; i.dispatchEvent(new Event('input'));");
  await ev("[...document.querySelectorAll('body > .modal-mask:not([id]) .modal .actions button')].find(b=>b.textContent==='确定删除').click();");
  await sleep(300);
  const del = calls.filter((x) => x.url === '/api/v1/instances/batch/delete').slice(-1)[0];
  eq(del.body.confirm, true);
  eq(del.body.dry_run === true, false);
  eq(del.body.ids.length, 1, '只提交能删的');
});
await t('实例表单：切换类型端口默认值 5432 / 27017 / 8123，回 MySQL 3306', async () => {
  await ev("document.getElementById('il-add').hidden=false; document.getElementById('il-add').click();");
  await waitFor("!document.getElementById('il-edit').hidden", '编辑框').catch(() => {});
  const set = async (v) => { await ev(`const s=document.getElementById('f-dbtype'); s.value=${JSON.stringify(v)}; s.dispatchEvent(new Event('change'));`); return q('#f-port', 'value'); };
  eq(await set('pgsql'), '5432');
  eq(await set('mongo'), '27017');
  eq(await set('clickhouse'), '8123');
  eq(await set('mysql'), '3306');
  eq(await set('redis'), '6379');
  await ev("const p=document.getElementById('f-port'); p.value='9999'; const s=document.getElementById('f-dbtype'); s.value='pgsql'; s.dispatchEvent(new Event('change'));");
  eq(await q('#f-port', 'value'), '9999', '自定义端口不被覆盖');
});

// ---- 提示条与空结果：直接用页面里加载的模块验证 ----
await t('提示条：管理员 / 资源组只有固定说明时整条隐藏，说明合进返回的悬停文字；有要留意的内容时才显示', async () => {
  const r = await ev(`const m = await import('/static/js/query/policybar.js'); const out = {};
    const run = (policy, global, dbType = 'mysql') => { const c = document.createElement('div'); const info = m.renderPolicy(c, { meta: { policy }, db: 'shop', dbType, global, applyHref: () => '' }); return { hidden: c.hidden, chips: [...c.querySelectorAll('.chip')].map((x) => x.textContent), info }; };
    out.admin = run({ mode: 'admin', max_rows: 5000 }, { timeout: 60, disableStar: false, masking: false });
    out.group = run({ mode: 'group', max_rows: 2000 }, { timeout: 30, disableStar: false, masking: false });
    out.soon = run({ mode: 'priv', max_rows: 100, all_tables: true, expire: '2026-11-01', expire_days: 3 }, { timeout: 60, disableStar: false, masking: false });
    out.mask = run({ mode: 'admin', max_rows: 5000 }, { timeout: 60, disableStar: true, masking: true });
    return out;`);
  eq([r.admin.hidden, r.admin.chips], [true, []]);
  has(r.admin.info, '管理员'); has(r.admin.info, '最多 5,000 行'); has(r.admin.info, '超时 60 秒'); has(r.admin.info, '只读：SELECT / WITH / SHOW / DESC / EXPLAIN');
  eq([r.group.hidden, r.group.chips], [true, []]);
  has(r.group.info, '资源组内实例'); has(r.group.info, '超时 30 秒');
  eq([r.mask.hidden, r.mask.chips], [true, []]);
  has(r.mask.info, '数据脱敏已启用'); has(r.mask.info, '禁止 SELECT *');
  eq([r.soon.hidden, r.soon.chips], [false, ['权限到期 2026-11-01（3 天后）']]);
  has(r.soon.info, '查询权限：shop 全部表 · 最多 100 行');
});
await t('空结果：字段名表头在上，“没有符合条件的数据”在表头下面', async () => {
  const r = await ev(`const g = await import('/static/js/query/grid.js'); const box = document.createElement('div'); document.body.append(box);
    g.renderResult(box, { columns: ['id', 'kind', 'msg'], rows: [], row_count: 0, cost_ms: 3, kind: 'select' }, { title: '结果' });
    const wrap = box.querySelector('.grid-wrap'); const th = [...box.querySelectorAll('thead th')].map((x) => x.textContent.replace(/\\s+/g, ''));
    const empty = box.querySelector('.res-empty'); const head = box.querySelector('thead').getBoundingClientRect(); const msg = empty.getBoundingClientRect();
    const out = { th, emptyInWrap: wrap.contains(empty), below: msg.top >= head.bottom - 1, before: !!(empty.compareDocumentPosition(box.querySelector('thead')) & Node.DOCUMENT_POSITION_PRECEDING) };
    box.remove(); return out;`);
  eq(r.th, ['#', 'id', 'kind', 'msg']);
  truthy(r.emptyInWrap && r.below && r.before, '提示应在表头之后');
});

// ---- 全局：控制台零报错 ----
await t('控制台零报错', async () => {
  // 拒绝类用例会得到 4xx 的接口响应，申请提交后跳转的详情页在假后端里不存在，这些是预期的网络日志
  // （428 是 SQL 防火墙“要求填写原因”的预期响应）
  const errs = consoleErrors.filter((e) => !/favicon/i.test(e) && !/Failed to load resource: .*(40[034]|42[89])/.test(e));
  eq(errs, []);
});

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
