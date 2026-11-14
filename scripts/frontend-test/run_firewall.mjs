/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_firewall.mjs
 * @time: 2026/10/28
 */
// 系统管理 › SQL 防火墙 页面级测试：自起假后端（接口形状按 internal/firewalladmin）+ 无头 Chrome（CDP），不依赖第三方包。
// 用法：node scripts/frontend-test/run_firewall.mjs [chrome路径]    环境变量 FW_HTTP_PORT（默认 8917）/ FW_CDP_PORT（默认 9254）/ FW_SHOT（截图目录）。
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
const HTTP = +(process.env.FW_HTTP_PORT || 8917);
const CDP = +(process.env.FW_CDP_PORT || 9254);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'";
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.json': 'application/json', '.svg': 'image/svg+xml' };

// ---- 假后端 ----
const META = {
  groups: [{ id: 3, name: 'DBA' }, { id: 5, name: '研发' }, { id: 6, name: '审计' }],
  instances: [{ id: 1, name: 'prod-mysql-01', db_type: 'mysql', env: 'prod' }, { id: 2, name: 'test-mysql-01', db_type: 'mysql', env: 'test' }],
  // 环境字典（1.39 起页面从 meta.envs 取环境选项与显示名，接口形状同 internal/firewalladmin.EnvItem）
  envs: [{ code: 'prod', name: '生产', is_prod: true }, { code: 'test', name: '测试', is_prod: false }],
  features: [], max_rules: 500, max_conditions: 20, big_in_items: 1000,
};
// 场景是否已接入执行由 state.pendingScenes 决定（默认都已接入；测试里单独打开“有场景尚未接入”的呈现）
function scenesMeta() {
  const off = state.pendingScenes;
  return [{ key: 'query', label: '在线查询', active: true }, { key: 'workflow', label: 'SQL 工单', active: !off, note: off ? '尚未接入' : '' },
    { key: 'export', label: '数据导出', active: !off, note: off ? '尚未接入' : '' }];
}
const rule = (o) => ({ id: 0, name: '', description: '', priority: 100, enabled: true, mode: 'observe', observe_since: '', observe_days: 0, scenes: ['query'], env: 'all',
  instance_ids: [], group_mode: 'all', group_ids: [], match: 'all', conditions: [], action: 'warn', action_arg: 0, message: '', notify: false,
  updated_by: 'admin', updated_at: '2026-10-12 10:00', created_at: '2026-10-01 10:00', hits7: 0, spark: [0, 0, 0, 0, 0, 0, 0], invalid: '', ...o });
const state = {};
function resetState() {
  Object.assign(state, {
    ready: true, enabled: true, mode: 'enforce', nextId: 100, failToggle: false, dupName: false, pendingScenes: false, pendingConds: [],
    rules: [
      rule({ id: 1, name: '禁止危险函数', description: 'SLEEP / BENCHMARK 制造长时间占用。', priority: 20, mode: 'enforce', scenes: ['query', 'workflow', 'export'], match: 'any',
        conditions: [{ t: 'keyword', v: ['SLEEP', 'BENCHMARK'] }], action: 'deny', message: '语句里包含不允许使用的函数。', hits7: 12, spark: [1, 2, 0, 3, 2, 1, 3] }),
      rule({ id: 2, name: '敏感表访问要求写原因', priority: 40, mode: 'enforce', env: 'prod', group_mode: 'except', group_ids: [3],
        conditions: [{ t: 'object', v: ['*.sys_users', '*.user_card'] }], action: 'reason', message: '该表含敏感数据，请说明用途。', hits7: 31, spark: [3, 5, 4, 6, 2, 4, 7] }),
      rule({ id: 3, name: '大范围扫描预警', priority: 70, mode: 'observe', observe_since: '2026-10-10 09:00', observe_days: 3, env: 'prod',
        conditions: [{ t: 'stmt', v: ['SELECT'] }, { t: 'scan_rows', v: 1000000 }], action: 'warn', message: '本次查询预计扫描超过 100 万行。', hits7: 0,
        evals: 40, avg_ms: 230, max_ms: 2900, unknown_pct: 35, slow: true, blind: true }),
      rule({ id: 4, name: 'IN 列表过长', priority: 80, enabled: false, mode: 'observe', scenes: ['query', 'workflow'],
        conditions: [{ t: 'feature', v: 'big_in' }], action: 'warn' }),
      rule({ id: 5, name: '手工插入的坏规则', priority: 90, mode: 'enforce', conditions: [], action: 'deny', invalid: '至少需要一个命中条件' }),
    ],
    hits: Array.from({ length: 25 }, (_, i) => ({
      id: i + 1, created_at: '2026-10-12 09:' + String(10 + i).padStart(2, '0'), last_at: '2026-10-12 10:' + String(10 + i).padStart(2, '0'), rule_id: i % 3 === 0 ? 2 : 1,
      rule_name: i % 3 === 0 ? '敏感表访问要求写原因' : '禁止危险函数', rule_exists: true, scene: 'query', user: i % 2 ? 'rd_zhao' : 'qa_sun', instance_id: 1, instance: 'prod-mysql-01', db: 'shop',
      effective: i % 3 === 0 ? 'reason' : i % 5 === 0 ? 'observe' : 'deny', would_action: i % 5 === 0 ? 'deny' : '', fingerprint: 'a1b2c3d4',
      sql: i % 3 === 0 ? 'SELECT UserId, Password FROM sys_users WHERE UserId = ?' : 'SELECT id, SLEEP(?) FROM orders LIMIT ?', reason: i % 3 === 0 ? '客诉排查，工单 12345' : '', repeat: i % 4 === 0 ? 3 : 1,
      query_log_id: 0, matched: i % 3 === 0 ? ['涉及 *.sys_users'] : ['包含 SLEEP'], unknown: [], downgraded: '', message: i % 3 === 0 ? '该表含敏感数据，请说明用途。' : '语句里包含不允许使用的函数。',
    })),
    excs: [
      { id: 1, rule_id: 3, rule_name: '大范围扫描预警', subject_kind: 'user', subject_value: 'rd_li', subject_text: '用户 rd_li', reason: '月度对账报表，已评估过', expires_at: '2026-10-31', expired: false, created_by: 'admin' },
      { id: 2, rule_id: 2, rule_name: '敏感表访问要求写原因', subject_kind: 'group', subject_value: '6', subject_text: '权限组 审计', reason: '审计取证需要', expires_at: '2026-10-01', expired: true, created_by: 'admin' },
    ],
    tpls: [
      { key: 'dangerous-func', category: '防滥用', name: '禁止危险函数', description: 'SLEEP / BENCHMARK ...', rule: rule({ conditions: [{ t: 'keyword', v: ['SLEEP'] }], action: 'deny' }), pending: false, applied: true },
      { key: 'cartesian', category: '防大查询', name: '笛卡尔积', description: 'JOIN 缺少连接条件', rule: rule({ conditions: [{ t: 'feature', v: 'cartesian' }], action: 'deny' }), pending: false, applied: false },
      { key: 'no-where-dml', category: '防误操作', name: '禁止不带 WHERE 的 UPDATE / DELETE', description: '生产库的整表更新 / 删除直接拒绝。', rule: rule({ scenes: ['workflow'], conditions: [{ t: 'feature', v: 'no_where' }], action: 'deny' }), pending: true, applied: false },
    ],
  });
}
resetState();
const calls = [];
const ok = (data) => ({ code: 'ok', message: 'ok', data });
const view = (r) => ({ ...r });

