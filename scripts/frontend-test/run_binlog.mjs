/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_binlog.mjs
 * @time: 2026/10/28
 */
// 日志解析（Binlog）页面级测试：自起假后端（接口形状按 internal/binlog）+ 无头 Chrome（CDP），读真实模板与静态文件，不依赖第三方包。
// 覆盖：上限显示、回滚分段的提示与“继续下一段”、段号随请求带上、下载走浏览器直接下载（不用 fetch + blob）。
// 用法：node scripts/frontend-test/run_binlog.mjs [chrome路径]    环境变量 BL_HTTP_PORT（默认 8931）/ BL_CDP_PORT（默认 9268）/ BL_SHOT（截图目录）。
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import http from 'node:http';
import { fileURLToPath } from 'node:url';
import { untranslate } from './tpl.mjs';
import { spawn, spawnSync } from 'node:child_process';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.join(here, '../..');
const staticRoot = path.join(root, 'web/static');
const HTTP = +(process.env.BL_HTTP_PORT || 8931);
const CDP = +(process.env.BL_CDP_PORT || 9268);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'";
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.json': 'application/json', '.svg': 'image/svg+xml' };

// ---- 假后端 ----
const LIMITS = { default_rows: 500, hard_rows: 500000, default_mb: 2, hard_mb: 1024, default_timeout: 120, max_timeout: 600, page_rows: 200, max_scan: 5000000, keep_minutes: 30 };
const INFO = {
  version: '8.0.36', flavor: 'mysql', log_bin: true, binlog_format: 'ROW', binlog_row_image: 'FULL', tz_offset_seconds: 0,
  files: [{ name: 'bin.000001', size: 1000 }, { name: 'bin.000002', size: 500 }], can_rollback: true,
};
const ROWS = [
  { seq: 1, trx: 2, time: '2026-10-19 10:00:00', file: 'bin.000001', pos: 400, end_pos: 500, schema: 'shop', table: 'orders', type: 'DELETE', sql: 'INSERT INTO `shop`.`orders` VALUES (2);' },
  { seq: 2, trx: 1, time: '2026-10-19 09:59:00', file: 'bin.000001', pos: 200, end_pos: 300, schema: 'shop', table: 'orders', type: 'INSERT', sql: 'DELETE FROM `shop`.`orders` WHERE `id` = 1;' },
];
const state = { jobs: {}, n: 0 };
function view(id, body) {
  const seg = body.segment || 1;
  const more = seg === 1; // 第 1 段后面还有，第 2 段是最后一段
  return {
    id, mode: body.mode, status: 'done', instance: 'mysql-a', owner: 'admin', mine: true, created_at: '2026-10-19 10:00:00', finished_at: '2026-10-19 10:00:01',
    expires_at: '2026-10-19 10:30:01', remain_seconds: 1799, keep_minutes: 30, range: `${body.start_file}:${body.start_pos || 4} → 当前末尾`, scanned: 12, matched: 2,
    file: 'bin.000001', pos: 530, bytes: 457, limits: { max_rows: body.max_rows, max_bytes: 2097152, max_scan: 5000000, timeout_sec: 120 },
    truncated: more ? '结果达到 2 条的上限，本段在事务边界结束，续接位置 bin.000001:389，后面还有。点“继续下一段”接着生成；回滚时要先执行最后一段，最后执行第 1 段。' : undefined, warnings: [], by_type: { INSERT: 1, DELETE: 1 }, trx: 2, can_download: true,
    segment: body.mode === 'rollback' ? (more ? { index: seg, more: true, next_file: 'bin.000001', next_pos: 389, reason: 'rows' } : { index: seg, more: false }) : undefined,
  };
}
const calls = [];
const ok = (data) => ({ code: 'ok', message: 'ok', data });

function readTpl(rel) { return untranslate(fs.readFileSync(path.join(root, 'web/templates', rel), 'utf8').replace(/\r\n/g, '\n')); }
function page() {
  const t = readTpl('binlog/index.html');
  const ci = t.indexOf('{{define "content"}}'), si = t.indexOf('{{define "scripts"}}');
  const content = t.slice(ci + '{{define "content"}}'.length, si).replace(/\n\{\{end\}\}\s*$/, '');
  const scripts = t.slice(si + '{{define "scripts"}}'.length).replace(/\{\{end\}\}\s*$/, '');
  const head = (t.match(/\{\{define "head"\}\}([\s\S]*?)\{\{end\}\}/) || [, ''])[1];
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="csrf-token" content="tok"><title>日志解析</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/shell.css">${head}</head><body><div class="app"><div class="main">${content}</div></div>${scripts}</body></html>`;
}

