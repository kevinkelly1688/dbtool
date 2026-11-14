/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_duty.mjs
 * @time: 2026/10/28
 */
// 值班表前端测试：顶栏“值班”芯片 + 浮层（web/static/js/shell/duty.js）和管理页（web/templates/duty/index.html + web/static/js/duty/main.js）。
// 自起静态服务 + 假接口 + 无头 Chrome（CDP），不依赖第三方包。
// 页面用的是真实资源：顶栏取自 web/templates/layout.html 里的 <header class="topbar">（Go 模板指令按服务端的规则代入），
// 管理页是真实模板（去掉 Go 模板指令）放进真实外壳，样式和脚本都是 web/static 里的真文件；接口返回的是测试里造的数据。
// 服务端怎么渲染顶栏、怎么算谁在值班，由 Go 测试（internal/duty）覆盖；这里只测浏览器里的显示与交互。
// 用法：node scripts/frontend-test/run_duty.mjs [chrome路径]
// 环境变量：DUTY_HTTP_PORT（默认 8940）、DUTY_CDP_PORT（默认 9277）、DUTY_SHOT（给一个目录就保存关键状态的截图）。
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
const tplRoot = path.join(root, 'web/templates');
const HTTP = +(process.env.DUTY_HTTP_PORT || 8940);
const CDP = +(process.env.DUTY_CDP_PORT || 9277);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const base = `http://127.0.0.1:${HTTP}`;
const MIME = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8', '.svg': 'image/svg+xml', '.json': 'application/json' };
const J = JSON.stringify;

// ---------------------------------------------------------------- 造数据
const clock = (m) => `${String(Math.floor(m / 60)).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`;
const seg = (s, e, user, display, name, extra = {}) => ({ s, e, start: clock(s), end: e >= 1440 ? '24:00' : clock(e), kind: 'shift', name, user, display, ...extra });
const gapSeg = (s, e, extra = {}) => ({ s, e, start: clock(s), end: e >= 1440 ? '24:00' : clock(e), gap: true, ...extra });
const NOW = 1792000000; // 服务器“现在”的 Unix 秒（只用来算剩余时间 / 下次刷新，和真实时间无关）
const TODAY = [seg(0, 480, 'zhaowu', '赵五', '夜班', { past: true }), seg(480, 1140, 'zhangsan', '张三', '白班', { cur: true }), seg(1140, 1440, 'lisi', '李四', '晚班')];

function nowView(over = {}) {
  return {
    enabled: true, now: NOW, now_min: 630, tz: 'UTC+08:00', date: '2026-10-22', state: 'on',
    cur: { kind: 'shift', name: '白班', user: 'zhangsan', display: '张三', contact: '分机 8001', email: 'zhangsan@x.example', start: '08:00', end: '19:00', ends_at: NOW + 8 * 3600 + 30 * 60 },
    next: { user: 'lisi', display: '李四', name: '晚班', kind: 'shift', at: NOW + 8 * 3600 + 30 * 60, time: '19:00' },
    next_at: NOW + 8 * 3600 + 30 * 60, today: TODAY, ...over,
  };
}

const SHIFTS = [
  { id: 1, name: '白班', user: 'zhangsan', display: '张三', email: 'zhangsan@x.example', active: true, missing: false, start: '08:00', end: '19:00', s: 480, e: 1140, cross: false, minutes: 660, mask: 127, contact: '分机 8001', remark: '', enabled: true, updated_by: 'root', updated_at: '2026-10-22 10:00', version: 'ver-1' },
  { id: 2, name: '晚班', user: 'lisi', display: '李四', email: 'lisi@x.example', active: true, missing: false, start: '19:00', end: '24:00', s: 1140, e: 0, cross: true, minutes: 300, mask: 127, contact: '', remark: '周末也上', enabled: true, updated_by: 'root', updated_at: '2026-10-22 10:01', version: 'ver-2' },
  { id: 3, name: '夜班', user: 'zhaowu', display: '赵五', email: 'zhaowu@x.example', active: true, missing: false, start: '00:00', end: '08:00', s: 0, e: 480, cross: false, minutes: 480, mask: 127, contact: '', remark: '', enabled: true, updated_by: 'root', updated_at: '2026-10-22 10:02', version: 'ver-3' },
];

function overview(over = {}) {
  const days = [0, 1, 2, 3, 4, 5, 6].map((i) => ({
    date: `2026-10-${22 + i}`, weekday: (4 + i) % 7, today: i === 0,
    segments: i === 0 ? TODAY : TODAY.map((g) => ({ ...g, cur: false, past: false })),
  }));
  return {
    ready: true, tz: 'UTC+08:00', now: NOW, current: nowView(), shifts: SHIFTS.map((s) => ({ ...s })), overrides: [], gaps: [], days,
    users: [{ username: 'zhangsan', display: '张三' }, { username: 'lisi', display: '李四' }, { username: 'wangwu', display: '王五' }, { username: 'plain', display: 'plain' }],
    limits: { name: 40, contact: 100, remark: 200, shifts: 50, override_days: 31 }, ...over,
  };
}

// 假接口的状态：每个测试开头 reset
const state = { overview: overview(), now: nowView(), nowStatus: 200, overviewStatus: 200, calls: [], reply: null };
function reset() {
  state.overview = overview();
  state.now = nowView();
  state.nowStatus = 200;
  state.overviewStatus = 200;
  state.calls = [];
  state.reply = null;
}

// ---------------------------------------------------------------- 页面
const html = (s) => s.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

// 顶栏：layout.html 里的 <header class="topbar">，值班芯片那一块按服务端模板的规则代入，其余模板指令去掉
function topbarHtml(chip) {
  const layout = fs.readFileSync(path.join(tplRoot, 'layout.html'), 'utf8');
  let head = layout.slice(layout.indexOf('<header class="topbar">'), layout.indexOf('</header>') + '</header>'.length);
  const m = head.match(/\{\{if \.Duty\}\}([\s\S]*?)\n  <time class="tb-clock"/);
  if (!m) throw new Error('layout.html 里找不到值班芯片块');
  let block = '';
  if (chip) {
    block = m[1].replace(/\{\{end\}\}\s*$/, '')
      .replace('{{.Duty.JSON}}', html(chip.json))
      .replace(/\{\{if \.Btn\.Can "duty\.view"\}}( data-manage="\/settings\/duty")\{\{end\}\}/, chip.manage ? '$1' : '')
      .replace('{{if eq .Duty.State "on"}}on{{else}}none{{end}}', chip.state === 'on' ? 'on' : 'none')
      .replace('{{if eq .Duty.State "on"}}{{.Duty.Name}}{{else}}{{t "暂无|duty"}}{{end}}', chip.state === 'on' ? html(chip.name) : '暂无')
      .replace(/\{\{if \.Duty\.Override\}}( <i class="td-tag">\{\{t "调班"\}\}<\/i>)\{\{end\}\}/, chip.override ? '$1' : '')
      .replace('{{.Duty.Range}}', chip.state === 'on' ? html(chip.range) : '');
  }
  head = head.replace(m[0], block + '\n  <time class="tb-clock"');
  return untranslate(head)
    .replace(/\{\{range \.Nav2\.Groups\}\}[\s\S]*?\{\{end\}\}/g, '')
    .replace(/\{\{-?[\s\S]*?-?\}\}/g, '');
}

function chipOf(view, manage = true) {
  const c = view.cur;
  return { json: J(view), state: view.state, name: c ? c.display : '', range: c ? `${c.start}–${c.end}` : '', override: !!(c && c.kind === 'override'), manage };
}

