/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: detail.js
 * @time: 2026/10/28
 */
// 性能工单详情：处理进度、影响指标、趋势图（原生 SVG）、SQL 与执行计划、诊断、时间线日志，以及状态流转按钮与弹窗。
// 按钮由服务端按“权限 + 当前状态”给出（detail.actions），这里不做权限判断，服务端会再校验一次。
import { api, riskConfirm, setButtonMeta } from '../common.js';
import { t, tp } from '../i18n.js';
import { $, el, svg, btn, sevBadge, statusBadge, fmtNum, fmtShort, fmtHours, setMsg, fillSelect, openModal, closeModal, wireModals, busy } from './util.js';

const root = $('perf-detail');
const id = root.dataset.id;
const base = `/api/v1/perf/issues/${id}`;
setButtonMeta({ 'perf.close_dup': t('误报关闭') });
const msg = $('pd-msg');
let d = null; // 当前详情
let opts = null; // 选项（仅 DBA 需要：用户、团队）

// 每个动作需要的字段、标题与说明。required 里的字段必填（服务端会再校验）；ask 是备注必填却没填时的提示。
const ACTIONS = {
  accept: { title: t('受理'), fields: ['plan', 'remark'], remark: t('备注（选填）') },
  fix: { title: t('已优化上线，申请验证'), fields: ['remark', 'wf'], required: ['remark'], remark: t('优化方案/上线说明（必填）'), ask: t('请填写优化方案/上线说明') },
  extend: { title: t('申请延期'), fields: ['ext', 'remark'], required: ['ext', 'remark'], remark: t('延期理由（必填）'), ask: t('请填写延期理由') },
  waive: { title: t('申请豁免'), fields: ['remark'], required: ['remark'], remark: t('豁免理由（必填，不少于 10 字）'), ask: t('请填写豁免理由') },
  comment: { title: t('评论'), fields: ['remark'], required: ['remark'], remark: t('评论内容'), ask: t('请填写评论内容') },
  assign: { title: t('指派'), fields: ['assignee', 'team', 'remark'], required: ['assignee'], remark: t('备注（选填）') },
  verify_pass: { title: t('验证通过'), fields: ['remark'], remark: t('备注（选填）') },
  verify_fail: { title: t('验证不通过'), fields: ['remark'], required: ['remark'], remark: t('验证不通过的原因（必填）'), ask: t('请填写验证不通过的原因') },
  extend_approve: { title: t('同意延期'), fields: ['remark'], remark: t('备注（选填）') },
  extend_reject: { title: t('驳回延期'), fields: ['remark'], remark: t('驳回原因（选填）') },
  waive_approve: { title: t('同意豁免'), fields: ['remark'], remark: t('备注（选填）'), tip: t('同意后工单关闭，仅影响升到 P0 才会自动重开。') },
  waive_reject: { title: t('驳回豁免'), fields: ['remark'], remark: t('驳回原因（选填）') },
  severity: { title: t('调整级别'), fields: ['sev', 'remark'], remark: t('备注（选填）'), tip: t('调整后级别锁定，不再自动调整。') },
  side: { title: t('切换处理方'), fields: ['side', 'remark'], remark: t('备注（选填）') },
  close_dup: { title: t('误报关闭'), fields: ['remark'], required: ['remark'], remark: t('原因（必填）'), ask: t('请填写原因'), tip: t('关闭后再出现也不会自动重开。') },
  reopen: { title: t('重开'), fields: ['remark'], remark: t('备注（选填）') },
  diagnose: { title: t('重新诊断'), fields: ['sql'] },
};
const DANGER = new Set(['verify_fail', 'extend_reject', 'waive_reject', 'close_dup']);
const PRIMARY = new Set(['fix', 'verify_pass', 'waive_approve', 'accept']);

let current = null; // 弹窗里正在做的动作

function addDays(dateStr, n) {
  const dt = new Date(dateStr + 'T00:00:00');
  dt.setDate(dt.getDate() + n);
  const p = (x) => String(x).padStart(2, '0');
  return `${dt.getFullYear()}-${p(dt.getMonth() + 1)}-${p(dt.getDate())}`;
}

