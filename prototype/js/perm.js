/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: perm.js
 * @time: 2026/10/28
 */
/* ============================================================================
   权限模型（原型）—— 与 internal/btnperm/registry.go 的登记表一一对应
   - 菜单（NAV）= 侧栏入口；能否看到取决于 perm 里是否持有任一「查看类」按钮码。
   - 按钮码（PERM_TREE）= 模块 → 页面 → 按钮；kind: v 查看 / o 操作 / d 高危（二次确认）；
     第 4 项 'L' 表示锁定（服务端仅超级用户，不可授予权限组，如系统管理里的大部分按钮）。
   - 权限组（GROUPS）= 按钮码集合，也是审批流里的“级”；用户可属于多个权限组（取并集）。
   - 超级用户（user.super）不受限制；用户另有「单独授权 / 单独禁用」（extraGrant / extraDeny）。
   - 页面里每个按钮统一经 B(权限码, 文本) 渲染：无权限时置灰 + 🔒（可配置为隐藏）。
   修改本文件后运行 python tools/build_help.py 刷新手册里的权限码附录。
   ========================================================================== */
const PERM_TREE = [
  {name: 'SQL 审核', pages: [
    {key: 'sqlworkflow', name: 'SQL 上线', btns: [
      ['sqlworkflow.view_own', '查看自己的工单', 'v'], ['sqlworkflow.view_group', '查看资源组内所有工单', 'v'],
      ['sqlworkflow.submit', '提交工单', 'o'], ['sqlworkflow.submit_batch', '提交批量上线', 'o'],
      ['sqlworkflow.skip_check', '跳过语法检查', 'd', 'L'], ['sqlworkflow.cancel_own', '终止自己的工单', 'o'],
      ['sqlworkflow.approve', '审批通过', 'o'], ['sqlworkflow.reject', '驳回 / 终止他人工单', 'o'],
      ['sqlworkflow.alter_window', '修改可执行时间', 'o'], ['sqlworkflow.execute_group', '立即执行（资源组内）', 'd'],
      ['sqlworkflow.execute_own', '立即执行（仅自己的）', 'd'], ['sqlworkflow.schedule', '定时执行', 'd'],
      ['sqlworkflow.manual_finish', '确认手工执行结束', 'd'], ['sqlworkflow.rollback', '查看 / 下载回滚语句', 'o'],
      ['sqlworkflow.osc', '在线改表控制（暂停 / 恢复 / 终止）', 'd'], ['sqlworkflow.batch_op', '批量实例 重试 / 关闭', 'd']]},
    {key: 'sqlanalyze', name: 'SQL 分析', btns: [
      ['sqlanalyze.view', '查看分析报告', 'v'], ['sqlanalyze.run', '上传并开始分析', 'o']]},
  ]},
  {name: 'SQL 查询', pages: [
    {key: 'query', name: '在线查询', btns: [
      ['query.view', '打开在线查询', 'v'], ['query.run', '执行查询（含执行计划）', 'o'], ['query.objinfo', '查看对象信息 / 可编程对象（表结构、建表语句、定义）', 'o'],
      ['query.group_run', '资源组内实例按管理员行数上限查询', 'o'], ['query.admin_run', '查看 / 查询全部实例，不受库表授权限制', 'd']]},
    {key: 'slowlogs', name: '查慢日志', btns: [['slowlogs.view', '查看慢日志与明细', 'v']]},
    {key: 'partman', name: '分区维护', btns: [
      ['partman.view', '查看配置与维护日志', 'v'], ['partman.add', '新增配置', 'o'], ['partman.edit', '编辑配置', 'o'],
      ['partman.run', '立即执行维护', 'd'], ['partman.delete', '删除配置', 'd']]},
    {key: 'archive', name: '数据归档', btns: [
      ['archive.view', '查看归档任务与运行记录', 'v'], ['archive.apply', '新建 / 修改归档任务（提交申请）', 'o'], ['archive.dryrun', '试运行', 'o'], ['archive.pause', '暂停 / 恢复', 'o'],
      ['archive.run', '立即执行', 'd'], ['archive.stop', '终止运行', 'd'], ['archive.restore', '恢复数据（回灌）', 'd'], ['archive.delete', '停用 / 删除任务', 'd'],
      ['archive.cfg_view', '查看备份存储与安全策略', 'v'], ['archive.store_edit', '编辑备份存储', 'd', 'L'], ['archive.policy_edit', '编辑安全策略', 'd', 'L']]},
    {key: 'export', name: '数据导出', btns: [
      ['export.view', '查看导出工单', 'v'], ['export.view_batch', '查看批量导出工单', 'v'], ['export.submit', '提交导出', 'o'],
      ['export.submit_batch', '提交批量导出', 'o'], ['export.download', '下载导出文件', 'd']]},
    {key: 'dict', name: '数据字典', btns: [
      ['dict.view', '查看表 / 视图 / 过程等元数据', 'v'], ['dict.export', '导出 HTML 字典', 'd']]},
    {key: 'qapply', name: '权限申请', btns: [
      ['qpriv.view', '查看申请与审批进度', 'v'], ['qpriv.apply', '申请 / 撤回查询权限', 'o'], ['qpriv.review', '审批查询权限申请', 'o']]},
    {key: 'qprivs', name: '查询权限', btns: [
      ['qpriv.list', '查看已授予的查询权限', 'v'], ['qpriv.manage', '修改 / 回收他人权限', 'd']]},
  ]},
  {name: 'SQL 优化', pages: [
    {key: 'sqladvisor', name: '优化工具', btns: [
      ['optimize.explain', '执行计划 / 索引建议 / 格式化', 'v'],
      ['optimize.tuning', 'SQLTuning', 'o'], ['optimize.soar', 'SOAR', 'o']]},
    {key: 'slowquery', name: '慢查日志', btns: [['slowquery.view', '查看统计与明细', 'v'], ['slowquery.advise', '获取优化建议', 'o'],
      ['redis.slowlog_reset', '重置 Redis 慢日志（SLOWLOG RESET）', 'd', 'L']]},
  ]},
  {name: '性能工单', pages: [
    {key: 'perf', name: '工单列表 / 详情 / 排行榜', btns: [
      ['perf.view', '查看工单与排行榜', 'v'], ['perf.handle', '受理 / 申请验证 / 申请延期 / 申请豁免', 'o'], ['perf.comment', '评论', 'o'],
      ['perf.create', '手工建单', 'o'], ['perf.assign', '指派', 'o'], ['perf.verify', '验证通过 / 不通过', 'o'],
      ['perf.approve', '审批延期 / 豁免', 'o'], ['perf.adjust', '调整级别 / 切换处理方', 'o'], ['perf.reopen', '重开 / 重新诊断', 'o'],
      ['perf.close_dup', '误报关闭', 'd']]},
    {key: 'perfcfg', name: '采集 / 团队 / SLA 配置', btns: [
      ['perfcfg.view', '查看配置', 'v'], ['perfcfg.collect', '修改采集配置 / 试采集', 'o'],
      ['perfcfg.team', '修改团队配置与路由规则', 'o'], ['perfcfg.sla', '修改 SLA 策略', 'd']]},
  ]},
  {name: '实例管理', pages: [
    {key: 'instlist', name: '实例列表', btns: [
      ['instance.view', '查看实例', 'v'], ['instance.test', '测试连接', 'o'], ['instance.add', '添加实例', 'd'],
      ['instance.edit', '编辑 / 启停实例', 'd'], ['instance.delete', '删除实例', 'd'], ['instance.purge_binlog', 'Binlog 清理', 'd']]},
    {key: 'sessions', name: '会话管理', btns: [
      ['session.open', '打开会话管理', 'v'], ['session.process', '查看进程', 'v'], ['session.tablespace', '查看 Top 表空间', 'v'],
      ['session.trx', '查看事务', 'v'], ['session.locks', '查看锁信息', 'v'], ['session.kill', '终止会话', 'd']]},
    {key: 'databases', name: '数据库管理', btns: [
      ['database.view', '查看数据库', 'v'], ['database.edit', '编辑 / 录入负责人与备注', 'o'], ['database.create', '创建数据库', 'd']]},
    {key: 'accounts', name: '账号管理', btns: [
      ['account.view', '查看账号与权限', 'v'], ['account.create', '创建账号', 'd'], ['account.edit', '编辑 / 录入登记信息', 'o'],
      ['account.grant', '授权 / 回收', 'd'], ['account.pwd', '修改密码', 'd'], ['account.lock', '锁定 / 解锁', 'd'], ['account.delete', '删除账号', 'd']]},
    {key: 'params', name: '参数配置', btns: [
      ['param.open', '打开参数配置', 'v'], ['param.view', '查看参数与修改历史', 'v'], ['param.edit', '修改参数值', 'd'],
      ['param.template', '添加 / 编辑参数模板', 'o']]},
    {key: 'inspect', name: '性能巡检', btns: [
      ['inspect.view', '查看报告与定时计划', 'v'], ['inspect.run', '立即 / 重新巡检', 'o'], ['inspect.cancel', '取消进行中的巡检', 'o'],
      ['inspect.delete', '删除报告', 'd'], ['inspect.plan_edit', '新建 / 编辑定时计划', 'o'],
      ['inspect.plan_toggle', '暂停 / 启用 / 删除定时计划', 'd'], ['inspect.plan_run', '立即运行定时计划', 'd']]},
    {key: 'binlog', name: 'Binlog 解析', btns: [
      ['binlog.view', '打开 Binlog 解析、查看 binlog 文件列表', 'v'], ['binlog.parse', '解析 binlog 还原 SQL', 'o'],
      ['binlog.rollback', '生成回滚 SQL', 'd'], ['binlog.download', '下载解析 / 回滚结果文件', 'd']]},
  ]},
  {name: '系统管理', pages: [
    {key: 'flow', name: '审批流程配置', btns: [['flow.view', '查看审批流', 'v', 'L'], ['flow.edit', '变更审批流', 'd', 'L']]},
    {key: 'rg', name: '资源组管理', btns: [
      ['rg.view', '查看资源组', 'v', 'L'], ['rg.edit', '新建 / 编辑资源组与通知群', 'o', 'L'],
      ['rg.relations', '调整成员与实例', 'o', 'L'], ['rg.delete', '删除 / 恢复资源组', 'd', 'L']]},
    {key: 'user', name: '用户管理', btns: [
      ['user.view', '查看用户', 'v', 'L'], ['user.edit', '新建 / 编辑用户', 'o', 'L'], ['user.unlock', '解锁账号', 'o', 'L'],
      ['user.reset', '重置密码 / 重置 2FA', 'd', 'L'], ['user.disable', '停用 / 启用', 'd', 'L'], ['user.grant_super', '授予超级用户', 'd', 'L']]},
    {key: 'ag', name: '权限组管理', btns: [
      ['ag.view', '查看权限组', 'v', 'L'], ['ag.edit', '新建 / 编辑权限与成员', 'd', 'L'], ['ag.delete', '删除权限组', 'd', 'L'],
      ['ag.btnperm', '按钮权限与生效模式', 'd', 'L']]},
    {key: 'sec', name: '登录与安全设置', btns: [['sec.view', '查看', 'v', 'L'], ['sec.edit', '修改', 'd', 'L']]},
    {key: 'goinception', name: 'goInception 配置', btns: [
      ['goinception.view', '查看连接信息与配置', 'v', 'L'], ['goinception.conn', '修改连接信息 / 备份库', 'd', 'L'],
      ['goinception.file', '修改配置文件（仅保存）', 'd', 'L'], ['goinception.restart', '保存并重启 / 仅重启', 'd', 'L'],
      ['goinception.rollback', '回滚历史版本', 'd', 'L']]},
    {key: 'authp', name: '登录方式', btns: [
      ['authp.view', '查看登录方式', 'v', 'L'], ['authp.test', '测试连接 / 测试登录', 'o', 'L'],
      ['authp.edit', '编辑配置 / 入组规则', 'd', 'L'], ['authp.toggle', '启用 / 停用 / 排序 / 删除', 'd', 'L']]},
    {key: 'notify', name: '通知渠道', btns: [
      ['notify.view', '查看渠道', 'v', 'L'], ['notify.edit', '新建 / 编辑渠道与 @ 规则', 'o', 'L'], ['notify.test', '测试发送', 'o', 'L']]},
    {key: 'redisrule', name: 'Redis 上线规则', btns: [
      ['redisrule.view', '查看 Redis 上线规则', 'v', 'L'], ['redisrule.edit', '新增 / 修改 / 启停 / 删除规则', 'd', 'L']]},
    {key: 'audit', name: '系统审计', btns: [['audit.view', '查看审计日志', 'v'], ['audit.export', '导出审计日志', 'd']]},
  ]},
];

