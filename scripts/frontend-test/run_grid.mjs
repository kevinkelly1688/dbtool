/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_grid.mjs
 * @time: 2026/10/28
 */
// 在线查询页面级测试：结果表格（悬停复制 / 双击预览 / 复制整列 / 键盘）、库表树右键“对象信息”、左侧“可编程对象”页签。
// 自起假后端（multi_engine_fake.mjs，页面取自真实模板与静态资源，CSP 与线上一致）+ 无头 Chrome（CDP），不依赖第三方包。
// 用法：node scripts/frontend-test/run_grid.mjs [chrome路径]    环境变量 GRID_HTTP_PORT（默认 8907）/ GRID_CDP_PORT（默认 9244）/ GRID_SHOT=截图目录。
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { startServer, calls, GRID_SCRIPT, GRID_LONG, GRID_NOTE, OBJ_DDL, FN_DEF, MS_DDL } from './multi_engine_fake.mjs';

const HTTP = +(process.env.GRID_HTTP_PORT || 8907);
const CDP = +(process.env.GRID_CDP_PORT || 9244);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const server = await startServer(HTTP);
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'grid-chrome-'));
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
const consoleErrors = [];
ws.addEventListener('message', (m) => {
  const d = JSON.parse(m.data);
  if (d.id && pend.has(d.id)) {
    const { res, rej } = pend.get(d.id);
    pend.delete(d.id);
    if (d.error) rej(new Error(d.error.message)); else res(d.result);
  } else if (d.method === 'Runtime.exceptionThrown') {
    consoleErrors.push(d.params.exceptionDetails.text + ' ' + ((d.params.exceptionDetails.exception || {}).description || ''));
  } else if (d.method === 'Log.entryAdded' && d.params.entry.level === 'error') {
    consoleErrors.push(d.params.entry.text + ' ' + (d.params.entry.url || ''));
  }
});
const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id; pend.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
await send('Runtime.enable');
await send('Page.enable');
await send('Log.enable');
await send('Emulation.setDeviceMetricsOverride', { width: 1366, height: 900, deviceScaleFactor: 1, mobile: false });

async function ev(expr) {
  const r = await send('Runtime.evaluate', { expression: `(async()=>{${expr}})()`, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error('页面异常：' + ((r.exceptionDetails.exception || {}).description || r.exceptionDetails.text));
  return r.result.value;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function shot(name) {
  if (!process.env.GRID_SHOT) return;
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.mkdirSync(process.env.GRID_SHOT, { recursive: true });
  fs.writeFileSync(path.join(process.env.GRID_SHOT, name + '.png'), Buffer.from(r.data, 'base64'));
}
async function waitFor(expr, what, ms = 6000) {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) {
    if (await ev(`return !!(${expr});`).catch(() => false)) return;
    await sleep(60);
  }
  throw new Error('等待超时：' + what);
}
let pass = 0, fail = 0;
async function t(name, fn) {
  try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; console.log('  FAIL ' + name + '\n       ' + e.message); }
}
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b).slice(0, 200)} 实得 ${JSON.stringify(a).slice(0, 200)}`); };
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };
const has = (s, sub, m) => { if (!String(s).includes(sub)) throw new Error(`${m || ''} 应包含 ${JSON.stringify(sub)}，实得 ${JSON.stringify(String(s).slice(0, 300))}`); };
const q = (sel, prop = 'textContent') => ev(`const e=document.querySelector(${JSON.stringify(sel)}); return e ? e.${prop} : null;`);
const qa = (sel, prop = 'textContent') => ev(`return [...document.querySelectorAll(${JSON.stringify(sel)})].map(e=>e.${prop});`);
const exists = (sel) => ev(`return !!document.querySelector(${JSON.stringify(sel)});`);

async function nav(url) {
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}${url}` });
  await sleep(200);
  await waitFor("document.readyState === 'complete'", '页面加载');
}
const setEd = (v) => ev(`ace.edit(document.getElementById('editor')).setValue(${JSON.stringify(v)}, 1);`);
async function openQuery(inst, db) {
  await nav('/query');
  await ev('localStorage.clear();');
  await nav(`/query?inst=${inst}&db=${db}`);
  await waitFor("document.querySelector('#tree .tree-head, #tree .alert, #tree .tree-hint') && document.getElementById('db').value", '库表树加载');
  await sleep(150);
  // 剪贴板换成桩：记录最后一次写入的文本（无头 Chrome 里读不到真实剪贴板）
  if (process.env.GRID_SKIN) await ev(`document.documentElement.setAttribute('data-skin', ${JSON.stringify(process.env.GRID_SKIN)});`);
  await ev("window.__clip = null; Object.defineProperty(navigator.clipboard, 'writeText', { value: async (t) => { window.__clip = t; }, configurable: true });");
}
async function runQuery(sql) {
  await setEd(sql);
  const n = calls.filter((c) => c.url === '/api/v1/query/execute').length;
  await ev("document.getElementById('run').click();");
  for (let i = 0; i < 100 && calls.filter((c) => c.url === '/api/v1/query/execute').length <= n; i++) await sleep(50);
  await waitFor("document.getElementById('run').getAttribute('aria-busy') === 'false' && document.querySelector('#result table.grid')", '结果表格');
  await sleep(80);
}
const clip = () => ev('return window.__clip;');
const clearClip = () => ev('window.__clip = null;');

