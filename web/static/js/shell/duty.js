/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: duty.js
 * @time: 2026/10/28
 */
// 顶栏“值班”芯片（时钟左边）：点开看当前值班人、今日排班和下一次交班；到交班的时刻自己刷新。
// 服务端渲染页面时把值班快照放在 #tb-duty 的 data-duty 里（首屏不用等接口），之后用 GET /api/v1/duty/now 更新；
// 取不到就保持原样，不影响页面；接口说没有值班信息了（班次都被停用 / 删除）就把整块藏起来。
// 所有文本都用 textContent 写入，不拼 HTML。
import { t } from '../i18n.js';
import { el, dayBar, dayTicks, poll } from '../duty/bar.js';

const box = document.getElementById('tb-duty');
if (box) init(box);

function init(box) {
  const btn = document.getElementById('td-btn');
  const pop = document.getElementById('td-pop');
  let snap = null;
  try { snap = JSON.parse(box.dataset.duty || 'null'); } catch (_) { snap = null; }
  if (!snap || !snap.enabled) { box.hidden = true; return; }
  let fetchedAt = Date.now();
  let timer = null; // 到交班时刻重新取
  let tick = null; // 浮层打开时，每 30 秒刷新“还剩多久”

  // 服务器此刻的 Unix 秒（按取到快照后过去的时间推算，不依赖浏览器时钟和服务器时钟是否一致）
  const serverNow = () => snap.now + (Date.now() - fetchedAt) / 1000;

  // 剩余时间按分钟四舍五入（刚打开时 8 小时 29 分 59 秒显示成 8 小时 30 分，不会一上来就少一分钟）
  function remainText(endsAt) {
    const min = Math.round(Math.max(0, endsAt - serverNow()) / 60);
    const h = Math.floor(min / 60);
    const m = min % 60;
    if (h > 0) return t('还剩 {h} 小时 {m} 分', { h, m });
    if (m > 0) return t('还剩 {m} 分钟', { m });
    return t('不到 1 分钟');
  }

  // ---------------- 芯片
  function paintChip() {
    const on = snap.state === 'on' && snap.cur;
    btn.classList.toggle('on', !!on);
    btn.classList.toggle('none', !on);
    btn.querySelector('.td-name').textContent = on ? snap.cur.display : t('暂无|duty');
    let range = btn.querySelector('.td-range');
    if (!range) { range = el('span', 'td-range'); btn.append(' ', range); }
    range.textContent = on ? `${snap.cur.start}–${snap.cur.end}` : '';
    let tag = btn.querySelector('.td-tag');
    if (on && snap.cur.kind === 'override') {
      if (!tag) { tag = el('i', 'td-tag', t('调班')); range.before(tag, ' '); }
    } else if (tag) {
      tag.remove();
    }
  }

  // ---------------- 浮层
  function row(g) {
    const li = el('li', 'td-row' + (g.cur ? ' cur' : '') + (g.past ? ' past' : '') + (g.gap ? ' gap' : ''));
    li.append(el('span', 'td-r-time', `${g.start}–${g.end}`));
    const who = el('span', 'td-r-who', g.gap ? t('无人值班') : g.display);
    li.append(who);
    const note = [];
    if (!g.gap && g.name) note.push(g.name);
    else if (!g.gap && g.kind === 'override') note.push(t('调班'));
    if (g.prev) note.push(t('接昨日'));
    if (g.cont) note.push(t('至次日'));
    li.append(el('span', 'td-r-note', note.join(' · ')));
    if (g.cur) li.append(el('i', 'td-r-cur', t('当前')));
    return li;
  }

  function buildPop() {
    const keepFocus = pop.contains(document.activeElement) && document.activeElement.classList.contains('td-manage'); // 定时刷新重建浮层时，别把键盘焦点丢掉
    const c = snap.cur;
    const parts = [];
    const head = el('div', 'td-head');
    head.append(el('span', 'td-h-label', t('当前值班')));
    if (c) head.append(el('span', 'td-h-shift', c.name || t('调班')));
    parts.push(head);
    if (c) {
      const who = el('div', 'td-who');
      who.append(el('b', '', c.display));
      if (c.user !== c.display) who.append(el('span', 'muted', c.user));
      parts.push(who);
      const meta = el('div', 'td-meta');
      meta.dataset.endsAt = String(c.ends_at);
      meta.textContent = `${c.start}–${c.end} · ${remainText(c.ends_at)}`;
      parts.push(meta);
      if (c.kind === 'override' && c.reason) parts.push(el('div', 'td-meta', t('调班原因：{reason}', { reason: c.reason })));
      const contact = c.contact || c.email; // 班次 / 调班里填了联系方式就显示它，没填显示账号邮箱
      if (contact) parts.push(el('div', 'td-contact', t('联系方式：{contact}', { contact })));
    } else {
      parts.push(el('div', 'td-who none', t('当前时段没有人值班')));
    }
    if (snap.today && snap.today.length) {
      const wrap = el('div', 'td-day');
      wrap.append(dayBar(snap.today, snap.now_min + (Date.now() - fetchedAt) / 60000), dayTicks());
      parts.push(wrap);
      parts.push(el('div', 'td-sec', t('今日排班')));
      const ul = el('ul', 'td-rows');
      for (const g of snap.today) ul.append(row(g));
      parts.push(ul);
    }
    const foot = el('div', 'td-foot');
    if (snap.next) {
      foot.append(el('span', 'td-next', snap.next.gap
        ? t('{time} 起无人值班', { time: snap.next.time })
        : t('{time} 交班给 {name}', { time: snap.next.time, name: snap.next.display })));
    } else {
      foot.append(el('span'));
    }
    if (box.dataset.manage) {
      const a = el('a', 'td-manage', t('管理值班表'));
      a.href = box.dataset.manage;
      foot.append(a);
    }
    parts.push(foot);
    parts.push(el('div', 'td-tz', t('时间为服务器时区 {tz}', { tz: snap.tz })));
    pop.replaceChildren(...parts);
    if (keepFocus) pop.querySelector('.td-manage')?.focus();
  }

  function tickRemain() {
    const meta = pop.querySelector('.td-meta[data-ends-at]');
    if (meta && snap.cur) meta.textContent = `${snap.cur.start}–${snap.cur.end} · ${remainText(snap.cur.ends_at)}`;
  }

  function setOpen(open) {
    pop.hidden = !open;
    btn.setAttribute('aria-expanded', String(open));
    clearInterval(tick);
    tick = null;
    if (open) {
      buildPop();
      tick = setInterval(tickRemain, 30000);
    }
  }

  btn.addEventListener('click', () => setOpen(pop.hidden));
  document.addEventListener('click', (e) => { if (!pop.hidden && !box.contains(e.target)) setOpen(false); });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !pop.hidden) { setOpen(false); btn.focus(); }
  });

  // ---------------- 刷新
  // 离服务器时区下一个零点还有多久（毫秒，多留 1.5 秒）：过了零点“今日排班”要换成新的一天，即使这时没有交班
  // （一天里已过的秒数 = 当天第几分钟 × 60 + 当前秒；服务器时区的偏移按整分钟算）
  const msToMidnight = () => (86400 - (snap.now_min * 60 + (snap.now % 60)) - (Date.now() - fetchedAt) / 1000) * 1000 + 1500;

  // 到交班时刻或零点重新取；平时最长 5 分钟取一次（别人改了班次、停用了账号也能跟上），最短隔 5 秒
  function schedule(ms) {
    clearTimeout(timer);
    timer = setTimeout(refresh, Math.min(Math.max(Math.min(ms, msToMidnight()), 5000), 5 * 60 * 1000));
  }

  async function refresh() {
    try {
      const d = await poll('/api/v1/duty/now');
      if (!d || !d.enabled) { box.hidden = true; setOpen(false); schedule(5 * 60 * 1000); return; }
      snap = d;
      fetchedAt = Date.now();
      box.hidden = false;
      paintChip();
      if (!pop.hidden) buildPop();
      schedule((d.next_at - d.now) * 1000 + 1500);
    } catch (e) {
      if (e && (e.status === 401 || e.status === 403)) return; // 登录过期 / 没有权限：不再轮询，用户下一次操作时页面自己会提示重新登录
      schedule(60 * 1000); // 暂时取不到：保持原样，一分钟后再试
    }
  }

  // 管理页改了班次 / 调班后发出这个事件：芯片马上取一次，不等下一次定时刷新（否则刚停用的班次，顶栏还显示着那个人）
  document.addEventListener('duty:changed', refresh);

  // 睡眠 / 切到后台再回来：定时器可能被推迟，回到前台时按需补一次
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && (Date.now() - fetchedAt > 60000 || serverNow() >= snap.next_at)) refresh();
  });

  paintChip();
  schedule((snap.next_at - snap.now) * 1000 + 1500);
}
