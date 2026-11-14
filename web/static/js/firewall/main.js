/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: main.js
 * @time: 2026/10/28
 */
// 系统管理 › SQL 防火墙（仅超级用户）：规则 / 语句测试台 / 命中记录 / 例外名单 / 规则模板。
// 所有来自服务端的文字都用 textContent 写入。
import { api, confirmRun, riskConfirm, toast } from '../common.js';
import { t, isZh } from '../i18n.js';
import {
  h, clear, $, SCENES, ACTIONS, condChip, actionBadge, resultBadge, badge, scopeText, fmtNum, highlightNodes, B, C, rich, GAP,
} from './vocab.js';
import { openEditor } from './editor.js';
import { openDrawer } from './drawer.js';
import { initBench } from './bench.js';

const BASE = '/api/v1/settings/firewall';
const TABS = ['rules', 'bench', 'hits', 'exc', 'tpl'];

const S = {
  meta: null, ov: null, rules: [], ready: true, missing: [], tab: 'rules',
  f: { q: '', action: '', scene: '', state: '' },
  hits: { items: [], total: 0, page: 1, size: 20, loaded: false },
  excs: [], tpls: [], bench: null, loadError: '',
};

// ---- 加载 ----
async function loadOverview() { S.ov = await api('GET', BASE + '/overview'); }
async function loadRules() {
  const r = await api('GET', BASE + '/rules');
  S.rules = r.rules || [];
  S.ready = r.ready;
  S.missing = r.missing || [];
}
async function loadExceptions() { S.excs = await api('GET', BASE + '/exceptions'); }
async function loadTemplates() { S.tpls = await api('GET', BASE + '/templates'); }

async function refresh() {
  try {
    await Promise.all([loadOverview(), loadRules(), loadExceptions(), loadTemplates()]);
  } catch (e) {
    toast(t('刷新失败：{msg}', { msg: e.message }), 'err');
  }
  renderAll();
}

function renderAll() {
  renderHead();
  renderAlerts();
  renderStats();
  renderExplainNote();
  renderRules();
  renderExceptions();
  renderTemplates();
  renderCounts();
  fillRuleSelect();
}

// ---- 顶部：全局模式 / 总开关 ----
function renderHead() {
  const ov = S.ov;
  document.querySelectorAll('#fw-gmode button').forEach((b) => b.setAttribute('aria-pressed', String(ov && b.dataset.m === ov.mode)));
  const sw = $('fw-switch');
  const on = !ov || ov.enabled;
  sw.textContent = on ? t('总开关：已开启') : t('总开关：已关闭');
  sw.classList.toggle('danger', !on);
  $('fw-b-new').disabled = !S.ready;
}

function renderAlerts() {
  const box = clear($('fw-alerts'));
  const ov = S.ov;
  if (S.loadError) {
    box.append(h('div', { class: 'alert err', role: 'alert', text: S.loadError }));
    return;
  }
  if (!S.ready) {
    box.append(h('div', { class: 'alert warn' }, ...rich(t('{title}（{tables}）：请先执行 {sql}，再刷新本页。',
      { title: B(t('防火墙的表还没建')), tables: S.missing.join(isZh ? '、' : ', '), sql: C('deploy/sql/240_firewall.sql') }))));
  }
  if (ov && !ov.enabled) {
    box.append(h('div', { class: 'alert err fw-alert-act' }, h('span', null, ...rich(t('{title}所有规则暂停执行，也不再记录命中。', { title: B(t('SQL 防火墙总开关已关闭：')) }))),
    h('button', { class: 'btn', type: 'button', text: t('重新开启…'), onclick: toggleSwitch })));
  }
  if (ov && ov.mode === 'observe') {
    box.append(h('div', { class: 'alert warn fw-alert-act' }, h('span', null, ...rich(t('{title}所有规则只记录命中，不拦截任何语句。', { title: B(t('全局观察模式：')) }))),
    h('button', { class: 'btn', type: 'button', text: t('切到拦截…'), onclick: () => setGlobalMode('enforce') })));
  }
  const bad = S.rules.filter((r) => r.invalid);
  if (bad.length) {
    box.append(h('div', { class: 'alert warn' }, h('b', { text: t('有 {n} 条规则无法通过校验，不会生效：', { n: bad.length }) }),
      GAP + bad.map((r) => r.name).join(isZh ? '、' : ', ')));
  }
  const off = ((S.meta && S.meta.scenes) || []).filter((s) => !s.active).map((s) => s.label);
  if (off.length) {
    box.append(h('div', { class: 'alert info', text: t('「{scenes}」场景尚未接入，规则可先配置。', { scenes: off.join(isZh ? '」「' : ', ') }) }));
  }
}

