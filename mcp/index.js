#!/usr/bin/env node
/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: index.js
 * @time: 2026/10/28
 */
'use strict';
/**
 * dbtool MCP server（零依赖，仅用 Node 内置 https / http）
 *
 * 目标平台：dbtool。共 23 个工具：查询、分析、工单（检查 / 提交 / 审批 / 执行 / 终止）。
 * 走 dbtool 的 JSON 接口 + JWT（Authorization: Bearer），不需要会话 cookie 和 CSRF。
 *
 * 双模式：
 *   MCP stdio（默认）  配在 ~/.claude.json，Claude Code 调用
 *   CLI（加 --cli）     node index.js --cli instances [kw]，用于本地验证与手工排查
 *
 * 当前状态：23 个工具全部对接，没有占位。
 *   常规（21 个）
 *                    查询组：list_instances / list_databases / list_tables / list_columns / describe_table /
 *                    sql_query / query_log / slow_log / slow_log_detail / dict_table_info / partman_list
 *                    分析组：sql_explain / sql_optimize / sql_analyze
 *                    工单组（只读，支持批量工单）：sql_check（含数据导出预检 export=true）/ list_workflows / workflow_detail / workflow_log
 *                    工单组（会创建真实工单，支持批量工单）：submit_sql_workflow / submit_export_workflow（创建前都会自动检查，不通过就不创建）
 *                    工单组（会终止工单，不能恢复）：cancel_workflow——只能撤回自己提交的，只接受等待审批 / 审核通过 / 定时执行，必须填终止原因
 *   设计要求：SQL Server 批量上线 / 批量导出用 instance_names 一次提交成【一个】批量工单
 *                    （POST /sqlworkflow 的 instance_ids），不能为每个实例各提交一个工单，也没有“逐实例循环提交”的回退。
 *                    sql_check、submit_sql_workflow、submit_export_workflow 都按这个方式对接：instance_names（≥ 2 个）→ 一次请求带全部 instance_ids。
 *   高危（2 个）  approve_workflow / execute_workflow：批量审核通过 / 批量执行，只接受超级管理员账号、只接受批量工单、
 *                    每次调用前重新登录核对 su、必须再传 confirm_workflow_id、提交人是自己的工单一律拒绝审核通过。见“高危”一节的注释。
 *
 * 认证：POST /api/v1/auth/login {username,password} → {token}；之后 Authorization: Bearer <token>。
 *       令牌有效期由 dbtool 的 security.jwt_hours 决定（默认 4 小时）：到期前 60 秒主动续登，收到 401 再续登一次重试。
 *       Bearer 调用不需要 CSRF；账号需在 API 白名单（security.api_user_ids，为空 = 不限制）内。
 * 连接：环境变量 DBTOOL_URL / DBTOOL_USER / DBTOOL_PASS / DBTOOL_TIMEOUT_MS，
 *       CLI 模式缺省回退读 ~/.claude.json 的 mcpServers.<节点名>.env（节点名默认 dbtool，可用 DBTOOL_MCP_NODE 改）。
 *       自签名证书的内网部署优先用 NODE_EXTRA_CA_CERTS 信任 CA；实在不行才设 DBTOOL_INSECURE_TLS=1（不校验证书）。
 *
 * 接口对照（均在 /api/v1 下，响应成功为 {data: …}，失败为 {code, message}）：
 *   实例 / 库 / 表 / 字段   GET  /query/instances[/:id/databases[/:db/tables[/:table/columns]]]
 *   在线查询 / 执行计划     POST /query/execute {instance_id, db, sql, limit, reason}
 *   查询历史               GET  /query/history?q&instance&start&end&status&offset&limit
 *   对象详情（DDL / 列 / 索引） GET /query/instances/:id/databases/:db/object-info?kind&name&schema
 *   慢日志                 GET  /slowlog/slowlogs、/slowlog/slowlogs/detail
 *   分区维护               GET  /partman/configs
 *   SQL 优化（SOAR / SQLTuning） POST /sqlopt/optimize {tool,instance_id,db,sql,options} → 异步任务 → GET /sqlopt/jobs/:id[/items/:i]
 *   内置优化建议           POST /sqlopt/advise {instance_id, db, sql}
 *   SQL 体检               POST /sqlopt/analyze {text} → 异步任务
 *
 * 使用说明（调用方需要知道的）：
 *   - instance_name 传 list_instances 里的全名（也接受数字 id），内部先映射成 dbtool 的实例 id。
 *   - dbtool 可能禁用 SELECT *、命中防火墙时要求填写原因（sql_query 的可选参数 reason）。
 *   - 脱敏信息用 masked_columns 返回，is_masked 由它推出。
 *   - sql_optimize 有 soar / sqltuning 两种外部工具（仅 MySQL 系实例）；sqladvisor 用 dbtool 内置建议，
 *     sqltuningadvisor（SQL Server Tuning Advisor）dbtool 没有。
 *   - 没有独立的数据字典页：describe_table / dict_table_info 都读对象详情接口（需要按钮权限 query.objinfo）。
 */

const https = require('https');
const http = require('http');
const fs = require('fs');
const path = require('path');
const os = require('os');

const VERSION = '3.0.0';
const MCP_NODE = process.env.DBTOOL_MCP_NODE || 'dbtool';

/** CLI 模式凭据回退：只读 ~/.claude.json 里本节点的 env */
function loadFromUserConfig() {
  try {
    const p = path.join(os.homedir(), '.claude.json');
    const node = JSON.parse(fs.readFileSync(p, 'utf8')).mcpServers[MCP_NODE];
    if (node && node.env) return node.env;
  } catch { /* 读不到就算了 */ }
  return {};
}

// ── 配置 ────────────────────────────────────────────────────────────────
const ENV = process.env.DBTOOL_URL ? process.env : { ...loadFromUserConfig(), ...process.env };
const CONF = {
  url: (ENV.DBTOOL_URL || '').replace(/\/+$/, ''),
  user: ENV.DBTOOL_USER || '',
  pass: ENV.DBTOOL_PASS || '',
  timeout: Number(ENV.DBTOOL_TIMEOUT_MS || 120000), // 比 dbtool 的查询超时（默认 60 秒）宽一些，避免客户端先超时
  insecure: ENV.DBTOOL_INSECURE_TLS === '1',
  // 返回给用户的工单链接用它；MCP 走内网地址、用户从外网地址打开时，用 DBTOOL_PUBLIC_URL 单独指定
  publicUrl: (ENV.DBTOOL_PUBLIC_URL || ENV.DBTOOL_URL || '').replace(/\/+$/, ''),
};

const RAW_MAX_CHARS = 25000; // 单次响应的最大字符数
const DEFAULT_LIMIT = 100;   // sql_query 缺省行数
const MAX_LIMIT = 2000;      // sql_query 的行数钳制
const JOB_WAIT_MS = 120000;  // 等异步任务（SOAR / SQLTuning / SQL 体检）的最长时间
const JOB_POLL_MS = 1500;

function clip(s) {
  s = String(s);
  return s.length > RAW_MAX_CHARS ? `${s.slice(0, RAW_MAX_CHARS)}\n… (已截断，总长 ${s.length} 字符；请缩小查询范围/减少列)` : s;
}

function clipStr(s, n) {
  s = s == null ? '' : String(s);
  return s.length > n ? `${s.slice(0, n)}…` : s;
}

/** 按字符（不是 UTF-16 单元）截到最多 n 个，超出时最后一个字符是省略号；dbtool 的审批备注上限是 500 个字符 */
function clipRunes(s, n) {
  const a = Array.from(s == null ? '' : String(s));
  return a.length > n ? `${a.slice(0, n - 1).join('')}…` : a.join('');
}

/** 错误信息里不能带出密码和令牌 */
function redact(s) {
  s = String(s);
  for (const secret of [CONF.pass, TOKEN]) {
    if (secret && secret.length >= 4) s = s.split(secret).join('***');
  }
  return s;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const enc = encodeURIComponent;

// ── HTTP 客户端 ─────────────────────────────────────────────────────────
function qs(obj) {
  const parts = [];
  for (const [k, v] of Object.entries(obj || {})) {
    if (v === undefined || v === null || v === '') continue;
    parts.push(`${enc(k)}=${enc(String(v))}`);
  }
  return parts.length ? `?${parts.join('&')}` : '';
}

/** 底层单次请求：返回 { status, headers, body }。json 为请求体对象 */
function rawReq(method, urlPath, { json, token } = {}) {
  return new Promise((resolve, reject) => {
    if (!CONF.url) return reject(new Error('缺少 DBTOOL_URL 配置'));
    let u;
    try { u = new URL(CONF.url + urlPath); } catch { return reject(new Error(`DBTOOL_URL 不合法: ${CONF.url}`)); }
    const mod = u.protocol === 'http:' ? http : https;
    const headers = { Accept: 'application/json' };
    if (token) headers.Authorization = `Bearer ${token}`;
    let data = null;
    if (json !== undefined) {
      data = JSON.stringify(json);
      headers['Content-Type'] = 'application/json; charset=UTF-8';
      headers['Content-Length'] = Buffer.byteLength(data);
    }
    const opts = {
      hostname: u.hostname,
      port: u.port || (u.protocol === 'http:' ? 80 : 443),
      path: u.pathname + u.search,
      method,
      headers,
      timeout: CONF.timeout,
    };
    if (u.protocol === 'https:' && CONF.insecure) opts.rejectUnauthorized = false;
    const req = mod.request(opts, (res) => {
      const chunks = [];
      res.on('data', (d) => chunks.push(d));
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks).toString('utf8') }));
    });
    req.on('timeout', () => { req.destroy(new Error(`dbtool 请求超时（${CONF.timeout}ms）: ${urlPath.split('?')[0]}`)); });
    req.on('error', (e) => reject(new Error(`dbtool 请求失败: ${redact(e.message)}`)));
    if (data) req.write(data);
    req.end();
  });
}

function parseJson(r) {
  try { return JSON.parse(r.body); } catch { return undefined; }
}

/** 失败响应 → Error。dbtool 的业务错误是 {code, message}；404 的纯文本说明模块没开或版本不含该接口 */
function apiError(r, j) {
  if (j && typeof j === 'object' && (j.message || j.code)) {
    return new Error(`dbtool 错误（HTTP ${r.status}${j.code ? ` ${j.code}` : ''}）：${redact(j.message || '')}`);
  }
  if (r.status === 404) return new Error('接口不存在（HTTP 404）：该 dbtool 部署没有开启对应模块，或版本不含该接口');
  const ct = String(r.headers['content-type'] || '');
  return new Error(`dbtool 返回了无法识别的内容（HTTP ${r.status}，${ct || '无类型'}）：${redact(String(r.body).slice(0, 200))}`);
}

// ── 登录与令牌 ──────────────────────────────────────────────────────────
let TOKEN = '';
let TOKEN_EXP = 0; // 毫秒时间戳，0 = 未知
let loginInFlight = null;

/** 令牌载荷（dbtool 签发：uid / username / su=登录时是否超级管理员 / iat / exp）。只解码、不验签：令牌是刚从 dbtool 登录拿到的 */
function tokenClaims(tok) {
  try { return JSON.parse(Buffer.from(String(tok).split('.')[1], 'base64url').toString('utf8')) || {}; } catch { return {}; }
}

function tokenExpiry(tok) {
  const c = tokenClaims(tok);
  return c.exp ? c.exp * 1000 : 0;
}

async function login() {
  if (!CONF.user || !CONF.pass) throw new Error('缺少 DBTOOL_USER / DBTOOL_PASS 配置');
  const r = await rawReq('POST', '/api/v1/auth/login', { json: { username: CONF.user, password: CONF.pass } });
  const j = parseJson(r);
  if (r.status !== 200 || !j || !j.token) {
    const e = apiError(r, j);
    throw new Error(`登录失败：${e.message}`);
  }
  TOKEN = j.token;
  TOKEN_EXP = tokenExpiry(TOKEN);
}

/** 保证有可用令牌；并发调用只登录一次（dbtool 对登录失败有限流和锁定，不要多发） */
async function ensureToken(force) {
  const stale = !TOKEN || (TOKEN_EXP > 0 && Date.now() > TOKEN_EXP - 60000);
  if (!force && !stale) return;
  if (!loginInFlight) loginInFlight = login().finally(() => { loginInFlight = null; });
  await loginInFlight;
}

/** 带令牌的 JSON 请求；401 时重新登录再试一次。返回解析后的响应体 */
async function api(method, urlPath, { json, query } = {}) {
  const full = urlPath + qs(query);
  await ensureToken(false);
  let r = await rawReq(method, full, { json, token: TOKEN });
  if (r.status === 401) {
    TOKEN = '';
    await ensureToken(true);
    r = await rawReq(method, full, { json, token: TOKEN });
  }
  const j = parseJson(r);
  if (r.status >= 400 || j === undefined) throw apiError(r, j);
  return j;
}

// ── 通用解析 ────────────────────────────────────────────────────────────
function requireArgs(args, names) {
  const missing = names.filter((n) => args[n] === undefined || args[n] === null || args[n] === '');
  if (missing.length) throw new Error(`缺少必填参数: ${missing.join(' / ')}`);
}

const READ_PREFIX = /^(select|show|explain|desc|describe|with)\b/i;

function assertReadOnlySql(sql, ctx) {
  const head = String(sql).replace(/^(\s|\/\*[\s\S]*?\*\/|--[^\n]*(\n|$))+/g, '');
  if (!READ_PREFIX.test(head)) {
    throw new Error(`${ctx}只放行查询类语句（select / show / explain / desc / with 开头）；写操作请走上线工单`);
  }
}

let INST_CACHE = { at: 0, list: [] };

/** 实例清单（id / name / db_type …），短缓存 */
async function instances(force) {
  if (!force && Date.now() - INST_CACHE.at < 60000 && INST_CACHE.list.length) return INST_CACHE.list;
  const j = await api('GET', '/api/v1/query/instances');
  INST_CACHE = { at: Date.now(), list: Array.isArray(j.data) ? j.data : [] };
  return INST_CACHE.list;
}

/** instance_name（全名，或数字 id）→ 实例 */
async function resolveInstance(nameOrId) {
  const key = String(nameOrId == null ? '' : nameOrId).trim();
  if (!key) throw new Error('instance_name 不能为空（用 list_instances 拿全名）');
  const find = (list) => list.find((i) => i.name === key)
    || (/^\d+$/.test(key) ? list.find((i) => String(i.id) === key) : undefined)
    || list.find((i) => String(i.name).toLowerCase() === key.toLowerCase());
  let hit = find(await instances(false));
  if (!hit) hit = find(await instances(true)); // 缓存可能旧了
  if (hit) return hit;
  const near = (await instances(false)).filter((i) => String(i.name).toLowerCase().includes(key.toLowerCase())).slice(0, 10).map((i) => i.name);
  throw new Error(`实例不存在或不可见: ${key}（用 list_instances 拿全名）${near.length ? `\n相近的: ${near.join(' / ')}` : ''}`);
}

