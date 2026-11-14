/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: trace.js
 * @time: 2026/10/28
 */
// 分区维护“运行输出”面板：时间 / 级别 / 消息逐行显示，可按级别筛选、搜索、复制、下载 .log。
// 所有文本一律用 textContent 写入（不拼 innerHTML）；面板在自身内部滚动，不撑宽页面。
import { api, toast } from '../common.js';
import { t } from '../i18n.js';
import { el } from './util.js';

const LEVELS = ['INFO', 'WARN', 'ERROR'];
const LEVEL_RANK = { INFO: 0, WARN: 1, ERROR: 2 };

export function normLevel(l) {
  const u = String(l || 'INFO').toUpperCase();
  if (u === 'WARNING') return 'WARN';
  return LEVELS.includes(u) ? u : 'INFO';
}

// 统计：[行数, WARN 数, ERROR 数]
export function traceCounts(lines) {
  let w = 0, e = 0;
  for (const x of lines || []) {
    const l = normLevel(x.level);
    if (l === 'WARN') w += 1;
    else if (l === 'ERROR') e += 1;
  }
  return [(lines || []).length, w, e];
}

export function traceSummary(lines) {
  const [n, w, e] = traceCounts(lines);
  let s = t('运行输出 {n} 行', { n });
  if (e) s += t('，{n} 条错误', { n: e });
  if (w) s += t('，{n} 条警告', { n: w });
  return s;
}

// 契约的行是 { time, level, message }。
function lineOf(x) {
  return { time: String(x.time || ''), level: normLevel(x.level), message: x.message === undefined || x.message === null ? '' : String(x.message) };
}

const isCutMark = (m) => /^…(?:已截断|truncated)/.test(String(m || '')); // 服务端按界面语言翻译过的标记也要认得

function plainText(lines) {
  return lines.map((x) => `${x.time} [${x.level.padEnd(5)}] ${x.message}`).join('\n') + (lines.length ? '\n' : '');
}

