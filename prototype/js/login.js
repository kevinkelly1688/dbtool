/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: login.js
 * @time: 2026/10/28
 */
/* 登录页（账号密码 / LDAP / 单点登录 / 两步验证）· 退出 · 个人菜单 · 修改密码 · 主题（深空 · 明亮 · 护眼） */
const THEMES = [['light', '☀ 明亮'], ['dark', '☾ 深空'], ['eye', '🌿 护眼']];
function curTheme() { return document.documentElement.dataset.theme || 'light'; }
function setTheme(t) {
  document.documentElement.dataset.theme = t;
  try { localStorage.setItem('dbt_theme', t); } catch (e) {}
  paintThemeSw(); applyWatermark();   // 水印颜色“跟随主题”
}
function paintThemeSw() {
  $$('[data-theme-sw]').forEach(el => el.innerHTML = THEMES.map(([id, n]) => `<button data-on="theme" data-t="${id}" class="${curTheme() === id ? 'on' : ''}" title="${n}">${n}</button>`).join(''));
}
ON.theme = d => { setTheme(d.t); toast('已切换为' + THEMES.find(x => x[0] === d.t)[1].slice(2) + '主题'); };

/* ---------- 登录页 ---------- */
const LG = {tab: 'local', fails: {}, failAt: {}, err: '', step: 'pwd', pending: null, how: ''};
const roleText = u => u.super ? '超级用户' : u.groups.map(groupName).join(' / ');
function showLogin() {
  closeModal(); closeDrawer(); $('#umenu').classList.remove('open');
  $('#app').hidden = true; applyWatermark(); const el = $('#login'); el.hidden = false;
  document.title = '登录 · DB Tool';
  let last = ''; try { last = localStorage.getItem('dbt_last_login') || ''; } catch (e) {}
  const sso = SSO_PROVIDERS.filter(p => p.on && p.login === 'btn');
  const ldapOn = SSO_PROVIDERS.some(p => p.on && p.type === 'LDAP');
  const twofa = LG.step === 'otp';
  el.innerHTML = `
  <div class="brand">
    <div class="lg"><i>DB</i>DB Tool</div>
    <h2>数据库变更、查询与运维一体化平台</h2>
    <p>SQL 上线审批、在线查询、慢日志与性能工单、实例管理与巡检；多级审批、按钮级权限、全程可审计。</p>
    <div class="feat">
      <div><b>⎘ SQL 上线</b>goInception 检查 · 多级审批 · 定时执行 · 回滚语句</div>
      <div><b>⌘ 在线查询</b>库表级授权 · 自动 LIMIT · 敏感列脱敏 · 查询审计</div>
      <div><b>⚑ 性能工单</b>慢查询自动建单 · P0–P3 分级 · SLA 跟踪</div>
      <div><b>⛨ 权限与审计</b>权限组 + 按钮级授权 · 两步验证 · 系统审计</div>
    </div>
    <div class="foot">原型 v0.1 · 模拟数据</div>
    <svg width="420" height="420" viewBox="0 0 100 100"><ellipse cx="50" cy="22" rx="34" ry="12" fill="#fff"/><path d="M16 22v56c0 7 15 12 34 12s34-5 34-12V22c0 7-15 12-34 12S16 29 16 22z" fill="#fff"/></svg>
  </div>
  <div class="pane">
    <div class="tools"><div class="theme-sw" data-theme-sw="1"></div></div>
    <form class="box" id="lgForm" autocomplete="off">
      ${twofa ? `
      <h3>两步验证</h3>
      <div class="fld" style="margin-top:18px"><label for="lgO">验证码（6 位）</label><input id="lgO" inputmode="numeric" maxlength="6" placeholder="OTP 验证码" autocomplete="one-time-code" style="letter-spacing:.4em;font-size:18px;text-align:center"></div>
      <div class="err" id="lgErr">${esc(LG.err)}</div>
      <button class="btn primary go" type="submit">验 证</button>
      <div class="row between small" style="margin-top:12px"><a data-on="lgBack">← 返回重新登录</a><a data-on="lgSms">改用短信验证码</a></div>
      <div class="demo">🧪 原型：验证码固定为 <b class="mono">${DEMO_OTP}</b></div>` : `
      <h3>欢迎登录</h3>
      <div class="tabs"><a data-on="lgTab" data-t="local" class="${LG.tab === 'local' ? 'on' : ''}">账号密码</a>${ldapOn ? `<a data-on="lgTab" data-t="LDAP" class="${LG.tab === 'LDAP' ? 'on' : ''}">LDAP 统一认证</a>` : ''}</div>
      <div class="fld"><label for="lgU">${LG.tab === 'LDAP' ? 'LDAP 账号（uid）' : '账号'}</label><input id="lgU" placeholder="请输入账号" value="${esc(last)}" autocomplete="username"></div>
      <div class="fld"><label for="lgP">密码</label><div class="pw"><input id="lgP" type="password" placeholder="请输入密码" autocomplete="current-password"><button type="button" data-on="lgEye" title="显示 / 隐藏密码">👁</button></div></div>
      <div class="err" id="lgErr">${esc(LG.err)}</div>
      <div class="row between small" style="margin-bottom:14px"><label class="chk"><input type="checkbox" id="lgRem" ${last ? 'checked' : ''}> 记住账号</label><a data-on="lgForgot">忘记密码？</a></div>
      <button class="btn primary go" type="submit">登 录</button>
      ${sso.length ? `<div class="or">其他登录方式</div><div class="sso">${sso.map(p => `<button type="button" data-on="lgSso" data-id="${p.id}" title="使用${p.name}登录"><i style="background:${p.color}">${p.ico}</i>${p.name}</button>`).join('')}</div>` : ''}
      <div class="demo">🧪 原型演示账号：
        <select data-on="noop" id="lgDemo"><option value="">— 选择身份 —</option>${USERS.filter(u => u.status !== 'disabled').map(u => `<option value="${u.login}">${u.login} · ${roleText(u)} · ${u.auth}${u.twofa ? ' · 需 2FA' : ''}${u.status === 'locked' ? '（已锁定）' : ''}</option>`).join('')}</select>
        <div style="margin-top:6px">密码 <b class="mono">${DEMO_PWD}</b></div></div>`}
    </form>
  </div>`;
  paintThemeSw();
  $('#lgForm').onsubmit = e => { e.preventDefault(); twofa ? tryOtp() : tryLogin(); };
  const demo = $('#lgDemo');
  if (demo) demo.onchange = e => { const u = USERS.find(x => x.login === e.target.value); if (!u) return; LG.tab = u.auth === 'LDAP' ? 'LDAP' : 'local'; LG.err = ''; showLogin(); $('#lgU').value = u.login; $('#lgP').value = DEMO_PWD; };
  setTimeout(() => (twofa ? $('#lgO') : last ? $('#lgP') : $('#lgU')).focus(), 30);
}
function lgError(msg) { LG.err = msg; const e = $('#lgErr'); if (e) e.textContent = msg; }
function authAudit(user, act, res, extra = '') { AUDIT.unshift({at: now(), user, ip: '10.1.1.2', module: '登录', code: 'auth.login', act, res, extra}); }
/* 登录锁定（同真实 internal/authx/login.go）：失败次数 ≥ 锁定次数 且 最近一次失败在「锁定时间（秒）」之内 → 拒绝；锁已超时则先把失败计数清零。
   所有失败（用户不存在 / 密码错 / 被锁定）只提示同一句“用户名或密码错误”，不泄漏账号是否存在，也不提示还剩几次；只有真实存在的账号才累计失败次数。 */