function openAction(name) {
  const def = ACTIONS[name];
  current = name;
  setMsg($('pa-msg'), '');
  $('pa-title').textContent = def.title;
  document.querySelectorAll('#m-perf-act [data-f]').forEach((l) => { l.hidden = !def.fields.includes(l.dataset.f); });
  $('pa-remark-label').textContent = def.remark || t('备注');
  $('pa-remark').value = '';
  $('pa-sql').value = '';
  $('pa-wf').value = '';
  $('pa-tip').textContent = def.tip || '';
  // 验证弹窗里直接给出修复前后的对比，DBA 不用翻详情页
  const fx = $('pa-effect');
  fx.hidden = !((name === 'verify_pass' || name === 'verify_fail') && d.effect);
  fx.replaceChildren();
  if (!fx.hidden) fx.append(document.createTextNode(effectSummary(d.effect)));
  const plan = $('pa-plan');
  plan.min = d.today;
  plan.max = d.max_plan_date || '';
  plan.value = '';
  $('pa-ext').min = d.resolve_deadline ? addDays(d.resolve_deadline.slice(0, 10), 1) : addDays(d.today, 1);
  $('pa-ext').value = '';
  if (opts) {
    fillSelect($('pa-assignee'), opts.users, (u) => t('{display}（{username}）', { display: u.display || u.username, username: u.username }), (u) => u.id);
    if (d.assignee_id) $('pa-assignee').value = String(d.assignee_id);
    fillSelect($('pa-team'), opts.teams, (tm) => tm.name, (tm) => tm.id, ['0', t('不变')]);
    fillSelect($('pa-sev'), opts.severities, (s) => tp(s.label), (s) => s.value);
    $('pa-sev').value = d.severity;
    fillSelect($('pa-side'), opts.owner_sides, (s) => tp(s.label), (s) => s.value);
    $('pa-side').value = d.owner_side;
  }
  $('pa-ok').className = 'btn ' + (DANGER.has(name) ? 'danger' : 'primary');
  openModal('m-perf-act');
}

async function submitAction(button) {
  const def = ACTIONS[current];
  const need = def.required || [];
  const remark = $('pa-remark').value.trim();
  if (need.includes('remark') && !remark) return setMsg($('pa-msg'), def.ask || t('请填写内容'));
  if (need.includes('ext') && !$('pa-ext').value) return setMsg($('pa-msg'), t('请选择延期到哪一天'));
  if (current === 'accept' && !$('pa-plan').value) return setMsg($('pa-msg'), t('请选择承诺完成日期'));
  // 误报关闭是两步确认：填完原因后，再单独说明后果并要求键入工单号；第二步取消则回到原因弹窗，不发请求
  if (current === 'close_dup') {
    const c = await riskConfirm({
      title: t('误报关闭工单 #{id}', { id }), level: 'high', code: 'perf.close_dup', okText: t('确认关闭'),
      lines: [t('关闭后，同一 SQL（同实例 + 库 + 指纹）再次出现也不会重开工单。')],
      details: [{ k: t('工单'), v: '#' + id }, { k: t('实例 / 库'), v: `${d.instance} / ${d.db_name || '-'}` }, { k: t('原因'), v: remark }],
      typed: '#' + id, typedLabel: t('请输入工单号确认：'),
    });
    if (!c.ok) return;
  }
  await busy(button, async () => {
    try {
      if (current === 'diagnose') {
        await api('POST', `${base}/diagnose`, { sql: $('pa-sql').value });
      } else {
        await api('POST', `${base}/actions`, {
          action: current, remark, plan_finish_date: $('pa-plan').value, extend_date: $('pa-ext').value,
          assignee_id: Number($('pa-assignee').value) || 0, team_id: Number($('pa-team').value) || 0, severity: $('pa-sev').value, owner_side: $('pa-side').value,
          workflow_id: current === 'fix' ? Number($('pa-wf').value) || 0 : 0,
        });
      }
      closeModal('m-perf-act');
      setMsg(msg, t('操作成功'), 'info');
      await load();
    } catch (e) {
      setMsg($('pa-msg'), e.message);
      if (e.status === 409) await load().catch(() => {}); // 别人刚改过：刷新后用户再决定
    }
  });
}

// ---- 渲染 ----

function lines(text, strip) {
  return (text || '').split('\n').map((s) => s.replace(strip, '').trim()).filter(Boolean);
}

