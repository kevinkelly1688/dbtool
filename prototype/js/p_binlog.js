/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_binlog.js
 * @time: 2026/10/28
 */
/* ============================================================================
   Binlog 解析 / 回滚（菜单 binlog，实例管理 › Binlog 解析）
   对应真实实现：internal/binlog、web/templates/binlog、web/static/js/binlog/binlog.js
   流程：选 MySQL 实例 → 读 binlog 文件列表与 binlog_format / binlog_row_image → 填范围与过滤条件 → 解析（后台任务，轮询进度，可取消）
         → 结果分页查看 / 复制 / 下载 → 任务列表（结束后保留 30 分钟）。只读：不会在目标库执行任何语句。
   原型里的假数据：每个 MySQL 系实例按实例 id 确定性地生成 8 个 binlog 文件和若干 INSERT / UPDATE / DELETE / DDL 事件，
   还原 SQL 与回滚 SQL 都由事件的行镜像真实推导（DELETE↔INSERT，UPDATE 前后镜像互换，整体倒序）。
   演示用的“不满足条件”实例：mysql-pay-prod-m（binlog_format=MIXED）、mysql-user-prod-m（binlog_row_image=MINIMAL）、
   mysql-order-view（log_bin=OFF）、mysql-dw-prod-s（SSH 隧道）、mariadb-log-prod（连接失败）。
   权限码：binlog.view 打开页面；binlog.parse 解析 / 取消 / 看任务；binlog.rollback 回滚（高危）；binlog.download 下载（高危）。
   ========================================================================== */