const lgLocked = login => (LG.fails[login] || 0) >= SETTINGS.loginFailLock && (LG.fails[login] || 0) > 0 && (LG.failAt[login] || 0) + SETTINGS.lockSeconds * 1000 > Date.now();
function tryLogin() {
  const login = $('#lgU').value.trim(), pwd = $('#lgP').value;
  if (!login || !pwd) return lgError('请输入账号和密码');
  const u = USERS.find(x => x.login === login);
  if (u && lgLocked(login)) { authAudit(login, '登录失败（账号已被临时锁定）', '失败'); return lgError('用户名或密码错误'); }
  if (u && LG.fails[login] >= SETTINGS.loginFailLock) LG.fails[login] = 0;   // 锁已超时：清零（同原系统）
  if (!u || pwd !== DEMO_PWD) {
    authAudit(login, '登录失败（账号或密码错误）', '失败');
    if (u) {
      const n = LG.fails[login] = (LG.fails[login] || 0) + 1; LG.failAt[login] = Date.now();
      if (n === SETTINGS.loginFailLock) authAudit(login, '账号锁定', '失败', `连续失败 ${n} 次，锁定 ${SETTINGS.lockSeconds} 秒`);
    }
    return lgError('用户名或密码错误');
  }
  const want = LG.tab === 'LDAP' ? 'LDAP' : '本地';
  if (u.auth !== want) return lgError(`该账号使用「${u.auth === 'LDAP' ? 'LDAP 统一认证' : u.auth === '本地' ? '账号密码' : u.auth + ' 单点登录'}」方式登录，请切换上方页签或使用对应登录方式`);
  if (u.status !== 'active') { const dis = u.status === 'disabled'; authAudit(login, `登录被拒绝（账号已${dis ? '停用' : '锁定'}）`, '拒绝'); return lgError('用户名或密码错误'); }   // 停用 / 锁定也只给通用提示（同真实：不泄漏账号状态），原因见审计
  try { $('#lgRem').checked ? localStorage.setItem('dbt_last_login', login) : localStorage.removeItem('dbt_last_login'); } catch (e) {}
  LG.fails[login] = 0; delete LG.failAt[login];
  const how = LG.tab === 'LDAP' ? 'LDAP' : '账号密码';
  if (u.twofa) { LG.pending = u; LG.how = how; LG.step = 'otp'; LG.err = ''; showLogin(); return; }
  enterApp(u, how);
}
function tryOtp() {
  const c = $('#lgO').value.trim();
  if (c !== DEMO_OTP) { authAudit(LG.pending.login, '两步验证失败', '失败'); return lgError('验证码不正确或已过期'); }
  const u = LG.pending, how = LG.how + ' + TOTP'; LG.step = 'pwd'; LG.pending = null; enterApp(u, how);
}
function enterApp(u, how) {
  ME = u; LG.err = ''; LG.step = 'pwd';
  const prev = u.last; u.last = now();
  authAudit(u.login, `登录成功（${how}）`, '成功');
  try { sessionStorage.setItem('dbt_uid', u.id); } catch (e) {}
  $('#login').hidden = true; $('#app').hidden = false;
  buildNav(); buildTop(); paintThemeSw();
  if (!location.hash || location.hash === '#') location.hash = 'home';
  render();
  if (prev && prev !== '—') toast(`欢迎，${u.name}。上次登录：${prev}`);
}
ON.lgTab = d => { LG.tab = d.t; LG.err = ''; const v = $('#lgU').value; showLogin(); $('#lgU').value = v; };
ON.lgEye = () => { const p = $('#lgP'); p.type = p.type === 'password' ? 'text' : 'password'; };
ON.lgBack = () => { LG.step = 'pwd'; LG.pending = null; LG.err = ''; showLogin(); };
ON.lgSms = () => toast('原型：已向 138****0001 发送短信验证码（固定 ' + DEMO_OTP + '）');
ON.lgForgot = () => openModal({title: '忘记密码', ok: '', cancel: '知道了', body: `<ul class="small" style="padding-left:18px;margin:0;line-height:1.9"><li><b>LDAP / AD 域账号</b>：请到公司统一身份平台找回密码。</li><li><b>本地账号</b>：请联系管理员重置密码。</li><li><b>单点登录账号</b>：无需密码，点上方对应的登录按钮。</li></ul>`});
ON.lgSso = d => {
  const p = SSO_PROVIDERS.find(x => x.id === d.id), u = USERS.find(x => x.login === p.demoUser);
  openModal({title: `使用 ${p.name} 登录`, ok: '', cancel: '取消', body: `<div style="text-align:center;padding:16px 0"><div style="width:56px;height:56px;border-radius:14px;margin:0 auto 12px;background:${p.color};color:#fff;display:grid;place-items:center;font-size:22px;font-weight:700">${p.ico}</div>
    <div id="ssoSt">正在跳转 ${p.name} 授权页…</div><div class="small muted" style="margin-top:6px">${p.proto}</div></div>`});
  const steps = [`${p.name} 已授权，获取用户信息（邮箱 ${u.email}）…`, `按邮箱匹配平台用户：${u.login}（${u.name}）`];
  steps.forEach((s, i) => setTimeout(() => { const el = $('#ssoSt'); if (el) el.textContent = s; }, 700 * (i + 1)));
  setTimeout(() => { if (!$('#ssoSt')) return; closeModal(); if (u.status !== 'active') return lgError('匹配到的账号已被锁定'); enterApp(u, p.name + ' 单点登录'); }, 2200);
};