function enCatalog() {
  const dir = path.join(root, 'web/i18n/en');
  const out = {};
  for (const f of fs.readdirSync(dir)) if (f.endsWith('.json')) Object.assign(out, JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8')));
  return out;
}
const EN = enCatalog();

function strip(s) {
  return untranslate(s).replace(/\{\{\/\*[\s\S]*?\*\/\}\}/g, '').replace(/\{\{-?[\s\S]*?-?\}\}/g, '');
}
function define(src, name) {
  const tag = `{{define "${name}"}}`;
  const i = src.indexOf(tag);
  if (i < 0) return '';
  const rest = src.slice(i + tag.length);
  const j = rest.search(/\{\{define "/);
  let body = j < 0 ? rest : rest.slice(0, j);
  const k = body.lastIndexOf('{{end}}');
  if (k >= 0) body = body.slice(0, k);
  return body;
}

function shell(lang, skin, top, content, scripts, headExtra = '') {
  return `<!doctype html><html lang="${lang}" data-skin="${skin}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="csrf-token" content="test-csrf"><title>duty</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${headExtra}</head>
<body><div class="app"><aside class="side"><a class="logo" href="#"><i>DB</i></a></aside><div class="main">${top}${content}</div></div>${scripts}</body></html>`;
}

function chipPage(q) {
  const view = state.now;
  const chip = q.get('chip') === 'none' ? null : chipOf(JSON.parse(q.get('view') || J(view)), q.get('manage') !== '0');
  const top = topbarHtml(chip);
  return shell(q.get('lang') || 'zh-CN', q.get('skin') || 'dark', top, '<main class="page"><div class="page-head"><h1>测试页</h1></div></main>',
    '<script type="module" src="/static/js/common.js"></script><script type="module" src="/static/js/shell/duty.js"></script>');
}

function managePage(q) {
  const src = fs.readFileSync(path.join(tplRoot, 'duty/index.html'), 'utf8');
  let content = strip(define(src, 'content'));
  if (q.get('ro') === '1') { // 没有权限：服务端把按钮渲染成禁用（Btn.Attr），页面上的编辑 / 撤销按钮也不出现
    content = content.replace('data-edit="1"', 'data-edit=""').replace('data-swap="1"', 'data-swap=""')
      .replace('id="duty-shift-new"', 'id="duty-shift-new" disabled').replace('id="duty-swap-new"', 'id="duty-swap-new" disabled');
  }
  return shell(q.get('lang') || 'zh-CN', q.get('skin') || 'dark', '<header class="topbar"><b>顶栏</b></header>', content, strip(define(src, 'scripts')), strip(define(src, 'head')));
}

function body(req) {
  return new Promise((resolve) => {
    let b = '';
    req.on('data', (c) => { b += c; });
    req.on('end', () => { try { resolve(b ? JSON.parse(b) : null); } catch (_) { resolve(null); } });
  });
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://x');
  const send = (code, obj) => { res.writeHead(code, { 'Content-Type': MIME['.json'] }); res.end(J(obj)); };
  if (url.pathname === '/chip') { res.writeHead(200, { 'Content-Type': MIME['.html'] }); return res.end(chipPage(url.searchParams)); }
  if (url.pathname === '/manage') { res.writeHead(200, { 'Content-Type': MIME['.html'] }); return res.end(managePage(url.searchParams)); }
  if (url.pathname === '/i18n/en.json') return send(200, EN);
  if (url.pathname.startsWith('/static/')) {
    const file = path.join(staticRoot, decodeURIComponent(url.pathname.slice(8)));
    if (file.startsWith(staticRoot) && fs.existsSync(file)) {
      res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream', 'Cache-Control': 'no-store' });
      return fs.createReadStream(file).pipe(res);
    }
  }
  if (url.pathname.startsWith('/api/v1/duty/')) {
    const sub = url.pathname.slice('/api/v1/duty/'.length);
    if (req.method === 'GET' && sub === 'now') {
      state.calls.push({ method: 'GET', url: url.pathname, headers: req.headers });
      return state.nowStatus === 200 ? send(200, { data: state.now }) : send(state.nowStatus, { code: 'internal', message: '服务内部错误' });
    }
    if (req.method === 'GET' && sub === 'overview') {
      state.calls.push({ method: 'GET', url: url.pathname, headers: req.headers });
      return state.overviewStatus === 200 ? send(200, { data: state.overview }) : send(state.overviewStatus, { code: 'internal', message: '服务内部错误' });
    }
    const b = await body(req);
    state.calls.push({ method: req.method, url: url.pathname, body: b });
    if (state.reply) { const r = state.reply; state.reply = null; return send(r.status, r.body); }
    return send(200, { data: { id: 99, ok: true } });
  }
  res.writeHead(404);
  res.end('nf');
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));

// ---------------------------------------------------------------- 无头 Chrome
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'duty-chrome-'));
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
  } else if (d.method === 'Log.entryAdded' && d.params.entry.level === 'error' && !/Failed to load resource/.test(d.params.entry.text)) {
    errors.push('log: ' + d.params.entry.text + ' ' + (d.params.entry.url || ''));
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
async function waitFor(expr, what, ms = 6000) {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) {
    if (await ev(`return !!(${expr});`).catch(() => false)) return;
    await sleep(40);
  }
  throw new Error('等待超时：' + what);
}

let pass = 0, fail = 0;
const failures = [];
async function t(name, fn) {
  reset();
  try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; failures.push(name); console.log('  FAIL ' + name + '\n       ' + e.message); }
}
const eq = (a, b, m) => { if (J(a) !== J(b)) throw new Error(`${m || ''} 期望 ${J(b)} 实得 ${J(a)}`); };
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };
const has = (s, sub, m) => { if (!String(s).includes(sub)) throw new Error(`${m || ''} 应包含 ${J(sub)}，实得 ${J(String(s).slice(0, 400))}`); };
const lacks = (s, sub, m) => { if (String(s).includes(sub)) throw new Error(`${m || ''} 不应包含 ${J(sub)}`); };

async function shot(name) {
  if (!process.env.DUTY_SHOT) return;
  fs.mkdirSync(process.env.DUTY_SHOT, { recursive: true });
  fs.writeFileSync(path.join(process.env.DUTY_SHOT, name + '.png'), Buffer.from((await send('Page.captureScreenshot', { format: 'png' })).data, 'base64'));
}

async function open(url, w = 1366, h = 800, mobile = false) {
  await send('Emulation.setDeviceMetricsOverride', { width: w, height: h, deviceScaleFactor: 1, mobile });
  await send('Page.navigate', { url: `${base}${url}` });
  for (let i = 0; i < 100; i++) {
    if (await ev('return document.readyState === "complete"').catch(() => false)) break;
    await sleep(40);
  }
}
async function openChip(query = '', w = 1366, h = 800) {
  await open(`/chip?${query}`, w, h);
  await ev('await document.fonts.ready;');
  await sleep(120);
}
async function openManage(query = '', w = 1366, h = 900) {
  await open(`/manage?${query}`, w, h);
  await waitFor('!document.getElementById("duty-body").hidden || !document.getElementById("duty-missing").hidden', '管理页加载');
  await ev('await document.fonts.ready;');
  await sleep(120);
}
const text = (sel) => ev(`const e = document.querySelector(${J(sel)}); return e ? e.innerText.replace(/\\s+/g, ' ').trim() : null;`);
const texts = (sel) => ev(`return [...document.querySelectorAll(${J(sel)})].map((e) => e.innerText.replace(/\\s+/g, ' ').trim());`);
const css = (sel, prop) => ev(`const e = document.querySelector(${J(sel)}); return e ? getComputedStyle(e)[${J(prop)}] : null;`);
const click = (sel) => ev(`document.querySelector(${J(sel)}).click(); return true;`);
const popOpen = () => ev('return !document.getElementById("td-pop").hidden;');
const setVal = (sel, v) => ev(`const e = document.querySelector(${J(sel)}); e.value = ${J(v)}; e.dispatchEvent(new Event('input', { bubbles: true })); e.dispatchEvent(new Event('change', { bubbles: true })); return true;`);
// 统一确认框（confirm.js）：点里面的某个按钮
const clickConfirm = (label) => ev(`const b = [...document.querySelectorAll('.cf-dialog button')].find((x) => x.textContent.trim() === ${J(label)}); if (!b) throw new Error('确认框里没有按钮 ' + ${J(label)}); b.click(); return true;`);

console.log(`值班表前端测试（${base}）`);