function renderStats() {
  const ov = S.ov;
  const box = clear($('fw-stats'));
  if (!ov) return;
  const cards = [
    [t('今日命中'), fmtNum(ov.hits_today), t('涉及 {users} 位用户 · {rules} 条规则', { users: fmtNum(ov.users_today), rules: fmtNum(ov.rules_hit_today) }), ''],
    [t('已拦截'), fmtNum(ov.blocked_today), t('要求原因 {reason} · 限行 / 脱敏 {tighten} · 告警 {warn}', { reason: fmtNum(ov.reason_today), tighten: fmtNum(ov.tighten_today), warn: fmtNum(ov.warn_today) }), ov.blocked_today ? 'crit' : ''],
    [t('观察命中'), fmtNum(ov.observe_today), ov.would_deny_today ? t('若全部生效，会多拦 {n} 条', { n: fmtNum(ov.would_deny_today) }) : '', ov.observe_today ? 'warn' : ''],
    [t('启用中的规则'), `${ov.rules_enabled} / ${ov.rules_total}`, t('{n} 条仍在观察期', { n: ov.rules_observing }), 'ok'],
    [t('长期零命中'), fmtNum(ov.rules_idle), t('启用超 7 天、近 7 天无命中'), ov.rules_idle ? 'warn' : ''],
  ];
  for (const [k, v, d, cls] of cards) {
    box.append(h('div', { class: 'fw-stat ' + cls }, h('div', { class: 'k', text: k }), h('div', { class: 'v', text: v }), h('div', { class: 'd', text: d })));
  }
}

function renderExplainNote() {
  const box = $('fw-explain');
  const ov = S.ov;
  const ex = ov && ov.explain;
  const parts = [];
  if (ex) {
    const avg = ex.calls ? Math.round(ex.total_ms / ex.calls) : 0;
    parts.push(t('EXPLAIN（本进程）：下发 {calls} 次，失败 {failures}（超时 {timeouts}），缓存命中 {hits}，平均 {avg} 毫秒，时限 {limit} 毫秒',
      { calls: fmtNum(ex.calls), failures: fmtNum(ex.failures), timeouts: fmtNum(ex.timeouts), hits: fmtNum(ex.cache_hits), avg, limit: fmtNum(ov.explain_timeout_ms) }));
    if ((ex.open || []).length) parts.push(t('有 {n} 个实例因连续失败暂停了 EXPLAIN（30 秒）：这些实例上的扫描行数条件暂按“未评估”处理', { n: ex.open.length }));
  }
  if (ov && ov.slow_rules) parts.push(t('慢规则 {n} 条', { n: ov.slow_rules }));
  if (ov && ov.blind_rules) parts.push(t('未评估占比偏高的规则 {n} 条', { n: ov.blind_rules }));
  box.textContent = parts.join(' · ');
  box.hidden = parts.length === 0;
}

function renderCounts() {
  $('fw-n-rules').textContent = S.rules.length ? String(S.rules.length) : '';
  $('fw-n-exc').textContent = S.excs.filter((e) => !e.expired).length ? String(S.excs.filter((e) => !e.expired).length) : '';
  $('fw-n-hits').textContent = S.hits.loaded ? fmtNum(S.hits.total) : '';
}

async function setGlobalMode(mode) {
  if (S.ov && S.ov.mode === mode) return;
  const toObserve = mode === 'observe';
  const r = await confirmRun({
    title: toObserve ? t('切换到全局观察模式') : t('切换到拦截模式'), level: toObserve ? 'high' : 'normal', okText: toObserve ? t('切到观察') : t('切到拦截'),
    lines: toObserve
      ? [t('所有规则只记录命中，不再拦截任何语句（包括已经“生效”的规则）。'), t('适合新规则上线前评估；评估完记得切回拦截。')]
      : [t('“生效”状态的规则开始执行动作：拒绝、要求原因、限行、强制脱敏。'), t('观察状态的规则仍只记录。')],
  }, () => api('PUT', BASE + '/mode', { mode }));
  if (r.ok) { toast(toObserve ? t('已切到全局观察模式：只记录，不拦截') : t('已切到拦截模式')); await refresh(); }
}