/* ---------- 个人菜单 / 退出 / 修改密码 ---------- */
ON.umenu = () => {
  const m = $('#umenu');
  m.innerHTML = `<div class="uh"><b>${esc(ME.name)}</b> <span class="small muted mono">${ME.login}</span><div class="small muted">${roleText(ME)} · ${ME.auth}认证</div><div class="small muted">本次登录：${ME.last}</div></div>
    <a data-on="uProfile">👤 个人信息与权限</a>${ME.auth === '本地' ? '<a data-on="uPwd">🔑 修改密码</a>' : ''}<a data-on="u2fa">🛡 两步验证：${ME.twofa ? '已开启' : '未开启'}</a><a href="#help/quickstart">❔ 使用手册</a>
    <div class="sep"></div><div style="padding:6px 10px"><div class="small muted" style="margin-bottom:6px">界面主题</div><div class="theme-sw" data-theme-sw="1"></div></div>
    <div class="sep"></div><a data-on="logout" style="color:var(--bad)">⎋ 退出登录</a>`;
  m.classList.toggle('open'); paintThemeSw();
};
document.addEventListener('click', e => { if (!e.target.closest('.umenu-wrap')) $('#umenu').classList.remove('open'); });
ON.uProfile = () => {
  $('#umenu').classList.remove('open');
  const ps = [...myCodes()];
  const rows = [['账号', ME.login], ['姓名', ME.name], ['部门', ME.dept], ['邮箱', ME.email], ['认证方式', ME.auth], ['权限组', ME.super ? '超级用户' : ME.groups.map(groupName).join('、')], ['资源组', ME.rgs.map(rgName).join('、')], ['可见实例', visibleInstances().length + ' 个'], ['本次登录', ME.last + '（' + ME.ip + '）'], ['页面水印', SETTINGS.watermark ? '已启用' : '未启用']];
  openDrawer('个人信息与权限', `<table>${rows.map(([k, v]) => `<tr><td class="muted" style="width:100px">${k}</td><td>${esc(v)}</td></tr>`).join('')}</table>
    <h4>我拥有的按钮权限（${ME.super ? '全部' : ps.length + ' 项'}）</h4>${PERM_TREE.map(m => m.pages.map(p => { const bs = p.btns.filter(b => can(b[0])); return bs.length ? `<div style="margin:6px 0"><b class="small">${p.name}</b> ${bs.map(b => `<span class="tag ${b[2] === 'd' ? 'bad' : b[2] === 'v' ? 'info' : ''}" style="margin:2px" title="${b[0]}">${b[1]}</span>`).join('')}</div>` : ''; }).join('')).join('')}
    ${ME.extraGrant.length || ME.extraDeny.length ? `<div class="hr"></div><div class="small muted">单独授权：${ME.extraGrant.join('、') || '无'}；单独禁用：${ME.extraDeny.join('、') || '无'}</div>` : ''}`);
};
ON.u2fa = () => { $('#umenu').classList.remove('open'); openModal({title: '两步验证', ok: ME.twofa ? '' : '绑定验证器', cancel: '关闭', body: ME.twofa ? '<div>已绑定验证器（TOTP）</div>' : '<div>用验证器 App 扫描二维码，输入 6 位动态码。</div><div class="empty"><div class="ic">▦</div><div class="small">（二维码占位）</div></div>', onOk() { toast('原型：绑定流程略'); } }); };
/* ---------- 密码策略校验：最小长度 / 复杂度取自「登录与安全设置」，其余规则与真实一致 ----------
   mode = 'admin'：新建用户 / 管理员手输重置密码，对应 internal/sysadmin/password.go（最长 128 个字符；相关信息 = 用户名、邮箱前缀、姓名，密码不能包含）；
   mode = 'self' ：自助改密 / 自助注册，对应 internal/authx/password.go（最长 256 字节；与用户名、姓名、邮箱前缀“太相似”即拒绝）。
   两处真实实现的提示语、常见弱口令表、检查顺序并不完全相同，这里各自照做。
   复杂度开启时要求同时含字母、数字、特殊字符（不要求大小写，空白不算特殊字符）。登录本身不校验复杂度，只在新建 / 改密时校验。 */
