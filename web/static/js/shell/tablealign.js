/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: tablealign.js
 * @time: 2026/10/28
 */
// 表头与列内容对齐：表头默认左对齐，而数字列（td.num 等）的内容是右对齐的，标题和数据就会错开。
// 这里按每一列数据格实际的对齐方式，给对应的 th 加 ta-r（右）/ ta-c（居中），样式在 app.css 末尾。
// 页面里用 fetch 后 replaceChildren / innerHTML 渲染的表，由 MutationObserver 在渲染后重新对齐。
const SAMPLE_ROWS = 30; // 每张表最多取前 30 行数据投票，大结果集不会拖慢页面
const CLS = { right: 'ta-r', center: 'ta-c' };

function norm(a) {
  if (a === 'right' || a === 'end') return 'right';
  if (a === 'center') return 'center';
  return 'left';
}

// 表头行：有 thead 时取 thead 的唯一一行；没有 thead 的表（脚本里 table.append(tr) 直接拼的）取全是 th 的第一行。
// 多行表头（rowspan / colspan 组合）不处理。
function headRow(table) {
  if (table.tHead) return table.tHead.rows.length === 1 ? table.tHead.rows[0] : null;
  const first = table.rows[0];
  return first && first.cells.length && [...first.cells].every((c) => c.tagName === 'TH') ? first : null;
}

function align(table) {
  const head = headRow(table);
  if (!head) return;
  const ths = [...head.cells];
  const cols = ths.reduce((n, th) => n + th.colSpan, 0);
  if (!cols) return;
  const votes = Array.from({ length: cols }, () => ({ left: 0, right: 0, center: 0 }));
  let used = 0;
  for (const row of table.rows) {
    if (used >= SAMPLE_ROWS) break;
    if (row === head || row.parentNode.tagName === 'THEAD' || row.parentNode.tagName === 'TFOOT') continue;
    // 只取每格都是单列的数据行：空状态行（colspan）、展开的 SQL 详情行等不参与投票
    if (row.cells.length !== cols || [...row.cells].some((td) => td.colSpan !== 1)) continue;
    used++;
    for (let i = 0; i < cols; i++) votes[i][norm(getComputedStyle(row.cells[i]).textAlign)]++;
  }
  if (!used) return;
  let col = 0;
  for (const th of ths) {
    const i = col;
    col += th.colSpan;
    if (th.colSpan !== 1) continue;
    const v = votes[i];
    const a = v.right > v.left && v.right >= v.center ? 'right' : v.center > v.left ? 'center' : 'left';
    th.classList.toggle(CLS.right, a === 'right');
    th.classList.toggle(CLS.center, a === 'center');
  }
}

const pending = new Set();
let raf = 0;
function schedule(table) {
  pending.add(table);
  if (!raf) raf = requestAnimationFrame(() => {
    raf = 0;
    const list = [...pending];
    pending.clear();
    list.forEach(align);
  });
}

function tablesIn(node) {
  if (node.nodeType !== 1) return [];
  const out = [...node.querySelectorAll('table')];
  const own = node.closest('table'); // 节点本身是 table，或是已在某张表里的 tbody / tr
  if (own) out.push(own);
  return out;
}

new MutationObserver((records) => {
  for (const r of records) {
    const own = r.target.nodeType === 1 ? r.target.closest('table') : null;
    if (own) schedule(own);
    for (const n of r.addedNodes) tablesIn(n).forEach(schedule);
  }
}).observe(document.body, { childList: true, subtree: true });

document.querySelectorAll('table').forEach(schedule);
