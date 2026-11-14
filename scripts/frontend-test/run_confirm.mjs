/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_confirm.mjs
 * @time: 2026/10/28
 */
// 统一确认组件回归测试：自起静态服务 + 无头 Chrome（CDP），不依赖任何第三方包。
// 用法：node scripts/frontend-test/run_confirm.mjs [chrome路径]   环境变量 CF_HTTP_PORT / CF_CDP_PORT 可改端口。
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const staticRoot = path.resolve(here, '../../web/static');
const HTTP = +(process.env.CF_HTTP_PORT || 8898);
const CDP = +(process.env.CF_CDP_PORT || 9235);
const chromePath = process.argv[2] || 'C:/Program Files/Google/Chrome/Application/chrome.exe';
// 与应用一致的严格 CSP（style-src 去掉 unsafe-inline 以便发现内联样式）
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'";
const MIME = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8' };

const server = http.createServer((req, res) => {
  const u = decodeURIComponent(req.url.split('?')[0]);
  let file = null;
  if (u.startsWith('/static/')) file = path.join(staticRoot, u.slice(8));
  else if (u === '/t/confirm.html') file = path.join(here, 'confirm.html');
  else if (u === '/test/confirm_page.js') file = path.join(here, 'confirm_page.js');
  const okPath = file && (file.startsWith(here) || file.startsWith(staticRoot)) && fs.existsSync(file);
  if (!okPath) { res.writeHead(404); res.end('nf'); return; }
  res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream', 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' });
  fs.createReadStream(file).pipe(res);
});
await new Promise((r) => server.listen(HTTP, '127.0.0.1', r));

const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'cf-chrome-'));
const chrome = spawn(chromePath, ['--headless=new', `--remote-debugging-port=${CDP}`, `--user-data-dir=${profile}`, '--no-first-run', '--disable-gpu', 'about:blank'], { stdio: 'ignore' });
const cleanup = () => {
  try { chrome.kill(); } catch (_) { /* 已退出 */ }
  server.close();
  setTimeout(() => { try { fs.rmSync(profile, { recursive: true, force: true }); } catch (_) { /* 占用中则留给系统清理 */ } }, 1500).unref();
};