/** 结果集的文本形式：元信息按 JSON 展开，行一行一条（比整体缩进省很多字符）；超长时从尾部丢行并写明 */
function stringifyResult(out) {
  const { rows, ...meta } = out;
  const all = Array.isArray(rows) ? rows : [];
  const build = (rs) => {
    const metaText = JSON.stringify(meta, null, 1);
    const head = metaText === '{}' ? '{' : metaText.replace(/\n}$/, ',');
    return `${head}\n "rows": [\n${rs.map((r) => `  ${JSON.stringify(r)}`).join(',\n')}\n ]\n}`;
  };
  const full = build(all);
  if (full.length <= RAW_MAX_CHARS) return full;
  let lo = 0;
  let hi = all.length;
  while (lo < hi) { // 二分：最多放得下多少行
    const mid = Math.ceil((lo + hi) / 2);
    if (build(all.slice(0, mid)).length <= RAW_MAX_CHARS - 300) lo = mid; else hi = mid - 1;
  }
  meta.rows_omitted = all.length - lo;
  meta.note = `结果超过 ${RAW_MAX_CHARS} 字符，只列出前 ${lo} 行；请缩小范围、减少列或加过滤条件`;
  return build(all.slice(0, lo));
}

/** 对象详情（dict 的 Detail）→ 文本 */
function renderDetail(d, mode) {
  const out = [`${d.kind || 'table'} ${d.schema ? `${d.schema}.` : ''}${d.name || ''}`];
  for (const s of d.sections || []) {
    if (mode === 'ddl' && !(s.type === 'code' || /列|column/i.test(String(s.title)))) continue;
    out.push('', `## ${s.title}`);
    if (s.type === 'kv') {
      for (const p of s.pairs || []) out.push(`${p.key}: ${p.value}`);
    } else if (s.type === 'table') {
      out.push((s.columns || []).join(' | '));
      for (const r of s.rows || []) out.push(r.join(' | '));
    } else if (s.type === 'code') {
      out.push(s.text || '');
      if (s.note) out.push(`(${s.note})`);
    } else {
      out.push(s.text || s.note || '');
    }
  }
  if (d.sensitive_columns && d.sensitive_columns.length) out.push('', `命中脱敏规则的列: ${d.sensitive_columns.join(', ')}`);
  for (const n of d.notes || []) out.push(`注: ${n}`);
  return clip(out.join('\n'));
}

// ── 查询组工具 ──────────────────────────────────────────────────────────

async function toolInstances({ keyword = '' }) {
  const list = await instances(true);
  const kw = String(keyword).trim().toLowerCase();
  const rows = kw
    ? list.filter((i) => String(i.name).toLowerCase().includes(kw) || String(i.id) === kw
      || String(i.db_type).toLowerCase() === kw || String(i.env_label || i.env || '').toLowerCase().includes(kw)
      || (i.tags || []).some((t) => String(t).toLowerCase().includes(kw)))
    : list;
  const lines = rows.map((i) => `${String(i.id).padStart(4)}  ${String(i.db_type).padEnd(10)}  ${String(i.role || '').padEnd(6)}  ${String(i.env_label || i.env || '-').padEnd(6)}  ${i.name}`);
  return clip(`共 ${rows.length} 个实例（id / db_type / 角色 / 环境 / name；后续工具的 instance_name 传全名或数字 id）:\n${lines.join('\n') || '(无匹配)'}`);
}

async function toolResource({ instance_name, db_name, tb_name, resource_type }) {
  const inst = await resolveInstance(instance_name);
  let p = `/api/v1/query/instances/${inst.id}/databases`;
  if (resource_type !== 'database') {
    requireArgs({ db_name }, ['db_name']);
    p += `/${enc(db_name)}/tables`;
    if (resource_type === 'column') {
      requireArgs({ tb_name }, ['tb_name']);
      p += `/${enc(tb_name)}/columns`;
    }
  }
  const j = await api('GET', p);
  const arr = j.data || [];
  return clip(`共 ${arr.length} 个:\n${arr.join('\n')}`);
}

/** 对象详情：mode=ddl 只要列信息和创建语句；mode=full 全部小节 */
async function objectInfo({ instance_name, db_name, tb_name, schema_name = '', kind = 'table' }, mode) {
  requireArgs({ instance_name, db_name, tb_name }, ['instance_name', 'db_name', 'tb_name']);
  const inst = await resolveInstance(instance_name);
  const j = await api('GET', `/api/v1/query/instances/${inst.id}/databases/${enc(db_name)}/object-info`, {
    query: { kind: kind || 'table', name: tb_name, schema: schema_name },
  });
  return renderDetail(j.data || {}, mode);
}

const toolDescribeTable = (a) => objectInfo(a, 'ddl');
const toolDictTableInfo = (a) => objectInfo(a, 'full'); // db_type 参数 dbtool 不需要，忽略

async function runQuery({ instance_name, db_name, sql, limit, reason }) {
  const inst = await resolveInstance(instance_name);
  const j = await api('POST', '/api/v1/query/execute', {
    json: { instance_id: inst.id, db: db_name, sql, limit, reason: reason || undefined },
  });
  return j.data || {};
}

async function toolQuery({ instance_name, db_name, sql_content, limit_num = DEFAULT_LIMIT, reason }) {
  requireArgs({ instance_name, db_name, sql_content }, ['instance_name', 'db_name', 'sql_content']);
  assertReadOnlySql(sql_content, 'sql_query ');
  let lim = Number(limit_num) || DEFAULT_LIMIT;
  if (lim > MAX_LIMIT) lim = MAX_LIMIT;
  if (lim < 1) lim = DEFAULT_LIMIT;
  const d = await runQuery({ instance_name, db_name, sql: sql_content, limit: lim, reason });
  const masked = d.masked_columns || [];
  return stringifyResult({
    full_sql: d.executed_sql,
    columns: d.columns,
    row_count: d.row_count,
    truncated: d.truncated || undefined,
    effective_limit: d.effective_limit,
    cost_ms: d.cost_ms,
    is_masked: masked.length > 0, // true = 有结果列被脱敏
    masked_columns: masked,
    mask_mode: d.mask_mode || undefined,
    notice: d.notice || undefined,
    warnings: d.warnings && d.warnings.length ? d.warnings : undefined,
    firewall: d.firewall || undefined,
    rows: d.rows,
  });
}

async function toolQueryLog({ limit = 20, offset = 0, search = '', start, end, status, instance_name }) {
  const query = { limit, offset, q: search, start, end, status };
  if (instance_name) query.instance = (await resolveInstance(instance_name)).name;
  const j = await api('GET', '/api/v1/query/history', { query });
  const pg = j.data || {};
  const items = (pg.items || []).map((r) => ({ ...r, sql: clipStr(r.sql, 600) }));
  return clip(JSON.stringify({ total: pg.total, scope: pg.scope, items }, null, 1));
}

async function toolSlowLog({ instance_id = '', start_date = '', end_date = '', search = '', limit = 20, offset = 0, db_name = '' }) {
  const j = await api('GET', '/api/v1/slowlog/slowlogs', {
    query: { instance_id, db_name, q: search, start: start_date, end: end_date, limit, offset },
  });
  const pg = j.data || {};
  const rows = (pg.rows || []).map((r) => ({
    instance_id: r.instance_id,
    instance_name: r.instance_name,
    db_name: r.db_name,
    sql_md5: r.sql_md5,
    exec_date: r.exec_date,
    count: r.count,
    query_time_avg: r.query_time_avg,
    query_time_max: r.query_time_max,
    query_time_sum: r.query_time_sum,
    rows_examined_avg: r.rows_examined_avg,
    rows_sent_avg: r.rows_sent_avg,
    table_name: r.table_name,
    sql_text: clipStr(r.sql_text, 500),
  }));
  return clip(JSON.stringify({ total: pg.total, from: pg.from, to: pg.to, rows }, null, 1));
}

async function toolSlowLogDetail({ instance_id, db_name = '', sql_md5, exec_date = '' }) {
  if (!instance_id || !sql_md5) throw new Error('instance_id / sql_md5 不能为空（从 slow_log 结果行里取）');
  const j = await api('GET', '/api/v1/slowlog/slowlogs/detail', {
    query: { instance_id, db_name, sql_md5, exec_date },
  });
  const d = j.data || {};
  const rows = (d.rows || []).map((r) => ({ ...r, sql_text: clipStr(r.sql_text, 800) }));
  return clip(JSON.stringify({ total: d.total, rows }, null, 1));
}

async function toolPartmanList({ search = '', limit = 50, offset = 0 }) {
  const j = await api('GET', '/api/v1/partman/configs', { query: { search, limit, offset } });
  const rows = (j.data || []).map((r) => ({
    id: r.id,
    instance_id: r.instance_id,
    instance_name: r.instance_name,
    db_type: r.db_type,
    dbname: r.dbname,
    tbname: r.tbname,
    column: r.column,
    col_type: r.col_type,
    period: r.period,
    interval: r.interval,
    retain: r.retain,
    pre_create: r.pre_create,
    isenable: r.isenable,
    supported: r.supported,
    last_run: r.last_run,
    lastest_time: r.lastest_time,
  }));
  return clip(JSON.stringify({ total: j.total, rows }, null, 1));
}

// ── 分析组工具（纯只读） ────────────────────────────────────────────────

async function toolExplain({ instance_name, db_name, sql_content }) {
  requireArgs({ instance_name, db_name, sql_content }, ['instance_name', 'db_name', 'sql_content']);
  let sql = String(sql_content).trim();
  if (!/^explain\b/i.test(sql)) sql = `explain ${sql}`;
  // 不另做只读限制：EXPLAIN 不执行语句，常用来看 UPDATE / DELETE 的执行计划；EXPLAIN ANALYZE 会真正执行，由 dbtool 拒绝。
  // 走在线查询：EXPLAIN 本来就是放行的语句，权限、审计、防火墙与页面一致；plan_tips 是平台给的风险提示
  const d = await runQuery({ instance_name, db_name, sql, limit: 0 });
  return stringifyResult({
    full_sql: d.executed_sql,
    columns: d.columns,
    plan_tips: d.plan_tips && d.plan_tips.length ? d.plan_tips : undefined,
    notice: d.notice || undefined,
    rows: d.rows,
  });
}

/** 等异步任务结束，返回任务视图 */
async function waitJob(id) {
  const t0 = Date.now();
  for (;;) {
    const j = await api('GET', `/api/v1/sqlopt/jobs/${enc(id)}`);
    const job = j.data || {};
    if (['done', 'failed', 'canceled'].includes(job.status)) return job;
    if (Date.now() - t0 > JOB_WAIT_MS) {
      throw new Error(`任务 ${id} 超过 ${JOB_WAIT_MS / 1000} 秒还没结束（状态 ${job.status}），可稍后到网页「SQL 优化」的最近报告里查看`);
    }
    await sleep(JOB_POLL_MS);
  }
}

async function jobItem(id, index) {
  const j = await api('GET', `/api/v1/sqlopt/jobs/${enc(id)}/items/${index}`);
  return j.data || {};
}

/** SOAR / SQLTuning / 内置建议的结果 → 文本 */
function renderOptimize(item) {
  if (item.status === 'failed' || item.error) throw new Error(`优化失败：${item.error || '未知原因'}`);
  const res = item.result;
  if (res && res.output) return clip(`${res.tool === 'soar' ? 'SOAR' : res.tool} 报告${res.truncated ? '（已被平台截断）' : ''}:\n${res.output}`);
  if (res && res.tuning) return clip(`SQLTuning 结果:\n${JSON.stringify(res.tuning, null, 1)}`);
  return clip(JSON.stringify(item, null, 1));
}

function renderAdvice(rep) {
  const findings = (rep.findings || []).map((f) => `[${f.level}] ${f.title}${f.detail ? `：${f.detail}` : ''}${f.suggestion ? `\n    建议：${f.suggestion}` : ''}`);
  const out = [
    `评分 ${rep.score}（最高级别 ${rep.highest || '无'}）  类型 ${rep.type}  涉及表 ${(rep.tables || []).join(', ') || '-'}`,
    `指纹: ${clipStr(rep.fingerprint, 400)}`,
    '',
    findings.length ? findings.join('\n') : '没有发现问题',
  ];
  if (rep.index_ddl && rep.index_ddl.length) out.push('', `建议索引:\n${rep.index_ddl.join('\n')}`);
  if (rep.plan && rep.plan.columns) {
    out.push('', `执行计划:\n${rep.plan.columns.join(' | ')}\n${(rep.plan.rows || []).map((r) => r.join(' | ')).join('\n')}`);
  }
  if (rep.plan_error) out.push(`执行计划失败: ${rep.plan_error}`);
  if (rep.soar) out.push('', `SOAR:\n${rep.soar}`);
  for (const n of rep.notes || []) out.push(`注: ${n}`);
  return clip(out.join('\n'));
}

const TUNING_DEFAULT_OPTIONS = ['sys_parm', 'sql_plan', 'obj_stat'];

async function toolOptimize({ engine = 'soar', instance_name, db_name, sql_content, options }) {
  requireArgs({ instance_name, db_name, sql_content }, ['instance_name', 'db_name', 'sql_content']);
  assertReadOnlySql(sql_content, 'sql_optimize ');
  if (engine === 'sqltuningadvisor') {
    throw new Error('dbtool 没有 SQL Server Tuning Advisor；SQL Server 的语句请用 sql_explain 看执行计划');
  }
  const inst = await resolveInstance(instance_name);
  if (engine === 'sqladvisor' || engine === 'advise') {
    // dbtool 没有美团 SQLAdvisor，用内置规则的优化建议（读表结构 + 分析执行计划）代替
    const j = await api('POST', '/api/v1/sqlopt/advise', { json: { instance_id: inst.id, db: db_name, sql: sql_content } });
    return `${engine === 'sqladvisor' ? '（dbtool 没有 SQLAdvisor，以下是内置建议）\n' : ''}${renderAdvice(j.data || {})}`;
  }
  if (engine !== 'soar' && engine !== 'sqltuning') {
    throw new Error(`engine 非法: ${engine}（可用 soar / sqltuning / sqladvisor(=advise)）`);
  }
  const body = { tool: engine, instance_id: inst.id, db: db_name, sql: sql_content };
  if (engine === 'sqltuning') body.options = Array.isArray(options) && options.length ? options : TUNING_DEFAULT_OPTIONS;
  const started = await api('POST', '/api/v1/sqlopt/optimize', { json: body });
  const job = await waitJob((started.data || {}).id);
  if (job.status !== 'done') throw new Error(`任务未成功（${job.status}）：${job.error || (job.items && job.items[0] && job.items[0].error) || '未知原因'}`);
  return renderOptimize(await jobItem(job.id, 0));
}