/* 扁平索引：按钮码 → 元信息；页面键 → 页面 */
const BTNS = {}, PAGES = {};
PERM_TREE.forEach(m => m.pages.forEach(p => {
  PAGES[p.key] = {...p, module: m.name};
  p.btns.forEach(([code, label, kind, lock]) => { BTNS[code] = {code, label, kind, locked: lock === 'L', page: p.key, pageName: p.name, module: m.name}; });
}));
const ALL_CODES = () => Object.keys(BTNS);
const GRANTABLE = () => Object.keys(BTNS).filter(c => !BTNS[c].locked);
const codesOf = (...pageKeys) => pageKeys.flatMap(k => PAGES[k].btns.filter(b => b[3] !== 'L').map(b => b[0]));

/* ---------- 侧栏菜单：与 internal/server/menu.go 及各模块 AddMenu 一致 ---------- */
/* perm：持有其中任一按钮码即显示；public：所有登录用户可见；help：该页对应的手册章节 */
const NAV_TREE = [
  {group: '工作台', items: [
    {id: 'home', ico: '⌂', name: '首页', public: true, help: 'quickstart'}]},
  {group: 'SQL 审核', items: [
    {id: 'sqlworkflow', ico: '⎘', name: 'SQL 上线', perm: ['sqlworkflow.view_own', 'sqlworkflow.view_group', 'sqlworkflow.submit'], help: 'sqlworkflow'},
    {id: 'sqlanalyze', ico: '⌕', name: 'SQL 分析', perm: ['sqlanalyze.view'], help: 'optimize'}]},
  {group: 'SQL 查询', items: [
    {id: 'query', ico: '⌘', name: '在线查询', perm: ['query.view'], help: 'query'},
    {id: 'slowlogs', ico: '◷', name: '查慢日志', perm: ['slowlogs.view'], help: 'slowlog'},
    {id: 'partman', ico: '◫', name: '分区维护', perm: ['partman.view'], help: 'partman'},
    {id: 'archive', ico: '▥', name: '数据归档', perm: ['archive.view'], help: 'archive'},
    {id: 'export', ico: '⇩', name: '数据导出', perm: ['export.view', 'export.view_batch', 'export.submit'], help: 'export'},
    {id: 'dict', ico: '☷', name: '数据字典', perm: ['dict.view'], help: 'dict'},
    {id: 'qapply', ico: '✎', name: '权限管理', perm: ['qpriv.view'], help: 'qpriv'},
    {id: 'qprivs', ico: '⚿', name: '查询权限', perm: ['qpriv.list'], help: 'qpriv'}]},
  {group: 'SQL 优化', items: [
    {id: 'sqladvisor', ico: '⚡', name: '优化工具', perm: ['optimize.explain'], help: 'optimize'},
    {id: 'slowquery', ico: '☰', name: '慢查日志', perm: ['slowquery.view'], help: 'slowlog'}]},
  {group: '性能工单', items: [
    {id: 'perf', ico: '⚑', name: '工单列表', perm: ['perf.view', 'perf.create', 'perfcfg.view'], help: 'perf'},
    {id: 'perfboard', ico: '♛', name: '团队排行榜', perm: ['perf.view', 'perfcfg.view'], help: 'perf'},
    {id: 'perfcollect', ico: '⇲', name: '采集配置', perm: ['perfcfg.view'], help: 'perf'},
    {id: 'perfteam', ico: '☺', name: '团队配置', perm: ['perfcfg.view'], help: 'perf'},
    {id: 'perfsla', ico: '⏱', name: 'SLA 策略', perm: ['perfcfg.view'], help: 'perf'}]},
  {group: '实例管理', items: [
    {id: 'instlist', ico: '▤', name: '实例列表', perm: ['instance.view'], help: 'instance'},
    {id: 'sessions', ico: '⇄', name: '会话管理', perm: ['session.open'], help: 'instance'},
    {id: 'databases', ico: '⛁', name: '数据库管理', perm: ['database.view'], help: 'instance'},
    {id: 'accounts', ico: '♙', name: '账号管理', perm: ['account.view'], help: 'instance'},
    {id: 'params', ico: '⚙', name: '参数配置', perm: ['param.open'], help: 'instance'},
    {id: 'binlog', ico: '▦', name: 'Binlog 解析', perm: ['binlog.view'], help: 'instance'},
    {id: 'inspect', ico: '✚', name: '性能巡检', perm: ['inspect.view'], help: 'inspect'}]},
  {group: '系统管理', items: [
    {id: 'flow', ico: '⇶', name: '审批流程配置', perm: ['flow.view'], help: 'sysadmin'},
    {id: 'rg', ico: '⧉', name: '资源组管理', perm: ['rg.view'], help: 'sysadmin'},
    {id: 'user', ico: '♟', name: '用户管理', perm: ['user.view'], help: 'sysadmin'},
    {id: 'ag', ico: '⛨', name: '权限组管理', perm: ['ag.view'], help: 'btnperm'},
    {id: 'authp', ico: '⚷', name: '登录方式', perm: ['authp.view'], help: 'sysadmin'},
    {id: 'sec', ico: '⛊', name: '登录与安全设置', perm: ['sec.view'], help: 'sysadmin'},
    {id: 'audit', ico: '☑', name: '系统审计', perm: ['audit.view'], help: 'sysadmin'},
    {id: 'goinception', ico: '⌬', name: 'goInception 配置', perm: ['goinception.view'], help: 'sysadmin'},
    {id: 'redisrule', ico: '⚖', name: 'Redis 上线规则', perm: ['redisrule.view'], help: 'sysadmin'},
    {id: 'notify', ico: '✉', name: '通知渠道', perm: ['notify.view'], help: 'sysadmin'}]},
  {group: '帮助', items: [
    {id: 'help', ico: '?', name: '使用手册', public: true}]},
];
const NAV = {};
NAV_TREE.forEach(g => g.items.forEach(m => { m.group = g.group; NAV[m.id] = m; }));   // 同一对象：页面设置 NAV[id].badge 后侧栏即可读到
/* 页面可设置 NAV[id].badge = () => 数字 来显示侧栏角标（badgeWarn = true 时为橙色） */

