/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: chart.js
 * @time: 2026/10/28
 */
// 批次吞吐曲线（内联 SVG，不依赖第三方库）：折线 = 每个采样点的行/秒，红色带 = 被限流暂停的时段，虚线 = 当前批大小。
import { t } from '../i18n.js';
import { el, fmtDur, nf } from './util.js';

const NS = 'http://www.w3.org/2000/svg';
function s(tag, attrs, text) {
  const e = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, String(v));
  if (text !== undefined) e.textContent = text;
  return e;
}

// series: [{t_ms, rows_per_sec, batch_size, throttled}]
export function throughputChart(series) {
  const wrap = el('div');
  if (!series || !series.length) {
    wrap.append(el('div', 'muted small', t('没有吞吐数据。')));
    return wrap;
  }
  const W = 640;
  const H = 170;
  const pl = 52;
  const pr = 10;
  const pt = 10;
  const pb = 24;
  const n = series.length;
  const maxV = Math.max(...series.map((p) => p.rows_per_sec), 1) * 1.12;
  const maxB = Math.max(...series.map((p) => p.batch_size), 1) * 1.12;
  const t0 = series[0].t_ms;
  const t1 = Math.max(series[n - 1].t_ms, t0 + 1);
  const X = (i) => pl + ((W - pl - pr) * (series[i].t_ms - t0)) / (t1 - t0);
  const Y = (v) => pt + (H - pt - pb) * (1 - v / maxV);
  const Yb = (v) => pt + (H - pt - pb) * (1 - v / maxB);

  const svg = s('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': t('批次吞吐曲线') });
  svg.style.cssText = 'width:100%;height:auto';
  for (let k = 0; k <= 4; k++) {
    const v = (maxV * k) / 4;
    const y = Y(v);
    svg.append(
      s('line', { x1: pl, x2: W - pr, y1: y.toFixed(1), y2: y.toFixed(1), stroke: 'var(--dt-line)' }),
      s('text', { x: pl - 6, y: (y + 4).toFixed(1), 'text-anchor': 'end', 'font-size': 10, fill: 'var(--dt-muted)' }, nf(Math.round(v))),
    );
  }
  // 被限流的时段：连续 throttled 采样点合并成一个色带
  let i = 0;
  while (i < n) {
    if (!series[i].throttled) { i++; continue; }
    let j = i;
    while (j + 1 < n && series[j + 1].throttled) j++;
    const x0 = X(i) - 1;
    const x1 = j + 1 < n ? X(j + 1) : X(j) + 6;
    const r = s('rect', { x: x0.toFixed(1), y: pt, width: Math.max(6, x1 - x0).toFixed(1), height: H - pt - pb, fill: 'var(--dt-crit)', opacity: '.16' });
    r.append(s('title', {}, t('被限流暂停（约 {dur}）', { dur: fmtDur(series[Math.min(j + 1, n - 1)].t_ms - series[i].t_ms) })));
    svg.append(r);
    i = j + 1;
  }
  const line = series.map((p, k) => (k ? 'L' : 'M') + X(k).toFixed(1) + ' ' + Y(p.rows_per_sec).toFixed(1)).join('');
  const bl = series.map((p, k) => (k ? 'L' : 'M') + X(k).toFixed(1) + ' ' + Yb(p.batch_size).toFixed(1)).join('');
  svg.append(
    s('path', { d: `${line} L${X(n - 1).toFixed(1)} ${Y(0).toFixed(1)} L${X(0).toFixed(1)} ${Y(0).toFixed(1)} Z`, fill: 'var(--dt-cyan)', opacity: '.1' }),
    s('path', { d: line, fill: 'none', stroke: 'var(--dt-cyan)', 'stroke-width': 1.8 }),
    s('path', { d: bl, fill: 'none', stroke: 'var(--dt-amber)', 'stroke-width': 1.3, 'stroke-dasharray': '4 3' }),
  );
  for (let k = 0; k <= 4; k++) {
    svg.append(s('text', { x: (pl + ((W - pl - pr) * k) / 4).toFixed(1), y: H - 6, 'text-anchor': k === 0 ? 'start' : k === 4 ? 'end' : 'middle', 'font-size': 10, fill: 'var(--dt-muted)' }, fmtDur(((t1 - t0) * k) / 4)));
  }
  wrap.append(svg);

  const legend = el('div', 'small muted');
  legend.style.cssText = 'display:flex;gap:16px;flex-wrap:wrap';
  const item = (color, text, op) => {
    const sp = el('span');
    const i = el('i');
    i.style.cssText = `display:inline-block;width:10px;height:10px;margin-right:4px;border-radius:2px;background:${color};opacity:${op || 1}`;
    sp.append(i, document.createTextNode(text));
    return sp;
  };
  legend.append(item('var(--dt-cyan)', t('吞吐（行/秒）')), item('var(--dt-amber)', t('批大小（自适应）')), item('var(--dt-crit)', t('被限流暂停'), 0.4));
  wrap.append(legend);
  return wrap;
}
