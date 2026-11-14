/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_archive_epoch.mjs
 * @time: 2026/10/28
 */
// 数据归档向导：整型时间戳列（bigint / int 里的 10 位秒 / 13 位毫秒）的回归测试。
// 静态服务 + 假后端 + 无头 Chrome（CDP），不依赖第三方包；页面模板与脚本取自 web/templates/archive、web/static。
// 用法：node scripts/frontend-test/run_archive_epoch.mjs [chrome路径]
// 环境变量：AR_HTTP_PORT（默认 8913）、AR_CDP_PORT（默认 9249）、ARCHIVE_SHOT（给一个 .png 路径就保存第 2 步的截图）。
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
const HTTP = +(process.env.AR_HTTP_PORT || 8913);
const CDP = +(process.env.AR_CDP_PORT || 9249);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const base = `http://127.0.0.1:${HTTP}`;
// 比应用的 CSP 更严：style-src 去掉 unsafe-inline，以便发现内联样式
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'";
const MIME = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8', '.md': 'text/plain; charset=utf-8' };

// ---------------------------------------------------------------- 假后端
const J = JSON.stringify;
const reqLog = [];

function pageHtml() {
  const src = untranslate(fs.readFileSync(path.join(root, 'web/templates/archive/index.html'), 'utf8'));
  const head = (src.match(/\{\{define "head"\}\}(.*?)\{\{end\}\}/s) || [, ''])[1];
  const i = src.indexOf('{{define "content"}}');
  const rest = src.slice(i + '{{define "content"}}'.length);
  const content = rest.slice(0, rest.indexOf('{{end}}\n\n{{define'));
  const scripts = (src.match(/\{\{define "scripts"\}\}(.*?)\{\{end\}\}/s) || [, ''])[1];
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="csrf-token" content="t"><title>数据归档</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${head}</head>
<body><div class="app"><div class="main">${content}</div></div>${scripts}</body></html>`;
}

const LIMITS = {
  name_len: [2, 50], retention_max_days: 3650, extra_where_max: 500, batch_size: [100, 10000], sleep_ms: [0, 5000], sleep_ms_min_when_delete: 50,
  max_threads_running: [4, 1000], max_lag_seconds: [1, 3600], min_disk_free_pct: [5, 50], max_wait_seconds: [60, 3600], target_batch_ms: [100, 5000],
  max_consecutive_failures: [1, 10], run_max_rows: [100, 2000000000], policy_max_rows: [1000, 2000000000], part_mb: [16, 4096], purge_reason: [5, 500],
  store_retention_max: 36500,
};
const POLICY = {
  min_retention_days: 30, allow_purge_no_backup: false, protected_patterns: ['mysql'], max_rows_per_run: 5000000, max_concurrent_per_instance: 2,
  defaults: { batch_size: 1000, sleep_ms: 200, max_threads_running: 32, max_lag_seconds: 5, min_disk_free_pct: 15, target_batch_ms: 500, max_wait_seconds: 600, max_consecutive_failures: 3 },
};
const col = (name, type, o = {}) => ({ name, type, nullable: false, indexed: false, is_time: false, is_int: /^(big)?int/.test(type), ...o });
const TABLES = {
  // 截图里的表：时间列是 bigint(20) 的 13 位毫秒时间戳，ReadTime = 0 表示未读
  sys_users_notify_message: {
    rows: 419835,
    columns: [col('Id', 'bigint(20)', { indexed: true }), col('UserId', 'bigint(20)', { indexed: true }), col('Title', 'varchar(200)'), col('Status', 'int(11)'),
      col('CreateTime', 'bigint(20)', { indexed: true }), col('LastUpdateTime', 'bigint(20)'), col('ReadTime', 'bigint(20)'), col('BrokenTs', 'bigint(20)')],
  },
  orders: { rows: 300, columns: [col('id', 'bigint(20)', { indexed: true }), col('created_at', 'datetime', { indexed: true, is_time: true }), col('status', 'int(11)')] },
};
const PROBES = {
  CreateTime: { sampled: 1000, in_sec: 0, in_ms: 1000, sample: 1700000000000, has_range: true, min: 1600000000000, max: 1760000000000, offset_sec: 28800, guess_unit: 'epoch_ms' },
  LastUpdateTime: { sampled: 1000, in_sec: 1000, in_ms: 0, sample: 1700000000, has_range: false, min: 0, max: 0, offset_sec: 28800, guess_unit: 'epoch_s' },
  Id: { sampled: 1000, in_sec: 0, in_ms: 0, sample: 7, has_range: false, min: 0, max: 0, offset_sec: 28800, guess_unit: '' },
  ReadTime: { sampled: 0, in_sec: 0, in_ms: 0, sample: 0, has_range: false, min: 0, max: 0, offset_sec: 28800, guess_unit: '' },
};

function whereOf(b) {
  const c = '`' + b.time_column + '`';
  const sec = Number(b.retention_days) * 86400;
  if (b.time_unit === 'epoch_ms') return `${c} > 0 AND ${c} < (UNIX_TIMESTAMP() - ${sec}) * 1000`;
  if (b.time_unit === 'epoch_s') return `${c} > 0 AND ${c} < UNIX_TIMESTAMP() - ${sec}`;
  return `${c} < NOW() - INTERVAL ${b.retention_days} DAY`;
}

async function readBody(req) {
  const chunks = [];
  for await (const c of req) chunks.push(c);
  const s = Buffer.concat(chunks).toString('utf8');
  return s ? JSON.parse(s) : undefined;
}
const json = (res, code, body) => { res.writeHead(code, { 'Content-Type': 'application/json' }); res.end(J(body)); };
const ok = (res, data) => json(res, 200, { data });

async function api(req, res, url) {
  const p = url.pathname.replace('/api/v1', '');
  const q = Object.fromEntries(url.searchParams);
  const body = req.method === 'GET' ? undefined : await readBody(req);
  reqLog.push({ method: req.method, path: url.pathname, query: q, body });
  if (p === '/button-perms/me') return ok(res, { mode: 'hide', deny_display: 'hide', superuser: true, allowed: [] });
  if (p === '/archive/meta') {
    return ok(res, { ready: true, me: { username: 'alice', display: 'Alice', is_super: true }, limits: LIMITS, policy: POLICY, enums: { modes: [], task_status: [], run_result: [], triggers: [], store_types: [], file_formats: ['csv', 'sql'] } });
  }
  if (p === '/archive/stores') return ok(res, []);
  if (p === '/archive/sources/instances') return ok(res, [{ id: 1, name: 'mysql-prod', db_type: 'mysql', env: 'prod', status: 'ok', resource_groups: [{ id: 1, name: '订单组', has_flow: true }] }]);
  if (p === '/archive/sources/databases') return ok(res, [{ name: 'lotto', protected: false }, { name: 'lotto_archive', protected: false }]);
  if (p === '/archive/sources/tables') return ok(res, Object.entries(TABLES).map(([name, t]) => ({ name, rows: t.rows, protected: false })));
  if (p === '/archive/sources/columns') {
    const t = TABLES[q.table];
    return ok(res, { columns: t.columns, pk_cols: [t.columns[0].name], has_pk: true, partitioned: false, has_triggers: false, fk_parents: [], rows: t.rows, protected: false, active_task: null });
  }
  if (p === '/archive/flow') return ok(res, { exists: true, levels: [{ level: 0, group_id: 10, group_name: '团队负责人', users: ['tl_wang'] }] });
  if (p === '/archive/sources/time-probe') {
    if (q.column === 'BrokenTs') return json(res, 502, { code: 'source_error', message: '探测时间列失败：context deadline exceeded' });
    return ok(res, PROBES[q.column] || PROBES.Id);
  }
  if (p === '/archive/preview') {
    return ok(res, { where: whereOf(body), sql: { select: 'SELECT …', insert: 'INSERT …', delete: 'DELETE …' }, issues: [], needs_approval: true, next_runs: [], dest_table_name: 'x' });
  }
  json(res, 404, { code: 'not_found', message: p });
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://x');
  try {
    if (url.pathname.startsWith('/api/v1/')) return await api(req, res, url);
    if (url.pathname === '/__reqs') return json(res, 200, reqLog);
    if (url.pathname === '/__reset') { reqLog.length = 0; return json(res, 200, {}); }
    if (url.pathname === '/archive') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP }); return res.end(pageHtml()); }
    if (url.pathname.startsWith('/static/')) {
      const file = path.join(staticRoot, decodeURIComponent(url.pathname.slice(8)));
      if (file.startsWith(staticRoot) && fs.existsSync(file)) {
        res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream', 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' });
        return fs.createReadStream(file).pipe(res);
      }
    }
    res.writeHead(404); res.end('nf');
  } catch (e) {
    res.writeHead(500); res.end(String(e && e.stack));
  }
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));

// ---------------------------------------------------------------- 无头 Chrome
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'ar-chrome-'));
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
const errors = []; // 控制台错误 / 未捕获异常 / CSP 违规
ws.addEventListener('message', (m) => {
  const d = JSON.parse(m.data);
  if (d.id && pend.has(d.id)) {
    const { res, rej } = pend.get(d.id);
    pend.delete(d.id);
    if (d.error) rej(new Error(d.error.message)); else res(d.result);
  } else if (d.method === 'Runtime.exceptionThrown') {
    errors.push('exception: ' + d.params.exceptionDetails.text + ' ' + ((d.params.exceptionDetails.exception || {}).description || ''));
  } else if (d.method === 'Log.entryAdded' && d.params.entry.level === 'error') {
    // 测试里故意触发的 502（探测失败）与 favicon 404 不算页面错误；CSP 违规（Refused to ...）与脚本异常才算
    if (!/Failed to load resource/.test(d.params.entry.text)) errors.push('log: ' + d.params.entry.text + ' ' + (d.params.entry.url || ''));
  } else if (d.method === 'Runtime.consoleAPICalled' && d.params.type === 'error') {
    errors.push('console: ' + d.params.args.map((a) => a.value || a.description).join(' '));
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
const has = (text, sub, m) => { if (!String(text).includes(sub)) throw new Error(`${m || ''} 应包含 ${J(sub)}，实得 ${J(String(text).slice(0, 400))}`); };
const lacks = (text, sub, m) => { if (String(text).includes(sub)) throw new Error(`${m || ''} 不应包含 ${J(sub)}`); };
const q = (sel, prop = 'textContent') => ev(`const e=document.querySelector(${J(sel)}); return e ? e.${prop} : null;`);
const exists = (sel) => ev(`return !!document.querySelector(${J(sel)});`);
const setSel = (sel, v) => ev(`const s=document.querySelector(${J(sel)}); if(!s) throw new Error('找不到 '+${J(sel)}); s.value=${J(v)}; s.dispatchEvent(new Event('change',{bubbles:true}));`);
const setInput = (sel, v) => ev(`const i=document.querySelector(${J(sel)}); i.value=${J(v)}; i.dispatchEvent(new Event('input',{bubbles:true}));`);
const clickBtn = (scope, text) => ev(`const b=[...document.querySelectorAll(${J(scope)})].find(e=>e.textContent.trim()===${J(text)}); if(!b) throw new Error('找不到按钮 '+${J(text)}); b.click();`);
async function until(fn, what, ms = 5000) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try { if (await fn()) return; } catch (_) { /* 继续等 */ }
    await sleep(60);
  }
  throw new Error('等待超时：' + what);
}
const reqsOf = async (part, method) => (await (await fetch(`${base}/__reqs`)).json()).filter((r) => r.path.includes(part) && (!method || r.method === method));
const lastPreview = async () => { const l = await reqsOf('/archive/preview', 'POST'); return l.length ? l[l.length - 1].body : null; };
const SEL = (label) => `#ar-w-body select[aria-label="${label}"]`;
const probeText = () => q('#ar-w-probe');
const unitVal = () => q('#ar-w-unit', 'value');

console.log(`数据归档向导（整型时间戳列）界面测试（${base}/archive#wizard）`);

await send('Page.navigate', { url: `${base}/archive#wizard` });
await until(() => ev('return document.readyState==="complete"'), '页面加载');
await until(() => exists(SEL('实例')), '向导第 1 步');

// ---------------- 第 1 步：选来源 ----------------
await t('选表后：默认选中名字像时间且有索引的整型列 CreateTime，并标出整型候选列', async () => {
  await setInput('#ar-w-body input[type="text"]', '通知消息归档');
  await setSel(SEL('实例'), '1');
  await until(() => exists(`${SEL('库')} option[value="lotto"]`), '库列表');
  await setSel(SEL('库'), 'lotto');
  await until(() => exists(`${SEL('表')} option[value="sys_users_notify_message"]`), '表列表');
  await setSel(SEL('表'), 'sys_users_notify_message');
  await until(() => exists('#ar-w-tinfo code'), '表信息');
  const info = await q('#ar-w-tinfo');
  has(info, '整型列（可能是时间戳）');
  has(info, 'CreateTime');
  has(info, 'LastUpdateTime');
  has(info, '另有 4 个整型列', '没有名字像时间的整型列合并成一句'); // Id UserId Status BrokenTs
  has(info, '没有 datetime / timestamp / date 列', '没有日期时间列时的说明');
  lacks(info, '没有可作时间列的列', '有整型候选时不应判红');
  eq((await reqsOf('/archive/sources/time-probe')).length, 1, '选表后应自动探测默认列');
});

await t('下一步：第 2 步里时间列是 CreateTime，时间单位自动预填为 13 位毫秒，并显示最早 / 最晚时间', async () => {
  await clickBtn('#ar-w-nav button', '下一步');
  await until(() => exists('#ar-w-unit'), '时间单位下拉');
  eq(await q(SEL('时间列'), 'value'), 'CreateTime');
  has(await q(`${SEL('时间列')} option:checked`), 'bigint(20) · 整型 · 有索引');
  await until(async () => (await unitVal()) === 'epoch_ms', '探测后预填单位');
  await until(async () => (await probeText()).includes('按 13 位解读'), '探测结果');
  const pt = await probeText();
  has(pt, '最早 2020-09-13 20:26:40，最晚 2025-10-09 16:53:20（服务器时区）');
  truthy(await exists('#ar-w-probe .ar-notice.ok'), '应是绿色提示');
});

await t('预览请求带着 time_unit，系统生成的 WHERE 是毫秒条件', async () => {
  await until(async () => { const b = await lastPreview(); return b && b.time_unit === 'epoch_ms'; }, '带单位的预览请求');
  const b = await lastPreview();
  eq([b.time_column, b.time_unit], ['CreateTime', 'epoch_ms']);
  await until(async () => (await q('#ar-w-where')).includes('UNIX_TIMESTAMP()'), 'WHERE 刷新');
  has(await q('#ar-w-where'), '`CreateTime` > 0 AND `CreateTime` < (UNIX_TIMESTAMP() - 15552000) * 1000');
});

if (process.env.ARCHIVE_SHOT) {
  await send('Emulation.setDeviceMetricsOverride', { width: 1100, height: 760, deviceScaleFactor: 1, mobile: false });
  await sleep(300);
  const shot = await send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(process.env.ARCHIVE_SHOT, Buffer.from(shot.data, 'base64'));
}

// ---------------- 单位与列选错 ----------------
await t('把单位改成 10 位秒：探测发现值是 13 位，给出红色提示', async () => {
  await setSel('#ar-w-unit', 'epoch_s');
  const pt = await probeText();
  has(pt, '看起来是 13 位的 Unix 时间戳，与所选的 10 位不符');
  truthy(await exists('#ar-w-probe .ar-notice.bad'), '应是红色提示');
  await until(async () => { const b = await lastPreview(); return b && b.time_unit === 'epoch_s'; }, '预览请求更新');
  await until(async () => (await q('#ar-w-where')).includes('UNIX_TIMESTAMP() - 15552000') && !(await q('#ar-w-where')).includes('* 1000'), 'WHERE 变成秒条件');
});

await t('换成 Id 列：单位清空，探测说不像时间戳；不选单位不能进下一步', async () => {
  await setSel(SEL('时间列'), 'Id');
  await until(async () => (await probeText()).includes('不像 10 位或 13 位的 Unix 时间戳'), 'Id 的探测结果');
  eq(await unitVal(), '', '换列后单位重新判断');
  await clickBtn('#ar-w-nav button', '下一步');
  await until(async () => (await q('#ar-w-err')).includes('请选择时间单位（10 位秒 / 13 位毫秒）'), '缺单位的提示');
  eq(await q('#ar-w-steps li.cur span'), '条件', '仍停在第 2 步');
});

await t('换成 ReadTime（几乎全是 0）：提示没有大于 0 的值，不预填单位', async () => {
  await setSel(SEL('时间列'), 'ReadTime');
  await until(async () => (await probeText()).includes('没有大于 0 的值'), 'ReadTime 的探测结果');
  truthy(await exists('#ar-w-probe .ar-notice.warn'), '应是黄色提示');
  eq(await unitVal(), '');
});

await t('换成探测失败的列：黄色提示，仍可手动选单位', async () => {
  await setSel(SEL('时间列'), 'BrokenTs');
  await until(async () => (await probeText()).includes('无法核对该列的取值'), '探测失败提示');
  has(await probeText(), 'context deadline exceeded');
  await setSel('#ar-w-unit', 'epoch_ms');
  eq(await unitVal(), 'epoch_ms');
  await until(async () => { const b = await lastPreview(); return b && b.time_column === 'BrokenTs' && b.time_unit === 'epoch_ms'; }, '预览请求');
});

await t('换成 LastUpdateTime（10 位秒）：预填 10 位，列上没有索引时只给抽样结果', async () => {
  await setSel(SEL('时间列'), 'LastUpdateTime');
  await until(async () => (await unitVal()) === 'epoch_s', '预填 10 位');
  await until(async () => (await probeText()).includes('按 10 位解读：抽样 1000 行'), '抽样结果');
  has(await probeText(), '例如 1700000000 对应 2023-11-15 06:13:20（服务器时区）');
});

await t('选了正确的单位后可以进入下一步', async () => {
  await setSel(SEL('时间列'), 'CreateTime');
  await until(async () => (await unitVal()) === 'epoch_ms', '预填');
  await clickBtn('#ar-w-nav button', '下一步');
  await until(async () => (await q('#ar-w-steps li.cur span')) === '模式与备份', '进入第 3 步');
});

// ---------------- 日期时间列不受影响 ----------------
await t('日期时间列的表：默认选 created_at，没有时间单位一行，请求里 time_unit 为空', async () => {
  await clickBtn('#ar-w-nav button', '上一步');
  await clickBtn('#ar-w-nav button', '上一步');
  await until(() => exists(SEL('表')), '回到第 1 步');
  await setSel(SEL('表'), 'orders');
  await until(async () => (await q('#ar-w-tinfo')).includes('created_at'), '表信息');
  has(await q('#ar-w-tinfo'), '2 个 int / bigint 列，没有名字像时间的', 'id / status 仍列为整型候选，但没有名字像时间的');
  await clickBtn('#ar-w-nav button', '下一步');
  await until(() => exists(SEL('时间列')), '第 2 步');
  eq(await q(SEL('时间列'), 'value'), 'created_at');
  eq(await exists('#ar-w-unit'), false, '日期时间列不显示时间单位');
  eq(await exists('#ar-w-probe'), false);
  await until(async () => { const b = await lastPreview(); return b && b.time_column === 'created_at' && b.time_unit === ''; }, '预览请求');
  await until(async () => (await q('#ar-w-where')).includes('`created_at` < NOW() - INTERVAL 180 DAY'), 'datetime 的条件不变');
  eq((await reqsOf('/archive/sources/time-probe')).filter((r) => r.query.table === 'orders').length, 0, '日期时间列不探测');
});

await t('在日期时间列和整型列之间来回切换：单位随列类型出现 / 消失', async () => {
  await setSel(SEL('时间列'), 'id');
  await until(() => exists('#ar-w-unit'), '整型列出现单位下拉');
  await setSel(SEL('时间列'), 'created_at');
  await until(async () => !(await exists('#ar-w-unit')), '日期时间列隐藏单位下拉');
  await until(async () => { const b = await lastPreview(); return b && b.time_column === 'created_at' && b.time_unit === ''; }, '预览请求回到 datetime');
});

// ---------------- 窄屏 ----------------
await t('手机宽度 375：第 2 步（整型时间列）没有页面级横向滚动', async () => {
  await setSel(SEL('时间列'), 'id');
  await until(() => exists('#ar-w-probe'), '核对区');
  await send('Emulation.setDeviceMetricsOverride', { width: 375, height: 812, deviceScaleFactor: 1, mobile: true });
  await sleep(300);
  const w = await ev('return [document.documentElement.scrollWidth, window.innerWidth];');
  truthy(w[0] <= w[1] + 1, `页面出现横向滚动 ${w[0]} > ${w[1]}`);
  await send('Emulation.clearDeviceMetricsOverride');
});

await t('全程无控制台错误 / CSP 违规', async () => { eq(errors, [], '控制台错误'); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
if (fail) console.log('失败项：\n  ' + failures.join('\n  '));
cleanup();
process.exit(fail ? 1 : 0);
