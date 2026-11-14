/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_export_layout.mjs
 * @time: 2026/10/28
 */
// 数据导出提交页页头布局的回归测试：审批流预览在页头里（紧跟标题，右边是“导出工单列表”按钮），提示条在页头下面整行。
// 静态服务 + 假后端 + 无头 Chrome（CDP），不依赖第三方包；页面模板与脚本取自 web/templates/sql_export_new.html、web/static。
// 用法：node scripts/frontend-test/run_export_layout.mjs [chrome路径]
// 环境变量：EL_HTTP_PORT（默认 8936）、EL_CDP_PORT（默认 9279）、EL_SHOT_DIR（给一个目录就把各宽度的截图存进去）。
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
const HTTP = +(process.env.EL_HTTP_PORT || 8936);
const CDP = +(process.env.EL_CDP_PORT || 9279);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const base = `http://127.0.0.1:${HTTP}`;
// 比应用的 CSP 更严：style-src 去掉 unsafe-inline，以便发现内联样式
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'";
const MIME = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8', '.md': 'text/plain; charset=utf-8' };
const J = JSON.stringify;

// ---------------------------------------------------------------- 假后端
const FLOWS = {
  one: { configured: true, auto: false, group_name: '研发组', levels: [{ level: 1, group_name: 'TL', approvers: [{ display: '张三' }] }], warnings: [] },
  two: { configured: true, auto: false, group_name: '研发组', levels: [{ level: 1, group_name: 'TL', approvers: [{ display: '张三' }] }, { level: 2, group_name: '部门负责人（华东区交易平台）', approvers: [{ display: '李四' }] }], warnings: [] },
  bad: { configured: true, auto: false, group_name: '研发组', levels: [{ level: 1, group_name: 'TL', approvers: [], warning: '第 1 级审批组没有可审批的人' }], warnings: ['第 1 级审批组没有可审批的人'] },
  auto: { configured: true, auto: true, group_name: '研发组', levels: [], warnings: [] },
  none: { configured: false, auto: false, group_name: '研发组', levels: [], warnings: [] },
};
const state = { flow: 'one' };

