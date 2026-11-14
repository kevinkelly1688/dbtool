/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: grid.js
 * @time: 2026/10/28
 */
// 查询结果渲染。所有单元格都用 textContent 写入，避免把数据库里的内容当 HTML 执行。
// 单元格的悬停复制 / 双击预览 / 复制整列在 gridtools.js。
import { t, tp, isZh } from '../i18n.js';
import { copyText } from './clip.js';
import { cellText } from './cells.js';
import { attachGridTools } from './gridtools.js';

const LIST_SEP = isZh ? '、' : ', '; // 列名列表的分隔符

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

export function toTSV(res) {
  const rows = [res.columns.join('\t')];
  for (const r of res.rows) rows.push(r.map((v) => (v === null ? '' : String(v).replace(/[\t\r\n]+/g, ' '))).join('\t'));
  return rows.join('\n');
}

const nf = (n) => Number(n).toLocaleString('en-US');

// opts: { title, estimate: () => Promise<number|-1>（结果被截断时估算总行数）, exportHref: 数据导出提交页地址（空则不显示链接）,
//         dbType: 实例类型（复制 IN 列表的引号规则、预览的高亮模式）, onNext: Redis 翻页 }
export function renderResult(container, res, opts = {}) {
  container.replaceChildren();
  const masked = new Set((res.masked_columns || []).map((c) => String(c).toLowerCase()));
  const meta = el('div', 'res-meta');
  meta.append(el('span', '', t('{title} · {n} 行', { title: opts.title || t('结果'), n: res.row_count })), el('span', '', `${res.cost_ms} ms`));
  if (res.stmt_kind) meta.append(el('span', 'chip', res.stmt_kind)); // PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦：引擎判定的语句种类
  if (res.masked_columns && res.masked_columns.length) {
    meta.append(el('span', 'chip ok', t('🛡 已脱敏：{cols}', { cols: res.masked_columns.join(LIST_SEP) })));
  }
  if (res.next_cmd && opts.onNext) { // Redis SCAN 系列：把游标换成新的再跑一次
    const nb = el('button', 'btn', t('下一页（游标 {cursor}）→', { cursor: res.cursor }));
    nb.type = 'button';
    nb.title = res.next_cmd;
    nb.addEventListener('click', () => opts.onNext(res.next_cmd));
    meta.append(nb);
  }
  meta.append(el('span', 'spacer'));
  const copy = el('button', 'btn ghost', t('复制'));
  copy.type = 'button';
  copy.addEventListener('click', async () => {
    const ok = await copyText(toTSV(res));
    copy.textContent = ok ? t('已复制') : t('复制失败');
    setTimeout(() => (copy.textContent = t('复制')), 1500);
  });
  meta.append(copy);
  container.append(meta);

  if (res.unmasked) container.append(el('div', 'alert warn res-note res-unmasked', t('数据脱敏总开关已关闭，该类型的查询结果不做脱敏'))); // 总开关（data_masking）关闭时后端才会带 unmasked
  for (const w of res.warnings || []) container.append(el('div', 'alert warn res-note', tp(w)));
  for (const n of res.notices || []) container.append(el('div', 'alert info res-note', tp(n)));
  if (res.notice) container.append(el('div', 'alert info res-note', tp(res.notice)));

  if (res.truncated) {
    const box = el('div', 'alert warn res-note');
    const n = nf(res.effective_limit);
    const txt = el('span', '', res.truncated_note ? tp(res.truncated_note)
      : res.limit_source ? t('结果已按 {n} 行截断（限制来源：{source}），符合条件的数据可能更多', { n, source: tp(res.limit_source) })
        : t('结果已按 {n} 行截断，符合条件的数据可能更多', { n }));
    const est = el('span', '', '');
    box.append(txt, est);
    if (opts.exportHref) {
      const a = el('a', 'alert-act', t('去提交数据导出工单 →'));
      a.href = opts.exportHref;
      a.target = '_blank';
      a.rel = 'noopener';
      box.append(a);
    }
    container.append(box);
    if (opts.estimate) {
      est.textContent = t('（正在估算总行数…）');
      opts.estimate().then((n) => { est.textContent = n >= 0 ? t('（估算约 {n} 行）', { n: nf(n) }) : ''; }).catch(() => { est.textContent = ''; });
    }
  }

  if (!res.columns.length) {
    container.append(el('div', 'res-empty', t('语句已执行，没有返回结果集')));
  } else {
    const wrap = el('div', 'grid-wrap');
    const table = el('table', 'grid');
    const thead = el('thead');
    const hr = el('tr');
    hr.append(el('th', 'rn', '#'));
    const maskedIdx = [];
    res.columns.forEach((c, i) => {
      const th = el('th', '', c);
      const ctype = res.column_types && res.column_types[i];
      if (ctype) th.title = ctype; // 类型只放在悬停提示里，不在表头下面另占一行
      if (masked.has(String(c).toLowerCase())) {
        maskedIdx[i] = true;
        th.classList.add('masked'); // 脱敏列：表头绿色字体
        th.title = t('敏感列（已脱敏）') + (ctype ? ' · ' + ctype : '');
      }
      hr.append(th);
    });
    thead.append(hr);
    const tbody = el('tbody');
    res.rows.forEach((row, i) => {
      const tr = el('tr');
      tr.append(el('td', 'rn', String(i + 1)));
      row.forEach((v, k) => {
        const td = el('td', v === null ? 'null' : typeof v === 'number' ? 'num' : '', cellText(v));
        if (maskedIdx[k] && v !== null) td.classList.add('masked');
        if (typeof v === 'string' && v.length > 40) td.title = v;
        tr.append(td);
      });
      tbody.append(tr);
    });
    table.append(thead, tbody);
    wrap.append(table);
    if (!res.rows.length) wrap.append(el('div', 'res-empty', t('没有符合条件的数据'))); // 字段名在上，“没有数据”在字段名下面
    container.append(wrap);
    if (res.rows.length) attachGridTools(wrap, table, res, { dbType: opts.dbType });
  }
}