async function toggleSwitch() {
  const on = !S.ov || S.ov.enabled;
  if (on) {
    const r = await confirmRun({
      title: t('关闭 SQL 防火墙总开关'), level: 'high', okText: t('关闭|turnoff'),
      lines: [t('关闭后所有规则暂停执行，也不再记录命中，等于撤掉这层防护。'), t('库表权限、只读会话、数据脱敏不受影响。这是高危操作，会写入系统审计。')],
      reason: { label: t('关闭原因'), required: true, min: 4, max: 200, placeholder: t('如：排查误拦截，临时关闭') },
    }, (reason) => api('PUT', BASE + '/switch', { enabled: false, reason }));
    if (r.ok) { toast(t('已关闭 SQL 防火墙总开关'), 'warn'); await refresh(); }
  } else {
    const r = await confirmRun({ title: t('开启 SQL 防火墙总开关'), lines: [t('开启后规则恢复执行，并重新记录命中。')], okText: t('开启|turnon') },
      () => api('PUT', BASE + '/switch', { enabled: true, reason: '' }));
    if (r.ok) { toast(t('已开启 SQL 防火墙总开关')); await refresh(); }
  }
}

// ---- 标签页 ----
function setTab(t, { push = true } = {}) {
  if (!TABS.includes(t)) t = 'rules';
  S.tab = t;
  document.querySelectorAll('#fw-tabs button').forEach((b) => b.setAttribute('aria-selected', String(b.dataset.t === t)));
  for (const x of TABS) $('fw-p-' + x).hidden = x !== t;
  if (push) { try { history.replaceState(null, '', '#' + t); } catch (_) { /* 忽略 */ } }
  if (t === 'hits' && !S.hits.loaded) loadHits(1);
}

// ---- 规则 ----
function pendingTypes() { return (S.meta && S.meta.pending_conditions) || []; }

function filteredRules() {
  const f = S.f;
  const q = f.q.trim().toLowerCase();
  return S.rules.filter((r) => {
    if (q && !(r.name + ' ' + r.description).toLowerCase().includes(q)) return false;
    if (f.action && r.action !== f.action) return false;
    if (f.scene && !r.scenes.includes(f.scene)) return false;
    if (f.state === 'on' && !r.enabled) return false;
    if (f.state === 'off' && r.enabled) return false;
    if (f.state === 'observe' && r.mode !== 'observe') return false;
    return true;
  });
}

function sparkNode(arr) {
  const max = Math.max(1, ...arr);
  const box = h('span', { class: 'fw-spark', title: t('近 7 天每天的命中次数：{counts}', { counts: arr.join(' / ') }) });
  for (const n of arr) {
    const i = document.createElement('i');
    i.style.height = Math.max(2, Math.round((n / max) * 20)) + 'px';
    box.append(i);
  }
  return box;
}

function statusCell(r) {
  const g = S.ov && S.ov.mode === 'observe';
  const cell = h('td');
  if (r.invalid) {
    cell.append(badge(t('无效，不会生效'), 'crit', r.invalid));
    return cell;
  }
  if (g && r.mode === 'enforce') cell.append(badge(t('观察中（全局）'), 'warn'));
  else if (r.mode === 'observe') cell.append(badge(r.observe_days ? t('观察中 · 第 {n} 天', { n: r.observe_days }) : t('观察中'), 'warn'));
  else cell.append(badge(t('已生效'), 'ok'));
  cell.append(h('div', null, h('button', { class: 'fw-link', type: 'button', onclick: () => switchRuleMode(r, r.mode === 'observe' ? 'enforce' : 'observe'),
    text: r.mode === 'observe' ? t('切到生效…') : t('改为观察…') })));
  return cell;
}

