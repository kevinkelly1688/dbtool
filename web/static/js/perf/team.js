/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: team.js
 * @time: 2026/10/28
 */
// 团队配置：团队（默认接口人 / TL / 总监 / 参与排行）与路由规则（库名正则 → 团队 / 负责人）。
import { api, confirmRun, setButtonMeta } from '../common.js';
import { t, tp } from '../i18n.js';
import { $, el, btn, setMsg, fillSelect, openModal, closeModal, wireModals, busy, applyButtons, loadButtons, can, setFormReadOnly, showReadOnlyNotice } from './util.js';

setButtonMeta({ 'perfcfg.team': t('修改团队配置与路由规则') });

const msg = $('pt-msg');
let data = null;
let editTeam = null;
let editRoute = null;
let ro = false; // 只有“查看配置”权限（没有 perfcfg.team）：页面只读
let rtInit = false;

const userName = (u) => t('{display}（{username}）', { display: u.display || u.username, username: u.username });

function renderTeams() {
  const body = $('pt-rows');
  body.replaceChildren();
  if (!data.teams.length) {
    const td = el('td', 'empty', t('还没有团队配置。'));
    td.colSpan = 8;
    const tr = el('tr');
    tr.append(td);
    body.append(tr);
    return;
  }
  for (const tm of data.teams) {
    const tr = el('tr');
    const dash = (v) => el('td', v ? '' : 'muted', v || '—');
    const en = el('td');
    en.append(el('span', 'st ' + (tm.enabled ? 'passed' : 'aborted'), tm.enabled ? t('是') : t('否')));
    const ops = el('td', 'row-actions');
    const edit = btn(t('编辑'), '', () => openTeam(tm));
    const del = btn(t('删除'), 'ghost danger', () => removeTeam(tm));
    edit.dataset.btn = del.dataset.btn = 'perfcfg.team';
    ops.append(edit, document.createTextNode(' '), del);
    if (ro) ops.append(document.createTextNode(' '), btn(t('查看'), '', () => openTeam(tm)));
    tr.append(el('td', '', tm.team), dash(tm.default_assignee), dash(tm.leaders.join(t('、|sep'))), dash(tm.directors.join(t('、|sep'))),
      el('td', tm.webhooks.length ? '' : 'muted', tm.webhooks.length ? tm.webhooks.map((w) => tp(w) + ' ✓').join(' ') : t('未配置')), en, dash(tm.remark), ops);
    body.append(tr);
  }
}

function renderRoutes() {
  const body = $('pr-rows');
  body.replaceChildren();
  if (!data.routes.length) {
    const td = el('td', 'empty', t('没有路由规则。'));
    td.colSpan = 7;
    const tr = el('tr');
    tr.append(td);
    body.append(tr);
  }
  for (const r of data.routes) {
    const tr = el('tr');
    const ops = el('td', 'row-actions');
    const edit = btn(t('编辑'), '', () => openRoute(r));
    const del = btn(t('删除'), 'ghost danger', () => removeRoute(r));
    edit.dataset.btn = del.dataset.btn = 'perfcfg.team';
    ops.append(edit, document.createTextNode(' '), del);
    if (ro) ops.append(document.createTextNode(' '), btn(t('查看'), '', () => openRoute(r)));
    tr.append(el('td', 'num', String(r.priority)), el('td', r.instance ? '' : 'muted', r.instance || t('全部')), el('td', 'mono', r.db_regex),
      el('td', r.tbl_regex ? 'mono' : 'muted', r.tbl_regex || t('不限')), el('td', '', r.team),
      el('td', r.assignee ? '' : 'muted', r.assignee || '—'), ops);
    body.append(tr);
  }
}

// ---- 团队 ----

function openTeam(tm) {
  editTeam = tm || null;
  setMsg($('mt-msg'), '');
  $('mt-title').textContent = tm ? t('编辑团队配置：{team}', { team: tm.team }) : t('新增团队配置');
  const used = new Set(data.teams.map((x) => x.resource_group_id));
  const groups = tm ? data.groups.filter((g) => g.id === tm.resource_group_id) : data.groups.filter((g) => !used.has(g.id));
  fillSelect($('mt-group'), groups, (g) => g.name, (g) => g.id);
  $('mt-group').disabled = !!tm;
  fillSelect($('mt-default'), data.users, userName, (u) => u.id, ['0', t('（无）')]);
  fillSelect($('mt-leaders'), data.users, userName, (u) => u.id);
  fillSelect($('mt-directors'), data.users, userName, (u) => u.id);
  $('mt-default').value = tm && tm.default_assignee_id ? String(tm.default_assignee_id) : '0';
  const pick = (sel, ids) => { for (const o of sel.options) o.selected = ids.includes(Number(o.value)); };
  pick($('mt-leaders'), tm ? tm.leader_ids : []);
  pick($('mt-directors'), tm ? tm.director_ids : []);
  $('mt-enabled').checked = tm ? tm.enabled : true;
  $('mt-remark').value = tm ? tm.remark : '';
  if (ro) setFormReadOnly('m-team', true);
  openModal('m-team');
}

