/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_submit_layout.mjs
 * @time: 2026/10/28
 */
// 提交 SQL 工单页布局的浏览器回归：
//   · 审批流预览在页头里、紧跟标题（药丸步骤条），不再是单独的卡片；“没配置 / 流程为空 / 无人可审 / 读取失败”的提示条在页头下面整行；
//   · “批量提交”“执行前备份”“跳过语法检查”三个选项放在同一个框里横着排，框在工单名称右边，按表单实际宽度（auto-fit 的列数）换布局；
//   · 各项的出现规则不变（备份：SQL Server / Redis / 批量时不显示；跳过检查：Redis / 批量时不显示）；
//   · 批量提交打开后，“选择要上线的实例”这一行：标题、搜索框、全选、清空、已选数量排在同一行（搜索框在全选前），实例列表在下面；搜索和全选照常工作。
// 真实模板（去掉 Go 模板指令）+ 真实样式和脚本 + 假接口 + 无头 Chrome（CDP）。
// 用法：node scripts/frontend-test/run_submit_layout.mjs [chrome路径]    环境变量 SL_HTTP_PORT（默认 8931）/ SL_CDP_PORT（默认 9268）/ SL_SHOT_DIR（可选，存批量打开后的截图）。
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
const HTTP = +(process.env.SL_HTTP_PORT || 8931);
const CDP = +(process.env.SL_CDP_PORT || 9268);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';

const INSTS = [
  { id: 1, name: 'mysql-a', db_type: 'mysql' },
  { id: 2, name: 'mssql-a', db_type: 'mssql' },
  { id: 3, name: 'mssql-b', db_type: 'mssql' },
  { id: 4, name: 'redis-a', db_type: 'redis' },
  { id: 5, name: '(Sit) V1-2001 [SQL-Server]-(印度)', db_type: 'mssql' }, // 真实环境里常见的长实例名：批量列表里不应该折成两行
  { id: 6, name: '(Sit) V1-2007 [SQL-Server]-(越南)', db_type: 'mssql' },
];
const OPTIONS = {
  groups: [{ id: 1, name: 'g1', instances: INSTS, export_instances: [] }],
  inception: true, native_allowed: true,
  batch: { allowed: true, types: ['mssql'], max: 100 },
  export: { allowed: false, batch_allowed: false, formats: [], max_rows: 10000, seconds: 60 },
};
const lvl = (n, name, ap, warning) => ({ level: n, group_id: 10 + n, group_name: name, approver_count: ap.length, approvers: ap.map((x) => ({ username: x, display: x })), warning });
const FLOWS = {
  one: { group_id: 1, group_name: 'g1', configured: true, auto: false, warnings: [], levels: [lvl(1, 'TL', ['王强', '李梅'])] },
  three: { group_id: 1, group_name: 'g1', configured: true, auto: false, warnings: [], levels: [lvl(1, 'TL', ['王强']), lvl(2, 'DBA', ['陈浩']), lvl(3, '架构组', ['赵敏'])] },
  noone: { group_id: 1, group_name: 'g1', configured: true, auto: false, warnings: ['第 2 级「DBA」：没有符合条件的审批人'], levels: [lvl(1, 'TL', ['王强']), lvl(2, 'DBA', [], '没有符合条件的审批人')] },
  none: { group_id: 1, group_name: 'g1', configured: false, auto: false, warnings: ['该资源组还没有配置 SQL 上线审批流，提交会被拒绝，请联系管理员。'], levels: [] },
};
let flow = 'one';

function page(rel) {
  const t = untranslate(fs.readFileSync(path.join(tplRoot, rel), 'utf8').replace(/\r\n/g, '\n'));
  const ci = t.indexOf('{{define "content"}}');
  const si = t.indexOf('{{define "scripts"}}');
  const clean = (s) => s
    .replace(/\{\{if \.Data\.[\w.]+\}\}([\s\S]*?)\{\{end\}\}/g, '$1')
    .replace(/\{\{\.Data\.[\w.]+\}\}/g, 'true')
    .replace(/\n\{\{end\}\}\s*$/, '').replace(/\{\{end\}\}\s*$/, '');
  const content = clean(t.slice(ci + '{{define "content"}}'.length, si));
  const scripts = clean(t.slice(si + '{{define "scripts"}}'.length));
  return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="csrf-token" content="tok"><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css"><link rel="stylesheet" href="/static/css/sqlflow.css"></head><body><div class="content">${content}</div>${scripts}</body></html>`;
}