const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, 'http://x');
  const p = decodeURIComponent(u.pathname);
  let body = '';
  for await (const c of req) body += c;
  const json = (status, obj) => { res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(obj)); };
  let parsed = null;
  try { parsed = body ? JSON.parse(body) : null; } catch (_) { /* 非 JSON */ }
  if (p.startsWith('/api/')) calls.push({ method: req.method, url: p + u.search, body: parsed });
  if (p === '/favicon.ico') { res.writeHead(204); res.end(); return; }
  if (p === '/binlog') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' }); res.end(page()); return; }
  if (p.startsWith('/static/')) {
    const f = path.join(staticRoot, p.slice(8));
    if (f.startsWith(staticRoot) && fs.existsSync(f) && fs.statSync(f).isFile()) {
      res.writeHead(200, { 'Content-Type': MIME[path.extname(f)] || 'application/octet-stream', 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' });
      fs.createReadStream(f).pipe(res);
      return;
    }
    res.writeHead(404); res.end('nf'); return;
  }
  if (p === '/api/v1/button-perms/me') return json(200, ok({ mode: 'compat', deny_display: 'hide', superuser: true, allowed: [] }));
  if (p === '/api/v1/binlog/options') return json(200, ok({ instances: [{ id: 1, name: 'mysql-a', role: '主库' }], perms: { parse: true, rollback: true, download: true }, limits: LIMITS }));
  if (p === '/api/v1/binlog/instances/1/info') return json(200, ok(INFO));
  if (p === '/api/v1/binlog/instances/1/databases') return json(200, ok(['shop']));
  if (p === '/api/v1/binlog/jobs' && req.method === 'GET') return json(200, ok([]));
  if (p === '/api/v1/binlog/parse' && req.method === 'POST') {
    const id = 'job' + (++state.n);
    state.jobs[id] = parsed;
    return json(200, ok({ ...view(id, parsed), status: 'queued', can_download: false, segment: undefined, truncated: undefined }));
  }
  let m = /^\/api\/v1\/binlog\/jobs\/(\w+)$/.exec(p);
  if (m && state.jobs[m[1]]) return json(200, ok(view(m[1], state.jobs[m[1]])));
  m = /^\/api\/v1\/binlog\/jobs\/(\w+)\/rows$/.exec(p);
  if (m) return json(200, ok({ total: ROWS.length, offset: 0, rows: ROWS }));
  m = /^\/api\/v1\/binlog\/jobs\/(\w+)\/download$/.exec(p);
  if (m) {
    res.writeHead(200, { 'Content-Type': 'text/plain; charset=utf-8', 'Content-Disposition': `attachment; filename="binlog_1_rollback_${m[1]}.sql"` });
    res.end('-- ok\n');
    return;
  }
  res.writeHead(404); res.end('nf');
}).listen(HTTP);

// ---- Chrome ----
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'bl-chrome-'));
const dlDir = fs.mkdtempSync(path.join(os.tmpdir(), 'bl-dl-'));
const chrome = spawn(chromePath, ['--headless=new', `--remote-debugging-port=${CDP}`, `--user-data-dir=${profile}`, '--no-first-run', '--disable-gpu', 'about:blank'], { stdio: 'ignore' });
let cleaned = false;
function cleanup() {
  if (cleaned) return;
  cleaned = true;
  try { if (process.platform === 'win32') spawnSync('taskkill', ['/PID', String(chrome.pid), '/T', '/F'], { stdio: 'ignore' }); else chrome.kill(); } catch (_) { /* 已退出 */ }
  server.close();
  setTimeout(() => { for (const d of [profile, dlDir]) { try { fs.rmSync(d, { recursive: true, force: true }); } catch (_) { /* 占用中 */ } } }, 1500).unref();
}
process.on('exit', cleanup);