(function () {
'use strict';

/* ---------- 上限与模拟参数（与 internal/binlog/types.go 一致） ---------- */
const LIM = {defRows: 500, hardRows: 20000, defMB: 2, hardMB: 8, defTmo: 120, maxTmo: 600, minTmo: 10, maxScan: 5000000, keepMin: 30, perUser: 2, global: 6, stored: 40};
const SPEED = 18 * 1048576;            // 模拟的 binlog 读取速度（字节 / 秒）
const AVG_EV = 400;                    // 模拟的平均事件大小（字节），用来估算“已扫描事件数”
const STMT_OVERHEAD = 160;             // 每行结果除 SQL 文本外的估算开销，计入体积上限
const TZ = '+08:00';
const NOW0 = TODAY + ' 11:20:00';      // 原型里的“此刻”
const T0 = Date.now(), SIM0 = Date.parse(NOW0.replace(' ', 'T'));
const simMs = (real = Date.now()) => SIM0 + (real - T0);
const tstr = ms => { const d = new Date(ms); return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`; };
const part = (id, html) => { const el = $('#' + id); if (el) el.innerHTML = html; };
const isMy = i => i.type === 'MySQL' || i.type === 'MariaDB';
const fmtBytes = n => n >= 1073741824 ? (n / 1073741824).toFixed(2) + ' GB' : n >= 1048576 ? (n / 1048576).toFixed(1) + ' MB' : n >= 1024 ? (n / 1024).toFixed(1) + ' KB' : n + ' B';
const nf = n => Number(n).toLocaleString('en-US');

/* 演示用：个别实例的 binlog 配置不满足回滚条件（其余默认 log_bin=ON、ROW、FULL） */
const CFG = {3: {fmt: 'MIXED'}, 4: {img: 'MINIMAL'}, 8: {logbin: false}};

/* ---------- 实例可见性 / 可用性 ---------- */
const instList = () => visibleInstances().filter(isMy).sort((a, b) => a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
const unsupportedOf = i => i.ssh ? 'SSH 隧道实例暂不支持' : '';
const instById = id => instList().find(i => i.id === +id);
const roleTxt = i => i.role === 'slave' ? '从' : '主';

/* ============================================================================
   假数据：binlog 文件 + 行事件
   ========================================================================== */
const NAMES = ['王伟', '李娜', '张敏', '刘洋', '陈静', '杨磊', '赵丽', '黄强', '周婷', '吴刚'];
const ADDRS = ['上海市浦东新区张江路 88 号', '北京市朝阳区望京街 10 号', '杭州市西湖区文三路 398 号', '深圳市南山区科技园南路 2 号', '成都市高新区天府大道 1200 号'];
const SKUS = ['无线蓝牙耳机 Pro', '机械键盘 K87', '27 寸显示器 4K', '速干运动 T 恤', '保温杯 500ml', '便携充电宝 20000mAh'];
const ONLY = {order_log: 'I', login_log: 'I'};                        // 只追加的日志表：只会有 INSERT
const SKIP = new Set(['bank_cards']);                                  // 不放进假事件的表
const MUT = {orders: ['status', 'amount'], order_items: ['qty', 'price'], user_addr: ['detail'], orders_2025: ['amount'], payments: ['status', 'channel'],
  refunds: ['amount'], users: ['level', 'email'], user_profile: ['real_name']};
const TOUCH = ['updated_at', 'paid_at'];                               // 更新时顺带刷新的时间列
const isNumCol = c => /^(tiny|small|medium|big)?int|^decimal|^float|^double/i.test(c.t);
const GEN = {
  order_no: x => 'SO' + x.d8 + x.R(10000000, 99999999), pay_no: x => 'PAY' + x.d8 + x.R(10000000, 99999999), refund_no: x => 'RF' + x.d8 + x.R(10000000, 99999999),
  user_id: x => x.R(10000000, 18230000), order_id: x => x.R(28000000, 28460300), sku_id: x => x.R(1000, 9000), sku_name: x => x.pick(SKUS), qty: x => x.R(1, 5),
  price: x => (x.R(500, 50000) / 100).toFixed(2), amount: x => (x.R(100, 99900) / 100).toFixed(2),
  status: x => x.tn === 'payments' ? x.pick([0, 1, 1, 1, 2]) : x.pick([0, 1, 1, 2, 3, 3, 4]),
  receiver_phone: x => '13' + x.R(100000000, 999999999), phone: x => '13' + x.R(100000000, 999999999), receiver_addr: x => x.pick(ADDRS), detail: x => x.pick(ADDRS),
  name: x => x.pick(NAMES), real_name: x => x.pick(NAMES), channel: x => x.pick(['alipay', 'wechat', 'card']), action: x => x.pick(['CREATE', 'PAY', 'SHIP', 'CANCEL', 'RECEIVE']),
  operator: x => x.pick(['system', 'dba_li', 'cs_wu', null]), username: x => 'u_' + x.R(100000, 999999).toString(36), email: x => 'u' + x.R(10000, 99999) + '@example.com',
  level: x => x.R(0, 5), ip: x => `10.${x.R(1, 9)}.${x.R(0, 250)}.${x.R(2, 250)}`, ua: x => 'Mozilla/5.0 (Windows NT 10.0) Chrome/' + x.R(110, 125), birthday: x => `${x.R(1975, 2003)}-${pad2(x.R(1, 12))}-${pad2(x.R(1, 28))}`,
  id_card: () => null,
};
function genVal(c, x) {
  if (c.def === 'auto_increment') return x.id;
  if (GEN[c.n]) return GEN[c.n](x);
  if (/datetime|timestamp/i.test(c.t)) return x.ts;
  if (/^date$/i.test(c.t)) return x.ts.slice(0, 10);
  const dm = /^decimal\(\d+,(\d+)\)/i.exec(c.t); if (dm) return (x.R(100, 99999) / 100).toFixed(+dm[1]);
  if (isNumCol(c)) return x.R(0, 100);
  return 'v' + x.R(1000, 9999);
}

function genData(i) {
  const r = rng(i.id * 7919 + 13), R = (a, b) => a + Math.floor(r() * (b - a + 1)), pick = l => l[Math.floor(r() * l.length)];
  const nowMs = SIM0 - 40000, base = 800 + i.id * 3;
  /* 文件：8 个，最后一个是正在写的，起止时间连续 */
  const lastStart = Date.parse(TODAY + 'T07:10:00') + (i.id % 5) * 11 * 60000, starts = new Array(8);
  starts[7] = lastStart;
  for (let k = 6; k >= 0; k--) starts[k] = starts[k + 1] - R(510, 630) * 60000;
  const files = starts.map((s, k) => ({name: 'mysql-bin.' + String(base + k).padStart(6, '0'), size: (k === 7 ? R(180, 640) : R(880, 1020)) * 1048576 + R(0, 1048575),
    start: s, end: k === 7 ? nowMs : starts[k + 1] - 1000, evs: []}));
  /* 表与权重 */
  const tabs = []; dbsOf(i.id).forEach(d => d.tables.forEach(t => { if (!t.view && !SKIP.has(t.n)) tabs.push({db: d.db, t, w: tabs.length === 0 ? 6 : tabs.length === 1 ? 3 : 1}); }));
  if (!tabs.length) return {files, evs: [], starts: files.map(() => [])};
  const wsum = tabs.reduce((a, b) => a + b.w, 0), pickTab = () => { let v = r() * wsum; for (const x of tabs) { v -= x.w; if (v <= 0) return x; } return tabs[0]; };
  const pool = {}, nextId = {};
  const mkx = (tn, ms, id) => ({R, r, pick, tn, id, ts: tstr(ms), d8: tstr(ms).slice(0, 10).replace(/-/g, '')});
  const genRow = (tab, ms, id) => { const x = mkx(tab.t.n, ms, id), row = {}; tab.t.cols.forEach(c => { row[c.n] = genVal(c, x); }); return row; };
  const newId = tab => { if (nextId[tab.t.n] == null) nextId[tab.t.n] = tab.t.rows - R(2000, 4000); return nextId[tab.t.n]++; };
  const existing = (tab, ms, take) => {                                   // 已存在的行：优先取本段 binlog 里写进来的，否则凭空生成一条“更早的行”
    const p = pool[tab.t.n] || (pool[tab.t.n] = []);
    if (p.length && r() < 0.4) { const k = Math.floor(r() * p.length); return take ? p.splice(k, 1)[0] : p[k]; }
    return genRow(tab, ms - R(1, 20) * 86400000, tab.t.rows - R(4500, 60000));
  };
  const mutate = (tab, row, ms) => {
    const after = {...row}, cols = MUT[tab.t.n] || [], x = mkx(tab.t.n, ms, 0);
    const cand = cols.length ? cols : tab.t.cols.filter(c => c.key !== 'PRI' && !/datetime|timestamp/i.test(c.t)).slice(0, 1).map(c => c.n);
    const n = cand.length > 1 && r() < 0.3 ? 2 : 1, chosen = [...cand].sort(() => r() - 0.5).slice(0, n);
    chosen.forEach(cn => { const c = tab.t.cols.find(y => y.n === cn); if (!c) return; let v = cn === 'status' || cn === 'level' ? (row[cn] >= 4 ? 3 : row[cn] + 1) : genVal(c, x), g = 0; while (String(v) === String(row[cn]) && g++ < 5) v = genVal(c, x); after[cn] = v; });
    tab.t.cols.forEach(c => { if (TOUCH.includes(c.n)) after[c.n] = tstr(ms); });
    return after;
  };
  const addIns = (tab, ms, evs) => { const row = genRow(tab, ms, newId(tab)); (pool[tab.t.n] || (pool[tab.t.n] = [])).push(row); evs.push({db: tab.db, tab, type: 'INSERT', before: null, after: row, ms}); };
  const addUpd = (tab, ms, evs, only) => { const b = existing(tab, ms), a = mutate(tab, b, ms); if (only) a[only[0]] = only[1]; const p = pool[tab.t.n] || (pool[tab.t.n] = []); const k = p.indexOf(b); if (k >= 0) p[k] = a; else p.push(a); evs.push({db: tab.db, tab, type: 'UPDATE', before: b, after: a, ms}); };
  const addDel = (tab, ms, evs) => { const b = existing(tab, ms, true); evs.push({db: tab.db, tab, type: 'DELETE', before: b, after: null, ms}); };
  const normalTrx = (ms) => {
    const evs = [], tab = pickTab(), allow = ONLY[tab.t.n] || 'IUD', roll = r() * 100;
    const ty = allow === 'I' ? 'I' : roll < 45 ? 'I' : roll < 85 ? 'U' : 'D', c = R(1, 3);
    for (let j = 0; j < c; j++) { const m = ms + j * R(0, 2) * 1000; ty === 'I' ? addIns(tab, m, evs) : ty === 'U' ? addUpd(tab, m, evs) : addDel(tab, m, evs); }
    return evs;
  };
  const first = tabs[0], ddlText = i.schema === 'pay' ? ['payments', "ALTER TABLE `pay_db`.`payments` ADD INDEX `idx_channel` (`channel`);"]
    : i.schema === 'user' ? ['users', "ALTER TABLE `user_db`.`users` MODIFY COLUMN `email` varchar(255) DEFAULT NULL;"]
    : ['orders', "ALTER TABLE `order_db`.`orders` ADD COLUMN `refund_flag` tinyint NOT NULL DEFAULT 0 COMMENT '退款标记';"];
  const updTab = tabs.find(x => (MUT[x.t.n] || []).some(c => c === 'status' || c === 'level')) || first, updCol = (MUT[updTab.t.n] || []).includes('status') ? ['status', 4] : ['level', 0];
  const trxs = [];                                                          // 每个事务 = {ms, evs}
  files.forEach((f, k) => {
    const slots = [], n = Math.round((k === 7 ? R(190, 260) : R(50, 110)) / 1.8), span = f.end - f.start - 10000;
    for (let j = 0; j < n; j++) slots.push({ms: f.start + 5000 + Math.floor(r() * span), kind: 'n'});
    if (k === 7) { slots.push({ms: nowMs - 38 * 60000, kind: 'del'}); slots.push({ms: nowMs - 55 * 60000, kind: 'upd'}); }
    if (k === 6) slots.push({ms: f.start + Math.floor(span / 2), kind: 'ddl'});
    slots.sort((a, b) => a.ms - b.ms);
    const list = [];
    slots.forEach(s => {
      let evs;
      if (s.kind === 'n') evs = normalTrx(s.ms);
      else if (s.kind === 'del') { evs = []; for (let j = 0; j < 12; j++) addDel(first, s.ms + j * 200, evs); }
      else if (s.kind === 'upd') { evs = []; for (let j = 0; j < 8; j++) addUpd(updTab, s.ms + j * 300, evs, updCol); }
      else evs = [{db: ddlText[1].split('`')[1], tab: tabs.find(x => x.t.n === ddlText[0]) || first, type: 'DDL', ddl: ddlText[1], ms: s.ms}];
      list.push({ms: s.ms, evs, ddl: s.kind === 'ddl'});
    });
    /* 编号与位置：事务按时间排好后，位置在文件里均匀铺开（只是示意，位置之间留了空隙） */
    const N = list.reduce((a, t) => a + t.evs.length, 0), slot = (f.size - 400) / N; let j = 0;
    list.forEach(t => {
      t.file = f.name; t.fi = k; t.pos = Math.floor(130 + j * slot + slot * 0.1 * r()); if (!t.ddl) f.evs.push(t); else f.evs.push(t);
      t.evs.forEach(e => { e.file = f.name; e.fi = k; e.pos = Math.floor(130 + j * slot + slot * 0.1 * r()); e.endPos = e.pos + R(100, Math.max(120, Math.min(2000, Math.floor(slot * 0.8)))); e.trxId = t; j++; });
      t.pos = t.evs[0].pos;
      trxs.push(t);
    });
  });
  const evs = []; trxs.forEach(t => t.evs.forEach(e => { e.ts = tstr(e.ms); evs.push(e); }));
  return {files, evs, trxs, starts: files.map(f => f.evs.map(t => t.pos))};
}
const CACHE = {};
const dataOf = i => CACHE[i.id] || (CACHE[i.id] = genData(i));

