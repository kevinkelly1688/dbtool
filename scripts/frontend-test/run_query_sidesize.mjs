/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_query_sidesize.mjs
 * @time: 2026/10/28
 */
// 在线查询左侧对象栏拖动调整宽度的回归测试：静态服务 + 无头 Chrome（CDP），不依赖第三方包。
// 页面只摆出工作台的骨架（.wb-body / 左栏 / 右栏），分隔条的标记取自真实模板 web/templates/query/index.html，
// 样式和脚本用真实的 app.css / query.css / js/query/sidesize.js（严格 CSP：style-src 没有 unsafe-inline）。
// 用法：node scripts/frontend-test/run_query_sidesize.mjs [chrome路径]
// 环境变量：QS_HTTP_PORT（默认 8938）、QS_CDP_PORT（默认 9281）、QS_SHOT_DIR（给一个目录就保存截图）。
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { untranslate } from './tpl.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '../..');
const staticRoot = path.join(root, 'web/static');
const HTTP = +(process.env.QS_HTTP_PORT || 8938);
const CDP = +(process.env.QS_CDP_PORT || 9281);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const base = `http://127.0.0.1:${HTTP}`;
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'";
const MIME = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8' };
const J = JSON.stringify;

// 分隔条的标记取自真实模板
const tplSrc = untranslate(fs.readFileSync(path.join(root, 'web/templates/query/index.html'), 'utf8'));
const resizer = (tplSrc.match(/<div class="wb-resizer"[^>]*><\/div>/) || [])[0];
if (!resizer) throw new Error('模板里没有 .wb-resizer');

const LONG = 'dbo.sp_Api_C2C_CancelRechargeOrderWithRefundNotifyCallback_v2';
const items = ['dbo.aspnet_UsersInRoles_AddUsersToRoles', LONG, 'dbo.sp_Get_Agent_Down'].map((n) => `<button type="button" class="prog-item"><span class="pg">·</span><span class="pn">${n}</span></button>`).join('');

