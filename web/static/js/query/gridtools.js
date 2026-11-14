/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: gridtools.js
 * @time: 2026/10/28
 */
// 结果表格的交互：悬停单元格出“复制 / 预览”、双击预览、方向键移动 + Enter 预览 + Ctrl+C 复制当前格、
// 列头“复制整列”菜单（列值 / 带列名 / IN 列表）。
// 结果可能有几千行，所以只用一个悬浮工具条 + 事件委托，不给每个单元格挂监听；单元格内容一律走 textContent / ACE setValue。
import { t } from '../i18n.js';
import { copyText } from './clip.js';
import { toast } from '../confirm.js';
import { cellText, columnValues, columnWithName, inList, previewKind } from './cells.js';
import { openPreview } from './preview.js';
import { aceMode, isRedis, isMongo } from './sqlutil.js';
import { openMenu, closeMenu } from './ctxmenu.js';
import { el, iconButton } from './ui.js';

// wrap: .grid-wrap；table: table.grid（已含表头与数据行）；res: 查询结果；opts.dbType: 实例类型（引号规则 / 高亮模式）。
export function attachGridTools(wrap, table, res, opts = {}) {
  closeMenu(); // 重新渲染结果时关掉上一个结果的列菜单
  const dbType = opts.dbType || '';
  const tbody = table.tBodies[0];
  const nCols = res.columns.length;
  const inListOK = !isRedis(dbType) && !isMongo(dbType);

  wrap.tabIndex = 0;
  wrap.setAttribute('role', 'region');
  wrap.setAttribute('aria-label', t('查询结果表格'));

  // ---- 悬浮工具条 ----
  const tools = el('div', 'cell-tools');
  tools.hidden = true;
  const bCopy = iconButton('cell-btn', t('复制单元格内容'), 'copy');
  const bView = iconButton('cell-btn', t('预览完整内容'), 'eye');
  tools.append(bCopy, bView);
  wrap.append(tools);

  const isData = (td) => !!td && td.tagName === 'TD' && !td.classList.contains('rn') && td.parentNode.parentNode === tbody;
  const rowOf = (td) => td.parentNode.sectionRowIndex;
  const colOf = (td) => td.cellIndex - 1;
  const valueOf = (td) => res.rows[rowOf(td)][colOf(td)];

  let at = null; // 工具条当前对着的单元格
  let active = null; // 键盘 / 点击选中的单元格
  let flashTimer = 0;

  function place(td) {
    at = td;
    tools.hidden = false;
    const w = wrap.getBoundingClientRect();
    const c = td.getBoundingClientRect();
    const bw = tools.offsetWidth;
    const bh = tools.offsetHeight;
    const visRight = wrap.scrollLeft + wrap.clientWidth;
    const cellLeft = c.left - w.left + wrap.scrollLeft;
    let left = Math.max(c.right - w.left + wrap.scrollLeft - bw - 2, cellLeft + 2); // 贴着单元格右缘；单元格比工具条窄时从左缘开始
    left = Math.min(left, visRight - bw - 2); // 单元格右半截在可视区外时，工具条留在可视区里
    tools.style.left = Math.max(left, wrap.scrollLeft + 2) + 'px';
    tools.style.top = Math.round(c.top - w.top + wrap.scrollTop + (c.height - bh) / 2) + 'px';
  }
  function hide() {
    tools.hidden = true;
    at = null;
  }
  function setActive(td) {
    if (active === td) return;
    if (active) active.classList.remove('cell-on');
    active = td;
    if (td) td.classList.add('cell-on');
  }
  function flash(btn, ok) {
    clearTimeout(flashTimer);
    bCopy.classList.remove('done', 'fail');
    btn.classList.add(ok ? 'done' : 'fail');
    flashTimer = setTimeout(() => btn.classList.remove('done', 'fail'), 1200);
  }

  async function copyCell(td) {
    const ok = await copyText(cellText(valueOf(td)));
    flash(bCopy, ok);
    if (!ok) toast(t('复制失败，请选中文字后按 Ctrl+C'), 'err');
  }
  function previewCell(td) {
    const r = rowOf(td);
    const c = colOf(td);
    const v = res.rows[r][c];
    const text = cellText(v);
    const type = res.column_types && res.column_types[c];
    openPreview({
      title: String(res.columns[c]),
      sub: t('第 {n} 行', { n: r + 1 }) + (type ? ' · ' + type : ''),
      text,
      isNull: v === null || v === undefined,
      mode: previewKind(text) === 'sql' ? aceMode(dbType) : 'ace/mode/text',
    });
  }

  // ---- 鼠标 ----
  wrap.addEventListener('mouseover', (e) => {
    const c = e.target.closest ? e.target.closest('td, th') : null;
    if (!c) return;
    if (isData(c)) place(c); else hide();
  });
  wrap.addEventListener('mouseleave', hide);
  wrap.addEventListener('click', (e) => {
    const td = e.target.closest ? e.target.closest('td') : null;
    if (!isData(td)) return;
    setActive(td);
    place(td); // 触屏没有悬停：点一下也出工具条
  });
  wrap.addEventListener('dblclick', (e) => {
    const td = e.target.closest ? e.target.closest('td') : null;
    if (!isData(td)) return;
    const s = window.getSelection();
    if (s) s.removeAllRanges(); // 双击会选中一个词，预览打开前清掉
    setActive(td);
    previewCell(td);
  });
  bCopy.addEventListener('click', () => { if (at) copyCell(at); });
  bView.addEventListener('click', () => { if (at) previewCell(at); });
  let raf = 0;
  wrap.addEventListener('scroll', () => {
    if (tools.hidden || !at || raf) return;
    raf = requestAnimationFrame(() => { raf = 0; if (at) place(at); });
  }, { passive: true });

  // ---- 键盘（焦点在表格容器上）----
  wrap.addEventListener('keydown', (e) => {
    if (e.target !== wrap) return;
    const k = e.key;
    if ((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && (k === 'c' || k === 'C')) {
      const s = window.getSelection();
      if (active && !(s && !s.isCollapsed && s.toString() !== '')) { // 有手选的文字时按浏览器默认复制
        e.preventDefault();
        place(active);
        copyCell(active);
      }
      return;
    }
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    const move = { ArrowLeft: [0, -1], ArrowRight: [0, 1], ArrowUp: [-1, 0], ArrowDown: [1, 0] }[k];
    if (move || k === 'Home' || k === 'End') {
      e.preventDefault();
      let r = active ? rowOf(active) : 0;
      let c = active ? colOf(active) : 0;
      if (active && move) { r += move[0]; c += move[1]; }
      if (k === 'Home') c = 0;
      if (k === 'End') c = nCols - 1;
      r = Math.min(Math.max(r, 0), tbody.rows.length - 1);
      c = Math.min(Math.max(c, 0), nCols - 1);
      const td = tbody.rows[r].cells[c + 1];
      setActive(td);
      td.scrollIntoView({ block: 'nearest', inline: 'nearest' });
      place(td);
    } else if (k === 'Enter' && active) {
      e.preventDefault();
      previewCell(active);
    } else if (k === 'Escape' && active) {
      setActive(null);
      hide();
    }
  });

  // ---- 列头：复制整列 ----
  const headCells = table.tHead.rows[0].cells;
  for (let i = 1; i < headCells.length; i++) {
    const name = String(res.columns[i - 1]);
    const b = iconButton('col-copy', t('复制「{name}」这一列', { name }), 'copy');
    b.setAttribute('aria-haspopup', 'menu');
    b.setAttribute('aria-expanded', 'false');
    headCells[i].firstChild.after(b); // 放在列名后面、类型行（块级）前面，不另起一行
  }
  table.tHead.addEventListener('click', (e) => {
    const b = e.target.closest ? e.target.closest('.col-copy') : null;
    if (!b) return;
    e.stopPropagation();
    if (b.getAttribute('aria-expanded') === 'true') { closeMenu(); return; }
    openColMenu(b, b.parentNode.cellIndex - 1);
  });

  function openColMenu(btn, k) {
    const rows = res.rows;
    const name = String(res.columns[k]);
    // 点选后先关菜单再执行：复制结果用全局提示告知
    const pick = (build) => async () => {
      const r = build();
      if (r.empty) { toast(r.msg, 'warn'); return; }
      const ok = await copyText(r.text);
      toast(ok ? r.msg : t('复制失败，请检查浏览器的剪贴板权限'), ok ? 'ok' : 'err');
    };
    const items = [
      { label: t('复制列值'), run: pick(() => ({ text: columnValues(rows, k), msg: t('已复制「{name}」{n} 行', { name, n: rows.length }) })) },
      { label: t('复制列名和列值'), run: pick(() => ({ text: columnWithName(name, rows, k), msg: t('已复制「{name}」和 {n} 行值', { name, n: rows.length }) })) },
    ];
    if (inListOK) {
      items.push({
        label: t('复制为 IN 列表'),
        run: pick(() => {
          const r = inList(rows, k, dbType);
          if (!r.count) return { empty: true, msg: t('「{name}」这一列没有可用的值（全是 NULL）', { name }) };
          return { text: r.text, msg: r.skipped ? t('已复制 {n} 个值（跳过 {skipped} 个 NULL）', { n: r.count, skipped: r.skipped }) : t('已复制 {n} 个值', { n: r.count }) };
        }),
      });
    }
    btn.setAttribute('aria-expanded', 'true');
    openMenu({ el: btn }, items, {
      label: t('复制「{name}」这一列', { name }),
      scrollRoots: [wrap],
      ignore: btn, // 再点一次按钮 = 收起（由点击处理函数完成）
      onClose: () => btn.setAttribute('aria-expanded', 'false'),
    });
  }
}
