/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: multi_engine_fake.mjs
 * @time: 2026/10/28
 */
// 多引擎（PostgreSQL / MongoDB / ClickHouse）前端页面测试用的假后端 + 页面外壳。
// 接口形状严格按 docs/multi-engine-query.md；页面内容取自真实模板（去掉 Go 模板指令），脚本取自 web/static。
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { untranslate } from './tpl.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '../..');
const staticRoot = path.join(root, 'web/static');
const tplRoot = path.join(root, 'web/templates');
// 与 internal/server/server.go 的真实 CSP 一致
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'";
const MIME = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8', '.svg': 'image/svg+xml', '.md': 'text/plain; charset=utf-8', '.json': 'application/json' };

// ---- 实例与元数据 ----
export const INSTANCES = [
  { ID: 1, Name: 'mysql-prod', DBType: 'mysql', Env: 'prod', Role: 'master' },
  { ID: 2, Name: 'mssql-a', DBType: 'mssql', Env: 'test', Role: 'master' },
  { ID: 3, Name: 'redis-a', DBType: 'redis', Env: 'test', Role: 'master' },
  { ID: 4, Name: 'pg-main', DBType: 'pgsql', Env: 'prod', Role: 'master' },
  { ID: 5, Name: 'mongo-main', DBType: 'mongo', Env: 'test', Role: 'master' },
  { ID: 6, Name: 'ch-main', DBType: 'clickhouse', Env: 'test', Role: 'slave' },
];
const byId = (id) => INSTANCES.find((i) => String(i.ID) === String(id));
const DBS = { 1: ['shop_my'], 2: ['master'], 4: ['shop', 'Reports'], 5: ['appdb'], 6: ['logs', 'default'] };
const ok = (data, extra = {}) => ({ data, ...extra });

const col = (name, type, extra = {}) => ({ name, type, key: '', nullable: true, comment: '', sensitive: false, ...extra });
const SCHEMAS = {
  4: {
    'public.orders': [col('id', 'integer', { key: 'PRI', nullable: false, comment: '主键' }), col('user_id', 'integer'), col('update', 'text'), col('Status', 'character varying(16)'), col('created_at', 'timestamp with time zone')],
    'audit.events': [col('id', 'bigint', { key: 'PRI' }), col('payload', 'jsonb')],
  },
  5: {
    orders: [col('_id', 'objectId', { key: 'PRI', nullable: false }), col('status', 'string|null', { comment: '采样 100 个文档，98 个含该字段' }), col('items', 'array')],
    'order-items': [col('_id', 'objectId', { key: 'PRI' }), col('sku', 'string')],
  },
  6: {
    events: [col('dt', 'Date', { key: 'PRI', nullable: false }), col('user_id', 'UInt64'), col('format', 'String'), col('msg', 'Nullable(String)')],
  },
  1: { orders: [col('id', 'bigint', { key: 'PRI' }), col('phone', 'varchar(20)', { sensitive: true })] },
};
const tm = (name, extra = {}) => ({ name, view: false, rows: 100, comment: '', locked: false, sensitive: [], ...extra });
function metaFor(id, db) {
  const base = { db, sensitive_total: 0, locked_total: 0 };
  const pol = (tables, tableLevel) => ({ mode: 'priv', source: 'x', max_rows: 1000, min_rows: 1000, all_tables: false, tables, expire: '2026-12-31', expire_days: 83, table_level: tableLevel });
  switch (Number(id)) {
    case 4:
      return { ...base, unmasked: false, locked_total: 1, policy: pol(['public.orders'], true), tables: [
        tm('public.orders', { schema: 'public', kind: 'table', rows: 12034, comment: '订单' }),
        tm('public.v_orders', { schema: 'public', kind: 'view', view: true, rows: -1, locked: true }),
        tm('public.mv_stats', { schema: 'public', kind: 'matview', view: true, rows: -1 }),
        tm('audit.events', { schema: 'audit', kind: 'partitioned', rows: 900000 }),
        tm('audit.ext_log', { schema: 'audit', kind: 'foreign', rows: -1 }),
      ] };
    case 5:
      return { ...base, unmasked: false, policy: pol([], true), tables: [
        tm('orders', { kind: 'collection', rows: 5200 }), tm('order-items', { kind: 'collection', rows: 18000 }),
        tm('v_active', { kind: 'collection', view: true, rows: -1, comment: '视图，源集合 orders' }), tm('metrics', { kind: 'timeseries', rows: 0 }),
      ] };
    case 6:
      return { ...base, unmasked: false, policy: pol(['events'], true), tables: [
        tm('dict_country', { kind: 'dictionary', rows: -1 }), tm('events', { kind: 'table', rows: 8000000, comment: '埋点事件' }),
        tm('events_mv', { kind: 'matview', view: true, rows: -1 }), tm('v_recent', { kind: 'view', view: true, rows: -1 }),
      ] };
    default:
      return { ...base, sensitive_total: 1, policy: { mode: 'priv', source: 'x', max_rows: 500, min_rows: 500, all_tables: true, tables: [] }, tables: [tm('orders', { rows: 1000, sensitive: ['phone'] }), tm('v_orders', { view: true, rows: -1 })] };
  }
}

