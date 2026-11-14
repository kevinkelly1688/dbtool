/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: partman_fake_backend.mjs
 * @time: 2026/10/28
 */
// 分区维护页面测试用的静态服务 + 假后端（接口形状以 docs/partman-merge-api.md 为准）。
// 页面模板直接取自 web/templates/partman/*.html（去掉 Go 模板的 define 包装），脚本取自 web/static。
// 控制接口：GET /__reqs 取全部请求记录；POST /__ctl 修改假后端行为；POST /__reset 重置。
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { untranslate } from './tpl.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '../..');
const staticRoot = path.join(root, 'web/static');
// 比应用的 CSP 更严：style-src 去掉 unsafe-inline，以便发现内联样式
export const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'";
const MIME = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8', '.md': 'text/plain; charset=utf-8' };

function block(src, name) {
  const open = `{{define "${name}"}}`;
  const i = src.indexOf(open);
  if (i < 0) return '';
  const rest = src.slice(i + open.length);
  const j = rest.indexOf('{{end}}\n\n{{define');
  const k = rest.lastIndexOf('{{end}}');
  const end = j >= 0 ? j : k;
  return rest.slice(0, end >= 0 ? end : rest.length);
}

function page(file, title) {
  const src = untranslate(fs.readFileSync(path.join(root, 'web/templates/partman', file), 'utf8'));
  const head = (src.match(/\{\{define "head"\}\}(.*?)\{\{end\}\}/s) || [, ''])[1];
  const content = block(src, 'content');
  const scripts = (src.match(/\{\{define "scripts"\}\}(.*?)\{\{end\}\}/s) || [, ''])[1];
  return `<!doctype html><html lang="zh-CN" data-skin="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="csrf-token" content="t"><title>${title}</title>
<link rel="stylesheet" href="/static/css/app.css"><link rel="stylesheet" href="/static/css/admin.css"><link rel="stylesheet" href="/static/css/shell.css">${head}</head>
<body><div class="app"><div class="main">${content}</div></div>${scripts}</body></html>`;
}

const now = '2026-10-10 12:00:00';
const INSTANCES = [
  { id: 1, name: 'mysql-prod', db_type: 'mysql', supported: true, dialect: 'mysql', can_init: true },
  { id: 2, name: 'sdb-01', db_type: 'selectdb', supported: true, dialect: 'doris', can_init: false },
  { id: 3, name: 'mssql-1', db_type: 'mssql', supported: false, dialect: '', can_init: false },
];

function freshState() {
  return {
    reqs: [],
    ctl: {},
    configs: [
      { id: 1, cnf_id: 1001, instance_id: 1, instance_name: 'mysql-prod', db_type: 'mysql', dialect: 'mysql', can_init: true, supported: true, dbname: 'order_db', tbname: 'order_event', column: 'created_at', col_type: 'datetime', period: 'day', interval: 1, retain: 30, pre_create: 3, timezone: 'Asia/Shanghai', isenable: true, env: 'sit', last_run: '' },
      { id: 2, cnf_id: 1002, instance_id: 1, instance_name: 'mysql-prod', db_type: 'mysql', dialect: 'mysql', can_init: true, supported: true, dbname: 'log', tbname: 'access_log', column: 'created_at', col_type: 'datetime', period: 'day', interval: 1, retain: 30, pre_create: 2, timezone: '', isenable: true, env: '', last_run: '' },
      { id: 3, cnf_id: 1003, instance_id: 2, instance_name: 'sdb-01', db_type: 'selectdb', dialect: 'doris', can_init: false, supported: true, dbname: 'dw', tbname: 'ods_event', column: 'ts', col_type: 'bigint', period: 'day', interval: 1, retain: 7, pre_create: 3, timezone: 'UTC+8', isenable: true, env: '', last_run: '' },
      { id: 4, cnf_id: 1004, instance_id: 3, instance_name: 'mssql-1', db_type: 'mssql', dialect: '', can_init: false, supported: false, dbname: 'x', tbname: 'y', column: 'z', col_type: 'int', period: 'day', interval: 1, retain: 1, pre_create: 1, timezone: 'UTC', isenable: false, env: '', last_run: '' },
    ],
    hooks: [
      { id: 1, type: 'dingtalk', webhook_url: 'https://oapi.dingtalk.com/robot/send?access_token=****', isenabled: true, mention_on: 'on_failure', mention: ['13800000000', 'all'], keyword: '分区', secret_set: true, create_time: now },
      { id: 2, type: 'slack', webhook_url: 'https://hooks.slack.com/services/****', isenabled: false, mention_on: 'never', mention: [], keyword: '', secret_set: false, create_time: now },
    ],
    nextHook: 3,
    initDone: false,
  };
}
export let S = freshState();

