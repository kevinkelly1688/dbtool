/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_authgroups.mjs
 * @time: 2026/10/28
 */
// 系统管理 › 权限组 页面级测试：权限只剩按钮这一套（没有原权限码矩阵、生效方式单选、严格模式预检、“按原权限码生成”），
// 保存基本信息不带 perm_codes，复制新建会把来源组的按钮授权复制过去，仅超级用户 / 基础按钮不进树只在页面下方注明。
// 假后端 + 真实模板 / 样式 / 脚本 + 无头 Chrome（CDP），不依赖第三方包。
// 用法：node scripts/frontend-test/run_authgroups.mjs [chrome路径]    环境变量 AG_HTTP_PORT（默认 8938）/ AG_CDP_PORT（默认 9281）/ AG_SHOT（截图目录）
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
const HTTP = +(process.env.AG_HTTP_PORT || 8938);
const CDP = +(process.env.AG_CDP_PORT || 9281);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'";
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.json': 'application/json', '.svg': 'image/svg+xml' };

// ---- 假后端 ----
const calls = [];
let mode = 'compat';
const ok = (data) => ({ code: 'ok', message: 'ok', data });
const GROUPS = [
  { id: 1, name: 'DBA', description: '数据库管理员', builtin: true, members: 2 },
  { id: 2, name: 'RD', description: '', builtin: false, members: 1 },
];
const DETAIL = {
  1: { ...GROUPS[0], members: [{ username: 'ann', display: 'Ann', is_active: true }, { username: 'bob', display: 'Bob', is_active: false }], flows: [] },
  2: { ...GROUPS[1], members: [{ username: 'cat', display: 'Cat', is_active: true }], flows: [] },
};
const b = (code, label, kind, extra = {}) => ({ code, label, kind, ...extra });
const REGISTRY = {
  modules: [
    { name: '变更发布', pages: [{ key: 'sqlworkflow', name: '变更工单', route: '/sqlworkflow', buttons: [
      b('sqlworkflow.view_own', '查看自己的工单', 'v'),
      b('sqlworkflow.submit', '提交工单', 'o'),
      b('sqlworkflow.approve', '审批通过', 'o'),
      b('sqlworkflow.execute_group', '立即执行（资源组内）', 'd'),
      b('sqlworkflow.cancel_own', '终止自己的工单', 'o', { open: true }), // 基础按钮：对所有登录用户固定开放
      b('sqlworkflow.skip_check', '跳过语法检查', 'd', { super: true, locked: true, note: '服务端强制仅超级用户' }),
      b('inspect.run', '立即巡检', 'o', { super: true, note: '默认仅超级用户，可显式授予' }),
    ] }] },
    { name: '访问控制', pages: [{ key: 'user', name: '用户', route: '/settings/users', buttons: [
      b('user.view', '查看用户', 'v', { super: true, locked: true }),
      b('user.edit', '新建 / 编辑用户', 'o', { super: true, locked: true }),
    ] }] },
  ],
  presets: { DBA: ['sqlworkflow.view_own', 'sqlworkflow.approve'] }, preset_names: ['DBA'], total: 9, grantable: 5, danger: 1,
};
const GROUP_BTNS = { 1: ['sqlworkflow.view_own', 'sqlworkflow.approve', 'sqlworkflow.skip_check', 'no.such'], 2: ['sqlworkflow.view_own'] };

const USER_VIEW = {
  user: { id: 7, username: 'ann', display: 'Ann', active: true, superuser: false },
  items: [
    { module: '变更发布', page: '变更工单', code: 'sqlworkflow.view_own', label: '查看自己的工单', kind: 'v', locked: false, allowed: true, source: 'group', groups: ['DBA'] },
    { module: '变更发布', page: '变更工单', code: 'sqlworkflow.approve', label: '审批通过', kind: 'o', locked: false, allowed: true, source: 'allow' },
    { module: '变更发布', page: '变更工单', code: 'sqlworkflow.cancel_own', label: '终止自己的工单', kind: 'o', locked: false, allowed: true, source: 'open' },
    { module: '变更发布', page: '变更工单', code: 'sqlworkflow.execute_group', label: '立即执行（资源组内）', kind: 'd', locked: false, allowed: false, source: 'deny' },
    { module: '变更发布', page: '变更工单', code: 'sqlworkflow.skip_check', label: '跳过语法检查', kind: 'd', locked: true, allowed: false, source: 'locked' },
    { module: '变更发布', page: '变更工单', code: 'inspect.run', label: '立即巡检', kind: 'o', locked: false, allowed: false, source: 'none' },
  ],
  overrides: [],
};