async function toolAnalyze({ text, instance_name, db_name }) {
  requireArgs({ text }, ['text']);
  const body = { text };
  if (instance_name) {
    requireArgs({ db_name }, ['db_name']);
    body.instance_id = (await resolveInstance(instance_name)).id;
    body.db = db_name;
  }
  const started = await api('POST', '/api/v1/sqlopt/analyze', { json: body });
  const job = await waitJob((started.data || {}).id);
  const items = job.items || [];
  const out = {
    status: job.status,
    notice: job.notice || undefined,
    error: job.error || undefined,
    summary: job.summary,
    items: items.map((i) => ({ index: i.index, type: i.type, level: i.level, score: i.score, findings: i.findings, status: i.status, error: i.error, tables: i.tables, sql: clipStr(i.sql, 200) })),
  };
  const details = [];
  for (const i of items.slice(0, 3)) { // 只展开前 3 条的完整报告，其余看清单
    if (i.status !== 'done') continue;
    const d = await jobItem(job.id, i.index);
    if (d.report) details.push({ index: i.index, full_sql: clipStr(d.full_sql, 800), report: d.report });
  }
  if (details.length) out.details = details;
  return clip(JSON.stringify(out, null, 1));
}

// ── 工单组工具 ──────────────────────────────────────────────────────────
// 已启用（只读、支持批量工单）：sql_check / list_workflows / workflow_detail / workflow_log。
// sql_check 的导出预检（export=true）会在每个实例上执行一次只读的 select count(*) 统计待导出行数，其余没有副作用。
// 已启用（会创建真实工单、支持批量工单）：submit_sql_workflow / submit_export_workflow。
// 已启用（会终止工单）：cancel_workflow。
//
// 批量工单：dbtool 的批量上线 / 批量导出是【一个工单覆盖多个实例】。instance_names（去重后 ≥ 2 个）对应请求里的 instance_ids，
// 绝不会拆成一个实例一个工单。

const WORKFLOW_PAGE = 20;       // dbtool 的工单列表每页固定 20 条（page 从 1 开始）
const MAX_WORKFLOW_ROWS = 100;  // list_workflows 一次最多返回多少条（超过 20 条会自动翻页）
const CHECK_MAX_ROWS = 50;      // sql_check / workflow_detail 里每组结果行的默认上限

const workflowUrl = (id) => `${CONF.publicUrl}/sqlworkflow/${id}`;

/** 数组；也容忍逗号 / 换行分隔的字符串 */
function asList(v) {
  if (Array.isArray(v)) return v.map((x) => String(x).trim()).filter(Boolean);
  if (typeof v === 'string') return v.split(/[\n,]/).map((x) => x.trim()).filter(Boolean);
  return [];
}

/** instance_names（去重后 ≥ 2 个）= 批量；否则按 instance_name 的单实例处理 */
async function resolveTargets({ instance_name, instance_names }) {
  const names = asList(instance_names);
  if (names.length) {
    const insts = [];
    const seen = new Set();
    for (const n of names) {
      const inst = await resolveInstance(n);
      if (!seen.has(inst.id)) { seen.add(inst.id); insts.push(inst); }
    }
    return { batch: insts.length >= 2, insts };
  }
  if (!instance_name) throw new Error('缺少 instance_name（单实例）或 instance_names（批量，至少 2 个）');
  return { batch: false, insts: [await resolveInstance(instance_name)] };
}

const levelOf = (v) => { const n = Number(v); return n >= 2 ? 'error' : n === 1 ? 'warning' : 'ok'; };

/** 检查 / 执行结果里的一行（goInception 的行格式；批量工单的行带 instance_name，汇总行 is_summary） */
function compactRow(r) {
  const o = { n: r.id, level: levelOf(r.errlevel), status: r.stagestatus };
  if (r.instance_name) o.instance = r.instance_name;
  if (r.is_summary) o.summary = true;
  if (r.batch_closed) o.closed = true;
  if (r.errormessage && r.errormessage !== 'None') o.message = clipStr(r.errormessage, 500);
  o.sql = clipStr(r.sql, 300);
  if (r.affected_rows) o.affected_rows = r.affected_rows;
  if (r.execute_time) o.execute_time = r.execute_time;
  return o;
}

/** 超过上限时优先留下汇总行和出错的行，其余按原顺序补满；返回 { rows, omitted }。toRow 把一行整理成输出的样子，缺省 compactRow */
function pickRows(rows, max, toRow = compactRow) {
  const all = Array.isArray(rows) ? rows : [];
  const hot = (r) => r.is_summary || Number(r.errlevel) >= 2;
  const keep = new Set([...all.filter(hot), ...all.filter((r) => !hot(r))].slice(0, max));
  return { rows: all.filter((r) => keep.has(r)).map((r) => toRow(r)), omitted: all.length - keep.size };
}

/** build(cap) 生成文本：按 caps 从大到小，取第一个不超长的 */
function fitText(build, caps) {
  let text = '';
  for (const cap of caps) {
    text = build(cap);
    if (text.length <= RAW_MAX_CHARS) return text;
  }
  return clip(text);
}

const ROW_CAPS = [200, 100, 50, 25, 12, 6, 3, 1, 0];

// ── 数据导出的公共检查（sql_check 的导出预检和 submit_export_workflow 共用） ──

const EXPORT_FORMATS = ['csv', 'xlsx', 'json', 'xml', 'sql']; // dbtool 支持的导出格式（internal/export）

const MERGE_NEEDS_BATCH = 'export_merge（合并成一个文件）只能用于批量导出：instance_names 要有至少 2 个不同的实例；只导出一个实例时不需要合并';

