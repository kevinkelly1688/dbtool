/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: btnperm_user.js
 * @time: 2026/10/28
 */
// 权限组管理 › 按用户查看有效权限：按钮级有效权限（含来源）与单独授权 / 禁用（含到期日）。接口见 /api/v1/settings/button-perms/users/*。
import { api, $, el, button, status, modal, msgBox, emptyRow, confirmRun } from './ui.js';
import { registry } from './btnperm.js';
import { t, tp } from '../i18n.js';

const BP = '/api/v1/settings/button-perms';

let view = null;
let setMsg = () => {};
let ovrM = null;

const fmt = (iso) => (iso ? iso.replace('T', ' ').slice(0, 16) : '');

function describe(it) {
  switch (it.source) {
    case 'super': return t('超级用户');
    case 'deny': return t('单独禁用') + (it.expire_at ? t('（到 {time}）', { time: fmt(it.expire_at) }) : '');
    case 'allow': return t('单独授权') + (it.expire_at ? t('（到 {time}）', { time: fmt(it.expire_at) }) : '');
    case 'group': return t('权限组 {name}', { name: (it.groups || []).join(t('、|sep')) });
    case 'open': return t('登录即可');
    case 'locked': return t('仅超级用户');
    case 'inactive': return t('用户已停用');
    default: return t('未授予');
  }
}

function render() {
  const body = $('bu-rows');
  const ob = $('bu-ovr');
  if (!view) {
    emptyRow(body, 5, t('请先选择用户'));
    emptyRow(ob, 6, t('请先选择用户'));
    $('bu-info').textContent = '';
    $('bu-sum').textContent = '';
    $('bu-open').disabled = true;
    return;
  }
  const q = $('bu-q').value.trim().toLowerCase();
  const flt = $('bu-flt').value; // all / allow / deny / ovr
  const has = new Set(view.overrides.filter((o) => !o.expired).map((o) => o.code));
  const pass = (it) => (flt === 'allow' ? it.allowed : flt === 'deny' ? !it.allowed : flt === 'ovr' ? has.has(it.code) : true);
  const rows = view.items.filter((it) => pass(it) && (q === '' || `${tp(it.page)} ${tp(it.label)} ${it.code}`.toLowerCase().includes(q)));
  // 来源汇总
  const allowed = view.items.filter((it) => it.allowed);
  const by = (src) => allowed.filter((it) => it.source === src).length;
  $('bu-sum').textContent = view.user.superuser
    ? t('共 {total} 个按钮，超级用户拥有全部', { total: view.items.length })
    : t('共 {total} 个按钮，允许 {allowed}：权限组 {group} · 单独授权 {allow} · 登录即可 {open} · 被单独禁用 {deny}',
      { total: view.items.length, allowed: allowed.length, group: by('group'), allow: by('allow'), open: by('open'), deny: view.items.filter((it) => it.source === 'deny').length });
  $('bu-info').textContent = `${view.user.username}${view.user.superuser ? ' · ' + t('超级用户（全部放行）') : ''}${view.user.active ? '' : ' · ' + t('已停用')}`;
  $('bu-open').disabled = view.user.superuser || !view.user.active;
  if (!rows.length) emptyRow(body, 5, t('没有符合条件的按钮'));
  else {
    body.replaceChildren();
    for (const it of rows) {
      const tr = el('tr', it.source === 'deny' ? 'lv-err' : it.source === 'allow' ? 'lv-warn' : '');
      const res = el('td');
      if (it.source === 'deny') res.append(status(t('禁止'), 'rejected'));
      else res.append(it.allowed ? status(t('允许'), 'passed') : status(it.locked ? t('仅超管') : t('无|perm'), 'aborted'));
      tr.append(el('td', '', `${tp(it.module)} / ${tp(it.page)}`), el('td', '', tp(it.label) + (it.kind === 'd' ? ' ⚠' : '')), el('td', 'mono small', it.code), res,
        el('td', it.source === 'none' || it.source === 'locked' ? 'muted' : '', describe(it)));
      body.append(tr);
    }
  }
  if (!view.overrides.length) emptyRow(ob, 6, t('没有单独授权 / 禁用'));
  else {
    ob.replaceChildren();
    for (const o of view.overrides) {
      const tr = el('tr', o.expired ? 'muted' : o.effect === 'deny' ? 'lv-err' : 'lv-warn');
      const eff = el('td');
      eff.append(status(o.effect === 'deny' ? t('单独禁用') : t('单独授权'), o.effect === 'deny' ? 'rejected' : 'passed'));
      const ops = el('td', 'row-actions');
      ops.append(button(t('撤销'), 'ghost danger', () => revoke(o)));
      tr.append(el('td', '', t('{name}（{code}）', { name: tp(o.label), code: o.code })), eff, el('td', '', o.expire_at ? fmt(o.expire_at) + (o.expired ? t('（已到期）') : '') : t('长期')),
        el('td', '', o.reason), el('td', 'small', `${o.created_by} ${fmt(o.created_at)}`), ops);
      ob.append(tr);
    }
  }
}

