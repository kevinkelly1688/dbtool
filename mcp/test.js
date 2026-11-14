/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: test.js
 * @time: 2026/10/28
 */
'use strict';
/**
 * dbtool MCP 的自测：起一个本地模拟的 dbtool（接口形状按 dbtool 源码里的处理函数写），
 * 再用 stdio 方式拉起 index.js，逐个调用工具。零依赖：node --test test.js（Node ≥ 18）。
 *
 * 模拟服务只覆盖查询类工具用到的接口；没有对接的工单类工具必须“不访问 dbtool”，这里也会校验。
 */
const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const path = require('node:path');
const { spawn } = require('node:child_process');

// ── 模拟 dbtool ─────────────────────────────────────────────────────────
const INSTANCES = [
  { id: 1, name: '(生产) 平台Saas [PolarDB-X]-tenant', db_type: 'polardb-x', role: 'master', env: 'prod', env_label: '生产', env_prod: true, tags: ['生产', '支持查询'] },
  { id: 2, name: 'test-mysql-01', db_type: 'mysql', role: 'master', env: 'test', env_label: '测试', env_prod: false, tags: ['测试'] },
  { id: 3, name: 'test-mysql-02', db_type: 'mysql', role: 'master', env: 'test', env_label: '测试', env_prod: false, tags: ['测试'] },
  { id: 11, name: 'ar001', db_type: 'mssql', role: 'master', env: 'prod', env_label: '生产', env_prod: true, tags: [] },
  { id: 12, name: 'ar002', db_type: 'mssql', role: 'master', env: 'prod', env_label: '生产', env_prod: true, tags: [] },
  { id: 13, name: 'ar003', db_type: 'mssql', role: 'master', env: 'prod', env_label: '生产', env_prod: true, tags: [] },
  { id: 14, name: 'ar004', db_type: 'mssql', role: 'master', env: 'prod', env_label: '生产', env_prod: true, tags: [] },
  { id: 15, name: 'ar005', db_type: 'mssql', role: 'master', env: 'prod', env_label: '生产', env_prod: true, tags: [] }, // 不在任何资源组里
];

// 提交页的选项（GET /api/v1/sqlworkflow/options）：两个资源组；批量只允许 mssql，最多 3 个。
// 导出：生产DDL上线 组里 ar004 能上线但不能导出（没开“查询”或没有库表查询权限）；ar005 不在任何资源组里
function makeOptions() {
  const opt = (...ids) => ids.map((id) => { const i = INSTANCES.find((x) => x.id === id); return { id: i.id, name: i.name, db_type: i.db_type }; });
  return {
    groups: [
      { id: 1, name: '生产DDL上线', instances: opt(1, 2, 11, 12, 13, 14), export_instances: opt(1, 11, 12, 13) },
      { id: 2, name: '测试环境上线', instances: opt(2, 3), export_instances: opt(2, 3) },
    ],
    batch: { allowed: true, types: ['mssql'], max: 3 },
    export: {
      allowed: true, batch_allowed: true, max_rows: 10000, seconds: 60, can_unmask: false, can_merge: true,
      formats: ['csv', 'xlsx', 'json', 'xml', 'sql'].map((value) => ({ value, label: value.toUpperCase() })),
    },
    inception: false, native_allowed: true,
  };
}

// 令牌载荷与 dbtool 签发的一致：uid / username / su（登录时是否超级管理员）/ iat / exp
function makeToken(ttlSec, username = 'tester', su = false) {
  const b64 = (o) => Buffer.from(JSON.stringify(o)).toString('base64url');
  const now = Math.floor(Date.now() / 1000);
  return `${b64({ alg: 'HS256', typ: 'JWT' })}.${b64({ uid: 1, username, su, iat: now, exp: now + ttlSec })}.sig`;
}

// 批量工单（SQL Server）：_ 开头的字段是模拟服务自己用的，不会出现在响应里
function makeBatchWorkflows() {
  const base = (id, extra) => ({
    id, name: `批量工单${id}`, status: 'workflow_manreviewing', status_label: '等待审批', is_batch: true, is_export: false,
    batch_targets: ['ar001', 'ar002', 'ar003'], batch_instances: [], instance: 'ar001', db_name: 'shop', group: '生产DDL上线',
    engineer: '张三（显示名）', _engineer: 'alice', can_review: true, can_execute: false,
    current_approvers: [{ username: 'ls', display: '李四' }], steps: [], logs: [], review_rows: [], exec_rows: [], ...extra,
  });
  const reviewPass = { status: 'workflow_review_pass', status_label: '审核通过', can_review: false, can_execute: true };
  return {
    201: base(201),
    202: base(202, { _engineer: 'root' }), // 超级管理员自己提交的
    203: base(203, { is_batch: false, batch_targets: [] }), // 单实例工单
    204: base(204, { status: 'workflow_finish', status_label: '已正常结束', can_review: false }),
    205: base(205, reviewPass),
    206: base(206, { ...reviewPass, can_execute: false }),
    207: base(207, { can_review: false }),
    209: base(209, { ...reviewPass, _engineer: 'root' }),
    210: base(210, { _levels: 2 }), // 两级审批
    211: base(211, { _noListRow: true }), // 列表里查不到这一行
    212: base(212, { _engineer: 'ROOT' }), // 提交人登录名大小写不同
    213: base(213, { is_export: true, export_unmasked: { reason: '对账', active: true } }),
    214: base(214, { status: 'workflow_timingtask', status_label: '定时执行', can_review: false, can_execute: true }),
    215: base(215, { ...reviewPass, _rejectExecute: '不在可执行时间范围内，如果需要修改执行时间请重新提交工单!' }),
    216: base(216, { _rejectPass: '用户不在当前节点的审核组内，无权限审核' }), // MCP 的检查都过了，但 dbtool 服务端不让审
    // 终止（撤回）工单：提交人是测试账号 tester 自己的（mine），can_cancel 是 dbtool 算好的“能不能终止”
    ...Object.fromEntries([
      [301, {}], // 批量上线工单，等待审批
      [302, { is_batch: false, batch_targets: [] }], // 单实例上线工单
      [303, { is_export: true }], // 批量导出工单
      [304, reviewPass], // 审核通过
      [305, { status: 'workflow_timingtask', status_label: '定时执行' }],
      [306, { status: 'workflow_executing', status_label: '执行中' }],
      [307, { status: 'workflow_queuing', status_label: '排队中' }],
      [308, { status: 'workflow_finish', status_label: '已正常结束' }],
      [309, { status: 'workflow_abort', status_label: '人工终止流程' }],
      [310, { status: 'workflow_autoreviewwrong', status_label: '自动审批不通过' }],
      [312, { can_cancel: false }], // dbtool 判定当前账号不能终止（比如没有终止按钮权限）
      [313, { _noListRow: true }], // 列表里查不到这一行
      [314, { _engineer: 'TESTER' }], // 提交人登录名大小写不同
      [315, { _rejectCancel: '你无权操作当前工单！' }], // MCP 的检查都过了，但 dbtool 服务端不让终止
      [316, { _hangCancel: true }], // 终止请求收到了，但连接被断开
      [317, { _engineer: '' }], // 列表里的提交人是空的
      [318, {}], [319, {}], [320, {}], // 备注长度的测试各用一张
      [321, { is_batch: false, batch_targets: [], is_export: true }], // 单实例导出工单
      [322, { _weirdCancel: true }], // 终止请求成功了，但响应里的状态不是“人工终止流程”
      [323, { _emptyCancel: true }], // 终止请求成功了，但响应里没有新状态
    ].map(([id, extra]) => [id, base(id, { _engineer: 'tester', can_cancel: true, can_review: false, ...extra })])),
    311: base(311, { can_cancel: true }), // 别人（alice）提交的工单，dbtool 说能终止（比如账号有驳回权限），MCP 也不让
  };
}

