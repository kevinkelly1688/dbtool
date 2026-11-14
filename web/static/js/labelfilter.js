/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: labelfilter.js
 * @time: 2026/10/28
 */
// 复选框列表的“输入即筛选”：按关键字模糊匹配（不区分大小写；空格分隔的每个词都要出现，没有命中时退到按顺序出现的字符），
// 不匹配的项隐藏，没有匹配项的分组标题（.grp）一起隐藏。已勾选的项不会因为被筛掉而取消勾选。
// 立即巡检弹窗、SQL 上线的“MSSQL 批量上线”实例列表共用。
import { fuzzyFilter } from './fuzzy.js';

// list：里面是 <b class="grp"> 分组标题（可没有）和 <label><input type=checkbox>名称</label>。返回 { shown, total }。
export function filterLabels(list, query) {
  const labels = [...list.querySelectorAll('label')];
  const hit = new Set(fuzzyFilter(labels.map((l) => l.textContent), query));
  labels.forEach((l, i) => { l.hidden = !hit.has(i); });
  let head = null;
  let any = false;
  const close = () => { if (head) head.hidden = !any; };
  for (const n of list.children) {
    if (n.classList.contains('grp')) {
      close();
      head = n;
      any = false;
    } else if (n.tagName === 'LABEL' && !n.hidden) {
      any = true;
    }
  }
  close();
  return { shown: hit.size, total: labels.length };
}