function renderRules() {
  const tbl = clear($('fw-rules'));
  tbl.append(h('thead', null, h('tr', null, [t('优先级'), t('规则 / 命中条件'), t('适用范围'), t('动作'), t('状态'), t('近 7 天命中'), t('启用'), t('操作')].map((x) => h('th', { text: x })))));
  const body = h('tbody');
  const list = filteredRules();
  if (!S.ready) {
    body.append(h('tr', null, h('td', { class: 'empty', colspan: '8', text: t('防火墙的表还没建') })));
  } else if (!list.length) {
    body.append(h('tr', null, h('td', { class: 'empty', colspan: '8', text: S.rules.length ? t('没有符合筛选条件的规则') : t('还没有规则') })));
  }
  for (const r of S.ready ? list : []) {
    const sc = scopeText(r, S.meta);
    const conds = [];
    r.conditions.forEach((c, i) => {
      if (i) conds.push(h('span', { class: 'fw-join', text: r.match === 'all' ? t('且') : t('或') }));
      conds.push(condChip(c, pendingTypes()));
    });
    const sw = h('input', { type: 'checkbox', checked: r.enabled, 'aria-label': t('启用 {name}', { name: r.name }), disabled: !!r.invalid && !r.enabled,
      onchange: () => toggleRule(r, sw) });
    body.append(h('tr', { class: r.enabled ? '' : 'off', 'data-id': String(r.id) },
      h('td', { class: 'mono', text: String(r.priority) }),
      h('td', null, h('div', { class: 'fw-rname' }, r.name,
        r.slow ? badge(t('慢 · 平均 {ms} 毫秒', { ms: Math.round(r.avg_ms) }), 'warn', t('共 {n} 次，最慢 {ms} 毫秒', { n: fmtNum(r.evals), ms: Math.round(r.max_ms) })) : null,
        r.blind ? badge(t('未评估 {pct}%', { pct: r.unknown_pct }), 'warn') : null),
      r.description ? h('div', { class: 'muted small', text: r.description }) : null,
        h('div', { class: 'fw-cond' }, conds)),
      h('td', null, h('div', { text: sc.scenes }), h('div', { class: 'muted small', text: sc.sub })),
      h('td', null, actionBadge(r.action, r.action_arg)),
      statusCell(r),
      h('td', null, h('b', { class: 'mono', text: fmtNum(r.hits7) }), sparkNode(r.spark || [])),
      h('td', null, h('label', { class: 'fw-sw' }, sw, h('b'))),
      h('td', null,
        h('button', { class: 'fw-link', type: 'button', text: t('编辑'), onclick: () => editRule(r) }),
        h('button', { class: 'fw-link', type: 'button', text: t('回放'), onclick: () => editRule(r, true) }),
        h('button', { class: 'fw-link danger', type: 'button', text: t('删除'), onclick: () => deleteRule(r) }))));
  }
  tbl.append(body);
}

function editRule(r, replay = false) {
  openEditor({ meta: S.meta, rule: r, replay, onSaved: onRuleSaved });
}

function newRule(draft) {
  openEditor({ meta: S.meta, draft, onSaved: onRuleSaved });
}

async function onRuleSaved(rule, isNew) {
  await refresh();
  const live = rule.mode === 'enforce' && rule.enabled;
  if (isNew) toast(live ? t('规则已创建，立即生效') : t('规则已创建（观察模式，不会拦截）'));
  else toast(live ? t('规则已保存，立即生效') : t('规则已保存（观察模式，不会拦截）'));
}

async function toggleRule(r, sw) {
  const want = sw.checked;
  sw.disabled = true;
  try {
    await api('POST', `${BASE}/rules/${r.id}/toggle`, { enabled: want });
    toast(want ? t('已启用「{name}」', { name: r.name }) : t('已停用「{name}」', { name: r.name }));
    await refresh();
  } catch (e) {
    sw.checked = !want;
    sw.disabled = false;
    toast(e.message, 'err');
  }
}

async function switchRuleMode(r, to) {
  const act = ACTIONS[r.action] ? ACTIONS[r.action].t : r.action;
  const lines = to === 'enforce'
    ? [t('规则「{name}」将开始对命中的语句执行「{action}」。', { name: r.name, action: act }),
      r.hits7 ? t('近 7 天它在观察期间命中了 {n} 次：建议先到“命中记录”确认没有误伤。', { n: r.hits7 }) : t('近 7 天没有任何命中记录（没有观察数据），建议先观察一段时间再生效。')]
    : [t('规则「{name}」改回观察后只记录命中，不再执行「{action}」。', { name: r.name, action: act })];
  const res = await confirmRun({
    title: to === 'enforce' ? t('把规则切到生效') : t('把规则改回观察'), level: to === 'enforce' ? 'high' : 'normal',
    okText: to === 'enforce' ? t('切到生效') : t('改为观察'), lines,
  }, () => api('POST', `${BASE}/rules/${r.id}/mode`, { mode: to }));
  if (!res.ok) return;
  await refresh();
  const w = res.result && res.result.warning;
  toast(to === 'enforce' ? t('规则已生效') : t('规则已改为观察'), w ? 'warn' : 'ok');
  if (w) toast(w, 'warn');
}