/* ---------- 权限组（角色）：与「预设 DBA / TL / RD / QA / Default」对应 ---------- */
const GROUPS = [
  {id: 'g_dba', name: 'DBA', builtin: true, desc: '数据库管理员：审批 / 执行 / 实例管理 / 性能工单处理，查看全部实例。',
    perms: GRANTABLE().filter(c => !['sqlworkflow.execute_own', 'binlog.rollback', 'binlog.download'].includes(c))},   // Binlog 回滚 / 下载默认只给超级用户，不进任何预设组
  {id: 'g_tl', name: '组长 TL', builtin: true, desc: '资源组负责人：审批本组工单与查询权限，不能执行上线。',
    perms: [].concat(codesOf('sqlanalyze', 'sqladvisor', 'slowquery', 'slowlogs', 'dict', 'qprivs').filter(c => !['qpriv.manage', 'dict.export', 'optimize.soar', 'optimize.tuning'].includes(c)),
      ['sqlworkflow.view_own', 'sqlworkflow.view_group', 'sqlworkflow.submit', 'sqlworkflow.cancel_own', 'sqlworkflow.approve', 'sqlworkflow.reject', 'sqlworkflow.alter_window', 'sqlworkflow.rollback',
       'query.view', 'query.run', 'query.objinfo', 'query.group_run', 'qpriv.view', 'qpriv.apply', 'qpriv.review', 'export.view', 'export.submit',
       'perf.view', 'perf.handle', 'perf.comment', 'instance.view', 'partman.view',
       'archive.view', 'archive.apply', 'archive.dryrun', 'archive.pause'])},
  {id: 'g_rd', name: '研发 RD', builtin: true, desc: '研发：提交工单、在线查询、申请查询权限、处理自己负责的性能工单。',
    perms: ['sqlworkflow.view_own', 'sqlworkflow.submit', 'sqlworkflow.cancel_own', 'sqlworkflow.execute_own', 'sqlworkflow.rollback',
      'query.view', 'query.run', 'query.objinfo', 'qpriv.view', 'qpriv.apply', 'qpriv.list', 'export.view', 'export.submit',
      'dict.view', 'slowlogs.view', 'partman.view', 'archive.view', 'archive.apply', 'archive.dryrun', 'sqlanalyze.view', 'sqlanalyze.run', 'optimize.explain', 'slowquery.view', 'slowquery.advise',
      'perf.view', 'perf.handle', 'perf.comment']},
  {id: 'g_qa', name: '测试 QA', builtin: true, desc: '测试：提交工单、在线查询、导出；不能自行执行上线。',
    perms: ['sqlworkflow.view_own', 'sqlworkflow.submit', 'sqlworkflow.cancel_own', 'sqlworkflow.rollback',
      'query.view', 'query.run', 'query.objinfo', 'qpriv.view', 'qpriv.apply', 'qpriv.list', 'export.view', 'export.submit', 'export.download', 'dict.view', 'slowlogs.view', 'optimize.explain', 'archive.view']},
  {id: 'g_audit', name: '审计', builtin: false, desc: '合规审计：只读审计日志与全部工单。',
    perms: ['audit.view', 'audit.export', 'sqlworkflow.view_own', 'sqlworkflow.view_group', 'qpriv.view', 'qpriv.list', 'export.view', 'archive.view']},
  {id: 'g_default', name: 'Default', builtin: true, desc: '新用户默认权限组：仅能在线查询（需另外申请库表权限）。',
    perms: ['query.view', 'query.run', 'query.objinfo', 'qpriv.view', 'qpriv.apply', 'qpriv.list', 'dict.view']},
];
const groupName = id => (GROUPS.find(g => g.id === id) || {name: id}).name;

