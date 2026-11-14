/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_notify_wecom.mjs
 * @time: 2026/10/28
 */
// 通知渠道页（企业微信渠道）浏览器回归：起内存演示服务（cmd/demo，假数据，不连真实库）+ 进程内假“企业微信机器人” + 无头 Chrome（CDP），不依赖第三方包。
// 覆盖：类型下拉里的“企业微信”、选中后不显示签名密钥（企业微信没有加签）、@ 规则里原生标识的提示、保存 / 列表 / 编辑、
// 测试发送收到的文本消息（mentioned_list / mentioned_mobile_list）、预览（平台用户不能真正 @ 的说明）、
// 从钉钉（带签名密钥）改成企业微信时旧密钥被清掉、对端返回 errcode ≠ 0 时的错误提示、严格 CSP 下控制台零报错。
// 用法：node scripts/frontend-test/run_notify_wecom.mjs [chrome路径]
// 环境变量：NW_APP_PORT（演示服务，默认 8910）、NW_CDP_PORT（Chrome 调试端口，默认 9246）、DEMO_EXE（已编译的 cmd/demo；不给则先 go build 到临时目录，需要 GOTOOLCHAIN=local）。
// 结束时只按 PID 结束自己启动的进程。
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '../..');
const APP = +(process.env.NW_APP_PORT || 8910);
const CDP = +(process.env.NW_CDP_PORT || 9246);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'nw-'));

// ---- 假企业微信机器人：记录收到的请求；failNext 为真时返回业务错误 ----
const hooks = [];
let failNext = false;
const robot = http.createServer((req, res) => {
  let b = '';
  req.on('data', (c) => { b += c; });
  req.on('end', () => {
    let body = null;
    try { body = JSON.parse(b); } catch (_) { /* 非 JSON */ }
    hooks.push({ url: req.url, body });
    res.writeHead(200, { 'Content-Type': 'application/json' });
    if (failNext) { failNext = false; res.end('{"errcode":93000,"errmsg":"invalid webhook url"}'); } else res.end('{"errcode":0,"errmsg":"ok"}');
  });
});
await new Promise((r) => robot.listen(0, '127.0.0.1', r));
const HOOK = `http://127.0.0.1:${robot.address().port}/cgi-bin/webhook/send?key=KEY-E2E-123`;