function overview() {
  const on = state.rules.filter((r) => r.enabled);
  return {
    ready: state.ready, missing: state.ready ? [] : ['dbt_firewall_rule', 'dbt_firewall_exception', 'dbt_firewall_hit'], enabled: state.enabled, mode: state.mode, on_error: 'allow', store_sql: 'masked',
    rules_total: state.rules.length, rules_enabled: on.length, rules_observing: on.filter((r) => r.mode === 'observe').length, rules_idle: 1,
    hits_today: 23, users_today: 4, rules_hit_today: 3, blocked_today: 5, reason_today: 6, tighten_today: 1, warn_today: 2, observe_today: 9, would_deny_today: 4,
    explain_timeout_ms: 3000, slow_rules: 1, blind_rules: 1,
    explain: { calls: 120, failures: 9, timeouts: 4, cache_hits: 310, skipped: 6, total_ms: 18000, max_ms: 3000, open: [3] },
  };
}
function evaluate(b) {
  const sql = String(b.sql || '');
  const low = sql.toLowerCase();
  const res = { verdict: 'pass', deny: false, rule: null, message: '', need_reason: false, reason_msgs: [], max_rows: 0, limited_by: '', force_mask: [], warnings: [],
    hits: [], skipped: [{ rule: { id: 4, name: 'IN 列表过长', priority: 80, action: 'warn', mode: 'observe' }, reason: '规则已停用' }],
    facts: { kind: (low.match(/^\s*(\w+)/) || [0, ''])[1].toUpperCase(), objects: ['orders'], fingerprint: 'f5a0b6c2', multi: false },
    global_mode: state.mode, enabled_flag: state.enabled, caps: { Reason: true, Limit: true, Mask: true }, highlight: [], failed: false, notes: ['库表来自词法解析；线上配置了 goInception 时用语法树。'] };
  if (low.includes('sleep')) {
    const r1 = state.rules[0];
    res.verdict = 'deny'; res.deny = true; res.rule = { id: 1, name: r1.name, priority: 20, action: 'deny', mode: 'enforce', message: r1.message }; res.message = r1.message;
    res.hits.push({ rule: res.rule, effective: 'deny', matched: ['包含 SLEEP'], unknown: [], highlight: ['SLEEP'] });
    res.highlight = ['SLEEP'];
  } else if (low.includes('sys_users')) {
    res.verdict = 'reason'; res.need_reason = true; res.reason_msgs = ['该表含敏感数据，请说明用途。'];
    res.hits.push({ rule: { id: 2, name: '敏感表访问要求写原因', priority: 40, action: 'reason', mode: 'enforce' }, effective: 'reason', matched: ['涉及 *.sys_users'], unknown: [], highlight: ['sys_users'] });
    res.highlight = ['sys_users'];
  } else if (b.scan_rows == null && low.includes('big')) {
    res.hits.push({ rule: { id: 3, name: '大范围扫描预警', priority: 70, action: 'warn', mode: 'observe' }, effective: 'observe', would_action: 'warn', matched: ['语句类型 SELECT'], unknown: ['预估扫描行数：未提供（EXPLAIN 尚未接入）'], highlight: [] });
  }
  return res;
}

function readTpl(rel) { return untranslate(fs.readFileSync(path.join(root, 'web/templates', rel), 'utf8').replace(/\r\n/g, '\n')); }
function page() {
  const t = readTpl('firewall/index.html');
  const ci = t.indexOf('{{define "content"}}'), si = t.indexOf('{{define "scripts"}}');
  const content = t.slice(ci + '{{define "content"}}'.length, si).replace(/\n\{\{end\}\}\s*$/, '');
  const scripts = t.slice(si + '{{define "scripts"}}'.length).replace(/\{\{end\}\}\s*$/, '');
  const head = (t.match(/\{\{define "head"\}\}([\s\S]*?)\{\{end\}\}/) || [, ''])[1];
  return `<!doctype html><html lang="zh-CN" data-skin="${process.env.FW_SKIN || 'dark'}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="csrf-token" content="tok"><title>SQL 防火墙</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${head}</head><body><div class="app"><div class="main">${content}</div></div>${scripts}</body></html>`;
}

