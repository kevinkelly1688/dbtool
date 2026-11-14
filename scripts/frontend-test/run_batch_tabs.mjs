/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_batch_tabs.mjs
 * @time: 2026/10/28
 */
// 在线查询“运行全部”（一条语句一个结果页签）的页面级测试：
// 真实的 /query 模板 + 真实静态文件 + 假后端（multi_engine_fake.mjs）+ 无头 Chrome（CDP），不依赖第三方包。
// 用法：node scripts/frontend-test/run_batch_tabs.mjs [chrome路径]    环境变量 BT_HTTP_PORT（默认 8947）/ BT_CDP_PORT（默认 9281）。
// BT_SHOT=目录 时在关键位置截图。结束时只按 PID 结束自己启动的 Chrome。
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { startServer, calls } from './multi_engine_fake.mjs';

const HTTP = +(process.env.BT_HTTP_PORT || 8947);
const CDP = +(process.env.BT_CDP_PORT || 9281);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const server = await startServer(HTTP);
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'bt-chrome-'));
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
  if (!process.env.BT_SHOT) return;
  await sleep(250);
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.mkdirSync(process.env.BT_SHOT, { recursive: true });
  fs.writeFileSync(path.join(process.env.BT_SHOT, name + '.png'), Buffer.from(r.data, 'base64'));
}
async function waitFor(expr, what, ms = 6000) {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) {
    if (await ev(`return !!(${expr});`).catch(() => false)) return;
    await sleep(40);
  }
  throw new Error('等待超时：' + what);
}
let pass = 0;
let fail = 0;
async function t(name, fn) {
  try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; console.log('  FAIL ' + name + '\n       ' + e.message); }
}
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b)} 实得 ${JSON.stringify(a)}`); };
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
const setEd = (v) => ev(`ace.edit(document.getElementById('editor')).setValue(${JSON.stringify(v)}, 1);`);
const selectRange = (a, b) => ev(`const ed=ace.edit(document.getElementById('editor')); const d=ed.session.doc; const p=d.indexToPosition(${a}), r=d.indexToPosition(${b}); ed.selection.setRange(new (ace.require('ace/range').Range)(p.row,p.column,r.row,r.column));`);
async function openQuery(inst, db) {
  await nav('/query');
  await ev('localStorage.clear();');
  await nav(`/query?inst=${inst}&db=${db}`);
  await waitFor("document.querySelector('#tree .tree-head, #tree .alert, #tree .tree-hint') && document.getElementById('db').value", '库表树加载');
  await sleep(150);
  await ev("window.__clip = null; Object.defineProperty(navigator.clipboard, 'writeText', { value: async (t) => { window.__clip = t; }, configurable: true });");
  // BT_SKIN=light / sepia：换皮肤再截图，看三套皮肤下的页签条和行号槽圆点
  if (process.env.BT_SKIN) await ev(`document.documentElement.setAttribute('data-skin', ${JSON.stringify(process.env.BT_SKIN)}); document.dispatchEvent(new Event('skinchange'));`);
}
const execCalls = () => calls.filter((c) => c.url === '/api/v1/query/execute');
const idle = () => waitFor("document.getElementById('run').getAttribute('aria-busy') === 'false'", '执行完成', 15000);
async function runAll(sql) {
  if (sql != null) await setEd(sql);
  await sleep(200); // 条数标签是防抖更新的
  await click('#run-all');
  await idle();
  await sleep(80);
}
async function runOne(sql) {
  if (sql != null) await setEd(sql);
  await click('#run');
  await idle();
  await sleep(80);
}
const tabs = () => qa('.bt-tab .bt-t1');
const activeTab = () => q('.bt-tab[aria-selected="true"] .bt-t1');
const clickTab = (label) => ev(`const t=[...document.querySelectorAll('.bt-tab')].find(e=>e.querySelector('.bt-t1').textContent===${JSON.stringify(label)}); if(!t) throw new Error('没有页签 ${label}'); t.click();`);
const tabClasses = () => qa('.bt-tab', 'className');
const sqlsSent = () => execCalls().map((c) => c.body.sql);
const mouse = (type, x, y, extra = {}) => send('Input.dispatchMouseEvent', { type, x, y, button: 'left', ...extra });
const center = (sel, idx = 0) => ev(`const e=document.querySelectorAll(${JSON.stringify(sel)})[${idx}]; e.scrollIntoView({block:'nearest',inline:'nearest'}); const r=e.getBoundingClientRect(); return {x: r.left + Math.min(r.width/2, 60), y: r.top + r.height/2};`);
async function clickAtEl(sel, idx = 0) {
  const p = await center(sel, idx);
  await mouse('mouseMoved', p.x, p.y);
  await mouse('mousePressed', p.x, p.y, { clickCount: 1 });
  await mouse('mouseReleased', p.x, p.y, { clickCount: 1 });
  await sleep(80);
}

const SCRIPT3 = '-- 订单概况\nselect 1 from orders;\n\nselect 2 from users;\n\nselect 3 from items;\n';

console.log(`运行全部（多结果页签）页面测试（http://127.0.0.1:${HTTP}/query）`);