// ---- 真实鼠标事件（CDP）----
const mouse = (type, x, y, extra = {}) => send('Input.dispatchMouseEvent', { type, x, y, button: 'left', ...extra });
const center = (sel, idx = 0) => ev(`const e=document.querySelectorAll(${JSON.stringify(sel)})[${idx}]; e.scrollIntoView({block:'nearest',inline:'nearest'}); const r=e.getBoundingClientRect(); return {x: r.left + Math.min(r.width/2, 60), y: r.top + r.height/2};`);
// 第 r 行第 c 列（从 0 起）的数据单元格
const cellSel = (r, c) => `#result table.grid tbody tr:nth-child(${r + 1}) td:nth-child(${c + 2})`;
async function hoverCell(r, c) {
  const p = await center(cellSel(r, c));
  await mouse('mouseMoved', p.x, p.y);
  await sleep(40);
  return p;
}
async function dblclickCell(r, c) {
  const p = await center(cellSel(r, c));
  await mouse('mouseMoved', p.x, p.y);
  await mouse('mousePressed', p.x, p.y, { clickCount: 1 });
  await mouse('mouseReleased', p.x, p.y, { clickCount: 1 });
  await mouse('mousePressed', p.x, p.y, { clickCount: 2 });
  await mouse('mouseReleased', p.x, p.y, { clickCount: 2 });
  await sleep(120);
}
async function clickEl(sel, idx = 0) {
  const p = await center(sel, idx);
  await mouse('mouseMoved', p.x, p.y);
  await mouse('mousePressed', p.x, p.y, { clickCount: 1 });
  await mouse('mouseReleased', p.x, p.y, { clickCount: 1 });
  await sleep(80);
}
const pvText = () => ev("const e=document.querySelector('.cv-ace'); return e ? ace.edit(e).getValue() : null;");
const pvMode = () => ev("const e=document.querySelector('.cv-ace'); return e ? ace.edit(e).session.getMode().$id : null;");
const key = (k, mods = {}) => ev(`document.querySelector('#result .grid-wrap').dispatchEvent(new KeyboardEvent('keydown', { key: ${JSON.stringify(k)}, bubbles: true, cancelable: true, ctrlKey: ${!!mods.ctrl} }));`);

console.log(`结果表格交互页面测试（http://127.0.0.1:${HTTP}/query）`);