const PWD_COMMON_ADMIN = new Set(['password', 'password1', 'password123', 'passw0rd', 'p@ssw0rd', 'p@ssword', '123456789', '1234567890', '12345678910', 'qwertyuiop', 'qwerty123', 'qwerty1234',
  '1q2w3e4r5t', '1qaz2wsx3', '1qaz2wsx', 'abc123456', 'abcd12345', 'abcd1234', 'admin1234', 'admin12345', 'administrator', 'letmein123', 'welcome123', 'iloveyou1', 'iloveyou12', 'changeme', 'changeme1',
  '123123123', '111111111', '000000000', '987654321', 'zaq12wsx', 'asdfghjkl', 'asdf1234', 'root12345', 'archery123', 'dbatool123', 'mysql12345', 'test12345', '12345678a', 'a12345678']);
const PWD_COMMON_SELF = new Set(['password', 'password1', 'password123', 'passw0rd', '123456', '1234567', '12345678', '123456789', '1234567890', '12345678910', 'qwerty', 'qwerty123', 'qwertyuiop', 'abc123',
  'abcd1234', 'abc12345', '111111', '11111111', '000000', '00000000', 'iloveyou', 'admin', 'admin123', 'admin1234', 'administrator', 'root', 'root1234', 'letmein', 'welcome', 'welcome1', 'monkey', 'dragon',
  'master', 'login', 'princess', 'sunshine', 'football', 'baseball', 'superman', 'trustno1', 'changeme', 'p@ssw0rd', 'p@ssword', 'zaq12wsx', '1q2w3e4r', '1qaz2wsx', 'q1w2e3r4', 'asdfghjkl', 'zxcvbnm',
  'archery', 'dbatool', 'mysql123']);
