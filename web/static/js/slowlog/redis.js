/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: redis.js
 * @time: 2026/10/28
 */
// 慢查日志页的 Redis 模式：选中 Redis 实例时，原来的“慢日志统计 / 明细”面板换成 Redis 的
// “命令统计 / 慢命令明细 / 采集状态”。数据来自 /api/v1/slowlog/redis/*（定时采集的 SLOWLOG，命令指纹 = 命令名 + 参数占位）。
// 表单（实例、时间范围、搜索、耗时下限）与原页面共用，由 slowquery.js 通过 getParams 传入。
import { t, tp } from '../i18n.js';
import { api, $, el, setMsg, fmtNum, fmtSec, qs, renderPager, bindSort, emptyRow, fmtTime } from './lib.js';

const SVG_NS = 'http://www.w3.org/2000/svg';
const svg = (tag, attrs = {}, text) => {
  const e = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, String(v));
  if (text !== undefined) e.textContent = text;
  return e;
};

// 耗时下限选项（Redis 的慢命令是毫秒级；值是秒，和 min_seconds 参数一致）
const MIN_OPTIONS = [['', t('耗时：不限')], ['0.001', t('耗时 ≥ {n} ms', { n: 1 })], ['0.005', t('耗时 ≥ {n} ms', { n: 5 })], ['0.01', t('耗时 ≥ {n} ms', { n: 10 })],
  ['0.05', t('耗时 ≥ {n} ms', { n: 50 })], ['0.1', t('耗时 ≥ {n} ms', { n: 100 })], ['0.5', t('耗时 ≥ {n} ms', { n: 500 })], ['1', t('耗时 ≥ {n} s', { n: 1 })]];

const fmtMs = (v) => {
  v = Number(v) || 0;
  if (v >= 1000) return fmtSec(v / 1000) + ' s';
  return (v >= 100 ? v.toFixed(0) : v.toFixed(2).replace(/\.?0+$/, '') || '0') + ' ms';
};
const fmtDur = (ms) => {
  const v = (Number(ms) || 0) / 1000;
  if (v >= 3600) return (v / 3600).toFixed(1) + ' h';
  if (v >= 60) return (v / 60).toFixed(1) + ' min';
  return fmtSec(v) + ' s';
};
const clip = (s, n) => ((s || '').length > n ? s.slice(0, n) + '…' : s || '');

// 行内趋势：范围内每天的慢查次数。
function sparkline(vals) {
  const W = 84, H = 22, P = 2;
  const s = svg('svg', { viewBox: `0 0 ${W} ${H}`, class: 'sq-spark', role: 'img', 'aria-label': t('范围内每天的慢查次数') });
  if (!vals || !vals.length) return s;
  const max = Math.max(...vals, 1);
  const n = vals.length;
  const pts = vals.map((v, i) => `${(P + (n === 1 ? (W - 2 * P) / 2 : (i * (W - 2 * P)) / (n - 1))).toFixed(1)},${(H - P - (v / max) * (H - 2 * P)).toFixed(1)}`);
  s.append(svg('polyline', { points: pts.join(' '), class: 'line' }), svg('title', {}, t('共 {total} 次；单日最多 {max} 次', { total: vals.reduce((a, b) => a + b, 0), max })));
  return s;
}