export async function load(userId) {
  if (!userId) { view = null; render(); return; }
  view = await api('GET', `${BP}/users/${userId}`);
  render();
}

async function revoke(o) {
  const reason = $('au-reason').value.trim();
  if (!reason) { setMsg(t('请先在上方填写原因（会写入系统审计）')); $('au-reason').focus(); return; }
  const u = view.user;
  const deny = o.effect === 'deny';
  const what = deny ? t('单独禁用') : t('单独授权');
  const r = await confirmRun({
    title: deny ? t('撤销单独禁用') : t('撤销单独授权'), code: 'ag.btnperm', level: 'high',
    lines: [t('撤销后该用户按权限组等其他来源重新计算此按钮的权限。'), t('原因会写入系统审计。')],
    details: [{ k: t('用户'), v: u.username }, { k: t('按钮'), v: t('{name}（{code}）', { name: tp(o.label), code: o.code }) }, { k: t('方式'), v: what }, { k: t('原因'), v: reason }],
    okText: t('确认撤销'),
  }, () => api('DELETE', `${BP}/users/${u.id}/overrides`, { codes: [o.code], reason }));
  if (!r.ok) return;
  setMsg('');
  try {
    await load(u.id);
    setMsg(t('已撤销，并写入系统审计'), 'info');
  } catch (e) { setMsg(e.message); }
}

// ---- 弹窗 ----

function fillList() {
  const box = $('up-list');
  const q = $('up-q').value.trim().toLowerCase();
  const keep = new Set([...box.querySelectorAll('input:checked')].map((c) => c.value));
  box.replaceChildren();
  for (const m of registry().modules) {
    for (const p of m.pages) {
      const bs = p.buttons.filter((b) => !b.locked && (q === '' || `${tp(p.name)} ${tp(b.label)} ${b.code}`.toLowerCase().includes(q)));
      if (!bs.length) continue;
      box.append(el('div', 'grp', `${tp(m.name)} / ${tp(p.name)}`));
      for (const b of bs) {
        const l = el('label', 'check');
        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.value = b.code;
        cb.checked = keep.has(b.code);
        l.append(cb, ' ', tp(b.label) + (b.kind === 'd' ? ' ⚠' : ''), ' ', el('small', 'mono muted', b.code));
        box.append(l);
      }
    }
  }
}

function openModal() {
  $('up-user').textContent = t('用户 {name}', { name: view.user.username });
  $('up-effect').value = 'allow';
  $('up-expire').value = '';
  $('up-q').value = '';
  $('up-reason').value = $('au-reason').value;
  $('up-list').replaceChildren();
  fillList();
  $('up-msg').replaceChildren();
  ovrM.open();
}

async function saveModal() {
  const msg = msgBox($('up-msg'));
  const codes = [...document.querySelectorAll('#up-list input:checked')].map((c) => c.value);
  const effect = $('up-effect').value;
  const reason = $('up-reason').value.trim();
  if (!codes.length) { msg(t('请选择至少一个按钮')); return; }
  if (!reason) { msg(t('请填写原因（会写入系统审计）')); return; }
  const all = registry().modules.flatMap((m) => m.pages.flatMap((p) => p.buttons));
  const danger = all.filter((b) => codes.includes(b.code) && b.kind === 'd');
  const u = view.user;
  const expire = $('up-expire').value;
  const allow = effect === 'allow';
  const names = codes.map((c) => { const b = all.find((x) => x.code === c); return b ? t('{name}（{code}）', { name: tp(b.label), code: c }) + (b.kind === 'd' ? ' ⚠' : '') : c; });
  const lines = [allow ? t('该用户将直接拥有以下按钮，不受所在权限组限制。') : t('以下按钮将对该用户禁用，禁用优先于权限组与单独授权。')];
  if (allow && danger.length) lines.push(t('其中 {n} 个是高危按钮：{names}。', { n: danger.length, names: danger.map((b) => tp(b.label)).join(t('、|sep')) }));
  const r = await confirmRun({
    title: allow ? t('单独授权') : t('单独禁用'), code: 'ag.btnperm', level: allow && danger.length ? 'high' : 'normal', lines,
    details: [{ k: t('用户'), v: u.username }, { k: t('按钮'), v: names.join('\n') }, { k: t('到期'), v: expire || t('长期') }, { k: t('原因'), v: reason }],
    okText: allow ? t('确认授权') : t('确认禁用'),
  }, () => api('PUT', `${BP}/users/${u.id}/overrides`, { effect, codes, expire_at: expire, reason }));
  if (!r.ok) return;
  ovrM.close();
  try {
    await load(u.id);
    setMsg(effect === 'deny' ? t('已单独禁用，并写入系统审计') : t('已单独授权，并写入系统审计'), 'info');
  } catch (e) { setMsg(e.message); }
}

export function init(msgFn) {
  setMsg = msgFn;
  ovrM = modal('m-user-perm');
  $('bu-open').addEventListener('click', openModal);
  $('bu-q').addEventListener('input', render);
  $('bu-flt').addEventListener('change', render);
  $('up-q').addEventListener('input', fillList);
  $('up-save').addEventListener('click', saveModal);
}