/* ---------- 平台用户（演示账号；密码均为 DEMO_PWD） ----------
   rgs：所属资源组（决定能看到的实例）；extraGrant / extraDeny：用户级单独授权 / 禁用 */
const USERS = [
  {id: 1, login: 'admin', name: '系统管理员', email: 'admin@example.com', dept: '基础架构部', super: true, groups: [], rgs: [1, 2, 3, 4], status: 'active', auth: '本地', twofa: true, last: '2026-10-07 09:12', ip: '10.1.1.2', extraGrant: [], extraDeny: []},
  {id: 2, login: 'dba_li', name: '李 DBA', email: 'li@example.com', dept: '数据平台部', super: false, groups: ['g_dba'], rgs: [1, 2, 3, 4], status: 'active', auth: 'LDAP', twofa: true, last: '2026-10-07 08:47', ip: '10.1.2.15', extraGrant: [], extraDeny: []},
  {id: 3, login: 'tl_wang', name: '王组长', email: 'wang@example.com', dept: '订单中心', super: false, groups: ['g_tl'], rgs: [1, 2], status: 'active', auth: 'LDAP', twofa: false, last: '2026-10-07 10:31', ip: '10.1.3.21', extraGrant: [], extraDeny: []},
  {id: 4, login: 'rd_zhao', name: '赵开发', email: 'zhao@example.com', dept: '订单中心', super: false, groups: ['g_rd'], rgs: [1], status: 'active', auth: 'LDAP', twofa: false, last: '2026-10-07 10:02', ip: '10.1.3.36', extraGrant: [], extraDeny: []},
  {id: 5, login: 'rd_zhou', name: '周开发', email: 'zhou@example.com', dept: '支付中心', super: false, groups: ['g_rd'], rgs: [2], status: 'active', auth: '飞书', twofa: false, last: '2026-10-06 18:20', ip: '10.1.4.8', extraGrant: ['export.download'], extraDeny: []},
  {id: 6, login: 'qa_sun', name: '孙测试', email: '', dept: '质量保障部', super: false, groups: ['g_qa'], rgs: [1, 3], status: 'active', auth: 'LDAP', twofa: false, last: '2026-10-06 16:55', ip: '10.1.5.12', extraGrant: [], extraDeny: []},
  {id: 7, login: 'auditor_chen', name: '陈审计', email: 'chen@example.com', dept: '风控合规部', super: false, groups: ['g_audit'], rgs: [1, 2, 3, 4], status: 'active', auth: '本地', twofa: false, last: '2026-09-30 15:20', ip: '10.1.6.3', extraGrant: [], extraDeny: []},
  {id: 8, login: 'dev_liu', name: '刘开发', email: 'liu@example.com', dept: '用户中心', super: false, groups: ['g_rd'], rgs: [3], status: 'locked', auth: '本地', twofa: false, last: '2026-08-12 11:02', ip: '10.1.7.9', extraGrant: [], extraDeny: []},
];
const DEMO_PWD = 'Dba@2026';    // 原型：所有演示账号的密码
const DEMO_OTP = '123456';      // 原型：两步验证码

