/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_watermark.mjs
 * @time: 2026/10/28
 */
// 水印层被删除 / 隐藏 / 改样式后恢复并上报：自起静态服务 + 假后端 + 无头 Chrome（CDP），不依赖第三方包。
// 用法：node scripts/frontend-test/run_watermark.mjs [chrome路径]   环境变量 WM_HTTP_PORT / WM_CDP_PORT 可改端口。
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const staticRoot = path.resolve(here, '../../web/static');
const HTTP = +(process.env.WM_HTTP_PORT || 8900);
const CDP = +(process.env.WM_CDP_PORT || 9237);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'";
const MIME = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8' };
const PAGE = '<!doctype html><meta charset="utf-8"><meta name="csrf-token" content="tok-1"><title>wm</title><body><p>页面</p><script type="module" src="/static/js/auth/watermark.js"></script>';

const posts = []; // 假后端收到的上报
const server = http.createServer((req, res) => {
  const u = decodeURIComponent(req.url.split('?')[0]);
  const json = (code, o) => { res.writeHead(code, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(o)); };
  if (u === '/t/wm.html') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP }); res.end(PAGE); return; }
  if (u === '/api/v1/auth/ui') return json(200, { data: { watermark: true, watermark_text: '张三（zhangsan）{时间}', watermark_extra: '', watermark_opacity: 14, watermark_font_size: 14, watermark_angle: -22, watermark_gap: 'normal', watermark_color: 'auto', watermark_vars: {} } });
  if (u === '/api/v1/auth/watermark-tamper' && req.method === 'POST') {
    let b = '';
    req.on('data', (c) => { b += c; });
    req.on('end', () => { posts.push({ body: JSON.parse(b || 'null'), csrf: req.headers['x-csrf-token'] }); json(200, { data: { ok: true } }); });
    return;
  }
  if (u.startsWith('/static/')) {
    const file = path.join(staticRoot, u.slice(8));
    if (file.startsWith(staticRoot) && fs.existsSync(file)) { res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream', 'Cache-Control': 'no-store' }); fs.createReadStream(file).pipe(res); return; }
  }
  res.writeHead(404); res.end('nf');
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));

const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'wm-chrome-'));
const chrome = spawn(chromePath, ['--headless=new', `--remote-debugging-port=${CDP}`, `--user-data-dir=${profile}`, '--no-first-run', '--disable-gpu', 'about:blank'], { stdio: 'ignore' });
const cleanup = () => {
  try { chrome.kill(); } catch (_) { /* 已退出 */ }
  server.close();
  setTimeout(() => { try { fs.rmSync(profile, { recursive: true, force: true }); } catch (_) { /* 占用中则留给系统清理 */ } }, 1500).unref();
};
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
  if (d.id && pend.has(d.id)) { const { res, rej } = pend.get(d.id); pend.delete(d.id); if (d.error) rej(new Error(d.error.message)); else res(d.result); }
  else if (d.method === 'Runtime.exceptionThrown') consoleErrors.push(d.params.exceptionDetails.text + ' ' + ((d.params.exceptionDetails.exception || {}).description || ''));
  else if (d.method === 'Log.entryAdded' && d.params.entry.level === 'error') consoleErrors.push(d.params.entry.text + ' ' + (d.params.entry.url || ''));
});
const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id; pend.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
await send('Runtime.enable'); await send('Page.enable'); await send('Log.enable');
async function ev(expr) {
  const r = await send('Runtime.evaluate', { expression: `(async()=>{${expr}})()`, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error('页面异常：' + ((r.exceptionDetails.exception || {}).description || r.exceptionDetails.text));
  return r.result.value;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let pass = 0, fail = 0;
async function t(name, fn) { try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; console.log('  FAIL ' + name + '\n       ' + e.message); } }
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b)} 实得 ${JSON.stringify(a)}`); };
const waitFor = async (cond, ms = 4000) => { for (let i = 0; i < ms / 100; i++) { if (await cond()) return true; await sleep(100); } return false; };

await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/t/wm.html` });
console.log(`水印篡改恢复与上报（http://127.0.0.1:${HTTP}/t/wm.html）`);
await t('水印层出现', async () => { eq(await waitFor(() => ev("return !!document.getElementById('dbt-watermark')")), true); });
const layerState = () => ev("const l=document.getElementById('dbt-watermark'); return l ? {display:l.style.display, hidden:l.hidden, fixed:l.style.position, bg:!!l.style.backgroundImage} : null");

await t('隐藏（display:none）立刻恢复；随后删除也恢复', async () => {
  await ev("document.getElementById('dbt-watermark').style.display='none'");
  eq(await waitFor(async () => { const s = await layerState(); return s && s.display === 'block'; }), true, '应恢复 display:block');
  // 紧接着再删除：同一防抖窗口，合并
  await ev("document.getElementById('dbt-watermark').remove()");
  eq(await waitFor(async () => !!(await layerState())), true, '被删后应重建');
  eq((await layerState()).bg, true, '重建后带平铺背景');
});
await t('防抖后只上报一次，kind 是最先发现的类型，带 CSRF，body 只有 kind', async () => {
  eq(await waitFor(() => posts.length >= 1, 5000), true, '应上报');
  await sleep(1500);
  eq(posts.length, 1, '合并为一次');
  eq(posts[0], { body: { kind: 'hidden' }, csrf: 'tok-1' });
});
await t('改样式（pointer-events）恢复', async () => {
  await ev("document.getElementById('dbt-watermark').style.pointerEvents='auto'");
  eq(await waitFor(async () => (await ev("return document.getElementById('dbt-watermark').style.pointerEvents")) === 'none'), true);
});
await t('页面里新增无关节点不触发上报', async () => {
  const n = posts.length;
  await ev("const d=document.createElement('div'); d.id='x1'; document.body.append(d);");
  await sleep(1500);
  eq(posts.length, n);
  eq((await layerState()).display, 'block');
});
await t('间隔内再次篡改：恢复，但上报被推迟（不会连发）', async () => {
  const n = posts.length;
  await ev("document.getElementById('dbt-watermark').remove()");
  eq(await waitFor(async () => !!(await layerState())), true, '重建');
  await sleep(1500);
  eq(posts.length, n, '30 秒间隔内不再发');
});
await t('严格 CSP 下无违规 / 页面无异常', async () => { eq(consoleErrors.filter((e) => !e.includes('favicon.ico')), []); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
ws.close();
cleanup();
process.exit(fail ? 1 : 0);
