/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: notify.js
 * @time: 2026/10/28
 */
// 通知渠道管理（仅超级用户）：机器人（钉钉 / 企业微信 / 飞书 / Lark / Slack / Telegram）与邮件（SMTP）渠道的列表与筛选、编辑（多资源组、值班人员、@ 规则、
// 仅工作时间 @、邮件的 SMTP 与收件人）、消息预览（不真发）、测试发送（邮件逐阶段显示结果）、行内启停、发送记录。
// 保存 / 删除 / 启停都走统一确认组件 confirmRun。
import { api, confirmRun } from '../common.js';
import { el, qs, pager, tabs, emptyRow, fillSelect, status } from '../sysadmin/ui.js';
import { t, tp, isZh } from '../i18n.js';

const $ = (id) => document.getElementById(id);
const els = {
  msg: $('nc-msg'), panel: $('nc-panel'), title: $('nc-title'), type: $('nc-type'), name: $('nc-name'), enabled: $('nc-enabled'),
  global: $('nc-global'), groups: $('nc-groups'), groupsHint: $('nc-groups-hint'), url: $('nc-url'),
  secret: $('nc-secret'), clearWrap: $('nc-clear-wrap'), clear: $('nc-clear'), events: $('nc-events'),
  duty: $('nc-duty'), dutyHint: $('nc-duty-hint'), rules: $('nc-rules'), rows: $('nc-rows'),
  simEvent: $('nc-sim-event'), simTime: $('nc-sim-time'), simLevel: $('nc-sim-level'), preview: $('nc-preview'), kpis: $('nc-kpis'),
  urlWrap: $('nc-url-wrap'), secretTxt: $('nc-secret-txt'), clearTxt: $('nc-clear-txt'), testToWrap: $('nc-test-to-wrap'), testTo: $('nc-test-to'),
  testResult: $('nc-test-result'),
};
const em = {
  box: $('nc-email'), notice: $('nc-em-notice'), host: $('nc-em-host'), port: $('nc-em-port'), enc: $('nc-em-enc'), user: $('nc-em-user'),
  verify: $('nc-em-verify'), verifyWarn: $('nc-em-verify-warn'), plainWrap: $('nc-em-plain-wrap'), plain: $('nc-em-plain'),
  from: $('nc-em-from'), fromName: $('nc-em-fromname'), to: $('nc-em-to'), cc: $('nc-em-cc'), prefix: $('nc-em-prefix'),
};
// 列表项分隔、冒号、括号随界面语言（中文用全角，英文用半角）
const SEP = isZh ? '、' : ', ';
const COLON = isZh ? '：' : ': ';
const PL = isZh ? '（' : ' (';
const PR = isZh ? '）' : ')';
const SIGN_TXT = t('签名密钥（可留空）');
const DEF_PORT = { none: 25, starttls: 587, ssl: 465 };
const ENC_LABEL = { none: t('不加密'), starttls: 'STARTTLS', ssl: 'SSL/TLS' };
const isEmail = (ty) => ty === 'email';
const splitAddrs = (v) => v.split(/[,;，；\s]+/).map((x) => x.trim()).filter(Boolean);
let lastEnc = 'starttls';

let ctx = null; // 服务端返回的选项与渠道列表
let editing = null; // 正在编辑的渠道；null 表示新建
let logsLoaded = false;

const TARGETS = [
  ['applicant', t('提交人')], ['reviewers', t('当前审批级的审批人')], ['dba_duty', t('DBA 值班')], ['auth_group', t('某个权限组的成员')],
  ['user', t('指定用户（用户名）')], ['raw', t('渠道原生标识')], ['all', t('所有人')],
];
// 渠道原生标识输入框的占位符（按渠道类型）
const RAW_PH = { slack: 'U012AB3CD', dingtalk: t('手机号 / userId'), wecom: t('成员 userid / 手机号'), feishu: 'ou_xxx', lark: 'ou_xxx', telegram: t('用户名 / 用户 ID') };
// 邮件渠道里部分 @ 目标的叫法不同（它们解析成收件人，而不是 @ 某人）
const EMAIL_TARGET = { raw: t('邮箱地址'), all: t('工单所属资源组的全部用户') };

function setMsg(text, kind = 'err') {
  els.msg.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  els.msg.append(d);
}