function renderHead() {
  const sev = $('pd-sev');
  sev.replaceChildren(sevBadge(d.severity, tp(d.severity_label)));
  $('pd-status').replaceChildren(statusBadge(d.status, d.overdue ? t('{status}（已超时）', { status: tp(d.status_label) }) : tp(d.status_label)));
  $('pd-sub').textContent = `${d.instance} / ${d.db_name || '-'} · ${tp(d.category_label)} · ${tp(d.owner_side_label)}` + (d.severity_locked ? ' · ' + t('级别已锁定') : '');
  const sla = $('pd-sla');
  sla.replaceChildren();
  if (d.accept_deadline || d.resolve_deadline) {
    sla.append(document.createTextNode(t('受理截止 {accept} ｜ 解决截止 ', { accept: d.accept_deadline || '-' })), el('b', '', d.resolve_deadline || '-'));
    if (d.left_hours !== null && d.left_hours !== undefined) {
      sla.append(document.createTextNode(' '), el('span', d.overdue ? 'crit' : '', d.overdue ? t('（已超时 {time}）', { time: fmtHours(d.left_hours) }) : t('（剩余 {time}）', { time: fmtHours(d.left_hours) })));
    } else if (d.status === 'fixed' || d.status === 'waive_pending') {
      sla.append(document.createTextNode(' '), el('span', 'muted', t('（待验证/审批中，SLA 暂停计时）')));
    }
    if (d.escalation_level > 0) sla.append(document.createTextNode(' '), el('span', 'badge crit', t('已升级 L{level}，催办 {count} 次', { level: d.escalation_level, count: d.remind_count })));
  }
  const pend = $('pd-pending');
  pend.replaceChildren();
  if (d.pending_extend_date) pend.append(el('div', 'alert warn', t('待审批申请：申请延期至 {date}，理由：{reason}', { date: d.pending_extend_date, reason: d.pending_reason })));
  else if (d.status === 'waive_pending') pend.append(el('div', 'alert warn', t('待审批申请：豁免理由：{reason}', { reason: d.pending_reason })));
  if (d.status === 'closed') pend.append(el('div', 'alert info', t('已关闭：{reason}（{time}）', { reason: tp(d.close_reason_label || ''), time: d.closed_at })));
}

function renderActions() {
  const box = $('pd-actions');
  box.replaceChildren();
  let sepDone = false;
  for (const a of d.actions) {
    if (a.dba && !sepDone) {
      sepDone = true;
      if (box.children.length) box.append(el('span', 'muted', t('｜ DBA：')));
    }
    box.append(btn(tp(a.label), DANGER.has(a.name) ? 'danger' : PRIMARY.has(a.name) ? 'primary' : '', () => openAction(a.name)));
  }
  if (!d.actions.length) box.append(el('span', 'muted', t('没有可执行的操作')));
}

function renderKpis() {
  const k = $('pd-kpis');
  k.replaceChildren();
  const items = [
    [fmtNum(d.impact_score) + ' s', t('日均浪费 DB 时间')], [d.load_share.toFixed(1) + '%', t('占该实例慢 SQL 总耗时')], [fmtNum(d.per_day_count), t('日均执行次数')],
    [fmtNum(d.avg_time_ms) + ' ms', t('平均耗时 · 最大 {max} ms', { max: fmtNum(d.max_time_ms) })], [fmtShort(d.rows_examined_avg), t('平均扫描行')], [fmtShort(d.rows_sent_avg), t('平均返回行')],
  ];
  for (const [v, l] of items) {
    const c = el('div');
    c.append(el('b', '', v), el('span', '', l));
    k.append(c);
  }
}

// 修复效果：与“标记修复”时记录的基线对比（修复当天与今天的数据不计）。
const pct = (v) => (v >= 0 ? t('下降 {pct}%', { pct: Math.abs(v).toFixed(1) }) : t('上升 {pct}%', { pct: Math.abs(v).toFixed(1) }));

// 返回 { base: 修复前基线, rest: 修复后的观察与结论 }；弹窗里两段连着显示，效果卡片里只显示后一段。
function effectParts(e) {
  const base = t('修复前基线：平均 {avg} ms，日均浪费 {daily} s。', { avg: fmtNum(e.baseline_avg_ms), daily: fmtNum(e.baseline_daily_sec) });
  if (!e.observed) return { base, rest: t('修复后的观察数据要从修复次日起累计（已观察 0 天，自动验证需要 {n} 天）。', { n: e.need_days }) };
  const verdict = e.pass ? t('已满足自动验证条件。') : t('暂未达到自动验证条件（需下降 ≥ {pct}%）。', { pct: e.need_pct });
  if (e.absent) return { base, rest: t('修复后已观察 {days} 天，没有再出现。', { days: e.days }) + verdict };
  return {
    base,
    rest: t('修复后已观察 {days} 天：平均 {avg} ms（{avgPct}），日均浪费 {daily} s（{dailyPct}）。', {
      days: e.days, avg: fmtNum(e.after_avg_ms, 1), avgPct: pct(e.avg_improve_pct), daily: fmtNum(e.after_daily_sec, 1), dailyPct: pct(e.daily_improve_pct),
    }) + verdict,
  };
}