// =============== 基本流程 ===============
await openQuery(1, 'shop_my');
await t('工具栏：有“运行全部”和“批量选项”；脚本三条时按钮上显示（3 条）', async () => {
  await setEd(SCRIPT3);
  await sleep(250);
  eq(await exists('#run-all'), true);
  eq(await exists('#batch-opts'), true);
  eq(await q('#run-all-n'), '（3 条）');
  eq(await q('#run-all', 'hidden'), false);
});
await t('运行全部：三条语句各发一次请求（不是整段脚本），顺序一致', async () => {
  await runAll();
  const sent = sqlsSent();
  eq(sent.length, 3);
  eq(sent[0], '-- 订单概况\nselect 1 from orders');
  eq(sent[1], 'select 2 from users');
  eq(sent[2], 'select 3 from items');
  eq(execCalls().every((c) => c.body.instance_id === 1 && c.body.db === 'shop_my'), true, '实例 / 库');
});
await t('页签条：汇总 + 结果 1 / 2 / 3，默认停在汇总；成功的页签不改颜色', async () => {
  eq(await tabs(), ['汇总', '结果 1', '结果 2', '结果 3']);
  eq(await activeTab(), '汇总');
  eq((await tabClasses()).some((c) => /st-(failed|timeout|blocked)/.test(c)), false);
});
await t('汇总：三行，状态都是成功，说明执行方式', async () => {
  eq((await qa('table.bt-ov tbody tr')).length, 3);
  eq(await qa('table.bt-ov tbody tr td.bt-stc'), ['成功', '成功', '成功']);
  has(await q('.bt-sum'), '共 3');
  has(await q('.bt-sum'), '依次执行 · 遇错停止');
  eq(await exists('#msg .alert'), false, '全部成功时不另外出提示条');
  await shot('batch_overview');
});
await t('汇总里点一行：切到对应的结果页签', async () => {
  await ev("document.querySelectorAll('table.bt-ov tbody tr')[1].click();");
  await sleep(80);
  eq(await activeTab(), '结果 2');
});
await t('结果页签里就是原来的结果面板：标题、复制；不再有“实际执行的 SQL”折叠，语句在页签的悬停提示里', async () => {
  has(await q('#result .res-meta'), '结果 2 · 1 行');
  eq(await exists('#result .res-meta .btn.ghost'), true, '复制按钮');
  eq(await exists('#result details.sql-fold'), false);
  has(await q('.bt-tab[aria-selected="true"]', 'title'), 'select 2 from users');
  eq(await exists('#result table.grid'), true);
  await shot('batch_result2');
});
await t('悬停单元格出现复制 / 预览工具条；双击单元格打开预览', async () => {
  const sel = '#result table.grid tbody tr:nth-child(1) td:nth-child(3)';
  const p = await center(sel);
  p.x = (await ev(`return document.querySelector(${JSON.stringify(sel)}).getBoundingClientRect().left;`)) + 8; // 单元格窄时中心会被悬停出来的工具条盖住，点左边
  await mouse('mouseMoved', p.x, p.y);
  await sleep(80);
  eq(await exists('.cell-tools:not([hidden])'), true);
  await mouse('mousePressed', p.x, p.y, { clickCount: 1 });
  await mouse('mouseReleased', p.x, p.y, { clickCount: 1 });
  await mouse('mousePressed', p.x, p.y, { clickCount: 2 });
  await mouse('mouseReleased', p.x, p.y, { clickCount: 2 });
  await waitFor("document.querySelector('.cv-dialog')", '预览弹窗');
  await ev("document.querySelector('.cv-dialog .cv-head button:last-child').click();");
  await sleep(100);
});
await t('列头有“复制整列”按钮', async () => {
  eq((await qa('#result table.grid th .col-copy')).length, 2);
});

