/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_redisq.js
 * @time: 2026/10/28
 */
/* ============================================================================
   Redis 在线查询（原型）：命令检查 / 限量改写 / 受限执行 / 结果整形 / key 浏览 / 示例命令。
   行为依据（命令白名单、拒绝文案、限量规则按原文移植）：
     internal/engine/redis/userquery.go（ParseUserCommand / RunUser / BrowseKeys）、internal/engine/redis/cmdkeys.go（TokenizeLine）、
     internal/query/redis.go（checkRedisCmd / shapeRedis / redisText）、web/static/js/query/redis_keys.js。
   数据全部虚构：每个 Redis 实例有一份确定性的“键空间”（按 db 编号），不连任何服务端；同一条命令永远得到同一份结果。
   导出 window.RedisQuery，由 p_query.js（在线查询页）与 p_priv.js（权限申请 / 直接授予）调用，本文件不注册菜单页。
   约定：命令参数与回复里的字符串都是“字节串”（每个字符 = 一个字节 0~255），与 Go 里的 string 一致；
         只在入口（toBin）和显示（redisText / quoteCLIArg）处做 UTF-8 编解码。
   ========================================================================== */
(function () {
'use strict';

/* ---------- 局部样式（只用 CSS 变量） ---------- */
const CSS = `
.rk-row{padding-right:8px}
.rk-type{display:inline-block;min-width:46px;text-align:center;font-size:11px;line-height:17px;padding:0 5px;border-radius:5px;font-weight:600;background:var(--panel2);color:var(--muted)}
.rk-string{background:var(--accent-soft);color:var(--accent)}.rk-hash{background:var(--ok-soft);color:var(--ok)}.rk-list{background:var(--warn-soft);color:var(--warn)}
.rk-set{background:var(--brand-soft);color:var(--brand)}.rk-zset{background:var(--bad-soft);color:var(--bad)}.rk-stream{border:1px solid var(--border)}
.rk-key{overflow:hidden;text-overflow:ellipsis;font:12px/1.5 ui-monospace,Consolas,monospace}
.rk-head{font-weight:600;cursor:default;display:flex;gap:6px;align-items:center;padding:4px 10px}
.rk-head .n{margin-left:auto;font-size:11px;font-weight:400;color:var(--muted)}
.rk-hint{padding:8px 12px;color:var(--muted);font-size:12px}
.rk-err{margin:8px;padding:8px 10px;border-radius:8px;background:var(--bad-soft);font-size:12px;white-space:normal}
.rk-more{margin:8px 10px}
.rq-long{display:inline-block;max-width:520px;overflow:hidden;text-overflow:ellipsis;vertical-align:bottom}
`;
(function () {
  if (document.getElementById('rq-style')) return;
  const s = document.createElement('style'); s.id = 'rq-style'; s.textContent = CSS; document.head.appendChild(s);
})();

/* ---------- 错误类型：UErr = 命令检查 / 探测不通过的业务原因（对应 *UserError）；SErr = Redis 服务端的错误回复 ---------- */
class UErr extends Error {}
class SErr extends Error {}

/* ---------- 限量（userquery.go 顶部常量） ---------- */
const SCAN_MAX = 1000, SCAN_DEF = 100, RANGE_MAX = 1000, FULL_MAX = 5000, KEYS_MAX = 500, BITCOUNT_MAX = 16 << 20, LPOS_MAXLEN = 100000;
const CMD_MAX_BYTES = 64 << 10, CELL_MAX = 64 << 10, REPLY_MAX = 4 << 20;
const MAX_SAFE = Math.pow(2, 50);

/* ============================================================================
   一、字节串工具、redis-cli 分词、引号还原
   ========================================================================== */
const enc = new TextEncoder(), decStrict = new TextDecoder('utf-8', {fatal: true});
const hex2 = n => n.toString(16).padStart(2, '0');
const toBin = s => { const b = enc.encode(String(s)); let r = ''; for (let i = 0; i < b.length; i++) r += String.fromCharCode(b[i]); return r; };
/* 字节串切成 rune：{c: 码点, s: 文本}；非法字节 {c: -1, b: 字节, s: null} */
function scanRunes(bs) {
  const out = [], n = bs.length; let i = 0;
  while (i < n) {
    const b = bs.charCodeAt(i);
    if (b < 0x80) { out.push({c: b, s: bs[i]}); i++; continue; }
    const len = b >= 0xc2 && b <= 0xdf ? 2 : b >= 0xe0 && b <= 0xef ? 3 : b >= 0xf0 && b <= 0xf4 ? 4 : 0;
    let ok = len > 0 && i + len <= n;
    for (let k = 1; ok && k < len; k++) { const cb = bs.charCodeAt(i + k); if (cb < 0x80 || cb > 0xbf) ok = false; }
    if (ok) {
      try { const s = decStrict.decode(Uint8Array.from(bs.slice(i, i + len), ch => ch.charCodeAt(0))); out.push({c: s.codePointAt(0), s}); i += len; continue; } catch (e) { /* 非法序列，按单字节处理 */ }
    }
    out.push({c: -1, b, s: null}); i++;
  }
  return out;
}
/* 把一个值转成可显示的文本（对应 redisText）：合法 UTF-8 且没有控制字符（\t \n \r 除外）原样显示，否则转成 \xHH 并标记 binary */
function redisText(bs) {
  if (/^[\x20-\x7e\t\n\r]*$/.test(bs)) return [bs, false];
  let out = '', bin = false;
  for (const r of scanRunes(bs)) {
    if (r.s === null) { out += '\\x' + hex2(r.b); bin = true; }
    else if ((r.c < 0x20 && r.c !== 9 && r.c !== 10 && r.c !== 13) || r.c === 0x7f) { out += '\\x' + hex2(r.c); bin = true; }
    else out += r.s;
  }
  return [out, bin];
}
/* TokenizeLine 的逆运算（quoteCLIArg）：无需引号的原样返回，否则用双引号包起来并转义；返回的是可显示文本 */
function quoteCLIArg(bs) {
  let plain = bs !== '';
  for (let i = 0; i < bs.length && plain; i++) { const ch = bs.charCodeAt(i); if (ch <= 0x20 || ch >= 0x7f || ch === 0x22 || ch === 0x27 || ch === 0x5c) plain = false; }
  if (plain) return bs;
  let out = '"';
  for (const r of scanRunes(bs)) {
    if (r.s === null) out += '\\x' + hex2(r.b);
    else if (r.c === 0x22) out += '\\"';
    else if (r.c === 0x5c) out += '\\\\';
    else if (r.c === 10) out += '\\n';
    else if (r.c === 13) out += '\\r';
    else if (r.c === 9) out += '\\t';
    else if (r.c < 0x20 || r.c === 0x7f) out += '\\x' + hex2(r.c);
    else out += r.s;
  }
  return out + '"';
}
const formatUserCommand = args => args.map(quoteCLIArg).join(' ');

const isSp = c => c === ' ' || c === '\n' || c === '\r' || c === '\t' || c === '\0';
const isHexCh = c => /^[0-9a-fA-F]$/.test(c);
/* 按 redis-cli 的规则分词（对应 TokenizeLine / sdssplitargs）：空白分隔；双引号内支持 \n \r \t \b \a \xHH 与 \" \\；
   单引号内只支持 \'；引号关闭后必须紧跟空白或行尾；引号没有关闭报错。入参 / 结果都是字节串。 */
function tokenizeLine(b) {
  const out = [], n = b.length; let i = 0;
  for (;;) {
    while (i < n && isSp(b[i])) i++;
    if (i >= n) return out;
    let cur = '', inD = false, inS = false, done = false;
    while (!done) {
      if (inD) {
        if (i >= n) throw new Error('双引号没有闭合');
        if (b[i] === '\\' && i + 3 < n && b[i + 1] === 'x' && isHexCh(b[i + 2]) && isHexCh(b[i + 3])) { cur += String.fromCharCode(parseInt(b.substr(i + 2, 2), 16)); i += 3; }
        else if (b[i] === '\\' && i + 1 < n) { i++; const c = b[i]; cur += c === 'n' ? '\n' : c === 'r' ? '\r' : c === 't' ? '\t' : c === 'b' ? '\b' : c === 'a' ? '\x07' : c; }
        else if (b[i] === '"') { if (i + 1 < n && !isSp(b[i + 1])) throw new Error('右引号后面必须是空白或行尾'); done = true; }
        else cur += b[i];
      } else if (inS) {
        if (i >= n) throw new Error('单引号没有闭合');
        if (b[i] === '\\' && i + 1 < n && b[i + 1] === "'") { i++; cur += "'"; }
        else if (b[i] === "'") { if (i + 1 < n && !isSp(b[i + 1])) throw new Error('右引号后面必须是空白或行尾'); done = true; }
        else cur += b[i];
      } else if (i >= n) done = true;
      else if (isSp(b[i])) done = true;
      else if (b[i] === '"') inD = true;
      else if (b[i] === "'") inS = true;
      else cur += b[i];
      if (i < n) i++;
    }
    out.push(cur);
  }
}

/* ============================================================================
   二、只读白名单与检查 / 改写（userquery.go 移植；拒绝文案原文）
   ========================================================================== */
const setOf = (...a) => new Set(a);
const DENY_BLOCKING = setOf('BLPOP', 'BRPOP', 'BRPOPLPUSH', 'BLMOVE', 'BLMPOP', 'BZPOPMIN', 'BZPOPMAX', 'BZMPOP', 'XREAD', 'XREADGROUP', 'WAIT', 'WAITAOF');
const DENY_SUB = setOf('MONITOR', 'SUBSCRIBE', 'PSUBSCRIBE', 'SSUBSCRIBE', 'UNSUBSCRIBE', 'PUNSUBSCRIBE', 'SUNSUBSCRIBE', 'PUBLISH', 'SPUBLISH', 'SYNC', 'PSYNC');
const DENY_SCRIPT = setOf('EVAL', 'EVALSHA', 'EVAL_RO', 'EVALSHA_RO', 'SCRIPT', 'FUNCTION', 'FCALL', 'FCALL_RO');
const DENY_ADMIN = setOf('CONFIG', 'INFO', 'CLIENT', 'DEBUG', 'COMMAND', 'SLOWLOG', 'LATENCY', 'ROLE', 'CLUSTER', 'ACL', 'MODULE', 'SHUTDOWN', 'FAILOVER', 'SWAPDB', 'RESET', 'LOLWUT', 'MONITOR', 'REPLCONF', 'READONLY', 'READWRITE');
const DENY_FLUSH = setOf('FLUSHALL', 'FLUSHDB');
const DENY_PERSIST = setOf('SAVE', 'BGSAVE', 'BGREWRITEAOF', 'LASTSAVE');
const DENY_REPL = setOf('SLAVEOF', 'REPLICAOF', 'MIGRATE');
const DENY_AUTH = setOf('AUTH', 'HELLO');
const DENY_TX = setOf('MULTI', 'EXEC', 'DISCARD', 'WATCH', 'UNWATCH');
const DENY_WRITE = setOf(
  'SET', 'SETNX', 'SETEX', 'PSETEX', 'MSET', 'MSETNX', 'APPEND', 'INCR', 'INCRBY', 'INCRBYFLOAT', 'DECR', 'DECRBY', 'GETSET', 'GETDEL', 'GETEX',
  'SETRANGE', 'SETBIT', 'BITFIELD', 'BITOP', 'DEL', 'UNLINK', 'EXPIRE', 'PEXPIRE', 'EXPIREAT', 'PEXPIREAT', 'PERSIST', 'RENAME', 'RENAMENX',
  'MOVE', 'COPY', 'RESTORE', 'RESTORE-ASKING', 'TOUCH', 'OBJECT FREQ-SET',
  'HSET', 'HSETNX', 'HMSET', 'HDEL', 'HINCRBY', 'HINCRBYFLOAT', 'HGETDEL', 'HSETEX', 'HEXPIRE',
  'LPUSH', 'RPUSH', 'LPUSHX', 'RPUSHX', 'LPOP', 'RPOP', 'LINSERT', 'LSET', 'LREM', 'LTRIM', 'RPOPLPUSH', 'LMOVE', 'LMPOP',
  'SADD', 'SREM', 'SPOP', 'SMOVE', 'SINTERSTORE', 'SUNIONSTORE', 'SDIFFSTORE',
  'ZADD', 'ZREM', 'ZINCRBY', 'ZPOPMIN', 'ZPOPMAX', 'ZMPOP', 'ZREMRANGEBYRANK', 'ZREMRANGEBYSCORE', 'ZREMRANGEBYLEX', 'ZUNIONSTORE', 'ZINTERSTORE',
  'ZDIFFSTORE', 'ZRANGESTORE', 'XADD', 'XDEL', 'XTRIM', 'XGROUP', 'XACK', 'XCLAIM', 'XAUTOCLAIM', 'XSETID', 'PFADD', 'PFMERGE',
  'GEOADD', 'GEORADIUS', 'GEORADIUSBYMEMBER', 'GEOSEARCHSTORE', 'SELECT');
/* 点名拒绝的原因；不在任何分类里返回空串（走“不在白名单”的通用提示） */
function denyReason(name) {
  if (name === 'KEYS') return 'KEYS 会在大库上长时间阻塞 Redis，已禁止；请改用 SCAN 0 MATCH 模式 COUNT 100 分批读取（或用左侧 key 浏览）';
  if (name === 'SELECT') return 'SELECT 已禁止：库由页面顶部的库下拉选择，连接时已指定';
  if (name === 'SORT' || name === 'SORT_RO') return name + ' 可能带 STORE 写入或对大集合排序，已禁止';
  if (DENY_AUTH.has(name)) return name + ' 已禁止：在线查询不允许认证 / 切换协议，也不要在命令里携带密码';
  if (DENY_BLOCKING.has(name)) return name + ' 是阻塞命令，会占住连接，已禁止';
  if (DENY_SUB.has(name)) return name + ' 是订阅 / 复制 / 监控类命令，在线查询不支持';
  if (DENY_SCRIPT.has(name)) return name + ' 可以执行脚本，已禁止';
  if (DENY_FLUSH.has(name)) return name + ' 会清空数据，已禁止';
  if (DENY_PERSIST.has(name)) return name + ' 属于持久化管理命令，已禁止';
  if (DENY_REPL.has(name)) return name + ' 属于复制 / 迁移命令，已禁止';
  if (DENY_TX.has(name)) return name + ' 是事务命令，在线查询一次只执行一条只读命令';
  if (DENY_ADMIN.has(name)) return name + ' 属于运维命令，已禁止；实例巡检、慢查询请使用对应的 DBA 页面';
  if (DENY_WRITE.has(name)) return name + ' 会修改数据，在线查询只允许只读命令；数据变更请通过「SQL 上线」提交 Redis 工单';
  return '';
}

/* ---- 参数解析辅助 ---- */
const addNotice = (c, s) => c.notices.push(s);
function intArg(c, i, what) {
  const s = c.args[i];
  if (!/^[+-]?\d+$/.test(s)) throw new UErr(`命令 ${c.name} 的${what}必须是整数`);
  const n = parseInt(s, 10);
  if (n > MAX_SAFE || n < -MAX_SAFE) throw new UErr(`命令 ${c.name} 的${what}必须是整数`);
  return n;
}
/* 从 args[from:] 起按 spec（选项名 → 参数个数）解析选项，返回 选项名 → 位置。重复的选项取最后一个 */
function parseOpts(c, from, spec) {
  const pos = {};
  for (let i = from; i < c.args.length;) {
    const opt = c.args[i].toUpperCase();
    if (!Object.prototype.hasOwnProperty.call(spec, opt)) throw new UErr(`命令 ${c.name} 不支持选项 ${c.args[i]}`);
    const n = spec[opt];
    if (i + n >= c.args.length) throw new UErr(`命令 ${c.name} 的选项 ${opt} 缺少参数`);
    pos[opt] = i; i += n + 1;
  }
  return pos;
}
/* COUNT 类选项：存在则要求是正整数并夹到 max；不存在且 def>0 则补上 */
function capCount(c, pos, opt, def, max) {
  if (!(opt in pos)) {
    if (def > 0) { c.args.push(opt, String(def)); addNotice(c, `未指定 ${opt}，已补默认值 ${def}`); }
    return;
  }
  const p = pos[opt], n = intArg(c, p + 1, opt + ' 值');
  if (n < 1) throw new UErr(`命令 ${c.name} 的 ${opt} 必须是正整数`);
  if (n > max) { c.args[p + 1] = String(max); addNotice(c, `${opt} ${n} 超过上限，已改为 ${max}`); }
}
/* LIMIT offset count：缺省补 LIMIT 0 N；count 为负（表示全部）或超限夹到 N */
function capLimit(c, pos) {
  if (!('LIMIT' in pos)) {
    c.args.push('LIMIT', '0', String(RANGE_MAX));
    addNotice(c, `未带 LIMIT，已自动加上 LIMIT 0 ${RANGE_MAX}（单次最多返回 ${RANGE_MAX} 条）`);
    return;
  }
  const p = pos.LIMIT;
  intArg(c, p + 1, 'LIMIT 偏移');
  const n = intArg(c, p + 2, 'LIMIT 条数');
  if (n < 0 || n > RANGE_MAX) { c.args[p + 2] = String(RANGE_MAX); addNotice(c, `LIMIT 条数超过上限，已改为 ${RANGE_MAX}`); }
}
/* 下标区间：两端都非负时静态收窄，含负数时登记探测，执行前换算成绝对下标 */
function rangeIdx(c, si, ei, lenCmd) {
  const s = intArg(c, si, '起始下标'), e = intArg(c, ei, '结束下标');
  c.indexBase = Math.max(s, 0);
  if (s >= 0 && e >= 0) {
    if (e >= s && e - s + 1 > RANGE_MAX) {
      c.args[ei] = String(s + RANGE_MAX - 1);
      addNotice(c, `区间过大，已收窄为 ${s}..${s + RANGE_MAX - 1}（单次最多 ${RANGE_MAX} 条，请调整 start / stop 分页）`);
    }
    return;
  }
  c.probes.push({kind: 'range', lenCmd, keys: [c.args[1]], si, ei});
}
function need(c, min, max) {
  const n = c.args.length - 1;
  if (n < min || (max >= 0 && n > max)) throw new UErr(`命令 ${c.name} 的参数个数不对`);
}
function scanCursor(c, i) { if (!/^\d+$/.test(c.args[i])) throw new UErr(`命令 ${c.name} 的游标必须是非负整数（从 0 开始）`); }
function scanLike(c, cursorIdx, spec) {
  need(c, cursorIdx, -1);
  scanCursor(c, cursorIdx);
  const pos = parseOpts(c, cursorIdx + 1, spec);
  if ('MATCH' in pos && c.args[pos.MATCH + 1].length > 512) throw new UErr('MATCH 模式过长（最多 512 字节）');
  if ('NOVALUES' in pos) c.flags.NOVALUES = true;
  capCount(c, pos, 'COUNT', SCAN_DEF, SCAN_MAX);
}
function capRandCount(c, i) {
  if (i >= c.args.length) return;
  const n = intArg(c, i, '数量');
  if (n > RANGE_MAX || n < -RANGE_MAX) { const v = n < 0 ? -RANGE_MAX : RANGE_MAX; c.args[i] = String(v); addNotice(c, `数量超过上限，已改为 ${v}`); }
}
function geoOpts(c, from, spec) {
  const pos = parseOpts(c, from, spec);
  for (const f of ['WITHCOORD', 'WITHDIST', 'WITHHASH']) if (f in pos) c.flags[f] = true;
  capCount(c, pos, 'COUNT', RANGE_MAX, RANGE_MAX);
}

/* ---- 白名单表：命令名 → 检查函数（共 60 个命令名；OBJECT / MEMORY 带子命令） ---- */
const arity = (min, max) => c => need(c, min, max);
const multi = min => c => {
  const n = c.args.length - 1;
  if (n < min) throw new UErr(`命令 ${c.name} 的参数个数不对`);
  if (n > KEYS_MAX + 1) throw new UErr(`命令 ${c.name} 一次最多 ${KEYS_MAX} 个键 / 成员`);
};
const full = (lenCmd, scanCmd) => c => {   // 全量读取：先探测元素个数
  need(c, 1, 1);
  c.probes.push({kind: 'card', lenCmd, keys: [c.args[1]], hint: `请改用 ${scanCmd} ${quoteCLIArg(c.args[1])} 0 COUNT 100 分页读取`});
};
const setOps = c => {
  multi(1)(c);
  c.probes.push({kind: 'card', lenCmd: 'SCARD', keys: c.args.slice(1), hint: '请改用 SSCAN 分页读取单个集合，或缩小集合'});
};
const TABLE = {
  /* 字符串 */
  GET: arity(1, 1), MGET: multi(1), STRLEN: arity(1, 1), GETRANGE: arity(3, 3), EXISTS: multi(1), TYPE: arity(1, 1),
  TTL: arity(1, 1), PTTL: arity(1, 1), EXPIRETIME: arity(1, 1), PEXPIRETIME: arity(1, 1),
  /* 哈希 */
  HGET: arity(2, 2), HMGET: multi(2), HGETALL: full('HLEN', 'HSCAN'), HLEN: arity(1, 1), HKEYS: full('HLEN', 'HSCAN'),
  HVALS: full('HLEN', 'HSCAN'), HEXISTS: arity(2, 2), HSTRLEN: arity(2, 2),
  HSCAN: c => scanLike(c, 2, {MATCH: 1, COUNT: 1, NOVALUES: 0}),
  HRANDFIELD: c => {
    need(c, 1, 3);
    if (c.args.length === 4) { if (c.args[3].toUpperCase() !== 'WITHVALUES') throw new UErr('HRANDFIELD 只支持可选的 WITHVALUES'); c.flags.WITHVALUES = true; }
    capRandCount(c, 2);
  },
  /* 列表 */
  LRANGE: c => { need(c, 3, 3); rangeIdx(c, 2, 3, 'LLEN'); },
  LLEN: arity(1, 1), LINDEX: arity(2, 2),
  LPOS: c => {
    need(c, 2, -1);
    const pos = parseOpts(c, 3, {RANK: 1, COUNT: 1, MAXLEN: 1});
    if ('COUNT' in pos) {
      const n = intArg(c, pos.COUNT + 1, 'COUNT 值');
      if (n < 0) throw new UErr('LPOS 的 COUNT 不能为负');
      if (n === 0 || n > RANGE_MAX) { c.args[pos.COUNT + 1] = String(RANGE_MAX); addNotice(c, `COUNT 已限制为最多 ${RANGE_MAX} 个匹配`); }
    }
    if ('MAXLEN' in pos) {
      const n = intArg(c, pos.MAXLEN + 1, 'MAXLEN 值');
      if (n < 0) throw new UErr('LPOS 的 MAXLEN 不能为负');
      if (n === 0 || n > 1000000) { c.args[pos.MAXLEN + 1] = String(LPOS_MAXLEN); addNotice(c, `MAXLEN 已限制为最多比较 ${LPOS_MAXLEN} 个元素`); }
    } else { c.args.push('MAXLEN', String(LPOS_MAXLEN)); addNotice(c, `已限制最多比较 ${LPOS_MAXLEN} 个元素（MAXLEN）`); }
  },
  /* 集合 */
  SMEMBERS: full('SCARD', 'SSCAN'), SCARD: arity(1, 1), SISMEMBER: arity(2, 2), SMISMEMBER: multi(2),
  SRANDMEMBER: c => { need(c, 1, 2); capRandCount(c, 2); },
  SSCAN: c => scanLike(c, 2, {MATCH: 1, COUNT: 1}), SINTER: setOps, SUNION: setOps, SDIFF: setOps,
  /* 有序集合 */
  ZRANGE: c => {
    need(c, 3, -1);
    const pos = parseOpts(c, 4, {BYSCORE: 0, BYLEX: 0, REV: 0, WITHSCORES: 0, LIMIT: 2});
    if ('WITHSCORES' in pos) c.flags.WITHSCORES = true;
    const bs = 'BYSCORE' in pos, bl = 'BYLEX' in pos;
    if (bs && bl) throw new UErr('ZRANGE 不能同时使用 BYSCORE 和 BYLEX');
    if (bs || bl) return capLimit(c, pos);
    if ('LIMIT' in pos) throw new UErr('ZRANGE 的 LIMIT 只能和 BYSCORE / BYLEX 一起使用');
    rangeIdx(c, 2, 3, 'ZCARD');
  },
  ZREVRANGE: c => {
    need(c, 3, 4);
    if (c.args.length === 5) { if (c.args[4].toUpperCase() !== 'WITHSCORES') throw new UErr('ZREVRANGE 只支持可选的 WITHSCORES'); c.flags.WITHSCORES = true; }
    rangeIdx(c, 2, 3, 'ZCARD');
  },
  ZRANGEBYSCORE: null, ZREVRANGEBYSCORE: null,
  ZRANGEBYLEX: c => { need(c, 3, -1); capLimit(c, parseOpts(c, 4, {LIMIT: 2})); },
  ZCARD: arity(1, 1), ZSCORE: arity(2, 2), ZMSCORE: multi(2), ZRANK: null, ZREVRANK: null, ZCOUNT: arity(3, 3), ZLEXCOUNT: arity(3, 3),
  ZSCAN: c => scanLike(c, 2, {MATCH: 1, COUNT: 1}),
  /* 其他 */
  SCAN: c => scanLike(c, 1, {MATCH: 1, COUNT: 1, TYPE: 1}),
  DBSIZE: arity(0, 0), XLEN: arity(1, 1),
  XRANGE: null, XREVRANGE: null,
  BITCOUNT: c => {
    const n = c.args.length - 1;
    if (n !== 1 && n !== 3 && n !== 4) throw new UErr('BITCOUNT 的参数个数不对（key [start end [BYTE|BIT]]）');
    if (n === 4) { const u = c.args[4].toUpperCase(); if (u !== 'BYTE' && u !== 'BIT') throw new UErr('BITCOUNT 的第 4 个参数只能是 BYTE 或 BIT'); }
    if (n === 1) c.probes.push({kind: 'strlen', lenCmd: 'STRLEN', keys: [c.args[1]], hint: '请带上 start end 区间分段统计'});
  },
  GETBIT: arity(2, 2), PFCOUNT: multi(1),
  OBJECT: c => {
    if (c.args.length < 2) throw new UErr('OBJECT 需要子命令，在线查询只支持 ENCODING / IDLETIME / FREQ');
    const sub = c.args[1].toUpperCase();
    if (sub !== 'ENCODING' && sub !== 'IDLETIME' && sub !== 'FREQ') throw new UErr(`OBJECT ${sub} 不在白名单内，在线查询只支持 OBJECT ENCODING / IDLETIME / FREQ`);
    if (c.args.length !== 3) throw new UErr(`OBJECT ${sub} 需要一个 key`);
    c.args[1] = sub; c.name = 'OBJECT ' + sub;
  },
  MEMORY: c => {
    if (c.args.length < 2) throw new UErr('MEMORY 需要子命令，在线查询只支持 MEMORY USAGE');
    const sub = c.args[1].toUpperCase();
    if (sub !== 'USAGE') throw new UErr(`MEMORY ${sub} 不在白名单内，在线查询只支持 MEMORY USAGE key [SAMPLES n]`);
    c.args[1] = 'USAGE'; c.name = 'MEMORY USAGE';
    if (c.args.length === 3) return;
    if (c.args.length === 5) {
      if (c.args[3].toUpperCase() !== 'SAMPLES') throw new UErr('MEMORY USAGE 的第 3 个参数只能是 SAMPLES');
      const n = /^[+-]?\d+$/.test(c.args[4]) ? parseInt(c.args[4], 10) : NaN;
      if (!(n >= 0 && n <= 100)) throw new UErr('MEMORY USAGE SAMPLES 需在 0 到 100 之间');
      return;
    }
    throw new UErr('MEMORY USAGE 的参数不合法');
  },
  GEOPOS: multi(2),
  GEODIST: c => {
    need(c, 3, 4);
    if (c.args.length === 5 && !['m', 'km', 'ft', 'mi'].includes(c.args[4].toLowerCase())) throw new UErr('GEODIST 的单位只能是 m / km / ft / mi');
  },
  GEORADIUS_RO: c => { need(c, 5, -1); geoOpts(c, 6, {WITHCOORD: 0, WITHDIST: 0, WITHHASH: 0, COUNT: 1, ANY: 0, ASC: 0, DESC: 0}); },
  GEOSEARCH: c => { need(c, 3, -1); geoOpts(c, 2, {FROMMEMBER: 1, FROMLONLAT: 2, BYRADIUS: 2, BYBOX: 3, WITHCOORD: 0, WITHDIST: 0, WITHHASH: 0, COUNT: 1, ANY: 0, ASC: 0, DESC: 0}); },
};
TABLE.ZRANGEBYSCORE = TABLE.ZREVRANGEBYSCORE = c => {
  need(c, 3, -1);
  const pos = parseOpts(c, 4, {WITHSCORES: 0, LIMIT: 2});
  if ('WITHSCORES' in pos) c.flags.WITHSCORES = true;
  capLimit(c, pos);
};
TABLE.ZRANK = TABLE.ZREVRANK = c => {
  need(c, 2, 3);
  if (c.args.length === 4 && c.args[3].toUpperCase() !== 'WITHSCORE') throw new UErr(`${c.name} 只支持可选的 WITHSCORE`);
};
TABLE.XRANGE = TABLE.XREVRANGE = c => { need(c, 3, 5); capCount(c, parseOpts(c, 4, {COUNT: 1}), 'COUNT', RANGE_MAX, RANGE_MAX); };
const COMMAND_NAMES = Object.keys(TABLE).sort();

/* 检查并改写一条已分词的命令：名称在白名单内、参数形态合法、返回量受限。不连服务端 */
function parseUserCommand(args) {
  if (!args.length) throw new UErr('命令为空');
  const name = args[0].toUpperCase();
  const r = denyReason(name);
  if (r) throw new UErr(r);
  const chk = Object.prototype.hasOwnProperty.call(TABLE, name) ? TABLE[name] : null;
  if (!chk) throw new UErr(`命令 ${name} 不在在线查询的只读白名单内（白名单为固定列表，不可配置）`);
  for (const a of args.slice(1)) { const u = a.toUpperCase(); if (u === 'AUTH' || u === 'AUTH2') throw new UErr('命令参数里含 AUTH，疑似携带密码，已拒绝'); }
  const c = {name, orig: args.slice(), args: [name].concat(args.slice(1)), notices: [], flags: {}, indexBase: 0, probes: []};
  chk(c);
  return c;
}
/* 命令文本的静态检查（checkRedisCmd）：非空、不超长、只有一条、redis-cli 规则分词、白名单与限量改写。不访问 Redis */
function check(text) {
  text = String(text == null ? '' : text).trim();
  if (!text) return {err: '命令为空'};
  if (toBin(text).length > CMD_MAX_BYTES) return {err: `命令过长（上限 ${CMD_MAX_BYTES >> 10} KB）`};
  const lines = text.split('\n').filter(l => l.trim() !== '').length;
  if (lines > 1) return {err: `检测到 ${lines} 行命令，在线查询一次只能执行一条；请选中其中一条再运行（值里的换行请写成 \\n）。`};
  let args;
  try { args = tokenizeLine(toBin(text)); } catch (e) { return {err: '命令解析失败：' + e.message}; }
  if (!args.length) return {err: '命令为空'};
  try {
    const cmd = parseUserCommand(args);
    return {cmd, line: formatUserCommand(cmd.args)};
  } catch (e) { if (e instanceof UErr) return {err: e.message}; throw e; }
}

/* ============================================================================
   三、模拟键空间：每个 Redis 实例、每个 db 一份确定性的数据
   ========================================================================== */
const NOW = Date.UTC(2026, 9, 7, 2, 41, 0) / 1000;           // 与在线查询页的“现在”（2026-10-07 10:41 CST）一致
const dtc = sec => new Date((sec + 28800) * 1000).toISOString().slice(0, 19).replace('T', ' ');
const h32 = s => { let h = 2166136261; s = String(s); for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619); } return h >>> 0; };
const rr = s => rng(h32(s));
const RI = (r, a, b) => a + Math.floor(r() * (b - a + 1));
const PK = (r, list) => list[Math.floor(r() * list.length)];
const pad = (n, w) => String(n).padStart(w, '0');
const S = toBin;                                              // 含中文的值要先转成字节串
const idN = (i, salt) => 80000000 + ((Math.imul(i * 2 + salt + 1, 2654435761) >>> 0) % 20000000);
const hex32 = (i, salt) => [0, 1, 2, 3].map(k => h32('t' + k + ':' + i + ':' + salt).toString(16).padStart(8, '0')).join('');
const ipOf = (i, salt) => { const h = h32('ip' + i + ':' + salt); return `10.${20 + (h & 7)}.${(h >> 3) & 255}.${1 + ((h >> 11) & 253)}`; };
const SKU_NAMES = ['无线蓝牙耳机 Pro', '机械键盘 87 键', 'USB-C 快充线 1m', '智能手环 5', '便携充电宝 20000mAh', '4K 显示器 27 寸', '人体工学椅', '保温杯 500ml', '电动牙刷', '护眼台灯', '无线鼠标', '游戏手柄'];
const CITIES = ['上海', '北京', '杭州', '深圳', '广州', '成都', '南京', '武汉'];
const NICKS = ['风里来', '小橘子', '阿杰', '晴天', 'dreamer', '大熊', '清风', 'luna', '老王', '木木'];
const DEVICES = ['Chrome/118 Windows', 'Safari 17 iPhone', 'Chrome 117 Android', 'Safari 16 macOS', 'Edge/118 Windows'];