// =============== MySQL ===============
await openQuery(1, 'shop_my');
await t('结果出来后：有 4 行、每列一个“复制整列”按钮、表格可聚焦', async () => {
  await runQuery('select gridtest');
  eq((await qa('#result table.grid tbody tr')).length, 4);
  eq((await qa('#result table.grid th .col-copy')).length, 5);
  eq(await exists('#result .res-tip'), false, '结果栏不再显示操作提示');
  eq(await q('#result .grid-wrap', 'tabIndex'), 0);
  eq(await ev("const b=document.querySelector('#result th .col-copy').getBoundingClientRect(), h=document.querySelector('#result th:not(.rn)').getBoundingClientRect(); return b.height <= h.height && h.height < 36 && !document.querySelector('#result th .ctype');"), true, '复制列按钮和列名同一行，表头下面没有类型行');
  eq(await exists('.cell-tools:not([hidden])'), false, '没悬停时不显示工具条');
  await shot('grid_result');
});
await t('悬停单元格：工具条出现在该单元格右缘，且在表格可视区内', async () => {
  await hoverCell(0, 1);
  eq(await exists('.cell-tools:not([hidden])'), true);
  const g = await ev(`const t=document.querySelector('.cell-tools').getBoundingClientRect(), c=document.querySelector(${JSON.stringify(cellSel(0, 1))}).getBoundingClientRect(), w=document.querySelector('#result .grid-wrap').getBoundingClientRect();
    return { inRow: t.top >= c.top - 4 && t.bottom <= c.bottom + 4, atRight: Math.abs(t.right - c.right) <= 6, inView: t.left >= w.left && t.right <= w.right };`);
  eq(g, { inRow: true, atRight: true, inView: true });
  await shot('grid_hover');
});
await t('点悬停工具条的“复制”：剪贴板得到完整的多行文本', async () => {
  await clearClip();
  await clickEl('.cell-tools .cell-btn', 0);
  eq(await clip(), GRID_SCRIPT);
  eq(await exists('.cell-tools .cell-btn.done'), true, '复制成功的反馈');
});
await t('NULL 单元格复制为 NULL；数字与布尔复制显示的文本', async () => {
  await hoverCell(1, 1);
  await clickEl('.cell-tools .cell-btn', 0);
  eq(await clip(), 'NULL');
  await hoverCell(0, 3);
  await clickEl('.cell-tools .cell-btn', 0);
  eq(await clip(), '115.5');
  await hoverCell(0, 4);
  await clickEl('.cell-tools .cell-btn', 0);
  eq(await clip(), 'true');
});
await t('鼠标移到行号 / 表头时工具条收起', async () => {
  await hoverCell(0, 1);
  eq(await exists('.cell-tools:not([hidden])'), true);
  const p = await center('#result table.grid thead th', 2);
  await mouse('mouseMoved', p.x, p.y);
  await sleep(40);
  eq(await exists('.cell-tools:not([hidden])'), false);
});
await t('双击单元格：弹出预览，带行号、SQL 高亮，内容与单元格完全一致', async () => {
  await dblclickCell(0, 1);
  await waitFor("document.querySelector('.cv-dialog')", '预览弹窗');
  eq(await q('.cv-title'), 'script');
  has(await q('.cv-meta'), '第 1 行');
  has(await q('.cv-meta'), 'text');
  has(await q('.cv-meta'), `${GRID_SCRIPT.length} 字符`);
  has(await q('.cv-meta'), '6 行');
  eq(await pvText(), GRID_SCRIPT);
  eq(await pvMode(), 'ace/mode/mysql');
  eq(await ev("return ace.edit(document.querySelector('.cv-ace')).getReadOnly();"), true, '只读');
  const h6 = await ev("return document.querySelector('.cv-dialog').getBoundingClientRect().height;");
  truthy(h6 > 150 && h6 < 420, '6 行内容的弹窗高度应跟内容走，实得 ' + h6);
  truthy(await ev("return !!document.querySelector('.cv-ace .ace_gutter-cell');"), '有行号');
  truthy(await ev("return document.querySelectorAll('.cv-ace .ace_keyword').length > 0;"), 'SQL 关键字被高亮');
  eq(await ev('return document.documentElement.classList.contains("cv-lock");'), true, '锁住页面滚动');
  await shot('grid_preview');
});
await t('预览右上角“复制”：复制完整内容，并给出已复制提示', async () => {
  await clearClip();
  await clickEl('.cv-dialog .cv-icon', 0);
  eq(await clip(), GRID_SCRIPT);
  eq(await exists('.cv-dialog .cv-icon.done'), true);
  has(await q('.cv-status'), `已复制（${GRID_SCRIPT.length} 字符）`);
  eq(await exists('.cf-toast'), false, '预览里用就地提示，不弹会盖住按钮的全局提示');
});
await t('Esc 关闭预览：弹窗消失、解除锁定、焦点回到表格', async () => {
  await send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
  await send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
  await waitFor("!document.querySelector('.cv-dialog')", '弹窗关闭');
  eq(await ev('return document.documentElement.classList.contains("cv-lock");'), false);
  eq(await ev("return document.activeElement === document.querySelector('#result .grid-wrap');"), true, '焦点回到表格');
});
await t('NULL 单元格的预览：显示 NULL 并标注“值为 NULL”；点 ✕ 关闭', async () => {
  await dblclickCell(1, 1);
  await waitFor("document.querySelector('.cv-dialog')", '预览弹窗');
  const h1 = await ev("return document.querySelector('.cv-dialog').getBoundingClientRect().height;");
  truthy(h1 < 230, '1 行内容的弹窗应更矮，实得 ' + h1);
  eq(await pvText(), 'NULL');
  has(await q('.cv-meta'), '值为 NULL');
  await clickEl('.cv-dialog .cv-icon', 1);
  await waitFor("!document.querySelector('.cv-dialog')", '弹窗关闭');
});
await t('不像 SQL 的内容按纯文本预览；点遮罩关闭', async () => {
  await dblclickCell(3, 1);
  await waitFor("document.querySelector('.cv-dialog')", '预览弹窗');
  eq(await pvText(), '{"k": 1}');
  eq(await pvMode(), 'ace/mode/text');
  await mouse('mousePressed', 8, 8, { clickCount: 1 });
  await mouse('mouseReleased', 8, 8, { clickCount: 1 });
  await waitFor("!document.querySelector('.cv-dialog')", '点遮罩关闭');
});
await t('超长内容打开预览不卡死：单行 52 万字符 / 1.5 万行都在 1.5 秒内', async () => {
  const r = await ev(`const m = await import('/static/js/query/preview.js');
    const out = {};
    for (const [k, text] of [['oneLine', 'y'.repeat(520000)], ['manyLines', 'select a, b, c from t where x = 1;\\n'.repeat(15000)]]) {
      const t0 = performance.now();
      const h = m.openPreview({ title: 'x', text, mode: 'ace/mode/text' });
      out[k] = Math.round(performance.now() - t0);
      h.close();
    }
    return out;`);
  truthy(r.oneLine < 1500 && r.manyLines < 1500, '打开耗时（毫秒）：' + JSON.stringify(r));
});
await t('超长内容：只预览前 500,000 个字符并提示，复制仍是完整内容', async () => {
  await dblclickCell(2, 1);
  await waitFor("document.querySelector('.cv-dialog')", '预览弹窗');
  has(await q('.cv-note'), '500,000');
  eq(await ev("return ace.edit(document.querySelector('.cv-ace')).getValue().length;"), 500000);
  await clearClip();
  await clickEl('.cv-dialog .cv-icon', 0);
  eq((await clip()).length, GRID_LONG.length);
  await send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
  await waitFor("!document.querySelector('.cv-dialog')", '弹窗关闭');
});
await t('列头“复制整列”菜单：复制列值 / 带列名 / IN 列表', async () => {
  await clickEl('#result table.grid thead th .col-copy', 2); // note 列
  await waitFor("document.querySelector('.pop-menu')", '菜单');
  eq((await qa('.pop-menu button')).length, 3);
  eq(await q('#result table.grid thead th .col-copy[aria-expanded="true"]', 'title'), '复制「note」这一列');
  await clearClip();
  await clickEl('.pop-menu button', 0);
  eq(await clip(), ['plain', GRID_NOTE, 'plain', GRID_NOTE].join('\n'), '列值每行一个');
  eq(await exists('.pop-menu'), false, '点选后菜单关闭');
  has(await q('.cf-toast:last-child'), '已复制「note」4 行');

  await clickEl('#result table.grid thead th .col-copy', 1); // script 列：含 NULL 与换行
  await clickEl('.pop-menu button', 1);
  const withName = await clip();
  eq(withName.split('\n')[0], 'script');
  eq(withName.split('\n').length, 5, '列名 + 4 行；值里的换行折成空格、NULL 为空行');
  eq(withName.split('\n')[2], '', 'NULL 为空行');

  await clickEl('#result table.grid thead th .col-copy', 2);
  await clickEl('.pop-menu button', 2);
  eq(await clip(), "('plain', 'it''s a\\\\b')", 'IN 列表：去重，MySQL 反斜杠双写');
  has(await q('.cf-toast:last-child'), '已复制 2 个值');

  await clickEl('#result table.grid thead th .col-copy', 1);
  await clickEl('.pop-menu button', 2);
  has(await q('.cf-toast:last-child'), '跳过 1 个 NULL');
});
await t('列菜单：Esc 关闭并把焦点还给按钮；点别处关闭', async () => {
  await clickEl('#result table.grid thead th .col-copy', 0);
  await waitFor("document.querySelector('.pop-menu')", '菜单');
  await send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
  await waitFor("!document.querySelector('.pop-menu')", '菜单关闭');
  eq(await ev("return document.activeElement === document.querySelector('#result table.grid thead th .col-copy');"), true);
  await clickEl('#result table.grid thead th .col-copy', 0);
  await waitFor("document.querySelector('.pop-menu')", '菜单');
  await mouse('mousePressed', 600, 700, { clickCount: 1 });
  await mouse('mouseReleased', 600, 700, { clickCount: 1 });
  await waitFor("!document.querySelector('.pop-menu')", '点别处关闭');
});
await t('键盘：方向键移动选中格，Enter 预览，Ctrl+C 复制当前格', async () => {
  await ev("document.querySelector('#result .grid-wrap').focus();");
  await key('Escape'); // 前面的点击 / 双击留下了选中格：先清掉，从头开始
  eq(await exists('#result td.cell-on'), false);
  await key('ArrowDown');
  eq(await ev("const a=document.querySelector('#result td.cell-on'); return a ? a.parentNode.sectionRowIndex + ',' + (a.cellIndex - 1) : null;"), '0,0');
  await key('ArrowRight');
  await key('ArrowDown');
  eq(await ev("const a=document.querySelector('#result td.cell-on'); return a ? a.parentNode.sectionRowIndex + ',' + (a.cellIndex - 1) : null;"), '1,1');
  await clearClip();
  await key('c', { ctrl: true });
  await sleep(60);
  eq(await clip(), 'NULL', 'Ctrl+C 复制选中格');
  await key('ArrowUp');
  await key('Enter');
  await waitFor("document.querySelector('.cv-dialog')", 'Enter 打开预览');
  eq(await pvText(), GRID_SCRIPT);
  await send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
  await waitFor("!document.querySelector('.cv-dialog')", '关闭');
  await key('Escape');
  eq(await exists('#result td.cell-on'), false, 'Esc 取消选中');
});
await t('手选了文字时 Ctrl+C 走浏览器默认复制，不被覆盖', async () => {
  await ev("document.querySelector('#result .grid-wrap').focus();");
  await key('ArrowDown');
  await ev("const td=document.querySelector('#result tbody td:nth-child(4)'); const r=document.createRange(); r.selectNodeContents(td); const s=getSelection(); s.removeAllRanges(); s.addRange(r);");
  await clearClip();
  await key('c', { ctrl: true });
  await sleep(60);
  eq(await clip(), null, '有手选文字时不拦截');
});
await t('没有数据行时不出现工具与列复制按钮', async () => {
  await ev(`document.getElementById('result').replaceChildren();`);
  const empty = await ev(`const m = await import('/static/js/query/grid.js'); const box = document.createElement('div'); document.body.append(box);
    m.renderResult(box, { columns: ['a'], rows: [], row_count: 0, cost_ms: 1, masked_columns: [] }, {});
    const r = { copyBtn: box.querySelectorAll('.col-copy').length, tools: box.querySelectorAll('.cell-tools').length }; box.remove(); return r;`);
  eq(empty, { copyBtn: 0, tools: 0 });
});