// ---- 演示服务 ----
let exe = process.env.DEMO_EXE;
if (!exe) {
  exe = path.join(tmp, 'demo.exe');
  const b = spawnSync('go', ['build', '-o', exe, './cmd/demo'], { cwd: root, env: { ...process.env, GOTOOLCHAIN: 'local' }, encoding: 'utf8' });
  if (b.status !== 0) { console.log('编译 cmd/demo 失败：\n' + b.stderr); process.exit(2); }
}
const app = spawn(exe, [], { env: { ...process.env, LISTEN: `127.0.0.1:${APP}` }, stdio: 'ignore' });
const profile = fs.mkdtempSync(path.join(tmp, 'chrome-'));
const chrome = spawn(chromePath, ['--headless=new', `--remote-debugging-port=${CDP}`, `--user-data-dir=${profile}`, '--no-first-run', '--disable-gpu', 'about:blank'], { stdio: 'ignore' });
const cleanup = () => {
  for (const p of [chrome, app]) { try { p.kill(); } catch (_) { /* 已退出 */ } }
  robot.close();
  setTimeout(() => { try { fs.rmSync(tmp, { recursive: true, force: true }); } catch (_) { /* 占用中则留给系统清理 */ } }, 1500).unref();
};
process.on('exit', cleanup);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function getWs() {
  for (let i = 0; i < 80; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${CDP}/json`)).json();
      const p = list.find((x) => x.type === 'page');
      if (p) return p.webSocketDebuggerUrl;
    } catch (_) { /* 等 Chrome 起来 */ }
    await sleep(250);
  }
  throw new Error('Chrome 未启动');
}
for (let i = 0; i < 60; i++) { try { if ((await fetch(`http://127.0.0.1:${APP}/login`)).ok) break; } catch (_) { /* 等服务起来 */ } await sleep(250); }

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
  } else if (d.method === 'Runtime.consoleAPICalled' && d.params.type === 'error') {
    consoleErrors.push('console.error ' + d.params.args.map((a) => a.value || a.description).join(' '));
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
let pass = 0, fail = 0;
async function t(name, fn) {
  try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; console.log('  FAIL ' + name + '\n       ' + e.message); }
}
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b)} 实得 ${JSON.stringify(a)}`); };
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };
const q = (sel, prop = 'textContent') => ev(`const e=document.querySelector(${JSON.stringify(sel)}); return e ? e.${prop} : null;`);
const visible = (sel) => ev(`const e=document.querySelector(${JSON.stringify(sel)}); return !!e && !e.hidden && getComputedStyle(e).display !== 'none' && e.getClientRects().length > 0;`);
const click = (sel) => ev(`document.querySelector(${JSON.stringify(sel)}).click();`);
const setVal = (sel, v) => ev(`const i=document.querySelector(${JSON.stringify(sel)}); i.value=${JSON.stringify(v)}; i.dispatchEvent(new Event('input',{bubbles:true})); i.dispatchEvent(new Event('change',{bubbles:true}));`);
const waitFor = async (expr, what, ms = 8000) => {
  const end = Date.now() + ms;
  while (Date.now() < end) { if (await ev(expr).catch(() => false)) return; await sleep(100); }
  throw new Error('等待超时：' + what);
};
const okBtn = '.cf-foot .cf-btn:last-child';
const rowText = () => q('#nc-rows');
// 加一条 @ 规则：目标、值（有输入框的才填）
const addRule = async (target, value = '') => {
  await click('#nc-add-rule');
  await ev(`const rows = document.querySelectorAll('#nc-rules .rule-row'); const row = rows[rows.length - 1]; const s = row.querySelector('select');
    s.value = ${JSON.stringify(target)}; s.dispatchEvent(new Event('change', { bubbles: true }));
    ${value ? `const i = row.querySelector('.rule-value input'); i.value = ${JSON.stringify(value)};` : ''}`);
};
const confirmSave = async () => {
  await click('#nc-save');
  await waitFor("return !!document.querySelector('.cf-dialog')", '确认弹窗');
  await click(okBtn);
  await waitFor("return !document.querySelector('.cf-dialog')", '弹窗关闭');
};
const apiList = () => ev("const r = await fetch('/api/v1/settings/notify'); return await r.json();");

// ---- 登录 ----
await send('Page.navigate', { url: `http://127.0.0.1:${APP}/login` });
await waitFor("return !!document.querySelector('input[name=username]')", '登录页');
await ev("document.querySelector('input[name=username]').value='admin'; document.querySelector('input[name=password]').value='admin123'; document.querySelector('form').submit();");
await sleep(800);
await send('Page.navigate', { url: `http://127.0.0.1:${APP}/settings/notify` });
await waitFor("return document.querySelectorAll('#nc-rows tr').length > 0 && !document.querySelector('#nc-rows').textContent.includes('加载中')", '渠道页加载');
console.log(`通知渠道页（企业微信渠道）浏览器测试：演示服务 :${APP}，假企业微信机器人 :${robot.address().port}`);

await t('类型下拉里有“企业微信”（排在钉钉后面）；选中后不显示签名密钥，Webhook 地址的占位是企业微信的域名', async () => {
  await click('#nc-new');
  eq(await ev("return [...document.querySelectorAll('#nc-type option')].map(o=>o.textContent).join(',')"), '钉钉,企业微信,飞书,Lark,Slack,Telegram,邮件');
  eq(await visible('#nc-secret-wrap'), true, '钉钉有签名密钥');
  await setVal('#nc-secret', 'typed-before-switch');
  await setVal('#nc-type', 'wecom');
  eq(await visible('#nc-secret-wrap'), false, '企业微信没有加签：签名密钥输入框隐藏');
  eq(await visible('#nc-clear-wrap'), false);
  eq(await q('#nc-secret', 'value'), '', '切换类型时清掉已经填的密钥');
  eq(await visible('#nc-url-wrap'), true);
  truthy((await q('#nc-url', 'placeholder')).startsWith('https://qyapi.weixin.qq.com/'), 'Webhook 占位：' + await q('#nc-url', 'placeholder'));
  eq(await visible('#nc-email'), false);
  // 切回钉钉：密钥输入框又出现
  await setVal('#nc-type', 'dingtalk');
  eq(await visible('#nc-secret-wrap'), true);
  await setVal('#nc-type', 'wecom');
});