// ---- 运行输出（trace）假数据：行形状 {t, l, m}，与 internal/partman/trace.go 的 traceLine 一致 ----
const line = (t, l, m) => ({ time: `2026-10-10 12:00:${t}`, level: l, message: m });
export function manualTrace() {
  return [
    line('00.012', 'INFO', '开始维护 order_db.order_event（手动，操作人 alice）'),
    line('00.020', 'INFO', '读取现有分区：30 个，最后边界 2026-10-10 00:00:00'),
    line('00.031', 'WARN', '时区 America/New_York 有夏令时：换季日前后按日的分区边界会漂移'),
    line('00.044', 'INFO', '计划：新增 1，删除 1'),
    line('00.120', 'ERROR', '执行失败：Error 1503 <img src=x onerror="window.__xss=1">'),
  ];
}
export function longTrace(n = 120) {
  const out = [line('00.000', 'INFO', '…已截断 37 行（运行输出超出 300 行 / 40KB 上限，先删较早的 INFO）')];
  for (let i = 0; i < n; i++) out.push(line(String(i % 60).padStart(2, '0') + '.' + String(100 + i).slice(-3), i % 25 === 0 ? 'WARN' : 'INFO', `第 ${i} 项检查：分区 p2026${String(1000 + i).slice(-4)} 边界一致，已存在，跳过 ${'x'.repeat(i % 7 === 0 ? 120 : 10)}`));
  return out;
}
const autoTrace = () => [
  line('00.001', 'INFO', '定时维护开始：可见配置 3 个'),
  line('00.050', 'INFO', 'cnf=1001 order_db.order_event：计划 新增 1 / 删除 0'),
  line('00.070', 'WARN', 'cnf=1002 log.access_log：未分区，定时维护不会初始化，已跳过'),
  line('00.090', 'ERROR', 'cnf=1003 dw.ods_event：读取目标表失败 connection refused'),
  line('00.100', 'INFO', '推送：dingtalk 发送成功（https://oapi.dingtalk.com/robot/send?access_token=****）'),
  line('00.110', 'INFO', '定时维护结束：成功 1 / 失败 1 / 跳过 1'),
];
export const LOGS = [
  { id: 11, cnf_id: 1001, config_id: 1, instance_name: 'mysql-prod', table: 'order_db.order_event', scope: 'config', create_time: '2026-10-10 12:00:00', source: 'manual', operator: 'alice', status: 'failed', items: [{ sql: 'ALTER TABLE `order_db`.`order_event` DROP PARTITION `p20260801`', status: 'failed', error: 'Error 1503' }], output_lines: 5, trace: manualTrace() },
  { id: 12, cnf_id: 1001, config_id: 1, instance_name: 'mysql-prod', table: 'order_db.order_event', scope: 'config', create_time: '2026-10-09 12:00:00', source: 'external', operator: '', status: 'unknown', items: [{ sql: 'ALTER TABLE x ADD PARTITION (PARTITION p1 VALUES LESS THAN (1))', status: '' }], output_lines: 0, trace: [] },
  { id: 13, cnf_id: 1001, config_id: 1, instance_name: 'mysql-prod', table: 'order_db.order_event', scope: 'config', create_time: '2026-10-08 12:00:00', source: 'auto', operator: 'system', status: 'success', items: [{ sql: 'ALTER TABLE y ADD PARTITION (PARTITION p2 VALUES LESS THAN (2))', status: 'success' }], output_lines: 121, trace: longTrace(120) },
  { id: 21, cnf_id: 0, config_id: 0, instance_name: '', table: '（定时维护轮次 / 推送测试）', scope: 'system', kind: 'auto_round', create_time: '2026-10-10 11:00:00', source: 'auto', operator: 'system', status: 'unknown', items: [], output_lines: 6, trace: autoTrace(),
    round: { checked: 3, executed: 1, nothing: 0, skipped: 1, failed: 1, guard_minutes: 30, auto_drop: false, notify_on: 'always', items_cut: 2,
      items: [{ cnf_id: 1001, table: 'order_db.order_event', result: 'executed', reason: '1 条语句成功' }, { cnf_id: 1002, table: 'log.access_log', result: 'skipped', reason: '表未分区：定时维护不会初始化，请在页面上手动“初始化为分区表”' }, { cnf_id: 1003, table: 'dw.ods_event', result: 'failed', reason: '计划失败：读取目标表失败' }] } },
  { id: 22, cnf_id: 0, config_id: 0, instance_name: '', table: '（定时维护轮次 / 推送测试）', scope: 'system', kind: 'webhook_test', create_time: '2026-10-10 10:00:00', source: 'manual', operator: 'alice', status: 'unknown', items: [], output_lines: 2,
    trace: [line('00.001', 'INFO', '推送测试开始：1 个通道'), line('00.200', 'INFO', '推送 dingtalk https://oapi.dingtalk.com/robot/send?access_token=**** 成功（HTTP 200）')] },
];
export function planTrace() {
  return [line('00.001', 'INFO', '检查 order_db.order_event：存在、已分区（RANGE COLUMNS(created_at)）'), line('00.002', 'INFO', '时区 Asia/Shanghai，当前 2026-10-10 12:00:00'), line('00.003', 'WARN', '保留数 30：将删除 1 个历史分区')];
}