// ---- 结果表格交互（复制 / 预览）用的数据集：多行 SQL、NULL、引号与反斜杠、数字、超长文本 ----
export const GRID_SCRIPT = [
  "SET block_encryption_mode = 'aes-256-cbc';",
  'SET @SiteId := 1008;',
  '-- 币种校验：不一致直接中断',
  'SELECT s.PayCode, @SiteId AS SiteId',
  '  FROM shopbasic.shop_info s',
  ' WHERE s.Id = @SiteId;',
].join('\n');
export const GRID_LONG = 'y'.repeat(520000);
export const GRID_NOTE = "it's a\\b"; // 单引号 + 一个反斜杠
const GRID = {
  columns: ['id', 'script', 'note', 'amount', 'flag'],
  types: ['int', 'text', 'varchar(64)', 'decimal(10,2)', 'tinyint'],
  rows: () => [
    [1, GRID_SCRIPT, 'plain', 115.5, true],
    [2, null, GRID_NOTE, 0, false],
    [3, GRID_LONG, 'plain', 7, true],
    [4, '{"k": 1}', GRID_NOTE, 115.5, true],
  ],
};

// ---- 对象信息 / 可编程对象 ----
export const OBJ_DDL = [
  'CREATE TABLE `orders` (',
  "  `id` bigint NOT NULL AUTO_INCREMENT COMMENT '主键',",
  '  `status` varchar(16) NOT NULL,',
  '  PRIMARY KEY (`id`)',
  ') ENGINE=InnoDB DEFAULT CHARSET=utf8mb4',
].join('\n');
export const MS_DDL = [
  'CREATE TABLE [dbo].[orders]',
  '(',
  '    [Id] int IDENTITY(1,1) NOT NULL,',
  '    [Name] nvarchar(50) NULL',
  ')',
  '',
  '-- INDEXES',
  'ALTER TABLE [dbo].[orders] ADD CONSTRAINT [PK_orders] PRIMARY KEY ([Id]);',
  'CREATE NONCLUSTERED INDEX [ix_name] ON [dbo].[orders] ([Name]);',
].join('\n');
export const FN_DEF =['CREATE FUNCTION `fn_a`(x INT) RETURNS int', 'BEGIN', '  RETURN x + 1;', 'END'].join('\n');
function programmables(inst) {
  const it = (name, comment = '', schema) => ({ ...(schema ? { schema } : {}), name, comment });
  if (inst.DBType === 'mssql') {
    return [
      { kind: 'view', label: '视图', items: [it('v_users', '', 'dbo')] },
      { kind: 'function', label: '函数', items: [it('GetMonthOfDays', '', 'dbo'), it('split', '', 'dbo')] },
      { kind: 'procedure', label: '存储过程', items: [it('usp_clean', '', 'dbo')] },
      { kind: 'trigger', label: '触发器', items: [] },
    ];
  }
  if (inst.DBType !== 'mysql') return [];
  return [
    { kind: 'view', label: '视图', items: [it('v_orders')] },
    { kind: 'function', label: '函数', items: [it('fn_a', '加一'), it('fn_b')] },
    { kind: 'procedure', label: '存储过程', items: [it('sp_x')] },
    { kind: 'trigger', label: '触发器', items: [], error: 'denied' },
    { kind: 'event', label: '定时任务', items: [] },
  ];
}
function objectInfo(inst, kind, name, schema) {
  if (kind === 'function' || kind === 'procedure') {
    return { kind, name, ...(schema ? { schema } : {}), sections: [
      { title: '基本信息', type: 'kv', pairs: [{ key: '名称', value: name }, { key: '所属库', value: 'shop_my' }, { key: '定义者', value: 'root@%' }] },
      { title: '定义', type: 'code', text: FN_DEF },
    ] };
  }
  if (inst.DBType === 'mssql') { // 与 internal/dict 的 SQL Server 详情同形：表信息 / 列信息 / 索引信息 / 建表语句（IDENTITY、-- INDEXES）
    return { kind, name, schema: schema || 'dbo', sections: [
      { title: '表信息', type: 'kv', pairs: [{ key: '架构', value: schema || 'dbo' }, { key: '表名', value: name }, { key: '主键', value: 'Id（自增）' }, { key: '表行数（估算）', value: '1376' }, { key: '当前自增值', value: '1377' }] },
      { title: '列信息', type: 'table', columns: ['列名', '类型', '可空', '默认值', '键', '额外', '注释'], rows: [['Id', 'int', '否', 'NULL', 'PRI', 'identity（自增）', '主键'], ['Name', 'nvarchar(50)', '是', 'NULL', '', '', '']] },
      { title: '索引信息', type: 'table', columns: ['索引名', '列', '唯一', '类型'], rows: [['PK_orders', 'Id', '是', 'CLUSTERED · 主键'], ['ix_name', 'Name', '否', 'NONCLUSTERED']] },
      { title: '建表语句（含索引、触发器）', type: 'code', text: MS_DDL, note: '由系统目录还原，仅供查看参考，不保证可直接执行。' },
    ] };
  }
  return { kind, name, ...(schema ? { schema } : {}), sections: [
    { title: '表信息', type: 'kv', pairs: [{ key: '表名', value: name }, { key: '引擎', value: 'InnoDB' }, { key: '表行数（估算）', value: '1376' }, { key: '数据大小(KB)', value: '352.00' }, { key: '表说明', value: '订单' },
      ...(String(name).endsWith('_wide') ? ['行格式', '平均行长度', '索引大小(KB)', '碎片大小(KB)', '当前自增值', '字符集排序规则', '创建时间', '更新时间', '分区表'].map((k, i) => ({ key: k, value: String(i + 1) })) : [])] },
    { title: '列信息', type: 'table', columns: ['列名', '类型', '可空', '默认值', '键', '额外', '注释', '脱敏'], rows: [['id', 'bigint', '否', 'NULL', 'PRI', 'auto_increment', '主键', ''], ['status', 'varchar(16)', '否', 'NULL', '', '', '订单状态，配置了数据脱敏的字段显示 Y', 'Y']] },
    { title: '索引信息', type: 'table', columns: ['索引名', '列', '唯一', '类型'], rows: [['PRIMARY', 'id', '是', 'BTREE']] },
    { title: '建表语句', type: 'code', text: OBJ_DDL },
  ], ...(inst.DBType === 'pgsql' ? { notes: ['建表语句是根据系统目录拼出的近似 DDL。'] } : {}) };
}

