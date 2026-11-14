/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: tree.js
 * @time: 2026/10/28
 */
// 左侧库表树：表 / 视图（估算行数、注释、🔒 无权限、🛡 敏感字段数）、点 ▸ 展开字段（类型、主键、敏感标记）、
// 搜索框只按表名过滤（不看注释和字段名）。全部用 DOM 节点 + textContent 写入，库里的名字和注释不会被当成 HTML。
import { api } from '../common.js';
import { t, isZh } from '../i18n.js';
import { shortCount } from './sqlutil.js';

const SVG_NS = 'http://www.w3.org/2000/svg';

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function tableIcon() {
  const s = document.createElementNS(SVG_NS, 'svg');
  s.setAttribute('class', 'ico');
  s.setAttribute('aria-hidden', 'true');
  const u = document.createElementNS(SVG_NS, 'use');
  u.setAttribute('href', '/static/img/icons.svg#table');
  s.append(u);
  return s;
}

// 把 kw 在 text 里的第一处命中标出来。
function hl(parent, text, kw) {
  const i = kw ? text.toLowerCase().indexOf(kw) : -1;
  if (i < 0) { parent.append(document.createTextNode(text)); return; }
  parent.append(document.createTextNode(text.slice(0, i)));
  parent.append(el('mark', 'tr-hl', text.slice(i, i + kw.length)));
  parent.append(document.createTextNode(text.slice(i + kw.length)));
}

// 新引擎的对象种类（meta 的 tables[].kind）：图标与右侧文字。MySQL 系 / SQL Server 没有 kind 字段，走原来的 view / 行数逻辑。
const KIND_ICON = { view: '◈', matview: '◈', dictionary: '▦', foreign: '⇆', timeseries: '◷' };
const KIND_LABEL = { view: t('视图'), matview: t('物化视图'), dictionary: t('字典'), foreign: t('外部表'), timeseries: t('时序集合'), partitioned: t('分区表') };
const LIST_SEP = isZh ? '、' : ', ';
const LABEL_ONLY = new Set(['view', 'matview', 'dictionary', 'foreign', 'timeseries']); // 这几类右侧只写种类，不写行数