/* ---------- 登录方式（SSO / 统一认证）：与「系统管理 › 登录方式」同一份数据 ---------- */
const SSO_PROVIDERS = [
  {id: 'ldap', name: 'LDAP', type: 'LDAP', color: '#4b5bdc', ico: 'L', proto: 'LDAP 绑定认证（ldap://ldap.example.com:389）', on: true, login: 'tab', demoUser: 'dba_li'},
  {id: 'feishu', name: '飞书', type: '飞书', color: '#3370ff', ico: '飞', proto: 'OAuth 2.0（飞书开放平台网页应用）', on: true, login: 'btn', demoUser: 'rd_zhou'},
  {id: 'dingtalk', name: '钉钉', type: '钉钉', color: '#1677ff', ico: '钉', proto: 'OAuth 2.0（钉钉扫码登录）', on: true, login: 'btn', demoUser: 'tl_wang'},
  {id: 'wecom', name: '企业微信', type: '企业微信', color: '#2b9a5b', ico: '企', proto: 'OAuth 2.0（企业微信网页授权）', on: true, login: 'btn', demoUser: 'rd_zhao'},
  {id: 'oidc', name: '公司 SSO', type: 'OIDC', color: '#e9a23b', ico: 'O', proto: 'OpenID Connect（https://sso.example.com）', on: true, login: 'btn', demoUser: 'qa_sun'},
  {id: 'cas', name: 'CAS', type: 'CAS', color: '#8892a6', ico: 'C', proto: 'CAS 3.0（https://cas.example.com/cas）', on: false, login: 'btn', demoUser: 'rd_zhao'},
  {id: 'lark', name: 'Lark', type: 'Lark', color: '#00b0a8', ico: 'La', proto: 'OAuth 2.0（Lark Developer）', on: false, login: 'btn', demoUser: 'rd_zhou'},
  {id: 'slack', name: 'Slack', type: 'Slack', color: '#611f69', ico: 'S', proto: 'Sign in with Slack（OpenID Connect）', on: false, login: 'btn', demoUser: 'rd_zhou'},
];