// ================================================================ 顶栏芯片
await t('芯片：有人值班时显示“值班 姓名 时间段”，在时钟前面，绿点', async () => {
  await openChip();
  eq(await text('#td-btn'), '值班 张三 08:00–19:00');
  truthy((await ev('return document.getElementById("td-btn").className')).split(' ').includes('on'), '有人值班用 on 样式');
  eq(await ev('const b = document.getElementById("tb-duty"); const c = document.getElementById("tb-clock"); return !!(b.compareDocumentPosition(c) & Node.DOCUMENT_POSITION_FOLLOWING);'), true, '值班芯片在时钟前面');
  eq(await ev('const d = document.querySelector(".td-dot"); const root = getComputedStyle(document.documentElement).getPropertyValue("--dt-ok").trim(); const probe = document.createElement("i"); probe.style.color = root; document.body.append(probe); const want = getComputedStyle(probe).color; return getComputedStyle(d).backgroundColor === want;'), true, '圆点用 --dt-ok');
  await shot('chip-desktop');
});

await t('芯片：窗口变窄——≤1180px 只留姓名，≤520px 只留“值班”，顶栏不出现横向滚动', async () => {
  for (const [w, wantName, wantRange, wantLabel] of [[1366, true, true, true], [1100, true, false, true], [900, true, false, true], [600, true, false, true], [480, false, false, true], [375, false, false, true]]) {
    await openChip('', w, 700);
    const r = await ev(`const vis = (s) => { const e = document.querySelector(s); return !!e && getComputedStyle(e).display !== 'none' && e.getBoundingClientRect().width > 0; };
      const top = document.querySelector('.topbar'); const b = document.getElementById('td-btn').getBoundingClientRect();
      return { name: vis('.td-name'), range: vis('.td-range'), label: vis('.td-label'), over: top.scrollWidth - top.clientWidth, right: Math.round(b.right), vw: innerWidth, hs: document.documentElement.scrollWidth - innerWidth };`);
    eq([r.name, r.range, r.label], [wantName, wantRange, wantLabel], `宽 ${w} 的显示项`);
    truthy(r.over <= 1 && r.hs <= 0, `宽 ${w} 顶栏不应横向溢出 ${J(r)}`);
    truthy(r.right <= r.vw, `宽 ${w} 芯片不应超出窗口 ${J(r)}`);
  }
  await openChip('', 375, 700);
  await shot('chip-375');
});

await t('浮层：点开显示当前值班人、联系方式、24 小时时间条、今日明细、交班提示和管理入口；再点 / Esc / 点外面关闭', async () => {
  await openChip();
  eq(await popOpen(), false, '默认收起');
  await click('#td-btn');
  eq(await popOpen(), true);
  eq(await ev('return document.getElementById("td-btn").getAttribute("aria-expanded")'), 'true');
  const pop = await text('#td-pop');
  for (const s of ['当前值班', '白班', '张三', 'zhangsan', '08:00–19:00', '还剩 8 小时 30 分', '联系方式：分机 8001', '今日排班', '19:00 交班给 李四', '管理值班表', '时间为服务器时区 UTC+08:00']) has(pop, s);
  lacks(pop, 'zhangsan@x.example', '填了联系方式就只显示它，不再带上邮箱');
  eq(await ev('return document.querySelectorAll("#td-pop .dv-seg").length'), 3, '时间条三段');
  eq(await ev('return [...document.querySelectorAll("#td-pop .dv-seg")].map((s) => s.className.replace("dv-seg", "").trim())'), ['past', 'cur', ''], '已结束 / 当前 / 未开始');
  eq(await ev('const g = document.querySelectorAll("#td-pop .dv-seg"); const w = [...g].map((s) => Math.round(s.getBoundingClientRect().width)); const tot = w.reduce((a, b) => a + b, 0); return [Math.abs(w[0] / tot - 8 / 24) < 0.03, Math.abs(w[1] / tot - 11 / 24) < 0.03, Math.abs(w[2] / tot - 5 / 24) < 0.03];'), [true, true, true], '三段宽度按 8 / 11 / 5 小时');
  const mark = await ev('const m = document.querySelector("#td-pop .dv-now"); const bar = document.querySelector("#td-pop .dv-bar"); return Math.round(parseFloat(m.style.left));');
  truthy(mark >= 43 && mark <= 45, `现在的竖线在 10:30 ≈ 43.75%，实得 ${mark}`);
  eq(await texts('#td-pop .td-row'), ['00:00–08:00 赵五 夜班', '08:00–19:00 张三 白班 当前', '19:00–24:00 李四 晚班'], '今日明细');
  eq(await ev('return document.querySelector("#td-pop .td-manage").getAttribute("href")'), '/settings/duty');
  await shot('chip-popover');
  await click('#td-btn');
  eq(await popOpen(), false, '再点收起');
  await click('#td-btn');
  await ev('document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" })); return 1;');
  eq(await popOpen(), false, 'Esc 收起');
  eq(await ev('return document.activeElement && document.activeElement.id'), 'td-btn', 'Esc 后焦点回到芯片');
  await click('#td-btn');
  await ev('document.querySelector(".page-head h1").click(); return 1;');
  eq(await popOpen(), false, '点外面收起');
});

await t('浮层不超出窗口（宽 375 / 520 / 700 / 1000 / 1366），手机宽度铺满屏宽', async () => {
  for (const w of [375, 520, 700, 1000, 1366]) {
    await openChip('', w, 800);
    await click('#td-btn');
    const r = await ev('const b = document.getElementById("td-pop").getBoundingClientRect(); return { l: Math.round(b.left), r: Math.round(b.right), t: Math.round(b.top), b: Math.round(b.bottom), vw: innerWidth, vh: innerHeight };');
    truthy(r.l >= 0 && r.r <= r.vw, `宽 ${w} 浮层左右不超出窗口 ${J(r)}`);
    truthy(r.b <= r.vh, `宽 ${w} 浮层不超出窗口底部 ${J(r)}`);
    if (w <= 700) truthy(r.r - r.l >= w - 30, `宽 ${w} 浮层应铺满屏宽 ${J(r)}`);
  }
  await openChip('', 375, 800);
  await click('#td-btn');
  await shot('chip-popover-375');
});

await t('没有 duty.view 的人：浮层里没有“管理值班表”入口', async () => {
  await openChip('manage=0');
  await click('#td-btn');
  lacks(await text('#td-pop'), '管理值班表');
  eq(await ev('return document.querySelectorAll("#td-pop .td-manage").length'), 0);
});

await t('当前时段没人：芯片是灰色的“值班 暂无”，浮层写“当前时段没有人值班”并给出下一次交班', async () => {
  const none = nowView({ state: 'none', cur: undefined, next: { user: 'zhangsan', display: '张三', name: '白班', kind: 'shift', at: NOW + 3600, time: '08:00' }, next_at: NOW + 3600,
    today: [gapSeg(0, 480, { cur: true }), seg(480, 1140, 'zhangsan', '张三', '白班'), seg(1140, 1440, 'lisi', '李四', '晚班')] });
  await openChip(`view=${encodeURIComponent(J(none))}`);
  eq(await text('#td-btn'), '值班 暂无');
  truthy((await ev('return document.getElementById("td-btn").className')).split(' ').includes('none'), '无人值班用 none 样式');
  await click('#td-btn');
  const pop = await text('#td-pop');
  has(pop, '当前时段没有人值班');
  has(pop, '08:00 交班给 张三');
  has(pop, '无人值班');
  eq(await ev('return document.querySelectorAll("#td-pop .dv-seg.gap").length'), 1, '没人的时段画成斜线');
});

await t('调班：芯片带“调班”标记，浮层写调班原因和起止时间', async () => {
  const swap = nowView({ cur: { kind: 'override', name: '', reason: '请假', user: 'wangwu', display: '王五', contact: '', email: 'wangwu@x.example', start: '09:00', end: '12:00', ends_at: NOW + 5400 },
    next: { user: 'zhangsan', display: '张三', name: '白班', kind: 'shift', at: NOW + 5400, time: '12:00' }, next_at: NOW + 5400,
    today: [seg(0, 480, 'zhaowu', '赵五', '夜班', { past: true }), seg(480, 540, 'zhangsan', '张三', '白班', { past: true }), { ...seg(540, 720, 'wangwu', '王五', '', { cur: true }), kind: 'override', reason: '请假' }, seg(720, 1140, 'zhangsan', '张三', '白班'), seg(1140, 1440, 'lisi', '李四', '晚班')] });
  await openChip(`view=${encodeURIComponent(J(swap))}`);
  eq(await text('#td-btn'), '值班 王五 调班 09:00–12:00');
  await click('#td-btn');
  const pop = await text('#td-pop');
  for (const s of ['调班原因：请假', '12:00 交班给 张三', '联系方式：wangwu@x.example', '09:00–12:00']) has(pop, s);
  eq(await ev('return document.querySelectorAll("#td-pop .dv-seg.swap").length'), 1, '调班那一段');
});