function effectSummary(e) {
  const p = effectParts(e);
  return p.base + p.rest;
}

function renderEffect() {
  const card = $('pd-effect-card');
  const e = d.effect;
  card.hidden = !e;
  if (!e) return;
  $('pd-effect-sub').textContent = e.observed ? t('已观察 {days} 天', { days: e.days }) : t('还没有观察数据');
  const box = $('pd-effect');
  box.replaceChildren();
  const cell = (v, l, cls) => {
    const c = el('div');
    c.append(el('b', cls || '', v), el('span', '', l));
    box.append(c);
  };
  cell(fmtNum(e.baseline_avg_ms) + ' ms', t('修复前平均耗时'));
  cell(e.observed ? (e.absent ? t('未再出现') : fmtNum(e.after_avg_ms, 1) + ' ms') : '—', e.observed && !e.absent ? t('修复后平均耗时（{pct}）', { pct: pct(e.avg_improve_pct) }) : t('修复后平均耗时'), e.observed && e.avg_improve_pct >= e.need_pct ? 'ok' : '');
  cell(fmtNum(e.baseline_daily_sec) + ' s', t('修复前日均浪费 DB 时间'));
  cell(e.observed ? (e.absent ? '0 s' : fmtNum(e.after_daily_sec, 1) + ' s') : '—', e.observed && !e.absent ? t('修复后日均浪费（{pct}）', { pct: pct(e.daily_improve_pct) }) : t('修复后日均浪费'), e.observed && e.daily_improve_pct >= e.need_pct ? 'ok' : '');
  $('pd-effect-note').textContent = effectParts(e).rest;
}

function renderInfo() {
  const info = $('pd-info');
  info.replaceChildren();
  const rows = [
    [t('团队'), d.team || '-'], [t('负责人'), d.assignee || t('待分派')], [t('跟进 DBA'), d.dba || '-'], [t('承诺完成日'), d.plan_finish_date || '—'],
    [t('受理时间'), d.accepted_at || '—'], [t('标记修复'), d.fixed_at ? (d.fix_desc ? t('{time}：{desc}', { time: d.fixed_at, desc: d.fix_desc }) : d.fixed_at) : '—'],
    [t('修复前基线'), d.baseline_avg_ms ? t('平均 {avg}ms，日均 {daily}s', { avg: fmtNum(d.baseline_avg_ms), daily: fmtNum(d.baseline_daily_sec) }) : '—'],
    [t('延期 / 重开'), t('延期 {used}/{max} 次，重开 {reopen} 次', { used: d.extend_times, max: d.max_extend_times, reopen: d.reopen_count })],
    [t('创建'), t('{time}（{source}）', { time: d.create_time, source: d.source === 'collector' ? t('自动采集') : d.source === 'slowlog' ? t('慢日志页面') : t('手工') })],
  ];
  for (const [k, v] of rows) info.append(el('dt', '', k), el('dd', '', v));
  if (d.workflow_id) { // 标记修复时关联的 SQL 上线工单
    const a = el('a', '', '#' + d.workflow_id);
    a.href = '/sqlworkflow/' + d.workflow_id;
    const dd = el('dd');
    dd.append(a);
    info.append(el('dt', '', t('关联上线工单')), dd);
  }
}

function renderDiag() {
  $('pd-diag-title').textContent = t('诊断：{category} → {side}', { category: tp(d.category_label), side: tp(d.owner_side_label) });
  const ul = $('pd-diag');
  ul.replaceChildren();
  for (const line of lines(d.diagnosis, /^-\s*/)) ul.append(el('li', '', tp(line)));
  const ol = $('pd-suggest');
  ol.replaceChildren();
  for (const line of lines(d.suggestion, /^\d+\.\s*/)) ol.append(el('li', '', tp(line)));
  if (!ol.children.length) ol.append(el('li', 'muted', t('暂无')));
  $('pd-ddl-box').hidden = !d.suggested_ddl;
  $('pd-ddl').textContent = d.suggested_ddl || '';
}

function renderSQL() {
  $('pd-sql').textContent = d.sample_sql;
  $('pd-fp').textContent = t('指纹：{fp}', { fp: d.fingerprint_full });
  $('pd-seen').textContent = t('涉及表：{tables} ｜ 首次 {first} ｜ 最近 {last}', { tables: d.tables || '-', first: (d.first_seen || '').slice(0, 10), last: (d.last_seen || '').slice(0, 10) });
  const card = $('pd-explain-card');
  card.hidden = !d.explain.length;
  const tbl = $('pd-explain');
  tbl.replaceChildren();
  if (!d.explain.length) return;
  const head = el('tr');
  for (const c of d.explain_cols) head.append(el('th', '', c));
  const th = el('thead');
  th.append(head);
  const tb = el('tbody');
  for (const r of d.explain) {
    const tr = el('tr');
    for (const c of d.explain_cols) tr.append(el('td', ['rows', 'filtered'].includes(c) ? 'num' : 'mono', r[c] === null || r[c] === undefined ? '' : String(r[c])));
    tb.append(tr);
  }
  tbl.append(th, tb);
}

