/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_flowprev.mjs
 * @time: 2026/10/28
 */
// 提交页（SQL 上线 / 数据导出共用）审批流预览的浏览器回归：只显示“第 N 级 · 审批组”，可审批人员在鼠标悬停提示里，无人可审标红。
// 最小页面 + 真实静态文件 + 假接口 + 无头 Chrome（CDP）。
// 用法：node scripts/frontend-test/run_flowprev.mjs [chrome路径]    环境变量 FP_HTTP_PORT（默认 8929）/ FP_CDP_PORT（默认 9266）。
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const staticRoot = path.resolve(here, '../../web/static');
const HTTP = +(process.env.FP_HTTP_PORT || 8929);
const CDP = +(process.env.FP_CDP_PORT || 9266);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';

const FLOWS = {
  1: { group_id: 1, group_name: 'SIT上线', configured: true, auto: false, warnings: [], levels: [
    { level: 1, group_id: 10, group_name: 'DBA', approver_count: 3, approvers: [
      { username: 'nick', display: 'nick', super: true }, { username: 'marcus', display: 'marcus', super: true }, { username: 'dba', display: '张三' }] },
  ] },
  2: { group_id: 2, group_name: '支付组', configured: true, auto: false, warnings: ['第 2 级「值班」：没有符合条件的审批人'], levels: [
    { level: 1, group_id: 10, group_name: '组长', approver_count: 1, approvers: [{ username: 'leader', display: '李组长' }] },
    { level: 2, group_id: 11, group_name: '值班', approver_count: 0, approvers: [], warning: '没有符合条件的审批人（资源组内属于「值班」且有 sql_review 权限的启用用户）。仅超级用户可以审批' },
  ] },
  3: { group_id: 3, group_name: '未配置组', configured: false, auto: false, warnings: ['该资源组还没有配置 SQL 上线审批流，提交会被拒绝，请联系管理员。'], levels: [] },
  4: { group_id: 4, group_name: '空流程组', configured: true, auto: true, warnings: [], levels: [] },
};
// 页里有两块：#b 放芯片；#n 是提示区（SQL 上线提交页把提示条放到页头下面整行）。show 不传提示区（数据导出提交页的用法），show2 传。
const PAGE = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/sqlflow.css"></head>
<body><div id="b"></div><div id="n"></div><script type="module">
import { showFlow } from '/static/js/sqlflow/flowprev.js';
window.show = async (g) => { await showFlow(document.getElementById('b'), g); };
window.show2 = async (g) => { await showFlow(document.getElementById('b'), g, document.getElementById('n')); };
window.ready = true;
</script></body></html>`;

const server = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x');
  if (u.pathname === '/') { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(PAGE); return; }
  if (u.pathname === '/api/v1/sqlworkflow/flow-preview') {
    const f = FLOWS[u.searchParams.get('group_id')];
    if (!f) { res.writeHead(500, { 'Content-Type': 'application/json' }); res.end(JSON.stringify({ code: 'internal', message: '读取失败啦' })); return; }
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ data: f }));
    return;
  }
  const f = path.join(staticRoot, u.pathname.replace(/^\/static\//, ''));
  if (u.pathname.startsWith('/static/') && f.startsWith(staticRoot) && fs.existsSync(f) && fs.statSync(f).isFile()) {
    res.writeHead(200, { 'Content-Type': f.endsWith('.js') ? 'text/javascript' : f.endsWith('.css') ? 'text/css' : 'text/plain' });
    res.end(fs.readFileSync(f));
    return;
  }
  res.writeHead(404);
  res.end('nf');
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'fp-chrome-'));
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
    errors.push(d.params.exceptionDetails.text);
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
await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/` });
for (let i = 0; i < 60; i++) { if (await ev('return !!window.ready').catch(() => false)) break; await sleep(100); }
console.log('审批流预览（悬停显示人员）测试');