// 趋势图：柱 = 慢命令次数（左轴），线 = 平均耗时（右轴，毫秒）。样式沿用 svg.trend。
function trendChart(points) {
  const W = 720, H = 150, L = 44, R = 52, T = 12, B = 26;
  const root = svg('svg', { viewBox: `0 0 ${W} ${H}`, class: 'trend', role: 'img', 'aria-label': t('Redis 慢命令次数与平均耗时趋势') });
  if (!points.length) {
    root.append(svg('text', { x: W / 2, y: H / 2, 'text-anchor': 'middle', class: 'ax' }, t('该时间范围内没有数据')));
    return root;
  }
  const pw = W - L - R, ph = H - T - B;
  const maxC = Math.max(...points.map((p) => p.count), 1);
  const maxA = Math.max(...points.map((p) => p.avg_ms), 0.000001);
  const slot = pw / points.length;
  const bw = Math.max(2, Math.min(26, slot * 0.6));
  const bars = svg('g', { class: 'bars' });
  const pts = [];
  points.forEach((p, i) => {
    const cx = L + slot * (i + 0.5);
    const h = (p.count / maxC) * ph;
    const r = svg('rect', { x: cx - bw / 2, y: T + ph - h, width: bw, height: Math.max(h, 1), rx: 1 });
    r.append(svg('title', {}, [p.t, t('慢命令 {count} 次', { count: p.count }), t('平均 {avg}，最大 {max}', { avg: fmtMs(p.avg_ms), max: fmtMs(p.max_ms) }),
      t('累计 {v}', { v: fmtMs(p.total_ms) })].join('\n')));
    bars.append(r);
    pts.push([cx, T + ph - (p.avg_ms / maxA) * ph]);
  });
  root.append(svg('line', { x1: L, y1: T + ph, x2: W - R, y2: T + ph, class: 'axis' }), bars);
  if (pts.length > 1) root.append(svg('polyline', { class: 'line', points: pts.map((q) => q.map((v) => v.toFixed(1)).join(',')).join(' ') }));
  else root.append(svg('circle', { class: 'dotpt', cx: pts[0][0], cy: pts[0][1], r: 3 }));
  root.append(svg('text', { x: L - 4, y: T + 8, 'text-anchor': 'end', class: 'ax' }, fmtNum(maxC)), svg('text', { x: L - 4, y: T + ph, 'text-anchor': 'end', class: 'ax' }, '0'),
    svg('text', { x: W - R + 4, y: T + 8, class: 'ax' }, fmtMs(maxA)));
  const idx = [...new Set([0, Math.floor((points.length - 1) / 2), points.length - 1])];
  for (const i of idx) {
    root.append(svg('text', { x: L + slot * (i + 0.5), y: H - 8, 'text-anchor': i === 0 ? 'start' : i === points.length - 1 ? 'end' : 'middle', class: 'ax' }, points[i].t.slice(5)));
  }
  return root;
}

/**
 * 初始化 Redis 模式。
 * @param {{root: HTMLElement, msg: HTMLElement, minSel: HTMLSelectElement, getParams: () => object, instName: () => string, canReset: boolean}} ctx
 * getParams() 返回当前表单的 {instance_id, start, end, q, min_seconds}。
 */
