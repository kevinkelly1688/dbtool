/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_shell.mjs
 * @time: 2026/10/28
 */
// 外壳页面测试：侧栏手风琴（默认只展开当前页所在分组、点开一组其余折叠、不记忆）；慢查日志页（SQL 优化）的空状态提示；系统配置页（st-* 元素）的浏览：提醒、分组导航、设置项展示、搜索与筛选。
// 自起假后端（multi_engine_fake.mjs 的 /navtest 页：结构同 layout.html 的 <nav>，脚本取自 web/static/js/common.js）+ 无头 Chrome（CDP），不依赖第三方包。
// 用法：node scripts/frontend-test/run_shell.mjs [chrome路径]    环境变量 SHELL_HTTP_PORT（默认 8908）/ SHELL_CDP_PORT（默认 9245）。
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { startServer, calls } from './multi_engine_fake.mjs';

const HTTP = +(process.env.SHELL_HTTP_PORT || 8908);
const CDP = +(process.env.SHELL_CDP_PORT || 9245);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const server = await startServer(HTTP);
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'shell-chrome-'));
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

const groupState = () => ev("return [...document.querySelectorAll('#nav .ng')].map((g) => [g.dataset.group, g.classList.contains('fold'), g.querySelector('.ng-btn').getAttribute('aria-expanded')]);");
const ALL_KEYS = ['check', 'query', 'opt', 'perf', 'inst', 'access', 'secure', 'plat', 'help'];

console.log(`外壳侧栏页面测试（http://127.0.0.1:${HTTP}/navtest）`);

await nav('/navtest');
await ev('localStorage.clear();');
await nav('/navtest');
const openKeys = async () => (await groupState()).filter((g) => !g[1]).map((g) => g[0]);
await t('首页（不属于任何分组）：全部分组默认折叠，条目都不显示', async () => {
  eq((await groupState()).map((g) => g[0]), ALL_KEYS);
  eq(await openKeys(), [], '没有展开的分组');
  eq((await groupState()).every((g) => g[2] === 'false'), true, 'aria-expanded 都是 false');
  const hidden = await ev("return [...document.querySelectorAll('#nav .ng-items')].every((e) => e.getClientRects().length === 0);");
  eq(hidden, true, '每组的条目都不显示');
});
await t('点开一组：它展开，其余仍折叠；再点另一组：前一组自动折叠', async () => {
  await ev("document.querySelector('#nav .ng[data-group=\"perf\"] .ng-btn').click();");
  eq(await openKeys(), ['perf']);
  eq(await ev("return document.querySelector('#nav .ng[data-group=\"perf\"] .ng-btn').getAttribute('aria-expanded');"), 'true');
  eq((await ev("return document.querySelector('#nav .ng[data-group=\"perf\"] .ng-items').getClientRects().length;")) > 0, true, '展开的组显示条目');
  await ev("document.querySelector('#nav .ng[data-group=\"query\"] .ng-btn').click();");
  eq(await openKeys(), ['query'], '同一时间只有一组展开');
  eq(await ev("return document.querySelector('#nav .ng[data-group=\"perf\"] .ng-items').getClientRects().length;"), 0, 'perf 已折叠');
});
await t('再点已展开的那组：折叠，全部分组都折叠', async () => {
  await ev("document.querySelector('#nav .ng[data-group=\"query\"] .ng-btn').click();");
  eq(await openKeys(), []);
});
await t('不记忆：刷新后回到默认；旧版本记下的状态被清掉', async () => {
  await ev("localStorage.setItem('dbt_nav_state', JSON.stringify({ perf: true, secure: true }));");
  await ev("document.querySelector('#nav .ng[data-group=\"perf\"] .ng-btn').click();");
  await nav('/navtest');
  eq(await openKeys(), [], '刷新后不保留手动展开的分组');
  eq(await ev("return localStorage.getItem('dbt_nav_state');"), null, '旧的记忆已清除');
});
await t('当前页所在分组默认展开，其余折叠；点别的组后当前组折叠', async () => {
  await nav('/navtest-on-secure');
  eq(await openKeys(), ['secure']);
  await ev("document.querySelector('#nav .ng[data-group=\"opt\"] .ng-btn').click();");
  eq(await openKeys(), ['opt'], '手风琴：打开 opt，secure 折叠');
});