/* ---- 规格构造：{type, ttl(初始秒，-1 永不过期), len, ...} ---- */
const mkStr = (v, o) => Object.assign({type: 'string', ttl: -1, len: v.length, val: () => v}, o);
const mkColl = (type, len, at, inv, o) => Object.assign({type, ttl: -1, len, at, inv: inv || (() => -1)}, o);
function pairsColl(type, items, o) {                          // hash: [[f,v]]；zset: [[m,score]]（按分数升序）；list/set: [字符串]
  let map = null;
  const keyOf = type === 'hash' || type === 'zset' ? p => p[0] : p => p;
  return mkColl(type, items.length, i => items[i], x => { if (!map) { map = new Map(); items.forEach((p, i) => map.set(keyOf(p), i)); } return map.has(x) ? map.get(x) : -1; }, o);
}
const mkHash = (pairs, o) => pairsColl('hash', pairs, o);
const mkList = (items, o) => pairsColl('list', items, o);
const mkSet = (items, o) => pairsColl('set', items, o);
const mkZset = (items, o) => pairsColl('zset', items, o);
const numAfter = (s, prefix) => s.startsWith(prefix) && /^\d+$/.test(s.slice(prefix.length)) ? parseInt(s.slice(prefix.length), 10) : NaN;
/* 用 “前缀 + 数字” 命名的大集合：成员 = prefix + (base + i*step)，可反查下标 */
function numberedSet(len, prefix, base, step, o) {
  return mkColl('set', len, i => prefix + (base + i * step), x => { const n = numAfter(x, prefix); const k = (n - base) / step; return Number.isInteger(k) && k >= 0 && k < len ? k : -1; }, o);
}
function numberedZset(len, prefix, base, step, scoreFn, o) {
  return mkColl('zset', len, i => [prefix + (base + i * step), scoreFn(i)], x => { const n = numAfter(x, prefix); const k = (n - base) / step; return Number.isInteger(k) && k >= 0 && k < len ? k : -1; }, o);
}
function numberedHash(len, prefix, base, step, valFn, o) {
  return mkColl('hash', len, i => [prefix + (base + i * step), valFn(i)], x => { const n = numAfter(x, prefix); const k = (n - base) / step; return Number.isInteger(k) && k >= 0 && k < len ? k : -1; }, o);
}
function mkStream(len, baseMs, stepMs, entry, o) {
  return mkColl('stream', len, i => [`${baseMs + i * stepMs}-0`, entry(i)], null, Object.assign({ms: i => baseMs + i * stepMs}, o));
}
const jsonLine = o => JSON.stringify(o);

