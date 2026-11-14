/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_sysconfig_edit.mjs
 * @time: 2026/10/28
 */
// 系统配置页“修改”功能的浏览器回归（页面：web/templates/sysconfig/index.html + web/static/js/sysconfig/main.js，
// 接口：internal/sysconfig/http.go、internal/settings）：
//   每项按类型给控件（数字框 / 开关 / 下拉 / 文本 / 密码框），改动先成为“草稿”（行高亮 + 底部“未保存”条，可放弃）；
//   “保存修改”弹统一确认组件（差异对比，高危项要键入“确认修改”），确认后才 PUT /api/v1/settings/config，请求体是 {changes}；
//   恢复默认 = 提交 null；敏感项只显示“已设置”、不回显内容；只读（locked）项控件禁用；服务端校验错误留在确认框并标在对应行；
//   “变更记录”抽屉（全部 / 单项，分页加载）。
// 与旧版（表格里逐项“修改”按钮 + 单项 PUT {value}）的差别：当前页面是“分组导航 + 批量草稿保存”，没有逐行编辑行，
// 旧用例里“取消不发请求 / 值没变 / 下拉含恢复默认空选项”分别对应现在的“放弃修改 / 改回原值不算修改 / 恢复默认按钮”。
// 最小页面（与 web/templates/sysconfig/index.html 同样的元素 id）+ 真实静态文件 + 假接口 + 无头 Chrome（CDP）。
// 用法：node scripts/frontend-test/run_sysconfig_edit.mjs [chrome路径]    环境变量 SE_HTTP_PORT（默认 8932）/ SE_CDP_PORT（默认 9269）。
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const staticRoot = path.resolve(here, '../../web/static');
const HTTP = +(process.env.SE_HTTP_PORT || 8932);
const CDP = +(process.env.SE_CDP_PORT || 9269);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';

