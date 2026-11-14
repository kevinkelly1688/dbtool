/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_batchwrap.mjs
 * @time: 2026/10/28
 */
// 提交页（SQL 上线 / 数据导出）“批量”开关的浏览器回归：
//   数据导出提交页：默认不显示，只有单实例下拉里选中的是可批量的类型（SQL Server）才出现；
//   SQL 上线提交页：开关始终显示在选项组里，不能用时置灰、不能勾，鼠标停上去（title）提示原因——
//     实例类型不对提示“仅支持 MSSQL”，类型对但没有批量权限提示“没有批量提交的权限”；能用的条件和数据导出页一样。
//   数据导出页：已经打开批量时开关保持可用（目标实例下拉收起），关闭后回到原来选中的实例；
//   SQL 上线页：批量打开后“目标实例”仍在页面上，换成另一个 SQL Server 实例批量保持打开，换成不支持批量的类型（MySQL、Redis 等）批量自动关闭并置灰；
//   复制批量工单也要能进入批量模式。
// 真实模板（去掉 Go 模板指令）+ 真实静态文件 + 假接口 + 无头 Chrome（CDP）。
// 用法：node scripts/frontend-test/run_batchwrap.mjs [chrome路径]    环境变量 BW_HTTP_PORT（默认 8930）/ BW_CDP_PORT（默认 9267）。
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
const HTTP = +(process.env.BW_HTTP_PORT || 8930);
const CDP = +(process.env.BW_CDP_PORT || 9267);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';

const INSTS = [
  { id: 1, name: 'mysql-a', db_type: 'mysql' },
  { id: 2, name: 'mssql-a', db_type: 'mssql' },
  { id: 3, name: 'mssql-b', db_type: 'mssql' },
  { id: 4, name: 'redis-a', db_type: 'redis' },
];
const OPTIONS = {
  groups: [{ id: 1, name: 'g1', instances: INSTS, export_instances: INSTS.filter((i) => i.db_type !== 'redis') }],
  inception: false, native_allowed: true,
  batch: { allowed: true, types: ['mssql'], max: 100 },
  export: { allowed: true, batch_allowed: true, formats: [{ value: 'csv', label: 'CSV' }], max_rows: 10000, seconds: 60 },
};

function page(rel) {
  const t = untranslate(fs.readFileSync(path.join(tplRoot, rel), 'utf8').replace(/\r\n/g, '\n'));
  const ci = t.indexOf('{{define "content"}}');
  const si = t.indexOf('{{define "scripts"}}');
  const clean = (s) => s
    .replace(/\{\{if \.Data\.[\w.]+\}\}([\s\S]*?)\{\{end\}\}/g, '$1')
    .replace(/\{\{\.Data\.[\w.]+\}\}/g, 'false')
    .replace(/\n\{\{end\}\}\s*$/, '').replace(/\{\{end\}\}\s*$/, '');
  const content = clean(t.slice(ci + '{{define "content"}}'.length, si));
  const scripts = clean(t.slice(si + '{{define "scripts"}}'.length));
  return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="csrf-token" content="tok"><link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/sqlflow.css"></head><body><div class="content">${content}</div>${scripts}</body></html>`;
}

let noperm = false; // 测试里切换：没有批量权限
const server = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x');
  const json = (obj) => { res.writeHead(200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(obj)); };
  if (u.pathname === '/__noperm') { noperm = u.searchParams.get('v') === '1'; return json({ data: null }); }
  if (u.pathname === '/sql-new') { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(page('sql_new.html')); return; }
  if (u.pathname === '/sql-export') { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(page('sql_export_new.html')); return; }
  if (u.pathname === '/api/v1/sqlworkflow/options') return json({ data: { ...OPTIONS, batch: { ...OPTIONS.batch, allowed: !noperm }, export: { ...OPTIONS.export, batch_allowed: !noperm } } });
  if (/^\/api\/v1\/sqlworkflow\/instances\/\d+\/databases$/.test(u.pathname)) return json({ data: ['db1', 'db2'] });
  if (u.pathname === '/api/v1/sqlworkflow/flow-preview') return json({ data: { group_id: 1, group_name: 'g1', configured: true, auto: false, warnings: [], levels: [] } });
  // 复制一张批量工单（两个 SQL Server 实例）：页面默认选中的是第一个实例（MySQL），复制后必须还能进入批量模式
  if (u.pathname === '/api/v1/sqlworkflow/9/copy') return json({ data: { group_name: 'g1', name: 'copy-batch', demand_url: '', is_backup: false, instance_id: 2, instance_ids: [2, 3], db_name: 'db2', sql: 'update t set a = 1;' } });
  if (u.pathname === '/api/v1/button-perms/me') return json({ data: { mode: 'compat', deny_display: 'hide', superuser: true, allowed: [] } });
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
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'bw-chrome-'));
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

