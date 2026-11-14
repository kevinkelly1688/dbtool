/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: md.js
 * @time: 2026/10/28
 */
// 极简 Markdown 渲染（只支持文档用到的子集），移植自原型 p_help.js。
// 安全：所有文本先转义再拼 HTML；链接只放行站内锚点（#...）和 http(s)；不支持原样 HTML。

export function esc(s) {
  return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// 行内：`code`、**粗体**、==高亮==、[文字](#章节) 与 [文字](http(s)://…)；其余一律转义。
export function mdInline(s) {
  const codes = [];
  let h = String(s).replace(/`([^`]+)`/g, (m, c) => {
    codes.push('<code>' + esc(c) + '</code>');
    return '\u0000' + (codes.length - 1) + '\u0000';
  });
  h = esc(h).replace(/\*\*(.+?)\*\*/g, '<b>$1</b>').replace(/==(.+?)==/g, '<mark>$1</mark>');
  h = h.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, t, u) => {
    u = u.replace(/&amp;/g, '&');
    if (u.charAt(0) === '#') return '<a href="#' + esc(u.slice(1)) + '">' + t + '</a>';
    if (/^https?:\/\//i.test(u)) return '<a href="' + esc(u) + '" target="_blank" rel="noopener noreferrer">' + t + '</a>';
    return t;
  });
  return h.replace(/\u0000(\d+)\u0000/g, (m, i) => codes[+i]);
}

function splitCells(line) {
  let s = line.trim();
  if (s.charAt(0) === '|') s = s.slice(1);
  if (s.charAt(s.length - 1) === '|') s = s.slice(0, -1);
  return s.split('|').map((c) => c.trim());
}

const isItem = (l) => /^\s*([-*]|\d+\.)\s+/.test(l);


// 列表：支持一层缩进（行首 ≥ 2 个空格）的子项，挂在上一条顶层项下面；子项的有序 / 无序由它自己的标记决定。
function renderList(lines) {
  const itemOf = (l) => {
    const m = /^(\s*)([-*]|\d+\.)\s+(.*)$/.exec(l);
    return { indent: m[1].length, ol: /\d/.test(m[2]), text: m[3] };
  };
  const all = lines.map(itemOf);
  const base = Math.min(...all.map((x) => x.indent));
  const top = [];
  all.forEach((x) => {
    if (x.indent - base >= 2 && top.length) top[top.length - 1].kids.push(x);
    else top.push({ ...x, kids: [] });
  });
  const wrap = (arr, html) => { const tag = arr[0].ol ? 'ol' : 'ul'; return '<' + tag + '>' + html + '</' + tag + '>'; };
  return wrap(top, top.map((t) => '<li>' + mdInline(t.text) +
    (t.kids.length ? wrap(t.kids, t.kids.map((k) => '<li>' + mdInline(k.text) + '</li>').join('')) : '') + '</li>').join(''));
}

// 块级：标题（## 进目录）、表格、列表（支持一层缩进子项）、> 提示块（以 ⚠ / 注意 / 警告 开头为警告）、``` 代码块。
// 返回 { html, toc: [{id, t}] }。
export function mdRender(src) {
  const L = String(src).replace(/\r\n?/g, '\n').split('\n');
  const out = [];
  const toc = [];
  const para = [];
  let i = 0;
  let hn = 0;
  const flush = () => {
    if (para.length) { out.push('<p>' + mdInline(para.join(' ')) + '</p>'); para.length = 0; }
  };
  while (i < L.length) {
    const ln = L[i];
    let m;
    if (/^```/.test(ln)) {
      flush();
      const b = [];
      i++;
      while (i < L.length && !/^```/.test(L[i])) b.push(L[i++]);
      i++;
      out.push('<pre><code>' + esc(b.join('\n')) + '</code></pre>');
      continue;
    }
    if (!ln.trim()) { flush(); i++; continue; }
    if ((m = /^(#{1,4})\s+(.*)$/.exec(ln))) {
      flush();
      const lv = m[1].length;
      const id = 'hp-h' + (++hn);
      if (lv === 2) toc.push({ id, t: m[2].replace(/[`*]/g, '') });
      out.push(lv === 1 ? '<h1>' + mdInline(m[2]) + '</h1>' : '<h' + lv + ' id="' + id + '">' + mdInline(m[2]) + '</h' + lv + '>');
      i++;
      continue;
    }
    if (/^\s*\|/.test(ln) && L[i + 1] && /^\s*\|?\s*:?-{2,}/.test(L[i + 1])) {
      flush();
      const head = splitCells(ln);
      i += 2;
      const rows = [];
      while (i < L.length && /^\s*\|/.test(L[i])) rows.push(splitCells(L[i++]));
      out.push('<div class="tbl-wrap"><table><thead><tr>' + head.map((c) => '<th>' + mdInline(c) + '</th>').join('') +
        '</tr></thead><tbody>' + rows.map((r) => '<tr>' + head.map((_, k) => '<td>' + mdInline(r[k] || '') + '</td>').join('') + '</tr>').join('') +
        '</tbody></table></div>');
      continue;
    }
    if (/^>/.test(ln)) {
      flush();
      const b = [];
      while (i < L.length && /^>/.test(L[i])) b.push(L[i++].replace(/^>\s?/, ''));
      const t = b.join(' ');
      out.push('<div class="tip' + (/^(⚠|注意|警告)/.test(t) ? ' warn' : '') + '">' + mdInline(t) + '</div>');
      continue;
    }
    if (isItem(ln)) {
      flush();
      const items = [];
      while (i < L.length && isItem(L[i])) items.push(L[i++]);
      out.push(renderList(items));
      continue;
    }
    para.push(ln.trim());
    i++;
  }
  flush();
  return { html: out.join('\n'), toc };
}