/* ---- 键族：re 识别键名；name(i) 生成第 i 个键名；make(m, name) 生成规格 ---- */
const fam = (type, re, name, make) => ({type, re, name, make});
const FAM_ORDER0 = {
  detail: fam('hash', /^order:detail:(\d{8})$/, i => 'order:detail:' + idN(i, 1), (m, nm) => {
    const r = rr(nm), created = NOW - RI(r, 60, 3 * 86400);
    return mkHash([['order_no', 'O20261006' + m[1]], ['user_id', String(RI(r, 10000, 9800000))], ['status', String(PK(r, [1, 1, 1, 2, 3]))], ['amount', (Math.pow(r(), 2) * 1800 + 9.9).toFixed(2)],
      ['pay_channel', PK(r, ['alipay', 'wechat', 'wechat', 'card'])], ['receiver_city', S(PK(r, CITIES))], ['items_cnt', String(PK(r, [1, 1, 2, 3, 5]))], ['coupon_id', String(PK(r, [0, 0, 0, 20231, 20388]))],
      ['created_at', dtc(created)], ['updated_at', dtc(created + RI(r, 20, 3600))]], {ttl: 86400});
  }),
  status: fam('string', /^order:status:(\d{8})$/, i => 'order:status:' + idN(i, 2), (m, nm) => {
    const r = rr(nm);
    return mkStr(jsonLine({status: PK(r, [1, 1, 2, 3]), pay: PK(r, ['alipay', 'wechat', 'card']), ts: NOW - RI(r, 30, 7200)}), {ttl: 600});
  }),
  cart: fam('hash', /^user:cart:(\d+)$/, i => 'user:cart:' + (10000 + (h32('c' + i) % 9800000)), (m, nm) => {
    const r = rr(nm), n = RI(r, 3, 18);
    return mkHash(Array.from({length: n}, (_, k) => ['sku:' + (100000 + RI(r, 0, 5999) * 3 + k * 7), String(PK(r, [1, 1, 1, 2, 3, 5]))]), {ttl: 7 * 86400});
  }),
  stock: fam('string', /^sku:stock:(\d+)$/, i => 'sku:stock:' + (100000 + (h32('s' + i) % 6000) * 3), (m, nm) => mkStr(String(RI(rr(nm), 0, 20000)))),
  skuinfo: fam('hash', /^sku:info:(\d+)$/, i => 'sku:info:' + (100000 + (h32('k' + i) % 6000) * 3), (m, nm) => {
    const r = rr(nm);
    return mkHash([['name', S(PK(r, SKU_NAMES))], ['price', (RI(r, 990, 129900) / 100).toFixed(2)], ['brand', PK(r, ['Anker', 'Logitech', 'Xiaomi', 'Philips', 'Huawei'])], ['category', PK(r, ['digital', 'office', 'home', 'sport'])],
      ['stock_ver', String(RI(r, 1, 9000))], ['updated_at', dtc(NOW - RI(r, 60, 86400 * 5))]], {ttl: 3600});
  }),
  lock: fam('string', /^lock:order:(\d{8})$/, i => 'lock:order:' + idN(i, 3), (m, nm) => mkStr(`dbtool-worker-0${RI(rr(nm), 1, 6)}:${RI(rr(nm + 'p'), 2000, 29999)}`, {ttl: 30})),
  items: fam('list', /^order:items:(\d{8})$/, i => 'order:items:' + idN(i, 4), (m, nm) => {
    const r = rr(nm), n = RI(r, 1, 6);
    return mkList(Array.from({length: n}, () => jsonLine({sku: 100000 + RI(r, 0, 5999) * 3, qty: PK(r, [1, 1, 2, 3]), price: (RI(r, 990, 59900) / 100).toFixed(2)})), {ttl: 86400});
  }),
};
const FAM_SESSION0 = {
  sess: fam('string', /^session:([0-9a-f]{32})$/, i => 'session:' + hex32(i, 1), (m, nm) => {
    const r = rr(nm);
    return mkStr(jsonLine({uid: RI(r, 10000, 9800000), login: dtc(NOW - RI(r, 60, 1700)), ip: ipOf(RI(r, 0, 99999), 5), ua: PK(r, DEVICES), csrf: hex32(RI(r, 0, 99999), 9).slice(0, 16)}), {ttl: 1800});
  }),
  sdata: fam('hash', /^session:data:([0-9a-f]{32})$/, i => 'session:data:' + hex32(i, 2), (m, nm) => {
    const r = rr(nm), uid = RI(r, 10000, 9800000);
    return mkHash([['uid', String(uid)], ['nickname', S(PK(r, NICKS))], ['phone', '1' + PK(r, [3, 5, 7, 8]) + RI(r, 100000000, 999999999)], ['email', PK(r, ['zhangwei', 'lina', 'wangjun', 'chenjie']) + RI(r, 1, 999) + '@' + PK(r, ['163.com', 'qq.com', 'gmail.com'])],
      ['roles', PK(r, ['member', 'member', 'vip', 'staff'])], ['last_active', dtc(NOW - RI(r, 10, 1700))], ['device', PK(r, DEVICES)]], {ttl: 1800});
  }),
  fail: fam('string', /^login:fail:(\d+\.\d+\.\d+\.\d+)$/, i => 'login:fail:' + ipOf(i, 7), (m, nm) => mkStr(String(RI(rr(nm), 1, 12)), {ttl: 600})),
  captcha: fam('string', /^captcha:([a-z0-9]{8})$/, i => 'captcha:' + h32('cp' + i).toString(36).padStart(8, '0').slice(-8), (m, nm) => mkStr(String(RI(rr(nm), 1000, 9999)), {ttl: 120})),
  devices: fam('list', /^user:devices:(\d+)$/, i => 'user:devices:' + (10000 + (h32('d' + i) % 9800000)), (m, nm) => {
    const r = rr(nm), n = RI(r, 1, 5);
    return mkList(Array.from({length: n}, () => jsonLine({ua: PK(r, DEVICES), ip: ipOf(RI(r, 0, 99999), 3), at: dtc(NOW - RI(r, 600, 86400 * 20))})), {ttl: 30 * 86400});
  }),
};
const FAM_SESSION1 = {
  profile: fam('hash', /^user:profile:(\d+)$/, i => 'user:profile:' + (10000 + (h32('p' + i) % 9800000)), (m, nm) => {
    const r = rr(nm);
    return mkHash([['nickname', S(PK(r, NICKS))], ['phone', '1' + PK(r, [3, 5, 7, 8]) + RI(r, 100000000, 999999999)], ['email', PK(r, ['zhangwei', 'lina', 'wangjun', 'chenjie']) + RI(r, 1, 999) + '@' + PK(r, ['163.com', 'qq.com', 'gmail.com'])],
      ['level', String(PK(r, [0, 1, 1, 2, 3, 5]))], ['reg_at', dtc(NOW - RI(r, 86400 * 30, 86400 * 900))], ['last_ip', ipOf(RI(r, 0, 99999), 11)]], {ttl: 7200});
  }),
  rlapi: fam('string', /^ratelimit:api:(\d+\.\d+\.\d+\.\d+)$/, i => 'ratelimit:api:' + ipOf(i, 13), (m, nm) => mkStr(String(RI(rr(nm), 1, 98)), {ttl: 60})),
  rlsms: fam('string', /^ratelimit:sms:(1\d{10})$/, i => 'ratelimit:sms:1' + PK(rr('sm' + i), [3, 5, 7, 8]) + (100000000 + (h32('sm' + i) % 899999999)), (m, nm) => mkStr(String(RI(rr(nm), 1, 5)), {ttl: 3600})),
};
const FAM_ORDER1 = {
  rcache: fam('string', /^report:cache:(\d+)$/, i => 'report:cache:' + (100000 + (h32('rc' + i) % 900000)), (m, nm) => {
    const r = rr(nm);
    return mkStr(jsonLine({report: 'sales_daily', dim: PK(r, ['city', 'channel', 'sku']), rows: RI(r, 10, 480), built_at: dtc(NOW - RI(r, 60, 280))}), {ttl: 300});
  }),
  rmeta: fam('hash', /^report:meta:(\d+)$/, i => 'report:meta:' + (100000 + (h32('rm' + i) % 900000)), (m, nm) => {
    const r = rr(nm);
    return mkHash([['name', 'sales_daily'], ['owner', PK(r, ['wang.lei', 'li.na', 'zhao.yang'])], ['rows', String(RI(r, 10, 480))], ['cost_ms', String(RI(r, 40, 2400))], ['status', 'ok'], ['built_at', dtc(NOW - RI(r, 60, 3600))]], {ttl: 3600});
  }),
};
const FAM_ORDER2 = {
  task: fam('hash', /^task:(\d+)$/, i => 'task:' + (500000 + (h32('tk' + i) % 4000000)), (m, nm) => {
    const r = rr(nm);
    return mkHash([['id', m[1]], ['type', PK(r, ['order.timeout', 'order.sync', 'stock.recalc', 'coupon.expire'])], ['status', PK(r, ['done', 'done', 'done', 'retry'])], ['retries', String(PK(r, [0, 0, 0, 1, 2]))],
      ['created_at', dtc(NOW - RI(r, 60, 86400))], ['payload_len', String(RI(r, 40, 900))]], {ttl: 86400});
  }),
  tres: fam('string', /^task:result:(\d+)$/, i => 'task:result:' + (500000 + (h32('tr' + i) % 4000000)), (m, nm) => mkStr(jsonLine({ok: true, cost_ms: RI(rr(nm), 3, 900)}), {ttl: 3600})),
};
const FAM_ORDER5 = {
  flag: fam('string', /^feature:flag:(\d+)$/, i => 'feature:flag:' + (1000 + (h32('ff' + i) % 90000)), (m, nm) => mkStr(PK(rr(nm), ['0', '1', '1']))),
};

