/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_sys.js
 * @time: 2026/10/28
 */
/* 系统管理（仅超级用户）：审批流程配置 flow / 资源组管理 rg / 用户管理 user / 权限组管理 ag
   规格：docs/user-guide.md §12、internal/sysadmin、internal/btnperm
   直接读写 data.js 的 RGROUPS / INSTANCES、perm.js 的 USERS / GROUPS / SETTINGS。
   导出：window.APPROVAL_FLOWS、window.approvalFlowOf(rg, type)、window.USER_OVERRIDES */
(function () {
  'use strict';

  /* ---------- 局部样式（只用 CSS 变量） ---------- */
  const css = document.createElement('style');
  css.textContent = `
  .sy-lv{display:flex;gap:10px;align-items:flex-start;border:1px solid var(--border);border-radius:10px;padding:10px 12px;background:var(--panel);margin-bottom:8px}
  .sy-lv.warn{border-color:var(--warn);background:var(--warn-soft)}
  .sy-lv.over{outline:2px dashed var(--brand)}
  .sy-lv.dragging{opacity:.45}
  .sy-lv .no{width:26px;height:26px;border-radius:50%;background:var(--brand-soft);color:var(--brand);font-weight:700;display:grid;place-items:center;flex:none}
  .sy-lv .drag{padding-top:3px;font-size:16px;user-select:none}
  .sy-lv .ops{display:flex;gap:4px;flex:none}
  tr.sy-warn td{background:var(--warn-soft)}
  td.sy-bad{background:var(--bad-soft)}
  td.sy-cell{cursor:pointer}td.sy-cell:hover{outline:2px solid var(--brand);outline-offset:-2px}
  .sy-xfer{display:grid;grid-template-columns:minmax(0,1fr) 64px minmax(0,1fr);gap:8px;align-items:stretch}
  .sy-xfer .box{border:1px solid var(--border);border-radius:10px;display:flex;flex-direction:column;min-height:300px}
  .sy-xfer .box h4{margin:0;padding:8px 10px;border-bottom:1px solid var(--border);font-size:13px;background:var(--panel2);border-radius:10px 10px 0 0;display:flex;justify-content:space-between}
  .sy-xfer .box .lst{overflow:auto;max-height:300px;padding:4px 6px;flex:1}
  .sy-xfer .box .lst label{display:flex;gap:8px;align-items:flex-start;padding:4px 4px;border-radius:6px;cursor:pointer}
  .sy-xfer .box .lst label:hover{background:var(--panel2)}
  .sy-xfer .mid{display:flex;flex-direction:column;justify-content:center;gap:8px}
  @media(max-width:700px){.sy-xfer{grid-template-columns:1fr}.sy-xfer .mid{flex-direction:row;justify-content:center}}
  .sy-pick{border:1px solid var(--border);border-radius:10px;max-height:240px;overflow:auto;padding:6px 10px}
  .sy-pick .ph{font-weight:600;font-size:12px;color:var(--muted);margin:6px 0 2px}
  .sy-pick label{display:flex;gap:6px;align-items:center;padding:2px 0;cursor:pointer}
  .sy-fb{display:inline-block;margin:3px 6px 3px 0}
  .sy-b{opacity:1}.sy-b.sy-lock{opacity:.5;cursor:not-allowed}
  input[type=date]{background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:6px 10px}
  .sy-scroll{max-height:56vh;overflow:auto}
  `;
  document.head.appendChild(css);

  /* ---------- 公共辅助 ---------- */
  const uById = id => USERS.find(u => u.id == id);
  const gById = id => GROUPS.find(g => g.id === id);
  const rgById = id => RGROUPS.find(r => r.id == id);
  const refreshShell = () => { buildNav(); buildTop(); };
  const gnum = id => (GROUPS.findIndex(g => g.id === id) + 1) * 10;      // 模拟 dbt_auth_group.id
  const dis = u => u.status === 'disabled';
  const sorted = list => ALL_CODES().filter(c => list.has ? list.has(c) : list.includes(c));
  const lbl = c => (BTNS[c] ? BTNS[c].label : c);
  const redo = () => { const open = $('#drawer').classList.contains('open') && US.drawer; rerender(); if (open) userDrawer(US.drawer); };
  const seg = (code, items, cur, op) => `<span class="subtabs">${items.map(([v, t]) => B(code, t, {cls: 'sm' + (v === cur ? ' primary' : ''), data: {op, to: v}})).join('')}</span>`;

  /* ---------- 种子：补充用户字段 / 几位用户 / 资源组机器人标志 ---------- */
  [
    {id: 9, login: 'rd_xu', name: '许开发', email: 'xu@example.com', dept: '数据平台部', groups: ['g_rd'], rgs: [4], status: 'active', auth: 'LDAP', last: '2026-10-05 14:08', ip: '10.1.8.17'},
    {id: 10, login: 'qa_lin', name: '林测试', email: 'lin@example.com', dept: '质量保障部', groups: ['g_qa'], rgs: [2, 4], status: 'active', auth: 'LDAP', last: '2026-10-02 09:41', ip: '10.1.5.31'},
    {id: 11, login: 'old_zhang', name: '张三（已离职）', email: 'zhang@example.com', dept: '订单中心', groups: ['g_rd'], rgs: [1], status: 'disabled', auth: 'LDAP', last: '2026-07-02 18:03', ip: '10.1.3.44'},
  ].forEach(u => { if (!USERS.some(x => x.login === u.login)) USERS.push(Object.assign({super: false, twofa: false, extraGrant: [], extraDeny: []}, u)); });
  const NOTIFY_SEED = {dba_li: {ding: 'li.dba', feishu: 'ou_9d3f21'}, tl_wang: {ding: 'wang.zl'}, rd_zhou: {feishu: 'ou_5c2174'}, rd_zhao: {ding: 'zhao.kf'}};
  USERS.forEach(u => { if (u.fail == null) u.fail = u.status === 'locked' ? SETTINGS.loginFailLock : 0; if (!u.notify) u.notify = NOTIFY_SEED[u.login] || {}; });

  const HOOK_N = {ding: '钉钉', feishu: '飞书', slack: 'Slack'};
  RGROUPS.forEach(r => { if (!r.hooks) r.hooks = {ding: r.bot === '钉钉', feishu: r.bot === '飞书', slack: r.bot === 'Slack'}; });
  RGROUPS[0].hooks.slack = true;
  const syncBot = r => { r.bot = Object.keys(HOOK_N).filter(k => r.hooks[k]).map(k => HOOK_N[k]).join('、'); };
  RGROUPS.forEach(syncBot);

  /* 用户级单独授权 / 禁用记录（含到期时间与原因）；USERS[].extraGrant / extraDeny 由它派生（只含未到期的） */
  const OVR = [];
  window.USER_OVERRIDES = OVR;
  const ovrLive = o => !o.expire || o.expire >= TODAY;
  function applyOvr(u) {
    u.extraGrant = OVR.filter(o => o.uid === u.id && o.effect === 'allow' && ovrLive(o)).map(o => o.code);
    u.extraDeny = OVR.filter(o => o.uid === u.id && o.effect === 'deny' && ovrLive(o)).map(o => o.code);
  }
  USERS.forEach(u => (u.extraGrant || []).forEach(c => OVR.push({uid: u.id, code: c, effect: 'allow', expire: dayOffset(24), reason: '支付对账需下载导出文件（周总监口头确认）', by: 'admin', at: '2026-10-01 15:20'})));
  OVR.push({uid: 6, code: 'export.download', effect: 'allow', expire: dayOffset(-7), reason: '季度对账临时下载', by: 'admin', at: '2026-09-20 10:05'});
  OVR.push({uid: 9, code: 'slowquery.advise', effect: 'deny', expire: dayOffset(14), reason: '新人试用期，暂不开放慢查优化建议（TL 要求）', by: 'admin', at: '2026-10-05 17:42'});
  USERS.forEach(applyOvr);

  /* 某用户是否拥有按钮（与 perm.js 的 can() 同口径，但针对任意用户） */
  function userHas(u, c) {
    if (u.super) return true;
    if (!BTNS[c] || BTNS[c].locked) return false;
    if (u.extraDeny.includes(c)) return false;
    return u.extraGrant.includes(c) || u.groups.some(g => (gById(g) || {perms: []}).perms.includes(c));
  }
  /* 每个按钮的有效结果与来源：{ok, src, rec} */
  function effOf(u) {
    const out = {};
    ALL_CODES().forEach(c => {
      const m = BTNS[c];
      if (u.super) { out[c] = {ok: true, src: '超级用户'}; return; }
      if (m.locked) { out[c] = {ok: false, src: '仅超级用户（不可授予）'}; return; }
      const d = OVR.find(o => o.uid === u.id && o.code === c && o.effect === 'deny' && ovrLive(o));
      if (d) { out[c] = {ok: false, src: '单独禁用', rec: d}; return; }
      const a = OVR.find(o => o.uid === u.id && o.code === c && o.effect === 'allow' && ovrLive(o));
      const gs = u.groups.map(gById).filter(g => g && g.perms.includes(c));
      if (gs.length) out[c] = {ok: true, src: '权限组 ' + gs.map(g => g.name).join('、'), rec: a};
      else if (a) out[c] = {ok: true, src: '单独授权', rec: a};
      else out[c] = {ok: false, src: '未授予'};
    });
    return out;
  }

  /* ---------- 原 Django 权限码 → 按钮（迁移助手 / 严格模式预检用；原型里的示意映射） ---------- */
  const LEGACY = [
    ['menu_sqlworkflow', '菜单：SQL 上线', ['sqlworkflow.view_own']], ['sql_submit', '提交 SQL 上线工单', ['sqlworkflow.submit']],
    ['sqlbatch_submit', '提交批量上线', ['sqlworkflow.submit_batch']], ['sql_review', '审核 SQL 上线工单', ['sqlworkflow.view_group', 'sqlworkflow.approve', 'sqlworkflow.reject', 'sqlworkflow.alter_window']],
    ['sql_execute_for_resource_group', '执行资源组内工单', ['sqlworkflow.execute_group', 'sqlworkflow.schedule', 'sqlworkflow.manual_finish']], ['sql_execute', '执行自己的工单', ['sqlworkflow.execute_own']],
    ['menu_sqlanalyze', '菜单：SQL 分析', ['sqlanalyze.view']], ['sql_analyze', 'SQL 分析', ['sqlanalyze.run']],
    ['menu_query', '菜单：在线查询', ['query.view']], ['query_submit', '提交查询', ['query.run']],
    ['query_resource_group_instance', '查询资源组内实例', ['query.group_run']], ['query_all_instances', '查询全部实例', ['query.admin_run']],
    ['menu_queryapplylist', '菜单：权限申请', ['qpriv.view']], ['query_review', '审核查询权限申请', ['qpriv.review']],
    ['query_applypriv', '查看已授予的查询权限', ['qpriv.list']], ['query_mgtpriv', '管理查询权限', ['qpriv.manage']],
    ['menu_data_dictionary', '菜单：数据字典', ['dict.view']], ['data_dictionary_export', '导出数据字典', ['dict.export']],
    ['menu_slowquery', '菜单：慢日志', ['slowlogs.view', 'slowquery.view']], ['menu_sqladvisor', '菜单：优化工具', ['optimize.explain']],
    ['optimize_sqltuning', 'SQLTuning', ['optimize.tuning']], ['optimize_soar', 'SOAR', ['optimize.soar']],
    ['menu_sqlexport', '菜单：数据导出', ['export.view']], ['sqlexport_submit', '提交数据导出', ['export.submit']], ['offline_download', '下载导出文件', ['export.download']],
    ['menu_instance_list', '菜单：实例列表', ['instance.view']], ['menu_dbdiagnostic', '菜单：会话管理', ['session.open', 'session.process']], ['process_kill', '终止会话', ['session.kill']],
    ['menu_perf', '菜单：性能工单', ['perf.view']], ['perf_manage', '性能工单管理', ['perf.assign', 'perf.verify', 'perf.approve', 'perf.adjust', 'perf.reopen']],
    ['audit_user', '系统审计', ['audit.view']],
    ['sql.view_group', 'Django 模型权限：查看权限组', []], ['sql.add_tag', 'Django 模型权限：新增标签', []],
  ];
  const LEG_MAP = {};
  LEGACY.forEach(([c, n, b]) => { LEG_MAP[c] = {name: n, b: b.filter(x => BTNS[x] && !BTNS[x].locked)}; });
  const LEG_GROUP = {
    g_dba: LEGACY.map(x => x[0]).filter(c => c !== 'audit_user' && c !== 'sql_execute' && !c.startsWith('sql.')),
    g_tl: ['menu_sqlworkflow', 'sql_submit', 'sql_review', 'menu_sqlanalyze', 'sql_analyze', 'menu_query', 'query_submit', 'query_resource_group_instance', 'menu_queryapplylist', 'query_review', 'menu_data_dictionary', 'menu_slowquery', 'menu_sqladvisor', 'menu_sqlexport', 'sqlexport_submit', 'menu_perf'],
    g_rd: ['menu_sqlworkflow', 'sql_submit', 'sql_execute', 'menu_query', 'query_submit', 'menu_queryapplylist', 'menu_data_dictionary', 'menu_slowquery', 'menu_sqlanalyze', 'sql_analyze', 'menu_sqladvisor', 'menu_sqlexport', 'sqlexport_submit', 'menu_perf'],
    g_qa: ['menu_sqlworkflow', 'sql_submit', 'menu_query', 'query_submit', 'menu_queryapplylist', 'menu_sqlexport', 'sqlexport_submit', 'offline_download', 'menu_data_dictionary', 'menu_slowquery'],
    g_audit: ['audit_user', 'menu_sqlworkflow', 'menu_queryapplylist', 'menu_sqlexport', 'sql.view_group'],
    g_default: ['menu_query', 'query_submit', 'menu_data_dictionary', 'menu_queryapplylist'],
  };
  const LEG_USER = {rd_zhou: ['menu_dbdiagnostic'], qa_sun: ['menu_instance_list']};      // 直接授予用户的原权限
  const LOGIN_ONLY = ['sqlworkflow.cancel_own', 'sqlworkflow.rollback', 'qpriv.apply'];  // 原系统“登录即可”的按钮
  const legBtns = codes => new Set((codes || []).flatMap(c => (LEG_MAP[c] || {b: []}).b));
  const PRESETS = {};
  [['DBA', 'g_dba'], ['TL', 'g_tl'], ['RD', 'g_rd'], ['QA', 'g_qa'], ['Default', 'g_default']].forEach(([n, id]) => { PRESETS[n] = gById(id).perms.slice(); });

  const strictSet = u => { const s = new Set(); u.groups.forEach(g => (gById(g) || {perms: []}).perms.forEach(c => s.add(c))); u.extraGrant.forEach(c => s.add(c)); u.extraDeny.forEach(c => s.delete(c)); return s; };
  function compatSet(u) {
    const s = strictSet(u);
    u.groups.forEach(g => legBtns(LEG_GROUP[g]).forEach(c => s.add(c)));
    legBtns(LEG_USER[u.login]).forEach(c => s.add(c)); LOGIN_ONLY.forEach(c => s.add(c));
    u.extraDeny.forEach(c => s.delete(c)); return s;
  }

  /* =====================================================================
     审批流（flow）
     ===================================================================== */
  const FT = {query: {name: '查询权限申请', need: 'qpriv.review', no: 1}, sql: {name: 'SQL 上线申请', need: 'sqlworkflow.approve', no: 2}, archive: {name: '数据归档申请', need: '', no: 3}};
  const FT_IDS = Object.keys(FT);
  /* FLOWS[资源组id][工单类型] = [权限组id, …]（按级顺序；未配置则无此键） */
  const FLOWS = {
    1: {sql: ['g_tl', 'g_dba'], query: ['g_tl', 'g_dba'], archive: ['g_tl', 'g_dba']},
    2: {sql: ['g_tl', 'g_dba'], query: ['g_tl', 'g_audit']},
    3: {sql: ['g_tl', 'g_dba'], query: ['g_dba']},
    4: {sql: ['g_dba'], query: ['g_dba'], archive: ['g_dba']},
  };
  window.APPROVAL_FLOWS = FLOWS;
  window.approvalFlowOf = (rg, type) => (FLOWS[rg] && FLOWS[rg][type]) ? FLOWS[rg][type].slice() : null;
  const curFlow = (rg, t) => (FLOWS[rg] && FLOWS[rg][t]) || [];
  const flowText = ids => ids.length ? ids.map(groupName).join(' → ') : '（空）';

  /* 某一级的可审批人 = 资源组内、属于该权限组、未停用、（非超级用户）的用户；需要对应审核按钮才算“可审” */
  function approversOf(gid, rgId, type, member) {
    const g = gById(gid), need = FT[type].need;
    if (!g) return {missing: true, list: [], ok: 0};
    member = member || (u => u.rgs.includes(rgId));
    const list = USERS.filter(u => !u.super && u.groups.includes(gid) && member(u) && !dis(u)).map(u => {
      const hasCode = !need || userHas(u, need);
      return {u, hasCode, locked: u.status === 'locked', ok: hasCode && u.status === 'active'};
    });
    return {g, list, ok: list.filter(x => x.ok).length};
  }
  function levelWarn(a, type) {
    if (a.missing) return '权限组已被删除，该级无法流转';
    if (!a.list.length) return '该级无人可审：资源组内没有属于该权限组的用户';
    if (a.ok) return '';
    if (a.list.every(x => !x.hasCode)) return `该级有 ${a.list.length} 位审批人，但都没有审核权限（需要 ${FT[type].need}）`;
    return '该级审批人账号均已锁定';
  }
  const flowWarns = (rg, t, member) => curFlow(rg, t).map((gid, i) => ({i, gid, msg: levelWarn(approversOf(gid, rg, t, member), t)})).filter(x => x.msg);

  const FL = {rg: 1, type: 'sql', draft: {}};
  const dkey = () => FL.rg + ':' + FL.type;
  const draftOf = () => FL.draft[dkey()] || curFlow(FL.rg, FL.type);

  pages.flow = {
    render(sub) {
      const cur = sub === 'matrix' || sub === 'chart' ? sub : 'rg';
      return tabs('flow', [['rg', '按资源组配置'], ['matrix', '总览矩阵'], ['chart', '审批与通知链路']], cur) +
        (cur === 'rg' ? flowEdit() : cur === 'matrix' ? flowMatrix() : flowChart());
    },
    init() {
      let from = -1;
      $$('.sy-lv[draggable]').forEach(el => {
        el.addEventListener('dragstart', e => { from = +el.dataset.i; el.classList.add('dragging'); e.dataTransfer.effectAllowed = 'move'; try { e.dataTransfer.setData('text/plain', String(from)); } catch (x) {} });
        el.addEventListener('dragend', () => { el.classList.remove('dragging'); $$('.sy-lv.over').forEach(x => x.classList.remove('over')); });
        el.addEventListener('dragover', e => { e.preventDefault(); el.classList.add('over'); });
        el.addEventListener('dragleave', () => el.classList.remove('over'));
        el.addEventListener('drop', e => { e.preventDefault(); const to = +el.dataset.i; if (from >= 0 && from !== to) moveLv(from, to); from = -1; });
      });
    }
  };
  function moveLv(from, to) {
    const arr = draftOf().slice(); const [x] = arr.splice(from, 1); arr.splice(to, 0, x);
    FL.draft[dkey()] = arr; rerender();
  }

  function lvHtml(gid, i, n) {
    const a = approversOf(gid, FL.rg, FL.type), w = levelWarn(a, FL.type);
    const names = a.list.map(x => `<span class="tag ${x.ok ? '' : x.hasCode ? 'warn' : 'bad'}" title="${esc(x.u.name)}${x.ok ? '' : x.hasCode ? '（账号已锁定）' : '（无审核权限）'}">${esc(x.u.login)} ${esc(x.u.name)}${x.locked ? ' 🔒' : ''}</span>`).join(' ');
    return `<div class="sy-lv ${w ? 'warn' : ''}" draggable="true" data-i="${i}">
      <span class="drag" title="拖动调整顺序">⠿</span><span class="no">${i + 1}</span>
      <div style="flex:1;min-width:0"><div class="row"><b>${esc(a.g ? a.g.name : gid)}</b>${a.g ? `<span class="small muted">权限组 · ${a.g.builtin ? '内置' : '自定义'} · 共 ${USERS.filter(u => u.groups.includes(gid)).length} 人</span>` : ''}</div>
        <div class="small" style="margin-top:4px">${esc(rgName(FL.rg))} 内的可审批人：${names || '<span class="muted">无</span>'}</div>
        ${w ? `<div class="small" style="color:var(--warn);margin-top:4px">⚠ ${esc(w)}</div>` : ''}</div>
      <div class="ops"><button class="btn sm" data-on="flMove" data-i="${i}" data-d="-1" ${i === 0 ? 'disabled' : ''} title="上移">↑</button><button class="btn sm" data-on="flMove" data-i="${i}" data-d="1" ${i === n - 1 ? 'disabled' : ''} title="下移">↓</button><button class="btn sm danger" data-on="flDel" data-i="${i}" title="移除该级">✕</button></div></div>`;
  }

  function flowEdit() {
    const rgs = RGROUPS.filter(r => r.enabled);
    if (!rgs.some(r => r.id === FL.rg)) FL.rg = rgs[0] ? rgs[0].id : 0;
    const rg = rgById(FL.rg); if (!rg) return empty('暂无可用资源组');
    const dr = draftOf(), cur = curFlow(rg.id, FL.type), dirty = dr.join() !== cur.join();
    const free = GROUPS.filter(g => !dr.includes(g.id));
    const left = rgs.map(r => { const n = FT_IDS.filter(t => curFlow(r.id, t).length).length, w = FT_IDS.reduce((a, t) => a + flowWarns(r.id, t).length, 0);
      return `<a data-on="flRg" data-id="${r.id}" class="${r.id === FL.rg ? 'on' : ''}">⧉ ${esc(r.name)}<span class="small muted" style="margin-left:auto">${n}/3${w ? ' <span style="color:var(--warn)">⚠' + w + '</span>' : ''}</span></a>`; }).join('');
    return `<div class="grid g-side">
      <div class="card" style="align-self:start"><h2>资源组 <span class="sub">已配置 / 3</span></h2><div class="role-list">${left}</div></div>
      <div class="card"><h2 style="font-size:16px">${esc(rg.name)} <span class="sub">负责人 ${esc(rg.leader)} · ${USERS.filter(u => u.rgs.includes(rg.id)).length} 人</span>
        <span class="right">${subtabs(FT_IDS.map(t => [t, FT[t].name + (curFlow(rg.id, t).length ? (flowWarns(rg.id, t).length ? ' ⚠' : ' ✓') : ' ○')]), FL.type, 'flType')}</span></h2>
        <div class="row" style="margin-bottom:10px"><span>当前审批流程：<b>${cur.length ? esc(flowText(cur)) : '<span class="muted">未配置</span>'}</b>${cur.length ? ` <span class="small muted">${cur.length} 级</span>` : ''}</span>
          ${dirty ? '<span class="tag warn">● 草稿未保存：' + esc(flowText(dr)) + '</span>' : ''}</div>
        <div class="small muted" style="margin-bottom:6px">变更审批流程</div>
        ${dr.length ? dr.map((gid, i) => lvHtml(gid, i, dr.length)).join('') : `<div class="empty" style="padding:18px"><div>尚无审批级别，请在下方添加权限组</div></div>`}
        <div class="toolbar" style="margin-top:6px"><select id="flAddSel" aria-label="添加审批权限组"><option value="">＋ 添加审批权限组…</option>${free.map(g => `<option value="${g.id}">${esc(g.name)}</option>`).join('')}</select>
          <button class="btn" data-on="flAdd" ${dr.length >= 10 ? 'disabled' : ''}>添加一级</button><button class="btn" data-on="flReset" ${dirty ? '' : 'disabled'}>恢复为当前流程</button><button class="btn danger" data-on="flClear" ${dr.length ? '' : 'disabled'}>清空</button>
          <span class="sp"></span>${cur.length ? B('flow.edit', '删除此审批流', {cls: 'danger', data: {op: 'del'}}) : ''}${B('flow.edit', '变更', {cls: 'primary', data: {op: 'save'}})}</div>
      </div></div>`;
  }

  function flowMatrix() {
    const rgs = RGROUPS.filter(r => r.enabled);
    let nCfg = 0, nWarn = 0;
    const rows = rgs.map(r => {
      const cells = FT_IDS.map(t => {
        const ids = curFlow(r.id, t);
        if (!ids.length) return `<td class="sy-cell muted" data-on="flCell" data-rg="${r.id}" data-t="${t}">未配置</td>`;
        nCfg++; const ws = flowWarns(r.id, t), wset = new Set(ws.map(x => x.i));
        if (ws.length) nWarn++;
        const txt = ids.map((g, i) => wset.has(i) ? `<b style="color:var(--bad)">${esc(groupName(g))}</b>` : esc(groupName(g))).join(' → ');
        return `<td class="sy-cell ${ws.length ? 'sy-bad' : ''}" data-on="flCell" data-rg="${r.id}" data-t="${t}">${txt}${ws.length ? `<div class="small" style="color:var(--bad)">⚠ ${ws.map(x => `第 ${x.i + 1} 级${x.msg.includes('无人') ? '无人可审' : '不可审'}`).join('、')}</div>` : ''}</td>`;
      }).join('');
      return `<tr><td><b>${esc(r.name)}</b></td>${cells}</tr>`;
    }).join('');
    return `<div class="grid g3" style="margin-bottom:14px">
      <div class="card kpi"><div class="l">资源组 × 工单类型</div><div class="v">${rgs.length * 3}</div><div class="d flat">${rgs.length} 个资源组 × 3 种类型</div></div>
      <div class="card kpi"><div class="l">已配置</div><div class="v">${nCfg}</div><div class="d ${rgs.length * 3 - nCfg ? 'up' : 'down'}">${rgs.length * 3 - nCfg} 项未配置</div></div>
      <div class="card kpi"><div class="l">有“无人可审”警告</div><div class="v" style="color:${nWarn ? 'var(--bad)' : 'inherit'}">${nWarn}</div></div></div>
    <div class="card"><h2>审批流总览</h2>
      <div class="tbl-wrap"><table><thead><tr><th>资源组</th>${FT_IDS.map(t => `<th>${FT[t].name}</th>`).join('')}</tr></thead><tbody>${rows}</tbody></table></div></div>`;
  }

  function flowChart() {
    const n = (x, y, w, h, cls, t1, t2) => `<g class="n ${cls}"><rect x="${x}" y="${y}" width="${w}" height="${h}" rx="7"/><text x="${x + w / 2}" y="${y + (t2 ? h / 2 - 3 : h / 2 + 5)}">${t1}</text>${t2 ? `<text x="${x + w / 2}" y="${y + h / 2 + 13}" style="font-size:11px;fill:var(--muted)">${t2}</text>` : ''}</g>`;
    const rg = rgById(FL.rg) || RGROUPS[0], t = FL.type, flow = curFlow(rg.id, t);
    const items = [{cls: 'ok', h: `提交人提交「${FT[t].name}」`, m: `资源组 ${esc(rg.name)} · 读取审批流`}].concat(flow.map((gid, i) => { const a = approversOf(gid, rg.id, t), w = levelWarn(a, t);
      return {cls: w ? 'bad' : 'cur', h: `第 ${i + 1} 级：${esc(groupName(gid))}`, m: w ? '⚠ ' + esc(w) : `机器人 @ ${a.list.filter(x => x.ok).map(x => esc(x.u.name)).join('、')}`}; }));
    items.push({cls: flow.length ? 'ok' : 'bad', h: flow.length ? '全部通过 → 工单生效 / 可执行' : '未配置审批流 → 提交被拒绝', m: `机器人通知：${rg.bot ? esc(rg.bot) + ' 群' : '该资源组未配置机器人'}（提交人 + 资源组群）`});
    return `<div class="card"><h2>审批与通知链路</h2>
      <svg viewBox="0 0 1000 380" class="diagram" role="img" aria-label="审批与通知链路">
        <defs><marker id="ah" markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto"><path d="M0,0 L8,4 L0,8 z" fill="var(--muted)"/></marker></defs>
        ${n(10, 140, 120, 50, 'info', '提交工单', '三种工单类型')}${n(160, 140, 150, 50, '', '读取审批流', '资源组 × 工单类型')}${n(160, 30, 150, 50, 'bad', '拒绝提交', '审批类型未配置审流')}
        ${n(350, 140, 130, 50, 'cur', '第 1 级审批', '权限组 A 内任一人')}${n(520, 140, 130, 50, 'cur', '第 2 级审批', '权限组 B 内任一人')}${n(700, 140, 130, 50, 'cur', '第 N 级审批', '最多 10 级')}${n(870, 140, 110, 50, 'ok', '全部通过', '生效 / 可执行')}
        ${n(350, 240, 300, 44, 'bad', '任一级驳回 / 提交人撤回 → 流程终止')}
        ${n(10, 320, 970, 44, 'info', '机器人通知（资源组机器人：钉钉 / 飞书 / Slack ＋ 通知渠道 @ 规则）', '提交 → 通知第 1 级审批人；每级通过 / 驳回 / 终止 → 通知提交人与资源组群；全部通过 → 通知提交人与可执行人')}
        <g class="e"><path d="M130,165 H160"/><path d="M310,165 H350"/><path d="M480,165 H520"/><path d="M650,165 H700"/><path d="M830,165 H870"/><path d="M235,140 V80"/><path d="M415,190 V240"/><path d="M585,190 V240"/>
          <path d="M70,190 V320" stroke-dasharray="4 3" style="stroke:var(--accent)"/><path d="M500,284 V320" stroke-dasharray="4 3" style="stroke:var(--accent)"/><path d="M925,190 V320" stroke-dasharray="4 3" style="stroke:var(--accent)"/></g>
        <g class="t"><text x="145" y="157">按类型</text><text x="330" y="157">已配置</text><text x="500" y="157">通过</text><text x="675" y="157">…</text><text x="850" y="157">通过</text><text x="262" y="112">未配置</text><text x="432" y="220">驳回</text><text x="602" y="220">驳回</text><text x="90" y="262" style="text-anchor:start">通知</text><text x="515" y="306" style="text-anchor:start">通知</text><text x="940" y="262" style="text-anchor:start">通知</text></g>
      </svg></div>
    <div class="card" style="margin-top:12px"><h2>实际示例<span class="right"><select data-onchange="flChartRg" aria-label="资源组">${RGROUPS.filter(r => r.enabled).map(r => `<option value="${r.id}" ${r.id === rg.id ? 'selected' : ''}>${esc(r.name)}</option>`).join('')}</select><select data-onchange="flChartType" aria-label="工单类型">${FT_IDS.map(x => `<option value="${x}" ${x === t ? 'selected' : ''}>${FT[x].name}</option>`).join('')}</select></span></h2>${tline(items)}</div>`;
  }

  ON.flRg = d => { FL.rg = +d.id; rerender(); };
  ON.flType = d => { FL.type = d.v; rerender(); };
  ON.flChartRg = (d, el) => { FL.rg = +el.value; rerender(); };
  ON.flChartType = (d, el) => { FL.type = el.value; rerender(); };
  ON.flAdd = () => { const v = $('#flAddSel').value; if (!v) return toast('请先选择要添加的权限组'); FL.draft[dkey()] = draftOf().concat(v); rerender(); };
  ON.flDel = d => { const a = draftOf().slice(); a.splice(+d.i, 1); FL.draft[dkey()] = a; rerender(); };
  ON.flMove = d => { const i = +d.i, j = i + +d.d; if (j >= 0 && j < draftOf().length) moveLv(i, j); };
  ON.flReset = () => { delete FL.draft[dkey()]; rerender(); };
  ON.flClear = () => { FL.draft[dkey()] = []; rerender(); };
  ON.flCell = d => { FL.rg = +d.rg; FL.type = d.t; go('flow', 'rg'); };

  ACTIONS['flow.edit'] = d => {
    const rg = rgById(FL.rg), t = FL.type, tn = FT[t].name, before = curFlow(rg.id, t), after = draftOf();
    const ids = a => a.map(gnum).join(',');
    if (d.op === 'del') {
      return risky('flow.edit', '删除审批流', `删除 <b>${esc(rg.name)} · ${tn}</b> 的审批流（${esc(flowText(before))}）。删除后该资源组<b>不能再提交该类工单</b>；进行中的工单不受影响。`,
        `DELETE FROM dbt_workflow_audit_setting WHERE group_id=${rg.id} AND workflow_type=${FT[t].no};`, () => {
          delete FLOWS[rg.id][t]; delete FL.draft[dkey()];
          audit('flow.edit', `删除审批流 ${rg.name} · ${tn}：${flowText(before)} ⇒ 未配置`); toast('已删除审批流'); rerender();
        });
    }
    if (!after.length) return toast('至少保留一级审批；如需取消配置请使用“删除此审批流”');
    if (after.join() === before.join()) return toast('审批流没有变化');
    const warns = after.map((g, i) => ({i, msg: levelWarn(approversOf(g, rg.id, t), t)})).filter(x => x.msg);
    risky('flow.edit', '变更审批流', `<b>${esc(rg.name)} · ${tn}</b><br>变更前：${esc(flowText(before)) || '未配置'}<br>变更后：<b>${esc(flowText(after))}</b>
      <br>新提交的工单立即按新流程流转，<b>已在审批中的工单不受影响</b>。${warns.length ? `<div style="margin-top:6px;color:var(--bad)">⚠ ${warns.map(w => `第 ${w.i + 1} 级「${esc(groupName(after[w.i]))}」：${esc(w.msg)}`).join('<br>⚠ ')}<br>仍要保存吗？保存后该级将无人可审，工单会卡住。</div>` : ''}`,
      before.length ? `UPDATE dbt_workflow_audit_setting SET audit_auth_groups='${ids(after)}'\n WHERE group_id=${rg.id} AND workflow_type=${FT[t].no};` : `INSERT INTO dbt_workflow_audit_setting (group_id, group_name, workflow_type, audit_auth_groups)\nVALUES (${rg.id}, '${rg.name}', ${FT[t].no}, '${ids(after)}');`, () => {
        FLOWS[rg.id] = FLOWS[rg.id] || {}; FLOWS[rg.id][t] = after.slice(); delete FL.draft[dkey()];
        audit('flow.edit', `修改审批流 ${rg.name} · ${tn}：${flowText(before)} ⇒ ${flowText(after)}`, '成功', warns.length ? `警告：${warns.length} 级无人可审` : '');
        toast('已变更：' + flowText(after)); rerender();
      });
  };

  /* =====================================================================
     资源组（rg）
     ===================================================================== */
  const RGS = {q: '', all: false};
  const DEMO_BUSY = {   // window.WORKFLOWS 不存在时的兜底示例
    1: [{kind: 'SQL 上线', id: 1042, name: '订单表增加字段 refund_flag', st: '等待审批', href: '#sqlworkflow/detail/1042'}, {kind: 'SQL 上线', id: 1037, name: '订单明细表补充联合索引', st: '定时执行', href: '#sqlworkflow/detail/1037'}],
    2: [{kind: 'SQL 上线', id: 1039, name: '支付流水表新增对账状态列', st: '审核通过', href: '#sqlworkflow/detail/1039'}],
  };
  const WF_BUSY = ['等待审批', '审核通过', '定时执行', '排队中', '执行中'];
  function busyOf(rgId) {
    const out = [], wf = window.WORKFLOWS, ap = window.APPLIES;
    if (Array.isArray(wf)) wf.filter(w => w.rg == rgId && WF_BUSY.includes(w.status)).forEach(w => out.push({kind: 'SQL 上线', id: w.id, name: w.name, st: w.status, href: '#sqlworkflow/detail/' + w.id}));
    else (DEMO_BUSY[rgId] || []).forEach(x => out.push(x));
    if (Array.isArray(ap)) ap.filter(a => a.status === '待审批' && instOf(a.inst).rgs.includes(+rgId)).forEach(a => out.push({kind: '查询权限', id: a.id, name: `${a.user} 申请 ${instOf(a.inst).name} / ${a.db}`, st: '待审批', href: '#qapply/detail/' + a.id}));
    return out;
  }
  const rgUsers = r => USERS.filter(u => u.rgs.includes(r.id) && !dis(u)), rgInsts = r => INSTANCES.filter(i => i.rgs.includes(r.id));
  const flowCfgN = r => FT_IDS.filter(t => curFlow(r.id, t).length).length;
  const hookTags = r => ['ding', 'feishu', 'slack'].map(k => r.hooks[k] ? `<span class="tag ok">${HOOK_N[k]} ✓</span>` : `<span class="tag">${HOOK_N[k]} 未配</span>`).join(' ');

  function rgTable() {
    const q = RGS.q.trim().toLowerCase();
    const list = RGROUPS.filter(r => (RGS.all || r.enabled) && (!q || (r.name + r.leader + r.remark).toLowerCase().includes(q)));
    const leaderName = l => { const u = USERS.find(x => x.login === l); return u ? `${esc(u.name)} <span class="small muted">${esc(l)}</span>` : esc(l); };
    return tbl(['ID', '组名称', '负责人', '成员', '实例', '审批流', '通知机器人', '状态', '操作'], list, {
      attrs: r => r.enabled && (!rgInsts(r).length || flowCfgN(r) < 3) ? ' class="sy-warn"' : '',
      cells: r => [r.id, `<b>${esc(r.name)}</b><div class="small muted">${esc(r.remark || '')}</div>`, leaderName(r.leader), `<span class="nowrap">${rgUsers(r).length} 人</span>`, `${rgInsts(r).length || '<span style="color:var(--warn)">0</span>'} 个`,
        r.enabled ? `${flowCfgN(r)} / 3 已配置` : '<span class="muted">—</span>', r.enabled || r.bot ? hookTags(r) : '<span class="muted">—</span>', r.enabled ? '<span class="tag ok">正常</span>' : '<span class="tag">已停用</span>',
        `<div class="ops">${B('rg.edit', '编辑', {cls: 'sm', data: {id: r.id}})}${B('rg.relations', '关联对象', {cls: 'sm', data: {id: r.id}})}${r.enabled ? B('rg.delete', '停用', {cls: 'sm danger', data: {id: r.id, op: 'disable'}}) : B('rg.delete', '恢复', {cls: 'sm', data: {id: r.id, op: 'restore'}}) + B('rg.delete', '彻底删除', {cls: 'sm danger', data: {id: r.id, op: 'purge'}})}</div>`]
    });
  }
  pages.rg = {
    render() {
      const en = RGROUPS.filter(r => r.enabled);
      const noInst = en.filter(r => !rgInsts(r).length).length, incomplete = en.filter(r => flowCfgN(r) < 3).length, noBot = en.filter(r => !r.bot).length;
      return `<div class="grid g4" style="margin-bottom:14px">
        <div class="card kpi"><div class="l">启用中的资源组</div><div class="v">${en.length}</div><div class="d flat">另有 ${RGROUPS.length - en.length} 个已停用</div></div>
        <div class="card kpi"><div class="l">没有实例的组</div><div class="v" style="color:${noInst ? 'var(--warn)' : 'inherit'}">${noInst}</div></div>
        <div class="card kpi"><div class="l">审批流不完整</div><div class="v" style="color:${incomplete ? 'var(--warn)' : 'inherit'}">${incomplete}</div></div>
        <div class="card kpi"><div class="l">未配置机器人</div><div class="v">${noBot}</div></div></div>
      <div class="card"><div class="toolbar"><input type="search" id="rgQ" data-oninput="rgQ" placeholder="搜索组名 / 负责人 / 备注" value="${esc(RGS.q)}" style="min-width:240px">
        <label class="chk"><input type="checkbox" data-onchange="rgAll" ${RGS.all ? 'checked' : ''}> 显示已停用</label><span class="sp"></span>${B('rg.edit', '＋ 新建资源组', {cls: 'primary', data: {id: ''}})}</div>
        <div id="rgTable">${rgTable()}</div></div>`;
    }
  };
  ON.rgQ = (d, el) => { RGS.q = el.value; $('#rgTable').innerHTML = rgTable(); };
  ON.rgAll = (d, el) => { RGS.all = el.checked; $('#rgTable').innerHTML = rgTable(); };

  ACTIONS['rg.edit'] = d => {
    const r = d.id ? rgById(d.id) : null, isNew = !r;
    const hookRow = (k, ph) => fld(HOOK_N[k] + ' Webhook', `<input id="rgH_${k}" class="mono" autocomplete="off" placeholder="${!isNew && r.hooks[k] ? '已设置（留空不修改）' : ph}">${!isNew && r.hooks[k] ? `<label class="chk small" style="margin-top:4px"><input type="checkbox" id="rgX_${k}"> 清除该机器人配置</label>` : ''}`);
    openModal({title: isNew ? '新建资源组' : '编辑资源组 · ' + r.name, code: 'rg.edit', wide: true,
      body: `<div class="form">${fld('组名称（唯一，≤100 字）', `<input id="rgN" maxlength="100" value="${esc(isNew ? '' : r.name)}" placeholder="例：营销中心">`, {req: true})}
        ${fld('负责人', `<select id="rgL">${USERS.filter(u => u.status === 'active').map(u => `<option value="${u.login}" ${(isNew ? 'dba_li' : r.leader) === u.login ? 'selected' : ''}>${esc(u.login)} · ${esc(u.name)}</option>`).join('')}</select>`)}
        ${fld('备注', `<input id="rgR" maxlength="100" value="${esc(isNew ? '' : r.remark || '')}" placeholder="业务范围说明">`)}
        ${hookRow('ding', 'https://oapi.dingtalk.com/robot/send?access_token=…')}${hookRow('feishu', 'https://open.feishu.cn/open-apis/bot/v2/hook/…')}${hookRow('slack', 'https://hooks.slack.com/services/…')}</div>`,
      onOk() {
        const name = $('#rgN').value.trim();
        if (!name || name.length > 100) { toast('组名称需为 1–100 个字符'); return false; }
        if (RGROUPS.some(x => x.name === name && (!r || x.id !== r.id))) { toast('已存在同名资源组（含已停用的）'); return false; }
        const chg = [], h = {};
        for (const k of ['ding', 'feishu', 'slack']) {
          const v = $('#rgH_' + k).value.trim(), x = $('#rgX_' + k);
          if (v) { if (!/^https?:\/\/[^\s]{4,}$/.test(v) || v.length > 255) { toast(HOOK_N[k] + ' Webhook 必须是完整的 http(s) 地址（≤255 字符）'); return false; } h[k] = true; chg.push(HOOK_N[k] + (!isNew && r.hooks[k] ? '：已更新' : '：已设置')); }
          else if (x && x.checked) { h[k] = false; chg.push(HOOK_N[k] + '：已清除'); }
        }
        const t = isNew ? {id: Math.max(...RGROUPS.map(x => x.id)) + 1, enabled: true, hooks: {ding: false, feishu: false, slack: false}} : r;
        const renamed = !isNew && t.name !== name;
        Object.assign(t, {name, leader: $('#rgL').value, remark: $('#rgR').value.trim()}); Object.assign(t.hooks, h); syncBot(t);
        if (isNew) RGROUPS.push(t);
        audit('rg.edit', `${isNew ? '新建' : '修改'}资源组 ${name}${renamed ? '（改名，已同步审批流组名）' : ''}`, '成功', chg.join('；') || 'Webhook 未变更');
        toast(isNew ? '已新建资源组' : '已保存'); rerender();
      }});
  };

  ACTIONS['rg.delete'] = d => {
    const r = rgById(d.id);
    if (d.op === 'restore') return openModal({title: '恢复资源组 · ' + r.name, code: 'rg.delete', ok: '恢复', body: '<div>恢复后该资源组重新生效，用户可再次看到其实例。</div>',
      onOk() { r.enabled = true; audit('rg.delete', '恢复资源组 ' + r.name); toast('已恢复'); rerender(); }});
    if (d.op === 'purge') {
      const refs = (Array.isArray(window.WORKFLOWS) ? window.WORKFLOWS.filter(w => w.rg == r.id).length : 0);
      if (rgUsers(r).length || rgInsts(r).length || refs) return openModal({title: '无法彻底删除 · ' + r.name, code: 'rg.delete', ok: '', cancel: '知道了',
        body: `<div class="notice bad"><span>⛔</span><div>该资源组仍有 ${rgUsers(r).length} 位成员、${rgInsts(r).length} 个实例${refs ? `、${refs} 个历史工单引用` : ''}。请先恢复该资源组、在“关联对象”中全部移除，再停用后删除。${refs ? '存在历史工单引用的资源组只能停用，不能彻底删除。' : ''}</div></div>`});
      return risky('rg.delete', '彻底删除资源组', `将从系统中永久删除资源组「${esc(r.name)}」及其审批流配置，无法恢复。`, `DELETE FROM dbt_resource_group WHERE group_id=${r.id};`, () => {
        RGROUPS.splice(RGROUPS.indexOf(r), 1); delete FLOWS[r.id]; audit('rg.delete', '彻底删除资源组 ' + r.name); toast('已删除'); rerender();
      }, r.name);
    }
    const busy = busyOf(r.id);
    if (busy.length) return openModal({title: '无法停用资源组 · ' + r.name, code: 'rg.delete', ok: '', cancel: '知道了',
      body: `<div class="notice bad"><span>⛔</span><div>拒绝停用：该资源组还有 <b>${busy.length}</b> 个进行中的工单（待审批的审批流，以及已通过但未结束的 SQL 工单：审核通过 / 定时执行 / 排队中 / 执行中）。请等它们结束或先终止后再停用。</div></div>
        ${tbl(['类型', '工单', '状态'], busy, {cells: b => [b.kind, `<a href="${b.href}">#${b.id}</a> ${esc(b.name)}`, tag(b.st, 'warn')]})}`});
    risky('rg.delete', '停用资源组', `停用资源组「${esc(r.name)}」：成员将看不到其实例，审批流不再生效，也不能再提交工单；可随时恢复。`,
      `UPDATE dbt_resource_group SET is_deleted=1 WHERE group_id=${r.id};`, () => { r.enabled = false; audit('rg.delete', '停用资源组 ' + r.name); toast('已停用'); rerender(); }, r.name);
  };

  /* 关联对象：穿梭框（用户 / 实例），改 USERS[].rgs 与 INSTANCES[].rgs，其他页面（可见实例、审批人）即时联动 */
  const RL = {id: 0, tab: 'u', u: new Set(), i: new Set(), selL: new Set(), selR: new Set(), ql: '', qr: ''};
  const relItems = () => RL.tab === 'u'
    ? USERS.filter(u => !dis(u)).map(u => ({id: u.id, t: `${u.login} ${u.name}`, s: u.super ? '超级用户' : u.groups.map(groupName).join('/')}))
    : INSTANCES.map(i => ({id: i.id, t: i.name, s: `${i.type} · ${i.env}${i.enabled === false ? ' · 已停用' : ''}`}));
  function relList(side) {
    const set = RL[RL.tab], q = (side === 'L' ? RL.ql : RL.qr).trim().toLowerCase(), sel = side === 'L' ? RL.selL : RL.selR;
    const items = relItems().filter(x => (side === 'L' ? !set.has(x.id) : set.has(x.id)) && (!q || (x.t + x.s).toLowerCase().includes(q)));
    return items.length ? items.map(x => `<label><input type="checkbox" data-onchange="rlSel" data-side="${side}" data-id="${x.id}" ${sel.has(x.id) ? 'checked' : ''}><span>${esc(x.t)}<div class="small muted">${esc(x.s)}</div></span></label>`).join('') : '<div class="empty" style="padding:20px">无</div>';
  }
  function relWarns() {
    const out = [], member = u => RL.u.has(u.id);
    FT_IDS.forEach(t => flowWarns(RL.id, t, member).forEach(w => { if (!flowWarns(RL.id, t).some(x => x.i === w.i)) out.push(`${FT[t].name} 第 ${w.i + 1} 级「${groupName(w.gid)}」将无人可审`); }));
    return out;
  }
  function relPaint() {
    const k = RL.tab, set = RL[k], orig = RL.orig[k], add = [...set].filter(x => !orig.has(x)).length, del = [...orig].filter(x => !set.has(x)).length;
    const w = relWarns();
    $('#relRoot').innerHTML = `<div class="subtabs" style="margin-bottom:10px">${[['u', `用户（${RL.u.size}）`], ['i', `实例（${RL.i.size}）`]].map(([v, t]) => `<a data-on="rlTab" data-v="${v}" class="${RL.tab === v ? 'on' : ''}">${t}</a>`).join('')}</div>
      <div class="sy-xfer"><div class="box"><h4><span>未关联</span></h4><div style="padding:6px"><input type="search" class="w-full" data-oninput="rlQ" data-side="L" placeholder="搜索" value="${esc(RL.ql)}"></div><div class="lst" id="relL">${relList('L')}</div></div>
        <div class="mid"><button class="btn" data-on="rlMove" data-to="R" title="添加所选">添加 ▶</button><button class="btn" data-on="rlMove" data-to="L" title="移除所选">◀ 移除</button></div>
        <div class="box"><h4><span>已关联到「${esc(rgName(RL.id))}」</span><span class="muted small">${set.size}</span></h4><div style="padding:6px"><input type="search" class="w-full" data-oninput="rlQ" data-side="R" placeholder="搜索" value="${esc(RL.qr)}"></div><div class="lst" id="relR">${relList('R')}</div></div></div>
      <div class="small muted" style="margin-top:8px">本次变更：新增 <b style="color:var(--ok)">${add}</b>，移除 <b style="color:var(--bad)">${del}</b></div>
      ${w.length ? `<div class="notice warn" style="margin-top:8px"><span>⚠</span><div>此调整会让审批流出现新的断点：<br>${w.map(esc).join('<br>')}</div></div>` : ''}`;
  }
  ON.rlTab = d => { RL.tab = d.v; RL.selL = new Set(); RL.selR = new Set(); RL.ql = RL.qr = ''; relPaint(); };
  ON.rlSel = (d, el) => { const s = d.side === 'L' ? RL.selL : RL.selR; el.checked ? s.add(+d.id) : s.delete(+d.id); };
  ON.rlQ = (d, el) => { if (d.side === 'L') RL.ql = el.value; else RL.qr = el.value; $('#rel' + d.side).innerHTML = relList(d.side); };
  ON.rlMove = d => {
    const set = RL[RL.tab], from = d.to === 'R' ? RL.selL : RL.selR;
    if (!from.size) return toast('请先勾选要' + (d.to === 'R' ? '添加' : '移除') + '的对象');
    from.forEach(id => d.to === 'R' ? set.add(id) : set.delete(id)); from.clear(); relPaint();
  };
  ACTIONS['rg.relations'] = d => {
    const r = rgById(d.id);
    if (!r.enabled) return toast('已停用的资源组不能调整关联，请先恢复');
    Object.assign(RL, {id: r.id, tab: 'u', selL: new Set(), selR: new Set(), ql: '', qr: '', u: new Set(rgUsers(r).map(u => u.id)), i: new Set(rgInsts(r).map(i => i.id))});
    RL.orig = {u: new Set(RL.u), i: new Set(RL.i)};
    openModal({title: '关联对象 · ' + r.name, code: 'rg.relations', wide: true, ok: '保存关联', body: '<div id="relRoot"></div>', onOk() {
      const du = USERS.filter(u => !dis(u) && RL.u.has(u.id) !== u.rgs.includes(r.id)), di = INSTANCES.filter(i => RL.i.has(i.id) !== i.rgs.includes(r.id));
      if (!du.length && !di.length) { toast('没有变化'); return; }
      const addU = du.filter(u => RL.u.has(u.id)), delU = du.filter(u => !RL.u.has(u.id)), addI = di.filter(i => RL.i.has(i.id)), delI = di.filter(i => !RL.i.has(i.id));
      du.forEach(u => { u.rgs = RL.u.has(u.id) ? u.rgs.concat(r.id) : u.rgs.filter(x => x !== r.id); });
      di.forEach(i => { i.rgs = RL.i.has(i.id) ? i.rgs.concat(r.id) : i.rgs.filter(x => x !== r.id); });
      if (du.length) audit('rg.relations', `调整资源组关联用户 ${r.name}：+${addU.length} −${delU.length}`, '成功', [...addU.map(u => '+ ' + u.login), ...delU.map(u => '- ' + u.login)].join(' '));
      if (di.length) audit('rg.relations', `调整资源组关联实例 ${r.name}：+${addI.length} −${delI.length}`, '成功', [...addI.map(i => '+ ' + i.name), ...delI.map(i => '- ' + i.name)].join(' '));
      toast('关联已保存'); refreshShell(); rerender();
    }});
    relPaint();
  };

  /* =====================================================================
     用户（user）
     ===================================================================== */
  const US = {q: '', g: '', rg: '', st: '', sort: 'last', drawer: 0, ef: 'all'};
  const reLogin = /^[A-Za-z0-9@.+\-_]{1,30}$/, reNotify = /^[A-Za-z0-9_\-.:@]{0,64}$/, reMail = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
  /* 密码策略校验在 login.js 的 pwdCheck（最小长度 / 复杂度取自「登录与安全设置」，其余规则同真实 internal/sysadmin/password.go） */
  const pwdErr = (pw, u) => pwdCheck(pw, [u.login, u.email, u.name], 'admin');
  /* 临时密码：同真实 generateTempPassword —— 长度 = 最小长度 + 4，夹在 14–32 之间；每类字符至少一个，去掉易混字符；最多试 20 次，生成不出满足策略的（如最小长度 > 32）返回 null */
  const genPwd = u => { const r = s => s[Math.floor(Math.random() * s.length)], n = Math.min(32, Math.max(14, (SETTINGS.pwdMinLen || 9) + 4)), all = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789!@#$%^&*';
    for (let t = 0; t < 20; t++) {
      const a = [r('ABCDEFGHJKLMNPQRSTUVWXYZ'), r('abcdefghijkmnpqrstuvwxyz'), r('23456789'), r('!@#$%^&*')]; while (a.length < n) a.push(r(all));
      const pw = a.sort(() => Math.random() - .5).join(''); if (!pwdErr(pw, u)) return pw;
    }
    return null; };
  const groupsHtml = u => (u.super ? '<span class="tag brand">超级用户</span> ' : '') + (u.groups.map(g => `<span class="tag">${esc(groupName(g))}</span>`).join(' ') || (u.super ? '' : '<span class="muted">—</span>'));
  const stHtml = u => u.status === 'active' ? '<span class="tag ok">正常</span>' : u.status === 'locked' ? `<span class="tag bad">已锁定</span><div class="small muted">失败 ${u.fail} 次（≥ ${SETTINGS.loginFailLock} 次）</div>` : '<span class="tag">已停用</span>';
  const activeSupers = () => USERS.filter(x => x.super && x.status === 'active');
  const nfHtml = u => ['ding', 'feishu', 'slack'].filter(k => u.notify[k]).map(k => HOOK_N[k] + ' ✓').join(' ') || '—';

  function usFiltered() {
    const q = US.q.trim().toLowerCase();
    const list = USERS.filter(u => (!q || (u.login + u.name + u.email + u.dept).toLowerCase().includes(q)) && (!US.g || (US.g === '_super' ? u.super : u.groups.includes(US.g))) &&
      (!US.rg || u.rgs.includes(+US.rg)) && (!US.st || (US.st === 'no2fa' ? !u.twofa : u.status === US.st)));
    const key = {last: u => u.last, login: u => u.login, status: u => u.status};
    return list.sort((a, b) => US.sort === 'last' ? (key.last(b) > key.last(a) ? 1 : -1) : String(key[US.sort](a)).localeCompare(String(key[US.sort](b))));
  }
  function usTable() {
    const list = usFiltered();
    return tbl(['账号', '姓名 / 邮箱', '部门', '权限组', '资源组', '认证方式', '2FA', '状态', '最近登录', '操作'], list, {
      attrs: u => u.status === 'locked' ? ' class="sy-warn"' : '',
      cells: u => [`<b class="mono">${esc(u.login)}</b>${u.id === ME.id ? ' <span class="tag brand">当前</span>' : ''}`, `${esc(u.name)}<div class="small muted">${esc(u.email)}</div>`, esc(u.dept), groupsHtml(u),
        `<span class="small">${esc(u.rgs.map(rgName).join('、') || '—')}</span>`, esc(u.auth), u.twofa ? 'TOTP' : '<span class="muted">未绑定</span>', stHtml(u), `<span class="small">${esc(u.last)}</span>`,
        `<div class="ops">${B('user.edit', '编辑', {cls: 'sm', data: {op: 'edit', id: u.id}})}<button class="btn sm" data-on="usDetail" data-id="${u.id}">详情 / 有效权限</button>${u.status === 'locked' ? B('user.unlock', '解锁', {cls: 'sm primary', data: {id: u.id}}) : ''}${u.status === 'disabled' ? B('user.disable', '启用', {cls: 'sm', data: {id: u.id}}) : B('user.disable', '停用', {cls: 'sm danger', data: {id: u.id}})}</div>`]
    }) + `<div class="pager"><span>共 ${list.length} 人${list.length !== USERS.length ? `（全部 ${USERS.length} 人）` : ''}</span></div>`;
  }
  pages.user = {
    render() {
      const locked = USERS.filter(u => u.status === 'locked').length, no2 = USERS.filter(u => !u.twofa && u.status === 'active').length;
      return `<div class="grid g4" style="margin-bottom:14px">
        <div class="card kpi"><div class="l">用户总数</div><div class="v">${USERS.length}</div><div class="d flat">${USERS.filter(u => u.status === 'active').length} 人正常</div></div>
        <div class="card kpi"><div class="l">超级用户</div><div class="v">${activeSupers().length}</div></div>
        <div class="card kpi"><div class="l">已锁定</div><div class="v" style="color:${locked ? 'var(--bad)' : 'inherit'}">${locked}</div></div>
        <div class="card kpi"><div class="l">未绑定 2FA</div><div class="v">${no2}</div></div></div>
      <div class="card"><div class="toolbar"><input type="search" data-oninput="usQ" placeholder="搜索账号 / 姓名 / 邮箱 / 部门" value="${esc(US.q)}" style="min-width:230px">
        <select data-onchange="usF" data-k="g" aria-label="权限组"><option value="">全部权限组</option><option value="_super" ${US.g === '_super' ? 'selected' : ''}>超级用户</option>${GROUPS.map(g => `<option value="${g.id}" ${US.g === g.id ? 'selected' : ''}>${esc(g.name)}</option>`).join('')}</select>
        <select data-onchange="usF" data-k="rg" aria-label="资源组"><option value="">全部资源组</option>${RGROUPS.map(r => `<option value="${r.id}" ${US.rg == r.id ? 'selected' : ''}>${esc(r.name)}${r.enabled ? '' : '（停用）'}</option>`).join('')}</select>
        <select data-onchange="usF" data-k="st" aria-label="状态">${[['', '全部状态'], ['active', '正常'], ['locked', '已锁定'], ['disabled', '已停用'], ['no2fa', '未绑定 2FA']].map(([v, t]) => `<option value="${v}" ${US.st === v ? 'selected' : ''}>${t}</option>`).join('')}</select>
        <select data-onchange="usF" data-k="sort" aria-label="排序">${[['last', '按最近登录'], ['login', '按账号'], ['status', '按状态']].map(([v, t]) => `<option value="${v}" ${US.sort === v ? 'selected' : ''}>${t}</option>`).join('')}</select>
        <span class="sp"></span>${B('user.edit', '＋ 新建用户', {cls: 'primary', data: {op: 'new'}})}</div>
        <div id="usTable">${usTable()}</div></div>`;
    }
  };
  ON.usQ = (d, el) => { US.q = el.value; $('#usTable').innerHTML = usTable(); };
  ON.usF = (d, el) => { US[d.k] = el.value; $('#usTable').innerHTML = usTable(); };
  ON.usDetail = d => userDrawer(+d.id);

  function ovrRows(u) {
    const rs = OVR.filter(o => o.uid === u.id);
    return rs.length ? tbl(['按钮', '方式', '到期', '原因', '操作人', ''], rs, {cells: o => [`${esc(lbl(o.code))}<div class="mono small muted">${o.code}</div>`,
      o.effect === 'allow' ? '<span class="tag ok">单独授权</span>' : '<span class="tag bad">单独禁用</span>', o.expire ? (ovrLive(o) ? esc(o.expire) : `<span class="tag">${esc(o.expire)} 已到期</span>`) : '长期', `<span class="small">${esc(o.reason)}</span>`, `<span class="small">${esc(o.by)}<br>${esc(o.at)}</span>`,
      B('ag.btnperm', '撤销', {cls: 'sm', data: {op: 'revoke', uid: u.id, code: o.code}})]}) : '<div class="small muted">暂无单独授权 / 禁用记录。</div>';
  }
  function effTable(u, flt) {
    const eff = effOf(u); let rows = '';
    PERM_TREE.forEach(m => m.pages.forEach(p => {
      p.btns.filter(b => { const e = eff[b[0]]; return flt === 'all' || (flt === 'allow' && e.ok) || (flt === 'deny' && !e.ok) || (flt === 'ovr' && e.rec); }).forEach((b, i) => {
        const e = eff[b[0]];
        const src = e.rec ? `<b>${e.src.startsWith('权限组') ? esc(e.src) + ' ＋ 单独授权' : esc(e.src)}</b> <span class="muted">（${esc(e.rec.by)} ${esc(e.rec.at.slice(0, 10))}${e.rec.expire ? '，到期 ' + esc(e.rec.expire) : ''}：${esc(e.rec.reason)}）</span>` : esc(e.src);
        rows += `<tr class="${e.rec ? (e.ok ? '' : 'sy-warn') : ''}"><td class="small">${i ? '' : esc(m.name + ' / ' + p.name)}</td><td>${esc(b[1])}${b[2] === 'd' ? ' <span class="risk">⚠</span>' : ''}</td><td class="mono small">${b[0]}</td>
          <td>${e.ok ? '<span class="tag ok">允许</span>' : e.src === '单独禁用' ? '<span class="tag bad">禁止</span>' : '<span class="tag">无</span>'}</td><td class="small">${src}</td></tr>`;
      });
    }));
    return `<div class="tbl-wrap sy-scroll"><table><thead><tr><th>页面</th><th>按钮</th><th>权限码</th><th>结果</th><th>来源</th></tr></thead><tbody>${rows || `<tr><td colspan="5">${empty('没有符合条件的按钮')}</td></tr>`}</tbody></table></div>`;
  }
  function effPanel(u) {
    const eff = effOf(u), codes = ALL_CODES(), ok = codes.filter(c => eff[c].ok).length;
    const bySrc = k => codes.filter(c => eff[c].ok && eff[c].src.startsWith(k)).length;
    return `<div class="row" style="margin:6px 0 10px"><span class="small">共 ${codes.length} 个按钮，允许 <b>${ok}</b>${u.super ? '（超级用户拥有全部）' : `：权限组 ${bySrc('权限组')} · 单独授权 ${bySrc('单独授权')} · 被单独禁用 ${codes.filter(c => eff[c].src === '单独禁用').length}`}</span><span class="sp"></span>
      <select data-onchange="efFlt" data-uid="${u.id}" aria-label="筛选">${[['all', '全部按钮'], ['allow', '仅允许'], ['deny', '仅禁止 / 未授予'], ['ovr', '仅单独授权 / 禁用']].map(([v, t]) => `<option value="${v}" ${US.ef === v ? 'selected' : ''}>${t}</option>`).join('')}</select></div>
      <div id="efBox">${effTable(u, US.ef)}</div>`;
  }
  ON.efFlt = (d, el) => { US.ef = el.value; $('#efBox').innerHTML = effTable(uById(d.uid), US.ef); };

  function userDrawer(id) {
    const u = uById(id); US.drawer = id;
    openDrawer(`${u.login} · ${u.name}`, `${kvList([['账号', `<b class="mono">${esc(u.login)}</b>${u.id === ME.id ? ' <span class="tag brand">当前登录</span>' : ''}`], ['姓名 / 部门', `${esc(u.name)} · ${esc(u.dept)}`], ['邮箱', esc(u.email)], ['认证方式', `${esc(u.auth)} · 2FA：${u.twofa ? 'TOTP' : '未绑定'}`],
      ['状态', stHtml(u)], ['权限组', groupsHtml(u)], ['资源组', esc(u.rgs.map(rgName).join('、') || '—')], ['通知标识', esc(nfHtml(u))], ['最近登录', `${esc(u.last)}（${esc(u.ip)}）`]])}
      <div class="row" style="margin:12px 0">${B('user.edit', '编辑', {cls: 'sm', data: {op: 'edit', id: u.id}})}${u.status === 'locked' ? B('user.unlock', '解锁', {cls: 'sm primary', data: {id: u.id}}) : ''}${u.status === 'disabled' ? B('user.disable', '启用', {cls: 'sm', data: {id: u.id}}) : B('user.disable', '停用', {cls: 'sm danger', data: {id: u.id}})}
        ${B('user.reset', '重置密码', {cls: 'sm', data: {op: 'pwd', id: u.id}})}${B('user.reset', '重置 2FA', {cls: 'sm', data: {op: '2fa', id: u.id}})}${B('user.grant_super', u.super ? '收回超级用户' : '授予超级用户', {cls: 'sm danger', data: {op: u.super ? 'revoke' : 'grant', id: u.id}})}</div>
      <h3 style="font-size:14px;margin:14px 0 6px">单独授权 / 禁用 <span class="right" style="float:right">${B('ag.btnperm', '＋ 单独授权 / 禁用…', {cls: 'sm', data: {op: 'ovr', uid: u.id}})}</span></h3>${ovrRows(u)}
      <h3 style="font-size:14px;margin:16px 0 0">有效权限（每个按钮的来源）</h3>${effPanel(u)}`, true);
  }

  /* ---- 新建 / 编辑 ---- */
  function userForm(u) {
    const isNew = !u, notify = u ? u.notify : {};
    return `<div class="form">${fld('用户名（≤30 字）', `<input id="ufL" maxlength="30" value="${esc(u ? u.login : '')}" ${u ? 'disabled' : ''}>`, {req: true})}
      ${fld('姓名（≤10 字）', `<input id="ufN" maxlength="10" value="${esc(u ? u.name : '')}">`, {req: true})}
      ${fld('邮箱', `<input id="ufE" value="${esc(u ? u.email : '')}" placeholder="name@example.com">`)}
      ${fld('部门', `<input id="ufD" value="${esc(u ? u.dept : '')}">`)}
      ${isNew ? fld('认证方式', `<select id="ufA"><option>本地</option><option>LDAP</option></select>`) + fld('初始密码（至少 9 位；LDAP 用户不填）', `<input type="password" id="ufP" autocomplete="new-password">`, {req: true}) : ''}
      ${fld('钉钉 UserID', `<input id="ufNd" value="${esc(notify.ding || '')}">`)}${fld('飞书 OpenID', `<input id="ufNf" value="${esc(notify.feishu || '')}">`)}${fld('Slack ID', `<input id="ufNs" value="${esc(notify.slack || '')}">`)}
      ${fld('权限组', GROUPS.map(g => `<label class="chk" style="margin-right:12px"><input type="checkbox" class="ufG" value="${g.id}" ${(u ? u.groups.includes(g.id) : g.id === 'g_default') ? 'checked' : ''}>${esc(g.name)}</label>`).join(''))}
      ${fld('资源组', RGROUPS.filter(r => r.enabled).map(r => `<label class="chk" style="margin-right:12px"><input type="checkbox" class="ufR" value="${r.id}" ${u && u.rgs.includes(r.id) ? 'checked' : ''}>${esc(r.name)}</label>`).join(''))}</div>`;
  }
  const checked = cls => $$(cls).filter(x => x.checked).map(x => x.value);
  ACTIONS['user.edit'] = d => {
    const u = d.op === 'new' ? null : uById(d.id);
    openModal({title: u ? '编辑用户 · ' + u.login : '新建用户', code: 'user.edit', wide: true, body: userForm(u), onOk() {
      const name = $('#ufN').value.trim(), email = $('#ufE').value.trim(), dept = $('#ufD').value.trim(), nt = {ding: $('#ufNd').value.trim(), feishu: $('#ufNf').value.trim(), slack: $('#ufNs').value.trim()};
      if (!name || name.length > 10) { toast('姓名需为 1–10 个字'); return false; }
      if (email && !reMail.test(email)) { toast('邮箱格式不正确'); return false; }
      for (const k in nt) if (!reNotify.test(nt[k])) { toast(HOOK_N[k] + ' 标识只允许字母数字和 _-.:@'); return false; }
      const groups = checked('.ufG'), rgs = checked('.ufR').map(Number);
      if (!u) {
        const login = $('#ufL').value.trim();
        if (!reLogin.test(login)) { toast('用户名只允许字母数字和 @.+-_，1–30 字'); return false; }
        if (USERS.some(x => x.login === login)) { toast('用户名已存在'); return false; }
        const auth = $('#ufA').value;
        if (auth === '本地') { const e = pwdErr($('#ufP').value, {login, email, name}); if (e) { toast(e); return false; } }
        USERS.push({id: Math.max(...USERS.map(x => x.id)) + 1, login, name, email, dept, super: false, groups, rgs, status: 'active', auth, twofa: false, last: '—', ip: '—', extraGrant: [], extraDeny: [], fail: 0, notify: nt});
        audit('user.edit', `新建用户 ${login}（${groups.map(groupName).join('/') || '无权限组'}；资源组 ${rgs.map(rgName).join('/') || '无'}）`);
        toast('已新建用户 ' + login);
      } else {
        const before = u.groups.map(groupName).join('/') + ' | ' + u.rgs.map(rgName).join('/');
        const keep = u.rgs.filter(r => { const g = rgById(r); return g && !g.enabled; });
        Object.assign(u, {name, email, dept, notify: nt, groups, rgs: rgs.concat(keep)});
        const after = u.groups.map(groupName).join('/') + ' | ' + u.rgs.map(rgName).join('/');
        audit('user.edit', `修改用户 ${u.login}${before !== after ? `：权限组/资源组 ${before} ⇒ ${after}` : ''}`); toast('已保存');
      }
      refreshShell(); redo();
    }});
  };
  ACTIONS['user.unlock'] = d => { const u = uById(d.id); u.fail = 0; if (typeof LG !== 'undefined') { LG.fails[u.login] = 0; delete LG.failAt[u.login]; } if (u.status === 'locked') u.status = 'active'; audit('user.unlock', `解锁账号 ${u.login}（失败次数清零）`); toast(`已解锁 ${u.login}`); refreshShell(); redo(); };
  ACTIONS['user.disable'] = d => {
    const u = uById(d.id);
    if (dis(u)) { u.status = 'active'; u.fail = 0; audit('user.disable', '启用用户 ' + u.login); toast('已启用 ' + u.login); refreshShell(); return redo(); }
    if (u.id === ME.id) return toast('不能停用自己');
    if (u.super && !activeSupers().some(x => x.id !== u.id)) return toast('不能停用最后一个超级用户');
    risky('user.disable', '停用用户', `停用 <b>${esc(u.login)}（${esc(u.name)}）</b>：该账号将无法登录，并从审批人中移除；可随时重新启用。`,
      `UPDATE dbt_sql_users SET is_active=0 WHERE username='${u.login}';`, () => { u.status = 'disabled'; audit('user.disable', '停用用户 ' + u.login); toast('已停用 ' + u.login); refreshShell(); redo(); });
  };
  ACTIONS['user.reset'] = d => {
    const u = uById(d.id);
    if (d.op === '2fa') {
      if (!u.twofa) return toast(`${u.login} 没有绑定两步验证，无需重置`);
      return risky('user.reset', '重置两步验证', `删除 <b>${esc(u.login)}</b> 的全部 2FA 绑定。下次登录${SETTINGS.force2fa ? '将被强制要求重新绑定' : '将不再要求输入动态码，直到 TA 重新绑定'}。`,
        `DELETE FROM dbt_2fa_config WHERE user_id=(SELECT id FROM dbt_sql_users WHERE username='${u.login}');`, () => { u.twofa = false; audit('user.reset', '重置 2FA ' + u.login); toast('已重置两步验证'); redo(); });
    }
    if (u.auth !== '本地') return toast(`${u.login} 使用 ${u.auth} 认证，密码由外部系统管理，不能在此重置`);
    /* 同真实：先选方式（系统生成临时密码 / 管理员手输），再进入统一确认框 */
    openModal({title: '重置密码：' + u.login, code: 'user.reset', okCls: 'danger primary', ok: '重置', body: `<div style="display:grid;gap:8px">
      <label class="chk"><input type="radio" name="rpMode" id="rpGen" checked data-onchange="rpMode"> 系统生成临时密码（仅显示一次）＋ 首次登录强制改密</label>
      <label class="chk"><input type="radio" name="rpMode" id="rpMan" data-onchange="rpMode"> 管理员手输新密码</label>
      <div id="rpManual" hidden><div class="form">${fld('新密码（至少 9 位）', '<input type="password" id="rpP1" autocomplete="new-password">')}${fld('再输入一次', '<input type="password" id="rpP2" autocomplete="new-password">')}${fld('', '<label class="chk"><input type="checkbox" id="rpForce"> 要求用户下次登录先修改密码</label>')}</div></div>
      </div>`,
      onOk() {
        const man = $('#rpMan').checked, force = man && $('#rpForce').checked, p1 = man ? $('#rpP1').value : '';
        if (man) {
          if (p1 !== $('#rpP2').value) { toast('两次输入的密码不一致'); return false; }
          const e = pwdErr(p1, u); if (e) { toast(e); return false; }
        }
        const sql = `UPDATE dbt_sql_users SET password='pbkdf2_sha256$390000$…' WHERE username='${u.login}';`;
        if (!man) return risky('user.reset', '重置密码', `将为 <b>${esc(u.login)}</b> 生成一个临时密码，<b>只显示一次</b>；用户首次登录须修改。`, sql, () => {
          const pw = genPwd(u); if (!pw) return toast('无法生成满足密码策略的临时密码，请改为手输');
          audit('user.reset', '重置密码 ' + u.login + '（系统生成临时密码，新密码未记录；首次登录强制改密）');
          setTimeout(() => openModal({title: '新密码（仅显示一次）· ' + u.login, ok: '', cancel: '我已记录，关闭', body: `${notice('请立即告知用户。关闭此窗口后将<b>无法再次查看</b>。', 'warn', '🔑')}${code(pw)}`}), 0);
        }, u.login);
        risky('user.reset', '重置密码', `将把 <b>${esc(u.login)}</b> 的密码改为你刚输入的新密码${force ? '，用户下次登录须先修改密码。' : '。'}密码不会写入审计。`, sql, () => {
          audit('user.reset', '重置密码 ' + u.login + '（管理员手输，新密码未记录' + (force ? '；下次登录强制改密' : '') + '）');
          toast(`已重置 ${u.login} 的密码${force ? '，用户下次登录须先修改密码' : ''}`);
        }, u.login);
      }});
  };
  ON.rpMode = () => { const m = $('#rpManual'); if (m) m.hidden = !$('#rpMan').checked; };
  ACTIONS['user.grant_super'] = d => {
    const u = uById(d.id);
    if (d.op === 'revoke') {
      if (u.id === ME.id) return toast('不能降级自己');
      if (!activeSupers().some(x => x.id !== u.id)) return toast('不能降级最后一个超级用户');
      return risky('user.grant_super', '收回超级用户', `收回 <b>${esc(u.login)}</b> 的超级用户身份，之后其权限按权限组与单独授权计算。`, `UPDATE dbt_sql_users SET is_superuser=0 WHERE username='${u.login}';`,
        () => { u.super = false; audit('user.grant_super', '收回超级用户 ' + u.login); toast('已收回'); refreshShell(); redo(); });
    }
    if (dis(u)) return toast('请先启用该用户');
    risky('user.grant_super', '授予超级用户', `<b>${esc(u.login)}（${esc(u.name)}）</b>将<b>绕过全部权限检查</b>，并可使用系统管理的所有页面。`,
      `UPDATE dbt_sql_users SET is_superuser=1, is_staff=1 WHERE username='${u.login}';`, () => { u.super = true; audit('user.grant_super', '授予超级用户 ' + u.login); toast('已授予超级用户'); refreshShell(); redo(); }, u.login);
  };

  /* =====================================================================
     权限组（ag）
     ===================================================================== */
  const AG = {gid: 'g_dba', draft: null, draftGid: '', q: '', fold: new Set(), prev: 'sqlworkflow', uid: 4};
  const curG = () => { let g = gById(AG.gid); if (!g) { g = GROUPS[0]; AG.gid = g.id; } return g; };
  function ensureDraft() { const g = curG(); if (AG.draftGid !== g.id || !AG.draft) { AG.draft = new Set(g.perms); AG.draftGid = g.id; } }
  const flowRefs = gid => { const out = []; RGROUPS.forEach(r => FT_IDS.forEach(t => curFlow(r.id, t).forEach((g, i) => { if (g === gid) out.push({rg: r, t, i}); }))); return out; };
  const membersOf = gid => USERS.filter(u => u.groups.includes(gid));
  const grantableN = () => GRANTABLE().length;

  pages.ag = {
    render(sub) {
      ensureDraft();
      const tab = ['perm', 'user'].includes(sub) ? sub : 'basic', g = curG();
      return `${modeCard()}<div class="grid g-side" style="margin-top:14px">
        <div class="card" style="align-self:start"><h2>权限组 <span class="sub">${GROUPS.length}</span><span class="right">${B('ag.edit', '＋ 新建', {cls: 'sm primary', data: {op: 'new'}})}</span></h2>
          <div class="role-list">${GROUPS.map(x => `<a data-on="agSel" data-id="${x.id}" class="${x.id === g.id ? 'on' : ''}">⛨ ${esc(x.name)}<span style="margin-left:auto" class="small muted">${x.builtin ? '<span class="tag">内置</span> ' : ''}${membersOf(x.id).length} 人</span></a>`).join('')}</div>
          </div>
        <div class="card" style="min-width:0"><h2 style="font-size:16px">${esc(g.name)} ${g.builtin ? '<span class="tag">内置</span>' : '<span class="tag brand">自定义</span>'}<span class="sub">${esc(g.desc)}</span></h2>
          ${tabs('ag', [['basic', '基本信息与成员'], ['perm', '按钮权限'], ['user', '按用户查看有效权限']], tab)}
          ${tab === 'basic' ? agBasic(g) : tab === 'perm' ? agPerm(g) : agUser()}</div></div>`;
    }
  };
  ON.agSel = d => { AG.gid = d.id; AG.draftGid = ''; rerender(); };

  function modeCard() {
    const strict = SETTINGS.btnMode === 'strict';
    return `<div class="card"><h2>按钮权限生效方式 <span class="sub">${strict ? '当前：严格模式' : '当前：兼容模式（默认）'}</span><span class="right">${seg('ag.btnperm', [['compat', '兼容模式'], ['strict', '严格模式']], SETTINGS.btnMode, 'mode')}</span></h2>
      <div class="row small muted"><span>无权限按钮呈现：</span>${seg('ag.btnperm', [['disable', '置灰并提示'], ['hide', '隐藏']], SETTINGS.noPermBtn, 'deny')}</div></div>`;
  }

  /* ---- ① 基本信息与成员 ---- */
  function agBasic(g) {
    const mem = membersOf(g.id), refs = flowRefs(g.id), n = g.perms.filter(c => BTNS[c] && !BTNS[c].locked).length;
    return `<div class="grid g2" style="gap:14px"><div><h3 style="font-size:14px;margin:0 0 8px">基本信息</h3>
        <div class="form">${fld('组名（≤150 字）', `<input id="agName" maxlength="150" value="${esc(g.name)}" ${g.builtin ? 'disabled' : ''}>`)}${fld('说明', `<input id="agDesc" value="${esc(g.desc)}">`)}</div>
        <div class="row" style="margin-top:10px">${B('ag.edit', '保存基本信息', {cls: 'primary', data: {op: 'meta'}})}${B('ag.delete', '删除权限组', {cls: 'danger', data: {gid: g.id}})}</div>
        <div class="hr"></div><div class="stat"><div><b>${mem.length}</b><span>成员</span></div><div><b>${n}</b><span>已授予按钮（共 ${grantableN()} 个可授予）</span></div><div><b>${refs.length}</b><span>被审批流引用</span></div></div></div>
      <div><h3 style="font-size:14px;margin:0 0 8px">成员</h3>
        ${mem.length ? tbl(['账号', '姓名', '资源组', '状态'], mem, {cells: u => [`<b class="mono">${esc(u.login)}</b>`, esc(u.name), `<span class="small">${esc(u.rgs.map(rgName).join('、') || '—')}</span>`, stHtml(u)]}) : empty('暂无成员')}
        <div class="small" style="margin-top:6px"><a href="#user">去用户管理 →</a></div>
        <h3 style="font-size:14px;margin:16px 0 8px">审批流引用</h3>${refs.length ? `<div class="small">${refs.map(x => `<div>${esc(x.rg.name)} · ${FT[x.t].name} · 第 ${x.i + 1} 级</div>`).join('')}</div><div class="small" style="margin-top:4px"><a href="#flow/rg">查看审批流 →</a></div>` : '<div class="small muted">未被任何审批流引用。</div>'}</div></div>`;
  }

  /* ---- ② 按钮权限（权限树） ---- */
  const btnChk = ([c, label, kind, lock]) => `<label class="chk sy-b ${lock ? 'sy-lock' : ''}" title="${esc(c)}${lock ? '：仅超级用户，不可授予权限组' : ''}"><input type="checkbox" data-onchange="agChk" data-c="${c}" ${AG.draft.has(c) ? 'checked' : ''} ${lock ? 'disabled' : ''}>${esc(label)}${kind === 'd' ? ' <span class="risk">⚠高危</span>' : ''}${lock ? ' <span class="tag">仅超级用户</span>' : ''} <span class="code">${c}</span></label>`;
  function treeHtml() {
    const D = AG.draft, q = AG.q.trim().toLowerCase();
    const out = PERM_TREE.map(m => {
      const pages = m.pages.map(p => ({p, btns: p.btns.filter(b => !q || (b[0] + b[1] + p.name + m.name).toLowerCase().includes(q))})).filter(x => x.btns.length);
      if (!pages.length) return '';
      const gr = m.pages.flatMap(p => p.btns).filter(b => b[3] !== 'L').map(b => b[0]), n = gr.filter(c => D.has(c)).length, fold = AG.fold.has(m.name) && !q;
      return `<div class="pg"><input type="checkbox" data-onchange="agMod" data-m="${esc(m.name)}" ${gr.length && n === gr.length ? 'checked' : ''} ${gr.length ? '' : 'disabled'}><span data-on="agFold" data-m="${esc(m.name)}" style="cursor:pointer;flex:1">${fold ? '▸' : '▾'} ${esc(m.name)} <span class="small muted" style="font-weight:400">${n}/${gr.length}${gr.length ? '' : ' · 全部仅超级用户'}</span></span></div>` +
        (fold ? '' : pages.map(({p, btns}) => { const pg = p.btns.filter(b => b[3] !== 'L').map(b => b[0]), pn = pg.filter(c => D.has(c)).length;
          return `<div class="pm"><div><label class="chk"><input type="checkbox" data-onchange="agPg" data-p="${p.key}" ${pg.length && pn === pg.length ? 'checked' : ''} ${pg.length ? '' : 'disabled'}><b>${esc(p.name)}</b></label><div class="small muted" style="margin-left:22px">${pn}/${pg.length} · <a data-on="agPrev" data-p="${p.key}">预览</a></div></div><div class="pbtns">${btns.map(btnChk).join('')}</div></div>`; }).join(''));
    }).join('');
    return out || empty('没有匹配的页面 / 按钮');
  }
  function statHtml() {
    const g = curG(), D = AG.draft, add = [...D].filter(c => !g.perms.includes(c)), del = g.perms.filter(c => !D.has(c)), dn = [...D].filter(c => BTNS[c] && BTNS[c].kind === 'd').length;
    return `已授予 <b>${[...D].filter(c => BTNS[c] && !BTNS[c].locked).length}</b> / ${grantableN()} 个按钮 · 其中高危 <b class="up">${dn}</b>
      ${add.length || del.length ? ` · <span style="color:var(--warn)">● 未保存：<b style="color:var(--ok)">+${add.length}</b> <b style="color:var(--bad)">−${del.length}</b></span>` : ' · <span class="muted">无未保存的修改</span>'}`;
  }
  function prevHtml() {
    const p = PAGES[AG.prev] || PAGES.sqlworkflow, D = AG.draft, vs = p.btns.filter(b => b[2] === 'v'), open = vs.some(b => D.has(b[0])) || (NAV[p.key] && NAV[p.key].perm.some(c => D.has(c)));
    return `<div class="row" style="margin-bottom:8px"><select data-onchange="agPrevSel" aria-label="预览页面">${PERM_TREE.map(m => `<optgroup label="${esc(m.name)}">${m.pages.map(x => `<option value="${x.key}" ${x.key === p.key ? 'selected' : ''}>${esc(x.name)}</option>`).join('')}</optgroup>`).join('')}</select>
      <span class="small">${open ? '<span class="tag ok">菜单可见 · 页面可打开</span>' : `<span class="tag bad">页面打不开</span> <span class="muted">没有任何“查看类”按钮</span>`}</span></div>
      <div style="border:1px dashed var(--border);border-radius:10px;padding:10px 12px"><div class="small muted" style="margin-bottom:4px">「${esc(p.name)}」页面上的按钮</div>
      ${p.btns.map(b => D.has(b[0]) ? `<button class="btn sy-fb ${b[2] === 'd' ? 'danger' : ''}" type="button" title="${esc(b[0])}">${esc(b[1])}${b[2] === 'd' ? ' ⚠' : ''}</button>` : `<button class="btn sy-fb noperm" type="button" disabled title="${b[3] === 'L' ? '仅超级用户可用' : '无权限：需要「' + esc(b[1]) + '」'}（${b[0]}）">${esc(b[1])}</button>`).join('')}</div>`;
  }
  const paintAg = () => { $('#agTree').innerHTML = treeHtml(); $('#agStat').innerHTML = statHtml(); $('#agPrevBox').innerHTML = prevHtml(); };

  function agPerm(g) {
    return `<div class="toolbar"><input type="search" data-oninput="agQ" placeholder="搜索页面 / 按钮 / 权限码" value="${esc(AG.q)}" style="min-width:220px">
      <select data-onchange="agPreset" aria-label="预设"><option value="">套用预设…</option>${Object.keys(PRESETS).map(n => `<option>${n}</option>`).join('')}</select>
      <button class="btn" data-on="agBulk" data-v="view">仅查看类</button><button class="btn" data-on="agBulk" data-v="all">全选可授予</button><button class="btn" data-on="agBulk" data-v="none">清空</button>
      ${B('ag.edit', '按原权限码生成…', {data: {op: 'migrate'}})}<span class="sp"></span><button class="btn" data-on="agBulk" data-v="reset">撤销修改</button>${B('ag.edit', '保存修改…', {cls: 'primary', data: {op: 'save'}})}</div>
      <div class="small" style="margin-bottom:8px" id="agStat">${statHtml()}</div>
      <div class="ptree" id="agTree">${treeHtml()}</div>
      <h3 style="font-size:14px;margin:16px 0 6px">预览页面效果</h3><div id="agPrevBox">${prevHtml()}</div>`;
  }
  ON.agQ = (d, el) => { AG.q = el.value; paintAg(); };
  ON.agFold = d => { AG.fold.has(d.m) ? AG.fold.delete(d.m) : AG.fold.add(d.m); paintAg(); };
  ON.agChk = (d, el) => { el.checked ? AG.draft.add(d.c) : AG.draft.delete(d.c); paintAg(); };
  const setMany = (codes, on) => codes.forEach(c => on ? AG.draft.add(c) : AG.draft.delete(c));
  ON.agPg = (d, el) => { setMany(PAGES[d.p].btns.filter(b => b[3] !== 'L').map(b => b[0]), el.checked); paintAg(); };
  ON.agMod = (d, el) => { setMany(PERM_TREE.find(m => m.name === d.m).pages.flatMap(p => p.btns).filter(b => b[3] !== 'L').map(b => b[0]), el.checked); paintAg(); };
  ON.agPrev = d => { AG.prev = d.p; paintAg(); const b = $('#agPrevBox'); if (b) b.scrollIntoView({behavior: 'smooth', block: 'center'}); };
  ON.agPrevSel = (d, el) => { AG.prev = el.value; paintAg(); };
  ON.agBulk = d => {
    if (d.v === 'all') AG.draft = new Set(GRANTABLE());
    else if (d.v === 'none') AG.draft = new Set();
    else if (d.v === 'view') AG.draft = new Set(GRANTABLE().filter(c => BTNS[c].kind === 'v'));
    else AG.draft = new Set(curG().perms);
    paintAg(); toast(d.v === 'reset' ? '已撤销未保存的修改' : '已更新勾选（未保存）');
  };
  ON.agPreset = (d, el) => { if (!el.value) return; AG.draft = new Set(PRESETS[el.value]); toast(`已套用预设「${el.value}」（尚未保存）`); el.value = ''; paintAg(); };

  /* 保存前的变更差异：只列出发生变化的页面 */
  function diffText(g, D) {
    const before = [], after = [], pageKeys = new Set([...g.perms, ...D].filter(c => BTNS[c]).map(c => BTNS[c].page));
    PERM_TREE.forEach(m => m.pages.forEach(p => { if (!pageKeys.has(p.key)) return;
      const same = p.btns.every(b => g.perms.includes(b[0]) === D.has(b[0])); if (same) return;
      p.btns.forEach(b => { const l = `${p.name} / ${b[1]}${b[2] === 'd' ? ' ⚠' : ''}  [${b[0]}]`; if (g.perms.includes(b[0])) before.push(l); if (D.has(b[0])) after.push(l); }); }));
    return [before.join('\n'), after.join('\n')];
  }
  /* 假设该权限组换成新权限后，审批流里是否出现新的“无人可审” */
  function flowImpact(g, D) {
    const old = g.perms, out = [];
    FT_IDS.forEach(t => RGROUPS.filter(r => r.enabled).forEach(r => { const b = flowWarns(r.id, t).map(w => w.i + ':' + w.gid); g.perms = [...D];
      const a = flowWarns(r.id, t); g.perms = old; a.forEach(w => { if (!b.includes(w.i + ':' + w.gid)) out.push(`${r.name} · ${FT[t].name} 第 ${w.i + 1} 级「${groupName(w.gid)}」：${w.msg}`); }); }));
    return out;
  }
  function saveGroupPerms() {
    const g = curG(), D = AG.draft, add = [...D].filter(c => !g.perms.includes(c)), del = g.perms.filter(c => !D.has(c));
    if (!add.length && !del.length) return toast('没有变化');
    const hi = add.filter(c => BTNS[c].kind === 'd'), mem = membersOf(g.id), [a, b] = diffText(g, D), imp = flowImpact(g, D);
    const noView = [...new Set([...D].filter(c => BTNS[c] && BTNS[c].kind !== 'v').map(c => BTNS[c].page))].filter(k => !PAGES[k].btns.some(x => x[2] === 'v' && D.has(x[0])) && PAGES[k].btns.some(x => x[2] === 'v'));
    openModal({title: `保存权限组「${g.name}」的按钮权限`, code: 'ag.edit', wide: true, ok: '确认保存', okCls: hi.length ? 'danger primary' : 'primary',
      body: `<div class="row" style="margin-bottom:8px"><span class="tag ok">新增 ${add.length}</span><span class="tag bad">移除 ${del.length}</span><span class="small muted">变更对 <b>${mem.length}</b> 名成员（${esc(mem.map(u => u.login).join('、') || '无')}）立即生效${mem.some(u => u.id === ME.id) ? '，包含你自己' : ''}。</span></div>
        ${diffHtml(a, b)}
        ${noView.length ? notice(`以下页面只勾选了操作按钮、没有勾选“查看类”按钮，用户将<b>打不开这些页面</b>：${noView.map(k => esc(PAGES[k].name)).join('、')}（允许保存）。`, 'warn', '⚠') : ''}
        ${imp.length ? notice('此变更会让审批流出现断点：<br>' + imp.map(esc).join('<br>'), 'warn', '⚠') : ''}
        ${hi.length ? `<div class="notice bad"><span>⚠</span><div>本次新增了 <b>${hi.length}</b> 个高危按钮：${hi.map(c => esc(lbl(c))).join('、')}。<br><label class="chk"><input type="checkbox" id="agHi"> 我已确认要授予以上高危按钮</label></div></div>` : ''}
        <div class="form" style="margin-top:8px">${fld('变更原因（选填）', '<input id="agWhy" maxlength="200">')}</div>`,
      onOk() {
        if (hi.length && !$('#agHi').checked) { toast('请先确认高危按钮'); return false; }
        g.perms = ALL_CODES().filter(c => D.has(c)); const why = $('#agWhy').value.trim();
        audit('ag.edit', `修改权限组「${g.name}」按钮权限：+${add.length} −${del.length}（影响 ${mem.length} 名成员）${why ? '；原因：' + why : ''}`, '成功', [...add.map(c => '+ ' + c), ...del.map(c => '- ' + c)].join(' '));
        AG.draftGid = ''; toast('已保存，立即生效'); refreshShell(); rerender();
      }});
  }

  /* 迁移助手：按原权限码生成按钮（只增不减，合并进当前勾选，之后再走“保存”确认） */
  const MG = {codes: new Set(), login: new Set()};
  function migratePaint() {
    const D = AG.draft, bs = new Set(); [...MG.codes].forEach(c => LEG_MAP[c].b.forEach(x => bs.add(x))); MG.login.forEach(x => bs.add(x));
    const have = [...bs].filter(c => D.has(c)), add = [...bs].filter(c => !D.has(c)), none = [...MG.codes].filter(c => !LEG_MAP[c].b.length);
    $('#mgRoot').innerHTML = `<div class="grid g2" style="gap:14px"><div><h4 style="margin:0 0 6px">① 该权限组的原权限码</h4><div class="sy-pick" style="max-height:340px">
        ${LEGACY.map(([c, n, b]) => `<label><input type="checkbox" data-onchange="mgChk" data-c="${c}" ${MG.codes.has(c) ? 'checked' : ''}><span><span class="mono small">${c}</span> <span class="small muted">${esc(n)}</span></span></label>`).join('')}
        <div class="ph">原系统“登录即可”的按钮</div>${LOGIN_ONLY.map(c => `<label><input type="checkbox" data-onchange="mgLogin" data-c="${c}" ${MG.login.has(c) ? 'checked' : ''}><span>${esc(lbl(c))} <span class="mono small muted">${c}</span></span></label>`).join('')}</div></div>
      <div><h4 style="margin:0 0 6px">② 展开预览</h4><div class="sy-pick" style="max-height:340px">
        <div class="ph">将新增（${add.length}）</div>${add.map(c => `<div class="small"><span class="tag ok">＋</span> ${esc(lbl(c))}${BTNS[c].kind === 'd' ? ' <span class="risk">⚠</span>' : ''} <span class="mono muted">${c}</span></div>`).join('') || '<div class="small muted">无</div>'}
        <div class="ph">已有（${have.length}）</div>${have.map(c => `<div class="small muted">✓ ${esc(lbl(c))} <span class="mono">${c}</span></div>`).join('') || '<div class="small muted">无</div>'}
        <div class="ph">无对应按钮（${none.length}）</div>${none.map(c => `<div class="small muted">∅ ${c} — ${esc(LEG_MAP[c].name)}</div>`).join('') || '<div class="small muted">无</div>'}</div></div></div>`;
  }
  ON.mgChk = (d, el) => { el.checked ? MG.codes.add(d.c) : MG.codes.delete(d.c); migratePaint(); };
  ON.mgLogin = (d, el) => { el.checked ? MG.login.add(d.c) : MG.login.delete(d.c); migratePaint(); };
  function openMigrate() {
    const g = curG(); MG.codes = new Set(LEG_GROUP[g.id] || []); MG.login = new Set(LOGIN_ONLY);
    openModal({title: `按原权限码生成 · ${g.name}`, code: 'ag.edit', wide: true, ok: '合并到当前勾选', body: '<div id="mgRoot"></div>', onOk() {
      const bs = new Set(); MG.codes.forEach(c => LEG_MAP[c].b.forEach(x => bs.add(x))); MG.login.forEach(x => bs.add(x));
      const n = [...bs].filter(c => !AG.draft.has(c)).length; bs.forEach(c => AG.draft.add(c)); toast(`已合并 ${n} 个按钮到当前勾选（尚未保存）`);
      if (!$('#agTree')) rerender(); else paintAg(); }});
    migratePaint();
  }

  /* ---- ③ 按用户查看有效权限 ---- */
  function agUser() {
    const u = uById(AG.uid) || USERS[0];
    return `<div class="toolbar"><label class="small muted">用户 <select data-onchange="agUser" style="min-width:260px">${USERS.map(x => `<option value="${x.id}" ${x.id === u.id ? 'selected' : ''}>${esc(x.login)} ${esc(x.name)}（${x.super ? '超级用户' : esc(x.groups.map(groupName).join('/') || '无权限组')}）</option>`).join('')}</select></label><span class="sp"></span>${B('ag.btnperm', '单独授权 / 禁用…', {data: {op: 'ovr', uid: u.id}})}</div>
      <h3 style="font-size:14px;margin:6px 0">单独授权 / 禁用记录</h3>${ovrRows(u)}
      <h3 style="font-size:14px;margin:14px 0 0">有效权限</h3>${effPanel(u)}`;
  }
  ON.agUser = (d, el) => { AG.uid = +el.value; rerender(); };

  /* ---- 新建 / 改名 / 删除 ---- */
  ACTIONS['ag.edit'] = d => {
    if (d.op === 'save') return saveGroupPerms();
    if (d.op === 'migrate') return openMigrate();
    if (d.op === 'meta') {
      const g = curG(), name = g.builtin ? g.name : $('#agName').value.trim(), desc = $('#agDesc').value.trim();
      if (!name || name.length > 150 || /[,\n]/.test(name)) return toast('名称 1–150 字，不能含逗号或换行');
      if (GROUPS.some(x => x !== g && x.name.toLowerCase() === name.toLowerCase())) return toast('已存在同名权限组');
      const old = g.name; g.name = name; g.desc = desc || g.desc;
      audit('ag.edit', `修改权限组基本信息：${old}${old !== name ? ' ⇒ ' + name : ''}`); toast('已保存'); refreshShell(); return rerender();
    }
    openModal({title: '新建权限组', code: 'ag.edit', body: `<div class="form">${fld('组名（≤150 字）', '<input id="ngN" maxlength="150" placeholder="例：SRE 值班">', {req: true})}${fld('说明', '<input id="ngD" placeholder="一句话说明用途">')}
      ${fld('初始权限', `<select id="ngT"><option value="">空白（不含任何按钮）</option>${Object.keys(PRESETS).map(n => `<option value="p:${n}">套用预设「${n}」</option>`).join('')}${GROUPS.map(g => `<option value="g:${g.id}">复制「${esc(g.name)}」</option>`).join('')}</select>`)}</div>`,
      onOk() {
        const name = $('#ngN').value.trim(), t = $('#ngT').value;
        if (!name || name.length > 150 || /[,\n]/.test(name)) { toast('名称 1–150 字，不能含逗号或换行'); return false; }
        if (GROUPS.some(x => x.name.toLowerCase() === name.toLowerCase())) { toast('已存在同名权限组'); return false; }
        const perms = !t ? [] : t.startsWith('p:') ? PRESETS[t.slice(2)].slice() : gById(t.slice(2)).perms.slice();
        const g = {id: 'g_c' + Date.now().toString(36), name, builtin: false, desc: $('#ngD').value.trim() || '自定义权限组', perms};
        GROUPS.push(g); AG.gid = g.id; AG.draftGid = ''; audit('ag.edit', `新建权限组「${name}」（初始 ${perms.length} 个按钮）`); toast('已新建权限组'); location.hash = 'ag/perm'; if (ROUTE.page === 'ag') rerender();
      }});
  };
  ACTIONS['ag.delete'] = d => {
    const g = gById(d.gid), mem = membersOf(g.id), refs = flowRefs(g.id);
    if (g.builtin) return toast('内置权限组不能删除');
    if (mem.length || refs.length) return openModal({title: '无法删除权限组 · ' + g.name, code: 'ag.delete', ok: '', cancel: '知道了', body: `<div class="notice bad"><span>⛔</span><div>拒绝删除：
      ${mem.length ? `<div>该权限组还有 <b>${mem.length}</b> 名成员（${esc(mem.map(u => u.login).join('、'))}），请先在“用户管理”里调整他们的权限组。</div>` : ''}
      ${refs.length ? `<div>被 <b>${refs.length}</b> 条审批流引用：${esc(refs.map(x => `${x.rg.name}·${FT[x.t].name} 第 ${x.i + 1} 级`).join('；'))}。请先在“审批流程配置”里改掉。</div>` : ''}</div></div>`});
    risky('ag.delete', '删除权限组', `删除权限组「${esc(g.name)}」及其全部按钮授权。`, `DELETE FROM dbt_auth_group_permissions WHERE group_id=${gnum(g.id)};\nDELETE FROM dbt_auth_group WHERE id=${gnum(g.id)};`, () => {
      GROUPS.splice(GROUPS.indexOf(g), 1); AG.gid = GROUPS[0].id; AG.draftGid = ''; audit('ag.delete', `删除权限组「${g.name}」`); toast('已删除'); refreshShell(); rerender();
    }, g.name);
  };

  /* ---- 生效模式 / 无权限呈现 / 单独授权 ---- */
  function preflightHtml() {
    const us = USERS.filter(u => !u.super && !dis(u)).map(u => { const c = compatSet(u), s = strictSet(u), lost = [...c].filter(x => !s.has(x));
      return {u, lost, pages: [...new Set(lost.map(x => BTNS[x].pageName))]}; }).filter(x => x.lost.length);
    const gs = GROUPS.map(g => ({g, n: [...legBtns(LEG_GROUP[g.id])].filter(c => !g.perms.includes(c)).length + LOGIN_ONLY.filter(c => !g.perms.includes(c)).length})).filter(x => x.n && membersOf(x.g.id).length);
    return {us, gs, html: `${notice('切换到<b>严格模式</b>后，原 Django 权限（含直接授给用户的）不再生效。', 'warn', '⚠')}
      <h4 style="margin:8px 0 4px">会因此失去访问的用户（${us.length}）</h4>${us.length ? tbl(['用户', '权限组', '将失去的按钮', '涉及页面'], us, {cells: x => [`<b class="mono">${esc(x.u.login)}</b> ${esc(x.u.name)}`, esc(x.u.groups.map(groupName).join('/') || '—'), x.lost.length + ' 个', `<span class="small">${esc(x.pages.slice(0, 4).join('、'))}${x.pages.length > 4 ? ' 等 ' + x.pages.length + ' 个' : ''}</span>`]}) : '<div class="small muted">没有用户会失去访问。</div>'}
      <h4 style="margin:12px 0 4px">有原权限但按钮表里尚未授予的权限组（${gs.length}）</h4>${gs.length ? tbl(['权限组', '成员', '尚未授予的按钮'], gs, {cells: x => [esc(x.g.name), membersOf(x.g.id).length + ' 人', `${x.n} 个`]}) : '<div class="small muted">所有权限组都已覆盖。</div>'}
      <div class="small muted" style="margin-top:6px">直接授予用户的原权限（${Object.keys(LEG_USER).join('、')}）将失效。</div>`};
  }
  ACTIONS['ag.btnperm'] = d => {
    if (d.op === 'ovr') return openOvr(+d.uid);
    if (d.op === 'revoke') {
      const u = uById(d.uid), i = OVR.findIndex(o => o.uid === u.id && o.code === d.code);
      if (i < 0) return;
      const o = OVR.splice(i, 1)[0]; applyOvr(u); audit('ag.btnperm', `撤销按钮${o.effect === 'allow' ? '单独授权' : '单独禁用'} ${u.login}：${o.code}`); toast('已撤销'); refreshShell(); return ROUTE.page === 'user' ? redo() : rerender();
    }
    if (d.op === 'deny') { if (SETTINGS.noPermBtn === d.to) return toast('已是该呈现方式'); SETTINGS.noPermBtn = d.to; audit('ag.btnperm', `修改无权限按钮呈现方式：${d.to === 'hide' ? '隐藏' : '置灰并提示'}`); toast('已切换（全站按钮立即生效）'); return rerender(); }
    if (SETTINGS.btnMode === d.to) return toast('当前已是' + (d.to === 'strict' ? '严格' : '兼容') + '模式');
    if (d.to === 'compat') return openModal({title: '切回兼容模式', code: 'ag.btnperm', ok: '切换', body: '<div>切回后原 Django 权限立即重新生效。</div>',
      onOk() { SETTINGS.btnMode = 'compat'; audit('ag.btnperm', '切换按钮权限生效模式 严格 → 兼容'); toast('已切换为兼容模式'); rerender(); }});
    if (!activeSupers().length) return toast('没有启用的超级用户，禁止切换到严格模式');
    const pf = preflightHtml();
    openModal({title: '⚠ 切换到严格模式 · 预检', code: 'ag.btnperm', wide: true, ok: '确认切换', okCls: 'danger primary',
      body: `${pf.html}<div class="form" style="margin-top:10px"><label class="req">确认输入</label><input id="stIn" placeholder="请输入 严格模式 确认"></div>`,
      onOk() {
        if ($('#stIn').value.trim() !== '严格模式') { toast('输入不匹配'); return false; }
        SETTINGS.btnMode = 'strict'; audit('ag.btnperm', `切换按钮权限生效模式 兼容 → 严格（预检：${pf.us.length} 名用户、${pf.gs.length} 个权限组受影响，已确认）`); toast('已切换为严格模式'); rerender();
      }});
  };

  /* 单独授权 / 禁用弹窗：可设到期时间，原因必填，写审计 */
  const OV = {sel: new Set(), q: ''};
  function ovList(u) {
    const q = OV.q.trim().toLowerCase();
    return PERM_TREE.map(m => m.pages.map(p => { const bs = p.btns.filter(b => b[3] !== 'L' && (!q || (b[0] + b[1] + p.name).toLowerCase().includes(q)));
      return bs.length ? `<div class="ph">${esc(m.name)} / ${esc(p.name)}</div>` + bs.map(b => `<label><input type="checkbox" data-onchange="ovChk" data-c="${b[0]}" ${OV.sel.has(b[0]) ? 'checked' : ''}>${esc(b[1])}${b[2] === 'd' ? ' <span class="risk">⚠</span>' : ''} <span class="mono small muted">${b[0]}</span> <span class="small ${userHas(u, b[0]) ? 'muted' : ''}">${userHas(u, b[0]) ? '（当前：允许）' : ''}</span></label>`).join('') : ''; }).join('')).join('') || '<div class="empty" style="padding:16px">无匹配按钮</div>';
  }
  function ovHi() { const hi = [...OV.sel].filter(c => BTNS[c].kind === 'd'); $('#ovHi').innerHTML = hi.length && $('#ovEff').value === 'allow' ? `<div class="notice bad"><span>⚠</span><div>已选 ${hi.length} 个高危按钮：${hi.map(c => esc(lbl(c))).join('、')}。<br><label class="chk"><input type="checkbox" id="ovHiChk"> 我已确认要单独授予这些高危按钮</label></div></div>` : ''; }
  ON.ovChk = (d, el) => { el.checked ? OV.sel.add(d.c) : OV.sel.delete(d.c); ovHi(); $('#ovCnt').textContent = OV.sel.size; };
  ON.ovQ = (d, el) => { OV.q = el.value; $('#ovList').innerHTML = ovList(uById(OV.uid)); };
  ON.ovEff = () => ovHi();
  function openOvr(uid) {
    const u = uById(uid);
    if (u.super) return toast('超级用户不受按钮权限限制，不能单独授权 / 禁用');
    OV.sel = new Set(); OV.q = ''; OV.uid = uid;
    openModal({title: `单独授权 / 禁用 · ${u.login}`, code: 'ag.btnperm', wide: true, body: `<div class="form">${fld('方式', '<select id="ovEff" data-onchange="ovEff"><option value="allow">单独授权（追加）</option><option value="deny">单独禁用（优先于授权）</option></select>')}
      ${fld('到期日（可空）', `<input type="date" id="ovExp" min="${TODAY}">`)}
      ${fld('按钮（已选 <b id="ovCnt">0</b>）', `<input type="search" data-oninput="ovQ" placeholder="搜索按钮 / 权限码" style="margin-bottom:6px"><div class="sy-pick" id="ovList">${ovList(u)}</div>`, {req: true})}
      ${fld('原因', '<input id="ovWhy" maxlength="200" placeholder="例：大促值班期间临时授权">', {req: true})}</div><div id="ovHi"></div>`,
      onOk() {
        const eff = $('#ovEff').value, exp = $('#ovExp').value, why = $('#ovWhy').value.trim();
        if (!OV.sel.size) { toast('请至少选择一个按钮'); return false; }
        if (!why) { toast('原因必填'); return false; }
        if (exp && exp < TODAY) { toast('到期日不能早于今天'); return false; }
        const hi = [...OV.sel].filter(c => BTNS[c].kind === 'd');
        if (eff === 'allow' && hi.length && !($('#ovHiChk') && $('#ovHiChk').checked)) { toast('请确认高危按钮'); return false; }
        OV.sel.forEach(c => { const i = OVR.findIndex(o => o.uid === u.id && o.code === c); if (i >= 0) OVR.splice(i, 1); OVR.push({uid: u.id, code: c, effect: eff, expire: exp, reason: why, by: ME.login, at: now()}); });
        applyOvr(u);
        audit('ag.btnperm', `${eff === 'allow' ? '单独授权' : '单独禁用'}按钮 ${u.login}：${[...OV.sel].join('、')}${exp ? '，到期 ' + exp : ''}；原因：${why}`);
        toast(`已${eff === 'allow' ? '单独授权' : '单独禁用'} ${OV.sel.size} 个按钮`); refreshShell(); ROUTE.page === 'user' ? redo() : rerender();
      }});
  }
})();
