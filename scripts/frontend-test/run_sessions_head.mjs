/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_sessions_head.mjs
 * @time: 2026/10/29
 */
// 会话诊断页顶部的浏览器回归：
//   右上角“实例”选择框的右边缘和卡片里统计块（阻塞源）的右边缘对齐；
//   实例下拉（输入即筛选）展开后，选项在窗口右侧也不会被切掉，长名字整条看得到，悬停有全名。
// 真实模板（去掉 Go 模板指令）+ 真实静态文件 + 假接口 + 无头 Chrome（CDP）。
// 用法：node scripts/frontend-test/run_sessions_head.mjs [chrome路径]    环境变量 SH_HTTP_PORT（默认 8951）/ SH_CDP_PORT（默认 9291）。
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { untranslate } from './tpl.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const staticRoot = path.resolve(here, '../../web/static');
const tplRoot = path.resolve(here, '../../web/templates');
const HTTP = +(process.env.SH_HTTP_PORT || 8951);
const CDP = +(process.env.SH_CDP_PORT || 9291);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';

const LONG = 'prod-mall-order-center-cluster-reader-01.mysql.polardb.singapore.rds.aliyuncs.com';
const CHOICES = [
  { id: 1, instance_name: 'mall_order', db_type_label: 'MySQL', supported: true },
  { id: 2, instance_name: LONG, db_type_label: 'MySQL', supported: true },
  { id: 3, instance_name: 'pg-report', db_type_label: 'PostgreSQL', supported: false },
];

function page() {
  const t = untranslate(fs.readFileSync(path.join(tplRoot, 'instmgr/sessions.html'), 'utf8').replace(/\r\n/g, '\n'));
  const ci = t.indexOf('{{define "content"}}');
  const si = t.indexOf('{{define "scripts"}}');
  const content = t.slice(ci + '{{define "content"}}'.length, si)
    .replace(/\{\{template "instmgr_side" \.Data\}\}/g, '')
    .replace(/\n\{\{end\}\}\s*$/, '').replace(/\{\{end\}\}\s*$/, '');
  const scripts = t.slice(si + '{{define "scripts"}}'.length).replace(/\{\{end\}\}\s*$/, '');
  return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="csrf-token" content="tok"><link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css"><link rel="stylesheet" href="/static/css/instmgr.css"></head><body><div class="content">${content}</div>${scripts}</body></html>`;
}

const server = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x');
  const json = (obj) => { res.writeHead(200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(obj)); };
  if (u.pathname === '/instances/sessions') { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(page()); return; }
  if (u.pathname === '/api/v1/instmgr/caps') return json({ data: { 'session.process': true, 'session.kill': true, 'session.tablespace': true, 'session.trx': true, 'session.locks': true } });
  if (u.pathname === '/api/v1/instmgr/choices') return json({ data: CHOICES });
  if (/^\/api\/v1\/instances\/\d+\/processlist$/.test(u.pathname)) return json({ data: { stats: { total: 8, active: 2, long: 0, blockers: 0 }, rows: [{ id: 1, user: 'root', host: 'h', db: 'd', time: 5, command: 'Sleep', state: '', info: '', severity: '' }] } });
  const f = path.join(staticRoot, u.pathname.replace(/^\/static\//, ''));
  if (u.pathname.startsWith('/static/') && f.startsWith(staticRoot) && fs.existsSync(f) && fs.statSync(f).isFile()) {
    res.writeHead(200, { 'Content-Type': f.endsWith('.js') ? 'text/javascript' : f.endsWith('.css') ? 'text/css' : 'application/octet-stream' });
    res.end(fs.readFileSync(f));
    return;
  }
  if (u.pathname.startsWith('/api/')) return json({ data: null });
  res.writeHead(404);
  res.end('nf');
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'sh-chrome-'));
const chrome = spawn(chromePath, ['--headless=new', `--remote-debugging-port=${CDP}`, `--user-data-dir=${profile}`, '--no-first-run', '--disable-gpu', '--window-size=1280,800', 'about:blank'], { stdio: 'ignore' });
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
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };

async function open(width) {
  await send('Emulation.setDeviceMetricsOverride', { width, height: 800, deviceScaleFactor: 1, mobile: false });
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/instances/sessions` });
  for (let i = 0; i < 80; i++) {
    if (await ev("return document.querySelectorAll('#p-stats > div').length === 4 && !!document.querySelector('#ss .ss-input')").catch(() => false)) break;
    await sleep(100);
  }
  await sleep(300);
}

for (const width of [1366, 1000]) {
  await open(width);
  await t(`${width} 宽：实例选择框右边缘 = 阻塞源统计块右边缘`, async () => {
    const r = await ev("const rr = (s) => document.querySelector(s).getBoundingClientRect().right; const kp = document.querySelectorAll('#p-stats > div'); return { input: rr('#ss .ss-input'), blocker: kp[kp.length - 1].getBoundingClientRect().right };");
    truthy(Math.abs(r.input - r.blocker) <= 1, `输入框右边 ${r.input}，阻塞源右边 ${r.blocker}`);
  });
  await t(`${width} 宽：页面没有横向滚动条`, async () => {
    eq(await ev('return document.documentElement.scrollWidth <= document.documentElement.clientWidth'), true);
  });
}

await open(1366);
await t('下拉展开后，最长的实例全名完整落在窗口内，选项带全名悬停提示', async () => {
  await ev("document.querySelector('#ss .ss-input').click(); await new Promise((r) => setTimeout(r, 150));");
  const r = await ev("const l = document.querySelector('.ss-list'); const b = l.getBoundingClientRect(); const o = [...l.querySelectorAll('.ss-opt')]; const lo = o.find((x) => x.textContent.includes('prod-mall-order')) || { scrollWidth: 0, clientWidth: 0, title: 'none:' + o.map((x) => x.textContent).join('|'), textContent: '' }; return { hidden: l.hidden, left: b.left, right: b.right, vw: innerWidth, n: o.length, clipped: lo.scrollWidth > lo.clientWidth, title: lo.title, text: lo.textContent };");
  eq(r.hidden, false);
  truthy(r.left >= 0 && r.right <= r.vw, `列表超出窗口：${r.left} ~ ${r.right} / ${r.vw}`);
  eq(r.clipped, false, '长名字被省略号截断');
  eq(r.title, r.text, '悬停提示是全名');
});
await t('输入关键字能筛到长名字的实例并选中', async () => {
  await ev("const i = document.querySelector('#ss .ss-input'); i.value = 'reader-01'; i.dispatchEvent(new Event('input', { bubbles: true })); await new Promise((r) => setTimeout(r, 100)); document.querySelector('.ss-opt').dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true })); await new Promise((r) => setTimeout(r, 300));");
  eq(await ev("return document.getElementById('ss-inst').value"), '2');
});
await t('页面无脚本异常', async () => { eq(errors, []); });

console.log(`\n${pass} 通过，${fail} 失败`);
cleanup();
process.exit(fail ? 1 : 0);