await t('到交班时刻自动刷新：取到新快照后芯片和打开着的浮层都更新（计时器加速 20 倍）', async () => {
  const hook = await send('Page.addScriptToEvaluateOnNewDocument', { source: `
    const realSet = window.setTimeout.bind(window);
    window.__timers = [];
    window.setTimeout = (fn, ms, ...a) => { const scaled = ms >= 4000 ? Math.max(100, ms / 20) : ms; window.__timers.push(ms); return realSet(fn, scaled, ...a); };` });
  try {
    // 初始快照：10 秒之后换班（要等 10 秒 + 1.5 秒余量 = 11.5 秒，加速 20 倍后约 0.6 秒）
    const first = nowView({ next_at: NOW + 10 });
    await open(`/chip?view=${encodeURIComponent(J(first))}`);
    await sleep(100);
    const after = nowView({ now: NOW + 300, now_min: 635, cur: { kind: 'shift', name: '晚班', user: 'lisi', display: '李四', contact: '分机 8002', email: 'lisi@x.example', start: '19:00', end: '24:00', ends_at: NOW + 20000 }, next: { user: 'zhaowu', display: '赵五', name: '夜班', kind: 'shift', at: NOW + 20000, time: '10-23 00:00' }, next_at: NOW + 20000 });
    state.now = after;
    await click('#td-btn'); // 打开着浮层等它更新
    await waitFor('document.getElementById("td-btn").innerText.includes("李四")', '芯片刷新成李四', 5000);
    eq(await text('#td-btn'), '值班 李四 19:00–24:00');
    has(await text('#td-pop'), '联系方式：分机 8002');
    lacks(await text('#td-pop'), 'lisi@x.example');
    has(await text('#td-pop'), '10-23 00:00 交班给 赵五');
    truthy(state.calls.some((c) => c.url === '/api/v1/duty/now'), '应请求了 /api/v1/duty/now');
    truthy(state.calls.filter((c) => c.url === '/api/v1/duty/now').every((c) => c.headers['x-background-poll'] === '1'), '定时刷新要带后台轮询标记，服务端才不会给会话续期');
    // 平时最长 5 分钟取一次：下一个计时器不超过 300000ms，也不小于 5000ms
    const timers = await ev('return window.__timers.filter((m) => m >= 4000)');
    truthy(timers.length >= 2 && timers.every((m) => m >= 5000 && m <= 300000), `计时器 ${J(timers)}`);
  } finally {
    await send('Page.removeScriptToEvaluateOnNewDocument', { identifier: hook.identifier });
  }
});

await t('接口说没有值班信息了：整块藏起来；接口出错：保持原样', async () => {
  const hook = await send('Page.addScriptToEvaluateOnNewDocument', { source: 'const rs = window.setTimeout.bind(window); window.setTimeout = (fn, ms, ...a) => rs(fn, ms >= 4000 ? 300 : ms, ...a);' });
  try {
    state.nowStatus = 500;
    await openChip();
    await sleep(900);
    eq(await text('#td-btn'), '值班 张三 08:00–19:00', '接口出错时保持原样');
    eq(await ev('return document.getElementById("tb-duty").hidden'), false);
    state.nowStatus = 200;
    state.now = { enabled: false };
    await waitFor('document.getElementById("tb-duty").hidden', '芯片隐藏', 4000);
    eq(await ev('return getComputedStyle(document.getElementById("tb-duty")).display'), 'none');
    // 之后又排上班：重新显示
    state.now = nowView();
    await waitFor('!document.getElementById("tb-duty").hidden', '芯片重新显示', 4000);
  } finally {
    await send('Page.removeScriptToEvaluateOnNewDocument', { identifier: hook.identifier });
  }
});

await t('收到 duty:changed（管理页改了班次 / 调班）：芯片马上取一次并更新，不等定时刷新', async () => {
  await openChip();
  eq(await text('#td-btn'), '值班 张三 08:00–19:00');
  const count = () => state.calls.filter((c) => c.url === '/api/v1/duty/now').length;
  const before = count();
  state.now = nowView({ cur: { ...nowView().cur, user: 'lisi', display: '李四', name: '晚班', start: '19:00', end: '24:00' } });
  await ev('document.dispatchEvent(new CustomEvent("duty:changed")); return 1;');
  await waitFor('document.getElementById("td-btn").innerText.includes("李四")', '芯片马上更新', 3000);
  eq(await text('#td-btn'), '值班 李四 19:00–24:00');
  eq(count(), before + 1, '只多取了一次');
});

await t('接口返回 401（登录过期）：芯片保持原样，不再继续轮询', async () => {
  const hook = await send('Page.addScriptToEvaluateOnNewDocument', { source: 'const rs = window.setTimeout.bind(window); window.setTimeout = (fn, ms, ...a) => rs(fn, ms >= 4000 ? 200 : ms, ...a);' });
  try {
    state.nowStatus = 401;
    await openChip();
    await sleep(1500);
    eq(state.calls.filter((c) => c.url === '/api/v1/duty/now').length, 1, '只请求了一次，之后不再轮询');
    eq(await text('#td-btn'), '值班 张三 08:00–19:00', '保持原样');
  } finally {
    await send('Page.removeScriptToEvaluateOnNewDocument', { identifier: hook.identifier });
  }
});

await t('快到服务器时区的零点时：在零点之后马上刷新（换成新的一天的“今日排班”），不等 5 分钟', async () => {
  const hook = await send('Page.addScriptToEvaluateOnNewDocument', { source: `
    const realSet = window.setTimeout.bind(window);
    window.__timers = [];
    window.setTimeout = (fn, ms, ...a) => { window.__timers.push(ms); return realSet(fn, ms >= 4000 ? 600000 : ms, ...a); };` }); // 只记录请求的时长，真正的计时器拖到测试结束之后
  try {
    // 23:59:40，下一次交班在 20000 秒之后；5 分钟的上限更晚，但离零点只有 20 秒
    await open(`/chip?view=${encodeURIComponent(J(nowView({ now_min: 1439, next_at: NOW + 20000 })))}`);
    await sleep(150);
    const want = (86400 - (1439 * 60 + (NOW % 60))) * 1000 + 1500;
    const timers = await ev('return window.__timers.filter((m) => m >= 4000)');
    truthy(timers.length >= 1 && timers.every((m) => m > want - 3000 && m <= want), `计时器 ${J(timers)}，应约为 ${want}`);
    // 白天（10:30）不受影响：还是最长 5 分钟
    await open('/chip');
    await sleep(150);
    const day = await ev('return window.__timers.filter((m) => m >= 4000)');
    truthy(day.every((m) => m === 300000), `白天的计时器应是 5 分钟：${J(day)}`);
  } finally {
    await send('Page.removeScriptToEvaluateOnNewDocument', { identifier: hook.identifier });
  }
});

await t('浮层：键盘焦点在“管理值班表”上时，自动刷新重建浮层不会丢掉焦点', async () => {
  const hook = await send('Page.addScriptToEvaluateOnNewDocument', { source: 'const rs = window.setTimeout.bind(window); window.setTimeout = (fn, ms, ...a) => rs(fn, ms >= 4000 ? 300 : ms, ...a);' });
  try {
    await openChip();
    await click('#td-btn');
    await ev('const a = document.querySelector("#td-pop .td-manage"); window.__old = a; a.focus(); return 1;');
    await waitFor('!window.__old.isConnected', '浮层被自动刷新重建', 4000);
    eq(await ev('const a = document.activeElement; return [a !== window.__old, a.classList.contains("td-manage")];'), [true, true], '焦点回到新的“管理值班表”链接上');
  } finally {
    await send('Page.removeScriptToEvaluateOnNewDocument', { identifier: hook.identifier });
  }
});

