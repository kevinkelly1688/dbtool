/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_opt.js
 * @time: 2026/10/28
 */
/* SQL 优化：优化工具（sqladvisor）与慢查日志（slowquery）
   规格：internal/sqlopt、internal/slowlog。
   分析引擎是词法级的“迷你优化器”：按 data.js 的表结构估算执行计划，规则编号沿用 internal/sqlopt 的内置规则（R/S/P）。 */
(function () {
'use strict';

/* ========================================================================
   1. 通用小工具与样式
   ======================================================================== */
const fi = n => Math.round(n).toLocaleString('en-US');
const pctS = v => (Math.round(v * 100) / 100).toFixed(2);
const clamp = (v, a, b) => Math.min(b, Math.max(a, v));
const daysBetween = (a, b) => Math.round((Date.parse(b + 'T00:00:00') - Date.parse(a + 'T00:00:00')) / 864e5);   // b - a
const LV_CLS = {'高': 'bad', '中': 'warn', '低': 'info', '提示': ''};
const CSS = `<style>
.op-acc{border:1px solid var(--border);border-radius:10px;margin-bottom:8px;background:var(--panel)}
.op-acc>summary{padding:8px 12px;cursor:pointer;font-weight:600}
.op-acc>.op-in{padding:0 12px 12px}
.op-find{border:1px solid var(--border);border-left-width:4px;border-radius:8px;padding:8px 12px;margin-bottom:8px;background:var(--panel)}
.op-find.bad{border-left-color:var(--bad)}.op-find.warn{border-left-color:var(--warn)}.op-find.info{border-left-color:var(--accent)}
.op-find .t{font-weight:600;display:flex;gap:8px;align-items:center;flex-wrap:wrap}
.op-find .d{font-size:13px;margin-top:4px}.op-find .f{font-size:12px;color:var(--muted);margin-top:4px}
.op-bar{height:12px;border-radius:4px;background:var(--panel2);border:1px solid var(--border);overflow:hidden;min-width:80px}
.op-bar b{display:block;height:100%;background:var(--c1)}.op-bar b.hot{background:var(--warn)}
tr.op-bad>td{background:var(--bad-soft)}tr.op-warn>td{background:var(--warn-soft)}
.op-sql{max-width:460px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:12px}
.op-th a{color:inherit;cursor:pointer;text-decoration:none}.op-th a:hover{color:var(--brand)}
.op-fl{display:flex;flex-direction:column;gap:2px;font-size:12px;color:var(--muted)}
.op-fl select,.op-fl input{min-width:110px}
.op-pre{font:12px/1.55 ui-monospace,Consolas,monospace;white-space:pre-wrap;word-break:break-all;margin:0}
.op-num{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap}
</style>`;
const durS = ms => ms >= 1000 ? (ms / 1000).toFixed(2) + ' s' : ms >= 10 ? ms.toFixed(1) + ' ms' : ms >= 1 ? ms.toFixed(2) + ' ms' : ms.toFixed(3) + ' ms';
const lvTag = lv => `<span class="tag ${LV_CLS[lv]}">${lv}</span>`;
const findingsHtml = list => list.map(f => `<div class="op-find ${LV_CLS[f.lv]}"><div class="t">${lvTag(f.lv)}<span class="tag">${esc(f.id)}</span>${esc(f.title)}</div>${f.detail ? `<div class="d">${f.detail}</div>` : ''}${f.fix ? `<div class="f">建议：${f.fix}</div>` : ''}</div>`).join('');
const scoreOf = list => Math.max(0, 100 - 20 * list.filter(f => f.lv === '高').length - 10 * list.filter(f => f.lv === '中').length - 3 * list.filter(f => f.lv === '低').length);
const tickCode = (text, lang) => `<pre class="code"><button class="cp" data-copy="${esc(text)}">复制</button>${lang === 'sql' ? sqlHl(text) : esc(text)}</pre>`;

/* ========================================================================
   2. SQL 解析（词法级：括号深度 + 顶层关键字切分，不依赖语法树）
   ======================================================================== */
const stripComments = s => s.replace(/\/\*[\s\S]*?\*\//g, ' ').replace(/(^|[ \t])--[^\n]*/g, ' ').replace(/(^|\n)[ \t]*#[^\n]*/g, '$1');
const maskStr = s => s.replace(/'(?:[^'\\]|\\[\s\S]|'')*'|"(?:[^"\\]|\\[\s\S])*"/g, q => q[0] + 'x'.repeat(Math.max(0, q.length - 2)) + q[q.length - 1]);
function depthMap(m) { const d = new Array(m.length); let k = 0; for (let i = 0; i < m.length; i++) { if (m[i] === ')') k--; d[i] = k; if (m[i] === '(') k++; } return d; }
function matchParen(m, i) { let d = 0; for (let k = i; k < m.length; k++) { if (m[k] === '(') d++; else if (m[k] === ')') { d--; if (!d) return k; } } return -1; }
function splitComma(s) {
  const m = maskStr(s), out = []; let d = 0, last = 0;
  for (let i = 0; i < m.length; i++) { const ch = m[i]; if (ch === '(') d++; else if (ch === ')') d--; else if (ch === ',' && !d) { out.push(s.slice(last, i).trim()); last = i + 1; } }
  out.push(s.slice(last).trim()); return out.filter(x => x !== '');
}
function findTop(P, re, s, e) {
  const g = new RegExp(re.source, 'gi'); g.lastIndex = s; const seg = P.m.slice(0, e); let x;
  while ((x = g.exec(seg))) { if (P.dm[x.index] === 0) return x; if (!x[0].length) g.lastIndex++; }
  return null;
}
function splitTop(P, re, s, e) {
  const g = new RegExp(re.source, 'gi'); g.lastIndex = s; const seg = P.m.slice(0, e), out = []; let last = s, pre = '', x;
  while ((x = g.exec(seg))) {
    if (!x[0].length) { g.lastIndex++; continue; }
    if (P.dm[x.index] !== 0) continue;
    out.push({s: last, e: x.index, pre}); pre = x[0].toLowerCase(); last = x.index + x[0].length;
  }
  out.push({s: last, e, pre}); return out;
}
const CMP_OPS = '<=>|!=|<>|>=|<=|=|>|<|\\bnot\\s+like\\b|\\blike\\b|\\bnot\\s+in\\b|\\bin\\b|\\bbetween\\b|\\bis\\s+not\\b|\\bis\\b';
const RE_FN = new RegExp('^([a-z_]\\w*)\\s*\\(\\s*(?:([a-z_]\\w*)\\s*\\.\\s*)?([a-z_]\\w*)\\s*(?:,[^()]*)?\\)\\s*(' + CMP_OPS + ')\\s*([\\s\\S]*)$', 'i');
const RE_COL = new RegExp('^(?:([a-z_]\\w*)\\s*\\.\\s*)?([a-z_]\\w*)\\s*([-+*/]\\s*[\\w.]+\\s*)?(' + CMP_OPS + ')\\s*([\\s\\S]*)$', 'i');
const FN_SKIP = new Set(['not', 'exists', 'in', 'and', 'or', 'interval', 'case', 'if', 'select']);
function classifyRhs(r) {
  r = r.trim();
  if (/^\(\s*select\b/i.test(r)) return {k: 'sub'};
  if (r[0] === '(' && r[r.length - 1] === ')') { const items = splitComma(r.slice(1, -1)); return {k: 'list', items, num: items.length > 0 && items.every(x => /^-?\d+(\.\d+)?$/.test(x))}; }
  if (r[0] === "'" || r[0] === '"') return {k: 'str', v: r.slice(1, r.lastIndexOf(r[0]))};
  if (/^-?\d+(\.\d+)?$/.test(r)) return {k: 'num', v: r};
  if (r === '?') return {k: 'ph'};
  if (/^null$/i.test(r)) return {k: 'null'};
  let m = /^([a-z_]\w*)\s*\.\s*([a-z_]\w*)$/i.exec(r); if (m) return {k: 'col', alias: m[1], col: m[2]};
  if (/^[a-z_]\w*$/i.test(r)) return {k: 'col', alias: '', col: r};
  if (/^\d+(\.\d+)?\s+and\s+/i.test(r) || /^'[^']*'\s+and\s+/i.test(r) || /^\?\s+and\s+/i.test(r)) return {k: 'between'};
  return {k: 'expr', v: r};
}
function matchWhole(mt) { return mt[0] === '(' && matchParen(mt, 0) === mt.length - 1; }
function parseCond(txt, mt) {
  txt = txt.trim(); mt = mt.trim();
  while (mt && matchWhole(mt)) { txt = txt.slice(1, -1).trim(); mt = mt.slice(1, -1).trim(); }
  const c = {text: txt, kind: 'cmp', neg: false};
  let flat = mt; for (let i = 0; i < 6; i++) flat = flat.replace(/\([^()]*\)/g, '()');
  if (/^(not\s+)?exists\b/i.test(mt)) { c.kind = 'exists'; return c; }
  if (/\b(and|or)\b/i.test(flat.replace(/\bbetween\b[\s\S]*?\band\b/ig, 'between'))) { c.kind = 'group'; return c; }
  if (/^not\s+/i.test(mt)) { c.neg = true; txt = txt.replace(/^not\s+/i, ''); mt = mt.replace(/^not\s+/i, ''); }
  let x = RE_FN.exec(mt), isFn = false;
  if (x && !FN_SKIP.has(x[1].toLowerCase())) isFn = true; else x = RE_COL.exec(mt);
  if (!x) { c.kind = 'expr'; return c; }
  const opIdx = isFn ? 4 : 4, rhsM = x[5];
  c.fn = isFn ? x[1].toLowerCase() : ''; c.arith = !isFn && !!x[3];
  c.alias = isFn ? (x[2] || '') : (x[1] || ''); c.col = (isFn ? x[3] : x[2]).toLowerCase();
  c.op = x[opIdx].toLowerCase().replace(/\s+/g, ' ');
  c.rhs = txt.slice(txt.length - rhsM.length).trim();
  c.rhsK = classifyRhs(c.rhs);
  if (c.op === 'between') c.rhsK = {k: 'between'};
  return c;
}
function andConds(P, s, e) {
  const pcs = splitTop(P, /\band\b/, s, e), spans = []; let cur = null;
  for (const pc of pcs) {
    cur = cur ? {s: cur.s, e: pc.e} : {s: pc.s, e: pc.e};
    const mt = P.m.slice(cur.s, cur.e), bi = mt.search(/\bbetween\b/i);
    if (bi >= 0 && !/\band\b/i.test(mt.slice(bi + 7))) continue;
    spans.push(cur); cur = null;
  }
  if (cur) spans.push(cur);
  return spans.map(sp => parseCond(P.sql.slice(sp.s, sp.e), P.m.slice(sp.s, sp.e))).filter(c => c.text);
}
function parseItem(t) {   // ORDER BY / GROUP BY 项
  const m = /^(?:([a-z_]\w*)\s*\.\s*)?([a-z_]\w*)(?:\s+(asc|desc))?$/i.exec(t.trim());
  return m ? {alias: m[1] || '', col: m[2].toLowerCase(), dir: (m[3] || 'asc').toLowerCase()} : {expr: true, text: t.trim()};
}
function parseRefs(P, s, e) {
  const pcs = splitTop(P, /,|\b(?:(?:left|right|inner|cross|natural)\s+(?:outer\s+)?)?join\b|\bstraight_join\b/, s, e);
  return pcs.filter(pc => P.sql.slice(pc.s, pc.e).trim()).map(pc => {
    const r = {s: pc.s, e: pc.e, jt: /left/.test(pc.pre) ? 'left' : /right/.test(pc.pre) ? 'right' : pc.pre === ',' ? 'comma' : pc.pre ? 'inner' : '', onConds: [], on: '', using: null, derived: null, db: '', name: '', alias: ''};
    const on = findTop(P, /\bon\b|\busing\b/, pc.s, pc.e); let end = pc.e;
    if (on) {
      end = on.index; r.on = P.sql.slice(on.index + on[0].length, pc.e).trim();
      if (/using/i.test(on[0])) r.using = ((/\(([^)]*)\)/.exec(r.on) || [0, ''])[1]).split(',').map(x => x.trim().toLowerCase()).filter(Boolean);
      else r.onConds = andConds(P, on.index + on[0].length, pc.e);
    }
    const ref = P.sql.slice(pc.s, end).trim().replace(/\b(?:use|force|ignore)\s+(?:index|key)\s*(?:for\s+\w+(?:\s+by)?\s*)?\([^)]*\)/ig, '').trim();
    if (ref[0] === '(') {
      const close = matchParen(maskStr(ref), 0), am = /^\s*(?:as\s+)?([a-z_]\w*)/i.exec(ref.slice(close + 1));
      r.derived = parseStmt(ref.slice(1, close)); r.alias = r.name = am ? am[1] : 'derived';
    } else {
      const m = /^([a-z_][\w$]*)(?:\s*\.\s*([a-z_][\w$]*))?(?:\s+(?:as\s+)?([a-z_]\w*))?$/i.exec(ref);
      if (m) { r.db = m[2] ? m[1] : ''; r.name = m[2] || m[1]; r.alias = m[3] || r.name; } else { r.name = ref; r.alias = ref; r.bad = true; }
    }
    return r;
  });
}
function parseStmt(raw) {
  const sql = stripComments(String(raw || '')).replace(/`/g, '').trim().replace(/[\s;]+$/, '');
  const P = {sql, m: maskStr(sql), refs: [], subs: [], conds: [], orBranches: null, hasOr: false, orderItems: [], groupItems: [], selItems: [], selText: '', limit: null, distinct: false, agg: false};
  P.dm = depthMap(P.m);
  const lead = /^[\s(]*(with|select|insert|replace|update|delete|explain|show|set|create|alter|drop|truncate|call|use|grant)\b/i.exec(P.m);
  P.kind = lead ? lead[1].toLowerCase() : 'other';
  if (P.kind === 'with') { P.kind = 'select'; P.cte = true; } if (P.kind === 'replace') P.kind = 'insert';
  P.hits = [];
  const g = /\b(select|from|where|group\s+by|order\s+by|having|limit|union(?:\s+all)?|set|values|for\s+update|on\s+duplicate\s+key\s+update|update|delete)\b/gi; let x;
  while ((x = g.exec(P.m))) if (P.dm[x.index] === 0) P.hits.push({kw: x[1].toLowerCase().replace(/\s+/g, ' '), s: x.index, e: x.index + x[0].length});
  P.sec = name => { const i = P.hits.findIndex(h => h.kw === name); if (i < 0) return null; return {s: P.hits[i].e, e: P.hits[i + 1] ? P.hits[i + 1].s : P.sql.length}; };
  P.text = name => { const r = P.sec(name); return r ? P.sql.slice(r.s, r.e).trim() : ''; };
  if (P.hits.some(h => h.kw.startsWith('union'))) {
    const pcs = splitTop(P, /\bunion(?:\s+all)?\b/, 0, P.sql.length);
    P.parts = pcs.map(pc => parseStmt(P.sql.slice(pc.s, pc.e).trim().replace(/^\((.*)\)$/s, '$1')));
    P.unionAll = pcs.slice(1).every(pc => /all/.test(pc.pre)); P.kind = 'select'; return P;
  }
  if (P.kind === 'insert') { const h = P.hits.find(h => h.kw === 'select'); if (h) P.inner = parseStmt(P.sql.slice(h.s)); return P; }
  if (!['select', 'update', 'delete'].includes(P.kind)) return P;
  const from = P.kind === 'update' ? P.sec('update') : P.sec('from');
  if (from) P.refs = parseRefs(P, from.s, from.e);
  const w = P.sec('where');
  if (w) {
    const ors = splitTop(P, /\bor\b/, w.s, w.e);
    if (ors.length > 1) { P.hasOr = true; P.orBranches = ors.map(o => andConds(P, o.s, o.e)); P.conds = [].concat(...P.orBranches); }
    else P.conds = andConds(P, w.s, w.e);
  }
  const sel = P.sec('select');
  if (sel) { let t = P.sql.slice(sel.s, sel.e).trim(); if (/^distinct\b/i.test(t)) { P.distinct = true; t = t.replace(/^distinct\s+/i, ''); } P.selText = t; P.selItems = splitComma(t); }
  const ob = P.sec('order by'), gb = P.sec('group by');
  P.orderItems = ob ? splitComma(P.sql.slice(ob.s, ob.e)).map(parseItem) : [];
  P.groupItems = gb ? splitComma(P.sql.slice(gb.s, gb.e)).map(parseItem) : [];
  const lx = /^(\?|\d+)\s*(?:,\s*(\?|\d+)|offset\s+(\?|\d+))?/i.exec(P.text('limit'));
  if (lx) P.limit = lx[2] != null ? {off: lx[1], cnt: lx[2]} : lx[3] != null ? {off: lx[3], cnt: lx[1]} : {off: '0', cnt: lx[1]};
  P.agg = /\b(count|sum|avg|max|min|group_concat)\s*\(/i.test(P.selText);
  // 子查询（最外层括号内的 select；FROM 里的派生表已在 refs 中）
  const re = /\(\s*select\b/gi; let skip = -1, y;
  const dsp = P.refs.filter(r => r.derived).map(r => [r.s, r.e]);
  while ((y = re.exec(P.m))) {
    if (y.index < skip) continue; const close = matchParen(P.m, y.index); if (close < 0) break; skip = close;
    if (dsp.some(([a, b]) => y.index >= a && y.index < b)) continue;
    const before = P.m.slice(0, y.index), h = [...P.hits].reverse().find(h => h.s <= y.index);
    const inner = parseStmt(P.sql.slice(y.index + 1, close)), innerAl = new Set(inner.refs.map(r => r.alias.toLowerCase()));
    const dep = P.refs.some(r => !innerAl.has(r.alias.toLowerCase()) && new RegExp('\\b' + r.alias + '\\s*\\.', 'i').test(inner.sql));
    P.subs.push({P: inner, pos: h ? h.kw : '', kind: /\b(not\s+)?in\s*$/i.test(before) ? 'in' : /\bexists\s*$/i.test(before) ? 'exists' : 'scalar', dep});
  }
  return P;
}
function allBlocks(P, acc = []) {
  if (P.parts) { P.parts.forEach(p => allBlocks(p, acc)); return acc; }
  if (P.inner) allBlocks(P.inner, acc);
  acc.push(P); P.refs.forEach(r => r.derived && allBlocks(r.derived, acc)); P.subs.forEach(s => allBlocks(s.P, acc));
  return acc;
}
function fingerprint(sql) {
  let s = stripComments(String(sql)).replace(/\s+/g, ' ').trim().replace(/;$/, '');
  s = s.replace(/'(?:[^'\\]|\\.|'')*'|"(?:[^"\\]|\\.)*"/g, '?').replace(/\b0x[0-9a-f]+\b/gi, '?').replace(/\b\d+(?:\.\d+)?\b/g, '?');
  s = s.replace(/\(\s*\?(?:\s*,\s*\?)+\s*\)/g, '(?+)').replace(/\blimit\s+\?\s*,\s*\?/gi, 'limit ?').replace(/\blimit\s+\?\s+offset\s+\?/gi, 'limit ?');
  return s.toLowerCase();
}

/* ========================================================================
   3. 迷你优化器：按 SCHEMAS 估算 EXPLAIN（访问类型 / 索引选择 / 行数 / Extra）
   ======================================================================== */
const WIN = 730;   // 假定表里的数据跨度（天），用于估算时间范围条件的选择率
function mkCtx(instId, db) { const inst = instOf(instId); return {inst, db, dbs: dbsOf(instId).map(d => d.db), v8: parseInt(inst.ver, 10) >= 8}; }
const tableDef = (ctx, db, name) => tablesOf(ctx.inst.id, db || ctx.db).find(t => t.n.toLowerCase() === String(name).toLowerCase() && !t.view);
const isStrCol = (def, col) => { const c = def && def.cols.find(x => x.n === col); return !!c && /char|text/i.test(c.t); };
function colLen(c) {
  const t = c.t.toLowerCase(); let n = 8, m;
  if (/^tinyint/.test(t)) n = 1; else if (/^smallint/.test(t)) n = 2; else if (/^mediumint/.test(t)) n = 3; else if (/^int/.test(t)) n = 4; else if (/^bigint/.test(t)) n = 8;
  else if ((m = /^decimal\((\d+),(\d+)\)/.exec(t))) { const bytes = d => Math.floor(d / 9) * 4 + [0, 1, 1, 2, 2, 3, 3, 4, 4][d % 9]; n = bytes(+m[1] - +m[2]) + bytes(+m[2]); }
  else if (/^datetime|^timestamp/.test(t)) n = 5; else if (/^date/.test(t)) n = 3;
  else if ((m = /^varchar\((\d+)\)/.exec(t))) n = +m[1] * 4 + 2; else if ((m = /^char\((\d+)\)/.exec(t))) n = +m[1] * 4;
  return n + (c.nul ? 1 : 0);
}
function rpk(def, col) {   // 非唯一等值时，每个键值平均命中的行数（原型的估算口径）
  const R = def.rows || 1, c = def.cols.find(x => x.n === col) || {t: ''}, n = col.toLowerCase();
  if (n === 'id' || (def.idx || []).some(ix => ix[2] && ix[1].length === 1 && ix[1][0] === col)) return 1;
  if (/phone|email|username|card_no|id_card|order_no|pay_no|refund_no/.test(n)) return Math.min(R, 1.3);
  if (n === 'user_id') return 4.8; if (/order_id/.test(n)) return 2.4; if (/sku_id/.test(n)) return Math.min(R, 380);
  if (/status|level|type|channel|action|state/.test(n) || /^tinyint/.test(c.t)) return R / 6;
  if (/_id$/.test(n)) return Math.min(R, 8);
  if (/created_at|paid_at|updated_at/.test(n)) return 3;
  return Math.max(1, R / 50);
}
function rangeFrac(def, col, c) {
  const cd = def.cols.find(x => x.n === col) || {t: ''};
  if (c && /date|time/.test(cd.t)) {
    const days = (c.rhs.match(/\d{4}-\d{2}-\d{2}/g) || []).map(d => daysBetween(d, TODAY));
    if (c.op === 'between' && days.length >= 2) return clamp((Math.abs(days[0] - days[1]) + 1) / WIN, .0005, 1);
    if (days.length) return /^>/.test(c.op) ? clamp((days[0] + 1) / WIN, .0005, 1) : clamp(1 - (days[0] + 1) / WIN, .0005, 1);
    const iv = /interval\s+(\d+)\s+(day|hour|week|month)/i.exec(c.rhs);
    if (iv && /^>/.test(c.op)) return clamp(+iv[1] * ({hour: 1 / 24, day: 1, week: 7, month: 30}[iv[2].toLowerCase()]) / WIN, .0005, 1);
  }
  if (c && c.op === 'like') return .005;
  return c && c.op === 'between' ? .11 : .33;
}
function condClass(c) {
  if (c.kind !== 'cmp' || c.neg) return 'other';
  if (c.fn || c.arith) return 'unusable';
  const op = c.op, k = c.rhsK;
  if (op === '=' || op === '<=>') return k.k === 'null' ? 'unusable' : 'eq';
  if (op === 'is') return k.k === 'null' ? 'eq' : 'unusable';
  if (op === 'in') return 'in';
  if (['>', '<', '>=', '<=', 'between'].includes(op)) return 'range';
  if (op === 'like') return k.k === 'str' ? (/^[%_]/.test(k.v) ? 'unusable' : 'range') : k.k === 'ph' ? 'range' : 'unusable';
  return 'unusable';
}
const condFactor = c => ({eq: .1, in: .25, range: c.op === 'between' ? .1111 : c.op === 'like' ? .2 : .3333, conv: .1}[c.cls] || (c.kind === 'group' ? .25 : c.cls === 'unusable' ? ({'=': .1, 'like': .2, '!=': .9, '<>': .9}[c.op] || .3333) : .3333));
function locate(tabs, alias, col) {
  if (alias) { const a = alias.toLowerCase(); return tabs.find(t => t.alias.toLowerCase() === a || t.name.toLowerCase() === a) || null; }
  return tabs.find(t => t.def && t.def.cols.some(c => c.n === col)) || null;
}
const newRow = o => Object.assign({id: null, select_type: 'SIMPLE', table: null, type: null, possible: [], key: null, keyLen: null, ref: null, rows: null, filtered: null, extra: [], flags: []}, o);

function accessPath(T, ctx, extra, jm) {
  const def = T.def, R = def.rows || 1, eq = new Map(), rng = new Map();
  T.eq.forEach((c, col) => eq.set(col, c.outerRef ? {c, join: c.outerRef} : {c}));
  T.inL.forEach((c, col) => eq.set(col, {c, inl: c.rhsK.k === 'list' ? c.rhsK.items.length : 50}));
  if (jm) jm.forEach((ref, col) => { if (!eq.has(col)) eq.set(col, {join: ref}); });
  T.rng.forEach((c, col) => rng.set(col, c));
  const idxs = (def.idx || []).concat(extra[def.n] || []);
  const poss = idxs.filter(ix => eq.has(ix[1][0]) || rng.has(ix[1][0]) || T.conv.has(ix[1][0])).map(ix => ix[0]);
  let best = null;
  if (!T.hasOr) idxs.forEach(ix => {
    let ue = 0, ur = '', inl = 0; const jr = [];
    for (const col of ix[1]) {
      const e = eq.get(col);
      if (e) { ue++; if (e.inl) inl = Math.max(inl, e.inl); jr.push(e.join || 'const'); } else if (rng.has(col)) { ur = col; break; } else break;
    }
    const n = ue + (ur ? 1 : 0); if (!n) return;
    const full = !!ix[2] && ue === ix[1].length && !inl, ordHit = T.ordCols && T.ordCols.length && !inl && T.ordCols.every((c, k) => ix[1][ue + k] === c);
    const score = n * 10 + (full ? 30 : 0) + (ordHit ? 5 : 0) + (ix[0] === 'PRIMARY' ? 1 : 0) - ix[1].length * .1;
    if (!best || score > best.score) best = {ix, ue, ur, inl, jr, score, full};
  });
  const ap = {possible: poss, ix: null, ue: 0, ur: '', inl: 0, ref: null, keyLen: null, used: new Set(), covering: false};
  if (!best) { ap.type = 'ALL'; ap.rows = R; }
  else {
    const {ix, ue, ur, inl, jr, full} = best, cols = ix[1]; let rows = R;
    if (full) rows = 1;
    else {
      for (let i = 0; i < ue; i++) rows = i === 0 ? Math.min(R, rpk(def, cols[0]) * (inl || 1)) : Math.max(1, rows / 8);
      if (ur) rows = Math.max(1, (ue ? rows : R) * rangeFrac(def, ur, rng.get(ur)));
    }
    Object.assign(ap, {ix, ue, ur, inl, rows: Math.min(R, rows), used: new Set(cols.slice(0, ue + (ur ? 1 : 0)))});
    const hasJoin = jr.some(x => x !== 'const');
    ap.type = full ? (hasJoin ? 'eq_ref' : 'const') : (ur || inl) ? 'range' : 'ref';
    ap.ref = ap.type === 'range' ? null : jr.join(',');
    ap.keyLen = cols.slice(0, ue + (ur ? 1 : 0)).reduce((a, c) => a + colLen(def.cols.find(x => x.n === c) || {t: 'bigint'}), 0);
  }
  let f = 1, unconsumed = 0;
  T.all.forEach(c => { if (ap.used.has(c.col) && ['eq', 'in', 'range'].includes(c.cls)) return; f *= condFactor(c); unconsumed++; });
  if (jm) jm.forEach((ref, col) => { if (!ap.used.has(col)) { f *= .1; unconsumed++; } });
  ap.filtered = ap.type === 'const' || ap.type === 'eq_ref' && !unconsumed ? 100 : Math.max(.01, f * 100);
  ap.where = unconsumed > 0; ap.rows = Math.max(1, ap.rows);
  return ap;
}
function refCols(P, T) {   // 语句里引用到 T 的哪些列（用于判断覆盖索引）
  const set = new Set(); let star = false;
  P.selItems.forEach(i => { if (i === '*' || new RegExp('^' + T.alias + '\\.\\*$', 'i').test(i)) star = true; });
  const re = /([a-z_]\w*)\s*\.\s*([a-z_]\w*)|(?<![\w.])([a-z_]\w*)(?!\s*\()/gi; let x;
  while ((x = re.exec(P.m))) {
    if (x[1]) { if (x[1].toLowerCase() === T.alias.toLowerCase() || x[1].toLowerCase() === T.name.toLowerCase()) set.add(x[2].toLowerCase()); }
    else if (T.def.cols.some(c => c.n === x[3].toLowerCase())) set.add(x[3].toLowerCase());
  }
  return {set, star};
}
function serves(items, T, tabs, ap, strict) {   // 驱动表索引能否提供 ORDER BY / GROUP BY 的有序性
  if (!items.length) return true;
  if (items.some(x => x.expr) || !ap.ix) return false;
  if (!items.every(x => locate(tabs, x.alias, x.col) === T)) return false;
  if (strict && (ap.inl || new Set(items.map(x => x.dir)).size > 1)) return false;
  const eqPart = ap.ix[1].slice(0, ap.ue), rest = ap.ix[1].slice(ap.ue);
  return items.every(x => eqPart.includes(x.col)) || items.every((x, k) => rest[k] === x.col);
}

function planBlock(P, ctx, extra, out, acc, sel, outerAliases) {
  const id = acc.n++, v8 = ctx.v8, info = {id, prod: 1};
  const wt = P.text('where');
  const tabs = P.refs.map(r => ({r, alias: r.alias, name: r.name, jt: r.jt, derived: r.derived, def: r.derived ? null : tableDef(ctx, r.db, r.name),
    eq: new Map(), inL: new Map(), rng: new Map(), conv: new Set(), all: [], jc: [], hasOr: P.hasOr, dRows: 1, dId: 0})).filter(t => t.def || t.derived);
  const tail = [];
  if (/^(1\s*=\s*0|false|0)$/i.test(wt)) out.push(newRow({id, select_type: sel, extra: ['Impossible WHERE'], flags: ['impossible']}));
  else if (!tabs.length) out.push(newRow({id, select_type: sel, extra: ['No tables used']}));
  else {
    tabs.forEach(T => { if (T.derived) { const bi = planBlock(T.derived, ctx, extra, tail, acc, 'DERIVED', []); T.dId = bi.id; T.dRows = Math.max(1, bi.prod); } });
    // 条件归属：WHERE（无顶层 OR 时）+ 各 JOIN 的 ON
    const conds = P.hasOr ? [] : P.conds.slice();
    P.refs.forEach(r => (r.onConds || []).forEach(c => conds.push(c)));
    P.refs.forEach((r, i) => (r.using || []).forEach(col => { const prev = tabs.find((t, j) => t.def && j < i && t.def.cols.some(c => c.n === col)); if (prev) conds.push({kind: 'cmp', op: '=', alias: r.alias, col, rhsK: {k: 'col', alias: prev.alias, col}, rhs: '', text: ''}); }));
    if (P.hasOr) tabs[0].all.push({kind: 'group', cls: 'other', text: 'or', col: ''});
    conds.forEach(c => {
      if (c.kind !== 'cmp') { const T = tabs.find(t => new RegExp('\\b' + t.alias + '\\s*\\.', 'i').test(c.text)) || tabs[0]; c.cls = 'other'; T.all.push(c); return; }
      const L = locate(tabs, c.alias, c.col); if (!L) return;
      const rk = c.rhsK;
      if (rk.k === 'col' && (c.op === '=' || c.op === '<=>')) {
        const Rt = locate(tabs, rk.alias, rk.col.toLowerCase());
        if (Rt && Rt !== L) { L.jc.push({col: c.col, o: Rt, oc: rk.col.toLowerCase()}); Rt.jc.push({col: rk.col.toLowerCase(), o: L, oc: c.col}); return; }
        if (!Rt && rk.alias && outerAliases.includes(rk.alias.toLowerCase())) { c.cls = 'eq'; c.outerRef = `${ctx.db}.${rk.alias}.${rk.col.toLowerCase()}`; L.eq.set(c.col, c); L.all.push(c); return; }
        if (Rt === L) { c.cls = 'other'; L.all.push(c); return; }
      }
      c.cls = condClass(c); L.all.push(c);
      if (L.def && isStrCol(L.def, c.col) && ['eq', 'in', 'range'].includes(c.cls) && (rk.k === 'num' || (rk.k === 'list' && rk.num))) { c.cls = 'conv'; c.conv = true; L.conv.add(c.col); return; }
      if (c.cls === 'eq') L.eq.set(c.col, c); else if (c.cls === 'in') L.inL.set(c.col, c); else if (c.cls === 'range' && !L.rng.has(c.col)) L.rng.set(c.col, c);
    });
    const derivedAp = T => ({type: 'ALL', ix: null, ue: 0, ur: '', inl: 0, ref: null, keyLen: null, used: new Set(), possible: [], rows: T.dRows, filtered: Math.max(.01, T.all.reduce((a, c) => a * condFactor(c), 1) * 100), where: T.all.length > 0});
    const joinMap = (T, done) => { const m = new Map(); T.jc.forEach(j => { if (done.includes(j.o) && !m.has(j.col)) m.set(j.col, `${ctx.db}.${j.o.alias}.${j.oc}`); }); return m; };
    const apOf = (T, jm) => T.derived ? derivedAp(T) : accessPath(T, ctx, extra, jm);
    const cost = (T, ap) => ap.rows * ap.filtered / 100;
    let order;
    if (tabs.length === 1 || tabs.some(t => t.jt === 'left' || t.jt === 'right')) order = tabs.slice();
    else {
      const left = tabs.slice().sort((a, b) => cost(a, apOf(a, null)) - cost(b, apOf(b, null)));
      order = [left.shift()];
      while (left.length) {
        let best = null, bc = Infinity;
        left.forEach(T => { const jm = joinMap(T, order), c = cost(T, apOf(T, jm)) + (jm.size ? 0 : 1e13); if (c < bc) { bc = c; best = T; } });
        left.splice(left.indexOf(best), 1); order.push(best);
      }
    }
    const ob = P.orderItems, gb = P.groupItems; let outerRows = 1;
    if (tabs.length === 1 && ob.length && !gb.length && !ob.some(x => x.expr)) tabs[0].ordCols = ob.map(x => x.col);
    order.forEach((T, i) => {
      const jm = i ? joinMap(T, order.slice(0, i)) : null; let ap = apOf(T, jm), scanIdx = false;
      if (i === 0 && !T.derived && ap.type === 'ALL' && tabs.length === 1 && ob.length && P.limit && !ob.some(x => x.expr)) {   // 没有可用索引时，用有序索引顺序扫描 + LIMIT 提前结束
        const ix = (T.def.idx || []).find(ix => ob.every((x, k) => ix[1][k] === x.col));
        if (ix) { ap = Object.assign({}, ap, {type: 'index', ix, ue: 0, ur: '', keyLen: ix[1].reduce((a, c) => a + colLen(T.def.cols.find(x => x.n === c)), 0)}); scanIdx = true; }
      }
      const ex = [], flags = [];
      let covering = false;
      if (ap.ix && !T.derived) {
        const rc = refCols(P, T), pk = ((T.def.idx || []).find(x => x[0] === 'PRIMARY') || [0, []])[1];
        covering = !rc.star && rc.set.size > 0 && [...rc.set].every(c => ap.ix[1].includes(c) || pk.includes(c));
      }
      if (ap.where && ap.type !== 'const') ex.push('Using where');
      if (covering && ap.type !== 'const') ex.push('Using index');
      if (ap.type === 'range' && ap.ix[1].length > 1 && !covering) ex.push('Using index condition');
      let filesort = false, temp = false;
      if (i === 0) {
        const served = scanIdx || serves(ob, T, tabs, ap, true);
        if (ob.length && !served) filesort = true;
        if (ob.length && scanIdx && v8 && ob.every(x => x.dir === 'desc')) ex.push('Backward index scan');
        else if (ob.length && served && ap.ix && !scanIdx && v8 && ob.every(x => x.dir === 'desc') && ap.ix[1].length) ex.push('Backward index scan');
        if (gb.length && !serves(gb, T, tabs, ap, false)) { temp = true; if (ob.length && !(ob.length === gb.length && ob.every((x, k) => x.col === gb[k].col))) filesort = true; else if (!v8) filesort = true; }
        else if (gb.length && ob.length && !(ob.length === gb.length && ob.every((x, k) => x.col === gb[k].col)) && !served) filesort = true;
        if (P.distinct && !gb.length) temp = true;
        if (tabs.length > 1 && ob.length && !ob.every(x => locate(tabs, x.alias, x.col) === T)) { temp = true; filesort = true; }
      }
      if (temp) ex.push('Using temporary'); if (filesort) ex.push('Using filesort');
      if (i > 0 && (ap.type === 'ALL' || ap.type === 'index')) ex.push(`Using join buffer (${v8 ? 'hash join' : 'Block Nested Loop'})`);
      ex.forEach(e => { if (/filesort/.test(e)) flags.push('filesort'); if (/temporary/.test(e)) flags.push('temp'); if (/join buffer/.test(e)) flags.push('joinbuf'); });
      if (ap.type === 'ALL' && !flags.includes('joinbuf') && !T.derived) flags.push('all'); if (ap.type === 'index') flags.push('index');
      if (ap.possible.length && !ap.ix) flags.push('nokey');
      if (/DEPENDENT/.test(sel)) flags.push('depsub');
      const rowsN = Math.round(ap.rows), row = newRow({id, select_type: sel, table: T.derived ? `<derived${T.dId}>` : T.alias, type: ap.type, possible: ap.possible, key: ap.ix ? ap.ix[0] : null,
        keyLen: ap.keyLen, ref: ap.ref, rows: rowsN, filtered: ap.filtered, extra: ex, flags, T, ap, covering});
      out.push(row);
      acc.units.push({T, ap, row, driver: i === 0, single: tabs.length === 1, ordCols: i === 0 && tabs.length === 1 && !gb.length && ob.length && !ob.some(x => x.expr) ? ob.map(x => x.col) : [], filesort, P, ctx});
      acc.examined += i === 0 ? ap.rows : (ap.type === 'ALL' || ap.type === 'index') ? ap.rows : outerRows * ap.rows;
      outerRows = Math.max(1, outerRows * ap.rows * ap.filtered / 100);
    });
    info.prod = outerRows;
  }
  tail.forEach(r => out.push(r));
  const aliases = tabs.map(t => t.alias.toLowerCase()).concat(outerAliases);
  P.subs.forEach(sb => planBlock(sb.P, ctx, extra, out, acc, sb.dep ? 'DEPENDENT SUBQUERY' : 'SUBQUERY', aliases));
  return info;
}
function buildPlan(P, ctx, extra) {
  const out = [], acc = {n: 1, units: [], examined: 0}, root = P.kind === 'insert' && P.inner ? P.inner : P;
  if (root.parts) {
    root.parts.forEach((p, i) => planBlock(p, ctx, extra, out, acc, i ? 'UNION' : 'PRIMARY', []));
    if (!root.unionAll) out.push(newRow({id: null, select_type: 'UNION RESULT', table: `<union${root.parts.map((_, i) => i + 1).join(',')}>`, type: 'ALL', extra: ['Using temporary'], flags: ['temp']}));
  } else {
    const nested = root.subs.length || root.refs.some(r => r.derived);
    planBlock(root, ctx, extra, out, acc, root.kind === 'update' ? 'UPDATE' : root.kind === 'delete' ? 'DELETE' : nested ? 'PRIMARY' : 'SIMPLE', []);
  }
  return {rows: out, units: acc.units, examined: acc.examined, grouped: !!(root.groupItems && root.groupItems.length) || !!root.distinct};
}

/* ---------- 执行计划解读（P001–P010）---------- */
function planFindings(plan, sugg) {
  const out = [], sg = t => sugg.find(s => s.table === t);
  plan.rows.forEach(r => {
    if (!r.table || r.id == null) return;
    const rows = r.rows || 0, tn = r.T && r.T.def ? r.T.def.n : r.table, s = sg(tn), tip = s ? `建议新增索引 <code>${esc(s.name)}(${s.cols.join(', ')})</code>（见“索引建议”）。` : '';
    const add = (id, lv, title, detail, fix) => out.push({id, lv, title, detail, fix, table: r.table});
    if (r.flags.includes('all')) add('P001', rows >= 1e5 ? '高' : rows >= 1000 ? '中' : '低', `表 ${r.table} 全表扫描`, `type=ALL，预估扫描 <b>${fi(rows)}</b> 行${r.filtered < 100 ? `，过滤后仅保留约 ${pctS(r.filtered)}%` : ''}。`, tip || '为 WHERE / JOIN 条件列建立合适的索引，或缩小扫描范围。');
    if (r.flags.includes('index')) add('P002', rows >= 1e5 ? '中' : '低', `表 ${r.table} 全索引扫描`, `type=index，按索引顺序读完 <code>${esc(r.key)}</code> 的全部叶子节点（预估 ${fi(rows)} 行）。`, '确认是为了 ORDER BY + LIMIT 提前结束；否则为过滤条件建立索引。');
    if (r.extra.includes('Using filesort')) add('P003', rows >= 1e5 ? '中' : '低', `表 ${r.table} 需要额外排序（Using filesort）`, `结果集需在内存 / 磁盘临时文件中排序，约 ${fi(rows)} 行参与排序。`, tip || '让 ORDER BY 列紧跟等值条件列建立联合索引，利用索引天然有序。');
    if (r.extra.includes('Using temporary')) add('P004', '中', `使用了临时表（Using temporary）`, `GROUP BY / DISTINCT / 跨表排序需要借助临时表，数据量大时会落盘。`, '让分组列命中索引的最左前缀，或缩小参与分组的结果集。');
    if (r.flags.includes('nokey')) add('P005', '中', `表 ${r.table} 有候选索引但没用上`, `possible_keys=<code>${esc(r.possible.join(', '))}</code>，key 为 NULL。常见原因：隐式类型转换、对列使用函数、选择率太低被优化器放弃。`, '检查条件两侧类型是否一致、是否对索引列做了运算。');
    if (r.flags.includes('joinbuf')) add('P006', rows >= 1e5 ? '高' : '中', `被驱动表 ${r.table} 无可用索引（Using join buffer）`, `连接条件列上没有索引，每次连接需扫描整表（${fi(rows)} 行）并借助 join buffer，随结果集放大而急剧变慢。`, tip || '在被驱动表的连接列上建立索引。');
    if (r.flags.includes('depsub')) add('P007', '高', `相关子查询（DEPENDENT SUBQUERY）`, `子查询依赖外层列，外层每一行都会执行一次内层查询。`, '改写为 JOIN，或先聚合再关联。');
    if (rows >= 1e4 && r.filtered != null && r.filtered <= 34 && !r.flags.includes('all') && !r.flags.includes('joinbuf')) add('P009', rows >= 1e6 ? '中' : '低', `表 ${r.table} 扫描后过滤率很低`, `预估扫描 ${fi(rows)} 行，过滤后仅剩约 ${pctS(r.filtered)}%，大部分读取是浪费的。`, tip || '把过滤条件列并入索引，或改写条件让它能用上索引（见“内置建议”）。');
    if (r.flags.includes('impossible')) add('P010', '提示', 'WHERE 条件恒为假（Impossible WHERE）', '优化器已判定不会返回任何行。', '');
  });
  return out;
}

/* ========================================================================
   4. 内置规则（R/S 系列）与索引建议
   ======================================================================== */
function lintBlock(B, out) {
  const add = (id, lv, title, detail, fix) => out.push({id, lv, title, detail, fix: fix || ''});
  if (B.kind === 'insert' && /^(?:insert|replace)\s+(?:into\s+)?[\w.]+\s+(?:values?|select)\b/i.test(B.m)) add('R011', '低', 'INSERT 没有指定列清单', '依赖表的列顺序，加列后语句会报错或写错列。', '写成 INSERT INTO t (c1, c2, …) VALUES (…)。');
  if (!['select', 'update', 'delete'].includes(B.kind)) return;
  const where = B.text('where');
  if (B.kind === 'select' && B.selItems.some(i => /^(?:[a-z_]\w*\.)?\*$/i.test(i))) add('R001', '低', '避免使用 SELECT *', '返回不需要的列会增加网络、内存开销，也让覆盖索引失效；表加列后返回结构会悄悄变化。', '只列出业务需要的列。');
  if ((B.kind === 'update' || B.kind === 'delete') && !where) add('R002', '高', `${B.kind.toUpperCase()} 没有 WHERE 条件`, '会修改 / 删除整张表的数据，且在大表上会长时间持锁。', '补充限定条件；确需全表操作请通过 SQL 上线分批执行并保留回滚语句。');
  const conds = B.conds.concat(...B.refs.map(r => r.onConds));
  conds.forEach(c => {
    if (c.kind !== 'cmp') return;
    if (c.fn || c.arith) {
      let fix = '把运算移到常量一侧，让列保持“裸露”。', m;
      if (c.fn === 'date' && (m = /^'(\d{4}-\d{2}-\d{2})/.exec(c.rhs))) fix = `改写为 <code>${esc(c.col)} &gt;= '${m[1]}' AND ${esc(c.col)} &lt; '${dayOffset(1, m[1])}'</code>。`;
      else if (c.fn === 'date_format' && (m = /^'(\d{4}-\d{2}-\d{2})'/.exec(c.rhs))) fix = `改写为 <code>${esc(c.col)} &gt;= '${m[1]}' AND ${esc(c.col)} &lt; '${dayOffset(1, m[1])}'</code>。`;
      else if (c.fn === 'year' && (m = /^(\d{4})$/.exec(c.rhs))) fix = `改写为 <code>${esc(c.col)} &gt;= '${m[1]}-01-01' AND ${esc(c.col)} &lt; '${+m[1] + 1}-01-01'</code>。`;
      else if (['lower', 'upper'].includes(c.fn)) fix = '使用大小写不敏感的排序规则（默认 _ci），不必再套 LOWER / UPPER；或在 8.0.13+ 建函数索引。';
      add('R003', '中', `对列 ${c.col} ${c.arith ? '做了运算' : '使用了函数 ' + c.fn.toUpperCase() + '()'}`, `<code>${esc(c.text)}</code> 会让 ${esc(c.col)} 上的索引无法使用，退化为全表 / 全索引扫描。`, fix);
    }
    const v = c.rhsK && c.rhsK.k === 'str' ? c.rhsK.v : c.rhs || '';
    if (c.op === 'like' && (/^[%_]/.test(c.rhsK && c.rhsK.k === 'str' ? c.rhsK.v : '') || /^concat\s*\(\s*'%'/i.test(c.rhs))) add('R004', '中', `LIKE 前导通配符：${c.col}`, `<code>${esc(c.text)}</code> 以 % 开头，B+Tree 索引无法定位起点，只能全表扫描。`, '改为前缀匹配（\'abc%\'）；确需模糊搜索请使用 FULLTEXT / ngram 全文索引或搜索引擎；只需后缀匹配可冗余存一个反转列并建索引。');
    if (['!=', '<>', 'not in', 'not like'].includes(c.op)) add('R008', '低', `否定条件：${c.col} ${c.op.toUpperCase()}`, '否定条件通常无法走索引范围扫描，选择率差时会退化为全表扫描。', '尽量改写为正向条件或区间。');
    if ((c.op === '=' || c.op === '!=' || c.op === '<>') && c.rhsK && c.rhsK.k === 'null') add('R014', '中', `与 NULL 比较要用 IS [NOT] NULL：${c.col}`, `<code>${esc(c.text)}</code> 永远不会为真（结果是 NULL）。`, c.op === '=' ? `改为 <code>${esc(c.col)} IS NULL</code>。` : `改为 <code>${esc(c.col)} IS NOT NULL</code>。`);
    if (c.op === 'in' && c.rhsK && c.rhsK.k === 'list' && c.rhsK.items.length >= 200) add('R010', '中', `IN 列表过长（${c.rhsK.items.length} 项）`, '过长的 IN 列表会增加解析与优化开销，超过 range_optimizer_max_mem_size 时放弃走索引。', '分批（每批 ≤ 200）或写入临时表后 JOIN。');
  });
  if (B.limit && /^\d+$/.test(B.limit.off) && +B.limit.off >= 10000) {
    const off = +B.limit.off;
    add('R005', off >= 1e5 ? '高' : '中', `深分页：LIMIT ${B.limit.off}, ${B.limit.cnt}`, `需要先读出并丢弃前 <b>${fi(off)}</b> 行，越往后越慢。`, `${deepPageFix(B)}`);
  }
  if (/\brand\s*\(\s*\)/i.test(B.text('order by'))) add('R006', '高', 'ORDER BY RAND()', '会为全部候选行生成随机数并建临时表排序，数据量大时极慢。', '应用层随机取主键；或先取 id 范围再随机偏移；或预生成随机列并建索引。');
  if (B.hasOr) add('R007', '低', 'WHERE 顶层使用了 OR', '即使两侧列都有索引，也可能退化为 index_merge 或全表扫描。', '改写为 UNION ALL（注意去重），同一列的 OR 合并为 IN。');
  B.subs.forEach(s => {
    if (s.kind === 'in') add('R009', '低', 'IN / NOT IN 子查询', '子查询结果集大或相关时，可能被逐行执行。', '改写为 JOIN 或 EXISTS，并确认关联列有索引。');
    if (s.dep) add('R017', '中', '相关子查询', '子查询引用了外层表的列，外层每一行都要执行一次内层查询（EXPLAIN 为 DEPENDENT SUBQUERY）。', '改写为 JOIN，或先聚合再关联。');
  });
  B.refs.forEach(r => { if (r.jt && r.jt !== 'comma' && !r.on && !r.using) add('R012', '高', `JOIN ${r.name} 没有 ON 条件`, '缺少连接条件会产生笛卡尔积，结果集呈乘法放大。', '补充 ON 连接条件。'); });
  if (B.refs.length > 1 && B.refs.some(r => r.jt === 'comma') && !where) add('R012', '高', '多表逗号连接且没有 WHERE', '没有连接条件，产生笛卡尔积。', '改用 JOIN … ON 并写明连接条件。');
  if (B.kind === 'select' && !B.cte && B.refs.length && !where && !B.limit && !B.agg && !B.groupItems.length) add('R015', '中', 'SELECT 既没有 WHERE 也没有 LIMIT', '会读取整张表并全部返回，大表上耗时且占用连接。', '增加过滤条件或 LIMIT。');
  if (B.kind === 'select' && B.limit && +B.limit.cnt > 1 && !B.orderItems.length && B.refs.length) add('R016', '低', 'LIMIT 没有 ORDER BY', '没有排序时，返回哪几行是不确定的（翻页会重复或漏数据）。', '补充确定性的 ORDER BY（如主键）。');
}
function deepPageFix(B) {
  const r = B.refs[0]; if (!r || B.refs.length !== 1 || r.derived) return '改用游标（上一页最后一条的 id / 排序键作为条件）或“延迟关联”。';
  const ob = B.text('order by'), wh = B.text('where');
  return `改用游标（<code>WHERE … AND 排序键 &lt; 上一页最后一条</code>），或延迟关联：<code>${esc(`SELECT t.* FROM ${r.name} t JOIN (SELECT id FROM ${r.name}${wh ? ' WHERE ' + wh : ''}${ob ? ' ORDER BY ' + ob : ''} LIMIT ${B.limit.off}, ${B.limit.cnt}) x ON x.id = t.id`)}</code>`;
}
function schemaLint(P, ctx, out) {   // 需要表结构的规则：S001 隐式类型转换
  allBlocks(P).forEach(B => {
    if (!['select', 'update', 'delete'].includes(B.kind)) return;
    const tabs = B.refs.map(r => ({alias: r.alias, name: r.name, def: r.derived ? null : tableDef(ctx, r.db, r.name)}));
    B.conds.concat(...B.refs.map(r => r.onConds)).forEach(c => {
      if (c.kind !== 'cmp' || c.fn || !c.rhsK) return;
      const T = locate(tabs, c.alias, c.col); if (!T || !T.def || !isStrCol(T.def, c.col)) return;
      const k = c.rhsK;
      if (!(['=', '<=>', '>', '<', '>=', '<=', 'in'].includes(c.op) && (k.k === 'num' || (k.k === 'list' && k.num)))) return;
      const cd = T.def.cols.find(x => x.n === c.col);
      out.push({id: 'S001', lv: '中', title: `隐式类型转换：${T.def.n}.${c.col}（${cd.t}）与数字比较`, detail: `<code>${esc(c.text)}</code>：字符串列与数字常量比较时，MySQL 会把<b>列</b>转成数字再比，列上的索引失效（预估退化为全表扫描）。`, fix: `把常量写成字符串：<code>${esc(c.col)} ${esc(c.op.toUpperCase())} ${k.k === 'num' ? "'" + esc(k.v) + "'" : '(' + k.items.map(x => "'" + esc(x) + "'").join(', ') + ')'}</code>。`});
    });
  });
}
function suggestIndexes(plan, ctx, P) {
  const out = [];
  plan.units.forEach(u => {
    const T = u.T; if (!T.def || T.derived || T.hasOr) return;
    const def = T.def, ok = c => { const cd = def.cols.find(x => x.n === c); return cd && !/text|blob|json/i.test(cd.t); };
    const eq = [...new Set([...T.eq.keys(), ...T.inL.keys(), ...T.jc.filter(j => !u.driver).map(j => j.col)])].filter(ok).sort((a, b) => rpk(def, a) - rpk(def, b));
    const rngCol = [...T.rng.keys()].filter(ok)[0];
    const cand = eq.concat(rngCol ? [rngCol] : u.ordCols.filter(ok)).filter((c, i, a) => a.indexOf(c) === i).slice(0, 5);
    if (!cand.length) return;
    const trig = u.ap.type === 'ALL' || u.ap.type === 'index' || u.filesort || (u.ap.rows >= 1e4 && u.ap.filtered <= 34);
    if (!trig) return;
    const k = eq.length;
    const covered = (def.idx || []).some(ix => ix[1].length >= cand.length && new Set(ix[1].slice(0, k)).size === k && eq.every(c => ix[1].slice(0, k).includes(c)) && cand.slice(k).every((c, i) => ix[1][k + i] === c));
    if (covered) return;
    const name = ('idx_' + cand.map(c => c.replace(/_(at|id|no)$/, '')).join('_')).slice(0, 48);
    const prefixOf = (def.idx || []).find(ix => ix[0] !== 'PRIMARY' && !ix[2] && ix[1].length < cand.length && ix[1].every((c, i) => cand[i] === c));
    const before = {type: u.ap.type, key: u.ap.ix ? u.ap.ix[0] : null, rows: Math.round(u.ap.rows), filesort: u.filesort};
    const sim = buildPlan(P, ctx, {[def.n]: [[name, cand, false]]}), su = sim.units.find(x => x.T.alias === T.alias);
    const after = su ? {type: su.ap.type, key: su.ap.ix ? su.ap.ix[0] : null, rows: Math.round(su.ap.rows), filesort: su.filesort} : before;
    const reason = [eq.length ? `等值 / IN 条件列 <code>${eq.join(', ')}</code>` : '', rngCol ? `范围条件列 <code>${rngCol}</code>` : (u.ordCols.length ? `ORDER BY 列 <code>${u.ordCols.join(', ')}</code>（避免 filesort）` : '')].filter(Boolean).join(' + ');
    const db = ctx.db, lv = def.rows >= 1e5 ? '高' : def.rows >= 1000 ? '中' : '低';
    out.push({table: def.n, db, name, cols: cand, level: lv, reason, before, after, rows: def.rows, size: def.size, prefixOf: prefixOf ? prefixOf[0] : '',
      ddl: `ALTER TABLE \`${db}\`.\`${def.n}\` ADD INDEX ${name}(${cand.map(c => '`' + c + '`').join(',')});`});
  });
  return out;
}