// 数据导出预检（POST /sqlworkflow/check 带 export:true；按服务端 checkExport 的行为写）：每个实例一行；单实例没有 instances；
// T-SQL 的 declare / set 批处理统计不了行数，只给一行警告。SQL 里的关键字用来触发各种结果：zero（0 行）/ toomany（第一个实例超过行数上限）/
// denied（最后一个实例没有库表查询权限）/ colmismatch（带 export_merge 时第二个实例的列不一致）。返回 [HTTP 状态, 响应体]
function exportCheck(st, body) {
  const sql = String(body.sql);
  const batch = !!body.instance_ids;
  const ids = body.instance_ids || [body.instance_id];
  const names = ids.map((id) => (INSTANCES.find((x) => x.id === id) || {}).name);
  const ex = st.options.export;
  if (batch ? !ex.batch_allowed : !ex.allowed) {
    return [403, { code: 'forbidden', message: batch ? '你没有提交批量数据导出工单的权限（缺少 export.submit_batch）' : '你没有提交数据导出工单的权限（缺少 export.submit）' }];
  }
  const data = (rows, instances) => [200, { data: {
    rows, error_count: rows.filter((r) => r.errlevel >= 2).length, warning_count: rows.filter((r) => r.errlevel === 1).length,
    syntax_type: 9, syntax_label: '离线导出', source: 'export', notice: '', instances,
  } }];
  const head = sql.replace(/^(\s|\/\*[\s\S]*?\*\/|--[^\n]*(\n|$)|#[^\n]*(\n|$))+/, ''); // 服务端的 FirstKeyword 会跳过开头的注释
  if (!/^(select|with|declare|set)\b/i.test(head)) { // 服务端自己的校验（MCP 本地检查在先，这里只是兜底）
    return data([{ id: 1, stage: '自动审核失败', errlevel: 2, stagestatus: '检查未通过！', errormessage: '违规语句：数据导出只允许 SELECT / WITH 查询', sql }], null);
  }
  if (/^(declare|set)\b/i.test(head)) {
    // 服务端的 affected_rows 没有 omitempty：没统计时也会输出 0
    return data([{ id: 1, errlevel: 1, stagestatus: '批处理语句，跳过行数预统计', errormessage: '检测到 declare/set 批处理语句，无法预先统计行数；执行时若超过阈值(10000)将中止导出。', sql, affected_rows: 0 }], null);
  }
  const count = sql.includes('zero') ? 0 : 120;
  if (sql.includes('manyerrors')) { // 12 条出错的行（第 1 条的原因特别长），用来测报错里的截断
    const rows12 = Array.from({ length: 12 }, (_, i) => ({ id: i + 1, errlevel: 2, stagestatus: '检查未通过！', errormessage: i === 0 ? `第 1 个问题：${'长'.repeat(500)}` : `第 ${i + 1} 个问题`, sql, affected_rows: 0 }));
    return data(rows12, null);
  }
  const rows = ids.map((id, i) => {
    const who = batch ? `实例[${names[i]}]` : '';
    const r = { id: i + 1, errlevel: 0, stagestatus: '行数统计完成', errormessage: batch ? `${who}待导出 ${count} 行` : 'None', sql, affected_rows: count };
    if (batch) Object.assign(r, { instance_id: id, instance_name: names[i] });
    if (sql.includes('toomany') && i === 0) Object.assign(r, { errlevel: 2, stagestatus: '检查未通过！', affected_rows: 20000, errormessage: `${who}导出数据行数(20000)超过阈值(10000)。` });
    if (sql.includes('nomsg') && i === 0) Object.assign(r, { errlevel: 2, stagestatus: '检查未通过！', errormessage: 'None' }); // 出错了但没有写原因
    if (sql.includes('nocount') && i === 0) r.affected_rows = null; // 服务端没给出行数
    if (sql.includes('blankcount') && i === 0) r.affected_rows = '';
    if (sql.includes('denied') && i === ids.length - 1) Object.assign(r, { errlevel: 2, stage: '自动审核失败', stagestatus: '无库表查询权限', affected_rows: 0, errormessage: `${who}没有 shop.orders 的查询权限` });
    if (sql.includes('countfail') && i === ids.length - 1) Object.assign(r, { errlevel: 2, stage: '自动审核失败', stagestatus: '检查未通过！', affected_rows: 0, errormessage: `${who}检查失败: Error 1146: Table 'shop.x' doesn't exist` });
    if (sql.includes('colmismatch') && body.export_merge && i === 1) Object.assign(r, { errlevel: 2, stagestatus: '检查未通过！', errormessage: `${who}返回的列与第一个实例「${names[0]}」不一致（它的列：id, name；这个实例的列：id, title）` });
    return r;
  });
  const instances = batch ? rows.map((r, i) => ({ id: ids[i], name: names[i], ok: r.errlevel < 2, error: r.errlevel >= 2 ? r.errormessage : undefined })) : null;
  if (sql.includes('instonly') && instances) instances[1] = { ...instances[1], ok: false, error: '连接失败：i/o timeout' }; // 行里没写原因，只有逐实例结果里有
  if (sql.includes('countfail') && instances) instances[ids.length - 1].error = "Error 1146: Table 'shop.x' doesn't exist"; // 逐实例结果里的原因不带“实例[…]”和“检查失败:”
  return data(rows, instances);
}

function startStub() {
  const st = {
    logins: 0, requests: [], tokenTtl: 3600, rejectNext: 0, jobPolls: 0, password: 'pw-secret-123', byPath: {},
    users: { tester: { su: false }, root: { su: true } }, // 账号 → 登录时是不是超级管理员
    wf: makeBatchWorkflows(),
    options: makeOptions(), created: [], nextWfId: 500, // 提交页选项；已创建的工单（收到的请求体）
    last: null, // 最近一次业务请求 { method, url, body }
  };
  const send = (res, status, obj) => {
    const body = typeof obj === 'string' ? obj : JSON.stringify(obj);
    res.writeHead(status, { 'Content-Type': typeof obj === 'string' ? 'text/plain' : 'application/json; charset=utf-8' });
    res.end(body);
  };
  const server = http.createServer((req, res) => {
    let raw = '';
    req.on('data', (d) => { raw += d; });
    req.on('end', () => {
      const url = new URL(req.url, 'http://x');
      const p = url.pathname;
      let body;
      try { body = raw ? JSON.parse(raw) : undefined; } catch { body = undefined; }
      st.requests.push(`${req.method} ${req.url}`);
      st.byPath[`${req.method} ${p}`] = body; // 每个接口最近一次收到的请求体

      if (p === '/api/v1/auth/login' && req.method === 'POST') {
        st.logins++;
        const user = body && st.users[body.username];
        if (!user || body.password !== st.password) {
          return send(res, 401, { code: 'bad_credentials', message: `账号或密码错误（${body && body.password}）` }); // 故意回显密码，验证 MCP 会抹掉
        }
        return send(res, 200, { token: makeToken(st.tokenTtl, body.username, user.su) });
      }
      if (!/^Bearer \S+$/.test(req.headers.authorization || '')) return send(res, 401, { code: 'unauthorized', message: '未登录或登录已过期' });
      if (st.rejectNext > 0) { st.rejectNext--; return send(res, 401, { code: 'unauthorized', message: '令牌无效或已过期' }); }
      st.last = { method: req.method, url: req.url, body };

      let m;
      if (p === '/api/v1/query/instances') return send(res, 200, { data: INSTANCES });
      if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases$/))) return send(res, 200, { data: ['shop', 'report'] });
      if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases\/([^/]+)\/tables$/))) return send(res, 200, { data: ['orders', 'users'] });
      if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases\/([^/]+)\/tables\/([^/]+)\/columns$/))) return send(res, 200, { data: ['id', 'user_id'] });
      if ((m = p.match(/^\/api\/v1\/query\/instances\/(\d+)\/databases\/([^/]+)\/object-info$/))) {
        if (url.searchParams.get('name') === 'nope') return send(res, 404, '404 page not found');
        return send(res, 200, { data: {
          kind: url.searchParams.get('kind'), name: url.searchParams.get('name'),
          sections: [
            { title: '基本信息', type: 'kv', pairs: [{ key: '引擎', value: 'InnoDB' }, { key: '估算行数', value: '6' }] },
            { title: '列信息', type: 'table', columns: ['列名', '类型'], rows: [['id', 'bigint'], ['user_id', 'bigint']] },
            { title: '索引信息', type: 'table', columns: ['索引', '列'], rows: [['PRIMARY', 'id']] },
            { title: '创建语句', type: 'code', text: 'CREATE TABLE `orders` (\n  `id` bigint\n)', note: '由系统目录还原，仅供参考' },
          ],
          sensitive_columns: ['phone'], notes: ['行数是估算'],
        } });
      }
      if (p === '/api/v1/query/execute' && req.method === 'POST') {
        const sql = String(body.sql);
        if (sql.includes('need_reason') && !body.reason) return send(res, 403, { code: 'reason_required', message: '命中防火墙规则，需要填写原因' });
        if (/^explain/i.test(sql)) {
          return send(res, 200, { data: { columns: ['id', 'table', 'type'], rows: [[1, 'orders', 'ALL']], row_count: 1, executed_sql: sql, kind: 'explain', plan_tips: [{ level: 'warn', text: '全表扫描' }], masked_columns: [] } });
        }
        if (sql.includes('big')) {
          const rows = Array.from({ length: 4000 }, (_, i) => [i, 'x'.repeat(40)]);
          return send(res, 200, { data: { columns: ['id', 'v'], rows, row_count: rows.length, executed_sql: sql, truncated: false, masked_columns: [] } });
        }
        return send(res, 200, { data: {
          columns: ['id', 'phone'], rows: [[1, '138****0001'], [2, '139****0002']], row_count: 2, effective_limit: body.limit, cost_ms: 12,
          masked_columns: ['phone'], mask_mode: 'syntax-tree', executed_sql: `${sql} LIMIT ${body.limit}`, notice: 'n1',
        } });
      }
      if (p === '/api/v1/query/history') {
        return send(res, 200, { data: { items: [{ id: 1, time: '2026-10-22 10:00:00', username: 'tester', instance: 'ar001', db: 'shop', sql: 's'.repeat(900), rows: 2, cost_sec: '0.1', status: 'ok' }], total: 1, admin: false, scope: '你执行过的 SQL' } });
      }
      if (p === '/api/v1/slowlog/slowlogs') {
        return send(res, 200, { data: { total: 1, from: '2026-10-01', to: '2026-10-22', rows: [{ instance_id: 1, instance_name: INSTANCES[0].name, db_name: 'shop', sql_md5: 'abc', exec_date: '2026-10-21', count: 5, query_time_avg: 1.5, query_time_max: 3, query_time_sum: 7.5, rows_examined_avg: 1000, rows_sent_avg: 1, table_name: 'orders', sql_text: 'select 1', user_host: 'u@h', fingerprint: 'select ?' }] } });
      }
      if (p === '/api/v1/slowlog/slowlogs/detail') return send(res, 200, { data: { total: 1, rows: [{ id: 1, start_time: '2026-10-21 10:00:00', query_time_sec: 2, sql_text: 'select sleep(2)' }] } });
      if (p === '/api/v1/partman/configs') return send(res, 200, { data: [{ id: 3, instance_id: 1, instance_name: 'x', db_type: 'mysql', dbname: 'shop', tbname: 'orders', column: 'created_at', col_type: 'datetime', period: 'day', interval: 1, retain: 30, pre_create: 3, isenable: true, supported: true, last_run: '2026-10-22 01:00:00', lastest_time: '2026-10-22 00:00:00', timezone: 'UTC' }], total: 1 });
      if (p === '/api/v1/sqlopt/optimize' && req.method === 'POST') { st.jobPolls = 0; return send(res, 200, { data: { id: 'job-1', status: 'queued', items: [] } }); }
      if (p === '/api/v1/sqlopt/analyze' && req.method === 'POST') { st.jobPolls = 0; return send(res, 200, { data: { id: 'job-2', status: 'queued', items: [] } }); }
      if ((m = p.match(/^\/api\/v1\/sqlopt\/jobs\/([^/]+)$/))) {
        st.jobPolls++;
        const done = st.jobPolls >= 2;
        if (m[1] === 'job-2') {
          return send(res, 200, { data: { id: 'job-2', status: done ? 'done' : 'running', total: 2, done: 2, failed: 0, summary: { total: 2, unique: 2 }, items: done ? [
            { index: 0, sql: 'select * from a', status: 'done', level: 'high', findings: 2, score: 60, type: 'select' },
            { index: 1, sql: 'select 1', status: 'done', level: 'info', findings: 0, score: 100, type: 'select' },
          ] : [] } });
        }
        return send(res, 200, { data: { id: m[1], status: done ? 'done' : 'running', items: [{ index: 0, status: done ? 'done' : 'waiting' }] } });
      }
      if ((m = p.match(/^\/api\/v1\/sqlopt\/jobs\/([^/]+)\/items\/(\d+)$/))) {
        if (m[1] === 'job-2') return send(res, 200, { data: { index: Number(m[2]), status: 'done', full_sql: 'select * from a', report: { type: 'select', score: 60, findings: [{ level: 'high', title: '禁止 select *' }] } } });
        return send(res, 200, { data: { index: 0, status: 'done', result: { tool: 'soar', output: '# SOAR 报告\n- 建议加索引' } } });
      }
      if (p === '/api/v1/sqlworkflow/check' && req.method === 'POST') {
        if (st.failCheck) return send(res, st.failCheck.status, { code: st.failCheck.code, message: st.failCheck.message }); // 检查接口出错
        if (String(body.sql).includes('hangcheck')) return req.socket.destroy(); // 请求收到了，但连接被断开
        if (body.export) {
          const reply = () => { const [status, payload] = exportCheck(st, body); send(res, status, payload); };
          return String(body.sql).includes('slowcheck') ? setTimeout(reply, 1500) : reply(); // slowcheck：统计行数很慢
        }
        const sql = String(body.sql);
        const mk = (i, level, msg) => ({ id: i, stage: 'CHECKED', errlevel: level, stagestatus: level ? 'Audit failed' : 'Audit completed', errormessage: msg || 'None', sql: `${sql.slice(0, 20)}#${i}`, affected_rows: 0 });
        let rows = [mk(1, 0)];
        let errors = 0;
        let warnings = 0;
        if (sql.includes('bad')) { rows = [mk(1, 2, '列 x 不存在')]; errors = 1; }
        if (sql.includes('warn')) { rows = [mk(1, '1', '建议加注释')]; warnings = 1; }
        if (sql.includes('many')) { rows = Array.from({ length: 120 }, (_, i) => mk(i + 1, i === 99 ? 2 : 0, i === 99 ? '第 100 条出错' : '')); errors = 1; }
        if (sql.includes('errwarn')) { rows = [mk(1, '1', '建议加注释'), mk(2, 2, '列 x 不存在')]; errors = 1; warnings = 1; } // 一条警告 + 一条错误
        if (sql.includes('nomsg')) { rows = [mk(1, 2)]; errors = 1; } // 出错了但没有写原因（errormessage 是 None）
        if (sql.includes('duprows')) { rows = [mk(1, 2, '列 x 不存在'), mk(1, 2, '列 x 不存在')]; errors = 2; } // 两行一模一样的错误
        if (sql.includes('longsql')) { rows = [{ ...mk(1, 2, '语法错误'), sql: `${sql}\n   ${'x'.repeat(200)}` }]; errors = 1; } // 语句很长，还带换行和多余的空白
        const data = { rows, error_count: errors, warning_count: warnings, syntax_type: 1, syntax_label: 'DDL', source: 'goinception', notice: '', instances: null };
        if (sql.includes('longnotice')) data.notice = `提示：${'x'.repeat(500)}`; // 很长的提示
        if (sql.includes('nativecheck')) { // 没配置 goInception 时的基础检查：来源是 native，带一条提示
          Object.assign(data, { source: 'native', notice: '未配置 goInception：当前只做了基础检查，不校验语法、表结构与影响行数，不支持回滚备份；语句的正确性请自行保证。' });
        }
        if (body.instance_ids) {
          data.instances = body.instance_ids.map((id, i) => ({ id, name: (INSTANCES.find((x) => x.id === id) || {}).name, ok: !(sql.includes('down') && i === 1), error: sql.includes('down') && i === 1 ? '连接失败：i/o timeout' : undefined }));
        }
        return sql.includes('slowcheck') ? setTimeout(() => send(res, 200, { data }), 1500) : send(res, 200, { data }); // slowcheck：检查很慢
      }
      if ((m = p.match(/^\/api\/v1\/sqlworkflow\/(\d+)\/(pass|execute)$/)) && req.method === 'POST') {
        const w = st.wf[m[1]];
        if (!w) return send(res, 404, { code: 'not_found', message: '工单不存在' });
        if (m[2] === 'pass') {
          if (w._rejectPass) return send(res, 403, { code: 'forbidden', message: w._rejectPass });
          if (w.status !== 'workflow_manreviewing') return send(res, 409, { code: 'conflict', message: `工单当前状态为「${w.status_label}」，不能审批` });
          if (w._levels > 1) { // 还有下一级审批
            w._levels--;
            w.current_approvers = [{ username: 'dba', display: '王DBA' }];
          } else {
            Object.assign(w, { status: 'workflow_review_pass', status_label: '审核通过', can_review: false, can_execute: true });
          }
          return send(res, 200, { data: { status: w.status, status_label: w.status_label } });
        }
        if (w._rejectExecute) return send(res, 409, { code: 'conflict', message: w._rejectExecute });
        if (w.status !== 'workflow_review_pass') return send(res, 409, { code: 'conflict', message: `工单当前状态为「${w.status_label}」，只有审核通过或定时执行的工单才能执行` });
        Object.assign(w, { status: 'workflow_queuing', status_label: '排队中', can_execute: false });
        return send(res, 200, { data: { status: w.status, status_label: w.status_label } });
      }
      if ((m = p.match(/^\/api\/v1\/sqlworkflow\/(\d+)\/cancel$/)) && req.method === 'POST') { // 终止工单
        const w = st.wf[m[1]];
        if (!w) return send(res, 404, { code: 'not_found', message: '工单不存在' });
        if (w._hangCancel) return req.socket.destroy(); // 请求收到了，但连接被断开：客户端不知道工单有没有终止
        if (w._rejectCancel) return send(res, 403, { code: 'forbidden', message: w._rejectCancel });
        if (!['workflow_manreviewing', 'workflow_review_pass', 'workflow_timingtask'].includes(w.status)) {
          return send(res, 409, { code: 'conflict', message: `工单当前状态为「${w.status_label}」，不能终止` });
        }
        if (!body || !String(body.remark || '').trim()) return send(res, 400, { code: 'bad_request', message: '终止原因不能为空' });
        if (Array.from(String(body.remark)).length > 500) return send(res, 400, { code: 'bad_request', message: '终止原因过长' });
        if (w._weirdCancel) return send(res, 200, { data: { status: 'workflow_manreviewing', status_label: '等待审批' } });
        if (w._emptyCancel) return send(res, 200, { data: {} });
        Object.assign(w, { status: 'workflow_abort', status_label: '人工终止流程', can_cancel: false, can_review: false, can_execute: false });
        return send(res, 200, { data: { status: w.status, status_label: w.status_label } });
      }
      if (p === '/api/v1/sqlworkflow/options' && req.method === 'GET') return send(res, 200, { data: st.options });
      if (p === '/api/v1/sqlworkflow' && req.method === 'POST') {
        const sql = String(body.sql);
        if (st.rejectCreateOnce) { st.rejectCreateOnce = false; return send(res, 401, { code: 'unauthorized', message: '令牌无效或已过期' }); }
        if (sql.includes('hang')) return req.socket.destroy(); // 请求收到了，但连接被断开：客户端不知道工单有没有创建
        if (sql.includes('servererr')) return send(res, 400, { code: 'bad_request', message: '您提交的是DDL语句，请选择DDL资源组！' });
        const id = st.nextWfId++;
        st.created.push(body);
        let status = 'workflow_manreviewing';
        let message = '工单已正常提交';
        if (sql.includes('autoreject')) { status = 'workflow_autoreviewwrong'; message = '检查结果有错误，系统直接审核不通过，请按提示修改后重新提交'; }
        if (sql.includes('nopass')) { status = 'workflow_review_pass'; message = '无需审批，系统直接审核通过'; }
        const labels = { workflow_manreviewing: '等待审批', workflow_autoreviewwrong: '自动审批不通过', workflow_review_pass: '审核通过' };
        const targets = (body.instance_ids || [body.instance_id]).map((x) => INSTANCES.find((i) => i.id === x).name);
        st.wf[id] = { id, name: body.name, status, status_label: labels[status], is_batch: !!body.instance_ids, batch_targets: body.instance_ids ? targets : [], instance: targets[0], db_name: body.db_name, group: body.group_name,
          is_export: !!body.export, export_format: body.export_format, export_merge: body.export_merge,
          current_approvers: status === 'workflow_manreviewing' ? [{ username: 'ls', display: '李四' }] : [], steps: [], logs: [], review_rows: [], exec_rows: [], can_review: false, can_execute: false };
        return send(res, 200, { data: { workflow_id: id, status, message } });
      }
      if (p === '/api/v1/sqlworkflow' && req.method === 'GET' && /^\d+$/.test(url.searchParams.get('q') || '')) {
        // 搜索词是数字：按工单号找，同时混进一行名字里带这个数字的别的工单（MCP 要挑出工号相同的那一行）
        const qv = url.searchParams.get('q');
        const w = st.wf[qv];
        const rows = [];
        if (w && !w._noListRow) {
          rows.push({ id: Number(qv) + 3000, workflow_name: `名字里带 ${qv}`, engineer: 'zzz', engineer_display: '某人' });
          rows.push({ id: w.id, workflow_name: w.name, engineer: w._engineer, engineer_display: w.engineer, status: w.status });
        }
        return send(res, 200, { data: rows, total: rows.length });
      }
      if (p === '/api/v1/sqlworkflow' && req.method === 'GET') {
        const page = Number(url.searchParams.get('page') || '1');
        const all = Array.from({ length: 45 }, (_, i) => i + 1).map((id) => ({
          id, workflow_name: `工单${id}`, engineer_display: '张三', engineer: 'zs', status: 'workflow_manreviewing', status_label: '待审核', create_time: '2026-10-22T10:00:00+08:00',
          instance_name: id % 5 === 0 ? 'ar001 等 3 个实例' : 'test-mysql-01', db_name: 'shop', group_name: '生产DDL上线', syntax_type: 1, syntax_label: 'DDL',
          batch_count: id % 5 === 0 ? 3 : undefined, is_export: id === 7, export_format: id === 7 ? 'csv' : undefined, approval_level: '第 1 / 2 级', waiting_for_you: id === 1,
        }));
        const slice = all.slice((page - 1) * 20, page * 20);
        return send(res, 200, { data: slice, total: all.length });
      }
      if ((m = p.match(/^\/api\/v1\/sqlworkflow\/(\d+)$/)) && req.method === 'GET' && st.wf[m[1]]) {
        const pub = Object.fromEntries(Object.entries(st.wf[m[1]]).filter(([k]) => !k.startsWith('_')));
        return send(res, 200, { data: pub });
      }
      if ((m = p.match(/^\/api\/v1\/sqlworkflow\/(\d+)$/)) && req.method === 'GET') {
        const id = Number(m[1]);
        if (id === 999) return send(res, 404, { code: 'not_found', message: '工单不存在' });
        const base = {
          id, name: `工单${id}`, status: 'workflow_finish', status_label: '执行结束', instance: 'test-mysql-01', db_name: 'shop', group: '生产DDL上线', env: 'prod', env_label: '生产', env_prod: true,
          engineer: '张三', create_time: '2026-10-22 10:00:00', finish_time: '2026-10-22 11:00:00', syntax_label: 'DDL', is_backup: true, flow: '研发组 → DBA',
          steps: [{ title: '提交', state: 'done', who: '张三', time: '2026-10-22T10:00:00+08:00' }, { title: '审批 · DBA', state: 'done', who: '李四', remark: '同意' }],
          nodes: [], current_approvers: [{ username: 'ls', display: '李四' }],
          logs: [{ operation: '提交工单', info: '', operator: '张三', time: '2026-10-22 10:00:00' }, { operation: '审核通过', info: '同意', operator: '李四', time: '2026-10-22 10:30:00' }, { operation: '执行结束', info: '', operator: '系统', time: '2026-10-22 11:00:00' }],
          review_rows: [{ id: 1, errlevel: 0, stagestatus: 'Audit completed', errormessage: 'None', sql: 'alter table t add c int', affected_rows: 0 }],
          exec_rows: [{ id: 1, errlevel: 0, stagestatus: 'Execute Successfully', errormessage: 'None', sql: 'alter table t add c int', affected_rows: 0, execute_time: '0.05' }],
          is_batch: false, is_export: false,
        };
        if (id === 102) { // 批量工单：ar001 成功，ar002 失败，ar003 已关闭
          Object.assign(base, {
            instance: 'ar001', is_batch: true, status: 'workflow_exception', status_label: '执行有异常',
            batch_targets: ['ar001', 'ar002', 'ar003'],
            batch_instances: [
              { id: 11, name: 'ar001', success: true, failed: false, closed: false, db_type: 'mssql', address: '10.0.0.1:1433', elapsed: 0.5, stmts: 1, note: '影响 0 行' },
              { id: 12, name: 'ar002', success: false, failed: true, closed: false, db_type: 'mssql', address: '10.0.0.2:1433', elapsed: 0.1, stmts: 1, note: '对象名无效' },
              { id: 13, name: 'ar003', success: false, failed: false, closed: true, db_type: 'mssql', address: '10.0.0.3:1433', elapsed: 0, stmts: 0 },
            ],
            exec_rows: [
              { id: 1, errlevel: 0, stagestatus: 'Execute Successfully', sql: 'alter table t add c int', instance_id: 11, instance_name: 'ar001' },
              { id: 2, errlevel: 0, stagestatus: 'Summary', sql: '', instance_id: 11, instance_name: 'ar001', is_summary: true },
              { id: 3, errlevel: 2, stagestatus: 'Execute failed', errormessage: '对象名无效', sql: 'alter table t add c int', instance_id: 12, instance_name: 'ar002' },
            ],
          });
        }
        if (id === 103) Object.assign(base, { is_batch: true, status: 'workflow_manreviewing', status_label: '待审核', exec_rows: [], batch_targets: ['ar001', 'ar002'], batch_instances: [], logs: [] });
        if (id === 104) { // 很多行：检查超长时优先保留出错的行
          base.exec_rows = Array.from({ length: 300 }, (_, i) => ({ id: i + 1, errlevel: i === 250 ? 2 : 0, stagestatus: 'ok', errormessage: i === 250 ? '第 251 条出错' : 'None', sql: `update t set a=${i} where id = ${i}`.padEnd(280, 'x') }));
        }
        return send(res, 200, { data: base });
      }
      if (p === '/api/v1/sqlopt/advise' && req.method === 'POST') {
        return send(res, 200, { data: { type: 'select', fingerprint: 'select ?', tables: ['orders'], score: 80, highest: 'medium', findings: [{ level: 'medium', title: '缺少索引', detail: 'user_id 无索引', suggestion: '建 idx_user' }], index_ddl: ['ALTER TABLE orders ADD INDEX idx_user (user_id)'], plan: { columns: ['id', 'type'], rows: [[1, 'ALL']] } } });
      }
      return send(res, 404, '404 page not found');
    });
  });
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve({ st, server, url: `http://127.0.0.1:${server.address().port}` })));
}

// ── 通过 stdio 驱动 MCP ─────────────────────────────────────────────────
function startMcp(url, extraEnv) {
  const child = spawn(process.execPath, [path.join(__dirname, 'index.js')], {
    env: { ...process.env, DBTOOL_URL: url, DBTOOL_USER: 'tester', DBTOOL_PASS: 'pw-secret-123', DBTOOL_TIMEOUT_MS: '10000', ...extraEnv },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  let acc = '';
  const waiters = new Map();
  child.stdout.setEncoding('utf8');
  child.stdout.on('data', (d) => {
    acc += d;
    let nl;
    while ((nl = acc.indexOf('\n')) >= 0) {
      const line = acc.slice(0, nl);
      acc = acc.slice(nl + 1);
      if (!line.trim()) continue;
      const msg = JSON.parse(line);
      const w = waiters.get(msg.id);
      if (w) { waiters.delete(msg.id); w(msg); }
    }
  });
  let seq = 0;
  const rpc = (method, params) => new Promise((resolve) => {
    const id = ++seq;
    waiters.set(id, resolve);
    child.stdin.write(`${JSON.stringify({ jsonrpc: '2.0', id, method, params })}\n`);
  });
  const call = async (name, args) => {
    const r = await rpc('tools/call', { name, arguments: args || {} });
    const res = r.result || {};
    return { isError: !!res.isError, text: (res.content && res.content[0] && res.content[0].text) || '', raw: r };
  };
  const stop = () => { child.stdin.end(); };
  return { child, rpc, call, stop };
}

const CORE_TOOL_NAMES = [
  'list_instances', 'list_databases', 'list_tables', 'list_columns', 'describe_table', 'sql_query', 'query_log', 'slow_log',
  'slow_log_detail', 'dict_table_info', 'partman_list', 'sql_explain', 'sql_optimize', 'sql_analyze', 'sql_check',
  'submit_sql_workflow', 'submit_export_workflow', 'list_workflows', 'workflow_detail', 'workflow_log', 'cancel_workflow',
];
const MARK = '（经 MCP 操作）';

test('MCP 协议与工具清单', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const init = await m.rpc('initialize', { protocolVersion: '2025-03-26' });
    assert.equal(init.result.protocolVersion, '2025-03-26');
    const init2 = await m.rpc('initialize', { protocolVersion: '1999-01-01' });
    assert.equal(init2.result.protocolVersion, '2024-11-05');
    const list = await m.rpc('tools/list');
    const names = list.result.tools.map((t) => t.name);
    assert.deepEqual(names.slice(0, 21), CORE_TOOL_NAMES, '前 21 个工具名和顺序固定');
    assert.deepEqual(names.slice(21), ['approve_workflow', 'execute_workflow'], '后 2 个是审批 / 执行（高危），排在最后');
    for (const t of list.result.tools) assert.equal(t.inputSchema.type, 'object', t.name);
    for (const n of ['approve_workflow', 'execute_workflow']) {
      const t = list.result.tools.find((x) => x.name === n);
      assert.deepEqual(t.inputSchema.required, ['workflow_id', 'confirm_workflow_id'], `${n} 必须传确认的工单号`);
      assert.match(t.description, /超级管理员/);
      assert.match(t.description, /批量工单/);
    }
    const bad = await m.rpc('no/such/method');
    assert.equal(bad.error.code, -32601);
    assert.equal(st.logins, 0, '只列清单不应登录');
  } finally { m.stop(); server.close(); }
});

test('23 个工具都已对接（没有占位）；批量参数在检查 / 上线 / 导出工具上都有', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const list = await m.rpc('tools/list');
    assert.equal(list.result.tools.length, 23);
    const byName = Object.fromEntries(list.result.tools.map((t) => [t.name, t]));
    for (const t of list.result.tools) {
      assert.ok(!t.description.includes('暂未对接'), `${t.name}：不应该再有占位说明`);
    }
    assert.equal(st.requests.length, 0, '只列清单不访问 dbtool');
    for (const n of ['sql_check', 'submit_sql_workflow', 'submit_export_workflow']) {
      const props = byName[n].inputSchema.properties;
      assert.equal(props.instance_names.type, 'array', `${n} 要能接收多个实例`);
      assert.match(props.instance_names.description, /一个批量工单|【一个】/, n);
      assert.ok(!byName[n].inputSchema.required.includes('instance_name'), `${n} 的 instance_name 不再必填（批量时用 instance_names）`);
    }
    assert.ok(byName.submit_export_workflow.inputSchema.properties.export_merge);
    assert.ok(byName.list_workflows.inputSchema.properties.batch);
  } finally { m.stop(); server.close(); }
});

test('登录、实例解析与基础浏览', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('list_instances', { keyword: 'mssql' });
    assert.match(r.text, /共 5 个实例/);
    assert.match(r.text, /ar001/);
    assert.equal(st.logins, 1);

    const all = await m.call('list_instances', {});
    assert.match(all.text, /\(生产\) 平台Saas \[PolarDB-X\]-tenant/);

    const dbs = await m.call('list_databases', { instance_name: 'test-mysql-01' });
    assert.match(dbs.text, /共 2 个:\nshop\nreport/);
    assert.equal(st.last.url, '/api/v1/query/instances/2/databases');

    const byId = await m.call('list_tables', { instance_name: '11', db_name: 'shop' });
    assert.match(byId.text, /orders/);
    assert.equal(st.last.url, '/api/v1/query/instances/11/databases/shop/tables');

    const ci = await m.call('list_columns', { instance_name: 'AR001', db_name: 'shop', tb_name: 'dbo.orders' });
    assert.match(ci.text, /user_id/);
    assert.equal(st.last.url, '/api/v1/query/instances/11/databases/shop/tables/dbo.orders/columns', '名字大小写不敏感；对象名里的点原样保留');

    const miss = await m.call('list_databases', { instance_name: 'ar00' });
    assert.equal(miss.isError, true);
    assert.match(miss.text, /实例不存在或不可见: ar00/);
    assert.match(miss.text, /ar001 \/ ar002/, '给出相近的实例名');
    assert.equal(st.logins, 1, '一直复用同一个令牌');
  } finally { m.stop(); server.close(); }
});