// =============== 慢 SQL 洞察页（性能优化）===============
await nav('/slowquery');
await waitFor("document.querySelectorAll('#sq-inst option').length > 1", '实例下拉');
const rowsText = () => ev("return document.querySelector('#sq-srows').textContent.trim();");
const pick = async (v) => { await ev(`const s=document.getElementById('sq-inst'); s.value=${JSON.stringify(String(v))}; s.dispatchEvent(new Event('change'));`); await sleep(400); };
await t('刚打开页面：不自动查询，提示先选实例；直接点“查询”也只是提醒先选；下拉里有汇总项和各实例', async () => {
  has(await rowsText(), '请选择实例');
  await ev("document.getElementById('sq-form').requestSubmit();"); // 没选实例就点“查询”
  await sleep(200);
  has(await q('#sq-msg'), '请先选择实例');
  eq((await qa('#sq-inst option')).slice(0, 3), ['请选择实例', '全部实例（汇总）', 'mysql-off（mysql）']);
  eq(calls.some((c) => c.url.startsWith('/api/v1/slowlog/review')), false, '没选实例不发统计请求');
});
await t('选了没开采集的实例：空结果直接点名“没有开启慢日志采集”并说明去哪里打开', async () => {
  await pick(1);
  const txt = await rowsText();
  has(txt, '该时间范围内没有慢日志统计');
  has(txt, '这个实例没有开启“慢日志采集”');
  has(txt, '实例管理');
});
await t('选了开了采集的实例：空结果给出“采集器状态”入口，不再说“没开启”', async () => {
  await pick(2);
  const txt = await rowsText();
  has(txt, '请确认实例已开启“慢日志采集”');
  has(txt, '采集器状态');
  eq(txt.includes('这个实例没有开启'), false);
});
await t('全部实例（汇总）：空结果提示“至少要有一个开启了慢日志采集”', async () => {
  await pick('all');
  has(await rowsText(), '至少要有一个开启了“慢日志采集”');
});
await t('明细页签：没有数据时同样给出原因', async () => {
  await pick(1);
  await ev("document.querySelector('#slowquery > .tabbar .tab[data-tab=\"det\"]').click();");
  await sleep(400);
  const txt = await ev("return document.querySelector('#sq-drows').textContent.trim();");
  has(txt, '没有符合条件的慢日志明细');
  has(txt, '这个实例没有开启“慢日志采集”');
});
// =============== 平台设置 › 系统配置（浏览）===============
// 页面与数据形状：web/templates/sysconfig/index.html、web/static/js/sysconfig/main.js、internal/sysconfig/http.go（元素 id 为 st-*）。
// 修改 / 草稿 / 保存 / 变更记录的交互在 run_sysconfig_edit.mjs 里测，这里只看浏览：提醒、分组导航、设置项展示、搜索与筛选。
await nav('/settings/config');
await waitFor("document.querySelectorAll('#st-nav button[data-group]').length > 1 && document.querySelector('#st-content .st-row')", '配置列表');
if (process.env.SHELL_SHOT) { // 设置 SHELL_SHOT=目录 时截图，便于人工看版面
  await sleep(200);
  const shot = await send('Page.captureScreenshot', { format: 'png' });
  fs.mkdirSync(process.env.SHELL_SHOT, { recursive: true });
  fs.writeFileSync(path.join(process.env.SHELL_SHOT, 'sysconfig.png'), Buffer.from(shot.data, 'base64'));
}
const stKeys = () => qa('#st-content .st-row', 'dataset.key');
const stRow = (key) => `document.querySelector(${JSON.stringify(`.st-row[data-key="${key}"]`)})`;
const stText = (key) => ev(`const r = ${stRow(key)}; return r ? r.textContent : null;`);
const stGo = (g) => ev(`document.querySelector(${JSON.stringify(`#st-nav button[data-group="${g}"]`)}).click();`);
const stSet = (id, v) => ev(`const e = document.getElementById(${JSON.stringify(id)}); if (e.type === 'checkbox') { e.checked = ${JSON.stringify(v)}; e.dispatchEvent(new Event('change')); } else { e.value = ${JSON.stringify(v)}; e.dispatchEvent(new Event('input')); }`);
await t('系统配置：页头；SECRET_KEY 公开缺省值与密文解不开的项各有一条提醒', async () => {
  has(await q('#st-main h1'), '系统配置');
  eq(await qa('#st-alerts .alert', 'className'), ['alert warn', 'alert err']);
  const al = await q('#st-alerts');
  has(al, 'SECRET_KEY 仍是公开的缺省值');
  has(al, 'dbt-admin gen-secrets');
  has(al, '有 1 项敏感设置的密文用当前密钥');
  has(al, 'sms.tencent_secret_key');
});
await t('系统配置：左侧分组导航——当前分组高亮，已修改项数与解不开项数用角标标出', async () => {
  const navs = await ev("return [...document.querySelectorAll('#st-nav button[data-group]')].map((b) => { const g = b.querySelector('.st-n-badge'); return [b.dataset.group, b.querySelector('.st-n-label').textContent, g ? g.className : '', g ? g.textContent : '']; });");
  eq(navs, [
    ['security', '登录与安全', 'st-n-badge mod', '1'],
    ['sms', '短信', 'st-n-badge bad', '1'],
    ['query', '在线查询与脱敏', 'st-n-badge mod', '1'],
    ['firewall', 'SQL 防火墙', '', ''],
    ['export', '数据导出', 'st-n-badge mod', '1'],
    ['optimizer', 'SQL 优化工具', '', ''],
  ]);
  eq(await qa('#st-nav button.on', 'dataset.group'), ['security']);
  eq(await q('#st-content h2'), '登录与安全');
  has(await q('#st-content .st-group-head p'), '登录锁定、密码与会话');
});
await t('系统配置：设置项按小节排列；数值项显示当前值、默认值、范围、说明与变更记录入口', async () => {
  eq(await stKeys(), ['security.login_lock_attempts', 'security.self_signup']);
  eq(await qa('#st-content .st-sec > h3'), ['登录锁定', '注册与默认权限']);
  const r = await stText('security.login_lock_attempts');
  has(r, '连续失败锁定次数');
  has(r, '已修改');
  has(r, '默认：5');
  has(r, '范围：1 ~ 1000');
  has(r, '连续登录失败达到这个次数后锁定账号。');
  has(r, '变更记录（admin）');
  eq(await ev(`const e = ${stRow('security.login_lock_attempts')}.querySelector('input'); return [e.type, e.value, e.min, e.max, e.placeholder, e.disabled];`), ['number', '3', '1', '1000', '5', false]);
});
await t('系统配置：开关显示开启 / 关闭，没设置过的数值给默认值占位，有专门页面的项带链接', async () => {
  await stGo('query');
  await waitFor("document.querySelector('#st-content h2').textContent === '在线查询与脱敏'", '切换分组');
  eq(await stKeys(), ['query.mask_enabled', 'query.forbid_select_star', 'query.max_execution_seconds']);
  const sw = (k) => ev(`const r = ${stRow(k)}; return [r.querySelector('input[type=checkbox]').checked, r.querySelector('.st-switch span').textContent];`);
  eq(await sw('query.mask_enabled'), [true, '开启']);
  eq(await sw('query.forbid_select_star'), [false, '关闭']);
  eq(await ev(`const e = ${stRow('query.max_execution_seconds')}.querySelector('input'); return [e.value, e.placeholder];`), ['', '60']);
  const num = await stText('query.max_execution_seconds');
  has(num, '默认：60 秒');
  has(num, '范围：1 ~ 3600 秒');
  eq(num.includes('已修改'), false, '没动过的项不带“已修改”');
  has(await stText('query.mask_enabled'), '已修改');
  eq(await ev(`const a = ${stRow('query.mask_enabled')}.querySelector('a'); return [a.getAttribute('href'), a.textContent];`), ['/settings/masking', '前往「数据脱敏」页']);
});
await t('系统配置：密钥类只显示“已设置”和修改 / 清除按钮，解不开的显示标记且不给内容', async () => {
  await stGo('sms');
  await waitFor("document.querySelector('#st-content h2').textContent === '短信'", '切换分组');
  const acts = (k) => ev(`return [...${stRow(k)}.querySelectorAll('button[data-act^="secret"]')].map((b) => b.textContent);`);
  const inputs = (k) => ev(`return ${stRow(k)}.querySelectorAll('input, textarea, select').length;`);
  has(await stText('sms.aliyun_access_key_secret'), '已设置');
  eq(await acts('sms.aliyun_access_key_secret'), ['修改', '清除']);
  eq(await inputs('sms.aliyun_access_key_secret'), 0, '已设置的密钥不回显输入框');
  has(await stText('sms.tencent_secret_key'), '密文解不开');
  eq(await acts('sms.tencent_secret_key'), ['修改']);
  eq(await inputs('sms.tencent_secret_key'), 0);
  eq(await ev("return document.querySelector('#st-nav button[data-group=\"sms\"] .st-n-badge').title;"), '密文解不开');
});
await t('系统配置：页面里不能修改的项标明“只读”和原因，控件禁用，没有任何修改按钮', async () => {
  await stGo('optimizer');
  await waitFor("document.querySelector('#st-content h2').textContent === 'SQL 优化工具'", '切换分组');
  eq(await stKeys(), ['optimizer.soar_path']);
  const r = await stText('optimizer.soar_path');
  has(r, '只读');
  has(r, '页面里不能修改：指向服务器上的程序');
  has(r, 'dbt-admin settings set --force');
  eq(await ev(`const r = ${stRow('optimizer.soar_path')}; return [r.classList.contains('locked'), r.querySelector('input').disabled, r.querySelectorAll('button[data-act]').length];`), [true, true, 0]);
});
await t('系统配置：搜索跨分组并按分组标题归类；没有结果给出提示；清空后回到当前分组', async () => {
  await stSet('st-q', 'select');
  eq(await stKeys(), ['query.forbid_select_star']);
  eq(await qa('#st-content .st-search-group'), ['在线查询与脱敏']);
  eq(await qa('#st-nav button.on', 'dataset.group'), [], '搜索时导航不高亮');
  await stSet('st-q', 'security');
  eq(await stKeys(), ['security.login_lock_attempts', 'security.self_signup']);
  await stSet('st-q', '脱敏 总开关');
  eq(await stKeys(), ['query.mask_enabled']);
  await stSet('st-q', 'zzz-no-such');
  has(await q('#st-content'), '没有符合条件的设置项');
  await stSet('st-q', '');
  eq(await q('#st-content h2'), 'SQL 优化工具');
  eq(await qa('#st-nav button.on', 'dataset.group'), ['optimizer']);
});
await t('系统配置：只看已修改——只留与默认值不同的项，分组里没有时给出提示', async () => {
  await stGo('query');
  await stSet('st-only-mod', true);
  eq(await stKeys(), ['query.mask_enabled']);
  await stGo('firewall');
  eq(await stKeys(), []);
  has(await q('#st-content'), '这个分组里没有已修改的设置');
  await stSet('st-only-mod', false);
  eq(await stKeys(), ['firewall.mode']);
});
await t('系统配置：只浏览不写——页面没有发出任何非 GET 请求，也没有未保存的修改条', async () => {
  const cfg = calls.filter((c) => c.url.startsWith('/api/v1/settings/config'));
  eq(cfg.length > 0, true, '应读取过配置接口');
  eq([...new Set(cfg.map((c) => c.method))], ['GET']);
  eq(await ev("return document.getElementById('st-bar').hidden;"), true);
});

await t('页面无控制台错误（含 CSP 违规）', async () => {
  eq(consoleErrors.filter((e) => !e.includes('/favicon.ico')), []);
});

console.log(`
${pass} 通过，${fail} 失败`);
cleanup();
process.exit(fail ? 1 : 0);