function pageHtml() {
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>在线查询</title><link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/shell.css"><link rel="stylesheet" href="/static/css/query.css"></head>
<body><div class="app"><div class="main">
<div class="wb-body" id="wb">
  <aside class="wb-side">
    <div class="side-pane" id="side-p-prog"><div class="search"><input id="prog-q" type="search"></div><div id="prog-tree">${items}</div></div>
    ${resizer}
  </aside>
  <section class="wb-main"><div id="box" style-note="右栏">编辑器和结果</div></section>
</div></div></div><script type="module" src="/boot.js"></script></body></html>`;
}
const BOOT = `import { initSideResize } from '/static/js/query/sidesize.js';
window.__calls = 0;
initSideResize(document.getElementById('wb'), document.getElementById('wb-resizer'), () => { window.__calls++; });
`;

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  try {
    if (url.pathname === '/') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP }); return res.end(pageHtml()); }
    if (url.pathname === '/boot.js') { res.writeHead(200, { 'Content-Type': MIME['.js'] }); return res.end(BOOT); }
    if (url.pathname.startsWith('/static/')) {
      const file = path.join(staticRoot, decodeURIComponent(url.pathname.slice(8)));
      if (file.startsWith(staticRoot) && fs.existsSync(file)) {
        res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream', 'Cache-Control': 'no-store' });
        return fs.createReadStream(file).pipe(res);
      }
    }
    res.writeHead(404); res.end('nf');
  } catch (e) {
    res.writeHead(500); res.end(String(e && e.stack));
  }
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));

// ---------------------------------------------------------------- 无头 Chrome
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'qs-chrome-'));
const chrome = spawn(chromePath, ['--headless=new', `--remote-debugging-port=${CDP}`, `--user-data-dir=${profile}`, '--no-first-run', '--disable-gpu', 'about:blank'], { stdio: 'ignore' });
const cleanup = () => {
  try { chrome.kill(); } catch (_) { /* 已退出 */ }
  server.close();
  setTimeout(() => { try { fs.rmSync(profile, { recursive: true, force: true }); } catch (_) { /* 占用中 */ } }, 1500).unref();
};
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
const errors = [];
ws.addEventListener('message', (m) => {
  const d = JSON.parse(m.data);
  if (d.id && pend.has(d.id)) {
    const { res, rej } = pend.get(d.id);
    pend.delete(d.id);
    if (d.error) rej(new Error(d.error.message)); else res(d.result);
  } else if (d.method === 'Runtime.exceptionThrown') {
    errors.push('exception: ' + d.params.exceptionDetails.text + ' ' + ((d.params.exceptionDetails.exception || {}).description || ''));
  } else if (d.method === 'Log.entryAdded' && d.params.entry.level === 'error') {
    if (/favicon\.ico/.test(d.params.entry.url || '')) return; // 骨架页没有图标
    errors.push('log: ' + d.params.entry.text + ' ' + (d.params.entry.url || ''));
  } else if (d.method === 'Runtime.consoleAPICalled' && d.params.type === 'error') {
    errors.push('console: ' + d.params.args.map((a) => a.value || a.description).join(' '));
  }
});
const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id; pend.set(i, { res, rej }); ws.send(J({ id: i, method, params })); });
await send('Runtime.enable');
await send('Page.enable');
await send('Log.enable');

async function ev(expr) {
  const r = await send('Runtime.evaluate', { expression: `(async()=>{${expr}})()`, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error('页面异常：' + ((r.exceptionDetails.exception || {}).description || r.exceptionDetails.text));
  return r.result.value;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let pass = 0, fail = 0;
const failures = [];
async function t(name, fn) {
  try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; failures.push(name); console.log('  FAIL ' + name + '\n       ' + e.message); }
}
const eq = (a, b, m) => { if (J(a) !== J(b)) throw new Error(`${m || ''} 期望 ${J(b)} 实得 ${J(a)}`); };
const near = (a, b, m, d = 1) => { if (Math.abs(a - b) > d) throw new Error(`${m || ''} 相差超过 ${d}px：${a} vs ${b}`); };

async function open(w = 1300, clear = true) {
  await send('Emulation.setDeviceMetricsOverride', { width: w, height: 800, deviceScaleFactor: 1, mobile: false });
  await send('Page.navigate', { url: `${base}/` });
  for (let i = 0; i < 80; i++) { if (await ev("return !!document.getElementById('wb-resizer') && document.getElementById('wb-resizer').hasAttribute('aria-valuenow')").catch(() => false)) break; await sleep(60); }
  if (clear) await ev("try { localStorage.clear(); } catch (_) {}");
  await sleep(100);
}
const geo = () => ev(`const r = (e) => { const b = e.getBoundingClientRect(); return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height), r: Math.round(b.right) }; };
  const pn = [...document.querySelectorAll('.pn')].map((e) => ({ text: e.textContent, cut: e.scrollWidth > e.clientWidth }));
  return { side: r(document.querySelector('.wb-side')), main: r(document.querySelector('.wb-main')), handle: r(document.getElementById('wb-resizer')), pn,
    stored: (() => { try { return localStorage.getItem('dbtool.query.sideWidth.v1'); } catch (_) { return 'x'; } })(),
    calls: window.__calls, hscroll: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    valuenow: document.getElementById('wb-resizer').getAttribute('aria-valuenow'), resizing: document.body.classList.contains('wb-resizing') };`);
const mouse = (type, x, y, extra = {}) => send('Input.dispatchMouseEvent', { type, x, y, button: 'left', buttons: type === 'mouseReleased' ? 0 : 1, clickCount: 1, ...extra });
// 从分隔条中点按下，水平拖到 toX，松开
async function drag(toX) {
  const g = await geo();
  const x = g.handle.x + Math.round(g.handle.w / 2);
  const y = g.handle.y + 200;
  await mouse('mouseMoved', x, y, { button: 'none', buttons: 0 });
  await mouse('mousePressed', x, y);
  const steps = 6;
  for (let i = 1; i <= steps; i++) await mouse('mouseMoved', Math.round(x + ((toX - x) * i) / steps), y);
  await mouse('mouseReleased', toX, y);
  await sleep(150);
}
const shot = async (name) => {
  if (!process.env.QS_SHOT_DIR) return;
  fs.mkdirSync(process.env.QS_SHOT_DIR, { recursive: true });
  const s = await send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(path.join(process.env.QS_SHOT_DIR, name + '.png'), Buffer.from(s.data, 'base64'));
};

console.log(`在线查询左侧栏拖动调整宽度（${base}）`);
await open();
await t('默认宽度 240；分隔条是可聚焦的竖向 separator，压在左栏右边线上；长的存储过程名被省略号截断', async () => {
  const g = await geo();
  eq(g.side.w, 240);
  eq(await ev("const h = document.getElementById('wb-resizer'); return [h.getAttribute('role'), h.getAttribute('aria-orientation'), h.tabIndex, h.title.length > 0, h.getAttribute('aria-label').length > 0]"), ['separator', 'vertical', 0, true, true]);
  near(g.handle.x + g.handle.w / 2, g.side.r, '分隔条骑在左栏右边线上', 2);
  eq(g.pn.find((p) => p.text.includes('CancelRecharge')).cut, true, '默认宽度下长名字被截断');
  await shot('qs-default');
});
await t('向右拖 260px：左栏变宽、右栏跟着让出，长名字完整显示，宽度记进浏览器，松手后通知编辑器重新量宽度', async () => {
  const before = (await geo()).calls;
  await drag((await geo()).handle.x + 4 + 260);
  const g = await geo();
  near(g.side.w, 500, '左栏宽度', 2);
  near(g.main.x, g.side.r, '右栏紧跟左栏', 2);
  eq(g.pn.every((p) => !p.cut), true, '所有名字都能完整显示');
  eq(g.stored, String(g.side.w));
  eq(g.valuenow, String(g.side.w));
  eq(g.resizing, false, '松手后恢复光标和选择');
  if (g.calls <= before) throw new Error('宽度变化后应通知编辑器重新量宽度');
  eq(g.hscroll <= 0, true, '没有页面级横向滚动');
  await shot('qs-wide');
});
await t('刷新页面后沿用上次的宽度', async () => {
  await open(1300, false);
  near((await geo()).side.w, 500, '', 2);
});
await t('拖得太远：最宽 720（并且右栏至少留 360）；拖到最左：最窄 180', async () => {
  await drag(1290);
  near((await geo()).side.w, 720, '', 1);
  await drag(5);
  near((await geo()).side.w, 180, '', 1);
});
await t('窗口不够宽时最宽受右栏最小宽度限制：900 宽的窗口左栏最多 540，窗口放大后恢复用户要的宽度', async () => {
  await open(1300);
  await drag(700); // 要 700
  near((await geo()).side.w, 700, '', 2);
  await send('Emulation.setDeviceMetricsOverride', { width: 900, height: 800, deviceScaleFactor: 1, mobile: false });
  await sleep(250);
  near((await geo()).side.w, 540, '窗口窄了，左栏让位给右栏', 1);
  await send('Emulation.setDeviceMetricsOverride', { width: 1300, height: 800, deviceScaleFactor: 1, mobile: false });
  await sleep(250);
  near((await geo()).side.w, 700, '窗口放大后回到用户要的宽度', 1);
});
await t('双击分隔条恢复默认宽度，并清掉记忆', async () => {
  await open(1300);
  await drag((await geo()).handle.x + 100);
  await ev("document.getElementById('wb-resizer').dispatchEvent(new MouseEvent('dblclick', { bubbles: true }))");
  await sleep(100);
  const g = await geo();
  eq([g.side.w, g.stored], [240, null]);
});
await t('键盘：聚焦后 → 加宽 16、Shift+→ 加宽 64、← 变窄，Home 恢复默认', async () => {
  await open(1300);
  await ev("document.getElementById('wb-resizer').focus()");
  const key = (k, extra = {}) => send('Input.dispatchKeyEvent', { type: 'keyDown', key: k, code: k, ...extra });
  await key('ArrowRight');
  eq((await geo()).side.w, 256);
  await key('ArrowRight', { modifiers: 8 }); // Shift
  eq((await geo()).side.w, 320);
  await key('ArrowLeft');
  eq((await geo()).side.w, 304);
  eq((await geo()).stored, '304');
  await key('Home');
  const g = await geo();
  eq([g.side.w, g.stored], [240, null]);
});
await t('手机宽度 375：上下排版（左栏在编辑器上面），分隔条隐藏', async () => {
  await open(375);
  eq(await ev("return getComputedStyle(document.getElementById('wb-resizer')).display"), 'none');
  const g = await geo();
  eq([g.side.x, g.main.x], [0, 0], '上下排版');
  eq(g.main.y >= g.side.y + g.side.h - 1, true, '右栏在左栏下面');
});
await t('localStorage 不可用（隐私模式）时照常使用', async () => {
  await send('Page.addScriptToEvaluateOnNewDocument', { source: "Object.defineProperty(window, 'localStorage', { get() { throw new Error('denied'); } });" });
  await open(1300, false);
  await drag((await geo()).handle.x + 4 + 120);
  near((await geo()).side.w, 360, '', 2);
});
await t('全程无控制台错误 / CSP 违规', async () => { eq(errors, [], '控制台错误'); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
if (fail) console.log('失败项：\n  ' + failures.join('\n  '));
cleanup();
process.exit(fail ? 1 : 0);