// =============== 对象信息 / 可编程对象 ===============
async function rightClickEl(sel, idx = 0) {
  const p = await center(sel, idx);
  await mouse('mouseMoved', p.x, p.y);
  await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: p.x, y: p.y, button: 'right', buttons: 2, clickCount: 1 });
  await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: p.x, y: p.y, button: 'right', buttons: 0, clickCount: 1 });
  await sleep(100);
  return p;
}
async function pickInst(instId, expectDb) {
  await ev(`const s=document.getElementById('inst'); s.value=${JSON.stringify(String(instId))}; s.dispatchEvent(new Event('change'));`);
  await waitFor(`document.getElementById('db').value === ${JSON.stringify(expectDb)} && document.querySelector('#tree .tree-head, #tree .alert')`, '切换实例后库表树');
  await sleep(150);
}
const infoCalls = () => calls.filter((c) => c.url.includes('/object-info'));
const progCalls = () => calls.filter((c) => c.url.includes('/programmables'));
const dialogHeadings = () => qa('.cv-dialog .oi-h');
const kvRows = () => ev("return [...document.querySelectorAll('.cv-dialog .oi-pairs tbody tr')].map((r) => [...r.cells].map((c) => c.textContent));");
const gridRows = (n) => ev(`return [...document.querySelectorAll('.cv-dialog .oi-grid')][${n}] ? [...document.querySelectorAll('.cv-dialog .oi-grid')][${n}].rows.length && [...[...document.querySelectorAll('.cv-dialog .oi-grid')][${n}].rows].map((r) => [...r.cells].map((c) => c.textContent)) : null;`);
const treeName = (n) => `#tree .tr-row .name[data-table="${n}"]`;
const closeDialog = async () => {
  await send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
  await waitFor("!document.querySelector('.cv-dialog')", '弹窗关闭');
};