// =============== 编辑器：行号槽圆点、高亮 ===============
await t('行号槽：每条语句第一行一个带序号的状态圆点（成功是绿色类）', async () => {
  const cls = await qa('.ace_gutter-cell.dbt-gm', 'className');
  eq(cls.length, 3);
  eq(cls.map((c) => (c.match(/dbt-gm-n(\d+)/) || [])[1]), ['1', '2', '3']);
  eq(cls.every((c) => c.includes('dbt-gm-ok')), true);
});
await t('当前页签对应的语句在编辑器里高亮', async () => {
  eq(await exists('.ace_marker-layer .dbt-stmt-act'), true);
  const row = await ev("const m=document.querySelector('.ace_marker-layer .dbt-stmt-act').getBoundingClientRect(); const g=document.querySelectorAll('.ace_gutter-cell.dbt-gm')[1].getBoundingClientRect(); return Math.abs(m.top - g.top) < 4;");
  eq(row, true, '高亮从结果 2 的语句第一行开始');
});
await t('点行号槽的圆点：跳到对应结果页签', async () => {
  await clickAtEl('.ace_gutter-cell.dbt-gm', 2);
  await sleep(100);
  eq(await activeTab(), '结果 3');
  await clickAtEl('.ace_gutter-cell.dbt-gm', 0);
  await sleep(100);
  eq(await activeTab(), '结果 1');
});
await t('键盘：方向键在页签间切换', async () => {
  await ev("document.querySelector('.bt-tab[aria-selected=\"true\"]').focus();");
  await ev("document.activeElement.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));");
  await sleep(100);
  eq(await activeTab(), '结果 2');
});
await t('右键页签：复制 SQL / 定位到语句', async () => {
  const p = await center('.bt-tab[aria-selected="true"]');
  await mouse('mouseMoved', p.x, p.y);
  await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: p.x, y: p.y, button: 'right', clickCount: 1 });
  await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: p.x, y: p.y, button: 'right', clickCount: 1 });
  await waitFor('document.querySelector(".pop-menu")', '右键菜单');
  eq(await qa('.pop-menu button'), ['复制 SQL', '定位到语句', '只重新运行这一条', '固定这个页签', '关闭', '关闭其他', '关闭右侧']);
  await ev("[...document.querySelectorAll('.pop-menu button')].find(b=>b.textContent==='复制 SQL').click();");
  await sleep(100);
  eq(await ev('return window.__clip;'), 'select 2 from users');
  await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: p.x, y: p.y, button: 'right', clickCount: 1 });
  await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: p.x, y: p.y, button: 'right', clickCount: 1 });
  await waitFor('document.querySelector(".pop-menu")', '右键菜单');
  await ev("[...document.querySelectorAll('.pop-menu button')].find(b=>b.textContent==='定位到语句').click();");
  await sleep(100);
  eq(await ev("return ace.edit(document.getElementById('editor')).getSelectedText();"), 'select 2 from users');
});

// =============== 单条运行保持原样 ===============
await t('运行（单条）不带固定页签：回到原来的单个结果，页签条和圆点都消失', async () => {
  await ev("ace.edit(document.getElementById('editor')).selection.clearSelection(); ace.edit(document.getElementById('editor')).moveCursorTo(0, 3);");
  const n = execCalls().length;
  await click('#run');
  await idle();
  await sleep(80);
  eq(execCalls().length, n + 1);
  eq(await exists('.bt-strip'), false);
  eq(await exists('.ace_gutter-cell.dbt-gm'), false);
  has(await q('#result .res-meta'), '结果 · 1 行');
  eq(await exists('.ace_marker-layer .dbt-stmt-act'), false);
});

