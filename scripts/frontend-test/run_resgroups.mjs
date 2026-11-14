/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_resgroups.mjs
 * @time: 2026/10/28
 */
// 平台设置 › 资源组 页面级测试（“通知群”列；编辑框里没有 Webhook 输入框、列出已有的通知渠道、还配着旧版 Webhook 的组才有“清除”入口）：自起假后端（接口形状按 internal/sysadmin）+ 无头 Chrome（CDP），不依赖第三方包。
// 用法：node scripts/frontend-test/run_resgroups.mjs [chrome路径]    环境变量 RG_HTTP_PORT（默认 8921）/ RG_CDP_PORT（默认 9258）/ RG_SHOT（截图目录）。
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
const HTTP = +(process.env.RG_HTTP_PORT || 8921);
const CDP = +(process.env.RG_CDP_PORT || 9258);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'";
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.json': 'application/json', '.svg': 'image/svg+xml' };

// ---- 假后端 ----
const calls = [];
const ok = (data) => ({ code: 'ok', message: 'ok', data });
const base = { users: 5, instances: 3, flows_configured: 3, flows_total: 3, deleted: false, leader: '', leader_display: '', remark: '' };
// 1 生产上线：只配了通知渠道（钉钉 2 个、邮件 1 个，没配旧版 Webhook）；2 UAT上线：只有旧版飞书 Webhook；
// 3 数据导出：旧版钉钉 + 钉钉渠道都有；4 归档数据：什么都没配；5 运维值班：只配了企业微信（1 个）和 Telegram（2 个）渠道
const GROUPS = [
  { id: 1, name: '生产上线', ding_set: false, feishu_set: false, slack_set: false, channels: { dingtalk: 2, email: 1 } },
  { id: 2, name: 'UAT上线', ding_set: false, feishu_set: true, slack_set: false, channels: {} },
  { id: 3, name: '数据导出', ding_set: true, feishu_set: false, slack_set: false, channels: { dingtalk: 1 } },
  { id: 4, name: '归档数据', ding_set: false, feishu_set: false, slack_set: false, channels: {} },
  { id: 5, name: '运维值班', ding_set: false, feishu_set: false, slack_set: false, channels: { wecom: 1, telegram: 2 } },
].map((g) => ({ ...base, ...g }));
const flows = [{ type: 1, label: 'SQL上线申请', configured: true, text: 'DBA' }, { type: 2, label: '查询权限申请', configured: true, text: 'DBA' }, { type: 3, label: '数据归档申请', configured: true, text: 'DBA' }];