function initPlan(body, cfg) {
  const opt = body.init || { start_mode: 'current', start_date: '' };
  if (opt.start_mode === 'earliest' && S.ctl.earliestFail) return { err: [409, 'earliest_unavailable', '读取最早数据时间超时（20 秒），请改用指定起始日期'] };
  if (opt.start_mode === 'date' && !/^\d{4}-\d{2}-\d{2}/.test(opt.start_date || '')) return { err: [400, 'bad_init_option', '起始日期格式应为 YYYY-MM-DD'] };
  const count = opt.start_mode === 'earliest' ? 40 : opt.start_mode === 'date' ? 12 : 3;
  const first = opt.start_mode === 'earliest' ? 'p20250901' : opt.start_mode === 'date' ? 'p20260929' : 'p20261010';
  const last = 'p' + (20261010 + count - 1);
  const bound = '2026-10-' + String(10 + count) + ' 00:00:00';
  const checks = [
    { code: 'engine', level: 'ok', message: '存储引擎 InnoDB 支持分区' },
    { code: 'unique_keys', level: 'ok', message: '唯一键均包含分区字段 created_at（主键 PRIMARY）' },
    { code: 'partition_count', level: count > 30 ? 'warn' : 'ok', message: `将创建 ${count} 个分区` },
  ];
  if (S.ctl.checkError) checks.push({ code: 'foreign_key', level: 'error', message: '表上有外键，MySQL 分区表不支持外键' });
  const can = !S.ctl.checkError;
  let sql = `ALTER TABLE \`log\`.\`access_log\` PARTITION BY RANGE COLUMNS(\`created_at\`) (PARTITION \`${first}\` VALUES LESS THAN ('2026-10-11 00:00:00'), …, PARTITION \`${last}\` VALUES LESS THAN ('${bound}'))`;
  if (S.ctl.longSql) sql = sql.slice(0, 200) + Array.from({ length: 300 }, (_, i) => `, PARTITION \`p2026${String(i).padStart(4, '0')}\` VALUES LESS THAN ('2026-12-31 00:00:00')`).join('') + sql.slice(-80);
  return {
    plan: {
      kind: 'init', dialect: 'mysql', timezone: 'UTC+08:00', table: 'log.access_log', now, method: '', expression: '', existing: [],
      statements: can ? [{ kind: 'init', partitions: [first, last], bound, sql, danger: true, heavy: true }] : [],
      drops: [], reorganize: false,
      warnings: opt.start_mode === 'current' ? ['存量数据晚于最后一个分区上界时 ALTER 会失败'] : [],
      init: {
        start_mode: opt.start_mode, start_date: opt.start_date || '', start: opt.start_mode === 'date' ? opt.start_date + ' 00:00:00' : '2026-10-10 00:00:00',
        partition_count: can ? count : 0, first_partition: first, last_partition: last, last_bound: bound,
        earliest_data: opt.start_mode === 'earliest' ? '2025-09-01 08:30:12' : '', can_execute: can, checks,
        notes: opt.start_mode === 'earliest' ? [] : [],
        confirm: { table: 'log.access_log', partition_count: count, first_partition: first, last_partition: last },
      },
    },
    count,
  };
}

