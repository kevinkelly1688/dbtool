/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: samples.js
 * @time: 2026/10/28
 */
// 示例 SQL：按当前库里的第一张表生成，含正常查询、规则示例和会被拦截的反例。
import { t } from '../i18n.js';
import { isMSSQL, isRedis, isPG, isMongo, isCH, isDM, isOra, quoteId, quoteTable, mongoColl } from './sqlutil.js';

// Redis 示例命令：key 名是占位符，按实际 key 替换；最后一组是会被拦截的反例（用来看拦截原因）。
function redisSamples() {
  const out = [];
  const add = (group, label, sql) => out.push({ group, label, sql });
  const gBrowse = t('浏览 key');
  const gString = t('字符串');
  const gHash = t('哈希');
  const gList = t('列表');
  const gSet = t('集合|set');
  const gZset = t('有序集合');
  const gRule = t('规则示例');
  const gBlock = t('会被拦截');
  add(gBrowse, t('SCAN：分批列出 key'), 'SCAN 0 MATCH * COUNT 100');
  add(gBrowse, t('SCAN：按前缀匹配'), 'SCAN 0 MATCH user:* COUNT 100');
  add(gBrowse, t('SCAN：只看某种类型'), 'SCAN 0 TYPE hash COUNT 100');
  add(gBrowse, t('DBSIZE：当前库 key 数'), 'DBSIZE');
  add(gString, 'GET', 'GET mykey');
  add(gString, t('MGET：一次读多个'), 'MGET key1 key2 key3');
  add(gString, 'STRLEN / TTL / TYPE', 'TTL mykey');
  add(gString, 'OBJECT ENCODING', 'OBJECT ENCODING mykey');
  add(gString, t('MEMORY USAGE：占用字节'), 'MEMORY USAGE mykey');
  add(gHash, t('HGET：读一个字段'), 'HGET myhash field');
  add(gHash, t('HGETALL（超过 5000 个字段会被拒绝）'), 'HGETALL myhash');
  add(gHash, t('HSCAN：分页读取'), 'HSCAN myhash 0 COUNT 100');
  add(gList, t('LRANGE：前 100 个'), 'LRANGE mylist 0 99');
  add(gList, t('LRANGE：最后 10 个'), 'LRANGE mylist -10 -1');
  add(gList, 'LLEN / LINDEX', 'LLEN mylist');
  add(gSet, t('SMEMBERS（超过 5000 个成员会被拒绝）'), 'SMEMBERS myset');
  add(gSet, t('SSCAN：分页读取'), 'SSCAN myset 0 COUNT 100');
  add(gSet, 'SISMEMBER', 'SISMEMBER myset member');
  add(gZset, t('ZRANGE：前 100 名（带分数）'), 'ZRANGE myzset 0 99 WITHSCORES');
  add(gZset, t('ZREVRANGE：分数最高的 10 个'), 'ZREVRANGE myzset 0 9 WITHSCORES');
  add(gZset, t('ZRANGEBYSCORE：按分数区间'), 'ZRANGEBYSCORE myzset -inf +inf WITHSCORES LIMIT 0 100');
  add(gZset, 'ZSCORE / ZRANK / ZCARD', 'ZSCORE myzset member');
  add('Stream', 'XLEN / XRANGE', 'XRANGE mystream - + COUNT 100');
  add('Stream', t('XREVRANGE：最新 10 条'), 'XREVRANGE mystream + - COUNT 10');
  add(gRule, t('COUNT 超过上限（自动改为 1000）'), 'SCAN 0 COUNT 100000');
  add(gRule, t('LRANGE 区间过大（自动收窄）'), 'LRANGE mylist 0 99999');
  add(gBlock, t('SET（写命令）'), 'SET mykey value');
  add(gBlock, t('DEL（写命令）'), 'DEL mykey');
  add(gBlock, t('KEYS *（会阻塞 Redis）'), 'KEYS *');
  add(gBlock, 'FLUSHDB', 'FLUSHDB');
  add(gBlock, t('CONFIG GET（运维命令）'), 'CONFIG GET *');
  add(gBlock, t('INFO（请用实例巡检）'), 'INFO');
  add(gBlock, t('EVAL（脚本）'), 'EVAL "return 1" 0');
  add(gBlock, t('SELECT（库在页面上选）'), 'SELECT 1');
  add(gBlock, t('一次执行多条命令'), 'GET a\nGET b');
  return out;
}

