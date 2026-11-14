/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: user_drawer.js
 * @time: 2026/10/28
 */
// 用户管理 › 用户详情抽屉：基本信息、最近登录 IP、页内直接单独授权 / 禁用（沿用 /api/v1/settings/button-perms 现有接口）、
// 按钮级有效权限（来源汇总 + 筛选）。仅超级用户。写操作后通过 handlers.changed() 通知列表刷新。
import { api, $, el, button, status, badge, modal, msgBox, confirmRun, toast } from './ui.js';
import { t, tp } from '../i18n.js';

const BP = '/api/v1/settings/button-perms';
const TFA = { totp: 'TOTP', sms: t('短信') };

let handlers = {}; // { edit(u), unlock(u), toggle(u, on), resetPw(u), reset2fa(u), changed() }
let user = null; // 用户详情
let view = null; // 按钮级有效权限
let flt = 'all';
let reg = null; // 按钮登记表（单独授权弹窗用）
let ovrModal = null;
let lastFocus = null;

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

function kv(rows) {
  const dl = el('dl', 'kv');
  for (const [k, v] of rows) {
    dl.append(el('dt', '', k));
    const dd = el('dd');
    if (v instanceof Node) dd.append(v); else dd.textContent = v === '' || v === null || v === undefined ? '—' : String(v);
    dl.append(dd);
  }
  return dl;
}

function groupBadges(list) {
  const box = el('span');
  if (!list.length) return document.createTextNode('—');
  for (const g of list) box.append(badge(g.name), document.createTextNode(' '));
  return box;
}

function statusNode(u) {
  const box = el('span');
  if (!u.is_active) box.append(status(t('已停用'), 'aborted'));
  else if (u.locked) box.append(status(t('已锁定'), 'rejected'), el('small', 'muted', ' ' + t('失败 {n} 次', { n: u.failed_login_count })));
  else box.append(status(t('正常'), 'passed'));
  if (u.is_superuser) box.append(document.createTextNode(' '), badge(t('超级用户'), 'warn'));
  if (u.must_change) box.append(document.createTextNode(' '), badge(t('下次登录须改密'), 'warn'));
  return box;
}

function notifyText(u) {
  const parts = [];
  if (u.has_ding) parts.push(t('钉钉') + ' ✓');
  if (u.has_feishu) parts.push(t('飞书') + ' ✓');
  if (u.has_slack) parts.push('Slack ✓');
  return parts.join(' ');
}

function renderInfo(body) {
  const u = user;
  const last = u.last_login ? (u.last_login_ip ? t('{time}（{ip}）', { time: u.last_login, ip: u.last_login_ip }) : u.last_login) : '';
  const tfa = u.two_fa.length ? u.two_fa.map((k) => TFA[k] || k).join(' + ') : t('未绑定');
  body.append(kv([
    [t('账号'), u.username], [t('姓名 / 部门'), `${u.display}${u.dept ? ' · ' + u.dept : ''}`], [t('邮箱'), u.email],
    [t('认证方式'), t('{auth} · 2FA：{tfa}', { auth: tp(u.auth_label) || t('本地'), tfa })],
    [t('状态'), statusNode(u)], [t('权限组'), groupBadges(u.auth_groups)], [t('资源组'), groupBadges(u.resource_groups)],
    [t('通知标识'), notifyText(u)], [t('最近登录'), last],
  ]));
  const ops = el('div', 'sy-drawer-ops');
  ops.append(button(t('编辑'), '', () => handlers.edit(u)));
  if (u.locked || (u.is_active && u.failed_login_count > 0)) ops.append(button(t('解锁'), u.locked ? 'primary' : '', () => handlers.unlock(u)));
  const rp = button(t('重置密码'), '', () => handlers.resetPw(u));
  if (!u.can_reset) { rp.disabled = true; rp.title = t('{auth} 认证，密码由外部系统管理', { auth: tp(u.auth_label) }); }
  ops.append(rp);
  if (u.two_fa.length) ops.append(button(t('重置 2FA'), '', () => handlers.reset2fa(u)));
  ops.append(u.is_active ? button(t('停用'), 'ghost danger', () => handlers.toggle(u, false)) : button(t('启用'), '', () => handlers.toggle(u, true)));
  const link = el('a', 'btn ghost', t('在权限组页查看'));
  link.href = '/settings/auth-groups#user=' + u.id;
  ops.append(link);
  body.append(ops);
}