function maintainPlan(cfg) {
  const doris = cfg.dialect === 'doris';
  return {
    kind: 'maintain', dialect: cfg.dialect || 'mysql', timezone: doris ? 'UTC+08:00' : 'Asia/Shanghai', table: `${cfg.dbname}.${cfg.tbname}`, now,
    method: doris ? 'RANGE' : 'RANGE COLUMNS', expression: cfg.column,
    existing: [{ name: 'p20261008', bound: '2026-10-09 00:00:00' }, { name: 'p20261009', bound: '2026-10-10 00:00:00' }].concat(doris ? [{ name: 'pbad', bound: '未知', unknown: true }] : []),
    statements: [
      { kind: 'add', partitions: ['p20261010'], bound: '2026-10-11 00:00:00', sql: doris ? 'ALTER TABLE dw.ods_event ADD PARTITION p20261010 VALUES [("1791561600000"), ("1791648000000"))' : 'ALTER TABLE `order_db`.`order_event` ADD PARTITION (PARTITION `p20261010` VALUES LESS THAN (\'2026-10-11 00:00:00\'))' },
      { kind: 'drop', partitions: ['p20260801'], bound: '2026-08-02 00:00:00', sql: 'ALTER TABLE `order_db`.`order_event` DROP PARTITION `p20260801`' },
    ],
    drops: ['p20260801'], reorganize: false,
    warnings: doris ? ['分区 pbad 的上界读不到，视为未知，不会被删除'] : [],
  };
}

function json(res, status, payload, extra = {}) {
  res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store', ...extra });
  res.end(JSON.stringify(payload));
}
const fail = (res, st, code, message, data) => json(res, st, { code, message, data });
const ok = (res, data, extra) => json(res, 200, { data, ...(extra || {}) });

const PLATFORM_HOSTS = {
  slack: ['hooks.slack.com'],
  wecom: ['qyapi.weixin.qq.com'],
  feishu: ['open.feishu.cn', 'open.larksuite.com'],
  dingtalk: ['oapi.dingtalk.com'],
};
function mask(u) { return u.replace(/(access_token=|key=|\/services\/|hook\/).*$/, '$1****'); }
function hookView(h) { const { secret, ...v } = h; return v; }

function readBody(req) {
  return new Promise((r) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => { const t = Buffer.concat(chunks).toString('utf8'); try { r(t ? JSON.parse(t) : {}); } catch (_) { r({}); } });
  });
}

export const extraRoutes = []; // 其它测试脚本可挂更多路由：(req,res,url,body)=>boolean

