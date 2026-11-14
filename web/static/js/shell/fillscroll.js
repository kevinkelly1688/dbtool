/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: fillscroll.js
 * @time: 2026/10/28
 */
// 列表页撑满窗口（.page.fill-list）以后，表格是在卡片里的 .grid-wrap 里滚动的：翻页、改筛选条件、点表头排序以后让表格回到顶部，
// 免得新一页从上一页滚到的位置开始看。自动刷新等不是用户操作引起的重画不会触发，滚动位置照旧；
// 表格里面的勾选框 / 输入框 / 按钮、弹窗和抽屉里的操作也不会触发。
function backToTop(from) {
  const page = from.closest && from.closest('.page.fill-list');
  if (!page) return;
  for (const g of page.querySelectorAll(':scope > .card.fill .grid-wrap')) g.scrollTop = 0;
}

// 表格里面、弹窗 / 抽屉里面的元素引起的事件不处理
const ignored = (el) => !!(el.closest && el.closest('.grid-wrap, .modal-mask, .modal, [role="dialog"]'));

// 翻页（页码条里的按钮 / 链接）、点表头排序
document.addEventListener('click', (e) => {
  if (e.target.closest && e.target.closest('.pager button, .pager a, .pm-pager button, .sort') && !(e.target.closest('.modal-mask, .modal, [role="dialog"]'))) backToTop(e.target);
}, true);
// 改筛选条件：下拉框、复选框、搜索框、筛选表单提交
for (const type of ['change', 'input', 'submit']) {
  document.addEventListener(type, (e) => { if (!ignored(e.target)) backToTop(e.target); }, true);
}