function renderOverrides(body) {
  const head = el('div', 'h-row');
  head.append(el('h3', '', t('单独授权 / 禁用')), el('span', 'spacer'));
  const add = button(t('＋ 单独授权 / 禁用…'), '', openOvr);
  add.disabled = !view || view.user.superuser || !view.user.active;
  if (add.disabled) add.title = view && view.user.superuser ? t('超级用户不受按钮权限限制') : t('用户已停用');
  head.append(add);
  body.append(head);
  if (!view || !view.overrides.length) { body.append(el('p', 'small muted', t('暂无单独授权 / 禁用记录。'))); return; }
  const tbl = el('table', 'list');
  const th = el('thead');
  const hr = el('tr');
  for (const c of [t('按钮'), t('方式'), t('到期'), t('原因'), t('设置人'), '']) hr.append(el('th', '', c));
  th.append(hr);
  const tb = el('tbody');
  for (const o of view.overrides) {
    const tr = el('tr', o.expired ? 'muted' : '');
    const eff = el('td');
    eff.append(status(o.effect === 'deny' ? t('单独禁用') : t('单独授权'), o.effect === 'deny' ? 'rejected' : 'passed'));
    const ops = el('td', 'row-actions');
    ops.append(button(t('撤销'), 'ghost danger', () => revoke(o)));
    tr.append(el('td', '', t('{name}（{code}）', { name: tp(o.label), code: o.code })), eff, el('td', 'small', o.expire_at ? fmt(o.expire_at) + (o.expired ? t('（已到期）') : '') : t('长期')),
      el('td', 'small', o.reason), el('td', 'small', `${o.created_by} ${fmt(o.created_at)}`), ops);
    tb.append(tr);
  }
  tbl.append(th, tb);
  body.append(tbl);
}

function renderEffective(body) {
  body.append(el('h3', '', t('有效权限')));
  if (!view) { body.append(el('p', 'small muted', t('无法载入按钮权限。'))); return; }
  const items = view.items;
  const allowed = items.filter((i) => i.allowed);
  const by = (src) => allowed.filter((i) => i.source === src).length;
  const denied = items.filter((i) => i.source === 'deny').length;
  const sum = view.user.superuser
    ? t('共 {total} 个按钮，超级用户拥有全部', { total: items.length })
    : t('共 {total} 个按钮，允许 {allowed}：权限组 {group} · 单独授权 {allow} · 登录即可 {open} · 被单独禁用 {deny}',
      { total: items.length, allowed: allowed.length, group: by('group'), allow: by('allow'), open: by('open'), deny: denied });
  const bar = el('div', 'h-row');
  bar.append(el('span', 'small', sum), el('span', 'spacer'));
  const sel = document.createElement('select');
  sel.setAttribute('aria-label', t('筛选有效权限'));
  for (const [v, label] of [['all', t('全部按钮')], ['allow', t('仅允许')], ['deny', t('仅禁止 / 未授予')], ['ovr', t('仅单独授权 / 禁用')]]) {
    const o = new Option(label, v);
    o.selected = v === flt;
    sel.append(o);
  }
  sel.addEventListener('change', () => { flt = sel.value; fillEffective(); });
  bar.append(sel);
  body.append(bar);
  const wrap = el('div', 'sy-scroll');
  wrap.id = 'ud-eff';
  body.append(wrap);
  fillEffective();
}

