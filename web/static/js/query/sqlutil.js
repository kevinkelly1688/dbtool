/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: sqlutil.js
 * @time: 2026/10/28
 */
// 生成 SQL 时用的标识符引用与方言差异（MySQL 系用反引号，SQL Server 用方括号、行数限制用 TOP，
// PostgreSQL 用双引号且表名是 "schema.table"，ClickHouse 用反引号，Oracle / 达梦用双引号（没加引号的名字是大写）、行数限制用 ROWNUM，MongoDB 生成 shell 风格语句）。

const RESERVED = new Set(['order', 'group', 'key', 'index', 'select', 'from', 'where', 'table', 'desc', 'rank', 'row', 'rows', 'condition', 'range', 'read', 'write', 'limit', 'interval', 'match', 'user', 'values', 'primary', 'check', 'case']);
// PostgreSQL / ClickHouse 引擎会拒绝未加引号的这些词，当列名 / 别名时必须加引号。
const PG_EXTRA = ['insert', 'update', 'delete', 'merge', 'truncate', 'into', 'uescape'];
const CH_EXTRA = ['settings', 'format', 'into', 'outfile', 'infile'];
// Oracle / 达梦：这些词当列名 / 别名必须加双引号（宁多勿漏：给不是保留字的名字加引号只是多两个引号，名字本身是大写时含义不变）。
const ORA_RESERVED = new Set(['access', 'add', 'all', 'alter', 'and', 'any', 'as', 'asc', 'audit', 'between', 'by', 'char', 'check', 'cluster', 'column', 'comment', 'compress', 'connect', 'create', 'current', 'date', 'decimal', 'default', 'delete', 'desc', 'distinct', 'drop', 'else', 'exclusive', 'exists', 'file', 'float', 'for', 'from', 'grant', 'group', 'having', 'identified', 'immediate', 'in', 'increment', 'index', 'initial', 'insert', 'integer', 'intersect', 'into', 'is', 'level', 'like', 'lock', 'long', 'maxextents', 'merge', 'minus', 'mode', 'modify', 'noaudit', 'nocompress', 'not', 'nowait', 'null', 'number', 'of', 'offline', 'on', 'online', 'option', 'or', 'order', 'pctfree', 'prior', 'public', 'raw', 'rename', 'resource', 'revoke', 'row', 'rowid', 'rownum', 'rows', 'select', 'session', 'set', 'share', 'size', 'smallint', 'start', 'successful', 'synonym', 'sysdate', 'table', 'then', 'to', 'trigger', 'truncate', 'uid', 'union', 'unique', 'update', 'user', 'validate', 'values', 'varchar', 'varchar2', 'view', 'whenever', 'where', 'with']);

export const isMSSQL = (type) => type === 'mssql';
export const isRedis = (type) => type === 'redis';
export const isPG = (type) => type === 'pgsql';
export const isMongo = (type) => type === 'mongo';
export const isCH = (type) => type === 'clickhouse';
export const isOracle = (type) => type === 'oracle';
export const isDM = (type) => type === 'dm';
// Oracle 系（Oracle、达梦）：没加引号的名字是大写，库里的真实名字大小写要照写，行数限制用 ROWNUM。
export const isOra = (type) => isOracle(type) || isDM(type);
// PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦：没有格式化、EXPLAIN 当普通语句运行。
export const isMulti = (type) => isPG(type) || isMongo(type) || isCH(type) || isOra(type);
// 没有“执行计划”页签的类型（Redis 无此概念；SQL Server 暂不支持；新引擎的 EXPLAIN 是普通语句）。
export const noPlanType = (type) => isMSSQL(type) || isRedis(type) || isMulti(type);

export const TYPE_NAME = { pgsql: 'PostgreSQL', mongo: 'MongoDB', clickhouse: 'ClickHouse', mssql: 'SQL Server', redis: 'Redis', oracle: 'Oracle', dm: 'DM' };

