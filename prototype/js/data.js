/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: data.js
 * @time: 2026/10/28
 */
/* ============================================================================
   共享模拟数据：实例、资源组、库表结构、审计种子。
   页面独有的数据放在各自的 p_*.js 顶部。所有数据均为虚构，仅用于原型演示。
   “今天”固定为 2026-10-07，模拟数据的时间都围绕这一天生成。
   ========================================================================== */
const TODAY = '2026-10-07';

/* ---------- 资源组：团队 / 实例分组，也是审批流与通知的单位 ---------- */
const RGROUPS = [
  {id: 1, name: '订单中心', leader: 'tl_wang', bot: '钉钉', enabled: true, remark: '订单、履约、售后相关业务'},
  {id: 2, name: '支付中心', leader: 'tl_wang', bot: '飞书', enabled: true, remark: '支付、退款、对账'},
  {id: 3, name: '用户中心', leader: 'dba_li', bot: '钉钉', enabled: true, remark: '账号、会员、登录日志'},
  {id: 4, name: '数据平台', leader: 'dba_li', bot: 'Slack', enabled: true, remark: '数仓、报表、离线分析'},
  {id: 5, name: '历史项目组', leader: 'admin', bot: '', enabled: false, remark: '已停用'},
];
const rgName = id => (RGROUPS.find(r => r.id === id) || {name: '#' + id}).name;

/* ---------- 实例 ----------
   type：MySQL / MariaDB / MsSQL / Redis；role：master / slave；tags 含 read_view 时所有人只能查 v_ 开头的视图；
   schema：对应下面 SCHEMAS 的键；slowlog：是否开启慢日志采集 */
const INSTANCES = [
  {id: 1, name: 'mysql-order-prod-m', type: 'MySQL', ver: '8.0.35', role: 'master', host: '10.20.1.11', port: 3306, env: '生产', rgs: [1], tags: ['生产', '核心'], schema: 'order', slowlog: true, status: 'ok', enabled: true, qps: 1840, conns: 312, ssh: false, user: 'dbtool_ro'},
  {id: 2, name: 'mysql-order-prod-s1', type: 'MySQL', ver: '8.0.35', role: 'slave', host: '10.20.1.12', port: 3306, env: '生产', rgs: [1], tags: ['生产'], schema: 'order', slowlog: true, status: 'ok', enabled: true, qps: 960, conns: 141, ssh: false, user: 'dbtool_ro'},
  {id: 3, name: 'mysql-pay-prod-m', type: 'MySQL', ver: '5.7.42', role: 'master', host: '10.20.2.11', port: 3306, env: '生产', rgs: [2], tags: ['生产', '核心', '金融'], schema: 'pay', slowlog: true, status: 'ok', enabled: true, qps: 1220, conns: 205, ssh: false, user: 'dbtool_ro'},
  {id: 4, name: 'mysql-user-prod-m', type: 'MySQL', ver: '8.0.35', role: 'master', host: '10.20.3.11', port: 3306, env: '生产', rgs: [3], tags: ['生产'], schema: 'user', slowlog: true, status: 'warn', enabled: true, qps: 2310, conns: 488, ssh: false, user: 'dbtool_ro'},
  {id: 5, name: 'mysql-dw-prod-s', type: 'MySQL', ver: '8.0.28', role: 'slave', host: '10.20.4.21', port: 3307, env: '生产', rgs: [4], tags: ['数仓'], schema: 'order', slowlog: false, status: 'ok', enabled: true, qps: 180, conns: 36, ssh: true, user: 'dbtool_ro'},
  {id: 6, name: 'mssql-report-prod', type: 'MsSQL', ver: '2019 (15.0.4153)', role: 'master', host: '10.20.4.31', port: 1433, env: '生产', rgs: [4], tags: ['报表', '批量上线'], schema: 'mssql', slowlog: false, status: 'ok', enabled: true, qps: 95, conns: 54, ssh: false, user: 'sa_dbtool'},
  {id: 7, name: 'mysql-order-test', type: 'MySQL', ver: '8.0.35', role: 'master', host: '10.30.1.11', port: 3306, env: '测试', rgs: [1, 2], tags: ['测试'], schema: 'order', slowlog: true, status: 'ok', enabled: true, qps: 40, conns: 18, ssh: false, user: 'dbtool_rw'},
  {id: 8, name: 'mysql-order-view', type: 'MySQL', ver: '8.0.35', role: 'slave', host: '10.20.1.15', port: 3306, env: '生产', rgs: [1], tags: ['生产', 'read_view'], schema: 'view', slowlog: false, status: 'ok', enabled: true, qps: 60, conns: 9, ssh: false, user: 'dbtool_ro'},
  {id: 9, name: 'mariadb-log-prod', type: 'MariaDB', ver: '10.6.16', role: 'master', host: '10.20.5.11', port: 3306, env: '生产', rgs: [4], tags: ['日志'], schema: 'user', slowlog: false, status: 'bad', enabled: true, qps: 0, conns: 0, ssh: false, user: 'dbtool_ro'},
  {id: 10, name: 'mysql-legacy-old', type: 'MySQL', ver: '5.6.51', role: 'master', host: '10.20.9.11', port: 3306, env: '生产', rgs: [5], tags: ['下线中'], schema: 'user', slowlog: false, status: 'bad', enabled: false, qps: 0, conns: 0, ssh: false, user: 'dbtool_ro'},
  /* Redis（type: 'Redis'）：用于实例列表 / 性能巡检 / 慢查日志 / 在线查询（命令模式）/ 权限申请（只支持库级）；其余 SQL 类页面用 visibleInstances() 默认不含它们，上述页面传 true。user 为空 = 只用密码认证；pwdSet:false = 实例没有设置密码 */
  {id: 11, name: 'redis-order-cache-m', type: 'Redis', ver: '7.0.15', role: 'master', host: '10.20.1.31', port: 6379, env: '生产', rgs: [1], tags: ['生产', '缓存'], schema: '', slowlog: true, status: 'ok', enabled: true, qps: 24500, conns: 186, ssh: false, user: '', pwdSet: true, pwdAt: '2026-08-12 10:20'},
  {id: 12, name: 'redis-order-cache-s', type: 'Redis', ver: '7.0.15', role: 'slave', host: '10.20.1.32', port: 6379, env: '生产', rgs: [1], tags: ['生产', '缓存'], schema: '', slowlog: true, status: 'ok', enabled: true, qps: 5900, conns: 52, ssh: false, user: 'dbtool_ro', pwdSet: true, pwdAt: '2026-08-12 10:24'},
  {id: 13, name: 'redis-session-prod', type: 'Redis', ver: '6.2.14', role: 'master', host: '10.20.3.31', port: 6379, env: '生产', rgs: [3], tags: ['生产', '会话'], schema: '', slowlog: true, status: 'warn', enabled: true, qps: 38100, conns: 4812, ssh: false, user: '', pwdSet: false},
];
const instOf = id => INSTANCES.find(i => i.id == id) || {id, name: '#' + id, type: 'MySQL', rgs: [], tags: []};
const instOpts = (list, sel) => (list || visibleInstances()).map(i => `<option value="${i.id}" ${i.id == sel ? 'selected' : ''}>${esc(i.name)}（${i.type}${i.role === 'slave' ? ' · 从' : ''}）</option>`).join('');
const isReadView = inst => (inst.tags || []).includes('read_view');

/* ---------- 库表结构 ----------
   列：[名称, 类型, 可空, 键(PRI/UNI/MUL/''), 默认值, 注释, 敏感(脱敏)?]；索引：[名称, [列], 唯一?]
   MySQL 表以 {n, c, rows, size(MB), eng, cols, idx} 描述；视图 {n, c, view:true, cols} */