await t('@ 规则：渠道原生标识的占位是“成员 userid / 手机号”，“所有人”有提示', async () => {
  await addRule('raw');
  eq(await q('#nc-rules .rule-value input', 'placeholder'), '成员 userid / 手机号');
  await ev("document.querySelector('#nc-rules .rule-row .btn.ghost').click();");
  await addRule('all');
  truthy((await q('#nc-rules .rule-value')).includes('会 @ 所有人'), '所有人');
  await ev("document.querySelector('#nc-rules .rule-row .btn.ghost').click();");
  eq(await ev("return document.querySelectorAll('#nc-rules .rule-row').length"), 0);
});

await t('保存：规则里 userid、手机号、所有人；列表显示“企业微信”和主机，没有“已加签”；接口里没有 key 和密钥', async () => {
  await setVal('#nc-name', 'E2E 企业微信');
  await setVal('#nc-url', HOOK);
  await addRule('raw', 'zhangsan');
  await addRule('raw', '13800138000');
  await addRule('all');
  await confirmSave();
  await waitFor("return document.querySelector('#nc-rows').textContent.includes('E2E 企业微信')", '保存后出现在列表');
  const row = await rowText();
  truthy(row.includes('企业微信') && row.includes('127.0.0.1'), '类型与主机：' + row);
  truthy(!row.includes('已加签'), '企业微信没有加签');
  truthy((await q('#nc-msg')).includes('已保存'), '提示');
  const api = JSON.stringify(await apiList());
  truthy(!api.includes('KEY-E2E-123') && !api.includes('"has_secret":true'), '接口里没有 key，也没有加签标记');
  truthy(api.includes('"type":"wecom"'), '类型');
});

await t('编辑已保存的企业微信渠道：规则回填，签名密钥输入框和“清除已有签名密钥”都不显示', async () => {
  await ev("[...document.querySelectorAll('#nc-rows tr')].find(tr=>tr.textContent.includes('E2E 企业微信')).querySelector('.row-actions .btn').click()");
  eq(await q('#nc-type', 'value'), 'wecom');
  eq(await visible('#nc-secret-wrap'), false);
  eq(await visible('#nc-clear-wrap'), false);
  eq(await ev("return [...document.querySelectorAll('#nc-rules .rule-row')].map(r => r.querySelector('select').value + ':' + ((r.querySelector('.rule-value input') || {}).value || '')).join(',')"), 'raw:zhangsan,raw:13800138000,all:');
  truthy((await q('#nc-url', 'placeholder')).includes('127.0.0.1'), '地址留空沿用：' + await q('#nc-url', 'placeholder'));
});

await t('测试发送：机器人收到文本消息，@ 列表是 userid + @all / 手机号；页面提示成功', async () => {
  hooks.length = 0;
  await click('#nc-test');
  await waitFor('return true', 'noop');
  await waitFor("return document.querySelector('#nc-msg').textContent.includes('测试消息已发送')", '测试发送提示');
  eq(hooks.length, 1, '只发 1 条');
  const h = hooks[0];
  truthy(h.url.startsWith('/cgi-bin/webhook/send?key=KEY-E2E-123') && !h.url.includes('sign=') && !h.url.includes('timestamp='), '地址里只有 key，没有加签：' + h.url);
  eq(h.body.msgtype, 'text');
  eq(h.body.text.mentioned_list, ['@all', 'zhangsan']);
  eq(h.body.text.mentioned_mobile_list, ['13800138000']);
  truthy(h.body.text.content.includes('通知渠道测试') && !h.body.text.content.includes('签名密钥'), '正文：' + h.body.text.content);
});

