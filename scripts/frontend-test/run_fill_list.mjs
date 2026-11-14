/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_fill_list.mjs
 * @time: 2026/10/28
 */
// 列表页“表格卡片撑满窗口”（app.css 的 .page.fill-list / .card.fill / .fill-pane）回归测试：静态服务 + 无头 Chrome（CDP），不依赖第三方包。
// 页面用的是真实模板（web/templates/*.html，去掉 Go 模板指令后放进真实外壳：侧栏 + 顶栏）和真实样式（app / admin / shell 加页面自己的 head 里的样式），
// 表格里塞示例行；页面脚本不加载（取数、渲染不是这里要测的）。逐页在几种窗口尺寸下量：卡片是否撑到窗口底部、表格是否在框里滚动、页面是否滚动。
// 用页签切换的页面（参数管理、会话诊断：几张卡片轮流显示；审计日志、通知渠道：一张卡片里几个面板轮流显示）每个页签都量一遍。
// 用法：node scripts/frontend-test/run_fill_list.mjs [chrome路径]
// 环境变量：FL_HTTP_PORT（默认 8934）、FL_CDP_PORT（默认 9271）、FILL_SHOT（给一个目录就保存每个页面撑满时的截图）。
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
const HTTP = +(process.env.FL_HTTP_PORT || 8934);
const CDP = +(process.env.FL_CDP_PORT || 9271);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const base = `http://127.0.0.1:${HTTP}`;
const MIME = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8', '.svg': 'image/svg+xml' };
const J = JSON.stringify;

// 页面清单：tpl 是模板文件；variants 是页签（card = 用 data-pane 切换的几张卡片里显示哪一张；pane = 一张卡片里显示哪个面板）
const PAGES = [
  { tpl: 'sql_list.html', name: '变更工单列表' },
  { tpl: 'sql_export_list.html', name: '数据导出列表' },
  { tpl: 'privileges.html', name: '授权清单' },
  { tpl: 'applies.html', name: '权限申请' },
  { tpl: 'instmgr/list.html', name: '实例列表' },
  { tpl: 'instmgr/accounts.html', name: '账号管理' },
  { tpl: 'instmgr/databases.html', name: '数据库管理' },
  { tpl: 'instmgr/params.html', name: '参数管理', variants: [{ card: 'vars' }, { card: 'tpl' }, { card: 'hist' }] },
  { tpl: 'instmgr/sessions.html', name: '会话诊断', variants: [{ card: 'proc' }, { card: 'space' }, { card: 'trx' }, { card: 'lock' }] },
  { tpl: 'perf/list.html', name: '问题工单' },
  { tpl: 'sysadmin/users.html', name: '用户' },
  { tpl: 'sysadmin/resource_groups.html', name: '资源组' },
  { tpl: 'sysadmin/audit.html', name: '审计日志', variants: [{ pane: 'log' }, { pane: 'wf' }, { pane: 'ql' }] },
  { tpl: 'notify_settings.html', name: '通知渠道', variants: [{ pane: 'list' }, { pane: 'logs' }] },
  { tpl: 'partman/index.html', name: '分区维护' },
  { tpl: 'partman/logs.html', name: '分区维护日志' },
  { tpl: 'env/index.html', name: '环境设置' },
];