const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, 'http://x');
  const p = decodeURIComponent(u.pathname);
  let body = '';
  for await (const c of req) body += c;
  const json = (status, obj) => { res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(obj)); };
  const fail = (status, code, message) => json(status, { code, message });
  let parsed = null;
  try { parsed = body ? JSON.parse(body) : null; } catch (_) { /* 非 JSON */ }
  if (req.method !== 'GET' || p.startsWith('/api/')) calls.push({ method: req.method, url: p + u.search, body: parsed });
  if (p === '/favicon.ico') { res.writeHead(204); res.end(); return; }
  if (p === '/firewall') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' }); res.end(page()); return; }
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
  const base = '/api/v1/settings/firewall';
  if (!p.startsWith(base)) { res.writeHead(404); res.end('nf'); return; }
  const sub = p.slice(base.length);
  let m;
  if (sub === '/meta') return json(200, ok({ ...META, scenes: scenesMeta(), pending_conditions: state.pendingConds }));
  if (sub === '/overview') return json(200, ok(overview()));
  if (sub === '/rules' && req.method === 'GET') return json(200, ok({ ready: state.ready, missing: state.ready ? [] : ['dbt_firewall_rule'], rules: state.rules.map(view), max_rules: 500 }));
  if (sub === '/rules' && req.method === 'POST') {
    if (state.dupName || state.rules.some((r) => r.name === parsed.name)) return fail(409, 'duplicate_name', '已有同名规则「' + parsed.name + '」，请换一个名称');
    if (parsed.action === 'mask') return fail(400, 'bad_request', '“强制脱敏”需要至少一个带列名的对象条件');
    const r = rule({ ...parsed, id: ++state.nextId, enabled: parsed.enabled !== false, observe_days: parsed.mode === 'observe' ? 1 : 0 });
    state.rules.push(r);
    return json(200, ok(view(r)));
  }
  if ((m = sub.match(/^\/rules\/(\d+)$/))) {
    const id = Number(m[1]);
    const i = state.rules.findIndex((r) => r.id === id);
    if (i < 0) return fail(404, 'not_found', '规则不存在');
    if (req.method === 'PUT') { state.rules[i] = rule({ ...state.rules[i], ...parsed, id }); return json(200, ok(view(state.rules[i]))); }
    if (req.method === 'DELETE') { state.rules.splice(i, 1); return json(200, ok({ removed_exceptions: 0 })); }
    return json(200, ok(view(state.rules[i])));
  }
  if ((m = sub.match(/^\/rules\/(\d+)\/toggle$/))) {
    if (state.failToggle) return fail(500, 'internal', '数据库暂时不可用');
    const r = state.rules.find((x) => x.id === Number(m[1]));
    r.enabled = !!parsed.enabled;
    return json(200, ok(view(r)));
  }
  if ((m = sub.match(/^\/rules\/(\d+)\/mode$/))) {
    const r = state.rules.find((x) => x.id === Number(m[1]));
    r.mode = parsed.mode; r.observe_days = parsed.mode === 'observe' ? 1 : r.observe_days;
    return json(200, ok({ rule: view(r), observed: r.hits7, warning: parsed.mode === 'enforce' && !r.hits7 ? '这条规则近 7 天没有任何命中记录（没有观察数据），已按你的选择切到生效。' : '' }));
  }
  if (sub === '/replay' && req.method === 'POST') {
    return json(200, ok({ applicable: true, scanned: 1840, scanned_by_scene: { query: 1700, workflow: 140 }, days: 7, truncated: false, hits: 37, users: 5, dba_hits: 2, dba_known: true, unevaluated: 0, rate: 0.0201,
      samples: [{ scene: 'workflow', sql: 'select id, sleep(1) from orders', user: 'rd_zhao', instance: 'prod-mysql-01', db: 'shop', at: '2026-10-11 15:02', why: ['包含 SLEEP'], would: 'deny' }],
      advice: '命中 2.0%（超过 1%）：建议先用观察模式跑一周，看过命中记录再决定是否生效。', notes: ['数据来自在线查询日志；日志里的语句是系统追加 LIMIT 之后的形态。'] }));
  }
  if (sub === '/evaluate') return json(200, ok(evaluate(parsed || {})));
  if (sub === '/hits') {
    const q = (k) => u.searchParams.get(k) || '';
    let list = state.hits.filter((h) => (!q('result') || h.effective === q('result')) && (!q('rule_id') || String(h.rule_id) === q('rule_id')) && (!q('q') || (h.sql + h.user).includes(q('q'))));
    const size = Number(q('size')) || 20, page = Number(q('page')) || 1;
    return json(200, ok({ items: list.slice((page - 1) * size, page * size), total: list.length, page, size }));
  }
  if ((m = sub.match(/^\/hits\/(\d+)$/))) {
    const h = state.hits.find((x) => x.id === Number(m[1]));
    return h ? json(200, ok(h)) : fail(404, 'not_found', '命中记录不存在');
  }
  if (sub === '/exceptions' && req.method === 'GET') return json(200, ok(state.excs));
  if (sub === '/exceptions' && req.method === 'POST') {
    if (String(parsed.reason || '').trim().length < 4) return fail(400, 'bad_request', '请填写原因（4–500 个字符）：说明这条规则为什么对该对象不适用');
    const r = state.rules.find((x) => x.id === parsed.rule_id);
    const e = { id: ++state.nextId, rule_id: parsed.rule_id, rule_name: r ? r.name : '', subject_kind: parsed.subject_kind, subject_value: parsed.subject_value,
      subject_text: ({ user: '用户 ', group: '权限组 #', instance: '实例 #', fingerprint: 'SQL 指纹 ' }[parsed.subject_kind]) + parsed.subject_value, reason: parsed.reason, expires_at: '2026-11-11', expired: false, created_by: 'admin' };
    state.excs.unshift(e);
    return json(200, ok(e));
  }
  if ((m = sub.match(/^\/exceptions\/(\d+)$/)) && req.method === 'DELETE') {
    state.excs = state.excs.filter((e) => e.id !== Number(m[1]));
    return json(200, ok({}));
  }
  if (sub === '/templates') return json(200, ok(state.tpls));
  if ((m = sub.match(/^\/templates\/([\w-]+)\/apply$/))) {
    const t = state.tpls.find((x) => x.key === m[1]);
    if (!t) return fail(404, 'not_found', '模板不存在');
    if (t.applied) return fail(409, 'duplicate_name', '已有同名规则');
    t.applied = true;
    const r = rule({ ...t.rule, id: ++state.nextId, name: t.name, description: t.description, mode: 'observe', observe_days: 1 });
    state.rules.push(r);
    return json(200, ok(view(r)));
  }
  if (sub === '/mode' && req.method === 'PUT') { state.mode = parsed.mode; return json(200, ok({ enabled: state.enabled, mode: state.mode, changed: true })); }
  if (sub === '/switch' && req.method === 'PUT') {
    if (!parsed.enabled && String(parsed.reason || '').trim().length < 4) return fail(400, 'bad_request', '关闭总开关需要填写原因（4–200 个字符）');
    state.enabled = !!parsed.enabled;
    return json(200, ok({ enabled: state.enabled, mode: state.mode, changed: true }));
  }
  res.writeHead(404); res.end('nf');
}).listen(HTTP);