await t('测试发送：对端返回 errcode ≠ 0 时页面显示企业微信的错误，不含 key', async () => {
  failNext = true;
  hooks.length = 0;
  await click('#nc-test');
  await waitFor("return document.querySelector('#nc-msg').textContent.includes('企业微信返回错误 93000')", '错误提示');
  const msg = await q('#nc-msg');
  truthy(msg.includes('invalid webhook url') && !msg.includes('KEY-E2E-123'), msg);
});

await t('预览：消息效果里高亮 @；平台用户（没有企业微信 ID）标成“文本，不会提醒”，悬停有说明；不发请求', async () => {
  await addRule('user', 'dev'); // 演示数据里的平台用户
  hooks.length = 0;
  await setVal('#nc-sim-event', 'submitted');
  await click('#nc-preview-btn');
  await waitFor("return !!document.querySelector('#nc-preview .nc-pv')", '预览');
  const p = await q('#nc-preview');
  truthy(p.includes('消息效果（企业微信'), '标题：' + p.slice(0, 120));
  const ats = await ev("return [...document.querySelectorAll('#nc-preview .nc-msg .at')].map(s => s.textContent)");
  truthy(ats.length >= 1, '消息正文里的 @ 被高亮：' + JSON.stringify(ats) + '\n' + p);
  const warn = await ev("const li = document.querySelector('#nc-preview li.warn'); return li ? [li.textContent, li.title] : null;");
  truthy(warn && warn[0].includes('文本，不会提醒') && warn[1].includes('企业微信渠道不能按平台用户 @'), '平台用户的提示：' + JSON.stringify(warn));
  truthy(p.includes('@所有人') || p.includes('@zhangsan'), '原生标识 / 所有人也列出');
  eq(hooks.length, 0, '预览不发请求');
  await click('#nc-cancel');
});

await t('从钉钉（带签名密钥）改成企业微信：编辑时不再显示密钥相关项；保存后旧密钥被清掉', async () => {
  await click('#nc-new');
  await setVal('#nc-name', 'E2E 钉钉');
  await setVal('#nc-url', 'https://oapi.dingtalk.com/robot/send?access_token=tok');
  await setVal('#nc-secret', 'SECdingding');
  await confirmSave();
  await waitFor("return document.querySelector('#nc-rows').textContent.includes('E2E 钉钉')", '钉钉渠道出现在列表');
  truthy((await rowText()).includes('已加签'), '钉钉渠道已加签');
  await ev("[...document.querySelectorAll('#nc-rows tr')].find(tr=>tr.textContent.includes('E2E 钉钉')).querySelector('.row-actions .btn').click()");
  eq(await visible('#nc-clear-wrap'), true, '钉钉渠道可以清除签名密钥');
  await setVal('#nc-type', 'wecom');
  eq(await visible('#nc-secret-wrap'), false);
  eq(await visible('#nc-clear-wrap'), false, '改成企业微信后不再显示“清除”');
  await setVal('#nc-url', HOOK);
  await confirmSave();
  await waitFor("return !document.querySelector('#nc-panel') || document.querySelector('#nc-panel').hidden", '编辑面板关闭');
  const list = await apiList();
  const ch = list.data.channels.find((c) => c.name === 'E2E 钉钉');
  truthy(ch && ch.type === 'wecom' && ch.has_secret === false, '旧密钥应被清掉：' + JSON.stringify(ch));
});

await t('控制台零报错（含严格 CSP 违规）', async () => {
  // 演示服务没有登录配置 / 巡检模块，导航栏会请求到 404；测试发送对端返回业务错误是故意触发的（HTTP 502）。这些之外的错误一律算失败。
  const demoNoise = [/favicon\.ico/, /\/api\/v1\/auth\/ui/, /\/api\/v1\/inspection\//];
  const expected502 = consoleErrors.filter((e) => /status of 502/.test(e) && /\/api\/v1\/settings\/notify\/test$/.test(e));
  eq(expected502.length, 1, '故意触发的发送失败只有 1 次');
  eq(consoleErrors.filter((e) => !demoNoise.some((re) => re.test(e)) && !expected502.includes(e)), []);
});

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