/* ---- 固定的“有名字”的键（演示各类型、大 key、二进制、超长值） ---- */
const GEO_STORES = [['sh-pudong', 121.5440, 31.2212], ['sh-xuhui', 121.4366, 31.1884], ['sh-jingan', 121.4472, 31.2290], ['hz-xihu', 120.1304, 30.2590], ['hz-binjiang', 120.2105, 30.2084],
  ['bj-chaoyang', 116.4837, 39.9219], ['bj-haidian', 116.2989, 39.9593], ['sz-nanshan', 113.9304, 22.5329], ['gz-tianhe', 113.3248, 23.1353], ['cd-gaoxin', 104.0668, 30.5728], ['nj-gulou', 118.7695, 32.0662], ['wh-hongshan', 114.4044, 30.5058]];
const geoScore = (lon, lat) => Math.floor((lon + 180) / 360 * 67108864) * 67108864 + Math.floor((lat + 90) / 180 * 67108864);
function geoZset(list) {
  const items = list.map(([m, lon, lat]) => [m, geoScore(lon, lat)]).sort((a, b) => a[1] - b[1]);
  const coord = new Map(list.map(([m, lon, lat]) => [m, [lon, lat]]));
  return mkZset(items, {geo: true, coord});
}
const lazy = f => { let v; return () => v || (v = f()); };
const bigFeed = lazy(() => {
  const r = rr('cache:home:feed'), items = [];
  for (let i = 0; items.join(',').length < 70000; i++) items.push(jsonLine({id: 100000 + i, title: S('热销推荐 ' + PK(r, SKU_NAMES)), price: (RI(r, 990, 99900) / 100).toFixed(2), tags: ['new', 'hot'], pos: i}));
  return '{"feed":[' + items.join(',') + ']}';
});
const binBlob = () => Array.from({length: 32}, (_, k) => String.fromCharCode((k * 37 + 11) & 255)).join('');
const hllBlob = lazy(() => 'HYLL\x01\0\0\0' + Array.from({length: 12296}, (_, k) => String.fromCharCode(k % 7 === 0 ? 0 : (k * 53 + 7) & 255)).join(''));
const FIXED_ORDER0 = {
  'counter:order:today': () => mkStr('184203'),
  'config:order:switch': () => mkHash([['auto_cancel_minutes', '30'], ['coupon_enabled', '1'], ['risk_check', 'on'], ['max_items_per_order', '99'], ['pay_timeout_seconds', '900'], ['refund_window_days', '15'],
    ['invoice_enabled', '1'], ['gray_ratio', '0.15'], ['gray_whitelist', '10086,10010,20001'], ['maintenance', 'off'], ['updated_by', 'wang.lei'], ['updated_at', '2026-10-05 21:30:12']]),
  'hot:sku:rank:20261007': () => numberedZset(2000, 'sku:', 100000, 3, i => 5 + i * 2 + (h32('r' + i) % 2), {ttl: 86400}),
  'order:timeout:queue': () => mkColl('list', 8420, i => `O20261007${pad(10000000 + i * 13, 8)}|${NOW + 1800 + i * 2}`, null),
  'order:paid:20261007': () => numberedSet(31288, '', 80000000, 7, {ttl: 86400}),
  'order:paid:20261005': () => numberedSet(126, '', 80000000, 11, {ttl: 5 * 86400}),
  'stream:order:events': () => mkStream(58320, (NOW - 58320 * 2) * 1000, 2000, i => { const r = rr('ev' + i); return ['event', PK(r, ['paid', 'paid', 'ship', 'sign', 'cancel']), 'order_no', 'O20261007' + idN(i, 5), 'user_id', String(RI(r, 10000, 9800000))]; }),
  'geo:stores': () => geoZset(GEO_STORES),
  'big:order:index': () => numberedHash(182044, 'o:', 80000000, 7, i => String(i % 64)),
  'blob:sign:key': () => mkStr(binBlob()),
  'cache:home:feed': () => { const v = bigFeed(); return mkStr(v, {ttl: 120}); },
  'bitmap:active:20261007': () => ({type: 'string', ttl: 86400, len: 131072, synth: true}),
  'bitmap:active:2026': () => ({type: 'string', ttl: -1, len: 20971520, synth: true}),
  'hll:uv:20261007': () => mkStr(hllBlob(), {hll: 482331, ttl: 86400}),
};
const FIXED_ORDER1 = {
  'report:lastbuild': () => mkStr('2026-10-07 10:30:02'),
  'stat:gmv:20261007': () => mkHash(Array.from({length: 11}, (_, k) => [pad(k, 2), String(RI(rr('gmv' + k), 8000, 90000)) + '.00']), {ttl: 86400}),
};
const FIXED_ORDER2 = {
  'task:queue:pending': () => mkColl('list', 640, i => jsonLine({id: 4100000 + i, type: 'order.timeout', retry: 0}), null),
  'task:queue:failed': () => mkColl('list', 37, i => jsonLine({id: 3900000 + i * 3, type: 'stock.recalc', err: 'timeout'}), null),
  'task:lastrun': () => mkStr('2026-10-07 10:40:00'),
};
const FIXED_ORDER5 = Object.fromEntries(['new_checkout', 'coupon_v2', 'pay_retry', 'risk_gray', 'search_rank_b', 'invoice_auto', 'refund_fast', 'cart_merge', 'sku_preload', 'push_remind', 'gray_ratio', 'maintenance'].map(n => ['feature:' + n, () => mkStr(PK(rr('f' + n), ['on', 'off', 'on']))]));
const FIXED_SESSION0 = {
  'user:online': () => numberedSet(128330, '', 10000, 71, {}),
  'token:blacklist': () => numberedZset(9214, 'jti:', 0, 1, i => NOW + 300 + i * 7, {}),
  'config:session:policy': () => mkHash([['idle_timeout_seconds', '1800'], ['absolute_timeout_hours', '12'], ['max_devices', '3'], ['rotate_token', 'on'], ['bind_ip', 'off'], ['same_site', 'Lax'], ['secure_cookie', 'on'], ['remember_days', '14'], ['updated_at', '2026-09-30 18:02:41']]),
  'stat:login:20261007': () => mkHash(Array.from({length: 11}, (_, k) => [pad(k, 2), String(RI(rr('lg' + k), 1200, 52000))]), {ttl: 2 * 86400}),
  'sess:index:uid': () => numberedHash(92401, 'u:', 10000, 97, i => hex32(i, 1).slice(0, 12)),
  'login:geo:recent': () => geoZset(GEO_STORES.slice(0, 8)),
};
const FIXED_SESSION1 = {
  'config:ratelimit': () => mkHash([['api_per_min', '100'], ['sms_per_hour', '5'], ['login_per_min', '10'], ['whitelist', '10.20.8.14,10.20.8.15']]),
};
/* 数据集：每个 db 的键数、键族循环（决定 SCAN 的枚举顺序）、固定键 */
const DATASETS = {
  order: {
    0: {n: 1284530, fams: FAM_ORDER0, cycle: ['detail', 'status', 'detail', 'stock', 'cart', 'skuinfo', 'detail', 'lock', 'status', 'items'], fixed: FIXED_ORDER0},
    1: {n: 52310, fams: FAM_ORDER1, cycle: ['rcache', 'rcache', 'rmeta'], fixed: FIXED_ORDER1},
    2: {n: 8120, fams: FAM_ORDER2, cycle: ['task', 'tres', 'task'], fixed: FIXED_ORDER2},
    5: {n: 312, fams: FAM_ORDER5, cycle: ['flag'], fixed: FIXED_ORDER5},
  },
  session: {
    0: {n: 3862014, fams: FAM_SESSION0, cycle: ['sess', 'sess', 'sdata', 'sess', 'fail', 'sess', 'sdata', 'captcha', 'sess', 'devices'], fixed: FIXED_SESSION0},
    1: {n: 120553, fams: FAM_SESSION1, cycle: ['profile', 'rlapi', 'profile', 'rlapi', 'rlsms', 'profile'], fixed: FIXED_SESSION1},
  },
};
for (const ds of Object.values(DATASETS)) for (const d of Object.values(ds)) { d.fixedNames = Object.keys(d.fixed); d.fixedAt = new Map(d.fixedNames.map((nm, j) => [3 + 11 * j, nm])); }
const dsName = inst => /session/.test(inst.name || '') ? 'session' : 'order';
const dbDef = (inst, dbn) => (DATASETS[dsName(inst)] || {})[dbn] || null;
const keyCount = (inst, dbn) => { const d = dbDef(inst, dbn); if (!d) return 0; return d.n - (inst.role === 'slave' && dbn === 0 ? 37 : 0); };
const dbs = inst => Array.from({length: 16}, (_, n) => ({db: String(n), keys: keyCount(inst, n)}));
const dbLabel = d => d.keys >= 0 ? `db${d.db} (${d.keys.toLocaleString('en-US')})` : 'db' + d.db;
const dbTxt = db => 'db' + String(db).replace(/^db/i, '');
const sameDb = (a, b) => String(a).replace(/^db/i, '') === String(b).replace(/^db/i, '');

const SPEC_CACHE = new Map();
function lookup(inst, dbn, name) {
  const d = dbDef(inst, dbn); if (!d) return null;
  const ck = inst.id + '/' + dbn + '/' + name;
  if (SPEC_CACHE.has(ck)) return SPEC_CACHE.get(ck);
  let spec = null;
  if (Object.prototype.hasOwnProperty.call(d.fixed, name)) spec = d.fixed[name]();
  else for (const f of Object.values(d.fams)) { const m = f.re.exec(name); if (m) { spec = f.make(m, name); break; } }
  if (spec) { spec.name = name; if (SPEC_CACHE.size > 3000) SPEC_CACHE.clear(); SPEC_CACHE.set(ck, spec); }
  return spec;
}
/* SCAN 的枚举：第 i 个键（固定键散布在前 ~400 个位置里，其余按键族循环） */
function keyAtI(inst, dbn, i) {
  const d = dbDef(inst, dbn);
  const fx = d.fixedAt.get(i);
  if (fx) return {name: fx, type: lookup(inst, dbn, fx).type};
  const f = d.fams[d.cycle[i % d.cycle.length]];
  return {name: f.name(i), type: f.type};
}