// PostgreSQL 示例：表名带 schema（引擎要求用 schema.table 访问非 public 的表）。
function pgSamples(ctx) {
  const out = [];
  const add = (group, label, sql) => out.push({ group, label, sql });
  const gOK = t('正常查询');
  const gRule = t('规则示例');
  const gBlock = t('会被拦截');
  const tn = ctx.table;
  const T = tn ? quoteTable(tn, 'pgsql') : '';
  const cols = (ctx.cols && ctx.cols.length ? ctx.cols : []).slice(0, 8);
  const list = cols.length ? cols.map((c) => quoteId(c, 'pgsql')).join(', ') : '1';
  const pk = ctx.pk ? quoteId(ctx.pk, 'pgsql') : '';
  const order = pk ? `\nORDER BY ${pk} DESC` : '';
  add(gOK, t('无表查询：SELECT 1'), 'select 1');
  if (tn) {
    add(gOK, t('前 50 行：{table}', { table: tn }), `select ${list}\nfrom ${T}${order}\nlimit 50;`);
    add(gOK, t('统计行数'), `select count(*) as cnt\nfrom ${T};`);
    add(gOK, t('WITH（CTE）示例'), `with t as (\n  select ${list}\n  from ${T}${order}\n  limit 100\n)\nselect count(*) as cnt from t;`);
    add(gOK, t('EXPLAIN（普通语句，不带 ANALYZE）'), `explain select ${list}\nfrom ${T}\nlimit 10;`);
  }
  add(gOK, t('information_schema（系统 schema 免表权限）'), 'select table_schema, table_name, table_type\nfrom information_schema.tables\nlimit 10;');
  add(gOK, t('pg_stat_user_tables：行数排行'), 'select schemaname, relname, n_live_tup\nfrom pg_stat_user_tables\norder by n_live_tup desc\nlimit 20;');
  add(gOK, t('SHOW 参数名'), 'show server_version');
  add(gRule, t('没带 LIMIT（自动补 LIMIT）'), tn ? `select ${list}\nfrom ${T};` : 'select generate_series(1, 5000);');
  add(gRule, t('多语句（只运行光标所在的一条）'), 'select 1;\nselect 2;');
  add(gBlock, t('INSERT（写操作）'), tn ? `insert into ${T} (${cols.length ? quoteId(cols[0], 'pgsql') : 'id'}) values (1);` : 'insert into public.t values (1)');
  add(gBlock, t('UPDATE（写操作）'), tn ? `update ${T} set ${cols.length ? quoteId(cols[0], 'pgsql') : 'id'} = 1 where false;` : 'update public.t set id = 1');
  add(gBlock, t('pg_sleep()（危险函数）'), 'select pg_sleep(5)');
  add(gBlock, t('pg_authid（敏感系统视图）'), 'select * from pg_authid');
  add(gBlock, t('EXPLAIN ANALYZE（会真实执行）'), 'explain analyze select 1');
  add(gBlock, t('FOR UPDATE（行锁）'), tn ? `select ${list} from ${T} for update;` : 'select 1 for update');
  add(gBlock, t('SET（改会话参数）'), 'set statement_timeout = 0');
  return out;
}