/* 实例当前的 binlog 信息（相当于 GET /instances/:id/info） */
function infoOf(i) {
  const c = CFG[i.id] || {}, d = dataOf(i), logBin = c.logbin !== false, fmt = c.fmt || 'ROW', img = c.img || 'FULL';
  const o = {ver: i.type === 'MariaDB' ? i.ver + '-MariaDB' : i.ver, logBin, fmt: logBin ? fmt : '', img: logBin ? img : '', files: logBin ? d.files : [], reason: ''};
  o.reason = !logBin ? '实例没有开启 binlog（log_bin=OFF）' : fmt !== 'ROW' ? `binlog_format=${fmt}，回滚需要 ROW 格式（STATEMENT / MIXED 里的语句无法还原行数据）` : img !== 'FULL' ? `binlog_row_image=${img}，回滚需要 FULL（MINIMAL / NOBLOB 的行镜像不完整）` : '';
  o.canRollback = !o.reason;
  return o;
}
const fileLabel = f => `${f.name}（${fmtBytes(f.size)} · ${tstr(f.start).slice(5, 16)} ~ ${tstr(f.end).slice(5, 16)}）`;

/* ============================================================================
   SQL 生成：还原 SQL / 回滚 SQL
   ========================================================================== */
const q = n => '`' + n + '`';
const lit = (c, v) => v === null || v === undefined ? 'NULL' : isNumCol(c) ? String(v) : "'" + String(v).replace(/\\/g, '\\\\').replace(/'/g, "\\'") + "'";
function whereOf(e, row, o) {
  const cols = e.tab.t.cols, pk = cols.filter(c => c.key === 'PRI'), use = o.full || !pk.length ? cols : pk;
  return {w: use.map(c => row[c.n] === null ? `${q(c.n)} IS NULL` : `${q(c.n)} = ${lit(c, row[c.n])}`).join(' AND '), lim: pk.length ? '' : ' LIMIT 1'};
}
function restore(e, o) {                                                   // e = {type, before, after, tab, db}
  const cols = e.tab.t.cols, tn = (o.noprefix ? '' : q(e.db) + '.') + q(e.tab.t.n);
  if (e.type === 'INSERT') return `INSERT INTO ${tn}(${cols.map(c => q(c.n)).join(',')}) VALUES (${cols.map(c => lit(c, e.after[c.n])).join(',')});`;
  if (e.type === 'DELETE') { const k = whereOf(e, e.before, o); return `DELETE FROM ${tn} WHERE ${k.w}${k.lim};`; }
  let ch = cols.filter(c => o.full || String(e.before[c.n]) !== String(e.after[c.n])); const k = whereOf(e, e.before, o);
  if (!ch.length) ch = cols.filter(c => c.key !== 'PRI').slice(0, 1);
  return `UPDATE ${tn} SET ${ch.map(c => `${q(c.n)}=${lit(c, e.after[c.n])}`).join(', ')} WHERE ${k.w}${k.lim};`;
}
const SWAP = {INSERT: 'DELETE', DELETE: 'INSERT', UPDATE: 'UPDATE'};
const rollbackOf = (e, o) => restore({...e, type: SWAP[e.type], before: e.after, after: e.before}, o);
const stmtSize = s => s.sql.length + s.schema.length + s.table.length + STMT_OVERHEAD;

/* ============================================================================
   任务：校验 → 计算结局（一次算好，再按时间回放进度）
   ========================================================================== */
const JOBS = [];                                   // 新的在前
const F = {inst: 0, mode: 'sql', sfile: '', spos: '', efile: '', epos: '', stime: '', etime: '', dbs: [], tables: '', types: {insert: true, update: true, delete: true, ddl: false},
  noprefix: false, extra: false, full: false, rows: String(LIM.defRows), mb: String(LIM.defMB), tmo: String(LIM.defTmo)};
const VW = {cur: null, page: 1, size: 20, msg: null};          // cur：当前查看的任务 id；msg：页顶提示
const fIdx = (files, n) => files.findIndex(f => f.name === n);
const parseT = v => { v = String(v || '').trim(); if (!v) return 0; const m = /^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2})(?::(\d{2}))?$/.exec(v); return m ? Date.parse(`${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6] || '00'}`) : NaN; };
const clamp = (v, lo, hi, def) => !v ? def : v < lo ? lo : v > hi ? hi : v;
const splitList = s => { const out = [], seen = new Set(); String(s || '').split(/[,，;\n ]+/).forEach(p => { p = p.trim(); if (p && !seen.has(p.toLowerCase())) { seen.add(p.toLowerCase()); out.push(p); } }); return out; };
const randId = () => Array.from({length: 24}, () => '0123456789abcdef'[Math.floor(Math.random() * 16)]).join('');
const activeOf = j => j.status === 'queued' || j.status === 'running';

/* 校验请求（相当于 buildPlan），返回 {err} 或 {plan} */
function buildPlan(req, i, inf) {
  const bad = m => ({err: m});
  if (!inf.logBin) return bad('该实例没有开启 binlog（log_bin=OFF）');
  if (req.mode === 'rollback' && inf.reason) return bad(`无法生成回滚 SQL：${inf.reason}。仍可使用“还原 SQL”模式解析。`);
  if (!req.sfile) return bad('请选择起始 binlog 文件');
  const si = fIdx(inf.files, req.sfile);
  if (si < 0) return bad(`起始文件 ${req.sfile} 不在实例当前的 binlog 列表里（可能已被清理）`);
  const sp = req.spos || 4;
  if (sp < 4) return bad('起始位置不能小于 4（binlog 文件头占 4 字节）');
  if (sp >= inf.files[si].size) return bad(`起始位置 ${sp} 超出文件 ${req.sfile} 的大小 ${inf.files[si].size}`);
  if (!req.efile && req.epos) return bad('指定结束位置时请同时选择结束文件');
  let ei = inf.files.length - 1;
  if (req.efile) {
    ei = fIdx(inf.files, req.efile);
    if (ei < 0) return bad(`结束文件 ${req.efile} 不在实例当前的 binlog 列表里`);
    if (ei < si) return bad('结束文件不能早于起始文件');
    if (ei === si && req.epos && req.epos <= sp) return bad('结束位置必须大于起始位置');
  }
  const st = parseT(req.stime), et = parseT(req.etime);
  if (Number.isNaN(st)) return bad('时间格式应为 YYYY-MM-DD HH:MM:SS：' + req.stime.slice(0, 40));
  if (Number.isNaN(et)) return bad('时间格式应为 YYYY-MM-DD HH:MM:SS：' + req.etime.slice(0, 40));
  if (st && et && et <= st) return bad('结束时间必须晚于开始时间');
  if (req.dbs.length > 50 || req.tables.length > 50) return bad('过滤条件最多 50 个');
  const types = {}; ['insert', 'update', 'delete', 'ddl'].forEach(k => { if (req.types[k]) types[k.toUpperCase()] = true; });
  if (!Object.keys(types).length) { types.INSERT = types.UPDATE = types.DELETE = true; }
  const lim = {rows: clamp(req.rows, 1, LIM.hardRows, LIM.defRows), mb: clamp(req.mb, 1, LIM.hardMB, LIM.defMB), tmo: clamp(req.tmo, LIM.minTmo, LIM.maxTmo, LIM.defTmo)};
  const ef = req.efile ? inf.files[ei] : inf.files[inf.files.length - 1], epos = req.efile ? (req.epos || ef.size) : ef.size;
  const tList = ['insert', 'update', 'delete', 'ddl'].filter(k => types[k.toUpperCase()]);
  const desc = `${req.sfile}:${sp} → ${!req.efile ? '当前末尾' : req.epos ? `${req.efile}:${req.epos}` : `${req.efile} 末尾`}` + (req.stime || req.etime ? `；时间 ${req.stime || '不限'} ~ ${req.etime || '不限'}` : '')
    + (req.dbs.length ? '；库 ' + req.dbs.join(',') : '') + (req.tables.length ? '；表 ' + req.tables.join(',') : '') + `；类型 ${tList.join(',')}`;
  return {plan: {req, si, ei, sp, ef, epos, st, et, types, lim, desc}};
}