async function api(req, res, url) {
  const body = req.method === 'GET' ? {} : await readBody(req);
  S.reqs.push({ method: req.method, path: url.pathname, query: Object.fromEntries(url.searchParams), body });
  const p = url.pathname.replace('/api/v1/partman', '');
  for (const f of extraRoutes) if (await f(req, res, p, body, url)) return;
  let m;
  if (p === '/overview' && req.method === 'GET') {
    return ok(res, {
      instances: INSTANCES, col_types: ['int', 'bigint', 'datetime', 'date'], periods: ['minute', 'hour', 'day', 'week', 'month', 'year'],
      timezones: ['Asia/Shanghai', 'UTC', 'Asia/Tokyo', 'Asia/Singapore', 'America/New_York', 'America/Los_Angeles', 'Europe/London', 'Europe/Berlin', '+08:00'],
      timezone_hint: { max_len: 20 }, notify_on: S.ctl.notifyOn || 'always', can_webhook: S.ctl.noWebhook ? false : true,
      init: { max_partitions: 5000, warn_partitions: 200, start_modes: ['current', 'earliest', 'date'] },
      can_add: true, can_edit: true, can_run: true, can_delete: S.ctl.noDelete ? false : true,
      auto: { enabled: true, drop: false, interval_minutes: 180 },
    });
  }
  if (p === '/summary') return ok(res, { total: S.configs.length, enabled: 3, supported: 3, truncated: false, items: S.configs.map((c) => ({ id: c.id, isenable: c.isenable, supported: c.supported })) });
  if (p === '/configs' && req.method === 'GET') return json(res, 200, { data: S.configs.map((c) => (c.id === 1 && S.ctl.disable1 ? { ...c, isenable: false } : c)), total: S.configs.length });
  if (p === '/configs' && req.method === 'POST') return ok(res, { id: 9, warnings: S.ctl.saveWarn ? ['时区 America/New_York 有夏令时：换季日前后按日的分区边界在 UTC 上会漂移 1 小时。建议改用固定偏移（如 -05:00）'] : undefined });
  if ((m = p.match(/^\/configs\/(\d+)\/status$/))) {
    const c = S.configs.find((x) => x.id === +m[1]);
    if (c.id === 2 && !S.initDone) return ok(res, { id: 2, state: 'unpartitioned', adds: 0, drops: 0, partitions: 0, can_init: !S.ctl.checkError, dialect: 'mysql', message: S.ctl.checkError ? '不能初始化：表上有外键' : '表尚未分区，可在“预览 / 执行”里初始化为分区表', checked_at: now });
    if (c.id === 4) return ok(res, { id: 4, state: 'unsupported', adds: 0, drops: 0 });
    if (c.id === 3) return ok(res, { id: 3, state: 'pending', adds: 1, drops: 0, partitions: 5, dialect: 'doris', checked_at: now });
    return ok(res, { id: c.id, state: 'pending', adds: 1, drops: 1, partitions: 30, dialect: 'mysql', checked_at: now });
  }
  if (p === '/preview' && req.method === 'POST') {
    const cfg = body.id ? S.configs.find((x) => x.id === body.id) : { id: 0, dbname: body.draft.dbname, tbname: body.draft.tbname, column: body.draft.column, dialect: 'mysql' };
    if (S.ctl.previewFail) return fail(res, 502, 'target_error', '读取目标表失败：connection refused');
    if (S.ctl.previewErr) return fail(res, S.ctl.previewErr.status || 409, S.ctl.previewErr.code, S.ctl.previewErr.message);
    if (cfg.id === 2 && !S.initDone) {
      const r = initPlan(body, cfg);
      if (r.err) return fail(res, r.err[0], r.err[1], r.err[2]);
      r.plan.output = { lines: planTrace(), truncated: false };
      return ok(res, r.plan);
    }
    const plan = maintainPlan(cfg);
    plan.output = S.ctl.noTrace ? null : { lines: planTrace(), truncated: false };
    if (body.draft) plan.warnings.push('时区 America/New_York 有夏令时：换季日前后按日的分区边界会漂移，建议改用固定偏移');
    return ok(res, plan);
  }
  if ((m = p.match(/^\/configs\/(\d+)\/run$/))) {
    const id = +m[1];
    if (S.ctl.runErr) return fail(res, S.ctl.runErr.status || 409, S.ctl.runErr.code, S.ctl.runErr.message);
    if (id === 2) {
      if (!body.confirm_init || !body.init) return fail(res, 409, 'confirm_required', '需要确认初始化');
      const r = initPlan(body, S.configs[1]);
      if (r.err) return fail(res, r.err[0], r.err[1], r.err[2]);
      if (S.ctl.mismatch) {
        S.ctl.mismatch = false;
        const plan = r.plan;
        plan.init.partition_count = 4;
        plan.init.confirm.partition_count = 4;
        plan.init.last_partition = 'p20261013';
        plan.init.confirm.last_partition = 'p20261013';
        return fail(res, 409, 'confirm_required', '目标表已变化：分区个数与确认值不一致，请核对新计划', plan);
      }
      if (S.ctl.checkError) return fail(res, 409, 'init_precheck_failed', '初始化前置检查不通过：表上有外键', r.plan);
      const c = body.confirm_init;
      if (c.table !== 'log.access_log' || c.partition_count !== r.count) return fail(res, 409, 'confirm_required', '确认值与服务端重算结果不一致', r.plan);
      if (S.ctl.runFail) return ok(res, { failed: 1, notice: '', statements: [{ kind: 'init', sql: r.plan.statements[0].sql, status: 'failed', error: 'Error 1503: A PRIMARY KEY must include all columns in the table\'s partitioning function', elapsed_ms: 31 }] });
      S.initDone = true;
      return ok(res, { failed: 0, log_id: 31, output: { lines: [line('01.000', 'INFO', '开始初始化 log.access_log'), line('01.842', 'INFO', 'ALTER 成功，用时 842ms')], truncated: false }, statements: [{ kind: 'init', sql: r.plan.statements[0].sql, status: 'success', elapsed_ms: 842 }] });
    }
    const plan = maintainPlan(S.configs.find((x) => x.id === id));
    if (JSON.stringify(body.confirm_drops) !== JSON.stringify(plan.drops)) return fail(res, 409, 'confirm_required', '确认的删除列表与服务端重算不一致', plan);
    return ok(res, { failed: 0, statements: plan.statements.map((s) => ({ kind: s.kind, sql: s.sql, status: 'success', elapsed_ms: 12 })) });
  }
  if (p === '/webhooks' && req.method === 'GET') return ok(res, { items: S.hooks.map(hookView), notify_on: S.ctl.notifyOn || 'always', notify_on_options: ['always', 'on_failure', 'never'] });
  if (p === '/webhooks' && req.method === 'POST') {
    // 与 internal/partman/push.go checkWebhookURL 一致：主机必须是该通道类型的平台域名，且 https；拒绝时文案列出允许的域名
    const hosts = PLATFORM_HOSTS[body.type] || [];
    let host = '';
    try { host = new URL(body.webhook_url || '').hostname; } catch (_) { /* 地址不合法 */ }
    if (!hosts.includes(host)) return fail(res, 400, 'webhook_host_not_allowed', `webhook 地址的主机 ${host} 不在 ${body.type} 通道允许的范围内（允许：${hosts.join('、')}）`);
    if (!(body.webhook_url || '').startsWith('https://')) return fail(res, 400, 'webhook_host_not_allowed', `${host} 的 webhook 地址必须使用 https`);
    if (S.hooks.some((h) => h.raw === body.webhook_url)) return fail(res, 409, 'duplicate', '该地址已存在');
    const h = { id: S.nextHook++, type: body.type, raw: body.webhook_url, webhook_url: mask(body.webhook_url), isenabled: !!body.isenabled, mention_on: body.mention_on, mention: body.mention || [], keyword: body.keyword || '', secret: body.secret || '', secret_set: !!body.secret, create_time: now };
    S.hooks.push(h);
    return ok(res, hookView(h));
  }
  if ((m = p.match(/^\/webhooks\/(\d+)$/))) {
    const h = S.hooks.find((x) => x.id === +m[1]);
    if (!h) return fail(res, 404, 'not_found', '通道不存在');
    if (req.method === 'DELETE') { S.hooks = S.hooks.filter((x) => x !== h); return ok(res, {}); }
    if (req.method === 'PUT') {
      if (body.webhook_url) { h.raw = body.webhook_url; h.webhook_url = mask(body.webhook_url); }
      Object.assign(h, { type: body.type, isenabled: !!body.isenabled, mention_on: body.mention_on, mention: body.mention || [], keyword: body.keyword || '' });
      if (body.clear_secret) { h.secret = ''; h.secret_set = false; } else if (body.secret) { h.secret = body.secret; h.secret_set = true; }
      return ok(res, hookView(h));
    }
  }
  if ((m = p.match(/^\/webhooks\/(\d+)\/enable$/))) {
    const h = S.hooks.find((x) => x.id === +m[1]);
    h.isenabled = !!body.enable;
    return ok(res, hookView(h));
  }
  if (p === '/webhooks/test') {
    const list = body.id ? S.hooks.filter((h) => h.id === body.id) : S.hooks.filter((h) => h.isenabled);
    if (!list.length) return fail(res, 409, 'no_channel', '没有可用的推送通道');
    return ok(res, { notify_on: 'always', log_id: 88, results: list.map((h, i) => (h.type === 'slack' || S.ctl.testFail ? { id: h.id, type: h.type, webhook_url: h.webhook_url, ok: false, status: 0, message: '请求失败：连接超时' } : { id: h.id, type: h.type, webhook_url: h.webhook_url, ok: true, status: 200, message: '发送成功' })) });
  }
  const view = (l) => {
    const { trace, ...v } = l;
    const o = { kind: 'maintenance', ...v };
    if (S.ctl.inlineOutput && l.id === 11) o.output = { lines: trace, truncated: false };
    return o;
  };
  if (p === '/logs' && req.method === 'GET') {
    let rows = LOGS;
    const sc = url.searchParams.get('scope');
    const src = url.searchParams.get('source');
    if (sc === 'system') rows = rows.filter((l) => l.scope === 'system');
    else if (src) rows = rows.filter((l) => l.source === src && l.scope !== 'system');
    return json(res, 200, { data: rows.map(view), total: rows.length, configs: S.configs.map((c) => ({ id: c.id, cnf_id: c.cnf_id, instance_name: c.instance_name, table: `${c.dbname}.${c.tbname}` })) });
  }
  if ((m = p.match(/^\/logs\/(\d+)$/))) {
    const l = LOGS.find((x) => x.id === +m[1]);
    if (!l) return fail(res, 404, 'not_found', '日志不存在');
    return ok(res, { ...view(l), output: { lines: l.trace, truncated: l.trace.some((x) => x.message.startsWith('…已截断')) } });
  }
  if ((m = p.match(/^\/logs\/(\d+)\/output$/))) {
    const l = LOGS.find((x) => x.id === +m[1]) || { trace: [] };
    res.writeHead(200, { 'Content-Type': 'text/plain; charset=utf-8', 'Content-Disposition': 'attachment; filename="partman_1001_20261010_120000.log"' });
    return res.end(l.trace.map((x) => `${x.time} [${x.level.padEnd(5)}] ${x.message}`).join('\n') + '\n');
  }
  if (/^\/instances\/\d+\/(databases|tables|columns)$/.test(p)) return ok(res, []);
  if ((m = p.match(/^\/configs\/(\d+)\/logs$/))) return ok(res, LOGS.filter((l) => l.config_id === +m[1]).map(view));
  return fail(res, 404, 'not_found', '未实现的假接口 ' + p);
}