// ClickHouse 示例：库在页面上选，表名不带库名。
function chSamples(ctx) {
  const out = [];
  const add = (group, label, sql) => out.push({ group, label, sql });
  const gOK = t('正常查询');
  const gRule = t('规则示例');
  const gBlock = t('会被拦截');
  const tn = ctx.table;
  const T = tn ? quoteTable(tn, 'clickhouse') : '';
  const cols = (ctx.cols && ctx.cols.length ? ctx.cols : []).slice(0, 8);
  const list = cols.length ? cols.map((c) => quoteId(c, 'clickhouse')).join(', ') : '1';
  const pk = ctx.pk ? quoteId(ctx.pk, 'clickhouse') : '';
  const order = pk ? `\nORDER BY ${pk} DESC` : '';
  add(gOK, t('版本：SELECT version()'), 'select version()');
  add(gOK, t('numbers(10)：生成数字'), 'select * from numbers(10)');
  if (tn) {
    add(gOK, t('前 50 行：{table}', { table: tn }), `select ${list}\nfrom ${T}${order}\nlimit 50`);
    add(gOK, t('统计行数'), `select count() as cnt\nfrom ${T}`);
    add(gOK, t('结构：DESCRIBE {table}', { table: tn }), `describe ${T}`);
    add(gOK, t('结构：SHOW CREATE TABLE {table}', { table: tn }), `show create table ${T}`);
    add(gOK, t('EXPLAIN（普通语句）'), `explain select count()\nfrom ${T}`);
  }
  add(gOK, t('SHOW TABLES（当前库）'), 'show tables');
  add(gOK, t('system.tables（system 库只开放摘要类表）'), 'select database, name, engine, total_rows\nfrom system.tables\nlimit 20');
  add(gRule, t('末尾 FORMAT 被忽略（输出格式固定）'), 'select 1 format JSON');
  add(gRule, t('多语句（只运行光标所在的一条）'), 'select 1;\nselect 2;');
  add(gBlock, t('SETTINGS 子句'), 'select 1 settings readonly = 0');
  add(gBlock, t('system.users（敏感系统表）'), 'select * from system.users');
  add(gBlock, t('file() 表函数（读服务器文件）'), "select * from file('/etc/passwd')");
  add(gBlock, t('remote() 表函数（连其它节点）'), "select * from remote('host', db, t)");
  add(gBlock, t('merge()（无法确定涉及的表）'), "select * from merge(currentDatabase(), '^t')");
  add(gBlock, t('sleep()（危险函数）'), 'select sleep(3)');
  add(gBlock, t('INSERT（写操作）'), 'insert into t values (1)');
  add(gBlock, t('OPTIMIZE TABLE（管理语句）'), tn ? `optimize table ${T}` : 'optimize table t');
  return out;
}