/** 导出只允许一条 select / with 查询（SQL Server 还有 declare / set 开头的只读批处理）：写操作请走上线工单。这里只挡明显不对的，只读、单条、行数等由服务端校验 */
function assertExportSql(sql, dbType) {
  const mssql = dbType === 'mssql';
  const comments = mssql
    ? /^(\s|\/\*[\s\S]*?\*\/|--[^\n]*(\n|$))+/
    : /^(\s|\/\*[\s\S]*?\*\/|--[^\n]*(\n|$)|#[^\n]*(\n|$))+/; // MySQL 系还认 # 注释
  const head = String(sql).replace(comments, '');
  if (!(/^(select|with)\b/i.test(head) || (mssql && /^(declare|set)\b/i.test(head)))) {
    throw new Error(`数据导出只允许 select / with 查询${mssql ? '（SQL Server 还允许 declare / set 开头的只读批处理）' : ''}；写操作请走上线工单（submit_sql_workflow）`);
  }
}

/** 布尔参数：没传 = false；传了就必须是明确的 true / false，认不出来的值报错（别悄悄当成 false） */
function parseBool(name, v) {
  if (v === undefined || v === null || v === '') return false;
  const f = flag01(v);
  if (!f) throw new Error(`${name} 只能是 true 或 false`);
  return f === '1';
}

/**
 * 导出预检的一行里有没有可信的“待导出行数”：dbtool 的 affected_rows 总会输出（没统计时是 0），所以要排除两种 0——
 * T-SQL 的 declare / set 批处理（统计不了，只给一行警告）和出错的行（没有权限、连不上……）。出错的行只有数字 > 0（超过上限、列不一致）时才算
 */
function exportCount(r) {
  if (r.affected_rows === undefined || r.affected_rows === null || r.affected_rows === '') return undefined;
  const n = Number(r.affected_rows);
  if (!Number.isFinite(n)) return undefined;
  if (/跳过行数预统计|无法预先统计行数/.test(`${r.stagestatus || ''} ${r.errormessage || ''}`)) return undefined;
  return n > 0 || Number(r.errlevel) < 2 ? n : undefined;
}

/** 导出预检里的一行：待导出行数单独写成 rows_to_export（0 行也要写出来，compactRow 会把 0 丢掉） */
function exportRow(r) {
  const o = compactRow(r);
  delete o.affected_rows;
  const n = exportCount(r);
  if (n !== undefined) o.rows_to_export = n;
  return o;
}

/**
 * 检查请求的请求体。批量 = 一次请求带全部 instance_ids。
 * 上线检查：dbtool 只在第一个实例上做语法与规则检查，再预检每个实例能否连接、库是否存在；
 * 导出预检（exp）：在每个实例上各统计一次待导出行数，核对库表查询权限和 SQL 防火墙，merge 时还核对各实例返回的列是否一致
 */
function checkBody({ batch, insts, db_name, sql, exp, merge }) {
  const body = batch
    ? { instance_ids: insts.map((i) => i.id), db_name, sql }
    : { instance_id: insts[0].id, db_name, sql };
  if (exp) {
    body.export = true;
    if (merge) body.export_merge = true;
  }
  return body;
}

/** 发检查请求（只发一次），算出 verdict：有错误或有实例用不了 = 不通过，只有警告 = 有警告。返回 { d: 响应数据, checks: 逐实例结果, verdict } */
async function postCheck(body) {
  const j = await api('POST', '/api/v1/sqlworkflow/check', { json: body });
  const d = j.data || {};
  const checks = Array.isArray(d.instances) ? d.instances : [];
  const badInst = checks.filter((c) => !c.ok);
  const verdict = d.error_count > 0 || badInst.length ? '不通过' : d.warning_count > 0 ? '有警告' : '通过';
  return { d, checks, verdict };
}

/** 导出预检各行待导出行数之和（合并成一个文件时就是文件里的行数）。每一行都有行数才给：T-SQL 批处理统计不了、或有的行没给行数时都不给——不能拿一部分当全部 */
function exportRowsTotal(rows) {
  const counts = (Array.isArray(rows) ? rows : []).map(exportCount);
  return counts.length && counts.every((n) => n !== undefined) ? counts.reduce((s, n) => s + n, 0) : undefined;
}

async function toolSqlCheck(args) {
  const { instance_name, instance_names, db_name, sql_content } = args;
  requireArgs({ db_name, sql_content }, ['db_name', 'sql_content']);
  const exp = parseBool('export', args.export); // 数据导出预检
  const merge = parseBool('export_merge', args.export_merge);
  if (merge && !exp) throw new Error('export_merge 只能和 export=true 一起用（合并成一个文件是批量导出的选项）');
  const { batch, insts } = await resolveTargets({ instance_name, instance_names });
  if (exp) { // 和 submit_export_workflow 同样的本地检查：这里不通过的，提交时也会被拒绝
    assertExportSql(sql_content, insts[0].db_type);
    if (merge && !batch) throw new Error(MERGE_NEEDS_BATCH);
  }
  const { d, checks, verdict } = await postCheck(checkBody({ batch, insts, db_name, sql: sql_content, exp, merge }));
  const exportTotal = exp && batch && verdict !== '不通过' ? exportRowsTotal(d.rows) : undefined; // 批量导出且没有出错才给合计
  const mode = exp
    ? (batch ? `批量导出（${insts.length} 个实例，提交时是一个批量导出工单，${merge ? '合并成一个文件' : '每个实例各一个文件'}）` : '单实例导出')
    : (batch ? `批量（${insts.length} 个实例，提交时是一个批量工单）` : '单实例');
  return fitText((cap) => {
    const { rows, omitted } = pickRows(d.rows, Math.min(cap, CHECK_MAX_ROWS), exp ? exportRow : compactRow);
    const out = {
      verdict,
      mode,
      instances: insts.map((i) => i.name),
      db_name,
      syntax: d.syntax_label,
      source: d.source,
      error_count: d.error_count,
      warning_count: d.warning_count,
      notice: d.notice || undefined,
    };
    if (exp) {
      if (exportTotal !== undefined) out.rows_to_export_total = exportTotal;
      out.next = verdict === '不通过'
        ? '检查不通过：submit_export_workflow 提交前会自动跑同样的预检，不通过就不会创建工单；先按 rows 里的提示改好，再重新检查'
        : '检查没有错误，可以用 submit_export_workflow 提交（参数保持一致，批量也传 instance_names；提交时会自动再预检一次，资源组、可导出清单、导出格式在提交时再校验）';
    }
    if (batch) {
      out.batch_note = exp
        ? `导出检查在每个实例上各做一次（统计待导出行数、核对库表查询权限和 SQL 防火墙${merge ? '，并核对各实例返回的列是否一致' : ''}）；instance_checks 是每个实例的结果`
        : `语法与规则只在第一个实例（${insts[0].name}）上检查；instance_checks 是每个实例能否连接、库是否存在的预检`;
      out.instance_checks = checks.map((c) => ({ name: c.name, ok: !!c.ok, error: c.error || undefined }));
    }
    out.rows = rows;
    if (omitted) out.rows_omitted = omitted;
    return JSON.stringify(out, null, 1);
  }, ROW_CAPS);
}

// ── 创建工单前的自动检查（submit_sql_workflow、submit_export_workflow 共用） ──
// 创建前先发一次和 sql_check 同样的检查请求：不通过、或者没能完成（接口出错、连接断开、超时），都抛错，调用方就不会发创建请求——
// 否则 dbtool 在检查有错误时会建出一张“自动审批不通过”的工单。没有跳过的开关。只有警告时照常创建，警告原文写进结果。

const msgOf = (x) => (x.errormessage && x.errormessage !== 'None' ? String(x.errormessage).trim() : '');

/** 上线检查里一行的原因：带上语句序号和语句开头（一批语句里要能看出是哪一条） */
function stmtReason(r) {
  const m = msgOf(r);
  if (!m) return '';
  const s = clipStr(String(r.sql || '').replace(/\s+/g, ' ').trim(), 80);
  return `${r.id ? `第 ${r.id} 条：` : ''}${m}${s ? `（${s}）` : ''}`;
}

/**
 * 发检查请求并把关。title = 提示里的名字（提交前检查 / 导出预检），slowHint = 超时时的补充说明，advice = 不通过时最后一行的建议，
 * rowReason = 把一行出错 / 警告的结果写成一条原因。返回 { r: postCheck 的结果, rows, warnings }
 */
async function checkBeforeCreate({ title, body, slowHint, advice, rowReason }) {
  let r;
  try {
    r = await postCheck(body);
  } catch (e) {
    throw new Error(`${title}没能完成，没有创建工单${/请求超时/.test(e.message) ? slowHint : ''}：${e.message}`);
  }
  const rows = Array.isArray(r.d.rows) ? r.d.rows : [];
  if (r.verdict === '不通过') {
    const fromRows = rows.filter((x) => Number(x.errlevel) >= 2).map(rowReason).filter(Boolean);
    // 逐实例结果里的原因：行里已经写了的（导出的行里就带着实例名和原因）不重复列；没写的（连不上、库不存在）补上实例名
    const fromInstances = r.checks.filter((c) => !c.ok && c.error)
      .map((c) => ({ name: String(c.name || ''), error: String(c.error).trim() }))
      .filter((c) => !fromRows.some((m) => m.includes(c.error)))
      .map((c) => (c.name && !c.error.includes(c.name) ? `${c.name}：${c.error}` : c.error));
    const reasons = [...new Set([...fromRows, ...fromInstances])];
    const shown = reasons.slice(0, 8).map((m, i) => `  ${i + 1}. ${clipStr(m, 300)}`);
    if (reasons.length > 8) shown.push(`  …共 ${reasons.length} 条`);
    if (!shown.length) shown.push('  （dbtool 没有给出具体原因，请到网页上点“检查”看）');
    throw new Error(`${title}不通过，没有创建工单：\n${shown.join('\n')}\n${advice}`);
  }
  const warnings = [...rows.filter((x) => Number(x.errlevel) === 1).map(rowReason), r.d.notice ? String(r.d.notice).trim() : '']
    .filter(Boolean).slice(0, 5).map((m) => clipStr(m, 300));
  return { r, rows, warnings };
}

/**
 * 检查通过（或只有警告）后创建出来的工单仍是“自动审批不通过”时，给一句可能的原因：
 * 检查只有警告，多半是管理员把自动驳回设成了“有警告也驳回”（review.auto_reject_level=warning）；检查是通过的，多半是检查之后情况变了
 */
function autoRejectHint(verdict, changed, name = '检查') {
  return verdict === '有警告'
    ? `提交前${name}只有警告、没有错误，多半是管理员把自动驳回设成了“有警告也驳回”（review.auto_reject_level=warning）。`
    : `提交前的${name}是通过的，多半是${changed}。`;
}

/** 提交上线工单前的检查（和 sql_check 的上线检查是同一个请求）。返回给提交结果用的摘要 { verdict: 通过 / 有警告, detail, warnings } */
async function checkDeploy({ batch, insts, db, sql }) {
  const g = await checkBeforeCreate({
    title: '提交前检查',
    body: checkBody({ batch, insts, db_name: db, sql, exp: false }),
    slowHint: '（检查可能很慢：SQL 很长，或批量实例多、要逐个确认能连接；可以调大 DBTOOL_TIMEOUT_MS）',
    advice: '请按提示改好后重新提交（可以先用 sql_check 反复检查）',
    rowReason: stmtReason,
  });
  return {
    verdict: g.r.verdict,
    detail: batch ? `${insts.length} 个实例都能连接、库存在；语法与规则只在第一个实例（${insts[0].name}）上检查` : '',
    warnings: g.warnings,
  };
}

/** dbtool 要起止日期同时给才按日期过滤，只给一个的话补另一个 */
function dateRange(start, end) {
  if (!start && !end) return {};
  const t = new Date();
  const pad = (n) => String(n).padStart(2, '0');
  return { start_date: start || '2000-01-01', end_date: end || `${t.getFullYear()}-${pad(t.getMonth() + 1)}-${pad(t.getDate())}` };
}

const flag01 = (v) => ([1, '1', true, 'true'].includes(v) ? '1' : [0, '0', false, 'false'].includes(v) ? '0' : '');

async function toolListWorkflows({ kind = 'sql', batch, status, navStatus, tab, search = '', limit = 20, offset = 0, instance_id, group_id, start_date, end_date }) {
  const st = String(status || navStatus || '').trim();
  const query = {
    kind: kind === 'export' ? 'export' : kind === 'all' ? '' : 'change', // dbtool：export 只看导出，change 只看上线
    batch: flag01(batch),
    status: st === 'all' ? '' : st,
    tab,
    q: search,
    instance_id,
    group_id,
    ...dateRange(start_date, end_date),
  };
  const want = Math.min(Math.max(Number(limit) || 20, 1), MAX_WORKFLOW_ROWS);
  const off = Math.max(Number(offset) || 0, 0);
  let page = Math.floor(off / WORKFLOW_PAGE) + 1; // 每页固定 20 条：limit / offset 换算成页码，必要时连翻几页
  let skip = off % WORKFLOW_PAGE;
  const rows = [];
  let total = 0;
  for (let n = 0; n < 6 && rows.length < want; n++, page++) {
    const j = await api('GET', '/api/v1/sqlworkflow', { query: { ...query, page } });
    const data = Array.isArray(j.data) ? j.data : [];
    total = j.total || 0;
    rows.push(...data.slice(skip));
    skip = 0;
    if (data.length < WORKFLOW_PAGE) break;
  }
  const out = rows.slice(0, want).map((r) => {
    const o = {
      id: r.id,
      name: r.workflow_name,
      status: r.status,
      status_label: r.status_label,
      kind: r.is_export ? 'export' : 'sql',
      instance: r.instance_name, // 批量工单是“第一个实例名 等 N 个实例”
      db_name: r.db_name,
      group: r.group_name,
      engineer: r.engineer_display || r.engineer,
      create_time: r.create_time,
      syntax: r.syntax_label,
    };
    if (r.batch_count) o.batch_count = r.batch_count;
    if (r.is_export) o.export_format = r.export_format;
    if (r.approval_level) o.approval_level = r.approval_level;
    if (r.waiting_for_you) o.waiting_for_you = true;
    if (r.schedule_at) o.schedule_at = r.schedule_at;
    if (r.skip_check) o.skip_check = true;
    o.detail_url = workflowUrl(r.id);
    return o;
  });
  return clip(JSON.stringify({ total, offset: off, returned: out.length, rows: out }, null, 1));
}

async function fetchWorkflow(workflowId) {
  const n = Number(workflowId);
  if (!Number.isInteger(n) || n <= 0) throw new Error('workflow_id 必须是正整数（工单号）');
  const j = await api('GET', `/api/v1/sqlworkflow/${n}`);
  return j.data || {};
}

const batchResult = (i) => (i.closed ? 'closed' : i.failed ? 'failed' : i.success ? 'success' : 'pending');

async function toolWorkflowDetail({ workflow_id, max_rows = CHECK_MAX_ROWS }) {
  const d = await fetchWorkflow(workflow_id);
  const want = Math.min(Math.max(Number(max_rows) || 0, 0), 200);
  return fitText((cap) => {
    const c = Math.min(cap, want);
    const review = pickRows(d.review_rows, c);
    const exec = pickRows(d.exec_rows, c);
    const out = {
      id: d.id,
      name: d.name,
      detail_url: workflowUrl(d.id),
      status: d.status,
      status_label: d.status_label,
      kind: d.is_export ? 'export' : 'sql',
      instance: d.instance,
      db_name: d.db_name,
      group: d.group,
      env: d.env_label || d.env || undefined,
      env_prod: d.env_prod || undefined,
      engineer: d.engineer,
      create_time: d.create_time,
      finish_time: d.finish_time || undefined,
      is_backup: d.is_backup,
      syntax: d.syntax_label,
      flow: d.flow || undefined,
      steps: (d.steps || []).map((s) => ({ title: s.title, state: s.state, who: s.who || undefined, time: s.time || undefined, remark: s.remark || undefined })),
      current_approvers: (d.current_approvers || []).map((a) => a.display || a.username),
      stuck_hint: d.stuck_hint || undefined,
    };
    if (d.is_batch) {
      const insts = Array.isArray(d.batch_instances) ? d.batch_instances : [];
      const count = (r) => insts.filter((i) => batchResult(i) === r).length;
      const total = (d.batch_targets || []).length || insts.length;
      out.batch = {
        total,
        success: count('success'),
        failed: count('failed'),
        closed: count('closed'),
        pending: insts.length ? count('pending') : total, // 还没有逐实例结果（没执行过）：全部算待执行
        // 已执行：每个实例各自的结果；还没执行：只有计划上线的实例名
        instances: insts.length
          ? insts.map((i) => ({ name: i.name, result: batchResult(i), db_type: i.db_type, address: i.address, elapsed: i.elapsed, stmts: i.stmts, note: clipStr(i.note, 300) || undefined }))
          : (d.batch_targets || []).map((n) => ({ name: n, result: 'pending' })),
        note: '批量工单是一个工单覆盖多个实例；检查结果（review_rows）只来自第一个实例，执行结果（exec_rows）带各实例的行和汇总行',
      };
    }
    if (d.is_export) {
      out.export = { format: d.export_format, file_name: d.file_name || undefined, rows: d.export_rows, merged: d.export_merge || undefined, unmasked: d.export_unmasked ? true : undefined };
    }
    out.review_rows = review.rows;
    if (review.omitted) out.review_rows_omitted = review.omitted;
    out.exec_rows = exec.rows;
    if (exec.omitted) out.exec_rows_omitted = exec.omitted;
    return JSON.stringify(out, null, 1);
  }, ROW_CAPS);
}

async function toolWorkflowLog({ workflow_id }) {
  const d = await fetchWorkflow(workflow_id);
  const lines = (d.logs || []).map((l) => `${l.time}  ${l.operator || '-'}  ${l.operation}${l.info ? `：${l.info}` : ''}`);
  const batchNote = d.is_batch ? `，批量 ${(d.batch_targets || []).length || (d.batch_instances || []).length} 个实例` : '';
  return clip([
    `工单 #${d.id} ${d.name}（${d.status_label}${batchNote}）`,
    `工单链接: ${workflowUrl(d.id)}`,
    '',
    lines.join('\n') || '(没有操作日志)',
  ].join('\n'));
}

// ── 高危：批量审核通过 / 批量执行（仅超级管理员） ───────────────────────────
// 审批和执行是高危操作（“须人工审批后才执行”是平台的安全边界）。这里的限制全部在 MCP 里先做，被拒绝时不会对工单发出任何请求
// （唯一的请求是为了核对身份而做的一次重新登录）；dbtool 服务端仍按自己的规则再校验一遍（按钮权限、审批流当前级、时间窗…）：
//   1. 只接受批量工单（is_batch）；
//   2. 只接受超级管理员账号：每次调用先重新登录取新令牌，看令牌里的 su（令牌旧了 su 可能已过期）；
//   3. 必须再传一次 confirm_workflow_id，且等于 workflow_id；
//   4. 审核通过：提交人是当前账号自己的工单一律拒绝（超级管理员也一样；dbtool 自己只对非超级管理员禁止自审）。
// 不含：定时执行、手工确认结束、失败实例的重试 / 关闭、驳回别人的工单（撤回自己提交的见 cancel_workflow）。

function parseWorkflowId(v) {
  const n = Number(v);
  if (!Number.isInteger(n) || n <= 0) throw new Error('workflow_id 必须是正整数（工单号）');
  return n;
}

function requireConfirm(id, confirm) {
  if (confirm === undefined || confirm === null || String(confirm).trim() === '') {
    throw new Error('缺少 confirm_workflow_id：请再传一次工单号确认（必须等于 workflow_id）');
  }
  if (Number(confirm) !== id) throw new Error(`confirm_workflow_id（${confirm}）和 workflow_id（${id}）不一致，已拒绝`);
}

/** 每次都重新登录，用新令牌里的 su 校验是不是超级管理员；返回令牌载荷 */
async function requireSuperuser(what) {
  await ensureToken(true);
  const c = tokenClaims(TOKEN);
  if (c.su !== true) {
    throw new Error(`${what}只接受超级管理员账号：当前账号 ${c.username || CONF.user} 不是超级管理员（已重新登录核对），没有对工单发出任何请求`);
  }
  return c;
}

/** 在工单列表里按工单号找到那一行：要读提交人的登录名（详情里的 engineer 是显示名，不能拿来比对） */
async function findWorkflowRow(id) {
  for (let page = 1; page <= 10; page++) {
    const j = await api('GET', '/api/v1/sqlworkflow', { query: { q: id, page } });
    const rows = Array.isArray(j.data) ? j.data : [];
    const hit = rows.find((r) => Number(r.id) === id); // 搜索词是数字时还会按名称等模糊匹配，要挑出工号相同的那一行
    if (hit) return hit;
    if (rows.length < WORKFLOW_PAGE) return null;
  }
  return null;
}

const same = (a, b) => String(a).trim().toLowerCase() === String(b).trim().toLowerCase();

const MCP_MARK = '（经 MCP 操作）';

/**
 * 审批 / 终止的备注：把写的原因截到装得下标记的长度，再加上“（经 MCP 操作）”——平台上能看出这一步是经 MCP 做的，原因再长标记也不会被截掉。
 * dbtool 的备注最多 500 个字符；没写原因时只有标记
 */
function mcpRemark(note) {
  const n = String(note == null ? '' : note).trim();
  if (!n) return '经 MCP 操作';
  return `${clipRunes(n, 500 - Array.from(MCP_MARK).length)}${MCP_MARK}`;
}

/** 两个工具共用的前置检查：工单号与确认、超级管理员、批量工单。返回 { id, claims, d } */
async function guardBatchWorkflow(what, { workflow_id, confirm_workflow_id }) {
  const id = parseWorkflowId(workflow_id);
  requireConfirm(id, confirm_workflow_id);
  const claims = await requireSuperuser(what);
  const d = await fetchWorkflow(id);
  if (!d.is_batch) {
    throw new Error(`${what}只接受批量工单：工单 #${id}「${d.name}」是单实例工单，请到网页上操作：${workflowUrl(id)}`);
  }
  return { id, claims, d };
}

const batchSize = (d) => (d.batch_targets || []).length || (d.batch_instances || []).length;
const unmaskedWarning = (d) => (d.is_export && d.export_unmasked ? '⚠ 这是“导出原值（不脱敏）”的导出工单' : '');

async function toolApproveWorkflow(args) {
  const { id, claims, d } = await guardBatchWorkflow('批量审核通过', args);
  if (d.status !== 'workflow_manreviewing') {
    throw new Error(`工单 #${id} 当前状态是「${d.status_label}」，只有「等待审批」的工单才能审核通过`);
  }
  // 提交人的登录名只在列表里有；找不到或为空都当作无法确认，一律拒绝
  const row = await findWorkflowRow(id);
  const submitter = row && String(row.engineer || '').trim();
  if (!submitter) throw new Error(`无法确认工单 #${id} 的提交人，已拒绝审核通过`);
  if ([claims.username, CONF.user].filter(Boolean).some((me) => same(me, submitter))) {
    throw new Error(`工单 #${id} 是当前账号（${submitter}）自己提交的，审核通过一律拒绝（超级管理员也一样）：请让另一位审批人处理`);
  }
  if (d.can_review === false) {
    throw new Error(`当前账号在工单 #${id} 上没有审批权限（缺少审批按钮权限，或还没轮到这个账号审批）；当前审批人：${(d.current_approvers || []).map((a) => a.display || a.username).join('、') || '无'}`);
  }
  const remark = mcpRemark(args.remark); // 审批备注里留下“这一步是经 MCP 做的”
  await api('POST', `/api/v1/sqlworkflow/${id}/pass`, { json: { remark } });
  let after = {};
  try { after = await fetchWorkflow(id); } catch { /* 审批已经提交，读不到新状态不影响结果 */ }
  const done = after.status === 'workflow_review_pass';
  return [
    `✅ 工单 #${id}「${d.name}」本级已审核通过（批量工单，${batchSize(d)} 个实例）`,
    after.status_label
      ? `状态: ${after.status_label}${done ? '（审批流已走完，可以执行了）' : `；还有后续审批，当前审批人：${(after.current_approvers || []).map((a) => a.display || a.username).join('、') || '无'}`}`
      : '状态: （读取新状态失败，请到链接核对）',
    `操作账号: ${claims.username || CONF.user}（超级管理员，已重新登录核对）`,
    unmaskedWarning(d),
    `工单链接（请转给用户核对）: ${workflowUrl(id)}`,
  ].filter(Boolean).join('\n');
}

async function toolExecuteWorkflow(args) {
  const { id, claims, d } = await guardBatchWorkflow('批量执行', args);
  if (d.status !== 'workflow_review_pass') {
    throw new Error(`工单 #${id} 当前状态是「${d.status_label}」，只有「审核通过」的工单才能立即执行（定时执行的工单请到网页上处理）`);
  }
  if (d.can_execute === false) {
    throw new Error(`当前账号在工单 #${id} 上没有执行权限（缺少执行按钮权限，或不在该工单所属资源组内）`);
  }
  const body = { mode: 'auto' };
  const executor = String(args.executor || '').trim();
  if (executor) body.executor = executor; // 不传就用 dbtool 的缺省执行方式（SQL Server 只有原生执行）
  await api('POST', `/api/v1/sqlworkflow/${id}/execute`, { json: body });
  let after = {};
  try { after = await fetchWorkflow(id); } catch { /* 已提交执行，读不到新状态不影响结果 */ }
  return [
    `✅ 工单 #${id}「${d.name}」已提交执行（批量工单，${batchSize(d)} 个实例）`,
    `状态: ${after.status_label || '（读取新状态失败，请到链接核对）'}；执行在后台进行，用 workflow_detail 查看每个实例的结果`,
    `操作账号: ${claims.username || CONF.user}（超级管理员，已重新登录核对）`,
    unmaskedWarning(d),
    `工单链接（请转给用户核对）: ${workflowUrl(id)}`,
  ].filter(Boolean).join('\n');
}

// ── 终止（撤回）工单 ────────────────────────────────────────────────────
// 会把工单变成“人工终止流程”，不能恢复（要再做只能重新提交），并通知相关人。限制全部在 MCP 里先做，被拒绝时不会对工单发出任何请求
// （为读工单要登录 / 读详情 / 读列表，这些都是只读的）；dbtool 服务端仍按自己的规则再校验一遍：
//   1. 必须填终止原因 remark（dbtool 要求必填），平台上显示为“<原因>（经 MCP 操作）”。这一条在登录之前判断；
//   2. 只接受 等待审批 / 审核通过 / 定时执行 的工单（dbtool 允许终止的全部状态）；排队中、执行中、已结束的不能终止，取消正在执行的也不在这里；
//   3. 只能撤回【自己提交的】工单：提交人的登录名只在工单列表里有（详情里的 engineer 是显示名），查不到、或不是当前账号，一律拒绝——
//      别人的工单是审批人的“驳回”，请到网页上操作；
//   4. 详情里 dbtool 自己算好的 can_cancel 为 false（比如账号没有终止按钮权限）也拒绝。
// 上线 / 导出、单实例 / 批量都可以：终止的是整张工单（批量工单是一个工单）。只发一次请求，不重试。
// 不含：取消正在执行（cancel-run）、取消定时计划（schedule/cancel）、标记执行有异常。
const CANCELLABLE = ['workflow_manreviewing', 'workflow_review_pass', 'workflow_timingtask'];

async function toolCancelWorkflow(args) {
  const id = parseWorkflowId(args.workflow_id);
  const reason = String(args.remark == null ? '' : args.remark).trim();
  if (!reason) throw new Error('缺少终止原因 remark：写明为什么要终止这张工单（dbtool 要求必填，最多 500 个字符）');
  await ensureToken(false);
  const me = String(tokenClaims(TOKEN).username || CONF.user || '').trim();
  const d = await fetchWorkflow(id);
  if (!CANCELLABLE.includes(d.status)) {
    throw new Error(`工单 #${id} 当前状态是「${d.status_label}」，只有「等待审批」「审核通过」「定时执行」的工单才能终止（排队中、执行中、已结束的不能终止；取消正在执行的请到网页上操作）`);
  }
  // 提交人的登录名只在列表里有；找不到或为空都当作无法确认，一律拒绝
  const row = await findWorkflowRow(id);
  const submitter = row && String(row.engineer || '').trim();
  if (!submitter) throw new Error(`无法确认工单 #${id} 的提交人，已拒绝终止`);
  if (!me || !same(me, submitter)) {
    throw new Error(`工单 #${id} 是 ${submitter} 提交的，不是当前账号（${me || '未知'}）：cancel_workflow 只能撤回自己提交的工单；别人的工单请审批人到网页上驳回：${workflowUrl(id)}`);
  }
  if (d.can_cancel === false) {
    throw new Error(`dbtool 判定当前账号不能终止工单 #${id}（缺少终止按钮权限，或工单状态刚变了）：请到网页上核对：${workflowUrl(id)}`);
  }
  const remark = mcpRemark(reason);
  let j;
  try {
    j = await api('POST', `/api/v1/sqlworkflow/${id}/cancel`, { json: { remark } });
  } catch (e) {
    // 请求发出去了却没收到响应：工单可能已经被终止，先核对状态再说
    if (/请求超时|请求失败/.test(e.message)) {
      throw new Error(`${e.message}\n⚠ 请求已经发出但没有收到响应，工单可能已经被终止：请先用 workflow_detail 核对状态，不要直接重试`);
    }
    throw e;
  }
  const r = j.data || {};
  const kind = `${d.is_export ? '数据导出' : 'SQL 上线'}工单${d.is_batch ? `（批量，${batchSize(d)} 个实例）` : ''}`;
  return [
    `✅ ${kind} #${id}「${d.name}」已终止（提交人撤回）`,
    `状态: ${r.status_label || '（响应里没有新状态，请到链接核对）'}（原状态：${d.status_label}）`,
    r.status && r.status !== 'workflow_abort' ? '⚠ 响应里的状态不是「人工终止流程」，请到链接核对' : '',
    `终止原因: ${remark}`,
    d.status === 'workflow_timingtask' ? '定时执行的计划已一并清除' : '',
    `操作账号: ${me}`,
    `工单链接（请转给用户核对）: ${workflowUrl(id)}`,
  ].filter(Boolean).join('\n');
}

// ── 提交 SQL 上线工单 ───────────────────────────────────────────────────
// 会在 dbtool 创建真实工单并通知审核人（执行仍须审核人批准）。批量（SQL Server）= 一次请求带全部 instance_ids，创建【一个】批量工单，
// 不会一个实例一个工单，也没有逐实例循环的回退。提交前先按 dbtool 提交页的选项（GET /sqlworkflow/options）校验资源组、实例归属、
// 批量类型和数量，不满足就整体报错、什么都不创建。
// 通过后自动跑一次提交前检查（checkDeploy：和 sql_check 的上线检查是同一个请求，批量时另外确认每个实例能连接、库存在）：
// 检查不通过、或者没能完成（超时、连不上……），都不发创建请求——否则 dbtool 在检查有错误时会建出一张“自动审批不通过”的工单。没有跳过检查的开关。
// 创建时服务端还会再检查一遍（语法与规则、权限、DDL / DML 资源组是否对得上…）。
// 不提供：跳过检查（skip_check）、定时执行。数据导出见下面的 submit_export_workflow。

// 与 dbtool 服务端的长度上限一致（按字符计），超了在本地直接拒绝
const MAX_WORKFLOW_NAME = 50;
const MAX_DEMAND_URL = 500;
const MAX_DB_NAME = 64;
const MAX_SQL_CHARS = 2000000;
const LOCAL_TIME = /^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}(:\d{2})?$/; // dbtool 接受的本地时间写法（无时区）
const runeLen = (s) => Array.from(String(s)).length;
const timeMs = (s) => Date.parse(String(s).trim().replace(' ', 'T'));