const server = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x');
  const json = (obj) => { res.writeHead(200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(obj)); };
  if (u.pathname === '/__flow') { flow = u.searchParams.get('v'); return json({ data: null }); }
  if (u.pathname === '/sql-new') { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(page('sql_new.html')); return; }
  if (u.pathname === '/api/v1/sqlworkflow/options') return json({ data: OPTIONS });
  if (/^\/api\/v1\/sqlworkflow\/instances\/\d+\/databases$/.test(u.pathname)) return json({ data: ['db1', 'db2'] });
  if (u.pathname === '/api/v1/sqlworkflow/flow-preview') return json({ data: FLOWS[flow] });
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
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'sl-chrome-'));
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
const near = (a, b, m, tol = 1) => { if (Math.abs(a - b) > tol) throw new Error(`${m || ''} ${a} 与 ${b} 相差超过 ${tol}`); };
const setW = async (w, h = 900) => { await send('Emulation.setDeviceMetricsOverride', { width: w, height: h, deviceScaleFactor: 1, mobile: w < 500 }); await sleep(250); };
// 环境变量 SL_SHOT_DIR 有值时，把打开批量后的页面截图存到这个目录（人工看效果用；不设置就什么都不做）
const SHOT_DIR = process.env.SL_SHOT_DIR;
const shot = async (name) => {
  if (!SHOT_DIR) return;
  fs.mkdirSync(SHOT_DIR, { recursive: true });
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(path.join(SHOT_DIR, name + '.png'), Buffer.from(r.data, 'base64'));
};

async function open(f = 'one') {
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/__flow?v=${f}` });
  await sleep(120);
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/sql-new` });
  for (let i = 0; i < 80; i++) {
    if (await ev("return document.querySelectorAll('#wf-inst option').length > 0 && !!document.querySelector('#flow-prev .sf-flowprev, #flow-note .alert')").catch(() => false)) break;
    await sleep(100);
  }
  await sleep(250);
}
const pick = (v) => ev(`const s = document.getElementById('wf-inst'); s.value = '${v}'; s.dispatchEvent(new Event('change')); await new Promise((r) => setTimeout(r, 150));`);

// 量位置：r() 取整数矩形；fields 按标签文字找表单里的字段
const geo = () => ev(`
  const r = (e) => { if (!e) return null; const b = e.getBoundingClientRect(); return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height), r: Math.round(b.right), b: Math.round(b.bottom) }; };
  const lbl = (txt) => [...document.querySelectorAll('#sql-new .form-grid > label')].find((l) => l.firstChild && l.firstChild.textContent.trim().startsWith(txt));
  const items = [...document.querySelectorAll('#wf-opts > label')].filter((x) => !x.hidden).map((x) => ({ id: x.id, ...r(x), bl: parseFloat(getComputedStyle(x).borderLeftWidth), bt: parseFloat(getComputedStyle(x).borderTopWidth) }));
  const fg = document.querySelector('#sql-new .form-grid');
  const o = document.getElementById('wf-opts');
  return {
    name: r(document.getElementById('wf-name')), group: r(lbl('资源组')), inst: r(lbl('目标实例')), db: r(lbl('数据库')), url: r(lbl('需求链接')),
    opts: r(o), optsBc: getComputedStyle(o).borderTopColor, items, head: r(document.querySelector('#sql-new .page-head')), title: r(document.querySelector('#sql-new h1')),
    flow: r(document.getElementById('flow-prev')), flowShown: getComputedStyle(document.getElementById('flow-prev')).display !== 'none',
    chips: [...document.querySelectorAll('#flow-prev .lv')].map(r), note: r(document.getElementById('flow-note')), noteAlerts: [...document.querySelectorAll('#flow-note .alert')].map((a) => ({ cls: a.className, text: a.textContent, ...r(a) })),
    grid: r(fg), cols: Math.max(1, Math.floor((fg.clientWidth + 24) / 324)), vw: innerWidth,
    hscroll: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    overflow: Math.round(Math.max(...[...document.querySelectorAll('#sql-new *')].filter((e) => e.getClientRects().length && !e.closest('.ace_editor')).map((e) => e.getBoundingClientRect().right)) - document.documentElement.clientWidth),
    chain: (() => {
      const out = [];
      for (let e = document.getElementById('wf-name'); e && e !== document.documentElement; e = e.parentElement) {
        const b = e.getBoundingClientRect();
        out.push(e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') + (typeof e.className === 'string' && e.className ? '.' + e.className.split(' ')[0] : '') + ':' + Math.round(b.x) + '..' + Math.round(b.right) + (e.scrollWidth > e.clientWidth + 1 ? '(sw' + e.scrollWidth + ')' : ''));
      }
      return out;
    })(),
    offenders: [...document.querySelectorAll('#sql-new *')].filter((e) => e.getClientRects().length && !e.closest('.ace_editor') && e.getBoundingClientRect().right > document.documentElement.clientWidth + 1)
      .slice(0, 6).map((e) => e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') + (typeof e.className === 'string' && e.className ? '.' + e.className.split(' ').join('.') : '') + '@' + Math.round(e.getBoundingClientRect().right)),
  };`);
