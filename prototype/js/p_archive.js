/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_archive.js
 * @time: 2026/10/28
 */
/* ============================================================================
   数据归档（菜单 archive，SQL 查询 › 数据归档）—— 定时、分批、先备份后删除的数据归档
   本原型已按真实实现更新，规则以真实实现为准：internal/archive（任务 / 审批 / 状态机 / 调度 / 运行控制）与 internal/archive/engine（参数范围 / 校验 / 试运行检查项 / 失败语义）；
   页面 = 四个页签（归档任务 / 运行记录 / 备份存储 / 安全策略）+ 六步向导（#archive/wizard）。
   全部是内存里的假数据，不连真实后端：操作只改内存，刷新页面还原；“运行”是会走动的模拟（时间被压缩），离开页面时定时器被清理、模拟暂停推进。
   权限码：archive.view / apply / dryrun / pause / run / stop / restore / delete / cfg_view / store_edit(锁定) / policy_edit(锁定)。
   审批没有单独的按钮码（审批流类型“数据归档申请”不带审核码）：审批人 = 资源组审批流里该级权限组中、属于该资源组的用户；不能审批自己提的（超级用户也不能）。
   通知：没有归档专用订阅项，归档事件复用现有事件名（开始执行 / 执行完成 / 执行有异常 / 待处理 / 审批通过 / 被驳回），见 p_sys2.js 通知渠道页。
   导出：window.ARCHIVE_TASKS / archivePendingList(user) / archivePendingFor(user)；设置 NAV.archive.badge。
   ========================================================================== */