// “审批通过”在公共词表里是“审批”按钮（Approve）；这里是事件 / 消息名（一种状态），用带上下文的键译成 Approved
const tpName = (s) => (s === '审批通过' ? t('审批通过|state') : tp(s)); // i18n-ignore 与接口返回的名称比较
// 取选项的显示名（服务端给的是中文原文，按界面语言翻译）
const labelOf = (list, v) => tpName((list.find((x) => String(x.value) === String(v)) || { label: v }).label);
// 事件的短名：去掉括号里的补充说明（中文全角括号，译文半角括号）
const shortLabel = (label) => tpName(label).split(/\s*[（(]/)[0];
const evName = (e) => shortLabel(labelOf(ctx.events, e));
// 列表里“机器人 / SMTP”一栏的提示：邮件渠道用已翻译的加密方式自己拼；机器人渠道是服务端给的主机名（旧版 webhook 的说明按目录翻译）
const hintOf = (c) => (isEmail(c.type) && c.email ? c.email.host + ':' + c.email.port + ' · ' + (ENC_LABEL[c.email.encryption] || c.email.encryption) : tp(c.webhook_hint));
const groupNameOf = (id) => (ctx.groups.find((g) => g.id === id) || { name: '#' + id }).name;
const groupText = (c) => (c.group_ids.length ? c.group_ids.map(groupNameOf).join(SEP) : t('全局（所有资源组）'));

function describeRule(r, type) {
  const tg = TARGETS.find((x) => x[0] === r.target);
  let s = tg ? ((isEmail(type) && EMAIL_TARGET[r.target]) || tg[1]) : r.target;
  if (r.target === 'auth_group') s += COLON + ((ctx.auth_groups.find((g) => String(g.id) === r.value) || {}).name || r.value);
  else if (r.value) s += COLON + r.value;
  if (r.events && r.events.length) s += PL + r.events.map(evName).join(SEP) + PR;
  if (r.work_hours) s += ' · ' + t('仅工作时间');
  return s;
}

// ---- KPI ----

function renderKpis() {
  const box = els.kpis;
  box.replaceChildren();
  const card = (label, value, sub, cls) => {
    const d = el('div');
    d.append(el('span', '', label), el('b', cls || '', value));
    if (sub) d.append(el('small', '', sub));
    box.append(d);
  };
  if (!ctx.ready) { box.hidden = true; return; }
  box.hidden = false;
  const on = ctx.channels.filter((c) => c.enabled);
  card(t('渠道'), String(ctx.channels.length), t('启用 {on} · 停用 {off}', { on: on.length, off: ctx.channels.length - on.length }));
  const allCovered = on.some((c) => !c.group_ids.length);
  const covered = new Set();
  for (const c of on) for (const g of c.group_ids) covered.add(g);
  const uncovered = allCovered ? [] : ctx.groups.filter((g) => !covered.has(g.id));
  card(t('资源组覆盖'), `${ctx.groups.length - uncovered.length} / ${ctx.groups.length}`,
    uncovered.length ? t('未覆盖：{list}', { list: uncovered.map((g) => g.name).join(SEP) }) : '', uncovered.length ? 'warn' : '');
  const evs = new Set();
  for (const c of on) for (const e of (c.events.length ? c.events : ctx.events.map((x) => x.value))) evs.add(e);
  card(t('事件覆盖'), `${evs.size} / ${ctx.events.length}`);
  if (ctx.log_ready && ctx.stats) card(t('近 7 天发送'), String(ctx.stats.total), t('失败 {n} 条', { n: ctx.stats.failed }), ctx.stats.failed ? 'bad' : '');
  else card(t('近 7 天发送'), '—');
}

// ---- 列表 ----

function filtered() {
  const ft = $('nc-f-type').value, g = $('nc-f-group').value, q = $('nc-f-q').value.trim().toLowerCase();
  return ctx.channels.filter((c) => (!ft || c.type === ft)
    && (!g || (g === 'global' ? !c.group_ids.length : c.group_ids.includes(Number(g))))
    && (!q || (c.name + ' ' + hintOf(c) + ' ' + c.webhook_hint).toLowerCase().includes(q)));
}

async function toggle(c) {
  const to = !c.enabled;
  const cr = await confirmRun({
    title: to ? t('启用通知渠道「{name}」', { name: c.name }) : t('停用通知渠道「{name}」', { name: c.name }),
    level: to ? 'normal' : 'high', code: 'notify.edit', codeName: t('新建 / 编辑渠道与 @ 规则'),
    lines: [to ? t('启用后，该渠道会按订阅的事件开始发送通知。') : t('停用后，该渠道不再收到任何事件通知；配置保留，可随时重新启用。')],
    details: [{ k: t('渠道'), v: c.name }, { k: t('类型'), v: labelOf(ctx.types, c.type) }, { k: isEmail(c.type) ? 'SMTP' : 'Webhook', v: hintOf(c) }],
    okText: to ? t('确认启用') : t('确认停用'),
  }, () => api('POST', `/api/v1/settings/notify/channels/${c.id}/toggle`, { enabled: to }));
  if (!cr.ok) return;
  try { await load(); setMsg(to ? t('已启用「{name}」', { name: c.name }) : t('已停用「{name}」', { name: c.name }), 'info'); } catch (e) { setMsg(e.message); }
}

function lastCell(c) {
  const td = el('td', 'nc-last');
  if (!ctx.log_ready) { td.append(el('span', 'muted small', '—')); return td; }
  if (!c.last_send) { td.append(el('span', 'muted small', t('从未发送'))); return td; }
  const l = c.last_send;
  const st = status(l.ok ? t('成功') : t('失败'), l.ok ? 'passed' : 'rejected');
  if (l.error) st.title = tp(l.error); // 失败原因；成功但有提示（如跳过了没有邮箱的用户）也放在这里
  td.append(el('span', 'small mono', l.time), st);
  if (l.test) td.append(el('span', 'muted small', t('测试发送')));
  return td;
}

// 邮件渠道在列表里的补充信息：发件人、固定收件人数、安全相关的风险标记。
function emailSummary(e) {
  const box = el('div', 'small nc-em-sum');
  box.append(el('span', 'muted', t('发件人 {from} · 固定收件人 {n} 个', { from: e.from_addr, n: (e.to || []).length + (e.cc || []).length })));
  if (e.skip_verify && e.encryption !== 'none') box.append(el('span', 'nc-risk', t('不校验证书')));
  if (e.encryption === 'none') box.append(el('span', 'nc-risk', e.username ? t('明文传输且明文认证') : t('明文传输')));
  return box;
}

function renderList() {
  els.rows.replaceChildren();
  if (!ctx.ready) {
    emptyRow(els.rows, 9, t('通知渠道表未创建，请先执行 deploy/sql/001_notify_channel.sql。'));
    return;
  }
  if (!ctx.channels.length) {
    emptyRow(els.rows, 9, t('还没有配置渠道。'));
    return;
  }
  const list = filtered();
  if (!list.length) {
    emptyRow(els.rows, 9, t('没有符合筛选条件的渠道。'));
    return;
  }
  for (const c of list) {
    const tr = el('tr');
    const ev = el('td', 'nc-events-cell');
    if (c.events.length) {
      ev.append(el('span', '', t('{n} 项', { n: c.events.length })), el('div', 'small muted', c.events.slice(0, 3).map(evName).join(SEP) + (c.events.length > 3 ? '…' : '')));
      ev.title = c.events.map(evName).join(SEP);
    } else ev.append(el('span', 'muted', t('全部事件')));
    const rules = el('td', '', c.rules.length ? t('{n} 条', { n: c.rules.length }) : t('不 @'));
    if (c.rules.length) rules.title = c.rules.map((r) => describeRule(r, c.type)).join('\n');
    const st = el('td');
    st.append(el('span', 'st ' + (c.enabled ? 'passed' : 'aborted'), c.enabled ? t('启用|status') : t('停用|status')));
    const ops = el('td', 'row-actions');
    const btn = (text, cls, fn) => {
      const b = el('button', 'btn ghost' + (cls ? ' ' + cls : ''), text);
      b.type = 'button';
      b.addEventListener('click', fn);
      return b;
    };
    ops.append(btn(t('编辑'), '', () => open(c)), btn(c.enabled ? t('停用') : t('启用'), '', () => toggle(c)), btn(t('删除'), 'danger', async () => {
      const cr = await confirmRun({
        title: t('删除通知渠道「{name}」', { name: c.name }), level: 'high', code: 'notify.edit', codeName: t('新建 / 编辑渠道与 @ 规则'),
        lines: [isEmail(c.type) ? t('SMTP 配置与密码一并清除，不可恢复。如只是暂时不发，建议先“停用”。') : t('Webhook 与签名密钥一并清除，不可恢复。如只是暂时不发，建议先“停用”。')],
        details: [{ k: t('渠道'), v: c.name }, { k: t('类型'), v: labelOf(ctx.types, c.type) }, { k: isEmail(c.type) ? 'SMTP' : 'Webhook', v: hintOf(c) }],
        okText: t('确认删除'),
      }, () => api('DELETE', `/api/v1/settings/notify/channels/${c.id}`));
      if (!cr.ok) return;
      try { await load(); setMsg(t('已删除'), 'info'); } catch (e) { setMsg(e.message); }
    }));
    const hook = el('td', 'mono small', hintOf(c) + (c.has_secret ? ' · ' + (isEmail(c.type) ? t('已设密码') : t('已加签')) : ''));
    if (isEmail(c.type) && c.email) hook.append(emailSummary(c.email));
    tr.append(el('td', '', c.name), el('td', '', labelOf(ctx.types, c.type)), el('td', 'small', groupText(c)),
      hook, ev, rules, st, lastCell(c), ops);
    els.rows.append(tr);
  }
}

// ---- 编辑面板 ----

function addRuleRow(rule = { target: 'applicant', value: '', events: [], work_hours: false }) {
  const row = el('div', 'rule-row');
  const target = el('select');
  target.setAttribute('aria-label', t('@ 目标'));
  for (const [v, l] of TARGETS) {
    const o = new Option((isEmail(els.type.value) && EMAIL_TARGET[v]) || l, v);
    if (v === 'all' && els.type.value === 'telegram' && rule.target !== 'all') { o.disabled = true; o.text = t('所有人（Telegram 不支持）'); } // Telegram 没有“@所有人”（服务端也会拒绝）
    target.append(o);
  }
  target.value = rule.target;
  const valueBox = el('span', 'rule-value');
  const rm = el('button', 'btn ghost', t('删除'));
  rm.type = 'button';
  rm.addEventListener('click', () => row.remove());

  const opts = el('div', 'rule-opts');
  const wh = el('label', 'mini');
  const whCb = document.createElement('input');
  whCb.type = 'checkbox';
  whCb.className = 'rule-wh';
  whCb.checked = !!rule.work_hours;
  wh.append(whCb, document.createTextNode(t('仅工作时间 @')));
  const det = el('details');
  det.append(el('summary', '', t('仅在这些事件时 @')));
  const evBox = el('div', 'rule-events');
  for (const e of ctx.events) {
    const lb = el('label', 'mini');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = e.value;
    cb.checked = (rule.events || []).includes(e.value);
    lb.append(cb, document.createTextNode(shortLabel(e.label)));
    evBox.append(lb);
  }
  det.append(evBox);
  if ((rule.events || []).length) det.open = true;
  opts.append(wh, det);

  const drawValue = () => {
    valueBox.replaceChildren();
    const tv = target.value;
    if (tv === 'auth_group') {
      const s = el('select');
      s.setAttribute('aria-label', t('权限组'));
      for (const g of ctx.auth_groups) s.append(new Option(g.name, String(g.id)));
      s.value = rule.value || (ctx.auth_groups[0] ? String(ctx.auth_groups[0].id) : '');
      valueBox.append(s);
    } else if (tv === 'user' || tv === 'raw') {
      const i = el('input');
      i.type = 'text';
      i.value = rule.value || '';
      i.placeholder = tv === 'user' ? t('平台用户名') : (isEmail(els.type.value) ? 'dba@example.com' : RAW_PH[els.type.value] || t('渠道原生 ID'));
      i.setAttribute('aria-label', tv === 'user' ? t('用户名') : (isEmail(els.type.value) ? t('邮箱地址') : t('原生标识')));
      valueBox.append(i);
    } else if (tv === 'all' && !isEmail(els.type.value)) {
      valueBox.append(el('span', 'small muted', els.type.value === 'telegram' ? t('Telegram 没有“@所有人”，请删除这条规则') : t('会 @ 所有人')));
    }
  };
  target.addEventListener('change', () => { rule = { ...rule, value: '' }; drawValue(); });
  drawValue();
  row.append(target, valueBox, rm, opts);
  els.rules.append(row);
}

function collectRules() {
  return [...els.rules.children].map((row) => {
    const target = row.querySelector('select').value;
    const v = row.querySelector('.rule-value select, .rule-value input');
    const r = {
      target,
      value: v ? v.value.trim() : '',
      events: [...row.querySelectorAll('.rule-events input:checked')].map((c) => c.value),
    };
    if (row.querySelector('.rule-wh').checked) r.work_hours = true;
    return r;
  });
}

function drawGroups(c) {
  els.groups.replaceChildren();
  const selected = new Set(c ? c.group_ids : []);
  els.global.checked = c ? !c.group_ids.length : true;
  for (const g of ctx.groups) {
    const lb = el('label');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = String(g.id);
    cb.checked = selected.has(g.id);
    cb.addEventListener('change', () => {
      if (cb.checked) {
        els.global.checked = false;
        if (!ctx.ext_ready) for (const o of els.groups.querySelectorAll('input')) if (o !== cb) o.checked = false; // 没有扩展表：只能关联一个
      }
      syncGroups();
    });
    lb.append(cb, document.createTextNode(g.name));
    els.groups.append(lb);
  }
  syncGroups();
  els.groupsHint.textContent = ctx.ext_ready ? '' : t('关联多个资源组需先执行 deploy/sql/091_notify_ext.sql，目前只能关联一个。');
}

function syncGroups() {
  for (const o of els.groups.querySelectorAll('input')) o.disabled = els.global.checked;
}

function selectedGroupIDs() {
  if (els.global.checked) return [];
  return [...els.groups.querySelectorAll('input:checked')].map((c) => Number(c.value));
}

function open(c) {
  editing = c || null;
  els.panel.hidden = false;
  els.preview.replaceChildren();
  els.title.textContent = c ? t('编辑渠道：{name}', { name: c.name }) : t('新增渠道');
  fillSelect(els.type, ctx.types, (x) => tp(x.label), (x) => x.value);
  els.type.value = c ? c.type : ctx.types[0].value;
  els.name.value = c ? c.name : '';
  els.enabled.checked = c ? c.enabled : true;
  drawGroups(c);
  els.url.value = '';
  els.secret.value = '';
  els.clear.checked = false;
  fillEmail(c && c.email);
  els.testTo.value = '';
  els.testResult.replaceChildren();
  els.events.replaceChildren();
  for (const e of ctx.events) {
    const lb = el('label');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.value = e.value;
    cb.checked = c ? c.events.includes(e.value) : false;
    lb.append(cb, document.createTextNode(tpName(e.label)));
    els.events.append(lb);
  }
  els.duty.value = c ? c.duty_users.join(',') : '';
  els.duty.disabled = !ctx.ext_ready;
  els.dutyHint.textContent = ctx.ext_ready ? '' : t('需先执行 deploy/sql/091_notify_ext.sql。');
  els.rules.replaceChildren();
  for (const r of c ? c.rules : []) addRuleRow(r);
  fillSelect(els.simEvent, [{ v: '', t: t('默认测试消息（忽略事件过滤）') }, ...ctx.events.map((e) => ({ v: e.value, t: tpName(e.label) }))], (x) => x.t, (x) => x.v);
  fillSelect(els.simLevel, [{ v: '', t: t('不模拟') }, ...ctx.auth_groups.map((g) => ({ v: g.id, t: g.name }))], (x) => x.t, (x) => x.v);
  applyType();
  els.panel.scrollIntoView({ block: 'nearest' });
}

// 邮件字段的初始值：编辑时取已保存的配置（密码不回显）；新建时给常用默认值（STARTTLS / 587 / 校验证书 / 主题前缀）。
function fillEmail(e) {
  em.host.value = e ? e.host : '';
  em.enc.value = e ? e.encryption : 'starttls';
  em.port.value = e ? e.port : DEF_PORT[em.enc.value];
  lastEnc = em.enc.value;
  em.user.value = e ? e.username : '';
  em.verify.checked = e ? !e.skip_verify : true;
  em.plain.checked = !!(e && e.allow_plain_auth);
  em.from.value = e ? e.from_addr : '';
  em.fromName.value = e ? e.from_name : '';
  em.to.value = e ? (e.to || []).join(', ') : '';
  em.cc.value = e ? (e.cc || []).join(', ') : '';
  em.prefix.value = e ? e.subject_prefix : '[dbtool]';
}

// 随渠道类型切换表单：邮件显示 SMTP 字段，隐藏 Webhook；密码与签名密钥共用一个输入框（含义随类型变化）。
function applyType() {
  const email = isEmail(els.type.value);
  em.box.hidden = !email;
  els.urlWrap.hidden = email;
  els.testToWrap.hidden = !email;
  els.secretTxt.textContent = email ? t('SMTP 密码（可留空）') : SIGN_TXT;
  els.secret.setAttribute('aria-label', email ? t('SMTP 密码') : t('签名密钥'));
  els.clearTxt.textContent = email ? t('清除已有 SMTP 密码') : t('清除已有签名密钥');
  const noSign = els.type.value === 'wecom' || els.type.value === 'telegram'; // 企业微信 / Telegram 机器人没有加签：不显示签名密钥（服务端也不保存；从别的类型改过来时旧密钥会被清掉）
  $('nc-secret-wrap').hidden = noSign;
  if (noSign) els.secret.value = '';
  const same = !!editing && editing.has_secret && isEmail(editing.type) === email && !noSign; // 机器人的签名密钥与 SMTP 密码不是一回事，换类型后不沿用
  els.clearWrap.hidden = !same;
  if (!same) els.clear.checked = false;
  els.secret.placeholder = same ? t('已设置，留空不修改') : '';
  em.notice.replaceChildren();
  if (email && !ctx.email_ready) {
    em.notice.append(el('div', 'alert warn', t('邮件渠道表未创建，请先执行 deploy/sql/210_notify_email.sql。')));
  }
  syncEnc();
  syncUrlHint();
  const cur = collectRules();
  els.rules.replaceChildren();
  for (const r of cur) addRuleRow(r); // 规则的叫法 / 提示随类型变化
}

// 加密方式变化：不加密时才出现“允许明文认证”；不加密没有证书可校验。
function syncEnc() {
  const enc = em.enc.value;
  em.plainWrap.hidden = !(isEmail(els.type.value) && enc === 'none');
  em.verify.disabled = enc === 'none';
  em.verifyWarn.hidden = em.verify.checked || enc === 'none';
}

function syncUrlHint() {
  if (isEmail(els.type.value)) return;
  const host = (ctx.hosts || {})[els.type.value];
  const same = !!editing && editing.type === els.type.value && editing.webhook_hint;
  const fresh = els.type.value === 'telegram' ? 'https://api.telegram.org/bot<Bot Token>/sendMessage?chat_id=<Chat ID>' : host ? `https://${host}/…` : 'https://…';
  els.url.placeholder = same ? t('{hint}（留空不改）', { hint: hintOf(editing) }) : fresh;
}

function dutyList() {
  return els.duty.value.split(/[,，\s]+/).map((s) => s.trim()).filter(Boolean);
}

function body() {
  const ids = selectedGroupIDs();
  return {
    id: editing ? editing.id : 0,
    group_id: ids[0] || 0, group_ids: ids, name: els.name.value, type: els.type.value, url: els.url.value.trim(), secret: els.secret.value,
    clear_secret: els.clear.checked, events: [...els.events.querySelectorAll('input:checked')].map((c) => c.value), rules: collectRules(),
    duty_users: dutyList(), enabled: els.enabled.checked,
    ...(isEmail(els.type.value) ? { email: emailBody(), test_to: els.testTo.value.trim() } : {}),
  };
}

function emailBody() {
  const enc = em.enc.value;
  return {
    host: em.host.value.trim(), port: Number(em.port.value) || 0, encryption: enc, skip_verify: enc !== 'none' && !em.verify.checked,
    username: em.user.value.trim(), allow_plain_auth: enc === 'none' && em.plain.checked,
    from_addr: em.from.value.trim(), from_name: em.fromName.value.trim(), to: splitAddrs(em.to.value), cc: splitAddrs(em.cc.value),
    subject_prefix: em.prefix.value.trim(),
  };
}

// 模拟参数：事件、时间、当前审批级
function simBody() {
  const b = body();
  if (els.simEvent.value) b.event = els.simEvent.value;
  if (els.simTime.value) b.work_time = els.simTime.value === 'true';
  if (els.simLevel.value) b.reviewer_group_id = Number(els.simLevel.value);
  return b;
}

// 保存前的确认：列出要保存的关键信息；邮件渠道里有安全风险的选项（不校验证书、明文传输）要特别写明，并按高危处理。
function saveConfirm(b) {
  const lines = [];
  const details = [{ k: t('渠道'), v: b.name }, { k: t('类型'), v: labelOf(ctx.types, b.type) }, { k: t('范围'), v: b.group_ids.length ? b.group_ids.map(groupNameOf).join(SEP) : t('全局（所有资源组）') },
    { k: t('状态'), v: b.enabled ? t('启用|status') : t('停用|status') }];
  let high = false;
  if (b.email) {
    const e = b.email;
    details.push({ k: 'SMTP', v: `${e.host}:${e.port} · ${ENC_LABEL[e.encryption]}` }, { k: t('认证'), v: e.username || t('不认证') },
      { k: t('发件人'), v: e.from_addr }, { k: t('固定收件人'), v: t('{n} 个', { n: e.to.length + e.cc.length }) });
    if (e.skip_verify) { high = true; lines.push(t('已关闭证书校验：无法确认对方是真正的邮件服务器，邮件内容和 SMTP 密码可能被中间人窃听。')); }
    if (e.encryption === 'none') { high = true; lines.push(e.username ? t('加密方式为“不加密”且启用了认证：邮件内容、用户名和密码都以明文在网络上传输。') : t('加密方式为“不加密”：邮件内容以明文在网络上传输。')); }
    if (b.secret) lines.push(t('将更新 SMTP 密码（加密保存，之后不会回显）。'));
    else if (b.clear_secret) lines.push(t('将清除已保存的 SMTP 密码。'));
  }
  if (!lines.length) lines.push(t('保存后立即生效。'));
  return {
    title: editing ? t('保存渠道「{name}」', { name: b.name }) : t('新增渠道「{name}」', { name: b.name }),
    level: high ? 'high' : 'normal', code: 'notify.edit', codeName: t('新建 / 编辑渠道与 @ 规则'),
    lines, details, okText: t('确认保存'),
  };
}

async function save() {
  setMsg('');
  const b = body();
  const cr = await confirmRun(saveConfirm(b), async () => {
    if (editing) await api('PUT', `/api/v1/settings/notify/channels/${editing.id}`, b);
    else await api('POST', '/api/v1/settings/notify/channels', b);
  });
  if (!cr.ok) return;
  els.panel.hidden = true;
  try { await load(); setMsg(t('已保存'), 'info'); } catch (e) { setMsg(e.message); }
}

async function guarded(btn, fn) {
  setMsg('');
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  try { await fn(); } catch (e) { setMsg(e.message); } finally { btn.disabled = false; btn.removeAttribute('aria-busy'); }
}

// 邮件测试发送：逐阶段（连接 / STARTTLS / 认证 / 发送）显示结果。结果里没有密码。
function renderStages(r) {
  const box = els.testResult;
  box.replaceChildren();
  const ul = el('ul', 'nc-stages');
  const stText = { ok: t('通过'), fail: t('失败'), skip: t('跳过') };
  for (const s of r.stages) {
    const li = el('li', 'nc-stage ' + s.status);
    li.append(el('b', '', s.name === 'connect' ? t('连接|stage') : tp(s.label)), el('span', 'nc-st ' + s.status, stText[s.status] || s.status), el('span', 'small', tp(s.detail)));
    ul.append(li);
  }
  box.append(el('h4', 'nc-sub', r.ok ? (r.to ? t('测试邮件已发送（{to}）', { to: r.to }) : t('测试邮件已发送')) : t('测试发送失败')), ul);
}

async function test() {
  els.testResult.replaceChildren();
  await guarded($('nc-test'), async () => {
    const r = await api('POST', '/api/v1/settings/notify/test', simBody());
    if (isEmail(els.type.value)) {
      renderStages(r);
      if (r.ok) setMsg(t('测试邮件已提交给 SMTP 服务器，请到收件箱确认。'), 'info');
      else setMsg(t('发送失败：') + tp(r.error));
      if (logsLoaded) loadLogs(1).catch(() => {});
      return;
    }
    setMsg(t('测试消息已发送，请到机器人所在的群里确认。'), 'info');
    if (logsLoaded) loadLogs(1).catch(() => {});
  });
}

// 从各平台的请求体里取出消息正文
function messageText(platform, payload) {
  if (!payload) return '';
  if (platform === 'dingtalk') return (payload.text || {}).content || '';
  if (platform === 'wecom') return (payload.markdown || payload.text || {}).content || ''; // markdown 消息或文本消息
  if (platform === 'feishu' || platform === 'lark') return (payload.content || {}).text || '';
  if (platform === 'telegram') return telegramText(payload.text || '');
  return payload.text || '';
}

// Telegram 的正文是 HTML（<b> / <pre> / <a>）：预览里去掉标签、还原 &lt; &gt; &amp;，留下群里实际看到的文字
function telegramText(html) {
  return html.replace(/<a href="[^"]*">([^<]*)<\/a>/g, '$1').replace(/<\/?(?:b|pre)>/g, '')
    .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&amp;/g, '&');
}