// 这一页按表单宽度换布局；用实际列数（auto-fit：每列最窄 300px、间距 24px；视口不超过 800px 时强制 1 列）核对
const colsOf = (g) => (g.vw <= 800 ? 1 : g.cols);

console.log('提交 SQL 工单页布局（真实模板 + 真实样式和脚本）');
await setW(1675);
await open('one');
await t('审批流在页头里、紧跟标题，原来的“审批流程预览”卡片没有了', async () => {
  const g = await geo();
  eq(await ev("return !document.getElementById('flow-card')"), true, '没有 #flow-card');
  eq(await ev("return [...document.querySelectorAll('#sql-new h2')].some((h) => h.textContent.includes('审批流程预览'))"), false, '页面里没有“审批流程预览”小节标题');
  eq(g.chips.length, 3);
  eq(g.flow.x >= g.title.r && g.flow.x - g.title.r < 60, true, `审批流紧跟标题：间距 ${g.flow.x - g.title.r}`);
  if (g.head.h > 40) throw new Error('页头应当是一行高，实际 ' + g.head.h);
  near(g.chips[0].y + g.chips[0].h / 2, g.title.y + g.title.h / 2, '芯片和标题垂直居中', 3);
});
await t('药丸步骤条：起点带蓝点，最后一步是绿色 ✓，中间各级是中性色', async () => {
  const x = await ev(`const q = (s) => { const e = document.querySelector(s); const c = getComputedStyle(e); const b = getComputedStyle(e, '::before'); return { radius: c.borderTopLeftRadius, bc: c.borderTopColor, color: c.color, before: b.content, bg: b.backgroundColor }; };
    return { start: q('#flow-prev .lv.start'), mid: q('#flow-prev .lv:not(.start):not(.end)'), end: q('#flow-prev .lv.end') };`);
  eq(x.start.radius, '13px');
  eq(x.start.before, '""');
  eq(x.end.before, '"✓"');
  if (!x.end.bc.includes('47, 199, 90') && !x.end.bc.includes('0, 127, 34') && !x.end.bc.includes('58, 115, 53')) throw new Error('最后一步应该是绿色边框：' + x.end.bc);
  if (x.mid.bc === x.end.bc) throw new Error('中间各级不应和最后一步同色：' + x.mid.bc);
});
await t('4 列：名称占前 2 列，选项组占后 2 列，和下面第 3、4 列对齐；选项组和名称输入框同高同位置', async () => {
  const g = await geo();
  eq(colsOf(g) >= 4, true, '这个宽度应有 4 列：' + g.cols);
  near(g.name.r, g.inst.r, '名称框右边缘 = 第 2 列右边缘');
  eq(g.name.x, g.group.x);
  eq(g.opts.x, g.db.x, '选项组左边缘 = 第 3 列');
  near(g.opts.r, g.url.r, '选项组右边缘 = 第 4 列右边缘');
  eq([g.opts.h, g.opts.y], [g.name.h, g.name.y]);
});
await t('三个选项在同一个框里、同一行，顺序是 批量提交 / 执行前备份 / 跳过语法检查，可见的各项之间有细线', async () => {
  const g = await geo();
  eq(g.items.map((i) => i.id), ['wf-batch-wrap', 'wf-backup-wrap', 'wf-skip-wrap']);
  eq(new Set(g.items.map((i) => i.y)).size, 1);
  eq(g.items.map((i) => i.bl), [0, 1, 1]);
  eq(g.items.every((i) => i.x >= g.opts.x && i.r <= g.opts.r), true, '都在框里');
});
await t('勾上“跳过语法检查”：整个选项组变橙色提醒，取消后恢复', async () => {
  const before = (await geo()).optsBc;
  await ev("document.getElementById('wf-skip').checked = true; document.getElementById('wf-skip').dispatchEvent(new Event('change'));");
  await sleep(100);
  // 真实页面勾上会先弹高危确认；这里直接看样式：确认弹窗取消时脚本会把勾去掉，所以只验证样式规则本身
  await sleep(350);
  const mid = (await geo()).optsBc;
  await ev("document.getElementById('wf-skip').checked = false; document.getElementById('wf-skip').dispatchEvent(new Event('change'));");
  await sleep(450);
  const after = (await geo()).optsBc;
  eq(before === after, true, '取消后恢复');
  if (!mid.includes('255, 150, 38') && !mid.includes('177, 80, 0') && !mid.includes('141, 90, 15')) throw new Error('勾上后应变橙色边框：' + mid);
});
await t('实例类型换了，选项出现规则不变：SQL Server 没有备份；Redis 只剩批量提交（置灰）；打开批量后只剩批量提交', async () => {
  await pick('2');
  eq((await geo()).items.map((i) => i.id), ['wf-batch-wrap', 'wf-skip-wrap']);
  await pick('4');
  eq((await geo()).items.map((i) => i.id), ['wf-batch-wrap']);
  await pick('3');
  await ev("const c = document.getElementById('wf-batch'); c.checked = true; c.dispatchEvent(new Event('change')); await new Promise((r) => setTimeout(r, 200));");
  const g = await geo();
  eq(g.items.map((i) => i.id), ['wf-batch-wrap']);
  eq(await ev("return !document.getElementById('wf-batch-list').hidden"), true);
  await ev("const c = document.getElementById('wf-batch'); c.checked = false; c.dispatchEvent(new Event('change')); await new Promise((r) => setTimeout(r, 200));");
  await pick('1');
  eq((await geo()).items.map((i) => i.id), ['wf-batch-wrap', 'wf-backup-wrap', 'wf-skip-wrap']);
});

