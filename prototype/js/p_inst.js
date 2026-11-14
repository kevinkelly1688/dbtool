/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_inst.js
 * @time: 2026/10/28
 */
/* ============================================================================
   实例管理（6 个菜单页）：instlist 实例列表 · sessions 会话管理 · databases 数据库管理
   · accounts 账号管理 · params 参数配置 · inspect 性能巡检
   行为依据：docs/user-guide.md §10-11、internal/instmgr、internal/inspect
   原则：所有改变数据的操作 → 权限码按钮 → risky 二次确认 → audit() → rerender()；密码从不进入日志 / 审计 / 界面。
   导出：window.INSPECT_REPORTS（首页读取）。
   ========================================================================== */
(function () {
'use strict';

/* ---------- 通用小工具 ---------- */
const NOW0 = TODAY + ' 11:20';                                   // 原型里的“此刻”（调度 / 下次运行 / 时间推算的基准）
const later = fn => setTimeout(fn, 0);                           // onOk 里要再开弹窗：必须延后，否则会被 closeModal 关掉
const T0 = Date.now();
const clk = () => { const d = new Date(NOW0.replace(' ', 'T') + ':00'); d.setTime(d.getTime() + Date.now() - T0); return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`; };   // 新记录的时间戳：以原型“此刻”为起点走表
const isMy = i => i.type === 'MySQL' || i.type === 'MariaDB';
const part = (id, html) => { const el = $('#' + id); if (el) el.innerHTML = html; };
const fv = id => { const el = $('#' + id); return el ? String(el.value).trim() : ''; };
const checkedVals = cls => $$('.' + cls).filter(x => x.checked).map(x => x.value);
const instById = id => INSTANCES.find(i => i.id == id);
const TYPE_NAME = {MsSQL: 'SQL Server', PgSQL: 'PostgreSQL', Mongo: 'MongoDB'};
const typeName = t => TYPE_NAME[t] || t;
const typeTag = t => t === 'Redis' ? '<span class="tag" style="border-color:var(--bad);color:var(--bad);background:transparent">Redis</span>' : t === 'PgSQL' ? tag('PostgreSQL', 'info') : t === 'Mongo' ? tag('MongoDB', 'ok') : t === 'ClickHouse' ? tag('ClickHouse', 'warn') : tag(t === 'MsSQL' ? 'SQL Server' : t, t === 'MsSQL' ? 'info' : t === 'MariaDB' ? 'brand' : '');
const errBox = id => `<div id="${id}" class="small" style="color:var(--bad);min-height:18px;margin-top:8px"></div>`;
const setErr = (id, msg) => { const el = $('#' + id); if (el) el.textContent = msg || ''; return false; };
const opts = (list, cur) => list.map(([v, t]) => `<option value="${esc(v)}" ${String(v) === String(cur) ? 'selected' : ''}>${esc(t)}</option>`).join('');
const unsupported = (i, what) => notice(`暂不支持 <b>${esc(typeName(i.type))}</b> 类型实例的${what}。`, 'warn', '!');
const connFail = i => notice(`无法连接目标实例 <b>${esc(i.name)}</b>（${esc(i.host)}:${i.port}）：连接超时（10 秒）。`, 'bad', '!');
/* 置灰按钮（业务规则禁止，区别于无权限）；无权限时仍走 B() 的统一表现 */
const bx = (code, label, o, why) => why && can(code) ? `<button class="btn ${(o && o.cls) || ''}" disabled style="opacity:.45;cursor:not-allowed" title="${esc(why)}">${label}</button>` : B(code, label, o);
const confirmBox = (code, title, body, fn, ok) => openModal({title, body, ok: ok || '确定', code, onOk() { fn(); }});
const ago = sec => { const d = new Date(NOW0.replace(' ', 'T') + ':00'); d.setSeconds(d.getSeconds() - sec); return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`; };
const R = (r, a, b) => a + Math.floor(r() * (b - a + 1));
const plat = login => (USERS.find(u => u.login === login) || {name: login}).name;
const activeUsers = () => USERS.filter(u => u.status === 'active');
const trunc = (s, n) => s && s.length > n ? s.slice(0, n) + ' …' : (s || '');
const DEFAULT_VER = {MySQL: '8.0.35', MariaDB: '10.6.16', MsSQL: '2019 (15.0.4153)', Redis: '7.0.15', PgSQL: '15.4', Mongo: '6.0.12', ClickHouse: '23.8.9.54', Doris: '2.1.7', SelectDB: '3.0.4'};
const DEFAULT_PORT = {MySQL: 3306, MariaDB: 3306, MsSQL: 1433, Redis: 6379, PgSQL: 5432, Mongo: 27017, ClickHouse: 8123, Doris: 9030, SelectDB: 9030};
/* “支持范围”：新增类型第一期只有在线查询和数据字典 */
const scopeLabel = i => isExtInst(i) ? EXT_SCOPE : i.type === 'Redis' ? '在线查询、SQL 上线、权限申请、性能巡检、慢查日志' : i.type === 'MsSQL' ? 'SQL 上线、在线查询、数据字典、性能巡检' : '全部功能';

/* 巡检标签（计划可按标签动态匹配）：给种子实例补上与原系统一致的 mysql_inspection / mssql_inspection（幂等） */
INSTANCES.forEach(i => {
  const t = i.type === 'MsSQL' ? 'mssql_inspection' : (i.type === 'MySQL' && i.env === '生产' && i.role === 'master') ? 'mysql_inspection' : '';
  if (t && !i.tags.includes(t)) i.tags.push(t);
});
const allTags = () => [...new Set(INSTANCES.flatMap(i => i.tags))];

/* ============================================================================
   1. 实例列表 instlist
   ========================================================================== */
const IM = {role: '', type: '', tag: '', st: '', q: ''};
const SSH_TUNNELS = ['jump-prod（10.1.0.5:22）', 'jump-dw（10.1.0.6:22）'];
const canManageInst = () => ME.super || can('instance.add') || can('instance.edit') || can('instance.delete');
/* 可见性：能新增 / 修改 / 删除实例的人看全部（含已停用），其余按资源组可见 */
const imList = () => canManageInst() ? INSTANCES : visibleInstances('all');
function imFiltered() {
  const q = IM.q.toLowerCase();
  return imList().filter(i => (!IM.role || i.role === IM.role) && (!IM.type || i.type === IM.type) && (!IM.tag || i.tags.includes(IM.tag)) &&
    (!IM.st || (IM.st === 'disabled' ? !i.enabled : IM.st === 'slow_on' ? i.slowlog : IM.st === 'slow_off' ? !i.slowlog : i.enabled && i.status === IM.st)) &&
    (!q || (i.name + ' ' + i.host).toLowerCase().includes(q)));
}
function imTable() {
  return tbl(['名称', '类型 / 版本', '支持范围', '角色', '地址', '标签', '资源组', '状态', 'QPS / 连接', '慢日志采集', '操作'], imFiltered(), {cells: i => [
    `<a data-on="imView" data-id="${i.id}" style="white-space:nowrap"><b>${esc(i.name)}</b></a>${i.ssh ? ' <span class="tag info" title="经 SSH 隧道连接">隧道</span>' : ''}`,
    `${typeTag(i.type)} <span class="small muted">${esc(i.ver)}</span>`,
    isExtInst(i) ? `<span class="small">${esc(scopeLabel(i))}</span>` : `<span class="small muted">${esc(scopeLabel(i))}</span>`,
    i.role === 'master' ? tag('主库', 'brand') : tag('从库'),
    `<span class="mono small nowrap">${esc(i.host)}:${i.port}</span>`,
    i.tags.map(t => tag(t, t === 'read_view' ? 'warn' : '')).join(' ') || '<span class="muted">—</span>',
    i.rgs.map(r => esc(rgName(r))).join('、') || '<span class="muted">—</span>',
    !i.enabled ? dot('', '已停用') : i.status === 'ok' ? dot('ok', '正常') : i.status === 'warn' ? dot('warn', '告警') : dot('bad', '连接失败'),
    i.enabled && i.status !== 'bad' ? `${fmt(i.qps)} / ${i.conns}` : '<span class="muted">—</span>',
    isExtInst(i) ? '<span class="muted">暂不支持</span>' : B('instance.edit', i.slowlog ? '● 采集中' : '○ 未采集', {cls: 'sm', data: {id: i.id, op: 'slow'}}),
    `<div class="ops">${B('instance.test', '测试连接', {cls: 'sm', data: {id: i.id}})}${B('instance.edit', '编辑', {cls: 'sm', data: {id: i.id, op: 'edit'}})}${B('instance.edit', i.enabled ? '停用' : '启用', {cls: 'sm', data: {id: i.id, op: 'enable'}})}${
      i.type === 'MySQL' ? bx('instance.purge_binlog', 'Binlog 清理', {cls: 'sm', data: {id: i.id}}, ME.super ? '' : '仅超级用户可用') : ''}${B('instance.delete', '删除', {cls: 'sm danger', data: {id: i.id}})}</div>`]});
}
pages.instlist = {
  render() {
    const all = imList(), c = f => all.filter(f).length;
    return `<div class="card"><h2>实例列表<span class="right">${B('instance.add', '＋ 添加实例', {cls: 'primary'})}</span></h2>
      <div class="stat" style="margin-bottom:14px"><div><b>${all.length}</b><span>实例</span></div><div><b>${c(i => i.enabled && i.status === 'ok')}</b><span>正常</span></div>
        <div><b style="color:var(--warn)">${c(i => i.enabled && i.status === 'warn')}</b><span>告警</span></div><div><b style="color:var(--bad)">${c(i => i.enabled && i.status === 'bad')}</b><span>连接失败</span></div>
        <div><b>${c(i => !i.enabled)}</b><span>已停用</span></div><div><b>${c(i => i.slowlog)}</b><span>慢日志采集中</span></div></div>
      <div class="toolbar">
        <select data-onchange="imF" data-k="role"><option value="">全部角色</option>${opts([['master', '主库 master'], ['slave', '从库 slave']], IM.role)}</select>
        <select data-onchange="imF" data-k="type"><option value="">全部类型</option>${opts([['MySQL', 'MySQL'], ['MariaDB', 'MariaDB'], ['MsSQL', 'SQL Server'], ['Redis', 'Redis'], ['PgSQL', 'PostgreSQL'], ['Mongo', 'MongoDB'], ['ClickHouse', 'ClickHouse'], ['Doris', 'Doris'], ['SelectDB', 'SelectDB']], IM.type)}</select>
        <select data-onchange="imF" data-k="tag"><option value="">全部标签</option>${opts(allTags().map(t => [t, t]), IM.tag)}</select>
        <select data-onchange="imF" data-k="st"><option value="">全部状态</option>${opts([['ok', '正常'], ['warn', '告警'], ['bad', '连接失败'], ['disabled', '已停用'], ['slow_on', '慢日志采集中'], ['slow_off', '慢日志未采集']], IM.st)}</select>
        <input type="search" placeholder="搜索实例名称 / HOST" value="${esc(IM.q)}" data-oninput="imF" data-k="q" style="width:220px"></div>
      <div id="imTbl">${imTable()}</div></div>`;
  }
};
ON.imF = (d, el) => { IM[d.k] = el.value; part('imTbl', imTable()); };
ON.imView = d => {
  const i = instById(d.id);
  openDrawer(i.name, `<div class="row" style="margin-bottom:10px">${typeTag(i.type)}${i.role === 'master' ? tag('主库', 'brand') : tag('从库')}${i.enabled ? '' : tag('已停用')}</div>${kvList([
    ['地址', `<span class="mono">${esc(i.host)}:${i.port}</span>`], ['版本', esc(i.ver)], ['环境', esc(i.env || '—')], ['连接账号', i.type === 'Redis' && !i.user ? `<span class="muted">未设置账号（仅密码认证）</span> <span class="small muted">${i.pwdSet === false ? '<b style="color:var(--bad)">实例没有设置密码</b>' : '密码已加密保存（' + (i.pwdAt ? '更新于 ' + esc(i.pwdAt) : '不回显') + '）'}</span>` : `<span class="mono">${esc(i.user)}</span> <span class="small muted">密码已加密保存（${i.pwdAt ? '更新于 ' + esc(i.pwdAt) : '不回显'}）</span>`],
    ['SSH 隧道', i.ssh ? esc(i.sshName || SSH_TUNNELS[0]) : '无'], ['资源组', i.rgs.map(r => esc(rgName(r))).join('、') || '—'], ['标签', i.tags.map(t => tag(t)).join(' ') || '—'],
    ['慢日志采集', i.slowlog ? tag('采集中', 'ok') : tag('未采集')], ['当前负载', i.enabled && i.status !== 'bad' ? `QPS ${fmt(i.qps)} · 连接 ${i.conns}` : '—']])}
    <div class="row" style="margin-top:12px">${isExtInst(i) ? `<a class="btn sm" href="#query?inst=${i.id}">在线查询</a><a class="btn sm" href="#dict">数据字典</a>` : i.type === 'Redis' ? '<a class="btn sm" href="#inspect">性能巡检</a><a class="btn sm" href="#slowquery">慢查日志</a>' : '<a class="btn sm" href="#sessions">会话管理</a><a class="btn sm" href="#databases">数据库</a><a class="btn sm" href="#accounts">账号</a><a class="btn sm" href="#params">参数</a>'}</div>`);
};

/* ---- 测试连接（模拟：10.x 内网可通；已知故障实例 / 非内网 / 密码 wrong 失败）---- */
function testConn(o) {
  const ex = INSTANCES.find(x => x.host === o.host && +x.port === +o.port);
  const ms = 2 + (o.host.split('.').reduce((a, b) => a + (+b || 0), 0) % 9);
  if (o.pwd === 'wrong') return {ok: false, msg: o.type === 'Redis' ? 'WRONGPASS invalid username-password pair or user is disabled.' : o.type === 'PgSQL' ? 'PostgreSQL 认证失败：账号或密码错误，或 pg_hba.conf 不允许该来源连接' : o.type === 'Mongo' ? 'MongoDB 认证失败：请检查实例的账号 / 密码 / 认证库（认证库固定为 admin）' : o.type === 'ClickHouse' ? 'ClickHouse 错误 516（认证失败，请检查账号密码）：default: Authentication failed: password is incorrect, or there is no user with such name.' : `认证失败：Access denied for user '${o.user}'@'10.1.0.30'（using password: YES）`};
  if (ex && ex.status === 'bad') return {ok: false, msg: ex.id === 9 ? `连接超时：${o.host}:${o.port}（dial tcp: i/o timeout，10 秒）` : `连接被拒绝：${o.host}:${o.port} 未监听（connection refused）`};
  if (!/^10\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(o.host)) return {ok: false, msg: `无法连接：${o.host} 不可达（原型里仅 10.x 内网地址可通）`};
  return {ok: true, ms, ver: ex ? ex.ver : DEFAULT_VER[o.type], ssh: o.ssh};
}
const connMsg = (o, r) => r.ok ? `连接成功：${esc(o.host)}:${o.port} · ${esc(typeName(o.type))} ${esc(r.ver)} · 延迟 ${r.ms} ms${r.ssh ? ' · 经 SSH 隧道 ' + esc(r.ssh.split('（')[0]) : ''}` : esc(r.msg);
ACTIONS['instance.test'] = d => {
  if (d.form) {                                           // 表单内测试（密码留空沿用已保存的）
    const o = formVals(); if (o.err) return setErr('ifErr', o.err);
    setErr('ifErr', ''); const r = testConn({...o, ssh: o.sshName});
    part('ifTestRes', `<span style="color:var(--${r.ok ? 'ok' : 'bad'})">${r.ok ? '✓ ' : '✗ '}${connMsg(o, r)}</span>`);
    audit('instance.test', `测试连接 ${o.host}:${o.port}（表单，未保存）`, r.ok ? '成功' : '失败'); return;
  }
  const i = instById(d.id), o = {host: i.host, port: i.port, user: i.user, type: i.type, ssh: i.ssh ? i.sshName || SSH_TUNNELS[0] : ''};
  const r = testConn(o);
  audit('instance.test', `测试连接 ${i.name}（${i.host}:${i.port}）`, r.ok ? '成功' : '失败', r.ok ? '' : r.msg);
  openModal({title: '测试连接 · ' + esc(i.name), code: 'instance.test', ok: '知道了', cancel: '', body: notice(connMsg(o, r), r.ok ? 'ok' : 'bad', r.ok ? '✓' : '✗')});
};

/* ---- 新建 / 编辑实例（页内弹窗）---- */
function instForm(i) {
  const e = !!i, d = i || {name: '', type: 'MySQL', role: 'master', env: '生产', host: '', port: 3306, user: '', rgs: [], tags: [], ssh: false, slowlog: true, enabled: true};
  const tags = [...new Set(['can_write', 'can_read', 'read_view', 'mysql_inspection', 'mssql_inspection', ...allTags()])];
  return `<div class="form">
    ${fld('实例名称', `<input id="ifName" value="${esc(d.name)}" maxlength="64" placeholder="例如 mysql-order-prod-m">`, {req: 1})}
    ${fld('数据库类型', `<select id="ifType" data-onchange="ifType">${opts([['MySQL', 'MySQL'], ['MariaDB', 'MariaDB'], ['MsSQL', 'SQL Server'], ['Redis', 'Redis'], ['PgSQL', 'PostgreSQL'], ['Mongo', 'MongoDB'], ['ClickHouse', 'ClickHouse'], ['Doris', 'Doris'], ['SelectDB', 'SelectDB']], d.type)}</select>`, {req: 1})}
    ${fld('角色', `<select id="ifRole">${opts([['master', '主库 master'], ['slave', '从库 slave']], d.role)}</select>`)}
    ${fld('环境', `<select id="ifEnv">${opts([['生产', '生产'], ['测试', '测试']], d.env)}</select>`)}
    ${fld('HOST', `<input id="ifHost" value="${esc(d.host)}" placeholder="10.20.1.13">`, {req: 1})}
    ${fld('PORT', `<input id="ifPort" type="number" value="${d.port}" min="1" max="65535">`, {req: 1})}
    ${fld('连接账号', `<input id="ifUser" value="${esc(d.user)}" placeholder="${d.type === 'Redis' ? '可空（仅密码认证）' : d.type === 'Mongo' ? '可空（不认证）' : 'dbtool_ro'}" autocomplete="off">`, {req: 1})}
    ${fld(e ? '密码（留空不改）' : '密码', `<input id="ifPwd" type="password" autocomplete="new-password">`, {req: !e})}
    ${fld('资源组', `<div class="row">${RGROUPS.filter(r => r.enabled).map(r => `<label class="chk"><input type="checkbox" class="ifRg" value="${r.id}" ${d.rgs.includes(r.id) ? 'checked' : ''}>${esc(r.name)}</label>`).join('')}</div>`, {req: 1})}
    ${fld('标签', `<div class="row">${tags.map(t => `<label class="chk"><input type="checkbox" class="ifTag" value="${esc(t)}" ${d.tags.includes(t) ? 'checked' : ''}>${esc(t)}</label>`).join('')}</div>`)}
    ${fld('SSH 隧道', `<select id="ifSsh"><option value="">不使用</option>${opts(SSH_TUNNELS.map(s => [s, s]), d.ssh ? d.sshName || SSH_TUNNELS[0] : '')}</select>`)}
    ${fld('选项', `<label class="chk" style="margin-right:14px"><input type="checkbox" id="ifSlow" ${d.slowlog ? 'checked' : ''}>慢日志采集</label><label class="chk"><input type="checkbox" id="ifEn" ${d.enabled ? 'checked' : ''}>启用</label>`)}
  </div>
  <div class="row" style="margin-top:12px">${B('instance.test', '测试连接', {data: {form: 1}})}<span id="ifTestRes" class="small"></span></div>${errBox('ifErr')}`;
}
function formVals() {
  const o = {name: fv('ifName'), type: fv('ifType'), role: fv('ifRole'), env: fv('ifEnv'), host: fv('ifHost'), port: +fv('ifPort'), user: fv('ifUser'), pwd: $('#ifPwd').value,
    rgs: checkedVals('ifRg').map(Number), tags: checkedVals('ifTag'), sshName: fv('ifSsh'), slowlog: $('#ifSlow').checked, enabled: $('#ifEn').checked};
  o.ssh = !!o.sshName;
  if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{1,63}$/.test(o.name)) o.err = '实例名称需 2–64 位，字母数字开头，只含字母数字与 . _ -';
  else if (!/^([A-Za-z0-9-]+\.)*[A-Za-z0-9-]+$/.test(o.host)) o.err = 'HOST 不是合法的 IP / 主机名';
  else if (!(o.port >= 1 && o.port <= 65535)) o.err = 'PORT 需在 1–65535';
  else if (!o.user && o.type !== 'Redis' && o.type !== 'Mongo') o.err = '请填写连接账号';
  else if (o.type === 'Redis' && o.ssh) o.err = 'Redis 暂不支持 SSH 隧道';
  return o;
}
function openInstForm(i) {
  const e = !!i;
  openModal({title: e ? `编辑实例 · ${esc(i.name)}` : '添加实例', wide: true, code: e ? 'instance.edit' : 'instance.add', ok: '保存', body: instForm(i), onOk() {
    const o = formVals(); if (o.err) return setErr('ifErr', o.err);
    if (INSTANCES.some(x => x.name === o.name && (!e || x.id !== i.id))) return setErr('ifErr', `实例名称「${o.name}」已存在`);
    if (!e && !o.pwd && o.type !== 'Redis' && o.type !== 'Mongo') return setErr('ifErr', '新建实例必须填写密码');
    if (!o.rgs.length) return setErr('ifErr', '请至少选择一个资源组（否则没有人能看到该实例）');
    const nz = v => Array.isArray(v) ? JSON.stringify([...v].sort()) : JSON.stringify(v);
    const show = (k, v) => k === 'role' ? (v === 'master' ? '主库' : '从库') : k === 'rgs' ? v.map(rgName).join('、') : k === 'tags' ? v.join('、') : k === 'slowlog' ? (v ? '开启' : '关闭') : k === 'enabled' ? (v ? '启用' : '停用') : k === 'ssh' ? (v ? '使用' : '不使用') : v;
    const changes = e ? ['name', 'type', 'role', 'env', 'host', 'port', 'user', 'rgs', 'tags', 'slowlog', 'enabled', 'ssh'].filter(k => nz(i[k]) !== nz(o[k])).map(k => [k, show(k, i[k]), show(k, o[k])]) : [];
    const LBL = {name: '名称', type: '类型', role: '角色', env: '环境', host: 'HOST', port: 'PORT', user: '连接账号', rgs: '资源组', tags: '标签', slowlog: '慢日志采集', enabled: '状态', ssh: 'SSH 隧道'};
    if (e && !changes.length && !o.pwd) { toast('没有修改'); return; }
    later(() => risky(e ? 'instance.edit' : 'instance.add', e ? `保存实例「${i.name}」` : `添加实例「${o.name}」`,
      e ? `将更新以下内容：<ul style="margin:6px 0 0;padding-left:20px">${changes.map(([k, a, b]) => `<li>${LBL[k]}：${esc(a || '—')} → <b>${esc(b || '—')}</b></li>`).join('')}${o.pwd ? '<li>连接密码：已更改</li>' : ''}</ul>`
        : `将${o.user ? '以 <b>' + esc(o.user) + '</b> ' : '不带账号（仅密码认证）'}连接 <b>${esc(o.host)}:${o.port}</b>（${esc(typeName(o.type))}，${o.role === 'master' ? '主库' : '从库'}）。资源组：${esc(o.rgs.map(rgName).join('、'))}。`, '', () => {
        const keep = {name: o.name, type: o.type, role: o.role, env: o.env, host: o.host, port: o.port, user: o.user, rgs: o.rgs, tags: o.tags, ssh: o.ssh, sshName: o.sshName, slowlog: o.slowlog, enabled: o.enabled};
        if (e) { const wasExt = isExtInst(i); Object.assign(i, keep); if (isExtInst(i) !== wasExt) i.schema = ''; if (o.pwd) { i.pwdSet = true; i.pwdAt = clk(); }
          audit('instance.edit', `编辑实例 ${i.name}：${changes.map(c => LBL[c[0]]).concat(o.pwd ? ['密码'] : []).join('、')}`, '成功', `${i.host}:${i.port}`); toast('实例已保存'); }
        else { const n = Object.assign({id: Math.max(...INSTANCES.map(x => x.id)) + 1, ver: DEFAULT_VER[o.type], status: 'ok', qps: 0, conns: 0, pwdSet: !!o.pwd, pwdAt: o.pwd ? clk() : '',
            schema: o.type === 'Redis' || EXT_TYPES.includes(o.type) ? '' : o.type === 'MsSQL' ? 'mssql' : /order/.test(o.name) ? 'order' : /pay/.test(o.name) ? 'pay' : /user/.test(o.name) ? 'user' : ''}, keep);
          INSTANCES.push(n); audit('instance.add', `添加实例 ${n.name}（${n.type} ${n.host}:${n.port}，资源组 ${n.rgs.map(rgName).join('/')}）`, '成功'); toast('实例已添加'); }
        rerender(); }));
  }});
}
ON.ifType = (d, el) => { const p = $('#ifPort'); if (['3306', '1433', '3307', '6379', '5432', '27017', '8123', '9030'].includes(p.value)) p.value = DEFAULT_PORT[el.value] || 3306;
  const u = $('#ifUser'); if (u) u.placeholder = el.value === 'Redis' ? '可空（仅密码认证）' : el.value === 'Mongo' ? '可空（不认证）' : 'dbtool_ro'; };

/* ---- 编辑 / 启停实例 / 切换慢日志（instance.edit 一个码，op 区分）---- */
ACTIONS['instance.add'] = () => openInstForm(null);
ACTIONS['instance.edit'] = d => {
  const i = instById(d.id);
  if (d.op === 'edit') return openInstForm(i);
  if (isExtInst(i) && d.op === 'slow') return toast('该类型暂不支持慢日志采集');
  if (d.op === 'slow') {
    const on = !i.slowlog;
    return risky('instance.edit', `${on ? '开启' : '关闭'}慢日志采集：${i.name}`, on ? '开启后平台会定期拉取该实例的慢日志。' : '关闭后该实例不再采集慢日志，已采集数据保留。', `-- 平台元数据（不改动目标库）\nUPDATE dbt_sql_instance SET slow_log_collect = ${on ? 1 : 0} WHERE id = ${i.id};`, () => {
      i.slowlog = on; audit('instance.edit', `${on ? '开启' : '关闭'}慢日志采集 ${i.name}`, '成功'); toast(on ? '已开启采集' : '已关闭采集'); rerender(); });
  }
  const on = !i.enabled;                                        // op === 'enable'
  risky('instance.edit', `${on ? '启用' : '停用'}实例：${i.name}`, on ? '启用后该实例重新出现在各业务页面的实例列表里。' : `停用后 <b>${esc(i.rgs.map(rgName).join('、'))}</b> 的成员无法再选择该实例，平台也不再连接它。`, `-- 平台元数据（不改动目标库）\nUPDATE dbt_sql_instance SET is_active = ${on ? 1 : 0} WHERE id = ${i.id};`, () => {
    i.enabled = on; audit('instance.edit', `${on ? '启用' : '停用'}实例 ${i.name}`, '成功'); toast(on ? '实例已启用' : '实例已停用'); rerender(); });
};

/* ---- 删除实例：被引用时拒绝并说明数量 ---- */
function refsOf(i) {
  const live = (arr, k) => (arr || []).filter(x => x[k || 'inst'] == i.id).length;
  const seed = i.id <= 10 ? (k => 1 + ((i.id * 37 + k * 11) % 40)) : () => 0;
  const rows = [
    ['SQL 上线工单（含批量导出）', Math.max(live(window.WORKFLOWS), i.id <= 10 ? seed(1) + (i.id === 10 ? 3 : 20) : 0), '先在「SQL 上线」处理 / 归档这些工单'],
    ['查询权限及申请', Math.max(live(window.APPLIES), seed(2)), '到「查询权限」回收该实例的授权'],
    ['慢日志采集数据', i.id > 10 ? 0 : i.slowlog ? 12480 + i.id * 311 : 340, '先关闭慢日志采集并清理历史数据'],
    ['分区维护配置', [1, 2, 4, 20, 21].includes(i.id) ? 2 + i.id % 3 : 0, '到「分区维护」删除相关配置'],
    ['性能巡检报告', ((window.INSPECT_REPORTS || []).filter(r => r.inst === i.id)).length, '到「性能巡检」删除该实例的报告与计划引用'],
    ['性能工单', live(window.PERF_ISSUES), '先关闭 / 转移相关性能工单']];
  return rows.filter(r => r[1] > 0);
}
ACTIONS['instance.delete'] = d => {
  const i = instById(d.id), refs = refsOf(i);
  if (refs.length) {
    audit('instance.delete', `删除实例 ${i.name} 被拒绝：存在引用（${refs.map(r => r[0] + ' ' + r[1]).join('、')}）`, '拒绝');
    return openModal({title: `无法删除「${esc(i.name)}」`, ok: '知道了', cancel: '', code: 'instance.delete', body: notice('该实例仍被以下对象引用，<b>已拒绝删除</b>。', 'bad', '✗') +
      tbl(['引用对象', '数量', '建议'], refs, {cells: r => [esc(r[0]), `<b>${fmtN(r[1])}</b>`, `<span class="small muted">${esc(r[2])}</span>`]})});
  }
  risky('instance.delete', `删除实例：${i.name}`, `账号登记、库登记、参数历史与资源组 / 标签关联将<b>随实例一起删除</b>，不可恢复；目标库本身不受影响。`,
    `DELETE FROM dbt_sql_instance WHERE id = ${i.id};  -- ${i.name}`, () => {
      INSTANCES.splice(INSTANCES.indexOf(i), 1); audit('instance.delete', `删除实例 ${i.name}（${i.host}:${i.port}）`, '成功'); toast('实例已删除'); rerender(); }, i.name);
};

/* ---- Binlog 清理：预览“将清理到哪个文件” → 输入文件名确认（仅超级用户、仅 MySQL）---- */
const BINLOG = {};
function binlogs(i) {
  if (!BINLOG[i.id]) { const base = 800 + i.id * 3, r = rng(i.id * 31 + 7);
    BINLOG[i.id] = Array.from({length: 8}, (_, k) => ({f: 'mysql-bin.' + String(base + k).padStart(6, '0'), mb: k === 7 ? R(r, 120, 700) : R(r, 880, 1024), at: dayOffset(k - 7) + ' ' + pad2(R(r, 0, 23)) + ':' + pad2(R(r, 0, 59))})); }
  return BINLOG[i.id];
}
const BL = {i: null, target: ''};
function blPreview() {
  const i = BL.i, list = binlogs(i), idx = list.findIndex(x => x.f === BL.target);
  if (idx < 0) return '<span class="muted small">选择文件后显示预览</span>';
  const del = list.slice(0, idx), mb = del.reduce((a, b) => a + b.mb, 0);
  const slaves = i.name.endsWith('-m') ? INSTANCES.filter(x => x.role === 'slave' && x.name.startsWith(i.name.slice(0, -1))) : [];
  const readAt = list[list.length - 2].f, blocked = slaves.length && idx > list.length - 2;
  return `${idx === 0 ? notice('所选文件已经是最早的 binlog，没有可清理的内容。', 'warn', '!') : ''}
    ${kvList([['将清理到', `<b class="mono">${esc(BL.target)}</b>（该文件本身保留）`], ['将删除', `${del.length} 个文件，约 ${fmtMB(mb)}：<span class="mono small">${del.length ? esc(del[0].f) + (del.length > 1 ? ' … ' + esc(del[del.length - 1].f) : '') : '—'}</span>`],
      ['保留', `${list.length - idx} 个文件（含正在写入的 ${esc(list[list.length - 1].f)}）`], ['已连接从库', slaves.length ? `${slaves.map(s => esc(s.name)).join('、')} · 最慢从库正读取 <span class="mono">${esc(readAt)}</span>` : '无']])}
    ${blocked ? notice(`从库尚未读完 <b>${esc(readAt)}</b> 之前的日志；清理到更靠后的文件会<b>导致复制中断</b>，已拒绝。请选择不晚于 ${esc(readAt)} 的文件。`, 'bad', '✗') : ''}`;
}
ACTIONS['instance.purge_binlog'] = d => {
  if (!ME.super) return toast('Binlog 清理仅超级用户可用');
  const i = instById(d.id); BL.i = i; BL.target = '';
  openModal({title: `清理 Binlog · ${esc(i.name)}`, code: 'instance.purge_binlog', ok: '下一步：输入文件名确认', okCls: 'danger primary', body:
    `${notice('<b>PURGE BINARY LOGS TO</b> 会删除所选文件<b>之前</b>的所有 binlog，且不可恢复。', 'warn', '!')}
    <div class="form"><label class="req">清理到</label><select id="blSel" data-onchange="blSel"><option value="">— 请选择 binlog 文件 —</option>${binlogs(i).map(x => `<option value="${x.f}">${x.f}　${fmtMB(x.mb)}　${x.at}</option>`).join('')}</select></div>
    <div id="blPrev" style="margin-top:12px">${blPreview()}</div>`, onOk() {
    const list = binlogs(i), idx = list.findIndex(x => x.f === BL.target);
    if (idx < 0) { toast('请先选择文件并预览'); return false; }
    if (idx === 0) { toast('没有可清理的内容'); return false; }
    const slaves = i.name.endsWith('-m') ? INSTANCES.filter(x => x.role === 'slave' && x.name.startsWith(i.name.slice(0, -1))) : [];
    if (slaves.length && idx > list.length - 2) { toast('从库尚未读取该文件，已拒绝'); audit('instance.purge_binlog', `清理 ${i.name} binlog 到 ${BL.target} 被拒绝：从库未读完`, '拒绝'); return false; }
    const target = BL.target;
    later(() => risky('instance.purge_binlog', `清理 ${i.name} 的 binlog 到 ${target}`, `将删除 <b>${idx}</b> 个文件（约 ${fmtMB(list.slice(0, idx).reduce((a, b) => a + b.mb, 0))}），不可恢复。`,
      `PURGE BINARY LOGS TO '${target}';`, () => { list.splice(0, idx); audit('instance.purge_binlog', `清理 binlog：${i.name} → ${target}（删除 ${idx} 个文件）`, '成功'); toast(`已清理 ${idx} 个 binlog 文件`); rerender(); }, target));
  }});
};
ON.blSel = (d, el) => { BL.target = el.value; part('blPrev', blPreview()); };

/* ============================================================================
   2. 会话管理 sessions
   ========================================================================== */
const SS = {inst: 0, sel: new Set(), f: {user: '', db: '', cmd: '', min: 0, q: ''}, tf: {min: 0, idle: false}, hl: 0};
const SD = {};
const SX_TABS = [['proc', '进程列表', 'session.process'], ['space', 'Top 表空间', 'session.tablespace'], ['trx', '事务', 'session.trx'], ['locks', '锁等待', 'session.locks']];
function sxInst() {
  const l = visibleInstances(true); let i = l.find(x => x.id === SS.inst);
  if (!i) { i = l[0]; SS.inst = i ? i.id : 0; SS.sel.clear(); }
  return i;
}
/* 每个实例一份可变的会话快照：进程列表 / 事务 / 锁等待互相一致，终止会话后一起变化 */
function sessOf(i) {
  if (SD[i.id]) return SD[i.id];
  const r = rng(i.id * 7919 + 3), db0 = (dbsOf(i.id)[0] || {db: 'app_db'}).db;
  const tb = tablesOf(i.id, db0).filter(t => !t.view), t1 = (tb[0] || {n: 'orders'}).n, t2 = (tb[1] || {n: 'order_items'}).n;
  let id = 88000 + R(r, 0, 900);
  const host = () => `10.20.${R(r, 1, 4)}.${R(r, 10, 90)}:${R(r, 40000, 60000)}`;
  const row = (user, db, cmd, time, state, info, x) => Object.assign({id: id += R(r, 1, 40), user, host: host(), db, cmd, time, state, info}, x);
  const blocker = row('app_rw', db0, 'Query', 1832, 'Sending data', `SELECT * FROM \`${t1}\` WHERE status = 1 AND created_at < '2026-09-01' ORDER BY created_at FOR UPDATE`, {long: 1});
  const rpt = row('report_ro', db0, 'Query', 412, 'Copying to tmp table', `SELECT DATE(created_at) AS d, COUNT(*) AS cnt, SUM(amount) AS gmv FROM \`${t1}\` GROUP BY d ORDER BY d DESC`);
  const w1 = row('app_rw', db0, 'Query', 35, 'updating', `UPDATE \`${t1}\` SET status = 2, updated_at = NOW() WHERE id = 28110345`);
  const w2 = row('app_rw', db0, 'Query', 21, 'updating', `UPDATE \`${t1}\` SET amount = amount - 10 WHERE id = 28110345`);
  const ghost = row('etl_job', db0, 'Query', 3, 'Sending data', `SELECT id, order_no FROM \`${t1}\` WHERE id > 28000000 LIMIT 5000`, {ghost: 1});
  const idle = row('app_rw', db0, 'Sleep', 1260, '', '', {idleTrx: 1, lastSql: `UPDATE \`${t2}\` SET qty = qty + 1 WHERE order_id = 28110112`});
  const sleeps = [R(r, 1, 30), R(r, 30, 200), R(r, 5, 90), 0, R(r, 200, 600)].map(t => row(['app_rw', 'app_ro', 'app_rw', 'job_sync', 'app_ro'][R(r, 0, 4)], db0, 'Sleep', t, '', ''));
  const me = Object.assign(row(i.user, null, 'Query', 0, 'executing', 'SELECT … FROM information_schema.PROCESSLIST WHERE ID <> CONNECTION_ID()'), {protected: 'platform', host: '10.1.0.30:48211'});
  const sys = i.role === 'master'
    ? [Object.assign(row('repl', null, 'Binlog Dump GTID', 604800, 'Master has sent all binlog to slave; waiting for more updates', ''), {protected: 'repl', host: '10.20.1.12:51210'})]
    : [Object.assign(row('system user', null, 'Connect', 604800, 'Waiting for master to send event', ''), {protected: 'repl', host: ''}), Object.assign(row('system user', null, 'Query', 0, 'Slave has read all relay log; waiting for more updates', ''), {protected: 'repl', host: ''})];
  const ev = Object.assign(row('event_scheduler', null, 'Daemon', 2400000, 'Waiting on empty queue', ''), {protected: 'event', host: 'localhost'});
  const ps = [blocker, rpt, w1, w2, ghost, idle, ...sleeps, me, ...sys, ev];
  const trx = [
    {thread: blocker.id, started: ago(1832), idle: 0, state: 'RUNNING', locked: 42180, mod: 0, info: blocker.info},
    {thread: w1.id, started: ago(35), idle: 0, state: 'LOCK WAIT', locked: 1, mod: 0, info: w1.info},
    {thread: w2.id, started: ago(21), idle: 0, state: 'LOCK WAIT', locked: 1, mod: 0, info: w2.info},
    {thread: idle.id, started: ago(1500), idle: 1260, state: 'RUNNING', locked: 3, mod: 12, info: idle.lastSql}];
  const locks = [
    {wait: w1.id, table: `${db0}.${t1}`, block: blocker.id, secs: 35},
    {wait: w2.id, table: `${db0}.${t1}`, block: w1.id, secs: 21}];
  return SD[i.id] = {ps, trx, locks, db0, t1};
}
const psOf = (i, id) => sessOf(i).ps.find(x => x.id == id);
function dropThread(i, id) {                       // 线程被终止后：事务、锁等待一起变化，被阻塞者继续运行
  const s = sessOf(i);
  s.ps = s.ps.filter(x => x.id != id); s.trx = s.trx.filter(x => x.thread != id);
  s.locks.filter(l => l.block == id).forEach(l => { const w = s.ps.find(x => x.id == l.wait), t = s.trx.find(x => x.thread == l.wait); if (t) t.state = 'RUNNING'; if (w) w.state = 'updating'; });
  s.locks = s.locks.filter(l => l.block != id && l.wait != id);
}
const isLong = t => t.cmd !== 'Sleep' && !t.protected && t.time >= 300;
const infoCell = (sql, id) => sql ? `<a data-on="sxInfo" data-id="${id}" class="mono small">${esc(trunc(sql, 60))}</a>` : '<span class="muted">—</span>';
const protTag = p => p ? tag({platform: '平台自身连接', repl: '复制线程', event: '事件调度'}[p] + ' · 受保护', 'info') : '';

function sxProcTable(i) {
  const f = SS.f, q = f.q.toLowerCase();
  const list = sessOf(i).ps.filter(t => (!f.user || t.user === f.user) && (!f.db || t.db === f.db) && (!f.cmd || (f.cmd === 'notsleep' ? t.cmd !== 'Sleep' : t.cmd === f.cmd)) && t.time >= f.min &&
    (!q || (t.user + ' ' + t.host + ' ' + (t.db || '') + ' ' + t.info).toLowerCase().includes(q))).sort((a, b) => b.time - a.time);
  return tbl(['<input type="checkbox" data-onchange="sxAll" title="全选当前列表">', 'THREAD ID', 'USER', 'HOST', 'DB', '耗时(s)', 'COMMAND', 'STATE', 'INFO', '操作'], list, {
    attrs: t => t.id == SS.hl ? ' style="outline:2px solid var(--brand);outline-offset:-2px"' : isLong(t) && t.time >= 1200 ? ' style="background:var(--bad-soft)"' : isLong(t) ? ' style="background:var(--warn-soft)"' : '',
    cells: t => [`<input type="checkbox" class="sxCk" value="${t.id}" data-onchange="sxCk" ${SS.sel.has(String(t.id)) ? 'checked' : ''}>`, `<span class="mono">${t.id}</span>`,
      `${esc(t.user)}${protTag(t.protected) ? '<br>' + protTag(t.protected) : ''}`, `<span class="mono small">${esc(t.host || '—')}</span>`, esc(t.db || '—'),
      `<b class="${isLong(t) && t.time >= 1200 ? 'up' : ''}">${t.time.toLocaleString()}</b>${isLong(t) ? ' <span class="tag ' + (t.time >= 1200 ? 'bad' : 'warn') + '">长时间运行</span>' : ''}${t.long ? ' <span class="tag bad">阻塞源</span>' : ''}`,
      esc(t.cmd), `<span class="small">${esc(t.state || '—')}</span>`, infoCell(t.info, t.id), B('session.kill', '终止', {cls: 'sm danger', data: {id: t.id}})]});
}
function sxProcTab(i) {
  const s = sessOf(i), f = SS.f, ps = s.ps, act = ps.filter(t => t.cmd !== 'Sleep' && !t.protected).length;
  return `<div class="card"><div class="stat" style="margin-bottom:12px"><div><b>${ps.length}</b><span>连接总数</span></div><div><b>${act}</b><span>活跃</span></div><div><b style="color:var(--warn)">${ps.filter(isLong).length}</b><span>长时间运行（≥300s）</span></div>
      <div><b>${ps.filter(t => t.protected).length}</b><span>受保护</span></div></div>
    <div class="toolbar"><select data-onchange="sxF" data-k="user"><option value="">全部用户</option>${opts([...new Set(ps.map(t => t.user))].map(u => [u, u]), f.user)}</select>
      <select data-onchange="sxF" data-k="db"><option value="">全部库</option>${opts([...new Set(ps.map(t => t.db).filter(Boolean))].map(u => [u, u]), f.db)}</select>
      <select data-onchange="sxF" data-k="cmd"><option value="">全部 command</option>${opts([['notsleep', 'Not Sleep'], ['Query', 'Query'], ['Sleep', 'Sleep']], f.cmd)}</select>
      <label class="small muted">耗时 ≥ <input type="number" min="0" value="${f.min}" data-oninput="sxF" data-k="min" style="width:80px"> s</label>
      <input type="search" placeholder="搜索 USER / HOST / SQL" value="${esc(f.q)}" data-oninput="sxF" data-k="q" style="width:220px"><span class="sp"></span>
      <span class="small muted" id="sxSelInfo">已选 ${SS.sel.size} 个会话</span>${B('session.kill', '终止所选会话', {cls: 'danger'})}</div>
    <div id="sxProc">${sxProcTable(i)}</div></div>`;
}
function sxSpaceTab(i) {
  const rows = [];
  dbsOf(i.id).forEach(d => d.tables.filter(t => !t.view).forEach(t => { const r = rng(t.rows % 9973 + t.n.length), frag = Math.round(t.size * (t.part ? 0.1 : 0.02) * (0.4 + r())), data = Math.round(t.size * 0.66);
    rows.push({db: d.db, n: t.n, eng: t.eng, total: t.size, rows: t.rows, data, idx: Math.max(0, t.size - data - frag), frag, pct: +(frag / t.size * 100).toFixed(1)}); }));
  rows.sort((a, b) => b.total - a.total);
  return `<div class="card"><h2>Top 表空间</h2>${tbl(['数据库', '表名', '引擎', '总空间(MB)', '行数', '数据(MB)', '索引(MB)', '碎片(MB)', '碎片率'], rows.slice(0, 20), {cells: t => [
    esc(t.db), `<span class="mono">${esc(t.n)}</span>`, t.eng, `<b>${t.total.toLocaleString()}</b>`, t.rows.toLocaleString(), t.data.toLocaleString(), t.idx.toLocaleString(), t.frag.toLocaleString(), `<span class="${t.pct >= 8 ? 'up b' : ''}">${t.pct}%</span>`]})}</div>`;
}
function sxTrxTable(i) {
  const f = SS.tf, list = sessOf(i).trx.filter(t => t.idle >= f.min && (!f.idle || t.idle > 0)).sort((a, b) => a.started < b.started ? -1 : 1);
  return tbl(['trx_started', '空闲(s)', 'trx_state', 'thread_id', 'USER', 'DB', '锁定行', '修改行', 'INFO'], list, {
    attrs: t => t.state === 'LOCK WAIT' ? ' style="background:var(--bad-soft)"' : t.idle >= 300 ? ' style="background:var(--warn-soft)"' : '',
    cells: t => { const p = psOf(i, t.thread) || {}; return [`<span class="mono small">${esc(t.started)}</span>`, t.idle ? `<b class="up">${t.idle.toLocaleString()}</b>` : '0', t.state === 'LOCK WAIT' ? tag('LOCK WAIT', 'bad') : tag('RUNNING', 'ok'),
      `<a data-on="sxJump" data-id="${t.thread}" class="mono">${t.thread}</a>`, esc(p.user || '—'), esc(p.db || '—'), t.locked.toLocaleString(), t.mod.toLocaleString(), infoCell(t.info, t.thread)]; }});
}
function sxTrxTab(i) {
  return `<div class="card"><h2>事务</h2>
    <div class="toolbar"><label class="small muted">持续空闲 ≥ <input type="number" min="0" value="${SS.tf.min}" data-oninput="sxTF" data-k="min" style="width:80px"> s</label>
      <label class="chk"><input type="checkbox" ${SS.tf.idle ? 'checked' : ''} data-onchange="sxTF" data-k="idle">仅空闲事务</label></div>
    <div id="sxTrx">${sxTrxTable(i)}</div></div>`;
}
function sxLocksTab(i) {
  const s = sessOf(i), L = s.locks, waiters = new Set(L.map(l => l.wait)), roots = [...new Set(L.map(l => l.block))].filter(b => !waiters.has(b));
  const sqlOf = id => { const p = psOf(i, id); return p ? (p.info || p.lastSql || '') : ''; };
  const walk = (id, dep) => [{id, dep}].concat(L.filter(l => l.block == id).flatMap(l => walk(l.wait, dep + 1)));
  const chain = roots.map(r => walk(r, 0));
  return `<div class="card"><h2>锁等待${L.length ? ` <span class="sub">${L.length} 个等待 · ${roots.length} 条阻塞链</span>` : ''}</h2>
    ${chain.length ? `<div class="notice bad"><span>⛓</span><div><b>阻塞链</b>${chain.map(c => `<div style="margin-top:8px">${c.map(n => { const p = psOf(i, n.id) || {}; return `<div style="margin-left:${n.dep * 24}px">${n.dep ? '↳ 被阻塞 ' : '<b>根因</b> '}<a data-on="sxJump" data-id="${n.id}" class="mono">#${n.id}</a> <span class="small muted">${esc(p.user || '')} · 已 ${p.time || 0}s</span> <span class="mono small">${esc(trunc(sqlOf(n.id), 70))}</span></div>`; }).join('')}</div>`).join('')}</div></div>` : empty('没有锁等待，一切正常', '✓')}
    ${L.length ? tbl(['等待状态', '等待线程', '等待事务 SQL', '等待的表', '阻塞线程', '阻塞事务 SQL', '已等待'], L, {cells: l => [tag('LOCK WAIT', 'bad'), `<a data-on="sxJump" data-id="${l.wait}" class="mono">${l.wait}</a>`, infoCell(sqlOf(l.wait), l.wait), `<span class="mono small">${esc(l.table)}</span>`,
      `<a data-on="sxJump" data-id="${l.block}" class="mono">${l.block}</a>`, infoCell(sqlOf(l.block), l.block), `<b>${l.secs} s</b>`]}) : ''}</div>`;
}
pages.sessions = {
  title: '会话管理',
  render(sub) {
    const tabsOk = SX_TABS.filter(t => can(t[2])), i = sxInst();
    const cur = (tabsOk.find(t => t[0] === sub) || tabsOk[0] || [])[0];
    if (!i) return notice('没有可用的实例。', 'warn', '!');
    let body;
    if (!tabsOk.length) body = notice('没有任何页签的查看权限。', 'warn', '!');
    else if (!isMy(i)) body = unsupported(i, '会话管理');
    else if (i.status === 'bad') body = connFail(i);
    else body = cur === 'space' ? sxSpaceTab(i) : cur === 'trx' ? sxTrxTab(i) : cur === 'locks' ? sxLocksTab(i) : sxProcTab(i);
    return `<div class="toolbar"><b>实例</b><select data-onchange="sxInst" style="min-width:300px">${instOpts(visibleInstances(true), i.id)}</select>${i.role === 'slave' ? tag('从库') : tag('主库', 'brand')}<span class="sp"></span>
      <button class="btn" data-on="sxRefresh">⟳ 刷新</button></div>
      ${tabsOk.length ? tabs('sessions', tabsOk.map(t => [t[0], t[1]]), cur) : ''}${body}`;
  }
};
ON.sxInst = (d, el) => { SS.inst = +el.value; SS.sel.clear(); SS.hl = 0; rerender(); };
ON.sxRefresh = () => { const i = sxInst(); if (i && SD[i.id]) SD[i.id].ps.forEach(t => { if (t.cmd !== 'Sleep' && !t.protected) t.time += R(Math.random, 1, 4); }); toast('已刷新'); rerender(); };
ON.sxF = (d, el) => { SS.f[d.k] = d.k === 'min' ? Math.max(0, +el.value || 0) : el.value; part('sxProc', sxProcTable(sxInst())); };
ON.sxTF = (d, el) => { SS.tf[d.k] = d.k === 'idle' ? el.checked : Math.max(0, +el.value || 0); part('sxTrx', sxTrxTable(sxInst())); };
ON.sxCk = (d, el) => { el.checked ? SS.sel.add(el.value) : SS.sel.delete(el.value); part('sxSelInfo', `已选 ${SS.sel.size} 个会话`); };
ON.sxAll = (d, el) => { $$('.sxCk').forEach(c => { c.checked = el.checked; el.checked ? SS.sel.add(c.value) : SS.sel.delete(c.value); }); part('sxSelInfo', `已选 ${SS.sel.size} 个会话`); };
ON.sxInfo = d => { const i = sxInst(), p = psOf(i, d.id) || {}; openDrawer(`线程 ${d.id} · 完整 SQL`, `${kvList([['用户', esc(p.user || '')], ['来源', esc(p.host || '')], ['库', esc(p.db || '—')], ['耗时', (p.time || 0) + ' s'], ['状态', esc(p.state || '—')]])}${code(p.info || p.lastSql || '（无）', 'sql')}`); };
ON.sxJump = d => { SS.hl = +d.id; SS.f = {user: '', db: '', cmd: '', min: 0, q: ''}; go('sessions', 'proc'); setTimeout(() => { const el = $(`.sxCk[value="${d.id}"]`); if (el) el.scrollIntoView({block: 'center'}); }, 30); };

/* ---- 终止会话：第 1 步预览 → 第 2 步二次确认（服务端重新校验）---- */
function killCheck(i, ids) {
  return ids.map(id => { const t = psOf(i, id);
    if (!t) return {id, ok: false, why: '线程已不存在（可能已自行结束），已跳过'};
    if (t.protected === 'platform') return {id, t, ok: false, why: '平台自身连接，不能终止（否则平台会被踢下线）'};
    if (t.protected === 'repl') return {id, t, ok: false, why: '复制线程（Binlog Dump / system user），不能终止'};
    if (t.protected === 'event') return {id, t, ok: false, why: '事件调度线程，不能终止'};
    return {id, t, ok: true}; });
}
ACTIONS['session.kill'] = d => {
  const i = sxInst(), ids = d.id ? [d.id] : [...SS.sel];
  if (!ids.length) return toast('请先勾选要终止的会话');
  const chk = killCheck(i, ids), okL = chk.filter(c => c.ok), bad = chk.filter(c => !c.ok);
  openModal({title: `终止会话 · 预览（第 1 步 / 共 2 步）`, code: 'session.kill', ok: okL.length ? '下一步：确认终止' : '', okCls: 'danger primary', body:
    `<div class="small muted">语句预览</div>
     ${okL.length ? code(okL.map(c => `KILL ${c.id};  -- ${c.t.user}@${(c.t.host || '').split(':')[0]} ${c.t.db || ''} 已运行 ${c.t.time}s`).join('\n'), 'sql') : ''}
     ${bad.length ? `<div class="notice bad" style="margin-top:8px"><span>✗</span><div><b>以下 ${bad.length} 个会话不会被终止</b>${bad.map(c => `<div class="small">#${c.id}：${esc(c.why)}</div>`).join('')}</div></div>` : ''}
     ${!okL.length ? '<div class="small muted">没有可终止的会话。</div>' : ''}`,
    onOk() { later(() => risky('session.kill', `终止 ${okL.length} 个会话（${i.name}）`, `终止后这些连接上<b>未提交的事务会回滚</b>。`,
      okL.map(c => `KILL ${c.id};`).join('\n'), () => {
        ids.forEach(id => { const t = psOf(i, id); if (t && t.ghost) dropThread(i, id); });              // 演示：etl_job 的短查询在确认前自行结束
        const res = killCheck(i, ids), done = [], rej = [];
        res.forEach(c => { if (c.ok) { dropThread(i, c.id); SS.sel.delete(String(c.id)); done.push(c);
            audit('session.kill', `终止会话 thread_id=${c.id} user=${c.t.user} host=${(c.t.host || '').split(':')[0]} db=${c.t.db || '—'}`, '成功', i.name); }
          else { rej.push(c); audit('session.kill', `终止会话 thread_id=${c.id} 被拒绝：${c.why}`, '拒绝', i.name); } });
        later(() => { openModal({title: '终止结果', ok: '知道了', cancel: '', body: `${done.length ? notice(`已终止 <b>${done.length}</b> 个会话：${done.map(c => '#' + c.id).join('、')}`, 'ok', '✓') : ''}
          ${rej.length ? `<div class="notice bad"><span>✗</span><div><b>被拒绝 ${rej.length} 个</b>${rej.map(c => `<div class="small">#${c.id}：${esc(c.why)}</div>`).join('')}</div></div>` : ''}`}); rerender(); });
      }));}});
};

