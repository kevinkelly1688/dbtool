/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: trend.js
 * @time: 2026/10/28
 */
// 同一实例的巡检趋势折线图（纯 SVG，无第三方库）。
// MySQL 系报告有健康评分，画评分折线（90 / 70 两条参考线）；SQL Server 报告没有评分，画告警数与关注数两条折线。
// 数据来自报告列表接口（reports?instance_id=…），只取已完成的报告，按开始时间从旧到新。
import { t } from '../i18n.js';

const NS = 'http://www.w3.org/2000/svg';

function svg(tag, attrs = {}, text) {
  const e = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, String(v));
  if (text !== undefined) e.textContent = text;
  return e;
}

// 有健康评分的报告种类（MySQL 系与 Redis）：趋势画评分；其余画告警 / 关注数
export const isScoreKind = (kind) => kind === 'mysql' || kind === 'redis';

// 取出要画的点：只要已完成的报告，旧 → 新，最多 limit 个。MySQL 系没有评分的报告（评分为空）跳过。
export function trendPoints(rows, kind, limit = 30) {
  const done = rows.filter((r) => r.status === 'finish' && r.kind === kind && (!isScoreKind(kind) || (r.health_score !== null && r.health_score !== undefined)));
  done.sort((a, b) => (a.start_time < b.start_time ? -1 : a.start_time > b.start_time ? 1 : 0));
  return done.slice(-limit).map((r) => ({
    ref: r.ref, time: r.start_time, score: r.health_score, alert: r.alert, attention: r.attention,
  }));
}

// 把值域映射到画布：返回 x(i) / y(v)。
export function scales(n, vmax, w, h, pad) {
  const iw = w - pad.l - pad.r;
  const ih = h - pad.t - pad.b;
  return {
    x: (i) => pad.l + (n <= 1 ? iw / 2 : (iw * i) / (n - 1)),
    y: (v) => pad.t + ih - (ih * Math.min(Math.max(v, 0), vmax)) / (vmax || 1),
  };
}

const label = (ts) => (ts ? ts.slice(5, 16).replace('T', ' ') : '');

// 画到 box 里。curRef 是当前报告（高亮）；点击某个点跳到那份报告。
export function renderTrend(box, points, kind, curRef) {
  box.replaceChildren();
  if (points.length < 2) {
    const p = document.createElement('p');
    p.className = 'muted small';
    p.textContent = t('已完成的报告不足 2 份，暂无趋势');
    box.append(p);
    return;
  }
  const W = 720;
  const H = 200;
  const pad = { l: 40, r: 16, t: 14, b: 30 };
  const isScore = isScoreKind(kind);
  const vmax = isScore ? 100 : Math.max(5, ...points.map((p) => Math.max(p.alert, p.attention)));
  const s = scales(points.length, vmax, W, H, pad);
  const root = svg('svg', { class: 'trend', viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': isScore ? t('健康评分趋势') : t('告警与关注数趋势') });

  // 横向网格 + 纵轴刻度
  const ticks = isScore ? [0, 50, 70, 90, 100] : [0, Math.round(vmax / 2), vmax];
  for (const tk of ticks) {
    const ref = isScore && (tk === 70 || tk === 90);
    root.append(svg('line', { class: ref ? 'trend-ref' : 'trend-grid', x1: pad.l, x2: W - pad.r, y1: s.y(tk), y2: s.y(tk) }));
    root.append(svg('text', { class: 'trend-lbl', x: pad.l - 6, y: s.y(tk) + 4, 'text-anchor': 'end' }, String(tk)));
  }
  // 横轴：首、中、尾三个时间
  const idx = [0, Math.floor((points.length - 1) / 2), points.length - 1].filter((v, i, a) => a.indexOf(v) === i);
  for (const i of idx) root.append(svg('text', { class: 'trend-lbl', x: s.x(i), y: H - 10, 'text-anchor': i === 0 ? 'start' : i === points.length - 1 ? 'end' : 'middle' }, label(points[i].time)));

  const series = isScore
    ? [{ cls: 'score', get: (p) => p.score, name: t('健康评分') }]
    : [{ cls: 'alert', get: (p) => p.alert, name: t('告警') }, { cls: 'attention', get: (p) => p.attention, name: t('关注') }];
  for (const se of series) {
    const d = points.map((p, i) => `${i ? 'L' : 'M'}${s.x(i).toFixed(1)} ${s.y(se.get(p)).toFixed(1)}`).join(' ');
    root.append(svg('path', { class: 'trend-line ' + se.cls, d }));
    points.forEach((p, i) => {
      const a = svg('a', { href: '/inspection/' + p.ref });
      const dot = svg('circle', { class: 'trend-dot ' + se.cls + (p.ref === curRef ? ' cur' : ''), cx: s.x(i), cy: s.y(se.get(p)), r: p.ref === curRef ? 5 : 3 });
      const tip = `${p.time.slice(0, 16).replace('T', ' ')} · ${se.name} ${se.get(p)}`;
      dot.append(svg('title', {}, p.ref === curRef ? t('{tip}（当前报告）', { tip }) : tip));
      a.append(dot);
      root.append(a);
    });
  }
  box.append(root);
}