await t('3 列（1300 宽）：名称占第 1 列，选项组占后 2 列，底边与名称输入框对齐，三项一行', async () => {
  await setW(1300);
  const g = await geo();
  eq(colsOf(g), 3, '应有 3 列：' + g.cols);
  near(g.name.w, g.group.w, '名称框 = 一列宽');
  eq(g.opts.x, g.inst.x, '选项组从第 2 列开始');
  near(g.opts.r, g.db.r, '选项组到第 3 列右边缘');
  near(g.opts.b, g.name.b, '底边对齐');
  eq(new Set(g.items.map((i) => i.y)).size, 1);
  eq(g.hscroll <= 0, true, '没有横向滚动条');
});
await t('2 列（1000 宽）：名称一行，选项组在它下面一行，同宽，三项一行', async () => {
  await setW(1000);
  const g = await geo();
  eq(colsOf(g), 2, '应有 2 列：' + g.cols);
  if (!(g.opts.y > g.name.b)) throw new Error('选项组应在名称框下面');
  eq(g.opts.x, g.name.x);
  near(g.opts.r, g.name.r);
  eq(new Set(g.items.map((i) => i.y)).size, 1);
  eq(g.hscroll <= 0, true);
});
await t('窄屏（700 / 390 宽）：审批流换到标题下面一行并靠左，选项组在名称框下面，三项上下排，没有横向溢出', async () => {
  for (const w of [700, 390]) {
    // 手机是在窄屏下打开页面的：重新打开（搜索式下拉在初始化时按当时的宽度固定最小宽度，宽屏打开后再缩窄会撑出一截，这是它原有的行为）
    await setW(w);
    await open('one');
    const g = await geo();
    if (!(g.flow.y >= g.title.b - 2)) throw new Error(`${w}：审批流应在标题下面一行`);
    near(g.flow.x, g.head.x, `${w}：审批流和页头左边缘对齐`, 4);
    if (!(g.opts.y > g.name.b)) throw new Error(`${w}：选项组应在名称框下面`);
    const ys = g.items.map((i) => i.y);
    eq(ys[0] < ys[1] && ys[1] < ys[2], true, `${w}：三项上下排 ${ys}`);
    eq(g.items.map((i) => i.bt > 0).join(), 'false,true,true', `${w}：上下排时用横线分隔`);
    eq(g.overflow <= 0 && g.hscroll <= 0, true, `${w}：没有横向溢出（${g.overflow} / ${g.hscroll}）${g.offenders.join(' ')} 祖先链 ${g.chain.join(' > ')}`);
  }
});