const PAGE = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="csrf-token" content="tok"><link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/sysconfig.css"></head>
<body><main class="page st" id="st-main"><div class="page-head"><h1>系统配置</h1><span class="spacer"></span><button type="button" class="btn ghost" id="st-hist-all">变更记录</button></div>
<div id="st-alerts"></div>
<div class="st-layout"><nav class="st-nav" id="st-nav"></nav>
<section class="st-body"><div class="st-search"><input id="st-q" type="search"><label class="st-chk"><input type="checkbox" id="st-only-mod"> 只看已修改</label></div><div id="st-content"></div></section></div>
<div class="st-bar" id="st-bar" hidden><span id="st-bar-text"></span><span class="spacer"></span><button type="button" class="btn ghost" id="st-discard">放弃修改</button><button type="button" class="btn pri" id="st-save">保存修改</button></div>
<div class="st-drawer" id="st-drawer" hidden><div class="st-drawer-panel"><div class="st-drawer-head"><h2 id="st-drawer-title">变更记录</h2><span class="spacer"></span><button type="button" class="btn ghost" id="st-drawer-close">✕</button></div>
<div class="st-drawer-body" id="st-drawer-body"></div><div class="st-drawer-foot"><button type="button" class="btn ghost" id="st-drawer-more" hidden>加载更多</button></div></div></div>
</main><script type="module" src="/static/js/sysconfig/main.js"></script></body></html>`;

// ---- 假数据：形状同 internal/settings 的 Item（Def + 运行时字段） ----
const LOCK = '指向服务器上的程序 / 配置文件，或含目标库账号密码：为避免页面被滥用时让服务执行任意程序，只能用 dbt-admin settings set --force 修改';
const mk = (o) => ({ section: '', default: '', value: '', effective: o.default || '', custom: false, modified: false, secret_set: false, undecryptable: false, version: 0, editable: !o.locked, ...o });
const items = [
  mk({ key: 'sms.aliyun_access_key_secret', group: 'sms', section: '阿里云', label: 'AccessKey Secret', type: 'secret', secret_set: true, custom: true, modified: true, version: 1, updated_by: 'admin', updated_at: '2026-10-09T08:00:00Z' }),
  mk({ key: 'sms.tencent_secret_key', group: 'sms', section: '腾讯云', label: 'SecretKey', type: 'secret' }),
  mk({ key: 'query.mask_enabled', group: 'query', section: '脱敏', label: '查询结果脱敏总开关', type: 'bool', default: 'false', help: '关闭后在线查询返回原值，脱敏规则暂不生效。',
    link: { label: '数据脱敏', href: '/settings/masking' }, value: 'true', effective: 'true', custom: true, modified: true, version: 1, updated_by: 'admin', updated_at: '2026-10-08T08:00:00Z' }),
  mk({ key: 'query.forbid_select_star', group: 'query', section: '查询限制', label: '禁止 select *', type: 'bool', default: 'false' }),
  mk({ key: 'firewall.on_error', group: 'firewall', section: '全局', label: '防火墙自身出错时', type: 'enum', default: 'allow', options: [{ value: 'allow', label: '放行' }, { value: 'deny', label: '拒绝' }] }),
  mk({ key: 'export.local_path', group: 'export', section: '存储', label: '本地导出目录', type: 'string', default: 'downloads/DataExportFile/', help: '相对路径时相对环境变量 EXPORT_BASE_DIR；不能含 ..。' }),
  mk({ key: 'export.max_rows', group: 'export', section: '上限', label: '导出行数上限', type: 'int', default: '10000', unit: '行', ranged: true, min: 1, max: 10000000,
    value: '20000', effective: '20000', custom: true, modified: true, version: 2, updated_by: 'admin', updated_at: '2026-10-11T08:00:00Z' }),
  mk({ key: 'partition.auto_drop', group: 'partition', section: '自动维护', label: '自动删除过期分区', type: 'bool', default: 'false', risk: '过期分区里的数据会被删除，不可恢复' }),
  mk({ key: 'optimizer.soar_path', group: 'optimizer', section: 'SOAR', label: 'SOAR 程序路径', type: 'string', locked: LOCK, edit_note: LOCK }),
];
const GROUPS = [
  { key: 'sms', label: '短信', desc: '短信两步验证使用的短信服务商与密钥' },
  { key: 'query', label: '在线查询与脱敏', desc: '查询超时、行数 / 字节上限、结果脱敏' },
  { key: 'firewall', label: 'SQL 防火墙', desc: '总开关、全局模式与出错策略' },
  { key: 'export', label: '数据导出', desc: '导出文件的存放位置与上限' },
  { key: 'partition', label: '分区维护', desc: '分区表自动续建与过期删除' },
  { key: 'optimizer', label: 'SQL 优化工具', desc: 'SOAR 程序路径与测试环境（只能用命令行修改）' },
];
const state = { ready: true, publicKey: false, undecryptable: [] };
const calls = []; // 每次 PUT：{ method, url, body, csrf }
const gets = []; // 变更记录接口的 GET 地址
let failNext = null;
let hid = 0;
// 先放 55 条旧记录（别的键），用来验证“加载更多”（每页 50）。
const history = [];
for (let i = 0; i < 55; i++) history.push({ id: ++hid, key: 'security.login_lock_attempts', label: '连续失败锁定次数', old: String(i + 3), new: String(i + 4), secret: false, action: 'set', by: 'seed', changed_at: '2026-09-01T08:00:00Z' });
history.reverse();

function view() {
  const groups = GROUPS.map((g) => {
    const its = items.filter((i) => i.group === g.key);
    return { ...g, total: its.length, modified: its.filter((i) => i.modified).length, issues: its.filter((i) => i.undecryptable).length };
  });
  return { ready: state.ready, groups, items: state.ready ? items : [], security: { public_secret_key: state.publicKey, separate_mirage_key: true }, undecryptable: state.undecryptable };
}

// 与 settings.Service.SetMany 一致的语义：null / '' = 恢复默认（敏感项 = 清除）；值没变的不记；校验不过整批不写。
function apply(changes) {
  const fields = {};
  for (const [k, v] of Object.entries(changes)) {
    const it = items.find((i) => i.key === k);
    if (!it) { fields[k] = '没有这个设置项'; continue; }
    if (it.locked) { fields[k] = it.locked; continue; }
    if (v !== null && v !== '' && it.type === 'int') {
      const n = Number(v);
      if (!Number.isInteger(n)) fields[k] = '需要一个整数';
      else if (it.ranged && (n < it.min || n > it.max)) fields[k] = `需要在 ${it.min} 到 ${it.max} 之间`;
    }
  }
  if (Object.keys(fields).length) return { fields };
  const done = [];
  for (const [k, v] of Object.entries(changes)) {
    const it = items.find((i) => i.key === k);
    const secret = it.type === 'secret';
    const reset = v === null || v === '';
    const old = secret ? '' : it.value;
    if (reset) {
      if (!it.custom) continue;
      it.custom = false; it.value = ''; it.secret_set = false; it.modified = false; it.effective = it.default;
    } else {
      if (!secret && it.custom && it.value === v) continue;
      it.custom = true;
      if (secret) { it.secret_set = true; it.modified = true; } else { it.value = v; it.effective = v; it.modified = v !== it.default; }
    }
    it.version++; it.updated_by = 'admin'; it.updated_at = '2026-10-14T08:00:00Z';
    done.push({ key: k, label: it.label, old, new: reset || secret ? '' : v, secret, reset });
    history.unshift({ id: ++hid, key: k, label: it.label, old, new: reset || secret ? '' : v, secret, action: reset ? 'reset' : 'set', by: 'admin', changed_at: '2026-10-14T08:00:00Z' });
  }
  return { changes: done };
}

const json = (res, status, obj) => { res.writeHead(status, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(obj)); };
const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, 'http://x');
  let body = '';
  for await (const c of req) body += c;
  if (u.pathname === '/') { res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(PAGE); return; }
  if (u.pathname === '/api/v1/settings/config' && req.method === 'GET') return json(res, 200, { data: view() });
  if (u.pathname === '/api/v1/settings/config' && req.method === 'PUT') {
    const parsed = JSON.parse(body);
    calls.push({ method: 'PUT', url: u.pathname, body: parsed, csrf: req.headers['x-csrf-token'] });
    if (failNext) {
      const f = failNext; failNext = null;
      return json(res, f.status, { code: f.code, message: f.message, data: f.fields ? { fields: f.fields } : undefined });
    }
    const r = apply(parsed.changes || {});
    if (r.fields) {
      const ks = Object.keys(r.fields);
      return json(res, 400, { code: 'bad_value', message: ks.length === 1 ? `${ks[0]}：${r.fields[ks[0]]}` : `有 ${ks.length} 项的值不合法`, data: { fields: r.fields } });
    }
    return json(res, 200, { data: { changes: r.changes, changed: r.changes.length > 0 } });
  }
  if (u.pathname === '/api/v1/settings/config/history' && req.method === 'GET') {
    gets.push(u.pathname + u.search);
    const key = u.searchParams.get('key') || '';
    const limit = +(u.searchParams.get('limit') || 50);
    const offset = +(u.searchParams.get('offset') || 0);
    const rows = history.filter((h) => !key || h.key === key);
    return json(res, 200, { data: { items: rows.slice(offset, offset + limit), limit, offset } });
  }
  const f = path.join(staticRoot, u.pathname.replace(/^\/static\//, ''));
  if (u.pathname.startsWith('/static/') && f.startsWith(staticRoot) && fs.existsSync(f) && fs.statSync(f).isFile()) {
    res.writeHead(200, { 'Content-Type': f.endsWith('.js') ? 'text/javascript' : f.endsWith('.css') ? 'text/css' : 'application/octet-stream' });
    res.end(fs.readFileSync(f));
    return;
  }
  res.writeHead(404);
  res.end('nf');
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'se-chrome-'));
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
await send('Emulation.setDeviceMetricsOverride', { width: 1366, height: 900, deviceScaleFactor: 1, mobile: false });
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
const has = (s, sub, m) => { if (!String(s).includes(sub)) throw new Error(`${m || ''} 应包含 ${JSON.stringify(sub)}，实得 ${JSON.stringify(String(s).slice(0, 300))}`); };
const dlg = "document.querySelector('.cf-dialog')"; // 统一确认组件
const waitFor = async (expr, ms = 3000) => { for (let i = 0; i < ms / 50; i++) { if (await ev(`return !!(${expr});`).catch(() => false)) return; await sleep(50); } throw new Error('等待超时：' + expr); };
const waitCalls = async (n) => { for (let i = 0; i < 60 && calls.length < n; i++) await sleep(50); };

await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/` });
await waitFor("document.querySelector('#st-nav button[data-group]') && document.querySelector('#st-content .st-row')");
console.log('系统配置页“修改”测试');