const selected = (sel) => [...sel.selectedOptions].map((o) => Number(o.value));

async function saveTeam(button) {
  await busy(button, async () => {
    setMsg($('mt-msg'), '');
    const body = { resource_group_id: Number($('mt-group').value), default_assignee_id: Number($('mt-default').value), leader_ids: selected($('mt-leaders')),
      director_ids: selected($('mt-directors')), enabled: $('mt-enabled').checked, remark: $('mt-remark').value };
    try {
      if (editTeam) await api('PUT', `/api/v1/perf/config/teams/${editTeam.id}`, body);
      else await api('POST', '/api/v1/perf/config/teams', body);
      closeModal('m-team');
      setMsg(msg, t('已保存'), 'info');
      await load();
    } catch (e) {
      setMsg($('mt-msg'), e.message);
    }
  });
}

async function removeTeam(tm) {
  const x = await confirmRun({
    title: t('删除团队配置 · {team}', { team: tm.team }), level: 'high', code: 'perfcfg.team', okText: t('确认删除'),
    lines: [t('删除后该团队不再接收自动分派；资源组与已有工单不受影响。')],
    details: [{ k: t('团队'), v: tm.team }, { k: t('状态'), v: tm.enabled ? t('启用|state') : t('停用|state') }],
  }, () => api('DELETE', `/api/v1/perf/config/teams/${tm.id}`));
  if (!x.ok) return;
  setMsg(msg, t('已删除'), 'info');
  try { await load(); } catch (e) { setMsg(msg, e.message); }
}

// ---- 路由规则 ----

function openRoute(r) {
  editRoute = r || null;
  setMsg($('mr-msg'), '');
  $('mr-title').textContent = r ? t('编辑路由规则 #{id}', { id: r.id }) : t('新增路由规则');
  fillSelect($('mr-inst'), data.instances, (i) => i.name, (i) => i.id, ['0', t('全部实例')]);
  fillSelect($('mr-team'), data.groups, (g) => g.name, (g) => g.id);
  fillSelect($('mr-user'), data.users, userName, (u) => u.id, ['0', t('（不指定）')]);
  $('mr-inst').value = r && r.instance_id ? String(r.instance_id) : '0';
  $('mr-regex').value = r ? r.db_regex : '.*';
  if (r) $('mr-team').value = String(r.team_id);
  $('mr-user').value = r && r.assignee_id ? String(r.assignee_id) : '0';
  $('mr-prio').value = r ? r.priority : 100;
  $('mr-tbl').value = r ? r.tbl_regex || '' : '';
  $('mr-tbl').disabled = !data.tbl_regex_ready; // 旁路表还没创建时不能设置表名正则
  $('mr-tbl').title = data.tbl_regex_ready ? '' : t('未执行 060_perf_ext.sql，暂不可用');
  if (ro) setFormReadOnly('m-route', true);
  openModal('m-route');
}

async function saveRoute(button) {
  await busy(button, async () => {
    setMsg($('mr-msg'), '');
    const body = { instance_id: Number($('mr-inst').value), db_regex: $('mr-regex').value, team_id: Number($('mr-team').value), assignee_id: Number($('mr-user').value),
      priority: Number($('mr-prio').value), tbl_regex: $('mr-tbl').value };
    try {
      if (editRoute) await api('PUT', `/api/v1/perf/config/routes/${editRoute.id}`, body);
      else await api('POST', '/api/v1/perf/config/routes', body);
      closeModal('m-route');
      setMsg(msg, t('已保存'), 'info');
      await load();
    } catch (e) {
      setMsg($('mr-msg'), e.message);
    }
  });
}

