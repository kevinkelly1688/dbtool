/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_maskadmin.mjs
 * @time: 2026/10/28
 */
// 系统管理 › 数据脱敏 页面级测试：自起假后端（接口形状按 internal/maskadmin）+ 无头 Chrome（CDP），不依赖第三方包。
// 用法：node scripts/frontend-test/run_maskadmin.mjs [chrome路径]    环境变量 MK_HTTP_PORT（默认 8916）/ MK_CDP_PORT（默认 9253）/ MK_SHOT（截图目录）。
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import http from 'node:http';
import { fileURLToPath } from 'node:url';
import { untranslate } from './tpl.mjs';
import { spawn, spawnSync } from 'node:child_process';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.join(here, '../..');
const staticRoot = path.join(root, 'web/static');
const HTTP = +(process.env.MK_HTTP_PORT || 8916);
const CDP = +(process.env.MK_CDP_PORT || 9253);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'";
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.json': 'application/json', '.svg': 'image/svg+xml' };

// ---- 假后端 ----
const ENGINES = {
  mysql: { label: 'MySQL', hier: '库 › 表 › 字段', path: '库.表.字段', table: '表', table_n: '张表', field: '字段', row_unit: '行', source: '目标库 information_schema.COLUMNS', locate: 'exact', how: 'goInception 语法树逐列溯源', note: '' },
  pgsql: { label: 'PostgreSQL', hier: '库 › 架构.表 › 列', path: '库.架构.表.列', table: '表', table_n: '张表', field: '列', row_unit: '行', source: 'pg_catalog', locate: 'exact', how: '结果集描述里自带每列的表 OID 与列号', note: '' },
  mongo: { label: 'MongoDB', hier: '库 › 集合 › 字段路径', path: '库.集合.字段路径', table: '集合', table_n: '个集合', field: '字段', row_unit: '文档', source: '采样每个集合最近的文档推断字段', locate: 'path', how: '按字段路径匹配返回的文档', note: '' },
};
const INSTANCES = [
  { id: 1, name: 'prod-mysql-01', db_type: 'mysql', label: 'MySQL', supported: true, count: 2, engine: ENGINES.mysql },
  { id: 2, name: 'risk-pg-01', db_type: 'pgsql', label: 'PostgreSQL', supported: true, count: 0, engine: ENGINES.pgsql },
  { id: 3, name: 'app-mongo-01', db_type: 'mongo', label: 'MongoDB', supported: true, count: 1, engine: ENGINES.mongo },
  { id: 4, name: 'cache-redis-01', db_type: 'redis', label: 'Redis', supported: false, why: '键值结构，没有表 / 字段', count: 0 },
];
const col = (name, type, comment = '') => ({ name, type, comment });
const CATALOGS = {
  '1:shop': { db: 'shop', truncated: false, tables: [
    { name: 'users', comment: '用户', view: false, rows: 1280000, columns: [col('id', 'bigint'), col('user_name', 'varchar(64)'), col('phone', 'varchar(20)', '手机号'), col('id_card', 'varchar(18)', '身份证号'), col('email', 'varchar(128)'), col('created_at', 'datetime')] },
    { name: 'orders', comment: '订单', view: false, rows: 9420000, columns: [col('id', 'bigint'), col('receiver_phone', 'varchar(20)'), col('phone', 'varchar(20)'), col('amount', 'decimal(12,2)')] },
    { name: 'sys_log', comment: '系统日志', view: false, rows: 55000000, columns: [col('id', 'bigint'), col('message', 'text')] },
  ] },
  '1:crm': { db: 'crm', truncated: false, tables: [
    { name: 'customers', comment: '客户', view: false, rows: 210000, columns: [col('id', 'bigint'), col('mobile', 'varchar(20)'), col('email', 'varchar(128)')] },
  ] },
  '2:riskdb': { db: 'riskdb', truncated: false, tables: [
    { name: 'public.account', comment: '账户', view: false, rows: 3600000, columns: [col('id', 'bigint'), col('phone', 'varchar(20)'), col('ext', 'jsonb')] },
    { name: 'audit.login_event', comment: '', view: false, rows: 42000000, columns: [col('id', 'bigint'), col('mobile', 'varchar(20)')] },
  ] },
  '3:appdb': { db: 'appdb', truncated: false, tables: [
    { name: 'users', comment: '用户', view: false, rows: 1200000, columns: [col('_id', 'objectId'), col('profile.phone', 'string|int'), col('profile.id_card', 'string'), col('contacts[].mobile', 'string')] },
  ] },
};
const DBS = { 1: ['shop', 'crm'], 2: ['riskdb'], 3: ['appdb'] };
const state = { policyOk: true, items: {}, saves: [], saveError: null, maskingOn: true, enableError: null };
function resetState() {
  state.policyOk = true; state.saves = []; state.saveError = null; state.maskingOn = true; state.enableError = null;
  state.items = {
    1: [
      { source: 'policy', id: 1, scope_db: 'shop', scope_table: 'users', field: 'phone', wildcard: false, rule: { kind: 'keep', prefix: 3, suffix: 4 }, rule_text: '保留前 3 位 + *** + 后 4 位', active: true, updated_by: 'admin', updated_at: '2026-10-11 10:00' },
      { source: 'legacy', id: 9, scope_db: 'shop', scope_table: 'users', field: 'id_card', wildcard: false, rule: { kind: 'three', hide: 2 }, rule_text: '三段式：隐藏中段', active: true, legacy_desc: '旧规则 100：三段式', updated_at: '2026-09-01 08:00' },
      { source: 'policy', id: 2, scope_db: '*', scope_table: '*', field: 'email', wildcard: true, rule: { kind: 'keep', prefix: 2, suffix: 0 }, rule_text: '保留前 2 位 + *** + 后 0 位', active: true, updated_by: 'admin', updated_at: '2026-10-11 10:01' },
    ],
    2: [], 3: [],
  };
}
resetState();
const calls = [];
const ok = (data) => ({ code: 'ok', message: 'ok', data });