export function startServer(port) {
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://x');
    try {
      if (url.pathname.startsWith('/api/v1/partman')) return await api(req, res, url);
      if (url.pathname === '/__reqs') return json(res, 200, S.reqs);
      if (url.pathname === '/__ctl' && req.method === 'POST') { Object.assign(S.ctl, await readBody(req)); return json(res, 200, S.ctl); }
      if (url.pathname === '/__reset') { S = freshState(); return json(res, 200, {}); }
      if (url.pathname === '/partman') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP }); return res.end(page('index.html', '分区维护')); }
      if (url.pathname === '/partman/logs') { res.writeHead(200, { 'Content-Type': MIME['.html'], 'Content-Security-Policy': CSP }); return res.end(page('logs.html', '维护日志')); }
      if (url.pathname.startsWith('/static/')) {
        const file = path.join(staticRoot, decodeURIComponent(url.pathname.slice(8)));
        if (file.startsWith(staticRoot) && fs.existsSync(file)) {
          res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream', 'Content-Security-Policy': CSP, 'Cache-Control': 'no-store' });
          return fs.createReadStream(file).pipe(res);
        }
      }
      res.writeHead(404); res.end('nf');
    } catch (e) {
      res.writeHead(500); res.end(String(e && e.stack));
    }
  });
  return new Promise((r) => server.listen(port, '127.0.0.1', () => r(server)));
}