test('sql_query：请求体、脱敏信息、只读白名单、原因、超长结果', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const ok = await m.call('sql_query', { instance_name: 'ar001', db_name: 'shop', sql_content: '  /* c */ select id, phone from users', limit_num: 50 });
    assert.equal(ok.isError, false);
    assert.deepEqual(st.last.body, { instance_id: 11, db: 'shop', sql: '  /* c */ select id, phone from users', limit: 50 });
    const out = JSON.parse(ok.text);
    assert.equal(out.is_masked, true);
    assert.deepEqual(out.masked_columns, ['phone']);
    assert.deepEqual(out.rows, [[1, '138****0001'], [2, '139****0002']]);
    assert.equal(out.full_sql, '  /* c */ select id, phone from users LIMIT 50');

    const clamp = await m.call('sql_query', { instance_name: 'ar001', db_name: 'shop', sql_content: 'select 1', limit_num: 999999 });
    assert.equal(JSON.parse(clamp.text).effective_limit, 2000, '行数上限钳到 2000');

    st.requests.length = 0;
    const w = await m.call('sql_query', { instance_name: 'ar001', db_name: 'shop', sql_content: 'delete from users' });
    assert.equal(w.isError, true);
    assert.match(w.text, /只放行查询类语句/);
    for (const s of ['update users set a=1', 'insert into t values (1)', 'drop table t', 'call p()', '-- x\nupdate t set a=1']) {
      assert.equal((await m.call('sql_query', { instance_name: 'ar001', db_name: 'shop', sql_content: s })).isError, true, s);
    }
    assert.equal(st.requests.length, 0, '写语句在客户端就被拒，不会发给 dbtool');

    const need = await m.call('sql_query', { instance_name: 'ar001', db_name: 'shop', sql_content: 'select need_reason' });
    assert.equal(need.isError, true);
    assert.match(need.text, /HTTP 403 reason_required/);
    const withReason = await m.call('sql_query', { instance_name: 'ar001', db_name: 'shop', sql_content: 'select need_reason', reason: '排查订单' });
    assert.equal(withReason.isError, false);
    assert.equal(st.last.body.reason, '排查订单');

    const big = await m.call('sql_query', { instance_name: 'ar001', db_name: 'shop', sql_content: 'select big' });
    assert.ok(big.text.length <= 25000, `超长结果被截断（${big.text.length}）`);
    const bigOut = JSON.parse(big.text); // 截断后仍是合法 JSON
    assert.ok(bigOut.rows_omitted > 0);
    assert.equal(bigOut.rows.length + bigOut.rows_omitted, 4000);
    assert.match(bigOut.note, /只列出前/);
  } finally { m.stop(); server.close(); }
});

test('令牌：401 重登一次重试；快到期主动续登；登录失败不泄露密码', async () => {
  const a = await startStub();
  const m1 = startMcp(a.url);
  try {
    await m1.call('list_instances', {});
    assert.equal(a.st.logins, 1);
    a.st.rejectNext = 1; // 下一次业务请求返回 401
    const r = await m1.call('list_databases', { instance_name: 'ar001' });
    assert.equal(r.isError, false, r.text);
    assert.equal(a.st.logins, 2, '401 之后重新登录一次');
  } finally { m1.stop(); a.server.close(); }

  const b = await startStub();
  b.st.tokenTtl = 30; // 令牌 30 秒后到期，不足 60 秒的余量，每次调用前都会续登
  const m2 = startMcp(b.url);
  try {
    await m2.call('list_instances', {});
    await m2.call('list_databases', { instance_name: 'ar001' });
    assert.ok(b.st.logins >= 2, `快到期的令牌主动续登（登录 ${b.st.logins} 次）`);
  } finally { m2.stop(); b.server.close(); }

  const c = await startStub();
  const m3 = startMcp(c.url, { DBTOOL_PASS: 'wrong-pass-999' });
  try {
    const r = await m3.call('list_instances', {});
    assert.equal(r.isError, true);
    assert.match(r.text, /登录失败/);
    assert.match(r.text, /bad_credentials/);
    assert.ok(!r.text.includes('wrong-pass-999'), `错误信息里不能出现密码：${r.text}`);
  } finally { m3.stop(); c.server.close(); }

  const d = await startStub();
  const m4 = startMcp(d.url, { DBTOOL_USER: '', DBTOOL_PASS: '' });
  try {
    const r = await m4.call('list_instances', {});
    assert.equal(r.isError, true);
    assert.match(r.text, /缺少 DBTOOL_USER \/ DBTOOL_PASS/);
  } finally { m4.stop(); d.server.close(); }
});

test('describe_table / dict_table_info：对象详情渲染', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const ddl = await m.call('describe_table', { instance_name: 'test-mysql-01', db_name: 'shop', tb_name: 'orders' });
    assert.equal(ddl.isError, false, ddl.text);
    assert.match(st.last.url, /object-info\?kind=table&name=orders$/);
    assert.match(ddl.text, /CREATE TABLE `orders`/);
    assert.match(ddl.text, /## 列信息/);
    assert.ok(!ddl.text.includes('索引信息'), 'describe_table 只要列信息和创建语句');
    assert.ok(!ddl.text.includes('估算行数'));

    const full = await m.call('dict_table_info', { instance_name: 'test-mysql-01', db_name: 'shop', tb_name: 'orders', db_type: 'mysql', kind: 'view', schema_name: 'dbo' });
    assert.match(full.text, /## 基本信息/);
    assert.match(full.text, /引擎: InnoDB/);
    assert.match(full.text, /## 索引信息/);
    assert.match(full.text, /命中脱敏规则的列: phone/);
    assert.match(full.text, /注: 行数是估算/);
    assert.match(st.last.url, /kind=view&name=orders&schema=dbo$/);

    const gone = await m.call('describe_table', { instance_name: 'test-mysql-01', db_name: 'shop', tb_name: 'nope' });
    assert.equal(gone.isError, true);
    assert.match(gone.text, /接口不存在（HTTP 404）/);
  } finally { m.stop(); server.close(); }
});

test('query_log / slow_log / slow_log_detail / partman_list', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const lg = await m.call('query_log', { limit: 5, search: 'orders', instance_name: 'ar001', start: '2026-10-01' });
    assert.equal(lg.isError, false, lg.text);
    assert.match(st.last.url, /^\/api\/v1\/query\/history\?limit=5&offset=0&q=orders&start=2026-10-01&instance=ar001$/);
    const lgOut = JSON.parse(lg.text);
    assert.equal(lgOut.total, 1);
    assert.ok(lgOut.items[0].sql.length <= 601, 'SQL 文本被截短');

    const sl = await m.call('slow_log', { instance_id: 1, start_date: '2026-10-01', end_date: '2026-10-22', search: 'orders', limit: 10 });
    assert.equal(sl.isError, false, sl.text);
    assert.match(st.last.url, /^\/api\/v1\/slowlog\/slowlogs\?instance_id=1&q=orders&start=2026-10-01&end=2026-10-22&limit=10&offset=0$/);
    assert.equal(JSON.parse(sl.text).rows[0].sql_md5, 'abc');

    const sd = await m.call('slow_log_detail', { instance_id: 1, db_name: 'shop', sql_md5: 'abc', exec_date: '2026-10-21' });
    assert.equal(sd.isError, false, sd.text);
    assert.match(st.last.url, /^\/api\/v1\/slowlog\/slowlogs\/detail\?instance_id=1&db_name=shop&sql_md5=abc&exec_date=2026-10-21$/);
    assert.equal(JSON.parse(sd.text).total, 1);
    assert.equal((await m.call('slow_log_detail', { instance_id: 1 })).isError, true, '缺 sql_md5');

    const pm = await m.call('partman_list', { search: 'orders' });
    assert.equal(pm.isError, false, pm.text);
    assert.match(st.last.url, /^\/api\/v1\/partman\/configs\?search=orders&limit=50&offset=0$/);
    const pmOut = JSON.parse(pm.text);
    assert.equal(pmOut.total, 1);
    assert.equal(pmOut.rows[0].tbname, 'orders');
    assert.equal(pmOut.rows[0].timezone, undefined, '只保留约定的字段');
  } finally { m.stop(); server.close(); }
});

test('sql_explain / sql_optimize / sql_analyze', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { /* 任务轮询间隔是常量 1.5 秒，测试里只轮询两次 */ });
  try {
    const ex = await m.call('sql_explain', { instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'select id from orders where user_id = 7' });
    assert.equal(ex.isError, false, ex.text);
    assert.equal(st.last.body.sql, 'explain select id from orders where user_id = 7', '自动补 explain 前缀');
    const exOut = JSON.parse(ex.text);
    assert.equal(exOut.plan_tips[0].text, '全表扫描');
    const ex2 = await m.call('sql_explain', { instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'EXPLAIN select 1' });
    assert.equal(st.last.body.sql, 'EXPLAIN select 1', '已有 explain 前缀不重复添加');
    assert.equal(ex2.isError, false);
    const exDml = await m.call('sql_explain', { instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'delete from t where id = 1' });
    assert.equal(exDml.isError, false, 'EXPLAIN 不执行语句，可以用来看 DML 的执行计划');
    assert.equal(st.last.body.sql, 'explain delete from t where id = 1');

    const soar = await m.call('sql_optimize', { engine: 'soar', instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'select id from orders' });
    assert.equal(soar.isError, false, soar.text);
    assert.match(soar.text, /SOAR 报告/);
    assert.match(soar.text, /建议加索引/);
    assert.deepEqual(st.byPath['POST /api/v1/sqlopt/optimize'], { tool: 'soar', instance_id: 2, db: 'shop', sql: 'select id from orders' });

    const tuning = await m.call('sql_optimize', { engine: 'sqltuning', instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'select id from orders' });
    assert.equal(tuning.isError, false, tuning.text);
    assert.deepEqual(st.byPath['POST /api/v1/sqlopt/optimize'], { tool: 'sqltuning', instance_id: 2, db: 'shop', sql: 'select id from orders', options: ['sys_parm', 'sql_plan', 'obj_stat'] });
    await m.call('sql_optimize', { engine: 'sqltuning', instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'select id from orders', options: ['sql_plan'] });
    assert.deepEqual(st.byPath['POST /api/v1/sqlopt/optimize'].options, ['sql_plan'], '可以指定 SQLTuning 的选项');

    st.requests.length = 0;
    const adv = await m.call('sql_optimize', { engine: 'sqladvisor', instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'select id from orders where user_id=1' });
    assert.equal(adv.isError, false, adv.text);
    assert.match(adv.text, /dbtool 没有 SQLAdvisor/);
    assert.match(adv.text, /评分 80/);
    assert.match(adv.text, /缺少索引/);
    assert.match(adv.text, /ADD INDEX idx_user/);
    assert.deepEqual(st.requests.filter((r) => r.startsWith('POST')), ['POST /api/v1/sqlopt/advise']);

    st.requests.length = 0;
    const tuna = await m.call('sql_optimize', { engine: 'sqltuningadvisor', instance_name: 'ar001', db_name: 'shop', sql_content: 'select 1' });
    assert.equal(tuna.isError, true);
    assert.match(tuna.text, /没有 SQL Server Tuning Advisor/);
    assert.equal(st.requests.length, 0);
    assert.equal((await m.call('sql_optimize', { engine: 'nope', instance_name: 'ar001', db_name: 'shop', sql_content: 'select 1' })).isError, true);
    assert.equal((await m.call('sql_optimize', { instance_name: 'ar001', db_name: 'shop', sql_content: 'update t set a=1' })).isError, true);

    const an = await m.call('sql_analyze', { text: 'select * from a;\nselect 1;' });
    assert.equal(an.isError, false, an.text);
    const anOut = JSON.parse(an.text);
    assert.equal(anOut.status, 'done');
    assert.equal(anOut.items.length, 2);
    assert.equal(anOut.details.length, 2);
    assert.equal(anOut.details[0].report.findings[0].title, '禁止 select *');
    assert.equal((await m.call('sql_analyze', { text: 'select 1', instance_name: 'ar001' })).isError, true, '带实例必须同时给库');
  } finally { m.stop(); server.close(); }
});

// ── 工单组（只读）：sql_check / list_workflows / workflow_detail / workflow_log，支持批量工单 ──

test('sql_check：单实例', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const ok = await m.call('sql_check', { instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'alter table t add c int' });
    assert.equal(ok.isError, false, ok.text);
    assert.deepEqual(st.byPath['POST /api/v1/sqlworkflow/check'], { instance_id: 2, db_name: 'shop', sql: 'alter table t add c int' });
    const out = JSON.parse(ok.text);
    assert.equal(out.verdict, '通过');
    assert.equal(out.mode, '单实例');
    assert.equal(out.error_count, 0);
    assert.equal(out.rows[0].level, 'ok');
    assert.equal(out.instance_checks, undefined, '单实例没有逐实例预检');

    const bad = JSON.parse((await m.call('sql_check', { instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'alter table bad' })).text);
    assert.equal(bad.verdict, '不通过');
    assert.equal(bad.rows[0].level, 'error');
    assert.equal(bad.rows[0].message, '列 x 不存在');

    const warn = JSON.parse((await m.call('sql_check', { instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'alter table warn' })).text);
    assert.equal(warn.verdict, '有警告');
    assert.equal(warn.rows[0].level, 'warning', 'errlevel 是字符串 "1" 也认');

    assert.equal((await m.call('sql_check', { db_name: 'shop', sql_content: 'x' })).isError, true, '没有实例');
    assert.equal((await m.call('sql_check', { instance_name: 'test-mysql-01', sql_content: 'x' })).isError, true, '缺库名');
    // 去重后只剩一个实例：按单实例检查；instance_names 优先于 instance_name
    const dup = JSON.parse((await m.call('sql_check', { instance_name: 'test-mysql-01', instance_names: ['ar001', 'AR001'], db_name: 'shop', sql_content: 'alter table t add c int' })).text);
    assert.equal(dup.mode, '单实例');
    assert.deepEqual(st.byPath['POST /api/v1/sqlworkflow/check'], { instance_id: 11, db_name: 'shop', sql: 'alter table t add c int' });
  } finally { m.stop(); server.close(); }
});

test('sql_check：批量（SQL Server）一次请求带全部实例，不逐个实例检查', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('sql_check', { instance_names: ['ar001', 'ar002'], db_name: 'shop', sql_content: 'alter table t add c int' });
    assert.equal(r.isError, false, r.text);
    assert.deepEqual(st.byPath['POST /api/v1/sqlworkflow/check'], { instance_ids: [11, 12], db_name: 'shop', sql: 'alter table t add c int' });
    assert.equal(st.requests.filter((x) => x.startsWith('POST /api/v1/sqlworkflow/check')).length, 1, '只发一次检查请求');
    const out = JSON.parse(r.text);
    assert.equal(out.verdict, '通过');
    assert.match(out.mode, /批量（2 个实例，提交时是一个批量工单）/);
    assert.deepEqual(out.instances, ['ar001', 'ar002']);
    assert.match(out.batch_note, /只在第一个实例（ar001）上检查/);
    assert.deepEqual(out.instance_checks.map((c) => c.ok), [true, true]);

    // 某个实例连不上：整体不通过，并指出是哪个
    const down = JSON.parse((await m.call('sql_check', { instance_names: 'ar001, ar002', db_name: 'shop', sql_content: 'alter table down' })).text);
    assert.equal(down.verdict, '不通过', '预检有实例连不上就不通过（逗号分隔的字符串也能用）');
    assert.deepEqual(down.instance_checks.filter((c) => !c.ok).map((c) => c.name), ['ar002']);
    assert.match(down.instance_checks[1].error, /i\/o timeout/);
    assert.deepEqual(st.byPath['POST /api/v1/sqlworkflow/check'].instance_ids, [11, 12]);

    const unknown = await m.call('sql_check', { instance_names: ['ar001', 'ar999'], db_name: 'shop', sql_content: 'x' });
    assert.equal(unknown.isError, true);
    assert.match(unknown.text, /实例不存在或不可见: ar999/);
  } finally { m.stop(); server.close(); }
});

test('sql_check：结果行很多时优先保留出错的行', async () => {
  const { server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('sql_check', { instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'alter many' });
    assert.ok(r.text.length <= 25000, `长度 ${r.text.length}`);
    const out = JSON.parse(r.text);
    assert.equal(out.verdict, '不通过');
    assert.equal(out.rows.length, 50);
    assert.equal(out.rows_omitted, 70);
    assert.ok(out.rows.some((x) => x.message === '第 100 条出错'), '第 100 条出错的行不能被截掉');
  } finally { m.stop(); server.close(); }
});

test('list_workflows：筛选参数、翻页、批量工单', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_PUBLIC_URL: 'https://dbtool.example.com/' });
  try {
    const r = await m.call('list_workflows', { batch: '1', status: 'workflow_manreviewing', search: '订单', instance_id: 11, start_date: '2026-10-01' });
    assert.equal(r.isError, false, r.text);
    const u = new URL(`http://x${st.last.url}`);
    assert.equal(u.pathname, '/api/v1/sqlworkflow');
    assert.equal(u.searchParams.get('kind'), 'change', 'kind=sql 对应 dbtool 的 change');
    assert.equal(u.searchParams.get('batch'), '1');
    assert.equal(u.searchParams.get('status'), 'workflow_manreviewing');
    assert.equal(u.searchParams.get('q'), '订单');
    assert.equal(u.searchParams.get('instance_id'), '11');
    assert.equal(u.searchParams.get('start_date'), '2026-10-01');
    assert.match(u.searchParams.get('end_date'), /^\d{4}-\d{2}-\d{2}$/, '只给起始日期时自动补结束日期');
    const out = JSON.parse(r.text);
    assert.equal(out.total, 45);
    assert.equal(out.returned, 20, '默认 20 条，一页');
    const batchRow = out.rows.find((x) => x.id === 5);
    assert.equal(batchRow.batch_count, 3);
    assert.equal(batchRow.instance, 'ar001 等 3 个实例');
    assert.equal(batchRow.detail_url, 'https://dbtool.example.com/sqlworkflow/5');
    assert.equal(out.rows.find((x) => x.id === 1).waiting_for_you, true);
    assert.equal(out.rows.find((x) => x.id === 7).kind, 'export');

    await m.call('list_workflows', { end_date: '2026-10-22' });
    const onlyEnd = new URL(`http://x${st.last.url}`).searchParams;
    assert.equal(onlyEnd.get('end_date'), '2026-10-22');
    assert.equal(onlyEnd.get('start_date'), '2000-01-01', '只给结束日期时自动补起始日期');
    await m.call('list_workflows', {});
    assert.equal(new URL(`http://x${st.last.url}`).searchParams.get('start_date'), null, '都不给就不按日期过滤');

    // navStatus 是 status 的别名；all = 不过滤；kind=export / all
    await m.call('list_workflows', { navStatus: 'workflow_finish', kind: 'export', batch: '0' });
    let q = new URL(`http://x${st.last.url}`).searchParams;
    assert.equal(q.get('status'), 'workflow_finish');
    assert.equal(q.get('kind'), 'export');
    assert.equal(q.get('batch'), '0');
    await m.call('list_workflows', { navStatus: 'all', kind: 'all', tab: 'approve' });
    q = new URL(`http://x${st.last.url}`).searchParams;
    assert.equal(q.get('status'), null);
    assert.equal(q.get('kind'), null);
    assert.equal(q.get('tab'), 'approve');
    assert.equal(q.get('batch'), null);

    // 每页固定 20：limit 30 连翻两页；offset 25 从第 2 页第 6 条起
    st.requests.length = 0;
    const two = JSON.parse((await m.call('list_workflows', { limit: 30 })).text);
    assert.equal(two.returned, 30);
    assert.deepEqual(two.rows.map((x) => x.id).slice(18, 22), [19, 20, 21, 22]);
    assert.deepEqual(st.requests.filter((x) => x.startsWith('GET /api/v1/sqlworkflow?')).map((x) => new URL(`http://x${x.slice(4)}`).searchParams.get('page')), ['1', '2']);
    const off = JSON.parse((await m.call('list_workflows', { limit: 10, offset: 25 })).text);
    assert.deepEqual(off.rows.map((x) => x.id), [26, 27, 28, 29, 30, 31, 32, 33, 34, 35]);
    const all = JSON.parse((await m.call('list_workflows', { limit: 500 })).text);
    assert.equal(all.returned, 45, '最多 100 条，这里只有 45 条');
  } finally { m.stop(); server.close(); }
});

