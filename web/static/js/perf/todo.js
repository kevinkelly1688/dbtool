/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: todo.js
 * @time: 2026/10/28
 */
// 侧栏“性能工单”分组标题上的待办角标：我的待办数，有超时则变橙。
// 取数接口很轻（两个计数）：GET /api/v1/perf/my-todo。没有权限（接口返回 403）或失败时静默不显示。
// 由 layout.html 在所有页面引入（有性能工单入口的用户）。
import { api } from '../common.js';
import { t } from '../i18n.js';

async function refresh() {
  const btn = document.querySelector('#nav .ng[data-group="perf"] .ng-btn');
  if (!btn) return;
  let todo;
  try { todo = await api('GET', '/api/v1/perf/my-todo'); } catch (_) { return; }
  let b = btn.querySelector('.perf-todo');
  if (!todo || !todo.todo) { if (b) b.remove(); return; }
  if (!b) {
    b = document.createElement('span');
    b.className = 'badge perf-todo';
    btn.insertBefore(b, btn.querySelector('.caret'));   // 角标靠右、在折叠箭头左边
  }
  b.classList.toggle('warn', todo.overdue > 0); // 有超时 → 橙色
  b.textContent = String(todo.todo);
  b.title = todo.overdue > 0 ? t('我的待办 {todo} 个，其中 {overdue} 个已超时', { todo: todo.todo, overdue: todo.overdue }) : t('我的待办 {todo} 个', { todo: todo.todo });
}

refresh();