async function getWs() {
  for (let i = 0; i < 80; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${CDP}/json`)).json();
      const pg = list.find((x) => x.type === 'page');
      if (pg) return pg.webSocketDebuggerUrl;
    } catch (_) { /* 等 Chrome 起来 */ }
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error('Chrome 未启动');
}
const ws = new WebSocket(await getWs());
await new Promise((r) => ws.addEventListener('open', r));
let seq = 0;
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
const send = (method, params = {}) => new Promise((res, rej) => { const i = ++seq; pend.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
await send('Runtime.enable'); await send('Page.enable'); await send('Log.enable');
await send('Emulation.setDeviceMetricsOverride', { width: 1366, height: 1400, deviceScaleFactor: 1, mobile: false });
await send('Page.setDownloadBehavior', { behavior: 'allow', downloadPath: dlDir }).catch(() => { /* 旧版本没有这条命令：下载请求照样会发出 */ });

async function ev(expr) {
  const r = await send('Runtime.evaluate', { expression: `(async()=>{${expr}})()`, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error('页面异常：' + ((r.exceptionDetails.exception || {}).description || r.exceptionDetails.text));
  return r.result.value;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function shot(name) {
  if (!process.env.BL_SHOT) return;
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.mkdirSync(process.env.BL_SHOT, { recursive: true });
  fs.writeFileSync(path.join(process.env.BL_SHOT, name + '.png'), Buffer.from(r.data, 'base64'));
}
async function waitFor(expr, what, ms = 6000) {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) {
    if (await ev(`return !!(${expr});`).catch(() => false)) return;
    await sleep(60);
  }
  throw new Error(`等待超时：${what}\n  请求：${calls.map((c) => c.method + ' ' + c.url).join(', ')}\n  控制台：${JSON.stringify(consoleErrors.slice(0, 3))}`);
}
let pass = 0, fail = 0;
async function t(name, fn) {
  try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; console.log('  FAIL ' + name + '\n       ' + e.message); }
}
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b)} 实得 ${JSON.stringify(a)}`); };
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };
const has = (s, sub, m) => { if (!String(s).includes(sub)) throw new Error(`${m || ''} 应包含 ${JSON.stringify(sub)}，实得 ${JSON.stringify(String(s).slice(0, 300))}`); };
const q = (sel, prop = 'textContent') => ev(`const e=document.querySelector(${JSON.stringify(sel)}); return e ? e.${prop} : null;`);
const clickText = (sel, text) => ev(`const b=[...document.querySelectorAll(${JSON.stringify(sel)})].find(x=>x.textContent.trim()===${JSON.stringify(text)}); if(!b) throw new Error('没有按钮 '+${JSON.stringify(text)}); b.click();`);
const lastParse = () => [...calls].reverse().find((c) => c.method === 'POST' && c.url === '/api/v1/binlog/parse');

// 起一次回滚任务：选实例 → 回滚模式 → 开始 → 确认；等任务结果出来
async function runRollback() {
  await ev(`document.getElementById('bl-mode-rb').click();`);
  await ev(`document.getElementById('bl-start').click();`);
  await waitFor(`[...document.querySelectorAll('.modal button')].some(b=>b.textContent.trim()==='确认生成')`, '回滚确认框');
  await clickText('.modal button', '确认生成');
  await waitFor(`document.querySelector('#bl-job-notes .alert')`, '任务结果');
}

