/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_hostport.mjs
 * @time: 2026/10/28
 */
// 实例列表 HOST:PORT 列的浏览器回归：
//   超过 30 个字符的地址折成 30 个字符宽（省略号）+ ▸，点一下（或回车 / 空格）展开全部、变成 ▾，再点收起；
//   不超过 30 个字符的地址原样显示，没有折叠控件；
//   折叠的只是显示，单元格文字（复制 / 搜索）仍是完整地址，title 里也有完整地址；展开一行不影响其它行。
// 真实模板（去掉 Go 模板指令）+ 真实静态文件 + 假接口 + 无头 Chrome（CDP）。
// 用法：node scripts/frontend-test/run_hostport.mjs [chrome路径]    环境变量 HP_HTTP_PORT（默认 8950）/ HP_CDP_PORT（默认 9290）。
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
const HTTP = +(process.env.HP_HTTP_PORT || 8950);
const CDP = +(process.env.HP_CDP_PORT || 9290);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';

const LONG1 = 'sit-gamesaas-tenant.mysql.polardb.singapore.rds.aliyuncs.com'; // 61 个字符 + :3306
const LONG2 = 'pc-gs53z5th32x63btn6.rwlb.singapore.rds.aliyuncs.com';
const SHORT = '10.20.0.13'; // 10.20.0.13:3306 = 15 个字符
const EDGE = 'a'.repeat(25); // 25 + ":3306" = 30 个字符，刚好不折
const EDGE1 = 'a'.repeat(26); // 31 个字符，折
const ITEMS = [
  [1, 'polar-basic', LONG1, 3306],
  [2, 'polar-tenant', LONG2, 3306],
  [3, 'mysql-short', SHORT, 3306],
  [4, 'edge-30', EDGE, 3306],
  [5, 'edge-31', EDGE1, 3306],
].map(([id, instance_name, host, port]) => ({ id, instance_name, type: 'master', db_type: 'mysql', host, port, is_enable: true, disabled: false }));

function page() {
  const t = untranslate(fs.readFileSync(path.join(tplRoot, 'instmgr/list.html'), 'utf8').replace(/\r\n/g, '\n'));
  const ci = t.indexOf('{{define "content"}}');
  const si = t.indexOf('{{define "scripts"}}');
  const content = t.slice(ci + '{{define "content"}}'.length, si)
    .replace(/\{\{template "instmgr_side" \.Data\}\}/g, '')
    .replace(/\n\{\{end\}\}\s*$/, '').replace(/\{\{end\}\}\s*$/, '');
  const scripts = t.slice(si + '{{define "scripts"}}'.length).replace(/\{\{end\}\}\s*$/, '');
  return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="csrf-token" content="tok"><link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/instmgr.css"></head><body><div class="content">${content}</div>${scripts}</body></html>`;
}

const server = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x');
  const json = (obj) => { res.writeHead(200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(obj)); };
  if (u.pathname === '/instances') { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(page()); return; }
  if (u.pathname === '/api/v1/instmgr/caps') return json({ data: { 'instance.add': true, 'instance.edit': true } });
  if (u.pathname === '/api/v1/instances/form-options') return json({ data: { db_types: [{ value: 'mysql', label: 'MySQL' }], tags: [], envs: [], tunnels: [], groups: [] } });
  if (u.pathname === '/api/v1/instances') return json({ data: { items: ITEMS, total: ITEMS.length, stats: { total: ITEMS.length, disabled: 0, collecting: ITEMS.length }, ext_ready: true } });
  if (/^\/api\/v1\/instances\/\d+\/health$/.test(u.pathname)) return json({ data: { status: 'ok', version: '8.0.18' } });
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
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'hp-chrome-'));
const chrome = spawn(chromePath, ['--headless=new', `--remote-debugging-port=${CDP}`, `--user-data-dir=${profile}`, '--no-first-run', '--disable-gpu', '--window-size=1500,900', 'about:blank'], { stdio: 'ignore' });
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

