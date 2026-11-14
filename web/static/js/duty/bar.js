/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: bar.js
 * @time: 2026/10/28
 */
// 值班表的小部件：一天 24 小时的时间条（顶栏浮层和管理页共用）、适用星期的文字。
// 所有往 DOM 写的内容都用 textContent / 属性，不拼 HTML。
import { t } from '../i18n.js';

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

// 页面自己定时取数用（顶栏芯片、管理页的每分钟刷新）：带“后台轮询”标记，服务端不会因此给会话续期
// （见 server.BackgroundPollHeader），否则一个没人看的页面会让登录永不过期。出错时抛出带 status 的 Error，格式同 common.js 的 api()。
export async function poll(url) {
  const res = await fetch(url, { headers: { Accept: 'application/json', 'X-Background-Poll': '1' }, credentials: 'same-origin' });
  let payload = null;
  try { payload = await res.json(); } catch (_) { /* 非 JSON 响应 */ }
  if (!res.ok) {
    const err = new Error((payload && payload.message) || `HTTP ${res.status}`);
    err.status = res.status;
    err.code = (payload && payload.code) || 'http_' + res.status;
    throw err;
  }
  return payload ? payload.data : null;
}

// 星期名：0 周日 … 6 周六
export const dayName = (d) => [t('周日'), t('周一'), t('周二'), t('周三'), t('周四'), t('周五'), t('周六')][d];

// 每周从周一排起（国内习惯），周日放最后
export const WEEK_ORDER = [1, 2, 3, 4, 5, 6, 0];

// 适用星期的位图 → 文字：每天 / 工作日 / 周末 / 周一、周三
export function daysText(mask) {
  if (mask === 127) return t('每天');
  if (mask === 0x3e) return t('工作日');
  if (mask === 0x41) return t('周末');
  return WEEK_ORDER.filter((d) => mask & (1 << d)).map(dayName).join(t('、|sep'));
}

// 一段时间的文字：08:00–19:00
export const rangeText = (seg) => `${seg.start}–${seg.end}`;

// 一段在时间条上的提示文字
export function segTitle(seg) {
  const who = seg.gap ? t('无人值班') : seg.display + (seg.name ? ` · ${seg.name}` : seg.kind === 'override' ? ` · ${t('调班')}` : '');
  return `${rangeText(seg)}  ${who}`;
}

// 给每个人分一个颜色序号（按首次出现的顺序），同一个人颜色一致
export function personIndex(segLists) {
  const idx = new Map();
  for (const segs of segLists) {
    for (const g of segs) {
      if (!g.gap && g.user && !idx.has(g.user)) idx.set(g.user, idx.size);
    }
  }
  return idx;
}

// 24 小时时间条：segs 是一天里的各段（s/e 为当天第几分钟）；nowMin 不为空时画一条“现在”的竖线。
// opts.colors：Map(user → 序号)，给了就按人上色；不给就只高亮“当前”这一段（顶栏浮层用）。
// opts.minLabel：短于这么多分钟的段不写名字（放不下，鼠标放上去有提示）；默认 100 分钟（浮层只有 300 来像素宽）。
export function dayBar(segs, nowMin, opts = {}) {
  const bar = el('div', 'dv-bar');
  const minLabel = opts.minLabel === undefined ? 100 : opts.minLabel;
  for (const g of segs) {
    const cls = ['dv-seg'];
    if (g.gap) cls.push('gap');
    if (g.cur) cls.push('cur');
    if (g.past) cls.push('past');
    if (g.kind === 'override') cls.push('swap');
    if (opts.colors && !g.gap && opts.colors.has(g.user)) cls.push('p' + (opts.colors.get(g.user) % 4));
    const seg = el('div', cls.join(' '), g.gap || g.e - g.s < minLabel ? '' : g.display);
    seg.style.flexGrow = String(Math.max(1, g.e - g.s));
    seg.title = segTitle(g);
    bar.append(seg);
  }
  if (nowMin !== null && nowMin !== undefined) {
    const mark = el('i', 'dv-now');
    mark.style.left = `${Math.min(100, Math.max(0, (nowMin / 1440) * 100))}%`;
    mark.title = t('现在');
    bar.append(mark);
  }
  return bar;
}

// 时间条下面的刻度：00:00 / 06:00 / 12:00 / 18:00 / 24:00
export function dayTicks() {
  const ticks = el('div', 'dv-ticks');
  for (const h of [0, 6, 12, 18, 24]) {
    const s = el('span', '', String(h).padStart(2, '0') + ':00');
    s.style.left = `${(h / 24) * 100}%`;
    ticks.append(s);
  }
  return ticks;
}