/* ---- glob（Redis 的 MATCH 语法：* ? [abc] [^a-z] \x） ---- */
function globToRe(p) {
  let re = '^';
  for (let i = 0; i < p.length; i++) {
    const c = p[i];
    if (c === '*') re += '[\\s\\S]*';
    else if (c === '?') re += '[\\s\\S]';
    else if (c === '[') {
      let j = i + 1, cls = '';
      if (p[j] === '^') { cls = '^'; j++; }
      while (j < p.length && p[j] !== ']') { cls += p[j] === '\\' && j + 1 < p.length ? '\\' + p[++j] : (p[j] === '\\' || p[j] === '[' ? '\\' + p[j] : p[j]); j++; }
      re += '[' + cls + ']'; i = j;
    } else if (c === '\\' && i + 1 < p.length) re += p[++i].replace(/[.*+?^${}()|[\]\\\/]/g, '\\$&');
    else re += c.replace(/[.*+?^${}()|[\]\\\/]/g, '\\$&');
  }
  try { return new RegExp(re + '$'); } catch (e) { return /^$/; }
}
const globMatcher = p => (p == null || p === '*') ? null : globToRe(p);

/* ============================================================================
   四、模拟 Redis 服务端：对规格执行只读命令（回复形态与 RESP2 一致：null / 整数 / 字符串 / 数组）
   ========================================================================== */
const WRONG = 'WRONGTYPE Operation against a key holding the wrong kind of value';
const asInt = v => typeof v === 'number' ? v : 0;
const fScore = n => String(n);
function getKey(cx, k) { return lookup(cx.inst, cx.db, k); }
function typed(cx, k, type) { const s = getKey(cx, k); if (!s) return null; if (s.type !== type) throw new SErr(WRONG); return s; }
function span(len, s, e) { if (s < 0) s += len; if (e < 0) e += len; s = Math.max(s, 0); if (e >= len) e = len - 1; return len === 0 || s > e ? null : [s, e]; }
function remainTtl(spec) { return spec.ttl < 0 ? -1 : 1 + (h32(spec.name + '#ttl') % spec.ttl); }
const range = (a, b) => { const out = []; for (let i = a; i <= b; i++) out.push(i); return out; };
const bytesOf = (spec, from, to) => {                               // synth 位图的字节（确定性伪随机）
  let s = ''; for (let i = from; i <= to; i++) s += String.fromCharCode(h32(spec.name + ':' + i) & 255); return s;
};
const popcnt = b => { let n = 0; while (b) { n += b & 1; b >>= 1; } return n; };
const allOf = spec => range(0, spec.len - 1).map(spec.at);
const moreThan = (spec, n) => { if (spec.len > n) throw new SErr('ERR 回复超过在线查询的读取范围'); };

function scanColl(spec, cursor, count, match, kind) {               // HSCAN / SSCAN / ZSCAN：返回 [游标, 扁平数组]
  const re = globMatcher(match), len = spec.len, small = len <= 128;
  let idx = small ? 0 : Math.min(parseInt(cursor, 10), len), end = small ? len : Math.min(len, idx + count);
  const out = [];
  for (let i = idx; i < end; i++) {
    const el = spec.at(i), name = Array.isArray(el) ? el[0] : el;
    if (re && !re.test(name)) continue;
    if (kind === 'hash') out.push(name, el[1]);
    else if (kind === 'hashkeys') out.push(name);
    else if (kind === 'zset') out.push(name, fScore(el[1]));
    else out.push(name);
  }
  return [end >= len ? '0' : String(end), out];
}
function pickRandom(spec, count, withVals) {                         // HRANDFIELD / SRANDMEMBER
  const r = rng(Date.now() & 0xffffff), len = spec.len, out = [];
  if (count === undefined) { const el = spec.at(Math.floor(r() * len)); return Array.isArray(el) ? el[0] : el; }
  const n = Math.abs(count), uniq = count >= 0, used = new Set();
  for (let k = 0; k < n && (!uniq || used.size < len); k++) {
    let i = Math.floor(r() * len); while (uniq && used.has(i)) i = (i + 1) % len; used.add(i);
    const el = spec.at(i); if (Array.isArray(el)) { out.push(el[0]); if (withVals) out.push(el[1]); } else out.push(el);
  }
  return out;
}
/* 有序集合：按分数的二分 */
function parseBound(s) {
  let ex = false; if (s[0] === '(') { ex = true; s = s.slice(1); }
  const l = s.toLowerCase(), v = l === '-inf' ? -Infinity : l === '+inf' || l === 'inf' ? Infinity : Number(s);
  if (Number.isNaN(v)) throw new SErr('ERR min or max is not a float');
  return {v, ex};
}
function lowerIdx(spec, b) { let lo = 0, hi = spec.len; while (lo < hi) { const m = (lo + hi) >> 1, sc = spec.at(m)[1]; if (b.ex ? sc <= b.v : sc < b.v) lo = m + 1; else hi = m; } return lo; }
function upperIdx(spec, b) { let lo = 0, hi = spec.len; while (lo < hi) { const m = (lo + hi) >> 1, sc = spec.at(m)[1]; if (b.ex ? sc < b.v : sc <= b.v) lo = m + 1; else hi = m; } return lo - 1; }
function lexBound(s) {
  if (s === '-') return {k: -1}; if (s === '+') return {k: 1};
  if (s[0] === '[') return {k: 0, v: s.slice(1), ex: false};
  if (s[0] === '(') return {k: 0, v: s.slice(1), ex: true};
  throw new SErr('ERR min or max not valid string range item');
}
function lexRange(spec, minS, maxS) {
  const a = lexBound(minS), b = lexBound(maxS), out = [];
  for (let i = 0; i < spec.len; i++) {
    const m = spec.at(i)[0];
    if (a.k === 0 && (a.ex ? m <= a.v : m < a.v)) continue;
    if (b.k === 0 && (b.ex ? m >= b.v : m > b.v)) continue;
    if (a.k === 1 || b.k === -1) continue;
    out.push(i);
  }
  return out;
}
function zOut(spec, idxs, withScores) {
  const out = []; for (const i of idxs) { const el = spec.at(i); out.push(el[0]); if (withScores) out.push(fScore(el[1])); } return out;
}
function limitList(idxs, lim) { if (!lim) return idxs; const [off, n] = lim; return idxs.slice(off, n < 0 ? undefined : off + n); }
function limitOf(a) { const p = a.findIndex(x => x.toUpperCase() === 'LIMIT'); return p < 0 ? null : [parseInt(a[p + 1], 10), parseInt(a[p + 2], 10)]; }
function zrangeCmd(cx, a, rev, mode) {                                // a = 去掉命令名后的参数
  const spec = typed(cx, a[0], 'zset'), tail = a.slice(3), ws = tail.some(x => x.toUpperCase() === 'WITHSCORES');
  if (!spec) return [];
  let idxs;
  if (mode === 'idx') {
    const sp = span(spec.len, parseInt(a[1], 10), parseInt(a[2], 10)); if (!sp) return [];
    idxs = range(sp[0], sp[1]); if (rev) idxs = idxs.map(i => spec.len - 1 - i);
  } else if (mode === 'score') {
    const lo = parseBound(rev ? a[2] : a[1]), hi = parseBound(rev ? a[1] : a[2]);
    const s = lowerIdx(spec, lo), e = upperIdx(spec, hi); idxs = s > e ? [] : range(s, e); if (rev) idxs.reverse();
    idxs = limitList(idxs, limitOf(tail));
  } else {
    const lo = rev ? a[2] : a[1], hi = rev ? a[1] : a[2];
    idxs = lexRange(spec, lo, hi); if (rev) idxs.reverse();
    idxs = limitList(idxs, limitOf(tail));
  }
  return zOut(spec, idxs, ws);
}
/* 地理 */
const UNIT = {m: 1, km: 1000, ft: 0.3048, mi: 1609.344};
function haversine(a, b) {
  const R = 6372797.560856, rad = x => x * Math.PI / 180, dLat = rad(b[1] - a[1]), dLon = rad(b[0] - a[0]);
  const h = Math.sin(dLat / 2) ** 2 + Math.cos(rad(a[1])) * Math.cos(rad(b[1])) * Math.sin(dLon / 2) ** 2;
  return 2 * R * Math.asin(Math.sqrt(h));
}
function geoSpec(cx, k) { const s = typed(cx, k, 'zset'); return s; }
function geoCoord(spec, m) { return spec && spec.geo ? spec.coord.get(m) || null : null; }
function geoQuery(spec, center, shape, o) {
  const out = [];
  if (!spec || !spec.geo) return out;
  for (const [m, c] of spec.coord) {
    let ok, d = haversine(center, c);
    if (shape.t === 'radius') ok = d <= shape.r;
    else { const dx = Math.abs(c[0] - center[0]) * 111320 * Math.cos(center[1] * Math.PI / 180), dy = Math.abs(c[1] - center[1]) * 111320; ok = dx <= shape.w / 2 && dy <= shape.h / 2; }
    if (ok) out.push({m, d, c});
  }
  out.sort((x, y) => o.desc ? y.d - x.d : x.d - y.d);
  const res = out.slice(0, o.count);
  return res.map(x => {
    if (!o.withDist && !o.withHash && !o.withCoord) return x.m;
    const e = [x.m];
    if (o.withDist) e.push((x.d / o.unit).toFixed(4));
    if (o.withHash) e.push(geoScore(x.c[0], x.c[1]));
    if (o.withCoord) e.push([x.c[0].toFixed(15), x.c[1].toFixed(15)]);
    return e;
  });
}
const optsOf = a => { const u = a.map(x => x.toUpperCase()); const ci = u.indexOf('COUNT'); return {withDist: u.includes('WITHDIST'), withHash: u.includes('WITHHASH'), withCoord: u.includes('WITHCOORD'), desc: u.includes('DESC'), count: ci < 0 ? RANGE_MAX : parseInt(a[ci + 1], 10)}; };
/* 流 ID 比较 */
function sid(str, isEnd) {
  if (str === '-') return {ms: -Infinity, seq: 0, ex: false};
  if (str === '+') return {ms: Infinity, seq: Infinity, ex: false};
  let ex = false; if (str[0] === '(') { ex = true; str = str.slice(1); }
  const p = str.split('-'), ms = Number(p[0]), seq = p.length > 1 ? Number(p[1]) : (isEnd ? Infinity : 0);
  if (!/^\d+$/.test(p[0]) || Number.isNaN(seq)) throw new SErr('ERR Invalid stream ID specified as stream command argument');
  return {ms, seq, ex};
}
const cmpId = (ms, seq, b) => ms !== b.ms ? (ms < b.ms ? -1 : 1) : seq === b.seq ? 0 : seq < b.seq ? -1 : 1;
function streamRange(spec, lo, hi, count, rev) {
  let s = 0, e = spec.len - 1;
  { let l = 0, h = spec.len; while (l < h) { const m = (l + h) >> 1, c = cmpId(spec.ms(m), 0, lo); if (c < 0 || (c === 0 && lo.ex)) l = m + 1; else h = m; } s = l; }
  { let l = 0, h = spec.len; while (l < h) { const m = (l + h) >> 1, c = cmpId(spec.ms(m), 0, hi); if (c < 0 || (c === 0 && !hi.ex)) l = m + 1; else h = m; } e = l - 1; }
  if (s > e) return [];
  if (rev) { const r = []; for (let i = e; i >= s && r.length < count; i--) r.push(i); return r.map(i => spec.at(i)); }
  return range(s, Math.min(e, s + count - 1)).map(i => spec.at(i));
}
const encOf = (spec, inst) => {
  const v7 = parseInt(inst.ver, 10) >= 7, small = spec.len <= 128;
  switch (spec.type) {
    case 'string': return /^-?\d{1,19}$/.test(spec.val ? spec.val() : '') ? 'int' : spec.len <= 44 ? 'embstr' : 'raw';
    case 'hash': return small ? (v7 ? 'listpack' : 'ziplist') : 'hashtable';
    case 'list': return small && v7 ? 'listpack' : 'quicklist';
    case 'set': return spec.len <= 512 && spec.at(0) && /^\d+$/.test(spec.at(0)) ? 'intset' : 'hashtable';
    case 'zset': return small ? (v7 ? 'listpack' : 'ziplist') : 'skiplist';
    default: return 'stream';
  }
};
const memOf = spec => spec.type === 'string' ? 56 + spec.len : spec.type === 'stream' ? 1200 + spec.len * 96 : 120 + spec.len * (spec.type === 'hash' ? 72 : spec.type === 'zset' ? 64 : 48);