// 把正文里的 @ 标记换成高亮的文本节点（不使用 innerHTML）
function appendMessage(box, text) {
  const re = /(<@[A-Za-z0-9_.@-]+>|<!subteam\^[A-Z0-9]+>|<!channel>|<!here>|<!everyone>|<at user_id="[^"]*">[^<]*<\/at>|@[^\s，。；：:]+)/g;
  let last = 0;
  for (const m of text.matchAll(re)) {
    if (m.index > last) box.append(document.createTextNode(text.slice(last, m.index)));
    const s = el('span', 'at', m[0].replace(/<at user_id="([^"]*)">([^<]*)<\/at>/, (_, id, n) => '@' + (n || id)));
    box.append(s);
    last = m.index + m[0].length;
  }
  if (last < text.length) box.append(document.createTextNode(text.slice(last)));
}

// 邮件预览的正文：主题、发件人、收件人、纯文本正文
function emailPreviewText(p) {
  const from = p.from_name ? `${p.from_name} <${p.from}>` : p.from;
  const lines = [t('主题：{subject}', { subject: p.subject }), t('发件人：{from}', { from }),
    t('收件人：{list}', { list: (p.to || []).join(', ') || t('（无）') })];
  if ((p.cc || []).length) lines.push(t('抄送：{list}', { list: p.cc.join(', ') }));
  return lines.join('\n') + '\n\n' + p.text;
}

