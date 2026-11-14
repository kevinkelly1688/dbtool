/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: board.js
 * @time: 2026/10/28
 */
// 团队排行榜：团队健康度表格、近 12 周新增/完成柱图、未关闭问题类型环图（均为原生 SVG）。
import { api } from '../common.js';
import { t, tp } from '../i18n.js';
import { $, el, svg, fmtNum, fmtSeconds, setMsg, qs } from './util.js';

const msg = $('pb-msg');

function kpi(v, label, cls) {
  const c = el('div');
  c.append(el('b', cls || '', v), el('span', '', label));
  return c;
}

function renderKpis(o) {
  $('pb-kpis').replaceChildren(
    kpi(fmtNum(o.open), t('未关闭工单')), kpi(fmtNum(o.overdue), t('已超时'), o.overdue ? 'crit' : ''), kpi(fmtNum(o.new), t('期间新增')),
    kpi(fmtNum(o.closed), t('期间优化完成'), 'ok'), kpi((o.waste_sec_per_day / 3600).toFixed(1) + 'h', t('未关闭问题 每天浪费 DB 时间'), o.waste_sec_per_day ? 'crit' : ''),
    kpi((o.saved_sec_per_day / 3600).toFixed(1) + 'h', t('期间优化 每天节省 DB 时间'), 'ok'),
  );
}

function renderTeams(teams) {
  const body = $('pb-rows');
  body.replaceChildren();
  if (!teams.length) {
    const td = el('td', 'empty', t('没有统计数据。'));
    td.colSpan = 12;
    const tr = el('tr');
    tr.append(td);
    body.append(tr);
    return;
  }
  for (const tm of teams) {
    const tr = el('tr');
    tr.style.cursor = 'pointer';
    tr.addEventListener('click', () => { location.href = '/perf'; });
    const score = el('td');
    score.append(el('span', 'score ' + (tm.score >= 80 ? 'good' : tm.score >= 50 ? 'mid' : 'bad'), String(tm.score)));
    const over = el('td', 'num');
    over.append(tm.overdue ? el('b', 'crit', String(tm.overdue)) : document.createTextNode('0'));
    tr.append(el('td', '', String(tm.rank)), el('td', '', tm.team), score, over, el('td', 'num', fmtNum(tm.not_accepted)), el('td', 'num', fmtNum(tm.open)),
      el('td', 'num', fmtNum(tm.p0p1_open)), el('td', 'num', fmtNum(tm.new)), el('td', 'num', fmtNum(tm.closed)),
      el('td', 'num', tm.sla_rate === null ? '-' : tm.sla_rate + '%'), el('td', 'num', tm.avg_fix_days === null ? '-' : String(tm.avg_fix_days)),
      el('td', 'num', fmtSeconds(tm.waste_sec_per_day)));
    body.append(tr);
  }
}

// 新增 vs 优化完成：每周两根柱（b1 新增、b2 完成）
function renderTrend(weeks) {
  const box = $('pb-trend');
  box.replaceChildren();
  if (!weeks.length) { box.append(el('p', 'muted', t('还没有数据。'))); return; }
  const W = 520, H = 140, left = 10, bottom = 16, top = 8;
  const s = svg('svg', { viewBox: `0 0 ${W} ${H}`, class: 'trend', role: 'img', 'aria-label': t('近 12 周新增与优化完成') });
  const max = Math.max(...weeks.flatMap((w) => [w.created, w.closed]), 1);
  const step = (W - left * 2) / weeks.length;
  const bw = Math.max(3, Math.min(14, step / 3));
  weeks.forEach((w, i) => {
    const x0 = left + i * step + (step - bw * 2 - 2) / 2;
    for (const [k, cls, dx] of [['created', 'b1', 0], ['closed', 'b2', bw + 2]]) {
      const h = ((H - top - bottom) * w[k]) / max;
      const r = svg('rect', { class: cls, x: (x0 + dx).toFixed(1), y: (H - bottom - h).toFixed(1), width: bw, height: Math.max(h, w[k] ? 1 : 0).toFixed(1) });
      r.append(svg('title', {}, k === 'created' ? t('{week} 起的一周：新增 {n}', { week: w.week, n: w[k] }) : t('{week} 起的一周：优化完成 {n}', { week: w.week, n: w[k] })));
      s.append(r);
    }
    if (i === 0 || i === weeks.length - 1 || weeks.length <= 6) {
      s.append(svg('text', { x: (x0 + bw).toFixed(1), y: H - 3, 'font-size': 9, fill: 'var(--dt-muted)', 'text-anchor': 'middle' }, w.week.slice(5)));
    }
  });
  box.append(s);
}

const COLORS = ['var(--dt-crit)', 'var(--dt-amber)', 'var(--dt-cyan)', 'var(--dt-ok)', 'var(--dt-info)', 'var(--dt-muted)'];

// 问题类型环图：按浪费 DB 时间占比，最多 5 类 + 其他
function renderCats(cats) {
  const box = $('pb-cat');
  box.replaceChildren();
  if (!cats.length) { box.append(el('p', 'muted', t('没有未关闭的问题。'))); return; }
  const totalWaste = cats.reduce((a, c) => a + c.waste, 0);
  const totalCount = cats.reduce((a, c) => a + c.count, 0);
  const rest = cats.slice(5);
  const items = cats.slice(0, 5).map((c) => ({ label: tp(c.label), v: c.waste, n: c.count }));
  if (rest.length) items.push({ label: t('其他'), v: rest.reduce((a, c) => a + c.waste, 0), n: rest.reduce((a, c) => a + c.count, 0) });
  const s = svg('svg', { viewBox: '0 0 280 140', class: 'trend', role: 'img', 'aria-label': t('未关闭问题类型占比') });
  const C = 2 * Math.PI * 50;
  let off = 0;
  items.forEach((it, i) => {
    const frac = totalWaste > 0 ? it.v / totalWaste : it.n / totalCount;
    const len = Math.max(frac * C, 0);
    const color = COLORS[i % COLORS.length];
    const c = svg('circle', { cx: 70, cy: 70, r: 50, fill: 'none', stroke: color, 'stroke-width': 22, 'stroke-dasharray': `${len.toFixed(1)} ${(C - len).toFixed(1)}`, 'stroke-dashoffset': (-off).toFixed(1), transform: 'rotate(-90 70 70)' });
    c.append(svg('title', {}, t('{label}：{n} 个，日均浪费 {time}', { label: it.label, n: it.n, time: fmtSeconds(it.v) })));
    s.append(c);
    off += len;
    s.append(svg('rect', { x: 140, y: 14 + i * 22, width: 10, height: 10, rx: 2, fill: color }),
      svg('text', { x: 156, y: 23 + i * 22, 'font-size': 11, fill: 'var(--dt-fg)' }, `${it.label} ${Math.round(frac * 100)}%`));
  });
  box.append(s);
}

async function load() {
  setMsg(msg, '');
  try {
    const b = await api('GET', '/api/v1/perf/board?' + qs({ days: $('pb-days').value, owner_side: $('pb-side').value }));
    renderKpis(b.overall);
    renderTeams(b.teams);
    renderTrend(b.trend);
    renderCats(b.by_category);
  } catch (e) {
    setMsg(msg, e.message);
  }
}

$('pb-days').addEventListener('change', load);
$('pb-side').addEventListener('change', load);
$('pb-filters').addEventListener('submit', (e) => e.preventDefault());
load();