/* 计算任务的最终结局：匹配到的语句、停止原因（超时 / 条数 / 体积 / 扫描事件数）、成功 / 失败 */
function compute(i, inf, p) {
  const d = dataOf(i), r = p.req, files = inf.files, fsz = k => files[k].size;
  const before = k => { let s = 0; for (let x = p.si; x < k; x++) s += fsz(x); return s; };
  const offOf = e => before(e.fi) + e.pos - p.sp;
  let E = before(p.ei) + p.epos - p.sp;
  const inRange = e => e.fi >= p.si && e.fi <= p.ei && (e.fi > p.si || e.pos >= p.sp) && (e.fi < p.ei || e.pos <= p.epos);
  const evs = d.evs.filter(inRange);
  /* 结束时间：读到第一个晚于它的事件就停 */
  if (p.et) { const x = evs.find(e => e.ms > p.et); if (x) E = Math.min(E, offOf(x)); }
  /* 起始位置必须在事务边界上 */
  if (p.sp !== 4 && !d.starts[p.si].includes(p.sp)) return {fail: 'bad_position', msg: '起始位置不在事务边界上（找不到对应的表映射事件）。请从事务开头的位置开始，或把起始位置留空从文件开头读取', E, stop: Math.min(E, 3 * 1048576), kept: [], scannedEv: 0};
  const dbs = new Set(r.dbs.map(x => x.toLowerCase())), tfs = r.tables.map(t => { const k = t.indexOf('.'); return k > 0 && k < t.length - 1 ? {s: t.slice(0, k).toLowerCase(), t: t.slice(k + 1).toLowerCase()} : {s: '', t: t.toLowerCase()}; });
  const o = {noprefix: r.noprefix, full: r.full};
  const M = [];                                                              // 匹配的语句（扫描顺序）
  evs.forEach(e => {
    const off = offOf(e); if (off >= E) return;
    if (e.ms < p.st) return;
    if (!p.types[e.type]) return;
    if (dbs.size && !dbs.has(e.db.toLowerCase())) return;
    if (tfs.length && !tfs.some(f => (!f.s || f.s === e.db.toLowerCase()) && f.t === e.tab.t.n.toLowerCase())) return;
    const ddl = e.type === 'DDL', sql = (ddl ? (r.mode === 'rollback' ? '-- [无法回滚的DDL] ' + e.ddl : e.ddl) : r.mode === 'rollback' ? rollbackOf(e, o) : restore(e, o)) + (r.extra ? ` -- ${e.file}:${e.pos} ${e.ts}` : '');
    M.push({off, e, st: {trxKey: ddl ? null : e.trxId, time: e.ts, file: e.file, pos: e.pos, schema: e.db, table: e.tab.t.n, type: e.type, sql, noRollback: ddl}});
  });
  /* 停止点：取各种限制里最早触发的那个 */
  const maxB = p.lim.mb * 1048576, tmoOff = p.lim.tmo * SPEED, scanOff = LIM.maxScan * AVG_EV;
  let stop = E, reason = '';
  const consider = (off, why) => { if (off < stop) { stop = off; reason = why; } };
  if (M.length > p.lim.rows) consider(M[p.lim.rows].off, 'rows');
  let acc = 0; for (let k = 0; k < M.length; k++) { acc += stmtSize(M[k].st); if (acc > maxB) { consider(M[k].off, 'bytes'); break; } }
  consider(scanOff, 'scan'); consider(tmoOff, 'timeout');
  const kept = M.filter(m => m.off < stop || (reason === '' && m.off <= stop));
  const scannedEv = Math.floor(stop / AVG_EV);
  const out = {E, stop, reason, kept, scannedEv, M};
  const txt = {
    rows: `结果达到 ${p.lim.rows} 条的上限，后面的变更没有列出。请缩小范围（结束位置 / 结束时间 / 库表 / 语句类型），或调大“最多条数”。`,
    bytes: `结果体积达到 ${p.lim.mb} MB 的上限，后面的变更没有列出。请缩小范围，或调大“结果体积上限”。`,
    scan: `扫描的事件数达到 ${nf(LIM.maxScan)} 的上限，任务提前结束。请缩小起止范围（用起止位置 / 时间 / 库表过滤）。`,
    timeout: `任务运行超过 ${p.lim.tmo} 秒被终止，已列出超时前解析到的部分。请缩小范围，或调大“超时时间”。`}[reason];
  if (reason === 'timeout' && (r.mode === 'rollback' || !kept.length)) { out.fail = 'timeout'; out.msg = `任务运行超过 ${p.lim.tmo} 秒仍未完成，已终止。请缩小范围（起止位置 / 时间 / 库表）后重试。`; }
  else if (reason && r.mode === 'rollback') { out.fail = 'limit_exceeded'; out.msg = txt + '回滚 SQL 必须完整，只回滚一部分会造成数据不一致，所以本次不给出结果。'; }
  else out.trunc = txt || '';
  return out;
}

/* 把计算结果落成语句行：回滚模式倒序（后发生的先撤销）、重新编号事务与序号、统计类型与体积 */
function finalizeRows(job) {
  const out = job.out, rb = job.mode === 'rollback';
  let rows = out.kept.map(m => m.st);
  const tmap = new Map(); let tn = 0;
  rows.forEach(s => { if (s.trxKey) { if (!tmap.has(s.trxKey)) tmap.set(s.trxKey, ++tn); s.trx = tmap.get(s.trxKey); } else s.trx = 0; });
  if (rb) rows = rows.slice().reverse();
  rows = rows.map((s, k) => ({...s, seq: k + 1}));
  const byType = {}; let bytes = 0; rows.forEach(s => { byType[s.type] = (byType[s.type] || 0) + 1; bytes += stmtSize(s); });
  const ddl = byType.DDL || 0;
  return {rows, byType, bytes, trx: tn, warnings: ddl && rb ? [`范围内有 ${ddl} 条 DDL / 非行格式语句，它们无法回滚，且之前的行变更可能依赖当时的表结构；请人工确认`] : []};
}

