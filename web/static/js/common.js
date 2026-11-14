/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: common.js
 * @time: 2026/10/28
 */
// 公共：CSRF、请求封装、皮肤与语言切换。
import { t, lang, setLang } from './i18n.js';

// 多语言入口：页面从这里或直接从 ./i18n.js 引入 t 均可。
export { t, tp, lang, isZh, locale, fmtNum, fmtDateTime } from './i18n.js';

export function csrfToken() {
  const m = document.querySelector('meta[name="csrf-token"]');
  return m ? m.content : '';
}

// 请求 JSON 接口。成功返回 data；失败抛出带 code/message/status 的 Error。
export async function api(method, url, body, opts = {}) {
  const headers = { Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET') headers['X-CSRF-Token'] = csrfToken();
  let res;
  try {
    res = await fetch(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal: opts.signal, credentials: 'same-origin' });
  } catch (e) {
    if (e.name === 'AbortError') throw e;
    const err = new Error(t('无法连接服务器，请检查网络后重试'));
    err.code = 'network';
    throw err;
  }
  let payload = null;
  try { payload = await res.json(); } catch (_) { /* 非 JSON 响应 */ }
  if (!res.ok) {
    const err = new Error((payload && payload.message) || t('请求失败（HTTP {status}）', { status: res.status }));
    err.code = (payload && payload.code) || 'http_' + res.status;
    err.status = res.status;
    err.data = payload && payload.data;
    throw err;
  }
  return payload ? payload.data : null;
}

// 请求服务端渲染的 HTML 片段。片段内容已由服务端转义，可安全写入 innerHTML。
export async function fragment(url, opts = {}) {
  const res = await fetch(url, { credentials: 'same-origin', signal: opts.signal });
  if (res.status === 401) return { ok: false, status: 401, html: `<div class="alert err">${t('登录已过期，请{link}', { link: `<a href="/login">${t('重新登录')}</a>` })}</div>` };
  return { ok: res.ok, status: res.status, html: await res.text() };
}

// ---- 皮肤 ----
const SKINS = ['dark', 'light', 'sepia'];

function applySkin(skin) {
  if (!SKINS.includes(skin)) skin = 'dark';
  document.documentElement.setAttribute('data-skin', skin);
  document.cookie = 'dbtool_skin=' + skin + '; path=/; max-age=' + 365 * 24 * 3600 + '; samesite=lax';
  document.querySelectorAll('[data-skin-set]').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.skinSet === skin)));
  document.dispatchEvent(new CustomEvent('skinchange', { detail: skin }));
}

document.querySelectorAll('[data-skin-set]').forEach((b) => b.addEventListener('click', () => applySkin(b.dataset.skinSet)));
document.querySelectorAll('[data-skin-set]').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.skinSet === document.documentElement.getAttribute('data-skin'))));

// ---- 界面语言 ----
document.querySelectorAll('[data-lang-set]').forEach((b) => {
  b.setAttribute('aria-pressed', String(b.dataset.langSet === lang));
  b.addEventListener('click', () => { if (b.dataset.langSet !== lang) setLang(b.dataset.langSet); });
});

// ---- 侧栏：手风琴分组；窄屏时侧栏是抽屉，☰ 打开，点遮罩 / 选菜单 / Esc 关闭 ----
// 同一时间只展开一个分组：默认只展开当前页所在分组（首页这类不属于任何分组的页面全部折叠）；
// 点某个分组标题 = 展开它并折叠其它分组，再点一次折叠它。不再记在浏览器里：刷新或换页后回到“当前页所在分组”。
// 服务端渲染时非当前分组已带 fold 类，这里只负责交互和补齐 aria-expanded（避免刷新时先全展开再收起的闪动）。
const NAV_KEY = 'dbt_nav_state'; // 旧版本记的展开 / 折叠状态：不再使用，清掉

function initSidebar() {
  const nav = document.getElementById('nav');
  if (!nav) return;
  try { localStorage.removeItem(NAV_KEY); } catch (_) { /* 隐私模式等：忽略 */ }
  const groups = [...nav.querySelectorAll('.ng')];
  const setOpen = (g, open) => {
    g.classList.toggle('fold', !open);
    g.querySelector('.ng-btn').setAttribute('aria-expanded', String(open));
  };
  groups.forEach((g) => setOpen(g, g.classList.contains('on')));
  nav.addEventListener('click', (e) => {
    const btn = e.target.closest('.ng-btn');
    if (!btn) return;
    const g = btn.parentNode;
    const open = g.classList.contains('fold'); // 现在是折叠的 → 展开它
    groups.forEach((x) => setOpen(x, x === g ? open : false));
  });
  const toggle = (open) => document.body.classList.toggle('side-open', open);
  const mt = document.getElementById('menu-toggle');
  if (mt) mt.addEventListener('click', () => toggle(!document.body.classList.contains('side-open')));
  const mask = document.getElementById('side-mask');
  if (mask) mask.addEventListener('click', () => toggle(false));
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') toggle(false); });
}
initSidebar();

// 统一确认组件与提示：页面从这里或直接从 ./confirm.js 引入均可。
export { riskConfirm, diffConfirm, confirmRun, closeConfirm, toast, setButtonMeta } from './confirm.js';

// 实例下拉支持“输入即筛选”（模糊匹配定位）；识别规则见 searchselect.js
import './searchselect.js';