function renderLog() {
  const ul = $('pd-log');
  ul.replaceChildren();
  for (const l of d.logs) {
    const li = el('li');
    const who = tp(l.operator_display || l.operator);
    li.append(el('time', '', l.time), el('b', '', tp(l.action_name)),
      document.createTextNode(l.content ? t('（{who}）：{content}', { who, content: tp(l.content) }) : t('（{who}）', { who })));
    ul.append(li);
  }
  if (!d.logs.length) ul.append(el('li', 'muted', t('暂无记录')));
}

// 趋势图：柱 = 每日总耗时(s)，线 = 平均耗时(ms)，虚线 = 标记修复的日期
function renderTrend(trend) {
  const box = $('pd-trend');
  box.replaceChildren();
  const pts = trend.points;
  if (!pts.length) { box.append(el('p', 'muted', t('还没有按天的指标。'))); return; }
  const W = 520, H = 130, padL = 10, padR = 10, top = 16, bottom = 14;
  const s = svg('svg', { viewBox: `0 0 ${W} ${H}`, class: 'trend', role: 'img', 'aria-label': t('近 30 天耗时趋势') });
  const step = (W - padL - padR) / pts.length;
  const bw = Math.max(2, Math.min(14, step * 0.6));
  const maxT = Math.max(...pts.map((p) => p.total_time), 1);
  const maxA = Math.max(...pts.map((p) => p.avg_ms), 1);
  const bars = svg('g', { class: 'bars' });
  const line = [];
  pts.forEach((p, i) => {
    const x = padL + i * step + (step - bw) / 2;
    const h = ((H - top - bottom) * p.total_time) / maxT;
    const bar = svg('rect', { x: x.toFixed(1), y: (H - bottom - h).toFixed(1), width: bw.toFixed(1), height: Math.max(h, 0.5).toFixed(1) });
    bar.append(svg('title', {}, t('{date}：总耗时 {total}s，平均 {avg}ms，{count} 次', { date: p.date, total: fmtNum(p.total_time, 1), avg: fmtNum(p.avg_ms, 1), count: fmtNum(p.exec_count) })));
    bars.append(bar);
    line.push(`${(x + bw / 2).toFixed(1)},${(H - bottom - ((H - top - bottom) * p.avg_ms) / maxA).toFixed(1)}`);
  });
  s.append(bars, svg('polyline', { class: 'line', points: line.join(' ') }));
  const fi = pts.findIndex((p) => p.date === trend.fixed_at);
  if (fi >= 0) {
    const x = padL + fi * step + step / 2;
    s.append(svg('line', { x1: x, y1: 6, x2: x, y2: H - bottom, stroke: 'var(--dt-ok)', 'stroke-dasharray': '4' }), svg('text', { x: x + 4, y: 14, fill: 'var(--dt-ok)', 'font-size': 10 }, t('标记修复')));
  }
  s.append(svg('text', { x: padL, y: H - 2, fill: 'var(--dt-muted)', 'font-size': 9 }, pts[0].date), svg('text', { x: W - padR, y: H - 2, fill: 'var(--dt-muted)', 'font-size': 9, 'text-anchor': 'end' }, pts[pts.length - 1].date));
  box.append(s);
}

async function load() {
  d = await api('GET', base);
  if (d.is_dba && !opts) opts = await api('GET', '/api/v1/perf/options');
  renderHead(); renderActions(); renderEffect(); renderKpis(); renderSQL(); renderDiag(); renderInfo(); renderLog();
  api('GET', `${base}/trend?days=30`).then(renderTrend).catch(() => {});
}

wireModals();
$('pa-ok').addEventListener('click', (e) => submitAction(e.currentTarget));
$('pd-copy').addEventListener('click', async (e) => {
  const b = e.currentTarget;
  try { await navigator.clipboard.writeText($('pd-sql').textContent); b.textContent = t('已复制'); } catch (_) { b.textContent = t('复制失败'); }
  setTimeout(() => { b.textContent = t('复制'); }, 1500);
});
load().catch((e) => {
  setMsg(msg, e.message);
  $('pd-sub').textContent = '';
});