// onContext(t, event, ctx)：右键表名 / 视图名（ctx = { inst, db, type }）。
export function createTree({ container, onTable, onColumn, onLocked, onLoaded, onContext }) {
  let ctx = { inst: '', db: '', type: '' };
  let meta = null;
  let kw = '';
  let seq = 0;
  const open = new Set();
  const closedSchemas = new Set(); // PostgreSQL：被折叠的 schema 分组
  const colCache = new Map(); // 表名 -> 列数组 | Promise

  const base = () => `/api/v1/query/instances/${encodeURIComponent(ctx.inst)}/databases/${encodeURIComponent(ctx.db)}`;

  function hint(text, err) {
    container.replaceChildren(el('div', err ? 'alert err' : 'tree-hint', text));
  }

  function columnsOf(table) {
    const c = colCache.get(table);
    if (c && !c.error) return Promise.resolve(c);
    const p = api('GET', `${base()}/tables/${encodeURIComponent(table)}/schema`).then((cols) => { colCache.set(table, cols); return cols; });
    return p;
  }

  async function toggle(t) {
    if (open.has(t.name)) open.delete(t.name); else open.add(t.name);
    render();
    if (open.has(t.name) && !colCache.has(t.name)) {
      try { await columnsOf(t.name); } catch (e) { colCache.set(t.name, { error: e.message }); }
      render();
    }
  }

  // 只按表名过滤（不区分大小写的子串；PostgreSQL 的表名是 schema.table，两段都能搜）。
  const matches = (t) => !kw || t.name.toLowerCase().includes(kw);

  // 一张表（或视图 / 集合）的一行，展开时跟着字段。shown 是显示名（PG 分组下去掉 schema 前缀）。
  function tableRows(frag, tb, shown) {
    const isOpen = open.has(tb.name);
    const row = el('div', 'tr-row' + (tb.view ? ' view' : '') + (tb.kind && tb.kind !== 'table' && tb.kind !== 'collection' ? ' kind-' + tb.kind : '') + (tb.locked ? ' locked' : ''));
    const arrow = el('button', 'arrow', isOpen ? '▾' : '▸');
    arrow.type = 'button';
    arrow.title = t('展开 / 收起字段');
    arrow.setAttribute('aria-label', isOpen ? t('收起 {name} 的字段', { name: tb.name }) : t('展开 {name} 的字段', { name: tb.name }));
    arrow.setAttribute('aria-expanded', String(isOpen));
    arrow.addEventListener('click', () => toggle(tb));
    const nameBtn = el('button', 'name');
    nameBtn.type = 'button';
    nameBtn.dataset.table = tb.name;
    nameBtn.title = [tb.comment, tb.locked ? t('无查询权限') : ''].filter(Boolean).join('\n');
    if (KIND_ICON[tb.kind]) {
      const ic = el('span', 'ico', KIND_ICON[tb.kind]);
      ic.title = KIND_LABEL[tb.kind] || tb.kind;
      nameBtn.append(ic);
    } else if (tb.view) nameBtn.append(el('span', 'ico', '◈')); else nameBtn.append(tableIcon());
    const label = el('span', 't');
    hl(label, shown, kw);
    nameBtn.append(label);
    if (tb.locked) {
      const lk = el('span', 'tr-lock', '🔒');
      lk.title = t('无查询权限');
      nameBtn.append(lk);
    }
    if (tb.sensitive && tb.sensitive.length) {
      const sh = el('span', 'tr-shield', '🛡' + tb.sensitive.length);
      sh.title = t('敏感字段：{cols}', { cols: tb.sensitive.join(LIST_SEP) });
      nameBtn.append(sh);
    }
    nameBtn.addEventListener('click', () => (tb.locked ? onLocked(tb) : onTable(tb)));
    const labelOnly = tb.kind ? LABEL_ONLY.has(tb.kind) : tb.view;
    const metaSpan = el('span', 'meta', labelOnly ? (KIND_LABEL[tb.kind] || t('视图')) : shortCount(tb.rows));
    if (!labelOnly && tb.rows >= 0) metaSpan.title = tb.kind === 'partitioned' ? t('分区表，估算约 {n} 行', { n: tb.rows }) : t('估算约 {n} 行', { n: tb.rows });
    row.append(arrow, nameBtn, metaSpan);
    if (onContext) row.addEventListener('contextmenu', (e) => { e.preventDefault(); onContext(tb, e, { ...ctx }); });
    frag.append(row);
    if (isOpen) {
      const cols = colCache.get(tb.name);
      if (!cols) frag.append(el('div', 'tree-hint', t('加载字段…')));
      else if (cols.error) frag.append(el('div', 'tree-hint', t('加载字段失败：{err}', { err: cols.error })));
      else {
        for (const c of cols) {
          const b = el('button', 'tr-col');
          b.type = 'button';
          b.title = c.comment || '';
          if (c.key === 'PRI') b.append(el('span', 'pk', '🔑'));
          b.append(el('span', 'cn', c.name));
          const ct = el('span', 'ct', c.type + (c.sensitive ? ' · 🛡' : ''));
          b.append(ct);
          b.addEventListener('click', () => onColumn(c.name));
          frag.append(b);
        }
      }
    }
  }

  // PostgreSQL：按 schema 分组（public 排第一），点分组标题折叠 / 展开；过滤时全部展开。
  function schemaGroups(list) {
    const groups = new Map();
    for (const t of list) {
      const sc = t.schema || (t.name.includes('.') ? t.name.slice(0, t.name.indexOf('.')) : 'public');
      if (!groups.has(sc)) groups.set(sc, []);
      groups.get(sc).push(t);
    }
    return [...groups.entries()].sort((a, b) => (a[0] === 'public' ? -1 : b[0] === 'public' ? 1 : a[0].localeCompare(b[0])));
  }

  function render() {
    if (!meta) return;
    const shown = meta.tables.filter(matches);
    const frag = document.createDocumentFragment();
    const head = el('div', 'tree-head');
    const count = ctx.type === 'mongo' ? t('{n} 个集合', { n: shown.length }) : t('{n} 个对象', { n: shown.length });
    head.append(el('span', '', '⛁ ' + meta.db), el('span', 'n', count));
    frag.append(head);
    if (!shown.length) frag.append(el('div', 'tree-hint', kw ? (ctx.type === 'mongo' ? t('没有匹配的集合') : t('没有匹配的表名')) : ctx.type === 'mongo' ? t('这个库里没有集合') : t('这个库里没有表')));
    if (ctx.type === 'pgsql') {
      for (const [sc, list] of schemaGroups(shown)) {
        const folded = closedSchemas.has(sc) && !kw;
        const gh = el('button', 'tr-group');
        gh.type = 'button';
        gh.setAttribute('aria-expanded', String(!folded));
        gh.title = folded ? t('展开 schema') : t('折叠 schema');
        gh.append(el('span', 'arrow', folded ? '▸' : '▾'), el('span', 'gn', sc), el('span', 'n', String(list.length)));
        gh.addEventListener('click', () => { if (closedSchemas.has(sc)) closedSchemas.delete(sc); else closedSchemas.add(sc); render(); });
        frag.append(gh);
        if (folded) continue;
        for (const tb of list) tableRows(frag, tb, tb.name.startsWith(sc + '.') ? tb.name.slice(sc.length + 1) : tb.name);
      }
    } else {
      for (const tb of shown) tableRows(frag, tb, tb.name);
    }
    container.replaceChildren(frag);
  }

  async function load(next) {
    ctx = { ...ctx, ...next };
    const my = ++seq;
    meta = null;
    open.clear();
    closedSchemas.clear();
    colCache.clear();
    if (!ctx.inst || !ctx.db) { hint(t('选择实例和数据库后显示表')); onLoaded(null); return; }
    hint(t('加载中…'));
    try {
      const m = await api('GET', `${base()}/meta`);
      if (my !== seq) return;
      meta = m;
      render();
      onLoaded(m);
    } catch (e) {
      if (my !== seq) return;
      hint(e.message, true);
      onLoaded(null);
    }
  }

  return {
    load,
    columnsOf,
    meta: () => meta,
    tables: () => (meta ? meta.tables : []),
    setFilter(text) {
      kw = text.trim().toLowerCase();
      render();
    },
  };
}
