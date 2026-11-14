/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_redisrule.js
 * @time: 2026/10/28
 */
/* ============================================================================
   Redis 上线规则（pages.redisrule，仅超级用户）+ 规则引擎（window.RedisRules，供「SQL 上线」的 Redis 命令模式调用）
   规格：internal/redisrule/{rule,eval,glob,templates}.go、internal/engine/redis/cmdkeys.go
   - 白名单语义：命令必须同时命中某条「启用」规则的 命令名 / 键模式 / 适用范围 / 键数上限，否则拒绝；预置 0 条规则 = 全部拒绝。
   - 硬禁止命令与无键 / 读命令不能写进规则（选了即时报错），提交检查与执行时命中也直接拒绝。
   - 规则的新增 / 修改 / 启停 / 删除都要二次确认并写系统审计（修改只记变化字段「旧 → 新」）。
   - 导出：window.RedisRules = {rules（实时数组）, splitLines, evalLine, evaluate, tokenize, redact, escapeBinary, simExec, simSnapshot, demoNames, MAX_LINES}
   - 原型简化：键 / 模式按 JS 字符（而非字节）匹配；\xHH 转义按单个字符处理；规则表永远“已创建”。
   ========================================================================== */
(function () {
  'use strict';

  /* ============================================================================
     命令静态知识（internal/engine/redis/cmdkeys.go）
     ========================================================================== */
  const HARD = new Set(('FLUSHALL FLUSHDB SHUTDOWN CONFIG DEBUG KEYS SCRIPT EVAL EVALSHA EVAL_RO EVALSHA_RO FCALL FCALL_RO FUNCTION MODULE SLAVEOF REPLICAOF ACL CLIENT MONITOR '
    + 'SAVE BGSAVE BGREWRITEAOF MIGRATE SWAPDB SELECT AUTH WAIT WAITAOF FAILOVER PSYNC SYNC SUBSCRIBE PSUBSCRIBE SSUBSCRIBE UNSUBSCRIBE PUNSUBSCRIBE SUNSUBSCRIBE '
    + 'MULTI EXEC DISCARD WATCH UNWATCH RESET HELLO QUIT MOVE BLPOP BRPOP BLMOVE BRPOPLPUSH BZPOPMIN BZPOPMAX BLMPOP BZMPOP XREAD XREADGROUP').split(' '));
  const HARD_LIST = [...HARD, 'SORT … STORE'].sort();
  const isHardArgs = args => {
    if (!args.length) return false;
    const n = args[0].toUpperCase();
    if (HARD.has(n)) return true;
    return (n === 'SORT' || n === 'SORT_RO') && args.slice(1).some(a => a.toUpperCase() === 'STORE');
  };
  const isHardName = n => HARD.has(String(n).trim().toUpperCase());

  const CMDS = {};
  const reg = (name, min, max, first, last, step, multi, v) => { CMDS[name] = {name, min, max, first, last, step, multi, validate: v || null}; };
  const one = (name, min, max, v) => reg(name, min, max, 1, 1, 1, false, v);
  const isInt = s => /^[+-]?\d+$/.test(s);
  const intArg = what => a => isInt(a[2]) ? '' : `${what} 需要整数`;
  const even = from => a => (a.length - from) % 2 !== 0 ? `${a[0].toUpperCase()} 的参数个数不对（需要成对的参数）` : '';
  function validateSet(a) {
    let ttl = false;
    for (let i = 3; i < a.length; i++) {
      const o = a[i].toUpperCase();
      if (o === 'NX' || o === 'XX' || o === 'GET') continue;
      if (o === 'KEEPTTL') { if (ttl) return 'SET 的过期选项只能出现一个'; ttl = true; continue; }
      if (o === 'EX' || o === 'PX' || o === 'EXAT' || o === 'PXAT') {
        if (ttl) return 'SET 的过期选项只能出现一个';
        ttl = true;
        if (i + 1 >= a.length) return `SET ${o} 缺少数值`;
        if (!isInt(a[i + 1]) || Number(a[i + 1]) <= 0) return `SET ${o} 需要正整数`;
        i++; continue;
      }
      return `SET 不支持的选项 ${JSON.stringify(a[i])}`;
    }
    return '';
  }
  const setHasTTL = a => a.slice(3).some(x => ['EX', 'PX', 'EXAT', 'PXAT', 'KEEPTTL'].includes(x.toUpperCase()));
  function validateCopy(a) {
    for (const x of a.slice(3)) {
      const u = x.toUpperCase();
      if (u === 'DB') return 'COPY 不支持 DB 选项（不能跨库复制）';
      if (u === 'REPLACE') continue;
      if (!/^[+-]?\d+$/.test(x)) return `COPY 不支持的选项 ${JSON.stringify(x)}`;
    }
    return '';
  }
  /* 字符串 / 位 */
  one('SET', 3, -1, validateSet); one('SETEX', 4, 4, intArg('SETEX 的过期秒数')); one('PSETEX', 4, 4, intArg('PSETEX 的过期毫秒数'));
  ['SETNX', 'GETSET', 'APPEND', 'INCRBY', 'DECRBY', 'INCRBYFLOAT'].forEach(n => one(n, 3, 3));
  ['GETDEL', 'INCR', 'DECR'].forEach(n => one(n, 2, 2));
  one('GETEX', 2, -1); one('SETRANGE', 4, 4); one('SETBIT', 4, 4); one('PFADD', 2, -1);
  /* 过期 */
  one('EXPIRE', 3, 4, intArg('EXPIRE 的秒数')); one('PEXPIRE', 3, 4, intArg('PEXPIRE 的毫秒数')); one('EXPIREAT', 3, 4, intArg('EXPIREAT 的时间戳')); one('PEXPIREAT', 3, 4, intArg('PEXPIREAT 的时间戳')); one('PERSIST', 2, 2);
  /* 哈希 */
  one('HSET', 4, -1, even(2)); one('HMSET', 4, -1, even(2)); one('HSETNX', 4, 4); one('HDEL', 3, -1); one('HINCRBY', 4, 4); one('HINCRBYFLOAT', 4, 4);
  /* 列表 */
  ['LPUSH', 'RPUSH', 'LPUSHX', 'RPUSHX'].forEach(n => one(n, 3, -1));
  one('LPOP', 2, 3); one('RPOP', 2, 3); one('LSET', 4, 4); one('LREM', 4, 4); one('LTRIM', 4, 4); one('LINSERT', 5, 5);
  /* 集合 / 有序集合 / 流 */
  one('SADD', 3, -1); one('SREM', 3, -1); one('SPOP', 2, 3);
  one('ZADD', 4, -1); one('ZREM', 3, -1); one('ZINCRBY', 4, 4); one('ZREMRANGEBYRANK', 4, 4); one('ZREMRANGEBYSCORE', 4, 4); one('ZREMRANGEBYLEX', 4, 4); one('ZPOPMIN', 2, 3); one('ZPOPMAX', 2, 3);
  one('XADD', 5, -1); one('XDEL', 3, -1); one('XTRIM', 4, -1);
  /* 多键 / 双键 */
  ['DEL', 'UNLINK', 'TOUCH'].forEach(n => reg(n, 2, -1, 1, -1, 1, true));
  reg('MSET', 3, -1, 1, -1, 2, true, even(1)); reg('MSETNX', 3, -1, 1, -1, 2, true, even(1));
  reg('PFMERGE', 2, -1, 1, -1, 1, true); ['SINTERSTORE', 'SUNIONSTORE', 'SDIFFSTORE'].forEach(n => reg(n, 3, -1, 1, -1, 1, true));
  reg('RENAME', 3, 3, 1, 2, 1, true); reg('RENAMENX', 3, 3, 1, 2, 1, true); reg('COPY', 3, -1, 1, 2, 1, true, validateCopy);
  reg('SMOVE', 4, 4, 1, 2, 1, true); reg('LMOVE', 5, 5, 1, 2, 1, true); reg('RPOPLPUSH', 3, 3, 1, 2, 1, true);
  reg('BITOP', 4, -1, 2, -1, 1, true, a => ['AND', 'OR', 'XOR', 'NOT'].includes(a[1].toUpperCase()) ? '' : 'BITOP 的操作必须是 AND / OR / XOR / NOT');
  const SUPPORTED = Object.keys(CMDS).sort();
  const MAX_COMMAND_BYTES = 1 << 20;
  const byteLen = s => new TextEncoder().encode(s).length;

  /* 取出命令里的键参数 */
  function keysOf(info, args) {
    const last = info.last < 0 ? args.length + info.last : info.last, step = Math.max(1, info.step), out = [];
    for (let i = info.first; i <= last && i < args.length; i += step) out.push(args[i]);
    return out;
  }
  /* 静态检查（不涉及规则）：返回 {p: {args, name, keys}} 或 {err: {kind: 'hard'|'unsupported'|'syntax', cmd, msg}} */
  function parseCommand(args) {
    if (!args.length) return {err: {kind: 'syntax', msg: '命令为空'}};
    const total = args.reduce((s, a) => s + byteLen(a), 0);
    if (total > MAX_COMMAND_BYTES) return {err: {kind: 'syntax', msg: `命令过大（${total} 字节，上限 ${MAX_COMMAND_BYTES}）`}};
    const name = args[0].toUpperCase();
    if (isHardArgs(args)) return {err: {kind: 'hard', cmd: name}};
    const info = CMDS[name];
    if (!info) return {err: {kind: 'unsupported', cmd: name}};
    if (args.length < info.min || (info.max >= 0 && args.length > info.max)) return {err: {kind: 'syntax', msg: `命令 ${name} 的参数个数不对`}};
    if (info.validate) { const m = info.validate(args); if (m) return {err: {kind: 'syntax', msg: m}}; }
    const keys = keysOf(info, args);
    if (!keys.length) return {err: {kind: 'unsupported', cmd: name}};
    return {p: {args, name, keys, multi: info.multi}};
  }

  /* 分词：同 redis-cli（sdssplitargs）。出错抛 Error。 */
  function tokenize(line) {
    const out = [], n = line.length, isSp = c => c === ' ' || c === '\n' || c === '\r' || c === '\t' || c === '\0', isHex = c => /^[0-9a-fA-F]$/.test(c || '');
    let i = 0;
    for (;;) {
      while (i < n && isSp(line[i])) i++;
      if (i >= n) return out;
      let cur = '', inD = false, inS = false, done = false;
      while (!done) {
        if (inD) {
          if (i >= n) throw new Error('双引号没有闭合');
          if (line[i] === '\\' && i + 3 < n && line[i + 1] === 'x' && isHex(line[i + 2]) && isHex(line[i + 3])) { cur += String.fromCharCode(parseInt(line.slice(i + 2, i + 4), 16)); i += 3; }
          else if (line[i] === '\\' && i + 1 < n) { i++; cur += ({n: '\n', r: '\r', t: '\t', b: '\b', a: '\x07'})[line[i]] || line[i]; }
          else if (line[i] === '"') { if (i + 1 < n && !isSp(line[i + 1])) throw new Error('右引号后面必须是空白或行尾'); done = true; }
          else cur += line[i];
        } else if (inS) {
          if (i >= n) throw new Error('单引号没有闭合');
          if (line[i] === '\\' && i + 1 < n && line[i + 1] === "'") { i++; cur += "'"; }
          else if (line[i] === "'") { if (i + 1 < n && !isSp(line[i + 1])) throw new Error('右引号后面必须是空白或行尾'); done = true; }
          else cur += line[i];
        } else {
          if (i >= n) done = true;
          else if (isSp(line[i])) done = true;
          else if (line[i] === '"') inD = true;
          else if (line[i] === "'") inS = true;
          else cur += line[i];
        }
        if (i < n) i++;
      }
      out.push(cur);
    }
  }
  /* 二进制安全转义：双引号包裹，\n \r \t 与不可见字符转义 */
  function escapeBinary(s) {
    let o = '"';
    for (const ch of String(s)) {
      const c = ch.codePointAt(0);
      if (ch === '"' || ch === '\\') o += '\\' + ch;
      else if (ch === '\n') o += '\\n'; else if (ch === '\r') o += '\\r'; else if (ch === '\t') o += '\\t';
      else if (c < 0x20 || c === 0x7f) o += '\\x' + c.toString(16).padStart(2, '0');
      else o += ch;
    }
    return o + '"';
  }

  /* ============================================================================
     glob（Redis 风格：* ? [abc] [^a] [a-z] \转义）
     ========================================================================== */
  function compileGlob(p) {
    const out = [];
    for (let i = 0; i < p.length; i++) {
      const c = p[i];
      if (c === '*') { if (out.length && out[out.length - 1].k === '*') continue; out.push({k: '*'}); }
      else if (c === '?') out.push({k: '?'});
      else if (c === '\\') { if (i + 1 < p.length) i++; out.push({k: 'l', b: p[i]}); }
      else if (c === '[') {
        const t = {k: '[', neg: false, items: []}; i++;
        if (i < p.length && p[i] === '^') { t.neg = true; i++; }
        let closed = false;
        for (; i < p.length; i++) {
          if (p[i] === ']') { closed = true; break; }
          let lo = p[i];
          if (lo === '\\' && i + 1 < p.length) { i++; lo = p[i]; }
          let hi = lo;
          if (i + 2 < p.length && p[i + 1] === '-' && p[i + 2] !== ']') { hi = p[i + 2]; i += 2; if (hi === '\\' && i + 1 < p.length) { i++; hi = p[i]; } }
          if (lo > hi) [lo, hi] = [hi, lo];
          t.items.push([lo, hi]);
        }
        if (!closed) throw new Error(`键匹配模式 ${JSON.stringify(p)} 里的 [ 没有对应的 ]`);
        out.push(t);
      } else out.push({k: 'l', b: c});
    }
    return out;
  }
  const tokMatch = (t, c) => t.k === 'l' ? t.b === c : t.k === '?' ? true : t.items.some(([lo, hi]) => c >= lo && c <= hi) !== t.neg;
  function globMatch(pattern, s) {
    let toks; try { toks = compileGlob(pattern); } catch (e) { return false; }
    let ti = 0, si = 0, starT = -1, starS = 0;
    while (si < s.length) {
      if (ti < toks.length && toks[ti].k === '*') { starT = ti; starS = si; ti++; }
      else if (ti < toks.length && tokMatch(toks[ti], s[si])) { ti++; si++; }
      else if (starT >= 0) { starS++; si = starS; ti = starT + 1; }
      else return false;
    }
    while (ti < toks.length && toks[ti].k === '*') ti++;
    return ti === toks.length;
  }
  /* 键匹配模式校验：非空、不过长、方括号闭合、不是纯通配符（至少一个固定字符） */
  function validatePattern(p) {
    if (p === '') throw new Error('键匹配模式不能为空');
    if (byteLen(p) > 200) throw new Error('键匹配模式过长（最多 200 字节）');
    const toks = compileGlob(p);
    if (!toks.some(t => t.k === 'l')) throw new Error(`键匹配模式 ${JSON.stringify(p)} 只含通配符：请至少写一个固定字符（如前缀 cache:*），避免一条规则放行全部键`);
  }

  /* ============================================================================
     规则：校验与规整（rule.go Normalize）、适用范围、检查（eval.go）
     ========================================================================== */
  const LIM = {name: 50, cmds: 40, pats: 20, scope: 50, maxKeys: 10000, defMax: 100, text: 500, rules: 500};
  function checkCommandName(name) {
    if (name.length > 30) throw new Error(`命令名过长：${name.slice(0, 30)}`);
    if (!/^[A-Z0-9_]+$/.test(name)) throw new Error(`命令名只能是字母、数字、下划线：${name}`);
    if (isHardName(name)) throw new Error(`命令 ${name} 属于硬禁止命令，任何规则都无法放行，不能写入规则`);
    if (!CMDS[name]) throw new Error(`命令 ${name} 不在受支持的写命令清单里（无键命令、读命令与未知命令不能写入规则）`);
  }
  /* 返回规整后的规则（不含 id / 创建人等）；不合法抛 Error（面向管理员的提示） */
  function normalize(inr) {
    const r = {...inr}, len = s => [...s].length;
    r.name = (r.name || '').trim(); r.description = (r.description || '').trim(); r.remark = (r.remark || '').trim();
    if (!r.name) throw new Error('请填写规则名称');
    if (len(r.name) > LIM.name) throw new Error(`规则名称过长（最多 ${LIM.name} 个字符）`);
    if (len(r.description) > LIM.text) throw new Error(`说明过长（最多 ${LIM.text} 个字符）`);
    if (len(r.remark) > LIM.text) throw new Error(`备注过长（最多 ${LIM.text} 个字符）`);
    const seen = new Set(), cmds = [];
    (r.commands || []).forEach(c => String(c).split(/[,\s，]+/).filter(Boolean).forEach(part => {
      const n = part.toUpperCase(); if (seen.has(n)) return;
      checkCommandName(n); seen.add(n); cmds.push(n);
    }));
    if (!cmds.length) throw new Error('请至少选择一个命令');
    if (cmds.length > LIM.cmds) throw new Error(`一条规则最多 ${LIM.cmds} 个命令`);
    r.commands = cmds.sort();
    const ps = new Set(), pats = [];
    (r.key_patterns || []).forEach(p => { p = String(p).trim(); if (p === '' || ps.has(p)) return; validatePattern(p); ps.add(p); pats.push(p); });
    if (!pats.length) throw new Error('请至少填写一个键匹配模式（如 cache:*）');
    if (pats.length > LIM.pats) throw new Error(`一条规则最多 ${LIM.pats} 个键匹配模式`);
    r.key_patterns = pats;
    const ids = [...new Set((r.instance_ids || []).map(Number))];
    if (ids.some(x => !x)) throw new Error('实例编号不合法');
    if (ids.length > LIM.scope) throw new Error(`最多限定 ${LIM.scope} 个实例`);
    r.instance_ids = ids.sort((a, b) => a - b);
    const dbs = [...new Set((r.dbs || []).map(Number))];
    dbs.forEach(d => { if (!(d >= 0 && d <= 1023)) throw new Error(`库编号 ${d} 不合法（0 ~ 1023）`); });
    if (dbs.length > LIM.scope) throw new Error(`最多限定 ${LIM.scope} 个库`);
    r.dbs = dbs.sort((a, b) => a - b);
    r.max_keys = Number(r.max_keys) || LIM.defMax;
    if (r.max_keys < 1 || r.max_keys > LIM.maxKeys) throw new Error(`单条命令影响键数上限需在 1 ~ ${LIM.maxKeys} 之间`);
    r.enabled = !!r.enabled; r.record_before = !!r.record_before;
    return r;
  }
  const inScope = (r, instId, db) => (!r.instance_ids.length || r.instance_ids.includes(instId)) && (!r.dbs.length || r.dbs.includes(db));
  const scopeText = r => (r.instance_ids.length ? `${r.instance_ids.length} 个指定实例` : '全部实例') + ' / ' + (r.dbs.length ? r.dbs.map(d => 'db' + d).join('、') : '全部库');

  const reject = (msg, extra) => Object.assign({level: 2, status: '拒绝', message: msg, name: '', keys: [], ruleId: 0, ruleName: '', recordBefore: false, args: []}, extra || {});
  /* 对已分词的命令做检查。t = {instId, db}。返回 verdict：{level 0/1/2, status 通过/警告/拒绝, message, name, keys, ruleId, ruleName, recordBefore, args} */
  function evaluate(rules, t, args) {
    const pr = parseCommand(args);
    if (pr.err) {
      const e = pr.err;
      if (e.kind === 'hard') return reject(`命中硬禁止命令 ${e.cmd}：任何规则都无法放行`);
      if (e.kind === 'unsupported') return reject(`命令 ${e.cmd} 不在受支持的写命令清单里（无键命令、读命令与未知命令不能通过工单执行）`, {name: e.cmd});
      return reject(`语法错误：${e.msg}`);
    }
    const p = pr.p, v = reject('', {name: p.name, keys: p.keys, args});
    const ST_NOCMD = 0, ST_SCOPE = 1, ST_KEYS = 2, ST_COUNT = 3, ST_OK = 4;
    let enabled = 0, bestStage = -1, bestMsg = '';
    for (const r of rules) {
      if (!r.enabled) continue;
      enabled++;
      let stage = ST_NOCMD, msg = '';
      if (!r.commands.includes(p.name)) { /* 规则不含该命令 */ }
      else if (!inScope(r, t.instId, t.db)) { stage = ST_SCOPE; msg = `规则「${r.name}」允许命令 ${p.name}，但不适用于当前实例 / 库（适用范围：${scopeText(r)}）`; }
      else {
        const bad = p.keys.find(k => !r.key_patterns.some(pt => globMatch(pt, k)));
        if (bad !== undefined) { stage = ST_KEYS; msg = `键 ${escapeBinary(bad)} 不匹配规则「${r.name}」允许的键模式（${r.key_patterns.join('、')}）`; }
        else if (p.keys.length > r.max_keys) { stage = ST_COUNT; msg = `命令涉及 ${p.keys.length} 个键，超过规则「${r.name}」的上限 ${r.max_keys}`; }
        else stage = ST_OK;
      }
      if (stage === ST_OK) {
        Object.assign(v, {level: 0, status: '通过', message: '', ruleId: r.id, ruleName: r.name, recordBefore: !!r.record_before});
        warnings(v, p);
        return v;
      }
      if (stage > bestStage) { bestStage = stage; bestMsg = msg; }
    }
    v.level = 2; v.status = '拒绝';
    v.message = enabled === 0 ? '没有任何启用的上线规则（默认全部拒绝），请联系管理员在「Redis 上线规则」里定义'
      : bestStage === ST_NOCMD ? `没有任何启用的规则允许命令 ${p.name}` : bestMsg;
    return v;
  }
  const atoi = s => (/^-?\d+$/.test(s) ? Number(s) : 0);
  function warnings(v, p) {
    const w = [], a = p.args;
    switch (p.name) {
      case 'SET': if (!setHasTTL(a)) w.push('SET 没有设置过期时间（EX / PX / EXAT / PXAT / KEEPTTL），键将永不过期'); break;
      case 'SETNX': case 'GETSET': case 'MSET': case 'MSETNX': w.push(p.name + ' 不能设置过期时间，键将永不过期（需要的话请随后加 EXPIRE）'); break;
      case 'EXPIRE': case 'PEXPIRE': if (atoi(a[2]) <= 0) w.push(p.name + ' 的时间 ≤ 0，键会被立即删除'); break;
      case 'EXPIREAT': case 'PEXPIREAT': if (atoi(a[2]) <= 0) w.push(p.name + ' 的时间戳 ≤ 0，键会被立即删除'); break;
      case 'PERSIST': w.push('PERSIST 会去掉过期时间，键将永不过期'); break;
      case 'RENAME': case 'COPY': w.push(p.name + ' 会覆盖目标键（若已存在）'); break;
      case 'DEL': case 'UNLINK': if (p.keys.length >= 20) w.push(`一次删除 ${p.keys.length} 个键，请确认`); break;
    }
    if (w.length) { v.level = 1; v.status = '警告'; v.message = w.join('；'); }
  }
  function evalLine(rules, t, line) {
    let args;
    try { args = tokenize(line); } catch (e) { return reject('语法错误：' + e.message); }
    if (!args.length) return reject('命令为空');
    return evaluate(rules, t, args);
  }
  /* 按行切分：空行忽略、行号保留；超过 200 条报错。返回 {lines: [{no, text}]} 或 {error} */
  const MAX_LINES = 200;
  function splitLines(text) {
    const lines = [];
    String(text).replace(/\r\n/g, '\n').split('\n').forEach((l, i) => { const t = l.replace(/\r/g, '').trim(); if (t) lines.push({no: i + 1, text: t}); });
    if (!lines.length) return {error: '命令不能为空'};
    if (lines.length > MAX_LINES) return {error: `一次最多提交 ${MAX_LINES} 条命令，当前 ${lines.length} 条`};
    return {lines};
  }

  /* ============================================================================
     执行相关的模拟：回复摘要 / 执行前原值 / 审计里的命令摘要（exec.go：Summarize / Snapshot / Redact）
     ========================================================================== */
  const h32 = s => { let h = 2166136261; for (const c of String(s)) { h ^= c.charCodeAt(0); h = Math.imul(h, 16777619); } return h >>> 0; };
  /* 模拟一条命令的服务端回复：返回 {text: 摘要, rows: 整数回复（影响行数）, sec: 耗时} */
  function simExec(args) {
    const p = parseCommand(args).p, a = args, name = p ? p.name : a[0].toUpperCase(), keys = p ? p.keys : [], hv = h32(a.join(' '));
    const sec = +(0.001 + (hv % 9) / 1000).toFixed(3);
    let text, rows = 0;
    const int = n => { rows = n; text = '(integer) ' + n; };
    if (['SET', 'SETEX', 'PSETEX', 'MSET', 'HMSET', 'LSET', 'LTRIM', 'RENAME', 'PFMERGE'].includes(name)) text = 'OK';
    else if (['DEL', 'UNLINK', 'TOUCH'].includes(name)) int(keys.filter(k => h32(k) % 5 !== 0).length);
    else if (name === 'HSET') int(Math.floor((a.length - 2) / 2));
    else if (name === 'HDEL') int(a.length - 2);
    else if (['GETDEL', 'GETSET', 'GETEX'].includes(name)) text = '"' + (hv % 90 + 10) + '"';
    else if (['LPOP', 'RPOP', 'SPOP'].includes(name)) text = '"item-' + (hv % 90 + 10) + '"';
    else if (['ZPOPMIN', 'ZPOPMAX'].includes(name)) text = '(array, 2 项) "member-1", "1"';
    else if (['INCRBYFLOAT', 'HINCRBYFLOAT'].includes(name)) text = '"' + (hv % 900 / 10 + 1).toFixed(1) + '"';
    else if (['INCR', 'DECR', 'INCRBY', 'DECRBY', 'HINCRBY', 'APPEND', 'SETRANGE', 'LPUSH', 'RPUSH', 'LPUSHX', 'RPUSHX', 'LINSERT', 'LREM', 'ZREM', 'XDEL', 'SADD', 'SREM'].includes(name)) int(hv % 20 + 1);
    else int(1);
    return {text, rows, sec};
  }
  const goDur = s => { const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), x = s % 60; return (h ? h + 'h' : '') + (h || m ? m + 'm' : '') + x + 's'; };
  const TYPE_OF = n => /^H/.test(n) ? 'hash' : /^(L|R)(PUSH|POP|SET|REM|TRIM|INSERT|MOVE)|^LPUSHX|^RPUSHX|^RPOPLPUSH|^LMOVE/.test(n) ? 'list' : /^S(ADD|REM|POP|MOVE)/.test(n) || /STORE$/.test(n) ? 'set' : /^Z/.test(n) ? 'zset' : /^X/.test(n) ? 'stream' : 'string';
  /* 执行前原值（规则勾选“记录执行前值”时）：最多 10 个键；哈希只记命令里点名的字段 */
  function simSnapshot(args) {
    const p = parseCommand(args).p; if (!p) return '';
    const hashFields = () => ['HSET', 'HMSET', 'HSETNX'].includes(p.name) ? args.filter((_, i) => i >= 2 && (i - 2) % 2 === 0) : p.name === 'HDEL' ? args.slice(2) : ['HINCRBY', 'HINCRBYFLOAT'].includes(p.name) ? [args[2]] : [];
    const lines = [];
    p.keys.forEach((k, i) => {
      if (i === 10) { lines.push(`…其余 ${p.keys.length - 10} 个键未记录`); return; }
      if (i > 10) return;
      const hv = h32(k), head = '键 ' + escapeBinary(k);
      if (hv % 7 === 0) { lines.push(head + '：执行前不存在'); return; }
      const typ = TYPE_OF(p.name), ttl = hv % 3 === 0 ? '，无过期时间' : '，剩余 ' + goDur(60 + hv % 7200);
      let s = `${head}：类型 ${typ}${ttl}`;
      if (typ === 'string') s += '，值 ' + escapeBinary('v' + hv % 1000);
      else if (typ === 'hash') {
        const fs = hashFields().slice(0, 20);
        s += fs.length ? '，原字段值 ' + fs.map((f, j) => escapeBinary(f) + '=' + ((hv + j) % 4 === 0 ? '(nil)' : escapeBinary(String((hv + j) % 5)))).join('，') : `，${hv % 30 + 1} 个字段（本命令未点名字段，内容未记录）`;
      } else s += `，${hv % 200 + 1} 个元素（内容未记录）`;
      lines.push(s);
    });
    return lines.join('\n');
  }
  const SECRET_RE = /pass|pwd|secret|token|credential|private/i;
  const cutRunes = (s, n) => { const r = [...s]; return r.length <= n ? s : r.slice(0, n).join('') + '…'; };
  /* 审计里的命令摘要：键 ≤ 64 字符；其余参数 ≤ 24 字符并标总字节数；键名含 pass / token 等时值参数一律 *** */
  function redact(args) {
    if (!args.length) return '';
    const pr = parseCommand(args), keyIdx = new Set(); let secret = false;
    if (pr.p) { const info = CMDS[pr.p.name], last = info.last < 0 ? args.length + info.last : info.last, step = Math.max(1, info.step); for (let i = info.first; i <= last && i < args.length; i += step) { keyIdx.add(i); if (SECRET_RE.test(args[i])) secret = true; } }
    const parts = [args[0].toUpperCase()];
    for (let i = 1; i < args.length; i++) {
      const x = args[i];
      parts.push(keyIdx.has(i) ? cutRunes(x, 64) : secret ? '***' : [...x].length > 24 ? cutRunes(x, 24).replace(/…$/, '') + `…(${byteLen(x)} 字节)` : x);
    }
    return parts.join(' ');
  }

  /* ============================================================================
     规则数据（预置 0 条 = 默认全部拒绝）、模板、演示规则
     ========================================================================== */
  const R = [];
  let NEXT_ID = 1;
  const redisInsts = () => INSTANCES.filter(i => i.type === 'Redis');
  const instLabel = id => { const i = INSTANCES.find(x => x.id === id && x.type === 'Redis'); return i ? i.name : `#${id}（实例不存在或不是 Redis）`; };
  const mk = o => Object.assign({enabled: true, commands: [], key_patterns: [], instance_ids: [], dbs: [], max_keys: 100, record_before: false, description: '', remark: ''}, o);
  const TEMPLATES = [
    {key: 'clean-cache', title: '清理缓存键', description: '允许 DEL / UNLINK 删除 cache: 开头的键，一次最多 100 个。', rule: mk({name: '清理缓存键', commands: ['DEL', 'UNLINK'], key_patterns: ['cache:*'], max_keys: 100, description: '清理缓存键：DEL / UNLINK cache:*'})},
    {key: 'set-expire', title: '设置过期', description: '允许对 session: 开头的键设置 / 修改过期时间。', rule: mk({name: '设置会话键过期', commands: ['EXPIRE', 'PEXPIRE', 'EXPIREAT', 'PEXPIREAT'], key_patterns: ['session:*'], max_keys: 1, description: '设置过期：EXPIRE / PEXPIRE session:*'})},
    {key: 'write-cache', title: '读后写入（写缓存键）', description: '允许 SET / SETEX / PSETEX 写 cache: 开头的键（建议带过期时间；SET 不带过期会给警告）。', rule: mk({name: '写入缓存键', commands: ['SET', 'SETEX', 'PSETEX'], key_patterns: ['cache:*'], max_keys: 1, description: '写入缓存：SET / SETEX cache:*'})},
    {key: 'fix-hash', title: '修复哈希字段', description: '允许对 user: 开头的哈希修改 / 删除字段，并记录执行前的字段值供恢复。', rule: mk({name: '修复用户哈希字段', commands: ['HSET', 'HDEL'], key_patterns: ['user:*'], max_keys: 1, record_before: true, description: '修复哈希字段：HSET / HDEL user:*'})},
  ];
  /* 🧪 仅原型演示：4 个模板规则 + 1 条限定实例 / 库的规则 */
  const DEMO = () => TEMPLATES.map(t => t.rule).concat([mk({name: '重置限流计数器（仅订单缓存主库 db0）', commands: ['DEL', 'SET'], key_patterns: ['ratelimit:*'], instance_ids: [11], dbs: [0], max_keys: 20, description: '🧪 演示：限定实例 redis-order-cache-m 的 db0，演示“适用范围不符”的拒绝原因'})]);

  /* ---- 变更差异（与 service.go 的审计字段一致）---- */
  const yn = b => b ? '是' : '否';
  const short = (s, n) => { const r = [...s]; return r.length <= n ? s : r.slice(0, n).join('') + '…'; };
  const fieldsOf = r => [
    ['名称', r.name], ['启用', yn(r.enabled)], ['命令', r.commands.join(',')], ['键模式', r.key_patterns.join(' | ')],
    ['实例', r.instance_ids.join(',') || '全部'], ['库', r.dbs.join(',') || '全部'], ['键数上限', String(r.max_keys)],
    ['记录执行前值', yn(r.record_before)], ['说明', r.description || ''], ['备注', r.remark || '']];
  const describe = r => fieldsOf(r).filter(f => f[1] !== '').map(f => f[0] + '=' + short(f[1], 80)).join('；');
  function diffAudit(a, b) {
    const fa = fieldsOf(a), fb = fieldsOf(b), parts = [];
    fa.forEach((f, i) => { if (f[1] !== fb[i][1]) parts.push(`${f[0]}：${short(f[1], 80)} → ${short(fb[i][1], 80)}`); });
    return parts.length ? parts.join('；') : '没有字段变化';
  }
  /* 页面上的二次确认差异：实例显示名称 */
  function fieldsUi(r) {
    const f = fieldsOf(r); f[4] = ['实例', r.instance_ids.map(instLabel).join(',') || '全部']; f[6] = ['键数上限', String(r.max_keys || 100)]; return f;
  }
  function diffUi(oldR, newR) {
    const a = oldR ? fieldsUi(oldR) : null, b = fieldsUi(newR), lines = [];
    b.forEach(([label, v], i) => {
      if (!a) { if (v !== '') lines.push(`${label}：${v}`); }
      else if (a[i][1] !== v) lines.push(`${label}：${a[i][1] || '（空）'} → ${v || '（空）'}`);
    });
    return lines.length ? lines.join('\n') : '没有字段变化';
  }
  const stamp = () => TODAY + ' ' + now().slice(11);

  /* ============================================================================
     页面
     ========================================================================== */
  const CSS = `<style>
    .rr-chips{display:flex;flex-wrap:wrap;gap:4px}
    .rr-chips .tag{font-family:ui-monospace,Consolas,monospace;font-size:11px}
    .rr-pick button.tag{cursor:pointer;border:0}
    .rr-pick button.tag:hover{filter:brightness(.92)}
    .rr-err{color:var(--bad);font-size:12px;margin-top:4px;min-height:1em}
    tr.rr-off td{opacity:.55}
    pre.rr-diff{background:var(--panel2);border:1px solid var(--border);border-radius:8px;padding:10px 12px;font-size:12px;white-space:pre-wrap;word-break:break-all;max-height:260px;overflow:auto}
  </style>`;
  const chips = (list, cls = '') => list.length ? `<div class="rr-chips">${list.map(x => `<span class="tag ${cls}">${esc(x)}</span>`).join('')}</div>` : '<span class="muted">—</span>';
  const scopeUi = r => (r.instance_ids.length ? r.instance_ids.map(instLabel).join('、') : '全部实例') + ' / ' + (r.dbs.length ? r.dbs.map(d => 'db' + d).join('、') : '全部库');

  function listHtml() {
    if (!R.length) return empty('还没有规则，Redis 工单默认全部拒绝。', '⚖');
    return tbl(['名称', '状态', '命令', '键匹配', '适用范围', '键数上限', '记录原值', '说明', '最近修改', '操作'], R, {
      attrs: r => r.enabled ? '' : ' class="rr-off"',
      cells: r => [`<b>${esc(r.name)}</b><div class="small muted">#${r.id}</div>`, r.enabled ? '<span class="tag ok">启用</span>' : '<span class="tag">停用</span>', chips(r.commands), chips(r.key_patterns),
        `<span class="small">${esc(scopeUi(r))}</span>`, `<span class="mono">${r.max_keys}</span>`, r.record_before ? '<span class="tag warn" title="原值明文显示在工单详情里">是</span>' : '否',
        `<span class="small">${esc(r.description)}</span>`, `<span class="small">${esc(r.updated_by)}<br><span class="muted mono">${esc(r.updated_at)}</span></span>`,
        `<div class="ops">${B('redisrule.edit', '编辑', {cls: 'sm', data: {op: 'edit', id: r.id}})}${B('redisrule.edit', r.enabled ? '停用' : '启用', {cls: 'sm', data: {op: 'toggle', id: r.id}})}${B('redisrule.edit', '删除', {cls: 'sm danger', data: {op: 'del', id: r.id}})}</div>`]});
  }
  pages.redisrule = {
    title: 'Redis 上线规则',
    render() {
      const on = R.filter(r => r.enabled).length;
      return CSS + `
      ${R.length && !on ? notice('<b>当前没有启用的规则：</b>所有 Redis 工单的检查都会被拒绝。', 'warn', '⚠') : ''}
      <div class="card"><h2>规则 <span class="sub">共 ${R.length} 条，启用 ${on} 条</span></h2>
        <div class="toolbar">${B('redisrule.edit', '＋ 新增规则', {cls: 'primary', data: {op: 'new'}})}
          <label class="small muted">从模板添加 <select id="rrTpl" style="max-width:320px"><option value="">选择模板…</option>${TEMPLATES.map(t => `<option value="${t.key}">${esc(t.title)}</option>`).join('')}</select></label>
          ${B('redisrule.edit', '填入编辑器', {data: {op: 'tpl'}})}
          ${B('redisrule.edit', '🧪 载入演示规则', {data: {op: 'demo'}, title: '原型演示：一键加入 5 条规则'})}
          <span class="sp"></span>${B('redisrule.view', '规则测试', {data: {op: 'test'}})}</div>
        <div id="rrList">${listHtml()}</div></div>
      <div class="card" style="margin-top:14px"><h2>命令清单</h2>
        <details open><summary>硬禁止命令</summary><div style="margin-top:6px">${chips(HARD_LIST, 'bad')}</div></details>
        <details style="margin-top:8px"><summary>支持的写命令（${SUPPORTED.length} 个）</summary><div style="margin-top:6px">${chips(SUPPORTED)}</div></details></div>`;
    }
  };

  /* ---- 新增 / 编辑 ---- */
  const parseCmds = text => { const seen = new Set(), out = []; text.split(/[\s,，]+/).forEach(p => { const n = p.trim().toUpperCase(); if (n && !seen.has(n)) { seen.add(n); out.push(n); } }); return out; };
  function cmdProblems(cmds) {
    const out = [];
    cmds.forEach(c => { if (HARD.has(c)) out.push(`命令 ${c} 属于硬禁止命令，任何规则都无法放行`); else if (!CMDS[c]) out.push(`命令 ${c} 不在受支持的写命令清单里（无键命令、读命令与未知命令不能写入规则）`); });
    return out;
  }
  function refreshCmdHint() {
    const inp = $('#rr_cmds'); if (!inp) return;
    const cmds = parseCmds(inp.value);
    $('#rr_cmd_err').textContent = cmdProblems(cmds).join('；');
    const have = new Set(cmds);
    $('#rr_pick').innerHTML = SUPPORTED.filter(c => !have.has(c)).map(c => `<button type="button" class="tag" data-on="rrPick" data-c="${c}">${c}</button>`).join('');
  }
  ON.rrCmds = () => refreshCmdHint();
  ON.rrPick = d => { const inp = $('#rr_cmds'), cur = inp.value.trim().replace(/[,，]\s*$/, ''); inp.value = cur ? cur + ',' + d.c : d.c; refreshCmdHint(); };

  function readForm() {
    const g = id => $('#' + id), dbs = g('rr_dbs').value.split(/[\s,，]+/).filter(Boolean);
    for (const d of dbs) if (!/^\d+$/.test(d)) throw new Error(`库编号 ${d} 不合法，应为 0 ~ 1023 的整数`);
    const mkeys = g('rr_max').value.trim();
    if (mkeys !== '' && !/^\d+$/.test(mkeys)) throw new Error('键数上限应为整数');
    return {name: g('rr_name').value.trim(), enabled: g('rr_on').checked, commands: parseCmds(g('rr_cmds').value), key_patterns: g('rr_pats').value.split('\n').map(x => x.trim()).filter(Boolean),
      instance_ids: [...g('rr_insts').selectedOptions].map(o => Number(o.value)), dbs: dbs.map(Number), max_keys: mkeys === '' ? 0 : Number(mkeys), record_before: g('rr_before').checked,
      description: g('rr_desc').value.trim(), remark: g('rr_remark').value.trim()};
  }
  function editModal(rule, preset) {
    const r = rule || (preset ? preset.rule : mk({})), ids = redisInsts().map(i => i.id);
    const instOpts = redisInsts().map(i => `<option value="${i.id}" ${r.instance_ids.includes(i.id) ? 'selected' : ''}>${esc(i.name)}</option>`)
      .concat(r.instance_ids.filter(id => !ids.includes(id)).map(id => `<option value="${id}" selected>${esc(instLabel(id))}</option>`)).join('');   // 已不存在的实例保留，避免保存时悄悄丢掉范围
    openModal({title: rule ? `编辑规则：${rule.name}` : preset ? `新增规则（来自模板：${preset.title}）` : '新增规则', wide: true, code: 'redisrule.edit', ok: '保存…',
      body: `<div id="rrErr"></div><div class="form">
        ${fld('规则名称（唯一）', `<input id="rr_name" maxlength="50" value="${esc(r.name || '')}" autocomplete="off">`, {req: true})}
        ${fld('状态', sw('rr_on', r.enabled !== false, '启用'))}
        ${fld('命令（逗号分隔）', `<input id="rr_cmds" class="mono" data-oninput="rrCmds" value="${esc(r.commands.join(','))}" placeholder="DEL,UNLINK" autocomplete="off"><div class="rr-err" id="rr_cmd_err"></div>
          <details style="margin-top:4px"><summary class="small">选择命令</summary><div class="rr-chips rr-pick" id="rr_pick" style="margin-top:6px;max-height:140px;overflow:auto"></div></details>`, {req: true})}
        ${fld('键匹配（glob，每行一个）', `<textarea id="rr_pats" rows="3" class="mono" placeholder="cache:*&#10;session:*">${esc(r.key_patterns.join('\n'))}</textarea>`, {req: true})}
        ${fld('限定实例（不选 = 全部）', `<select id="rr_insts" multiple size="4" style="width:100%">${instOpts}</select>`)}
        ${fld('限定 db 编号（留空 = 全部）', `<input id="rr_dbs" value="${esc(r.dbs.join(','))}" placeholder="0,1,3" autocomplete="off">`)}
        ${fld('单条命令键数上限（1 ~ 10000）', `<input id="rr_max" inputmode="numeric" value="${r.max_keys || 100}" autocomplete="off">`)}
        ${fld('记录执行前值', `<label class="chk"><input type="checkbox" id="rr_before" ${r.record_before ? 'checked' : ''}> 执行前记录被改键的原值（<b style="color:var(--warn)">原值会明文显示在工单详情里</b>）</label>`)}
        ${fld('说明', `<input id="rr_desc" maxlength="500" value="${esc(r.description || '')}" autocomplete="off">`)}
        ${fld('备注', `<input id="rr_remark" maxlength="500" value="${esc(r.remark || '')}" autocomplete="off">`)}</div>`,
      onOk() {
        const err = t => { $('#rrErr').innerHTML = notice(esc(t), 'bad', '✕'); return false; };
        let body, nr;
        try {
          body = readForm();
          const probs = cmdProblems(body.commands); if (probs.length) return err(probs[0]);
          nr = normalize(body);
        } catch (e) { return err(e.message); }
        if (R.some(x => x !== rule && x.name === nr.name)) return err('已有同名规则');
        if (!rule && R.length >= LIM.rules) return err(`规则最多 ${LIM.rules} 条`);
        confirmChange({title: rule ? `确认修改规则「${rule.name}」` : `确认新增规则「${nr.name}」`, text: rule ? '将按下面的差异修改规则：' : '将新增下面的规则：', diff: diffUi(rule, nr),
          run() {
            if (rule) {
              const old = {...rule, commands: rule.commands.slice(), key_patterns: rule.key_patterns.slice(), instance_ids: rule.instance_ids.slice(), dbs: rule.dbs.slice()};
              Object.assign(rule, nr, {updated_by: ME.login, updated_at: stamp()});
              audit('redisrule.edit', `Redis上线规则-修改：规则 #${rule.id}「${old.name}」：${diffAudit(old, rule)}`, '成功'); toast(`规则「${rule.name}」已修改`);
            } else {
              const added = Object.assign(nr, {id: NEXT_ID++, created_by: ME.login, updated_by: ME.login, updated_at: stamp()}); R.push(added);
              audit('redisrule.edit', `Redis上线规则-新增：规则 #${added.id}「${added.name}」：${describe(added)}`, '成功'); toast(`规则「${added.name}」已新增`);
            }
            rerender();
          }});
        return false;
      }});
    refreshCmdHint();
  }
  /* 二次确认：变更会立即影响所有 Redis 工单的检查与执行 */
  function confirmChange({title, text, diff, run}) {
    openModal({title: '⚠ ' + title, code: 'redisrule.edit', ok: '确认', okCls: 'danger primary',
      body: `${notice(esc(text), 'warn', '⚠')}${diff ? `<pre class="rr-diff">${esc(diff)}</pre>` : ''}<div class="small muted">变更立即对所有 Redis 工单生效，并写入系统审计。</div>`,
      onOk() { run(); }});
  }
  ACTIONS['redisrule.edit'] = d => {
    if (d.op === 'new') return editModal(null);
    if (d.op === 'tpl') { const t = TEMPLATES.find(x => x.key === ($('#rrTpl') || {}).value); return t ? editModal(null, t) : toast('请先选择一个模板'); }
    if (d.op === 'demo') {
      const todo = DEMO().filter(x => !R.some(r => r.name === x.name));
      if (!todo.length) return toast('演示规则已全部载入');
      return confirmChange({title: `🧪 载入演示规则（${todo.length} 条）`, text: '仅原型演示：将一次性新增下面的规则（已存在同名的会跳过），均为启用状态。', diff: todo.map(x => `「${x.name}」 ${x.commands.join(',')}  ${x.key_patterns.join(' | ')}  ${scopeUi(x)}  上限 ${x.max_keys}${x.record_before ? '  记录执行前值' : ''}`).join('\n'),
        run() { todo.forEach(x => { const added = Object.assign(normalize(x), {id: NEXT_ID++, created_by: ME.login, updated_by: ME.login, updated_at: stamp()}); R.push(added); audit('redisrule.edit', `Redis上线规则-新增：规则 #${added.id}「${added.name}」：${describe(added)}（🧪 原型演示规则）`, '成功'); }); toast(`已载入 ${todo.length} 条演示规则`); rerender(); }});
    }
    const r = R.find(x => x.id == d.id); if (!r) return toast('规则不存在');
    if (d.op === 'edit') return editModal(r);
    if (d.op === 'toggle') {
      const to = !r.enabled;
      return confirmChange({title: `确认${to ? '启用' : '停用'}规则「${r.name}」`, text: to ? '启用后，匹配这条规则的 Redis 命令将被允许提交和执行。' : '停用后，只靠这条规则放行的命令将被拒绝。', diff: `启用：${yn(r.enabled)} → ${yn(to)}`,
        run() { const was = r.enabled; r.enabled = to; r.updated_by = ME.login; r.updated_at = stamp(); audit('redisrule.edit', `Redis上线规则-${to ? '启用' : '停用'}：规则 #${r.id}「${r.name}」：启用 ${yn(was)} → ${yn(to)}`, '成功'); toast(`规则「${r.name}」已${to ? '启用' : '停用'}`); rerender(); }});
    }
    if (d.op === 'del') {
      return confirmChange({title: `确认删除规则「${r.name}」`, text: '删除后无法恢复。只靠这条规则放行的命令将被拒绝。', diff: fieldsUi(r).filter(f => f[1] !== '').map(f => `${f[0]}：${f[1]}`).join('\n'),
        run() { R.splice(R.indexOf(r), 1); audit('redisrule.edit', `Redis上线规则-删除：规则 #${r.id}「${r.name}」（删除前）：${describe(r)}`, '成功'); toast(`规则「${r.name}」已删除`); rerender(); }});
    }
  };

  /* ---- 规则测试：用当前规则试检查一段命令（不执行、不写审计）---- */
  ON.rrTestRun = () => {
    const db = $('#rrT_db').value.trim(), out = $('#rrT_out');
    if (!/^\d+$/.test(db) || +db > 1023) { out.innerHTML = notice('db 编号应为 0 ~ 1023 的整数', 'bad', '✕'); return; }
    const sp = splitLines($('#rrT_text').value);
    if (sp.error) { out.innerHTML = notice(esc(sp.error), 'bad', '✕'); return; }
    const t = {instId: +$('#rrT_inst').value, db: +db}, rows = sp.lines.map(l => ({l, v: evalLine(R, t, l.text)}));
    out.innerHTML = tbl(['行', '结果', '命令', '说明'], rows, {
      attrs: x => x.v.level === 2 ? ' style="background:var(--bad-soft)"' : x.v.level === 1 ? ' style="background:var(--warn-soft)"' : '',
      cells: x => [x.l.no, `<span class="tag ${['ok', 'warn', 'bad'][x.v.level]}">${x.v.status}</span>`, `<span class="mono small">${esc(x.l.text)}</span>`, `<span class="small">${esc(x.v.message || (x.v.ruleName ? `命中规则「${x.v.ruleName}」` : ''))}</span>`]});
  };
  ACTIONS['redisrule.view'] = () => {
    openModal({title: '规则测试', wide: true, code: 'redisrule.view', ok: '', cancel: '关闭',
      body: `<div class="form">${fld('实例', `<select id="rrT_inst"><option value="0">（任意实例）</option>${redisInsts().map(i => `<option value="${i.id}">${esc(i.name)}</option>`).join('')}</select>`)}
          ${fld('db 编号', '<input id="rrT_db" value="0" inputmode="numeric" autocomplete="off" style="width:120px">')}
          ${fld('命令（每行一条）', '<textarea id="rrT_text" rows="5" class="mono" placeholder="DEL cache:a cache:b&#10;SET cache:x 1 EX 60" autocomplete="off"></textarea>')}</div>
        <div class="row" style="margin:8px 0"><button class="btn primary" data-on="rrTestRun">检查</button><span class="small muted">共 ${R.length} 条规则，启用 ${R.filter(r => r.enabled).length} 条</span></div><div id="rrT_out"></div>`});
  };

  window.RedisRules = {rules: R, splitLines, evalLine, evaluate, tokenize, redact, escapeBinary, simExec, simSnapshot, MAX_LINES, hard: HARD_LIST, supported: SUPPORTED, demoNames: () => DEMO().map(x => x.name)};
})();
