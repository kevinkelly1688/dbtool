/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_notify_email.mjs
 * @time: 2026/10/28
 */
// 通知渠道页（邮件渠道）浏览器回归：起内存演示服务（cmd/demo，假数据，不连真实库）+ 进程内假 SMTP + 无头 Chrome（CDP），不依赖第三方包。
// 覆盖：类型切换后的表单、加密方式联动、证书校验 / 明文认证的风险提示、保存 / 启停 / 删除走统一确认组件、测试发送的分阶段结果、预览、
// 严格 CSP 下控制台零报错。
// 用法：node scripts/frontend-test/run_notify_email.mjs [chrome路径]
// 环境变量：NE_APP_PORT（演示服务，默认 8904）、NE_CDP_PORT（Chrome 调试端口，默认 9241）、DEMO_EXE（已编译的 cmd/demo；不给则先 go build 到临时目录，需要 GOTOOLCHAIN=local）。
// 结束时只按 PID 结束自己启动的进程。
import net from 'node:net';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '../..');
const APP = +(process.env.NE_APP_PORT || 8904);
const CDP = +(process.env.NE_CDP_PORT || 9241);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'ne-'));

// ---- 假 SMTP：明文，AUTH PLAIN 一律通过，记录收到的邮件 ----
const mails = [];
let authSeen = 0;
const smtp = net.createServer((c) => {
  let buf = '';
  let mail = null;
  let data = null;
  c.write('220 fake ESMTP\r\n');
  c.on('data', (chunk) => {
    buf += chunk.toString('utf8');
    for (;;) {
      if (data !== null) {
        const i = buf.indexOf('\r\n.\r\n');
        if (i < 0) return;
        mail.data = buf.slice(0, i);
        buf = buf.slice(i + 5);
        data = null;
        mails.push(mail);
        c.write('250 queued\r\n');
        continue;
      }
      const i = buf.indexOf('\r\n');
      if (i < 0) return;
      const line = buf.slice(0, i);
      buf = buf.slice(i + 2);
      const up = line.toUpperCase();
      if (up.startsWith('EHLO')) c.write('250-fake\r\n250-AUTH PLAIN\r\n250 8BITMIME\r\n');
      else if (up.startsWith('AUTH')) { authSeen++; c.write('235 ok\r\n'); }
      else if (up.startsWith('MAIL FROM:')) { mail = { rcpts: [], data: '' }; c.write('250 ok\r\n'); }
      else if (up.startsWith('RCPT TO:')) { mail.rcpts.push(line.slice(line.indexOf('<') + 1, line.indexOf('>'))); c.write('250 ok\r\n'); }
      else if (up === 'DATA') { data = ''; c.write('354 go\r\n'); }
      else if (up === 'QUIT') { c.write('221 bye\r\n'); c.end(); return; }
      else c.write('250 ok\r\n');
    }
  });
  c.on('error', () => {});
});
await new Promise((r) => smtp.listen(0, '127.0.0.1', r));
const SMTP_PORT = smtp.address().port;

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
  smtp.close();
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
const setChecked = (sel, v) => ev(`const i=document.querySelector(${JSON.stringify(sel)}); if (i.checked !== ${v}) i.click();`);
const waitFor = async (expr, what, ms = 8000) => {
  const end = Date.now() + ms;
  while (Date.now() < end) { if (await ev(expr).catch(() => false)) return; await sleep(100); }
  throw new Error('等待超时：' + what);
};
const okBtn = '.cf-foot .cf-btn:last-child';
const cancelBtn = '.cf-foot .cf-btn:first-child';
const dialogOpen = () => visible('.cf-dialog');
const rowText = () => q('#nc-rows');

// ---- 登录 ----
await send('Page.navigate', { url: `http://127.0.0.1:${APP}/login` });
await waitFor("return !!document.querySelector('input[name=username]')", '登录页');
await ev("document.querySelector('input[name=username]').value='admin'; document.querySelector('input[name=password]').value='admin123'; document.querySelector('form').submit();");
await sleep(800);
await send('Page.navigate', { url: `http://127.0.0.1:${APP}/settings/notify` });
await waitFor("return document.querySelectorAll('#nc-rows tr').length > 0 && !document.querySelector('#nc-rows').textContent.includes('加载中')", '渠道页加载');
console.log(`通知渠道页（邮件渠道）浏览器测试：演示服务 :${APP}，假 SMTP :${SMTP_PORT}`);