export async function copyText(text) {
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch (_) { /* 退回到选区复制 */ }
  const ta = el('textarea', 'pm-out-copybuf');
  ta.value = text;
  ta.setAttribute('readonly', '');
  document.body.append(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  ta.remove();
  return ok;
}

// 把 text 里命中 term 的部分用 <mark> 包出来（createElement，不碰 innerHTML）。
function appendHighlighted(parent, text, term) {
  if (!term) { parent.append(document.createTextNode(text)); return; }
  const low = text.toLowerCase();
  const t = term.toLowerCase();
  let i = 0;
  for (;;) {
    const j = low.indexOf(t, i);
    if (j < 0) break;
    if (j > i) parent.append(document.createTextNode(text.slice(i, j)));
    parent.append(el('mark', '', text.slice(j, j + t.length)));
    i = j + t.length;
  }
  if (i < text.length) parent.append(document.createTextNode(text.slice(i)));
}

// opts: { exportUrl, filename, truncated, title }
// 返回面板根节点（已填好，调用方负责挂到页面上）。
export function buildTracePanel(lines, opts = {}) {
  const all = (lines || []).map(lineOf);
  const root = el('div', 'pm-out');

  const bar = el('div', 'pm-out-bar');
  const level = el('select', 'pm-out-level');
  level.setAttribute('aria-label', t('按级别筛选运行输出'));
  for (const [v, label] of [['INFO', t('全部级别')], ['WARN', t('警告及以上')], ['ERROR', t('仅错误')]]) level.append(new Option(label, v));
  const search = el('input', 'pm-out-search');
  search.type = 'search';
  search.placeholder = t('搜索运行输出');
  search.setAttribute('aria-label', t('搜索运行输出'));
  search.autocomplete = 'off';
  const count = el('span', 'pm-out-count muted small');
  const copy = el('button', 'btn ghost pm-out-copy', t('复制'));
  copy.type = 'button';
  bar.append(level, search, count, el('span', 'spacer'), copy);
  let dl = null;
  if (opts.exportUrl) {
    dl = el('a', 'btn ghost pm-out-dl', t('下载 .log'));
    dl.href = opts.exportUrl;
    dl.setAttribute('download', opts.filename || '');
    dl.rel = 'noopener';
    bar.append(dl);
  }

  const cutLines = all.filter((x) => isCutMark(x.message));
  const notice = el('div', 'pm-out-cut-note');
  if (cutLines.length || opts.truncated) {
    notice.append(el('span', 'st waiting', t('已截断')), el('span', 'small', ' ' + (cutLines.length ? cutLines[0].message : t('较早的内容已被截去。'))));
  } else {
    notice.hidden = true;
  }

  const body = el('div', 'pm-out-body');
  body.tabIndex = 0;
  body.setAttribute('role', 'log');
  body.setAttribute('aria-label', opts.title || t('运行输出'));

  function draw() {
    const min = LEVEL_RANK[level.value] || 0;
    const term = search.value.trim();
    const low = term.toLowerCase();
    body.replaceChildren();
    let shown = 0;
    for (const x of all) {
      if (LEVEL_RANK[x.level] < min && !isCutMark(x.message)) continue;
      if (low && !(x.message.toLowerCase().includes(low) || x.time.toLowerCase().includes(low))) continue;
      const row = el('div', 'pm-out-line pm-lv-' + x.level.toLowerCase() + (isCutMark(x.message) ? ' pm-out-cut' : ''));
      row.append(el('span', 'pm-out-t', x.time), el('span', 'pm-out-l', x.level));
      const msg = el('span', 'pm-out-m');
      appendHighlighted(msg, x.message, term);
      row.append(msg);
      body.append(row);
      shown += 1;
    }
    if (!shown) body.append(el('div', 'pm-out-empty muted small', all.length ? t('没有符合筛选条件的输出行') : t('这条记录没有运行输出')));
    count.textContent = t('{shown} / {total} 行', { shown, total: all.length });
  }
  level.addEventListener('change', draw);
  search.addEventListener('input', draw);
  copy.addEventListener('click', async () => {
    const min = LEVEL_RANK[level.value] || 0;
    const low = search.value.trim().toLowerCase();
    const sel = all.filter((x) => (LEVEL_RANK[x.level] >= min || isCutMark(x.message)) && (!low || x.message.toLowerCase().includes(low) || x.time.toLowerCase().includes(low)));
    const ok = await copyText(plainText(sel));
    toast(ok ? t('已复制 {n} 行运行输出', { n: sel.length }) : t('复制失败，请手工选中文本复制'), ok ? 'ok' : 'warn');
  });
  draw();
  root.append(bar, notice, body);
  return root;
}

// 可展开的运行输出：{ btn, holder }。点按钮才（首次）取数并构建面板；open=true 时一开始就展开。
// load() 返回 { lines, exportUrl?, filename?, truncated? }；onToggle(open) 供调用方同步外层容器（如表格行）的显隐。
export function traceParts({ label, load, open = false, title, onToggle }) {
  const btn = el('button', 'btn ghost pm-out-toggle', label);
  btn.type = 'button';
  btn.setAttribute('aria-expanded', 'false');
  const holder = el('div', 'pm-out-holder');
  holder.hidden = true;
  let loaded = false;
  let loading = false;
  async function setOpen(want) {
    holder.hidden = !want;
    btn.setAttribute('aria-expanded', String(want));
    if (onToggle) onToggle(want);
    if (!want || loaded || loading) return;
    loading = true;
    holder.replaceChildren(el('p', 'muted small', t('正在读取运行输出…')));
    try {
      const r = await load();
      holder.replaceChildren(buildTracePanel(r.lines, { exportUrl: r.exportUrl, filename: r.filename, truncated: r.truncated, title }));
      loaded = true;
    } catch (e) {
      holder.replaceChildren(el('div', 'alert err', e.message));
    } finally {
      loading = false;
    }
  }
  btn.addEventListener('click', () => setOpen(holder.hidden));
  if (open) setOpen(true);
  return { btn, holder };
}

// 按钮和面板放在同一个容器里（预览 / 执行结果等块级场景）。
export function traceToggle(opts) {
  const { btn, holder } = traceParts(opts);
  const wrap = el('div', 'pm-out-wrap');
  wrap.append(btn, holder);
  return wrap;
}

// 一条维护日志（列表项）的“运行输出”：按钮放在“执行结果”单元格里，面板放在紧随其后的整行（colSpan 列）里，
// 避免窄列把等宽输出挤成一条细缝。明细与导出走 /logs/:id、/logs/:id/output。
// 返回 { btn, row }：row 为 null 表示这条日志没有运行输出（btn 是一句说明）。
export function logOutputRow(l, colSpan) {
  const n = l.output_lines || 0;
  if (!n) return { btn: el('div', 'pm-out-none muted small', t('无运行输出')), row: null };
  // 没有语句项、也没有轮次汇总的记录（如推送测试）：运行输出就是全部内容，直接展开
  const onlyOutput = !(l.items || []).length && !l.raw && !l.round;
  const row = el('tr', 'pm-out-row');
  row.hidden = !onlyOutput;
  const cell = el('td', 'pm-out-cell');
  cell.colSpan = colSpan;
  const { btn, holder } = traceParts({
    label: t('运行输出（{n} 行）', { n }),
    title: t('日志 {id} 的运行输出', { id: l.id }),
    open: onlyOutput,
    onToggle: (open) => { row.hidden = !open; },
    load: async () => {
      // 契约：列表项只带 output_lines（行数）；明细在 GET /logs/:id 的 output。列表项若已内联 output.lines 就不再请求。
      const inline = l.output && Array.isArray(l.output.lines) ? l.output : null;
      const o = inline || (await api('GET', `/api/v1/partman/logs/${encodeURIComponent(l.id)}`)).output || {};
      return { lines: o.lines || [], truncated: !!o.truncated, exportUrl: `/api/v1/partman/logs/${encodeURIComponent(l.id)}/output` };
    },
  });
  cell.append(holder);
  row.append(cell);
  return { btn, row };
}

// 预览 / 执行结果里随响应带回的输出（不一定落库）：给出现成的行；有 log_id 时可下载。
export function inlineTrace({ label, output, logId, open = false }) {
  const lines = (output && output.lines) || [];
  return traceToggle({
    label: t('{label}（{n} 行）', { label, n: lines.length }),
    title: label,
    open,
    load: async () => ({ lines, truncated: !!(output && output.truncated), exportUrl: logId ? `/api/v1/partman/logs/${encodeURIComponent(logId)}/output` : '' }),
  });
}
