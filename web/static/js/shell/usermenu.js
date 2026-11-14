/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: usermenu.js
 * @time: 2026/10/28
 */
// 顶栏个人菜单（头像下拉）+「个人信息与权限」抽屉。
// 菜单里的“修改密码 / 外部账号绑定 / 两步验证状态”由 GET /api/v1/me/profile（概要）决定显示与否；
// 抽屉打开时再取 ?full=1（权限组、资源组、可见实例数、按钮权限按页面分组）。
// 所有文本都用 textContent 写入，不拼 HTML。
import { api } from '../common.js';
import { t, tp } from '../i18n.js';

const menu = document.getElementById('umenu');
if (menu) init(menu);

function el(tag, props, ...kids) {
  const e = document.createElement(tag);
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v === undefined || v === null || v === false) continue;
      if (k === 'class') e.className = v;
      else if (k === 'text') e.textContent = v;
      else e.setAttribute(k, v === true ? '' : String(v));
    }
  }
  for (const kid of kids.flat()) {
    if (kid === null || kid === undefined || kid === false) continue;
    e.appendChild(typeof kid === 'string' ? document.createTextNode(kid) : kid);
  }
  return e;
}

function init(menu) {
  const btn = document.getElementById('um-btn');
  const pop = document.getElementById('um-pop');
  let brief = null; // 概要（Promise）

  function loadBrief() {
    if (!brief) {
      brief = api('GET', '/api/v1/me/profile').then((p) => { fillMenu(p); return p; }).catch(() => { brief = null; return null; });
    }
    return brief;
  }

  function fillMenu(p) {
    document.getElementById('um-pwd').hidden = !p.can_change_password;
    document.getElementById('um-bind').hidden = !p.can_bind;
    const row = document.getElementById('um-2fa');
    const st = document.getElementById('um-2fa-state');
    if (p.two_fa) {
      row.hidden = false;
      st.textContent = p.two_fa === 'enabled' ? t('已开启') : t('未开启');
      st.className = 'um-state ' + (p.two_fa === 'enabled' ? 'ok' : 'off');
    }
  }

  function setOpen(open) {
    pop.hidden = !open;
    btn.setAttribute('aria-expanded', String(open));
    if (open) loadBrief();
  }

  btn.addEventListener('click', () => setOpen(pop.hidden));
  btn.addEventListener('mouseenter', loadBrief); // 提前取，打开时菜单项已就绪
  document.addEventListener('click', (e) => { if (!pop.hidden && !menu.contains(e.target)) setOpen(false); });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !pop.hidden) { setOpen(false); btn.focus(); }
  });
  pop.addEventListener('click', (e) => {
    // 选了链接 / 按钮就收起菜单（主题圆点不收，方便对比）
    if (e.target.closest('.um-theme, .um-out')) return;
    if (e.target.closest('a.um-item, button.um-item')) setOpen(false);
  });

  document.getElementById('um-profile').addEventListener('click', openDrawer);
}

// ---------- 抽屉 ----------

let drawer = null;
let lastFocus = null;

function ensureDrawer() {
  if (drawer) return drawer;
  const mask = el('div', { class: 'pf-mask', id: 'pf-mask', hidden: true });
  const close = el('button', { class: 'btn ghost', type: 'button', id: 'pf-close', 'aria-label': t('关闭'), text: '✕' });
  const body = el('div', { class: 'pf-body', id: 'pf-body' });
  const box = el('aside', { class: 'pf-drawer', id: 'pf-drawer', role: 'dialog', 'aria-modal': 'true', 'aria-label': t('个人信息与权限'), hidden: true },
    el('header', { class: 'pf-head' }, el('h2', { text: t('个人信息与权限') }), el('span', { class: 'spacer' }), close), body);
  document.body.append(mask, box);
  const shut = () => closeDrawer();
  mask.addEventListener('click', shut);
  close.addEventListener('click', shut);
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !box.hidden) shut(); });
  drawer = { mask, box, body, close };
  return drawer;
}

function closeDrawer() {
  if (!drawer) return;
  drawer.mask.hidden = true;
  drawer.box.hidden = true;
  if (lastFocus && lastFocus.isConnected) lastFocus.focus();
}

async function openDrawer() {
  const d = ensureDrawer();
  lastFocus = document.activeElement;
  d.mask.hidden = false;
  d.box.hidden = false;
  d.close.focus();
  d.body.replaceChildren(el('div', { class: 'muted', text: t('加载中…') }));
  let p;
  try {
    p = await api('GET', '/api/v1/me/profile?full=1');
  } catch (e) {
    d.body.replaceChildren(el('div', { class: 'alert err', role: 'alert', text: e.message || t('加载失败，请稍后重试') }));
    return;
  }
  d.body.replaceChildren(...render(p));
}

function chips(list) {
  if (!list || !list.length) return el('span', { class: 'muted', text: t('无') });
  return el('span', { class: 'pf-chips' }, list.map((x) => el('span', { class: 'chip', text: x })));
}

