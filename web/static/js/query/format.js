/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: format.js
 * @time: 2026/10/28
 */
// SQL 格式化：主要子句（FROM / WHERE / GROUP BY / ORDER BY / LIMIT / JOIN …）另起一行，WHERE 里的 AND / OR 缩进换行，
// 关键字大写。只做排版，不改变语义；字符串、注释、引号标识符原样保留。

const KEYWORDS = new Set('select from where and or not in is null like between group by order having limit offset join left right inner outer cross full on as distinct union all exists case when then else end desc asc show explain with top using interval'.split(' '));

export function formatSQL(sql) {
  const toks = [];
  const re = /(--[^\n]*|#[^\n]*|\/\*[\s\S]*?\*\/)|('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*")|(`[^`]*`)|(\[[^\]]*\])|([A-Za-z_][A-Za-z_0-9$]*)|(\s+)|([\s\S])/g;
  let x;
  while ((x = re.exec(sql))) toks.push(x[0]);
  const isWs = (t) => /^\s+$/.test(t);
  const nextW = (j) => { j++; while (j < toks.length && isWs(toks[j])) j++; return (toks[j] || '').toLowerCase(); };
  let out = '';
  let depth = 0;
  let clause = '';
  let prev = '';
  let between = false;
  const nl = (ind) => { out = out.replace(/[ ]+$/, '') + '\n' + ' '.repeat(ind); };
  for (let i = 0; i < toks.length; i++) {
    const t = toks[i];
    if (isWs(t)) { if (out && !/\s$/.test(out)) out += ' '; continue; }
    if (/^[A-Za-z_]/.test(t)) {
      const lw = t.toLowerCase();
      if (depth === 0) {
        if (['from', 'where', 'having', 'limit', 'union'].includes(lw)) { nl(0); clause = lw; }
        else if ((lw === 'group' || lw === 'order') && nextW(i) === 'by') { nl(0); clause = lw; }
        else if (['left', 'right', 'inner', 'cross', 'full'].includes(lw) || (lw === 'join' && !['left', 'right', 'inner', 'cross', 'full', 'outer', 'natural'].includes(prev))) nl(0);
        else if (lw === 'select') clause = 'select';
        else if ((lw === 'and' || lw === 'or') && clause === 'where' && !between) nl(2);
      }
      if (lw === 'between') between = true; else if (lw === 'and' && between) between = false;
      out += KEYWORDS.has(lw) ? t.toUpperCase() : t;
      prev = lw;
      continue;
    }
    if (t === '(') depth++; else if (t === ')') depth = Math.max(0, depth - 1);
    out += t;
    if (t === ',' && depth === 0 && clause === 'select' && out.length - out.lastIndexOf('\n') > 72) nl(4);
    prev = t;
  }
  return out.replace(/[ ]+\n/g, '\n').trim();
}
