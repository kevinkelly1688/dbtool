/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: slowlogs_chart.js
 * @time: 2026/10/28
 */
// 查慢日志的每日折线图（原生 SVG，无第三方库）。样式见 slowlog_logs.css 的 svg.slchart。
import { t } from '../i18n.js';
import { fmtNum } from './lib.js';

const SVG_NS = 'http://www.w3.org/2000/svg';

function svg(tag, attrs = {}, text) {
  const e = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, String(v));
  if (text !== undefined) e.textContent = text;
  return e;
}

// 取一个“好看”的纵轴上限：1 / 2 / 5 × 10^n。
export function niceMax(v) {
  if (!(v > 0)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  const m = v / p;
  const n = m <= 1 ? 1 : m <= 2 ? 2 : m <= 5 ? 5 : 10;
  return n * p;
}

// 画折线图。points: [{ label: 'YYYY-MM-DD', value: number }]，opts: { cls: 'c1' | 'c2', name, fmt(value) -> string, ariaLabel }。
export function lineChart(points, opts = {}) {
  const fmt = opts.fmt || ((v) => fmtNum(v));
  const W = 520, H = 170, L = 46, R = 12, T = 12, B = 26;
  const root = svg('svg', { viewBox: `0 0 ${W} ${H}`, class: 'slchart ' + (opts.cls || 'c1'), role: 'img', 'aria-label': opts.ariaLabel || opts.name || t('折线图') });
  if (!points.length) {
    root.append(svg('text', { x: W / 2, y: H / 2, 'text-anchor': 'middle', class: 'ax' }, t('该时间范围内没有数据')));
    return root;
  }
  const pw = W - L - R, ph = H - T - B;
  const top = niceMax(Math.max(...points.map((p) => p.value), 0));
  const x = (i) => (points.length === 1 ? L + pw / 2 : L + (pw * i) / (points.length - 1));
  const y = (v) => T + ph - (Math.min(v, top) / top) * ph;

  // 横向参考线与纵轴刻度（0 / 1/2 / 上限）
  for (const f of [0, 0.5, 1]) {
    const yy = T + ph - f * ph;
    root.append(svg('line', { x1: L, y1: yy, x2: W - R, y2: yy, class: f === 0 ? 'axis' : 'grid' }));
    root.append(svg('text', { x: L - 5, y: yy + 3, 'text-anchor': 'end', class: 'ax' }, fmt(top * f)));
  }
  const coords = points.map((p, i) => [x(i), y(p.value)]);
  if (points.length > 1) {
    const line = coords.map((c) => c.map((v) => v.toFixed(1)).join(',')).join(' ');
    root.append(svg('polygon', { class: 'area', points: `${L},${T + ph} ${line} ${x(points.length - 1).toFixed(1)},${T + ph}` }));
    root.append(svg('polyline', { class: 'ln', points: line }));
  }
  // 点：天数不多时画出小圆点；每个点上叠一个大一点的透明圆承载悬停提示
  const showDots = points.length <= 62;
  points.forEach((p, i) => {
    if (showDots) root.append(svg('circle', { class: 'dot', cx: coords[i][0].toFixed(1), cy: coords[i][1].toFixed(1), r: 2.5 }));
    const hit = svg('circle', { class: 'hit', cx: coords[i][0].toFixed(1), cy: coords[i][1].toFixed(1), r: 7 });
    hit.append(svg('title', {}, `${p.label}\n${t('{name}：{value}', { name: opts.name || t('数值'), value: fmt(p.value) })}`));
    root.append(hit);
  });
  // 横轴：首、1/4、中、3/4、尾（去掉年份）
  const last = points.length - 1;
  const idx = [...new Set([0, Math.round(last / 4), Math.round(last / 2), Math.round((last * 3) / 4), last])];
  for (const i of idx) {
    const anchor = i === 0 ? 'start' : i === last ? 'end' : 'middle';
    root.append(svg('text', { x: x(i), y: H - 8, 'text-anchor': anchor, class: 'ax' }, points[i].label.slice(5)));
  }
  return root;
}