// ---- Chrome ----
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'fw-chrome-'));
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
  if (!process.env.FW_SHOT) return;
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.mkdirSync(process.env.FW_SHOT, { recursive: true });
  fs.writeFileSync(path.join(process.env.FW_SHOT, name + '.png'), Buffer.from(r.data, 'base64'));
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
const clickText = (scope, text) => ev(`const b=[...document.querySelectorAll(${JSON.stringify(scope)})].find(x=>x.textContent.trim()===${JSON.stringify(text)}); if(!b) throw new Error('找不到按钮 '+${JSON.stringify(text)}); b.click();`);
const clickDlg = (text) => clickText('.cf-dialog button', text);
const setVal = (sel, v) => ev(`const e=document.querySelector(${JSON.stringify(sel)}); e.value=${JSON.stringify(v)}; e.dispatchEvent(new Event('input',{bubbles:true})); e.dispatchEvent(new Event('change',{bubbles:true}));`);
const lastCall = (url, method) => calls.filter((c) => c.url.split('?')[0] === url && (!method || c.method === method)).pop();
const count = (url, method) => calls.filter((c) => c.url.split('?')[0] === url && (!method || c.method === method)).length;
const BASE = '/api/v1/settings/firewall';
// 规则名后面可能跟着“慢 / 未评估”徽标：名称是 .fw-rname 的第一个文本节点
const rowOf = (name) => `[...document.querySelectorAll('#fw-rules tbody tr')].find(r => r.querySelector('.fw-rname') && r.querySelector('.fw-rname').firstChild.textContent === ${JSON.stringify(name)})`;
const ruleNames = () => ev("return [...document.querySelectorAll('#fw-rules .fw-rname')].map(e => e.firstChild.textContent);");
async function nav(hash = '') {
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/firewall${hash}` });
  await sleep(200);
  await waitFor("document.readyState === 'complete'", '页面加载');
  await waitFor("document.querySelectorAll('#fw-stats .fw-stat').length === 5 || document.querySelector('#fw-alerts .alert.err')", '概览');
  await waitFor("document.querySelectorAll('#fw-rules tbody tr').length > 0", '规则列表');
}
async function closeToasts() { await ev("document.querySelectorAll('.cf-toast').forEach(t => t.click());"); }

// ================= 用例 =================
console.log('SQL 防火墙页面');

await t('页面加载：全局状态、5 张统计卡、规则列表、标签页数量（场景都已接入时没有“尚未接入”提示）', async () => {
  resetState();
  await nav();
  eq(await qa('#fw-gmode button', 'getAttribute("aria-pressed")'), ['false', 'true'], '全局模式是“拦截”');
  has(await q('#fw-switch'), '已开启', '总开关');
  const stats = await qa('#fw-stats .fw-stat .k');
  eq(stats, ['今日命中', '已拦截', '观察命中', '启用中的规则', '长期零命中'], '统计卡');
  has(await q('#fw-stats'), '若全部生效，会多拦 4 条', '观察命中说明');
  eq((await qa('#fw-rules tbody tr')).length, 5, '规则行');
  eq(await q('#fw-n-rules'), '5', '规则数量');
  truthy(!(await q('#fw-alerts')).includes('尚未接入'), '场景都已接入时不提示“尚未接入”');
  await waitFor("document.getElementById('fw-n-hits').textContent === '25'", '命中数量');
  eq(await q('#fw-n-exc'), '1', '有效例外数量（过期的不计）');
  await shot('fw-rules');
});

await t('规则行：优先级顺序、动作徽标、状态、无效规则与暂未接入条件', async () => {
  const names = await ruleNames();
  eq(names, ['禁止危险函数', '敏感表访问要求写原因', '大范围扫描预警', 'IN 列表过长', '手工插入的坏规则'], '按列表顺序');
  has(await ev(`return ${rowOf('大范围扫描预警')}.textContent;`), '观察中 · 第 3 天', '观察状态');
  has(await ev(`return ${rowOf('禁止危险函数')}.textContent;`), '已生效', '生效状态');
  has(await ev(`return ${rowOf('手工插入的坏规则')}.textContent;`), '无效，不会生效', '无效规则');
  truthy(await ev(`return ${rowOf('IN 列表过长')}.classList.contains('off');`), '停用的规则变灰');
  eq(await ev(`return ${rowOf('大范围扫描预警')}.querySelectorAll('.fw-chip.pending').length;`), 0, 'P5 之后预估扫描行数有事实来源，不再标暂未接入');
  has(await ev(`return ${rowOf('大范围扫描预警')}.querySelector('.fw-rname').textContent;`), '慢 · 平均 230 毫秒', '慢规则标记');
  has(await ev(`return ${rowOf('大范围扫描预警')}.querySelector('.fw-rname').textContent;`), '未评估 35%', '未评估占比标记');
  has(await q('#fw-explain'), 'EXPLAIN（本进程）：下发 120 次，失败 9（超时 4），缓存命中 310，平均 150 毫秒，时限 3,000 毫秒', 'EXPLAIN 使用情况');
  has(await q('#fw-explain'), '有 1 个实例因连续失败暂停了 EXPLAIN', '熔断提示');
  has(await q('#fw-explain'), '慢规则 1 条', '慢规则计数');
  eq(await ev(`return ${rowOf('禁止危险函数')}.querySelectorAll('.fw-spark i').length;`), 7, '近 7 天小柱图');
  has(await q('#fw-alerts'), '1 条规则无法通过校验'.replace('1 条', '有 1 条'), '无效规则横幅');
});

await t('筛选：名称搜索、动作、状态', async () => {
  await setVal('#fw-f-q', '危险');
  eq((await ruleNames()).length, 1, '按名称');
  await setVal('#fw-f-q', '');
  await setVal('#fw-f-action', 'reason');
  eq(await ruleNames(), ['敏感表访问要求写原因'], '按动作');
  await setVal('#fw-f-action', '');
  await setVal('#fw-f-state', 'observe');
  eq(await ruleNames(), ['大范围扫描预警', 'IN 列表过长'], '观察中（含已停用的）');
  await setVal('#fw-f-state', 'off');
  eq(await ruleNames(), ['IN 列表过长'], '已停用');
  await setVal('#fw-f-state', '');
  await setVal('#fw-f-scene', 'workflow');
  eq((await ruleNames()).length, 2, '按场景');
  await setVal('#fw-f-scene', '');
});

await t('启停开关：POST toggle 并刷新；失败时开关回退并提示', async () => {
  await ev(`${rowOf('禁止危险函数')}.querySelector('.fw-sw input').click();`);
  await waitFor("document.querySelector('.cf-toast')", '提示');
  const c = lastCall(BASE + '/rules/1/toggle');
  eq(c.body, { enabled: false }, '请求体');
  await waitFor(`${rowOf('禁止危险函数')}.classList.contains('off')`, '行变灰');
  await closeToasts();
  state.failToggle = true;
  await ev(`${rowOf('禁止危险函数')}.querySelector('.fw-sw input').click();`);
  await waitFor("document.querySelector('.cf-toast-err')", '错误提示');
  has(await q('.cf-toast-err'), '数据库暂时不可用', '错误信息');
  eq(await ev(`return ${rowOf('禁止危险函数')}.querySelector('.fw-sw input').checked;`), false, '失败后开关回退');
  state.failToggle = false;
  await closeToasts();
  await ev(`${rowOf('禁止危险函数')}.querySelector('.fw-sw input').click();`);
  await waitFor(`!${rowOf('禁止危险函数')}.classList.contains('off')`, '重新启用');
  await closeToasts();
});

await t('观察 → 生效：高危确认弹窗说明近 7 天命中；确认后 POST mode，并提示没有观察数据', async () => {
  await ev(`${rowOf('大范围扫描预警')}.querySelector('.fw-link').click();`);
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  has(await q('.cf-dialog'), '把规则切到生效', '标题');
  has(await q('.cf-dialog'), '没有观察数据', '近 7 天零命中的提醒');
  eq(count(BASE + '/rules/3/mode'), 0, '确认前不发请求');
  await clickDlg('切到生效');
  await waitFor("!document.querySelector('.cf-dialog')", '弹窗关闭');
  eq(lastCall(BASE + '/rules/3/mode').body, { mode: 'enforce' }, '请求体');
  await waitFor(`${rowOf('大范围扫描预警')}.textContent.includes('已生效')`, '状态变为已生效');
  await waitFor("[...document.querySelectorAll('.cf-toast')].some(t => t.textContent.includes('没有任何命中记录'))", '没有观察数据的提示');
  await closeToasts();
});

await t('删除：确认弹窗列出近 7 天命中；确认后 DELETE', async () => {
  await ev(`${rowOf('IN 列表过长')}.querySelector('.fw-link.danger').click();`);
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  has(await q('.cf-dialog'), '命中记录会保留', '说明');
  await clickDlg('删除');
  await waitFor(`!${rowOf('IN 列表过长')}`, '规则消失');
  eq(count(BASE + '/rules/4', 'DELETE'), 1, '只发一次 DELETE');
  await closeToasts();
});

await t('新建规则：缺省观察模式、全部实例；保存 POST 规范化的请求体并刷新列表', async () => {
  await click('#fw-b-new');
  await waitFor("document.querySelector('.fw-drawer')", '编辑器抽屉');
  has(await q('.fw-drawer h3'), '新建规则', '标题');
  has(await q('.fw-drawer h3 .fw-badge'), '观察模式', '默认观察');
  eq(await q('.fw-drawer select[aria-label="实例环境"]', 'value'), 'all', '默认环境');
  eq((await qa('.fw-drawer .fw-checks .tag')).length, 0, '场景都已接入，不带“尚未接入”标记');
  await setVal('.fw-drawer input[aria-label="规则名称"]', '禁止 INTO OUTFILE');
  await ev("const sel=document.querySelector('.fw-crow select[aria-label=\"条件类型\"]'); sel.value='keyword'; sel.dispatchEvent(new Event('change'));");
  await setVal('.fw-crow input[type=text]', 'into outfile, into dumpfile');
  await ev("[...document.querySelectorAll('.fw-act')].find(b => b.textContent.startsWith('拒绝')).click();");
  await setVal('.fw-drawer input[aria-label="提示文案"]', '不允许写出文件');
  await shot('fw-editor');
  await clickText('.fw-drawer .ft button', '保存');
  await waitFor("!document.querySelector('.fw-drawer')", '抽屉关闭');
  const c = lastCall(BASE + '/rules', 'POST');
  eq([c.body.name, c.body.mode, c.body.env, c.body.scenes, c.body.action, c.body.match, c.body.enabled], ['禁止 INTO OUTFILE', 'observe', 'all', ['query'], 'deny', 'all', true], '请求体');
  eq(c.body.conditions, [{ t: 'keyword', v: ['into outfile', 'into dumpfile'] }], '条件');
  await waitFor(`!!${rowOf('禁止 INTO OUTFILE')}`, '新规则出现在列表里');
  has(await q('.cf-toast'), '观察模式，不会拦截', '保存提示');
  await closeToasts();
});

await t('编辑器：直接保存为“生效”会先弹高危确认，取消则不发请求', async () => {
  await click('#fw-b-new');
  await waitFor("document.querySelector('.fw-drawer')", '编辑器抽屉');
  await setVal('.fw-drawer input[aria-label="规则名称"]', '直接生效的规则');
  await ev("document.querySelector('.fw-seg.mode button[data-m=\"enforce\"]').click();");
  has(await q('.fw-drawer h3 .fw-badge'), '已生效', '徽标跟随模式');
  const before = count(BASE + '/rules', 'POST');
  await clickText('.fw-drawer .ft button', '保存');
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  has(await q('.cf-dialog'), '以“生效”模式保存', '标题');
  await clickDlg('取消');
  await sleep(150);
  eq(count(BASE + '/rules', 'POST'), before, '取消后不发请求');
  truthy(await q('.fw-drawer'), '抽屉仍在');
});

await t('编辑器：服务端校验错误显示在抽屉里，内容不丢；未保存关闭会先确认', async () => {
  await setVal('.fw-drawer input[aria-label="规则名称"]', '禁止危险函数'); // 与已有规则重名
  await ev("document.querySelector('.fw-seg.mode button[data-m=\"observe\"]').click();");
  await clickText('.fw-drawer .ft button', '保存');
  await waitFor("document.querySelector('.fw-drawer .alert.err') && !document.querySelector('.fw-drawer .alert.err').hidden", '抽屉内的错误');
  has(await q('.fw-drawer .alert.err'), '已有同名规则', '重名提示');
  eq(await q('.fw-drawer input[aria-label="规则名称"]', 'value'), '禁止危险函数', '输入保留');
  await click('.fw-drawer h3 .x');
  await waitFor("document.querySelector('.cf-dialog')", '放弃修改确认');
  has(await q('.cf-dialog'), '放弃未保存的修改', '提示');
  await clickDlg('放弃修改');
  await waitFor("!document.querySelector('.fw-drawer')", '抽屉关闭');
});

await t('编辑已有规则：带出现有值；回放调用 /replay 并显示命中数与建议', async () => {
  await ev(`${rowOf('禁止危险函数')}.querySelectorAll('.fw-link')[1].click();`); // 编辑
  await waitFor("document.querySelector('.fw-drawer')", '编辑器抽屉');
  has(await q('.fw-drawer h3'), '编辑规则', '标题');
  eq(await q('.fw-drawer input[aria-label="规则名称"]', 'value'), '禁止危险函数', '名称');
  eq(await q('.fw-crow input[type=text]', 'value'), 'SLEEP, BENCHMARK', '关键字');
  await clickText('#fw-replay button', '开始回放');
  await waitFor("document.querySelector('#fw-replay .alert')", '回放结果');
  has(await q('#fw-replay'), '1,840 条语句（在线查询 1,700、SQL 工单 140）中，命中 37 条（2.0%）', '统计与来源');
  has(await q('#fw-replay'), 'SQL 工单 · rd_zhao', '样例带场景');
  has(await q('#fw-replay'), '其中 2 条来自 DBA 组', 'DBA 占比');
  has(await q('#fw-replay .alert.warn'), '建议先用观察模式', '建议');
  const c = lastCall(BASE + '/replay');
  eq(c.body.rule.name, '禁止危险函数', '回放的是当前草稿');
  await shot('fw-replay');
  await click('.fw-drawer .ft .btn.ghost');
  await waitFor("!document.querySelector('.fw-drawer')", '无修改直接关闭');
});

await t('语句测试台：示例语句 → /evaluate；拒绝结论、命中位置高亮、被跳过的规则', async () => {
  await click('#fw-b-test');
  await waitFor("!document.getElementById('fw-p-bench').hidden", '测试台');
  await setVal('#fw-b-pre', '3'); // 危险函数
  await waitFor("document.querySelector('#fw-b-res .fw-verdict')", '结果');
  const c = lastCall(BASE + '/evaluate');
  eq([c.body.scene, c.body.env, c.body.when, c.body.scan_rows, c.body.instance_id], ['query', 'prod', 'workday_day', null, 0], '请求体');
  has(await q('.fw-verdict'), '拒绝', '结论');
  truthy(await ev("return document.querySelector('.fw-verdict').classList.contains('deny');"), '红色');
  eq(await qa('#fw-b-res pre.fw-sql mark'), ['SLEEP'], '高亮');
  has(await q('#fw-b-res .fw-skipped'), '规则已停用', '被跳过的规则');
  has(await q('#fw-b-res'), '库表来自词法解析', '说明');
  await shot('fw-bench');
});

await t('测试台：放行 / 要求原因 / 观察命中的呈现；一键加入指纹黑名单打开预填的编辑器', async () => {
  await setVal('#fw-b-sql', 'select * from sys_users where id = 1');
  await click('#fw-b-run');
  await waitFor("document.querySelector('.fw-verdict.mid')", '要求原因');
  has(await q('.fw-verdict'), '要求填写原因', '结论');
  await setVal('#fw-b-sql', 'select a from big_table');
  await click('#fw-b-run');
  await waitFor("document.querySelector('.fw-verdict.pass') && document.querySelector('#fw-b-res .fw-hit')", '放行但有观察命中');
  has(await q('.fw-verdict'), '放行', '结论');
  has(await q('#fw-b-res .fw-hit'), '观察，不执行', '观察命中');
  has(await q('#fw-b-res .fw-hit .why.unk'), '未评估', '未评估的条件');
  await clickText('#fw-b-res button', '把这个指纹加入黑名单…');
  await waitFor("document.querySelector('.fw-drawer')", '编辑器');
  has(await q('.fw-drawer input[aria-label="规则名称"]', 'value'), '指纹黑名单 f5a0b6c2', '预填名称');
  eq(await q('.fw-crow input[aria-label="SQL 指纹"]', 'value'), 'f5a0b6c2', '预填指纹条件');
  await click('.fw-drawer h3 .x'); // 带入的草稿没有再改动：直接关闭，不弹“放弃修改”
  await waitFor("!document.querySelector('.fw-drawer')", '关闭');
  eq(await q('.cf-dialog'), null, '没有确认弹窗');
});

await t('命中记录：列表、合并次数、按结果筛选、分页、详情', async () => {
  await click('#fw-tabs [data-t="hits"]');
  await waitFor("document.querySelectorAll('#fw-hits tbody tr').length === 20", '第一页 20 条');
  has(await q('#fw-h-pager'), '共 25 条', '总数');
  has(await q('#fw-hits tbody'), '×3', '合并次数');
  await click('#fw-h-pager .btn:last-child'.replace(':last-child', ':nth-of-type(2)'));
  await waitFor("document.querySelectorAll('#fw-hits tbody tr').length === 5", '第二页 5 条');
  await setVal('#fw-h-result', 'observe');
  await waitFor("document.querySelectorAll('#fw-hits tbody tr').length === 3 && [...document.querySelectorAll('#fw-hits tbody tr')].every(r => r.textContent.includes('观察命中'))", '按结果筛选');
  eq(lastCall(BASE + '/hits').url.includes('result=observe'), true, '查询参数');
  await setVal('#fw-h-result', '');
  await waitFor("document.querySelectorAll('#fw-hits tbody tr').length === 20", '恢复');
  await ev("document.querySelector('#fw-hits tbody tr .fw-link').click();");
  await waitFor("document.querySelector('.fw-drawer')", '详情抽屉');
  has(await q('.fw-drawer'), '填写的原因', '原因字段');
  has(await q('.fw-drawer'), '命中的条件', '命中说明');
  await shot('fw-hit');
});

await t('命中详情 → 为该用户添加例外：预填规则与用户；原因太短时错误显示在抽屉里；成功后例外名单更新', async () => {
  await clickText('.fw-drawer .ft button', '为该用户添加例外…');
  await waitFor("document.querySelector('.fw-drawer h3') && document.querySelector('.fw-drawer h3').textContent.includes('添加例外')", '例外抽屉');
  eq(await ev("const s=document.querySelector('.fw-drawer select[aria-label=\"规则\"]'); return s.options[s.selectedIndex].text;"), '敏感表访问要求写原因', '预填规则');
  eq(await q('.fw-drawer input[aria-label="用户名"]', 'value'), 'qa_sun', '预填用户');
  await setVal('.fw-drawer textarea', 'ab');
  await clickText('.fw-drawer .ft button', '保存');
  await waitFor("document.querySelector('.fw-drawer .alert.err') && !document.querySelector('.fw-drawer .alert.err').hidden", '错误');
  has(await q('.fw-drawer .alert.err'), '请填写原因', '提示');
  await setVal('.fw-drawer textarea', '客服排查客诉需要，已备案');
  await setVal('.fw-drawer select[aria-label="有效期"]', '90');
  await clickText('.fw-drawer .ft button', '保存');
  await waitFor("!document.querySelector('.fw-drawer')", '抽屉关闭');
  const c = lastCall(BASE + '/exceptions', 'POST');
  eq([c.body.rule_id, c.body.subject_kind, c.body.subject_value, c.body.days], [2, 'user', 'qa_sun', 90], '请求体');
  await waitFor("!document.getElementById('fw-p-exc').hidden && document.querySelectorAll('#fw-excs tbody tr').length === 3", '切到例外名单并刷新');
  has(await q('#fw-excs'), '用户 qa_sun', '新例外');
  await closeToasts();
});

await t('例外名单：过期的标记；撤销走确认弹窗再 DELETE', async () => {
  const rows = await qa('#fw-excs tbody tr');
  truthy(rows.some((r) => r.includes('已过期')), '过期标记');
  await ev("[...document.querySelectorAll('#fw-excs tbody tr')].find(r => r.textContent.includes('rd_li')).querySelector('.fw-link').click();");
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  has(await q('.cf-dialog'), '规则对该对象立即恢复生效', '说明');
  await clickDlg('撤销');
  await waitFor("![...document.querySelectorAll('#fw-excs tbody tr')].some(r => r.textContent.includes('rd_li'))", '例外消失');
  eq(count(BASE + '/exceptions/1', 'DELETE'), 1, 'DELETE');
  await closeToasts();
});

await t('规则模板：已添加的禁用；未接入场景的有标记；添加后回到规则页', async () => {
  await click('#fw-tabs [data-t="tpl"]');
  const cards = await qa('#fw-tpls .fw-tpl h5');
  eq(cards.length, 3, '模板数');
  has(await q('#fw-tpls'), '场景尚未接入', '未接入标记');
  eq(await ev("return [...document.querySelectorAll('#fw-tpls .fw-tpl')].find(c => c.textContent.includes('禁止危险函数')).querySelector('button').disabled;"), true, '已添加的模板禁用');
  await ev("[...document.querySelectorAll('#fw-tpls .fw-tpl')].find(c => c.textContent.includes('笛卡尔积')).querySelector('button').click();");
  await waitFor("!document.getElementById('fw-p-rules').hidden && " + rowOf('笛卡尔积'), '回到规则页并出现新规则');
  has(await ev(`return ${rowOf('笛卡尔积')}.textContent;`), '观察中', '模板规则是观察模式');
  eq(count(BASE + '/templates/cartesian/apply', 'POST'), 1, 'POST apply');
  await closeToasts();
});

await t('全局模式：切到观察要高危确认；横幅出现；切回拦截', async () => {
  await clickText('#fw-gmode button', '观察');
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  has(await q('.cf-dialog'), '全局观察模式', '标题');
  eq(count(BASE + '/mode', 'PUT'), 0, '确认前不发请求');
  await clickDlg('切到观察');
  await waitFor("document.querySelector('#fw-alerts .alert.warn.fw-alert-act')", '观察横幅');
  eq(lastCall(BASE + '/mode').body, { mode: 'observe' }, '请求体');
  has(await q('#fw-rules'), '观察中（全局）', '生效规则显示为全局观察');
  await shot('fw-global-observe');
  await clickText('#fw-alerts .alert .btn', '切到拦截…');
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  await clickDlg('切到拦截');
  await waitFor("!document.querySelector('#fw-alerts .alert.warn.fw-alert-act')", '横幅消失');
  await closeToasts();
});

await t('总开关：关闭必须写原因（至少 4 个字），确认后 PUT switch；横幅可重新开启', async () => {
  await click('#fw-switch');
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  has(await q('.cf-dialog'), '关闭 SQL 防火墙总开关', '标题');
  await ev("document.querySelector('.cf-dialog .cf-textarea').value='排查误拦截，临时关闭'; document.querySelector('.cf-dialog .cf-textarea').dispatchEvent(new Event('input',{bubbles:true}));");
  await clickDlg('关闭');
  await waitFor("document.querySelector('#fw-alerts .alert.err')", '关闭横幅');
  eq(lastCall(BASE + '/switch').body, { enabled: false, reason: '排查误拦截，临时关闭' }, '请求体');
  has(await q('#fw-switch'), '已关闭', '按钮文案');
  await shot('fw-switch-off');
  await clickText('#fw-alerts .alert .btn', '重新开启…');
  await waitFor("document.querySelector('.cf-dialog')", '确认弹窗');
  await clickDlg('开启');
  await waitFor("!document.querySelector('#fw-alerts .alert.err')", '横幅消失');
  eq(lastCall(BASE + '/switch').body, { enabled: true, reason: '' }, '重新开启');
  await closeToasts();
});

await t('深链接 #hits 直接打开命中记录；窄屏（手机宽度）不出现横向滚动', async () => {
  await nav('#hits');
  await waitFor("!document.getElementById('fw-p-hits').hidden", '命中记录页');
  await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 800, deviceScaleFactor: 1, mobile: true });
  await sleep(250);
  const over = await ev("return document.documentElement.scrollWidth - document.documentElement.clientWidth;");
  truthy(over <= 1, `页面横向溢出 ${over}px`);
  await send('Emulation.setDeviceMetricsOverride', { width: 1366, height: 900, deviceScaleFactor: 1, mobile: false });
});

await t('表还没建：顶部提示执行建表脚本，新建按钮禁用，规则列表给出提示', async () => {
  state.ready = false;
  state.rules = [];
  await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/firewall` });
  await sleep(200);
  await waitFor("document.querySelector('#fw-alerts .alert.warn')", '提示');
  has(await q('#fw-alerts'), '240_firewall.sql', '建表脚本');
  eq(await q('#fw-b-new', 'disabled'), true, '新建按钮禁用');
  has(await q('#fw-rules'), '防火墙的表还没建', '列表提示');
  state.ready = true;
});

