/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_partman_merge.mjs
 * @time: 2026/10/28
 */
// 分区维护页面（并入外部程序后的新界面）回归测试：静态服务 + 假后端 + 无头 Chrome（CDP），不依赖第三方包。
// 用法：node scripts/frontend-test/run_partman_merge.mjs [chrome路径]
// 环境变量：PM_HTTP_PORT（默认 8909）、PM_CDP_PORT（默认 9245）。结束时只结束自己启动的 Chrome 进程。
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn } from 'node:child_process';
import { startServer } from './partman_fake_backend.mjs';
import { registerTraceTests } from './partman_trace_tests.mjs';

const HTTP = +(process.env.PM_HTTP_PORT || 8909);
const CDP = +(process.env.PM_CDP_PORT || 9245);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const base = `http://127.0.0.1:${HTTP}`;

const server = await startServer(HTTP);
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pm-chrome-'));
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
    // 测试里故意触发的 4xx / 409 与 favicon 404 不算页面错误；CSP 违规（Refused to ...）与脚本异常才算
    if (!/Failed to load resource/.test(d.params.entry.text)) errors.push('log: ' + d.params.entry.text + ' ' + (d.params.entry.url || ''));
  } else if (d.method === 'Runtime.consoleAPICalled' && d.params.type === 'error') {
    errors.push('console: ' + d.params.args.map((a) => a.value || a.description).join(' '));
  }
});
const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id; pend.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
await send('Runtime.enable');
await send('Page.enable');
await send('Log.enable');

