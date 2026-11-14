/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: bindings.js
 * @time: 2026/10/28
 */
// 账号绑定页：列出已绑定的外部账号，允许解绑；管理员开放“允许用户自行绑定”时可以发起绑定（走 /login/sso/:id?bind=1）。
// 往页面写内容一律用 textContent / createElement，不拼 innerHTML。
import { api, confirmRun } from '../common.js';
import { t, tp } from '../i18n.js';

const $= (id) => document.getElementById(id);

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

function msg(text, kind = 'err') {
  const box = $('bd-msg');
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

function emptyRow(text) {
  const tr = el('tr');
  const td = el('td', 'empty', text);
  td.colSpan = 4;
  tr.append(td);
  return tr;
}

async function unbind(item) {
  const r = await confirmRun({
    title: t('解绑外部账号'), danger: true,
    lines: [t('解绑后将不能再用「{name}」登录这个账号，需要时可以重新绑定（管理员开放自行绑定时）。', { name: tp(item.provider_name) })],
    details: [{ k: t('登录方式'), v: tp(item.provider_name) }, { k: t('类型'), v: tp(item.kind_label) || '—' }, { k: t('绑定时间'), v: item.bound_at }],
    okText: t('确认解绑'),
  }, () => api('DELETE', '/api/v1/account/bindings/' + item.id));
  if (!r.ok) return;
  msg(t('已解绑'), 'info');
  try {
    await load();
  } catch (e) {
    msg(e.message);
  }
}

function render(pg) {
  $('bd-off').hidden = !!pg.allowed;
  const rows = $('bd-rows');
  rows.replaceChildren();
  if (!pg.items.length) rows.append(emptyRow(t('还没有绑定任何外部账号。')));
  for (const it of pg.items) {
    const tr = el('tr');
    const op = el('td', 'row-actions');
    if (pg.allowed) {
      const b = el('button', 'btn ghost danger', t('解绑'));
      b.type = 'button';
      b.addEventListener('click', () => unbind(it));
      op.append(b);
    }
    tr.append(el('td', '', tp(it.provider_name)), el('td', '', tp(it.kind_label) || '—'), el('td', 'small', it.bound_at), op);
    rows.append(tr);
  }
  const box = $('bd-choices');
  box.replaceChildren();
  for (const c of pg.available || []) {
    const a = el('a', 'btn', t('绑定{name}（{kind}）', { name: c.name, kind: tp(c.kind_label) }));
    a.href = c.href;
    box.append(a);
  }
  $('bd-add').hidden = !pg.allowed || !(pg.available || []).length;
}

async function load() {
  render(await api('GET', '/api/v1/account/bindings'));
}

load().catch((e) => {
  msg(t('加载失败：') + e.message);
  $('bd-rows').replaceChildren(emptyRow(t('加载失败')));
});