/* ============================================================================
   3. 数据库管理 databases
   ========================================================================== */
const DM = {inst: 0, q: '', f: ''};
const DBREG = {};                                              // `${inst}:${db}` → {owner, remark}（平台登记）
const DBEXTRA = {};                                            // inst → [{db, charset, coll}]（实例上存在、但不在 SCHEMAS 里的库 / 刚创建的库）
const DB_SEED = {order_db: ['rd_zhao', '订单主库'], pay_db: ['rd_zhou', '支付主库'], user_db: ['dev_liu', '用户中心主库'], ReportDW: ['dba_li', '报表数仓']};
const SYS_DBS = ['mysql', 'information_schema', 'performance_schema', 'sys'];
const CHARSETS = {utf8mb4: ['utf8mb4_general_ci', 'utf8mb4_unicode_ci', 'utf8mb4_bin', 'utf8mb4_0900_ai_ci'], utf8: ['utf8_general_ci', 'utf8_bin'], latin1: ['latin1_swedish_ci', 'latin1_bin'], gbk: ['gbk_chinese_ci', 'gbk_bin']};
const defColl = (i, cs) => cs === 'utf8mb4' ? (/^8\./.test(i.ver) ? 'utf8mb4_0900_ai_ci' : 'utf8mb4_general_ci') : CHARSETS[cs] ? CHARSETS[cs][0] : cs + '_general_ci';
function dmInst() { const all = visibleInstances(true); let i = all.find(x => x.id === DM.inst); if (!i) { i = all.find(isMy) || all[0]; DM.inst = i ? i.id : 0; } return i; }
function dbRows(i) {
  const rows = dbsOf(i.id).map(d => ({db: d.db, charset: d.charset === 'utf8mb4' ? 'utf8mb4' : d.charset, coll: defColl(i, d.charset), tables: d.tables.length, mb: d.tables.reduce((a, t) => a + (t.size || 0), 0)}));
  if (i.id % 2 === 1 && i.schema && i.schema !== 'mssql' && i.schema !== 'view') rows.push({db: 'tmp_migrate_0912', charset: 'utf8', coll: 'utf8_general_ci', tables: 6, mb: 410});
  (DBEXTRA[i.id] || []).forEach(x => rows.push({db: x.db, charset: x.charset, coll: x.coll, tables: 0, mb: 0, fresh: 1}));
  return rows;
}
const regOf = (i, db) => DBREG[`${i.id}:${db}`] || (i.role === 'master' && DB_SEED[db] ? {owner: DB_SEED[db][0], remark: DB_SEED[db][1]} : null);
function dmTable(i) {
  const q = DM.q.toLowerCase(), accs = accOf(i);
  const rows = dbRows(i).filter(r => (!q || r.db.toLowerCase().includes(q)) && (DM.f === '' || (DM.f === 'reg') === !!regOf(i, r.db)));
  return tbl(['数据库名', '字符集', '排序规则', '表数 / 大小', '授权账号', '负责人', '备注', '操作'], rows, {cells: r => { const g = regOf(i, r.db);
    const au = accs.filter(a => !a.builtin && !a.platform && a.grants.some(x => x.obj.includes('`' + r.db + '`') || x.obj === '*.*')).map(a => `${a.user}@${a.host}`).filter((x, k, arr) => arr.indexOf(x) === k && !x.startsWith('mysql.') && !x.startsWith('root@localhost'));
    return [`<b class="mono">${esc(r.db)}</b>${r.fresh ? ' <span class="tag ok">新建</span>' : ''}`, esc(r.charset), `<span class="small">${esc(r.coll)}</span>`, r.tables ? `${r.tables} 张 · ${fmtMB(r.mb)}` : '<span class="muted">—</span>',
      au.length ? `<span class="mono small">${au.slice(0, 3).map(esc).join('<br>')}${au.length > 3 ? `<br><span class="muted">… 共 ${au.length} 个</span>` : ''}</span>` : '<span class="muted">—</span>',
      g ? `${esc(plat(g.owner))} <span class="small muted">${esc(g.owner)}</span>` : '<span class="muted">未录入</span>', g ? esc(g.remark) : '', B('database.edit', g ? '编辑' : '录入', {cls: 'sm', data: {db: r.db}})]; }});
}
pages.databases = {
  render() {
    const i = dmInst(); if (!i) return notice('没有可用的实例。', 'warn', '!');
    return `<div class="card"><h2>数据库管理<span class="right">${B('database.create', '＋ 创建数据库', {cls: 'primary'})}</span></h2>
      <div class="toolbar"><b>实例</b><select data-onchange="dmInst" style="min-width:300px">${instOpts(visibleInstances(true), i.id)}</select>
        <select data-onchange="dmF" data-k="f"><option value="">全部库</option>${opts([['reg', '已录入'], ['unreg', '未录入']], DM.f)}</select>
        <input type="search" placeholder="搜索库名" value="${esc(DM.q)}" data-oninput="dmF" data-k="q" style="width:200px"></div>
      ${!isMy(i) ? unsupported(i, '数据库管理（含建库）') : i.status === 'bad' ? connFail(i) : `<div id="dmTbl">${dmTable(i)}</div>`}</div>`;
  }
};
ON.dmInst = (d, el) => { DM.inst = +el.value; rerender(); };
ON.dmF = (d, el) => { DM[d.k] = el.value; part('dmTbl', dmTable(dmInst())); };
const ownerSel = (id, cur) => `<select id="${id}"><option value="">— 请选择负责人 —</option>${opts(activeUsers().map(u => [u.login, `${u.name}（${u.login}）`]), cur)}</select>`;
ACTIONS['database.edit'] = d => {
  const i = dmInst(), g = regOf(i, d.db) || {owner: '', remark: ''};
  openModal({title: `${regOf(i, d.db) ? '编辑' : '录入'}数据库信息 · ${esc(d.db)}`, code: 'database.edit', body: `<div class="form">${fld('数据库', `<input value="${esc(d.db)}" readonly>`)}${fld('负责人', ownerSel('dbOwner', g.owner), {req: 1})}${fld('备注', `<input id="dbRemark" value="${esc(g.remark)}" maxlength="120" placeholder="例如：订单主库">`)}</div>${errBox('dbErr')}`, onOk() {
    if (!fv('dbOwner')) return setErr('dbErr', '请选择负责人');
    DBREG[`${i.id}:${d.db}`] = {owner: fv('dbOwner'), remark: fv('dbRemark')};
    audit('database.edit', `登记库信息 ${i.name}/${d.db}：负责人 ${fv('dbOwner')}${fv('dbRemark') ? '，备注「' + fv('dbRemark') + '」' : ''}`); toast('已保存'); later(rerender); }});
};
function collOpts(i, cs, cur) { const ok = c => !c.includes('0900') || (i.type === 'MySQL' && /^8\./.test(i.ver)); return (CHARSETS[cs] || []).filter(ok).map(c => `<option ${c === cur ? 'selected' : ''}>${c}</option>`).join(''); }
ACTIONS['database.create'] = () => {
  const i = dmInst();
  openModal({title: `创建数据库 · ${esc(i.name)}`, code: 'database.create', ok: '下一步：确认创建', body: `<div class="form">
    ${fld('数据库名称', `<input id="dcName" maxlength="64" placeholder="order_report" autocomplete="off">`, {req: 1})}
    ${fld('字符集', `<select id="dcCs" data-onchange="dcCs">${opts(Object.keys(CHARSETS).map(c => [c, c]), 'utf8mb4')}</select>`, {req: 1})}
    ${fld('排序规则', `<select id="dcColl">${collOpts(i, 'utf8mb4', defColl(i, 'utf8mb4'))}</select>`, {req: 1})}
    ${fld('负责人', ownerSel('dcOwner', ME.login), {req: 1})}${fld('备注', '<input id="dcRemark" maxlength="120" placeholder="用途 / 业务说明">')}</div>${errBox('dcErr')}`, onOk() {
    const n = fv('dcName'), cs = fv('dcCs'), co = fv('dcColl'), own = fv('dcOwner'), rem = fv('dcRemark');
    if (!/^[A-Za-z]([A-Za-z0-9_-]{0,62}[A-Za-z0-9])?$/.test(n)) return setErr('dcErr', '库名不合法：字母开头、字母或数字结尾，只含字母数字下划线中划线，≤ 64 位');
    if (SYS_DBS.includes(n.toLowerCase())) return setErr('dcErr', `「${n}」是系统库名，不能创建`);
    if (dbRows(i).some(r => r.db.toLowerCase() === n.toLowerCase())) return setErr('dcErr', `库「${n}」已存在`);
    if (!own) return setErr('dcErr', '请选择负责人');
    const sql = `CREATE DATABASE \`${n}\` DEFAULT CHARACTER SET ${cs} COLLATE ${co};`;
    later(() => risky('database.create', `在 ${i.name} 上创建数据库「${n}」`, `将在目标实例上执行建库语句，并登记负责人 <b>${esc(own)}</b>。建库后没有任何账号拥有该库的权限。`, sql, () => {
      (DBEXTRA[i.id] = DBEXTRA[i.id] || []).push({db: n, charset: cs, coll: co}); DBREG[`${i.id}:${n}`] = {owner: own, remark: rem};
      audit('database.create', `创建数据库 ${i.name}/${n}（${cs} / ${co}）`, '成功', sql); toast('数据库已创建'); rerender(); }));
  }});
};
ON.dcCs = (d, el) => { const i = dmInst(); $('#dcColl').innerHTML = collOpts(i, el.value, defColl(i, el.value)); };