// ---- 执行 ----
export const calls = []; // 记录 { method, url, body }
// “我的 SQL”面板用的内存数据：history 每项 { id, time, username, display, instance, db, sql, rows, cost_sec, status, reason }
export const fake = { admin: false, events: false, history: [], fav: [], favSeq: 0, favError: null };
function execute(req) {
  const inst = byId(req.instance_id);
  const sql = String(req.sql || '');
  const fail = (status, code, message, data) => ({ status, body: { code, message, ...(data ? { data } : {}) } });
  if (!inst) return fail(400, 'bad_request', '请选择实例并输入 SQL');
  if (/forbidden/i.test(sql)) return fail(400, 'bad_query', inst.DBType === 'mongo' ? '不允许的方法：insertOne（测试拒绝）' : '只允许 SELECT 等只读语句（测试拒绝）');
  if (/\bsecret\b/i.test(sql)) return fail(403, 'no_privilege', `你无 ${req.db}.public.secret 表的查询权限，请先申请。`, { database: req.db, table: inst.DBType === 'pgsql' ? 'public.secret' : 'secret' });
  // SQL 防火墙“要求填写原因”：没带 reason 先返回 428，前端弹窗收集后带 reason 重发
  if (/\bneedsreason\b/i.test(sql) && !String(req.reason || '').trim()) return fail(428, 'reason_required', '该表含敏感数据，请说明查询用途。', { messages: ['该表含敏感数据，请说明查询用途。'] });
  if (/\bbusyq\b/i.test(sql)) return fail(429, 'busy', '你同时执行的 PostgreSQL / MongoDB / ClickHouse 查询已达 3 条，请等待其中一条完成后再试');
  if (/\bboom\b/i.test(sql)) return fail(400, 'exec_error', "Table 'shop.boom' doesn't exist"); // 引擎报错（运行全部的“失败”状态）
  if (/\btimeoutq\b/i.test(sql)) return fail(408, 'timeout', '查询超过 60 秒已终止，请缩小范围或增加过滤条件。');
  const big = /\bbig\b/i.test(sql);
  const common ={ effective_limit: 1000, limit_source: 'shop 库权限', cost_ms: 12, masked_columns: [], query_log_id: 9 };
  switch (inst.DBType) {
    case 'pgsql':
      return { status: 200, body: ok({ ...common, columns: ['id', 'name'], column_types: ['int4', 'text'], rows: [[1, 'alice'], [2, null]], row_count: 2, truncated: big, ...(big ? { truncated_by: 'rows', truncated_note: '结果已按 1000 行上限截断，可加过滤条件或分页读取。' } : {}), executed_sql: `${sql.trim()} LIMIT 1001`, kind: 'pgsql', stmt_kind: /^\s*explain/i.test(sql) ? 'explain' : 'select', warnings: ['警告一：含截断单元格'], notices: ['提示一：已补 LIMIT 1001', '提示二'], unmasked: false }) };
    case 'mongo':
      return { status: 200, body: ok({ ...common, columns: ['_id', 'status', 'items'], rows: [['ObjectId("65a1")', 'paid', '[{"sku":"a;b"}]']], row_count: 1, truncated: false, executed_sql: 'db.orders.find({}).limit(1001)', kind: 'mongo', stmt_kind: 'find', warnings: [], notices: [], unmasked: false, _echo: sql }) };
    case 'clickhouse':
      return { status: 200, body: ok({ ...common, columns: ['n'], column_types: ['UInt8'], rows: [[1]], row_count: 1, truncated: big, ...(big ? { truncated_by: 'bytes', truncated_note: '结果超过 4 MiB 字节上限，已截断。' } : {}), executed_sql: 'select 1', kind: 'clickhouse', stmt_kind: 'select', warnings: [], notices: ['已忽略 FORMAT 子句'], unmasked: false }) };
    case 'redis':
      return { status: 200, body: ok({ columns: ['value'], rows: [['v1']], row_count: 1, effective_limit: 1000, limit_source: 'db0', truncated: false, cost_ms: 2, masked_columns: [], executed_sql: sql, kind: 'redis' }) };
    default: {
      if (/^\s*explain/i.test(sql)) return { status: 200, body: ok({ columns: ['id', 'select_type', 'table', 'type', 'key', 'rows', 'Extra'], rows: [[1, 'SIMPLE', 'orders', 'ALL', null, 1000, 'Using where']], row_count: 1, effective_limit: 500, limit_source: 'x', truncated: false, cost_ms: 3, masked_columns: [], executed_sql: sql, kind: 'explain', plan_tips: [] }) };
      if (/manyrows/i.test(sql)) return { status: 200, body: ok({ ...common, columns: ['id', 'name', 'amount'], column_types: ['int', 'varchar', 'decimal'], rows: Array.from({ length: 300 }, (_, i) => [i + 1, 'name-' + (i + 1), (i + 1) * 1.5]), row_count: 300, truncated: false, executed_sql: sql, kind: 'select', notices: ['旧提示：已改写 LIMIT'] }) };
      if (/gridtest/i.test(sql)) return { status: 200, body: ok({ ...common, columns: GRID.columns, column_types: GRID.types, rows: GRID.rows(), row_count: 4, truncated: false, executed_sql: sql, kind: 'select' }) };
      return { status: 200, body: ok({ columns: ['id', 'phone'], rows: [[1, '138****0000']], row_count: 1, effective_limit: 500, limit_source: '管理员上限', truncated: big, cost_ms: 5, masked_columns: ['phone'], mask_mode: 'syntax-tree', executed_sql: sql, notice: '旧提示：已改写 LIMIT', kind: 'select' }) };
    }
  }
}