async function getWs() {
  for (let i = 0; i < 80; i++) {
    try {
      const r = await fetch(`http://127.0.0.1:${CDP}/json`);
      const list = await r.json();
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

// 在页面里求值（支持 await）
async function ev(expr) {
  const r = await send('Runtime.evaluate', { expression: `(async()=>{${expr}})()`, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error('页面异常：' + ((r.exceptionDetails.exception || {}).description || r.exceptionDetails.text));
  return r.result.value;
}
const KEYS = { Enter: 13, Escape: 27, Tab: 9 };
async function key(k, extra = {}) {
  const base = { key: k, code: k, windowsVirtualKeyCode: KEYS[k] || 0, ...extra };
  if (k === 'Enter') base.text = '\r';
  await send('Input.dispatchKeyEvent', { type: base.text ? 'keyDown' : 'rawKeyDown', ...base });
  await send('Input.dispatchKeyEvent', { type: 'keyUp', ...base });
}
const typeText = (s) => send('Input.insertText', { text: s });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let pass = 0, fail = 0;
async function t(name, fn) {
  try { await fn(); pass++; console.log('  ok   ' + name); } catch (e) { fail++; console.log('  FAIL ' + name + '\n       ' + e.message); }
}
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${m || ''} 期望 ${JSON.stringify(b)} 实得 ${JSON.stringify(a)}`); };
const truthy = (a, m) => { if (!a) throw new Error('应为真：' + m); };

async function reset() {
  await ev("window.CF.closeConfirm(); document.querySelectorAll('.cf-toasts').forEach(e=>e.remove()); document.getElementById('opener').focus(); window.__r = undefined;");
}
// 打开弹窗但不 await，结果存到 window.__r
const open = (expr) => ev(`window.__r = undefined; ${expr}.then(r => { window.__r = r; }); await new Promise(r => setTimeout(r, 120));`);
const result = () => ev("await new Promise(r=>setTimeout(r,60)); return window.__r === undefined ? '__pending' : window.__r;");
const q = (sel, prop = 'textContent') => ev(`const e=document.querySelector(${JSON.stringify(sel)}); return e ? e.${prop} : null;`);
const exists = (sel) => ev(`return !!document.querySelector(${JSON.stringify(sel)});`);
const click = (sel) => ev(`document.querySelector(${JSON.stringify(sel)}).click();`);
const input = (sel, v) => ev(`const i=document.querySelector(${JSON.stringify(sel)}); i.value=${JSON.stringify(v)}; i.dispatchEvent(new Event('input',{bubbles:true}));`);
const okBtn = '.cf-foot .cf-btn:last-child';
const cancelBtn = '.cf-foot .cf-btn:first-child';
const CLICK_MASK = "const m=document.querySelector('.cf-mask'); m.dispatchEvent(new MouseEvent('mousedown',{bubbles:true})); m.dispatchEvent(new MouseEvent('click',{bubbles:true}));";

await send('Page.navigate', { url: `http://127.0.0.1:${HTTP}/t/confirm.html` });
for (let i = 0; i < 40; i++) { if (await ev('return !!window.__ready').catch(() => false)) break; await sleep(150); }
truthy(await ev('return !!window.__ready'), '测试页加载失败');
console.log(`确认组件测试（http://127.0.0.1:${HTTP}/t/confirm.html）`);

await t('基本：确认返回 ok:true，样式被动态插入', async () => {
  await reset();
  await open("CF.riskConfirm({title:'T',lines:['a']})");
  truthy(await exists('.cf-dialog'), '弹窗存在');
  truthy(await exists('link[data-cf-css][href="/static/css/confirm.css"]'), 'link 已插入');
  eq(await ev("return getComputedStyle(document.querySelector('.cf-mask')).position"), 'fixed', 'CSS 生效');
  await click(okBtn);
  eq(await result(), { ok: true });
  eq(await exists('.cf-dialog'), false, '关闭后移除');
});
await t('取消按钮 -> ok:false', async () => {
  await reset(); await open("CF.riskConfirm({title:'T'})");
  await click(cancelBtn); eq(await result(), { ok: false });
});
await t('Esc 取消；焦点还原到触发元素', async () => {
  await reset(); await ev("document.getElementById('opener').focus()");
  await open("CF.riskConfirm({title:'T'})");
  await key('Escape');
  eq(await result(), { ok: false });
  eq(await ev('return document.activeElement.id'), 'opener', '焦点还原');
});
await t('Enter 确认（焦点在对话框内非输入框时）；输入框内 Enter 不确认', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',reason:{label:'原因',required:false}})");
  await ev("document.querySelector('.cf-textarea').focus()");
  await key('Enter');
  eq(await result(), '__pending', '输入框内 Enter 不应确认');
  await ev("const b=document.querySelector('.cf-body'); b.tabIndex=-1; b.focus();");
  await key('Enter');
  eq(await result(), { ok: true, value: '' });
});
await t('ARIA：role=dialog / aria-modal / aria-labelledby 指向标题', async () => {
  await reset(); await open("CF.riskConfirm({title:'删除实例'})");
  eq(await q('.cf-dialog', "getAttribute('role')"), 'dialog');
  eq(await q('.cf-dialog', "getAttribute('aria-modal')"), 'true');
  eq(await ev("const d=document.querySelector('.cf-dialog'); return document.getElementById(d.getAttribute('aria-labelledby')).textContent"), '删除实例');
});
await t('焦点陷阱：Tab/Shift+Tab 循环且不离开弹窗，外部聚焦被拉回', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',sql:'select 1',level:'high'})");
  const inside = () => ev("return document.querySelector('.cf-dialog').contains(document.activeElement)");
  for (let i = 0; i < 9; i++) { await key('Tab'); truthy(await inside(), 'Tab 第' + i + '次'); }
  for (let i = 0; i < 9; i++) { await key('Tab', { modifiers: 8 }); truthy(await inside(), 'Shift+Tab 第' + i + '次'); }
  await ev("document.getElementById('opener').focus()");
  truthy(await inside(), '外部聚焦被拉回');
});
await t('高危：页脚显示权限码与“高危操作”，标题徽标，初始焦点在取消，确认钮红色', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',level:'high',code:'sqlworkflow.execute'})");
  const foot = await q('.cf-foot');
  truthy(foot.includes('sqlworkflow.execute') && foot.includes('高危操作'), foot);
  eq(await q('.cf-badge'), '高危');
  eq(await ev('return document.activeElement.textContent'), '取消', '高危默认焦点');
  eq(await ev("return document.querySelector('.cf-foot .cf-btn:last-child').className.includes('cf-btn-danger')"), true, '红色确认');
  eq(await q(okBtn), '确认执行');
});
await t('高危 + focusOk：初始焦点在确认按钮，样式仍是高危；有键入框时仍先到键入框；不传 focusOk 的高危框不变', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',level:'high',focusOk:true,okText:'确认执行'})");
  eq(await ev('return document.activeElement.textContent'), '确认执行', '焦点在确认');
  eq(await q('.cf-badge'), '高危');
  eq(await ev("return document.querySelector('.cf-foot .cf-btn:last-child').className.includes('cf-btn-danger')"), true, '仍是红色高危确认');
  await reset(); await open("CF.riskConfirm({title:'T',level:'high',focusOk:true,typed:'W-1'})");
  eq(await ev('return document.activeElement.tagName'), 'INPUT', '有键入框时先到键入框');
  await reset(); await open("CF.riskConfirm({title:'T',level:'high'})");
  eq(await ev('return document.activeElement.textContent'), '取消', '没传 focusOk：仍在取消');
});
await t('danger 单独控制确认按钮颜色；okText/cancelText 生效', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',danger:true,okText:'删除',cancelText:'算了'})");
  eq(await q(okBtn), '删除'); eq(await q(cancelBtn), '算了');
  eq(await ev("return document.querySelector('.cf-foot .cf-btn:last-child').className.includes('cf-btn-danger')"), true);
  eq(await exists('.cf-badge'), false, 'level 非 high 无徽标');
});
await t('权限码 + 按钮元信息：显示按钮名称与高危标记', async () => {
  await reset();
  await ev("CF.setButtonMeta({'user.delete':{name:'删除用户',kind:'d'}})");
  await open("CF.riskConfirm({title:'T',code:'user.delete'})");
  const foot = await q('.cf-foot');
  truthy(foot.includes('删除用户') && foot.includes('高危操作'), foot);
});
await t('普通级别：无高危标记，遮罩点击可取消', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',code:'a.b'})");
  truthy(!(await q('.cf-foot')).includes('高危'), '无高危字样');
  await ev(CLICK_MASK);
  eq(await result(), { ok: false });
});
await t('高危：点击遮罩不关闭', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',level:'high'})");
  await ev(CLICK_MASK);
  eq(await result(), '__pending'); truthy(await exists('.cf-dialog'), '仍在');
});
await t('普通：在弹窗内按下、在遮罩上松开不误关', async () => {
  await reset(); await open("CF.riskConfirm({title:'T'})");
  await ev("document.querySelector('.cf-dialog').dispatchEvent(new MouseEvent('mousedown',{bubbles:true})); document.querySelector('.cf-mask').dispatchEvent(new MouseEvent('click',{bubbles:true}));");
  eq(await result(), '__pending');
});
await t('typed：不一致禁用，一致（区分大小写）启用，返回不含 typed', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',level:'high',typed:'Prod-DB',typedLabel:'请输入实例名'})");
  eq(await q(okBtn, 'disabled'), true, '初始禁用');
  eq(await ev('return document.activeElement.tagName'), 'INPUT', '焦点在键入框');
  await typeText('prod-db'); eq(await q(okBtn, 'disabled'), true, '大小写不同仍禁用');
  await input('.cf-input', '');
  await typeText('Prod-DB'); eq(await q(okBtn, 'disabled'), false, '一致后启用');
  await click(okBtn); eq(await result(), { ok: true });
});
await t('typed：禁用时点击确认无效，Enter 无效', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',typed:'x'})");
  await click(okBtn); eq(await result(), '__pending');
  await key('Enter'); eq(await result(), '__pending');
});
await t('typed 弹窗点击遮罩不关闭（防止丢失已键入内容）', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',typed:'x'})");
  await ev(CLICK_MASK); eq(await result(), '__pending');
});
await t('reason：必填 + 最小/最大长度 + 去首尾空白', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',reason:{label:'原因',required:true,min:5,max:10,placeholder:'写原因'}})");
  eq(await q(okBtn, 'disabled'), true, '空');
  eq(await q('.cf-textarea', 'placeholder'), '写原因');
  await typeText('abcd'); eq(await q(okBtn, 'disabled'), true, '4 < min');
  await typeText('e'); eq(await q(okBtn, 'disabled'), false, '5 满足');
  eq(await q('.cf-textarea', 'maxLength'), 10);
  await input('.cf-textarea', '   ');
  eq(await q(okBtn, 'disabled'), true, '纯空白视为空');
  await input('.cf-textarea', '  需要扩容  ');
  eq(await q(okBtn, 'disabled'), true, '去空白后 4 字 < 5');
  await input('.cf-textarea', '  需要扩容测试  ');
  await click(okBtn); eq(await result(), { ok: true, value: '需要扩容测试' });
});
await t('reason：非必填可空；填了但不足 min 则禁用；按码点计数', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',reason:{label:'备注',min:3}})");
  eq(await q(okBtn, 'disabled'), false, '空且非必填');
  await typeText('ab'); eq(await q(okBtn, 'disabled'), true, '填了不足 3');
  await input('.cf-textarea', '😀😀😀');
  eq(await q(okBtn, 'disabled'), false, '3 个码点');
});
await t('typed + reason 同时要求', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',level:'high',typed:'OK1',reason:{required:true,min:2}})");
  await typeText('OK1'); eq(await q(okBtn, 'disabled'), true, '缺原因');
  await ev("document.querySelector('.cf-textarea').focus()"); await typeText('原因'); eq(await q(okBtn, 'disabled'), false);
  await click(okBtn); eq(await result(), { ok: true, value: '原因' });
});
await t('details/lines/sql 渲染；SQL 为可滚动容器；有复制按钮', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',lines:['l1','l2'],details:[{k:'工单号',v:'1001'},{k:'实例',v:null}],sql:['select 1;','select 2;']})");
  eq(await ev("return document.querySelectorAll('.cf-lines p').length"), 2);
  eq(await ev("return [...document.querySelectorAll('.cf-details dt,.cf-details dd')].map(e=>e.textContent)"), ['工单号', '1001', '实例', '—']);
  eq(await q('.cf-sql'), 'select 1;\nselect 2;');
  eq(await ev("return getComputedStyle(document.querySelector('.cf-sql')).overflowY"), 'auto');
  truthy(await exists('.cf-sqlbar .cf-btn'), '复制按钮');
});
await t('文本不被当 HTML 解析（title/lines/details/sql/code/typed/reason/toast）', async () => {
  await reset();
  const evil = '<img src=x onerror="window.__pwned=1"><svg onload="window.__pwned=1"><script>window.__pwned=1</script>';
  const j = JSON.stringify(evil);
  await open(`CF.riskConfirm({title:${j},lines:[${j}],details:[{k:${j},v:${j}}],sql:${j},code:${j},typed:${j},typedLabel:${j},reason:{label:${j},placeholder:${j}}})`);
  await sleep(300);
  eq(await ev("return document.querySelectorAll('.cf-dialog img, .cf-dialog svg, .cf-dialog script').length"), 0, '不应产生 img/svg/script');
  eq(await ev('return window.__pwned === undefined'), true, '无副作用');
  truthy((await q('.cf-lines p')).includes('<img'), '以文本显示');
  await ev("CF.diffConfirm({title:'t',rows:[{label:" + j + ",from:" + j + ",to:'x'}]}).then(r=>{window.__r=r});await new Promise(r=>setTimeout(r,200));");
  eq(await ev("return document.querySelectorAll('.cf-dialog img, .cf-dialog svg, .cf-dialog script').length"), 0, 'diff 不解析 HTML');
  await reset();
  await ev(`window.CF.toast(${j}, 'err'); await new Promise(r=>setTimeout(r,100));`);
  eq(await ev("return document.querySelectorAll('.cf-toasts img, .cf-toasts svg').length"), 0, 'toast 不解析 HTML');
  eq(await ev('return window.__pwned === undefined'), true, '无副作用(2)');
});
await t('diffConfirm：无变化不弹窗；只列出有变化的行', async () => {
  await reset();
  eq(await ev("return await CF.diffConfirm({title:'T',rows:[{label:'a',from:1,to:'1'},{label:'b',from:null,to:''}]})"), { ok: false, noChange: true });
  eq(await exists('.cf-dialog'), false, '无弹窗');
  eq(await ev("return await CF.diffConfirm({title:'T',rows:[]})"), { ok: false, noChange: true }, '空 rows');
  await open("CF.diffConfirm({title:'T',rows:[{label:'same',from:'x',to:'x'},{label:'阈值',from:'10',to:'20'},{label:'备注',from:'',to:'新'}]})");
  eq(await ev("return [...document.querySelectorAll('.cf-diff tbody tr')].map(r=>[...r.children].map(c=>c.textContent))"), [['阈值', '10', '20'], ['备注', '（空）', '新']]);
  await click(okBtn); eq(await result(), { ok: true });
});
await t('diffConfirm：reason/typed/code/level 透传', async () => {
  await reset(); await open("CF.diffConfirm({title:'T',level:'high',code:'sysadmin.x',typed:'abc',reason:{required:true},rows:[{label:'a',from:1,to:2}]})");
  truthy((await q('.cf-foot')).includes('sysadmin.x'), '页脚'); truthy(await exists('.cf-textarea'), '原因框'); eq(await q(okBtn, 'disabled'), true);
});
await t('并发：后一个替换前一个，前一个 resolve ok:false', async () => {
  await reset();
  const r = await ev(`const p1 = CF.riskConfirm({title:'first'}); await new Promise(r=>setTimeout(r,80)); const p2 = CF.riskConfirm({title:'second'}); await new Promise(r=>setTimeout(r,80));
    const r1 = await p1; const n = document.querySelectorAll('.cf-dialog').length; const t = document.querySelector('.cf-title').textContent; document.querySelector('.cf-foot .cf-btn:last-child').click(); const r2 = await p2; return {r1, n, t, r2};`);
  eq(r.n, 1); eq(r.t, 'second'); eq(r.r1.ok, false); eq(r.r2, { ok: true });
});
await t('并发：同一事件循环连续调用只留最后一个', async () => {
  await reset();
  const r = await ev(`const a = CF.riskConfirm({title:'A'}); const b = CF.riskConfirm({title:'B'}); const c = CF.riskConfirm({title:'C'}); const ra = await a, rb = await b; await new Promise(r=>setTimeout(r,150));
    const n = document.querySelectorAll('.cf-dialog').length; const t = document.querySelector('.cf-title').textContent; document.querySelector('.cf-foot .cf-btn:first-child').click(); return {ra, rb, n, t, rc: await c};`);
  eq(r.n, 1); eq(r.t, 'C'); eq(r.ra.ok, false); eq(r.rb.ok, false); eq(r.rc, { ok: false });
});
await t('关闭后 html 滚动锁释放', async () => {
  await reset(); await open("CF.riskConfirm({title:'T'})");
  eq(await ev("return document.documentElement.classList.contains('cf-lock')"), true);
  await click(cancelBtn); await result();
  eq(await ev("return document.documentElement.classList.contains('cf-lock')"), false);
});
await t('confirmRun：成功 -> 执行期间转圈禁用且 Esc 无效 -> 关闭并返回 result/value', async () => {
  await reset();
  await ev(`window.__go = null; window.__r = undefined; CF.confirmRun({title:'T',reason:{required:true,min:2}}, (v) => new Promise((res) => { window.__go = res; window.__seen = v; })).then(r => { window.__r = r; }); await new Promise(r=>setTimeout(r,120));`);
  await ev("document.querySelector('.cf-textarea').focus()"); await typeText('原因X');
  await click(okBtn); await sleep(60);
  eq(await ev("const b=document.querySelector('.cf-foot .cf-btn:last-child'); return [b.disabled, b.classList.contains('cf-spin'), b.getAttribute('aria-busy'), document.querySelector('.cf-foot .cf-btn:first-child').disabled]"), [true, true, 'true', true]);
  await key('Escape'); eq(await result(), '__pending', '执行中 Esc 不关闭');
  await ev('window.__go({id: 7})');
  eq(await result(), { ok: true, result: { id: 7 }, value: '原因X' });
  eq(await ev('return window.__seen'), '原因X');
});
await t('confirmRun：失败显示在弹窗内且不关闭，可重试直至成功', async () => {
  await reset();
  await ev(`window.__n = 0; window.__r = undefined; CF.confirmRun({title:'T'}, async () => { window.__n++; if (window.__n < 3) throw new Error('boom ' + window.__n + ' <b>x</b>'); return 'done'; }).then(r => { window.__r = r; }); await new Promise(r=>setTimeout(r,120));`);
  eq(await q('.cf-err', 'hidden'), true, '初始隐藏');
  await click(okBtn); await sleep(100);
  eq(await q('.cf-err'), 'boom 1 <b>x</b>'); eq(await ev("return document.querySelectorAll('.cf-err b').length"), 0, '错误文本不解析 HTML');
  eq(await q('.cf-err', 'hidden'), false);
  eq(await q(okBtn, 'disabled'), false, '可重试'); eq(await q(cancelBtn, 'disabled'), false); eq(await result(), '__pending');
  await click(okBtn); await sleep(100); eq(await q('.cf-err'), 'boom 2 <b>x</b>');
  await click(okBtn); eq(await result(), { ok: true, result: 'done' });
  eq(await ev('return window.__n'), 3);
});
await t('confirmRun：取消不执行 asyncFn；rows 无变化不弹窗不执行', async () => {
  await reset();
  await ev('window.__called = 0');
  await open("CF.confirmRun({title:'T'}, async () => { window.__called++; })");
  await click(cancelBtn); eq(await result(), { ok: false }); eq(await ev('return window.__called'), 0);
  eq(await ev("return await CF.confirmRun({title:'T', rows:[{label:'a',from:1,to:1}]}, async () => { window.__called++; })"), { ok: false, noChange: true });
  eq(await ev('return window.__called'), 0);
});
await t('confirmRun：执行中被新弹窗替换 -> 前者 ok:false，后续失败改用 toast', async () => {
  await reset();
  const r = await ev(`let rej; const p1 = CF.confirmRun({title:'one'}, () => new Promise((_, r) => { rej = r; })); await new Promise(r=>setTimeout(r,80));
    document.querySelector('.cf-foot .cf-btn:last-child').click(); await new Promise(r=>setTimeout(r,50));
    const p2 = CF.riskConfirm({title:'two'}); await new Promise(r=>setTimeout(r,80));
    const r1 = await p1; rej(new Error('late fail')); await new Promise(r=>setTimeout(r,80));
    const t = document.querySelector('.cf-title').textContent; const toast = document.querySelector('.cf-toast-err')?.textContent; document.querySelector('.cf-foot .cf-btn:first-child').click(); await p2; return {r1, t, toast};`);
  eq(r.r1.ok, false); eq(r.t, 'two'); eq(r.toast, 'late fail');
});
await t('toast：三种类型、非法类型回落 ok、数量上限 4、点击关闭', async () => {
  await reset();
  await ev("CF.toast('a'); CF.toast('b','warn'); CF.toast('c','err'); CF.toast('d','bogus'); CF.toast('e'); CF.toast('f')");
  eq(await ev("return document.querySelectorAll('.cf-toast').length"), 4, '上限 4');
  eq(await ev("return [...document.querySelectorAll('.cf-toast')].map(e=>e.className.replace('cf-toast ',''))"), ['cf-toast-err', 'cf-toast-ok', 'cf-toast-ok', 'cf-toast-ok']);
  await ev("document.querySelector('.cf-toast').click()"); eq(await ev("return document.querySelectorAll('.cf-toast').length"), 3);
});
await t('皮肤：三套皮肤下弹窗背景互不相同（取自 --dt-panel）', async () => {
  await reset(); await open("CF.riskConfirm({title:'T'})");
  const bgs = [];
  for (const s of ['dark', 'light', 'sepia']) {
    await ev(`document.documentElement.setAttribute('data-skin','${s}')`);
    bgs.push(await ev("return getComputedStyle(document.querySelector('.cf-dialog')).backgroundColor"));
  }
  eq(new Set(bgs).size, 3, JSON.stringify(bgs));
  await ev("document.documentElement.setAttribute('data-skin','dark')");
});
await t('超长文本换行：弹窗与主体不产生横向溢出', async () => {
  await reset();
  const long = 'x'.repeat(600);
  await open(`CF.riskConfirm({title:'${'T'.repeat(200)}',lines:['${long}'],details:[{k:'k',v:'${long}'}],sql:'${long}',code:'${'c'.repeat(200)}'})`);
  eq(await ev("const d=document.querySelector('.cf-dialog').getBoundingClientRect(); return d.right <= innerWidth + 0.5 && document.documentElement.scrollWidth <= innerWidth + 1"), true);
  eq(await ev("const b=document.querySelector('.cf-body'); return b.scrollWidth <= b.clientWidth + 1"), true, '主体无横向溢出');
});
await t('桌面尺寸：输入框 >=16px，按钮高度 >=40px', async () => {
  await reset(); await open("CF.riskConfirm({title:'T',typed:'a',reason:{required:true}})");
  eq(await ev('return [...document.querySelectorAll(".cf-input")].every(e=>parseFloat(getComputedStyle(e).fontSize)>=16)'), true);
  eq(await ev('return [...document.querySelectorAll(".cf-foot .cf-btn")].every(e=>e.getBoundingClientRect().height>=40)'), true);
});
await t('手机宽度(375x700)：全屏、页脚吸底、按钮>=40px、输入框>=16px、无横向滚动', async () => {
  await send('Emulation.setDeviceMetricsOverride', { width: 375, height: 700, deviceScaleFactor: 1, mobile: true });
  await sleep(150);
  await reset();
  await open("CF.riskConfirm({title:'T',level:'high',lines:Array.from({length:40},(_, i)=>'line '+i),typed:'abc',code:'a.b',sql:'select 1'})");
  const m = await ev("const d=document.querySelector('.cf-dialog').getBoundingClientRect(), f=document.querySelector('.cf-foot').getBoundingClientRect(); return {w:d.width,h:d.height,iw:innerWidth,ih:innerHeight,footBottom:f.bottom,btnH:[...document.querySelectorAll('.cf-foot .cf-btn')].map(b=>b.getBoundingClientRect().height),fs:parseFloat(getComputedStyle(document.querySelector('.cf-input')).fontSize),sw:document.documentElement.scrollWidth}");
  const s = JSON.stringify(m);
  truthy(Math.abs(m.w - m.iw) < 1, '宽度全屏 ' + s); truthy(Math.abs(m.h - m.ih) < 2, '高度全屏 ' + s); truthy(Math.abs(m.footBottom - m.ih) < 2, '页脚吸底 ' + s);
  truthy(m.btnH.every((h) => h >= 40), '按钮高度 ' + s); truthy(m.fs >= 16, '字号 ' + s); truthy(m.sw <= m.iw, '无横向滚动 ' + s);
  await click(cancelBtn); await result();
  await send('Emulation.clearDeviceMetricsOverride');
});
await t('Esc 被组件拦截，不泄露给页面其他 keydown 监听', async () => {
  await reset();
  await ev("window.__esc = 0; document.addEventListener('keydown', (e) => { if (e.key === 'Escape') window.__esc++; });");
  await open("CF.riskConfirm({title:'T'})");
  await key('Escape'); await result();
  eq(await ev('return window.__esc'), 0);
});
await t('严格 CSP（style-src self）下无违规 / 页面无异常', async () => {
  eq(consoleErrors.filter((e) => !e.includes('favicon.ico')), []);
});

console.log(`\n通过 ${pass}，失败 ${fail}`);
ws.close();
cleanup();
process.exit(fail ? 1 : 0);
