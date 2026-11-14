/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_sqlfold.mjs
 * @time: 2026/10/28
 */
// SQL 工单详情页“结果表：所有列超出定义的列宽就折叠”的浏览器回归：用一个最小的 HTML（表格结构和 sql_detail.html 一致：colgroup、th / td 的 data-c，
// 只含 detail.js 需要的元素）+ 真实的 app.css / sqlflow.css / detail.js，无头 Chrome（CDP），不依赖第三方包，也不需要起整个服务。
// 覆盖：实例 / 阶段 / SQL / 信息各列放不下才折叠、放得下的不折叠且整列文字左对齐、点 ▸ 或这一行展开（列宽和别的列位置不变）、“已关闭”标记折叠时可见、
// 汇总行的信息整句显示（“保存文件”另起一行）、列头 ▸ 一键展开整列、“展开全部 / 折叠全部”管所有列、窗口变宽后重新判断、键盘操作、控制台零报错。
// 用法：node scripts/frontend-test/run_sqlfold.mjs [chrome路径]    环境变量 SF_HTTP_PORT（默认 8927）/ SF_CDP_PORT（默认 9264）。
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const staticRoot = path.resolve(here, '../../web/static');
const HTTP = +(process.env.SF_HTTP_PORT || 8927);
const CDP = +(process.env.SF_CDP_PORT || 9264);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';

const LONG = "UPDATE pay_channel\nSET ApiCode='demo-merchant-2'\n    ,PrivateKey='demo-private-key-2'\n    ,state=1\nWHERE Code ='PayChannelA' ;";
const SHORT = 'USE [shopdb]';
const ONE_LINE_SQL = 'UPDATE dbo.ProductConfig SET Enabled = 1 WHERE SiteId = 9901 AND Enabled IS NULL;'; // 单行但很长：窗口窄时放不下，宽屏放得下
const NAME_A = '(演示站) V1-9900 [SQL-Server]-(越南)';
const NAME_B = '(演示站) V1-9901 [SQL-Server]-(印度)';
const NAME_S = 'prod-order-01';
const OK = 'Execute Successfully';
const BK = 'Execute Successfully\nBackup successfully';
// 信息列：回灌 / 核对之类的多行说明（每行一天）
const MSG = Array.from({ length: 12 }, (_, i) => `2026-09-${String(9 + i).padStart(2, '0')} 删 ${i} 插 ${i} 核对 ${i} ✓`).join('\n');
const ERRMSG = "Invalid object name 'dbo.ProductConfig'. 目标库里没有这张表，请先同步表结构。"; // 单行，比信息列宽
// 批量导出的汇总行：“保存文件: xxx.zip。”要另起一行
const SUMMARY_HEAD = '批量导出汇总：共 2 个实例，成功 2 个，失败 0 个，已关闭 0 个。';
const SUMMARY_FILE = '保存文件: 3062_20261017131322_5d8fa18a.zip。';
const SUMMARY = SUMMARY_HEAD + SUMMARY_FILE;
const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;');
const row = (cls, id, inst, stage, lv, msg, sql, aff, time) => `<tr class="${cls}"><td data-c="id">${id}</td><td data-c="inst">${inst}</td><td class="nw" data-c="stage">${esc(stage)}</td><td class="nw" data-c="lv">${lv}</td>`
  + `<td class="wrap" data-c="msg">${msg ? `<div class="msgcell">${esc(msg)}</div>` : ''}</td><td class="mono wrap sqlcell" data-c="sql">${esc(sql)}</td><td class="num" data-c="aff">${aff}</td><td class="num" data-c="time">${time}</td></tr>`;