/* ---------- 校验与总入口 ---------- */
function validateStmt(raw, mode) {   // mode: explain / tuning / soar
  const txt = String(raw || '').trim(); if (!txt) return '请先输入 SQL';
  if (txt.length > 60000) return '语句超过 60KB 上限';
  const body = stripComments(txt).trim().replace(/[\s;]+$/, ''); if (!body) return '语句为空（只有注释）';
  if (maskStr(body).includes(';')) return '只能分析单条语句：检测到多条语句，请拆开后分别分析';
  let kind = (/^[\s(]*([a-z]+)/i.exec(body) || [0, ''])[1].toLowerCase();
  if (kind === 'explain') return validateStmt(body.replace(/^\s*explain\s+(?:(?:format\s*=\s*\w+|extended|partitions)\s+)?/i, ''), mode);
  if (/^\s*explain\s+analyze/i.test(txt)) return '拒绝：EXPLAIN ANALYZE 会真实执行语句';
  if (/\binto\s+(outfile|dumpfile)\b/i.test(body)) return '拒绝：不允许 INTO OUTFILE / DUMPFILE';
  const bad = /\b(sleep|benchmark|load_file|get_lock)\s*\(/i.exec(body); if (bad) return `拒绝：语句包含 ${bad[1].toUpperCase()}()，可能造成阻塞或副作用`;
  if (mode === 'tuning' && !['select', 'with'].includes(kind)) return 'SQLTuning 只接受单条 SELECT（PROFILE 会真实执行该语句）';
  if (mode === 'soar' && !['select', 'insert', 'update', 'delete', 'replace', 'alter'].includes(kind)) return 'SOAR 需要以 SELECT / INSERT / UPDATE / DELETE 等语句关键字开头';
  if (mode === 'explain') {
    if (!['select', 'with', 'insert', 'replace', 'update', 'delete'].includes(kind)) return '仅支持 SELECT / UPDATE / DELETE / INSERT…SELECT 的执行计划';
    if (/^(insert|replace)\b/i.test(body) && !/\bselect\b/i.test(body)) return '仅支持 INSERT … SELECT（不含 SELECT 的 INSERT 没有执行计划）';
  }
  return '';
}
function analyze(sqlText, instId, db) {
  const A = {err: '', notes: [], P: null, ctx: mkCtx(instId, db), plan: null, planErr: '', lint: [], plint: [], sugg: [], score: 100, missing: []};
  const verr = validateStmt(sqlText, 'explain');
  if (verr && /^(只能分析单条|拒绝|请先输入|语句为空|语句超过)/.test(verr)) { A.err = verr; return A; }
  const txt = String(sqlText).replace(/^\s*explain\s+(?:(?:format\s*=\s*\w+|extended|partitions)\s+)?/i, '');
  try {
    const P = parseStmt(txt); A.P = P;
    const blocks = allBlocks(P);
    const badDb = blocks.flatMap(b => b.refs).find(r => r.db && !A.ctx.dbs.includes(r.db));
    if (badDb) { A.err = `库 ${badDb.db} 不在当前实例的可见范围内（语句里带库名的表引用必须落在可见库内）`; return A; }
    blocks.forEach(b => b.refs.forEach(r => { if (!r.derived && !tableDef(A.ctx, r.db, r.name)) A.missing.push((r.db || db) + '.' + r.name); }));
    if (P.parts && !P.unionAll) A.lint.push({id: 'R013', lv: '低', title: 'UNION 未指定 ALL', detail: 'UNION 会对结果去重（需要临时表排序）。', fix: '确认不需要去重时使用 UNION ALL。'});
    blocks.forEach(b => lintBlock(b, A.lint));
    const seen = new Set(); A.lint = A.lint.filter(f => { const k = f.id + f.title; if (seen.has(k)) return false; seen.add(k); return true; });
    if (A.missing.length) { A.planErr = `ERROR 1146 (42S02): Table '${A.missing[0]}' doesn't exist`; A.notes.push(`表 ${[...new Set(A.missing)].join('、')} 在所选实例 / 库中不存在：仅做了静态规则分析（不含表结构相关的规则、执行计划与索引建议）。请核对实例与库。`); }
    else if (verr) { A.planErr = verr; }
    else if (P.cte) { A.planErr = '暂不解析 WITH（CTE）语句的执行计划，仅做静态规则分析'; }
    else if (P.kind === 'insert' && !P.inner) { A.planErr = '没有可分析的 SELECT'; }
    else {
      schemaLint(P, A.ctx, A.lint);
      A.plan = buildPlan(P, A.ctx, {});
      A.sugg = suggestIndexes(A.plan, A.ctx, P.inner || P);
      A.sugg.forEach(s => A.lint.push({id: 'S002', lv: s.level, title: `建议为 ${s.table} 新增索引 ${s.name}(${s.cols.join(', ')})`, detail: `${s.reason}；预估 ${s.before.type}/${fi(s.before.rows)} 行 → ${s.after.type}/${fi(s.after.rows)} 行${s.before.filesort && !s.after.filesort ? '，并消除 filesort' : ''}。`, fix: `<code>${esc(s.ddl)}</code>`}));
      A.plint = planFindings(A.plan, A.sugg);
    }
  } catch (e) { A.err = '解析失败：' + e.message; }
  A.score = scoreOf(A.lint.concat(A.plint));
  return A;
}

/* ---------- 执行计划 JSON（EXPLAIN FORMAT=JSON 风格）---------- */
function planJson(plan) {
  const cost = r => { const R = r.T && r.T.def ? r.T.def.rows : 1; return {read: r.type === 'ALL' ? R * .11 + 1 : r.rows * .25 + .35, eval: r.rows * (r.filtered || 100) / 100 * .1}; };
  const tableObj = r => {
    if (!r.table) return {message: r.extra.join('; ')};
    const c = cost(r), o = {table_name: r.table, access_type: r.type};
    if (r.possible.length) o.possible_keys = r.possible;
    if (r.key) { o.key = r.key; o.used_key_parts = r.key ? (r.T.def.idx.find(x => x[0] === r.key) || [0, r.ap && r.ap.ix ? r.ap.ix[1] : []])[1].slice(0, (r.ap.ue || 0) + (r.ap.ur ? 1 : 0)) : []; o.key_length = String(r.keyLen); }
    if (r.ref) o.ref = r.ref.split(',');
    o.rows_examined_per_scan = r.rows; o.rows_produced_per_join = Math.round(r.rows * (r.filtered || 100) / 100); o.filtered = pctS(r.filtered || 100);
    if (r.extra.includes('Using index')) o.using_index = true;
    if (r.extra.includes('Using index condition')) o.index_condition = '(…)';
    if (r.extra.some(e => /join buffer/.test(e))) { o.using_join_buffer = /hash/.test(r.extra.join()) ? 'hash join' : 'Block Nested Loop'; }
    o.cost_info = {read_cost: c.read.toFixed(2), eval_cost: c.eval.toFixed(2), prefix_cost: (c.read + c.eval).toFixed(2), data_read_per_join: fmtBytes(r.rows * (r.T && r.T.def ? r.T.def.size * 1048576 / Math.max(1, r.T.def.rows) : 100))};
    if (r.T && r.T.def && !r.extra.includes('Using index')) o.used_columns = r.T.def.cols.map(x => x.n).slice(0, 8);
    if (r.ap && r.ap.where) { const cs = r.T.all.filter(c => c.text && !(r.ap.used.has(c.col) && ['eq', 'in', 'range'].includes(c.cls))).map(c => c.text.replace(/\s+/g, ' ')); o.attached_condition = '(' + (cs.join(' and ') || '…') + ')'; }
    if (r.T && r.T.derived) { const inner = blockJson(r.T.dId); o.materialized_from_subquery = {using_temporary_table: true, dependent: false, cacheable: true, query_block: inner}; }
    return o;
  };
  const blockJson = id => {
    const rows = plan.rows.filter(r => r.id === id && r.select_type !== 'UNION RESULT'); if (!rows.length) return {};
    let body = rows.length === 1 ? {table: tableObj(rows[0])} : {nested_loop: rows.map(r => ({table: tableObj(r)}))};
    const f = rows[0].extra, total = rows.reduce((a, r) => a + parseFloat(cost(r).read + cost(r).eval), 0);
    if (f.includes('Using temporary') && plan.grouped) body = {grouping_operation: Object.assign({using_temporary_table: true, using_filesort: f.includes('Using filesort')}, body)};
    else if (f.includes('Using filesort') || f.includes('Using temporary')) body = {ordering_operation: Object.assign(f.includes('Using temporary') ? {using_temporary_table: true, using_filesort: true} : {using_filesort: true}, body)};
    const subs = plan.rows.filter(r => r.id > id && /SUBQUERY/.test(r.select_type) && r.id === Math.min(...plan.rows.filter(x => /SUBQUERY/.test(x.select_type)).map(x => x.id)));
    const res = Object.assign({select_id: id, cost_info: {query_cost: total.toFixed(2)}}, body);
    return res;
  };
  const subIds = [...new Set(plan.rows.filter(r => /SUBQUERY/.test(r.select_type)).map(r => r.id))];
  const unions = plan.rows.filter(r => r.select_type === 'UNION' || (r.select_type === 'PRIMARY' && plan.rows.some(x => x.select_type === 'UNION')));
  if (plan.rows.some(r => r.select_type === 'UNION RESULT')) {
    const ids = [...new Set(unions.map(r => r.id))];
    return {query_block: {union_result: {using_temporary_table: true, table_name: plan.rows.find(r => r.select_type === 'UNION RESULT').table, access_type: 'ALL', query_specifications: ids.map(i => ({dependent: false, cacheable: true, query_block: blockJson(i)}))}}};
  }
  const main = blockJson(1);
  if (subIds.length) main.attached_subqueries = subIds.map(i => ({dependent: plan.rows.some(r => r.id === i && /DEPENDENT/.test(r.select_type)), cacheable: !plan.rows.some(r => r.id === i && /DEPENDENT/.test(r.select_type)), query_block: blockJson(i)}));
  return {query_block: main};
}
const fmtBytes = b => b >= 1073741824 ? (b / 1073741824).toFixed(1) + 'G' : b >= 1048576 ? (b / 1048576).toFixed(1) + 'M' : b >= 1024 ? (b / 1024).toFixed(1) + 'K' : Math.round(b) + '';

/* ========================================================================
   5. SQL 美化
   ======================================================================== */
const PHRASES = ['on duplicate key update', 'left outer join', 'right outer join', 'group by', 'order by', 'union all', 'left join', 'right join', 'inner join', 'cross join', 'natural join', 'insert into', 'delete from', 'for update'];
const MAJOR = new Set(['select', 'from', 'where', 'group by', 'order by', 'having', 'limit', 'union', 'union all', 'set', 'values', 'insert into', 'delete from', 'update', 'on duplicate key update', 'for update']);
const JOINS = new Set(['join', 'left join', 'right join', 'inner join', 'cross join', 'natural join', 'left outer join', 'right outer join', 'straight_join']);
const FN_UP = new Set('count sum avg max min date now ifnull coalesce concat date_format from_unixtime unix_timestamp substr substring if cast convert year month day lower upper trim length round floor ceil group_concat rand curdate date_sub date_add datediff timestampdiff abs replace left right in'.split(' '));
const NO_FN = new Set('in and or on values not exists from join as when then else select where by having like between limit using union all any some set into'.split(' '));
function tokenize(s) {
  const re = /(\/\*[\s\S]*?\*\/|--[^\n]*|#[^\n]*)|('(?:[^'\\]|\\[\s\S]|'')*'|"(?:[^"\\]|\\[\s\S])*"|`[^`]*`)|(@@?[\w.]+|[A-Za-z_À-￿][\w$À-￿]*|\d+(?:\.\d+)?|\?)|(<=>|<=|>=|<>|!=|:=|\|\||&&|<<|>>|[-+*\/%=<>!(),.;~^&|])|(\s+)|([\s\S])/g;
  const out = []; let x;
  while ((x = re.exec(s))) {
    if (x[5]) continue;
    if (x[1]) out.push({t: 'c', v: x[1]}); else if (x[2]) out.push({t: 's', v: x[2]}); else if (x[3]) out.push({t: /^\d/.test(x[3]) ? 'n' : 'w', v: x[3]}); else out.push({t: 'y', v: x[4] || x[6]});
  }
  const merged = [];
  for (let i = 0; i < out.length; i++) {
    let done = false;
    if (out[i].t === 'w') for (const p of PHRASES) {
      const ws = p.split(' ');
      if (ws.every((w, k) => out[i + k] && out[i + k].t === 'w' && out[i + k].v.toLowerCase() === w)) { merged.push({t: 'w', v: p}); i += ws.length - 1; done = true; break; }
    }
    if (!done) merged.push(out[i]);
  }
  return merged;
}
function formatSql(src) {
  const T = tokenize(src), lines = []; let cur = '', curInd = 0, lvl = 0, clause = '', between = false; const stack = [];
  const ind = n => '    '.repeat(n);
  const start = n => { if (cur.trim()) lines.push(ind(curInd) + cur.trim()); cur = ''; curInd = n; };
  const commas = i => { let d = 0, n = 0; for (let k = i + 1; k < T.length; k++) { const t = T[k]; if (t.v === '(') d++; else if (t.v === ')') { if (!d) break; d--; } else if (!d && t.t === 'w' && MAJOR.has(t.v.toLowerCase())) break; else if (!d && t.v === ',') n++; } return n; };
  const kwText = v => v.toUpperCase();
  let prev = null;
  const put = (txt, t) => {
    const noSp = !cur || /[(.@]$/.test(cur) || txt === ')' || txt === ',' || txt === '.' || txt === ';' || (txt === '(' && prev && prev.t === 'w' && !NO_FN.has(prev.v.toLowerCase()) && /^[a-z_]/i.test(prev.v) && !(clause === 'insert into' && !stack.length)) || (prev && prev.v === '.' ) ;
    cur += (noSp ? '' : ' ') + txt;
  };
  for (let i = 0; i < T.length; i++) {
    const t = T[i], low = t.t === 'w' ? t.v.toLowerCase() : '', plain = stack.length && !stack[stack.length - 1].sub;
    if (t.t === 'c') { put(t.v, t); if (/^(--|#)/.test(t.v)) start(curInd); prev = t; continue; }
    if (t.t === 'w' && !plain && MAJOR.has(low) && !(low === 'values' && clause === 'on duplicate key update')) {
      start(lvl); clause = low; cur = kwText(low);
      if ((low === 'select' || low === 'set') && commas(i) > 0) { if (low === 'select' && T[i + 1] && T[i + 1].t === 'w' && T[i + 1].v.toLowerCase() === 'distinct') { cur += ' DISTINCT'; i++; } start(lvl + 1); }
      prev = t; continue;
    }
    if (t.t === 'w' && !plain && JOINS.has(low)) { start(lvl + 1); clause = 'from'; cur = kwText(low); prev = t; continue; }
    if (t.t === 'w' && low === 'on' && !plain) { clause = 'on'; }
    if (t.t === 'w' && low === 'between') between = true;
    if (t.t === 'w' && (low === 'and' || low === 'or') && !plain && ['where', 'on', 'having'].includes(clause)) {
      if (low === 'and' && between) { between = false; put('AND', t); prev = t; continue; }
      start(lvl + (clause === 'on' ? 2 : 1)); cur = kwText(low); prev = t; continue;
    }
    if (t.v === ',' && !plain) { if (['select', 'set', 'from'].includes(clause)) { cur += ','; start(lvl + 1); } else cur += ','; prev = t; continue; }
    if (t.v === '(') {
      const nx = T[i + 1], sub = nx && nx.t === 'w' && nx.v.toLowerCase() === 'select';
      put('(', t); stack.push({sub, clause});
      if (sub) { start(lvl + 1); lvl++; clause = ''; }
      prev = t; continue;
    }
    if (t.v === ')') {
      const top = stack.pop();
      if (top && top.sub) { lvl--; start(lvl); cur = ')'; clause = top.clause; } else cur += ')';
      prev = t; continue;
    }
    if (t.v === ';') { cur += ';'; start(0); lvl = 0; clause = ''; prev = t; continue; }
    let txt = t.v;
    if (t.t === 'w' && SQL_KW.has(low) && !(prev && prev.v === '.') && !(T[i + 1] && T[i + 1].v === '.')) txt = kwText(txt);
    else if (t.t === 'w' && FN_UP.has(low) && T[i + 1] && T[i + 1].v === '(') txt = kwText(txt);
    put(txt, t); prev = t;
  }
  start(0);
  return lines.join('\n');
}

/* ========================================================================
   6. 优化工具页（pages.sqladvisor）
   ======================================================================== */
const SAMPLES = [
  {k: 'page', name: '慢分页（深翻页 + filesort）', schema: 'order', db: 'order_db', sql: 'SELECT id, order_no, amount, status, created_at\nFROM orders\nWHERE status = 1\nORDER BY created_at DESC\nLIMIT 500000, 20;'},
  {k: 'conv', name: '隐式类型转换（字符串列 = 数字）', schema: 'order', db: 'order_db', sql: 'SELECT id, order_no, amount, status\nFROM orders\nWHERE order_no = 20261007001234;'},
  {k: 'star', name: 'SELECT * + 函数作用于索引列', schema: 'order', db: 'order_db', sql: "SELECT *\nFROM orders\nWHERE DATE(created_at) = '2026-10-05'\n  AND status = 1;"},
  {k: 'like', name: "LIKE '%x' 前导通配符", schema: 'order', db: 'order_db', sql: "SELECT id, order_id, sku_name, qty\nFROM order_items\nWHERE sku_name LIKE '%手机壳'\nORDER BY id DESC\nLIMIT 50;"},
  {k: 'join', name: '缺索引的 JOIN', schema: 'order', db: 'order_db', sql: "SELECT o.order_no, o.amount, a.name, a.detail\nFROM orders o\nJOIN user_addr a ON a.phone = o.receiver_phone\nWHERE o.status = 3\n  AND o.created_at >= '2026-10-01';"},
];
/* 外部工具就绪情况（与 dbt_sql_config 对应）：SOAR 未配置，用来演示“提示卡片而不报错” */
const TOOLS = {
  tuning: {name: 'SQLTuning', ready: true, path: '内置（移植自 sql_tuning.py）', code: 'optimize.tuning'},
  soar: {name: 'SOAR', ready: false, path: '', code: 'optimize.soar', why: 'dbt_sql_config 中未配置 <code>soar</code>（程序路径）与 <code>soar_test_dsn</code>（测试环境 DSN）'},
};
const ADV = {inst: 0, db: '', sql: SAMPLES[0].sql, sample: 'page', tab: 'plan', planFmt: 'table', res: null, out: {}, seq: {}, tune: {params: false, plan: true, stats: true, profile: true}, autorun: false, stale: false};
const advInsts = () => visibleInstances().filter(i => i.type === 'MySQL' && !isReadView(i));
function ensureAdv() {
  const list = advInsts();
  if (!list.some(i => i.id === ADV.inst)) { ADV.inst = list.length ? list[0].id : 0; ADV.db = ''; }
  const dbs = ADV.inst ? dbsOf(ADV.inst).map(d => d.db) : [];
  if (!dbs.includes(ADV.db)) ADV.db = dbs[0] || '';
}
function snap() {
  const r = ADV.res;
  if (r && r.sql === ADV.sql && r.inst === ADV.inst && r.db === ADV.db) return r;
  return (ADV.res = {sql: ADV.sql, inst: ADV.inst, db: ADV.db, A: analyze(ADV.sql, ADV.inst, ADV.db), ran: {}});
}
const instLabel = () => { const i = instOf(ADV.inst); return `${i.name} / ${ADV.db}`; };

pages.sqladvisor = {
  title: '优化工具',
  render() {
    // 从“慢查日志”跳转：沿用原系统的 sessionStorage 键，读取后清除并自动运行
    try {
      const s = sessionStorage.getItem('advisor_sql_content');
      if (s) {
        const dbn = sessionStorage.getItem('advisor_db_name') || '', inm = sessionStorage.getItem('advisor_instance_name') || '';
        ['advisor_sql_content', 'advisor_db_name', 'advisor_instance_name'].forEach(k => sessionStorage.removeItem(k));
        const it = advInsts().find(i => i.name === inm); if (it) { ADV.inst = it.id; ADV.db = dbn; }
        ADV.sql = s; ADV.sample = ''; ADV.res = null; ADV.out = {}; ADV.autorun = true;
      }
    } catch (e) {}
    ensureAdv();
    const list = advInsts(), dbs = ADV.inst ? dbsOf(ADV.inst).map(d => d.db) : [], n = A => A ? A.lint.length + A.plint.length : 0;
    const st = Object.values(TOOLS).map(t => `<span class="tag ${t.ready ? 'ok' : 'warn'}" title="${esc(t.path || '')}">${t.name} ${t.ready ? '就绪' : '未配置'}</span>`).join(' ');
    return `${CSS}
    <div class="grid g-main" style="align-items:stretch">
      <div class="card" style="display:flex;flex-direction:column"><h2>SQL<span class="right"><select data-onchange="advSample" aria-label="示例 SQL"><option value="">示例 SQL…</option>${SAMPLES.map(s => `<option value="${s.k}" ${ADV.sample === s.k ? 'selected' : ''}>${s.name}</option>`).join('')}</select></span></h2>
        <textarea class="sql-ed" id="advSql" style="flex:1 1 auto" data-oninput="advSql" spellcheck="false" placeholder="粘贴一条 SQL">${esc(ADV.sql)}</textarea>
        <div class="toolbar" style="margin:10px 0 0">
          ${B('optimize.explain', '执行计划', {cls: 'primary', data: {m: 'plan'}})}${B('optimize.explain', '内置建议', {data: {m: 'rules'}})}${B('optimize.explain', 'SQL 美化', {data: {m: 'fmt'}})}
          <span class="muted">｜</span>
          ${B('optimize.tuning', 'SQLTuning')}${B('optimize.soar', 'SOAR')}
          <span class="sp"></span><button class="btn" data-on="advClear">清空</button></div></div>
      <div class="card"><h2>选项</h2>
        <div class="form" style="grid-template-columns:78px minmax(0,1fr)">
          <label>实例</label><select data-onchange="advInst">${list.length ? instOpts(list, ADV.inst) : '<option>（无可用实例）</option>'}</select>
          <label>数据库</label><select data-onchange="advDb">${dbs.map(d => `<option ${d === ADV.db ? 'selected' : ''}>${esc(d)}</option>`).join('')}</select>
          <label>SQLTuning</label><div style="display:flex;flex-direction:column;gap:3px;font-size:13px">
            <label class="chk"><input type="checkbox" data-onchange="advOpt" data-k="params" ${ADV.tune.params ? 'checked' : ''}> 显示系统参数</label>
            <label class="chk"><input type="checkbox" data-onchange="advOpt" data-k="plan" ${ADV.tune.plan ? 'checked' : ''}> 显示执行计划</label>
            <label class="chk"><input type="checkbox" data-onchange="advOpt" data-k="stats" ${ADV.tune.stats ? 'checked' : ''}> 显示相关对象统计信息</label>
            <label class="chk"><input type="checkbox" data-onchange="advOpt" data-k="profile" ${ADV.tune.profile ? 'checked' : ''}> 显示 PROFILE 跟踪（会真实执行）</label></div></div>
        <div class="small muted" style="margin-top:10px">外部工具：${st}</div></div>
    </div>
    <div class="card" style="margin-top:16px"><div id="advRes">${advResult()}</div></div>`;
  },
  init() {
    if (ADV.autorun) {
      ADV.autorun = false;
      if (can('optimize.explain')) ACTIONS['optimize.explain']({m: 'rules'});
    }
  }
};
const refreshRes = () => { const el = $('#advRes'); if (el) el.innerHTML = advResult(); };
ON.advSql = (d, el) => { ADV.sql = el.value; ADV.sample = ''; const st = !!(ADV.res && ADV.res.sql !== ADV.sql); if (st !== ADV.stale) { ADV.stale = st; refreshRes(); } };
ON.advSample = (d, el) => {
  const s = SAMPLES.find(x => x.k === el.value); if (!s) return;
  ADV.sql = s.sql; ADV.sample = s.k; ADV.res = null; ADV.out = {};
  const it = advInsts().find(i => instOf(i.id).schema === s.schema && dbsOf(i.id).some(x => x.db === s.db));
  if (it) { ADV.inst = it.id; ADV.db = s.db; } else toast(`当前身份没有可用的「${s.db}」实例，将按所选实例分析（表不存在时只做静态规则）`);
  rerender();
};
ON.advInst = (d, el) => { ADV.inst = +el.value; ADV.db = ''; rerender(); };
ON.advDb = (d, el) => { ADV.db = el.value; };
ON.advOpt = (d, el) => { ADV.tune[d.k] = el.checked; };
ON.advClear = () => { ADV.sql = ''; ADV.sample = ''; ADV.res = null; ADV.out = {}; rerender(); };
ON.advTab = d => { ADV.tab = d.v; refreshRes(); };
ON.advFmt = d => { ADV.planFmt = d.v; refreshRes(); };
ON.advApplyFmt = () => { const s = snap(); ADV.sql = formatSql(ADV.sql); ADV.res = null; rerender(); toast('已替换到编辑器'); };
ON.advGoRules = () => { ADV.tab = 'rules'; if (can('optimize.explain')) ACTIONS['optimize.explain']({m: 'rules'}); };

ACTIONS['optimize.explain'] = d => {
  const m = d.m || 'plan';
  if (!ADV.sql.trim()) return toast('请先输入 SQL');
  if (!ADV.inst) return toast('没有可用的实例');
  const s = snap();
  if (m === 'fmt') { s.ran.fmt = true; ADV.tab = 'fmt'; audit('optimize.explain', 'SQL优化-格式化', '成功', instLabel()); }
  else if (m === 'rules') { s.ran.rules = s.ran.index = true; ADV.tab = 'rules'; audit('optimize.explain', 'SQL优化-内置建议：' + (s.A.err || `命中 ${s.A.lint.length + s.A.plint.length} 条`), s.A.err ? '失败' : '成功', instLabel()); }
  else { s.ran.plan = true; ADV.tab = 'plan'; audit('optimize.explain', 'SQL优化-执行计划：' + (s.A.err || s.A.planErr || `${s.A.plan.rows.length} 行`), s.A.err || s.A.planErr ? '失败' : '成功', instLabel()); }
  refreshRes();
};
async function runTool(kind) {
  if (!ADV.sql.trim()) return toast('请先输入 SQL');
  if (!ADV.inst) return toast('没有可用的实例');
  const T = TOOLS[kind], s = snap(), sql = ADV.sql; ADV.tab = kind;
  const err = validateStmt(sql, kind);
  if (err) { ADV.out[kind] = {state: 'err', sql, msg: err}; audit(T.code, `SQL优化-${T.name}：校验未通过`, '失败', instLabel()); return refreshRes(); }
  if (!T.ready) { ADV.out[kind] = {state: 'unconf', sql}; audit(T.code, `SQL优化-${T.name}：未配置`, '失败', instLabel()); return refreshRes(); }
  const tk = ADV.seq[kind] = (ADV.seq[kind] || 0) + 1;
  ADV.out[kind] = {state: 'run', sql}; refreshRes();
  await sleep(kind === 'tuning' ? 1200 : 800);
  if (ADV.seq[kind] !== tk) return;
  ADV.out[kind] = {state: 'done', sql, A: s.A, at: now()};
  audit(T.code, `SQL优化-${T.name}`, '成功', instLabel() + ' / ' + sql.replace(/\s+/g, ' ').slice(0, 120));
  refreshRes();
}
ACTIONS['optimize.tuning'] = () => runTool('tuning');
ACTIONS['optimize.soar'] = () => runTool('soar');

/* ---------- 结果区 ---------- */
const RTABS = [['plan', '执行计划'], ['rules', '内置建议'], ['index', '索引建议'], ['tuning', 'SQLTuning'], ['soar', 'SOAR'], ['fmt', 'SQL 美化']];
function advResult() {
  const s = ADV.res, A = s && s.A, cnt = A ? A.lint.length + A.plint.length : 0; ADV.stale = !!(s && s.sql !== ADV.sql);
  const bar = `<div class="tabs">${RTABS.map(([id, t]) => {
    const badge = id === 'rules' && s && s.ran.rules ? ` <span class="tag ${cnt ? 'warn' : 'ok'}">${cnt}</span>` : id === 'index' && s && s.ran.index ? ` <span class="tag ${A.sugg.length ? 'warn' : 'ok'}">${A.sugg.length}</span>` : ADV.out[id] ? ` <span class="tag ${ADV.out[id].state === 'done' ? 'ok' : ADV.out[id].state === 'run' ? 'info' : 'warn'}">${{done: '✓', run: '…', err: '!', unconf: '未配置'}[ADV.out[id].state]}</span>` : '';
    return `<a data-on="advTab" data-v="${id}" class="${ADV.tab === id ? 'on' : ''}">${t}${badge}</a>`;
  }).join('')}</div>`;
  const stale = s && s.sql !== ADV.sql ? notice('SQL 已修改，下方是上一次运行的结果。', 'warn', '⚠') : '';
  const body = {plan: tabPlan, rules: tabRules, index: tabIndex, tuning: tabTool, soar: tabTool, fmt: tabFmt}[ADV.tab](ADV.tab);
  return bar + stale + body;
}
const hint = (btn, what) => empty(`点击上方「${btn}」查看${what}`, '▷');
function errCard(A) { return notice(`<b>校验未通过：</b>${esc(A.err)}`, 'bad', '✕'); }

function tabPlan() {
  const s = ADV.res; if (!s || !s.ran.plan) return hint('执行计划', '执行计划');
  const A = s.A; if (A.err) return errCard(A);
  if (A.planErr) return notice(`<b>${esc(A.planErr)}</b>`, 'bad', '✕');
  const pl = A.plan, bad = pl.rows.filter(r => r.flags.includes('all') || r.flags.includes('joinbuf') || r.flags.includes('depsub')), mx = Math.max(0, ...pl.rows.map(r => r.rows || 0));
  const head = `<div class="row between" style="margin-bottom:10px"><div class="stat"><div><b>${pl.rows.filter(r => r.table).length}</b><span>访问的表</span></div><div><b>${fi(pl.examined)}</b><span>预估扫描行数</span></div><div><b style="color:var(--${bad.length ? 'bad' : 'ok'})">${bad.length}</b><span>全表扫描 / 无索引连接</span></div></div>
    <div>${subtabs([['table', '表格'], ['json', 'JSON']], ADV.planFmt, 'advFmt')}</div></div>`;
  let view;
  if (ADV.planFmt === 'json') {
    const txt = JSON.stringify(planJson(pl), null, 2), hot = /"access_type": "ALL"|"using_filesort": true|"using_temporary_table": true|"using_join_buffer"|"access_type": "index"/;
    view = `<pre class="code"><button class="cp" data-copy="${esc(txt)}">复制</button>${txt.split('\n').map(l => hot.test(l) ? `<span class="hl">${esc(l)}</span>` : esc(l)).join('\n')}</pre>`;
  } else {
    const ex = r => r.extra.length ? r.extra.map(e => /filesort|temporary|join buffer/.test(e) ? `<span class="tag warn">${esc(e)}</span>` : esc(e)).join('; ') : '<span class="muted">—</span>';
    view = tbl(['id', 'select_type', 'table', 'type', 'possible_keys', 'key', 'key_len', 'ref', 'rows', 'filtered', 'Extra'], pl.rows, {
      attrs: r => r.flags.includes('all') && r.rows >= 1e5 || r.flags.includes('joinbuf') || r.flags.includes('depsub') ? ' class="op-bad"' : r.flags.some(f => ['all', 'filesort', 'temp', 'index', 'nokey'].includes(f)) ? ' class="op-warn"' : '',
      cells: r => [r.id == null ? '' : r.id, `<span class="small">${esc(r.select_type)}</span>`, r.table ? `<b>${esc(r.table)}</b>` : '<span class="muted">—</span>',
        r.type ? `<span class="tag ${r.type === 'ALL' ? 'bad' : r.type === 'index' ? 'warn' : 'ok'} mono">${r.type}</span>` : '<span class="muted">NULL</span>', `<span class="mono small">${r.possible.length ? esc(r.possible.join(',')) : 'NULL'}</span>`,
        `<span class="mono small">${r.key ? esc(r.key) : '<span class="tag bad">NULL</span>'}</span>`, `<span class="mono small">${r.keyLen == null ? 'NULL' : r.keyLen}</span>`, `<span class="mono small">${r.ref ? esc(r.ref) : 'NULL'}</span>`,
        r.rows == null ? '' : `<span class="op-num">${fi(r.rows)}</span>`, r.filtered == null ? '' : `<span class="op-num">${pctS(r.filtered)}</span>`, `<span class="small">${ex(r)}</span>`]});
  }
  const finds = A.plint.length ? `<h2 style="margin:16px 0 8px;font-size:14px">执行计划解读</h2>${findingsHtml(A.plint)}` : notice('执行计划没有发现明显问题：均走索引，无 filesort / 临时表。', 'ok', '✓');
  return head + view + finds;
}
function tabRules() {
  const s = ADV.res; if (!s || !s.ran.rules) return hint('内置建议', '规则命中');
  const A = s.A; if (A.err) return errCard(A);
  const all = A.lint.concat(A.plint).sort((a, b) => ['高', '中', '低', '提示'].indexOf(a.lv) - ['高', '中', '低', '提示'].indexOf(b.lv));
  const cnt = lv => all.filter(f => f.lv === lv).length, P = A.P;
  return `<div class="score" style="margin-bottom:12px">${ring(A.score, '评分')}<div><div class="b" style="font-size:15px">命中 ${all.length} 条建议</div>
      <div class="small muted">高 ${cnt('高')} · 中 ${cnt('中')} · 低 ${cnt('低')} · 提示 ${cnt('提示')}</div>
      <div class="small muted" style="margin-top:4px">语句类型 <b>${esc(P.parts ? 'UNION' : P.kind.toUpperCase())}</b> · 指纹 <span class="mono">${esc(fingerprint(s.sql).slice(0, 110))}${fingerprint(s.sql).length > 110 ? '…' : ''}</span></div></div></div>
    ${A.notes.map(n => notice(esc(n), 'warn', '⚠')).join('')}
    ${all.length ? findingsHtml(all) : notice('没有命中任何规则。', 'ok', '✓')}`;
}
function tabIndex() {
  const s = ADV.res; if (!s || !s.ran.index) return hint('内置建议', '索引建议');
  const A = s.A; if (A.err) return errCard(A);
  if (!A.plan) return notice(esc(A.planErr || '无法推导索引') + '：没有表结构就无法给出索引建议。', 'warn', '⚠');
  const defs = [...new Map(A.plan.units.filter(u => u.T.def).map(u => [u.T.def.n, u.T.def])).values()];
  const sg = A.sugg.map(x => `<div class="op-find ${LV_CLS[x.level]}"><div class="t">${lvTag(x.level)}<span class="tag">S002</span>${esc(x.db)}.${esc(x.table)} 新增索引 <span class="mono">${esc(x.name)}</span></div>
      <div class="d">${x.reason}。</div>
      <div class="d small">执行计划预估：<span class="tag ${x.before.type === 'ALL' ? 'bad' : 'warn'}">${x.before.type}</span> ${fi(x.before.rows)} 行${x.before.filesort ? ' · filesort' : ''} → <span class="tag ok">${x.after.type}</span> ${fi(x.after.rows)} 行${x.after.filesort ? ' · filesort' : ''}${x.before.filesort && !x.after.filesort ? '（消除排序）' : ''}</div>
      ${tickCode(x.ddl, 'sql')}
      <div class="f">${x.prefixOf ? `新索引以现有 <code>${esc(x.prefixOf)}</code> 为前缀，上线后可评估删除 ${esc(x.prefixOf)}。` : ''}表 ${esc(x.table)} 约 ${fmtN(x.rows)} 行 / ${fmtMB(x.size)}，建议低峰通过 <a href="#sqlworkflow/new">SQL 上线</a>执行。</div></div>`).join('');
  const idx = tbl(['表', '索引', '列', '唯一', '估算基数'], defs.flatMap(d => (d.idx || []).map(ix => [`<span class="mono">${esc(d.n)}</span>`, `<span class="mono">${esc(ix[0])}</span>`, `<span class="mono small">${ix[1].map(esc).join(', ')}</span>`, ix[2] ? '是' : '', `<span class="op-num">${fi(ix[2] ? d.rows : d.rows / rpk(d, ix[1][0]))}</span>`])));
  return (sg || notice('现有索引已能覆盖该语句的访问条件，不需要新增索引。' + (A.lint.some(f => f.id === 'S001' || f.id === 'R003' || f.id === 'R004') ? '若仍然很慢，问题多半出在写法上（见“内置建议”里的 R / S 规则）。' : ''), 'ok', '✓')) +
    `<h2 style="margin:16px 0 8px;font-size:14px">相关表的现有索引</h2>${idx}`;
}
function tabFmt() {
  const s = ADV.res; if (!s || !s.ran.fmt) return hint('SQL 美化', '格式化后的 SQL');
  const f = formatSql(s.sql);
  return `<div class="toolbar"><span class="sp"></span><button class="btn sm" data-on="advApplyFmt">替换编辑器内容</button></div>${tickCode(f, 'sql')}`;
}
function tabTool(kind) {
  const o = ADV.out[kind], T = TOOLS[kind];
  if (!o) return hint(T.name, T.name + ' 的结果');
  if (o.state === 'unconf') return `<div class="card" style="box-shadow:none;background:var(--panel2)"><h2>⚙ ${T.name} 未配置</h2><p style="margin:0 0 10px">${T.why}。</p>
    <button class="btn" data-on="advGoRules">改用内置建议</button></div>`;
  if (o.state === 'err') return notice(`<b>${T.name} 校验未通过：</b>${esc(o.msg)}`, 'bad', '✕');
  if (o.state === 'run') return `<div class="empty"><div class="ic">⏳</div><div>${T.name} 正在${kind === 'tuning' ? '采集表结构 / 统计信息 / 索引' + (ADV.tune.profile ? '并 PROFILE' : '') : '分析'}…</div></div>`;
  const stale = o.sql !== ADV.sql ? notice('SQL 已修改，下方是上一次运行的结果。', 'warn', '⚠') : '';
  return stale + tuningOut(o);
}
function profileSteps(A) {
  const scan = Math.max(1, A.plan.examined), v = scan * 0.00035, fs = A.plan.rows.some(r => r.extra.includes('Using filesort')), tp = A.plan.rows.some(r => r.extra.includes('Using temporary')), nt = A.plan.units.length;
  const steps = [['starting', .08], ['checking permissions', .01 * nt], ['Opening tables', .05 * nt], ['init', .04], ['System lock', .01], ['optimizing', .03 * nt], ['statistics', .04 * nt], ['preparing', .03], ['executing', .004]];
  if (tp) steps.push(['Creating tmp table', Math.min(5000, v * .15)]);
  steps.push(['Sending data', v]); if (fs) steps.push(['Sorting result', Math.min(60000, scan * .00008 + .2)]);
  steps.push(['end', .002], ['query end', .004], ['closing tables', .003], ['freeing items', .006], ['cleaning up', .003]);
  return steps;
}
function tuningOut(o) {
  const A = o.A, inst = instOf(ADV.inst), op = ADV.tune; if (A.err) return notice(esc(A.err), 'bad', '✕');
  if (!A.plan) return notice(esc(A.planErr || '无法分析') + '：SQLTuning 需要目标库里的表结构。', 'warn', '⚠');
  const sec = (title, inner, open) => `<details class="op-acc" ${open ? 'open' : ''}><summary>${title}</summary><div class="op-in">${inner}</div></details>`;
  const defs = [...new Map(A.plan.units.filter(u => u.T.def).map(u => [u.T.def.n, u.T.def])).values()];
  let h = sec('BASIC INFORMATION', kvList([['工具', 'MySQL SQL Tuning Tools v1.0（内置）'], ['MySQL 版本', esc(inst.ver)], ['实例 / 库', `${esc(inst.name)} / ${esc(ADV.db)}`], ['事务模式', 'SET SESSION TRANSACTION READ ONLY；max_execution_time=60s'], ['涉及的表', defs.map(d => `<span class="mono">${esc(d.n)}</span>`).join('、')]]), true);
  if (op.params) h += sec('SYSTEM PARAMETERS', tbl(['参数', '值'], [['innodb_buffer_pool_size', inst.env === '测试' ? '2G' : '32G'], ['join_buffer_size', '256K'], ['sort_buffer_size', '256K'], ['tmp_table_size', '16M'], ['max_heap_table_size', '16M'], ['long_query_time', '1.000000'], ['transaction_isolation', 'REPEATABLE-READ'], ['optimizer_switch', 'index_merge=on,…,hash_join=on']], {cells: r => [`<span class="mono">${r[0]}</span>`, `<span class="mono">${r[1]}</span>`]}));
  if (op.plan) {
    const rw = rewriteSql(o.sql, A);
    h += sec('SQL PLAN', tbl(['id', 'table', 'type', 'key', 'rows', 'filtered', 'Extra'], A.plan.rows, {cells: r => [r.id == null ? '' : r.id, r.table || '', r.type || '', esc(r.key || 'NULL'), r.rows == null ? '' : fi(r.rows), r.filtered == null ? '' : pctS(r.filtered), esc(r.extra.join('; '))]}) +
      `<div class="small muted" style="margin:10px 0 4px">OPTIMIZER REWRITE SQL（SHOW WARNINGS）</div>${tickCode(rw, 'sql')}`, true);
  }
  if (op.stats) h += sec('OBJECT STATISTICS', defs.map(d => `<div class="small b" style="margin:8px 0 4px">${esc(d.n)}　<span class="muted">${fmtN(d.rows)} 行 · ${fmtMB(d.size)} · ${esc(d.eng)}</span></div>${tickCode(ddlOf(ADV.inst, ADV.db, d), 'sql')}
      ${tbl(['索引', '列', '唯一', '估算基数'], d.idx || [], {cells: ix => [`<span class="mono">${esc(ix[0])}</span>`, `<span class="mono small">${ix[1].join(', ')}</span>`, ix[2] ? '是' : '', fi(ix[2] ? d.rows : d.rows / rpk(d, ix[1][0]))]})}`).join(''));
  if (op.profile) {
    const steps = profileSteps(A), tot = steps.reduce((a, s) => a + s[1], 0), mx = Math.max(...steps.map(s => s[1])), killed = tot > 60000;
    h += sec(`PROFILE（分步耗时 · 总计 ${fmtMs(Math.min(tot, 60000))}）`, `${killed ? notice('<b>ERROR 3024</b>：Query execution was interrupted, maximum statement execution time exceeded。语句在 60 秒被服务端终止（<code>KILL QUERY</code>），以下为终止前的耗时分布。', 'bad', '✕') : notice('PROFILE 已在只读事务内真实执行该查询。', 'info')}
      <div class="tbl-wrap"><table><thead><tr><th>Status</th><th style="width:48%">耗时分布</th><th class="op-num">Duration</th><th class="op-num">占比</th></tr></thead><tbody>${steps.map(([n, ms]) => `<tr><td class="mono small">${n}</td><td><div class="op-bar"><b class="${ms === mx ? 'hot' : ''}" style="width:${Math.max(1.5, Math.sqrt(ms / mx) * 100).toFixed(1)}%"></b></div></td><td class="op-num small">${durS(ms)}</td><td class="op-num small">${(ms / tot * 100).toFixed(1)}%</td></tr>`).join('')}</tbody></table></div>
      <div class="small muted" style="margin:10px 0 4px">SESSION STATUS (DIFFERENT)</div>${kvList([['Handler_read_next', fi(A.plan.examined * .6)], ['Handler_read_rnd_next', fi(A.plan.rows.some(r => r.type === 'ALL') ? A.plan.examined : 1)], ['Innodb_rows_read', fi(A.plan.examined)], ['Created_tmp_tables', A.plan.rows.some(r => r.extra.includes('Using temporary')) ? '1' : '0'], ['Sort_rows', fi(A.plan.rows.some(r => r.extra.includes('Using filesort')) ? Math.min(A.plan.examined, 2e6) : 0)]])}
      <div class="small b" style="margin-top:10px">EXECUTE TIME：${fmtMs(Math.min(tot, 60000))}${killed ? '（被终止）' : ''}</div>`, true);
  } else h += `<div class="small muted">未勾选 PROFILE，没有真实执行该语句。</div>`;
  return h;
}
function rewriteSql(sql, A) {   // 近似 SHOW WARNINGS 的“优化器改写”：补全库名 / 别名限定，关键字小写
  const P = parseStmt(sql), blocks = P.refs || []; if (!blocks.length) return '/* select#1 */ ' + P.sql;
  const tabs = blocks.map(r => ({r, alias: r.alias, name: r.name, def: r.derived ? null : tableDef(A.ctx, r.db, r.name)})), db = A.ctx.db;
  const SKW = new Set(['select', 'from', 'where', 'and', 'or', 'order', 'by', 'group', 'limit', 'join', 'on', 'left', 'inner', 'as', 'desc', 'asc', 'in', 'like', 'not', 'between', 'is', 'null', 'distinct', 'having', 'count', 'sum', 'date', 'right', 'union', 'all']);
  const body = P.sql.replace(/('(?:[^'\\]|\\.|'')*')|([a-z_]\w*)(?:\s*\.\s*([a-z_]\w*))?/gi, (m, str, a, b) => {
    if (str) return str;
    if (b) { const T = tabs.find(t => t.alias.toLowerCase() === a.toLowerCase()); return T && T.def ? `\`${db}\`.\`${T.alias}\`.\`${b}\`` : m; }
    if (SKW.has(a.toLowerCase())) return a.toLowerCase();
    const T = tabs.find(t => t.def && t.def.cols.some(c => c.n === a)); if (T) return `\`${db}\`.\`${T.alias}\`.\`${a}\``;
    const TT = tabs.find(t => t.name === a && t.def); return TT ? `\`${db}\`.\`${a}\`` : m;
  });
  return '/* select#1 */ ' + body.replace(/\s+/g, ' ');
}

/* ========================================================================
   7. 慢查日志页（pages.slowquery）
   数据：按指纹 × 天生成（种子固定），聚合 / 明细 / 趋势都从同一份生成函数导出。
   ======================================================================== */
const ri = (r, a, b) => a + Math.floor(r() * (b - a + 1));
const pickOf = (r, l) => l[Math.floor(r() * l.length)];
/* base：[每天次数, 平均耗时 ms, 平均扫描行, 平均返回行, 平均锁等待 ms]；since：首次出现距今天数；shape：趋势；batch：周末明显减少 */
const SQ_SPECS = [
  {inst: 1, db: 'order_db', shape: 'grow', base: [420, 3200, 910000, 20, 0], since: 140, who: 'app_order', sql: r => `SELECT id, order_no, amount, status, created_at FROM orders WHERE status = ${ri(r, 0, 4)} ORDER BY created_at DESC LIMIT ${ri(r, 8, 95) * 10000}, 20`},
  {inst: 1, db: 'order_db', shape: 'flat', base: [160, 5200, 3100000, 1, 0], since: 200, who: 'app_order', sql: r => `SELECT COUNT(*) FROM order_items WHERE sku_id = ${ri(r, 100000, 999999)}`},
  {inst: 1, db: 'order_db', shape: 'spike', base: [75, 4100, 4700000, 36, 0], since: 60, who: 'app_order', sql: r => `SELECT * FROM orders WHERE DATE(created_at) = '${dayOffset(-ri(r, 1, 20))}' AND status = ${ri(r, 0, 4)}`},
  {inst: 1, db: 'order_db', shape: 'flat', base: [310, 2100, 4, 1, 1850], since: 180, who: 'app_order', sql: r => `UPDATE orders SET status = ${ri(r, 1, 4)}, updated_at = NOW() WHERE order_no = '${'2026' + ri(r, 10000000, 99999999)}'`},
  {inst: 1, db: 'order_db', shape: 'new', base: [95, 6800, 71230887, 50, 0], since: 9, who: 'app_order', sql: r => `SELECT id, order_id, sku_name, qty FROM order_items WHERE sku_name LIKE '%${pickOf(r, ['手机壳', '充电器', '数据线', '耳机', '保护膜'])}' ORDER BY id DESC LIMIT 50`},
  {inst: 1, db: 'order_db', shape: 'flat', base: [4, 38000, 5200000, 0, 6200], since: 110, batch: true, who: 'job_archive', sql: r => `DELETE FROM order_log WHERE created_at < '${dayOffset(-ri(r, 180, 400))}' LIMIT 5000`},
  {inst: 1, db: 'order_db', shape: 'decline', base: [88, 3600, 1800000, 12, 0], since: 150, who: 'app_order', sql: r => `SELECT id, order_no, amount FROM orders WHERE id IN (SELECT order_id FROM order_items WHERE sku_id = ${ri(r, 100000, 999999)})`},
  {inst: 1, db: 'order_db', shape: 'flat', base: [230, 7400, 28460310, 1, 0], since: 95, who: 'app_order', sql: r => `SELECT id, order_no, amount, status FROM orders WHERE order_no = ${'2026' + ri(r, 10000000, 99999999) + '12'}`},
  {inst: 1, db: 'order_db', shape: 'grow', base: [60, 9200, 28460310, 1, 0], since: 70, who: 'app_report', sql: r => `SELECT COUNT(*) FROM orders WHERE created_at >= '${dayOffset(-ri(r, 1, 30))}' AND created_at < '${dayOffset(0)}'`},
  {inst: 2, db: 'order_db', shape: 'flat', base: [14, 18200, 9600000, 6, 0], since: 120, batch: true, who: 'report_ro', sql: r => `SELECT status, COUNT(*), SUM(amount) FROM orders WHERE created_at BETWEEN '${dayOffset(-ri(r, 30, 60))}' AND '${dayOffset(-ri(r, 1, 29))}' GROUP BY status`},
  {inst: 2, db: 'order_db', shape: 'spike', base: [9, 42000, 74000000, 800, 0], since: 80, batch: true, who: 'report_ro', sql: r => `SELECT o.order_no, o.amount, a.name FROM orders o JOIN user_addr a ON a.phone = o.receiver_phone WHERE o.status = ${ri(r, 1, 3)} AND o.created_at >= '${dayOffset(-ri(r, 3, 30))}'`},
  {inst: 2, db: 'order_archive', shape: 'flat', base: [22, 5600, 12000000, 100, 0], since: 170, who: 'report_ro', sql: r => `SELECT order_no, amount FROM orders_2025 WHERE amount BETWEEN ${ri(r, 100, 900)} AND ${ri(r, 1000, 5000)} ORDER BY created_at DESC LIMIT 100`},
  {inst: 2, db: 'order_db', shape: 'grow', base: [48, 3300, 6200000, 5000, 0], since: 55, who: 'report_ro', sql: r => `SELECT DISTINCT user_id FROM orders WHERE created_at >= '${dayOffset(-ri(r, 1, 14))}' AND amount >= ${ri(r, 5, 50) * 10}`},
  {inst: 2, db: 'order_db', shape: 'flat', base: [37, 4800, 2300000, 4, 0], since: 130, who: 'report_ro', sql: r => `SELECT id, order_no, status FROM orders WHERE user_id = ${ri(r, 1000000, 9999999)} OR receiver_phone = '1${ri(r, 3000000000, 3999999999)}'`},
  {inst: 2, db: 'order_db', shape: 'new', base: [120, 2700, 152300440, 3, 0], since: 6, who: 'app_cs', sql: r => `SELECT id, order_id, action, created_at FROM order_log WHERE operator = 'cs_${ri(r, 100, 999)}' AND created_at >= '${dayOffset(-ri(r, 1, 7))}'`},
  {inst: 3, db: 'pay_db', shape: 'flat', base: [140, 3900, 6400000, 22, 0], since: 160, who: 'app_pay', sql: r => `SELECT * FROM payments WHERE DATE_FORMAT(paid_at, '%Y-%m-%d') = '${dayOffset(-ri(r, 1, 10))}' AND channel = '${pickOf(r, ['alipay', 'wechat', 'card'])}'`},
  {inst: 3, db: 'pay_db', shape: 'grow', base: [26, 6100, 3300000, 1, 0], since: 90, batch: true, who: 'job_recon', sql: r => `SELECT SUM(amount) FROM payments WHERE status = 1 AND paid_at >= '${dayOffset(-ri(r, 1, 7))}' AND paid_at < '${dayOffset(0)}'`},
  {inst: 3, db: 'pay_db', shape: 'flat', base: [52, 2400, 1900000, 3, 0], since: 100, who: 'app_pay', sql: r => `SELECT * FROM refunds WHERE pay_no IN (SELECT pay_no FROM payments WHERE order_no = '${'2026' + ri(r, 10000000, 99999999)}')`},
  {inst: 3, db: 'pay_db', shape: 'spike', base: [31, 8800, 4100000, 20, 0], since: 45, who: 'app_pay', sql: r => `SELECT p.pay_no, r.refund_no FROM payments p LEFT JOIN refunds r ON r.pay_no = p.pay_no WHERE p.paid_at >= '${dayOffset(-ri(r, 1, 20))}' ORDER BY p.paid_at DESC LIMIT ${ri(r, 10, 90) * 1000}, 20`},
  {inst: 4, db: 'user_db', shape: 'grow', base: [380, 4300, 18230455, 1, 0], since: 85, who: 'app_user', sql: r => `SELECT id, username, phone FROM users WHERE phone = ${13000000000 + ri(r, 0, 999999999)}`},
  {inst: 4, db: 'user_db', shape: 'flat', base: [210, 3600, 18230455, 20, 0], since: 190, who: 'app_user', sql: r => `SELECT id, username, email FROM users WHERE username LIKE '%${pickOf(r, ['zhang', 'wang', 'li', 'chen', 'liu', 'yang'])}${ri(r, 1, 99)}' LIMIT 20`},
  {inst: 4, db: 'user_db', shape: 'flat', base: [8, 26000, 31000000, 50, 0], since: 140, batch: true, who: 'job_stat', sql: r => `SELECT user_id, COUNT(*) FROM login_log WHERE created_at >= '${dayOffset(-ri(r, 1, 7))}' GROUP BY user_id ORDER BY COUNT(*) DESC LIMIT 50`},
  {inst: 4, db: 'user_db', shape: 'decline', base: [95, 2900, 880000, 1, 0], since: 120, who: 'app_user', sql: r => `SELECT COUNT(DISTINCT ip) FROM login_log WHERE user_id = ${ri(r, 1000000, 9999999)} AND created_at > '${dayOffset(-ri(r, 7, 60))}'`},
  {inst: 4, db: 'user_db', shape: 'new', base: [64, 5100, 17002210, 10, 0], since: 12, who: 'app_cs', sql: r => `SELECT user_id, real_name, birthday FROM user_profile WHERE real_name LIKE '%${pickOf(r, ['伟', '芳', '磊', '娜', '静'])}%'`},
  {inst: 7, db: 'order_db', shape: 'flat', base: [6, 1800, 200000, 10, 0], since: 100, who: 'qa_tool', sql: r => `SELECT * FROM orders ORDER BY RAND() LIMIT ${ri(r, 1, 5) * 10}`},
  {inst: 7, db: 'order_db', shape: 'flat', base: [9, 1400, 340000, 3, 0], since: 75, who: 'qa_tool', sql: r => `SELECT * FROM orders o, order_items i WHERE o.id = i.order_id AND o.amount > ${ri(r, 1, 9) * 100}`},
];
SQ_SPECS.forEach((s, i) => { s.n = i + 1; s.id = 'fp' + String(i + 1).padStart(2, '0'); s.fp = fingerprint(s.sql(rng(7 + i))); s.hash = hash16(s.fp); });
function hash16(str) {   // 两路 FNV-1a 拼成 16 位十六进制，充当指纹 ID
  let a = 0x811c9dc5, b = 0x01000193;
  for (let i = 0; i < str.length; i++) { a = Math.imul(a ^ str.charCodeAt(i), 0x01000193) >>> 0; b = Math.imul(b ^ str.charCodeAt(i), 0x85ebca6b) >>> 0; }
  return (a.toString(16).padStart(8, '0') + b.toString(16).padStart(8, '0')).toUpperCase();
}
const fmtDur = ms => ms >= 3.6e6 ? (ms / 3.6e6).toFixed(1) + ' h' : fmtMs(ms);
const fmtR = n => n >= 1e4 ? fmt(n) : fi(n);
const SQ_CACHE = {};
function dayStat(s, k) {   // k = 距今天数（0 = 今天）
  if (k > s.since) return null;
  const key = s.id + ':' + k; if (SQ_CACHE[key] !== undefined) return SQ_CACHE[key];
  const r = rng(s.n * 7919 + k * 104729 + 13), dow = new Date(dayOffset(-k) + 'T00:00:00').getDay(), wk = dow === 0 || dow === 6 ? (s.batch ? .2 : .7) : 1;
  let f = 1, g = 1;
  if (s.shape === 'grow') f = Math.max(.45, 1 - .0055 * k); else if (s.shape === 'decline') f = .55 + .45 * Math.min(1, k / 60);
  else if (s.shape === 'spike' && k >= 1 && k <= 4) { f = 3.2; g = 1.5; } else if (s.shape === 'new') f = 1 + (s.since - k) * .04;
  const b = s.base, cnt = Math.max(0, Math.round(b[0] * f * wk * (.75 + .5 * r()) * (k === 0 ? .55 : 1)));
  let res = null;
  if (cnt > 0) { const avg = b[1] * g * (.85 + .3 * r()); res = {cnt, avg, max: avg * (2.4 + 3 * r()), scan: b[2] * (.9 + .2 * r()), ret: b[3] * (.9 + .2 * r()), lock: b[4] * (.8 + .4 * r())}; }
  return (SQ_CACHE[key] = res);
}
function aggOf(s, from, to) {
  const a = {cnt: 0, ms: 0, max: 0, scan: 0, ret: 0, lock: 0, first: dayOffset(-s.since), last: '', series: []};
  for (let k = daysBetween(to, TODAY); k <= daysBetween(from, TODAY); k++) {   // 从新到旧
    const d = dayStat(s, k); a.series.unshift(d ? d.cnt : 0); if (!d) continue;
    a.cnt += d.cnt; a.ms += d.cnt * d.avg; a.max = Math.max(a.max, d.max); a.scan += d.cnt * d.scan; a.ret += d.cnt * d.ret; a.lock += d.cnt * d.lock; if (!a.last) a.last = dayOffset(-k);
  }
  a.avg = a.cnt ? a.ms / a.cnt : 0; return a;
}
const SQ_USERS = {1: ['app_order', 'app_report', 'job_archive'], 2: ['report_ro', 'app_cs'], 3: ['app_pay', 'job_recon'], 4: ['app_user', 'job_stat', 'app_cs'], 7: ['qa_tool']};
function recordsOf(s, from, to, thr) {
  const out = [];
  for (let k = daysBetween(to, TODAY); k <= daysBetween(from, TODAY); k++) {
    const d = dayStat(s, k); if (!d) continue;
    const r = rng(s.n * 31337 + k * 977), n = Math.min(d.cnt, d.cnt > 150 ? 3 : 2);
    for (let j = 0; j < n; j++) {
      const dur = d.avg * (.6 + 1.1 * r()) / 1000; if (dur < thr) continue;
      const sec = ri(r, k === 0 ? 0 : 0, k === 0 ? 38000 : 86399), t = `${pad2(Math.floor(sec / 3600))}:${pad2(Math.floor(sec % 3600 / 60))}:${pad2(sec % 60)}`;
      out.push({fp: s, at: `${dayOffset(-k)} ${t}`, user: `${s.who}@10.20.${s.inst === 3 ? 8 : s.inst === 4 ? 9 : 7}.${ri(r, 11, 58)}`, dur, lock: Math.min(d.lock * (.5 + r()) / 1000, dur * .92), scan: Math.round(d.scan * (.8 + .4 * r())), ret: Math.max(0, Math.round(d.ret * (.8 + .4 * r()))), sql: s.sql(r)});
    }
  }
  return out;
}
const sqInsts = () => visibleInstances(true).filter(i => i.slowlog);                  // 含 Redis 实例：选中后换成 Redis 面板（p_redis.js）
const SQ = {inst: 0, db: '', preset: '7', from: dayOffset(-6), to: TODAY, thr: 1, kw: '', sort: 'ms', dir: -1, open: new Set(), fp: '', page: 1, dsort: 'at', dpage: 1};
const SQ_PRESETS = [['0', '今天'], ['3', '近 3 天'], ['7', '近 7 天'], ['30', '近 30 天'], ['93', '近 93 天（上限）'], ['custom', '自定义']];
function sqEnsure() { const l = sqInsts(); if (SQ.inst && !l.some(i => i.id === SQ.inst)) { SQ.inst = 0; SQ.db = ''; } }
function sqRows() {
  const kw = SQ.kw.trim().toLowerCase(), insts = sqInsts().map(i => i.id);
  return SQ_SPECS.filter(s => insts.includes(s.inst) && (!SQ.inst || s.inst === SQ.inst) && (!SQ.db || s.db === SQ.db) && (!kw || s.fp.includes(kw) || s.db.includes(kw) || s.hash.toLowerCase().includes(kw)))
    .map(s => Object.assign({s}, aggOf(s, SQ.from, SQ.to))).filter(a => a.cnt > 0 && a.avg / 1000 >= SQ.thr);
}
const spanDays = () => daysBetween(SQ.from, SQ.to) + 1;

pages.slowquery = {
  title: '慢查日志',
  render(sub) {
    sqEnsure();
    if (SQ.inst && window.RedisSlow && window.RedisSlow.isRedis(SQ.inst)) return CSS + window.RedisSlow.render(sub, sqInsts(), SQ.inst);   // Redis 实例：命令统计 / 慢命令明细 / 采集状态
    const tab = sub === 'detail' ? 'detail' : 'stat';
    const insts = sqInsts(), dbs = [...new Set(SQ_SPECS.filter(s => insts.some(i => i.id === s.inst) && (!SQ.inst || s.inst === SQ.inst)).map(s => s.db))];
    if (SQ.db && !dbs.includes(SQ.db)) SQ.db = '';
    return `${CSS}
    <div class="card" style="margin-bottom:14px"><div class="toolbar" style="margin:0;align-items:flex-end;gap:12px">
      <label class="op-fl">实例<select data-onchange="sqInst"><option value="0">全部有慢日志的实例</option>${insts.map(i => `<option value="${i.id}" ${i.id === SQ.inst ? 'selected' : ''}>${esc(i.name)}</option>`).join('')}</select></label>
      <label class="op-fl">数据库<select data-onchange="sqDb"><option value="">全部数据库</option>${dbs.map(d => `<option ${d === SQ.db ? 'selected' : ''}>${esc(d)}</option>`).join('')}</select></label>
      <label class="op-fl">时间范围<select data-onchange="sqPreset">${SQ_PRESETS.map(([v, t]) => `<option value="${v}" ${SQ.preset === v ? 'selected' : ''}>${t}</option>`).join('')}</select></label>
      <label class="op-fl">开始日期<input type="text" value="${SQ.from}" data-onchange="sqDate" data-k="from" style="width:112px" placeholder="YYYY-MM-DD"></label>
      <label class="op-fl">结束日期<input type="text" value="${SQ.to}" data-onchange="sqDate" data-k="to" style="width:112px" placeholder="YYYY-MM-DD"></label>
      <label class="op-fl">耗时 ≥<select data-onchange="sqThr">${[0, 1, 2, 5, 10, 30].map(v => `<option value="${v}" ${SQ.thr === v ? 'selected' : ''}>${v} s</option>`).join('')}</select></label>
      <label class="op-fl">关键字<input type="search" value="${esc(SQ.kw)}" data-oninput="sqKw" placeholder="表名 / 指纹片段" style="width:170px"></label>
      <button class="btn" data-on="sqReset">重置</button></div></div>
    ${tabs('slowquery', [['stat', '统计'], ['detail', '明细']], tab)}
    <div id="sqBody">${tab === 'stat' ? sqStat() : sqDetail()}</div>`;
  }
};
function sqRefresh() { const el = $('#sqBody'); if (el) el.innerHTML = ROUTE.sub === 'detail' ? sqDetail() : sqStat(); }
ON.sqInst = (d, el) => { SQ.inst = +el.value; SQ.db = ''; SQ.page = SQ.dpage = 1; rerender(); };
ON.sqDb = (d, el) => { SQ.db = el.value; SQ.page = SQ.dpage = 1; rerender(); };
ON.sqPreset = (d, el) => { SQ.preset = el.value; if (el.value !== 'custom') { SQ.to = TODAY; SQ.from = dayOffset(-(+el.value === 0 ? 0 : +el.value - 1)); } SQ.page = SQ.dpage = 1; rerender(); };
ON.sqDate = (d, el) => {
  const v = el.value.trim();
  if (!/^\d{4}-\d{2}-\d{2}$/.test(v) || isNaN(Date.parse(v))) { toast('日期格式应为 YYYY-MM-DD'); return rerender(); }
  if (v > TODAY) { toast('不能晚于今天（2026-10-07）'); return rerender(); }
  const nf = d.k === 'from' ? v : SQ.from, nt = d.k === 'to' ? v : SQ.to;
  if (nf > nt) { toast('开始日期不能晚于结束日期'); return rerender(); }
  if (daysBetween(nf, nt) + 1 > 93) { toast('查询时间范围最长 93 天，请缩小范围'); return rerender(); }
  SQ.from = nf; SQ.to = nt; SQ.preset = 'custom'; SQ.page = SQ.dpage = 1; rerender();
};
ON.sqThr = (d, el) => { SQ.thr = +el.value; SQ.page = SQ.dpage = 1; rerender(); };
ON.sqKw = (d, el) => { SQ.kw = el.value; SQ.page = SQ.dpage = 1; sqRefresh(); };
ON.sqReset = () => { Object.assign(SQ, {inst: 0, db: '', preset: '7', from: dayOffset(-6), to: TODAY, thr: 1, kw: '', fp: '', page: 1, dpage: 1}); rerender(); };
ON.sqSort = d => { if (SQ.sort === d.k) SQ.dir = -SQ.dir; else { SQ.sort = d.k; SQ.dir = -1; } SQ.page = 1; sqRefresh(); };
ON.sqOpen = d => { if (SQ.open.has(d.id)) SQ.open.delete(d.id); else SQ.open.add(d.id); sqRefresh(); };
ON.sqPage = d => { SQ.page = +d.p; sqRefresh(); };
ON.sqDPage = d => { SQ.dpage = +d.p; sqRefresh(); };
ON.sqDSort = d => { SQ.dsort = d.k; SQ.dpage = 1; sqRefresh(); };
ON.sqToDetail = d => { SQ.fp = d.id; SQ.dpage = 1; go('slowquery', 'detail'); };
ON.sqClearFp = () => { SQ.fp = ''; SQ.dpage = 1; rerender(); };
ON.sqOpenAdv = d => {
  const s = SQ_SPECS.find(x => x.id === d.id), sql = d.sql || s.sql(rng(s.n * 11));
  try { sessionStorage.setItem('advisor_sql_content', sql); sessionStorage.setItem('advisor_db_name', s.db); sessionStorage.setItem('advisor_instance_name', instOf(s.inst).name); } catch (e) {}
  closeModal(); closeDrawer(); go('sqladvisor');
};
ON.sqDetailRow = d => {
  const [id, at] = d.rec.split('|'), s = SQ_SPECS.find(x => x.id === id), rec = recordsOf(s, SQ.from, SQ.to, SQ.thr).find(x => x.at === at);
  if (!rec) return;
  openDrawer('慢日志明细', `${kvList([['开始时间', rec.at], ['实例 / 库', `${esc(instOf(s.inst).name)} / ${esc(s.db)}`], ['用户@主机', `<span class="mono">${esc(rec.user)}</span>`], ['耗时', `<b>${rec.dur.toFixed(2)} s</b>`], ['锁等待', rec.lock.toFixed(2) + ' s'], ['扫描 / 返回行', `${fi(rec.scan)} / ${fi(rec.ret)}`], ['指纹', `<span class="mono small">${esc(s.fp)}</span>`]])}
    <div class="small muted" style="margin:12px 0 4px">SQL 语句</div>${tickCode(rec.sql, 'sql')}
    <div class="toolbar">${B('slowquery.advise', '获取优化建议', {cls: 'primary', data: {fp: s.id, sql: rec.sql, scan: rec.scan, ret: rec.ret, dur: rec.dur, lock: rec.lock}})}${canView('sqladvisor') ? `<button class="btn" data-on="sqOpenAdv" data-id="${s.id}" data-sql="${esc(rec.sql)}">在优化工具中打开</button>` : ''}</div>`);
};
const recCmp = (a, b) => SQ.dsort === 'dur' ? b.dur - a.dur : (a.at < b.at ? 1 : -1);

function sqStat() {
  const long = spanDays() > 93; if (long) return notice('查询时间范围最长 93 天，请缩小范围后再查。', 'bad', '✕');
  const rows = sqRows(), val = {cnt: a => a.cnt, ms: a => a.ms, avg: a => a.avg, max: a => a.max, scan: a => a.scan, ret: a => a.ret, first: a => a.first, last: a => a.last};
  rows.sort((a, b) => { const x = val[SQ.sort](a), y = val[SQ.sort](b); return (x < y ? -1 : x > y ? 1 : 0) * SQ.dir; });
  const tot = rows.reduce((a, r) => ({cnt: a.cnt + r.cnt, ms: a.ms + r.ms, max: Math.max(a.max, r.max)}), {cnt: 0, ms: 0, max: 0}), size = 15, pages_ = Math.max(1, Math.ceil(rows.length / size)), pg = Math.min(SQ.page, pages_), cur = rows.slice((pg - 1) * size, pg * size);
  const th = (k, t) => `<a data-on="sqSort" data-k="${k}">${t}${SQ.sort === k ? (SQ.dir < 0 ? ' ▾' : ' ▴') : ''}</a>`;
  const kpi = (v, l, d) => `<div class="card kpi"><div class="v">${v}</div><div class="l">${l}</div>${d ? `<div class="d muted">${d}</div>` : ''}</div>`;
  const heads = ['', 'SQL 指纹', '实例 / 库', th('cnt', '次数'), th('ms', '总耗时'), th('avg', '平均耗时'), th('max', '最大耗时'), th('scan', '扫描行（总）'), th('ret', '返回行（总）'), th('first', '首次出现'), th('last', '最近出现'), '趋势', '操作'];
  const body = [];
  cur.forEach(a => {
    const s = a.s, open = SQ.open.has(s.id), inst = instOf(s.inst);
    body.push(`<tr class="click"><td><a data-on="sqOpen" data-id="${s.id}" title="展开代表 SQL 与趋势">${open ? '▾' : '▸'}</a></td>
      <td><a data-on="sqToDetail" data-id="${s.id}"><div class="op-sql mono">${esc(s.fp)}</div></a><div class="small muted mono">${s.hash}</div></td>
      <td class="small nowrap">${esc(inst.name)}<br><span class="muted">${esc(s.db)}</span></td>
      <td class="op-num">${fi(a.cnt)}</td><td class="op-num">${fmtDur(a.ms)}</td><td class="op-num"><b>${fmtMs(a.avg)}</b></td><td class="op-num">${fmtMs(a.max)}</td>
      <td class="op-num">${fmtR(a.scan)}<div class="small muted">均 ${fmtR(a.scan / a.cnt)}</div></td><td class="op-num">${fmtR(a.ret)}<div class="small muted">均 ${fmtR(a.ret / a.cnt)}</div></td>
      <td class="small nowrap">${a.first}</td><td class="small nowrap">${a.last}</td><td>${spark(a.series.length > 1 ? downsample(a.series, 14) : [0, a.cnt], 'var(--c1)', 84, 24)}</td>
      <td>${B('slowquery.advise', '优化建议', {cls: 'sm', data: {fp: s.id}})}</td></tr>`);
    if (open) {
      const days = a.series.length, lab = Array.from({length: 5}, (_, i) => dayOffset(-Math.round((days - 1) * (4 - i) / 4), SQ.to).slice(5));
      const avgSeries = []; for (let k = daysBetween(SQ.to, TODAY) + days - 1; k >= daysBetween(SQ.to, TODAY); k--) { const d = dayStat(s, k); avgSeries.push(d ? d.avg / 1000 : 0); }
      body.push(`<tr><td></td><td colspan="12" style="background:var(--panel2)"><div class="grid g2" style="align-items:start"><div><div class="small muted" style="margin-bottom:4px">代表 SQL（取样）</div>${tickCode(s.sql(rng(s.n * 11)), 'sql')}
        <div class="small muted">涉及：<span class="mono">${esc(inst.name)}</span> · ${esc(s.db)} · 采集账号 <span class="mono">${esc(s.who)}</span></div>
        <div class="toolbar" style="margin-top:8px">${B('slowquery.advise', '获取优化建议', {cls: 'primary sm', data: {fp: s.id}})}${canView('sqladvisor') ? `<button class="btn sm" data-on="sqOpenAdv" data-id="${s.id}">在优化工具中打开</button>` : ''}<button class="btn sm" data-on="sqToDetail" data-id="${s.id}">查看明细</button></div></div>
        <div><div class="small muted">每日慢查次数</div>${lineChart([{name: '次数', data: a.series.length > 1 ? a.series : [0, a.cnt], color: 'var(--c1)'}], {w: 520, h: 120, xl: lab})}
        <div class="small muted">每日平均耗时（s）</div>${lineChart([{name: '平均耗时', data: avgSeries.length > 1 ? avgSeries : [0, a.avg / 1000], color: 'var(--c3)'}], {w: 520, h: 120, unit: 's', xl: lab})}</div></div></td></tr>`);
    }
  });
  const pager = `<div class="pager"><span>共 ${rows.length} 个指纹</span>${Array.from({length: pages_}, (_, i) => `<a data-on="sqPage" data-p="${i + 1}" class="${i + 1 === pg ? 'on' : ''}">${i + 1}</a>`).join('')}</div>`;
  return `<div class="grid g4" style="margin-bottom:14px">${kpi(rows.length, '慢 SQL 指纹数', `${spanDays()} 天内`)}${kpi(fi(tot.cnt), '慢查询总次数', '')}${kpi(fmtDur(tot.ms), '总耗时', tot.cnt ? `平均 ${fmtMs(tot.ms / tot.cnt)}` : '')}${kpi(fmtMs(tot.max), '最慢一次', '')}</div>
    <div class="tbl-wrap"><table class="op-th"><thead><tr>${heads.map(h => `<th>${h}</th>`).join('')}</tr></thead><tbody>${body.length ? body.join('') : `<tr><td colspan="${heads.length}">${empty('该条件下没有慢 SQL')}</td></tr>`}</tbody></table></div>${pager}`;
}
const downsample = (arr, n) => { if (arr.length <= n) return arr; const out = []; for (let i = 0; i < n; i++) { const a = Math.floor(i * arr.length / n), b = Math.floor((i + 1) * arr.length / n); out.push(arr.slice(a, b).reduce((x, y) => x + y, 0)); } return out; };
function sqDetail() {
  if (spanDays() > 93) return notice('查询时间范围最长 93 天，请缩小范围后再查。', 'bad', '✕');
  const kw = SQ.kw.trim().toLowerCase(), insts = sqInsts().map(i => i.id);
  const specs = SQ_SPECS.filter(s => insts.includes(s.inst) && (!SQ.inst || s.inst === SQ.inst) && (!SQ.db || s.db === SQ.db) && (!SQ.fp || s.id === SQ.fp) && (!kw || s.fp.includes(kw)));
  let recs = [].concat(...specs.map(s => recordsOf(s, SQ.from, SQ.to, SQ.thr))).sort(recCmp);
  const total = Math.min(recs.length, 200), size = 20, n = Math.max(1, Math.ceil(total / size)), pg = Math.min(SQ.dpage, n); recs = recs.slice(0, 200);
  const cur = recs.slice((pg - 1) * size, pg * size), chip = SQ.fp ? SQ_SPECS.find(s => s.id === SQ.fp) : null;
  const th = (k, t) => `<a data-on="sqDSort" data-k="${k}">${t}${SQ.dsort === k ? ' ▾' : ''}</a>`;
  return `${chip ? `<div class="notice"><span>⌕</span><div>仅显示指纹 <span class="mono">${esc(chip.fp.slice(0, 90))}</span> 的记录　<a data-on="sqClearFp">取消过滤 ✕</a></div></div>` : ''}
    <div class="tbl-wrap"><table class="op-th"><thead><tr><th>${th('at', '开始时间')}</th><th>实例 / 库</th><th>用户@主机</th><th>${th('dur', '耗时')}</th><th>锁等待</th><th>扫描行</th><th>返回行</th><th>SQL</th></tr></thead><tbody>
    ${cur.length ? cur.map(r => `<tr class="click" data-on="sqDetailRow" data-rec="${r.fp.id}|${r.at}"><td class="nowrap small">${r.at}</td><td class="small nowrap">${esc(instOf(r.fp.inst).name)}<br><span class="muted">${esc(r.fp.db)}</span></td><td class="mono small nowrap">${esc(r.user)}</td>
      <td class="op-num"><b>${r.dur.toFixed(2)}</b> s</td><td class="op-num small">${r.lock.toFixed(2)} s</td><td class="op-num">${fi(r.scan)}</td><td class="op-num">${fi(r.ret)}</td><td><div class="op-sql mono">${esc(r.sql)}</div></td></tr>`).join('') : `<tr><td colspan="8">${empty('没有符合条件的慢日志记录')}</td></tr>`}</tbody></table></div>
    <div class="pager"><span>共 ${total} 条</span>${Array.from({length: n}, (_, i) => `<a data-on="sqDPage" data-p="${i + 1}" class="${i + 1 === pg ? 'on' : ''}">${i + 1}</a>`).join('')}</div>`;
}

/* ---------- 获取优化建议（基于规则，不连目标库）---------- */
function candidateIndex(P, ctx) {
  const b = P.refs && P.refs.length === 1 && !P.refs[0].derived ? P : null; if (!b || P.hasOr) return '';
  const eq = [], rng = [];
  P.conds.forEach(c => { if (c.kind !== 'cmp' || c.fn || c.arith || c.neg) return; const k = condClass(c); if ((k === 'eq' || k === 'in') && !eq.includes(c.col)) eq.push(c.col); else if (k === 'range' && !rng.includes(c.col)) rng.push(c.col); });
  const cols = eq.concat(rng.slice(0, 1)); if (!cols.length && P.orderItems.length && !P.orderItems.some(x => x.expr)) cols.push(...P.orderItems.map(x => x.col));
  const def = ctx ? tableDef(ctx, P.refs[0].db, P.refs[0].name) : null, have = def && (def.idx || []).find(ix => cols.length && ix[1].slice(0, cols.length).join() === cols.join());
  if (have) return {have: have[0], cols};
  return cols.length ? `ALTER TABLE \`${P.refs[0].name}\` ADD INDEX idx_${cols.map(c => c.replace(/_(at|id|no)$/, '')).join('_')}(${cols.map(c => '`' + c + '`').join(',')});` : '';
}
ACTIONS['slowquery.advise'] = d => {
  const s = SQ_SPECS.find(x => x.id === d.fp), sql = d.sql || s.sql(rng(s.n * 11)), inst = instOf(s.inst), a = d.scan ? {avg: +d.dur * 1000, scan: +d.scan, ret: +d.ret, cnt: 1} : aggOf(s, SQ.from, SQ.to);
  const P = parseStmt(sql), list = []; allBlocks(P).forEach(b => lintBlock(b, list));
  const seen = new Set(), finds = list.filter(f => { const k = f.id + f.title; if (seen.has(k)) return false; seen.add(k); return true; });
  const scan = d.scan ? +d.scan : a.scan / Math.max(1, a.cnt), ret = d.scan ? +d.ret : a.ret / Math.max(1, a.cnt);
  if (scan >= 1e4 && scan / Math.max(1, ret) >= 100) finds.push({id: 'R100', lv: scan >= 1e6 ? '高' : '中', title: '扫描行数远大于返回行数：缺少索引的迹象', detail: `平均每次扫描 <b>${fi(scan)}</b> 行、仅返回 ${fi(ret)} 行（比例约 ${fi(scan / Math.max(1, ret))}:1）。`, fix: '为 WHERE / JOIN 条件列建立合适的索引（以 EXPLAIN 为准）。'});
  const lockMs = d.scan ? +d.lock * 1000 : a.lock / Math.max(1, a.cnt), durMs = d.scan ? +d.dur * 1000 : a.avg;
  if (durMs > 0 && lockMs / durMs >= .5) finds.push({id: 'R102', lv: '中', title: '耗时主要消耗在锁等待', detail: `平均锁等待 <b>${fmtMs(lockMs)}</b>，占总耗时的 ${Math.min(100, Math.round(lockMs / durMs * 100))}%；扫描行数很少，说明慢的不是查找而是在排队等锁。`, fix: '排查同一行 / 同一范围上的长事务与热点行更新，缩短事务、避免事务内等待外部调用，按固定顺序更新多行。'});
  const jc = allBlocks(P).flatMap(b => b.refs.flatMap(r => r.onConds.filter(c => c.kind === 'cmp' && c.op === '=' && c.rhsK && c.rhsK.k === 'col').map(c => `${c.alias ? c.alias + '.' : ''}${c.col} = ${c.rhs}`)));
  if (jc.length) finds.push({id: 'R101', lv: '提示', title: '确认连接列上都有索引', detail: `连接条件：<code>${jc.map(esc).join('</code>、<code>')}</code>。被驱动表的连接列没有索引时，每个外层行都要扫描整表（EXPLAIN 里会出现 Using join buffer）。`, fix: '到「优化工具」执行 EXPLAIN 确认，并按“索引建议”补齐。'});
  const idx = candidateIndex(P, mkCtx(s.inst, s.db)), score = scoreOf(finds);
  audit('slowquery.advise', `获取优化建议：${s.fp.slice(0, 60)}`, '成功', `${inst.name} / ${s.db} / 命中 ${finds.length} 条`);
  openModal({title: '获取优化建议', wide: true, ok: '', cancel: '关闭', code: 'slowquery.advise', body: `
    ${kvList([['实例 / 库', `${esc(inst.name)} / ${esc(s.db)}`], ['平均耗时', fmtMs(a.avg)], ['扫描 / 返回行（均）', `${fi(scan)} / ${fi(ret)}`], ['指纹', `<span class="mono small">${esc(s.fp)}</span>`]])}
    <div class="small muted" style="margin:12px 0 4px">SQL</div>${tickCode(sql, 'sql')}
    <div class="score" style="margin:10px 0">${ring(score, '评分')}<div><div class="b" style="font-size:15px">命中 ${finds.length} 条建议</div></div></div>
    ${finds.length ? findingsHtml(finds) : notice('没有命中规则。这条语句慢，可能与数据量、锁等待或索引选择有关。', 'ok', '✓')}
    ${idx && idx.have ? `<div class="small muted" style="margin-top:10px">推测的候选索引列 <code>${idx.cols.join(', ')}</code> 已被现有索引 <code>${esc(idx.have)}</code> 覆盖。</div>` : idx ? `<div class="small muted" style="margin:10px 0 4px">候选索引（仅供参考）</div>${tickCode(idx, 'sql')}` : ''}
    ${canView('sqladvisor') ? `<div class="toolbar" style="margin-top:12px"><button class="btn primary" data-on="sqOpenAdv" data-id="${s.id}" data-sql="${esc(sql)}">在优化工具中打开</button></div>` : ''}`});
};
})();