// =============== 出错：停止 / 继续 ===============
await t('默认遇错停止：失败那条红字页签，后面的“未执行”，不再发请求', async () => {
  const n = execCalls().length;
  await runAll('select 1 from a;\nselect boom;\nselect 3 from c;\n');
  eq(execCalls().length - n, 2);
  eq(await tabs(), ['汇总', '结果 1', '结果 2', '结果 3']);
  const cls = await tabClasses();
  has(cls[2], 'st-failed');
  has(cls[3], 'st-skipped');
  has(await q('#msg'), '共 3 条：成功 1 条，失败 1 条，未执行 1 条');
  eq(await qa('table.bt-ov tbody tr td.bt-stc'), ['成功', '失败', '未执行']);
  await shot('batch_stop_on_error');
});
await t('失败页签：提示语 + 错误码 + 定位 / 重新运行', async () => {
  await clickTab('结果 2');
  await sleep(80);
  has(await q('.bt-err .alert'), "执行失败：Table 'shop.boom' doesn't exist");
  has(await q('.bt-err .alert .chip'), 'exec_error');
  eq(await qa('.bt-err .bt-acts .btn'), ['定位到语句', '重新运行']);
});
await t('未执行的页签：说明原因，可单独运行这一条', async () => {
  await clickTab('结果 3');
  await sleep(80);
  has(await q('.bt-state'), '前面第 2 条没有成功，已按设置停止');
  eq(await qa('.bt-state .btn'), ['运行这一条']);
});
await t('批量选项：改成“继续执行”后，失败的后面照常执行；选项会记住', async () => {
  await click('#batch-opts');
  eq(await exists('.bt-pop'), true);
  await ev("document.querySelector('.bt-pop input[name=bt-stop][value=\"0\"]').click();");
  await click('#batch-opts');
  eq(await exists('.bt-pop'), false);
  const n = execCalls().length;
  await runAll();
  eq(execCalls().length - n, 3);
  has(await q('.bt-sum'), '依次执行 · 遇错继续');
  eq(await qa('table.bt-ov tbody tr td.bt-stc'), ['成功', '失败', '成功']);
  eq(JSON.parse(await ev("return localStorage.getItem('dbtool.query.batch.v1');")), { stop: false, parallel: false });
});
await t('汇总：重试没成功的 → 只重发失败那一条', async () => {
  const n = execCalls().length;
  await ev("[...document.querySelectorAll('.bt-sum .btn')].find(b=>b.textContent.startsWith('重试没成功的')).click();");
  await idle();
  await sleep(100);
  eq(execCalls().length - n, 1);
  eq(sqlsSent().slice(-1)[0], 'select boom');
});
await t('无权限：页签里有“申请 … 的查询权限”入口', async () => {
  await runAll('select secret from t1;\nselect 2;\n');
  await clickTab('结果 1');
  await sleep(80);
  has(await q('.bt-err .alert'), '被拦截：');
  has(await q('.bt-err .alert'), '表的查询权限，请先申请。');
  has(await q('.bt-err .alert'), 'no_privilege');
  has(await q('.bt-err .alert'), 'shop_my.secret'); // 申请入口（链接或“需要申请”说明）
});
await t('被拦截 / 超时分别是橙色 / 红色页签', async () => {
  await runAll('select forbidden;\nselect timeoutq;\n');
  const cls = await tabClasses();
  has(cls[1], 'st-blocked');
  has(cls[2], 'st-timeout');
  eq(await qa('table.bt-ov tbody tr td.bt-stc'), ['被拦截', '超时']);
});

// 选项恢复默认，后面的用例按“遇错停止”
await ev("localStorage.setItem('dbtool.query.batch.v1', JSON.stringify({ stop: true, parallel: false }));");
await openQuery(1, 'shop_my');

// =============== 选区 / 条数上限 / 前置 SET ===============
await t('选中其中两条：按钮上显示（2 条），只运行选中的', async () => {
  await setEd('select 1 from a;\nselect 2 from b;\nselect 3 from c;\n');
  const text = 'select 1 from a;\nselect 2 from b;\nselect 3 from c;\n';
  await selectRange(text.indexOf('select 2'), text.indexOf('select 3') + 4);
  await sleep(250);
  eq(await q('#run-all-n'), '（2 条）');
  const n = execCalls().length;
  await click('#run-all');
  await idle();
  await sleep(80);
  eq(sqlsSent().slice(n), ['select 2 from b', 'select 3 from c']);
});
await t('超过 20 条：提示，一条都不执行', async () => {
  const many = Array.from({ length: 21 }, (_, i) => `select ${i + 1};`).join('\n');
  await setEd(many);
  await sleep(250);
  const n = execCalls().length;
  await click('#run-all');
  await sleep(300);
  eq(execCalls().length, n);
  has(await q('#msg'), '一次最多运行 20 条语句（现在有 21 条）');
});
await t('MySQL 脚本开头的 SET 不单独成页签，随每条语句一起提交', async () => {
  await runAll('SET @a = 1;\nselect @a;\nselect 2;\n');
  const sent = sqlsSent().slice(-2);
  eq(sent, ['SET @a = 1\n;\nselect @a', 'SET @a = 1\n;\nselect 2']);
  eq(await tabs(), ['汇总', '结果 1', '结果 2']);
});
await t('只有一条语句：不出页签条，和原来的单个结果一样', async () => {
  await runAll('select 1 from only_one;');
  eq(await exists('.bt-strip'), false);
  has(await q('#result .res-meta'), '结果 · 1 行');
  eq(await exists('.ace_gutter-cell.dbt-gm'), true, '行号槽里仍有圆点，说明这条是批量运行出来的');
});

