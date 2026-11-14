/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_export_merge.mjs
 * @time: 2026/10/28
 */
// 数据导出提交页“批量导出合并成一个文件”选项的回归测试：静态服务 + 假后端 + 无头 Chrome（CDP），不依赖第三方包。
// 页面模板与脚本取自 web/templates/sql_export_new.html、web/static。
// 用法：node scripts/frontend-test/run_export_merge.mjs [chrome路径]
// 环境变量：EM_HTTP_PORT（默认 8933）、EM_CDP_PORT（默认 9270）、EXPORT_SHOT（给一个 .png 路径就保存勾选后的截图）。
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
const HTTP = +(process.env.EM_HTTP_PORT || 8933);
const CDP = +(process.env.EM_CDP_PORT || 9270);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const base = `http://127.0.0.1:${HTTP}`;
// 比应用的 CSP 更严：style-src 去掉 unsafe-inline，以便发现内联样式
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'";
const MIME = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8', '.md': 'text/plain; charset=utf-8' };
const J = JSON.stringify;

// ---------------------------------------------------------------- 假后端
const state = { canMerge: false, reqs: [] };

function pageHtml() {
  const src = untranslate(fs.readFileSync(path.join(root, 'web/templates/sql_export_new.html'), 'utf8'));
  const head = (src.match(/\{\{define "head"\}\}(.*?)\{\{end\}\}/s) || [, ''])[1];
  const i = src.indexOf('{{define "content"}}');
  const rest = src.slice(i + '{{define "content"}}'.length);
  let content = rest.slice(0, rest.indexOf('{{end}}\n\n{{define'));
  content = content.replace(/\{\{if [^}]*\}\}.*?\{\{end\}\}/gs, '').replace(/\{\{[^}]*\}\}/g, ''); // 去掉服务端才有的条件块
  const scripts = (src.match(/\{\{define "scripts"\}\}(.*?)\{\{end\}\}/s) || [, ''])[1];
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="csrf-token" content="t"><title>提交数据导出工单</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${head}</head>
<body><div class="app"><div class="main">${content}</div></div>${scripts}</body></html>`;
}

async function readBody(req) {
  const chunks = [];
  for await (const c of req) chunks.push(c);
  const s = Buffer.concat(chunks).toString('utf8');
  return s ? JSON.parse(s) : undefined;
}
const json = (res, code, body) => { res.writeHead(code, { 'Content-Type': 'application/json' }); res.end(J(body)); };
const ok = (res, data) => json(res, 200, { data });

async function api(req, res, url) {
  const p = url.pathname.replace('/api/v1', '');
  const body = req.method === 'GET' ? undefined : await readBody(req);
  state.reqs.push({ method: req.method, path: url.pathname + url.search, body });
  if (p === '/button-perms/me') return ok(res, { mode: 'hide', deny_display: 'hide', superuser: false, allowed: [] });
  if (p === '/sqlworkflow/options') {
    return ok(res, {
      groups: [{ id: 1, name: '研发组', instances: [], export_instances: [{ id: 11, name: 'mssql-11', db_type: 'mssql' }, { id: 12, name: 'mssql-12', db_type: 'mssql' }] }],
      inception: false, native_allowed: true, batch: { allowed: true, types: ['mssql'], max: 100 },
      export: { allowed: true, batch_allowed: true, formats: [{ value: 'csv', label: 'CSV' }], max_rows: 10000, seconds: 60, can_unmask: false, can_merge: state.canMerge },
    });
  }
  if (/^\/sqlworkflow\/instances\/1[12]\/databases$/.test(p)) return ok(res, ['shop']);
  if (p === '/sqlworkflow/flow-preview') return ok(res, { configured: true, auto: false, group_name: '研发组', levels: [{ level: 1, group_name: '组长', approvers: [] }], warnings: [] });
  if (p === '/sqlworkflow/check') {
    return ok(res, {
      source: 'export', error_count: 0, warning_count: 0,
      rows: [
        { id: 1, instance_name: 'mssql-11', stagestatus: '行数统计完成', errlevel: 0, errormessage: 'None', sql: 'select 1', affected_rows: 2 },
        { id: 2, instance_name: 'mssql-12', stagestatus: '行数统计完成', errlevel: 0, errormessage: 'None', sql: 'select 1', affected_rows: 2 },
      ],
      instances: [{ id: 11, name: 'mssql-11', ok: true }, { id: 12, name: 'mssql-12', ok: true }],
    });
  }
  if (p === '/sqlworkflow' && req.method === 'POST') return ok(res, { workflow_id: 42, status: 'workflow_manreviewing', message: '导出工单已正常提交' });
  json(res, 404, { code: 'not_found', message: p });
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://x');
  try {
    if (url.pathname.startsWith('/api/v1/')) return await api(req, res, url);
    if (url.pathname === '/__reqs') return json(res, 200, state.reqs);
    if (url.pathname === '/__set') { state.canMerge = url.searchParams.get('can') === '1'; state.reqs.length = 0; return json(res, 200, {}); }
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
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'em-chrome-'));
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
const has = (text, sub, m) => { if (!String(text).includes(sub)) throw new Error(`${m || ''} 应包含 ${J(sub)}，实得 ${J(String(text).slice(0, 300))}`); };
const q = (sel, prop = 'textContent') => ev(`const e=document.querySelector(${J(sel)}); return e ? e.${prop} : null;`);
const exists = (sel) => ev(`return !!document.querySelector(${J(sel)});`);
const hidden = (sel) => ev(`const e=document.querySelector(${J(sel)}); return !e || e.hidden || !(e.offsetWidth||e.offsetHeight||e.getClientRects().length);`);
const click = (sel) => ev(`const e=document.querySelector(${J(sel)}); if(!e) throw new Error('找不到 '+${J(sel)}); e.click();`);
async function until(fn, what, ms = 5000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try { if (await fn()) return; } catch (_) { /* 继续等 */ }
    await sleep(60);
  }
  throw new Error('等待超时：' + what);
}
const reqs = async () => (await fetch(`${base}/__reqs`)).json();
const submits = async () => (await reqs()).filter((r) => r.method === 'POST' && r.path === '/api/v1/sqlworkflow');
const checks = async () => (await reqs()).filter((r) => r.method === 'POST' && r.path === '/api/v1/sqlworkflow/check');

async function openPage(canMerge) {
  await fetch(`${base}/__set?can=${canMerge ? 1 : 0}`);
  await send('Page.navigate', { url: `${base}/sqlworkflow/export/new` });
  await until(() => ev('return document.readyState==="complete"'), '页面加载');
  await until(() => ev("return document.querySelector('#wf-db').options.length > 0 && document.querySelector('#wf-db').value === 'shop'"), '库列表');
}
// 打开批量并全选两个实例，等库列表出来
async function pickBatch() {
  await click('#wf-batch');
  await until(() => ev("return !document.getElementById('wf-batch-list').hidden && document.querySelectorAll('#wf-batch-checks input').length === 2"), '批量实例列表');
  await click('#wf-batch-all');
  await until(() => ev("return document.getElementById('wf-db').value === 'shop'"), '批量库列表');
}
// 填名称与查询语句、检查，直到提交按钮可用
async function readyToSubmit() {
  await ev(`const n=document.getElementById('wf-name'); n.value='导出订单'; n.dispatchEvent(new Event('input',{bubbles:true}));
    window.ace.edit(document.getElementById('wf-editor')).setValue('select id, name from orders where id > 0', 1);`);
  await click('#wf-check');
  await until(() => ev("return !document.getElementById('wf-submit').disabled"), '检查通过');
}

console.log(`数据导出提交页“合并成一个文件”界面测试（${base}/sqlworkflow/export/new）`);

// ---------------- 服务端不提供合并（没权限 / 表缺列）----------------
await openPage(false);
await t('服务端没开放合并：批量打开后也看不到合并选项，请求里没有这个字段', async () => {
  await pickBatch();
  truthy(await hidden('#wf-merge-wrap'), '合并选项应隐藏');
  await readyToSubmit();
  await click('#wf-submit');
  await until(async () => (await submits()).length === 1, '提交请求');
  const b = (await submits())[0].body;
  eq([b.instance_ids, 'export_merge' in b, 'export_merge_col' in b], [[11, 12], false, false]);
  eq((await checks()).map((c) => 'export_merge' in c.body), [false]);
});

// ---------------- 服务端开放合并 ----------------
await openPage(true);
await t('批量没打开时看不到合并选项；打开批量后出现，默认不勾选，也没有列名输入框', async () => {
  truthy(await hidden('#wf-merge-wrap'), '单实例导出不应有合并选项');
  await pickBatch();
  truthy(!(await hidden('#wf-merge-wrap')), '批量打开后应显示合并选项');
  has(await q('#wf-merge-wrap'), '合并成一个文件');
  has(await q('#wf-merge-wrap'), '各实例返回的列必须一致');
  eq(await q('#wf-merge', 'checked'), false);
  truthy(!(await exists('#wf-merge-col')), '不应再有第一列列名输入框');
  truthy(!((await q('#wf-merge-wrap')).includes('实例名称')), '提示里不应再提实例名称列');
  if (process.env.EXPORT_SHOT) {
    await click('#wf-merge');
    await send('Emulation.setDeviceMetricsOverride', { width: 1100, height: 900, deviceScaleFactor: 1, mobile: false });
    await sleep(300);
    const shot = await send('Page.captureScreenshot', { format: 'png' });
    fs.writeFileSync(process.env.EXPORT_SHOT, Buffer.from(shot.data, 'base64'));
    await send('Emulation.clearDeviceMetricsOverride');
    await click('#wf-merge');
  }
});

await t('检查请求带 export_merge；改动合并选项后之前的检查作废', async () => {
  await click('#wf-merge');
  eq(await q('#wf-merge', 'checked'), true);
  await readyToSubmit();
  const c = await checks();
  eq([c.length, c[0].body.export_merge, c[0].body.instance_ids], [1, true, [11, 12]]);
  await click('#wf-merge'); // 取消勾选：要重新检查
  truthy(await ev("return document.getElementById('wf-submit').disabled"), '改动后提交按钮应失效');
  await click('#wf-merge');
  truthy(await ev("return document.getElementById('wf-submit').disabled"), '勾回去也要重新检查');
  await click('#wf-check');
  await until(() => ev("return !document.getElementById('wf-submit').disabled"), '重新检查通过');
  eq((await checks()).length, 2);
});

await t('提交：请求带 export_merge，不带列名之类的其它字段', async () => {
  await click('#wf-submit');
  await until(async () => (await submits()).length === 1, '提交请求');
  const b = (await submits())[0].body;
  eq([b.export_format, b.instance_ids, b.export_merge, 'export_merge_col' in b], ['csv', [11, 12], true, false]);
});

await t('没勾选合并：检查和提交的请求里都没有 export_merge', async () => {
  await openPage(true);
  await pickBatch();
  await readyToSubmit();
  await click('#wf-submit');
  await until(async () => (await submits()).length === 1, '提交请求');
  const b = (await submits())[0].body;
  eq([b.instance_ids, 'export_merge' in b], [[11, 12], false]);
  eq((await checks()).map((c) => 'export_merge' in c.body), [false]);
});

await t('勾选后又关掉批量：请求里不带合并字段', async () => {
  await openPage(true);
  await pickBatch();
  await click('#wf-merge');
  await click('#wf-batch'); // 关闭批量
  truthy(await hidden('#wf-merge-wrap'), '关闭批量后合并选项不可见');
  await until(() => ev("return document.getElementById('wf-db').value === 'shop'"), '单实例库列表');
  await readyToSubmit();
  await click('#wf-submit');
  await until(async () => (await submits()).length === 1, '提交请求');
  const b = (await submits())[0].body;
  eq([b.instance_id, 'export_merge' in b], [11, false]);
  eq((await checks()).map((c) => 'export_merge' in c.body), [false]);
});

await t('手机宽度 375：展开合并选项后没有页面级横向滚动', async () => {
  await openPage(true);
  await pickBatch();
  await click('#wf-merge');
  await send('Emulation.setDeviceMetricsOverride', { width: 375, height: 812, deviceScaleFactor: 1, mobile: true });
  await sleep(300);
  const w = await ev('return [document.documentElement.scrollWidth, window.innerWidth];');
  truthy(w[0] <= w[1] + 1, `页面出现横向滚动 ${w[0]} > ${w[1]}`);
  await send('Emulation.clearDeviceMetricsOverride');
});

await t('全程无控制台错误 / CSP 违规', async () => { eq(errors, [], '控制台错误'); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
if (fail) console.log('失败项：\n  ' + failures.join('\n  '));
cleanup();
process.exit(fail ? 1 : 0);