// ---------------------------------------------------------------- 页面骨架（真实模板 + 真实外壳）
const SIDENAV = '<aside class="sidenav"><h4>实例管理</h4><a href="#" class="on">实例列表</a></aside>';
function strip(s) {
  return untranslate(s)
    .replace(/\{\{\/\*[\s\S]*?\*\/\}\}/g, '')
    .replace(/\{\{-?\s*template "instmgr_side"[^}]*\}\}/g, SIDENAV)
    .replace(/\{\{-?[\s\S]*?-?\}\}/g, '');
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
function pageHtml(tpl) {
  const src = fs.readFileSync(path.join(tplRoot, tpl), 'utf8');
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>${tpl}</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${strip(define(src, 'head'))}</head>
<body><div class="app"><aside class="side"><a class="logo" href="#"><i>DB</i></a></aside><div class="main"><header class="topbar"><b>顶栏</b></header>
${strip(define(src, 'content'))}
</div></div><script type="module" src="/static/js/shell/fillscroll.js"></script></body></html>`;
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/p') {
    res.writeHead(200, { 'Content-Type': MIME['.html'] });
    return res.end(pageHtml(url.searchParams.get('t')));
  }
  if (url.pathname.startsWith('/static/')) {
    const file = path.join(staticRoot, decodeURIComponent(url.pathname.slice(8)));
    if (file.startsWith(staticRoot) && fs.existsSync(file)) {
      res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream', 'Cache-Control': 'no-store' });
      return fs.createReadStream(file).pipe(res);
    }
  }
  res.writeHead(404); res.end('nf');
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));

// ---------------------------------------------------------------- 无头 Chrome
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'fl-chrome-'));
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
    errors.push('exception: ' + d.params.exceptionDetails.text);
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

let pass = 0, fail = 0;
const failures = [];
async function t(name, fn) {
  try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; failures.push(name); console.log('  FAIL ' + name + '\n       ' + e.message); }
}
const eq = (a, b, m) => { if (J(a) !== J(b)) throw new Error(`${m || ''} 期望 ${J(b)} 实得 ${J(a)}`); };
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };
const near = (a, b, tol, m) => { if (Math.abs(a - b) > tol) throw new Error(`${m || ''} 期望约 ${b}（±${tol}）实得 ${a}`); };

// 打开页面并切到要测的页签；rows = 给显示着的那张表塞多少行示例数据
async function open(pg, variant, rows, w, h, mobile = false) {
  await send('Emulation.setDeviceMetricsOverride', { width: w, height: h, deviceScaleFactor: 1, mobile });
  await send('Page.navigate', { url: `${base}/p?t=${encodeURIComponent(pg.tpl)}` });
  for (let i = 0; i < 80; i++) {
    if (await ev('return document.readyState === "complete"')) break;
    await sleep(50);
  }
  await ev(`
    const v = ${J(variant || {})};
    const page = document.querySelector('.main > main.page, .main > .with-side > main.page');
    // 几张卡片用页签轮流显示：只留要测的那张
    if (v.card) page.querySelectorAll(':scope > .card.fill[data-pane]').forEach((c) => { c.hidden = c.dataset.pane !== v.card; });
    // 一张卡片里几个面板轮流显示
    if (v.pane) page.querySelectorAll('.card.fill > .fill-pane').forEach((p) => { p.hidden = p.dataset.pane !== v.pane; });
    // 给显示着的表塞示例行（原来的“加载中…”占位行换掉）
    const card = [...page.querySelectorAll(':scope > .card.fill')].find((c) => !c.hidden);
    const wrap = [...card.querySelectorAll('.grid-wrap')].find((g) => g.offsetParent !== null);
    const tb = wrap.querySelector('table').tBodies[0] || wrap.querySelector('table').createTBody();
    const cols = wrap.querySelector('thead tr') ? wrap.querySelector('thead tr').cells.length : 8;
    tb.replaceChildren();
    for (let i = 0; i < ${rows}; i++) { const tr = document.createElement('tr'); for (let c = 0; c < cols; c++) { const td = document.createElement('td'); td.textContent = '示例 ' + (i + 1) + '-' + (c + 1); tr.append(td); } tb.append(tr); }`);
  // 等字体加载完、版面稳定再量（第一次打开页面时字体还在加载，顶部的筛选栏可能多换一行，高度会变）
  await ev('await document.fonts.ready; await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));');
  await sleep(150);
}
const measure = () => ev(`
  const r = (e) => { const b = e.getBoundingClientRect(); return { top: Math.round(b.top), bottom: Math.round(b.bottom), h: Math.round(b.height) }; };
  const page = document.querySelector('.main > main.page, .main > .with-side > main.page');
  const cards = [...page.querySelectorAll(':scope > .card.fill')].filter((c) => !c.hidden);
  const card = cards[0]; const gw = [...card.querySelectorAll('.grid-wrap')].find((g) => g.offsetParent !== null);
  return { vh: innerHeight, gutter: parseFloat(getComputedStyle(page).paddingLeft), nCards: cards.length, card: r(card), grid: r(gw), page: { sh: page.scrollHeight, ch: page.clientHeight },
    cardScroll: { sh: card.scrollHeight, ch: card.clientHeight }, gridScroll: { sh: gw.scrollHeight, ch: gw.clientHeight }, maxH: getComputedStyle(gw).maxHeight,
    hs: document.documentElement.scrollWidth - innerWidth, hasTable: !!gw.querySelector('table.list') };`);

console.log(`列表页“表格卡片撑满窗口”测试（真实模板 + 真实样式，${base}/p）`);

for (const pg of PAGES) {
  for (const variant of pg.variants || [{}]) {
    const label = pg.name + (variant.card ? `（${variant.card}）` : variant.pane ? `（${variant.pane}）` : '');
    await t(`${label}：结构 + 窗口足够高时卡片撑到页面底部（只留和左右一样宽的边距），表格占满剩余高度，页面不滚动`, async () => {
      await open(pg, variant, 3, 1400, 1000);
      const m = await measure();
      eq([m.nCards, m.hasTable], [1, true], '同一时间只显示一张撑满的卡片，卡片里有 .grid-wrap > table.list');
      near(m.vh - m.card.bottom, m.gutter, 2, '卡片下面的留白应与左右边距一样');
      truthy(m.vh - m.card.bottom <= 24, `卡片下面的留白不应超过 24px，实得 ${m.vh - m.card.bottom}`);
      truthy(m.grid.h >= 240, `表格区域不应小于 240，实得 ${m.grid.h}`);
      truthy(m.page.sh <= m.page.ch + 1, `页面不应滚动 ${J(m.page)}`);
      eq(m.maxH, 'none', '不再限制 52vh');
      if (process.env.FILL_SHOT && !variant.card && !variant.pane) {
        fs.mkdirSync(process.env.FILL_SHOT, { recursive: true });
        fs.writeFileSync(path.join(process.env.FILL_SHOT, pg.tpl.replace(/[\\/]/g, '_') + '.png'), Buffer.from((await send('Page.captureScreenshot', { format: 'png' })).data, 'base64'));
      }
    });
    await t(`${label}：行很多时卡片高度不变，行在表格区域里滚动，页面不滚动`, async () => {
      await open(pg, variant, 3, 1400, 1000);
      const few = await measure();
      await open(pg, variant, 80, 1400, 1000);
      const many = await measure();
      eq([many.card.h, many.grid.h], [few.card.h, few.grid.h], '行数不影响卡片和表格区域的高度');
      truthy(many.gridScroll.sh > many.gridScroll.ch * 1.5, `表格区域内部应出现滚动 ${J(many.gridScroll)}`);
      truthy(many.page.sh <= many.page.ch + 1, `页面不应滚动 ${J(many.page)}`);
    });
  }
}

await t('窗口很矮（1400×520）：每个页面的卡片保持最小高度 460，表格区域不小于 240，由页面（或卡片自己）滚动，没有内容溢出到卡片外', async () => {
  for (const pg of PAGES) {
    for (const variant of pg.variants || [{}]) {
      await open(pg, variant, 80, 1400, 520);
      const m = await measure();
      const label = pg.name + (variant.card || variant.pane || '');
      truthy(m.card.h >= 460, `${label}: 卡片最小高度 ${m.card.h}`);
      truthy(m.grid.h >= 240, `${label}: 表格区域最小高度 ${m.grid.h}`);
      truthy(m.page.sh > m.page.ch + 1 || m.cardScroll.sh > m.cardScroll.ch + 1, `${label}: 窗口太矮时页面或卡片要能滚动 ${J({ page: m.page, card: m.cardScroll })}`);
    }
  }
});

await t('窗口变高变矮：表格区域随之伸缩（取授权清单、审计日志、参数管理各量一次）', async () => {
  for (const [name, variant] of [['授权清单', {}], ['审计日志', { pane: 'log' }], ['参数管理', { card: 'vars' }]]) {
    const pg = PAGES.find((p) => p.name === name);
    await open(pg, variant, 80, 1400, 900);
    const a = await measure();
    await open(pg, variant, 80, 1400, 1200);
    const b = await measure();
    near(b.grid.h - a.grid.h, 300, 2, `${name}: 窗口高 300 表格区域就高 300`);
  }
});

await t('手机宽度 375：每个页面都没有页面级横向滚动', async () => {
  for (const pg of PAGES) {
    for (const variant of pg.variants || [{}]) {
      await open(pg, variant, 20, 375, 812, true);
      const m = await measure();
      eq(m.hs <= 1, true, `${pg.name}${variant.card || variant.pane || ''}: 页面出现横向滚动 ${m.hs}`);
    }
  }
});

await t('翻页、改筛选条件、点表头排序后表格回到顶部；表格里的勾选框、弹窗里的输入、自动刷新重画不会把表格拉回顶部', async () => {
  const pg = PAGES.find((p) => p.name === '问题工单');
  await open(pg, {}, 80, 1400, 900);
  const r = await ev(`
    const page = document.querySelector('.main > .with-side > main.page, .main > main.page');
    const card = page.querySelector(':scope > .card.fill'); const gw = card.querySelector('.grid-wrap');
    card.querySelector('.pager').innerHTML = '<button class="btn" id="t-next" type="button">下一页</button>';
    const th = gw.querySelector('thead th'); th.innerHTML = '<button class="sort" id="t-sort" type="button">排序</button>';
    const cb = document.createElement('input'); cb.type = 'checkbox'; gw.querySelector('tbody td').append(cb);
    const modal = document.createElement('div'); modal.className = 'modal-mask'; modal.innerHTML = '<div class="modal"><input type="text" id="t-modal-in"></div>'; page.append(modal);
    const down = () => { gw.scrollTop = 400; return gw.scrollTop; };
    const fire = (el, type) => el.dispatchEvent(new Event(type, { bubbles: true }));
    const out = { canScroll: down() };
    document.getElementById('t-next').click(); out.next = gw.scrollTop;
    down(); document.getElementById('t-sort').click(); out.sort = gw.scrollTop;
    down(); fire(page.querySelector('.page-head select'), 'change'); out.filterChange = gw.scrollTop;
    down(); fire(page.querySelector('.page-head input[type=search]'), 'input'); out.filterInput = gw.scrollTop;
    down(); fire(page.querySelector('form'), 'submit'); out.filterSubmit = gw.scrollTop;
    down(); fire(cb, 'change'); out.rowCheckbox = gw.scrollTop;
    down(); fire(document.getElementById('t-modal-in'), 'input'); out.modalInput = gw.scrollTop;
    down(); const tb = gw.querySelector('tbody'); tb.replaceChildren(...[...tb.rows]); out.refresh = gw.scrollTop;
    return out;`);
  truthy(r.canScroll > 0, `表格要能滚动才有意义 ${J(r)}`);
  eq([r.next, r.sort, r.filterChange, r.filterInput, r.filterSubmit], [0, 0, 0, 0, 0], '翻页 / 排序 / 筛选后回到顶部');
  eq([r.rowCheckbox > 0, r.modalInput > 0, r.refresh > 0], [true, true, true], '表格里的勾选框、弹窗里的输入、自动刷新重画不动滚动位置');
});

await t('没加 fill-list / fill 的页面不受影响：.grid-wrap 仍然最高 52vh，卡片按内容高度', async () => {
  await send('Emulation.setDeviceMetricsOverride', { width: 1400, height: 1000, deviceScaleFactor: 1, mobile: false });
  await send('Page.navigate', { url: `${base}/p?t=${encodeURIComponent('sql_list.html')}` });
  await sleep(500);
  const m = await ev(`
    const page = document.querySelector('main.page');
    page.classList.remove('fill-list');
    page.querySelector('.card').classList.remove('fill');
    const gw = page.querySelector('.grid-wrap'); const tb = gw.querySelector('table').tBodies[0]; tb.replaceChildren();
    for (let i = 0; i < 80; i++) { const tr = document.createElement('tr'); for (let c = 0; c < 8; c++) { const td = document.createElement('td'); td.textContent = '行 ' + i; tr.append(td); } tb.append(tr); }
    await new Promise((r) => setTimeout(r, 100));
    return { maxH: getComputedStyle(gw).maxHeight, h: Math.round(gw.getBoundingClientRect().height), display: getComputedStyle(page).display };`);
  eq([m.maxH, m.display], ['520px', 'block'], '去掉类之后恢复原样');
  truthy(m.h <= 521, `最高 52vh，实得 ${m.h}`);
});

await t('全程无控制台错误 / CSP 违规', async () => { eq(errors, [], '控制台错误'); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
if (fail) console.log('失败项：\n  ' + failures.join('\n  '));
cleanup();
process.exit(fail ? 1 : 0);