// =============== 固定页签 / 关闭 ===============
await t('固定页签：下一次运行（单条或全部）不会替换它', async () => {
  await runAll('select 1 from a;\nselect 2 from b;\n');
  await clickTab('结果 2');
  await ev("document.querySelector('.bt-tab[aria-selected=\"true\"] .bt-pin').click();");
  await sleep(80);
  has((await tabClasses())[2], 'pinned');
  // 单条运行：并进同一排页签
  await setEd('select 9 from z;');
  await click('#run');
  await idle();
  await sleep(80);
  eq(await tabs(), ['结果 2', '结果 1']);
  has((await tabClasses())[0], 'pinned');
  has(await q('.bt-tab[aria-selected="true"]', 'title'), 'select 9 from z');
  await clickTab('结果 2');
  await sleep(80);
  has(await q('.bt-tab[aria-selected="true"]', 'title'), 'select 2 from b');
  // 再整批运行：固定的还在，汇总排在最前
  await runAll('select 5 from e;\nselect 6 from f;\n');
  eq(await tabs(), ['汇总', '结果 2', '结果 1', '结果 2']);
  await shot('batch_pinned');
});
await t('关闭页签：× 关掉当前，固定的不受影响；全关光后回到空状态', async () => {
  await ev("document.querySelectorAll('.bt-tab')[3].querySelector('.bt-x').click();");
  await sleep(80);
  eq(await tabs(), ['汇总', '结果 2', '结果 1']);
  await ev("document.querySelectorAll('.bt-tab')[2].dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, clientX: 200, clientY: 300 }));");
  await waitFor('document.querySelector(".pop-menu")', '右键菜单');
  await ev("[...document.querySelectorAll('.pop-menu button')].find(b=>b.textContent==='关闭其他').click();");
  await sleep(80);
  eq(await tabs(), ['汇总', '结果 1']);
  await ev("document.querySelectorAll('.bt-tab')[1].querySelector('.bt-x').click();");
  await sleep(80);
  eq(await exists('.bt-strip'), false);
  has(await q('#result .res-empty'), '运行查询后');
  eq(await exists('.ace_gutter-cell.dbt-gm'), false);
});

// =============== 执行中 / 取消 / 同时执行 / 原因 ===============
await openQuery(1, 'shop_my');
await t('执行中：未执行的是空心圆页签，正在跑的转圈；取消后其余标成已取消', async () => {
  await setEd('select 1 from a;\nselect delay1500 from b;\nselect 3 from c;\n');
  await sleep(250);
  await click('#run-all');
  await waitFor('document.querySelectorAll(".bt-tab .bt-si.running").length === 1', '第二条在执行');
  const cls = await tabClasses();
  has(cls[1], 'st-ok');
  has(cls[2], 'st-running');
  has(cls[3], 'st-pending');
  eq(await ev("return document.getElementById('cancel').hidden;"), false);
  eq(await ev("return document.getElementById('run-all').disabled;"), true, '运行中不能再点');
  await shot('batch_running');
  await click('#cancel');
  await idle();
  await sleep(100);
  eq(await qa('table.bt-ov tbody tr td.bt-stc'), ['成功', '已取消', '已取消']);
});
await t('同时执行（最多 3 条）：三条各慢 500ms，总用时明显小于 1500ms', async () => {
  await click('#batch-opts');
  await ev("document.querySelector('.bt-pop input[name=bt-par][value=\"1\"]').click();");
  await click('#batch-opts');
  const t0 = Date.now();
  await runAll('select delay500 a;\nselect delay500 b;\nselect delay500 c;\n');
  const ms = Date.now() - t0;
  if (ms > 1200) throw new Error('用时 ' + ms + 'ms，没有并发');
  has(await q('.bt-sum'), '同时执行');
  await ev("localStorage.setItem('dbtool.query.batch.v1', JSON.stringify({ stop: true, parallel: false }));");
});
await openQuery(1, 'shop_my');
await t('防火墙要求原因：整批只弹一次，带上原因重发', async () => {
  const n = execCalls().length;
  await setEd('select needsreason a;\nselect needsreason b;\n');
  await sleep(250);
  await click('#run-all');
  await waitFor('document.querySelector(".cf-dialog")', '原因弹窗');
  await ev("const i=document.querySelector('.cf-dialog textarea'); i.value='排查线上订单问题'; i.dispatchEvent(new Event('input'));");
  await ev("[...document.querySelectorAll('.cf-dialog button')].find(b => b.textContent.includes('提交并执行')).click();");
  await idle();
  await sleep(100);
  const sent = execCalls().slice(n);
  eq(sent.length, 4, '两条各先被要求原因、再带原因重发');
  eq(sent.filter((c) => c.body.reason === '排查线上订单问题').length, 2);
  eq(await ev("return document.querySelectorAll('.cf-dialog').length;"), 0, '没有第二次弹窗');
  eq(await qa('table.bt-ov tbody tr td.bt-stc'), ['成功', '成功']);
});
await t('防火墙要求原因：不填 → 这几条都标成已取消，不重发', async () => {
  const n = execCalls().length;
  await click('#run-all');
  await waitFor('document.querySelector(".cf-dialog")', '原因弹窗');
  await ev("[...document.querySelectorAll('.cf-dialog button')].find(b => b.textContent.includes('取消')).click();");
  await idle();
  await sleep(100);
  eq(execCalls().length - n, 2, '各被要求一次原因，之后没有重发');
  eq(await qa('table.bt-ov tbody tr td.bt-stc'), ['已取消', '已取消']);
  has(await q('.bt-ov'), '没有填写原因');
});