const EX = {
  /* 字符串 */
  GET(cx, a) {
    const s = typed(cx, a[0], 'string'); if (!s) return null;
    if (s.len > BITCOUNT_MAX) throw new SErr('ERR 回复超过 16MB 的单个批量上限，请用 GETRANGE 分段读取');
    return s.synth ? bytesOf(s, 0, s.len - 1) : s.val();
  },
  MGET(cx, a) { return a.map(k => { const s = getKey(cx, k); return s && s.type === 'string' && !s.synth ? s.val() : null; }); },
  STRLEN(cx, a) { const s = typed(cx, a[0], 'string'); return s ? s.len : 0; },
  GETRANGE(cx, a) {
    const s = typed(cx, a[0], 'string'); if (!s) return '';
    const sp = span(s.len, parseInt(a[1], 10), parseInt(a[2], 10)); if (!sp) return '';
    return s.synth ? bytesOf(s, sp[0], Math.min(sp[1], sp[0] + 4095)) : s.val().slice(sp[0], sp[1] + 1);
  },
  EXISTS(cx, a) { return a.filter(k => getKey(cx, k)).length; },
  TYPE(cx, a) { const s = getKey(cx, a[0]); return s ? s.type : 'none'; },
  TTL(cx, a) { const s = getKey(cx, a[0]); return !s ? -2 : remainTtl(s); },
  PTTL(cx, a) { const s = getKey(cx, a[0]); if (!s) return -2; const t = remainTtl(s); return t < 0 ? -1 : t * 1000 - (h32(s.name) % 1000); },
  EXPIRETIME(cx, a) { const s = getKey(cx, a[0]); if (!s) return -2; const t = remainTtl(s); return t < 0 ? -1 : NOW + t; },
  PEXPIRETIME(cx, a) { const s = getKey(cx, a[0]); if (!s) return -2; const t = remainTtl(s); return t < 0 ? -1 : (NOW + t) * 1000; },
  /* 哈希 */
  HGET(cx, a) { const s = typed(cx, a[0], 'hash'); if (!s) return null; const i = s.inv(a[1]); return i < 0 ? null : s.at(i)[1]; },
  HMGET(cx, a) { const s = typed(cx, a[0], 'hash'); return a.slice(1).map(f => { if (!s) return null; const i = s.inv(f); return i < 0 ? null : s.at(i)[1]; }); },
  HGETALL(cx, a) { const s = typed(cx, a[0], 'hash'); if (!s) return []; moreThan(s, FULL_MAX); return allOf(s).flat(); },
  HLEN(cx, a) { const s = typed(cx, a[0], 'hash'); return s ? s.len : 0; },
  HKEYS(cx, a) { const s = typed(cx, a[0], 'hash'); if (!s) return []; moreThan(s, FULL_MAX); return allOf(s).map(p => p[0]); },
  HVALS(cx, a) { const s = typed(cx, a[0], 'hash'); if (!s) return []; moreThan(s, FULL_MAX); return allOf(s).map(p => p[1]); },
  HEXISTS(cx, a) { const s = typed(cx, a[0], 'hash'); return s && s.inv(a[1]) >= 0 ? 1 : 0; },
  HSTRLEN(cx, a) { const s = typed(cx, a[0], 'hash'); if (!s) return 0; const i = s.inv(a[1]); return i < 0 ? 0 : s.at(i)[1].length; },
  HSCAN(cx, a) {
    const s = typed(cx, a[0], 'hash'); if (!s) return ['0', []];
    const u = a.map(x => x.toUpperCase()), mi = u.indexOf('MATCH'), ci = u.indexOf('COUNT');
    return scanColl(s, a[1], parseInt(a[ci + 1], 10), mi < 0 ? null : a[mi + 1], u.includes('NOVALUES') ? 'hashkeys' : 'hash');
  },
  HRANDFIELD(cx, a) { const s = typed(cx, a[0], 'hash'); if (!s) return a.length > 1 ? [] : null; return pickRandom(s, a.length > 1 ? parseInt(a[1], 10) : undefined, a.length > 2); },
  /* 列表 */
  LRANGE(cx, a) { const s = typed(cx, a[0], 'list'); if (!s) return []; const sp = span(s.len, parseInt(a[1], 10), parseInt(a[2], 10)); return sp ? range(sp[0], sp[1]).map(s.at) : []; },
  LLEN(cx, a) { const s = typed(cx, a[0], 'list'); return s ? s.len : 0; },
  LINDEX(cx, a) { const s = typed(cx, a[0], 'list'); if (!s) return null; let i = parseInt(a[1], 10); if (i < 0) i += s.len; return i < 0 || i >= s.len ? null : s.at(i); },
  LPOS(cx, a) {
    const s = typed(cx, a[0], 'list'), u = a.map(x => x.toUpperCase()), opt = k => { const p = u.indexOf(k, 2); return p < 0 ? null : parseInt(a[p + 1], 10); };
    const rank = opt('RANK') || 1, cnt = opt('COUNT'), maxlen = opt('MAXLEN') || LPOS_MAXLEN, hits = [];
    if (s) {
      const lim = Math.min(s.len, maxlen);
      if (rank > 0) for (let i = 0, skip = rank - 1; i < lim; i++) { if (s.at(i) === a[1]) { if (skip > 0) skip--; else hits.push(i); } }
      else for (let i = s.len - 1, skip = -rank - 1, seen = 0; i >= 0 && seen < lim; i--, seen++) { if (s.at(i) === a[1]) { if (skip > 0) skip--; else hits.push(i); } }
    }
    return cnt === null ? (hits.length ? hits[0] : null) : hits.slice(0, cnt);
  },
  /* 集合 */
  SMEMBERS(cx, a) { const s = typed(cx, a[0], 'set'); if (!s) return []; moreThan(s, FULL_MAX); return allOf(s); },
  SCARD(cx, a) { const s = typed(cx, a[0], 'set'); return s ? s.len : 0; },
  SISMEMBER(cx, a) { const s = typed(cx, a[0], 'set'); return s && s.inv(a[1]) >= 0 ? 1 : 0; },
  SMISMEMBER(cx, a) { const s = typed(cx, a[0], 'set'); return a.slice(1).map(m => s && s.inv(m) >= 0 ? 1 : 0); },
  SRANDMEMBER(cx, a) { const s = typed(cx, a[0], 'set'); if (!s) return a.length > 1 ? [] : null; return pickRandom(s, a.length > 1 ? parseInt(a[1], 10) : undefined, false); },
  SSCAN(cx, a) {
    const s = typed(cx, a[0], 'set'); if (!s) return ['0', []];
    const u = a.map(x => x.toUpperCase()), mi = u.indexOf('MATCH'), ci = u.indexOf('COUNT');
    return scanColl(s, a[1], parseInt(a[ci + 1], 10), mi < 0 ? null : a[mi + 1], 'set');
  },
  SINTER(cx, a) { const ss = a.map(k => typed(cx, k, 'set')); if (ss.some(s => !s)) return []; const base = allOf(ss[0]); return base.filter(m => ss.slice(1).every(s => s.inv(m) >= 0)); },
  SUNION(cx, a) { const out = new Set(); for (const k of a) { const s = typed(cx, k, 'set'); if (s) allOf(s).forEach(m => out.add(m)); } return [...out]; },
  SDIFF(cx, a) { const s0 = typed(cx, a[0], 'set'); if (!s0) return []; const rest = a.slice(1).map(k => typed(cx, k, 'set')); return allOf(s0).filter(m => rest.every(s => !s || s.inv(m) < 0)); },
  /* 有序集合 */
  ZRANGE(cx, a) {
    const u = a.map(x => x.toUpperCase()), rev = u.includes('REV', 3), mode = u.includes('BYSCORE', 3) ? 'score' : u.includes('BYLEX', 3) ? 'lex' : 'idx';
    return zrangeCmd(cx, a, rev, mode);
  },
  ZREVRANGE(cx, a) { return zrangeCmd(cx, a, true, 'idx'); },
  ZRANGEBYSCORE(cx, a) { return zrangeCmd(cx, a, false, 'score'); },
  ZREVRANGEBYSCORE(cx, a) { return zrangeCmd(cx, a, true, 'score'); },
  ZRANGEBYLEX(cx, a) { return zrangeCmd(cx, a, false, 'lex'); },
  ZCARD(cx, a) { const s = typed(cx, a[0], 'zset'); return s ? s.len : 0; },
  ZSCORE(cx, a) { const s = typed(cx, a[0], 'zset'); if (!s) return null; const i = s.inv(a[1]); return i < 0 ? null : fScore(s.at(i)[1]); },
  ZMSCORE(cx, a) { const s = typed(cx, a[0], 'zset'); return a.slice(1).map(m => { if (!s) return null; const i = s.inv(m); return i < 0 ? null : fScore(s.at(i)[1]); }); },
  ZRANK(cx, a) { const s = typed(cx, a[0], 'zset'); if (!s) return null; const i = s.inv(a[1]); if (i < 0) return null; return a.length > 2 ? [i, fScore(s.at(i)[1])] : i; },
  ZREVRANK(cx, a) { const s = typed(cx, a[0], 'zset'); if (!s) return null; const i = s.inv(a[1]); if (i < 0) return null; return a.length > 2 ? [s.len - 1 - i, fScore(s.at(i)[1])] : s.len - 1 - i; },
  ZCOUNT(cx, a) { const s = typed(cx, a[0], 'zset'); if (!s) return 0; return Math.max(0, upperIdx(s, parseBound(a[2])) - lowerIdx(s, parseBound(a[1])) + 1); },
  ZLEXCOUNT(cx, a) { const s = typed(cx, a[0], 'zset'); return s ? lexRange(s, a[1], a[2]).length : 0; },
  ZSCAN(cx, a) {
    const s = typed(cx, a[0], 'zset'); if (!s) return ['0', []];
    const u = a.map(x => x.toUpperCase()), mi = u.indexOf('MATCH'), ci = u.indexOf('COUNT');
    return scanColl(s, a[1], parseInt(a[ci + 1], 10), mi < 0 ? null : a[mi + 1], 'zset');
  },
  /* 其他 */
  SCAN(cx, a) {
    const u = a.map(x => x.toUpperCase()), mi = u.indexOf('MATCH', 1), ci = u.indexOf('COUNT', 1), ti = u.indexOf('TYPE', 1);
    const re = globMatcher(mi < 0 ? null : a[mi + 1]), count = parseInt(a[ci + 1], 10), type = ti < 0 ? null : a[ti + 1].toLowerCase(), N = keyCount(cx.inst, cx.db), idx = parseInt(a[0], 10), out = [];
    if (!N || idx >= N) return ['0', []];
    const end = Math.min(N, idx + count);
    for (let i = idx; i < end; i++) { const k = keyAtI(cx.inst, cx.db, i); if (re && !re.test(k.name)) continue; if (type && k.type !== type) continue; out.push(k.name); }
    return [end >= N ? '0' : String(end), out];
  },
  DBSIZE(cx) { return keyCount(cx.inst, cx.db); },
  XLEN(cx, a) { const s = typed(cx, a[0], 'stream'); return s ? s.len : 0; },
  XRANGE(cx, a) {
    const s = typed(cx, a[0], 'stream'); if (!s) return [];
    const ci = a.findIndex((x, i) => i > 2 && x.toUpperCase() === 'COUNT'), n = ci < 0 ? RANGE_MAX : parseInt(a[ci + 1], 10);
    return streamRange(s, sid(a[1], false), sid(a[2], true), n, false);
  },
  XREVRANGE(cx, a) {
    const s = typed(cx, a[0], 'stream'); if (!s) return [];
    const ci = a.findIndex((x, i) => i > 2 && x.toUpperCase() === 'COUNT'), n = ci < 0 ? RANGE_MAX : parseInt(a[ci + 1], 10);
    return streamRange(s, sid(a[2], false), sid(a[1], true), n, true);
  },
  BITCOUNT(cx, a) {
    const s = typed(cx, a[0], 'string'); if (!s) return 0;
    let from = 0, to = s.len - 1, bit = false;
    if (a.length > 1) { bit = (a[3] || '').toUpperCase() === 'BIT'; const lim = bit ? s.len * 8 : s.len, sp = span(lim, parseInt(a[1], 10), parseInt(a[2], 10)); if (!sp) return 0; from = sp[0]; to = sp[1]; }
    const nbytes = bit ? Math.ceil((to - from + 1) / 8) : to - from + 1;
    if (s.synth) return Math.round(nbytes * (bit ? .5 : 4) * (0.96 + (h32(s.name + from + ':' + to) % 80) / 1000));
    const v = s.val(); let n = 0;
    if (bit) { for (let i = from; i <= to; i++) n += (v.charCodeAt(i >> 3) >> (7 - (i & 7))) & 1; return n; }
    for (let i = from; i <= to; i++) n += popcnt(v.charCodeAt(i)); return n;
  },
  GETBIT(cx, a) {
    const s = typed(cx, a[0], 'string'); const off = parseInt(a[1], 10); if (!s || !(off >= 0) || off >= s.len * 8) return 0;
    if (s.synth) return h32(s.name + '#' + off) & 1;
    return (s.val().charCodeAt(off >> 3) >> (7 - (off & 7))) & 1;
  },
  PFCOUNT(cx, a) {
    let n = 0;
    for (const k of a) { const s = getKey(cx, k); if (!s) continue; if (s.type !== 'string' || !s.hll) throw new SErr(s.type !== 'string' ? WRONG : 'WRONGTYPE Key is not a valid HyperLogLog string value.'); n = a.length === 1 ? s.hll : Math.round(Math.max(n, s.hll) * 1.03); }
    return n;
  },
  OBJECT(cx, a) {                                                 // a = [子命令, key]
    const sub = a[0], s = getKey(cx, a[1]);
    if (!s) return null;
    if (sub === 'ENCODING') return encOf(s, cx.inst);
    if (sub === 'IDLETIME') return h32(s.name + '#idle') % 86400;
    throw new SErr('ERR An LFU maxmemory policy is not selected, access frequency not tracked. Please note that when switching between policies at runtime LRU and LFU data will take some time to adjust.');
  },
  MEMORY(cx, a) { const s = getKey(cx, a[1]); return s ? memOf(s) : null; },   // a = ['USAGE', key, ...]
  GEOPOS(cx, a) { const s = geoSpec(cx, a[0]); return a.slice(1).map(m => { const c = geoCoord(s, m); return c ? [c[0].toFixed(15), c[1].toFixed(15)] : null; }); },
  GEODIST(cx, a) {
    const s = geoSpec(cx, a[0]), c1 = geoCoord(s, a[1]), c2 = geoCoord(s, a[2]); if (!c1 || !c2) return null;
    return (haversine(c1, c2) / UNIT[(a[3] || 'm').toLowerCase()]).toFixed(4);
  },
  GEORADIUS_RO(cx, a) {
    const s = geoSpec(cx, a[0]), unit = a[4].toLowerCase(), o = optsOf(a.slice(5));
    return geoQuery(s, [Number(a[1]), Number(a[2])], {t: 'radius', r: Number(a[3]) * UNIT[unit]}, Object.assign(o, {unit: UNIT[unit]}));
  },
  GEOSEARCH(cx, a) {
    const s = geoSpec(cx, a[0]), u = a.map(x => x.toUpperCase()), o = optsOf(a.slice(1));
    let center, shape, unit;
    const fm = u.indexOf('FROMMEMBER'), fl = u.indexOf('FROMLONLAT'), br = u.indexOf('BYRADIUS'), bb = u.indexOf('BYBOX');
    if (fm > 0) { center = geoCoord(s, a[fm + 1]); if (!center) throw new SErr('ERR could not decode requested zset member'); } else if (fl > 0) center = [Number(a[fl + 1]), Number(a[fl + 2])];
    else throw new SErr('ERR exactly one of FROMMEMBER or FROMLONLAT can be specified for GEOSEARCH');
    if (br > 0) { unit = UNIT[a[br + 2].toLowerCase()]; shape = {t: 'radius', r: Number(a[br + 1]) * unit}; } else if (bb > 0) { unit = UNIT[a[bb + 3].toLowerCase()]; shape = {t: 'box', w: Number(a[bb + 1]) * unit, h: Number(a[bb + 2]) * unit}; }
    else throw new SErr('ERR exactly one of BYRADIUS and BYBOX can be specified for GEOSEARCH');
    if (!unit) throw new SErr('ERR unsupported unit provided. please use M, KM, FT, MI');
    return geoQuery(s, center, shape, Object.assign(o, {unit}));
  },
};
function execCmd(cx, args) {
  const f = EX[args[0]];
  if (!f) throw new SErr(`ERR unknown command '${args[0]}'`);
  return f(cx, args.slice(1));
}