function fillEffective() {
  const wrap = $('ud-eff');
  if (!wrap) return;
  const ovr = new Set(view.overrides.filter((o) => !o.expired).map((o) => o.code));
  const rows = view.items.filter((it) => {
    if (flt === 'allow') return it.allowed;
    if (flt === 'deny') return !it.allowed;
    if (flt === 'ovr') return ovr.has(it.code);
    return true;
  });
  const tbl = el('table', 'list');
  const th = el('thead');
  const hr = el('tr');
  for (const c of [t('页面'), t('按钮'), t('结果'), t('来源')]) hr.append(el('th', '', c));
  th.append(hr);
  const tb = el('tbody');
  if (!rows.length) {
    const tr = el('tr');
    const td = el('td', 'empty', t('没有符合条件的按钮'));
    td.colSpan = 4;
    tr.append(td);
    tb.append(tr);
  }
  for (const it of rows) {
    const tr = el('tr', it.source === 'deny' ? 'lv-err' : it.source === 'allow' ? 'lv-warn' : '');
    const res = el('td');
    if (it.source === 'deny') res.append(status(t('禁止'), 'rejected'));
    else res.append(it.allowed ? status(t('允许'), 'passed') : status(it.locked ? t('仅超管') : t('无|perm'), 'aborted'));
    const nm = el('td', '', tp(it.label) + (it.kind === 'd' ? ' ⚠' : ''));
    nm.append(el('div', 'mono small muted', it.code));
    tr.append(el('td', 'small', `${tp(it.module)} / ${tp(it.page)}`), nm, res, el('td', it.source === 'none' || it.source === 'locked' ? 'small muted' : 'small', describe(it)));
    tb.append(tr);
  }
  tbl.append(th, tb);
  wrap.replaceChildren(tbl);
}

function paint() {
  const body = $('ud-body');
  body.replaceChildren();
  $('ud-title').textContent = `${user.username} · ${user.display}`;
  renderInfo(body);
  renderOverrides(body);
  renderEffective(body);
}

// ---- 撤销 / 新增单独授权 ----

async function revoke(o) {
  const u = user;
  const deny = o.effect === 'deny';
  const what = deny ? t('单独禁用') : t('单独授权');
  const r = await confirmRun({
    title: deny ? t('撤销单独禁用') : t('撤销单独授权'), code: 'ag.btnperm', level: 'high',
    lines: [deny ? t('撤销后该用户恢复按权限组 / 其他来源计算此按钮的权限。') : t('撤销后该用户不再通过单独授权拥有此按钮，按权限组等其他来源重新计算。'), t('原因会写入系统审计。')],
    details: [{ k: t('用户'), v: u.username }, { k: t('按钮'), v: t('{name}（{code}）', { name: tp(o.label), code: o.code }) }, { k: t('方式'), v: what }],
    reason: { label: t('原因'), required: true, max: 200, placeholder: t('请说明原因（必填）') },
    okText: t('确认撤销'),
  }, (reason) => api('DELETE', `${BP}/users/${u.id}/overrides`, { codes: [o.code], reason }));
  if (!r.ok) return;
  try {
    await reload();
    handlers.changed();
  } catch (e) { toast(e.message, 'err'); }
}

async function ensureRegistry() {
  if (!reg) reg = await api('GET', BP + '/registry');
  return reg;
}