function readTpl(rel) { return untranslate(fs.readFileSync(path.join(root, 'web/templates', rel), 'utf8').replace(/\r\n/g, '\n')); }
function page() {
  const t = readTpl('sysadmin/auth_groups.html');
  const ci = t.indexOf('{{define "content"}}'), si = t.indexOf('{{define "scripts"}}');
  const content = t.slice(ci + '{{define "content"}}'.length, si).replace(/\n\{\{end\}\}\s*$/, '');
  const scripts = t.slice(si + '{{define "scripts"}}'.length).replace(/\{\{end\}\}\s*$/, '');
  const head = (t.match(/\{\{define "head"\}\}([\s\S]*?)\{\{end\}\}/) || [, ''])[1];
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="csrf-token" content="t"><title>权限组</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${head}</head><body><div class="app"><div class="main">${content.replace(/\{\{[^}]*\}\}/g, '')}</div></div>${scripts}</body></html>`;
}

const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, 'http://x');
  const p = decodeURIComponent(u.pathname);
  let body = '';
  for await (const c of req) body += c;
  const json = (status, obj) => { res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(obj)); };
  let parsed = null;
  try { parsed = body ? JSON.parse(body) : null; } catch (_) { /* 非 JSON */ }
  if (req.method !== 'GET' && p.startsWith('/api/')) calls.push({ method: req.method, url: p + u.search, body: parsed });
  if (p === '/favicon.ico') { res.writeHead(204); res.end(); return; }
  if (p === '/__mode') { mode = u.searchParams.get('v') || 'compat'; json(200, {}); return; }
  if (p === '/__calls') { json(200, calls); return; }
  if (p === '/__clear') { calls.length = 0; json(200, {}); return; }
  if (p === '/page') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' }); res.end(page()); return; }
  if (p.startsWith('/static/')) {
    const f = path.join(staticRoot, p.slice(8));
    if (f.startsWith(staticRoot) && fs.existsSync(f) && fs.statSync(f).isFile()) {
      res.writeHead(200, { 'Content-Type': MIME[path.extname(f)] || 'application/octet-stream', 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' });
      fs.createReadStream(f).pipe(res);
      return;
    }
    res.writeHead(404); res.end('nf'); return;
  }
  const API = '/api/v1/settings/auth-groups';
  const BP = '/api/v1/settings/button-perms';
  let m;
  if (p === '/api/v1/button-perms/me') return json(200, ok({ mode, deny_display: 'hide', superuser: true, allowed: [] }));
  if (p === API && req.method === 'GET') return json(200, ok(GROUPS));
  if (p === API && req.method === 'POST') return json(200, ok({ id: 3 }));
  if ((m = p.match(/^\/api\/v1\/settings\/auth-groups\/(\d+)$/)) && req.method === 'GET') {
    const id = Number(m[1]);
    if (id === 3) return json(200, ok({ id: 3, name: 'DBA 副本', description: '数据库管理员', builtin: false, members: [], flows: [] }));
    return DETAIL[id] ? json(200, ok(DETAIL[id])) : json(404, { code: 'not_found', message: '权限组不存在' });
  }
  if ((m = p.match(/^\/api\/v1\/settings\/auth-groups\/(\d+)$/)) && req.method === 'PUT') return json(200, ok({}));
  if (p === BP + '/registry') return json(200, ok({ ...REGISTRY, setting: { mode, deny_display: 'hide' }, tables_ready: true }));
  if ((m = p.match(/^\/api\/v1\/settings\/button-perms\/groups\/(\d+)$/)) && req.method === 'GET') return json(200, ok({ group: GROUPS.find((g) => g.id === Number(m[1])) || { id: 3, name: 'DBA 副本', members: 0 }, codes: GROUP_BTNS[Number(m[1])] || [] }));
  if ((m = p.match(/^\/api\/v1\/settings\/button-perms\/groups\/(\d+)$/)) && req.method === 'PUT') return json(200, ok({ diff: { added: parsed.codes, removed: [] }, warnings: [], members: 0, total: parsed.codes.length, changed: true }));
  if (p === BP + '/setting' && req.method === 'PUT') return json(200, ok({ mode, deny_display: parsed.deny_display }));
  if (p === BP + '/users/7' && req.method === 'GET') return json(200, ok(USER_VIEW));
  if (p === '/api/v1/settings/users') return json(200, ok({ rows: [{ id: 7, username: 'ann', display: 'Ann', is_active: true }], total: 1 }));
  res.writeHead(404); res.end('nf');
}).listen(HTTP);

// ---- Chrome ----
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'ag-chrome-'));
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
  } else if (d.method === 'Log.entryAdded' && d.params.entry.level === 'error' && !/favicon/.test(d.params.entry.url || '')) {
    consoleErrors.push(d.params.entry.text + ' ' + (d.params.entry.url || ''));
  }
});
const send = (method, params = {}) => new Promise((res, rej) => { const i = ++seq; pend.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
await send('Runtime.enable'); await send('Page.enable'); await send('Log.enable');
await send('Emulation.setDeviceMetricsOverride', { width: 1366, height: 1000, deviceScaleFactor: 1, mobile: false });

async function ev(expr) {
  const r = await send('Runtime.evaluate', { expression: `(async()=>{${expr}})()`, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error('页面异常：' + ((r.exceptionDetails.exception || {}).description || r.exceptionDetails.text));
  return r.result.value;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function shot(name) {
  if (!process.env.AG_SHOT) return;
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.mkdirSync(process.env.AG_SHOT, { recursive: true });
  fs.writeFileSync(path.join(process.env.AG_SHOT, name + '.png'), Buffer.from(r.data, 'base64'));
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
const eq = (a, b2, m) => { if (JSON.stringify(a) !== JSON.stringify(b2)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b2)} 实得 ${JSON.stringify(a)}`); };
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };
const getCalls = async () => (await fetch(`http://127.0.0.1:${HTTP}/__calls`)).json();
const clearCalls = () => fetch(`http://127.0.0.1:${HTTP}/__clear`);

