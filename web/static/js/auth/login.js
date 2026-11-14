/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: login.js
 * @time: 2026/10/28
 */
// 登录页小功能：记住账号（只存在本机浏览器里，且只存用户名，不存密码）、显示 / 隐藏密码、“忘记密码”说明弹窗。
// 页面没有脚本时登录表单照常可用，这些增强控件默认是隐藏的，由这里显示。

import { t } from '../i18n.js';

const KEY = 'dbt_last_login';

function load() { try { return localStorage.getItem(KEY) || ''; } catch (_) { return ''; } }
function save(v) { try { if (v) localStorage.setItem(KEY, v); else localStorage.removeItem(KEY); } catch (_) { /* 隐私模式等：不记住即可 */ } }

const form = document.getElementById('login-form');
if (form) {
  const u = document.getElementById('u');
  const p = document.getElementById('p');
  const rem = document.getElementById('rem');

  // 记住账号：有记住的账号就预填并勾选，焦点放到密码框
  const last = load();
  if (last && !u.value) {
    u.value = last;
    rem.checked = true;
    p.focus();
  } else if (!u.value) {
    u.focus();
  }
  form.addEventListener('submit', () => save(rem.checked ? u.value.trim() : ''));

  // 密码显示 / 隐藏
  const eye = document.getElementById('pw-eye');
  if (eye) {
    eye.hidden = false;
    eye.addEventListener('click', () => {
      const show = p.type === 'password';
      p.type = show ? 'text' : 'password';
      eye.textContent = show ? t('隐藏') : t('显示');
      eye.setAttribute('aria-label', show ? t('隐藏密码') : t('显示密码'));
      eye.setAttribute('aria-pressed', String(show));
      p.focus();
    });
  }
}

// 左上角的“元数据库 OK”和右侧时钟：状态读未登录也能访问的 /readyz（状态 fail 或请求失败显示“异常”）；时钟是浏览器本地时间，页面不可见时暂停。
const dbChip = document.getElementById('lgn-db');
const clock = document.getElementById('login-clock');
if (dbChip || clock) {
  const two = (n) => String(n).padStart(2, '0');
  const zh = (document.documentElement.lang || '').startsWith('zh');
  const paintClock = () => {
    if (!clock) return;
    const d = new Date();
    const hms = `${two(d.getHours())}:${two(d.getMinutes())}:${two(d.getSeconds())}`;
    clock.textContent = zh ? `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日 ${hms}` : `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())} ${hms}`; // i18n-ignore：日期写法随界面语言
  };
  const probe = async () => {
    if (!dbChip) return;
    let ok = false;
    try {
      const r = await fetch('/readyz', { cache: 'no-store', credentials: 'omit' });
      ok = r.ok;
    } catch (_) { /* 连不上：按异常显示 */ }
    dbChip.hidden = false;
    dbChip.classList.toggle('bad', !ok);
    document.getElementById('lgn-db-s').textContent = ok ? 'OK' : t('异常');
  };
  let clockTimer = null;
  let probeTimer = null;
  const start = () => {
    paintClock();
    probe();
    if (!clockTimer) clockTimer = setInterval(paintClock, 1000);
    if (!probeTimer) probeTimer = setInterval(probe, 30000);
  };
  const stop = () => { clearInterval(clockTimer); clearInterval(probeTimer); clockTimer = probeTimer = null; };
  document.addEventListener('visibilitychange', () => (document.hidden ? stop() : start()));
  if (!document.hidden) start();
}

// 忘记密码说明
const modal = document.getElementById('m-forgot');
const open = document.getElementById('forgot-open');
if (modal && open) {
  let back = null;
  const close = () => { modal.hidden = true; if (back) back.focus(); };
  open.hidden = false;
  open.addEventListener('click', () => {
    back = open;
    modal.hidden = false;
    document.getElementById('forgot-close').focus();
  });
  document.getElementById('forgot-close').addEventListener('click', close);
  modal.addEventListener('click', (e) => { if (e.target === modal) close(); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !modal.hidden) close(); });
}