/* ============================================================================
   4. 账号管理 accounts
   ========================================================================== */
const AM = {inst: 0, f: '', q: ''};
const AD = {};
const PLATFORM_ACCS = ['dbtool_ro', 'dbtool_rw', 'sa_dbtool'];
const HIGH_PRIVS = ['FILE', 'SUPER', 'SHUTDOWN', 'RELOAD', 'CREATE USER', 'REPLICATION SLAVE', 'GRANT OPTION', 'PROCESS', 'REPLICATION CLIENT', 'ALL PRIVILEGES'];
const WRITE_PRIVS = ['INSERT', 'UPDATE', 'DELETE', 'CREATE', 'ALTER', 'DROP', 'INDEX', 'ALL PRIVILEGES'];
const PG = {data: ['SELECT', 'INSERT', 'UPDATE', 'DELETE'], struct: ['CREATE', 'ALTER', 'INDEX', 'DROP', 'CREATE TEMPORARY TABLES', 'SHOW VIEW', 'CREATE ROUTINE', 'ALTER ROUTINE', 'EXECUTE', 'CREATE VIEW', 'EVENT', 'TRIGGER'],
  admin: ['GRANT OPTION', 'LOCK TABLES', 'REFERENCES'], global: ['FILE', 'PROCESS', 'RELOAD', 'SHUTDOWN', 'SUPER', 'CREATE USER', 'REPLICATION SLAVE', 'REPLICATION CLIENT', 'SHOW DATABASES', 'CREATE TABLESPACE']};
const privGroups = sc => sc === 'global' ? [['数据', PG.data], ['结构', PG.struct], ['管理', PG.admin], ['全局管理', PG.global]] : sc === 'db' ? [['数据', PG.data], ['结构', PG.struct], ['管理', PG.admin]]
  : sc === 'table' ? [['数据', PG.data], ['结构', ['CREATE', 'ALTER', 'INDEX', 'DROP', 'SHOW VIEW', 'CREATE VIEW', 'TRIGGER']], ['管理', ['GRANT OPTION', 'REFERENCES']]] : [['数据', ['SELECT', 'INSERT', 'UPDATE', 'REFERENCES']]];