// =============== 执行计划、其他类型 ===============
await t('批量结果之后点“执行计划”：多出一个“执行计划”页签，结果页签还在', async () => {
  await runAll('select 1 from a;\nselect 2 from b;\n');
  await ev("ace.edit(document.getElementById('editor')).moveCursorTo(0, 3);");
  await click('#explain');
  await idle();
  await sleep(100);
  eq(await tabs(), ['汇总', '结果 1', '结果 2', '执行计划']);
  eq(await activeTab(), '执行计划');
  has(await q('#result .res-meta'), '执行计划');
  await clickTab('结果 1');
  await sleep(80);
  has(await q('#result .res-meta'), '结果 1 · 1 行');
});
await t('Redis / MongoDB 实例：没有“运行全部”和“批量选项”', async () => {
  await ev("const s=document.getElementById('inst'); s.value='3'; s.dispatchEvent(new Event('change'));");
  await waitFor("document.getElementById('run-all').hidden === true", 'Redis 隐藏运行全部');
  eq(await q('#batch-opts', 'hidden'), true);
  await ev("const s=document.getElementById('inst'); s.value='5'; s.dispatchEvent(new Event('change'));");
  await sleep(300);
  eq(await q('#run-all', 'hidden'), true);
  await ev("const s=document.getElementById('inst'); s.value='4'; s.dispatchEvent(new Event('change'));");
  await waitFor("document.getElementById('run-all').hidden === false && document.getElementById('db').value === 'shop'", 'PostgreSQL 显示运行全部');
});
await t('PostgreSQL：同样一条语句一个页签（不带 MySQL 的前置 SET 规则）', async () => {
  await runAll('SET search_path = public;\nselect 1;\nselect 2;\n');
  const sent = sqlsSent().slice(-3);
  eq(sent, ['SET search_path = public', 'select 1', 'select 2']);
  eq(await tabs(), ['汇总', '结果 1', '结果 2', '结果 3']);
});
await t('切换编辑器标签：各自的批量结果互不影响', async () => {
  await openQuery(1, 'shop_my');
  await runAll('select 1 from a;\nselect 2 from b;\n');
  await click('#tabs .tab:last-child'); // “+” 新建标签
  await sleep(300);
  eq(await exists('.bt-strip'), false);
  eq(await exists('.ace_gutter-cell.dbt-gm'), false);
  await ev("document.querySelectorAll('#tabs .tab')[0].click();");
  await sleep(300);
  eq(await tabs(), ['汇总', '结果 1', '结果 2']);
  eq((await qa('.ace_gutter-cell.dbt-gm')).length, 2);
});
await t('批量选项弹层：两组各两个单选项', async () => {
  await click('#batch-opts');
  eq((await qa('.bt-pop label')).length, 4);
  await click('#batch-opts');
});

// 测试故意触发的 400 / 403 / 408 / 428 响应，浏览器会各记一条 “Failed to load resource”，不算页面报错
await t('控制台零报错', async () => { eq(consoleErrors.filter((e) => !/favicon|Failed to load resource/.test(e)), []); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