/* 创建任务（相当于 POST /parse 成功后）。opts.seed：种子任务，直接给出结束状态 */
function createJob(i, inf, p, owner, opts = {}) {
  const out = compute(i, inf, p), t = Date.now() + (opts.agoMs ? -opts.agoMs : 0);
  const dur = out.fail === 'bad_position' ? 1800 : Math.min(9000, Math.max(1800, (out.reason === 'timeout' ? p.lim.tmo : out.stop / SPEED) / 12 * 1000));
  const job = {id: randId(), mode: p.req.mode, owner, instId: i.id, inst: i.name, range: p.desc, lim: p.lim, req: p.req, out, status: 'queued', created: t, started: t, dur, finished: 0,
    err: '', errCode: '', warnings: [], trunc: '', res: null, snap: null};
  JOBS.unshift(job);
  if (opts.done) settle(job, out.fail ? 'failed' : 'done', opts.agoMs - 1000 * 60);
  return job;
}
function settle(job, status, finishedAgo = 0) {
  job.status = status; job.finished = Date.now() - finishedAgo;
  const out = job.out;
  if (status === 'failed') { job.err = out.msg || '任务失败'; job.errCode = out.fail || 'internal'; job.snap = snapAt(job, 1); return; }
  const fr = finalizeRows(job);
  job.res = {rows: fr.rows, byType: fr.byType, bytes: fr.bytes, trx: fr.trx, scanned: out.scannedEv};
  job.warnings = fr.warnings; job.trunc = out.trunc || ''; job.snap = snapAt(job, 1);
}
/* 进度快照：f = 0~1 的完成比例（相当于 GET /jobs/:id 里的 scanned / 当前位置 / 已匹配 / 体积） */
function snapAt(job, f) {
  const out = job.out, off = out.stop * f, inf = infoOf(instById(job.instId) || INSTANCES.find(x => x.id === job.instId)), files = inf.files;
  let abs = (job.req.spos || 4) + off, k = Math.max(0, fIdx(files, job.req.sfile));
  while (k < files.length - 1 && abs > files[k].size) { abs -= files[k].size; k++; }
  let matched = 0, bytes = 0;
  if (job.res && f >= 1) { matched = job.res.rows.length; bytes = job.res.bytes; }
  else for (const m of out.kept) { if (m.off > off) break; matched++; bytes += stmtSize(m.st); }
  return {scanned: Math.floor(off / AVG_EV), file: files[k] ? files[k].name : '', pos: Math.max(4, Math.floor(abs)), matched, bytes};
}
const jobById = id => JOBS.find(j => j.id === id);
const canSee = j => (j.owner === ME.login || ME.super) && (j.mode === 'rollback' ? can('binlog.rollback') : can('binlog.parse'));
const gc = () => { const now = Date.now(); for (let k = JOBS.length - 1; k >= 0; k--) { const j = JOBS[k]; if (!activeOf(j) && j.finished && now - j.finished > LIM.keepMin * 60000) { if (VW.cur === j.id) VW.cur = null; JOBS.splice(k, 1); } } };
const remainSec = j => activeOf(j) ? -1 : Math.max(0, Math.floor((j.finished + LIM.keepMin * 60000 - Date.now()) / 1000));
const fmtRemain = s => s < 0 ? '—' : s <= 0 ? '即将清理' : s < 60 ? '不到 1 分钟' : Math.floor(s / 60) + ' 分钟';

/* 每 300ms 推进进行中的任务；页面不在 binlog 时只更新状态，不碰 DOM */
function tick() {
  let changed = false;
  JOBS.forEach(j => {
    if (!activeOf(j)) return;
    const f = Math.min(1, (Date.now() - j.started) / j.dur);
    if (j.status === 'queued' && Date.now() - j.started > 350) j.status = 'running';
    if (f >= 1) settle(j, j.out.fail ? 'failed' : 'done'); else if (j.status === 'running') j.snap = snapAt(j, j.out.fail === 'bad_position' ? f * 0.1 : f);
    changed = true;
  });
  const n = JOBS.length; gc(); if (n !== JOBS.length) changed = true;
  if (changed && ROUTE.page === 'binlog') { const cur = jobById(VW.cur); part('blJobBox', jobCard(cur)); part('blJobsBox', jobsCard()); if (cur && cur.status === 'done' && !$('#blResWrap')) part('blResBox', resCard(cur)); if (!cur) part('blResBox', ''); }
}
setInterval(tick, 300);

/* ---------- 种子任务（超级用户在任务列表里能看到别人的） ---------- */
let seeded = false;
function seed() {
  if (seeded) return; seeded = true;
  const i1 = INSTANCES.find(x => x.id === 1), inf1 = infoOf(i1), f = inf1.files[inf1.files.length - 1].name;
  const r1 = {mode: 'sql', sfile: f, spos: 0, efile: '', epos: 0, stime: '', etime: '', dbs: [], tables: ['orders'], types: {insert: false, update: true, delete: true, ddl: false}, rows: 0, mb: 0, tmo: 0, noprefix: false, extra: false, full: false};
  const b1 = buildPlan(r1, i1, inf1);
  if (b1.plan) createJob(i1, inf1, b1.plan, 'dba_li', {done: true, agoMs: 9 * 60000});
  const i2 = INSTANCES.find(x => x.id === 7), inf2 = infoOf(i2), f2 = inf2.files[inf2.files.length - 2].name;
  const r2 = {mode: 'rollback', sfile: f2, spos: 0, efile: '', epos: 0, stime: '', etime: '', dbs: [], tables: [], types: {insert: true, update: true, delete: true, ddl: true}, rows: 0, mb: 0, tmo: 0, noprefix: false, extra: false, full: false};
  const b2 = buildPlan(r2, i2, inf2);
  if (b2.plan) createJob(i2, inf2, b2.plan, 'dba_li', {done: true, agoMs: 22 * 60000});
}

/* ============================================================================
   页面
   ========================================================================== */
const ST = {queued: ['info', '排队'], running: ['brand', '解析中'], done: ['ok', '已完成'], failed: ['bad', '失败'], canceled: ['', '已取消']};
const MODE_L = {sql: '还原 SQL', rollback: '回滚 SQL'};
const CHK = 'text-align:left;color:var(--text);margin-right:14px';
const setMsg = (text, cls = 'bad') => { VW.msg = text ? {text, cls} : null; part('blMsg', msgHtml()); };
const msgHtml = () => VW.msg ? notice(esc(VW.msg.text), VW.msg.cls, '!') : '';
const curInst = () => { const i = instById(F.inst); return i && !unsupportedOf(i) ? i : null; };
const modeOk = (inf) => ({parse: can('binlog.parse'), rb: can('binlog.rollback') && !!inf && inf.canRollback});

function startBtn() {
  const i = curInst(), inf = i && i.status !== 'bad' ? infoOf(i) : null, rb = F.mode === 'rollback';
  const code = rb ? 'binlog.rollback' : 'binlog.parse', label = rb ? '开始生成回滚 SQL' : '开始解析';
  const why = !i ? '请先选择实例' : !inf ? '无法连接该实例' : !inf.files.length ? '该实例没有 binlog 文件' : '';
  if (why && can(code)) return `<button class="btn primary" disabled style="opacity:.45;cursor:not-allowed" title="${esc(why)}">${label}</button>`;
  return B(code, label, {cls: 'primary', data: {op: 'start'}});
}