await t('英文界面：浮层和芯片是英文', async () => {
  await openChip('lang=en');
  await click('#td-btn');
  const pop = await text('#td-pop');
  for (const s of ['Currently on duty', "Today's roster", 'Handover to 李四 at 19:00', 'Manage roster', 'Contact: 分机 8001', '8 h 30 min left', 'Times are in the server time zone UTC+08:00', 'Current']) has(pop, s);
  lacks(pop, 'zhangsan@x.example');
  // 没人值班时芯片上的名字位置：英文是 Nobody（芯片上的“值班”二字是服务端按语言渲染的，这里的外壳是静态的，不测）
  const none = nowView({ state: 'none', cur: undefined, next: undefined, today: [gapSeg(0, 1440, { cur: true })] });
  await openChip(`lang=en&view=${encodeURIComponent(J(none))}`);
  eq(await text('#td-btn .td-name'), 'Nobody');
  await click('#td-btn');
  has(await text('#td-pop'), 'Nobody is on duty right now');
});

// 对比度：各皮肤下，芯片和浮层里带颜色的文字都要看得清（≥ 4.5，小字号）
const CONTRAST_JS = `
  const parse = (c) => { const m = c.match(/rgba?\\(([^)]+)\\)/); const p = m[1].split(/[ ,\\/]+/).filter(Boolean).map(Number); return { r: p[0], g: p[1], b: p[2], a: p[3] === undefined ? 1 : p[3] }; };
  const over = (top, bot) => ({ r: top.r * top.a + bot.r * (1 - top.a), g: top.g * top.a + bot.g * (1 - top.a), b: top.b * top.a + bot.b * (1 - top.a), a: 1 });
  const bgOf = (el) => { const chain = []; for (let e = el; e; e = e.parentElement) chain.push(parse(getComputedStyle(e).backgroundColor)); let acc = { r: 255, g: 255, b: 255, a: 1 }; for (let i = chain.length - 1; i >= 0; i--) acc = over(chain[i], acc); return acc; };
  const lum = (c) => { const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b); };
  const ratio = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
  const check = (sel) => { const el = document.querySelector(sel); const fg = parse(getComputedStyle(el).color); const bg = bgOf(el); return { sel, ratio: Math.round(ratio(over(fg, bg), bg) * 100) / 100 }; };`;
await t('三套皮肤（深色 / 浅色 / 护眼）下，芯片和浮层里的文字对比度都 ≥ 4.5', async () => {
  for (const skin of ['dark', 'light', 'sepia']) {
    await openChip(`skin=${skin}`);
    await click('#td-btn');
    const r = await ev(`${CONTRAST_JS} return ['#td-btn', '#td-btn .td-name', '#td-btn .td-label', '#td-btn .td-range', '.td-h-shift', '.td-r-cur', '.td-row.cur', '.td-row.past', '.td-manage', '.td-tz', '.td-meta', '.dv-seg.cur'].map(check);`);
    for (const x of r) truthy(x.ratio >= 4.5 || (x.sel === '.td-row.past' || x.sel === '.td-tz') && x.ratio >= 3, `${skin} ${x.sel} 对比度 ${x.ratio}`);
    if (skin !== 'dark') await shot(`chip-popover-${skin}`);
  }
});

await t('调班标记和“暂无”状态在三套皮肤下也看得清', async () => {
  const swap = nowView({ cur: { kind: 'override', name: '', reason: '', user: 'wangwu', display: '王五', start: '09:00', end: '12:00', ends_at: NOW + 5400 } });
  const none = nowView({ state: 'none', cur: undefined });
  for (const skin of ['dark', 'light', 'sepia']) {
    await openChip(`skin=${skin}&view=${encodeURIComponent(J(swap))}`);
    let r = await ev(`${CONTRAST_JS} return [check('.td-tag'), check('#td-btn .td-name')];`);
    for (const x of r) truthy(x.ratio >= 4.5, `${skin} 调班 ${x.sel} ${x.ratio}`);
    await openChip(`skin=${skin}&view=${encodeURIComponent(J(none))}`);
    r = await ev(`${CONTRAST_JS} return [check('#td-btn .td-name'), check('#td-btn .td-label')];`);
    for (const x of r) truthy(x.ratio >= 4.5, `${skin} 暂无 ${x.sel} ${x.ratio}`);
  }
});

// ================================================================ 管理页
await t('管理页：当前值班、7 天预览、班次列表、空的调班列表；24 小时全覆盖', async () => {
  await openManage();
  const now = await text('#duty-now');
  for (const s of ['当前值班', '张三', 'zhangsan', '白班', '08:00–19:00 · 还剩 8 小时 30 分', '联系方式：分机 8001', '19:00 交班给 李四', '24 小时全覆盖', '现在：2026-10-22 10:30']) has(now, s);
  lacks(now, 'zhangsan@x.example', '填了联系方式就只显示它，不再带上邮箱');
  eq(await ev('return document.getElementById("duty-now").hasAttribute("aria-live")'), false, '每分钟自动刷新的区域不要 aria-live（读屏会反复播报）');
  eq(await ev('return document.querySelectorAll("#duty-days button").length'), 7, '七个日期页签');
  eq(await texts('#duty-days button').then((a) => a.slice(0, 3)), ['今天', '明天', '周六 10-24']);
  eq(await ev('return document.querySelectorAll("#duty-day .dv-seg").length'), 3);
  eq(await texts('#duty-day .duty-day-list li'), ['00:00–08:00 赵五 夜班', '08:00–19:00 张三 白班 当前', '19:00–24:00 李四 晚班']);
  eq(await ev('return document.querySelectorAll("#duty-day .dv-now").length'), 1, '今天画“现在”的竖线');
  eq(await texts('#duty-shifts tbody tr').then((a) => a.length), 3);
  const rows = await texts('#duty-shifts tbody tr');
  has(rows[0], '白班'); has(rows[0], '08:00–19:00 11 小时'); has(rows[0], '每天'); has(rows[0], '分机 8001');
  has(rows[1], '19:00–24:00 5 小时'); has(rows[1], '周末也上'); has(rows[1], 'lisi@x.example');
  has(rows[2], '00:00–08:00 8 小时');
  eq(await text('#duty-overrides tbody tr'), '最近没有临时调班。');
  await shot('manage-desktop');
});

await t('管理页：点日期页签切换预览；今天以外不画“现在”线、不标当前', async () => {
  await openManage();
  await ev('document.querySelectorAll("#duty-days button")[3].click(); return 1;');
  eq(await ev('return document.querySelectorAll("#duty-days button")[3].getAttribute("aria-pressed")'), 'true');
  eq(await ev('return document.querySelectorAll("#duty-day .dv-now").length'), 0);
  lacks(await text('#duty-day'), '当前');
  eq(await ev('return document.querySelectorAll("#duty-day .dv-seg.cur").length'), 0);
});

await t('管理页：时间条不同的人颜色不同，同一个人各天颜色一致；短于 50 分钟的段不写名字', async () => {
  const days = overview().days;
  days[0].segments = [seg(0, 480, 'zhaowu', '赵五', '夜班'), seg(480, 520, 'zhangsan', '张三', '白班', { cur: true }), seg(520, 1140, 'lisi', '李四', '晚班'), seg(1140, 1440, 'zhaowu', '赵五', '夜班')];
  state.overview = overview({ days });
  await openManage();
  const r = await ev('return [...document.querySelectorAll("#duty-day .dv-seg")].map((s) => ({ c: [...s.classList].filter((x) => /^p\\d$/.test(x))[0], t: s.textContent }));');
  eq(r.map((x) => x.c), ['p0', 'p1', 'p2', 'p0'], '按人上色（同一个人同一个颜色）');
  eq(r.map((x) => x.t), ['赵五', '', '李四', '赵五'], '40 分钟的段放不下名字');
});