await t('类型下拉有“邮件”；新增渠道时机器人表单：Webhook 可见、邮件字段隐藏', async () => {
  await click('#nc-new');
  eq(await ev("return [...document.querySelectorAll('#nc-type option')].map(o=>o.textContent).join(',')"), '钉钉,企业微信,飞书,Lark,Slack,Telegram,邮件');
  eq(await visible('#nc-url-wrap'), true, 'Webhook');
  eq(await visible('#nc-email'), false, '邮件区');
  eq(await visible('#nc-test-to-wrap'), false, '测试收件人');
  truthy((await q('#nc-secret-txt')).includes('签名密钥'), '签名密钥标签');
});

await t('切换到邮件：Webhook 隐藏、SMTP 区显示，默认 STARTTLS / 587 / 校验证书 / 主题前缀，密码标签变化', async () => {
  await setVal('#nc-type', 'email');
  eq(await visible('#nc-url-wrap'), false);
  eq(await visible('#nc-email'), true);
  eq(await visible('#nc-test-to-wrap'), true);
  eq(await q('#nc-em-enc', 'value'), 'starttls');
  eq(await q('#nc-em-port', 'value'), '587');
  eq(await q('#nc-em-verify', 'checked'), true);
  eq(await q('#nc-em-prefix', 'value'), '[dbtool]');
  truthy((await q('#nc-secret-txt')).includes('SMTP 密码'), '密码标签');
  eq(await q('#nc-secret', 'type'), 'password');
  // 规则帮助区已不存在：邮件的叫法直接体现在 @ 规则的目标下拉里（notify.js EMAIL_TARGET），新增一行规则验证，再删掉
  await click('#nc-add-rule');
  const rawLabel = await ev("const s=document.querySelector('#nc-rules .rule-row select'); return [...s.options].find(o=>o.value==='raw').textContent;");
  truthy(rawLabel.includes('邮箱'), '规则目标里“渠道原生标识”在邮件渠道叫邮箱地址：' + rawLabel);
  await ev("document.querySelector('#nc-rules .rule-row .btn.ghost').click();");
  eq(await ev("return document.querySelectorAll('#nc-rules .rule-row').length"), 0, '临时规则已删除');
});

await t('加密方式联动：端口跟随默认值；不加密出现“允许明文认证”及风险提示；关闭校验出现风险提示', async () => {
  await setVal('#nc-em-enc', 'ssl'); eq(await q('#nc-em-port', 'value'), '465');
  await setVal('#nc-em-enc', 'none'); eq(await q('#nc-em-port', 'value'), '25');
  eq(await visible('#nc-em-plain-wrap'), true);
  truthy((await q('#nc-em-plain-wrap')).includes('明文'), '明文认证风险提示');
  await setVal('#nc-em-enc', 'starttls'); eq(await q('#nc-em-port', 'value'), '587');
  eq(await visible('#nc-em-plain-wrap'), false);
  eq(await visible('#nc-em-verify-warn'), false);
  await setChecked('#nc-em-verify', false);
  eq(await visible('#nc-em-verify-warn'), true);
  truthy((await q('#nc-em-verify-warn')).includes('中间人'), '证书风险提示');
  await setChecked('#nc-em-verify', true);
  eq(await visible('#nc-em-verify-warn'), false);
  await setVal('#nc-em-port', '2525'); await setVal('#nc-em-enc', 'ssl'); eq(await q('#nc-em-port', 'value'), '2525', '改过的端口不被覆盖');
});

await t('@ 规则里邮件的叫法：邮箱地址 / 资源组的全部用户', async () => {
  await click('#nc-add-rule');
  eq(await ev("return [...document.querySelectorAll('#nc-rules .rule-row select')[0].options].map(o=>o.textContent).join('|')"),
    '提交人|当前审批级的审批人|DBA 值班|某个权限组的成员|指定用户（用户名）|邮箱地址|工单所属资源组的全部用户');
  await ev("const s=document.querySelector('#nc-rules .rule-row select'); s.value='applicant'; s.dispatchEvent(new Event('change',{bubbles:true}));");
});

const fillForm = async (extra = {}) => {
  await setVal('#nc-name', 'E2E 邮件');
  await setVal('#nc-em-host', '127.0.0.1');
  await setVal('#nc-em-enc', 'none');
  await setVal('#nc-em-port', String(SMTP_PORT));
  await setVal('#nc-em-user', extra.user ?? 'mailer');
  await setVal('#nc-secret', 'Sm7p-pass');
  await setVal('#nc-em-from', 'dbtool@example.com');
  await setVal('#nc-em-fromname', 'DBA 工具');
  await setVal('#nc-em-to', 'ops@example.com, dba@example.com');
  await setVal('#nc-em-cc', 'boss@example.com');
};