// Oracle / 达梦示例：库（schema）在页面上选，表名不带 schema；行数限制用 ROWNUM（排序后取前 N 行要套一层子查询）。
function oraSamples(ctx) {
  const dm = isDM(ctx.dbType);
  const out = [];
  const add = (group, label, sql) => out.push({ group, label, sql });
  const gOK = t('正常查询');
  const gRule = t('规则示例');
  const gBlock = t('会被拦截');
  const tn = ctx.table;
  const T = tn ? quoteTable(tn, ctx.dbType) : '';
  const cols = (ctx.cols && ctx.cols.length ? ctx.cols : []).slice(0, 8);
  const list = cols.length ? cols.map((c) => quoteId(c, ctx.dbType)).join(', ') : '1';
  const pk = ctx.pk ? quoteId(ctx.pk, ctx.dbType) : '';
  add(gOK, t('当前时间：SELECT SYSDATE FROM DUAL'), 'SELECT SYSDATE FROM DUAL');
  if (tn) {
    add(gOK, t('前 50 行：{table}', { table: tn }), pk ? `SELECT * FROM (\n  SELECT ${list}\n  FROM ${T}\n  ORDER BY ${pk} DESC\n)\nWHERE ROWNUM <= 50` : `SELECT ${list}\nFROM ${T}\nWHERE ROWNUM <= 50`);
    add(gOK, t('统计行数'), `SELECT COUNT(*) AS cnt\nFROM ${T}`);
    add(gOK, dm ? t('EXPLAIN（执行计划）') : t('EXPLAIN PLAN FOR（执行计划）'), dm ? `EXPLAIN SELECT COUNT(*)\nFROM ${T}` : `EXPLAIN PLAN FOR SELECT COUNT(*)\nFROM ${T}`);
  }
  add(gOK, t('数据字典：当前库的表（ALL_TABLES）'), 'SELECT table_name, num_rows\nFROM all_tables\nWHERE owner = USER\nORDER BY table_name');
  add(gOK, t('数据字典：列（ALL_TAB_COLUMNS）'), tn ? `SELECT column_name, data_type, nullable\nFROM all_tab_columns\nWHERE table_name = '${tn.replace(/'/g, "''")}'\nORDER BY column_id` : 'SELECT table_name, column_name, data_type\nFROM all_tab_columns\nWHERE ROWNUM <= 50');
  add(gRule, t('多语句（只运行光标所在的一条）'), 'SELECT 1 FROM DUAL;\nSELECT 2 FROM DUAL;');
  add(gRule, t('只放行内置函数（NVL / DECODE / TO_CHAR …）'), "SELECT NVL(NULL, 'x') a, TO_CHAR(SYSDATE, 'YYYY-MM-DD') b FROM DUAL");
  add(gBlock, t('INSERT（写操作）'), tn ? `INSERT INTO ${T} VALUES (1)` : 'INSERT INTO t VALUES (1)');
  add(gBlock, t('UPDATE（写操作）'), tn ? `UPDATE ${T} SET ${cols.length ? quoteId(cols[0], ctx.dbType) : 'id'} = 1 WHERE 1 = 0` : 'UPDATE t SET id = 1');
  add(gBlock, t('DROP TABLE（改结构）'), tn ? `DROP TABLE ${T}` : 'DROP TABLE t');
  add(gBlock, t('FOR UPDATE（行锁）'), tn ? `SELECT ${list} FROM ${T} FOR UPDATE` : 'SELECT 1 FROM DUAL FOR UPDATE');
  add(gBlock, t('自定义函数 / 包（按定义者权限执行）'), 'SELECT my_func(1) FROM DUAL');
  add(gBlock, t('DBMS_LOCK.SLEEP（包函数）'), 'SELECT DBMS_LOCK.SLEEP(5) FROM DUAL');
  add(gBlock, t('数据库链接（@链接名）'), tn ? `SELECT * FROM ${T}@other_db` : 'SELECT * FROM t@other_db');
  add(gBlock, t('DBA_USERS（数据库内部视图）'), 'SELECT * FROM dba_users');
  add(gBlock, dm ? t('V$INSTANCE（动态性能视图）') : t('V$SESSION（动态性能视图）'), dm ? 'SELECT * FROM v$instance' : 'SELECT * FROM v$session');
  add(gBlock, t('SYS 里的对象（系统 schema）'), 'SELECT * FROM sys.user$');
  add(gBlock, t('PL/SQL 块'), 'BEGIN NULL; END;');
  return out;
}

