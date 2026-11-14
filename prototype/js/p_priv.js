/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_priv.js
 * @time: 2026/10/28
 */
/* ============================================================================
   p_priv.js —— 数据导出（export）/ 权限管理=查询权限申请与审批（qapply）/ 查询权限（qprivs）
   规格：docs/user-guide.md 第 2、4 节与真实实现。
   导出：window.APPLIES / applyPendingFor / applyPendingList / QPRIVS / EXPORTS / exportPendingList
   说明：导出工单的审批 / 执行 / 终止 / 批量重试沿用「SQL 上线」的权限码（sqlworkflow.*），
        这些码的 ACTIONS 归 p_sql.js 所有，所以本页用本地按钮 PB() + ON.* 处理，不注册同名 ACTIONS。
   ========================================================================== */
(function () {
  'use strict';

  /* ---------------------------------------------------------------- 公共小工具 */
  const hashStr = s => { let h = 2166136261; for (const c of String(s)) { h ^= c.charCodeAt(0); h = Math.imul(h, 16777619) >>> 0; } return h >>> 0; };
  const nowT = () => TODAY + ' ' + now().slice(11);                 // 原型“当前时间”：日期固定为演示日
  const userOf = l => USERS.find(u => u.login === l) || {login: l, name: l, dept: '—', groups: [], rgs: [], extraGrant: [], extraDeny: []};
  const uTxt = l => `<span title="${esc(userOf(l).name)}">${esc(l)}</span>`;
  const asLogin = u => typeof u === 'string' ? u : (u && u.login) || '';
  const numFmt = n => Number(n).toLocaleString('en-US');
  const isDate = s => /^\d{4}-\d{2}-\d{2}$/.test(s) && !isNaN(new Date(s + 'T00:00:00'));
  const daysLeft = d => Math.round((new Date(d + 'T00:00:00') - new Date(TODAY + 'T00:00:00')) / 86400000);
  const lg = (o, who, text) => o.logs.push({at: nowT(), who, text});

  /* 某用户是否持有某权限码（原型里 can() 只能算当前用户，待办角标需要算别人） */
  function hasCode(u, code) {
    if (!BTNS[code]) return false;
    if (u.super) return true;
    if (BTNS[code].locked) return false;
    const s = new Set();
    u.groups.forEach(gid => { const g = GROUPS.find(x => x.id === gid); if (g) g.perms.forEach(p => s.add(p)); });
    (u.extraGrant || []).forEach(c => s.add(c)); (u.extraDeny || []).forEach(c => s.delete(c));
    return s.has(code);
  }
  /* 本地权限按钮：渲染同 B()，但点击走 data-on（因为 sqlworkflow.* 的 ACTIONS 归 p_sql.js） */
  function PB(code, label, on, data = {}, cls = '') {
    const meta = BTNS[code];
    if (!can(code)) {
      if (SETTINGS.noPermBtn === 'hide') return '';
      return `<button class="btn ${cls} noperm" disabled title="无权限：需要「${esc(meta.label)}」（${code}）">${label}</button>`;
    }
    const d = Object.entries(data).map(([k, v]) => ` data-${k}="${esc(v)}"`).join('');
    return `<button class="btn ${cls}" data-on="${on}"${d} title="${esc(meta.label + (meta.kind === 'd' ? '（高危）' : ''))}">${label}</button>`;
  }
  function guard(code) {
    if (can(code)) return true;
    audit(code, '尝试操作：' + BTNS[code].label, '拒绝（无权限）'); toast('无权限'); return false;
  }

  /* ---------------------------------------------------------------- 审批流（与资源组绑定，导出与权限申请共用） */
  const LV = {tl: {name: '组长', gid: 'g_tl'}, dba: {name: 'DBA', gid: 'g_dba'}};
  const FLOWS = {1: [LV.tl, LV.dba], 2: [LV.tl, LV.dba], 3: [LV.dba], 4: [LV.dba]};   // 3/4 组长本身就是 DBA，只有一级
  const flowOf = rg => FLOWS[rg] || [LV.dba];
  const flowText = rg => flowOf(rg).map(l => l.name).join(' → ');
  const eligible = (u, lv, rg) => !!lv && (u.super || (u.groups.includes(lv.gid) && u.rgs.includes(rg)));
  const candidates = (lv, rg, excl) => USERS.filter(u => u.status === 'active' && !u.super && u.login !== excl && u.groups.includes(lv.gid) && u.rgs.includes(rg));
  const candTxt = (lv, rg, excl) => { const c = candidates(lv, rg, excl); return c.length ? c.map(u => `${u.name}（${u.login}）`).join('、') : '管理员'; };
  const rgOfInst = (inst, u) => inst.rgs.find(r => u.rgs.includes(r)) || inst.rgs[0] || 1;
  const botOf = rg => (RGROUPS.find(r => r.id === rg) || {}).bot || '钉钉';
  const notifyTxt = (rg, who) => `已通过${botOf(rg)}机器人通知 ${who}`;
  /* 审批时间线：w 需含 created / rg / level / approvals；pending 表示仍在审批中 */
  function flowTl(w, who, pending) {
    const items = [{cls: 'ok', h: `提交 · ${esc(userOf(who).name)}（${esc(who)}）`, m: esc(w.created)}];
    flowOf(w.rg).forEach((l, i) => {
      const ap = w.approvals[i];
      if (ap) items.push({cls: ap.act === '通过' ? 'ok' : 'bad', h: `${l.name}审批 · ${ap.act}`, m: `${esc(ap.by)} · ${esc(ap.at)}${ap.note ? '　备注：' + esc(ap.note) : ''}`});
      else if (pending && i === w.level) items.push({cls: 'cur', h: `${l.name}审批 · 审批中`, m: `候选审批人：${esc(candTxt(l, w.rg, who))}`});
      else items.push({cls: 'skip', h: `${l.name}审批`, m: pending ? '待上一级通过' : '流程已结束，未到达'});
    });
    return items;
  }
  const logHtml = logs => `<div class="tl">${logs.slice().reverse().map(l => `<div class="it ok"><div class="h">${esc(l.text)}</div><div class="m">${esc(l.at)} · ${esc(l.who)}</div></div>`).join('')}</div>`;

  /* ================================================================ 一、查询权限（QPRIVS）—— 先于其他页面初始化 */
  const QPRIVS = [
    {id: 1, user: 'rd_zhao', inst: 1, db: 'order_db', tables: ['*'], limit: 1000, expire: '2026-11-30', src: '申请 #36'},
    {id: 2, user: 'rd_zhao', inst: 1, db: 'order_archive', tables: ['orders_2025'], limit: 200, expire: '2026-10-12', src: '申请 #38'},
    {id: 3, user: 'rd_zhao', inst: 7, db: 'order_db', tables: ['*'], limit: 5000, expire: '2027-03-31', src: '管理员直接授予'},
    {id: 4, user: 'tl_wang', inst: 3, db: 'pay_db', tables: ['payments', 'refunds'], limit: 2000, expire: '2026-11-20', src: '申请 #39'},
    {id: 5, user: 'tl_wang', inst: 1, db: 'order_db', tables: ['*'], limit: 5000, expire: '2026-12-31', src: '管理员直接授予'},
    {id: 6, user: 'qa_sun', inst: 1, db: 'order_db', tables: ['orders', 'order_items'], limit: 500, expire: '2026-10-30', src: '申请 #40'},
    {id: 7, user: 'qa_sun', inst: 4, db: 'user_db', tables: ['users'], limit: 100, expire: '2026-10-10', src: '申请 #37'},
    {id: 8, user: 'qa_sun', inst: 7, db: 'order_db', tables: ['*'], limit: 2000, expire: '2027-01-15', src: '管理员直接授予'},
    {id: 9, user: 'rd_zhou', inst: 7, db: 'order_db', tables: ['*'], limit: 2000, expire: '2026-12-31', src: '管理员直接授予'},
    {id: 10, user: 'dev_liu', inst: 4, db: 'user_db', tables: ['*'], limit: 1000, expire: '2026-09-30', src: '申请 #32'},
    {id: 11, user: 'rd_zhao', inst: 1, db: 'order_db', tables: ['orders'], limit: 100, expire: '2026-09-20', src: '申请 #31'},
    {id: 12, user: 'rd_zhao', inst: 11, db: 'db0', tables: ['*'], limit: 500, expire: '2026-12-31', src: '管理员直接授予'},
    {id: 13, user: 'qa_sun', inst: 13, db: 'db1', tables: ['*'], limit: 200, expire: '2026-10-12', src: '管理员直接授予'},
    {id: 14, user: 'rd_zhao', inst: 12, db: 'db0', tables: ['*'], limit: 200, expire: '2026-09-25', src: '管理员直接授予'},
    /* PostgreSQL / MongoDB / ClickHouse：库级（'*'）与表级（PG 为 schema.table，Mongo 为集合名，ClickHouse 为表名）都支持；库名 / 表名区分大小写 */
    {id: 15, user: 'rd_zhao', inst: 14, db: 'order_core', tables: ['public.orders', 'public.order_items'], limit: 500, expire: '2026-12-31', src: '申请 #46'},
    {id: 16, user: 'rd_zhao', inst: 16, db: 'order_mongo', tables: ['orders'], limit: 200, expire: '2026-11-30', src: '申请 #47'},
    {id: 17, user: 'rd_zhao', inst: 18, db: 'olap_order', tables: ['*'], limit: 1000, expire: '2026-12-31', src: '管理员直接授予'},
    {id: 18, user: 'qa_sun', inst: 17, db: 'user_mongo', tables: ['*'], limit: 100, expire: '2026-10-12', src: '管理员直接授予'},
    {id: 19, user: 'rd_zhao', inst: 14, db: 'order_report', tables: ['*'], limit: 300, expire: '2026-09-28', src: '管理员直接授予'},
    {id: 20, user: 'tl_wang', inst: 14, db: 'order_core', tables: ['*'], limit: 2000, expire: '2027-03-31', src: '管理员直接授予'},
    {id: 21, user: 'tl_wang', inst: 18, db: 'olap_order', tables: ['fact_orders', 'v_order_daily'], limit: 2000, expire: '2027-03-31', src: '管理员直接授予'},
    {id: 22, user: 'qa_sun', inst: 16, db: 'order_mongo', tables: ['orders', 'order_events'], limit: 500, expire: '2026-10-30', src: '管理员直接授予'},
  ];
  window.QPRIVS = QPRIVS;
  const nextQid = () => Math.max(0, ...QPRIVS.map(p => p.id)) + 1;
  const privState = p => { const d = daysLeft(p.expire); return d < 0 ? ['bad', '已过期'] : d <= 7 ? ['warn', '即将到期'] : ['ok', '有效']; };
  const tblTxt = t => t.includes('*') ? '（全库）' : t.join('、');
  const isRds = id => instOf(id).type === 'Redis';
  const isExt = id => isExtInst(instOf(id));
  const pDbs = id => isRds(id) && window.RedisQuery ? RedisQuery.dbs(instOf(id)).map(d => ({db: 'db' + d.db})) : dbsOf(id);   // Redis：库 = db 编号（db0..db15），没有表

  /* ================================================================ 二、权限申请（APPLIES） */
  const AP_ST = {'待审批': 'warn', '审批通过': 'ok', '审批驳回': 'bad', '已撤回': '', '已过期': ''};
  const APPLIES = [];
  (function seedApplies() {
    const A = [
      {id: 48, user: 'qa_sun', inst: 18, db: 'olap_order', tables: ['fact_orders'], limit: 500, expire: '2026-10-21', created: '2026-10-07 09:58', status: '待审批', level: 0, approvals: [], reason: '回归测试：核对分析库里订单事实表的金额汇总与线上订单库是否一致。'},
      {id: 47, user: 'rd_zhao', inst: 16, db: 'order_mongo', tables: ['orders'], limit: 200, expire: '2026-11-30', created: '2026-10-02 11:05', status: '审批通过', level: 2, approvals: [{by: 'tl_wang', at: '2026-10-02 11:30', act: '通过', note: ''}, {by: 'dba_li', at: '2026-10-02 14:40', act: '通过', note: '仅 orders 集合，文档不做脱敏，注意不要导出'}], reason: '客诉：按用户查订单文档并核对嵌套明细（items）与收货地址。', grantedId: 16},
      {id: 46, user: 'rd_zhao', inst: 14, db: 'order_core', tables: ['public.orders', 'public.order_items'], limit: 500, expire: '2026-12-31', created: '2026-09-28 10:20', status: '审批通过', level: 2, approvals: [{by: 'tl_wang', at: '2026-09-28 10:50', act: '通过', note: ''}, {by: 'dba_li', at: '2026-09-28 11:35', act: '通过', note: ''}], reason: '订单中心 PostgreSQL 从迁移后日常排查：需要 orders 与 order_items 两张表只读。', grantedId: 15},
      {id: 44, user: 'rd_zhou', inst: 3, db: 'pay_db', tables: ['payments'], limit: 10000, expire: '2026-10-13', created: '2026-10-06 18:20', status: '待审批', level: 0, approvals: [], reason: '核对支付渠道回调丢单：需按 pay_no 抽查 10 月 1–5 日的 payments 记录，预计 7 天内完成。'},
      {id: 43, user: 'rd_zhao', inst: 2, db: 'order_db', tables: ['orders', 'order_items'], limit: 500, expire: '2026-11-06', created: '2026-10-07 09:40', status: '待审批', level: 1, approvals: [{by: 'tl_wang', at: '2026-10-07 10:05', act: '通过', note: '同意，仅限从库排查'}], reason: '线上订单金额与明细对不上，需要在从库只读排查（不影响主库）。'},
      {id: 42, user: 'qa_sun', inst: 4, db: 'user_db', tables: ['users', 'login_log'], limit: 200, expire: '2026-10-21', created: '2026-10-07 09:05', status: '待审批', level: 0, approvals: [], reason: '回归测试：验证会员等级迁移后的用户数据与登录日志是否一致。'},
      {id: 41, user: 'tl_wang', inst: 3, db: 'pay_db', tables: ['bank_cards'], limit: 100, expire: '2026-10-14', created: '2026-10-06 14:12', status: '待审批', level: 0, approvals: [], reason: '处理用户投诉：核对绑卡记录（仅查看卡种与绑定时间）。'},
      {id: 40, user: 'qa_sun', inst: 1, db: 'order_db', tables: ['orders', 'order_items'], limit: 500, expire: '2026-10-30', created: '2026-10-02 10:30', status: '审批通过', level: 2, approvals: [{by: 'tl_wang', at: '2026-10-02 11:20', act: '通过', note: ''}, {by: 'dba_li', at: '2026-10-02 14:05', act: '通过', note: ''}], reason: '双十一大促前订单链路回归，需要对照线上数据核对金额计算。', grantedId: 6},
      {id: 39, user: 'tl_wang', inst: 3, db: 'pay_db', tables: ['payments', 'refunds'], limit: 2000, expire: '2026-11-20', created: '2026-09-28 11:00', status: '审批通过', level: 2, approvals: [{by: 'admin', at: '2026-09-28 11:30', act: '通过', note: '组长本人申请，由管理员代审'}, {by: 'dba_li', at: '2026-09-28 15:12', act: '通过', note: ''}], reason: '支付中心月度对账，需要核对支付单与退款单。', grantedId: 4},
      {id: 38, user: 'rd_zhao', inst: 1, db: 'order_archive', tables: ['orders_2025'], limit: 200, expire: '2026-10-12', created: '2026-09-30 15:20', status: '审批通过', level: 2, approvals: [{by: 'tl_wang', at: '2026-09-30 16:01', act: '通过', note: ''}, {by: 'dba_li', at: '2026-10-01 09:30', act: '通过', note: '归档库仅限 10 天'}], reason: '客诉：查询 2025 年历史订单，需要翻归档表。', grantedId: 2},
      {id: 37, user: 'qa_sun', inst: 4, db: 'user_db', tables: ['users'], limit: 100, expire: '2026-10-10', created: '2026-09-26 16:00', status: '审批通过', level: 1, approvals: [{by: 'dba_li', at: '2026-09-26 17:40', act: '通过', note: ''}], reason: '会员系统冒烟测试，核对注册用户的等级字段。', grantedId: 7},
      {id: 36, user: 'rd_zhao', inst: 1, db: 'order_db', tables: ['*'], limit: 1000, expire: '2026-11-30', created: '2026-09-15 09:50', status: '审批通过', level: 2, approvals: [{by: 'tl_wang', at: '2026-09-15 10:20', act: '通过', note: ''}, {by: 'dba_li', at: '2026-09-15 11:02', act: '通过', note: ''}], reason: '订单中心日常排查线上问题，需要 order_db 全库只读。', grantedId: 1},
      {id: 35, user: 'qa_sun', inst: 4, db: 'user_db', tables: ['user_profile'], limit: 100, expire: '2026-10-17', created: '2026-10-03 10:00', status: '审批驳回', level: 0, approvals: [{by: 'dba_li', at: '2026-10-03 11:15', act: '驳回', note: '包含身份证号等敏感字段，请改用脱敏视图，或申请 users 表'}], reason: '测试用户资料页展示，需要核对真实姓名与生日字段。'},
      {id: 34, user: 'rd_zhao', inst: 2, db: 'order_db', tables: ['*'], limit: 5000, expire: '2026-11-03', created: '2026-09-29 14:08', status: '审批驳回', level: 1, approvals: [{by: 'tl_wang', at: '2026-09-29 14:50', act: '通过', note: ''}, {by: 'dba_li', at: '2026-09-29 16:22', act: '驳回', note: '整库且 5000 行偏大，请缩小到所需表，行数不超过 1000'}], reason: '需要在从库做多表关联的对账查询。'},
      {id: 33, user: 'rd_zhou', inst: 3, db: 'pay_db', tables: ['refunds'], limit: 1000, expire: '2026-10-15', created: '2026-10-01 17:40', status: '已撤回', level: 0, approvals: [], endAt: '2026-10-01 18:02', reason: '核对退款流水（需求变更，已撤回）。'},
      {id: 32, user: 'dev_liu', inst: 4, db: 'user_db', tables: ['*'], limit: 1000, expire: '2026-09-30', created: '2026-08-20 10:12', status: '已过期', level: 1, approvals: [{by: 'dba_li', at: '2026-08-20 11:00', act: '通过', note: ''}], reason: '用户中心登录异常排查，需要 user_db 全库只读。', grantedId: 10},
      {id: 31, user: 'rd_zhao', inst: 1, db: 'order_db', tables: ['orders'], limit: 100, expire: '2026-09-20', created: '2026-09-06 09:30', status: '已过期', level: 2, approvals: [{by: 'tl_wang', at: '2026-09-06 10:02', act: '通过', note: ''}, {by: 'dba_li', at: '2026-09-06 10:40', act: '通过', note: ''}], reason: '排查 9 月初的订单状态异常。', grantedId: 11},
    ];
    A.forEach(a => {
      a.rg = rgOfInst(instOf(a.inst), userOf(a.user));
      a.approvals.forEach((p, i) => { p.lv = (flowOf(a.rg)[i] || LV.dba).name; });
      a.logs = [{at: a.created, who: a.user, text: `提交申请，等待审批，审批流程：${flowText(a.rg)}`}];
      a.approvals.forEach(p => a.logs.push({at: p.at, who: p.by, text: `${p.lv}${p.act}${p.note ? '：' + p.note : ''}`}));
      if (a.grantedId) a.logs.push({at: a.approvals[a.approvals.length - 1].at, who: '系统', text: `审批通过，已自动授予查询权限 #${a.grantedId}`});
      if (a.status === '已过期') a.logs.push({at: a.expire + ' 23:59', who: '系统', text: '权限已到期失效'});
      if (a.status === '已撤回') a.logs.push({at: a.endAt, who: a.user, text: '申请人撤回申请'});
      APPLIES.push(a);
    });
  })();
  window.APPLIES = APPLIES;
  const nextAid = () => Math.max(0, ...APPLIES.map(a => a.id)) + 1;

  function canReviewBy(u, a) {
    if (a.status !== '待审批' || a.user === u.login) return false;
    return hasCode(u, 'qpriv.review') && eligible(u, flowOf(a.rg)[a.level], a.rg);
  }
  window.applyPendingList = user => { const u = typeof user === 'string' ? USERS.find(x => x.login === user) : user; return u ? APPLIES.filter(a => canReviewBy(u, a)) : []; };
  window.applyPendingFor = user => window.applyPendingList(user).length;
  NAV.qapply.badge = () => window.applyPendingFor(ME);

  /* ================================================================ 三、数据导出（EXPORTS） */
  const MAXROWS = 10000;
  const FMT_EXT = {csv: 'csv', xlsx: 'xlsx', json: 'json', xml: 'xml', sql: 'sql'};
  const FMT = {csv: 'CSV', xlsx: 'Excel（xlsx）', json: 'JSON', xml: 'XML', sql: 'SQL（INSERT 语句）'};
  const EX_ST = {'等待审批': 'warn', '审核通过': 'info', '执行中': 'brand', '已正常结束': 'ok', '执行有异常': 'bad', '人工终止': ''};
  const rand8 = seed => hashStr(seed).toString(16).padStart(8, '0');
  const stamp = () => (TODAY + now().slice(10)).replace(/[-: ]/g, '') + pad2(new Date().getSeconds());
  const fmtSz = kb => kb >= 1024 ? (kb / 1024).toFixed(1) + ' MB' : Math.max(1, Math.round(kb)) + ' KB';

  const EXPORTS = [];
  (function seedExports() {
    const S_ORD = "SELECT id, order_no, user_id, amount, status, created_at\nFROM orders\nWHERE created_at >= '2026-09-01' AND created_at < '2026-10-01'\nORDER BY id";
    const ok = (inst, rows, secs, file) => ({inst, st: 'ok', rows, est: rows, secs, msg: '导出文件: ' + file});
    const X = [
      {id: 1047, name: '导出 9 月订单对账数据', submitter: 'rd_zhao', inst: 1, db: 'order_db', fmt: 'csv', sql: S_ORD, rows: 3842, created: '2026-10-07 10:12', status: '等待审批', level: 0, approvals: []},
      {id: 1046, name: '测试环境订单明细抽样', submitter: 'qa_sun', inst: 7, db: 'order_db', fmt: 'xlsx', sql: "SELECT id, order_id, sku_id, sku_name, qty, price\nFROM order_items\nWHERE order_id BETWEEN 880000 AND 886000\nORDER BY id", rows: 6120, created: '2026-10-07 09:20', status: '等待审批', level: 1, approvals: [{by: 'tl_wang', at: '2026-10-07 09:48', act: '通过', note: ''}]},
      {id: 1045, name: '多库订单对账（批量）', submitter: 'admin', insts: [1, 2, 7], db: 'order_db', fmt: 'csv', sql: "SELECT id, order_no, amount, status\nFROM orders\nWHERE created_at >= '2026-10-01' AND created_at < '2026-10-07'", rowsEach: {1: 4120, 2: 3977, 7: 520}, created: '2026-10-06 17:30', status: '等待审批', level: 1, approvals: [{by: 'tl_wang', at: '2026-10-06 18:10', act: '通过', note: ''}]},
      {id: 1044, name: '导出已取消订单（JSON）', submitter: 'rd_zhao', inst: 1, db: 'order_db', fmt: 'json', sql: "SELECT id, order_no, user_id, amount, created_at\nFROM orders\nWHERE status = 4 AND created_at >= '2026-10-01' AND created_at < '2026-10-07'\nORDER BY id", rows: 2210, created: '2026-10-06 16:02', status: '审核通过', level: 2, sched: '2026-10-07 22:30', approvals: [{by: 'tl_wang', at: '2026-10-06 16:30', act: '通过', note: ''}, {by: 'dba_li', at: '2026-10-06 17:05', act: '通过', note: '低峰期执行'}]},
      {id: 1043, name: '用户订单明细（测试验收）', submitter: 'qa_sun', inst: 1, db: 'order_db', fmt: 'csv', sql: "SELECT id, order_id, sku_id, qty, price\nFROM order_items\nWHERE order_id >= 880000 AND order_id < 888000\nORDER BY id", rows: 8420, created: '2026-10-07 08:40', status: '审核通过', level: 2, approvals: [{by: 'tl_wang', at: '2026-10-07 08:55', act: '通过', note: ''}, {by: 'dba_li', at: '2026-10-07 09:20', act: '通过', note: ''}]},
      {id: 1041, name: '支付单 9 月明细', submitter: 'rd_zhou', inst: 3, db: 'pay_db', fmt: 'xlsx', sql: "SELECT id, pay_no, order_no, channel, amount, status, paid_at\nFROM payments\nWHERE paid_at >= '2026-09-25' AND paid_at < '2026-10-01'\nORDER BY id", rows: 8420, created: '2026-10-07 07:50', status: '执行中', level: 2, progress: 63, approvals: [{by: 'tl_wang', at: '2026-10-07 08:10', act: '通过', note: ''}, {by: 'dba_li', at: '2026-10-07 08:41', act: '通过', note: ''}], started: '2026-10-07 10:36'},
      {id: 1040, name: '导出 9 月上旬订单', submitter: 'rd_zhao', inst: 1, db: 'order_db', fmt: 'csv', sql: "SELECT id, order_no, user_id, amount, status, created_at\nFROM orders\nWHERE created_at >= '2026-09-01' AND created_at < '2026-09-11'\nORDER BY id", rows: 3842, created: '2026-10-06 08:40', status: '已正常结束', level: 2, approvals: [{by: 'tl_wang', at: '2026-10-06 09:02', act: '通过', note: ''}, {by: 'dba_li', at: '2026-10-06 09:30', act: '通过', note: ''}], file: {name: 'order_db_20261006084012_3fa91c2e.zip', size: '412 KB', at: '2026-10-06 09:41'}, dls: 2, secs: 1.274},
      {id: 1038, name: '9 月退款单导出', submitter: 'tl_wang', inst: 3, db: 'pay_db', fmt: 'xlsx', sql: "SELECT id, refund_no, pay_no, amount, created_at\nFROM refunds\nWHERE created_at >= '2026-09-01' AND created_at < '2026-10-01'\nORDER BY id", rows: 1560, created: '2026-10-05 15:10', status: '已正常结束', level: 2, approvals: [{by: 'admin', at: '2026-10-05 15:40', act: '通过', note: '组长本人提交，管理员代审'}, {by: 'dba_li', at: '2026-10-05 16:12', act: '通过', note: ''}], file: {name: 'pay_db_20261005162033_91d0be47.zip', size: '188 KB', at: '2026-10-05 16:20'}, dls: 1, secs: 0.886},
      {id: 1036, name: '三实例订单状态统计（批量）', submitter: 'dba_li', insts: [1, 2, 7], db: 'order_db', fmt: 'csv', sql: "SELECT status, COUNT(*) AS cnt, SUM(amount) AS gmv\nFROM orders\nGROUP BY status", rowsEach: {1: 5, 2: 5, 7: 5}, created: '2026-10-05 13:40', status: '已正常结束', level: 2, approvals: [{by: 'tl_wang', at: '2026-10-05 13:55', act: '通过', note: ''}, {by: 'admin', at: '2026-10-05 14:02', act: '通过', note: ''}], file: {name: '1036_20261005141203_8be40d17.zip', size: '3 KB', at: '2026-10-05 14:12'}, dls: 1},
      {id: 1035, name: '多实例订单抽样（批量）', submitter: 'dba_li', insts: [1, 2, 5], db: 'order_db', fmt: 'csv', sql: "SELECT id, order_no, amount, status, created_at\nFROM orders\nWHERE created_at >= '2026-10-05' AND created_at < '2026-10-06'", rowsEach: {1: 4120, 2: 3977, 5: 3902}, created: '2026-10-06 09:30', status: '执行有异常', level: 2, approvals: [{by: 'tl_wang', at: '2026-10-06 09:50', act: '通过', note: ''}, {by: 'admin', at: '2026-10-06 10:11', act: '通过', note: ''}], file: {name: '1035_20261006101803_c2a7761f.zip', size: '1.4 MB', at: '2026-10-06 10:18'}, dls: 0, fail: {5: '导出失败: 查询超时（超过 max_export_execution_time = 60 s），已 KILL QUERY'}},
      {id: 1033, name: '8 月支付渠道对账（SQL）', submitter: 'rd_zhou', inst: 3, db: 'pay_db', fmt: 'sql', sql: "SELECT id, pay_no, order_no, channel, amount, paid_at\nFROM payments\nWHERE paid_at >= '2026-08-01' AND paid_at < '2026-08-02'\nORDER BY id", rows: 5180, created: '2026-08-20 11:05', status: '已正常结束', level: 2, approvals: [{by: 'tl_wang', at: '2026-08-20 11:30', act: '通过', note: ''}, {by: 'dba_li', at: '2026-08-20 13:02', act: '通过', note: ''}], file: {name: 'pay_db_20260820131455_47be0c19.zip', size: '506 KB', at: '2026-08-20 13:14', cleaned: true}, dls: 1},
      {id: 1031, name: '全量订单导出（含手机号）', submitter: 'qa_sun', inst: 1, db: 'order_db', fmt: 'csv', sql: "SELECT id, order_no, receiver_phone, receiver_addr, amount\nFROM orders\nWHERE created_at >= '2026-09-01' AND created_at < '2026-09-30'", rows: 9600, created: '2026-10-03 14:20', status: '人工终止', level: 0, approvals: [{by: 'tl_wang', at: '2026-10-03 15:02', act: '驳回', note: '导出范围过大且包含手机号、地址，请缩小时间范围并去掉敏感列'}]},
      {id: 1030, name: '会员订单全月导出', submitter: 'rd_zhao', inst: 1, db: 'order_db', fmt: 'csv', sql: "SELECT id, order_no, user_id, amount, status\nFROM orders\nWHERE created_at >= '2026-09-20' AND created_at < '2026-09-27'", rows: 9780, created: '2026-10-02 09:12', status: '执行有异常', level: 2, approvals: [{by: 'tl_wang', at: '2026-10-02 09:40', act: '通过', note: ''}, {by: 'dba_li', at: '2026-10-02 10:05', act: '通过', note: ''}], failSingle: '执行时数据已超限：实际 12034 行 > 上限 10000，已中止，未写出任何数据'},
      {id: 1028, name: '用户注册明细', submitter: 'qa_sun', inst: 4, db: 'user_db', fmt: 'csv', sql: "SELECT id, username, level, created_at\nFROM users\nWHERE created_at >= '2026-09-29' AND created_at < '2026-10-01'\nORDER BY id", rows: 980, created: '2026-09-30 11:00', status: '已正常结束', level: 1, approvals: [{by: 'dba_li', at: '2026-09-30 11:25', act: '通过', note: ''}], file: {name: 'user_db_20260930113510_6d02f4a8.zip', size: '96 KB', at: '2026-09-30 11:35'}, dls: 1},
    ];
    X.forEach(w => {
      const insts = w.insts || [w.inst];
      w.batch = !!w.insts; w.inst = insts[0]; w.insts = insts;
      w.rg = rgOfInst(instOf(w.inst), userOf(w.submitter));
      w.approvals.forEach((p, i) => { p.lv = (flowOf(w.rg)[i] || LV.dba).name; });
      const ext = FMT_EXT[w.fmt] || w.fmt;
      w.items = insts.map(id => {
        const rows = w.batch ? w.rowsEach[id] : w.rows;
        const base = {inst: id, rows: 0, est: rows, secs: 0, msg: '', p: 0};
        if (w.status === '已正常结束' || w.status === '执行有异常') {
          const f = w.fail && w.fail[id], fs = w.failSingle;
          if (f || fs) return Object.assign(base, {st: 'fail', tried: true, msg: f || fs});
          return Object.assign(base, {st: 'ok', tried: true, rows, secs: +(0.8 + (hashStr(w.id + '' + id) % 180) / 100).toFixed(3), msg: `导出文件: ${w.batch ? instOf(id).name : w.db}.${ext}`});
        }
        if (w.status === '执行中') return Object.assign(base, {st: 'run', p: w.progress});
        return Object.assign(base, {st: 'wait'});
      });
      if (w.batch) w.rows = w.items.reduce((s, i) => s + i.est, 0);
      w.logs = [{at: w.created, who: w.submitter, text: `提交${w.batch ? '批量' : ''}导出工单，等待审批，审批流程：${flowText(w.rg)}`}];
      w.approvals.forEach(p => w.logs.push({at: p.at, who: p.by, text: `${p.lv}${p.act}${p.note ? '：' + p.note : ''}`}));
      if (w.sched) w.logs.push({at: w.approvals[w.approvals.length - 1].at, who: w.submitter, text: `设置定时导出：${w.sched}`});
      if (w.started) w.logs.push({at: w.started, who: 'dba_li', text: '开始导出'});
      if (w.file || w.failSingle) { w.logs.push({at: w.approvals.length ? w.approvals[w.approvals.length - 1].at : w.created, who: w.approvals.length ? w.approvals[w.approvals.length - 1].by : '系统', text: '开始导出'}); w.logs.push({at: w.file ? w.file.at : w.created, who: '系统', text: w.status === '已正常结束' ? `导出完成，保存文件: ${w.file.name}` : (w.failSingle ? '导出异常：' + w.failSingle : '批量导出完成，部分实例失败，已成功实例的文件仍可下载')}); }
      if (w.status === '人工终止') w.logs.push({at: w.approvals[0].at, who: w.approvals[0].by, text: '驳回，流程终止'});
      w.sched = w.sched || ''; w.progress = w.progress || 0; w.dls = w.dls || 0;
      EXPORTS.push(w);
    });
  })();
  window.EXPORTS = EXPORTS;
  const exById = id => EXPORTS.find(w => w.id == id);
  const nextEid = () => Math.max(0, ...EXPORTS.map(w => w.id)) + 3;   // 与 SQL 上线工单共用号段，所以不连号

  function canSeeEx(w) {
    if (!(w.batch ? can('export.view_batch') : can('export.view'))) return false;
    return ME.super || w.submitter === ME.login || (can('sqlworkflow.view_group') && w.rg && ME.rgs.includes(w.rg));
  }
  function canApproveEx(u, w) {
    if (w.status !== '等待审批' || w.submitter === u.login) return false;
    return hasCode(u, 'sqlworkflow.approve') && eligible(u, flowOf(w.rg)[w.level], w.rg);
  }
  window.exportPendingList = user => { const u = typeof user === 'string' ? USERS.find(x => x.login === user) : user; return u ? EXPORTS.filter(w => canApproveEx(u, w) && (w.batch ? hasCode(u, 'export.view_batch') : true)) : []; };
  const exTag = w => `<span class="tag ${EX_ST[w.status]}">${w.status}${w.status === '审核通过' && w.sched ? ' · 定时' : ''}</span>`;

  /* ---------------------------------------------------------------- 导出：语句检查 / 行数统计（模拟 COUNT） */
  function pickTable(inst, db) {
    const ts = tablesOf(inst.id, db);
    return isReadView(inst) ? ts.find(t => t.view && t.n.startsWith('v_')) : ts.find(t => !t.view);
  }
  function sampleSql(inst, db, kind = 'ok') {
    const t = pickTable(inst, db); if (!t) return 'SELECT 1';
    const mssql = inst.type === 'MsSQL', tn = (mssql ? 'dbo.' : '') + t.n;
    const cols = t.cols.filter(c => !c.mask).slice(0, 6).map(c => c.n), first = t.cols[0].n;
    const dc = t.cols.find(c => /^(datetime|date|timestamp)/.test(c.t));
    if (kind === 'bad') return `UPDATE ${tn} SET ${cols[cols.length - 1]} = NULL WHERE ${first} = 1`;
    if (kind === 'big') return `SELECT ${cols.join(', ')}\nFROM ${tn}\nORDER BY ${first}`;
    if (mssql) return `SELECT TOP 5000 ${cols.join(', ')}\nFROM ${tn}\nORDER BY ${first}`;
    return `SELECT ${cols.join(', ')}\nFROM ${tn}\n${dc ? `WHERE ${dc.n} >= '2026-09-01' AND ${dc.n} < '2026-10-01'` : `WHERE ${first} > 0\n  AND ${first} <= 5000`}\nORDER BY ${first}`;
  }
  /* 返回 {lv:'pass|warn|error', stage, msg, rows} */
  function analyze(sqlRaw, inst, db) {
    const E = (msg, stage = '规则检查未通过') => ({lv: 'error', stage, msg, rows: 0});
    let s = (sqlRaw || '').trim();
    if (!s) return E('请先填写查询语句');
    s = s.replace(/;\s*$/, '');
    const lower = s.toLowerCase().replace(/'(?:[^'\\]|\\.)*'/g, "''").replace(/--[^\n]*/g, '').replace(/\/\*[\s\S]*?\*\//g, '');
    if (lower.includes(';')) return E('只允许一条查询语句，不能用 ; 夹带第二条');
    if (!/^\s*(select|with)\b/.test(lower)) return E('只允许 SELECT / WITH 查询语句');
    const bad = lower.match(/\b(insert|update|delete|drop|alter|truncate|create|grant|revoke)\b|\breplace\s+into\b/);
    if (bad) return E(`[statement.readonly] 导出只允许只读查询，检测到 ${(bad[1] || 'REPLACE INTO').toUpperCase()}`);
    if (/into\s+(outfile|dumpfile)/.test(lower)) return E('不允许 INTO OUTFILE / DUMPFILE');
    if (/for\s+update|lock\s+in\s+share\s+mode/.test(lower)) return E('不允许 FOR UPDATE / LOCK IN SHARE MODE');
    if (/\b(load_file|sleep|benchmark)\s*\(/.test(lower)) return E('不允许使用 load_file / sleep / benchmark 等函数');
    if (/mysql\.user|mysql\.db\b/.test(lower)) return E('不允许查询 mysql.user 等系统账号表');
    if (inst.type === 'MsSQL' && (/\bexec(ute)?\b/.test(lower) || /\binto\s+[#\w\[]/.test(lower))) return E('SQL Server 导出不允许 EXEC 与 SELECT ... INTO');
    const ctes = new Set([...lower.matchAll(/(?:\bwith|,)\s*(\w+)\s+as\s*\(/g)].map(m => m[1]));
    const tbls = [...lower.matchAll(/\b(?:from|join)\s+([`"\[\]\w.]+)/g)].map(m => m[1].replace(/[`"\[\]]/g, '').split('.').pop()).filter(t => t && !ctes.has(t));
    if (!tbls.length) return E('未找到 FROM 的表');
    for (const t of tbls) if (!findTable(inst.id, db, t)) return E(`表 ${db}.${t} 不存在`, '行数统计失败');
    if (isReadView(inst)) { const nv = tbls.find(t => !t.startsWith('v_')); if (nv) return E(`实例 ${inst.name} 带 read_view 标签，只能导出 v_ 开头的视图（${nv} 不是）`); }
    const t0 = findTable(inst.id, db, tbls[0]), h = hashStr(lower + inst.id);
    const base = t0.rows || (/summary/.test(t0.n) ? 730 : 2840000);
    const where = lower.match(/\bwhere\b([\s\S]*)/);
    const range = where && !/\b1\s*=\s*1\b/.test(where[1]) && (/(>=|>)[\s\S]*(<=|<)/.test(where[1]) || /\bbetween\b|\bin\s*\(|=\s*('|\d)/.test(where[1]));
    let n;
    if (/\bgroup\s+by\b/.test(lower)) n = 5 + h % 40;
    else if (/^\s*select\s+(count|sum|avg|min|max)\s*\([^)]*\)\s*(as\s+\w+\s*)?(from|$)/.test(lower)) n = 1;
    else if (where) n = range ? Math.min(9800, Math.round((300 + h % 9000) * (0.9 + (inst.id % 15) / 100))) : MAXROWS + 1 + h % 28000;
    else n = base;
    const lim = lower.match(/\blimit\s+(\d+)/) || lower.match(/\btop\s*\(?\s*(\d+)/);
    if (lim) n = Math.min(n, +lim[1]);
    if (n > MAXROWS) return {lv: 'error', stage: '行数统计完成', msg: `导出数据行数(${numFmt(n)})超过阈值(${numFmt(MAXROWS)})。`, rows: n};
    if (n > MAXROWS * 0.8) return {lv: 'warn', stage: '行数统计完成', msg: '待导出行数接近上限，执行时若数据增长超限会被中止。', rows: n};
    return {lv: 'pass', stage: '行数统计完成', msg: '', rows: n};
  }

  /* ---------------------------------------------------------------- 导出：提交页 */
  const NX = {inst: 0, db: '', fmt: 'csv', name: '', sql: '', sqlAuto: true, batch: false, insts: [], schedOn: false, schedAt: '', check: null};
  const nxAll = () => NX.batch ? [NX.inst, ...NX.insts] : [NX.inst];
  const nxKey = () => [NX.batch, nxAll().join(','), NX.db, NX.sql.trim()].join('|');
  function nxInit() {
    const vis = visibleInstances();
    if (!vis.some(i => i.id === NX.inst)) { NX.inst = (vis[0] || {}).id; NX.db = ''; NX.sql = ''; NX.sqlAuto = true; NX.check = null; NX.insts = []; }
    const inst = instOf(NX.inst), dbs = dbsOf(inst.id);
    if (!dbs.some(d => d.db === NX.db)) { NX.db = (dbs[0] || {}).db || ''; NX.sqlAuto = true; }
    if (NX.sqlAuto || !NX.sql) { NX.sql = sampleSql(inst, NX.db); NX.sqlAuto = true; }
    NX.insts = NX.insts.filter(id => id !== NX.inst && visibleInstances().some(i => i.id === id && i.type === inst.type));
    if (!can('export.submit_batch')) { NX.batch = false; }
  }
  function nxSync() {
    const g = id => document.getElementById(id);
    if (g('nxName')) NX.name = g('nxName').value;
    if (g('nxFmt')) NX.fmt = g('nxFmt').value;
    if (g('nxSql')) NX.sql = g('nxSql').value;
    if (g('nxSchedOn')) NX.schedOn = g('nxSchedOn').checked;
    if (g('nxSchedAt')) NX.schedAt = g('nxSchedAt').value;
  }
  function exNewPage() {
    nxInit();
    const inst = instOf(NX.inst), dbs = dbsOf(inst.id), vis = visibleInstances();
    const canSub = can('export.submit') || can('export.submit_batch'), canBatch = can('export.submit_batch');
    const rg = rgOfInst(inst, ME), cands = vis.filter(i => i.type === inst.type && i.id !== inst.id);
    const c = NX.check, fresh = c && c.key === nxKey();
    const subCode = NX.batch ? 'export.submit_batch' : 'export.submit';
    return exTabs('new') + `
    <div class="card" style="margin-bottom:16px"><h2>导出工单信息</h2>
      <div class="form">
        ${fld('工单名称', `<input id="nxName" value="${esc(NX.name)}" placeholder="如：9 月订单对账" ${canSub ? '' : 'disabled'}>`, {req: true})}
        ${fld(NX.batch ? '主实例' : '目标实例', `<select id="nxInst" data-onchange="nxInst" ${canSub ? '' : 'disabled'}>${instOpts(vis, NX.inst)}</select>`, {req: true})}
        ${fld('数据库', `<select id="nxDb" data-onchange="nxDb" ${canSub ? '' : 'disabled'}>${dbs.map(d => `<option ${d.db === NX.db ? 'selected' : ''}>${esc(d.db)}</option>`).join('')}</select>`, {req: true})}
        ${fld('批量导出', `<label class="chk"><input type="checkbox" id="nxBatch" data-onchange="nxBatch" ${NX.batch ? 'checked' : ''} ${canBatch ? '' : 'disabled'}> 在多个实例上各导出一份</label>`)}
        ${NX.batch ? `<label>其余实例</label><div>
          <div class="row">${cands.length ? cands.map(i => `<label class="chk"><input type="checkbox" data-onchange="nxTick" data-id="${i.id}" ${NX.insts.includes(i.id) ? 'checked' : ''}> ${esc(i.name)}</label>`).join('') : '<span class="muted">没有可选的同类型实例</span>'}</div>
          <div class="small muted" style="margin-top:4px">已选 ${nxAll().length} 个实例（含主实例）</div></div>` : ''}
        ${fld('资源组 / 审批流', `<span>${esc(rgName(rg))}</span> <span class="tag brand">${flowText(rg)}</span>`)}
        ${fld('导出格式', `<select id="nxFmt" ${canSub ? '' : 'disabled'}>${Object.entries(FMT).map(([k, v]) => `<option value="${k}" ${k === NX.fmt ? 'selected' : ''}>${v}</option>`).join('')}</select>`, {req: true})}
        ${fld('定时执行（可不填）', `<label class="chk"><input type="checkbox" id="nxSchedOn" data-onchange="nxSched" ${NX.schedOn ? 'checked' : ''} ${canSub ? '' : 'disabled'}> 审批通过后在指定时间自动导出</label> <input id="nxSchedAt" placeholder="2026-10-07 22:30" value="${esc(NX.schedAt)}" style="width:170px;margin-left:8px" ${NX.schedOn && canSub ? '' : 'disabled'}>`)}
      </div></div>
    <div class="card" style="margin-bottom:16px"><h2>查询语句
        <span class="right"><span class="small muted">示例：</span><a data-on="nxEx" data-k="ok">合法查询</a><a data-on="nxEx" data-k="big">无条件全表</a><a data-on="nxEx" data-k="bad">写入语句</a></span></h2>
      <textarea class="sql-ed" id="nxSql" rows="7" data-oninput="nxSql" spellcheck="false" ${canSub ? '' : 'disabled'}>${esc(NX.sql)}</textarea>
      <div class="row" style="margin-top:10px"><span class="sp"></span>
        <button class="btn ${canSub ? '' : 'noperm'}" data-on="nxCheck" ${canSub ? '' : 'disabled'}>检查并统计行数</button>
        ${B(subCode, '提交工单', {cls: 'primary'})}</div></div>
    <div class="card" id="nxChk" ${c && !fresh ? 'style="opacity:.55"' : ''}><h2>检查结果 ${c ? `<span class="sub">通过 ${c.rows.filter(r => r.lv !== 'error').length}　错误 ${c.rows.filter(r => r.lv === 'error').length}${fresh ? '' : '　<b style="color:var(--warn)">（语句或实例已变更，请重新检查）</b>'}</span>` : ''}</h2>
      ${c ? tbl(['#', '实例', '阶段', '级别', '信息', '待导出行数'], c.rows.map((r, i) => [i + 1, esc(instOf(r.inst).name), r.stage, `<span class="tag ${r.lv === 'pass' ? 'ok' : r.lv === 'warn' ? 'warn' : 'bad'}">${r.lv === 'pass' ? '通过' : r.lv === 'warn' ? '警告' : '错误'}</span>`, esc(r.msg) || '<span class="muted">—</span>', r.rows ? `<div style="min-width:130px"><b>${numFmt(r.rows)}</b><div class="meter"><b class="${r.rows > MAXROWS ? 'bad' : r.rows > MAXROWS * .8 ? 'warn' : ''}" style="width:${Math.min(100, r.rows / MAXROWS * 100)}%"></b></div></div>` : '—'])) : empty('尚未检查', '⌕')}</div>`;
  }
  ON.nxInst = (d, el) => { nxSync(); NX.inst = +el.value; NX.db = ''; NX.sqlAuto = true; NX.check = null; rerender(); };
  ON.nxDb = (d, el) => { nxSync(); NX.db = el.value; NX.sqlAuto = true; NX.check = null; rerender(); };
  ON.nxBatch = (d, el) => { nxSync(); NX.batch = el.checked; NX.check = null; rerender(); };
  ON.nxTick = (d, el) => { nxSync(); const id = +d.id; NX.insts = el.checked ? [...new Set([...NX.insts, id])] : NX.insts.filter(x => x !== id); rerender(); };
  ON.nxSched = () => { nxSync(); rerender(); };
  ON.nxSql = (d, el) => { NX.sql = el.value; NX.sqlAuto = false; const c = $('#nxChk'); if (c && NX.check) c.style.opacity = NX.check.key === nxKey() ? '' : '.55'; };
  ON.nxEx = d => { nxSync(); const inst = instOf(NX.inst); NX.sql = sampleSql(inst, NX.db, d.k); NX.sqlAuto = d.k === 'ok'; rerender(); };
  ON.nxCheck = () => {
    nxSync();
    if (!(can('export.submit') || can('export.submit_batch'))) return toast('无权限');
    const rows = nxAll().map(id => {
      const inst = instOf(id);
      if (!dbsOf(id).some(d => d.db === NX.db)) return {inst: id, lv: 'error', stage: '规则检查未通过', msg: `实例 ${inst.name} 不存在数据库 ${NX.db}`, rows: 0};
      return Object.assign({inst: id}, analyze(NX.sql, inst, NX.db));
    });
    NX.check = {key: nxKey(), rows, ok: rows.every(r => r.lv !== 'error')};
    toast(NX.check.ok ? `检查通过，待导出 ${numFmt(rows.reduce((s, r) => s + r.rows, 0))} 行` : '检查未通过，请根据提示修改');
    rerender();
  };
  function nxSubmit(batch) {
    nxSync();
    const code = batch ? 'export.submit_batch' : 'export.submit';
    if (!!NX.batch !== batch) return toast('提交类型与表单不一致');
    const name = NX.name.trim();
    if (!name) return toast('请填写工单名称');
    const c = NX.check;
    if (!c || c.key !== nxKey()) return toast('请先点击「检查并统计行数」（语句或实例变更后需重新检查）');
    if (!c.ok) return toast('检查未通过（行数超限或语句不合规），无法提交');
    const ids = nxAll();
    if (batch && ids.length < 2) return toast('批量导出至少选择 2 个实例');
    let sched = '';
    if (NX.schedOn) {
      sched = NX.schedAt.trim();
      if (!/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/.test(sched) || sched < nowT().slice(0, 16)) return toast('定时时间格式应为 YYYY-MM-DD HH:mm，且不能早于当前时间');
    }
    const inst = instOf(NX.inst), rg = rgOfInst(inst, ME), id = nextEid();
    const w = {id, name, submitter: ME.login, rg, inst: NX.inst, insts: ids, batch, db: NX.db, fmt: NX.fmt, sql: NX.sql.trim(), rows: c.rows.reduce((s, r) => s + r.rows, 0),
      created: nowT(), status: '等待审批', level: 0, approvals: [], sched, progress: 0, dls: 0, file: null,
      items: ids.map(i => ({inst: i, st: 'wait', rows: 0, est: c.rows.find(r => r.inst === i).rows, secs: 0, msg: '', p: 0})),
      logs: [{at: nowT(), who: ME.login, text: `提交${batch ? '批量' : ''}导出工单，等待审批，审批流程：${flowText(rg)}`}]};
    if (sched) lg(w, ME.login, `计划定时导出：${sched}`);
    EXPORTS.unshift(w);
    audit(code, `提交${batch ? '批量' : ''}导出工单 #${id}「${name}」（${FMT[w.fmt]}，${numFmt(w.rows)} 行）`, '成功', `${ids.map(i => instOf(i).name).join(',')} / ${w.db}`);
    toast(`导出工单 #${id} 已提交，${notifyTxt(rg, candTxt(flowOf(rg)[0], rg, ME.login))}`);
    Object.assign(NX, {name: '', sqlAuto: true, check: null, insts: [], schedOn: false, schedAt: '', batch: false});
    buildNav(); go('export', 'detail/' + id);
  }
  ACTIONS['export.submit'] = () => nxSubmit(false);
  ACTIONS['export.submit_batch'] = () => nxSubmit(true);

  /* ---------------------------------------------------------------- 导出：列表 */
  const EL = {tab: 'normal', st: '', q: '', todo: false};
  const exTabs = cur => tabs('export', [['list', '导出工单'], ['new', '提交导出']], cur);
  function exListRows() {
    const list = EXPORTS.filter(w => canSeeEx(w) && (EL.tab === 'batch') === w.batch && (!EL.st || w.status === EL.st) &&
      (!EL.q || (w.name + w.submitter + w.id + w.db).toLowerCase().includes(EL.q.toLowerCase())) && (!EL.todo || canApproveEx(ME, w)))
      .sort((a, b) => b.id - a.id);
    return tbl(['工单号', '名称', '实例 / 库', '格式', '行数', '提交人', '资源组', '状态', '提交时间', '操作'], list, {
      attrs: w => ` class="click" data-on="exOpen" data-id="${w.id}"`,
      cells: w => [`<a href="#export/detail/${w.id}">#${w.id}</a>`, `<b>${esc(w.name)}</b>`,
        w.batch ? `<span class="tag brand">批量 ${w.items.length} 实例</span> <span class="mono small">${esc(w.db)}</span>` : `${esc(instOf(w.inst).name)}<div class="small muted mono">${esc(w.db)}</div>`,
        FMT[w.fmt].split('（')[0], numFmt(w.rows), uTxt(w.submitter), esc(rgName(w.rg)), exTag(w) + (canApproveEx(ME, w) ? ' <span class="tag warn">待我审批</span>' : ''), esc(w.created),
        `<div class="ops"><a class="btn sm" href="#export/detail/${w.id}">详情</a></div>`]
    });
  }
  function exListPage() {
    const vis = EXPORTS.filter(canSeeEx), cnt = s => vis.filter(w => w.status === s).length;
    const showBatch = can('export.view_batch');
    if (EL.tab === 'batch' && !showBatch) EL.tab = 'normal';
    return exTabs('list') + `
    <div class="grid g4" style="margin-bottom:14px">
      ${[['等待审批', cnt('等待审批'), 'warn'], ['待执行（审核通过）', cnt('审核通过'), ''], ['执行中', cnt('执行中'), ''], ['执行有异常', cnt('执行有异常'), cnt('执行有异常') ? 'bad' : '']].map(([l, v, c]) => `<div class="card kpi"><div class="v ${c === 'bad' ? 'up' : ''}">${v}</div><div class="l">${l}</div></div>`).join('')}
    </div>
    <div class="toolbar">
      ${subtabs([['normal', '普通导出'], ...(showBatch ? [['batch', '批量导出']] : [])], EL.tab, 'exTab')}
      <select data-onchange="exSt"><option value="">全部状态</option>${Object.keys(EX_ST).map(s => `<option ${s === EL.st ? 'selected' : ''}>${s}</option>`).join('')}</select>
      <input type="search" placeholder="搜索工单号 / 名称 / 提交人" value="${esc(EL.q)}" data-oninput="exQ" style="min-width:240px">
      <label class="chk"><input type="checkbox" data-onchange="exTodo" ${EL.todo ? 'checked' : ''}> 只看待我审批</label>
      <span class="sp"></span><a class="btn primary" href="#export/new">＋ 提交导出</a></div>
    <div class="card" id="exList">${exListRows()}</div>`;
  }
  ON.exTab = d => { EL.tab = d.v; rerender(); };
  ON.exSt = (d, el) => { EL.st = el.value; rerender(); };
  ON.exTodo = (d, el) => { EL.todo = el.checked; rerender(); };
  ON.exQ = (d, el) => { EL.q = el.value; $('#exList').innerHTML = exListRows(); };
  ON.exOpen = (d, el, e) => { if (e.target.closest('a')) return; go('export', 'detail/' + d.id); };

  /* ---------------------------------------------------------------- 导出：详情 */
  function exDetailPage(id) {
    const w = exById(id);
    const back = `<div class="row" style="margin-bottom:12px"><a class="btn" href="#export">← 返回列表</a></div>`;
    if (!w) return back + `<div class="card">${empty('找不到这张导出工单')}</div>`;
    if (!canSeeEx(w)) return back + `<div class="card">${empty('工单不存在，或你没有权限查看。', '🔒')}</div>`;
    const pending = w.status === '等待审批', approver = pending && w.submitter !== ME.login && eligible(ME, flowOf(w.rg)[w.level], w.rg);
    const lv = flowOf(w.rg)[w.level];
    const execCode = can('sqlworkflow.execute_own') && !can('sqlworkflow.execute_group') && w.submitter === ME.login ? 'sqlworkflow.execute_own' : 'sqlworkflow.execute_group';
    const stopCode = w.submitter === ME.login ? 'sqlworkflow.cancel_own' : 'sqlworkflow.reject';
    const busy = w.items.some(i => i.st === 'run');
    const instTxt = w.batch ? w.items.map(i => esc(instOf(i.inst).name)).join('、') : esc(instOf(w.inst).name);
    const basic = kvList([
      ['提交人', `${esc(userOf(w.submitter).name)}（${esc(w.submitter)}）`], ['资源组', esc(rgName(w.rg))],
      ['目标实例', instTxt], ['数据库', `<span class="mono">${esc(w.db)}</span>`],
      ['类型', w.batch ? `<span class="tag brand">批量导出 · ${w.items.length} 实例</span>` : '离线导出（单实例）'], ['导出格式', `${FMT[w.fmt]}`],
      ['待导出行数', `${numFmt(w.rows)}${w.batch ? ' <span class="muted small">（各实例合计）</span>' : ''}`], ['提交时间', esc(w.created)],
      ['计划执行', w.sched ? `${esc(w.sched)} <span class="tag info">定时</span>` : '<span class="muted">未设置</span>'], ['审批流程', `${flowText(w.rg)}`]], 'c2');

    /* 审批流程 + 审批操作 */
    let review = '';
    if (pending) {
      if (w.submitter === ME.login) review = '';
      else if (approver) review = `<div class="hr"></div><div class="row">${PB('sqlworkflow.approve', '审批通过', 'exApprove', {id: w.id}, 'primary')}${PB('sqlworkflow.reject', '驳回', 'exReject', {id: w.id}, 'danger')}</div>`;
      else review = `<div class="small muted" style="margin-top:6px">当前等待「${lv.name}」审批：${esc(candTxt(lv, w.rg, w.submitter))}。</div>`;
      if (w.submitter === ME.login || ME.super || ME.rgs.includes(w.rg) && can('sqlworkflow.reject')) review += `<div class="row" style="margin-top:8px">${PB(stopCode, '终止工单', 'exStop', {id: w.id}, 'sm danger')}</div>`;
    }
    const flowCard = `<div class="card"><h2>审批流程</h2>${tline(flowTl(w, w.submitter, pending).concat(
      w.status === '审核通过' ? [{cls: 'cur', h: '审核通过，等待导出', m: w.sched ? '已设置定时：' + esc(w.sched) : ''}] :
      w.status === '执行中' ? [{cls: 'cur', h: '导出执行中', m: `进度 ${w.progress}%`}] :
      w.status === '已正常结束' ? [{cls: 'ok', h: '导出完成', m: w.file ? esc(w.file.at) : ''}] :
      w.status === '执行有异常' ? [{cls: 'bad', h: '导出有异常', m: '见下方各实例状态'}] :
      w.status === '人工终止' ? [{cls: 'bad', h: '人工终止流程', m: ''}] : []))}${review}</div>`;

    /* 执行区 */
    let exec = '';
    if (w.status === '审核通过') {
      exec = `<div class="card"><h2>导出</h2>
        ${w.sched ? notice(`已设置定时导出：<b>${esc(w.sched)}</b>。`, '', '⏱') : ''}
        <div class="row">${PB(execCode, '立即导出', 'exRun', {id: w.id}, 'primary')}
          <input id="exSchedAt" placeholder="2026-10-07 22:30" value="${esc(w.sched)}" style="width:170px">${PB('sqlworkflow.schedule', w.sched ? '修改定时' : '定时导出', 'exSched', {id: w.id})}
          ${w.sched ? PB('sqlworkflow.schedule', '取消定时', 'exSchedCancel', {id: w.id}, 'sm') + PB(execCode, '原型：模拟到点触发', 'exRun', {id: w.id, t: 1}, 'sm') : ''}
          <span class="sp"></span>${PB(stopCode, '终止工单', 'exStop', {id: w.id}, 'danger')}</div></div>`;
    } else if (w.status === '执行中') {
      exec = `<div class="card"><h2>导出执行中</h2>
        <div class="row"><div class="meter" style="flex:1;height:10px"><b style="width:${w.progress}%"></b></div><b>${w.progress}%</b></div></div>`;
    }
    /* 结果文件 */
    let file = '';
    if (w.status === '已正常结束' || w.status === '执行有异常') {
      const f = w.file, failN = w.items.filter(i => i.st === 'fail').length;
      file = `<div class="card"><h2>导出结果文件</h2>${f ? kvList([
        ['文件名', `<span class="mono">${esc(f.name)}</span>`], ['大小', esc(f.size)], ['生成时间', esc(f.at)],
        ['状态', f.cleaned ? '<span class="tag bad">文件已被清理</span>' : failN ? '<span class="tag warn">部分实例成功，可下载已成功文件</span>' : '<span class="tag ok">可下载</span>'],
        ['下载次数', `${w.dls} 次`]], 'c2') + (f.cleaned ? notice('文件不存在（可能已被清理），请重新提交导出。', 'bad', '⚠') : '') : notice('没有生成文件（全部实例失败）。', 'warn', '⚠')}
        ${f ? `<div class="row" style="margin-top:10px">${B('export.download', '⇩ 下载导出文件', {cls: 'primary', data: {id: w.id}})}</div>` : ''}</div>`;
    }
    /* 各实例 / 执行结果 */
    const resRows = w.items.map((i, n) => [n + 1, esc(instOf(i.inst).name), i.st === 'ok' ? '执行正常' : i.st === 'fail' ? '异常终止' : i.st === 'run' ? '执行中' : i.st === 'closed' ? '已关闭' : '等待执行',
      `<span class="tag ${i.st === 'ok' ? 'ok' : i.st === 'fail' ? 'bad' : i.st === 'run' ? 'brand' : ''}">${i.st === 'ok' ? '成功' : i.st === 'fail' ? '失败' : i.st === 'run' ? '执行中' : i.st === 'closed' ? '已关闭' : '等待'}</span>`,
      i.st === 'run' ? `<div class="meter" style="min-width:90px"><b style="width:${i.p}%"></b></div>` : esc(i.msg) || '<span class="muted">—</span>',
      i.st === 'ok' ? numFmt(i.rows) : i.st === 'wait' ? `<span class="muted">预计 ${numFmt(i.est)}</span>` : '—', i.st === 'ok' ? i.secs : '—',
      w.batch && i.st === 'fail' ? `<div class="ops">${PB('sqlworkflow.batch_op', '重试', 'exRetry', {id: w.id, inst: i.inst}, 'sm')}${PB('sqlworkflow.batch_op', '关闭', 'exClose', {id: w.id, inst: i.inst}, 'sm danger')}</div>` : '']);
    const heads = ['#', '实例', '阶段', '状态', '信息', '影响行数', '耗时(s)', ...(w.batch ? ['操作'] : [])];
    const showRes = ['执行中', '已正常结束', '执行有异常'].includes(w.status);
    const sumTxt = () => { const act = w.items.filter(i => i.st !== 'closed'), ok = act.filter(i => i.st === 'ok').length, bad = act.filter(i => i.st === 'fail').length; return `批量导出${busy ? '进行中' : '完成'}：共 ${w.items.length} 个实例，成功 ${ok} 个，失败 ${bad} 个${w.items.some(i => i.st === 'closed') ? '，已关闭 ' + w.items.filter(i => i.st === 'closed').length + ' 个' : ''}${w.file ? '，保存文件: ' + w.file.name : ''}。`; };
    const res = showRes ? `<div class="card"><h2>${w.batch ? '各实例导出状态' : '导出结果'}</h2>
      ${tbl(heads, resRows.map(r => w.batch ? r : r.slice(0, 7)))}
      ${w.batch ? `<div class="notice ${w.status === '执行有异常' ? 'warn' : 'ok'}" style="margin-top:10px"><span>${w.status === '执行有异常' ? '⚠' : '✓'}</span><div>${esc(sumTxt())}${w.status === '执行有异常' ? '异常实例未包含在压缩包中；已成功实例的文件仍可下载。' : ''}</div></div>` : ''}</div>` : '';
    const stopped = w.status === '人工终止' ? notice(`工单已人工终止：${esc((w.approvals.find(a => a.act === '驳回') || {}).note || w.note || '—')}`, 'bad', '✕') : '';

    return back + `<div class="row" style="margin-bottom:12px"><h2 style="margin:0;font-size:18px">导出工单 #${w.id}　${esc(w.name)}</h2>${exTag(w)}</div>${stopped}
    <div class="grid g-main" style="margin-bottom:16px">
      <div class="grid" style="align-content:start">
        <div class="card"><h2>基本信息</h2>${basic}</div>
        <div class="card"><h2>导出查询</h2>${code(w.sql, 'sql')}</div>
        ${exec}${res}${file}
      </div>
      <div class="grid" style="align-content:start">${flowCard}<div class="card"><h2>操作日志</h2>${logHtml(w.logs)}</div></div>
    </div>`;
  }
  ON.exApprove = d => {
    if (!guard('sqlworkflow.approve')) return;
    const w = exById(d.id); if (!w || !canApproveEx(ME, w)) return toast(w && w.submitter === ME.login ? '不能审批自己提交的工单' : '你不是当前审批人');
    openModal({title: `审批通过 · 导出工单 #${w.id}`, code: 'sqlworkflow.approve', body: `<div class="form"><label>备注（可不填）</label><textarea id="exNote" rows="2"></textarea></div><div class="small muted" style="margin-top:8px">${flowOf(w.rg)[w.level + 1] ? `通过后流转到「${flowOf(w.rg)[w.level + 1].name}」继续审批。` : '这是最后一级，通过后工单变为「审核通过」。'}</div>`,
      onOk() {
        const note = $('#exNote').value.trim(), l = flowOf(w.rg)[w.level];
        w.approvals.push({lv: l.name, by: ME.login, at: nowT(), act: '通过', note}); lg(w, ME.login, `${l.name}审批通过${note ? '：' + note : ''}`);
        let msg;
        if (w.level + 1 < flowOf(w.rg).length) { w.level++; msg = `已通过「${l.name}」审批，${notifyTxt(w.rg, candTxt(flowOf(w.rg)[w.level], w.rg, w.submitter))}`; }
        else { w.status = '审核通过'; w.level = flowOf(w.rg).length; msg = `审核通过，${notifyTxt(w.rg, w.submitter)}${w.sched ? '；已按计划等待定时导出' : ''}`; }
        audit('sqlworkflow.approve', `审批通过导出工单 #${w.id}（${l.name}）`); toast(msg); buildNav(); rerender();
      }});
  };
  ON.exReject = d => {
    if (!guard('sqlworkflow.reject')) return;
    const w = exById(d.id); if (!w || !canApproveEx(ME, w)) return toast('你不是当前审批人，或不能驳回自己的工单');
    openModal({title: `驳回 · 导出工单 #${w.id}`, okCls: 'danger primary', ok: '确认驳回', code: 'sqlworkflow.reject', body: `<div class="form"><label class="req">驳回原因</label><textarea id="exNote" rows="3"></textarea></div>`,
      onOk() {
        const note = $('#exNote').value.trim(); if (!note) { toast('请填写驳回原因'); return false; }
        const l = flowOf(w.rg)[w.level];
        w.approvals.push({lv: l.name, by: ME.login, at: nowT(), act: '驳回', note}); w.status = '人工终止'; lg(w, ME.login, `${l.name}驳回，流程终止：${note}`);
        audit('sqlworkflow.reject', `驳回导出工单 #${w.id}：${note}`); toast(`已驳回，${notifyTxt(w.rg, w.submitter)}`); buildNav(); rerender();
      }});
  };
  ON.exStop = d => {
    const w = exById(d.id); if (!w) return;
    const code = w.submitter === ME.login ? 'sqlworkflow.cancel_own' : 'sqlworkflow.reject'; if (!guard(code)) return;
    openModal({title: `终止导出工单 #${w.id}`, okCls: 'danger primary', ok: '确认终止', code, body: `<div class="form"><label>终止原因（可不填）</label><textarea id="exNote" rows="2"></textarea></div>`,
      onOk() { w.status = '人工终止'; w.note = $('#exNote').value.trim() || '提交人 / 管理员终止'; lg(w, ME.login, `人工终止工单：${w.note}`); audit(code, `终止导出工单 #${w.id}`); toast(`工单已终止，${notifyTxt(w.rg, w.submitter)}`); buildNav(); rerender(); }});
  };
  function exExecCode(w) { return can('sqlworkflow.execute_own') && !can('sqlworkflow.execute_group') && w.submitter === ME.login ? 'sqlworkflow.execute_own' : 'sqlworkflow.execute_group'; }
  ON.exRun = d => {
    const w = exById(d.id), code = exExecCode(w); if (!w || !guard(code)) return;
    if (w.status !== '审核通过') return toast('工单状态已变化');
    if (code === 'sqlworkflow.execute_group' && !(ME.super || ME.rgs.includes(w.rg))) return toast('只能执行所在资源组内的工单');
    risky(code, d.t ? `定时到点 · 立即导出 #${w.id}` : `立即导出 #${w.id}`, `将在 <b>${w.items.map(i => esc(instOf(i.inst).name)).join('、')}</b> 上执行导出查询，生成 ${FMT[w.fmt]} 文件并压缩为 zip；数据超过行数上限会中止。`, w.sql, () => {
      lg(w, ME.login, d.t ? '定时时间到，开始导出' : '立即导出，开始执行'); audit(code, `${d.t ? '定时触发' : '立即'}导出工单 #${w.id}`);
      w.status = '执行中'; w.progress = 0; w.items.forEach(i => { i.st = 'wait'; i.p = 0; i.rows = 0; i.tried = false; });
      toast('已开始导出，' + notifyTxt(w.rg, w.submitter)); buildNav(); rerender(); exTick(w);
    });
  };
  ON.exSched = d => {
    if (!guard('sqlworkflow.schedule')) return;
    const w = exById(d.id), v = ($('#exSchedAt').value || '').trim();
    if (!/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/.test(v) || v < nowT().slice(0, 16)) return toast('时间格式应为 YYYY-MM-DD HH:mm，且不能早于当前时间');
    risky('sqlworkflow.schedule', `设置定时导出 #${w.id}`, `将在 <b>${esc(v)}</b> 由调度器自动执行导出。`, `-- 定时任务：${v}\n-- 导出工单 #${w.id}`, () => {
      w.sched = v; lg(w, ME.login, `设置定时导出：${v}`); audit('sqlworkflow.schedule', `设置导出工单 #${w.id} 定时执行 ${v}`); toast(`已设置定时导出，${notifyTxt(w.rg, w.submitter)}`); rerender();
    });
  };
  ON.exSchedCancel = d => { if (!guard('sqlworkflow.schedule')) return; const w = exById(d.id); w.sched = ''; lg(w, ME.login, '取消定时导出'); audit('sqlworkflow.schedule', `取消导出工单 #${w.id} 定时执行`); toast('已取消定时'); rerender(); };
  ON.exRetry = d => {
    if (!guard('sqlworkflow.batch_op')) return;
    const w = exById(d.id), it = w.items.find(i => i.inst == d.inst), nm = instOf(it.inst).name;
    risky('sqlworkflow.batch_op', `重试实例 ${nm}`, `重新在 <b>${esc(nm)}</b> 上导出，成功后文件会<b>合并进同一个压缩包</b>，其余实例的文件不受影响。`, w.sql, () => {
      it.st = 'run'; it.p = 0; it.msg = ''; w.status = '执行中'; lg(w, ME.login, `重试实例 ${nm}`); audit('sqlworkflow.batch_op', `重试导出工单 #${w.id} 的实例 ${nm}`); rerender(); exTick(w);
    });
  };
  ON.exClose = d => {
    if (!guard('sqlworkflow.batch_op')) return;
    const w = exById(d.id), it = w.items.find(i => i.inst == d.inst), nm = instOf(it.inst).name;
    risky('sqlworkflow.batch_op', `关闭实例 ${nm}`, `关闭后该实例<b>不再计入工单成败</b>，也不会再有文件。`, '', () => {
      it.st = 'closed'; it.msg = '已关闭，不再计入工单成败'; lg(w, ME.login, `关闭实例 ${nm}`); audit('sqlworkflow.batch_op', `关闭导出工单 #${w.id} 的实例 ${nm}`); exFinish(w); rerender();
    });
  };
  ACTIONS['export.download'] = d => {
    const w = exById(d.id); if (!w || !canSeeEx(w) || !w.file) return toast('工单不存在或没有文件');
    risky('export.download', `下载导出文件 · 工单 #${w.id}`, `导出文件包含<b>未脱敏的业务数据</b>，请妥善保管。<br>本次下载会写入审计日志。`, `GET /api/v1/sqlworkflow/${w.id}/download   # ${w.file.name}`, () => {
      const f = w.file, extra = `${w.items.map(i => instOf(i.inst).name).join(',')} / ${w.db}`;
      if (f.cleaned) {
        audit('export.download', `下载导出文件 #${w.id} ${f.name}`, '失败', '文件不存在（可能已被清理）'); lg(w, ME.login, '下载失败：文件不存在（可能已被清理）');
        toast('文件不存在（可能已被清理）'); rerender(); setTimeout(() => openModal({title: '下载失败', ok: '', cancel: '关闭', body: notice('文件不存在（可能已被清理）。', 'bad', '✕')}), 0); return;
      }
      w.dls++; lg(w, ME.login, `下载文件 ${f.name}`); audit('export.download', `下载导出文件 #${w.id} ${f.name}（${f.size}）`, '成功', extra);
      toast(`开始下载 ${f.name}（模拟），已写入审计日志`); rerender();
    });
  };

  /* ---------------------------------------------------------------- 导出：模拟执行（逐实例推进） */
  const TIMERS = {};
  function exTick(w) {
    if (TIMERS[w.id]) return;
    TIMERS[w.id] = setInterval(() => {
      const cur = w.items.find(i => i.st === 'run') || w.items.find(i => i.st === 'wait');
      if (!cur) { clearInterval(TIMERS[w.id]); delete TIMERS[w.id]; exFinish(w); if (ROUTE.page === 'export') rerender(); return; }
      if (cur.st === 'wait') { cur.st = 'run'; cur.p = 0; }
      cur.p = Math.min(100, (cur.p || 0) + 18 + hashStr(w.id + '' + cur.inst + cur.p) % 14);
      if (cur.p >= 100) {
        const nm = instOf(cur.inst).name;
        if (w.batch && cur.inst === 5 && !cur.tried) { cur.st = 'fail'; cur.msg = '导出失败: 查询超时（超过 max_export_execution_time = 60 s），已 KILL QUERY'; lg(w, '系统', `实例 ${nm} 导出失败：超时`); }
        else { cur.st = 'ok'; cur.rows = cur.est; cur.secs = +(0.8 + (hashStr(w.id + nm) % 180) / 100).toFixed(3); cur.msg = `导出文件: ${w.batch ? nm : w.db}.${FMT_EXT[w.fmt]}`; if (cur.tried) lg(w, '系统', `实例 ${nm} 重试成功，文件已合并进压缩包`); }
        cur.tried = true;
      }
      const n = w.items.length;
      w.progress = Math.round(w.items.reduce((s, i) => s + (['ok', 'fail', 'closed'].includes(i.st) ? 100 : i.st === 'run' ? (i.p || 0) : 0), 0) / n);
      if (ROUTE.page === 'export' && ROUTE.sub === 'detail/' + w.id) rerender();
    }, 450);
  }
  function exFinish(w) {
    const act = w.items.filter(i => i.st !== 'closed'), oks = act.filter(i => i.st === 'ok'), fails = act.filter(i => i.st === 'fail');
    const kb = oks.reduce((s, i) => s + i.rows * ({csv: .1, xlsx: .06, json: .16, xml: .22, sql: .14}[w.fmt] || .1), 0) + 2;
    if (oks.length) {
      if (w.file && !w.file.cleaned) { w.file.size = fmtSz(kb); w.file.at = nowT(); }
      else w.file = {name: w.batch ? `${w.id}_${stamp()}_${rand8(w.id + 'f')}.zip` : `${w.db}_${stamp()}_${rand8(w.id + 'f')}.zip`, size: fmtSz(kb), at: nowT()};
    }
    w.status = fails.length || !oks.length ? '执行有异常' : '已正常结束'; w.progress = 100;
    lg(w, '系统', w.status === '已正常结束' ? `导出完成，保存文件: ${w.file.name}` : (oks.length ? `批量导出完成：成功 ${oks.length} 个，失败 ${fails.length} 个，保存文件: ${w.file.name}` : '导出失败，未生成文件'));
    audit('sqlworkflow.execute_group', `导出工单 #${w.id} ${w.status}`, w.status === '已正常结束' ? '成功' : '失败');
    toast(`导出工单 #${w.id} ${w.status}，${notifyTxt(w.rg, w.submitter)}`); buildNav();
  }

  pages.export = {
    title: sub => /^detail\//.test(sub || '') ? `导出工单 #${sub.split('/')[1]}` : /^new/.test(sub || '') ? '提交数据导出' : '数据导出',
    render(sub) {
      const p = (sub || 'list').split('/');
      return p[0] === 'new' ? exNewPage() : p[0] === 'detail' ? exDetailPage(p[1]) : exListPage();
    },
    init(sub) {
      const p = (sub || '').split('/');
      if (p[0] === 'detail') { const w = exById(p[1]); if (w && w.items.some(i => i.st === 'run' || (w.status === '执行中' && i.st === 'wait'))) exTick(w); }
    }
  };

  /* ================================================================ 权限申请页（qapply） */
  const AP = {tab: 'mine', q: '', st: ''};
  const NA = {inst: 0, db: '', scope: 'table', tables: [], days: 30, limit: 1000, reason: '', qs: null};
  const apTabs = cur => tabs('qapply', [['list', '申请列表'], ['new', '申请权限']], cur);
  const seeAllAp = () => can('qpriv.review') || can('audit.view');
  function canSeeAp(a) { return ME.super || a.user === ME.login || (seeAllAp() && ME.rgs.includes(a.rg)); }
  const apCurLv = a => a.status === '待审批' ? flowOf(a.rg)[a.level] : null;
  const canReview = a => canReviewBy(ME, a);
  const apTag = a => `<span class="tag ${AP_ST[a.status]}">${a.status}</span>`;
  function apRows() {
    let list = APPLIES.filter(canSeeAp);
    if (AP.tab === 'mine') list = list.filter(a => a.user === ME.login);
    else if (AP.tab === 'todo') list = list.filter(canReview);
    list = list.filter(a => (!AP.st || a.status === AP.st) && (!AP.q || (a.user + a.db + a.tables.join(',') + instOf(a.inst).name + a.id).toLowerCase().includes(AP.q.toLowerCase()))).sort((a, b) => b.id - a.id);
    return tbl(['编号', '申请人', '实例', '库 / 表', '行数上限', '有效期至', '审批进度', '状态', '提交时间', '操作'], list, {
      attrs: a => ` class="click" data-on="apOpen" data-id="${a.id}"`,
      cells: a => {
        const lv = apCurLv(a);
        return [`<a href="#qapply/detail/${a.id}">#${a.id}</a>`, uTxt(a.user), esc(instOf(a.inst).name), `<span class="mono">${esc(a.db)}</span><div class="small muted">${a.tables.includes('*') ? '库权限（全库）' : '表权限：' + esc(a.tables.join('、'))}</div>`,
          numFmt(a.limit), esc(a.expire), lv ? `<span class="tag info">${lv.name} 审批中</span> <span class="small muted">${a.level + 1}/${flowOf(a.rg).length}</span>` : '<span class="muted">—</span>', apTag(a) + (a.status === '待审批' && canReview(a) ? ' <span class="tag warn">待我审批</span>' : ''), esc(a.created),
          `<div class="ops"><a class="btn sm" href="#qapply/detail/${a.id}">详情</a>${a.status === '待审批' && a.user === ME.login ? B('qpriv.apply', '撤回', {cls: 'sm danger', data: {op: 'withdraw', id: a.id}}) : ''}</div>`];
      }
    });
  }
  function apListPage() {
    const mineN = APPLIES.filter(a => a.user === ME.login).length, todoN = APPLIES.filter(canReview).length, allN = APPLIES.filter(canSeeAp).length;
    const more = seeAllAp();
    if (!more && AP.tab !== 'mine') AP.tab = 'mine';
    if (AP.tab === 'todo' && !can('qpriv.review')) AP.tab = 'mine';
    if (AP.tab === 'mine' && !mineN && more) AP.tab = can('qpriv.review') && todoN ? 'todo' : 'all';
    return apTabs('list') + `
    <div class="toolbar">
      ${subtabs([['mine', `我的申请（${mineN}）`], ...(can('qpriv.review') ? [['todo', `待我审批（${todoN}）`]] : []), ...(more ? [['all', `全部（${allN}）`]] : [])], AP.tab, 'apTab')}
      <select data-onchange="apSt"><option value="">全部状态</option>${Object.keys(AP_ST).map(s => `<option ${s === AP.st ? 'selected' : ''}>${s}</option>`).join('')}</select>
      <input type="search" placeholder="搜索编号 / 申请人 / 实例" value="${esc(AP.q)}" data-oninput="apQ" style="min-width:260px">
      <span class="sp"></span>${B('qpriv.apply', '＋ 申请权限', {cls: 'primary', data: {op: 'new'}})}</div>
    <div class="card" id="apList">${apRows()}</div>`;
  }
  ON.apTab = d => { AP.tab = d.v; rerender(); };
  ON.apSt = (d, el) => { AP.st = el.value; rerender(); };
  ON.apQ = (d, el) => { AP.q = el.value; $('#apList').innerHTML = apRows(); };
  ON.apOpen = (d, el, e) => { if (e.target.closest('a,button')) return; go('qapply', 'detail/' + d.id); };

  /* ---------- 申请表单 ---------- */
  function naInit(sub) {
    const q = {}; (sub.split('?')[1] || '').split('&').forEach(kv => { const [k, v] = kv.split('='); if (k) q[k] = decodeURIComponent(v || ''); });
    if (NA.qs !== sub) {
      NA.qs = sub; NA.scope = 'table'; NA.tables = []; NA.days = 30; NA.limit = 1000; NA.reason = '';
      const vis = visibleInstances('all'), i = vis.find(x => x.id == q.inst) || vis[0];
      NA.inst = i ? i.id : 0; NA.db = q.db || '';
      if (q.table) NA.tables = q.table.split(',');
      if (q.db && !q.table) NA.scope = 'db';
    }
    const vis = visibleInstances('all'); if (!vis.some(i => i.id === NA.inst)) { NA.inst = (vis[0] || {}).id; NA.db = ''; NA.tables = []; }
    if (isRds(NA.inst)) { NA.scope = 'db'; if (NA.db && !/^db/i.test(NA.db)) NA.db = 'db' + NA.db; } const dbs = pDbs(NA.inst); if (!dbs.some(d => d.db === NA.db)) { NA.db = (dbs[0] || {}).db || ''; NA.tables = []; }
    const tn = tablesOf(NA.inst, NA.db).map(t => t.n); NA.tables = NA.tables.filter(t => tn.includes(t));
  }
  function naSync() {
    const g = id => document.getElementById(id);
    if (g('naReason')) NA.reason = g('naReason').value;
    if (g('naLimit')) NA.limit = g('naLimit').value;
    if (g('naDays')) NA.days = +g('naDays').value;
    if ($$('.naTbl').length || NA.scope === 'table') NA.tables = $$('.naTbl').filter(x => x.checked).map(x => x.value);
  }
  function apNewPage(sub) {
    naInit(sub);
    const vis = visibleInstances('all'), inst = instOf(NA.inst), rds = isRds(NA.inst), dbs = pDbs(NA.inst), tbs = tablesOf(NA.inst, NA.db), can_ = can('qpriv.apply');
    const rg = rgOfInst(inst, ME), exp = dayOffset(NA.days);
    return apTabs('new') + `
    <div class="grid g-main"><div class="card"><h2>申请查询权限</h2>
      <div class="form">
        ${fld('目标实例', `<select id="naInst" data-onchange="naInst" ${can_ ? '' : 'disabled'}>${instOpts(vis, NA.inst)}</select>`, {req: true})}
        ${fld('资源组 / 审批流', `${esc(rgName(rg))} <span class="tag brand">${flowText(rg)}</span>`)}
        ${fld('数据库', `<select id="naDb" data-onchange="naDb" ${can_ ? '' : 'disabled'}>${dbs.map(d => `<option ${d.db === NA.db ? 'selected' : ''}>${esc(d.db)}</option>`).join('')}</select>`, {req: true})}
        ${fld('权限类型', `<label class="chk"><input type="radio" name="naScope" value="table" data-onchange="naScope" ${NA.scope === 'table' ? 'checked' : ''} ${can_ && !rds ? '' : 'disabled'}> 表权限（指定表）</label>　<label class="chk"><input type="radio" name="naScope" value="db" data-onchange="naScope" ${NA.scope === 'db' ? 'checked' : ''} ${can_ ? '' : 'disabled'}> 库权限（整个库）</label>`)}
        ${NA.scope === 'table' ? `<label class="req">${inst.type === 'Mongo' ? '集合（可多选）' : '表（可多选）'}</label><div class="row">${tbs.map(t => `<label class="chk"><input type="checkbox" class="naTbl" value="${esc(t.n)}" ${NA.tables.includes(t.n) ? 'checked' : ''} ${can_ ? '' : 'disabled'}> <span class="mono">${esc(t.n)}</span>${t.kind === 'dict' ? ' <span class="tag">字典</span>' : t.kind === 'matview' ? ' <span class="tag">物化视图</span>' : t.view ? ' <span class="tag">视图</span>' : ''}</label>`).join('') || `<span class="muted">该库没有${inst.type === 'Mongo' ? '集合' : '表'}</span>`}</div>` : ''}
        ${fld('有效期', `<select id="naDays" data-onchange="naDays" ${can_ ? '' : 'disabled'}>${[7, 14, 30, 90, 180].map(d => `<option value="${d}" ${d === NA.days ? 'selected' : ''}>${d} 天</option>`).join('')}</select> <span class="muted small">有效期至 <b id="naExp">${exp}</b></span>`, {req: true})}
        ${fld('返回行数上限（1 ~ 1,000,000）', `<input type="number" id="naLimit" min="1" max="1000000" value="${esc(NA.limit)}" style="max-width:160px" ${can_ ? '' : 'disabled'}>`, {req: true})}
        ${fld('申请理由', `<textarea id="naReason" rows="3" placeholder="说明用途、数据范围" ${can_ ? '' : 'disabled'}>${esc(NA.reason)}</textarea>`, {req: true})}
      </div>
      <div class="row" style="margin-top:14px"><span class="sp"></span><a class="btn" href="#qapply">取消</a>${B('qpriv.apply', '提交申请', {cls: 'primary', data: {op: 'submit'}})}</div></div>
      <div class="card"><h2>审批流程</h2>${tline(flowOf(rg).map((l, i) => ({cls: i ? 'skip' : 'cur', h: `${l.name}审批`, m: `候选：${esc(candTxt(l, rg, ME.login))}`})))}</div></div>`;
  }
  ON.naInst = (d, el) => { naSync(); NA.inst = +el.value; NA.db = ''; NA.tables = []; rerender(); };
  ON.naDb = (d, el) => { naSync(); NA.db = el.value; NA.tables = []; rerender(); };
  ON.naScope = (d, el) => { naSync(); NA.scope = el.value; rerender(); };
  ON.naDays = (d, el) => { NA.days = +el.value; const x = $('#naExp'); if (x) x.textContent = dayOffset(NA.days); };

  const sameSet = (a, b) => a.length === b.length && a.every(x => b.includes(x));
  function covers(p, user, inst, db, tables) {
    return p.user === user && p.inst === inst && p.db === db && p.expire >= TODAY && (p.tables.includes('*') || (!tables.includes('*') && tables.every(t => p.tables.includes(t))));
  }
  function applyGrant(a) {
    const id = nextQid();
    QPRIVS.push({id, user: a.user, inst: a.inst, db: a.db, tables: a.tables.slice(), limit: a.limit, expire: a.expire, src: `申请 #${a.id}`});
    a.grantedId = id; return id;
  }
  ACTIONS['qpriv.apply'] = d => {
    if (d.op === 'new') { go('qapply', 'new'); return; }
    if (d.op === 'withdraw') {
      const a = APPLIES.find(x => x.id == d.id);
      if (!a || a.status !== '待审批' || a.user !== ME.login) return toast('只有申请人本人可以撤回待审批的申请');
      return openModal({title: `撤回申请 #${a.id}`, okCls: 'danger primary', ok: '确认撤回', code: 'qpriv.apply', body: '撤回后流程终止，需要时可重新提交。',
        onOk() { a.status = '已撤回'; a.endAt = nowT(); lg(a, ME.login, '申请人撤回申请'); audit('qpriv.apply', `撤回查询权限申请 #${a.id}`); toast(`申请已撤回，${notifyTxt(a.rg, candTxt(flowOf(a.rg)[a.level], a.rg, a.user))}`); buildNav(); rerender(); }});
    }
    /* submit */
    naSync();
    const inst = instOf(NA.inst), limit = parseInt(NA.limit, 10), reason = NA.reason.trim();
    const tables = NA.scope === 'db' ? ['*'] : NA.tables.slice();
    if (!NA.db) return toast('请选择数据库');
    if (isRds(NA.inst) && NA.scope === 'table') return toast('Redis 没有表，只支持库级权限，不能申请表权限');
    if (NA.scope === 'table' && !tables.length) return toast('请至少选择一张表');
    if (!(limit >= 1 && limit <= 1000000)) return toast('行数上限需在 1 ~ 1,000,000 之间');
    if (reason.length < 5) return toast('请填写申请理由（至少 5 个字）');
    const have = QPRIVS.find(p => covers(p, ME.login, NA.inst, NA.db, tables));
    if (have) return openModal({title: '无需重复申请', ok: '', cancel: '知道了', body: notice(`你已拥有 <b>${esc(inst.name)} / ${esc(NA.db)}</b> 的有效查询权限（权限 #${have.id}，${tblTxt(have.tables)}，至 ${have.expire}），无需重复申请。`, 'warn', '⚠')});
    const dup = APPLIES.find(a => a.user === ME.login && a.status === '待审批' && a.inst === NA.inst && a.db === NA.db && (a.tables.includes('*') || tables.includes('*') || a.tables.some(t => tables.includes(t))));
    if (dup) return openModal({title: '已有待审批的相同申请', ok: '', cancel: '知道了', body: notice(`申请 <a href="#qapply/detail/${dup.id}">#${dup.id}</a> 与本次内容重叠且仍在审批中，请等待审批或先撤回。`, 'warn', '⚠')});
    const rg = rgOfInst(inst, ME), id = nextAid();
    const a = {id, user: ME.login, inst: NA.inst, db: NA.db, tables, limit, expire: dayOffset(NA.days), created: nowT(), status: '待审批', level: 0, approvals: [], reason, rg, logs: []};
    lg(a, ME.login, `提交申请，等待审批，审批流程：${flowText(rg)}`);
    APPLIES.unshift(a); audit('qpriv.apply', `申请查询权限 ${NA.db}${tables.includes('*') ? '.*' : '.' + tables.join(',')}（${NA.days} 天，${limit} 行）`, '成功', inst.name);
    NA.qs = null; AP.tab = 'mine';
    toast(`申请 #${id} 已提交，${notifyTxt(rg, candTxt(flowOf(rg)[0], rg, ME.login))}`); buildNav(); go('qapply', 'detail/' + id);
  };

  /* ---------- 审批 ---------- */
  ACTIONS['qpriv.review'] = d => {
    const a = APPLIES.find(x => x.id == d.id); if (!a) return;
    if (a.user === ME.login) { audit('qpriv.review', `尝试审批自己的申请 #${a.id}`, '拒绝（不能审批自己的申请）'); return toast('不能审批自己的申请'); }
    if (!canReview(a)) return toast('你不是当前审批人，或申请已不在待审批状态');
    const lv = flowOf(a.rg)[a.level], last = a.level + 1 >= flowOf(a.rg).length;
    if (d.op === 'reject') {
      return openModal({title: `驳回申请 #${a.id}`, okCls: 'danger primary', ok: '确认驳回', code: 'qpriv.review', body: `<div class="form"><label class="req">驳回原因</label><textarea id="apNote" rows="3"></textarea></div>`,
        onOk() {
          const note = $('#apNote').value.trim(); if (!note) { toast('请填写驳回原因'); return false; }
          a.approvals.push({lv: lv.name, by: ME.login, at: nowT(), act: '驳回', note}); a.status = '审批驳回'; lg(a, ME.login, `${lv.name}驳回：${note}`);
          audit('qpriv.review', `驳回查询权限申请 #${a.id}：${note}`); toast(`已驳回，${notifyTxt(a.rg, a.user)}`); buildNav(); rerender();
        }});
    }
    openModal({title: `审批通过 · 申请 #${a.id}`, code: 'qpriv.review', body: `<div class="form"><label>备注（可不填）</label><textarea id="apNote" rows="2"></textarea></div>
      <div class="small muted" style="margin-top:8px">${last ? '这是最后一级：通过后将<b>自动授予</b>查询权限。' : `通过后流转到「${flowOf(a.rg)[a.level + 1].name}」继续审批。`}</div>`,
      onOk() {
        const note = $('#apNote').value.trim();
        a.approvals.push({lv: lv.name, by: ME.login, at: nowT(), act: '通过', note}); lg(a, ME.login, `${lv.name}通过${note ? '：' + note : ''}`);
        let msg;
        if (!last) { a.level++; msg = `已通过「${lv.name}」审批，${notifyTxt(a.rg, candTxt(flowOf(a.rg)[a.level], a.rg, a.user))}`; }
        else { a.status = '审批通过'; a.level = flowOf(a.rg).length; const gid = applyGrant(a); lg(a, '系统', `审批通过，已自动授予查询权限 #${gid}`); msg = `已授予，可在在线查询使用；${notifyTxt(a.rg, a.user)}`; }
        audit('qpriv.review', `审批通过查询权限申请 #${a.id}（${lv.name}）`); toast(msg); buildNav(); rerender();
      }});
  };

  /* ---------- 申请详情 ---------- */
  function apDetailPage(id) {
    const a = APPLIES.find(x => x.id == id), back = `<div class="row" style="margin-bottom:12px"><a class="btn" href="#qapply">← 返回列表</a></div>`;
    if (!a) return back + `<div class="card">${empty('找不到这条申请')}</div>`;
    if (!canSeeAp(a)) return back + `<div class="card">${empty('申请不存在，或你没有权限查看。', '🔒')}</div>`;
    const pending = a.status === '待审批', lv = apCurLv(a), steps = ['提交申请', ...flowOf(a.rg).map(l => l.name + '审批'), '授予权限'];
    const cur = pending ? 1 + a.level : a.status === '审批通过' || a.status === '已过期' ? steps.length : a.approvals.length + 1;
    let act = '';
    if (pending) {
      const me = a.user === ME.login, rv = canReview(a);
      act = `<div class="hr"></div>`;
      if (!me && !rv) act += `<div class="small muted" style="margin-bottom:8px">当前等待「${lv.name}」审批：${esc(candTxt(lv, a.rg, a.user))}。</div>`;
      act += `<div class="row">${me ? B('qpriv.apply', '撤回申请', {cls: 'danger', data: {op: 'withdraw', id: a.id}}) : ''}
        ${!me && (rv || can('qpriv.review') && eligible(ME, lv, a.rg)) ? B('qpriv.review', '驳回', {cls: 'danger', data: {op: 'reject', id: a.id}}) + B('qpriv.review', '通过', {cls: 'primary', data: {op: 'pass', id: a.id}}) : ''}
        ${me && can('qpriv.review') ? `<button class="btn noperm" disabled title="不能审批自己的申请">通过</button>` : ''}</div>`;
    }
    const grant = a.grantedId ? QPRIVS.find(p => p.id === a.grantedId) : null;
    const tl = flowTl(a, a.user, pending);
    if (a.status === '已撤回') tl.push({cls: 'bad', h: '申请人撤回', m: esc(a.endAt || '')});
    if (a.status === '审批通过') tl.push({cls: 'ok', h: '已自动授予查询权限', m: grant ? `权限 #${grant.id}` : ''});
    if (a.status === '已过期') tl.push({cls: 'skip', h: '权限已到期失效', m: esc(a.expire)});
    if (a.status === '审批驳回') tl.push({cls: 'bad', h: '流程终止', m: '申请被驳回'});
    return back + `<div class="row" style="margin-bottom:12px"><h2 style="margin:0;font-size:18px">权限申请 #${a.id}　${esc(a.db)}${a.tables.includes('*') ? '（全库）' : '.' + esc(a.tables.join('、'))}</h2>${apTag(a)}</div>
    ${stepsBar(steps, cur)}
    <div class="grid g-main" style="margin-bottom:16px"><div class="grid" style="align-content:start">
      <div class="card"><h2>申请信息</h2>${kvList([
        ['申请人', `${esc(userOf(a.user).name)}（${esc(a.user)}）`], ['部门', esc(userOf(a.user).dept)], ['资源组', esc(rgName(a.rg))], ['审批流程', flowText(a.rg)],
        ['目标实例', `${esc(instOf(a.inst).name)} <span class="tag">${esc(instOf(a.inst).env || '')}</span>`], ['权限类型', a.tables.includes('*') ? '库权限' : '表权限'],
        ['数据库', `<span class="mono">${esc(a.db)}</span>`], ['表', a.tables.includes('*') ? '（全库）' : `<span class="mono">${esc(a.tables.join('、'))}</span>`],
        ['返回行数上限', numFmt(a.limit)], ['有效期至', esc(a.expire)], ['提交时间', esc(a.created)], ['申请理由', esc(a.reason)]], 'c2')}
        ${grant ? notice(`已授予查询权限 #${grant.id}（${esc(grant.db)}，${tblTxt(grant.tables)}，${numFmt(grant.limit)} 行，至 ${grant.expire}）。<a href="#query">去在线查询</a> · <a href="#qprivs">查看查询权限</a>`, 'ok', '✓') : ''}
        ${a.status === '审批驳回' ? notice(`被驳回：${esc((a.approvals.find(p => p.act === '驳回') || {}).note || '')}。可调整后<a href="#qapply/new?inst=${a.inst}&db=${encodeURIComponent(a.db)}">重新申请</a>。`, 'bad', '✕') : ''}</div>
      <div class="card"><h2>操作日志</h2>${logHtml(a.logs)}</div></div>
      <div class="card" style="align-self:start"><h2>审批流程</h2>${tline(tl)}${act}</div></div>`;
  }
  pages.qapply = {
    title: sub => /^detail\//.test(sub || '') ? `权限申请 #${sub.split('/')[1]}` : /^new/.test(sub || '') ? '申请查询权限' : '权限管理',
    render(sub) {
      sub = sub || 'list'; const path = sub.split('?')[0].split('/');
      if (path[0] !== 'new') NA.qs = null;
      return path[0] === 'new' ? apNewPage(sub) : path[0] === 'detail' ? apDetailPage(path[1]) : apListPage();
    }
  };

  /* ================================================================ 查询权限页（qprivs） */
  const QP = {user: '', inst: '', db: '', q: ''};
  const seeAllPriv = () => can('qpriv.manage') || can('audit.view');
  const srcHtml = p => { const m = /申请 #(\d+)/.exec(p.src); return m ? `<a href="#qapply/detail/${m[1]}">${esc(p.src)}</a>` : `<span class="tag">${esc(p.src)}</span>`; };
  function privList() {
    return QPRIVS.filter(p => (seeAllPriv() || p.user === ME.login) && (!QP.user || p.user === QP.user) && (!QP.inst || p.inst == QP.inst) && (!QP.db || p.db === QP.db) &&
      (!QP.q || (p.user + p.db + p.tables.join(',') + instOf(p.inst).name).toLowerCase().includes(QP.q.toLowerCase())))
      .sort((a, b) => a.expire.localeCompare(b.expire));
  }
  function privRows() {
    const m = can('qpriv.manage');
    return tbl(['用户', '实例', '库', '表', '类型', '行数上限', '有效期至', '授予来源', '状态', ...(m ? ['操作'] : [])], privList(), {
      attrs: p => privState(p)[1] === '即将到期' ? ' style="background:var(--warn-soft)"' : privState(p)[1] === '已过期' ? ' style="opacity:.62"' : '',
      cells: p => { const [c, t] = privState(p), d = daysLeft(p.expire);
        return [uTxt(p.user), esc(instOf(p.inst).name), `<span class="mono">${esc(p.db)}</span>`, `<span class="mono">${p.tables.includes('*') ? '（全部）' : esc(p.tables.join('、'))}</span>`, p.tables.includes('*') ? '库权限' : '表权限',
          numFmt(p.limit), `${esc(p.expire)}<div class="small muted">${d < 0 ? '已过期 ' + -d + ' 天' : d === 0 ? '今天到期' : '剩余 ' + d + ' 天'}</div>`, srcHtml(p), `<span class="tag ${c}">${t}</span>`,
          ...(m ? [`<div class="ops">${B('qpriv.manage', '修改', {cls: 'sm', data: {op: 'edit', id: p.id}})}${B('qpriv.manage', '回收', {cls: 'sm danger', data: {op: 'revoke', id: p.id}})}</div>`] : [])]; }
    });
  }
  function privPage() {
    const all = QPRIVS.filter(p => seeAllPriv() || p.user === ME.login);
    const cnt = f => all.filter(f).length;
    const users = [...new Set(all.map(p => p.user))], insts = [...new Set(all.map(p => p.inst))];
    const dbs = [...new Set(all.filter(p => !QP.inst || p.inst == QP.inst).map(p => p.db))];
    return `<div class="grid g4" style="margin-bottom:14px">
      ${[['有效权限', cnt(p => privState(p)[1] !== '已过期'), ''], ['7 天内到期', cnt(p => privState(p)[1] === '即将到期'), 'warn'], ['已过期', cnt(p => privState(p)[1] === '已过期'), ''], [seeAllPriv() ? '涉及用户' : '涉及实例', seeAllPriv() ? users.length : insts.length, '']].map(([l, v, c]) => `<div class="card kpi"><div class="v" ${c === 'warn' && v ? 'style="color:var(--warn)"' : ''}>${v}</div><div class="l">${l}</div></div>`).join('')}</div>
    <div class="toolbar">
      ${seeAllPriv() ? `<select data-onchange="qpUser"><option value="">全部用户</option>${users.map(u => `<option ${u === QP.user ? 'selected' : ''}>${esc(u)}</option>`).join('')}</select>` : ''}
      <select data-onchange="qpInst"><option value="">全部实例</option>${insts.map(i => `<option value="${i}" ${QP.inst == i ? 'selected' : ''}>${esc(instOf(i).name)}</option>`).join('')}</select>
      <select data-onchange="qpDb"><option value="">全部库</option>${dbs.map(d => `<option ${d === QP.db ? 'selected' : ''}>${esc(d)}</option>`).join('')}</select>
      <input type="search" placeholder="搜索用户、库、表" value="${esc(QP.q)}" data-oninput="qpQ" style="min-width:200px">
      <span class="sp"></span><a class="btn" href="#qapply/new">申请权限</a>${B('qpriv.manage', '＋ 直接授予', {cls: 'primary', data: {op: 'grant'}})}</div>
    <div class="card" id="qpList">${privRows()}</div>`;
  }
  ON.qpUser = (d, el) => { QP.user = el.value; rerender(); };
  ON.qpInst = (d, el) => { QP.inst = el.value; QP.db = ''; rerender(); };
  ON.qpDb = (d, el) => { QP.db = el.value; rerender(); };
  ON.qpQ = (d, el) => { QP.q = el.value; $('#qpList').innerHTML = privRows(); };

  ACTIONS['qpriv.manage'] = d => {
    if (d.op === 'grant') return grantModal();
    const p = QPRIVS.find(x => x.id == d.id); if (!p) return;
    const nm = instOf(p.inst).name;
    if (d.op === 'edit') {
      return openModal({title: `修改查询权限 #${p.id} · ${p.user}`, code: 'qpriv.manage', body: `${kvList([['实例', esc(nm)], ['库 / 表', `<span class="mono">${esc(p.db)}</span> ${tblTxt(p.tables)}`]])}
        <div class="form" style="margin-top:10px"><label class="req">有效期至</label><input id="qmExp" value="${esc(p.expire)}" placeholder="YYYY-MM-DD"><label class="req">行数上限</label><input type="number" id="qmLim" min="1" max="1000000" value="${p.limit}"></div>`,
        onOk() {
          const e = $('#qmExp').value.trim(), l = parseInt($('#qmLim').value, 10);
          if (!isDate(e) || e < TODAY) { toast('有效期格式应为 YYYY-MM-DD，且不能早于今天'); return false; }
          if (!(l >= 1 && l <= 1000000)) { toast('行数上限需在 1 ~ 1,000,000 之间'); return false; }
          const old = `${p.expire} / ${p.limit}`; p.expire = e; p.limit = l;
          const ap = /申请 #(\d+)/.exec(p.src); if (ap) { const a = APPLIES.find(x => x.id == ap[1]); if (a) lg(a, ME.login, `修改已授予权限：有效期 ${e}，行数 ${l}`); }
          audit('qpriv.manage', `修改查询权限 #${p.id}（${p.user}）：${old} → ${e} / ${l}`, '成功', nm); toast(`已修改，${notifyTxt(rgOfInst(instOf(p.inst), userOf(p.user)), p.user)}`); rerender();
        }});
    }
    risky('qpriv.manage', `回收查询权限 #${p.id}`, `将回收 <b>${esc(p.user)}</b> 在 <b>${esc(nm)} / ${esc(p.db)}</b>（${tblTxt(p.tables)}）上的查询权限，该用户将无法再查询这些库表。`, `DELETE FROM dbt_query_privileges WHERE privilege_id = ${p.id};  -- ${p.user} @ ${nm}`, () => {
      QPRIVS.splice(QPRIVS.indexOf(p), 1);
      const ap = /申请 #(\d+)/.exec(p.src); if (ap) { const a = APPLIES.find(x => x.id == ap[1]); if (a) lg(a, ME.login, `已授予的权限 #${p.id} 被回收`); }
      audit('qpriv.manage', `回收查询权限 #${p.id}（${p.user} @ ${nm}/${p.db}）`, '成功', nm); toast(`已回收，${notifyTxt(rgOfInst(instOf(p.inst), userOf(p.user)), p.user)}`); rerender();
    });
  };
  function grantModal() {
    const insts = INSTANCES.filter(i => i.enabled !== false);
    openModal({title: '直接授予查询权限', code: 'qpriv.manage', wide: true, body: `<div class="form">
      <label class="req">用户</label><select id="gmUser">${USERS.filter(u => u.status === 'active' && !u.super).map(u => `<option value="${u.login}">${esc(u.login)}（${esc(u.name)}）</option>`).join('')}</select>
      <label class="req">实例</label><select id="gmInst">${instOpts(insts, insts[0].id)}</select>
      <label class="req">数据库</label><select id="gmDb"></select>
      <label>权限类型</label><div><label class="chk"><input type="radio" name="gmScope" value="db" checked> 库权限</label>　<label class="chk"><input type="radio" name="gmScope" value="table"> 表权限</label></div>
      <label>表</label><div class="row" id="gmTbls"></div>
      <label class="req">有效期至</label><input id="gmExp" value="${dayOffset(30)}" placeholder="YYYY-MM-DD">
      <label class="req">行数上限</label><input type="number" id="gmLim" min="1" max="1000000" value="1000"></div>`,
      onOk() {
        const user = $('#gmUser').value, inst = +$('#gmInst').value, db = $('#gmDb').value, scope = document.querySelector('input[name=gmScope]:checked').value;
        const tables = scope === 'db' ? ['*'] : $$('.gmT').filter(x => x.checked).map(x => x.value), e = $('#gmExp').value.trim(), l = parseInt($('#gmLim').value, 10);
        if (!db) { toast('请选择数据库'); return false; }
        if (scope === 'table' && isRds(inst)) { toast('Redis 只支持库级权限，不能授予表权限'); return false; }
        if (scope === 'table' && !tables.length) { toast('请至少选择一张表'); return false; }
        if (!isDate(e) || e < TODAY) { toast('有效期格式应为 YYYY-MM-DD，且不能早于今天'); return false; }
        if (!(l >= 1 && l <= 1000000)) { toast('行数上限需在 1 ~ 1,000,000 之间'); return false; }
        const id = nextQid(); QPRIVS.push({id, user, inst, db, tables, limit: l, expire: e, src: '管理员直接授予'});
        audit('qpriv.manage', `直接授予查询权限 #${id}：${user} @ ${instOf(inst).name}/${db}（${tblTxt(tables)}，${l} 行，至 ${e}）`, '成功', instOf(inst).name);
        toast(`已授予，可在在线查询使用；${notifyTxt(rgOfInst(instOf(inst), userOf(user)), user)}`); rerender();
      }});
    const fillDb = () => { const i = +$('#gmInst').value; $('#gmDb').innerHTML = pDbs(i).map(d => `<option>${esc(d.db)}</option>`).join(''); lockR(); fillTbl(); };
    const fillTbl = () => { const sc = document.querySelector('input[name=gmScope]:checked').value; $('#gmTbls').innerHTML = sc === 'db' ? '<span class="muted">（全库）</span>' : tablesOf(+$('#gmInst').value, $('#gmDb').value).map(t => `<label class="chk"><input type="checkbox" class="gmT" value="${esc(t.n)}"> <span class="mono">${esc(t.n)}</span></label>`).join(''); };
    function lockR() { const r = isRds(+$('#gmInst').value); document.querySelector('input[name=gmScope][value=table]').disabled = r; if (r) document.querySelector('input[name=gmScope][value=db]').checked = true; }
    $('#gmInst').onchange = fillDb; $('#gmDb').onchange = fillTbl; $$('input[name=gmScope]').forEach(r => r.onchange = fillTbl); fillDb();
  }
  pages.qprivs = {title: '查询权限', render() { return privPage(); }};

})();
