/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_query_fill.mjs
 * @time: 2026/10/28
 */
// 在线查询页“结果区随右侧面板剩余高度伸缩”的页面级测试：
// 复用 multi_engine_fake.mjs 的假后端（真实的 query/index.html 与前端脚本）+ 无头 Chrome（CDP），不依赖第三方包。
// 要点：结果区占满右侧面板剩余的高度，结果表在结果框里滚动；页签 / 编辑器 / 工具栏保持原高度；行很多时右侧面板不再出现第二道滚动条。
// 用法：node scripts/frontend-test/run_query_fill.mjs [chrome路径]    环境变量 QF_HTTP_PORT（默认 8935）/ QF_CDP_PORT（默认 9272）/ QF_SHOT（截图目录）。
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { startServer, calls } from './multi_engine_fake.mjs';

const HTTP = +(process.env.QF_HTTP_PORT || 8935);
const CDP = +(process.env.QF_CDP_PORT || 9272);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const server = await startServer(HTTP);
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'qf-chrome-'));
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
const J = JSON.stringify;
const eq = (a, b, m) => { if (J(a) !== J(b)) throw new Error(`${m || ''} 期望 ${J(b)} 实得 ${J(a)}`); };
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };
const near = (a, b, tol, m) => { if (Math.abs(a - b) > tol) throw new Error(`${m || ''} 期望约 ${b}（±${tol}）实得 ${a}`); };
const setEd = (v) => ev(`const e=ace.edit(document.getElementById('editor')); e.setValue(${J(v)}, 1);`);

async function openQuery(w, h) {
  await send('Emulation.setDeviceMetricsOverride', { width: w, height: h, deviceScaleFactor: 1, mobile: false });
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/query` });
  await sleep(200);
  await waitFor("document.readyState === 'complete'", '页面加载');
  await ev('localStorage.clear();');
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/query?inst=1&db=shop` });
  await sleep(200);
  await waitFor("document.readyState === 'complete'", '页面加载');
  await waitFor("document.getElementById('db').value", '库');
  // 假后端的页面外壳多包了一层 .content、也没有顶栏；这里还原成真实外壳的结构（.main > 顶栏 + 页面内容），布局才和线上一致
  await ev("const c = document.querySelector('.main > .content'); if (c) c.replaceWith(...c.childNodes); if (!document.querySelector('.main > .topbar')) { const h = document.createElement('header'); h.className = 'topbar'; h.innerHTML = '<b>顶栏</b>'; document.querySelector('.main').prepend(h); }");
  await sleep(150);
}
// 运行一条查询，等到结果头里出现“· N 行”（不能只等 .grid-wrap：上一次的结果还在页面上）
async function run(sql, rows = 1) {
  await setEd(sql);
  await ev("document.getElementById('run').click();");
  try {
    await waitFor(`(document.querySelector('#result .res-meta') || {}).textContent && document.querySelector('#result .res-meta').textContent.includes('· ${rows} 行')`, '结果');
  } catch (e) {
    const recent = calls.filter((c) => c.url.includes('/api/v1/query/') && c.method === 'POST').slice(-2);
    const seen = await ev("return (document.getElementById('msg') || {}).textContent + ' | ' + document.getElementById('result').innerText.slice(0, 160).split('\\n').join(' ');");
    throw new Error(`${e.message}；最近的查询请求：${J(recent).slice(0, 300)}；页面上：${seen}`);
  }
  await sleep(150);
}
const measure = () => ev(`
  const R = (e) => { if (!e) return null; const b = e.getBoundingClientRect(); return { t: Math.round(b.top), b: Math.round(b.bottom), h: Math.round(b.height) }; };
  const wm = document.querySelector('.wb-main'); const res = document.getElementById('result'); const gw = res.querySelector('.grid-wrap'); const body = res.querySelector('.res-body');
  const pad = parseFloat(getComputedStyle(wm).paddingBottom);
  return { vh: innerHeight, pad, wbMain: R(wm), result: R(res), grid: R(gw), body: R(body), editor: R(document.querySelector('.editor-wrap')), toolbar: R(document.querySelector('.wb-main > .toolbar')),
    mainScroll: [wm.scrollHeight, wm.clientHeight], gridScroll: gw ? [gw.scrollHeight, gw.clientHeight] : null, gridMax: gw ? getComputedStyle(gw).maxHeight : null,
    bodyScroll: body ? [body.scrollHeight, body.clientHeight] : null, docScroll: [document.documentElement.scrollHeight, innerHeight] };`);

console.log(`在线查询“结果区随剩余高度伸缩”测试（http://127.0.0.1:${HTTP}/query）`);