async function open(m = 'compat') {
  await fetch(`http://127.0.0.1:${HTTP}/__mode?v=${m}`);
  await clearCalls();
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/page` });
  await waitFor("document.querySelectorAll('#ag-list button').length === 2 && document.querySelectorAll('#bp-tree .bp-page').length > 0", '页面加载');
  await sleep(150);
}

console.log('权限组页面（权限只剩按钮这一套）');
await open('compat');

await t('没有原权限码矩阵、生效方式单选、严格模式预检、“按原权限码生成”；基本信息页签只剩名称 / 说明', async () => {
  for (const id of ['ag-matrix', 'ag-q', 'bp-mode-compat', 'bp-mode-strict', 'bp-migrate', 'm-bp-strict', 'm-bp-migrate', 'au-rows', 'au-reason', 'au-only']) {
    eq(await ev(`return !!document.getElementById('${id}')`), false, id + ' 应已删除');
  }
  eq(await ev("return [...document.querySelectorAll('#ag-main .tabbar .tab')].map((x) => x.textContent.trim())"), ['基本信息', '成员', '按用户查看有效权限', '按钮权限']);
  eq(await ev("return document.getElementById('ag-name').value + '|' + document.getElementById('ag-desc').value"), 'DBA|数据库管理员');
  eq(await ev("return document.getElementById('ag-name').disabled"), true, '内置组不能改名');
});

await t('还没迁移时提示去跑 dbt-admin perm migrate；迁移后不再显示', async () => {
  eq(await ev("return document.getElementById('bp-compat').hidden"), false);
  eq(await ev("return document.getElementById('bp-compat').textContent.includes('dbt-admin perm migrate')"), true);
  await open('strict');
  eq(await ev("return document.getElementById('bp-compat').hidden"), true);
  await open('compat');
});

await t('按钮树：只列可授予的按钮，没有“原：…”标签；仅超级用户的和基础按钮只在页面下方注明', async () => {
  await ev("document.querySelector('#ag-main .tab[data-tab=\"btn\"]').click()");
  const r = await ev(`const pg = (k) => document.querySelector('#bp-tree .bp-page[data-page="' + k + '"]');
    const labels = (k) => [...pg(k).querySelectorAll('label.bp')].map((l) => l.textContent.replace(/\\s+/g, ' ').trim());
    return { wf: labels('sqlworkflow'), wfNotes: [...pg('sqlworkflow').querySelectorAll('.bp-note')].map((n) => n.textContent),
      userShown: pg('user').offsetParent !== null && pg('user').closest('details').offsetParent !== null, userBoxes: pg('user').querySelectorAll('input').length, userNotes: [...pg('user').querySelectorAll('.bp-note')].map((n) => n.textContent), userBadge: pg('user').querySelector('.bp-head').textContent,
      text: document.getElementById('bp-tree').textContent };`);
  eq(r.wf.length, 5, '5 个可授予的按钮（不含跳过语法检查、终止自己的工单）');
  truthy(!r.text.includes('原：'), '树里不应再出现“原：…”标签');
  truthy(r.wf.some((x) => x.includes('立即巡检') && x.includes('默认仅超级用户')), '默认仅超管但可授予的按钮带提示: ' + r.wf.join('|'));
  eq(r.wfNotes, ['仅超级用户：跳过语法检查', '所有登录用户固定开放：终止自己的工单']);
  eq(r.userShown, true, '整页仅超级用户的页面也要显示出来（不能被搜索过滤掉）');
  eq(r.userBoxes, 0, '整页仅超级用户：没有复选框');
  eq(r.userNotes, ['仅超级用户：查看用户、新建 / 编辑用户']);
  truthy(r.userBadge.includes('仅超管'), '整页仅超管有徽标');
  await shot('buttons');
});

await t('搜索：按页面 / 按钮 / 权限码过滤；整页仅超级用户的页面按页面名或其中的按钮名匹配', async () => {
  const q = async (v) => ev(`const i = document.getElementById('bp-q'); i.value = ${JSON.stringify(v)}; i.dispatchEvent(new Event('input')); return [...document.querySelectorAll('#bp-tree .bp-page')].filter((p) => p.offsetParent !== null).map((p) => p.dataset.page);`);
  eq(await q(''), ['sqlworkflow', 'user']);
  eq(await q('审批'), ['sqlworkflow']);
  eq(await q('新建 / 编辑用户'), ['user']);
  eq(await q('不存在的东西'), []);
  eq(await q(''), ['sqlworkflow', 'user']);
});

await t('已授予的按钮勾上；锁定的 / 未登记的码被忽略，不进计数', async () => {
  const r = await ev(`return { checked: [...document.querySelectorAll('#bp-tree input[data-code]:checked')].map((c) => c.dataset.code).sort(), total: document.getElementById('bp-total').textContent, all: document.getElementById('bp-all').textContent };`);
  eq(r.checked, ['sqlworkflow.approve', 'sqlworkflow.view_own']);
  eq([r.total, r.all], ['2', '5']);
});

await t('保存基本信息：只发名称和说明，不带 perm_codes', async () => {
  await ev("document.querySelector('#ag-main .tab[data-tab=\"basic\"]').click(); document.getElementById('ag-desc').value = '新的说明'; document.getElementById('ag-save').click();");
  await waitFor("document.getElementById('ag-msg').textContent.includes('已保存')", '保存提示');
  const put = (await getCalls()).find((c) => c.method === 'PUT' && c.url.endsWith('/auth-groups/1'));
  truthy(put, '应有 PUT');
  eq(put.body, { name: 'DBA', description: '新的说明' });
});

await t('保存“无权限的按钮”显示方式：只发 deny_display（迁移状态由 dbt-admin perm migrate 写，页面不改）', async () => {
  await clearCalls();
  await ev("document.getElementById('bp-deny').value = 'disable'; document.getElementById('bp-setting-save').click();");
  await waitFor("true", 'x', 50);
  await sleep(300);
  const put = (await getCalls()).find((c) => c.url.endsWith('/button-perms/setting'));
  truthy(put, '应有 PUT /setting');
  eq(put.body, { deny_display: 'disable' });
});

await t('新建权限组：只发名称和说明；复制新建：创建后把来源组的按钮授权（去掉锁定的和未登记的）复制过去', async () => {
  await clearCalls();
  await ev("document.getElementById('ag-new').click();");
  await ev("document.getElementById('ag-name').value = 'QA'; document.getElementById('ag-desc').value = ''; document.getElementById('ag-save').click();");
  await waitFor("true", 'x', 50);
  await sleep(400);
  let cs = await getCalls();
  const post = cs.find((c) => c.method === 'POST' && c.url.endsWith('/auth-groups'));
  eq(post.body, { name: 'QA', description: '' });
  eq(cs.filter((c) => c.url.includes('/button-perms/groups/')).length, 0, '不是复制：不写按钮');

  await open('compat');
  await clearCalls();
  await ev("document.getElementById('ag-copy').click();");
  eq(await ev("return document.getElementById('ag-name').value"), 'DBA 副本');
  await ev("document.getElementById('ag-save').click();");
  await waitFor("true", 'x', 50);
  await sleep(500);
  cs = await getCalls();
  const put = cs.find((c) => c.method === 'PUT' && c.url.endsWith('/button-perms/groups/3'));
  truthy(put, '复制后应写按钮授权: ' + JSON.stringify(cs.map((c) => c.method + ' ' + c.url)));
  eq(put.body.codes, ['sqlworkflow.view_own', 'sqlworkflow.approve']);
  truthy(put.body.reason.includes('DBA'), '原因里写明来源: ' + put.body.reason);
});

await t('“按用户查看有效权限”页签：只有用户选择和按钮级有效权限，没有旧权限码的表格', async () => {
  await open('compat');
  await ev("document.querySelector('#ag-main .tab[data-tab=\"user\"]').click()");
  eq(await ev("return [...document.querySelectorAll('[data-pane=\"user\"] h3, [data-pane=\"user\"] h4')].map((h) => h.textContent.replace(/\\s+/g, ' ').trim())"), ['有效权限', '单独授权 / 禁用记录']);
  eq(await ev("return !!document.getElementById('au-user') && !!document.getElementById('bu-rows')"), true);
});

await t('选择用户后：来源汇总写“登录即可”，没有“原权限 / 严格模式 / 兼容模式”；每一行的来源说明正确', async () => {
  await ev("document.querySelector('#ag-main .tab[data-tab=\"user\"]').click(); document.getElementById('au-search').click();");
  await waitFor("document.querySelectorAll('#au-user option').length > 1", '用户下拉');
  await ev("const s = document.getElementById('au-user'); s.value = '7'; s.dispatchEvent(new Event('change'));");
  await waitFor("document.querySelectorAll('#bu-rows tr').length === 6", '有效权限行');
  const r = await ev(`return { sum: document.getElementById('bu-sum').textContent, info: document.getElementById('bu-info').textContent,
    src: [...document.querySelectorAll('#bu-rows tr')].map((tr) => tr.lastElementChild.textContent.trim()) };`);
  eq(r.sum, '共 6 个按钮，允许 3：权限组 1 · 单独授权 1 · 登录即可 1 · 被单独禁用 1');
  eq(r.info, 'ann');
  eq(r.src, ['权限组 DBA', '单独授权', '登录即可', '单独禁用', '仅超级用户', '未授予']);
  truthy(!/原权限|严格模式|兼容模式/.test(r.sum + r.info + r.src.join('')), '不应再出现旧权限码 / 生效方式字样');
  await shot('user-effective');
});

await t('从用户详情里的链接（#user=7）进入：用户下拉自动补全并选中该用户，直接显示有效权限', async () => {
  await clearCalls();
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/page?from=drawer#user=7` });
  await waitFor("document.querySelectorAll('#bu-rows tr').length === 6", '有效权限行');
  eq(await ev("return document.getElementById('au-user').value"), '7');
  eq(await ev("return document.getElementById('bu-info').textContent"), 'ann');
  eq(await ev("return document.getElementById('ag-msg').textContent.trim()"), '', '不应有错误提示');
});

await t('用户详情抽屉的脚本能加载（语法 / 依赖没坏）', async () => {
  const ok = await ev("const m = await import('/static/js/sysadmin/user_drawer.js'); return Object.keys(m).length > 0;");
  eq(ok, true);
});

await t('全程无控制台错误', async () => { eq(consoleErrors, []); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