test('workflow_detail：单实例与批量工单', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_PUBLIC_URL: 'https://dbtool.example.com' });
  try {
    const single = JSON.parse((await m.call('workflow_detail', { workflow_id: 101 })).text);
    assert.equal(st.last.url, '/api/v1/sqlworkflow/101');
    assert.equal(single.detail_url, 'https://dbtool.example.com/sqlworkflow/101');
    assert.equal(single.status_label, '执行结束');
    assert.equal(single.env, '生产');
    assert.equal(single.batch, undefined);
    assert.equal(single.steps.length, 2);
    assert.deepEqual(single.current_approvers, ['李四']);
    assert.equal(single.exec_rows[0].status, 'Execute Successfully');

    const batch = JSON.parse((await m.call('workflow_detail', { workflow_id: 102 })).text);
    assert.equal(batch.status_label, '执行有异常');
    assert.deepEqual({ total: batch.batch.total, success: batch.batch.success, failed: batch.batch.failed, closed: batch.batch.closed, pending: batch.batch.pending }, { total: 3, success: 1, failed: 1, closed: 1, pending: 0 });
    assert.deepEqual(batch.batch.instances.map((i) => `${i.name}:${i.result}`), ['ar001:success', 'ar002:failed', 'ar003:closed']);
    assert.equal(batch.batch.instances[1].note, '对象名无效');
    assert.match(batch.batch.note, /只来自第一个实例/);
    assert.ok(batch.exec_rows.some((r) => r.summary && r.instance === 'ar001'));
    assert.ok(batch.exec_rows.some((r) => r.level === 'error' && r.instance === 'ar002'));

    // 还没执行的批量工单：只有计划上线的实例名
    const pend = JSON.parse((await m.call('workflow_detail', { workflow_id: 103 })).text);
    assert.deepEqual(pend.batch.instances, [{ name: 'ar001', result: 'pending' }, { name: 'ar002', result: 'pending' }]);
    assert.deepEqual({ total: pend.batch.total, success: pend.batch.success, pending: pend.batch.pending }, { total: 2, success: 0, pending: 2 }, '还没执行：全部算待执行');

    // 结果行很多：超长时自动减行，优先保留出错的行；max_rows 可以调小
    const many = await m.call('workflow_detail', { workflow_id: 104 });
    assert.ok(many.text.length <= 25000);
    const manyOut = JSON.parse(many.text);
    assert.ok(manyOut.exec_rows.some((r) => r.message === '第 251 条出错'));
    assert.ok(manyOut.exec_rows_omitted > 0);
    const few = JSON.parse((await m.call('workflow_detail', { workflow_id: 104, max_rows: 2 })).text);
    assert.equal(few.exec_rows.length, 2);
    assert.ok(few.exec_rows.some((r) => r.message === '第 251 条出错'));

    for (const bad of [0, -1, 'abc', 1.5, null]) {
      const r = await m.call('workflow_detail', { workflow_id: bad });
      assert.equal(r.isError, true, String(bad));
    }
    const missing = await m.call('workflow_detail', { workflow_id: 999 });
    assert.equal(missing.isError, true);
    assert.match(missing.text, /HTTP 404 not_found.*工单不存在/);
  } finally { m.stop(); server.close(); }
});

test('workflow_log：操作日志', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_PUBLIC_URL: 'https://dbtool.example.com' });
  try {
    const r = await m.call('workflow_log', { workflow_id: 101 });
    assert.equal(r.isError, false, r.text);
    assert.equal(st.last.url, '/api/v1/sqlworkflow/101');
    assert.match(r.text, /工单 #101 工单101（执行结束）/);
    assert.match(r.text, /工单链接: https:\/\/dbtool\.example\.com\/sqlworkflow\/101/);
    assert.match(r.text, /2026-10-22 10:30:00  李四  审核通过：同意/);
    assert.match(r.text, /2026-10-22 10:00:00  张三  提交工单\n/);

    const b = await m.call('workflow_log', { workflow_id: 102 });
    assert.match(b.text, /批量 3 个实例/);
    const none = await m.call('workflow_log', { workflow_id: 103 });
    assert.match(none.text, /\(没有操作日志\)/);
    assert.equal((await m.call('workflow_log', { workflow_id: 'x' })).isError, true);
  } finally { m.stop(); server.close(); }
});

test('只读工单工具不会发出任何会改动工单的请求', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    await m.call('sql_check', { instance_names: ['ar001', 'ar002'], db_name: 'shop', sql_content: 'alter table t add c int' });
    await m.call('sql_check', { instance_name: 'ar001', db_name: 'shop', sql_content: 'select 1', export: true });
    await m.call('sql_check', { instance_names: ['ar001', 'ar002'], db_name: 'shop', sql_content: 'select 1', export: true, export_merge: true });
    await m.call('list_workflows', {});
    await m.call('workflow_detail', { workflow_id: 102 });
    await m.call('workflow_log', { workflow_id: 102 });
    const writes = st.requests.filter((r) => /^(POST|PUT|PATCH|DELETE) /.test(r) && !r.startsWith('POST /api/v1/auth/login') && !r.startsWith('POST /api/v1/sqlworkflow/check'));
    assert.deepEqual(writes, [], '除了登录和“检查”（只读），不能有别的写请求');
    assert.ok(!st.requests.some((r) => /\/(pass|cancel|execute|schedule|run-date|osc|retry|close)\b/.test(r)));
  } finally { m.stop(); server.close(); }
});

// ── 批量审核通过 / 批量执行（仅超级管理员） ──────────────────────────────

const posts = (st, re) => st.requests.filter((r) => re.test(r));

test('approve_workflow / execute_workflow：工单号和确认不对，连登录都不发', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_USER: 'root' });
  try {
    for (const tool of ['approve_workflow', 'execute_workflow']) {
      for (const args of [
        { workflow_id: 201 },
        { workflow_id: 201, confirm_workflow_id: '' },
        { workflow_id: 201, confirm_workflow_id: 202 },
        { workflow_id: 0, confirm_workflow_id: 0 },
        { workflow_id: 'abc', confirm_workflow_id: 'abc' },
        { workflow_id: 1.5, confirm_workflow_id: 1.5 },
      ]) {
        const r = await m.call(tool, args);
        assert.equal(r.isError, true, `${tool} ${JSON.stringify(args)}`);
      }
      assert.match((await m.call(tool, { workflow_id: 201 })).text, /缺少 confirm_workflow_id/);
      assert.match((await m.call(tool, { workflow_id: 201, confirm_workflow_id: 202 })).text, /不一致，已拒绝/);
    }
    assert.equal(st.requests.length, 0, '本地校验失败时不能发出任何请求，包括登录');
  } finally { m.stop(); server.close(); }
});

test('approve_workflow / execute_workflow：非超级管理员账号被拒，只发了登录请求', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url); // 账号 tester 不是超级管理员
  try {
    for (const [tool, what] of [['approve_workflow', '批量审核通过'], ['execute_workflow', '批量执行']]) {
      st.requests.length = 0;
      const r = await m.call(tool, { workflow_id: 201, confirm_workflow_id: 201 });
      assert.equal(r.isError, true, tool);
      assert.match(r.text, new RegExp(`${what}只接受超级管理员账号：当前账号 tester 不是超级管理员`));
      assert.deepEqual(st.requests, ['POST /api/v1/auth/login'], '只有一次为了核对身份的登录，没有碰任何工单');
    }
  } finally { m.stop(); server.close(); }
});

test('超级管理员标记每次重新登录核对：账号被降级后，旧令牌不算数', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_USER: 'root' });
  try {
    const ok = await m.call('approve_workflow', { workflow_id: 201, confirm_workflow_id: 201 });
    assert.equal(ok.isError, false, ok.text);
    assert.equal(st.logins, 1);
    st.users.root.su = false; // dbtool 里把 root 降级了，但 MCP 手里还有一个没过期的旧令牌
    st.requests.length = 0;
    const r = await m.call('approve_workflow', { workflow_id: 210, confirm_workflow_id: 210 });
    assert.equal(r.isError, true);
    assert.match(r.text, /不是超级管理员（已重新登录核对）/);
    assert.equal(st.logins, 2, '每次调用都重新登录');
    assert.deepEqual(st.requests, ['POST /api/v1/auth/login']);
    st.users.root.su = true;
    st.requests.length = 0;
    await m.call('execute_workflow', { workflow_id: 205, confirm_workflow_id: 205 });
    assert.equal(st.logins, 3, 'execute 也每次重新登录');
  } finally { m.stop(); server.close(); }
});

test('approve_workflow：只接受批量工单、等待审批的工单、别人提交的工单、有审批权限的工单', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_USER: 'root' });
  try {
    const call = (id) => m.call('approve_workflow', { workflow_id: id, confirm_workflow_id: id });
    assert.match((await call(203)).text, /批量审核通过只接受批量工单：工单 #203「批量工单203」是单实例工单/);
    assert.match((await call(204)).text, /当前状态是「已正常结束」，只有「等待审批」的工单才能审核通过/);
    assert.match((await call(202)).text, /工单 #202 是当前账号（root）自己提交的，审核通过一律拒绝（超级管理员也一样）/);
    assert.match((await call(212)).text, /自己提交的，审核通过一律拒绝/, '登录名大小写不同也算自己');
    assert.match((await call(207)).text, /没有审批权限.*当前审批人：李四/);
    assert.match((await call(211)).text, /无法确认工单 #211 的提交人，已拒绝审核通过/, '查不到提交人时一律拒绝，不能放行');
    const missing = await call(999);
    assert.equal(missing.isError, true);
    assert.match(missing.text, /HTTP 404 not_found/);
    assert.deepEqual(posts(st, /\/pass$/), [], '以上任何一种都不能发出审批请求');
    assert.equal(st.wf[202].status, 'workflow_manreviewing', '工单状态没有变');
  } finally { m.stop(); server.close(); }
});

test('approve_workflow：审核通过的请求、备注标记、多级审批、导出原值提示', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_USER: 'root' });
  try {
    const ok = await m.call('approve_workflow', { workflow_id: 201, confirm_workflow_id: 201, remark: '同意上线' });
    assert.equal(ok.isError, false, ok.text);
    assert.deepEqual(st.byPath['POST /api/v1/sqlworkflow/201/pass'], { remark: '同意上线（经 MCP 操作）' });
    assert.match(ok.text, /✅ 工单 #201「批量工单201」本级已审核通过（批量工单，3 个实例）/);
    assert.match(ok.text, /状态: 审核通过（审批流已走完，可以执行了）/);
    assert.match(ok.text, /操作账号: root（超级管理员，已重新登录核对）/);
    assert.ok(ok.text.includes(`工单链接（请转给用户核对）: ${url}/sqlworkflow/201`));
    assert.equal(st.wf[201].status, 'workflow_review_pass');
    assert.ok(st.requests.some((r) => r.startsWith('GET /api/v1/sqlworkflow?q=201')), '到列表里按工单号找提交人的登录名');
    assert.equal(posts(st, /\/pass$/).length, 1);

    // 两级审批：第一次通过后还在审批；没有备注时备注就是“经 MCP 操作”
    const first = await m.call('approve_workflow', { workflow_id: 210, confirm_workflow_id: 210 });
    assert.equal(first.isError, false, first.text);
    assert.deepEqual(st.byPath['POST /api/v1/sqlworkflow/210/pass'], { remark: '经 MCP 操作' });
    assert.match(first.text, /状态: 等待审批；还有后续审批，当前审批人：王DBA/);
    const second = await m.call('approve_workflow', { workflow_id: 210, confirm_workflow_id: 210 });
    assert.match(second.text, /审批流已走完/);

    // 已经通过的工单再来一次：MCP 先挡在状态检查上
    const again = await m.call('approve_workflow', { workflow_id: 201, confirm_workflow_id: 201 });
    assert.equal(again.isError, true);
    assert.match(again.text, /当前状态是「审核通过」，只有「等待审批」的工单才能审核通过/);

    const unmasked = await m.call('approve_workflow', { workflow_id: 213, confirm_workflow_id: 213 });
    assert.equal(unmasked.isError, false, unmasked.text);
    assert.match(unmasked.text, /⚠ 这是“导出原值（不脱敏）”的导出工单/);
  } finally { m.stop(); server.close(); }
});

test('approve_workflow：dbtool 服务端拒绝时报错，不能当作成功，也不重试', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_USER: 'root' });
  try {
    const r = await m.call('approve_workflow', { workflow_id: 216, confirm_workflow_id: 216 });
    assert.equal(r.isError, true, r.text);
    assert.match(r.text, /HTTP 403 forbidden.*无权限审核/);
    assert.ok(!r.text.includes('✅'), '不能出现“已审核通过”');
    assert.equal(posts(st, /\/pass$/).length, 1, '只发一次，不重试');
    assert.equal(st.wf[216].status, 'workflow_manreviewing');
  } finally { m.stop(); server.close(); }
});

test('approve_workflow：很长的备注会被截到 500 字以内', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_USER: 'root' });
  try {
    const r = await m.call('approve_workflow', { workflow_id: 201, confirm_workflow_id: 201, remark: 'x'.repeat(900) });
    assert.equal(r.isError, false, r.text);
    const remark = st.byPath['POST /api/v1/sqlworkflow/201/pass'].remark;
    assert.equal(Array.from(remark).length, 500, '备注正好 500 个字符（dbtool 的上限；超过会被服务端拒绝）');
    assert.ok(remark.endsWith(`…${MARK}`), '截的是写的原因，“（经 MCP 操作）”标记留着');
    const emoji = await m.call('approve_workflow', { workflow_id: 210, confirm_workflow_id: 210, remark: '😀'.repeat(700) });
    assert.equal(emoji.isError, false, emoji.text);
    const emojiRemark = st.byPath['POST /api/v1/sqlworkflow/210/pass'].remark;
    assert.ok(Array.from(emojiRemark).length <= 500, '按字符计数，不会把表情符号劈开');
    assert.ok(emojiRemark.endsWith(MARK));
  } finally { m.stop(); server.close(); }
});

test('execute_workflow：只接受批量工单、审核通过的工单、有执行权限的工单', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_USER: 'root' });
  try {
    const call = (id, extra) => m.call('execute_workflow', { workflow_id: id, confirm_workflow_id: id, ...extra });
    assert.match((await call(203)).text, /批量执行只接受批量工单/);
    assert.match((await call(201)).text, /当前状态是「等待审批」，只有「审核通过」的工单才能立即执行/);
    assert.match((await call(214)).text, /当前状态是「定时执行」，只有「审核通过」的工单才能立即执行（定时执行的工单请到网页上处理）/);
    assert.match((await call(206)).text, /没有执行权限/);
    assert.deepEqual(posts(st, /\/execute$/), [], '以上任何一种都不能发出执行请求');

    const ok = await call(205);
    assert.equal(ok.isError, false, ok.text);
    assert.deepEqual(st.byPath['POST /api/v1/sqlworkflow/205/execute'], { mode: 'auto' }, '只用 auto 模式，不传 executor 就用 dbtool 的缺省');
    assert.match(ok.text, /✅ 工单 #205「批量工单205」已提交执行（批量工单，3 个实例）/);
    assert.match(ok.text, /状态: 排队中；执行在后台进行，用 workflow_detail 查看每个实例的结果/);
    assert.match(ok.text, /操作账号: root（超级管理员，已重新登录核对）/);
    assert.equal(st.wf[205].status, 'workflow_queuing');

    // 执行没有“提交人不能是自己”的限制（只对审核通过有）；executor 可以指定
    const own = await call(209, { executor: 'native' });
    assert.equal(own.isError, false, own.text);
    assert.deepEqual(st.byPath['POST /api/v1/sqlworkflow/209/execute'], { mode: 'auto', executor: 'native' });

    // dbtool 服务端拒绝（比如不在可执行时间窗）：原样转出，只发一次，不重试
    st.requests.length = 0;
    const refused = await call(215);
    assert.equal(refused.isError, true);
    assert.match(refused.text, /HTTP 409 conflict.*不在可执行时间范围内/);
    assert.equal(posts(st, /\/execute$/).length, 1);
  } finally { m.stop(); server.close(); }
});

test('审批 / 执行不会发出别的写请求（只有 pass、execute 和登录）', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_USER: 'root' });
  try {
    await m.call('approve_workflow', { workflow_id: 201, confirm_workflow_id: 201 });
    await m.call('execute_workflow', { workflow_id: 205, confirm_workflow_id: 205 });
    const writes = st.requests.filter((r) => /^(POST|PUT|PATCH|DELETE) /.test(r));
    assert.deepEqual([...writes].sort(), ['POST /api/v1/auth/login', 'POST /api/v1/auth/login', 'POST /api/v1/sqlworkflow/201/pass', 'POST /api/v1/sqlworkflow/205/execute'].sort());
    assert.ok(!st.requests.some((r) => /\/(cancel|schedule|run-date|osc|retry|close)\b|\/batch\//.test(r)));
  } finally { m.stop(); server.close(); }
});

// ── 提交 SQL 上线工单（含批量工单：一个工单覆盖多个实例） ──────────────────

const OK_SQL = 'alter table t add c int';
const CREATE = 'POST /api/v1/sqlworkflow';

test('submit_sql_workflow：已经启用，不再是占位', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const list = await m.rpc('tools/list');
    const t = list.result.tools.find((x) => x.name === 'submit_sql_workflow');
    assert.ok(!t.description.includes('暂未对接'));
    assert.match(t.description, /会在 dbtool 创建真实工单并通知审核人/);
    assert.match(t.description, /一个批量工单|【一个】批量工单/);
    assert.deepEqual(t.inputSchema.required, ['workflow_name', 'group_name', 'db_name', 'sql_content']);
    assert.equal(t.inputSchema.properties.skip_check, undefined, '不提供跳过检查');
    assert.equal(st.requests.length, 0);
  } finally { m.stop(); server.close(); }
});

test('submit_sql_workflow：本地校验不通过时不访问 dbtool', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const base = { workflow_name: '加列', group_name: '生产DDL上线', instance_name: 'ar001', db_name: 'shop', sql_content: OK_SQL };
    const cases = [
      [{ ...base, workflow_name: undefined }, /缺少必填参数: workflow_name/],
      [{ ...base, group_name: '' }, /缺少必填参数: group_name/],
      [{ ...base, db_name: undefined }, /缺少必填参数: db_name/],
      [{ ...base, sql_content: '   ' }, /sql_content 不能为空/],
      [{ ...base, workflow_name: '长'.repeat(51) }, /工单名称过长（最多 50 个字符，当前 51 个）/],
      [{ ...base, db_name: 'd'.repeat(65) }, /db_name 不能为空，且最多 64 个字符/],
      [{ ...base, demand_url: 'ftp://x' }, /demand_url 必须以 http/],
      [{ ...base, demand_url: `http://${'a'.repeat(500)}` }, /demand_url 过长/],
      [{ ...base, run_date_start: '2026-10-22' }, /run_date_start 的格式应为/],
      [{ ...base, run_date_end: '明天' }, /run_date_end 的格式应为/],
      [{ ...base, run_date_start: '2026-10-22 18:00', run_date_end: '2026-10-22 09:00' }, /结束时间必须晚于开始时间/],
      [{ ...base, run_date_start: '2026-10-22 18:00', run_date_end: '2026-10-22 18:00' }, /结束时间必须晚于开始时间/],
    ];
    for (const [args, re] of cases) {
      const r = await m.call('submit_sql_workflow', args);
      assert.equal(r.isError, true, JSON.stringify(args).slice(0, 80));
      assert.match(r.text, re);
    }
    assert.equal(st.requests.length, 0, '本地校验不通过时，连登录都不发');
    // 工单名正好 50 个字符、时间的两种写法都过得了本地校验
    const edge = await m.call('submit_sql_workflow', { ...base, workflow_name: '长'.repeat(50), run_date_start: '2026-10-22 09:00', run_date_end: '2026-10-22T18:00:30' });
    assert.equal(edge.isError, false, edge.text);
    assert.equal(st.created[0].run_date_start, '2026-10-22 09:00');
    assert.equal(st.created[0].run_date_end, '2026-10-22T18:00:30');
  } finally { m.stop(); server.close(); }
});

test('submit_sql_workflow：单实例', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_PUBLIC_URL: 'https://dbtool.example.com' });
  try {
    const r = await m.call('submit_sql_workflow', { workflow_name: '给订单表加列', group_name: '生产ddl上线', instance_name: 'ar001', db_name: 'shop', sql_content: OK_SQL, demand_url: 'https://jira.example.com/1' });
    assert.equal(r.isError, false, r.text);
    assert.equal(st.created.length, 1);
    assert.deepEqual(st.created[0], { name: '给订单表加列', instance_id: 11, db_name: 'shop', group_name: '生产DDL上线', sql: OK_SQL, is_backup: true, demand_url: 'https://jira.example.com/1' },
      '资源组名按 dbtool 里的写法发出去（输入大小写不同也认）；不带 instance_ids / skip_check / 导出相关字段');
    assert.match(r.text, /✅ SQL 上线工单已创建（单实例）/);
    assert.match(r.text, /工单号: #500  状态: 等待审批/);
    assert.match(r.text, /dbtool 提示: 工单已正常提交/);
    assert.match(r.text, /资源组: 生产DDL上线  库: shop/);
    assert.match(r.text, /实例: ar001/);
    assert.match(r.text, /须审核人在平台批准后才会执行；当前审批人：李四/);
    assert.match(r.text, /工单链接（⚠ 必须原样发给用户核对）: https:\/\/dbtool\.example\.com\/sqlworkflow\/500/);
    assert.equal(st.requests.filter((x) => x === 'GET /api/v1/sqlworkflow/options').length, 1);

    // is_backup 可以关；只有一个名字的 instance_names 按单实例处理
    const one = await m.call('submit_sql_workflow', { workflow_name: '单个', group_name: '生产DDL上线', instance_names: ['ar002'], db_name: 'shop', sql_content: OK_SQL, is_backup: false });
    assert.equal(one.isError, false, one.text);
    assert.equal(st.created[1].instance_id, 12);
    assert.equal(st.created[1].instance_ids, undefined);
    assert.equal(st.created[1].is_backup, false);
    assert.match(one.text, /（单实例）/);
  } finally { m.stop(); server.close(); }
});