/* ---------- 平台设置（部分影响全局表现；系统管理页面会读写） ---------- */
const SETTINGS = {
  noPermBtn: 'disable',        // disable 置灰带锁 / hide 隐藏
  btnMode: 'compat',           // 按钮权限生效模式：compat 兼容 / strict 严格
  watermark: true,             // 页面水印开关。开关、内容与样式只有超级用户能在「登录与安全设置」里改（sec.edit 是锁定码）
  wmCfg: {text: '{姓名}（{账号}）{时间}', extra: '内部资料 严禁外传', opacity: 14, size: 14, angle: -22, gap: 'normal', color: 'auto'},
  /* 登录与安全设置（取值范围 / 单位 / 默认值以真实实现为准：internal/authx/settings.go 的 DefaultSettings 与 validateSecurity） */
  loginFailLock: 5, lockSeconds: 300,        // 登录失败锁定次数 1–100；锁定时间 10–86400 秒
  force2fa: false, selfRegister: false,
  pwdMinLen: 9, pwdComplexity: false,        // 密码最小长度 6–64；复杂度（字母 + 数字 + 特殊字符）默认关
  sessionMinutes: 300, sessionBrowserClose: true,   // 会话有效期 5–43200 分钟（滑动续期）；默认关闭浏览器即退出
  jwtHours: 4, jwtRequireOtp: false,         // JWT access 有效期 1–720 小时；JWT 登录默认不要求 OTP
  apiWhitelist: [],                          // API 用户白名单（用户 ID）；为空 = 不限制
  defGroups: [], defRgs: [], smsProvider: 'disabled',   // 新用户默认权限组 / 资源组默认为空
};