async function deleteRule(r) {
  const nexc = S.excs.filter((e) => e.rule_id === r.id && !e.expired).length;
  const res = await confirmRun({
    title: t('删除规则'), level: 'high', okText: t('删除'), danger: true,
    lines: [nexc ? t('删除后这条规则不再生效；已有的命中记录会保留。它的 {n} 条例外会一并删除。', { n: nexc }) : t('删除后这条规则不再生效；已有的命中记录会保留。')],
    details: [{ k: t('规则'), v: r.name }, { k: t('近 7 天命中'), v: String(r.hits7) }, { k: t('当前状态'), v: r.mode === 'observe' ? t('观察中') : t('已生效') }],
  }, () => api('DELETE', `${BASE}/rules/${r.id}`));
  if (res.ok) { toast(t('已删除「{name}」', { name: r.name })); await refresh(); }
}

// ---- 命中记录 ----
function hitQuery(page) {
  const p = new URLSearchParams();
  const add = (k, v) => { if (v) p.set(k, v); };
  add('q', $('fw-h-q').value.trim());
  add('rule_id', $('fw-h-rule').value);
  add('result', $('fw-h-result').value);
  add('scene', $('fw-h-scene').value);
  add('days', $('fw-h-days').value);
  p.set('page', String(page));
  p.set('size', String(S.hits.size));
  return p.toString();
}

async function loadHits(page = 1) {
  const tbl = $('fw-hits');
  try {
    const r = await api('GET', BASE + '/hits?' + hitQuery(page));
    S.hits = { ...S.hits, items: r.items, total: r.total, page: r.page, size: r.size, loaded: true };
  } catch (e) {
    clear(tbl).append(h('tbody', null, h('tr', null, h('td', { class: 'empty', colspan: '9', text: t('加载失败：') + e.message }))));
    return;
  }
  renderHits();
  renderCounts();
}

function renderHits() {
  const tbl = clear($('fw-hits'));
  tbl.append(h('thead', null, h('tr', null, [t('时间'), t('用户'), t('实例 / 库'), t('场景'), t('规则'), t('结果'), t('语句'), t('次数'), ''].map((x) => h('th', { text: x })))));
  const body = h('tbody');
  if (!S.hits.items.length) body.append(h('tr', null, h('td', { class: 'empty', colspan: '9', text: t('没有符合条件的命中记录') })));
  for (const x of S.hits.items) {
    body.append(h('tr', { 'data-hit': String(x.id) },
      h('td', { class: 'mono small', text: x.last_at }),
      h('td', { text: x.user }),
      h('td', null, h('div', { text: x.instance || '—' }), h('div', { class: 'muted small', text: x.db })),
      h('td', { text: SCENES[x.scene] || x.scene }),
      h('td', null, h('span', { text: x.rule_name }), x.rule_exists ? null : h('span', { class: 'muted small', text: t('（已删除）') })),
      h('td', null, resultBadge(x.effective, x.would_action)),
      h('td', null, h('span', { class: 'fw-sqlc', title: x.sql, text: x.sql })),
      h('td', { class: 'mono', text: x.repeat > 1 ? '×' + x.repeat : '' }),
      h('td', null, h('button', { class: 'fw-link', type: 'button', text: t('详情'), onclick: () => showHit(x.id) }))));
  }
  tbl.append(body);
  const pg = clear($('fw-h-pager'));
  const pages = Math.max(1, Math.ceil(S.hits.total / S.hits.size));
  pg.append(
    h('button', { class: 'btn', type: 'button', text: t('上一页'), disabled: S.hits.page <= 1, onclick: () => loadHits(S.hits.page - 1) }),
    h('span', { class: 'muted small', text: t('第 {page} / {pages} 页 · 共 {total} 条', { page: S.hits.page, pages, total: fmtNum(S.hits.total) }) }),
    h('button', { class: 'btn', type: 'button', text: t('下一页'), disabled: S.hits.page >= pages, onclick: () => loadHits(S.hits.page + 1) }));
}