await t('管理页：每周空档、值班人账号停用 / 不存在的提示', async () => {
  const shifts = SHIFTS.map((s) => ({ ...s }));
  shifts[0].active = false;
  shifts[2].active = false; shifts[2].missing = true;
  state.overview = overview({ shifts, gaps: [{ mask: 62, start: '00:00', end: '08:00', s: 0, e: 480 }, { mask: 65, start: '00:00', end: '24:00', s: 0, e: 1440 }],
    overrides: [{ id: 7, user: 'gone', display: 'gone', active: false, missing: false, start: '2026-10-22 20:00', end: '2026-10-22 22:00', start_ts: 1, end_ts: 2, reason: '', canceled: false, state: 'upcoming', created_by: 'root', created_at: '2026-10-22 09:00' }] });
  await openManage();
  const now = await text('#duty-now');
  has(now, '每周有这些时段没有人值班：工作日 00:00–08:00；周末 00:00–24:00');
  has(now, '班次「白班」的值班人 zhangsan 的账号已停用，这段时间会显示为无人值班。');
  has(now, '班次「夜班」的值班人 zhaowu 的账号已不存在，这段时间会显示为无人值班。');
  has(now, '调班 #7 的值班人 gone 的账号已停用，这段时间会显示为无人值班。');
  const rows = await texts('#duty-shifts tbody tr');
  has(rows[0], '账号已停用'); has(rows[2], '账号不存在');
  lacks(now, '24 小时全覆盖');
});

await t('管理页：没有班次时提示怎么开始；表没建时只显示提示', async () => {
  state.overview = overview({ shifts: [], gaps: [], current: nowView({ enabled: false, state: 'none', cur: undefined, next: undefined, today: [gapSeg(0, 1440)] }),
    days: overview().days.map((d) => ({ ...d, segments: [gapSeg(0, 1440)] })) });
  await openManage();
  has(await text('#duty-now'), '还没有排班。点右上角“新建班次”开始');
  eq(await text('#duty-shifts tbody tr'), '还没有班次。');
  eq(await texts('#duty-day .duty-day-list li'), ['00:00–24:00 无人值班']);
  state.overview = { ready: false, shifts: [], overrides: [], gaps: [], days: [], users: [], current: {}, limits: {} };
  await openManage();
  eq(await ev('return [document.getElementById("duty-missing").hidden, document.getElementById("duty-body").hidden]'), [false, true]);
  has(await text('#duty-missing'), 'deploy/sql/300_duty.sql');
});

await t('管理页：数据没取到（出错 / 表没建）时“新建班次 / 临时调班”禁用，取到后放开；没有权限的按钮不动', async () => {
  const btns = 'return [document.getElementById("duty-shift-new").disabled, document.getElementById("duty-swap-new").disabled];';
  state.overviewStatus = 500;
  await open('/manage');
  await waitFor('document.getElementById("duty-msg").innerText.includes("服务内部错误")', '加载失败提示');
  eq(await ev(btns), [true, true], '取数出错：按钮禁用');
  eq(await ev('return document.getElementById("duty-body").hidden'), true);
  state.overviewStatus = 200;
  state.overview = { ready: false, shifts: [], overrides: [], gaps: [], days: [], users: [], current: {}, limits: {} };
  await openManage();
  eq(await ev(btns), [true, true], '表没建：按钮禁用');
  state.overview = overview();
  await openManage();
  eq(await ev(btns), [false, false], '取到数据：按钮可用');
  await openManage('ro=1'); // 没有权限的按钮是服务端渲染成禁用的（见 managePage），数据取到后脚本也不能把它放开
  eq(await ev(btns), [true, true], '没有权限的按钮保持禁用');
});

await t('管理页：预览里的调班那一段写“调班：原因”，没写原因就只写“调班”', async () => {
  const days = overview().days;
  days[0].segments = [seg(0, 540, 'zhaowu', '赵五', '夜班', { past: true }), seg(540, 720, 'wangwu', '王五', '', { kind: 'override', reason: '请假', cur: true }),
    seg(720, 1140, 'wangwu', '王五', '', { kind: 'override', reason: '' }), seg(1140, 1440, 'lisi', '李四', '晚班')];
  state.overview = overview({ days });
  await openManage();
  const rows = await texts('#duty-day .duty-day-list li');
  has(rows[1], '调班：请假');
  has(rows[2], '调班'); lacks(rows[2], '调班：');
  await openManage('lang=en');
  has((await texts('#duty-day .duty-day-list li'))[1], 'Swap: 请假');
});

await t('管理页：每分钟的自动刷新带后台轮询标记（不给会话续期）；打开页面的首次加载不带', async () => {
  const hook = await send('Page.addScriptToEvaluateOnNewDocument', { source: 'const rs = window.setInterval.bind(window); window.setInterval = (fn, ms, ...a) => rs(fn, ms >= 60000 ? 300 : ms, ...a);' });
  try {
    await openManage();
    await waitFor('window.performance.now() > 1200', '等几轮自动刷新', 4000);
    const gets = state.calls.filter((c) => c.url === '/api/v1/duty/overview');
    truthy(gets.length >= 3, `应有几次自动刷新：${gets.length}`);
    eq(gets[0].headers['x-background-poll'], undefined, '首次加载是用户的操作，照常续期会话');
    truthy(gets.slice(1).every((c) => c.headers['x-background-poll'] === '1'), '自动刷新要带标记');
  } finally {
    await send('Page.removeScriptToEvaluateOnNewDocument', { identifier: hook.identifier });
  }
});

await t('新建班次：弹窗默认 08:00–19:00、每天；时长提示随时间变化；星期快捷按钮', async () => {
  await openManage();
  await click('#duty-shift-new');
  eq(await ev('return !document.getElementById("duty-sm").hidden'), true);
  eq(await text('#duty-f-span'), '08:00 到 19:00，共 11 小时');
  eq(await ev('return [...document.querySelectorAll("#duty-f-wd input")].map((c) => c.checked)'), [true, true, true, true, true, true, true]);
  eq(await ev('return [...document.querySelectorAll("#duty-f-wd label")].map((l) => l.textContent.trim())'), ['周一', '周二', '周三', '周四', '周五', '周六', '周日'], '周一排在最前');
  await setVal('#duty-f-start', '19:00'); await setVal('#duty-f-end', '00:00');
  eq(await text('#duty-f-span'), '19:00 到 24:00（当天结束），共 5 小时');
  await setVal('#duty-f-start', '22:00'); await setVal('#duty-f-end', '06:00');
  eq(await text('#duty-f-span'), '22:00 到次日 06:00（跨天），共 8 小时');
  await setVal('#duty-f-start', '08:00'); await setVal('#duty-f-end', '08:00');
  eq(await text('#duty-f-span'), '24 小时：从 08:00 到次日 08:00');
  await setVal('#duty-f-end', '08:30');
  eq(await text('#duty-f-span'), '08:00 到 08:30，共 30 分钟');
  await ev('document.querySelector("[data-wd=work]").click(); return 1;');
  eq(await ev('return [...document.querySelectorAll("#duty-f-wd input")].map((c) => c.checked)'), [true, true, true, true, true, false, false], '工作日');
  await ev('document.querySelector("[data-wd=weekend]").click(); return 1;');
  eq(await ev('return [...document.querySelectorAll("#duty-f-wd input")].map((c) => c.checked)'), [false, false, false, false, false, true, true], '周末');
  await ev('document.querySelector("[data-wd=all]").click(); return 1;');
  eq(await ev('return [...document.querySelectorAll("#duty-f-wd input")].every((c) => c.checked)'), true, '每天');
  await shot('manage-shift-modal');
  await ev('document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" })); return 1;');
  eq(await ev('return document.getElementById("duty-sm").hidden'), true, 'Esc 关弹窗');
});