function render(p) {
  const rows = [];
  const add = (k, v) => rows.push(el('dt', { text: k }), el('dd', null, v));
  add(t('账号'), p.username);
  add(t('姓名'), p.display || '—');
  add(t('部门'), p.dept || '—');
  add(t('邮箱'), p.email || '—');
  add(t('认证方式'), chips((p.auth_methods || []).map((x) => tp(x))));
  if (p.two_fa) add(t('两步验证'), el('span', { class: 'um-state ' + (p.two_fa === 'enabled' ? 'ok' : 'off'), text: p.two_fa === 'enabled' ? t('已开启') : t('未开启') }));
  add(t('权限组'), p.superuser ? el('span', null, el('span', { class: 'badge warn', text: t('超级用户') }), ' ', chips(p.auth_groups)) : chips(p.auth_groups));
  add(t('资源组'), chips(p.resource_groups));
  if (p.instance_count !== undefined) add(t('可见实例'), t('{n} 个', { n: p.instance_count }));
  if (p.last_login) add(t('上次登录'), p.last_login);
  const ip = p.login_ip;
  const exp = p.session_expire;
  let login = '';
  if (ip && exp) login = t('来自 {ip}（会话有效至 {time}）', { ip, time: exp });
  else if (ip) login = t('来自 {ip}', { ip });
  else if (exp) login = t('（会话有效至 {time}）', { time: exp });
  add(t('本次登录'), login || '—');

  const out = [el('section', { class: 'pf-sec' }, el('h3', { text: t('账号信息') }), el('dl', { class: 'kv' }, rows))];
  const links = [];
  if (p.can_change_password) links.push(el('a', { class: 'btn', href: '/account/password', text: t('修改密码') }));
  if (p.can_bind) links.push(el('a', { class: 'btn', href: '/account/bindings', text: t('外部账号绑定') }));
  if (links.length) out.push(el('div', { class: 'pf-links' }, links));
  out.push(renderButtons(p));
  return out;
}

const SOURCE = {
  super: t('超级用户'),
  allow: t('单独授权'),
  deny: t('单独禁用'),
  legacy: t('原权限码'),
  open: t('登录即可'),
  none: t('未授予'),
  locked: t('仅超级用户可用'),
  inactive: t('账号未启用'),
};

function sourceText(b) {
  if (b.source === 'group') return t('权限组：{groups}', { groups: (b.groups || []).join(t('、|sep')) });
  let s = SOURCE[b.source] || b.source;
  if (b.expire_at && (b.source === 'allow' || b.source === 'deny')) s = t('{source}（至 {time}）', { source: s, time: b.expire_at });
  return s;
}

function renderButtons(p) {
  const sec = el('section', { class: 'pf-sec' }, el('h3', { text: t('我拥有的按钮权限') }));
  const b = p.buttons;
  if (!b) {
    sec.appendChild(el('div', { class: 'muted', text: tp(p.buttons_error) || t('暂无按钮权限信息') }));
    return sec;
  }
  const cnt = { total: b.total, n: b.allowed, g: b.granted, d: b.denied };
  let sum;
  if (p.superuser) sum = t('超级用户拥有全部 {total} 个按钮权限', cnt);
  else if (b.granted && b.denied) sum = t('共 {total} 个按钮，你拥有 {n} 个；单独授权 {g} 个；单独禁用 {d} 个', cnt);
  else if (b.granted) sum = t('共 {total} 个按钮，你拥有 {n} 个；单独授权 {g} 个', cnt);
  else if (b.denied) sum = t('共 {total} 个按钮，你拥有 {n} 个；单独禁用 {d} 个', cnt);
  else sum = t('共 {total} 个按钮，你拥有 {n} 个', cnt);
  sec.appendChild(el('p', { class: 'muted small', text: sum }));

  const q = el('input', { type: 'search', placeholder: t('按名称 / 权限码筛选'), 'aria-label': t('筛选按钮'), autocomplete: 'off' });
  const showAll = el('input', { type: 'checkbox' });
  const onlyOv = el('input', { type: 'checkbox' });
  sec.appendChild(el('div', { class: 'pf-filter' }, q,
    el('label', { class: 'mini' }, showAll, ' ' + t('同时显示没有的按钮')),
    el('label', { class: 'mini' }, onlyOv, ' ' + t('只看单独授权 / 禁用'))));
  const list = el('div', { class: 'pf-list' });
  sec.appendChild(list);

  const draw = () => {
    const kw = q.value.trim().toLowerCase();
    const mods = new Map();
    let shown = 0;
    for (const pg of b.pages) {
      const btns = pg.buttons.filter((x) => {
        if (!showAll.checked && !x.allowed && x.source !== 'deny') return false; // 默认只列有的；单独禁用的始终列出，方便解释“为什么没有”
        if (onlyOv.checked && x.source !== 'allow' && x.source !== 'deny') return false;
        return !kw || x.label.toLowerCase().includes(kw) || tp(x.label).toLowerCase().includes(kw) || x.code.toLowerCase().includes(kw);
      });
      if (!btns.length) continue;
      shown += btns.length;
      if (!mods.has(pg.module)) mods.set(pg.module, []);
      mods.get(pg.module).push(el('div', { class: 'pf-page' }, el('div', { class: 'pf-pname', text: tp(pg.page) }),
        el('div', { class: 'pf-btns' }, btns.map((x) => {
          const cls = ['pf-btn'];
          if (x.source === 'deny') cls.push('deny'); else if (x.allowed) cls.push('ok'); else cls.push('no');
          if (x.source === 'allow') cls.push('grant');
          if (x.kind === 'd') cls.push('danger');
          return el('span', { class: cls.join(' '), title: x.code + ' · ' + sourceText(x) },
            tp(x.label), x.kind === 'd' ? el('i', { text: t('高危') }) : null,
            x.source === 'allow' ? el('i', { class: 'g', text: t('单独授权') }) : null,
            x.source === 'deny' ? el('i', { class: 'x', text: t('单独禁用') }) : null);
        }))));
    }
    if (!shown) { list.replaceChildren(el('div', { class: 'muted', text: t('没有符合条件的按钮。') })); return; }
    list.replaceChildren(...[...mods].map(([m, pages]) => el('div', { class: 'pf-mod' }, el('h4', { text: tp(m) }), pages)));
  };
  q.addEventListener('input', draw);
  showAll.addEventListener('change', draw);
  onlyOv.addEventListener('change', draw);
  draw();
  return sec;
}
