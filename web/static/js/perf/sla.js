/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: sla.js
 * @time: 2026/10/28
 */
// SLA 策略：四个级别各一行，直接在表格里改；升级通知对象矩阵（L1 / L2 / L3 各 @ 谁）；一次保存。下面只读展示级别评估规则。
// 只有“查看配置”权限（没有 perfcfg.sla）时整页只读：输入框禁用，保存按钮由 data-btn 置灰。
import { api, confirmRun, setButtonMeta, toast } from '../common.js';
import { t } from '../i18n.js';
import { $, el, sevBadge, fmtNum, setMsg, applyButtons, loadButtons, can, showReadOnlyNotice } from './util.js';

setButtonMeta({ 'perfcfg.sla': t('修改 SLA 策略') });

const msg = $('ps-msg');
const LABEL = { P0: t('P0-紧急'), P1: t('P1-高'), P2: t('P2-中'), P3: t('P3-低') };
const FIELDS = ['accept_hours', 'resolve_hours', 'l3_after_hours', 'remind_interval_hours', 'max_extend_times'];
const NAMES = { accept_hours: t('受理时限'), resolve_hours: t('解决时限'), l3_after_hours: t('超时后升级总监'), remind_interval_hours: t('重复催办间隔'), max_extend_times: t('最多延期次数') };
const ESC_ROWS = [
  ['L1', t('催办'), t('未受理超过“受理时限”')],
  ['L2', t('升级'), t('超过“解决时限”')],
  ['L3', t('升级总监'), t('超过解决时限 + “超时后升级总监”小时')],
];
const TARGETS = [['owner', t('负责人')], ['tl', t('团队 TL')], ['dba', t('跟进 DBA')], ['director', t('总监')], ['global', t('全局渠道')]];
let policies = [];
let readOnly = false;
let escReady = true;
let lastOpts = null; // 最近一次从服务端取到的数据：“重置”用它还原表单

function input(p, f) {
  const i = el('input', 'num-in');
  i.type = 'number';
  i.min = f === 'max_extend_times' || f === 'l3_after_hours' ? '0' : '1';
  i.value = p[f];
  i.dataset.sev = p.severity;
  i.dataset.field = f;
  i.disabled = readOnly;
  i.setAttribute('aria-label', `${LABEL[p.severity]} ${NAMES[f]}`);
  return i;
}

function renderEscalation(o) {
  const body = $('ps-esc');
  body.replaceChildren();
  const m = o.escalation || {};
  escReady = !!o.escalation_ready;
  for (const [lv, name, when] of ESC_ROWS) {
    const tr = el('tr');
    const first = el('td');
    first.append(el('span', 'badge crit', lv), document.createTextNode(' ' + name));
    tr.append(first, el('td', '', when));
    for (const [key, label] of TARGETS) {
      const td = el('td');
      const c = document.createElement('input');
      c.type = 'checkbox';
      // global（同时发到全局通知渠道）缺省为勾选：这是升级通知一直以来的行为
      c.checked = key === 'global' ? !(m[lv] && m[lv].global === false) : !!(m[lv] && m[lv][key]);
      c.dataset.level = lv;
      c.dataset.target = key;
      c.disabled = readOnly || !escReady || key === 'owner'; // 每一级都必须通知负责人，不能取消
      c.setAttribute('aria-label', t('{lv} 通知{label}', { lv, label }));
      td.append(c);
      tr.append(td);
    }
    body.append(tr);
  }
  $('ps-esc-note').textContent = escReady ? '' : t('升级通知对象暂时只能使用默认值（未执行 060_perf_ext.sql）。');
}

function render(o) {
  lastOpts = o;
  policies = o.policies;
  const body = $('ps-rows');
  body.replaceChildren();
  for (const p of o.policies) {
    const tr = el('tr');
    const sev = el('td');
    sev.append(sevBadge(p.severity, LABEL[p.severity]));
    tr.append(sev);
    for (const f of FIELDS) {
      const td = el('td');
      td.append(input(p, f));
      tr.append(td);
    }
    body.append(tr);
  }
  renderEscalation(o);
  const rules = $('ps-rules');
  rules.replaceChildren();
  const big = (v) => v >= 1e17;
  for (const sev of ['P0', 'P1', 'P2']) {
    const r = o.rules[sev];
    const tr = el('tr');
    if (!r) { const td = el('td', 'muted', t('未配置')); td.colSpan = 3; tr.append(el('td', '', sev), td); rules.append(tr); continue; }
    tr.append(el('td', '', sev), el('td', '', big(r.daily_sec) ? '—' : `≥ ${fmtNum(r.daily_sec)} s`), el('td', '', big(r.share) ? '—' : `≥ ${fmtNum(r.share)}%`),
      el('td', '', big(r.avg_ms) || big(r.per_day) ? '—' : t('≥ {ms} ms 且 ≥ {n} 次', { ms: fmtNum(r.avg_ms), n: fmtNum(r.per_day) })));
    rules.append(tr);
  }
  const p3 = el('tr');
  const td = el('td', '', t('其余'));
  td.colSpan = 3;
  p3.append(el('td', '', 'P3'), td);
  rules.append(p3);
  $('ps-custom').textContent = o.rules_custom ? t('当前使用自定义值') : t('当前使用默认值');
}