const privAll = sc => privGroups(sc).flatMap(g => g[1]).filter(p => p !== 'GRANT OPTION');
const amInst = () => { const all = visibleInstances(true); let i = all.find(x => x.id === AM.inst); if (!i) { i = all.find(isMy) || all[0]; AM.inst = i ? i.id : 0; } return i; };
function accOf(i) {
  if (AD[i.id]) return AD[i.id];
  const dbs = dbsOf(i.id).map(d => d.db), key = i.schema && i.schema !== 'view' && i.schema !== 'mssql' ? i.schema : 'app', r = rng(i.id * 101 + 5);
  const G = (obj, privs, cols) => Object.assign({obj, privs}, cols ? {cols} : {});
  const A = (user, host, grants, o) => Object.assign({user, host, grants, locked: false, reg: null, created: `2025-0${R(r, 1, 9)}-1${R(r, 0, 9)}`}, o);
  const bt = d => '`' + d + '`.*', RW = ['SELECT', 'INSERT', 'UPDATE', 'DELETE'];
  const owner = ({1: 'rd_zhao', 2: 'rd_zhou', 3: 'dev_liu', 4: 'dba_li'})[i.rgs[0]] || 'dba_li';
  const list = [
    A('root', 'localhost', [G('*.*', ['ALL PRIVILEGES', 'GRANT OPTION'])], {builtin: 1}),
    A('mysql.sys', 'localhost', [G('`sys`.*', ['TRIGGER']), G('`mysql`.`user`', ['SELECT'])], {builtin: 1, locked: true}),
    A('mysql.session', 'localhost', [G('*.*', ['SUPER']), G('`performance_schema`.*', ['SELECT'])], {builtin: 1, locked: true}),
    A('mysql.infoschema', 'localhost', [G('*.*', ['SELECT'])], {builtin: 1, locked: true}),
    A(i.user, '10.1.%', [G('*.*', i.user === 'dbtool_rw' ? [...RW, 'CREATE', 'ALTER', 'INDEX', 'DROP', 'PROCESS', 'SHOW VIEW'] : ['SELECT', 'PROCESS', 'SHOW VIEW', 'REPLICATION CLIENT'])], {platform: 1, reg: {owner: 'dba_li', purpose: '平台连接账号（查询 / 采集 / 巡检）', expire: ''}})];
  if (dbs.length) {
    list.push(A(`${key}_rw`, '10.20.%', dbs.slice(0, 2).map(d => G(bt(d), RW)), {reg: {owner, purpose: '应用读写', expire: ''}}));
    list.push(A(`${key}_ro`, '10.20.%', dbs.map(d => G(bt(d), ['SELECT'])), {reg: {owner, purpose: '应用只读 / 从库读流量', expire: ''}}));
    list.push(A('report_ro', '10.20.2.%', [G(bt(dbs[0]), ['SELECT', 'SHOW VIEW'])], {reg: {owner: 'dba_li', purpose: 'BI 报表只读', expire: '2026-12-31'}}));
    list.push(A('etl_job', '10.20.4.%', [G(bt(dbs[0]), [...RW, 'CREATE TEMPORARY TABLES', 'LOCK TABLES'])], {reg: {owner: 'dba_li', purpose: 'ETL 同步任务', expire: ''}}));
    if (i.id % 3 === 1) list.push(A('dev_tmp', '%', [G(bt(dbs[0]), [...RW, 'CREATE', 'ALTER', 'DROP'])], {}));
    if (i.role === 'master') list.push(A('fix_zhao', '10.1.3.%', [G(bt(dbs[0]), [...RW, 'ALTER', 'INDEX'])], {reg: {owner: 'rd_zhao', purpose: '临时数据订正（审批单 #1039）', expire: '2026-10-10'}}));
    if (i.id === 3) list.push(A('bi_export', '10.20.%', [G('*.*', ['SELECT', 'FILE'])], {reg: {owner: 'dba_li', purpose: '对账导出（待收敛）', expire: ''}}));
  }
  if (i.id === 4 || i.id === 9) list.push(A('ops_admin', '%', [G('*.*', ['ALL PRIVILEGES'])], {}));
  if (i.id === 4) list.push(A('legacy_batch', '10.20.9.%', [G(bt(dbs[0] || 'user_db'), RW)], {locked: true, reg: {owner: 'dev_liu', purpose: '遗留批处理（待下线）', expire: '2026-09-30'}}));
  return AD[i.id] = list;
}
const accKey = a => `${a.user}@${a.host}`;
const accFind = (i, k) => accOf(i).find(a => accKey(a) === k);
function riskOf(a) {
  if (a.builtin || a.platform) return [];
  const ps = a.grants.flatMap(g => g.privs), out = [], hi = [...new Set(ps.filter(p => ['FILE', 'SUPER', 'SHUTDOWN', 'RELOAD', 'CREATE USER', 'REPLICATION SLAVE', 'GRANT OPTION'].includes(p)))];
  if (a.host === '%' && ps.some(p => WRITE_PRIVS.includes(p))) out.push('任意主机 % 且拥有写 / 结构变更权限');
  if (ps.includes('ALL PRIVILEGES')) out.push('拥有 ALL PRIVILEGES');
  if (hi.length) out.push('含高危权限 ' + hi.join('、'));
  return out;
}
const accProt = (i, a) => a.builtin ? '数据库内置账号，受保护' : (a.platform || PLATFORM_ACCS.includes(a.user) || a.user === i.user) ? '平台连接该实例的账号，受保护' : '';
const grantLine = g => `GRANT ${g.cols ? g.privs.map(p => `${p} (${g.cols.join(', ')})`).join(', ') : g.privs.join(', ')} ON ${g.obj};`;
const expTag = e => { if (!e) return '<span class="muted">不限期</span>'; const n = Math.round((new Date(e) - new Date(TODAY)) / 864e5); return n < 0 ? `${esc(e)} ${tag('已过期', 'bad')}` : n <= 7 ? `${esc(e)} ${tag(n + ' 天后到期', 'warn')}` : esc(e); };
function amTable(i) {
  const q = AM.q.toLowerCase();
  const rows = accOf(i).filter(a => (!q || (accKey(a) + (a.reg ? a.reg.purpose + a.reg.owner : '')).toLowerCase().includes(q)) &&
    (!AM.f || (AM.f === 'reg' ? !!a.reg : AM.f === 'unreg' ? !a.reg : AM.f === 'risk' ? riskOf(a).length : a.locked))).sort((a, b) => (a.builtin ? 2 : a.platform ? 1 : 0) - (b.builtin ? 2 : b.platform ? 1 : 0));
  return tbl(['账号', '权限', '登记信息（使用人 / 用途 / 到期）', '操作'], rows, {
    attrs: a => riskOf(a).length ? ' style="background:var(--warn-soft)"' : '',
    cells: a => { const p = accProt(i, a), rk = riskOf(a), k = accKey(a), dd = {k}, why = p;
      const b = (code, label, cls, x) => bx(code, label, {cls: 'sm ' + (cls || ''), data: Object.assign({k}, x || {})}, why);
      return [`<div class="mono"><b>'${esc(a.user)}'@'${esc(a.host)}'</b></div><div class="row" style="gap:4px;margin-top:2px">${a.builtin ? tag('内置', 'info') : ''}${a.platform || (!a.builtin && p) ? tag('平台自用', 'info') : ''}${rk.length ? '<span class="tag warn">⚠ 高危</span>' : ''}${a.locked ? tag('已锁定', 'bad') : ''}${a.reg || a.builtin ? '' : tag('未录入')}</div>${rk.length ? `<div class="small" style="color:var(--warn)">${esc(rk.join('；'))}</div>` : ''}`,
        `<div class="mono small">${a.grants.slice(0, 3).map(g => esc(trunc(grantLine(g), 90))).join('<br>')}${a.grants.length > 3 ? `<br><a data-on="amView" data-k="${esc(k)}">… 共 ${a.grants.length} 条，查看全部</a>` : ''}${!a.grants.length ? '<span class="muted">无任何权限（USAGE）</span>' : ''}</div>`,
        a.builtin ? '<span class="muted small">数据库内置，无需登记</span>' : a.reg ? `${esc(plat(a.reg.owner))} <span class="small muted">${esc(a.reg.owner)}</span><div class="small">${esc(a.reg.purpose)}</div><div class="small">到期：${expTag(a.reg.expire)}</div>` : '<span class="muted">未登记</span>',
        `<div class="ops">${b('account.edit', a.reg ? '编辑' : '录入')}${b('account.grant', '授权 / 回收', 'primary')}${b('account.pwd', '改密')}${b('account.lock', a.locked ? '解锁' : '锁定')}${b('account.delete', '删除', 'danger')}</div>${p ? `<div class="small muted" style="margin-top:2px">${esc(p.split('，')[0])}</div>` : ''}`]; }});
}
pages.accounts = {
  render() {
    const i = amInst(); if (!i) return notice('没有可用的实例。', 'warn', '!');
    const all = isMy(i) ? accOf(i) : [];
    return `<div class="card"><h2>账号管理<span class="right">${B('account.create', '＋ 创建账号', {cls: 'primary'})}</span></h2>
      <div class="toolbar"><b>实例</b><select data-onchange="amInst" style="min-width:300px">${instOpts(visibleInstances(true), i.id)}</select>
        <select data-onchange="amF" data-k="f"><option value="">全部账号</option>${opts([['reg', '已录入'], ['unreg', '未录入'], ['risk', '高危账号'], ['locked', '已锁定']], AM.f)}</select>
        <input type="search" placeholder="搜索账号 / 主机 / 使用人" value="${esc(AM.q)}" data-oninput="amF" data-k="q" style="width:220px"><span class="sp"></span>
        ${isMy(i) ? `<span class="small muted">共 ${all.length} 个 · <span style="color:var(--warn)">高危 ${all.filter(a => riskOf(a).length).length}</span></span>` : ''}</div>
      ${!isMy(i) ? unsupported(i, '账号管理') : i.status === 'bad' ? connFail(i) : `<div id="amTbl">${amTable(i)}</div>`}</div>`;
  }
};
ON.amInst = (d, el) => { AM.inst = +el.value; rerender(); };
ON.amF = (d, el) => { AM[d.k] = el.value; part('amTbl', amTable(amInst())); };
ON.amView = d => { const i = amInst(), a = accFind(i, d.k); openDrawer(accKey(a), `<div class="small muted" style="margin-bottom:6px">SHOW GRANTS FOR '${esc(a.user)}'@'${esc(a.host)}'</div>${code(a.grants.map(grantLine).join('\n') || '-- 无任何权限（USAGE）', 'sql')}`); };

const pwdErr = (p, p2) => p.length < 8 || p.length > 32 ? '密码长度需 8–32 位' : !/^[\x21-\x7e]+$/.test(p) ? '密码只能包含可见 ASCII 字符（不含空格）' : (!/[A-Za-z]/.test(p) || !/\d/.test(p) || !/[^A-Za-z0-9]/.test(p)) ? '密码需同时包含英文、数字、特殊符号' : p !== p2 ? '两次输入的密码不一致' : '';
const masked = (u, h) => `'${u}'@'${h}'`;
/* 创建账号（密码只用于校验与“加密保存”，不进入任何日志 / 审计 / 界面）*/
ACTIONS['account.create'] = () => {
  const i = amInst(); if (!isMy(i)) return toast('该类型实例暂不支持账号管理');
  openModal({title: `创建账号 · ${esc(i.name)}`, code: 'account.create', ok: '下一步：确认创建', body: `<div class="form">
    ${fld('账号', '<input id="acU" maxlength="16" autocomplete="off" placeholder="1–16 位，字母开头">', {req: 1})}
    ${fld('主机（多个用 | 分隔）', '<input id="acH" value="10.20.%">', {req: 1})}
    ${fld('密码', '<input id="acP" type="password" autocomplete="new-password" placeholder="8–32 位，含字母数字符号">', {req: 1})}${fld('确认密码', '<input id="acP2" type="password" autocomplete="new-password">', {req: 1})}
    ${fld('使用人', ownerSel('acO', ME.login), {req: 1})}${fld('用途', '<input id="acR" maxlength="80" placeholder="例如：订单服务读写">')}${fld('到期日（可不填）', '<input id="acE" placeholder="YYYY-MM-DD">')}</div>${errBox('acErr')}`, onOk() {
    const u = fv('acU'), hs = fv('acH').split('|').map(x => x.trim()).filter(Boolean), own = fv('acO'), pur = fv('acR'), exp = fv('acE'), pe = pwdErr($('#acP').value, $('#acP2').value);
    if (!/^[A-Za-z][A-Za-z0-9_]{0,15}$/.test(u)) return setErr('acErr', '账号需 1–16 位，字母开头，只含字母数字下划线');
    if (u === 'root' || u.startsWith('mysql.') || PLATFORM_ACCS.includes(u)) return setErr('acErr', `「${u}」是保留 / 平台自用账号名，不能创建`);
    if (!hs.length || hs.some(h => !/^(localhost|%|(\d{1,3}|%)(\.(\d{1,3}|%)){0,3})$/.test(h))) return setErr('acErr', '主机格式不对：IP / 网段（可含 %），多个用 | 分隔');
    const dup = hs.find(h => accOf(i).some(a => a.user === u && a.host === h)); if (dup) return setErr('acErr', `账号 '${u}'@'${dup}' 已存在`);
    if (pe) return setErr('acErr', pe);
    if (!own) return setErr('acErr', '请选择使用人');
    if (exp && !/^\d{4}-\d{2}-\d{2}$/.test(exp)) return setErr('acErr', '到期日格式应为 YYYY-MM-DD');
    later(() => risky('account.create', `创建账号 ${u}（${hs.length} 个主机）`, `将在 <b>${esc(i.name)}</b> 上创建账号，密码加密保存，之后不再显示。${hs.includes('%') ? '<br><b>注意：主机为 % 表示任意来源均可连接。</b>' : ''}`,
      hs.map(h => `CREATE USER ${masked(u, h)} IDENTIFIED BY '******';`).join('\n'), () => {
        hs.forEach(h => accOf(i).push({user: u, host: h, grants: [], locked: false, created: TODAY, reg: {owner: own, purpose: pur, expire: exp}}));
        audit('account.create', `创建账号 ${hs.map(h => masked(u, h)).join('、')}（使用人 ${own}）`, '成功', i.name); toast('账号已创建（尚无任何权限）'); rerender(); }));
  }});
};
ACTIONS['account.edit'] = d => {
  const i = amInst(), a = accFind(i, d.k), g = a.reg || {owner: '', purpose: '', expire: ''};
  openModal({title: `${a.reg ? '编辑' : '录入'}账号登记信息`, code: 'account.edit', body: `<div class="form">
    ${fld('账号', `<input class="mono" value="${esc(masked(a.user, a.host))}" readonly>`)}${fld('使用人', ownerSel('aeO', g.owner), {req: 1})}${fld('用途', `<input id="aeR" value="${esc(g.purpose)}" maxlength="80">`)}${fld('到期日（可不填）', `<input id="aeE" value="${esc(g.expire)}" placeholder="YYYY-MM-DD">`)}</div>${errBox('aeErr')}`, onOk() {
    if (!fv('aeO')) return setErr('aeErr', '请选择使用人');
    if (fv('aeE') && !/^\d{4}-\d{2}-\d{2}$/.test(fv('aeE'))) return setErr('aeErr', '到期日格式应为 YYYY-MM-DD');
    a.reg = {owner: fv('aeO'), purpose: fv('aeR'), expire: fv('aeE')};
    audit('account.edit', `登记账号信息 ${masked(a.user, a.host)}：使用人 ${a.reg.owner}${a.reg.expire ? '，到期 ' + a.reg.expire : ''}`, '成功', i.name); toast('已保存'); later(rerender); }});
};
ACTIONS['account.pwd'] = d => {
  const i = amInst(), a = accFind(i, d.k);
  openModal({title: `修改密码 · ${esc(masked(a.user, a.host))}`, code: 'account.pwd', ok: '下一步：确认修改', body: `${notice('会修改<b>库里账号的真实密码</b>，使用该账号的应用需同步更换。', 'warn', '!')}<div class="form">
    ${fld('新密码', '<input id="pwA" type="password" autocomplete="new-password" placeholder="8–32 位，含字母数字符号">', {req: 1})}${fld('确认密码', '<input id="pwB" type="password" autocomplete="new-password">', {req: 1})}</div>${errBox('pwErr')}`, onOk() {
    const e = pwdErr($('#pwA').value, $('#pwB').value); if (e) return setErr('pwErr', e);
    later(() => risky('account.pwd', `修改 ${masked(a.user, a.host)} 的密码`, `实例 <b>${esc(i.name)}</b>。`, `ALTER USER ${masked(a.user, a.host)} IDENTIFIED BY '******';`, () => {
      audit('account.pwd', `修改账号密码 ${masked(a.user, a.host)}`, '成功', i.name); toast('密码已修改'); rerender(); }));
  }});
};
ACTIONS['account.lock'] = d => {
  const i = amInst(), a = accFind(i, d.k), lock = !a.locked;
  risky('account.lock', `${lock ? '锁定' : '解锁'}账号 ${masked(a.user, a.host)}`, lock ? '锁定后该账号无法再登录，已建立的连接不受影响。' : '解锁后该账号可重新登录。', `ALTER USER ${masked(a.user, a.host)} ACCOUNT ${lock ? 'LOCK' : 'UNLOCK'};`, () => {
    a.locked = lock; audit('account.lock', `${lock ? '锁定' : '解锁'}账号 ${masked(a.user, a.host)}`, '成功', i.name); toast(lock ? '已锁定' : '已解锁'); rerender(); });
};
ACTIONS['account.delete'] = d => {
  const i = amInst(), a = accFind(i, d.k);
  risky('account.delete', `删除账号 ${masked(a.user, a.host)}`, `将从 <b>${esc(i.name)}</b> 删除该账号及其全部权限，并删除平台里的登记信息。<b>不可恢复</b>，使用它的应用将无法连接。`, `DROP USER ${masked(a.user, a.host)};`, () => {
    AD[i.id].splice(AD[i.id].indexOf(a), 1); audit('account.delete', `删除账号 ${masked(a.user, a.host)}`, '成功', i.name); toast('账号已删除'); rerender(); }, accKey(a));
};

/* ---- 授权 / 回收：结构化选项 → 服务端生成语句 → 预览 → 二次确认 ---- */
const AG = {k: '', op: 'grant', scope: 'db', dbs: [], table: '', cols: [], privs: []};
const agObjs = (st, esc_) => st.scope === 'global' ? ['*.*'] : st.scope === 'db' ? st.dbs.map(d => '`' + (esc_ ? d.replace(/_/g, '\\_') : d) + '`.*') : ['`' + st.dbs[0] + '`.`' + st.table + '`'];
function agStmts(a, st) {
  const pv = st.scope === 'column' ? st.privs.map(p => `${p} (${st.cols.map(c => '`' + c + '`').join(', ')})`).join(', ') : st.privs.join(', ');
  return agObjs(st, true).map(o => st.op === 'grant' ? `GRANT ${pv} ON ${o} TO ${masked(a.user, a.host)};` : `REVOKE ${pv} ON ${o} FROM ${masked(a.user, a.host)};`);
}
function agCheck(st) {
  if (st.scope === 'db' && !st.dbs.length) return '请至少选择一个数据库';
  if ((st.scope === 'table' || st.scope === 'column') && (!st.dbs[0] || !st.table)) return '请选择数据库和表';
  if (st.scope === 'column' && !st.cols.length) return '请至少选择一列';
  if (!st.privs.length) return '请至少选择一项权限';
  const allow = new Set(privGroups(st.scope).flatMap(g => g[1]).concat(['global', 'db'].includes(st.scope) ? ['ALL PRIVILEGES'] : []));
  const badP = st.privs.find(p => !allow.has(p)); return badP ? `权限「${badP}」不在该范围的白名单里` : '';
}
function agBody() {
  const i = amInst(), a = accFind(i, AG.k), dbs = dbsOf(i.id).map(d => d.db).concat((DBEXTRA[i.id] || []).map(x => x.db));
  const tables = AG.dbs[0] ? tablesOf(i.id, AG.dbs[0]).filter(t => !t.view) : [], cols = (tables.find(t => t.n === AG.table) || {cols: []}).cols;
  const err = agCheck(AG), stm = err ? [] : agStmts(a, AG), warn = [...new Set(AG.privs.filter(p => HIGH_PRIVS.includes(p) && p !== 'PROCESS' && p !== 'REPLICATION CLIENT'))];
  const pg = privGroups(AG.scope), allBox = ['global', 'db'].includes(AG.scope) ? `<label class="chk" style="margin-bottom:6px"><input type="checkbox" class="agPr" value="ALL PRIVILEGES" data-onchange="agSet" data-k="priv" ${AG.privs.includes('ALL PRIVILEGES') ? 'checked' : ''}><b style="color:var(--bad)">ALL PRIVILEGES</b> <span class="small muted">全部权限（高危）</span></label>` : '';
  return `<div class="cols-2"><div><h4 style="margin:0 0 8px">对象选择</h4><div class="form" style="grid-template-columns:90px minmax(0,1fr)">
      ${fld('操作类型', `<select data-onchange="agSet" data-k="op">${opts([['grant', '赋权 GRANT'], ['revoke', '回收 REVOKE']], AG.op)}</select>`)}
      ${fld('权限范围', `<select data-onchange="agSet" data-k="scope">${opts([['global', '全局权限 *.*'], ['db', '库权限'], ['table', '表权限'], ['column', '列权限']], AG.scope)}</select>`)}
      ${AG.scope === 'db' ? fld('数据库', `<div style="max-height:150px;overflow:auto;border:1px solid var(--border);border-radius:8px;padding:6px 8px">${dbs.map(d => `<label class="chk" style="display:flex"><input type="checkbox" class="agDb" value="${esc(d)}" data-onchange="agSet" data-k="db" ${AG.dbs.includes(d) ? 'checked' : ''}>${esc(d)}</label>`).join('') || '<span class="muted small">该实例没有库</span>'}</div>`) : ''}
      ${AG.scope === 'table' || AG.scope === 'column' ? fld('数据库', `<select data-onchange="agSet" data-k="dbsel"><option value="">— 选择 —</option>${opts(dbs.map(d => [d, d]), AG.dbs[0])}</select>`) + fld('表', `<select data-onchange="agSet" data-k="table"><option value="">— 选择 —</option>${opts(tables.map(t => [t.n, t.n]), AG.table)}</select>`) : ''}
      ${AG.scope === 'column' && AG.table ? fld('列', `<div style="max-height:130px;overflow:auto">${cols.map(c => `<label class="chk" style="display:flex"><input type="checkbox" class="agCol" value="${esc(c.n)}" data-onchange="agSet" data-k="cols" ${AG.cols.includes(c.n) ? 'checked' : ''}>${esc(c.n)} <span class="small muted">${esc(c.t)}</span></label>`).join('')}</div>`) : ''}
      ${AG.scope === 'global' ? fld('说明', '<span class="small" style="color:var(--bad)">全局权限对该实例<b>所有库</b>生效</span>') : ''}</div></div>
    <div><h4 style="margin:0 0 8px">权限选择</h4>${allBox}${pg.map(([n, ps]) => `<div style="margin-bottom:8px"><div class="small muted">${n}</div><div class="row" style="gap:4px 12px">${ps.map(p => `<label class="chk"><input type="checkbox" class="agPr" value="${p}" data-onchange="agSet" data-k="priv" ${AG.privs.includes(p) ? 'checked' : ''} ${AG.privs.includes('ALL PRIVILEGES') ? 'disabled' : ''}>${HIGH_PRIVS.includes(p) && p !== 'PROCESS' && p !== 'REPLICATION CLIENT' ? `<span style="color:var(--bad)">${p}</span>` : p}</label>`).join('')}</div></div>`).join('')}</div></div>
    <div class="small muted" style="margin-top:6px">语句预览</div>
    ${stm.length ? code(stm.join('\n'), 'sql') : `<div class="small muted" style="padding:10px 0">${esc(err)}</div>`}
    ${warn.length ? notice(`含高危权限：<b>${warn.join('、')}</b>${AG.scope === 'global' ? '，且作用于全局' : ''}。`, 'bad', '⚠') : ''}${errBox('agErr')}`;
}
ACTIONS['account.grant'] = d => {
  const i = amInst(), a = accFind(i, d.k); Object.assign(AG, {k: d.k, op: 'grant', scope: 'db', dbs: [], table: '', cols: [], privs: []});
  openModal({title: `授权 / 回收 · ${esc(masked(a.user, a.host))}`, wide: true, code: 'account.grant', ok: '下一步：确认执行', okCls: 'danger primary', body: agBody(), onOk() {
    const e = agCheck(AG); if (e) return setErr('agErr', e);
    if (AG.op === 'revoke' && !agObjs(AG).some(o => a.grants.some(g => g.obj === o && (AG.scope !== 'column' || JSON.stringify(g.cols) === JSON.stringify(AG.cols))))) return setErr('agErr', '该账号在所选对象上没有任何权限，无需回收');
    const st = JSON.parse(JSON.stringify(AG)), stm = agStmts(a, st);
    later(() => risky('account.grant', `${st.op === 'grant' ? '授权' : '回收'} ${masked(a.user, a.host)}`, `实例 <b>${esc(i.name)}</b>。${st.op === 'grant' ? '授权立即生效。' : '回收后使用该账号的应用可能立刻报错。'}`, stm.join('\n'), () => {
      agObjs(st).forEach(o => { const same = x => x.obj === o && JSON.stringify(x.cols || null) === JSON.stringify(st.scope === 'column' ? st.cols : null); let g = a.grants.find(same);
        if (st.op === 'grant') { if (!g) a.grants.push(g = Object.assign({obj: o, privs: []}, st.scope === 'column' ? {cols: st.cols} : {})); st.privs.forEach(p => g.privs.includes(p) || g.privs.push(p));
          if (g.privs.includes('ALL PRIVILEGES')) g.privs = ['ALL PRIVILEGES'].concat(g.privs.filter(p => p === 'GRANT OPTION')); }
        else if (g) { let ps = g.privs; if (st.privs.includes('ALL PRIVILEGES')) ps = []; else { if (ps.includes('ALL PRIVILEGES')) ps = privAll(st.scope).concat(ps.filter(p => p === 'GRANT OPTION')); ps = ps.filter(p => !st.privs.includes(p)); }
          g.privs = ps; if (!ps.length) a.grants.splice(a.grants.indexOf(g), 1); } });
      audit('account.grant', `${st.op === 'grant' ? '授权' : '回收'} ${masked(a.user, a.host)}：${stm.join(' ')}`, '成功', i.name); toast(st.op === 'grant' ? '授权已执行' : '回收已执行'); rerender(); }));
  }});
};
ON.agSet = (d, el) => {
  const k = d.k;
  if (k === 'op') AG.op = el.value;
  else if (k === 'scope') Object.assign(AG, {scope: el.value, dbs: [], table: '', cols: [], privs: []});
  else if (k === 'db') AG.dbs = checkedVals('agDb');
  else if (k === 'dbsel') Object.assign(AG, {dbs: el.value ? [el.value] : [], table: '', cols: []});
  else if (k === 'table') Object.assign(AG, {table: el.value, cols: []});
  else if (k === 'cols') AG.cols = checkedVals('agCol');
  else if (k === 'priv') { AG.privs = checkedVals('agPr'); if (AG.privs.includes('ALL PRIVILEGES') && el.value === 'ALL PRIVILEGES') AG.privs = ['ALL PRIVILEGES']; }
  $('#modal .mbody').innerHTML = agBody();
};

/* ============================================================================
   5. 参数配置 params
   ========================================================================== */