function fillOvrList() {
  const box = $('uo-list');
  const q = $('uo-q').value.trim().toLowerCase();
  const keep = new Set([...box.querySelectorAll('input:checked')].map((c) => c.value));
  box.replaceChildren();
  for (const m of reg.modules) {
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

async function openOvr() {
  try { await ensureRegistry(); } catch (e) { toast(e.message, 'err'); return; }
  $('uo-user').textContent = t('用户 {name}', { name: user.username });
  $('uo-effect').value = 'allow';
  $('uo-expire').value = '';
  $('uo-q').value = '';
  $('uo-reason').value = '';
  $('uo-msg').replaceChildren();
  fillOvrList();
  ovrModal.open();
}

async function saveOvr() {
  const msg = msgBox($('uo-msg'));
  const codes = [...document.querySelectorAll('#uo-list input:checked')].map((c) => c.value);
  const effect = $('uo-effect').value;
  const reason = $('uo-reason').value.trim();
  if (!codes.length) { msg(t('请选择至少一个按钮')); return; }
  if (!reason) { msg(t('请填写原因（会写入系统审计）')); return; }
  const all = reg.modules.flatMap((m) => m.pages.flatMap((p) => p.buttons));
  const danger = all.filter((b) => codes.includes(b.code) && b.kind === 'd');
  const expire = $('uo-expire').value;
  const u = user;
  const r = await confirmRun(overrideConfirm({ u, effect, codes, all, danger, expire, reason }),
    () => api('PUT', `${BP}/users/${u.id}/overrides`, { effect, codes, expire_at: expire, reason }));
  if (!r.ok) return;
  ovrModal.close();
  try {
    await reload();
    handlers.changed();
  } catch (e) { toast(e.message, 'err'); }
}

// 单独授权 / 禁用的确认弹窗内容：授予高危按钮时标红；原因来自上面已填的输入框，这里只展示
function overrideConfirm({ u, effect, codes, all, danger, expire, reason }) {
  const allow = effect === 'allow';
  const names = codes.map((c) => { const b = all.find((x) => x.code === c); return b ? t('{name}（{code}）', { name: tp(b.label), code: c }) + (b.kind === 'd' ? ' ⚠' : '') : c; });
  const lines = [allow ? t('该用户将直接拥有以下按钮，不受所在权限组限制。') : t('以下按钮将对该用户禁用，禁用优先于权限组与单独授权。')];
  if (allow && danger.length) lines.push(t('其中 {n} 个是高危按钮：{names}。', { n: danger.length, names: danger.map((b) => tp(b.label)).join(t('、|sep')) }));
  return {
    title: allow ? t('单独授权') : t('单独禁用'), code: 'ag.btnperm', level: allow && danger.length ? 'high' : 'normal',
    lines,
    details: [{ k: t('用户'), v: u.username }, { k: t('按钮'), v: names.join('\n') }, { k: t('到期'), v: expire || t('长期') }, { k: t('原因'), v: reason }],
    okText: allow ? t('确认授权') : t('确认禁用'),
  };
}

// ---- 打开 / 关闭 ----

async function reload() {
  const [u, v] = await Promise.all([
    api('GET', `/api/v1/settings/users/${user.id}`),
    api('GET', `${BP}/users/${user.id}`).catch(() => null),
  ]);
  user = u;
  view = v;
  paint();
}

export async function open(id) {
  lastFocus = document.activeElement;
  user = await api('GET', `/api/v1/settings/users/${id}`);
  view = await api('GET', `${BP}/users/${id}`).catch(() => null);
  flt = 'all';
  paint();
  $('us-drawer').hidden = false;
  $('us-drawer-mask').hidden = false;
  $('ud-close').focus();
}

export function close() {
  $('us-drawer').hidden = true;
  $('us-drawer-mask').hidden = true;
  if (lastFocus && lastFocus.focus) lastFocus.focus();
}

// refresh 在列表里的操作（编辑 / 解锁 / 启停）完成后，如果抽屉开着就刷新它。
export async function refresh() {
  if (user && !$('us-drawer').hidden) { try { await reload(); } catch (_) { close(); } }
}

export function init(h) {
  handlers = h;
  ovrModal = modal('m-user-ovr');
  $('ud-close').addEventListener('click', close);
  $('us-drawer-mask').addEventListener('click', close);
  $('us-drawer').addEventListener('keydown', (e) => { if (e.key === 'Escape') close(); });
  $('uo-q').addEventListener('input', fillOvrList);
  $('uo-save').addEventListener('click', saveOvr);
}
