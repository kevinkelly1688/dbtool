/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: main.js
 * @time: 2026/10/28
 */
// 值班表管理页（平台设置 › 值班表）：当前值班、7 天排班预览、班次的新建 / 修改 / 停用 / 删除、临时调班。
// 所有往 DOM 写的内容都用 textContent；真正的校验与授权在服务端。
import { api, riskConfirm } from '../common.js';
import { t, tp } from '../i18n.js';
import { el, dayName, daysText, WEEK_ORDER, dayBar, dayTicks, personIndex, poll } from './bar.js';

const $ = (id) => document.getElementById(id);
const root = $('duty');
const canEdit = root.dataset.edit === '1';
const canSwap = root.dataset.swap === '1';

function setMsg(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

function btn(text, cls, fn) {
  const b = el('button', 'btn ' + cls, text);
  b.type = 'button';
  b.addEventListener('click', fn);
  return b;
}

const two = (n) => String(n).padStart(2, '0');
const toMin = (hhmm) => { const [h, m] = hhmm.split(':').map(Number); return h * 60 + m; };

// 时长：5 小时 / 5 小时 30 分 / 45 分钟
function durText(min) {
  const h = Math.floor(min / 60);
  const m = min % 60;
  if (h === 0) return t('{m} 分钟', { m });
  return m === 0 ? t('{h} 小时', { h }) : t('{h} 小时 {m} 分', { h, m });
}

const maskDays = (mask) => WEEK_ORDER.filter((d) => mask & (1 << d));

let ov = null; // 概览（GET /api/v1/duty/overview）
let dayIdx = 0; // 预览选中的是第几天
let editing = null; // 正在修改的班次 id；null 表示新建
let editingVersion = ''; // 打开编辑框时那个班次的版本：保存时带回去，别人这期间改过就会被拒绝
let loadFailed = false;

const ready = () => !!(ov && ov.ready);

// 改了班次 / 调班之后重新载入页面数据，并让顶栏的值班芯片马上跟上（见 shell/duty.js 的 duty:changed）
async function reloadAfterChange() {
  await load();
  document.dispatchEvent(new CustomEvent('duty:changed'));
}

// 没取到数据（出错 / 表还没建）时，“新建班次 / 临时调班”点了也没法用：先禁用，取到了再放开。
// 没有权限的按钮是服务端渲染成禁用的，不在这里动。
function syncButtons() {
  const b1 = $('duty-shift-new');
  const b2 = $('duty-swap-new');
  if (b1 && canEdit) b1.disabled = !ready();
  if (b2 && canSwap) b2.disabled = !ready();
}

// background 为 true 是页面自己每分钟的刷新：不给会话续期（见 bar.js 的 poll）
async function load(background = false) {
  try {
    ov = background ? await poll('/api/v1/duty/overview') : await api('GET', '/api/v1/duty/overview');
  } catch (e) {
    loadFailed = true;
    syncButtons();
    setMsg($('duty-msg'), tp(e.message));
    return;
  }
  if (loadFailed) { loadFailed = false; setMsg($('duty-msg'), ''); }
  syncButtons();
  $('duty-missing').hidden = ov.ready;
  $('duty-body').hidden = !ov.ready;
  if (!ov.ready) return;
  $('duty-tz').textContent = t('时间为服务器时区 {tz}', { tz: ov.tz });
  fillUsers();
  if (dayIdx >= ov.days.length) dayIdx = 0;
  renderNow();
  renderPreview();
  renderShifts();
  renderOverrides();
}

function fillUsers() {
  const dl = $('duty-users');
  dl.replaceChildren();
  for (const u of ov.users) {
    const o = el('option');
    o.value = u.username;
    if (u.display && u.display !== u.username) o.label = u.display;
    dl.append(o);
  }
}

// 输入框里可能是登录名，也可能是显示名：唯一匹配到显示名时换成登录名
function resolveUser(v) {
  v = v.trim();
  if (!v || ov.users.some((u) => u.username === v)) return v;
  const hit = ov.users.filter((u) => u.display === v);
  return hit.length === 1 ? hit[0].username : v;
}

// ---------------------------------------------------------------- 当前值班
function remainText(endsAt) {
  const min = Math.round(Math.max(0, endsAt - ov.now) / 60);
  const h = Math.floor(min / 60);
  const m = min % 60;
  if (h > 0) return t('还剩 {h} 小时 {m} 分', { h, m });
  if (m > 0) return t('还剩 {m} 分钟', { m });
  return t('不到 1 分钟');
}

function renderNow() {
  const box = $('duty-now');
  const c = ov.current;
  const nowText = `${c.date} ${two(Math.floor(c.now_min / 60))}:${two(c.now_min % 60)}`;
  const h2 = el('h2', '', t('当前值班'));
  h2.append(el('span', 'spacer'), el('span', 'muted small', t('现在：{time}', { time: nowText })));
  const parts = [h2];

  const cur = el('div', 'duty-cur');
  if (c.cur) {
    cur.append(el('span', 'duty-dot'), el('b', 'duty-who', c.cur.display));
    if (c.cur.user !== c.cur.display) cur.append(el('span', 'muted', c.cur.user));
    cur.append(el('span', 'badge ok', c.cur.name || t('调班')));
    cur.append(el('span', 'duty-meta', `${c.cur.start}–${c.cur.end} · ${remainText(c.cur.ends_at)}`));
    const contact = c.cur.contact || c.cur.email; // 班次 / 调班里填了联系方式就显示它，没填显示账号邮箱
    if (contact) cur.append(el('span', 'muted', t('联系方式：{contact}', { contact })));
  } else {
    cur.append(el('span', 'duty-dot none'), el('b', 'duty-who none', t('当前时段没有人值班')));
  }
  if (c.next) {
    cur.append(el('span', 'muted', c.next.gap
      ? t('{time} 起无人值班', { time: c.next.time })
      : t('{time} 交班给 {name}', { time: c.next.time, name: c.next.display })));
  }
  parts.push(cur);

  const notes = el('div', 'duty-notes');
  const enabled = ov.shifts.filter((s) => s.enabled);
  if (!enabled.length) {
    notes.append(alertLine('info', t('还没有排班。点右上角“新建班次”开始，例如：白班 08:00–19:00、晚班 19:00–24:00、夜班 00:00–08:00。')));
  } else if (ov.gaps.length) {
    const gaps = ov.gaps.map((g) => `${daysText(g.mask)} ${g.start}–${g.end}`).join(t('；|sep'));
    notes.append(alertLine('warn', t('每周有这些时段没有人值班：{gaps}', { gaps })));
  } else {
    notes.append(el('span', 'badge ok', t('24 小时全覆盖')));
  }
  for (const s of enabled.filter((x) => !x.active)) {
    const p = { name: s.name, user: s.user };
    notes.append(alertLine('err', s.missing
      ? t('班次「{name}」的值班人 {user} 的账号已不存在，这段时间会显示为无人值班。', p)
      : t('班次「{name}」的值班人 {user} 的账号已停用，这段时间会显示为无人值班。', p)));
  }
  for (const o of ov.overrides.filter((x) => !x.active && (x.state === 'active' || x.state === 'upcoming'))) {
    const p = { id: o.id, user: o.user };
    notes.append(alertLine('err', o.missing
      ? t('调班 #{id} 的值班人 {user} 的账号已不存在，这段时间会显示为无人值班。', p)
      : t('调班 #{id} 的值班人 {user} 的账号已停用，这段时间会显示为无人值班。', p)));
  }
  parts.push(notes);
  box.replaceChildren(...parts);
}

function alertLine(kind, text) {
  const a = el('div', 'alert ' + kind, text);
  a.style.margin = '8px 0 0';
  return a;
}

// ---------------------------------------------------------------- 排班预览
function dayLabel(d, i) {
  if (i === 0) return t('今天');
  if (i === 1) return t('明天');
  return `${dayName(d.weekday)} ${d.date.slice(5)}`;
}

function renderPreview() {
  const tabs = $('duty-days');
  tabs.replaceChildren();
  ov.days.forEach((d, i) => {
    const b = btn(dayLabel(d, i), i === dayIdx ? 'primary' : 'ghost', () => { dayIdx = i; renderPreview(); });
    b.setAttribute('aria-pressed', String(i === dayIdx));
    tabs.append(b);
  });
  const day = ov.days[dayIdx];
  const colors = personIndex(ov.days.map((d) => d.segments));
  const box = $('duty-day');
  box.replaceChildren();
  const wrap = el('div', 'duty-bar-wrap');
  wrap.append(dayBar(day.segments, day.today ? ov.current.now_min : null, { colors, minLabel: 50 }), dayTicks());
  box.append(wrap);
  const ul = el('ul', 'duty-day-list');
  for (const g of day.segments) {
    const li = el('li', g.gap ? 'gap' : '');
    const sw = el('i', 'duty-sw' + (g.gap ? ' gap' : ` p${colors.get(g.user) % 4}`));
    li.append(sw, el('span', 'duty-t', `${g.start}–${g.end}`), el('span', 'duty-p', g.gap ? t('无人值班') : g.display));
    const note = [];
    if (g.kind === 'override') note.push(g.reason ? t('调班：{reason}', { reason: g.reason }) : t('调班'));
    else if (g.name) note.push(g.name);
    if (g.prev) note.push(t('接昨日'));
    if (g.cont) note.push(t('至次日'));
    li.append(el('span', 'muted', note.join(' · ')));
    if (g.cur) li.append(el('i', 'duty-cur-tag', t('当前')));
    ul.append(li);
  }
  box.append(ul);
}

// ---------------------------------------------------------------- 班次
function personCell(x) {
  const td = el('td');
  td.append(el('span', '', x.display));
  if (x.user !== x.display) td.append(' ', el('span', 'muted small', x.user));
  if (!x.active) td.append(' ', el('span', 'badge crit', x.missing ? t('账号不存在') : t('账号已停用')));
  return td;
}

function renderShifts() {
  const tb = $('duty-shifts').tBodies[0];
  tb.replaceChildren();
  if (!ov.shifts.length) {
    const td = el('td', 'empty', t('还没有班次。'));
    td.colSpan = 7;
    const tr = el('tr');
    tr.append(td);
    tb.append(tr);
    return;
  }
  for (const s of ov.shifts) {
    const tr = el('tr', s.enabled ? '' : 'off');
    const name = el('td');
    name.append(el('b', '', s.name));
    if (!s.enabled) name.append(' ', el('span', 'badge', t('已停用')));
    if (s.remark) name.append(el('div', 'muted small', s.remark));
    const time = el('td', 'duty-time');
    time.append(`${s.start}–${s.end}`, ' ', el('span', 'muted small', s.minutes === 1440 ? t('24 小时') : durText(s.minutes)));
    tr.append(name, personCell(s), time, el('td', '', daysText(s.mask)),
      el('td', 'muted', s.contact || s.email || '—'),
      el('td', 'muted small', s.updated_at ? `${s.updated_at} ${s.updated_by || ''}`.trim() : '—'));
    const ops = el('td', 'duty-ops');
    if (canEdit) {
      ops.append(btn(t('编辑'), '', () => openShift(s)), ' ',
        btn(s.enabled ? t('停用') : t('启用'), 'ghost', () => toggleShift(s)), ' ',
        btn(t('删除'), 'ghost danger', () => removeShift(s)));
    }
    tr.append(ops);
    tb.append(tr);
  }
}

// 停用 / 启用只发这一个开关：服务端按库里最新的内容改，不会拿页面上可能已经过期的整行去覆盖别人刚做的修改
async function toggleShift(s) {
  const enabled = !s.enabled;
  try {
    await api('POST', `/api/v1/duty/shifts/${s.id}/enable`, { enabled });
    setMsg($('duty-msg'), enabled ? t('已启用') : t('已停用'), 'info');
    await reloadAfterChange();
  } catch (e) {
    setMsg($('duty-msg'), tp(e.message));
  }
}

async function removeShift(s) {
  const r = await riskConfirm({
    title: t('删除班次「{name}」？', { name: s.name }),
    lines: [t('删除后这个班次不再排班，其它班次和临时调班不受影响。')],
    level: 'high',
    okText: t('确定删除'),
  });
  if (!r.ok) return;
  try {
    await api('POST', `/api/v1/duty/shifts/${s.id}/delete`, { confirm: true });
    setMsg($('duty-msg'), t('已删除'), 'info');
    await reloadAfterChange();
  } catch (e) {
    setMsg($('duty-msg'), tp(e.message));
  }
}

// 适用星期的复选框（周一排在最前）
function buildWeekdays() {
  const box = $('duty-f-wd');
  for (const d of WEEK_ORDER) {
    const lab = el('label', 'duty-wd-item');
    const cb = el('input');
    cb.type = 'checkbox';
    cb.value = String(d);
    lab.append(cb, ' ', dayName(d));
    box.append(lab);
  }
}
const wdBoxes = () => [...$('duty-f-wd').querySelectorAll('input')];
function setWeekdays(days) { for (const cb of wdBoxes()) cb.checked = days.includes(Number(cb.value)); }

function updateSpan() {
  const s = $('duty-f-start').value;
  const e = $('duty-f-end').value;
  const box = $('duty-f-span');
  if (!s || !e) { box.textContent = ''; return; }
  const sm = toMin(s);
  const em = toMin(e);
  let text;
  if (sm === em) text = t('24 小时：从 {s} 到次日 {s}', { s });
  else if (em === 0) text = t('{s} 到 24:00（当天结束），共 {dur}', { s, dur: durText(1440 - sm) });
  else if (em < sm) text = t('{s} 到次日 {e}（跨天），共 {dur}', { s, e, dur: durText(1440 - sm + em) });
  else text = t('{s} 到 {e}，共 {dur}', { s, e, dur: durText(em - sm) });
  box.textContent = text;
}

function openShift(s) {
  if (!ready()) return;
  editing = s ? s.id : null;
  editingVersion = s ? s.version || '' : '';
  $('duty-sm-title').textContent = s ? t('编辑班次') : t('新建班次');
  $('duty-f-name').value = s ? s.name : '';
  $('duty-f-user').value = s ? s.user : '';
  $('duty-f-start').value = s ? s.start : '08:00';
  $('duty-f-end').value = s ? (s.e === 0 ? '00:00' : s.end) : '19:00';
  setWeekdays(s ? maskDays(s.mask) : [0, 1, 2, 3, 4, 5, 6]);
  $('duty-f-contact').value = s ? s.contact || '' : '';
  $('duty-f-remark').value = s ? s.remark || '' : '';
  $('duty-f-enabled').checked = s ? s.enabled : true;
  setMsg($('duty-sm-msg'), '');
  updateSpan();
  $('duty-sm').hidden = false;
  $('duty-f-name').focus();
}

function closeShift() { $('duty-sm').hidden = true; }

async function saveShift() {
  const msg = $('duty-sm-msg');
  if (!ready()) return setMsg(msg, t('数据还没加载好，请稍后再试'));
  const name = $('duty-f-name').value.trim();
  const username = resolveUser($('duty-f-user').value);
  const start = $('duty-f-start').value;
  const end = $('duty-f-end').value;
  const days = wdBoxes().filter((cb) => cb.checked).map((cb) => Number(cb.value));
  if (!name) return setMsg(msg, t('请填写班次名称'));
  if (!username) return setMsg(msg, t('请选择值班人'));
  if (!start || !end) return setMsg(msg, t('请填写开始和结束时间'));
  if (!days.length) return setMsg(msg, t('至少选择一个适用星期'));
  const body = {
    name, username, start, end, weekdays: days,
    contact: $('duty-f-contact').value.trim(), remark: $('duty-f-remark').value.trim(), enabled: $('duty-f-enabled').checked,
  };
  try {
    if (editing === null) await api('POST', '/api/v1/duty/shifts', body);
    else await api('PUT', '/api/v1/duty/shifts/' + editing, { ...body, version: editingVersion });
    closeShift();
    setMsg($('duty-msg'), t('已保存'), 'info');
    await reloadAfterChange();
  } catch (e) {
    setMsg(msg, tp(e.message));
    if (e.code === 'conflict' && editing !== null) await load(); // 别人刚改过：把列表刷成最新的，关掉编辑框重新打开就是新内容
  }
}

// ---------------------------------------------------------------- 临时调班
const STATE = { active: ['badge ok', t('进行中')], upcoming: ['badge', t('未开始')], past: ['badge', t('已结束')], canceled: ['badge', t('已撤销')] };

function renderOverrides() {
  const tb = $('duty-overrides').tBodies[0];
  tb.replaceChildren();
  if (!ov.overrides.length) {
    const td = el('td', 'empty', t('最近没有临时调班。'));
    td.colSpan = 6;
    const tr = el('tr');
    tr.append(td);
    tb.append(tr);
    return;
  }
  for (const o of ov.overrides) {
    const tr = el('tr', o.state === 'past' || o.state === 'canceled' ? 'off' : '');
    const time = el('td', 'duty-time', `${o.start} ~ ${o.end}`);
    const [cls, label] = STATE[o.state] || STATE.past;
    const st = el('td');
    st.append(el('span', cls, label));
    if (o.canceled && o.canceled_by) st.append(' ', el('span', 'muted small', `${o.canceled_at} ${o.canceled_by}`.trim()));
    tr.append(time, personCell(o), el('td', 'muted', o.reason || '—'), st,
      el('td', 'muted small', `${o.created_at || ''} ${o.created_by || ''}`.trim() || '—'));
    const ops = el('td', 'duty-ops');
    if (canSwap && (o.state === 'active' || o.state === 'upcoming')) ops.append(btn(t('撤销'), 'ghost danger', () => cancelOverride(o)));
    tr.append(ops);
    tb.append(tr);
  }
}

async function cancelOverride(o) {
  const r = await riskConfirm({
    title: t('撤销调班 #{id}？', { id: o.id }),
    details: [{ k: t('值班人'), v: o.display }, { k: t('时间'), v: `${o.start} ~ ${o.end}` }],
    lines: [t('撤销后这段时间按班次排班。')],
    level: 'high',
    okText: t('确定撤销'),
  });
  if (!r.ok) return;
  try {
    await api('POST', `/api/v1/duty/overrides/${o.id}/cancel`, { confirm: true });
    setMsg($('duty-msg'), t('已撤销'), 'info');
    await reloadAfterChange();
  } catch (e) {
    setMsg($('duty-msg'), tp(e.message));
  }
}

function openOverride() {
  if (!ready()) return;
  $('duty-o-user').value = '';
  $('duty-o-reason').value = '';
  $('duty-o-contact').value = '';
  $('duty-o-start').value = '';
  $('duty-o-end').value = '';
  $('duty-o-date').value = ov.days[0].date;
  const sel = $('duty-o-shift');
  sel.replaceChildren();
  const first = el('option', '', t('— 手动填写 —'));
  first.value = '';
  sel.append(first);
  for (const s of ov.shifts.filter((x) => x.enabled)) {
    const o = el('option', '', `${s.name}  ${s.start}–${s.end}`);
    o.value = String(s.id);
    sel.append(o);
  }
  setMsg($('duty-om-msg'), '');
  $('duty-om').hidden = false;
  $('duty-o-user').focus();
}

function closeOverride() { $('duty-om').hidden = true; }

// 选了班次和日期，就把开始 / 结束时间填成那个班次在那一天的时间段
function fillFromShift() {
  const id = Number($('duty-o-shift').value);
  const date = $('duty-o-date').value;
  const s = ov.shifts.find((x) => x.id === id);
  if (!s || !date) return;
  let endDate = date;
  if (s.cross) {
    const d = new Date(`${date}T00:00:00Z`);
    d.setUTCDate(d.getUTCDate() + 1);
    endDate = d.toISOString().slice(0, 10);
  }
  $('duty-o-start').value = `${date}T${s.start}`;
  $('duty-o-end').value = `${endDate}T${s.e === 0 ? '00:00' : s.end}`;
}

async function saveOverride() {
  const msg = $('duty-om-msg');
  if (!ready()) return setMsg(msg, t('数据还没加载好，请稍后再试'));
  const username = resolveUser($('duty-o-user').value);
  const start = $('duty-o-start').value;
  const end = $('duty-o-end').value;
  if (!username) return setMsg(msg, t('请选择值班人'));
  if (!start || !end) return setMsg(msg, t('请填写开始和结束时间'));
  const body = {
    username, start: start.replace('T', ' '), end: end.replace('T', ' '),
    reason: $('duty-o-reason').value.trim(), contact: $('duty-o-contact').value.trim(),
  };
  try {
    await api('POST', '/api/v1/duty/overrides', body);
    closeOverride();
    setMsg($('duty-msg'), t('已保存'), 'info');
    await reloadAfterChange();
  } catch (e) {
    setMsg(msg, tp(e.message));
  }
}

// ---------------------------------------------------------------- 绑定
buildWeekdays();
syncButtons(); // 第一次取到数据之前先禁用
if (canEdit) $('duty-shift-new')?.addEventListener('click', () => openShift(null));
if (canSwap) $('duty-swap-new')?.addEventListener('click', openOverride);
$('duty-sm-cancel').addEventListener('click', closeShift);
$('duty-sm-ok').addEventListener('click', saveShift);
$('duty-om-cancel').addEventListener('click', closeOverride);
$('duty-om-ok').addEventListener('click', saveOverride);
for (const id of ['duty-f-start', 'duty-f-end']) $(id).addEventListener('input', updateSpan);
$('duty-o-shift').addEventListener('change', fillFromShift);
$('duty-o-date').addEventListener('change', fillFromShift);
for (const b of root.querySelectorAll('[data-wd]')) {
  b.addEventListener('click', () => setWeekdays({ all: [0, 1, 2, 3, 4, 5, 6], work: [1, 2, 3, 4, 5], weekend: [6, 0] }[b.dataset.wd]));
}
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  if (!$('duty-sm').hidden) closeShift();
  else if (!$('duty-om').hidden) closeOverride();
});
// 页面开着时每分钟刷新一次：“当前值班 / 还剩多久 / 现在的竖线”跟着走；弹窗开着时不刷（不打断填写）
setInterval(() => { if (!document.hidden && $('duty-sm').hidden && $('duty-om').hidden) load(true); }, 60000);

await load();
