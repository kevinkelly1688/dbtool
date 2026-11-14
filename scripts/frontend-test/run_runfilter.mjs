/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_runfilter.mjs
 * @time: 2026/10/28
 */
// “立即巡检”弹窗实例搜索（web/static/js/inspect/runfilter.js）的浏览器回归：最小页面 + 真实静态文件 + 无头 Chrome。
// 用法：node scripts/frontend-test/run_runfilter.mjs [chrome路径]    环境变量 RF_HTTP_PORT（默认 8928）/ RF_CDP_PORT（默认 9265）。
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const staticRoot = path.resolve(here, '../../web/static');
const HTTP = +(process.env.RF_HTTP_PORT || 8928);
const CDP = +(process.env.RF_CDP_PORT || 9265);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';

const INSTS = [
  ['SQL Server', ['(Sit) shop-2001 [SQL-Server]-(华东)', '(Sit) shop-2007 [SQL-Server]-(华南)', '(演示站) shop-9900 [SQL-Server]-(华南)', 'ar001', 'ar002']],
  ['MySQL / 兼容类型', ['prod-mysql-01', 'prod-mysql-02', 'AR-Report']],
  ['Redis', ['redis-cache-01']],
];
const html = INSTS.map(([g, l]) => `<b class="grp">${g}</b>` + l.map((n, i) => `<label><input type="checkbox" value="${g}-${i}">${n}</label>`).join('')).join('');
const PAGE = `<!doctype html><html><head><meta charset="utf-8"></head><body>
<input type="search" id="q"><div id="list">${html}</div>
<script type="module">
import { applyRunFilter } from '/static/js/inspect/runfilter.js';
window.f = (q) => applyRunFilter(document.getElementById('list'), q);
window.ready = true;
</script></body></html>`;

const server = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x');
  if (u.pathname === '/') { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(PAGE); return; }
  const f = path.join(staticRoot, u.pathname.replace(/^\/static\//, ''));
  if (u.pathname.startsWith('/static/') && f.startsWith(staticRoot) && fs.existsSync(f)) {
    res.writeHead(200, { 'Content-Type': f.endsWith('.js') ? 'text/javascript' : 'text/plain' });
    res.end(fs.readFileSync(f));
    return;
  }
  res.writeHead(404);
  res.end('nf');
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'rf-chrome-'));
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
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function getWs() {
  for (let i = 0; i < 80; i++) {
    try {
      const l = await (await fetch(`http://127.0.0.1:${CDP}/json`)).json();
      const p = l.find((x) => x.type === 'page');
      if (p) return p.webSocketDebuggerUrl;
    } catch (_) { /* 等 Chrome */ }
    await sleep(250);
  }
  throw new Error('Chrome 未启动');
}
const ws = new WebSocket(await getWs());
await new Promise((r) => ws.addEventListener('open', r));
let id = 0;
const pend = new Map();
ws.addEventListener('message', (m) => {
  const d = JSON.parse(m.data);
  if (d.id && pend.has(d.id)) { const { res, rej } = pend.get(d.id); pend.delete(d.id); if (d.error) rej(new Error(d.error.message)); else res(d.result); }
});
const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id; pend.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
await send('Runtime.enable');
await send('Page.enable');
async function ev(expr) {
  const r = await send('Runtime.evaluate', { expression: `(async()=>{${expr}})()`, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error('页面异常：' + ((r.exceptionDetails.exception || {}).description || r.exceptionDetails.text));
  return r.result.value;
}
let pass = 0;
let fail = 0;
async function t(name, fn) { try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; console.log('  FAIL ' + name + '\n       ' + e.message); } }
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b)} 实得 ${JSON.stringify(a)}`); };
await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/` });
for (let i = 0; i < 60; i++) { if (await ev('return !!window.ready').catch(() => false)) break; await sleep(100); }

const run = (q) => ev(`const r = window.f(${JSON.stringify(q)}); return { r, names: [...document.querySelectorAll('#list label:not([hidden])')].map(l=>l.textContent), heads: [...document.querySelectorAll('#list .grp:not([hidden])')].map(b=>b.textContent) };`);

console.log('立即巡检实例搜索测试');
await t('空关键字：全部显示', async () => {
  const x = await run('');
  eq(x.r, { shown: 9, total: 9 });
  eq(x.heads.length, 3);
});
await t('子串匹配，不区分大小写；没有匹配项的类型标题一起隐藏', async () => {
  const x = await run('MYSQL');
  eq(x.names, ['prod-mysql-01', 'prod-mysql-02']);
  eq(x.heads, ['MySQL / 兼容类型']);
});
await t('空格分隔的每个词都要出现；中文也行', async () => {
  eq((await run('shop 华南')).names, ['(Sit) shop-2007 [SQL-Server]-(华南)', '(演示站) shop-9900 [SQL-Server]-(华南)']);
  eq((await run('华东')).names, ['(Sit) shop-2001 [SQL-Server]-(华东)']);
});
await t('没有子串命中时退到按顺序出现的字符（模糊）', async () => {
  eq((await run('pdm1')).names, ['prod-mysql-01']); // p…d…m…1
});
await t('没有任何匹配：返回 0 项、标题全隐藏', async () => {
  const x = await run('zzzz');
  eq(x.r.shown, 0);
  eq(x.heads, []);
});
await t('已勾选的实例不会因为被筛掉而取消勾选', async () => {
  await run('');
  await ev("document.querySelector('#list label input').checked = true;");
  await run('redis');
  eq(await ev("return document.querySelectorAll('#list input:checked').length;"), 1);
  eq((await run('')).r.shown, 9);
});

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
