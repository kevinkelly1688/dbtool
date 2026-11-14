/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_sys2.js
 * @time: 2026/10/28
 */
/* ============================================================================
   系统管理（二）：登录方式 authp / 登录与安全设置 sec / 系统审计 audit / goInception 配置 goinception / 通知渠道 notify
   - 除 audit 外，其余页面的按钮全是「仅超级用户（locked）」，admin 上完整可用；audit 对持有 audit.view 的权限组（审计、DBA）开放。
   - 登录方式直接读写 perm.js 的 SSO_PROVIDERS（登录页每次打开都会读取）；安全设置直接读写 SETTINGS；审计读 data.js 的 AUDIT。
   - 导出：window.NOTIFY_CHANNELS（给首页 / 其他页面用）。
   ========================================================================== */
(function () {
  'use strict';

  /* ---------- 通用小工具 ---------- */
  const hash8 = s => { let h = 2166136261; for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619); } return (h >>> 0).toString(16).padStart(8, '0'); };
  const userBy = login => USERS.find(u => u.login === login);
  const userName = login => { const u = userBy(login); return u ? u.name : login; };
  const clock = () => { const d = new Date(); return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`; };
  const opt = (v, t, cur) => `<option value="${esc(v)}" ${String(v) === String(cur) ? 'selected' : ''}>${esc(t)}</option>`;
  const INP = 'background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:6px 10px;color:inherit';
  const val = id => { const el = $('#' + id); return el ? el.value.trim() : ''; };
  const chk = id => { const el = $('#' + id); return !!(el && el.checked); };
  const checkedVals = sel => $$(sel).filter(x => x.checked).map(x => x.value);
  const ylist = (arr, empty = '无') => arr.length ? arr.map(esc).join('、') : empty;
  const CSS2 = `<style>
    .p2-cols{display:grid;grid-template-columns:minmax(300px,1fr) minmax(0,1.3fr);gap:16px;align-items:start}
    @media(max-width:1100px){.p2-cols{grid-template-columns:1fr}}
    .p2-ico{display:inline-grid;place-items:center;width:24px;height:24px;border-radius:7px;color:#fff;font-size:12px;font-weight:700;font-style:normal;vertical-align:middle;margin-right:6px;flex:none}
    tr.p2-over td{background:var(--brand-soft)}
    .p2-acc{border:1px solid var(--border);border-radius:10px;margin-bottom:10px;overflow:hidden}
    .p2-acc>summary{padding:8px 12px;cursor:pointer;background:var(--panel2);font-weight:600}
    .p2-pf{display:grid;grid-template-columns:minmax(0,1fr) 150px;gap:6px 10px;align-items:center;padding:10px 12px}
    .p2-pf .nm{font-size:13px}.p2-pf .nm .mono{font-size:11px;color:var(--muted)}
    .p2-pf input[type=text],.p2-pf input[type=password],.p2-pf select{width:100%}
    .p2-msg{border:1px solid var(--border);border-radius:10px;padding:12px 14px;background:var(--panel2);font-size:13px;line-height:1.75;overflow:auto}
    .p2-at{background:var(--accent-soft);color:var(--accent);border-radius:4px;padding:0 4px;white-space:nowrap}
    .p2-at.miss{background:var(--warn-soft);color:var(--warn)}
    .p2-line{line-height:1.9;font-size:13px}.p2-line.ok{color:var(--ok)}.p2-line.bad{color:var(--bad)}.p2-line.warn{color:var(--warn)}.p2-line.info{color:var(--text)}
    .p2-rule{display:grid;grid-template-columns:150px minmax(0,1fr) auto;gap:8px;align-items:start;padding:8px 0;border-bottom:1px dashed var(--border)}
    .p2-rule>div{min-width:0}
    .p2-ev{display:flex;flex-wrap:wrap;gap:4px 14px}
    .p2-ev .chk{font-size:13px}
    .p2-wsel{display:flex;gap:6px;align-items:center;flex-wrap:wrap}
    .p2-stg{list-style:none;margin:6px 0 0;padding:0;display:grid;gap:4px}
    .p2-stg li{display:grid;grid-template-columns:84px 54px minmax(0,1fr);gap:8px;align-items:baseline;font-size:12.5px;padding:4px 8px;border:1px solid var(--border);border-radius:8px;background:var(--panel2)}
    .p2-stg .ok{color:var(--ok);font-weight:600}.p2-stg .fail{color:var(--bad);font-weight:600}.p2-stg .skip{color:var(--muted)}
    .p2-risk{color:var(--bad);font-weight:600}
    .wm-prev{height:150px;border:1px dashed var(--border);border-radius:10px;background-color:var(--bg);background-repeat:repeat;overflow:hidden}
    .wm-vars{display:flex;flex-wrap:wrap;gap:6px;margin-top:6px}
    .wm-vars a{cursor:pointer;font-family:ui-monospace,Consolas,monospace;font-size:12px}
  </style>`;

  /* ============================================================================
     系统审计 audit（非 locked）
     ========================================================================== */
  const H = (at, user, code, act, res = '成功', extra = '', ip = '') => {
    const u = userBy(user);
    return {at, user, ip: ip || (u && u.ip) || '203.0.113.45', module: code === 'auth.login' ? '登录' : (BTNS[code] || {}).pageName || '—', code, act, res, extra};
  };
  /* 历史审计（接在 data.js 种子之后，整体按时间倒序） */
  const HIST = [
    H('2026-10-06 10:52', 'rd_zhou', 'auth.login', '登录成功（飞书 单点登录）'),
    H('2026-10-06 10:45', 'rd_zhao', 'sqlworkflow.submit', '提交工单 #1038「订单流水表新增索引 idx_order_action」', '成功', 'mysql-order-prod-m / order_db'),
    H('2026-10-06 10:20', 'dba_li', 'sqlworkflow.execute_group', '立即执行工单 #1036「支付流水表增加对账标记」', '成功', 'mysql-pay-prod-m / pay_db'),
    H('2026-10-06 09:58', 'auditor_chen', 'audit.export', '导出审计日志 CSV：214 条（2026-09-30 ~ 2026-10-06）'),
    H('2026-10-06 09:30', 'admin', 'sec.edit', '修改登录与安全设置：lock_time_threshold 900 → 1800 秒'),
    H('2026-10-06 09:12', 'admin', 'auth.login', '登录成功（账号密码 + TOTP）'),
    H('2026-10-06 08:55', 'tl_wang', 'auth.login', '登录失败（LDAP 密码错误）第 1 次', '失败'),
    H('2026-10-05 18:31', 'tl_wang', 'auth.login', '登录成功（钉钉 单点登录）'),
    H('2026-10-05 17:20', 'dba_li', 'account.pwd', '修改账号密码 report_ro@10.20.%（密码不记录）', '成功', 'mssql-report-prod'),
    H('2026-10-05 16:02', 'rd_zhao', 'qpriv.apply', '申请查询权限 order_db.orders（3 天，5000 行）', '成功', 'mysql-order-prod-m'),
    H('2026-10-05 15:40', 'tl_wang', 'qpriv.review', '审批通过查询权限申请 #2031（rd_zhao → order_db.orders）'),
    H('2026-10-05 14:12', 'dba_li', 'inspect.run', '立即巡检 mysql-user-prod-m（报告 #R-0412）', '成功', 'mysql-user-prod-m'),
    H('2026-10-05 11:08', 'qa_sun', 'export.submit', '提交导出工单 #E-318「测试环境订单抽样」', '成功', 'mysql-order-test / order_db'),
    H('2026-10-05 10:15', 'dev_liu', 'auth.login', '登录被拒绝（账号已锁定）', '拒绝'),
    H('2026-10-04 18:22', 'admin', 'authp.test', '测试登录「旧 CAS」失败：证书校验失败（unable to verify the first certificate）', '失败'),
    H('2026-10-04 17:05', 'admin', 'authp.edit', '修改登录方式「旧 CAS」：CAS 服务地址（连接配置变更，已自动停用，测试结果作废）'),
    H('2026-10-04 14:30', 'dba_li', 'session.kill', '终止会话 thread_id=77310', '成功', 'mysql-user-prod-m'),
    H('2026-10-04 10:02', 'dba_li', 'param.edit', '修改参数 max_connections 2000 → 2500（旧值已记录）', '成功', 'mysql-user-prod-m'),
    H('2026-10-03 22:19', 'unknown', 'auth.login', '登录失败（用户不存在 ops_test）×3', '失败', '', '203.0.113.45'),
    H('2026-10-03 22:14', 'unknown', 'auth.login', '登录失败（账号或密码错误，账号 admin）第 2 次', '失败', '', '203.0.113.45'),
    H('2026-10-03 16:40', 'admin', 'notify.edit', '修改通知渠道「数据平台 · 数仓告警」订阅事件：+ 巡检告警'),
    H('2026-10-03 13:25', 'rd_zhou', 'perf.handle', '受理性能工单 #PF-2031（承诺完成日 2026-10-10）', '成功', 'mysql-pay-prod-m'),
    H('2026-10-03 09:12', 'admin', 'auth.login', '登录成功（账号密码 + TOTP）'),
    H('2026-10-02 18:21', 'admin', 'goinception.restart', '保存并重启 goInception：#12（docker，健康检查通过，耗时 3.2 s）'),
    H('2026-10-02 18:20', 'admin', 'goinception.file', '保存 goInception 配置 #12：check_dml_where: false → true'),
    H('2026-10-02 15:50', 'dba_li', 'sqlworkflow.approve', '审批通过工单 #1021', '成功', 'mysql-order-prod-m'),
    H('2026-10-02 11:30', 'rd_zhao', 'sqlworkflow.submit', '提交工单 #1021「订单表增加字段 pay_channel」', '成功', 'mysql-order-prod-m / order_db'),
    H('2026-10-01 20:11', 'dba_li', 'auth.login', '登录成功（LDAP + TOTP）'),
    H('2026-09-30 15:22', 'auditor_chen', 'audit.export', '导出审计日志 CSV：388 条（2026-09-23 ~ 2026-09-30）'),
    H('2026-09-30 15:20', 'auditor_chen', 'auth.login', '登录成功（账号密码）'),
    H('2026-09-30 11:40', 'admin', 'ag.btnperm', '按钮权限：审计 新增 audit.export（高危）；预检通过，影响 1 个用户'),
    H('2026-09-29 09:05', 'admin', 'user.unlock', '解锁账号 qa_sun'),
    H('2026-09-29 08:12', 'qa_sun', 'auth.login', '登录失败（账号或密码错误）×5，账号 qa_sun 已锁定 30 分钟', '失败', '', '10.1.5.12'),
    H('2026-09-28 10:06', 'admin', 'goinception.restart', '保存并重启 goInception：#11（docker，健康检查通过，耗时 2.9 s）'),
    H('2026-09-28 10:05', 'admin', 'goinception.file', '保存 goInception 配置 #11：max_update_rows: 5000 → 10000'),
    H('2026-09-26 17:30', 'admin', 'flow.edit', '修改审批流 支付中心 · 查询权限申请：TL ⇒ TL → DBA'),
    H('2026-09-26 14:03', 'dba_li', 'database.create', '创建数据库 order_archive_2026（utf8mb4）', '成功', 'mysql-order-prod-m'),
    H('2026-09-25 16:43', 'admin', 'goinception.restart', '保存并重启 goInception：#10 重启失败，已自动恢复到 #9（健康检查通过）', '失败'),
    H('2026-09-25 16:42', 'admin', 'goinception.file', '保存 goInception 配置 #10：osc_bin_dir: /usr/local/bin → /opt/percona/bin'),
    H('2026-09-23 11:15', 'tl_wang', 'sqlworkflow.reject', '驳回工单 #1010（原因：缺少回滚方案）', '成功', 'mysql-pay-prod-m'),
    H('2026-09-21 18:02', 'rd_zhao', 'sqlworkflow.cancel_own', '终止自己的工单 #1008'),
    H('2026-09-20 10:00', 'admin', 'authp.toggle', '启用登录方式「飞书」（测试通过后启用）'),
    H('2026-09-20 09:48', 'admin', 'authp.test', '测试登录「飞书」成功：取得 email、入组规则命中 1 条'),
    H('2026-09-20 09:30', 'admin', 'authp.edit', '新增登录方式「飞书」（飞书，首次登录：仅限已有用户）'),
    H('2026-09-18 15:12', 'dev_liu', 'auth.login', '登录失败（账号或密码错误）×3', '失败'),
    H('2026-09-15 14:20', 'admin', 'rg.edit', '修改资源组「数据平台」通知群：Slack Webhook 已更新（不记录地址）'),
    H('2026-09-12 09:31', 'admin', 'goinception.restart', '保存并重启 goInception：#9（docker，健康检查通过，耗时 3.1 s）'),
    H('2026-09-12 09:30', 'admin', 'goinception.file', '保存 goInception 配置 #9：osc_on: false → true'),
    H('2026-09-10 10:12', 'dba_li', 'partman.run', '立即执行分区维护 order_db.order_log', '成功', 'mysql-order-prod-m'),
    H('2026-09-08 17:45', 'admin', 'user.reset', '重置 2FA：dev_liu'),
    H('2026-09-08 11:00', 'admin', 'user.edit', '新建用户 dev_liu（用户中心 · 研发 RD）'),
    H('2026-09-05 16:20', 'admin', 'sec.edit', '修改登录与安全设置：password_min_len 9 → 10；password_complexity false → true'),
    H('2026-09-03 10:24', 'admin', 'ag.edit', '新建权限组「审计」，成员 auditor_chen'),
    H('2026-09-02 09:12', 'admin', 'auth.login', '登录成功（账号密码 + TOTP）'),
    H('2026-09-01 08:30', 'admin', 'sec.edit', '初始化登录与安全设置'),
  ];
  HIST.sort((a, b) => b.at.localeCompare(a.at));
  AUDIT.push(...HIST);

  const AU = {tab: 'sys', lastTab: '', range: '30d', from: '', to: '', user: '', cat: '', res: '', risk: false, q: '', page: 1, size: 15, view: [],
    wfSt: '', wfRg: '', wfQ: '', qUser: '', qInst: '', qQ: '', qRej: false};
  const AU_CATS = [['login', '登录认证'], ['sqlflow', 'SQL 上线'], ['query', '查询与导出'], ['inst', '实例管理'], ['perf', '性能工单'], ['opt', 'SQL 优化'], ['sys', '系统管理']];
  const catOf = e => {
    const c = String(e.code || '').split('.')[0];
    if (c === 'auth') return 'login';
    if (['sqlworkflow', 'sqlanalyze'].includes(c)) return 'sqlflow';
    if (['query', 'qpriv', 'export', 'dict', 'partman', 'archive', 'slowlogs'].includes(c)) return 'query';
    if (['instance', 'session', 'database', 'account', 'param', 'inspect'].includes(c)) return 'inst';
    if (['perf', 'perfcfg'].includes(c)) return 'perf';
    if (['optimize', 'slowquery'].includes(c)) return 'opt';
    return 'sys';
  };
  const catName = e => (AU_CATS.find(c => c[0] === catOf(e)) || [0, '—'])[1];
  const isRisk = e => !!(BTNS[e.code] && BTNS[e.code].kind === 'd');
  const resTag = r => `<span class="tag ${r === '成功' ? 'ok' : r === '失败' ? 'bad' : 'warn'}">${esc(r)}</span>`;

  function auFiltered() {
    const lo = {today: TODAY, '7d': dayOffset(-6), '30d': dayOffset(-29), custom: AU.from}[AU.range] || '';
    const hi = AU.range === 'custom' ? AU.to : '';
    const q = AU.q.trim().toLowerCase();
    return AUDIT.filter(e => {
      const d = e.at.slice(0, 10);
      return (!lo || d >= lo) && (!hi || d <= hi) && (!AU.user || e.user === AU.user) && (!AU.cat || catOf(e) === AU.cat) && (!AU.res || e.res === AU.res) && (!AU.risk || isRisk(e)) &&
        (!q || `${e.user} ${userName(e.user)} ${e.act} ${e.extra} ${e.ip} ${e.code} ${e.module}`.toLowerCase().includes(q));
    }).sort((a, b) => b.at.localeCompare(a.at));
  }
  function auFilterDesc() {
    const r = {today: '今天', '7d': '近 7 天', '30d': '近 30 天', all: '全部时间', custom: `${AU.from || '…'} ~ ${AU.to || '…'}`}[AU.range];
    return [r, AU.user && '用户 ' + AU.user, AU.cat && AU_CATS.find(c => c[0] === AU.cat)[1], AU.res && '结果 ' + AU.res, AU.risk && '仅高危', AU.q && '关键词「' + AU.q + '」'].filter(Boolean).join('，');
  }
  /* 分页条：data-on="auPage" */
  function auPager(total, page, size) {
    const n = Math.max(1, Math.ceil(total / size));
    const nums = []; for (let i = 1; i <= n; i++) if (i === 1 || i === n || Math.abs(i - page) <= 1) nums.push(i); else if (nums[nums.length - 1] !== '…') nums.push('…');
    return `<div class="pager"><span>共 ${total} 条 · 第 ${page} / ${n} 页</span>${page > 1 ? `<a data-on="auPage" data-p="${page - 1}">‹ 上一页</a>` : ''}${nums.map(i => i === '…' ? '<span>…</span>' : `<a data-on="auPage" data-p="${i}" class="${i === page ? 'on' : ''}">${i}</a>`).join('')}${page < n ? `<a data-on="auPage" data-p="${page + 1}">下一页 ›</a>` : ''}</div>`;
  }
  function auListHtml() {
    const rows = auFiltered(), n = Math.max(1, Math.ceil(rows.length / AU.size));
    if (AU.page > n) AU.page = n;
    AU.view = rows;
    const cur = rows.slice((AU.page - 1) * AU.size, AU.page * AU.size), base = (AU.page - 1) * AU.size;
    return tbl(['时间', '用户', '类别 / 模块', '动作', '结果', '来源 IP', '额外信息'], cur, {
      attrs: (e, i) => ` class="click" data-on="auOpen" data-i="${base + i}"`,
      cells: e => [`<span class="mono small nowrap">${e.at}</span>`, `<b>${esc(e.user)}</b><div class="small muted">${esc(userName(e.user) === e.user ? '—' : userName(e.user))}</div>`,
        `<span class="tag">${catName(e)}</span> <span class="small muted">${esc(e.module)}</span>${isRisk(e) ? ' <span class="tag bad" title="高危按钮">高危</span>' : ''}`,
        `<span title="${esc(e.act)}">${esc(e.act.length > 56 ? e.act.slice(0, 56) + '…' : e.act)}</span>`, resTag(e.res), `<span class="mono small">${esc(e.ip)}</span>`, `<span class="small muted">${esc(e.extra || '—')}</span>`]
    }) + auPager(rows.length, AU.page, AU.size);
  }
  function auSys() {
    const lo = dayOffset(-6), recent = AUDIT.filter(e => e.at.slice(0, 10) >= lo);
    const fail = recent.filter(e => e.code === 'auth.login' && e.res !== '成功').length, lock = recent.filter(e => e.code === 'auth.login' && /锁定/.test(e.act)).length;
    const risk = recent.filter(isRisk).length, deny = recent.filter(e => e.res === '拒绝').length;
    const days = Array.from({length: 14}, (_, i) => dayOffset(i - 13)), per = days.map(d => AUDIT.filter(e => e.at.slice(0, 10) === d).length);
    const byUser = {}; recent.forEach(e => { byUser[e.user] = (byUser[e.user] || 0) + 1; });
    const top = Object.entries(byUser).sort((a, b) => b[1] - a[1]).slice(0, 6), mx = top.length ? top[0][1] : 1;
    const users = [...new Set(AUDIT.map(e => e.user))].sort();
    return `<div class="grid g4" style="margin-bottom:16px">
      <div class="card kpi"><div class="l">近 7 天审计事件</div><div class="v">${recent.length}</div><div class="d flat">累计 ${AUDIT.length} 条</div></div>
      <div class="card kpi"><div class="l">登录失败 / 拒绝</div><div class="v">${fail}</div><div class="d ${fail > 3 ? 'up' : 'flat'}">其中账号锁定 ${lock} 次</div></div>
      <div class="card kpi"><div class="l">高危操作</div><div class="v">${risk}</div></div>
      <div class="card kpi"><div class="l">被拒绝（无权限）</div><div class="v">${deny}</div></div></div>
    <div class="grid g2" style="margin-bottom:16px">
      <div class="card"><h2>近 14 天审计事件</h2>${barChart(days.map((d, i) => [d.slice(5), per[i]]), {h: 150})}</div>
      <div class="card"><h2>近 7 天最活跃用户</h2>${top.length ? top.map(([u, n]) => `<div class="row" style="margin:6px 0;flex-wrap:nowrap"><span style="width:150px"><b>${esc(u)}</b> <span class="small muted">${esc(userName(u) === u ? '' : userName(u))}</span></span><span style="flex:1">${meter(Math.round(n / mx * 100), 101, 101)}</span><b style="width:30px;text-align:right">${n}</b></div>`).join('') : empty('近 7 天没有审计事件')}</div></div>
    <div class="toolbar">
      <select data-onchange="auF" data-k="user"><option value="">全部用户</option>${users.map(u => opt(u, u + (userBy(u) ? '（' + userBy(u).name + '）' : ''), AU.user)).join('')}</select>
      <select data-onchange="auF" data-k="cat"><option value="">全部动作类别</option>${AU_CATS.map(c => opt(c[0], c[1], AU.cat)).join('')}</select>
      <select data-onchange="auF" data-k="res"><option value="">全部结果</option>${['成功', '失败', '拒绝'].map(r => opt(r, r, AU.res)).join('')}</select>
      <select data-onchange="auF" data-k="range">${[['today', '今天'], ['7d', '近 7 天'], ['30d', '近 30 天'], ['all', '全部时间'], ['custom', '自定义…']].map(r => opt(r[0], r[1], AU.range)).join('')}</select>
      ${AU.range === 'custom' ? `<input type="date" style="${INP}" data-onchange="auF" data-k="from" value="${AU.from}" aria-label="起始日期"> ~ <input type="date" style="${INP}" data-onchange="auF" data-k="to" value="${AU.to}" aria-label="结束日期">` : ''}
      <input type="search" placeholder="搜索 用户 / 动作 / IP / 备注" data-oninput="auF" data-k="q" value="${esc(AU.q)}" style="min-width:220px">
      <label class="chk"><input type="checkbox" data-onchange="auF" data-k="risk" ${AU.risk ? 'checked' : ''}> 仅高危操作</label>
      <button class="btn" data-on="auReset">重置</button><span class="sp"></span>${B('audit.export', '⇩ 导出 CSV')}</div>
    <div class="card" id="auList">${auListHtml()}</div>`;
  }

  /* ---- SQL 上线审计：读 window.WORKFLOWS（运行时可能不存在，兜底模拟数据） ---- */
  const MOCK_WF = [
    [1042, '订单表增加字段 refund_flag', 1, 'order_db', '待审批', 'rd_zhao', 1, '2026-10-07 10:08'], [1041, '用户表增加索引 idx_level_created', 4, 'user_db', '检查中', 'tl_wang', 3, '2026-10-07 09:40'],
    [1040, '支付流水表归档历史分区', 3, 'pay_db', '执行中', 'dba_li', 2, '2026-10-07 08:55'], [1039, '退款单增加字段 refund_reason', 3, 'pay_db', '审批通过', 'rd_zhou', 2, '2026-10-06 11:12'],
    [1038, '订单流水表新增索引 idx_order_action', 1, 'order_db', '待审批', 'rd_zhao', 1, '2026-10-06 10:45'], [1037, '报表库新增汇总表 agg_sales_daily', 6, 'ReportDW', '已完成', 'dba_li', 4, '2026-10-06 09:20'],
    [1036, '支付流水表增加对账标记', 3, 'pay_db', '已完成', 'rd_zhou', 2, '2026-10-05 17:30'], [1035, '订单明细表修改字段长度 sku_name', 1, 'order_db', '执行有异常', 'rd_zhao', 1, '2026-10-05 15:02'],
    [1034, '测试库清理历史订单', 7, 'order_db', '已终止', 'qa_sun', 1, '2026-10-05 11:40'], [1033, '用户登录日志按月分区', 4, 'user_db', '已驳回', 'tl_wang', 3, '2026-10-04 16:18'],
    [1032, '订单主表增加字段 invoice_no', 1, 'order_db', '已完成', 'rd_zhao', 1, '2026-10-04 10:05'], [1031, '数仓订单宽表加索引', 5, 'order_db', '定时执行', 'dba_li', 4, '2026-10-03 18:22'],
    [1030, '批量上线：报表库 3 实例统一加字段', 6, 'ReportDW', '已完成', 'dba_li', 4, '2026-10-03 14:00'], [1029, '用户资料表增加字段 nick_name', 4, 'user_db', '已完成', 'tl_wang', 3, '2026-10-02 15:47']
  ].map(r => ({id: r[0], name: r[1], inst: r[2], db: r[3], status: r[4], submitter: r[5], rg: r[6], created: r[7], type: r[1].startsWith('批量') ? 'batch' : 'sql'}));
  const wfData = () => (Array.isArray(window.WORKFLOWS) && window.WORKFLOWS.length ? window.WORKFLOWS : MOCK_WF);
  const wfTag = st => { const c = /待|检查|定时/.test(st) ? 'warn' : /完成|成功|通过/.test(st) ? 'ok' : /驳回|异常|失败/.test(st) ? 'bad' : /终止|取消|撤/.test(st) ? '' : 'info'; return `<span class="tag ${c}">${esc(st)}</span>`; };
  function auWfRows() {
    const q = AU.wfQ.trim().toLowerCase();
    return wfData().filter(w => (!AU.wfSt || w.status === AU.wfSt) && (!AU.wfRg || String(w.rg) === AU.wfRg) && (!q || `${w.id} ${w.name} ${w.submitter} ${w.db} ${instOf(w.inst).name}`.toLowerCase().includes(q))).slice().sort((a, b) => String(b.created).localeCompare(String(a.created)));
  }
  function auWfList() {
    const rows = auWfRows(), n = Math.max(1, Math.ceil(rows.length / AU.size));
    if (AU.page > n) AU.page = n;
    return tbl(['工单', '名称', '实例 / 库', '提交人', '资源组', '状态', '提交时间'], rows.slice((AU.page - 1) * AU.size, AU.page * AU.size), {
      cells: w => [`<a href="#sqlworkflow/detail/${w.id}">#${w.id}</a>${w.type === 'batch' ? ' <span class="tag info">批量</span>' : ''}`, `<a href="#sqlworkflow/detail/${w.id}">${esc(w.name)}</a>`, `${esc(instOf(w.inst).name)}<div class="small muted">${esc(w.db)}</div>`,
        `<b>${esc(w.submitter)}</b> <span class="small muted">${esc(userName(w.submitter))}</span>`, esc(rgName(w.rg)), wfTag(w.status), `<span class="mono small">${esc(w.created)}</span>`]
    }) + auPager(rows.length, AU.page, AU.size);
  }
  function auWf() {
    const data = wfData(), sts = [...new Set(data.map(w => w.status))];
    const cnt = {}; data.forEach(w => { cnt[w.status] = (cnt[w.status] || 0) + 1; });
    const cols = ['var(--c1)', 'var(--c2)', 'var(--c3)', 'var(--c4)', 'var(--c5)', 'var(--c6)'];
    return `<div class="card" style="margin-bottom:16px"><h2>工单状态分布 <span class="sub">共 ${data.length} 个</span></h2>${stackBar(Object.entries(cnt).map(([k, v], i) => [k, v, cols[i % cols.length]]))}</div>
    <div class="toolbar"><select data-onchange="auF" data-k="wfSt"><option value="">全部状态</option>${sts.map(s => opt(s, s, AU.wfSt)).join('')}</select>
      <select data-onchange="auF" data-k="wfRg"><option value="">全部资源组</option>${RGROUPS.filter(r => r.enabled).map(r => opt(r.id, r.name, AU.wfRg)).join('')}</select>
      <input type="search" placeholder="搜索工单号 / 名称 / 提交人" data-oninput="auF" data-k="wfQ" value="${esc(AU.wfQ)}" style="min-width:260px"></div>
    <div class="card" id="auList">${auWfList()}</div>`;
  }

  /* ---- 查询审计：模拟的 dbt_query_log ---- */
  const QTPL = [
    {i: 1, db: 'order_db', us: ['rd_zhao', 'tl_wang', 'dba_li'], sql: 'SELECT order_no, status, amount FROM orders WHERE user_id = 8800123 ORDER BY created_at DESC LIMIT 50', rows: [12, 50], ms: [8, 60]},
    {i: 1, db: 'order_db', us: ['rd_zhao', 'qa_sun'], sql: "SELECT status, COUNT(*) AS cnt FROM orders WHERE created_at >= '2026-10-01' GROUP BY status", rows: [5, 5], ms: [180, 900]},
    {i: 2, db: 'order_db', us: ['tl_wang', 'dba_li'], sql: "SELECT id, order_no, amount FROM orders WHERE status = 1 AND created_at >= '2026-10-06' LIMIT 200", rows: [120, 200], ms: [30, 140]},
    {i: 3, db: 'pay_db', us: ['rd_zhou', 'tl_wang'], sql: "SELECT pay_no, channel, amount, paid_at FROM payments WHERE order_no = 'O20261006118842'", rows: [1, 2], ms: [3, 12]},
    {i: 3, db: 'pay_db', us: ['rd_zhou'], sql: 'SELECT * FROM bank_cards LIMIT 100', rej: '无库表权限：pay_db.bank_cards（请先在「权限管理」申请）'},
    {i: 4, db: 'user_db', us: ['dba_li'], sql: 'SELECT user_id, ip, created_at FROM login_log WHERE user_id = 1000231 ORDER BY id DESC LIMIT 20', rows: [20, 20], ms: [6, 30]},
    {i: 4, db: 'user_db', us: ['dba_li'], sql: 'SELECT id_card, real_name FROM user_profile LIMIT 10', rows: [10, 10], ms: [4, 20], note: '敏感列 id_card、real_name 已脱敏'},
    {i: 1, db: 'order_db', us: ['qa_sun'], sql: 'DELETE FROM orders WHERE id = 1', rej: '仅允许 SELECT / SHOW / DESC / EXPLAIN（只读查询）'},
    {i: 8, db: 'order_db', us: ['rd_zhao'], sql: 'SELECT * FROM orders WHERE id < 100', rej: 'read_view 实例只允许查询以 v_ 开头的视图'},
    {i: 7, db: 'order_db', us: ['qa_sun', 'rd_zhao'], sql: 'SELECT * FROM order_items WHERE order_id IN (1001, 1002, 1003)', rows: [3, 9], ms: [4, 15]},
    {i: 5, db: 'order_db', us: ['dba_li'], sql: "SELECT DATE(created_at) d, SUM(amount) gmv FROM orders WHERE created_at >= '2026-09-01' GROUP BY d", rows: [30, 38], ms: [900, 4200]},
    {i: 1, db: 'order_db', us: ['rd_zhao'], sql: 'SELECT * FROM order_log', rows: [100, 100], ms: [20, 70], note: '未带 LIMIT，已自动追加 LIMIT 100'},
    {i: 6, db: 'ReportDW', us: ['dba_li'], sql: 'SELECT TOP 20 customer_key, SUM(amount) AS amt FROM dbo.fact_sales GROUP BY customer_key ORDER BY amt DESC', rows: [20, 20], ms: [600, 3100]},
    {i: 3, db: 'pay_db', us: ['tl_wang'], sql: "EXPLAIN SELECT * FROM payments WHERE status = 1 AND paid_at > '2026-10-01'", rows: [1, 1], ms: [2, 8], note: '执行计划（type=range，rows≈41 万）'},
    {i: 1, db: 'order_db', us: ['rd_zhao', 'tl_wang'], sql: 'SELECT COUNT(*) FROM orders WHERE status = 0', rows: [1, 1], ms: [260, 1400]},
    {i: 2, db: 'order_db', us: ['rd_zhao'], sql: 'SELECT * FROM user_addr WHERE user_id = 7700421', rows: [1, 4], ms: [3, 9], note: '敏感列 name、phone、detail 已脱敏'},
  ];
  const QLOG = (() => {
    const r = rng(20261007), out = []; let t = new Date(2026, 9, 7, 10, 22, 41).getTime();
    for (let k = 0; k < 38; k++) {
      const tp = QTPL[Math.floor(r() * QTPL.length)], d = new Date(t), user = pick(r, tp.us);
      const rows = tp.rej ? 0 : Math.round(tp.rows[0] + r() * (tp.rows[1] - tp.rows[0])), ms = tp.rej ? 0 : Math.round(tp.ms[0] + r() * (tp.ms[1] - tp.ms[0]));
      out.push({id: 9000 - k, at: `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`, user, inst: tp.i, db: tp.db, sql: tp.sql, rows, ms, rej: tp.rej || '', note: tp.note || ''});
      t -= (12 + r() * 380) * 60000;
    }
    return out;
  })();
  function auQRows() {
    const q = AU.qQ.trim().toLowerCase();
    return QLOG.filter(x => (!AU.qUser || x.user === AU.qUser) && (!AU.qInst || String(x.inst) === AU.qInst) && (!AU.qRej || x.rej) && (!q || `${x.sql} ${x.db} ${x.rej} ${x.note}`.toLowerCase().includes(q)));
  }
  function auQList() {
    const rows = auQRows(), n = Math.max(1, Math.ceil(rows.length / AU.size));
    if (AU.page > n) AU.page = n;
    AU.view = rows; const base = (AU.page - 1) * AU.size;
    return tbl(['时间', '用户', '实例 / 库', 'SQL', '行数', '耗时', '结果'], rows.slice(base, base + AU.size), {
      attrs: (x, i) => ` class="click" data-on="auQOpen" data-i="${base + i}"`,
      cells: x => [`<span class="mono small nowrap">${x.at}</span>`, `<b>${esc(x.user)}</b>`, `${esc(instOf(x.inst).name)}<div class="small muted">${esc(x.db)}</div>`,
        `<span class="mono small" title="${esc(x.sql)}">${esc(x.sql.length > 70 ? x.sql.slice(0, 70) + '…' : x.sql)}</span>`, x.rej ? '—' : fmt(x.rows), x.rej ? '—' : `<span class="${x.ms > 2000 ? 'up' : ''}">${fmtMs(x.ms)}</span>`,
        x.rej ? `<span class="tag bad" title="${esc(x.rej)}">已拒绝</span>` : x.note ? `<span class="tag warn" title="${esc(x.note)}">已处理</span>` : '<span class="tag ok">正常</span>']
    }) + auPager(rows.length, AU.page, AU.size);
  }
  function auQuery() {
    const rej = QLOG.filter(x => x.rej).length, ok = QLOG.filter(x => !x.rej), avg = ok.length ? Math.round(ok.reduce((a, b) => a + b.ms, 0) / ok.length) : 0, slow = ok.reduce((a, b) => (b.ms > a.ms ? b : a), {ms: 0, user: '—'});
    const users = [...new Set(QLOG.map(x => x.user))].sort(), insts = [...new Set(QLOG.map(x => x.inst))];
    return `<div class="grid g4" style="margin-bottom:16px">
      <div class="card kpi"><div class="l">近 12 天查询</div><div class="v">${QLOG.length}</div><div class="d flat">${users.length} 位用户</div></div>
      <div class="card kpi"><div class="l">被拒绝</div><div class="v">${rej}</div></div>
      <div class="card kpi"><div class="l">平均耗时</div><div class="v">${fmtMs(avg)}</div></div>
      <div class="card kpi"><div class="l">最慢一条</div><div class="v">${fmtMs(slow.ms)}</div><div class="d flat">${esc(slow.user)}</div></div></div>
    <div class="toolbar"><select data-onchange="auF" data-k="qUser"><option value="">全部用户</option>${users.map(u => opt(u, u, AU.qUser)).join('')}</select>
      <select data-onchange="auF" data-k="qInst"><option value="">全部实例</option>${insts.map(i => opt(i, instOf(i).name, AU.qInst)).join('')}</select>
      <input type="search" placeholder="搜索 SQL / 库 / 原因" data-oninput="auF" data-k="qQ" value="${esc(AU.qQ)}" style="min-width:240px">
      <label class="chk"><input type="checkbox" data-onchange="auF" data-k="qRej" ${AU.qRej ? 'checked' : ''}> 仅被拒绝</label></div>
    <div class="card" id="auList">${auQList()}</div>`;
  }

  function auRefresh() { const b = $('#auList'); if (!b) return; b.innerHTML = AU.tab === 'wf' ? auWfList() : AU.tab === 'query' ? auQList() : auListHtml(); }
  ON.auF = (d, el) => { AU[d.k] = el.type === 'checkbox' ? el.checked : el.value; AU.page = 1; if (d.k === 'range') return rerender(); auRefresh(); };
  ON.auPage = d => { AU.page = +d.p; auRefresh(); };
  ON.auReset = () => { Object.assign(AU, {range: '30d', from: '', to: '', user: '', cat: '', res: '', risk: false, q: '', page: 1}); rerender(); };
  ON.auOpen = d => {
    const e = AU.view[d.i]; if (!e) return;
    const meta = BTNS[e.code], u = userBy(e.user);
    const same = AUDIT.filter(x => x.user === e.user && x !== e).slice(0, 6);
    openDrawer('审计详情', `${kvList([['时间', `<span class="mono">${e.at}</span>`], ['用户', `<b>${esc(e.user)}</b> ${u ? `<span class="muted">${esc(u.name)} · ${u.super ? '超级用户' : esc(u.groups.map(groupName).join(' / '))}</span>` : '<span class="muted">未登录 / 不存在的账号</span>'}`],
      ['来源 IP', `<span class="mono">${esc(e.ip)}</span>`], ['类别 / 模块', `${catName(e)} · ${esc(e.module)}`], ['权限码', e.code === 'auth.login' ? '<code>auth.login</code>（登录认证）' : `<code>${esc(e.code)}</code>${meta ? ` ${esc(meta.label)} ${meta.kind === 'd' ? '<span class="tag bad">高危</span>' : meta.kind === 'v' ? '<span class="tag info">查看</span>' : ''}${meta.locked ? ' <span class="tag">仅超级用户</span>' : ''}` : ''}`],
      ['动作', esc(e.act)], ['结果', resTag(e.res)], ['额外信息', esc(e.extra)]])}
      ${same.length ? `<h4>该用户的其他近期操作</h4>${tbl(['时间', '动作', '结果'], same.map(x => [`<span class="mono small">${x.at}</span>`, esc(x.act.length > 40 ? x.act.slice(0, 40) + '…' : x.act), resTag(x.res)]))}` : ''}`, false);
  };
  ON.auQOpen = d => {
    const x = AU.view[d.i]; if (!x) return;
    openDrawer('查询详情', `${kvList([['时间', `<span class="mono">${x.at}</span>`], ['用户', `<b>${esc(x.user)}</b> ${esc(userName(x.user))}`], ['实例', esc(instOf(x.inst).name)], ['库', esc(x.db)], ['行数', x.rej ? '—' : String(x.rows)], ['耗时', x.rej ? '—' : fmtMs(x.ms)],
      ['结果', x.rej ? `<span class="tag bad">已拒绝</span> ${esc(x.rej)}` : x.note ? `<span class="tag warn">已处理</span> ${esc(x.note)}` : '<span class="tag ok">正常</span>']])}<h4>SQL</h4>${code(x.sql, 'sql')}`, false);
  };

  /* CSV 导出：UTF-8 BOM、防公式注入（以 = + - @ 开头的单元格前补 '）、最多 50000 行 */
  function auCsv(rows) {
    const cell = s => { s = String(s == null ? '' : s); if (/^[=+\-@\t\r]/.test(s)) s = "'" + s; return '"' + s.replace(/"/g, '""') + '"'; };
    return '﻿' + [['时间', '用户', '姓名', '来源IP', '类别', '模块', '权限码', '动作', '结果', '额外信息']].concat(rows.slice(0, 50000).map(e => [e.at, e.user, userName(e.user), e.ip, catName(e), e.module, e.code, e.act, e.res, e.extra])).map(r => r.map(cell).join(',')).join('\r\n');
  }
  function auDownload(rows) {
    const text = auCsv(rows), name = `audit_${TODAY.replace(/-/g, '')}_${clock().replace(/:/g, '').slice(0, 4)}.csv`;
    try {
      const url = URL.createObjectURL(new Blob([text], {type: 'text/csv;charset=utf-8'})), a = document.createElement('a');
      a.href = url; a.download = name; document.body.appendChild(a); a.click(); setTimeout(() => { URL.revokeObjectURL(url); a.remove(); }, 800);
      return name;
    } catch (e) { copyText(text); return name + '（浏览器禁止下载，内容已复制到剪贴板）'; }
  }
  ACTIONS['audit.export'] = () => {
    const rows = auFiltered(); if (!rows.length) return toast('当前筛选条件下没有可导出的记录');
    risky('audit.export', '导出审计日志（CSV）', `将导出当前筛选结果 <b>${rows.length}</b> 条${rows.length > 50000 ? '（超过上限，截断为 50000 条）' : ''}。<br>筛选条件：${esc(auFilterDesc())}。<br><b>导出行为本身会写入系统审计</b>。`, null, () => {
      const name = auDownload(rows);
      audit('audit.export', `导出审计日志 CSV：${rows.length} 条（${auFilterDesc()}）`, '成功', name);
      toast(`已导出 ${rows.length} 条审计记录（${name}）`); rerender();
    });
  };

  pages.audit = {
    render(sub) {
      const tab = ['sys', 'wf', 'query'].includes(sub) ? sub : 'sys';
      if (AU.lastTab !== tab) { AU.page = 1; AU.lastTab = tab; }
      AU.tab = tab;
      return CSS2 + tabs('audit', [['sys', '系统审计'], ['wf', 'SQL 上线审计'], ['query', '查询审计']], tab) + (tab === 'sys' ? auSys() : tab === 'wf' ? auWf() : auQuery());
    }
  };

  /* ============================================================================
     登录方式 authp（直接读写 perm.js 的 SSO_PROVIDERS；登录页每次打开都会读取）
     启用前必须“测试通过”；连接配置变更后自动停用并作废测试结果；密钥加密存储、留空不改。
     ========================================================================== */
  const AP_KINDS = {
    'LDAP': {key: 'ldap', name: 'LDAP / AD 域', ico: 'L', color: '#4b5bdc', login: 'tab', req: ['url'], map: {user: '（登录时输入的账号）', name: 'displayName|cn', email: 'mail'}, fields: [
      {k: 'url', l: '服务器地址', t: 'text', ph: 'ldaps://ldap.example.com:636'},
      {k: 'secure', l: '连接方式', t: 'select', o: ['LDAPS（SSL）', 'StartTLS', '明文（不建议）']},
      {k: 'verifyCert', l: '校验服务端证书', t: 'check'},
      {k: 'bindDn', l: '绑定 DN（查询账号）', t: 'text', ph: 'cn=svc-dba,ou=svc,dc=example,dc=com'},
      {k: 'bindPwd', l: '绑定密码', t: 'secret'},
      {k: 'baseDn', l: '用户搜索 Base', t: 'text', ph: 'ou=people,dc=example,dc=com'},
      {k: 'filter', l: '用户搜索过滤', t: 'text', ph: '(sAMAccountName=%(user)s)'},
      {k: 'dnTpl', l: '用户 DN 模板（与搜索过滤二选一）', t: 'text', ph: 'uid=%(user)s,ou=people,dc=example,dc=com'},
      {k: 'syncProfile', l: '每次登录同步更新姓名 / 邮箱', t: 'check', nf: 1}]},
    'OIDC': {key: 'oidc', name: 'OIDC（标准 OpenID Connect）', ico: 'O', color: '#e9a23b', login: 'btn', req: ['wellknown', 'clientId', 'clientSecret'], map: {user: 'preferred_username|email_local', name: 'name|preferred_username', email: 'email'}, fields: [
      {k: 'wellknown', l: 'Wellknown URL', t: 'text', ph: 'https://sso.example.com/.well-known/openid-configuration'},
      {k: 'clientId', l: 'Client ID', t: 'text'}, {k: 'clientSecret', l: 'Client Secret', t: 'secret'},
      {k: 'scopes', l: 'Scopes', t: 'text', d: 'openid profile email'}, {k: 'alg', l: 'id_token 签名算法', t: 'select', o: ['RS256', 'ES256']},
      {k: 'callback', l: '回调地址（填到身份提供方）', t: 'ro'}]},
    '钉钉': {key: 'dingtalk', name: '钉钉', ico: '钉', color: '#1677ff', login: 'btn', req: ['appKey', 'appSecret'], map: {user: 'org_email_local|email_local|userid', name: 'name', email: 'email|org_email'}, fields: [
      {k: 'appKey', l: 'AppKey', t: 'text'}, {k: 'appSecret', l: 'AppSecret', t: 'secret'}, {k: 'corpId', l: '企业 CorpId（可空）', t: 'text'},
      {k: 'backfill', l: '登录后回填钉钉 UserID（用于通知 @）', t: 'check', nf: 1}, {k: 'callback', l: '回调地址', t: 'ro'}]},
    'CAS': {key: 'cas', name: 'CAS', ico: 'C', color: '#8892a6', login: 'btn', req: ['url'], map: {user: 'user', name: 'displayName|cn|name|user', email: 'mail|email'}, fields: [
      {k: 'url', l: 'CAS 服务地址', t: 'text', ph: 'https://cas.example.com/cas/'}, {k: 'version', l: '协议版本', t: 'select', o: ['3', '2', '1']},
      {k: 'verifyCert', l: '校验 CAS 服务端证书', t: 'check'}, {k: 'forceHttps', l: '强制 service URL 使用 https', t: 'check'}, {k: 'globalLogout', l: '退出时同时退出 CAS（全局登出）', t: 'check', nf: 1}]},
    '飞书': {key: 'feishu', name: '飞书', ico: '飞', color: '#3370ff', login: 'btn', req: ['appId', 'appSecret'], map: {user: 'user_id|email_local|enterprise_email_local', name: 'name|en_name', email: 'email|enterprise_email'}, fields: [
      {k: 'appId', l: 'App ID', t: 'text', ph: 'cli_xxx'}, {k: 'appSecret', l: 'App Secret', t: 'secret'}, {k: 'tenantKey', l: '租户限制 tenant_key（可空）', t: 'text'},
      {k: 'backfill', l: '登录后回填飞书 OpenID（用于通知 @）', t: 'check', nf: 1}, {k: 'callback', l: '回调地址', t: 'ro'}]},
    'Lark': {key: 'lark', name: 'Lark（国际版飞书）', ico: 'La', color: '#00b0a8', login: 'btn', req: ['appId', 'appSecret'], map: {user: 'user_id|email_local|enterprise_email_local', name: 'name|en_name', email: 'email|enterprise_email'}, fields: [
      {k: 'appId', l: 'App ID', t: 'text', ph: 'cli_xxx'}, {k: 'appSecret', l: 'App Secret', t: 'secret'}, {k: 'tenantKey', l: '租户限制 tenant_key（可空）', t: 'text'},
      {k: 'backfill', l: '登录后回填 OpenID（用于通知 @）', t: 'check', nf: 1}, {k: 'callback', l: '回调地址', t: 'ro'}]},
    'Slack': {key: 'slack', name: 'Slack', ico: 'S', color: '#611f69', login: 'btn', req: ['clientId', 'clientSecret'], map: {user: 'email_local', name: 'name|real_name', email: 'email'}, fields: [
      {k: 'clientId', l: 'Client ID', t: 'text'}, {k: 'clientSecret', l: 'Client Secret', t: 'secret'}, {k: 'teamId', l: '限制工作区 Team ID（可空）', t: 'text', ph: 'T0123ABCD'},
      {k: 'backfill', l: '登录后回填 Slack ID（用于通知 @）', t: 'check', nf: 1}, {k: 'callback', l: '回调地址', t: 'ro'}]},
    '企业微信': {key: 'wecom', name: '企业微信', ico: '企', color: '#2b9a5b', login: 'btn', req: ['corpId', 'agentId', 'secret'], map: {user: 'userid', name: 'name|userid', email: 'biz_mail|email'}, fields: [
      {k: 'corpId', l: '企业 CorpId', t: 'text', ph: 'ww…'}, {k: 'agentId', l: '应用 AgentId', t: 'text'}, {k: 'secret', l: '应用 Secret', t: 'secret'},
      {k: 'mode', l: '授权方式', t: 'select', o: ['企业微信内免登 + 浏览器扫码', '仅浏览器扫码']}, {k: 'callback', l: '可信域名 / 回调地址', t: 'ro'}]},
  };
  const AP_ORDER = ['LDAP', 'OIDC', '钉钉', 'CAS', '飞书', 'Lark', 'Slack', '企业微信'];
  const AP_MATCH = [['username', '用户名'], ['email', '邮箱'], ['phone', '手机号'], ['thirdId', '第三方 ID（钉钉 UserID / 飞书 OpenID / Slack ID）']];
  const AP_SEED = {
    ldap: {cfg: {url: 'ldap://ldap.example.com:389', secure: 'StartTLS', bindDn: 'cn=svc-dba,ou=svc,dc=example,dc=com', baseDn: 'ou=people,dc=example,dc=com', filter: '(uid=%(user)s)'}, sec: {bindPwd: 1}, first: 'create', matchBy: ['username'], groups: ['g_default'], last: '2026-10-07 08:47', testAt: '2026-09-10 11:20'},
    feishu: {cfg: {appId: 'cli_a5b2c3d4e5f6'}, sec: {appSecret: 1}, first: 'existing', matchBy: ['email', 'username'], last: '2026-10-06 10:52', testAt: '2026-09-20 09:48',
      rules: [{attr: 'department', op: 'eq', val: '支付中心', ag: ['g_rd'], rg: [2]}]},
    dingtalk: {cfg: {appKey: 'dingoa8x2k1mq0p3', corpId: 'ding9f3c2a71e0b4'}, sec: {appSecret: 1}, first: 'existing', matchBy: ['username', 'email'], last: '2026-10-05 18:31', testAt: '2026-08-21 15:02'},
    wecom: {cfg: {corpId: 'ww7c1e5a9d3b2f40', agentId: '1000012'}, sec: {secret: 1}, first: 'existing', matchBy: ['username', 'thirdId'], last: '2026-10-06 10:02', testAt: '2026-08-21 15:30'},
    oidc: {cfg: {wellknown: 'https://sso.example.com/.well-known/openid-configuration', clientId: 'dbtool'}, sec: {clientSecret: 1}, first: 'create', matchBy: ['username', 'email'], groups: ['g_default'], rgs: [1], last: '2026-10-06 09:10', testAt: '2026-08-21 14:48',
      rules: [{attr: 'groups', op: 'contains', val: 'dba-team', ag: ['g_dba'], rg: [1, 2, 3, 4]}, {attr: 'department', op: 'eq', val: '订单中心', ag: ['g_rd'], rg: [1]}], syncRules: true},
    cas: {cfg: {url: 'https://cas.example.com/cas/', version: '3', verifyCert: true}, first: 'existing', matchBy: ['username'], testAt: '2026-10-04 18:22', testBad: '最近一次测试失败：证书校验失败（unable to verify the first certificate）'},
  };
  const AP = {drag: null, rules: []};
  const APR = {localForm: 'collapse', superLocal: true, ext2fa: false, backfill: true, selfBind: false};
  const AP_ICON = {ok: '✔', bad: '✖', warn: '⚠', info: 'ℹ'};
  const apKind = p => AP_KINDS[p.type] || AP_KINDS['OIDC'];
  const apCallback = p => `https://dba.example.com/login/sso/${p.id}/callback`;
  const apDefaultCfg = type => { const o = {}; AP_KINDS[type].fields.forEach(f => { if (f.t === 'select') o[f.k] = f.o[0]; else if (f.t === 'check') o[f.k] = f.k !== 'forceHttps'; else if (f.t === 'text') o[f.k] = f.d || ''; }); return o; };
  /* 连接配置指纹：属性映射 / 入组规则 / 同步开关不参与（改它们不需要重新测试） */
  const apFp = p => { const o = {}; apKind(p).fields.forEach(f => { if (!f.nf && f.t !== 'ro' && f.t !== 'secret') o[f.k] = p.cfg[f.k]; }); return hash8(JSON.stringify([p.type, o, p.secVer || 0])); };
  const apValid = p => !!p.tested && p.testHash === apFp(p);
  function apProto(p) {
    const c = p.cfg;
    return {LDAP: `LDAP 绑定认证（${c.url || '未配置'}）`, OIDC: `OpenID Connect（${(c.wellknown || '').replace(/\/\.well-known.*$/, '') || '未配置'}）`, 'CAS': `CAS ${c.version || 3}.0（${c.url || '未配置'}）`}[p.type] || (AP_KINDS[p.type].name + ' OAuth 2.0');
  }
  function apEnsure(p) {
    if (p.cfg) return p;
    const K = apKind(p), s = AP_SEED[p.id] || {};
    p.cfg = Object.assign(apDefaultCfg(p.type), s.cfg || {}); p.sec = Object.assign({}, s.sec || {}); p.secVer = 0;
    p.first = s.first || 'existing'; p.matchBy = (s.matchBy || ['username', 'email']).slice(); p.groups = (s.groups || []).slice(); p.rgs = (s.rgs || []).slice();
    p.map = Object.assign({}, K.map, s.map || {}); p.rules = (s.rules || []).map(r => ({...r, ag: r.ag.slice(), rg: r.rg.slice()})); p.syncRules = !!s.syncRules;
    p.last = s.last || ''; p.testAt = s.testAt || ''; p.testBad = s.testBad || '';
    p.tested = !!p.on; p.testHash = p.tested ? apFp(p) : '';
    return p;
  }
  SSO_PROVIDERS.forEach(apEnsure);
  function apMissing(p) {
    const K = apKind(p), out = [];
    K.fields.forEach(f => { if (!(K.req || []).includes(f.k)) return; if (!(f.t === 'secret' ? p.sec[f.k] : p.cfg[f.k])) out.push(f.l); });
    if (p.type === 'LDAP' && !p.cfg.dnTpl && !(p.cfg.baseDn && p.cfg.filter)) out.push('用户搜索 Base + 过滤器（或 DN 模板）');
    return out;
  }
  const apBound = p => USERS.filter(u => u.auth === p.type).length;
  const apStateTag = p => p.on ? '<span class="tag ok">启用</span>' : p.testBad ? '<span class="tag bad">异常</span>' : apValid(p) ? '<span class="tag info">停用 · 已测试</span>' : '<span class="tag warn">停用 · 未测试</span>';

  /* 测试登录：返回 {ok, lines:[[cls, text]], msg}；不创建用户、不写绑定、不建会话 */
  function apTest(p, acct) {
    const L = [], add = (c, t) => L.push([c, t]), K = apKind(p), r = rng(parseInt(hash8(p.id + (p.secVer || 0)), 16));
    const fail = msg => ({ok: false, lines: L, msg});
    const miss = apMissing(p);
    if (miss.length) { add('bad', `配置不完整，缺少：${miss.join('、')}`); return fail('配置不完整：' + miss.join('、')); }
    const lat = 18 + Math.floor(r() * 90), main = p.cfg.url || p.cfg.wellknown || '';
    if (main) {
      const m = main.match(/^([a-z]+):\/\/([^\/:]+)/);
      if (!m) { add('bad', `地址格式不正确：${main}`); return fail('地址格式不正确'); }
      add('info', `解析 ${m[2]} → 10.${20 + Math.floor(r() * 5)}.${Math.floor(r() * 9)}.${10 + Math.floor(r() * 80)}`);
      if (p.type === 'LDAP' && p.cfg.secure === '明文（不建议）') add('warn', '使用明文 LDAP 连接，账号密码在网络上以明文传输，不建议用于生产');
      if (p.type === 'CAS' && p.cfg.verifyCert && /cas\.example\.com/.test(main)) {
        add('bad', '证书校验失败：unable to verify the first certificate（服务端证书链不完整）');
        add('info', '请修复证书链后重测；或临时取消「校验 CAS 服务端证书」（不建议）。');
        return fail('证书校验失败：unable to verify the first certificate');
      }
      add('ok', `连接 ${m[2]} 成功（延迟 ${lat} ms${m[1] === 'https' || m[1] === 'ldaps' ? '，TLS 1.3' : ''}）`);
    } else add('ok', `连接 ${K.name} 开放平台成功（延迟 ${lat} ms）`);
    if (p.type === 'OIDC') add('ok', `discovery 正常：issuer 与 Wellknown 地址一致；JWKS 取到 2 个 ${p.cfg.alg} 公钥`);
    let u;
    if (p.type === 'LDAP') {
      if (!acct.u || !acct.pw) { add('bad', '请填写测试账号和密码（空密码一律拒绝，避免被当作匿名绑定而“成功”）'); return fail('未提供测试账号 / 密码'); }
      add('ok', `使用查询账号 ${p.cfg.bindDn || '（匿名）'} 绑定成功`);
      u = userBy(acct.u);
      if (!u || acct.pw !== DEMO_PWD) { add('bad', u ? '用户认证失败：Invalid Credentials (49)' : `目录中搜索不到账号 ${acct.u}（${p.cfg.dnTpl ? 'DN 模板' : '过滤器 ' + p.cfg.filter}）`); return fail('LDAP 认证失败'); }
      add('ok', p.cfg.dnTpl ? '按 DN 模板直接绑定，认证通过' : '先搜后绑：搜到 1 个条目，用户认证通过');
    } else {
      u = userBy(p.demoUser) || userBy('rd_zhao');
      add('ok', `授权完成并取得 ${p.name} 用户信息${p.type === 'OIDC' ? '（id_token 签名 / nonce / aud / exp 校验通过）' : p.type === 'CAS' ? '（ticket 校验通过，service 一致）' : ''}`);
    }
    add('ok', `取得属性：username=${u.login}　display=${u.name}　email=${u.email}`);
    const ext = {department: u.dept, groups: u.groups.map(groupName).join(','), email: u.email, username: u.login, title: '工程师'};
    const hit = (p.rules || []).filter(x => { const v = String(ext[x.attr] || ''); return x.op === 'eq' ? v === x.val : v.includes(x.val); });
    if (hit.length) hit.forEach(x => add('ok', `命中入组规则：${x.attr} ${x.op === 'eq' ? '=' : '包含'} ${x.val} → 权限组 ${ylist(x.ag.map(groupName), '—')}、资源组 ${ylist(x.rg.map(rgName), '—')}`));
    else add('info', `未命中任何入组规则：${p.first === 'create' ? `自动创建的用户将按默认入组（权限组 ${ylist(p.groups.map(groupName), '全局默认')}、资源组 ${ylist(p.rgs.map(rgName), '全局默认')}）` : '已有用户的组不变'}`);
    let found = null, how = '';
    for (const k of p.matchBy) {
      const f = {username: () => USERS.find(a => a.login === u.login), email: () => USERS.find(a => a.email === u.email), phone: () => null, thirdId: () => null}[k]();
      if (f) { found = f; how = AP_MATCH.find(m => m[0] === k)[1].replace(/（.*/, ''); break; }
    }
    if (found && found.super && APR.superLocal) add('warn', `匹配到超级用户 ${found.login}：已设置“超级用户只能本地登录”，平台不会放行`);
    else if (found && found.status !== 'active') add('warn', `匹配到用户 ${found.login}，但该账号已被锁定 / 停用，登录会被拒绝`);
    else if (found) add('info', `匹配到已有用户 ${found.login}（按${how}），登录后进入该账号`);
    else if (p.first === 'create') add('info', `未匹配到已有用户：首次登录将自动创建 ${u.login}（随机不可用密码，display 截断到 10 个字符）`);
    else add('warn', '未匹配到已有用户，且设置为「仅限已有用户」：平台不会放行');
    return {ok: true, lines: L, msg: '测试通过'};
  }
  function apFinish(p, res) {
    p.testAt = now();
    if (res.ok) { p.tested = true; p.testHash = apFp(p); p.testBad = ''; }
    else { p.tested = false; p.testHash = ''; p.testBad = '最近一次测试失败：' + res.msg; if (p.on) { p.on = false; toast(`「${p.name}」测试失败，已自动停用，登录页上的按钮已移除`); } }
    audit('authp.test', `测试登录「${p.name}」${res.ok ? '成功' : '失败：' + res.msg}`, res.ok ? '成功' : '失败');
  }

  /* ---- 页面 ---- */
  function apPreview() {
    const on = SSO_PROVIDERS.filter(p => p.on && p.login === 'btn'), ldap = SSO_PROVIDERS.some(p => p.on && p.type === 'LDAP');
    return `<div style="max-width:380px;margin:0 auto;border:1px solid var(--border);border-radius:14px;padding:18px;background:var(--panel2)">
      <div style="font-size:18px;font-weight:700">欢迎登录</div><div class="small muted">请使用平台账号或公司统一账号登录</div>
      <div class="subtabs" style="margin:12px 0"><a class="on">账号密码</a>${ldap ? '<a>LDAP 统一认证</a>' : ''}</div>
      <div style="display:grid;gap:8px"><div style="${INP};color:var(--muted)">账号</div><div style="${INP};color:var(--muted)">密码</div><div class="btn primary" style="text-align:center">登 录</div></div>
      ${on.length ? `<div class="small muted" style="text-align:center;margin:14px 0 8px">— 其他登录方式 —</div><div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(70px,1fr));gap:8px">${on.map(p => `<div style="border:1px solid var(--border);background:var(--panel);border-radius:10px;padding:8px 2px;text-align:center;font-size:11px"><i class="p2-ico" style="background:${p.color};margin:0 0 2px">${esc(p.ico)}</i><div>${esc(p.name)}</div></div>`).join('')}</div>` : '<div class="small muted" style="text-align:center;margin-top:12px">没有启用的外部登录方式</div>'}</div>`;
  }
  pages.authp = {
    render() {
      const list = SSO_PROVIDERS, on = list.filter(p => p.on).length, untested = list.filter(p => !p.on && !p.testBad && !apValid(p)).length, bad = list.filter(p => !p.on && p.testBad).length;
      const rows = list.map((p, i) => `<tr data-id="${esc(p.id)}"><td class="drag" title="拖动调整登录页显示顺序">⋮⋮</td>
        <td><i class="p2-ico" style="background:${p.color}">${esc(p.ico)}</i><b>${esc(p.name)}</b><div class="small muted mono">${esc(p.id)}</div></td><td><span class="tag">${esc(p.type)}</span></td>
        <td>${apStateTag(p)}${p.testBad && !p.on ? `<div class="small" style="color:var(--bad)">${esc(p.testBad)}</div>` : p.testAt ? `<div class="small muted">测试 ${esc(p.testAt)}</div>` : ''}</td>
        <td class="small">${p.login === 'tab' ? 'LDAP 页签（走账号密码表单）' : `按钮「${esc(p.name)}」`}</td><td class="small">${p.first === 'create' ? '自动创建用户' : '仅限已有用户'}</td>
        <td class="small">${p.first === 'create' || p.groups.length ? `${esc(p.groups.map(groupName).join(' / ') || '全局默认')}<div class="muted">${esc(p.rgs.map(rgName).join(' / ') || '—')}</div>` : '<span class="muted">—</span>'}</td>
        <td class="small">${esc(p.last || '—')}</td>
        <td><div class="ops">${B('authp.edit', '配置', {cls: 'sm', data: {mode: 'edit', id: p.id}})}${B('authp.test', '测试', {cls: 'sm', data: {id: p.id}})}${p.on ? B('authp.toggle', '停用', {cls: 'sm', data: {op: 'off', id: p.id}}) : B('authp.toggle', '启用', {cls: 'sm', data: {op: 'on', id: p.id}})}${!p.on ? B('authp.toggle', '删除', {cls: 'sm danger', data: {op: 'del', id: p.id}}) : ''}${i > 0 ? B('authp.toggle', '↑', {cls: 'sm', data: {op: 'up', id: p.id}, title: '上移'}) : ''}${i < list.length - 1 ? B('authp.toggle', '↓', {cls: 'sm', data: {op: 'down', id: p.id}, title: '下移'}) : ''}</div></td></tr>`).join('');
      return CSS2 + `
      <div class="grid g4" style="margin-bottom:16px">
        <div class="card kpi"><div class="l">已定义（外部）</div><div class="v">${list.length}</div></div>
        <div class="card kpi"><div class="l">启用中</div><div class="v">${on}</div></div>
        <div class="card kpi"><div class="l">未测试</div><div class="v">${untested}</div></div>
        <div class="card kpi"><div class="l">测试异常</div><div class="v">${bad}</div></div></div>
      <div class="card"><h2>已定义的登录方式<span class="right">${B('authp.edit', '＋ 添加登录方式', {cls: 'primary', data: {mode: 'add'}})}</span></h2>
        <div class="tbl-wrap"><table><thead><tr><th></th><th>名称</th><th>类型</th><th>状态</th><th>登录页</th><th>首次登录</th><th>默认入组</th><th>最近成功登录</th><th>操作</th></tr></thead><tbody id="apRows">
          <tr><td></td><td><i class="p2-ico" style="background:var(--muted)">本</i><b>本地账号密码</b><div class="small muted mono">local</div></td><td><span class="tag brand">内置</span></td><td><span class="tag ok">启用</span></td><td class="small">账号 + 密码表单</td><td class="small muted">不适用</td><td class="small muted">—</td><td class="small">${esc(USERS[0].last)}</td><td><a class="btn sm" href="#sec">去安全设置</a></td></tr>${rows}</tbody></table></div></div>
      <div class="grid g2" style="margin-top:16px">
        <div class="card"><h2>通用规则 <span class="right">${B('authp.edit', '保存通用规则', {cls: 'primary sm', data: {mode: 'rules'}})}</span></h2><div class="form">
          ${fld('本地账号表单', `<select id="ar_localForm">${[['always', '始终显示'], ['collapse', '折叠'], ['hide', '隐藏（仅 /login?local=1 可用）']].map(o => opt(o[0], o[1], APR.localForm)).join('')}</select>`)}
          ${fld('', `<label class="chk"><input type="checkbox" id="ar_superLocal" ${APR.superLocal ? 'checked' : ''}> 超级用户只能本地登录</label>`)}
          ${fld('', `<label class="chk"><input type="checkbox" id="ar_ext2fa" ${APR.ext2fa ? 'checked' : ''}> 外部登录后仍要求两步验证</label>`)}
          ${fld('', `<label class="chk"><input type="checkbox" id="ar_backfill" ${APR.backfill ? 'checked' : ''}> 登录后把第三方 ID 回填到用户资料</label>`)}
          ${fld('', `<label class="chk"><input type="checkbox" id="ar_selfBind" ${APR.selfBind ? 'checked' : ''}> 允许用户自行绑定 / 解绑外部账号</label>`)}</div></div>
        <div class="card"><h2>登录页预览</h2>${apPreview()}</div></div>`;
    },
    init() {
      if (!can('authp.toggle')) return;
      $$('#apRows tr[data-id]').forEach(tr => {
        tr.draggable = true;
        tr.ondragstart = e => { AP.drag = tr.dataset.id; e.dataTransfer.effectAllowed = 'move'; try { e.dataTransfer.setData('text/plain', tr.dataset.id); } catch (x) {} };
        tr.ondragover = e => { e.preventDefault(); tr.classList.add('p2-over'); };
        tr.ondragleave = () => tr.classList.remove('p2-over');
        tr.ondrop = e => { e.preventDefault(); tr.classList.remove('p2-over'); apMove(AP.drag, tr.dataset.id); };
      });
    }
  };
  function apMove(from, to) {
    const i = SSO_PROVIDERS.findIndex(x => x.id === from), j = SSO_PROVIDERS.findIndex(x => x.id === to);
    if (i < 0 || j < 0 || i === j) return;
    const [p] = SSO_PROVIDERS.splice(i, 1); SSO_PROVIDERS.splice(j, 0, p);
    audit('authp.toggle', `调整登录页按钮顺序：${SSO_PROVIDERS.map(x => x.name).join(' → ')}`); toast('已调整登录页显示顺序'); rerender();
  }

  /* ---- 启用 / 停用 / 删除 / 排序（authp.toggle，高危） ---- */
  ACTIONS['authp.toggle'] = d => {
    const i = SSO_PROVIDERS.findIndex(x => x.id === d.id), p = SSO_PROVIDERS[i]; if (!p) return;
    if (d.op === 'up' || d.op === 'down') {
      const j = d.op === 'up' ? i - 1 : i + 1; if (j < 0 || j >= SSO_PROVIDERS.length) return;
      [SSO_PROVIDERS[i], SSO_PROVIDERS[j]] = [SSO_PROVIDERS[j], SSO_PROVIDERS[i]];
      audit('authp.toggle', `${d.op === 'up' ? '上移' : '下移'}登录方式「${p.name}」：${SSO_PROVIDERS.map(x => x.name).join(' → ')}`); toast('已调整登录页显示顺序'); return rerender();
    }
    if (d.op === 'on') {
      if (!apValid(p)) {
        audit('authp.toggle', `尝试启用登录方式「${p.name}」被拒绝：尚未测试通过（或连接配置已变更）`, '拒绝');
        return openModal({title: '不能启用：尚未测试通过', ok: '去测试', cancel: '知道了', code: 'authp.toggle',
          body: notice(`「${esc(p.name)}」${p.testBad ? '最近一次测试失败：' + esc(p.testBad.replace(/^最近一次测试失败：/, '')) : '还没有测试通过，或测试后修改过连接配置'}。<b>测试通过后才能启用</b>。`, 'warn', '⚠'),
          onOk() { setTimeout(() => ACTIONS['authp.test']({id: p.id}), 0); }});
      }
      return risky('authp.toggle', `启用登录方式「${p.name}」`, `启用后登录页立即出现「${esc(p.name)}」${p.login === 'tab' ? '页签' : '按钮'}。<br>测试通过时间：${esc(p.testAt)}；首次登录：${p.first === 'create' ? '自动创建用户' : '仅限已有用户'}。`, null, () => {
        p.on = true; audit('authp.toggle', `启用登录方式「${p.name}」（${p.type}，测试通过 ${p.testAt}）`); toast(`已启用「${p.name}」，登录页立即生效`); rerender();
      });
    }
    if (d.op === 'off') {
      const n = apBound(p);
      return risky('authp.toggle', `停用登录方式「${p.name}」`, `停用后登录页不再显示「${esc(p.name)}」。${n ? `当前有 <b>${n}</b> 个账号使用「${esc(p.type)}」认证，他们将无法再用该方式登录。` : ''}`, null, () => {
        p.on = false; audit('authp.toggle', `停用登录方式「${p.name}」（影响 ${n} 个账号）`); toast(`已停用「${p.name}」`); rerender();
      });
    }
    if (d.op === 'del') {
      if (p.on) return toast('须先停用，再删除');
      const n = apBound(p);
      return risky('authp.toggle', `删除登录方式「${p.name}」`, `删除后配置与密钥一并清除，<b>不可恢复</b>；该方式下的外部身份绑定（${n} 个账号）一并清除。`, null, () => {
        SSO_PROVIDERS.splice(i, 1); audit('authp.toggle', `删除登录方式「${p.name}」（${p.type}）`); toast(`已删除「${p.name}」`); rerender();
      }, p.name);
    }
  };

  /* ---- 测试连接 / 测试登录（authp.test） ---- */
  ACTIONS['authp.test'] = d => {
    const p = SSO_PROVIDERS.find(x => x.id === d.id); if (!p) return;
    const isLdap = p.type === 'LDAP', u = userBy(p.demoUser);
    openModal({title: `测试登录 · ${p.name}`, code: 'authp.test', ok: '开始测试', cancel: '关闭', wide: true,
      body: `${isLdap ? `<div class="form">${fld('测试账号', '<input id="apTu" placeholder="如 dba_li" autocomplete="off">', {req: true})}${fld('密码', '<input id="apTp" type="password" autocomplete="new-password" placeholder="原型演示密码见登录页">', {req: true})}</div>`
        : notice(`原型中直接模拟授权，授权用户：<b>${esc(u ? u.login : '—')}</b>。`)}
        <div id="apTestOut" style="margin-top:10px"></div>`,
      onOk() {
        const acct = isLdap ? {u: val('apTu'), pw: ($('#apTp') || {}).value || ''} : null, res = apTest(p, acct), out = $('#apTestOut'), btn = $('#mOk');
        out.innerHTML = '<div class="p2-line info">⏳ 正在测试…</div>'; btn.disabled = true;
        let acc = '';
        res.lines.forEach((l, i) => setTimeout(() => { const o = $('#apTestOut'); if (!o) return; acc += `<div class="p2-line ${l[0]}">${AP_ICON[l[0]]} ${esc(l[1])}</div>`; o.innerHTML = acc + (i < res.lines.length - 1 ? '<div class="p2-line info">⏳ …</div>' : ''); }, 420 * (i + 1)));
        setTimeout(() => {
          apFinish(p, res);
          const o = $('#apTestOut'), b = $('#mOk');
          if (o) o.innerHTML = acc + `<div class="hr"></div><div class="${res.ok ? 'notice ok' : 'notice bad'}"><span>${res.ok ? '✔' : '✖'}</span><div>${res.ok ? '测试通过，现在可以启用该登录方式。' : '测试失败：' + esc(res.msg)}</div></div>`;
          if (b) { b.disabled = false; b.textContent = '重新测试'; }
          rerender();
        }, 420 * (res.lines.length + 1));
        return false;
      }});
  };

  /* ---- 添加 / 编辑（authp.edit） ---- */
  function apRulesHtml() {
    return AP.rules.length ? `<div class="tbl-wrap"><table><thead><tr><th>条件（外部属性）</th><th>加入权限组</th><th>加入资源组</th><th></th></tr></thead><tbody>${AP.rules.map((r, i) => `<tr data-ri="${i}">
      <td><div class="p2-wsel"><input class="mono" data-ar="attr" value="${esc(r.attr)}" style="width:100px" placeholder="属性"><select data-ar="op"><option value="eq" ${r.op === 'eq' ? 'selected' : ''}>等于</option><option value="contains" ${r.op === 'contains' ? 'selected' : ''}>包含</option></select><input class="mono" data-ar="val" value="${esc(r.val)}" style="width:130px" placeholder="值"></div></td>
      <td><select multiple size="3" data-ar="ag">${GROUPS.map(g => `<option value="${g.id}" ${r.ag.includes(g.id) ? 'selected' : ''}>${esc(g.name)}</option>`).join('')}</select></td>
      <td><select multiple size="3" data-ar="rg">${RGROUPS.filter(x => x.enabled).map(g => `<option value="${g.id}" ${r.rg.includes(g.id) ? 'selected' : ''}>${esc(g.name)}</option>`).join('')}</select></td>
      <td><button class="btn sm danger" data-on="apRuleDel" data-i="${i}">删除</button></td></tr>`).join('')}</tbody></table></div>` : '<div class="small muted" style="padding:6px 0">暂无规则</div>';
  }
  function apSyncRules() {
    $$('#apRules tr[data-ri]').forEach(tr => {
      const r = AP.rules[+tr.dataset.ri]; if (!r) return;
      r.attr = tr.querySelector('[data-ar=attr]').value.trim(); r.op = tr.querySelector('[data-ar=op]').value; r.val = tr.querySelector('[data-ar=val]').value.trim();
      r.ag = [...tr.querySelector('[data-ar=ag]').selectedOptions].map(o => o.value); r.rg = [...tr.querySelector('[data-ar=rg]').selectedOptions].map(o => +o.value);
    });
  }
  ON.apRuleAdd = () => { apSyncRules(); AP.rules.push({attr: 'department', op: 'eq', val: '', ag: [], rg: []}); $('#apRules').innerHTML = apRulesHtml(); };
  ON.apRuleDel = d => { apSyncRules(); AP.rules.splice(+d.i, 1); $('#apRules').innerHTML = apRulesHtml(); };
  function apFormHtml(p) {
    const K = apKind(p);
    const ctl = f => {
      const id = 'ap_' + f.k, v = p.cfg[f.k];
      if (f.t === 'secret') return `<input type="password" id="${id}" placeholder="${p.sec[f.k] ? '已设置（留空不修改）' : ''}" autocomplete="new-password">`;
      if (f.t === 'select') return `<select id="${id}">${f.o.map(o => opt(o, o, v)).join('')}</select>`;
      if (f.t === 'check') return `<label class="chk"><input type="checkbox" id="${id}" ${v ? 'checked' : ''}> ${f.l}</label>`;
      if (f.t === 'ro') return `<input class="mono" value="${esc(apCallback(p))}" readonly>`;
      return `<input class="mono" id="${id}" value="${esc(v || '')}" placeholder="${esc(f.ph || '')}">`;
    };
    return `<details class="p2-acc" open><summary>基本信息</summary><div class="form" style="padding:12px">
      ${fld('名称（登录页按钮文字）', `<input id="ap_name" value="${esc(p.name)}" maxlength="20">`, {req: true})}
      ${fld('首次登录', `<select id="ap_first">${opt('create', '自动创建用户', p.first)}${opt('existing', '仅允许已有用户登录（不自动创建）', p.first)}</select>`)}
      ${fld('匹配依据', `<div class="p2-ev">${AP_MATCH.map(m => `<label class="chk"><input type="checkbox" class="ap_mb" value="${m[0]}" ${p.matchBy.includes(m[0]) ? 'checked' : ''}>${m[1]}</label>`).join('')}</div>`)}
      ${fld('', `<label class="chk"><input type="checkbox" id="ap_sync" ${p.syncRules ? 'checked' : ''}> 每次登录都按入组规则同步</label>`)}</div></details>
    <details class="p2-acc" open><summary>连接参数</summary><div class="form" style="padding:12px">${K.fields.map(f => f.t === 'check' ? fld('', ctl(f)) : fld(f.l, ctl(f), {req: (K.req || []).includes(f.k)})).join('')}</div></details>
    <details class="p2-acc"><summary>用户属性映射</summary><div class="form" style="padding:12px">
      ${fld('用户名 username', `<input class="mono" id="ap_m_user" value="${esc(p.map.user)}">`)}${fld('姓名 display', `<input class="mono" id="ap_m_name" value="${esc(p.map.name)}">`)}${fld('邮箱 email', `<input class="mono" id="ap_m_email" value="${esc(p.map.email)}">`)}</div></details>
    <details class="p2-acc" open><summary>默认入组</summary><div class="form" style="padding:12px">
      ${fld('权限组', `<div class="p2-ev">${GROUPS.map(g => `<label class="chk"><input type="checkbox" class="ap_gp" value="${g.id}" ${p.groups.includes(g.id) ? 'checked' : ''}>${esc(g.name)}</label>`).join('')}</div>`)}
      ${fld('资源组', `<div class="p2-ev">${RGROUPS.filter(r => r.enabled).map(g => `<label class="chk"><input type="checkbox" class="ap_rg" value="${g.id}" ${p.rgs.includes(g.id) ? 'checked' : ''}>${esc(g.name)}</label>`).join('')}</div>`)}</div></details>
    <details class="p2-acc" open><summary>入组规则</summary><div style="padding:8px 12px"><div id="apRules">${apRulesHtml()}</div><button class="btn sm" data-on="apRuleAdd" style="margin-top:8px">＋ 添加规则</button></div></details>`;
  }
  function apEditModal(p) {
    AP.rules = p.rules.map(r => ({...r, ag: r.ag.slice(), rg: r.rg.slice()}));
    openModal({title: `配置登录方式 · ${p.name}${p.name === p.type ? '' : `（${p.type}）`}`, wide: true, code: 'authp.edit', ok: '保存', body: apFormHtml(p), onOk() { return apSave(p); }});
  }
  function apSave(p) {
    const K = apKind(p), cfg = {}, sec = {}, name = val('ap_name');
    if (!name) { toast('请填写名称'); return false; }
    if (SSO_PROVIDERS.some(x => x !== p && x.name === name)) { toast('名称已被其他登录方式使用'); return false; }
    K.fields.forEach(f => {
      const el = $('#ap_' + f.k); if (!el || f.t === 'ro') return;
      if (f.t === 'secret') { if (el.value) sec[f.k] = el.value; } else cfg[f.k] = f.t === 'check' ? el.checked : el.value.trim();
    });
    for (const k of ['url', 'wellknown']) {
      const v = cfg[k]; if (!v) continue;
      if (!(/^https:\/\//.test(v) || (p.type === 'LDAP' && /^ldaps?:\/\//.test(v)) || /^http:\/\/(127\.0\.0\.1|localhost)/.test(v))) { toast(p.type === 'LDAP' ? '服务器地址须以 ldap:// 或 ldaps:// 开头' : '地址必须是 https://（回环地址除外）'); return false; }
    }
    if (p.type === 'LDAP') {
      if (cfg.filter && !cfg.filter.includes('%(user)s')) { toast('用户搜索过滤必须包含 %(user)s'); return false; }
      if (cfg.dnTpl && !cfg.dnTpl.includes('%(user)s')) { toast('DN 模板必须包含 %(user)s'); return false; }
      if (cfg.filter && !/^\(.*\)$/.test(cfg.filter)) { toast('过滤器语法不正确：应为 (attr=value) 形式'); return false; }
    }
    const matchBy = checkedVals('.ap_mb'); if (!matchBy.length) { toast('请至少选择一种匹配依据'); return false; }
    apSyncRules();
    for (const r of AP.rules) { if (!r.attr || !r.val) { toast('入组规则的属性和值不能为空'); return false; } if (!r.ag.length && !r.rg.length) { toast('入组规则至少要加入一个权限组或资源组'); return false; } }
    const nv = {name, first: val('ap_first'), matchBy, groups: checkedVals('.ap_gp'), rgs: checkedVals('.ap_rg').map(Number), syncRules: chk('ap_sync'), map: {user: val('ap_m_user'), name: val('ap_m_name'), email: val('ap_m_email')}, rules: AP.rules.map(r => ({...r, ag: r.ag.slice(), rg: r.rg.slice()}))};
    const ch = [], diff = (l, a, b) => { if (String(a) !== String(b)) ch.push(`${l}：${a === '' || a == null ? '（空）' : a} → ${b === '' ? '（空）' : b}`); };
    diff('名称', p.name, nv.name); diff('首次登录', p.first === 'create' ? '自动创建' : '仅已有用户', nv.first === 'create' ? '自动创建' : '仅已有用户');
    diff('匹配依据', p.matchBy.join(','), nv.matchBy.join(',')); diff('默认权限组', p.groups.map(groupName).join('、'), nv.groups.map(groupName).join('、')); diff('默认资源组', p.rgs.map(rgName).join('、'), nv.rgs.map(rgName).join('、'));
    diff('每次登录同步入组', p.syncRules ? '是' : '否', nv.syncRules ? '是' : '否'); diff('入组规则', p.rules.map(r => r.attr + r.op + r.val + r.ag + r.rg).join(';'), nv.rules.map(r => r.attr + r.op + r.val + r.ag + r.rg).join(';'));
    ['user', 'name', 'email'].forEach(k => diff('属性映射 ' + k, p.map[k], nv.map[k]));
    K.fields.forEach(f => { if (f.t === 'secret') { if (sec[f.k]) ch.push(`${f.l}：已修改（密钥不记录）`); } else if (f.t !== 'ro') diff(f.l, f.t === 'check' ? (p.cfg[f.k] ? '是' : '否') : p.cfg[f.k], f.t === 'check' ? (cfg[f.k] ? '是' : '否') : cfg[f.k]); });
    if (!ch.length) { toast('没有变更'); return true; }
    const q = Object.assign({}, p, {cfg: Object.assign({}, p.cfg, cfg), secVer: (p.secVer || 0) + (Object.keys(sec).length ? 1 : 0)}), connChanged = apFp(q) !== apFp(p);
    risky('authp.edit', `保存登录方式「${name}」配置`, `${connChanged ? `<b>连接参数有变更</b>：${p.on ? '该登录方式将被<b>自动停用</b>，' : ''}测试结果作废，需重新测试通过后才能启用。<br>` : '无需重新测试。<br>'}变更内容：<ul style="margin:6px 0 0;padding-left:18px">${ch.map(c => `<li>${esc(c)}</li>`).join('')}</ul>`, null, () => {
      const wasOn = p.on;
      Object.assign(p, {name: nv.name, first: nv.first, matchBy: nv.matchBy, groups: nv.groups, rgs: nv.rgs, syncRules: nv.syncRules, map: nv.map, rules: nv.rules});
      Object.assign(p.cfg, cfg); Object.keys(sec).forEach(k => { p.sec[k] = 1; }); if (Object.keys(sec).length) p.secVer = (p.secVer || 0) + 1;
      if (connChanged) { p.tested = false; p.testHash = ''; p.testBad = ''; if (p.on) p.on = false; }
      p.proto = apProto(p);
      audit('authp.edit', `修改登录方式「${name}」：${ch.slice(0, 4).join('；')}${ch.length > 4 ? ` 等 ${ch.length} 项` : ''}${connChanged ? (wasOn ? '（连接配置变更，已自动停用，测试结果作废）' : '（连接配置变更，测试结果作废）') : ''}`);
      toast(connChanged ? `已保存「${name}」；连接配置变更，需重新测试后启用` : `已保存「${name}」`); rerender();
    });
    return false;
  }
  function apAddModal() {
    openModal({title: '添加登录方式', code: 'authp.edit', ok: '下一步',
      body: `<div class="form">${fld('类型', `<select id="apNewType">${AP_ORDER.map(t => `<option value="${t}">${AP_KINDS[t].name}</option>`).join('')}</select>`)}${fld('名称（登录页按钮文字）', '<input id="apNewName" placeholder="如：公司 SSO" maxlength="20">', {req: true})}</div>`,
      onOk() {
        const type = val('apNewType'), name = val('apNewName'), K = AP_KINDS[type];
        if (!name) { toast('请填写名称'); return false; }
        if (SSO_PROVIDERS.some(x => x.name === name)) { toast('名称已存在'); return false; }
        let n = 1, id = K.key; while (SSO_PROVIDERS.some(x => x.id === id)) id = `${K.key}-${++n}`;
        const p = {id, name, type, color: K.color, ico: K.ico, proto: '（未配置）', on: false, login: K.login, demoUser: 'rd_zhao'};
        apEnsure(p); p.proto = apProto(p); SSO_PROVIDERS.push(p);
        audit('authp.edit', `新增登录方式「${name}」（${type}，默认停用，需测试通过后启用）`); rerender();
        setTimeout(() => apEditModal(p), 0);
      }});
  }
  function apSaveRules() {
    const nv = {localForm: val('ar_localForm'), superLocal: chk('ar_superLocal'), ext2fa: chk('ar_ext2fa'), backfill: chk('ar_backfill'), selfBind: chk('ar_selfBind')};
    const lines = o => Object.entries(o).map(([k, v]) => `${k} = ${v}`).join('\n'), a = lines(APR), b = lines(nv);
    if (a === b) return toast('没有变更');
    risky('authp.edit', '保存登录通用规则', `通用规则影响<b>所有</b>登录方式，变更立即生效：${diffHtml(a, b)}${!nv.superLocal ? '<br><b>已取消“超级用户只能本地登录”：外部身份匹配到超级用户时将允许登录。</b>' : ''}`, null, () => {
      Object.assign(APR, nv); audit('authp.edit', '修改登录通用规则：' + b.split('\n').filter((l, i) => l !== a.split('\n')[i]).join('；')); toast('已保存通用规则'); rerender();
    });
  }
  ACTIONS['authp.edit'] = d => {
    if (d.mode === 'rules') return apSaveRules();
    if (d.mode === 'add') return apAddModal();
    const p = SSO_PROVIDERS.find(x => x.id === d.id); if (p) apEditModal(p);
  };

  /* ============================================================================
     登录与安全设置 sec（直接读写全局 SETTINGS；保存前展示变更差异并二次确认）
     ========================================================================== */
  /* 取值范围 / 单位 / 默认值 / 校验 / 文案以真实实现为准：internal/authx/settings.go（DefaultSettings、validateSecurity）、web/templates/auth/security.html、web/static/js/auth/security.js。
     默认值在 perm.js 的 SETTINGS；这里只补短信配置（短信密钥的“已配置”标记不回显明文）。 */
  if (!SETTINGS.sms) SETTINGS.sms = {al: {ak: 'LTAI5tQ8xJm2Zp4D', sign: 'DBA平台', tpl: 'SMS_248103347', var: 'code', sk: true}, tc: {sid: '', app: '', sign: '', tpl: '', sk: false}};
  if (SETTINGS.sms.al.var === undefined) SETTINGS.sms.al.var = 'code';
  let secWmTimer = null;

  const secSnap = () => ({lock: SETTINGS.loginFailLock, lockSec: SETTINGS.lockSeconds, f2fa: !!SETTINGS.force2fa, signup: !!SETTINGS.selfRegister, pwdLen: SETTINGS.pwdMinLen, pwdCx: !!SETTINGS.pwdComplexity,
    sessMin: SETTINGS.sessionMinutes, sessClose: !!SETTINGS.sessionBrowserClose, jwt: SETTINGS.jwtHours, jwtOtp: !!SETTINGS.jwtRequireOtp, api: SETTINGS.apiWhitelist.map(Number).sort((a, b) => a - b), dg: SETTINGS.defGroups.slice(), dr: SETTINGS.defRgs.slice().sort((a, b) => a - b),
    wm: !!SETTINGS.watermark, wmc: wmCfg(), smsP: SETTINGS.smsProvider, al: Object.assign({skNew: false}, SETTINGS.sms.al), tc: Object.assign({skNew: false}, SETTINGS.sms.tc)});
  /* 变更差异的行：[设置键, 显示名, 值]（顺序与显示名同真实 security.js 的 LABELS；密钥项的值是 {secret, has, neu}，只显示“已修改”） */
  const onOff = b => b ? '开启' : '关闭', listTxt = a => a.length ? a.map(String).sort().join('、') : '（空）', orEmpty = v => v === '' || v == null ? '（空）' : String(v);
  const secKey = o => ({secret: true, has: !!o.sk, neu: !!o.skNew});
  function secRows(s) {
    return [['lock_cnt_threshold', '登录失败锁定次数', s.lock], ['lock_time_threshold', '锁定时间（秒）', s.lockSec], ['enforce_2fa', '强制两步验证', onOff(s.f2fa)], ['sign_up_enabled', '开放自助注册', onOff(s.signup)],
      ['password_min_len', '密码最小长度', s.pwdLen], ['password_complexity', '密码需含字母 / 数字 / 特殊字符', onOff(s.pwdCx)], ['session_minutes', '会话有效期（分钟）', s.sessMin], ['session_browser_close', '关闭浏览器即退出', onOff(s.sessClose)],
      ['jwt_hours', 'JWT 有效期（小时）', s.jwt], ['api_jwt_require_otp', 'JWT 登录要求 OTP', onOff(s.jwtOtp)], ['api_user_whitelist', 'API 用户白名单（用户 ID）', listTxt(s.api)],
      ['default_auth_group', '新用户默认权限组', listTxt(s.dg.map(groupName))], ['default_resource_group', '新用户默认资源组', listTxt(s.dr.map(rgName))], ['watermark_enabled', '页面水印', onOff(s.wm)],
      ['watermark_text', '水印内容模板', orEmpty(s.wmc.text)], ['watermark_extra', '水印附加文字', orEmpty(s.wmc.extra)], ['watermark_opacity', '水印透明度（%）', s.wmc.opacity], ['watermark_font_size', '水印字号（px）', s.wmc.size],
      ['watermark_angle', '水印倾斜角度（°）', s.wmc.angle], ['watermark_gap', '水印疏密', (WM_GAPS[s.wmc.gap] || [s.wmc.gap])[0]], ['watermark_color', '水印颜色', WM_COLORS[s.wmc.color] || s.wmc.color],
      ['sms_provider', '短信服务商', {disabled: '关闭', aliyun: '阿里云', tencent: '腾讯云'}[s.smsP] || s.smsP], ['aliyun_access_key_id', '阿里云 AccessKey ID', orEmpty(s.al.ak)], ['aliyun_access_key_secret', '阿里云 AccessKey Secret', secKey(s.al)],
      ['aliyun_sign_name', '阿里云短信签名', orEmpty(s.al.sign)], ['aliyun_template_code', '阿里云模板编号', orEmpty(s.al.tpl)], ['aliyun_variable_name', '阿里云模板变量名', orEmpty(s.al.var)],
      ['tencent_secret_id', '腾讯云 SecretId', orEmpty(s.tc.sid)], ['tencent_secret_key', '腾讯云 SecretKey', secKey(s.tc)], ['tencent_sign_name', '腾讯云短信签名', orEmpty(s.tc.sign)],
      ['tencent_template_id', '腾讯云模板 ID', orEmpty(s.tc.tpl)], ['tencent_sdk_appid', '腾讯云 SDK AppID', orEmpty(s.tc.app)]];
  }
  function secDiff(o, n) {
    const A = secRows(o), B = secRows(n), out = [];
    A.forEach((a, i) => {
      const b = B[i];
      if (typeof a[2] === 'object') { if (b[2].neu) out.push({key: a[0], label: a[1], from: a[2].has ? '已配置' : '（未设置）', to: '已修改'}); return; }
      if (String(a[2]) !== String(b[2])) out.push({key: a[0], label: a[1], from: String(a[2]), to: String(b[2])});
    });
    return out;
  }
  /* 风险提示：降低安全强度的修改（不阻止保存，只让管理员看清楚）。条目与文案同真实 security.js 的 risksOf */
  function secRisks(o, n) {
    const w = [];
    if (n.pwdLen < o.pwdLen) w.push(`密码最小长度降低（${o.pwdLen} → ${n.pwdLen}）：已有账号不受影响，新建 / 改密按新规则。`);
    if (o.f2fa && !n.f2fa) w.push('关闭“强制两步验证”：未绑定两步验证的用户登录时不再被要求绑定。');
    if (!o.signup && n.signup) w.push('开放自助注册：任何能访问登录页的人都可以注册账号（按默认权限组入组）。');
    if (n.lock > o.lock || n.lockSec < o.lockSec) w.push('放宽账号锁定策略（次数变多或锁定时间变短）会降低暴力破解的成本。');
    if (o.jwtOtp && !n.jwtOtp) w.push('关闭“JWT 登录要求 OTP”：已绑定两步验证的用户通过 API 登录将不再校验动态口令。');
    if (o.wm && !n.wm) w.push('关闭页面水印：此后截图 / 拍屏将无法追溯到具体的人。');
    if (n.wm && n.wmc.opacity < 6) w.push(`水印透明度只有 ${n.wmc.opacity}%：截图后很可能看不清，起不到溯源作用。`);
    if (o.api.length && !n.api.length) w.push('清空 API 用户白名单：此后任何用户的 JWT 都能调用 API。');
    if (n.sessMin > o.sessMin && n.sessMin > 60 * 24) w.push(`会话有效期超过 24 小时（${n.sessMin} 分钟）：无人看管的浏览器保持登录的时间更长。`);
    return w;
  }
  /* 水印设置：从表单取值（strict=false 时取不到就用默认值，给实时预览用）与校验。只有超级用户进得了这个页面 */
  const wmForm = strict => {
    const n = (id, lo, hi, def, label) => { const v = parseInt(val(id), 10); if (v >= lo && v <= hi) return v; if (strict) throw new Error(`${label}须为 ${lo}–${hi} 之间的整数`); return def; };
    return {text: val('sc_wm_text'), extra: val('sc_wm_extra'), opacity: n('sc_wm_op', 3, 30, WM_DEF.opacity, '水印透明度（%）'), size: n('sc_wm_size', 12, 28, WM_DEF.size, '水印字号'),
      angle: n('sc_wm_angle', -60, 0, WM_DEF.angle, '水印倾斜角度'), gap: val('sc_wm_gap'), color: val('sc_wm_color')};
  };
  function wmCheck(c) {
    const len = s => [...s].length, all = c.text + c.extra, known = WM_VARS.map(v => v[0]);
    if (!c.text) throw new Error('水印内容不能为空');
    if (len(c.text) > 60) throw new Error('水印内容最长 60 个字符');
    if (len(c.extra) > 40) throw new Error('附加文字最长 40 个字符');
    const bad = (all.match(/\{[^{}]*\}/g) || []).find(v => !known.includes(v.slice(1, -1)));
    if (bad) throw new Error(`不认识的变量 ${bad}，可用：${known.map(k => '{' + k + '}').join(' ')}`);
    if (/[{}]/.test(all.replace(/\{[^{}]*\}/g, ''))) throw new Error('花括号不成对：变量要写成 {账号} 这样的形式');
    if (!/\{(账号|姓名)\}/.test(c.text)) throw new Error('水印内容必须包含 {账号} 或 {姓名}，否则截图无法追溯到具体的人');
    if (!WM_GAPS[c.gap] || !WM_COLORS[c.color]) throw new Error('水印疏密或颜色取值不合法');
  }
  /* 数字项的校验同真实 security.js：整数（正整数）且在 data-min ~ data-max 内，错误显示在输入框下方；水印的数字项与文本沿用下面 wmForm / wmCheck */
  const SEC_NUMS = [['sc_lock', 'lock'], ['sc_lockt', 'lockSec'], ['sc_pwdlen', 'pwdLen'], ['sc_sess', 'sessMin'], ['sc_jwt', 'jwt']];
  function secErr(id, text) { const e = $('#' + id + '_err'), inp = $('#' + id); if (!e) return; e.hidden = !text; e.textContent = text || ''; if (text) inp.setAttribute('aria-invalid', 'true'); else inp.removeAttribute('aria-invalid'); }
  function secMsg(text, kind = 'bad') { const b = $('#secMsg'); if (b) b.innerHTML = text ? notice(esc(text), kind === 'bad' ? 'bad' : '', kind === 'bad' ? '⚠' : 'ℹ') : ''; }
  function secNums() {
    const nums = {}; let first = null;
    SEC_NUMS.forEach(([id, k]) => {
      const el = $('#' + id), s = el.value.trim(), min = Number(el.dataset.min), max = Number(el.dataset.max), name = el.dataset.name; let err = '';
      if (!/^\d+$/.test(s)) err = `${name}必须是正整数`; else if (Number(s) < min || Number(s) > max) err = `${name}应在 ${min} 到 ${max} 之间`;
      secErr(id, err); if (err && !first) first = el; nums[k] = Number(s);
    });
    return {first, nums};
  }
  function secRead() {
    const v = secNums();
    if (v.first) { secMsg('有设置项不合法，请按提示修改后再保存。'); v.first.focus(); return null; }
    secMsg('');
    const wmc = wmForm(true); wmCheck(wmc);
    if (['sc_al_ak', 'sc_al_sign', 'sc_al_tpl', 'sc_al_var', 'sc_tc_sid', 'sc_tc_sign', 'sc_tc_tpl', 'sc_tc_app'].some(id => val(id).length > 200)) throw new Error('短信服务参数过长');
    const n = v.nums;
    return {lock: n.lock, lockSec: n.lockSec, f2fa: chk('sc_f2fa'), signup: chk('sc_signup'), pwdLen: n.pwdLen, pwdCx: chk('sc_pwdcx'),
      sessMin: n.sessMin, sessClose: chk('sc_sessclose'), jwt: n.jwt, jwtOtp: chk('sc_jwtotp'), api: checkedVals('.sc_api').map(Number).sort((a, b) => a - b), dg: checkedVals('.sc_dg'), dr: checkedVals('.sc_dr').map(Number).sort((a, b) => a - b),
      wm: chk('sc_wm'), wmc, smsP: val('sc_smsp'), al: {ak: val('sc_al_ak'), sign: val('sc_al_sign'), tpl: val('sc_al_tpl'), var: val('sc_al_var') || 'code', sk: SETTINGS.sms.al.sk, skNew: !!$('#sc_al_sk').value},
      tc: {sid: val('sc_tc_sid'), app: val('sc_tc_app'), sign: val('sc_tc_sign'), tpl: val('sc_tc_tpl'), sk: SETTINGS.sms.tc.sk, skNew: !!$('#sc_tc_sk').value}};
  }

  function secSave() {
    secMsg('');
    let nv; try { nv = secRead(); } catch (e) { return toast(e.message); }
    if (!nv) return;
    const old = secSnap(), ch = secDiff(old, nv);
    if (!ch.length) { secMsg('没有变更。', 'info'); return toast('没有变更'); }
    const warns = secRisks(old, nv);
    /* 统一确认框：变更差异 + 风险提示合并展示（同真实 confirmRun），确认后立即生效 */
    openModal({title: '确认保存登录与安全设置', code: 'sec.edit', okCls: 'danger primary', ok: warns.length ? '我已了解风险，确认保存' : '确认保存', wide: true,
      body: `<p style="margin:0 0 8px">共 ${ch.length} 项变更，保存后立即生效。</p>${warns.length ? `<div class="notice bad"><span>⚠</span><div><div>请确认以下风险：</div>${warns.map(w => `<div>· ${esc(w)}</div>`).join('')}</div></div>` : ''}${tbl(['项目', '原值', '新值'], ch, {cells: c => [esc(c.label), esc(c.from), esc(c.to)]})}`,
      onOk() {
        Object.assign(SETTINGS, {loginFailLock: nv.lock, lockSeconds: nv.lockSec, force2fa: nv.f2fa, selfRegister: nv.signup, pwdMinLen: nv.pwdLen, pwdComplexity: nv.pwdCx, sessionMinutes: nv.sessMin, sessionBrowserClose: nv.sessClose,
          jwtHours: nv.jwt, jwtRequireOtp: nv.jwtOtp, apiWhitelist: nv.api, defGroups: nv.dg, defRgs: nv.dr, watermark: nv.wm, wmCfg: nv.wmc, smsProvider: nv.smsP});
        SETTINGS.sms = {al: {ak: nv.al.ak, sign: nv.al.sign, tpl: nv.al.tpl, var: nv.al.var, sk: nv.al.sk || nv.al.skNew}, tc: {sid: nv.tc.sid, app: nv.tc.app, sign: nv.tc.sign, tpl: nv.tc.tpl, sk: nv.tc.sk || nv.tc.skNew}};
        clearTimeout(secWmTimer); applyWatermark();
        audit('sec.edit', `修改登录与安全设置：${ch.map(c => c.to === '已修改' ? `${c.key}: 已修改` : `${c.key}: ${c.from} → ${c.to}`).join('；')}`);
        toast(old.wm !== nv.wm ? `设置已保存；页面水印已${nv.wm ? '开启，现在即可在页面上看到' : '关闭'}` : nv.wm && ch.some(c => /^watermark_/.test(c.key)) ? '设置已保存；水印已按新的内容与样式生效' : '已保存，立即生效'); rerender();
      }});
  }
  ACTIONS['sec.edit'] = d => {
    if (d.mode === 'sms') {
      const say = (t, kind) => { const b = $('#secSmsMsg'); if (b) b.innerHTML = t ? notice(esc(t), kind === 'info' ? '' : 'bad', kind === 'info' ? 'ℹ' : '⚠') : ''; };
      const t = val('sc_smsphone');
      if (!t) { say('请填写测试手机号'); const i = $('#sc_smsphone'); return i && i.focus(); }
      if (!/^1\d{10}$/.test(t)) return say('手机号格式不正确');
      const saved = SETTINGS.smsProvider;
      if (saved === 'disabled') return say('短信服务商尚未保存：请先选择服务商、填写参数并点“保存配置”后再测试');
      audit('sec.edit', `发送测试短信（${saved === 'aliyun' ? '阿里云' : '腾讯云'}）到 ${t.slice(0, 3)}****${t.slice(-4)}`);
      return say(`测试短信已发送到 ${t}，请查收${saved !== val('sc_smsp') ? '（注意：服务商的修改还没保存，本次用的是已保存的配置）' : ''}`, 'info');
    }
    secSave();
  };
  ON.secSms = (d, el) => { const al = $('#secAl'), tc = $('#secTc'), bx = $('#secSmsBox'); if (al) al.hidden = el.value !== 'aliyun'; if (tc) tc.hidden = el.value !== 'tencent'; if (bx) bx.hidden = el.value === 'disabled'; };
  /* 全页预览：按表单里还没保存的内容临时显示 5 秒 */
  ON.secWm = () => {
    if (!can('sec.edit')) return toast('仅超级用户可以定义水印');
    let cfg; try { cfg = wmForm(true); wmCheck(cfg); } catch (e) { return toast(e.message); }
    clearTimeout(secWmTimer); applyWatermark(cfg); toast('已临时显示水印 5 秒（未保存）');
    secWmTimer = setTimeout(() => applyWatermark(), 5000);
  };
  /* 卡片里的实时效果预览：内容 / 样式一改就重画 */
  ON.secWmLive = () => {
    const el = $('#wmPrev'); if (!el) return;
    const c = wmForm(false), t = wmTile(c), o = $('#sc_wm_opv');
    if (o) o.textContent = c.opacity + '%';
    el.style.backgroundImage = t ? t.url : ''; el.style.backgroundSize = t ? `${t.W}px ${t.H}px` : '';
  };
  /* 点变量按钮：插到“水印内容”输入框的光标处 */
  ON.secWmVar = d => {
    const el = $('#sc_wm_text'); if (!el) return;
    const a = el.selectionStart == null ? el.value.length : el.selectionStart, b = el.selectionEnd == null ? a : el.selectionEnd, v = '{' + d.v + '}';
    el.value = el.value.slice(0, a) + v + el.value.slice(b); el.focus(); el.setSelectionRange(a + v.length, a + v.length); ON.secWmLive();
  };
  ON.secPrev2fa = () => openModal({title: '两步验证页预览', ok: '', cancel: '关闭', body: `<div style="max-width:360px;margin:0 auto">
    <h3 style="margin:0 0 4px">两步验证</h3>
    <div class="form" style="grid-template-columns:90px minmax(0,1fr);margin-top:12px">${fld('验证方式', '<select><option>验证器（TOTP）</option><option>短信验证码</option></select>')}${fld('验证码', '<input placeholder="6 位数字" maxlength="6" style="letter-spacing:.3em">')}</div>
    <div class="row" style="margin:10px 0"><button class="btn">获取验证码</button></div><button class="btn primary" style="width:100%">验 证</button>
    <div class="empty" style="padding:14px"><div class="ic">▦</div><div class="small">二维码 + 密钥</div></div></div>`});

  pages.sec = {
    init() { ON.secWmLive(); },
    render() {
      const S = SETTINGS, sm = S.sms, W = wmCfg();
      /* 数字项：type=text + inputmode=numeric（同真实），取值范围写在 data-min / data-max 上，保存时校验，错误显示在输入框下方 */
      const num = (id, v, lo, hi, name) => `<input type="text" inputmode="numeric" id="${id}" value="${v}" data-min="${lo}" data-max="${hi}" data-name="${name}" style="width:110px"><div class="hint" id="${id}_err" role="alert" style="color:var(--bad)" hidden></div>`;
      const wnum = (id, v, lo, hi) => `<input type="number" id="${id}" value="${v}" min="${lo}" max="${hi}" style="width:110px" data-oninput="secWmLive">`;
      const lockMin = Math.round(S.lockSeconds / 60 * 10) / 10, hm = m => (m % 60 === 0 ? `${m / 60}h` : `${m}m`);
      const actIds = USERS.filter(u => u.status === 'active').map(u => u.id), apiIds = S.apiWhitelist.map(Number);
      return CSS2 + `
      <div id="secMsg"></div>
      <div class="grid g4" style="margin-bottom:16px">
        <div class="card kpi"><div class="l">账号锁定策略</div><div class="v">${S.loginFailLock} 次 / ${lockMin} 分钟</div></div>
        <div class="card kpi"><div class="l">两步验证</div><div class="v">${S.force2fa ? '强制' : '自愿'}</div><div class="d ${S.force2fa ? 'down' : 'flat'}">${USERS.filter(u => u.twofa).length} / ${USERS.length} 个账号已绑定</div></div>
        <div class="card kpi"><div class="l">密码策略</div><div class="v">≥ ${S.pwdMinLen} 位</div><div class="d flat">${S.pwdComplexity ? '需含字母 / 数字 / 特殊字符' : '不要求复杂度'}</div></div>
        <div class="card kpi"><div class="l">会话 / JWT</div><div class="v">${hm(S.sessionMinutes)} / ${S.jwtHours}h</div><div class="d flat">${S.selfRegister ? '自助注册：开放' : '自助注册：关闭'}</div></div></div>
      <div class="toolbar"><button class="btn" data-on="secPrev2fa">预览两步验证页</button><a class="btn" href="#authp">去登录方式</a><span class="sp"></span>${B('sec.edit', '保存配置', {cls: 'primary'})}</div>
      <div class="grid g2">
        <div class="card"><h2>账号安全</h2><div class="form">
          ${fld('登录失败几次锁账号', num('sc_lock', S.loginFailLock, 1, 100, '登录失败锁定次数'))}
          ${fld('锁定时间（秒）', num('sc_lockt', S.lockSeconds, 10, 86400, '锁定时间'))}
          ${fld('强制两步验证', sw('sc_f2fa', S.force2fa, '开启'))}
          ${fld('开放用户自助注册', sw('sc_signup', S.selfRegister, '开启'))}
          ${fld('密码最小长度', num('sc_pwdlen', S.pwdMinLen, 6, 64, '密码最小长度'))}
          ${fld('密码复杂度', sw('sc_pwdcx', S.pwdComplexity, '需含字母、数字、特殊字符'))}</div></div>
        <div class="card"><h2>会话与 API</h2><div class="form">
          ${fld('会话有效期（分钟）', num('sc_sess', S.sessionMinutes, 5, 43200, '会话有效期'))}
          ${fld('关闭浏览器即退出', sw('sc_sessclose', S.sessionBrowserClose, '开启'))}
          ${fld('JWT access 有效期（小时）', num('sc_jwt', S.jwtHours, 1, 720, 'JWT 有效期'))}
          ${fld('JWT 登录要求 OTP', sw('sc_jwtotp', S.jwtRequireOtp, '开启'))}
          ${fld('API 用户白名单（空 = 不限制）', `<div class="p2-ev">${USERS.filter(u => u.status === 'active').map(u => `<label class="chk"><input type="checkbox" class="sc_api" value="${u.id}" ${apiIds.includes(u.id) ? 'checked' : ''}>${esc(u.login)}</label>`).join('')}${apiIds.filter(i => !actIds.includes(i)).map(i => `<label class="chk"><input type="checkbox" class="sc_api" value="${i}" checked>用户 #${i}（不在列表中）</label>`).join('')}</div>`)}
          ${fld('新用户默认权限组',`<div class="p2-ev">${GROUPS.map(g => `<label class="chk"><input type="checkbox" class="sc_dg" value="${g.id}" ${S.defGroups.includes(g.id) ? 'checked' : ''}>${esc(g.name)}</label>`).join('')}</div>`)}
          ${fld('新用户默认资源组', `<div class="p2-ev">${RGROUPS.filter(r => r.enabled).map(g => `<label class="chk"><input type="checkbox" class="sc_dr" value="${g.id}" ${S.defRgs.includes(g.id) ? 'checked' : ''}>${esc(g.name)}</label>`).join('')}</div>`)}</div></div>
        <div class="card"><h2>页面水印</h2><div class="form">
          ${fld('启用水印', sw('sc_wm', S.watermark, '启用'))}
          ${fld('水印内容', `<input class="mono" id="sc_wm_text" maxlength="60" value="${esc(W.text)}" data-oninput="secWmLive"><div class="wm-vars">${WM_VARS.map(v => `<a class="tag info" data-on="secWmVar" data-v="${v[0]}" title="${esc(v[1])}">{${v[0]}}</a>`).join('')}</div>`, {req: true})}
          ${fld('附加文字（第二行，可空）', `<input id="sc_wm_extra" maxlength="40" value="${esc(W.extra)}" data-oninput="secWmLive" placeholder="如：内部资料 严禁外传">`)}
          ${fld('透明度', `<div class="row"><input type="range" id="sc_wm_op" min="3" max="30" value="${W.opacity}" data-oninput="secWmLive" style="flex:1"><b id="sc_wm_opv" style="width:42px;text-align:right">${W.opacity}%</b></div>`)}
          ${fld('字号（px）', wnum('sc_wm_size', W.size, 12, 28))}
          ${fld('倾斜角度（°）', wnum('sc_wm_angle', W.angle, -60, 0))}
          ${fld('疏密', `<select id="sc_wm_gap" data-onchange="secWmLive">${Object.keys(WM_GAPS).map(k => opt(k, WM_GAPS[k][0], W.gap)).join('')}</select>`)}
          ${fld('颜色', `<select id="sc_wm_color" data-onchange="secWmLive">${Object.keys(WM_COLORS).map(k => opt(k, WM_COLORS[k], W.color)).join('')}</select>`)}
          ${fld('效果预览', '<div class="wm-prev" id="wmPrev"></div>')}
          ${fld('', '<button class="btn" data-on="secWm">👁 全页预览 5 秒（未保存）</button>')}</div></div>
        <div class="card"><h2>两步验证短信服务</h2><div class="form">
          ${fld('服务商', `<select id="sc_smsp" data-onchange="secSms">${opt('disabled', '关闭（仅 TOTP）', S.smsProvider)}${opt('aliyun', '阿里云', S.smsProvider)}${opt('tencent', '腾讯云', S.smsProvider)}</select>`)}</div>
          <div id="secAl" style="margin-top:10px" ${S.smsProvider === 'aliyun' ? '' : 'hidden'}><div class="form">${fld('AccessKey ID', `<input class="mono" id="sc_al_ak" value="${esc(sm.al.ak)}" placeholder="LTAI…" autocomplete="off">`)}${fld('AccessKey Secret', `<input type="password" id="sc_al_sk" placeholder="${sm.al.sk ? '已配置（留空不修改）' : ''}" autocomplete="new-password">`)}${fld('短信签名', `<input id="sc_al_sign" value="${esc(sm.al.sign)}" autocomplete="off">`)}${fld('模板编号', `<input class="mono" id="sc_al_tpl" value="${esc(sm.al.tpl)}" placeholder="SMS_123456789" autocomplete="off">`)}${fld('模板变量名', `<input class="mono" id="sc_al_var" value="${esc(sm.al.var)}" placeholder="code" autocomplete="off">`)}</div></div>
          <div id="secTc" style="margin-top:10px" ${S.smsProvider === 'tencent' ? '' : 'hidden'}><div class="form">${fld('SecretId', `<input class="mono" id="sc_tc_sid" value="${esc(sm.tc.sid)}" autocomplete="off">`)}${fld('SecretKey', `<input type="password" id="sc_tc_sk" placeholder="${sm.tc.sk ? '已配置（留空不修改）' : ''}" autocomplete="new-password">`)}${fld('短信签名', `<input id="sc_tc_sign" value="${esc(sm.tc.sign)}" autocomplete="off">`)}${fld('模板 ID', `<input class="mono" id="sc_tc_tpl" value="${esc(sm.tc.tpl)}" autocomplete="off">`)}${fld('SDK AppID', `<input class="mono" id="sc_tc_app" value="${esc(sm.tc.app)}" autocomplete="off">`)}</div></div>
          <div id="secSmsBox" style="margin-top:10px" ${S.smsProvider === 'disabled' ? 'hidden' : ''}>
            <div class="row"><label class="small muted" for="sc_smsphone">测试手机号</label><input id="sc_smsphone" inputmode="tel" placeholder="13800000000" autocomplete="off" style="width:150px">${B('sec.edit', '发送测试短信（用已保存的配置）', {cls: 'sm', data: {mode: 'sms'}})}</div>
            <div id="secSmsMsg" style="margin-top:6px"></div></div></div></div>
      <div class="card" style="margin-top:16px"><h2>已定义的登录方式<span class="right"><a class="btn sm" href="#authp">去配置登录方式</a></span></h2>
        ${tbl(['名称', '类型', '状态'], SSO_PROVIDERS, {cells: p => [esc(p.name), `<span class="tag">${esc(p.type)}</span>`, p.on ? '<span class="tag ok">启用</span>' : '<span class="tag">停用</span>']})}</div>`;
    }
  };

  /* ============================================================================
     goInception 配置 goinception
     连接信息（即时生效）+ config.toml 在线编辑（参数表单 ⇄ 源文件双向同步、语法 / 取值校验）
     + 版本历史 / 对比 / 回滚 + 保存并重启（goInception 以 Docker 部署，重启方式固定为 docker restart <容器>；演示健康检查失败自动恢复上一版本）
     ========================================================================== */
  const GI_PARAMS = [
    {sec: 'inc', key: 'check_primary_key', t: 'bool', g: '审核规则 [inc]', l: '表必须有主键'},
    {sec: 'inc', key: 'check_table_comment', t: 'bool', g: '审核规则 [inc]', l: '表必须有注释'},
    {sec: 'inc', key: 'check_column_comment', t: 'bool', g: '审核规则 [inc]', l: '列必须有注释'},
    {sec: 'inc', key: 'check_insert_field', t: 'bool', g: '审核规则 [inc]', l: 'INSERT 必须指定列'},
    {sec: 'inc', key: 'check_dml_where', t: 'bool', g: '审核规则 [inc]', l: 'UPDATE / DELETE 必须有 WHERE', risk: v => v === false ? '关闭 check_dml_where 后，UPDATE / DELETE 可以不带 WHERE，存在误改误删全表的风险' : ''},
    {sec: 'inc', key: 'check_dml_limit', t: 'bool', g: '审核规则 [inc]', l: 'DML 必须带 LIMIT'},
    {sec: 'inc', key: 'enable_drop_table', t: 'bool', g: '审核规则 [inc]', l: '允许 DROP TABLE', risk: v => v === true ? 'enable_drop_table = true：线上误删表将无法被审核拦截' : ''},
    {sec: 'inc', key: 'enable_drop_database', t: 'bool', g: '审核规则 [inc]', l: '允许 DROP DATABASE', risk: v => v === true ? 'enable_drop_database = true：线上误删库将无法被审核拦截' : ''},
    {sec: 'inc', key: 'lang', t: 'enum', o: ['zh-CN', 'en-US'], g: '阈值与语言 [inc]', l: '审核提示语言'},
    {sec: 'inc', key: 'max_update_rows', t: 'int', min: 1, max: 100000000, g: '阈值与语言 [inc]', l: '单条 DML 最大影响行数', risk: v => v > 1000000 ? `max_update_rows = ${v} 超过 100 万行：大事务会拖慢主从复制` : ''},
    {sec: 'inc', key: 'max_keys', t: 'int', min: 1, max: 64, g: '阈值与语言 [inc]', l: '单表最大索引数'},
    {sec: 'inc', key: 'backup_host', t: 'str', g: '备份与回滚 [inc]', l: '备份库 host'},
    {sec: 'inc', key: 'backup_port', t: 'int', min: 1, max: 65535, g: '备份与回滚 [inc]', l: '备份库 port'},
    {sec: 'inc', key: 'backup_user', t: 'str', g: '备份与回滚 [inc]', l: '备份库 user'},
    {sec: 'inc', key: 'backup_password', t: 'secret', g: '备份与回滚 [inc]', l: '备份库 password（加密显示，写入专用）'},
    {sec: 'osc', key: 'osc_on', t: 'bool', g: '在线改表 [osc] / [ghost]', l: '启用 pt-osc'},
    {sec: 'osc', key: 'osc_min_table_size', t: 'int', min: 0, max: 1048576, g: '在线改表 [osc] / [ghost]', l: '超过多大的表才走 osc（MB）'},
    {sec: 'osc', key: 'osc_bin_dir', t: 'str', g: '在线改表 [osc] / [ghost]', l: 'pt-osc 可执行文件目录'},
    {sec: 'ghost', key: 'ghost_on', t: 'bool', g: '在线改表 [osc] / [ghost]', l: '启用 gh-ost'},
    {sec: 'log', key: 'level', t: 'enum', o: ['debug', 'info', 'warn', 'error'], g: '日志 [log]', l: '日志级别'},
  ];
  const GI_DEF = {check_primary_key: true, check_table_comment: true, check_column_comment: true, check_insert_field: true, check_dml_where: true, check_dml_limit: false, enable_drop_table: false, enable_drop_database: false,
    lang: 'zh-CN', max_update_rows: 10000, max_keys: 16, backup_host: '10.20.9.31', backup_port: 3306, backup_user: 'inc_backup', backup_password: '********', osc_on: true, osc_min_table_size: 16, osc_bin_dir: '/usr/local/bin', ghost_on: false, level: 'info'};
  const giLit = v => typeof v === 'string' ? JSON.stringify(v) : String(v);
  function giMake(ov) {
    const v = Object.assign({}, GI_DEF, ov), L = k => giLit(v[k]);
    return `# goInception 配置文件（由 dbtool「goInception 配置」维护）
# 直接在服务器上改动本文件会被识别为“磁盘文件与生效版本不一致”，可导入为新版本

[inc]
lang = ${L('lang')}

# --- 审核规则 ---
check_primary_key = ${L('check_primary_key')}
check_table_comment = ${L('check_table_comment')}
check_column_comment = ${L('check_column_comment')}
check_insert_field = ${L('check_insert_field')}
check_dml_where = ${L('check_dml_where')}   # 关闭有误删全表风险
check_dml_limit = ${L('check_dml_limit')}
enable_drop_table = ${L('enable_drop_table')}
enable_drop_database = ${L('enable_drop_database')}
max_update_rows = ${L('max_update_rows')}
max_keys = ${L('max_keys')}

# --- 备份库（回滚语句写到这里，需与“连接信息”里的备份库一致）---
backup_host = ${L('backup_host')}
backup_port = ${L('backup_port')}
backup_user = ${L('backup_user')}
backup_password = ${L('backup_password')}   # 密码行显示脱敏，保存时未改动则保持原值

[osc]
osc_on = ${L('osc_on')}
osc_min_table_size = ${L('osc_min_table_size')}
osc_bin_dir = ${L('osc_bin_dir')}

[ghost]
ghost_on = ${L('ghost_on')}

[log]
level = ${L('level')}
`;
  }
  const GI_STEPS = [
    [6, '2026-08-21 15:40', '导入磁盘上已有的配置文件（初始版本）', {check_table_comment: false, backup_host: '10.20.9.21', osc_on: false, max_update_rows: 5000, check_dml_where: false}, null],
    [7, '2026-09-02 10:12', 'check_table_comment: false → true', {check_table_comment: true}, 6],
    [8, '2026-09-05 17:08', 'backup_host: 10.20.9.21 → 10.20.9.31', {backup_host: '10.20.9.31'}, 7],
    [9, '2026-09-12 09:30', 'osc_on: false → true', {osc_on: true}, 8],
    [10, '2026-09-25 16:42', 'osc_bin_dir: /usr/local/bin → /opt/percona/bin', {osc_bin_dir: '/opt/percona/bin'}, 9],
    [11, '2026-09-28 10:05', 'max_update_rows: 5000 → 10000', {max_update_rows: 10000}, 9],
    [12, '2026-10-02 18:20', 'check_dml_where: false → true', {check_dml_where: true}, 11],
  ];
  const GI = {tab: 'conn', view: 'both', pwdNew: false, flow: null, log: [], versions: [],
    conn: {host: '10.20.9.30', port: 4000, user: 'inception', bHost: '10.20.9.31', bPort: 3306, bUser: 'inc_backup'},
    deploy: {path: '/etc/goinception/config.toml', container: 'goinception', healthSec: 60, waitMax: 600, autoRollback: true, waitDefault: true},
    proc: {state: 'running', ver: 'v1.2.7', conns: 5, started: '2026-09-24 03:10', tasks: [{id: 1040, st: '执行中', name: '支付流水表归档历史分区'}, {id: 1041, st: '检查中', name: '用户表增加索引 idx_level_created'}]},
    lastRestart: {at: '2026-10-02 18:21', ok: true, ms: 3200, op: 'admin'}};
  (() => {
    const acc = {};
    GI_STEPS.forEach(([v, at, summary, delta, from]) => {
      acc[v] = Object.assign({}, from ? acc[from] : {}, delta);
      GI.versions.push({v, at, op: 'admin', summary, status: v === 10 ? 'rolledback' : 'applied', content: giMake(acc[v]), reason: '', ms: v === 6 ? 0 : 2900 + v * 20, restoredTo: v === 10 ? 9 : 0});
    });
    GI.versions.find(x => x.v === 10).reason = '重启后健康检查失败（60 秒内 goInception 一直连不上）。启动日志：osc_bin_dir "/opt/percona/bin" 不存在，进程退出（exit status 1）。已自动恢复到 #9 并再次重启，健康检查通过。';
  })();
  const giSorted = () => GI.versions.slice().sort((a, b) => b.v - a.v);
  const giCur = () => GI.versions.filter(x => x.status === 'applied').reduce((a, b) => (b.v > a.v ? b : a));
  const giPending = () => { const c = giCur(); return GI.versions.filter(x => x.status === 'draft' && x.v > c.v).reduce((a, b) => (!a || b.v > a.v ? b : a), null); };
  const giDisk = () => { const p = giPending(); return (p || giCur()).content; };
  const giNextV = () => Math.max(...GI.versions.map(x => x.v)) + 1;
  GI.text = giCur().content;
  /* goInception 以 Docker 部署：重启方式只有一种，用户点“重启”就是 docker restart 该容器（容器名由管理员在「部署与重启」里设定） */
  const giCmd = D => `sudo -n docker restart ${D.container}`;
  const giModeName = () => 'Docker（docker restart）';
  const GI_CONTAINER_RE = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/;
  function giLabel(x) {
    const cur = giCur(), pend = giPending();
    if (x.status === 'applied') return x.v === cur.v ? ['ok', '当前生效'] : ['', '已被替换'];
    if (x.status === 'draft') return pend && pend.v === x.v ? ['warn', '待重启生效'] : ['', '已被替换'];
    if (x.status === 'rolledback') return ['bad', `重启失败，已自动回滚到 #${x.restoredTo}`];
    return ['bad', '失败'];
  }

  /* ---- TOML 子集：解析 / 校验 / 行级修改 ---- */
  function giValue(s) {
    s = s.trim(); if (!s) return {err: '缺少值'};
    const q = s[0];
    if (q === '"' || q === "'") {
      let i = 1, out = '';
      while (i < s.length) { const c = s[i]; if (q === '"' && c === '\\') { out += s[i + 1] || ''; i += 2; continue; } if (c === q) break; out += c; i++; }
      if (i >= s.length) return {err: '字符串缺少结束引号'};
      const rest = s.slice(i + 1); if (rest.trim() && rest.trim()[0] !== '#') return {err: '字符串后面有多余内容'};
      return {type: 'str', val: out, raw: s.slice(0, i + 1), tail: rest};
    }
    if (q === '[') { const j = s.indexOf(']'); if (j < 0) return {err: '数组缺少结束的 ]'}; return {type: 'arr', val: s.slice(0, j + 1), raw: s.slice(0, j + 1), tail: s.slice(j + 1)}; }
    const k = s.indexOf('#'), tok = (k < 0 ? s : s.slice(0, k)).trim(), tail = k < 0 ? '' : s.slice(tok.length);
    if (tok === 'true' || tok === 'false') return {type: 'bool', val: tok === 'true', raw: tok, tail};
    if (/^[+-]?\d[\d_]*$/.test(tok)) return {type: 'int', val: parseInt(tok.replace(/_/g, ''), 10), raw: tok, tail};
    if (/^[+-]?\d+\.\d+$/.test(tok)) return {type: 'float', val: parseFloat(tok), raw: tok, tail};
    return {err: `“${tok}” 不是合法的 TOML 值（字符串需要加引号）`};
  }
  function giAnalyze(text) {
    const map = {}, errors = [], warnings = [], secs = new Set(), known = new Set(GI_PARAMS.map(p => p.sec));
    let sec = '';
    if (text.length > 1048576) errors.push({n: 1, msg: '文件超过 1 MB 上限'});
    text.split('\n').forEach((ln, i) => {
      const n = i + 1, s = ln.trim(); if (!s || s[0] === '#') return;
      let m = s.match(/^\[([A-Za-z0-9_.-]+)\]\s*(#.*)?$/);
      if (m) { sec = m[1]; if (secs.has(sec)) errors.push({n, msg: `段 [${sec}] 重复定义`}); secs.add(sec); return; }
      if (/^\[/.test(s)) { errors.push({n, msg: '段名格式不正确（应为 [name]）'}); return; }
      m = s.match(/^([A-Za-z0-9_.-]+)\s*=\s*(.*)$/);
      if (!m) { errors.push({n, msg: '语法错误：既不是 [段] 也不是 key = value'}); return; }
      const v = giValue(m[2]); if (v.err) { errors.push({n, msg: `${m[1]}：${v.err}`}); return; }
      const id = sec + '.' + m[1];
      if (map[id]) errors.push({n, msg: `${m[1]} 在 [${sec}] 中重复`}); else map[id] = Object.assign({n, sec, key: m[1]}, v);
    });
    Object.values(map).forEach(e => {
      const p = GI_PARAMS.find(x => x.sec === e.sec && x.key === e.key);
      if (!p) { warnings.push({n: e.n, msg: known.has(e.sec) ? `未知参数 ${e.key}（目录里没有，goInception 可能不识别，重启后才知道）` : `未知段 [${e.sec}] 下的参数 ${e.key}`}); return; }
      let err = '';
      if (p.t === 'bool' && e.type !== 'bool') err = `${e.key} 应为布尔值 true / false，当前是 ${e.raw}`;
      else if (p.t === 'int') err = e.type !== 'int' ? `${e.key} 应为整数，当前是 ${e.raw}` : (e.val < p.min || e.val > p.max ? `${e.key} 超出取值范围 ${p.min} – ${p.max}（当前 ${e.val}）` : '');
      else if (p.t === 'enum' && !(e.type === 'str' && p.o.includes(e.val))) err = `${e.key} 应为 ${p.o.map(o => '"' + o + '"').join(' / ')} 之一`;
      else if ((p.t === 'str' || p.t === 'secret') && e.type !== 'str') err = `${e.key} 应为字符串（需要加双引号）`;
      if (err) errors.push({n: e.n, msg: err});
      else if (p.risk) { const w = p.risk(e.val); if (w) warnings.push({n: e.n, msg: w}); }
    });
    errors.sort((a, b) => a.n - b.n); warnings.sort((a, b) => a.n - b.n);
    return {map, errors, warnings};
  }
  function giPatch(text, sec, key, lit) {
    const lines = text.split('\n'); let cur = '', secStart = -1, last = -1, found = -1;
    for (let i = 0; i < lines.length; i++) {
      const s = lines[i].trim(); if (!s || s[0] === '#') continue;
      const m = s.match(/^\[([A-Za-z0-9_.-]+)\]/);
      if (m) { cur = m[1]; if (cur === sec && secStart < 0) secStart = i; continue; }
      if (cur !== sec) continue;
      last = i;
      if (new RegExp('^' + key + '\\s*=').test(s)) { found = i; break; }
    }
    if (found >= 0) {
      const m = lines[found].match(/^(\s*[^=]+=\s*)(.*)$/), v = giValue(m[2]);
      lines[found] = m[1] + lit + (v.err ? '' : v.tail || '');
    } else if (secStart >= 0) lines.splice((last >= 0 ? last : secStart) + 1, 0, `${key} = ${lit}`);
    else return text.replace(/\n*$/, '\n') + `\n[${sec}]\n${key} = ${lit}\n`;
    return lines.join('\n');
  }
  /* 行级 LCS 对比（带上下文，折叠未改动区段）；渲染样式同 core 的 diffHtml */
  function giDiffCtx(a, b, ctx = 2) {
    const A = String(a).split('\n'), Bn = String(b).split('\n'), n = A.length, m = Bn.length;
    if (n * m > 4e6) return diffHtml(a, b);
    const L = Array.from({length: n + 1}, () => new Array(m + 1).fill(0));
    for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--) L[i][j] = A[i] === Bn[j] ? L[i + 1][j + 1] + 1 : Math.max(L[i + 1][j], L[i][j + 1]);
    const ops = []; let i = 0, j = 0;
    while (i < n && j < m) { if (A[i] === Bn[j]) { ops.push([' ', A[i]]); i++; j++; } else if (L[i + 1][j] >= L[i][j + 1]) ops.push(['-', A[i++]]); else ops.push(['+', Bn[j++]]); }
    while (i < n) ops.push(['-', A[i++]]); while (j < m) ops.push(['+', Bn[j++]]);
    if (!ops.some(o => o[0] !== ' ')) return '<div class="small muted">两个版本的内容完全相同</div>';
    const near = ops.map((o, k) => { for (let t = Math.max(0, k - ctx); t <= Math.min(ops.length - 1, k + ctx); t++) if (ops[t][0] !== ' ') return true; return false; });
    let h = '', skip = 0;
    const flush = () => { if (skip) { h += `<span style="color:var(--muted)">  ⋯ 省略 ${skip} 行未改动</span>`; skip = 0; } };
    ops.forEach((o, k) => { if (!near[k]) { skip++; return; } flush(); h += o[0] === '-' ? `<span class="del">- ${esc(o[1])}</span>` : o[0] === '+' ? `<span class="add">+ ${esc(o[1])}</span>` : `<span>  ${esc(o[1])}</span>`; });
    flush();
    return `<pre class="diff">${h}</pre>`;
  }
  /* 变更摘要：只写“key: 旧 → 新”，密码只写“已修改” */
  function giSummary(a, b) {
    const A = giAnalyze(a).map, Bm = giAnalyze(b).map, out = [];
    Object.keys(Bm).forEach(id => {
      const x = A[id], y = Bm[id];
      if (!x) out.push(`${y.key}: 新增 ${/password/.test(y.key) ? '（已设置）' : y.raw}`);
      else if (x.raw !== y.raw) out.push(/password|passwd|secret|token/.test(y.key) ? `${y.key}: 已修改` : `${y.key}: ${x.raw.replace(/^"|"$/g, '')} → ${y.raw.replace(/^"|"$/g, '')}`);
    });
    Object.keys(A).forEach(id => { if (!Bm[id]) out.push(`${A[id].key}: 已删除`); });
    if (GI.pwdNew) out.push('backup_password: 已修改');
    return out.length ? (out.length > 3 ? out.slice(0, 3).join('；') + `；等 ${out.length} 项` : out.join('；')) : '仅调整注释 / 排版';
  }
  const giEffective = t => GI.pwdNew ? t.replace(/(backup_password\s*=\s*)"\*{8}"/, '$1"********（已修改）"') : t;

  /* ---- 文件页：表单 ⇄ 源文件同步 ---- */
  function giPanelHtml() {
    const a = giAnalyze(GI.text); let h = '';
    h += a.errors.length ? `<div class="notice bad"><span>✖</span><div><b>${a.errors.length} 个错误（修正后才能保存）</b>${a.errors.slice(0, 6).map(e => `<div class="small"><a data-on="giGoto" data-n="${e.n}">第 ${e.n} 行</a>：${esc(e.msg)}</div>`).join('')}${a.errors.length > 6 ? `<div class="small muted">… 另有 ${a.errors.length - 6} 个</div>` : ''}</div></div>`
      : `<div class="notice ok"><span>✔</span><div>语法正确　✔ 参数类型与取值范围合法　${a.warnings.length ? `⚠ ${a.warnings.length} 个警告` : '✔ 没有风险取值'}</div></div>`;
    if (a.warnings.length) h += `<div class="notice warn"><span>⚠</span><div><b>${a.warnings.length} 个警告（可以保存，但保存时需要确认）</b>${a.warnings.map(e => `<div class="small"><a data-on="giGoto" data-n="${e.n}">第 ${e.n} 行</a>：${esc(e.msg)}</div>`).join('')}</div></div>`;
    return h;
  }
  function giDiffHtml() {
    const cur = giCur(), pend = giPending(), changed = GI.text !== giDisk() || GI.pwdNew;
    if (GI.text === cur.content && !GI.pwdNew) return `<div class="small muted">与当前生效的 #${cur.v} 完全一致，没有待保存的变更。</div>`;
    return `<h4 style="margin:6px 0">变更对比 <small class="muted">当前生效 #${cur.v} → ${changed ? '待保存' : `待重启生效的 #${pend.v}`}</small></h4>${giDiffCtx(cur.content, giEffective(GI.text))}<div class="small muted">${esc(giSummary(cur.content, GI.text))}</div>`;
  }
  function giRefresh(fromText) {
    const p = $('#giPanel'), d = $('#giDiff'), dirty = $('#giDirty');
    if (p) p.innerHTML = giPanelHtml(); if (d) d.innerHTML = giDiffHtml();
    if (dirty) dirty.innerHTML = GI.text !== giDisk() || GI.pwdNew ? '<span class="tag warn">● 有未保存的修改</span>' : '';
    if (!fromText) return;
    const a = giAnalyze(GI.text);
    GI_PARAMS.forEach(p => {
      const el = $(`#gp_${p.sec}_${p.key}`); if (!el || el === document.activeElement || p.t === 'secret') return;
      const e = a.map[p.sec + '.' + p.key];
      if (p.t === 'bool') { el.indeterminate = !e || e.type !== 'bool'; el.checked = !!(e && e.val === true); } else el.value = e ? (e.type === 'str' ? e.val : String(e.raw)) : '';
    });
  }
  ON.giText = (d, el) => { GI.text = el.value; giRefresh(true); };
  ON.giParam = (d, el) => {
    const p = GI_PARAMS.find(x => x.sec === d.s && x.key === d.k); if (!p) return;
    let lit;
    if (p.t === 'bool') lit = el.checked ? 'true' : 'false';
    else if (p.t === 'int') { if (!/^\d+$/.test(el.value.trim())) { toast('请输入整数'); giRefresh(true); return; } lit = String(parseInt(el.value, 10)); }
    else { if (!el.value.trim()) { toast('值不能为空；要删除参数请直接在源文件里编辑'); giRefresh(true); return; } lit = giLit(el.value.trim()); }
    GI.text = giPatch(GI.text, p.sec, p.key, lit); const ta = $('#giText'); if (ta) ta.value = GI.text; giRefresh(false);
  };
  ON.giPwd = (d, el) => { if (!el.value) return; GI.pwdNew = true; el.value = ''; el.placeholder = '新密码已记录，保存时生效'; giRefresh(false); toast('新备份库密码已记录，保存时生效'); };
  ON.giGoto = d => {
    const ta = $('#giText'); if (!ta) return; const ls = ta.value.split('\n'), n = +d.n; let s = 0; for (let i = 0; i < n - 1; i++) s += ls[i].length + 1;
    ta.focus(); ta.setSelectionRange(s, s + (ls[n - 1] || '').length); ta.scrollTop = Math.max(0, (n - 4) * (parseFloat(getComputedStyle(ta).lineHeight) || 21));
  };
  ON.giCheck = () => { const a = giAnalyze(GI.text); giRefresh(false); toast(a.errors.length ? `校验未通过：${a.errors.length} 个错误` : a.warnings.length ? `校验通过，有 ${a.warnings.length} 个警告` : '校验通过：语法、参数名、取值范围均合法'); };
  ON.giRevert = () => { GI.text = giDisk(); GI.pwdNew = false; toast('已恢复为' + (giPending() ? '待重启生效的' : '当前生效的') + '版本'); rerender(); };
  ON.giView = (d, el) => { GI.view = el.value; rerender(); };
  function giFileTab() {
    const cur = giCur(), pend = giPending(), D = GI.deploy;
    const groups = []; GI_PARAMS.forEach(p => { let g = groups.find(x => x[0] === p.g); if (!g) groups.push(g = [p.g, []]); g[1].push(p); });
    const ctl = p => {
      const id = `gp_${p.sec}_${p.key}`, at = `data-onchange="giParam" data-s="${p.sec}" data-k="${p.key}"`;
      if (p.t === 'bool') return `<input type="checkbox" id="${id}" ${at}>`;
      if (p.t === 'enum') return `<select id="${id}" ${at}>${p.o.map(o => `<option>${o}</option>`).join('')}</select>`;
      if (p.t === 'secret') return `<input type="password" id="${id}" data-onchange="giPwd" placeholder="已设置（输入即修改）" autocomplete="new-password">`;
      return `<input type="text" class="mono" id="${id}" ${at} ${p.t === 'int' ? `title="${p.min} – ${p.max}"` : ''}>`;
    };
    const form = GI.view === 'src' ? '' : `<div class="card"><h2>常用参数</h2>${groups.map(([g, ps], i) => `<details class="p2-acc" ${i < 2 ? 'open' : ''}><summary>${esc(g)}</summary>${ps.map(p => `<div class="p2-pf"><div class="nm">${esc(p.l)}<div class="mono">${p.key}${p.t === 'int' ? `（${p.min}–${p.max}）` : ''}</div></div><div>${ctl(p)}</div></div>`).join('')}</details>`).join('')}
      </div>`;
    return `<div class="toolbar"><span class="mono small">${esc(D.path)}</span><span class="tag ok">当前生效 #${cur.v}</span>${pend ? `<span class="tag warn">待重启生效 #${pend.v}</span>` : ''}<span class="muted small">最后修改 ${esc((pend || cur).at)} · ${esc((pend || cur).op)}</span><span id="giDirty"></span><span class="sp"></span>
      <label class="muted small">视图 <select data-onchange="giView">${opt('both', '结构化表单 + 源文件', GI.view)}${opt('src', '只看源文件', GI.view)}</select></label></div>
    <div class="${GI.view === 'src' ? '' : 'p2-cols'}">${form}
      <div class="card"><h2>源文件 config.toml</h2>
        <textarea id="giText" class="sql-ed" spellcheck="false" wrap="off" data-oninput="giText" style="min-height:430px">${esc(GI.text)}</textarea>
        <div id="giPanel" style="margin-top:10px">${giPanelHtml()}</div><div id="giDiff">${giDiffHtml()}</div>
        <div class="row" style="margin-top:12px"><button class="btn" data-on="giCheck">校验</button><button class="btn" data-on="giRevert">恢复为${pend ? '待生效' : '当前生效'}版本</button><span class="sp"></span>${B('goinception.file', '仅保存（不重启）')}${B('goinception.restart', '保存并重启 goInception', {cls: 'primary', data: {mode: 'save'}})}</div></div></div>`;
  }

  /* ---- 保存 / 写版本 ---- */
  function giGate() {
    const a = giAnalyze(GI.text);
    if (a.errors.length) { toast(`配置有 ${a.errors.length} 个错误，请先修正（第 ${a.errors[0].n} 行：${a.errors[0].msg}）`); return null; }
    if (GI.text === giDisk() && !GI.pwdNew) { toast('内容与' + (giPending() ? '待生效版本' : '当前生效版本') + '相同，无需保存'); return null; }
    return a;
  }
  const giWarnHtml = a => a.warnings.length ? `<div style="margin:6px 0"><b>风险取值（需确认）：</b>${a.warnings.map(w => `<div class="small">第 ${w.n} 行：${esc(w.msg)}</div>`).join('')}</div>` : '';
  function giWrite(summary, code, content) {
    const text = content != null ? content : GI.text, v = {v: giNextV(), at: now(), op: ME.login, summary, status: 'draft', content: text, reason: '', ms: 0, restoredTo: 0};
    GI.versions.push(v); GI.text = text; GI.pwdNew = false;
    audit(code, `保存 goInception 配置 #${v.v}：${summary}`); return v;
  }
  ACTIONS['goinception.file'] = () => {
    const a = giGate(); if (!a) return;
    const sum = giSummary(giDisk(), GI.text);
    risky('goinception.file', '仅保存 goInception 配置文件', `将把变更写入 <code>${esc(GI.deploy.path)}</code>（版本 <b>#${giNextV()}</b>），<b>不重启</b>，下次重启后生效。${giWarnHtml(a)}<div style="margin-top:6px">变更：${esc(sum)}</div>${giDiffCtx(giDisk(), giEffective(GI.text))}`, null, () => {
      const v = giWrite(sum, 'goinception.file'); toast(`已保存为 #${v.v}（待重启生效）`); rerender();
    }, a.warnings.length ? '确认风险' : '');
  };

  /* ---- 重启流程：校验 → 检查任务 → 写文件 → 重启 → 健康检查 → 失败自动恢复 ---- */
  async function giRunFlow(ver, o) {
    const D = GI.deploy, cur = giCur(); GI.flow = {v: ver ? ver.v : 0}; GI.log = [];
    const log = s => { GI.log.push(`[${clock()}] ${s}`); const el = $('#giLog'); if (el) { el.textContent = GI.log.join('\n'); el.scrollTop = el.scrollHeight; } };
    const upd = i => { const e = $('#giFlowSteps'); if (e) e.innerHTML = stepsBar(['校验配置', '检查任务', '写入文件', '重启', '健康检查', '完成'], i); };
    openModal({title: ver ? `保存并重启 · 版本 #${ver.v}` : '重启 goInception', ok: '', cancel: '关闭（后台继续）', wide: true, body: `<div id="giFlowSteps"></div><div class="log" id="giLog" style="height:230px"></div>`});
    const t0 = Date.now(); let ok = true, why = '';
    upd(0); log('加锁（同一时刻只允许一个保存 / 重启 / 回滚）… 成功'); await sleep(450);
    if (ver) log('校验配置文件：语法 / 参数名 / 取值范围 … 通过');
    upd(1); await sleep(400);
    const tasks = GI.proc.tasks.slice();
    if (!tasks.length) log('inception get processlist：没有进行中的任务');
    else if (o.force) { log(`进行中的任务 ${tasks.map(t => '#' + t.id).join('、')}：选择“立即重启”，这些任务将被中断，对应工单停在“执行有异常”`); GI.proc.tasks = []; }
    else { log(`进行中的任务 ${tasks.map(t => '#' + t.id + '（' + t.st + '）').join('、')}：选择“等待完成”（最长 ${D.waitMax} 秒），期间不重启`); for (const t of tasks) { await sleep(800); log(`任务 #${t.id} 已完成`); GI.proc.tasks = GI.proc.tasks.filter(x => x.id !== t.id); } }
    upd(2); await sleep(450); if (ver) log(`备份当前文件到 ${D.path}.dbtool.bak；临时文件 → fsync → rename 原子写入 #${ver.v}（sha256 ${hash8(ver.content)}…）`); else log('磁盘文件无需改动（仅重启）');
    upd(3); GI.proc.state = 'restarting'; if (ROUTE.page === 'goinception') { const st = $('#giStat'); if (st) st.innerHTML = '<span class="tag warn">重启中</span>'; }
    await sleep(500); log(`执行：${giCmd(D)}`); await sleep(900); log('命令退出码 0');
    upd(4); log(`健康检查：反复连接 ${GI.conn.host}:${GI.conn.port}，最长 ${D.healthSec} 秒`); await sleep(900);
    const bad = o.fail || (ver && /SMK_BAD/.test(ver.content));
    if (bad) {
      ok = false; why = '健康检查失败：连接被拒绝（goInception 启动后立即退出，疑似新配置导致起不来）';
      log('✖ ' + why);
      if (D.autoRollback && cur) {
        await sleep(600); log(`自动恢复：写回上一个生效版本 #${cur.v}，再次重启`); await sleep(900); log(`执行：${giCmd(D)}`); await sleep(800); log(`健康检查通过（#${cur.v}）`);
        if (ver) { ver.status = 'rolledback'; ver.restoredTo = cur.v; ver.reason = `${why}。已自动恢复到 #${cur.v} 并再次重启，健康检查通过。`; }
      } else { await sleep(400); log('自动回滚已关闭（或没有可恢复的版本）：磁盘保持新内容，需要人工处理'); if (ver) { ver.status = 'failed'; ver.reason = why + '；自动回滚已关闭，磁盘保持新内容。'; } }
    } else { await sleep(400); log('✔ inception get variables 正常，goInception 已就绪'); if (ver) ver.status = 'applied'; }
    const ms = Date.now() - t0; if (ver) ver.ms = ms;
    upd(5); GI.lastRestart = {at: now(), ok, ms, op: ME.login}; GI.proc.state = 'running'; GI.flow = null;
    setTimeout(() => { if (!GI.proc.tasks.length) GI.proc.tasks = [{id: 1042 + GI.versions.length, st: '检查中', name: '订单表增加字段 refund_flag'}]; }, 4000);   // 业务仍在提交新任务
    log(ok ? `完成：${ver ? '#' + ver.v + ' 已生效' : '重启成功'}，耗时 ${(ms / 1000).toFixed(1)} 秒` : `结束：${ver && ver.status === 'rolledback' ? '新版本未生效，已回到 #' + cur.v : '需要人工处理'}`);
    audit('goinception.restart', ok ? `${ver ? '保存并重启' : '重启'} goInception${ver ? '：#' + ver.v : ''}（docker restart ${D.container}，健康检查通过，耗时 ${(ms / 1000).toFixed(1)} s）` : `${ver ? '保存并重启 goInception：#' + ver.v + ' ' : '重启 goInception：'}${why}${ver && ver.status === 'rolledback' ? `，已自动恢复到 #${cur.v}` : ''}`, ok ? '成功' : '失败');
    if (ROUTE.page === 'goinception') rerender();
  }
  const giFlowOpts = () => ({force: ($('input[name=giWhen]:checked') || {}).value === 'force', fail: chk('giSimFail')});
  function giRestartDetail(a, ver) {
    const D = GI.deploy, tasks = GI.proc.tasks;
    return `<ol style="margin:0 0 8px;padding-left:20px;line-height:1.8"><li>校验配置文件（语法 / 参数名 / 取值范围）</li><li>${ver ? `备份当前文件并写入新版本 <b>#${giNextV()}</b>（变更：${esc(giSummary(giDisk(), GI.text))}）` : '不改配置文件（仅重启）'}</li>
      <li>重启 Docker 容器 <b>${esc(D.container)}</b>：<code>${esc(giCmd(D))}</code></li><li>健康检查：反复连接 ${esc(GI.conn.host)}:${GI.conn.port}，最长 ${D.healthSec} 秒</li><li>失败则${D.autoRollback ? '<b>自动恢复上一个生效版本并再次重启</b>' : '不自动恢复（已关闭），磁盘保持新内容'}</li></ol>${a ? giWarnHtml(a) : ''}
      ${tasks.length ? `<div class="notice warn" style="margin:8px 0"><span>⚠</span><div>当前有 <b>${tasks.length} 个任务</b>通过 goInception 执行（${tasks.map(t => `工单 #${t.id} ${t.st}`).join('、')}）。立即重启会中断它们，被中断的工单停在“执行有异常”，需人工确认目标库状态。</div></div>
        <label class="chk" style="display:flex;margin:4px 0"><input type="radio" name="giWhen" value="wait" ${D.waitDefault ? 'checked' : ''}> 等待执行中的任务完成后再重启（最长 ${D.waitMax} 秒，超时则不重启）</label><label class="chk" style="display:flex;margin:4px 0"><input type="radio" name="giWhen" value="force" ${D.waitDefault ? '' : 'checked'}> 立即重启（会中断正在执行的任务）</label>` : '<div class="small muted">当前没有进行中的任务。</div>'}
      <label class="chk" style="display:flex;margin-top:8px"><input type="checkbox" id="giSimFail"> <span class="small">🧪 原型演示：模拟“新配置导致 goInception 起不来”，观察自动恢复流程</span></label>`;
  }
  ACTIONS['goinception.restart'] = d => {
    const D = GI.deploy;
    if (d.mode === 'deploy') return giSaveDeploy();
    if (d.mode === 'only') {
      const pend = giPending();
      return risky('goinception.restart', '仅重启 goInception', `将重启 goInception 容器 <b>${esc(D.container)}</b>${pend ? `，并先把待生效的 <b>#${pend.v}</b> 写入磁盘` : '，不改配置文件'}。${giRestartDetail(null, null)}`, giCmd(D), () => { const o = giFlowOpts(); setTimeout(() => giRunFlow(pend, o), 0); }, '重启');
    }
    const a = giGate(); if (!a) return;
    const sum = giSummary(giDisk(), GI.text);
    risky('goinception.restart', '保存并重启 goInception', giRestartDetail(a, true), giCmd(D), () => {
      const o = giFlowOpts(), v = giWrite(sum, 'goinception.restart');
      setTimeout(() => giRunFlow(v, o), 0);
    }, '重启');
  };

  /* ---- 回滚（高危）：生成新版本 → 走保存并重启流程 ---- */
  ACTIONS['goinception.rollback'] = d => {
    const t = GI.versions.find(x => x.v === +d.v); if (!t) return;
    if (t.status === 'failed' || t.status === 'rolledback') return toast('失败 / 已自动回滚的版本不能作为回滚目标');
    if (t.v === giCur().v) return toast('该版本就是当前生效版本');
    const D = GI.deploy, sum = `回滚到 #${t.v}：${t.summary}`;
    risky('goinception.rollback', `回滚到版本 #${t.v}`, `以 #${t.v} 的内容生成<b>新版本 #${giNextV()}</b>（摘要“${esc(sum)}”），并走保存并重启流程。<div style="margin-top:8px">${giDiffCtx(giDisk(), t.content)}</div>${giRestartDetail(null, true)}`, giCmd(D), () => {
      const o = giFlowOpts(), v = giWrite(sum, 'goinception.rollback', t.content);
      setTimeout(() => giRunFlow(v, o), 0);
    }, `#${t.v}`);
  };

  /* ---- 历史版本 ---- */
  ON.giVer = d => {
    const x = GI.versions.find(y => y.v === +d.v); if (!x) return;
    const prev = giSorted().find(y => y.v < x.v && (y.status === 'applied' || y.status === 'draft')), lb = giLabel(x);
    openDrawer(`版本 #${x.v}`, `${kvList([['版本', '#' + x.v], ['时间', x.at], ['操作人', esc(x.op)], ['状态', `<span class="tag ${lb[0]}">${esc(lb[1])}</span>`], ['变更摘要', `<span class="mono small">${esc(x.summary)}</span>`], ['重启方式', esc(giModeName()) + ' · ' + esc(GI.deploy.container)],
      ['重启结果', x.status === 'draft' ? '尚未重启' : x.ms ? `${x.status === 'applied' ? '成功' : '失败'}，耗时 ${(x.ms / 1000).toFixed(1)} 秒` : '—'], ['内容 SHA-256', `<span class="mono small">${hash8(x.content)}${hash8(x.content + '1')}${hash8(x.content + '2')}${hash8(x.content + '3')}…</span>`]])}
      ${x.reason ? `<div style="margin-top:10px">${notice(esc(x.reason), 'bad', '✖')}<h4>goInception 启动日志（尾部）</h4><div class="log" style="height:110px">2026-09-25 16:43:31 [INFO] load config /etc/goinception/config.toml\n2026-09-25 16:43:31 [ERROR] [osc] osc_bin_dir "/opt/percona/bin": no such file or directory\n2026-09-25 16:43:31 [FATAL] invalid config, exit status 1</div></div>` : ''}
      <h4>${prev ? `与上一版本 #${prev.v} 的对比` : '文件内容'}</h4>${prev ? giDiffCtx(prev.content, x.content) : code(x.content)}`, true);
  };
  function giHistTab() {
    const rows = giSorted();
    return `<div class="card"><h2>历史版本</h2>${tbl(['版本', '时间', '操作人', '变更摘要', '状态', '操作'], rows, {
      cells: x => { const lb = giLabel(x), canRb = (x.status === 'applied' || x.status === 'draft') && x.v !== giCur().v;
        return [`<b>#${x.v}</b>`, `<span class="mono small">${x.at}</span>`, esc(x.op), `<span class="mono small">${esc(x.summary)}</span>`, `<span class="tag ${lb[0]}">${esc(lb[1])}</span>`,
          `<div class="ops"><button class="btn sm" data-on="giVer" data-v="${x.v}">${x.reason ? '查看原因' : '查看'}</button>${canRb ? B('goinception.rollback', '回滚到此版本', {cls: 'sm', data: {v: x.v}}) : ''}</div>`]; }
    })}</div>`;
  }

  /* ---- 连接信息 ---- */
  ACTIONS['goinception.conn'] = d => {
    const b = d.t === 'backup', P = b ? ['gi_bh', 'gi_bp', 'gi_bu', 'gi_bw'] : ['gi_h', 'gi_p', 'gi_u', 'gi_w'], C = GI.conn;
    const host = val(P[0]), port = parseInt(val(P[1]), 10), user = val(P[2]), pwd = $('#' + P[3]).value;
    if (!host) return toast('请填写 HOST'); if (!(port >= 1 && port <= 65535)) return toast('PORT 须为 1–65535');
    if (!user) return toast('请填写 USER');
    const oh = b ? C.bHost : C.host, op = b ? C.bPort : C.port, ou = b ? C.bUser : C.user, moved = host !== oh || port !== op || user !== ou;
    if (d.mode === 'test') {
      const L = [], ms = 6 + (hash8(host).charCodeAt(0) % 20);
      if (moved && !pwd) { L.push(['ok', `连接 ${host}:${port} …`], ['bad', '认证失败：改了地址 / 端口 / 账号但没有重新输入密码。为避免把已保存的密码发给新地址，本次测试没有携带密码。']); }
      else if (!/^10\./.test(host)) L.push(['bad', `无法连接 ${host}:${port}：connection timed out（5 秒）`]);
      else if (b) L.push(['ok', `备份库 ${host}:${port} 可连接（延迟 ${ms} ms）`], ['ok', `账号 ${user} 认证通过，MySQL 8.0.35`], ['ok', 'SHOW GRANTS：有 INSERT / CREATE 权限，可写入回滚备份']);
      else L.push(['ok', `连接 ${host}:${port} 成功（延迟 ${ms} ms）`], ['ok', `goInception ${GI.proc.ver} · 账号 ${user} 认证通过`], ['ok', '`inception get variables` 返回 42 项，确认对方是 goInception']);
      return openModal({title: b ? '测试备份库' : '测试连接', code: 'goinception.conn', ok: '', cancel: '关闭', body: L.map(l => `<div class="p2-line ${l[0]}">${AP_ICON[l[0]]} ${esc(l[1])}</div>`).join('')});
    }
    risky('goinception.conn', `保存${b ? '备份库' : 'goInception'}连接信息`, `将更新 <code>dbt_sql_config</code> 中的${b ? '备份库' : 'goInception'}连接：<b>${esc(host)}:${port}</b>（用户 ${esc(user)}${pwd ? '，<b>密码已修改</b>' : '，密码不变'}）。`,
      [`UPDATE dbt_sql_config SET value = '${host}' WHERE item = '${b ? 'inception_remote_backup_host' : 'go_inception_host'}';`, `UPDATE dbt_sql_config SET value = '${port}' WHERE item = '${b ? 'inception_remote_backup_port' : 'go_inception_port'}';`, `UPDATE dbt_sql_config SET value = '${user}' WHERE item = '${b ? 'inception_remote_backup_user' : 'go_inception_user'}';`].concat(pwd ? [`UPDATE dbt_sql_config SET value = '<mirage 加密>' WHERE item = '${b ? 'inception_remote_backup_password' : 'go_inception_password'}';`] : []).join('\n'), () => {
        if (b) Object.assign(C, {bHost: host, bPort: port, bUser: user}); else Object.assign(C, {host, port, user});
        audit('goinception.conn', `修改goInception${b ? '备份库' : ''}连接信息：${host}:${port}（用户 ${user}${pwd ? '，密码已修改' : ''}）`); toast('连接信息已保存，即时生效'); rerender();
      });
  };
  function giConnTab() {
    const C = GI.conn;
    return `<div class="grid g2"><div class="card"><h2>审核 / 执行连接</h2><div class="form">
        ${fld('HOST', `<input class="mono" id="gi_h" value="${esc(C.host)}">`)}${fld('PORT', `<input class="mono" id="gi_p" value="${C.port}">`)}${fld('USER', `<input class="mono" id="gi_u" value="${esc(C.user)}">`)}${fld('PASSWORD', '<input type="password" id="gi_w" placeholder="已设置（留空不修改）" autocomplete="new-password">')}</div>
        <div class="row" style="margin-top:12px">${B('goinception.conn', '测试连接', {data: {mode: 'test', t: 'main'}})}<span class="sp"></span>${B('goinception.conn', '保存（即时生效）', {cls: 'primary', data: {mode: 'save', t: 'main'}})}</div></div>
      <div class="card"><h2>备份库</h2><div class="form">
        ${fld('BACKUP HOST', `<input class="mono" id="gi_bh" value="${esc(C.bHost)}">`)}${fld('BACKUP PORT', `<input class="mono" id="gi_bp" value="${C.bPort}">`)}${fld('BACKUP USER', `<input class="mono" id="gi_bu" value="${esc(C.bUser)}">`)}${fld('BACKUP PASSWORD', '<input type="password" id="gi_bw" placeholder="已设置（留空不修改）" autocomplete="new-password">')}</div>
        <div class="row" style="margin-top:12px">${B('goinception.conn', '测试备份库', {data: {mode: 'test', t: 'backup'}})}<span class="sp"></span>${B('goinception.conn', '保存（即时生效）', {cls: 'primary', data: {mode: 'save', t: 'backup'}})}</div></div></div>`;
  }

  /* ---- 部署与重启 ---- */
  function giSaveDeploy() {
    const D = GI.deploy, nv = {path: val('gd_path'), container: val('gd_container'), healthSec: parseInt(val('gd_health'), 10), waitMax: parseInt(val('gd_wait'), 10), autoRollback: chk('gd_auto'), waitDefault: chk('gd_waitdef')};
    if (!/^\/.+\.toml$/.test(nv.path) || /[\x00-\x1f]/.test(nv.path)) return toast('配置文件路径必须是以 .toml 结尾的绝对路径');
    if (!GI_CONTAINER_RE.test(nv.container)) return toast('容器名不合法：只允许字母、数字和 _ . -，且以字母或数字开头（最长 128 位）');
    if (!(nv.healthSec >= 5 && nv.healthSec <= 600)) return toast('健康检查等待时间须为 5–600 秒');
    if (!(nv.waitMax >= 10 && nv.waitMax <= 3600)) return toast('等待任务最长时间须为 10–3600 秒');
    const ch = Object.keys(nv).filter(k => String(nv[k]) !== String(D[k])).map(k => `${k}: ${D[k]} → ${nv[k]}`);
    if (!ch.length) return toast('没有变更');
    risky('goinception.restart', '保存部署与重启设置', `重启命令固定为 <code>docker restart 容器名</code>：<ul style="margin:6px 0 0;padding-left:18px">${ch.map(c => `<li>${esc(c)}</li>`).join('')}</ul>`, giCmd(nv), () => {
      Object.assign(D, nv); audit('goinception.restart', `修改goInception部署设置：Docker 容器 ${nv.container}；${ch.slice(0, 3).join('；')}`); toast('部署与重启设置已保存'); rerender();
    });
  }
  ON.giDeploy = () => { const D = Object.assign({}, GI.deploy, {container: val('gd_container')}); const p = $('#gd_prev'); if (p) p.value = giCmd(D); };
  ON.giRefreshStatus = () => { toast('进程状态已刷新'); rerender(); };
  ACTIONS['goinception.view'] = () => {
    const D = GI.deploy, L = [['ok', `配置文件 ${D.path}：可读写，所在目录可创建临时文件（原子替换）`], ['ok', '文件属主 / 属组可恢复，权限位保持不变']];
    L.push(['ok', `sudo -n -l：允许执行 docker restart ${D.container}（白名单内）`], ['ok', `docker inspect ${D.container}：容器存在，状态 running`]);
    L.push(['ok', `goInception ${GI.conn.host}:${GI.conn.port} 可连接，inception get variables 正常`]);
    openModal({title: '检查权限与连通性', ok: '', cancel: '关闭', code: 'goinception.view', body: L.map(l => `<div class="p2-line ${l[0]}">${AP_ICON[l[0]]} ${esc(l[1])}</div>`).join('')});
  };
  function giDeployTab() {
    const D = GI.deploy, P = GI.proc, lr = GI.lastRestart, cur = giCur(), pend = giPending(), days = Math.floor((new Date(TODAY) - new Date(P.started.slice(0, 10))) / 864e5);
    return `<div class="grid g2"><div class="card"><h2>配置文件与重启方式</h2><div class="form">
        ${fld('配置文件路径', `<input class="mono" id="gd_path" value="${esc(D.path)}">`)}
        ${fld('重启方式', `<input value="${esc(giModeName())}" readonly>`)}
        ${fld('容器名', `<input class="mono" id="gd_container" value="${esc(D.container)}" data-oninput="giDeploy">`, {req: true})}
        ${fld('重启命令（只读预览）', `<input class="mono" id="gd_prev" value="${esc(giCmd(D))}" readonly>`)}
        ${fld('健康检查（秒，5–600）', `<input class="mono" id="gd_health" value="${D.healthSec}">`)}
        ${fld('等待任务（秒，10–3600）', `<input class="mono" id="gd_wait" value="${D.waitMax}">`)}
        ${fld('', `<label class="chk"><input type="checkbox" id="gd_auto" ${D.autoRollback ? 'checked' : ''}> 健康检查失败时自动恢复上一个生效版本并再次重启</label>`)}
        ${fld('', `<label class="chk"><input type="checkbox" id="gd_waitdef" ${D.waitDefault ? 'checked' : ''}> 有任务正在执行时，默认选“等待完成后再重启”</label>`)}</div>
        <div class="row" style="margin-top:12px">${B('goinception.view', '检查权限与连通性')}<span class="sp"></span>${B('goinception.restart', '保存', {cls: 'primary', data: {mode: 'deploy'}})}</div></div>
      <div class="card"><h2>进程状态</h2>${kvList([['状态', P.state === 'running' ? '<span class="tag ok">运行中</span>' : '<span class="tag warn">重启中</span>'], ['版本', 'goInception ' + P.ver], ['启动时间', `${P.started}（已运行 ${days} 天）`], ['当前连接 / 任务', `${P.conns} 个连接 · ${P.tasks.length} 个任务${P.tasks.length ? '（' + P.tasks.map(t => `工单 #${t.id} ${t.st}`).join('、') + '）' : ''}`],
        ['配置文件', pend ? `已生效 #${cur.v}；磁盘上是待生效的 #${pend.v} <span class="tag warn">未生效</span>` : `已生效版本 #${cur.v}；磁盘文件与 #${cur.v} 一致 ✔`], ['最近一次重启', `${lr.at}（${lr.ok ? '成功' : '失败'}${lr.ms ? `，耗时 ${(lr.ms / 1000).toFixed(1)} 秒` : ''}，docker restart ${esc(D.container)}，${esc(lr.op)}）`]])}
        <div class="row" style="margin-top:12px"><button class="btn" data-on="giRefreshStatus">刷新状态</button><span class="sp"></span>${B('goinception.restart', '仅重启', {cls: 'danger', data: {mode: 'only'}})}</div></div></div>`;
  }

  pages.goinception = {
    render(sub) {
      const tab = ['conn', 'file', 'hist', 'deploy'].includes(sub) ? sub : 'conn'; GI.tab = tab;
      const pend = giPending(), P = GI.proc, D = GI.deploy;
      const banner = GI.flow ? notice(`流程进行中：${GI.flow.v ? '版本 #' + GI.flow.v : '仅重启'}（保存 → 重启 → 健康检查）。`, '', '⏳')
        : pend ? notice(`有<b>未生效的配置 #${pend.v}</b>（已写入磁盘，待重启生效）。 ${B('goinception.restart', '仅重启', {cls: 'sm', data: {mode: 'only'}})}`, 'warn', '⏳') : '';
      return CSS2 + `<div class="row" style="margin-bottom:10px"><span id="giStat">${P.state === 'running' ? '<span class="tag ok">运行中</span>' : '<span class="tag warn">重启中</span>'}</span><span class="muted small">${esc(P.ver)} · 已运行 ${Math.floor((new Date(TODAY) - new Date(P.started.slice(0, 10))) / 864e5)} 天 · 当前 ${P.tasks.length} 个任务在执行</span><span class="sp"></span><span class="muted small">重启方式：Docker（${esc(D.container)}）</span></div>
      ${banner}${tabs('goinception', [['conn', '连接信息'], ['file', '配置文件'], ['hist', '历史版本'], ['deploy', '部署与重启']], tab)}
      ${tab === 'conn' ? giConnTab() : tab === 'file' ? giFileTab() : tab === 'hist' ? giHistTab() : giDeployTab()}`;
    },
    init(sub) { if (sub === 'file') giRefresh(true); }
  };

  /* ============================================================================
     通知渠道 notify（钉钉 / 飞书 / Lark / Slack 机器人 + 邮件 SMTP；事件订阅、@ 规则、测试发送）
     导出 window.NOTIFY_CHANNELS：元素含 id, name, type, enabled, rgs（全局渠道 global=true，rgs 为全部启用的资源组）
     ========================================================================== */
  const NT_EV = [['submitted', '工单提交', '新工单提交'], ['pending', '待审批', '新工单 / 流转到下一级审批'], ['passed', '审批通过', ''], ['rejected', '被驳回', '含系统自动驳回'], ['aborted', '提交人终止', ''],
    ['scheduled', '已设置定时执行', ''], ['executing', '开始执行', ''], ['finished', '执行完成', '含手工确认结束'], ['exception', '执行异常', ''], ['perf_overdue', '性能工单超时', 'SLA 到期未处理'], ['inspect_alert', '巡检告警', '健康分低于阈值']];
  const ntEvName = k => (NT_EV.find(e => e[0] === k) || [0, k])[1];
  const NT_TARGETS = [['applicant', '提交人'], ['reviewers', '当前审批级的审批人'], ['dba_duty', 'DBA 值班'], ['auth_group', '权限组成员'], ['user', '指定用户'], ['raw', '渠道原生 ID'], ['all', '所有人']];
  const NT_TYPES = {
    '钉钉': {host: 'oapi.dingtalk.com', ph: 'https://oapi.dingtalk.com/robot/send?access_token=…', raw: '手机号（11 位）', rawRe: /^1\d{10}$/},
    '飞书': {host: 'open.feishu.cn', ph: 'https://open.feishu.cn/open-apis/bot/v2/hook/…', raw: 'open_id（ou_…）', rawRe: /^ou_[A-Za-z0-9]+$/},
    'Lark': {host: 'open.larksuite.com', ph: 'https://open.larksuite.com/open-apis/bot/v2/hook/…', raw: 'open_id（ou_…）', rawRe: /^ou_[A-Za-z0-9]+$/},
    'Slack': {host: 'hooks.slack.com', ph: 'https://hooks.slack.com/services/T…/B…/…', raw: '用户 U… / 用户组 S…', rawRe: /^[US][A-Z0-9]{6,}$/}};
  /* 邮件渠道（type = email）：SMTP 配置存 mail；密码与签名密钥同一套加密、不回显（这里只有 hasPw）。@ 规则解析成 dbt_sql_users.email */
  NT_TYPES['邮件'] = {host: 'smtp.example.com', ph: '', raw: '邮箱地址（如 dba@example.com）', rawRe: /^[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$/, email: true};
  const NT_ENC = [['starttls', 'STARTTLS（常用端口 587）', 587], ['ssl', 'SSL/TLS（常用端口 465）', 465], ['none', '不加密（常用端口 25）', 25]];
  const ntEncName = k => ({starttls: 'STARTTLS', ssl: 'SSL/TLS', none: '不加密'})[k] || k;
  const isMail = c => c.type === '邮件';
  const reAddr = /^[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$/;
  const ntAddrs = v => v.split(/[,;，；\s]+/).map(x => x.trim()).filter(Boolean);
  const ntDedup = a => a.filter((x, i) => a.findIndex(y => y.toLowerCase() === x.toLowerCase()) === i);   // 不区分大小写去重，保留先出现的写法
  const NT_MAIL_DEF = {host: 'smtp.example.com', port: 587, enc: 'starttls', verify: true, user: 'dbtool@example.com', hasPw: true, plain: false, from: 'dbtool@example.com', fromName: 'DBA 工具', to: ['dba-team@example.com'], cc: [], prefix: '[dbtool]'};
  const ntTargetLabel = (t, ty) => ty === '邮件' && t[0] === 'raw' ? '邮箱地址' : ty === '邮件' && t[0] === 'all' ? '资源组的全部用户' : t[1];
  const NT_COLOR = {blue: 'var(--accent)', green: 'var(--ok)', red: 'var(--bad)', orange: 'var(--warn)', gray: 'var(--muted)'};
  const allRgIds = () => RGROUPS.filter(r => r.enabled).map(r => r.id);
  const NOTIFY = [
    {id: 1, name: '订单中心 · 研发通知', type: '钉钉', enabled: true, rgs: [1], global: false, host: 'oapi.dingtalk.com', tail: 'a3f9', signed: true, events: [],
      rules: [{t: 'applicant', v: '', ev: [], wh: false}, {t: 'reviewers', v: '', ev: ['pending'], wh: true}], last: '2026-10-07 10:08', lastOk: true},
    {id: 2, name: '订单中心 · DBA 值班群', type: 'Slack', enabled: true, rgs: [1], global: false, host: 'hooks.slack.com', tail: 'k2Qx', signed: true, events: ['pending', 'passed', 'rejected', 'executing', 'finished', 'exception'],
      rules: [{t: 'applicant', v: '', ev: ['passed', 'finished', 'exception'], wh: false}, {t: 'reviewers', v: '', ev: ['pending'], wh: true}, {t: 'dba_duty', v: '', ev: ['exception'], wh: false}, {t: 'raw', v: 'S0ARY4FQATB', ev: [], wh: false}], last: '2026-10-07 10:08', lastOk: true},
    {id: 3, name: '支付中心 · 审批群', type: '飞书', enabled: true, rgs: [2], global: false, host: 'open.feishu.cn', tail: 'e81c', signed: true, events: ['pending', 'passed', 'rejected', 'exception'],
      rules: [{t: 'reviewers', v: '', ev: ['pending'], wh: true}, {t: 'applicant', v: '', ev: ['passed', 'rejected', 'exception'], wh: false}], last: '2026-10-06 11:12', lastOk: true},
    {id: 4, name: '用户中心 · 变更通知', type: '钉钉', enabled: true, rgs: [3], global: false, host: 'oapi.dingtalk.com', tail: '7b20', signed: false, events: [],
      rules: [{t: 'auth_group', v: 'g_dba', ev: ['exception'], wh: false}, {t: 'applicant', v: '', ev: [], wh: false}], last: '2026-10-06 17:30', lastOk: false},
    {id: 5, name: '数据平台 · 数仓告警', type: 'Slack', enabled: true, rgs: [4], global: false, host: 'hooks.slack.com', tail: 'p9Zd', signed: true, events: ['exception', 'perf_overdue', 'inspect_alert'],
      rules: [{t: 'dba_duty', v: '', ev: [], wh: false}], last: '2026-10-07 06:10', lastOk: true},
    {id: 6, name: '全局告警（飞书）', type: '飞书', enabled: false, rgs: allRgIds(), global: true, host: 'open.feishu.cn', tail: '03aa', signed: false, events: ['perf_overdue', 'inspect_alert'],
      rules: [{t: 'all', v: '', ev: ['inspect_alert'], wh: false}], last: '', lastOk: true},
    {id: 7, name: '海外团队（Lark）', type: 'Lark', enabled: true, rgs: [4], global: false, host: 'open.larksuite.com', tail: 'c5d1', signed: true, events: ['pending', 'passed'],
      rules: [{t: 'reviewers', v: '', ev: ['pending'], wh: false}], last: '2026-10-05 21:40', lastOk: true},
    {id: 8, name: '订单中心 · 审批邮件', type: '邮件', enabled: true, rgs: [1, 2], global: false, host: 'smtp.example.com', tail: '', signed: true, mail: {...NT_MAIL_DEF}, events: ['pending', 'passed', 'rejected', 'exception'],
      rules: [{t: 'reviewers', v: '', ev: ['pending'], wh: false}, {t: 'applicant', v: '', ev: ['passed', 'rejected', 'exception'], wh: false}], last: '2026-10-07 10:08', lastOk: true},
    {id: 9, name: '全局告警（邮件 · 内网自签名证书）', type: '邮件', enabled: false, rgs: allRgIds(), global: true, host: 'mail.corp.local', tail: '', signed: false,
      mail: {host: 'mail.corp.local', port: 465, enc: 'ssl', verify: false, user: '', hasPw: false, plain: false, from: 'alert@corp.local', fromName: '', to: ['oncall@corp.local'], cc: ['dba-lead@corp.local'], prefix: '[告警]'}, events: ['perf_overdue', 'inspect_alert'],
      rules: [{t: 'dba_duty', v: '', ev: [], wh: false}, {t: 'raw', v: 'sre-oncall@corp.local', ev: ['inspect_alert'], wh: false}], last: '2026-10-06 03:00', lastOk: false},
  ];
  window.NOTIFY_CHANNELS = NOTIFY;
  const NT_LOG = [['2026-10-07 10:08', 1, 'submitted', true, ''], ['2026-10-07 10:08', 2, 'pending', true, ''], ['2026-10-07 10:08', 8, 'pending', true, '1 个用户没有邮箱，已跳过：孙测试'], ['2026-10-07 09:52', 5, 'exception', true, ''], ['2026-10-07 06:10', 5, 'inspect_alert', true, ''],
    ['2026-10-06 17:30', 4, 'exception', false, 'HTTP 429：触发钉钉机器人限流（20 条/分钟），已记录，不影响业务'], ['2026-10-06 11:12', 3, 'passed', true, ''], ['2026-10-06 03:00', 9, 'inspect_alert', false, '连接 mail.corp.local:465 失败：i/o timeout（10 秒）；已重试 1 次仍失败，不影响业务'], ['2026-10-05 22:10', 8, 'passed', false, '认证失败：535 5.7.8 Authentication credentials invalid'], ['2026-10-05 21:40', 7, 'pending', true, ''], ['2026-10-05 15:40', 2, 'passed', true, '']];
  const NT = {type: '', rg: '', q: '', rules: [], pv: '钉钉', test: null};
  const ntRgText = c => c.global ? '全局（所有资源组）' : c.rgs.map(rgName).join('、') || '—';

  /* ---- 消息样例 / @ 解析 / 各类机器人格式 ---- */
  const WF_URL = 'https://dba.example.com/sqlworkflow/1042';
  const wfF = (rgn, flow, cur, extra) => [['发起时间', '2026-10-07 10:08:21'], ['发起人', '赵开发（rd_zhao）'], ['组', rgn], ['目标实例', 'mysql-order-prod-m'], ['数据库', 'order_db'], ['审批流程', flow], ['当前审批', cur], ['工单名称', '订单表增加字段 refund_flag']].concat(extra || [], [['工单地址', WF_URL]]);
  /* 数据归档没有单独的订阅项，归档事件复用上面已有的事件名（Event.Kind = archive）：归档开始 → 开始执行；归档完成、被窗口 / 上限中断 → 执行完成；
     负载中断、失败、校验失败、结构变更暂停、自动暂停 → 执行有异常；申请提交 / 流转 / 通过 / 驳回 → 待处理 / 审批通过 / 被驳回。
     下面是这六个事件在“数据归档”来源下的消息样例；测试发送里把“消息来源”选成“数据归档”即可预览 / 发送。 */
  const arcF = (g, extra) => [['发起时间', '2026-10-07 02:00:03'], ['发起人', '赵开发（rd_zhao）'], ['组', g], ['任务', '订单主表-已完成订单归档'], ['来源', 'order_db.orders（实例 #1）'], ['模式', '归档并删除']].concat(extra, [['任务地址', 'https://dba.example.com/archive#task-1']]);
  const NT_ARC = {
    pending: {title: '[数据归档申请]新的数据归档申请', color: 'blue', level: 'g_tl', fields: g => arcF(g, [['审批流程', '组长 TL(current) → DBA']])},
    passed: {title: '[数据归档申请]审批通过，任务已启用', color: 'green', fields: g => arcF(g, [['审批人', '李 DBA（dba_li）']])},
    rejected: {title: '[数据归档申请]被驳回', color: 'red', fields: g => arcF(g, [['驳回人', '李 DBA（dba_li）'], ['驳回原因', 'created_at 上没有索引，请先建索引后再提']])},
    executing: {title: '[数据归档]订单主表-已完成订单归档：归档开始', color: 'blue', fields: g => arcF(g, [['运行', '#5031（schedule）'], ['统计', '读取 0 行 · 备份 0 行 · 删除 0 行 · 0 批']])},
    finished: {title: '[数据归档]订单主表-已完成订单归档：归档完成', color: 'green', fields: g => arcF(g, [['运行', '#5031（schedule）'], ['统计', '读取 51,203 行 · 备份 51,203 行 · 删除 51,203 行 · 52 批']])},
    exception: {title: '[数据归档]订单主表-已完成订单归档：归档失败', color: 'red', fields: g => arcF(g, [['运行', '#5034（schedule）'], ['统计', '读取 77,000 行 · 备份 76,000 行 · 删除 76,000 行 · 77 批'], ['错误', 'ERROR 2013 (HY000): Lost connection to MySQL server during query；失败批已回滚，源数据未删除']])},
  };
  const NT_SAMPLE = {
    submitted: {title: '[SQL上线申请]新的工单申请#1042', color: 'blue', level: 'g_tl', fields: g => wfF(g, '组长 TL(current) → DBA', '组长 TL')},
    pending: {title: '[SQL上线申请]工单流转到下一级#1042', color: 'blue', level: 'g_dba', fields: g => wfF(g, '组长 TL ✔ → DBA(current)', 'DBA')},
    passed: {title: '[SQL上线申请]审批通过#1042', color: 'green', fields: g => wfF(g, '组长 TL ✔ → DBA ✔', '—', [['审批人', '李 DBA（dba_li）']])},
    rejected: {title: '[SQL上线申请]工单被驳回#1042', color: 'red', fields: g => wfF(g, '组长 TL ✘', '—', [['驳回人', '王组长（tl_wang）'], ['驳回原因', '缺少回滚方案，请补充后重新提交']])},
    aborted: {title: '[SQL上线申请]提交人终止工单#1042', color: 'gray', fields: g => wfF(g, '组长 TL(current) → DBA', '组长 TL', [['终止人', '赵开发（rd_zhao）']])},
    scheduled: {title: '[SQL上线申请]已设置定时执行#1042', color: 'blue', fields: g => wfF(g, '组长 TL ✔ → DBA ✔', '—', [['定时执行时间', '2026-10-08 02:00']])},
    executing: {title: '[SQL上线申请]开始执行#1042', color: 'blue', fields: g => wfF(g, '组长 TL ✔ → DBA ✔', '—', [['执行人', '李 DBA（dba_li）']])},
    finished: {title: '[SQL上线申请]工单执行完成#1042', color: 'green', fields: g => wfF(g, '组长 TL ✔ → DBA ✔', '—', [['执行结果', '成功，耗时 3.2 s，影响行数 0']])},
    exception: {title: '[SQL上线申请]工单执行有异常#1042', color: 'red', fields: g => wfF(g, '组长 TL ✔ → DBA ✔', '—', [['错误信息', 'Lock wait timeout exceeded; try restarting transaction']])},
    perf_overdue: {title: '[性能工单]P1 工单已超时 #PF-2031', color: 'orange', fields: g => [['级别', 'P1'], ['负责人', '周开发（rd_zhou）'], ['组', g], ['实例', 'mysql-pay-prod-m'], ['SQL 指纹', '3fa9c1e07b…（payments 按 paid_at 范围扫描）'], ['已超时', '18 小时（SLA 解决期限 2026-10-06 18:00）'], ['工单地址', 'https://dba.example.com/perf/detail/PF-2031']]},
    inspect_alert: {title: '[性能巡检]mysql-user-prod-m 健康分 62（较昨日 -9）', color: 'orange', fields: g => [['实例', 'mysql-user-prod-m'], ['所属组', g], ['告警项', '连接数使用率 91%；慢查询 412 条/天；主从延迟 38 s'], ['巡检时间', '2026-10-07 06:10'], ['报告地址', 'https://dba.example.com/inspect/detail/R-0412']]},
  };
  const NT_NOID = ['qa_sun', 'auditor_chen'];   // 没有渠道用户标识的用户：退化为文本 @显示名
  function ntRef(u, type) {
    const h = hash8(u.login + type);
    if (type === '邮件') return {login: u.login, name: u.name, id: u.email ? 'mail' : '', email: u.email || ''};
    return {login: u.login, name: u.name, id: NT_NOID.includes(u.login) ? '' : type === '钉钉' ? '138' + String(parseInt(h.slice(0, 6), 16) % 90000000 + 10000000) : type === 'Slack' ? 'U0' + h.toUpperCase().slice(0, 7) : 'ou_' + h + h.slice(0, 8)};
  }
  const ntTargetName = (r, ty) => ntTargetLabel(NT_TARGETS.find(t => t[0] === r.t) || [r.t, r.t], ty) + (r.t === 'auth_group' ? `（${groupName(r.v)}）` : r.t === 'user' ? `（${r.v}）` : r.t === 'raw' ? `（${r.v}）` : '');
  function ntMsg(c, ev, wh, src) {
    const S = (src === 'archive' && NT_ARC[ev]) || NT_SAMPLE[ev], rgid = c.global ? 1 : c.rgs[0], rgn = rgName(rgid), ments = [], skipped = [], seen = new Set();
    const addU = u => { if (seen.has(u.login)) return; seen.add(u.login); ments.push(ntRef(u, c.type)); };
    c.rules.forEach(r => {
      const label = ntTargetName(r, c.type);
      if (r.ev.length && !r.ev.includes(ev)) { skipped.push(`${label}：该规则只在「${r.ev.map(ntEvName).join('、')}」时 @，当前事件不 @`); return; }
      if (r.wh && !wh) { skipped.push(`${label}：设置了“仅工作时间 @”，当前是非工作时间，只发消息不 @`); return; }
      if (r.t === 'applicant') { if (S.fields('').some(f => f[0] === '发起人')) addU(userBy('rd_zhao')); else if (ev === 'perf_overdue') addU(userBy('rd_zhou')); else skipped.push(`${label}：该事件没有提交人`); }
      else if (r.t === 'reviewers') {
        if (!S.level) { skipped.push(`${label}：「${ntEvName(ev)}」事件没有当前审批级`); return; }
        const us = USERS.filter(u => u.status === 'active' && u.groups.includes(S.level) && u.rgs.includes(rgid));
        if (!us.length) { skipped.push(`${label}：资源组「${rgn}」里没有「${groupName(S.level)}」成员，该级无人可审，退化为文本`); ments.push({text: '@' + groupName(S.level), miss: true}); } else us.forEach(addU);
      } else if (r.t === 'dba_duty') addU(userBy('dba_li'));
      else if (r.t === 'auth_group') { const us = USERS.filter(u => u.status === 'active' && u.groups.includes(r.v) && u.rgs.includes(rgid)); if (!us.length) skipped.push(`${label}：资源组「${rgn}」里没有该权限组成员`); else us.forEach(addU); }
      else if (r.t === 'user') { const u = userBy(r.v); if (u) addU(u); else skipped.push(`${label}：用户不存在`); }
      else if (r.t === 'raw') ments.push({raw: true, text: r.v});
      else if (r.t === 'all') ments.push({all: true});
    });
    const mail = isMail(c) ? Object.assign(ntMailRcpts(c, ments, rgid), {subject: (c.mail.prefix ? c.mail.prefix + ' ' : '') + S.title}) : null;
    return {title: S.title, color: S.color, fields: S.fields(rgn), ments: ments.slice(0, 50), skipped, mail};
  }
  const ntAtHtml = (x, type) => x.all ? '<span class="p2-at">@所有人</span>' : x.raw ? `<span class="p2-at">@${esc(x.text)}</span>` : x.miss ? `<span class="p2-at miss">${esc(x.text)}</span>` : x.id ? `<span class="p2-at">@${esc(x.name)}</span>` : `<span class="p2-at miss" title="该用户没有此渠道的标识，退化为文本，不会发错人">@${esc(x.name)}（文本）</span>`;
  function ntRender(type, m) {
    const ats = m.ments.map(x => ntAtHtml(x, type)).join(' ');
    if (type === 'Slack') return `<div class="p2-msg">${ats ? ats + ' ' : ''}<b>${esc(m.title)}</b><br>${m.fields.map(f => `${esc(f[0])}：${esc(f[1])}`).join('<br>')}</div>`;
    if (type === '钉钉') return `<div class="p2-msg"><div style="font-weight:700;font-size:15px;margin-bottom:6px">${esc(m.title)}</div>${m.fields.map(f => `<div>· <b>${esc(f[0])}</b>：${esc(f[1])}</div>`).join('')}${ats ? `<div style="margin-top:6px">${ats}</div>` : ''}</div>`;
    return `<div class="p2-msg" style="padding:0;overflow:hidden"><div style="background:${NT_COLOR[m.color]};color:#fff;padding:8px 14px;font-weight:700">${esc(m.title)}</div><div style="padding:10px 14px;display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:6px 14px">${m.fields.map(f => `<div style="${/地址|错误|原因|工单名称|SQL/.test(f[0]) ? 'grid-column:1/-1' : ''}"><div class="small muted">${esc(f[0])}</div>${esc(f[1])}</div>`).join('')}</div>${ats ? `<div style="padding:0 14px 10px">${ats}</div>` : ''}</div>`;
  }
  function ntPayload(c, m) {
    const sign = c.signed ? {timestamp: '1791331701', sign: 'sIgN…（HMAC-SHA256，已加签）'} : {};
    const real = m.ments.filter(x => x.id), ids = real.map(x => x.id);
    if (c.type === 'Slack') {
      const esc2 = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
      const at = m.ments.map(x => x.all ? '<!channel>' : x.raw ? (x.text[0] === 'S' ? `<!subteam^${x.text}>` : `<@${x.text}>`) : x.id ? `<@${x.id}>` : '@' + (x.name || x.text.replace(/^@/, ''))).join(' ');
      return {text: `${at ? at + ' ' : ''}*${esc2(m.title)}*\n${m.fields.map(f => `${esc2(f[0])}：${esc2(f[1])}`).join('\n')}`};
    }
    if (c.type === '钉钉') {
      const mob = ids.concat(m.ments.filter(x => x.raw).map(x => x.text));
      return {msgtype: 'markdown', markdown: {title: m.title, text: `### ${m.title}\n${m.fields.map(f => `- **${f[0]}**：${f[1]}`).join('\n')}${mob.length ? '\n\n' + mob.map(x => '@' + x).join(' ') : ''}`}, at: {atMobiles: mob, isAtAll: m.ments.some(x => x.all)}};
    }
    const at = m.ments.map(x => x.all ? '<at id=all></at>' : x.raw ? `<at id=${x.text}></at>` : x.id ? `<at id=${x.id}></at>` : '@' + (x.name || x.text.replace(/^@/, ''))).join(' ');
    return Object.assign({}, sign, {msg_type: 'interactive', card: {header: {title: {tag: 'plain_text', content: m.title}, template: m.color}, elements: [{tag: 'div', fields: m.fields.slice(0, 6).map(f => ({is_short: true, text: {tag: 'lark_md', content: `**${f[0]}**\n${f[1]}`}}))}].concat(at ? [{tag: 'div', text: {tag: 'lark_md', content: at}}] : [])}});
  }
  function ntPreviewHtml(c, ev, wh, src) {
    if (isMail(c)) return ntMailPreview(c, ntMsg(c, ev, wh, src), ev);
    const m = ntMsg(c, ev, wh, src), url = c.type === '钉钉' ? `POST https://${NT_TYPES['钉钉'].host}/robot/send?access_token=***${c.signed ? '&timestamp=1791331701&sign=***（已加签）' : ''}` : `POST https://${c.host || NT_TYPES[c.type].host}/…/${c.tail ? '***' + c.tail : '***'}`;
    return `<div class="cols-2"><div><div class="small muted" style="margin-bottom:6px">消息效果（${esc(c.type)}）</div>${ntRender(c.type, m)}</div><div style="min-width:0"><div class="small muted mono" style="margin-bottom:2px;word-break:break-all">${esc(url)}</div>${code(JSON.stringify(ntPayload(c, m), null, 2))}</div></div>
      ${m.skipped.length ? `<div style="margin-top:8px">${notice(`<b>未 @ 的规则：</b>${m.skipped.map(s => `<div class="small">· ${esc(s)}</div>`).join('')}`, 'warn', '⚠')}</div>` : ''}`;
  }

  /* ---- 邮件渠道：收件人解析 / 预览 / 列表单元格 / 编辑字段 / 测试发送 ---- */
  /* 收件人 = 固定 To + Cc + 规则解析出的邮箱（去重、没有邮箱的用户跳过并写明、最多 50 个） */
  function ntMailRcpts(c, ments, rgid) {
    const mc = c.mail, seen = new Set(), to = [], cc = [], noMail = [], notes = []; let dropped = 0;
    const add = (arr, a) => { const k = a.toLowerCase(); if (seen.has(k)) return; if (to.length + cc.length >= 50) { dropped++; return; } seen.add(k); arr.push(a); };
    mc.to.forEach(a => add(to, a)); mc.cc.forEach(a => add(cc, a));
    const person = u => { if (!u.email) noMail.push(u.name); else add(to, u.email); };
    ments.forEach(x => {
      if (x.all) USERS.filter(u => u.status === 'active' && u.rgs.includes(rgid)).forEach(person);
      else if (x.raw) { if (reAddr.test(x.text)) add(to, x.text); else notes.push(`原生标识 ${x.text} 不是邮箱地址`); }
      else if (x.email !== undefined) person({name: x.name, email: x.email});
    });
    const nm = [...new Set(noMail)];
    if (nm.length) notes.push(`${nm.length} 个用户没有邮箱，已跳过：${nm.join('、')}`);
    if (dropped) notes.push(`收件人超过 50 个上限，已丢弃 ${dropped} 个`);
    return {to, cc, noMail: nm, notes};
  }
  function ntMailCell(c) {
    const m = c.mail, n = m.to.length + m.cc.length;
    return `<span class="mono small">${esc(m.host)}:${m.port} · ${ntEncName(m.enc)}</span><div class="small ${m.user ? '' : 'muted'}">${m.user ? (m.hasPw ? '🔒 已设密码' : '未设密码') + ' · ' + esc(m.user) : '不认证'}</div>
      <div class="small muted">发件人 ${esc(m.from)} · 固定收件人 ${n} 个</div>${m.enc === 'none' ? `<div class="small p2-risk">明文传输${m.user ? '且明文认证' : ''}</div>` : !m.verify ? '<div class="small p2-risk">不校验证书</div>' : ''}`;
  }
  function ntMailPreview(c, m, ev) {
    const mc = c.mail, ml = m.mail, from = mc.fromName ? `${mc.fromName} <${mc.from}>` : mc.from, addrs = a => a.length ? a.map(esc).join('、') : '—';
    const head = `<div style="padding:10px 14px;border-bottom:1px solid var(--border);background:var(--panel);display:grid;grid-template-columns:56px minmax(0,1fr);gap:3px 8px;font-size:13px"><span class="small muted">主题</span><b>${esc(ml.subject)}</b><span class="small muted">发件人</span><span>${esc(from)}</span><span class="small muted">收件人</span><span>${addrs(ml.to)}</span>${ml.cc.length ? `<span class="small muted">抄送</span><span>${addrs(ml.cc)}</span>` : ''}</div>`;
    const body = `<div style="padding:10px 14px"><div style="font-weight:700;margin-bottom:6px">${esc(m.title)}</div><table style="border-collapse:collapse;width:100%">${m.fields.map(f => `<tr><td class="small muted" style="padding:2px 12px 2px 0;white-space:nowrap;vertical-align:top">${esc(f[0])}</td><td style="padding:2px 0;word-break:break-all">${esc(f[1])}</td></tr>`).join('')}</table></div>
      <div class="small muted" style="padding:6px 14px 10px;border-top:1px solid var(--border)">事件：${esc(ntEvName(ev))} · 渠道：${esc(c.name)} · 本邮件由 dbtool 自动发送，请勿直接回复。</div>`;
    const total = ml.to.length + ml.cc.length;
    const li = (a, extra) => `<div class="small">· ${esc(a)}${extra || ''}</div>`;
    const who = ml.to.map(a => li(a)).join('') + ml.cc.map(a => li(a, '（抄送）')).join('') + ml.noMail.map(n => `<div class="small" style="color:var(--warn)">· ${esc(n)}（没有邮箱，跳过）</div>`).join('');
    const payload = {from: mc.from, from_name: mc.fromName, to: ml.to, cc: ml.cc, subject: ml.subject, text: `${m.title}\n\n${m.fields.map(f => f[0] + '：' + f[1]).join('\n')}`};
    const smtp = `SMTP ${mc.host}:${mc.port} · ${ntEncName(mc.enc)} · ${mc.enc === 'none' ? '无证书' : mc.verify ? '校验证书' : '<span class="p2-risk">不校验证书</span>'} · ${mc.user ? '认证 PLAIN/LOGIN' : '不认证'}`;
    return `<div class="cols-2"><div><div class="small muted" style="margin-bottom:6px">邮件效果（邮件）</div><div class="p2-msg" style="padding:0;overflow:hidden">${head}${body}</div></div>
      <div style="min-width:0"><div class="small muted" style="margin-bottom:4px">将发送给谁（${total} 个收件人）</div>${who || '<div class="small muted">没有可用的收件人</div>'}<div class="small muted" style="margin:8px 0 4px">${smtp}</div>${code(JSON.stringify(payload, null, 2))}</div></div>
      ${total === 0 ? `<div style="margin-top:8px">${notice('<b>没有可用的收件人：</b>真实发送会失败。', 'bad', '⚠')}</div>` : ''}
      ${ml.notes.length || m.skipped.length ? `<div style="margin-top:8px">${notice(`<b>说明：</b>${ml.notes.concat(m.skipped).map(x => `<div class="small">· ${esc(x)}</div>`).join('')}`, 'warn', '⚠')}</div>` : ''}`;
  }
  function ntMailFields(cur, isMT) {
    const mc = cur.mail || {host: '', port: 587, enc: 'starttls', verify: true, user: '', hasPw: false, plain: false, from: '', fromName: '', to: [], cc: [], prefix: '[dbtool]'};
    return `<div id="nt_mailbox" style="display:${isMT ? 'contents' : 'none'}">
      ${fld('SMTP 主机', `<input class="mono" id="nt_m_host" value="${esc(mc.host)}" placeholder="smtp.example.com" autocomplete="off">`, {req: true})}
      ${fld('端口 / 加密方式', `<input id="nt_m_port" type="number" min="1" max="65535" value="${mc.port}" style="width:110px"> <select id="nt_m_enc" data-onchange="ntEnc">${NT_ENC.map(e => opt(e[0], e[1], mc.enc)).join('')}</select><div id="nt_m_nonebox" class="small p2-risk" style="margin-top:6px;display:${mc.enc === 'none' ? '' : 'none'}">邮件内容以明文传输。</div>`, {req: true})}
      ${fld('服务器证书', `<label class="chk"><input type="checkbox" id="nt_m_verify" data-onchange="ntEnc" ${mc.verify ? 'checked' : ''} ${mc.enc === 'none' ? 'disabled' : ''}> 校验服务器证书（建议开启）</label><div id="nt_m_vwarn" class="small p2-risk" style="margin-top:6px;display:${!mc.verify && mc.enc !== 'none' ? '' : 'none'}">已关闭证书校验：无法确认对方身份，存在被中间人窃听的风险。</div>`)}
      ${fld('SMTP 用户名（留空 = 不认证）', `<input class="mono" id="nt_m_user" value="${esc(mc.user)}" autocomplete="off">`)}
      ${fld('SMTP 密码', `<input type="password" id="nt_m_pw" placeholder="${mc.hasPw ? '已设置（留空不修改）' : ''}" autocomplete="new-password">${mc.hasPw ? '<label class="chk" style="margin-top:6px"><input type="checkbox" id="nt_m_clrpw"> 清除已有 SMTP 密码</label>' : ''}`)}
      ${fld('明文认证', `<div id="nt_m_plainbox" style="display:${mc.enc === 'none' ? '' : 'none'}"><label class="chk"><input type="checkbox" id="nt_m_plain" ${mc.plain ? 'checked' : ''}> 允许明文认证</label><div class="small p2-risk" style="margin-top:6px">用户名和密码将以明文发送，存在被窃听的风险。</div></div>`)}
      ${fld('发件人', `<input class="mono" id="nt_m_from" value="${esc(mc.from)}" placeholder="dbtool@example.com" style="width:55%"> <input id="nt_m_fname" value="${esc(mc.fromName)}" placeholder="显示名（可选）" maxlength="64" style="width:38%">`, {req: true})}
      ${fld('固定收件人 To（逗号分隔）', `<input class="mono" id="nt_m_to" value="${esc(mc.to.join(', '))}" placeholder="ops@example.com, dba@example.com">`)}
      ${fld('抄送 Cc（逗号分隔）',`<input class="mono" id="nt_m_cc" value="${esc(mc.cc.join(', '))}">`)}
      ${fld('主题前缀', `<input id="nt_m_prefix" value="${esc(mc.prefix)}" maxlength="50" placeholder="[dbtool]">`)}</div>`;
  }
  /* 测试发送：按配置和模拟的服务器行为，给出 连接 / STARTTLS / 认证 / 发送 四个阶段的结果（不含密码） */
  const NT_SIM = [['ok', '正常'], ['cert', '证书校验失败（自签名证书）'], ['auth', '认证失败（535）'], ['timeout', '连接超时（10 秒）'], ['nostarttls', '服务器不支持 STARTTLS']];
  function ntStages(c, sim, n) {
    const mc = c.mail, addr = `${mc.host}:${mc.port}`, st = ['连接', 'STARTTLS', '认证', '发送'].map(x => [x, 'skip', '未执行']);
    const fail = (i, d) => { st[i] = [st[i][0], 'fail', d]; return {ok: false, stages: st, err: d}; };
    if (mc.user && mc.enc === 'none' && !mc.plain) return fail(0, '加密方式为“不加密”，且未勾选“允许明文认证”：为避免密码明文传输，已拒绝连接');
    if (sim === 'timeout') return fail(0, `连接 ${addr} 失败：i/o timeout（10 秒）`);
    const certMsg = `证书校验失败（${addr}）：x509: certificate signed by unknown authority。如果是自签名证书，请让服务器使用受信任的证书；也可以在渠道里关闭“校验证书”，但这会使连接可被中间人窃听`;
    if (mc.enc === 'ssl' && sim === 'cert' && mc.verify) return fail(0, certMsg);
    st[0] = ['连接', 'ok', `已连接 ${addr}（${mc.enc === 'ssl' ? 'SSL/TLS 1.3' : '明文'}）`];
    if (mc.enc === 'starttls') {
      if (sim === 'nostarttls') return fail(1, '服务器没有通告 STARTTLS（为避免降级为明文，已中止）');
      if (sim === 'cert' && mc.verify) return fail(1, certMsg);
      st[1] = ['STARTTLS', 'ok', '已升级为 TLS 1.3'];
    } else st[1] = ['STARTTLS', 'skip', mc.enc === 'ssl' ? '不适用：已在连接时完成 SSL/TLS 握手' : '不适用：未启用加密'];
    if (mc.user) {
      if (sim === 'auth') return fail(2, '认证失败：535 5.7.8 Authentication credentials invalid');
      st[2] = ['认证', 'ok', '认证成功' + (mc.enc === 'none' ? '（明文连接，用户名和密码未加密传输）' : '')];
    } else st[2] = ['认证', 'skip', '未配置用户名，不认证'];
    st[3] = ['发送', 'ok', `已提交给服务器，${n} 个收件人`];
    return {ok: true, stages: st, err: ''};
  }
  const ntStagesHtml = (r, to) => `<div style="margin-top:10px"><b>${r.ok ? `测试邮件已发送${to ? '（' + esc(to) + '）' : ''}` : '测试发送失败'}</b><ul class="p2-stg">${r.stages.map(x => `<li><b>${x[0]}</b><span class="${x[1]}">${{ok: '通过', fail: '失败', skip: '跳过'}[x[1]]}</span><span class="small muted">${esc(x[2])}</span></li>`).join('')}</ul></div>`;
  ON.ntMailTest = () => { const c = NOTIFY.find(x => x.id === NT.test); const p = $('#ntPrev'); if (c && p) p.innerHTML = ntPreviewHtml(c, val('ntTEv'), val('ntTWh') === '1', 'sql'); };
  function ntMailTestModal(c) {
    NT.test = c.id; const ev0 = c.events[0] || 'pending';
    openModal({title: `测试发送 · ${c.name}`, wide: true, code: 'notify.test', ok: '发送测试消息', cancel: '关闭',
      body: `${c.enabled ? '' : notice('<b>该渠道当前已停用</b>，测试发送仍会发出。')}
        <div class="toolbar"><label class="small muted">模拟事件 <select id="ntTEv" data-onchange="ntMailTest">${NT_EV.map(e => opt(e[0], e[1] + (c.events.length && !c.events.includes(e[0]) ? '（未订阅）' : ''), ev0)).join('')}</select></label>
          <label class="small muted">模拟发送时间 <select id="ntTWh" data-onchange="ntMailTest">${opt('1', '工作时间（周三 14:30）', '1')}${opt('0', '非工作时间（周六 22:10）', '1')}</select></label>
          <label class="small muted">模拟服务器行为 <select id="ntTSim">${NT_SIM.map(x => opt(x[0], x[1], 'ok')).join('')}</select></label></div>
        <div class="form" style="margin:6px 0"><label>测试收件人</label><div><input class="mono" id="ntTTo" placeholder="单个邮箱地址（留空 = 按渠道设置）" autocomplete="off"></div></div>
        <div id="ntPrev">${ntPreviewHtml(c, ev0, true, 'sql')}</div><div id="ntStages"></div>`,
      onOk() {
        const ev = val('ntTEv'), to = val('ntTTo'), m = ntMsg(c, ev, val('ntTWh') === '1', 'sql');
        if (to && !reAddr.test(to)) { toast('测试收件人：邮箱地址格式不正确（只填一个地址）'); return false; }
        const n = to ? 1 : m.mail.to.length + m.mail.cc.length;
        let r;
        if (!n) r = {ok: false, err: '没有可用的收件人', stages: ['连接', 'STARTTLS', '认证', '发送'].map(x => [x, 'skip', '未执行'])};
        else r = ntStages(c, val('ntTSim'), n);
        c.last = now(); c.lastOk = r.ok; NT_LOG.unshift([c.last, c.id, ev, r.ok, r.ok ? (m.mail.noMail.length && !to ? `${m.mail.noMail.length} 个用户没有邮箱，已跳过：${m.mail.noMail.join('、')}` : '') : r.err]);
        audit('notify.test', `测试发送到通知渠道「${c.name}」（邮件，SMTP ${c.mail.host}:${c.mail.port}，测试收件人 ${to || '渠道固定收件人与 @ 规则'}，事件：${ntEvName(ev)}）`, r.ok ? '成功' : '失败', c.type);
        $('#ntStages').innerHTML = ntStagesHtml(r, to); toast(r.ok ? `测试邮件已发送（模拟），${n} 个收件人` : '测试发送失败：' + r.err);
        return false;
      }});
  }

  /* ---- 列表 / 筛选 ---- */
  function ntListHtml() {
    const q = NT.q.trim().toLowerCase();
    const rows = NOTIFY.filter(c => (!NT.type || c.type === NT.type) && (!NT.rg || (NT.rg === 'global' ? c.global : c.rgs.includes(+NT.rg))) && (!q || (c.name + c.host).toLowerCase().includes(q)));
    return tbl(['名称', '类型', '关联资源组', '机器人 / SMTP', '订阅事件', '@ 规则', '状态', '最近发送', '操作'], rows, {
      cells: c => [`<b>${esc(c.name)}</b>`, `<span class="tag brand">${esc(c.type)}</span>`, c.global ? '<span class="tag info">全局</span> <span class="small muted">所有资源组</span>' : `<span class="small">${esc(ntRgText(c))}</span>`,
        isMail(c) ? ntMailCell(c) : `<span class="mono small">${esc(c.host)}/…${esc(c.tail)}</span><div class="small ${c.signed ? '' : 'muted'}">${c.signed ? '🔒 已加签' : '未加签'}</div>`,
        c.events.length ? `<span title="${esc(c.events.map(ntEvName).join('、'))}">${c.events.length} 项</span><div class="small muted">${esc(c.events.slice(0, 2).map(ntEvName).join('、'))}${c.events.length > 2 ? '…' : ''}</div>` : '<span class="muted">全部事件</span>', `${c.rules.length} 条`,
        c.enabled ? '<span class="tag ok">启用</span>' : '<span class="tag">停用</span>', c.last ? `<span class="small mono">${c.last}</span><div>${c.lastOk ? '<span class="tag ok">成功</span>' : '<span class="tag bad">失败</span>'}</div>` : '<span class="muted small">从未发送</span>',
        `<div class="ops">${B('notify.edit', '编辑', {cls: 'sm', data: {id: c.id}})}${B('notify.test', '测试发送', {cls: 'sm', data: {id: c.id}})}${B('notify.edit', c.enabled ? '停用' : '启用', {cls: 'sm', data: {id: c.id, op: 'toggle'}})}${B('notify.edit', '删除', {cls: 'sm danger', data: {id: c.id, op: 'del'}})}</div>`]
    });
  }
  ON.ntF = (d, el) => { NT[d.k] = el.value; const b = $('#ntList'); if (b) b.innerHTML = ntListHtml(); };
  ON.ntPv = d => { NT.pv = d.v; rerender(); };
  pages.notify = {
    render() {
      const on = NOTIFY.filter(c => c.enabled).length, covered = new Set(); NOTIFY.filter(c => c.enabled).forEach(c => c.rgs.forEach(r => covered.add(r)));
      const uncovered = RGROUPS.filter(r => r.enabled && !covered.has(r.id)), evCover = new Set(); NOTIFY.filter(c => c.enabled).forEach(c => (c.events.length ? c.events : NT_EV.map(e => e[0])).forEach(e => evCover.add(e)));
      const failN = NT_LOG.filter(x => !x[3]).length, pv = {id: 0, type: NT.pv, name: '预览', rgs: [1], global: false, host: NT_TYPES[NT.pv].host, tail: '', signed: true, mail: NT.pv === '邮件' ? {...NT_MAIL_DEF} : null, events: [], rules: [{t: 'applicant', v: '', ev: [], wh: false}, {t: 'reviewers', v: '', ev: ['pending', 'submitted'], wh: false}]};
      return CSS2 + `
      <div class="grid g4" style="margin-bottom:16px">
        <div class="card kpi"><div class="l">渠道</div><div class="v">${NOTIFY.length}</div><div class="d flat">启用 ${on} · 停用 ${NOTIFY.length - on}</div></div>
        <div class="card kpi"><div class="l">资源组覆盖</div><div class="v">${RGROUPS.filter(r => r.enabled).length - uncovered.length} / ${RGROUPS.filter(r => r.enabled).length}</div><div class="d ${uncovered.length ? 'up' : 'down'}">${uncovered.length ? '未覆盖：' + esc(uncovered.map(r => r.name).join('、')) : '全部有启用的渠道'}</div></div>
        <div class="card kpi"><div class="l">事件覆盖</div><div class="v">${evCover.size} / ${NT_EV.length}</div></div>
        <div class="card kpi"><div class="l">近期发送</div><div class="v">${NT_LOG.length}</div><div class="d ${failN ? 'up' : 'down'}">失败 ${failN} 条</div></div></div>
      <div class="toolbar"><select data-onchange="ntF" data-k="type"><option value="">全部类型</option>${Object.keys(NT_TYPES).map(t => opt(t, t, NT.type)).join('')}</select>
        <select data-onchange="ntF" data-k="rg"><option value="">全部资源组</option><option value="global" ${NT.rg === 'global' ? 'selected' : ''}>全局渠道</option>${RGROUPS.filter(r => r.enabled).map(r => opt(r.id, r.name, NT.rg)).join('')}</select>
        <input type="search" placeholder="搜索 渠道名称 / 机器人或 SMTP 主机" data-oninput="ntF" data-k="q" value="${esc(NT.q)}" style="min-width:220px"><span class="sp"></span>${B('notify.edit', '＋ 新增渠道', {cls: 'primary', data: {id: 'new'}})}</div>
      <div class="card" id="ntList">${ntListHtml()}</div>
      <div class="grid g2" style="margin-top:16px">
        <div class="card"><h2>消息效果预览<span class="right">${subtabs(Object.keys(NT_TYPES).map(t => [t, t]), NT.pv, 'ntPv')}</span></h2>${ntPreviewHtml(pv, 'pending', true)}</div>
        <div class="card"><h2>最近发送记录</h2>${tbl(['时间', '渠道', '事件', '结果'], NT_LOG.slice(0, 9), {cells: x => { const c = NOTIFY.find(y => y.id === x[1]); return [`<span class="mono small">${x[0]}</span>`, esc(c ? c.name : '#' + x[1]), esc(ntEvName(x[2])), x[3] ? `<span class="tag ok">成功</span>${x[4] ? `<div class="small muted">${esc(x[4])}</div>` : ''}` : `<span class="tag bad" title="${esc(x[4])}">失败</span><div class="small muted">${esc(x[4])}</div>`]; }})}
          </div></div>`;
    }
  };

  /* ---- 新建 / 编辑 ---- */
  function ntRuleHtml() {
    return NT.rules.length ? NT.rules.map((r, i) => {
      const ty = NT.type_ || 'Slack';
      const valCtl = r.t === 'auth_group' ? `<select data-nr="v">${GROUPS.map(g => opt(g.id, g.name, r.v)).join('')}</select>` : r.t === 'user' ? `<select data-nr="v">${USERS.filter(u => u.status === 'active').map(u => opt(u.login, u.login + '（' + u.name + '）', r.v)).join('')}</select>`
        : r.t === 'raw' ? `<input class="mono" data-nr="v" value="${esc(r.v)}" placeholder="${esc(NT_TYPES[ty].raw)}">` : `<span class="small muted">${r.t === 'all' ? (ty === '邮件' ? '资源组的全部启用用户' : '渠道里的所有人') : r.t === 'dba_duty' ? '今日：李 DBA' : r.t === 'reviewers' ? '当前审批级' : '工单提交人'}</span>`;
      return `<div class="p2-rule" data-ri="${i}"><div><select data-nr="t" data-onchange="ntRuleType" data-i="${i}">${NT_TARGETS.map(t => opt(t[0], ntTargetLabel(t, ty), r.t)).join('')}</select></div>
        <div>${valCtl}<div class="p2-wsel" style="margin-top:6px"><span class="small muted">仅在：</span><select multiple size="3" data-nr="ev" style="min-width:150px">${NT_EV.map(e => `<option value="${e[0]}" ${r.ev.includes(e[0]) ? 'selected' : ''}>${e[1]}</option>`).join('')}</select></div>
          <label class="chk" style="margin-top:6px"><input type="checkbox" data-nr="wh" ${r.wh ? 'checked' : ''}> 仅工作时间 @</label></div>
        <div><button class="btn sm danger" data-on="ntRuleDel" data-i="${i}">删除</button></div></div>`;
    }).join('') : '<div class="small muted" style="padding:6px 0">暂无 @ 规则</div>';
  }
  function ntSync() {
    $$('#ntRules .p2-rule').forEach(row => {
      const r = NT.rules[+row.dataset.ri]; if (!r) return; const g = k => row.querySelector(`[data-nr=${k}]`);
      r.t = g('t').value; r.v = g('v') ? g('v').value.trim() : ''; r.ev = [...g('ev').selectedOptions].map(o => o.value); r.wh = g('wh').checked;
    });
  }
  ON.ntRuleAdd = () => { ntSync(); NT.rules.push({t: 'applicant', v: '', ev: [], wh: false}); $('#ntRules').innerHTML = ntRuleHtml(); };
  ON.ntRuleDel = d => { ntSync(); NT.rules.splice(+d.i, 1); $('#ntRules').innerHTML = ntRuleHtml(); };
  ON.ntRuleType = d => { ntSync(); const r = NT.rules[+d.i]; r.v = r.t === 'auth_group' ? GROUPS[0].id : r.t === 'user' ? 'dba_li' : ''; $('#ntRules').innerHTML = ntRuleHtml(); };
  ON.ntType = (d, el) => {
    ntSync(); NT.type_ = el.value; $('#ntRules').innerHTML = ntRuleHtml(); const h = $('#nt_hook'); if (h) h.placeholder = NT_TYPES[el.value].ph;
    const mail = !!NT_TYPES[el.value].email, hb = $('#nt_hookbox'), mb = $('#nt_mailbox'); if (hb) hb.style.display = mail ? 'none' : 'contents'; if (mb) mb.style.display = mail ? 'contents' : 'none'; if (mail) ON.ntEnc();
  };
  /* 加密方式 / 证书校验联动：端口跟随常用端口（只替换没改过的默认值）；不加密才出现“允许明文认证”；关闭校验出现风险提示 */
  ON.ntEnc = () => {
    const enc = val('nt_m_enc'), port = $('#nt_m_port'), def = (NT_ENC.find(e => e[0] === enc) || NT_ENC[0])[2];
    if (port && (!port.value || NT_ENC.some(e => String(e[2]) === port.value))) port.value = def;
    const pb = $('#nt_m_plainbox'), vw = $('#nt_m_vwarn'), vf = $('#nt_m_verify'), nb = $('#nt_m_nonebox');
    if (pb) pb.style.display = enc === 'none' ? '' : 'none';
    if (nb) nb.style.display = enc === 'none' ? '' : 'none';
    if (vf) vf.disabled = enc === 'none';
    if (vw) vw.style.display = (!chk('nt_m_verify') && enc !== 'none') ? '' : 'none';
  };
  function ntEditModal(c) {
    const isNew = !c, cur = c || {id: 0, name: '', type: 'Slack', enabled: true, rgs: [], global: false, host: '', tail: '', signed: false, mail: null, events: [], rules: [{t: 'applicant', v: '', ev: [], wh: false}]};
    const isMT = cur.type === '邮件';
    NT.rules = cur.rules.map(r => ({...r, ev: r.ev.slice()})); NT.type_ = cur.type;
    openModal({title: isNew ? '新增通知渠道' : `编辑通知渠道 · ${cur.name}`, wide: true, code: 'notify.edit', ok: '保存',
      body: `<div class="form">
        ${fld('类型', `<select id="nt_type" data-onchange="ntType">${Object.keys(NT_TYPES).map(t => opt(t, t, cur.type)).join('')}</select>`, {req: true})}
        ${fld('名称', `<input id="nt_name" value="${esc(cur.name)}" maxlength="50" placeholder="如：订单中心 · DBA 值班群">`, {req: true})}
        ${fld('状态', sw('nt_on', cur.enabled, '启用'))}
        ${fld('关联资源组', `<label class="chk"><input type="checkbox" id="nt_global" ${cur.global ? 'checked' : ''}> 全局（对所有资源组生效）</label><div class="p2-ev" style="margin-top:6px">${RGROUPS.filter(r => r.enabled).map(g => `<label class="chk"><input type="checkbox" class="nt_rg" value="${g.id}" ${!cur.global && cur.rgs.includes(g.id) ? 'checked' : ''}>${esc(g.name)}</label>`).join('')}</div>`, {req: true})}
        <div id="nt_hookbox" style="display:${isMT ? 'none' : 'contents'}">${fld('Webhook 地址', `<input class="mono" id="nt_hook" placeholder="${isNew ? esc(NT_TYPES[cur.type].ph) : `已设置：${esc(cur.host)}/…${esc(cur.tail)}（留空不修改）`}" autocomplete="off">`, {req: isNew})}
        ${fld('签名密钥', `<input type="password" id="nt_sign" placeholder="${cur.signed ? '已设置（留空不修改）' : '开启“加签”时填写，可留空'}" autocomplete="new-password">${cur.signed ? '<label class="chk" style="margin-top:6px"><input type="checkbox" id="nt_clrsign"> 清除已有签名密钥</label>' : ''}`)}</div>${ntMailFields(cur, isMT)}
        ${fld('订阅事件（不选 = 全部）', `<div class="p2-ev">${NT_EV.map(e => `<label class="chk"><input type="checkbox" class="nt_ev" value="${e[0]}" ${cur.events.includes(e[0]) ? 'checked' : ''}>${e[1]}</label>`).join('')}</div>`)}
        <label>@ 规则</label><div><div id="ntRules">${ntRuleHtml()}</div><button class="btn sm" data-on="ntRuleAdd" style="margin-top:8px">＋ 添加 @ 规则</button></div></div>`,
      onOk() {
        ntSync();
        const type = val('nt_type'), name = val('nt_name'), global = chk('nt_global'), rgs = checkedVals('.nt_rg').map(Number), hook = val('nt_hook'), sign = $('#nt_sign').value, T = NT_TYPES[type];
        if (!name) { toast('请填写名称'); return false; } if (NOTIFY.some(x => x !== c && x.name === name)) { toast('名称已存在'); return false; }
        if (!global && !rgs.length) { toast('请选择关联的资源组，或勾选“全局”'); return false; }
        if (isNew && !hook && !T.email) { toast('请填写 Webhook 地址'); return false; }
        if (!T.email && hook && !new RegExp('^https://' + T.host.replace(/\./g, '\\.') + '/').test(hook)) { toast(`${type} 的 Webhook 应以 https://${T.host}/ 开头`); return false; }
        if (!isNew && !T.email && cur.type !== type && !hook) { toast('更换类型后需要重新填写 Webhook 地址'); return false; }
        if (NT.rules.length > 20) { toast('@ 规则最多 20 条'); return false; }
        for (const r of NT.rules) {
          if (r.t === 'raw' && T.email) { if (!reAddr.test(r.v)) { toast('“邮箱地址”规则需要填写合法的邮箱地址'); return false; } }
          else if (r.t === 'raw') { if (!r.v) { toast('“渠道原生 ID”规则需要填写 ID'); return false; } if (!/^[A-Za-z0-9_.:@-]+$/.test(r.v)) { toast('原生 ID 只允许字母数字与 _ . : @ -'); return false; } if (!T.rawRe.test(r.v)) { toast(`原生 ID 格式不符：${type} 需要 ${T.raw}`); return false; } }
        }
        let mail = null, risk = [];
        if (T.email) {
          const m = {host: val('nt_m_host'), port: +val('nt_m_port'), enc: val('nt_m_enc'), verify: chk('nt_m_verify'), user: val('nt_m_user'), plain: chk('nt_m_plain'), from: val('nt_m_from'), fromName: val('nt_m_fname'),
            to: ntAddrs(val('nt_m_to')), cc: ntAddrs(val('nt_m_cc')), prefix: val('nt_m_prefix')}, pw = $('#nt_m_pw').value;
          if (!m.host) { toast('请填写 SMTP 主机'); return false; }
          if (!/^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$/.test(m.host) || m.host.length > 253) { toast('SMTP 主机不合法（只允许主机名或 IP，不要带 smtp:// 和端口）'); return false; }
          if (!(m.port >= 1 && m.port <= 65535)) { toast('端口需要在 1 到 65535 之间'); return false; }
          if (m.enc !== 'none') m.plain = false;
          if (m.enc === 'none') m.verify = true;
          if (m.user && m.enc === 'none' && !m.plain) { toast('加密方式为“不加密”时默认不允许认证（用户名和密码会明文传输）：请改用 STARTTLS / SSL，或勾选“允许明文认证”'); return false; }
          if (pw && !m.user) { toast('填写了密码但没有填写用户名'); return false; }
          if (/[\r\n]/.test(m.user + pw + m.fromName + m.prefix + m.host)) { toast('SMTP 配置里不能含换行'); return false; }
          if (m.user.length > 200 || pw.length > 200) { toast('用户名 / 密码最多 200 个字符'); return false; }
          if (!reAddr.test(m.from)) { toast('发件人地址格式不正确'); return false; }
          if ([...m.fromName].length > 64) { toast('发件人显示名最多 64 个字符'); return false; }
          if ([...m.prefix].length > 50) { toast('主题前缀最多 50 个字符'); return false; }
          m.to = ntDedup(m.to); m.cc = ntDedup(m.cc).filter(x => !m.to.some(t => t.toLowerCase() === x.toLowerCase()));
          const bad = m.to.concat(m.cc).find(a => !reAddr.test(a)); if (bad) { toast(`收件地址格式不正确：${bad}`); return false; }
          if (m.to.length + m.cc.length > 20) { toast('固定收件人（To 与 Cc 合计）最多 20 个'); return false; }
          m.hasPw = pw ? true : chk('nt_m_clrpw') ? false : (c && c.type === '邮件' && c.mail ? c.mail.hasPw : false);
          m.pwChanged = pw ? '已更新' : chk('nt_m_clrpw') ? '已清除' : '未改';
          mail = m;
          if (m.enc !== 'none' && !m.verify) risk.push('已关闭证书校验：无法确认对方是真正的邮件服务器，邮件内容和 SMTP 密码可能被中间人窃听。');
          if (m.enc === 'none') risk.push(m.user ? '加密方式为“不加密”且启用了认证：邮件内容、用户名和密码都以明文在网络上传输。' : '加密方式为“不加密”：邮件内容以明文在网络上传输。');
        }
        const events = checkedVals('.nt_ev'), ch = [];
        const apply = () => {
        const nc = Object.assign(c || {id: Math.max(0, ...NOTIFY.map(x => x.id)) + 1, last: '', lastOk: true}, {name, type, enabled: chk('nt_on'), global, rgs: global ? allRgIds() : rgs, events, rules: NT.rules.map(r => ({...r, ev: r.ev.slice()}))});
        let mailTxt = '';
        if (mail) {
          nc.mail = {host: mail.host, port: mail.port, enc: mail.enc, verify: mail.verify, user: mail.user, hasPw: mail.hasPw, plain: mail.plain, from: mail.from, fromName: mail.fromName, to: mail.to, cc: mail.cc, prefix: mail.prefix};
          nc.host = mail.host; nc.tail = ''; nc.signed = mail.hasPw;
          ch.push(`SMTP ${mail.host}:${mail.port} ${ntEncName(mail.enc)}${mail.enc !== 'none' && !mail.verify ? '，不校验证书' : ''}${mail.plain ? '，允许明文认证' : ''}`, `认证用户 ${mail.user || '（无）'}`, `发件人 ${mail.from}`, `固定收件人 ${mail.to.length + mail.cc.length} 个`, `密码 ${mail.pwChanged}`);
          mailTxt = `SMTP ${mail.host}:${mail.port} ${ntEncName(mail.enc)}；认证用户 ${mail.user || '（无）'}；发件人 ${mail.from}；固定收件人 ${mail.to.length + mail.cc.length} 个；密码 ${mail.pwChanged}`;
        } else {
          delete nc.mail;
          if (hook) { nc.host = T.host; nc.tail = hook.slice(-4); ch.push('Webhook 已更新'); } else if (isNew) return false;
          if (sign) { nc.signed = true; ch.push('签名密钥已更新'); } else if (chk('nt_clrsign')) { nc.signed = false; ch.push('签名密钥已清除'); } else if (isNew) nc.signed = false;
        }
        if (isNew) { NOTIFY.push(nc); audit('notify.edit', `新增通知渠道「${name}」（${type}，${ntRgText(nc)}，订阅 ${events.length ? events.length + ' 项事件' : '全部事件'}，${nc.rules.length} 条 @ 规则${mailTxt ? '；' + mailTxt : ''}）`); toast(`已新增渠道「${name}」`); }
        else { audit('notify.edit', `修改通知渠道「${name}」：${ch.concat([`订阅 ${events.length ? events.map(ntEvName).join('、') : '全部事件'}`, `${nc.rules.length} 条 @ 规则`]).join('；')}（${mail ? '密码不记录' : '地址与密钥不记录'}）`); toast(`已保存渠道「${name}」`); }
        rerender();
        };
        if (risk.length) { risky('notify.edit', '保存存在传输安全风险的邮件渠道', risk.map(esc).join('<br>') + '<br>确认要保存吗？', '', apply); return false; }
        apply();
      }});
  }
  ACTIONS['notify.edit'] = d => {
    if (d.id === 'new') return ntEditModal(null);
    const c = NOTIFY.find(x => x.id == d.id); if (!c) return;
    if (d.op === 'toggle') {
      const to = !c.enabled;
      return openModal({title: `${to ? '启用' : '停用'}通知渠道「${c.name}」`, ok: to ? '确认启用' : '确认停用', okCls: to ? 'primary' : 'danger primary', code: 'notify.edit',
        body: notice(to ? '启用后，该渠道会按订阅的事件开始发送通知。' : '停用后，该渠道不再收到任何事件通知；配置保留。', to ? '' : 'warn', to ? 'ℹ' : '⚠'),
        onOk() { c.enabled = to; audit('notify.edit', `${to ? '启用' : '停用'}通知渠道「${c.name}」`); toast(`已${to ? '启用' : '停用'}「${c.name}」`); rerender(); }});
    }
    if (d.op === 'del') return openModal({title: `删除通知渠道「${c.name}」`, ok: '删除', okCls: 'danger primary', code: 'notify.edit', body: notice(`删除后该渠道不再收到任何通知（${ntRgText(c)}），${isMail(c) ? 'SMTP 配置与密码' : 'Webhook 与签名密钥'}一并清除。`, 'bad', '⚠'),
      onOk() { NOTIFY.splice(NOTIFY.indexOf(c), 1); audit('notify.edit', `删除通知渠道「${c.name}」（${c.type}）`); toast('已删除'); rerender(); }});
    ntEditModal(c);
  };

  /* ---- 测试发送 ---- */
  ON.ntTest = () => { const c = NOTIFY.find(x => x.id === NT.test); const p = $('#ntPrev'); if (c && p) p.innerHTML = ntPreviewHtml(c, val('ntTEv'), val('ntTWh') === '1', val('ntTSrc')); };
  ACTIONS['notify.test'] = d => {
    const c = NOTIFY.find(x => x.id == d.id); if (!c) return;
    if (isMail(c)) return ntMailTestModal(c);
    NT.test = c.id; const ev0 = c.events[0] || 'pending';
    openModal({title: `测试发送 · ${c.name}`, wide: true, code: 'notify.test', ok: '发送测试消息',
      body: `${c.enabled ? '' : notice('<b>该渠道当前已停用</b>，测试发送仍会发出。')}
        <div class="toolbar"><label class="small muted">模拟事件 <select id="ntTEv" data-onchange="ntTest">${NT_EV.map(e => opt(e[0], e[1] + (c.events.length && !c.events.includes(e[0]) ? '（未订阅）' : ''), ev0)).join('')}</select></label>
          <label class="small muted">消息来源 <select id="ntTSrc" data-onchange="ntTest">${opt('sql', 'SQL 上线 / 权限 / 性能 / 巡检', 'sql')}${opt('archive', '数据归档', 'sql')}</select></label>
          <label class="small muted">模拟发送时间 <select id="ntTWh" data-onchange="ntTest">${opt('1', '工作时间（周三 14:30）', '1')}${opt('0', '非工作时间（周六 22:10）', '1')}</select></label></div>
        <div id="ntPrev">${ntPreviewHtml(c, ev0, true, 'sql')}</div>`,
      onOk() {
        const ev = val('ntTEv'), src = val('ntTSrc'), m = ntMsg(c, ev, val('ntTWh') === '1', src), ok = true;
        c.last = now(); c.lastOk = ok; NT_LOG.unshift([c.last, c.id, ev, ok, '']);
        audit('notify.test', `测试发送到通知渠道「${c.name}」（事件：${ntEvName(ev)}${src === 'archive' && NT_ARC[ev] ? '，数据归档消息' : ''}，@ ${m.ments.length} 个目标）`, '成功', c.type);
        toast(`已向「${c.name}」发送测试消息（模拟），@ ${m.ments.length} 个目标`); rerender();
      }});
  };
})();