async function removeRoute(r) {
  const x = await confirmRun({
    title: t('删除路由规则 #{id}', { id: r.id }), level: 'high', code: 'perfcfg.team', okText: t('确认删除'),
    lines: [t('删除后命中该规则的新工单按后续规则分派。')],
    details: [{ k: t('规则编号'), v: '#' + r.id }, { k: t('库名正则'), v: r.db_regex }, { k: t('表名正则'), v: r.tbl_regex || '' }, { k: t('优先级'), v: String(r.priority ?? '') }],
  }, () => api('DELETE', `/api/v1/perf/config/routes/${r.id}`));
  if (!x.ok) return;
  setMsg(msg, t('已删除'), 'info');
  try { await load(); } catch (e) { setMsg(msg, e.message); }
}

// ---- 路由测试 ----

const SRC_LABEL = { route: t('命中的路由规则'), db_owner: t('库负责人（数据库管理）'), team_default: t('团队默认接口人'), dba_rotation: t('DBA 值班轮询'), collect_config: t('采集配置的默认团队'), instance_group: t('实例所属资源组') };

function initRouteTest() {
  fillSelect($('rt-inst'), data.instances, (i) => i.name, (i) => i.id);
  $('rt-go').addEventListener('click', (e) => busy(e.currentTarget, async () => {
    const box = $('rt-res');
    box.replaceChildren();
    try {
      const r = await api('POST', '/api/v1/perf/config/route-test', {
        instance_id: Number($('rt-inst').value), db: $('rt-db').value.trim(), table: $('rt-tbl').value.trim(), sql: $('rt-sql').value, owner_side: $('rt-side').value,
      });
      const lines = [];
      const ruleArgs = r.matched ? { id: r.rule.id, priority: r.rule.priority, db: r.rule.db_regex, tbl: r.rule_tbl_regex } : null;
      lines.push(r.matched
        ? (r.rule_tbl_regex ? t('命中路由规则 #{id}（优先级 {priority}，库名 {db}，表名 {tbl}）', ruleArgs) : t('命中路由规则 #{id}（优先级 {priority}，库名 {db}）', ruleArgs))
        : t('没有命中任何路由规则'));
      const teamName = r.team || t('无');
      lines.push(r.team_src ? t('团队：{team}（{src}）', { team: teamName, src: SRC_LABEL[r.team_src] || r.team_src }) : t('团队：{team}', { team: teamName }));
      const assigneeName = r.assignee || t('无，工单将停在“待分派”');
      lines.push(r.assignee_src ? t('负责人：{assignee}（{src}）', { assignee: assigneeName, src: SRC_LABEL[r.assignee_src] || r.assignee_src })
        : t('负责人：{assignee}', { assignee: assigneeName }));
      if (r.dba) lines.push(t('跟进 DBA：{dba}', { dba: r.dba }));
      if (r.tables && r.tables.length) lines.push(t('参与匹配的表：{tables}', { tables: r.tables.join(t('、|sep')) }));
      const cls = r.matched || r.assignee ? 'alert info' : 'alert warn';
      const div = el('div', cls);
      for (const l of lines) div.append(el('div', '', l));
      box.append(div);
    } catch (err) {
      setMsg(box, err.message);
    }
  }));
}

async function load() {
  data = await api('GET', '/api/v1/perf/config/team');
  const b = await loadButtons();
  ro = !!b && !can(b, 'perfcfg.team');
  if (ro) showReadOnlyNotice($('pt-ro'), t('团队配置与路由规则'));
  renderTeams();
  renderRoutes();
  if (!rtInit) { rtInit = true; initRouteTest(); }
  await applyButtons(document); // 无“修改团队配置”权限的按钮置灰
}

wireModals();
$('pt-new').addEventListener('click', () => {
  if ($('pt-tabs').querySelector('.tab.on').dataset.pane === 'routes') openRoute(null);
  else openTeam(null);
});
$('pr-new').addEventListener('click', () => openRoute(null));
$('mt-ok').addEventListener('click', (e) => saveTeam(e.currentTarget));
$('mr-ok').addEventListener('click', (e) => saveRoute(e.currentTarget));
document.querySelectorAll('#pt-tabs .tab').forEach((tab) => tab.addEventListener('click', () => {
  document.querySelectorAll('#pt-tabs .tab').forEach((x) => x.classList.toggle('on', x === tab));
  $('pt-pane-teams').hidden = tab.dataset.pane !== 'teams';
  $('pt-pane-routes').hidden = tab.dataset.pane !== 'routes';
}));
load().catch((e) => setMsg(msg, e.message));