/* ---------- 当前登录用户 / 权限计算 ---------- */
let ME = USERS[0];
function myCodes() {
  const s = new Set();
  ME.groups.forEach(gid => { const g = GROUPS.find(x => x.id === gid); if (g) g.perms.forEach(p => s.add(p)); });
  ME.extraGrant.forEach(c => s.add(c));
  ME.extraDeny.forEach(c => s.delete(c));
  return s;
}
function can(code) {
  if (!BTNS[code]) { console.warn('未登记的权限码', code); return false; }
  if (ME.super) return true;
  if (BTNS[code].locked) return false;
  return myCodes().has(code);
}
const canAny = codes => codes.some(can);
const canView = id => !NAV[id] ? false : (NAV[id].public || canAny(NAV[id].perm));

/* 我能看到的实例：超级用户 / 持有 query.admin_run 看全部，否则按资源组关联。
   Redis 实例默认不包含（SQL 类页面都按 MySQL / SQL Server 处理）；实例列表 / 性能巡检 / 慢查日志等 Redis 相关页面传 true。
   PgSQL / Mongo / ClickHouse（EXT_TYPES，第一期只支持在线查询和数据字典）同样默认不包含，也不被 true 带出：只有传 'ext'（带这三类，不带 Redis）或 'all'（全带）的页面才看得到 */