/** 不用访问 dbtool 就能判断的：必填、长度、链接、时间窗。返回整理后的值 */
function validateSubmitArgs(a) {
  requireArgs(a, ['workflow_name', 'group_name', 'db_name', 'sql_content']);
  const name = String(a.workflow_name).trim();
  const db = String(a.db_name).trim();
  const url = String(a.demand_url || '').trim();
  const sql = String(a.sql_content);
  if (!name) throw new Error('workflow_name 不能为空');
  if (runeLen(name) > MAX_WORKFLOW_NAME) throw new Error(`工单名称过长（最多 ${MAX_WORKFLOW_NAME} 个字符，当前 ${runeLen(name)} 个）`);
  if (!db || runeLen(db) > MAX_DB_NAME) throw new Error(`db_name 不能为空，且最多 ${MAX_DB_NAME} 个字符`);
  if (!sql.trim()) throw new Error('sql_content 不能为空');
  if (runeLen(sql) > MAX_SQL_CHARS) throw new Error(`SQL 内容过长（最多 ${MAX_SQL_CHARS} 个字符）`);
  if (url && !/^https?:\/\//i.test(url)) throw new Error('demand_url 必须以 http:// 或 https:// 开头');
  if (runeLen(url) > MAX_DEMAND_URL) throw new Error(`demand_url 过长（最多 ${MAX_DEMAND_URL} 个字符）`);
  const start = String(a.run_date_start || '').trim();
  const end = String(a.run_date_end || '').trim();
  for (const [k, v] of [['run_date_start', start], ['run_date_end', end]]) {
    if (v && !LOCAL_TIME.test(v)) throw new Error(`${k} 的格式应为 YYYY-MM-DD HH:MM[:SS]`);
  }
  if (start && end && !(timeMs(end) > timeMs(start))) throw new Error('可执行时间的结束时间必须晚于开始时间');
  return { name, db, url, sql, start, end };
}

const nameList = (list, max = 20) => {
  const names = (list || []).map((i) => i.name);
  return `${names.slice(0, max).join(' / ')}${names.length > max ? ` …（共 ${names.length} 个）` : ''}` || '(没有)';
};

/** 在提交页选项里按名字找资源组（先精确，再不分大小写）；找不到就列出你能用的。返回 dbtool 里的规范写法 */
function findGroup(opts, group_name) {
  const groups = Array.isArray(opts.groups) ? opts.groups : [];
  const want = String(group_name).trim();
  const group = groups.find((g) => g.name === want) || groups.find((g) => String(g.name).toLowerCase() === want.toLowerCase());
  if (!group) {
    throw new Error(`资源组不存在，或你不在该组内：${want}。你能用的资源组：${groups.map((g) => g.name).join(' / ') || '(没有)'}`);
  }
  return group;
}

/** 批量工单对实例类型和数量的限制（批量上线、批量导出共用 dbtool 的 batch.types / batch.max）；word = 上线 / 导出 */
function checkBatchLimits(opts, insts, word) {
  const b = opts.batch || {};
  const types = Array.isArray(b.types) ? b.types : [];
  const bad = insts.filter((i) => !types.includes(i.db_type));
  if (bad.length) {
    throw new Error(`批量${word}只支持这些类型的实例：${types.join('、') || '(没有)'}；不符合的：${bad.map((i) => `${i.name}（${i.db_type}）`).join('、')}`);
  }
  if (new Set(insts.map((i) => i.db_type)).size > 1) throw new Error(`所选实例的数据库类型不一致，批量${word}要求所有实例为同一类型`);
  if (b.max && insts.length > b.max) throw new Error(`单个批量工单最多支持 ${b.max} 个实例，当前选择了 ${insts.length} 个`);
}

/** 按 dbtool 提交页的选项校验：资源组、实例归属、批量权限 / 类型 / 数量。返回选中的资源组（用 dbtool 里的规范名字） */
function validateAgainstOptions(opts, { group_name, insts, batch }) {
  const group = findGroup(opts, group_name);
  const inGroup = new Set((group.instances || []).map((i) => i.id));
  const outside = insts.filter((i) => !inGroup.has(i.id));
  if (outside.length) {
    throw new Error(`以下实例不在资源组「${group.name}」的可上线清单里（实例要带“上线”标签，并关联到该资源组）：${outside.map((i) => i.name).join('、')}。该组可上线的实例：${nameList(group.instances)}`);
  }
  if (batch) {
    if (!(opts.batch || {}).allowed) throw new Error('当前账号没有批量上线的权限（缺少 sqlworkflow.submit_batch），不能提交批量工单');
    checkBatchLimits(opts, insts, '上线');
  }
  return group;
}

const STATUS_LABEL = {
  workflow_manreviewing: '等待审批',
  workflow_autoreviewwrong: '自动审批不通过',
  workflow_review_pass: '审核通过',
  workflow_timingtask: '定时执行',
};

/**
 * 创建工单（上线和导出共用）：只发一次请求，不自动重试。
 * 请求发出去了却没收到响应（超时、连接被断）时工单可能已经创建，提示先核对，别直接重提。返回 { workflow_id, status, message }
 */
async function createWorkflow(body, lookup = 'list_workflows（search 填工单名）') {
  try {
    const j = await api('POST', '/api/v1/sqlworkflow', { json: body });
    return j.data || {};
  } catch (e) {
    if (/请求超时|请求失败/.test(e.message)) {
      throw new Error(`${e.message}\n⚠ 请求已经发出但没有收到响应，工单可能已经创建：请先用 ${lookup}核对，不要直接重新提交`);
    }
    throw e;
  }
}

async function toolSubmitSqlWorkflow(args) {
  const v = validateSubmitArgs(args); // 本地能判断的先判断，不通过时不访问 dbtool
  const { batch, insts } = await resolveTargets(args);
  const j0 = await api('GET', '/api/v1/sqlworkflow/options');
  const group = validateAgainstOptions(j0.data || {}, { group_name: args.group_name, insts, batch });
  const pre = await checkDeploy({ batch, insts, db: v.db, sql: v.sql }); // 不通过 / 没能完成就到此为止，不发创建请求
  const body = {
    name: v.name,
    instance_ids: batch ? insts.map((i) => i.id) : undefined, // 批量 = 一次请求带全部实例，创建一个批量工单
    instance_id: batch ? undefined : insts[0].id,
    db_name: v.db,
    group_name: group.name,
    sql: v.sql,
    is_backup: args.is_backup === undefined ? true : Boolean(args.is_backup),
  };
  if (v.url) body.demand_url = v.url;
  if (v.start) body.run_date_start = v.start;
  if (v.end) body.run_date_end = v.end;
  const r = await createWorkflow(body);
  let after = {};
  try { after = r.workflow_id ? await fetchWorkflow(r.workflow_id) : {}; } catch { /* 工单已经创建，读不到详情不影响结果 */ }
  const label = after.status_label || STATUS_LABEL[r.status] || r.status;
  const approvers = (after.current_approvers || []).map((a) => a.display || a.username).join('、');
  const lines = [
    `✅ SQL 上线工单已创建${batch ? `（批量工单，${insts.length} 个实例，只有这一个工单）` : '（单实例）'}`,
    `工单号: #${r.workflow_id}  状态: ${label}`,
    r.message ? `dbtool 提示: ${r.message}` : '',
    `资源组: ${group.name}  库: ${v.db}`,
    `实例: ${insts.map((i) => i.name).join(', ')}`,
    `提交前检查: ${pre.verdict}${pre.detail ? `（${pre.detail}）` : ''}`,
    ...pre.warnings.map((w) => `  ⚠ ${w}`),
  ];
  if (batch && asList(args.instance_names).length > insts.length) lines.push(`（instance_names 里有重复的实例，已去重）`);
  if (r.status === 'workflow_autoreviewwrong') {
    lines.push(`⚠ 检查有错误，系统自动审批不通过，工单没有进入审批。${autoRejectHint(pre.verdict, '检查之后表结构或数据变了')}请到链接里看具体原因（workflow_detail 也能看到检查结果），改好 SQL 后重新提交`);
  } else if (r.status === 'workflow_review_pass') {
    lines.push('⚠ 这张工单无需审批，系统已直接审核通过，现在就可以执行：请先到链接里核对，再决定是否执行');
  } else {
    lines.push(`须审核人在平台批准后才会执行${approvers ? `；当前审批人：${approvers}` : ''}`);
  }
  lines.push(`工单链接（⚠ 必须原样发给用户核对）: ${workflowUrl(r.workflow_id)}`);
  return lines.filter(Boolean).join('\n');
}

// ── 提交数据导出工单 ────────────────────────────────────────────────────
// 会在 dbtool 创建真实的数据导出工单并通知审核人（审核人批准、再执行导出，才会生成文件）。批量导出（SQL Server）= 一次请求带全部 instance_ids，
// 创建【一个】批量导出工单（至少 2 个不同的实例），不会一个实例一个工单，也没有逐实例循环的回退；export_merge 让各实例的数据合并写进同一个文件。
// 提交前先按 dbtool 提交页的选项（GET /sqlworkflow/options）校验：导出权限、资源组、实例是否在该组的“可导出”清单里、批量类型和数量、导出格式、
// 能否合并，不满足就整体报错、什么都不创建。
// 通过后自动跑一次导出预检（precheckExport：和 sql_check 的 export=true 是同一个请求，批量时每个实例各统计一次待导出行数）：
// 预检不通过、或者没能完成（超时、连不上……），都不发创建请求——否则 dbtool 在检查有错误时会建出一张“自动审批不通过”的工单。没有跳过预检的开关。
// 创建时服务端还会再检查一遍（预检之后数据 / 权限又变了，仍可能建出“自动审批不通过”的工单，结果里会说明）。
// 不提供：导出原值（不脱敏，dbtool 只允许超级用户，是否脱敏一律按平台规则）、跳过检查、定时导出。
// SQL / 合并的本地检查（assertExportSql、MERGE_NEEDS_BATCH）和 sql_check 共用，定义在 sql_check 前面。

/** 按 dbtool 提交页的选项校验数据导出：权限、资源组、实例是否可导出、批量类型 / 数量、导出格式、能否合并。返回选中的资源组 */
function validateExportOptions(opts, { group_name, insts, batch, format, merge }) {
  const group = findGroup(opts, group_name);
  const ex = opts.export || {};
  if (batch ? !ex.batch_allowed : !ex.allowed) {
    throw new Error(batch
      ? '当前账号没有批量数据导出的权限（缺少 export.submit_batch），不能提交批量导出工单'
      : '当前账号没有数据导出的权限（缺少 export.submit），不能提交导出工单');
  }
  const exportable = new Set((group.export_instances || []).map((i) => i.id));
  const notExportable = insts.filter((i) => !exportable.has(i.id));
  if (notExportable.length) {
    throw new Error(`以下实例不在资源组「${group.name}」的可导出清单里（实例要开启“查询”，你要有它的库 / 表查询权限，并关联到该资源组）：${notExportable.map((i) => i.name).join('、')}。该组可导出的实例：${nameList(group.export_instances)}`);
  }
  if (batch) checkBatchLimits(opts, insts, '导出');
  const formats = (Array.isArray(ex.formats) ? ex.formats : []).map((f) => f.value);
  if (formats.length && !formats.includes(format)) {
    throw new Error(`当前不能导出成 ${format} 格式；可选：${formats.join(' / ')}`);
  }
  if (merge && !ex.can_merge) {
    throw new Error('当前不能把批量导出合并成一个文件（账号缺少批量导出权限，或管理员还没有更新 dbt_sql_export_ext 表）');
  }
  return group;
}

/**
 * 提交前的导出预检（共用的 checkBeforeCreate）：不通过、或者没能完成，都抛错（调用方就不会发创建请求）。
 * 返回给提交结果用的摘要 { verdict: 通过 / 有警告, count: 待导出行数的说明（统计不了时为空）, warnings: 警告原文 }
 */
async function precheckExport({ batch, insts, db, sql, merge }) {
  const g = await checkBeforeCreate({
    title: '导出预检',
    body: checkBody({ batch, insts, db_name: db, sql, exp: true, merge }),
    slowHint: '（统计待导出行数的查询可能很慢：缩小范围，或调大 DBTOOL_TIMEOUT_MS）',
    advice: '请按提示改好后重新提交（可以先用 sql_check（export=true）反复预检）',
    rowReason: msgOf,
  });
  const total = exportRowsTotal(g.rows);
  return {
    verdict: g.r.verdict,
    count: total === undefined ? '' : batch ? `${insts.length} 个实例共 ${total} 行待导出` : `待导出 ${total} 行`,
    warnings: g.warnings,
  };
}

async function toolSubmitExportWorkflow(args) {
  const v = validateSubmitArgs(args); // 本地能判断的先判断，不通过时不访问 dbtool
  const format = String(args.export_format || 'csv').trim().toLowerCase();
  if (!EXPORT_FORMATS.includes(format)) throw new Error(`export_format 只能是 ${EXPORT_FORMATS.join(' / ')}（默认 csv）`);
  const merge = parseBool('export_merge', args.export_merge);
  const { batch, insts } = await resolveTargets(args);
  assertExportSql(v.sql, insts[0].db_type);
  if (merge && !batch) throw new Error(MERGE_NEEDS_BATCH);
  const j0 = await api('GET', '/api/v1/sqlworkflow/options');
  const group = validateExportOptions(j0.data || {}, { group_name: args.group_name, insts, batch, format, merge });
  const pre = await precheckExport({ batch, insts, db: v.db, sql: v.sql, merge }); // 不通过 / 没能完成就到此为止，不发创建请求
  const body = {
    name: v.name,
    instance_ids: batch ? insts.map((i) => i.id) : undefined, // 批量 = 一次请求带全部实例，创建一个批量导出工单
    instance_id: batch ? undefined : insts[0].id,
    db_name: v.db,
    group_name: group.name,
    sql: v.sql,
    export: true,
    export_format: format,
  };
  if (merge) body.export_merge = true;
  if (v.url) body.demand_url = v.url;
  if (v.start) body.run_date_start = v.start;
  if (v.end) body.run_date_end = v.end;
  const r = await createWorkflow(body, 'list_workflows（kind 填 export，search 填工单名）');
  let after = {};
  try { after = r.workflow_id ? await fetchWorkflow(r.workflow_id) : {}; } catch { /* 工单已经创建，读不到详情不影响结果 */ }
  const label = after.status_label || STATUS_LABEL[r.status] || r.status;
  const approvers = (after.current_approvers || []).map((a) => a.display || a.username).join('、');
  const lines = [
    `✅ 数据导出工单已创建${batch ? `（批量导出，${insts.length} 个实例，只有这一个工单）` : '（单实例）'}`,
    `工单号: #${r.workflow_id}  状态: ${label}`,
    r.message ? `dbtool 提示: ${r.message}` : '',
    `资源组: ${group.name}  库: ${v.db}`,
    `实例: ${insts.map((i) => i.name).join(', ')}`,
    `导出格式: ${format}${batch ? (merge ? '；各实例的数据合并成一个文件' : '；每个实例各导出一个文件') : ''}`,
    `提交前预检: ${pre.verdict}${pre.count ? `（${pre.count}）` : ''}`,
    ...pre.warnings.map((w) => `  ⚠ ${w}`),
    '脱敏: 按平台规则处理（MCP 不提供“导出原值”）',
  ];
  if (asList(args.instance_names).length > insts.length) {
    lines.push(`（instance_names 里有重复的实例，已去重${batch ? '' : '，只剩 1 个实例，按单实例导出'}）`);
  }
  if (r.status === 'workflow_autoreviewwrong') {
    lines.push(`⚠ 检查有错误，系统自动审批不通过，工单没有进入审批。${autoRejectHint(pre.verdict, '预检之后数据或权限变了', '预检')}常见原因：导出行数超过上限、被 SQL 防火墙拒绝、查询执行失败（表不存在 / 连不上）、合并导出时各实例返回的列不一致。请到链接里看具体原因（workflow_detail 也能看到检查结果），改好后重新提交`);
  } else if (r.status === 'workflow_review_pass') {
    lines.push('⚠ 这张导出工单无需审批，系统已直接审核通过，只差执行导出：请先到链接里核对，再决定是否执行');
  } else {
    lines.push(`须审核人在平台批准后，再执行导出才会生成文件${approvers ? `；当前审批人：${approvers}` : ''}`);
  }
  lines.push(`工单链接（⚠ 必须原样发给用户核对）: ${workflowUrl(r.workflow_id)}`);
  return lines.filter(Boolean).join('\n');
}

// ── 工具清单 ────────────────────────────────────────────────────────────
const INSTANCE_NAME_DESC = 'list_instances 返回的实例全名（也可传数字 id）';

// 批量工单：dbtool 的批量上线 / 批量导出是【一个工单覆盖多个实例】（同一段 SQL 依次在各实例上执行），
// 不是一个实例一个工单。目前只有 SQL Server 允许批量（服务端 BATCH_DB_TYPES，默认 mssql），一个工单最多 100 个实例。
const BATCH_NAMES = {
  type: 'array',
  items: { type: 'string' },
  description: '批量：多个 SQL Server 实例（至少 2 个，同一类型、同属 group_name 资源组，最多 100 个）。'
    + '传了就只创建【一个】批量工单覆盖这些实例，绝不会拆成一个实例一个工单；此时忽略 instance_name',
};

const TOOLS = [
  {
    name: 'list_instances',
    description:
      '列出当前账号可查询的 dbtool 数据库实例（id / 类型 / 主从 / 环境 / 全名）。'
      + '其它工具的 instance_name 传这里返回的全名（含括号、空格、中文），也可以传数字 id。',
    inputSchema: {
      type: 'object',
      properties: { keyword: { type: 'string', description: '按名称 / 类型 / 环境 / 标签模糊过滤（可选），如 生产 / mysql / doris' } },
    },
  },
  {
    name: 'list_databases',
    description: '列出实例下的数据库。',
    inputSchema: {
      type: 'object',
      properties: { instance_name: { type: 'string', description: INSTANCE_NAME_DESC } },
      required: ['instance_name'],
    },
  },
  {
    name: 'list_tables',
    description: '列出库下的表名（PostgreSQL / SQL Server 是 架构.表名）。',
    inputSchema: {
      type: 'object',
      properties: { instance_name: { type: 'string' }, db_name: { type: 'string' } },
      required: ['instance_name', 'db_name'],
    },
  },
  {
    name: 'list_columns',
    description: '列出表的列名。',
    inputSchema: {
      type: 'object',
      properties: { instance_name: { type: 'string' }, db_name: { type: 'string' }, tb_name: { type: 'string' } },
      required: ['instance_name', 'db_name', 'tb_name'],
    },
  },
  {
    name: 'describe_table',
    description:
      '看表结构：列信息 + 创建语句（MySQL 系是 show create table 的完整 DDL）。读 dbtool 的对象详情接口，需要按钮权限 query.objinfo。'
      + '视图 / 存储过程 / 函数 / 触发器 / 事件用 kind 指定。',
    inputSchema: {
      type: 'object',
      properties: {
        instance_name: { type: 'string' },
        db_name: { type: 'string' },
        tb_name: { type: 'string', description: '对象名（表 / 视图 / 过程 / 函数 …）' },
        schema_name: { type: 'string', description: 'SQL Server / PostgreSQL 的架构名，一般留空' },
        kind: { type: 'string', enum: ['table', 'view', 'procedure', 'function', 'trigger', 'event'], description: '默认 table' },
      },
      required: ['instance_name', 'db_name', 'tb_name'],
    },
  },
  {
    name: 'sql_query',
    description:
      '在指定实例/库执行只读查询（仅放行 select/show/explain/desc/with 开头；写操作会被拒）。'
      + '行数上限由平台按账号策略再限制一次。注意：① 平台可能禁用 SELECT *（会提示列出字段）；'
      + '② 敏感字段按平台脱敏规则处理（is_masked / masked_columns 说明哪些列被脱敏）；'
      + '③ 生产实例是真实线上数据，查询会记入平台审计日志；④ 命中防火墙“要求填写原因”的规则时，用 reason 传原因。',
    inputSchema: {
      type: 'object',
      properties: {
        instance_name: { type: 'string', description: INSTANCE_NAME_DESC },
        db_name: { type: 'string' },
        sql_content: { type: 'string', description: '单条查询 SQL' },
        limit_num: { type: 'number', description: `返回行数上限，默认 ${DEFAULT_LIMIT}，钳 ${MAX_LIMIT}` },
        reason: { type: 'string', description: '命中防火墙“要求填写原因”规则时要填的原因（可选）' },
        schema_name: { type: 'string', description: '保留参数：dbtool 的库表已含架构，忽略' },
      },
      required: ['instance_name', 'db_name', 'sql_content'],
    },
  },
  {
    name: 'query_log',
    description: '查在线查询的历史记录（SQL / 实例 / 耗时 / 时间 / 状态）。普通账号只看到自己的；管理员账号会看到所有人的。',
    inputSchema: {
      type: 'object',
      properties: {
        limit: { type: 'number', description: '默认 20' },
        offset: { type: 'number' },
        search: { type: 'string', description: '按 SQL 内容模糊搜' },
        instance_name: { type: 'string', description: '只看某个实例（可选）' },
        start: { type: 'string', description: '起始日期 YYYY-MM-DD（可选）' },
        end: { type: 'string', description: '结束日期 YYYY-MM-DD（可选）' },
        status: { type: 'string', description: '状态过滤（可选，如 ok）' },
      },
    },
  },
  {
    name: 'slow_log',
    description:
      '查慢日志（平台集中采集）。按 sql_md5 + 日期聚合展示：实例/库/次数/耗时/样例 SQL。'
      + 'instance_id 用 list_instances 返回的数字 id；日期格式 YYYY-MM-DD。想看某条的逐日明细用 slow_log_detail。需要按钮权限 slowlogs.view 或 slowquery.view。',
    inputSchema: {
      type: 'object',
      properties: {
        instance_id: { type: 'number', description: '实例数字 id（可选，不传=全部可见实例）' },
        db_name: { type: 'string', description: '只看某个库（可选）' },
        start_date: { type: 'string', description: 'YYYY-MM-DD' },
        end_date: { type: 'string', description: 'YYYY-MM-DD' },
        search: { type: 'string', description: '按 SQL 文本 / 库名 / 用户模糊搜' },
        limit: { type: 'number', description: '默认 20' },
        offset: { type: 'number' },
      },
    },
  },
  {
    name: 'slow_log_detail',
    description: '慢日志明细（从 slow_log 结果行取 instance_id / db_name / sql_md5 / exec_date 传入）。',
    inputSchema: {
      type: 'object',
      properties: {
        instance_id: { type: 'number' },
        db_name: { type: 'string' },
        sql_md5: { type: 'string' },
        exec_date: { type: 'string', description: 'YYYY-MM-DD' },
      },
      required: ['instance_id', 'sql_md5'],
    },
  },
  {
    name: 'dict_table_info',
    description:
      '表（或视图 / 过程 / 函数 …）的完整详情：基本信息、列、索引、创建语句等所有小节（比 describe_table 更全）。'
      + 'dbtool 没有独立的数据字典页，这里读对象详情接口，需要按钮权限 query.objinfo。',
    inputSchema: {
      type: 'object',
      properties: {
        instance_name: { type: 'string' },
        db_name: { type: 'string' },
        tb_name: { type: 'string' },
        db_type: { type: 'string', description: '保留参数：dbtool 不需要，忽略' },
        schema_name: { type: 'string', description: 'SQL Server / PostgreSQL 的架构名，一般留空' },
        kind: { type: 'string', enum: ['table', 'view', 'procedure', 'function', 'trigger', 'event'], description: '默认 table' },
      },
      required: ['instance_name', 'db_name', 'tb_name'],
    },
  },
  {
    name: 'partman_list',
    description: '分区维护配置列表（哪些表在自动维护分区：实例/库/表/分区列/周期/保留数/是否启用/最近执行时间）。只读。',
    inputSchema: {
      type: 'object',
      properties: {
        search: { type: 'string', description: '按表名等模糊搜' },
        limit: { type: 'number', description: '默认 50' },
        offset: { type: 'number' },
      },
    },
  },
  // ── 分析组 ──
  {
    name: 'sql_explain',
    description: '在指定实例/库对 SQL 跑 explain 执行计划（只读；不带 explain 前缀会自动补），并返回平台的执行计划风险提示（全表扫描、filesort 等）。',
    inputSchema: {
      type: 'object',
      properties: { instance_name: { type: 'string' }, db_name: { type: 'string' }, sql_content: { type: 'string' } },
      required: ['instance_name', 'db_name', 'sql_content'],
    },
  },
  {
    name: 'sql_optimize',
    description:
      'SQL 优化建议（只读，异步任务，通常几秒到一分钟）。engine: soar=SOAR 体检报告（需要管理员已配置 SOAR 测试环境）/ '
      + 'sqltuning=SQLTuning 分析（真实执行 select，选项见 options）/ sqladvisor 或 advise=dbtool 内置规则建议（读表结构、分析执行计划，推荐先用）。'
      + 'soar / sqltuning 仅 MySQL 系实例；sqltuningadvisor（SQL Server）dbtool 没有。需要按钮权限：soar 要 optimize.soar，sqltuning 要 optimize.tuning，sqladvisor / advise 要 optimize.explain。',
    inputSchema: {
      type: 'object',
      properties: {
        engine: { type: 'string', enum: ['soar', 'sqltuning', 'sqladvisor', 'advise', 'sqltuningadvisor'], description: '默认 soar' },
        instance_name: { type: 'string' },
        db_name: { type: 'string' },
        sql_content: { type: 'string' },
        options: {
          type: 'array',
          items: { type: 'string', enum: ['sys_parm', 'sql_plan', 'obj_stat', 'sql_profile'] },
          description: '仅 sqltuning：要显示的内容，默认 sys_parm / sql_plan / obj_stat；sql_profile 会真实执行语句',
        },
        verbose: { type: 'number', description: '保留参数：忽略' },
      },
      required: ['instance_name', 'db_name', 'sql_content'],
    },
  },
  {
    name: 'sql_analyze',
    description:
      'SQL 体检（只读，异步任务）：粘贴 SQL / 慢日志片段，平台做指纹归并、规则检查并给出评分和建议。'
      + '传了 instance_name + db_name 还会读表结构并分析执行计划。需要按钮权限 sqlanalyze.run。',
    inputSchema: {
      type: 'object',
      properties: {
        text: { type: 'string', description: '要分析的 SQL / 慢日志文本' },
        instance_name: { type: 'string', description: '可选：带上实例会更准' },
        db_name: { type: 'string', description: '传了 instance_name 时必填' },
      },
      required: ['text'],
    },
  },
  // ── 工单组 ──
  {
    name: 'sql_check',
    description:
      '对 SQL 跑 dbtool 的提交前检查，只读，不创建工单；返回 verdict（通过 / 有警告 / 不通过）和逐条结果。'
      + '上线检查（默认，无副作用）：goInception 审核规则（没配置 goInception 时只做基础检查）。批量（SQL Server）用 instance_names 传多个实例：'
      + '语法与规则只在第一个实例上检查，并预检每个实例能否连接、库是否存在。submit_sql_workflow 提交时也会自动先跑同样的检查，不通过就不创建工单；想先看结果就单独用它。'
      + '数据导出预检（export=true）：检查是不是单条只读查询（select / with）、待导出行数是否超过上限、你对库表有没有查询权限、SQL 防火墙是否拒绝；'
      + '批量时每个实例各检查一次，export_merge=true 时还核对各实例返回的列是否一致（合并成一个文件的前提）。'
      + 'submit_export_workflow 提交时会自动先跑同样的预检，不通过就不创建工单（不经预检直接提交不通过的导出，dbtool 会建出一张“自动审批不通过”的工单）；'
      + '想在提交前先看结果、行数，或把结果给用户看，就单独用它。'
      + '注意导出预检会在每个实例上执行一次 select count(*) 统计待导出行数（只读，有超时），大表请把过滤条件写好。',
    inputSchema: {
      type: 'object',
      properties: {
        instance_name: { type: 'string', description: '单实例检查；批量检查改传 instance_names' },
        instance_names: BATCH_NAMES,
        db_name: { type: 'string' },
        sql_content: { type: 'string', description: '要检查的 DML/DDL；export=true 时是导出用的一条 select / with 查询' },
        export: { type: 'boolean', description: '数据导出预检（默认 false = 上线检查）' },
        export_merge: { type: 'boolean', description: '仅 export=true 且批量：同时核对各实例返回的列是否一致（合并成一个文件的前提）' },
      },
      required: ['db_name', 'sql_content'],
    },
  },
  {
    name: 'submit_sql_workflow',
    description:
      '提交 SQL 上线工单（DML/DDL 变更）。⚠ 会在 dbtool 创建真实工单并通知审核人，执行须审核人批准。'
      + '只在用户明确要求提交时使用：先把工单名、资源组、目标实例和 SQL 给用户看、得到明确同意（想先看检查结果，可以先用 sql_check）；返回的工单链接必须原样发给用户核对。'
      + '多个 SQL Server 实例要上线同一段 SQL 时，用 instance_names 一次提交成【一个】批量工单（绝不会拆成一个实例一个工单）。'
      + '提交前会按 dbtool 提交页的选项校验：资源组是不是你所在的、实例是否属于该资源组的可上线清单、批量的权限 / 类型（目前只有 SQL Server）/ 数量（最多 100 个），'
      + '不满足就整体报错、什么都不创建。'
      + '通过后会自动跑一次提交前检查（和 sql_check 的上线检查是同一个：goInception 审核规则，批量时另外确认每个实例能连接、库存在），'
      + '检查不通过、或者没能完成，都不创建工单，直接报错并列出原因——所以不会建出“自动审批不通过”的工单；没有跳过检查的开关。'
      + 'DDL / DML 要选名字里带 DDL / DML 的资源组，由 dbtool 服务端校验。'
      + '不含跳过检查、定时执行、数据导出。',
    inputSchema: {
      type: 'object',
      properties: {
        workflow_name: { type: 'string', description: '工单标题，写清目的（最多 50 个字符）' },
        group_name: { type: 'string', description: '资源组名称，必须是你所在的资源组；传错会列出你能用的资源组' },
        instance_name: { type: 'string', description: '单实例上线；批量上线改传 instance_names' },
        instance_names: BATCH_NAMES,
        db_name: { type: 'string' },
        sql_content: { type: 'string', description: '要上线的 DML / DDL，可多条' },
        demand_url: { type: 'string', description: '需求链接，http(s):// 开头（可选）' },
        is_backup: { type: 'boolean', description: '是否备份（默认 true；dbtool 按检查来源决定实际是否备份，SQL Server 没有备份）' },
        run_date_start: { type: 'string', description: '可执行时间窗开始，YYYY-MM-DD HH:MM[:SS]（可选）' },
        run_date_end: { type: 'string', description: '可执行时间窗结束，必须晚于开始（可选）' },
      },
      required: ['workflow_name', 'group_name', 'db_name', 'sql_content'],
    },
  },
  {
    name: 'submit_export_workflow',
    description:
      '提交数据导出工单（一条 select 查询的结果导出为文件）。⚠ 会在 dbtool 创建真实工单并通知审核人，审核人批准、再执行导出后才会生成文件。'
      + '只在用户明确要求提交时使用：先把工单名、资源组、目标实例、库、SQL 和导出格式给用户看、得到明确同意（想先看行数，可以先用 sql_check（export=true；批量传 instance_names，合并时加 export_merge=true）预检）；'
      + '返回的工单链接必须原样发给用户核对。'
      + '多个 SQL Server 实例要导出同一条查询时，用 instance_names（至少 2 个不同的实例）一次提交成【一个】批量导出工单（绝不会拆成一个实例一个工单）；'
      + 'export_merge=true 时各实例的数据合并写进同一个文件（只能用于批量导出，各实例返回的列必须一致），否则每个实例各导出一个文件。'
      + '提交前会按 dbtool 提交页的选项校验：导出权限、资源组是不是你所在的、实例是否在该组的可导出清单里、批量的类型（目前只有 SQL Server）/ 数量（最多 100 个）、导出格式、能否合并，'
      + '不满足就整体报错、什么都不创建。SQL 只能是一条 select / with 查询（SQL Server 还可以是 declare / set 开头的只读批处理）；'
      + '通过后会自动跑一次导出预检（和 sql_check 的 export=true 是同一个检查：行数上限、库表查询权限、SQL 防火墙、合并时各实例的列是否一致，批量时每个实例各统计一次待导出行数），'
      + '预检不通过、或者没能完成，都不创建工单，直接报错并列出原因——所以不会建出“自动审批不通过”的工单；没有跳过预检的开关。'
      + '不含导出原值（不脱敏）、跳过检查、定时导出。',
    inputSchema: {
      type: 'object',
      properties: {
        workflow_name: { type: 'string', description: '工单标题，写清导出的目的（最多 50 个字符）' },
        group_name: { type: 'string', description: '资源组名称，必须是你所在的资源组；传错会列出你能用的资源组' },
        instance_name: { type: 'string', description: '单实例导出；批量导出改传 instance_names' },
        instance_names: BATCH_NAMES,
        db_name: { type: 'string' },
        sql_content: { type: 'string', description: '导出用的一条 select / with 查询' },
        export_format: { type: 'string', enum: EXPORT_FORMATS, description: '导出格式，默认 csv' },
        export_merge: { type: 'boolean', description: '仅批量导出：各实例的数据合并写进同一个文件（要求各实例返回的列一致）；默认 false，每个实例各一个文件' },
        demand_url: { type: 'string', description: '需求链接，http(s):// 开头（可选）' },
        run_date_start: { type: 'string', description: '可执行时间窗开始，YYYY-MM-DD HH:MM[:SS]（可选）' },
        run_date_end: { type: 'string', description: '可执行时间窗结束，必须晚于开始（可选）' },
      },
      required: ['workflow_name', 'group_name', 'db_name', 'sql_content'],
    },
  },
  {
    name: 'list_workflows',
    description:
      '工单列表（只读）。kind: sql=上线工单（默认）/ export=导出工单 / all=两种都看；batch 可只看批量工单或只看单实例工单。'
      + '范围是你有权限看到的工单（普通账号看自己和所在资源组的）。status 可选：workflow_manreviewing(等待审批) / workflow_review_pass(审核通过) / '
      + 'workflow_timingtask(定时执行) / workflow_queuing(排队中) / workflow_executing(执行中) / workflow_finish(已正常结束) / '
      + 'workflow_exception(执行有异常) / workflow_abort(人工终止流程) / workflow_autoreviewwrong(自动审批不通过)，不传=全部。'
      + '批量工单的 instance 是“第一个实例名 等 N 个实例”，并带 batch_count。每行带 detail_url。',
    inputSchema: {
      type: 'object',
      properties: {
        kind: { type: 'string', enum: ['sql', 'export', 'all'], description: '默认 sql' },
        batch: { type: 'string', enum: ['1', '0'], description: '1 只看批量工单，0 只看单实例工单，不传=全部' },
        status: { type: 'string', description: '状态过滤，见上；不传=全部' },
        navStatus: { type: 'string', description: '同 status（别名）' },
        tab: { type: 'string', enum: ['mine', 'approve', 'exec'], description: 'mine=我提交的，approve=待我审批，exec=待我执行' },
        search: { type: 'string', description: '工单名 / 提交人 / 库 / 实例名；纯数字（可带 #）按工单号' },
        limit: { type: 'number', description: '默认 20，最多 100（dbtool 每页固定 20 条，超过会自动翻页）' },
        offset: { type: 'number' },
        instance_id: { type: 'number', description: '实例数字 id（可选）' },
        group_id: { type: 'number', description: '资源组 id（可选）' },
        start_date: { type: 'string', description: 'YYYY-MM-DD；只给一端时另一端自动补' },
        end_date: { type: 'string', description: 'YYYY-MM-DD' },
      },
    },
  },
  {
    name: 'workflow_detail',
    description:
      '工单详情（只读）：状态、审批流与当前审批人、检查结果、执行结果（逐条语句）。批量工单会列出每个实例各自的结果（success / failed / closed / pending）和汇总。'
      + 'workflow_id 是工单号，从 list_workflows 的行里取。',
    inputSchema: {
      type: 'object',
      properties: {
        workflow_id: { type: 'number', description: '工单号' },
        max_rows: { type: 'number', description: '检查结果和执行结果各最多返回多少行，默认 50，最多 200；超长时自动减少，优先保留汇总行和出错的行' },
      },
      required: ['workflow_id'],
    },
  },
  {
    name: 'workflow_log',
    description: '工单操作日志（只读）：谁在何时提交 / 审核 / 执行 / 终止。批量工单是一个工单一份日志。',
    inputSchema: { type: 'object', properties: { workflow_id: { type: 'number', description: '工单号' } }, required: ['workflow_id'] },
  },
  {
    name: 'cancel_workflow',
    description:
      '终止（撤回）工单。⚠ 会把工单变成“人工终止流程”，不能恢复（要再做只能重新提交），并通知相关人。'
      + '只在用户明确要求终止某张具体工单时使用：先用 workflow_detail 把工单名、目标实例和当前状态给用户看，并得到明确同意。'
      + '限制（不满足就直接拒绝，不会对工单发出任何请求）：① 只能撤回【自己提交的】工单，别人的工单请审批人到网页上驳回；'
      + '② 只接受“等待审批 / 审核通过 / 定时执行”的工单（排队中、执行中、已结束的不能终止，取消正在执行的也不在这里）；'
      + '③ 必须填终止原因 remark（最多 500 个字符，平台上显示为“<原因>（经 MCP 操作）”）。'
      + '上线工单、导出工单、单实例、批量都可以，终止的是整张工单（批量工单是一个工单覆盖多个实例）；定时执行的计划随工单一起清除。'
      + 'dbtool 服务端还会按自己的规则再校验一遍（终止按钮权限等）。',
    inputSchema: {
      type: 'object',
      properties: {
        workflow_id: { type: 'number', description: '工单号' },
        remark: { type: 'string', description: '终止原因（必填，最多 500 个字符）；平台上会显示为“<原因>（经 MCP 操作）”' },
      },
      required: ['workflow_id', 'remark'],
    },
  },
  // ── 高危：批量审核通过 / 批量执行，仅超级管理员 ──
  {
    name: 'approve_workflow',
    description:
      '【高危，会改变工单状态】审核通过一张【批量工单】的当前这一级审批（多级审批要再调用一次）。'
      + '只在用户明确要求审核通过某张具体工单时使用，调用前先用 workflow_detail 把工单名、目标实例和 SQL 给用户看，并得到明确同意。'
      + '限制（不满足就直接拒绝，不会对工单发出任何请求）：① 只接受超级管理员账号，每次调用前都会重新登录核对；② 只接受批量工单；'
      + '③ 提交人是当前账号自己的工单一律拒绝（超级管理员也一样）；④ 必须再传一次 confirm_workflow_id，且等于 workflow_id。'
      + 'dbtool 服务端还会按自己的规则（审批按钮权限、审批流当前级）再校验一遍。',
    inputSchema: {
      type: 'object',
      properties: {
        workflow_id: { type: 'number', description: '工单号' },
        confirm_workflow_id: { type: 'number', description: '再传一次工单号确认，必须等于 workflow_id' },
        remark: { type: 'string', description: '审批备注（可选）；平台上会显示为“<备注>（经 MCP 操作）”' },
      },
      required: ['workflow_id', 'confirm_workflow_id'],
    },
  },
  {
    name: 'execute_workflow',
    description:
      '【高危，会改动目标库】立即执行一张已审核通过的【批量工单】（所有实例，后台异步执行；用 workflow_detail 看每个实例的结果）。'
      + '只在用户明确要求执行某张具体工单时使用，调用前先用 workflow_detail 把工单名、目标实例和 SQL 给用户看，并得到明确同意。'
      + '限制（不满足就直接拒绝，不会对工单发出任何请求）：① 只接受超级管理员账号，每次调用前都会重新登录核对；② 只接受批量工单；'
      + '③ 工单状态必须是“审核通过”（定时执行的不接受）；④ 必须再传一次 confirm_workflow_id，且等于 workflow_id。'
      + '不含定时执行、手工确认结束、失败实例的重试 / 关闭。dbtool 服务端还会校验执行权限、可执行时间窗和同库 DDL 冲突。',
    inputSchema: {
      type: 'object',
      properties: {
        workflow_id: { type: 'number', description: '工单号' },
        confirm_workflow_id: { type: 'number', description: '再传一次工单号确认，必须等于 workflow_id' },
        executor: { type: 'string', enum: ['goinception', 'native'], description: '执行方式（可选）；不传用 dbtool 的缺省（SQL Server 只有原生执行）' },
      },
      required: ['workflow_id', 'confirm_workflow_id'],
    },
  },
];

const HANDLERS = {
  list_instances: toolInstances,
  list_databases: (a) => toolResource({ ...a, resource_type: 'database' }),
  list_tables: (a) => toolResource({ ...a, resource_type: 'table' }),
  list_columns: (a) => toolResource({ ...a, resource_type: 'column' }),
  describe_table: toolDescribeTable,
  sql_query: toolQuery,
  query_log: toolQueryLog,
  slow_log: toolSlowLog,
  slow_log_detail: toolSlowLogDetail,
  dict_table_info: toolDictTableInfo,
  partman_list: toolPartmanList,
  sql_explain: toolExplain,
  sql_optimize: toolOptimize,
  sql_analyze: toolAnalyze,
  sql_check: toolSqlCheck,
  submit_sql_workflow: toolSubmitSqlWorkflow,
  submit_export_workflow: toolSubmitExportWorkflow,
  list_workflows: toolListWorkflows,
  workflow_detail: toolWorkflowDetail,
  workflow_log: toolWorkflowLog,
  cancel_workflow: toolCancelWorkflow,
  approve_workflow: toolApproveWorkflow,
  execute_workflow: toolExecuteWorkflow,
};

// ── MCP stdio 模式 ──────────────────────────────────────────────────────
const SUPPORTED_PROTOCOLS = ['2025-06-18', '2025-03-26', '2024-11-05'];

function reply(msg) { process.stdout.write(`${JSON.stringify(msg)}\n`); }

async function handleRpc(req) {
  const { method, params } = req;
  if (method === 'initialize') {
    const want = params && params.protocolVersion;
    return {
      protocolVersion: SUPPORTED_PROTOCOLS.includes(want) ? want : '2024-11-05',
      capabilities: { tools: {} },
      serverInfo: { name: MCP_NODE, version: VERSION },
    };
  }
  if (method === 'tools/list') return { tools: TOOLS };
  if (method === 'tools/call') {
    const name = params && params.name;
    const fn = HANDLERS[name];
    if (!fn) throw new Error(`未知工具: ${name}`);
    const text = await fn((params && params.arguments) || {});
    return { content: [{ type: 'text', text }] };
  }
  if (method === 'ping') return {};
  throw Object.assign(new Error(`不支持的方法: ${method}`), { code: -32601 });
}

function runMcp() {
  let acc = '';
  let inFlight = 0;
  let stdinEnded = false;
  const maybeExit = () => { if (stdinEnded && inFlight === 0) process.exit(0); };

  process.stdin.setEncoding('utf8');
  process.stdin.on('data', (chunk) => {
    acc += chunk;
    let nl;
    while ((nl = acc.indexOf('\n')) >= 0) {
      const line = acc.slice(0, nl).trim();
      acc = acc.slice(nl + 1);
      if (!line) continue;

      let req;
      try { req = JSON.parse(line); } catch { continue; }
      if (req.id === undefined || req.id === null) continue; // 通知（如 notifications/initialized）不需要回复

      inFlight++;
      handleRpc(req).then(
        (result) => reply({ jsonrpc: '2.0', id: req.id, result }),
        (err) => {
          if (req.method === 'tools/call') {
            reply({ jsonrpc: '2.0', id: req.id, result: { content: [{ type: 'text', text: `错误: ${redact(err.message)}` }], isError: true } });
          } else {
            reply({ jsonrpc: '2.0', id: req.id, error: { code: err.code || -32603, message: redact(err.message) } });
          }
        },
      ).finally(() => { inFlight--; maybeExit(); });
    }
  });
  process.stdin.on('end', () => { stdinEnded = true; maybeExit(); });
}

// ── CLI 模式 ────────────────────────────────────────────────────────────
const USAGE = `dbtool 查询（CLI 模式，版本 ${VERSION}）

  查询组:
    node index.js --cli instances [keyword]
    node index.js --cli dbs <instance_name>
    node index.js --cli tables <instance_name> <db_name>
    node index.js --cli columns <instance_name> <db_name> <tb_name>
    node index.js --cli desc <instance_name> <db_name> <tb_name>
    node index.js --cli dict <instance_name> <db_name> <tb_name>
    node index.js --cli query <instance_name> <db_name> <sql> [limit]
    node index.js --cli querylog [search]
    node index.js --cli slowlog [instance_id] [start_date] [end_date]
    node index.js --cli slowdetail <instance_id> <sql_md5> [db_name] [exec_date]
    node index.js --cli partman [search]
  分析组:
    node index.js --cli explain <instance_name> <db_name> <sql>
    node index.js --cli optimize <engine> <instance_name> <db_name> <sql>
    node index.js --cli analyze <text>
  工单组（只读）:
    node index.js --cli check <instance_name[,instance_name…]> <db_name> <sql>      逗号分隔多个实例 = 批量检查
    node index.js --cli checkexport <instance_name[,instance_name…]> <db_name> <select_sql> [merge]
                                                                                    数据导出预检（只读；会在每个实例上统计一次待导出行数），merge = 核对各实例的列是否一致
    node index.js --cli wfs [sql|export|all] [status] [search]
    node index.js --cli wfdetail <workflow_id>
    node index.js --cli wflog <workflow_id>
  工单组（会创建真实工单并通知审核人；逗号分隔多个实例 = 批量工单，只创建一个；创建前都会自动检查，不通过就不创建）:
    node index.js --cli submit <group_name> <instance_name[,instance_name…]> <db_name> <title> <sql>
    node index.js --cli export <group_name> <instance_name[,instance_name…]> <db_name> <title> <select_sql> [csv|xlsx|json|xml|sql] [merge]
                                                                                    数据导出工单；多个实例 = 批量导出（只创建一个工单），merge = 合并成一个文件
  工单组（高危，仅超级管理员，只接受批量工单；两个参数都是工单号：第二个是再确认一次）:
    node index.js --cli approve <workflow_id> <confirm_workflow_id> [remark]
    node index.js --cli execute <workflow_id> <confirm_workflow_id> [goinception|native]
  工单组（会终止工单，不能恢复；只能撤回自己提交的，只接受等待审批 / 审核通过 / 定时执行的工单）:
    node index.js --cli cancel <workflow_id> <remark>                                终止原因必填
`;

async function runCli(argv) {
  const [cmd, ...a] = argv;
  const map = {
    instances: () => toolInstances({ keyword: a[0] }),
    dbs: () => toolResource({ instance_name: a[0], resource_type: 'database' }),
    tables: () => toolResource({ instance_name: a[0], db_name: a[1], resource_type: 'table' }),
    columns: () => toolResource({ instance_name: a[0], db_name: a[1], tb_name: a[2], resource_type: 'column' }),
    desc: () => toolDescribeTable({ instance_name: a[0], db_name: a[1], tb_name: a[2] }),
    dict: () => toolDictTableInfo({ instance_name: a[0], db_name: a[1], tb_name: a[2] }),
    query: () => toolQuery({ instance_name: a[0], db_name: a[1], sql_content: a[2], limit_num: a[3] }),
    querylog: () => toolQueryLog({ search: a[0] }),
    slowlog: () => toolSlowLog({ instance_id: a[0], start_date: a[1], end_date: a[2] }),
    slowdetail: () => toolSlowLogDetail({ instance_id: a[0], sql_md5: a[1], db_name: a[2], exec_date: a[3] }),
    partman: () => toolPartmanList({ search: a[0] }),
    explain: () => toolExplain({ instance_name: a[0], db_name: a[1], sql_content: a[2] }),
    optimize: () => toolOptimize({ engine: a[0], instance_name: a[1], db_name: a[2], sql_content: a[3] }),
    analyze: () => toolAnalyze({ text: a[0] }),
    check: () => toolSqlCheck({ instance_names: asList(a[0]).length > 1 ? a[0] : undefined, instance_name: asList(a[0]).length > 1 ? undefined : a[0], db_name: a[1], sql_content: a[2] }),
    checkexport: () => {
      if (a[3] !== undefined && a[3] !== 'merge') throw new Error('第 4 个参数只能是 merge（核对各实例返回的列是否一致）');
      return toolSqlCheck({
        instance_names: asList(a[0]).length > 1 ? a[0] : undefined,
        instance_name: asList(a[0]).length > 1 ? undefined : a[0],
        db_name: a[1],
        sql_content: a[2],
        export: true,
        export_merge: a[3] === 'merge' ? true : undefined,
      });
    },
    wfs: () => toolListWorkflows({ kind: a[0] || 'sql', status: a[1], search: a[2] }),
    wfdetail: () => toolWorkflowDetail({ workflow_id: a[0] }),
    wflog: () => toolWorkflowLog({ workflow_id: a[0] }),
    submit: () => toolSubmitSqlWorkflow({ group_name: a[0], instance_names: asList(a[1]).length > 1 ? a[1] : undefined, instance_name: asList(a[1]).length > 1 ? undefined : a[1], db_name: a[2], workflow_name: a[3], sql_content: a[4] }),
    export: () => {
      if (a[6] !== undefined && a[6] !== 'merge') throw new Error('第 7 个参数只能是 merge（合并成一个文件）');
      return toolSubmitExportWorkflow({
        group_name: a[0],
        instance_names: asList(a[1]).length > 1 ? a[1] : undefined,
        instance_name: asList(a[1]).length > 1 ? undefined : a[1],
        db_name: a[2],
        workflow_name: a[3],
        sql_content: a[4],
        export_format: a[5],
        export_merge: a[6] === 'merge' ? true : undefined,
      });
    },
    cancel: () => toolCancelWorkflow({ workflow_id: a[0], remark: a[1] }),
    approve: () => toolApproveWorkflow({ workflow_id: a[0], confirm_workflow_id: a[1], remark: a[2] }),
    execute: () => toolExecuteWorkflow({ workflow_id: a[0], confirm_workflow_id: a[1], executor: a[2] }),
  };
  const fn = map[cmd];
  if (!fn) {
    console.log(USAGE);
    return cmd ? 1 : 0;
  }
  try {
    console.log(await fn());
    return 0;
  } catch (e) {
    console.error(`错误: ${redact(e.message)}`);
    return 1;
  }
}

if (require.main === module) {
  const i = process.argv.indexOf('--cli');
  if (i >= 0) {
    runCli(process.argv.slice(i + 1)).then((code) => process.exit(code));
  } else {
    runMcp();
  }
}

module.exports = { TOOLS, HANDLERS, stringifyResult, renderDetail, renderAdvice, assertReadOnlySql };