await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/instances` });
for (let i = 0; i < 80; i++) {
  if (await ev("return document.querySelectorAll('#il-rows tr').length >= 5").catch(() => false)) break;
  await sleep(100);
}
await sleep(300);

// 第 n 行（从 0 数）HOST:PORT 单元格的状态
const row = (n) => ev(`const td = document.querySelectorAll('#il-rows tr')[${n}].children[5]; const hp = td.querySelector('.im-hp'); const tx = td.querySelector('.im-hp-t');
  return { text: td.textContent.replace(/[▸▾]/g, '').trim(), fold: !!hp, open: hp ? hp.classList.contains('open') : null, expanded: hp ? hp.getAttribute('aria-expanded') : null,
    chev: hp ? td.querySelector('.im-hp-x').textContent : '', title: hp ? hp.title : '', role: hp ? hp.getAttribute('role') : '', tab: hp ? hp.tabIndex : null,
    w: tx ? Math.round(tx.getBoundingClientRect().width) : Math.round(td.getBoundingClientRect().width), sw: tx ? tx.scrollWidth : 0, clipped: tx ? tx.scrollWidth > tx.clientWidth : false };`);
const click = (n) => ev(`document.querySelectorAll('#il-rows tr')[${n}].querySelector('.im-hp').click(); await new Promise((r) => setTimeout(r, 50));`);
const key = (n, k) => ev(`const e = document.querySelectorAll('#il-rows tr')[${n}].querySelector('.im-hp'); e.dispatchEvent(new KeyboardEvent('keydown', { key: ${JSON.stringify(k)}, bubbles: true, cancelable: true })); await new Promise((r) => setTimeout(r, 50));`);

let collapsedW = 0;
await t('长地址默认折叠：显示被截断（省略号）、右边有 ▸，单元格文字仍是完整地址', async () => {
  for (const [n, host] of [[0, LONG1], [1, LONG2]]) {
    const r = await row(n);
    eq([r.text, r.fold, r.open, r.expanded, r.chev, r.clipped], [`${host}:3306`, true, false, 'false', '▸', true]);
    eq([r.role, r.tab], ['button', 0]);
  }
  collapsedW = (await row(0)).w;
});
await t('折叠宽度约 30 个字符（比完整地址短一半以上）', async () => {
  const a = await row(0);
  if (!(a.sw > a.w * 1.8)) throw new Error(`完整宽度 ${a.sw} 应不小于折叠宽度 ${a.w} 的 1.8 倍`);
  eq((await row(1)).w, collapsedW, '两行折叠宽度相同（等宽字体按字符数算）');
});
await t('title 里有完整地址和“点击展开”提示', async () => {
  const r = await row(0);
  eq(r.title, `${LONG1}:3306\n点击展开完整地址`);
});
await t('短地址（15 个字符）原样显示，没有折叠控件；刚好 30 个字符也不折，31 个字符折', async () => {
  const s = await row(2);
  eq([s.text, s.fold], [`${SHORT}:3306`, false]);
  const e30 = await row(3);
  eq([e30.text, e30.fold], [`${EDGE}:3306`, false]);
  const e31 = await row(4);
  eq([e31.text, e31.fold, e31.clipped], [`${EDGE1}:3306`, true, true], '31 个字符：折叠，最后 1 个字符被省略');
});
await t('点一下展开：显示全部、▾、title 变成“点击收起”，其它行不受影响', async () => {
  await click(0);
  const r = await row(0);
  eq([r.open, r.expanded, r.chev, r.clipped, r.title], [true, 'true', '▾', false, '点击收起地址']);
  if (!(r.w >= r.sw - 1)) throw new Error('展开后宽度应放下完整地址');
  eq((await row(1)).open, false, '第二行还是折叠的');
});
await t('再点一下收起，回到折叠宽度', async () => {
  await click(0);
  const r = await row(0);
  eq([r.open, r.chev, r.clipped, r.w], [false, '▸', true, collapsedW]);
});
await t('键盘：回车展开、空格收起', async () => {
  await key(1, 'Enter');
  eq((await row(1)).open, true);
  await key(1, ' ');
  eq((await row(1)).open, false);
  await key(1, 'a');
  eq((await row(1)).open, false, '其它键不动');
});
await t('展开不会把别的列挤坏：状态列（正常徽标）仍在同一行', async () => {
  await click(0);
  const r = await ev("const tr = document.querySelectorAll('#il-rows tr')[0]; const b = tr.children[6].querySelector('.badge'); return { badge: b ? b.textContent : '', h: Math.round(tr.getBoundingClientRect().height) };");
  eq(r.badge, '正常');
  if (r.h > 90) throw new Error('行高 ' + r.h + '：展开后地址换行了');
  await click(0);
});
await t('页面零脚本异常', async () => { eq(errors, []); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