const PM = {inst: 0, q: '', dev: '', ed: '', pick: '', hist: 'cur'};
const BLACK_PARAMS = new Set(['init_connect', 'plugin_dir', 'datadir', 'secure_file_priv', 'general_log_file', 'slow_query_log_file']);
const RO_PARAMS = new Set(['innodb_log_file_size', 'innodb_buffer_pool_instances']);          // 数据库自己拒绝动态修改（1238）
let PT_ID = 0;
const PTPL = [
  ['max_connections', '151', '2000', '[100-100000]', 1, '允许的最大并发连接数'], ['max_user_connections', '0', '0', '[0-100000]', 1, '单个账号的最大并发连接数，0 表示不限制'],
  ['wait_timeout', '28800', '600', '[30-31536000]', 1, '非交互连接空闲多少秒后被服务端断开'], ['interactive_timeout', '28800', '600', '[30-31536000]', 1, '交互连接的空闲超时（秒）'],
  ['max_allowed_packet', '4194304', '67108864', '[1024-1073741824]', 1, '单个包 / 单条语句允许的最大字节数'], ['innodb_buffer_pool_size', '134217728', '25769803776', '[5242880-…]', 1, 'InnoDB 缓冲池大小（字节），可在线调整'],
  ['innodb_flush_log_at_trx_commit', '1', '1', '[0|1|2]', 1, '事务提交时 redo 刷盘策略：1 最安全，0 / 2 可能丢最近 1 秒事务'], ['sync_binlog', '1', '1', '[0-4294967295]', 1, 'binlog 刷盘频率：1 表示每次提交 fsync（最安全）'],
  ['innodb_io_capacity', '200', '4000', '[100-200000]', 1, 'InnoDB 后台任务 IOPS 预算，SSD 建议调大'], ['innodb_io_capacity_max', '2000', '8000', '[100-200000]', 1, '后台刷脏的 IOPS 上限'],
  ['innodb_lock_wait_timeout', '50', '10', '[1-1073741824]', 1, '行锁等待超时（秒）'], ['innodb_print_all_deadlocks', 'OFF', 'ON', '[ON|OFF]', 1, '把所有死锁信息写入错误日志'],
  ['slow_query_log', 'OFF', 'ON', '[ON|OFF]', 1, '是否开启慢查询日志'], ['long_query_time', '10', '1', '[0-31536000]', 1, '超过该秒数的查询记入慢日志'],
  ['log_queries_not_using_indexes', 'OFF', 'OFF', '[ON|OFF]', 1, '把未使用索引的查询也记入慢日志（日志量可能很大）'], ['table_open_cache', '4000', '4000', '[1-524288]', 1, '表描述符缓存大小'],
  ['thread_cache_size', '9', '100', '[0-16384]', 1, '线程缓存个数，降低高并发下的建连开销'], ['tmp_table_size', '16777216', '67108864', '[1024-…]', 1, '内存临时表上限（字节）'],
  ['sort_buffer_size', '262144', '1048576', '[32768-…]', 1, '单连接排序缓冲（字节），不宜过大'], ['binlog_format', 'ROW', 'ROW', '[ROW|MIXED|STATEMENT]', 0, 'binlog 格式，影响复制与闪回，不开放平台修改'],
  ['transaction_isolation', 'REPEATABLE-READ', 'READ-COMMITTED', '[READ-UNCOMMITTED|READ-COMMITTED|REPEATABLE-READ|SERIALIZABLE]', 0, '全局事务隔离级别（由应用 / DBA 统一规划）'], ['read_only', 'OFF', '', '[ON|OFF]', 0, '只读开关，主从切换时由 DBA 操作，不开放'],
  ['innodb_log_file_size', '50331648', '2147483648', '[4194304-…]', 1, 'redo 日志文件大小（需改配置重启；动态修改会被数据库拒绝）'], ['slow_query_log_file', '', '', '', 1, '慢日志文件路径（平台黑名单参数，修改会被拒绝）'],
  ['init_connect', '', '', '', 1, '连接初始化语句（平台黑名单参数，修改会被拒绝）']].map(a => ({id: ++PT_ID, db: 'MySQL', name: a[0], def: a[1], rec: a[2], range: a[3], editable: !!a[4], desc: a[5]}));
const EXTRA_VARS = i => [['version', i.ver, '服务端版本'], ['version_comment', i.type === 'MariaDB' ? 'mariadb.org binary distribution' : 'MySQL Community Server - GPL', '版本说明'], ['character_set_server', 'utf8mb4', '默认字符集'], ['collation_server', defColl(i, 'utf8mb4'), '默认排序规则'],
  ['default_storage_engine', 'InnoDB', '默认存储引擎'], ['log_bin', 'ON', '是否开启 binlog'], ['gtid_mode', 'ON', 'GTID 模式'], ['server_id', String(1000 + i.id), '复制用的服务器 ID'], ['time_zone', '+08:00', '时区'],
  ['sql_mode', 'ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ENGINE_SUBSTITUTION', 'SQL 模式'], ['lower_case_table_names', '1', '表名大小写策略（只读）'], ['max_binlog_size', '1073741824', '单个 binlog 文件上限']];
const PHIST = [
  {inst: 1, name: 'long_query_time', old: '10', nu: '1', by: 'dba_li', at: '2026-10-05 21:12'}, {inst: 1, name: 'max_connections', old: '1000', nu: '2000', by: 'dba_li', at: '2026-10-03 10:40'},
  {inst: 3, name: 'innodb_io_capacity', old: '200', nu: '4000', by: 'dba_li', at: '2026-10-04 15:26'}, {inst: 4, name: 'innodb_lock_wait_timeout', old: '50', nu: '10', by: 'admin', at: '2026-10-02 16:05'},
  {inst: 4, name: 'wait_timeout', old: '28800', nu: '600', by: 'dba_li', at: '2026-09-29 11:48'}, {inst: 2, name: 'slow_query_log', old: 'OFF', nu: 'ON', by: 'dba_li', at: '2026-09-28 09:31'},
  {inst: 5, name: 'tmp_table_size', old: '16777216', nu: '67108864', by: 'dba_li', at: '2026-09-25 20:14'}];
const PVAL = {};
function pRange(s) {
  let m = /^\[(-?\d+)-(\d+|…)\]$/.exec(s || ''); if (m) return {t: 'int', min: +m[1], max: m[2] === '…' ? null : +m[2]};
  m = /^\[([^\]|]+(?:\|[^\]|]+)+)\]$/.exec(s || ''); if (m) return {t: 'enum', vals: m[1].split('|')};
  return {t: 'text'};
}
const tplOf = n => PTPL.find(p => p.name === n);
function pvals(i) {
  if (PVAL[i.id]) return PVAL[i.id];
  const r = rng(i.id * 977 + 11), v = {};
  PTPL.forEach(p => { const rg = pRange(p.range), u = r(); let x = p.rec !== '' ? p.rec : p.def;
    if (p.rec !== '' && u > 0.72 && u <= 0.88) x = p.def;
    else if (p.rec !== '' && u > 0.88) x = rg.t === 'int' ? String(Math.max(rg.min, Math.round(+p.rec * (u > .94 ? 1.5 : .5)))) : rg.t === 'enum' ? rg.vals[(rg.vals.indexOf(p.rec) + 1) % rg.vals.length] : x;
    v[p.name] = x; });
  v.slow_query_log = i.slowlog ? 'ON' : 'OFF'; v.read_only = i.role === 'slave' ? 'ON' : 'OFF';
  if (i.id === 4) { v.innodb_flush_log_at_trx_commit = '2'; v.sync_binlog = '0'; }
  PHIST.filter(h => h.inst === i.id).reverse().forEach(h => { v[h.name] = h.nu; });
  return PVAL[i.id] = v;
}
const pmInst = () => { const all = visibleInstances(true); let i = all.find(x => x.id === PM.inst); if (!i) { i = all.find(isMy) || all[0]; PM.inst = i ? i.id : 0; } return i; };
const PM_TABS = [['vars', '运行值', 'param.view'], ['tpl', '参数模板', 'param.template'], ['edit', '修改参数', 'param.edit'], ['hist', '修改历史', 'param.view']];
function pCheck(p, v) {
  if (BLACK_PARAMS.has(p.name)) return `「${p.name}」在平台黑名单里，拒绝修改`;
  if (v === '') return '请输入新值';
  const rg = pRange(p.range);
  if (rg.t === 'int') { if (!/^-?\d+$/.test(v)) return `「${p.name}」是数值型参数，不接受字符串「${v}」`; const n = +v; if (n < rg.min || (rg.max != null && n > rg.max)) return `取值 ${v} 超出允许范围 ${p.range}`; }
  else if (rg.t === 'enum') { if (!rg.vals.some(x => x.toLowerCase() === v.toLowerCase())) return `取值需是 ${rg.vals.join(' / ')} 之一`; }
  else if (/[;'"\\`]/.test(v)) return '文本值不能包含 ; 引号或反斜杠';
  return '';
}
const pNorm = (p, v) => { const rg = pRange(p.range); return rg.t === 'enum' ? rg.vals.find(x => x.toLowerCase() === v.toLowerCase()) : rg.t === 'int' ? String(+v) : v; };
const pSql = (p, v) => `SET GLOBAL ${p.name} = ${p && pRange(p.range).t === 'int' ? v : "'" + String(v).replace(/'/g, "''") + "'"};`;
const pDiff = (i, p) => p.rec !== '' && pvals(i)[p.name] !== p.rec;
function pmVars(i) {
  const v = pvals(i), q = PM.q.toLowerCase();
  const rows = PTPL.map(p => ({p, extra: 0, name: p.name, val: v[p.name]})).concat(EXTRA_VARS(i).map(([n, val, desc]) => ({extra: 1, name: n, val, p: {name: n, def: '—', rec: '', range: '', editable: false, desc}})));
  const list = rows.filter(r => (!q || r.name.includes(q) || r.p.desc.includes(q)) && (!PM.dev || (!r.extra && pDiff(i, r.p))) && (!PM.ed || (PM.ed === 'yes') === (!r.extra && r.p.editable)));
  return tbl(['参数名', '运行值', '默认值', '推荐值（模板）', '可修改值', '允许修改', '描述', ''], list, {
    attrs: r => !r.extra && pDiff(i, r.p) ? ' style="background:var(--warn-soft)"' : '',
    cells: r => { const p = r.p, dv = !r.extra && pDiff(i, p), why = r.extra ? '不在参数模板里' : !p.editable ? '模板里标为不可修改' : '';
      return [`<span class="mono"><b>${esc(p.name)}</b></span>${r.extra ? ' <span class="tag">未在模板中</span>' : ''}`, `<span class="mono ${dv ? 'b' : ''}" style="word-break:break-all">${esc(trunc(r.val, 48))}</span>${dv ? ' <span class="tag warn">偏离推荐</span>' : ''}`, `<span class="mono small">${esc(p.def || '—')}</span>`, `<span class="mono small">${esc(p.rec || '—')}</span>`, `<span class="mono small">${esc(p.range || '—')}</span>`,
        r.extra ? '<span class="muted">—</span>' : p.editable ? tag('可修改', 'ok') : tag('不可修改'), `<span class="small" style="display:inline-block;min-width:180px">${esc(p.desc)}</span>`, bx('param.edit', '修改', {cls: 'sm', data: {n: p.name}}, why)]; }});
}
function pmTabVars(i) {
  const v = pvals(i), nDev = PTPL.filter(p => pDiff(i, p)).length;
  return `<div class="card"><h2>运行值 <span class="sub">共 ${PTPL.length + EXTRA_VARS(i).length} 项</span><span class="right">${nDev ? tag(`${nDev} 项偏离模板推荐值`, 'warn') : tag('与模板推荐值一致', 'ok')}</span></h2>
    <div class="toolbar"><input type="search" placeholder="搜索参数名 / 描述" value="${esc(PM.q)}" data-oninput="pmF" data-k="q" style="width:240px">
      <select data-onchange="pmF" data-k="dev"><option value="">全部参数</option><option value="1" ${PM.dev ? 'selected' : ''}>仅看偏离推荐值</option></select>
      <select data-onchange="pmF" data-k="ed"><option value="">允许修改：全部</option>${opts([['yes', '允许修改：是'], ['no', '允许修改：否']], PM.ed)}</select></div>
    <div id="pmVars">${pmVars(i)}</div></div>`;
}
function pmTabTpl() {
  return `<div class="card"><h2>参数模板 <span class="sub">${PTPL.length} 个</span><span class="right">${B('param.template', '＋ 添加参数模板', {cls: 'primary'})}</span></h2>
    ${tbl(['数据库类型', '参数名', '默认值', '推荐值', '可修改值', '允许修改', '描述', ''], PTPL, {cells: p => [esc(p.db), `<span class="mono"><b>${esc(p.name)}</b></span>`, `<span class="mono small">${esc(p.def || '—')}</span>`, `<span class="mono small">${esc(p.rec || '—')}</span>`, `<span class="mono small">${esc(p.range || '（自由文本）')}</span>`,
      (p.editable ? tag('可修改', 'ok') : tag('不可修改')) + (BLACK_PARAMS.has(p.name) ? ' ' + tag('黑名单', 'bad') : RO_PARAMS.has(p.name) ? ' ' + tag('需重启', 'warn') : ''), `<span class="small">${esc(p.desc)}</span>`, B('param.template', '编辑', {cls: 'sm', data: {id: p.id}})]})}</div>`;
}
function pmTabEdit(i) {
  const eds = PTPL.filter(p => p.editable), p = tplOf(PM.pick) && tplOf(PM.pick).editable ? tplOf(PM.pick) : eds[0], cur = pvals(i)[p.name];
  return `<div class="card"><h2>修改参数</h2>
    <div class="form" style="max-width:720px">${fld('参数', `<select id="pmSel" data-onchange="pmPick">${opts(eds.map(x => [x.name, x.name]), p.name)}</select>`, {req: 1})}
      ${fld('当前运行值', `<span class="mono b">${esc(cur || '（空）')}</span> ${pDiff(i, p) ? tag('偏离推荐值 ' + p.rec, 'warn') : ''}`)}${fld('说明', `<span class="small">${esc(p.desc)}</span>`)}
      ${fld('默认 / 推荐', `<span class="mono small">默认 ${esc(p.def || '—')} · 推荐 ${esc(p.rec || '—')}</span>`)}${fld('可修改值', `<span class="mono">${esc(p.range || '（自由文本）')}</span>`)}
      ${fld('新值', `<input id="pmNew" class="mono" value="${esc(PM.val || '')}" placeholder="${esc(p.rec || '新值')}" autocomplete="off">`, {req: 1})}</div>
    <div class="row" style="margin-top:12px">${B('param.edit', '预览并执行', {cls: 'primary', data: {run: 1}})}</div>${errBox('pmErr')}</div>`;
}
function pmTabHist(i) {
  const rows = PHIST.filter(h => PM.hist === 'all' || h.inst === i.id);
  return `<div class="card"><h2>修改历史 <span class="sub">${rows.length} 条</span><span class="right"><select data-onchange="pmF" data-k="hist">${opts([['cur', '仅当前实例'], ['all', '全部实例']], PM.hist)}</select></span></h2>
    ${tbl(['实例', '参数名', '变更前', '变更后', '执行的语句', '修改人', '修改时间'], rows, {cells: h => { const p = tplOf(h.name); return [esc(instOf(h.inst).name), `<span class="mono">${esc(h.name)}</span>`, `<span class="mono">${esc(h.old)}</span>`, `<span class="mono b">${esc(h.nu)}</span>`, `<span class="mono small">${esc(p ? pSql(p, h.nu) : `SET GLOBAL ${h.name} = ${h.nu};`)}</span>`, esc(h.by), `<span class="small">${esc(h.at)}</span>`]; }})}</div>`;
}
pages.params = {
  render(sub) {
    const i = pmInst(); if (!i) return notice('没有可用的实例。', 'warn', '!');
    const tabsOk = PM_TABS.filter(t => can(t[2])), cur = (tabsOk.find(t => t[0] === sub) || tabsOk[0] || [])[0];
    let body = !tabsOk.length ? notice('没有任何页签的查看权限。', 'warn', '!') : !isMy(i) ? unsupported(i, '参数配置') : i.status === 'bad' ? connFail(i)
      : cur === 'tpl' ? pmTabTpl() : cur === 'edit' ? pmTabEdit(i) : cur === 'hist' ? pmTabHist(i) : pmTabVars(i);
    return `<div class="toolbar"><b>实例</b><select data-onchange="pmInst" style="min-width:300px">${instOpts(visibleInstances(true), i.id)}</select></div>
      ${tabsOk.length ? tabs('params', tabsOk.map(t => [t[0], t[1]]), cur) : ''}${body}`;
  }
};
ON.pmInst = (d, el) => { PM.inst = +el.value; rerender(); };
ON.pmF = (d, el) => { PM[d.k] = d.k === 'dev' ? el.value : el.value; if (d.k === 'hist') rerender(); else part('pmVars', pmVars(pmInst())); };
ON.pmPick = (d, el) => { PM.pick = el.value; PM.val = ''; rerender(); };
ACTIONS['param.edit'] = d => {
  if (!d.run) { PM.pick = d.n; PM.val = ''; return go('params', 'edit'); }                    // 行内“修改”：带到“修改参数”页签
  const i = pmInst(), p = tplOf(fv('pmSel')), nv = fv('pmNew'); PM.val = nv; PM.pick = p.name;
  const e = pCheck(p, nv); if (e) return setErr('pmErr', e);
  const cur = pvals(i)[p.name], norm = pNorm(p, nv);
  if (norm === cur) return setErr('pmErr', `新值与运行值相同（${cur}），不执行`);
  setErr('pmErr', '');
  const sql = pSql(p, norm);
  risky('param.edit', `修改参数 ${p.name}（${i.name}）`, `<div class="mono">${esc(p.name)}：<s>${esc(cur)}</s> → <b>${esc(norm)}</b></div><div class="small" style="margin-top:6px">立即对新连接生效，<b>不会写入配置文件</b>。</div>`, sql, () => {
    if (RO_PARAMS.has(p.name)) {                                    // 数据库对只读参数的报错（1238）把关
      audit('param.edit', `修改参数 ${p.name}：${cur} → ${norm}，数据库拒绝（1238）`, '失败', i.name);
      return later(() => openModal({title: '执行失败', ok: '知道了', cancel: '', body: notice(`目标库拒绝：<span class="mono">ERROR 1238 (HY000): Variable '${esc(p.name)}' is a read only variable</span><br>该参数不能动态修改，运行值未变化。`, 'bad', '✗')}));
    }
    pvals(i)[p.name] = norm; PHIST.unshift({inst: i.id, name: p.name, old: cur, nu: norm, by: ME.login, at: clk().slice(0, 16)}); PM.val = '';
    audit('param.edit', `修改参数 ${p.name}：${cur} → ${norm}`, '成功', i.name); toast('参数已修改');
    later(() => { openModal({title: '已执行', ok: '知道了', cancel: '', body: notice(`<b>${esc(p.name)}</b> 已由 ${esc(cur)} 改为 <b>${esc(norm)}</b>（${esc(i.name)}）。`, 'ok', '✓') + `<div class="small muted">此修改<b>没有写入配置文件</b>，重启后失效：</div>${code(`[mysqld]\n${p.name} = ${norm}`)}`}); rerender(); });
  });
};
ACTIONS['param.template'] = d => {
  const p = d.id ? PTPL.find(x => x.id == d.id) : null, e = !!p, o = p || {db: 'MySQL', name: '', def: '', rec: '', range: '', editable: true, desc: ''};
  openModal({title: e ? `编辑参数模板 · ${esc(o.name)}` : '添加参数模板', code: 'param.template', body: `<div class="form">
    ${fld('数据库类型', `<select id="ptDb"${e ? ' disabled' : ''}>${opts([['MySQL', 'MySQL / MariaDB']], o.db)}</select>`)}${fld('参数名', `<input id="ptName" class="mono" value="${esc(o.name)}" ${e ? 'readonly' : ''} placeholder="innodb_io_capacity">`, {req: 1})}
    ${fld('默认值', `<input id="ptDef" class="mono" value="${esc(o.def)}">`)}${fld('推荐值', `<input id="ptRec" class="mono" value="${esc(o.rec)}" placeholder="留空则不提示偏离">`)}
    ${fld('可修改值', `<input id="ptRange" class="mono" value="${esc(o.range)}" placeholder="[1-65535] 或 [ON|OFF]">`)}
    ${fld('允许在平台修改', `<label class="chk"><input type="checkbox" id="ptEd" ${o.editable ? 'checked' : ''}>允许</label>`)}${fld('描述', `<input id="ptDesc" value="${esc(o.desc)}" maxlength="120">`)}</div>${errBox('ptErr')}`, onOk() {
    const n = fv('ptName'), rg = fv('ptRange'), rec = fv('ptRec'), tmp = {name: n, range: rg};
    if (!/^[a-z][a-z0-9_]*$/.test(n)) return setErr('ptErr', '参数名只能是小写字母、数字、下划线，字母开头');
    if (!e && PTPL.some(x => x.name === n)) return setErr('ptErr', `参数「${n}」已有模板`);
    if (rg && !/^\[(-?\d+)-(\d+|…)\]$/.test(rg) && !/^\[[^\]|]+(\|[^\]|]+)+\]$/.test(rg)) return setErr('ptErr', '可修改值格式不对：范围 [1-65535]（上限可写 …）或枚举 [ON|OFF]');
    if (rg && rec && pRange(rg).t !== 'text') { const m = pCheck(Object.assign({}, tmp), rec); if (m && !BLACK_PARAMS.has(n)) return setErr('ptErr', '推荐值不满足可修改值：' + m); }
    const nu = {db: 'MySQL', name: n, def: fv('ptDef'), rec, range: rg, editable: $('#ptEd').checked, desc: fv('ptDesc')};
    if (e) Object.assign(p, nu); else PTPL.push(Object.assign({id: ++PT_ID}, nu));
    audit('param.template', `${e ? '修改' : '添加'}参数模板 ${n}（${nu.range || '自由文本'}，${nu.editable ? '可修改' : '不可修改'}）`, '成功'); toast('模板已保存'); later(rerender); }});
};

/* ============================================================================
   6. 性能巡检 inspect：报告列表 / 详情 / 定时计划
   MySQL 系（18+1 步，评分）与 SQL Server（27 项，无评分）在同一个列表里；巡检全程只读。
   ========================================================================== */
