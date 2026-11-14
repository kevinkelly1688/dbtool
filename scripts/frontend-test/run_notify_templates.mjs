/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_notify_templates.mjs
 * @time: 2026/10/28
 */
// 通知消息模板页浏览器回归：起内存演示服务（cmd/demo，假数据，不连真实库）+ 无头 Chrome（CDP），不依赖第三方包。
// 覆盖：消息列表与搜索、打开模板（沿用系统默认）、变量插入、内容块增删改、SQL 显示方式与“隐藏字符串常量”、各渠道预览、
// 校验提示（未知变量不能保存）、保存 / 恢复走统一确认组件、渠道专属范围、控制台零报错（含严格 CSP）。
// 用法：node scripts/frontend-test/run_notify_templates.mjs [chrome路径]
// 环境变量：NT_APP_PORT（演示服务，默认 8905）、NT_CDP_PORT（Chrome 调试端口，默认 9242）、DEMO_EXE（已编译的 cmd/demo；不给则先 go build 到临时目录，需要 GOTOOLCHAIN=local）。
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '../..');
const APP = +(process.env.NT_APP_PORT || 8905);
const CDP = +(process.env.NT_CDP_PORT || 9242);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'nt-'));

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
  for (const p of [chrome, app]) { try { if (process.platform === 'win32') spawnSync('taskkill', ['/PID', String(p.pid), '/T', '/F'], { stdio: 'ignore' }); else p.kill(); } catch (_) { /* 已退出 */ } }
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
await send('Emulation.setDeviceMetricsOverride', { width: 1500, height: 1000, deviceScaleFactor: 1, mobile: false });

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
const has = (s, sub, m) => { if (!String(s).includes(sub)) throw new Error(`${m || ''} 应包含 ${JSON.stringify(sub)}，实得 ${JSON.stringify(String(s).slice(0, 300))}`); };
const q = (sel, prop = 'textContent') => ev(`const e=document.querySelector(${JSON.stringify(sel)}); return e ? e.${prop} : null;`);
const qa = (sel, prop = 'textContent') => ev(`return [...document.querySelectorAll(${JSON.stringify(sel)})].map(e=>e.${prop});`);
const visible = (sel) => ev(`const e=document.querySelector(${JSON.stringify(sel)}); return !!e && !e.hidden && getComputedStyle(e).display !== 'none' && e.getClientRects().length > 0;`);
const click = (sel) => ev(`document.querySelector(${JSON.stringify(sel)}).click();`);
const setVal = (sel, v) => ev(`const i=document.querySelector(${JSON.stringify(sel)}); i.value=${JSON.stringify(v)}; i.dispatchEvent(new Event('input',{bubbles:true})); i.dispatchEvent(new Event('change',{bubbles:true}));`);
const waitFor = async (expr, what, ms = 8000) => {
  const end = Date.now() + ms;
  while (Date.now() < end) { if (await ev(expr).catch(() => false)) return; await sleep(100); }
  throw new Error('等待超时：' + what);
};
const okBtn = '.cf-foot .cf-btn:last-child';
const openMsg = async (name) => {
  await ev(`[...document.querySelectorAll('#nt-events .nt-ev')].find(b=>b.textContent.startsWith(${JSON.stringify(name)})).click();`);
  await waitFor("return !document.getElementById('nt-editor').hidden", '编辑器');
  await sleep(150);
};
const waitPreview = (sub) => waitFor(`return document.getElementById('nt-pv').textContent.includes(${JSON.stringify(sub)})`, '预览含 ' + sub);

// ---- 登录并打开页面 ----
await send('Page.navigate', { url: `http://127.0.0.1:${APP}/login` });
await waitFor("return !!document.querySelector('input[name=username]')", '登录页');
await ev("document.querySelector('input[name=username]').value='admin'; document.querySelector('input[name=password]').value='admin123'; document.querySelector('form').submit();");
await sleep(800);
await send('Page.navigate', { url: `http://127.0.0.1:${APP}/settings/notify/templates` });
await waitFor("return document.querySelectorAll('#nt-events .nt-ev').length > 0", '消息列表');
console.log(`通知消息模板页浏览器测试：演示服务 :${APP}`);