function renderPreview(pv) {
  const box = els.preview;
  box.replaceChildren();
  const email = pv.platform === 'email';
  const wrap = el('div', 'nc-pv');
  const left = el('div');
  const info = { type: labelOf(ctx.types, pv.platform), event: evName(pv.event), time: pv.work_time ? t('工作时间') : t('非工作时间') };
  left.append(el('h4', '', email ? t('邮件效果（{type} · 事件：{event} · {time}）', info) : t('消息效果（{type} · 事件：{event} · {time}）', info)));
  const msg = el('div', 'nc-msg');
  if (email) msg.append(document.createTextNode(emailPreviewText(pv.payload)));
  else appendMessage(msg, messageText(pv.platform, pv.payload));
  left.append(msg);
  const right = el('div');
  right.append(el('h4', '', email ? t('将发送给谁（{n} 个收件人）', { n: pv.mentions.length }) : t('@ 解析（{n} 个目标）', { n: pv.mentions.length })));
  const ul = el('ul');
  if (!pv.mentions.length) ul.append(el('li', 'muted', email ? t('没有可用的收件人') : t('没有 @ 任何人')));
  for (const m of pv.mentions) {
    const note = email ? t('没有邮箱，收不到邮件') : t('文本，不会提醒');
    const li = el('li', m.resolved ? '' : 'warn', tp(m.label) + (m.resolved ? '' : PL + note + PR));
    if (m.note) li.title = tp(m.note);
    ul.append(li);
  }
  right.append(ul);
  if (pv.skipped.length) {
    right.append(el('h4', '', t('未生效的规则')));
    const sk = el('ul');
    for (const s of pv.skipped) {
      const li = el('li', 'warn');
      li.append(el('b', '', tp(s.rule)), document.createTextNode(COLON + tp(s.reason)));
      sk.append(li);
    }
    right.append(sk);
  }
  const det = el('details');
  det.append(el('summary', '', email ? t('邮件内容（JSON）') : (pv.signed ? t('请求体（未含签名）') : t('请求体'))), el('pre', '', JSON.stringify(pv.payload, null, 2)));
  right.append(det);
  wrap.append(left, right);
  box.append(wrap);
}