async function showHit(id) {
  let x;
  try { x = await api('GET', `${BASE}/hits/${id}`); } catch (e) { toast(e.message, 'err'); return; }
  const rows = [
    [t('时间'), x.repeat > 1 ? t('{at}（最近 {last}，合并 {n} 次）', { at: x.created_at, last: x.last_at, n: x.repeat }) : x.created_at],
    [t('用户'), x.user], [t('实例 / 库'), (x.instance || '—') + (x.db ? ' / ' + x.db : '')], [t('场景'), SCENES[x.scene] || x.scene],
    [t('规则'), x.rule_exists ? x.rule_name : t('{name}（已删除）', { name: x.rule_name })], [t('指纹'), x.fingerprint || '—'],
  ];
  if (x.reason) rows.push([t('填写的原因'), x.reason]);
  if (x.query_log_id) rows.push([t('查询日志'), t('#{id}（语句已执行）', { id: x.query_log_id })]);
  const info = x.effective === 'observe'
    ? t('规则处于观察模式：这条语句已正常执行，只留下这条记录。') + (x.would_action && ACTIONS[x.would_action] ? GAP + t('若规则已生效，将被「{action}」。', { action: ACTIONS[x.would_action].t }) : '')
    : x.effective === 'deny' ? t('这条语句没有执行。') + (x.message ? GAP + t('用户看到的说明：{msg}', { msg: x.message }) : '')
      : x.effective === 'reason' ? t('用户填写原因后语句才被执行。') : t('语句已放行，并按规则做了处理。');
  const body = [
    h('div', { class: 'fw-fgrid' }, rows.flatMap(([k, v]) => [h('label', { text: k }), h('div', { class: k === t('指纹') ? 'mono' : '', text: v })])),
    h('div', null, h('div', { class: 'muted small fw-lbl', text: t('语句') }),
      h('pre', { class: 'fw-sql' }, ...highlightNodes(x.sql, []))),
    h('div', { class: 'alert info', text: info }),
  ];
  if ((x.matched || []).length) body.push(h('div', null, h('div', { class: 'muted small fw-lbl', text: t('命中的条件') }), h('ul', { class: 'fw-notes' }, x.matched.map((m) => h('li', { text: m })))));
  if ((x.unknown || []).length) body.push(h('div', { class: 'alert warn' }, t('未评估的条件：{items}', { items: x.unknown.join(isZh ? '；' : '; ') })));
  if (x.downgraded) body.push(h('div', { class: 'alert warn', text: x.downgraded }));
  const foot = [];
  if (x.rule_exists && x.user) {
    foot.push(h('button', { class: 'btn', type: 'button', text: t('为该用户添加例外…'), onclick: () => { ui.close(); openExceptionDialog({ rule_id: x.rule_id, subject_kind: 'user', subject_value: x.user }); } }));
  }
  if (x.fingerprint) {
    foot.push(h('button', { class: 'btn', type: 'button', text: t('用此指纹新建规则…'), onclick: () => {
      ui.close();
      newRule({ name: t('指纹黑名单 {fp}', { fp: x.fingerprint }), description: t('来自命中记录 #{id}', { id: x.id }), priority: 5, mode: 'observe', scenes: [x.scene], match: 'any',
        conditions: [{ t: 'fingerprint', v: x.fingerprint }], action: 'deny', message: t('这条语句已被确认有风险，请联系 DBA。') });
    } }));
  }
  const ui = openDrawer({ title: t('命中详情'), badge: resultBadge(x.effective, x.would_action), body, footer: [...foot,
    h('button', { class: 'btn', type: 'button', text: t('关闭'), onclick: () => ui.close() })] });
}

// ---- 例外 ----
function renderExceptions() {
  const tbl = clear($('fw-excs'));
  tbl.append(h('thead', null, h('tr', null, [t('规则'), t('豁免对象'), t('原因'), t('有效期至'), t('创建人'), ''].map((x) => h('th', { text: x })))));
  const body = h('tbody');
  if (!S.excs.length) body.append(h('tr', null, h('td', { class: 'empty', colspan: '6', text: t('还没有例外') })));
  for (const e of S.excs) {
    body.append(h('tr', { class: e.expired ? 'off' : '' },
      h('td', { text: e.rule_name }),
      h('td', { text: e.subject_text }),
      h('td', { text: e.reason }),
      h('td', null, e.expires_at, e.expired ? badge(t('已过期'), 'warn') : null),
      h('td', { text: e.created_by }),
      h('td', null, h('button', { class: 'fw-link danger', type: 'button', text: e.expired ? t('清除') : t('撤销'), onclick: () => revokeException(e) }))));
  }
  tbl.append(body);
}

