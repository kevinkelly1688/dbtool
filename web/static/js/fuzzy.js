/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: fuzzy.js
 * @time: 2026/10/28
 */
// 实例下拉的模糊匹配（纯函数，searchselect.js 使用；单独成文件便于不带浏览器地测试）。
// 规则：不区分大小写；空格分隔的每个词都要出现在选项文字里（子串）；没有子串命中时退到“按顺序出现的字符”（子序列）。

function norm(t) { return String(t || '').toLowerCase(); }

function subsequence(text, q) {
  let i = 0;
  for (const ch of text) {
    if (ch === q[i]) i++;
    if (i === q.length) return true;
  }
  return q.length === 0;
}

// 返回匹配的下标列表（保持原顺序）；子串优先，没有再退到子序列
export function fuzzyFilter(texts, query) {
  const q = norm(query).trim();
  const idx = texts.map((_, i) => i);
  if (!q) return idx;
  const words = q.split(/\s+/).filter(Boolean);
  const hit = idx.filter((i) => { const t = norm(texts[i]); return words.every((w) => t.includes(w)); });
  if (hit.length) return hit;
  const joined = words.join('');
  return idx.filter((i) => subsequence(norm(texts[i]), joined));
}