async function previewMsg() {
  await guarded($('nc-preview-btn'), async () => {
    const b = simBody();
    if (!b.event) b.event = 'pending';
    renderPreview(await api('POST', '/api/v1/settings/notify/preview', b));
  });
}

// ---- 发送记录 ----

function logParams(page) {
  return { channel_id: $('nl-channel').value, event: $('nl-event').value, failed: $('nl-failed').checked ? 1 : '', page, page_size: 20 };
}

async function loadLogs(page = 1) {
  const data = await api('GET', '/api/v1/settings/notify/logs' + qs(logParams(page)));
  const note = $('nl-note');
  note.replaceChildren();
  const body = $('nl-rows');
  if (!data.ready) {
    note.append(el('div', 'alert warn', t('发送记录表未创建，请先执行 deploy/sql/092_notify_log.sql。')));
    emptyRow(body, 7, t('没有发送记录。'));
    $('nl-pager').replaceChildren();
    return;
  }
  if (!data.rows.length) emptyRow(body, 7, t('没有符合条件的发送记录。'));
  else {
    body.replaceChildren();
    for (const r of data.rows) {
      const tr = el('tr', r.ok ? '' : 'lv-err');
      const res = el('td');
      res.append(status(r.ok ? t('成功') : t('失败'), r.ok ? 'passed' : 'rejected'));
      tr.append(el('td', 'small mono', r.time), el('td', '', r.channel_name || '#' + r.channel_id), el('td', '', evName(r.event) + (r.test ? PL + t('测试') + PR : '')), res,
        el('td', 'num', r.cost_ms + ' ms'), el('td', 'num', String(r.mentions)), el('td', 'small', tp(r.error) || '—'));
      body.append(tr);
    }
  }
  pager($('nl-pager'), data, (n) => loadLogs(n).catch((e) => setMsg(e.message)));
}