console.log('批量提交打开后的“选择要上线的实例”');
// 在 SQL Server 实例上打开批量；每个宽度各打开一次页面（页面是在这个宽度下打开的），再量标题 / 搜索框 / 全选 / 清空 / 已选数量的位置
const batchOn = async () => {
  await pick('2');
  await ev("const c = document.getElementById('wf-batch'); c.checked = true; c.dispatchEvent(new Event('change')); await new Promise((r) => setTimeout(r, 250));");
};
const bgeo = () => ev(`
  const r = (s) => { const e = document.querySelector(s); const b = e.getBoundingClientRect(); return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height), r: Math.round(b.right), b: Math.round(b.bottom), cy: Math.round(b.y + b.height / 2) }; };
  // #wf-batch-list 本身不生成盒子（display: contents），表头（.lbl）和实例列表（#wf-batch-checks）各自是网格项
  return { head: r('#wf-batch-list > .lbl'), title: r('#wf-batch-list .bl-title'), search: r('#wf-batch-q'), all: r('#wf-batch-all'), none: r('#wf-batch-none'), count: r('#wf-batch-count'), checks: r('#wf-batch-checks'), grid: r('#sql-new .form-grid'),
    group: r('#wf-group'), url: r('#wf-url'),
    n: document.querySelectorAll('#wf-batch-checks label').length, itemH: Math.max(...[...document.querySelectorAll('#wf-batch-checks label')].map((l) => Math.round(l.getBoundingClientRect().height))),
    vw: innerWidth, hscroll: document.documentElement.scrollWidth - document.documentElement.clientWidth };`);
await t('宽屏（1675 / 1300 / 1000 宽）：标题、搜索框、全选、清空、已选数量在同一行，搜索框在全选前，实例列表在这一行下面', async () => {
  for (const w of [1675, 1300, 1000]) {
    await setW(w);
    await open('one');
    await batchOn();
    await shot('batch_' + w);
    const b = await bgeo();
    eq(b.n, 4, `${w}：四个 SQL Server 实例`);
    const row = [b.title, b.search, b.all, b.none, b.count];
    row.forEach((e) => near(e.cy, b.title.cy, `${w}：同一行`, 3));
    eq(row.every((e, i) => i === 0 || e.x > row[i - 1].x), true, `${w}：从左到右应是 标题 / 搜索框 / 全选 / 清空 / 已选数量，实得 ${row.map((e) => e.x)}`);
    eq([b.search.h, b.all.h, b.none.h], [32, 32, 32], `${w}：搜索框和按钮同高`);
    if (b.search.w < 180 || b.search.w > 321) throw new Error(`${w}：搜索框宽度 ${b.search.w} 应在 180–320 之间`);
    if (!(b.checks.y >= Math.max(...row.map((e) => e.b)))) throw new Error(`${w}：实例列表应在表头这一行下面`);
    if (w === 1675) near(b.head.w, 2 * ((b.grid.w - 72) / 4) + 24, `${w}：4 列时表头占 2 列`, 2);
    else near(b.head.w, b.grid.w, `${w}：列数不够时表头占满一整行`, 2);
    // 实例列表单独占一整行，左右边缘和上面的字段对齐（左 = 资源组的左边缘，右 = 表单右边缘）
    eq(b.checks.x, b.group.x, `${w}：列表左边缘和“资源组”对齐`);
    near(b.checks.w, b.grid.w, `${w}：列表占满表单宽度`, 2);
    near(b.checks.r, b.grid.r, `${w}：列表右边缘和表单右边缘对齐`, 2);
    if (w === 1675) near(b.checks.r, b.url.r, `${w}：4 列时和“需求链接”右边缘对齐`, 2);
    if (b.itemH > 26) throw new Error(`${w}：实例名被折成了两行（行高 ${b.itemH}）`);
    eq(b.hscroll <= 0, true, `${w}：没有横向滚动条`);
  }
});
// 打开批量前后，这几个字段的位置和大小必须一样（“版面不动”）；量的是可见的控件本身（搜索式下拉的输入框、select、input），不是被网格拉高的 label
const fixedRects = () => ev(`
  const r = (e) => { const b = e.getBoundingClientRect(); return [Math.round(b.x), Math.round(b.y), Math.round(b.width), Math.round(b.height)]; };
  const q = (s) => document.querySelector(s);
  const inst = q('#wf-inst-label .ss-input') || q('#wf-inst');
  return { group: r(q('#wf-group')), inst: r(inst), instShown: inst.getClientRects().length > 0, db: r(q('#wf-db')), url: r(q('#wf-url')), start: r(q('#wf-start')), end: r(q('#wf-end')),
    cols: getComputedStyle(q('#sql-new .form-grid')).gridTemplateColumns, ssMin: (q('#wf-inst-label .ss') || {style: {}}).style.minWidth };`);