async function open(p) {
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}${p}` });
  for (let i = 0; i < 80; i++) {
    if (await ev("return document.querySelectorAll('#wf-inst option').length > 0 && !!document.getElementById('wf-batch-wrap')").catch(() => false)) break;
    await sleep(100);
  }
  await sleep(200);
}
const state = () => ev(`const w = document.getElementById('wf-batch-wrap'); const c = document.getElementById('wf-batch');
  return { wrap: !w.hidden, title: w.querySelector('.batch-title').textContent, on: c.checked, disabled: c.disabled, off: w.classList.contains('off'), tip: w.title,
    pe: getComputedStyle(c).pointerEvents, aria: w.getAttribute('aria-disabled'),
    list: !document.getElementById('wf-batch-list').hidden, instLabel: !document.getElementById('wf-inst-label').hidden,
    inst: document.getElementById('wf-inst').value, checks: [...document.querySelectorAll('#wf-batch-checks label')].map((l) => l.textContent.trim()),
    checked: [...document.querySelectorAll('#wf-batch-checks input:checked')].map((c) => c.value), dbs: [...document.querySelectorAll('#wf-db option')].map((o) => o.textContent), db: document.getElementById('wf-db').value };`);
const pick = (v) => ev(`const s = document.getElementById('wf-inst'); s.value = '${v}'; s.dispatchEvent(new Event('change')); await new Promise((r) => setTimeout(r, 150));`);
const toggle = () => ev("const c = document.getElementById('wf-batch'); c.checked = !c.checked; c.dispatchEvent(new Event('change')); await new Promise((r) => setTimeout(r, 150));");

// 能用：数据导出页是“显示”；SQL 上线页开关始终显示，能用 = 显示且没有置灰
const usable = (s, always) => (always ? s.wrap && !s.disabled : s.wrap);
for (const [label, url, title, always] of [['SQL 上线提交页', '/sql-new', '批量提交', true], ['数据导出提交页', '/sql-export', '批量导出', false]]) {
  console.log(label);
  await open(url);
  await t(always ? '开关始终显示，默认选中的是 MySQL 实例所以置灰，悬停提示“仅支持 MSSQL”' : '默认不显示：默认选中的是 MySQL 实例', async () => {
    const s = await state();
    eq(s.inst, '1');
    eq([usable(s, always), s.list, s.instLabel], [false, false, true]);
    if (always) {
      eq([s.wrap, s.disabled, s.off, s.aria, s.tip], [true, true, true, 'true', '仅支持 MSSQL']);
      eq(s.pe, 'none', '置灰时鼠标事件落在标签上，title 才会弹出来');
    }
  });
  await t('选中 SQL Server 实例才能用，标题是“' + title + '”', async () => {
    await pick('2');
    const s = await state();
    eq(usable(s, always), true);
    eq(s.title, title);
    eq(s.on, false);
    if (always) eq([s.disabled, s.off, s.aria, s.tip], [false, false, 'false', ''], '可用时没有提示');
  });
  if (always) {
    await t('选中 Redis 实例置灰，提示“仅支持 MSSQL”', async () => {
      await pick('4');
      const s = await state();
      eq([usable(s, always), s.tip], [false, '仅支持 MSSQL']);
    });
  }
  await t(always ? '换回 MySQL 实例又置灰' : '换回 MySQL 实例又隐藏', async () => {
    await pick('1');
    const s = await state();
    eq(usable(s, always), false);
    if (always) eq(s.tip, '仅支持 MSSQL');
  });
  await t(always ? '打开批量：只列 SQL Server 实例；“目标实例”留在原位（版面不动）；开关保持可用' : '打开批量：实例下拉收起、只列 SQL Server 实例；开关保持可用', async () => {
    await pick('3');
    await toggle();
    const s = await state();
    eq([usable(s, always), s.on, s.list, s.instLabel], [true, true, true, always]);
    eq(s.checks, ['mssql-a', 'mssql-b']);
  });
  await t('关闭批量：回到原来选中的实例（mssql-b），开关仍在', async () => {
    await toggle();
    const s = await state();
    eq([usable(s, always), s.on, s.list, s.instLabel], [true, false, false, true]);
    eq(s.inst, '3', '保持原选中的实例');
  });
  if (always) {
    // SQL 上线页批量打开后“目标实例”仍在页面上：它的类型照样决定能不能批量
    await t('批量打开后换成另一个 SQL Server 实例：批量保持打开', async () => {
      await pick('3');
      await toggle();
      eq((await state()).on, true);
      await pick('2');
      const s = await state();
      eq([s.on, s.list, s.disabled, s.off, s.tip], [true, true, false, false, ''], '换成另一个 SQL Server 实例');
    });
    for (const [id, what] of [['1', 'MySQL'], ['4', 'Redis']]) {
      await t(`批量打开后换成 ${what} 实例：批量自动关闭并置灰（提示“仅支持 MSSQL”），实例列表收起，库列表按这个实例重新加载`, async () => {
        await pick('3');
        if (!(await state()).on) await toggle();
        eq((await state()).on, true, '先打开批量');
        await pick(id);
        const s = await state();
        eq([s.on, s.list, s.disabled, s.off, s.tip], [false, false, true, true, '仅支持 MSSQL'], `换成 ${what} 实例`);
        eq([s.inst, s.dbs[0] !== '先勾选实例' && s.dbs.length > 0], [id, true], '库列表按所选实例加载，不再是“先勾选实例”');
      });
    }
    await t('换回 SQL Server 实例：批量可以再打开，打开时实例列表是新的（上次的勾选不残留）', async () => {
      await pick('3');
      const s0 = await state();
      eq([s0.disabled, s0.on], [false, false]);
      await toggle();
      const s = await state();
      eq([s.on, s.list, s.checked], [true, true, []]);
      await toggle();
    });
    // 复制批量工单：页面默认选中的是 MySQL 实例，复制出来必须仍是批量模式、勾上原来的实例
    await t('复制批量工单：进入批量模式，勾上原工单的两个实例，库选原来的', async () => {
      await open('/sql-new?copy=9');
      for (let i = 0; i < 60; i++) {
        if ((await state()).checked.length === 2) break;
        await sleep(100);
      }
      const s = await state();
      eq([s.on, s.list, s.checked, s.db], [true, true, ['2', '3'], 'db2']);
      eq((await ev("return ['mssql-a', 'mssql-b'].includes(document.getElementById('wf-inst').selectedOptions[0].textContent.split('（')[0])")), true, '目标实例指到了可批量的 SQL Server 实例');
      await open('/sql-new');
    });
  }
  await t(always ? '再换成 MySQL 实例置灰，批量始终是关闭状态' : '再换成 MySQL 实例隐藏，批量始终是关闭状态', async () => {
    await pick('1');
    const s = await state();
    eq([usable(s, always), s.on], [false, false]);
  });
  if (always) {
    await t('类型对但没有批量权限：选 SQL Server 仍置灰，提示“没有批量提交的权限”', async () => {
      await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/__noperm?v=1` });
      await sleep(150);
      await open(url);
      await pick('2');
      const s = await state();
      eq([s.wrap, s.disabled, s.off, s.tip], [true, true, true, '没有批量提交的权限']);
      await pick('1');
      eq((await state()).tip, '仅支持 MSSQL', '类型不对时还是先提示类型');
      await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/__noperm?v=0` });
      await sleep(150);
      await open(url);
    });
  }
}
await t('控制台零报错', async () => { eq(errors, []); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