const pwdLen = () => (SETTINGS.pwdMinLen > 0 ? SETTINGS.pwdMinLen : 9);
const pwdHint = () => `至少 ${pwdLen()} 个字符${SETTINGS.pwdComplexity ? '，需含字母、数字和特殊字符' : ''}。`;
function pwdCheck(pw, related, mode) {
  const min = pwdLen(), n = [...pw].length, low = pw.toLowerCase(), self = mode === 'self', bytes = s => new TextEncoder().encode(s).length;
  const digits = [...pw].every(ch => /\p{Nd}/u.test(ch));
  if (!self && !pw.trim()) return '密码不能为空';
  if (n < min) return `密码太短，至少需要 ${min} 个字符`;
  if (self && bytes(pw) > 256) return '密码过长（不超过 256 字节）';
  if (!self && n > 128) return '密码太长，最多 128 个字符';
  if (self) {
    if (PWD_COMMON_SELF.has(low)) return '密码太常见，请换一个更难猜的';
    if (digits) return '密码不能全是数字';
    for (let a of related) { a = String(a || '').trim().toLowerCase(); if ([...a].length < 3) continue; if (low === a || low.includes(a) || (bytes(low) >= 4 && a.includes(low))) return '密码与用户名太相似'; }
  } else {
    if (digits) return '密码不能全是数字';
    if (PWD_COMMON_ADMIN.has(low)) return '密码过于常见，请换一个更难猜的';
    for (let r of related) { r = String(r || '').trim().toLowerCase(); const at = r.indexOf('@'); if (at > 0) r = r.slice(0, at); if ([...r].length >= 3 && low.includes(r)) return '密码不能包含用户名、邮箱或姓名'; }
  }
  if (SETTINGS.pwdComplexity) {
    let l = false, d = false, s = false;
    for (const ch of pw) { if (/\p{L}/u.test(ch)) l = true; else if (/\p{Nd}/u.test(ch)) d = true; else if (!/\s/u.test(ch)) s = true; }
    if (!l || !d || !s) return '密码需要同时包含字母、数字和特殊字符';
  }
  return '';
}
ON.uPwd = () => {
  $('#umenu').classList.remove('open');
  openModal({title: '修改密码', body: `<div class="form"><label class="req">当前密码</label><input type="password" id="pw0" autocomplete="current-password"><label class="req">新密码</label><input type="password" id="pw1" autocomplete="new-password"><div class="hint" id="pwHint">${pwdHint()}</div><label class="req">确认新密码</label><input type="password" id="pw2" autocomplete="new-password"></div>`,
    onOk() {
      const a = $('#pw0').value, b = $('#pw1').value, c = $('#pw2').value;
      if (!a || !b) { toast('请填写当前密码和新密码'); return false; }
      if (b !== c) { toast('两次输入的新密码不一致'); return false; }
      if (a !== DEMO_PWD) { toast('当前密码不正确'); return false; }
      if (b === a) { toast('新密码不能与当前密码相同'); return false; }
      const e = pwdCheck(b, [ME.login, ME.name, (ME.email || '').split('@')[0]], 'self'); if (e) { toast(e); return false; }
      authAudit(ME.login, '修改密码', '成功'); toast('密码已修改（原型不会真正保存）');
    }});
};
ON.logout = () => {
  $('#umenu').classList.remove('open');
  openModal({title: '退出登录', ok: '退出', okCls: 'danger primary', body: `<div>确定退出账号 <b>${ME.login}</b> 吗？</div>`, onOk() {
    authAudit(ME.login, '退出登录', '成功');
    try { sessionStorage.removeItem('dbt_uid'); } catch (e) {}
    setTimeout(showLogin, 0);
  }});
};

/* 启动入口：会话内已登录则直接进入，否则显示登录页 */
function loginInit() {
  let uid = null; try { uid = sessionStorage.getItem('dbt_uid'); } catch (e) {}
  const u = uid && USERS.find(x => x.id == uid && x.status === 'active');
  if (u) { ME = u; $('#login').hidden = true; $('#app').hidden = false; buildNav(); buildTop(); paintThemeSw(); render(); }
  else showLogin();
}