async function revokeException(e) {
  const res = await confirmRun({
    title: e.expired ? t('清除过期的例外') : t('撤销例外'), okText: e.expired ? t('清除') : t('撤销'), level: e.expired ? 'normal' : 'high',
    lines: [e.expired ? t('这条例外已经过期，不再生效；清除只是把它从名单里移走。') : t('撤销后，规则对该对象立即恢复生效。')],
    details: [{ k: t('规则'), v: e.rule_name }, { k: t('豁免对象'), v: e.subject_text }, { k: t('原因'), v: e.reason }],
  }, () => api('DELETE', `${BASE}/exceptions/${e.id}`));
  if (res.ok) { toast(t('已撤销')); await refresh(); }
}

function openExceptionDialog(pre = {}) {
  if (!S.rules.length) { toast(t('还没有规则，先创建规则再添加例外'), 'warn'); return; }
  const err = h('div', { class: 'alert err', role: 'alert', hidden: true });
  const showErr = (m) => { err.textContent = m || ''; err.hidden = !m; };
  const ruleSel = h('select', { 'aria-label': t('规则') }, S.rules.map((r) => h('option', { value: String(r.id), selected: r.id === pre.rule_id, text: r.name })));
  const valueBox = h('div');
  const kindSel = h('select', { 'aria-label': t('豁免对象类型'), onchange: () => buildValue() },
    [['user', t('用户')], ['group', t('权限组')], ['instance', t('实例')], ['fingerprint', t('SQL 指纹')]].map(([v, label]) =>
      h('option', { value: v, selected: (pre.subject_kind || 'user') === v, text: label })));
  let valueEl = null;
  function buildValue() {
    const kind = kindSel.value;
    const cur = pre.subject_kind === kind ? String(pre.subject_value || '') : '';
    if (kind === 'group') {
      valueEl = h('select', { 'aria-label': t('权限组') }, ((S.meta && S.meta.groups) || []).map((g) => h('option', { value: String(g.id), selected: String(g.id) === cur, text: g.name })));
    } else if (kind === 'instance') {
      valueEl = h('select', { 'aria-label': t('实例') }, ((S.meta && S.meta.instances) || []).map((i) => h('option', { value: String(i.id), selected: String(i.id) === cur, text: i.name })));
    } else {
      valueEl = h('input', { type: 'text', class: kind === 'fingerprint' ? 'mono' : null, value: cur, autocomplete: 'off',
        placeholder: kind === 'user' ? t('用户名，如 rd_li') : t('8 位十六进制指纹'), 'aria-label': kind === 'user' ? t('用户名') : t('SQL 指纹') });
    }
    clear(valueBox).append(valueEl);
  }
  buildValue();
  const daysSel = h('select', { 'aria-label': t('有效期') }, [[7, t('7 天')], [30, t('30 天')], [90, t('90 天')], [365, t('1 年')]]
    .map(([v, label]) => h('option', { value: String(v), selected: v === 30, text: label })));
  const reason = h('textarea', { rows: '3', placeholder: t('原因（必填，至少 4 个字）'), 'aria-label': t('原因'), maxlength: '500' });
  const save = h('button', { class: 'btn primary', type: 'button', text: t('保存'), onclick: async () => {
    showErr('');
    save.disabled = true;
    try {
      await api('POST', BASE + '/exceptions', {
        rule_id: Number(ruleSel.value), subject_kind: kindSel.value, subject_value: valueEl.value.trim(), reason: reason.value.trim(), days: Number(daysSel.value),
      });
      ui.close();
      toast(t('例外已添加'));
      await refresh();
      setTab('exc');
    } catch (e) {
      showErr(e.message);
    } finally {
      save.disabled = false;
    }
  } });
  const ui = openDrawer({
    title: t('添加例外'), narrow: true,
    body: [err, h('div', { class: 'fw-fgrid' },
      h('label', { text: t('规则') }), ruleSel,
      h('label', { text: t('豁免对象') }), h('div', null, kindSel, h('div', { class: 'fw-gap' }, valueBox)),
      h('label', { text: t('有效期') }), daysSel,
      h('label', { class: 'top', text: t('原因') }), reason)],
    footer: [h('button', { class: 'btn ghost', type: 'button', text: t('取消'), onclick: () => ui.close() }), save],
  });
}