function formHtml() {
  const list = instList(), i = curInst();
  const down = i && i.status === 'bad';
  const inf = i && !down ? infoOf(i) : null, md = modeOk(inf), files = inf ? inf.files : [], sf = fIdx(files, F.sfile) >= 0 ? F.sfile : (files.length ? files[files.length - 1].name : '');
  F.sfile = sf;
  if (F.mode === 'rollback' && !md.rb) F.mode = 'sql';
  if (F.mode === 'sql' && !md.parse && md.rb) F.mode = 'rollback';
  const dbs = i ? dbsOf(i.id).map(d => d.db).sort() : [];
  const sample = inf && files.length && sf ? (() => { const s = dataOf(i).starts[fIdx(files, sf)]; return s && s.length ? s[Math.floor(s.length / 2)] : ''; })() : '';
  let info = '<span class="muted">请选择实例</span>';
  if (i && down) info = '<span class="muted">读取失败</span>';
  else if (inf) info = `<span class="nowrap">${tag(inf.ver)} ${tag('binlog_format=' + (inf.fmt || '?'), inf.fmt === 'ROW' ? 'ok' : 'warn')} ${tag('binlog_row_image=' + (inf.img || '?'), inf.img === 'FULL' ? 'ok' : 'warn')} ${tag(files.length + ' 个 binlog 文件')} ${tag('时区 ' + TZ)}</span>
    <div class="small" style="margin-top:6px;color:var(--${inf.canRollback ? 'ok' : 'warn'})">${inf.canRollback ? '可以生成回滚 SQL（ROW + FULL）' : '不能生成回滚 SQL：' + esc(inf.reason)}</div>`;
  let modeNote = '';
  if (!can('binlog.rollback')) modeNote = '没有生成回滚 SQL 的权限';
  const opt = (v, t, sel, dis) => `<option value="${esc(v)}" ${sel ? 'selected' : ''} ${dis ? 'disabled' : ''}>${esc(t)}</option>`;
  const ty = k => `checked`.repeat(F.types[k] ? 1 : 0);
  return `
  <div class="form" style="grid-template-columns:140px minmax(0,1fr);margin-bottom:12px">
    ${fld('实例（仅 MySQL）', `<select data-onchange="blF" data-k="inst" aria-label="实例"><option value="">${list.length ? '请选择实例' : '（没有可用的 MySQL 实例）'}</option>${list.map(x => opt(x.id, `${x.name}（${x.type} · ${roleTxt(x)}）${unsupportedOf(x) ? ' · ' + unsupportedOf(x) : ''}`, x.id === F.inst, !!unsupportedOf(x))).join('')}</select>`)}
    ${fld('实例状态', `<div id="blInfo">${info}</div>`)}
  </div>
  <div class="grid g2" style="gap:24px">
    <div class="form" style="grid-template-columns:140px minmax(0,1fr)">
      ${fld('模式', `<label class="chk" style="${CHK}"><input type="radio" name="blMode" ${F.mode === 'sql' ? 'checked' : ''} ${md.parse ? '' : 'disabled'} data-onchange="blF" data-k="mode" value="sql"> 还原 SQL</label><label class="chk" style="${CHK}"><input type="radio" name="blMode" ${F.mode === 'rollback' ? 'checked' : ''} ${md.rb ? '' : 'disabled'} data-onchange="blF" data-k="mode" value="rollback"> 生成回滚 SQL</label>`,
        modeNote ? {hint: esc(modeNote)} : {})}
      ${fld('起始文件', `<select data-onchange="blF" data-k="sfile" aria-label="起始文件">${files.map(f => opt(f.name, fileLabel(f), f.name === sf)).join('')}</select>`)}
      ${fld('起始位置', `<input type="text" inputmode="numeric" autocomplete="off" data-oninput="blF" data-k="spos" value="${esc(F.spos)}" placeholder="留空 = 文件开头">`,
        sample ? {hint: `原型：本文件的一个事务起点是 ${sample}`} : {})}
      ${fld('结束文件', `<select data-onchange="blF" data-k="efile" aria-label="结束文件"><option value="">读到当前最后一个文件的末尾</option>${files.map(f => opt(f.name, fileLabel(f), f.name === F.efile)).join('')}</select>`)}
      ${fld('结束位置', `<input type="text" inputmode="numeric" autocomplete="off" data-oninput="blF" data-k="epos" value="${esc(F.epos)}" placeholder="${F.efile ? '留空 = 该文件末尾' : '需先选结束文件'}" ${F.efile ? '' : 'disabled'}>`)}
      ${fld('开始时间（服务器时区）', `<input type="text" autocomplete="off" data-oninput="blF" data-k="stime" value="${esc(F.stime)}" placeholder="${TODAY} 10:00:00">`)}
      ${fld('结束时间（服务器时区）', `<input type="text" autocomplete="off" data-oninput="blF" data-k="etime" value="${esc(F.etime)}" placeholder="${TODAY} 10:30:00">`)}
    </div>
    <div class="form" style="grid-template-columns:140px minmax(0,1fr)">
      ${fld('数据库（可多选）', `<select multiple size="5" data-onchange="blF" data-k="dbs" aria-label="数据库">${dbs.map(d => opt(d, d, F.dbs.includes(d))).join('')}</select>`)}
      ${fld('表', `<textarea rows="3" autocomplete="off" data-oninput="blF" data-k="tables" placeholder="orders&#10;order_db.order_items">${esc(F.tables)}</textarea>`)}
      ${fld('语句类型', ['insert', 'update', 'delete'].map(k => `<label class="chk" style="${CHK}"><input type="checkbox" data-onchange="blF" data-k="type:${k}" ${ty(k)}> ${k.toUpperCase()}</label>`).join('') + `<label class="chk" style="${CHK}"><input type="checkbox" data-onchange="blF" data-k="type:ddl" ${ty('ddl')}> DDL（只列出，不可回滚）</label>`)}
      ${fld('输出选项', [['noprefix', '不带库名前缀'], ['extra', '语句后附位置 / 时间注释'], ['full', 'SET / WHERE 带全部列']].map(([k, t]) => `<label class="chk" style="${CHK}"><input type="checkbox" data-onchange="blF" data-k="${k}" ${F[k] ? 'checked' : ''}> ${t}</label>`).join(''))}
      ${fld(`最多条数（≤ ${LIM.hardRows}）`, `<input type="text" inputmode="numeric" autocomplete="off" data-oninput="blF" data-k="rows" value="${esc(F.rows)}">`)}
      ${fld(`体积上限（MB，≤ ${LIM.hardMB}）`, `<input type="text" inputmode="numeric" autocomplete="off" data-oninput="blF" data-k="mb" value="${esc(F.mb)}">`)}
      ${fld(`超时（秒，${LIM.minTmo} – ${LIM.maxTmo}）`, `<input type="text" inputmode="numeric" autocomplete="off" data-oninput="blF" data-k="tmo" value="${esc(F.tmo)}">`)}
    </div>
  </div>
  <div class="toolbar" style="margin-top:14px"><span id="blStartBox">${startBtn()}</span></div>`;
}

function jobCard(j) {
  if (!j) return '';
  const [cls, label] = ST[j.status] || ['', j.status], s = j.snap || {scanned: 0, file: '', pos: 0, matched: 0, bytes: 0}, act = activeOf(j);
  const f = act ? Math.min(1, (Date.now() - j.started) / j.dur) : 1, simSec = Math.round(f * (j.out.reason === 'timeout' ? j.lim.tmo : j.out.stop / SPEED));
  const res = j.res, by = res ? res.byType : {}, types = Object.entries(by).map(([k, n]) => `${k} ${nf(n)}`).join(' · ');
  const kpi = (l, v) => `<div><b style="font-size:18px">${v}</b><div class="small muted">${l}</div></div>`;
  const cancel = act ? (j.mode === 'rollback' ? B('binlog.rollback', '取消任务', {cls: 'danger', data: {op: 'cancel', id: j.id}}) : B('binlog.parse', '取消任务', {cls: 'danger', data: {op: 'cancel', id: j.id}})) : '';
  return `<div class="card" style="margin-bottom:16px"><h2>当前任务 <span class="sub">${tag(label, cls)} ${esc(j.inst)} · ${MODE_L[j.mode]} · ${esc(j.range)}</span><span class="right">${cancel}</span></h2>
    ${act ? `<div class="meter" style="margin-bottom:6px;height:8px"><b style="width:${Math.round(f * 100)}%"></b></div><div class="small muted" style="margin-bottom:10px">模拟已运行 ${simSec} 秒 / 超时 ${j.lim.tmo} 秒</div>` : ''}
    <div style="display:flex;gap:32px;flex-wrap:wrap;margin:8px 0 10px">${kpi('已扫描事件', nf(s.scanned))}${kpi('当前位置', esc(s.file || '—') + ':' + (s.pos || 0))}${kpi('已匹配语句', nf(res ? res.rows.length : s.matched))}${kpi('结果体积', fmtBytes(res ? res.bytes : s.bytes))}${kpi('事务数', nf(res ? res.trx : 0))}</div>
    ${types ? `<div class="small muted">类型分布：${esc(types)}</div>` : ''}
    ${j.status === 'failed' ? notice(esc(j.err || '任务失败'), 'bad', '✗') : ''}${j.status === 'canceled' ? notice('任务已取消', 'warn', '!') : ''}
    ${j.trunc ? notice(esc(j.trunc), 'warn', '!') : ''}${j.warnings.map(w => notice(esc(w), 'warn', '!')).join('')}
    ${j.status === 'done' ? `<div class="small muted">结果保留到 ${tstr(simMs(j.finished + LIM.keepMin * 60000))}</div>` : ''}
    <div class="small muted" style="margin-top:6px">本次上限：${nf(j.lim.rows)} 条 · ${j.lim.mb} MB · ${j.lim.tmo} 秒 · 扫描 ${nf(LIM.maxScan)} 事件 · 任务 ID ${j.id}</div></div>`;
}