const rowSel = (k) => `.st-row[data-key="${k}"]`;
const row = (k) => `document.querySelector(${JSON.stringify(rowSel(k))})`;
const field = (k, tag = 'input') => `document.querySelector(${JSON.stringify(rowSel(k) + ' ' + tag)})`;
const go = async (g) => {
  await ev(`document.querySelector(${JSON.stringify(`#st-nav button[data-group="${g}"]`)}).click();`);
  await waitFor(`document.querySelector(${JSON.stringify(`#st-nav button.on[data-group="${g}"]`)})`);
};
const typeInto = (k, v, tag = 'input') => ev(`const e = ${field(k, tag)}; e.value = ${JSON.stringify(v)}; e.dispatchEvent(new Event('input', { bubbles: true }));`);
const barShown = () => ev("return !document.getElementById('st-bar').hidden;");
const barText = () => ev("return document.getElementById('st-bar-text').textContent;");
const confirmBtn = ".cf-foot .cf-btn:last-child";
const cancelBtn = ".cf-foot .cf-btn:first-child";

await t('页面按分组列出设置项；只读（locked）项控件禁用并说明原因；有专门页面的项带链接', async () => {
  await go('export');
  eq(await ev(`return [...document.querySelectorAll('#st-content .st-row')].map((r) => r.dataset.key);`), ['export.local_path', 'export.max_rows']);
  eq(await ev(`return ${field('export.max_rows')}.disabled;`), false, '可改项控件可用');
  await go('optimizer');
  const r = await ev(`const r = ${row('optimizer.soar_path')}; return [r.classList.contains('locked'), r.querySelector('input').disabled, r.textContent];`);
  eq([r[0], r[1]], [true, true]);
  has(r[2], '只读');
  has(r[2], '页面里不能修改：指向服务器上的程序');
  await go('query');
  eq(await ev(`const a = ${row('query.mask_enabled')}.querySelector('a'); return [a.getAttribute('href'), a.textContent];`), ['/settings/masking', '前往「数据脱敏」页']);
  eq(await ev(`return ${field('query.mask_enabled')}.disabled;`), false, '有专门页面的项在本页仍可改');
});

await t('数值项：带范围的数字框；改动出现未保存条；放弃修改恢复原值且不发请求', async () => {
  await go('export');
  eq(await ev(`const e = ${field('export.max_rows')}; return [e.type, e.min, e.max, e.value, e.placeholder];`), ['number', '1', '10000000', '20000', '10000']);
  eq(await barShown(), false, '没改动时没有未保存条');
  await typeInto('export.max_rows', '30000');
  eq(await barShown(), true);
  has(await barText(), '有 1 项修改尚未保存');
  eq(await ev(`return ${row('export.max_rows')}.classList.contains('dirty');`), true, '被改的行高亮');
  eq(await ev("return !!document.querySelector('#st-nav button[data-group=\"export\"] .st-n-dot');"), true, '分组导航上有未保存的小点');
  await ev("document.getElementById('st-discard').click();");
  eq(await barShown(), false);
  eq(await ev(`return ${field('export.max_rows')}.value;`), '20000', '放弃后回到服务端的值');
  eq(await ev("return !!document.querySelector('.st-row.dirty, .st-n-dot');"), false);
  eq(calls.length, 0);
});

await t('保存：先弹差异确认，确认后才发 PUT，请求体是 {changes}，页面刷新成新值', async () => {
  await typeInto('export.max_rows', '500000');
  await ev("document.getElementById('st-save').click();");
  await waitFor(dlg);
  const text = await ev(`return ${dlg}.textContent;`);
  for (const k of ['保存 1 项系统设置', 'export.max_rows', '20000', '500000']) has(text, k, '差异确认里');
  eq(await ev(`return ${dlg}.classList.contains('cf-high');`), false, '非高危项不按高危弹窗');
  eq(calls.length, 0, '确认前不应发请求');
  await ev(`document.querySelector(${JSON.stringify(confirmBtn)}).click();`);
  await waitCalls(1);
  eq(calls.length, 1);
  eq(calls[0].method, 'PUT');
  eq(calls[0].body, { changes: { 'export.max_rows': '500000' } });
  eq(calls[0].csrf, 'tok', '带 CSRF 令牌');
  await waitFor(`!${dlg} && document.getElementById('st-bar').hidden && ${field('export.max_rows')}.value === '500000'`);
  has(await ev("return document.querySelector('.cf-toasts').textContent;"), '已保存 1 项');
  eq(await ev("return !!document.querySelector('.st-row.dirty');"), false);
});

await t('改回服务端当前值：不算修改，不出现未保存条；没有草稿时点保存什么也不发生', async () => {
  calls.length = 0;
  await go('query');
  await ev(`${field('query.forbid_select_star')}.click();`); // false → true
  eq(await barShown(), true);
  eq(await ev(`return ${row('query.forbid_select_star')}.querySelector('.st-switch span').textContent;`), '开启');
  await ev(`${field('query.forbid_select_star')}.click();`); // 改回 false
  eq(await barShown(), false);
  await ev("document.getElementById('st-save').click();");
  await sleep(300);
  eq(await ev(`return !!${dlg};`), false, '没有改动不弹确认');
  eq(calls.length, 0);
});

await t('取值集合：下拉给出全部取值；改了再改回不算修改', async () => {
  await go('firewall');
  eq(await ev(`const e = ${field('firewall.on_error', 'select')}; return [e.tagName, [...e.options].map((o) => o.value), [...e.options].map((o) => o.textContent), e.value];`),
    ['SELECT', ['allow', 'deny'], ['放行', '拒绝'], 'allow']);
  await ev(`const e = ${field('firewall.on_error', 'select')}; e.value = 'deny'; e.dispatchEvent(new Event('change', { bubbles: true }));`);
  eq(await barShown(), true);
  await ev(`const e = ${field('firewall.on_error', 'select')}; e.value = 'allow'; e.dispatchEvent(new Event('change', { bubbles: true }));`);
  eq(await barShown(), false);
});

await t('恢复默认：已修改项有“恢复默认”按钮，确认框显示“默认”，提交的值是 null', async () => {
  calls.length = 0;
  await go('export');
  eq(await ev(`return !!${row('export.local_path')}.querySelector('button[data-act="reset"]');`), false, '没改过的项没有恢复默认');
  await ev(`${row('export.max_rows')}.querySelector('button[data-act="reset"]').click();`);
  eq(await barShown(), true);
  eq(await ev(`return ${field('export.max_rows')}.value;`), '', '输入框清空，占位显示默认值');
  await ev("document.getElementById('st-save').click();");
  await waitFor(dlg);
  const text = await ev(`return ${dlg}.textContent;`);
  has(text, '500000');
  has(text, '10000（默认）');
  await ev(`document.querySelector(${JSON.stringify(confirmBtn)}).click();`);
  await waitCalls(1);
  eq(calls[0].body, { changes: { 'export.max_rows': null } });
  await waitFor(`!${dlg} && document.getElementById('st-bar').hidden && !${row('export.max_rows')}.querySelector('button[data-act="reset"]')`);
  eq(await ev(`return ${field('export.max_rows')}.value;`), '');
});

await t('高危项：确认框要求键入“确认修改”，没键入前确认钮禁用，并列出高危原因', async () => {
  calls.length = 0;
  await go('partition');
  has(await ev(`return ${row('partition.auto_drop')}.textContent;`), '高危');
  await ev(`${field('partition.auto_drop')}.click();`);
  await ev("document.getElementById('st-save').click();");
  await waitFor(dlg);
  const text = await ev(`return ${dlg}.textContent;`);
  for (const k of ['partition.auto_drop', '过期分区里的数据会被删除，不可恢复', '确认修改']) has(text, k, '高危确认框里');
  eq(await ev(`return ${dlg}.classList.contains('cf-high');`), true);
  eq(await ev(`return document.querySelector(${JSON.stringify(confirmBtn)}).disabled;`), true, '未键入时确认钮应禁用');
  await ev("const i = document.querySelector('.cf-dialog input'); i.value = '确认修改'; i.dispatchEvent(new Event('input', { bubbles: true }));");
  await sleep(100);
  eq(await ev(`return document.querySelector(${JSON.stringify(confirmBtn)}).disabled;`), false, '键入后确认钮可用');
  eq(calls.length, 0);
  await ev(`document.querySelector(${JSON.stringify(confirmBtn)}).click();`);
  await waitCalls(1);
  eq(calls[0].body, { changes: { 'partition.auto_drop': 'true' } });
  await waitFor(`!${dlg} && ${field('partition.auto_drop')}.checked`);
  has(await ev(`return ${row('partition.auto_drop')}.textContent;`), '已修改');
});

await t('服务端校验失败：错误留在确认框里不关闭，对应行标出原因，取消后草稿保留，再改就清掉提示', async () => {
  calls.length = 0;
  failNext = { status: 400, code: 'bad_value', message: 'export.max_rows：需要在 1 到 10000000 之间', fields: { 'export.max_rows': '需要在 1 到 10000000 之间' } };
  await go('export');
  await typeInto('export.max_rows', '99');
  await ev("document.getElementById('st-save').click();");
  await waitFor(dlg);
  await ev(`document.querySelector(${JSON.stringify(confirmBtn)}).click();`);
  await waitFor(`${dlg} && ${dlg}.textContent.includes('需要在 1 到 10000000 之间')`);
  eq(await ev(`return !!${dlg};`), true, '失败时弹窗应保留');
  eq(await ev(`return document.querySelector('.cf-err').hidden;`), false);
  await waitFor(`${row('export.max_rows')}.querySelector('.st-err')`);
  eq(await ev(`return ${row('export.max_rows')}.querySelector('.st-err').textContent;`), '需要在 1 到 10000000 之间');
  await ev(`document.querySelector(${JSON.stringify(cancelBtn)}).click();`);
  await waitFor(`!${dlg}`);
  eq(await barShown(), true, '取消后草稿还在');
  eq(await ev(`return ${field('export.max_rows')}.value;`), '99');
  await typeInto('export.max_rows', '100');
  eq(await ev(`return !!${row('export.max_rows')}.querySelector('.st-err');`), false, '再改就清掉该项提示');
  await ev("document.getElementById('st-discard').click();");
  eq(await barShown(), false);
});

await t('服务端逐项校验：越界值被拒（400 + fields），页面不写入', async () => {
  calls.length = 0;
  await typeInto('export.max_rows', '0');
  await ev("document.getElementById('st-save').click();");
  await waitFor(dlg);
  await ev(`document.querySelector(${JSON.stringify(confirmBtn)}).click();`);
  await waitFor(`${row('export.max_rows')}.querySelector('.st-err')`);
  eq(calls.length, 1);
  eq(await ev(`return ${row('export.max_rows')}.querySelector('.st-err').textContent;`), '需要在 1 到 10000000 之间');
  await ev(`document.querySelector(${JSON.stringify(cancelBtn)}).click();`);
  await waitFor(`!${dlg}`);
  await ev("document.getElementById('st-discard').click();");
});

await t('密钥项：已设置的不回显，点“修改”才出现密码框；空值不成草稿；确认框不含内容', async () => {
  calls.length = 0;
  await go('sms');
  const k = 'sms.aliyun_access_key_secret';
  eq(await ev(`return ${row(k)}.querySelectorAll('input').length;`), 0);
  // 没设置过的密钥直接给密码框
  eq(await ev(`const e = ${field('sms.tencent_secret_key')}; return [e.type, e.placeholder];`), ['password', '输入值']);
  await ev(`${row(k)}.querySelector('button[data-act="secret-edit"]').click();`);
  eq(await ev(`const e = ${field(k)}; return [e.type, e.placeholder, e.value];`), ['password', '输入新值（留空不修改）', '']);
  await typeInto(k, '');
  eq(await barShown(), false, '空值不算修改');
  await typeInto(k, 'sk-new-123456');
  eq(await barShown(), true);
  await ev("document.getElementById('st-save').click();");
  await waitFor(dlg);
  const text = await ev(`return ${dlg}.textContent;`);
  if (text.includes('sk-new-123456')) throw new Error('确认框里不能出现密钥内容');
  has(text, '（新值，不显示）');
  await ev(`document.querySelector(${JSON.stringify(confirmBtn)}).click();`);
  await waitCalls(1);
  eq(calls[0].body, { changes: { [k]: 'sk-new-123456' } });
  await waitFor(`!${dlg} && document.getElementById('st-bar').hidden && ${row(k)}.querySelectorAll('input').length === 0`);
});

await t('密钥项清除：“清除”先成草稿（将清除 / 可撤销），保存后以 null 提交，之后回到未设置', async () => {
  calls.length = 0;
  const k = 'sms.aliyun_access_key_secret';
  await ev(`${row(k)}.querySelector('button[data-act="secret-clear"]').click();`);
  has(await ev(`return ${row(k)}.textContent;`), '将清除');
  eq(await barShown(), true);
  await ev(`${row(k)}.querySelector('button[data-act="undo"]').click();`);
  eq(await barShown(), false, '撤销后不算修改');
  await ev(`${row(k)}.querySelector('button[data-act="secret-clear"]').click();`);
  await ev("document.getElementById('st-save').click();");
  await waitFor(dlg);
  has(await ev(`return ${dlg}.textContent;`), '（清除）');
  await ev(`document.querySelector(${JSON.stringify(confirmBtn)}).click();`);
  await waitCalls(1);
  eq(calls[0].body, { changes: { [k]: null } });
  await waitFor(`!${dlg} && ${field(k)} && ${field(k)}.type === 'password'`);
});

await t('变更记录：全部 / 单项抽屉，新的在前，敏感项不记内容，可加载更多，Esc 关闭', async () => {
  await go('export');
  await ev("document.getElementById('st-hist-all').click();");
  await waitFor("!document.getElementById('st-drawer').hidden && document.querySelectorAll('#st-drawer-body .st-h-item').length === 50");
  eq(await ev("return document.getElementById('st-drawer-title').textContent;"), '变更记录（全部设置）');
  eq(await ev("return document.getElementById('st-drawer-more').hidden;"), false, '还有更多记录');
  const first = await ev("return document.querySelector('#st-drawer-body .st-h-item').textContent;");
  has(first, 'sms.aliyun_access_key_secret');
  has(first, '恢复默认');
  has(first, '敏感项：内容不记录');
  const body = await ev("return document.getElementById('st-drawer-body').textContent;");
  eq(body.includes('sk-new-123456'), false);
  has(body, '500000'); // 此前保存的旧值 → 新值
  await ev("document.getElementById('st-drawer-more').click();");
  await waitFor(`document.querySelectorAll('#st-drawer-body .st-h-item').length === ${history.length}`);
  eq(await ev("return document.getElementById('st-drawer-more').hidden;"), true);
  eq(gets.slice(-1)[0], '/api/v1/settings/config/history?limit=50&offset=50');
  await ev("document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));");
  eq(await ev("return document.getElementById('st-drawer').hidden;"), true);
  // 单项
  await ev(`${row('export.max_rows')}.querySelector('button[data-act="history"]').click();`);
  await waitFor("!document.getElementById('st-drawer').hidden && document.querySelectorAll('#st-drawer-body .st-h-item').length > 0");
  eq(await ev("return document.getElementById('st-drawer-title').textContent;"), '变更记录：导出行数上限');
  eq(gets.slice(-1)[0], '/api/v1/settings/config/history?limit=50&offset=0&key=export.max_rows');
  eq(await ev("return [...document.querySelectorAll('#st-drawer-body .st-key')].every((e) => e.textContent === 'export.max_rows');"), true);
  await ev("document.getElementById('st-drawer-close').click();");
  eq(await ev("return document.getElementById('st-drawer').hidden;"), true);
});

await t('搜索与“只看已修改”：搜索跨分组；只看已修改只留与默认值不同的项', async () => {
  await ev("const q = document.getElementById('st-q'); q.value = 'select'; q.dispatchEvent(new Event('input', { bubbles: true }));");
  eq(await ev("return [...document.querySelectorAll('#st-content .st-row')].map((r) => r.dataset.key);"), ['query.forbid_select_star']);
  await ev("const q = document.getElementById('st-q'); q.value = 'zzz-no-such'; q.dispatchEvent(new Event('input', { bubbles: true }));");
  has(await ev("return document.getElementById('st-content').textContent;"), '没有符合条件的设置项');
  await ev("const q = document.getElementById('st-q'); q.value = ''; q.dispatchEvent(new Event('input', { bubbles: true }));");
  await go('query');
  await ev("const c = document.getElementById('st-only-mod'); c.checked = true; c.dispatchEvent(new Event('change', { bubbles: true }));");
  eq(await ev("return [...document.querySelectorAll('#st-content .st-row')].map((r) => r.dataset.key);"), ['query.mask_enabled']);
  await ev("const c = document.getElementById('st-only-mod'); c.checked = false; c.dispatchEvent(new Event('change', { bubbles: true }));");
});

await t('提醒：SECRET_KEY 公开缺省值、密文解不开的项各一条；设置表不存在时只给部署提示', async () => {
  const load = async () => {
    await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/` });
    await waitFor("document.querySelector('#st-nav button[data-group]') || document.querySelector('#st-alerts .alert')");
  };
  state.publicKey = true;
  state.undecryptable = ['sms.tencent_secret_key', 'export.local_path'];
  await load();
  const al = await ev("return document.getElementById('st-alerts').textContent;");
  has(al, 'SECRET_KEY 仍是公开的缺省值');
  has(al, '有 2 项敏感设置的密文用当前密钥');
  has(al, 'sms.tencent_secret_key、export.local_path');
  state.ready = false;
  await load();
  has(await ev("return document.getElementById('st-alerts').textContent;"), '设置表 dbt_setting 不存在');
  eq(await ev("return document.getElementById('st-content').children.length;"), 0, '没有设置表时不列设置项');
  state.ready = true;
});

await t('控制台零报错', async () => { eq(errors.filter((e) => !/favicon/.test(e)), []); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