function initLogFilters() {
  fillSelect($('nl-channel'), [{ id: '', name: t('全部渠道') }, ...ctx.channels.map((c) => ({ id: c.id, name: c.name }))], (x) => x.name, (x) => x.id);
  fillSelect($('nl-event'), [{ value: '', label: t('全部事件') }, ...ctx.events], (x) => shortLabel(x.label), (x) => x.value);
}

function initListFilters() {
  fillSelect($('nc-f-type'), [{ value: '', label: t('全部类型') }, ...ctx.types], (x) => tp(x.label), (x) => x.value);
  fillSelect($('nc-f-group'), [{ id: '', name: t('全部资源组') }, { id: 'global', name: t('全局渠道') }, ...ctx.groups], (x) => x.name, (x) => x.id);
}

async function load() {
  const keepT = $('nc-f-type').value, keepG = $('nc-f-group').value;
  ctx = await api('GET', '/api/v1/settings/notify');
  // 容错：空列表被序列化成 null 时按空数组处理
  for (const k of ['channels', 'groups', 'auth_groups', 'types', 'events']) if (!Array.isArray(ctx[k])) ctx[k] = [];
  for (const c of ctx.channels) {
    for (const k of ['group_ids', 'duty_users', 'events', 'rules']) if (!Array.isArray(c[k])) c[k] = [];
  }
  initListFilters();
  $('nc-f-type').value = keepT;
  $('nc-f-group').value = keepG;
  initLogFilters();
  renderKpis();
  renderList();
}

