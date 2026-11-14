/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: clip.js
 * @time: 2026/10/28
 */
// 复制到剪贴板：优先用异步剪贴板接口；非安全上下文 / 被拒绝时退回临时 textarea + execCommand。返回是否成功。
export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (_) {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('aria-hidden', 'true');
    ta.className = 'cv-offscreen';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch (_e) { /* 忽略 */ }
    ta.remove();
    return ok;
  }
}