await t('只显示“第 1 级 · DBA”，页面文字里没有人员名字', async () => {
  await ev('await window.show(1);');
  const nodes = await ev("return [...document.querySelectorAll('#b .lv')].map(n=>n.textContent);");
  eq(nodes, ['提交', '第 1 级 · DBA', '审核通过，可执行']);
  const text = await ev("return document.getElementById('b').textContent;");
  for (const n of ['nick', 'marcus', '张三']) if (text.includes(n)) throw new Error('页面文字里不应出现人员：' + n);
});
await t('鼠标悬停（title）显示可审批人员；可聚焦', async () => {
  const x = await ev("const n=[...document.querySelectorAll('#b .lv')][1]; return { title: n.title, tab: n.tabIndex, aria: n.getAttribute('aria-label') };");
  eq(x.title, '可审批人员（3）：nick（超级用户）、marcus（超级用户）、张三');
  eq(x.tab, 0);
  if (!x.aria.includes('第 1 级 DBA')) throw new Error('aria-label：' + x.aria);
});
await t('某一级无人可审：标红并写“无人可审”，悬停是原因', async () => {
  await ev('await window.show(2);');
  const x = await ev("const ns=[...document.querySelectorAll('#b .lv')]; return { texts: ns.map(n=>n.textContent), cls: ns.map(n=>n.className), tip: ns[2].title, first: ns[1].title };");
  eq(x.texts, ['提交', '第 1 级 · 组长', '第 2 级 · 值班：无人可审', '审核通过，可执行']);
  eq(x.cls[2].includes('bad'), true);
  eq(x.cls[1].includes('bad'), false);
  if (!x.tip.includes('没有符合条件的审批人')) throw new Error('原因提示：' + x.tip);
  eq(x.first, '可审批人员（1）：李组长');
});
await t('不传提示区（数据导出提交页）：无人可审的提示条还和芯片放在一起', async () => {
  await ev('await window.show(2);');
  const x = await ev("return { alerts: document.querySelectorAll('#b .alert.warn').length, text: document.querySelector('#b .alert.warn').textContent, note: document.getElementById('n').children.length };");
  eq(x.alerts, 1);
  eq(x.note, 0);
  if (!x.text.includes('第 2 级「值班」')) throw new Error('提示条文字：' + x.text);
});
await t('首尾两个芯片带 start / end 类（页头里的药丸步骤条用来画蓝点和绿色 ✓）', async () => {
  await ev('await window.show(1);');
  const cls = await ev("return [...document.querySelectorAll('#b .lv')].map(n => n.className);");
  eq(cls.map((c) => c.includes('start')), [true, false, false]);
  eq(cls.map((c) => c.includes('end')), [false, false, true]);
});

console.log('审批流预览（传提示区：芯片在 #b，提示条在 #n）测试');
await t('正常流程：芯片在 #b，提示区是空的', async () => {
  await ev('await window.show2(1);');
  const x = await ev("return { lv: [...document.querySelectorAll('#b .lv')].map(n => n.textContent), alerts: document.querySelectorAll('#b .alert').length, note: document.getElementById('n').children.length };");
  eq(x.lv, ['提交', '第 1 级 · DBA', '审核通过，可执行']);
  eq([x.alerts, x.note], [0, 0]);
});
await t('某级无人可审：芯片仍标红，整句提示条在提示区而不在芯片区', async () => {
  await ev('await window.show2(2);');
  const x = await ev("return { lv: [...document.querySelectorAll('#b .lv')].map(n => n.textContent), bad: document.querySelectorAll('#b .lv.bad').length, alerts: document.querySelectorAll('#b .alert').length, note: [...document.querySelectorAll('#n .alert.warn')].map(n => n.textContent) };");
  eq(x.lv, ['提交', '第 1 级 · 组长', '第 2 级 · 值班：无人可审', '审核通过，可执行']);
  eq([x.bad, x.alerts], [1, 0]);
  eq(x.note, ['第 2 级「值班」：没有符合条件的审批人']);
});
await t('没配置审批流：红色提示在提示区，芯片区是空的', async () => {
  await ev('await window.show2(3);');
  const x = await ev("return { box: document.getElementById('b').children.length, note: [...document.querySelectorAll('#n .alert.err')].map(n => n.textContent) };");
  eq(x.box, 0);
  eq(x.note, ['该资源组还没有配置 SQL 上线审批流，提交会被拒绝，请联系管理员。']);
});
await t('流程为空（直接通过）：提示在提示区', async () => {
  await ev('await window.show2(4);');
  const x = await ev("return { box: document.getElementById('b').children.length, note: [...document.querySelectorAll('#n .alert.info')].map(n => n.textContent) };");
  eq(x.box, 0);
  eq(x.note, ['资源组「空流程组」的审批流为空：提交后系统直接审核通过，不需要人工审批。']);
});
await t('读取失败：提示在提示区，芯片区清空；换回正常流程后提示区清空', async () => {
  await ev('await window.show2(9);');
  let x = await ev("return { box: document.getElementById('b').children.length, note: [...document.querySelectorAll('#n .alert.warn')].map(n => n.textContent) };");
  eq(x.box, 0);
  eq(x.note, ['审批流读取失败：读取失败啦']);
  await ev('await window.show2(1);');
  x = await ev("return { lv: document.querySelectorAll('#b .lv').length, note: document.getElementById('n').children.length };");
  eq([x.lv, x.note], [3, 0]);
});
await t('没选资源组：两块都清空', async () => {
  await ev("await window.show2('');");
  const x = await ev("return [document.getElementById('b').children.length, document.getElementById('n').children.length];");
  eq(x, [0, 0]);
});
await t('控制台零报错', async () => { eq(errors, []); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