test('submit_sql_workflow：批量 = 一次请求、一个工单，不拆成一个实例一个工单', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('submit_sql_workflow', { workflow_name: '批量加列', group_name: '生产DDL上线', instance_names: ['ar001', 'ar002', 'ar003'], db_name: 'shop', sql_content: OK_SQL });
    assert.equal(r.isError, false, r.text);
    assert.equal(st.requests.filter((x) => x === CREATE).length, 1, '只发一次创建请求');
    assert.equal(st.created.length, 1, '只创建一个工单');
    assert.deepEqual(st.created[0].instance_ids, [11, 12, 13]);
    assert.equal(st.created[0].instance_id, undefined);
    assert.match(r.text, /✅ SQL 上线工单已创建（批量工单，3 个实例，只有这一个工单）/);
    assert.match(r.text, /实例: ar001, ar002, ar003/);
    assert.equal(st.wf[500].is_batch, true);

    // 重复的名字去重；逗号分隔的字符串也认；去重后第一个实例是主实例
    const dup = await m.call('submit_sql_workflow', { workflow_name: '批量2', group_name: '生产DDL上线', instance_names: 'ar002, AR002, ar001', db_name: 'shop', sql_content: OK_SQL });
    assert.equal(dup.isError, false, dup.text);
    assert.deepEqual(st.created[1].instance_ids, [12, 11]);
    assert.match(dup.text, /instance_names 里有重复的实例，已去重/);
    assert.equal(st.created.length, 2);
  } finally { m.stop(); server.close(); }
});

test('submit_sql_workflow：提交前按 options 校验，不满足就整体报错、什么都不创建', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const call = (extra) => m.call('submit_sql_workflow', { workflow_name: '加列', group_name: '生产DDL上线', db_name: 'shop', sql_content: OK_SQL, ...extra });
    const expectRefused = async (extra, re, why) => {
      const r = await call(extra);
      assert.equal(r.isError, true, `${why}：${r.text}`);
      assert.match(r.text, re, why);
    };
    // 资源组
    await expectRefused({ instance_name: 'ar001', group_name: '不存在的组' }, /资源组不存在，或你不在该组内：不存在的组。你能用的资源组：生产DDL上线 \/ 测试环境上线/, '资源组不存在');
    // 实例归属
    await expectRefused({ instance_name: 'ar005' }, /以下实例不在资源组「生产DDL上线」的可上线清单里.*：ar005。该组可上线的实例：.*ar001/, '单实例不在资源组里');
    await expectRefused({ instance_names: ['ar001', 'ar005'] }, /：ar005。该组可上线的实例/, '批量里有一个不在资源组里');
    await expectRefused({ instance_name: 'ar001', group_name: '测试环境上线' }, /以下实例不在资源组「测试环境上线」的可上线清单里/, '实例属于另一个资源组');
    // 批量权限、类型、数量
    st.options.batch.allowed = false;
    await expectRefused({ instance_names: ['ar001', 'ar002'] }, /没有批量上线的权限（缺少 sqlworkflow.submit_batch）/, '没有批量权限');
    st.options.batch.allowed = true;
    await expectRefused({ instance_names: ['test-mysql-01', 'test-mysql-02'], group_name: '测试环境上线' }, /批量上线只支持这些类型的实例：mssql；不符合的：test-mysql-01（mysql）、test-mysql-02（mysql）/, '批量只支持 mssql');
    st.options.batch.types = ['mssql', 'mysql'];
    await expectRefused({ instance_names: ['ar001', 'test-mysql-01'] }, /所选实例的数据库类型不一致/, '批量实例类型不一致');
    st.options.batch.types = ['mssql'];
    await expectRefused({ instance_names: ['ar001', 'ar002', 'ar003', 'ar004'] }, /单个批量工单最多支持 3 个实例，当前选择了 4 个/, '超过批量上限');
    // 实例名解析
    await expectRefused({ instance_names: ['ar001', 'ar999'] }, /实例不存在或不可见: ar999/, '实例名不存在');

    assert.equal(st.created.length, 0, '以上任何一种都不能创建工单');
    assert.equal(st.requests.filter((x) => x === CREATE).length, 0, '也没有发出创建请求（更没有退化成逐个实例提交）');

    // 单实例不受批量规则限制：mysql 实例可以单个提交
    st.options.groups[1].instances = st.options.groups[1].instances.slice(0, 1);
    const ok = await call({ instance_name: 'test-mysql-01', group_name: '测试环境上线' });
    assert.equal(ok.isError, false, ok.text);
    assert.equal(st.created.length, 1);
  } finally { m.stop(); server.close(); }
});

test('submit_sql_workflow：自动审批不通过 / 无需审批 / 服务端拒绝 / 连接断开', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_TIMEOUT_MS: '5000' });
  try {
    const call = (sql) => m.call('submit_sql_workflow', { workflow_name: '加列', group_name: '生产DDL上线', instance_name: 'ar001', db_name: 'shop', sql_content: sql });
    const auto = await call('alter table autoreject');
    assert.equal(auto.isError, false, auto.text);
    assert.match(auto.text, /状态: 自动审批不通过/);
    assert.match(auto.text, /⚠ 检查有错误，系统自动审批不通过，工单没有进入审批/);
    assert.match(auto.text, /提交前的检查是通过的，多半是检查之后表结构或数据变了。/, '检查通过后才出现的“自动审批不通过”，要说明是检查之后变了');
    assert.ok(!auto.text.includes('须审核人在平台批准后才会执行'));

    // 检查只有警告、却被自动审批不通过：多半是管理员把自动驳回设成了“有警告也驳回”
    const warnRej = await call('alter table warn autoreject');
    assert.equal(warnRej.isError, false, warnRej.text);
    assert.match(warnRej.text, /状态: 自动审批不通过/);
    assert.match(warnRej.text, /提交前检查只有警告、没有错误，多半是管理员把自动驳回设成了“有警告也驳回”（review\.auto_reject_level=warning）。/);
    assert.ok(!warnRej.text.includes('提交前的检查是通过的'));

    const nopass = await call('alter table nopass');
    assert.match(nopass.text, /状态: 审核通过/);
    assert.match(nopass.text, /⚠ 这张工单无需审批，系统已直接审核通过，现在就可以执行/);
    assert.ok(!nopass.text.includes('须审核人在平台批准后才会执行'));

    const before = st.created.length;
    st.requests.length = 0;
    const refused = await call('alter table servererr');
    assert.equal(refused.isError, true);
    assert.match(refused.text, /HTTP 400 bad_request.*请选择DDL资源组/);
    assert.equal(st.created.length, before, '服务端拒绝时没有创建工单');
    assert.equal(st.requests.filter((x) => x === CREATE).length, 1, '只发一次，不重试');

    st.requests.length = 0;
    const hang = await call('alter table hang');
    assert.equal(hang.isError, true);
    assert.match(hang.text, /请求失败/);
    assert.match(hang.text, /工单可能已经创建：请先用 list_workflows（search 填工单名）核对，不要直接重新提交/);
    assert.equal(st.requests.filter((x) => x === CREATE).length, 1, '连接断开后也不自动重试，免得重复创建');
  } finally { m.stop(); server.close(); }
});

test('submit_sql_workflow：令牌失效重登后重发，只创建一个工单；不发跳过检查 / 导出 / 定时字段', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    await m.call('list_instances', {}); // 先登录并缓存实例清单
    st.rejectCreateOnce = true; // 创建请求第一次返回 401（令牌失效），此时工单并没有被创建
    const r = await m.call('submit_sql_workflow', { workflow_name: '加列', group_name: '生产DDL上线', instance_names: ['ar001', 'ar002'], db_name: 'shop', sql_content: OK_SQL });
    assert.equal(r.isError, false, r.text);
    assert.equal(st.logins, 2, '401 之后重新登录一次');
    assert.equal(st.requests.filter((x) => x === CREATE).length, 2, '第一次被拒（401），重发一次');
    assert.equal(st.created.length, 1, '最终只创建一个工单');
    const allowed = new Set(['name', 'instance_ids', 'instance_id', 'db_name', 'group_name', 'sql', 'is_backup', 'demand_url', 'run_date_start', 'run_date_end']);
    assert.deepEqual(Object.keys(st.created[0]).filter((k) => !allowed.has(k)), [], '请求体里没有 skip_check / export / plan_run_at 等字段');
  } finally { m.stop(); server.close(); }
});

// ── 提交数据导出工单（含批量导出：一个工单覆盖多个实例） ──────────────────

const EXPORT_SQL = 'select id, name from orders where id < 100';
const exportArgs = (extra) => ({ workflow_name: '导出订单', group_name: '生产DDL上线', db_name: 'shop', sql_content: EXPORT_SQL, ...extra });

test('submit_export_workflow：已经启用，不再是占位', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const list = await m.rpc('tools/list');
    const t = list.result.tools.find((x) => x.name === 'submit_export_workflow');
    assert.ok(!t.description.includes('暂未对接'));
    assert.match(t.description, /会在 dbtool 创建真实工单并通知审核人/);
    assert.match(t.description, /一个批量导出工单|【一个】批量导出工单/);
    assert.match(t.description, /至少 2 个不同的实例/);
    assert.deepEqual(t.inputSchema.required, ['workflow_name', 'group_name', 'db_name', 'sql_content']);
    const props = t.inputSchema.properties;
    assert.deepEqual(props.export_format.enum, ['csv', 'xlsx', 'json', 'xml', 'sql']);
    assert.equal(props.export_merge.type, 'boolean');
    assert.equal(props.instance_names.type, 'array');
    for (const k of ['skip_check', 'export_unmasked', 'export_unmask_reason', 'plan_run_at', 'is_backup']) assert.equal(props[k], undefined, `不提供 ${k}`);
    assert.equal(st.requests.length, 0);
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：本地校验不通过时不访问 dbtool', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const base = exportArgs({ instance_name: 'ar001' });
    const cases = [
      [{ ...base, workflow_name: undefined }, /缺少必填参数: workflow_name/],
      [{ ...base, group_name: '' }, /缺少必填参数: group_name/],
      [{ ...base, db_name: undefined }, /缺少必填参数: db_name/],
      [{ ...base, sql_content: '   ' }, /sql_content 不能为空/],
      [{ ...base, workflow_name: '长'.repeat(51) }, /工单名称过长（最多 50 个字符，当前 51 个）/],
      [{ ...base, db_name: 'd'.repeat(65) }, /db_name 不能为空，且最多 64 个字符/],
      [{ ...base, demand_url: 'ftp://x' }, /demand_url 必须以 http/],
      [{ ...base, run_date_start: '2026-10-22' }, /run_date_start 的格式应为/],
      [{ ...base, run_date_start: '2026-10-22 18:00', run_date_end: '2026-10-22 09:00' }, /结束时间必须晚于开始时间/],
      [{ ...base, export_format: 'pdf' }, /export_format 只能是 csv \/ xlsx \/ json \/ xml \/ sql（默认 csv）/],
      [{ ...base, export_merge: 'maybe' }, /export_merge 只能是 true 或 false/],
    ];
    for (const [args, re] of cases) {
      const r = await m.call('submit_export_workflow', args);
      assert.equal(r.isError, true, JSON.stringify(args).slice(0, 80));
      assert.match(r.text, re);
    }
    assert.equal(st.requests.length, 0, '本地校验不通过时，连登录都不发');
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：只允许 select / with 查询、合并只能用于批量：不读选项也不创建', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const refused = [
      [{ instance_name: 'ar001', sql_content: 'delete from orders' }, /数据导出只允许 select \/ with 查询（SQL Server 还允许 declare \/ set 开头的只读批处理）；写操作请走上线工单（submit_sql_workflow）/],
      [{ instance_name: 'ar001', sql_content: '/* 先清理 */ update orders set a = 1' }, /数据导出只允许 select/],
      [{ instance_name: 'ar001', sql_content: 'show tables' }, /数据导出只允许 select/],
      [{ instance_name: 'ar001', sql_content: 'exec sp_who' }, /数据导出只允许 select/],
      [{ instance_name: 'ar001', sql_content: '(select 1)' }, /数据导出只允许 select/],
      [{ instance_name: 'test-mysql-01', group_name: '测试环境上线', sql_content: 'declare @a int; select @a' }, /数据导出只允许 select \/ with 查询；写操作请走上线工单/],
      [{ instance_name: 'ar001', export_merge: true }, /export_merge（合并成一个文件）只能用于批量导出：instance_names 要有至少 2 个不同的实例/],
      [{ instance_names: ['ar001', 'AR001'], export_merge: true }, /只能用于批量导出/],
    ];
    for (const [extra, re] of refused) {
      const r = await m.call('submit_export_workflow', exportArgs(extra));
      assert.equal(r.isError, true, JSON.stringify(extra));
      assert.match(r.text, re);
    }
    assert.ok(!st.requests.includes('GET /api/v1/sqlworkflow/options'), '没有走到读提交选项那一步');
    assert.equal(st.requests.filter((x) => x === CREATE).length, 0);
    assert.equal(st.created.length, 0);

    // 合法的写法：开头有注释、大小写不限、with；SQL Server 的 declare / set 批处理；MySQL 系的 # 注释
    const allowed = [
      [{ instance_name: 'ar001', sql_content: '-- 导出订单\n/* 给财务 */ SELECT 1' }],
      [{ instance_name: 'ar001', sql_content: 'WITH t AS (select 1 a) select * from t' }],
      [{ instance_name: 'ar001', sql_content: "declare @d date = '2026-01-01'; select * from orders where d > @d" }],
      [{ instance_name: 'ar001', sql_content: 'set @d = 1; select @d' }],
      [{ instance_name: 'test-mysql-01', group_name: '测试环境上线', sql_content: '# 导出\nselect 1' }],
    ];
    for (const [extra] of allowed) {
      const r = await m.call('submit_export_workflow', exportArgs(extra));
      assert.equal(r.isError, false, `${extra.sql_content}：${r.text}`);
    }
    assert.equal(st.created.length, allowed.length);
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：单实例', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_PUBLIC_URL: 'https://dbtool.example.com' });
  try {
    const r = await m.call('submit_export_workflow', exportArgs({ group_name: '生产ddl上线', instance_name: 'ar001', demand_url: 'https://jira.example.com/2', export_format: 'XLSX' }));
    assert.equal(r.isError, false, r.text);
    assert.equal(st.created.length, 1);
    assert.deepEqual(st.created[0], { name: '导出订单', instance_id: 11, db_name: 'shop', group_name: '生产DDL上线', sql: EXPORT_SQL, export: true, export_format: 'xlsx', demand_url: 'https://jira.example.com/2' },
      '资源组名按 dbtool 里的写法、格式转小写；不带 instance_ids / is_backup / 跳过检查 / 导出原值 / 定时字段');
    assert.match(r.text, /✅ 数据导出工单已创建（单实例）/);
    assert.match(r.text, /工单号: #500  状态: 等待审批/);
    assert.match(r.text, /dbtool 提示: 工单已正常提交/);
    assert.match(r.text, /资源组: 生产DDL上线  库: shop/);
    assert.match(r.text, /实例: ar001/);
    assert.match(r.text, /导出格式: xlsx\n/, '单实例不写合并 / 分文件');
    assert.match(r.text, /脱敏: 按平台规则处理（MCP 不提供“导出原值”）/);
    assert.match(r.text, /须审核人在平台批准后，再执行导出才会生成文件；当前审批人：李四/);
    assert.match(r.text, /工单链接（⚠ 必须原样发给用户核对）: https:\/\/dbtool\.example\.com\/sqlworkflow\/500/);
    assert.equal(st.requests.filter((x) => x === 'GET /api/v1/sqlworkflow/options').length, 1);
    assert.equal(st.wf[500].is_export, true);

    // 格式缺省 csv；只有一个名字的 instance_names 按单实例处理；export_merge 明确写 false / "false" 都不合并
    const one = await m.call('submit_export_workflow', exportArgs({ instance_names: ['ar002'], export_merge: false }));
    assert.equal(one.isError, false, one.text);
    assert.deepEqual(st.created[1], { name: '导出订单', instance_id: 12, db_name: 'shop', group_name: '生产DDL上线', sql: EXPORT_SQL, export: true, export_format: 'csv' });
    assert.match(one.text, /（单实例）/);
    const dup = await m.call('submit_export_workflow', exportArgs({ instance_names: ['ar002', 'AR002'], export_merge: 'false' }));
    assert.equal(dup.isError, false, dup.text);
    assert.equal(st.created[2].instance_id, 12);
    assert.equal(st.created[2].instance_ids, undefined);
    assert.equal(st.created[2].export_merge, undefined);
    assert.match(dup.text, /instance_names 里有重复的实例，已去重，只剩 1 个实例，按单实例导出/);

    // 不是 SQL Server 的实例也能单个导出（批量类型限制只管批量）
    const my = await m.call('submit_export_workflow', exportArgs({ group_name: '测试环境上线', instance_name: 'test-mysql-01', export_format: 'json' }));
    assert.equal(my.isError, false, my.text);
    assert.equal(st.created[3].instance_id, 2);
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：批量导出 = 一次请求、一个工单，不拆成一个实例一个工单；export_merge 合并成一个文件', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('submit_export_workflow', exportArgs({ instance_names: ['ar001', 'ar002', 'ar003'] }));
    assert.equal(r.isError, false, r.text);
    assert.equal(st.requests.filter((x) => x === CREATE).length, 1, '只发一次创建请求');
    assert.equal(st.created.length, 1, '只创建一个工单');
    assert.deepEqual(st.created[0], { name: '导出订单', instance_ids: [11, 12, 13], db_name: 'shop', group_name: '生产DDL上线', sql: EXPORT_SQL, export: true, export_format: 'csv' });
    assert.match(r.text, /✅ 数据导出工单已创建（批量导出，3 个实例，只有这一个工单）/);
    assert.match(r.text, /实例: ar001, ar002, ar003/);
    assert.match(r.text, /导出格式: csv；每个实例各导出一个文件/);
    assert.equal(st.wf[500].is_batch, true);
    assert.equal(st.wf[500].is_export, true);

    // 合并成一个文件：export_merge: true 原样发给 dbtool；逗号分隔的字符串、重复的名字也认，去重后第一个实例是主实例
    const mg = await m.call('submit_export_workflow', exportArgs({ instance_names: 'ar002, AR002, ar001', export_merge: true, export_format: 'json' }));
    assert.equal(mg.isError, false, mg.text);
    assert.equal(st.created.length, 2);
    assert.deepEqual(st.created[1], { name: '导出订单', instance_ids: [12, 11], db_name: 'shop', group_name: '生产DDL上线', sql: EXPORT_SQL, export: true, export_format: 'json', export_merge: true });
    assert.match(mg.text, /（批量导出，2 个实例，只有这一个工单）/);
    assert.match(mg.text, /导出格式: json；各实例的数据合并成一个文件/);
    assert.match(mg.text, /instance_names 里有重复的实例，已去重\）/);
    assert.equal(st.wf[501].export_merge, true);

    // "true" 字符串也算合并
    const s = await m.call('submit_export_workflow', exportArgs({ instance_names: ['ar001', 'ar003'], export_merge: 'true' }));
    assert.equal(s.isError, false, s.text);
    assert.equal(st.created[2].export_merge, true);
    assert.equal(st.requests.filter((x) => x === CREATE).length, 3, '三次提交，三个工单（每次一个）');
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：提交前按 options 校验，不满足就整体报错、什么都不创建', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const call = (extra) => m.call('submit_export_workflow', exportArgs(extra));
    const expectRefused = async (extra, re, why) => {
      const r = await call(extra);
      assert.equal(r.isError, true, `${why}：${r.text}`);
      assert.match(r.text, re, why);
    };
    // 资源组
    await expectRefused({ instance_name: 'ar001', group_name: '不存在的组' }, /资源组不存在，或你不在该组内：不存在的组。你能用的资源组：生产DDL上线 \/ 测试环境上线/, '资源组不存在');
    // 导出权限（单实例、批量各有各的）
    st.options.export.allowed = false;
    await expectRefused({ instance_name: 'ar001' }, /没有数据导出的权限（缺少 export.submit）/, '没有单实例导出权限');
    st.options.export.allowed = true;
    st.options.export.batch_allowed = false;
    await expectRefused({ instance_names: ['ar001', 'ar002'] }, /没有批量数据导出的权限（缺少 export.submit_batch）/, '没有批量导出权限');
    st.options.export.batch_allowed = true;
    // 实例是否在该组的“可导出”清单里：ar004 能上线、不能导出；ar005 不在任何资源组；ar001 属于另一个组的清单之外
    await expectRefused({ instance_name: 'ar004' }, /以下实例不在资源组「生产DDL上线」的可导出清单里.*：ar004。该组可导出的实例：.*ar001/, '能上线但不能导出');
    await expectRefused({ instance_names: ['ar001', 'ar005'] }, /：ar005。该组可导出的实例/, '批量里有一个不在资源组里');
    await expectRefused({ instance_name: 'ar001', group_name: '测试环境上线' }, /不在资源组「测试环境上线」的可导出清单里/, '实例属于另一个资源组');
    // 批量类型、数量
    await expectRefused({ instance_names: ['test-mysql-01', 'test-mysql-02'], group_name: '测试环境上线' }, /批量导出只支持这些类型的实例：mssql；不符合的：test-mysql-01（mysql）、test-mysql-02（mysql）/, '批量只支持 mssql');
    st.options.batch.types = ['mssql', 'mysql'];
    st.options.groups[0].export_instances.push({ id: 2, name: 'test-mysql-01', db_type: 'mysql' }, { id: 14, name: 'ar004', db_type: 'mssql' });
    await expectRefused({ instance_names: ['ar001', 'test-mysql-01'] }, /所选实例的数据库类型不一致，批量导出要求所有实例为同一类型/, '批量实例类型不一致');
    st.options.batch.types = ['mssql'];
    await expectRefused({ instance_names: ['ar001', 'ar002', 'ar003', 'ar004'] }, /单个批量工单最多支持 3 个实例，当前选择了 4 个/, '超过批量上限');
    // 导出格式：dbtool 没有提供的格式
    st.options.export.formats = [{ value: 'csv', label: 'CSV' }];
    await expectRefused({ instance_name: 'ar001', export_format: 'xlsx' }, /当前不能导出成 xlsx 格式；可选：csv/, '格式不在选项里');
    st.options.export.formats = ['csv', 'xlsx', 'json', 'xml', 'sql'].map((value) => ({ value, label: value }));
    // 合并成一个文件：dbtool 没开放
    st.options.export.can_merge = false;
    await expectRefused({ instance_names: ['ar001', 'ar002'], export_merge: true }, /当前不能把批量导出合并成一个文件/, '不能合并');
    st.options.export.can_merge = true;
    // 实例名解析
    await expectRefused({ instance_names: ['ar001', 'ar999'] }, /实例不存在或不可见: ar999/, '实例名不存在');

    assert.equal(st.created.length, 0, '以上任何一种都不能创建工单');
    assert.equal(st.requests.filter((x) => x === CREATE).length, 0, '也没有发出创建请求（更没有退化成逐个实例提交）');

    // 条件满足后同样的请求可以提交；不合并时 can_merge 为 false 也不影响
    st.options.export.can_merge = false;
    const ok = await call({ instance_names: ['ar001', 'ar002'] });
    assert.equal(ok.isError, false, ok.text);
    assert.equal(st.created.length, 1);
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：自动审批不通过 / 无需审批 / 服务端拒绝 / 连接断开', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_TIMEOUT_MS: '5000' });
  try {
    const call = (sql) => m.call('submit_export_workflow', exportArgs({ instance_name: 'ar001', sql_content: sql }));
    const auto = await call('select 1 -- autoreject');
    assert.equal(auto.isError, false, auto.text);
    assert.match(auto.text, /状态: 自动审批不通过/);
    assert.match(auto.text, /⚠ 检查有错误，系统自动审批不通过，工单没有进入审批/);
    assert.match(auto.text, /提交前的预检是通过的，多半是预检之后数据或权限变了/, '预检通过后才出现的“自动审批不通过”，要说明是预检之后变了');
    assert.match(auto.text, /导出行数超过上限/);

    // 预检只有警告（T-SQL 批处理统计不了行数）、却被自动审批不通过：多半是管理员把自动驳回设成了“有警告也驳回”
    const warnRej = await m.call('submit_export_workflow', exportArgs({ instance_names: ['ar001', 'ar002'], sql_content: 'declare @d int = 1; select @d -- autoreject' }));
    assert.equal(warnRej.isError, false, warnRej.text);
    assert.match(warnRej.text, /状态: 自动审批不通过/);
    assert.match(warnRej.text, /提交前预检只有警告、没有错误，多半是管理员把自动驳回设成了“有警告也驳回”（review\.auto_reject_level=warning）。/);
    assert.ok(!warnRej.text.includes('提交前的预检是通过的'));
    assert.ok(!auto.text.includes('须审核人在平台批准后'));

    const nopass = await call('select 1 -- nopass');
    assert.match(nopass.text, /状态: 审核通过/);
    assert.match(nopass.text, /⚠ 这张导出工单无需审批，系统已直接审核通过，只差执行导出/);
    assert.ok(!nopass.text.includes('须审核人在平台批准后'));

    const before = st.created.length;
    st.requests.length = 0;
    const refused = await call('select 1 -- servererr');
    assert.equal(refused.isError, true);
    assert.match(refused.text, /HTTP 400 bad_request/);
    assert.equal(st.created.length, before, '服务端拒绝时没有创建工单');
    assert.equal(st.requests.filter((x) => x === CREATE).length, 1, '只发一次，不重试');

    st.requests.length = 0;
    const hang = await call('select 1 -- hang');
    assert.equal(hang.isError, true);
    assert.match(hang.text, /请求失败/);
    assert.match(hang.text, /工单可能已经创建：请先用 list_workflows（kind 填 export，search 填工单名）核对，不要直接重新提交/);
    assert.equal(st.requests.filter((x) => x === CREATE).length, 1, '连接断开后也不自动重试，免得重复创建');
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：令牌失效重登后重发，只创建一个工单；请求体里没有不该有的字段', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    await m.call('list_instances', {}); // 先登录并缓存实例清单
    st.rejectCreateOnce = true; // 创建请求第一次返回 401（令牌失效），此时工单并没有被创建
    const r = await m.call('submit_export_workflow', exportArgs({
      instance_names: ['ar001', 'ar002'], export_merge: true, export_format: 'xml',
      demand_url: 'https://jira.example.com/3', run_date_start: '2026-10-22 09:00', run_date_end: '2026-10-22 18:00',
    }));
    assert.equal(r.isError, false, r.text);
    assert.equal(st.logins, 2, '401 之后重新登录一次');
    assert.equal(st.requests.filter((x) => x === CREATE).length, 2, '第一次被拒（401），重发一次');
    assert.equal(st.created.length, 1, '最终只创建一个工单');
    const body = st.created[0];
    const allowed = new Set(['name', 'instance_ids', 'instance_id', 'db_name', 'group_name', 'sql', 'export', 'export_format', 'export_merge', 'demand_url', 'run_date_start', 'run_date_end']);
    assert.deepEqual(Object.keys(body).filter((k) => !allowed.has(k)), [], '请求体里没有多余的字段');
    for (const k of ['is_backup', 'skip_check', 'export_unmasked', 'export_unmask_reason', 'plan_run_at']) assert.ok(!(k in body), `不能带 ${k}`);
    assert.equal(body.export, true);
    assert.equal(body.run_date_start, '2026-10-22 09:00');
    assert.equal(body.run_date_end, '2026-10-22 18:00');
  } finally { m.stop(); server.close(); }
});