const COL = (n, t, nul, key, def, cmt, mask) => ({n, t, nul, key, def, cmt, mask: !!mask});
const SCHEMAS = {
  order: [
    {db: 'order_db', charset: 'utf8mb4', tables: [
      {n: 'orders', c: '订单主表', rows: 28460310, size: 9120, eng: 'InnoDB', cols: [
        COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('order_no', 'varchar(32)', 0, 'UNI', '', '订单号'), COL('user_id', 'bigint unsigned', 0, 'MUL', '', '用户ID'),
        COL('status', 'tinyint', 0, 'MUL', '0', '状态 0待支付 1已支付 2已发货 3完成 4取消'), COL('amount', 'decimal(12,2)', 0, '', '0.00', '订单金额'), COL('receiver_phone', 'varchar(20)', 1, '', 'NULL', '收货人手机号', true),
        COL('receiver_addr', 'varchar(255)', 1, '', 'NULL', '收货地址', true), COL('created_at', 'datetime', 0, 'MUL', 'CURRENT_TIMESTAMP', '创建时间'), COL('updated_at', 'datetime', 0, '', 'CURRENT_TIMESTAMP', '更新时间')],
        idx: [['PRIMARY', ['id'], true], ['uk_order_no', ['order_no'], true], ['idx_user_created', ['user_id', 'created_at'], false], ['idx_status', ['status'], false]]},
      {n: 'order_items', c: '订单明细', rows: 71230887, size: 18400, eng: 'InnoDB', cols: [
        COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('order_id', 'bigint unsigned', 0, 'MUL', '', '订单ID'), COL('sku_id', 'bigint unsigned', 0, 'MUL', '', 'SKU'),
        COL('sku_name', 'varchar(128)', 0, '', '', '商品名称'), COL('qty', 'int', 0, '', '1', '数量'), COL('price', 'decimal(10,2)', 0, '', '0.00', '单价')],
        idx: [['PRIMARY', ['id'], true], ['idx_order', ['order_id'], false], ['idx_sku', ['sku_id'], false]]},
      {n: 'order_log', c: '订单流水日志（按月分区）', rows: 152300440, size: 41200, eng: 'InnoDB', part: true, cols: [
        COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('order_id', 'bigint unsigned', 0, 'MUL', '', '订单ID'), COL('action', 'varchar(32)', 0, '', '', '动作'),
        COL('operator', 'varchar(64)', 1, '', 'NULL', '操作人'), COL('created_at', 'datetime', 0, 'PRI', 'CURRENT_TIMESTAMP', '发生时间')],
        idx: [['PRIMARY', ['id', 'created_at'], true], ['idx_order', ['order_id'], false]]},
      {n: 'user_addr', c: '用户收货地址', rows: 6120044, size: 1560, eng: 'InnoDB', cols: [
        COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('user_id', 'bigint unsigned', 0, 'MUL', '', '用户ID'), COL('name', 'varchar(32)', 0, '', '', '收件人', true),
        COL('phone', 'varchar(20)', 0, '', '', '手机号', true), COL('detail', 'varchar(255)', 0, '', '', '详细地址', true)],
        idx: [['PRIMARY', ['id'], true], ['idx_user', ['user_id'], false]]},
      {n: 'v_order_summary', c: '订单汇总视图', view: true, cols: [COL('day', 'date', 0, '', '', '日期'), COL('cnt', 'bigint', 0, '', '', '订单数'), COL('gmv', 'decimal(14,2)', 0, '', '', '成交额')]},
    ], procs: [{n: 'sp_close_expired_orders', t: 'PROCEDURE'}, {n: 'fn_order_fee', t: 'FUNCTION'}], events: [{n: 'ev_archive_order_log', sched: 'EVERY 1 DAY', st: 'ENABLED'}], triggers: [{n: 'trg_orders_au', ev: 'AFTER UPDATE', tbl: 'orders'}]},
    {db: 'order_archive', charset: 'utf8mb4', tables: [
      {n: 'orders_2025', c: '2025 年订单归档', rows: 41200800, size: 12800, eng: 'InnoDB', cols: [
        COL('id', 'bigint unsigned', 0, 'PRI', '', '主键'), COL('order_no', 'varchar(32)', 0, 'UNI', '', '订单号'), COL('amount', 'decimal(12,2)', 0, '', '0.00', '订单金额'), COL('created_at', 'datetime', 0, 'MUL', '', '创建时间')],
        idx: [['PRIMARY', ['id'], true], ['uk_order_no', ['order_no'], true]]},
    ], procs: [], events: [], triggers: []},
  ],
  pay: [
    {db: 'pay_db', charset: 'utf8mb4', tables: [
      {n: 'payments', c: '支付单', rows: 33180122, size: 8800, eng: 'InnoDB', cols: [
        COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('pay_no', 'varchar(40)', 0, 'UNI', '', '支付流水号'), COL('order_no', 'varchar(32)', 0, 'MUL', '', '订单号'),
        COL('channel', 'varchar(16)', 0, '', '', '渠道 alipay/wechat/card'), COL('amount', 'decimal(12,2)', 0, '', '0.00', '金额'), COL('status', 'tinyint', 0, 'MUL', '0', '状态'), COL('paid_at', 'datetime', 1, 'MUL', 'NULL', '支付时间')],
        idx: [['PRIMARY', ['id'], true], ['uk_pay_no', ['pay_no'], true], ['idx_order', ['order_no'], false], ['idx_status_paid', ['status', 'paid_at'], false]]},
      {n: 'refunds', c: '退款单', rows: 1820340, size: 520, eng: 'InnoDB', cols: [
        COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('refund_no', 'varchar(40)', 0, 'UNI', '', '退款流水号'), COL('pay_no', 'varchar(40)', 0, 'MUL', '', '原支付流水'), COL('amount', 'decimal(12,2)', 0, '', '0.00', '退款金额'), COL('created_at', 'datetime', 0, '', 'CURRENT_TIMESTAMP', '创建时间')],
        idx: [['PRIMARY', ['id'], true], ['uk_refund_no', ['refund_no'], true], ['idx_pay', ['pay_no'], false]]},
      {n: 'bank_cards', c: '绑定银行卡', rows: 4410220, size: 980, eng: 'InnoDB', cols: [
        COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('user_id', 'bigint unsigned', 0, 'MUL', '', '用户ID'), COL('card_no', 'varchar(32)', 0, '', '', '银行卡号', true), COL('holder', 'varchar(32)', 0, '', '', '持卡人', true), COL('id_card', 'varchar(24)', 1, '', 'NULL', '身份证号', true)],
        idx: [['PRIMARY', ['id'], true], ['idx_user', ['user_id'], false]]},
    ], procs: [{n: 'sp_daily_reconcile', t: 'PROCEDURE'}], events: [{n: 'ev_reconcile', sched: 'EVERY 1 DAY', st: 'ENABLED'}], triggers: []},
  ],
  user: [
    {db: 'user_db', charset: 'utf8mb4', tables: [
      {n: 'users', c: '用户表', rows: 18230455, size: 4300, eng: 'InnoDB', cols: [
        COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('username', 'varchar(64)', 0, 'UNI', '', '用户名'), COL('phone', 'varchar(20)', 1, 'UNI', 'NULL', '手机号', true), COL('email', 'varchar(128)', 1, 'MUL', 'NULL', '邮箱', true),
        COL('level', 'tinyint', 0, '', '0', '会员等级'), COL('created_at', 'datetime', 0, '', 'CURRENT_TIMESTAMP', '注册时间')],
        idx: [['PRIMARY', ['id'], true], ['uk_username', ['username'], true], ['uk_phone', ['phone'], true], ['idx_email', ['email'], false]]},
      {n: 'login_log', c: '登录日志', rows: 389002114, size: 52000, eng: 'InnoDB', part: true, cols: [
        COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('user_id', 'bigint unsigned', 0, 'MUL', '', '用户ID'), COL('ip', 'varchar(45)', 0, '', '', '登录 IP'), COL('ua', 'varchar(255)', 1, '', 'NULL', 'User-Agent'), COL('created_at', 'datetime', 0, 'PRI', 'CURRENT_TIMESTAMP', '登录时间')],
        idx: [['PRIMARY', ['id', 'created_at'], true], ['idx_user_time', ['user_id', 'created_at'], false]]},
      {n: 'user_profile', c: '用户资料', rows: 17002210, size: 3200, eng: 'InnoDB', cols: [
        COL('user_id', 'bigint unsigned', 0, 'PRI', '', '用户ID'), COL('real_name', 'varchar(32)', 1, '', 'NULL', '真实姓名', true), COL('id_card', 'varchar(24)', 1, '', 'NULL', '身份证号', true), COL('birthday', 'date', 1, '', 'NULL', '生日')],
        idx: [['PRIMARY', ['user_id'], true]]},
    ], procs: [], events: [], triggers: []},
  ],
  mssql: [
    {db: 'ReportDW', charset: 'Chinese_PRC_CI_AS', schema: 'dbo', tables: [
      {n: 'fact_sales', c: '销售事实表', rows: 98210044, size: 22000, eng: 'ROWSTORE', cols: [
        COL('sale_id', 'bigint', 0, 'PRI', '', '主键'), COL('date_key', 'int', 0, 'MUL', '', '日期键'), COL('customer_key', 'int', 0, 'MUL', '', '客户键'), COL('amount', 'decimal(14,2)', 0, '', '0', '金额')],
        idx: [['PK_fact_sales', ['sale_id'], true], ['IX_fact_date', ['date_key'], false]]},
      {n: 'dim_customer', c: '客户维度', rows: 2310220, size: 640, eng: 'ROWSTORE', cols: [
        COL('customer_key', 'int', 0, 'PRI', '', '客户键'), COL('name', 'nvarchar(64)', 0, '', '', '名称'), COL('phone', 'varchar(20)', 1, '', 'NULL', '手机号', true), COL('region', 'nvarchar(32)', 1, '', 'NULL', '地区')],
        idx: [['PK_dim_customer', ['customer_key'], true]]},
    ], procs: [{n: 'usp_load_fact_sales', t: 'PROCEDURE'}], events: [], triggers: []},
  ],
  view: [
    {db: 'order_db', charset: 'utf8mb4', tables: [
      {n: 'v_orders', c: '订单只读视图（已脱敏）', view: true, cols: [COL('id', 'bigint unsigned', 0, '', '', '主键'), COL('order_no', 'varchar(32)', 0, '', '', '订单号'), COL('status', 'tinyint', 0, '', '', '状态'), COL('amount', 'decimal(12,2)', 0, '', '', '订单金额'), COL('created_at', 'datetime', 0, '', '', '创建时间')]},
      {n: 'v_order_summary', c: '订单汇总视图', view: true, cols: [COL('day', 'date', 0, '', '', '日期'), COL('cnt', 'bigint', 0, '', '', '订单数'), COL('gmv', 'decimal(14,2)', 0, '', '', '成交额')]},
      {n: 'orders', c: '订单主表（read_view 实例上不可直接查询）', rows: 28460310, size: 9120, eng: 'InnoDB', cols: [COL('id', 'bigint unsigned', 0, 'PRI', '', '主键')], idx: [['PRIMARY', ['id'], true]]},
    ], procs: [], events: [], triggers: []},
  ],
};
/* 实例 → 库列表 / 表 / 建表语句 */
const dbsOf = instId => SCHEMAS[instOf(instId).schema] || [];
const tablesOf = (instId, db) => ((dbsOf(instId).find(d => d.db === db) || {}).tables) || [];
const findTable = (instId, db, name) => tablesOf(instId, db).find(t => t.n === name);
function ddlOf(instId, db, t) {
  const inst = instOf(instId);
  if (!t) return '';
  if (t.view) return `CREATE VIEW \`${t.n}\` AS\nSELECT ${t.cols.map(c => c.n).join(', ')}\nFROM \`${db}\`.\`${t.n.replace(/^v_/, '')}\`;`;
  if (inst.type === 'MsSQL') return `CREATE TABLE [dbo].[${t.n}] (\n${t.cols.map(c => `  [${c.n}] ${c.t}${c.nul ? ' NULL' : ' NOT NULL'}`).join(',\n')},\n  CONSTRAINT [PK_${t.n}] PRIMARY KEY CLUSTERED ([${t.cols[0].n}])\n);`;
  return `CREATE TABLE \`${t.n}\` (\n${t.cols.map(c => `  \`${c.n}\` ${c.t}${c.nul ? ' DEFAULT NULL' : ' NOT NULL'}${c.def === 'auto_increment' ? ' AUTO_INCREMENT' : c.def && c.def !== 'NULL' && c.def !== '' ? ` DEFAULT ${/^(CURRENT_TIMESTAMP|\d+(\.\d+)?)$/.test(c.def) ? c.def : "'" + c.def + "'"}` : ''}${c.cmt ? ` COMMENT '${c.cmt}'` : ''}`).join(',\n')},\n${(t.idx || []).map(x => x[0] === 'PRIMARY' ? `  PRIMARY KEY (${x[1].map(c => '`' + c + '`').join(',')})` : `  ${x[2] ? 'UNIQUE ' : ''}KEY \`${x[0]}\` (${x[1].map(c => '`' + c + '`').join(',')})`).join(',\n')}\n) ENGINE=${t.eng || 'InnoDB'} DEFAULT CHARSET=${(dbsOf(instId).find(d => d.db === db) || {}).charset || 'utf8mb4'} COMMENT='${t.c}';`;
}
const fmtN = n => n >= 1e8 ? (n / 1e8).toFixed(2) + ' 亿' : n >= 1e4 ? (n / 1e4).toFixed(1) + ' 万' : String(n);
const fmtMB = mb => mb >= 1024 ? (mb / 1024).toFixed(1) + ' GB' : mb + ' MB';

/* ---------- 系统审计（种子；各页面操作通过 audit() 追加到最前） ---------- */
const AUDIT = [
  {at: '2026-10-07 10:31', user: 'tl_wang', ip: '10.1.3.21', module: '登录', code: 'auth.login', act: '登录成功（LDAP）', res: '成功', extra: ''},
  {at: '2026-10-07 10:08', user: 'rd_zhao', ip: '10.1.3.36', module: 'SQL 上线', code: 'sqlworkflow.submit', act: '提交工单 #1042「订单表增加字段 refund_flag」', res: '成功', extra: 'mysql-order-prod-m / order_db'},
  {at: '2026-10-07 09:52', user: 'dba_li', ip: '10.1.2.15', module: '实例管理', code: 'session.kill', act: '终止会话 thread_id=88231', res: '成功', extra: 'mysql-user-prod-m'},
  {at: '2026-10-07 09:12', user: 'admin', ip: '10.1.1.2', module: '登录', code: 'auth.login', act: '登录成功（账号密码 + TOTP）', res: '成功', extra: ''},
  {at: '2026-10-07 08:47', user: 'dba_li', ip: '10.1.2.15', module: '登录', code: 'auth.login', act: '登录成功（LDAP + TOTP）', res: '成功', extra: ''},
  {at: '2026-10-06 18:20', user: 'rd_zhou', ip: '10.1.4.8', module: '权限管理', code: 'qpriv.apply', act: '申请查询权限 pay_db.payments（7 天，10000 行）', res: '成功', extra: 'mysql-pay-prod-m'},
  {at: '2026-10-06 17:30', user: 'unknown', ip: '203.0.113.77', module: '登录', code: 'auth.login', act: '登录失败（账号或密码错误）×5，账号 dev_liu 已锁定', res: '失败', extra: ''},
  {at: '2026-10-06 16:41', user: 'dba_li', ip: '10.1.2.15', module: '账号管理', code: 'account.grant', act: '授权 report_ro@10.20.%：GRANT SELECT ON ReportDW.*', res: '成功', extra: 'mssql-report-prod'},
  {at: '2026-10-06 15:03', user: 'admin', ip: '10.1.1.2', module: '权限组管理', code: 'ag.btnperm', act: '切换按钮权限生效模式 兼容 → 兼容（预检通过，无变化）', res: '成功', extra: ''},
  {at: '2026-10-06 11:12', user: 'tl_wang', ip: '10.1.3.21', module: 'SQL 上线', code: 'sqlworkflow.approve', act: '审批通过工单 #1039', res: '成功', extra: 'mysql-pay-prod-m'},
];
const SELF_IP = '10.1.1.2';
function audit(code, act, res = '成功', extra = '') {
  const meta = BTNS[code];
  AUDIT.unshift({at: now(), user: ME.login, ip: ME.ip || SELF_IP, module: meta ? meta.pageName : '—', code, act, res, extra});
}

/* ============================================================================
   多引擎实例（PostgreSQL / MongoDB / ClickHouse）：第一期只支持「在线查询」和「数据字典」。
   这些实例默认不出现在其它 SQL 类页面（见 perm.js 的 visibleInstances：withRedis = true 只带 Redis，'ext' 带这三类，'all' 全带）。
   查询模拟见 p_engines.js（window.EngineQuery）。type 取值与 internal/instmgr/types.go 的显示名一致：PgSQL / Mongo / ClickHouse。
   ========================================================================== */
const EXT_TYPES = ['PgSQL', 'Mongo', 'ClickHouse'];
const isExtInst = inst => !!inst && EXT_TYPES.includes(inst.type);
const EXT_LABEL = {PgSQL: 'PostgreSQL', Mongo: 'MongoDB', ClickHouse: 'ClickHouse'};
const EXT_SCOPE = '在线查询、数据字典';
INSTANCES.push(
  {id: 14, name: 'pg-order-prod-m', type: 'PgSQL', ver: '15.4', role: 'master', host: '10.20.1.41', port: 5432, env: '生产', rgs: [1], tags: ['生产', '订单'], schema: 'pg_order', slowlog: false, status: 'ok', enabled: true, qps: 860, conns: 74, ssh: false, user: 'dbtool_ro', pwdSet: true, pwdAt: '2026-09-02 14:08'},
  {id: 15, name: 'pg-bi-prod-m', type: 'PgSQL', ver: '14.9', role: 'master', host: '10.20.4.41', port: 5432, env: '生产', rgs: [4], tags: ['生产', '数仓'], schema: 'pg_bi', slowlog: false, status: 'ok', enabled: true, qps: 210, conns: 31, ssh: false, user: 'dbtool_ro', pwdSet: true, pwdAt: '2026-09-02 14:12'},
  {id: 16, name: 'mongo-order-prod', type: 'Mongo', ver: '6.0.12', role: 'master', host: '10.20.1.51', port: 27017, env: '生产', rgs: [1], tags: ['生产', '订单'], schema: 'mongo_order', slowlog: false, status: 'ok', enabled: true, qps: 1420, conns: 126, ssh: false, user: 'dbtool_ro', pwdSet: true, pwdAt: '2026-09-05 10:30'},
  {id: 17, name: 'mongo-user-prod', type: 'Mongo', ver: '5.0.21', role: 'master', host: '10.20.3.51', port: 27017, env: '生产', rgs: [3], tags: ['生产', '用户'], schema: 'mongo_user', slowlog: false, status: 'ok', enabled: true, qps: 980, conns: 88, ssh: false, user: 'dbtool_ro', pwdSet: true, pwdAt: '2026-09-05 10:34'},
  {id: 18, name: 'ch-order-olap', type: 'ClickHouse', ver: '23.8.9.54', role: 'master', host: '10.20.1.61', port: 8123, env: '生产', rgs: [1, 4], tags: ['生产', '分析'], schema: 'ch_order', slowlog: false, status: 'ok', enabled: true, qps: 140, conns: 22, ssh: false, user: 'dbtool_ro', pwdSet: true, pwdAt: '2026-09-08 16:20'},
  {id: 19, name: 'ch-dw-prod', type: 'ClickHouse', ver: '24.3.5.47', role: 'master', host: '10.20.4.61', port: 8123, env: '生产', rgs: [4], tags: ['生产', '数仓'], schema: 'ch_dw', slowlog: false, status: 'warn', enabled: true, qps: 75, conns: 12, ssh: false, user: 'dbtool_ro', pwdSet: true, pwdAt: '2026-09-08 16:26'},
);

/* 库表种子。t.n：PgSQL 为 schema.表名；Mongo 为集合名；ClickHouse 为表名。t.kind：view / matview / dict；t.sk / t.pk / t.part：ClickHouse 的排序键 / 主键 / 分区键。
   列类型用各引擎自己的类型名（PgSQL = format_type 的写法，Mongo = 采样得到的 BSON 类型，ClickHouse = 列类型）。这三类第一期都没有列脱敏规则，所以不标敏感列。 */
Object.assign(SCHEMAS, {
  pg_order: [
    {db: 'order_core', charset: 'UTF8', tables: [
      {n: 'public.orders', c: '订单主表', rows: 18420550, size: 7340, eng: 'heap', own: 'app_order', cols: [
        COL('id', 'bigint', 0, 'PRI', 'identity', '主键（GENERATED ALWAYS AS IDENTITY）'), COL('order_no', 'character varying(32)', 0, 'UNI', '', '订单号'), COL('user_id', 'bigint', 0, 'MUL', '', '用户 ID'),
        COL('status', 'smallint', 0, 'MUL', '0', '状态 0待支付 1已支付 2已发货 3完成 4取消'), COL('amount', 'numeric(12,2)', 0, '', '0.00', '订单金额'), COL('currency', 'character(3)', 0, '', "'CNY'::bpchar", '币种'),
        COL('channel', 'text', 1, '', '', '下单渠道 app / h5 / mini'), COL('ext', 'jsonb', 1, '', '', '扩展信息（优惠、设备等）'), COL('tags', 'text[]', 1, '', '', '标签'),
        COL('trace_no', 'bigint', 1, '', '', '全链路追踪号（雪花 ID，超过 2^53 的值在结果里以字符串返回）'), COL('created_at', 'timestamp with time zone', 0, 'MUL', 'now()', '创建时间'),
        COL('updated_at', 'timestamp with time zone', 0, '', 'now()', '更新时间'), COL('paid_at', 'timestamp with time zone', 1, '', '', '支付时间')],
        idx: [['orders_pkey', ['id'], true], ['uk_orders_order_no', ['order_no'], true], ['idx_orders_user_created', ['user_id', 'created_at'], false], ['idx_orders_status', ['status'], false], ['idx_orders_ext_gin', ['ext'], false]]},
      {n: 'public.order_items', c: '订单明细', rows: 52330190, size: 14800, eng: 'heap', own: 'app_order', cols: [
        COL('id', 'bigint', 0, 'PRI', 'identity', '主键'), COL('order_id', 'bigint', 0, 'MUL', '', '订单 ID'), COL('sku_id', 'bigint', 0, 'MUL', '', 'SKU'), COL('sku_name', 'text', 0, '', '', '商品名称'),
        COL('qty', 'integer', 0, '', '1', '数量'), COL('price', 'numeric(10,2)', 0, '', '0.00', '单价'), COL('attrs', 'jsonb', 1, '', '', '规格属性'), COL('created_at', 'timestamp with time zone', 0, '', 'now()', '创建时间')],
        idx: [['order_items_pkey', ['id'], true], ['idx_items_order', ['order_id'], false], ['idx_items_sku', ['sku_id'], false]]},
      {n: 'public.order_status', c: '订单状态字典', rows: 5, size: 0, eng: 'heap', own: 'app_order', cols: [
        COL('status', 'smallint', 0, 'PRI', '', '状态码'), COL('name', 'text', 0, '', '', '状态名'), COL('remark', 'text', 1, '', '', '说明')], idx: [['order_status_pkey', ['status'], true]]},
      {n: 'public.attachments', c: '订单附件（凭证、发票）', rows: 2481330, size: 3900, eng: 'heap', own: 'app_order', cols: [
        COL('id', 'uuid', 0, 'PRI', 'gen_random_uuid()', '主键'), COL('order_id', 'bigint', 0, 'MUL', '', '订单 ID'), COL('filename', 'text', 0, '', '', '文件名'), COL('content_type', 'text', 1, '', '', 'MIME 类型'),
        COL('size_bytes', 'integer', 0, '', '0', '大小（字节）'), COL('digest', 'bytea', 1, '', '', 'SHA-256 摘要'), COL('created_at', 'timestamp with time zone', 0, '', 'now()', '创建时间')],
        idx: [['attachments_pkey', ['id'], true], ['idx_attach_order', ['order_id'], false]]},
      {n: 'audit.order_log', c: '订单流水日志（按月分区）', rows: 96220480, size: 21800, eng: 'heap', own: 'app_audit', part: true, cols: [
        COL('id', 'bigint', 0, 'PRI', 'identity', '主键'), COL('order_id', 'bigint', 0, 'MUL', '', '订单 ID'), COL('action', 'character varying(32)', 0, '', '', '动作'), COL('operator', 'text', 1, '', '', '操作人'),
        COL('detail', 'jsonb', 1, '', '', '变更详情'), COL('created_at', 'timestamp with time zone', 0, 'PRI', 'now()', '发生时间')],
        idx: [['order_log_pkey', ['id', 'created_at'], true], ['idx_order_log_order', ['order_id'], false]]},
      {n: 'public.v_order_daily', c: '每日订单汇总视图', view: true, kind: 'view', own: 'app_order', cols: [COL('day', 'date', 0, '', '', '日期'), COL('cnt', 'bigint', 0, '', '', '订单数'), COL('gmv', 'numeric', 0, '', '', '成交额')]},
      {n: 'public.mv_sku_rank', c: 'SKU 销量榜（物化视图，每小时刷新）', view: true, kind: 'matview', own: 'app_order', rows: 48210, size: 12, cols: [
        COL('sku_id', 'bigint', 0, '', '', 'SKU'), COL('sku_name', 'text', 0, '', '', '商品名称'), COL('qty', 'bigint', 0, '', '', '近 30 天销量'), COL('gmv', 'numeric', 0, '', '', '近 30 天成交额')]},
    ]},
    {db: 'order_report', charset: 'UTF8', tables: [
      {n: 'report.daily_sales', c: '每日销售汇总', rows: 41820, size: 6, eng: 'heap', own: 'bi_etl', cols: [
        COL('day', 'date', 0, 'PRI', '', '日期'), COL('channel', 'text', 0, 'PRI', '', '渠道'), COL('orders', 'bigint', 0, '', '0', '订单数'), COL('gmv', 'numeric(14,2)', 0, '', '0.00', '成交额'), COL('refund', 'numeric(14,2)', 0, '', '0.00', '退款额')],
        idx: [['daily_sales_pkey', ['day', 'channel'], true]]},
      {n: 'report.sku_stat', c: 'SKU 销售统计', rows: 48210, size: 9, eng: 'heap', own: 'bi_etl', cols: [
        COL('sku_id', 'bigint', 0, 'PRI', '', 'SKU'), COL('sku_name', 'text', 0, '', '', '商品名称'), COL('sales_30d', 'bigint', 0, '', '0', '近 30 天销量'), COL('stock', 'integer', 1, '', '', '库存'), COL('updated_at', 'timestamp with time zone', 0, '', 'now()', '更新时间')],
        idx: [['sku_stat_pkey', ['sku_id'], true]]},
    ]},
  ],
  pg_bi: [
    {db: 'bi_dw', charset: 'UTF8', tables: [
      {n: 'dw.fact_orders', c: '订单事实表', rows: 18398200, size: 5200, eng: 'heap', own: 'bi_etl', cols: [
        COL('order_id', 'bigint', 0, 'PRI', '', '订单 ID'), COL('date_key', 'integer', 0, 'MUL', '', '日期键 yyyymmdd'), COL('user_key', 'bigint', 0, 'MUL', '', '用户键'), COL('channel', 'text', 1, '', '', '渠道'),
        COL('amount', 'numeric(12,2)', 0, '', '0.00', '金额'), COL('status', 'smallint', 0, '', '0', '状态'), COL('paid_at', 'timestamp without time zone', 1, '', '', '支付时间（无时区）')],
        idx: [['fact_orders_pkey', ['order_id'], true], ['idx_fact_date', ['date_key'], false], ['idx_fact_user', ['user_key'], false]]},
      {n: 'dw.dim_user', c: '用户维度', rows: 2310880, size: 640, eng: 'heap', own: 'bi_etl', cols: [
        COL('user_key', 'bigint', 0, 'PRI', '', '用户键'), COL('level', 'smallint', 0, '', '0', '会员等级'), COL('city', 'text', 1, '', '', '城市'), COL('reg_date', 'date', 1, '', '', '注册日期'), COL('tags', 'text[]', 1, '', '', '用户标签')],
        idx: [['dim_user_pkey', ['user_key'], true]]},
      {n: 'dw.dim_date', c: '日期维度', rows: 3652, size: 1, eng: 'heap', own: 'bi_etl', cols: [
        COL('date_key', 'integer', 0, 'PRI', '', '日期键'), COL('day', 'date', 0, '', '', '日期'), COL('week_no', 'smallint', 0, '', '', '周'), COL('is_workday', 'boolean', 0, '', 'true', '是否工作日')],
        idx: [['dim_date_pkey', ['date_key'], true]]},
    ]},
    {db: 'bi_ods', charset: 'UTF8', tables: [
      {n: 'ods.orders_raw', c: '订单原始报文（ODS）', rows: 18420550, size: 11400, eng: 'heap', own: 'bi_etl', cols: [
        COL('id', 'bigint', 0, 'PRI', 'identity', '主键'), COL('payload', 'jsonb', 0, '', '', '原始报文'), COL('source', 'text', 0, '', '', '来源系统'), COL('loaded_at', 'timestamp with time zone', 0, 'MUL', 'now()', '入库时间')],
        idx: [['orders_raw_pkey', ['id'], true], ['idx_raw_loaded', ['loaded_at'], false]]},
    ]},
  ],
  mongo_order: [
    {db: 'order_mongo', charset: '', tables: [
      {n: 'orders', c: '订单文档（含明细与收货地址）', rows: 12480330, size: 9800, eng: 'WiredTiger', cols: [
        COL('_id', 'objectId', 0, 'PRI', '', '采样 100 个文档，100 个含该字段'), COL('orderNo', 'string', 0, 'UNI', '', '采样 100 个文档，100 个含该字段'), COL('userId', 'long', 0, 'MUL', '', '采样 100 个文档，100 个含该字段'),
        COL('status', 'int', 0, 'MUL', '', '采样 100 个文档，100 个含该字段'), COL('amount', 'decimal', 0, '', '', '采样 100 个文档，100 个含该字段'), COL('items', 'array', 0, '', '', '采样 100 个文档，100 个含该字段'),
        COL('address', 'object', 1, '', '', '采样 100 个文档，94 个含该字段'), COL('couponId', 'objectId|null', 1, '', '', '采样 100 个文档，37 个含该字段'), COL('payChannel', 'string', 1, '', '', '采样 100 个文档，92 个含该字段'),
        COL('createdAt', 'date', 0, 'MUL', '', '采样 100 个文档，100 个含该字段'), COL('paidAt', 'date|null', 1, '', '', '采样 100 个文档，88 个含该字段')],
        idx: [['_id_', ['_id'], true], ['orderNo_1', ['orderNo'], true], ['userId_1_createdAt_-1', ['userId', 'createdAt'], false], ['status_1', ['status'], false]]},
      {n: 'order_events', c: '订单事件流', rows: 86210044, size: 31200, eng: 'WiredTiger', cols: [
        COL('_id', 'objectId', 0, 'PRI', '', '采样 100 个文档，100 个含该字段'), COL('orderId', 'objectId', 0, 'MUL', '', '采样 100 个文档，100 个含该字段'), COL('type', 'string', 0, '', '', '采样 100 个文档，100 个含该字段'),
        COL('operator', 'string|null', 1, '', '', '采样 100 个文档，83 个含该字段'), COL('payload', 'object', 1, '', '', '采样 100 个文档，79 个含该字段'), COL('ts', 'date', 0, 'MUL', '', '采样 100 个文档，100 个含该字段')],
        idx: [['_id_', ['_id'], true], ['orderId_1_ts_1', ['orderId', 'ts'], false]]},
      {n: 'carts', c: '购物车', rows: 4820110, size: 2100, eng: 'WiredTiger', cols: [
        COL('_id', 'objectId', 0, 'PRI', '', '采样 100 个文档，100 个含该字段'), COL('userId', 'long', 0, 'UNI', '', '采样 100 个文档，100 个含该字段'), COL('skus', 'array', 0, '', '', '采样 100 个文档，100 个含该字段'),
        COL('updatedAt', 'date', 0, '', '', '采样 100 个文档，100 个含该字段'), COL('coupon', 'string', 1, '', '', '采样 100 个文档，21 个含该字段')],
        idx: [['_id_', ['_id'], true], ['userId_1', ['userId'], true]]},
      {n: 'coupons', c: '优惠券模板', rows: 1840, size: 1, eng: 'WiredTiger', cols: [
        COL('_id', 'objectId', 0, 'PRI', '', '采样 100 个文档，100 个含该字段'), COL('code', 'string', 0, 'UNI', '', '采样 100 个文档，100 个含该字段'), COL('discount', 'double', 0, '', '', '采样 100 个文档，100 个含该字段'),
        COL('minAmount', 'int', 1, '', '', '采样 100 个文档，64 个含该字段'), COL('validTo', 'date', 0, '', '', '采样 100 个文档，100 个含该字段'), COL('enabled', 'bool', 0, '', '', '采样 100 个文档，100 个含该字段')],
        idx: [['_id_', ['_id'], true], ['code_1', ['code'], true]]},
      {n: 'v_order_summary', c: '视图，源集合 orders', view: true, kind: 'view', cols: [
        COL('_id', 'string', 0, '', '', '采样 30 个文档，30 个含该字段'), COL('cnt', 'int', 0, '', '', '采样 30 个文档，30 个含该字段'), COL('gmv', 'decimal', 0, '', '', '采样 30 个文档，30 个含该字段')]},
    ]},
    {db: 'app_log', charset: '', tables: [
      {n: 'api_logs', c: '接口访问日志（TTL 7 天）', rows: 412880330, size: 88400, eng: 'WiredTiger', cols: [
        COL('_id', 'objectId', 0, 'PRI', '', '采样 100 个文档，100 个含该字段'), COL('path', 'string', 0, '', '', '采样 100 个文档，100 个含该字段'), COL('method', 'string', 0, '', '', '采样 100 个文档，100 个含该字段'),
        COL('status', 'int', 0, '', '', '采样 100 个文档，100 个含该字段'), COL('costMs', 'int', 0, '', '', '采样 100 个文档，100 个含该字段'), COL('traceId', 'string', 0, 'MUL', '', '采样 100 个文档，100 个含该字段'),
        COL('ts', 'date', 0, 'MUL', '', '采样 100 个文档，100 个含该字段')],
        idx: [['_id_', ['_id'], true], ['traceId_1', ['traceId'], false], ['ts_1', ['ts'], false]]},
      {n: 'error_events', c: '应用异常事件', rows: 1820440, size: 940, eng: 'WiredTiger', cols: [
        COL('_id', 'objectId', 0, 'PRI', '', '采样 100 个文档，100 个含该字段'), COL('service', 'string', 0, 'MUL', '', '采样 100 个文档，100 个含该字段'), COL('level', 'string', 0, '', '', '采样 100 个文档，100 个含该字段'),
        COL('message', 'string', 0, '', '', '采样 100 个文档，100 个含该字段'), COL('stack', 'string|null', 1, '', '', '采样 100 个文档，58 个含该字段'), COL('ts', 'date', 0, '', '', '采样 100 个文档，100 个含该字段')],
        idx: [['_id_', ['_id'], true], ['service_1_ts_-1', ['service', 'ts'], false]]},
    ]},
  ],
  mongo_user: [
    {db: 'user_mongo', charset: '', tables: [
      {n: 'users', c: '用户档案', rows: 18230455, size: 6200, eng: 'WiredTiger', cols: [
        COL('_id', 'objectId', 0, 'PRI', '', '采样 100 个文档，100 个含该字段'), COL('uid', 'long', 0, 'UNI', '', '采样 100 个文档，100 个含该字段'), COL('nickname', 'string', 0, '', '', '采样 100 个文档，100 个含该字段'),
        COL('phone', 'string', 1, 'UNI', '', '采样 100 个文档，91 个含该字段'), COL('level', 'int', 0, '', '', '采样 100 个文档，100 个含该字段'), COL('profile', 'object', 1, '', '', '采样 100 个文档，72 个含该字段'),
        COL('tags', 'array', 1, '', '', '采样 100 个文档，66 个含该字段'), COL('regAt', 'date', 0, '', '', '采样 100 个文档，100 个含该字段')],
        idx: [['_id_', ['_id'], true], ['uid_1', ['uid'], true], ['phone_1', ['phone'], true]]},
      {n: 'user_sessions', c: '登录会话（TTL 30 天）', rows: 9120330, size: 3300, eng: 'WiredTiger', cols: [
        COL('_id', 'objectId', 0, 'PRI', '', '采样 100 个文档，100 个含该字段'), COL('uid', 'long', 0, 'MUL', '', '采样 100 个文档，100 个含该字段'), COL('ip', 'string', 0, '', '', '采样 100 个文档，100 个含该字段'),
        COL('ua', 'string', 1, '', '', '采样 100 个文档，96 个含该字段'), COL('loginAt', 'date', 0, 'MUL', '', '采样 100 个文档，100 个含该字段'), COL('expireAt', 'date', 0, '', '', '采样 100 个文档，100 个含该字段')],
        idx: [['_id_', ['_id'], true], ['uid_1', ['uid'], false], ['expireAt_1', ['expireAt'], false]]},
      {n: 'devices', c: '绑定设备', rows: 5410220, size: 1400, eng: 'WiredTiger', cols: [
        COL('_id', 'objectId', 0, 'PRI', '', '采样 100 个文档，100 个含该字段'), COL('uid', 'long', 0, 'MUL', '', '采样 100 个文档，100 个含该字段'), COL('deviceId', 'string', 0, '', '', '采样 100 个文档，100 个含该字段'),
        COL('os', 'string', 0, '', '', '采样 100 个文档，100 个含该字段'), COL('pushToken', 'string|null', 1, '', '', '采样 100 个文档，74 个含该字段'), COL('lastSeen', 'date', 0, '', '', '采样 100 个文档，100 个含该字段')],
        idx: [['_id_', ['_id'], true], ['uid_1', ['uid'], false]]},
    ]},
    {db: 'user_behavior', charset: '', tables: [
      {n: 'page_views', c: '页面浏览埋点', rows: 902330180, size: 164000, eng: 'WiredTiger', cols: [
        COL('_id', 'objectId', 0, 'PRI', '', '采样 100 个文档，100 个含该字段'), COL('uid', 'long', 1, 'MUL', '', '采样 100 个文档，87 个含该字段'), COL('page', 'string', 0, '', '', '采样 100 个文档，100 个含该字段'),
        COL('stayMs', 'int', 0, '', '', '采样 100 个文档，100 个含该字段'), COL('ts', 'date', 0, 'MUL', '', '采样 100 个文档，100 个含该字段')],
        idx: [['_id_', ['_id'], true], ['uid_1_ts_-1', ['uid', 'ts'], false]]},
      {n: 'search_history', c: '搜索词记录', rows: 61420990, size: 8800, eng: 'WiredTiger', cols: [
        COL('_id', 'objectId', 0, 'PRI', '', '采样 100 个文档，100 个含该字段'), COL('uid', 'long', 0, 'MUL', '', '采样 100 个文档，100 个含该字段'), COL('keyword', 'string', 0, '', '', '采样 100 个文档，100 个含该字段'),
        COL('hits', 'int', 0, '', '', '采样 100 个文档，100 个含该字段'), COL('ts', 'date', 0, '', '', '采样 100 个文档，100 个含该字段')],
        idx: [['_id_', ['_id'], true], ['uid_1', ['uid'], false]]},
    ]},
  ],
  ch_order: [
    {db: 'olap_order', charset: '', tables: [
      {n: 'fact_orders', c: '订单事实表', rows: 1624880310, size: 186000, eng: 'MergeTree', pk: 'order_date, user_id', sk: 'order_date, user_id, order_id', part: 'toYYYYMM(order_date)', cols: [
        COL('order_id', 'UInt64', 0, '', '', '订单 ID'), COL('user_id', 'UInt64', 0, 'PRI', '', '用户 ID'), COL('order_date', 'Date', 0, 'PRI', '', '下单日期'), COL('created_at', "DateTime('Asia/Shanghai')", 0, '', '', '下单时间'),
        COL('status', 'UInt8', 0, '', '', '状态 0待支付 1已支付 2已发货 3完成 4取消'), COL('amount', 'Decimal(12, 2)', 0, '', '', '订单金额'), COL('channel', 'LowCardinality(String)', 0, '', '', '渠道'),
        COL('city', 'LowCardinality(String)', 1, '', '', '收货城市'), COL('coupon_id', 'Nullable(UInt64)', 1, '', '', '优惠券'), COL('tags', 'Array(String)', 0, '', '[]', '标签'), COL('trace_id', 'UUID', 0, '', '', '追踪 ID')],
        idx: [['PRIMARY', ['order_date', 'user_id'], true], ['idx_status', ['status'], false]]},
      {n: 'order_events', c: '订单事件（宽表，Map 存扩展属性）', rows: 6120440120, size: 540000, eng: 'MergeTree', pk: 'event_date, order_id', sk: 'event_date, order_id, event_time', part: 'toYYYYMM(event_date)', cols: [
        COL('event_date', 'Date', 0, 'PRI', '', '事件日期'), COL('event_time', 'DateTime64(3)', 0, '', '', '事件时间（服务端时区）'), COL('order_id', 'UInt64', 0, 'PRI', '', '订单 ID'),
        COL('event_type', "Enum8('create' = 1, 'pay' = 2, 'ship' = 3, 'sign' = 4, 'cancel' = 5)", 0, '', '', '事件类型'), COL('props', 'Map(String, String)', 0, '', '{}', '扩展属性'), COL('operator', 'Nullable(String)', 1, '', '', '操作人')],
        idx: [['PRIMARY', ['event_date', 'order_id'], true]]},
      {n: 'v_order_daily', c: '每日订单汇总视图', view: true, kind: 'view', cols: [COL('order_date', 'Date', 0, '', '', '日期'), COL('cnt', 'UInt64', 0, '', '', '订单数'), COL('gmv', 'Decimal(38, 2)', 0, '', '', '成交额')]},
      {n: 'mv_order_hourly', c: '分时订单汇总（物化视图 → SummingMergeTree）', view: true, kind: 'matview', eng: 'MaterializedView', cols: [COL('hour', 'DateTime', 0, '', '', '小时'), COL('orders', 'UInt64', 0, '', '', '订单数'), COL('gmv', 'Decimal(38, 2)', 0, '', '', '成交额')]},
      {n: 'ext_orders_mysql', c: 'MySQL 引擎外部表（演示：执行前引擎核对会拒绝）', eng: 'MySQL', rows: 1200000, size: 0, cols: [COL('id', 'UInt64', 0, '', '', '订单 ID'), COL('status', 'UInt8', 0, '', '', '状态')]},
      {n: 'dict_city', c: '城市字典（Dictionary）', view: true, kind: 'dict', eng: 'Dictionary', rows: 372, cols: [COL('city_id', 'UInt32', 0, 'PRI', '', '城市编码'), COL('city_name', 'String', 0, '', '', '城市名'), COL('region', 'String', 0, '', '', '大区')]},
    ]},
    {db: 'olap_user', charset: '', tables: [
      {n: 'user_visits', c: '用户访问明细', rows: 8820330120, size: 910000, eng: 'MergeTree', pk: 'visit_date, user_id', sk: 'visit_date, user_id, visit_time', part: 'toYYYYMMDD(visit_date)', cols: [
        COL('visit_date', 'Date', 0, 'PRI', '', '访问日期'), COL('visit_time', 'DateTime', 0, '', '', '访问时间（服务端时区）'), COL('user_id', 'UInt64', 0, 'PRI', '', '用户 ID'),
        COL('page', 'LowCardinality(String)', 0, '', '', '页面'), COL('stay_ms', 'UInt32', 0, '', '0', '停留时长（毫秒）'), COL('ip', 'IPv4', 0, '', '', '来源 IP'), COL('is_new', 'Bool', 0, '', '', '是否新用户')],
        idx: [['PRIMARY', ['visit_date', 'user_id'], true]]},
      {n: 'user_funnel', c: '转化漏斗日汇总', rows: 182440, size: 14, eng: 'ReplacingMergeTree', pk: 'stat_date, step', sk: 'stat_date, step', part: 'toYYYYMM(stat_date)', cols: [
        COL('stat_date', 'Date', 0, 'PRI', '', '统计日期'), COL('step', 'String', 0, 'PRI', '', '漏斗步骤'), COL('users', 'UInt64', 0, '', '', '人数'), COL('rate', 'Float64', 0, '', '', '转化率')],
        idx: [['PRIMARY', ['stat_date', 'step'], true]]},
    ]},
  ],
  ch_dw: [
    {db: 'dw_report', charset: '', tables: [
      {n: 'events_raw', c: '原始事件（超大表，演示读取量限制）', rows: 12480330990, size: 2140000, eng: 'MergeTree', pk: 'event_date, event_id', sk: 'event_date, event_id', part: 'toYYYYMM(event_date)', cols: [
        COL('event_date', 'Date', 0, 'PRI', '', '事件日期'), COL('event_id', 'UInt64', 0, 'PRI', '', '事件 ID'), COL('user_id', 'UInt64', 0, '', '', '用户 ID'), COL('name', 'LowCardinality(String)', 0, '', '', '事件名'),
        COL('value', 'Float64', 0, '', '0', '数值'), COL('payload', 'String', 0, '', "''", '原始报文（JSON）')],
        idx: [['PRIMARY', ['event_date', 'event_id'], true]]},
      {n: 'dim_user', c: '用户维度', rows: 2310880, size: 520, eng: 'MergeTree', pk: 'user_id', sk: 'user_id', part: '', cols: [
        COL('user_id', 'UInt64', 0, 'PRI', '', '用户 ID'), COL('level', 'UInt8', 0, '', '0', '会员等级'), COL('city', 'LowCardinality(String)', 1, '', '', '城市'), COL('reg_date', 'Date', 1, '', '', '注册日期')],
        idx: [['PRIMARY', ['user_id'], true]]},
      {n: 'v_events_daily', c: '每日事件汇总视图', view: true, kind: 'view', cols: [COL('event_date', 'Date', 0, '', '', '日期'), COL('name', 'String', 0, '', '', '事件名'), COL('cnt', 'UInt64', 0, '', '', '次数')]},
      {n: 'dict_channel', c: '渠道字典（Dictionary）', view: true, kind: 'dict', eng: 'Dictionary', rows: 24, cols: [COL('channel_id', 'UInt8', 0, 'PRI', '', '渠道编码'), COL('channel_name', 'String', 0, '', '', '渠道名')]},
    ]},
  ],
});

/* ============================================================================
   SelectDB / Doris 实例（type 值与 internal/instmgr/types.go 的显示名一致：SelectDB / Doris）与分区维护演示用库表。
   这两类在后端属于“MySQL 协议系”（mysqlLike），除分区维护外的页面与 MySQL 一样可见；分区维护走专用的 SHOW PARTITIONS / ADD PARTITION VALUES [(..),(..)) 方言。
   ========================================================================== */
INSTANCES.push(
  {id: 20, name: 'doris-dw-prod', type: 'Doris', ver: '2.1.7', role: 'master', host: '10.20.4.71', port: 9030, env: '生产', rgs: [4], tags: ['生产', '数仓'], schema: 'doris_dw', slowlog: true, status: 'ok', enabled: true, qps: 420, conns: 58, ssh: false, user: 'dbtool_ro'},
  {id: 21, name: 'selectdb-log-prod', type: 'SelectDB', ver: '3.0.4', role: 'master', host: '10.20.5.71', port: 9030, env: '生产', rgs: [1, 4], tags: ['生产', '日志'], schema: 'selectdb_log', slowlog: true, status: 'ok', enabled: true, qps: 260, conns: 33, ssh: false, user: 'dbtool_ro'},
);
Object.assign(SCHEMAS, {
  doris_dw: [
    {db: 'dw_ads', charset: 'utf8mb4', tables: [
      {n: 'ads_traffic_hour', c: '渠道流量小时汇总（按小时 RANGE 分区）', rows: 612440210, size: 38200, eng: 'OLAP', part: true, cols: [
        COL('stat_hour', 'datetime', 0, 'PRI', '', '统计小时'), COL('channel', 'varchar(32)', 0, 'PRI', '', '渠道'), COL('pv', 'bigint', 0, '', '0', '浏览量'), COL('uv', 'bigint', 0, '', '0', '访客数')],
        idx: [['PRIMARY', ['stat_hour', 'channel'], true]]},
      {n: 'dwd_event_detail', c: '事件明细（bigint 毫秒时间戳，按天 RANGE 分区）', rows: 9284410055, size: 912000, eng: 'OLAP', part: true, cols: [
        COL('event_ts', 'bigint', 0, 'PRI', '', '事件时间（毫秒）'), COL('event_id', 'bigint', 0, 'PRI', '', '事件 ID'), COL('user_id', 'bigint', 0, '', '', '用户 ID'), COL('name', 'varchar(64)', 0, '', '', '事件名')],
        idx: [['PRIMARY', ['event_ts', 'event_id'], true]]},
      {n: 'ods_click_raw', c: '点击原始流水（int 秒级时间戳，按天分区；老版本建的分区 Range 文本读不出上界）', rows: 3120450771, size: 288000, eng: 'OLAP', part: true, cols: [
        COL('click_ts', 'int', 0, 'PRI', '', '点击时间（秒）'), COL('click_id', 'bigint', 0, 'PRI', '', '点击 ID'), COL('url', 'varchar(512)', 1, '', 'NULL', '落地页')],
        idx: [['PRIMARY', ['click_ts', 'click_id'], true]]},
      {n: 'dim_channel', c: '渠道维表（未分区；Doris 不自动初始化）', rows: 86, size: 1, eng: 'OLAP', cols: [
        COL('channel', 'varchar(32)', 0, 'PRI', '', '渠道编码'), COL('channel_name', 'varchar(64)', 0, '', '', '渠道名'), COL('created_at', 'datetime', 1, '', 'NULL', '创建时间')],
        idx: [['PRIMARY', ['channel'], true]]},
    ], procs: [], events: [], triggers: []},
  ],
  selectdb_log: [
    {db: 'log_center', charset: 'utf8mb4', tables: [
      {n: 'app_access_log', c: '应用访问日志（按天 RANGE 分区，datetime）', rows: 20418330219, size: 1480000, eng: 'OLAP', part: true, cols: [
        COL('log_time', 'datetime', 0, 'PRI', '', '日志时间'), COL('trace_id', 'varchar(64)', 0, 'PRI', '', '链路 ID'), COL('app', 'varchar(48)', 0, '', '', '应用'), COL('status', 'int', 0, '', '200', 'HTTP 状态码')],
        idx: [['PRIMARY', ['log_time', 'trace_id'], true]]},
      {n: 'audit_trail', c: '操作审计（按周 RANGE 分区，datetime；周分区名取窗口最后一天）', rows: 412330981, size: 40800, eng: 'OLAP', part: true, cols: [
        COL('op_time', 'datetime', 0, 'PRI', '', '操作时间'), COL('op_id', 'bigint', 0, 'PRI', '', '操作 ID'), COL('operator', 'varchar(64)', 0, '', '', '操作人'), COL('action', 'varchar(64)', 0, '', '', '动作')],
        idx: [['PRIMARY', ['op_time', 'op_id'], true]]},
    ], procs: [], events: [], triggers: []},
  ],
});

/* MySQL 未分区表的“初始化为分区表”演示库（追加到订单中心实例的库列表）。
   t.minAt = 表里最早一行数据的时间；t.minTimeout = SELECT MIN(col) 超时；t.fk = 有外键。
   覆盖：三种起点都可用 / MIN 超时 / 唯一键不含分区字段 / 有外键 / 引擎不支持分区 / 列类型与配置不一致。 */
SCHEMAS.order.push(
  {db: 'order_events', charset: 'utf8mb4', tables: [
    {n: 'order_event', c: '订单事件（未分区，datetime）', rows: 86420115, size: 21400, eng: 'InnoDB', minAt: '2026-08-20 03:11:09', cols: [
      COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('order_id', 'bigint unsigned', 0, 'MUL', '', '订单ID'), COL('event', 'varchar(32)', 0, '', '', '事件'), COL('event_time', 'datetime', 0, 'PRI', 'CURRENT_TIMESTAMP', '发生时间')],
      idx: [['PRIMARY', ['id', 'event_time'], true], ['idx_order', ['order_id'], false]]},
    {n: 'order_trace', c: '订单链路追踪（未分区，bigint 毫秒时间戳）', rows: 240118332, size: 61200, eng: 'InnoDB', minAt: '2026-09-12 00:00:07', cols: [
      COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('trace_id', 'varchar(48)', 0, 'MUL', '', '链路ID'), COL('ts_ms', 'bigint', 0, 'PRI', '', '毫秒时间戳'), COL('span', 'varchar(64)', 0, '', '', '节点')],
      idx: [['PRIMARY', ['id', 'ts_ms'], true], ['idx_trace', ['trace_id'], false]]},
    {n: 'order_audit_bin', c: '订单审计流水（未分区，int 秒级时间戳，12 亿行：MIN 扫描会超时）', rows: 1204118990, size: 388000, eng: 'InnoDB', minAt: '2025-11-03 02:20:41', minTimeout: true, cols: [
      COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('ts', 'int', 0, 'PRI', '', '秒级时间戳'), COL('actor', 'varchar(64)', 0, '', '', '操作人'), COL('detail', 'varchar(255)', 1, '', 'NULL', '详情')],
      idx: [['PRIMARY', ['id', 'ts'], true], ['idx_actor', ['actor'], false]]},
    {n: 'order_snapshot', c: '订单快照（未分区；有不含分区字段的唯一键）', rows: 5120330, size: 2100, eng: 'InnoDB', minAt: '2026-06-01 00:00:01', cols: [
      COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('order_no', 'varchar(32)', 0, 'UNI', '', '订单号'), COL('snap_at', 'datetime', 0, 'MUL', 'CURRENT_TIMESTAMP', '快照时间')],
      idx: [['PRIMARY', ['id'], true], ['uk_order_no', ['order_no'], true], ['idx_snap', ['snap_at'], false]]},
    {n: 'order_ref_log', c: '订单关联日志（未分区；有外键，MySQL 分区表不支持外键）', rows: 930221, size: 410, eng: 'InnoDB', fk: true, minAt: '2026-07-15 08:00:00', cols: [
      COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('order_id', 'bigint unsigned', 0, 'MUL', '', '订单ID（外键 → orders.id）'), COL('created_at', 'datetime', 0, 'PRI', 'CURRENT_TIMESTAMP', '创建时间')],
      idx: [['PRIMARY', ['id', 'created_at'], true], ['idx_order', ['order_id'], false]]},
    {n: 'order_memo_old', c: '订单备注（旧表，MyISAM；MySQL 8.0 只有 InnoDB / NDB 支持分区）', rows: 1220330, size: 380, eng: 'MyISAM', minAt: '2025-12-01 00:00:00', cols: [
      COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('memo', 'varchar(255)', 1, '', 'NULL', '备注'), COL('created_at', 'datetime', 0, 'PRI', 'CURRENT_TIMESTAMP', '创建时间')],
      idx: [['PRIMARY', ['id', 'created_at'], true]]},
    {n: 'order_legacy_ts', c: '订单旧时间戳表（时间字段是 varchar，配置却写成 bigint）', rows: 710440, size: 190, eng: 'InnoDB', minAt: '2025-10-01 00:00:00', cols: [
      COL('id', 'bigint unsigned', 0, 'PRI', 'auto_increment', '主键'), COL('create_ts', 'varchar(20)', 0, 'PRI', '', '创建时间（字符串毫秒）')],
      idx: [['PRIMARY', ['id', 'create_ts'], true]]},
  ], procs: [], events: [], triggers: []},
);
