/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_cancelrun.mjs
 * @time: 2026/10/28
 */
// 工单详情页“取消执行”按钮的浏览器回归：原因必填 → 弹确认（说明后果）→ 确认后才发请求，请求里带原因。
// 最小页面（只含 detail.js 需要的元素）+ 真实静态文件 + 假接口 + 无头 Chrome（CDP）。
// 用法：node scripts/frontend-test/run_cancelrun.mjs [chrome路径]    环境变量 CR_HTTP_PORT（默认 8931）/ CR_CDP_PORT（默认 9268）。
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const staticRoot = path.resolve(here, '../../web/static');
const HTTP = +(process.env.CR_HTTP_PORT || 8931);
const CDP = +(process.env.CR_CDP_PORT || 9268);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';

const PAGE = `<!doctype html><html><head><meta charset="utf-8"><meta name="csrf-token" content="tok"><link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/sqlflow.css"></head>
<body><main id="detail" data-id="7" data-status="workflow_executing" data-pending="true" data-instance="prod-mssql-01" data-db="shopdb" data-batch="false"><div id="detail-msg"></div>
<section class="card" id="progress-card"><h2>执行进度</h2>
<div id="cancel-run-box"><label for="cancel-reason">原因</label><input type="text" id="cancel-reason" maxlength="500">
<div class="actions"><button class="btn danger" id="act-cancel-run" type="button">取消执行</button></div></div></section></main>
<script type="module" src="/static/js/sqlflow/detail.js"></script></body></html>`;

const calls = [];
const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, 'http://x');
  let body = '';
  for await (const c of req) body += c;
  if (u.pathname === '/') { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(PAGE); return; }
  if (u.pathname.startsWith('/api/')) {
    if (req.method !== 'GET') calls.push({ method: req.method, url: u.pathname, body: body ? JSON.parse(body) : null });
    res.writeHead(200, { 'Content-Type': 'application/json' });
    if (u.pathname === '/api/v1/sqlworkflow/7/cancel-run') { res.end(JSON.stringify({ data: { status: 'workflow_executing', status_label: '执行中', message: '已发出取消请求' } })); return; }
    res.end(JSON.stringify({ data: { status: 'workflow_executing', active: true, done: 0, total: 0 } }));
    return;
  }
  const f = path.join(staticRoot, u.pathname.replace(/^\/static\//, ''));
  if (u.pathname.startsWith('/static/') && f.startsWith(staticRoot) && fs.existsSync(f) && fs.statSync(f).isFile()) {
    res.writeHead(200, { 'Content-Type': f.endsWith('.js') ? 'text/javascript' : f.endsWith('.css') ? 'text/css' : 'application/octet-stream' });
    res.end(fs.readFileSync(f));
    return;
  }
  res.writeHead(404);
  res.end('nf');
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'cr-chrome-'));
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
const errors = [];
ws.addEventListener('message', (m) => {
  const d = JSON.parse(m.data);
  if (d.id && pend.has(d.id)) {
    const { res, rej } = pend.get(d.id);
    pend.delete(d.id);
    if (d.error) rej(new Error(d.error.message)); else res(d.result);
  } else if (d.method === 'Runtime.exceptionThrown') {
    errors.push(d.params.exceptionDetails.text + ' ' + ((d.params.exceptionDetails.exception || {}).description || ''));
  }
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
async function t(name, fn) {
  try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; console.log('  FAIL ' + name + '\n       ' + e.message); }
}
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b)} 实得 ${JSON.stringify(a)}`); };
const modal = "document.querySelector('.cf-dialog')"; // 统一确认组件（web/static/js/confirm.js）

await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/` });
for (let i = 0; i < 60; i++) { if (await ev("return !!document.getElementById('act-cancel-run')").catch(() => false)) break; await sleep(100); }
await sleep(300);
console.log('工单详情页“取消执行”测试');

await t('没写原因：提示必填，不弹确认、不发请求', async () => {
  await ev("document.getElementById('act-cancel-run').click();");
  await sleep(300);
  eq(await ev(`return !!${modal};`), false, '不应弹确认');
  eq(calls.length, 0);
  if (!(await ev("return document.getElementById('detail-msg').textContent;")).includes('取消原因')) throw new Error('应提示填写取消原因');
});

await t('写了原因：先弹确认（说明后果），取消则不发请求', async () => {
  await ev("document.getElementById('cancel-reason').value = '执行太久，影响线上业务';");
  await ev("document.getElementById('act-cancel-run').click();");
  for (let i = 0; i < 30 && !(await ev(`return !!${modal};`)); i++) await sleep(100);
  const text = await ev(`return ${modal}.textContent;`);
  for (const k of ['取消执行', '不会回滚', '执行有异常', '执行太久，影响线上业务']) if (!text.includes(k)) throw new Error('确认框里应有：' + k);
  await ev("document.querySelector('.cf-foot .cf-btn:first-child').click();"); // 取消（不确认）
  await sleep(300);
  eq(calls.length, 0, '点“取消”不应发请求');
});

await t('确认后发请求，带取消原因', async () => {
  await ev("document.getElementById('act-cancel-run').click();");
  for (let i = 0; i < 30 && !(await ev(`return !!${modal};`)); i++) await sleep(100);
  await ev("document.querySelector('.cf-foot .cf-btn:last-child').click();"); // 确认取消执行
  for (let i = 0; i < 30 && !calls.length; i++) await sleep(100);
  eq(calls.length, 1);
  eq(calls[0].url, '/api/v1/sqlworkflow/7/cancel-run');
  eq(calls[0].body, { remark: '执行太久，影响线上业务' });
});

await t('控制台零报错', async () => { eq(errors.filter((e) => !/favicon/.test(e)), []); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