// ---- 字典 ----
const DICT_NOTES = {
  pgsql: ['行数是 PostgreSQL 的统计估算（pg_class.reltuples），尚未统计过的表不显示；PostgreSQL 不记录创建/修改时间，因此没有这两项。'],
  mongo: ['MongoDB 没有固定 schema：字段与类型是对集合采样前 100 个文档推断的，没出现在样本里的字段不会列出；文档数是元数据计数，非精确。没有表注释，也不记录创建/修改时间。'],
  clickhouse: ['ClickHouse 不记录表的创建时间；“更新时间”是最近一次 DDL（元数据文件修改时间）。行数/大小对非 MergeTree 引擎可能为空，显示为“-”。'],
};
function dictObjects(inst, kind) {
  switch (inst.DBType) {
    case 'pgsql':
      return kind === 'table'
        ? [{ schema: 'public', name: 'orders', comment: '订单', rows_est: 12034, size_bytes: 3145728 }, { schema: 'public', name: 'users', comment: '' }, { schema: 'audit', name: 'events', comment: '审计', rows_est: 900000, size_bytes: 1073741824 }]
        : [{ schema: 'public', name: 'v_orders', comment: '' }];
    case 'mongo':
      return kind === 'table' ? [{ name: 'orders', comment: '', type: 'collection', rows_est: 5200, size_bytes: 1048576 }, { name: 'metrics', comment: '', type: 'timeseries' }] : [{ name: 'v_active', comment: '视图，源集合 orders', type: 'view' }];
    case 'clickhouse':
      return kind === 'table' ? [{ name: 'events', comment: '埋点', type: 'MergeTree', rows_est: 8000000, size_bytes: 2147483648 }, { name: 'dict_country', comment: '', type: 'Dictionary' }] : [{ name: 'v_recent', comment: '', type: 'View' }];
    default:
      return [{ name: 'orders', comment: '订单', rows_est: 1000, size_bytes: 16384, sensitive: 1 }];
  }
}
function dictDetail(inst, q) {
  const sections = [
    { title: '基本信息', type: 'kv', pairs: [{ key: '类型', value: inst.DBType === 'mongo' ? 'collection' : 'table' }, { key: '行数（估算）', value: '' }] },
    { title: '字段', type: 'table', columns: ['列名', '类型', '可空', '默认值', '键', '额外', '注释'], rows: [['id', 'integer', '否', '', 'PRI', '', '主键']] },
    { title: '索引', type: 'table', columns: ['索引名', '列', '唯一', '类型'], rows: [] },
    { title: '建表语句', type: 'code', text: 'CREATE TABLE ' + q.get('name') + ' (...)' },
  ];
  return { kind: q.get('kind'), schema: q.get('schema') || undefined, name: q.get('name'), sections, notes: DICT_NOTES[inst.DBType] };
}

// ---- 实例管理 ----
function instItem(i) {
  const label = { mysql: 'MySQL', mssql: 'SQL Server', redis: 'Redis', pgsql: 'PostgreSQL', mongo: 'MongoDB', clickhouse: 'ClickHouse' }[i.DBType];
  const port = { mysql: 3306, mssql: 1433, redis: 6379, pgsql: 5432, mongo: 27017, clickhouse: 8123 }[i.DBType];
  const extra = ['pgsql', 'mongo', 'clickhouse'].includes(i.DBType) ? { features: ['query'], features_label: '在线查询' } : {};
  return { id: i.ID, instance_name: i.Name, type: i.Role, db_type: i.DBType, db_type_label: label, host: '10.0.0.' + i.ID, port, env: i.Env, env_label: i.Env, groups: [{ id: 1, name: 'g1' }], tags: [], is_enable: false, disabled: false, tunnel_id: 0, user: 'u', db_name: '', charset: '', show_db_name_regex: '', denied_db_name_regex: '', service_name: '', sid: '', mode: '', is_ssl: false, verify_ssl: true, can_binlog: i.DBType === 'mysql', ...extra };
}