// MongoDB 示例：一次一条 shell 风格语句；库在页面上选。集合名取左侧第一个集合，没有时用占位名 orders。
function mongoSamples(ctx) {
  const out = [];
  const add = (group, label, sql) => out.push({ group, label, sql });
  const gOK = t('正常查询');
  const gRule = t('规则示例');
  const gBlock = t('会被拦截');
  const C = mongoColl(ctx.table || 'orders');
  add(gOK, t('find：前 10 条'), `${C}.find({}).limit(10)`);
  add(gOK, t('find：条件 + 投影 + 排序'), `${C}.find({status: "paid"}, {_id: 1, status: 1}).sort({_id: -1}).limit(20)`);
  add(gOK, 'findOne', `${C}.findOne({})`);
  add(gOK, 'countDocuments', `${C}.countDocuments({})`);
  add(gOK, 'distinct', `${C}.distinct("status")`);
  add(gOK, t('aggregate：分组统计'), `${C}.aggregate([{$match: {status: "paid"}}, {$group: {_id: "$uid", n: {$sum: 1}}}, {$sort: {n: -1}}, {$limit: 10}])`);
  add(gOK, t('getIndexes：索引'), `${C}.getIndexes()`);
  add(gOK, t('stats：集合统计'), `${C}.stats()`);
  add(gOK, t('getCollectionNames（需库级权限）'), 'db.getCollectionNames()');
  add(gRule, t('find 不带 limit（自动收窄到行数上限）'), `${C}.find({})`);
  add(gRule, t('第二条语句（拒绝：一次只能一条）'), `${C}.find({}).limit(1);\n${C}.find({}).limit(2)`);
  add(gBlock, t('insertOne（写操作）'), `${C}.insertOne({})`);
  add(gBlock, t('drop（删集合）'), `${C}.drop()`);
  add(gBlock, t('runCommand（原始命令）'), 'db.runCommand({ping: 1})');
  add(gBlock, t('$where（服务端 JavaScript）'), `${C}.find({$where: "this.a > 1"})`);
  add(gBlock, t('aggregate 的 $out（写集合）'), `${C}.aggregate([{$match: {}}, {$out: "x"}])`);
  add(gBlock, t('aggregate 的 $merge（写集合）'), `${C}.aggregate([{$merge: {into: "x"}}])`);
  add(gBlock, t('getSiblingDB（切库请用页面库下拉）'), 'db.getSiblingDB("admin").x.find()');
  add(gBlock, t('system.* 集合'), 'db.system.users.find()');
  return out;
}

export const REDIS_COMMANDS = ['GET', 'MGET', 'STRLEN', 'GETRANGE', 'EXISTS', 'TYPE', 'TTL', 'PTTL', 'EXPIRETIME', 'PEXPIRETIME',
  'HGET', 'HMGET', 'HGETALL', 'HLEN', 'HKEYS', 'HVALS', 'HEXISTS', 'HSTRLEN', 'HSCAN', 'HRANDFIELD', 'LRANGE', 'LLEN', 'LINDEX', 'LPOS',
  'SMEMBERS', 'SCARD', 'SISMEMBER', 'SMISMEMBER', 'SRANDMEMBER', 'SSCAN', 'SINTER', 'SUNION', 'SDIFF',
  'ZRANGE', 'ZREVRANGE', 'ZRANGEBYSCORE', 'ZREVRANGEBYSCORE', 'ZRANGEBYLEX', 'ZCARD', 'ZSCORE', 'ZMSCORE', 'ZRANK', 'ZREVRANK', 'ZCOUNT', 'ZLEXCOUNT', 'ZSCAN',
  'SCAN', 'DBSIZE', 'XLEN', 'XRANGE', 'XREVRANGE', 'BITCOUNT', 'GETBIT', 'PFCOUNT', 'OBJECT ENCODING', 'OBJECT IDLETIME', 'OBJECT FREQ', 'MEMORY USAGE',
  'GEOPOS', 'GEODIST', 'GEORADIUS_RO', 'GEOSEARCH'];