function readTpl(rel) { return untranslate(fs.readFileSync(path.join(root, 'web/templates', rel), 'utf8').replace(/\r\n/g, '\n')); }
function page() {
  const t = readTpl('sysadmin/resource_groups.html');
  const ci = t.indexOf('{{define "content"}}'), si = t.indexOf('{{define "scripts"}}');
  const content = t.slice(ci + '{{define "content"}}'.length, si).replace(/\n\{\{end\}\}\s*$/, '');
  const scripts = t.slice(si + '{{define "scripts"}}'.length).replace(/\{\{end\}\}\s*$/, '');
  const head = (t.match(/\{\{define "head"\}\}([\s\S]*?)\{\{end\}\}/) || [, ''])[1];
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="csrf-token" content="tok">
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${head}</head><body><div class="app"><div class="main">${content}</div></div>${scripts}</body></html>`;
}

const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, 'http://x');
  const p = decodeURIComponent(u.pathname);
  let body = '';
  for await (const c of req) body += c;
  const json = (status, obj) => { res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(obj)); };
  let parsed = null;
  try { parsed = body ? JSON.parse(body) : null; } catch (_) { /* 非 JSON */ }
  if (req.method !== 'GET' || p.startsWith('/api/')) calls.push({ method: req.method, url: p + u.search, body: parsed });
  if (p === '/favicon.ico') { res.writeHead(204); res.end(); return; }
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
  if (p === '/api/v1/button-perms/me') return json(200, ok({ mode: 'compat', deny_display: 'hide', superuser: true, allowed: [] }));
  if (p === '/api/v1/settings/users') return json(200, ok({ items: [], total: 0 }));
  const API = '/api/v1/settings/resource-groups';
  let m;
  if (p === API + '/summary') return json(200, ok({ enabled: 4, disabled: 0, no_instance: 0, flow_incomplete: 0, no_bot: 1 }));
  if (p === API) return json(200, ok(GROUPS));
  if ((m = p.match(/^\/api\/v1\/settings\/resource-groups\/(\d+)$/)) && req.method === 'PUT') return json(200, ok({}));
  if ((m = p.match(/^\/api\/v1\/settings\/resource-groups\/(\d+)$/)) && req.method === 'GET') {
    const g = GROUPS.find((x) => x.id === Number(m[1]));
    return g ? json(200, ok({ ...g, flows })) : json(404, { code: 'not_found', message: '资源组不存在' });
  }
  res.writeHead(404); res.end('nf');
}).listen(HTTP);

// ---- Chrome ----
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'rg-chrome-'));
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
  if (!process.env.RG_SHOT) return;
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.mkdirSync(process.env.RG_SHOT, { recursive: true });
  fs.writeFileSync(path.join(process.env.RG_SHOT, name + '.png'), Buffer.from(r.data, 'base64'));
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

console.log(`资源组页面测试（http://127.0.0.1:${HTTP}/page）`);
await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/page` });
await waitFor("document.querySelectorAll('#rg-rows tr').length === 5 && document.querySelector('#rg-rows .badge')", '资源组列表');

// 每行“通知群”那一格里各徽标的文字与提示
const hookCells = () => ev(`return [...document.querySelectorAll('#rg-rows tr')].map((tr) => { const td = tr.children[6]; return [...td.querySelectorAll('.badge')].map((b) => [b.textContent, b.title, b.className]); });`);

await t('通知群：只配了通知渠道的组显示渠道个数（钉钉 ×2、邮件 ×1），不再全是“未配”', async () => {
  const cells = await hookCells();
  eq(cells[0].map((b) => b[0]), ['钉钉 ×2', '飞书 未配', 'Slack 未配', '邮件 ×1'], '生产上线');
  has(cells[0][0][1], '通知渠道 2 个（含全局渠道）');
  has(cells[0][0][2], 'ok');
  has(cells[0][1][2], 'badge');
  truthy(!cells[0][1][2].includes('ok'), '飞书未配不应是 ok 样式');
});
await t('通知群：只有旧版 Webhook 的组标“（旧版）”', async () => {
  const cells = await hookCells();
  eq(cells[1].map((b) => b[0]), ['钉钉 未配', '飞书（旧版）', 'Slack 未配'], 'UAT上线');
  has(cells[1][1][1], '资源组上的旧版 Webhook 已配置');
});
await t('通知群：旧版与渠道都有时以渠道个数为准，提示里两者都写明', async () => {
  const cells = await hookCells();
  eq(cells[2][0][0], '钉钉 ×1', '数据导出');
  has(cells[2][0][1], '通知渠道 1 个');
  has(cells[2][0][1], '旧版 Webhook 已配置');
});
await t('通知群：什么都没配的组三个都是“未配”，企业微信 / Lark / Telegram / 邮件不占位', async () => {
  const cells = await hookCells();
  eq(cells[3].map((b) => b[0]), ['钉钉 未配', '飞书 未配', 'Slack 未配'], '归档数据');
});
await t('通知群：配了企业微信 / Telegram 渠道的组显示“企业微信 ×1”（排在钉钉后面）和“Telegram ×2”（排在 Slack 后面），其它组不出现', async () => {
  const cells = await hookCells();
  eq(cells[4].map((b) => b[0]), ['钉钉 未配', '企业微信 ×1', '飞书 未配', 'Slack 未配', 'Telegram ×2'], '运维值班');
  has(cells[4][1][1], '通知渠道 1 个（含全局渠道）');
  has(cells[4][1][2], 'ok');
  has(cells[4][4][1], '通知渠道 2 个（含全局渠道）');
  has(cells[4][4][2], 'ok');
  for (const i of [0, 1, 2, 3]) truthy(!cells[i].some((b) => b[0].startsWith('企业微信') || b[0].startsWith('Telegram')), `第 ${i + 1} 个组不应出现企业微信 / Telegram`);
});
await shot('rg-list');

await t('编辑框：已有的通知渠道里列出企业微信 / Telegram', async () => {
  await ev("[...document.querySelectorAll('#rg-rows tr')][4].querySelector('button').click();");
  await waitFor("document.querySelector('#re-chan-now') && !document.querySelector('#re-chan-now').hidden", '通知渠道提示');
  eq(await q('#re-chan-now'), '该资源组已有的通知渠道：企业微信 ×1、Telegram ×2（含全局渠道）');
  await ev("document.querySelector('#m-res-edit [data-close]').click();");
});

await t('编辑框：没有 Webhook 输入框，只有“去通知渠道配置”的说明，并列出该组已有的通知渠道', async () => {
  await ev("[...document.querySelectorAll('#rg-rows tr')][0].querySelector('button').click();");
  await waitFor("document.querySelector('#re-chan-now') && !document.querySelector('#re-chan-now').hidden", '通知渠道提示');
  eq(await qa('#m-res-edit input[type=password], #m-res-edit input.mono', 'id'), [], '不应再有 Webhook 输入框');
  eq(await q('#re-ding'), null, '钉钉输入框已去掉');
  has(await q('#m-res-edit .small-note'), '资源组的通知请到“通知渠道”配置');
  eq(await q('#m-res-edit .small-note a', 'getAttribute("href")'), '/settings/notify');
  eq(await q('#re-chan-now'), '该资源组已有的通知渠道：钉钉 ×2、邮件 ×1（含全局渠道）');
  eq(await q('#re-legacy', 'hidden'), true, '没有旧版 Webhook 的组不出现清除入口');
  await shot('rg-edit');
  await ev("document.querySelector('#m-res-edit [data-close]').click();");
});
await t('编辑框：还配着旧版 Webhook 的组出现说明和对应的“清除”勾选，保存时只发清除标记', async () => {
  await ev("[...document.querySelectorAll('#rg-rows tr')][1].querySelector('button').click();");
  await waitFor("document.querySelector('#re-legacy') && !document.querySelector('#re-legacy').hidden", '旧版 Webhook 清除入口');
  has(await q('#re-legacy .small-note'), '这个资源组还配着旧版 Webhook');
  eq([await q('#re-ding-clear', 'hidden'), await q('#re-feishu-clear', 'hidden'), await q('#re-slack-clear', 'hidden')], [true, false, true], '只有飞书显示清除');
  await shot('rg-edit-legacy');
  await click('#re-feishu-cb');
  await click('#re-save');
  await waitFor("document.querySelector('#m-res-edit').hidden", '保存后关闭编辑框');
  const put = calls.filter((c) => c.method === 'PUT').pop();
  truthy(put && put.url === '/api/v1/settings/resource-groups/2', '发出了 PUT');
  eq([put.body.clear_feishu, put.body.clear_ding, put.body.clear_slack], [true, false, false], '清除标记');
  eq(Object.keys(put.body).filter((k) => k.endsWith('_webhook')), [], '不再提交 Webhook 内容');
});
await t('编辑框：没有通知渠道的组提示“还没有配置”；新建时不显示这行', async () => {
  await ev("[...document.querySelectorAll('#rg-rows tr')][3].querySelector('button').click();");
  await waitFor("document.querySelector('#re-chan-now').textContent === '该资源组还没有配置通知渠道。'", '换成第 4 个组的提示（编辑框是异步取详情的）');
  eq(await q('#re-chan-now'), '该资源组还没有配置通知渠道。');
  await ev("document.querySelector('#m-res-edit [data-close]').click();");
  await ev("document.querySelector('#rg-new').click();");
  await waitFor("document.querySelector('#re-chan-now').hidden", '新建时隐藏');
  await ev("document.querySelector('#m-res-edit [data-close]').click();");
});

await t('页面无控制台错误（含 CSP 违规）', async () => {
  eq(consoleErrors, [], '控制台');
});

await shot('rg-final');
console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