const SIMPLE = /^[A-Za-z_][A-Za-z0-9_$]*$/;
const PG_SIMPLE = /^[a-z_][a-z0-9_$]*$/; // PG 未加引号的标识符会折叠为小写，含大写的名字必须加引号
const ORA_SIMPLE = /^[A-Z][A-Z0-9_$#]*$/; // Oracle / 达梦未加引号的标识符会折叠为大写，含小写的名字必须加引号

// 单个标识符。简单名字原样输出，其余加引号。
export function quoteId(name, type) {
  if (isMSSQL(type)) return /^[A-Za-z_][A-Za-z0-9_]*$/.test(name) && !RESERVED.has(name.toLowerCase()) ? name : '[' + name.replace(/]/g, ']]') + ']';
  if (isPG(type)) {
    const l = name.toLowerCase();
    return PG_SIMPLE.test(name) && !RESERVED.has(l) && !PG_EXTRA.includes(l) ? name : '"' + name.replace(/"/g, '""') + '"';
  }
  if (isCH(type)) {
    const l = name.toLowerCase();
    return SIMPLE.test(name) && !RESERVED.has(l) && !CH_EXTRA.includes(l) ? name : '`' + name.replace(/`/g, '``') + '`';
  }
  if (isOra(type)) return ORA_SIMPLE.test(name) && !ORA_RESERVED.has(name.toLowerCase()) ? name : '"' + name.replace(/"/g, '""') + '"';
  if (isMongo(type)) return SIMPLE.test(name) ? name : JSON.stringify(name); // 字段名：简单名字原样，其余（含点号路径）用 JSON 字符串
  return /^[A-Za-z_][A-Za-z0-9_$]*$/.test(name) && !RESERVED.has(name.toLowerCase()) ? name : '`' + name.replace(/`/g, '``') + '`';
}

// 表名。SQL Server / PostgreSQL 的表名形如 "schema.table"，逐段引用（PG 只按第一个点切分，没有点的补 public）。
export function quoteTable(name, type) {
  if (isPG(type)) {
    const i = name.indexOf('.');
    return i > 0 ? quoteId(name.slice(0, i), type) + '.' + quoteId(name.slice(i + 1), type) : 'public.' + quoteId(name, type);
  }
  if (isMSSQL(type)) {
    const i = name.lastIndexOf('.');
    if (i > 0) return quoteId(name.slice(0, i), type) + '.' + quoteId(name.slice(i + 1), type);
  }
  return quoteId(name, type);
}

// MongoDB 的集合引用：标识符形式的用 db.<集合>，其余（含点、横线、与 db 方法重名）用 db.getCollection("名称")。
const MONGO_DB_METHODS = new Set(['stats', 'getCollection', 'getCollectionNames', 'getMongo', 'getName', 'getSiblingDB', 'runCommand', 'aggregate']);
export function mongoColl(name) {
  return SIMPLE.test(name) && !MONGO_DB_METHODS.has(name) ? 'db.' + name : 'db.getCollection(' + JSON.stringify(name) + ')';
}

// 生成 "SELECT 列 FROM 表 限制 n 行"；MongoDB 生成 db.<集合>.find({}).limit(n)。
export function selectSQL(table, cols, type, n = 100) {
  if (isMongo(type)) return `${mongoColl(table)}.find({}).limit(${Math.min(n, 10)})`;
  const list = cols && cols.length ? cols.map((c) => quoteId(c, type)).join(', ') : '1';
  if (isMSSQL(type)) return `SELECT TOP ${n} ${list}\nFROM ${quoteTable(table, type)};`;
  if (isOra(type)) return `SELECT ${list}\nFROM ${quoteTable(table, type)}\nWHERE ROWNUM <= ${n};`; // Oracle 12c 之前没有 FETCH FIRST / LIMIT；达梦同样认 ROWNUM
  return `SELECT ${list}\nFROM ${quoteTable(table, type)}\nLIMIT ${n};`;
}

// 估算行数的简短显示：1234 -> 1.2k，3_400_000 -> 3.4M；未知返回空串。
export function shortCount(n) {
  if (n == null || n < 0) return '';
  if (n >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, '') + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
  if (n >= 1e4) return (n / 1e3).toFixed(0) + 'k';
  if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'k';
  return String(n);
}

// 各引擎在 ACE 里的高亮模式（vendor 里只有 mysql / pgsql / sqlserver / sql / text）：
// Redis 命令、MongoDB 语句按纯文本显示，ClickHouse / Oracle / 达梦用通用 sql 模式，其余按 MySQL。
const ACE_MODE = { pgsql: 'ace/mode/pgsql', mssql: 'ace/mode/sqlserver', redis: 'ace/mode/text', mongo: 'ace/mode/text', clickhouse: 'ace/mode/sql', oracle: 'ace/mode/sql', dm: 'ace/mode/sql' };
export const aceMode = (type) => ACE_MODE[type] || 'ace/mode/mysql';
