/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: i18n.js
 * @time: 2026/10/28
 */
// 界面多语言（见 internal/i18n）。
//
// 以中文原文为键：t 的第一个参数就是中文原文；带参数用具名占位，如 t 的原文里写 {n}，第二个参数传 { n: rows.length }。
// 中文页不下发任何目录；其它语言在模块加载时取一次 /i18n/<lang>.json（带 ETag，浏览器条件请求），
// 取不到或缺译都回落中文原文。写在 t() 里的必须是字符串字面量（提取与 lint 都靠它），不要拼接。
const root = document.documentElement;

/** 当前界面语言（<html lang>，由服务端按请求语言写入）。 */
export const lang = root.getAttribute('lang') || 'zh-CN';
export const isZh = lang.toLowerCase().startsWith('zh');
/** 给 Intl / toLocaleString 用的区域。 */
export const locale = isZh ? 'zh-CN' : 'en-US';

let msgs = {};
if (!isZh) {
  try {
    const res = await fetch(`/i18n/${encodeURIComponent(lang)}.json`, { credentials: 'same-origin' });
    if (res.ok) msgs = await res.json();
  } catch (_) { /* 目录取不到：全部回落中文原文 */ }
}

const ctxRe = /\|[a-z][a-z0-9_.-]*$/; // “词|上下文”里的上下文后缀，只用来区分同形异义，不展示

/** 翻译一条中文原文；params 代入 {名字} 占位。缺译回落原文。 */
export function t(key, params) {
  let s = Object.prototype.hasOwnProperty.call(msgs, key) ? msgs[key] : key.replace(ctxRe, '');
  if (params) s = s.replace(/\{(\w+)\}/g, (m, k) => (Object.prototype.hasOwnProperty.call(params, k) ? String(params[k]) : m));
  return s;
}

/**
 * 翻译“运行时的字符串”（接口返回的状态名、标签、提示等）：精确匹配目录，没有就原样返回。
 * 和 t 的区别：t 的参数必须是源码里的字面量；tp 的参数是变量，不做“|上下文”处理，也不会改动用户数据。
 */
export function tp(s) {
  if (typeof s !== 'string' || isZh) return s;
  return Object.prototype.hasOwnProperty.call(msgs, s) ? msgs[s] : s;
}

/** 数字按界面语言做千分位等格式化。 */
export function fmtNum(n, opts) {
  const v = Number(n);
  return Number.isFinite(v) ? v.toLocaleString(locale, opts) : '—';
}

/** 日期时间按界面语言格式化；d 为 Date / 时间戳 / ISO 字符串。 */
export function fmtDateTime(d, opts) {
  const v = d instanceof Date ? d : new Date(d);
  return Number.isNaN(v.getTime()) ? '' : v.toLocaleString(locale, opts);
}

const COOKIE = 'dbtool_lang';

/** 切换界面语言：写 cookie 后用 GET 重新载入当前页（不用 reload()，避免把 POST 结果页重新提交）。 */
export function setLang(code) {
  document.cookie = `${COOKIE}=${encodeURIComponent(code)}; path=/; max-age=${365 * 24 * 3600}; samesite=lax`;
  const u = new URL(location.href);
  u.searchParams.delete('lang'); // URL 上的 ?lang= 会压过 cookie，必须去掉
  location.replace(u.href);
}