function resCard(j) {
  if (!j || j.status !== 'done' || !j.res) return '';
  const rows = j.res.rows, n = rows.length, pages = Math.max(1, Math.ceil(n / VW.size)), pg = Math.min(VW.page, pages), slice = rows.slice((pg - 1) * VW.size, pg * VW.size);
  const canDl = can('binlog.download') && (j.mode !== 'rollback' || can('binlog.rollback'));
  const dl = canDl ? B('binlog.download', '下载 .sql 文件', {data: {id: j.id}}) : (can('binlog.download') ? `<button class="btn" disabled style="opacity:.45;cursor:not-allowed" title="回滚任务的下载另需 binlog.rollback">下载 .sql 文件</button>` : B('binlog.download', '下载 .sql 文件'));
  const tb = t => t === 'DELETE' ? 'bad' : t === 'UPDATE' ? 'warn' : t === 'INSERT' ? 'ok' : 'info';
  return `<div class="card" id="blResWrap" style="margin-bottom:16px"><h2>结果 <span class="sub">${MODE_L[j.mode]} · 共 ${nf(n)} 条</span></h2>
    <div class="toolbar"><button class="btn" data-on="blCopyAll" data-id="${j.id}" ${n ? '' : 'disabled'}>复制全部</button>${n ? dl : `<button class="btn" disabled style="opacity:.45;cursor:not-allowed" title="没有结果可下载">下载 .sql 文件</button>`}</div>
    ${!n ? empty('范围内没有符合条件的语句') : tbl(['#', '事务', '事件时间', '位置', '库.表', '类型', 'SQL', ''], slice, {cls: '', cells: s => [s.seq, s.trx || '—', `<span class="nowrap">${esc(s.time)}</span>`, `<span class="mono small nowrap">${esc(s.file)}:${s.pos}</span>`,
      `<span class="nowrap">${esc(s.schema)}${s.table ? '.' + esc(s.table) : ''}</span>`, `<span class="nowrap">${tag(s.type, tb(s.type))}${s.noRollback ? ' ' + tag('不可回滚', 'warn') : ''}</span>`,
      `<div class="sqlv mono small" style="white-space:pre-wrap;word-break:break-all;min-width:260px">${sqlHl(s.sql)}</div>`, `<button class="btn sm" data-copy="${esc(s.sql)}">复制</button>`]})}
    <div class="pager" style="margin-top:10px;display:flex;gap:8px;align-items:center;flex-wrap:wrap"><span class="small muted">共 ${nf(n)} 条 · 第 ${pg} / ${pages} 页</span>
      <button class="btn sm" data-on="blPage" data-p="${pg - 1}" ${pg <= 1 ? 'disabled' : ''}>上一页</button><button class="btn sm" data-on="blPage" data-p="${pg + 1}" ${pg >= pages ? 'disabled' : ''}>下一页</button>
      <span class="small muted">每页</span><select data-onchange="blSize" aria-label="每页条数">${[20, 50, 100, 200].map(x => `<option value="${x}" ${x === VW.size ? 'selected' : ''}>${x}</option>`).join('')}</select></div></div>`;
}

function jobsCard() {
  const ok = can('binlog.parse') || can('binlog.rollback'), list = ok ? JOBS.filter(canSee).sort((a, b) => b.created - a.created) : [];
  const rows = list.map(j => {
    const s = j.snap || {}, [cls, label] = ST[j.status] || ['', j.status], act = activeOf(j);
    const cancel = act ? (j.mode === 'rollback' ? B('binlog.rollback', '取消', {cls: 'sm danger', data: {op: 'cancel', id: j.id}}) : B('binlog.parse', '取消', {cls: 'sm danger', data: {op: 'cancel', id: j.id}})) : '';
    return [`<span class="nowrap">${tstr(simMs(j.created))}</span>`, esc(j.owner) + (j.owner === ME.login ? '（我）' : ''), esc(j.inst), MODE_L[j.mode], `<span class="small">${esc(j.range)}</span>`,
      `${tag(label, cls)}${j.err && j.status === 'failed' ? `<div class="small muted" style="max-width:260px">${esc(j.err)}</div>` : ''}`,
      j.status === 'done' ? `${nf(j.res.rows.length)} 条${j.trunc ? '（已截断）' : ''}` : act ? `已扫描 ${nf(s.scanned || 0)}` : '—', fmtRemain(remainSec(j)),
      `<div class="ops"><button class="btn sm" data-on="blView" data-id="${j.id}">查看</button>${cancel}</div>`];
  });
  return `<div class="card"><h2>我的任务<span class="right"><button class="btn sm" data-on="blRefresh">刷新</button></span></h2>
    ${ok ? tbl(['创建时间', '创建人', '实例', '模式', '范围', '状态', '结果', '剩余保留', ''], rows, {cells: r => r}) : notice('没有查看任务的权限。', 'warn', '!')}</div>`;
}

pages.binlog = {
  title: 'Binlog 解析',
  render() {
    seed(); gc();
    if (!instById(F.inst) || unsupportedOf(instById(F.inst))) F.inst = 0;
    const cur = jobById(VW.cur) && canSee(jobById(VW.cur)) ? jobById(VW.cur) : null;
    if (!cur) VW.cur = null;
    return `<div id="blMsg">${msgHtml()}</div>
      <div class="card" style="margin-bottom:16px"><h2>解析范围</h2><div id="blFormBox">${formHtml()}</div></div>
      <div id="blJobBox">${jobCard(cur)}</div><div id="blResBox">${cur ? resCard(cur) : ''}</div><div id="blJobsBox">${jobsCard()}</div>`;
  }
};