async function ev(expr) {
  const r = await send('Runtime.evaluate', { expression: `(async()=>{${expr}})()`, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error('页面异常：' + ((r.exceptionDetails.exception || {}).description || r.exceptionDetails.text));
  return r.result.value;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const KEYS = { Enter: 13, Escape: 27, Tab: 9 };
async function key(k) {
  const base = { key: k, code: k, windowsVirtualKeyCode: KEYS[k] || 0 };
  await send('Input.dispatchKeyEvent', { type: 'rawKeyDown', ...base });
  await send('Input.dispatchKeyEvent', { type: 'keyUp', ...base });
}

let pass = 0, fail = 0;
const failures = [];
async function t(name, fn) {
  try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; failures.push(name); console.log('  FAIL ' + name + '\n       ' + e.message); }
}
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b)} 实得 ${JSON.stringify(a)}`); };
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };
const has = (text, sub, m) => { if (!String(text).includes(sub)) throw new Error(`${m || ''} 应包含 ${JSON.stringify(sub)}，实得 ${JSON.stringify(String(text).slice(0, 300))}`); };
const lacks = (text, sub, m) => { if (String(text).includes(sub)) throw new Error(`${m || ''} 不应包含 ${JSON.stringify(sub)}`); };

const J = JSON.stringify;
const q = (sel, prop = 'textContent') => ev(`const e=document.querySelector(${J(sel)}); return e ? e.${prop} : null;`);
const qa = (sel, prop = 'textContent') => ev(`return [...document.querySelectorAll(${J(sel)})].map(e=>e.${prop});`);
const exists = (sel) => ev(`return !!document.querySelector(${J(sel)});`);
const visible = (sel) => ev(`const e=document.querySelector(${J(sel)}); return !!e && !e.hidden && !!(e.offsetWidth||e.offsetHeight||e.getClientRects().length);`);
const click = (sel) => ev(`const e=document.querySelector(${J(sel)}); if(!e) throw new Error('找不到 '+${J(sel)}); e.click();`);
const setVal = (sel, v) => ev(`const i=document.querySelector(${J(sel)}); i.value=${J(v)}; i.dispatchEvent(new Event('input',{bubbles:true})); i.dispatchEvent(new Event('change',{bubbles:true}));`);
const clickText = (scope, text) => ev(`const b=[...document.querySelectorAll(${J(scope)})].find(e=>e.textContent.trim()===${J(text)}); if(!b) throw new Error('找不到按钮 '+${J(text)}); b.click();`);
async function until(fn, what, ms = 4000) {
  const end = Date.now() + ms;
  let last;
  while (Date.now() < end) {
    try { last = await fn(); if (last) return last; } catch (_) { /* 继续等 */ }
    await sleep(60);
  }
  throw new Error('等待超时：' + what);
}
const reqs = async () => (await fetch(`${base}/__reqs`)).json();
const ctl = (o) => fetch(`${base}/__ctl`, { method: 'POST', body: J(o) });
const reset = () => fetch(`${base}/__reset`);
async function reqsOf(pathPart, method) {
  return (await reqs()).filter((r) => r.path.includes(pathPart) && (!method || r.method === method));
}
async function noHScroll(label) {
  const w = await ev('return [document.documentElement.scrollWidth, window.innerWidth];');
  truthy(w[0] <= w[1] + 1, `${label}：页面出现横向滚动 ${w[0]} > ${w[1]}`);
}

async function load(url = '/partman', ctlInit) {
  await reset();
  if (ctlInit) await ctl(ctlInit);
  await send('Page.navigate', { url: base + url });
  await until(() => ev('return document.readyState==="complete"'), '页面加载');
  await sleep(250);
}
async function openPage(ctlInit) {
  await load('/partman', ctlInit);
  await until(() => ev("return document.querySelectorAll('#pm-rows tr').length >= 4"), '列表渲染');
  await until(() => ev("return !document.querySelector('#pm-rows').textContent.includes('检测中')"), '状态加载');
}
const rowOf = (name) => `([...document.querySelectorAll('#pm-rows tr')].find(r=>r.textContent.includes(${J(name)})))`;
const rowBtn = (name, text) => ev(`const r=${rowOf(name)}; const b=[...r.querySelectorAll('button')].find(x=>x.textContent.trim()===${J(text)}); if(!b) throw new Error('行内没有按钮 '+${J(text)}); b.click();`);
const CF_INPUT = '.cf-dialog input[type="text"], .cf-dialog .cf-input';
const okBtn = '.cf-foot .cf-btn:last-child';
const cancelBtn = '.cf-foot .cf-btn:first-child';

console.log(`分区维护界面测试（${base}/partman）`);

// ---------------- 列表 ----------------
await openPage();
await t('列表：4 行、无控制台错误 / CSP 违规', async () => {
  eq(await ev("return document.querySelectorAll('#pm-rows tr').length"), 4);
  eq(errors, [], '控制台错误');
});
await t('列表：Doris 实例带标记，不支持的类型提示支持范围', async () => {
  has(await ev(`return ${rowOf('ods_event')}.textContent`), 'SelectDB');
  const un = await ev(`return ${rowOf('x')}.textContent`);
  has(un, '暂不支持（mssql）');
  lacks(un, '外部程序');
});
await t('列表：未分区的状态显示“未分区 可初始化”，KPI 单列未分区', async () => {
  has(await ev(`return ${rowOf('access_log')}.querySelector('.pm-stat').textContent`), '未分区');
  has(await ev(`return ${rowOf('access_log')}.querySelector('.pm-stat').textContent`), '可初始化');
  const k = await ev("return [...document.querySelectorAll('#pm-kpis div')].map(d=>d.textContent)");
  truthy(k.some((x) => x.includes('未分区') && x.startsWith('1')), 'KPI 未分区=1 ' + J(k));
  eq(k.length, 6);
});
await t('列表：定时维护横幅不再提示外部程序', async () => {
  const b = await q('#pm-auto');
  has(b, '每 180 分钟一轮');
  lacks(b, 'partman daemon');
});

// ---------------- 时区 ----------------
await t('时区：输入框 + 常用项，点常用项填入', async () => {
  await click('#pm-new');
  truthy(await visible('#pm-edit'), '编辑弹窗');
  eq(await q('#pe-tz', 'tagName'), 'INPUT');
  const chips = await qa('#pe-tz-chips button');
  has(chips.join('|'), '跟随目标库');
  await clickText('#pe-tz-chips button', 'UTC');
  eq(await q('#pe-tz', 'value'), 'UTC');
  eq(await ev(`return [...document.querySelectorAll('#pe-tz-chips button')].filter(b=>b.getAttribute('aria-pressed')==='true').map(b=>b.dataset.tz)`), ['UTC'], '选中的常用项');
  await clickText('#pe-tz-chips button', '跟随目标库');
  eq(await q('#pe-tz', 'value'), '');
  // 留空 = 跟随目标库：现在由输入框占位文字与“跟随目标库”常用项表达，说明行不再重复
  has(await q('#pe-tz', 'placeholder'), '跟随目标库');
  eq(await q('#pe-tz-note'), '', '留空时说明行为空');
  eq(await ev(`return [...document.querySelectorAll('#pe-tz-chips button')].filter(b=>b.getAttribute('aria-pressed')==='true').map(b=>b.dataset.tz)`), [''], '选中“跟随目标库”');
});
await t('时区：固定偏移不提示 / IANA 夏令时提示建议固定偏移 / 超长提示', async () => {
  for (const v of ['UTC+8', '+08:00', 'GMT+08:00']) {
    await setVal('#pe-tz', v);
    eq(await q('#pe-tz-note'), '', `${v} 是固定偏移，不应有提示`);
    eq(await q('#pe-tz-note', 'className'), 'muted small');
  }
  await setVal('#pe-tz', 'America/New_York');
  has(await q('#pe-tz-note'), '夏令时');
  has(await q('#pe-tz-note'), '固定偏移', '提示里建议改用固定偏移');
  eq(await q('#pe-tz-note', 'className'), 'pm-tz-warn small');
  await setVal('#pe-tz', 'America/Argentina/Buenos_Aires');
  has(await q('#pe-tz-note'), '最多 20 个字符');
  has(await q('#pe-tz-note'), '无法保存');
  eq(await q('#pe-tz-note', 'className'), 'pm-tz-warn small');
  await setVal('#pe-tz', 'Asia/Shanghai');
  eq(await q('#pe-tz-note'), '', '无夏令时的 IANA 名字不提示');
  eq(await q('#pe-tz-note', 'className'), 'muted small');
});
await t('时区：datalist 含常用 IANA 名字；空值随 draft 提交', async () => {
  const opts = await qa('#pe-tzs option', 'value');
  truthy(opts.includes('America/New_York') && opts.includes('GMT+08:00'), 'datalist ' + opts.length);
  await setVal('#pe-inst', '1');
  await setVal('#pe-db', 'order_db');
  await setVal('#pe-tb', 'order_event');
  await setVal('#pe-col', 'created_at');
  await setVal('#pe-tz', '');
  await click('#pe-preview');
  await until(async () => (await reqsOf('/preview')).length > 0, '预览请求');
  const r = (await reqsOf('/preview')).pop();
  eq(r.body.draft.timezone, '');
  await until(() => exists('#pe-plan .pm-stmt'), '预览结果');
  const plan = await q('#pe-plan');
  has(plan, '夏令时');
  has(plan, 'Asia/Shanghai');
});
await t('保存配置：服务端返回的 warnings 显示在页面上', async () => {
  await ctl({ saveWarn: true });
  await click('#pe-save');
  await until(() => visible('#pm-msg .alert.warn'), '保存警告');
  has(await q('#pm-msg'), '已保存');
  has(await q('#pm-msg'), '夏令时');
  truthy(!(await visible('#pm-edit')), '弹窗已关');
});

// ---------------- 初始化 ----------------
await openPage();
await t('初始化：未分区表预览显示初始化面板、摘要与前置检查', async () => {
  await rowBtn('access_log', '预览 / 执行');
  await until(() => visible('#pr-init'), '初始化面板');
  const plan = await q('#pr-plan');
  has(plan, '起点');
  has(plan, '3 个：p20261010 ~ p20261012');
  has(plan, '前置检查');
  has(plan, '存储引擎 InnoDB 支持分区');
  has(plan, '初始化');
  has(plan, 'UTC+08:00');
  eq(await q('#pr-go'), '初始化为分区表…');
  truthy(await visible('#pr-go'), '按钮可见');
  eq(await ev("return document.getElementById('pr-go').disabled"), false);
  const body = (await reqsOf('/preview')).pop().body;
  eq(body, { id: 2 });
});
await t('初始化：默认从当前窗口；切换起点会重新预览并带 init 参数', async () => {
  await ev("document.querySelector('input[name=pr-start][value=earliest]').click()");
  await until(async () => (await reqsOf('/preview')).length >= 2, '重新预览');
  const b = (await reqsOf('/preview')).pop().body;
  eq(b, { id: 2, init: { start_mode: 'earliest', start_date: '' } });
  await until(() => ev("return document.getElementById('pr-plan').textContent.includes('40 个')"), '40 个分区');
  has(await q('#pr-plan'), '2025-09-01 08:30:12', '最早数据时间');
});
await t('初始化：读不到最早数据 -> 错误提示，面板保留，可改选', async () => {
  await ctl({ earliestFail: true });
  await ev("document.querySelector('input[name=pr-start][value=current]').click()");
  await until(() => ev("return document.getElementById('pr-plan').textContent.includes('3 个')"), '回到当前窗口');
  await ev("document.querySelector('input[name=pr-start][value=earliest]').click()");
  await until(() => visible('#pr-msg .alert.err'), '错误提示');
  has(await q('#pr-msg'), '指定起始日期');
  truthy(await visible('#pr-init'), '面板仍在');
  truthy(!(await visible('#pr-go')), '执行按钮隐藏');
  await ctl({ earliestFail: false });
});
await t('初始化：指定起始日期——空日期不发请求，填了才预览', async () => {
  const n0 = (await reqsOf('/preview')).length;
  await ev("document.querySelector('input[name=pr-start][value=date]').click()");
  await until(() => visible('#pr-start-date'), '日期输入框');
  has(await q('#pr-msg'), '请填写起始日期');
  eq((await reqsOf('/preview')).length, n0, '空日期不应请求');
  await setVal('#pr-start-date', '2026-09-29');
  await click('#pr-start-apply');
  await until(async () => (await reqsOf('/preview')).length > n0, '日期预览');
  eq((await reqsOf('/preview')).pop().body, { id: 2, init: { start_mode: 'date', start_date: '2026-09-29' } });
  await until(() => ev("return document.getElementById('pr-plan').textContent.includes('12 个')"), '12 个分区');
});
await t('初始化：确认框——高危、列出语句、必须键入库.表；确认后请求带 confirm_init', async () => {
  await click('#pr-go');
  await until(() => exists('.cf-dialog'), '确认框');
  has(await q('.cf-title'), '初始化为分区表');
  has(await q('.cf-dialog'), 'PARTITION BY RANGE COLUMNS');
  has(await q('.cf-dialog'), 'log.access_log');
  has(await q('.cf-dialog'), 'partman.run');
  eq(await ev(`return document.querySelector(${J(okBtn)}).disabled`), true, '未键入时禁用');
  await ev(`const i=document.querySelector('.cf-dialog input'); i.focus(); i.value='log.wrong'; i.dispatchEvent(new Event('input',{bubbles:true}));`);
  eq(await ev(`return document.querySelector(${J(okBtn)}).disabled`), true);
  await ev(`const i=document.querySelector('.cf-dialog input'); i.value='log.access_log'; i.dispatchEvent(new Event('input',{bubbles:true}));`);
  eq(await ev(`return document.querySelector(${J(okBtn)}).disabled`), false);
  await click(okBtn);
  await until(async () => (await reqsOf('/run')).length > 0, 'run 请求');
  const b = (await reqsOf('/run')).pop().body;
  eq(b.confirm_init, { table: 'log.access_log', partition_count: 12, first_partition: 'p20260929', last_partition: 'p20261021' });
  eq(b.init, { start_mode: 'date', start_date: '2026-09-29' });
  eq(Object.keys(b).sort(), ['confirm_init', 'init'], '请求体不含 SQL 或其它字段');
  await until(() => ev("return !document.querySelector('.cf-dialog')"), '确认框关闭');
  await until(() => ev("return document.getElementById('pr-plan').textContent.includes('成功')"), '结果表');
  has(await q('#pr-msg'), '执行完成');
  truthy(!(await visible('#pr-init')), '面板隐藏');
  truthy(!(await visible('#pr-go')), '按钮隐藏');
});
await t('初始化后：列表状态不再是未分区', async () => {
  await click('#pr-close');
  await until(() => ev(`return !${rowOf('access_log')}.querySelector('.pm-stat').textContent.includes('未分区')`), '状态刷新');
});

await openPage();
await t('初始化：确认值与重算不一致 -> 弹窗内报错并更新页面，不会自动用新值重试', async () => {
  await ctl({ mismatch: true });
  await rowBtn('access_log', '预览 / 执行');
  await until(() => visible('#pr-init'), '面板');
  await click('#pr-go');
  await until(() => exists('.cf-dialog'), '确认框');
  await ev(`const i=document.querySelector('.cf-dialog input'); i.value='log.access_log'; i.dispatchEvent(new Event('input',{bubbles:true}));`);
  await click(okBtn);
  await until(() => ev("const e=document.querySelector('.cf-dialog [role=alert]'); return !!e && e.textContent.trim().length>0"), '弹窗内错误');
  has(await q('.cf-dialog [role="alert"]'), '分区个数与确认值不一致');
  await until(() => ev("return document.getElementById('pr-plan').textContent.includes('4 个')"), '页面已更新为新计划');
  const n = (await reqsOf('/run')).length;
  await click(okBtn); // 再点一次：不应发请求
  await sleep(300);
  eq((await reqsOf('/run')).length, n, '不应再发 run');
  has(await q('.cf-dialog [role="alert"]'), '请取消本窗口');
  await click(cancelBtn);
  await until(() => ev("return !document.querySelector('.cf-dialog')"), '关闭');
  await click('#pr-close');
});
await t('初始化：前置检查有“不满足”项 -> 展示原因，没有执行按钮', async () => {
  await ctl({ checkError: true });
  await rowBtn('access_log', '预览 / 执行');
  await until(() => visible('#pr-init'), '面板');
  const plan = await q('#pr-plan');
  has(plan, '不满足');
  has(plan, '表上有外键');
  has(plan, '前置检查未通过');
  truthy(!(await visible('#pr-go')), '无执行按钮');
  await click('#pr-close');
  await ctl({ checkError: false });
});
await t('初始化：没有 delete_partmanconfig 时只能预览并说明原因', async () => {
  await openPage({ noDelete: true });
  await rowBtn('access_log', '预览 / 执行');
  await until(() => visible('#pr-init'), '面板');
  truthy(await visible('#pr-init-perm'), '权限说明');
  // 页面只说明“没有初始化权限，只能预览”；具体权限名由服务端 403 forbidden 的文案给出（见下方 forbidden 用例）
  has(await q('#pr-init-perm'), '没有初始化权限');
  has(await q('#pr-init-perm'), '只能预览');
  eq(await ev("return document.getElementById('pr-go').disabled"), true);
  await click('#pr-close');
});
await t('初始化：ALTER 失败 -> 结果表显示失败原因', async () => {
  await openPage();
  await ctl({ runFail: true });
  await rowBtn('access_log', '预览 / 执行');
  await until(() => visible('#pr-go'), '按钮');
  await click('#pr-go');
  await until(() => exists('.cf-dialog'), '确认框');
  await ev(`const i=document.querySelector('.cf-dialog input'); i.value='log.access_log'; i.dispatchEvent(new Event('input',{bubbles:true}));`);
  await click(okBtn);
  await until(() => ev("return document.getElementById('pr-plan').textContent.includes('Error 1503')"), '失败原因');
  has(await q('#pr-msg'), '执行失败');
  await click('#pr-close');
  await ctl({ runFail: false });
});

// ---------------- 其它预览 / 执行 ----------------
await openPage();
await t('含 DROP 的计划仍是原有二次确认：键入库.表才可执行', async () => {
  await rowBtn('order_event', '预览 / 执行');
  await until(() => visible('#pr-danger'), '危险区');
  truthy(!(await visible('#pr-init')), '没有初始化面板');
  eq(await ev("return document.getElementById('pr-go').disabled"), true);
  await setVal('#pr-confirm', 'order_db.order_event');
  eq(await ev("return document.getElementById('pr-go').disabled"), false);
  eq(await q('#pr-go'), '确认执行');
  await click('#pr-close');
});
await t('Doris 配置：预览显示方言语句与“未知上界不删除”提示，没有初始化面板', async () => {
  await rowBtn('ods_event', '预览 / 执行');
  await until(() => exists('#pr-plan .pm-stmt'), '预览');
  has(await q('#pr-plan'), 'VALUES [(');
  has(await q('#pr-plan'), '上界读不到');
  truthy(!(await visible('#pr-init')), '无初始化面板');
  await click('#pr-close');
});

// ---------------- 契约错误码 ----------------
for (const [code, message] of [
  ['max_partition', 'dw.ods_event 含 MAXVALUE 分区，SelectDB / Doris 不能在它之后追加分区，请先手工调整分区'],
  ['name_conflict', '要创建的分区 p20261008 与现有分区重名，但边界不同（不连续），请先检查表的分区是否按本配置的周期建立'],
  ['not_partitioned', 'SelectDB / Doris 不支持自动初始化未分区表，请先把 dw.ods_event 建为 RANGE 分区表'],
  ['timezone_unknown', '配置的时区为空，需要读取目标库的会话时区，但没有读到；请在配置里明确填写时区'],
]) {
  await t(`预览 409 ${code}：原文显示在弹窗内，没有执行按钮 / 初始化面板`, async () => {
    await openPage();
    await ctl({ previewErr: { status: 409, code, message } });
    await rowBtn('ods_event', '预览 / 执行');
    await until(() => visible('#pr-msg .alert.err'), '错误提示');
    has(await q('#pr-msg'), message);
    truthy(!(await visible('#pr-go')), '无执行按钮');
    truthy(!(await visible('#pr-init')), '无初始化面板');
    await click('#pr-close');
  });
}
await t('编辑弹窗预览遇到 409 时错误显示在编辑弹窗内', async () => {
  await openPage();
  await ctl({ previewErr: { status: 409, code: 'name_conflict', message: '要创建的分区 p1 与现有分区重名，但边界不同' } });
  await click('#pm-new');
  await setVal('#pe-db', 'order_db');
  await setVal('#pe-tb', 'order_event');
  await setVal('#pe-col', 'created_at');
  await click('#pe-preview');
  await until(() => visible('#pm-edit-msg .alert.err'), '错误');
  has(await q('#pm-edit-msg'), '重名');
  await click('#pe-cancel');
});
for (const [code, status, message] of [
  ['bad_init_option', 400, '起始日期 2026-12-01 晚于当前窗口（2026-10-10 00:00:00），初始化需要从当前窗口或更早开始'],
  ['disabled', 409, '该配置的维护开关是关闭的，请先在编辑里开启'],
  ['forbidden', 403, '没有权限初始化为分区表（重建整张表）（需要 delete_partmanconfig）'],
  ['earliest_unavailable', 409, '读取最早数据时间失败或超时（context deadline exceeded），请改用“指定起始日期”'],
]) {
  await t(`初始化执行返回 ${code}：错误留在确认框内，不关闭、不改页面`, async () => {
    await openPage();
    await rowBtn('access_log', '预览 / 执行');
    await until(() => visible('#pr-go'), '按钮');
    await ctl({ runErr: { status, code, message } });
    await click('#pr-go');
    await until(() => exists('.cf-dialog'), '确认框');
    await ev(`const i=document.querySelector('.cf-dialog input'); i.value='log.access_log'; i.dispatchEvent(new Event('input',{bubbles:true}));`);
    await click(okBtn);
    await until(() => ev("const e=document.querySelector('.cf-dialog [role=alert]'); return !!e && e.textContent.includes(" + J(message.slice(0, 12)) + ")"), '错误文案');
    truthy(await exists('.cf-dialog'), '确认框仍在');
    truthy(await visible('#pr-init'), '初始化面板仍在');
    await click(cancelBtn);
    await until(() => ev("return !document.querySelector('.cf-dialog')"), '关闭');
    await click('#pr-close');
  });
}

// ---------------- 推送设置 ----------------
await openPage();
await t('推送设置：入口、列表、脱敏地址、已设置标志、全局时机说明', async () => {
  truthy(await visible('#pm-hooks'), '入口按钮');
  await click('#pm-hooks');
  await until(() => exists('#ph-list .pm-hook'), '通道列表');
  eq((await qa('#ph-list .pm-hook')).length, 2);
  const txt = await q('#ph-list');
  has(txt, 'access_token=****');
  has(txt, '已设置');
  has(txt, '未设置');
  has(await q('#ph-rule'), '全局推送时机');
  has(await q('#ph-rule'), '执行了语句或失败后推送');
});
await t('推送设置：新增——地址不在白名单报错留在弹窗；成功后列表出现，密钥不回显', async () => {
  await click('#ph-new');
  truthy(await visible('#pm-hook-edit'), '编辑弹窗');
  await setVal('#phe-type', 'feishu');
  await setVal('#phe-url', 'https://evil.example.com/hook/abc');
  await click('#phe-save');
  await until(() => visible('#phe-msg .alert.err'), '错误');
  // 允许的域名不再有固定的说明行，改由服务端拒绝时的文案列出（internal/partman/push.go checkWebhookURL）
  has(await q('#phe-msg'), '不在');
  has(await q('#phe-msg'), '允许的范围');
  has(await q('#phe-msg'), 'open.feishu.cn');
  eq((await reqsOf('/webhooks', 'POST')).pop().body.type, 'feishu');
  truthy(await visible('#pm-hook-edit'), '弹窗保留');
  await setVal('#phe-url', 'https://open.feishu.cn/open-apis/bot/v2/hook/abcdef123456');
  await setVal('#phe-keyword', '分区维护');
  await setVal('#phe-secret', 'TOPSECRET-SIGN-KEY');
  await setVal('#phe-mention', '13800001111, @all  ou_abc');
  await click('#phe-save');
  await until(async () => !(await visible('#pm-hook-edit')), '弹窗关闭');
  await until(() => ev("return document.querySelectorAll('#ph-list .pm-hook').length===3"), '新通道出现');
  const b = (await reqsOf('/webhooks', 'POST')).pop().body;
  eq(b.mention, ['13800001111', '@all', 'ou_abc']);
  eq(b.secret, 'TOPSECRET-SIGN-KEY');
  const html = await ev('return document.documentElement.outerHTML');
  lacks(html, 'TOPSECRET-SIGN-KEY', '页面里不应出现密钥');
  lacks(html, 'abcdef123456', '页面里不应出现完整 webhook 地址');
  has(await q('#ph-list'), 'hook/****');
});
await t('推送设置：编辑——地址与密钥输入框为空，留空保存 = 不改；可勾选清空密钥', async () => {
  await ev("document.querySelectorAll('#ph-list .pm-hook')[0].querySelectorAll('button')[0].click()");
  await until(() => visible('#pm-hook-edit'), '编辑弹窗');
  eq(await q('#phe-url', 'value'), '');
  eq(await q('#phe-secret', 'value'), '');
  eq(await q('#phe-secret', 'type'), 'password');
  has(await q('#phe-url', 'placeholder'), 'access_token=****');
  has(await q('#phe-secret', 'placeholder'), '已设置');
  truthy(await visible('#phe-clear-box'), '清空复选框');
  await click('#phe-save');
  await until(async () => !(await visible('#pm-hook-edit')), '关闭');
  const b = (await reqsOf('/webhooks/1', 'PUT')).pop().body;
  eq([b.webhook_url, b.secret, b.clear_secret], ['', '', undefined]);
  await ev("document.querySelectorAll('#ph-list .pm-hook')[0].querySelectorAll('button')[0].click()");
  await until(() => visible('#pm-hook-edit'), '再次编辑');
  await click('#phe-clear-secret');
  await click('#phe-save');
  await until(() => ev("return document.querySelectorAll('#ph-list .pm-hook')[0].textContent.includes('未设置')"), '密钥被清空');
  eq((await reqsOf('/webhooks/1', 'PUT')).pop().body.clear_secret, true);
});
await t('推送设置：启停、测试发送（单个 / 全部已启用）显示每个通道结果', async () => {
  await ev("document.querySelectorAll('#ph-list .pm-hook')[1].querySelectorAll('button')[1].click()");
  await until(() => ev("return document.querySelectorAll('#ph-list .pm-hook')[1].textContent.includes('停用')"), '已启用');
  eq((await reqsOf('/webhooks/2/enable')).pop().body, { enable: true });
  await click('#ph-test-all');
  await until(() => exists('#ph-results .pm-test-results li'), '测试结果');
  const items = await qa('#ph-results li');
  truthy(items.length >= 2, '多个通道');
  truthy(items.some((x) => x.includes('发送成功')) && items.some((x) => x.includes('连接超时')), '成功与失败都显示 ' + J(items));
  eq((await reqsOf('/webhooks/test')).pop().body, { id: 0 });
  has(await q('#ph-results'), '系统记录 #88');
  eq(await q('#ph-results a', "getAttribute('href')"), '/partman/logs?source=system');
});
await t('推送设置：关键词 / @ 对象里的 HTML 只当文本显示', async () => {
  await click('#ph-new');
  await setVal('#phe-url', 'https://hooks.slack.com/services/T000/B000/xyz123');
  await setVal('#phe-type', 'slack');
  await setVal('#phe-keyword', '<img src=x onerror="window.__x2=1">');
  await setVal('#phe-mention', '<b>boss</b>');
  await click('#phe-save');
  await until(() => ev("return document.querySelectorAll('#ph-list .pm-hook').length===4"), '新通道');
  has(await q('#ph-list'), '<img src=x onerror="window.__x2=1">');
  eq(await ev("return document.querySelectorAll('#ph-list img').length"), 0, '不应生成 img');
  eq(await ev("return document.querySelectorAll('#ph-list b').length"), 4, '只有每张卡片标题里的 b');
  eq(await ev('return window.__x2 === undefined'), true);
  await ev("document.querySelectorAll('#ph-list .pm-hook')[3].querySelectorAll('button')[3].click()");
  await until(() => exists('.cf-dialog'), '确认框');
  await click(okBtn);
  await until(() => ev("return document.querySelectorAll('#ph-list .pm-hook').length===3"), '已删除');
});
await t('推送设置：删除走统一确认框', async () => {
  await ev("document.querySelectorAll('#ph-list .pm-hook')[2].querySelectorAll('button')[3].click()");
  await until(() => exists('.cf-dialog'), '确认框');
  has(await q('.cf-title'), '删除推送通道');
  await click(okBtn);
  await until(() => ev("return document.querySelectorAll('#ph-list .pm-hook').length===2"), '已删除');
  eq((await reqsOf('/webhooks/3', 'DELETE')).length, 1);
});
await t('推送设置：Esc 只关最上层弹窗', async () => {
  await click('#ph-new');
  await until(() => visible('#pm-hook-edit'), '编辑弹窗');
  await key('Escape');
  await until(async () => !(await visible('#pm-hook-edit')), '编辑弹窗关闭');
  truthy(await visible('#pm-hook'), '设置弹窗仍在');
  await key('Escape');
  await until(async () => !(await visible('#pm-hook')), '设置弹窗关闭');
});
await t('没有 change_partmanconfig 时不显示推送设置入口', async () => {
  await openPage({ noWebhook: true });
  truthy(!(await visible('#pm-hooks')), '入口隐藏');
});

// ---------------- 手机 / 平板 ----------------
for (const [label, w, h] of [['手机 375', 375, 812], ['平板 768', 768, 1024]]) {
  await send('Emulation.setDeviceMetricsOverride', { width: w, height: h, deviceScaleFactor: 1, mobile: w < 700 });
  await openPage();
  await t(`${label}：列表页无页面级横向滚动`, async () => { await noHScroll('列表'); });
  await t(`${label}：编辑 / 初始化 / 推送设置弹窗无页面级横向滚动，按钮在视口内`, async () => {
    await click('#pm-new');
    await noHScroll('编辑弹窗');
    await setVal('#pe-tz', 'America/Argentina/Buenos_Aires');
    await noHScroll('编辑弹窗-时区提示');
    await click('#pe-cancel');
    await rowBtn('access_log', '预览 / 执行');
    await until(() => visible('#pr-init'), '面板');
    await noHScroll('初始化弹窗');
    const goBox = await ev("const r=document.getElementById('pr-go').getBoundingClientRect(); return [r.left, r.right, innerWidth];");
    truthy(goBox[0] >= 0 && goBox[1] <= goBox[2] + 1, '执行按钮在视口内 ' + J(goBox));
    await click('#pr-go');
    await until(() => exists('.cf-dialog'), '确认框');
    await noHScroll('确认框');
    await click(cancelBtn);
    await click('#pr-close');
    await click('#pm-hooks');
    await until(() => exists('#ph-list .pm-hook'), '通道');
    await noHScroll('推送设置');
    await click('#ph-new');
    await noHScroll('通道编辑');
    await click('#phe-cancel');
    await click('#ph-close');
  });
}
await send('Emulation.clearDeviceMetricsOverride');

// ---------------- 运行输出（trace） ----------------
await registerTraceTests({ t, eq, truthy, has, lacks, ev, q, qa, exists, visible, click, setVal, until, sleep, ctl, reqsOf, reqs, load, openPage, rowBtn, send, noHScroll, base, errors, key });

await t('全程无控制台错误 / CSP 违规', async () => { eq(errors, [], '控制台错误'); });

console.log(`\n通过 ${pass}，失败 ${fail}`);
if (fail) console.log('失败项：\n  ' + failures.join('\n  '));
cleanup();
process.exit(fail ? 1 : 0);