await t('新建班次：必填校验；显示名自动换成登录名；提交的 JSON；成功后关弹窗并刷新', async () => {
  await openManage();
  await click('#duty-shift-new');
  await click('#duty-sm-ok');
  eq(await text('#duty-sm-msg'), '请填写班次名称');
  await setVal('#duty-f-name', '早班');
  await click('#duty-sm-ok');
  eq(await text('#duty-sm-msg'), '请选择值班人');
  await setVal('#duty-f-user', '王五');
  await ev('document.querySelectorAll("#duty-f-wd input").forEach((c) => { c.checked = false; }); return 1;');
  await click('#duty-sm-ok');
  eq(await text('#duty-sm-msg'), '至少选择一个适用星期');
  await setVal('#duty-f-start', ''); await ev('document.querySelector("[data-wd=work]").click(); return 1;');
  await click('#duty-sm-ok');
  eq(await text('#duty-sm-msg'), '请填写开始和结束时间');
  eq(state.calls.filter((c) => c.method === 'POST').length, 0, '校验不过不发请求');
  await setVal('#duty-f-start', '09:00'); await setVal('#duty-f-end', '18:00');
  await setVal('#duty-f-contact', ' 8003 '); await setVal('#duty-f-remark', '备注');
  await click('#duty-sm-ok');
  await waitFor('document.getElementById("duty-sm").hidden', '弹窗关闭');
  const post = state.calls.find((c) => c.method === 'POST');
  eq(post.url, '/api/v1/duty/shifts');
  eq(post.body, { name: '早班', username: 'wangwu', start: '09:00', end: '18:00', weekdays: [1, 2, 3, 4, 5], contact: '8003', remark: '备注', enabled: true });
  eq(await text('#duty-msg'), '已保存');
});

await t('新建班次：服务端报冲突（409）时错误留在弹窗里，弹窗不关；修改后可以再提交', async () => {
  await openManage();
  await click('#duty-shift-new');
  await setVal('#duty-f-name', '加班'); await setVal('#duty-f-user', 'lisi');
  state.reply = { status: 409, body: { code: 'conflict', message: '与班次「白班」时间重叠：周日 18:00–19:00' } };
  await click('#duty-sm-ok');
  await waitFor('document.getElementById("duty-sm-msg").innerText.includes("时间重叠")', '冲突提示');
  eq(await ev('return document.getElementById("duty-sm").hidden'), false, '弹窗不关');
  eq(await text('#duty-sm-msg'), '与班次「白班」时间重叠：周日 18:00–19:00');
  await setVal('#duty-f-start', '19:00'); await setVal('#duty-f-end', '20:00');
  await click('#duty-sm-ok');
  await waitFor('document.getElementById("duty-sm").hidden', '再次提交后关闭');
  eq(state.calls.filter((c) => c.method === 'POST').length, 2);
});

await t('编辑班次：预填（结束 24:00 显示成 00:00、星期按位图还原），保存走 PUT', async () => {
  const shifts = SHIFTS.map((s) => ({ ...s }));
  shifts[1].mask = 0x3e; // 周一到周五
  state.overview = overview({ shifts });
  await openManage();
  await ev('document.querySelectorAll("#duty-shifts tbody tr")[1].querySelector(".duty-ops button").click(); return 1;');
  eq(await text('#duty-sm-title'), '编辑班次');
  eq(await ev('return [document.getElementById("duty-f-name").value, document.getElementById("duty-f-user").value, document.getElementById("duty-f-start").value, document.getElementById("duty-f-end").value, document.getElementById("duty-f-remark").value]'), ['晚班', 'lisi', '19:00', '00:00', '周末也上']);
  eq(await ev('return [...document.querySelectorAll("#duty-f-wd input")].map((c) => c.checked)'), [true, true, true, true, true, false, false]);
  await setVal('#duty-f-name', '晚班（改）');
  await click('#duty-sm-ok');
  await waitFor('document.getElementById("duty-sm").hidden', '关闭');
  const put = state.calls.find((c) => c.method === 'PUT');
  eq(put.url, '/api/v1/duty/shifts/2');
  eq(put.body, { name: '晚班（改）', username: 'lisi', start: '19:00', end: '00:00', weekdays: [1, 2, 3, 4, 5], contact: '', remark: '周末也上', enabled: true, version: 'ver-2' }, '带上打开编辑框时的版本');
});

await t('编辑班次：别人刚改过（409）→ 提示留在弹窗里，列表刷成最新的，重新打开就是新内容', async () => {
  await openManage();
  await ev('document.querySelectorAll("#duty-shifts tbody tr")[0].querySelector(".duty-ops button").click(); return 1;');
  await setVal('#duty-f-contact', '分机 1111');
  // 保存之前别人改了：服务端回 409，之后列表里已经是新内容
  state.reply = { status: 409, body: { code: 'conflict', message: '这个班次刚被别人修改过，请刷新页面后重新编辑' } };
  state.overview = overview({ shifts: SHIFTS.map((s, i) => (i === 0 ? { ...s, contact: '分机 9999', version: 'ver-1b' } : { ...s })) });
  const gets = state.calls.filter((c) => c.url === '/api/v1/duty/overview').length;
  await click('#duty-sm-ok');
  await waitFor('document.getElementById("duty-sm-msg").innerText.includes("刚被别人修改过")', '冲突提示');
  eq(await ev('return document.getElementById("duty-sm").hidden'), false, '弹窗不关');
  await waitFor(`document.querySelector("#duty-shifts tbody tr").innerText.includes("分机 9999")`, '列表刷成最新');
  truthy(state.calls.filter((c) => c.url === '/api/v1/duty/overview').length > gets, '冲突后重新取了概览');
  // 关掉再打开：预填的是别人改过的内容，保存带的是新版本
  await click('#duty-sm-cancel');
  await ev('document.querySelectorAll("#duty-shifts tbody tr")[0].querySelector(".duty-ops button").click(); return 1;');
  eq(await ev('return document.getElementById("duty-f-contact").value'), '分机 9999');
  await click('#duty-sm-ok');
  await waitFor('document.getElementById("duty-sm").hidden', '关闭');
  const puts = state.calls.filter((c) => c.method === 'PUT');
  eq(puts.map((p) => p.body.version), ['ver-1', 'ver-1b'], '第二次带的是新版本');
});

await t('停用 / 启用走单独的接口，只发开关；删除要二次确认；改完发出 duty:changed 让顶栏芯片马上跟上', async () => {
  await openManage();
  await ev('window.__changed = 0; document.addEventListener("duty:changed", () => { window.__changed++; }); return 1;');
  await ev('const row = document.querySelectorAll("#duty-shifts tbody tr")[0]; [...row.querySelectorAll("button")].find((b) => b.textContent === "停用").click(); return 1;');
  await waitFor('document.getElementById("duty-msg").innerText.includes("已停用")', '已停用提示');
  await waitFor('window.__changed === 1', '改完发出 duty:changed');
  const en = state.calls.find((c) => c.url.endsWith('/enable'));
  eq([en.method, en.url, en.body], ['POST', '/api/v1/duty/shifts/1/enable', { enabled: false }], '停用只发开关，不带页面上可能已过期的其它字段');
  eq(state.calls.filter((c) => c.method === 'PUT').length, 0, '不再用整行覆盖的 PUT');
  // 已停用的班次：按钮变成“启用”，发 enabled: true
  const shifts = SHIFTS.map((s, i) => (i === 0 ? { ...s, enabled: false } : { ...s }));
  state.overview = overview({ shifts });
  await openManage();
  await ev('const row = document.querySelectorAll("#duty-shifts tbody tr")[0]; [...row.querySelectorAll("button")].find((b) => b.textContent === "启用").click(); return 1;');
  await waitFor('document.getElementById("duty-msg").innerText.includes("已启用")', '已启用提示');
  eq(state.calls.filter((c) => c.url.endsWith('/enable')).map((c) => c.body), [{ enabled: false }, { enabled: true }]);
  // 删除：先弹确认，没点确定之前不发请求
  await ev('const row = document.querySelectorAll("#duty-shifts tbody tr")[2]; [...row.querySelectorAll("button")].find((b) => b.textContent === "删除").click(); return 1;');
  await waitFor('document.querySelector(".cf-dialog")', '确认框');
  const conf = await text('.cf-dialog');
  has(conf, '删除班次「夜班」？');
  has(conf, '其它班次和临时调班不受影响');
  eq(state.calls.filter((c) => c.url.endsWith('/delete')).length, 0, '没点确定之前不发请求');
  await clickConfirm('确定删除');
  await waitFor('document.getElementById("duty-msg").innerText.includes("已删除")', '已删除提示');
  const del = state.calls.find((c) => c.url.endsWith('/delete'));
  eq([del.url, del.body], ['/api/v1/duty/shifts/3/delete', { confirm: true }]);
});