// ── sql_check 的数据导出预检（export=true，只读） ─────────────────────────

const CHECK = 'POST /api/v1/sqlworkflow/check';
const parseOut = (r) => JSON.parse(r.text);
const writesOf = (st) => st.requests.filter((x) => /^(POST|PUT|PATCH|DELETE) /.test(x) && !x.startsWith('POST /api/v1/auth/login'));

test('sql_check：带上数据导出预检的参数，说明里写明会统计行数', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const list = await m.rpc('tools/list');
    const t = list.result.tools.find((x) => x.name === 'sql_check');
    assert.equal(t.inputSchema.properties.export.type, 'boolean');
    assert.equal(t.inputSchema.properties.export_merge.type, 'boolean');
    assert.deepEqual(t.inputSchema.required, ['db_name', 'sql_content']);
    assert.match(t.description, /数据导出预检（export=true）/);
    assert.match(t.description, /select count\(\*\)/, '要写明导出预检会在每个实例上执行一次统计');
    assert.match(t.description, /自动审批不通过/);
    const sub = list.result.tools.find((x) => x.name === 'submit_export_workflow');
    assert.match(sub.description, /sql_check（export=true/, '提交导出工单的说明里要求先预检');
    assert.equal(st.requests.length, 0);
  } finally { m.stop(); server.close(); }
});

test('sql_check（export=true）：单实例——只发一次检查请求，不读选项也不创建', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('sql_check', { instance_name: 'ar001', db_name: 'shop', sql_content: EXPORT_SQL, export: true });
    assert.equal(r.isError, false, r.text);
    assert.deepEqual(st.byPath[CHECK], { instance_id: 11, db_name: 'shop', sql: EXPORT_SQL, export: true });
    const out = parseOut(r);
    assert.equal(out.verdict, '通过');
    assert.equal(out.mode, '单实例导出');
    assert.equal(out.syntax, '离线导出');
    assert.equal(out.source, 'export');
    assert.equal(out.rows.length, 1);
    assert.equal(out.rows[0].rows_to_export, 120);
    assert.equal(out.rows[0].affected_rows, undefined, '待导出行数统一写成 rows_to_export');
    assert.equal(out.rows[0].status, '行数统计完成');
    assert.equal(out.rows_to_export_total, undefined, '单实例不另给合计');
    assert.equal(out.instance_checks, undefined);
    assert.equal(out.batch_note, undefined);
    assert.match(out.next, /检查没有错误，可以用 submit_export_workflow 提交/);
    assert.deepEqual(writesOf(st), [CHECK], '除了登录，只有这一次检查请求');
    assert.ok(!st.requests.includes('GET /api/v1/sqlworkflow/options'), '预检不读提交选项');
    assert.equal(st.created.length, 0);

    // 0 行也要明确写出来（compactRow 会把 0 丢掉）
    const zero = parseOut(await m.call('sql_check', { instance_name: 'ar001', db_name: 'shop', sql_content: 'select 1 -- zero', export: true }));
    assert.equal(zero.verdict, '通过');
    assert.equal(zero.rows[0].rows_to_export, 0);

    // export 写成字符串 "true" 也认；false 或不写就是上线检查：请求里没有 export 字段，输出里没有导出提示
    await m.call('sql_check', { instance_name: 'ar001', db_name: 'shop', sql_content: EXPORT_SQL, export: 'true' });
    assert.equal(st.byPath[CHECK].export, true);
    const plain = parseOut(await m.call('sql_check', { instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'alter table t add c int', export: false }));
    assert.equal(plain.mode, '单实例');
    assert.deepEqual(st.byPath[CHECK], { instance_id: 2, db_name: 'shop', sql: 'alter table t add c int' });
    assert.equal(plain.next, undefined);
    assert.equal(plain.rows_to_export_total, undefined);
  } finally { m.stop(); server.close(); }
});

test('sql_check（export=true）：行数超限 / 没有库表权限 / 批处理 / 服务端拒绝', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const check = async (extra) => parseOut(await m.call('sql_check', { instance_name: 'ar001', db_name: 'shop', export: true, ...extra }));
    const many = await check({ sql_content: 'select 1 -- toomany' });
    assert.equal(many.verdict, '不通过');
    assert.equal(many.error_count, 1);
    assert.equal(many.rows[0].level, 'error');
    assert.match(many.rows[0].message, /导出数据行数\(20000\)超过阈值\(10000\)/);
    assert.equal(many.rows[0].rows_to_export, 20000, '超限的行把实际行数也写出来');
    assert.match(many.next, /提交前会自动跑同样的预检，不通过就不会创建工单/);
    assert.ok(!many.next.includes('可以用 submit_export_workflow 提交'));

    const denied = await check({ sql_content: 'select 1 -- denied' });
    assert.equal(denied.verdict, '不通过');
    assert.equal(denied.rows[0].status, '无库表查询权限');
    assert.match(denied.rows[0].message, /没有 shop\.orders 的查询权限/);
    assert.equal(denied.rows[0].rows_to_export, undefined, '出错又没有行数的行不写 rows_to_export（0 不代表“0 行”）');
    assert.match(denied.next, /不通过就不会创建工单/);

    // 服务端没给出行数（null / 空串）：不能编造成“0 行”
    for (const kw of ['nocount', 'blankcount']) {
      const nc = await check({ sql_content: `select 1 -- ${kw}` });
      assert.equal(nc.verdict, '通过', kw);
      assert.equal(nc.rows[0].rows_to_export, undefined, `${kw}：没有行数就不写 rows_to_export`);
    }

    // SQL Server 的 declare / set 批处理统计不了行数：只给警告，不给 rows_to_export
    const decl = await check({ sql_content: 'declare @d int = 1; select @d' });
    assert.equal(decl.verdict, '有警告');
    assert.equal(decl.rows[0].level, 'warning');
    assert.match(decl.rows[0].message, /无法预先统计行数/);
    assert.equal(decl.rows[0].rows_to_export, undefined);
    assert.match(decl.next, /检查没有错误，可以用 submit_export_workflow 提交/);

    // 服务端不让用（账号没有导出权限）：错误原样转出，不当作检查结果
    st.options.export.allowed = false;
    const no = await m.call('sql_check', { instance_name: 'ar001', db_name: 'shop', sql_content: EXPORT_SQL, export: true });
    assert.equal(no.isError, true);
    assert.match(no.text, /HTTP 403 forbidden.*缺少 export.submit/);
    st.options.export.allowed = true;
    st.options.export.batch_allowed = false;
    const nob = await m.call('sql_check', { instance_names: ['ar001', 'ar002'], db_name: 'shop', sql_content: EXPORT_SQL, export: true });
    assert.equal(nob.isError, true);
    assert.match(nob.text, /缺少 export.submit_batch/);
    // 超限、无权限、两种没给行数、批处理，加上两次 403：共 7 次检查请求，被拒绝时也不重试
    assert.deepEqual(writesOf(st), Array(7).fill(CHECK), '每次只发一次检查请求，被拒绝时不重试');
    assert.equal(st.created.length, 0);
  } finally { m.stop(); server.close(); }
});

test('sql_check（export=true）：批量——一次请求带全部实例，export_merge 才核对各实例的列', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('sql_check', { instance_names: ['ar001', 'ar002', 'ar003'], db_name: 'shop', sql_content: EXPORT_SQL, export: true });
    assert.equal(r.isError, false, r.text);
    assert.deepEqual(st.byPath[CHECK], { instance_ids: [11, 12, 13], db_name: 'shop', sql: EXPORT_SQL, export: true });
    assert.equal(st.requests.filter((x) => x === CHECK).length, 1, '只发一次检查请求');
    const out = parseOut(r);
    assert.equal(out.verdict, '通过');
    assert.equal(out.mode, '批量导出（3 个实例，提交时是一个批量导出工单，每个实例各一个文件）');
    assert.deepEqual(out.instances, ['ar001', 'ar002', 'ar003']);
    assert.deepEqual(out.rows.map((x) => x.instance), ['ar001', 'ar002', 'ar003']);
    assert.deepEqual(out.rows.map((x) => x.rows_to_export), [120, 120, 120]);
    assert.equal(out.rows_to_export_total, 360);
    assert.match(out.batch_note, /每个实例上各做一次/);
    assert.ok(!out.batch_note.includes('列是否一致'));
    assert.deepEqual(out.instance_checks.map((c) => c.ok), [true, true, true]);

    // 合并成一个文件：请求里带 export_merge，说明里写“合并成一个文件”，并核对列
    const mg = parseOut(await m.call('sql_check', { instance_names: 'ar001, ar002', db_name: 'shop', sql_content: EXPORT_SQL, export: true, export_merge: true }));
    assert.deepEqual(st.byPath[CHECK], { instance_ids: [11, 12], db_name: 'shop', sql: EXPORT_SQL, export: true, export_merge: true });
    assert.equal(mg.mode, '批量导出（2 个实例，提交时是一个批量导出工单，合并成一个文件）');
    assert.match(mg.batch_note, /并核对各实例返回的列是否一致/);
    assert.equal(mg.rows_to_export_total, 240, '合并成一个文件时，合计就是文件里的行数');

    // 第二个实例的列不一致：只有带 export_merge 才查得出来
    const bad = parseOut(await m.call('sql_check', { instance_names: ['ar001', 'ar002'], db_name: 'shop', sql_content: 'select 1 -- colmismatch', export: true, export_merge: true }));
    assert.equal(bad.verdict, '不通过');
    assert.match(bad.rows[1].message, /返回的列与第一个实例「ar001」不一致/);
    assert.deepEqual(bad.instance_checks.map((c) => c.ok), [true, false]);
    assert.equal(bad.rows_to_export_total, undefined, '有实例出错时不给合计');
    assert.match(bad.next, /不通过就不会创建工单/);
    const same = parseOut(await m.call('sql_check', { instance_names: ['ar001', 'ar002'], db_name: 'shop', sql_content: 'select 1 -- colmismatch', export: true }));
    assert.equal(same.verdict, '通过', '不合并时各实例各一个文件，列不必一致');

    // 最后一个实例没有库表查询权限：指出是哪一个
    const dn = parseOut(await m.call('sql_check', { instance_names: ['ar001', 'ar002'], db_name: 'shop', sql_content: 'select 1 -- denied', export: true }));
    assert.equal(dn.verdict, '不通过');
    assert.deepEqual(dn.instance_checks.filter((c) => !c.ok).map((c) => c.name), ['ar002']);
    assert.match(dn.instance_checks[1].error, /实例\[ar002\]没有 shop\.orders 的查询权限/);

    // 有的实例没给出行数：其余实例的行数照写，但不给合计（不能拿一部分当全部）
    const part = parseOut(await m.call('sql_check', { instance_names: ['ar001', 'ar002'], db_name: 'shop', sql_content: 'select 1 -- nocount', export: true }));
    assert.equal(part.verdict, '通过');
    assert.deepEqual(part.rows.map((x) => x.rows_to_export), [undefined, 120]);
    assert.equal(part.rows_to_export_total, undefined);

    // T-SQL 批处理：统计不了行数，只给一行警告（不是逐实例的行）
    const decl = parseOut(await m.call('sql_check', { instance_names: ['ar001', 'ar002'], db_name: 'shop', sql_content: 'declare @d int = 1; select @d', export: true }));
    assert.equal(decl.verdict, '有警告');
    assert.equal(decl.rows.length, 1);
    assert.equal(decl.rows_to_export_total, undefined);
    assert.deepEqual(decl.instance_checks, []);

    // 重复的名字去重；只剩一个实例就是单实例导出预检
    const dup = parseOut(await m.call('sql_check', { instance_names: ['ar001', 'AR001'], db_name: 'shop', sql_content: EXPORT_SQL, export: true }));
    assert.equal(dup.mode, '单实例导出');
    assert.deepEqual(st.byPath[CHECK], { instance_id: 11, db_name: 'shop', sql: EXPORT_SQL, export: true });

    assert.deepEqual([...new Set(writesOf(st))], [CHECK], '始终只有检查请求');
    assert.equal(st.created.length, 0);
  } finally { m.stop(); server.close(); }
});

