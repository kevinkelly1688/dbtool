/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_redis.js
 * @time: 2026/10/28
 */
/* ============================================================================
   Redis 支持（原型）：巡检（检查项 / 评分 / 报告详情）与慢查询面板。
   行为依据：internal/inspect/redis_checks.go / redis_report.go / redis_collect.go、
   internal/slowlog/redis_*.go、web/static/js/slowlog/redis.js。数据全部虚构，只发只读命令的约定见手册。
   导出（运行时由 p_inst.js / p_opt.js 调用，本文件不注册菜单页）：
     window.RedisInspect  —— genItems / health / detailBody / miniTable / steps
     window.RedisSlow     —— isRedis / render（慢查日志页选中 Redis 实例时的面板）
   ========================================================================== */
(function () {
'use strict';

const R = (r, a, b) => a + Math.floor(r() * (b - a + 1));
const MB = 1 << 20, GB = 1024 * MB;
const daysBetween = (a, b) => Math.round((Date.parse(b + 'T00:00:00') - Date.parse(a + 'T00:00:00')) / 864e5);
const NOW0 = TODAY + ' 11:20';
const NOW_MS = Date.parse(NOW0.replace(' ', 'T') + ':00');
const T0 = Date.now();
const clkFull = () => { const d = new Date(NOW_MS + Date.now() - T0); return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`; };
const fmtTs = sec => { const d = new Date(sec * 1000); return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`; };
const fi = n => Math.round(n).toLocaleString('en-US');
const bytes = b => b >= 2 ** 40 ? (b / 2 ** 40).toFixed(2) + ' TB' : b >= GB ? (b / GB).toFixed(2) + ' GB' : b >= MB ? (b / MB).toFixed(1) + ' MB' : b >= 1024 ? (b / 1024).toFixed(1) + ' KB' : Math.round(b) + ' B';
const durText = s => s >= 86400 ? `${Math.floor(s / 86400)} 天 ${Math.floor(s % 86400 / 3600)} 小时` : s >= 3600 ? `${Math.floor(s / 3600)} 小时 ${Math.floor(s % 3600 / 60)} 分钟` : s >= 60 ? `${Math.floor(s / 60)} 分钟` : `${s} 秒`;
const has = v => v !== undefined && v !== null;
const isRedisInst = id => (INSTANCES.find(i => i.id == id) || {}).type === 'Redis';

/* ---------- 命令指纹（internal/engine/redis/fingerprint.go）---------- */
const TWO_WORD = new Set(['CONFIG', 'CLIENT', 'SLOWLOG', 'MEMORY', 'XINFO', 'XGROUP', 'SCRIPT', 'OBJECT', 'CLUSTER', 'COMMAND', 'ACL', 'FUNCTION', 'PUBSUB', 'LATENCY', 'MODULE']);
const KEEP_WORDS = new Set(('EX PX EXAT PXAT NX XX GT LT CH INCR KEEPTTL GET COUNT MATCH TYPE LIMIT WITHSCORES BYSCORE BYLEX REV LEFT RIGHT BEFORE AFTER MAXLEN MINID STORE STOREDIST ASC DESC ALPHA BY WEIGHTS AGGREGATE SUM MIN MAX WITHCOORD WITHDIST WITHHASH ASYNC SYNC REPLACE FROMMEMBER FROMLONLAT BYRADIUS BYBOX BLOCK STREAMS GROUP NOACK WITHVALUES ID SAMPLES').split(' '));
function fingerprint(args) {
  if (!args.length) return '';
  let name = args[0].toUpperCase(), rest = args.slice(1);
  if (TWO_WORD.has(name) && rest.length) { name += ' ' + rest[0].toUpperCase(); rest = rest.slice(1); }
  const parts = [name]; let run = 0;
  const flush = () => { if (run === 1) parts.push('?'); else if (run === 2) parts.push('?', '?'); else if (run > 2) parts.push('?', '?', '...'); run = 0; };
  rest.forEach(a => { const kw = a.toUpperCase(); if (KEEP_WORDS.has(kw) && a.length <= 12) { flush(); parts.push(kw); } else run++; });
  flush(); return parts.join(' ');
}
function cmdName(args) { let n = args[0].toUpperCase(); if (TWO_WORD.has(n) && args.length > 1) n += ' ' + args[1].toUpperCase(); return n; }
const quoteArg = a => /[\s"']/.test(a) ? '"' + a.replace(/"/g, '\\"') + '"' : a;
const cmdText = args => args.map(quoteArg).join(' ');
function hash12(str) {
  let a = 0x811c9dc5, b = 0x01000193;
  for (let i = 0; i < str.length; i++) { a = Math.imul(a ^ str.charCodeAt(i), 0x01000193) >>> 0; b = Math.imul(b ^ str.charCodeAt(i), 0x85ebca6b) >>> 0; }
  return (a.toString(16).padStart(8, '0') + b.toString(16).padStart(8, '0')).slice(0, 12);
}

/* ============================================================================
   一、巡检：检查项判定（对应 redis_checks.go；阈值同源）
   ========================================================================== */
const TH = {usageAtt: 80, usageAlert: 90, fragMin: 32 * MB, fragAtt: 1.5, fragAlert: 2.0, fragSwap: 1.0, peakAtt: 95, rdbAgeAtt: 86400, rdbAgeAlert: 259200,
  cliAtt: 80, cliAlert: 90, blockedAtt: 10, blockedAlert: 50, rejectedAlert: 100, evictedAlert: 1000000, hitMin: 10000, hitAtt: 90, hitAlert: 70,
  slowAtt: 10, slowAlert: 100, forkAtt: 500000, forkAlert: 2000000, lagAtt: 10, lagAlert: 30, ioAtt: 10, ioAlert: 60, gapAlert: 100 * MB,
  bigBytesAtt: 1 * MB, bigBytesAlert: 10 * MB, bigElemsAtt: 10000, bigElemsAlert: 100000, uptimeAtt: 600, volatileMin: 10};
const SECTIONS = [['basic', '基本信息'], ['replication', '主从复制'], ['memory', '内存'], ['persistence', '持久化'], ['clients', '客户端连接'], ['stats', '性能与命中率'], ['security', '安全'], ['keyspace', '键空间']];
const secName = code => (SECTIONS.find(s => s[0] === code) || [0, code])[1];
const ADVICE = {
  'basic.version': 'Redis 5.0 以前已停止维护，缺少 ACL、Stream、MEMORY USAGE 等能力；规划升级到仍在维护的版本。',
  'basic.uptime': '实例刚启动或正在加载数据：确认重启原因（OOM 被杀 / 人为重启），并检查持久化文件是否完整、主从是否已重新同步。',
  'replication.role': '单节点没有高可用：重要数据建议配置从库并启用哨兵 / 集群，至少保证持久化开启。',
  'replication.link': '从库与主库的复制链路异常：检查网络、主库 repl-backlog-size 与 client-output-buffer-limit replica，必要时在从库上重新同步。',
  'replication.lag': '复制延迟偏大：检查主库写入量和从库负载，网络带宽，以及是否有大 key / 慢命令阻塞了从库回放。',
  'memory.usage': '内存使用率偏高：评估扩容或设置 maxmemory 与合适的淘汰策略，清理无用 key / 过期数据，排查大 key。',
  'memory.maxmemory': '未设置 maxmemory：数据增长会一直占用主机内存，直到被系统 OOM 杀掉；按主机内存的 60%~70% 设置，并预留 fork 和复制缓冲的空间。',
  'memory.policy': 'maxmemory 已设置但淘汰策略是 noeviction：内存满后所有写入都会报错；缓存场景改用 allkeys-lru / allkeys-lfu，数据库场景则要做好容量告警。',
  'memory.frag': '碎片率过高：开启 activedefrag yes（Redis 4.0+），或在低峰期重启 / 主从切换释放；低于 1 说明发生了内存交换，必须扩内存。',
  'memory.peak': '历史内存峰值已接近上限：说明曾出现过内存压力，评估扩容或调整淘汰策略。',
  'persistence.mode': '没有开启任何持久化：实例重启后数据全部丢失；纯缓存可忽略，否则开启 AOF（appendonly yes）或配置 RDB（save）。',
  'persistence.rdb': 'RDB 保存失败或太久没有成功：检查磁盘空间 / 权限、fork 是否失败（内存超配 vm.overcommit_memory=1）和 dir 配置。',
  'persistence.aof': 'AOF 写入或重写失败：检查磁盘空间与 IO，必要时 redis-check-aof 修复；磁盘写满会让 Redis 拒绝写入。',
  'clients.usage': '连接数接近 maxclients：检查业务连接池配置是否泄漏，必要时调大 maxclients（同时留意系统文件描述符限制）。',
  'clients.blocked': '阻塞中的客户端较多（BLPOP / BRPOP / WAIT 等）：确认是否是预期的消费者；异常堆积说明生产端停了或消费逻辑卡住。',
  'clients.rejected': '有连接被拒绝（达到 maxclients）：检查连接泄漏，调大 maxclients 或使用连接池 / 代理收敛连接数。',
  'stats.ops': '仅供参考：当前每秒命令数。',
  'stats.hit': '命中率偏低：检查缓存 key 的过期时间是否过短、是否被淘汰、是否有大量穿透查询，必要时增加内存或预热缓存。',
  'stats.evicted': '已有 key 因内存不足被淘汰：评估扩容，或确认缓存策略是否符合预期；非缓存实例出现淘汰要尽快处理。',
  'stats.slowlog': '存在慢命令：到“SQL 优化 › 慢查日志”查看 Redis 慢查询，避免 KEYS / HGETALL / SMEMBERS 等 O(N) 命令作用于大 key，改用 SCAN / HSCAN 或拆分 key。',
  'stats.fork': 'fork 耗时过长会让 Redis 在 RDB / AOF 重写时卡顿：减小实例内存、避开业务高峰、关闭透明大页（THP）。',
  'security.password': '实例没有设置访问密码：任何能连到端口的人都可读写全部数据；设置 requirepass（或 ACL 用户），并限制网络访问范围。',
  'security.exposed': '实例监听了所有网卡且关闭了 protected-mode：只绑定内网地址，保持 protected-mode yes，并用防火墙 / 安全组限制来源。',
  'security.commands': 'FLUSHALL / FLUSHDB / KEYS / CONFIG / DEBUG / SHUTDOWN 等危险命令仍可直接使用：用 rename-command 改名或置空，或用 ACL 禁止业务账号使用。',
  'keyspace.summary': '使用 volatile-* 淘汰策略时，没有过期时间的 key 不会被淘汰：给缓存 key 设置过期时间，或改用 allkeys-* 策略。',
  'keyspace.bigkey': '存在大 key：拆分为多个小 key（按范围 / hash 分片），避免对大 key 做 O(N) 命令；删除大 key 用 UNLINK。'};
const ADVICE_FAILED = '采集失败：请确认巡检账号权限足够、实例网络可达后重新巡检。';
const DANGEROUS = ['FLUSHALL', 'FLUSHDB', 'KEYS', 'CONFIG', 'DEBUG', 'SHUTDOWN'];
/* 各检查项的扣分 {关注, 告警}（redisPenalty）；告警且扣分 ≥ 20 的算严重（critical） */
const PEN = {'basic.version': [2, 0], 'basic.uptime': [2, 0], 'replication.link': [5, 20], 'replication.lag': [3, 10],
  'memory.usage': [10, 25], 'memory.maxmemory': [3, 0], 'memory.policy': [2, 0], 'memory.frag': [3, 10], 'memory.peak': [3, 0],
  'persistence.mode': [5, 0], 'persistence.rdb': [4, 20], 'persistence.aof': [0, 20],
  'clients.usage': [8, 20], 'clients.blocked': [3, 8], 'clients.rejected': [5, 10],
  'stats.hit': [4, 10], 'stats.evicted': [5, 10], 'stats.slowlog': [5, 10], 'stats.fork': [3, 8],
  'security.password': [0, 20], 'security.exposed': [5, 15], 'security.commands': [3, 0],
  'keyspace.summary': [3, 8], 'keyspace.bigkey': [3, 10]};
const ST = ['ok', 'warn', 'bad', 'na'];

function evalRedis(s) {
  const out = [], info = s.info, cfg = s.cfg;
  const add = c => { c.advice = ADVICE[c.section + '.' + c.key] || ''; out.push(c); };
  const cfgGet = k => cfg && has(cfg[k]) ? cfg[k] : undefined;
  const role = info.role || 'master';
  const maxmem = has(info.maxmemory) ? info.maxmemory : (has(cfgGet('maxmemory')) ? +cfgGet('maxmemory') : undefined);
  const policy = info.maxmemory_policy || cfgGet('maxmemory-policy') || '';

  /* 基本信息 */
  { const ver = info.redis_version, mode = info.redis_mode || 'standalone';
    let v = `Redis ${ver || '未知版本'} · ${mode}`; if (info.os) v += ' · ' + info.os.slice(0, 60);
    const major = parseInt(ver, 10) || 0;
    add({section: 'basic', key: 'version', name: '版本与连通', value: v, threshold: '主版本 ≥ 5', level: major > 0 && major < 5 ? 1 : 0});
    const up = info.uptime_in_seconds || 0; let uv = durText(up), ul = 0;
    if (up > 0 && up < TH.uptimeAtt) { ul = 1; uv += '（刚启动）'; }
    if (info.loading === 1) { ul = 1; uv += '（正在加载数据）'; }
    add({section: 'basic', key: 'uptime', name: '运行时长', value: uv, threshold: `≥ ${TH.uptimeAtt / 60} 分钟`, level: ul}); }

  /* 主从复制 */
  { const roleVal = role === 'slave' ? `slave（主库 ${info.master_host}:${info.master_port}）` : `master，从库 ${info.connected_slaves || 0} 个`;
    add({section: 'replication', key: 'role', name: '角色', value: roleVal, threshold: '—', level: 0});
    if (role === 'slave') {
      const status = info.master_link_status; let lv = `链路 ${status || '未知'}`, ll = 0;
      if (status !== 'up') { ll = 2; if (has(info.master_link_down_since_seconds) && info.master_link_down_since_seconds >= 0) lv += `，已断开 ${durText(info.master_link_down_since_seconds)}`; }
      else if (info.master_sync_in_progress === 1) { ll = 1; lv += '，正在全量同步'; }
      add({section: 'replication', key: 'link', name: '主从链路', value: lv, threshold: 'master_link_status = up', level: ll});
      let gv = '—', gl = 0;
      if (has(info.master_last_io_seconds_ago)) {
        const io = info.master_last_io_seconds_ago;
        if (io < 0) { gv = '与主库无通信'; gl = 2; } else { gv = `距上次与主库通信 ${io} 秒`; gl = io >= TH.ioAlert ? 2 : io >= TH.ioAtt ? 1 : 0; }
      } else { gl = 3; gv = '未采集（INFO 里没有 master_last_io_seconds_ago）'; }
      add({section: 'replication', key: 'lag', name: '复制延迟', value: gv, threshold: `关注 ≥ ${TH.ioAtt} 秒 / 告警 ≥ ${TH.ioAlert} 秒`, level: gl});
    } else {
      const reps = (info.slaves || []).slice().sort((a, b) => a.addr < b.addr ? -1 : 1);
      let linkLevel = 0, lagLevel = 0, linkVal = '无从库', lagVal = '—', table = null;
      if (reps.length) {
        const rows = [], bad = []; let maxLag = 0, maxGap = 0, online = 0;
        reps.forEach(r => {
          const gap = Math.max((info.master_repl_offset || 0) - r.offset, 0);
          rows.push([r.addr, r.state, r.lag, gap]);
          if (r.state === 'online') { online++; maxLag = Math.max(maxLag, r.lag); maxGap = Math.max(maxGap, gap); return; }
          bad.push(`${r.addr}(${r.state})`);
          linkLevel = ['wait_bgsave', 'send_bulk', 'bg_transfer'].includes(r.state) ? Math.max(linkLevel, 1) : 2;
        });
        linkVal = `${online} / ${reps.length} 个从库在线` + (bad.length ? '；异常：' + bad.join('、') : '');
        lagVal = `最大延迟 ${maxLag} 秒，最大偏移差 ${bytes(maxGap)}`;
        lagLevel = maxLag >= TH.lagAlert || maxGap >= TH.gapAlert ? 2 : maxLag >= TH.lagAtt ? 1 : 0;
        table = {columns: ['从库', '状态', '延迟(秒)', '偏移差(字节)'], rows};
      }
      add({section: 'replication', key: 'link', name: '从库连接状态', value: linkVal, threshold: '全部从库 state = online', level: linkLevel, table});
      add({section: 'replication', key: 'lag', name: '复制延迟', value: lagVal, threshold: `关注 ≥ ${TH.lagAtt} 秒 / 告警 ≥ ${TH.lagAlert} 秒或偏移差 ≥ ${bytes(TH.gapAlert)}`, level: lagLevel});
    } }

  /* 内存 */
  { const used = info.used_memory;
    const usage = {section: 'memory', key: 'usage', name: '内存使用率', threshold: `关注 ≥ ${TH.usageAtt}% / 告警 ≥ ${TH.usageAlert}%`, level: 0};
    let limit = 0, basis = '';
    if (has(maxmem) && maxmem > 0) { limit = maxmem; basis = 'maxmemory'; } else if (has(info.total_system_memory) && info.total_system_memory > 0) { limit = info.total_system_memory; basis = '主机内存'; }
    if (!has(used)) { usage.level = 3; usage.value = '未采集（INFO 里没有 used_memory）'; }
    else if (limit <= 0) { usage.level = 3; usage.value = `已用 ${bytes(used)}；无法确定上限（未设置 maxmemory 且读不到主机内存）`; }
    else { const pct = used * 100 / limit; usage.value = `${bytes(used)} / ${bytes(limit)}（${basis}）= ${pct.toFixed(1)}%`; usage.level = pct >= TH.usageAlert ? 2 : pct >= TH.usageAtt ? 1 : 0; }
    add(usage);
    const mm = {section: 'memory', key: 'maxmemory', name: 'maxmemory', threshold: '> 0', level: 0};
    if (!has(maxmem)) { mm.level = 3; mm.value = '未采集（INFO 与 CONFIG GET 都读不到）'; } else if (maxmem === 0) { mm.level = 1; mm.value = '0（未设置上限）'; } else mm.value = bytes(maxmem);
    add(mm);
    const pc = {section: 'memory', key: 'policy', name: '淘汰策略', threshold: '设置 maxmemory 时不应为 noeviction（缓存场景）', level: 0};
    if (!policy) { pc.level = 3; pc.value = '未采集'; } else if (policy === 'noeviction' && has(maxmem) && maxmem > 0) { pc.level = 1; pc.value = policy + '（内存满后写入会报错）'; } else pc.value = policy;
    add(pc);
    const frag = {section: 'memory', key: 'frag', name: '内存碎片率', threshold: `1.0–${TH.fragAtt.toFixed(1)} 为宜；≥ ${TH.fragAlert.toFixed(1)} 告警；< 1 说明发生了内存交换`, level: 0};
    const ratio = info.mem_fragmentation_ratio;
    if (!has(ratio)) { frag.level = 3; frag.value = '未采集'; }
    else if (used < TH.fragMin) frag.value = `${ratio.toFixed(2)}（数据量 ${bytes(used)} 太小，不评估）`;
    else { frag.value = `${ratio.toFixed(2)}（RSS ${bytes(info.used_memory_rss || 0)}）`;
      if (ratio < TH.fragSwap) { frag.level = 2; frag.value += '，可能已使用交换分区'; } else if (ratio >= TH.fragAlert) frag.level = 2; else if (ratio >= TH.fragAtt) frag.level = 1; }
    add(frag);
    const pk = {section: 'memory', key: 'peak', name: '历史内存峰值', threshold: `占 maxmemory < ${TH.peakAtt}%`, level: 0};
    if (!has(info.used_memory_peak)) { pk.level = 3; pk.value = '未采集'; }
    else if (has(maxmem) && maxmem > 0) { const pct = info.used_memory_peak * 100 / maxmem; pk.value = `${bytes(info.used_memory_peak)}（占 maxmemory ${pct.toFixed(1)}%）`; if (pct >= TH.peakAtt) pk.level = 1; }
    else pk.value = bytes(info.used_memory_peak);
    add(pk); }

  /* 持久化 */
  { let rdbOn = false, rdbKnown = false, saveCfg = '';
    if (has(cfgGet('save'))) { saveCfg = String(cfgGet('save')).trim(); rdbOn = saveCfg !== ''; rdbKnown = true; }
    let aofOn = info.aof_enabled === 1; if (has(cfgGet('appendonly')) && !aofOn) aofOn = cfgGet('appendonly') === 'yes';
    if (!rdbKnown) rdbOn = has(info.rdb_last_save_time);
    const onOff = (on, d) => !on ? '关' : d ? `开（${d.slice(0, 60)}）` : '开';
    add({section: 'persistence', key: 'mode', name: '持久化方式', value: `RDB：${onOff(rdbOn, saveCfg)}；AOF：${onOff(aofOn, '')}`, threshold: 'RDB 或 AOF 至少开启一种', level: !rdbOn && !aofOn ? 1 : 0});
    const rdb = {section: 'persistence', key: 'rdb', name: 'RDB 最近一次保存', threshold: `bgsave 状态 ok，且有变更时 ${durText(TH.rdbAgeAtt)} 内保存过`, level: 0};
    const status = info.rdb_last_bgsave_status || '', last = info.rdb_last_save_time, changes = info.rdb_changes_since_last_save || 0;
    if (!status) { rdb.level = 3; rdb.value = '未采集（INFO 里没有 rdb_last_bgsave_status）'; }
    else if (!rdbOn && rdbKnown) rdb.value = 'RDB 未开启';
    else { rdb.value = 'bgsave 状态 ' + status; let age = 0;
      if (has(last) && last > 0) { age = s.nowS - last; rdb.value += `，最近一次保存在 ${durText(age)} 前（${fmtTs(last)}）`; }
      if (status !== 'ok') rdb.level = 2;
      else if (rdbOn && changes > 0 && age > TH.rdbAgeAlert) { rdb.level = 2; rdb.value += `，此后有 ${changes} 次变更未保存`; }
      else if (rdbOn && changes > 0 && age > TH.rdbAgeAtt) { rdb.level = 1; rdb.value += `，此后有 ${changes} 次变更未保存`; } }
    add(rdb);
    const aof = {section: 'persistence', key: 'aof', name: 'AOF 状态', threshold: '最近一次写入和重写都是 ok', level: 0};
    if (!aofOn) aof.value = 'AOF 未开启';
    else { const w = info.aof_last_write_status || '', rw = info.aof_last_bgrewrite_status || '';
      aof.value = `最近写入 ${w || '未知'}，最近重写 ${rw || '未知'}`; if (has(info.aof_current_size)) aof.value += '，文件 ' + bytes(info.aof_current_size);
      if ((w && w !== 'ok') || (rw && rw !== 'ok')) aof.level = 2; }
    add(aof); }

  /* 客户端连接 */
  { const conn = info.connected_clients; let maxc = info.maxclients; if (!has(maxc) && has(cfgGet('maxclients'))) maxc = +cfgGet('maxclients');
    const use = {section: 'clients', key: 'usage', name: '连接数', threshold: `占 maxclients：关注 ≥ ${TH.cliAtt}% / 告警 ≥ ${TH.cliAlert}%`, level: 0};
    if (!has(conn)) { use.level = 3; use.value = '未采集'; } else if (!has(maxc) || maxc <= 0) use.value = `${conn}（maxclients 未知）`;
    else { const pct = conn * 100 / maxc; use.value = `${conn} / ${maxc} = ${pct.toFixed(1)}%`; use.level = pct >= TH.cliAlert ? 2 : pct >= TH.cliAtt ? 1 : 0; }
    add(use);
    const bl = {section: 'clients', key: 'blocked', name: '阻塞中的客户端', threshold: `关注 ≥ ${TH.blockedAtt} / 告警 ≥ ${TH.blockedAlert}`, level: 0};
    if (!has(info.blocked_clients)) { bl.level = 3; bl.value = '未采集'; } else { bl.value = String(info.blocked_clients); bl.level = info.blocked_clients >= TH.blockedAlert ? 2 : info.blocked_clients >= TH.blockedAtt ? 1 : 0; }
    add(bl);
    const rc = {section: 'clients', key: 'rejected', name: '被拒绝的连接数', threshold: `自启动累计：> 0 关注 / ≥ ${TH.rejectedAlert} 告警`, level: 0};
    if (!has(info.rejected_connections)) { rc.level = 3; rc.value = '未采集'; } else { rc.value = `${info.rejected_connections}（累计接入 ${info.total_connections_received || 0}）`; rc.level = info.rejected_connections >= TH.rejectedAlert ? 2 : info.rejected_connections > 0 ? 1 : 0; }
    add(rc); }

  /* 性能与命中率 */
  { if (has(info.instantaneous_ops_per_sec)) add({section: 'stats', key: 'ops', name: '每秒命令数', value: String(info.instantaneous_ops_per_sec), threshold: '—', level: 0});
    const hit = {section: 'stats', key: 'hit', name: 'keyspace 命中率', threshold: `请求 ≥ ${TH.hitMin} 次时：关注 < ${TH.hitAtt}% / 告警 < ${TH.hitAlert}%`, level: 0};
    if (!has(info.keyspace_hits) || !has(info.keyspace_misses)) { hit.level = 3; hit.value = '未采集'; }
    else if (info.keyspace_hits + info.keyspace_misses < TH.hitMin) hit.value = `请求数 ${info.keyspace_hits + info.keyspace_misses} 太少，不评估`;
    else { const pct = info.keyspace_hits * 100 / (info.keyspace_hits + info.keyspace_misses); hit.value = `${pct.toFixed(2)}%（命中 ${info.keyspace_hits} / 未命中 ${info.keyspace_misses}）`; hit.level = pct < TH.hitAlert ? 2 : pct < TH.hitAtt ? 1 : 0; }
    add(hit);
    const ev = {section: 'stats', key: 'evicted', name: '被淘汰的 key', threshold: `自启动累计：> 0 关注 / ≥ ${TH.evictedAlert} 告警`, level: 0};
    if (!has(info.evicted_keys)) { ev.level = 3; ev.value = '未采集'; } else { ev.value = `${info.evicted_keys}（已过期删除 ${info.expired_keys || 0}）`; ev.level = info.evicted_keys >= TH.evictedAlert ? 2 : info.evicted_keys > 0 ? 1 : 0; }
    add(ev);
    const sl = {section: 'stats', key: 'slowlog', name: '慢日志条数（SLOWLOG LEN）', threshold: `关注 ≥ ${TH.slowAtt} / 告警 ≥ ${TH.slowAlert}`, level: 0};
    if (s.slowErr) { sl.level = 3; sl.value = '未采集：' + s.slowErr; }
    else { sl.value = String(s.slowLen);
      if ((s.slowTop || []).length) { const top = s.slowTop.reduce((a, e) => e.durUs > a.durUs ? e : a, s.slowTop[0]);
        sl.value += `，最慢 ${(top.durUs / 1000).toFixed(1)} ms（${cmdName(top.args)}）`;
        sl.table = {columns: ['时间', '耗时(ms)', '命令（指纹）', '客户端'], rows: s.slowTop.slice(0, 5).map(e => [fmtTs(e.time), (e.durUs / 1000).toFixed(1), fingerprint(e.args), e.client])}; }
      sl.level = s.slowLen >= TH.slowAlert ? 2 : s.slowLen >= TH.slowAtt ? 1 : 0; }
    add(sl);
    const fk = {section: 'stats', key: 'fork', name: '最近一次 fork 耗时', threshold: `关注 ≥ ${TH.forkAtt / 1000} ms / 告警 ≥ ${TH.forkAlert / 1000} ms`, level: 0};
    if (!has(info.latest_fork_usec)) { fk.level = 3; fk.value = '未采集'; } else { fk.value = (info.latest_fork_usec / 1000).toFixed(1) + ' ms'; fk.level = info.latest_fork_usec >= TH.forkAlert ? 2 : info.latest_fork_usec >= TH.forkAtt ? 1 : 0; }
    add(fk); }

  /* 安全 */
  { const pw = {section: 'security', key: 'password', name: '访问密码', threshold: '必须设置 requirepass 或 ACL', level: 0};
    const rp = cfgGet('requirepass');
    if (has(rp) && rp !== '') pw.value = '已设置';
    else if (has(rp)) { if (s.hasPassword) pw.value = 'requirepass 为空，但平台使用密码 / ACL 账号连接成功'; else { pw.level = 2; pw.value = '未设置（无密码即可访问）'; } }
    else if (s.hasPassword) pw.value = '已配置连接密码（CONFIG GET 不可用，无法核对 requirepass）';
    else { pw.level = 2; pw.value = '平台未使用密码连接，且无法读取 requirepass：实例很可能没有设置密码'; }
    add(pw);
    const ex = {section: 'security', key: 'exposed', name: 'protected-mode 与绑定地址', threshold: '监听所有网卡时应开启 protected-mode', level: 0};
    const pm = cfgGet('protected-mode'), bind = cfgGet('bind');
    if (s.cfgErr || (!has(pm) && !has(bind))) { ex.level = 3; ex.value = '未采集（CONFIG GET 不可用）'; }
    else { const b = has(bind) ? String(bind).trim() : '', all = !has(bind) || b === '' || /0\.0\.0\.0|\*|::/.test(b);
      ex.value = `protected-mode ${pm || '未知'}；bind ${b || '(空)'}`;
      if (all && pm === 'no') ex.level = pw.level === 2 ? 2 : 1; }
    add(ex);
    const cm = {section: 'security', key: 'commands', name: '危险命令是否被改名', threshold: 'FLUSHALL / FLUSHDB / KEYS / CONFIG / DEBUG / SHUTDOWN 已改名或禁用', level: 0};
    if (s.cmdErr) { cm.level = 3; cm.value = '未采集：' + s.cmdErr; }
    else { const open = DANGEROUS.filter(c => s.cmdAvail[c.toLowerCase()]); if (!open.length) cm.value = '全部已改名或禁用'; else { cm.level = 1; cm.value = '仍可直接使用：' + open.join('、'); } }
    add(cm); }

  /* 键空间 */
  { const dbs = s.keyspace || [];
    const sum = {section: 'keyspace', key: 'summary', name: '键数量与过期键占比', threshold: `volatile-* 策略下带过期时间的 key 占比 ≥ ${TH.volatileMin}%`, level: 0};
    let keys = 0, exp = 0; const rows = [];
    dbs.forEach(d => { keys += d.keys; exp += d.expires; rows.push([d.name, d.keys, d.expires, (d.keys ? d.expires * 100 / d.keys : 0).toFixed(1) + '%', Math.floor(d.avgTtlMs / 1000)]); });
    if (rows.length) sum.table = {columns: ['库', 'key 数', '带过期时间', '占比', '平均 TTL(秒)'], rows};
    if (!keys) sum.value = '没有 key';
    else { const pct = exp * 100 / keys; sum.value = `${keys} 个 key，其中 ${exp} 个带过期时间（${pct.toFixed(1)}%）`;
      if (policy.startsWith('volatile-')) {
        if (exp === 0) { sum.level = 2; sum.value += `；淘汰策略 ${policy} 只淘汰带过期时间的 key，当前没有可淘汰的 key`; }
        else if (pct < TH.volatileMin) { sum.level = 1; sum.value += `；淘汰策略 ${policy} 下可淘汰的 key 太少`; } } }
    add(sum);
    const bk = {section: 'keyspace', key: 'bigkey', name: '大 key 抽样', threshold: `单 key ≥ ${bytes(TH.bigBytesAtt)} 关注 / ≥ ${bytes(TH.bigBytesAlert)} 告警`, level: 0};
    if (s.bigN <= 0) bk.value = '未开启（默认关闭；立即巡检时可勾选，或设置环境变量 REDIS_INSPECT_BIGKEY_SAMPLE）';
    else if (s.bigErr && (!s.bigKeys || s.bigKeys.scanned === 0)) { bk.level = 3; bk.value = '未采集：' + s.bigErr; }
    else { const res = s.bigKeys; bk.value = `抽样 ${res.scanned} 个 key（上限 ${s.bigN}），`;
      if (!res.exact) bk.threshold = `Redis 不支持 MEMORY USAGE：按元素个数判断，≥ ${TH.bigElemsAtt} 关注 / ≥ ${TH.bigElemsAlert} 告警`;
      if (!res.top.length) bk.value += '没有发现 key';
      else { const top = res.top[0];
        if (res.exact) { bk.value += `最大 ${bytes(top.bytes)}（${top.type}）`; bk.level = top.bytes >= TH.bigBytesAlert ? 2 : top.bytes >= TH.bigBytesAtt ? 1 : 0; }
        else { bk.value += `最大 ${top.elems} 个元素（${top.type}）`; bk.level = top.elems >= TH.bigElemsAlert ? 2 : top.elems >= TH.bigElemsAtt ? 1 : 0; }
        bk.table = {columns: ['库', 'key', '类型', '大小'], rows: res.top.map(k => [k.db, k.key.slice(0, 120), k.type, res.exact ? bytes(k.bytes) : `${k.elems} 个元素`])}; } }
    add(bk); }
  return out;
}

/* 评分（RedisHealth）：100 分起，关注 / 告警按 PEN 扣分，不低于 0，未采集不扣分；告警且扣分 ≥ 20 → critical */
function health(items, bigN) {
  let score = 100, status = 'healthy'; const issues = [], warnings = [], suggestions = [], seen = new Set();
  (items || []).forEach(c => {
    const key = c.sec + '.' + c.key, pen = PEN[key] || [0, 0], line = `[${secName(c.sec)}] ${c.name}：${c.val}`;
    if (c.lvl === 2) { issues.push(line); score -= pen[1]; if (pen[1] >= 20) status = 'critical'; }
    else if (c.lvl === 1) { warnings.push(line); score -= pen[0]; }
    else return;
    const adv = ADVICE[key];
    if (adv && !seen.has(key) && suggestions.length < 8) { seen.add(key); suggestions.push(c.name + '：' + adv); }
  });
  if (!bigN) suggestions.push('未做大 key 抽样：低峰期可在“立即巡检”里勾选“大 key 抽样”（限量，默认 1000 个 key）。');
  return {score: Math.max(0, score), status, issues, warnings, suggestions};
}

/* ---------- 各实例的“采集到的原始数据”（按实例档案 + 报告日龄 + 随机抖动生成）---------- */
const NOW_S = Math.floor(NOW_MS / 1000);
function slowEntry(r, time, durMs, args, ip, name) { return {time, durUs: Math.round(durMs * 1000), args, client: `${ip}:${R(r, 30000, 60999)}`, name}; }
function profileFor(inst, age, r, big) {
  const id = inst.id, ago = sec => NOW_S - age * 86400 - sec;               // 报告当时的“N 秒前”（age = 报告距今天数）
  const common = {tcp_port: inst.port, redis_mode: 'standalone', os: 'Linux 5.10.0-28-amd64 x86_64'};
  if (id === 11) {                                   // 主库：8GB maxmemory，内存缓慢上涨；有 1 个从库；有密码
    const used = Math.round((6.7 - 0.15 * age) * GB + R(r, -80, 80) * MB), frag = 1.24 + R(r, 0, 12) / 100, masterOff = 8812334120 - age * 41000000;
    const lag = age === 3 ? 14 : R(r, 0, 1);
    const info = Object.assign({}, common, {redis_version: inst.ver, role: 'master', uptime_in_seconds: (49 - age) * 86400 + R(r, 0, 80000), connected_slaves: 1, master_repl_offset: masterOff,
      slaves: [{addr: '10.20.1.32:6379', state: 'online', offset: masterOff - R(r, 0, 2400), lag}],
      used_memory: used, used_memory_rss: Math.round(used * frag), used_memory_peak: Math.round((age >= 3 ? 7.4 : 7.8) * GB), mem_fragmentation_ratio: frag, total_system_memory: 16 * GB, maxmemory: 8 * GB, maxmemory_policy: 'allkeys-lru',
      aof_enabled: 0, rdb_last_bgsave_status: 'ok', rdb_last_save_time: ago(R(r, 200, 3000)), rdb_changes_since_last_save: R(r, 2000, 90000),
      connected_clients: 160 + R(r, 0, 60), maxclients: 10000, blocked_clients: R(r, 0, 3), rejected_connections: 0, total_connections_received: 9882114 - age * 38000, instantaneous_ops_per_sec: 22000 + R(r, 0, 5000),
      keyspace_hits: 8612450221 - age * 62000000, keyspace_misses: 321908114 - age * 2300000, evicted_keys: Math.max(1200, 128000 - age * 4000), expired_keys: 96411203 - age * 700000, latest_fork_usec: 180000 + R(r, 0, 140000)});
    const slowTop = [slowEntry(r, ago(540), 420.3, ['KEYS', 'session:*'], '10.20.8.15', 'ops-script'), slowEntry(r, ago(1260), 72.4, ['ZRANGE', 'rank:daily:20261007', '0', '-1', 'WITHSCORES'], '10.20.7.41', 'rank-job'),
      slowEntry(r, ago(2100), 55.1, ['SMEMBERS', 'cart:u:1129384'], '10.20.7.22', 'cart-svc'), slowEntry(r, ago(3300), 38.2, ['HGETALL', 'order:detail:8821734'], '10.20.7.18', 'order-api'), slowEntry(r, ago(4100), 26.9, ['LRANGE', 'feed:u:77120', '0', '-1'], '10.20.7.33', 'feed-svc')];
    return {info, cfg: {requirepass: 'x', 'protected-mode': 'yes', bind: '10.20.1.31 127.0.0.1', save: '3600 1 300 100 60 10000', appendonly: 'no', maxclients: '10000'}, slowLen: R(r, 14, 40), slowTop,
      cmdAvail: {flushall: false, flushdb: false, keys: true, config: true, debug: false, shutdown: false}, hasPassword: true,
      keyspace: [{name: 'db0', keys: 12340221 + (30 - age) * 5200, expires: 11980004 + (30 - age) * 5100, avgTtlMs: 5400000}],
      bigKeys: big ? {scanned: 1000, exact: true, top: [{db: 'db0', key: 'activity:rank:20260930', type: 'zset', bytes: 3.4 * MB}, {db: 'db0', key: 'cart:u:1129384', type: 'set', bytes: 1.7 * MB}, {db: 'db0', key: 'feed:u:77120', type: 'list', bytes: 880 * 1024},
        {db: 'db0', key: 'order:detail:8821734', type: 'hash', bytes: 310 * 1024}, {db: 'db0', key: 'tag:hot:202610', type: 'set', bytes: 120 * 1024}]} : null};
  }
  if (id === 12) {                                   // 从库：用 ACL 账号巡检，没有 SLOWLOG 权限 → 慢日志条数“未采集”；age=2 时复制链路断过
    const used = Math.round((5.35 - 0.14 * age) * GB + R(r, -60, 60) * MB), frag = 1.08 + R(r, 0, 8) / 100, down = age === 2;
    const info = Object.assign({}, common, {redis_version: inst.ver, role: 'slave', uptime_in_seconds: (49 - age) * 86400 + R(r, 0, 60000), master_host: '10.20.1.31', master_port: 6379,
      master_link_status: down ? 'down' : 'up', master_last_io_seconds_ago: down ? -1 : R(r, 0, 2), master_sync_in_progress: 0, master_link_down_since_seconds: down ? 95 : -1,
      used_memory: used, used_memory_rss: Math.round(used * frag), used_memory_peak: Math.round(7.0 * GB), mem_fragmentation_ratio: frag, total_system_memory: 16 * GB, maxmemory: 8 * GB, maxmemory_policy: 'allkeys-lru',
      aof_enabled: 1, aof_last_write_status: 'ok', aof_last_bgrewrite_status: 'ok', aof_current_size: Math.round(3.1 * GB), rdb_last_bgsave_status: 'ok', rdb_last_save_time: ago(86400 * 6),
      connected_clients: 40 + R(r, 0, 25), maxclients: 10000, blocked_clients: 0, rejected_connections: 0, total_connections_received: 2210044 - age * 9000, instantaneous_ops_per_sec: 5000 + R(r, 0, 1800),
      keyspace_hits: 2041220877 - age * 14000000, keyspace_misses: 112300410 - age * 800000, evicted_keys: 0, expired_keys: 33120440 - age * 250000, latest_fork_usec: 210000 + R(r, 0, 60000)});
    return {info, cfg: {requirepass: 'x', 'protected-mode': 'yes', bind: '10.20.1.32 127.0.0.1', save: '', appendonly: 'yes', maxclients: '10000'}, slowErr: "NOPERM this user has no permissions to run the 'slowlog' command",
      cmdAvail: {flushall: false, flushdb: false, keys: true, config: true, debug: false, shutdown: false}, hasPassword: true,
      keyspace: [{name: 'db0', keys: 12338901 + (30 - age) * 5200, expires: 11978100 + (30 - age) * 5100, avgTtlMs: 5400000}],
      bigKeys: big ? {scanned: 1000, exact: true, top: [{db: 'db0', key: 'activity:rank:20260930', type: 'zset', bytes: 3.4 * MB}, {db: 'db0', key: 'cart:u:1129384', type: 'set', bytes: 1.7 * MB}]} : null};
  }
  if (id === 13) {                                   // 会话库：无密码、无 maxmemory、无持久化——多项告警
    const used = Math.round((13.4 - 0.2 * age) * GB + R(r, -90, 90) * MB), frag = 1.12 + R(r, 0, 5) / 100;
    const info = Object.assign({}, common, {redis_version: inst.ver, role: 'master', uptime_in_seconds: (112 - age) * 86400 + R(r, 0, 60000), connected_slaves: 0, master_repl_offset: 0,
      used_memory: used, used_memory_rss: Math.round(used * frag), used_memory_peak: Math.round(14.0 * GB), mem_fragmentation_ratio: frag, total_system_memory: 16 * GB, maxmemory: 0, maxmemory_policy: 'noeviction',
      aof_enabled: 0, rdb_last_bgsave_status: 'ok', rdb_last_save_time: ago(86400 * 40),
      connected_clients: 4300 + R(r, 0, 600), maxclients: 10000, blocked_clients: 0, rejected_connections: Math.max(1, 37 - age), total_connections_received: 482113992 - age * 2100000, instantaneous_ops_per_sec: 38000 + R(r, 0, 3000),
      keyspace_hits: 3310000000 - age * 24000000, keyspace_misses: 319000000 - age * 2100000, evicted_keys: 0, expired_keys: 512044120 - age * 3900000, latest_fork_usec: 0});
    const slowTop = [slowEntry(r, ago(300), 880.5, ['KEYS', 'sess:*'], '10.20.9.12', 'ops-script'), slowEntry(r, ago(720), 143.2, ['ZREMRANGEBYSCORE', 'sess:expire', '0', '1791340800'], '10.20.9.40', 'sess-gc'),
      slowEntry(r, ago(1500), 96.7, ['SMEMBERS', 'sess:online:2026100711'], '10.20.9.27', 'portal-api'), slowEntry(r, ago(2200), 41.0, ['HGETALL', 'sess:7f3a91c2e8'], '10.20.9.27', 'portal-api'), slowEntry(r, ago(3900), 33.8, ['DEL', 'sess:bulk:20261006'], '10.20.9.40', 'sess-gc')];
    return {info, cfg: {requirepass: '', 'protected-mode': 'yes', bind: '10.20.3.31', save: '', appendonly: 'no', maxclients: '10000'}, slowLen: age <= 1 ? 112 : R(r, 40, 95), slowTop,
      cmdAvail: {flushall: true, flushdb: true, keys: true, config: true, debug: true, shutdown: true}, hasPassword: false,
      keyspace: [{name: 'db0', keys: 28410117 - age * 30000, expires: 28401000 - age * 30000, avgTtlMs: 1380000}, {name: 'db3', keys: 12044, expires: 0, avgTtlMs: 0}],
      bigKeys: big ? {scanned: 1000, exact: true, top: [{db: 'db0', key: 'sess:online:2026100711', type: 'set', bytes: 12.8 * MB}, {db: 'db0', key: 'sess:bulk:20261006', type: 'list', bytes: 2.2 * MB}]} : null};
  }
  /* 其他（新增的）Redis 实例：一台健康的小实例 */
  const used = Math.round(900 * MB + R(r, 0, 80) * MB);
  const info = Object.assign({}, common, {redis_version: inst.ver, role: inst.role === 'slave' ? 'slave' : 'master', uptime_in_seconds: 12 * 86400 + R(r, 0, 70000), connected_slaves: 0, master_repl_offset: 0,
    master_host: '10.20.0.1', master_port: 6379, master_link_status: 'up', master_last_io_seconds_ago: 1, master_sync_in_progress: 0,
    used_memory: used, used_memory_rss: Math.round(used * 1.12), used_memory_peak: Math.round(used * 1.1), mem_fragmentation_ratio: 1.12, total_system_memory: 8 * GB, maxmemory: 4 * GB, maxmemory_policy: 'allkeys-lru',
    aof_enabled: 1, aof_last_write_status: 'ok', aof_last_bgrewrite_status: 'ok', aof_current_size: 600 * MB, rdb_last_bgsave_status: 'ok', rdb_last_save_time: ago(3600),
    connected_clients: 20 + R(r, 0, 10), maxclients: 10000, blocked_clients: 0, rejected_connections: 0, total_connections_received: 120000, instantaneous_ops_per_sec: 800 + R(r, 0, 300),
    keyspace_hits: 90000000, keyspace_misses: 3000000, evicted_keys: 0, expired_keys: 1000000, latest_fork_usec: 90000});
  return {info, cfg: {requirepass: 'x', 'protected-mode': 'yes', bind: inst.host, save: '3600 1 300 100 60 10000', appendonly: 'yes', maxclients: '10000'}, slowLen: 2, slowTop: [slowEntry(r, ago(4000), 11.2, ['SMEMBERS', 'tags:all'], '10.20.0.9', 'app')],
    cmdAvail: {flushall: false, flushdb: false, keys: false, config: false, debug: false, shutdown: false}, hasPassword: true, keyspace: [{name: 'db0', keys: 220000, expires: 180000, avgTtlMs: 3600000}], bigKeys: null};
}

/* 生成一份报告的检查项（与 p_inst.js 里 MySQL / SQL Server 的 items 同形：cat / name / th / st / val / adv，另带 sec / key / lvl / table） */
function genItems(inst, seed, opt) {
  opt = opt || {};
  const at = (opt.at || TODAY).slice(0, 10), age = Math.max(0, Math.min(30, daysBetween(at, TODAY))), r = rng(seed);
  const snap = profileFor(inst, age, r, !!opt.bigkey);
  snap.nowS = NOW_S - age * 86400; snap.bigN = opt.bigkey ? 1000 : 0; snap.hasPassword = !!snap.hasPassword;
  const checks = evalRedis(snap);
  const items = checks.map(c => ({cat: secName(c.section), sec: c.section, key: c.key, name: c.name, th: c.threshold, lvl: c.level, st: ST[c.level], val: c.value, adv: c.level ? (c.advice || ADVICE_FAILED) : '', table: c.table || null, scope: ''}));
  const keys = (snap.keyspace || []).reduce((a, d) => a + d.keys, 0), i = snap.info;
  items.server = {version: i.redis_version, mode: i.redis_mode, role: i.role, uptime: i.uptime_in_seconds, used: i.used_memory, maxmemory: i.maxmemory || 0, keys, clients: i.connected_clients, ops: i.instantaneous_ops_per_sec, os: i.os};
  items.bigN = snap.bigN;
  return items;
}

const STEPS = ['INFO', 'CONFIG GET（逐项）', 'SLOWLOG LEN / GET', 'COMMAND INFO（危险命令是否改名）', '大 key 抽样'];
const BADGE = {ok: 'ok', warn: 'warn', bad: 'bad'};
const miniTable = t => tbl(t.columns, t.rows, {cells: r => r.map(c => `<span class="mono small">${esc(c)}</span>`)});
const scoreColor = s => s >= 85 ? 'var(--ok)' : s >= 70 ? 'var(--warn)' : 'var(--bad)';

/* 报告详情主体（running / failed / cancelled / finish 都在这里处理） */
function detailBody(rep, ctx) {
  const inst = ctx.inst;
  if (rep.status === 'running') {
    const doneSteps = STEPS.slice(0, rep.done);
    return `<div class="card"><h2>巡检进行中</h2><div class="row" style="gap:16px"><div style="flex:1">${meter(Math.round(rep.done / rep.total * 100))}</div><b>${rep.done} / ${rep.total} 步</b><span class="muted">耗时 ${rep.dur}</span></div>
      <div class="small muted" style="margin-top:8px">已完成：${doneSteps.map(esc).join(' → ') || '准备中…'}</div></div>`;
  }
  if (rep.status === 'failed') return notice(`巡检失败：<b>${esc(rep.err)}</b>`, 'bad', '✗');
  if (rep.status === 'cancelled') return notice(`巡检已取消，仅完成 ${rep.done} / ${rep.total} 步，没有产出检查项。`, 'warn', '!');
  const items = rep.items, bad = items.filter(x => x.st === 'bad'), warn = items.filter(x => x.st === 'warn'), hp = health(items, items.bigN), sv = items.server || {};
  const hist = (window.INSPECT_REPORTS || []).filter(x => x.inst === rep.inst && x.status === 'finish').sort((a, b) => a.at < b.at ? -1 : 1);
  const facts = [['版本', `Redis ${esc(sv.version)} · ${esc(sv.mode)}`], ['角色', sv.role === 'slave' ? 'slave（从库）' : 'master（主库）'], ['运行时长', esc(durText(sv.uptime || 0))],
    ['内存', sv.maxmemory > 0 ? `${bytes(sv.used)} / maxmemory ${bytes(sv.maxmemory)}` : `${bytes(sv.used)}（未设置 maxmemory）`], ['key 数 / 连接数', `${fi(sv.keys)} / ${sv.clients}`], ['每秒命令数', fi(sv.ops || 0)],
    ['大 key 抽样', items.bigN ? `已开启（抽样 ${items.bigN} 个 key）` : '未开启']];
  const blocks = [['bad', '需处理的问题', hp.issues, '✗'], ['warn', '需要关注', hp.warnings, '!'], ['ok', '优化建议', hp.suggestions, '✓']];
  return `<div class="grid g-main"><div class="card"><div class="score">${ring(rep.score)}<div style="flex:1"><div class="stat"><div><b style="color:var(--bad)">${rep.bad}</b><span>告警</span></div><div><b style="color:var(--warn)">${rep.warn}</b><span>关注</span></div><div><b style="color:var(--ok)">${rep.ok}</b><span>正常</span></div><div><b>${rep.na}</b><span>未采集</span></div></div>
      ${hp.status === 'critical' ? '<div class="small" style="margin-top:8px"><b style="color:var(--bad)">存在单项扣 ≥ 20 分的告警，状态为 critical。</b></div>' : ''}</div></div>
      ${hist.length > 1 ? `<div style="margin-top:10px"><div class="small muted">该实例健康分趋势</div>${lineChart([{name: '健康分', color: 'var(--c2)', data: hist.map(x => x.score)}], {h: 120, xl: [hist[0].at.slice(5, 10), hist[Math.floor(hist.length / 2)].at.slice(5, 10), hist[hist.length - 1].at.slice(5, 10)]})}</div>` : ''}</div>
    <div class="card"><h2>报告概况</h2>${kvList(facts)}</div></div>
    ${blocks.map(([cls, title, list, ic]) => list.length ? notice(`<b>${title}</b><ol style="margin:4px 0 0;padding-left:20px">${list.map(x => `<li>${esc(x)}</li>`).join('')}</ol>`, cls, ic) : '').join('')}
    ${SECTIONS.map(([code, name]) => { const l = items.filter(x => x.sec === code); if (!l.length) return ''; const b = l.filter(x => x.st === 'bad').length, w = l.filter(x => x.st === 'warn').length, na = l.filter(x => x.st === 'na').length;
      return `<details class="card" style="margin-top:12px" ${b || w || na ? 'open' : ''}><summary style="cursor:pointer;font-weight:600">${esc(name)} ${b ? `<span class="tag bad">告警 ${b}</span>` : ''}${w ? `<span class="tag warn">关注 ${w}</span>` : ''}${na ? `<span class="tag">未采集 ${na}</span>` : ''}${!b && !w && !na ? '<span class="tag ok">正常</span>' : ''} <span class="small muted" style="font-weight:400">${l.length} 项</span></summary><div style="margin-top:10px">${
        tbl(['检查项', '当前值', '阈值', '结果', '建议'], l, {attrs: x => x.st === 'bad' ? ' style="background:var(--bad-soft)"' : x.st === 'warn' ? ' style="background:var(--warn-soft)"' : '',
          cells: x => [`<b>${esc(x.name)}</b>`, `<span class="mono small">${esc(x.val)}</span>${x.table ? `<div style="margin-top:6px">${miniTable(x.table)}</div>` : ''}`, `<span class="small muted">${esc(x.th)}</span>`, x.st === 'na' ? tag('未采集') : tagOf(x.st), `<span class="small">${esc(x.adv || '—')}</span>`]})}</div></details>`; }).join('')}`;
}

window.RedisInspect = {genItems, health, detailBody, miniTable, steps: STEPS.length, stepNames: STEPS, fingerprint};

/* ============================================================================
   二、慢查询：数据生成（按指纹 × 天；种子固定），面板在文件后半
   ========================================================================== */
/* base：[每天次数, 平均耗时 ms]；shape：趋势（flat / grow / spike / new / decline）；batch：周末明显减少；until：这么多天之前停止出现（采集权限丢失）
   full(r)：生成一条完整命令的参数数组（key 名是业务样例，真实环境里完整命令只有有查看权限的人能看到） */
const A = (...a) => a;
const sess = r => R(r, 1000000, 9999999);
const SPECS = [
  {inst: 11, shape: 'grow', base: [180, 38], since: 120, who: 'order-api', ip: '10.20.7.18', full: r => A('HGETALL', 'order:detail:' + sess(r))},
  {inst: 11, shape: 'spike', base: [6, 420], since: 70, who: 'ops-script', ip: '10.20.8.15', full: r => A('KEYS', ['session:*', 'cart:u:*', 'order:detail:88*'][R(r, 0, 2)])},
  {inst: 11, shape: 'flat', base: [90, 55], since: 140, who: 'cart-svc', ip: '10.20.7.22', full: r => A('SMEMBERS', 'cart:u:' + sess(r))},
  {inst: 11, shape: 'flat', base: [45, 27], since: 100, who: 'feed-svc', ip: '10.20.7.33', full: r => A('LRANGE', 'feed:u:' + sess(r), '0', '-1')},
  {inst: 11, shape: 'flat', base: [28, 72], since: 90, batch: true, who: 'rank-job', ip: '10.20.7.41', full: r => A('ZRANGE', 'rank:daily:' + dayOffset(-R(r, 0, 5)).replace(/-/g, ''), '0', '-1', 'WITHSCORES')},
  {inst: 11, shape: 'grow', base: [210, 14], since: 150, who: 'order-api', ip: '10.20.7.18', full: r => A('SET', 'order:snap:' + sess(r), `{"id":${sess(r)},"items":[{"sku":9981,"qty":2},{"sku":10442,"qty":1},{"sku":2231,"qty":5}... (${R(r, 2800, 9800)} more bytes)`, 'EX', '3600')},
  {inst: 11, shape: 'flat', base: [60, 19], since: 110, who: 'price-svc', ip: '10.20.7.52', full: r => A('MGET', ...Array.from({length: 31}, (_, k) => 'sku:price:' + (100000 + R(r, 0, 899999) + k)), `... (${R(r, 4, 40)} more arguments)`)},
  {inst: 11, shape: 'flat', base: [3, 160], since: 60, who: 'ops-script', ip: '10.20.8.15', full: r => A('DEL', ['activity:rank:20260930', 'activity:rank:20260929', 'tag:hot:202609'][R(r, 0, 2)])},
  {inst: 11, shape: 'flat', base: [8, 95], since: 80, batch: true, who: 'tag-job', ip: '10.20.7.60', full: r => A('SUNIONSTORE', 'tag:merge:tmp', 'tag:hot:202610', 'tag:hot:202609')},
  {inst: 11, shape: 'new', base: [40, 31], since: 9, who: 'seckill-svc', ip: '10.20.7.71', full: r => A('EVALSHA', 'a8f5f167f44f4964e6c998dee827110c', '2', 'stock:sku:' + R(r, 1000, 9999), 'lock:sku:' + R(r, 1000, 9999), '1')},
  {inst: 11, shape: 'flat', base: [14, 48], since: 50, who: 'ops-script', ip: '10.20.8.15', full: r => A('SCAN', String(R(r, 0, 99999)), 'MATCH', 'order:*', 'COUNT', '10000')},
  {inst: 11, shape: 'new', base: [2, 18], since: 6, who: 'order-api-new', ip: '10.20.7.80', full: () => A('AUTH', '***')},
  /* 12：从库，ACL 账号缺 SLOWLOG 权限后停止采集（约 9 天前） */
  {inst: 12, shape: 'flat', base: [26, 33], since: 60, until: 9, who: 'report-ro', ip: '10.20.7.90', full: r => A('HGETALL', 'order:detail:' + sess(r))},
  {inst: 12, shape: 'flat', base: [4, 210], since: 60, until: 9, batch: true, who: 'report-ro', ip: '10.20.7.90', full: r => A('KEYS', 'order:detail:88*')},
  {inst: 12, shape: 'flat', base: [11, 41], since: 60, until: 9, who: 'report-ro', ip: '10.20.7.91', full: r => A('SMEMBERS', 'tag:hot:202609')},
  /* 13：会话库 */
  {inst: 13, shape: 'grow', base: [240, 41], since: 100, who: 'portal-api', ip: '10.20.9.27', full: r => A('HGETALL', 'sess:' + R(r, 0x10000000, 0xfffffffff).toString(16))},
  {inst: 13, shape: 'flat', base: [9, 880], since: 90, who: 'ops-script', ip: '10.20.9.12', full: r => A('KEYS', 'sess:*')},
  {inst: 13, shape: 'spike', base: [34, 143], since: 80, who: 'sess-gc', ip: '10.20.9.40', full: r => A('ZREMRANGEBYSCORE', 'sess:expire', '0', String(1791340800 - R(r, 0, 86400)))},
  {inst: 13, shape: 'flat', base: [70, 97], since: 120, who: 'portal-api', ip: '10.20.9.27', full: r => A('SMEMBERS', 'sess:online:' + dayOffset(-R(r, 0, 3)).replace(/-/g, '') + pad2(R(r, 0, 23)))},
  {inst: 13, shape: 'flat', base: [5, 34], since: 70, batch: true, who: 'sess-gc', ip: '10.20.9.40', full: r => A('DEL', 'sess:bulk:' + dayOffset(-R(r, 0, 6)).replace(/-/g, ''))},
  {inst: 13, shape: 'new', base: [52, 22], since: 11, who: 'portal-api', ip: '10.20.9.28', full: r => A('MGET', ...Array.from({length: 8}, () => 'sess:' + R(r, 0x10000000, 0xfffffffff).toString(16)))}];
SPECS.forEach((s, i) => { s.n = i + 1; s.id = 'rf' + String(i + 1).padStart(2, '0'); const a = s.full(rng(7 + i)); s.name = cmdName(a); s.fp = fingerprint(a); s.fpId = hash12(s.fp + '|' + s.inst); s.client = s.who; });

/* 单次耗时分布：94% 均匀分布在 [0.6, 1.4]×平均，6% 长尾 [1.4, 3.6]×平均——耗时下限筛选对统计与明细一致 */
const MIX = [[.94, .6, 1.4], [.06, 1.4, 3.6]];
function frac(x) { let mass = 0, sum = 0; MIX.forEach(([w, lo, hi]) => { if (x >= hi) return; const l = Math.max(lo, x), m = w * (hi - l) / (hi - lo); mass += m; sum += m * (l + hi) / 2; }); return [mass, sum]; }
function sampleDur(r, avg, minMs) {
  for (let t = 0; t < 10; t++) { const u = r(), p = u < MIX[0][0] ? MIX[0] : MIX[1], m = p[1] + (p[2] - p[1]) * r(), d = avg * m; if (d >= minMs) return d; }
  return 0;
}
const DAY_CACHE = {};
function dayBase(s, k) {                         // k = 距今天数
  if (k > s.since || (s.until && k < s.until)) return null;
  const key = s.id + ':' + k; if (key in DAY_CACHE) return DAY_CACHE[key];
  const r = rng(s.n * 7919 + k * 104729 + 29), dow = new Date(dayOffset(-k) + 'T00:00:00').getDay(), wk = dow === 0 || dow === 6 ? (s.batch ? .2 : .7) : 1;
  let f = 1, g = 1;
  if (s.shape === 'grow') f = Math.max(.45, 1 - .0055 * k); else if (s.shape === 'decline') f = .55 + .45 * Math.min(1, k / 60);
  else if (s.shape === 'spike' && k >= 1 && k <= 4) { f = 3.2; g = 1.6; } else if (s.shape === 'new') f = 1 + (s.since - k) * .04;
  const cnt = Math.max(0, Math.round(s.base[0] * f * wk * (.75 + .5 * r()) * (k === 0 ? .55 : 1)));
  return DAY_CACHE[key] = cnt > 0 ? {cnt, avg: s.base[1] * g * (.85 + .3 * r()), maxm: 2.2 + 1.4 * r()} : null;
}
const EXTRA = [];                                 // “立即采集 / 重置”新采集入库的记录：{s, at, dur}
function dayAgg(s, k, minMs) {                    // 某指纹某天（含手动采集新增）在耗时下限下的 {cnt, total, max}
  const d = dayBase(s, k); let cnt = 0, total = 0, max = 0;
  if (d) { const [mass, sum] = frac(minMs / d.avg); cnt = Math.round(d.cnt * mass); total = d.cnt * sum * d.avg; max = cnt && d.avg * d.maxm >= minMs ? d.avg * d.maxm : 0; if (!max && cnt) max = minMs * 1.05; }
  if (k === 0) EXTRA.filter(e => e.s === s && e.dur >= minMs).forEach(e => { cnt++; total += e.dur; max = Math.max(max, e.dur); });
  return cnt ? {cnt, total, max, avg: total / cnt} : null;
}
const HOUR_W = [.4, .3, .25, .25, .3, .5, 1, 2, 3.5, 4.5, 5, 5, 4.5, 4.8, 5, 5, 4.8, 4.2, 3.5, 3, 2.5, 2, 1.2, .7], HOUR_SUM = HOUR_W.reduce((a, b) => a + b, 0);
function secOfDay(r, k) { const lim = k === 0 ? 40000 : 86399; for (let t = 0; t < 12; t++) { const h = R(r, 0, 23); if (r() * 5 < HOUR_W[h]) { const sec = h * 3600 + R(r, 0, 3599); if (sec <= lim) return sec; } } return R(r, 0, lim); }
const hms = sec => `${pad2(Math.floor(sec / 3600))}:${pad2(Math.floor(sec % 3600 / 60))}:${pad2(sec % 60)}`;
const clientOf = (s, r) => `${s.ip}:${R(r, 30000, 60999)}`;
function recordsOf(s, k, minMs) {                 // 某指纹某天的明细样本（每天最多 2–4 条）
  const d = dayBase(s, k), out = [];
  if (d) { const r = rng(s.n * 31337 + k * 977 + 5), want = Math.min(dayAgg(s, k, minMs) ? dayAgg(s, k, minMs).cnt : 0, d.cnt > 100 ? 4 : 2);
    for (let j = 0; j < want; j++) { const dur = sampleDur(r, d.avg, minMs); if (!dur) continue; const args = s.full(r); out.push({s, at: dayOffset(-k) + ' ' + hms(secOfDay(r, k)), dur, args, text: cmdText(args), client: clientOf(s, r), cname: s.who}); } }
  if (k === 0) EXTRA.filter(e => e.s === s && e.dur >= minMs).forEach(e => out.push({s, at: e.at, dur: e.dur, args: e.args, text: cmdText(e.args), client: e.client, cname: s.who}));
  return out;
}
const sampleAt = (s, k) => { const r = rng(s.n * 55 + k * 3 + 1); return dayOffset(-k) + ' ' + hms(secOfDay(r, k)); };

/* ---------- 页面状态 ---------- */
const RQ = {preset: '7', from: dayOffset(-6), to: TODAY, cmd: '', min: '', kw: '', sort: 'cnt', dir: -1, open: new Set(), fp: '', page: 1, dsort: 'at', dpage: 1, msg: null};
const MIN_OPTS = [['', '耗时：不限'], ['1', '耗时 ≥ 1 ms'], ['5', '耗时 ≥ 5 ms'], ['10', '耗时 ≥ 10 ms'], ['50', '耗时 ≥ 50 ms'], ['100', '耗时 ≥ 100 ms'], ['500', '耗时 ≥ 500 ms'], ['1000', '耗时 ≥ 1 s']];
const PRESETS = [['0', '今日'], ['7', '最近 7 天'], ['31', '最近 31 天'], ['93', '最近 93 天（上限）'], ['custom', '自定义']];
const spanDays = () => daysBetween(RQ.from, RQ.to) + 1;
const minMs = () => RQ.min === '' ? 0 : +RQ.min;
/* 实例上当前的慢日志（环形缓冲）与尚未采集的条数；采集状态 */
const LIVE = {11: {len: 96, pending: 3}, 12: {len: 40, pending: 0}, 13: {len: 128, pending: 7}};
const live = id => LIVE[id] || (LIVE[id] = {len: 0, pending: 0});
const STATE = {
  11: {status: 'ok', lastRun: TODAY + ' 11:19:12', lastOk: TODAY + ' 11:19:12', lastRows: 2, error: '', slowId: 88231, slowTime: TODAY + ' 11:16:42', manual: 0},
  12: {status: 'error', lastRun: TODAY + ' 11:19:30', lastOk: dayOffset(-9) + ' 08:41:05', lastRows: 0, error: "SLOWLOG LEN：NOPERM this user has no permissions to run the 'slowlog' command（采集账号需要 SLOWLOG 权限；失败后指数退避，最长 10 分钟）", slowId: 40117, slowTime: dayOffset(-9) + ' 08:40:51', manual: 0},
  13: {status: 'ok', lastRun: TODAY + ' 11:19:44', lastOk: TODAY + ' 11:19:44', lastRows: 5, error: '', slowId: 502994, slowTime: TODAY + ' 11:18:03', manual: 0}};
const stateOf = id => STATE[id] || (STATE[id] = {status: 'ok', lastRun: '—', lastOk: '—', lastRows: 0, error: '', slowId: -1, slowTime: '', manual: 0});
function totalRows(id) { let n = 0; SPECS.filter(s => s.inst === id).forEach(s => { for (let k = 0; k <= 93; k++) { const d = dayBase(s, k); if (d) n += d.cnt; } }); return n + EXTRA.filter(e => e.s.inst === id).length; }

function matched() {                              // 当前筛选下的各指纹聚合行
  const kw = RQ.kw.trim().toLowerCase(), mm = minMs(), rows = [];
  SPECS.filter(s => s.inst === Q_INST && (!RQ.cmd || s.name === RQ.cmd) && (!kw || s.fp.toLowerCase().includes(kw) || s.name.toLowerCase().includes(kw) || s.ip.includes(kw) || s.who.toLowerCase().includes(kw))).forEach(s => {
    const a = {s, cnt: 0, total: 0, max: 0, first: '', last: '', series: []};
    for (let k = daysBetween(RQ.to, TODAY); k <= daysBetween(RQ.from, TODAY); k++) {
      const d = dayAgg(s, k, mm); a.series.unshift(d ? d.cnt : 0); if (!d) continue;
      a.cnt += d.cnt; a.total += d.total; a.max = Math.max(a.max, d.max);
      const t = k === 0 ? (EXTRA.filter(e => e.s === s).map(e => e.at).sort().pop() || sampleAt(s, 0)) : sampleAt(s, k);
      if (!a.last) a.last = t; a.first = t;
    }
    if (a.cnt) { a.avg = a.total / a.cnt; rows.push(a); }
  });
  return rows;
}
let Q_INST = 0;
const fmtMsR = v => v >= 1000 ? (v / 1000).toFixed(2) + ' s' : v >= 100 ? Math.round(v) + ' ms' : (Math.round(v * 100) / 100) + ' ms';
const fmtDurR = ms => ms >= 3.6e6 ? (ms / 3.6e6).toFixed(1) + ' h' : ms >= 6e4 ? (ms / 6e4).toFixed(1) + ' min' : ms >= 1000 ? (ms / 1000).toFixed(2) + ' s' : Math.round(ms) + ' ms';
const clip = (s, n) => s.length > n ? s.slice(0, n) + '…' : s;

/* 趋势：柱 = 慢命令次数，线 = 平均耗时（毫秒）；bucket = day / hour */
function trendPoints(specs, bucket) {
  const mm = minMs(), pts = [], days = [];
  for (let k = daysBetween(RQ.to, TODAY) + daysBetween(RQ.from, RQ.to); k >= daysBetween(RQ.to, TODAY); k--) days.push(k);
  days.forEach(k => {
    let cnt = 0, total = 0, max = 0; specs.forEach(s => { const d = dayAgg(s, k, mm); if (d) { cnt += d.cnt; total += d.total; max = Math.max(max, d.max); } });
    if (bucket === 'day') { pts.push({t: dayOffset(-k), count: cnt, avg_ms: cnt ? total / cnt : 0, max_ms: max, total_ms: total}); return; }
    for (let h = 0; h < 24; h++) { if (k === 0 && h > 11) break; const c = Math.round(cnt * HOUR_W[h] / HOUR_SUM); pts.push({t: dayOffset(-k) + ' ' + pad2(h) + ':00', count: c, avg_ms: c ? total / Math.max(1, cnt) : 0, max_ms: c ? max : 0, total_ms: c ? total / Math.max(1, cnt) * c : 0}); }
  });
  return pts;
}
function trendSvg(points, W = 720) {
  const H = 150, L = 44, Rr = 52, T = 12, Bm = 26;
  if (!points.length || points.every(p => !p.count)) return `<svg viewBox="0 0 ${W} ${H}" style="width:100%;height:auto"><text x="${W / 2}" y="${H / 2}" text-anchor="middle" font-size="12" fill="var(--muted)">该时间范围内没有数据</text></svg>`;
  const pw = W - L - Rr, ph = H - T - Bm, maxC = Math.max(...points.map(p => p.count), 1), maxA = Math.max(...points.map(p => p.avg_ms), 1e-6), slot = pw / points.length, bw = Math.max(2, Math.min(26, slot * .6));
  let bars = '', line = [];
  points.forEach((p, i) => { const cx = L + slot * (i + .5), h = p.count / maxC * ph;
    bars += `<rect x="${(cx - bw / 2).toFixed(1)}" y="${(T + ph - h).toFixed(1)}" width="${bw.toFixed(1)}" height="${Math.max(h, 1).toFixed(1)}" rx="1" fill="var(--c1)" opacity=".7"><title>${esc(p.t)}\n慢命令 ${p.count} 次\n平均 ${fmtMsR(p.avg_ms)}，最大 ${fmtMsR(p.max_ms)}\n累计 ${fmtMsR(p.total_ms)}</title></rect>`;
    line.push([cx, T + ph - p.avg_ms / maxA * ph]); });
  const idx = [...new Set([0, Math.floor((points.length - 1) / 2), points.length - 1])];
  return `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Redis 慢命令次数与平均耗时趋势" style="width:100%;height:auto"><line x1="${L}" y1="${T + ph}" x2="${W - Rr}" y2="${T + ph}" stroke="var(--border)"/>${bars}
    ${line.length > 1 ? `<polyline fill="none" stroke="var(--c3)" stroke-width="1.8" points="${line.map(q => q.map(v => v.toFixed(1)).join(',')).join(' ')}"/>` : `<circle cx="${line[0][0]}" cy="${line[0][1]}" r="3" fill="var(--c3)"/>`}
    <text x="${L - 4}" y="${T + 8}" text-anchor="end" font-size="10" fill="var(--muted)">${fi(maxC)}</text><text x="${L - 4}" y="${T + ph}" text-anchor="end" font-size="10" fill="var(--muted)">0</text><text x="${W - Rr + 4}" y="${T + 8}" font-size="10" fill="var(--muted)">${fmtMsR(maxA)}</text>
    ${idx.map(i => `<text x="${L + slot * (i + .5)}" y="${H - 8}" text-anchor="${i === 0 ? 'start' : i === points.length - 1 ? 'end' : 'middle'}" font-size="10" fill="var(--muted)">${esc(points[i].t.slice(5))}</text>`).join('')}</svg>`;
}
const downsample = (arr, n) => { if (arr.length <= n) return arr; const out = []; for (let i = 0; i < n; i++) { const a = Math.floor(i * arr.length / n), b = Math.floor((i + 1) * arr.length / n); out.push(arr.slice(a, b).reduce((x, y) => x + y, 0)); } return out; };

/* ---------- 面板 ---------- */
const kpi = (v, l, d) => `<div class="card kpi"><div class="v">${v}</div><div class="l">${l}</div>${d ? `<div class="d muted">${d}</div>` : ''}</div>`;
const sortTh = (k, t) => `<a data-on="rqSort" data-k="${k}">${t}${RQ.sort === k ? (RQ.dir < 0 ? ' ▾' : ' ▴') : ''}</a>`;
function cmdNames() { const set = new Set(); SPECS.filter(s => s.inst === Q_INST).forEach(s => { for (let k = daysBetween(RQ.to, TODAY); k <= daysBetween(RQ.from, TODAY); k++) if (dayBase(s, k)) { set.add(s.name); break; } }); return [...set].sort(); }

function panelStat() {
  if (spanDays() > 93) return notice('查询时间范围最长 93 天，请缩小范围后再查。', 'bad', '✕');
  const rows = matched(), val = {cmd: a => a.s.name, cnt: a => a.cnt, total: a => a.total, avg: a => a.avg, max: a => a.max, first: a => a.first, last: a => a.last};
  rows.sort((a, b) => { const x = val[RQ.sort](a), y = val[RQ.sort](b); return (x < y ? -1 : x > y ? 1 : 0) * RQ.dir; });
  const tot = rows.reduce((a, r) => ({cnt: a.cnt + r.cnt, ms: a.ms + r.total, max: Math.max(a.max, r.max)}), {cnt: 0, ms: 0, max: 0});
  const size = 15, np = Math.max(1, Math.ceil(rows.length / size)), pg = Math.min(RQ.page, np), cur = rows.slice((pg - 1) * size, pg * size), bucket = spanDays() <= 3 ? 'hour' : 'day';
  const heads = ['', sortTh('cmd', '命令'), '命令指纹', sortTh('cnt', '次数'), sortTh('total', '总耗时'), sortTh('avg', '平均'), sortTh('max', '最大'), sortTh('first', '首次出现'), sortTh('last', '最近出现'), '趋势', '操作'];
  const body = [];
  cur.forEach(a => {
    const s = a.s, open = RQ.open.has(s.id);
    body.push(`<tr class="click"><td><a data-on="rqOpen" data-id="${s.id}" title="展开完整命令与趋势">${open ? '▾' : '▸'}</a></td><td class="mono nowrap">${esc(s.name)}</td>
      <td><a data-on="rqToDet" data-id="${s.id}"><div class="op-sql mono">${esc(s.fp)}</div></a><div class="small muted mono">${s.fpId}</div></td>
      <td class="op-num">${fi(a.cnt)}</td><td class="op-num">${fmtDurR(a.total)}</td><td class="op-num"><b>${fmtMsR(a.avg)}</b></td><td class="op-num">${fmtMsR(a.max)}</td>
      <td class="small nowrap">${esc(a.first.slice(5, 16))}</td><td class="small nowrap">${esc(a.last.slice(5, 16))}</td><td>${spark(a.series.length > 1 ? downsample(a.series, 14) : [0, a.cnt], 'var(--c1)', 84, 24)}</td>
      <td><button class="btn sm" data-copy="${esc(s.fp)}">复制指纹</button></td></tr>`);
    if (open) {
      const recs = []; for (let k = daysBetween(RQ.to, TODAY); k <= daysBetween(RQ.from, TODAY) && recs.length < 1; k++) recs.push(...recordsOf(s, k, minMs()));
      const sample = recs.sort((x, y) => x.at < y.at ? 1 : -1)[0];
      body.push(`<tr><td></td><td colspan="10" style="background:var(--panel2)"><div class="grid g2" style="align-items:start"><div><div class="small muted" style="margin-bottom:4px">最近一条的完整命令${sample ? '（' + esc(sample.at) + ' · ' + fmtMsR(sample.dur) + '）' : ''}</div><pre class="code op-pre">${esc(sample ? sample.text : s.fp)}</pre>
        <div class="small muted">客户端 <span class="mono">${esc(s.ip)}</span> · ${esc(s.who)}</div>
        <div class="toolbar" style="margin-top:8px"><button class="btn sm" data-on="rqToDet" data-id="${s.id}">查看明细</button></div></div>
        <div><div class="small muted">该命令的历史趋势 <span class="muted">次数（柱）· 平均耗时（线）</span></div>${trendSvg(trendPoints([s], bucket))}</div></div></td></tr>`);
    }
  });
  const pager = `<div class="pager"><span>共 ${rows.length} 个命令指纹</span>${Array.from({length: np}, (_, i) => `<a data-on="rqPage" data-p="${i + 1}" class="${i + 1 === pg ? 'on' : ''}">${i + 1}</a>`).join('')}</div>`;
  const allSpecs = SPECS.filter(s => rows.some(r => r.s === s));
  return `<div class="grid g4" style="margin-bottom:14px">${kpi(rows.length, '命令指纹数', `${spanDays()} 天内`)}${kpi(fi(tot.cnt), '慢命令总次数', '')}${kpi(fmtDurR(tot.ms), '总耗时', tot.cnt ? `平均 ${fmtMsR(tot.ms / tot.cnt)}` : '')}${kpi(fmtMsR(tot.max), '最慢一次', '')}</div>
    <div class="card" style="margin-bottom:14px"><div class="muted small">慢命令次数（柱）· 平均耗时（线，毫秒）</div>${trendSvg(trendPoints(allSpecs, bucket), 1180)}</div>
    <div class="tbl-wrap"><table class="op-th"><thead><tr>${heads.map(h => `<th>${h}</th>`).join('')}</tr></thead><tbody>${body.length ? body.join('') : `<tr><td colspan="${heads.length}">${empty('该时间范围内没有 Redis 慢命令（可放宽耗时下限或扩大时间范围）')}</td></tr>`}</tbody></table></div>${pager}`;
}
function detailRecs() {
  const kw = RQ.kw.trim().toLowerCase(), mm = minMs(); let recs = [];
  SPECS.filter(s => s.inst === Q_INST && (!RQ.cmd || s.name === RQ.cmd) && (!RQ.fp || s.id === RQ.fp) && (!kw || s.fp.toLowerCase().includes(kw) || s.name.toLowerCase().includes(kw) || s.ip.includes(kw) || s.who.toLowerCase().includes(kw))).forEach(s => {
    for (let k = daysBetween(RQ.to, TODAY); k <= daysBetween(RQ.from, TODAY); k++) recs.push(...recordsOf(s, k, mm)); });
  return recs.sort((a, b) => RQ.dsort === 'dur' ? b.dur - a.dur : (a.at < b.at ? 1 : -1));
}
function panelDet() {
  if (spanDays() > 93) return notice('查询时间范围最长 93 天，请缩小范围后再查。', 'bad', '✕');
  const all = detailRecs(), total = Math.min(all.length, 300), recs = all.slice(0, 300), size = 20, np = Math.max(1, Math.ceil(total / size)), pg = Math.min(RQ.dpage, np), cur = recs.slice((pg - 1) * size, pg * size);
  const chip = RQ.fp ? SPECS.find(s => s.id === RQ.fp) : null, th = (k, t) => `<a data-on="rqDSort" data-k="${k}">${t}${RQ.dsort === k ? ' ▾' : ''}</a>`;
  return `${chip ? `<div class="notice"><span>⌕</span><div>按命令指纹过滤：<span class="mono">${esc(chip.fp)}</span> <span class="small muted mono">${chip.fpId}</span>　<a data-on="rqClearFp">清除指纹过滤 ✕</a></div></div>` : ''}
    <div class="tbl-wrap"><table class="op-th"><thead><tr><th>${th('at', '执行时间')}</th><th>${th('dur', '耗时')}</th><th>命令</th><th>客户端</th><th>完整命令</th></tr></thead><tbody>
    ${cur.length ? cur.map(r => `<tr class="click" data-on="rqRow" data-rec="${r.s.id}|${r.at}|${Math.round(r.dur * 1000)}"><td class="nowrap small">${esc(r.at)}</td><td class="op-num"><b>${fmtMsR(r.dur)}</b></td><td class="mono nowrap">${esc(r.s.name)}</td>
      <td class="mono small">${esc(r.client)}<div class="muted">${esc(r.cname)}</div></td><td><div class="op-sql mono" title="${esc(r.text)}">${esc(clip(r.text, 200))}</div></td></tr>`).join('') : `<tr><td colspan="5">${empty('没有符合条件的慢命令')}</td></tr>`}</tbody></table></div>
    <div class="pager"><span>共 ${total} 条</span>${Array.from({length: np}, (_, i) => `<a data-on="rqDPage" data-p="${i + 1}" class="${i + 1 === pg ? 'on' : ''}">${i + 1}</a>`).join('')}</div>`;
}
function panelCol() {
  const list = visibleInstances(true).filter(i => i.type === 'Redis' && i.slowlog);
  return `<div class="small muted" style="margin-bottom:8px">采集器已启用，每个 Redis 实例约每 60 秒采集一次。</div>
    ${tbl(['实例', '状态', '最近一次采集', '最近成功', '本次新增', '累计入库', '游标（条目 id / 时间）', '说明'], list, {cells: i => { const s = stateOf(i.id);
      return [esc(i.name), s.status === 'ok' ? '<span class="tag ok">正常</span>' : '<span class="tag bad">失败</span>', `<span class="nowrap small">${esc(s.lastRun)}</span>`, `<span class="nowrap small">${esc(s.lastOk)}</span>`, s.lastRows, fi(totalRows(i.id)),
        `<span class="mono small">${s.slowId >= 0 ? s.slowId + ' / ' + esc(s.slowTime) : '—'}</span>`, `<span class="small">${esc(s.error)}</span>`]; }})}`;
}
function rqBody(tab) { return tab === 'detail' ? panelDet() : tab === 'collect' ? panelCol() : panelStat(); }

function render(sub, insts) {
  const inst = INSTANCES.find(i => i.id === Q_INST), tab = sub === 'detail' ? 'detail' : sub === 'collect' ? 'collect' : 'stat';
  const msg = RQ.msg; RQ.msg = null;
  if (RQ.cmd && !cmdNames().includes(RQ.cmd)) RQ.cmd = '';                      // 切换实例后，上一个实例的命令 / 指纹过滤不再适用
  if (RQ.fp && (SPECS.find(s => s.id === RQ.fp) || {}).inst !== Q_INST) RQ.fp = '';
  return `<div class="card" style="margin-bottom:14px"><div class="toolbar" style="margin:0;align-items:flex-end;gap:12px">
      <label class="op-fl">实例<select data-onchange="sqInst"><option value="0">全部有慢日志的实例</option>${insts.map(i => `<option value="${i.id}" ${i.id === Q_INST ? 'selected' : ''}>${esc(i.name)}</option>`).join('')}</select></label>
      <label class="op-fl">时间范围<select data-onchange="rqPreset">${PRESETS.map(([v, t]) => `<option value="${v}" ${RQ.preset === v ? 'selected' : ''}>${t}</option>`).join('')}</select></label>
      <label class="op-fl">开始日期<input type="text" value="${RQ.from}" data-onchange="rqDate" data-k="from" style="width:112px" placeholder="YYYY-MM-DD"></label>
      <label class="op-fl">结束日期<input type="text" value="${RQ.to}" data-onchange="rqDate" data-k="to" style="width:112px" placeholder="YYYY-MM-DD"></label>
      <label class="op-fl">耗时下限<select data-onchange="rqMin">${MIN_OPTS.map(([v, t]) => `<option value="${v}" ${RQ.min === v ? 'selected' : ''}>${t}</option>`).join('')}</select></label>
      <label class="op-fl">关键字<input type="search" value="${esc(RQ.kw)}" data-oninput="rqKw" placeholder="搜索命令 / 客户端地址" style="width:190px"></label>
      <button class="btn" data-on="rqReset">重置</button></div></div>
    ${msg ? notice(esc(msg.text), msg.kind || 'info', msg.kind === 'bad' ? '✗' : msg.kind === 'ok' ? '✓' : 'ℹ') : ''}
    ${tabs('slowquery', [['stat', '命令统计'], ['detail', '慢命令明细'], ['collect', '采集状态']], tab)}
    <div class="toolbar"><span class="muted small">${esc(inst ? inst.name : '')} · ${esc(inst ? inst.host + ':' + inst.port : '')}${inst && inst.role === 'slave' ? ' · 从库' : ''}</span><span class="sp"></span>
      <label class="op-fl" style="flex-direction:row;align-items:center;gap:6px">命令<select data-onchange="rqCmd"><option value="">全部命令</option>${cmdNames().map(c => `<option ${RQ.cmd === c ? 'selected' : ''}>${esc(c)}</option>`).join('')}</select></label>
      <button class="btn" data-on="rqCollect">立即采集</button>
      ${B('redis.slowlog_reset', '重置慢日志', {cls: 'danger', data: {inst: Q_INST}})}</div>
    <div id="rqBody">${rqBody(tab)}</div>`;
}
const refresh = () => { const el = $('#rqBody'); if (el) el.innerHTML = rqBody(ROUTE.sub === 'detail' ? 'detail' : ROUTE.sub === 'collect' ? 'collect' : 'stat'); };
const resetPaging = () => { RQ.page = RQ.dpage = 1; };

ON.rqPreset = (d, el) => { RQ.preset = el.value; if (el.value !== 'custom') { RQ.to = TODAY; RQ.from = dayOffset(-(+el.value === 0 ? 0 : +el.value - 1)); } resetPaging(); rerender(); };
ON.rqDate = (d, el) => {
  const v = el.value.trim();
  if (!/^\d{4}-\d{2}-\d{2}$/.test(v) || isNaN(Date.parse(v))) { toast('日期格式应为 YYYY-MM-DD'); return rerender(); }
  if (v > TODAY) { toast('不能晚于今天（2026-10-07）'); return rerender(); }
  const nf = d.k === 'from' ? v : RQ.from, nt = d.k === 'to' ? v : RQ.to;
  if (nf > nt) { toast('开始日期不能晚于结束日期'); return rerender(); }
  if (daysBetween(nf, nt) + 1 > 93) { toast('查询时间范围最长 93 天，请缩小范围'); return rerender(); }
  RQ.from = nf; RQ.to = nt; RQ.preset = 'custom'; resetPaging(); rerender();
};
ON.rqMin = (d, el) => { RQ.min = el.value; resetPaging(); rerender(); };
ON.rqCmd = (d, el) => { RQ.cmd = el.value; resetPaging(); rerender(); };
ON.rqKw = (d, el) => { RQ.kw = el.value; resetPaging(); refresh(); };
ON.rqReset = () => { Object.assign(RQ, {preset: '7', from: dayOffset(-6), to: TODAY, cmd: '', min: '', kw: '', fp: '', page: 1, dpage: 1}); rerender(); };
ON.rqSort = d => { if (RQ.sort === d.k) RQ.dir = -RQ.dir; else { RQ.sort = d.k; RQ.dir = d.k === 'cmd' ? 1 : -1; } RQ.page = 1; refresh(); };
ON.rqOpen = d => { RQ.open.has(d.id) ? RQ.open.delete(d.id) : RQ.open.add(d.id); refresh(); };
ON.rqPage = d => { RQ.page = +d.p; refresh(); };
ON.rqDPage = d => { RQ.dpage = +d.p; refresh(); };
ON.rqDSort = d => { RQ.dsort = d.k; RQ.dpage = 1; refresh(); };
ON.rqToDet = d => { RQ.fp = d.id; RQ.dpage = 1; go('slowquery', 'detail'); };
ON.rqClearFp = () => { RQ.fp = ''; RQ.dpage = 1; rerender(); };
ON.rqRow = d => {
  const [id, at, us] = d.rec.split('|'), s = SPECS.find(x => x.id === id), rec = detailRecs().find(x => x.s === s && x.at === at && Math.round(x.dur * 1000) === +us);
  if (!rec) return;
  openDrawer('Redis 慢命令明细', `${kvList([['执行时间', esc(rec.at)], ['实例', esc(instOf(s.inst).name)], ['耗时', `<b>${fmtMsR(rec.dur)}</b>`], ['命令', `<span class="mono">${esc(s.name)}</span>`], ['客户端地址', `<span class="mono">${esc(rec.client)}</span>`], ['客户端名称', esc(rec.cname)], ['命令指纹', `<span class="mono small">${esc(s.fp)}</span>`]])}
    <div class="small muted" style="margin:12px 0 4px">完整命令（密码参数已脱敏为 ***）</div><pre class="code op-pre">${esc(rec.text)}</pre>`);
};

/* ---- 立即采集（同一实例 10 秒内只能一次）/ 重置慢日志（高危，redis.slowlog_reset）---- */
function collectInto(inst) {                      // 把实例上尚未采集的条目入库，返回新增条数；失败返回 {err}
  const st = stateOf(inst.id), lv = live(inst.id);
  if (st.status === 'error') return {err: st.error.replace(/（.*$/, '').replace(/^SLOWLOG LEN：/, '')};
  const n = lv.pending, specs = SPECS.filter(s => s.inst === inst.id), r = rng(Date.now() & 0xffff);
  for (let i = 0; i < n && specs.length; i++) { const s = specs[R(r, 0, specs.length - 1)], d = dayBase(s, 1) || dayBase(s, 2) || {avg: s.base[1]}, args = s.full(r), dur = Math.max(10, sampleDur(r, d.avg, 0));
    EXTRA.push({s, at: clkFull(), dur, args, client: clientOf(s, r)}); st.slowId = (st.slowId < 0 ? 0 : st.slowId) + 1; st.slowTime = clkFull(); }
  lv.pending = 0; st.lastRun = st.lastOk = clkFull(); st.lastRows = n;
  return {n};
}
ON.rqCollect = () => {
  const inst = INSTANCES.find(i => i.id === Q_INST); if (!inst) return;
  const st = stateOf(inst.id), now = Date.now();
  if (st.manualAt && now - st.manualAt < 10000) { RQ.msg = {text: `操作过于频繁，请 ${Math.ceil((10000 - (now - st.manualAt)) / 1000)} 秒后再试`, kind: 'bad'}; return rerender(); }
  st.manualAt = now;
  const res = collectInto(inst);
  if (res.err) RQ.msg = {text: '采集失败：' + res.err, kind: 'bad'};
  else RQ.msg = {text: res.n ? `已采集入库 ${res.n} 条新的慢命令` : '没有新的慢命令（实例上的慢日志都已入库）', kind: 'info'};
  rerender();
};
ACTIONS['redis.slowlog_reset'] = d => {
  const inst = instOf(+d.inst), lv = live(inst.id);
  risky('redis.slowlog_reset', `重置 Redis 慢日志：${inst.name}`,
    `将对实例 <b>${esc(inst.name)}</b> 执行 <code>SLOWLOG RESET</code>，清空实例上现有的 <b>${lv.len}</b> 条慢日志（重置前先采集入库）；不可撤销。`,
    'SLOWLOG RESET', () => {
      const res = collectInto(inst);
      if (res.err) {
        audit('redis.slowlog_reset', 'Redis慢查询-重置慢日志', '失败', `实例：${inst.name}（#${inst.id}）；结果：失败（重置前的保底采集失败，已取消重置：${res.err}）`);
        RQ.msg = {text: `重置前的保底采集失败，已取消重置：${res.err}`, kind: 'bad'};
      } else {
        const cleared = lv.len; lv.len = 0;
        audit('redis.slowlog_reset', 'Redis慢查询-重置慢日志', '成功', `实例：${inst.name}（#${inst.id}）；结果：成功；重置前实例上有 ${cleared} 条慢日志，其中新采集入库 ${res.n} 条（redis.slowlog_reset）`);
        RQ.msg = {text: `已重置：清空了实例上的 ${cleared} 条慢日志（重置前新采集入库 ${res.n} 条）`, kind: 'ok'};
        toast('已重置 Redis 慢日志');
      }
      rerender();
    }, inst.name);
};

window.RedisSlow = {isRedis: isRedisInst, render(sub, insts, instId) { Q_INST = instId; return render(sub, insts); }};
})();