await t('消息列表按分组展示；搜索过滤', async () => {
  const groups = await qa('#nt-events .nt-grp');
  truthy(groups.includes('SQL 上线 / 数据导出') && groups.includes('查询权限') && groups.includes('数据归档'), '分组 ' + groups);
  await setVal('#nt-q', '被修改');
  const hit = await qa('#nt-events .nt-ev');
  await setVal('#nt-q', '');
  eq(hit, ['权限被修改 / 回收']);
});

await t('打开“执行完成”：标题与内容块是系统默认，预览有 Slack 样式；SQL 常量被隐藏', async () => {
  await openMsg('执行完成');
  eq(await q('#nt-title'), '执行完成');
  eq(await q('#nt-ttl', 'value'), '[{{kind}}]工单已正常结束#{{ticket_id}}');
  eq(await q('#nt-tone', 'value'), 'ok');
  truthy((await qa('#nt-blocks .nt-blk-h b')).some((x) => x.includes('字段表')) && (await qa('#nt-blocks .nt-blk-h b')).some((x) => x.includes('SQL')), '内容块');
  await waitPreview('SQL上线申请');
  const pv = await q('#nt-pv');
  has(pv, "secret_key = '***'", '隐藏字符串常量');
  truthy(!pv.includes('demo-secret-0123456789'), '密钥原文不应出现在预览里');
  eq(await visible('#nt-side'), true);
});

await t('SQL 显示方式改成“只显示前 N 字”后预览里常量原样可见；改回“隐藏”再隐藏', async () => {
  // 执行完成的默认模板有两个代码块：先是“执行记录”，最后是“工单详情预览”（SQL）
  const sel = '#nt-blocks .nt-blk:last-child .nt-code .opt select';
  await setVal(sel, 'head');
  await waitFor("return document.getElementById('nt-pv').textContent.includes('demo-secret-0123456789')", '常量可见');
  await setVal(sel, 'none');
  await waitFor("return document.getElementById('nt-pv').textContent.includes('SQL 已隐藏')", '不显示 SQL');
  await setVal(sel, 'mask');
  await waitFor("return document.getElementById('nt-pv').textContent.includes(\"'***'\")", '再次隐藏');
});

await t('代码块 / 字段的值是多行输入框：回车换行，换行后的内容出现在预览里', async () => {
  const code = '#nt-blocks .nt-blk:last-child .nt-code textarea';
  eq(await ev(`return document.querySelector(${JSON.stringify(code)}).tagName;`), 'TEXTAREA', '代码块内容应是多行输入框');
  eq(await ev("return document.querySelector('#nt-blocks .nt-row textarea').tagName;"), 'TEXTAREA', '字段的值应是多行输入框');
  const old = await ev(`return document.querySelector(${JSON.stringify(code)}).value;`);
  await setVal(code, '{{sql}}\n\n{{url}}');
  await waitFor("return document.getElementById('nt-pv').textContent.includes('/sqlworkflow/')", '换行后的 {{url}} 出现在预览里');
  eq(await ev(`return document.querySelector(${JSON.stringify(code)}).value.split('\\n').length;`), 3);
  await setVal(code, old);
});

await t('点变量插入到光标所在输入框；未知变量报错且不能保存', async () => {
  await click('#nt-ttl');
  await ev("const i=document.getElementById('nt-ttl'); i.focus(); i.setSelectionRange(i.value.length, i.value.length); i.dispatchEvent(new Event('focusin',{bubbles:true}));");
  await ev("[...document.querySelectorAll('#nt-vars .nt-chip')].find(c=>c.textContent.includes('{{instance}}')).click();");
  has(await q('#nt-ttl', 'value'), '{{instance}}');
  await setVal('#nt-ttl', '[{{kind}}]{{nope}}');
  await waitFor("return document.getElementById('nt-issues').textContent.includes('不存在的变量')", '问题提示');
  eq(await q('#nt-save', 'disabled'), true, '有硬伤不能保存');
  await setVal('#nt-ttl', '✅ 工单 #{{ticket_id}} 执行成功（{{elapsed}}）');
  await waitFor("return !document.getElementById('nt-save').disabled", '可保存');
});