/* ---------- 交互 ---------- */
const refreshForm = () => part('blFormBox', formHtml());
ON.blF = (d, el) => {
  const k = d.k;
  if (k === 'inst') {
    F.inst = +el.value || 0; F.sfile = ''; F.efile = ''; F.epos = ''; F.dbs = []; setMsg('');
    const i = curInst();
    if (i && i.status === 'bad') setMsg(`连接实例 ${i.name} 失败：dial tcp ${i.host}:${i.port}: i/o timeout`);
    else if (i && !infoOf(i).files.length) setMsg('该实例没有 binlog 文件，或没有开启 binlog（log_bin=OFF）。', 'warn');
    if (F.mode === 'rollback' && !(i && i.status !== 'bad' && infoOf(i).canRollback)) { F.mode = 'sql'; if (i && i.status !== 'bad' && infoOf(i).files.length) toast('该实例不能生成回滚 SQL，已切回“还原 SQL”'); }
    refreshForm();
  } else if (k === 'mode') { F.mode = el.value; refreshForm(); }
  else if (k === 'sfile') { F.sfile = el.value; refreshForm(); }
  else if (k === 'efile') { F.efile = el.value; if (!el.value) F.epos = ''; refreshForm(); }
  else if (k === 'dbs') F.dbs = [...el.selectedOptions].map(o => o.value);
  else if (k.startsWith('type:')) F.types[k.slice(5)] = el.checked;
  else if (['noprefix', 'extra', 'full'].includes(k)) F[k] = el.checked;
  else F[k] = el.value;
};
ON.blView = d => { VW.cur = d.id; VW.page = 1; const j = jobById(d.id); part('blJobBox', jobCard(j)); part('blResBox', resCard(j)); window.scrollTo({top: 0, behavior: 'smooth'}); };
ON.blRefresh = () => { gc(); part('blJobsBox', jobsCard()); toast('已刷新'); };
ON.blPage = d => { VW.page = Math.max(1, +d.p || 1); part('blResBox', resCard(jobById(VW.cur))); };
ON.blSize = (d, el) => { VW.size = +el.value || 20; VW.page = 1; part('blResBox', resCard(jobById(VW.cur))); };
ON.blCopyAll = d => { const j = jobById(d.id); if (j && j.res) copyText(j.res.rows.map(r => r.sql).join('\n')); };

function numOf(s) { s = String(s || '').trim(); if (!s) return 0; return /^\d+$/.test(s) ? Number(s) : NaN; }
function collect() {
  const v = {spos: numOf(F.spos), epos: F.efile ? numOf(F.epos) : 0, rows: numOf(F.rows), mb: numOf(F.mb), tmo: numOf(F.tmo)};
  if (Object.values(v).some(Number.isNaN)) return null;
  return {mode: F.mode, sfile: F.sfile, efile: F.efile, stime: F.stime.trim(), etime: F.etime.trim(), dbs: F.dbs.slice(), tables: splitList(F.tables), types: {...F.types},
    noprefix: F.noprefix, extra: F.extra, full: F.full, ...v};
}

/* 提交任务（相当于 POST /parse）：服务端校验 → 并发上限 → 写审计 → 后台运行 */
function submit(req) {
  const i = curInst();
  if (!i) return setMsg('请选择实例');
  if (i.status === 'bad') return setMsg(`连接实例 ${i.name} 失败：dial tcp ${i.host}:${i.port}: i/o timeout`);
  const inf = infoOf(i), b = buildPlan(req, i, inf);
  if (b.err) return setMsg(b.err);
  const mine = JOBS.filter(j => j.owner === ME.login && activeOf(j)).length;
  if (mine >= LIM.perUser) return setMsg(`你已有 ${LIM.perUser} 个解析任务在运行，请等它们结束或先取消一个。`);
  if (JOBS.filter(activeOf).length >= LIM.global) return setMsg('系统当前的 Binlog 解析任务较多，请稍后再试。');
  setMsg('');
  const job = createJob(i, inf, b.plan, ME.login), rb = job.mode === 'rollback';
  audit(rb ? 'binlog.rollback' : 'binlog.parse', `Binlog解析-${rb ? '回滚' : '解析'}：实例:${i.name}(#${i.id}) 任务:${job.id} 范围:${job.range}`, '成功', i.name);
  VW.cur = job.id; VW.page = 1;
  part('blJobBox', jobCard(job)); part('blResBox', ''); part('blJobsBox', jobsCard());
  const el = $('#blJobBox'); if (el && el.scrollIntoView) el.scrollIntoView({behavior: 'smooth', block: 'nearest'});
}
function cancelJob(id) {
  const j = jobById(id);
  if (!j) return toast('任务不存在或已过期');
  if (!activeOf(j)) { setMsg('任务已经结束', 'warn'); return; }
  j.snap = j.snap || snapAt(j, 0); j.status = 'canceled'; j.err = '已取消'; j.finished = Date.now();
  audit('binlog.parse', `Binlog解析-取消：实例:${j.inst}(#${j.instId}) 任务:${j.id}`, '成功', j.inst);
  part('blJobBox', jobCard(jobById(VW.cur))); part('blJobsBox', jobsCard());
}
const doStart = d => {
  if (d.op === 'cancel') return cancelJob(d.id);
  const req = collect();
  if (!req) return setMsg('位置、最多条数、体积上限、超时必须是数字');
  if (req.mode === 'rollback') {
    risky('binlog.rollback', '生成回滚 SQL', `<ul style="margin:0;padding-left:18px"><li>平台只会生成回滚语句，<b>不会在目标库上执行任何语句</b>；是否执行、何时执行由你决定。</li>
      <li>回滚 SQL 按“后发生的先撤销”的顺序给出，执行前务必在测试环境验证；范围内有 DDL 时无法回滚。</li><li>这次操作会写入系统审计，结果里可能含有业务敏感数据。</li></ul>`, '', () => submit(req));
  } else submit(req);
};
ACTIONS['binlog.parse'] = doStart;
ACTIONS['binlog.rollback'] = doStart;

/* 下载：生成与服务端 renderFile 同格式的 .sql（同一事务的语句包在 BEGIN / COMMIT 里） */
function renderFile(j) {
  const rb = j.mode === 'rollback', rows = j.res.rows, L = [];
  L.push('-- ==========================================================================', `-- Binlog 解析结果（${MODE_L[j.mode]}）`, `-- 实例: ${j.inst}    操作人: ${ME.login}    生成时间: ${tstr(simMs())}`, `-- 范围: ${j.range}`, `-- 语句数: ${rows.length}    事务数: ${j.res.trx}`);
  if (rb) L.push('-- 回滚语句按“后发生的先撤销”的顺序排列，请严格按文件顺序执行；执行前务必先在测试环境验证。');
  if (j.trunc) L.push(`-- 注意: ${j.trunc}`);
  j.warnings.forEach(w => L.push(`-- 警告: ${w}`));
  L.push('-- 内容可能含有业务敏感数据，请妥善保管，用后删除。', '-- ==========================================================================');
  let open = 0;
  rows.forEach(s => { if (s.trx !== open) { if (open) L.push('COMMIT;'); if (s.trx) L.push('BEGIN;'); open = s.trx; } L.push(s.sql); });
  if (open) L.push('COMMIT;');
  return L.join('\n') + '\n';
}
ACTIONS['binlog.download'] = d => {
  const j = jobById(d.id);
  if (!j || j.status !== 'done' || !j.res) return setMsg('任务尚未成功完成，没有可下载的结果');
  if (j.mode === 'rollback' && !can('binlog.rollback')) return setMsg('没有下载回滚任务结果的权限（需要 binlog.rollback）');
  risky('binlog.download', '下载结果文件', `<div>文件里是解析出的完整 SQL（${nf(j.res.rows.length)} 条），可能含有业务敏感数据；<b>每次下载都会写入系统审计</b>。请妥善保管，用后删除。</div>`, '', () => {
    const text = renderFile(j), name = `binlog_${j.instId}_${j.mode}_${j.id.slice(0, 8)}.sql`;
    audit('binlog.download', `Binlog解析-下载：实例:${j.inst}(#${j.instId}) 任务:${j.id} 模式:${j.mode} 语句数:${j.res.rows.length} 大小:${new Blob([text]).size}字节`, '成功', j.inst);
    try { const a = document.createElement('a'); a.href = URL.createObjectURL(new Blob([text], {type: 'text/plain;charset=utf-8'})); a.download = name; document.body.append(a); a.click(); a.remove(); setTimeout(() => URL.revokeObjectURL(a.href), 10000); } catch (e) { toast('下载失败：' + e.message); return; }
    toast('已下载 ' + name);
  });
};

/* 供演示 / 自测查看（不影响其它页面） */
window.BINLOG_DEMO = {jobs: JOBS, F, infoOf, buildPlan, compute, instList};
})();
