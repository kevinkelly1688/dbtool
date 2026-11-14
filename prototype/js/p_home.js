/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_home.js
 * @time: 2026/10/28
 */
/* 首页：按当前身份聚合“我的待办”、快捷入口、实例健康、最近动态。
   数据来自其他页面导出的 window.WORKFLOWS / APPLIES / PERF_ISSUES（运行时读取，缺失则兜底为空） */
(function () {
  'use strict';
  const H = {tab: 'approve'};
  const list = n => window[n] || [];
  const hour = () => new Date().getHours();
  const greet = () => hour() < 6 ? '夜深了' : hour() < 12 ? '早上好' : hour() < 14 ? '中午好' : hour() < 19 ? '下午好' : '晚上好';

  /* 待办聚合：返回 [{kind, id, title, sub, href, warn}] */
  function todos() {
    const out = {approve: [], mine: [], perf: []};
    const wfp = typeof window.wfPendingList === 'function' ? window.wfPendingList(ME) : [];
    wfp.forEach(w => out.approve.push({kind: 'SQL 上线', id: w.id, title: w.name, sub: `${instOf(w.inst).name} / ${w.db} · ${w.submitter} · ${w.created}`, href: '#sqlworkflow/detail/' + w.id}));
    list('WORKFLOWS').forEach(w => {
      if (w.submitter === ME.login && ['等待审批', '审核通过', '定时执行', '执行中', '执行有异常'].includes(w.status)) out.mine.push({kind: 'SQL 上线', id: w.id, title: w.name, sub: `${instOf(w.inst).name} / ${w.db} · ${w.status}`, href: '#sqlworkflow/detail/' + w.id, warn: w.status === '执行有异常', st: w.status});
    });
    const app = typeof window.applyPendingList === 'function' ? window.applyPendingList(ME) : [];
    app.forEach(a => out.approve.push({kind: '查询权限', id: a.id, title: `${a.user} 申请 ${instOf(a.inst).name} / ${a.db}`, sub: a.created, href: '#qapply/detail/' + a.id}));
    list('APPLIES').forEach(a => {
      if (a.user === ME.login && a.status === '待审批') out.mine.push({kind: '查询权限', id: a.id, title: `申请 ${instOf(a.inst).name} / ${a.db}`, sub: '审批中', href: '#qapply/detail/' + a.id, st: '待审批'});
    });
    const arc = typeof window.archivePendingList === 'function' ? window.archivePendingList(ME) : [];
    arc.forEach(t => out.approve.push({kind: '数据归档', id: t.id, title: t.name, sub: `${instOf(t.inst).name} / ${t.db}.${t.tbl} · ${t.creator} · ${t.created}`, href: '#archive/tasks'}));
    list('PERF_ISSUES').forEach(p => {
      if (p.owner === ME.login && !['已关闭', '待验证'].includes(p.status)) out.perf.push({kind: p.level, id: p.id, title: p.title, sub: `${p.status}${p.overdue ? ' · 已超时' : ''}`, href: '#perf/detail/' + p.id, warn: p.overdue, st: p.level});
    });
    return out;
  }
  const kpi = (l, v, d, cls = 'flat', href = '') => `<div class="card kpi">${href ? `<a href="${href}" style="color:inherit;text-decoration:none">` : ''}<div class="l">${l}</div><div class="v">${v}</div>${d ? `<div class="d ${cls}">${d}</div>` : ''}${href ? '</a>' : ''}</div>`;

  const QUICK = [
    ['sqlworkflow', '⎘', '提交 SQL 工单', '#sqlworkflow/new', 'sqlworkflow.submit'], ['query', '⌘', '在线查询', '#query', 'query.view'],
    ['qapply', '✎', '申请查询权限', '#qapply/new', 'qpriv.apply'], ['export', '⇩', '数据导出', '#export/new', 'export.submit'],
    ['sqladvisor', '⚡', 'SQL 优化', '#sqladvisor', 'optimize.explain'], ['dict', '☷', '数据字典', '#dict', 'dict.view'],
    ['slowquery', '☰', '慢查日志', '#slowquery', 'slowquery.view'], ['perf', '⚑', '性能工单', '#perf', 'perf.view'],
    ['sessions', '⇄', '会话管理', '#sessions', 'session.open'], ['inspect', '✚', '性能巡检', '#inspect', 'inspect.view'],
  ];

  pages.home = {
    title: () => `${greet()}，${ME.name}`,
    render() {
      const td = todos(), vis = visibleInstances();
      const abnormal = vis.filter(i => i.status !== 'ok').length;
      const wf = list('WORKFLOWS').filter(w => ME.super || can('sqlworkflow.view_group') || w.submitter === ME.login);
      const running = wf.filter(w => ['等待审批', '审核通过', '定时执行', '执行中'].includes(w.status)).length;
      const perfTodo = typeof window.perfTodoFor === 'function' ? window.perfTodoFor(ME) : {n: 0, overdue: 0};
      const trend = [...Array(7)].map((_, i) => { const d = dayOffset(i - 6); return [d.slice(5), wf.filter(w => (w.created || '').startsWith(d)).length || (i * 3 + 2) % 5 + 1, 'var(--c1)']; });
      const quick = QUICK.filter(q => can(q[4]));
      const items = td[H.tab] || [];
      const recent = AUDIT.filter(a => ME.super || can('audit.view') || a.user === ME.login).slice(0, 7);
      return `
      <div class="toolbar"><span class="tag brand">${ME.super ? '超级用户' : esc(ME.groups.map(groupName).join(' / '))}</span>
        <span class="small muted">${esc(ME.dept)} · 资源组：${ME.rgs.map(rgName).join('、') || '无'} · ${TODAY} · 上次登录 ${esc(ME.last)}</span><span class="sp"></span>
        ${can('sqlworkflow.submit') ? '<a class="btn primary" href="#sqlworkflow/new">＋ 提交 SQL 工单</a>' : ''}</div>
      <div class="grid g4">
        ${kpi('待我审批', td.approve.length, td.approve.length ? '' : '没有待审批事项', 'flat')}
        ${kpi('进行中工单（我的 / 全部）', td.mine.filter(x => x.kind === 'SQL 上线').length + ' / ' + running, '')}
        ${kpi('性能工单待办', perfTodo.n, perfTodo.overdue ? `其中 ${perfTodo.overdue} 个已超时` : '无超时', perfTodo.overdue ? 'up' : 'flat')}
        ${kpi('我可见的实例', vis.length, abnormal ? `${abnormal} 个状态异常` : '全部正常', abnormal ? 'up' : 'down')}
      </div>
      <div class="grid g-main" style="margin-top:16px">
        <div class="grid" style="align-content:start">
          <div class="card"><h2>我的待办 <span class="sub">${td.approve.length + td.mine.length + td.perf.length} 项</span></h2>
            ${subtabs([['approve', `待我审批 ${td.approve.length}`], ['mine', `我提交的 ${td.mine.length}`], ['perf', `性能工单 ${td.perf.length}`]], H.tab, 'homeTab')}
            <div style="margin-top:10px">${items.length ? `<div class="tbl-wrap"><table><tbody>${items.map(t => `<tr class="click" data-on="homeGo" data-h="${t.href}"><td style="width:92px"><span class="tag ${t.kind.startsWith('P') ? (t.kind === 'P0' ? 'p0' : t.kind === 'P1' ? 'p1' : 'p2') : 'brand'}">${esc(t.kind)}</span></td>
              <td><b>${esc(t.title)}</b><div class="small muted">${esc(t.sub)}</div></td><td style="width:80px;text-align:right">${t.warn ? '<span class="tag bad">需处理</span>' : '<a>查看 →</a>'}</td></tr>`).join('')}</tbody></table></div>`
              : empty(H.tab === 'approve' ? '没有等待你审批的事项 🎉' : '暂无事项', '✓')}</div></div>
          <div class="card"><h2>快捷入口</h2>
            ${quick.length ? `<div class="grid g4" style="gap:10px">${quick.map(q => `<a href="${q[3]}" class="card" style="padding:12px;text-align:center;box-shadow:none;text-decoration:none;color:var(--text)"><div style="font-size:22px;color:var(--brand)">${q[1]}</div><div class="small" style="margin-top:4px">${q[2]}</div></a>`).join('')}</div>` : empty('当前身份没有可用的快捷入口，请联系管理员授权', '🔒')}</div>
          <div class="card"><h2>近 7 日 SQL 工单 <span class="sub">共 ${trend.reduce((s, t) => s + t[1], 0)} 张</span></h2>${barChart(trend, {h: 150})}</div>
        </div>
        <div class="grid" style="align-content:start">
          <div class="card"><h2>实例健康 <span class="sub">${vis.length} 个</span><span class="right">${can('instance.view') ? '<a href="#instlist">实例列表 →</a>' : ''}</span></h2>
            ${vis.length ? vis.slice(0, 9).map(i => `<div class="row between small" style="padding:5px 0;border-bottom:1px solid var(--border)"><span class="ellipsis" style="max-width:200px">${dot(i.status === 'ok' ? 'ok' : i.status === 'warn' ? 'warn' : 'bad', esc(i.name))}</span><span class="muted">${i.type} · ${i.role === 'master' ? '主' : '从'}${i.status !== 'ok' ? ` · <span class="${i.status === 'bad' ? 'up' : ''}">${i.status === 'bad' ? '连接失败' : '连接数偏高'}</span>` : ''}</span></div>`).join('') : empty('没有可见实例：请联系管理员把你加入资源组', '▤')}
            ${vis.length > 9 ? `<div class="small muted" style="padding-top:6px">… 另有 ${vis.length - 9} 个</div>` : ''}</div>
          <div class="card"><h2>最近动态 <span class="sub">${ME.super || can('audit.view') ? '全平台' : '我的操作'}</span><span class="right">${can('audit.view') ? '<a href="#audit">系统审计 →</a>' : ''}</span></h2>
            ${recent.length ? recent.map(a => `<div style="padding:6px 0;border-bottom:1px solid var(--border)"><div class="small"><b>${esc(a.user)}</b> ${esc(a.act)}</div><div class="small muted">${esc(a.at)} · ${esc(a.module)}${a.res !== '成功' ? ` · <span class="up">${esc(a.res)}</span>` : ''}</div></div>`).join('') : empty('暂无记录')}</div>
          <div class="card"><h2>平台状态</h2>
            ${[['goInception 检查服务', '<span class="tag ok">正常</span>'], ['通知机器人', '<span class="tag ok">钉钉 / 飞书 / Slack 正常</span>'], ['定时调度器', '<span class="tag ok">运行中（3 个待执行任务）</span>'], ['元数据库', '<span class="tag ok">独立库 · dbt_ 前缀</span>'], ['按钮权限模式', `<span class="tag ${SETTINGS.btnMode === 'strict' ? 'warn' : 'info'}">${SETTINGS.btnMode === 'strict' ? '严格' : '兼容'}</span>`]].map(([k, v]) => `<div class="row between small" style="padding:6px 0;border-bottom:1px solid var(--border)"><span class="muted">${k}</span>${v}</div>`).join('')}</div>
        </div>
      </div>`;
    },
  };
  ON.homeTab = d => { H.tab = d.v; rerender(); };
  ON.homeGo = d => { location.hash = d.h.slice(1); };
})();