const MYSQL_CHK = [   // [分类, 检查项, (范围), 阈值, [[权重, 级别, 当前值, 建议]...]]  —— 值可为字符串或 (rng, ctx) => 字符串
  ['连接', '连接使用率', '', '>80% 关注 / >95% 告警', [[10, 'ok', r => R(r, 18, 62) + '%', ''], [2, 'warn', r => R(r, 81, 94) + '%', '连接数接近 max_connections：排查连接泄漏，评估调大 max_connections 或引入连接池'], [1, 'bad', r => R(r, 96, 99) + '%', '连接即将耗尽：立即定位占连接最多的来源并限流，必要时临时调大 max_connections']]],
  ['连接', '活跃线程数', '', '—（仅展示）', [[1, 'ok', r => String(R(r, 6, 48)), '']]],
  ['内存 / 缓冲池', '缓冲池命中率', '', '<99% 关注 / <90% 告警', [[10, 'ok', r => `99.${R(r, 52, 99)}%`, ''], [2, 'warn', r => `98.${R(r, 1, 9)}%`, '热数据超出缓冲池：评估增大 innodb_buffer_pool_size，排查全表扫描冲刷缓冲池'], [1, 'bad', r => `${R(r, 84, 89)}.${R(r, 0, 9)}%`, '命中率过低、磁盘读压力大：优先扩大缓冲池并排查大扫描 SQL']]],
  ['内存 / 缓冲池', '缓冲池使用率', '', '—（仅展示）', [[1, 'ok', r => R(r, 92, 99) + '%', '']]],
  ['查询', '慢查询数（近 24 小时）', '', '>10 关注 / >100 告警', [[8, 'ok', r => String(R(r, 0, 9)), ''], [3, 'warn', r => String(R(r, 11, 90)), '慢 SQL 偏多：到「慢查日志」查看 Top 指纹并获取优化建议'], [1, 'bad', r => String(R(r, 120, 2400)), '慢 SQL 暴增：优先处理 Top3 指纹（加索引 / 改写），必要时限流']]],
  ['查询', '慢日志状态', '', '未启用则关注', [[8, 'ok', '已启用（long_query_time = 1s）', ''], [1, 'warn', '未启用', 'slow_query_log = OFF，慢 SQL 无法被采集：到「参数配置」开启并设置 long_query_time']]],
  ['事务', 'InnoDB 行锁等待', '', '>100 关注 / >1000 告警', [[9, 'ok', r => String(R(r, 0, 60)), ''], [2, 'warn', r => String(R(r, 120, 800)), '行锁竞争偏高：检查热点行更新，缩短事务'], [1, 'bad', r => String(R(r, 1100, 5000)), '行锁等待严重：到「会话管理 → 锁等待」定位阻塞源']]],
  ['事务', 'undo 版本链长度', '', '>10000 关注', [[9, 'ok', r => String(R(r, 200, 4000)), ''], [1, 'warn', r => String(R(r, 12000, 90000)), '存在长事务拖住 purge：检查并结束长时间未提交的事务']]],
  ['I/O', 'InnoDB 日志等待', '', '>0 关注', [[9, 'ok', '0', ''], [1, 'warn', r => String(R(r, 3, 60)), 'redo 日志缓冲不足或刷盘慢：评估增大 innodb_log_buffer_size / 提升磁盘性能']]],
  ['长事务 / MDL', '最长事务时长', '', '>60s 关注 / >300s 告警', [[8, 'ok', r => R(r, 0, 40) + ' s', ''], [2, 'warn', r => R(r, 62, 280) + ' s', '有较长事务：让应用尽快提交，避免大事务'], [1, 'bad', r => R(r, 320, 2400) + ' s', '存在长时间未提交的事务（持锁并撑大 undo）：在「会话管理 → 事务」定位并终止']]],
  ['长事务 / MDL', '元数据锁（MDL）等待', '', '有等待告警', [[9, 'ok', '无', ''], [1, 'bad', r => `${R(r, 1, 6)} 个会话在等待 MDL`, '有会话被 DDL / 长事务阻塞：先找出持有 MDL 的会话，必要时终止；DDL 放到低峰期']]],
  ['自增 ID', '自增列最大使用率', '', '≥70% 关注 / ≥85% 告警', [[8, 'ok', (r, c) => `${c.tbl}.id 已用 ${R(r, 3, 40)}%`, ''], [2, 'warn', (r, c) => `${c.tbl}.id 已用 ${R(r, 70, 84)}%`, '自增列将在较长时间后耗尽：规划改为 bigint unsigned 或分表'], [1, 'bad', (r, c) => `${c.tbl}.id 已用 ${R(r, 86, 97)}%（int，上限 2,147,483,647）`, '预计数十天内耗尽：尽快在线改为 bigint unsigned（SQL 上线 · 在线改表）']]],
  ['无主键表', '无主键表数量', '', '有则关注（含 ≥100 万行的表告警）', [[7, 'ok', '0', ''], [3, 'warn', r => R(r, 1, 5) + ' 张', '无主键表在复制 / 行格式上有风险：补充自增主键'], [1, 'bad', r => R(r, 2, 4) + ' 张，含 ≥100 万行的大表', 'ROW 格式下大表无主键会让从库回放极慢：优先补主键']]],
  ['死锁（7 天）', '死锁次数', '', '>0 关注 / ≥10 告警', [[7, 'ok', '0', ''], [3, 'warn', r => String(R(r, 1, 8)), '出现死锁：检查加锁顺序，统一更新顺序并缩短事务'], [1, 'bad', r => String(R(r, 12, 60)), '死锁频繁：排查热点业务的加锁顺序与隔离级别']]],
  ['慢查询 Top10', 'Top1 平均耗时', '', '≥1s 关注 / ≥10s 告警', [[6, 'ok', r => `${R(r, 1, 9) / 10} s`, ''], [3, 'warn', r => `${R(r, 11, 80) / 10} s`, 'Top1 指纹耗时偏高：获取优化建议并评估加索引'], [1, 'bad', r => `${R(r, 100, 380) / 10} s`, 'Top1 指纹耗时严重：优先优化，必要时下线对应功能']]],
  ['大表 / 碎片', '大表与碎片', '', '>50GB 或 >5000 万行关注；>200GB 告警；碎片 ≥1GB 且 ≥30% 关注', [[6, 'ok', '无超限大表', ''], [3, 'warn', (r, c) => `${c.tbl} 碎片 ${R(r, 30, 41)}%（约 ${R(r, 2, 9)} GB）`, '碎片率高：低峰期 OPTIMIZE TABLE 或走在线改表回收空间'], [1, 'bad', (r, c) => `${c.tbl} 已达 ${R(r, 210, 380)} GB`, '超大表：评估归档 / 分区，避免 DDL 与备份窗口失控']]],
  ['索引', '冗余索引', '', '存在即关注', [[6, 'ok', '无', ''], [4, 'warn', r => `${R(r, 2, 7)} 个`, '冗余索引占用空间并拖慢写入：例如 idx_user 被 idx_user_created 覆盖，评估后删除']]],
  ['字符集', '字符集 / 排序规则一致性', '', '非 utf8mb4 或混用关注', [[7, 'ok', 'utf8mb4 一致', ''], [3, 'warn', r => `${R(r, 2, 9)} 张表与库字符集不一致`, '字符集混用会导致索引失效 / 乱码：统一为 utf8mb4']]],
  ['参数基线', '关键参数基线', '', 'innodb_flush_log_at_trx_commit / sync_binlog 偏离告警', [[8, 'ok', '均在基线内', ''], [1, 'warn', 'binlog_format = MIXED', '建议 binlog_format = ROW'], [1, 'bad', 'innodb_flush_log_at_trx_commit = 2，sync_binlog = 0', '宕机可能丢失最近 1 秒事务且 binlog 不同步：到「参数配置」恢复为 1 / 1']]],
  ['高危账号', '高危账号', '', '匿名 / 远程空密码告警；任意主机高权限关注', [[7, 'ok', '无', ''], [3, 'warn', r => `${R(r, 1, 3)} 个（% 主机 + 写权限）`, '到「账号管理」收敛来源主机并回收多余权限']]],
  ['容量', '数据总大小', '', '—（仅展示）', [[1, 'ok', (r, c) => `${fmtMB(c.mb)}`, '']]],
  ['复制与 binlog', '复制状态', '', 'IO / SQL 线程异常告警；延迟 >60s 关注 / >300s 告警', [[8, 'ok', (r, c) => c.inst.role === 'master' ? `主库，${c.slaves} 个从库连接正常` : `IO / SQL 线程 Yes，延迟 ${R(r, 0, 3)} s`, ''],
    [2, 'warn', (r, c) => c.inst.role === 'master' ? `1 个从库延迟 ${R(r, 70, 250)} s` : `延迟 ${R(r, 70, 250)} s`, '复制延迟偏高：排查大事务 / 从库 IO 压力 / 并行复制配置'], [1, 'bad', (r, c) => c.inst.role === 'master' ? '1 个从库 IO 线程已断开' : 'SQL 线程已停止（Slave_SQL_Running = No）', '复制中断：到从库查看 SHOW REPLICA STATUS 的 Last_Error 并修复']]],
  ['复制与 binlog', 'binlog 总大小', '', '>100GB 关注', [[8, 'ok', r => `${R(r, 8, 60)} GB（${R(r, 8, 40)} 个文件）`, ''], [2, 'warn', r => `${R(r, 105, 240)} GB`, 'binlog 占用偏大：确认保留天数，并在「实例列表」预览后清理']]]];
const MSSQL_CHK = [   // 与 MySQL 同结构，[分类, 检查项, 范围('实例' | '库'), 阈值, 结果]
  ['实例概况', '版本与补丁级别', '实例', '落后 ≥2 个累积更新关注', [[7, 'ok', 'SQL Server 2019 CU18', ''], [3, 'warn', 'SQL Server 2019 CU14（落后 4 个 CU）', '规划在维护窗口升级到最新 CU']]],
  ['实例概况', '运行时长', '实例', '—（仅展示）', [[1, 'ok', r => `${R(r, 3, 120)} 天`, '']]],
  ['实例概况', '关键配置（max server memory / MAXDOP）', '实例', 'MAXDOP=0 或内存未限制关注', [[6, 'ok', 'max server memory 110000 MB · MAXDOP 8', ''], [4, 'warn', 'MAXDOP = 0（不限制并行度）', '建议按 NUMA 节点核数设置 MAXDOP（通常 ≤8）']]],
  ['实例概况', '等待统计 Top', '实例', 'CXPACKET 占比 >30% 关注', [[6, 'ok', 'PAGEIOLATCH_SH 22% · WRITELOG 14%', ''], [3, 'warn', 'CXPACKET 38% · PAGEIOLATCH_SH 20%', '并行等待占比高：评估 MAXDOP 与 cost threshold for parallelism（当前 5，建议 30–50）']]],
  ['CPU', '信号等待占比', '实例', '>15% 关注 / >30% 告警', [[8, 'ok', r => R(r, 2, 11) + '%', ''], [2, 'warn', r => R(r, 16, 28) + '%', 'CPU 调度压力偏大：排查 Top CPU SQL'], [1, 'bad', r => R(r, 31, 55) + '%', 'CPU 严重不足：立即定位高 CPU 语句并限流']]],
  ['CPU', '可运行任务数', '实例', '>10 关注', [[8, 'ok', r => String(R(r, 0, 6)), ''], [2, 'warn', r => String(R(r, 11, 25)), '可运行队列偏长：CPU 争用']]],
  ['CPU', 'CPU Top SQL', '实例', '—（仅展示）', [[1, 'ok', 'EXEC usp_load_fact_sales 占 41% CPU', '']]],
  ['内存', '页生命周期 PLE', '实例', '< 300 秒 × (内存GB/4) 告警', [[7, 'ok', r => `${R(r, 4000, 22000)} 秒`, ''], [1, 'bad', '180 秒（阈值 < 300 秒 × 每 4GB）', '缓冲池被冲刷：排查大扫描报表，评估加内存']]],
  ['内存', '内存授予等待', '实例', '>0 关注', [[8, 'ok', '0', ''], [2, 'warn', r => `${R(r, 1, 6)} 个查询在等待授予`, '大排序 / 哈希查询在抢内存：优化语句或限制并发（Resource Governor）']]],
  ['内存', '各库缓冲池占用', '实例', '—（仅展示）', [[1, 'ok', 'ReportDW 78% · tempdb 9%', '']]],
  ['IO', '文件读写延迟', '库', '读 >20ms 关注 / >50ms 告警', [[7, 'ok', r => `读 ${R(r, 2, 12)} ms · 写 ${R(r, 1, 6)} ms`, ''], [2, 'warn', r => `读 ${R(r, 22, 45)} ms`, '磁盘读延迟偏高：检查存储队列与大扫描'], [1, 'bad', r => `读 ${R(r, 55, 120)} ms`, '磁盘延迟严重：与存储团队确认 IO 路径']]],
  ['IO', '挂起的 IO', '实例', '>0 关注', [[8, 'ok', '0', ''], [2, 'warn', r => String(R(r, 2, 14)), '有挂起 IO：关注存储抖动']]],
  ['IO', '读取最多的 SQL', '实例', '—（仅展示）', [[1, 'ok', 'SELECT … FROM fact_sales（逻辑读 1.2 亿）', '']]],
  ['IO', 'tempdb 使用', '实例', '>70% 关注', [[8, 'ok', r => R(r, 4, 40) + '%', ''], [2, 'warn', r => R(r, 72, 90) + '%', 'tempdb 使用偏高：检查排序 / 临时表 / 版本存储']]],
  ['阻塞与死锁', '当前阻塞链', '实例', '>30s 关注 / >60s 告警', [[8, 'ok', '无', ''], [2, 'bad', 'SPID 58 被 SPID 71 阻塞 94 秒', '先分析 SPID 71 的事务；必要时 KILL，并缩短事务']]],
  ['阻塞与死锁', '长事务', '实例', '>5 分钟关注', [[8, 'ok', '无', ''], [2, 'warn', r => `最长 ${R(r, 6, 40)} 分钟`, '长事务会持锁并阻止日志截断：让应用及时提交']]],
  ['阻塞与死锁', '死锁（对比窗口）', '实例', '>0 关注', [[7, 'ok', '0', ''], [3, 'warn', r => String(R(r, 1, 5)), '出现死锁：从 system_health 事件里取死锁图，调整访问顺序']]],
  ['索引', '缺失索引建议', '库', '影响分 >80 关注', [[6, 'ok', '无高影响缺失索引', ''], [4, 'warn', r => `${R(r, 2, 6)} 个（最高影响分 ${R(r, 82, 99)}）`, '评估创建建议的索引（先在测试环境验证）']]],
  ['索引', '未使用索引', '库', '—（仅展示）', [[1, 'ok', r => `${R(r, 0, 9)} 个`, '']]],
  ['Top SQL', '按执行次数', '实例', '—（仅展示）', [[1, 'ok', 'Top1：每分钟 1.8 万次', '']]],
  ['Top SQL', '按平均耗时', '实例', '>1s 关注', [[7, 'ok', r => `${R(r, 1, 8) / 10} s`, ''], [3, 'warn', r => `${R(r, 12, 90) / 10} s`, 'Top1 平均耗时偏高：检查执行计划与统计信息']]],
  ['数据库与文件', '数据 / 日志文件剩余空间', '库', '剩余 <15% 关注 / <5% 告警', [[7, 'ok', r => `剩余 ${R(r, 25, 60)}%`, ''], [2, 'warn', r => `剩余 ${R(r, 7, 14)}%`, '文件空间紧张：规划扩容或归档'], [1, 'bad', r => `剩余 ${R(r, 1, 4)}%`, '空间即将耗尽：立即扩容']]],
  ['数据库与文件', '日志文件使用', '库', '>70% 关注', [[8, 'ok', r => R(r, 3, 40) + '%', ''], [2, 'warn', r => R(r, 72, 95) + '%', '日志无法截断：检查长事务 / 日志备份']]],
  ['数据库与文件', '备份状态', '库', '全备 >7 天 / 日志备份 >1 天关注；无备份告警', [[7, 'ok', '全备 1 天前 · 日志备份 12 分钟前', ''], [2, 'warn', '全备 9 天前', '全量备份已超期：检查备份作业'], [1, 'bad', 'ReportDW 无任何备份记录', '立即建立备份策略']]],
  ['数据库与文件', '数据库选项', '库', 'auto_close / auto_shrink 开启关注', [[8, 'ok', '正常', ''], [2, 'warn', 'ReportDW：auto_shrink = ON', '关闭 auto_shrink，避免碎片与性能抖动']]],
  ['错误日志与作业', '失败的 Agent 作业（24h）', '实例', '>0 关注', [[7, 'ok', '0', ''], [3, 'warn', r => `${R(r, 1, 3)} 个失败`, '到 SQL Server Agent 查看失败作业的历史']]],
  ['错误日志与作业', '转储文件 / 错误日志', '实例', '存在转储告警', [[5, 'na', '无权限：需要 securityadmin，已跳过', '为巡检账号授予只读权限后可采集'], [4, 'ok', '无', '']]]];
function genItems(inst, seed, opt) {
  if (inst.type === 'Redis') return window.RedisInspect.genItems(inst, seed, opt);          // Redis：检查项与评分见 p_redis.js（对应 internal/inspect/redis_*.go）
  const r = rng(seed), mssql = inst.type === 'MsSQL', f = ({4: 3, 9: 2.5, 3: 1.6, 1: .6})[inst.id] || 1;
  const tb = dbsOf(inst.id).flatMap(d => d.tables.filter(t => !t.view).map(t => `${d.db}.${t.n}`)), db0 = (dbsOf(inst.id)[0] || {db: 'ReportDW'}).db;
  const ctx = {inst, tbl: tb[R(r, 0, Math.max(0, tb.length - 1))] || 'app_db.t_log', mb: dbsOf(inst.id).reduce((a, d) => a + d.tables.reduce((s, t) => s + (t.size || 0), 0), 0), slaves: INSTANCES.filter(x => x.role === 'slave' && inst.name.endsWith('-m') && x.name.startsWith(inst.name.slice(0, -1))).length || 1};
  return (mssql ? MSSQL_CHK : MYSQL_CHK).map(([cat, name, scope, th, outs]) => {
    const ws = outs.map(o => o[1] === 'ok' || o[1] === 'na' ? o[0] : o[0] * f); let u = r() * ws.reduce((a, b) => a + b, 0), k = 0;
    while (k < ws.length - 1 && u >= ws[k]) { u -= ws[k]; k++; }
    const [, st, val, adv] = outs[k];
    return {cat, name, scope: scope === '库' ? db0 : scope, th, st, val: typeof val === 'function' ? val(r, ctx) : val, adv};
  });
}
const INSPECT_REPORTS = [];
window.INSPECT_REPORTS = INSPECT_REPORTS;                                // 首页读取：id / inst / score / at / bad / warn（另有 status、kind；非 finish 的 score / bad / warn 为 0）
let RID = 411;
const durStr = s => `${Math.floor(s / 60)}m${pad2(s % 60)}s`;
function tally(rep) {
  const c = st => rep.items.filter(x => x.st === st).length;
  rep.bad = c('bad'); rep.warn = c('warn'); rep.na = c('na'); rep.ok = c('ok');
  rep.score = rep.status !== 'finish' ? 0 : rep.kind === 'redis' ? window.RedisInspect.health(rep.items, rep.items.bigN).score : Math.max(0, 100 - rep.bad * 6 - rep.warn * 2);   // Redis 按真实扣分表（见 p_redis.js）
}
function mkReport(o) {
  const inst = instById(o.inst), items = genItems(inst, o.id * 131 + o.inst, o), r = rng(o.id), redis = inst.type === 'Redis';
  const rep = Object.assign({kind: inst.type === 'MsSQL' ? 'mssql' : redis ? 'redis' : 'mysql', noScore: inst.type === 'MsSQL', items, total: redis ? window.RedisInspect.steps : items.length, done: redis ? window.RedisInspect.steps : items.length, status: 'finish', trigger: 'manual', plan: '', by: '', err: '', sec: inst.type === 'MsSQL' ? R(r, 150, 330) : redis ? R(r, 3, 14) : R(r, 6, 22)}, o);
  if (rep.status === 'running') { rep.sec = 0; rep.willFail = false; }
  if (rep.status === 'failed') { rep.items = []; rep.sec = 10; rep.err = `连接超时（${inst.host}:${inst.port}）`; }
  if (rep.status === 'cancelled') rep.items = redis ? [] : items.slice(0, rep.done);
  tally(rep); if (rep.status === 'running') { rep.bad = rep.warn = rep.na = rep.ok = rep.score = 0; }
  rep.dur = durStr(rep.sec); return rep;
}
[[412, 1, TODAY + ' 11:18', 'manual', '', 'running', 'dba_li', 11], [411, 9, TODAY + ' 10:40', 'manual', '', 'failed', 'dba_li'],
  [410, 6, TODAY + ' 08:00', 'legacy', '', 'finish', '系统'], [409, 7, TODAY + ' 02:00', 'plan', '生产 MySQL 夜间巡检', 'finish', '系统'], [408, 5, TODAY + ' 02:00', 'plan', '生产 MySQL 夜间巡检', 'finish', '系统'],
  [407, 4, TODAY + ' 02:00', 'plan', '生产 MySQL 夜间巡检', 'finish', '系统'], [406, 3, TODAY + ' 02:00', 'plan', '生产 MySQL 夜间巡检', 'finish', '系统'], [405, 2, TODAY + ' 02:00', 'plan', '生产 MySQL 夜间巡检', 'finish', '系统'],
  [404, 1, TODAY + ' 02:00', 'plan', '生产 MySQL 夜间巡检', 'finish', '系统'], [403, 2, dayOffset(-1) + ' 16:12', 'manual', '', 'finish', 'admin'], [400, 6, dayOffset(-1) + ' 08:00', 'legacy', '', 'finish', '系统'],
  [398, 1, dayOffset(-1) + ' 02:00', 'plan', '生产 MySQL 夜间巡检', 'finish', '系统'], [397, 4, dayOffset(-1) + ' 02:00', 'plan', '生产 MySQL 夜间巡检', 'finish', '系统'], [396, 3, dayOffset(-1) + ' 02:00', 'plan', '生产 MySQL 夜间巡检', 'finish', '系统'],
  [402, 6, dayOffset(-2) + ' 03:00', 'plan', 'SQL Server 每周巡检', 'finish', '系统'], [401, 4, dayOffset(-2) + ' 09:30', 'manual', '', 'finish', 'dba_li'], [399, 6, dayOffset(-2) + ' 08:00', 'legacy', '', 'cancelled', '系统', 8],
  [395, 1, dayOffset(-3) + ' 14:05', 'manual', '', 'finish', 'dba_li'], [394, 4, dayOffset(-2) + ' 02:00', 'plan', '生产 MySQL 夜间巡检', 'finish', '系统'], [393, 3, dayOffset(-2) + ' 02:00', 'plan', '生产 MySQL 夜间巡检', 'finish', '系统']]
  .forEach(([id, inst, at, trigger, plan, status, by, done]) => INSPECT_REPORTS.push(mkReport({id, inst, at, trigger, plan, status, by, done: status === 'running' || status === 'cancelled' ? done : undefined})));
INSPECT_REPORTS.forEach(r => { if (r.done === undefined) r.done = r.total; });
RID = Math.max(...INSPECT_REPORTS.map(r => r.id));
/* Redis 报告种子：「Redis 每日巡检」计划（每天 03:10）最近 5 天 × 3 个实例（缺两份，趋势有断点）+ 一份带大 key 抽样的手动巡检 */
(() => {
  const rows = [];
  for (let d = 0; d <= 4; d++) [11, 12, 13].forEach(inst => { if (!(d === 3 && inst === 12) && !(d === 4 && inst === 13)) rows.push([dayOffset(-d) + ' 03:10', inst, 'plan', 'Redis 每日巡检', '系统', false]); });
  rows.push([dayOffset(-3) + ' 14:20', 11, 'manual', '', 'dba_li', true]);
  rows.sort((a, b) => a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : a[1] - b[1]);
  rows.forEach(([at, inst, trigger, plan, by, bigkey]) => { const rep = mkReport({id: ++RID, inst, at, trigger, plan, status: 'finish', by, bigkey}); rep.done = rep.total; INSPECT_REPORTS.push(rep); });
})();
const instOk = id => { const i = instById(id); return !!i && (ME.super || can('query.admin_run') || i.rgs.some(x => ME.rgs.includes(x))); };
const trigText = r => r.trigger === 'manual' ? `手动 · ${r.by}` : r.trigger === 'legacy' ? '定时 · 原系统配置' : `定时 · ${r.plan}`;
const scoreColor = s => s >= 85 ? 'var(--ok)' : s >= 70 ? 'var(--warn)' : 'var(--bad)';
let INS_TIMER = null;
function ensureTimer() { if (!INS_TIMER) INS_TIMER = setInterval(inTick, 1400); }
function inTick() {                                                    // 异步进度：进行中 → 完成 / 失败（页面不跳转、不回滚滚动条）
  const run = INSPECT_REPORTS.filter(r => r.status === 'running');
  if (!run.length) { clearInterval(INS_TIMER); INS_TIMER = null; return; }
  let ended = false;
  run.forEach(r => {
    r.done = Math.min(r.total, r.done + 2); r.sec += r.kind === 'mssql' ? 14 : 3; r.dur = durStr(r.sec);
    if (r.willFail && r.done >= 3) { r.status = 'failed'; r.items = []; r.done = 0; r.err = `连接超时（${(instById(r.inst) || {}).host}:${(instById(r.inst) || {}).port}）`; r.sec = 10; r.dur = durStr(10); ended = true; }
    else if (r.done >= r.total) { r.status = 'finish'; tally(r); ended = true; }
  });
  const sub = ROUTE.page === 'inspect' ? ROUTE.sub : null;
  if (sub == null) return;
  if (sub.startsWith('detail/')) { const rep = INSPECT_REPORTS.find(x => x.id == sub.slice(7)); if (rep) { if (ended) rerender(); else part('inDetail', inDetailBody(rep)); } }
  else if (!sub || sub === 'list') { if (ended) rerender(); else part('inList', inListTable()); }
}
function startInspect(inst, o) {
  if (INSPECT_REPORTS.some(r => r.inst === inst.id && r.status === 'running')) return null;
  const rep = mkReport(Object.assign({id: ++RID, inst: inst.id, at: clk().slice(0, 16), trigger: 'manual', plan: '', by: ME.login, status: 'running', done: 0}, o || {}));
  rep.willFail = inst.status === 'bad'; INSPECT_REPORTS.unshift(rep); ensureTimer(); return rep;
}

/* ---- 列表 ---- */
const IN = {inst: '', level: '', trig: '', sel: new Set()};
const inFiltered = () => INSPECT_REPORTS.filter(r => instOk(r.inst) && (!IN.inst || r.inst == IN.inst) && (!IN.trig || (IN.trig === 'manual') === (r.trigger === 'manual')) &&
  (!IN.level || (IN.level === 'bad' ? r.status === 'finish' && r.bad > 0 : IN.level === 'warn' ? r.status === 'finish' && !r.bad && r.warn > 0 : IN.level === 'ok' ? r.status === 'finish' && !r.bad && !r.warn : IN.level === 'failed' ? r.status === 'failed' : IN.level === 'cancelled' ? r.status === 'cancelled' : r.status === 'running')))
  .sort((a, b) => a.at < b.at ? 1 : a.at > b.at ? -1 : b.id - a.id);
