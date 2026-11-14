/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: password.js
 * @time: 2026/10/28
 */
// 自助修改密码页：页面脚本可用时走 JSON 接口（失败不刷新页面，输入保留），否则表单直接提交到 /account/password。
// 往页面写内容一律用 textContent / createElement，不拼 innerHTML。
import { api } from '../common.js';
import { t } from '../i18n.js';

const $ = (id) => document.getElementById(id);
const form = $('pw-form');

function msg(text, kind) {
  const box = $('pw-msg');
  box.replaceChildren();
  if (!text) return;
  const d = document.createElement('div');
  d.className = 'alert ' + kind;
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  d.textContent = text;
  box.append(d);
}

// 提交前的本地检查：只做不涉及密码策略细节的（策略以服务端为准）
function localCheck(cur, nw, cfm) {
  if (!cur || !nw || !cfm) return t('请填写当前密码、新密码和确认新密码');
  if (nw !== cfm) return t('两次输入的新密码不一致');
  if (nw === cur) return t('新密码不能与当前密码相同');
  return '';
}

if (form) {
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const cur = $('pw-cur').value;
    const nw = $('pw-new').value;
    const cfm = $('pw-cfm').value;
    const bad = localCheck(cur, nw, cfm);
    if (bad) { msg(bad, 'err'); return; }
    const btn = $('pw-submit');
    btn.disabled = true;
    btn.setAttribute('aria-busy', 'true');
    try {
      await api('POST', '/api/v1/account/password', { current: cur, new: nw, confirm: cfm });
      form.reset();
      msg(t('密码已修改，正在返回首页…'), 'info');
      setTimeout(() => { location.href = '/'; }, 1200);
    } catch (err) {
      msg(err.message, 'err');
      $('pw-cur').value = '';
      btn.disabled = false;
      btn.removeAttribute('aria-busy');
    }
  });
}