console.log('日志解析页面测试');
await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/binlog` });
await waitFor(`document.getElementById('bl-inst') && document.getElementById('bl-inst').options.length > 1`, '实例列表');
await ev(`const s=document.getElementById('bl-inst'); s.value='1'; s.dispatchEvent(new Event('change',{bubbles:true}));`);
await waitFor(`document.getElementById('bl-sfile').options.length === 2`, '文件列表');

await t('页面上的“最多条数（≤ N）”“结果体积上限（≤ N）”显示接口给的当前上限', async () => {
  eq(await q('#bl-hard-rows'), '500,000');
  eq(await q('#bl-hard-mb'), '1,024');
  eq(await q('#bl-max-rows', 'value'), '500');
  eq(await q('#bl-max-mb', 'value'), '2');
});

await t('回滚任务到上限：结果下面写明第几段、续接位置和执行顺序，并给“继续下一段”按钮', async () => {
  await runRollback();
  await shot('segment-1');
  const text = await q('#bl-job-notes');
  has(text, '第 1 段');
  has(text, '续接位置 bin.000001:389');
  has(text, '先执行最后一段，最后执行第 1 段');
  const btns = await ev(`return [...document.querySelectorAll('#bl-job-notes button')].map(b=>b.textContent.trim());`);
  eq(btns, ['继续下一段']);
  eq(lastParse().body.segment, 1, '第一个任务是第 1 段');
});

await t('点“继续下一段”：续接位置填进起始位置，其它条件保留，并提示点哪个按钮', async () => {
  await ev(`document.getElementById('bl-max-rows').value = '777';`);
  await clickText('#bl-job-notes button', '继续下一段');
  eq(await q('#bl-sfile', 'value'), 'bin.000001');
  eq(await q('#bl-spos', 'value'), '389');
  eq(await q('#bl-max-rows', 'value'), '777', '其它条件不变');
  const msg = await q('#bl-msg');
  has(msg, 'bin.000001:389');
  has(msg, '第 2 段');
});

await t('接着开始：请求带上 start_pos=389 和 segment=2；最后一段没有“继续下一段”', async () => {
  await ev(`document.getElementById('bl-start').click();`);
  await waitFor(`[...document.querySelectorAll('.modal button')].some(b=>b.textContent.trim()==='确认生成')`, '回滚确认框');
  await clickText('.modal button', '确认生成');
  await waitFor(`document.getElementById('bl-job-meta').textContent.includes('bin.000001:389') && document.querySelector('#bl-job-notes').textContent.includes('这是最后一段')`, '第 2 个任务的结果（最后一段）');
  const body = lastParse().body;
  eq([body.start_file, body.start_pos, body.segment, body.max_rows], ['bin.000001', 389, 2, 777]);
  const btns = await ev(`return [...document.querySelectorAll('#bl-job-notes button')].map(b=>b.textContent.trim());`);
  eq(btns, []);
  has(await q('#bl-job-notes'), '这是最后一段');
  await shot('segment-2');
});

await t('改了起始位置就从第 1 段重新算', async () => {
  await ev(`document.getElementById('bl-spos').value = '500';`);
  await ev(`document.getElementById('bl-start').click();`);
  await waitFor(`[...document.querySelectorAll('.modal button')].some(b=>b.textContent.trim()==='确认生成')`, '回滚确认框');
  await clickText('.modal button', '确认生成');
  await waitFor(`document.getElementById('bl-job-meta').textContent.includes('bin.000001:500')`, '新任务的范围');
  eq(lastParse().body.segment, 1);
  eq(lastParse().body.start_pos, 500);
});

await t('还原模式不带段号：起始位置即使恰好是续接位置，也只有回滚模式才算下一段', async () => {
  await ev(`document.getElementById('bl-spos').value = '389';`); // 和“继续下一段”填的位置相同
  await ev(`document.querySelector('input[name="bl-mode"][value="sql"]').click();`);
  await ev(`document.getElementById('bl-start').click();`); // 还原模式不需要确认
  await waitFor(`document.getElementById('bl-job-meta').textContent.includes('bin.000001:389')`, '新任务的范围');
  eq(lastParse().body.mode, 'sql');
  eq(lastParse().body.segment, 1);
});

await t('下载：先确认任务还在，再让浏览器直接下载（不用 fetch + blob）', async () => {
  // 切回最后一个回滚任务的结果再下载
  const before = calls.length;
  await ev(`document.getElementById('bl-download').disabled = false;`);
  await ev(`document.getElementById('bl-download').click();`);
  await waitFor(`[...document.querySelectorAll('.modal button')].some(b=>b.textContent.trim()==='确认下载')`, '下载确认框');
  await clickText('.modal button', '确认下载');
  const t0 = Date.now();
  let got = null;
  while (Date.now() - t0 < 5000 && !got) {
    got = calls.slice(before).find((c) => /\/download$/.test(c.url));
    await sleep(80);
  }
  truthy(got, '应有对 /download 的请求');
  eq(got.method, 'GET');
  const pre = calls.slice(before).findIndex((c) => /\/jobs\/job\d+$/.test(c.url));
  truthy(pre >= 0 && pre < calls.slice(before).indexOf(got), '下载前先查了任务状态');
});

await t('控制台零报错（模板里两处内联 style="max-height:none" 被 CSP 拦截是已有的告警，不在这里管）', async () => {
  eq(consoleErrors.filter((e) => !/favicon/.test(e) && !/Applying inline style violates/.test(e)), []);
});

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
