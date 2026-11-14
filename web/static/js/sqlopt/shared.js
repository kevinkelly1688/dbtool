/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: shared.js
 * @time: 2026/10/28
 */
// SQL 分析 / SQL 优化工具共用的小工具：DOM 辅助、实例与库下拉、任务轮询、建议/执行计划/Markdown 渲染。
// 所有来自服务端或数据库的字符串都用 textContent 写入，不拼 innerHTML。
import { api } from '../common.js';
import { t, locale } from '../i18n.js';

export const API = '/api/v1/sqlopt';

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

export function option(value, label) {
  const o = document.createElement('option');
  o.value = value;
  o.textContent = label;
  return o;
}

// 在容器里显示一条提示；text 为空则清除。
export function setMsg(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

export const loadOptions = () => api('GET', `${API}/options`);
// 取实例上可选的库。列表为空时在 msgBox 里说明原因（账号看不到库 / 系统库被隐藏 / 被实例的库名规则过滤），而不是只给一个空下拉。
export async function loadDatabases(instId, msgBox) {
  const r = await api('GET', `${API}/instances/${instId}/databases?detail=1`);
  const list = r.databases || [];
  if (!list.length && msgBox) {
    setMsg(msgBox, r.total
      ? t('该实例上没有可选的库：共 {total} 个库，其中系统库 {sys} 个已隐藏，{show} 个不符合实例的“库名显示规则”，{denied} 个命中实例的“库名屏蔽规则”。', { total: r.total, sys: r.hidden_system, show: r.hidden_show, denied: r.hidden_denied })
      : t('该实例的账号看不到任何库（SHOW DATABASES 为空），请检查实例账号的权限。'), 'info');
  }
  return list;
}

// 轮询任务直到结束。onUpdate 每次拿到最新状态时调用；返回 { promise, stop }。
export function pollJob(id, onUpdate, interval = 1500) {
  let stopped = false;
  let timer = null;
  let fails = 0;
  const promise = new Promise((resolve, reject) => {
    const tick = async () => {
      if (stopped) return;
      try {
        const v = await api('GET', `${API}/jobs/${id}`);
        fails = 0;
        onUpdate(v);
        if (v.status === 'queued' || v.status === 'running') timer = setTimeout(tick, interval);
        else resolve(v);
      } catch (e) {
        if (e.status === 404 || e.status === 403 || ++fails >= 5) reject(e);
        else timer = setTimeout(tick, interval * 2);
      }
    };
    tick();
  });
  return { promise, stop() { stopped = true; clearTimeout(timer); } };
}

export const LEVEL_LABEL = { high: t('高|sqlopt_level'), medium: t('中|sqlopt_level'), low: t('低|sqlopt_level'), info: t('提示') };
const LEVEL_BADGE = { high: 'badge crit', medium: 'badge warn', low: 'badge', info: 'badge' };

export function levelBadge(level) {
  return el('span', LEVEL_BADGE[level] || 'badge', LEVEL_LABEL[level] || level);
}

export function scoreClass(score) {
  return score >= 80 ? 'good' : score >= 60 ? 'mid' : 'bad';
}

// 结果集 → 表格
export function renderTable(tbl) {
  const wrap = el('div', 'so-tbl-wrap');
  const table = el('table', 'list');
  const head = el('tr');
  for (const c of tbl.columns || []) head.append(el('th', '', c));
  const thead = el('thead');
  thead.append(head);
  table.append(thead);
  const body = el('tbody');
  for (const row of tbl.rows || []) {
    const tr = el('tr');
    for (const v of row) {
      const td = el('td');
      if (v === null || v === undefined) td.append(el('span', 'so-null', 'NULL'));
      else td.textContent = String(v);
      tr.append(td);
    }
    body.append(tr);
  }
  if (!(tbl.rows || []).length) {
    const tr = el('tr');
    const td = el('td', 'empty', t('没有数据'));
    td.colSpan = Math.max(1, (tbl.columns || []).length);
    tr.append(td);
    body.append(tr);
  }
  table.append(body);
  wrap.append(table);
  if (tbl.truncated) wrap.append(el('div', 'muted small', t('结果过长，只显示了前面一部分')));
  return wrap;
}

function inline(text) {
  const frag = document.createDocumentFragment();
  for (const part of text.split(/(`[^`]+`|\*\*[^*]+\*\*)/)) {
    if (!part) continue;
    if (part.startsWith('`') && part.endsWith('`') && part.length > 2) frag.append(el('code', '', part.slice(1, -1)));
    else if (part.startsWith('**') && part.endsWith('**') && part.length > 4) frag.append(el('strong', '', part.slice(2, -2)));
    else frag.append(document.createTextNode(part));
  }
  return frag;
}

// 把 SOAR 的 Markdown 报告渲染成 DOM：标题、列表、代码块、段落；表格按原文显示。不生成链接，不解析 HTML。
export function renderMarkdown(src) {
  const root = el('div', 'so-md');
  const lines = String(src).replace(/\r\n/g, '\n').split('\n');
  let ul = null;
  let code = null;
  let table = null;
  const flushTable = () => {
    if (table) root.append(el('pre', 'sqlbox mono', table.join('\n')));
    table = null;
  };
  for (const line of lines) {
    if (line.trim().startsWith('```')) {
      if (code) { root.append(el('pre', 'sqlbox mono', code.join('\n'))); code = null; } else { flushTable(); ul = null; code = []; }
      continue;
    }
    if (code) { code.push(line); continue; }
    if (line.trim().startsWith('|')) { ul = null; (table = table || []).push(line); continue; }
    flushTable();
    let m = /^(#{1,6})\s+(.*)$/.exec(line);
    if (m) { ul = null; root.append(el(m[1].length <= 2 ? 'h3' : m[1].length === 3 ? 'h4' : 'h5', '', m[2])); continue; }
    m = /^\s*(?:[-*]|\d+\.)\s+(.*)$/.exec(line);
    if (m) {
      if (!ul) { ul = el('ul'); root.append(ul); }
      const li = el('li');
      li.append(inline(m[1]));
      ul.append(li);
      continue;
    }
    ul = null;
    if (line.trim()) { const p = el('p'); p.append(inline(line.replace(/^>\s?/, ''))); root.append(p); }
  }
  if (code) root.append(el('pre', 'sqlbox mono', code.join('\n')));
  flushTable();
  return root;
}

export function renderFindings(list) {
  const ul = el('ul', 'so-finds');
  for (const f of list) {
    const li = el('li', 'so-find ' + f.level);
    const head = el('div', 't');
    head.append(levelBadge(f.level), el('span', '', f.title), el('code', '', f.id));
    li.append(head);
    if (f.detail) li.append(el('p', '', f.detail));
    if (f.suggestion) {
      if (/^ALTER\s/i.test(f.suggestion)) li.append(el('pre', 'sqlbox mono', f.suggestion));
      else li.append(el('p', '', t('建议：{text}', { text: f.suggestion })));
    }
    ul.append(li);
  }
  return ul;
}

function section(root, title) {
  root.append(el('div', 'so-sec', title));
}

// 建索引前后的执行计划对比（后者是服务端估算）与相关表的现有索引清单。报告里没有这些字段时什么也不画。
function planCell(b, field) {
  switch (field) {
    case 'type': return b.type || '—';
    case 'key': return b.key || t('无（未使用索引）');
    case 'rows': return b.rows_known ? Number(b.rows).toLocaleString(locale) : '—';
    case 'sort': return b.filesort ? t('filesort（额外排序）') : t('无');
    default: return b.temporary ? t('临时表') : t('无');
  }
}

export function renderIndexExtras(root, rep) {
  for (const pc of rep.plan_compare || []) {
    section(root, t('建索引前后的执行计划对比：{table}（后者为估算）', { table: pc.table }));
    const wrap = el('div', 'so-tbl-wrap');
    const table = el('table', 'list so-cmp');
    const head = el('tr');
    head.append(el('th', '', t('项目')), el('th', '', t('现在（真实 EXPLAIN）')), el('th', '', t('建 {name} 之后（估算）', { name: pc.new_index })));
    const thead = el('thead');
    thead.append(head);
    const body = el('tbody');
    for (const [label, field] of [[t('访问类型 type'), 'type'], [t('使用的索引 key'), 'key'], [t('扫描行数 rows'), 'rows'], [t('排序'), 'sort'], [t('临时表'), 'tmp']]) {
      const before = planCell(pc.before, field);
      const after = planCell(pc.after, field);
      const tr = el('tr');
      const tdAfter = el('td', before !== after ? 'so-cmp-new' : '', after);
      tr.append(el('td', '', label), el('td', 'mono', before), tdAfter);
      tdAfter.classList.add('mono');
      body.append(tr);
    }
    table.append(thead, body);
    wrap.append(table);
    root.append(wrap);
    if (pc.basis) root.append(el('div', 'muted small', t('估算依据：{text}', { text: pc.basis })));
  }
  const idx = rep.existing_indexes || [];
  if (idx.length) {
    section(root, t('相关表的现有索引'));
    const wrap = el('div', 'so-tbl-wrap');
    const table = el('table', 'list');
    const head = el('tr');
    for (const h of [t('表'), t('索引'), t('列'), t('唯一'), t('基数（估算）'), t('备注')]) head.append(el('th', '', h));
    const thead = el('thead');
    thead.append(head);
    const body = el('tbody');
    for (const ix of idx) {
      const tr = el('tr', ix.redundant ? 'so-redundant' : '');
      tr.append(el('td', 'mono', ix.table), el('td', 'mono', ix.name), el('td', 'mono', (ix.columns || []).join(', ')), el('td', '', ix.unique ? t('是') : ''),
        el('td', 'num', ix.cardinality ? Number(ix.cardinality).toLocaleString(locale) : '—'), el('td', ix.redundant ? 'warn-text' : 'muted', ix.redundant ? t('冗余：{text}', { text: ix.redundant }) : ''));
      body.append(tr);
    }
    table.append(thead, body);
    wrap.append(table);
    root.append(wrap);
  }
}

// 一条 SQL 的完整报告（内置建议 + 索引建议 + 执行计划 + SOAR）
export function renderReport(root, rep) {
  const sc = el('div', 'so-score');
  sc.append(el('span', 'muted', t('综合评分')), el('b', scoreClass(rep.score), String(rep.score)),
    el('span', 'muted', rep.findings.length ? t('{n} 条建议', { n: rep.findings.length }) : t('没有发现问题')));
  root.append(sc);
  if (rep.findings.length) root.append(renderFindings(rep.findings));
  if (rep.index_ddl && rep.index_ddl.length) {
    section(root, t('索引建议'));
    root.append(el('pre', 'sqlbox mono', rep.index_ddl.join('\n')));
  }
  renderIndexExtras(root, rep);
  if (rep.plan_error) root.append(el('div', 'alert warn', t('执行计划获取失败：{text}', { text: rep.plan_error })));
  if (rep.plan) {
    section(root, t('执行计划（EXPLAIN）'));
    root.append(renderTable(rep.plan));
  }
  if (rep.soar) {
    section(root, t('SOAR 报告'));
    root.append(renderMarkdown(rep.soar));
  }
  if (rep.soar_error) root.append(el('div', 'alert warn', t('SOAR：{text}', { text: rep.soar_error })));
  for (const n of rep.notes || []) root.append(el('div', 'alert info', n));
}