await openQuery(1, 'shop_my');
await t('左侧有“表 / 可编程对象”页签，默认停在“表”', async () => {
  eq(await exists('#side-tabs:not([hidden])'), true);
  eq(await qa('#side-tabs button'), ['表', '可编程对象']);
  eq(await q('#side-t-tables', 'getAttribute("aria-selected")'), 'true');
  eq(await q('#side-p-prog', 'hidden'), true);
  eq(await exists('#tree .tr-row'), true);
});
await t('右键表名：在鼠标处弹出“对象信息”菜单；点别处收起', async () => {
  const p = await rightClickEl(treeName('orders'));
  await waitFor("document.querySelector('.pop-menu')", '右键菜单');
  has(await q('.pop-menu'), '对象信息');
  eq((await qa('.pop-menu button')).length, 1, '只有一项');
  eq(await exists('.pop-menu .sub'), false, '不再显示对象名那一行');
  eq(await q('.pop-menu button'), '对象信息');
  const w = await ev("const m=document.querySelector('.pop-menu'); return [m.offsetWidth, m.offsetHeight];");
  truthy(w[0] < 160 && w[1] < 50, '宽高贴合内容：' + JSON.stringify(w));
  const pos = await ev("const r=document.querySelector('.pop-menu').getBoundingClientRect(); return {l:r.left, t:r.top};");
  truthy(Math.abs(pos.l - p.x) < 4 && Math.abs(pos.t - p.y) < 4, '菜单应出现在鼠标位置：' + JSON.stringify([pos, p]));
  await mouse('mousePressed', 700, 600, { clickCount: 1 });
  await mouse('mouseReleased', 700, 600, { clickCount: 1 });
  await waitFor("!document.querySelector('.pop-menu')", '点别处收起');
});
await t('点“对象信息”：基本信息（含数据库一行与序号）+ 创建语句；右上角复制得到完整语句', async () => {
  await rightClickEl(treeName('orders'));
  await waitFor("document.querySelector('.pop-menu')", '右键菜单');
  await clickEl('.pop-menu button');
  await waitFor("document.querySelector('.cv-dialog .oi-kv')", '对象信息弹窗');
  await waitFor("document.querySelector('.cv-dialog .cv-ace')", '创建语句');
  eq(await q('.cv-title'), 'orders');
  has(await q('.cv-meta'), '表 · shop_my');
  eq(await dialogHeadings(), ['基本信息', '列信息', '索引信息', '创建语句']);
  const rows = await kvRows();
  eq(rows[0], ['1', '数据库', 'shop_my']);
  eq(rows[1], ['2', '表名', 'orders']);
  eq(rows.map((r) => r[1]), ['数据库', '表名', '引擎', '表行数（估算）', '数据大小(KB)', '表说明']);
  const cols = await gridRows(0);
  eq(cols[0], ['#', '列名', '类型', '可空', '默认值', '键', '额外', '注释', '脱敏']);
  eq(cols[1], ['1', 'id', 'bigint', '否', 'NULL', 'PRI', 'auto_increment', '主键', '']);
  eq(cols[2][8], 'Y', '配置了脱敏的字段写 Y');
  // 除“注释”外的列一行显示（不换行）；注释可以换行；配置了脱敏的行有标记
  eq(await ev("return getComputedStyle(document.querySelector('.cv-dialog .oi-grid td:nth-child(3)')).whiteSpace;"), 'nowrap');
  eq(await ev("return getComputedStyle(document.querySelector('.cv-dialog .oi-grid td.wrap')).whiteSpace;"), 'normal');
  eq(await ev("return document.querySelectorAll('.cv-dialog .oi-grid tr.oi-sens').length;"), 1);
  const idxs = await gridRows(1);
  eq(idxs[0], ['#', '索引名', '列', '唯一', '类型']);
  eq(idxs[1], ['1', 'PRIMARY', 'id', '是', 'BTREE']);
  eq(await pvText(), OBJ_DDL);
  eq(await pvMode(), 'ace/mode/mysql');
  eq(await ev("return ace.edit(document.querySelector('.cv-ace')).getReadOnly();"), true, '只读');
  const u = infoCalls().slice(-1)[0].url;
  has(u, '/api/v1/query/instances/1/databases/shop_my/object-info');
  has(u, 'name=orders');
  has(u, 'kind=table');
  eq(await q('.cv-dialog .cv-icon', 'disabled'), false);
  await clearClip();
  await clickEl('.cv-dialog .cv-icon', 0);
  eq(await clip(), OBJ_DDL);
  has(await q('.cv-status'), '已复制');
  await shot('objinfo_table');
});
await t('Esc 关闭对象信息，焦点回到树里的表名', async () => {
  await closeDialog();
  eq(await ev("return document.activeElement && document.activeElement.dataset.table;"), 'orders');
});
await t('视图右键：按 kind=view 取', async () => {
  await rightClickEl(treeName('v_orders'));
  await clickEl('.pop-menu button');
  await waitFor("document.querySelector('.cv-dialog .oi-kv')", '对象信息弹窗');
  has(infoCalls().slice(-1)[0].url, 'kind=view');
  has(await q('.cv-meta'), '视图 · shop_my');
  await closeDialog();
});
await t('SQL Server 的表：基本信息（主键标注自增）、列信息（identity 自增）、索引信息、创建语句含 IDENTITY 与索引语句', async () => {
  await pickInst(2, 'master');
  await rightClickEl(treeName('orders'));
  await waitFor("document.querySelector('.pop-menu')", '右键菜单');
  await clickEl('.pop-menu button');
  await waitFor("document.querySelector('.cv-dialog .cv-ace')", '对象信息弹窗');
  const u = infoCalls().slice(-1)[0].url;
  has(u, 'schema=dbo');
  has(u, 'name=orders');
  has(u, 'kind=table');
  eq(await dialogHeadings(), ['基本信息', '列信息', '索引信息', '创建语句（含索引、触发器）']);
  const rows = await kvRows();
  eq(rows.find((r) => r[1] === '主键').slice(1), ['主键', 'Id（自增）']);
  eq(rows[0], ['1', '数据库', 'master']);
  eq(rows.find((r) => r[1] === '当前自增值')[2], '1377');
  const cols = await gridRows(0);
  eq(cols[1], ['1', 'Id', 'int', '否', 'NULL', 'PRI', 'identity（自增）', '主键']);
  const idxs = await gridRows(1);
  eq(idxs[1], ['1', 'PK_orders', 'Id', '是', 'CLUSTERED · 主键']);
  eq(idxs[2], ['2', 'ix_name', 'Name', '否', 'NONCLUSTERED']);
  const ddl = await pvText();
  eq(ddl, MS_DDL);
  has(ddl, '[Id] int IDENTITY(1,1) NOT NULL');
  has(ddl, '-- INDEXES');
  eq(await pvMode(), 'ace/mode/sqlserver');
  has(await q('.cv-dialog .oi-hint'), '仅供查看参考');
  await shot('objinfo_mssql');
  await closeDialog();
  await pickInst(1, 'shop_my');
});
await t('基本信息属性多（>8）时分成左右两栏、序号连续；属性少时仍是一栏', async () => {
  await ev(`const m = await import('/static/js/query/objinfo.js'); m.showObjectInfo({ inst: '1', db: 'shop_my', kind: 'table', name: 'orders_wide', type: 'mysql' });`);
  await waitFor("document.querySelector('.cv-dialog .oi-kvcols')", '分栏');
  const tabs = await ev("return [...document.querySelectorAll('.cv-dialog .oi-kvcols .oi-pairs')].map((t) => [...t.tBodies[0].rows].map((r) => r.cells[0].textContent));");
  eq(tabs.length, 2);
  eq(tabs[0], ['1', '2', '3', '4', '5', '6', '7', '8']);
  eq(tabs[1], ['9', '10', '11', '12', '13', '14', '15']);
  const w = await ev("return [...document.querySelectorAll('.cv-dialog .oi-kvcols .oi-kvwrap')].map((e) => Math.round(e.getBoundingClientRect().width));");
  eq(Math.abs(w[0] - w[1]) <= 2, true, '两栏等宽');
  await shot('objinfo_wide');
  await closeDialog();
});
await t('取不到时在弹窗里显示原因，复制按钮置灰', async () => {
  await ev(`const m = await import('/static/js/query/objinfo.js'); m.showObjectInfo({ inst: '1', db: 'shop_my', kind: 'table', name: 'gone', type: 'mysql' });`);
  await waitFor("document.querySelector('.cv-dialog .alert.err')", '错误提示');
  has(await q('.cv-dialog .alert.err'), '不存在，或当前账号没有权限查看');
  eq(await q('.cv-dialog .cv-icon', 'disabled'), true);
  await closeDialog();
});
await t('可编程对象页签：按类别分组并带数量；只请求一次；触发器读取失败的原因显示在该组', async () => {
  await clickEl('#side-t-prog');
  await waitFor("document.querySelector('#prog-tree .tr-group')", '分组');
  eq(await qa('#prog-tree .tr-group .gn'), ['视图', '函数', '存储过程', '触发器', '定时任务']);
  eq(await qa('#prog-tree .tr-group .n'), ['(1)', '(2)', '(1)', '(0)', '(0)']);
  eq(await exists('#side-p-tables:not([hidden])'), false);
  eq(progCalls().length, 1);
  await clickEl('#prog-tree .tr-group[data-kind="trigger"]');
  has(await q('#prog-tree .tree-hint'), '读取失败：denied');
  await clickEl('#prog-tree .tr-group[data-kind="trigger"]');
  await shot('prog_tab');
});
await t('展开“函数”双击对象：打开定义（只读），标题是对象名；复制得到定义语句', async () => {
  await clickEl('#prog-tree .tr-group[data-kind="function"]');
  eq(await qa('#prog-tree .prog-item', 'dataset.name'), ['fn_a', 'fn_b']);
  const p = await center('#prog-tree .prog-item[data-name="fn_a"]');
  await mouse('mouseMoved', p.x, p.y);
  await mouse('mousePressed', p.x, p.y, { clickCount: 1 });
  await mouse('mouseReleased', p.x, p.y, { clickCount: 1 });
  await mouse('mousePressed', p.x, p.y, { clickCount: 2 });
  await mouse('mouseReleased', p.x, p.y, { clickCount: 2 });
  await waitFor("document.querySelector('.cv-dialog .cv-ace')", '定义弹窗');
  eq(await q('.cv-title'), 'fn_a');
  has(await q('.cv-meta'), '函数 · shop_my');
  eq(await dialogHeadings(), ['基本信息', '定义']);
  eq(await pvText(), FN_DEF);
  const u = infoCalls().slice(-1)[0].url;
  has(u, 'kind=function');
  has(u, 'name=fn_a');
  await clearClip();
  await clickEl('.cv-dialog .cv-icon', 0);
  eq(await clip(), FN_DEF);
  await closeDialog();
});
await t('搜索框只按对象名过滤：命中的组自动展开并显示命中数', async () => {
  await ev("const i=document.getElementById('prog-q'); i.value='FN_B'; i.dispatchEvent(new Event('input'));");
  eq(await qa('#prog-tree .tr-group .gn'), ['函数']);
  eq(await qa('#prog-tree .tr-group .n'), ['(1)']);
  eq(await qa('#prog-tree .prog-item', 'dataset.name'), ['fn_b']);
  await ev("const i=document.getElementById('prog-q'); i.value='加一'; i.dispatchEvent(new Event('input'));"); // 注释不参与匹配
  has(await q('#prog-tree .tree-hint'), '没有匹配的对象');
  await ev("const i=document.getElementById('prog-q'); i.value=''; i.dispatchEvent(new Event('input'));");
  eq((await qa('#prog-tree .tr-group')).length, 5);
});
await t('键盘：对象上按 Enter 打开定义；右键对象出“对象信息”菜单', async () => {
  await ev("document.querySelector('#prog-tree .prog-item[data-name=\"fn_b\"]').focus();");
  await ev("document.activeElement.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }));");
  await waitFor("document.querySelector('.cv-dialog .cv-ace')", 'Enter 打开');
  eq(await q('.cv-title'), 'fn_b');
  await closeDialog();
  await rightClickEl('#prog-tree .prog-item[data-name="fn_a"]');
  await waitFor("document.querySelector('.pop-menu')", '右键菜单');
  await clickEl('.pop-menu button');
  await waitFor("document.querySelector('.cv-dialog .cv-ace')", '对象信息');
  eq(await q('.cv-title'), 'fn_a');
  await closeDialog();
});
await t('来回切页签不重复请求；回到“表”页签树仍在', async () => {
  const n = progCalls().length;
  await clickEl('#side-t-tables');
  eq(await exists('#tree .tr-row'), true);
  eq(await q('#side-p-prog', 'hidden'), true);
  await clickEl('#side-t-prog');
  eq(progCalls().length, n);
});
await t('换成 PostgreSQL：页签隐藏、回到“表”；右键 public.orders 按 schema + name 取，并显示引擎说明', async () => {
  await pickInst(4, 'shop');
  eq(await exists('#side-tabs:not([hidden])'), false);
  eq(await exists('#side-p-tables:not([hidden])'), true);
  await rightClickEl(treeName('public.orders'));
  await clickEl('.pop-menu button');
  await waitFor("document.querySelector('.cv-dialog .oi-kv')", '对象信息弹窗');
  const u = infoCalls().slice(-1)[0].url;
  has(u, 'schema=public');
  has(u, 'name=orders');
  has(u, 'kind=table');
  has(await q('.cv-dialog .oi-alert'), '近似 DDL');
  eq(await pvMode(), 'ace/mode/pgsql');
  await closeDialog();
});
await t('SQL Server：可编程对象带架构（dbo.GetMonthOfDays），按 架构 + 名称取，用 SQL Server 高亮', async () => {
  await pickInst(2, 'master');
  eq(await exists('#side-tabs:not([hidden])'), true);
  await clickEl('#side-t-prog');
  await waitFor("document.querySelector('#prog-tree .tr-group')", '分组');
  await clickEl('#prog-tree .tr-group[data-kind="function"]');
  eq(await qa('#prog-tree .prog-item', 'dataset.name'), ['dbo.GetMonthOfDays', 'dbo.split']);
  const p = await center('#prog-tree .prog-item[data-name="dbo.GetMonthOfDays"]');
  await mouse('mouseMoved', p.x, p.y);
  await mouse('mousePressed', p.x, p.y, { clickCount: 1 });
  await mouse('mouseReleased', p.x, p.y, { clickCount: 1 });
  await mouse('mousePressed', p.x, p.y, { clickCount: 2 });
  await mouse('mouseReleased', p.x, p.y, { clickCount: 2 });
  await waitFor("document.querySelector('.cv-dialog .cv-ace')", '定义弹窗');
  const u = infoCalls().slice(-1)[0].url;
  has(u, 'schema=dbo');
  has(u, 'name=GetMonthOfDays');
  has(u, 'kind=function');
  eq(await q('.cv-title'), 'dbo.GetMonthOfDays');
  eq(await pvMode(), 'ace/mode/sqlserver');
  await closeDialog();
});
await t('在“可编程对象”页签时换成 Redis：回到“表”页签并隐藏页签条', async () => {
  await pickInst(3, '0');
  eq(await exists('#side-tabs:not([hidden])'), false);
  eq(await exists('#side-p-tables:not([hidden])'), true);
  eq(await q('#side-p-prog', 'hidden'), true);
});