await t('保存走确认组件；服务端拒绝（不加密 + 认证但未允许明文认证）时错误显示在弹窗里，弹窗不关闭', async () => {
  await fillForm();
  await click('#nc-save');
  await waitFor("return !!document.querySelector('.cf-dialog')", '确认弹窗');
  truthy((await q('.cf-title')).includes('新增渠道「E2E 邮件」'), '标题');
  truthy((await q('.cf-details')).includes('127.0.0.1:' + SMTP_PORT), '详情含 SMTP');
  truthy((await q('.cf-lines')).includes('明文'), '风险说明');
  eq(await ev("return document.querySelector('.cf-dialog').classList.contains('cf-high')"), true, '风险项按高危');
  await click(okBtn);
  await waitFor("const e=document.querySelector('.cf-err'); return !!e && e.textContent.includes('明文认证')", '弹窗内错误');
  eq(await dialogOpen(), true, '弹窗仍在');
  await click(cancelBtn);
  await waitFor("return !document.querySelector('.cf-dialog')", '弹窗关闭');
  eq(await visible('#nc-panel'), true, '取消后编辑面板仍在');
});

await t('勾选“允许明文认证”后保存成功；列表显示 SMTP 主机 / 端口 / 加密、已设密码、风险标记；页面不含密码', async () => {
  await setChecked('#nc-em-plain', true);
  await click('#nc-save');
  await waitFor("return !!document.querySelector('.cf-dialog')", '确认弹窗');
  await click(okBtn);
  await waitFor("return !document.querySelector('.cf-dialog') && document.querySelector('#nc-rows').textContent.includes('E2E 邮件')", '保存后出现在列表');
  const row = await rowText();
  truthy(row.includes(`127.0.0.1:${SMTP_PORT} · 不加密`), 'SMTP 摘要：' + row);
  truthy(row.includes('已设密码'), '已设密码');
  truthy(row.includes('dbtool@example.com') && row.includes('固定收件人 3 个'), '发件人 / 收件人数');
  truthy(row.includes('明文传输且明文认证'), '风险标记');
  truthy((await q('#nc-msg')).includes('已保存'), '提示');
  eq(await ev("return document.documentElement.outerHTML.includes('Sm7p-pass')"), false, '页面里不应出现密码');
  const api = await ev("const r = await fetch('/api/v1/settings/notify'); return await r.text();");
  eq(api.includes('Sm7p-pass'), false, '接口里不应出现密码');
  truthy(api.includes('"has_secret":true'), 'has_secret');
});

await t('编辑：配置回填，密码框为空并提示“已设置，留空不修改”', async () => {
  await ev("[...document.querySelectorAll('#nc-rows .row-actions .btn')].find(b=>b.textContent==='编辑').click()");
  eq(await visible('#nc-email'), true);
  eq(await q('#nc-em-host', 'value'), '127.0.0.1');
  eq(await q('#nc-em-enc', 'value'), 'none');
  eq(await q('#nc-em-to', 'value'), 'ops@example.com, dba@example.com');
  eq(await q('#nc-em-plain', 'checked'), true);
  eq(await q('#nc-secret', 'value'), '');
  // 提示现在是密码框的 placeholder（notify.js applyType），不再是独立的 #nc-secret-hint 元素
  truthy((await q('#nc-secret', 'placeholder')).includes('留空不修改'), '提示');
  eq(await visible('#nc-clear-wrap'), true);
  truthy((await q('#nc-clear-txt')).includes('SMTP 密码'), '清除选项文案');
});

await t('测试发送：只发给测试收件人，逐阶段显示结果（连接 / STARTTLS / 认证 / 发送）', async () => {
  mails.length = 0;
  await setVal('#nc-test-to', 'me@example.com');
  await click('#nc-test');
  await waitFor("return document.querySelectorAll('#nc-test-result .nc-stage').length === 4", '四个阶段');
  const stages = await ev("return [...document.querySelectorAll('#nc-test-result .nc-stage')].map(li => li.children[0].textContent + ':' + li.children[1].textContent).join(',')");
  eq(stages, '连接:通过,STARTTLS:跳过,认证:通过,发送:通过');
  truthy((await q('#nc-test-result')).includes('me@example.com'), '结果标题含收件人');
  truthy((await q('#nc-msg')).includes('测试邮件已提交'), '提示');
  await waitFor('return true', 'noop');
  eq(mails.length, 1);
  eq(mails[0].rcpts, ['me@example.com']);
  truthy(mails[0].data.includes('Subject:'), '邮件头');
  truthy(authSeen >= 1, '做过认证');
  truthy(!(await q('#nc-test-result')).includes('Sm7p-pass'), '结果里不含密码');
});

