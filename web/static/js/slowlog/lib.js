/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: lib.js
 * @time: 2026/10/28
 */
// 慢日志两个页面共用的小工具：DOM、格式化、日期范围、分页、趋势图（原生 SVG）。
import { t } from '../i18n.js';

export { api } from '../common.js';

export const $ = (id) => document.getElementById(id);

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

export function setMsg(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

// 大数缩写：1234567 -> 1.2M。
export function fmtNum(n) {
  n = Number(n) || 0;
  const a = Math.abs(n);
  if (a >= 1e9) return (n / 1e9).toFixed(1) + 'B';
  if (a >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (a >= 1e4) return (n / 1e3).toFixed(0) + 'K';
  return String(Math.round(n));
}

export const fmtSec = (n) => (Number(n) || 0).toFixed(3).replace(/\.?0+$/, '') || '0';

export function ymd(d) {
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

// 最近 days 天（含今天）。
export function lastDays(days) {
  const end = new Date();
  const start = new Date();
  start.setDate(end.getDate() - (days - 1));
  return [ymd(start), ymd(end)];
}

// 后端 time.Time 序列化成 ISO 字符串；零值显示为 —。
export function fmtTime(s) {
  if (!s || s.startsWith('0001-01-01')) return '—';
  return s.slice(0, 19).replace('T', ' ');
}

export function qs(obj) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(obj)) {
    if (v !== undefined && v !== null && v !== '') p.set(k, String(v));
  }
  return p.toString();
}

const MAX_OFFSET = 20000; // 与服务端 MaxOffset 一致

// 渲染分页条。go(offset) 由调用方实现。
export function renderPager(box, { total, offset, limit }, go) {
  box.replaceChildren();
  const page = Math.floor(offset / limit) + 1;
  const pages = Math.max(1, Math.ceil(total / limit));
  box.append(el('span', '', t('共 {total} 条，第 {page} / {pages} 页', { total, page, pages })), el('span', 'spacer'));
  const prev = el('button', 'btn', t('上一页'));
  prev.type = 'button';
  prev.disabled = offset <= 0;
  prev.addEventListener('click', () => go(Math.max(0, offset - limit)));
  const next = el('button', 'btn', t('下一页'));
  next.type = 'button';
  next.disabled = offset + limit >= total || offset + limit > MAX_OFFSET;
  next.addEventListener('click', () => go(offset + limit));
  box.append(prev, next);
  if (offset + limit < total && offset + limit > MAX_OFFSET) {
    box.append(el('span', 'muted small', t('翻页已到上限，请缩小查询范围')));
  }
}

// 可排序表头：th 上写 data-sort；state.sort / state.order 保存当前排序。
export function bindSort(thead, state, reload) {
  const mark = () => {
    for (const th of thead.querySelectorAll('th[data-sort]')) {
      const on = th.dataset.sort === state.sort;
      th.setAttribute('aria-sort', on ? (state.order === 'asc' ? 'ascending' : 'descending') : 'none');
      th.dataset.arrow = on ? (state.order === 'asc' ? ' ▲' : ' ▼') : '';
    }
  };
  for (const th of thead.querySelectorAll('th[data-sort]')) {
    th.classList.add('sortable');
    th.tabIndex = 0;
    const fire = () => {
      if (state.sort === th.dataset.sort) state.order = state.order === 'asc' ? 'desc' : 'asc';
      else { state.sort = th.dataset.sort; state.order = 'desc'; }
      state.offset = 0;
      mark();
      reload();
    };
    th.addEventListener('click', fire);
    th.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); fire(); } });
  }
  mark();
}

export function emptyRow(cols, text) {
  const tr = el('tr');
  const td = el('td', 'empty', text);
  td.colSpan = cols;
  tr.append(td);
  return tr;
}

const SVG_NS = 'http://www.w3.org/2000/svg';
function svg(tag, attrs = {}, text) {
  const e = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, String(v));
  if (text !== undefined) e.textContent = text;
  return e;
}

// 趋势图：柱 = 慢查次数（左轴），线 = 95% 耗时（右轴，秒）。样式见 admin.css 的 svg.trend。
export function trendChart(points) {
  const W = 520, H = 150, L = 44, R = 44, T = 12, B = 26;
  const root = svg('svg', { viewBox: `0 0 ${W} ${H}`, class: 'trend', role: 'img', 'aria-label': t('慢查次数与 95% 耗时趋势') });
  if (!points.length) {
    root.append(svg('text', { x: W / 2, y: H / 2, 'text-anchor': 'middle', class: 'ax' }, t('该时间范围内没有数据')));
    return root;
  }
  const pw = W - L - R, ph = H - T - B;
  const maxC = Math.max(...points.map((p) => p.count), 1);
  const maxP = Math.max(...points.map((p) => p.pct95), 0.000001);
  const slot = pw / points.length;
  const bw = Math.max(2, Math.min(26, slot * 0.6));
  const bars = svg('g', { class: 'bars' });
  const pts = [];
  points.forEach((p, i) => {
    const cx = L + slot * (i + 0.5);
    const h = (p.count / maxC) * ph;
    const r = svg('rect', { x: cx - bw / 2, y: T + ph - h, width: bw, height: Math.max(h, 1), rx: 1 });
    r.append(svg('title', {}, [p.t, t('慢查 {count} 次', { count: p.count }), t('95% 耗时 {v} s', { v: fmtSec(p.pct95) }),
      t('平均 {avg} s，最大 {max} s', { avg: fmtSec(p.avg), max: fmtSec(p.max) })].join('\n')));
    bars.append(r);
    pts.push([cx, T + ph - (p.pct95 / maxP) * ph]);
  });
  root.append(svg('line', { x1: L, y1: T + ph, x2: W - R, y2: T + ph, class: 'axis' }), bars);
  if (pts.length > 1) root.append(svg('polyline', { class: 'line', points: pts.map((q) => q.map((v) => v.toFixed(1)).join(',')).join(' ') }));
  else root.append(svg('circle', { class: 'dotpt', cx: pts[0][0], cy: pts[0][1], r: 3 }));
  root.append(svg('text', { x: L - 4, y: T + 8, 'text-anchor': 'end', class: 'ax' }, fmtNum(maxC)));
  root.append(svg('text', { x: L - 4, y: T + ph, 'text-anchor': 'end', class: 'ax' }, '0'));
  root.append(svg('text', { x: W - R + 4, y: T + 8, class: 'ax' }, fmtSec(maxP) + 's'));
  const label = (ts) => ts.slice(5); // 去掉年份：日 MM-DD，小时 MM-DD HH:00
  const idx = [...new Set([0, Math.floor((points.length - 1) / 2), points.length - 1])];
  for (const i of idx) {
    root.append(svg('text', { x: L + slot * (i + 0.5), y: H - 8, 'text-anchor': i === 0 ? 'start' : i === points.length - 1 ? 'end' : 'middle', class: 'ax' }, label(points[i].t)));
  }
  return root;
}
