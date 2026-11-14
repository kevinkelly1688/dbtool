/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: watermark_test.mjs
 * @time: 2026/10/28
 */
// 水印篡改判定与上报（防抖）的断言测试：node scripts/frontend-test/watermark_test.mjs
import assert from 'node:assert/strict';

// ---- 最小环境：假 document / fetch / 定时器 / 时钟 ----
let now = 1_000_000;
Date.now = () => now;
const timers = [];
globalThis.setTimeout = (fn, ms) => { timers.push({ fn, ms }); return timers.length; };
globalThis.clearTimeout = () => {};
globalThis.setInterval = () => 0;
globalThis.clearInterval = () => {};
const calls = [];
let failNext = false;
globalThis.fetch = async (url, init) => {
  if (init && init.method === 'POST') {
    calls.push({ url, body: JSON.parse(init.body), headers: init.headers });
    if (failNext) return { ok: false, status: 429, json: async () => ({ code: 'too_many_requests', message: 'x' }) };
    return { ok: true, status: 200, json: async () => ({ data: { ok: true } }) };
  }
  return { ok: false, status: 401, json: async () => null }; // /auth/ui：未登录，水印保持关闭
};
const noop = () => [];
globalThis.document = { addEventListener() {}, querySelector: () => ({ content: 'tok' }), querySelectorAll: noop, getElementById: () => null, documentElement: { getAttribute: (k) => (k === 'lang' ? 'zh-CN' : 'dark') }, body: {} };
globalThis.MutationObserver = class { observe() {} disconnect() {} }; // common.js → searchselect.js 加载时会用到
globalThis.localStorage = { getItem: () => null, setItem() {} };

const { classifyTamper, reportTamper } = await import('../../web/static/js/auth/watermark.js');

// ---- classifyTamper ----
const layer = (o = {}) => ({ isConnected: true, hidden: false, style: {}, getAttribute: () => 'position: fixed;', ...o });
assert.equal(classifyTamper(null, ''), 'removed');
assert.equal(classifyTamper(layer({ isConnected: false }), 'position: fixed;'), 'removed');
assert.equal(classifyTamper(layer(), 'position: fixed;'), '');
assert.equal(classifyTamper(layer({ hidden: true }), 'position: fixed;'), 'hidden');
assert.equal(classifyTamper(layer({ style: { display: 'none' } }), 'position: fixed;'), 'hidden');
assert.equal(classifyTamper(layer({ style: { visibility: 'hidden' } }), 'position: fixed;'), 'hidden');
assert.equal(classifyTamper(layer({ style: { opacity: '0' } }), 'position: fixed;'), 'hidden');
assert.equal(classifyTamper(layer({ getAttribute: () => 'position: fixed; z-index: 1;' }), 'position: fixed;'), 'style');
// 隐藏优先于样式改动（display:none 也会让 style 属性变）
assert.equal(classifyTamper(layer({ style: { display: 'none' }, getAttribute: () => 'display: none;' }), 'position: fixed;'), 'hidden');

// ---- reportTamper：只认枚举；防抖；间隔；失败静默 ----
const flush = async () => { const t = timers.splice(0); for (const x of t) await x.fn(); };
reportTamper('bogus');
reportTamper('');
reportTamper(undefined);
assert.equal(timers.length, 0, '非法类型不应排队');

reportTamper('style');
reportTamper('removed'); // 同一窗口内的后续事件合并，取最先发现的
reportTamper('hidden');
assert.equal(timers.length, 1, '防抖：同一窗口只排一个定时器');
assert.ok(timers[0].ms >= 800 && timers[0].ms <= 30000);
await flush();
assert.equal(calls.length, 1);
assert.equal(calls[0].url, '/api/v1/auth/watermark-tamper');
assert.deepEqual(calls[0].body, { kind: 'style' }, 'body 只有 kind');
assert.equal(calls[0].headers['X-CSRF-Token'], 'tok');

// 紧接着再发生：要等满间隔
now += 1000;
reportTamper('removed');
assert.equal(timers.length, 1);
assert.ok(timers[0].ms > 20000, '两次上报至少隔 30 秒: ' + timers[0].ms);
now += 30000;
await flush();
assert.equal(calls.length, 2);
assert.deepEqual(calls[1].body, { kind: 'removed' });

// 失败（被限流 / 未登录）不抛错，也不影响后续
failNext = true;
now += 31000;
reportTamper('hidden');
await flush();
assert.equal(calls.length, 3);
failNext = false;
now += 31000;
reportTamper('hidden');
await flush();
assert.equal(calls.length, 4);

console.log('watermark ok');
process.exit(0);