// ctx: { dbType, table: 表名（可空）, cols: 字段名数组, pk: 主键字段名（可空）, disableStar: bool }
export function buildSamples(ctx) {
  if (isRedis(ctx.dbType)) return redisSamples();
  if (isPG(ctx.dbType)) return pgSamples(ctx);
  if (isCH(ctx.dbType)) return chSamples(ctx);
  if (isOra(ctx.dbType)) return oraSamples(ctx);
  if (isMongo(ctx.dbType)) return mongoSamples(ctx);
  const out = [];
  const add = (group, label, sql) => out.push({ group, label, sql });
  const gOK = t('正常查询');
  const gRule = t('规则示例');
  const gBlock = t('会被拦截');
  const ms = isMSSQL(ctx.dbType);
  const tn = ctx.table;
  if (!tn) {
    add(gOK, t('无表查询：SELECT 1'), 'SELECT 1 AS ok;');
    return out;
  }
  const T = quoteTable(tn, ctx.dbType);
  const cols = (ctx.cols && ctx.cols.length ? ctx.cols : []).slice(0, 8);
  const list = cols.length ? cols.map((c) => quoteId(c, ctx.dbType)).join(', ') : '1';
  const pk = ctx.pk ? quoteId(ctx.pk, ctx.dbType) : '';
  const top = (n) => (ms ? `TOP ${n} ` : '');
  const lim = (n) => (ms ? '' : `\nLIMIT ${n}`);
  const order = pk ? `\nORDER BY ${pk} DESC` : '';

  add(gOK, t('前 50 行：{table}', { table: tn }), `SELECT ${top(50)}${list}\nFROM ${T}${order}${lim(50)};`);
  add(gOK, t('统计行数'), `SELECT COUNT(*) AS cnt\nFROM ${T};`);
  add(gOK, t('WITH（CTE）示例'), `WITH t AS (\n  SELECT ${top(100)}${list}\n  FROM ${T}${order}${lim(100)}\n)\nSELECT COUNT(*) AS cnt FROM t;`);
  if (!ms) {
    add(gOK, t('结构：SHOW CREATE TABLE {table}', { table: tn }), `SHOW CREATE TABLE ${T};`);
    add(gOK, t('结构：DESC {table}', { table: tn }), `DESC ${T};`);
    add(gOK, t('结构：SHOW INDEX FROM {table}', { table: tn }), `SHOW INDEX FROM ${T};`);
    add(gOK, t('执行计划：EXPLAIN'), `EXPLAIN SELECT ${list}\nFROM ${T}${order}\nLIMIT 10;`);
  }
  add(gRule, t('没带 LIMIT / TOP（自动补）'), `SELECT ${list}\nFROM ${T};`);
  add(gRule, ms ? t('TOP 超过上限（自动改写）') : t('LIMIT 超过上限（自动改写）'), ms ? `SELECT TOP 100000 ${list}\nFROM ${T};` : `SELECT ${list}\nFROM ${T}\nLIMIT 100000;`);
  add(gRule, t('多语句（只执行第 1 条）'), `SELECT COUNT(*) AS cnt FROM ${T};\nSELECT 1 AS second_statement;`);
  add(gBlock, t('UPDATE（写操作）'), `UPDATE ${T} SET ${cols.length ? quoteId(cols[0], ctx.dbType) : 'id'} = 1 WHERE 1 = 0;`);
  add(gBlock, t('DROP TABLE（DDL）'), `DROP TABLE ${T};`);
  add(gBlock, t('WITH 里夹带写操作'), `WITH t AS (SELECT 1 AS x)\nDELETE FROM ${T} WHERE 1 = 0;`);
  if (ctx.disableStar) add(gBlock, 'SELECT *', `SELECT * FROM ${T}${ms ? '' : ' LIMIT 10'};`);
  if (!ms) {
    add(gBlock, t('sleep() 函数'), 'SELECT sleep(30);');
    add(gBlock, t('查 mysql 系统库'), 'SELECT host, user FROM mysql.user;');
    add(gBlock, t('FOR UPDATE 加锁读'), `SELECT ${list} FROM ${T} WHERE 1 = 0 FOR UPDATE;`);
    add(gBlock, t('EXPLAIN ANALYZE（会真实执行）'), `EXPLAIN ANALYZE SELECT ${list} FROM ${T} LIMIT 1;`);
  } else {
    add(gBlock, t('EXEC（执行存储过程）'), 'EXEC sp_who;');
  }
  return out;
}
