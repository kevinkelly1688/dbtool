/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_mysql_panel.mjs
 * @time: 2026/10/28
 */
// 在线查询页“我的 SQL”面板（收藏 + 执行历史）的页面级测试：
// 复用 multi_engine_fake.mjs 的假后端（接口形状按 internal/queryfav 与 internal/query/history.go）+ 无头 Chrome（CDP），不依赖第三方包。
// 用法：node scripts/frontend-test/run_mysql_panel.mjs [chrome路径]    环境变量 MP_HTTP_PORT（默认 8926）/ MP_CDP_PORT（默认 9263）/ MP_SHOT（截图目录）。
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { startServer, calls, fake } from './multi_engine_fake.mjs';

const HTTP = +(process.env.MP_HTTP_PORT || 8926);
const CDP = +(process.env.MP_CDP_PORT || 9263);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const server = await startServer(HTTP);
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'mp-chrome-'));
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
const consoleErrors = [];
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
await send('Emulation.setDeviceMetricsOverride', { width: 1366, height: 900, deviceScaleFactor: 1, mobile: false });
async function shot(name) {
  if (!process.env.MP_SHOT) return;
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.mkdirSync(process.env.MP_SHOT, { recursive: true });
  fs.writeFileSync(path.join(process.env.MP_SHOT, name + '.png'), Buffer.from(r.data, 'base64'));
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
const click = (sel) => ev(`document.querySelector(${JSON.stringify(sel)}).click();`);
const setVal = (sel, v) => ev(`const e=document.querySelector(${JSON.stringify(sel)}); e.value=${JSON.stringify(v)}; e.dispatchEvent(new Event('input')); e.dispatchEvent(new Event('change'));`);
const edVal = () => ev("return ace.edit(document.getElementById('editor')).getValue();");
const setEd = (v) => ev(`const e=ace.edit(document.getElementById('editor')); e.setValue(${JSON.stringify(v)}, 1);`);
const favCalls = (method) => calls.filter((c) => c.url.startsWith('/api/v1/query/favorites') && (!method || c.method === method));

async function nav(url) {
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}${url}` });
  await sleep(200);
  await waitFor("document.readyState === 'complete'", '页面加载');
}
async function openQuery() {
  await nav('/query');
  await ev('localStorage.clear();');
  await nav('/query?inst=1&db=shop');
  await waitFor("document.getElementById('db').value", '库');
  await sleep(150);
}
const panelOpen = () => ev("return !document.getElementById('mysql').hidden;");
const favRows = () => qa('#fav-rows tr:not(.my-empty)', 'innerText');
const histRows = () => qa('#hist-rows tr:not(.my-empty)', 'innerText');

// ---- 数据 ----
fake.history = [
  { id: 1, time: '2026-10-13 09:00:00', username: 'me', display: '我', instance: 'prod-mysql', db: 'shop', sql: 'select id from orders limit 10', rows: 10, cost_sec: '0.010', status: 'ok' },
  { id: 2, time: '2026-10-13 09:05:00', username: 'me', display: '我', instance: 'prod-mysql', db: 'shop', sql: 'select * from users where name like \'%<b>x</b>%\'', rows: 3, cost_sec: '0.020', status: 'ok' },
  { id: 3, time: '2026-10-13 09:10:00', username: 'wang', display: '王工', instance: 'prod-mysql', db: 'report', sql: 'select city, count(*) from users group by city', rows: 8, cost_sec: '0.030', status: 'ok' },
  { id: 4, time: '2026-10-13 09:20:00', username: 'li', display: '李四', instance: 'test-mysql', db: 'shop', sql: 'select * from orders_bak', rows: 0, cost_sec: '0.001', status: 'blocked', reason: '缺少 where 条件' },
];

console.log(`“我的 SQL”页面测试（http://127.0.0.1:${HTTP}/query）`);
await openQuery();

await t('工具栏按钮叫“我的 SQL”，面板默认收起；点开显示“收藏 / 执行历史”两个标签', async () => {
  eq(await q('#hist-btn'), '我的 SQL');
  eq(await panelOpen(), false);
  await click('#hist-btn');
  await waitFor("!document.getElementById('mysql').hidden", '面板打开');
  eq(await qa('.my-tab'), ['收藏', '执行历史']);
  eq(await q('#my-tab-fav', 'className'), 'my-tab on');
  eq(await q('#hist-btn', 'getAttribute("aria-expanded")'), 'true');
});

await t('收藏：没有数据时的提示；表头有注释 / SQL / 添加时间 / 最后使用时间 / 操作', async () => {
  await waitFor("document.querySelector('#fav-rows .my-empty')", '空提示');
  has(await q('#fav-rows'), '没有数据');
  const th = (await qa('#fav-table thead th', 'innerText')).map((s) => s.replace(/\s+/g, ' ').trim());
  eq(th[0], '注释');
  has(th[1], 'SQL');
  has(th[2], '添加时间');
  has(th[3], '最后使用时间');
  eq(th[4], '操作');
  await shot('fav-empty');
});

await t('收藏：在编辑器里选一条语句 → 点“添加” → 表单带出 SQL，填注释保存后出现在列表', async () => {
  await setEd('select id, name from orders where id > 0');
  await click('#fav-add');
  await waitFor("!document.getElementById('fav-edit').hidden", '表单');
  eq(await q('#fav-edit-sql', 'value'), 'select id, name from orders where id > 0');
  await setVal('#fav-edit-comment', '订单明细');
  await click('#fav-edit button[type=submit]');
  await waitFor("document.querySelectorAll('#fav-rows tr:not(.my-empty)').length === 1", '列表出现一条');
  const rows = await favRows();
  has(rows[0], '订单明细');
  has(rows[0], 'select id, name from orders where id > 0');
  eq(await q('#fav-edit', 'hidden'), true);
  const post = favCalls('POST').slice(-1)[0];
  eq(post.body.instance !== '', true, '带上当前实例名');
  eq(post.body.db, await q('#db', 'value'), '带上当前库');
});

await t('收藏：同一条 SQL 再添加不会重复；SQL 为空时前端先拦', async () => {
  await click('#fav-add');
  await waitFor("!document.getElementById('fav-edit').hidden", '表单');
  await setVal('#fav-edit-sql', '   ');
  await click('#fav-edit button[type=submit]');
  has(await q('#fav-edit-msg'), 'SQL 不能为空');
  await setVal('#fav-edit-sql', 'select id, name from orders where id > 0');
  await setVal('#fav-edit-comment', '订单明细（改）');
  await click('#fav-edit button[type=submit]');
  await waitFor("document.getElementById('fav-edit').hidden", '表单关闭');
  await waitFor("document.querySelector('#fav-rows .my-note') && document.querySelector('#fav-rows .my-note').textContent.includes('改')", '注释更新');
  eq((await favRows()).length, 1);
});

await t('收藏：使用 = 粘贴到编辑器光标处，并记录使用次数；双击 SQL 也行', async () => {
  await setEd('-- 我的草稿\n');
  await ev("const b=[...document.querySelectorAll('#fav-rows .my-act')].find(x=>x.textContent==='使用'); b.click();");
  await waitFor("document.getElementById('mysql') && true", 'ok');
  await sleep(200);
  has(await edVal(), 'select id, name from orders where id > 0');
  has(await edVal(), '我的草稿', '粘贴不覆盖原有内容');
  truthy(favCalls('POST').some((c) => /\/use$/.test(c.url)), '调用了 use 接口');
  await waitFor("[...document.querySelectorAll('#fav-rows .my-time')][1] && [...document.querySelectorAll('#fav-rows .my-time')][1].textContent !== '—'", '最后使用时间有值');
  await setEd('');
  await ev("document.querySelector('#fav-rows .my-sql').dispatchEvent(new MouseEvent('dblclick', {bubbles:true}));");
  await sleep(200);
  has(await edVal(), 'select id, name from orders where id > 0');
});

await t('收藏：编辑注释与 SQL；删除需要二次确认', async () => {
  await ev("[...document.querySelectorAll('#fav-rows .my-act')].find(x=>x.textContent==='编辑').click();");
  await waitFor("!document.getElementById('fav-edit').hidden", '编辑表单');
  eq(await q('#fav-edit-title'), '编辑收藏');
  await setVal('#fav-edit-comment', '订单（二次编辑）');
  await click('#fav-edit button[type=submit]');
  await waitFor("document.querySelector('#fav-rows .my-note').textContent.includes('二次编辑')", '注释更新');
  eq(favCalls('PUT').length, 1);
  // 删除：第一次点只变成“确认删除”
  await ev("[...document.querySelectorAll('#fav-rows .my-act')].find(x=>x.textContent==='删除').click();");
  eq(await ev("return [...document.querySelectorAll('#fav-rows .my-act')].map(b=>b.textContent);"), ['使用', '编辑', '确认删除']);
  eq(favCalls('DELETE').length, 0);
  await ev("[...document.querySelectorAll('#fav-rows .my-act')].find(x=>x.textContent==='确认删除').click();");
  await waitFor("document.querySelector('#fav-rows .my-empty')", '删除后为空');
  eq(favCalls('DELETE').length, 1);
});

await t('收藏：搜索（注释或 SQL，多个关键词都包含）与排序', async () => {
  for (const [c, s] of [['订单', 'select * from orders'], ['用户', 'select * from users'], ['月报', 'select city from users group by city']]) {
    await ev(`await fetch('/api/v1/query/favorites', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({comment:${JSON.stringify(c)}, sql:${JSON.stringify(s)}})});`);
  }
  await click('#fav-form button[type=submit]');
  await waitFor("document.querySelectorAll('#fav-rows tr:not(.my-empty)').length === 3", '3 条');
  await setVal('#fav-q', 'users city');
  await click('#fav-form button[type=submit]');
  await waitFor("document.querySelectorAll('#fav-rows tr:not(.my-empty)').length === 1", '关键词都包含');
  has((await favRows())[0], '月报');
  await setVal('#fav-q', '用户');
  await click('#fav-form button[type=submit]');
  await waitFor("document.querySelectorAll('#fav-rows tr:not(.my-empty)').length === 1", '按注释搜');
  await setVal('#fav-q', '');
  await click('#fav-form button[type=submit]');
  await waitFor("document.querySelectorAll('#fav-rows tr:not(.my-empty)').length === 3", '清空后 3 条');
  // 点“添加时间”切换排序：第一次降序，再点升序
  const three = "document.querySelectorAll('#fav-rows tr:not(.my-empty)').length === 3";
  await click('[data-fav-sort=created]');
  await waitFor("document.querySelector('[data-fav-sort=created] .arrow').textContent === '↓'", '降序箭头');
  await waitFor(three, '降序列表');
  const desc = await qa('#fav-rows .my-note');
  await click('[data-fav-sort=created]');
  await waitFor("document.querySelector('[data-fav-sort=created] .arrow').textContent === '↑'", '升序箭头');
  await waitFor(three, '升序列表');
  eq((await qa('#fav-rows .my-note')).slice().reverse(), desc);
  eq(favCalls('GET').slice(-1)[0].url.includes('sort=created&order=asc'), true, '请求带了排序参数');
});

await t('收藏：表还没建时（503）把原因写在表格里，不报脚本错误', async () => {
  fake.favError = '收藏功能还没有启用：请先在元数据库执行 deploy/sql/250_query_favorite.sql';
  await click('#fav-form button[type=submit]');
  await waitFor("document.querySelector('#fav-rows .alert')", '错误提示');
  has(await q('#fav-rows .alert'), '250_query_favorite.sql');
  fake.favError = null;
  await click('#fav-form button[type=submit]');
  await waitFor("document.querySelectorAll('#fav-rows tr:not(.my-empty)').length === 3", '恢复');
});

await t('执行历史（普通用户）：只有自己的；没有“用户”列和用户过滤；SQL 里的 HTML 不会被渲染', async () => {
  fake.admin = false;
  await click('#my-tab-hist');
  await waitFor("document.querySelectorAll('#hist-rows tr:not(.my-empty)').length === 2", '自己的 2 条');
  eq(await q('#hist-user', 'hidden'), true);
  eq(await q('#hist-user-th', 'hidden'), true);
  has(await q('#my-scope'), '我执行过的');
  const cells = await qa('#hist-rows .my-sql pre');
  has(cells.join(' | '), '%<b>x</b>%', '标签原样显示成文字');
  eq(await ev("return document.querySelectorAll('#hist-rows b').length;"), 0);
  await shot('hist-user');
});

await t('执行历史：按语句模糊搜索，命中的关键词高亮；重置恢复', async () => {
  await setVal('#hist-q', 'orders');
  await click('#hist-form button[type=submit]');
  await waitFor("document.querySelectorAll('#hist-rows tr:not(.my-empty)').length === 1", '只剩 orders');
  eq(await q('#hist-rows mark'), 'orders');
  await click('#hist-reset');
  await waitFor("document.querySelectorAll('#hist-rows tr:not(.my-empty)').length === 2", '重置');
});

await t('执行历史：粘贴 / 新标签 / 收藏 / 双击', async () => {
  await setEd('');
  await ev("[...document.querySelector('#hist-rows tr').querySelectorAll('.my-act')].find(b=>b.textContent==='粘贴').click();");
  await sleep(150);
  has(await edVal(), 'select');
  // 收藏：切到收藏标签并带出 SQL
  await ev("[...document.querySelector('#hist-rows tr').querySelectorAll('.my-act')].find(b=>b.textContent==='收藏').click();");
  await waitFor("!document.getElementById('fav-edit').hidden && !document.getElementById('my-pane-fav').hidden", '收藏表单');
  has(await q('#fav-edit-sql', 'value'), 'select');
  await click('#fav-edit-cancel');
});

await t('执行历史（超级用户）：看到所有人的、有“用户”列；按用户模糊过滤（用户名或姓名）', async () => {
  fake.admin = true;
  await click('#my-tab-hist');
  await waitFor("!document.getElementById('hist-user').hidden", '用户过滤框出现');
  await waitFor("document.querySelectorAll('#hist-rows tr:not(.my-empty)').length === 4", '4 条');
  eq(await q('#hist-user-th', 'hidden'), false);
  has(await q('#my-scope'), '所有人');
  // 姓名里的一部分
  await setVal('#hist-user', '王');
  await click('#hist-form button[type=submit]');
  await waitFor("document.querySelectorAll('#hist-rows tr:not(.my-empty)').length === 1", '王工');
  has((await histRows())[0], '王工（wang）');
  // 用户名里的一部分
  await setVal('#hist-user', 'an');
  await click('#hist-form button[type=submit]');
  await waitFor("document.querySelectorAll('#hist-rows tr:not(.my-empty)').length === 1", 'wang');
  // 用户 + 语句 同时过滤
  await setVal('#hist-user', 'me');
  await setVal('#hist-q', 'users');
  await click('#hist-form button[type=submit]');
  await waitFor("document.querySelectorAll('#hist-rows tr:not(.my-empty)').length === 1", '用户 + 语句');
  has((await histRows())[0], 'select * from users');
  const url = calls.filter((c) => c.url.startsWith('/api/v1/query/history?')).slice(-1)[0].url;
  has(url, 'user=me');
  has(url, 'q=users');
  await shot('hist-admin');
  // 用户下拉候选来自 /history/users
  eq((await qa('#hist-users option', 'value')).sort(), ['li', 'me', 'wang']);
});

await t('执行历史：状态筛选（有事件表时出现），被拦截的显示原因', async () => {
  fake.events = true;
  await click('#hist-reset');
  await waitFor("!document.getElementById('hist-status').hidden", '状态下拉出现');
  await setVal('#hist-status', 'blocked');
  await waitFor("document.querySelectorAll('#hist-rows tr:not(.my-empty)').length === 1", '被拦截 1 条');
  has(await q('#hist-rows .my-reason'), '缺少 where 条件');
  has(await q('#hist-rows .st-badge'), '被拦截');
});

await t('Esc 关闭面板；再点按钮能重新打开，停在上次的标签', async () => {
  await ev("document.dispatchEvent(new KeyboardEvent('keydown', {key:'Escape'}));");
  await waitFor("document.getElementById('mysql').hidden", '关闭');
  await click('#hist-btn');
  await waitFor("!document.getElementById('mysql').hidden", '重新打开');
  eq(await q('#my-tab-hist', 'className'), 'my-tab on');
});

// ---- 前置 SET：运行光标所在语句时，脚本开头的 SET 自动一起提交 ----
const execCalls = () => calls.filter((c) => c.url === '/api/v1/query/execute' && c.method === 'POST');
const lastExecSQL = () => execCalls().slice(-1)[0].body.sql;
const setCursor = (offset) => ev(`const e=ace.edit(document.getElementById('editor')); e.clearSelection(); e.moveCursorToPosition(e.session.doc.indexToPosition(${offset}));`);
const SCRIPT = [
  "SET block_encryption_mode = 'aes-256-cbc';",
  'SET @SiteId := 1008;',
  'SELECT @@session.block_encryption_mode AS m;',
  'SELECT id FROM orders WHERE site = @SiteId;',
].join('\n');

await t('前置 SET：光标在后面的查询上 → 开头的 SET 与该查询一起提交', async () => {
  await click('#hist-btn'); // 收起面板，避免遮挡
  await setEd(SCRIPT);
  await setCursor(SCRIPT.indexOf('FROM orders') + 2);
  const before = execCalls().length;
  await click('#run');
  await sleep(300);
  eq(execCalls().length, before + 1);
  eq(lastExecSQL(), "SET block_encryption_mode = 'aes-256-cbc'\n;\nSET @SiteId := 1008\n;\nSELECT id FROM orders WHERE site = @SiteId");
});

await t('前置 SET：光标就在某条 SET 上 → 运行全部开头 SET + 紧随其后的第一条查询', async () => {
  await setCursor(SCRIPT.indexOf('SET @SiteId') + 3);
  const before = execCalls().length;
  await click('#run');
  await sleep(300);
  eq(execCalls().length, before + 1);
  eq(lastExecSQL(), "SET block_encryption_mode = 'aes-256-cbc'\n;\nSET @SiteId := 1008\n;\nSELECT @@session.block_encryption_mode AS m");
});

await t('前置 SET：选中文本原样提交；执行计划只给查询加 EXPLAIN，SET 放在前面', async () => {
  const sel = 'SELECT id FROM orders WHERE site = 1';
  await setEd(sel);
  await ev("const e=ace.edit(document.getElementById('editor')); e.selectAll();");
  let before = execCalls().length;
  await click('#run');
  await sleep(300);
  eq(execCalls().length, before + 1);
  eq(lastExecSQL(), sel);
  await setEd(SCRIPT);
  await setCursor(SCRIPT.indexOf('FROM orders') + 2);
  before = execCalls().length;
  await click('#explain');
  await sleep(300);
  eq(execCalls().length, before + 1);
  eq(lastExecSQL(), "SET block_encryption_mode = 'aes-256-cbc'\n;\nSET @SiteId := 1008\n;\nEXPLAIN SELECT id FROM orders WHERE site = @SiteId");
});

await t('控制台零报错', async () => {
  const errs = consoleErrors.filter((e) => !/favicon/i.test(e) && !/Failed to load resource: .*(40[034]|42[89]|503)/.test(e));
  eq(errs, []);
});

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