test('sql_check（export=true）：本地检查不通过时不发检查请求；不带 export 的上线检查不受影响', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    // 完全在本地判断的：连登录都不发
    for (const [extra, re] of [
      [{ export_merge: true }, /export_merge 只能和 export=true 一起用/],
      [{ export: 'maybe' }, /export 只能是 true 或 false/],
      [{ export: true, export_merge: 'maybe' }, /export_merge 只能是 true 或 false/],
    ]) {
      const r = await m.call('sql_check', { instance_names: ['ar001', 'ar002'], db_name: 'shop', sql_content: EXPORT_SQL, ...extra });
      assert.equal(r.isError, true, JSON.stringify(extra));
      assert.match(r.text, re);
    }
    assert.equal(st.requests.length, 0);

    // 解析实例之后才能判断的：只读了实例清单，没有发检查请求
    for (const [extra, re] of [
      [{ instance_name: 'ar001', sql_content: 'delete from orders' }, /数据导出只允许 select \/ with 查询（SQL Server 还允许 declare \/ set 开头的只读批处理）；写操作请走上线工单/],
      [{ instance_name: 'ar001', sql_content: 'show tables' }, /数据导出只允许 select/],
      [{ instance_name: 'test-mysql-01', sql_content: 'declare @a int; select @a' }, /数据导出只允许 select \/ with 查询；写操作请走上线工单/],
      [{ instance_name: 'ar001', export_merge: true }, /export_merge（合并成一个文件）只能用于批量导出/],
      [{ instance_names: ['ar001', 'AR001'], export_merge: true }, /只能用于批量导出/],
    ]) {
      const r = await m.call('sql_check', { db_name: 'shop', sql_content: EXPORT_SQL, export: true, ...extra });
      assert.equal(r.isError, true, JSON.stringify(extra));
      assert.match(r.text, re);
    }
    assert.deepEqual(writesOf(st), [], '没有发出检查请求');

    // 上线检查不受这些导出规则影响：不带 export 的 sql_check 里 delete 照样交给服务端检查
    const del = await m.call('sql_check', { instance_name: 'test-mysql-01', db_name: 'shop', sql_content: 'delete from orders where id = 1' });
    assert.equal(del.isError, false, del.text);
    assert.deepEqual(st.byPath[CHECK], { instance_id: 2, db_name: 'shop', sql: 'delete from orders where id = 1' });
  } finally { m.stop(); server.close(); }
});

// ── submit_export_workflow 创建前自动预检：不通过 / 没能完成都不发创建请求 ──

const OPTIONS = 'GET /api/v1/sqlworkflow/options';

test('submit_export_workflow：创建前自动预检，通过才创建（单实例：选项 → 预检 → 创建）', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('submit_export_workflow', exportArgs({ instance_name: 'ar001' }));
    assert.equal(r.isError, false, r.text);
    assert.deepEqual(st.requests.filter((x) => x === OPTIONS || x === CHECK || x === CREATE), [OPTIONS, CHECK, CREATE], '先校验选项，再预检，最后创建');
    assert.deepEqual(st.byPath[CHECK], { instance_id: 11, db_name: 'shop', sql: EXPORT_SQL, export: true });
    assert.equal(st.created.length, 1);
    assert.equal(st.created[0].sql, st.byPath[CHECK].sql, '预检和创建用的是同一条 SQL');
    assert.equal(st.created[0].db_name, st.byPath[CHECK].db_name);
    assert.equal(st.created[0].instance_id, st.byPath[CHECK].instance_id);
    assert.match(r.text, /提交前预检: 通过（待导出 120 行）/);
    assert.ok(!r.text.includes('⚠ 检测到'), '没有警告就不写警告');
    // 预检用的是写操作以外的请求：除了登录，只多了一次检查请求
    assert.deepEqual(writesOf(st), [CHECK, CREATE]);
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：批量导出的预检带全部实例（一次请求），export_merge 也带给预检', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('submit_export_workflow', exportArgs({ instance_names: ['ar001', 'ar002', 'ar003'], export_merge: true, export_format: 'xlsx' }));
    assert.equal(r.isError, false, r.text);
    assert.equal(st.requests.filter((x) => x === CHECK).length, 1, '批量只发一次预检请求，不逐个实例预检');
    assert.equal(st.requests.filter((x) => x === CREATE).length, 1);
    assert.deepEqual(st.byPath[CHECK], { instance_ids: [11, 12, 13], db_name: 'shop', sql: EXPORT_SQL, export: true, export_merge: true });
    assert.deepEqual(st.created[0].instance_ids, [11, 12, 13]);
    assert.match(r.text, /提交前预检: 通过（3 个实例共 360 行待导出）/);

    // 不合并：预检里也没有 export_merge
    const nm = await m.call('submit_export_workflow', exportArgs({ instance_names: ['ar001', 'ar002'] }));
    assert.equal(nm.isError, false, nm.text);
    assert.deepEqual(st.byPath[CHECK], { instance_ids: [11, 12], db_name: 'shop', sql: EXPORT_SQL, export: true });
    assert.match(nm.text, /提交前预检: 通过（2 个实例共 240 行待导出）/);
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：预检不通过就不创建工单，报错里列出原因', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const cases = [
      [{ instance_name: 'ar001', sql_content: 'select 1 -- toomany' }, /导出数据行数\(20000\)超过阈值\(10000\)/],
      [{ instance_name: 'ar001', sql_content: 'select 1 -- denied' }, /没有 shop\.orders 的查询权限/],
      [{ instance_names: ['ar001', 'ar002'], sql_content: 'select 1 -- denied' }, /实例\[ar002\]没有 shop\.orders 的查询权限/],
      [{ instance_names: ['ar001', 'ar002'], export_merge: true, sql_content: 'select 1 -- colmismatch' }, /返回的列与第一个实例「ar001」不一致/],
      [{ instance_names: ['ar001', 'ar002'], sql_content: 'select 1 -- instonly' }, /连接失败：i\/o timeout/], // 行里都没问题，只有逐实例结果里写了某个实例不能用
    ];
    for (const [extra, re] of cases) {
      const r = await m.call('submit_export_workflow', exportArgs(extra));
      assert.equal(r.isError, true, JSON.stringify(extra));
      assert.match(r.text, /导出预检不通过，没有创建工单：/);
      assert.match(r.text, /\n {2}1\. /, '原因按序号列出');
      assert.match(r.text, re);
      assert.equal(r.text.split('\n').filter((l) => /^ {2}\d+\. /.test(l)).length, 1, '同一个原因（行里一条、逐实例结果里一条）只列一次');
      assert.match(r.text, /请按提示改好后重新提交（可以先用 sql_check（export=true）反复预检）/);
    }

    // 出错了但服务端没写原因（errormessage 是 None）：不能把 None 当原因列出来
    const nomsg = await m.call('submit_export_workflow', exportArgs({ instance_name: 'ar001', sql_content: 'select 1 -- nomsg' }));
    assert.equal(nomsg.isError, true);
    assert.match(nomsg.text, /导出预检不通过，没有创建工单：\n {2}（dbtool 没有给出具体原因，请到网页上点“检查”看）\n/);
    assert.ok(!nomsg.text.includes('None'));
    assert.equal(st.requests.filter((x) => x === CHECK).length, cases.length + 1, '每次只预检一次，不重试（多出的一次是上面 nomsg 那条）');
    assert.equal(st.requests.filter((x) => x === CREATE).length, 0, '一个创建请求都没有发');
    assert.equal(st.created.length, 0);

    // 同样的 SQL 不合并就能过：说明 export_merge 确实带给了预检；过了才创建
    const ok = await m.call('submit_export_workflow', exportArgs({ instance_names: ['ar001', 'ar002'], sql_content: 'select 1 -- colmismatch' }));
    assert.equal(ok.isError, false, ok.text);
    assert.equal(st.created.length, 1);
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：预检的报错最多列 8 条、每条最多 300 字', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('submit_export_workflow', exportArgs({ instance_name: 'ar001', sql_content: 'select 1 -- manyerrors' }));
    assert.equal(r.isError, true);
    const items = r.text.split('\n').filter((l) => /^ {2}\d+\. /.test(l));
    assert.equal(items.length, 8);
    assert.ok(items[0].length <= 2 + 3 + 300 + 1 + 2, `第一条被截断：${items[0].length}`);
    assert.ok(items[0].endsWith('…'));
    assert.match(r.text, /\n {2}…共 12 条\n/);
    assert.ok(!r.text.includes('第 9 个问题'));
    assert.equal(st.created.length, 0);
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：预检有警告照样提交，警告写进结果', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    // SQL Server 的 declare / set 批处理统计不了行数，只有一行警告
    const r = await m.call('submit_export_workflow', exportArgs({ instance_names: ['ar001', 'ar002'], sql_content: 'declare @d int = 1; select @d' }));
    assert.equal(r.isError, false, r.text);
    assert.equal(st.created.length, 1);
    assert.match(r.text, /提交前预检: 有警告\n/, '统计不了行数时不写行数');
    assert.match(r.text, /\n {2}⚠ 检测到 declare\/set 批处理语句，无法预先统计行数/);
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：预检没能完成（接口出错 / 连接断开 / 太慢）也不创建工单，不重试', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_TIMEOUT_MS: '5000' });
  try {
    // 检查接口返回错误
    st.failCheck = { status: 502, code: 'bad_gateway', message: '上游超时' };
    const bad = await m.call('submit_export_workflow', exportArgs({ instance_name: 'ar001' }));
    assert.equal(bad.isError, true);
    assert.match(bad.text, /导出预检没能完成，没有创建工单：.*HTTP 502 bad_gateway.*上游超时/);
    assert.equal(st.requests.filter((x) => x === CHECK).length, 1, '不重试');
    st.failCheck = null;

    // 连接被断开
    const hang = await m.call('submit_export_workflow', exportArgs({ instance_name: 'ar001', sql_content: 'select 1 -- hangcheck' }));
    assert.equal(hang.isError, true);
    assert.match(hang.text, /导出预检没能完成，没有创建工单：.*请求失败/);
    assert.equal(st.requests.filter((x) => x === CHECK).length, 2);
    assert.equal(st.requests.filter((x) => x === CREATE).length, 0, '预检没完成就不发创建请求');
    assert.equal(st.created.length, 0);

    // 恢复后同样的提交可以成功
    const ok = await m.call('submit_export_workflow', exportArgs({ instance_name: 'ar001' }));
    assert.equal(ok.isError, false, ok.text);
    assert.equal(st.created.length, 1);
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：预检超时时提示统计行数可能很慢，不创建工单', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_TIMEOUT_MS: '500' });
  try {
    const r = await m.call('submit_export_workflow', exportArgs({ instance_name: 'ar001', sql_content: 'select 1 -- slowcheck' }));
    assert.equal(r.isError, true);
    assert.match(r.text, /导出预检没能完成，没有创建工单（统计待导出行数的查询可能很慢：缩小范围，或调大 DBTOOL_TIMEOUT_MS）：/);
    assert.match(r.text, /请求超时/);
    assert.equal(st.requests.filter((x) => x === CREATE).length, 0);
    assert.equal(st.created.length, 0);
  } finally { m.stop(); server.close(); }
});

test('submit_export_workflow：选项校验不过就不预检；预检没有跳过的开关', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    // 选项校验在前：不过时不会去实例上统计行数
    st.options.export.allowed = false;
    const no = await m.call('submit_export_workflow', exportArgs({ instance_name: 'ar001' }));
    assert.equal(no.isError, true);
    assert.match(no.text, /缺少 export.submit/);
    st.options.export.allowed = true;
    const grp = await m.call('submit_export_workflow', exportArgs({ instance_name: 'ar001', group_name: '不存在的组' }));
    assert.equal(grp.isError, true);
    const ne = await m.call('submit_export_workflow', exportArgs({ instance_name: 'ar004' }));
    assert.equal(ne.isError, true);
    assert.equal(st.requests.filter((x) => x === CHECK).length, 0, '选项校验没过，没有发预检请求');

    // 没有开关：schema 里没有，传了也不认
    const list = await m.rpc('tools/list');
    const t = list.result.tools.find((x) => x.name === 'submit_export_workflow');
    assert.deepEqual(Object.keys(t.inputSchema.properties).filter((k) => /skip|check/i.test(k)), []);
    const forced = await m.call('submit_export_workflow', exportArgs({ instance_name: 'ar001', sql_content: 'select 1 -- toomany', skip_precheck: true, skip_check: true, precheck: false }));
    assert.equal(forced.isError, true);
    assert.match(forced.text, /导出预检不通过，没有创建工单/);
    assert.equal(st.created.length, 0);
    assert.match(t.description, /没有跳过预检的开关/);
    assert.match(t.description, /预检不通过、或者没能完成，都不创建工单/);
  } finally { m.stop(); server.close(); }
});

// ── submit_sql_workflow 创建前自动检查：不通过 / 没能完成都不发创建请求 ──

const sqlArgs = (extra) => ({ workflow_name: '加列', group_name: '生产DDL上线', db_name: 'shop', sql_content: OK_SQL, ...extra });
const numbered = (text) => text.split('\n').filter((l) => /^ {2}\d+\. /.test(l));

test('submit_sql_workflow：创建前自动检查，通过才创建（单实例：选项 → 检查 → 创建）', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('submit_sql_workflow', sqlArgs({ instance_name: 'ar001' }));
    assert.equal(r.isError, false, r.text);
    assert.deepEqual(st.requests.filter((x) => x === OPTIONS || x === CHECK || x === CREATE), [OPTIONS, CHECK, CREATE], '先校验选项，再检查，最后创建');
    assert.deepEqual(st.byPath[CHECK], { instance_id: 11, db_name: 'shop', sql: OK_SQL }, '上线检查，请求里没有 export');
    assert.equal(st.created.length, 1);
    assert.equal(st.created[0].sql, st.byPath[CHECK].sql, '检查和创建用的是同一条 SQL');
    assert.equal(st.created[0].db_name, st.byPath[CHECK].db_name);
    assert.equal(st.created[0].instance_id, st.byPath[CHECK].instance_id);
    assert.match(r.text, /\n提交前检查: 通过\n/, '单实例没有补充说明');
    assert.ok(!r.text.includes('⚠ 未配置'), '没有警告就不写警告');
    assert.deepEqual(writesOf(st), [CHECK, CREATE], '除了登录，只多了一次检查请求');
  } finally { m.stop(); server.close(); }
});

test('submit_sql_workflow：批量上线的检查带全部实例（一次请求），说明语法与规则只查了第一个实例', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const r = await m.call('submit_sql_workflow', sqlArgs({ instance_names: ['ar001', 'ar002', 'ar003'] }));
    assert.equal(r.isError, false, r.text);
    assert.equal(st.requests.filter((x) => x === CHECK).length, 1, '批量只发一次检查请求，不逐个实例检查');
    assert.equal(st.requests.filter((x) => x === CREATE).length, 1);
    assert.deepEqual(st.byPath[CHECK], { instance_ids: [11, 12, 13], db_name: 'shop', sql: OK_SQL });
    assert.deepEqual(st.created[0].instance_ids, [11, 12, 13]);
    assert.match(r.text, /提交前检查: 通过（3 个实例都能连接、库存在；语法与规则只在第一个实例（ar001）上检查）/);
  } finally { m.stop(); server.close(); }
});

test('submit_sql_workflow：检查不通过就不创建工单，报错里列出原因（含批量时有实例连不上）', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const call = (extra) => m.call('submit_sql_workflow', sqlArgs(extra));
    const refused = async (extra) => {
      const r = await call(extra);
      assert.equal(r.isError, true, JSON.stringify(extra));
      assert.match(r.text, /^错误: 提交前检查不通过，没有创建工单：\n/);
      assert.match(r.text, /\n请按提示改好后重新提交（可以先用 sql_check 反复检查）$/);
      return r.text;
    };
    // 语句有错误：带语句序号和语句开头
    const bad = await refused({ instance_name: 'ar001', sql_content: 'alter table bad' });
    assert.deepEqual(numbered(bad), ['  1. 第 1 条：列 x 不存在（alter table bad#1）']);
    // 批量时有实例连不上：行里都没问题，原因只在逐实例结果里——补上实例名
    const down = await refused({ instance_names: ['ar001', 'ar002'], sql_content: 'alter table down' });
    assert.deepEqual(numbered(down), ['  1. ar002：连接失败：i/o timeout']);
    // 既有语句错误、又有实例连不上：两条都列
    const both = await refused({ instance_names: ['ar001', 'ar002'], sql_content: 'alter table bad down' });
    assert.deepEqual(numbered(both), ['  1. 第 1 条：列 x 不存在（alter table bad down#1）', '  2. ar002：连接失败：i/o timeout']);
    // 一大批语句里只有一条出错：只列出错的那一条
    const many = await refused({ instance_name: 'ar001', sql_content: 'alter many' });
    assert.deepEqual(numbered(many), ['  1. 第 100 条：第 100 条出错（alter many#100）']);
    // 既有警告又有错误：原因里只列错误，警告不是“不通过”的原因
    const mix = await refused({ instance_name: 'ar001', sql_content: 'alter table errwarn' });
    assert.deepEqual(numbered(mix), ['  1. 第 2 条：列 x 不存在（alter table errwarn#2）']);
    // 两行一模一样的错误：同一个原因只列一次
    const dup = await refused({ instance_name: 'ar001', sql_content: 'alter table duprows' });
    assert.deepEqual(numbered(dup), ['  1. 第 1 条：列 x 不存在（alter table duprows#1）']);
    // 出错了但没写原因：给兜底说明，不能把 None 当原因
    const nomsg = await refused({ instance_name: 'ar001', sql_content: 'alter table nomsg' });
    assert.match(nomsg, /没有创建工单：\n {2}（dbtool 没有给出具体原因，请到网页上点“检查”看）\n请按提示/);
    assert.ok(!nomsg.includes('None'));
    // 语句很长：开头最多 80 个字符，换行和连续的空白折成一个空格
    const long = await refused({ instance_name: 'ar001', sql_content: 'alter table longsql' });
    const item = numbered(long)[0];
    assert.match(item, /^ {2}1\. 第 1 条：语法错误（alter table longsql x+…）$/);
    assert.equal(item.match(/（(.*)）/)[1].length, 81, '语句开头 80 个字符 + 省略号');

    assert.equal(st.requests.filter((x) => x === CHECK).length, 8, '每次只检查一次，不重试');
    assert.equal(st.requests.filter((x) => x === CREATE).length, 0, '一个创建请求都没有发');
    assert.equal(st.created.length, 0);
    // 去掉问题后同样的提交可以成功
    const ok = await call({ instance_names: ['ar001', 'ar002'] });
    assert.equal(ok.isError, false, ok.text);
    assert.equal(st.created.length, 1);
  } finally { m.stop(); server.close(); }
});

test('submit_sql_workflow：检查只有警告照样创建，警告写进结果（含基础检查的提示）', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const w = await m.call('submit_sql_workflow', sqlArgs({ instance_name: 'ar001', sql_content: 'alter table warn' }));
    assert.equal(w.isError, false, w.text);
    assert.equal(st.created.length, 1);
    assert.match(w.text, /\n提交前检查: 有警告\n {2}⚠ 第 1 条：建议加注释（alter table warn#1）\n/);

    // 没配置 goInception（来源 native）：检查结果里的提示原样写出来
    const n = await m.call('submit_sql_workflow', sqlArgs({ instance_name: 'ar001', sql_content: 'alter table nativecheck' }));
    assert.equal(n.isError, false, n.text);
    assert.equal(st.created.length, 2);
    assert.match(n.text, /\n提交前检查: 通过\n {2}⚠ 未配置 goInception：当前只做了基础检查，不校验语法、表结构与影响行数/);

    // 很长的提示最多写 300 个字
    const ln = await m.call('submit_sql_workflow', sqlArgs({ instance_name: 'ar001', sql_content: 'alter table longnotice' }));
    assert.equal(ln.isError, false, ln.text);
    const longLine = ln.text.split('\n').find((l) => l.startsWith('  ⚠ '));
    assert.equal(longLine.length, 4 + 300 + 1, '300 个字 + 省略号');
    assert.ok(longLine.endsWith('…'));
  } finally { m.stop(); server.close(); }
});

test('submit_sql_workflow：检查没能完成（接口出错 / 连接断开 / 太慢）也不创建工单，不重试', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_TIMEOUT_MS: '5000' });
  try {
    st.failCheck = { status: 500, code: 'no_inception', message: '未配置 goInception，无法检查 SQL，请联系管理员。' };
    const bad = await m.call('submit_sql_workflow', sqlArgs({ instance_name: 'ar001' }));
    assert.equal(bad.isError, true);
    assert.match(bad.text, /提交前检查没能完成，没有创建工单：.*HTTP 500 no_inception.*未配置 goInception，无法检查 SQL/);
    assert.equal(st.requests.filter((x) => x === CHECK).length, 1, '不重试');
    st.failCheck = null;

    const hang = await m.call('submit_sql_workflow', sqlArgs({ instance_name: 'ar001', sql_content: 'alter table hangcheck' }));
    assert.equal(hang.isError, true);
    assert.match(hang.text, /提交前检查没能完成，没有创建工单：.*请求失败/);
    assert.equal(st.requests.filter((x) => x === CHECK).length, 2);
    assert.equal(st.requests.filter((x) => x === CREATE).length, 0, '检查没完成就不发创建请求');
    assert.equal(st.created.length, 0);

    const ok = await m.call('submit_sql_workflow', sqlArgs({ instance_name: 'ar001' }));
    assert.equal(ok.isError, false, ok.text);
    assert.equal(st.created.length, 1);
  } finally { m.stop(); server.close(); }

  // 太慢：超时后提示检查可能很慢，不创建
  const s2 = await startStub();
  const m2 = startMcp(s2.url, { DBTOOL_TIMEOUT_MS: '500' });
  try {
    const slow = await m2.call('submit_sql_workflow', sqlArgs({ instance_name: 'ar001', sql_content: 'alter table slowcheck' }));
    assert.equal(slow.isError, true);
    assert.match(slow.text, /提交前检查没能完成，没有创建工单（检查可能很慢：SQL 很长，或批量实例多、要逐个确认能连接；可以调大 DBTOOL_TIMEOUT_MS）：/);
    assert.match(slow.text, /请求超时/);
    assert.equal(s2.st.created.length, 0);
    assert.equal(s2.st.requests.filter((x) => x === CREATE).length, 0);
  } finally { m2.stop(); s2.server.close(); }
});