(function () {
'use strict';

/* ---------- 模拟时钟：与 Binlog 解析页同口径，“此刻”是 TODAY 11:20，之后随真实时间流逝 ---------- */
const NOW0 = TODAY + ' 11:20:00', T0 = Date.now(), SIM0 = Date.parse(NOW0.replace(' ', 'T'));
const simMs = (real = Date.now()) => SIM0 + (real - T0);
const tstr = ms => { const d = new Date(ms); return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`; };
const nowS = () => tstr(simMs());
const parseT = s => Date.parse(String(s).replace(' ', 'T'));
const DAY = 86400000;
const nf = n => Number(n).toLocaleString('en-US');
const fmtBytes = n => n >= 1099511627776 ? (n / 1099511627776).toFixed(2) + ' TB' : n >= 1073741824 ? (n / 1073741824).toFixed(2) + ' GB' : n >= 1048576 ? (n / 1048576).toFixed(1) + ' MB' : n >= 1024 ? (n / 1024).toFixed(1) + ' KB' : Math.round(n) + ' B';
const fmtDur = ms => { const s = Math.round(ms / 1000); if (s < 60) return s + ' s'; const m = Math.floor(s / 60); if (m < 60) return m + ' min ' + pad2(s % 60) + ' s'; return Math.floor(m / 60) + ' h ' + pad2(m % 60) + ' min'; };
const hs = s => { let h = 2166136261; for (const c of String(s)) { h ^= c.charCodeAt(0); h = Math.imul(h, 16777619); } return h >>> 0; };
const hex = (s, n) => { let o = '', k = 0; while (o.length < n) o += hs(s + ':' + (k++)).toString(16).padStart(8, '0'); return o.slice(0, n); };
const part = (id, html) => { const el = $('#' + id); if (el) el.innerHTML = html; };
const opt = (v, t, sel, dis) => `<option value="${esc(v)}" ${String(v) === String(sel) ? 'selected' : ''} ${dis ? 'disabled' : ''}>${esc(t)}</option>`;
const bq = s => '`' + s + '`';
const clone = o => JSON.parse(JSON.stringify(o));
const uLabel = login => { const u = USERS.find(x => x.login === login); return u ? `${esc(u.login)}（${esc(u.name)}）` : esc(login); };
const isMy = i => i.type === 'MySQL' || i.type === 'MariaDB';
const WF_OPEN = ['审核通过', '定时执行', '排队中', '执行中'];      // 待执行 / 执行中（还在等审批的工单不算）

/* ---------- 范围（以 engine/params.go、validate.go 为准） ----------
   批间休眠 / 复制延迟阈值取 0 在服务端表示“用策略默认值”，所以界面不能显式设 0（休眠最小 1，会删除数据的模式 ≥ 50）；
   复制延迟检查在实例没有从库时自动跳过、有从库时必检，不提供关闭。单次运行行数下限 100（超过策略硬上限只是黄色警告，实际按硬上限截断）。 */
const LIM = {batch: [100, 10000], sleep: [1, 5000], sleepDel: 50, threads: [4, 1000], lag: [1, 3600], disk: [5, 50], maxWait: [1, 60], targetMs: [100, 5000], fails: [1, 10], maxRowsMin: 100, partMB: [16, 4096],
  minWin: 30, cronGap: 60, extra: 500, name: [2, 50], retainMax: 3650, reason: [5, 500], storeKeep: [0, 36500], polRows: [1000, 2000000000], polConc: [1, 10]};
const MODES = {archive: ['归档并删除', 'ok', '备份并校验后删除源数据'], backup: ['仅备份', 'info', '只备份，不删除源数据'],
  cleanup: ['仅清理', 'warn', '不备份，直接删除']};
const ST_T = {pending: ['待审批', 'warn'], rejected: ['已驳回', 'bad'], enabled: ['已启用', 'ok'], paused: ['已暂停', 'warn'], disabled: ['已停用', ''], draft: ['草稿', '']};
/* 运行结果：键是原型内部名，对应真实实现的 running / paused / success / window_end / load_abort / limit_reached / failed / verify_failed / stopped / structure_changed / interrupted / skipped */
const ST_R = {running: ['运行中', 'brand'], paused: ['已暂停', 'warn'], success: ['成功', 'ok'], window: ['窗口结束中断', 'info'], load: ['负载中断', 'warn'], limit: ['已达上限', 'info'], failed: ['失败', 'bad'], verify: ['校验失败', 'bad'],
  killed: ['已终止', ''], schema: ['结构变更暂停', 'warn'], interrupted: ['中断', 'warn'], skipped: ['已跳过', '']};
const RES_KEY = {running: 'running', paused: 'paused', success: 'success', window: 'window_end', load: 'load_abort', limit: 'limit_reached', failed: 'failed', verify: 'verify_failed', killed: 'stopped', schema: 'structure_changed', interrupted: 'interrupted', skipped: 'skipped'};
const PAUSE_T = {manual: '人工暂停', auto: '自动暂停', schema: '结构变更暂停'};
const TRG = {sched: '定时', manual: '手动', restore: '恢复', dry: '试运行'};
const STORE_T = {local: '本地目录', nfs: 'NFS 挂载目录', s3: 'S3 兼容（含 MinIO）', oss: '阿里云 OSS（S3 兼容端点）'};
/* 本原型里的“外键”事实（data.js 没有外键信息）：被子表外键引用的表不能归档（设计 §8 限制） */
const FK_REF = {'user_db.users': ['user_profile（fk_profile_user）'], 'pay_db.payments': ['refunds（fk_refund_pay）']};
const SCHEMA_SPAN = t => /日志|流水|log/i.test((t.c || '') + t.n) ? 540 : 900;   // 假设表里数据覆盖的天数，用来估算命中比例

/* ---------- 安全策略（设计 §7 页签 4；engine.IsProtected / Policy.validate）：受保护库表是一个“库”或“库.表”模式列表，支持 * ?，不区分大小写；内置系统库无论策略怎么配都生效 ---------- */
const BUILTIN_PROTECTED = ['mysql', 'information_schema', 'performance_schema', 'sys'];
const POLICY = {minRetain: 30, batch: 1000, sleep: 200, threads: 32, lag: 5, disk: 15, maxWait: 10, targetMs: 500, fails: 3, maxRows: 5000000, maxConc: 2,
  protectedPatterns: BUILTIN_PROTECTED.slice(), allowNoBackup: true, by: 'admin', at: '2026-09-18 16:20'};
const POL_META = [['minRetain', '最小保留天数', '天'], ['batch', '默认批量', '行'], ['sleep', '默认批间休眠', 'ms'], ['threads', 'Threads_running 阈值', ''], ['lag', '复制延迟阈值', '秒'],
  ['disk', '磁盘剩余阈值', '%'], ['maxWait', '负载最长等待', '分钟'], ['targetMs', '批耗时目标', 'ms'], ['fails', '连续失败暂停', '批'], ['maxRows', '单次运行最大行数', '行'], ['maxConc', '每实例最大并发运行', '个'],
  ['protectedPatterns', '受保护库表（永不归档）', ''], ['allowNoBackup', '允许不备份清理', '']];
const wild = (p, s) => new RegExp('^' + [...String(p)].map(c => c === '*' ? '.*' : c === '?' ? '.' : c.replace(/[.+^${}()|[\]\\]/g, '\\$&')).join('') + '$', 'i').test(String(s));
const protectedHit = (db, tn, pats) => BUILTIN_PROTECTED.concat(pats || POLICY.protectedPatterns).some(p => {
  p = String(p).trim(); if (!p) return false;
  const i = p.indexOf('.');
  return i >= 0 ? !!tn && wild(p.slice(0, i), db) && wild(p.slice(i + 1), tn) : wild(p, db);
});
const withBuiltin = list => { const seen = new Set(), out = []; BUILTIN_PROTECTED.concat(list || []).forEach(x => { x = String(x).trim(); if (x && !seen.has(x.toLowerCase())) { seen.add(x.toLowerCase()); out.push(x); } }); return out; };

/* ---------- 备份存储（StoreView：local / nfs 用 path；s3 / oss 用 endpoint + bucket + AK / SK；状态 = 停用 → off，否则 ok / err / unknown（未测试）） ---------- */
let STORE_ID = 5;
const TB_ = 1099511627776;
const STORES = [
  {id: 1, name: '本地归档盘（归档服务器）', type: 'local', path: '/data/dba-archive', endpoint: '', region: '', bucket: '', prefix: 'dbtool', ak: '', skSet: false, pathStyle: false, keep: 90, enabled: true, st: 'ok', free: 1.8 * TB_,
    lastTest: {at: '2026-10-07 08:30', ok: true, ms: 12, msg: '写入 / 读取 / 删除探测对象均成功（耗时 12 ms）'}},
  {id: 2, name: 'NFS 归档存储 nas01', type: 'nfs', path: '/mnt/nas01/dba-archive', endpoint: '', region: '', bucket: '', prefix: 'archive', ak: '', skSet: false, pathStyle: false, keep: 365, enabled: true, st: 'ok', free: 14.6 * TB_,
    lastTest: {at: '2026-10-07 08:30', ok: true, ms: 38, msg: '写入 / 读取 / 删除探测对象均成功（耗时 38 ms）'}},
  {id: 3, name: 'MinIO 内网归档', type: 's3', path: '', endpoint: 'http://10.255.8.5:9000', region: '', bucket: 'dba-archive', prefix: 'prod', ak: 'minioadmin01', skSet: true, pathStyle: true, keep: 180, enabled: true, st: 'err', free: null,
    lastTest: {at: '2026-10-07 08:30', ok: false, ms: 0, msg: '测试失败：写入探测对象失败：dial tcp 10.255.8.5:9000: i/o timeout'}},
  {id: 4, name: '阿里云 OSS 冷归档', type: 'oss', path: '', endpoint: 'https://oss-cn-shanghai.aliyuncs.com', region: '', bucket: 'corp-dba-archive', prefix: 'dbtool', ak: 'LTAI5tQ8xZ3kA9fd', skSet: true, pathStyle: false, keep: 0, enabled: true, st: 'ok', free: null,
    lastTest: {at: '2026-10-07 08:30', ok: true, ms: 86, msg: '写入 / 读取 / 删除探测对象均成功（耗时 86 ms）'}},
  {id: 5, name: 'MinIO 测试环境', type: 's3', path: '', endpoint: 'http://minio-test.example.com:9000', region: '', bucket: 'dba-archive-test', prefix: '', ak: 'testaccess01', skSet: true, pathStyle: true, keep: 30, enabled: true, st: 'unknown', free: null, lastTest: null},
];
const storeOf = id => STORES.find(s => s.id === +id);
const storeUse = id => TASKS.filter(t => t.bk.file.on && t.bk.file.store === id).length;
const isFsT = t => t === 'local' || t === 'nfs';
const stStatus = s => !s.enabled ? 'off' : (s.st === 'ok' || s.st === 'err' ? s.st : 'unknown');
const STO_ST = {ok: ['正常', 'ok'], err: ['异常', 'bad'], off: ['已停用', ''], unknown: ['未测试', 'warn']};
const maskAk = ak => { const r = [...String(ak || '')]; return !r.length ? '' : r.length <= 6 ? r[0] + '****' : r.slice(0, 4).join('') + '****' + r.slice(-3).join(''); };
const absPathRe = /^(\/|[A-Za-z]:[\\/]|\\\\)/;
/* 测试连接（模拟）：结果由地址决定 —— 含 10.255. 或 timeout → 超时；含 refused / invalid / bad / 403 → 失败；其余成功。凭据不进信息（真实实现会把错误里的 AK / SK 替换成 ***） */
function storeProbe(c) {
  const a = String(isFsT(c.type) ? c.path : c.endpoint || '').trim(), ms = 10 + hs(a) % 90;
  if (storeProbe.assumeOk) return {ok: true, kind: 'ok', ms, msg: `写入 / 读取 / 删除探测对象均成功（耗时 ${ms} ms）`};      // 仅用于生成种子数据里“当时通过”的历史试运行
  if (/10\.255\.|timeout/i.test(a)) return {ok: false, kind: 'timeout', ms: 0, msg: `测试失败：写入探测对象失败：dial tcp ${a.replace(/^\w+:\/\//, '')}: i/o timeout`};
  if (/refused|invalid|bad|403/i.test(a)) return {ok: false, kind: 'fail', ms: 0, msg: '测试失败：' + (isFsT(c.type) ? '写入探测对象失败：permission denied 或目录不存在' : '写入探测对象失败：403 SignatureDoesNotMatch（AK / SK 不匹配或无写权限）')};
  return {ok: true, kind: 'ok', ms, msg: `写入 / 读取 / 删除探测对象均成功（耗时 ${ms} ms）`};
}

/* ---------- 表 / 实例 辅助 ---------- */
const pkCols = t => { const p = (t.idx || []).find(x => x[0] === 'PRIMARY'); return p ? p[1] : t.cols.filter(c => c.key === 'PRI').map(c => c.n); };
const timeCols = t => (t.cols || []).filter(c => /^(datetime|timestamp|date)\b/i.test(c.t));
const fpOf = (instId, db, tn) => { const t = findTable(instId, db, tn); return t && t.cols ? hex(t.cols.map(c => c.n + ':' + c.t).join(',') + '|' + pkCols(t).join(','), 12) : ''; };
const eligibleInsts = () => visibleInstances().filter(i => isMy(i) && i.role === 'master' && !isReadView(i) && i.enabled !== false);
const slavesOf = inst => INSTANCES.filter(i => i.role === 'slave' && isMy(i) && !isReadView(i) && i.enabled !== false && i.schema === inst.schema && i.env === inst.env && i.rgs.some(r => inst.rgs.includes(r)));
const archFlow = rg => (typeof window.approvalFlowOf === 'function' ? window.approvalFlowOf(rg, 'archive') : null);
const rgOptions = inst => (inst.rgs || []).filter(r => (ME.super || can('query.admin_run') || ME.rgs.includes(r)) && (RGROUPS.find(x => x.id === r) || {}).enabled);
const levelUsers = (rg, gid) => USERS.filter(u => !u.super && u.status === 'active' && u.groups.includes(gid) && u.rgs.includes(rg));
const lvName = gid => groupName(gid);

/* ---------- 调度（每天 / 每周 / 每月 / 高级 cron） ---------- */
const WD = ['日', '一', '二', '三', '四', '五', '六'];
const hm = s => { const m = /^(\d{1,2}):(\d{2})$/.exec(String(s || '').trim()); return m && +m[1] < 24 && +m[2] < 60 ? +m[1] * 60 + +m[2] : -1; };
function parseCron(str) {
  const f = String(str || '').trim().split(/\s+/);
  if (f.length !== 5) return {err: 'cron 需要 5 段：分 时 日 月 周'};
  const R5 = [[0, 59], [0, 23], [1, 31], [1, 12], [0, 7]], out = [];
  for (let i = 0; i < 5; i++) {
    const set = new Set(), [lo, hi] = R5[i];
    for (const part of f[i].split(',')) {
      const m = /^(\*|\d+(?:-\d+)?)(?:\/(\d+))?$/.exec(part);
      if (!m) return {err: `第 ${i + 1} 段「${f[i]}」不是有效的 cron 表达式`};
      let a, b; const step = m[2] ? +m[2] : 1;
      if (!step) return {err: '步长不能为 0'};
      if (m[1] === '*') { a = lo; b = hi; } else if (m[1].includes('-')) { [a, b] = m[1].split('-').map(Number); } else { a = +m[1]; b = m[2] ? hi : a; }
      if (a < lo || b > hi || a > b) return {err: `第 ${i + 1} 段「${f[i]}」超出范围 ${lo}–${hi}`};
      for (let v = a; v <= b; v += step) set.add(i === 4 && v === 7 ? 0 : v);
    }
    out.push({set, star: f[i].startsWith('*')});
  }
  return {f: out};
}
/* 从 fromMs 起（不含）的后 n 次触发时间；返回毫秒数组 */
function schedTimes(s, fromMs, n) {
  const out = [];
  if (s.kind === 'cron') {
    const p = parseCron(s.cron); if (p.err) return out;
    const [mi, ho, dm, mo, dw] = p.f; let t = Math.floor(fromMs / 60000) * 60000 + 60000, guard = 0;
    while (out.length < n && guard++ < 60 * 24 * 400) {
      const d = new Date(t);
      const dayOk = dm.star && dw.star ? true : dm.star ? dw.set.has(d.getDay()) : dw.star ? dm.set.has(d.getDate()) : (dm.set.has(d.getDate()) || dw.set.has(d.getDay()));
      if (mo.set.has(d.getMonth() + 1) && dayOk && ho.set.has(d.getHours()) && mi.set.has(d.getMinutes())) out.push(t);
      t += 60000;
    }
    return out;
  }
  const m = hm(s.time); if (m < 0) return out;
  const d0 = new Date(fromMs); d0.setHours(0, 0, 0, 0);
  for (let k = 0; k < 800 && out.length < n; k++) {
    const d = new Date(d0.getTime()); d.setDate(d.getDate() + k);
    const ok = s.kind === 'day' || (s.kind === 'week' && (s.wd || []).includes(d.getDay())) || (s.kind === 'month' && d.getDate() === +s.md);
    if (!ok) continue;
    const t = new Date(d.getFullYear(), d.getMonth(), d.getDate(), Math.floor(m / 60), m % 60, 0).getTime();
    if (t > fromMs) out.push(t);
  }
  return out;
}
const schedText = s => s.kind === 'day' ? `每天 ${s.time}` : s.kind === 'week' ? `每周${(s.wd || []).slice().sort().map(d => WD[d]).join('、')} ${s.time}` : s.kind === 'month' ? `每月 ${s.md} 日 ${s.time}` : `cron ${s.cron}`;
const winDur = w => { const a = hm(w.from), b = hm(w.to); return a < 0 || b < 0 ? -1 : a === b ? 1440 : (b > a ? b - a : b + 1440 - a); };      // 开始 = 结束表示不限制（24 小时）
const inWin = (min, w) => { const a = hm(w.from), b = hm(w.to); if (a === b) return true; return a < b ? min >= a && min < b : min >= a || min < b; };
const winText = w => { const a = hm(w.from), b = hm(w.to); if (a === b && a >= 0) return '不限制'; const d = winDur(w); return `${w.from} – ${w.to}${b <= a ? '（跨零点）' : ''}（${d >= 60 ? (d % 60 ? (d / 60).toFixed(1) : d / 60) + ' 小时' : d + ' 分钟'}）`; };
const minOf = ms => { const d = new Date(ms); return d.getHours() * 60 + d.getMinutes(); };
const fmtAt = ms => tstr(ms).slice(0, 16);

/* ---------- 附加条件（engine/where.go 的移植）：不是黑名单过滤原文，而是词法分析后按小文法重新解析，再用记号重新生成 SQL ----------
   允许：比较（= <> != < <= > >= <=>）、[NOT] IN (…)（≤ 200 项）、[NOT] LIKE、BETWEEN … AND …、IS [NOT] NULL/TRUE/FALSE、AND OR NOT、括号（≤ 20 层）、字符串 / 数字字面量、列名（可反引号，不可带库表名限定）；
   拒绝：分号、注释、反斜杠、函数调用、子查询、SLEEP / BENCHMARK / INTO OUTFILE / UNION / DDL / DML 关键字、十六进制 / 二进制字面量、算术运算、控制字符。 */
const EX_KW = new Set(['and', 'or', 'not', 'in', 'is', 'null', 'like', 'between', 'true', 'false']);
const EX_FORBID = new Set(('select union sleep benchmark outfile dumpfile load_file insert update delete drop alter create truncate replace grant revoke call exec execute handler set rename join from where group having order limit into for lock show use '
  + 'if case when then else end exists all any some interval xor regexp rlike div mod collate binary cast convert load do prepare deallocate kill shutdown with values table database schema procedure function trigger view index describe explain').split(' '));
const isLt = c => !!c && /\p{L}/u.test(c), isDg = c => !!c && /\p{Nd}/u.test(c);
const wStart = c => isLt(c) || c === '_' || c === '$', wPart = c => wStart(c) || isDg(c);
const validIdent = s => { const a = [...String(s == null ? '' : s)]; return a.length >= 1 && a.length <= 64 && a.every(c => isLt(c) || isDg(c) || c === '_' || c === '$' || c === '-'); };
const quoteIdent = s => '`' + String(s).replace(/`/g, '``') + '`';
function lexExtra(s) {
  const rs = [...s], n = rs.length, toks = [], E = m => { throw new Error(m); };
  const prevOperand = () => { const t = toks[toks.length - 1]; if (!t) return false; if (['ident', 'qident', 'string', 'number', 'rparen'].includes(t.k)) return true; return t.k === 'kw' && ['NULL', 'TRUE', 'FALSE'].includes(t.v); };
  for (let i = 0; i < n;) {
    const r = rs[i], nx = rs[i + 1];
    if (r === ' ' || r === '\t' || r === '\n' || r === '\r') { i++; continue; }
    if (r === ';') E('不允许分号（只能是一个条件表达式，不能写多条语句）');
    if (r === '#') E('不允许注释符（-- /* */ #）');
    if (r === '\\') E('不允许反斜杠');
    if ((r === '/' && nx === '*') || (r === '*' && nx === '/') || (r === '-' && nx === '-')) E('不允许注释符（-- /* */ #）');
    if (r === "'" || r === '"') {
      let b = '', j = i + 1, closed = false;
      while (j < n) {
        const c = rs[j];
        if (c === r) { if (rs[j + 1] === r) { b += r; j += 2; continue; } closed = true; j++; break; }
        if (c === '\\') E('不允许反斜杠');
        if (c.codePointAt(0) < 0x20 && c !== '\t') E('字符串里不允许控制字符');
        b += c; j++;
      }
      if (!closed) E('引号不成对');
      toks.push({k: 'string', v: b}); i = j; continue;
    }
    if (r === '`') {
      let j = i + 1;
      while (j < n && rs[j] !== '`') j++;
      if (j >= n) E('反引号不成对');
      const name = rs.slice(i + 1, j).join('');
      if (!validIdent(name)) E(`列名 "${name}" 含不允许的字符`);
      if (rs[j + 1] === '`') E('列名里不允许反引号');
      toks.push({k: 'qident', v: name}); i = j + 1; continue;
    }
    if (isDg(r) || (r === '.' && isDg(nx)) || ((r === '-' || r === '+') && i + 1 < n && (isDg(nx) || nx === '.') && !prevOperand())) {
      let j = i;
      if (rs[j] === '-' || rs[j] === '+') j++;
      while (j < n && isDg(rs[j])) j++;
      if (j < n && rs[j] === '.') { j++; while (j < n && isDg(rs[j])) j++; }
      if (j < n && (rs[j] === 'e' || rs[j] === 'E')) { let k = j + 1; if (k < n && (rs[k] === '+' || rs[k] === '-')) k++; if (k < n && isDg(rs[k])) { while (k < n && isDg(rs[k])) k++; j = k; } }
      if (j < n && wPart(rs[j])) E(`数字后面不能紧跟字母（不支持十六进制 / 二进制字面量）："${rs.slice(i, j + 1).join('')}"`);
      const num = rs.slice(i, j).join('');
      if (num === '-' || num === '+' || num === '.') E('数字格式不对');
      toks.push({k: 'number', v: num.replace(/^\+/, '')}); i = j; continue;
    }
    if (wStart(r)) {
      let j = i;
      while (j < n && wPart(rs[j])) j++;
      const word = rs.slice(i, j).join(''), lw = word.toLowerCase();
      if (rs[j] === '.') E(`不允许带库名 / 表名限定的列名（${word}.…）`);
      if (EX_KW.has(lw)) toks.push({k: 'kw', v: word.toUpperCase()});
      else if (EX_FORBID.has(lw)) E(`不允许出现关键字 ${word.toUpperCase()}（禁止子查询、联表、函数副作用、DML / DDL）`);
      else { if (!validIdent(word)) E(`列名 "${word}" 含不允许的字符`); toks.push({k: 'ident', v: word}); }
      i = j; continue;
    }
    if (r === '(') { toks.push({k: 'lparen', v: '('}); i++; continue; }
    if (r === ')') { toks.push({k: 'rparen', v: ')'}); i++; continue; }
    if (r === ',') { toks.push({k: 'comma', v: ','}); i++; continue; }
    if (r === '=') { toks.push({k: 'op', v: '='}); i++; continue; }
    if (r === '<') { if (nx === '=' && rs[i + 2] === '>') { toks.push({k: 'op', v: '<=>'}); i += 3; } else if (nx === '=') { toks.push({k: 'op', v: '<='}); i += 2; } else if (nx === '>') { toks.push({k: 'op', v: '<>'}); i += 2; } else { toks.push({k: 'op', v: '<'}); i++; } continue; }
    if (r === '>') { if (nx === '=') { toks.push({k: 'op', v: '>='}); i += 2; } else { toks.push({k: 'op', v: '>'}); i++; } continue; }
    if (r === '!') { if (nx === '=') { toks.push({k: 'op', v: '<>'}); i += 2; continue; } E('不允许的字符 !'); }
    if (r.codePointAt(0) < 0x20) E('不允许控制字符');
    E(`不允许的字符 "${r}"（只允许比较运算、IN、LIKE、BETWEEN、IS NULL 与 AND / OR）`);
  }
  return toks;
}
/* 返回 {ok, sql, cols} 或 {ok:false, err}；sql 是可拼进 WHERE 的规范化片段（标识符加反引号、字符串统一单引号、关键字大写），cols 是引用到的列名 */
function normalizeExtra(s0) {
  const s = String(s0 == null ? '' : s0).trim();
  if (!s) return {ok: true, sql: '', cols: []};
  try {
    const len = [...s].length;
    if (len > LIM.extra) throw new Error(`附加条件最长 ${LIM.extra} 字（当前 ${len} 字）`);
    const toks = lexExtra(s);
    if (!toks.length) return {ok: true, sql: '', cols: []};
    const P = {pos: 0, buf: '', cols: [], seen: new Set(), depth: 0}, E = m => { throw new Error(m); };
    const peek = () => toks[P.pos], isKw = kw => { const t = peek(); return !!t && t.k === 'kw' && t.v === kw; };
    const emit = x => { if (P.buf && !P.buf.endsWith('(') && x !== ')') P.buf += ' '; P.buf += x; };
    const operand = () => {
      const t = peek();
      if (!t) E('条件不完整：缺少值或列名');
      if (t.k === 'ident' || t.k === 'qident') {
        P.pos++;
        const n2 = peek();
        if (n2 && n2.k === 'lparen') E(`不允许函数调用 ${t.v}()（只允许比较运算、IN、LIKE、BETWEEN、IS NULL 与 AND / OR）`);
        emit(quoteIdent(t.v));
        const lc = t.v.toLowerCase();
        if (!P.seen.has(lc)) { P.seen.add(lc); P.cols.push(t.v); }
      } else if (t.k === 'string') { P.pos++; emit("'" + t.v.replace(/'/g, "''") + "'"); }
      else if (t.k === 'number') { P.pos++; emit(t.v); }
      else if (t.k === 'kw') { if (['NULL', 'TRUE', 'FALSE'].includes(t.v)) { P.pos++; emit(t.v); } else E('此处不应出现 ' + t.v); }
      else E('条件不完整：此处需要值或列名');
    };
    const predicate = () => {
      operand();
      let t = peek();
      if (!t) E('不是有效的条件表达式（缺少比较运算 / IN / LIKE / IS NULL）');
      if (t.k === 'op') { P.pos++; emit(t.v); operand(); return; }
      if (t.k !== 'kw') E('不是有效的条件表达式（缺少比较运算 / IN / LIKE / IS NULL）');
      let neg = false;
      if (t.v === 'NOT') { neg = true; P.pos++; emit('NOT'); t = peek(); if (!t || t.k !== 'kw') E('NOT 后面应是 IN / LIKE / BETWEEN'); }
      if (t.v === 'IN') {
        P.pos++; emit('IN');
        const lp = peek();
        if (!lp || lp.k !== 'lparen') E('IN 后面应是括号列表');
        P.pos++; emit('(');
        let cnt = 0;
        for (;;) {
          operand(); cnt++;
          if (cnt > 200) E('IN 列表最多 200 项');
          const n2 = peek();
          if (!n2) E('括号不成对');
          P.pos++;
          if (n2.k === 'comma') { P.buf += ','; continue; }
          if (n2.k === 'rparen') { emit(')'); return; }
          E('IN 列表里只能是值，用逗号分隔');
        }
      }
      if (t.v === 'LIKE') { P.pos++; emit('LIKE'); operand(); return; }
      if (t.v === 'BETWEEN') { P.pos++; emit('BETWEEN'); operand(); if (!isKw('AND')) E('BETWEEN 需要 AND'); P.pos++; emit('AND'); operand(); return; }
      if (t.v === 'IS') {
        if (neg) E('不是有效的条件表达式');
        P.pos++; emit('IS');
        if (isKw('NOT')) { P.pos++; emit('NOT'); }
        const n2 = peek();
        if (!n2 || n2.k !== 'kw' || !['NULL', 'TRUE', 'FALSE'].includes(n2.v)) E('IS 后面应是 NULL / TRUE / FALSE（或 NOT NULL）');
        P.pos++; emit(n2.v); return;
      }
      E('不是有效的条件表达式（缺少比较运算 / IN / LIKE / IS NULL）');
    };
    let expr;
    const primary = () => {
      const t = peek();
      if (t && t.k === 'lparen') { P.pos++; emit('('); expr(); const r = peek(); if (!r || r.k !== 'rparen') E('括号不成对'); P.pos++; emit(')'); return; }
      predicate();
    };
    const not = () => { if (isKw('NOT')) { P.pos++; emit('NOT'); return not(); } primary(); };
    const and = () => { not(); while (isKw('AND')) { P.pos++; emit('AND'); not(); } };
    expr = () => {
      if (++P.depth > 20) E('括号嵌套太深');
      and();
      while (isKw('OR')) { P.pos++; emit('OR'); and(); }
      P.depth--;
    };
    expr();
    if (P.pos < toks.length) E(toks[P.pos].k === 'rparen' ? '括号不成对' : `条件表达式在 "${toks[P.pos].v}" 附近有多余内容`);
    return {ok: true, sql: P.buf, cols: P.cols};
  } catch (e) { return {ok: false, err: e.message}; }
}

/* ---------- 任务 → WHERE / 将执行的 SQL（engine.buildWhere / BuildSQLPreview）：附加条件永远加括号 ---------- */
function whereRes(t) {
  if (!validIdent(t.tcol)) return {err: `时间列名无效：${JSON.stringify(t.tcol || '')}`};
  const days = Number(t.retain);
  if (!(days >= 1)) return {err: '保留天数必须是正整数'};
  const x = normalizeExtra(t.extra);
  if (!x.ok) return {err: '附加条件：' + x.err};
  return {sql: `${quoteIdent(t.tcol)} < NOW() - INTERVAL ${Math.floor(days)} DAY` + (x.sql ? ` AND (${x.sql})` : ''), cols: x.cols};
}
const whereOf = t => { const w = whereRes(t); return w.err ? '' : w.sql; };

/* 模板：库表备份的表名模板可用 {table} {yyyy} {mm} {dd} {yyyymm} {yyyymmdd}；文件路径模板可用 {inst} {db} {table} {yyyy} {mm} {dd} {yyyymm} {yyyymmdd} {run_id} {task_id} */
const tplVars = ms => { const d = new Date(ms), y = String(d.getFullYear()).padStart(4, '0'), mo = pad2(d.getMonth() + 1), dd = pad2(d.getDate()); return {yyyy: y, mm: mo, dd, yyyymm: y + mo, yyyymmdd: y + mo + dd}; };
const tplFill = (tpl, vars) => String(tpl).replace(/\{([^{}]*)\}/g, (m, k) => (k in vars ? vars[k] : m));
const sanitizeName = s => [...String(s)].map(c => isLt(c) || isDg(c) || c === '_' || c === '$' ? c : '_').slice(0, 64).join('');
const tableNameOf = (tpl, table, ms) => sanitizeName(tplFill(String(tpl || '').trim() ? tpl : '{table}_archive', Object.assign(tplVars(ms), {table})));
const tplExpand = (tpl, t, _seq, ms) => tableNameOf(tpl, t.tbl, ms || simMs());
const segClean = s => [...String(s)].map(c => isLt(c) || isDg(c) || c === '_' || c === '-' || c === '.' ? c : '_').join('').replace(/\.\./g, '__');
const litClean = s => [...String(s)].map(c => isLt(c) || isDg(c) || '_-./'.includes(c) ? c : c === '\\' ? '/' : '_').join('');
function pathOf(tpl, t, runId, ms) {
  const v = Object.assign(tplVars(ms || simMs()), {inst: segClean(instOf(t.inst).name), db: segClean(t.db), table: segClean(t.tbl), run_id: String(runId || 0), task_id: String(t.id || 0)});
  tpl = String(tpl || '').trim() ? String(tpl) : '{inst}/{db}/{table}/{yyyy}/{mm}/{dd}/{run_id}';
  let out = '', last = 0;
  tpl.replace(/\{[^{}]*\}/g, (m, idx) => { out += litClean(tpl.slice(last, idx)); const k = m.slice(1, -1); out += k in v ? v[k] : litClean(m); last = idx + m.length; return m; });
  out += litClean(tpl.slice(last));
  return out.split('/').filter(s => s && s !== '.').map(s => s === '..' ? '_' : s).join('/');
}
const PH_TABLE = ['table', 'yyyy', 'mm', 'dd', 'yyyymm', 'yyyymmdd'], PH_PATH = ['inst', 'db', 'table', 'yyyy', 'mm', 'dd', 'yyyymm', 'yyyymmdd', 'run_id', 'task_id'];
function phErr(tpl, vars) {
  for (const m of String(tpl).match(/\{[^{}]*\}/g) || []) if (!vars.includes(m.slice(1, -1))) return `模板里有不认识的占位符 ${m}`;
  return /[{}]/.test(String(tpl).replace(/\{[^{}]*\}/g, '')) ? '模板里的花括号不成对' : '';
}
function tableTplErr(tpl) {
  if (!String(tpl || '').trim()) return '目标表名模板不能为空';
  if (!/^[A-Za-z0-9_{}]+$/.test(tpl)) return '目标表名模板只能含字母、数字、下划线与 {table} {yyyy} {mm} {dd} {yyyymm}';
  return phErr(tpl, PH_TABLE) || ([...tplFill(tpl, Object.assign(tplVars(Date.UTC(2026, 0, 1)), {table: ''}))].length > 64 ? '目标表名模板展开后超过 64 位' : '');
}
function pathTplErr(tpl) {
  if (!String(tpl || '').trim()) return '路径模板不能为空';
  if ([...tpl].length > 128) return '路径模板最长 128 位';
  if (!/^[A-Za-z0-9_\-.\/{}]+$/.test(tpl)) return '路径模板只能含字母、数字、_ - . / 与 {inst} {db} {table} {yyyy} {mm} {dd} {run_id}';
  if (tpl.startsWith('/')) return '路径模板不能以 / 开头';
  if (tpl.includes('..')) return '路径模板不能含 ..';
  return phErr(tpl, PH_PATH);
}
const fileExt = f => (f.fmt === 'sql' ? 'sql' : 'csv') + (f.gzip ? '.gz' : '') + (f.enc ? '.enc' : '');
const dbTarget = t => t.bk.db.on ? {inst: t.bk.db.inst || t.inst, db: t.bk.db.db, tn: tplExpand(t.bk.db.tpl, t)} : null;
const filePath = (t, runId) => `${pathOf(t.bk.file.path, t, runId)}/part-0001.${fileExt(t.bk.file)}`;
/* “将执行的 SQL”：和真实实现一样，向导预览 / 任务详情拿不到表结构（主键写 `<主键>`、列写 *），试运行报告里才是真实主键与列；任务有断点时 `主键 > 断点` 带字面量 */
function sqlPreview(t, o) {
  o = o || {};
  const w = whereRes(t), where = w.err ? `/* 条件无效：${w.err} */` : w.sql, pkc = o.pk && o.pk.length ? o.pk : ['<主键>'], pkq = pkc.map(quoteIdent);
  const colList = o.cols && o.cols.length ? o.cols.map(quoteIdent).join(', ') : '*', src = `${quoteIdent(t.db || '?')}.${quoteIdent(t.tbl || '?')}`, ck = o.ck || [];
  let after = '?';
  if (ck.length && ck.length === pkc.length) { const l = ck.map(v => "'" + String(v).replace(/\\/g, '\\\\').replace(/'/g, "\\'") + "'"); after = l.length > 1 ? '(' + l.join(', ') + ')' : l[0]; }
  else if (pkc.length > 1) after = '(' + pkc.map(() => '?').join(', ') + ')';
  const pkExpr = pkq.length > 1 ? '(' + pkq.join(', ') + ')' : pkq[0], out = {select: '', insert: '', delete: ''};
  out.select = `SELECT ${colList} FROM ${src}\n WHERE ${where}\n   AND ${pkExpr} > ${after} -- 断点：上一批最大主键（首批无此条件）\n ORDER BY ${pkq.join(', ')}\n LIMIT ${Number(t.p.batch) || 1000};`;
  if (t.mode !== 'cleanup' && t.bk.db.on) {
    const d = dbTarget(t), vals = '(' + (o.cols && o.cols.length ? o.cols.map(() => '?').join(', ') : '?, ?, …') + ')';
    out.insert = `-- 目标：实例 #${d.inst}\nINSERT INTO ${quoteIdent(d.db || '?')}.${quoteIdent(d.tn)} (${colList})\nVALUES ${vals}, …; -- 本批读到的行；主键已存在且内容一致视为“已存在”，不一致 = 校验失败`;
  }
  if (t.mode !== 'backup') out.delete = `DELETE FROM ${src}\n WHERE ${pkExpr} IN (?, ?, …) -- 本批读到的主键，备份校验通过后才执行\n   AND (${where}); -- 重复一遍归档条件，避免删到期间被改过的行`;
  return out;
}
const sqlBlocks = (q, mode) => `<div class="small muted">① 读</div>${code(q.select, 'sql')}
  ${q.insert ? `<div class="small muted">② 写目标</div>${code(q.insert, 'sql')}` : `<div class="small muted">② 写目标</div>${mode === 'cleanup' ? notice('仅清理模式：<b>不备份</b>，直接删除。', 'warn', '!') : notice('没有库表备份。', '', 'ℹ')}`}
  ${q.delete ? `<div class="small muted">③ 删源</div>${code(q.delete, 'sql')}` : `<div class="small muted">③ 删源</div>${notice('仅备份模式：<b>不删除</b>源表数据。', '', 'ℹ')}`}`;

/* ---------- 表单校验（向导每一步） ---------- */
const TASKS = [], RUNS = [], SIMS = new Map();
let TASK_ID = 0, RUN_ID = 5000;
const taskOf = id => TASKS.find(t => t.id == id), runOf = id => RUNS.find(r => r.id == id);
const activeTaskOn = (t, exceptId) => TASKS.find(x => x.id !== exceptId && x.inst === t.inst && x.db === t.db && x.tbl === t.tbl && ['pending', 'enabled', 'paused'].includes(x.status));
/* 稳定性参数的校验（engine.Validate “稳定性参数”部分）：返回 [{lv, msg}]。取 0 在服务端等于“用策略默认值”，所以休眠 / 复制延迟阈值不能填 0 */
const NUM = v => v === '' || v == null ? NaN : Number(v);
function paramIssues(t) {
  const p = t.p, out = [], er = msg => out.push({lv: 'error', msg});
  const rg = (v, [lo, hi], nm, unit) => { const n = NUM(v); if (!Number.isInteger(n) || n < lo || n > hi) er(`${nm}需要在 ${nf(lo)}–${nf(hi)}${unit || ''} 之间的整数`); };
  rg(p.batch, LIM.batch, '批大小', ' 行');
  if (NUM(p.sleep) === 0) er('批间休眠不能填 0：服务端把 0 当作“用策略默认值”，无法表达 0 ms（最小填 1；归档并删除 / 仅清理 不得低于 50 ms）');
  else rg(p.sleep, LIM.sleep, '批间休眠', ' ms');
  if (t.mode !== 'backup' && NUM(p.sleep) >= 1 && NUM(p.sleep) < LIM.sleepDel) er(`会删除源表数据的模式（归档并删除 / 仅清理）批间休眠不得低于 ${LIM.sleepDel} ms`);
  rg(p.threads, LIM.threads, 'Threads_running 阈值');
  if (NUM(p.lag) === 0) er('复制延迟阈值不能填 0：服务端把 0 当作“用策略默认值”（范围 1–3,600 秒）；有从库时必检、没有从库时自动跳过，不提供关闭');
  else rg(p.lag, LIM.lag, '复制延迟阈值', ' 秒');
  rg(p.disk, LIM.disk, '磁盘剩余阈值', ' %'); rg(p.maxWait, LIM.maxWait, '最长等待', ' 分钟'); rg(p.targetMs, LIM.targetMs, '批耗时目标', ' ms'); rg(p.fails, LIM.fails, '连续失败暂停阈值', ' 批');
  const mr = NUM(p.maxRows);
  if (!Number.isInteger(mr) || mr < LIM.maxRowsMin) er(`单次运行最多处理行数需要是不小于 ${LIM.maxRowsMin} 的整数`);
  else if (mr > POLICY.maxRows) out.push({lv: 'warn', msg: `单次运行行数上限 ${nf(mr)} 超过安全策略的硬上限 ${nf(POLICY.maxRows)}，实际运行将按 ${nf(POLICY.maxRows)} 截断`});
  return out;
}
/* 模式 × 备份去向（engine.Validate 的 mode 部分） */
function modeIssues(t) {
  const out = [], er = m => out.push({lv: 'error', msg: m});
  if (!MODES[t.mode]) er('请选择模式');
  else if (t.mode === 'cleanup') { if (!POLICY.allowNoBackup) er('安全策略当前不允许“不备份清理”（仅超级用户可在「安全策略」里放开）'); }
  else if (!t.bk.db.on && !t.bk.file.on) er('请至少选择一个备份去向（归档库表 / 文件存储）；“不备份”只能用模式“仅清理”');
  return out;
}
const reasonErr = t => { const n = [...String(t.reason || '').trim()].length; return n < LIM.reason[0] ? `仅清理必须填写“不备份理由”（至少 ${LIM.reason[0]} 个字），审批人会看到` : n > LIM.reason[1] ? `不备份理由最多 ${LIM.reason[1]} 字` : ''; };
function destTableIssues(t) {
  const d = t.bk.db, out = [], er = m => out.push({lv: 'error', msg: m}), ti = d.inst ? instOf(d.inst) : null;
  if (!d.inst) er('请选择归档库表所在的实例');
  else if (!isMy(ti) || ti.role !== 'master' || isReadView(ti) || ti.enabled === false) er('归档库表所在实例只能是 MySQL / MariaDB 主库');
  else if (!d.db) er('请选择归档库');
  else if (!validIdent(d.db)) er(`归档库名无效：${JSON.stringify(d.db)}`);
  else if (protectedHit(d.db, '')) er(`归档库 ${d.db} 属于受保护库`);
  const te = tableTplErr(d.tpl);
  if (te) er(te);
  else {
    const raw = [...tplFill(d.tpl, Object.assign(tplVars(simMs()), {table: t.tbl}))].length, same = tableNameOf(d.tpl, t.tbl, simMs()).toLowerCase() === String(t.tbl).toLowerCase();
    if (raw > 64) er(`模板展开后的表名超过 64 位（${raw} 位，源表名 ${t.tbl}）`);
    else if (d.inst === t.inst && d.db === t.db) {
      if (same) er('目标表不能就是源表');
      else if ([1, 6, 12].some(m => tableNameOf(d.tpl, t.tbl, new Date(2026, m - 1, 1).getTime()).toLowerCase() === String(t.tbl).toLowerCase())) er('模板在某些日期会展开成源表本身，请换一个模板');
    }
  }
  return out;
}
function destFileIssues(t) {
  const f = t.bk.file, out = [], er = m => out.push({lv: 'error', msg: m}), s = storeOf(f.store);
  if (!f.store || !s) er('请选择备份存储'); else if (!s.enabled) er(`备份存储「${s.name}」已停用`);
  if (f.fmt !== 'csv' && f.fmt !== 'sql') er('文件格式只能是 csv 或 sql');
  const pe = pathTplErr(f.path);
  if (pe) er(pe); else if (!String(f.path).includes('{run_id}')) out.push({lv: 'warn', msg: '路径模板里没有 {run_id}：同一天多次运行会写到同一目录下（引擎会自动避开已存在的文件名，但不便于按运行查找）'});
  const pm = NUM(f.partMB);
  if (f.partMB !== '' && f.partMB != null && pm !== 0 && (!Number.isInteger(pm) || pm < LIM.partMB[0] || pm > LIM.partMB[1])) er(`单个文件大小上限需要在 ${LIM.partMB[0]}–${nf(LIM.partMB[1])} MB 之间（0 表示默认 256）`);
  return out;
}
/* 调度 + 执行窗口（开始 = 结束表示不限制窗口） */
function schedErrs(t) {
  const e = [], s = t.sched, w = t.win;
  if (s.kind === 'cron') {
    const p = parseCron(s.cron);
    if (p.err) e.push(p.err);
    else { const ts = schedTimes(s, simMs(), 24); if (ts.length < 2) e.push('该 cron 在未来一段时间内触发不足 2 次'); else { let gap = Infinity; for (let i = 1; i < ts.length; i++) gap = Math.min(gap, ts[i] - ts[i - 1]); if (gap < LIM.cronGap * 60000) e.push(`cron 最短间隔 ${LIM.cronGap} 分钟（当前最短 ${Math.round(gap / 60000)} 分钟）`); } }
  } else {
    if (hm(s.time) < 0) e.push('开始时间格式应为 HH:MM');
    if (s.kind === 'week' && !(s.wd || []).length) e.push('请至少选择一个星期');
    if (s.kind === 'month' && !(Number.isInteger(+s.md) && +s.md >= 1 && +s.md <= 28)) e.push('每月的日期取 1–28（避开月末差异）');
  }
  if (hm(w.from) < 0 || hm(w.to) < 0) e.push('执行窗口格式应为 HH:MM');
  else if (hm(w.from) !== hm(w.to)) {
    if (winDur(w) < LIM.minWin) e.push(`执行窗口至少 ${LIM.minWin} 分钟（当前 ${winDur(w)} 分钟）`);
    else if (s.kind !== 'cron' && hm(s.time) >= 0 && !inWin(hm(s.time), w)) e.push(`开始时间 ${s.time} 不在执行窗口 ${w.from}–${w.to} 内，任务永远不会被调度`);
    else if (s.kind === 'cron' && !parseCron(s.cron).err) { const ts = schedTimes(s, simMs(), 5); if (ts.length && ts.every(x => !inWin(minOf(x), w))) e.push('cron 未来 5 次触发都落在执行窗口之外，任务会一直被跳过'); }
  }
  return e;
}
/* 第 step 步的校验，返回文案数组。opt.draft：存草稿只做服务端“不查库”的校验（不看主键 / 外键 / 时间列 / 审批流 / 同表互斥），不要求试运行 */
function errsStep(t, step, opt) {
  const draft = !!(opt && opt.draft), e = [], tb = findTable(t.inst, t.db, t.tbl), inst = t.inst ? instOf(t.inst) : null;
  if (step === 0) {
    const nm = String(t.name || '').trim(), nl = [...nm].length;
    if (nl < LIM.name[0] || nl > LIM.name[1]) e.push(`任务名称需要 ${LIM.name[0]}–${LIM.name[1]} 个字（当前 ${nl}）`);
    else if (TASKS.some(x => x.id !== t.id && x.name === nm)) e.push('任务名称已存在，请换一个');
    if (!t.inst) e.push('请选择实例');
    else if (!t.db) e.push('请选择库');
    else if (!tb) e.push('请选择表');
    else {
      if (protectedHit(t.db, t.tbl)) e.push(`${t.db}.${t.tbl} 属于受保护库表（安全策略），永不允许归档`);
      if (!draft) {
        if (!pkCols(tb).length) e.push('该表没有主键（含唯一非空键），不允许建任务');
        if (FK_REF[t.db + '.' + t.tbl]) e.push(`表被外键子表引用：${FK_REF[t.db + '.' + t.tbl].join('、')}；本功能不处理有外键子表引用的表`);
        if (!timeCols(tb).length) e.push('该表没有 datetime / timestamp / date 列，不能按时间归档');
        const dup = activeTaskOn(t, t.id); if (dup) e.push(`这张表已有进行中的任务「${dup.name}」（${ST_T[dup.status][0]}）；同一张表同时只能有一个进行中的任务`);
      }
    }
    if (t.inst) { if (!rgOptions(inst).length) e.push('你不属于该实例的任何资源组，无法为它建任务'); else if (!t.rg) e.push('请选择所属资源组'); else if (!draft && !archFlow(t.rg)) e.push(`资源组「${rgName(t.rg)}」没有配置“数据归档申请”审批流，不能提交；请联系管理员在「审批流程配置」里配置`); }
  } else if (step === 1) {
    if (!t.tcol) e.push('请选择时间列');
    const r = Number(t.retain);
    if (!Number.isInteger(r) || r < 1) e.push('保留天数必须是正整数');
    else if (r < POLICY.minRetain) e.push(`保留天数不得小于安全策略的最小保留天数 ${POLICY.minRetain} 天（想归档最近 ${POLICY.minRetain} 天内的数据，请联系管理员修改“安全策略”）`);
    else if (r > LIM.retainMax) e.push(`保留天数最大 ${LIM.retainMax} 天`);
    const x = normalizeExtra(t.extra); if (!x.ok) e.push('附加条件：' + x.err);
  } else if (step === 2) {
    modeIssues(t).forEach(i => e.push(i.msg));
    if (t.mode === 'cleanup') { const re = reasonErr(t); if (re) e.push(re); }
    else {
      if (t.bk.db.on) destTableIssues(t).forEach(i => i.lv === 'error' && e.push(i.msg));
      if (t.bk.file.on) destFileIssues(t).forEach(i => i.lv === 'error' && e.push(i.msg));
    }
  } else if (step === 3) paramIssues(t).forEach(i => i.lv === 'error' && e.push(i.msg));
  else if (step === 4) e.push(...schedErrs(t));
  return e;
}
/* 试运行结果与哪些字段绑定（spec_hash）：来源 / 条件 / 模式 / 备份去向 / 总量上限；判断“是否需要重新审批”时不含总量上限（提高由 looserParts 识别，降低不需要审批），但含所属资源组 */
const keyOf = (t, rows) => {
  const cl = t.mode === 'cleanup', d = t.bk.db, f = t.bk.file;
  return JSON.stringify([t.inst, t.rg, t.db, t.tbl, t.tcol, +t.retain, String(t.extra || '').trim(), t.mode, cl ? String(t.reason || '').trim() : '',
    !cl && d.on ? [d.inst || t.inst, d.db, String(d.tpl).trim()] : null, !cl && f.on ? [+f.store, f.fmt, !!f.gzip, !!f.enc, String(f.path).trim(), +f.partMB || 256] : null, rows ? +t.p.maxRows : 0]);
};
const KEY_FIELDS = t => keyOf(t, true), KEY_NOROWS = t => keyOf(t, false);
const winSet = w => { const s = new Set(), a = hm(w.from), b = hm(w.to); if (a < 0 || b < 0) return s; for (let m = a, k = 0; k < winDur(w); k++, m = (m + 1) % 1440) s.add(m); return s; };
/* 是否“放宽”（rules.go looserParts：批量只减不增、休眠只增不减、窗口只缩不扩、总量只降不升；各阈值同理） */
function looserParts(o, n) {
  const l = [], N = Number, a = o.p, b = n.p;
  if (N(b.batch) > N(a.batch)) l.push('批大小增大'); if (N(b.sleep) < N(a.sleep)) l.push('批间休眠减小'); if (N(b.maxRows) > N(a.maxRows)) l.push('单次总量上限提高');
  if (N(b.threads) > N(a.threads)) l.push('Threads_running 阈值提高'); if (N(b.lag) > N(a.lag)) l.push('复制延迟阈值放宽'); if (N(b.disk) < N(a.disk)) l.push('磁盘剩余阈值降低');
  if (N(b.targetMs) > N(a.targetMs)) l.push('批耗时目标放宽'); if (N(b.maxWait) > N(a.maxWait)) l.push('负载最长等待延长'); if (N(b.fails) > N(a.fails)) l.push('连续失败暂停阈值提高');
  const ow = winSet(o.win), nw = winSet(n.win); if ([...nw].some(m => !ow.has(m))) l.push('执行窗口扩大');
  return l;
}
/* 给人看的变更说明（变更历史 / 审计用） */
function describeChanges(o, n) {
  const d = [], a = o.p, b = n.p, N = Number;
  if (N(a.batch) !== N(b.batch)) d.push(`批大小 ${a.batch} → ${b.batch}`);
  if (N(a.sleep) !== N(b.sleep)) d.push(`批间休眠 ${a.sleep} → ${b.sleep} ms`);
  if (N(a.maxRows) !== N(b.maxRows)) d.push(`单次上限 ${N(a.maxRows)} → ${N(b.maxRows)} 行`);
  if (N(a.threads) !== N(b.threads)) d.push(`Threads_running 阈值 ${a.threads} → ${b.threads}`);
  if (N(a.lag) !== N(b.lag)) d.push(`复制延迟阈值 ${a.lag} → ${b.lag} 秒`);
  if (N(a.disk) !== N(b.disk)) d.push(`磁盘剩余阈值 ${a.disk}% → ${b.disk}%`);
  if (N(a.targetMs) !== N(b.targetMs)) d.push(`批耗时目标 ${a.targetMs} → ${b.targetMs} ms`);
  if (N(a.maxWait) !== N(b.maxWait)) d.push(`负载最长等待 ${a.maxWait} → ${b.maxWait} 分钟`);
  if (N(a.fails) !== N(b.fails)) d.push(`连续失败暂停 ${a.fails} → ${b.fails} 批`);
  if (o.win.from !== n.win.from || o.win.to !== n.win.to) d.push(`窗口 ${o.win.from}–${o.win.to} → ${n.win.from}–${n.win.to}`);
  if (schedText(o.sched) !== schedText(n.sched)) d.push(`调度 ${schedText(o.sched)} → ${schedText(n.sched)}`);
  if (o.name !== n.name) d.push(`名称 ${o.name} → ${n.name}`);
  if (KEY_NOROWS(o) !== KEY_NOROWS(n)) d.push('关键字段（来源 / 条件 / 模式 / 备份去向）有修改');
  return d;
}
/* 把 o 改成 n 是否需要重新审批（rules.go needsApproval）：草稿 / 已驳回 / 已停用 / 待审批、结构变更暂停、改了关键字段、放宽了参数 → 需要；其余（调度 / 窗口 / 限速参数且只更保守）直接保存 */
function needApprovalOf(old, o, n) {
  if (['rejected', 'disabled', 'draft', 'pending'].includes(old.status)) return {need: true, why: `任务当前状态（${ST_T[old.status][0]}）需要提交审批`};
  if (old.status === 'paused' && old.pauseKind === 'schema') return {need: true, why: '源表结构变更后需要重新试运行并提交审批'};
  if (KEY_NOROWS(o) !== KEY_NOROWS(n)) return {need: true, why: '修改了关键字段（来源 / 条件 / 模式 / 备份去向 / 总量上限）'};
  const l = looserParts(o, n);
  if (l.length) return {need: true, why: '放宽了参数：' + l.join('、')};
  return {need: false, why: ''};
}

/* ---------- 试运行（只读，结果由表行数 / 条件确定性地推出）：检查项的 key / 标题 / 级别 / 顺序与 engine/check.go、check_targets.go 一致，另加服务层的 workflow / concurrency / flow ---------- */
function sampleRows(t, tb, est) {
  const r = rng(hs(t.inst + t.db + t.tbl + t.retain + (t.extra || ''))), cut = parseT(TODAY + ' 00:00:00') - t.retain * DAY, lead = pkCols(tb)[0], base = 1000 + Math.floor(r() * 9000);
  const rows = [];
  for (let i = 0; i < Math.min(10, Math.max(est, 0)); i++) {
    rows.push(tb.cols.map(c => {
      const ty = c.t.toLowerCase();
      if (/phone|mobile/i.test(c.n)) return '138' + (10000000 + Math.floor(r() * 89999999));
      if (c.n === lead) return base + i * (2 + Math.floor(r() * 5));
      if (/^(datetime|timestamp)/.test(ty)) return tstr(cut - Math.floor(r() * 40 * DAY) - i * 3600000).replace(/ \d\d:\d\d:\d\d$/, ' ' + pad2(Math.floor(r() * 24)) + ':' + pad2(Math.floor(r() * 60)) + ':' + pad2(Math.floor(r() * 60)));
      if (/^date\b/.test(ty)) return tstr(cut - Math.floor(r() * 40 * DAY)).slice(0, 10);
      if (/order_no|pay_no|refund_no/.test(c.n)) return ({order_no: 'SO', pay_no: 'PAY', refund_no: 'RF'}[c.n]) + '2025' + String(10000000 + Math.floor(r() * 89999999));
      if (c.n === 'status') return [3, 4, 3, 3, 4][Math.floor(r() * 5)];
      if (c.n === 'action') return ['CREATE', 'PAY', 'SHIP', 'CANCEL'][Math.floor(r() * 4)];
      if (c.n === 'ip') return `10.${1 + Math.floor(r() * 8)}.${Math.floor(r() * 250)}.${2 + Math.floor(r() * 240)}`;
      if (/decimal/.test(ty)) return (Math.floor(r() * 99900 + 100) / 100).toFixed(2);
      if (/int/.test(ty)) return Math.floor(r() * (c.n.endsWith('_id') ? 18000000 : 5)) + 1;
      return c.n === 'operator' ? ['system', 'dba_li', 'cs_wu'][Math.floor(r() * 3)] : (c.n + '_' + Math.floor(r() * 9999)).slice(0, 24);
    }));
  }
  return rows;
}
const COUNT_CAP = 10000000, EXPLAIN_LIMIT = 200000, BIG_PCT = 30;
function dryCompute(t, key) {
  const tb = findTable(t.inst, t.db, t.tbl), inst = instOf(t.inst), items = [], add = (k, title, lv, msg) => items.push({k, title, lv, msg});
  if (!tb) return null;
  /* 连不上源库：真实实现返回 502 source_error（没有检查报告），向导里列在“提交前需要先修正”下面 */
  if (inst.status === 'bad') return {key: key || '', at: simMs(), error: `连接源 / 目标失败：dial tcp ${inst.host}:${inst.port}: i/o timeout`};
  const p = t.p, maxRows = Math.min(+p.maxRows, POLICY.maxRows), rows = tb.rows || 1, pk = pkCols(tb), tcc = tb.cols.find(c => c.n === t.tcol), batch = +p.batch;
  /* ① 规格本身 */
  const hit = protectedHit(t.db, t.tbl);
  add('protected', '受保护库表', hit ? 'error' : 'ok', hit ? `${t.db}.${t.tbl} 属于受保护库表（安全策略），永不允许归档` : '不在受保护库表名单内');
  add('retention', '保留天数', +t.retain >= POLICY.minRetain ? 'ok' : 'error', +t.retain >= POLICY.minRetain ? `保留 ${t.retain} 天，不小于策略下限 ${POLICY.minRetain} 天` : `保留天数不得小于安全策略的最小保留天数 ${POLICY.minRetain} 天`);
  const ex = normalizeExtra(t.extra), exTxt = String(t.extra || '').trim();
  add('extra', '条件语法与安全', ex.ok ? 'ok' : 'error', ex.ok ? (exTxt ? '附加条件通过检查（只含比较 / IN / LIKE / AND / OR / IS NULL）' : '没有附加条件') : '附加条件：' + ex.err);
  const mi = modeIssues(t);
  if (mi.length) mi.forEach(i => add('mode', '模式与备份去向', i.lv, i.msg)); else add('mode', '模式与备份去向', 'ok', MODES[t.mode][0]);
  const useDb = t.mode !== 'cleanup' && t.bk.db.on, useFile = t.mode !== 'cleanup' && t.bk.file.on;
  if (useDb) { const di = destTableIssues(t); if (di.length) di.forEach(i => add('dest_table_spec', '归档库表设置', i.lv, i.msg)); else add('dest_table_spec', '归档库表设置', 'ok', '目标库表设置合法'); }
  if (useFile) { const fi = destFileIssues(t).filter(i => !/备份存储「/.test(i.msg)); if (fi.length) fi.forEach(i => add('dest_file_spec', '文件备份设置', i.lv, i.msg)); else add('dest_file_spec', '文件备份设置', 'ok', '文件备份设置合法'); }
  const pi = paramIssues(t);
  if (pi.length) pi.forEach(i => add('params', '稳定性参数', i.lv, i.msg)); else add('params', '稳定性参数', 'ok', '参数在允许范围内');
  /* ② 源表结构 */
  if (!pk.length) add('pk', '主键', 'error', '没有主键（含唯一非空键），不允许建任务'); else add('pk', '主键', 'ok', `主键 (${pk.join(', ')})，按主键升序分批`);
  const timeOK = !!tcc && /^(datetime|timestamp|date)\b/i.test(tcc.t);
  if (!t.tcol) { /* 未选时间列：向导里已拦截 */ } else if (!tcc) add('time_column', '时间列', 'error', `表里没有列 ${t.tcol}`);
  else if (!timeOK) add('time_column', '时间列', 'error', `时间列 ${tcc.n} 的类型是 ${tcc.t}，必须是 date / datetime / timestamp`);
  else add('time_column', '时间列', 'ok', `${tcc.n}（${tcc.t}）`);
  /* 原型的列信息只有“键标记”：把有键标记的时间列当作“是某个索引的最左列” */
  const indexed = timeOK && tcc.key !== '', idxName = indexed ? ((tb.idx || []).find(x => x[1].includes(t.tcol)) || [`idx_${t.tcol}`])[0] : '';
  if (timeOK) {
    if (indexed) add('index', '时间列索引', 'ok', `时间列是索引 ${idxName} 的最左列`);
    else if (rows > EXPLAIN_LIMIT) add('index', '时间列索引', 'error', `时间列 ${tcc.n} 上没有索引，表约 ${rows} 行，每批读取会大范围扫描，会拖垮线上，请先建索引`);
    else add('index', '时间列索引', 'warn', `时间列 ${tcc.n} 上没有索引（表较小，约 ${rows} 行，可以接受）`);
  }
  let extraColsOK = ex.ok;
  if (ex.ok && exTxt) {
    const unknown = ex.cols.filter(c => !tb.cols.some(ic => ic.n.toLowerCase() === c.toLowerCase()));
    if (unknown.length) { add('extra', '条件语法与安全', 'error', `附加条件里的 ${unknown.join('、')} 不是表 ${t.tbl} 的列`); extraColsOK = false; }
  }
  const fk = FK_REF[t.db + '.' + t.tbl];
  if (fk) add('fk', '外键', 'error', `被外键子表引用：${fk.join('、')}；本功能不处理有外键子表引用的表`); else add('fk', '外键', 'ok', '没有外键子表引用');
  const trg = ((dbsOf(t.inst).find(d => d.db === t.db) || {}).triggers || []).filter(x => x.tbl === t.tbl);
  if (trg.length) add('trigger', '触发器', 'warn', '表上有触发器：归档删除会触发 DELETE 触发器，请确认其副作用'); else add('trigger', '触发器', 'ok', '没有触发器');
  if (tb.part) add('partition', '分区表', 'warn', '这是分区表：按普通表处理分批删除；若按时间分区，更推荐用「分区维护」直接 DROP PARTITION，开销小得多'); else add('partition', '分区表', 'ok', '不是分区表');
  if (tb.eng && tb.eng !== 'InnoDB') add('engine', '存储引擎', 'warn', `存储引擎是 ${tb.eng}，不是 InnoDB：删除不支持行级锁与回滚，请谨慎`);
  const big = tb.cols.filter(c => /blob|text|json/i.test(c.t));
  if (big.length) add('blob', '大字段', batch > 500 ? 'warn' : 'ok', `含大字段列 ${big.map(c => c.n).join('、')}，建议批大小不超过 500（当前 ${batch}）`); else add('blob', '大字段', 'ok', '没有 BLOB / TEXT / JSON 大字段');
  /* ③ EXPLAIN、命中行数、样例 */
  const canQuery = ex.ok && extraColsOK && timeOK && pk.length > 0;
  const span = SCHEMA_SPAN(tb), share0 = Math.max(0, Math.min(1, (span - t.retain) / span)), exF = exTxt ? 0.35 + (hs(exTxt) % 40) / 100 : 1, jit = 1 + ((hs(t.tbl + t.retain) % 7) - 3) / 100;
  let est = 0, capped = false, tableRows = rows, pct = 0, sample = [];
  if (canQuery) {
    est = Math.round(rows * share0 * exF * jit);
    if (est > COUNT_CAP) { est = COUNT_CAP; capped = true; }
    const scan = indexed ? Math.min(rows, Math.round(est * 1.05)) : rows, key0 = idxName;
    if (!indexed) add('explain', 'EXPLAIN', scan > EXPLAIN_LIMIT ? 'error' : 'warn', scan > EXPLAIN_LIMIT ? `type=ALL，未使用索引，预计扫描 ≈ ${nf(scan)} 行，远超阈值 ${nf(EXPLAIN_LIMIT)}：会拖垮线上，请先建索引` : `type=ALL，未使用索引，预计扫描 ≈ ${nf(scan)} 行（表较小，可以接受）`);
    else if (scan > EXPLAIN_LIMIT) add('explain', 'EXPLAIN', 'warn', `key=${key0}，type=range，预计扫描 ≈ ${nf(scan)} 行，超过阈值 ${nf(EXPLAIN_LIMIT)}`);
    else add('explain', 'EXPLAIN', 'ok', `type=range，key=${key0}，预计扫描 ≈ ${nf(scan)} 行，未超过阈值`);
    if (tableRows < est) tableRows = est;
    pct = tableRows > 0 ? est * 100 / tableRows : 0;
    const capNote = capped ? '（统计到上限，实际更多）' : '';
    if (est === 0) add('est_rows', '命中行数', 'warn', '按当前条件没有符合条件的数据，任务运行后将什么也不做');
    else if (pct > BIG_PCT) add('large_batch', '命中比例', 'warn', `命中 ${nf(est)} 行${capNote}，占表约 ${pct.toFixed(1)}%，超过 ${BIG_PCT}%：属于大批量，提交时需要勾选“已确认大批量”`);
    else add('large_batch', '命中比例', 'ok', `命中 ${nf(est)} 行${capNote}，占表约 ${pct.toFixed(1)}%（未超过 ${BIG_PCT}%）`);
    if (est > maxRows) add('max_rows', '单次总量上限', 'warn', `命中 ${nf(est)} 行 > 单次上限 ${nf(maxRows)} 行：需要约 ${Math.ceil(est / maxRows)} 次运行才能清完，每次以“已达上限”结束`);
    else add('max_rows', '单次总量上限', 'ok', `命中 ${nf(est)} 行 ≤ 单次上限 ${nf(maxRows)} 行，一次可跑完`);
    sample = sampleRows(t, tb, est);
  }
  const eff = Math.min(est, maxRows), batches = eff > 0 ? Math.ceil(eff / batch) : 0, rowB = Math.max(120, Math.round((tb.size || 100) * 1048576 / rows));
  const secs = eff > 0 ? Math.ceil(batches * (120 + batch * 0.22 + +p.sleep) / 1000) : 0, binlogMB = eff > 0 && t.mode !== 'backup' ? Math.ceil(eff * rowB * 1.1 / 1048576) : 0;
  /* ④ 模式 / 备份去向 */
  if (t.mode === 'cleanup') add('purge', '不备份清理', 'warn', '仅清理不备份，删除后无法恢复');
  if (useDb) {
    const d = t.bk.db, ti = instOf(d.inst), dd = dbsOf(ti.id).find(x => x.db === d.db), tn = tplExpand(d.tpl, t), exT = dd && dd.tables.find(x => x.n === tn);
    if (ti.status === 'bad') add('dest_table', '目标 · 归档库表', 'error', `没有目标库连接（实例 #${d.inst} 不可用？）`);
    else if (!dd) add('dest_table', '目标 · 归档库表', 'error', `目标库 ${d.db} 不存在，请先建库`);
    else {
      if (exT) {
        const miss = tb.cols.filter(c => !exT.cols.some(x => x.n === c.n));
        add('dest_table', '目标 · 归档库表', miss.length ? 'error' : 'ok', miss.length ? `目标表 ${d.db}.${tn} 已存在，但缺少列 ${miss.map(c => c.n).join('、')}，结构不兼容` : `目标表 ${d.db}.${tn} 已存在且结构兼容；主键冲突视为“已存在”，比对一致才算成功`);
      } else add('dest_table', '目标 · 归档库表', 'ok', `目标表 ${d.db}.${tn} 不存在，运行时自动按源表结构创建`);
      const pctD = 40 + hs(ti.name) % 40;
      add('dest_disk', '目标 · 磁盘', pctD < +p.disk ? 'error' : 'ok', pctD < +p.disk ? `目标端磁盘剩余 ${pctD}.0% 低于阈值` : `目标端磁盘剩余 ${pctD}.0%`);
    }
  }
  if (useFile) {
    const f = t.bk.file, s = storeOf(f.store);
    if (!s || !s.enabled) add('dest_store', '目标 · 文件存储', 'error', `备份存储 #${f.store} 不可用（未配置或已停用）`);
    else {
      const pr = storeProbe(s);
      if (!pr.ok) add('dest_store', '目标 · 文件存储', 'error', '连通性测试失败：' + pr.msg.replace(/^测试失败：/, ''));
      else {
        const need = est * Math.max(120, rowB) / (f.gzip ? 3 : 1), mb = n => Math.floor(n / 1048576);
        if (!isFsT(s.type)) add('dest_store', '目标 · 文件存储', 'ok', `连通性正常（探测耗时 ${pr.ms} ms），无法得知可用空间（对象存储）`);
        else if (need > 0 && (s.free || 0) < need) add('dest_store', '目标 · 文件存储', 'error', `可用空间 ${nf(mb(s.free || 0))} MB 小于预计备份体积约 ${nf(mb(need))} MB`);
        else add('dest_store', '目标 · 文件存储', 'ok', `连通性正常（探测耗时 ${pr.ms} ms），可用空间 ${nf(mb(s.free || 0))} MB，预计备份体积约 ${nf(mb(need))} MB`);
      }
    }
  }
  /* ⑤ 负载与从库 */
  const sl = slavesOf(inst);
  add('replica', '从库复制延迟', 'ok', sl.length ? `检测到从库，每批前检查复制延迟 ≤ ${p.lag} 秒` : '没有从库，复制延迟检查自动跳过');
  const thr = 6 + hs(inst.name) % 9 + (inst.status === 'warn' ? 30 : 0), lag = sl.length ? (hs(inst.name + 'lag') % 30) / 10 : 0, dsk = 50 + hs(inst.name + 'disk') % 30;
  add('load', '当前负载', thr > +p.threads || (sl.length && lag > +p.lag) || dsk < +p.disk ? 'warn' : 'ok',
    [`Threads_running=${thr}（阈值 ${p.threads}）`].concat(sl.length ? [`复制延迟 ${lag} 秒（阈值 ${p.lag} 秒）`] : [], [`源库磁盘剩余 ${dsk}.0%（阈值 ${p.disk}%）`]).join('；'));
  /* ⑥ 服务层补充的项：未结束的 SQL 上线工单、实例并发、审批流是否存在 */
  const wf = (window.WORKFLOWS || []).filter(w => w.inst === t.inst && w.db === t.db && WF_OPEN.includes(w.status) && new RegExp('\\b' + t.tbl + '\\b', 'i').test(w.sql || ''));
  add('workflow', 'SQL 上线工单', wf.length ? 'warn' : 'ok', wf.length ? `源表有未结束的 SQL 上线工单：${wf.map(w => '#' + w.id + '「' + w.name + '」（' + w.status + '）').join('、')}；工单结束前，定时运行会被跳过` : '源表没有待执行 / 执行中的 SQL 上线工单');
  const conc = [...SIMS.values()].filter(s => s.task.inst === t.inst && s.task.id !== t.id).length;
  add('concurrency', '实例并发', conc >= POLICY.maxConc ? 'warn' : 'ok', conc >= POLICY.maxConc ? `该实例当前已有 ${conc} 个归档运行在跑（上限 ${POLICY.maxConc}），定时运行可能被推迟` : `该实例当前 ${conc} 个归档运行（上限 ${POLICY.maxConc}）`);
  if (t.rg) add('flow', '审批流', archFlow(t.rg) ? 'ok' : 'error', archFlow(t.rg) ? '已配置“数据归档申请”审批流' : '该资源组没有配置“数据归档申请”审批流，不能提交；请联系管理员在「审批流程配置」里配置');
  const bad = items.filter(i => i.lv === 'error' && i.k !== 'large_batch').length, warn = items.filter(i => i.lv === 'warn' && i.k !== 'large_batch').length;
  const cols = tb.cols.map(c => c.n);
  return {key: key || '', at: simMs(), est, capped, rows: tableRows, pct, eff, batches, secs, binlogMB, sample, cols, pkCols: pk, sql: sqlPreview(t, {pk, cols}), items, bad, warn, needBig: pct > BIG_PCT};
}
const dryValid = (d, key) => !!d && !d.error && d.key === key && simMs() - d.at < 7 * DAY;

/* ============================================================================
   任务 / 运行记录 种子 与 运行记录构造
   ========================================================================== */
const P = o => Object.assign({batch: POLICY.batch, sleep: POLICY.sleep, threads: POLICY.threads, lag: POLICY.lag, disk: POLICY.disk, maxWait: POLICY.maxWait, targetMs: POLICY.targetMs, maxRows: Math.min(5000000, POLICY.maxRows), fails: POLICY.fails}, o || {});
const BK0 = () => ({db: {on: false, inst: 0, db: '', tpl: '{table}_{yyyy}'}, file: {on: false, store: 0, fmt: 'csv', gzip: true, enc: false, path: '{db}/{table}/{yyyy}{mm}{dd}', partMB: 256}});
const blankTask = () => ({id: 0, name: '', rg: 0, inst: 0, db: '', tbl: '', tcol: '', retain: Math.max(180, POLICY.minRetain), extra: '', mode: 'archive', reason: '', bk: Object.assign(BK0(), {db: {on: true, inst: 0, db: '', tpl: '{table}_{yyyy}'}}),
  sched: {kind: 'day', time: '02:00', wd: [1], md: 1, cron: '0 2 * * *'}, win: {from: '01:00', to: '05:00'}, p: P(), status: 'draft'});
function mkTask(o) {
  const t = Object.assign({id: ++TASK_ID, extra: '', reason: '', status: 'pending', hist: [], cum: {rows: 0, runs: 0}, appr: null, last: null, running: 0, ck: [], sched: {kind: 'day', time: '02:00', wd: [1], md: 1, cron: '0 2 * * *'}}, o);
  t.p = P(o.p); t.bk = {db: Object.assign(BK0().db, (o.bk || {}).db), file: Object.assign(BK0().file, (o.bk || {}).file)};
  t.sched = Object.assign({kind: 'day', time: '02:00', wd: [1], md: 1, cron: '0 2 * * *'}, o.sched); if (t.fp == null) t.fp = fpOf(t.inst, t.db, t.tbl);
  TASKS.push(t); return t;
}
const A = (lv, user, at, note) => ({lv, user, at, note: note || '', action: 'approve'});
const approved = (applicant, at, steps) => ({applicant, at, levels: ['g_tl', 'g_dba'], level: 2, steps});
(function seedTasks() {
  mkTask({name: '订单主表-已完成订单归档', rg: 1, inst: 1, db: 'order_db', tbl: 'orders', tcol: 'created_at', retain: 365, extra: 'status IN (3,4)', mode: 'archive', status: 'enabled', creator: 'rd_zhao', created: '2026-09-14 10:20',
    bk: {db: {on: true, inst: 1, db: 'order_archive', tpl: 'orders_{yyyy}'}}, sched: {kind: 'day', time: '02:00'}, win: {from: '01:00', to: '05:00'},
    appr: approved('rd_zhao', '2026-09-14 10:20', [A(0, 'tl_wang', '2026-09-14 11:05', '同意，保留 365 天符合财务要求'), A(1, 'dba_li', '2026-09-14 14:30', '试运行 3 项检查通过')]),
    hist: [{at: '2026-09-14 10:20', user: 'rd_zhao', text: '提交申请'}, {at: '2026-09-14 14:30', user: 'dba_li', text: '审批通过，任务已启用'}, {at: '2026-09-21 09:12', user: 'dba_li', text: '调整参数（更保守，无需重新审批）：批间休眠 100 → 200 ms'},
      {at: '2026-10-02 02:05', user: 'system', text: '自动暂停：备份校验不一致，已自动暂停（该批未删除任何源数据）。'}, {at: '2026-10-02 09:30', user: 'dba_li', text: '恢复任务（按调度继续，从断点接着跑）'}]});
  mkTask({name: '订单流水日志-冷数据入OSS', rg: 1, inst: 1, db: 'order_db', tbl: 'order_log', tcol: 'created_at', retain: 90, mode: 'archive', status: 'enabled', creator: 'rd_zhao', created: '2026-09-10 15:02',
    bk: {file: {on: true, store: 4, fmt: 'csv', gzip: true, enc: true, path: '{db}/{table}/{yyyy}{mm}{dd}/{run_id}'}}, sched: {kind: 'week', time: '01:30', wd: [0]}, win: {from: '01:00', to: '06:00'}, p: {batch: 2000, sleep: 300},
    appr: approved('rd_zhao', '2026-09-10 15:02', [A(0, 'tl_wang', '2026-09-10 16:20', '同意'), A(1, 'dba_li', '2026-09-11 09:40', '分区表，后续建议改用分区维护 DROP PARTITION；先按此方案落冷')]),
    hist: [{at: '2026-09-10 15:02', user: 'rd_zhao', text: '提交申请'}, {at: '2026-09-11 09:40', user: 'dba_li', text: '审批通过，任务已启用'}]});
  mkTask({name: '测试库订单流水清理', rg: 1, inst: 7, db: 'order_db', tbl: 'order_log', tcol: 'created_at', retain: 30, mode: 'cleanup', status: 'enabled', creator: 'rd_zhao', created: '2026-09-18 17:30',
    reason: '测试环境压测产生的临时流水，无保留价值，已和测试负责人确认可直接清理', sched: {kind: 'day', time: '03:30'}, win: {from: '03:00', to: '05:00'},
    appr: approved('rd_zhao', '2026-09-18 17:30', [A(0, 'tl_wang', '2026-09-18 17:50', '测试库，同意不备份'), A(1, 'dba_li', '2026-09-19 09:05', '已放开“允许不备份清理”策略，仅限本任务')]),
    hist: [{at: '2026-09-18 17:30', user: 'rd_zhao', text: '提交申请（试运行于 2026-09-18 17:12）'}, {at: '2026-09-19 09:05', user: 'dba_li', text: '审批通过，任务已启用'}]});
  mkTask({name: '测试库订单归档（双备份）', rg: 1, inst: 7, db: 'order_db', tbl: 'orders', tcol: 'created_at', retain: 180, extra: 'status IN (3,4) AND amount > 0', mode: 'archive', status: 'pending', creator: 'rd_zhao', created: '2026-10-07 09:58',
    bk: {db: {on: true, inst: 7, db: 'order_archive', tpl: 'orders_{yyyy}'}, file: {on: true, store: 2, fmt: 'sql', gzip: true, enc: false, path: '{db}/{table}/{yyyy}{mm}/{run_id}'}}, sched: {kind: 'day', time: '02:30'}, win: {from: '02:00', to: '05:00'},
    appr: {applicant: 'rd_zhao', at: '2026-10-07 09:58', levels: ['g_tl', 'g_dba'], level: 0, steps: []}, hist: [{at: '2026-10-07 09:58', user: 'rd_zhao', text: '提交申请（试运行于 2026-10-07 09:40）'}]});
  mkTask({name: '订单归档库2025冷归档', rg: 1, inst: 1, db: 'order_archive', tbl: 'orders_2025', tcol: 'created_at', retain: 365, mode: 'archive', status: 'paused', creator: 'rd_zhao', created: '2026-08-28 14:10',
    bk: {file: {on: true, store: 2, fmt: 'csv', gzip: true, enc: false, path: '{db}/{table}/{yyyy}{mm}/{run_id}'}}, sched: {kind: 'month', time: '02:00', md: 1}, win: {from: '01:00', to: '05:00'}, p: {batch: 3000, sleep: 100},
    pauseKind: 'auto', pauseAt: '2026-10-01 02:09', pauseReason: '连续多批失败，已自动暂停；失败批已回滚，未删除任何未备份的数据。 详情：ERROR 1205 (HY000): Lock wait timeout exceeded; try restarting transaction（连续 3 批失败，从库 mysql-order-prod-s1 复制延迟 48 s）',
    appr: approved('rd_zhao', '2026-08-28 14:10', [A(0, 'tl_wang', '2026-08-28 15:00', ''), A(1, 'dba_li', '2026-08-29 10:11', '')]),
    hist: [{at: '2026-08-28 14:10', user: 'rd_zhao', text: '提交申请'}, {at: '2026-08-29 10:11', user: 'dba_li', text: '审批通过，任务已启用'}, {at: '2026-10-01 02:09', user: 'system', text: '自动暂停：连续多批失败，已自动暂停；失败批已回滚，未删除任何未备份的数据。'}]});
  mkTask({name: '用户表-未激活账号归档', rg: 4, inst: 9, db: 'user_db', tbl: 'users', tcol: 'created_at', retain: 60, extra: 'level = 0', mode: 'archive', status: 'rejected', creator: 'rd_xu', created: '2026-10-02 11:40',
    bk: {file: {on: true, store: 1, fmt: 'csv', gzip: true, enc: false, path: '{db}/{table}/{yyyy}{mm}{dd}/{run_id}'}}, sched: {kind: 'day', time: '03:00'}, win: {from: '02:00', to: '05:00'},
    appr: {applicant: 'rd_xu', at: '2026-10-02 11:40', levels: ['g_dba'], level: 0, steps: [{lv: 0, user: 'dba_li', at: '2026-10-02 15:12', action: 'reject', note: ''}], reject: {user: 'dba_li', at: '2026-10-02 15:12', reason: 'created_at 上没有索引，按条件会全表扫描 mariadb-log-prod；users 还被 user_profile 外键引用，本功能不处理。请先建索引并处理外键关系后重新提交。'}},
    hist: [{at: '2026-10-02 11:40', user: 'rd_xu', text: '提交申请'}, {at: '2026-10-02 15:12', user: 'dba_li', text: '驳回：created_at 上没有索引，按条件会全表扫描 mariadb-log-prod；users 还被 user_profile 外键引用，本功能不处理。请先建索引并处理外键关系后重新提交。'}]});
  mkTask({name: '用户登录日志按周归档', rg: 3, inst: 4, db: 'user_db', tbl: 'login_log', tcol: 'created_at', retain: 180, mode: 'archive', status: 'paused', creator: 'dev_liu', created: '2026-08-20 10:00',
    bk: {file: {on: true, store: 1, fmt: 'csv', gzip: true, enc: false, path: '{db}/{table}/{yyyy}{mm}/{run_id}'}}, sched: {kind: 'week', time: '04:00', wd: [1]}, win: {from: '03:00', to: '06:00'},
    fp: 'a91c0d77be32', pauseKind: 'schema', pauseAt: '2026-10-05 04:00', pauseReason: '源表结构指纹与审批时不一致（列名 / 类型 / 主键有变化），已暂停。需要重新试运行并重新提交审批。 详情：源表结构指纹与审批 / 试运行时不一致（列名、类型或主键有变化），需要重新试运行',
    appr: {applicant: 'dev_liu', at: '2026-08-20 10:00', levels: ['g_dba'], level: 1, steps: [A(0, 'dba_li', '2026-08-20 14:20', '分区表，已确认保留 180 天')]},
    hist: [{at: '2026-08-20 10:00', user: 'dev_liu', text: '提交申请'}, {at: '2026-08-20 14:20', user: 'dba_li', text: '审批通过，任务已启用'}, {at: '2026-10-05 04:00', user: 'system', text: '结构指纹变化，任务已暂停并通知'}]});
  mkTask({name: '登录日志归档到MinIO', rg: 4, inst: 9, db: 'user_db', tbl: 'login_log', tcol: 'created_at', retain: 90, mode: 'archive', status: 'pending', creator: 'rd_xu', created: '2026-10-07 10:40',
    bk: {file: {on: true, store: 3, fmt: 'csv', gzip: true, enc: true, path: '{db}/{table}/{yyyy}{mm}{dd}/{run_id}'}}, sched: {kind: 'day', time: '01:30'}, win: {from: '01:00', to: '04:00'}, p: {batch: 1000, sleep: 300},
    appr: {applicant: 'rd_xu', at: '2026-10-07 10:40', levels: ['g_dba'], level: 0, steps: []}, hist: [{at: '2026-10-07 10:40', user: 'rd_xu', text: '提交申请（试运行于 2026-10-07 10:30）'}]});
  /* 草稿：保存草稿不要求试运行、不建审批；可在向导里继续编辑、试运行、提交 */
  mkTask({name: '订单归档库2025-测试环境备份（草稿）', rg: 1, inst: 7, db: 'order_archive', tbl: 'orders_2025', tcol: 'created_at', retain: 730, mode: 'backup', status: 'draft', creator: 'rd_zhao', created: '2026-10-06 16:05',
    bk: {file: {on: true, store: 1, fmt: 'csv', gzip: true, enc: false, path: '{db}/{table}/{yyyy}{mm}/{run_id}'}}, sched: {kind: 'week', time: '03:00', wd: [6]}, win: {from: '02:00', to: '05:00'}, p: {sleep: 100},
    hist: [{at: '2026-10-06 16:05', user: 'rd_zhao', text: '创建任务（草稿）'}]});
})();

/* ---------- 运行记录构造 ---------- */
const RUN_STATUS_DB = id => (instOf(id) || {}).name;
function fillBackups(run, t, st, start) {
  const tb = findTable(t.inst, t.db, t.tbl);
  if (run.backed > 0) {
    const d = dbTarget(t); if (d) run.backups.push({kind: 'db', inst: RUN_STATUS_DB(d.inst), loc: `${d.db}.${tplExpand(t.bk.db.tpl, t, 1, start)}`, rows: run.backed});
    if (t.bk.file.on) {
      const f = t.bk.file, s = storeOf(f.store), rowB = Math.max(80, Math.round(((tb && tb.size) || 100) * 1048576 / ((tb && tb.rows) || 1) * (f.gzip ? 0.3 : 0.6))), per = Math.max(1, Math.floor((+f.partMB || 256) * 1048576 / Math.max(80, rowB * (f.gzip ? 3 : 1)))),
        nf0 = Math.max(1, Math.ceil(run.backed / per)), dir = pathOf(f.path, t, run.id, start), files = [];
      for (let k = 0; k < nf0; k++) { const rws = k < nf0 - 1 ? per : run.backed - per * (nf0 - 1); files.push({key: `${dir}/part-${String(k + 1).padStart(4, '0')}.${fileExt(f)}`, rows: rws, bytes: rws * rowB, sha256: hex('f' + run.id + k, 64)}); }
      run.backups.push({kind: 'file', store: s ? s.name : '（存储已删除）', storeId: f.store, path: dir, files, rows: run.backed, bytes: files.reduce((a, x) => a + x.bytes, 0), enc: f.enc ? 'aes-256-gcm' : '', manifestSha: hex('m' + run.id, 64), manifestOk: st !== 'failed' && st !== 'verify'});
    }
  }
}
/* 校验结果（runner.go verifyOf）：目标端 COUNT / 主键集合（库表备份）、写入行数 / 清单回读 sha256（文件备份）、删除行数；仅清理只校验删除行数 */
function fillVerify(run, t, st) {
  const bad = st === 'verify', failed = st === 'failed', V = run.verify;
  if (t.mode === 'cleanup') V.push({k: '无备份（仅清理）', ok: true, msg: '本任务不备份，只校验删除行数'});
  else {
    if (t.bk.db.on) {
      V.push({k: '目标端 COUNT 与读取行数一致', ok: !bad, msg: bad ? run.err : `${nf(run.batches)} 批全部一致`});
      V.push({k: '目标端主键集合与读到的一致', ok: !bad, msg: bad ? run.err : '逐批比对通过'});
    }
    if (t.bk.file.on) {
      const fb = run.backups.find(b => b.kind === 'file');
      V.push({k: '写入行数 = 读到行数', ok: !bad, msg: bad ? run.err : `${nf(run.backed)} 行`});
      V.push({k: '清单回读校验（sha256）', ok: !!fb && !bad && !failed, msg: fb && !bad && !failed ? `${fb.files.length} 个文件，回读校验 sha256 一致` : '运行未生成清单'});
    }
  }
  if (t.mode !== 'backup') V.push({k: '删除行数 = 读到行数', ok: !bad && (run.deleted === run.backed || t.mode === 'cleanup'), msg: `读取 ${nf(run.scanned)} 行，删除 ${nf(run.deleted)} 行`});
}
const FIRST_PK = t => 1000 + hs('pk0' + t.id + t.tbl) % 9000;      // 任务第一次运行时的起始主键（原型里随便给一个确定的数）
function mkRun(t, o) {
  const batch = +t.p.batch, r = rng(hs('mk' + t.id + o.start + o.trg)), dry = o.trg === 'dry', rows = o.rows, st = o.st, tb = findTable(t.inst, t.db, t.tbl), skipped = st === 'skipped', quick = skipped || st === 'schema';
  const dur = o.dur != null ? o.dur : o.durMin * 60000;
  const okRows = st === 'failed' || st === 'verify' ? Math.max(0, rows - batch) : rows;
  const run = {id: ++RUN_ID, task: t.id, name: t.name, inst: t.inst, tbl: t.db + '.' + t.tbl, trg: o.trg, start: o.start, end: o.start + dur, status: st, by: o.by || (o.trg === 'sched' ? 'system' : ME.login), scanned: rows,
    backed: o.back != null ? o.back : dry || t.mode === 'cleanup' ? 0 : okRows, deleted: dry || o.trg === 'restore' || t.mode === 'backup' ? 0 : st === 'verify' ? Math.max(0, okRows - batch) : okRows, batches: dry || quick ? 0 : Math.ceil(rows / batch), dur,
    peakThr: quick || dry ? 0 : st === 'load' ? (o.thr || 58) : 11 + Math.floor(r() * 14), peakLag: quick || dry ? 0 : Math.round((0.3 + r() * 3.6) * 10) / 10, throttles: 0, series: [], bsize: [], events: [], verify: [], backups: [], err: o.err || '', log: [], restoreOf: o.of || 0, restored: [], pkFrom: 0, pkTo: 0, note: o.note || '', autoPause: !!o.autoPause};
  const n = 48, avg = dur > 0 ? rows / (dur / 1000) : 0;
  if (!dry && o.trg !== 'restore' && !quick) {
    const nth = st === 'load' ? 1 : (st === 'limit' || st === 'window') ? 2 : r() < 0.35 ? 1 : 0, at = [];
    for (let k = 0; k < nth; k++) at.push(Math.floor((0.25 + 0.4 * k + r() * 0.2) * n));
    run.throttles = nth;
    at.forEach((i, k) => {
      const lagEv = k % 2 === 1 || (st !== 'load' && r() < 0.4), w = st === 'load' ? 600 : 30 * (1 + Math.floor(r() * 3));
      run.events.push({at: tstr(o.start + dur * i / n), kind: lagEv ? 'lag' : 'threads', val: lagEv ? Math.round((6 + r() * 6) * 10) / 10 : (st === 'load' ? run.peakThr : 33 + Math.floor(r() * 12)), thr: lagEv ? t.p.lag : t.p.threads, waited: w, end: st === 'load' ? 'abort' : 'resume', batchTo: lagEv ? 0 : Math.max(250, batch / 2)});
    });
    for (let i = 0; i < n; i++) {
      const dip = at.some(x => i >= x && i < x + (st === 'load' ? 2 : 1));
      run.series.push(Math.max(0, Math.round(dip ? avg * 0.05 : avg * (0.78 + r() * 0.44))));
      run.bsize.push(dip || at.some(x => i > x && i <= x + 3) ? Math.max(250, batch / 2) : batch);
    }
  }
  const lead = tb ? pkCols(tb)[0] : 'id', from = o.pkFrom != null ? o.pkFrom : FIRST_PK(t) + Math.floor(r() * 90000);
  run.pkFrom = rows && !quick ? from : 0; run.pkTo = rows && !quick ? (o.pkTo != null ? o.pkTo : from + Math.round(rows * 1.4)) : 0; run.pkName = lead;
  if (!dry && o.trg !== 'restore' && !quick && st !== 'interrupted') { fillBackups(run, t, st, o.start); fillVerify(run, t, st); }
  /* 日志（措辞对齐 engine 的事件：运行开始 / 负载超限 / 负载恢复 / 窗口 / 上限 / 结束） */
  const L = run.log, at = ms => tstr(ms).slice(11), wh = whereOf(t);
  if (dry) L.push(`${at(o.start)} 试运行开始（只读：不写、不删）`, `${at(o.start + 2000)} EXPLAIN 完成，预估命中 ${nf(rows)} 行`, `${at(o.start + dur)} 试运行${st === 'failed' ? '未通过（存在红色检查项）' : '完成'}`);
  else if (o.trg === 'restore') L.push(`${at(o.start)} 恢复开始：run #${run.restoreOf}，主键范围 ${run.pkName} ∈ [${run.pkFrom}, ${run.pkTo}]`, `${at(o.start + 1000)} INSERT IGNORE 回灌（只补缺、不覆盖）`, `${at(o.start + dur)} 完成：补回 ${nf(run.backed)} 行，${nf(Math.max(0, rows - run.backed))} 行已存在被跳过`);
  else if (skipped) L.push(`${at(o.start)} 调度到点，前置检查未通过，本次跳过：${run.note}`);
  else {
    L.push(`${at(o.start)} 运行开始（${MODES[t.mode][0]}），条件：${wh}`);
    if (st === 'schema') L.push(`${at(o.start + 800)} 源表结构与审批 / 试运行时不一致，已停止：${t.fp.slice(0, 8)} → ${hex('drift' + t.id, 8)}`);
    else {
      if (t.bk.db.on && t.mode !== 'cleanup') L.push(`${at(o.start + 600)} 归档库表 ${t.bk.db.db}.${tplExpand(t.bk.db.tpl, t, 1, o.start)} 就绪`);
      L.push(`${at(o.start + 1500)} 从断点 ${run.pkName} > ${run.pkFrom} 继续，批大小 ${batch}，休眠 ${t.p.sleep} ms`);
      run.events.forEach(e => L.push(`${e.at.slice(11)} 负载超限，暂停等待：${e.kind === 'lag' ? `复制延迟 ${e.val} 秒超过阈值 ${e.thr} 秒` : `Threads_running=${e.val} 超过阈值 ${e.thr}`}` + (e.end === 'abort' ? `；负载持续超限超过 ${t.p.maxWait * 60} 秒，本次运行中断` : `；负载恢复正常，已等待 ${e.waited} 秒，继续运行${e.batchTo ? `（批大小降到 ${e.batchTo}）` : ''}`)));
      if (st === 'window') L.push(`${at(o.start + dur)} 已超出执行窗口 ${t.win.from}–${t.win.to}，优雅停止，下次从断点继续`);
      if (st === 'limit') L.push(`${at(o.start + dur)} 已达单次运行行数上限 ${nf(+t.p.maxRows)}`);
      if (st === 'failed') L.push(`${at(o.start + dur)} 批失败：${run.err}`);
      if (st === 'verify') L.push(`${at(o.start + dur)} 备份校验不一致，该批未删除源数据，运行已停止：${run.err}`);
      if (st === 'killed') L.push(`${at(o.start + dur)} 收到终止请求（${run.by}），当前批做完后结束，断点 ${run.pkName} = ${run.pkTo}`);
      if (st === 'interrupted') L.push(`${at(o.start + dur)} ${run.note}`);
      if (st === 'success') L.push(`${at(o.start + dur - 500)} 没有更多符合条件的行`);
      const fb = run.backups.find(b => b.kind === 'file');
      if (fb && st !== 'failed' && st !== 'verify') L.push(`${at(o.start + dur - 200)} 文件备份完成：${fb.files.length} 个文件，${nf(fb.rows)} 行，清单 ${fb.path}/manifest.json`);
    }
    L.push(`${at(o.start + dur)} 运行结束：${RES_KEY[st] || st}，扫描 ${nf(rows)} 行，备份 ${nf(run.backed)} 行，删除 ${nf(run.deleted)} 行，${nf(run.batches)} 批`);
  }
  RUNS.push(run); return run;
}
/* 任务的累计 / 上次运行 / 断点：成功跑完的“归档并删除 / 仅清理”断点清空（下次从头，才能捡到后来才过期的小主键行），“仅备份”保留断点；窗口 / 负载 / 上限 / 失败 / 终止结束时保留 */
function refreshTaskStats(t) {
  const rs = RUNS.filter(r => r.task === t.id && (r.trg === 'sched' || r.trg === 'manual') && !['running', 'paused', 'skipped'].includes(r.status)).sort((a, b) => a.start - b.start);
  t.cum = {rows: rs.reduce((a, r) => a + (t.mode === 'backup' ? r.backed : r.deleted), 0), runs: rs.length};
  const last = rs[rs.length - 1]; t.last = last ? last.id : 0;
  let ck = [];
  rs.forEach(r => { if (r.start < (t.ckFrom || 0)) return; if (r.status === 'success' && t.mode !== 'backup') ck = []; else if (r.scanned > 0 && r.status !== 'schema') ck = [String(r.pkTo)]; });
  t.ck = ck;
}
(function seedRuns() {
  const T = id => taskOf(id), at = (d, hm_) => parseT(dayOffset(d) + ' ' + hm_ + ':00');
  const specs = [
    [1, -14, '14:00', 'dry', 'success', null, 0.1, {by: 'rd_zhao'}],
    [1, -10, '02:00', 'sched', 'limit', 5000000, 148], [1, -9, '02:00', 'sched', 'limit', 5000000, 151], [1, -8, '02:00', 'sched', 'success', 1862304, 66], [1, -7, '02:00', 'sched', 'success', 48120, 6],
    [1, -6, '02:00', 'sched', 'load', 31000, 19, {thr: 58}],
    [1, -5, '02:00', 'sched', 'verify', 20480, 5, {autoPause: true, err: '备份端同主键内容不一致：目标端按主键只查到 19,480 行，读取 20,480 行（少 1,000 行）；该批未删除源数据'}],
    [1, -4, '02:00', 'sched', 'failed', 77000, 11, {err: 'ERROR 2013 (HY000): Lost connection to MySQL server during query（目标 order_archive 所在实例切换主库，重试 3 次仍失败）；失败批已回滚，源数据未删除'}],
    [1, -3, '02:00', 'sched', 'success', 49650, 6], [1, -3, '15:20', 'manual', 'success', 3200, 2, {by: 'dba_li'}], [1, -2, '02:00', 'sched', 'success', 51203, 6], [1, -1, '02:00', 'sched', 'success', 50388, 6],
    [2, -24, '01:30', 'sched', 'limit', 5000000, 262], [2, -17, '01:30', 'sched', 'limit', 5000000, 258], [2, -10, '01:30', 'sched', 'window', 3810000, 270], [2, -3, '01:30', 'sched', 'limit', 5000000, 255],
    [2, -1, '16:40', 'manual', 'killed', 212000, 14, {by: 'dba_li'}],
    [3, -6, '03:30', 'sched', 'skipped', 0, 0, {note: '同一实例同时最多 2 个归档运行（安全策略），当前已有 2 个在跑，请稍后再试'}],
    [3, -5, '03:30', 'sched', 'interrupted', 120000, 8, {note: '执行进程异常退出或心跳超时，运行被标记为中断；断点以最后一批为准，下次接着跑'}],
    [3, -4, '03:30', 'sched', 'success', 210000, 12], [3, -3, '03:30', 'sched', 'success', 198000, 11], [3, -2, '03:30', 'sched', 'success', 205000, 12], [3, -1, '03:30', 'sched', 'success', 201300, 12],
    [5, -36, '02:00', 'sched', 'success', 612000, 52], [5, -6, '02:00', 'sched', 'failed', 24000, 9, {autoPause: true, err: 'ERROR 1205 (HY000): Lock wait timeout exceeded; try restarting transaction（连续 3 批失败，从库 mysql-order-prod-s1 复制延迟 48 s）；已自动暂停任务'}],
    [7, -16, '04:00', 'sched', 'skipped', 0, 0, {note: '备份存储「本地归档盘（归档服务器）」当前不可用（最近一次测试失败：测试失败：写入探测对象失败：permission denied 或目录不存在），不能运行'}],
    [7, -9, '04:00', 'sched', 'success', 128000, 18], [7, -2, '04:00', 'sched', 'schema', 0, 0.02, {autoPause: true, err: '源表结构指纹与审批 / 试运行时不一致（列名、类型或主键有变化），需要重新试运行'}],
    [4, 0, '09:40', 'dry', 'success', null, 0.1, {by: 'rd_zhao'}], [8, 0, '10:30', 'dry', 'success', null, 0.1, {by: 'rd_xu'}], [6, -5, '11:10', 'dry', 'failed', null, 0.1, {by: 'rd_xu'}], [9, -1, '16:20', 'dry', 'success', null, 0.1, {by: 'rd_zhao'}],
  ];
  specs.sort((a, b) => at(a[1], a[2]) - at(b[1], b[2]));
  specs.forEach(([tid, d, hh, trg, st, rows, durMin, o]) => {
    const t = T(tid), start = at(d, hh);
    o = Object.assign({}, o || {});
    if (trg === 'dry') {      // 种子的试运行：假装当时实例是连通的
      const inst = instOf(t.inst), s0 = inst.status; inst.status = 'ok'; storeProbe.assumeOk = true;
      const dc = dryCompute(t, ''); inst.status = s0; storeProbe.assumeOk = false;
      rows = dc ? dc.est : 0; st = dc && dc.bad ? 'failed' : 'success';
      if (dc) { o.note = `预估命中 ${nf(dc.est)} 行（占表 ${dc.pct.toFixed(1)}%），${dc.warn} 个警告${dc.bad ? '，存在红色检查项' : ''}`; const e0 = dc.items.find(i => i.lv === 'error' && i.k !== 'large_batch'); if (e0) o.err = (e0.title + '：' + e0.msg).slice(0, 400); }
    }
    mkRun(t, Object.assign({start, trg, st, rows, durMin}, o));
  });
  /* 恢复：两条（恢复本身也是一条运行记录） */
  const src1 = RUNS.find(r => r.task === 1 && r.trg === 'manual'), src2 = RUNS.find(r => r.task === 1 && r.start === at(-7, '02:00'));
  [[src1, -2, '10:05', 3200, 3200, 'dba_li'], [src2, -6, '09:30', 1000, 1000, 'dba_li']].forEach(([s, d, hh, rows, back, by]) => {
    const rr = mkRun(T(1), {start: at(d, hh), trg: 'restore', st: 'success', rows, durMin: 1, by, of: s.id, back, pkFrom: s.pkFrom, pkTo: Math.min(s.pkTo, s.pkFrom + rows * 2),
      note: `按运行 #${s.id} 的主键范围 INSERT IGNORE 回灌（只补缺、不覆盖）：备份中 ${rows} 行，补回 ${back} 行`}); s.restored.push(rr.id);
  });
  RUNS.sort((a, b) => a.start - b.start);
  TASKS.forEach(refreshTaskStats);
})();

/* ============================================================================
   审批、可见性、导出（侧栏角标 / 首页待办 / 其他页面可读）
   ========================================================================== */
const taskVisible = t => ME.super || can('query.admin_run') || ME.rgs.includes(t.rg);
const visibleTasks = () => TASKS.filter(taskVisible);
const visibleRuns = () => RUNS.filter(r => { const t = taskOf(r.task); return t ? taskVisible(t) : (r.by === ME.login || ME.super || can('query.admin_run')); });
function canApprove(t, u) {
  u = u || ME;
  if (t.status !== 'pending' || !t.appr || u.login === t.appr.applicant) return false;
  if (u.super) return true;
  return u.status === 'active' && u.groups.includes(t.appr.levels[t.appr.level]) && u.rgs.includes(t.rg);
}
window.ARCHIVE_TASKS = TASKS;
window.archivePendingList = u => TASKS.filter(t => canApprove(t, u));
window.archivePendingFor = u => window.archivePendingList(u).length;
const badge = () => window.archivePendingFor(ME);
NAV.archive.badge = badge;                                   // NAV 与 NAV_TREE 是同一批对象，设置一次即可；下面的循环只是保险
NAV_TREE.forEach(g => g.items.forEach(m => { if (m.id === 'archive') m.badge = badge; }));
const pendingLvText = t => { const a = t.appr; if (!a) return ''; const gid = a.levels[a.level], us = levelUsers(t.rg, gid).filter(u => u.login !== a.applicant); return `第 ${a.level + 1}/${a.levels.length} 级 · 待${lvName(gid)}（${us.length ? us.map(u => u.login).join('、') : '本级无其他可审批人，仅超级用户可审'}）`; };

/* ============================================================================
   运行模拟引擎：每 400 ms 一个 tick，每个 tick 约等于 1 个“负载自检 + 若干批”；时间被压缩
   ========================================================================== */
const TICK_MS = 400;
let TIMER = 0;
const UI = {tick() {}, done() {}};
const hmsOf = ms => tstr(ms).slice(11);
const runningOf = t => [...SIMS.values()].find(s => s.task.id === t.id);
const ckOf = t => (t.ck && t.ck.length) ? +t.ck[0] : FIRST_PK(t);      // 断点：没有断点（首次 / 上次成功跑完）就从头
function ensureTimer() { if (!TIMER && [...SIMS.values()].some(s => !s.paused)) TIMER = setInterval(tick, TICK_MS); }
function stopTimer() { if (TIMER) { clearInterval(TIMER); TIMER = 0; } }
function tick() {
  if (!$('#arView') || !/^#archive(\/|$)/.test(location.hash) || !canView('archive')) { stopTimer(); return; }      // 离开页面 / 切换成无权限身份：自己清理定时器
  SIMS.forEach(s => { if (!s.paused) stepSim(s); });
  UI.tick();
  if (![...SIMS.values()].some(s => !s.paused)) stopTimer();
}
function startRun(t, o) {
  const start = simMs(), tb = findTable(t.inst, t.db, t.tbl), inst = instOf(t.inst), drift = t.fp !== fpOf(t.inst, t.db, t.tbl), badInst = inst.status === 'bad';
  /* 结构指纹不一致 / 连不上源库：和真实实现一样，运行会先启动，很快以 structure_changed（任务置为“已暂停（结构变更）”）或 failed（连接失败）结束，而不是在点击时被直接拒绝 */
  const dc = drift || badInst ? null : dryCompute(t, ''), total = dc ? dc.eff : 0;
  const run = {id: ++RUN_ID, task: t.id, name: t.name, inst: t.inst, tbl: t.db + '.' + t.tbl, trg: o.trg || 'manual', start, end: start, status: 'running', by: o.by || ME.login, scanned: 0, backed: 0, deleted: 0, batches: 0, dur: 0,
    peakThr: 0, peakLag: 0, throttles: 0, series: [], bsize: [], events: [], verify: [], backups: [], err: '', log: [], restoreOf: 0, restored: [], pkFrom: ckOf(t), pkTo: 0, pkName: tb ? pkCols(tb)[0] : 'id', note: '', autoPause: false};
  RUNS.push(run);
  const ticks = 70, bundle = Math.max(1, Math.ceil(total / (+t.p.batch) / ticks));
  const s = {run, task: t, total, est: dc ? dc.est : 0, done: 0, batch: +t.p.batch, sleep: +t.p.sleep, vtime: 0, tick: 0, paused: false, stop: false, hold: false, spike: 0, lagSpike: 0, threads: 9, lag: 0.4, ok: 0, wait: null,
    rnd: rng(hs('sim' + run.id + t.id)), autoAt: Math.max(3, Math.round(ticks * 0.3)), autoDone: !!o.noAuto, ignoreWin: !!o.ignoreWin, hasSlave: slavesOf(inst).length > 0, bundle, speed: 0, ticks, series: [], bsize: [], fail: drift ? 'schema' : badInst ? 'conn' : ''};
  SIMS.set(run.id, s); t.running = run.id;
  run.log.push(`${hmsOf(start)} 运行开始（${MODES[t.mode][0]}），条件：${whereOf(t)}（${TRG[run.trg]}，操作人 ${run.by}${o.ignoreWin ? '，已忽略执行窗口' : ''}）`,
    `${hmsOf(start)} 前置检查通过：状态已启用、无未结束的 SQL 上线工单、实例并发 ${SIMS.size}/${POLICY.maxConc}、备份存储可用；会话 lock_wait_timeout=3s、innodb_lock_wait_timeout=3s`);
  if (s.fail) { ensureTimer(); return s; }
  if (!total) { run.log.push(`${hmsOf(start)} 没有更多符合条件的行`); finishSim(s, 'success'); return s; }
  run.log.push(`${hmsOf(start)} 从断点 ${run.pkName} > ${run.pkFrom} 开始，批大小 ${s.batch}，批间休眠 ${s.sleep} ms；本次最多处理 ${nf(total)} 行${total < s.est ? `（预估命中 ${nf(s.est)} 行，受单次上限限制）` : ''}`);
  ensureTimer();
  return s;
}
function stepSim(s) {
  const t = s.task, run = s.run, p = t.p, r = s.rnd, simNow = run.start + s.vtime;
  s.tick++;
  if (s.fail) { s.vtime += 600; if (s.tick >= 2) finishSim(s, s.fail === 'schema' ? 'schema' : 'failed'); return; }
  if (s.stop) return finishSim(s, 'killed');
  if (!s.ignoreWin && !inWin(minOf(simNow), t.win)) return finishSim(s, 'window');
  if (!s.autoDone && s.tick >= s.autoAt) { s.autoDone = true; s.spike = 3; }          // 演示：约 30% 进度处自动出现一次负载尖峰
  const heavy = s.spike > 0 || s.hold; if (s.spike > 0) s.spike--;
  const thr = Math.round(8 + 4 * Math.sin(s.tick / 4) + r() * 3 + Math.min(9, s.batch / 350) + (heavy ? 40 + r() * 10 : 0));
  const lagSp = s.lagSpike > 0; if (lagSp) s.lagSpike--;
  s.threads = thr; s.lag = s.hasSlave ? Math.max(0, Math.round((s.lag * 0.5 + r() * 0.8 + (lagSp ? 8 : 0)) * 10) / 10) : 0;
  run.peakThr = Math.max(run.peakThr, thr); run.peakLag = Math.max(run.peakLag, s.lag);
  const overT = thr > +p.threads, overL = s.hasSlave && s.lag > +p.lag;
  if (overT || overL) {
    const why = overT ? `Threads_running=${thr} 超过阈值 ${p.threads}` : `复制延迟 ${s.lag} 秒超过阈值 ${p.lag} 秒`;
    if (!s.wait) {
      s.wait = {at: tstr(simNow), kind: overT ? 'threads' : 'lag', val: overT ? thr : s.lag, thr: overT ? +p.threads : +p.lag, waited: 0, end: 'wait', batchTo: 0, why};
      run.events.push(s.wait); run.throttles++;
      run.log.push(`${hmsOf(simNow)} 负载超限，暂停等待：${why}（每 30 秒复查）`);
    }
    s.wait.waited += 30; s.vtime += 30000; s.speed = 0; s.series.push(0); s.bsize.push(s.batch);
    if (s.wait.waited >= +p.maxWait * 60) { s.wait.end = 'abort'; run.log.push(`${hmsOf(run.start + s.vtime)} 负载持续超限超过 ${p.maxWait * 60} 秒，本次运行中断：${s.wait.why}（不算失败）`); return finishSim(s, 'load'); }
    return;
  }
  if (s.wait) { s.wait.end = 'resume'; run.log.push(`${hmsOf(simNow)} 负载恢复正常，已等待 ${s.wait.waited} 秒，继续运行`); s.wait = null; }
  const bms = Math.round(70 + s.batch * 0.2 * (1 + thr / 45) + r() * 50);
  if (bms > +p.targetMs && s.batch > 100) {
    const nb = Math.max(100, Math.floor(s.batch / 2)), ns = Math.min(5000, s.sleep * 2); run.log.push(`${hmsOf(simNow)} 批耗时 ${bms} ms > 目标 ${p.targetMs} ms：批大小 ${s.batch} → ${nb}，休眠 ${s.sleep} → ${ns} ms`);
    if (s.wait == null && run.events.length) run.events[run.events.length - 1].batchTo = run.events[run.events.length - 1].batchTo || nb;
    s.batch = nb; s.sleep = ns; s.ok = 0;
  }
  const rows = Math.min(s.total - s.done, s.bundle * s.batch), nbat = Math.max(1, Math.ceil(rows / s.batch));
  if (bms <= +p.targetMs) { s.ok += nbat; if (s.ok >= 5 && (s.batch < +p.batch || s.sleep > +p.sleep)) { const nb = Math.min(+p.batch, Math.ceil(s.batch * 1.5)), ns = Math.max(+p.sleep, Math.floor(s.sleep / 2)); run.log.push(`${hmsOf(simNow)} 连续 5 批正常，逐步恢复：批大小 ${s.batch} → ${nb}，休眠 ${s.sleep} → ${ns} ms`); s.batch = nb; s.sleep = ns; s.ok = 0; } }
  s.done += rows; s.vtime += nbat * (bms + s.sleep); run.batches += nbat;
  s.speed = Math.round(rows / (nbat * (bms + s.sleep) / 1000)); s.series.push(s.speed); s.bsize.push(s.batch);
  run.scanned = s.done; run.backed = t.mode === 'cleanup' ? 0 : s.done; run.deleted = t.mode === 'backup' ? 0 : s.done; run.pkTo = run.pkFrom + Math.round(s.done * 1.4);
  if (s.tick % 12 === 0) run.log.push(`${hmsOf(run.start + s.vtime)} 第 ${nf(run.batches)} 批：累计 ${nf(s.done)} / ${nf(s.total)} 行，断点 ${run.pkName} = ${run.pkTo}，批大小 ${s.batch}`);
  if (s.done >= s.total) finishSim(s, s.total < s.est ? 'limit' : 'success');
}
function finishSim(s, st) {
  const t = s.task, run = s.run, rows = s.done, inst = instOf(t.inst), failed0 = st === 'schema' || (st === 'failed' && s.fail);
  SIMS.delete(run.id); t.running = 0;
  run.status = st; run.dur = Math.max(s.vtime, 1000); run.end = run.start + run.dur; run.series = s.series; run.bsize = s.bsize;
  run.scanned = rows; run.backed = t.mode === 'cleanup' ? 0 : rows; run.deleted = t.mode === 'backup' ? 0 : rows; run.pkTo = rows ? run.pkFrom + Math.round(rows * 1.4) : 0; if (!rows) run.pkFrom = 0;
  if (!run.peakThr && !failed0) run.peakThr = s.threads;
  const at = hmsOf(run.end);
  if (st === 'schema') {
    run.err = '源表结构指纹与审批 / 试运行时不一致（列名、类型或主键有变化），需要重新试运行'; run.autoPause = true;
    run.log.push(`${at} 源表结构与审批 / 试运行时不一致，已停止：${t.fp.slice(0, 8)} → ${hex('drift' + t.id + run.id, 8)}`);
    if (t.status === 'enabled') {      // 服务层据此把任务置为“已暂停（结构变更）”并通知
      t.status = 'paused'; t.pauseKind = 'schema'; t.pauseAt = nowS(); t.pauseReason = '源表结构指纹与审批时不一致（列名 / 类型 / 主键有变化），已暂停。需要重新试运行并重新提交审批。 详情：' + run.err;
      t.hist.push({at: nowS(), user: 'system', text: '结构指纹变化，任务已暂停并通知'});
    }
  } else if (failed0) {
    run.err = `连接源 / 目标失败：dial tcp ${inst.host}:${inst.port}: i/o timeout`; run.log.push(`${at} ${run.err}`);       // 瞬时的连接错误：失败但不自动暂停
  } else {
    fillBackups(run, t, st, run.start); fillVerify(run, t, st);
    if (st === 'window') run.log.push(`${at} 已超出执行窗口 ${t.win.from}–${t.win.to}，优雅停止，下次从断点继续`);
    if (st === 'limit') run.log.push(`${at} 已达单次运行行数上限 ${nf(+t.p.maxRows)}`);
    if (st === 'success') run.log.push(`${at} 没有更多符合条件的行`);
    if (st === 'killed') run.log.push(`${at} 收到终止请求（${s.killBy || run.by}），当前批做完后结束，断点 ${run.pkName} = ${run.pkTo}`);
    const fb = run.backups.find(b => b.kind === 'file');
    if (fb) run.log.push(`${at} 文件备份完成：${fb.files.length} 个文件，${nf(fb.rows)} 行，清单 ${fb.path}/manifest.json`);
  }
  run.log.push(`${at} 运行结束：${RES_KEY[st]}，扫描 ${nf(rows)} 行，备份 ${nf(run.backed)} 行，删除 ${nf(run.deleted)} 行，${nf(run.batches)} 批`);
  refreshTaskStats(t);
  AUDIT.unshift({at: now(), user: 'system', ip: '—', module: '数据归档', code: 'archive.view', act: `数据归档-运行结束：运行 #${run.id}「${t.name}」(${run.trg === 'manual' ? 'manual' : 'schedule'})结束：${RES_KEY[st]}，读取 ${nf(rows)} 行，备份 ${nf(run.backed)} 行，删除 ${nf(run.deleted)} 行`, res: st === 'success' || st === 'limit' ? '成功' : '警告', extra: inst.name});
  UI.done(s);
}
const simPause = (s, on) => { s.paused = on; s.run.log.push(`${hmsOf(s.run.start + s.vtime)} ${on ? `运行已暂停（保留断点，可恢复；操作人 ${ME.login}）` : `运行已恢复（操作人 ${ME.login}）`}`); if (!on) ensureTimer(); };

/* 立即执行 / 调度前置检查（runner.go precheck + startRun；调度命中则记一条“已跳过”运行，立即执行返回 409 skipped）。结构指纹不一致不在这里判：运行启动后由引擎判定 */
function startBlock(t, ignoreWin) {
  if (t.status !== 'enabled') return `任务当前状态为「${ST_T[t.status][0]}」，只有已启用的任务可以运行`;
  if (+t.retain < POLICY.minRetain) return `保留天数 ${t.retain} 天低于安全策略的最小保留天数 ${POLICY.minRetain} 天，不能运行；请编辑任务后重新审批`;
  if (t.mode === 'cleanup' && !POLICY.allowNoBackup) return '安全策略当前不允许“不备份清理”，不能运行；请联系管理员';
  if (protectedHit(t.db, t.tbl)) return `${t.db}.${t.tbl} 现在属于受保护库表（安全策略），不能运行`;
  if (t.bk.file.on) {
    const s = storeOf(t.bk.file.store);
    if (!s) return '备份存储已被删除，先备份后删除的前提不成立，不能运行';
    if (!s.enabled) return `备份存储「${s.name}」已停用，先备份后删除的前提不成立，不能运行`;
    if (stStatus(s) === 'err') return `备份存储「${s.name}」当前不可用（最近一次测试失败：${s.lastTest ? s.lastTest.msg : '—'}），不能运行`;
  }
  if (!ignoreWin && !inWin(minOf(simMs()), t.win)) return `当前时间 ${tstr(simMs()).slice(11, 16)} 不在执行窗口 ${t.win.from}–${t.win.to} 内；需要立即运行请勾选“忽略执行窗口”`;
  if (instOf(t.inst).enabled === false) return '来源实例不存在或已停用';
  const wf = (window.WORKFLOWS || []).filter(w => w.inst === t.inst && w.db === t.db && WF_OPEN.includes(w.status) && new RegExp('\\b' + t.tbl + '\\b', 'i').test(w.sql || ''));
  if (wf.length) return `源表有未结束的 SQL 上线工单 #${wf[0].id}「${wf[0].name}」（${wf[0].status}），本次跳过；工单结束后再试`;
  if (runningOf(t)) return '该任务已有一次运行在进行中（上次运行还没有结束）';
  const conc = [...SIMS.values()].filter(x => x.task.inst === t.inst).length;
  if (conc >= POLICY.maxConc) return `同一实例同时最多 ${POLICY.maxConc} 个归档运行（安全策略），当前已有 ${conc} 个在跑，请稍后再试`;
  return '';
}
const nextRunOf = t => t.status === 'enabled' ? (schedTimes(t.sched, simMs(), 1)[0] || 0) : 0;

/* ============================================================================
   页面：归档任务 / 运行记录 / 备份存储 / 安全策略
   ========================================================================== */
const AR = {tab: 'tasks', f: {st: '', mode: '', inst: '', q: '', mine: false}, rf: {task: '', trg: '', st: '', q: ''}, dr: null};
let W = null;           // 向导状态

const stTag = t => `<span class="tag ${ST_T[t.status][1]}">${ST_T[t.status][0]}</span>`;
const rsTag = r => `<span class="tag ${ST_R[r.status][1]}">${ST_R[r.status][0]}</span>`;
const modeTag = m => `<span class="tag ${MODES[m][1]}" title="${esc(MODES[m][2])}">${MODES[m][0]}</span>`;
const storeShort = s => s ? s.name.replace(/（.*?）/, '') : '（已删除）';
function bkTags(t) {
  if (t.mode === 'cleanup') return '<span class="tag warn" title="不备份，删除后无法恢复">不备份</span>';
  const o = [];
  if (t.bk.db.on) { const d = dbTarget(t); o.push(`<span class="tag info" title="${esc(instOf(d.inst).name)}">库表 ${esc(d.db)}.${esc(t.bk.db.tpl)}</span>`); }
  if (t.bk.file.on) { const s = storeOf(t.bk.file.store), bad = !s || ['err', 'off'].includes(stStatus(s)); o.push(`<span class="tag ${bad ? 'bad' : 'brand'}" title="${s ? esc(s.name + (bad ? '：备份存储当前不可用' : '')) : '存储已删除或无权查看'}">文件 ${esc(storeShort(s))}${t.bk.file.gzip ? ' · gzip' : ''}${t.bk.file.enc ? ' · 加密' : ''}</span>`); }
  return o.join(' ') || '<span class="muted">—</span>';
}
const lastRunCell = t => { const r = runOf(t.last); return r ? `${rsTag(r)}<div class="small muted nowrap">${fmtAt(r.start).slice(5)}</div>` : '<span class="muted">—</span>'; };
const nextCell = t => { const n = nextRunOf(t); return n ? `<span class="nowrap">${fmtAt(n).slice(5)}</span>` : '<span class="muted">—</span>'; };
const clip = (s, n) => String(s).length > n ? String(s).slice(0, n) + '…' : String(s);

/* ---------- 运行中的实时面板（只含进度 / 仪表，不含按钮，避免每个 tick 重绘时点不中） ---------- */
function liveHtml(t, full) {
  const s = runningOf(t); if (!s) return '';
  const pct = s.total ? Math.min(100, s.done / s.total * 100) : 100, left = s.total - s.done, eta = s.speed > 0 ? fmtDur(left / s.speed * 1000) : '—';
  const state = s.paused ? ['已暂停', 'warn'] : s.wait ? [`等待负载 ${s.wait.waited}s`, 'bad'] : ['运行中', 'brand'];
  const gauge = (label, v, th, unit) => `<div class="ar-gauge"><span>${label}</span><div class="ar-bar"><b class="${v > th ? 'bad' : v > th * 0.75 ? 'warn' : ''}" style="width:${Math.min(100, v / (th * 1.5) * 100)}%"></b><i></i></div><b>${v}${unit} / 阈值 ${th}${unit}</b></div>`;
  const head = `<div class="ar-prog"><b style="width:${pct.toFixed(1)}%"></b></div><div class="small"><span class="tag ${state[1]}">${state[0]}</span> ${pct.toFixed(1)}% · ${nf(s.done)} / ${nf(s.total)} 行 · ${nf(s.speed)} 行/s · 预计剩余 ${eta}</div>`;
  if (!full) return head + `<div class="small muted">批 ${nf(s.batch)} · 休眠 ${t.p.sleep} ms · 限流 ${s.run.throttles} 次</div>`;
  return head + `<div class="ar-gauges">${gauge('Threads_running', s.threads, +t.p.threads, '')}${gauge('复制延迟', s.lag, +t.p.lag, 's')}</div>
    <div class="small muted">当前批大小 <b>${nf(s.batch)}</b>（设定 ${nf(+t.p.batch)}）· 批间休眠 设定 <b>${t.p.sleep} ms</b> · 已用时 ${fmtDur(s.vtime)}（时间已压缩）· 本次限流 <b>${s.run.throttles}</b> 次 · 峰值 Threads_running ${s.run.peakThr}</div>
    ${s.wait ? `<div class="notice bad" style="margin-top:6px"><span>⏸</span><div>负载超限，已暂停等待：${esc(s.wait.why)}，已等 ${s.wait.waited}s（最长 ${t.p.maxWait * 60}s）。</div></div>` : ''}`;
}
const liveBox = (t, full) => runningOf(t) ? `<div data-live="${t.id}" data-mode="${full ? 'full' : 'mini'}" class="ar-live">${liveHtml(t, full)}</div>` : '';

/* ---------- 归档任务 ---------- */
function taskRows() {
  const f = AR.f, q = f.q.trim().toLowerCase();
  return visibleTasks().filter(t => (!f.st || t.status === f.st) && (!f.mode || t.mode === f.mode) && (!f.inst || t.inst == f.inst) && (!f.mine || canApprove(t)) && (!q || `${t.name} ${t.db}.${t.tbl} ${instOf(t.inst).name}`.toLowerCase().includes(q)))
    .sort((a, b) => (canApprove(b) - canApprove(a)) || (b.running ? 1 : 0) - (a.running ? 1 : 0) || a.id - b.id);
}
function kpiHtml() {
  const v = visibleTasks(), n = s => v.filter(t => t.status === s).length, run = v.filter(t => runningOf(t)).length, mine = v.filter(t => canApprove(t)).length;
  const k = (l, val, d, cls) => `<div class="card kpi"><div class="l">${l}</div><div class="v">${val}</div>${d ? `<div class="d ${cls || 'flat'}">${d}</div>` : ''}</div>`;
  return `<div class="ar-kpis">${k('任务总数', v.length)}${k('已启用', n('enabled'), run ? `其中 ${run} 个正在运行` : '', run ? 'down' : 'flat')}${k('待审批', n('pending'), mine ? `其中 ${mine} 个等我审批` : '', mine ? 'up' : 'flat')}${k('已暂停', n('paused'))}${k('累计归档行数', fmt(v.reduce((a, t) => a + t.cum.rows, 0)))}</div>`;
}
function taskTable() {
  const rows = taskRows();
  return tbl(['名称', '来源表', '模式', '备份去向', '调度', '状态', '上次运行', '下次运行', '累计归档行数', '操作'], rows, {cls: 'ar-cards ar-tb-tasks', cells: t => {
    const inst = instOf(t.inst), ap = canApprove(t);
    return [`<a data-on="arTask" data-id="${t.id}"><b>${esc(t.name)}</b></a><div class="small muted">${esc(rgName(t.rg))} · ${esc(t.creator || '—')}</div>`,
      `<div class="small muted nowrap">${esc(inst.name)}</div><code>${esc(t.db)}.${esc(t.tbl)}</code>`, modeTag(t.mode), bkTags(t),
      `<span class="nowrap">${esc(schedText(t.sched))}</span><div class="small muted nowrap">窗口 ${t.win.from}–${t.win.to}</div>`,
      `${stTag(t)}${t.status === 'pending' ? `<div class="small muted" style="max-width:220px">${esc(pendingLvText(t))}</div>` : ''}${t.status === 'paused' ? `<div class="small muted" style="max-width:220px" title="${esc(t.pauseReason)}">${PAUSE_T[t.pauseKind] || '已暂停'}：${esc(clip(t.pauseReason || '', 30))}</div>` : ''}${liveBox(t, false)}`,
      lastRunCell(t), nextCell(t), `<span class="nowrap">${nf(t.cum.rows)}</span>${t.cum.runs ? `<div class="small muted">${t.cum.runs} 次运行</div>` : ''}`,
      `<div class="ops"><button class="btn sm" data-on="arTask" data-id="${t.id}">详情</button>${ap ? `<button class="btn sm primary" data-on="arApprove" data-id="${t.id}">通过</button><button class="btn sm danger" data-on="arReject" data-id="${t.id}">驳回</button>`
        : t.status === 'enabled' && !t.running ? B('archive.run', '立即执行', {cls: 'sm', data: {id: t.id}}) + B('archive.pause', '暂停', {cls: 'sm', data: {op: 'pause', id: t.id}})
        : t.running ? B('archive.pause', runningOf(t) && runningOf(t).paused ? '继续' : '暂停', {cls: 'sm', data: {op: 'pause', id: t.id}}) + B('archive.stop', '终止', {cls: 'sm danger', data: {id: t.id}})
        : t.status === 'paused' && t.pauseKind !== 'schema' ? B('archive.pause', '恢复', {cls: 'sm', data: {op: 'resume', id: t.id}}) : ''}</div>`];
  }});
}
function tasksHtml() {
  const f = AR.f, insts = [...new Set(visibleTasks().map(t => t.inst))].map(instOf), mineN = visibleTasks().filter(t => canApprove(t)).length;
  return `<div id="arKpi">${kpiHtml()}</div>
    <div class="toolbar">
      <select data-onchange="arF" data-k="st" aria-label="状态"><option value="">状态：全部</option>${Object.keys(ST_T).filter(k => k !== 'draft').map(k => opt(k, ST_T[k][0], f.st)).join('')}</select>
      <select data-onchange="arF" data-k="mode" aria-label="模式"><option value="">模式：全部</option>${Object.keys(MODES).map(k => opt(k, MODES[k][0], f.mode)).join('')}</select>
      <select data-onchange="arF" data-k="inst" aria-label="实例"><option value="">实例：全部</option>${insts.map(i => opt(i.id, i.name, f.inst)).join('')}</select>
      <input type="search" placeholder="搜索 任务名 / 库表 / 实例" data-oninput="arF" data-k="q" value="${esc(f.q)}" style="min-width:220px" aria-label="搜索">
      <label class="chk"><input type="checkbox" data-onchange="arF" data-k="mine" ${f.mine ? 'checked' : ''}> 待我审批${mineN ? `（${mineN}）` : ''}</label>
      <span class="sp"></span>${B('archive.apply', '＋ 新建归档任务', {cls: 'primary', data: {op: 'new'}})}</div>
    <div id="arList">${taskTable()}</div>`;
}

/* ---------- 任务详情抽屉 ---------- */
function apprTimeline(t) {
  const a = t.appr; if (!a) return empty('尚未提交审批');
  const items = [{cls: 'ok', h: `${uLabel(a.applicant)} 提交申请`, m: esc(a.at) + (t.mode === 'cleanup' ? ` · 不备份理由：${esc(t.reason)}` : '')}];
  a.levels.forEach((gid, i) => {
    const st = a.steps.find(x => x.lv === i);
    if (st) items.push({cls: st.action === 'approve' ? 'ok' : 'bad', h: `第 ${i + 1} 级 ${esc(lvName(gid))}：${uLabel(st.user)} ${st.action === 'approve' ? '通过' : '驳回'}`, m: esc(st.at) + (st.note ? ' · ' + esc(st.note) : '')});
    else if (t.status === 'pending' && i === a.level) items.push({cls: 'cur', h: `第 ${i + 1} 级 ${esc(lvName(gid))}：待审批`, m: esc(pendingLvText(t).replace(/^第 \d+\/\d+ 级 · /, ''))});
    else items.push({cls: 'skip', h: `第 ${i + 1} 级 ${esc(lvName(gid))}`, m: a.reject || a.aborted ? '流程已终止' : '等待上一级通过'});
  });
  if (a.reject) items.push({cls: 'bad', h: `已驳回（${uLabel(a.reject.user)}）`, m: esc(a.reject.reason)});
  if (a.aborted) items.push({cls: 'bad', h: '审批已作废', m: '任务被停用，审批流随之作废'});
  if (t.status !== 'pending' && !a.reject && !a.aborted && a.level >= a.levels.length) items.push({cls: 'ok', h: '审批通过，任务已启用', m: ''});
  return tline(items);
}
/* 备份去向的明细行（库表 / 文件的模板、格式、压缩 / 加密、单文件上限） */
const destLines = t => {
  const o = [];
  if (t.mode !== 'cleanup') {
    if (t.bk.db.on) o.push(`库表：实例#${t.bk.db.inst || t.inst} · ${t.bk.db.db}.${t.bk.db.tpl}`);
    if (t.bk.file.on) { const f = t.bk.file; o.push(`文件：${f.fmt.toUpperCase()}${f.gzip ? ' · gzip' : ''}${f.enc ? ' · AES-256-GCM 加密' : ''} · 路径 ${f.path} · 单文件 ≤ ${f.partMB || 256} MB`); }
  }
  return o.map(x => `<div class="small muted">${esc(x)}</div>`).join('');
};
function taskDrawerHtml(t) {
  const inst = instOf(t.inst), q = sqlPreview(t, {ck: t.ck}), s = runningOf(t), rs = RUNS.filter(r => r.task === t.id).sort((a, b) => b.start - a.start).slice(0, 6), ap = canApprove(t);
  const pr = t.p;
  const btns = [ap ? `<button class="btn primary" data-on="arApprove" data-id="${t.id}">审批通过</button><button class="btn danger" data-on="arReject" data-id="${t.id}">驳回</button>` : '',
    B('archive.apply', '编辑 / 重新提交', {data: {op: 'edit', id: t.id}}), B('archive.dryrun', '试运行', {data: {op: 'task', id: t.id}}),
    t.status === 'enabled' && !s ? B('archive.run', '立即执行', {data: {id: t.id}}) + B('archive.pause', '暂停任务', {data: {op: 'pause', id: t.id}}) : '',
    s ? B('archive.pause', s.paused ? '继续运行' : '暂停运行', {data: {op: 'pause', id: t.id}}) + B('archive.stop', '终止运行', {cls: 'danger', data: {id: t.id}}) : '',
    t.status === 'paused' && t.pauseKind !== 'schema' ? B('archive.pause', '恢复任务', {data: {op: 'resume', id: t.id}}) : '',
    B('archive.delete', ['enabled', 'paused', 'pending'].includes(t.status) ? '停用任务' : '删除任务', {cls: 'danger', data: {id: t.id}})].join('');
  return `<div class="row" style="margin-bottom:8px">${stTag(t)}${modeTag(t.mode)}<span class="small muted">${esc(rgName(t.rg))} · 申请人 ${uLabel(t.creator)} · ${esc(t.created || '')}</span></div>
    ${t.status === 'paused' ? notice(`<b>${PAUSE_T[t.pauseKind] || '已暂停'}</b>${t.pauseAt ? '（' + esc(t.pauseAt) + '）' : ''}：${esc(t.pauseReason || '—')}`, 'warn', '⏸') : ''}
    ${t.status === 'rejected' && t.appr && t.appr.reject ? notice(`<b>已驳回</b>（${uLabel(t.appr.reject.user)}，${esc(t.appr.reject.at)}）：${esc(t.appr.reject.reason)}`, 'bad', '✕') : ''}
    ${t.status === 'pending' ? notice(`审批中：${esc(pendingLvText(t))}`, 'warn', '⏳') : ''}
    ${t.mode === 'cleanup' ? notice(`<b>仅清理：不备份，删除后无法恢复。</b>理由：${esc(t.reason)}`, 'warn', '!') : ''}
    ${s ? `<h4 style="margin:10px 0 6px">运行中 <span class="small muted">模拟 · 时间已压缩</span></h4>${liveBox(t, true)}
      <div class="toolbar" style="margin-top:6px"><span class="small muted">演示：</span><button class="btn sm" data-on="arSpike" data-id="${t.id}">模拟负载升高</button><button class="btn sm" data-on="arHold" data-id="${t.id}">模拟持续高负载（触发“负载中断”）</button><button class="btn sm" data-on="arLag" data-id="${t.id}">模拟从库延迟</button><button class="btn sm" data-on="arRelease" data-id="${t.id}">解除高负载</button></div>` : ''}
    <div class="toolbar" style="margin:10px 0">${btns}</div>
    ${kvList([['实例 / 库表', `${esc(inst.name)}${inst.status === 'bad' ? ' <span class="tag bad">连接异常</span>' : ''}<br><code>${esc(t.db)}.${esc(t.tbl)}</code>`], ['归档条件', whereOf(t) ? `<code>${esc(whereOf(t))}</code>` : '—'], ['模式', modeTag(t.mode)], ['备份去向', bkTags(t) + destLines(t)],
      ['调度', `${esc(schedText(t.sched))} <span class="small muted">下次 ${nextRunOf(t) ? fmtAt(nextRunOf(t)) : '—'}</span>`], ['执行窗口', esc(winText(t.win))],
      ['稳定性参数', `批 ${nf(+pr.batch)} 行 · 休眠 ${pr.sleep} ms · Threads_running ≤ ${pr.threads} · 复制延迟 ≤ ${pr.lag} s · 磁盘剩余 ≥ ${pr.disk}% · 最长等待 ${pr.maxWait} 分钟 · 批耗时目标 ${pr.targetMs} ms · 单次上限 ${nf(+pr.maxRows)} 行 · 连续失败 ${pr.fails} 批自动暂停`],
      ['累计', `${nf(t.cum.rows)} 行 / ${t.cum.runs} 次运行 · 断点 ${pkCols(findTable(t.inst, t.db, t.tbl) || {cols: []})[0] || 'id'} = ${t.ck && t.ck.length ? esc(t.ck.join(',')) : '（从头）'}`]], '')}
    <h4 style="margin:16px 0 4px">将执行的 SQL</h4>${sqlBlocks(q, t.mode)}
    <h4 style="margin:16px 0 4px">审批进度</h4>${apprTimeline(t)}
    <h4 style="margin:16px 0 4px">最近运行 <a class="small" data-on="arRunsOf" data-id="${t.id}">查看全部</a></h4>
    ${rs.length ? tbl(['运行', '开始', '触发', '结果', '删除行数'], rs, {cells: r => [`<a data-on="arRun" data-id="${r.id}">#${r.id}</a>`, `<span class="nowrap small">${fmtAt(r.start).slice(5)}</span>`, TRG[r.trg], rsTag(r), nf(r.deleted)]}) : empty('还没有运行记录')}
    <h4 style="margin:16px 0 4px">变更历史</h4>${t.hist.length ? tline(t.hist.slice().reverse().map(h => ({cls: 'ok', h: esc(h.text), m: `${esc(h.at)} · ${esc(h.user)}`}))) : empty('暂无')}`;
}
function openTask(id) {
  const t = taskOf(id); if (!t) return;
  AR.dr = {kind: 'task', id: t.id}; openDrawer(`归档任务 · ${t.name}`, taskDrawerHtml(t), true);
}
function refreshDrawer() {
  if (!AR.dr || !$('#drawer').classList.contains('open')) return;
  if (AR.dr.kind === 'task') { const t = taskOf(AR.dr.id); if (t) $('#drBody').innerHTML = taskDrawerHtml(t); else closeDrawer(); }
  else if (AR.dr.kind === 'run') { const r = runOf(AR.dr.id); if (r) $('#drBody').innerHTML = runDrawerHtml(r); }
}
function redraw() {
  const b = $('#arBody'); if (b) b.innerHTML = AR.tab === 'runs' ? runsHtml() : AR.tab === 'stores' ? storesHtml() : AR.tab === 'policy' ? policyHtml() : tasksHtml();
  refreshDrawer(); buildNav();
}

/* ---------- 试运行结果展示（向导第六步 / 任务详情共用；对应真实前端的 checkNode） ---------- */
function dryHtml(d, t) {
  const lv = {ok: ['ok', '通过'], warn: ['warn', '警告'], error: ['bad', '不通过']};
  const k = (l, v, s) => `<div class="card kpi" style="padding:10px 12px"><div class="l">${l}</div><div class="v" style="font-size:20px">${v}</div><div class="d flat">${s || '&nbsp;'}</div></div>`;
  const nErr = d.items.filter(i => i.lv === 'error').length, nWarn = d.items.filter(i => i.lv === 'warn').length, nOk = d.items.length - nErr - nWarn;
  return `<div class="ar-kpis">${k('预估命中行数', (d.capped ? '≥ ' : '') + nf(d.est), `占表 ${d.pct.toFixed(2)}%（表约 ${nf(d.rows)} 行）${d.capped ? ' · 统计有上限' : ''}`)}${k('预计批数', nf(d.batches))}${k('预计耗时', d.secs ? fmtDur(d.secs * 1000) : '—')}${k('预计 binlog', d.binlogMB ? nf(d.binlogMB) + ' MB' : '—')}</div>
    <h4 style="margin:12px 0 6px">检查项 <span class="small muted">${nOk} 通过 · ${nWarn} 警告 · ${nErr} 不通过</span></h4>
    <div class="ar-checks">${d.items.map(c => `<div class="ar-chk ${c.lv === 'error' ? 'bad' : c.lv}" data-key="${esc(c.k)}"><span class="tag ${lv[c.lv][0]}">${c.k === 'large_batch' && c.lv !== 'ok' ? '需确认' : lv[c.lv][1]}</span><b>${esc(c.title)}</b><span>${esc(c.msg)}</span></div>`).join('')}</div>
    <h4 style="margin:12px 0 6px">最前 ${d.sample.length} 行样例</h4>
    ${d.sample.length ? tbl(d.cols, d.sample.map(r => r.map(v => esc(v))), {cells: r => r}) : empty('没有符合条件的数据')}
    <h4 style="margin:12px 0 4px">将执行的 SQL</h4>${sqlBlocks(d.sql, t.mode)}
    <div class="small muted" style="margin-top:6px">有效期至 ${tstr(d.at + 7 * DAY).slice(0, 16)}</div>`;
}
const taskDry = t => { const key = KEY_FIELDS(t); if (!dryValid(t.dry, key)) t.dry = dryCompute(t, key); return t.dry; };


/* ---------- 运行记录 ---------- */
function runRows() {
  const f = AR.rf, q = f.q.trim().toLowerCase();
  return visibleRuns().filter(r => (!f.task || r.task == f.task) && (!f.trg || r.trg === f.trg) && (!f.st || r.status === f.st) && (!q || `${r.name} ${r.tbl} ${r.by}`.toLowerCase().includes(q))).sort((a, b) => b.start - a.start);
}
function runsHtml() {
  const f = AR.rf, tasks = visibleTasks(), rows = runRows();
  return `<div class="toolbar">
    <select data-onchange="arRF" data-k="task" aria-label="任务"><option value="">任务：全部</option>${tasks.map(t => opt(t.id, t.name, f.task)).join('')}</select>
    <select data-onchange="arRF" data-k="trg" aria-label="触发方式"><option value="">触发方式：全部</option>${Object.keys(TRG).map(k => opt(k, TRG[k], f.trg)).join('')}</select>
    <select data-onchange="arRF" data-k="st" aria-label="结果"><option value="">结果：全部</option>${Object.keys(ST_R).map(k => opt(k, ST_R[k][0], f.st)).join('')}</select>
    <input type="search" placeholder="搜索 任务 / 库表 / 操作人" data-oninput="arRF" data-k="q" value="${esc(f.q)}" style="min-width:220px" aria-label="搜索"><span class="sp"></span><span class="small muted">共 ${rows.length} 条</span></div>
    <div id="arRunList">${runTable(rows)}</div>`;
}
function runTable(rows) {
  return tbl(['运行', '任务', '开始 / 结束', '触发', '结果', '扫描 / 备份 / 删除', '批数', '耗时', '峰值 Threads', '限流', ''], rows.slice(0, 80), {cls: 'ar-cards ar-tb-runs', cells: r => {
    const t = taskOf(r.task);
    return [`<a data-on="arRun" data-id="${r.id}"><b>#${r.id}</b></a>`, `${t ? `<a data-on="arTask" data-id="${t.id}">${esc(r.name)}</a>` : esc(r.name || '（未提交的草稿）')}<div class="small muted"><code>${esc(r.tbl)}</code></div>`,
      `<span class="nowrap small">${fmtAt(r.start)}</span><div class="small muted nowrap">${r.status === 'running' ? '进行中' : fmtAt(r.end).slice(5)}</div>`, `${TRG[r.trg]}<div class="small muted">${esc(r.by)}</div>`, rsTag(r) + (r.status === 'running' && runningOf(taskOf(r.task) || {id: -1}) ? `<div data-live="${r.task}" data-mode="mini" class="ar-live">${liveHtml(taskOf(r.task), false)}</div>` : ''),
      r.trg === 'dry' ? `<span class="muted">预估 ${nf(r.scanned)}（只读）</span>` : `<span class="nowrap">${nf(r.scanned)} / ${nf(r.backed)} / ${nf(r.deleted)}</span>`, r.trg === 'dry' ? '—' : nf(r.batches), r.status === 'running' ? '—' : fmtDur(r.dur),
      r.trg === 'dry' || r.trg === 'restore' ? '—' : r.peakThr, r.trg === 'dry' || r.trg === 'restore' ? '—' : (r.throttles ? `<span style="color:var(--warn)">${r.throttles}</span>` : 0), `<button class="btn sm" data-on="arRun" data-id="${r.id}">详情</button>`];
  }});
}

/* 批次吞吐曲线（内联 SVG）：折线 = 每个采样点的行/秒，红色带 = 被限流暂停的时段，虚线 = 当前批大小 */
function runChart(r) {
  const s = r.series; if (!s.length) return empty('没有吞吐数据（试运行 / 恢复 / 未执行）');
  const W_ = 640, H = 170, pl = 52, pr = 10, pt = 10, pb = 24, n = s.length, max = Math.max(...s, 1) * 1.12, mb = Math.max(...r.bsize, 1) * 1.12;
  const X = i => pl + (W_ - pl - pr) * i / (n - 1 || 1), Y = v => pt + (H - pt - pb) * (1 - v / max), Yb = v => pt + (H - pt - pb) * (1 - v / mb);
  let g = '';
  for (let k = 0; k <= 4; k++) { const v = max * k / 4, y = Y(v); g += `<line x1="${pl}" x2="${W_ - pr}" y1="${y.toFixed(1)}" y2="${y.toFixed(1)}" stroke="var(--border)"/><text x="${pl - 6}" y="${(y + 4).toFixed(1)}" text-anchor="end" font-size="10" fill="var(--muted)">${fmt(Math.round(v))}</text>`; }
  const dur = Math.max(r.status === 'running' ? (r.dur || s.length * 1000) : r.dur, 1);
  const bands = r.events.map(e => { const i = Math.max(0, Math.min(n - 1, Math.round((parseT(e.at) - r.start) / dur * (n - 1)))), w = Math.max(6, (W_ - pl - pr) / n * Math.max(1, e.waited / 60)); return `<rect x="${(X(i) - 1).toFixed(1)}" y="${pt}" width="${w.toFixed(1)}" height="${H - pt - pb}" fill="var(--bad)" opacity=".16"><title>${esc(e.at.slice(11))} 限流：${e.kind === 'lag' ? '复制延迟 ' + e.val + 's' : 'Threads_running ' + e.val}，等待 ${e.waited}s</title></rect>`; }).join('');
  const line = s.map((v, i) => (i ? 'L' : 'M') + X(i).toFixed(1) + ' ' + Y(v).toFixed(1)).join(''), bl = r.bsize.map((v, i) => (i ? 'L' : 'M') + X(i).toFixed(1) + ' ' + Yb(v).toFixed(1)).join('');
  const xl = [0, 1, 2, 3, 4].map(k => `<text x="${(pl + (W_ - pl - pr) * k / 4).toFixed(1)}" y="${H - 6}" text-anchor="${k === 0 ? 'start' : k === 4 ? 'end' : 'middle'}" font-size="10" fill="var(--muted)">${fmtDur(dur * k / 4)}</text>`).join('');
  return `<svg viewBox="0 0 ${W_} ${H}" role="img" aria-label="批次吞吐曲线" style="width:100%;height:auto">${g}${bands}<path d="${line} L${X(n - 1).toFixed(1)} ${Y(0)} L${pl} ${Y(0)} Z" fill="var(--brand)" opacity=".1"/><path d="${line}" fill="none" stroke="var(--brand)" stroke-width="1.8"/><path d="${bl}" fill="none" stroke="var(--c3)" stroke-width="1.3" stroke-dasharray="4 3"/>${xl}</svg>
    <div class="legend"><span><i style="background:var(--brand)"></i>吞吐（行/秒）</span><span><i style="background:var(--c3)"></i>批大小（自适应）</span><span><i style="background:var(--bad);opacity:.4"></i>被限流暂停</span></div>`;
}
const runStatHtml = r => kvList([['结果', `${rsTag(r)}${r.status === 'running' ? ' <span class="small muted">模拟进行中</span>' : ''}`], ['扫描 / 备份 / 删除', `${nf(r.scanned)} / ${nf(r.backed)} / ${nf(r.deleted)} 行`], ['批数 / 耗时', `${nf(r.batches)} 批 · ${r.status === 'running' ? '进行中' : fmtDur(r.dur)}`],
  ['峰值 Threads_running', r.trg === 'dry' || r.trg === 'restore' ? '—' : r.peakThr], ['峰值复制延迟', r.trg === 'dry' || r.trg === 'restore' ? '—' : (r.peakLag + ' s')], ['被限流次数', r.trg === 'dry' || r.trg === 'restore' ? '—' : r.throttles],
  ['主键断点范围', r.pkTo ? `${esc(r.pkName)} ∈ [${r.pkFrom}, ${r.pkTo}]` : '—']], 'c2');
function runDrawerHtml(r) {
  const t = taskOf(r.task), canRestore = t && r.backups.some(b => b.kind === 'db') && r.backed > 0 && !['running'].includes(r.status) && ['sched', 'manual'].includes(r.trg);
  const dbs = r.backups.filter(b => b.kind === 'db'), files = r.backups.filter(b => b.kind === 'file');
  return `<div class="row" style="margin-bottom:8px">${rsTag(r)}<span class="tag">${TRG[r.trg]}</span><span class="small muted">${t ? `<a data-on="arTask" data-id="${t.id}">${esc(r.name)}</a>` : esc(r.name)} · <code>${esc(r.tbl)}</code> · ${esc(instOf(r.inst).name)}</span></div>
    ${r.err ? notice(`<b>错误：</b>${esc(r.err)}`, 'bad', '✕') : ''}${r.note ? notice(esc(r.note), '', 'ℹ') : ''}${r.autoPause ? notice('该运行触发了自动暂停，任务已置为“已暂停”。', 'warn', '⏸') : ''}
    ${r.restoreOf ? notice(`这是对 <a data-on="arRun" data-id="${r.restoreOf}">运行 #${r.restoreOf}</a> 的数据恢复，<b>只补缺、不覆盖</b>。`, 'info', '↺') : ''}
    ${r.status === 'running' ? `<div data-live="${r.task}" data-mode="full" class="ar-live">${t ? liveHtml(t, true) : ''}</div>` : ''}
    ${kvList([['操作人 / 触发', `${uLabel(r.by)} · ${TRG[r.trg]}`], ['开始', fmtAt(r.start) + tstr(r.start).slice(16)], ['结束', r.status === 'running' ? '进行中' : tstr(r.end)]], 'c2')}<div id="arRunStat" style="margin-top:8px">${runStatHtml(r)}</div>
    ${r.trg === 'sched' || r.trg === 'manual' ? `<h4 style="margin:14px 0 4px">批次吞吐曲线</h4><div id="arRunChart">${runChart(r)}</div>
    <h4 style="margin:14px 0 4px">限流事件</h4>
    ${r.events.length ? tbl(['时间', '触发项', '读数 / 阈值', '等待', '结果'], r.events, {cells: e => [`<span class="nowrap">${esc(e.at.slice(5))}</span>`, ({lag: '从库复制延迟', disk: '磁盘剩余', threads: 'Threads_running'})[e.kind] || e.kind, `${e.val}${e.kind === 'lag' ? ' s' : ''} / ${e.thr}${e.kind === 'lag' ? ' s' : ''}`, e.waited + ' s', e.end === 'abort' ? '<span class="tag warn">超过最长等待，中断</span>' : e.end === 'wait' ? '<span class="tag bad">等待中</span>' : `<span class="tag ok">已恢复</span>${e.batchTo ? ' <span class="small muted">批量降到 ' + e.batchTo + '</span>' : ''}`]}) : '<div class="small muted">本次运行没有被限流。</div>'}
    <h4 style="margin:14px 0 4px">校验结果</h4>${r.verify.length ? `<div class="ar-checks">${r.verify.map(v => `<div class="ar-chk ${v.ok ? 'ok' : 'bad'}"><span class="tag ${v.ok ? 'ok' : 'bad'}">${v.ok ? '通过' : '失败'}</span><b>${esc(v.k)}</b><span>${esc(v.msg)}</span></div>`).join('')}</div>` : '<div class="small muted">没有校验记录。</div>'}
    <h4 style="margin:14px 0 4px">备份位置</h4>${!r.backups.length ? '<div class="small muted">' + (t && t.mode === 'cleanup' ? '仅清理：本任务不备份。' : '本次运行没有产生备份数据。') + '</div>' : ''}
    ${dbs.map(b => `<div class="card" style="margin-bottom:8px;padding:10px 12px"><div class="row"><span class="tag info">库表备份</span><code>${esc(b.loc)}</code><span class="small muted">${esc(b.inst)} · ${nf(b.rows)} 行</span><span class="sp"></span>${canRestore ? B('archive.restore', '恢复数据', {cls: 'sm danger', data: {id: r.id}}) : ''}</div>${r.restored.length ? `<div class="small muted">已恢复过：${r.restored.map(i => `<a data-on="arRun" data-id="${i}">#${i}</a>`).join('、')}</div>` : ''}</div>`).join('')}
    ${files.map(b => `<div class="card" style="margin-bottom:8px;padding:10px 12px"><div class="row"><span class="tag brand">文件备份</span><span>${esc(b.store)}</span><code>${esc(b.path)}</code></div>
      <div class="small muted" style="margin:4px 0">${nf(b.rows)} 行 · ${b.files.length} 个文件 · ${fmtBytes(b.bytes)} · ${b.enc ? '加密 ' + esc(b.enc) : '未加密'} · 清单 sha256 <code>${b.manifestSha.slice(0, 16)}…</code> ${b.manifestOk ? '<span class="tag ok">回读校验通过</span>' : '<span class="tag bad">未生成清单</span>'}</div>
      ${tbl(['文件', '行数', '大小', 'sha256'], b.files, {cells: f => [`<code>${esc(f.key)}</code>`, nf(f.rows), fmtBytes(f.bytes), `<code title="${f.sha256}">${f.sha256.slice(0, 16)}…</code>`]})}
      ${b.manifestOk ? `<div class="toolbar" style="margin-top:6px"><button class="btn sm" data-on="arManifest" data-id="${r.id}" data-i="${r.backups.indexOf(b)}">下载清单（模拟）</button></div>` : ''}</div>`).join('')}` : ''}
    <h4 style="margin:14px 0 4px">日志</h4><pre class="log" style="height:auto;max-height:240px">${esc(r.log.join('\n') || '（无）')}</pre>`;
}
function openRun(id) { const r = runOf(id); if (!r) return; AR.dr = {kind: 'run', id: r.id}; openDrawer(`运行 #${r.id} · ${r.name || '试运行'}`, runDrawerHtml(r), true); }

/* ---------- 备份存储 ---------- */
const stoTag = s => { const st = STO_ST[stStatus(s)]; return `<span class="tag ${st[1]}">${st[0]}</span>`; };
function storesHtml() {
  const bad = STORES.filter(s => stStatus(s) === 'err').length, edit = can('archive.store_edit');
  return `${bad ? notice(`有 <b>${bad}</b> 个备份存储连接异常，引用它的任务将被拒绝运行。`, 'bad', '!') : ''}
    <div class="toolbar"><span class="small muted">共 ${STORES.length} 个存储</span><span class="sp"></span>${B('archive.store_edit', '＋ 新建备份存储', {cls: 'primary', data: {op: 'new'}})}</div>
    ${tbl(['名称', '类型', '地址 / 桶 / 前缀', 'AK / SK', '保留', '状态', '最近测试', '被引用', '操作'], STORES, {cls: 'ar-cards ar-tb-stores', cells: s => [`<b>${esc(s.name)}</b>`, STORE_T[s.type],
      `<code>${esc((isFsT(s.type) ? s.path : s.endpoint) || '—')}</code>${s.bucket ? `<div class="small muted">桶 ${esc(s.bucket)}</div>` : ''}<div class="small muted">前缀 ${esc(s.prefix || '/')}</div>`,
      isFsT(s.type) ? '<span class="muted">—</span>' : `<span class="small mono">${esc(edit ? s.ak : maskAk(s.ak)) || '—'}</span> / <span class="small">${s.skSet ? '●●●●●●（已保存）' : '<span class="muted">未设置</span>'}</span>`, s.keep ? s.keep + ' 天' : '永久',
      stoTag(s) + (stStatus(s) === 'ok' && isFsT(s.type) && s.free != null ? `<div class="small muted">可用 ${esc(fmtBytes(s.free))}</div>` : ''), s.lastTest ? `<span class="small nowrap">${esc(s.lastTest.at.slice(5))}</span><div class="small ${s.lastTest.ok ? 'muted' : ''}" style="${s.lastTest.ok ? '' : 'color:var(--bad)'};max-width:220px">${esc(clip(s.lastTest.msg, 46))}</div>` : '—',
      storeUse(s.id) ? `${storeUse(s.id)} 个任务` : '<span class="muted">0</span>',
      `<div class="ops">${B('archive.store_edit', '编辑', {cls: 'sm', data: {op: 'edit', id: s.id}})}${B('archive.store_edit', '测试连接', {cls: 'sm', data: {op: 'test', id: s.id}})}${B('archive.store_edit', s.enabled ? '停用' : '启用', {cls: 'sm', data: {op: 'toggle', id: s.id}})}${B('archive.store_edit', '删除', {cls: 'sm danger', data: {op: 'del', id: s.id}})}</div>`]})}
`;
}

/* ---------- 安全策略 ---------- */
const polVal = (k, v) => k === 'allowNoBackup' ? (v ? '<span class="tag warn">允许（需填写理由并走审批）</span>' : '<span class="tag ok">不允许</span>') : Array.isArray(v) ? (v.length ? v.map(x => `<code>${esc(x)}</code>`).join(' ') : '<span class="muted">（空）</span>') : nf(v);
function policyHtml() {
  const unit = k => (POL_META.find(m => m[0] === k) || [0, 0, ''])[2];
  return `<div class="card"><h2>当前策略 <span class="sub">最后修改 ${esc(POLICY.at)} · ${esc(POLICY.by)}</span><span class="right">${B('archive.policy_edit', '编辑策略', {cls: 'primary'})}</span></h2>
    ${kvList(POL_META.map(([k, nm]) => [esc(nm), polVal(k, POLICY[k]) + (unit(k) && !Array.isArray(POLICY[k]) && typeof POLICY[k] !== 'boolean' ? ' ' + unit(k) : '')]), 'c2')}</div>`;
}
const bodyOf = tab => tab === 'runs' ? runsHtml() : tab === 'stores' ? storesHtml() : tab === 'policy' ? policyHtml() : tasksHtml();

/* ============================================================================
   新建 / 编辑向导（设计 §7：来源 → 条件 → 模式与备份 → 稳定性参数 → 调度 → 试运行并提交）
   ========================================================================== */
const STEPS = ['来源', '条件', '模式与备份', '稳定性参数', '调度', '试运行并提交'];
function initWiz(task) {
  W = {edit: !!task, orig: task ? clone(task) : null, t: task ? clone(task) : blankTask(), step: 0, dry: task ? task.dry : null, ack: {warn: false, big: false}, drying: false, serverErrs: []};
  ['hist', 'appr', 'dry', 'cum'].forEach(k => delete W.t[k]);
  W.t.p = Object.assign(P(), W.t.p); W.t.sched = Object.assign({kind: 'day', time: '02:00', wd: [1], md: 1, cron: '0 2 * * *'}, W.t.sched); W.t.bk.file = Object.assign({partMB: 256}, W.t.bk.file);
}
const wTb = () => W && W.t.inst && W.t.db ? findTable(W.t.inst, W.t.db, W.t.tbl) : null;
const allErrs = () => [0, 1, 2, 3, 4].map(i => errsStep(W.t, i).map(m => ({step: i, m}))).flat();
/* 是否需要重新审批：草稿 / 已驳回 / 已停用 / 待审批 / 结构变更暂停、改了关键字段、放宽了参数 → 需要；其余（调度 / 窗口 / 限速参数且只更保守）直接保存 */
function needApproval() {
  if (!W.edit) return true;
  return needApprovalOf(W.orig, W.orig, W.t).need;
}
function blockReason() {
  const e = allErrs(); if (e.length) return `第 ${e[0].step + 1} 步有问题：${e[0].m}`;
  if (!can('archive.apply')) return '没有提交权限（archive.apply）';
  if (!needApproval()) return '';
  const d = W.dry, key = KEY_FIELDS(W.t);
  if (!dryValid(d, key)) return d ? '试运行结果已失效（条件 / 目标 / 来源有改动或超过 7 天），请重新试运行' : '提交前必须先试运行';
  if (d.bad) return `试运行有 ${d.bad} 个红色检查项，不能提交`;
  if (d.warn && !W.ack.warn) return `试运行有 ${d.warn} 个黄色警告，请勾选“已知晓”`;
  if (d.needBig && !W.ack.big) return '命中比例超过 30%，请勾选“已确认大批量”';
  return '';
}
function wizardPage() {
  if (!can('archive.apply')) return `<div id="arView">${notice('你没有新建 / 修改归档任务的权限（<code>archive.apply</code>）。', 'warn', '!')}<a class="btn" href="#archive/tasks">返回任务列表</a></div>`;
  if (!W) initWiz(null);
  return `<div id="arView" data-wiz="1"><div class="toolbar"><button class="btn" data-on="arWizExit">← 返回任务列表</button><b>${W.edit ? `编辑归档任务「${esc(W.orig.name)}」` : '新建归档任务'}</b></div>
    <div class="card"><div id="arWizSteps">${stepsBar(STEPS, W.step)}</div><div id="arWizBody">${stepHtml()}</div><div id="arWizErr">${errHtml()}</div><div id="arWizNav" class="toolbar" style="margin-top:12px">${navHtml()}</div></div></div>`;
}
const stepHtml = () => [step0, step1, step2, step3, step4, step5][W.step]();
const errHtml = () => {
  const e = W.step < 5 ? errsStep(W.t, W.step) : allErrs().map(x => `第 ${x.step + 1} 步：${x.m}`), lines = e.concat(W.serverErrs || []);
  return lines.length ? `<div class="notice bad" style="margin-top:10px"><span>✕</span><div><b>${W.step < 5 ? '本步还有问题' : '提交前需要先修正'}</b><ul style="margin:4px 0 0;padding-left:18px">${lines.map(m => `<li>${esc(m)}</li>`).join('')}</ul></div></div>` : '';
};
function navHtml() {
  const last = W.step === 5, why = last ? blockReason() : '', canDraft = !W.edit || ['draft', 'rejected'].includes(W.orig.status);      // 只有新建、草稿和已驳回的任务可以保存草稿
  return `<button class="btn" data-on="arWizPrev" ${W.step ? '' : 'disabled'}>上一步</button>
    ${last ? (why ? `<button class="btn primary" disabled title="${esc(why)}">${needApproval() ? '提交申请' : '保存修改'}</button>` : B('archive.apply', needApproval() ? '提交申请' : '保存修改', {cls: 'primary', data: {op: 'submit'}})) : `<button class="btn primary" data-on="arWizNext">下一步</button>`}
    ${last && canDraft ? B('archive.apply', W.edit ? '保存修改（不提交）' : '保存草稿', {data: {op: 'draft'}}) : ''}
    ${last && why ? `<span class="small" style="color:var(--bad)">${esc(why)}</span>` : ''}
    <span class="sp"></span><button class="btn" data-on="arWizExit">取消</button>`;
}
function drawWiz() { part('arWizSteps', stepsBar(STEPS, W.step)); part('arWizBody', stepHtml()); part('arWizErr', errHtml()); part('arWizNav', navHtml()); }
const whereHtml = t => { if (!t.tcol) return '<span class="small muted">（选择时间列后由系统生成）</span>'; const w = whereRes(t); return w.err ? `<div class="small" style="color:var(--bad)">${esc(w.err)}</div>` : code(w.sql, 'sql'); };
function updWizDyn() {
  part('arWizErr', errHtml()); part('arWizNav', navHtml());
  if ($('#arWhere')) part('arWhere', whereHtml(W.t));
  if ($('#arSqlPrev')) part('arSqlPrev', sqlBlocks(sqlPreview(W.t), W.t.mode));
  if ($('#arSchedPrev')) part('arSchedPrev', schedPrevHtml());
  if ($('#arTplEx')) part('arTplEx', tplExHtml());
  if ($('#arPEst')) part('arPEst', pEstHtml());
}

/* 第 1 步：来源 */
function flowHtml() {
  const t = W.t; if (!t.rg) return '';
  const fl = archFlow(t.rg);
  if (!fl) return notice('该资源组没有配置“数据归档申请”审批流，不能提交。', 'bad', '✕');
  const parts = fl.map((gid, i) => { const us = levelUsers(t.rg, gid); return `第 ${i + 1} 级 ${esc(lvName(gid))}（${us.length ? us.map(u => esc(u.login)).join('、') : '本级无人可审，仅超级用户可审'}）`; });
  return notice('审批流：' + parts.join(' → '), fl.some(gid => !levelUsers(t.rg, gid).length) ? 'warn' : '', 'ℹ');
}
function step0() {
  const t = W.t, insts = eligibleInsts().slice(), inst = t.inst ? instOf(t.inst) : null, lock = W.edit ? 'disabled' : '';
  if (inst && !insts.some(i => i.id === inst.id)) insts.push(inst);
  const dbs = inst ? dbsOf(inst.id) : [], tbs = inst && t.db ? tablesOf(inst.id, t.db).filter(x => !x.view) : [], rgs = inst ? rgOptions(inst) : [], tb = wTb();
  return `<div class="form" style="grid-template-columns:150px minmax(0,1fr);max-width:860px">
    ${fld('任务名称', `<input type="text" autocomplete="off" maxlength="50" data-oninput="arW" data-k="name" value="${esc(t.name)}" placeholder="例：订单主表-已完成订单归档">`, {req: 1})}
    ${fld('实例', `<select data-onchange="arW" data-k="inst" ${lock} aria-label="实例"><option value="">请选择实例</option>${insts.map(i => opt(i.id, `${i.name}（${i.type} ${i.ver}${i.env ? ' · ' + i.env : ''}）${i.status === 'bad' ? ' · 连接异常' : i.status === 'warn' ? ' · 告警' : ''}`, t.inst)).join('')}</select>`, {req: 1})}
    ${fld('库', `<select data-onchange="arW" data-k="db" ${lock} aria-label="库"><option value="">请选择库</option>${dbs.map(d => opt(d.db, d.db + (protectedHit(d.db, '') ? '（受保护库）' : ''), t.db)).join('')}</select>`, {req: 1})}
    ${fld('表', `<select data-onchange="arW" data-k="tbl" ${lock} aria-label="表"><option value="">请选择表</option>${tbs.map(x => opt(x.n, `${x.n}（≈${fmtN(x.rows)} 行）${protectedHit(t.db, x.n) ? ' · 受保护' : ''}`, t.tbl, protectedHit(t.db, x.n))).join('')}</select>`, {req: 1})}
    ${fld('所属资源组', `<select data-onchange="arW" data-k="rg" aria-label="资源组"><option value="">${inst ? '请选择资源组' : '先选择实例'}</option>${rgs.map(r => opt(r, rgName(r) + (archFlow(r) ? '' : '（未配置“数据归档申请”审批流）'), t.rg, !archFlow(r))).join('')}</select>`, {req: 1})}
  </div>${W.edit ? notice('实例 / 库 / 表创建后不可修改。', 'info', 'ℹ') : ''}
  <div id="arFlow">${flowHtml()}</div>
  ${tb ? `<h4 style="margin:14px 0 6px">表信息</h4>${kvList([['说明', esc(tb.c || '—')], ['行数 / 大小', `≈ ${fmtN(tb.rows)} 行 · ${fmtMB(tb.size)} · ${esc(tb.eng)}`], ['主键', `<code>${esc(pkCols(tb).join(', ') || '无')}</code> ${pkCols(tb).length ? '' : '<span class="tag bad">无主键，不允许建任务</span>'}`],
    ['可作时间列', timeCols(tb).map(c => `<code>${esc(c.n)}</code> <span class="small muted">${esc(c.t)}${c.key ? ' · 有索引' : ' · 无索引'}</span>`).join('、') || '<span class="tag bad">没有 datetime / timestamp / date 列</span>'], ['其它', `${tb.part ? '<span class="tag warn">分区表（按普通表处理）</span> ' : ''}${FK_REF[t.db + '.' + t.tbl] ? '<span class="tag bad">被外键子表引用</span> ' : ''}${protectedHit(t.db, t.tbl) ? '<span class="tag bad">受保护库表</span>' : ''}` || '—']], 'c2')}` : ''}`;
}
/* 第 2 步：条件 */
function step1() {
  const t = W.t, tb = wTb(), tcs = tb ? timeCols(tb) : [];
  return `<div class="form" style="grid-template-columns:150px minmax(0,1fr);max-width:860px">
    ${fld('时间列', `<select data-onchange="arW" data-k="tcol" aria-label="时间列">${tcs.map(c => opt(c.n, `${c.n}（${c.t}${c.key ? ' · 有索引' : ' · 无索引'}）`, t.tcol)).join('') || '<option value="">（请先选择表）</option>'}</select>`, {req: 1})}
    ${fld(`保留天数（≥ ${POLICY.minRetain}）`, `<input type="number" min="${POLICY.minRetain}" max="${LIM.retainMax}" step="1" data-oninput="arW" data-k="retain" value="${esc(t.retain)}" style="max-width:160px"> 天`, {req: 1})}
    ${fld('附加条件（可选）', `<textarea rows="3" maxlength="500" data-oninput="arW" data-k="extra" placeholder="例：status IN (3,4)" aria-label="附加条件">${esc(t.extra)}</textarea>`)}
  </div><h4 style="margin:12px 0 4px">系统生成的 WHERE</h4><div id="arWhere">${whereHtml(t)}</div>`;
}
const tplExHtml = () => {
  const t = W.t, parts = [];
  if (t.mode !== 'cleanup' && t.bk.db.on && t.bk.db.db) parts.push(`库表 <code>${esc(t.bk.db.db)}.${esc(tplExpand(t.bk.db.tpl, t))}</code>`);
  if (t.mode !== 'cleanup' && t.bk.file.on && storeOf(t.bk.file.store)) parts.push(`文件 <code>${esc(filePath(t, 0))}</code>`);
  return parts.length ? `示例（按今天展开）：${parts.join('；')}` : '';
};
/* 第 3 步：模式与备份 */
function step2() {
  const t = W.t, d = t.bk.db, f = t.bk.file, insts = eligibleInsts().slice();
  if (d.inst && !insts.some(i => i.id === d.inst)) insts.push(instOf(d.inst));
  const dbsT = d.inst ? dbsOf(d.inst) : [];
  const modeCards = `<div class="ar-modes">${Object.keys(MODES).map(m => { const dis = m === 'cleanup' && !POLICY.allowNoBackup; return `<label class="ar-mode ${t.mode === m ? 'on' : ''} ${dis ? 'dis' : ''}"><input type="radio" name="arMode" value="${m}" data-onchange="arW" data-k="mode" ${t.mode === m ? 'checked' : ''} ${dis ? 'disabled' : ''}><b>${MODES[m][0]}</b><span>${MODES[m][2]}${dis ? '<br><b style="color:var(--bad)">安全策略未允许</b>' : ''}</span></label>`; }).join('')}</div>`;
  const dbPanel = `<div class="ar-bk"><label class="chk"><input type="checkbox" data-onchange="arW" data-k="bk.db.on" ${d.on ? 'checked' : ''}> <b>归档库表</b></label>
      ${d.on ? `<div class="form" style="grid-template-columns:130px minmax(0,1fr);margin-top:8px">${fld('目标实例', `<select data-onchange="arW" data-k="bk.db.inst" aria-label="目标实例"><option value="">请选择实例</option>${insts.map(i => opt(i.id, i.name + (i.id === t.inst ? '（同实例）' : ''), d.inst)).join('')}</select>`)}
        ${fld('归档库', `<select data-onchange="arW" data-k="bk.db.db" aria-label="归档库"><option value="">请选择库</option>${dbsT.map(x => opt(x.db, x.db + (protectedHit(x.db, '') ? '（受保护库）' : ''), d.db)).join('')}</select>`)}
        ${fld('表名模板', `<input type="text" autocomplete="off" data-oninput="arW" data-k="bk.db.tpl" value="${esc(d.tpl)}" placeholder="{table}_{yyyy}">`)}</div>` : ''}</div>`;
  const filePanel = `<div class="ar-bk"><label class="chk"><input type="checkbox" data-onchange="arW" data-k="bk.file.on" ${f.on ? 'checked' : ''}> <b>文件存储</b></label>
      ${f.on ? `<div class="form" style="grid-template-columns:130px minmax(0,1fr);margin-top:8px">${fld('备份存储', `<select data-onchange="arW" data-k="bk.file.store" aria-label="备份存储"><option value="">请选择</option>${STORES.map(s => opt(s.id, `${s.name}（${STORE_T[s.type]}）${stStatus(s) === 'off' ? ' · 已停用' : stStatus(s) === 'err' ? ' · 连接异常' : ''}`, f.store, !s.enabled)).join('')}</select>`)}
        ${fld('格式', `<select data-onchange="arW" data-k="bk.file.fmt" aria-label="格式">${opt('csv', 'CSV', f.fmt)}${opt('sql', 'SQL（INSERT 语句）', f.fmt)}</select>`)}
        ${fld('压缩 / 加密', `<label class="chk"><input type="checkbox" data-onchange="arW" data-k="bk.file.gzip" ${f.gzip ? 'checked' : ''}> gzip 压缩</label> <label class="chk" style="margin-left:14px"><input type="checkbox" data-onchange="arW" data-k="bk.file.enc" ${f.enc ? 'checked' : ''}> AES-256-GCM 加密</label>`)}
        ${fld('路径模板', `<input type="text" autocomplete="off" data-oninput="arW" data-k="bk.file.path" value="${esc(f.path)}" placeholder="{db}/{table}/{yyyy}{mm}{dd}">`)}
        ${fld('单文件上限（MB）', `<input type="number" min="${LIM.partMB[0]}" max="${LIM.partMB[1]}" step="1" data-oninput="arW" data-k="bk.file.partMB" value="${esc(f.partMB)}" style="max-width:140px">`)}</div>` : ''}</div>`;
  return `<h4 style="margin:0 0 6px">模式</h4>${modeCards}
    ${t.mode === 'cleanup' ? `${notice('<b>仅清理：不备份，直接删除，删除后无法恢复。</b>', 'warn', '!')}
      <div class="form" style="grid-template-columns:150px minmax(0,1fr);max-width:860px">${fld('不备份理由（至少 5 字）', `<textarea rows="3" maxlength="500" data-oninput="arW" data-k="reason" placeholder="例：压测临时流水，无保留价值" aria-label="不备份理由">${esc(t.reason)}</textarea>`, {req: 1})}</div>`
    : `<h4 style="margin:14px 0 6px">备份去向</h4>${dbPanel}${filePanel}<div class="small muted" id="arTplEx" style="margin-top:6px">${tplExHtml()}</div>`}
    <h4 style="margin:14px 0 4px">将执行的 SQL</h4><div id="arSqlPrev">${sqlBlocks(sqlPreview(t), t.mode)}</div>`;
}
/* 第 4 步：稳定性参数 */
const pEstHtml = () => { const t = W.t, p = t.p, b = +p.batch, sl = +p.sleep; if (!(b > 0) || !(sl >= 0)) return ''; const per = 120 + b * 0.22 + sl; return `约每批 ${Math.round(120 + b * 0.22)} ms 读写 + 休眠 ${sl} ms，吞吐 ≈ ${nf(Math.round(b / (per / 1000)))} 行/秒；单次上限 ${nf(+p.maxRows)} 行约需 ${fmtDur(Math.ceil(+p.maxRows / b) * per)}（粗略估算）。`; };
function step3() {
  const t = W.t, p = t.p, inst = t.inst ? instOf(t.inst) : null, sl = inst ? slavesOf(inst) : [], num = (k, w) => `<input type="number" step="1" data-oninput="arW" data-k="p.${k}" value="${esc(p[k])}" style="max-width:${w || 150}px">`;
  return `<div class="form" style="grid-template-columns:200px minmax(0,1fr);max-width:900px">
    ${fld('批大小（行）', num('batch'), {req: 1})}
    ${fld('批间休眠（ms）', num('sleep'), {req: 1})}
    ${fld('Threads_running 阈值', num('threads'), {req: 1})}
    ${fld('复制延迟阈值（秒）', num('lag'), {req: 1})}
    ${fld('磁盘剩余阈值（%）', num('disk'))}
    ${fld('最长等待（分钟）', num('maxWait'))}
    ${fld('批耗时目标（ms）', num('targetMs'))}
    ${fld('连续失败暂停（批）', num('fails'))}
    ${fld('单次运行最多处理（行）', num('maxRows', 200), {req: 1})}
  </div><div class="row" style="margin-top:8px"><button class="btn sm" data-on="arPDef">恢复为策略默认值</button><span class="small muted" id="arPEst">${pEstHtml()}</span></div>`;
}
/* 第 5 步：调度 */
function schedPrevHtml() {
  const t = W.t, ts = schedTimes(t.sched, simMs(), 5);
  if (!ts.length) return '<span class="small muted">当前设置算不出未来的触发时间（或预览还在计算）</span>';
  return `<ol style="margin:4px 0;padding-left:20px;line-height:2">${ts.map(x => `<li>${fmtAt(x)} 周${WD[new Date(x).getDay()]} ${inWin(minOf(x), t.win) ? '<span class="tag ok">在窗口内</span>' : '<span class="tag bad">在窗口外，将被跳过</span>'}</li>`).join('')}</ol>`;
}
function step4() {
  const t = W.t, s = t.sched;
  return `<div class="form" style="grid-template-columns:150px minmax(0,1fr);max-width:860px">
    ${fld('调度方式', `<select data-onchange="arW" data-k="sched.kind" aria-label="调度方式">${opt('day', '每天', s.kind)}${opt('week', '每周', s.kind)}${opt('month', '每月', s.kind)}${opt('cron', '高级 cron', s.kind)}</select>`, {req: 1})}
    ${s.kind === 'cron' ? fld('cron 表达式（分 时 日 月 周）', `<input type="text" autocomplete="off" data-oninput="arW" data-k="sched.cron" value="${esc(s.cron)}" placeholder="0 2 * * *" style="max-width:260px">`, {req: 1})
      : fld('开始时间', `<input type="time" data-oninput="arW" data-k="sched.time" value="${esc(s.time)}" style="max-width:160px">`, {req: 1})}
    ${s.kind === 'week' ? fld('星期', WD.map((n, i) => `<label class="chk" style="margin-right:10px"><input type="checkbox" data-onchange="arWd" data-d="${i}" ${(s.wd || []).includes(i) ? 'checked' : ''}> 周${n}</label>`).join('')) : ''}
    ${s.kind === 'month' ? fld('每月几日（1–28）', `<input type="number" min="1" max="28" step="1" data-oninput="arW" data-k="sched.md" value="${esc(s.md)}" style="max-width:120px">`) : ''}
    ${fld('执行窗口', `<input type="time" data-oninput="arW" data-k="win.from" value="${esc(t.win.from)}" style="max-width:150px"> – <input type="time" data-oninput="arW" data-k="win.to" value="${esc(t.win.to)}" style="max-width:150px">`, {req: 1})}
  </div><h4 style="margin:12px 0 2px">未来 5 次触发</h4><div id="arSchedPrev">${schedPrevHtml()}</div>`;
}
/* 第 6 步：试运行并提交 */
function step5() {
  const t = W.t, key = KEY_FIELDS(t), d = W.dry, valid = dryValid(d, key), na = needApproval(), errs = allErrs(), nd = W.edit ? needApprovalOf(W.orig, W.orig, t) : {need: true, why: ''};
  const sum = kvList([['名称', esc(t.name)], ['来源', `${esc(t.inst ? instOf(t.inst).name : '—')} · <code>${esc(t.db)}.${esc(t.tbl)}</code> · ${esc(rgName(t.rg))}`], ['条件', whereOf(t) ? `<code>${esc(whereOf(t))}</code>` : '—'], ['模式', modeTag(t.mode)], ['备份去向', bkTags(t)],
    ['调度 / 窗口', `${esc(schedText(t.sched))} · 窗口 ${esc(winText(t.win))}`], ['稳定性', `批 ${t.p.batch} 行 · 休眠 ${t.p.sleep} ms · 单次上限 ${nf(+t.p.maxRows)} 行 · Threads ≤ ${t.p.threads} · 延迟 ≤ ${t.p.lag}s`]], 'c2');
  let dry = '';
  if (!na) dry = notice('本次修改<b>只更保守</b>，<b>无需重新审批</b>，可直接保存。', 'ok', '✓');
  else {
    if (W.edit && ['rejected', 'disabled', 'draft', 'pending'].includes(W.orig.status)) dry += notice('该任务当前为「' + ST_T[W.orig.status][0] + '」，重新提交会开启一轮新的审批' + (W.orig.status === 'pending' ? '（原审批作废）' : '') + '。', 'info', 'ℹ');
    else if (W.edit && nd.need) dry += notice(`需要重新试运行并重新走审批：${esc(nd.why)}；审批通过前任务暂停调度。`, 'warn', '!');
    dry += `<div class="toolbar" style="margin:10px 0"><b>试运行</b><span class="sp"></span>${W.drying ? '<span class="tag brand">试运行中…</span>' : ''}${B('archive.dryrun', d ? '重新试运行' : '开始试运行', {cls: 'primary', data: {op: 'wiz'}})}</div>`;
    if (W.drying) dry += `<div class="ar-prog" style="margin:6px 0"><b style="width:60%;animation:arpulse 1s infinite alternate"></b></div>`;
    else if (!d) dry += empty('还没有试运行结果。', '▶');
    else {
      dry += valid ? '' : notice(dryValid(d, d.key) ? '试运行结果已失效：条件 / 目标 / 来源有改动，请重新试运行。' : '试运行结果已超过 7 天有效期，请重新试运行。', 'warn', '!');
      dry += `<div class="${valid ? '' : 'ar-stale'}">${dryHtml(d, t)}</div>`;
      if (valid) {
        dry += (d.bad ? notice(`<b>${d.bad} 个红色检查项未通过，不能提交。</b>`, 'bad', '✕') : '') + (d.warn ? `<label class="chk" style="margin:6px 0"><input type="checkbox" data-onchange="arAck" data-k="warn" ${W.ack.warn ? 'checked' : ''}> 我已阅读以上 <b>${d.warn}</b> 个黄色警告并确认继续</label><br>` : '') + (d.needBig ? `<label class="chk" style="margin:6px 0"><input type="checkbox" data-onchange="arAck" data-k="big" ${W.ack.big ? 'checked' : ''}> <b>已确认大批量</b>：预估命中 ${d.capped ? '至少 ' : ''}${nf(d.est)} 行（占表 ${d.pct.toFixed(1)}%，超过 30%）</label>` : '');
      }
    }
  }
  return `<h4 style="margin:0 0 6px">配置确认</h4>${sum}${errs.length ? notice('前面的步骤还有未修正的问题，修正前不能试运行 / 提交。', 'bad', '✕') : ''}${t.mode === 'cleanup' ? notice(`<b>仅清理：不备份。</b>理由：${esc(t.reason)}`, 'warn', '!') : ''}<div style="margin-top:10px">${dry}</div>`;
}

/* ============================================================================
   交互：普通交互（ON）与带权限码的按钮（ACTIONS）
   ========================================================================== */
const val = id => { const el = $('#' + id); return el ? el.value : ''; };
const setPath = (o, path, v) => { const ks = path.split('.'); const last = ks.pop(); const tgt = ks.reduce((a, k) => a[k], o); tgt[last] = v; };
const afterWrite = () => { redraw(); };
/* 系统审计：动作名沿用真实实现的约定“数据归档-<动作>”（internal/audit/ext.go 把它们归到 archive.* 权限码和“查询与导出”类别） */
const AUD_CODE = a => /^存储-/.test(a) ? 'archive.store_edit' : /^策略/.test(a) ? 'archive.policy_edit' : ['提交申请', '保存草稿', '修改任务'].includes(a) ? 'archive.apply' : a === '试运行' ? 'archive.dryrun'
  : ['暂停任务', '恢复任务', '暂停运行', '恢复运行'].includes(a) ? 'archive.pause' : a === '立即执行' ? 'archive.run' : a === '终止运行' ? 'archive.stop' : a === '恢复数据' ? 'archive.restore' : ['停用任务', '删除任务'].includes(a) ? 'archive.delete' : 'archive.view';
const aud = (action, detail, res, extra) => audit(AUD_CODE(action), `数据归档-${action}：${detail}`, res || '成功', extra || '');
UI.tick = () => {
  $$('[data-live]').forEach(el => { const t = taskOf(el.dataset.live); if (t) el.innerHTML = liveHtml(t, el.dataset.mode === 'full'); });
  if ($('#arKpi')) part('arKpi', kpiHtml());
  if (AR.dr && AR.dr.kind === 'run' && $('#drawer').classList.contains('open')) { const r = runOf(AR.dr.id); if (r && r.status === 'running') { part('arRunChart', runChart(r)); part('arRunStat', runStatHtml(r)); } }
};
UI.done = s => {
  toast(`归档运行 #${s.run.id} 已结束：${ST_R[s.run.status][0]}（读取 ${nf(s.run.scanned)} 行，删除 ${nf(s.run.deleted)} 行）`);
  if ($('#arBody')) redraw(); else buildNav();
};

/* ---------- 过滤 / 打开详情 ---------- */
ON.arF = (d, el) => { AR.f[d.k] = el.type === 'checkbox' ? el.checked : el.value; part('arList', taskTable()); };
ON.arRF = (d, el) => { AR.rf[d.k] = el.value; part('arRunList', runTable(runRows())); };
ON.arTask = d => openTask(d.id);
ON.arRun = d => openRun(d.id);
ON.arRunsOf = d => { AR.rf = {task: d.id, trg: '', st: '', q: ''}; closeDrawer(); go('archive', 'runs'); };
ON.arManifest = d => {
  const r = runOf(d.id), b = r && r.backups[+d.i]; if (!b) return;
  const t = taskOf(r.task), tb = findTable(r.inst, r.tbl.split('.')[0], r.tbl.split('.')[1]);
  const manifest = {files: b.files.map(f => ({key: f.key, rows: f.rows, size: f.bytes, sha256: f.sha256})), rows: b.rows, min_pk: [String(r.pkFrom)], max_pk: [String(r.pkTo)], cols: ((tb || {cols: []}).cols).map(c => ({name: c.n, type: c.t})), enc: b.enc || '',
    format: t ? t.bk.file.fmt : 'csv', gzip: t ? !!t.bk.file.gzip : false, table: r.tbl.split('.')[1], manifest_key: `${b.path}/manifest.json`};
  try { const a = document.createElement('a'); a.href = URL.createObjectURL(new Blob([JSON.stringify(manifest, null, 2)], {type: 'application/json'})); a.download = `manifest-${r.id}.json`; document.body.append(a); a.click(); a.remove(); setTimeout(() => URL.revokeObjectURL(a.href), 8000); toast('已下载清单 manifest-' + r.id + '.json（模拟）'); } catch (e) { toast('当前环境不支持下载'); }
};
/* 运行中的演示控制 */
const simOf = d => { const t = taskOf(d.id); return t && runningOf(t); };
ON.arSpike = d => { const s = simOf(d); if (s) { s.spike = 3; toast('已模拟一次负载升高：Threads_running 将超过阈值，运行会暂停等待并计入限流事件'); } };
ON.arHold = d => { const s = simOf(d); if (s) { s.hold = true; toast('已模拟持续高负载：等待超过最长等待后，本次将以“负载中断”结束'); } };
ON.arLag = d => { const s = simOf(d); if (s) { s.lagSpike = 3; toast(s.hasSlave ? '已模拟从库复制延迟升高' : '该实例没有从库，复制延迟检查跳过'); } };
ON.arRelease = d => { const s = simOf(d); if (s) { s.hold = false; s.spike = 0; toast('已解除高负载'); } };

/* ---------- 审批 ---------- */
ON.arApprove = d => {
  const t = taskOf(d.id); if (!t || !canApprove(t)) return toast('你不能审批这个申请（不是当前级审批人；或这是你自己提的——超级用户也不能审批自己提的）');
  const a = t.appr, last = a.level === a.levels.length - 1;
  openModal({title: `审批通过 · ${esc(t.name)}`, ok: last ? '通过并启用任务' : '通过', body: `${notice(`当前：${esc(pendingLvText(t))}`)}
      ${kvList([['来源', `${esc(instOf(t.inst).name)} · <code>${esc(t.db)}.${esc(t.tbl)}</code>`], ['条件', `<code>${esc(whereOf(t))}</code>`], ['模式 / 备份', `${modeTag(t.mode)} ${bkTags(t)}`], ['调度', esc(schedText(t.sched)) + ' · 窗口 ' + esc(winText(t.win))]])}
      ${t.mode === 'cleanup' ? notice(`<b>仅清理（不备份）</b>，理由：${esc(t.reason)}`, 'warn', '!') : ''}
      <div class="form" style="grid-template-columns:90px minmax(0,1fr);margin-top:8px"><label>审批意见（选填）</label><input type="text" id="arNote" autocomplete="off" maxlength="100"></div>`,
    onOk() {
      if (!canApprove(t)) { toast('状态已变化，无法审批'); return; }
      const lv = a.level, note = val('arNote').trim();
      a.steps.push({lv, user: ME.login, at: nowS(), action: 'approve', note}); a.level++;
      if (a.level >= a.levels.length) {
        t.status = 'enabled'; t.pauseKind = ''; t.pauseReason = ''; t.hist.push({at: nowS(), user: ME.login, text: '审批通过，任务已启用'});
        aud('审批通过', `审批通过任务「${t.name}」，审批已全部通过，任务已启用`, '成功', instOf(t.inst).name); toast(`已通过最后一级，「${t.name}」已启用；已通知申请人 ${t.appr.applicant}`);
      } else {
        t.hist.push({at: nowS(), user: ME.login, text: '审批通过（进入下一级）'});
        aud('审批通过', `审批通过任务「${t.name}」，进入下一级审批`, '成功', instOf(t.inst).name); toast(`已通过第 ${lv + 1} 级，已通知第 ${lv + 2} 级审批人`);
      }
      afterWrite();
    }});
};
ON.arReject = d => {
  const t = taskOf(d.id); if (!t || !canApprove(t)) return toast('你不能审批这个申请（不是当前级审批人；或这是你自己提的——超级用户也不能审批自己提的）');
  openModal({title: `驳回 · ${esc(t.name)}`, ok: '驳回', okCls: 'danger primary', body: `<div class="form" style="grid-template-columns:90px minmax(0,1fr)"><label class="req">驳回原因</label><textarea id="arWhy" rows="3" maxlength="200" placeholder="例：时间列没有索引"></textarea></div>`,
    onOk() {
      const why = val('arWhy').trim(); if ([...why].length < 2) { toast('请填写驳回原因（至少 2 个字）'); return false; }
      if (!canApprove(t)) { toast('状态已变化，无法审批'); return; }
      t.appr.steps.push({lv: t.appr.level, user: ME.login, at: nowS(), action: 'reject', note: why}); t.appr.reject = {user: ME.login, at: nowS(), reason: why}; t.status = 'rejected';
      t.hist.push({at: nowS(), user: ME.login, text: '驳回：' + why}); aud('驳回', `驳回任务「${t.name}」：${why}`, '成功', instOf(t.inst).name); toast(`已驳回，已通知申请人 ${t.appr.applicant}`); afterWrite();
    }});
};

/* ---------- 新建 / 编辑 / 提交 / 保存草稿 ---------- */
function normalizeWiz() {
  const t = W.t; ['retain'].forEach(k => { t[k] = Number(t[k]); }); ['batch', 'sleep', 'threads', 'lag', 'disk', 'maxWait', 'targetMs', 'maxRows', 'fails'].forEach(k => { t.p[k] = Number(t.p[k]); });
  t.sched.md = Number(t.sched.md); t.extra = String(t.extra || '').trim(); t.name = String(t.name).trim(); t.reason = String(t.reason || '').trim();
  t.bk.db.tpl = String(t.bk.db.tpl || '').trim(); t.bk.file.path = String(t.bk.file.path || '').trim(); t.bk.file.partMB = Number(t.bk.file.partMB) || 256;
}
function submitWiz(draft) {
  normalizeWiz();
  const t = W.t, inst = instOf(t.inst).name;
  const mkAppr = () => ({applicant: ME.login, at: nowS(), levels: archFlow(t.rg).slice(), level: 0, steps: []});
  const applyTo = target => { ['name', 'rg', 'tcol', 'retain', 'extra', 'mode', 'reason', 'bk', 'sched', 'win', 'p'].forEach(k => { target[k] = clone(t[k]); }); };
  let target;
  if (draft) {
    /* 保存草稿：新建、草稿和已驳回的任务可以；不要求试运行、不建审批；其余状态不行（已生效的任务要改，只能做“更保守”的调整直接保存，或提交审批） */
    if (W.edit && !['draft', 'rejected'].includes(W.orig.status)) return toast('只有草稿和已驳回的任务可以保存草稿；其他状态的任务要么做“更保守”的调整直接保存，要么提交审批');
    if (!String(t.name).trim()) return toast('请先填写任务名称');
    const es = [0, 1, 2, 3, 4].flatMap(i => errsStep(t, i, {draft: true}).map(m => `第 ${i + 1} 步：${m}`));
    if (es.length) { W.serverErrs = es; part('arWizErr', errHtml()); return toast(es[0]); }
    W.serverErrs = [];
    if (!W.edit) {
      const o = clone(t); delete o.id;
      target = mkTask(Object.assign(o, {status: 'draft', creator: ME.login, created: nowS(), appr: null, dry: null, hist: [{at: nowS(), user: ME.login, text: '创建任务（草稿）'}]}));
      aud('保存草稿', `任务「${t.name}」：${t.db}.${t.tbl}，${MODES[t.mode][0]}，保留 ${t.retain} 天`, '成功', inst);
      toast('已保存草稿（没有提交审批，也不要求试运行）');
    } else {
      target = taskOf(W.orig.id); if (!target) return toast('任务已不存在');
      if (runningOf(target)) return toast('运行中的任务不能编辑，请先暂停或终止本次运行');
      applyTo(target); target.hist.push({at: nowS(), user: ME.login, text: '保存修改（未提交）'});
      aud('保存草稿', `修改任务「${t.name}」`, '成功', inst);
      toast(target.status === 'draft' ? '已保存草稿' : '已保存修改（没有提交，状态仍为已驳回）');
    }
  } else {
    const why = blockReason(); if (why) return toast(why);
    const na = needApproval();
    if (!W.edit) {
      const o = clone(t); delete o.id;
      target = mkTask(Object.assign(o, {status: 'pending', creator: ME.login, created: nowS(), appr: mkAppr(), dry: W.dry, hist: [{at: nowS(), user: ME.login, text: `提交申请（试运行于 ${tstr(W.dry.at)}）`}]}));
      aud('提交申请', `新建任务「${t.name}」并提交审批：${t.db}.${t.tbl}，${MODES[t.mode][0]}，保留 ${t.retain} 天`, '成功', inst);
      toast(`已提交申请，进入待审批：${pendingLvText(target)}`);
    } else {
      target = taskOf(W.orig.id); if (!target) return toast('任务已不存在');
      if (runningOf(target)) return toast('运行中的任务不能编辑，请先暂停或终止本次运行');
      const o = W.orig, diff = describeChanges(o, t), keyCh = ['inst', 'db', 'tbl', 'tcol', 'retain', 'extra'].some(k => String(o[k]).trim() !== String(t[k]).trim());
      applyTo(target);
      if (na) {
        Object.assign(target, {status: 'pending', appr: mkAppr(), dry: W.dry, pauseKind: '', pauseReason: '', fp: fpOf(target.inst, target.db, target.tbl)});
        if (keyCh) { target.ckFrom = simMs() + 1; refreshTaskStats(target); }      // 来源 / 条件改了，旧断点不再适用
        target.hist.push({at: nowS(), user: ME.login, text: `修改并重新提交审批（${diff.join('；') || '无字段变化'}）`});
        aud('提交申请', `修改任务「${t.name}」并重新提交审批：${diff.join('；') || '无字段变化'}`, '成功', inst); toast(`已重新提交审批：${pendingLvText(target)}`);
      } else {
        target.hist.push({at: nowS(), user: ME.login, text: `调整参数（更保守，无需重新审批）：${diff.join('；') || '无字段变化'}`});
        aud('修改任务', `调整任务「${t.name}」参数（更保守，无需重新审批）：${diff.join('；') || '无字段变化'}`, '成功', inst); toast('已保存修改（更保守的调整，无需重新审批）');
      }
    }
  }
  const id = target.id; W = null; buildNav(); go('archive', 'tasks'); setTimeout(() => { if ($('#arView')) openTask(id); }, 80);
}
ACTIONS['archive.apply'] = d => {
  if (d.op === 'submit') return submitWiz(false);
  if (d.op === 'draft') return submitWiz(true);
  if (d.op === 'edit') {
    const t = taskOf(d.id); if (!t) return;
    if (runningOf(t)) return toast('运行中的任务不能编辑，请先暂停或终止本次运行');
    initWiz(t);
  } else initWiz(null);
  go('archive', 'wizard');
};
ON.arWizExit = () => { W = null; go('archive', 'tasks'); };
ON.arWizPrev = () => { if (W.step > 0) { W.step--; W.serverErrs = []; drawWiz(); } };
ON.arWizNext = () => { const e = errsStep(W.t, W.step); if (e.length) { toast(e[0]); part('arWizErr', errHtml()); return; } W.step++; W.serverErrs = []; drawWiz(); window.scrollTo(0, 0); };
ON.arPDef = () => { Object.assign(W.t.p, P()); drawWiz(); };
ON.arAck = (d, el) => { W.ack[d.k] = el.checked; part('arWizNav', navHtml()); };
ON.arWd = (d, el) => { const s = W.t.sched, n = +d.d; s.wd = (s.wd || []).filter(x => x !== n); if (el.checked) s.wd.push(n); updWizDyn(); };
const REDRAW_KEYS = new Set(['inst', 'db', 'tbl', 'mode', 'rg', 'bk.db.on', 'bk.file.on', 'bk.db.inst', 'sched.kind']);
ON.arW = (d, el, e) => {
  const k = d.k, t = W.t; let v = el.type === 'checkbox' ? el.checked : el.value;
  if (k === 'inst') {
    t.inst = +v || 0; t.db = ''; t.tbl = ''; t.tcol = ''; const inst = t.inst ? instOf(t.inst) : null, rgs = inst ? rgOptions(inst) : [];
    t.rg = rgs.find(r => archFlow(r)) || rgs[0] || 0; if (inst) { t.bk.db.inst = inst.id; const dbs = dbsOf(inst.id); t.bk.db.db = (dbs.find(x => /archive/i.test(x.db)) || {db: ''}).db; }
  } else if (k === 'db') { t.db = v; t.tbl = ''; t.tcol = ''; }
  else if (k === 'tbl') { t.tbl = v; const tb = findTable(t.inst, t.db, v), tcs = tb ? timeCols(tb) : []; t.tcol = (tcs.find(c => c.n === 'created_at') || tcs[0] || {n: ''}).n; }
  else if (k === 'rg') t.rg = +v || 0;
  else if (k === 'bk.db.on' && v) { const dbs = dbsOf(t.bk.db.inst || t.inst); if (!t.bk.db.inst) t.bk.db.inst = t.inst; if (!t.bk.db.db) t.bk.db.db = (dbs.find(x => /archive/i.test(x.db)) || dbs[0] || {db: ''}).db; setPath(t, k, v); }
  else if (k === 'bk.db.inst') { t.bk.db.inst = +v; const dbs = dbsOf(+v); t.bk.db.db = (dbs.find(x => /archive/i.test(x.db)) || dbs[0] || {db: ''}).db; }
  else if (k === 'bk.file.on' && v) { setPath(t, k, v); if (!t.bk.file.store) t.bk.file.store = (STORES.find(s => s.enabled && stStatus(s) !== 'err') || {id: 0}).id; }
  else if (k === 'bk.file.store') t.bk.file.store = +v || 0;
  else setPath(t, k, v);
  if (e && e.type === 'change' && REDRAW_KEYS.has(k)) { W.ack = {warn: false, big: false}; W.serverErrs = []; drawWiz(); } else updWizDyn();
};
function runWizDry() {
  const e = allErrs(); if (e.length) { toast(`第 ${e[0].step + 1} 步：${e[0].m}`); return; }
  if (W.drying) return;
  normalizeWiz(); W.drying = true; W.serverErrs = []; if (W.step === 5) drawWiz();
  setTimeout(() => {
    if (!W) return;
    const t = W.t, d = dryCompute(t, KEY_FIELDS(t)); W.drying = false;
    if (!d) { toast('表信息读取失败'); return; }
    if (d.error) { W.serverErrs = [d.error]; if (W.step === 5 && $('#arWizBody')) drawWiz(); return; }      // 502 source_error：连不上源 / 目标，没有检查报告，也不记运行
    W.dry = d; W.ack = {warn: false, big: false};
    const e0 = d.items.find(i => i.lv === 'error' && i.k !== 'large_batch');
    mkRun(Object.assign(clone(t), {id: W.edit ? W.orig.id : 0}), {start: simMs(), trg: 'dry', st: d.bad ? 'failed' : 'success', rows: d.est, dur: 2500 + hs(t.name) % 4000, by: ME.login,
      note: `预估命中 ${nf(d.est)} 行（占表 ${d.pct.toFixed(1)}%），${d.warn} 个警告${d.bad ? '，存在红色检查项' : ''}`, err: e0 ? (e0.title + '：' + e0.msg).slice(0, 400) : ''});
    aud('试运行', `试运行归档任务「${t.name}」：预估命中 ${nf(d.est)} 行（占表 ${d.pct.toFixed(1)}%），${d.warn} 个警告，${d.bad ? '有红色项' : '通过'}`, d.bad ? '警告' : '成功', instOf(t.inst).name);
    if (W.step === 5 && $('#arWizBody')) drawWiz();
  }, 700);
}
ACTIONS['archive.dryrun'] = d => {
  if (d.op !== 'task') return runWizDry();
  const t = taskOf(d.id); if (!t) return;
  const ev = [0, 1, 2, 3, 4].flatMap(i => errsStep(t, i, {draft: true})); if (ev.length) return toast('校验未通过：' + ev[0]);
  const r = dryCompute(t, KEY_FIELDS(t)); if (!r) return;
  if (r.error) return toast(r.error);
  t.dry = r;
  const e0 = r.items.find(i => i.lv === 'error' && i.k !== 'large_batch');
  mkRun(t, {start: simMs(), trg: 'dry', st: r.bad ? 'failed' : 'success', rows: r.est, dur: 2500 + hs(t.name) % 4000, by: ME.login, note: `预估命中 ${nf(r.est)} 行（占表 ${r.pct.toFixed(1)}%），${r.warn} 个警告${r.bad ? '，存在红色检查项' : ''}`, err: e0 ? (e0.title + '：' + e0.msg).slice(0, 400) : ''});
  aud('试运行', `试运行归档任务「${t.name}」：预估命中 ${nf(r.est)} 行（占表 ${r.pct.toFixed(1)}%），${r.warn} 个警告，${r.bad ? '有红色项' : '通过'}`, r.bad ? '警告' : '成功', instOf(t.inst).name);
  openModal({title: `试运行结果 · ${esc(t.name)}`, wide: true, ok: '', cancel: '关闭', code: 'archive.dryrun', body: (r.bad ? notice('有红色检查项未通过，不能提交审批。', 'bad', '✕') : '') + dryHtml(r, t)});
  if ($('#arBody') && AR.tab === 'runs') redraw();
};

/* ---------- 暂停 / 恢复 / 立即执行 / 终止 ---------- */
ACTIONS['archive.pause'] = d => {
  const t = taskOf(d.id); if (!t) return;
  const s = runningOf(t);
  if (d.op === 'resume') {
    if (t.status !== 'paused') return toast(`只有已暂停的任务可以恢复（当前：${ST_T[t.status][0]}）`);
    if (t.pauseKind === 'schema') return toast('源表结构变更导致的暂停需要重新试运行并提交审批（点“编辑 / 重新提交”），不能直接恢复');
    t.status = 'enabled'; t.pauseKind = ''; t.pauseReason = ''; t.pauseAt = ''; t.hist.push({at: nowS(), user: ME.login, text: '恢复任务（按调度继续，从断点接着跑）'});
    aud('恢复任务', `恢复归档任务「${t.name}」`, '成功', instOf(t.inst).name); toast('已恢复，按调度从断点继续'); return afterWrite();
  }
  if (s) {
    simPause(s, !s.paused); aud(s.paused ? '暂停运行' : '恢复运行', `${s.paused ? '暂停' : '继续'}归档运行 #${s.run.id}「${t.name}」`, '成功', instOf(t.inst).name);
    toast(s.paused ? '已暂停：当前批做完后停，保留断点，可继续' : '已继续运行'); return afterWrite();
  }
  if (t.status !== 'enabled') return toast(`只有已启用的任务可以暂停（当前：${ST_T[t.status][0]}）`);
  t.status = 'paused'; t.pauseKind = 'manual'; t.pauseAt = nowS(); t.pauseReason = `${ME.login} 手动暂停`; t.hist.push({at: nowS(), user: ME.login, text: '暂停任务'});
  aud('暂停任务', `暂停归档任务「${t.name}」`, '成功', instOf(t.inst).name); toast('任务已暂停，不再按调度运行'); afterWrite();
};
ACTIONS['archive.run'] = d => {
  const t = taskOf(d.id); if (!t) return;
  const blk = startBlock(t, true);      // 窗口之外的前置检查（窗口由确认框里的“忽略执行窗口”决定）
  if (blk) return toast(blk);
  const dc = taskDry(t), eff = dc && !dc.error ? dc.eff : 0, est = dc && !dc.error ? dc.est : 0;
  const inWindow = inWin(minOf(simMs()), t.win);
  openModal({title: '⚠ 立即执行归档', okCls: 'danger primary', ok: '确认执行', code: 'archive.run', body: `<div class="notice bad"><span>⚠</span><div>将<b>立即</b>对 <code>${esc(t.db)}.${esc(t.tbl)}</code>（${esc(instOf(t.inst).name)}）执行一次归档：${MODES[t.mode][0]}。预计本次处理 <b>${nf(eff)}</b> 行（预估命中 ${nf(est)} 行，单次上限 ${nf(Math.min(+t.p.maxRows, POLICY.maxRows))} 行）；${t.mode === 'backup' ? '只备份，不删除。' : t.mode === 'cleanup' ? '<b>不备份，删除后无法恢复。</b>' : '先备份并校验，通过后才删源。'}</div></div>
      ${kvList([['条件', `<code>${esc(whereOf(t))}</code>`], ['当前时间 / 窗口', `${tstr(simMs()).slice(11, 16)} / ${esc(winText(t.win))} ${inWindow ? '<span class="tag ok">在窗口内</span>' : '<span class="tag warn">窗口外</span>'}`]])}
      <label class="chk" style="margin-top:10px"><input type="checkbox" id="arRunIgn" ${inWindow ? '' : 'checked'}> 忽略执行窗口</label>`,
    onOk() {
      const ign = $('#arRunIgn').checked, blk2 = startBlock(t, ign);
      if (blk2) { aud('立即执行', `立即执行归档任务「${t.name}」被拒绝：${blk2}`, '拒绝', instOf(t.inst).name); toast(blk2); return false; }
      const s = startRun(t, {trg: 'manual', by: ME.login, ignoreWin: ign});
      aud('立即执行', `立即执行归档任务「${t.name}」（运行 #${s.run.id}）${ign ? '，已忽略执行窗口' : ''}`, '成功', instOf(t.inst).name);
      t.hist.push({at: nowS(), user: ME.login, text: `立即执行（运行 #${s.run.id}）`}); toast(`已开始运行 #${s.run.id}（模拟，约 30 秒）`);
      if ($('#arBody')) { redraw(); } openTask(t.id);
    }});
};
ACTIONS['archive.stop'] = d => {
  const t = taskOf(d.id), s = t && runningOf(t); if (!s) return toast('没有进行中的运行');
  risky('archive.stop', '终止运行', `终止「${esc(t.name)}」的运行 #${s.run.id}：当前批做完后结束，保留断点（${esc(s.run.pkName)} = ${s.run.pkFrom + Math.round(s.done * 1.4)}）。已处理 ${nf(s.done)} / ${nf(s.total)} 行。`, '', () => {
    const cur = runningOf(t); if (!cur) return;
    cur.killBy = ME.login; aud('终止运行', `终止归档运行 #${cur.run.id}「${t.name}」（已处理 ${nf(cur.done)} 行）`, '成功', instOf(t.inst).name); finishSim(cur, 'killed');
  });
};
ACTIONS['archive.delete'] = d => {
  const t = taskOf(d.id); if (!t) return;
  if (runningOf(t)) return toast('该任务正在运行，请先终止运行');
  if (['enabled', 'paused', 'pending'].includes(t.status)) {
    return risky('archive.delete', '停用归档任务', `停用「${esc(t.name)}」后不再调度${t.status === 'pending' ? '，进行中的审批也随之作废' : ''}；运行记录与已备份的数据都保留。`, '', () => {
      if (t.status === 'pending' && t.appr) t.appr.aborted = true;
      t.status = 'disabled'; t.pauseKind = ''; t.pauseReason = ''; t.hist.push({at: nowS(), user: ME.login, text: '停用任务'}); aud('停用任务', `停用归档任务「${t.name}」`, '成功', instOf(t.inst).name); toast('任务已停用'); afterWrite();
    });
  }
  risky('archive.delete', '删除归档任务', `将删除任务「${esc(t.name)}」的配置；历史运行记录与已备份的数据保留。`, '', () => {
    TASKS.splice(TASKS.indexOf(t), 1); aud('删除任务', `删除归档任务「${t.name}」（${t.db}.${t.tbl}）`, '成功', instOf(t.inst).name); closeDrawer(); AR.dr = null; toast('任务已删除'); afterWrite();
  }, t.name);
};
ACTIONS['archive.restore'] = d => {
  const r = runOf(d.id), t = r && taskOf(r.task); if (!t) return toast('任务已删除，无法恢复');
  const db = r.backups.find(b => b.kind === 'db'); if (!db) return;
  const again = r.restored.length > 0;
  risky('archive.restore', '恢复数据（回灌到源表）', `按运行 #${r.id} 的主键范围（${esc(r.pkName)} ∈ [${r.pkFrom}, ${r.pkTo}]）把 <b>${nf(r.backed)}</b> 行从 <code>${esc(db.loc)}</code> 以 <code>INSERT IGNORE</code> 回灌到源表 <code>${esc(r.tbl)}</code>（${esc(instOf(r.inst).name)}）。<b>只补缺、不覆盖</b>已存在的行。${again ? '<br><b>该运行已恢复过，再次恢复通常是空操作。</b>' : ''}`,
    `INSERT IGNORE INTO ${bq(r.tbl.split('.')[0])}.${bq(r.tbl.split('.')[1])}\nSELECT * FROM ${bq(db.loc.split('.')[0])}.${bq(db.loc.split('.')[1])}\n WHERE ${bq(r.pkName)} BETWEEN ${r.pkFrom} AND ${r.pkTo};`, () => {
      const inserted = again ? 0 : r.deleted, rr = mkRun(t, {start: simMs(), trg: 'restore', st: 'success', rows: r.backed, dur: Math.max(3000, Math.round(inserted / 20000) * 1000 + 2000), by: ME.login, of: r.id, back: inserted, pkFrom: r.pkFrom, pkTo: r.pkTo,
        note: `按运行 #${r.id} 的主键范围 INSERT IGNORE 回灌（只补缺、不覆盖）：备份中 ${nf(r.backed)} 行，补回 ${nf(inserted)} 行`});
      r.restored.push(rr.id);
      aud('恢复数据', `恢复归档数据：运行 #${r.id}「${t.name}」→ ${r.tbl}，补回 ${nf(inserted)} 行、跳过 ${nf(r.backed - inserted)} 行已存在（恢复运行 #${rr.id}）`, '成功', instOf(r.inst).name);
      toast(`已恢复：补回 ${nf(inserted)} 行，${nf(r.backed - inserted)} 行已存在被跳过`); if ($('#arBody')) redraw(); openRun(rr.id);
    }, t.name);
};

/* ---------- 备份存储 ---------- */
const hasDotDot = p => String(p).split(/[\/\\]/).some(s => s === '..');
function storeFormHtml(s) {
  const n = !s, ty = n ? 'nfs' : s.type, fs = isFsT(ty), used = !n && storeUse(s.id) > 0, v = k => esc(n ? '' : (s[k] == null ? '' : s[k]));
  return `<div class="form" style="grid-template-columns:120px minmax(0,1fr)">
    ${fld('名称', `<input type="text" id="stName" autocomplete="off" maxlength="50" value="${v('name')}" placeholder="例：阿里云 OSS 冷归档">`, {req: 1})}
    ${fld('类型', `<select id="stType" data-onchange="arSType" ${used ? 'disabled' : ''} aria-label="类型">${Object.keys(STORE_T).map(k => opt(k, STORE_T[k], ty)).join('')}</select>`, {req: 1, hint: used ? '已被任务引用，不能修改类型' : ''})}
    <div id="stFs" style="display:${fs ? 'contents' : 'none'}">${fld('目录', `<input type="text" id="stPath" autocomplete="off" value="${v('path')}" placeholder="绝对路径">`, {req: 1})}</div>
    <div id="stObj" style="display:${fs ? 'none' : 'contents'}">${fld('Endpoint', `<input type="text" id="stEndpoint" autocomplete="off" value="${v('endpoint')}" placeholder="https://host:port">`, {req: 1})}
      ${fld('Region（可选）', `<input type="text" id="stRegion" autocomplete="off" value="${v('region')}">`)}
      ${fld('桶', `<input type="text" id="stBucket" autocomplete="off" value="${v('bucket')}">`, {req: 1})}
      ${fld('AK', `<input type="text" id="stAk" autocomplete="off" value="${v('ak')}">`, {req: 1})}
      ${fld('SK', `<input type="password" id="stSk" autocomplete="new-password" value="" placeholder="${!n && s.skSet ? '●●●●●●●●  已保存，留空表示不修改' : '密钥'}">`)}
      ${fld('路径风格', `<label class="chk"><input type="checkbox" id="stPS" ${!n && s.pathStyle ? 'checked' : ''}> 路径风格访问</label>`)}</div>
    ${fld('路径前缀', `<input type="text" id="stPrefix" autocomplete="off" value="${v('prefix')}" placeholder="dbtool">`)}
    ${fld('保留天数（0 = 永久）', `<input type="number" id="stKeep" min="0" max="${LIM.storeKeep[1]}" step="1" value="${n ? 90 : s.keep}" style="max-width:140px"> 天`)}
    ${n ? fld('启用', '<label class="chk"><input type="checkbox" id="stEn" checked> 创建后立即可用</label>') : ''}
  </div><div class="toolbar" style="margin-top:10px"><button class="btn" data-on="arSTest">测试连接</button><span class="small muted" id="stTestOut"></span></div>`;
}
ON.arSType = () => {
  const fs = isFsT(val('stType')); if ($('#stFs')) $('#stFs').style.display = fs ? 'contents' : 'none'; if ($('#stObj')) $('#stObj').style.display = fs ? 'none' : 'contents';
};
const stoCur = () => ({type: val('stType'), path: val('stPath').trim(), endpoint: val('stEndpoint').trim(), bucket: val('stBucket').trim()});
ON.arSTest = () => {
  const c = stoCur(), out = $('#stTestOut'); if (!out) return;
  if (!(isFsT(c.type) ? c.path : c.endpoint)) { out.innerHTML = `<span class="tag bad">失败</span> ${isFsT(c.type) ? '请填写目录' : '请填写 Endpoint'}`; return; }
  const p = storeProbe(c); out.innerHTML = '测试中…';
  setTimeout(() => { if (!$('#stTestOut')) return; $('#stTestOut').innerHTML = `<span class="tag ${p.ok ? 'ok' : 'bad'}">${p.ok ? '连接成功' : p.kind === 'timeout' ? '超时' : '失败'}</span> ${esc(p.msg)}`; }, 450);
};
/* 校验（stores.go validate）：名称 2–50 且不重名；local / nfs 要绝对路径、不含 ..；s3 / oss 要 endpoint（http(s)://）、桶、AK，新建时要 SK；前缀不含 .. 且 ≤ 200 字；保留天数 0–36500 */
function checkStore(n, self) {
  const e = [], nl = [...n.name].length;
  if (nl < 2 || nl > 50) e.push(`名称需要 2–50 个字（当前 ${nl}）`); else if (STORES.some(x => x !== self && x.name === n.name)) e.push('存储名称已存在');
  if (isFsT(n.type)) { if (!n.path) e.push('请填写目录'); else if (!absPathRe.test(n.path)) e.push('目录需要是绝对路径'); else if (hasDotDot(n.path)) e.push('目录不能包含 ..'); }
  else {
    if (!/^https?:\/\//.test(n.endpoint)) e.push('endpoint 需要以 http:// 或 https:// 开头');
    if (!n.bucket) e.push('请填写桶名'); if (!n.ak) e.push('请填写 AK'); if (!n.sk && !(self && self.skSet)) e.push('请填写 SK');
  }
  if (hasDotDot(n.prefix)) e.push('路径前缀不能包含 ..'); if ([...n.prefix].length > 200) e.push('路径前缀最长 200 字');
  if (!Number.isInteger(n.keep) || n.keep < LIM.storeKeep[0] || n.keep > LIM.storeKeep[1]) e.push(`保留天数需要在 0（永久）到 ${nf(LIM.storeKeep[1])} 之间`);
  return e;
}
ACTIONS['archive.store_edit'] = d => {
  const s = storeOf(d.id);
  if (d.op === 'new' || d.op === 'edit') {
    if (d.op === 'edit' && !s) return;
    return openModal({title: d.op === 'new' ? '新建备份存储' : `编辑备份存储 · ${esc(s.name)}`, wide: true, code: 'archive.store_edit', ok: '保存', body: storeFormHtml(d.op === 'new' ? null : s),
      onOk() {
        const type = $('#stType').disabled ? s.type : val('stType');
        const n = {name: val('stName').trim(), type, path: val('stPath').trim(), endpoint: val('stEndpoint').trim().replace(/\/+$/, ''), region: val('stRegion').trim(), bucket: val('stBucket').trim(), prefix: val('stPrefix').trim().replace(/^\/+|\/+$/g, ''),
          ak: val('stAk').trim(), sk: val('stSk'), pathStyle: !!($('#stPS') && $('#stPS').checked), keep: Number(val('stKeep'))};
        const e = checkStore(n, s); if (e.length) { toast(e[0]); return false; }
        const fs = isFsT(n.type);
        if (d.op === 'new') {
          const ns = {id: ++STORE_ID, name: n.name, type: n.type, path: fs ? n.path : '', endpoint: fs ? '' : n.endpoint, region: fs ? '' : n.region, bucket: fs ? '' : n.bucket, prefix: n.prefix, ak: fs ? '' : n.ak, skSet: !fs && !!n.sk, pathStyle: !fs && n.pathStyle, keep: n.keep,
            enabled: !$('#stEn') || $('#stEn').checked, st: 'unknown', free: null, lastTest: null};
          STORES.push(ns); aud('存储-新建', `新建备份存储「${ns.name}」（${ns.type}）`, '成功', STORE_T[ns.type]);
          toast('已新建备份存储，状态“未测试”：请点“测试连接”确认可用');
        } else {
          const ch = []; [['name', '名称'], ['type', '类型'], ['path', '目录'], ['endpoint', 'Endpoint'], ['bucket', '桶'], ['prefix', '路径前缀'], ['keep', '保留天数']].forEach(([k, nm]) => { if (String(s[k]) !== String(n[k]) && !(fs && ['endpoint', 'bucket'].includes(k)) && !(!fs && k === 'path')) ch.push(`${nm} ${s[k]} → ${n[k]}`); });
          if (!fs && n.ak !== s.ak) ch.push('AK 已修改'); if (n.sk) ch.push('SK 已更新');
          Object.assign(s, {name: n.name, type: n.type, path: fs ? n.path : '', endpoint: fs ? '' : n.endpoint, region: fs ? '' : n.region, bucket: fs ? '' : n.bucket, prefix: n.prefix, ak: fs ? '' : n.ak, pathStyle: !fs && n.pathStyle, keep: n.keep});
          s.skSet = fs ? false : (n.sk ? true : s.skSet);
          s.st = 'unknown'; s.lastTest = null;      // 配置变了，之前的测试结果不再可信
          aud('存储-修改', `修改备份存储「${s.name}」（${s.type}）${n.sk ? '，更新了 SK' : ''}${ch.length ? '：' + ch.join('；') : ''}（AK / SK 值不记录）`, '成功', STORE_T[s.type]); toast('已保存，状态变为“未测试”：请点“测试连接”确认可用');
        }
        afterWrite();
      }});
  }
  if (!s) return;
  if (d.op === 'test') {
    toast('正在测试连接…（写入、读取、删除探测对象）');
    return setTimeout(() => {
      const p = storeProbe(s); s.lastTest = {at: nowS().slice(0, 16), ok: p.ok, ms: p.ms, msg: p.msg}; s.st = p.ok ? 'ok' : 'err'; if (p.ok && isFsT(s.type) && s.free == null) s.free = 2 * TB_;
      aud('存储-测试', `测试备份存储「${s.name}」连接：${p.ok ? '成功' : '失败（' + p.msg + '）'}`, p.ok ? '成功' : '失败', STORE_T[s.type]);
      openModal({title: `测试连接 · ${esc(s.name)}`, ok: '', cancel: '关闭', code: 'archive.store_edit', body: `<div class="notice ${p.ok ? 'ok' : 'bad'}"><span>${p.ok ? '✓' : '✕'}</span><div><b>${p.ok ? '连接成功' : p.kind === 'timeout' ? '连接超时' : '连接失败'}</b><br>${esc(p.msg)}</div></div>
        ${kvList([['存储', esc(s.name)], ['耗时', p.ms ? p.ms + ' ms' : '—'], ['可用空间', isFsT(s.type) && p.ok && s.free != null ? fmtBytes(s.free) : '—（对象存储不限或无法得知）']])}`});
      afterWrite();
    }, 500);
  }
  if (d.op === 'toggle') {
    const act = () => { s.enabled = !s.enabled; aud(s.enabled ? '存储-启用' : '存储-停用', `${s.enabled ? '启用' : '停用'}备份存储「${s.name}」`, '成功', STORE_T[s.type]); toast(s.enabled ? '已启用' : '已停用'); afterWrite(); };
    if (s.enabled && storeUse(s.id)) return risky('archive.store_edit', '停用备份存储', `「${esc(s.name)}」被 <b>${storeUse(s.id)}</b> 个任务引用，停用后这些任务将无法运行。`, '', act);
    return act();
  }
  if (d.op === 'del') {
    const used = TASKS.filter(t => t.bk.file.on && t.bk.file.store === s.id);
    if (used.length) return toast(`该存储被 ${used.length} 个任务引用（${used.slice(0, 2).map(t => t.name).join('、')}${used.length > 2 ? '…' : ''}），不能删除；可先改任务的备份存储，或停用该存储`);
    risky('archive.store_edit', '删除备份存储', `删除「${esc(s.name)}」的配置（不会删除对象存储里已有的备份文件）。`, '', () => { STORES.splice(STORES.indexOf(s), 1); aud('存储-删除', `删除备份存储「${s.name}」`, '成功', STORE_T[s.type]); toast('已删除'); afterWrite(); }, s.name);
  }
};

/* ---------- 安全策略 ---------- */
const patList = s => String(s || '').split(/[\s,，;；]+/).map(x => x.trim()).filter(Boolean);
const PAT_RE = /^[A-Za-z0-9_$.*?\-]{1,128}$/;
/* 差异与风险（policy.go diffPolicy）：降低最小保留天数、打开不备份清理、提高单次行数 / 并发、缩小受保护范围算“放宽”；各项默认值的变化不算放宽 */
function diffPolicy(o, n) {
  const ch = [], risks = [], yn = b => b ? '允许' : '不允许', sorted = l => l.length ? l.slice().sort().join(' ') : '（空）';
  const add = (label, from, to, risk, msg) => { if (String(from) === String(to)) return; ch.push({label, from: String(from), to: String(to), risk: !!risk}); if (risk && msg) risks.push(msg); };
  add('最小保留天数', o.minRetain, n.minRetain, n.minRetain < o.minRetain, `最小保留天数降低到 ${n.minRetain} 天：将允许归档最近 ${n.minRetain} 天内的数据`);
  add('允许不备份清理', yn(o.allowNoBackup), yn(n.allowNoBackup), n.allowNoBackup && !o.allowNoBackup, '打开“允许不备份清理”：任务可以不备份直接删除数据，删除后无法恢复');
  add('单次运行最大行数', o.maxRows, n.maxRows, n.maxRows > o.maxRows, '提高单次运行最大行数：单次运行的影响范围变大');
  add('每实例最大并发运行', o.maxConc, n.maxConc, n.maxConc > o.maxConc, '提高每实例并发上限：同一实例上同时归档的任务变多，注意负载');
  const nn = new Set(n.protectedPatterns.map(x => x.toLowerCase()));
  add('受保护库表', sorted(o.protectedPatterns), sorted(n.protectedPatterns), o.protectedPatterns.some(x => !nn.has(x.toLowerCase())), '缩小了受保护库表范围：被移出名单的库表将允许创建归档任务');
  add('默认批量', o.batch, n.batch); add('默认批间休眠(ms)', o.sleep, n.sleep); add('Threads_running 阈值', o.threads, n.threads); add('复制延迟阈值(秒)', o.lag, n.lag); add('磁盘剩余阈值(%)', o.disk, n.disk);
  add('批耗时目标(ms)', o.targetMs, n.targetMs); add('负载等待上限(秒)', o.maxWait * 60, n.maxWait * 60); add('连续失败暂停阈值', o.fails, n.fails);
  return {ch, risks};
}
const polText = p => POL_META.map(([k, nm, u]) => `${nm}：${Array.isArray(p[k]) ? (p[k].join('、') || '（空）') : typeof p[k] === 'boolean' ? (p[k] ? '允许' : '不允许') : p[k] + (u ? ' ' + u : '')}`).join('\n');
ACTIONS['archive.policy_edit'] = () => {
  const p = POLICY, n = (id, v, [min, max], w) => `<input type="number" id="${id}" min="${min}" max="${max}" step="1" value="${esc(v)}" style="max-width:${w || 140}px">`;
  openModal({title: '编辑安全策略', wide: true, code: 'archive.policy_edit', ok: '下一步：查看差异', body: `<div class="form" style="grid-template-columns:200px minmax(0,1fr)">
    ${fld('最小保留天数', n('pl_minRetain', p.minRetain, [1, LIM.retainMax]) + ' 天')}${fld('默认批量（行）', n('pl_batch', p.batch, LIM.batch))}${fld('默认批间休眠（ms）', n('pl_sleep', p.sleep, [LIM.sleepDel, LIM.sleep[1]]))}
    ${fld('Threads_running 阈值', n('pl_threads', p.threads, LIM.threads))}${fld('复制延迟阈值（秒）', n('pl_lag', p.lag, LIM.lag))}${fld('磁盘剩余阈值（%）', n('pl_disk', p.disk, LIM.disk))}${fld('负载最长等待（分钟）', n('pl_maxWait', p.maxWait, LIM.maxWait))}${fld('批耗时目标（ms）', n('pl_targetMs', p.targetMs, LIM.targetMs))}${fld('连续失败暂停（批）', n('pl_fails', p.fails, LIM.fails))}
    ${fld('单次运行最大行数', n('pl_maxRows', p.maxRows, LIM.polRows, 180))}${fld('每实例最大并发运行', n('pl_maxConc', p.maxConc, LIM.polConc))}
    ${fld('受保护库表（每行一个：库名 或 库.表）', `<textarea id="pl_pats" rows="4" aria-label="受保护库表">${esc(p.protectedPatterns.join('\n'))}</textarea>`)}
    ${fld('允许不备份清理', `<label class="chk"><input type="checkbox" id="pl_nb" ${p.allowNoBackup ? 'checked' : ''}> 允许“仅清理”模式（不备份直接删除）</label>`)}</div>`,
    onOk() {
      const g = id => Number(val(id)), pats = patList(val('pl_pats'));
      const np = {minRetain: g('pl_minRetain'), batch: g('pl_batch'), sleep: g('pl_sleep'), threads: g('pl_threads'), lag: g('pl_lag'), disk: g('pl_disk'), maxWait: g('pl_maxWait'), targetMs: g('pl_targetMs'), fails: g('pl_fails'), maxRows: g('pl_maxRows'), maxConc: g('pl_maxConc'),
        protectedPatterns: pats, allowNoBackup: $('#pl_nb').checked};
      const rg = [['minRetain', 1, LIM.retainMax, '最小保留天数'], ['batch', LIM.batch[0], LIM.batch[1], '默认批量'], ['sleep', LIM.sleepDel, LIM.sleep[1], '默认批间休眠'], ['threads', LIM.threads[0], LIM.threads[1], 'Threads_running 阈值'], ['lag', LIM.lag[0], LIM.lag[1], '复制延迟阈值'],
        ['disk', LIM.disk[0], LIM.disk[1], '磁盘剩余阈值'], ['maxWait', LIM.maxWait[0], LIM.maxWait[1], '负载最长等待（分钟）'], ['targetMs', LIM.targetMs[0], LIM.targetMs[1], '批耗时目标'], ['fails', LIM.fails[0], LIM.fails[1], '连续失败暂停阈值'],
        ['maxRows', LIM.polRows[0], LIM.polRows[1], '单次运行最大行数'], ['maxConc', LIM.polConc[0], LIM.polConc[1], '每实例最大并发运行']];
      for (const [k, lo, hi, nm] of rg) if (!Number.isInteger(np[k]) || np[k] < lo || np[k] > hi) { toast(`${nm}需要在 ${nf(lo)}–${nf(hi)} 之间的整数`); return false; }
      const badPat = pats.find(x => !PAT_RE.test(x)); if (badPat) { toast(`受保护库表模式「${badPat}」不合法：只允许字母、数字、_ $ . - 与通配符 * ?，格式为 库 或 库.表`); return false; }
      np.protectedPatterns = withBuiltin(pats);      // 内置系统库无论怎么填都会补回来
      const old = {}; POL_META.forEach(([k]) => { old[k] = Array.isArray(POLICY[k]) ? POLICY[k].slice() : POLICY[k]; });
      const df = diffPolicy(old, np);
      if (!df.ch.length) { toast('策略没有变化'); return false; }
      openModal({title: '⚠ 确认修改安全策略', wide: true, okCls: 'danger primary', ok: '确认修改', code: 'archive.policy_edit',
        body: `<div class="notice bad"><span>⚠</span><div>以下 <b>${df.ch.length}</b> 项将被修改，<b>立即生效</b>并写入系统审计。</div></div>
          ${tbl(['项目', '原值', '新值'], df.ch, {cells: c => [esc(c.label) + (c.risk ? ' <span class="tag warn">放宽</span>' : ''), esc(c.from), esc(c.to)]})}
          ${df.risks.length ? `<h4 style="margin:12px 0 4px">风险提示</h4><ul style="margin:0;padding-left:18px">${df.risks.map(x => `<li>${esc(x)}</li>`).join('')}</ul>` : ''}`,
        onOk() {
          Object.assign(POLICY, np, {by: ME.login, at: nowS().slice(0, 16)});
          aud('策略修改', df.ch.map(c => `${c.label}：${c.from} → ${c.to}`).join('；'), '成功', `${df.ch.length} 项`);
          toast('安全策略已更新，新建 / 编辑向导即刻按新策略校验'); afterWrite();
        }});
      return;
    }});
};

/* ============================================================================
   页面入口
   ========================================================================== */
const tabsFor = () => [['tasks', '归档任务'], ['runs', '运行记录']].concat(can('archive.cfg_view') ? [['stores', '备份存储'], ['policy', '安全策略']] : []);
pages.archive = {
  title: '数据归档',
  render(sub) {
    stopTimer();
    let cur = String(sub || '').split('/')[0] || 'tasks';
    if (cur === 'wizard') return wizardPage();
    if (!tabsFor().some(x => x[0] === cur)) cur = 'tasks';
    AR.tab = cur; AR.dr = null;
    return `<div id="arView">${tabs('archive', tabsFor(), cur)}<div id="arBody">${bodyOf(cur)}</div></div>`;
  },
  init() { ensureTimer(); }
};
window.addEventListener('hashchange', () => { if (!/^#archive(\/|$)/.test(location.hash)) stopTimer(); });
window.ARCHIVE_DEMO = {TASKS, RUNS, SIMS, STORES, POLICY, AR, get W() { return W; }, get timer() { return TIMER; }, startRun, stepSim, finishSim, startBlock, taskOf, runOf, dryCompute, errsStep, normalizeExtra, sqlPreview, schedTimes, parseCron, canApprove, blockReason, simMs, tstr};
})();