// ---- 页面外壳 ----
function readTpl(rel) {
  return untranslate(fs.readFileSync(path.join(tplRoot, rel), 'utf8').replace(/\r\n/g, '\n'));
}
function sections(rel) {
  const t = readTpl(rel);
  const ci = t.indexOf('{{define "content"}}');
  const si = t.indexOf('{{define "scripts"}}');
  let content = t.slice(ci + '{{define "content"}}'.length, si).replace(/\n\{\{end\}\}\s*$/, '');
  let scripts = t.slice(si + '{{define "scripts"}}'.length).replace(/\{\{end\}\}\s*$/, '');
  const head = (t.match(/\{\{define "head"\}\}([\s\S]*?)\{\{end\}\}/) || [, ''])[1];
  return { content, scripts, head };
}
function shell(title, { head, content, scripts }) {
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="csrf-token" content="tok"><title>${title}</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${head}</head><body><div class="app"><div class="main"><div class="content">${content}</div></div></div>${scripts}</body></html>`;
}
function queryPage() {
  const s = sections('query/index.html');
  s.content = s.content
    .replace(/\{\{range \.Data\.Instances\}\}([\s\S]*?)\{\{else\}\}[\s\S]*?\{\{end\}\}/g, (_, body) => INSTANCES.map((i) => body.replace(/\{\{\.(\w+)\}\}/g, (_m, k) => i[k] ?? '')).join(''))
    .replace('{{.Data.ApplyEnabled}}', 'true').replace('{{.Data.ExportEnabled}}', 'true')
    .replace('{{.Data.Policy.MaxExecSeconds}}', '60').replace('{{.Data.Policy.DisableStar}}', 'false').replace('{{.Data.Policy.DataMasking}}', 'true');
  return shell('query', s);
}
function applyPage() {
  const t = readTpl('applies.html');
  const a = t.indexOf('<section class="card" id="apply-panel"');
  const b = t.indexOf('</section>', a) + '</section>'.length;
  const panel = t.slice(a, b).replace('{{if not .Data.ReasonRequired}} hidden{{end}}', '');
  const content = `<main class="page" id="applies"><div class="page-head"><h1>查询权限申请</h1><button class="btn primary" id="new-apply" type="button">申请权限</button></div>${panel}</main>`;
  return shell('apply', { head: '<link rel="stylesheet" href="/static/css/apply.css">', content, scripts: '<script type="module" src="/static/js/apply/apply.js"></script>' });
}
function instPage() {
  const s = sections('instmgr/list.html');
  s.content = s.content.replace('{{template "instmgr_side" .Data}}', '');
  return shell('inst', s);
}
// 侧栏手风琴的测试页：分组与条目同 layout.html 的 <nav>；on 指当前页所在分组。
function navPage(on) {
  const groups = [['check', '变更发布', ['变更工单', 'SQL 体检']], ['query', '数据服务', ['在线查询', '数据导出']], ['opt', '性能优化', ['智能调优', '慢 SQL 洞察']],
    ['perf', '性能治理', ['问题工单']], ['inst', '实例运维', ['实例列表']], ['access', '访问控制', ['用户']], ['secure', '安全合规', ['审批流程', '审计日志']],
    ['plat', '平台设置', ['环境', '系统配置']], ['help', '帮助中心', ['使用文档']]];
  const html = groups.map(([k, label, items]) => `<div class="ng${k === on ? ' on' : ' fold'}" data-group="${k}"><button class="ng-btn" type="button" aria-expanded="${k === on}"><span>${label}</span><i class="caret"></i></button>` +
    `<div class="ng-items">${items.map((it) => `<a href="#${k}">${it}</a>`).join('')}</div></div>`).join('');
  return shell('nav', { head: '', content: `<aside class="side" id="side"><nav class="nav" id="nav">${html}</nav></aside>`, scripts: '<script type="module" src="/static/js/common.js"></script>' });
}
// 慢 SQL 洞察页（性能优化）：模板里的 {{if}} 去掉，保留按钮；接口见 internal/slowlog/http.go。
function slowQueryPage() {
  const s = sections('slowlog/slowquery.html');
  s.content = s.content.replace(/{{if .Data.w+}}([sS]*?){{end}}/g, '$1');
  s.head = '<link rel="stylesheet" href="/static/css/slowlog.css"><link rel="stylesheet" href="/static/css/slowquery.css">';
  return shell('slowquery', s);
}
export const SLOW_INSTANCES = [
  { id: 1, name: 'mysql-off', db_type: 'mysql', source: 'collected', collect_enabled: false },
  { id: 2, name: 'mysql-on', db_type: 'mysql', source: 'collected', collect_enabled: true },
  { id: 3, name: 'doris-a', db_type: 'doris', source: 'collected', collect_enabled: true },
];
// 平台设置 › 系统配置：接口 GET /api/v1/settings/config 的形状同 internal/sysconfig 的 View
// （{ ready, groups[{key,label,desc,total,modified,issues}], items[settings.Item], security, undecryptable[] }）；
// 每个 item 是 internal/settings 的 Def（key/group/section/label/help/type/default/unit/min/max/ranged/options/risk/locked/link）
// 加上运行时字段（value/effective/custom/modified/secret_set/undecryptable/updated_by/updated_at/editable/edit_note）。
function sysConfigPage() {
  const s = sections('sysconfig/index.html');
  s.head = '<link rel="stylesheet" href="/static/css/sysconfig.css">';
  return shell('sysconfig', s);
}
const LOCK_PROGRAM = '指向服务器上的程序 / 配置文件，或含目标库账号密码：为避免页面被滥用时让服务执行任意程序，只能用 dbt-admin settings set --force 修改';
// 一项设置：type 是 bool / int / float / string / text / enum / list / secret；未写的运行时字段取“没动过默认值”的状态。
const cfgItem = (o) => ({ section: '', default: '', value: '', effective: o.default || '', custom: false, modified: false, secret_set: false, undecryptable: false, version: 0, editable: !o.locked, ...o });
const SYSCONFIG_ITEMS = [
  cfgItem({ key: 'security.login_lock_attempts', group: 'security', section: '登录锁定', label: '连续失败锁定次数', type: 'int', default: '5', ranged: true, min: 1, max: 1000, help: '连续登录失败达到这个次数后锁定账号。',
    value: '3', effective: '3', custom: true, modified: true, version: 2, updated_by: 'admin', updated_at: '2026-10-10T08:00:00Z' }),
  cfgItem({ key: 'security.self_signup', group: 'security', section: '注册与默认权限', label: '允许自助注册', type: 'bool', default: 'false', help: '登录页是否显示注册入口。' }),
  cfgItem({ key: 'sms.aliyun_access_key_secret', group: 'sms', section: '阿里云', label: 'AccessKey Secret', type: 'secret', secret_set: true, custom: true, modified: true, version: 1, updated_by: 'admin', updated_at: '2026-10-09T08:00:00Z' }),
  cfgItem({ key: 'sms.tencent_secret_key', group: 'sms', section: '腾讯云', label: 'SecretKey', type: 'secret', custom: true, undecryptable: true }),
  cfgItem({ key: 'query.mask_enabled', group: 'query', section: '脱敏', label: '查询结果脱敏总开关', type: 'bool', default: 'false', help: '关闭后在线查询返回原值，脱敏规则暂不生效。',
    link: { label: '数据脱敏', href: '/settings/masking' }, value: 'true', effective: 'true', custom: true, modified: true, version: 1, updated_by: 'admin', updated_at: '2026-10-08T08:00:00Z' }),
  cfgItem({ key: 'query.forbid_select_star', group: 'query', section: '查询限制', label: '禁止 select *', type: 'bool', default: 'false', value: 'false', effective: 'false', custom: true, version: 1 }),
  cfgItem({ key: 'query.max_execution_seconds', group: 'query', section: '查询限制', label: '查询超时', type: 'int', default: '60', unit: '秒', ranged: true, min: 1, max: 3600, help: '超时后发 KILL QUERY 终止这条查询。' }),
  cfgItem({ key: 'firewall.mode', group: 'firewall', section: '全局', label: '全局模式', type: 'enum', default: 'observe', help: '观察模式下规则只记录命中，不拦截。',
    options: [{ value: 'observe', label: '观察（只记录不拦截）' }, { value: 'enforce', label: '拦截' }], link: { label: 'SQL 防火墙', href: '/settings/firewall' } }),
  cfgItem({ key: 'export.storage_type', group: 'export', section: '存储', label: '导出文件存储类型', type: 'enum', default: 'local', options: [{ value: 'local', label: '本地目录' }], help: '目前只支持本地目录。' }),
  cfgItem({ key: 'export.max_rows', group: 'export', section: '上限', label: '导出行数上限', type: 'int', default: '10000', unit: '行', ranged: true, min: 1, max: 10000000, help: '导出工单最多导出多少行；结果全部读进内存，别调太大。',
    value: '500000', effective: '500000', custom: true, modified: true, version: 3, updated_by: 'admin', updated_at: '2026-10-11T08:00:00Z' }),
  cfgItem({ key: 'optimizer.soar_path', group: 'optimizer', section: 'SOAR', label: 'SOAR 程序路径', type: 'string', locked: LOCK_PROGRAM, edit_note: LOCK_PROGRAM, help: '未配置时优化页提示未配置（409）。' }),
];
const SYSCONFIG_GROUPS = [
  { key: 'security', label: '登录与安全', desc: '登录锁定、密码与会话、两步验证、自助注册、页面水印' },
  { key: 'sms', label: '短信', desc: '短信两步验证使用的短信服务商与密钥' },
  { key: 'query', label: '在线查询与脱敏', desc: '查询超时、行数 / 字节上限、结果脱敏' },
  { key: 'firewall', label: 'SQL 防火墙', desc: '总开关、全局模式与出错策略（规则在“SQL 防火墙”页面维护）' },
  { key: 'export', label: '数据导出', desc: '导出文件的存放位置与上限' },
  { key: 'optimizer', label: 'SQL 优化工具', desc: 'SOAR 程序路径与测试环境（只能用命令行修改）' },
].map((g) => {
  const its = SYSCONFIG_ITEMS.filter((i) => i.group === g.key);
  return { ...g, total: its.length, modified: its.filter((i) => i.modified).length, issues: its.filter((i) => i.undecryptable).length };
});
export const SYSCONFIG = {
  ready: true,
  security: { public_secret_key: true, separate_mirage_key: true },
  groups: SYSCONFIG_GROUPS,
  items: SYSCONFIG_ITEMS,
  undecryptable: ['sms.tencent_secret_key'],
};
const PAGES = { '/query': queryPage, '/applies': applyPage, '/instances': instPage, '/slowquery': slowQueryPage, '/settings/config': sysConfigPage, '/navtest': () => navPage(''), '/navtest-on-secure': () => navPage('secure') };

export function startServer(port) {
  const server = http.createServer(async (req, res) => {
    const u = new URL(req.url, 'http://x');
    const p = decodeURIComponent(u.pathname);
    let body = '';
    for await (const c of req) body += c;
    const json = (status, obj) => { res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(obj)); };
    if (req.method !== 'GET' || p.startsWith('/api/') || p.startsWith('/ui/')) {
      let parsed = null;
      try { parsed = body ? JSON.parse(body) : null; } catch (_) { /* 非 JSON */ }
      calls.push({ method: req.method, url: p + u.search, body: parsed });
    }
    if (PAGES[p]) { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' }); res.end(PAGES[p]()); return; }
    if (p.startsWith('/static/')) {
      const f = path.join(staticRoot, p.slice(8));
      if (f.startsWith(staticRoot) && fs.existsSync(f) && fs.statSync(f).isFile()) {
        res.writeHead(200, { 'Content-Type': MIME[path.extname(f)] || 'application/octet-stream', 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' });
        fs.createReadStream(f).pipe(res);
        return;
      }
      res.writeHead(404); res.end('nf'); return;
    }
    // ---- /ui 片段 ----
    if (p === '/ui/query/databases') {
      const id = u.searchParams.get('instance');
      const html = (DBS[id] || []).map((d) => `<option value="${d}">${d}</option>`).join('');
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(html); return;
    }
    // ---- 公共 ----
    if (p === '/api/v1/button-perms/me') return json(200, ok({ mode: 'compat', deny_display: 'hide', superuser: true, allowed: [] }));
    if (p === '/api/v1/query/history') {
      const kws = (u.searchParams.get('q') || '').toLowerCase().split(/\s+/).filter(Boolean);
      const user = (u.searchParams.get('user') || '').toLowerCase();
      const inst = u.searchParams.get('instance') || '';
      const status = u.searchParams.get('status') || '';
      let rows = fake.history.filter((h) => (fake.admin || h.username === 'me')
        && (!fake.admin || !user || h.username.toLowerCase().includes(user) || (h.display || '').toLowerCase().includes(user))
        && (!inst || h.instance === inst) && (!status || h.status === status) && kws.every((k) => h.sql.toLowerCase().includes(k)));
      const off = +(u.searchParams.get('offset') || 0), lim = +(u.searchParams.get('limit') || 20);
      return json(200, ok({ items: rows.slice(off, off + lim), total: rows.length, scope: fake.admin ? (user ? `用户名或姓名含“${user}”的人执行过的 SQL` : '所有人执行过的 SQL') : '我执行过的 SQL', admin: fake.admin, events: fake.events }));
    }
    if (p === '/api/v1/query/history/users') {
      if (!fake.admin) return json(403, { code: 'forbidden', message: '只有管理员可以查看其他用户的执行历史' });
      const seen = {};
      fake.history.forEach((h) => { seen[h.username] = seen[h.username] || { username: h.username, display: h.display, count: 0 }; seen[h.username].count++; });
      return json(200, ok(Object.values(seen)));
    }
    // ---- 收藏 ----
    if (p === '/api/v1/query/favorites' && req.method === 'GET') {
      if (fake.favError) return json(503, { code: 'table_missing', message: fake.favError });
      const kws = (u.searchParams.get('q') || '').toLowerCase().split(/\s+/).filter(Boolean);
      let rows = fake.fav.filter((f) => kws.every((k) => f.sql.toLowerCase().includes(k) || f.comment.toLowerCase().includes(k)));
      const key = u.searchParams.get('sort') === 'created' ? (f) => f.created_at : (f) => f.last_used_at || f.created_at;
      const dir = u.searchParams.get('order') === 'asc' ? 1 : -1;
      rows = rows.slice().sort((a, b) => (key(a) < key(b) ? -dir : key(a) > key(b) ? dir : 0));
      const off = +(u.searchParams.get('offset') || 0), lim = +(u.searchParams.get('limit') || 20);
      return json(200, ok({ items: rows.slice(off, off + lim), total: rows.length, max: 500 }));
    }
    if (p === '/api/v1/query/favorites' && req.method === 'POST') {
      const b = JSON.parse(body);
      const sql = (b.sql || '').trim();
      if (!sql) return json(400, { code: 'bad_request', message: 'SQL 不能为空' });
      const cur = fake.fav.find((f) => f.sql === sql);
      if (cur) { if (b.comment) cur.comment = b.comment; return json(200, ok({ item: cur, existed: true })); }
      const now = new Date(Date.UTC(2026, 9, 13, 9, ++fake.favSeq)).toISOString().slice(0, 19).replace('T', ' ');
      const it = { id: fake.favSeq, comment: (b.comment || '').trim(), sql, instance: b.instance || '', db: b.db || '', created_at: now, last_used_at: '', use_count: 0 };
      fake.fav.push(it);
      return json(200, ok({ item: it, existed: false }));
    }
    const fm = p.match(/^\/api\/v1\/query\/favorites\/(\d+)(\/use)?$/);
    if (fm) {
      const it = fake.fav.find((f) => f.id === +fm[1]);
      if (!it) return json(404, { code: 'not_found', message: '收藏不存在' });
      if (req.method === 'DELETE') { fake.fav = fake.fav.filter((f) => f !== it); return json(200, ok({ ok: true })); }
      if (fm[2]) { it.use_count++; it.last_used_at = new Date(Date.UTC(2026, 9, 13, 10, it.use_count)).toISOString().slice(0, 19).replace('T', ' '); return json(200, ok({ ok: true })); }
      const b = JSON.parse(body);
      it.comment = (b.comment || '').trim();
      it.sql = (b.sql || '').trim();
      return json(200, ok(it));
    }
    // ---- 查询 ----
    let m;
    if (p === '/api/v1/query/execute' && req.method === 'POST') {
      const b = JSON.parse(body);
      const r = execute(b);
      const d = String(b.sql || '').match(/\bdelay(\d+)\b/i); // 语句里写 delay400：先等 400 毫秒再回（运行全部的执行中 / 取消 / 同时执行）
      if (d) await new Promise((ok) => setTimeout(ok, +d[1]));
      return json(r.status, r.body);
    }
    if (p === '/api/v1/query/estimate') return json(200, ok({ rows: 4321, method: 'explain' }));
    if (p === '/api/v1/settings/config') return json(200, ok(SYSCONFIG));
    if (p === '/api/v1/slowlog/instances') return json(200, ok(SLOW_INSTANCES));
    if (p === '/api/v1/slowlog/databases') return json(200, ok([]));
    if (p === '/api/v1/slowlog/review' || p === '/api/v1/slowlog/review/history') {
      const all = u.searchParams.get('all') === '1';
      return json(200, ok({ total: 0, rows: [], source: all ? 'all' : 'collected', source_label: all ? '全部实例汇总' : '平台采集（dbt_slow_logs）', from: '2026-10-07', to: '2026-10-13', summary: null }));
    }
    if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases\/([^/]+)\/programmables$/))) return json(200, ok({ groups: programmables(byId(m[1])) }));
    if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases\/([^/]+)\/object-info$/))) {
      if (u.searchParams.get('name') === 'gone') return json(404, { code: 'not_found', message: '表 gone 不存在，或当前账号没有权限查看' });
      return json(200, ok(objectInfo(byId(m[1]), u.searchParams.get('kind'), u.searchParams.get('name'), u.searchParams.get('schema'))));
    }
    if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/redis\/databases$/))) return json(200, ok([{ db: '0', keys: 3 }, { db: '1', keys: -1 }]));
    if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases\/([^/]+)\/keys$/))) return json(200, ok({ keys: [{ key: 'user:1', arg: 'user:1', type: 'hash' }, { key: 'k2', arg: 'k2', type: 'string' }], cursor: '0', done: true }));
    if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases$/))) return json(200, ok(DBS[m[1]] || []));
    if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases\/([^/]+)\/tables$/))) return json(200, ok(metaFor(m[1], m[2]).tables.map((t) => t.name)));
    if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases\/([^/]+)\/meta$/))) return json(200, ok(metaFor(m[1], m[2])));
    if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases\/([^/]+)\/tables\/([^/]+)\/schema$/))) return json(200, ok((SCHEMAS[m[1]] || {})[m[3]] || [col('id', 'int', { key: 'PRI' })]));
    if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases\/([^/]+)\/search$/))) {
      const id = Number(m[1]);
      if (id === 5) return json(200, ok([]));
      return json(200, ok(id === 4 ? [{ table: 'audit.events', column: 'payload', comment: '' }] : []));
    }
    if (p === '/api/v1/query/apply/options') return json(200, ok({ instances: INSTANCES.map((i) => ({ id: i.ID, name: i.Name, db_type: i.DBType })), groups: ['g1'], reason_required: true }));
    if (p === '/api/v1/query/privilege-applies' && req.method === 'POST') return json(200, ok({ apply_id: 77 }));
    // ---- 字典 ----
    // ---- 实例管理 ----
    if (p === '/api/v1/instmgr/caps') return json(200, ok({ 'instance.add': true, 'instance.edit': true, 'instance.delete': true, 'instance.test': true, 'instance.purge_binlog': true, 'session.open': true, 'database.view': true, 'account.view': true, 'param.open': true }));
    if (p === '/api/v1/instances/form-options') return json(200, ok({ groups: [{ id: 1, name: 'g1' }], tags: [], tunnels: [], ext_ready: true, db_types: [['mysql', 'MySQL'], ['mssql', 'SQL Server'], ['redis', 'Redis'], ['pgsql', 'PostgreSQL'], ['mongo', 'MongoDB'], ['clickhouse', 'ClickHouse']].map(([value, label]) => ({ value, label })) }));
    if (p === '/api/v1/instances' && req.method === 'GET') { const items = INSTANCES.map(instItem); return json(200, ok({ items, total: items.length, stats: { total: items.length, disabled: 0, collecting: 0 }, ext_ready: true })); }
    if (p === '/api/v1/instances/batch/disable' && req.method === 'POST') {
      const b = JSON.parse(body);
      return json(200, ok({ items: b.ids.map((id) => ({ id, name: (byId(id) || {}).Name || '#' + id, ok: true })), done: b.ids.length, failed: 0 }));
    }
    if (p === '/api/v1/instances/batch/collect' && req.method === 'POST') {
      const b = JSON.parse(body);
      return json(200, ok({ items: b.ids.map((id) => ({ id, name: (byId(id) || {}).Name || '#' + id, ok: true })), done: b.ids.length, failed: 0 }));
    }
    if (p === '/api/v1/instances/batch/delete' && req.method === 'POST') {
      const b = JSON.parse(body);
      const items = b.ids.map((id) => (id === 6
        ? { id, name: (byId(id) || {}).Name || '#' + id, ok: false, message: '仍被引用：SQL 工单 3 条' }
        : { id, name: (byId(id) || {}).Name || '#' + id, ok: true }));
      return json(200, ok({ items, done: items.filter((x) => x.ok).length, failed: items.filter((x) => !x.ok).length, dry_run: !!b.dry_run }));
    }
    if ((m = p.match(/^\/api\/v1\/instances\/(\d+)\/health$/))) return json(200, ok({ status: 'ok', version: '1.0', qps: 1, connections: 1, latency_ms: 3, checked_at: 'now' }));
    json(404, { code: 'not_found', message: '假后端未实现：' + p });
  });
  return new Promise((r) => server.listen(port, '127.0.0.1', () => r(server)));
}