const PAGE = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/sqlflow.css"></head>
<body><main id="detail" data-id="1" data-status="workflow_finish" data-pending="false"><div id="detail-msg"></div>
<section class="card"><h2>执行结果</h2><div class="grid-wrap"><table class="list rt-inst rt-time" id="result-table">
<colgroup><col class="c-id"><col class="c-inst"><col class="c-stage"><col class="c-lv"><col class="c-msg"><col class="c-sql"><col class="c-aff"><col class="c-time"></colgroup>
<thead><tr><th data-c="id">#</th><th data-c="inst">实例</th><th class="nw" data-c="stage">阶段</th><th class="nw" data-c="lv">级别</th><th data-c="msg">信息</th><th data-c="sql">SQL</th><th data-c="aff">影响行数</th><th data-c="time">耗时(s)</th></tr></thead><tbody>
${row('', 1, esc(NAME_A), OK, '通过', '', SHORT, 0, 0)}
${row('', 2, esc(NAME_A), BK, '通过', MSG, LONG, 1, '0.005')}
${row('', 3, `${esc(NAME_B)} <small class="muted">已关闭</small>`, OK, '通过', '已回灌 3 行', ONE_LINE_SQL, 0, 0)}
${row('lv-err', 4, NAME_S, 'Execute Failed', '错误', ERRMSG, SHORT, 0, '0.002')}
${row('', 5, NAME_S, OK, '通过', '保存文件: AR062rw.csv', SHORT, 0, 0)}
${row('sum', 6, '汇总', '执行完成', '通过', SUMMARY, SHORT, 0, 0)}
</tbody></table></div></section></main>
<script type="module" src="/static/js/sqlflow/detail.js"></script></body></html>`;

const server = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x');
  if (u.pathname === '/') { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(PAGE); return; }
  if (u.pathname.startsWith('/static/')) {
    const f = path.join(staticRoot, u.pathname.slice('/static/'.length));
    if (f.startsWith(staticRoot) && fs.existsSync(f) && fs.statSync(f).isFile()) {
      const type = f.endsWith('.js') ? 'text/javascript' : f.endsWith('.css') ? 'text/css' : 'application/octet-stream';
      res.writeHead(200, { 'Content-Type': type });
      res.end(fs.readFileSync(f));
      return;
    }
  }
  res.writeHead(404);
  res.end('not found');
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));

const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'sf-chrome-'));
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
      const list = await (await fetch(`http://127.0.0.1:${CDP}/json`)).json();
      const p = list.find((x) => x.type === 'page');
      if (p) return p.webSocketDebuggerUrl;
    } catch (_) { /* 等 Chrome 起来 */ }
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
  } else if (d.method === 'Log.entryAdded' && d.params.entry.level === 'error') {
    errors.push(d.params.entry.text + ' ' + (d.params.entry.url || ''));
  }
});
const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id; pend.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
await send('Runtime.enable');
await send('Page.enable');
await send('Log.enable');
const setWidth = async (w) => { await send('Emulation.setDeviceMetricsOverride', { width: w, height: 900, deviceScaleFactor: 1, mobile: false }); await sleep(250); };
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

await setWidth(1500);
await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/` });
for (let i = 0; i < 60; i++) { if (await ev("return !!document.getElementById('result-fold-all')").catch(() => false)) break; await sleep(100); }
// 页面里的小工具：q(行, 列) 取单元格；st(行, 列) 取折叠状态；heads() 取各列表头的 x 位置
await ev(`window.q = (r, c) => document.querySelectorAll('#result-table tbody tr')[r].querySelector('td[data-c="' + c + '"]');
window.st = (r, c) => { const td = q(r, c), b = td.querySelector('.rf-t'), one = td.querySelector('.rf-one'), full = td.querySelector('.rf-full');
  return { caret: !!b && !b.hidden, expanded: b ? b.getAttribute('aria-expanded') : null, one: one && !one.hidden ? one.textContent : null, full: full && !full.hidden ? full.textContent : null,
    x: one ? Math.round(one.getBoundingClientRect().x) : null, h: Math.round(td.getBoundingClientRect().height),
    text: [...td.querySelectorAll('.rf-one, .rf-full')].filter((e) => !e.hidden).map((e) => e.textContent).join('') || td.textContent.trim() }; };