await t('有场景尚未接入时：顶部提示、编辑器里场景带“尚未接入”标记（可勾选，规则可先配置）', async () => {
  state.pendingScenes = true;
  await nav();
  has(await q('#fw-alerts'), '「SQL 工单」「数据导出」', '未接入场景提示');
  await click('#fw-b-new');
  await waitFor("document.querySelector('.fw-drawer')", '编辑器');
  eq((await qa('.fw-drawer .fw-checks .tag')).length, 2, '未接入标记');
  await ev("document.querySelector('.fw-drawer .fw-checks input[value=\"workflow\"]').click();");
  // 当前页面不再给“选了未接入场景”额外的说明文字，只保留场景名旁的“尚未接入”标记（editor.js 的场景勾选区）
  eq(await ev("return document.querySelector('.fw-drawer .fw-checks input[value=\"workflow\"]').checked;"), true, '可以勾选未接入的场景（规则可先配置）');
  eq(await ev("return document.querySelector('.fw-drawer .fw-checks input[value=\"workflow\"]').parentElement.textContent.includes('尚未接入');"), true, '选中后场景名旁仍带“尚未接入”标记');
  eq(await ev("return document.querySelector('.fw-drawer .fw-checks input[value=\"query\"]').parentElement.textContent.includes('尚未接入');"), false, '已接入的场景不带标记');
  await click('.fw-drawer h3 .x');
  await waitFor("document.querySelector('.cf-dialog') || !document.querySelector('.fw-drawer')", '关闭');
  if (await q('.cf-dialog')) await clickDlg('放弃修改');
  state.pendingScenes = false;
});