await t('打开批量后版面不动：资源组、目标实例、数据库、需求链接、开始 / 结束时间的位置和大小都和没打开时一样（1675 / 1300 / 1000 / 390）', async () => {
  for (const w of [1675, 1300, 1000, 390]) {
    await setW(w, 1800); // 视口足够高：打开批量后页面变高，出现竖向滚动条会把整个表单挤窄十几像素，和版面无关
    await open('one');
    await pick('2');
    const before = await fixedRects();
    eq(before.instShown, true, `${w}：没打开批量时目标实例可见`);
    await batchOn();
    const after = await fixedRects();
    // 手机宽度下选项组是上下排的，打开批量后隐藏了“备份”“跳过语法检查”两项，选项组变矮，整个表单会整体上移一小段；这时比较各字段之间的相对位置
    const norm = (o) => (w >= 700 ? o : Object.fromEntries(Object.entries(o).map(([k, v]) => [k, Array.isArray(v) ? [v[0] - o.group[0], v[1] - o.group[1], v[2], v[3]] : v])));
    const a = norm(after);
    const b0 = norm(before);
    const tol = w >= 700 ? 0 : 1; // 手机宽度整体上移时有小数舍入，允许 1px
    try {
      for (const k of Object.keys(b0)) {
        if (Array.isArray(b0[k])) b0[k].forEach((v, i) => near(a[k][i], v, `${w}：${k}[${i}]（打开批量前后）`, tol));
        else if (k !== 'cols' && k !== 'ssMin') eq(a[k], b0[k], `${w}：${k}`);
      }
    } catch (e) {
      throw new Error(`${e.message}\n       之前 ${JSON.stringify(before)}\n       之后 ${JSON.stringify(after)}`);
    }
  }
});
await t('4 列（1675）：选择区放进日期那一行右边的空位，占第 3、4 列，搜索框 / 按钮和日期输入框对齐', async () => {
  await setW(1675);
  await open('one');
  await batchOn();
  const b = await bgeo();
  const f = await ev(`const r = (s) => { const e = document.querySelector(s); const b = e.getBoundingClientRect(); return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height), r: Math.round(b.right), b: Math.round(b.bottom), cy: Math.round(b.y + b.height / 2) }; };
    return { db: r('#wf-db'), url: r('#wf-url'), start: r('#wf-start'), end: r('#wf-end') };`);
  eq(b.head.x, f.db.x, '表头左边缘 = 第 3 列（和上面“数据库”对齐）');
  near(b.head.r, f.url.r, '表头右边缘 = 第 4 列（和上面“需求链接”对齐）', 2);
  if (!(b.head.x > f.end.r)) throw new Error('表头应在“结束时间”右边');
  near(b.search.cy, f.start.cy, '搜索框和开始时间输入框垂直对齐', 2);
  near(b.all.cy, f.start.cy, '全选按钮和开始时间输入框垂直对齐', 2);
  // 实例列表在表头下面单独占一整行，不再缩在右边两列里
  eq(b.checks.x, b.group.x, '列表左边缘 = 表单左边缘（和“资源组”对齐）');
  near(b.checks.r, f.url.r, '列表右边缘 = 表单右边缘（和“需求链接”对齐）', 2);
  if (!(b.checks.y > b.head.b)) throw new Error('列表应在表头下面');
  if (!(b.checks.y > Math.max(f.start.b, f.end.b))) throw new Error('列表应在日期那一行下面');
});
await t('手机宽度（390）：按 标题 → 搜索框 → 全选 → 清空 → 已选数量 的顺序排，放不下才换行，没有横向溢出', async () => {
  await setW(390);
  await open('one');
  await batchOn();
  await shot('batch_390');
  const b = await bgeo();
  const row = [b.title, b.search, b.all, b.none, b.count];
  eq(row.every((e, i) => i === 0 || (Math.abs(e.cy - row[i - 1].cy) <= 3 && e.x > row[i - 1].x) || e.cy > row[i - 1].cy + 3), true, `按阅读顺序排列（同一行从左到右，放不下才换到下一行），实得 ${JSON.stringify(row.map((e) => [e.x, e.cy]))}`);
  near(b.search.cy, b.title.cy, '手机上标题和搜索框也在同一行', 3);
  if (!(b.checks.y >= Math.max(...row.map((e) => e.b)))) throw new Error('实例列表应在表头下面');
  eq(b.hscroll <= 0, true, '没有横向滚动条');
  eq(b.head.r <= b.vw && b.checks.r <= b.vw, true, `实例选择区没有超出屏幕 ${b.head.r} / ${b.checks.r} / ${b.vw}`);
});
await t('搜索框和全选照常工作：输入即筛选，全选只选搜索结果，清空取消全部', async () => {
  await setW(1675);
  await open('one');
  await batchOn();
  const q = (v) => ev(`const i = document.getElementById('wf-batch-q'); i.value = '${v}'; i.dispatchEvent(new Event('input')); await new Promise((r) => setTimeout(r, 100)); return [...document.querySelectorAll('#wf-batch-checks label')].filter((l) => !l.hidden).map((l) => l.textContent.trim());`);
  const count = () => ev("return document.getElementById('wf-batch-count').textContent");
  eq(await q(''), ['mssql-a', 'mssql-b', '(Sit) V1-2001 [SQL-Server]-(印度)', '(Sit) V1-2007 [SQL-Server]-(越南)']);
  eq(await q('mssql-b'), ['mssql-b']);
  await ev("document.getElementById('wf-batch-all').click(); await new Promise((r) => setTimeout(r, 150));");
  eq(await ev("return [...document.querySelectorAll('#wf-batch-checks input:checked')].map((c) => c.value)"), ['3']);
  eq(await count(), '已选 1 / 100，匹配 1 / 4');
  await q('');
  eq(await count(), '已选 1 / 100');
  await ev("document.getElementById('wf-batch-none').click(); await new Promise((r) => setTimeout(r, 150));");
  eq(await count(), '已选 0 / 100');
});