export function initRedis(ctx) {
  const rq = $('rq');
  const stat = { offset: 0, limit: 30, sort: 'exec_count', order: 'desc' };
  const det = { offset: 0, limit: 30, sort: 'start_time', order: 'desc' };
  let tab = 'stat';
  let fpId = '';
  let active = false;
  let savedMin = null;
  const defaultMin = [...ctx.minSel.options].map((o) => [o.value, o.textContent]); // 原页面的耗时下限选项（秒级），离开 Redis 模式时还原

  const params = (extra = {}) => ({ ...ctx.getParams(), command: $('rq-cmd').value, ...extra });
  const showMsg = (text, k) => setMsg(ctx.msg, text, k);

  function setTab(name) {
    tab = name;
    for (const b of rq.querySelectorAll('#rq-tabs .tab')) {
      const on = b.dataset.rtab === name;
      b.classList.toggle('on', on);
      b.setAttribute('aria-selected', String(on));
    }
    $('rq-pane-stat').hidden = name !== 'stat';
    $('rq-pane-det').hidden = name !== 'det';
    $('rq-pane-col').hidden = name !== 'col';
    $('rq-kpis').hidden = name === 'col';
    $('rq-trend').parentElement.hidden = name === 'col';
    reload();
  }

  // ---- 统计 ----

  function renderKpis(sum) {
    $('rq-k-fps').textContent = sum ? fmtNum(sum.fingerprints) : '—';
    $('rq-k-cnt').textContent = sum ? fmtNum(sum.exec_count) : '—';
    $('rq-k-sum').textContent = sum ? fmtDur(sum.total_ms) : '—';
    $('rq-k-avg').textContent = sum && sum.exec_count ? t('平均 {v}', { v: fmtMs(sum.total_ms / sum.exec_count) }) : '';
    $('rq-k-max').textContent = sum ? fmtMs(sum.max_ms) : '—';
  }

  function renderStat(page) {
    const body = $('rq-srows');
    body.replaceChildren();
    renderKpis(page.summary);
    if (page.notes && page.notes.length) showMsg(page.notes.map(tp).join(' '), 'info');
    if (!page.rows.length) body.append(emptyRow(11, t('该时间范围内没有 Redis 慢命令')));
    for (const r of page.rows) {
      const tr = el('tr');
      const tg = el('td');
      const toggle = el('button', 'btn ghost', '▸');
      toggle.type = 'button';
      toggle.setAttribute('aria-expanded', 'false');
      toggle.setAttribute('aria-label', t('展开完整命令与趋势'));
      tg.append(toggle);
      const link = el('a', 'mono wrap', clip(r.fingerprint, 100));
      link.href = '#';
      link.title = r.fingerprint;
      link.addEventListener('click', (e) => { e.preventDefault(); fpId = r.fp_id; det.offset = 0; setTab('det'); });
      const fpTd = el('td', 'mono wrap');
      fpTd.append(link, el('div', 'muted small mono', r.fp_id.slice(0, 12)));
      const sp = el('td');
      sp.append(sparkline(r.spark));
      const ops = el('td', 'sq-ops');
      const cp = el('button', 'btn sm', t('复制指纹'));
      cp.type = 'button';
      cp.addEventListener('click', () => copyText(r.fingerprint, cp));
      ops.append(cp);
      tr.append(tg, el('td', 'mono nowrap', r.command_name), fpTd, el('td', 'num', r.exec_count), el('td', 'num', fmtSec(r.total_ms)), el('td', 'num', fmtSec(r.avg_ms)),
        el('td', 'num', fmtSec(r.max_ms)), el('td', 'small nowrap', (r.first_seen || '—').slice(5, 16)), el('td', 'small nowrap', (r.last_seen || '—').slice(5, 16)), sp, ops);
      const dtr = el('tr');
      dtr.hidden = true;
      const dtd = el('td');
      dtd.colSpan = 11;
      dtr.append(dtd);
      let loaded = false;
      toggle.addEventListener('click', () => {
        const open = dtr.hidden;
        dtr.hidden = !open;
        toggle.textContent = open ? '▾' : '▸';
        toggle.setAttribute('aria-expanded', String(open));
        if (open && !loaded) { loaded = true; fillExpand(dtd, r); }
      });
      body.append(tr, dtr);
    }
    renderPager($('rq-spager'), { total: page.total, offset: stat.offset, limit: stat.limit }, (off) => { stat.offset = off; loadStat(); });
  }

  async function fillExpand(td, r) {
    const grid = el('div', 'dgrid');
    grid.style.margin = '0';
    grid.append(el('pre', 'sqlbox mono', r.sample_text || r.fingerprint));
    const right = el('div');
    right.append(el('b', 'small', t('该命令的历史趋势')), document.createTextNode(' '), el('span', 'muted small', t('次数（柱）· 平均耗时（线）')));
    const holder = el('div', 'muted small', t('加载中…'));
    right.append(holder);
    grid.append(right);
    td.append(grid);
    try {
      const tr = await api('GET', '/api/v1/slowlog/redis/trend?' + qs(params({ fp_id: r.fp_id, bucket: spanDays() <= 3 ? 'hour' : 'day' })));
      holder.replaceChildren(trendChart(tr.points));
    } catch (e) {
      holder.textContent = t('趋势加载失败：{msg}', { msg: e.message });
    }
  }

  function spanDays() {
    const p = ctx.getParams();
    const a = Date.parse(p.start), b = Date.parse(p.end);
    return Number.isNaN(a) || Number.isNaN(b) ? 7 : Math.round((b - a) / 86400000) + 1;
  }

  async function loadTrend() {
    const holder = $('rq-trend');
    try {
      const tr = await api('GET', '/api/v1/slowlog/redis/trend?' + qs(params({ fp_id: fpId && tab === 'det' ? fpId : '', bucket: spanDays() <= 3 ? 'hour' : 'day' })));
      holder.replaceChildren(trendChart(tr.points));
    } catch (e) {
      holder.replaceChildren(el('span', 'muted small', t('趋势加载失败：{msg}', { msg: e.message })));
    }
  }

  async function loadStat() {
    showMsg('');
    $('rq-srows').replaceChildren(emptyRow(11, t('加载中…')));
    try {
      renderStat(await api('GET', '/api/v1/slowlog/redis/review?' + qs(params({ sort: stat.sort, order: stat.order, limit: stat.limit, offset: stat.offset }))));
    } catch (e) {
      $('rq-srows').replaceChildren(emptyRow(11, t('加载失败')));
      $('rq-spager').replaceChildren();
      renderKpis(null);
      showMsg(e.message, e.code === 'table_missing' ? 'warn' : 'err');
    }
  }

  // ---- 明细 ----

  function renderDet(page) {
    const body = $('rq-drows');
    body.replaceChildren();
    if (!page.rows.length) body.append(emptyRow(5, t('没有符合条件的慢命令')));
    for (const r of page.rows) {
      const cmd = el('td', 'mono wrap', clip(r.command_text, 200));
      cmd.title = r.command_text;
      const client = el('td', 'mono small');
      client.textContent = r.client_addr || '—';
      if (r.client_name) client.append(el('div', 'muted', r.client_name));
      const tr = el('tr');
      tr.append(el('td', 'nowrap', r.start_time), el('td', 'num', fmtSec(r.duration_ms)), el('td', 'mono nowrap', r.command_name), client, cmd);
      body.append(tr);
    }
    renderPager($('rq-dpager'), { total: page.total, offset: det.offset, limit: det.limit }, (off) => { det.offset = off; loadDet(); });
  }

  async function loadDet() {
    showMsg('');
    $('rq-filter').textContent = fpId ? t('按命令指纹过滤：{id}', { id: fpId.slice(0, 12) }) : '';
    $('rq-clear').hidden = !fpId;
    $('rq-drows').replaceChildren(emptyRow(5, t('加载中…')));
    try {
      renderDet(await api('GET', '/api/v1/slowlog/redis/history?' + qs(params({ fp_id: fpId, sort: det.sort, order: det.order, limit: det.limit, offset: det.offset }))));
    } catch (e) {
      $('rq-drows').replaceChildren(emptyRow(5, t('加载失败')));
      $('rq-dpager').replaceChildren();
      showMsg(e.message, e.code === 'table_missing' ? 'warn' : 'err');
    }
  }

  // ---- 采集状态 ----

  async function loadCollector() {
    const body = $('rq-crows');
    body.replaceChildren(emptyRow(8, t('加载中…')));
    try {
      const ov = await api('GET', '/api/v1/slowlog/redis/collector');
      $('rq-col-note').textContent = !ov.table_ready
        ? t('Redis 慢查询的表未创建。')
        : ov.enabled
          ? t('采集器已启用，约每 {sec} 秒采集一次。', { sec: ov.interval_sec })
          : t('采集器被环境变量 SLOWLOG_COLLECT_ENABLE 关闭，可用“立即采集”手动拉取。');
      body.replaceChildren();
      if (!ov.items.length) body.append(emptyRow(8, t('还没有采集记录')));
      for (const s of ov.items) {
        const tr = el('tr');
        const st = el('td');
        st.append(el('span', 'st ' + (s.status === 'ok' ? 'passed' : 'rejected'), s.status === 'ok' ? t('正常') : t('失败')));
        tr.append(el('td', '', s.instance_name || '#' + s.instance_id), st, el('td', 'nowrap', fmtTime(s.last_run_at)), el('td', 'nowrap', fmtTime(s.last_ok_at)),
          el('td', 'num', s.last_rows), el('td', 'num', fmtNum(s.total_rows)),
          el('td', 'mono small', s.last_slow_id >= 0 ? `${s.last_slow_id} / ${fmtTime(s.last_slow_time)}` : '—'), el('td', 'small', tp(s.error || '')));
        body.append(tr);
      }
    } catch (e) {
      body.replaceChildren(emptyRow(8, t('加载失败：{msg}', { msg: e.message })));
    }
  }

  // ---- 命令下拉 ----

  async function loadCommands() {
    const sel = $('rq-cmd');
    const cur = sel.value;
    sel.replaceChildren(new Option(t('全部命令'), ''));
    const p = ctx.getParams();
    if (!p.instance_id) return;
    try {
      const list = await api('GET', '/api/v1/slowlog/redis/commands?' + qs({ instance_id: p.instance_id, start: p.start, end: p.end }));
      for (const c of list) sel.append(new Option(c, c));
      sel.value = list.includes(cur) ? cur : '';
    } catch (_) { /* 命令列表只是便利功能 */ }
  }

  function copyText(text, btn) {
    const done = () => { const old = btn.textContent; btn.textContent = t('已复制'); setTimeout(() => { btn.textContent = old; }, 1200); };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(done, () => {});
  }

  // ---- 立即采集 / 重置 ----

  async function collectNow() {
    const p = ctx.getParams();
    if (!p.instance_id) return;
    const b = $('rq-collect');
    b.disabled = true;
    try {
      const res = await api('POST', '/api/v1/slowlog/redis/collect', { instance_id: Number(p.instance_id) });
      showMsg(res.inserted ? t('已采集入库 {n} 条新的慢命令', { n: res.inserted }) : t('没有新的慢命令（实例上的慢日志都已入库）'), 'info');
      await loadCommands();
      reload();
    } catch (e) {
      showMsg(e.message);
    } finally {
      b.disabled = false;
    }
  }

  function openReset() {
    $('rq-mname').textContent = ctx.instName();
    $('rq-mconfirm').value = '';
    $('rq-mgo').disabled = true;
    $('rq-modal').hidden = false;
    $('rq-mconfirm').focus();
  }
  const closeReset = () => { $('rq-modal').hidden = true; };

  async function doReset() {
    const p = ctx.getParams();
    $('rq-mgo').disabled = true;
    try {
      const res = await api('POST', '/api/v1/slowlog/redis/reset', { instance_id: Number(p.instance_id) });
      closeReset();
      showMsg(t('已重置：清空了实例上的 {cleared} 条慢日志（重置前新采集入库 {collected} 条）', { cleared: res.cleared, collected: res.collected }), 'info');
      reload();
    } catch (e) {
      closeReset();
      showMsg(e.message);
    }
  }

  // ---- 入口 ----

  function reload() {
    if (!active) return;
    if (!ctx.getParams().instance_id) { showMsg(t('请先选择 Redis 实例'), 'info'); return; }
    if (tab === 'col') { loadCollector(); return; }
    loadTrend();
    if (tab === 'stat') loadStat(); else loadDet();
  }

  return {
    // 进入 / 离开 Redis 模式（切换实例时调用）
    setActive(on) {
      if (on === active) return;
      active = on;
      rq.hidden = !on;
      if (on) {
        savedMin = ctx.minSel.value;
        ctx.minSel.replaceChildren(...MIN_OPTIONS.map(([v, txt]) => new Option(txt, v)));
        $('rq-reset').hidden = !ctx.canReset;
        $('sq-q').placeholder = t('搜索命令 / 客户端地址');
        fpId = '';
        stat.offset = 0;
        det.offset = 0;
        loadCommands();
      } else {
        ctx.minSel.replaceChildren(...defaultMin.map(([v, txt]) => new Option(txt, v)));
        $('sq-q').placeholder = t('搜索 SQL');
        if (savedMin !== null) ctx.minSel.value = savedMin;
      }
    },
    reload,
    resetPaging() { stat.offset = 0; det.offset = 0; fpId = ''; },
    refreshCommands: loadCommands,
    init() {
      for (const b of rq.querySelectorAll('#rq-tabs .tab')) b.addEventListener('click', () => setTab(b.dataset.rtab));
      $('rq-cmd').addEventListener('change', () => { stat.offset = 0; det.offset = 0; reload(); });
      $('rq-collect').addEventListener('click', collectNow);
      $('rq-reset').addEventListener('click', openReset);
      $('rq-mcancel').addEventListener('click', closeReset);
      $('rq-mgo').addEventListener('click', doReset);
      $('rq-mconfirm').addEventListener('input', () => { $('rq-mgo').disabled = $('rq-mconfirm').value.trim() !== ctx.instName(); });
      $('rq-modal').addEventListener('click', (e) => { if (e.target === $('rq-modal')) closeReset(); });
      document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !$('rq-modal').hidden) closeReset(); });
      $('rq-clear').addEventListener('click', () => { fpId = ''; det.offset = 0; reload(); });
      bindSort($('rq-shead'), stat, reload);
      bindSort($('rq-dhead'), det, reload);
    },
  };
}