await t('增删内容块：加一个链接按钮，预览出现按钮；删除一块', async () => {
  const n0 = (await qa('#nt-blocks .nt-blk')).length;
  await click('[data-add=link]');
  eq((await qa('#nt-blocks .nt-blk')).length, n0 + 1);
  await waitFor("return !!document.querySelector('#nt-pv .btnx')", '按钮');
  await ev("[...document.querySelectorAll('#nt-blocks .nt-blk')].pop().querySelector('.nt-blk-h button:last-child').click();");
  eq((await qa('#nt-blocks .nt-blk')).length, n0);
});

await t('切到飞书 / 邮件预览：飞书标题栏按状态着色；邮件显示主题', async () => {
  await ev("[...document.querySelectorAll('#nt-ptabs .tab')].find(b=>b.textContent.includes('飞书')).click();");
  await waitFor("return !!document.querySelector('#nt-pv .nt-pv-msg.feishu.ok')", '飞书预览');
  await ev("[...document.querySelectorAll('#nt-ptabs .tab')].find(b=>b.textContent==='邮件').click();");
  await waitFor("return document.getElementById('nt-pv').textContent.includes('主题：')", '邮件主题');
  await ev("[...document.querySelectorAll('#nt-ptabs .tab')].find(b=>b.textContent==='Slack').click();");
});

await t('预览平台里有“企业微信”（排在钉钉后面）：标题、字段、SQL（常量已隐藏）、链接都排出来', async () => {
  eq(await qa('#nt-ptabs .tab'), ['Slack', '钉钉', '企业微信', '飞书 / Lark', 'Telegram', '邮件']);
  await ev("[...document.querySelectorAll('#nt-ptabs .tab')].find(b=>b.textContent==='企业微信').click();");
  await waitFor("return !!document.querySelector('#nt-pv .nt-pv-msg.wecom.ok')", '企业微信预览');
  const pv = await q('#nt-pv');
  has(pv, '工单 #3058 执行成功', '标题');
  has(pv, "secret_key = '***'", '企业微信预览里 SQL 常量也被隐藏');
  truthy(!pv.includes('demo-secret-0123456789'), '密钥原文不应出现在预览里');
  eq(await ev("return document.querySelectorAll('#nt-pv .nt-pv-msg.wecom ul.ln li').length > 0"), true, '字段按“名称：值”逐行排');
  await ev("[...document.querySelectorAll('#nt-ptabs .tab')].find(b=>b.textContent==='Slack').click();");
});

await t('预览平台里有“Telegram”（排在飞书 / Lark 后面）：标题、字段、SQL（常量已隐藏）都排出来', async () => {
  await ev("[...document.querySelectorAll('#nt-ptabs .tab')].find(b=>b.textContent==='Telegram').click();");
  await waitFor("return !!document.querySelector('#nt-pv .nt-pv-msg.telegram.ok')", 'Telegram 预览');
  const pv = await q('#nt-pv');
  has(pv, '工单 #3058 执行成功', '标题');
  has(pv, "secret_key = '***'", 'Telegram 预览里 SQL 常量也被隐藏');
  truthy(!pv.includes('demo-secret-0123456789'), '密钥原文不应出现在预览里');
  eq(await ev("return document.querySelectorAll('#nt-pv .nt-pv-msg.telegram ul.ln li').length > 0"), true, '字段按“名称：值”逐行排');
  await ev("[...document.querySelectorAll('#nt-ptabs .tab')].find(b=>b.textContent==='Slack').click();");
});

await t('保存走确认组件；列表出现“全局”标记；重新打开取回保存的标题', async () => {
  await setVal('#nt-note', '页面测试');
  await click('#nt-save');
  await waitFor("return !!document.querySelector('.cf-dialog')", '确认弹窗');
  has(await q('.cf-title'), '保存「执行完成」的模板');
  await click(okBtn);
  await waitFor("return !document.querySelector('.cf-dialog')", '关闭');
  await waitFor("return [...document.querySelectorAll('#nt-events .nt-ev')].find(b=>b.textContent.startsWith('执行完成')).textContent.includes('全局')", '全局标记');
  eq(await visible('#nt-reset'), true);
  // 换一条消息再回来
  await openMsg('审批通过');
  await openMsg('执行完成');
  eq(await q('#nt-ttl', 'value'), '✅ 工单 #{{ticket_id}} 执行成功（{{elapsed}}）');
  has(await q('#nt-source'), '已定制');
});