function inListTable() {
  const list = inFiltered();
  return tbl(['<input type="checkbox" data-onchange="inAll" title="全选">', '报告', '实例', '类型', '巡检时间', '触发', '状态', '健康分', '告警', '关注', '未采集', '耗时', '操作'], list, {cells: r => { const i = instById(r.inst) || {name: '#' + r.inst}, fin = r.status === 'finish', part_ = fin || r.status === 'cancelled';
    return [`<input type="checkbox" class="inCk" value="${r.id}" data-onchange="inCk" ${IN.sel.has(String(r.id)) ? 'checked' : ''}>`, `<a href="#inspect/detail/${r.id}"><b>#${r.id}</b></a>`, `<span class="nowrap">${esc(i.name)}</span>`, r.kind === 'mssql' ? tag('SQL Server', 'info') : tag(i.type || 'MySQL'), `<span class="small nowrap">${esc(r.at)}</span>`, `<span class="small">${esc(trigText(r))}</span>`,
      r.status === 'running' ? `${tag('进行中', 'info')} <span class="small muted">${r.done} / ${r.total} 项</span>${meter(Math.round(r.done / r.total * 100))}` : fin ? tag('已完成', 'ok') : r.status === 'failed' ? `${tag('失败', 'bad')}<div class="small muted">${esc(r.err)}</div>` : `${tag('已取消')} <span class="small muted">${r.done} / ${r.total} 项</span>`,
      fin && !r.noScore ? `<b style="color:${scoreColor(r.score)}">${r.score}</b>` : '<span class="muted">—</span>',
      part_ ? (r.bad ? `<span class="tag bad">${r.bad}</span>` : '0') : '–', part_ ? (r.warn ? `<span class="tag warn">${r.warn}</span>` : '0') : '–', part_ ? r.na : '–', r.dur,
      `<div class="ops">${r.status === 'running' ? B('inspect.cancel', '取消', {cls: 'sm', data: {id: r.id}}) : `<a class="btn sm" href="#inspect/detail/${r.id}">${r.status === 'failed' ? '详情' : '查看报告'}</a>`}${r.status === 'failed' || r.status === 'cancelled' ? B('inspect.run', '重新巡检', {cls: 'sm', data: {inst: r.inst}}) : ''}${B('inspect.delete', '删除', {cls: 'sm danger', data: {id: r.id}})}</div>`]; }});
}
const inBatch = () => `${B('inspect.cancel', `批量取消 (${[...IN.sel].filter(id => (INSPECT_REPORTS.find(r => r.id == id) || {}).status === 'running').length})`)}${B('inspect.delete', `批量删除 (${IN.sel.size})`, {cls: 'danger'})}`;
const inTabs = cur => tabs('inspect', [['list', '巡检报告'], ['plans', `定时巡检 <span class="tag">${PLANS.length} 个计划</span>`]], cur);
pages.inspect = {
  title: sub => (sub || '').startsWith('detail/') ? '巡检报告 #' + sub.slice(7) : '性能巡检',
  render(sub) {
    sub = sub || 'list';
    if (sub.startsWith('detail/')) return inDetail(sub.slice(7));
    return inTabs(sub === 'plans' ? 'plans' : 'list') + (sub === 'plans' ? inPlans() : inList());
  },
  init() { if (INSPECT_REPORTS.some(r => r.status === 'running')) ensureTimer(); }
};
function inList() {
  const vis = visibleInstances(true);
  return `<div class="card"><h2>巡检报告<span class="right">${B('inspect.run', '▶ 立即巡检', {cls: 'primary'})}<span id="inBatch" class="row">${inBatch()}</span></span></h2>
    <div class="toolbar"><select data-onchange="inF" data-k="inst"><option value="">全部实例</option>${opts(vis.map(i => [i.id, `${i.type === 'MsSQL' ? 'SQL Server' : i.type}：${i.name}`]), IN.inst)}</select>
      <select data-onchange="inF" data-k="level"><option value="">全部级别</option>${opts([['bad', '有告警'], ['warn', '仅关注'], ['ok', '全部正常'], ['failed', '巡检失败'], ['cancelled', '已取消'], ['running', '进行中']], IN.level)}</select>
      <select data-onchange="inF" data-k="trig"><option value="">全部触发</option>${opts([['plan', '定时'], ['manual', '手动']], IN.trig)}</select></div>
    <div id="inList">${inListTable()}</div>${pager(inFiltered().length)}</div>`;
}
ON.inF = (d, el) => { IN[d.k] = el.value; rerender(); };
ON.inCk = (d, el) => { el.checked ? IN.sel.add(el.value) : IN.sel.delete(el.value); part('inBatch', inBatch()); };
ON.inAll = (d, el) => { $$('.inCk').forEach(c => { c.checked = el.checked; el.checked ? IN.sel.add(c.value) : IN.sel.delete(c.value); }); part('inBatch', inBatch()); };

/* ---- 立即巡检 / 取消 / 删除 ---- */
ACTIONS['inspect.run'] = d => {
  if (d.inst) {                                                     // 失败 / 已取消的报告上点“重新巡检”
    const i = instById(d.inst), rep = startInspect(i);
    if (!rep) return toast(`${i.name} 已有进行中的巡检，已跳过`);
    audit('inspect.run', `重新巡检 ${i.name}（报告 #${rep.id}）`, '成功'); toast('已提交巡检，后台执行中'); return go('inspect', 'list');
  }
  const vis = visibleInstances(true), busy = new Set(INSPECT_REPORTS.filter(r => r.status === 'running').map(r => r.inst));
  const grp = (t, l) => l.length ? `<div class="b small" style="margin:8px 0 4px">${t}</div>${l.map(i => `<label class="chk" style="display:flex;padding:2px 0"><input type="checkbox" class="irCk" value="${i.id}" ${busy.has(i.id) ? 'disabled' : ''} data-onchange="irCnt">${esc(i.name)} <span class="small muted">${esc(i.type === 'MsSQL' ? i.ver : i.type + ' ' + i.ver)}${i.status === 'bad' ? ' · 当前连接失败' : ''}${busy.has(i.id) ? ' · 巡检进行中' : ''}</span></label>`).join('')}` : '';
  openModal({title: '立即巡检', code: 'inspect.run', ok: '开始巡检', body: `<div class="row" style="justify-content:space-between"><b>选择实例（可多选）</b><span class="small"><a data-on="irAll" data-v="1">全选</a> · <a data-on="irAll" data-v="0">清空</a></span></div>
    <div style="max-height:320px;overflow:auto;border:1px solid var(--border);border-radius:8px;padding:4px 10px;margin-top:6px">${grp('SQL Server', vis.filter(i => i.type === 'MsSQL'))}${grp('MySQL / 兼容类型', vis.filter(i => i.type !== 'MsSQL' && i.type !== 'Redis'))}${grp('Redis', vis.filter(i => i.type === 'Redis'))}</div>
    ${vis.some(i => i.type === 'Redis') ? '<label class="chk small" style="display:flex;gap:6px;align-items:flex-start;margin-top:8px"><input type="checkbox" id="irBig" style="margin-top:3px"><span>Redis 大 key 抽样（对线上有轻微压力，建议低峰期使用）</span></label>' : ''}
    <div id="irCnt" class="small" style="margin-top:8px">已选 0 个</div>`, onOk() {
    const ids = checkedVals('irCk').map(Number); if (!ids.length) { toast('请至少选择一个实例'); return false; }
    const bigkey = !!($('#irBig') && $('#irBig').checked), made = [], skip = []; ids.forEach(id => { const i = instById(id), rep = startInspect(i, i.type === 'Redis' ? {bigkey} : undefined); rep ? made.push(rep) : skip.push(i.name); });
    audit('inspect.run', `立即巡检 ${made.map(r => instById(r.inst).name).join('、')}（共 ${made.length} 个${skip.length ? '，跳过 ' + skip.length + ' 个' : ''}）`, '成功'); toast(`已提交 ${made.length} 个实例的巡检${skip.length ? '，跳过 ' + skip.length + ' 个' : ''}`);
    later(() => go('inspect', 'list'));
  }});
};
ON.irAll = d => { $$('.irCk').forEach(c => { if (!c.disabled) c.checked = d.v === '1'; }); ON.irCnt(); };
ON.irCnt = () => part('irCnt', `已选 ${checkedVals('irCk').length} 个`);
ACTIONS['inspect.cancel'] = d => {
  const ids = d.id ? [d.id] : [...IN.sel], runs = ids.map(id => INSPECT_REPORTS.find(r => r.id == id)).filter(r => r && r.status === 'running');
  if (!runs.length) return toast('所选报告里没有进行中的巡检');
  confirmBox('inspect.cancel', '取消巡检', `确定取消 ${runs.length} 个进行中的巡检吗？已采集的检查项会保留。<div class="small muted" style="margin-top:6px">${runs.map(r => '#' + r.id + ' ' + esc(instOf(r.inst).name)).join('、')}</div>`, () => {
    runs.forEach(r => { r.status = 'cancelled'; r.items = r.kind === 'redis' ? [] : r.items.slice(0, r.done); tally(r); IN.sel.delete(String(r.id)); });
    audit('inspect.cancel', `取消巡检 ${runs.map(r => '#' + r.id).join('、')}`, '成功'); toast('已取消'); later(rerender); }, '确定取消');
};
ACTIONS['inspect.delete'] = d => {
  const ids = d.id ? [d.id] : [...IN.sel], all = ids.map(id => INSPECT_REPORTS.find(r => r.id == id)).filter(Boolean), del = all.filter(r => r.status !== 'running');
  if (!all.length) return toast('请先勾选要删除的报告');
  if (!del.length) { audit('inspect.delete', `删除报告 ${all.map(r => '#' + r.id).join('、')} 被拒绝：均在进行中`, '拒绝'); return toast('所选报告都在进行中，不能删除（请先取消）'); }
  risky('inspect.delete', `删除 ${del.length} 份巡检报告`, `将删除报告 <b>${del.map(r => '#' + r.id).join('、')}</b>，含检查项明细，不可恢复。${all.length > del.length ? `<br>另有 ${all.length - del.length} 份进行中的已自动排除。` : ''}`, `DELETE FROM dbt_sql_mysql_inspection_report / dbt_sql_mssql_inspection_report / dbt_inspect_redis_report WHERE id IN (${del.map(r => r.id).join(', ')});`, () => {
    del.forEach(r => { INSPECT_REPORTS.splice(INSPECT_REPORTS.indexOf(r), 1); IN.sel.delete(String(r.id)); });
    audit('inspect.delete', `删除巡检报告 ${del.map(r => '#' + r.id).join('、')}`, '成功'); toast('已删除'); if (ROUTE.sub.startsWith('detail/')) go('inspect', 'list'); else rerender(); });
};

/* ---- 详情 ---- */
function inDetailBody(rep) {
  const inst = instById(rep.inst) || {name: '#' + rep.inst, ver: '', host: '', port: ''}, my = rep.kind === 'mysql', r = rng(rep.id * 7 + 1);
  if (rep.kind === 'redis') return window.RedisInspect.detailBody(rep, {inst});
  if (rep.status === 'running') return `<div class="card"><h2>巡检进行中</h2><div class="row" style="gap:16px"><div style="flex:1">${meter(Math.round(rep.done / rep.total * 100))}</div><b>${rep.done} / ${rep.total} 项</b><span class="muted">耗时 ${rep.dur}</span></div>
    <div class="small muted" style="margin-top:8px">已采集：${rep.items.slice(Math.max(0, rep.done - 3), rep.done).map(x => esc(x.name)).join(' → ') || '准备中…'}</div></div>`;
  if (rep.status === 'failed') return notice(`巡检失败：<b>${esc(rep.err)}</b>。目标实例不可达，报告里没有检查项。`, 'bad', '✗');
  const bad = rep.items.filter(x => x.st === 'bad'), warn = rep.items.filter(x => x.st === 'warn'), na = rep.items.filter(x => x.st === 'na'), sug = [...new Set(bad.concat(warn).map(x => x.adv).filter(Boolean))].slice(0, 3);
  const db = dbsOf(inst.id), nTbl = db.reduce((a, d) => a + d.tables.length, 0);
  const sum = my ? [['版本', esc((inst.type) + ' ' + inst.ver)], ['运行时长', `${R(r, 3, 160)} 天 ${R(r, 0, 23)} 小时`], ['数据库 / 表', `${db.length} 个库 · ${nTbl} 张表（样例）`], ['数据总大小', fmtMB(db.reduce((a, d) => a + d.tables.reduce((s, t) => s + (t.size || 0), 0), 0))], ['角色', inst.role === 'master' ? '主库' : '从库']]
    : [['版本', 'SQL Server 2019 · CU18 · Enterprise'], ['启动时间', dayOffset(-R(r, 8, 60)) + ' 02:10'], ['对比窗口', '与上一份报告的差值（实例未重启）'], ['CPU / 内存', '16 核 · 131072 MB · max server memory 110000 MB'], ['MAXDOP / 阈值', '0 / 5'], ['用户库', db.map(d => esc(d.db)).join('、') || '—']];
  const cats = [...new Set(rep.items.map(x => x.cat))];
  const hist = my ? INSPECT_REPORTS.filter(x => x.inst === rep.inst && x.status === 'finish').sort((a, b) => a.at < b.at ? -1 : 1) : [];
  return `<div class="grid g-main"><div class="card"><div class="score">${my ? ring(rep.score) : ''}<div style="flex:1"><div class="stat"><div><b style="color:var(--bad)">${rep.bad}</b><span>告警</span></div><div><b style="color:var(--warn)">${rep.warn}</b><span>关注</span></div><div><b style="color:var(--ok)">${rep.ok}</b><span>正常</span></div><div><b>${rep.na}</b><span>未采集</span></div></div>
      ${rep.status === 'cancelled' ? `<div class="small muted" style="margin-top:8px"><b>已取消，仅完成 ${rep.done} / ${rep.total} 项。</b></div>` : ''}</div></div>
      ${hist.length > 1 ? `<div style="margin-top:10px"><div class="small muted">该实例健康分趋势</div>${lineChart([{name: '健康分', color: 'var(--c2)', data: hist.map(x => x.score)}], {h: 120, xl: [hist[0].at.slice(5, 10), hist[Math.floor(hist.length / 2)].at.slice(5, 10), hist[hist.length - 1].at.slice(5, 10)]})}</div>` : ''}</div>
    <div class="card"><h2>报告概况</h2>${kvList(sum)}</div></div>
    ${bad.length ? notice(`<b>需处理的问题</b><ol style="margin:4px 0 0;padding-left:20px">${bad.map(x => `<li>[${esc(x.cat)}] ${esc(x.name)}：${esc(x.val)}</li>`).join('')}</ol>`, 'bad', '✗') : ''}
    ${warn.length ? notice(`<b>需要关注</b><ol style="margin:4px 0 0;padding-left:20px">${warn.map(x => `<li>[${esc(x.cat)}] ${esc(x.name)}：${esc(x.val)}</li>`).join('')}</ol>`, 'warn', '!') : ''}
    ${na.length ? notice(`<b>未采集的检查</b><ol style="margin:4px 0 0;padding-left:20px">${na.map(x => `<li>${esc(x.name)}：${esc(x.val)}</li>`).join('')}</ol>`, '', 'ℹ') : ''}
    ${sug.length ? notice(`<b>优化建议</b><ol style="margin:4px 0 0;padding-left:20px">${sug.map(x => `<li>${esc(x)}</li>`).join('')}</ol>`, 'ok', '✓') : ''}
    ${cats.map(c => { const l = rep.items.filter(x => x.cat === c), b = l.filter(x => x.st === 'bad').length, w = l.filter(x => x.st === 'warn').length;
      return `<details class="card" style="margin-top:12px" ${b || w ? 'open' : ''}><summary style="cursor:pointer;font-weight:600">${esc(c)} ${b ? `<span class="tag bad">告警 ${b}</span>` : ''}${w ? `<span class="tag warn">关注 ${w}</span>` : ''}${!b && !w ? '<span class="tag ok">正常</span>' : ''} <span class="small muted" style="font-weight:400">${l.length} 项</span></summary><div style="margin-top:10px">${
        tbl(['检查项'].concat(my ? [] : ['范围'], ['当前值', '阈值', '结果', '建议']), l, {attrs: x => x.st === 'bad' ? ' style="background:var(--bad-soft)"' : x.st === 'warn' ? ' style="background:var(--warn-soft)"' : '',
          cells: x => [`<b>${esc(x.name)}</b>`].concat(my ? [] : [esc(x.scope)], [`<span class="mono small">${esc(x.val)}</span>`, `<span class="small muted">${esc(x.th)}</span>`, x.st === 'na' ? tag('未采集') : tagOf(x.st), `<span class="small">${esc(x.adv || '—')}</span>`])})}</div></details>`; }).join('')}`;
}
function inDetail(id) {
  const rep = INSPECT_REPORTS.find(x => x.id == id);
  if (!rep || !instOk(rep.inst)) return `<div class="toolbar"><a class="btn" href="#inspect/list">← 返回列表</a></div>${empty('报告不存在、已被删除，或不在你可见的实例范围内')}`;
  const inst = instById(rep.inst), st = {running: tag('进行中', 'info'), finish: tag('已完成', 'ok'), failed: tag('失败', 'bad'), cancelled: tag('已取消')}[rep.status];
  return `<div class="toolbar"><a class="btn" href="#inspect/list">← 返回列表</a><b style="font-size:16px">巡检报告 #${rep.id} · ${esc(inst ? inst.name : '#' + rep.inst)}</b>${st}${rep.kind === 'mssql' ? tag('SQL Server', 'info') : rep.kind === 'redis' ? tag('Redis') : ''}<span class="sp"></span>
    ${B('inspect.run', '重新巡检', {data: {inst: rep.inst}})}${B('inspect.delete', '删除报告', {cls: 'danger', data: {id: rep.id}})}</div>
    <div class="small muted" style="margin-bottom:12px">开始 ${esc(rep.at)} · 耗时 ${rep.dur} · ${esc(trigText(rep))} · ${rep.kind === 'redis' ? rep.total + ' 步采集' + (rep.items.length ? ' · ' + rep.items.length + ' 项检查' : '') : rep.total + ' 项检查'}</div><div id="inDetail">${inDetailBody(rep)}</div>`;
}