tabs($('nc-tabs'), (name) => {
  if (name === 'logs' && !logsLoaded) { logsLoaded = true; loadLogs(1).catch((e) => setMsg(e.message)); }
});
$('nl-form').addEventListener('submit', (e) => { e.preventDefault(); loadLogs(1).catch((err) => setMsg(err.message)); });
$('nc-filter').addEventListener('submit', (e) => e.preventDefault());
for (const id of ['nc-f-type', 'nc-f-group']) $(id).addEventListener('change', renderList);
$('nc-f-q').addEventListener('input', renderList);

$('nc-new').addEventListener('click', () => {
  if (!ctx.ready) return setMsg(t('通知渠道表未创建，请先执行 deploy/sql/001_notify_channel.sql。'));
  open(null);
});
$('nc-cancel').addEventListener('click', () => { els.panel.hidden = true; });
$('nc-save').addEventListener('click', save);
$('nc-test').addEventListener('click', test);
$('nc-preview-btn').addEventListener('click', previewMsg);
$('nc-add-rule').addEventListener('click', () => addRuleRow());
els.global.addEventListener('change', syncGroups);
els.type.addEventListener('change', applyType);
em.enc.addEventListener('change', () => {
  const was = Number(em.port.value);
  if (!was || was === DEF_PORT[lastEnc]) em.port.value = DEF_PORT[em.enc.value]; // 端口还是上一种加密方式的默认值就跟着换
  lastEnc = em.enc.value;
  syncEnc();
});
em.verify.addEventListener('change', syncEnc);

load().catch((e) => setMsg(e.message));