// ---- 模板 ----
function renderTemplates() {
  const box = clear($('fw-tpls'));
  if (!S.tpls.length) { box.append(h('div', { class: 'fw-empty', text: t('没有可用的模板') })); return; }
  for (const tpl of S.tpls) {
    const conds = [];
    tpl.rule.conditions.forEach((c) => conds.push(condChip(c, pendingTypes())));
    const btn = h('button', { class: 'btn', type: 'button', disabled: tpl.applied || !S.ready, text: tpl.applied ? t('已添加') : t('添加（观察模式）'), onclick: () => applyTemplate(tpl) });
    box.append(h('div', { class: 'fw-tpl' },
      h('span', { class: 'cat', text: tpl.category }), h('h5', { text: tpl.name }), h('p', { text: tpl.description }),
      h('div', { class: 'fw-cond' }, conds),
      h('div', { class: 'foot' }, actionBadge(tpl.rule.action, tpl.rule.action_arg), tpl.pending ? badge(t('场景尚未接入'), 'warn') : null,
        h('span', { class: 'spacer' }), btn)));
  }
}

async function applyTemplate(tpl) {
  try {
    await api('POST', `${BASE}/templates/${encodeURIComponent(tpl.key)}/apply`, {});
    toast(t('已添加「{name}」（观察模式）', { name: tpl.name }));
    await refresh();
    setTab('rules');
  } catch (e) {
    toast(e.message, 'err');
  }
}

function fillRuleSelect() {
  const sel = $('fw-h-rule');
  const cur = sel.value;
  const ids = new Set([''].concat(S.rules.map((r) => String(r.id))));
  clear(sel).append(h('option', { value: '', text: t('全部规则') }), ...S.rules.map((r) => h('option', { value: String(r.id), text: r.name })));
  sel.value = ids.has(cur) ? cur : '';
}

// ---- 启动 ----
async function boot() {
  try {
    S.meta = await api('GET', BASE + '/meta');
  } catch (e) {
    S.loadError = t('页面加载失败：{msg}', { msg: e.message });
    renderAlerts();
    return;
  }
  // 静态下拉
  $('fw-f-action').append(...Object.keys(ACTIONS).map((k) => h('option', { value: k, text: ACTIONS[k].t })));
  $('fw-f-scene').append(...Object.keys(SCENES).map((k) => h('option', { value: k, text: SCENES[k] })));
  $('fw-h-scene').append(...Object.keys(SCENES).map((k) => h('option', { value: k, text: SCENES[k] })));

  S.bench = initBench({
    meta: S.meta,
    openRuleDraft: (d) => newRule(d),
    openException: (p) => openExceptionDialog({ ...p, rule_id: S.rules[0] && S.rules[0].id }),
  });

  // 事件
  document.querySelectorAll('#fw-tabs button').forEach((b) => b.addEventListener('click', () => setTab(b.dataset.t)));
  document.querySelectorAll('#fw-gmode button').forEach((b) => b.addEventListener('click', () => setGlobalMode(b.dataset.m)));
  $('fw-switch').addEventListener('click', toggleSwitch);
  $('fw-b-test').addEventListener('click', () => setTab('bench'));
  $('fw-b-new').addEventListener('click', () => newRule());
  $('fw-e-new').addEventListener('click', () => openExceptionDialog({ rule_id: S.rules[0] && S.rules[0].id }));
  $('fw-f-q').addEventListener('input', (e) => { S.f.q = e.target.value; renderRules(); });
  for (const [id, key] of [['fw-f-action', 'action'], ['fw-f-scene', 'scene'], ['fw-f-state', 'state']]) {
    $(id).addEventListener('change', (e) => { S.f[key] = e.target.value; renderRules(); });
  }
  let timer = null;
  $('fw-h-q').addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(() => loadHits(1), 300); });
  for (const id of ['fw-h-rule', 'fw-h-result', 'fw-h-scene', 'fw-h-days']) $(id).addEventListener('change', () => loadHits(1));
  $('fw-h-refresh').addEventListener('click', () => loadHits(S.hits.page));

  // 浏览器前进 / 后退、手动改地址栏里的 #标签 时同步标签页
  window.addEventListener('hashchange', () => {
    const t = (location.hash || '').replace('#', '');
    if (TABS.includes(t) && t !== S.tab) setTab(t, { push: false });
  });

  await refresh();
  const initial = (location.hash || '').replace('#', '');
  setTab(TABS.includes(initial) ? initial : 'rules', { push: false });
  if (!S.hits.loaded) loadHits(1); // 标签上的数量
}

boot();