// 读出升级矩阵；owner 一列固定为 true。
function readEscalation() {
  const out = {};
  for (const [lv] of ESC_ROWS) out[lv] = { owner: true, tl: false, dba: false, director: false, global: true };
  for (const c of document.querySelectorAll('#ps-esc input[type=checkbox]')) {
    if (c.dataset.target !== 'owner') out[c.dataset.level][c.dataset.target] = c.checked;
  }
  return out;
}

// 升级矩阵规范化：和 readEscalation 同形（owner 恒为 true；global 缺省为 true）
function normEsc(m) {
  const out = {};
  for (const [lv] of ESC_ROWS) {
    const x = (m && m[lv]) || {};
    out[lv] = { owner: true, tl: !!x.tl, dba: !!x.dba, director: !!x.director, global: x.global !== false };
  }
  return out;
}
const escText = (e) => TARGETS.filter(([k]) => e[k]).map(([, label]) => label).join(t('、|sep')) || t('无');

// 与服务端当前值逐项比较，只列出有变化的项
function diffRows(out, esc) {
  const rows = [];
  for (const p of policies) {
    const n = out.find((x) => x.severity === p.severity);
    for (const f of FIELDS) rows.push({ label: `${LABEL[p.severity]} · ${NAMES[f]}`, from: p[f], to: n[f] });
  }
  if (esc) {
    const old = normEsc(lastOpts && lastOpts.escalation);
    for (const [lv, name] of ESC_ROWS) rows.push({ label: t('{lv} {name} · 通知对象', { lv, name }), from: escText(old[lv]), to: escText(esc[lv]) });
  }
  return rows;
}

async function save(button) {
  setMsg(msg, '');
  const out = policies.map((p) => ({ severity: p.severity }));
  for (const i of document.querySelectorAll('#ps-rows input')) {
    const row = out.find((x) => x.severity === i.dataset.sev);
    if (i.value === '' || !Number.isInteger(Number(i.value))) return setMsg(msg, t('{label} 的{name}需要填整数', { label: LABEL[i.dataset.sev], name: NAMES[i.dataset.field] }));
    row[i.dataset.field] = Number(i.value);
  }
  const body = { policies: out };
  if (escReady) body.escalation = readEscalation();
  // 保存前看差异：没有变化不发请求；失败留在弹窗里可重试
  const r = await confirmRun({
    title: t('保存 SLA 策略'), code: 'perfcfg.sla', level: 'high', okText: t('确认保存'),
    lines: [t('SLA 策略对之后新建 / 重新计算时限的工单生效，请确认下面的变更：')],
    rows: diffRows(out, escReady ? body.escalation : null),
  }, () => api('PUT', '/api/v1/perf/config/sla', body));
  if (r.noChange) return toast(t('没有变更'), 'warn');
  if (!r.ok) return;
  setMsg(msg, t('已保存。'), 'info');
  try { render(await api('GET', '/api/v1/perf/config/sla')); } catch (e) { setMsg(msg, e.message); }
  await applyButtons(document);
}

// 重置：丢掉页面上还没保存的修改，恢复成服务端当前的值
function reset() {
  if (!lastOpts) return;
  setMsg(msg, '');
  render(lastOpts);
  toast(t('已重置'), 'ok');
}

async function init() {
  const o = await api('GET', '/api/v1/perf/config/sla');
  const b = await loadButtons();
  readOnly = !!b && !can(b, 'perfcfg.sla');
  if (readOnly) showReadOnlyNotice($('ps-ro'), t('SLA 策略'));
  render(o);
  await applyButtons(document); // 无“修改 SLA 策略”权限时保存按钮置灰
  if (readOnly) $('ps-reset').disabled = true;
}

$('ps-save').addEventListener('click', () => save());
$('ps-reset').addEventListener('click', reset);
init().catch((e) => setMsg(msg, e.message));