console.log('审批流预览的各种状态');
await setW(1675);
await t('3 级流程：5 个芯片仍在页头一行里', async () => {
  await open('three');
  const g = await geo();
  eq(g.chips.length, 5);
  if (g.head.h > 40) throw new Error('页头高 ' + g.head.h);
});
await t('某级无人可审：芯片标红，整句提示是页头下面的整行条（不在页头里）', async () => {
  await open('noone');
  const g = await geo();
  eq(await ev("return document.querySelectorAll('#flow-prev .lv.bad').length"), 1);
  eq(await ev("return document.querySelectorAll('#flow-prev .alert').length"), 0);
  eq(g.noteAlerts.length, 1);
  eq(g.noteAlerts[0].cls.includes('warn'), true);
  if (!(g.noteAlerts[0].y >= g.head.b)) throw new Error('提示条应在页头下面');
  near(g.noteAlerts[0].w, g.grid.w + 48 + 2, '提示条和卡片同宽', 3);
  if (g.head.h > 40) throw new Error('页头不应被撑高：' + g.head.h);
});
await t('没配置审批流：红色提示条在页头下面，页头里没有步骤条', async () => {
  await open('none');
  const g = await geo();
  eq(g.noteAlerts.map((a) => a.cls.includes('err')), [true]);
  eq(g.flowShown, false, '没有芯片时审批流容器不占位');
  eq(g.head.h <= 40, true);
});
await t('控制台零报错', async () => { eq(errors, []); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