/* 受限执行（RunUser）：先做命令登记的探测（元素个数 / 字符串长度 / 负下标换算），再执行 */
function runUser(inst, dbn, cmd) {
  const cx = {inst, db: +dbn};
  const res = {args: cmd.args.slice(), notices: cmd.notices.slice(), indexBase: cmd.indexBase, reply: null};
  for (const pr of cmd.probes) {
    if (pr.kind === 'card' || pr.kind === 'strlen') {
      let total = 0;
      for (const k of pr.keys) total += asInt(execCmd(cx, [pr.lenCmd, k]));
      let limit = FULL_MAX, what = '元素';
      if (pr.kind === 'strlen') { limit = BITCOUNT_MAX; what = '字节'; }
      if (total > limit) throw new UErr(`key 太大：共 ${total} 个${what}，超过在线查询的全量读取阈值 ${limit}；${pr.hint}`);
    } else {
      const n = asInt(execCmd(cx, [pr.lenCmd, pr.keys[0]]));
      let s = parseInt(res.args[pr.si], 10), e = parseInt(res.args[pr.ei], 10);
      if (s < 0) s += n; if (e < 0) e += n;
      s = Math.max(s, 0); if (e >= n) e = n - 1;
      if (n === 0 || s > e) continue;                    // 空区间，原样发送，服务端返回空
      if (e - s + 1 > RANGE_MAX) {
        e = s + RANGE_MAX - 1;
        res.notices.push(`区间过大（共 ${n} 条），已收窄为 ${s}..${e}（单次最多 ${RANGE_MAX} 条，请分页）`);
      }
      res.args[pr.si] = String(s); res.args[pr.ei] = String(e); res.indexBase = s;
    }
  }
  res.reply = execCmd(cx, res.args);
  return res;
}

/* ============================================================================
   五、结果整形（shapeRedis 移植）：统一成二维表
   ========================================================================== */
function newShape(max) {
  const sh = {columns: [], rows: [], truncated: false, cursor: '', nextCmd: '', binary: 0, cut: 0, max, bytes: 0};
  sh.cell = v => {
    if (v === null || v === undefined) return '(nil)';
    if (typeof v === 'number') return v;
    if (typeof v === 'string') {
      let [s, bin] = redisText(v);
      if (bin) sh.binary++;
      if (bin ? s.length > CELL_MAX : v.length > CELL_MAX) {       // 单个值显示 ≤ 64KB（按字节），超出截断并标明总字节数
        sh.cut++;
        if (bin) s = s.slice(0, CELL_MAX);
        else { let cut = CELL_MAX; while (cut > 0 && (v.charCodeAt(cut) & 0xc0) === 0x80) cut--; s = redisText(v.slice(0, cut))[0]; }
        s += `…（已截断，共 ${v.length} 字节）`;
      }
      return s;
    }
    if (Array.isArray(v)) return '[' + v.map(x => String(sh.cell(x))).join(', ') + ']';
    return String(v);
  };
  /* 追加一行，返回 false 表示已达行数 / 字节上限（此行及之后的都被丢弃） */
  sh.add = (...cells) => {
    if (sh.truncated) return false;
    if (sh.max > 0 && sh.rows.length >= sh.max) { sh.truncated = true; return false; }
    const row = cells.map(sh.cell);
    const n = row.reduce((a, x) => a + (typeof x === 'string' ? x.length : 8), 0);
    if (sh.bytes + n > REPLY_MAX) { sh.truncated = true; return false; }
    sh.bytes += n; sh.rows.push(row); return true;
  };
  return sh;
}
function shapeRedis(cmd, ur, maxRows) {
  const sh = newShape(maxRows), arr = ur.reply;
  if (!Array.isArray(arr)) { sh.columns = ['value']; sh.add(arr); return sh; }
  const list = (col, base) => { sh.columns = ['index', col]; for (let i = 0; i < arr.length; i++) if (!sh.add(base + i, arr[i])) return; };
  const pairs = (a, b, items) => { sh.columns = [a, b]; for (let i = 0; i < items.length; i += 2) if (!sh.add(items[i], i + 1 < items.length ? items[i + 1] : null)) return; };
  const keyed = (a, b, keys) => { sh.columns = [a, b]; for (let i = 0; i < arr.length; i++) if (!sh.add(i < keys.length ? keys[i] : '', arr[i])) return; };
  const args = cmd.args, tail = n => args.length > n ? args.slice(n) : [];
  switch (cmd.name) {
    case 'SCAN': case 'SSCAN': case 'HSCAN': case 'ZSCAN': {
      let items = [];
      if (arr.length === 2) {
        sh.cursor = String(sh.cell(arr[0])); items = Array.isArray(arr[1]) ? arr[1] : [];
        const ci = cmd.name === 'SCAN' ? 1 : 2;
        if (sh.cursor !== '0' && ci < ur.args.length) { const next = ur.args.slice(); next[ci] = sh.cursor; sh.nextCmd = formatUserCommand(next); }
      }
      if (cmd.name === 'HSCAN' && !cmd.flags.NOVALUES) {
        sh.columns = ['field', 'value']; sh.add('(cursor)', sh.cursor);
        for (let i = 0; i < items.length; i += 2) if (!sh.add(items[i], i + 1 < items.length ? items[i + 1] : null)) break;
      } else if (cmd.name === 'ZSCAN') {
        sh.columns = ['member', 'score']; sh.add('(cursor)', sh.cursor);
        for (let i = 0; i < items.length; i += 2) if (!sh.add(items[i], i + 1 < items.length ? items[i + 1] : null)) break;
      } else {
        sh.columns = ['index', {SCAN: 'key', SSCAN: 'member', HSCAN: 'field'}[cmd.name]]; sh.add('(cursor)', sh.cursor);
        for (let i = 0; i < items.length; i++) if (!sh.add(i + 1, items[i])) break;
      }
      break;
    }
    case 'HGETALL': pairs('field', 'value', arr); break;
    case 'HRANDFIELD': if (cmd.flags.WITHVALUES) pairs('field', 'value', arr); else list('field', 0); break;
    case 'ZRANGE': case 'ZREVRANGE': case 'ZRANGEBYSCORE': case 'ZREVRANGEBYSCORE':
      if (cmd.flags.WITHSCORES) pairs('member', 'score', arr); else list('member', ur.indexBase); break;
    case 'ZRANGEBYLEX': list('member', 0); break;
    case 'MGET': keyed('key', 'value', tail(1)); break;
    case 'HMGET': keyed('field', 'value', tail(2)); break;
    case 'SMISMEMBER': keyed('member', 'is_member', tail(2)); break;
    case 'ZMSCORE': keyed('member', 'score', tail(2)); break;
    case 'GEOPOS':
      sh.columns = ['member', 'longitude', 'latitude'];
      for (let i = 0; i < arr.length; i++) { const pos = arr[i]; if (!sh.add(i + 2 < args.length ? args[i + 2] : '', Array.isArray(pos) && pos.length === 2 ? pos[0] : null, Array.isArray(pos) && pos.length === 2 ? pos[1] : null)) break; }
      break;
    case 'GEORADIUS_RO': case 'GEOSEARCH': shapeGeo(sh, cmd, arr); break;
    case 'XRANGE': case 'XREVRANGE':
      sh.columns = ['id', 'field', 'value'];
      stream: for (const ent of arr) {
        if (!Array.isArray(ent) || ent.length < 2) continue;
        const fv = Array.isArray(ent[1]) ? ent[1] : [];
        if (!fv.length) { if (!sh.add(ent[0], null, null)) break stream; continue; }
        for (let i = 0; i < fv.length; i += 2) if (!sh.add(ent[0], fv[i], i + 1 < fv.length ? fv[i + 1] : null)) break stream;
      }
      break;
    case 'LRANGE': list('value', ur.indexBase); break;
    case 'SMEMBERS': case 'SINTER': case 'SUNION': case 'SDIFF': case 'SRANDMEMBER': list('member', 0); break;
    case 'HKEYS': list('field', 0); break;
    default: list('value', 0);
  }
  return sh;
}
function shapeGeo(sh, cmd, arr) {
  const f = cmd.flags, wd = f.WITHDIST, wh = f.WITHHASH, wc = f.WITHCOORD;
  if (!wd && !wh && !wc) { sh.columns = ['index', 'member']; for (let i = 0; i < arr.length; i++) if (!sh.add(i, arr[i])) return; return; }
  sh.columns = ['member'];
  if (wd) sh.columns.push('distance');
  if (wh) sh.columns.push('hash');
  if (wc) sh.columns.push('longitude', 'latitude');
  for (const ent of arr) {
    if (!Array.isArray(ent) || !ent.length) continue;
    const row = [ent[0]]; let i = 1;
    if (wd && i < ent.length) row.push(ent[i++]);
    if (wh && i < ent.length) row.push(ent[i++]);
    if (wc) { let lon = null, lat = null; if (i < ent.length && Array.isArray(ent[i]) && ent[i].length === 2) { lon = ent[i][0]; lat = ent[i][1]; } row.push(lon, lat); }
    if (!sh.add(...row)) return;
  }
}

/* 执行一条已通过静态检查的命令并整形：返回 {ok, columns, rows, truncated, cursor, nextCmd, executed, notices, ms} 或 {err:{code,msg}} */
function run(inst, dbn, cmd, maxRows) {
  let ur;
  try { ur = runUser(inst, dbn, cmd); }
  catch (e) {
    if (e instanceof UErr) return {err: {code: 'bad_query', msg: e.message}};
    if (e instanceof SErr) return {err: {code: 'engine_error', msg: e.message}};
    throw e;
  }
  const sh = shapeRedis(cmd, ur, maxRows), notices = ur.notices.slice();
  if (sh.binary > 0) notices.push(`${sh.binary} 个值含不可见字符或非 UTF-8 字节，已按 \\xHH 转义显示。`);
  if (sh.cut > 0) notices.push(`${sh.cut} 个值超过 ${CELL_MAX >> 10} KB，已截断显示。`);
  if (sh.truncated) notices.push('结果已按行数 / 回复大小上限截断；可缩小范围，或用 SCAN 系列命令分批读取。');
  const executed = formatUserCommand(ur.args);
  const ms = Math.max(1, 1 + (h32(executed) % 4) + Math.floor(sh.rows.length / 300) + (cmd.name === 'SCAN' ? 1 : 0) + (cmd.probes.length ? 1 : 0));
  return {ok: true, columns: sh.columns, rows: sh.rows, truncated: sh.truncated, cursor: sh.cursor, nextCmd: sh.nextCmd, executed, notices, ms};
}

/* ============================================================================
   六、key 浏览（BrowseKeys 移植）与点 key 生成读取命令（redis_keys.js 的 commandFor / toMatch）
   ========================================================================== */