for (const [w, h] of [[1920, 1080], [1440, 900], [1366, 768]]) {
  await t(`${w}×${h} 结果行很少：结果框一直撑到右侧面板底部，右侧面板不滚动`, async () => {
    await openQuery(w, h);
    await run('select id, phone from orders', 1);
    const m = await measure();
    near(m.result.b + m.pad, m.wbMain.b, 2, '结果框底边应贴着面板内边距');
    truthy(m.mainScroll[0] <= m.mainScroll[1] + 1, `右侧面板不应滚动 ${J(m.mainScroll)}`);
    truthy(m.grid.b <= m.result.b + 1, `结果表不应超出结果框 ${m.grid.b} > ${m.result.b}`);
    eq(m.editor.h, 220, '编辑器高度不被压缩');
    if (process.env.QF_SHOT && w === 1440) {
      fs.mkdirSync(process.env.QF_SHOT, { recursive: true });
      fs.writeFileSync(path.join(process.env.QF_SHOT, 'query_few.png'), Buffer.from((await send('Page.captureScreenshot', { format: 'png' })).data, 'base64'));
    }
  });
  await t(`${w}×${h} 结果 300 行：结果表在结果框里滚动，右侧面板没有第二道滚动条，结果框高度不变`, async () => {
    await openQuery(w, h);
    await run('select id, phone from orders', 1);
    const few = await measure();
    await run('select * from orders manyrows', 300);
    const many = await measure();
    truthy(many.gridScroll[0] > many.gridScroll[1] * 2, `结果表内部应滚动 ${J(many.gridScroll)}`);
    eq(many.gridMax, 'none', '不再限制 52vh');
    truthy(many.mainScroll[0] <= many.mainScroll[1] + 1, `右侧面板不应出现第二道滚动条 ${J(many.mainScroll)}`);
    truthy(many.grid.b <= many.result.b + 1, `结果表不应超出结果框 ${many.grid.b} > ${many.result.b}`);
    eq([many.result.t, many.result.b], [few.result.t, few.result.b], '结果框位置和高度不随行数变');
    eq([many.editor.h, many.toolbar.h], [few.editor.h, few.toolbar.h], '编辑器 / 工具栏高度不变');
    if (process.env.QF_SHOT && w === 1440) {
      fs.writeFileSync(path.join(process.env.QF_SHOT, 'query_many.png'), Buffer.from((await send('Page.captureScreenshot', { format: 'png' })).data, 'base64'));
    }
  });
}

await t('还没运行查询（空状态）：结果框也撑满剩余高度', async () => {
  await openQuery(1440, 900);
  const m = await measure();
  near(m.result.b + m.pad, m.wbMain.b, 2, '空状态的结果框底边');
  truthy(m.mainScroll[0] <= m.mainScroll[1] + 1, `右侧面板不应滚动 ${J(m.mainScroll)}`);
});

await t('执行计划：计划表同样在结果框里滚动，不出现第二道滚动条', async () => {
  await openQuery(1440, 900);
  await setEd('select * from orders');
  await ev("document.getElementById('explain').click();");
  await waitFor("document.querySelector('#result .res-body .grid-wrap')", '执行计划');
  await sleep(150);
  const m = await measure();
  near(m.result.b + m.pad, m.wbMain.b, 2, '结果框底边');
  truthy(m.mainScroll[0] <= m.mainScroll[1] + 1, `右侧面板不应滚动 ${J(m.mainScroll)}`);
  truthy(m.grid.b <= m.result.b + 1, '计划表不应超出结果框');
});

await t('窗口很矮（1280×600）：结果框保持最小高度 200，由右侧面板滚动', async () => {
  await openQuery(1280, 600);
  await run('select * from orders manyrows', 300);
  const m = await measure();
  truthy(m.result.h >= 200, `结果框最小高度 ${m.result.h}`);
  // 结果头 + 提示 + 结果表最小高度加起来放不进 200px 时，是结果框自己（.res-body）出滚动条，而不是内容溢出到框外
  truthy(m.grid.b <= m.result.b + 1 || m.bodyScroll[0] > m.bodyScroll[1], `结果表要么在结果框里，要么结果框自己能滚动 ${J(m.bodyScroll)}`);
  eq(m.editor.h, 220, '编辑器高度不被压缩');
});

await t('编辑器可以拖高：结果框相应变矮，面板仍不滚动', async () => {
  await openQuery(1440, 900);
  await run('select * from orders manyrows', 300);
  const a = await measure();
  await ev("document.querySelector('.editor-wrap').style.height = '320px';");
  await sleep(150);
  const b = await measure();
  eq(b.editor.h, 320, '编辑器高度');
  near(a.result.h - b.result.h, 100, 2, `结果框矮了 100（之前 ${J(a.result)}，之后 ${J(b.result)}，面板 ${J(b.wbMain)}）`);
  truthy(b.mainScroll[0] <= b.mainScroll[1] + 1, `右侧面板不应滚动 ${J(b.mainScroll)}`);
});

await t('手机宽度 375：上下叠放的布局不受影响，结果表仍然最高 52vh，页面没有横向滚动', async () => {
  await openQuery(375, 812);
  await run('select * from orders manyrows', 300);
  const m = await ev(`
    const gw = document.querySelector('#result .grid-wrap');
    return { maxH: getComputedStyle(gw).maxHeight, h: Math.round(gw.getBoundingClientRect().height), vh: innerHeight, hs: document.documentElement.scrollWidth - innerWidth };`);
  eq(m.maxH, Math.round(m.vh * 0.52 * 100) / 100 + 'px', '手机宽度下仍是 52vh');
  truthy(m.h <= m.vh * 0.52 + 1, `结果表最高 52vh，实得 ${m.h}`);
  truthy(m.hs <= 1, `页面出现横向滚动 ${m.hs}`);
});

await t('全程无控制台错误', async () => { eq(consoleErrors.filter((e) => !/favicon.ico/.test(e)), [], '控制台错误'); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
