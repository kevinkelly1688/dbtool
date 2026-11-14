/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: clock.js
 * @time: 2026/10/28
 */
// 顶栏时钟：浏览器本地时间，每秒刷新，格式「2026年10月12日 15:42:36」。页面不可见时暂停，回到前台立即补一次。
import { t } from '../i18n.js';

const box = document.getElementById('tb-clock');
if (box) {
  const two = (n) => String(n).padStart(2, '0');
  const paint = () => {
    const d = new Date();
    box.textContent = t('{y}年{m}月{d}日 {time}', {
      y: d.getFullYear(), m: d.getMonth() + 1, d: d.getDate(), time: `${two(d.getHours())}:${two(d.getMinutes())}:${two(d.getSeconds())}`,
    });
    box.dateTime = d.toISOString();
  };
  let timer = null;
  const start = () => { paint(); if (!timer) timer = setInterval(paint, 1000); };
  const stop = () => { if (timer) { clearInterval(timer); timer = null; } };
  document.addEventListener('visibilitychange', () => (document.hidden ? stop() : start()));
  start();
}