// =============== PostgreSQL / MongoDB ===============
await openQuery(4, 'shop');
await t('PostgreSQL：IN 列表只双写单引号、跳过 NULL', async () => {
  await runQuery('select id, name from public.orders');
  await clickEl('#result table.grid thead th .col-copy', 1);
  await clickEl('.pop-menu button', 2);
  eq(await clip(), "('alice')");
  has(await q('.cf-toast:last-child'), '跳过 1 个 NULL');
});
await openQuery(5, 'appdb');
await t('MongoDB：列菜单没有 IN 列表', async () => {
  await runQuery('db.orders.find({})');
  await clickEl('#result table.grid thead th .col-copy', 1);
  await waitFor("document.querySelector('.pop-menu')", '菜单');
  eq((await qa('.pop-menu button')).length, 2);
});
await t('MongoDB：预览一律纯文本', async () => {
  await dblclickCell(0, 2);
  await waitFor("document.querySelector('.cv-dialog')", '预览弹窗');
  eq(await pvMode(), 'ace/mode/text');
  eq(await pvText(), '[{"sku":"a;b"}]');
});
await t('页面无控制台错误（含 CSP 违规）', async () => {
  eq(consoleErrors.filter((e) => !e.includes('/favicon.ico') && !e.includes('name=gone')), []); // gone：故意取一个不存在的对象，浏览器会记一条 404
});

console.log(`\n${pass} 通过，${fail} 失败`);
cleanup();
process.exit(fail ? 1 : 0);
