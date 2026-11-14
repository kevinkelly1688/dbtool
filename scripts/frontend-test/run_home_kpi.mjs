/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_home_kpi.mjs
 * @time: 2026/10/28
 */
// 首页“我可见的实例”卡片（按每个实例最近一次巡检统计）与巡检列表的 ?latest=1&level= 跳转的回归测试：
// 静态服务 + 假后端 + 无头 Chrome（CDP），不依赖第三方包。首页只摆出 home.js 用到的真实标记（卡片、实例健康列表），
// 巡检列表页取自真实模板 web/templates/inspect/list.html。
// 用法：node scripts/frontend-test/run_home_kpi.mjs [chrome路径]
// 环境变量：HK_HTTP_PORT（默认 8937）、HK_CDP_PORT（默认 9280）、HK_SHOT_DIR（给一个目录就保存首页截图）。
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
const HTTP = +(process.env.HK_HTTP_PORT || 8937);
const CDP = +(process.env.HK_CDP_PORT || 9280);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const base = `http://127.0.0.1:${HTTP}`;
// 首页用比应用更严的 CSP（style-src 去掉 unsafe-inline，以便发现内联样式）；巡检列表的真实模板里本来就有 style 属性，用应用实际的 CSP
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'";
const APP_CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'";
const MIME = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8', '.md': 'text/plain; charset=utf-8' };
const J = JSON.stringify;

// ---------------------------------------------------------------- 假后端
const TOTAL = 118; // 可见实例数
const state = { scn: 'alerts', reqs: [] };

// 各场景下“每个实例最近一次巡检”（服务端已按 latest=1 取好）：前 100 个实例有巡检数据，后 18 个从没巡检过（okfull 例外）
function reportsFor(scn) {
  const mk = (i, over) => ({ kind: 'mysql', id: 1000 + i, instance_id: i, instance_name: 'inst-' + i, status: 'finish', alert: 0, attention: 0, failed: 0, health_score: 90, start_time: '2026-10-23T03:00:00+08:00', ...over });
  const n = scn === 'okfull' ? TOTAL : 100;
  const out = [];
  for (let i = 1; i <= n; i++) {
    let over = {};
    if (scn === 'alerts') {
      if (i <= 10) over = { alert: 3 };
      else if (i === 11 || i === 12) over = { status: 'failed', health_score: null };
    } else if (scn === 'failedonly' && i <= 3) over = { status: 'failed', health_score: null };
    out.push(mk(i, over));
  }
  return scn === 'none' ? [] : out;
}

const ids = Array.from({ length: TOTAL }, (_, i) => i + 1);

function homeHtml() {
  // 标记与 web/templates/home.html 一致：卡片 <a class="hk" id="k-inst">，实例健康 <section id="home-insts" data-ids>，列表只放前 9 个
  const items = ids.slice(0, 9).map((i) => `<li data-inst="${i}"><span class="dot unknown" aria-hidden="true"></span><span class="in-name">inst-${i}</span><span class="in-meta muted">mysql · 主</span><span class="in-st muted small" data-role="st"></span></li>`).join('');
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="csrf-token" content="t"><title>首页</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/shell.css"><link rel="stylesheet" href="/static/css/home.css"></head>
<body><div class="app"><div class="main"><main class="page" id="home">
<div class="home-kpis" id="home-kpis">
  <a class="hk" id="k-inst" href="/instances"><span class="hk-l">我可见的实例</span><span class="hk-v" data-role="v">${TOTAL}</span><span class="hk-d" data-role="d"></span></a>
</div>
<section class="card" id="home-insts" data-ids="${ids.join(',')}">
  <h2>实例健康 <span class="muted small">${TOTAL} 个</span><span class="spacer"></span><a class="small" href="/inspection" id="insp-link" hidden>性能巡检 →</a></h2>
  <ul class="inst-list" id="inst-list">${items}</ul>
</section>
</main></div></div><script type="module" src="/static/js/home.js"></script></body></html>`;
}

function inspectHtml() {
  const src = untranslate(fs.readFileSync(path.join(root, 'web/templates/inspect/list.html'), 'utf8'));
  const head = (src.match(/\{\{define "head"\}\}(.*?)\{\{end\}\}/s) || [, ''])[1];
  const i = src.indexOf('{{define "content"}}');
  const rest = src.slice(i + '{{define "content"}}'.length);
  let content = rest.slice(0, rest.indexOf('{{end}}\n\n'));
  content = content.replace(/\{\{if [^}]*\}\}.*?\{\{end\}\}/gs, '').replace(/\{\{[^}]*\}\}/g, '');
  const scripts = (src.match(/\{\{define "scripts"\}\}(.*?)\{\{end\}\}/s) || [, ''])[1];
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="csrf-token" content="t"><title>性能巡检</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${head}</head>
<body><div class="app"><div class="main">${content}</div></div>${scripts}</body></html>`;
}