await t('临时调班：按班次填入时间（跨天的班到第二天）；提交的时间是 “年-月-日 时:分”；撤销要二次确认', async () => {
  const ov = [
    { id: 5, user: 'wangwu', display: '王五', active: true, missing: false, start: '2026-10-22 19:00', end: '2026-10-23 00:00', start_ts: 1, end_ts: 2, reason: '请假', canceled: false, state: 'upcoming', created_by: 'root', created_at: '2026-10-22 09:00' },
    { id: 4, user: 'lisi', display: '李四', active: true, missing: false, start: '2026-10-20 08:00', end: '2026-10-20 12:00', start_ts: 1, end_ts: 2, reason: '', canceled: false, state: 'past', created_by: 'root', created_at: '2026-10-19 09:00' },
    { id: 3, user: 'lisi', display: '李四', active: true, missing: false, start: '2026-10-21 08:00', end: '2026-10-21 12:00', start_ts: 1, end_ts: 2, reason: '', canceled: true, canceled_by: 'root', canceled_at: '2026-10-20 10:00', state: 'canceled', created_by: 'root', created_at: '2026-10-19 09:00' },
  ];
  state.overview = overview({ overrides: ov });
  await openManage();
  const rows = await texts('#duty-overrides tbody tr');
  eq(rows.length, 3);
  has(rows[0], '未开始'); has(rows[0], '请假'); has(rows[0], '撤销');
  has(rows[1], '已结束'); lacks(rows[1], '撤销');
  has(rows[2], '已撤销'); lacks(rows[2], ' 撤销 ');
  await click('#duty-swap-new');
  eq(await ev('return [...document.getElementById("duty-o-shift").options].map((o) => o.textContent)'), ['— 手动填写 —', '白班  08:00–19:00', '晚班  19:00–24:00', '夜班  00:00–08:00']);
  await setVal('#duty-o-date', '2026-10-25');
  await ev('const s = document.getElementById("duty-o-shift"); s.value = "2"; s.dispatchEvent(new Event("change", { bubbles: true })); return 1;');
  eq(await ev('return [document.getElementById("duty-o-start").value, document.getElementById("duty-o-end").value]'), ['2026-10-25T19:00', '2026-10-26T00:00'], '晚班（跨天）填到第二天 00:00');
  await ev('const s = document.getElementById("duty-o-shift"); s.value = "1"; s.dispatchEvent(new Event("change", { bubbles: true })); return 1;');
  eq(await ev('return [document.getElementById("duty-o-start").value, document.getElementById("duty-o-end").value]'), ['2026-10-25T08:00', '2026-10-25T19:00'], '白班在当天');
  await click('#duty-om-ok');
  eq(await text('#duty-om-msg'), '请选择值班人');
  await setVal('#duty-o-user', '王五'); await setVal('#duty-o-reason', ' 出差 '); await setVal('#duty-o-contact', '8009');
  await click('#duty-om-ok');
  await waitFor('document.getElementById("duty-om").hidden', '关闭');
  const post = state.calls.find((c) => c.url === '/api/v1/duty/overrides');
  eq(post.body, { username: 'wangwu', start: '2026-10-25 08:00', end: '2026-10-25 19:00', reason: '出差', contact: '8009' });
  // 撤销
  await ev('const row = document.querySelectorAll("#duty-overrides tbody tr")[0]; [...row.querySelectorAll("button")].find((b) => b.textContent === "撤销").click(); return 1;');
  await waitFor('document.querySelector(".cf-dialog")', '确认框');
  const conf = await text('.cf-dialog');
  has(conf, '撤销调班 #5？');
  has(conf, '王五');
  eq(state.calls.filter((c) => c.url.endsWith('/cancel')).length, 0);
  await clickConfirm('确定撤销');
  await waitFor('document.getElementById("duty-msg").innerText.includes("已撤销")', '已撤销提示');
  const c = state.calls.find((x) => x.url.endsWith('/cancel'));
  eq([c.url, c.body], ['/api/v1/duty/overrides/5/cancel', { confirm: true }]);
});

await t('只读用户（没有 duty.edit / duty.swap）：行里没有任何操作按钮', async () => {
  await openManage('ro=1');
  eq(await ev('return document.querySelectorAll("#duty-shifts .duty-ops button").length'), 0);
  const ov = [{ id: 5, user: 'wangwu', display: '王五', active: true, missing: false, start: '2026-10-22 19:00', end: '2026-10-23 00:00', start_ts: 1, end_ts: 2, reason: '请假', canceled: false, state: 'upcoming', created_by: 'root', created_at: '2026-10-22 09:00' }];
  state.overview = overview({ overrides: ov });
  await openManage('ro=1');
  eq(await ev('return document.querySelectorAll("#duty-overrides .duty-ops button").length'), 0);
  eq(await texts('#duty-shifts tbody tr').then((a) => a.length), 3, '列表仍然能看');
});

await t('窄屏（1000 / 800 / 600 / 375）：页面没有横向滚动条，表格在卡片里横向滚动；弹窗不超出窗口', async () => {
  const ov = [{ id: 5, user: 'wangwu', display: '王五', active: true, missing: false, start: '2026-10-22 19:00', end: '2026-10-23 00:00', start_ts: 1, end_ts: 2, reason: '请假请假请假请假请假请假请假', canceled: false, state: 'upcoming', created_by: 'root', created_at: '2026-10-22 09:00' }];
  state.overview = overview({ overrides: ov });
  for (const w of [1000, 800, 600, 375]) {
    await openManage('', w, 800);
    const r = await ev('return { hs: document.documentElement.scrollWidth - innerWidth, page: (() => { const p = document.querySelector(".page"); return p.scrollWidth - p.clientWidth; })() };');
    truthy(r.hs <= 0 && r.page <= 0, `宽 ${w} 页面不应横向滚动 ${J(r)}`);
    await click('#duty-shift-new');
    const m = await ev('const b = document.querySelector("#duty-sm .modal").getBoundingClientRect(); return { l: Math.round(b.left), r: Math.round(b.right), vw: innerWidth };');
    truthy(m.l >= 0 && m.r <= m.vw, `宽 ${w} 弹窗不超出窗口 ${J(m)}`);
    await ev('document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" })); return 1;');
  }
  await openManage('', 375, 800);
  await shot('manage-375');
});

await t('管理页英文界面', async () => {
  await openManage('lang=en');
  const now = await text('#duty-now');
  for (const s of ['Currently on duty', 'Contact: 分机 8001', 'Handover to 李四 at 19:00', 'Covered 24 hours a day', '8 h 30 min left']) has(now, s);
  lacks(now, 'zhangsan@x.example');
  eq(await texts('#duty-days button').then((a) => a.slice(0, 3)), ['Today', 'Tomorrow', '周六 10-24'.replace('周六', 'Sat')]);
  has(await text('#duty-shifts'), 'Daily');
  await click('#duty-shift-new');
  eq(await text('#duty-sm-title'), 'New shift');
  eq(await text('#duty-f-span'), '08:00 to 19:00, 11 h in total');
  await click('#duty-sm-ok');
  eq(await text('#duty-sm-msg'), 'Enter a shift name');
});

await t('管理页三套皮肤下的主要文字对比度 ≥ 4.5', async () => {
  for (const skin of ['dark', 'light', 'sepia']) {
    await openManage(`skin=${skin}`);
    const r = await ev(`${CONTRAST_JS} return ['.duty-who', '.duty-meta', '#duty-now .badge.ok', '#duty-shifts th', '#duty-shifts tbody tr td', '.duty-t', '.duty-p', '.dv-seg.p0', '.dv-seg.p1', '.dv-seg.p2', '.dv-seg.cur'].map(check);`);
    for (const x of r) truthy(x.ratio >= 4.5, `${skin} ${x.sel} 对比度 ${x.ratio}`);
  }
});

await t('全程无控制台错误 / CSP 违规', async () => { eq(errors, [], '控制台错误'); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
if (fail) console.log('失败项：\n  ' + failures.join('\n  '));
cleanup();
process.exit(fail ? 1 : 0);
