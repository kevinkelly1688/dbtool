/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: statements.js
 * @time: 2026/10/28
 */
// 语句切分与定位（浏览器端，仅用于“运行光标所在语句”；服务端会再次校验与切分）。

// 返回顶层分号切出的语句区间 [{start, end}]（end 不含分号），忽略引号与注释里的分号。
export function splitRanges(text) {
  const out = [];
  let start = 0;
  let i = 0;
  const n = text.length;
  const push = (end) => {
    const seg = text.slice(start, end);
    if (seg.replace(/\/\*[\s\S]*?\*\/|--[^\n]*|#[^\n]*/g, '').trim() !== '') out.push({ start, end });
  };
  while (i < n) {
    const c = text[i];
    if (c === "'" || c === '"' || c === '`') {
      i++;
      while (i < n) {
        if (text[i] === '\\' && c !== '`') { i += 2; continue; }
        if (text[i] === c) {
          if (text[i + 1] === c) { i += 2; continue; }
          i++;
          break;
        }
        i++;
      }
    } else if (c === '#' || (c === '-' && text[i + 1] === '-' && /[\s]|$/.test(text[i + 2] || ' '))) {
      while (i < n && text[i] !== '\n') i++;
    } else if (c === '/' && text[i + 1] === '*') {
      i += 2;
      while (i < n && !(text[i] === '*' && text[i + 1] === '/')) i++;
      i += 2;
    } else if (c === ';') {
      push(i);
      start = i + 1;
      i++;
    } else {
      i++;
    }
  }
  push(n);
  return out;
}

// 光标偏移所在（或最近的前一条）语句在 ranges 里的下标。
function indexAt(ranges, offset) {
  let i = ranges.findIndex((r) => offset >= r.start && offset <= r.end + 1);
  if (i < 0) {
    for (let k = 0; k < ranges.length; k++) if (ranges[k].end < offset) i = k;
    if (i < 0) i = 0;
  }
  return i;
}

// 光标偏移所在（或最近的前一条）语句；找不到返回 null。
export function statementAt(text, offset) {
  const ranges = splitRanges(text);
  if (!ranges.length) return null;
  const hit = ranges[indexAt(ranges, offset)];
  return text.slice(hit.start, hit.end).trim();
}

const LEADING_NOISE = /^(?:\s+|\/\*[^]*?\*\/|--[^\n]*(?:\n|$)|#[^\n]*(?:\n|$))+/;

// 语句（去掉开头的注释后）是不是 SET。拼接语句时分号前垫一个换行：语句末尾若是 -- 行注释，分号不会被注释吞掉。
export function isSetStatement(stmt) {
  return /^set\b/i.test(String(stmt).replace(LEADING_NOISE, ''));
}

// MySQL 系脚本的运行目标：{ prelude, stmt }。
// 脚本开头连续的 SET 语句（给用户变量赋值等）是“前置语句”，服务端会先在同一个会话里执行它们，再执行 stmt：
//   - 光标在后面的某条查询上：prelude = 开头的 SET，stmt = 这条查询；
//   - 光标就在开头的某条 SET 上：prelude = 开头的 SET，stmt = 紧随其后的第一条语句（运行“整段准备工作 + 第一条查询”）；
//   - 脚本里没有开头的 SET：prelude 为空，stmt 与 statementAt 相同；
//   - 整份脚本全是 SET：原样交给服务端，由它提示“没有要运行的查询”。
export function runParts(text, offset) {
  const ranges = splitRanges(text);
  if (!ranges.length) return { prelude: '', stmt: text.trim() };
  const segs = ranges.map((r) => text.slice(r.start, r.end).trim());
  let n = 0;
  while (n < segs.length && isSetStatement(segs[n])) n++;
  const i = indexAt(ranges, offset);
  if (n === 0) return { prelude: '', stmt: segs[i] };
  if (n === segs.length) return { prelude: '', stmt: segs.join('\n;\n') };
  return { prelude: segs.slice(0, n).join('\n;\n'), stmt: segs[i < n ? n : i] };
}