await t('条件暂未接入时：条件标签带“暂未接入”标记，编辑器的条件类型下拉带“（暂未接入）”后缀', async () => {
  resetState(); // 前面的“表还没建”用例清空了规则
  state.pendingConds = ['scan_rows'];
  await nav();
  eq(await ev(`return ${rowOf('大范围扫描预警')}.querySelectorAll('.fw-chip.pending').length;`), 1, 'scan_rows 条件标记为暂未接入');
  await ev(`${rowOf('大范围扫描预警')}.querySelectorAll('.fw-link')[1].click();`);
  await waitFor("document.querySelector('.fw-drawer')", '编辑器');
  // 当前页面不再有编辑器内的整段提示，暂未接入的条件类型在条件类型下拉里带“（暂未接入）”后缀（editor.js renderConds）
  const optTexts = await ev("return Array.from(document.querySelectorAll('.fw-drawer .fw-crow select[aria-label=\"条件类型\"] option')).map((o) => o.textContent);");
  truthy(optTexts.some((x) => x.includes('预估扫描行数') && x.includes('（暂未接入）')), 'scan_rows 在条件类型下拉里标注“（暂未接入）”');
  // 每个条件行各有一个下拉，所以按去重后的选项文字计数
  eq([...new Set(optTexts.filter((x) => x.includes('（暂未接入）')))].length, 1, '只有未接入的条件类型带后缀');
  await click('.fw-drawer .ft .btn.ghost');
  await waitFor("!document.querySelector('.fw-drawer')", '关闭');
  state.pendingConds = [];
});

await t('页面无控制台错误（含 CSP 违规）', async () => {
  for (let i = consoleErrors.length - 1; i >= 0; i--) {
    // 故意制造的 409 / 500 / 400 响应，浏览器会把它们记成资源加载错误；这是预期的，不是页面缺陷
    if (/status of (400|409|500)|\b(400|409|500) \(/.test(consoleErrors[i])) consoleErrors.splice(i, 1);
  }
  eq(consoleErrors, [], '控制台');
});

await shot('fw-final');
console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