test('submit_sql_workflow：选项校验不过就不检查；检查没有跳过的开关', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    // 选项校验在前：不过时不会去检查（批量检查要连每个实例）
    const grp = await m.call('submit_sql_workflow', sqlArgs({ instance_name: 'ar001', group_name: '不存在的组' }));
    assert.equal(grp.isError, true);
    const out = await m.call('submit_sql_workflow', sqlArgs({ instance_name: 'ar005' }));
    assert.equal(out.isError, true);
    st.options.batch.allowed = false;
    const nob = await m.call('submit_sql_workflow', sqlArgs({ instance_names: ['ar001', 'ar002'] }));
    assert.equal(nob.isError, true);
    assert.match(nob.text, /缺少 sqlworkflow.submit_batch/);
    assert.equal(st.requests.filter((x) => x === CHECK).length, 0, '选项校验没过，没有发检查请求');

    // 没有开关：schema 里没有，传了也不认
    const list = await m.rpc('tools/list');
    const t = list.result.tools.find((x) => x.name === 'submit_sql_workflow');
    assert.deepEqual(Object.keys(t.inputSchema.properties).filter((k) => /skip|check/i.test(k)), []);
    assert.match(t.description, /没有跳过检查的开关/);
    assert.match(t.description, /检查不通过、或者没能完成，都不创建工单/);
    const forced = await m.call('submit_sql_workflow', sqlArgs({ instance_name: 'ar001', sql_content: 'alter table bad', skip_check: true, skip_precheck: true }));
    assert.equal(forced.isError, true);
    assert.match(forced.text, /提交前检查不通过，没有创建工单/);
    assert.equal(st.created.length, 0);
  } finally { m.stop(); server.close(); }
});

test('导出预检：行里已经写了的原因，逐实例结果里的同一条不重复列出', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    // 行里是“实例[ar002]检查失败: Error 1146…”，逐实例结果里是不带前缀的同一条原因：只列一次
    const r = await m.call('submit_export_workflow', exportArgs({ instance_names: ['ar001', 'ar002'], sql_content: 'select 1 -- countfail' }));
    assert.equal(r.isError, true);
    assert.deepEqual(numbered(r.text), ["  1. 实例[ar002]检查失败: Error 1146: Table 'shop.x' doesn't exist"]);
    assert.equal(st.created.length, 0);
  } finally { m.stop(); server.close(); }
});

// ── 终止（撤回）工单：cancel_workflow ──────────────────────────────────────

const cancelPath = (id) => `POST /api/v1/sqlworkflow/${id}/cancel`;
const cancelsOf = (st) => st.requests.filter((x) => /\/cancel$/.test(x));
const listReads = (st) => st.requests.filter((x) => x.startsWith('GET /api/v1/sqlworkflow?'));

test('cancel_workflow：已经启用；终止原因必填，没有 confirm_workflow_id', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const list = await m.rpc('tools/list');
    const t = list.result.tools.find((x) => x.name === 'cancel_workflow');
    assert.ok(!t.description.includes('暂未对接'));
    assert.deepEqual(t.inputSchema.required, ['workflow_id', 'remark']);
    assert.deepEqual(Object.keys(t.inputSchema.properties).sort(), ['remark', 'workflow_id'], '按确认的方案：只要求填终止原因，不要求再传一次工单号');
    assert.match(t.description, /只能撤回【自己提交的】工单/);
    assert.match(t.description, /等待审批 \/ 审核通过 \/ 定时执行/);
    assert.match(t.description, /人工终止流程/);
    assert.match(t.description, /不能恢复/);
    assert.equal(st.requests.length, 0);
  } finally { m.stop(); server.close(); }
});

test('cancel_workflow：工单号不对 / 没写终止原因：连登录都不发', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    const cases = [
      [{ remark: '提交错了' }, /workflow_id 必须是正整数/],
      [{ workflow_id: 0, remark: '提交错了' }, /workflow_id 必须是正整数/],
      [{ workflow_id: 'abc', remark: '提交错了' }, /workflow_id 必须是正整数/],
      [{ workflow_id: 1.5, remark: '提交错了' }, /workflow_id 必须是正整数/],
      [{ workflow_id: 301 }, /缺少终止原因 remark/],
      [{ workflow_id: 301, remark: '' }, /缺少终止原因 remark/],
      [{ workflow_id: 301, remark: '   \n ' }, /缺少终止原因 remark/],
    ];
    for (const [args, re] of cases) {
      const r = await m.call('cancel_workflow', args);
      assert.equal(r.isError, true, JSON.stringify(args));
      assert.match(r.text, re);
    }
    assert.equal(st.requests.length, 0);
  } finally { m.stop(); server.close(); }
});

test('cancel_workflow：撤回自己提交的工单——请求体、备注标记、结果（上线 / 导出、单实例 / 批量、三种状态）', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_PUBLIC_URL: 'https://dbtool.example.com/' });
  try {
    const r = await m.call('cancel_workflow', { workflow_id: 301, remark: '提交错了实例' });
    assert.equal(r.isError, false, r.text);
    assert.deepEqual(st.byPath[cancelPath(301)], { remark: `提交错了实例${MARK}` });
    assert.equal(cancelsOf(st).length, 1, '只发一次终止请求');
    assert.match(r.text, /✅ SQL 上线工单（批量，3 个实例） #301「批量工单301」已终止（提交人撤回）/);
    assert.match(r.text, /状态: 人工终止流程（原状态：等待审批）/);
    assert.ok(r.text.includes(`终止原因: 提交错了实例${MARK}`));
    assert.match(r.text, /操作账号: tester/);
    assert.match(r.text, /工单链接（请转给用户核对）: https:\/\/dbtool\.example\.com\/sqlworkflow\/301/);
    assert.ok(!r.text.includes('定时执行的计划'));
    assert.equal(st.wf[301].status, 'workflow_abort');

    // 单实例上线、批量导出、单实例导出
    assert.match((await m.call('cancel_workflow', { workflow_id: 302, remark: 'x' })).text, /✅ SQL 上线工单 #302/);
    assert.match((await m.call('cancel_workflow', { workflow_id: 303, remark: 'x' })).text, /✅ 数据导出工单（批量，3 个实例） #303/);
    assert.match((await m.call('cancel_workflow', { workflow_id: 321, remark: 'x' })).text, /✅ 数据导出工单 #321/);
    // 审核通过、定时执行（定时的计划随工单一起清除）
    const rp = await m.call('cancel_workflow', { workflow_id: 304, remark: '不上了' });
    assert.match(rp.text, /状态: 人工终止流程（原状态：审核通过）/);
    assert.ok(!rp.text.includes('定时执行的计划'));
    const tm = await m.call('cancel_workflow', { workflow_id: 305, remark: '不上了' });
    assert.match(tm.text, /状态: 人工终止流程（原状态：定时执行）/);
    assert.match(tm.text, /定时执行的计划已一并清除/);
    // 提交人登录名大小写不同也认；终止原因前后的空白去掉
    const up = await m.call('cancel_workflow', { workflow_id: 314, remark: '  大小写不同  ' });
    assert.equal(up.isError, false, up.text);
    assert.equal(st.byPath[cancelPath(314)].remark, `大小写不同${MARK}`);

    // 除了登录，只有终止请求：没有别的写请求，也没有取消执行 / 取消定时 / 审批 / 执行
    assert.ok(writesOf(st).every((x) => /\/cancel$/.test(x)), writesOf(st).join(' | '));
    assert.equal(cancelsOf(st).length, 7);
    assert.ok(!st.requests.some((x) => /\/(pass|execute|schedule|cancel-run|mark-exception|run-date|osc)\b/.test(x)));
  } finally { m.stop(); server.close(); }
});

test('cancel_workflow：别人提交的工单一律拒绝，不发终止请求（dbtool 说能终止、超级管理员也一样）', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  const root = startMcp(url, { DBTOOL_USER: 'root' });
  try {
    for (const who of [m, root]) {
      const r = await who.call('cancel_workflow', { workflow_id: 311, remark: '驳回' });
      assert.equal(r.isError, true);
      assert.match(r.text, /工单 #311 是 alice 提交的，不是当前账号（(tester|root)）：cancel_workflow 只能撤回自己提交的工单；别人的工单请审批人到网页上驳回：/);
      assert.match(r.text, /\/sqlworkflow\/311/);
    }
    assert.equal(cancelsOf(st).length, 0);
    assert.equal(st.wf[311].status, 'workflow_manreviewing', '工单没有被动');
    // 超级管理员撤回自己提交的没有问题（202 是 root 自己提交的）
    const own = await root.call('cancel_workflow', { workflow_id: 202, remark: '自己撤回' });
    assert.equal(own.isError, false, own.text);
    assert.equal(cancelsOf(st).length, 1);
  } finally { m.stop(); root.stop(); server.close(); }
});

test('cancel_workflow：状态不对（排队中 / 执行中 / 已结束 / 已终止 / 自动审批不通过）直接拒绝，连提交人都不去查', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    for (const [id, label] of [[306, '执行中'], [307, '排队中'], [308, '已正常结束'], [309, '人工终止流程'], [310, '自动审批不通过']]) {
      const r = await m.call('cancel_workflow', { workflow_id: id, remark: '不要了' });
      assert.equal(r.isError, true, label);
      assert.match(r.text, new RegExp(`工单 #${id} 当前状态是「${label}」，只有「等待审批」「审核通过」「定时执行」的工单才能终止`));
      assert.match(r.text, /取消正在执行的请到网页上操作/);
    }
    assert.equal(cancelsOf(st).length, 0);
    assert.equal(listReads(st).length, 0, '状态不对时，没有去工单列表里查提交人');
  } finally { m.stop(); server.close(); }
});

test('cancel_workflow：查不到提交人 / 提交人为空 / dbtool 判定不能终止 / 工单不存在，都拒绝', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    for (const id of [313, 317]) {
      const r = await m.call('cancel_workflow', { workflow_id: id, remark: '不要了' });
      assert.equal(r.isError, true);
      assert.match(r.text, new RegExp(`无法确认工单 #${id} 的提交人，已拒绝终止`));
    }
    const no = await m.call('cancel_workflow', { workflow_id: 312, remark: '不要了' });
    assert.equal(no.isError, true);
    assert.match(no.text, /dbtool 判定当前账号不能终止工单 #312（缺少终止按钮权限，或工单状态刚变了）/);
    const gone = await m.call('cancel_workflow', { workflow_id: 999, remark: '不要了' });
    assert.equal(gone.isError, true);
    assert.match(gone.text, /HTTP 404/);
    assert.equal(cancelsOf(st).length, 0);
  } finally { m.stop(); server.close(); }
});

test('cancel_workflow：dbtool 服务端拒绝时报错、不当作成功，也不重试；连接断开提示先核对', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url, { DBTOOL_TIMEOUT_MS: '5000' });
  try {
    const refused = await m.call('cancel_workflow', { workflow_id: 315, remark: '不要了' });
    assert.equal(refused.isError, true);
    assert.match(refused.text, /HTTP 403 forbidden.*你无权操作当前工单！/);
    assert.ok(!refused.text.includes('✅'));
    assert.equal(cancelsOf(st).length, 1, '只发一次，不重试');
    assert.equal(st.wf[315].status, 'workflow_manreviewing');

    const hang = await m.call('cancel_workflow', { workflow_id: 316, remark: '不要了' });
    assert.equal(hang.isError, true);
    assert.match(hang.text, /请求失败/);
    assert.match(hang.text, /请求已经发出但没有收到响应，工单可能已经被终止：请先用 workflow_detail 核对状态，不要直接重试/);
    assert.equal(cancelsOf(st).length, 2, '连接断开后也不自动重试');

    // 请求成功了，但响应里的状态不对 / 没有状态：不能说“已经终止”而不提醒，要请你到链接里核对
    const weird = await m.call('cancel_workflow', { workflow_id: 322, remark: '不要了' });
    assert.equal(weird.isError, false, weird.text);
    assert.match(weird.text, /状态: 等待审批（原状态：等待审批）\n⚠ 响应里的状态不是「人工终止流程」，请到链接核对\n/);
    const empty = await m.call('cancel_workflow', { workflow_id: 323, remark: '不要了' });
    assert.equal(empty.isError, false, empty.text);
    assert.match(empty.text, /状态: （响应里没有新状态，请到链接核对）（原状态：等待审批）/);
    assert.ok(!empty.text.includes('⚠ 响应里的状态不是'));
  } finally { m.stop(); server.close(); }
});

test('cancel_workflow：很长的终止原因截到 500 字以内，“（经 MCP 操作）”标记不被截掉', async () => {
  const { st, server, url } = await startStub();
  const m = startMcp(url);
  try {
    await m.call('cancel_workflow', { workflow_id: 318, remark: 'x'.repeat(900) });
    const long = st.byPath[cancelPath(318)].remark;
    assert.equal(Array.from(long).length, 500, '正好 500 个字符（dbtool 的上限）');
    assert.ok(long.startsWith('x'.repeat(489)));
    assert.ok(long.endsWith(`…${MARK}`));

    await m.call('cancel_workflow', { workflow_id: 319, remark: '😀'.repeat(700) });
    const emoji = st.byPath[cancelPath(319)].remark;
    assert.ok(Array.from(emoji).length <= 500, '按字符计数');
    assert.ok(emoji.endsWith(MARK));
    assert.ok(!/[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/.test(emoji), '不会把表情符号劈开');

    // 正好装得下的原因（500 - 标记 = 490 个字符）原样保留，不加省略号
    await m.call('cancel_workflow', { workflow_id: 320, remark: 'y'.repeat(490) });
    assert.equal(st.byPath[cancelPath(320)].remark, `${'y'.repeat(490)}${MARK}`);
  } finally { m.stop(); server.close(); }
});

// ── CLI 模式 ────────────────────────────────────────────────────────────

function runCli(url, args) {
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [path.join(__dirname, 'index.js'), '--cli', ...args], {
      env: { ...process.env, DBTOOL_URL: url, DBTOOL_USER: 'tester', DBTOOL_PASS: 'pw-secret-123', DBTOOL_TIMEOUT_MS: '10000' },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let out = '';
    let err = '';
    child.stdout.setEncoding('utf8');
    child.stderr.setEncoding('utf8');
    child.stdout.on('data', (d) => { out += d; });
    child.stderr.on('data', (d) => { err += d; });
    child.on('close', (code) => resolve({ code, out, err }));
  });
}

test('CLI：export 命令（多个实例 = 批量导出；第 7 个参数 merge = 合并成一个文件）', async () => {
  const { st, server, url } = await startStub();
  try {
    const r = await runCli(url, ['export', '生产DDL上线', 'ar001,ar002', 'shop', '导出订单', EXPORT_SQL, 'xlsx', 'merge']);
    assert.equal(r.code, 0, r.err);
    assert.match(r.out, /数据导出工单已创建（批量导出，2 个实例，只有这一个工单）/);
    assert.deepEqual(st.created[0], { name: '导出订单', instance_ids: [11, 12], db_name: 'shop', group_name: '生产DDL上线', sql: EXPORT_SQL, export: true, export_format: 'xlsx', export_merge: true });

    const single = await runCli(url, ['export', '生产DDL上线', 'ar001', 'shop', '导出订单', EXPORT_SQL]);
    assert.equal(single.code, 0, single.err);
    assert.deepEqual(st.created[1], { name: '导出订单', instance_id: 11, db_name: 'shop', group_name: '生产DDL上线', sql: EXPORT_SQL, export: true, export_format: 'csv' });

    // 预检不通过：不创建工单，退出码 1
    const gated = await runCli(url, ['export', '生产DDL上线', 'ar001', 'shop', '必败导出', 'select 1 -- toomany']);
    assert.equal(gated.code, 1);
    assert.match(gated.err, /导出预检不通过，没有创建工单/);
    assert.equal(st.created.length, 2);

    st.requests.length = 0;
    const bad = await runCli(url, ['export', '生产DDL上线', 'ar001,ar002', 'shop', '导出订单', EXPORT_SQL, 'csv', 'mrege']);
    assert.equal(bad.code, 1);
    assert.match(bad.err, /第 7 个参数只能是 merge/);
    assert.equal(st.requests.length, 0, '参数不对时不访问 dbtool');
    assert.equal(st.created.length, 2);

    const help = await runCli(url, []);
    assert.match(help.out, /--cli export <group_name>/);
    assert.ok(!help.out.includes('暂未对接'));
    assert.match(help.out, /--cli cancel <workflow_id> <remark>/);
    assert.ok(!help.out.includes('submit_export_workflow'));
    assert.match(help.out, /--cli checkexport <instance_name/);
  } finally { server.close(); }
});

test('CLI：submit 命令（创建前自动检查：不通过不创建，退出码 1）', async () => {
  const { st, server, url } = await startStub();
  try {
    const ok = await runCli(url, ['submit', '生产DDL上线', 'ar001,ar002', 'shop', '批量加列', OK_SQL]);
    assert.equal(ok.code, 0, ok.err);
    assert.match(ok.out, /SQL 上线工单已创建（批量工单，2 个实例，只有这一个工单）/);
    assert.match(ok.out, /提交前检查: 通过（2 个实例都能连接、库存在/);
    assert.deepEqual(st.byPath[CHECK], { instance_ids: [11, 12], db_name: 'shop', sql: OK_SQL });
    assert.equal(st.created.length, 1);

    const bad = await runCli(url, ['submit', '生产DDL上线', 'ar001', 'shop', '必败', 'alter table bad']);
    assert.equal(bad.code, 1);
    assert.match(bad.err, /提交前检查不通过，没有创建工单：/);
    assert.match(bad.err, /第 1 条：列 x 不存在/);
    assert.equal(st.created.length, 1);
    assert.equal(st.requests.filter((x) => x === CREATE).length, 1, '被拦下的那次没有发创建请求');
  } finally { server.close(); }
});

test('CLI：cancel 命令（终止原因必填；只能撤回自己提交的）', async () => {
  const { st, server, url } = await startStub();
  try {
    const ok = await runCli(url, ['cancel', '301', '提交错了']);
    assert.equal(ok.code, 0, ok.err);
    assert.match(ok.out, /已终止（提交人撤回）/);
    assert.deepEqual(st.byPath[cancelPath(301)], { remark: `提交错了${MARK}` });

    const others = await runCli(url, ['cancel', '311', '驳回']);
    assert.equal(others.code, 1);
    assert.match(others.err, /不是当前账号/);
    assert.equal(cancelsOf(st).length, 1);

    st.requests.length = 0;
    const bare = await runCli(url, ['cancel', '302']);
    assert.equal(bare.code, 1);
    assert.match(bare.err, /缺少终止原因 remark/);
    assert.equal(st.requests.length, 0, '没写终止原因时不访问 dbtool');
  } finally { server.close(); }
});

test('CLI：checkexport 命令（多个实例 = 批量导出预检；第 4 个参数 merge = 核对列是否一致）', async () => {
  const { st, server, url } = await startStub();
  try {
    const r = await runCli(url, ['checkexport', 'ar001,ar002', 'shop', EXPORT_SQL, 'merge']);
    assert.equal(r.code, 0, r.err);
    const out = JSON.parse(r.out);
    assert.equal(out.verdict, '通过');
    assert.match(out.mode, /批量导出（2 个实例.*合并成一个文件）/);
    assert.deepEqual(st.byPath[CHECK], { instance_ids: [11, 12], db_name: 'shop', sql: EXPORT_SQL, export: true, export_merge: true });

    const single = await runCli(url, ['checkexport', 'ar001', 'shop', EXPORT_SQL]);
    assert.equal(single.code, 0, single.err);
    assert.deepEqual(st.byPath[CHECK], { instance_id: 11, db_name: 'shop', sql: EXPORT_SQL, export: true });
    assert.equal(JSON.parse(single.out).mode, '单实例导出');

    st.requests.length = 0;
    const bad = await runCli(url, ['checkexport', 'ar001,ar002', 'shop', EXPORT_SQL, 'mrege']);
    assert.equal(bad.code, 1);
    assert.match(bad.err, /第 4 个参数只能是 merge/);
    assert.equal(st.requests.length, 0, '参数不对时不访问 dbtool');
    assert.equal(st.created.length, 0);
  } finally { server.close(); }
});
