/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: btnperm.js
 * @time: 2026/10/28
 */
// 按钮级权限的前端小工具（给各业务页面用）：取当前用户拥有的按钮，按“无权限的按钮：隐藏 / 置灰”设置处理带 data-btn 的元素。
// 页面里写 <button data-btn="sqlworkflow.approve">审批通过</button>，脚本里 await applyButtons(document)。
// 注意：这只是显示层，真正的授权在服务端（每个接口都按权限码校验）。
import { api } from './common.js';
import { t } from './i18n.js';

let cached = null;

// 返回 {deny, superuser, allowed:Set}；失败时返回 null（页面保持原样，由服务端兜底拒绝）。
export async function loadButtons() {
  if (cached) return cached;
  try {
    const d = await api('GET', '/api/v1/button-perms/me');
    cached = { deny: d.deny_display, superuser: d.superuser, allowed: new Set(d.allowed) };
  } catch (_) {
    cached = null;
  }
  return cached;
}

export const can = (b, code) => !b || b.superuser || b.allowed.has(code);

// 处理 root 下所有带 data-btn（可写多个码，空格分隔，任一即可）的元素。
export async function applyButtons(root = document) {
  const b = await loadButtons();
  if (!b) return;
  for (const e of root.querySelectorAll('[data-btn]')) {
    const ok = e.dataset.btn.split(/\s+/).some((c) => can(b, c));
    if (ok) continue;
    if (b.deny === 'disable') {
      e.disabled = true;
      e.setAttribute('aria-disabled', 'true');
      e.title = t('无权限：需要 {code}', { code: e.dataset.btn });
    } else {
      e.hidden = true;
    }
  }
}
