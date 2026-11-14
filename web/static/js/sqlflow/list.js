/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: list.js
 * @time: 2026/10/28
 */
// 工单列表 / 导出列表：下拉筛选一改就提交（搜索框仍回车或点“查询”）。
const form = document.getElementById('sf-filter');
if (form) {
  form.querySelectorAll('select').forEach((sel) => sel.addEventListener('change', () => form.requestSubmit()));
}