await t('测试发送失败：连不上时显示失败阶段与原因', async () => {
  await setVal('#nc-em-port', '1');
  await click('#nc-test');
  await waitFor("return !!document.querySelector('#nc-test-result .nc-stage.fail')", '失败阶段', 20000);
  truthy((await q('#nc-msg')).includes('发送失败'), '错误提示');
  const stages = await ev("return [...document.querySelectorAll('#nc-test-result .nc-stage')].map(li => li.children[0].textContent + ':' + li.children[1].textContent).join(',')");
  eq(stages, '连接:失败,STARTTLS:跳过,认证:跳过,发送:跳过');
  await setVal('#nc-em-port', String(SMTP_PORT));
});

await t('预览：显示邮件主题 / 收件人 / 正文，不发送', async () => {
  mails.length = 0;
  await setVal('#nc-sim-event', 'passed');
  await click('#nc-preview-btn');
  await waitFor("return !!document.querySelector('#nc-preview .nc-pv')", '预览');
  const p = await q('#nc-preview');
  truthy(p.includes('邮件效果') && p.includes('将发送给谁') && p.includes('ops@example.com') && p.includes('boss@example.com'), p.slice(0, 300));
  eq(mails.length, 0, '预览不发邮件');
});

await t('取消编辑后，停用走确认组件：取消不生效，确认后状态变为停用', async () => {
  await click('#nc-cancel');
  const toggleBtn = "[...document.querySelectorAll('#nc-rows .row-actions .btn')].find(b=>b.textContent==='停用' || b.textContent==='启用')";
  await ev(`${toggleBtn}.click()`);
  await waitFor("return !!document.querySelector('.cf-dialog')", '确认弹窗');
  truthy((await q('.cf-title')).includes('停用通知渠道「E2E 邮件」'), '标题');
  await click(cancelBtn);
  await waitFor("return !document.querySelector('.cf-dialog')", '关闭');
  truthy((await rowText()).includes('启用'), '取消后仍启用');
  await ev(`${toggleBtn}.click()`);
  await waitFor("return !!document.querySelector('.cf-dialog')", '确认弹窗');
  await click(okBtn);
  await waitFor("return !document.querySelector('.cf-dialog') && document.querySelector('#nc-rows .st.aborted')", '停用后状态');
  truthy((await q('#nc-msg')).includes('已停用'), '提示');
});

await t('机器人渠道仍可保存（回归）：Webhook 必填，保存同样走确认组件', async () => {
  await click('#nc-new');
  await setVal('#nc-type', 'slack');
  eq(await visible('#nc-email'), false);
  eq(await visible('#nc-url-wrap'), true);
  await setVal('#nc-name', 'Slack 群');
  await setVal('#nc-url', 'https://hooks.slack.com/services/T1/B2/TOKEN');
  await click('#nc-save');
  await waitFor("return !!document.querySelector('.cf-dialog')", '确认弹窗');
  eq(await ev("return document.querySelector('.cf-dialog').classList.contains('cf-high')"), false, '普通级别');
  await click(okBtn);
  await waitFor("return !document.querySelector('.cf-dialog') && document.querySelector('#nc-rows').textContent.includes('Slack 群')", '保存');
});

await t('删除走确认组件', async () => {
  const delBtn = (name) => `[...document.querySelectorAll('#nc-rows tr')].find(tr=>tr.textContent.includes(${JSON.stringify(name)})).querySelector('.btn.danger')`;
  await ev(`${delBtn('E2E 邮件')}.click()`);
  await waitFor("return !!document.querySelector('.cf-dialog')", '确认弹窗');
  truthy((await q('.cf-title')).includes('删除通知渠道「E2E 邮件」'), '标题');
  truthy((await q('.cf-details')).includes('SMTP'), '详情标 SMTP');
  await click(okBtn);
  await waitFor("return !document.querySelector('.cf-dialog') && !document.querySelector('#nc-rows').textContent.includes('E2E 邮件')", '删除后消失');
  await ev(`${delBtn('Slack 群')}.click()`);
  await waitFor("return !!document.querySelector('.cf-dialog')", '确认弹窗');
  await click(okBtn);
  await waitFor("return !document.querySelector('.cf-dialog')", '关闭');
});

await t('控制台零报错（含严格 CSP 违规）', async () => {
  // 演示服务没有登录配置 / 巡检模块，导航栏会请求到 404；第 5 项测试故意触发一次服务端 400（弹窗内显示）。这些之外的错误一律算失败。
  const demoNoise = [/favicon\.ico/, /\/api\/v1\/auth\/ui/, /\/api\/v1\/inspection\//];
  const expected400 = consoleErrors.filter((e) => /status of 400/.test(e) && /\/api\/v1\/settings\/notify\/channels$/.test(e));
  eq(expected400.length, 1, '故意触发的服务端校验错误只有 1 次');
  eq(consoleErrors.filter((e) => !demoNoise.some((re) => re.test(e)) && !expected400.includes(e)), []);
});

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