function pageHtml() {
  const src = untranslate(fs.readFileSync(path.join(root, 'web/templates/sql_export_new.html'), 'utf8'));
  const head = (src.match(/\{\{define "head"\}\}(.*?)\{\{end\}\}/s) || [, ''])[1];
  const i = src.indexOf('{{define "content"}}');
  const rest = src.slice(i + '{{define "content"}}'.length);
  let content = rest.slice(0, rest.indexOf('{{end}}\n\n{{define'));
  // “导出工单列表”按钮在 {{if .Data.CanViewList}} 里，布局要按有这个按钮来量，所以只拿掉条件标记；其余条件块去掉
  content = content.replace(/\{\{if \.Data\.CanViewList\}\}(.*?)\{\{end\}\}/gs, '$1').replace(/\{\{if [^}]*\}\}.*?\{\{end\}\}/gs, '').replace(/\{\{[^}]*\}\}/g, '');
  const scripts = (src.match(/\{\{define "scripts"\}\}(.*?)\{\{end\}\}/s) || [, ''])[1];
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="csrf-token" content="t"><title>提交数据导出工单</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${head}</head>
<body><div class="app"><div class="main">${content}</div></div>${scripts}</body></html>`;
}

const json = (res, code, body) => { res.writeHead(code, { 'Content-Type': 'application/json' }); res.end(J(body)); };
const ok = (res, data) => json(res, 200, { data });

function api(req, res, url) {
  const p = url.pathname.replace('/api/v1', '');
  if (p === '/button-perms/me') return ok(res, { mode: 'hide', deny_display: 'hide', superuser: false, allowed: [] });
  if (p === '/sqlworkflow/options') {
    return ok(res, {
      groups: [{ id: 1, name: '研发组', instances: [], export_instances: [{ id: 11, name: 'mssql-11', db_type: 'mssql' }, { id: 12, name: 'mssql-12', db_type: 'mssql' }] }],
      inception: false, native_allowed: true, batch: { allowed: true, types: ['mssql'], max: 100 },
      export: { allowed: true, batch_allowed: true, formats: [{ value: 'csv', label: 'CSV' }], max_rows: 10000, seconds: 60, can_unmask: false, can_merge: false },
    });
  }
  if (/^\/sqlworkflow\/instances\/1[12]\/databases$/.test(p)) return ok(res, ['shop']);
  if (p === '/sqlworkflow/flow-preview') return ok(res, FLOWS[state.flow]);
  json(res, 404, { code: 'not_found', message: p });
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  try {
    if (url.pathname.startsWith('/api/v1/')) return api(req, res, url);
    if (url.pathname === '/__flow') { state.flow = url.searchParams.get('v') || 'one'; return json(res, 200, {}); }
    if (url.pathname === '/sqlworkflow/export/new') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP }); return res.end(pageHtml()); }
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
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'el-chrome-'));
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
    // Ace 编辑器（第三方库）会注入内联 style，在这里比应用更严的 CSP 下会报违规，与本页改动无关，不算页面错误
    if (!/Failed to load resource/.test(d.params.entry.text) && !(d.params.entry.url || '').includes('/static/vendor/ace/')) errors.push('log: ' + d.params.entry.text + ' ' + (d.params.entry.url || ''));
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
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };
const near = (a, b, m, d = 2) => { if (Math.abs(a - b) > d) throw new Error(`${m || ''} 相差超过 ${d}px：${a} vs ${b}`); };
async function until(fn, what, ms = 5000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try { if (await fn()) return; } catch (_) { /* 继续等 */ }
    await sleep(60);
  }
  throw new Error('等待超时：' + what);
}

const setW = (w, h = 900) => send('Emulation.setDeviceMetricsOverride', { width: w, height: h, deviceScaleFactor: 1, mobile: w < 600 });
async function open(flow = 'one', w = 1400) {
  await fetch(`${base}/__flow?v=${flow}`);
  await setW(w);
  await send('Page.navigate', { url: `${base}/sqlworkflow/export/new` });
  await until(() => ev('return document.readyState==="complete"'), '页面加载');
  await until(() => ev("return document.querySelector('#wf-db').options.length > 0 && document.querySelector('#flow-prev').children.length + document.querySelector('#flow-note').children.length > 0 && !document.querySelector('#flow-prev .muted')"), '审批流加载');
  await sleep(150);
}
const shot = async (name) => {
  if (!process.env.EL_SHOT_DIR) return;
  fs.mkdirSync(process.env.EL_SHOT_DIR, { recursive: true });
  const s = await send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(path.join(process.env.EL_SHOT_DIR, name + '.png'), Buffer.from(s.data, 'base64'));
};

// 量位置：r() 取整数矩形
const geo = () => ev(`
  const r = (e) => { if (!e) return null; const b = e.getBoundingClientRect(); return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height), r: Math.round(b.right), b: Math.round(b.bottom) }; };
  const head = document.querySelector('#exp-new .page-head');
  return {
    head: r(head), back: r(head.querySelector('a.btn.ghost')), title: r(head.querySelector('h1')), flow: r(document.getElementById('flow-prev')),
    flowShown: getComputedStyle(document.getElementById('flow-prev')).display !== 'none', flowInHead: head.contains(document.getElementById('flow-prev')),
    list: r(head.querySelector('a.btn:not(.ghost)')), chips: [...document.querySelectorAll('#flow-prev .lv')].map(r),
    note: r(document.getElementById('flow-note')), noteAlerts: [...document.querySelectorAll('#flow-note .alert')].map((a) => ({ cls: a.className, text: a.textContent })),
    headAlerts: head.querySelectorAll('.alert').length, firstCard: r(document.querySelector('#exp-new > section.card')),
    vw: innerWidth, hscroll: document.documentElement.scrollWidth - document.documentElement.clientWidth,
  };`);

console.log(`数据导出提交页页头布局（真实模板 + 真实样式和脚本，${base}/sqlworkflow/export/new）`);

await open('one', 1400);
await t('审批流在页头里，紧跟标题；原来中部的“审批流程预览”卡片没有了', async () => {
  const g = await geo();
  eq(await ev("return !document.getElementById('flow-card')"), true, '没有 #flow-card');
  eq(await ev("return [...document.querySelectorAll('#exp-new h2')].some((h) => h.textContent.includes('审批流程预览'))"), false, '页面里没有“审批流程预览”小节标题');
  truthy(g.flowInHead, '#flow-prev 在 .page-head 里');
  eq(g.chips.length, 3, '提交 / 第 1 级 / 审核通过 三个芯片');
  if (g.head.h > 40) throw new Error('页头应当是一行高，实际 ' + g.head.h);
  near(g.chips[0].y + g.chips[0].h / 2, g.title.y + g.title.h / 2, '芯片和标题垂直居中', 3);
  truthy(g.flow.x >= g.title.r, `芯片在标题右边：${g.flow.x} >= ${g.title.r}`);
  truthy(g.flow.x - g.title.r < 60, `芯片紧跟标题：间距 ${g.flow.x - g.title.r}`);
  truthy(g.flow.r <= g.list.x, `芯片不压着“导出工单列表”按钮：${g.flow.r} <= ${g.list.x}`);
  near(g.list.r, g.head.r, '“导出工单列表”按钮仍贴着页头右边缘');
  eq(g.noteAlerts, [], '正常流程没有提示条');
  await shot('export-1400-one');
});
await t('药丸步骤条：起点带蓝点，最后一步是绿色 ✓', async () => {
  const x = await ev(`const q = (s) => { const e = document.querySelector(s); const c = getComputedStyle(e); return { radius: c.borderTopLeftRadius, before: getComputedStyle(e, '::before').content }; };
    return { start: q('#flow-prev .lv.start'), end: q('#flow-prev .lv.end') };`);
  eq(x.start, { radius: '13px', before: '""' });
  eq(x.end, { radius: '13px', before: '"✓"' });
  eq(await ev("return [...document.querySelectorAll('#flow-prev .lv')].map((e) => e.textContent.replace(/\\s+/g, ' ').trim())"), ['提交', '第 1 级 · TL', '审核通过，可执行']);
  eq(await ev("return document.querySelector('#flow-prev .lv:not(.start):not(.end)').title"), '可审批人员（1）：张三', '悬停提示里有审批人');
});

await open('two', 1400);
await t('两级审批：芯片仍在标题同一行，页头一行高', async () => {
  const g = await geo();
  eq(g.chips.length, 4);
  if (g.head.h > 40) throw new Error('页头应当是一行高，实际 ' + g.head.h);
  truthy(g.flow.r <= g.list.x, `芯片不压着按钮：${g.flow.r} <= ${g.list.x}`);
  eq(g.hscroll <= 0, true, '没有横向滚动');
  await shot('export-1400-two');
});

for (const [flow, want] of [['none', 'alert err'], ['auto', 'alert info'], ['bad', 'alert warn']]) {
  await open(flow, 1400);
  await t(`流程「${flow}」：提示条在页头下面整行，不挤进页头`, async () => {
    const g = await geo();
    truthy(g.noteAlerts.length >= 1, '提示区里有提示条');
    truthy(g.noteAlerts.some((a) => a.cls === want || a.cls.startsWith(want)), `有 ${want}：${J(g.noteAlerts)}`);
    eq(g.headAlerts, 0, '页头里没有提示条');
    truthy(g.note.y >= g.head.b - 1, `提示区在页头下面：${g.note.y} >= ${g.head.b}`);
    truthy(g.note.b <= g.firstCard.y + 1, '提示区在第一张卡片上面');
    if (flow === 'bad') eq(await ev("return document.querySelectorAll('#flow-prev .lv.bad').length"), 1, '无人可审的那一级标红');
    if (flow === 'none' || flow === 'auto') eq(g.flowShown, false, '没有芯片时 #flow-prev 不占位');
    await shot('export-1400-' + flow);
  });
}

await t('换资源组 / 重新读取时提示区和芯片区同步清空再填充', async () => {
  await open('bad', 1400);
  eq(await ev("return document.querySelectorAll('#flow-note .alert').length > 0"), true);
  await fetch(`${base}/__flow?v=one`);
  await ev("const s = document.getElementById('wf-group'); s.dispatchEvent(new Event('change'));");
  await until(() => ev("return document.querySelectorAll('#flow-prev .lv').length === 3 && document.querySelectorAll('#flow-note .alert').length === 0"), '换回正常流程后提示区清空');
});

for (const w of [1100, 800, 600, 375]) {
  await open('two', w);
  await t(`宽度 ${w}：没有页面级横向滚动，芯片都在页面里`, async () => {
    const g = await geo();
    eq(g.hscroll <= 0, true, `横向滚动 ${g.hscroll}`);
    truthy(g.chips.every((c) => c.r <= g.vw && c.x >= 0), '所有芯片都在视口里：' + J(g.chips.map((c) => [c.x, c.r])));
    truthy(g.list.r <= g.vw, '“导出工单列表”按钮在视口里');
    await shot('export-' + w + '-two');
  });
}

await t('手机宽度 375：芯片换到标题下面单独一行，按钮仍在标题那一行', async () => {
  await open('one', 375);
  const g = await geo();
  truthy(g.flow.y >= g.title.b - 1, `芯片在标题下面：${g.flow.y} >= ${g.title.b}`);
  truthy(g.list.y < g.flow.y, `按钮在芯片上面：${g.list.y} < ${g.flow.y}`);
  truthy(g.flow.x <= g.title.x + 2 || g.flow.x <= g.back.x + 2, `手机上芯片左对齐：x=${g.flow.x}`);
  await shot('export-375-one');
});

await t('全程无控制台错误 / CSP 违规', async () => { eq(errors, [], '控制台错误'); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
if (fail) console.log('失败项：\n  ' + failures.join('\n  '));
cleanup();
process.exit(fail ? 1 : 0);
