/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: plan.js
 * @time: 2026/10/28
 */
// 执行计划表格：type 着色（ALL 红、index / range 橙、ref / eq_ref / const 绿）、Extra 里的 filesort / temporary 标橙，
// 下面列出服务端给出的风险提示（全表扫描、函数使索引失效、filesort、前导 %、深分页等）。
import { t, tp } from '../i18n.js';

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

const TYPE_CLASS = { ALL: 'bad', index: 'warn', range: 'warn', index_merge: 'warn', fulltext: 'warn', ref: 'ok', eq_ref: 'ok', const: 'ok', system: 'ok', ref_or_null: 'ok', unique_subquery: 'ok', index_subquery: 'ok' };

export function renderPlan(container, res) {
  const meta = el('div', 'res-meta');
  meta.append(el('span', '', t('执行计划 · {n} 行', { n: res.row_count })), el('span', '', `${res.cost_ms} ms`));
  container.append(meta);
  if (res.notice) container.append(Object.assign(el('div', 'alert info res-note', tp(res.notice))));

  const typeIdx = res.columns.findIndex((c) => c.toLowerCase() === 'type');
  const extraIdx = res.columns.findIndex((c) => c.toLowerCase() === 'extra');
  const wrap = el('div', 'grid-wrap');
  const table = el('table', 'grid');
  const thead = el('thead');
  const hr = el('tr');
  for (const c of res.columns) hr.append(el('th', '', c));
  thead.append(hr);
  const tbody = el('tbody');
  for (const row of res.rows) {
    const tr = el('tr');
    row.forEach((v, i) => {
      const td = el('td', v === null ? 'null' : '');
      if (v === null || v === undefined) td.textContent = 'NULL';
      else if (i === typeIdx) td.append(el('span', 'plan-type ' + (TYPE_CLASS[String(v)] || ''), String(v)));
      else {
        td.textContent = String(v);
        if (i === extraIdx && /Using (filesort|temporary)/.test(td.textContent)) td.className = 'plan-warn';
        if (typeof v === 'number') td.classList.add('num');
      }
      tr.append(td);
    });
    tbody.append(tr);
  }
  table.append(thead, tbody);
  wrap.append(table);
  container.append(wrap);

  const tips = el('div', 'plan-tips');
  if (res.plan_tips && res.plan_tips.length) {
    for (const tip of res.plan_tips) {
      tips.append(el('div', 'alert ' + (tip.level === 'bad' ? 'bad' : tip.level === 'warn' ? 'warn' : 'info'), (tip.level === 'info' ? '💡 ' : '⚠ ') + tp(tip.text)));
    }
  } else if (typeIdx >= 0) {
    tips.append(el('div', 'alert good', t('✓ 未发现明显风险')));
  }
  if (tips.childElementCount) container.append(tips);
}