await t('未保存的修改切换时提示；放弃后换过去', async () => {
  await setVal('#nt-name', '改了一下');
  await ev("[...document.querySelectorAll('#nt-events .nt-ev')].find(b=>b.textContent.startsWith('审批通过')).click();");
  await waitFor("return !!document.querySelector('.cf-dialog')", '放弃提示');
  has(await q('.cf-title'), '放弃未保存的修改');
  await click('.cf-foot .cf-btn:first-child'); // 继续编辑
  await waitFor("return !document.querySelector('.cf-dialog')", '关闭');
  eq(await q('#nt-title'), '执行完成');
  await ev("[...document.querySelectorAll('#nt-events .nt-ev')].find(b=>b.textContent.startsWith('审批通过')).click();");
  await waitFor("return !!document.querySelector('.cf-dialog')", '放弃提示');
  await click(okBtn);
  await waitFor("return document.getElementById('nt-title').textContent === '审批通过'", '已切换');
});

await t('渠道专属范围：选一个渠道，说明“未定制，发送时用全局模板 / 系统默认文案”', async () => {
  // 演示数据里没有渠道：先用接口建一个 Slack 渠道（地址不会被访问），再刷新页面
  const made = await ev(`const r = await fetch('/api/v1/settings/notify/channels', { method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': document.querySelector('meta[name=csrf-token]').content },
    body: JSON.stringify({ group_id: 0, name: '模板测试群', type: 'slack', url: 'https://hooks.slack.com/services/T1/B2/TOKEN', enabled: true, events: [], rules: [] }) });
    return r.status;`);
  eq(made, 200, '建渠道');
  await send('Page.navigate', { url: `http://127.0.0.1:${APP}/settings/notify/templates` });
  await waitFor("return document.querySelectorAll('#nt-events .nt-ev').length > 0", '消息列表');
  await openMsg('执行完成');
  const opts = await ev("return [...document.querySelectorAll('#nt-scope option')].map(o=>[o.value,o.textContent]);");
  truthy(opts.length >= 2, '演示数据里至少有一个渠道 ' + JSON.stringify(opts));
  await setVal('#nt-scope', opts[1][0]);
  // 文案见 templates.js renderSource：「{scope}」未定制，发送时用{全局模板 | 系统默认文案}。
  await waitFor("return document.getElementById('nt-source').textContent.includes('未定制')", '渠道范围说明');
  has(await q('#nt-source'), '发送时用全局模板'); // 全局已经定制过：上一层是全局模板
});

await t('恢复：删除全局模板走确认组件，标记消失', async () => {
  await setVal('#nt-scope', 'global');
  await waitFor("return document.getElementById('nt-source').textContent.includes('已定制')", '回到全局');
  await click('#nt-reset');
  await waitFor("return !!document.querySelector('.cf-dialog')", '确认弹窗');
  has(await q('.cf-title'), '删除这个范围的模板');
  await click(okBtn);
  await waitFor("return !document.querySelector('.cf-dialog')", '关闭');
  await waitFor("return ![...document.querySelectorAll('#nt-events .nt-ev')].find(b=>b.textContent.startsWith('执行完成')).textContent.includes('全局')", '标记消失');
  eq(await q('#nt-ttl', 'value'), '[{{kind}}]工单已正常结束#{{ticket_id}}', '回到系统默认');
});

await t('历史：能看到保存与删除记录', async () => {
  await click('#nt-history');
  await waitFor("return document.querySelectorAll('#nt-hist tr').length >= 3", '历史行');
  const txt = await q('#nt-hist');
  has(txt, '页面测试');
  has(txt, '删除');
});

await t('控制台零报错（含严格 CSP 违规）', async () => {
  const demoNoise = [/favicon\.ico/, /\/api\/v1\/auth\/ui/, /\/api\/v1\/inspection\//];
  eq(consoleErrors.filter((e) => !demoNoise.some((re) => re.test(e))), []);
});

console.log(`\n通过 ${pass}，失败 ${fail}`);
cleanup();
process.exit(fail ? 1 : 0);