function visibleInstances(withRedis) {
  const all = ME.super || can('query.admin_run');
  const redisOk = withRedis === true || withRedis === 'all', extOk = withRedis === 'ext' || withRedis === 'all';
  return INSTANCES.filter(i => i.enabled !== false && (i.type === 'Redis' ? redisOk : EXT_TYPES.includes(i.type) ? extOk : true) && (all || i.rgs.some(r => ME.rgs.includes(r))));
}

/* 统一的按钮渲染：B(权限码, 文本, {cls, data:{...}, title}) —— 点击由 core.js 分发到 ACTIONS[权限码] */
function B(code, label, o = {}) {
  const meta = BTNS[code];
  const data = Object.entries(o.data || {}).map(([k, v]) => ` data-${k}="${esc(v)}"`).join('');
  const cls = 'btn ' + (o.cls || '');
  if (!can(code)) {
    if (SETTINGS.noPermBtn === 'hide') return '';
    return `<button class="${cls} noperm" disabled title="${meta && meta.locked ? '仅超级用户可用' : '无权限：需要「' + (meta ? meta.label : code) + '」'}（${code}）">${label}</button>`;
  }
  return `<button class="${cls}" data-act="${code}"${data} title="${esc(o.title || (meta ? meta.label + (meta.kind === 'd' ? '（高危）' : '') : ''))}">${label}</button>`;
}
