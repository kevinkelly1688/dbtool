/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: poller.js
 * @time: 2026/10/28
 */
// 运行进度轮询：有视图需要时（任务列表有运行中任务、详情抽屉 / 运行详情打开）每 2 秒拉一次 /progress，分发给订阅者。
// 页面不可见时暂停；没有人需要时自动停止。
import { A } from './api.js';

const INTERVAL = 2000;
const listeners = new Set();
let need = 0;
let timer = 0;
let busy = false;
let last = null; // 最近一次结果（数组），null = 还没取到

async function tick() {
  if (busy || document.hidden || need <= 0) return;
  busy = true;
  try {
    last = await A.progress();
    for (const fn of [...listeners]) {
      try { fn(last); } catch (e) { console.error(e); }
    }
  } catch (_) {
    /* 网络抖动 / 会话过期：下个周期再试，页面其余部分不受影响 */
  } finally {
    busy = false;
  }
}

function sync() {
  if (need > 0 && !timer) {
    timer = setInterval(tick, INTERVAL);
    tick();
  } else if (need <= 0 && timer) {
    clearInterval(timer);
    timer = 0;
  }
}

// acquire 返回 release 函数；同一视图重复 acquire 会累加，必须成对释放。
export function acquire() {
  need += 1;
  sync();
  let done = false;
  return () => {
    if (done) return;
    done = true;
    need = Math.max(0, need - 1);
    sync();
  };
}

export function subscribe(fn) {
  listeners.add(fn);
  if (last) fn(last);
  return () => listeners.delete(fn);
}

export function latest() {
  return last;
}

export function byTask(list, taskId) {
  return (list || []).find((p) => p.task_id === taskId) || null;
}

export function byRun(list, runId) {
  return (list || []).find((p) => p.run_id === runId) || null;
}

document.addEventListener('visibilitychange', () => {
  if (!document.hidden) tick();
});