function commandFor(type, arg) {
  switch (type) {
    case 'string': return `GET ${arg}`;
    case 'hash': return `HGETALL ${arg}`;
    case 'list': return `LRANGE ${arg} 0 99`;
    case 'set': return `SMEMBERS ${arg}`;
    case 'zset': return `ZRANGE ${arg} 0 99 WITHSCORES`;
    case 'stream': return `XRANGE ${arg} - + COUNT 100`;
    default: return `TYPE ${arg}`;
  }
}
/* 输入没有通配符时按前缀匹配（自动补 *）；空输入匹配全部 */
function toMatch(input) {
  const t = String(input || '').trim();
  if (!t) return '*';
  return /[*?[]/.test(t) ? t : t + '*';
}
/* 用 SCAN 从 cursor 起收集至少 want 个匹配 match 的 key（每轮 COUNT ≤ 500、最多 60 轮），再取类型 */
function browse(inst, dbn, match, cursor, want) {
  want = Math.min(want > 0 ? want : 100, 500);
  match = match || '*';
  if (match.length > 512) return {err: '匹配模式过长（最多 512 字节）'};
  cursor = cursor || '0';
  if (!/^\d+$/.test(cursor)) return {err: '游标必须是非负整数'};
  const cx = {inst, db: +dbn}, keys = [];
  let cur = cursor;
  for (let round = 0; round < 60 && keys.length < want; round++) {
    const args = ['SCAN', cur, 'COUNT', String(Math.min(500, Math.max(want, 50)))];
    if (match !== '*') args.push('MATCH', toBin(match));
    const [next, ks] = EX.SCAN(cx, args.slice(1));
    cur = next; for (const k of ks) keys.push(k);
    if (cur === '0') break;
  }
  const out = keys.map(k => {
    const [text, bin] = redisText(k), sp = lookup(inst, +dbn, k);
    return {key: text, arg: quoteCLIArg(k), type: sp ? sp.type : '', binary: bin};
  });
  return {keys: out, cursor: cur, done: cur === '0'};
}

/* ============================================================================
   七、页面片段（供 p_query.js 调用，纯 HTML 字符串）
   ========================================================================== */
const ED_PLACEHOLDER = '在这里写一条 Redis 命令';
/* key 浏览列表：st = {db, match, keys, cursor, done, loading, error:{msg,link}|null} */
function keysHtml(st) {
  let h = `<div class="rk-head">⛁ db${esc(String(st.db).replace(/^db/i, ''))}<span class="n">匹配 ${esc(st.match)} · ${st.keys.length} 个 key</span></div>`;
  if (st.error) h += `<div class="rk-err">${esc(st.error.msg)}${st.error.link ? `<div style="margin-top:6px"><a class="btn sm primary" href="${esc(st.error.link.href)}" style="text-decoration:none">${esc(st.error.link.text)} →</a></div>` : ''}</div>`;
  if (!st.error && !st.loading && !st.keys.length) h += `<div class="rk-hint">${st.done ? '没有匹配的 key（或该库为空）' : '这一批没有匹配的 key'}</div>`;
  st.keys.forEach((k, i) => {
    h += `<div class="tn l1 rk-row" data-on="rkPick" data-i="${i}" title="${esc(commandFor(k.type, k.arg) + (k.binary ? '\n含不可见字符，已按 \\xHH 显示' : ''))}"><span class="rk-type rk-${esc(k.type || 'none')}">${esc(k.type || '?')}</span><span class="rk-key">${esc(k.key)}</span></div>`;
  });
  if (st.loading) h += '<div class="rk-hint">加载中…</div>';
  if (!st.loading && !st.done && !st.error) h += '<button class="btn sm rk-more" data-on="rkMore">加载更多</button>';
  if (!st.loading && st.done && st.keys.length && !st.error) h += '<div class="rk-hint">已扫描完整个库</div>';
  return h;
}
/* 结果表：r = {cols:[{n}], rows, n, ms, instName, db, cursor, nextCmd, executed, rtabsHtml} */
function resultHtml(r, max) {
  const cell = (v, ci) => v == null ? '<span class="pq-null">NULL</span>' : typeof v === 'number' ? esc(v) : String(v).length > 160 ? `<span class="rq-long" title="${esc(String(v).slice(0, 400))}">${esc(v)}</span>` : esc(v);
  const shown = r.rows.slice(0, max);
  return `<div class="row" style="margin-bottom:6px"><b>${Number(r.n).toLocaleString('en-US')} 行</b><span class="muted">· 耗时 ${r.ms} ms</span><span class="muted small">· ${esc(r.instName)} / ${esc(dbTxt(r.db))}</span>
      <span class="pq-chip warn">Redis 结果不做脱敏</span>
      ${r.nextCmd ? `<button class="btn sm" data-on="qryRNext" title="${esc(r.nextCmd)}">下一页（游标 ${esc(r.cursor)}）→</button>` : ''}<span class="sp"></span>
      <button class="btn sm" data-on="qryCopyRes">复制结果</button></div>
    <div class="pq-grid"><table><thead><tr><th class="rn">#</th>${r.cols.map(c => `<th>${esc(c.n)}</th>`).join('')}</tr></thead>
      <tbody>${shown.map((row, i) => `<tr><td class="rn">${i + 1}</td>${row.map((v, k) => `<td class="${typeof v === 'number' ? 'n' : ''}">${cell(v, k)}</td>`).join('')}</tr>`).join('') || `<tr><td colspan="${r.cols.length + 1}">${empty('命令执行成功，没有返回数据')}</td></tr>`}</tbody></table></div>
    ${r.rows.length > max ? `<div class="small muted" style="margin-top:6px">为保证页面流畅，原型只渲染前 ${max} 行（共 ${r.rows.length.toLocaleString('en-US')} 行）。<a data-on="qryShowAll">显示全部</a></div>` : ''}
    <details class="pq-d" style="margin-top:10px"><summary>实际执行的命令</summary><div><div class="pq-sqlv">${esc(r.executed)}</div></div></details>`;
}

/* ============================================================================
   八、示例命令（含各类型的例子、限量演示和会被拦截的反例）
   ========================================================================== */
function samples(inst, db) {
  const L = [], add = (g, label, sql) => L.push({g, label, sql}), ds = dsName(inst), dbn = +String(db).replace(/^db/i, '');
  add('浏览 key', 'SCAN：分批列出 key', 'SCAN 0 MATCH * COUNT 100');
  add('浏览 key', 'SCAN：只看某种类型', 'SCAN 0 TYPE hash COUNT 100');
  add('浏览 key', 'DBSIZE：当前库 key 数', 'DBSIZE');
  if (ds === 'order' && dbn === 0) {
    add('浏览 key', 'SCAN：按前缀匹配', 'SCAN 0 MATCH order:detail:* COUNT 100');
    add('字符串', 'GET：今日订单计数', 'GET counter:order:today');
    add('字符串', 'MGET：一次读多个', 'MGET order:status:80123456 order:status:80654321 counter:order:today');
    add('字符串', 'TTL / TYPE / OBJECT ENCODING', 'TTL order:status:80123456');
    add('字符串', 'MEMORY USAGE：占用字节', 'MEMORY USAGE order:detail:80123456');
    add('字符串', 'GET 二进制值（按 \\xHH 显示并提示）', 'GET blob:sign:key');
    add('字符串', 'GET 超长值（超过 64KB 截断显示）', 'GET cache:home:feed');
    add('字符串', 'BITCOUNT：位图统计', 'BITCOUNT bitmap:active:20261007');
    add('字符串', 'PFCOUNT：HyperLogLog 基数', 'PFCOUNT hll:uv:20261007');
    add('哈希', 'HGETALL：订单详情（10 个字段）', 'HGETALL order:detail:80123456');
    add('哈希', 'HGET：读一个字段', 'HGET order:detail:80123456 status');
    add('哈希', 'HSCAN：分页读取大哈希', 'HSCAN big:order:index 0 COUNT 100');
    add('哈希', 'HGETALL 大哈希（超过 5000 个字段会被拒绝）', 'HGETALL big:order:index');
    add('列表', 'LRANGE：订单明细', 'LRANGE order:items:80123456 0 -1');
    add('列表', 'LRANGE：超时队列前 100 个', 'LRANGE order:timeout:queue 0 99');
    add('列表', 'LRANGE：最后 10 个（负下标先探测长度）', 'LRANGE order:timeout:queue -10 -1');
    add('列表', 'LLEN / LINDEX / LPOS', 'LLEN order:timeout:queue');
    add('集合', 'SMEMBERS：小集合', 'SMEMBERS order:paid:20261005');
    add('集合', 'SMEMBERS 大集合（超过 5000 个成员会被拒绝）', 'SMEMBERS order:paid:20261007');
    add('集合', 'SSCAN：分页读取', 'SSCAN order:paid:20261007 0 MATCH 800* COUNT 100');
    add('集合', 'SISMEMBER', 'SISMEMBER order:paid:20261007 80000007');
    add('有序集合', 'ZREVRANGE：销量最高的 10 个（带分数）', 'ZREVRANGE hot:sku:rank:20261007 0 9 WITHSCORES');
    add('有序集合', 'ZRANGEBYSCORE：按分数区间', 'ZRANGEBYSCORE hot:sku:rank:20261007 3900 +inf WITHSCORES LIMIT 0 20');
    add('有序集合', 'ZSCORE / ZRANK / ZCARD', 'ZSCORE hot:sku:rank:20261007 sku:103000');
    add('Stream', 'XLEN / XRANGE', 'XRANGE stream:order:events - + COUNT 20');
    add('Stream', 'XREVRANGE：最新 10 条', 'XREVRANGE stream:order:events + - COUNT 10');
    add('地理', 'GEOPOS：门店经纬度', 'GEOPOS geo:stores sh-pudong bj-chaoyang');
    add('地理', 'GEODIST：两店距离（km）', 'GEODIST geo:stores sh-pudong bj-chaoyang km');
    add('地理', 'GEOSEARCH：上海周边 30km 的门店', 'GEOSEARCH geo:stores FROMLONLAT 121.47 31.23 BYRADIUS 30 km ASC WITHDIST COUNT 10');
    add('规则演示', 'COUNT 超过上限（自动改为 1000）', 'SCAN 0 COUNT 100000');
    add('规则演示', 'LRANGE 区间过大（自动收窄到 1000 条）', 'LRANGE order:timeout:queue 0 99999');
    add('规则演示', 'LRANGE 负下标区间过大（探测后收窄）', 'LRANGE order:timeout:queue -100000 -1');
    add('规则演示', 'ZRANGEBYSCORE 没带 LIMIT（自动补 LIMIT 0 1000）', 'ZRANGEBYSCORE hot:sku:rank:20261007 -inf +inf');
    add('规则演示', 'XRANGE 没带 COUNT（自动补 1000）', 'XRANGE stream:order:events - +');
    add('规则演示', 'BITCOUNT 不带区间且字符串超过 16MB（被拒绝）', 'BITCOUNT bitmap:active:2026');
    add('规则演示', '服务端错误回复（WRONGTYPE）', 'HGETALL counter:order:today');
  } else if (ds === 'session' && dbn === 0) {
    add('浏览 key', 'SCAN：按前缀匹配', 'SCAN 0 MATCH session:* COUNT 100');
    add('字符串', 'GET：会话（JSON）', 'GET session:3f2a9c4d5b6e7f801a2b3c4d5e6f7081');
    add('字符串', 'TTL：会话剩余秒数', 'TTL session:3f2a9c4d5b6e7f801a2b3c4d5e6f7081');
    add('字符串', 'GET：登录失败次数', 'GET login:fail:10.20.8.14');
    add('哈希', 'HGETALL：会话数据（含手机号 / 邮箱，不脱敏）', 'HGETALL session:data:3f2a9c4d5b6e7f801a2b3c4d5e6f7081');
    add('哈希', 'HGETALL：会话策略', 'HGETALL config:session:policy');
    add('哈希', 'HGETALL：今日按小时登录数', 'HGETALL stat:login:20261007');
    add('哈希', 'HSCAN：分页读取 uid 索引（大哈希）', 'HSCAN sess:index:uid 0 COUNT 100');
    add('哈希', 'HGETALL 大哈希（超过 5000 个字段会被拒绝）', 'HGETALL sess:index:uid');
    add('列表', 'LRANGE：用户设备', 'LRANGE user:devices:10086 0 -1');
    add('集合', 'SSCAN：在线用户', 'SSCAN user:online 0 COUNT 100');
    add('集合', 'SMEMBERS 大集合（超过 5000 个成员会被拒绝）', 'SMEMBERS user:online');
    add('集合', 'SCARD / SISMEMBER', 'SCARD user:online');
    add('有序集合', 'ZRANGE：令牌黑名单前 20 个', 'ZRANGE token:blacklist 0 19 WITHSCORES');
    add('有序集合', 'ZCOUNT：未来 1 小时内到期的数量', `ZCOUNT token:blacklist ${NOW + 300} ${NOW + 3600}`);
    add('地理', 'GEOPOS：最近登录城市', 'GEOPOS login:geo:recent sh-pudong hz-xihu');
    add('规则演示', 'COUNT 超过上限（自动改为 1000）', 'SCAN 0 COUNT 100000');
    add('规则演示', 'ZRANGE 区间过大（自动收窄到 1000 条）', 'ZRANGE token:blacklist 0 99999 WITHSCORES');
    add('规则演示', '服务端错误回复（WRONGTYPE）', 'LRANGE config:session:policy 0 -1');
  } else if (ds === 'session' && dbn === 1) {
    add('浏览 key', 'SCAN：用户资料', 'SCAN 0 MATCH user:profile:* COUNT 100');
    add('哈希', 'HGETALL：用户资料（手机号 / 邮箱明文，不脱敏）', 'HGETALL user:profile:10086');
    add('哈希', 'HGETALL：限流配置', 'HGETALL config:ratelimit');
    add('字符串', 'GET：接口限流计数', 'GET ratelimit:api:10.20.8.14');
  } else if (ds === 'order') {
    const fx = (DATASETS.order[dbn] || {}).fixedNames || [];
    if (fx[0]) add('字符串 / 其他', 'TYPE：第一个固定 key', 'TYPE ' + fx[0]);
    if (dbn === 2) { add('列表', 'LRANGE：待处理任务', 'LRANGE task:queue:pending 0 19'); add('列表', 'LLEN：失败任务数', 'LLEN task:queue:failed'); }
    if (dbn === 1) add('哈希', 'HGETALL：今日分时 GMV', 'HGETALL stat:gmv:20261007');
    if (dbn === 5) add('字符串', 'GET：功能开关', 'GET feature:new_checkout');
  }
  const sess = ds === 'session', K = sess ? {s: 'login:fail:10.20.8.14', d: 'user:devices:10086', l: 'user:devices:10086'} : {s: 'counter:order:today', d: 'order:status:80123456', l: 'order:timeout:queue'};
  add('会被拦截', 'SET（写命令）', `SET ${K.s} 0`);
  add('会被拦截', 'DEL（写命令）', `DEL ${K.d}`);
  add('会被拦截', 'KEYS *（会阻塞 Redis）', 'KEYS *');
  add('会被拦截', 'FLUSHDB', 'FLUSHDB');
  add('会被拦截', 'CONFIG GET（运维命令）', 'CONFIG GET *');
  add('会被拦截', 'INFO（请用实例巡检）', 'INFO');
  add('会被拦截', 'EVAL（脚本）', 'EVAL "return 1" 0');
  add('会被拦截', 'SELECT（库在页面上选）', 'SELECT 1');
  add('会被拦截', 'BLPOP（阻塞命令）', `BLPOP ${K.l} 0`);
  add('会被拦截', 'SORT（可能带 STORE）', `SORT ${K.l}`);
  add('会被拦截', '不在白名单（OBJECT HELP）', 'OBJECT HELP');
  add('会被拦截', '参数里带 AUTH（疑似携带密码）', 'GET AUTH');
  add('会被拦截', '一次执行多条命令（末尾留一个空行，光标在空行时按全部内容检查）', `GET ${K.s}\nGET ${K.d}\n`);
  add('会被拦截', '引号没有闭合', `GET "${K.s}`);
  add('会被拦截', 'LRANGE 下标不是整数', `LRANGE ${K.l} 0 abc`);
  return L;
}

window.RedisQuery = {
  isRedis: inst => !!inst && inst.type === 'Redis',
  dbs, dbLabel, dbTxt, sameDb, check, run, browse, commandFor, toMatch, samples, keysHtml, resultHtml,
  editorPlaceholder: ED_PLACEHOLDER, commandNames: COMMAND_NAMES,
  /* 测试 / 调试用 */
  _tokenize: s => tokenizeLine(toBin(s)).map(x => redisText(x)[0]),
};
})();