function readTpl(rel) { return untranslate(fs.readFileSync(path.join(root, 'web/templates', rel), 'utf8').replace(/\r\n/g, '\n')); }
function page() {
  const t = readTpl('maskadmin/index.html');
  const ci = t.indexOf('{{define "content"}}'), si = t.indexOf('{{define "scripts"}}');
  const content = t.slice(ci + '{{define "content"}}'.length, si).replace(/\n\{\{end\}\}\s*$/, '');
  const scripts = t.slice(si + '{{define "scripts"}}'.length).replace(/\{\{end\}\}\s*$/, '');
  const head = (t.match(/\{\{define "head"\}\}([\s\S]*?)\{\{end\}\}/) || [, ''])[1];
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="csrf-token" content="tok"><title>数据脱敏</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${head}</head><body><div class="app"><div class="main">${content}</div></div>${scripts}</body></html>`;
}

const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, 'http://x');
  const p = decodeURIComponent(u.pathname);
  let body = '';
  for await (const c of req) body += c;
  const json = (status, obj) => { res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(obj)); };
  if (req.method !== 'GET' || p.startsWith('/api/')) {
    let parsed = null;
    try { parsed = body ? JSON.parse(body) : null; } catch (_) { /* 非 JSON */ }
    calls.push({ method: req.method, url: p + u.search, body: parsed });
  }
  if (p === '/favicon.ico') { res.writeHead(204); res.end(); return; }
  if (p === '/masking') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' }); res.end(page()); return; }
  if (p.startsWith('/static/')) {
    const f = path.join(staticRoot, p.slice(8));
    if (f.startsWith(staticRoot) && fs.existsSync(f) && fs.statSync(f).isFile()) {
      res.writeHead(200, { 'Content-Type': MIME[path.extname(f)] || 'application/octet-stream', 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' });
      fs.createReadStream(f).pipe(res);
      return;
    }
    res.writeHead(404); res.end('nf'); return;
  }
  if (p === '/api/v1/button-perms/me') return json(200, ok({ mode: 'compat', deny_display: 'hide', superuser: true, allowed: [] }));
  const base = '/api/v1/settings/masking';
  const id = Number(u.searchParams.get('instance_id'));
  if (p === base + '/instances') return json(200, ok(INSTANCES));
  if (p === base + '/databases') return json(200, ok(DBS[id] || []));
  if (p === base + '/catalog') {
    const c = CATALOGS[id + ':' + u.searchParams.get('db')];
    return c ? json(200, ok(c)) : json(404, { code: 'not_found', message: '库不存在' });
  }
  if (p === base + '/config') {
    return json(200, ok({ items: state.items[id] || [], policy_ok: state.policyOk, masking_on: state.maskingOn, export_on: state.maskingOn, query_check: false }));
  }
  if (p === base + '/enable' && req.method === 'POST') {
    if (state.enableError) return json(500, { code: 'internal', message: state.enableError });
    const changed = !state.maskingOn;
    state.maskingOn = true;
    return json(200, ok({ masking_on: true, changed }));
  }
  if (p === base + '/save' && req.method === 'POST') {
    const r = JSON.parse(body);
    if (state.saveError) return json(500, { code: 'internal', message: state.saveError });
    state.saves.push(r);
    // 套用到假数据，让“保存后重新读取配置”看得到
    const list = state.items[r.instance_id] || (state.items[r.instance_id] = []);
    const res0 = { added: 0, updated: 0, removed: 0, toggled: 0 };
    for (const c of r.changes) {
      const i = list.findIndex((x) => x.scope_db === c.scope_db && x.scope_table === c.scope_table && x.field === c.field);
      if (c.op === 'set') {
        const it = { source: 'policy', id: 100 + list.length, scope_db: c.scope_db, scope_table: c.scope_table, field: c.field, wildcard: c.scope_db === '*', rule: c.rule, rule_text: '新规则', active: true, updated_by: 'admin', updated_at: '刚刚' };
        if (i >= 0) { list[i] = it; res0.updated++; } else { list.push(it); res0.added++; }
      } else if (c.op === 'remove' && i >= 0) { list.splice(i, 1); res0.removed++; }
      else if ((c.op === 'enable' || c.op === 'disable') && i >= 0) { list[i].active = c.op === 'enable'; res0.toggled++; }
    }
    return json(200, ok(res0));
  }
  res.writeHead(404); res.end('nf');
}).listen(HTTP);

// ---- Chrome ----
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'mk-chrome-'));
const chrome = spawn(chromePath, ['--headless=new', `--remote-debugging-port=${CDP}`, `--user-data-dir=${profile}`, '--no-first-run', '--disable-gpu', 'about:blank'], { stdio: 'ignore' });
let cleaned = false;
function cleanup() {
  if (cleaned) return;
  cleaned = true;
  try { if (process.platform === 'win32') spawnSync('taskkill', ['/PID', String(chrome.pid), '/T', '/F'], { stdio: 'ignore' }); else chrome.kill(); } catch (_) { /* 已退出 */ }
  server.close();
  setTimeout(() => { try { fs.rmSync(profile, { recursive: true, force: true }); } catch (_) { /* 占用中 */ } }, 1500).unref();
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
let seq = 0;
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
const send = (method, params = {}) => new Promise((res, rej) => { const i = ++seq; pend.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
await send('Runtime.enable'); await send('Page.enable'); await send('Log.enable');
await send('Emulation.setDeviceMetricsOverride', { width: 1366, height: 900, deviceScaleFactor: 1, mobile: false });

async function ev(expr) {
  const r = await send('Runtime.evaluate', { expression: `(async()=>{${expr}})()`, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error('页面异常：' + ((r.exceptionDetails.exception || {}).description || r.exceptionDetails.text));
  return r.result.value;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function shot(name) {
  if (!process.env.MK_SHOT) return;
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.mkdirSync(process.env.MK_SHOT, { recursive: true });
  fs.writeFileSync(path.join(process.env.MK_SHOT, name + '.png'), Buffer.from(r.data, 'base64'));
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
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b)} 实得 ${JSON.stringify(a)}`); };
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };
const has = (s, sub, m) => { if (!String(s).includes(sub)) throw new Error(`${m || ''} 应包含 ${JSON.stringify(sub)}，实得 ${JSON.stringify(String(s).slice(0, 300))}`); };
const q = (sel, prop = 'textContent') => ev(`const e=document.querySelector(${JSON.stringify(sel)}); return e ? e.${prop} : null;`);
const qa = (sel, prop = 'textContent') => ev(`return [...document.querySelectorAll(${JSON.stringify(sel)})].map(e=>e.${prop});`);
const click = (sel) => ev(`document.querySelector(${JSON.stringify(sel)}).click();`);
async function nav() {
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/masking` });
  await sleep(200);
  await waitFor("document.readyState === 'complete'", '页面加载');
}
async function setInst(id) {
  await ev(`const s=document.getElementById('mk-inst'); s.value=${JSON.stringify(String(id))}; s.dispatchEvent(new Event('change'));`);
  await sleep(300);
}
async function setDb(db) {
  await ev(`const s=document.getElementById('mk-db'); s.value=${JSON.stringify(db)}; s.dispatchEvent(new Event('change'));`);
  await waitFor("document.querySelector('#mk-tree .mk-tbl')", '库表树');
}
const wizReady = async () => { await nav(); await waitFor("document.getElementById('mk-db').options.length > 1", '库列表'); };

// ================= 用例 =================
console.log('数据脱敏页面');

await t('页面加载：实例下拉只列支持的实例，引擎说明条、已配置计数', async () => {
  resetState();
  await nav();
  await waitFor("document.getElementById('mk-db').options.length > 1", '库列表');
  const opts = await qa('#mk-inst option');
  eq(opts.length, 3, '支持的实例数');
  has(opts[0], 'prod-mysql-01', '第一个实例');
  has(await q('#mk-engine'), 'MySQL', '引擎说明');
  has(await q('#mk-engine'), '精确定位', '定位精度');
  eq(await q('#mk-cfg-count'), '3', '已配置计数');
  has(await q('#mk-alerts'), '另有 1 个实例不在脱敏范围内', '不支持的实例提示');
  truthy(!(await q('#mk-apply', 'disabled')) === false, '没选字段时应用按钮禁用');
});

await t('版面：底栏与两个面板都在可视区域内（面板内部滚动，页面不再出现整页滚动）', async () => {
  const m = await ev("const r = (id) => document.getElementById(id).getBoundingClientRect(); return { bar: r('mk-bar').bottom, wiz: r('mk-wiz').bottom, h: innerHeight, sh: document.scrollingElement.scrollHeight };");
  truthy(m.bar <= m.h + 1, `底栏 ${m.bar} 超出视口 ${m.h}`);
  truthy(m.wiz <= m.h, `向导区 ${m.wiz} 超出视口 ${m.h}`);
});

await t('选库后列出表与字段；已脱敏的字段显示规则', async () => {
  await setDb('shop');
  eq(await qa('#mk-tree .mk-tbl .name'), ['orders', 'sys_log', 'users'], '表');
  await ev("document.querySelector('.mk-tbl[data-t$=\"users\"] .mk-caret').click();");
  const rows = await qa('.mk-tbl[data-t$="users"] .mk-col-row');
  truthy(rows.some((r) => r.includes('phone') && r.includes('保留前 3 位')), 'phone 已脱敏');
  truthy(rows.some((r) => r.includes('id_card') && r.includes('旧配置')), '旧配置标记');
  truthy(rows.some((r) => r.includes('email') && r.includes('保留前 2 位')), 'email 通配生效');
});

await t('选中疑似敏感 → 套用手机号预设 → 暂存 → 底栏计数', async () => {
  await click('#mk-b-sus');
  const hint = await q('#mk-sel-hint');
  has(hint, '已选 3 个', '疑似敏感且未脱敏：orders.receiver_phone、orders.phone、users.user_name');
  await ev("document.querySelector('[data-preset=\"0\"]').click();");
  has(await q('#mk-impact'), '2 张表的 3 个字段', '影响面板');
  has(await q('#mk-impact'), '每类用各自的推荐规则', '含手机号与姓名两类时给出推荐规则按钮');
  await click('#mk-apply');
  has(await q('#mk-bar'), '待保存 3 项', '底栏');
  has(await q('#mk-bar'), '新增 3', '新增计数');
  await shot('wizard-staged');
});

await t('按字段名视图：同名字段聚合，一勾选中所有表里的它', async () => {
  await click('#mk-v-name');
  const names = await qa('#mk-tree .mk-tbl .name');
  truthy(names.includes('phone'), '有 phone 聚合行');
  await ev("document.querySelector('.mk-tbl[data-n=\"phone\"] input[data-act=\"nsel\"]').click();");
  has(await q('#mk-sel-hint'), '已选 2 个', 'users.phone 与 orders.phone');
  await click('#mk-b-none');
  await click('#mk-v-table');
});

await t('规则面板：步进器改位数 → 试算实时变化；超界被夹住', async () => {
  await ev("document.querySelector('[data-kind=\"keep\"]').click();");
  await ev("document.querySelector('[data-step=\"prefix\"][data-d=\"1\"]').click();");
  const out = await qa('#mk-prev .out');
  eq(out[0], '1381***5678'.slice(0, 0) + '1381***5678'.replace('1381', '1381'), '前 4 位');
  await ev("for (let i=0;i<80;i++) document.querySelector('[data-step=\"suffix\"][data-d=\"1\"]').click();");
  eq(await q('#mk-params input[data-num="suffix"]', 'value'), '64', '后缀上限 64');
  await ev("document.querySelector('[data-preset=\"0\"]').click();");
});

await t('保存：确认弹窗列出差异，确认后 POST 变更并刷新配置', async () => {
  await click('#mk-bar .btn.primary');
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  has(await q('.cf-dialog'), 'shop.orders.receiver_phone', '差异里的字段');
  has(await q('.cf-dialog'), '保留前 3 位 + *** + 后 4 位', '新规则');
  await ev("[...document.querySelectorAll('.cf-dialog button')].find(b => b.textContent.includes('确认并生效')).click();");
  await waitFor('document.querySelector(".cf-toast")', '保存成功提示');
  const last = calls.filter((c) => c.url === '/api/v1/settings/masking/save').pop();
  truthy(last, '应发出保存请求');
  eq(last.body.instance_id, 1, '实例');
  eq(last.body.changes.length, 3, '变更条数');
  const rp = last.body.changes.find((c) => c.field === 'receiver_phone');
  eq([rp.op, rp.scope_db, rp.scope_table, rp.rule.kind, rp.rule.prefix, rp.rule.suffix], ['set', 'shop', 'orders', 'keep', 3, 4], '变更体');
  await waitFor("document.getElementById('mk-cfg-count').textContent === '6'", '重新读取配置');
  has(await q('#mk-bar'), '没有未保存的变更', '底栏清空');
});

await t('“该实例所有同名字段”：暂存成 *.*.字段 通配', async () => {
  await ev("document.querySelector('.mk-tbl[data-t$=\"users\"] .mk-caret').click();");
  await ev("document.querySelector('.mk-col-row[data-k*=\"user_name\"] input').click();");
  await ev("document.querySelector('input[name=mk-scope][value=name]').click();");
  await ev("document.querySelector('[data-preset=\"3\"]').click();");
  await click('#mk-apply');
  has(await q('#mk-bar'), '待保存 1 项', '通配暂存');
  await click('#mk-bar .btn.primary');
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  has(await q('.cf-dialog'), '任意库表', '通配路径展示');
  await ev("[...document.querySelectorAll('.cf-dialog button')].find(b => b.textContent.includes('取消')).click();");
  await sleep(100);
  await ev("document.getElementById('mk-bar').querySelector('.btn').click();"); // 放弃变更
  await ev("document.querySelector('input[name=mk-scope][value=exact]').click();");
});

await t('已配置字段：批量停用 / 移除先暂存；旧配置有标记', async () => {
  await click('#mk-t-cfg');
  const rows = await qa('#mk-ctable tbody tr');
  eq(rows.length, 6, '配置行数');
  truthy(rows.some((r) => r.includes('id_card') && r.includes('旧配置')), '旧配置标记');
  await shot('config-list');
  await ev("document.querySelector('#mk-ctable tr[data-k*=\"id_card\"] input[data-act=\"ccsel\"]').click();");
  await click('#mk-c-off');
  has(await q('#mk-bar'), '待保存 1 项', '停用暂存');
  await ev("document.querySelector('#mk-ctable tr[data-k*=\"id_card\"] input[data-act=\"ccsel\"]').click();");
  await ev("document.querySelector('#mk-ctable tr[data-k*=\"id_card\"] input[data-act=\"ccsel\"]').click();");
  await click('#mk-c-del');
  has(await q('#mk-bar'), '移除 1', '移除覆盖停用');
  await ev("document.getElementById('mk-bar').querySelector('.btn').click();");
});

await t('粘贴清单：从右往左匹配，未匹配的行给出提示', async () => {
  await click('#mk-t-wiz');
  await click('#mk-paste-open');
  await ev("const t=document.getElementById('mk-paste'); t.value='users.phone\\nreceiver_phone\\nnope.x'; t.dispatchEvent(new Event('input'));");
  has(await q('#mk-paste-res'), '匹配到 2 个', '匹配数');
  has(await q('#mk-paste-res'), '未匹配 1 行', '未匹配');
  await click('#mk-p-ok');
  has(await q('#mk-sel-hint'), '已选 2 个', '选中');
  await click('#mk-b-none');
});

await t('MongoDB 实例：术语变集合 / 字段，可手动补充路径，类型不统一有提示', async () => {
  await setInst(3);
  await waitFor("document.querySelector('#mk-tree .mk-tbl')", 'mongo 目录');
  eq(await q('#mk-v-table'), '按集合', '术语');
  eq(await q('#mk-addpath', 'hidden'), false, '补路径入口');
  await ev("document.getElementById('mk-ap-in').value='users.profile.wechat'; document.getElementById('mk-ap-add').click();");
  has(await q('#mk-sel-hint'), '已选 1 个', '新增后选中');
  await ev("document.querySelector('.mk-col-row[data-k*=\"profile.phone\"] input').click();");
  has(await q('#mk-impact'), '类型不统一', '类型不统一提示');
  has(await q('#mk-kinds'), '整字段隐藏', 'Mongo 的整字段');
  await click('#mk-b-none');
});

await t('PostgreSQL 实例：表名带架构；JSON 列有提示', async () => {
  await setInst(2);
  await waitFor("document.querySelector('#mk-tree .mk-tbl')", 'pg 目录');
  eq(await qa('#mk-tree .mk-tbl .name'), ['audit.login_event', 'public.account'], '带架构的表名');
  await ev("document.querySelector('.mk-tbl[data-t$=\"public.account\"] .mk-caret').click();");
  await ev("document.querySelector('.mk-col-row[data-k*=\"ext\"] input').click();");
  has(await q('#mk-impact'), 'JSON / Map', 'jsonb 提示');
  await click('#mk-b-none');
});

await t('策略表没建：红色提示，保存按钮禁用', async () => {
  state.policyOk = false;
  await nav();
  await waitFor("document.getElementById('mk-db').options.length > 1", '库列表');
  has(await q('#mk-alerts'), '脱敏策略表还没建', '提示');
  state.policyOk = true;
});

await t('总开关关闭：横幅带“开启总开关”按钮；确认后 POST /enable，横幅变为已开启', async () => {
  state.maskingOn = false;
  await nav();
  await waitFor("document.querySelector('#mk-alerts .mk-alert-act .btn')", '带按钮的横幅');
  has(await q('#mk-alerts .mk-alert-act'), '暂不生效', '关闭提示');
  has(await q('#mk-alerts .mk-alert-act .btn'), '开启总开关', '按钮文字');
  await shot('master-switch-off');
  const before = calls.filter((c) => c.url === '/api/v1/settings/masking/enable').length;
  await click('#mk-alerts .mk-alert-act .btn');
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  has(await q('.cf-dialog'), '开启系统脱敏总开关', '弹窗标题');
  has(await q('.cf-dialog'), 'SQL 导出工单', '说明影响面：导出工单');
  has(await q('.cf-dialog'), 'query_check', '提示 query_check 当前关闭');
  eq(calls.filter((c) => c.url === '/api/v1/settings/masking/enable').length, before, '确认前不发请求');
  await ev("[...document.querySelectorAll('.cf-dialog button')].find(b => b.textContent.includes('确认开启')).click();");
  await waitFor("!document.querySelector('#mk-alerts .mk-alert-act')", '按钮消失');
  eq(calls.filter((c) => c.url === '/api/v1/settings/masking/enable').length, before + 1, '只发一次 /enable');
  eq(String(await q('#mk-alerts')).includes('暂不生效'), false, '横幅变为已开启（不再提示总开关已关闭）');
  await waitFor('document.querySelector(".cf-toast")', '成功提示');
  has(await q('.cf-toast'), '已开启', '提示');
  await shot('master-switch-on');
});

await t('开启失败：错误显示在弹窗内，横幅与按钮保持，可重试', async () => {
  state.maskingOn = false;
  state.enableError = '数据库暂时不可用';
  await nav();
  await waitFor("document.querySelector('#mk-alerts .mk-alert-act .btn')", '带按钮的横幅');
  await click('#mk-alerts .mk-alert-act .btn');
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  await ev("[...document.querySelectorAll('.cf-dialog button')].find(b => b.textContent.includes('确认开启')).click();");
  await waitFor("document.querySelector('.cf-dialog') && document.querySelector('.cf-dialog').textContent.includes('数据库暂时不可用')", '弹窗内的错误');
  truthy(await q('#mk-alerts .mk-alert-act .btn'), '失败后按钮仍在');
  await ev("[...document.querySelectorAll('.cf-dialog button')].find(b => b.textContent.includes('取消')).click();");
  state.enableError = null;
  state.maskingOn = true;
  // 浏览器会把这次故意制造的 500 记成控制台错误；它是预期的，不能算进最后的“无控制台错误”
  for (let i = consoleErrors.length - 1; i >= 0; i--) if (consoleErrors[i].includes('/settings/masking/enable')) consoleErrors.splice(i, 1);
});

await t('页面无控制台错误', async () => {
  eq(consoleErrors, [], '控制台');
});

await shot('maskadmin-final');
console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