const json = (res, code, body) => { res.writeHead(code, { 'Content-Type': 'application/json' }); res.end(J(body)); };

function api(req, res, url) {
  const p = url.pathname.replace('/api/v1', '');
  state.reqs.push(url.pathname + url.search);
  if (p === '/inspection/reports') {
    if (state.scn === 'forbidden') return json(res, 403, { code: 'forbidden', message: '没有权限' });
    if (url.searchParams.get('latest') === '1' && url.searchParams.get('limit') === '2000') return json(res, 200, { data: reportsFor(state.scn), total: reportsFor(state.scn).length });
    return json(res, 200, { data: [], total: 0 }); // 列表页的请求：只关心请求参数
  }
  if (p === '/inspection/options') return json(res, 200, { data: { instances: [{ id: 1, name: 'inst-1', label: 'MySQL' }], can_run: true, can_delete: true, can_cancel: true, can_plan: true, plans_ready: true, channels: [], tags: [] } });
  if (p === '/inspection/plans') return json(res, 200, { data: [] });
  json(res, 404, { code: 'not_found', message: p });
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  try {
    if (url.pathname.startsWith('/api/v1/')) return api(req, res, url);
    if (url.pathname === '/__set') { state.scn = url.searchParams.get('scn') || 'alerts'; state.reqs.length = 0; return json(res, 200, {}); }
    if (url.pathname === '/__reqs') return json(res, 200, state.reqs);
    if (url.pathname === '/') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP }); return res.end(homeHtml()); }
    if (url.pathname === '/inspection') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': APP_CSP }); return res.end(inspectHtml()); }
    if (url.pathname.startsWith('/static/')) {
      const file = path.join(staticRoot, decodeURIComponent(url.pathname.slice(8)));
      if (file.startsWith(staticRoot) && fs.existsSync(file)) {
        res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream', 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' });
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
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'hk-chrome-'));
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
const errors = []; // 控制台错误 / 未捕获异常 / CSP 违规
ws.addEventListener('message', (m) => {
  const d = JSON.parse(m.data);
  if (d.id && pend.has(d.id)) {
    const { res, rej } = pend.get(d.id);
    pend.delete(d.id);
    if (d.error) rej(new Error(d.error.message)); else res(d.result);
  } else if (d.method === 'Runtime.exceptionThrown') {
    errors.push('exception: ' + d.params.exceptionDetails.text + ' ' + ((d.params.exceptionDetails.exception || {}).description || ''));
  } else if (d.method === 'Log.entryAdded' && d.params.entry.level === 'error') {
    // 假后端对 403 的响应会让浏览器记一条 "Failed to load resource"，这是 forbidden 场景预期内的
    if (!/Failed to load resource/.test(d.params.entry.text)) errors.push('log: ' + d.params.entry.text + ' ' + (d.params.entry.url || ''));
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
async function until(fn, what, ms = 5000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try { if (await fn()) return; } catch (_) { /* 继续等 */ }
    await sleep(60);
  }
  throw new Error('等待超时：' + what);
}
const reqs = async () => (await fetch(`${base}/__reqs`)).json();

// 打开首页并等巡检请求结束（forbidden 时没有变化，等请求出现即可）
async function openHome(scn) {
  await fetch(`${base}/__set?scn=${scn}`);
  await send('Emulation.setDeviceMetricsOverride', { width: 1300, height: 800, deviceScaleFactor: 1, mobile: false });
  await send('Page.navigate', { url: `${base}/` });
  await until(async () => (await reqs()).some((r) => r.startsWith('/api/v1/inspection/reports')), '巡检请求');
  await sleep(250);
}
const card = () => ev(`const k = document.getElementById('k-inst'); return { v: k.querySelector('[data-role="v"]').textContent, d: k.querySelector('[data-role="d"]').textContent, href: k.getAttribute('href'), warn: k.classList.contains('warn'), title: k.title };`);
const shot = async (name) => {
  if (!process.env.HK_SHOT_DIR) return;
  fs.mkdirSync(process.env.HK_SHOT_DIR, { recursive: true });
  const s = await send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(path.join(process.env.HK_SHOT_DIR, name + '.png'), Buffer.from(s.data, 'base64'));
};

console.log(`首页“我可见的实例”卡片与巡检列表跳转（${base}）`);

await openHome('alerts');
await t('向巡检接口要“每个实例最近一次”（latest=1），条数上限放到 2000，不再是被截成 20 的 limit=200', async () => {
  eq((await reqs()).filter((r) => r.startsWith('/api/v1/inspection/reports')), ['/api/v1/inspection/reports?latest=1&limit=2000']);
});
await t('10 个实例有告警、2 个巡检失败：写清是“巡检”告警 / 失败，数字只统计有巡检数据的实例，点击去巡检列表看有告警的', async () => {
  const c = await card();
  eq(c.v, String(TOTAL));
  eq(c.d, '10 个有巡检告警，2 个巡检失败');
  eq(c.warn, true);
  eq(c.href, '/inspection?latest=1&level=alert');
  eq(c.title, '按每个实例最近一次巡检统计：100 / 118 个实例有巡检数据');
  await shot('home-alerts');
});
await t('实例健康列表里的点仍按最近一次巡检着色', async () => {
  eq(await ev(`return document.querySelector('li[data-inst="1"] .dot').className`), 'dot bad');
  eq(await ev(`return document.querySelector('li[data-inst="1"] [data-role="st"]').textContent`), '告警 3 项');
  eq(await ev(`return document.querySelector('li[data-inst="9"] .dot').className`), 'dot bad');
  eq(await ev(`return document.getElementById('insp-link').hidden`), false);
});

await openHome('failedonly');
await t('只有巡检失败：写“巡检失败”，链接带 level=failed', async () => {
  const c = await card();
  eq([c.d, c.warn, c.href], ['3 个实例巡检失败', true, '/inspection?latest=1&level=failed']);
});

await openHome('okpartial');
await t('已巡检的都正常、但有实例没有巡检数据：不说“全部正常”，链接仍是实例列表', async () => {
  const c = await card();
  eq([c.d, c.warn, c.href], ['已巡检的 100 个全部正常', false, '/instances']);
});
// 上面 okpartial 场景在假后端里等同 100 个都没告警：用 okfull 场景再看“全部有数据”
await openHome('okfull');
await t('全部实例都有巡检数据且没有告警：巡检全部正常', async () => {
  const c = await card();
  eq([c.d, c.warn, c.href, c.title], ['巡检全部正常', false, '/instances', '按每个实例最近一次巡检统计：118 / 118 个实例有巡检数据']);
  await shot('home-ok');
});

await openHome('none');
await t('没有任何巡检数据：尚无巡检数据', async () => {
  const c = await card();
  eq([c.d, c.warn, c.href], ['尚无巡检数据', false, '/instances']);
});

await openHome('forbidden');
await t('没有巡检权限（接口 403）：卡片保持原样，不报错', async () => {
  const c = await card();
  eq([c.v, c.d, c.href, c.warn], [String(TOTAL), '', '/instances', false]);
  eq(await ev(`return document.getElementById('insp-link').hidden`), true);
});

// ---------------- 巡检列表：从首页卡片带参数跳过来 ----------------
async function openInspect(query) {
  await fetch(`${base}/__set?scn=none`);
  await send('Page.navigate', { url: `${base}/inspection${query}` });
  await until(async () => (await reqs()).some((r) => r.startsWith('/api/v1/inspection/reports')), '列表请求');
  await sleep(200);
}
await openInspect('?latest=1&level=alert');
await t('带 latest=1&level=alert 打开：级别筛选选中“有告警”，请求带这两个参数，页面上提示只看每个实例最近一次', async () => {
  eq(await ev(`return document.getElementById('f-level').value`), 'alert');
  const r = (await reqs()).filter((x) => x.startsWith('/api/v1/inspection/reports'))[0];
  const q = new URL(r, 'http://x').searchParams;
  eq([q.get('latest'), q.get('level'), q.get('limit')], ['1', 'alert', '20']);
  const box = await ev(`const b = document.querySelector('#msg .alert.info'); return b ? { text: b.textContent, link: b.querySelector('a').getAttribute('href') } : null`);
  eq(box, { text: '仅显示每个实例最近一次巡检（成功或失败）的结果。 查看全部报告', link: '/inspection' });
});
await t('换级别筛选后仍只看每个实例最近一次', async () => {
  await ev(`const s = document.getElementById('f-level'); s.value = 'failed'; s.dispatchEvent(new Event('change'));`);
  await until(async () => (await reqs()).filter((x) => x.startsWith('/api/v1/inspection/reports')).length >= 2, '第二次列表请求');
  const q = new URL((await reqs()).filter((x) => x.startsWith('/api/v1/inspection/reports')).pop(), 'http://x').searchParams;
  eq([q.get('latest'), q.get('level')], ['1', 'failed']);
});
await openInspect('');
await t('不带参数：和以前一样，没有 latest，也没有提示条', async () => {
  const r = (await reqs()).filter((x) => x.startsWith('/api/v1/inspection/reports'))[0];
  const q = new URL(r, 'http://x').searchParams;
  eq([q.has('latest'), q.has('level')], [false, false]);
  eq(await ev(`return document.querySelectorAll('#msg .alert').length`), 0);
});
await openInspect('?level=bogus');
await t('level 不是列表里有的值：忽略，不报错', async () => {
  eq(await ev(`return document.getElementById('f-level').value`), '');
});

await t('全程无控制台错误 / CSP 违规', async () => { eq(errors, [], '控制台错误'); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
if (fail) console.log('失败项：\n  ' + failures.join('\n  '));
cleanup();
process.exit(fail ? 1 : 0);