window.heads = () => [...document.querySelectorAll('#result-table thead th')].map((th) => Math.round(th.getBoundingClientRect().x) + ':' + Math.round(th.getBoundingClientRect().width)).join(' ');
window.headBtn = (c) => document.querySelector('#result-table thead th[data-c="' + c + '"] .rf-th');`);
const st = (r, c) => ev(`return st(${r}, '${c}');`);

console.log('工单详情页 结果表折叠测试（所有列超出列宽就折叠）');

await t('实例列：长名字折叠成一行带 ▸，短名字不折叠，整列文字左对齐', async () => {
  const a = await st(0, 'inst');
  eq([a.caret, a.expanded, a.one, a.full], [true, 'false', NAME_A, null], '长名字');
  if (a.h > 60) throw new Error('折叠后应只有一行，实际高度 ' + a.h);
  const s = await st(3, 'inst');
  eq([s.caret, s.one], [false, NAME_S], '短名字（放得下）不折叠，没有 ▸');
  eq(s.x, a.x, '有折叠的列里，放得下的格子也留出 ▸ 的位置，文字左对齐');
});

await t('点 ▸ 展开全名、再点 ▾ 折叠；展开时各列的位置不变', async () => {
  const before = await ev('return heads();');
  await ev("q(0, 'inst').querySelector('.rf-t').click();");
  let a = await st(0, 'inst');
  eq([a.caret, a.expanded, a.one, a.full], [true, 'true', null, NAME_A]);
  eq(await ev('return heads();'), before, '展开后列宽、列的位置不变');
  await ev("q(0, 'inst').querySelector('.rf-t').click();");
  a = await st(0, 'inst');
  eq([a.expanded, a.full], ['false', null]);
});

await t('点那一行文字也能展开', async () => {
  await ev("q(1, 'inst').querySelector('.rf-one').click();");
  eq((await st(1, 'inst')).full, NAME_A);
  await ev("q(1, 'inst').querySelector('.rf-t').click();");
});

await t('阶段列：带换行的折叠成一行，“Execute Successfully”放得下不折叠，整列左对齐', async () => {
  const bk = await st(1, 'stage');
  eq([bk.caret, bk.one], [true, 'Execute Successfully Backup successfully']);
  const ok = await st(0, 'stage');
  eq([ok.caret, ok.one], [false, OK]);
  eq(ok.x, bk.x, '整列左对齐');
  await ev("q(1, 'stage').querySelector('.rf-t').click();");
  eq((await st(1, 'stage')).full, BK, '全文原样（含换行）');
  await ev("q(1, 'stage').querySelector('.rf-t').click();");
});

await t('SQL 列：多行折叠成一行，很短的一行不折叠；展开后全文原样（含换行）', async () => {
  const s0 = await st(0, 'sql');
  eq([s0.caret, s0.text], [false, SHORT], '短 SQL 不折叠');
  const s1 = await st(1, 'sql');
  eq(s1.one, "UPDATE pay_channel SET ApiCode='demo-merchant-2' ,PrivateKey='demo-private-key-2' ,state=1 WHERE Code ='PayChannelA' ;");
  if (s1.h > 60) throw new Error('折叠后应只有一行，实际高度 ' + s1.h);
  await ev("q(1, 'sql').querySelector('.rf-t').click();");
  eq((await st(1, 'sql')).full, LONG, '全文原样（含换行）');
  await ev("q(1, 'sql').querySelector('.rf-t').click();");
  eq((await st(1, 'sql')).full, null);
});

await t('信息列：多行折叠、比列宽长的单行也折叠、很短的不折叠；展开后全文原样', async () => {
  const m1 = await st(1, 'msg');
  eq([m1.caret, m1.one], [true, MSG.replace(/\s+/g, ' ')]);
  eq((await st(2, 'msg')).caret, false, '很短的一行信息不折叠');
  eq((await st(2, 'msg')).text, '已回灌 3 行');
  const m3 = await st(3, 'msg');
  eq([m3.caret, m3.one], [true, ERRMSG], '比信息列宽的单行');
  await ev("q(1, 'msg').querySelector('.rf-t').click();");
  eq((await st(1, 'msg')).full, MSG, '全文原样（含换行）');
  await ev("q(1, 'msg').querySelector('.rf-t').click();");
  eq((await st(0, 'msg')).text, '', '没有信息的行什么都没有');
});

await t('实例名后面的“已关闭”标记折叠时也看得见', async () => {
  const r = await ev("const td = q(2, 'inst'), x = td.querySelector('.rf-x'); return x ? { text: x.textContent.trim(), inside: x.getBoundingClientRect().right <= td.getBoundingClientRect().right } : null;");
  eq(r, { text: '已关闭', inside: true });
  const a = await st(2, 'inst');
  eq([a.caret, a.one], [true, NAME_B], '名字本身照样折叠');
});

await t('汇总行：信息整句显示不折叠，“保存文件”另起一行；单实例结果开头的“保存文件: x”不拆', async () => {
  eq(await ev("return !!q(5, 'msg').querySelector('.rf');"), false, '汇总句不建折叠结构');
  const lines = await ev("return [...q(5, 'msg').querySelectorAll('.msgcell > div')].map((d) => d.textContent);");
  eq(lines, [SUMMARY_HEAD, SUMMARY_FILE], '汇总行拆成两行');
  eq(await ev("return q(5, 'msg').textContent;"), SUMMARY, '拆行不改文字');
  eq(await ev("const m = q(5, 'msg').querySelector('.msgcell'); return m.getBoundingClientRect().right <= q(5, 'msg').getBoundingClientRect().right + 1;"), true, '整句在列宽里换行，不超出信息列');
  eq((await st(4, 'msg')).caret, false, '短的“保存文件: x”不折叠');
  eq(await ev("return [...q(4, 'msg').querySelectorAll('.msgcell > div:not(.rf)')].length;"), 0, '开头就是“保存文件”的不拆');
  eq(await ev("return !!q(5, 'sql').querySelector('.rf');"), true, '汇总行里的其它列照常');
});

await t('数字列和 # 列：放得下的不建折叠结构', async () => {
  eq(await ev("return [0, 1, 2, 3].every((r) => ['id', 'lv', 'aff', 'time'].every((c) => !q(r, c).querySelector('.rf')));"), true);
});

await t('列头 ▸：有折叠内容的列才出现；点一下展开整列，再点折叠', async () => {
  const shown = await ev("return [...document.querySelectorAll('#result-table thead .rf-th')].filter((b) => !b.hidden).map((b) => b.parentNode.dataset.c);");
  eq(shown, ['inst', 'stage', 'msg', 'sql']);
  await ev("headBtn('inst').click();");
  eq(await ev("return [0, 1, 2, 3, 4, 5].map((r) => st(r, 'inst').expanded);"), ['true', 'true', 'true', null, null, null], '前三行（长名字）展开；短名字没有 ▸');
  eq(await ev("return headBtn('inst').textContent;"), '▾');
  eq(await ev("return headBtn('inst').title;"), '折叠这一列');
  await ev("headBtn('inst').click();");
  eq(await ev("return [0, 1, 2].map((r) => st(r, 'inst').expanded);"), ['false', 'false', 'false']);
  eq(await ev("return headBtn('inst').textContent;"), '▸');
});

await t('“展开全部 / 折叠全部”管所有列', async () => {
  eq(await ev("return document.getElementById('result-fold-all').textContent;"), '展开全部');
  await ev("document.getElementById('result-fold-all').click();");
  eq(await ev("return [...document.querySelectorAll('#result-table .rf.over')].every((b) => b.querySelector('.rf-t').getAttribute('aria-expanded') === 'true');"), true, '所有折叠着的都展开了');
  eq(await ev("return new Set([...document.querySelectorAll('#result-table .rf.over')].map((b) => b.parentNode.closest('td').dataset.c)).size;"), 4, '实例 / 阶段 / 信息 / SQL 四列');
  eq(await ev("return document.getElementById('result-fold-all').textContent;"), '折叠全部');
  await ev("document.getElementById('result-fold-all').click();");
  eq(await ev("return [...document.querySelectorAll('#result-table .rf.over')].every((b) => b.querySelector('.rf-t').getAttribute('aria-expanded') === 'false');"), true);
  eq(await ev("return document.getElementById('result-fold-all').textContent;"), '展开全部');
});

await t('窗口变宽后重新判断：很长的单行 SQL 在宽屏放得下，不再折叠；窗口缩回去又折叠', async () => {
  eq((await st(2, 'sql')).caret, true, '1500 宽：放不下，折叠');
  await setWidth(2600);
  eq(await ev("return [st(2, 'sql').caret, st(2, 'sql').text];"), [false, ONE_LINE_SQL], '2600 宽：放得下，原样显示');
  await setWidth(1500);
  eq((await st(2, 'sql')).caret, true, '缩回 1500 宽：又折叠');
});

await t('键盘：Tab 到 ▸ 按 Enter 展开，焦点还在按钮上，再按 Enter 折叠', async () => {
  await ev("q(0, 'inst').querySelector('.rf-t').focus();");
  const key = (type) => send('Input.dispatchKeyEvent', { type, key: 'Enter', code: 'Enter', windowsVirtualKeyCode: 13, text: type === 'keyDown' ? String.fromCharCode(13) : undefined });
  await key('keyDown'); await key('keyUp');
  await sleep(100);
  eq(await ev("const b = q(0, 'inst').querySelector('.rf-t'); return [b.getAttribute('aria-expanded'), document.activeElement === b];"), ['true', true]);
  await key('keyDown'); await key('keyUp');
  await sleep(100);
  eq(await ev("const b = q(0, 'inst').querySelector('.rf-t'); return [b.getAttribute('aria-expanded'), document.activeElement === b];"), ['false', true]);
});

await t('控制台零报错', async () => {
  eq(errors.filter((e) => !/favicon/.test(e)), []);
});

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