/* ---- 定时巡检：cron 解析 / 下次运行 ---- */
const WD = ['日', '一', '二', '三', '四', '五', '六'];
const fmtD = d => `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
function cronParse(expr) {                                         // 标准 5 段；不接受 @every / TZ= 等描述符
  const f = String(expr || '').trim().split(/\s+/), RG = [[0, 59], [0, 23], [1, 31], [1, 12], [0, 7]];
  if (f.length !== 5) return {err: 'cron 必须是 5 段：分 时 日 月 周'};
  const sets = [];
  for (let k = 0; k < 5; k++) {
    const set = new Set();
    for (const pt of f[k].split(',')) {
      const m = /^(\*|\d+(?:-\d+)?)(?:\/(\d+))?$/.exec(pt); if (!m) return {err: `第 ${k + 1} 段「${f[k]}」格式不对（仅支持 * , - /）`};
      let a, b; if (m[1] === '*') [a, b] = RG[k]; else if (m[1].includes('-')) [a, b] = m[1].split('-').map(Number); else { a = +m[1]; b = m[2] ? RG[k][1] : a; }
      const step = m[2] ? +m[2] : 1; if (a < RG[k][0] || b > RG[k][1] || a > b || step < 1) return {err: `第 ${k + 1} 段「${f[k]}」超出范围 ${RG[k][0]}–${RG[k][1]}`};
      for (let x = a; x <= b; x += step) set.add(k === 4 && x === 7 ? 0 : x);
    }
    sets.push(set);
  }
  return {sets, domStar: f[2][0] === '*', dowStar: f[4][0] === '*'};
}
function cronNext(c, from, n) {
  const out = [], d = new Date(from.replace(' ', 'T') + ':00'), [mi, h, dom, mon, dow] = c.sets; d.setSeconds(0, 0); d.setMinutes(d.getMinutes() + 1);
  for (let g = 0; g < 600000 && out.length < n; g++) {
    const dm = dom.has(d.getDate()), dw = dow.has(d.getDay()), dayOk = c.domStar && c.dowStar ? true : c.domStar ? dw : c.dowStar ? dm : (dm || dw);
    if (!mon.has(d.getMonth() + 1) || !dayOk) { d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + 1); continue; }
    if (!h.has(d.getHours())) { d.setMinutes(0, 0, 0); d.setHours(d.getHours() + 1); continue; }
    if (mi.has(d.getMinutes())) out.push(new Date(d));
    d.setMinutes(d.getMinutes() + 1);
  }
  return out;
}
const planCron = p => { const [h, m] = (p.time || '0:0').split(':').map(Number); return p.kind === 'daily' ? `${m} ${h} * * *` : p.kind === 'weekly' ? `${m} ${h} * * ${p.wdays.slice().sort().join(',')}` : p.kind === 'monthly' ? `${m} ${h} ${p.mday} * *` : p.cron; };
const periodText = p => p.kind === 'daily' ? `每天 ${p.time}` : p.kind === 'weekly' ? `每周${p.wdays.slice().sort().map(d => WD[d]).join('、')} ${p.time}` : p.kind === 'monthly' ? `每月 ${p.mday} 日 ${p.time}` : `cron <code>${esc(p.cron)}</code>`;
const rel = s => (s.slice(0, 10) === TODAY ? '今天' : s.slice(0, 10) === dayOffset(1) ? '明天' : s.slice(5, 10)) + ' ' + s.slice(11, 16);
function nextRuns(p, n) { const c = cronParse(planCron(p)); return c.err ? [] : cronNext(c, NOW0, n).map(fmtD); }
function gapErr(expr) {                                              // 两次运行间隔不得小于 5 分钟
  const c = cronParse(expr); if (c.err) return c.err;
  const t = cronNext(c, NOW0, 24); if (!t.length) return '该表达式在未来一年内不会触发';
  const gap = Math.min(...t.slice(1).map((x, i) => (x - t[i]) / 60000)); return gap < 5 ? `两次运行间隔 ${gap} 分钟，不得小于 5 分钟` : '';
}
const NOTIFY_TEXT = {alert: '仅告警 / 失败时', always: '每次汇总', none: '不通知'};
const notifyChans = () => (window.NOTIFY_CHANNELS && window.NOTIFY_CHANNELS.length ? window.NOTIFY_CHANNELS.filter(c => c.enabled !== false).map(c => c.name) : ['运维值班群', 'DBA 钉钉群']);   // 运行时读取（p_sys2 导出）

/* ---- 计划数据 ---- */
const PLANS = [
  {id: 1, name: '生产 MySQL 夜间巡检', mode: 'insts', insts: [1, 2, 3, 4, 5, 7], tags: [], kind: 'daily', time: '02:00', wdays: [], mday: 1, cron: '', conc: 3, gap: 30, skipBusy: true, retry: true, timeout: 15, notify: 'alert', chans: ['运维值班群'], keep: 90, enabled: true, creator: 'dba_li', last: {at: TODAY + ' 02:00', ok: 6, fail: 0, skip: 0}},
  {id: 2, name: 'SQL Server 每周巡检', mode: 'insts', insts: [6], tags: [], kind: 'weekly', time: '03:00', wdays: [1], mday: 1, cron: '', conc: 1, gap: 300, skipBusy: true, retry: true, timeout: 20, notify: 'always', chans: ['DBA 钉钉群'], keep: 180, enabled: true, creator: 'dba_li', last: {at: dayOffset(-2) + ' 03:00', ok: 1, fail: 0, skip: 0}},
  {id: 3, name: '大促前每小时巡检', mode: 'insts', insts: [1, 3], tags: [], kind: 'cron', time: '', wdays: [], mday: 1, cron: '0 * * * *', conc: 2, gap: 0, skipBusy: true, retry: false, timeout: 10, notify: 'alert', chans: ['运维值班群'], keep: 7, enabled: false, creator: 'admin', last: null},
  {id: 4, name: '核心库标签巡检', mode: 'tag', insts: [], tags: ['核心'], kind: 'daily', time: '05:30', wdays: [], mday: 1, cron: '', conc: 2, gap: 60, skipBusy: true, retry: true, timeout: 15, notify: 'alert', chans: ['DBA 钉钉群'], keep: 60, enabled: true, creator: 'dba_li', last: {at: TODAY + ' 05:30', ok: 2, fail: 0, skip: 0}},
  {id: 101, legacy: 'mssql', name: '原系统配置（兼容）· SQL Server', mode: 'tag', insts: [], tags: ['mssql_inspection'], kind: 'daily', time: '08:00', wdays: [], mday: 1, cron: '', conc: 1, gap: 0, skipBusy: true, retry: false, timeout: 15, notify: 'none', chans: [], keep: 90, enabled: true, creator: '系统', last: {at: TODAY + ' 08:00', ok: 1, fail: 0, skip: 0}},
  {id: 102, legacy: 'mysql', name: '原系统配置（兼容）· MySQL 系', mode: 'tag', insts: [], tags: ['mysql_inspection'], kind: 'daily', time: '03:30', wdays: [], mday: 1, cron: '', conc: 1, gap: 0, skipBusy: true, retry: false, timeout: 15, notify: 'none', chans: [], keep: 90, enabled: true, creator: '系统', last: {at: TODAY + ' 03:30', ok: 3, fail: 0, skip: 0}},
  {id: 5, name: 'Redis 每日巡检', mode: 'insts', insts: [11, 12, 13], tags: [], kind: 'daily', time: '03:10', wdays: [], mday: 1, cron: '', conc: 2, gap: 30, skipBusy: true, retry: true, timeout: 10, notify: 'alert', chans: ['运维值班群'], keep: 30, enabled: true, creator: 'dba_li', last: {at: TODAY + ' 03:10', ok: 3, fail: 0, skip: 0}}];
let PL_ID = 5;
const PRUNS = {
  1: [{at: TODAY + ' 02:00', total: 6, ok: 6, fail: 0, skip: 0, bad: 3, warn: 14, sec: 215, note: '', n: 6}, {at: dayOffset(-1) + ' 02:00', total: 6, ok: 5, fail: 0, skip: 1, bad: 2, warn: 11, sec: 198, note: 'mysql-order-prod-s1 已有进行中的手动巡检，已跳过', n: 5},
    {at: dayOffset(-2) + ' 02:00', total: 6, ok: 4, fail: 2, skip: 0, bad: 1, warn: 9, sec: 305, note: 'mysql-dw-prod-s、mysql-order-test 连接超时（重试后仍失败）', n: 6}],
  2: [{at: dayOffset(-2) + ' 03:00', total: 1, ok: 1, fail: 0, skip: 0, bad: 2, warn: 5, sec: 198, note: '', n: 1}, {at: dayOffset(-9) + ' 03:00', total: 1, ok: 1, fail: 0, skip: 0, bad: 1, warn: 6, sec: 211, note: '', n: 1}],
  4: [{at: TODAY + ' 05:30', total: 2, ok: 2, fail: 0, skip: 0, bad: 1, warn: 4, sec: 41, note: '', n: 2}]};
PRUNS[5] = [0, 1, 2].map(d => { const rs = INSPECT_REPORTS.filter(r => r.plan === 'Redis 每日巡检' && r.at.slice(0, 10) === dayOffset(-d));
  return {at: dayOffset(-d) + ' 03:10', total: rs.length, ok: rs.length, fail: 0, skip: 0, bad: rs.reduce((a, r) => a + r.bad, 0), warn: rs.reduce((a, r) => a + r.warn, 0), sec: 20 + rs.length * 7, note: '', n: rs.length}; });
const planInsts = p => p.mode === 'tag' ? INSTANCES.filter(i => i.enabled && !isExtInst(i) && i.tags.some(t => p.tags.includes(t))) : p.insts.map(instById).filter(Boolean);
function inPlans() {
  const rows = PLANS.slice().sort((a, b) => (a.legacy ? 1 : 0) - (b.legacy ? 1 : 0));
  return `<div class="card"><h2>定时巡检计划<span class="right">${B('inspect.plan_edit', '＋ 新建定时计划', {cls: 'primary'})}</span></h2>
    ${tbl(['计划名称', '巡检实例', '执行周期', '错峰 / 并发', '通知', '报告保留', '状态', '上次运行', '下次运行', '操作'], rows, {attrs: p => p.legacy ? ' style="background:var(--warn-soft)"' : '', cells: p => {
      const li = planInsts(p), nx = p.enabled ? nextRuns(p, 1)[0] : '';
      return [`<b>${esc(p.name)}</b>${p.legacy ? ' ' + tag('内置', 'info') : ''}<div class="small muted">${esc(p.creator)}</div>`,
        p.mode === 'tag' ? `按标签 ${p.tags.map(t => `<code>${esc(t)}</code>`).join(' ')}<div class="small muted">当前匹配 ${li.length} 个实例</div>` : `${li.length} 个实例<div class="small muted" title="${esc(li.map(i => i.name).join('、'))}">${esc(li.slice(0, 2).map(i => i.name).join('、'))}${li.length > 2 ? ' 等' : ''}</div>`,
        periodText(p), p.legacy ? '串行' : `并发 ${p.conc} · ${p.gap ? '间隔 ' + p.gap + ' 秒' : '无间隔'}`, p.notify === 'none' ? '<span class="muted">不通知</span>' : `${NOTIFY_TEXT[p.notify]}<div class="small muted">→ ${esc(p.chans.join('、') || '—')}</div>`, p.keep + ' 天',
        p.legacy ? tag('启用', 'ok') : B('inspect.plan_toggle', p.enabled ? '● 启用中' : '○ 已暂停', {cls: 'sm', data: {id: p.id, op: 'toggle'}}),
        p.last ? `<span class="small">${esc(rel(p.last.at))}<br>${tag(p.last.ok + ' 成功', 'ok')}${p.last.fail ? ' ' + tag(p.last.fail + ' 失败', 'bad') : ''}${p.last.skip ? ' ' + tag(p.last.skip + ' 跳过') : ''}</span>` : '<span class="muted">—</span>',
        p.enabled ? `<span class="small">${nx ? esc(rel(nx)) : '—'}</span>` : '<span class="muted small">已暂停</span>',
        p.legacy ? bx('inspect.plan_toggle', '转为正式计划', {cls: 'sm', data: {id: p.id, op: 'migrate'}}, ME.super ? '' : '仅超级用户可用')
          : `<div class="ops">${B('inspect.plan_edit', '编辑', {cls: 'sm', data: {id: p.id}})}<button class="btn sm" data-on="ipRuns" data-id="${p.id}">运行记录</button>${B('inspect.plan_run', '立即运行', {cls: 'sm', data: {id: p.id}})}${B('inspect.plan_toggle', '删除', {cls: 'sm danger', data: {id: p.id, op: 'delete'}})}</div>`]; }})}</div>`;
}
const runsOf = p => PRUNS[p.id] || (PRUNS[p.id] = []);
ON.ipRuns = d => {
  const p = PLANS.find(x => x.id == d.id), rs = runsOf(p);
  openModal({title: `运行记录 · ${esc(p.name)}`, wide: true, ok: '关闭', cancel: '', body: rs.length ? tbl(['计划触发时间', '实例数', '成功', '失败', '跳过', '告警 / 关注合计', '耗时', '说明', '报告'], rs, {attrs: r => r.fail ? ' style="background:var(--bad-soft)"' : r.skip ? ' style="background:var(--warn-soft)"' : '', cells: r => [esc(r.at), r.total, r.ok, r.fail ? `<b class="up">${r.fail}</b>` : 0, r.skip, `<span class="tag bad">${r.bad}</span> / <span class="tag warn">${r.warn}</span>`, durStr(r.sec), `<span class="small">${esc(r.note || '—')}</span>`, `<a href="#inspect/list" data-mclose="1">${r.n} 份报告</a>`]}) : empty('该计划还没有运行记录')});
};
/* 立即运行：为计划里每个实例生成报告（已有进行中的跳过），写运行记录；通知按计划设置模拟 */
function runPlan(p) {
  const insts = planInsts(p).filter(i => i.enabled), made = [], skip = [];
  insts.forEach(i => { const rep = startInspect(i, {trigger: p.legacy ? 'legacy' : 'plan', plan: p.name, by: '系统'}); rep ? made.push(rep) : skip.push(i.name); });
  const fail = made.filter(r => r.willFail).length, bad = made.reduce((a, r) => a + (r.willFail ? 0 : r.items.filter(x => x.st === 'bad').length), 0), warn = made.reduce((a, r) => a + (r.willFail ? 0 : r.items.filter(x => x.st === 'warn').length), 0);
  const send = p.notify === 'always' || (p.notify === 'alert' && (bad > 0 || fail > 0));
  const note = [skip.length ? `${skip.join('、')} 已有进行中的巡检，已跳过` : '', fail ? `${made.filter(r => r.willFail).map(r => instById(r.inst).name).join('、')} 连接超时` : '', p.notify === 'none' ? '' : send ? `已通知 ${p.chans.join('、')}（${p.notify === 'always' ? '汇总' : '有告警 / 失败'}）` : '无告警 / 失败，按“仅告警时发”未发送通知'].filter(Boolean).join('；');
  runsOf(p).unshift({at: clk().slice(0, 16), total: insts.length, ok: made.length - fail, fail, skip: skip.length, bad, warn, sec: Math.max(30, made.length * 25), note, n: made.length});
  p.last = {at: clk().slice(0, 16), ok: made.length - fail, fail, skip: skip.length};
  return {made, skip, note};
}
ACTIONS['inspect.plan_run'] = d => {
  const p = PLANS.find(x => x.id == d.id), li = planInsts(p);
  risky('inspect.plan_run', `立即运行计划「${esc(p.name)}」`, `将<b>立即</b>为 ${li.length} 个实例（${esc(li.slice(0, 4).map(i => i.name).join('、'))}${li.length > 4 ? ' 等' : ''}）各生成一份巡检报告，已有进行中巡检的实例会被跳过。通知方式：${NOTIFY_TEXT[p.notify]}。`, null, () => {
    const r = runPlan(p); audit('inspect.plan_run', `立即运行计划「${p.name}」：${r.made.length} 个实例已提交${r.skip.length ? '，跳过 ' + r.skip.length : ''}`, '成功'); toast(`已提交 ${r.made.length} 个实例的巡检`); go('inspect', 'list'); });
};
ACTIONS['inspect.plan_toggle'] = d => {
  const p = PLANS.find(x => x.id == d.id);
  if (d.op === 'delete') return risky('inspect.plan_toggle', `删除计划「${esc(p.name)}」`, '删除计划及其运行记录；<b>已产生的报告保留</b>。此操作不可恢复。', null, () => { PLANS.splice(PLANS.indexOf(p), 1); delete PRUNS[p.id]; audit('inspect.plan_toggle', `删除定时巡检计划「${p.name}」`, '成功'); toast('计划已删除'); rerender(); }, p.name);
  if (d.op === 'migrate') {
    if (!ME.super) return toast('仅超级用户可以转为正式计划');
    const k = p.legacy === 'mssql' ? 'MSSQL' : 'MYSQL';
    return risky('inspect.plan_toggle', `转为正式计划：${p.name}`, `将创建一个“${periodText(p).replace(/<[^>]+>/g, '')}、按标签 ${p.tags.join(' / ')} 选实例、保留 ${p.keep} 天”的正式计划，并<b>关闭原系统对应的全局定时任务</b>，避免重复巡检。`, `DELETE FROM dbt_q_schedule WHERE name = '${k === 'MSSQL' ? 'MSSQL' : 'MySQL'}性能巡检';`, () => {
      p.legacy = ''; p.name = (p.mode === 'tag' && p.tags[0] === 'mssql_inspection' ? 'SQL Server' : 'MySQL 系') + ' 每日巡检（原系统转入）'; p.id = ++PL_ID; p.creator = ME.login; p.conc = 2; p.gap = 30;
      audit('inspect.plan_toggle', `转为正式计划「${p.name}」，已关闭原系统 ${k} 全局定时`, '成功'); toast('已转为正式计划，原全局定时已关闭'); rerender(); });
  }
  const on = !p.enabled;
  risky('inspect.plan_toggle', `${on ? '启用' : '暂停'}计划「${esc(p.name)}」`, on ? '启用后按周期自动巡检。' : '暂停后不再按周期触发。', null, () => { p.enabled = on; audit('inspect.plan_toggle', `${on ? '启用' : '暂停'}定时巡检计划「${p.name}」`, '成功'); toast(on ? '计划已启用' : '计划已暂停'); rerender(); });
};

/* ---- 新建 / 编辑计划 ---- */
function planForm(p) {
  const e = !!p, d = p || {name: '', mode: 'insts', insts: [], tags: [], kind: 'daily', time: '02:00', wdays: [1], mday: 1, cron: '', conc: 3, gap: 30, skipBusy: true, retry: true, timeout: 15, notify: 'alert', chans: notifyChans().slice(0, 1), keep: 90, enabled: true};
  const vis = visibleInstances(true), tags = allTags().filter(t => INSTANCES.some(i => i.tags.includes(t))), chs = notifyChans();
  const grp = (t, l) => l.length ? `<div class="b small" style="margin:6px 0 2px">${t}</div>${l.map(i => `<label class="chk" style="display:flex;padding:1px 0"><input type="checkbox" class="pfInst" value="${i.id}" data-onchange="pfCnt" ${d.insts.includes(i.id) ? 'checked' : ''}>${esc(i.name)} <span class="small muted">${i.type === 'MsSQL' ? 'SQL Server' : esc(i.type)}</span></label>`).join('')}` : '';
  const show = (k, ks) => `style="${ks.includes(d.kind) ? '' : 'display:none'}"`;
  return `<div class="form">${fld('计划名称', `<input id="pfName" value="${esc(d.name)}" maxlength="40" placeholder="例如 生产 MySQL 夜间巡检">`, {req: 1})}
    ${fld('选择实例', `<label class="chk" style="margin-right:14px"><input type="radio" name="pfMode" value="insts" data-onchange="pfMode" ${d.mode === 'insts' ? 'checked' : ''}>指定实例</label><label class="chk"><input type="radio" name="pfMode" value="tag" data-onchange="pfMode" ${d.mode === 'tag' ? 'checked' : ''}>按标签动态匹配</label>`, {req: 1})}</div>
    <div id="pfBoxI" style="${d.mode === 'insts' ? '' : 'display:none'};margin:4px 0 8px"><div class="row small" style="margin-bottom:4px"><span class="muted">快速选择：</span><a data-on="pfSel" data-v="all">全选</a><a data-on="pfSel" data-v="none">清空</a><a data-on="pfSel" data-v="mysql">全部 MySQL 系</a><a data-on="pfSel" data-v="mssql">全部 SQL Server</a><a data-on="pfSel" data-v="redis">全部 Redis</a><span class="sp"></span><span id="pfCnt" class="muted">已选 ${d.insts.length} 个</span></div>
      <div style="max-height:170px;overflow:auto;border:1px solid var(--border);border-radius:8px;padding:2px 10px">${grp('MySQL / 兼容类型', vis.filter(i => i.type !== 'MsSQL' && i.type !== 'Redis'))}${grp('SQL Server', vis.filter(i => i.type === 'MsSQL'))}${grp('Redis', vis.filter(i => i.type === 'Redis'))}</div></div>
    <div id="pfBoxT" style="${d.mode === 'tag' ? '' : 'display:none'};margin:4px 0 8px"><div class="row">${tags.map(t => `<label class="chk"><input type="checkbox" class="pfTag" value="${esc(t)}" ${d.tags.includes(t) ? 'checked' : ''}>${esc(t)} <span class="small muted">（${INSTANCES.filter(i => i.enabled && i.tags.includes(t)).length}）</span></label>`).join('')}</div></div>
    <div class="form">${fld('周期', `<select id="pfKind" data-onchange="pfNext">${opts([['daily', '每天'], ['weekly', '每周'], ['monthly', '每月'], ['cron', '自定义 cron']], d.kind)}</select>`, {req: 1})}
    <label ${show('time', ['daily', 'weekly', 'monthly'])} id="pfRowTime0">时刻</label><div ${show('time', ['daily', 'weekly', 'monthly'])} id="pfRowTime"><input id="pfTime" type="time" value="${esc(d.time || '02:00')}" data-onchange="pfNext" style="width:140px"></div>
    <label ${show('wd', ['weekly'])} id="pfRowWd0">每周几</label><div ${show('wd', ['weekly'])} id="pfRowWd">${WD.map((w, k) => [k, w]).slice(1).concat([[0, '日']]).map(([k, w]) => `<label class="chk" style="margin-right:8px"><input type="checkbox" class="pfWd" value="${k}" data-onchange="pfNext" ${d.wdays.includes(k) ? 'checked' : ''}>${w}</label>`).join('')}</div>
    <label ${show('md', ['monthly'])} id="pfRowMd0">每月几日（1–28）</label><div ${show('md', ['monthly'])} id="pfRowMd"><select id="pfMd" data-onchange="pfNext" style="width:100px">${Array.from({length: 28}, (_, k) => `<option ${d.mday === k + 1 ? 'selected' : ''}>${k + 1}</option>`).join('')}</select></div>
    <label ${show('cron', ['cron'])} id="pfRowCron0">cron 表达式（分 时 日 月 周）</label><div ${show('cron', ['cron'])} id="pfRowCron"><input id="pfCron" class="mono" value="${esc(d.cron)}" placeholder="0 2 * * *" data-oninput="pfNext"></div>
    ${fld('下次运行', '<div id="pfNext" class="small"></div>')}
    ${fld('并发 / 错峰', `<span class="small muted">同时巡检</span> <input id="pfConc" type="number" min="1" max="10" value="${d.conc}" style="width:70px"> <span class="small muted">个实例，每启动一个间隔</span> <input id="pfGap" type="number" min="0" max="3600" value="${d.gap}" style="width:80px"> <span class="small muted">秒</span>`)}
    ${fld('执行选项', `<label class="chk" style="margin-right:12px"><input type="checkbox" id="pfSkip" ${d.skipBusy ? 'checked' : ''}>已有进行中巡检时跳过</label><label class="chk" style="margin-right:12px"><input type="checkbox" id="pfRetry" ${d.retry ? 'checked' : ''}>失败后 5 分钟重试 1 次</label><span class="small muted">单实例超时</span> <input id="pfTo" type="number" min="1" max="240" value="${d.timeout}" style="width:70px"> <span class="small muted">分钟</span>`)}
    ${fld('通知', `<select id="pfNotify">${opts([['alert', '仅出现告警 / 巡检失败时'], ['always', '每次计划跑完（汇总）'], ['none', '不通知']], d.notify)}</select>`)}
    ${fld('通知渠道', `<div class="row">${chs.map(c => `<label class="chk"><input type="checkbox" class="pfCh" value="${esc(c)}" ${d.chans.includes(c) ? 'checked' : ''}>${esc(c)}</label>`).join('')}</div>`)}
    ${fld('报告保留', `<input id="pfKeep" type="number" min="1" max="3650" value="${d.keep}" style="width:90px"> <span class="small muted">天</span> <label class="chk" style="margin-left:14px"><input type="checkbox" id="pfEn" ${d.enabled ? 'checked' : ''}>启用</label>`)}</div>${errBox('pfErr')}`;
}
function planRead() {
  const o = {name: fv('pfName'), mode: ($$('input[name=pfMode]').find(x => x.checked) || {}).value || 'insts', insts: checkedVals('pfInst').map(Number), tags: checkedVals('pfTag'), kind: fv('pfKind'), time: fv('pfTime'), wdays: checkedVals('pfWd').map(Number),
    mday: +fv('pfMd') || 1, cron: fv('pfCron'), conc: +fv('pfConc'), gap: +fv('pfGap'), skipBusy: $('#pfSkip').checked, retry: $('#pfRetry').checked, timeout: +fv('pfTo'), notify: fv('pfNotify'), chans: checkedVals('pfCh'), keep: +fv('pfKeep'), enabled: $('#pfEn').checked};
  return o;
}
function planErr(o, self) {
  if (!o.name) return '请填写计划名称';
  if (PLANS.some(x => x.name === o.name && x !== self)) return `计划名称「${o.name}」已存在`;
  if (o.mode === 'insts' && !o.insts.length) return '请至少选择一个实例';
  if (o.mode === 'tag' && !o.tags.length) return '请至少选择一个标签';
  if (o.kind !== 'cron' && !/^\d{2}:\d{2}$/.test(o.time)) return '请填写执行时刻';
  if (o.kind === 'weekly' && !o.wdays.length) return '每周计划请至少选择一天';
  const ge = gapErr(planCron(o)); if (ge) return ge;
  if (!(o.conc >= 1 && o.conc <= 10)) return '并发数需在 1–10';
  if (!(o.gap >= 0 && o.gap <= 3600)) return '错峰间隔需在 0–3600 秒';
  if (!(o.timeout >= 1 && o.timeout <= 240)) return '单实例超时需在 1–240 分钟';
  if (!(o.keep >= 1 && o.keep <= 3650)) return '报告保留天数需在 1–3650';
  if (o.notify !== 'none' && !o.chans.length) return '请选择至少一个通知渠道（或把通知设为“不通知”）';
  return '';
}
ACTIONS['inspect.plan_edit'] = d => {
  const p = d.id ? PLANS.find(x => x.id == d.id) : null;
  openModal({title: p ? `编辑计划 · ${esc(p.name)}` : '新建定时巡检计划', wide: true, code: 'inspect.plan_edit', ok: '保存', body: planForm(p), onOk() {
    const o = planRead(), e = planErr(o, p); if (e) return setErr('pfErr', e);
    const keep = {name: o.name, mode: o.mode, insts: o.mode === 'insts' ? o.insts : [], tags: o.mode === 'tag' ? o.tags : [], kind: o.kind, time: o.kind === 'cron' ? '' : o.time, wdays: o.wdays, mday: o.mday, cron: o.cron, conc: o.conc, gap: o.gap, skipBusy: o.skipBusy, retry: o.retry, timeout: o.timeout, notify: o.notify, chans: o.notify === 'none' ? [] : o.chans, keep: o.keep, enabled: o.enabled};
    if (p) Object.assign(p, keep); else PLANS.push(Object.assign({id: ++PL_ID, creator: ME.login, last: null}, keep));
    audit('inspect.plan_edit', `${p ? '修改' : '新建'}定时巡检计划「${o.name}」：${periodText(o).replace(/<[^>]+>/g, '')}，${o.mode === 'tag' ? '按标签 ' + o.tags.join('/') : o.insts.length + ' 个实例'}，保留 ${o.keep} 天`, '成功'); toast('计划已保存'); later(rerender);
  }});
  setTimeout(ON.pfNext, 0);
};
ON.pfMode = d => { const t = ($$('input[name=pfMode]').find(x => x.checked) || {}).value === 'tag'; $('#pfBoxI').style.display = t ? 'none' : ''; $('#pfBoxT').style.display = t ? '' : 'none'; };
ON.pfSel = d => { $$('.pfInst').forEach(c => { const i = instById(c.value); c.checked = d.v === 'all' || (d.v === 'mssql' && i.type === 'MsSQL') || (d.v === 'mysql' && i.type !== 'MsSQL' && i.type !== 'Redis') || (d.v === 'redis' && i.type === 'Redis'); }); ON.pfCnt(); };
ON.pfCnt = () => part('pfCnt', `已选 ${checkedVals('pfInst').length} 个`);
ON.pfNext = () => {
  if (!$('#pfKind')) return;
  const k = $('#pfKind').value, vis = (id, on) => { [id, id + '0'].forEach(x => { const el = $('#' + x); if (el) el.style.display = on ? '' : 'none'; }); };
  vis('pfRowTime', k !== 'cron'); vis('pfRowWd', k === 'weekly'); vis('pfRowMd', k === 'monthly'); vis('pfRowCron', k === 'cron');
  const o = planRead(), ge = o.kind === 'cron' || o.time ? gapErr(planCron(o)) : '';
  const runs = ge ? [] : nextRuns(o, 3);
  part('pfNext', ge ? `<span style="color:var(--bad)">${esc(ge)}</span>` : runs.length ? runs.map(esc).join('、') + ' …' : '<span class="muted">—</span>');
};

})();
