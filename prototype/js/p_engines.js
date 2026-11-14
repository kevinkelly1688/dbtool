/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_engines.js
 * @time: 2026/10/28
 */
/* ============================================================================
   PostgreSQL / MongoDB / ClickHouse 在线查询（原型）：语句检查（按各引擎的拒绝 / 放行清单移植）→ 受限执行（模拟）→ 结果整形 → 库表树 / 数据字典 / 示例语句。
   行为依据（拒绝原因的文案、放行 / 拒绝清单与限量取自引擎源码）：
     internal/engine/postgres/{lex,check,run,errors}.go、internal/engine/mongo/{parser,check,run,errors}.go、internal/engine/clickhouse/{lexer,check,denylist,engine,client}.go、
     internal/query/{multi,pgsql,mongo,clickhouse,auth_multi}.go（库表授权与响应形态）。
   数据全部虚构：每个实例有确定性的库表和行（按行下标生成，同一条语句永远得到同一份结果）；不连任何服务端。
   第一期范围：只有在线查询 + 库表元数据（数据字典、库表树、权限申请的库表选择）；这三类没有列脱敏，查询结果页固定提示“不做脱敏”。
   导出 window.EngineQuery，由 p_query.js（在线查询 / 数据字典）、p_priv.js（权限申请）、p_inst.js（实例列表）调用，本文件不注册菜单页。
   本文件不依赖 DOM，只依赖 core.js 的 esc / rng 与 data.js 的 SCHEMAS / INSTANCES / COL 等全局量（供脚本在 Node 里做单元验证）。
   ========================================================================== */
(function () {
'use strict';

/* ---------- 局部样式（只用 CSS 变量） ---------- */
const CSS = `
.eq-grp{font-weight:600;cursor:default;display:flex;gap:6px;align-items:center;padding:4px 10px;color:var(--muted);font-size:12px}
.eq-grp .n{margin-left:auto;font-weight:400}
.eq-ico{display:inline-block;min-width:18px;text-align:center;color:var(--muted)}
.eq-ico.v{color:var(--brand)}.eq-ico.m{color:var(--warn)}.eq-ico.d{color:var(--ok)}
.eq-ty{display:block;font:11px/1.3 ui-monospace,Consolas,monospace;color:var(--muted);font-weight:400}
.eq-long{display:inline-block;max-width:520px;overflow:hidden;text-overflow:ellipsis;vertical-align:bottom}
.eq-note{margin:6px 0}
.eq-warn li{margin:2px 0}
`;
if (typeof document !== 'undefined' && !document.getElementById('eq-style')) {
  const s = document.createElement('style'); s.id = 'eq-style'; s.textContent = CSS; document.head.appendChild(s);
}

/* ---------- 错误：bad_query = 检查拒绝 / PG 服务端 SQL 错误；engine_error = 其它服务端错误；timeout = 超时 ---------- */
class EErr extends Error {
  constructor(msg, code, title) { super(msg); this.ecode = code || 'bad_query'; this.etitle = title || ''; }
}
const bad = (msg, title) => new EErr(msg, 'bad_query', title);

/* ---------- 小工具 ---------- */
const h32 = s => { let h = 2166136261; s = String(s); for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619); } return h >>> 0; };
const R = s => rng(h32(s));
const RI = (r, a, b) => a + Math.floor(r() * (b - a + 1));
const PK = (r, list) => list[Math.floor(r() * list.length)];
const pad = (n, w) => String(n).padStart(w == null ? 2 : w, '0');
const nf = n => Number(n).toLocaleString('en-US');
const setOf = (...a) => new Set(a);
const MAX_SAFE = 9007199254740991;
const NOW_MS = Date.UTC(2026, 9, 7, 2, 41, 3);              // 2026-10-07 10:41:03 +08:00，与在线查询页的“现在”一致
const TZ = 8 * 3600e3, DAY = 86400e3;
const ldate = ms => new Date(Math.floor((ms + TZ) / DAY) * DAY - TZ);      // 本地（+08:00）日期的 0 点
const lparts = ms => { const d = new Date(ms + TZ); return {y: d.getUTCFullYear(), mo: d.getUTCMonth() + 1, d: d.getUTCDate(), h: d.getUTCHours(), mi: d.getUTCMinutes(), s: d.getUTCSeconds(), ms: d.getUTCMilliseconds(), wd: d.getUTCDay()}; };
const ymdOf = ms => { const p = lparts(ms); return `${p.y}-${pad(p.mo)}-${pad(p.d)}`; };
const hmsOf = ms => { const p = lparts(ms); return `${pad(p.h)}:${pad(p.mi)}:${pad(p.s)}`; };
const fracOf = (ms, n) => { const f = pad(lparts(ms).ms, 3); return n >= 6 ? f + '000' : f; };
/* 把字符串当成时间解析：没有时区的按 +08:00；失败返回 NaN */
function parseTs(s) {
  s = String(s).trim();
  let m = /^(\d{4})-(\d{1,2})-(\d{1,2})(?:[ T](\d{1,2}):(\d{2})(?::(\d{2})(?:\.(\d{1,9}))?)?)?\s*(Z|[+-]\d{2}(?::?\d{2})?)?$/.exec(s);
  if (!m) return NaN;
  let off = TZ;
  if (m[8]) off = m[8] === 'Z' ? 0 : (m[8][0] === '-' ? -1 : 1) * (+m[8].slice(1, 3) * 3600e3 + (+(m[8].replace(':', '').slice(3, 5)) || 0) * 60e3);
  return Date.UTC(+m[1], +m[2] - 1, +m[3], +(m[4] || 0), +(m[5] || 0), +(m[6] || 0), Math.floor(+(('0.' + (m[7] || '0')) * 1000))) - off;
}
const isDate = v => v instanceof Date;
const ms2 = v => isDate(v) ? v.getTime() : NaN;
const clip = (s, n) => (s = String(s)).length > n ? s.slice(0, n) + '…' : s;
const hex = (r, n) => Array.from({length: n}, () => RI(r, 0, 15).toString(16)).join('');
const uuidOf = r => `${hex(r, 8)}-${hex(r, 4)}-4${hex(r, 3)}-${PK(r, ['8', '9', 'a', 'b'])}${hex(r, 3)}-${hex(r, 12)}`;

/* ============================================================================
   一、库表定义与行生成
   ========================================================================== */
const SKUS = ['无线蓝牙耳机 Pro', '机械键盘 87 键', 'USB-C 快充线 1m', '智能手环 5', '便携充电宝 20000mAh', '4K 显示器 27 寸', '人体工学椅', '保温杯 500ml', '电动牙刷', '护眼台灯', '无线鼠标', '游戏手柄'];
const CITIES = ['上海', '北京', '杭州', '深圳', '广州', '成都', '南京', '武汉', '西安', '苏州'];
const NICKS = ['风里来', '小橘子', '阿杰', '晴天', 'dreamer', '大熊', '清风', 'luna', '老王', '木木', '星河', 'coco'];
const OPERATORS = ['system', 'wang.lei', 'li.na', 'job:order-timeout', 'system', 'zhao.yang'];
const UAS = ['Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/118.0', 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0) Safari/604.1', 'Mozilla/5.0 (Linux; Android 13) Chrome/117.0 Mobile', 'Mozilla/5.0 (Macintosh; Intel Mac OS X 13_5) Safari/605.1'];
const PAGES = ['/home', '/search', '/item/detail', '/cart', '/checkout', '/order/list', '/user/center', '/coupon'];
const ipOf = r => `10.${RI(r, 20, 31)}.${RI(r, 0, 255)}.${RI(r, 1, 254)}`;

/* 把列类型归类：int / big / float / dec / str / bool / date / ts / arr / map / obj / uuid / bytes / enum / oid */
function kindOf(t) {
  let s = String(t || '').trim();
  for (let k = 0; k < 3; k++) s = s.replace(/^(Nullable|LowCardinality)\((.*)\)$/, '$2');
  const l = s.toLowerCase();
  if (/\[\]$/.test(s) || /^array\(/i.test(s)) return 'arr';
  if (/^map\(/i.test(s)) return 'map';
  if (/^(jsonb?|object|tuple|nested)\b/i.test(s)) return 'obj';
  if (/^uuid$/i.test(s)) return 'uuid';
  if (/^(bytea|bindata)/i.test(s)) return 'bytes';
  if (/^(bool|boolean)$/i.test(s)) return 'bool';
  if (/^objectid/i.test(s)) return 'oid';
  if (/^enum/i.test(s)) return 'enum';
  if (/^(timestamp|datetime)/i.test(s)) return 'ts';
  if (/^date(32)?$/i.test(s)) return 'date';
  if (/^(U?Int(8|16|32)|smallint|integer|int|int2|int4)$/.test(s) || /^(smallint|integer|int|int2|int4)$/.test(l)) return 'int';
  if (/^(U?Int(64|128|256)|bigint|int8|long)$/.test(s) || /^(bigint|int8|long)$/.test(l)) return 'big';
  if (/^(float|double|real|float32|float64)/i.test(s)) return 'float';
  if (/^(numeric|decimal)/i.test(s)) return 'dec';
  return 'str';
}
function scaleOf(t) { const m = /\(\s*\d+\s*,\s*(\d+)\s*\)/.exec(String(t || '')); return m ? +m[1] : (/^(numeric|decimal)$/i.test(String(t || '')) ? -1 : 0); }
const enumVals = t => { const out = []; String(t).replace(/'([^']*)'\s*=\s*(\d+)/g, (m, a, b) => out.push(a)); return out; };

/* 同一张表的补充信息（行生成用）：idc = 以行下标 + 1 为值的主键列；fk = 列 → 父表（用父表总行数换算，使明细与主表对得上）；视图 / 物化视图的行另算 */
const META = {
  'pg_order/public.orders': {idc: 'id'},
  'pg_order/public.order_items': {idc: 'id', fk: {order_id: 'pg_order/public.orders'}},
  'pg_order/audit.order_log': {idc: 'id', fk: {order_id: 'pg_order/public.orders'}},
  'pg_order/public.attachments': {fk: {order_id: 'pg_order/public.orders'}},
  'pg_bi/dw.fact_orders': {idc: 'order_id'}, 'pg_bi/dw.dim_user': {idc: 'user_key'}, 'pg_bi/dw.dim_date': {idc: 'date_key'}, 'pg_bi/ods.orders_raw': {idc: 'id'},
  'ch_order/fact_orders': {idc: 'order_id'}, 'ch_order/order_events': {fk: {order_id: 'ch_order/fact_orders'}},
  'ch_dw/events_raw': {idc: 'event_id'}, 'ch_dw/dim_user': {idc: 'user_id'},
};

/* ---- 表定义对象：把 SCHEMAS 里的 {n, cols…} 包成带 key / total / 列类型分类的 td ---- */
const TDS = new Map();
function findDb(inst, db) { return (SCHEMAS[inst.schema] || []).find(d => d.db === db) || null; }
function tdOf(inst, db, name) {
  const d = findDb(inst, db); if (!d) return null;
  const t = d.tables.find(x => x.n === name); if (!t) return null;
  const key = inst.schema + '/' + t.n;
  if (TDS.has(key + '@' + db)) return TDS.get(key + '@' + db);
  const meta = META[key] || {};
  const td = {key, inst, db, t, n: t.n, schema: inst.schema, type: inst.type, view: !!t.view, kind: t.kind || (t.view ? 'view' : 'table'), meta,
    total: t.rows != null ? t.rows : (t.view ? 30 : 1000), cols: t.cols.map(c => Object.assign({}, c, {kind: kindOf(c.t), scale: scaleOf(c.t)})), fkTotals: {}};
  for (const [c, pk] of Object.entries(meta.fk || {})) {
    const ptab = pk.split('/')[1], pd = (SCHEMAS[inst.schema] || []).flatMap(d => d.tables).find(x => x.n === ptab);
    td.fkTotals[c] = pd ? pd.rows : 1000;
  }
  td.colMap = Object.fromEntries(td.cols.map(c => [c.n.toLowerCase(), c]));
  TDS.set(key + '@' + db, td);
  return td;
}

/* ---- 行生成 ---- */
const POOL = 3000;
/* 第 i 行（0..total-1）距“现在”的秒数：最近 POOL 行铺在最近 30 天，更早的铺在前 700 天（小表整体铺在 700 天内） */
const warp0 = u => u + 0.3 * Math.sin(2 * Math.PI * 29.3 * u) / (2 * Math.PI * 29.3) + 0.3 * Math.sin(2 * Math.PI * 4.3 * u + 1) / (2 * Math.PI * 4.3);
const WARP_A = warp0(0), WARP_B = warp0(1) - warp0(0);
const warp = u => (warp0(u) - WARP_A) / WARP_B;     // 单调：让每天 / 每周的行数不完全相同，数据更像真的
function ageSec(i, total) {
  const pool = Math.min(total, POOL);
  if (total <= POOL) return warp((total - 1 - i) / total) * 700 * 86400;
  if (i >= total - pool) return warp((total - 1 - i) / pool) * 30 * 86400;
  return 30 * 86400 + (total - pool - 1 - i) / (total - pool) * 700 * 86400;
}
const tsAt = (i, total) => { const gap = (total <= POOL ? 700 * 86400 / total : 30 * 86400 / POOL) * 1000; return Math.round(NOW_MS - ageSec(i, total) * 1000 - (h32('j' + i) % 1000) / 1000 * gap * 0.45); };
const STATUS_W = [1, 1, 1, 3, 3, 2, 0, 4];

function genObj(ln, r, ts, td) {
  if (ln === 'ext') return Object.assign({device: PK(r, ['ios', 'android', 'h5', 'mini']), ver: PK(r, ['8.2.1', '8.3.0', '8.3.2', '9.0.0']), ip: ipOf(r)}, r() < 0.3 ? {coupon: {id: PK(r, [20231, 20388, 20412]), off: PK(r, [5, 10, 20])}} : {}, r() < 0.15 ? {gift: true} : {});
  if (ln === 'attrs') return {color: PK(r, ['黑', '白', '银', '蓝']), spec: PK(r, ['标准版', 'Pro', 'Max'])};
  if (ln === 'detail') return PK(r, [{from: 0, to: 1}, {from: 1, to: 2}, {from: 2, to: 3}, {reason: 'timeout'}, {amount: 59.9}]);
  if (ln === 'payload') return {order_no: 'O' + ymdOf(ts).replace(/-/g, '') + RI(r, 10000000, 99999999), items: RI(r, 1, 4), amount: +(Math.pow(r(), 2) * 1800 + 9.9).toFixed(2), source: PK(r, ['oms', 'pay', 'wms'])};
  if (ln === 'props') return Object.assign({channel: PK(r, ['app', 'h5', 'mini'])}, r() < 0.5 ? {coupon: String(PK(r, [20231, 20388]))} : {}, r() < 0.3 ? {ip: ipOf(r)} : {});
  return {k: RI(r, 1, 9)};
}
function genVal(td, c, i, row, r) {
  const total = td.total, ts = tsAt(i, total), ln = c.n.toLowerCase(), k = c.kind, meta = td.meta;
  if (meta.idc === c.n) return k === 'uuid' ? uuidOf(r) : i + 1;
  if (td.fkTotals[c.n]) return Math.floor(i * td.fkTotals[c.n] / total) + 1;
  if (c.nul && ln !== 'ext' && /^(coupon_id|couponid|coupon)$/.test(ln) && r() < 0.62) return null;
  if (c.nul && r() < 0.06 && !/^(channel|city|tags|ext|level)$/.test(ln)) return null;
  switch (k) {
    case 'uuid': return uuidOf(r);
    case 'bytes': return hex(r, 64);
    case 'bool': return ln === 'is_new' ? r() < 0.2 : ln === 'is_workday' ? i % 7 < 5 : r() < 0.5;
    case 'arr': return ln === 'tags' ? Array.from({length: RI(r, 0, 3)}, () => PK(r, ['new', 'vip', 'gift', 'coupon', 'risk', 'presale'])).filter((x, j, a) => a.indexOf(x) === j) : [RI(r, 1, 9)];
    case 'map': case 'obj': return genObj(ln, r, ts, td);
    case 'enum': { const ev = enumVals(c.t); return PK(r, ev.length ? ev : ['a']); }
    case 'date': {
      if (/^(day|stat_date|order_date|event_date|visit_date)$/.test(ln)) return ldate(ts);
      if (ln === 'reg_date') return ldate(NOW_MS - RI(r, 30, 1400) * DAY);
      return ldate(ts);
    }
    case 'ts': {
      if (ln === 'paid_at') return row.status != null && +row.status >= 1 && +row.status <= 3 ? new Date(ts + RI(r, 20, 3600) * 1000) : null;
      if (ln === 'updated_at' || ln === 'last_seen') return new Date(ts + RI(r, 20, 3600) * 1000);
      return new Date(ts);
    }
  }
  /* 数值 / 字符串：按列名 */
  const num = k === 'int' || k === 'big' || k === 'float' || k === 'dec';
  if (ln === 'user_key') return 1 + h32(td.key + 'u' + i) % 2310000;
  if (/^(user_id|uid)$/.test(ln)) return 10000 + h32(td.key + 'u' + i) % 9800000;
  if (ln === 'sku_id') return 100000 + h32(td.key + 's' + i) % 6000 * 3;
  if (/^order_id$/.test(ln)) return 80000000 + h32(td.key + 'o' + i) % 20000000;
  if (ln === 'trace_no') return BigInt('723485021638' + pad(1000000 + (h32('t' + i) % 8999999), 7));
  if (/order_no$/.test(ln)) return 'O' + ymdOf(ts).replace(/-/g, '') + pad(10000000 + (Math.imul(i + 7, 2654435761) >>> 0) % 89999999, 8);
  if (ln === 'status') return PK(r, STATUS_W);
  if (ln === 'level') return PK(r, [0, 0, 1, 1, 2, 3, 4, 5]);
  if (/^(amount|price|gmv|refund|total|value)$/.test(ln)) { const v = +(Math.pow(r(), 2) * 1800 + 9.9).toFixed(2); return k === 'dec' || k === 'float' ? v : Math.round(v); }
  if (ln === 'qty') return PK(r, [1, 1, 1, 2, 2, 3, 5]);
  if (ln === 'users') return RI(r, 1000, 90000);
  if (/^(coupon_id|couponid)$/.test(ln)) return PK(r, [20231, 20388, 20412]);
  if (ln === 'rate') return +(0.1 + r() * 0.8).toFixed(4);
  if (/^(stay_ms|cost_ms|costms)$/.test(ln)) return RI(r, 20, 4000);
  if (ln === 'size_bytes') return RI(r, 2000, 3000000);
  if (ln === 'stock') return RI(r, 0, 20000);
  if (ln === 'sales_30d') return RI(r, 0, 9000);
  if (ln === 'week_no') return 1 + Math.floor(i / 7) % 53;
  if (ln === 'date_key') return +ymdOf(ts).replace(/-/g, '');
  if (ln === 'currency') return 'CNY';
  if (ln === 'channel') return PK(r, ['app', 'h5', 'mini', 'app', 'pc']);
  if (ln === 'source') return PK(r, ['oms', 'pay', 'wms', 'crm']);
  if (/^(city|region)$/.test(ln)) return PK(r, CITIES);
  if (ln === 'sku_name') return SKUS[((row.sku_id || 100000) / 3 | 0) % SKUS.length];
  if (/^(nickname)$/.test(ln)) return PK(r, NICKS);
  if (ln === 'action') return PK(r, ['create', 'pay', 'ship', 'sign', 'cancel', 'refund']);
  if (ln === 'operator') return PK(r, OPERATORS);
  if (ln === 'filename') return `invoice_${ymdOf(ts).replace(/-/g, '')}_${RI(r, 100, 999)}.pdf`;
  if (ln === 'content_type') return PK(r, ['application/pdf', 'image/png', 'image/jpeg']);
  if (ln === 'page') return PK(r, PAGES);
  if (ln === 'ip') return ipOf(r);
  if (ln === 'step') return PK(r, ['浏览', '加购', '下单', '支付']);
  if (/^(name)$/.test(ln)) return PK(r, ['purchase', 'page_view', 'click', 'search', 'add_cart', 'pay_ok']);
  if (/^(sku_name|city_name)$/.test(ln)) return PK(r, SKUS);
  if (ln === 'payload' && k === 'str') return JSON.stringify(genObj('payload', r, ts, td));
  if (num) return k === 'float' || k === 'dec' ? +(r() * 1000).toFixed(2) : RI(r, 0, 1000);
  return clip(ln, 6) + '_' + RI(r, 100, 9999);
}
function rowAt(td, i) {
  const row = {};
  for (const c of td.cols) row[c.n] = genVal(td, c, i, row, R(td.key + '#' + i + '#' + c.n));
  const hk = HOOK[td.key]; if (hk) hk(row, i, td);
  return row;
}
/* 少数表的行需要列之间保持一致 */
const HOOK = {
  'pg_order/public.order_status': (row, i) => { const L = [[0, '待支付', '下单未付款，30 分钟后自动取消'], [1, '已支付', ''], [2, '已发货', ''], [3, '完成', '确认收货或超过 7 天自动完成'], [4, '取消', '用户取消或超时取消']]; const x = L[i % 5]; row.status = x[0]; row.name = x[1]; row.remark = x[2] || null; },
  'pg_order/public.mv_sku_rank': (row, i, td) => { row.sku_id = 100000 + (i * 7 % 6000) * 3; row.sku_name = SKUS[(row.sku_id / 3 | 0) % SKUS.length]; row.qty = 80 + Math.floor(i / td.total * 52000); row.gmv = +(row.qty * (30 + (row.sku_id / 3 | 0) % 300)).toFixed(2); },
  'pg_order/report.sku_stat': (row, i) => { row.sku_id = 100000 + i * 3; row.sku_name = SKUS[i % SKUS.length]; },
  'pg_order/report.daily_sales': (row, i, td) => { const di = Math.floor(i / 4); row.day = ldate(NOW_MS - (Math.floor(td.total / 4) - 1 - di) * DAY); row.channel = ['app', 'h5', 'mini', 'pc'][i % 4]; row.orders = 3000 + h32('d' + i) % 30000; row.gmv = +(row.orders * (80 + h32('g' + i) % 60)).toFixed(2); row.refund = +(row.gmv * 0.02).toFixed(2); },
  'pg_bi/dw.dim_date': (row, i) => { const t = NOW_MS - (3652 - 1 - i) * DAY; row.day = ldate(t); row.date_key = +ymdOf(t).replace(/-/g, ''); row.week_no = 1 + Math.floor(((t + TZ) / DAY + 4) / 7) % 53; row.is_workday = lparts(t).wd % 6 !== 0; },
  'ch_order/dict_city': (row, i) => { row.city_id = 110000 + i * 100; row.city_name = CITIES[i % CITIES.length] + (i >= CITIES.length ? '(' + i + ')' : ''); row.region = ['华东', '华北', '华南', '西南'][i % 4]; },
  'ch_dw/dict_channel': (row, i) => { row.channel_id = i + 1; row.channel_name = ['app', 'h5', 'mini', 'pc', 'partner', 'offline'][i % 6] + (i >= 6 ? '_' + i : ''); },
};

/* 视图：按天汇总最近 30 天（不是真的从底表算，只保证数字量级和底表一致） */
function viewRows(td) {
  const base = (SCHEMAS[td.schema] || []).flatMap(d => d.tables).find(x => /orders$/.test(x.n) && !x.view);
  const tot = (base && base.rows) || 1e7, per = tot / 730, rows = [];
  for (let d = 29; d >= 0; d--) {
    const t = NOW_MS - d * DAY, r = R(td.key + 'v' + d), cnt = Math.round(per * (0.8 + r() * 0.5)), gmv = +(cnt * (95 + r() * 30)).toFixed(2);
    const row = {};
    for (const c of td.cols) row[c.n] = /hour/.test(c.n) ? new Date(Math.floor(t / 3600e3) * 3600e3) : /day|date/.test(c.n) ? ldate(t) : /cnt|orders/.test(c.n) ? cnt : /gmv/.test(c.n) ? gmv : /hour/.test(c.n) ? new Date(Math.floor(t / 3600e3) * 3600e3) : 'x';
    rows.push(row);
  }
  return rows;
}
const POOLS = new Map();
/* 表的“最近 want 行”（升序）。视图 / 物化视图的小结果直接用 viewRows */
function poolRows(td, want) {
  if (td.view && (td.kind === 'view' || td.n === 'mv_order_hourly')) return viewRows(td);
  const total = td.total, n = Math.min(total, Math.min(20000, Math.max(POOL, want || 0))), ck = td.key + '@' + td.db + '#' + n;
  if (POOLS.has(ck)) return POOLS.get(ck);
  const out = [];
  for (let i = total - n; i < total; i++) out.push(rowAt(td, i));
  if (POOLS.size > 30) POOLS.clear();
  POOLS.set(ck, out);
  return out;
}

/* ============================================================================
   二、SQL 子集：切词 + 解析 + 在样本行上求值（PostgreSQL 与 ClickHouse 共用；结果是“像真的”模拟数据，不是真正的数据库）。
   支持：SELECT [DISTINCT] 列表 / 表达式 / 聚合 / FROM 表、子查询、函数表（generate_series、numbers）/ [LEFT|INNER|CROSS] JOIN / WHERE / GROUP BY / HAVING / ORDER BY / LIMIT OFFSET / UNION / WITH(CTE) / VALUES / TABLE。
   不支持（返回“原型未模拟”）：窗口函数、ARRAY JOIN、LATERAL、递归 CTE。
   ========================================================================== */
class SynErr extends Error { constructor(tok, src, msg) { super(msg || 'syntax'); this.tok = tok; this.pos = tok ? tok.pos : (src || '').length; this.near = tok ? tok.raw : ''; } }
class NoSim extends Error {}

const KW_STOP = setOf('where', 'prewhere', 'group', 'having', 'order', 'limit', 'offset', 'fetch', 'union', 'intersect', 'except', 'window', 'for', 'on', 'using', 'join', 'inner', 'left', 'right', 'full', 'cross', 'natural',
  'outer', 'final', 'sample', 'array', 'settings', 'format', 'global', 'any', 'all', 'asof', 'semi', 'anti', 'lateral', 'returning', 'into', 'qualify');

function sqlTokens(src, dia) {
  const toks = [], n = src.length; let i = 0;
  const push = (t, v, raw, pos, extra) => toks.push(Object.assign({t, v, l: t === 'w' ? String(v).toLowerCase() : v, raw, pos}, extra || {}));
  while (i < n) {
    const c = src[i];
    if (/\s/.test(c)) { i++; continue; }
    if (c === '-' && src[i + 1] === '-') { while (i < n && src[i] !== '\n') i++; continue; }
    if (c === '/' && src[i + 1] === '*') { const e = src.indexOf('*/', i + 2); i = e < 0 ? n : e + 2; continue; }
    const st = i;
    if ((c === 'E' || c === 'e') && src[i + 1] === "'" && dia === 'pg') { i++; }
    if (src[i] === "'") {
      let j = i + 1, s = '';
      const esc = dia === 'ch' || src[st] === 'E' || src[st] === 'e';
      while (j < n) {
        if (src[j] === "'") { if (src[j + 1] === "'") { s += "'"; j += 2; continue; } break; }
        if (esc && src[j] === '\\' && j + 1 < n) { const x = src[j + 1]; s += x === 'n' ? '\n' : x === 't' ? '\t' : x; j += 2; continue; }
        s += src[j++];
      }
      i = j + 1; push('s', s, src.slice(st, i), st); continue;
    }
    if (c === '$' && dia === 'pg') { const m = /^\$([A-Za-z_]*)\$/.exec(src.slice(i)); if (m) { const e = src.indexOf(m[0], i + m[0].length); const s = src.slice(i + m[0].length, e < 0 ? n : e); i = e < 0 ? n : e + m[0].length; push('s', s, '$$', st); continue; } }
    if (c === '"' || (c === '`' && dia === 'ch')) {
      let j = i + 1, s = '';
      while (j < n) { if (src[j] === c) { if (src[j + 1] === c) { s += c; j += 2; continue; } break; } s += src[j++]; }
      i = j + 1; push('q', s, src.slice(st, i), st); continue;
    }
    if (/[0-9]/.test(c) || (c === '.' && /[0-9]/.test(src[i + 1] || ''))) {
      const m = /^(0[xX][0-9a-fA-F]+|\d+\.?\d*(?:[eE][+-]?\d+)?|\.\d+(?:[eE][+-]?\d+)?)/.exec(src.slice(i));
      i += m[0].length; push('n', Number(m[0]), m[0], st, {int: /^\d+$/.test(m[0])}); continue;
    }
    if (/[A-Za-z_\u0080-￿]/.test(c)) {
      let j = i + 1; while (j < n && /[A-Za-z0-9_$\u0080-￿]/.test(src[j])) j++;
      push('w', src.slice(i, j), src.slice(i, j), st); i = j; continue;
    }
    const m3 = src.slice(i, i + 3), m2 = src.slice(i, i + 2);
    const op = ['->>', '#>>'].includes(m3) ? m3 : ['<=', '>=', '<>', '!=', '||', '::', '->', '#>', '@>', '<@', '~*', '!~', '==', '&&'].includes(m2) ? m2 : c;
    if (op === '!~' && src[i + 2] === '*') { i += 3; push('o', '!~*', '!~*', st); continue; }
    i += op.length; push('o', op, op, st);
  }
  return toks;
}

const AGG_FUNCS = setOf('count', 'sum', 'avg', 'min', 'max', 'array_agg', 'string_agg', 'any', 'anylast', 'uniq', 'uniqexact', 'uniqcombined', 'uniqhll12', 'grouparray', 'argmax', 'argmin', 'quantile', 'median', 'bool_and', 'bool_or', 'stddev', 'variance', 'topk', 'grouparray', 'json_agg', 'jsonb_agg');
const TYPE_WORDS = setOf('date', 'timestamp', 'timestamptz', 'time', 'interval');

class Parser {
  constructor(src, dia) { this.src = src; this.dia = dia; this.toks = sqlTokens(src, dia); this.i = 0; }
  pk(o) { return this.toks[this.i + (o || 0)]; }
  isW(w, o) { const t = this.pk(o); return !!t && t.t === 'w' && t.l === w; }
  isO(s, o) { const t = this.pk(o); return !!t && t.t === 'o' && t.v === s; }
  eatW(w) { if (this.isW(w)) { this.i++; return true; } return false; }
  eatO(s) { if (this.isO(s)) { this.i++; return true; } return false; }
  err(t) { throw new SynErr(t || this.pk(), this.src); }
  expW(w) { if (!this.eatW(w)) this.err(); }
  expO(s) { if (!this.eatO(s)) this.err(); }
  idn(t) { return this.dia === 'pg' ? (t.t === 'w' ? t.l : t.v) : t.v; }
  isIdent(t) { return !!t && (t.t === 'q' || (t.t === 'w' && !KW_STOP.has(t.l))); }
  done() { return this.i >= this.toks.length; }

  parseStatement() {
    this.eatO(';');
    const q = this.parseQuery();
    this.eatO(';');
    if (!this.done()) this.err();
    return q;
  }
  parseQuery() {
    const ctes = [];
    if (this.eatW('with')) {
      if (this.isW('recursive')) throw new NoSim('递归 CTE');
      do {
        const nt = this.pk(); if (!this.isIdent(nt)) this.err();
        this.i++; let cols = null;
        if (this.eatO('(')) { cols = []; do { cols.push(this.idn(this.pk())); this.i++; } while (this.eatO(',')); this.expO(')'); }
        this.expW('as'); this.eatW('not'); this.eatW('materialized'); this.expO('(');
        const q = this.parseQuery(); this.expO(')');
        ctes.push({name: this.idn(nt), cols, q});
      } while (this.eatO(','));
    }
    let q = this.parseSetOps();
    q.ctes = ctes;
    return q;
  }
  parseSetOps() {
    let left = this.parseSelectCore();
    const ops = [];
    while (this.isW('union') || this.isW('intersect') || this.isW('except')) {
      const op = this.pk().l; this.i++; let all = false;
      if (this.eatW('all')) all = true; else this.eatW('distinct');
      ops.push({op, all, q: this.parseSelectCore()});
    }
    if (ops.length) left = {k: 'q', setops: ops, left, order: null, limit: null, offset: null};
    this.parseTail(left);
    return left;
  }
  parseTail(q) {
    if (this.isW('order') && this.isW('by', 1)) {
      this.i += 2; q.order = [];
      do {
        const e = this.parseExpr(); let dir = 1;
        if (this.eatW('desc')) dir = -1; else this.eatW('asc');
        let nulls = null; if (this.eatW('nulls')) { nulls = this.pk().l; this.i++; }
        q.order.push({e, dir, nulls});
      } while (this.eatO(','));
    }
    for (;;) {
      if (this.eatW('limit')) {
        let a = this.parseExpr();
        if (this.eatO(',')) { q.offset = a; a = this.parseExpr(); } else if (this.eatW('offset')) q.offset = this.parseExpr();
        q.limit = a;
        if (this.isW('by')) throw new NoSim('LIMIT … BY');
      } else if (this.eatW('offset')) { q.offset = this.parseExpr(); this.eatW('rows'); this.eatW('row'); }
      else if (this.eatW('fetch')) { this.eatW('first') || this.eatW('next'); q.limit = this.isW('row') || this.isW('rows') ? {k: 'num', v: 1} : this.parseExpr(); this.eatW('rows') || this.eatW('row'); this.eatW('only'); }
      else break;
    }
  }
  parseSelectCore() {
    if (this.eatO('(')) { const q = this.parseQuery(); this.expO(')'); return q.ctes && q.ctes.length ? q : Object.assign(q, {paren: true}); }
    if (this.eatW('values')) {
      const rows = [];
      do { this.expO('('); const r = []; do r.push(this.parseExpr()); while (this.eatO(',')); this.expO(')'); rows.push(r); } while (this.eatO(','));
      return {k: 'q', values: rows, order: null, limit: null, offset: null};
    }
    if (this.eatW('table')) { const f = this.parseTableRef(); return {k: 'q', items: [{star: true}], from: [f], joins: [], order: null, limit: null, offset: null}; }
    this.expW('select');
    const q = {k: 'q', distinct: false, items: [], from: [], joins: [], where: null, group: null, having: null, order: null, limit: null, offset: null};
    if (this.eatW('distinct')) { q.distinct = true; if (this.isW('on')) throw new NoSim('DISTINCT ON'); } else this.eatW('all');
    do {
      if (this.isO('*')) { this.i++; q.items.push({star: true}); continue; }
      const t0 = this.pk(); if (!t0) this.err();
      const startPos = t0.pos;
      const e = this.parseExpr();
      const endPos = this.pk() ? this.pk().pos : this.src.length;
      let alias = null;
      if (this.eatW('as')) { alias = this.idn(this.pk()); this.i++; }
      else if (this.pk() && this.isIdent(this.pk()) && !this.isW('from')) { alias = this.idn(this.pk()); this.i++; }
      q.items.push({e, alias, text: this.src.slice(startPos, endPos).trim()});
    } while (this.eatO(','));
    if (this.eatW('from')) {
      q.from.push(this.parseFromItem());
      for (;;) {
        if (this.eatO(',')) { q.from.push(this.parseFromItem()); continue; }
        const j = this.parseJoin(); if (!j) break;
        q.joins.push(Object.assign(j, {after: q.from.length - 1}));
      }
    }
    if (this.eatW('prewhere')) q.where = this.parseExpr();
    if (this.eatW('where')) { const w = this.parseExpr(); q.where = q.where ? {k: 'bin', op: 'and', l: q.where, r: w} : w; }
    if (this.isW('group') && this.isW('by', 1)) {
      this.i += 2; q.group = [];
      do q.group.push(this.parseExpr()); while (this.eatO(','));
      if (this.isW('with') && (this.isW('totals', 1) || this.isW('rollup', 1) || this.isW('cube', 1))) throw new NoSim('GROUP BY … WITH ' + this.pk(1).v.toUpperCase());
    }
    if (this.eatW('having')) q.having = this.parseExpr();
    if (this.isW('window')) throw new NoSim('WINDOW 子句');
    return q;
  }
  parseJoin() {
    let type = 'inner', any = false;
    const save = this.i;
    this.eatW('global'); this.eatW('natural');
    if (this.isW('any') || this.isW('all') || this.isW('asof') || this.isW('semi') || this.isW('anti')) this.i++;
    if (this.eatW('inner')) type = 'inner';
    else if (this.eatW('left')) { type = 'left'; this.eatW('outer'); }
    else if (this.eatW('right')) { type = 'right'; this.eatW('outer'); }
    else if (this.eatW('full')) { type = 'full'; this.eatW('outer'); }
    else if (this.eatW('cross')) type = 'cross';
    if (!this.eatW('join')) { this.i = save; return null; }
    const src = this.parseFromItem();
    let on = null, using = null;
    if (this.eatW('on')) on = this.parseExpr();
    else if (this.eatW('using')) { using = []; const par = this.eatO('('); do { using.push(this.idn(this.pk())); this.i++; } while (this.eatO(',')); if (par) this.expO(')'); }
    return {type, src, on, using};
  }
  parseTableRef() {
    this.eatW('only');
    const t = this.pk(); if (!this.isIdent(t)) this.err();
    const parts = [this.idn(t)]; this.i++;
    while (this.isO('.') && this.isIdent(this.pk(1))) { this.i++; parts.push(this.idn(this.pk())); this.i++; }
    return {kind: 'table', parts, alias: null, pos: t.pos};
  }
  parseFromItem() {
    if (this.isW('lateral')) throw new NoSim('LATERAL');
    let src;
    if (this.isO('(')) {
      if (this.isW('select', 1) || this.isW('with', 1) || this.isW('values', 1) || this.isO('(', 1)) { this.i++; const q = this.parseQuery(); this.expO(')'); src = {kind: 'sub', q, alias: null}; }
      else { this.i++; const f = this.parseFromItem(); while (this.parseJoin()) {} this.expO(')'); src = f; }
    } else {
      const t = this.pk(); if (!t) this.err();
      let k = 0; while (this.isIdent(this.pk(k)) && this.isO('.', k + 1) && this.isIdent(this.pk(k + 2))) k += 2;
      if (this.isIdent(t) && this.isO('(', k + 1)) {
        const parts = [this.idn(t)]; this.i++; while (this.isO('.')) { this.i++; parts.push(this.idn(this.pk())); this.i++; }
        this.i++; const args = [];
        if (!this.isO(')')) do args.push(this.parseExpr()); while (this.eatO(','));
        this.expO(')'); src = {kind: 'func', name: parts[parts.length - 1].toLowerCase(), full: parts.join('.'), args, alias: null};
      } else src = this.parseTableRef();
    }
    if (this.dia === 'ch') { if (this.eatW('final')) src.final = true; if (this.eatW('sample')) { this.parseExpr(); } }
    if (this.eatW('as')) { src.alias = this.idn(this.pk()); this.i++; }
    else if (this.isIdent(this.pk())) { src.alias = this.idn(this.pk()); this.i++; }
    if (src.alias && this.isO('(') && src.kind === 'func') { this.i++; src.colAliases = []; do { src.colAliases.push(this.idn(this.pk())); this.i++; } while (this.eatO(',')); this.expO(')'); }
    return src;
  }

  /* ---- 表达式（优先级爬升）---- */
  parseExpr() { return this.parseOr(); }
  parseOr() { let l = this.parseAnd(); while (this.eatW('or')) l = {k: 'bin', op: 'or', l, r: this.parseAnd()}; return l; }
  parseAnd() { let l = this.parseNot(); while (this.eatW('and')) l = {k: 'bin', op: 'and', l, r: this.parseNot()}; return l; }
  parseNot() { if (this.eatW('not')) return {k: 'un', op: 'not', e: this.parseNot()}; return this.parseCmp(); }
  parseCmp() {
    let l = this.parseConcat();
    for (;;) {
      const t = this.pk(); if (!t) return l;
      if (t.t === 'o' && ['=', '==', '<>', '!=', '<', '>', '<=', '>=', '~', '~*', '!~', '!~*', '@>', '<@', '&&'].includes(t.v)) {
        this.i++; const op = t.v === '==' ? '=' : t.v === '!=' ? '<>' : t.v;
        if (this.dia === 'pg' && (this.isW('any') || this.isW('all') || this.isW('some')) && this.isO('(', 1)) { const all = this.pk().l === 'all'; this.i += 2; let r; if (this.isW('select') || this.isW('with')) { r = {k: 'sub', q: this.parseQuery(), arr: true}; } else r = this.parseExpr(); this.expO(')'); l = {k: 'quant', op, all, l, r}; continue; }
        l = {k: 'bin', op, l, r: this.parseConcat()}; continue;
      }
      if (t.t === 'o' && t.v === '?' ) { this.i++; l = {k: 'bin', op: '?', l, r: this.parseConcat()}; continue; }
      if (this.isW('is')) {
        this.i++; const neg = this.eatW('not');
        if (this.eatW('null')) { l = {k: 'isnull', e: l, neg}; continue; }
        if (this.eatW('true') || this.eatW('false')) { const tv = this.toks[this.i - 1].l === 'true'; l = {k: 'bin', op: neg ? '<>' : '=', l, r: {k: 'bool', v: tv}}; continue; }
        if (this.eatW('distinct')) { this.expW('from'); l = {k: 'bin', op: neg ? 'isnotdistinct' : 'isdistinct', l, r: this.parseConcat()}; continue; }
        this.err();
      }
      let neg = false, save = this.i;
      if (this.isW('not') && (this.isW('in', 1) || this.isW('like', 1) || this.isW('ilike', 1) || this.isW('between', 1))) { this.i++; neg = true; }
      if (this.eatW('in') || (this.dia === 'ch' && (this.eatW('global') && this.eatW('in')))) {
        if (this.eatO('(')) {
          if (this.isW('select') || this.isW('with')) { const q = this.parseQuery(); this.expO(')'); l = {k: 'in', e: l, sub: q, neg}; }
          else { const list = []; if (!this.isO(')')) do list.push(this.parseExpr()); while (this.eatO(',')); this.expO(')'); l = {k: 'in', e: l, list, neg}; }
        } else { const r = this.parseConcat(); l = {k: 'in', e: l, arr: r, neg}; }
        continue;
      }
      if (this.isW('between')) { this.i++; const lo = this.parseConcat(); this.expW('and'); const hi = this.parseConcat(); l = {k: 'between', e: l, lo, hi, neg}; continue; }
      if (this.isW('like') || this.isW('ilike')) { const ci = this.pk().l === 'ilike'; this.i++; const pat = this.parseConcat(); l = {k: 'like', e: l, pat, neg, ci}; continue; }
      if (this.isW('similar')) throw new NoSim('SIMILAR TO');
      this.i = save;
      return l;
    }
  }
  parseConcat() {
    let l = this.parseAdd();
    for (;;) {
      const t = this.pk();
      if (t && t.t === 'o' && ['||', '->', '->>', '#>', '#>>'].includes(t.v)) { this.i++; l = {k: 'bin', op: t.v, l, r: this.parseAdd()}; } else return l;
    }
  }
  parseAdd() {
    let l = this.parseMul();
    for (;;) { const t = this.pk(); if (t && t.t === 'o' && (t.v === '+' || t.v === '-')) { this.i++; l = {k: 'bin', op: t.v, l, r: this.parseMul()}; } else return l; }
  }
  parseMul() {
    let l = this.parseUnary();
    for (;;) { const t = this.pk(); if (t && t.t === 'o' && (t.v === '*' || t.v === '/' || t.v === '%')) { this.i++; l = {k: 'bin', op: t.v, l, r: this.parseUnary()}; } else return l; }
  }
  parseUnary() {
    if (this.isO('-')) { this.i++; return {k: 'un', op: '-', e: this.parseUnary()}; }
    if (this.isO('+')) { this.i++; return this.parseUnary(); }
    return this.parsePostfix(this.parsePrimary());
  }
  parsePostfix(e) {
    for (;;) {
      if (this.isO('::')) { this.i++; e = {k: 'cast', e, type: this.parseType()}; continue; }
      if (this.isO('[') && this.dia === 'pg') { this.i++; const i = this.parseExpr(); this.expO(']'); e = {k: 'idx', e, i}; continue; }
      if (this.isO('[') && this.dia === 'ch') { this.i++; const i = this.parseExpr(); this.expO(']'); e = {k: 'idx', e, i}; continue; }
      return e;
    }
  }
  parseType() {
    let s = '';
    const t = this.pk(); if (!t || (t.t !== 'w' && t.t !== 'q')) this.err();
    s = String(t.v).toLowerCase(); this.i++;
    while (this.pk() && this.pk().t === 'w' && ['varying', 'precision', 'with', 'without', 'time', 'zone', 'double'].includes(this.pk().l)) { s += ' ' + this.pk().l; this.i++; }
    if (this.isO('(')) { this.i++; while (!this.isO(')') && this.pk()) this.i++; this.expO(')'); }
    while (this.isO('[') && this.isO(']', 1)) { this.i += 2; s += '[]'; }
    return s;
  }
  parsePrimary() {
    const t = this.pk(); if (!t) this.err();
    if (t.t === 'n') { this.i++; return {k: 'num', v: t.v, int: t.int, raw: t.raw}; }
    if (t.t === 's') { this.i++; return {k: 'str', v: t.v}; }
    if (t.t === 'o') {
      if (t.v === '(') {
        this.i++;
        if (this.isW('select') || this.isW('with') || this.isW('values')) { const q = this.parseQuery(); this.expO(')'); return {k: 'sub', q}; }
        const e = this.parseExpr();
        if (this.eatO(',')) { const items = [e]; do items.push(this.parseExpr()); while (this.eatO(',')); this.expO(')'); return {k: 'tuple', items}; }
        this.expO(')'); return {k: 'paren', e};
      }
      if (t.v === '[') { this.i++; const items = []; if (!this.isO(']')) do items.push(this.parseExpr()); while (this.eatO(',')); this.expO(']'); return {k: 'arr', items}; }
      if (t.v === '*') { this.i++; return {k: 'star'}; }
      this.err();
    }
    if (t.t === 'w') {
      const l = t.l;
      if (l === 'null') { this.i++; return {k: 'null'}; }
      if (l === 'true' || l === 'false') { this.i++; return {k: 'bool', v: l === 'true'}; }
      if (l === 'case') return this.parseCase();
      if ((l === 'cast' || l === 'try_cast') && this.isO('(', 1)) { this.i += 2; const e = this.parseExpr(); this.expW('as'); const type = this.parseType(); this.expO(')'); return {k: 'cast', e, type}; }
      if (l === 'extract' && this.isO('(', 1)) { this.i += 2; const f = this.pk().l; this.i++; this.expW('from'); const e = this.parseExpr(); this.expO(')'); return {k: 'fn', name: 'extract', args: [{k: 'str', v: f}, e]}; }
      if ((l === 'substring' || l === 'trim' || l === 'position' || l === 'overlay') && this.isO('(', 1)) return this.parseSpecialFn(l);
      if (l === 'exists' && this.isO('(', 1)) { this.i += 2; const q = this.parseQuery(); this.expO(')'); return {k: 'exists', q}; }
      if (l === 'array' && this.isO('[', 1)) { this.i++; return this.parsePrimary(); }
      if (l === 'array' && this.isO('(', 1)) { this.i += 2; const q = this.parseQuery(); this.expO(')'); return {k: 'sub', q, arr: true}; }
      if (l === 'interval' && this.pk(1) && this.pk(1).t === 's') { this.i++; const s = this.pk().v; this.i++; return {k: 'interval', ms: parseInterval(s)}; }
      if (l === 'interval' && this.dia === 'ch' && this.pk(1) && this.pk(1).t === 'n') { this.i++; const n = this.pk().v; this.i++; const u = this.pk().l; this.i++; return {k: 'interval', ms: parseInterval(n + ' ' + u)}; }
      if ((l === 'date' || l === 'timestamp' || l === 'timestamptz' || l === 'time') && this.pk(1) && this.pk(1).t === 's' && this.dia === 'pg') { this.i++; const s = this.pk().v; this.i++; return {k: 'cast', e: {k: 'str', v: s}, type: l}; }
      if (['current_date', 'current_timestamp', 'current_user', 'current_schema', 'current_catalog', 'localtimestamp', 'session_user'].includes(l) && !this.isO('(', 1)) { this.i++; return {k: 'fn', name: l, args: []}; }
      if (l === 'distinct' || l === 'select') this.err();
      /* 标识符：可带限定名，或函数调用 */
      if (KW_STOP.has(l) && l !== 'left' && l !== 'right' && l !== 'any' && l !== 'all' && l !== 'format') this.err();
    }
    if (t.t !== 'w' && t.t !== 'q') this.err();
    const parts = [this.idn(t)]; this.i++;
    if (this.isO('(') && t.t === 'w') {
      this.i++; return this.parseCall(parts[0]);
    }
    while (this.isO('.') ) {
      const nx = this.pk(1); if (nx && nx.t === 'o' && nx.v === '*') { this.i += 2; return {k: 'star', q: parts.join('.')}; }
      if (!nx || (nx.t !== 'w' && nx.t !== 'q')) break;
      this.i += 2; parts.push(this.idn(nx));
      if (this.isO('(') && nx.t === 'w') { this.i++; return this.parseCall(parts[parts.length - 1], parts.slice(0, -1)); }
    }
    return {k: 'col', parts, pos: t.pos};
  }
  parseCall(name, prefix) {
    const lname = name.toLowerCase(), fn = {k: 'fn', name: lname, rawName: name, args: [], prefix: prefix || []};
    let distinct = false;
    if (this.isO('*') && this.isO(')', 1)) { this.i += 2; fn.star = true; return this.finishCall(fn); }
    if (this.eatW('distinct')) distinct = true;
    if (!this.isO(')')) do fn.args.push(this.parseExpr()); while (this.eatO(','));
    if (this.isW('order') && this.isW('by', 1)) { this.i += 2; fn.orderBy = []; do { const e = this.parseExpr(); let dir = 1; if (this.eatW('desc')) dir = -1; else this.eatW('asc'); fn.orderBy.push({e, dir}); } while (this.eatO(',')); }
    this.expO(')');
    fn.distinct = distinct;
    { const m = /^(count|sum|avg|min|max|uniq|uniqexact|any|anylast)if$/.exec(lname); if (this.dia === 'ch' && m && fn.args.length) { fn.name = m[1]; fn.filter = fn.args.pop(); if (m[1] === 'count' && !fn.args.length) fn.star = true; } }
    /* ClickHouse 参数化聚合：quantile(0.95)(x) */
    if (this.dia === 'ch' && this.isO('(') && ['quantile', 'quantiles', 'topk', 'quantileexact'].includes(lname)) {
      this.i++; fn.params = fn.args; fn.args = []; if (!this.isO(')')) do fn.args.push(this.parseExpr()); while (this.eatO(',')); this.expO(')');
    }
    return this.finishCall(fn);
  }
  finishCall(fn) {
    if (this.isW('filter') && this.isO('(', 1)) { this.i += 2; this.expW('where'); fn.filter = this.parseExpr(); this.expO(')'); }
    if (this.isW('over')) throw new NoSim('窗口函数 ' + fn.rawName + '() OVER (…)');
    return fn;
  }
  parseSpecialFn(l) {
    this.i += 2; const fn = {k: 'fn', name: l, args: []};
    if (l === 'trim') { let side = 'both'; if (this.isW('both') || this.isW('leading') || this.isW('trailing')) { side = this.pk().l; this.i++; } let chars = null; if (!this.isW('from')) chars = this.parseExpr(); if (this.eatW('from')) { const e = this.parseExpr(); this.expO(')'); return {k: 'fn', name: 'btrim_' + side, args: [e].concat(chars ? [chars] : [])}; } this.expO(')'); return {k: 'fn', name: 'btrim_both', args: [chars]}; }
    fn.args.push(this.parseExpr());
    if (this.eatW('from') || this.eatW('in')) fn.args.push(this.parseExpr());
    if (this.eatW('for')) fn.args.push(this.parseExpr());
    while (this.eatO(',')) fn.args.push(this.parseExpr());
    this.expO(')');
    if (l === 'position') fn.args.reverse();
    return fn;
  }
  parseCase() {
    this.expW('case'); const c = {k: 'case', operand: null, whens: [], els: null};
    if (!this.isW('when')) c.operand = this.parseExpr();
    while (this.eatW('when')) { const w = this.parseExpr(); this.expW('then'); c.whens.push([w, this.parseExpr()]); }
    if (this.eatW('else')) c.els = this.parseExpr();
    this.expW('end');
    return c;
  }
}
function parseInterval(s) {
  s = String(s).trim().toLowerCase(); let ms = 0, any = false;
  s.replace(/(-?\d+(?:\.\d+)?)\s*(year|month|mon|week|day|hour|minute|min|second|sec|ms|millisecond)s?/g, (m, n, u) => { any = true; ms += +n * ({year: 365 * DAY, month: 30 * DAY, mon: 30 * DAY, week: 7 * DAY, day: DAY, hour: 3600e3, minute: 60e3, min: 60e3, second: 1e3, sec: 1e3, ms: 1, millisecond: 1})[u]; return m; });
  const hm = /(\d+):(\d{2}):(\d{2})/.exec(s); if (hm) { any = true; ms += (+hm[1] * 3600 + +hm[2] * 60 + +hm[3]) * 1000; }
  if (!any && /^\d+$/.test(s)) ms = +s * 1000;
  return ms;
}

/* ============================================================================
   三、求值：值比较 / 函数库 / 聚合 / 类型推断
   ========================================================================== */
const isBig = v => typeof v === 'bigint';
const isNull = v => v === null || v === undefined;
const toNum = v => isNull(v) ? null : typeof v === 'number' ? v : isBig(v) ? Number(v) : isDate(v) ? v.getTime() : typeof v === 'boolean' ? (v ? 1 : 0) : (v !== '' && !isNaN(Number(v))) ? Number(v) : NaN;
const toStr = v => isNull(v) ? null : typeof v === 'string' ? v : isDate(v) ? isoFor(v) : (Array.isArray(v) || (typeof v === 'object' && !isBig(v))) ? JSON.stringify(v, (k, x) => isBig(x) ? String(x) : x) : String(v);
const isoFor = d => ymdOf(d.getTime()) + ' ' + hmsOf(d.getTime());
const asTs = v => isDate(v) ? v.getTime() : typeof v === 'string' ? parseTs(v) : NaN;
function cmpVals(a, b) {
  if (isNull(a) || isNull(b)) return null;
  if (isDate(a) || isDate(b)) {
    const x = asTs(a), y = asTs(b);
    if (!isNaN(x) && !isNaN(y)) return x < y ? -1 : x > y ? 1 : 0;
    const s = toStr(a), t = toStr(b); return s < t ? -1 : s > t ? 1 : 0;
  }
  if (typeof a === 'string' && typeof b === 'string') return a < b ? -1 : a > b ? 1 : 0;
  const x = toNum(a), y = toNum(b);
  if (!isNaN(x) && !isNaN(y)) return x < y ? -1 : x > y ? 1 : 0;
  const s = toStr(a), t = toStr(b); return s < t ? -1 : s > t ? 1 : 0;
}
const eqVals = (a, b) => { const c = cmpVals(a, b); return c === null ? null : c === 0; };
function likeRe(p, ci) {
  let re = '^';
  for (let i = 0; i < p.length; i++) { const c = p[i]; if (c === '\\' && i + 1 < p.length) re += p[++i].replace(/[.*+?^${}()|[\]\\]/g, '\\$&'); else if (c === '%') re += '[\\s\\S]*'; else if (c === '_') re += '[\\s\\S]'; else re += c.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'); }
  return new RegExp(re + '$', ci ? 'i' : '');
}
const r2 = (v, n) => { const f = Math.pow(10, n || 0); return Math.round((v + Number.EPSILON * Math.sign(v)) * f) / f; };
function truncTs(unit, ms) {
  const p = lparts(ms), L = (y, mo, d, h, mi, s) => Date.UTC(y, mo - 1, d, h, mi, s) - TZ;
  switch (unit) {
    case 'second': return L(p.y, p.mo, p.d, p.h, p.mi, p.s); case 'minute': return L(p.y, p.mo, p.d, p.h, p.mi, 0); case 'hour': return L(p.y, p.mo, p.d, p.h, 0, 0);
    case 'day': return L(p.y, p.mo, p.d, 0, 0, 0); case 'week': return L(p.y, p.mo, p.d - ((p.wd + 6) % 7), 0, 0, 0);
    case 'month': return L(p.y, p.mo, 1, 0, 0, 0); case 'quarter': return L(p.y, Math.floor((p.mo - 1) / 3) * 3 + 1, 1, 0, 0, 0); case 'year': return L(p.y, 1, 1, 0, 0, 0);
  }
  throw bad(`date_trunc 的单位 "${unit}" 无法识别`);
}
function fmtTime(ms, f, dia) {
  const p = lparts(ms);
  if (dia === 'ch') {
    return String(f).replace(/%([YymdHMSjWwfeTFDhIpkBbAa%])/g, (m, c) => ({Y: p.y, y: pad(p.y % 100), m: pad(p.mo), d: pad(p.d), H: pad(p.h), M: pad(p.mi), S: pad(p.s), e: p.d, T: `${pad(p.h)}:${pad(p.mi)}:${pad(p.s)}`, F: `${p.y}-${pad(p.mo)}-${pad(p.d)}`, D: `${pad(p.mo)}/${pad(p.d)}/${pad(p.y % 100)}`, '%': '%', i: pad(p.mi)})[c]);
  }
  return String(f).replace(/YYYY|YY|MM|DD|HH24|HH12|HH|MI|SS|MS|Dy|Day|Mon/g, m => ({YYYY: p.y, YY: pad(p.y % 100), MM: pad(p.mo), DD: pad(p.d), HH24: pad(p.h), HH12: pad(p.h % 12 || 12), HH: pad(p.h % 12 || 12), MI: pad(p.mi), SS: pad(p.s), MS: pad(p.ms, 3), Dy: ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'][p.wd], Day: ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'][p.wd], Mon: ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][p.mo - 1]})[m]);
}
function castTo(v, type, dia) {
  if (isNull(v)) return null;
  const t = type.replace(/\[\]$/, '');
  if (/\[\]$/.test(type)) return Array.isArray(v) ? v : [v];
  if (/^(int|integer|int4|int2|smallint|bigint|int8|uint\d+|int\d+|toint)/.test(t)) { const n = Math.trunc(toNum(v)); if (isNaN(n)) throw bad(`invalid input syntax for type integer: "${v}"`); return n; }
  if (/^(numeric|decimal|float|double|real|float\d+)/.test(t)) { const n = toNum(v); if (isNaN(n)) throw bad(`invalid input syntax for type numeric: "${v}"`); return n; }
  if (/^(text|varchar|character|char|string|bpchar)/.test(t)) return toStr(v);
  if (t === 'date') { const ms = asTs(v); if (isNaN(ms)) throw bad(`invalid input syntax for type date: "${v}"`); return ldate(ms); }
  if (/^(timestamp|timestamptz|datetime)/.test(t)) { const ms = asTs(v); if (isNaN(ms)) throw bad(`invalid input syntax for type timestamp: "${v}"`); return new Date(ms); }
  if (/^bool/.test(t)) return typeof v === 'boolean' ? v : /^(t|true|1|yes|on)$/i.test(String(v));
  if (/^json/.test(t)) { if (typeof v === 'string') { try { return JSON.parse(v); } catch (e) { throw bad(`invalid input syntax for type json`); } } return v; }
  return v;
}
function asDate(v) { const ms = asTs(v); return isNaN(ms) ? null : new Date(ms); }
function jget(v, k) {
  if (isNull(v)) return null;
  if (typeof v === 'string') { try { v = JSON.parse(v); } catch (e) { return null; } }
  if (Array.isArray(v)) { const i = typeof k === 'number' ? k : Number(k); return v[i < 0 ? v.length + i : i] === undefined ? null : v[i < 0 ? v.length + i : i]; }
  if (v && typeof v === 'object') return Object.prototype.hasOwnProperty.call(v, k) ? v[k] : null;
  return null;
}
const jpath = (v, path) => { path = String(path).replace(/^\{|\}$/g, '').split(','); let cur = v; for (const p of path) { cur = jget(cur, p.trim()); if (cur === null) return null; } return cur; };
function jcontains(a, b) {
  if (isNull(a) || isNull(b)) return null;
  if (typeof a === 'string') { try { a = JSON.parse(a); } catch (e) { return false; } }
  if (typeof b === 'string') { try { b = JSON.parse(b); } catch (e) { return false; } }
  const go = (x, y) => {
    if (Array.isArray(y)) return Array.isArray(x) ? y.every(e => x.some(f => go(f, e))) : y.every(e => go(x, e));
    if (y && typeof y === 'object') return x && typeof x === 'object' && !Array.isArray(x) && Object.keys(y).every(k => k in x && go(x[k], y[k]));
    return x === y;
  };
  return go(a, b);
}
const addMs = (v, ms, sign) => { const t = asTs(v); return isNaN(t) ? null : new Date(t + sign * ms); };

/* ---- 标量函数：name → (args, cx) => 值（args 已求值）---- */
const F = {
  lower: a => isNull(a[0]) ? null : String(toStr(a[0])).toLowerCase(), upper: a => isNull(a[0]) ? null : String(toStr(a[0])).toUpperCase(),
  lowerutf8: a => F.lower(a), upperutf8: a => F.upper(a),
  length: a => isNull(a[0]) ? null : Array.isArray(a[0]) ? a[0].length : [...String(toStr(a[0]))].length, char_length: a => F.length(a), octet_length: a => isNull(a[0]) ? null : new TextEncoder().encode(String(toStr(a[0]))).length, lengthutf8: a => F.length(a),
  concat: a => a.some(isNull) && !0 ? a.filter(x => !isNull(x)).map(toStr).join('') : a.map(toStr).join(''),
  concat_ws: a => a.slice(1).filter(x => !isNull(x)).map(toStr).join(toStr(a[0])),
  substr: a => { const s = toStr(a[0]); if (s === null) return null; const st = Math.max(1, toNum(a[1])), len = a.length > 2 ? toNum(a[2]) : undefined; return [...s].slice(st - 1, len === undefined ? undefined : st - 1 + len).join(''); },
  left: a => isNull(a[0]) ? null : toNum(a[1]) >= 0 ? [...toStr(a[0])].slice(0, toNum(a[1])).join('') : [...toStr(a[0])].slice(0, toNum(a[1])).join(''),
  right: a => isNull(a[0]) ? null : toNum(a[1]) >= 0 ? [...toStr(a[0])].slice(-toNum(a[1]) || undefined).join('') : [...toStr(a[0])].slice(-toNum(a[1])).join(''),
  btrim_both: a => isNull(a[0]) ? null : a[1] ? toStr(a[0]).replace(new RegExp('^[' + toStr(a[1]).replace(/[\]\\^-]/g, '\\$&') + ']+|[' + toStr(a[1]).replace(/[\]\\^-]/g, '\\$&') + ']+$', 'g'), '') : toStr(a[0]).trim(),
  btrim_leading: a => isNull(a[0]) ? null : toStr(a[0]).replace(/^\s+/, ''), btrim_trailing: a => isNull(a[0]) ? null : toStr(a[0]).replace(/\s+$/, ''),
  btrim: a => F.btrim_both(a), ltrim: a => F.btrim_leading(a), rtrim: a => F.btrim_trailing(a), trimboth: a => F.btrim_both(a),
  replace: a => isNull(a[0]) ? null : toStr(a[0]).split(toStr(a[1])).join(toStr(a[2])), replaceall: a => F.replace(a), replaceone: a => isNull(a[0]) ? null : toStr(a[0]).replace(toStr(a[1]), toStr(a[2])),
  split_part: a => isNull(a[0]) ? null : (toStr(a[0]).split(toStr(a[1]))[toNum(a[2]) - 1] || ''),
  position: a => isNull(a[0]) ? null : toStr(a[1]).indexOf(toStr(a[0])) + 1, strpos: a => isNull(a[0]) ? null : toStr(a[0]).indexOf(toStr(a[1])) + 1,
  repeat: a => { const n = toNum(a[1]); if (n * String(a[0]).length > 5e6) throw new EErr('memory', 'engine_error'); return isNull(a[0]) ? null : String(a[0]).repeat(Math.max(0, n)); },
  reverse: a => isNull(a[0]) ? null : [...toStr(a[0])].reverse().join(''), md5: a => isNull(a[0]) ? null : hex(R('md5' + toStr(a[0])), 32),
  lpad: a => toStr(a[0]).padStart(toNum(a[1]), a.length > 2 ? toStr(a[2]) : ' '), rpad: a => toStr(a[0]).padEnd(toNum(a[1]), a.length > 2 ? toStr(a[2]) : ' '),
  initcap: a => isNull(a[0]) ? null : toStr(a[0]).replace(/\b\w/g, c => c.toUpperCase()),
  coalesce: a => { for (const x of a) if (!isNull(x)) return x; return null; }, ifnull: a => isNull(a[0]) ? a[1] : a[0], nullif: a => eqVals(a[0], a[1]) ? null : a[0],
  greatest: a => a.filter(x => !isNull(x)).reduce((m, x) => cmpVals(x, m) > 0 ? x : m), least: a => a.filter(x => !isNull(x)).reduce((m, x) => cmpVals(x, m) < 0 ? x : m),
  abs: a => isNull(a[0]) ? null : Math.abs(toNum(a[0])), sign: a => Math.sign(toNum(a[0])), sqrt: a => Math.sqrt(toNum(a[0])), power: a => Math.pow(toNum(a[0]), toNum(a[1])), pow: a => F.power(a), exp: a => Math.exp(toNum(a[0])), ln: a => Math.log(toNum(a[0])), log10: a => Math.log10(toNum(a[0])), log: a => a.length > 1 ? Math.log(toNum(a[1])) / Math.log(toNum(a[0])) : Math.log10(toNum(a[0])),
  round: a => isNull(a[0]) ? null : r2(toNum(a[0]), a.length > 1 ? toNum(a[1]) : 0), floor: a => isNull(a[0]) ? null : Math.floor(toNum(a[0])), ceil: a => isNull(a[0]) ? null : Math.ceil(toNum(a[0])), ceiling: a => F.ceil(a), trunc: a => isNull(a[0]) ? null : (isDate(a[0]) ? a[0] : Math.trunc(toNum(a[0]))),
  mod: a => toNum(a[0]) % toNum(a[1]), modulo: a => toNum(a[0]) % toNum(a[1]), intdiv: a => Math.trunc(toNum(a[0]) / toNum(a[1])), div: a => Math.trunc(toNum(a[0]) / toNum(a[1])),
  now: () => new Date(NOW_MS), current_timestamp: () => new Date(NOW_MS), localtimestamp: () => new Date(NOW_MS), transaction_timestamp: () => new Date(NOW_MS), statement_timestamp: () => new Date(NOW_MS), clock_timestamp: () => new Date(NOW_MS),
  current_date: () => ldate(NOW_MS), today: () => ldate(NOW_MS), yesterday: () => ldate(NOW_MS - DAY), toyyyymm: a => { const p = lparts(asTs(a[0])); return p.y * 100 + p.mo; }, toyyyymmdd: a => { const p = lparts(asTs(a[0])); return p.y * 10000 + p.mo * 100 + p.d; },
  date_trunc: a => isNull(a[1]) ? null : new Date(truncTs(String(toStr(a[0])).toLowerCase(), asTs(a[1]))),
  todate: a => isNull(a[0]) ? null : ldate(asTs(a[0])), todatetime: a => isNull(a[0]) ? null : new Date(asTs(a[0])), todatetime64: a => F.todatetime(a),
  tostartofday: a => new Date(truncTs('day', asTs(a[0]))), tostartofhour: a => new Date(truncTs('hour', asTs(a[0]))), tostartofminute: a => new Date(truncTs('minute', asTs(a[0]))), tostartofweek: a => ldate(truncTs('week', asTs(a[0]))), tostartofmonth: a => ldate(truncTs('month', asTs(a[0]))), tostartofyear: a => ldate(truncTs('year', asTs(a[0]))), tostartofquarter: a => ldate(truncTs('quarter', asTs(a[0]))), tostartofinterval: a => new Date(truncTs('day', asTs(a[0]))),
  tohour: a => lparts(asTs(a[0])).h, tominute: a => lparts(asTs(a[0])).mi, tosecond: a => lparts(asTs(a[0])).s, today_: () => 0, todayofmonth: a => lparts(asTs(a[0])).d, tomonth: a => lparts(asTs(a[0])).mo, toyear: a => lparts(asTs(a[0])).y, todayofweek: a => { const w = lparts(asTs(a[0])).wd; return w === 0 ? 7 : w; },
  formatdatetime: a => isNull(a[0]) ? null : fmtTime(asTs(a[0]), toStr(a[1]), 'ch'), to_char: a => isNull(a[0]) ? null : isDate(a[0]) || typeof a[0] === 'string' && !isNaN(asTs(a[0])) ? fmtTime(asTs(a[0]), toStr(a[1]), 'pg') : String(toNum(a[0]).toFixed(Math.max(0, (String(a[1]).split('.')[1] || '').length))),
  datediff: a => Math.floor((asTs(a[2]) - asTs(a[1])) / ({second: 1e3, minute: 60e3, hour: 3600e3, day: DAY, week: 7 * DAY})[String(a[0]).toLowerCase()]), datesub: a => new Date(asTs(a[2]) - toNum(a[1]) * ({day: DAY, hour: 3600e3, minute: 60e3, week: 7 * DAY})[String(a[0]).toLowerCase()]), dateadd: a => new Date(asTs(a[2]) + toNum(a[1]) * ({day: DAY, hour: 3600e3, minute: 60e3, week: 7 * DAY})[String(a[0]).toLowerCase()]),
  extract: a => { const t = asTs(a[1]); if (isNaN(t)) return null; const p = lparts(t), f = String(a[0]).toLowerCase(); return ({year: p.y, month: p.mo, day: p.d, hour: p.h, minute: p.mi, second: p.s, dow: p.wd, isodow: p.wd || 7, epoch: Math.floor(t / 1000), doy: Math.floor((ldate(t).getTime() - (Date.UTC(p.y, 0, 1) - TZ)) / DAY) + 1, quarter: Math.floor((p.mo - 1) / 3) + 1, week: 1 + Math.floor(((ldate(t).getTime() - (Date.UTC(p.y, 0, 1) - TZ)) / DAY) / 7)})[f]; },
  date_part: a => F.extract([a[0], a[1]]), to_timestamp: a => typeof a[0] === 'number' ? new Date(a[0] * 1000) : asDate(a[0]), to_date: a => { const d = asDate(a[0]); return d ? ldate(d.getTime()) : null; },
  age: a => a.length > 1 ? {ms: asTs(a[0]) - asTs(a[1])} : {ms: NOW_MS - asTs(a[0])},
  tostring: a => toStr(a[0]), toint8: a => Math.trunc(toNum(a[0])), toint16: a => Math.trunc(toNum(a[0])), toint32: a => Math.trunc(toNum(a[0])), toint64: a => Math.trunc(toNum(a[0])), touint8: a => Math.trunc(toNum(a[0])), touint16: a => Math.trunc(toNum(a[0])), touint32: a => Math.trunc(toNum(a[0])), touint64: a => Math.trunc(toNum(a[0])), tofloat32: a => toNum(a[0]), tofloat64: a => toNum(a[0]), todecimal64: a => toNum(a[0]), todecimal32: a => toNum(a[0]), todecimal128: a => toNum(a[0]),
  if: a => (a[0] === true || (typeof a[0] === 'number' && a[0] !== 0)) ? a[1] : a[2],
  multiif: a => { for (let i = 0; i + 1 < a.length; i += 2) if (a[i] === true || (typeof a[i] === 'number' && a[i] !== 0)) return a[i + 1]; return a.length % 2 ? a[a.length - 1] : null; },
  isnull: a => isNull(a[0]), isnotnull: a => !isNull(a[0]), empty: a => isNull(a[0]) || a[0] === '' || (Array.isArray(a[0]) && !a[0].length), notempty: a => !F.empty(a),
  has: a => Array.isArray(a[0]) && a[0].some(x => eqVals(x, a[1])), arrayjoin: () => { throw new NoSim('arrayJoin'); },
  array_length: a => Array.isArray(a[0]) ? a[0].length : null, cardinality: a => Array.isArray(a[0]) ? a[0].length : null, array_to_string: a => Array.isArray(a[0]) ? a[0].join(toStr(a[1])) : null, arraystringconcat: a => Array.isArray(a[0]) ? a[0].join(a.length > 1 ? toStr(a[1]) : '') : null,
  splitbychar: a => isNull(a[1]) ? null : toStr(a[1]).split(toStr(a[0])), string_to_array: a => isNull(a[0]) ? null : toStr(a[0]).split(toStr(a[1])),
  jsonb_array_length: a => { const v = typeof a[0] === 'string' ? JSON.parse(a[0]) : a[0]; return Array.isArray(v) ? v.length : null; }, json_array_length: a => F.jsonb_array_length(a),
  jsonb_typeof: a => { const v = a[0]; return v === null ? 'null' : Array.isArray(v) ? 'array' : typeof v === 'object' ? 'object' : typeof v === 'number' ? 'number' : typeof v === 'boolean' ? 'boolean' : 'string'; }, json_typeof: a => F.jsonb_typeof(a),
  jsonb_build_object: a => { const o = {}; for (let i = 0; i + 1 < a.length; i += 2) o[toStr(a[i])] = a[i + 1]; return o; }, json_build_object: a => F.jsonb_build_object(a), to_jsonb: a => a[0], to_json: a => a[0],
  version: (a, cx) => cx.dia === 'ch' ? cx.inst.ver : `PostgreSQL ${cx.inst.ver} on x86_64-pc-linux-gnu, compiled by gcc (GCC) 11.4.1 20230605, 64-bit`,
  current_database: (a, cx) => cx.db, currentdatabase: (a, cx) => cx.db, current_schema: () => 'public', current_user: (a, cx) => cx.inst.user, session_user: (a, cx) => cx.inst.user, current_catalog: (a, cx) => cx.db, user: (a, cx) => cx.inst.user, hostname: (a, cx) => cx.inst.host.replace(/\./g, '-'),
  current_setting: a => ({server_version: '15.4', server_encoding: 'UTF8', timezone: 'Asia/Shanghai', 'transaction_read_only': 'on', statement_timeout: '30s', max_connections: '500', work_mem: '16MB', default_transaction_read_only: 'on', search_path: '"public", pg_catalog', datestyle: 'ISO, MDY'})[String(a[0]).toLowerCase()] ?? (() => { throw bad(`unrecognized configuration parameter "${a[0]}"`); })(),
  txid_current: () => 4821330, pg_backend_pid: () => 18233, pg_postmaster_start_time: () => new Date(NOW_MS - 41 * DAY), pg_is_in_recovery: () => false, pg_database_size: () => 18253611008, pg_size_pretty: a => { let n = toNum(a[0]); const u = ['bytes', 'kB', 'MB', 'GB', 'TB']; let i = 0; while (n >= 1024 && i < 4) { n /= 1024; i++; } return `${i ? Math.round(n) : n} ${u[i]}`; },
  pg_relation_size: () => 7340000000, pg_total_relation_size: () => 9120000000, has_table_privilege: () => true, obj_description: () => null, pg_get_userbyid: () => 'app_order',
  uniq: null, ascii: a => String(a[0]).charCodeAt(0), chr: a => String.fromCharCode(toNum(a[0])), toyear_: null, generate_series: null,
  dictget: (a, cx) => dictGetFn(a, cx), dictgetordefault: (a, cx) => dictGetFn(a.slice(0, 3), cx) ?? a[3], dictgetorNull: null,
  tuple: a => a, nan: () => NaN, rand: () => 0, random: () => 0.5,
};
delete F.uniq; delete F.toyear_; delete F.generate_series; delete F.dictgetorNull; delete F.today_;
function dictGetFn(a, cx) {
  const nm = String(a[0]).split('.').pop(), key = toNum(a[2]);
  const td = tdOf(cx.inst, cx.db, nm) || (cx.inst.schema ? ((SCHEMAS[cx.inst.schema] || []).flatMap(d => d.tables.map(t => tdOf(cx.inst, d.db, t.n))).find(x => x && x.n === nm)) : null);
  if (!td) throw new EErr(`ClickHouse 错误 36（参数不合法）：Dictionary (\`${a[0]}\`) not found. (BAD_ARGUMENTS)`, 'engine_error');
  const idx = key - (td.kind === 'dict' && td.n === 'dict_city' ? 110000 : 0) / 100 - (td.n === 'dict_channel' ? 1 : 0);
  const i = td.n === 'dict_city' ? (key - 110000) / 100 : key - 1;
  if (!(i >= 0 && i < td.total)) return null;
  const row = rowAt(td, i), col = String(a[1]); return row[col] === undefined ? null : row[col];
}

/* ---- 聚合 ---- */
const isAggFn = e => e && e.k === 'fn' && AGG_FUNCS.has(e.name) && !(e.name === 'any' && false);
function hasAgg(e) {
  if (!e || typeof e !== 'object') return false;
  if (e.k === 'fn' && AGG_FUNCS.has(e.name)) return true;
  if (e.k === 'sub' || e.k === 'exists') return false;
  for (const key of ['l', 'r', 'e', 'lo', 'hi', 'pat', 'i', 'operand', 'els']) if (e[key] && hasAgg(e[key])) return true;
  for (const key of ['args', 'items', 'list']) if (e[key] && e[key].some(hasAgg)) return true;
  if (e.whens && e.whens.some(([w, t]) => hasAgg(w) || hasAgg(t))) return true;
  return false;
}
function aggregate(e, grp, cx) {
  let rows = grp.rows; const name = e.name;
  if (e.orderBy) { const ks = rows.map(env => ({env, k: e.orderBy.map(o => evalExpr(o.e, env, cx))})); ks.sort((a, b) => { for (let i = 0; i < e.orderBy.length; i++) { const c = cmpVals(a.k[i], b.k[i]); if (c) return c * e.orderBy[i].dir; } return 0; }); rows = ks.map(x => x.env); }
  const vals = () => { let vs = rows.map(env => evalExpr(e.args[0], env, cx)); if (e.filter) vs = rows.map(env => ({env, v: evalExpr(e.args[0], env, cx)})).filter(x => evalExpr(e.filter, x.env, cx) === true).map(x => x.v); return vs; };
  const sc = grp.synth ? 1 : cx.scale;
  switch (name) {
    case 'count': {
      if (e.star || !e.args.length) { let n = e.filter ? rows.filter(env => evalExpr(e.filter, env, cx) === true).length : rows.length; return Math.round(n * sc); }
      let vs = vals().filter(v => !isNull(v)); if (e.distinct) vs = [...new Set(vs.map(toStr))];
      return Math.round(vs.length * (e.distinct ? Math.pow(sc, 0.7) : sc));
    }
    case 'sum': { const vs = vals().filter(v => !isNull(v)).map(toNum); return vs.length ? r2(vs.reduce((a, b) => a + b, 0) * sc, 4) : null; }
    case 'avg': { const vs = vals().filter(v => !isNull(v)).map(toNum); return vs.length ? vs.reduce((a, b) => a + b, 0) / vs.length : null; }
    case 'min': case 'max': { const vs = vals().filter(v => !isNull(v)); if (!vs.length) return null; return vs.reduce((m, x) => (name === 'min' ? cmpVals(x, m) < 0 : cmpVals(x, m) > 0) ? x : m); }
    case 'uniq': case 'uniqexact': case 'uniqcombined': case 'uniqhll12': { const vs = vals().filter(v => !isNull(v)); return Math.round(new Set(vs.map(toStr)).size * Math.pow(sc, 0.7)); }
    case 'any': case 'anylast': { const vs = vals(); const v = name === 'any' ? vs[0] : vs[vs.length - 1]; return v === undefined ? null : v; }
    case 'array_agg': case 'grouparray': case 'json_agg': case 'jsonb_agg': { const vs = vals(); return e.distinct ? [...new Set(vs.map(toStr))].map(x => x) : vs.slice(0, 1000); }
    case 'string_agg': { const vs = rows.map(env => evalExpr(e.args[0], env, cx)).filter(v => !isNull(v)); return vs.length ? vs.map(toStr).join(toStr(evalExpr(e.args[1], grp.rows[0] || {}, cx))) : null; }
    case 'argmax': case 'argmin': { let best = null, bv = null; for (const env of rows) { const k = evalExpr(e.args[1], env, cx); if (isNull(k)) continue; if (best === null || (name === 'argmax' ? cmpVals(k, bv) > 0 : cmpVals(k, bv) < 0)) { best = env; bv = k; } } return best ? evalExpr(e.args[0], best, cx) : null; }
    case 'quantile': case 'median': { const vs = vals().filter(v => !isNull(v)).map(toNum).sort((a, b) => a - b); const p = name === 'median' ? 0.5 : (e.params ? toNum(evalExpr(e.params[0], {}, cx)) : 0.5); return vs.length ? vs[Math.min(vs.length - 1, Math.floor(p * vs.length))] : null; }
    case 'bool_and': return vals().every(v => v === true); case 'bool_or': return vals().some(v => v === true);
    case 'stddev': case 'variance': { const vs = vals().filter(v => !isNull(v)).map(toNum); if (vs.length < 2) return null; const m = vs.reduce((a, b) => a + b, 0) / vs.length, v = vs.reduce((a, b) => a + (b - m) * (b - m), 0) / (vs.length - 1); return name === 'variance' ? v : Math.sqrt(v); }
  }
  throw new NoSim('聚合函数 ' + name + '()');
}

/* ---- 表达式求值 ---- */
function colErr(cx, name, q) {
  if (cx.dia === 'ch') return new EErr(`ClickHouse 错误 47（未知的列或标识符）：Unknown expression or function identifier \`${q ? q + '.' : ''}${name}\` in scope SELECT ... (UNKNOWN_IDENTIFIER)`, 'engine_error');
  return new EErr(`PostgreSQL 报错：column ${q ? q + '.' : ''}${name} does not exist [42703]`.replace('column ', 'column "').replace(' does not', '" does not').replace('"' + (q ? q + '.' : ''), '"' + (q ? q + '.' : '')), 'bad_query', '');
}
function lookupCol(env, parts, cx) {
  let q = null, n;
  if (parts.length >= 2) { q = parts[parts.length - 2]; n = parts[parts.length - 1]; } else n = parts[0];
  const al = env.__al;
  if (q) {
    let r = env[q]; if (!r && cx.dia === 'pg') { const k = al.find(a => a.toLowerCase() === q.toLowerCase()); r = k && env[k]; }
    if (!r) { const k = al.find(a => env[a].__t === q); r = k && env[k]; }
    if (!r && cx.outer) return lookupCol(cx.outer, parts, Object.assign(Object.create(cx), {outer: null}));
    if (!r) throw cx.dia === 'ch' ? new EErr(`ClickHouse 错误 47（未知的列或标识符）：Unknown expression or function identifier \`${q}.${n}\` in scope SELECT (UNKNOWN_IDENTIFIER)`, 'engine_error') : bad(`PostgreSQL 报错：missing FROM-clause entry for table "${q}" [42P01]`);
    if (n in r) return r[n];
    const kk = Object.keys(r).find(k => k.toLowerCase() === n.toLowerCase());
    if (kk && cx.dia === 'pg') return r[kk];
    throw colErr(cx, n, q);
  }
  for (const a of al) if (n in env[a]) return env[a][n];
  if (cx.dia === 'pg') for (const a of al) { const kk = Object.keys(env[a]).find(k => k.toLowerCase() === n.toLowerCase()); if (kk) return env[a][kk]; }
  if (cx.outAlias && n in cx.outAlias) return cx.outAlias[n](env);
  if (cx.outer) return lookupCol(cx.outer, parts, Object.assign(Object.create(cx), {outer: cx.outerUp || null}));
  if (parts.length === 1 && cx.dia === 'pg') {
    const lw = n.toLowerCase();
    if (lw === 'current_date') return ldate(NOW_MS);
  }
  throw colErr(cx, n);
}
function truthy(v) { return v === true || (typeof v === 'number' && v !== 0); }
function evalExpr(e, env, cx, grp) {
  switch (e.k) {
    case 'num': return e.v;
    case 'str': return e.v;
    case 'null': return null;
    case 'bool': return e.v;
    case 'paren': return evalExpr(e.e, env, cx, grp);
    case 'col': return lookupCol(env, e.parts, cx);
    case 'star': throw bad('* 不能出现在这里');
    case 'tuple': return e.items.map(x => evalExpr(x, env, cx, grp));
    case 'arr': return e.items.map(x => evalExpr(x, env, cx, grp));
    case 'interval': return {ms: e.ms};
    case 'cast': return castTo(evalExpr(e.e, env, cx, grp), e.type, cx.dia);
    case 'idx': { const a = evalExpr(e.e, env, cx, grp), iv = evalExpr(e.i, env, cx, grp); if (a && typeof a === 'object' && !Array.isArray(a) && !isDate(a)) { const v = a[String(iv)]; return v === undefined ? (cx.dia === 'ch' ? '' : null) : v; } const i = toNum(iv); return Array.isArray(a) ? (a[i - 1] === undefined ? null : a[i - 1]) : null; }
    case 'un': {
      const v = evalExpr(e.e, env, cx, grp);
      if (e.op === 'not') return isNull(v) ? null : !truthy(v);
      return isNull(v) ? null : -toNum(v);
    }
    case 'bin': return evalBin(e, env, cx, grp);
    case 'isnull': { const v = evalExpr(e.e, env, cx, grp); return e.neg ? !isNull(v) : isNull(v); }
    case 'quant': { const l = evalExpr(e.l, env, cx, grp); let arr = evalExpr(e.r, env, cx, grp); if (!Array.isArray(arr)) arr = [arr]; if (isNull(l)) return null; const t = x => evalBin({k: 'bin', op: e.op, l: {k: 'lit', v: l}, r: {k: 'lit', v: x}}, env, cx, grp); const rs = arr.map(t); return e.all ? rs.every(x => x === true) : rs.some(x => x === true); }
    case 'lit': return e.v;
    case 'between': { const v = evalExpr(e.e, env, cx, grp), lo = evalExpr(e.lo, env, cx, grp), hi = evalExpr(e.hi, env, cx, grp); const a = cmpVals(v, lo), b = cmpVals(v, hi); if (a === null || b === null) return null; const r = a >= 0 && b <= 0; return e.neg ? !r : r; }
    case 'like': { const v = evalExpr(e.e, env, cx, grp), p = evalExpr(e.pat, env, cx, grp); if (isNull(v) || isNull(p)) return null; const r = likeRe(toStr(p), e.ci).test(toStr(v)); return e.neg ? !r : r; }
    case 'in': {
      const v = evalExpr(e.e, env, cx, grp); if (isNull(v)) return null;
      let list;
      if (e.sub) list = subSet(e.sub, cx); else if (e.arr) { const a = evalExpr(e.arr, env, cx, grp); list = Array.isArray(a) ? a : [a]; } else list = e.list.map(x => evalExpr(x, env, cx, grp));
      if (Array.isArray(v)) { const hit = list.some(x => Array.isArray(x) && x.length === v.length && x.every((y, i) => eqVals(y, v[i]))); return e.neg ? !hit : hit; }
      const hit = list.some(x => eqVals(v, x) === true);
      return e.neg ? !hit : hit;
    }
    case 'case': {
      const op = e.operand ? evalExpr(e.operand, env, cx, grp) : undefined;
      for (const [w, t] of e.whens) { const c = op === undefined ? truthy(evalExpr(w, env, cx, grp)) : eqVals(op, evalExpr(w, env, cx, grp)) === true; if (c) return evalExpr(t, env, cx, grp); }
      return e.els ? evalExpr(e.els, env, cx, grp) : null;
    }
    case 'sub': { const rs = subQuery(e.q, cx, env); if (e.arr) return rs.rows.map(r => r[0]); if (rs.rows.length > 1) throw bad('PostgreSQL 报错：more than one row returned by a subquery used as an expression [21000]'); return rs.rows.length ? rs.rows[0][0] : null; }
    case 'exists': return subQuery(e.q, cx, env).rows.length > 0;
    case 'fn': {
      if (AGG_FUNCS.has(e.name) && !(e.name === 'any' && !grp && false)) {
        if (!grp) throw cx.dia === 'ch' ? new EErr(`ClickHouse 错误 184（聚合函数位置不合法）：Aggregate function ${e.rawName}() is found in WHERE in query (ILLEGAL_AGGREGATION)`, 'engine_error') : bad(`PostgreSQL 报错：aggregate functions are not allowed in WHERE [42803]`);
        return aggregate(e, grp, cx);
      }
      if (e.name === 'coalesce' || e.name === 'ifnull') { for (const a of e.args) { const v = evalExpr(a, env, cx, grp); if (!isNull(v)) return v; } return null; }
      if (e.name === 'if' || e.name === 'multiif') { const a = e.args.map(x => () => evalExpr(x, env, cx, grp)); if (e.name === 'if') return truthy(a[0]()) ? a[1]() : a[2](); for (let i = 0; i + 1 < a.length; i += 2) if (truthy(a[i]())) return a[i + 1](); return a.length % 2 ? a[a.length - 1]() : null; }
      const args = e.args.map(x => evalExpr(x, env, cx, grp));
      const f = F[e.name];
      if (!f) {
        if (e.name === 'unnest' || e.name === 'generate_series') throw new NoSim(`SELECT 列表里的集合返回函数 ${e.name}()（请写在 FROM 里）`);
        throw cx.dia === 'ch' ? new EErr(`ClickHouse 错误 46（未知的函数）：Function with name \`${e.rawName}\` does not exist. (UNKNOWN_FUNCTION)`, 'engine_error') : bad(`PostgreSQL 报错：function ${e.rawName}(${args.map(a => typeName1(a)).join(', ')}) does not exist（提示：No function matches the given name and argument types. You might need to add explicit type casts.） [42883]`);
      }
      return f(args, cx);
    }
  }
  throw new NoSim('表达式 ' + e.k);
}
const typeName1 = v => isNull(v) ? 'unknown' : typeof v === 'number' ? (Number.isInteger(v) ? 'integer' : 'numeric') : typeof v === 'boolean' ? 'boolean' : isDate(v) ? 'timestamp with time zone' : 'text';
function evalBin(e, env, cx, grp) {
  const op = e.op;
  if (op === 'and') { const l = evalExpr(e.l, env, cx, grp); if (l === false || l === 0) return false; const r = evalExpr(e.r, env, cx, grp); if (r === false || r === 0) return false; return (isNull(l) || isNull(r)) ? null : true; }
  if (op === 'or') { const l = evalExpr(e.l, env, cx, grp); if (truthy(l)) return true; const r = evalExpr(e.r, env, cx, grp); if (truthy(r)) return true; return (isNull(l) || isNull(r)) ? null : false; }
  const l = evalExpr(e.l, env, cx, grp), r = evalExpr(e.r, env, cx, grp);
  switch (op) {
    case '=': return eqVals(l, r); case '<>': { const x = eqVals(l, r); return x === null ? null : !x; }
    case '<': { const c = cmpVals(l, r); return c === null ? null : c < 0; } case '>': { const c = cmpVals(l, r); return c === null ? null : c > 0; }
    case '<=': { const c = cmpVals(l, r); return c === null ? null : c <= 0; } case '>=': { const c = cmpVals(l, r); return c === null ? null : c >= 0; }
    case 'isdistinct': return !(isNull(l) && isNull(r)) && (isNull(l) !== isNull(r) || cmpVals(l, r) !== 0);
    case 'isnotdistinct': return (isNull(l) && isNull(r)) || (!isNull(l) && !isNull(r) && cmpVals(l, r) === 0);
    case '||': if (Array.isArray(l) || Array.isArray(r)) return [].concat(l, r); return (isNull(l) || isNull(r)) ? null : toStr(l) + toStr(r);
    case '->': return jget(l, typeof r === 'number' ? r : String(r));
    case '->>': { const v = jget(l, typeof r === 'number' ? r : String(r)); return v === null ? null : typeof v === 'object' ? JSON.stringify(v) : String(v); }
    case '#>': return jpath(l, r); case '#>>': { const v = jpath(l, r); return v === null ? null : typeof v === 'object' ? JSON.stringify(v) : String(v); }
    case '@>': return Array.isArray(l) && Array.isArray(r) ? r.every(x => l.some(y => eqVals(x, y))) : jcontains(l, r);
    case '<@': return Array.isArray(l) && Array.isArray(r) ? l.every(x => r.some(y => eqVals(x, y))) : jcontains(r, l);
    case '&&': return Array.isArray(l) && Array.isArray(r) ? l.some(x => r.some(y => eqVals(x, y))) : null;
    case '?': { const v = typeof l === 'string' ? (() => { try { return JSON.parse(l); } catch (x) { return null; } })() : l; return v && typeof v === 'object' ? (Array.isArray(v) ? v.includes(r) : Object.prototype.hasOwnProperty.call(v, r)) : false; }
    case '~': case '~*': case '!~': case '!~*': { if (isNull(l) || isNull(r)) return null; let re; try { re = new RegExp(toStr(r), op.endsWith('*') ? 'i' : ''); } catch (x) { throw bad(`PostgreSQL 报错：invalid regular expression: ${x.message} [2201B]`); } const m = re.test(toStr(l)); return op[0] === '!' ? !m : m; }
    case '+': case '-': {
      if (isNull(l) || isNull(r)) return null;
      if (l && typeof l === 'object' && !isDate(l) && 'ms' in l) { if (isDate(r)) return addMs(r, l.ms, 1); return {ms: l.ms + (r && r.ms || 0) * (op === '+' ? 1 : -1)}; }
      if (r && typeof r === 'object' && !isDate(r) && 'ms' in r) { if (isDate(l) || typeof l === 'string') return addMs(l, r.ms, op === '+' ? 1 : -1); }
      if (isDate(l) && isDate(r) && op === '-') return {ms: l.getTime() - r.getTime()};
      if (isDate(l) && typeof r === 'number') return new Date(l.getTime() + (op === '+' ? 1 : -1) * r * ((l.getTime() + TZ) % DAY === 0 || cx.dia === 'pg' ? DAY : 1000));
      if (isDate(r) && typeof l === 'number' && op === '+') return new Date(r.getTime() + l * DAY);
      const a = toNum(l), b = toNum(r); if (isNaN(a) || isNaN(b)) throw cx.dia === 'ch' ? new EErr(`ClickHouse 错误 43（类型不匹配）：Illegal types of arguments (String, UInt8) of function ${op === '+' ? 'plus' : 'minus'} (ILLEGAL_TYPE_OF_ARGUMENT)`, 'engine_error') : bad(`PostgreSQL 报错：operator does not exist: text ${op} integer（提示：No operator matches the given name and argument types. You might need to add explicit type casts.） [42883]`);
      return op === '+' ? a + b : a - b;
    }
    case '*': case '/': case '%': {
      if (isNull(l) || isNull(r)) return null;
      const a = toNum(l), b = toNum(r); if (isNaN(a) || isNaN(b)) throw bad(`PostgreSQL 报错：operator does not exist: text ${op} integer [42883]`);
      if (op === '*') return a * b;
      if (b === 0) { if (cx.dia === 'ch') return op === '/' ? Infinity : 0; throw bad('PostgreSQL 报错：division by zero [22012]'); }
      if (op === '/') { if (cx.dia === 'pg' && Number.isInteger(a) && Number.isInteger(b)) { const ta = tyOf(e.l, cx, cx.curSrcs), tb = tyOf(e.r, cx, cx.curSrcs); if ((ta.k === 'int' || ta.k === 'big') && (tb.k === 'int' || tb.k === 'big')) return Math.trunc(a / b); } return a / b; }
      return a % b;
    }
  }
  throw new NoSim('运算符 ' + op);
}
function subSet(q, cx) { return subQuery(q, cx, null).rows.map(r => r[0]); }
function subQuery(q, cx, env) {
  const sub = Object.assign(Object.create(cx), {ctes: new Map(cx.ctes), outer: env});
  return execQuery(q, sub);
}

/* ---- 类型推断（结果列类型）：{k, t(原类型串), scale} ---- */
const T = (k, extra) => Object.assign({k}, extra || {});
function tyOf(e, cx, cols) {
  switch (e.k) {
    case 'num': return T(e.int ? (Math.abs(e.v) < 2147483647 ? 'int' : 'big') : 'dec', {scale: e.int ? 0 : (String(e.raw || e.v).split('.')[1] || '').replace(/[eE].*$/, '').length, lit: true, chT: e.int ? (e.v < 0 ? 'Int8' : e.v < 256 ? 'UInt8' : e.v < 65536 ? 'UInt16' : 'UInt32') : 'Float64'});
    case 'str': return T('str', {lit: true, chT: 'String'});
    case 'null': return T('str', {chT: 'Nullable(Nothing)'});
    case 'bool': return T('bool', {chT: 'Bool'});
    case 'paren': return tyOf(e.e, cx, cols);
    case 'col': { const c = findColDesc(e.parts, cols); return c || T('str'); }
    case 'cast': { const t = e.type; const k = kindOf(t.replace(/^int$/, 'integer')); const chT = ({int4: 'Int32', integer: 'Int32', bigint: 'Int64', text: 'String', date: 'Date', numeric: 'Decimal(38, 4)'})[t]; return T(/^(timestamp|timestamptz|datetime)/.test(t) ? 'ts' : /^(text|varchar|character|char|string)/.test(t) ? 'str' : /^json/.test(t) ? 'obj' : k, {pgT: t, chT}); }
    case 'bin': {
      if (['=', '<>', '<', '>', '<=', '>=', 'and', 'or', '~', '~*', '!~', '!~*', '@>', '<@', '&&', '?', 'isdistinct', 'isnotdistinct'].includes(e.op)) return T('bool', {chT: 'UInt8'});
      if (e.op === '||') return T('str', {chT: 'String'});
      if (e.op === '->') return T('obj', {chT: 'String'}); if (e.op === '->>' || e.op === '#>>') return T('str', {chT: 'String'}); if (e.op === '#>') return T('obj');
      const a = tyOf(e.l, cx, cols), b = tyOf(e.r, cx, cols);
      if (a.k === 'date' || a.k === 'ts') return (b.k === 'int' || b.k === 'big' || b.pgT === 'interval') ? a : (b.k === 'date' || b.k === 'ts') ? T('str', {pgT: 'interval'}) : T('str');
      const rank = {int: 1, big: 2, dec: 3, float: 4}; const ra = rank[a.k] || 0, rb = rank[b.k] || 0;
      const k = Math.max(ra, rb) === 4 ? 'float' : Math.max(ra, rb) === 3 ? 'dec' : Math.max(ra, rb) === 2 || (e.op === '*') ? 'big' : 'int';
      if (e.op === '/' && cx.dia === 'ch') return T('float', {chT: 'Float64'});
      return T(k, {scale: e.op === '/' && cx.dia === 'pg' ? 16 : Math.max(a.scale || 0, b.scale || 0), chT: k === 'float' ? 'Float64' : k === 'dec' ? `Decimal(38, ${Math.max(a.scale || 0, b.scale || 0)})` : k === 'big' ? 'Int64' : 'Int32'});
    }
    case 'un': return e.op === 'not' ? T('bool', {chT: 'UInt8'}) : tyOf(e.e, cx, cols);
    case 'isnull': case 'like': case 'in': case 'between': case 'exists': case 'quant': return T('bool', {chT: 'UInt8'});
    case 'case': { for (const [, t] of e.whens) { const x = tyOf(t, cx, cols); if (!x.lit || x.k !== 'str') return x; } return e.whens.length ? tyOf(e.whens[0][1], cx, cols) : T('str'); }
    case 'sub': return e.arr ? T('arr') : T('str');
    case 'arr': return T('arr', {chT: 'Array(' + (e.items.length ? (tyOf(e.items[0], cx, cols).chT || 'String') : 'Nothing') + ')'});
    case 'interval': return T('str', {pgT: 'interval'});
    case 'idx': return T('str');
    case 'fn': return fnTy(e, cx, cols);
  }
  return T('str');
}
function findColDesc(parts, cols) {
  const n = parts[parts.length - 1], q = parts.length > 1 ? parts[parts.length - 2] : null;
  for (const s of cols || []) {
    if (q && s.alias !== q && s.t !== q) continue;
    const c = s.cols.find(c => c.n === n || c.n.toLowerCase() === n.toLowerCase());
    if (c) return T(c.kind, {t: c.t, scale: c.scale, col: c, nul: c.nul});
  }
  return null;
}
function fnTy(e, cx, cols) {
  const nm = e.name, a0 = () => e.args[0] ? tyOf(e.args[0], cx, cols) : T('str');
  switch (nm) {
    case 'count': case 'uniq': case 'uniqexact': case 'uniqcombined': case 'uniqhll12': return T('big', {chT: 'UInt64'});
    case 'sum': { const t = a0(); return t.k === 'dec' ? T('dec', {scale: t.scale, chT: `Decimal(38, ${t.scale})`}) : t.k === 'float' ? T('float', {chT: 'Float64'}) : T(cx.dia === 'ch' ? 'big' : 'dec', {scale: 0, chT: /^U/.test(t.t || '') ? 'UInt64' : 'Int64'}); }
    case 'avg': { const t = a0(); return cx.dia === 'ch' ? T('float', {chT: 'Float64'}) : T(t.k === 'float' ? 'float' : 'dec', {scale: 16}); }
    case 'min': case 'max': case 'any': case 'anylast': return a0();
    case 'argmax': case 'argmin': return a0();
    case 'quantile': case 'median': return T('float', {chT: 'Float64'});
    case 'array_agg': case 'grouparray': return T('arr', {chT: 'Array(' + (a0().chT || a0().t || 'String') + ')'});
    case 'string_agg': return T('str');
    case 'lower': case 'upper': case 'concat': case 'concat_ws': case 'substr': case 'substring': case 'replace': case 'replaceall': case 'replaceone': case 'left': case 'right': case 'btrim_both': case 'btrim_leading': case 'btrim_trailing': case 'btrim': case 'ltrim': case 'rtrim': case 'trimboth': case 'split_part': case 'to_char': case 'tostring': case 'repeat': case 'md5': case 'version': case 'current_database': case 'currentdatabase': case 'current_user': case 'current_schema': case 'current_setting': case 'formatdatetime': case 'reverse': case 'lpad': case 'rpad': case 'initcap': case 'lowerutf8': case 'upperutf8': case 'hostname': case 'user': return T('str', {chT: 'String'});
    case 'length': case 'char_length': case 'position': case 'strpos': case 'octet_length': case 'lengthutf8': case 'array_length': case 'cardinality': case 'jsonb_array_length': case 'json_array_length': return T(cx.dia === 'ch' ? 'big' : 'int', {chT: 'UInt64'});
    case 'now': case 'current_timestamp': case 'localtimestamp': case 'date_trunc': case 'to_timestamp': case 'todatetime': case 'tostartofhour': case 'tostartofminute': case 'tostartofday': case 'transaction_timestamp': case 'statement_timestamp': case 'clock_timestamp': case 'pg_postmaster_start_time': return T('ts', {chT: 'DateTime'});
    case 'current_date': case 'today': case 'yesterday': case 'todate': case 'tostartofweek': case 'tostartofmonth': case 'tostartofyear': case 'tostartofquarter': case 'to_date': return T('date', {chT: 'Date'});
    case 'extract': case 'date_part': return T('dec', {scale: 0, chT: 'UInt32'});
    case 'toyyyymm': case 'toyyyymmdd': return T('int', {chT: 'UInt32'});
    case 'tohour': case 'tominute': case 'tosecond': case 'todayofmonth': case 'tomonth': case 'toyear': case 'todayofweek': return T('int', {chT: 'UInt8'});
    case 'datediff': return T('big', {chT: 'Int64'});
    case 'round': case 'abs': case 'floor': case 'ceil': case 'ceiling': case 'trunc': { const t = a0(); return t.k === 'int' || t.k === 'big' ? t : T(t.k === 'dec' ? 'dec' : 'float', {scale: e.args[1] && e.args[1].k === 'num' ? e.args[1].v : (nm === 'round' ? t.scale : 0), chT: t.chT || 'Float64'}); }
    case 'power': case 'pow': case 'sqrt': case 'exp': case 'ln': case 'log': case 'log10': return T('float', {chT: 'Float64'});
    case 'coalesce': case 'ifnull': case 'nullif': case 'greatest': case 'least': { for (const a of e.args) { const t = tyOf(a, cx, cols); if (!t.lit || t.k !== 'str') return t; } return a0(); }
    case 'if': return e.args[1] ? tyOf(e.args[1], cx, cols) : T('str');
    case 'multiif': return e.args[1] ? tyOf(e.args[1], cx, cols) : T('str');
    case 'toint8': case 'toint16': case 'toint32': return T('int', {chT: 'Int32'}); case 'toint64': return T('big', {chT: 'Int64'}); case 'touint8': case 'touint16': case 'touint32': return T('int', {chT: 'UInt32'}); case 'touint64': return T('big', {chT: 'UInt64'});
    case 'tofloat32': case 'tofloat64': return T('float', {chT: 'Float64'});
    case 'has': case 'isnull': case 'isnotnull': case 'empty': case 'notempty': case 'pg_is_in_recovery': return T('bool', {chT: 'UInt8'});
    case 'jsonb_build_object': case 'json_build_object': case 'to_jsonb': case 'to_json': return T('obj');
    case 'jsonb_typeof': case 'json_typeof': return T('str');
    case 'splitbychar': case 'string_to_array': return T('arr', {chT: 'Array(String)'});
    case 'pg_database_size': case 'pg_relation_size': case 'pg_total_relation_size': return T('big');
    case 'pg_size_pretty': return T('str'); case 'txid_current': case 'pg_backend_pid': return T('big');
    case 'age': return T('str', {pgT: 'interval'});
    case 'dictget': case 'dictgetordefault': return T('str', {chT: 'String'});
  }
  return T('str', {chT: 'String'});
}

/* ============================================================================
   四、执行：来源（表 / 子查询 / 函数表 / 系统视图）→ 连接 → WHERE → 分组聚合 → 排序 / 去重 / LIMIT
   ========================================================================== */
const mkEnv = (aliases, rows) => { const e = Object.assign({}, rows); Object.defineProperty(e, '__al', {value: aliases, enumerable: false, writable: true}); return e; };
const rowObj = (names, row) => { const o = {}; names.forEach((n, i) => { o[n] = row[i]; }); return o; };
const splitAnd = e => !e ? [] : e.k === 'bin' && e.op === 'and' ? splitAnd(e.l).concat(splitAnd(e.r)) : e.k === 'paren' ? splitAnd(e.e) : [e];
const litOf = e => e.k === 'num' ? e.v : e.k === 'str' ? e.v : e.k === 'paren' ? litOf(e.e) : undefined;
const nullRow = cols => Object.fromEntries(cols.map(c => [c.n, null]));
const refsAlias = (e, alias, cols) => {
  if (!e || typeof e !== 'object') return false;
  if (e.k === 'col') { if (e.parts.length > 1) return e.parts[e.parts.length - 2] === alias; return cols.some(c => c.n === e.parts[0]); }
  for (const key of ['l', 'r', 'e', 'lo', 'hi', 'pat', 'i', 'operand', 'els']) if (e[key] && refsAlias(e[key], alias, cols)) return true;
  for (const key of ['args', 'items', 'list']) if (e[key] && e[key].some(x => refsAlias(x, alias, cols))) return true;
  if (e.whens) return e.whens.some(([w, t]) => refsAlias(w, alias, cols) || refsAlias(t, alias, cols));
  return false;
};

/* ---- 系统视图 / 目录（information_schema、pg_catalog、system）---- */
const C = (n, t) => ({n, t: t || 'text', kind: kindOf(t || 'text'), scale: scaleOf(t || ''), nul: 1});
function allTables(cx) { return (SCHEMAS[cx.inst.schema] || []).flatMap(d => d.tables.map(t => ({db: d.db, t}))); }
function virtTable(cx, schema, name) {
  const inst = cx.inst, ls = schema.toLowerCase(), ln = name.toLowerCase();
  const dbs = SCHEMAS[inst.schema] || [], cur = dbs.find(d => d.db === cx.db) || {tables: []};
  if (cx.dia === 'pg') {
    if (ls === 'information_schema') {
      if (ln === 'tables') return {cols: [C('table_catalog'), C('table_schema'), C('table_name'), C('table_type')], rows: cur.tables.map(t => { const [s, n] = t.n.split('.'); return {table_catalog: cx.db, table_schema: s, table_name: n, table_type: t.view ? 'VIEW' : 'BASE TABLE'}; })};
      if (ln === 'columns') return {cols: [C('table_catalog'), C('table_schema'), C('table_name'), C('column_name'), C('ordinal_position', 'integer'), C('data_type'), C('is_nullable'), C('column_default')], rows: cur.tables.flatMap(t => { const [s, n] = t.n.split('.'); return t.cols.map((c, i) => ({table_catalog: cx.db, table_schema: s, table_name: n, column_name: c.n, ordinal_position: i + 1, data_type: c.t.replace(/\(.*\)/, ''), is_nullable: c.nul ? 'YES' : 'NO', column_default: c.def && c.def !== 'identity' ? c.def : null})); })};
      if (ln === 'schemata') return {cols: [C('catalog_name'), C('schema_name'), C('schema_owner')], rows: [...new Set(cur.tables.map(t => t.n.split('.')[0]))].map(s => ({catalog_name: cx.db, schema_name: s, schema_owner: 'postgres'}))};
    }
    if (ls === 'pg_catalog') {
      if (ln === 'pg_tables') return {cols: [C('schemaname'), C('tablename'), C('tableowner')], rows: cur.tables.filter(t => !t.view).map(t => { const [s, n] = t.n.split('.'); return {schemaname: s, tablename: n, tableowner: t.own || 'postgres'}; })};
      if (ln === 'pg_views') return {cols: [C('schemaname'), C('viewname'), C('viewowner')], rows: cur.tables.filter(t => t.view && t.kind === 'view').map(t => { const [s, n] = t.n.split('.'); return {schemaname: s, viewname: n, viewowner: t.own || 'postgres'}; })};
      if (ln === 'pg_namespace') return {cols: [C('nspname'), C('nspowner', 'bigint')], rows: ['pg_catalog', 'information_schema', ...new Set(cur.tables.map(t => t.n.split('.')[0]))].map((s, i) => ({nspname: s, nspowner: 10 + i}))};
      if (ln === 'pg_database') return {cols: [C('datname'), C('datdba', 'bigint'), C('encoding', 'integer'), C('datallowconn', 'boolean')], rows: dbs.map(d => ({datname: d.db, datdba: 10, encoding: 6, datallowconn: true}))};
      if (ln === 'pg_roles') return {cols: [C('rolname'), C('rolsuper', 'boolean'), C('rolcanlogin', 'boolean'), C('rolconnlimit', 'integer'), C('rolpassword')], rows: [['postgres', true, true, -1], ['app_order', false, true, 100], ['dbtool_ro', false, true, 20], ['bi_etl', false, true, 50]].map(r => ({rolname: r[0], rolsuper: r[1], rolcanlogin: r[2], rolconnlimit: r[3], rolpassword: '********'}))};
      if (ln === 'pg_stat_user_tables') return {cols: [C('schemaname'), C('relname'), C('n_live_tup', 'bigint'), C('n_dead_tup', 'bigint'), C('seq_scan', 'bigint'), C('idx_scan', 'bigint'), C('last_autovacuum', 'timestamp with time zone')], rows: cur.tables.filter(t => !t.view).map((t, i) => { const [s, n] = t.n.split('.'); return {schemaname: s, relname: n, n_live_tup: t.rows, n_dead_tup: Math.round(t.rows * 0.004) + i, seq_scan: 120 + i * 17, idx_scan: Math.round(t.rows / 40) + 900, last_autovacuum: new Date(NOW_MS - (i + 1) * 3600e3 * 5)}; })};
      if (ln === 'pg_locks') return {cols: [C('locktype'), C('mode'), C('granted', 'boolean'), C('pid', 'integer')], rows: [['relation', 'AccessShareLock', true, 18233], ['virtualxid', 'ExclusiveLock', true, 18233], ['relation', 'RowExclusiveLock', true, 20411]].map(r => ({locktype: r[0], mode: r[1], granted: r[2], pid: r[3]}))};
      if (ln === 'pg_settings') return {cols: [C('name'), C('setting'), C('unit'), C('category')], rows: [['max_connections', '500', null], ['shared_buffers', '2097152', '8kB'], ['work_mem', '16384', 'kB'], ['statement_timeout', '30000', 'ms'], ['default_transaction_read_only', 'on', null], ['TimeZone', 'Asia/Shanghai', null]].map(r => ({name: r[0], setting: r[1], unit: r[2], category: '—'}))};
    }
  } else {
    if (ls === 'system') {
      if (ln === 'one') return {cols: [C('dummy', 'UInt8')], rows: [{dummy: 0}]};
      if (ln === 'databases') return {cols: [C('name', 'String'), C('engine', 'String'), C('comment', 'String')], rows: [...dbs.map(d => ({name: d.db, engine: 'Atomic', comment: ''})), {name: 'default', engine: 'Atomic', comment: ''}, {name: 'system', engine: 'Atomic', comment: ''}, {name: 'INFORMATION_SCHEMA', engine: 'Memory', comment: ''}, {name: 'information_schema', engine: 'Memory', comment: ''}]};
      if (ln === 'tables') return {cols: [C('database', 'String'), C('name', 'String'), C('engine', 'String'), C('total_rows', 'Nullable(UInt64)'), C('total_bytes', 'Nullable(UInt64)'), C('comment', 'String')], rows: allTables(cx).map(x => ({database: x.db, name: x.t.n, engine: x.t.eng || (x.t.view ? 'View' : 'MergeTree'), total_rows: x.t.view && x.t.kind !== 'dict' ? null : x.t.rows, total_bytes: x.t.size ? x.t.size * 1048576 : null, comment: x.t.c || ''}))};
      if (ln === 'columns') return {cols: [C('database', 'String'), C('table', 'String'), C('name', 'String'), C('type', 'String'), C('position', 'UInt64'), C('is_in_primary_key', 'UInt8'), C('comment', 'String')], rows: allTables(cx).flatMap(x => x.t.cols.map((c, i) => ({database: x.db, table: x.t.n, name: c.n, type: c.t, position: i + 1, is_in_primary_key: c.key === 'PRI' ? 1 : 0, comment: c.cmt || ''})))};
      if (ln === 'parts') return {cols: [C('database', 'String'), C('table', 'String'), C('partition', 'String'), C('name', 'String'), C('rows', 'UInt64'), C('bytes_on_disk', 'UInt64'), C('modification_time', 'DateTime'), C('active', 'UInt8')], rows: allTables(cx).filter(x => !x.t.view && x.t.rows > 100000).flatMap(x => Array.from({length: 4}, (_, i) => ({database: x.db, table: x.t.n, partition: ymdOf(NOW_MS - i * 31 * DAY).slice(0, 7).replace('-', ''), name: `${ymdOf(NOW_MS - i * 31 * DAY).slice(0, 7).replace('-', '')}_${1 + i}_${1 + i}_0`, rows: Math.round(x.t.rows / 24), bytes_on_disk: Math.round(x.t.size * 1048576 / 24), modification_time: new Date(NOW_MS - i * 31 * DAY), active: 1})))};
      if (ln === 'table_engines') return {cols: [C('name', 'String'), C('supports_ttl', 'UInt8'), C('supports_sort_order', 'UInt8')], rows: ['MergeTree', 'ReplacingMergeTree', 'SummingMergeTree', 'AggregatingMergeTree', 'Distributed', 'Dictionary', 'View', 'MaterializedView', 'Memory', 'Log'].map(n => ({name: n, supports_ttl: /MergeTree/.test(n) ? 1 : 0, supports_sort_order: /MergeTree/.test(n) ? 1 : 0}))};
      if (ln === 'time_zones') return {cols: [C('time_zone', 'String')], rows: ['Asia/Shanghai', 'Asia/Tokyo', 'Europe/London', 'UTC', 'America/New_York'].map(n => ({time_zone: n}))};
      if (ln === 'data_type_families') return {cols: [C('name', 'String'), C('case_insensitive', 'UInt8')], rows: ['UInt8', 'UInt16', 'UInt32', 'UInt64', 'Int32', 'Int64', 'Float64', 'String', 'Date', 'DateTime', 'Decimal', 'Array', 'Map', 'UUID', 'Enum8'].map(n => ({name: n, case_insensitive: 0}))};
      if (ln === 'data_skipping_indices' || ln === 'parts_columns') return {cols: [C('database', 'String'), C('table', 'String'), C('name', 'String')], rows: []};
    }
    if (ls === 'information_schema') {
      if (ln === 'tables') return {cols: [C('table_catalog', 'String'), C('table_schema', 'String'), C('table_name', 'String'), C('table_type', 'String')], rows: allTables(cx).map(x => ({table_catalog: x.db, table_schema: x.db, table_name: x.t.n, table_type: x.t.view ? 'VIEW' : 'BASE TABLE'}))};
      if (ln === 'columns') return {cols: [C('table_schema', 'String'), C('table_name', 'String'), C('column_name', 'String'), C('ordinal_position', 'UInt64'), C('data_type', 'String')], rows: allTables(cx).flatMap(x => x.t.cols.map((c, i) => ({table_schema: x.db, table_name: x.t.n, column_name: c.n, ordinal_position: i + 1, data_type: c.t})))};
      if (ln === 'schemata') return {cols: [C('catalog_name', 'String'), C('schema_name', 'String')], rows: dbs.map(d => ({catalog_name: d.db, schema_name: d.db}))};
    }
  }
  return null;
}

/* ---- 解析表名 ---- */
function notExistTable(cx, parts) {
  if (cx.dia === 'ch') {
    const db = parts.length > 1 ? parts[0] : cx.db, nm = parts[parts.length - 1];
    if (!(SCHEMAS[cx.inst.schema] || []).some(d => d.db === db) && !/^(system|information_schema|default)$/i.test(db)) return new EErr(`ClickHouse 错误 81（库不存在）：Database ${db} does not exist. (UNKNOWN_DATABASE)`, 'engine_error');
    return new EErr(`ClickHouse 错误 60（表不存在）：Table ${db}.${nm} does not exist. (UNKNOWN_TABLE)`, 'engine_error');
  }
  return new EErr(`PostgreSQL 报错：relation "${parts.slice(-2).join('.')}" does not exist（位置 ${'?'}） [42P01]`.replace('（位置 ?）', ''), 'bad_query');
}
function resolveTable(parts, cx) {
  const inst = cx.inst;
  if (cx.dia === 'pg') {
    let schema = null, name;
    if (parts.length === 1) name = parts[0]; else if (parts.length === 2) { [schema, name] = parts; } else { schema = parts[parts.length - 2]; name = parts[parts.length - 1]; }
    const tryPub = (s) => tdOf(inst, cx.db, s + '.' + name);
    if (schema) {
      if (/^(information_schema|pg_catalog)$/i.test(schema)) { const v = virtTable(cx, schema, name); if (v) return {virt: v, alias: name}; }
      const td = tryPub(schema); if (td) return {td, alias: name};
      throw notExistTable(cx, parts);
    }
    const td = tryPub('public'); if (td) return {td, alias: name};
    const v = virtTable(cx, 'pg_catalog', name); if (v) return {virt: v, alias: name};
    throw notExistTable(cx, parts);
  }
  const db = parts.length > 1 ? parts[0] : cx.db, name = parts[parts.length - 1];
  if (/^(system|information_schema)$/i.test(db)) { const v = virtTable(cx, db, name); if (v) return {virt: v, alias: name}; throw notExistTable(cx, [db, name]); }
  const td = tdOf(inst, db, name); if (td) return {td, alias: name};
  throw notExistTable(cx, parts.length > 1 ? parts : [db, name]);
}

/* ---- 按键取行（模拟索引查找）：用于 WHERE key = 常量 与 JOIN ... ON r.key = l.x ---- */
function lookupRows(td, col, v, tail) {
  const total = td.total, ln = col.n;
  if (isNull(v)) return [];
  if (td.meta.idc === col.n && (col.kind === 'int' || col.kind === 'big')) { const i = Math.trunc(toNum(v)) - 1; return i >= 0 && i < total && Number.isInteger(toNum(v)) ? [rowAt(td, i)] : []; }
  const pt = td.fkTotals[ln];
  if (pt) {
    const x = Math.trunc(toNum(v)); if (!(x >= 1 && x <= pt)) return [];
    const lo = Math.ceil((x - 1) * total / pt), hi = Math.ceil(x * total / pt), out = [];
    for (let i = lo; i < hi && i < total; i++) { const r = rowAt(td, i); r[ln] = col.kind === 'big' || col.kind === 'int' ? x : v; out.push(r); }
    return out;
  }
  if (col.kind === 'uuid') { const i = h32(String(v)) % total; const r = rowAt(td, i); r[ln] = String(v); return [r]; }
  if (isUniqueCol(td, col)) { const i = total - 1 - (h32(td.key + String(v)) % Math.min(total, POOL)); const r = rowAt(td, i); r[ln] = typeof r[ln] === 'number' ? toNum(v) : v; return [r]; }
  const k = 1 + h32(td.key + ln + String(v)) % 6, out = [];
  for (let j = 0; j < k; j++) { const i = total - 1 - ((h32(String(v) + 'x' + j) + j * 104729) % Math.min(total, POOL)); const r = rowAt(td, i); r[ln] = typeof r[ln] === 'number' ? toNum(v) : v; out.push(r); }
  return out.sort((a, b) => (a.created_at && b.created_at ? a.created_at - b.created_at : 0));
}
const KEYISH = c => c.key === 'PRI' || c.key === 'UNI' || c.key === 'MUL';
const isUniqueCol = (td, col) => td.meta.idc === col.n || col.key === 'UNI' || (col.key === 'PRI' && td.type === 'PgSQL' && (td.t.idx || []).some(x => x[2] && x[1].length === 1 && x[1][0] === col.n));
function synthByWhere(td, conj, alias, cx) {
  for (const e of conj) {
    let colE = null, lits = null;
    if (e.k === 'bin' && e.op === '=') { const a = litOf(e.r), b = litOf(e.l); if (e.l.k === 'col' && a !== undefined) { colE = e.l; lits = [a]; } else if (e.r.k === 'col' && b !== undefined) { colE = e.r; lits = [b]; } }
    else if (e.k === 'in' && !e.neg && e.list && e.e.k === 'col' && e.list.every(x => litOf(x) !== undefined)) { colE = e.e; lits = e.list.map(litOf); }
    if (!colE) continue;
    if (colE.parts.length > 1 && colE.parts[colE.parts.length - 2] !== alias && colE.parts[colE.parts.length - 2] !== td.n.split('.').pop()) continue;
    const c = td.colMap[colE.parts[colE.parts.length - 1].toLowerCase()];
    if (!c || !KEYISH(c) || (c.kind === 'ts' || c.kind === 'date')) continue;
    if (td.meta.idc !== c.n && c.key === 'MUL' && /^(status|level|channel|type)$/i.test(c.n)) continue;
    const rows = []; for (const v of lits.slice(0, 50)) rows.push(...lookupRows(td, c, v));
    return rows;
  }
  return null;
}

/* ---- 来源 ---- */
function loadSource(item, cx, hint) {
  if (item.kind === 'table') {
    const name = item.parts[item.parts.length - 1];
    if (item.parts.length === 1 && cx.ctes.has(name)) {
      const rs = cx.ctes.get(name), names = rs.cols.map(c => c.n);
      return {alias: item.alias || name, t: name, cols: rs.cols.map(c => ({n: c.n, t: c.d.t || '', kind: c.d.k, scale: c.d.scale || 0, nul: 1, d: c.d})), rows: rs.rows.map(r => rowObj(names, r)), cte: true};
    }
    const rt = resolveTable(item.parts, cx);
    if (rt.virt) return {alias: item.alias || rt.alias, t: rt.alias, cols: rt.virt.cols, rows: rt.virt.rows};
    const td = rt.td, alias = item.alias || rt.alias;
    const src = {alias, t: rt.alias, cols: td.cols, td};
    if (hint && hint.scan) {
      const syn = (td.view && td.kind === 'view') ? null : synthByWhere(td, hint.conj, alias, cx);
      if (syn) { src.rows = syn; src.synth = true; cx.synth = true; }
      else { src.rows = poolRows(td, hint.want); if (td.total > POOL && !(td.view && td.kind === 'view')) cx.scale = Math.max(1, (hint.hasWhere ? td.total * 30 / 730 : td.total) / src.rows.length); }
    }
    return src;
  }
  if (item.kind === 'sub') {
    const rs = execQuery(item.q, Object.assign(Object.create(cx), {ctes: new Map(cx.ctes)})), names = rs.cols.map(c => c.n);
    return {alias: item.alias || 'subq', t: item.alias || 'subq', cols: rs.cols.map(c => ({n: c.n, t: c.d.t || '', kind: c.d.k, scale: c.d.scale || 0, nul: 1, d: c.d})), rows: rs.rows.map(r => rowObj(names, r))};
  }
  /* 函数表 */
  const nm = item.name, args = item.args.map(a => evalExpr(a, mkEnv([], {}), cx));
  if (nm === 'generate_series' && cx.dia === 'pg') {
    const a = toNum(args[0]), b = toNum(args[1]), st = args.length > 2 ? toNum(args[2]) : 1;
    if (isNaN(a) || isNaN(b) || !st) throw bad('PostgreSQL 报错：generate_series 的参数必须是数字 [22023]');
    const n = Math.floor((b - a) / st) + 1;
    if (n > 5e8) throw new EErr('超时', 'timeout');
    if (n > 200000) throw new NoSim(`generate_series 生成 ${nf(n)} 行（原型最多模拟 200,000 行）`);
    const cn = (item.colAliases && item.colAliases[0]) || item.alias || 'generate_series';
    return {alias: item.alias || 'generate_series', t: 'generate_series', cols: [C(cn, 'integer')], rows: Array.from({length: Math.max(0, n)}, (_, i) => ({[cn]: a + i * st}))};
  }
  if (nm === 'numbers' && cx.dia === 'ch') {
    let off = 0, n = args[0], st = 1; if (args.length >= 2) { off = args[0]; n = args[1]; } if (args.length >= 3) st = args[2];
    if (n > 200000) throw new NoSim(`numbers(${n}) 生成 ${nf(n)} 行（原型最多模拟 200,000 行）`);
    return {alias: item.alias || 'numbers', t: 'numbers', cols: [C('number', 'UInt64')], rows: Array.from({length: n}, (_, i) => ({number: off + i * st}))};
  }
  if (nm === 'unnest' && cx.dia === 'pg') {
    const arr = Array.isArray(args[0]) ? args[0] : []; const cn = (item.colAliases && item.colAliases[0]) || item.alias || 'unnest';
    return {alias: item.alias || 'unnest', t: 'unnest', cols: [C(cn)], rows: arr.map(v => ({[cn]: v}))};
  }
  if (cx.dia === 'pg') throw new EErr(`PostgreSQL 报错：function ${item.full || nm}(${args.map(a => typeName1(a)).join(', ')}) does not exist（提示：No function matches the given name and argument types. You might need to add explicit type casts.） [42883]`, 'bad_query');
  throw new EErr(`ClickHouse 错误 46（未知的函数）：Unknown table function ${nm}. (UNKNOWN_FUNCTION)`, 'engine_error');
}

/* ---- 连接 ---- */
function findKeyEq(step, right, leftCols, cx) {
  const conds = splitAnd(step.on);
  if (step.using) return {col: step.using[0], left: {k: 'col', parts: [step.using[0]]}, rest: conds};
  for (let ci = 0; ci < conds.length; ci++) {
    const e = conds[ci];
    if (e.k !== 'bin' || e.op !== '=') continue;
    for (const [rs, ls] of [[e.l, e.r], [e.r, e.l]]) {
      if (rs.k !== 'col') continue;
      const q = rs.parts.length > 1 ? rs.parts[rs.parts.length - 2] : null, n = rs.parts[rs.parts.length - 1];
      const hasCol = right.cols.find(c => c.n === n || c.n.toLowerCase() === n.toLowerCase());
      if (!hasCol) continue;
      if (q ? q !== right.alias : leftCols.some(c => c.cols.some(x => x.n === n))) continue;
      if (refsAlias(ls, right.alias, right.cols) && (ls.k !== 'col' || (ls.parts.length > 1 && ls.parts[ls.parts.length - 2] === right.alias))) continue;
      return {col: hasCol.n, left: ls, rest: conds.filter((x, i) => i !== ci)};
    }
  }
  return null;
}
function joinStep(envs, step, srcsSoFar, cx, leftTotal) {
  const item = step.src || step.item, right = loadSource(item, cx, null), alias = right.alias;
  const kind = step.type || 'cross';
  if (kind === 'right' || kind === 'full') throw new NoSim(kind.toUpperCase() + ' JOIN');
  const key = right.td && step.type !== 'cross' ? findKeyEq(step, right, srcsSoFar, cx) : (right.td && step.using ? findKeyEq(step, right, srcsSoFar, cx) : null);
  const out = [];
  const newAl = al => al.concat([alias]);
  if (key && right.td && !(right.td.view && right.td.kind === 'view')) {
    const col = right.td.colMap[key.col.toLowerCase()];
    for (const env of envs) {
      const lv = evalExpr(key.left, env, cx);
      const rs = KEYISH(col) || right.td.meta.idc === col.n ? lookupRows(right.td, col, lv) : poolRows(right.td).filter(r => eqVals(r[col.n], lv) === true);
      let hit = false;
      for (const r of rs) {
        const e2 = mkEnv(newAl(env.__al), Object.assign({}, env, {[alias]: r}));
        if (key.rest.every(c => truthy(evalExpr(c, e2, cx)))) { out.push(e2); hit = true; }
      }
      if (!hit && kind === 'left') out.push(mkEnv(newAl(env.__al), Object.assign({}, env, {[alias]: nullRow(right.cols)})));
    }
    cx.synth = true;
    srcsSoFar.push(right);
    return out;
  }
  /* 没有可用的等值键：笛卡尔积再过滤；两边都是大表则超时 */
  const rrows = right.rows || (right.td ? poolRows(right.td) : []);
  const rightTotal = right.td ? right.td.total : rrows.length;
  if (leftTotal * rightTotal > 5e9 && !step.on === false || (leftTotal * rightTotal > 5e9 && kind === 'cross')) {
    if (cx.dia === 'ch') throw new EErr(`ClickHouse 错误 158（超过读取行数 / 字节上限，请加过滤条件或缩小范围）：Limit for rows (controlled by 'max_rows_to_read' setting) exceeded, max rows: 100.00 million, current rows: ${nf(rightTotal)}. (TOO_MANY_ROWS)`, 'engine_error');
    throw new EErr('timeout', 'timeout');
  }
  if (envs.length * rrows.length > 2e6) throw new NoSim(`连接两侧的样本行数太多（${nf(envs.length)} × ${nf(rrows.length)}）；请给 JOIN 加上索引列的等值条件`);
  for (const env of envs) {
    let hit = false;
    for (const r of rrows) {
      const e2 = mkEnv(newAl(env.__al), Object.assign({}, env, {[alias]: r}));
      if (!step.on || truthy(evalExpr(step.on, e2, cx))) { out.push(e2); hit = true; }
      if (out.length > 300000) throw new NoSim('连接结果行数过多');
    }
    if (!hit && kind === 'left') out.push(mkEnv(newAl(env.__al), Object.assign({}, env, {[alias]: nullRow(right.cols)})));
  }
  srcsSoFar.push(right);
  return out;
}

/* ---- SELECT ---- */
function itemName(it, cx) {
  if (it.alias) return it.alias;
  const e = it.e;
  if (cx.dia === 'ch') return e.k === 'col' ? e.parts[e.parts.length - 1] : it.text;
  const bare = x => x.k === 'paren' ? bare(x.e) : x;
  const b = bare(e);
  if (b.k === 'col') return b.parts[b.parts.length - 1];
  if (b.k === 'fn') return b.name === 'btrim_both' || /^btrim_/.test(b.name) ? 'btrim' : b.name;
  if (b.k === 'cast') return b.e.k === 'col' ? b.e.parts[b.e.parts.length - 1] : b.type;
  if (b.k === 'case') return 'case'; if (b.k === 'arr') return 'array'; if (b.k === 'exists') return 'exists';
  return '?column?';
}
function expandItems(q, srcs, cx) {
  const out = [];
  for (const it of q.items) {
    if (it.star || (it.e && it.e.k === 'star')) {
      const qual = it.e && it.e.q;
      if (!srcs.length) throw cx.dia === 'ch' ? new EErr('ClickHouse 错误 62（SQL 语法错误）：Syntax error: SELECT * with no tables specified is not valid (SYNTAX_ERROR)', 'engine_error') : bad('PostgreSQL 报错：SELECT * with no tables specified is not valid [42601]');
      for (const s of srcs) { if (qual && s.alias !== qual && s.t !== qual) continue; for (const c of s.cols) out.push({e: {k: 'col', parts: srcs.length > 1 ? [s.alias, c.n] : [c.n]}, alias: null, text: c.n, fromStar: true}); }
    } else out.push(it);
  }
  return out;
}
function execSelect(q, cx) {
  cx.scale = 1; cx.synth = false; cx.outAlias = null;
  const conj = splitAnd(q.where), srcs = [];
  let envs;
  let leftTotal = 1;
  if (!q.from.length) envs = [mkEnv([], {})];
  else {
    const first = loadSource(q.from[0], cx, {scan: true, conj, want: (cx.maxRows || POOL) + 1, hasWhere: !!q.where});
    srcs.push(first); leftTotal = first.td ? first.td.total : (first.rows || []).length;
    envs = first.rows.map(r => mkEnv([first.alias], {[first.alias]: r}));
    const steps = [];
    for (let k = 0; k < q.from.length; k++) { if (k > 0) steps.push({type: 'cross', item: q.from[k]}); for (const j of q.joins) if (j.after === k) steps.push(j); }
    for (const st of steps) envs = joinStep(envs, st, srcs, cx, leftTotal);
    if (cx.dia === 'ch' && first.td) chReadLimit(first.td, q, cx);
  }
  cx.curSrcs = srcs;
  const items = expandItems(q, srcs, cx);
  validateCols(q, items, srcs, cx);
  if (q.where) envs = envs.filter(env => truthy(evalExpr(q.where, env, cx)));
  const names = items.map(it => itemName(it, cx));
  const aliasExpr = {}; items.forEach((it, i) => { if (it.alias) aliasExpr[it.alias] = it.e; });
  const resolveRef = (e) => {
    if (e.k === 'num' && e.int && e.v >= 1 && e.v <= items.length) return items[e.v - 1].e;
    if (e.k === 'col' && e.parts.length === 1 && aliasExpr[e.parts[0]] && !srcs.some(s => s.cols.some(c => c.n === e.parts[0]))) return aliasExpr[e.parts[0]];
    return e;
  };
  const aggMode = !!q.group || items.some(it => hasAgg(it.e)) || (q.having && hasAgg(q.having)) || (q.order && q.order.some(o => hasAgg(o.e)));
  let outRows = [];   // {vals, env, grp}
  if (aggMode) {
    const gexprs = (q.group || []).map(resolveRef), groups = new Map();
    for (const env of envs) {
      const key = gexprs.length ? JSON.stringify(gexprs.map(g => { const v = evalExpr(g, env, cx); return isDate(v) ? v.getTime() : isBig(v) ? String(v) : v; })) : '';
      let g = groups.get(key); if (!g) groups.set(key, g = {rows: [], synth: cx.synth});
      g.rows.push(env);
    }
    if (!q.group && !groups.size) groups.set('', {rows: [], synth: cx.synth});
    for (const g of groups.values()) {
      const env = g.rows[0] || mkEnv([], {});
      cx.outAlias = null;
      const vals = items.map(it => evalExpr(it.e, env, cx, g));
      cx.outAlias = Object.fromEntries(items.filter(it => it.alias).map((it, i) => [it.alias, en => evalExpr(it.e, en, cx, g)]));
      if (q.having && !truthy(evalExpr(q.having, env, cx, g))) continue;
      outRows.push({vals, env, grp: g});
    }
    cx.outAlias = null;
  } else {
    cx.outAlias = null;
    for (const env of envs) outRows.push({vals: items.map(it => evalExpr(it.e, env, cx)), env, grp: null});
  }
  /* DISTINCT */
  if (q.distinct) { const seen = new Set(); outRows = outRows.filter(r => { const k = JSON.stringify(r.vals, (kk, v) => isBig(v) ? String(v) : v); if (seen.has(k)) return false; seen.add(k); return true; }); }
  /* ORDER BY */
  if (q.order) {
    const ords = q.order.map(o => { const r = resolveRef(o.e); const idx = items.findIndex(it => it.e === r); return {e: r, dir: o.dir, nulls: o.nulls, idx}; });
    const keyOf = (row, o) => o.idx >= 0 ? row.vals[o.idx] : evalExpr(o.e, row.env, Object.assign(Object.create(cx), {outAlias: Object.fromEntries(items.map((it, i) => it.alias ? [it.alias, () => row.vals[i]] : null).filter(Boolean))}), row.grp);
    const keyed = outRows.map(r => ({r, k: ords.map(o => keyOf(r, o))}));
    keyed.sort((a, b) => { for (let i = 0; i < ords.length; i++) { const x = a.k[i], y = b.k[i], nf = ords[i].nulls === 'first' ? true : ords[i].nulls === 'last' ? false : (cx.dia === 'pg' && ords[i].dir === -1); if (isNull(x) && isNull(y)) continue; if (isNull(x)) return nf ? -1 : 1; if (isNull(y)) return nf ? 1 : -1; const c = cmpVals(x, y); if (c) return c * ords[i].dir; } return 0; });
    outRows = keyed.map(x => x.r);
  }
  /* OFFSET / LIMIT */
  const lim = q.limit ? toNum(evalExpr(q.limit, mkEnv([], {}), cx)) : null, off = q.offset ? toNum(evalExpr(q.offset, mkEnv([], {}), cx)) : 0;
  if (off) outRows = outRows.slice(off);
  if (lim !== null) outRows = outRows.slice(0, lim);
  const colDesc = items.map(it => tyOf(it.e, cx, srcs));
  return {cols: items.map((it, i) => ({n: names[i], d: colDesc[i]})), rows: outRows.map(r => r.vals), scale: cx.scale, synth: cx.synth};
}
function validateCols(q, items, srcs, cx) {
  if (!srcs.length) return;
  const known = n => srcs.some(s => s.cols.some(c => c.n === n || (cx.dia === 'pg' && c.n.toLowerCase() === n.toLowerCase())));
  const aliases = new Set(items.filter(i => i.alias).map(i => i.alias));
  const walk = (e, allowAlias) => {
    if (!e || typeof e !== 'object') return;
    if (e.k === 'col') {
      const n = e.parts[e.parts.length - 1], qn = e.parts.length > 1 ? e.parts[e.parts.length - 2] : null;
      if (qn) { if (cx.outer) return; const s = srcs.find(s => s.alias === qn || s.t === qn || (cx.dia === 'pg' && s.alias.toLowerCase() === qn.toLowerCase())); if (!s) throw cx.dia === 'ch' ? new EErr(`ClickHouse 错误 47（未知的列或标识符）：Unknown expression or function identifier \`${qn}.${n}\` in scope SELECT (UNKNOWN_IDENTIFIER)`, 'engine_error') : bad(`PostgreSQL 报错：missing FROM-clause entry for table "${qn}" [42P01]`); if (!s.cols.some(c => c.n === n || (cx.dia === 'pg' && c.n.toLowerCase() === n.toLowerCase()))) throw colErr(cx, n, qn); return; }
      if (!known(n) && !(allowAlias && aliases.has(n)) && !(cx.outer)) throw colErr(cx, n);
      return;
    }
    if (e.k === 'sub' || e.k === 'exists' || (e.k === 'in' && e.sub)) { if (e.k === 'in') walk(e.e, allowAlias); return; }
    for (const key of ['l', 'r', 'e', 'lo', 'hi', 'pat', 'i', 'operand', 'els', 'filter']) if (e[key]) walk(e[key], allowAlias);
    for (const key of ['args', 'items', 'list']) if (e[key]) e[key].forEach(x => walk(x, allowAlias));
    if (e.whens) e.whens.forEach(([w, t]) => { walk(w, allowAlias); walk(t, allowAlias); });
  };
  items.forEach(it => walk(it.e, false));
  walk(q.where, false); (q.group || []).forEach(g => walk(g, true)); walk(q.having, true); (q.order || []).forEach(o => walk(o.e, true));
}
/* ClickHouse 读取量限制（max_rows_to_read 默认 1e8）：整表扫描类查询读到的行数超过限制时由服务端报错 */
function chReadLimit(td, q, cx) {
  if (td.view || td.total <= 1e8 || cx.synth) return;
  const onlyCount = q.items.length === 1 && q.items[0].e && q.items[0].e.k === 'fn' && q.items[0].e.name === 'count' && !q.where && !q.group;
  if (onlyCount) return;
  const streaming = !q.group && !q.order && !q.items.some(it => it.e && hasAgg(it.e)) && !q.distinct;
  if (streaming) return;
  const pk = String(td.t.pk || '').split(',')[0].trim();
  const conj = splitAnd(q.where);
  const eqPk = pk && conj.some(e => e.k === 'bin' && e.op === '=' && refsCol(e, pk));
  const hasPk = eqPk || (pk && conj.some(e => e.k === 'bin' && ['>=', '>', '<', '<='].includes(e.op) && refsCol(e, pk))) || (pk && conj.some(e => e.k === 'between' && refsCol(e, pk)));
  const est = eqPk ? td.total / 730 : hasPk ? td.total * 0.012 : td.total;
  if (est > 1e8) throw new EErr(`ClickHouse 错误 158（超过读取行数 / 字节上限，请加过滤条件或缩小范围）：Limit for rows (controlled by 'max_rows_to_read' setting) exceeded, max rows: 100.00 million, current rows: ${nf(Math.round(est))}. (TOO_MANY_ROWS)`, 'engine_error');
}
const refsCol = (e, name) => { if (!e || typeof e !== 'object') return false; if (e.k === 'col') return e.parts[e.parts.length - 1] === name; for (const key of ['l', 'r', 'e', 'lo', 'hi']) if (e[key] && refsCol(e[key], name)) return true; if (e.args) return e.args.some(a => refsCol(a, name)); return false; };

function execValues(q, cx) {
  const rows = q.values.map(r => r.map(e => evalExpr(e, mkEnv([], {}), cx)));
  const cols = (q.values[0] || []).map((e, i) => ({n: cx.dia === 'ch' ? `c${i + 1}` : `column${i + 1}`, d: tyOf(e, cx, [])}));
  return {cols, rows};
}
function execQuery(q, cx) {
  if (q.ctes && q.ctes.length) {
    cx = Object.assign(Object.create(cx), {ctes: new Map(cx.ctes)});
    for (const c of q.ctes) { const rs = execQuery(c.q, cx); if (c.cols) rs.cols = rs.cols.map((x, i) => ({n: c.cols[i] || x.n, d: x.d})); cx.ctes.set(c.name, rs); }
  }
  let rs;
  if (q.values) { rs = execValues(q, cx); }
  else if (q.setops) {
    rs = execQuery(q.left, cx);
    for (const so of q.setops) {
      const b = execQuery(so.q, cx);
      if (b.cols.length !== rs.cols.length) throw cx.dia === 'ch' ? new EErr('ClickHouse 错误 258（UNION 列数不一致）：Different number of columns in UNION ALL elements (UNION_ALL_RESULT_STRUCTURES_MISMATCH)', 'engine_error') : bad('PostgreSQL 报错：each UNION query must have the same number of columns [42601]');
      let rows = rs.rows.concat(b.rows);
      if (so.op === 'intersect') { const set = new Set(b.rows.map(r => JSON.stringify(r))); rows = rs.rows.filter(r => set.has(JSON.stringify(r))); }
      else if (so.op === 'except') { const set = new Set(b.rows.map(r => JSON.stringify(r))); rows = rs.rows.filter(r => !set.has(JSON.stringify(r))); }
      if (!so.all) { const seen = new Set(); rows = rows.filter(r => { const k = JSON.stringify(r, (kk, v) => isBig(v) ? String(v) : v); if (seen.has(k)) return false; seen.add(k); return true; }); }
      rs = {cols: rs.cols, rows};
    }
    if (q.order) { const idx = q.order.map(o => { const e = o.e; const i = e.k === 'num' ? e.v - 1 : rs.cols.findIndex(c => e.k === 'col' && c.n === e.parts[e.parts.length - 1]); return {i, dir: o.dir}; }); rs.rows.sort((a, b) => { for (const o of idx) { const c = cmpVals(a[o.i], b[o.i]); if (c) return c * o.dir; } return 0; }); }
    if (q.offset) rs.rows = rs.rows.slice(toNum(evalExpr(q.offset, mkEnv([], {}), cx)));
    if (q.limit) rs.rows = rs.rows.slice(0, toNum(evalExpr(q.limit, mkEnv([], {}), cx)));
  } else {
    rs = execSelect(q, cx);
    if (q.values === undefined && q.order === null && false) rs = rs;
  }
  return rs;
}

/* ============================================================================
   五、PostgreSQL：语句检查（internal/engine/postgres/{lex,check}.go 的移植，拒绝原因原文）+ 受限执行 + 结果整形
   ========================================================================== */
const PG = {};
const pgErr = (fmt) => new EErr(fmt, 'bad_query');
const pgLexErr = (src, pos, msg) => { let line = 1, col = 1; for (let i = 0; i < pos && i < src.length; i++) { if (src[i] === '\n') { line++; col = 1; } else col++; } return pgErr(`SQL 解析失败（第 ${line} 行第 ${col} 列）：${msg}`); };
const isSpc = c => c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === '\f' || c === '\v';
const isDg = c => c >= '0' && c <= '9';
const isIS = c => (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c === '_' || c > '\x7f';
const isIC = c => isIS(c) || isDg(c) || c === '$';
const isOC = c => '+-*/<>=~!@#%^&|`?'.includes(c);
const isHx = c => isDg(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F');
const asciiLower = s => s.replace(/[A-Z]/g, c => c.toLowerCase());

function pgScanQuoted(src, i, quote, escape) {
  const n = src.length; let j = i + 1;
  while (j < n) {
    if (escape && src[j] === '\\') j += 2;
    else if (src[j] === quote) { if (j + 1 < n && src[j + 1] === quote) { j += 2; continue; } return j + 1; }
    else { if (src[j] === '\0') throw pgLexErr(src, j, '字符串含有 NUL 字符'); j++; }
  }
  throw pgLexErr(src, i, `${quote === '"' ? '带引号的标识符' : '字符串'}没有结束（缺少 ${quote}）`);
}
function pgScanNumber(src, i) {
  const n = src.length; let j = i;
  if (src[j] === '0' && j + 2 < n && 'xXoObB'.includes(src[j + 1]) && (isHx(src[j + 2]) || src[j + 2] === '_')) { j += 2; while (j < n && (isHx(src[j]) || src[j] === '_')) j++; }
  else {
    while (j < n && (isDg(src[j]) || (src[j] === '_' && j + 1 < n && isDg(src[j + 1])))) j++;
    if (j < n && src[j] === '.' && !(j + 1 < n && src[j + 1] === '.')) { j++; while (j < n && (isDg(src[j]) || (src[j] === '_' && j + 1 < n && isDg(src[j + 1])))) j++; }
    if (j < n && (src[j] === 'e' || src[j] === 'E')) { let k = j + 1; if (k < n && (src[k] === '+' || src[k] === '-')) k++; if (k < n && isDg(src[k])) { while (k < n && isDg(src[k])) k++; j = k; } }
  }
  if (j < n && isIS(src[j])) throw pgLexErr(src, i, '数字后面紧跟了字母或下划线');
  return j;
}
function pgLex(src) {
  const toks = [], comments = [], n = src.length; let i = 0;
  const tk = (kind, text, low, start, end) => toks.push({kind, text, low, start, end});
  while (i < n) {
    const c = src[i];
    if (c === '\0') throw pgLexErr(src, i, 'SQL 中含有 NUL 字符');
    if (isSpc(c)) { i++; continue; }
    if (c === '-' && src[i + 1] === '-') { let j = i + 2; while (j < n && src[j] !== '\n' && src[j] !== '\r') j++; comments.push([i, j]); i = j; continue; }
    if (c === '/' && src[i + 1] === '*') {
      let depth = 1, j = i + 2;
      while (j < n && depth > 0) { if (src[j] === '/' && src[j + 1] === '*') { depth++; j += 2; } else if (src[j] === '*' && src[j + 1] === '/') { depth--; j += 2; } else j++; }
      if (depth > 0) throw pgLexErr(src, i, '块注释没有结束（缺少 */）');
      comments.push([i, j]); i = j; continue;
    }
    if (c === "'") { const j = pgScanQuoted(src, i, "'", false); tk('string', src.slice(i, j), src.slice(i, j), i, j); i = j; continue; }
    if (c === '"') {
      const j = pgScanQuoted(src, i, '"', false), content = src.slice(i + 1, j - 1).replace(/""/g, '"');
      if (content === '') throw pgLexErr(src, i, '不允许零长度的带引号标识符 ""');
      tk('quoted', content, asciiLower(content), i, j); i = j; continue;
    }
    if (isIS(c)) {
      let j = i + 1; while (j < n && isIC(src[j])) j++;
      const word = src.slice(i, j);
      if ((word === 'e' || word === 'E') && j < n && src[j] === "'") { const k = pgScanQuoted(src, j, "'", true); tk('string', src.slice(i, k), src.slice(i, k), i, k); i = k; continue; }
      if ((word === 'u' || word === 'U') && j + 1 < n && src[j] === '&' && (src[j + 1] === "'" || src[j + 1] === '"')) throw pgLexErr(src, i, '不支持 U& 形式的 Unicode 转义字符串 / 标识符');
      tk('word', word, asciiLower(word), i, j); i = j; continue;
    }
    if (c === '$') {
      if (i + 1 < n && isDg(src[i + 1])) throw pgLexErr(src, i, '在线查询不支持参数占位符 $n，请直接写入字面量');
      let j = i + 1; if (j < n && isIS(src[j])) while (j < n && isIC(src[j]) && src[j] !== '$') j++;
      if (j >= n || src[j] !== '$') throw pgLexErr(src, i, '无法识别的 $ 符号');
      const tag = src.slice(i, j + 1), end = src.indexOf(tag, j + 1);
      if (end < 0) throw pgLexErr(src, i, `美元引号字符串没有结束（缺少 ${tag}）`);
      const k = end + tag.length; tk('string', src.slice(i, k), src.slice(i, k), i, k); i = k; continue;
    }
    if (isDg(c) || (c === '.' && i + 1 < n && isDg(src[i + 1]))) { const j = pgScanNumber(src, i); tk('number', src.slice(i, j), src.slice(i, j), i, j); i = j; continue; }
    if (isOC(c)) {
      let j = i;
      while (j < n && isOC(src[j])) { if (src[j] === '-' && src[j + 1] === '-') break; if (src[j] === '/' && src[j + 1] === '*') break; j++; }
      tk('op', src.slice(i, j), src.slice(i, j), i, j); i = j; continue;
    }
    if ('()[],;.:'.includes(c)) { tk('punct', c, c, i, i + 1); i++; continue; }
    throw pgLexErr(src, i, `不允许的字符 ${JSON.stringify(c)}`);
  }
  return {toks, comments};
}
const tIsWord = (t, w) => !!t && t.kind === 'word' && t.low === w;
const tIsPunct = (t, p) => !!t && t.kind === 'punct' && t.text === p;
const tIsOne = (t, ...ws) => !!t && t.kind === 'word' && ws.includes(t.low);
const tIdent = t => !!t && (t.kind === 'word' || t.kind === 'quoted');
const fold = t => t.kind === 'quoted' ? t.text : t.low;

const PG_DENY_STMT = {
  insert: 'INSERT 会写入数据', update: 'UPDATE 会修改数据', delete: 'DELETE 会删除数据', merge: 'MERGE 会修改数据', truncate: 'TRUNCATE 会清空表', copy: 'COPY 可以读写服务器文件或执行外部程序',
  create: 'CREATE 是 DDL', alter: 'ALTER 是 DDL', drop: 'DROP 是 DDL', comment: 'COMMENT 会修改对象注释', security: 'SECURITY LABEL 会修改安全标签',
  grant: 'GRANT 会修改权限', revoke: 'REVOKE 会修改权限', reassign: 'REASSIGN OWNED 会修改对象属主', call: 'CALL 会执行存储过程（可能有副作用）', do: 'DO 会执行匿名代码块',
  set: 'SET 会修改会话变量', reset: 'RESET 会修改会话变量', discard: 'DISCARD 会修改会话状态',
  begin: '事务控制语句不允许（在线查询已经在只读事务里执行）', start: '事务控制语句不允许（在线查询已经在只读事务里执行）', commit: '事务控制语句不允许（可能逃出只读事务）', end: '事务控制语句不允许（可能逃出只读事务）',
  abort: '事务控制语句不允许', rollback: '事务控制语句不允许', savepoint: '事务控制语句不允许', release: '事务控制语句不允许', prepare: 'PREPARE 不允许（可能 PREPARE TRANSACTION）',
  execute: 'EXECUTE 会执行预备语句', deallocate: 'DEALLOCATE 会修改会话状态', declare: 'DECLARE 游标不允许', fetch: 'FETCH 游标不允许', move: 'MOVE 游标不允许', close: 'CLOSE 游标不允许',
  listen: 'LISTEN 是通知机制', unlisten: 'UNLISTEN 是通知机制', notify: 'NOTIFY 是通知机制', lock: 'LOCK 会加表锁', vacuum: 'VACUUM 是维护命令', analyze: 'ANALYZE 会更新统计信息', analyse: 'ANALYZE 会更新统计信息',
  cluster: 'CLUSTER 会重写表', reindex: 'REINDEX 会重建索引', refresh: 'REFRESH MATERIALIZED VIEW 会写数据', checkpoint: 'CHECKPOINT 是维护命令', load: 'LOAD 会加载服务器上的共享库', import: 'IMPORT FOREIGN SCHEMA 会创建对象',
};
const PG_DENY_WORD = {insert: 'INSERT（可写 CTE 或写入语句）', update: 'UPDATE（可写 CTE、FOR UPDATE 行锁）', delete: 'DELETE（可写 CTE 或删除语句）', merge: 'MERGE（可写 CTE 或合并语句）', truncate: 'TRUNCATE', into: 'INTO（SELECT … INTO 会建表）', uescape: 'UESCAPE（Unicode 转义）'};
const PG_FN_EXACT = {
  pg_sleep: '会让会话睡眠并占住连接', pg_sleep_for: '会让会话睡眠并占住连接', pg_sleep_until: '会让会话睡眠并占住连接', pg_terminate_backend: '会终止别人的会话', pg_cancel_backend: '会取消别人的查询', pg_reload_conf: '会重载服务器配置',
  set_config: '会修改会话 / 事务配置（可能关闭只读保护）', nextval: '会推进序列（有副作用）', setval: '会修改序列（有副作用）', setseed: '会修改会话随机数种子', pg_notify: '会发送通知', pg_export_snapshot: '会导出快照并持有它',
  pg_promote: '会提升备库', pg_switch_wal: '会切换 WAL', pg_switch_xlog: '会切换 WAL', pg_rotate_logfile: '会轮转日志文件', pg_extension_config_dump: '会修改扩展配置', pg_stat_file: '可以读取服务器文件信息',
  ts_stat: '会把字符串当 SQL 执行', pg_stat_get_activity: '会泄露其它会话的 SQL 文本', pg_show_all_file_settings: '会泄露配置文件内容', loread: 'large object 读写', lowrite: 'large object 读写',
  pg_current_logfile: '涉及服务器文件', pg_logdir_ls: '可以列出服务器目录', pg_truncate_visibility_map: '会修改可见性映射', pg_wal_replay_pause: '会暂停备库回放', pg_wal_replay_resume: '会恢复备库回放',
};
const PG_FN_PREFIX = [['pg_read_', '可以读取服务器文件'], ['pg_ls_', '可以列出服务器目录'], ['pg_file_', '可以读写服务器文件'], ['lo_', 'large object 读写（可读写服务器文件）'], ['dblink', 'dblink 可以连接任意库并执行 SQL'], ['postgres_fdw', '外部数据包装器'],
  ['pg_advisory', '咨询锁会占住连接并污染连接池'], ['pg_try_advisory', '咨询锁会占住连接并污染连接池'], ['pg_terminate_', '会终止别人的会话'], ['pg_cancel_', '会取消别人的查询'], ['pg_logical_', '逻辑复制槽读写'], ['pg_replication_', '复制槽 / 复制源管理'],
  ['pg_create_', '会创建对象 / 复制槽 / 还原点'], ['pg_drop_', '会删除对象 / 复制槽'], ['pg_copy_', '会复制复制槽'], ['pg_switch_', '会切换 WAL'], ['pg_rotate_', '会轮转日志'], ['pg_start_', '备份控制'], ['pg_stop_', '备份控制'], ['pg_backup_', '备份控制'],
  ['pg_import_', '会导入数据 / 排序规则'], ['pg_log_backend_', '会让其它会话输出日志'], ['pg_stat_reset', '会重置统计信息'], ['pg_set_', '会修改统计信息等目录数据'], ['pg_clear_', '会清除统计信息等目录数据'], ['pg_restore_', '会修改统计信息等目录数据'],
  ['pg_get_wal_', '可以读取 WAL 内容'], ['pg_stat_get_backend_', '会泄露其它会话的 SQL 文本'], ['pg_wal_replay_', '会控制备库回放']];
const PG_REL = {
  pg_authid: '含角色密码哈希', pg_shadow: '含角色密码哈希', pg_auth_members: '角色成员关系', pg_db_role_setting: '角色 / 库级配置', pg_user_mapping: '含外部服务器凭据', pg_user_mappings: '含外部服务器凭据', pg_foreign_server: '含外部服务器连接选项',
  pg_foreign_data_wrapper: '含外部数据包装器选项', pg_foreign_table: '含外部表选项', pg_largeobject: 'large object 内容', pg_largeobject_metadata: 'large object 元数据', pg_statistic: '含列的采样值（绕过表权限）',
  pg_statistic_ext_data: '含扩展统计的采样值（绕过表权限）', pg_subscription: '含订阅连接串', pg_stat_subscription: '订阅状态', pg_stat_wal_receiver: '含上游连接串', pg_stat_activity: '含其它会话的 SQL 文本', pg_stat_replication: '复制连接信息',
  pg_file_settings: '配置文件内容', pg_hba_file_rules: 'pg_hba.conf 内容', pg_ident_file_mappings: 'pg_ident.conf 内容', pg_replication_origin: '复制源', pg_replication_origin_status: '复制源', pg_config: '服务器编译与路径信息',
};
const pgDenyFn = name => { if (PG_FN_EXACT[name]) return PG_FN_EXACT[name]; for (const [p, w] of PG_FN_PREFIX) if (name.startsWith(p)) return w; if (name.includes('_to_xml')) return '把表名 / 查询字符串当 SQL 执行，会绕过表级权限检查'; return ''; };
const pgDenyRel = name => PG_REL[name] || (name.startsWith('pg_stat_statements') ? '含其它会话的 SQL 文本' : '');
const PG_EXPLAIN_OPT = setOf('verbose', 'costs', 'settings', 'generic_plan', 'buffers', 'summary', 'memory', 'format');
const PG_SAFE_SRF = setOf('generate_series', 'generate_subscripts', 'unnest', 'regexp_matches', 'regexp_split_to_table', 'string_to_table', 'json_each', 'json_each_text', 'jsonb_each', 'jsonb_each_text', 'json_array_elements', 'json_array_elements_text', 'jsonb_array_elements',
  'jsonb_array_elements_text', 'json_object_keys', 'jsonb_object_keys', 'json_to_record', 'json_to_recordset', 'jsonb_to_record', 'jsonb_to_recordset', 'json_populate_record', 'json_populate_recordset', 'jsonb_populate_record', 'jsonb_populate_recordset',
  'jsonb_path_query', 'jsonb_path_query_tz', 'json_table', 'xmltable', 'ts_debug', 'ts_parse', 'ts_token_type', 'pg_get_keywords', 'pg_show_all_settings', 'pg_available_extensions', 'pg_available_extension_versions', 'pg_timezone_names', 'pg_timezone_abbrevs',
  'current_setting', 'version', 'now', 'unnest_multirange', 'pg_options_to_table', 'pg_tablespace_databases', 'pg_listening_channels', 'pg_get_catalog_foreign_keys');
const PG_NOFROM_FN = setOf('extract', 'substring', 'trim', 'overlay', 'position');
const PG_FROM_END = setOf('where', 'group', 'having', 'order', 'limit', 'offset', 'fetch', 'union', 'intersect', 'except', 'window', 'for', 'returning', 'select', 'into');
const rejectPg = why => pgErr(`在线查询只允许只读语句（SELECT / WITH … SELECT / VALUES / TABLE / EXPLAIN / SHOW）：${why}。数据变更请通过「SQL 上线」提交工单。`);
const isSystemSchema = s => { const l = String(s).toLowerCase(); return l === 'information_schema' || l.startsWith('pg_'); };

function pgParseExplainHeader(st) {
  let i = 1;
  if (i >= st.length) throw rejectPg('EXPLAIN 后面缺少语句');
  if (tIsPunct(st[i], '(')) {
    let depth = 0, cur = [];
    const flush = () => {
      if (!cur.length) throw rejectPg('EXPLAIN 选项列表为空');
      const name = cur[0];
      if (name.kind !== 'word') throw rejectPg('EXPLAIN 选项必须是关键字');
      if (name.low === 'analyze' || name.low === 'analyse') throw rejectPg('EXPLAIN ANALYZE 会真正执行语句，不允许；请使用不带 ANALYZE 的 EXPLAIN');
      if (!PG_EXPLAIN_OPT.has(name.low)) throw rejectPg('EXPLAIN 选项 ' + name.text.toUpperCase() + ' 不在放行列表内（放行：VERBOSE / COSTS / SETTINGS / GENERIC_PLAN / BUFFERS / SUMMARY / MEMORY / FORMAT）');
      if (cur.length > 2) throw rejectPg('EXPLAIN 选项的写法不合法');
      if (cur.length === 2 && !['word', 'number', 'string'].includes(cur[1].kind)) throw rejectPg('EXPLAIN 选项的值不合法');
      cur = [];
    };
    for (let j = i; j < st.length; j++) {
      const t = st[j];
      if (tIsPunct(t, '(')) { depth++; if (depth > 1) throw rejectPg('EXPLAIN 选项里不允许嵌套括号'); }
      else if (tIsPunct(t, ')')) { depth--; if (depth === 0) { flush(); i = j + 1; if (i >= st.length) throw rejectPg('EXPLAIN 后面缺少语句'); return i; } }
      else if (tIsPunct(t, ',') && depth === 1) flush();
      else cur.push(t);
    }
    throw rejectPg('EXPLAIN 选项括号没有闭合');
  }
  while (i < st.length && tIsOne(st[i], 'analyze', 'analyse', 'verbose')) { if (st[i].low !== 'verbose') throw rejectPg('EXPLAIN ANALYZE 会真正执行语句，不允许；请使用不带 ANALYZE 的 EXPLAIN'); i++; }
  if (i >= st.length) throw rejectPg('EXPLAIN 后面缺少语句');
  return i;
}
function pgScanDenied(st) {
  for (let i = 0; i < st.length; i++) {
    const t = st[i];
    if (t.kind === 'word') {
      if (PG_DENY_WORD[t.low]) throw rejectPg(`语句中含有 ${PG_DENY_WORD[t.low]}；如果它是列名或别名，请用双引号括起来`);
      if (t.low === 'for' && i + 1 < st.length && tIsOne(st[i + 1], 'share', 'no', 'key')) throw rejectPg('不允许行级锁子句（FOR SHARE / FOR KEY SHARE）');
    }
    if (!tIdent(t)) continue;
    const why = pgDenyRel(t.low);
    if (why) throw rejectPg(`不允许访问系统对象 ${t.text}（${why}）`);
    if (i + 1 < st.length && tIsPunct(st[i + 1], '(')) { const w = pgDenyFn(t.low); if (w) throw rejectPg(`不允许调用函数 ${t.text}（${w}）`); }
  }
}
/* 涉及的表（check.go tableScanner 的移植） */
function pgScanTables(toks, from, dbName) {
  const W = {NONE: 0, NAME: 1, COLS: 2, BODY: 3, AFTER: 4};
  const refs = [], funcs = []; let hasLimit = false;
  const mk = (kind, first, ctes, extra) => Object.assign({kind, first, inFrom: false, expect: false, noFrom: false, ctes: new Set(ctes || []), with: W.NONE, recursive: false, pending: '', bodyOf: ''}, extra || {});
  const stack = [mk(0, from, [])];
  const top = () => stack[stack.length - 1];
  const tok = i => (i >= 0 && i < toks.length ? toks[i] : null);
  const takeName = i => {
    const sc = top(), parts = [fold(toks[i])]; let j = i;
    while (j + 2 < toks.length && tIsPunct(toks[j + 1], '.') && tIdent(toks[j + 2])) { parts.push(fold(toks[j + 2])); j += 2; }
    sc.expect = false;
    if (j + 1 < toks.length && tIsPunct(toks[j + 1], '(')) { funcs.push(parts); return j; }
    if (parts.length === 1) { if (sc.ctes.has(parts[0])) return j; }
    else if (parts.length === 2) { /* schema.table */ }
    else if (parts.length === 3) { if (dbName && parts[0] !== dbName) throw pgErr(`不支持跨库引用 ${parts[0]}.${parts[1]}.${parts[2]}（PostgreSQL 的连接绑定在一个库上，请切换到目标库后查询）`); }
    else throw pgErr(`不支持的对象名称：${parts.join('.')}`);
    refs.push(parts); return j;
  };
  for (let i = from; i < toks.length; i++) {
    const t = toks[i]; const sc = top();
    if (sc.with !== W.NONE) {
      if (sc.with === W.NAME) {
        if (tIsWord(t, 'recursive') && sc.pending === '' && !sc.recursive) { sc.recursive = true; continue; }
        if (tIdent(t)) { sc.pending = fold(t); sc.with = W.COLS; continue; }
        throw pgErr('无法解析 WITH 子句：CTE 名称位置不是标识符');
      } else if (sc.with === W.COLS) {
        if (tIsPunct(t, '(')) { let depth = 0; for (; i < toks.length; i++) { if (tIsPunct(toks[i], '(')) depth++; else if (tIsPunct(toks[i], ')')) { depth--; if (depth === 0) break; } } continue; }
        if (tIsWord(t, 'as')) { sc.with = W.BODY; continue; }
        throw pgErr('无法解析 WITH 子句：CTE 名称后面缺少 AS');
      } else if (sc.with === W.BODY) {
        if (tIsOne(t, 'not', 'materialized')) continue;
        if (tIsPunct(t, '(')) { const child = mk('(', i + 1, sc.ctes, {bodyOf: sc.pending}); if (sc.recursive) child.ctes.add(sc.pending); sc.with = W.AFTER; stack.push(child); continue; }
        throw pgErr('无法解析 WITH 子句：AS 后面缺少括号');
      } else if (sc.with === W.AFTER) {
        if (tIsPunct(t, ',')) { sc.with = W.NAME; sc.pending = ''; continue; }
        sc.with = W.NONE;
        if (!(tIsOne(t, 'select', 'values', 'table') || tIsPunct(t, '('))) throw rejectPg('WITH 的主语句必须是 SELECT / VALUES / TABLE');
      }
    }
    if (tIsPunct(t, '(') || tIsPunct(t, '[')) {
      const child = mk(t.text, i + 1, sc.ctes);
      if (t.text === '(') {
        const next = tok(i + 1), derived = sc.expect && tIsOne(next, 'select', 'with', 'values', 'table'), nested = sc.expect && !derived;
        child.inFrom = nested; child.expect = nested;
        const prev = tok(i - 1); if (prev && !sc.expect && prev.kind === 'word' && PG_NOFROM_FN.has(prev.low)) child.noFrom = true;
        sc.expect = false;
      }
      stack.push(child);
    } else if (tIsPunct(t, ')') || tIsPunct(t, ']')) {
      if (stack.length <= 1 || (t.text === ')') !== (sc.kind === '(')) throw pgErr('括号不匹配');
      if (sc.with !== W.NONE) throw pgErr('无法解析 WITH 子句：不完整');
      stack.pop(); if (sc.bodyOf) top().ctes.add(sc.bodyOf);
    } else if (tIsPunct(t, ',')) { if (sc.inFrom) sc.expect = true; }
    else if (t.kind === 'word') {
      let skip = false;
      switch (t.low) {
        case 'with': if (i === sc.first) { sc.with = W.NAME; sc.pending = ''; sc.recursive = false; skip = true; } break;
        case 'from': { const prev = tok(i - 1), prev2 = tok(i - 2); const isDF = tIsWord(prev, 'distinct') && (tIsWord(prev2, 'is') || tIsWord(prev2, 'not')); if (sc.noFrom || isDF) { skip = true; break; } sc.inFrom = true; sc.expect = true; skip = true; break; }
        case 'join': sc.inFrom = true; sc.expect = true; skip = true; break;
        case 'select': sc.noFrom = false; break;
        case 'table': { const prev = tok(i - 1); if (i === sc.first || (prev && tIsOne(prev, 'union', 'intersect', 'except', 'all', 'distinct'))) { sc.expect = true; skip = true; } break; }
        case 'lateral': case 'only': if (sc.expect) skip = true; break;
        case 'rows': { const nx = tok(i + 1); if (nx && sc.expect && tIsWord(nx, 'from')) skip = true; break; }
        case 'limit': case 'fetch': if (stack.length === 1) hasLimit = true; break;
      }
      if (skip) continue;
      if (sc.inFrom && PG_FROM_END.has(t.low)) { sc.inFrom = false; sc.expect = false; continue; }
      if (sc.expect) i = takeName(i);
    } else if (t.kind === 'quoted') { if (sc.expect) i = takeName(i); }
  }
  if (stack.length !== 1) throw pgErr('括号不匹配');
  if (top().with !== W.NONE) throw pgErr('无法解析 WITH 子句：不完整');
  return {refs, funcs, hasLimit};
}
/* 对外：检查一条语句；通过返回 ck，拒绝抛 EErr(bad_query) */
PG.check = function (text, db) {
  if (text.length > 64 << 10) throw pgErr('SQL 过长（上限 64 KB）');
  const {toks, comments} = pgLex(text);
  const stmts = []; let from = 0;
  toks.forEach((t, i) => { if (tIsPunct(t, ';')) { if (i > from) stmts.push([from, i]); from = i + 1; } });
  if (from < toks.length) stmts.push([from, toks.length]);
  if (!stmts.length) throw pgErr('没有有效的 SQL 语句');
  if (stmts.length > 1) throw pgErr(`检测到 ${stmts.length} 条语句，在线查询一次只能执行一条；请选中其中一条再运行。`);
  const st = toks.slice(stmts[0][0], stmts[0][1]);
  { const stack = []; for (const t of st) { if (t.kind !== 'punct') continue; if (t.text === '(' || t.text === '[') stack.push(t); else if (t.text === ')' || t.text === ']') { const want = t.text === ')' ? '(' : '['; if (!stack.length || stack[stack.length - 1].text !== want) throw pgLexErr(text, t.start, `括号不匹配（多余的 ${t.text}）`); stack.pop(); } } if (stack.length) throw pgLexErr(text, stack[stack.length - 1].start, `括号不匹配（${stack[stack.length - 1].text} 没有闭合）`); }
  /* 去注释后的单条语句文本 */
  let sql = '', pos = st[0].start; const end = st[st.length - 1].end;
  for (const [cs, ce] of comments) { if (ce <= st[0].start || cs >= end) continue; if (cs > pos) sql += text.slice(pos, cs); sql += ' '; if (ce > pos) pos = ce; }
  if (pos < end) sql += text.slice(pos, end);
  sql = sql.trim();
  const ck = {kind: '', sql, tables: [], functions: [], needAllPriv: false, hasLimit: false};
  let lead = 0; while (lead < st.length && tIsPunct(st[lead], '(')) lead++;
  if (lead >= st.length || st[lead].kind !== 'word') throw rejectPg('语句必须以 SELECT / WITH / VALUES / TABLE / EXPLAIN / SHOW 开头');
  const first = st[lead]; let innerFrom = 0;
  switch (first.low) {
    case 'select': case 'with': case 'values': case 'table': ck.kind = 'select'; break;
    case 'explain': if (lead !== 0) throw rejectPg('EXPLAIN 不能放在括号里'); innerFrom = pgParseExplainHeader(st); ck.kind = 'explain'; break;
    case 'show': {
      if (lead !== 0) throw rejectPg('SHOW 不能放在括号里');
      if (st.length < 2) throw rejectPg('SHOW 后面缺少参数名');
      for (const t of st.slice(1)) if (!(t.kind === 'word' || t.kind === 'quoted' || tIsPunct(t, '.'))) throw rejectPg('SHOW 后面只能是参数名');
      ck.kind = 'show'; break;
    }
    default: if (PG_DENY_STMT[first.low]) throw rejectPg(PG_DENY_STMT[first.low]); throw rejectPg('不支持以 ' + first.text.toUpperCase() + ' 开头的语句');
  }
  pgScanDenied(st);
  if (ck.kind === 'show') return ck;
  const body = st.slice(innerFrom); let bl = 0; while (bl < body.length && tIsPunct(body[bl], '(')) bl++;
  if (bl >= body.length || !tIsOne(body[bl], 'select', 'with', 'values', 'table')) throw rejectPg('EXPLAIN 只能解释 SELECT / WITH / VALUES / TABLE 语句');
  const sc = pgScanTables(st, innerFrom, db);
  ck.hasLimit = sc.hasLimit;
  const seen = new Set();
  for (const parts of sc.refs) {
    const ref = parts.length === 1 ? {schema: 'public', name: parts[0]} : parts.length === 2 ? {schema: parts[0], name: parts[1]} : {schema: parts[1], name: parts[2]};
    const k = ref.schema + '.' + ref.name; if (!seen.has(k)) { seen.add(k); ck.tables.push(ref); }
  }
  for (const f of sc.funcs) { ck.functions.push(f.join('.')); const last = f[f.length - 1], builtin = f.length === 1 || (f.length === 2 && f[0] === 'pg_catalog'); if (!(builtin && PG_SAFE_SRF.has(last))) ck.needAllPriv = true; }
  return ck;
};
PG.execSql = (ck, maxRows) => ck.kind === 'select' && !ck.hasLimit && maxRows > 0 ? ck.sql + ' LIMIT ' + (maxRows + 1) : ck.sql;

/* ---- 结果整形 ---- */
const PG_T = {bigint: 'int8', integer: 'int4', smallint: 'int2', boolean: 'bool', text: 'text', date: 'date', jsonb: 'jsonb', json: 'json', uuid: 'uuid', bytea: 'bytea', 'double precision': 'float8', real: 'float4', 'timestamp with time zone': 'timestamptz', 'timestamp without time zone': 'timestamp', 'text[]': 'text[]', 'integer[]': 'int4[]', 'bigint[]': 'int8[]'};
function pgTypeName(d) {
  if (d.pgT) return d.pgT === 'timestamptz' ? 'timestamptz' : d.pgT;
  if (d.t) { const t = String(d.t).toLowerCase(); if (PG_T[t]) return PG_T[t]; if (/^numeric/.test(t)) return 'numeric'; if (/^character varying/.test(t)) return 'varchar'; if (/^character\(/.test(t)) return 'bpchar'; return t; }
  return ({int: 'int4', big: 'int8', float: 'float8', dec: 'numeric', str: 'text', bool: 'bool', date: 'date', ts: 'timestamptz', arr: 'text[]', obj: 'jsonb', uuid: 'uuid', bytes: 'bytea', enum: 'text', null: 'text'})[d.k] || 'text';
}
const CELL_MAX = 64 << 10;
function jsonSafe(v) { return JSON.stringify(v, (k, x) => isBig(x) ? (x > BigInt(MAX_SAFE) || x < -BigInt(MAX_SAFE) ? String(x) : Number(x)) : isDate(x) ? ymdOf(x.getTime()) + 'T' + hmsOf(x.getTime()) : x); }
function intervalText(ms) {
  const neg = ms < 0; ms = Math.abs(ms); const d = Math.floor(ms / DAY), rest = ms - d * DAY, h = Math.floor(rest / 3600e3), m = Math.floor(rest % 3600e3 / 60e3), s = Math.floor(rest % 60e3 / 1e3);
  return (neg ? '-' : '') + (d ? d + (d === 1 ? ' day ' : ' days ') : '') + `${pad(h)}:${pad(m)}:${pad(s)}`;
}
function pgCell(v, d, st) {
  if (isNull(v)) return null;
  if (v && typeof v === 'object' && !isDate(v) && 'ms' in v && !Array.isArray(v)) return intervalText(v.ms);
  switch (d.k) {
    case 'int': case 'big': { if (isBig(v)) return (v > BigInt(MAX_SAFE) || v < -BigInt(MAX_SAFE)) ? String(v) : Number(v); const n = Math.round(toNum(v)); return n; }
    case 'float': { const n = toNum(v); return Number.isFinite(n) ? n : (isNaN(n) ? 'NaN' : n > 0 ? 'Infinity' : '-Infinity'); }
    case 'dec': { const n = toNum(v); if (!Number.isFinite(n)) return String(n); const sc = d.scale >= 0 ? d.scale : 2; return n.toFixed(Math.min(sc, 16)); }
    case 'bool': return !!v;
    case 'date': return isDate(v) ? ymdOf(v.getTime()) : String(v);
    case 'ts': {
      if (!isDate(v)) return String(v);
      const ms = v.getTime(), base = ymdOf(ms) + 'T' + hmsOf(ms), f = lparts(ms).ms;
      const tz = pgTypeName(d) === 'timestamp' ? '' : '+08:00';
      return base + (f ? '.' + pad(f, 3) + (tz ? '' : '000') : '') + tz;
    }
    case 'arr': return jsonSafe(v);
    case 'obj': return typeof v === 'string' ? jsonSafe(JSON.parse(v)) : jsonSafe(v);
    case 'bytes': { st.binary++; const s = String(v); return '\\x' + s; }
    case 'uuid': return String(v);
  }
  if (isDate(v)) return ymdOf(v.getTime()) + ' ' + hmsOf(v.getTime());
  if (Array.isArray(v) || typeof v === 'object') return jsonSafe(v);
  const s = String(v);
  if (s.length > CELL_MAX) { st.cut++; return s.slice(0, CELL_MAX) + `…（已截断，共 ${new TextEncoder().encode(s).length} 字节）`; }
  return s;
}

PG.run = function (inst, db, ck, lim) {
  const maxRows = lim.maxRows, cx = {dia: 'pg', inst, db, maxRows, ctes: new Map(), scale: 1, defSchema: 'public'};
  const executed = PG.execSql(ck, maxRows), warnings = [];
  let rs, planned = false;
  if (ck.kind === 'show') rs = pgShow(ck, cx);
  else if (ck.kind === 'explain') { rs = pgExplain(ck, cx); planned = true; }
  else {
    let ast;
    try { ast = new Parser(ck.sql, 'pg').parseStatement(); } catch (e) { throw wrapSim(e, ck.sql, 'pg'); }
    try { rs = execQuery(ast, cx); } catch (e) { throw wrapSim(e, ck.sql, 'pg'); }
  }
  const st = {binary: 0, cut: 0};
  let rows = rs.rows, truncated = false, truncatedBy = '';
  if (rows.length > maxRows) { rows = rows.slice(0, maxRows); truncated = true; truncatedBy = 'rows'; }
  const types = rs.cols.map(c => pgTypeName(c.d));
  const out = []; let bytes = 0;
  for (const r of rows) {
    const row = r.map((v, i) => pgCell(v, rs.cols[i].d, st));
    const rb = row.reduce((a, x) => a + (typeof x === 'string' ? x.length : 8), 0);
    if (bytes + rb > (lim.maxBytes || 4 << 20) && out.length) { truncated = true; truncatedBy = 'bytes'; break; }
    bytes += rb; out.push(row);
  }
  if (ck.kind === 'select' && !ck.hasLimit && maxRows > 0) warnings.push(`语句没有 LIMIT，已自动限制最多返回 ${maxRows} 行。`);
  if (truncated) warnings.push(`结果已按${truncatedBy === 'rows' ? '行数上限' : '结果大小上限'}截断（最多 ${maxRows} 行 / ${(lim.maxBytes || 4 << 20) >> 20} MB）；请缩小范围或用 LIMIT / OFFSET 分页。`);
  if (st.binary) warnings.push(`${st.binary} 个 bytea 值按 \\xHH 十六进制显示。`);
  if (st.cut) warnings.push(`${st.cut} 个值超过 ${CELL_MAX >> 10} KB，已截断显示。`);
  return {columns: rs.cols.map(c => c.n), types, rows: out, truncated, truncatedBy, warnings, notices: [], executed, stmtKind: ck.kind, synth: rs.synth, scale: rs.scale};
};
function wrapSim(e, sql, dia) {
  if (e instanceof EErr) return e;
  if (e instanceof SynErr) {
    const near = e.near;
    if (dia === 'pg') return new EErr(`PostgreSQL 报错：${near ? `syntax error at or near "${near}"` : 'syntax error at end of input'}（位置 ${e.pos + 1}） [42601]`, 'bad_query');
    return new EErr(`ClickHouse 错误 62（SQL 语法错误）：Syntax error: failed at position ${e.pos + 1}${near ? ` ('${near}')` : ' (end of query)'}: ${clip(sql.slice(e.pos), 40)}. Expected one of: token, Comma, Arrow, ... (SYNTAX_ERROR)`, 'engine_error');
  }
  if (e instanceof NoSim) return new EErr(`这个写法在原型里没有模拟：${e.message}。真实服务会正常执行；原型请改用更简单的写法。`, 'engine_error', '原型未模拟');
  throw e;
}
function pgShow(ck, cx) {
  const name = ck.sql.replace(/^\s*show\s+/i, '').trim().replace(/"/g, '');
  const m = {server_version: '15.4', server_encoding: 'UTF8', client_encoding: 'UTF8', timezone: 'Asia/Shanghai', datestyle: 'ISO, MDY', transaction_read_only: 'on', default_transaction_read_only: 'on', statement_timeout: '30s', max_connections: '500', work_mem: '16MB', search_path: '"public", pg_catalog', standard_conforming_strings: 'on', lock_timeout: '5s'};
  const k = name.toLowerCase();
  if (k === 'all') return {cols: [{n: 'name', d: T('str')}, {n: 'setting', d: T('str')}, {n: 'description', d: T('str')}], rows: Object.entries(m).map(([a, b]) => [a, b, ''])};
  if (!(k in m)) throw new EErr(`PostgreSQL 报错：unrecognized configuration parameter "${name}" [42704]`, 'bad_query');
  return {cols: [{n: k === 'timezone' ? 'TimeZone' : k, d: T('str')}], rows: [[m[k]]]};
}
function pgExplain(ck, cx) {
  const inner = ck.sql.replace(/^\s*explain\s*(\([^)]*\))?\s*(verbose\s+)?/i, '');
  let ast; try { ast = new Parser(inner, 'pg').parseStatement(); } catch (e) { throw wrapSim(e, inner, 'pg'); }
  const lines = []; const q = ast.setops ? ast.left : ast;
  const li = q.limit ? litOf(q.limit) : null;
  let ind = 0; const push = (s) => { lines.push('  '.repeat(ind) + (ind ? '->  ' : '') + s); };
  const tabs = (q.from || []).map(f => f.kind === 'table' ? resolveTableSafe(f.parts, cx) : null).filter(Boolean);
  if (!tabs.length) { lines.push('Result  (cost=0.00..0.01 rows=1 width=4)'); return {cols: [{n: 'QUERY PLAN', d: T('str')}], rows: lines.map(l => [l])}; }
  const total = tabs[0].td ? tabs[0].td.total : 100;
  const agg = (q.items || []).some(it => it.e && hasAgg(it.e));
  if (li !== null && li !== undefined) { push(`Limit  (cost=0.43..${(li * 0.9 + 1).toFixed(2)} rows=${li} width=96)`); ind++; }
  if (agg) { push(`${q.group ? 'HashAggregate' : 'Aggregate'}  (cost=${(total * 0.013).toFixed(2)}..${(total * 0.013 + 0.02).toFixed(2)} rows=${q.group ? 24 : 1} width=${q.group ? 48 : 8})`); ind++; }
  if (q.order && !(li !== null && tabs[0].td && q.order[0].e.k === 'col' && tabs[0].td.meta.idc === q.order[0].e.parts[q.order[0].e.parts.length - 1])) { push(`Sort  (cost=${(total * 0.05).toFixed(2)}..${(total * 0.05 + 8).toFixed(2)} rows=${Math.min(total, 1000)} width=96)`); ind++; lines.push('  '.repeat(ind) + `  Sort Key: ${q.order.map(o => (o.e.k === 'col' ? o.e.parts.join('.') : '…') + (o.dir < 0 ? ' DESC' : '')).join(', ')}`); }
  const td = tabs[0].td; const conj = splitAnd(q.where);
  const keyed = td && conj.find(e => e.k === 'bin' && e.op === '=' && e.l.k === 'col' && td.colMap[e.l.parts[e.l.parts.length - 1].toLowerCase()] && ['PRI', 'UNI'].includes(td.colMap[e.l.parts[e.l.parts.length - 1].toLowerCase()].key));
  const nm = td ? td.n : 'x';
  if (keyed) { const col = td.colMap[keyed.l.parts[keyed.l.parts.length - 1].toLowerCase()], ix = (td.t.idx || []).find(x => x[1][0] === col.n);
    push(`Index Scan using ${ix ? ix[0] : nm.split('.').pop() + '_pkey'} on ${nm}  (cost=0.43..8.45 rows=1 width=96)`); lines.push('  '.repeat(ind) + `  Index Cond: (${col.n} = ${keyed.r.k === 'str' ? "'" + keyed.r.v + "'::text" : keyed.r.v})`); }
  else if (q.where) { push(`Seq Scan on ${nm}  (cost=0.00..${(total * 0.17).toFixed(2)} rows=${Math.max(1, Math.round(total * 0.02))} width=96)`); lines.push('  '.repeat(ind) + '  Filter: ' + clip(ck.sql.replace(/^[\s\S]*?\bwhere\b/i, '').replace(/\border by[\s\S]*$/i, '').replace(/\blimit[\s\S]*$/i, '').trim(), 80)); }
  else push(`Seq Scan on ${nm}  (cost=0.00..${(total * 0.17).toFixed(2)} rows=${total} width=96)`);
  if (/^\s*explain\s*\(.*\bformat\s+json/i.test(ck.sql)) return {cols: [{n: 'QUERY PLAN', d: T('obj')}], rows: [[[{Plan: {'Node Type': keyed ? 'Index Scan' : 'Seq Scan', 'Relation Name': nm, 'Total Cost': +(total * 0.17).toFixed(2), 'Plan Rows': keyed ? 1 : total}}]]]};
  return {cols: [{n: 'QUERY PLAN', d: T('str')}], rows: lines.map(l => [l])};
}
function resolveTableSafe(parts, cx) { try { return resolveTable(parts, cx); } catch (e) { return null; } }

/* ============================================================================
   六、ClickHouse：语句检查（internal/engine/clickhouse/{lexer,check,denylist}.go 的移植，拒绝原因原文）+ 受限执行 + 结果整形
   ========================================================================== */
const CH = {};
const chErr = msg => new EErr(msg, 'bad_query');
const chPos = (src, off) => { let line = 1, col = 1; for (let i = 0; i < off && i < src.length; i++) { if (src[i] === '\n') { line++; col = 1; } else col++; } return `第 ${line} 行第 ${col} 列`; };
const isWS = c => c === '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z');
const isWC = c => isWS(c) || isDg(c);
function chLexQuoted(src, i, q, kind) {
  const n = src.length; let s = '', esc = false, j = i + 1;
  while (j < n) {
    const ch = src[j];
    if (ch === '\\') {
      if (j + 1 >= n) throw chErr(`SQL 在${chPos(src, i)}有未结束的引号`);
      const nx = src[j + 1];
      if (nx === '\\' || nx === q) s += nx; else { esc = true; s += '\\' + nx; }
      j += 2;
    } else if (ch === q) { if (src[j + 1] === q) { s += q; j += 2; continue; } return {tok: {kind, text: src.slice(i, j + 1), val: s, esc, start: i, end: j + 1}, end: j + 1}; }
    else { s += ch; j++; }
  }
  throw chErr(`SQL 在${chPos(src, i)}有未结束的引号`);
}
function chLex(src) {
  const toks = [], n = src.length; let i = 0;
  while (i < n) {
    const c = src[i];
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === '\f' || c === '\v') { i++; continue; }
    if (c === '\0' || c < ' ' || c === '\x7f') throw chErr(`SQL 在${chPos(src, i)}含有控制字符，已拒绝`);
    if (c > '\x7f') throw chErr(`SQL 在${chPos(src, i)}含有非 ASCII 字符（引号、注释之外）；标识符里的中文等字符请用反引号括起来`);
    if (c === '-' && src[i + 1] === '-') { while (i < n && src[i] !== '\n') i++; continue; }
    if (c === '/' && src[i + 1] === '*') { const j = src.indexOf('*/', i + 2); if (j < 0) throw chErr(`SQL 在${chPos(src, i)}有未结束的 /* 注释`); i = j + 2; continue; }
    if (c === '#') throw chErr(`SQL 在${chPos(src, i)}含有 # 字符；不支持 # 注释，请改用 -- 或 /* */`);
    if (c === "'") { const r = chLexQuoted(src, i, "'", 'string'); toks.push(r.tok); i = r.end; continue; }
    if (c === '"' || c === '`') { const r = chLexQuoted(src, i, c, 'quoted'); if (r.tok.esc) throw chErr(`SQL 在${chPos(src, i)}的带引号标识符里含有转义序列，已拒绝（标识符里只允许用重复引号表示引号本身）`); toks.push(r.tok); i = r.end; continue; }
    if (c === '$') {
      const m = /^\$(\$|[A-Za-z_][A-Za-z0-9_]*\$)/.exec(src.slice(i));
      if (m) { const open = m[0], k = src.indexOf(open, i + open.length); if (k < 0) throw chErr(`SQL 在${chPos(src, i)}有未结束的 ${open} 字符串`); const end = k + open.length; toks.push({kind: 'string', text: src.slice(i, end), val: src.slice(i + open.length, k), start: i, end}); i = end; continue; }
      let j = i + 1; while (j < n && (isWC(src[j]) || src[j] === '$')) j++; toks.push({kind: 'word', text: src.slice(i, j), start: i, end: j}); i = j; continue;
    }
    if (isDg(c) || (c === '.' && isDg(src[i + 1] || ''))) {
      let j = i + 1; const hexn = src[i] === '0' && (src[i + 1] === 'x' || src[i + 1] === 'X');
      while (j < n) { const ch = src[j]; if (isWC(ch) || ch === '.') j++; else if ((ch === '+' || ch === '-') && !hexn && (src[j - 1] === 'e' || src[j - 1] === 'E')) j++; else break; }
      toks.push({kind: 'number', text: src.slice(i, j), start: i, end: j}); i = j; continue;
    }
    if (isWS(c)) { let j = i + 1; while (j < n && (isWC(src[j]) || src[j] === '$')) j++; toks.push({kind: 'word', text: src.slice(i, j), start: i, end: j}); i = j; continue; }
    toks.push({kind: 'punct', text: c, start: i, end: i + 1}); i++;
  }
  return toks;
}
const cIs = (t, kw) => !!t && t.kind === 'word' && t.text.toLowerCase() === kw;
const cP = (t, p) => !!t && t.kind === 'punct' && t.text === p;
const cIdent = t => !!t && (t.kind === 'word' || t.kind === 'quoted');
const cName = t => t.kind === 'quoted' ? t.val : t.text;
const cLow = t => t.text.toLowerCase();

const CH_TABLE_FN = {
  file: '读服务器本地文件', filecluster: '读服务器本地文件', url: '访问外部网络', urlcluster: '访问外部网络', s3: '访问对象存储', s3cluster: '访问对象存储', gcs: '访问对象存储', hdfs: '访问 HDFS', hdfscluster: '访问 HDFS',
  azureblobstorage: '访问对象存储', azureblobstoragecluster: '访问对象存储', deltalake: '访问数据湖', deltalakecluster: '访问数据湖', iceberg: '访问数据湖', icebergcluster: '访问数据湖', hudi: '访问数据湖', paimon: '访问数据湖', hive: '访问外部系统',
  mysql: '访问外部数据库', postgresql: '访问外部数据库', mongodb: '访问外部数据库', jdbc: '访问外部数据库', odbc: '访问外部数据库', sqlite: '读服务器本地文件', redis: '访问外部系统', meilisearch: '访问外部系统', ytsaurus: '访问外部系统', arrowflight: '访问外部系统', prometheusquery: '访问外部系统',
  remote: '连接其它服务器', remotesecure: '连接其它服务器', cluster: '连接其它集群', clusterallreplicas: '连接其它集群', executable: '执行外部程序', input: '读取客户端输入',
  generaterandom: '产生无限数据', numbers_mt: '多线程生成超大数据', zeros: '产生无限数据', zeros_mt: '产生无限数据', loop: '产生无限数据', generate_series: '产生超大数据', generateseries: '产生超大数据', null: '不在在线查询范围内',
  merge: '按正则匹配任意多张表，无法静态确定涉及的表', mergetreeindex: '读取内部索引', mergetreeanalyzeindexes: '读取内部索引', mergetreeprojection: '读取内部结构', dictionary: '请直接 FROM 字典名', view: '不在在线查询范围内', viewifpermitted: '不在在线查询范围内',
  viewexplain: '不在在线查询范围内', format: '不在在线查询范围内', fuzzjson: '不在在线查询范围内', fuzzquery: '不在在线查询范围内', timeseriesdata: '不在在线查询范围内', timeseriesmetrics: '不在在线查询范围内', timeseriestags: '不在在线查询范围内',
};
const CH_SCALAR_OK = setOf('format'), CH_TABLE_ALLOWED = setOf('numbers', 'values');
const CH_DENY_SCALAR = {sleep: '会让查询睡眠，占住连接和线程', sleepeachrow: '会让查询睡眠，占住连接和线程', hascolumnintable: '带远端参数时会连接其它服务器', catboostevaluate: '会读取服务器上的模型文件', trap: '调试用函数，会使服务异常', modelevaluate: '会读取服务器上的模型文件', executesqlfromfile: '会读取服务器上的文件'};
const CH_SYSTEM_OK = setOf('one', 'databases', 'tables', 'columns', 'parts', 'parts_columns', 'data_skipping_indices', 'data_type_families', 'table_engines', 'time_zones');
const CH_DENY_ENGINES = {file: '读服务器本地文件', filelog: '读服务器本地文件', url: '访问外部网络', s3: '访问对象存储', s3queue: '访问对象存储', hdfs: '访问 HDFS', azureblobstorage: '访问对象存储', azurequeue: '访问对象存储', deltalake: '访问数据湖', deltalakes3: '访问数据湖', iceberg: '访问数据湖', icebergs3: '访问数据湖', hudi: '访问数据湖', hive: '访问外部系统',
  mysql: '访问外部数据库', postgresql: '访问外部数据库', materializedpostgresql: '访问外部数据库', mongodb: '访问外部数据库', jdbc: '访问外部数据库', odbc: '访问外部数据库', sqlite: '读服务器本地文件', redis: '访问外部系统', kafka: '消费外部消息队列（会移动消费位点）', rabbitmq: '消费外部消息队列（会移动消费位点）', nats: '消费外部消息队列（会移动消费位点）',
  executable: '执行外部程序', executablepool: '执行外部程序', generaterandom: '产生无限数据', ytsaurus: '访问外部系统', arrowflight: '访问外部系统', meilisearch: '访问外部系统'};
const CH_DENY_FIRST = {insert: '写入', alter: '修改表结构', create: '创建对象', drop: '删除对象', truncate: '清空表', rename: '重命名', optimize: '触发合并（OPTIMIZE）', kill: '终止查询 / 变更', system: '服务器管理命令', attach: '挂载对象', detach: '卸载对象', grant: '授权', revoke: '回收权限',
  set: '修改会话设置', use: '切换库（库由页面顶部的下拉框选择）', check: '校验表（CHECK）', backup: '备份', restore: '恢复', exchange: '交换表', undrop: '恢复被删除的表', watch: '实时视图订阅（WATCH）', delete: '删除数据', update: '修改数据', replace: '写入', move: '移动数据',
  call: '调用过程', begin: '事务控制', commit: '事务控制', rollback: '事务控制', unlock: '锁', lock: '锁', analyze: '分析', vacuum: '维护'};
const CH_FROM_END = setOf('where', 'prewhere', 'group', 'having', 'order', 'limit', 'union', 'intersect', 'except', 'window', 'qualify', 'offset', 'fetch', 'into', 'settings', 'format', 'select', 'array');
const chSystemList = () => [...CH_SYSTEM_OK].sort().join(' / ');
const chIsSystem = db => String(db).toLowerCase() === 'system';
const chIsInfo = db => String(db).toLowerCase() === 'information_schema';

function chGlobalScan(toks) {
  for (let i = 0; i < toks.length; i++) {
    const t = toks[i];
    if (!cIdent(t)) continue;
    const prevDot = i > 0 && cP(toks[i - 1], '.'), nextParen = i + 1 < toks.length && cP(toks[i + 1], '(');
    const name = cName(t).toLowerCase();
    if (t.kind === 'word' && !prevDot) {
      switch (name) {
        case 'settings': throw chErr('不允许 SETTINGS 子句：服务端限额与只读模式由平台统一设置，不能在语句里覆盖（列名 / 表名叫 settings 请用反引号括起来）');
        case 'into': throw chErr('不允许 INTO 子句（INSERT INTO / INTO OUTFILE）：在线查询只读');
        case 'outfile': case 'infile': throw chErr(`不允许 ${name.toUpperCase()}：在线查询不能读写文件`);
        case 'format': if (!nextParen) throw chErr('不允许自行指定 FORMAT：在线查询的输出格式固定（只允许写在语句末尾，会被忽略；列名叫 format 请用反引号括起来）'); break;
      }
    }
    if (CH_DENY_SCALAR[name] && !prevDot) throw chErr(`不允许使用函数 ${name}：${CH_DENY_SCALAR[name]}`);
    if (nextParen && CH_TABLE_FN[name] && !CH_SCALAR_OK.has(name)) throw chErr(`不允许使用 ${name}(...)：${CH_TABLE_FN[name]}`);
  }
}
function chCheckTableFunc(toks, i, lname) {
  if (!CH_TABLE_ALLOWED.has(lname)) { const why = CH_TABLE_FN[lname]; if (why) throw chErr(`不允许使用表函数 ${lname}：${why}`); throw chErr(`不允许使用表函数 ${lname}：在线查询的表函数采用默认拒绝，只放行 numbers(N) 与 values(...)`); }
  if (lname !== 'numbers') return;
  let j = i + 2, cnt = 0;
  for (; j < toks.length; j++) {
    const t = toks[j]; if (cP(t, ')')) break; if (cP(t, ',')) continue;
    if (t.kind !== 'number') throw chErr('numbers(...) 的参数只能是整数字面量');
    if (!/^\d+$/.test(t.text) || Number(t.text) > 100000000) throw chErr('numbers(...) 的参数不能超过 100000000');
    cnt++;
  }
  if (j >= toks.length || cnt < 1 || cnt > 3) throw chErr('numbers(...) 需要 1 到 3 个整数字面量参数');
}
function chStringObjectArg(toks, i, fn) {
  if (i >= toks.length || toks[i].kind !== 'string') throw chErr(`${fn} 的第一个参数必须是字符串常量（字典 / 表名），否则无法确定涉及的对象`);
  if (i + 1 >= toks.length || !(cP(toks[i + 1], ',') || cP(toks[i + 1], ')'))) throw chErr(`${fn} 的第一个参数必须是单独的字符串常量`);
  const t = toks[i];
  if (t.esc || /[\0\\`"]/.test(t.val)) throw chErr(`${fn} 的对象名含有转义或特殊字符，已拒绝`);
  const v = t.val; if (v === '') throw chErr(`${fn} 的对象名为空`);
  const k = v.indexOf('.');
  if (k >= 0) { if (v.slice(k + 1).includes('.') || k === 0 || k === v.length - 1) throw chErr(`${fn} 的对象名 "${v}" 写法不支持`); return {db: v.slice(0, k), name: v.slice(k + 1)}; }
  return {db: '', name: v};
}
function chScanSelect(toks) {
  const stack = [{kind: 0, inFrom: false, expect: false, inWith: false, pending: '', defName: '', ctes: new Set()}];
  const top = () => stack[stack.length - 1], isCTE = n => stack.some(f => f.ctes.has(n));
  const refs = [], n = toks.length, peek = i => i < n ? toks[i] : {kind: 'punct', text: ''};
  for (let i = 0; i < n; i++) {
    const t = toks[i], f = top();
    if (cP(t, '(')) {
      const nf = {kind: 0, inFrom: false, expect: false, inWith: false, pending: '', defName: '', ctes: new Set()}, nx = peek(i + 1);
      if (cIs(nx, 'select') || cIs(nx, 'with')) { nf.kind = 0; f.expect = false; }
      else if (f.expect && f.inFrom) { nf.kind = 1; nf.inFrom = true; nf.expect = true; f.expect = false; }
      else nf.kind = 2;
      if (f.pending) { nf.defName = f.pending; f.pending = ''; }
      stack.push(nf);
    } else if (cP(t, ')')) {
      if (stack.length === 1) throw chErr('括号不匹配：多了一个右括号');
      const child = stack.pop(); if (child.defName) top().ctes.add(child.defName);
    } else if (cP(t, ',')) { if (f.inFrom && f.kind !== 2) f.expect = true; }
    else if (cP(t, '{')) { if (f.expect) throw chErr('不支持参数化的表名 {name:Type}'); }
    else if (cIdent(t)) {
      const prevDot = i > 0 && cP(toks[i - 1], '.'), nextParen = cP(peek(i + 1), '('), lname = cName(t).toLowerCase();
      if (nextParen && !prevDot) {
        if (lname.startsWith('dictget') || lname === 'dicthas' || lname === 'dictisin' || lname === 'joinget' || lname === 'joingetornull') refs.push(chStringObjectArg(toks, i + 2, lname));
      }
      if (f.expect && !(t.kind === 'word' && lname === 'select')) {
        if (nextParen && !cP(peek(i + 1), '.')) { chCheckTableFunc(toks, i, lname); f.expect = false; continue; }
        let db = '', name = cName(t);
        if (cP(peek(i + 1), '.') && cIdent(peek(i + 2))) {
          db = cName(t); name = cName(peek(i + 2)); i += 2;
          if (cP(peek(i + 1), '.')) throw chErr('不支持的表名写法（出现多于两段的名字）');
          if (cP(peek(i + 1), '(')) throw chErr(`不允许 ${db}.${name}(...) 形式的调用`);
        }
        f.expect = false;
        if (db === '' && isCTE(name)) continue;
        refs.push({db, name}); continue;
      }
      if (t.kind !== 'word' || prevDot) continue;
      switch (lname) {
        case 'with': { const nx = peek(i + 1); if (!(cIs(nx, 'totals') || cIs(nx, 'fill') || cIs(nx, 'ties') || cIs(nx, 'rollup') || cIs(nx, 'cube'))) f.inWith = true; break; }
        case 'select': f.inWith = false; f.inFrom = false; f.expect = false; break;
        case 'from': if (f.kind !== 2) { f.inFrom = true; f.expect = true; } break;
        case 'join': if (i > 0 && cIs(toks[i - 1], 'array')) { f.inFrom = false; f.expect = false; } else if (f.kind !== 2) { f.inFrom = true; f.expect = true; } break;
        case 'union': case 'intersect': case 'except': { const nx = peek(i + 1); if (lname !== 'except' || cIs(nx, 'select') || cIs(nx, 'with') || cIs(nx, 'all') || cIs(nx, 'distinct')) f.ctes = new Set(); if (f.inFrom) { f.inFrom = false; f.expect = false; } break; }
        case 'as': if (f.inWith && i > 0 && cIdent(toks[i - 1]) && cP(peek(i + 1), '(')) f.pending = cName(toks[i - 1]); break;
        case 'in': {
          const nx = peek(i + 1); if (!cIdent(nx)) break;
          if (nx.kind === 'word') { const l = cLow(nx); if (['null', 'true', 'false', 'select', 'with'].includes(l)) break; }
          if (cP(peek(i + 2), '(')) break;
          let db = '', name = cName(nx), step = 1;
          if (cP(peek(i + 2), '.') && cIdent(peek(i + 3))) { db = cName(nx); name = cName(peek(i + 3)); step = 3; if (cP(peek(i + 4), '(')) throw chErr(`不允许 IN ${db}.${name}(...) 形式的调用`); }
          i += step;
          if (db === '' && isCTE(name)) break;
          refs.push({db, name}); break;
        }
        default: if (f.inFrom && CH_FROM_END.has(lname)) { f.inFrom = false; f.expect = false; }
      }
    }
  }
  if (stack.length !== 1) throw chErr('括号不匹配：缺少右括号');
  return refs;
}
function chQname(toks, i) {
  if (i >= toks.length || !cIdent(toks[i])) return null;
  if (i + 2 < toks.length && cP(toks[i + 1], '.') && cIdent(toks[i + 2])) return {db: cName(toks[i]), name: cName(toks[i + 2]), next: i + 3};
  return {db: '', name: cName(toks[i]), next: i + 1};
}
function chScanExplain(toks) {
  let i = 1;
  const kinds = setOf('ast', 'syntax', 'plan', 'pipeline', 'estimate');
  if (i < toks.length && toks[i].kind === 'word') {
    const w = cLow(toks[i]);
    if (kinds.has(w)) i++;
    else if (w === 'query') { if (i + 1 < toks.length && cIs(toks[i + 1], 'tree')) i += 2; else throw chErr('EXPLAIN QUERY 后面只能是 TREE'); }
    else if (w === 'select' || w === 'with') { /* 无子类型 */ }
    else throw chErr(`EXPLAIN 只放行 AST / SYNTAX / QUERY TREE / PLAN / PIPELINE / ESTIMATE；${toks[i].text.toUpperCase()} 不在白名单内`);
  }
  while (i + 2 < toks.length && toks[i].kind === 'word' && cP(toks[i + 1], '=') && toks[i + 2].kind === 'number') { i += 3; if (i < toks.length && cP(toks[i], ',')) i++; }
  if (i >= toks.length || !(cIs(toks[i], 'select') || cIs(toks[i], 'with'))) throw chErr('EXPLAIN 后面必须是 SELECT / WITH 查询语句');
  return {refs: chScanSelect(toks.slice(i)), sub: toks.slice(i)};
}
function chScanShow(toks) {
  if (toks.length < 2 || toks[1].kind !== 'word') throw chErr('SHOW 后面缺少子命令');
  let i = 1; while (i < toks.length && (cIs(toks[i], 'temporary') || cIs(toks[i], 'full') || cIs(toks[i], 'extended'))) i++;
  if (i >= toks.length) throw chErr('SHOW 后面缺少子命令');
  const sub = cLow(toks[i]); let rest = toks.slice(i + 1);
  const refs = [], dbs = [], info = {sub};
  switch (sub) {
    case 'databases': case 'schemas': case 'tables': case 'dictionaries': break;
    case 'create': {
      if (!rest.length) throw chErr('SHOW CREATE 后面缺少对象类型');
      const kind = cLow(rest[0]);
      if (rest[0].kind !== 'word' || !['table', 'view', 'dictionary', 'database', 'temporary'].includes(kind)) throw chErr('SHOW CREATE 只放行 TABLE / VIEW / DICTIONARY / DATABASE（SHOW CREATE USER / ROLE / QUOTA 等含权限信息，不放行）');
      rest = rest.slice(1);
      if (kind === 'temporary') { if (!rest.length || !cIs(rest[0], 'table')) throw chErr('SHOW CREATE TEMPORARY 后面必须是 TABLE'); rest = rest.slice(1); }
      const q = chQname(rest, 0);
      if (!q || q.next !== rest.length) throw chErr('SHOW CREATE 的参数只能是 [库.]名称');
      info.what = kind;
      if (kind === 'database') { if (q.db) throw chErr('SHOW CREATE DATABASE 后面只能是库名'); return {refs: [], dbs: [q.name], info: Object.assign(info, {db: q.name})}; }
      return {refs: [{db: q.db, name: q.name}], dbs: [], info};
    }
    case 'columns': case 'fields': case 'index': case 'indexes': case 'keys': break;
    default: throw chErr(`SHOW ${toks[i].text.toUpperCase()} 不在白名单内；在线查询的 SHOW 只支持 DATABASES / TABLES / DICTIONARIES / CREATE TABLE|VIEW|DICTIONARY|DATABASE / COLUMNS / INDEX`);
  }
  const objs = [], filler = setOf('from', 'in', 'like', 'ilike', 'not', 'limit', 'full', 'extended', 'temporary');
  for (let j = 0; j < rest.length; j++) {
    const t = rest[j];
    if (t.kind === 'word' && filler.has(cLow(t))) {
      const l = cLow(t);
      if (l === 'from' || l === 'in') { const q = chQname(rest, j + 1); if (!q) throw chErr('SHOW 的 FROM / IN 后面缺少名称'); objs.push(q.db ? q.db + '\0' + q.name : q.name); j = q.next - 1; }
      else if (l === 'like' || l === 'ilike') { if (j + 1 >= rest.length || rest[j + 1].kind !== 'string') throw chErr('SHOW 的 LIKE 后面必须是字符串常量'); info.like = rest[j + 1].val; info.ilike = l === 'ilike'; info.notLike = j > 0 && cIs(rest[j - 1], 'not'); j++; }
      else if (l === 'limit') { if (j + 1 >= rest.length || rest[j + 1].kind !== 'number') throw chErr('SHOW 的 LIMIT 后面必须是数字'); info.limit = Number(rest[j + 1].text); j++; }
    } else throw chErr(`SHOW 语句里出现了不支持的写法（${t.text}）；只支持 FROM / IN 库表名、LIKE '模式'、LIMIT 数字`);
  }
  if (sub === 'tables' || sub === 'dictionaries') { for (const o of objs) { if (o.includes('\0')) throw chErr(`SHOW ${sub.toUpperCase()} FROM 后面只能是库名`); dbs.push(o); } info.db = dbs[0]; }
  else if (sub !== 'databases' && sub !== 'schemas') {
    if (!objs.length || objs.length > 2) throw chErr(`SHOW ${sub.toUpperCase()} 需要 FROM 表名 [FROM 库名]`);
    let db = '', name = objs[0]; const k = name.indexOf('\0'); if (k >= 0) { db = name.slice(0, k); name = name.slice(k + 1); }
    if (objs.length === 2) { if (objs[1].includes('\0')) throw chErr(`SHOW ${sub.toUpperCase()} 的第二个 FROM 只能是库名`); if (db && db !== objs[1]) throw chErr(`SHOW ${sub.toUpperCase()} 指定了两个不同的库`); db = objs[1]; }
    refs.push({db, name});
  }
  return {refs, dbs, info};
}
CH.check = function (text, curDb) {
  if (String(text).trim() === '') throw chErr('SQL 为空');
  if (text.length > 128 << 10) throw chErr('SQL 过长（上限 128 KB）');
  const toks0 = chLex(text);
  if (!toks0.length) throw chErr('SQL 里只有注释，没有可执行的语句');
  let end = toks0.length; if (cP(toks0[end - 1], ';')) end--;
  let body = toks0.slice(0, end);
  for (const t of body) if (cP(t, ';')) throw chErr('检测到多条语句；在线查询一次只能执行一条，请选中其中一条再运行');
  if (!body.length) throw chErr('SQL 为空');
  const ck = {db: curDb, notices: [], tables: [], databases: [], kind: '', stmt: '', info: null};
  const n = body.length;
  if (n >= 2 && cIs(body[n - 2], 'format') && body[n - 1].kind === 'word' && !(n >= 3 && cP(body[n - 3], '.'))) {
    body = body.slice(0, n - 2); ck.notices.push('已忽略 FORMAT 子句：在线查询的输出格式固定');
    if (!body.length) throw chErr('SQL 为空');
  }
  ck.stmt = text.slice(body[0].start, body[body.length - 1].end);
  const first = body[0];
  if (first.kind !== 'word') { if (cP(first, '(')) throw chErr('暂不支持以括号开头的语句；请去掉最外层括号，以 SELECT / WITH 开头'); throw chErr('语句必须以 SELECT / WITH / SHOW / DESCRIBE / EXPLAIN / EXISTS 开头'); }
  const kw = cLow(first);
  if (!['select', 'with', 'show', 'describe', 'desc', 'exists', 'explain'].includes(kw)) {
    if (CH_DENY_FIRST[kw]) throw chErr(`不允许 ${kw.toUpperCase()}：${CH_DENY_FIRST[kw]}。在线查询只读；数据或结构变更请通过 SQL 工单提交`);
    throw chErr(`语句类型 ${first.text.toUpperCase()} 不在在线查询的白名单内，只支持 SELECT / WITH / SHOW / DESCRIBE / EXPLAIN / EXISTS`);
  }
  chGlobalScan(body);
  let refs = [], dbOnly = [];
  if (kw === 'select' || kw === 'with') { ck.kind = 'select'; refs = chScanSelect(body); }
  else if (kw === 'show') { ck.kind = 'show'; const r = chScanShow(body); refs = r.refs; dbOnly = r.dbs; ck.info = r.info; }
  else if (kw === 'describe' || kw === 'desc') {
    ck.kind = 'describe'; let i = 1; if (i < body.length && cIs(body[i], 'table')) i++;
    const q = chQname(body, i); if (!q || q.next !== body.length) throw chErr('DESCRIBE 只支持 DESCRIBE [TABLE] [库.]表名；DESCRIBE 子查询 / 表函数不放行');
    refs = [{db: q.db, name: q.name}];
  } else if (kw === 'exists') {
    ck.kind = 'exists'; let i = 1; if (i < body.length && cIs(body[i], 'temporary')) i++;
    let isDB = false; if (i < body.length) { if (cIs(body[i], 'database')) { isDB = true; i++; } else if (cIs(body[i], 'table') || cIs(body[i], 'view') || cIs(body[i], 'dictionary')) i++; }
    const q = chQname(body, i); if (!q || q.next !== body.length) throw chErr('EXISTS 只支持 EXISTS [TABLE|VIEW|DICTIONARY|DATABASE] [库.]名称');
    if (isDB) { if (q.db) throw chErr('EXISTS DATABASE 后面只能是库名'); dbOnly = [q.name]; } else refs = [{db: q.db, name: q.name}];
  } else { ck.kind = 'explain'; const r = chScanExplain(body); refs = r.refs; }
  const cur = curDb || 'default', seen = new Set(), dbs = new Set();
  for (const r of refs) {
    const db = r.db || cur;
    if (chIsSystem(db)) { if (!(r.name === '' || CH_SYSTEM_OK.has(r.name.toLowerCase()))) throw chErr(`不允许访问 system.${r.name}：system 库在线查询只开放库表结构类表（${chSystemList()}）；账号权限、他人 SQL、集群与凭据信息请通过 DBA 页面查看`); }
    const k = db + '\0' + r.name; dbs.add(db);
    if (!seen.has(k)) { seen.add(k); ck.tables.push({db, name: r.name}); }
  }
  for (const d of dbOnly) dbs.add(d);
  if (ck.tables.length > 200) throw chErr(`语句涉及 ${ck.tables.length} 张表，超过上限 200`);
  ck.databases = [...dbs].sort();
  return ck;
};
/* 执行前的引擎核对（engine.go）：命中外部 / 文件 / 消息队列类引擎的表一律拒绝 */
function chEngineCheck(inst, ck) {
  for (const r of ck.tables) {
    if (chIsSystem(r.db) || chIsInfo(r.db)) continue;
    const td = tdOf(inst, r.db, r.name); if (!td) continue;
    const eng = String(td.t.eng || '').toLowerCase();
    if (CH_DENY_ENGINES[eng]) throw chErr(`不允许查询 ${r.db}.${r.name}：该表使用 ${td.t.eng} 引擎（${CH_DENY_ENGINES[eng]}）`);
  }
}

/* ---- 结果整形 ---- */
function chTypeName(d) {
  if (d.t) return d.t;
  if (d.chT) return d.chT;
  return ({int: 'Int32', big: 'UInt64', float: 'Float64', dec: 'Decimal(38, 2)', str: 'String', bool: 'Bool', date: 'Date', ts: 'DateTime', arr: 'Array(String)', obj: 'String', uuid: 'UUID', bytes: 'String', enum: 'String'})[d.k] || 'String';
}
function chCell(v, d, tname, st, lim) {
  if (isNull(v)) return null;
  let t = tname; for (let k = 0; k < 3; k++) t = t.replace(/^(Nullable|LowCardinality)\((.*)\)$/, '$2');
  if (v && typeof v === 'object' && !isDate(v) && 'ms' in v && !Array.isArray(v)) return intervalText(v.ms);
  if (/^U?Int(64|128|256)$/.test(t)) return String(isBig(v) ? v : Math.round(toNum(v)));
  if (/^U?Int(8|16|32)$/.test(t)) return Math.round(toNum(v));
  if (/^Float/.test(t)) { const n = toNum(v); return Number.isFinite(n) ? n : (isNaN(n) ? 'nan' : n > 0 ? 'inf' : '-inf'); }
  if (/^Decimal/.test(t)) { const m = /,\s*(\d+)\s*\)/.exec(t); return toNum(v).toFixed(m ? +m[1] : 2); }
  if (/^Bool$/.test(t)) return !!v;
  if (/^Date(32)?$/.test(t)) return isDate(v) ? ymdOf(v.getTime()) : String(v);
  if (/^DateTime64/.test(t)) { const m = /DateTime64\(\s*(\d+)\s*(?:,\s*'([^']+)')?\s*\)/.exec(t), pr = m ? +m[1] : 3; return isDate(v) ? ymdOf(v.getTime()) + 'T' + hmsOf(v.getTime()) + (pr ? '.' + fracOf(v.getTime(), pr).slice(0, pr) : '') + (m && m[2] ? '+08:00' : '') : String(v); }
  if (/^DateTime/.test(t)) { const m = /DateTime\('([^']+)'\)/.exec(t); return isDate(v) ? ymdOf(v.getTime()) + 'T' + hmsOf(v.getTime()) + (m ? '+08:00' : '') : String(v); }
  if (/^(Array|Map|Tuple|Nested|JSON)/.test(t) || (typeof v === 'object' && !isDate(v))) return chCut(jsonSafe(v), st, lim);
  const s = String(v);
  if (/[\x00-\x08\x0b\x0c\x0e-\x1f]/.test(s)) { st.binary.add(d.n || ''); return '\\x' + Array.from(new TextEncoder().encode(s)).map(b => b.toString(16).padStart(2, '0')).join('').slice(0, 2 * CELL_MAX); }
  return chCut(s, st, lim);
}
function chCut(s, st, lim) { const max = lim.cell || CELL_MAX; if (s.length <= max) return s; st.cut.add(1); st.cutN = (st.cutN || 0) + 1; return s.slice(0, max) + `…（已截断，原长 ${new TextEncoder().encode(s).length} 字节）`; }
CH.run = function (inst, curDb, ck, lim) {
  chEngineCheck(inst, ck);
  const maxRows = lim.maxRows, cx = {dia: 'ch', inst, db: curDb || 'default', maxRows, ctes: new Map(), scale: 1};
  let rs;
  try { rs = chExec(ck, cx); } catch (e) { throw wrapSim(e, ck.stmt, 'ch'); }
  const st = {binary: new Set(), cut: new Set()};
  let rows = rs.rows, truncated = false, truncatedBy = '';
  if (rows.length > maxRows) { rows = rows.slice(0, maxRows); truncated = true; truncatedBy = 'rows'; }
  const types = rs.cols.map(c => chTypeName(c.d));
  const out = []; let bytes = 0; const maxBytes = lim.maxBytes || 4 << 20;
  for (const r of rows) {
    const row = r.map((v, i) => chCell(v, Object.assign({n: rs.cols[i].n}, rs.cols[i].d), types[i], st, {}));
    const rb = row.reduce((a, x) => a + (typeof x === 'string' ? x.length : 8), 0);
    if (bytes + rb > maxBytes && out.length) { truncated = true; truncatedBy = 'bytes'; break; }
    bytes += rb; out.push(row);
  }
  const warnings = [], nm = i => `列 ${i}`;
  if (truncated) warnings.push(truncatedBy === 'rows' ? `结果超过 ${maxRows} 行上限，已截断；请加 WHERE / LIMIT 缩小范围` : `结果超过 ${maxBytes >> 20} MB 上限，已截断；请只查询需要的列`);
  for (const c of st.binary) warnings.push(`${c || '某列'}含二进制内容，已以 \\xHH 形式显示`);
  if (st.cutN) warnings.push(`有单元格超过 ${CELL_MAX >> 10} KB，已截断`);
  return {columns: rs.cols.map(c => c.n), types, rows: out, truncated, truncatedBy, warnings, notices: ck.notices.slice(), executed: ck.stmt, stmtKind: ck.kind, synth: rs.synth, scale: rs.scale};
};
function chColsOf(td) { return td.cols; }
function chDdl(td) {
  const t = td.t, full = td.db + '.' + t.n;
  if (t.kind === 'dict') return `CREATE DICTIONARY ${full}\n(\n${t.cols.map(c => `    \`${c.n}\` ${c.t}${c.key === 'PRI' ? '' : ' DEFAULT \'\''}`).join(',\n')}\n)\nPRIMARY KEY ${t.cols[0].n}\nSOURCE(CLICKHOUSE(TABLE 'dim_${t.n.replace(/^dict_/, '')}' DB '${td.db}'))\nLIFETIME(MIN 300 MAX 600)\nLAYOUT(HASHED())`;
  if (t.view) return `CREATE ${t.kind === 'matview' ? 'MATERIALIZED VIEW' : 'VIEW'} ${full}\n(\n${t.cols.map(c => `    \`${c.n}\` ${c.t}`).join(',\n')}\n) ${t.kind === 'matview' ? 'ENGINE = SummingMergeTree\nORDER BY hour\nAS' : 'AS'}\nSELECT ${t.cols.map(c => c.n).join(', ')}\nFROM ${td.db}.fact_orders\nGROUP BY ${t.cols[0].n}`;
  return `CREATE TABLE ${full}\n(\n${t.cols.map(c => `    \`${c.n}\` ${c.t}${c.def ? ' DEFAULT ' + c.def : ''}${c.cmt ? " COMMENT '" + c.cmt + "'" : ''}`).join(',\n')}\n)\nENGINE = ${t.eng}${t.part ? `\nPARTITION BY ${t.part}` : ''}\nORDER BY (${t.sk || t.pk || 'tuple()'})\nSETTINGS index_granularity = 8192${t.c ? `\nCOMMENT '${t.c}'` : ''}`;
}
function chExec(ck, cx) {
  const inst = cx.inst, dbs = SCHEMAS[inst.schema] || [], mk = (names, rows, types) => ({cols: names.map((n, i) => ({n, d: T('str', {t: (types || [])[i] || 'String'})})), rows});
  if (ck.kind === 'select') {
    const ast = new Parser(ck.stmt, 'ch').parseStatement();
    return execQuery(ast, cx);
  }
  if (ck.kind === 'show') {
    const f = ck.info, sub = f.sub, like = f.like != null ? likeRe(f.like, f.ilike) : null;
    const filt = names => names.filter(n => !like || (like.test(n) !== !!f.notLike)).slice(0, f.limit || 1e9);
    if (sub === 'databases' || sub === 'schemas') return mk(['name'], filt(['INFORMATION_SCHEMA', 'default', 'information_schema', ...dbs.map(d => d.db), 'system'].sort()).map(n => [n]));
    if (sub === 'tables' || sub === 'dictionaries') {
      const db = f.db || cx.db;
      if (!dbs.some(d => d.db === db) && !/^(default|system|information_schema)$/i.test(db)) throw new EErr(`ClickHouse 错误 81（库不存在）：Database ${db} does not exist. (UNKNOWN_DATABASE)`, 'engine_error');
      const d = dbs.find(x => x.db === db), list = (d ? d.tables : []).filter(t => sub === 'dictionaries' ? t.kind === 'dict' : t.kind !== 'dict').map(t => t.n);
      return mk(['name'], filt(db.toLowerCase() === 'system' ? [...CH_SYSTEM_OK] : list).map(n => [n]));
    }
    if (sub === 'create') {
      if (f.what === 'database') { if (!dbs.some(d => d.db === f.db) && !/^(default|system)$/i.test(f.db)) throw new EErr(`ClickHouse 错误 81（库不存在）：Database ${f.db} does not exist. (UNKNOWN_DATABASE)`, 'engine_error'); return mk(['statement'], [[`CREATE DATABASE ${f.db}\nENGINE = Atomic`]]); }
      const r = ck.tables[0], td = tdOf(inst, r.db, r.name); if (!td) throw notExistTable(cx, [r.db, r.name]);
      return mk(['statement'], [[chDdl(td)]]);
    }
    const r = ck.tables[0], td = tdOf(inst, r.db, r.name); if (!td) throw notExistTable(cx, [r.db, r.name]);
    if (sub === 'columns' || sub === 'fields') return mk(['field', 'type', 'null', 'key', 'default', 'extra'], filt(td.cols.map(c => c.n)).map(n => { const c = td.colMap[n.toLowerCase()]; return [c.n, c.t, /^Nullable/.test(c.t) ? 'YES' : 'NO', c.key === 'PRI' ? 'PRI' : '', c.def || null, '']; }));
    return mk(['table', 'non_unique', 'key_name', 'seq_in_index', 'column_name', 'index_type'], String(td.t.pk || '').split(',').map(s => s.trim()).filter(Boolean).map((c, i) => [td.n, 0, 'PRIMARY', i + 1, c, 'primary']));
  }
  if (ck.kind === 'describe') {
    const r = ck.tables[0], td = tdOf(inst, r.db, r.name); if (!td) throw notExistTable(cx, [r.db, r.name]);
    return mk(['name', 'type', 'default_type', 'default_expression', 'comment', 'codec_expression', 'ttl_expression'], td.cols.map(c => [c.n, c.t, c.def ? 'DEFAULT' : '', c.def || '', c.cmt || '', '', '']));
  }
  if (ck.kind === 'exists') {
    if (!ck.tables.length) return {cols: [{n: 'result', d: T('int', {t: 'UInt8'})}], rows: [[dbs.some(d => d.db === ck.databases[0]) || /^(default|system|information_schema)$/i.test(ck.databases[0]) ? 1 : 0]]};
    const r = ck.tables[0]; return {cols: [{n: 'result', d: T('int', {t: 'UInt8'})}], rows: [[tdOf(inst, r.db, r.name) || (chIsSystem(r.db) && CH_SYSTEM_OK.has(r.name.toLowerCase())) ? 1 : 0]]};
  }
  /* explain */
  const kindWord = (/^\s*explain\s+(ast|syntax|query\s+tree|plan|pipeline|estimate)/i.exec(ck.stmt) || [0, 'plan'])[1].toLowerCase().replace(/\s+/g, ' ');
  const tabs = ck.tables.filter(t => !chIsSystem(t.db)); for (const r of tabs) if (!tdOf(inst, r.db, r.name)) throw notExistTable(cx, [r.db, r.name]);
  const inner = ck.stmt.replace(/^\s*explain\s+(ast|syntax|query\s+tree|plan|pipeline|estimate)?\s*([a-z_]+\s*=\s*\d+\s*,?\s*)*/i, '');
  const first = tabs[0] ? `${tabs[0].db}.${tabs[0].name}` : 'system.one';
  let lines;
  if (kindWord === 'ast') lines = ['SelectWithUnionQuery (children 1)', ' ExpressionList (children 1)', '  SelectQuery (children 2)', '   ExpressionList (children 1)', '    Asterisk', `   TablesInSelectQuery (children 1)`, `    TablesInSelectQueryElement (children 1)`, `     TableExpression (children 1)`, `      TableIdentifier ${first}`];
  else if (kindWord === 'syntax') lines = [clip(inner.replace(/\bFROM\s+(\w+)\b/i, (m, t) => `FROM ${tabs[0] ? tabs[0].db + '.' : ''}${t}`), 400)];
  else if (kindWord === 'estimate') lines = null;
  else if (kindWord === 'pipeline') lines = ['(Expression)', 'ExpressionTransform × 4', '  (ReadFromMergeTree)', '  MergeTreeSelect(pool: ReadPool, algorithm: Thread) × 4 0 → 1'];
  else lines = ['Expression ((Projection + Before ORDER BY))', `  ReadFromMergeTree (${first})`];
  if (lines === null) { const td = tabs[0] && tdOf(inst, tabs[0].db, tabs[0].name); return mk(['database', 'table', 'parts', 'rows', 'marks'], [[tabs[0] ? tabs[0].db : 'system', tabs[0] ? tabs[0].name : 'one', 4, td ? Math.round(td.total * 0.012) : 1, td ? Math.round(td.total * 0.012 / 8192) : 1]]); }
  return mk(['explain'], lines.map(l => [l]));
}

/* ============================================================================
   七、MongoDB：shell 风格语句解析 + 检查（internal/engine/mongo/{parser,check}.go 的移植，拒绝原因原文）+ 受限执行 + 结果整形
   ========================================================================== */
const MG = {};
const mgErr = msg => new EErr(msg, 'bad_query');
const MG_MAX_STMT = 64 << 10, MG_MAX_NEST = 64;
/* 值的内部表示：普通 JS 值 + 带 $$t 标记的特殊值 */
const mgS = (t, o) => Object.assign({$$t: t}, o);
const isSp = v => v && typeof v === 'object' && v.$$t !== undefined;
const oidHex = (i, ts, coll) => pad(Math.floor(ts / 1000).toString(16), 8) + pad(i.toString(16), 8) + pad((h32(coll + i) >>> 0).toString(16), 8);
const mkOid = hex => mgS('oid', {hex});
const mkLong = n => mgS('long', {n});
const mkDec = s => mgS('dec', {s: String(s)});
const mkDate = ms => mgS('date', {ms});
function unwrap(v) {
  if (!isSp(v)) return v;
  switch (v.$$t) { case 'oid': return 'oid:' + v.hex; case 'long': return Number(v.n); case 'int': return v.n; case 'dec': return Number(v.s); case 'date': return v.ms; case 'uuid': return v.s; case 're': return v; default: return JSON.stringify(v); }
}
function mgLex(src) {
  const toks = [], n = src.length; let i = 0, line = 1, col = 1;
  const posOf = off => { let l = 1, c = 1; for (let k = 0; k < off && k < src.length; k++) { if (src[k] === '\n') { l++; c = 1; } else c++; } return [l, c]; };
  const E = (off, msg) => { const [l, c] = posOf(off); return mgErr(`第 ${l} 行第 ${c} 列：${msg}`); };
  while (i < n) {
    const c = src[i];
    if (/\s/.test(c)) { i++; continue; }
    if (c === '/' && src[i + 1] === '/') { while (i < n && src[i] !== '\n') i++; continue; }
    if (c === '/' && src[i + 1] === '*') { const j = src.indexOf('*/', i + 2); if (j < 0) throw E(i, '块注释没有结束（缺少 */）'); i = j + 2; continue; }
    if (c === '"' || c === "'") {
      let j = i + 1, s = '';
      while (j < n && src[j] !== c) {
        if (src[j] === '\\' && j + 1 < n) { const x = src[j + 1]; s += x === 'n' ? '\n' : x === 't' ? '\t' : x === 'r' ? '\r' : x === 'u' && /^[0-9a-fA-F]{4}$/.test(src.slice(j + 2, j + 6)) ? String.fromCharCode(parseInt(src.slice(j + 2, j + 6), 16)) : x; j += x === 'u' ? 6 : 2; continue; }
        s += src[j++];
      }
      if (j >= n) throw E(i, '字符串没有结束（缺少 ' + c + '）');
      toks.push({k: 'str', v: s, off: i, text: src.slice(i, j + 1)}); i = j + 1; continue;
    }
    if (/[0-9]/.test(c) || (c === '.' && /[0-9]/.test(src[i + 1] || ''))) {
      const m = /^(0[xX][0-9a-fA-F]+|\d*\.?\d+(?:[eE][+-]?\d+)?|\d+\.)/.exec(src.slice(i));
      toks.push({k: 'num', v: Number(m[0]), off: i, text: m[0], isInt: /^(0[xX][0-9a-fA-F]+|\d+)$/.test(m[0])}); i += m[0].length; continue;
    }
    if (/[A-Za-z_$]/.test(c)) { let j = i + 1; while (j < n && /[A-Za-z0-9_$]/.test(src[j])) j++; toks.push({k: 'id', v: src.slice(i, j), off: i, text: src.slice(i, j)}); i = j; continue; }
    if ('{}[](),:.;-+'.includes(c)) { toks.push({k: 'p', v: c, off: i, text: c}); i++; continue; }
    if (c === '/') {   // 正则字面量（只在值的位置出现；这里先按“可能的正则”切出来，解析器决定是否采用）
      let j = i + 1, inCls = false;
      while (j < n && (src[j] !== '/' || inCls) && src[j] !== '\n') { if (src[j] === '\\') j++; else if (src[j] === '[') inCls = true; else if (src[j] === ']') inCls = false; j++; }
      if (j < n && src[j] === '/') { let k = j + 1; while (k < n && /[a-z]/.test(src[k])) k++; toks.push({k: 're', v: src.slice(i + 1, j), flags: src.slice(j + 1, k), off: i, text: src.slice(i, k)}); i = k; continue; }
    }
    throw E(i, `无法识别的字符“${c}”`);
  }
  return {toks, E};
}
class MgParser {
  constructor(src) { this.src = src; const r = mgLex(src); this.toks = r.toks; this.E = r.E; this.i = 0; }
  pk(o) { return this.toks[this.i + (o || 0)]; }
  desc(t) { return t ? `“${t.text}”` : '语句结尾'; }
  fail(t, msg) { return this.E(t ? t.off : this.src.length, msg); }
  eatP(c) { const t = this.pk(); if (t && t.k === 'p' && t.v === c) { this.i++; return true; } return false; }
  expP(c) { const t = this.pk(); if (t && t.k === 'p' && t.v === c) { this.i++; return t; } throw this.fail(t, `此处应为“${c}”，实际是${this.desc(t)}`); }
  parseStatement() {
    if (!this.toks.length) throw mgErr('语句为空');
    const t = this.pk();
    if (!(t.k === 'id' && t.v === 'db')) throw this.fail(t, '语句必须以 db. 开头，例如 db.users.find({})' + (t.v === 'use' ? '；切换库请用页面顶部的库下拉' : t.v === 'show' ? '；查看集合请执行 db.getCollectionNames()，库列表在左侧库表树里' : (t.v === 'rs' || t.v === 'sh') ? '；副本集 / 分片管理命令不支持' : ''));
    this.i++;
    const st = {coll: '', hasColl: false, calls: []};
    let nt = this.pk();
    if (nt && nt.k === 'p' && nt.v === '[') { this.i++; const s = this.pk(); if (!s || s.k !== 'str') throw this.fail(s, 'db[...] 里应为带引号的集合名'); this.i++; this.expP(']'); st.coll = s.v; st.hasColl = true; if (!this.eatP('.')) throw this.fail(this.pk(), '集合后面需要跟方法，例如 .find({})'); }
    else {
      this.expP('.');
      const parts = [];
      for (;;) {
        nt = this.pk();
        if (!nt || (nt.k !== 'id' && nt.k !== 'str')) throw this.fail(nt, `“.”后面应为集合名或方法名，实际是${this.desc(nt)}`);
        const after = this.pk(1);
        if (nt.k === 'id' && after && after.k === 'p' && after.v === '(') {
          if (parts.length) { st.coll = parts.join('.'); st.hasColl = true; break; }
          if (nt.v === 'getCollection') {
            this.i += 2; const s = this.pk(); if (!s || s.k !== 'str') throw this.fail(s, 'getCollection 需要一个字符串参数（集合名）'); this.i++; this.expP(')');
            st.coll = s.v; st.hasColl = true; if (!this.eatP('.')) throw this.fail(this.pk(), '集合后面需要跟方法，例如 .find({})'); break;
          }
          break;   // db 级方法
        }
        parts.push(nt.v); this.i++;
        if (!this.eatP('.')) throw this.fail(this.pk(), '集合后面需要跟方法，例如 .find({})');
      }
    }
    for (;;) {
      const m = this.pk();
      if (!m || m.k !== 'id') throw this.fail(m, `“.”后面应为方法名，实际是${this.desc(m)}`);
      this.i++;
      if (!(this.pk() && this.pk().k === 'p' && this.pk().v === '(')) throw this.fail(this.pk(), `${m.v} 后面缺少括号，应为 .${m.v}(...)`);
      this.i++;
      const args = [];
      if (!this.eatP(')')) { for (;;) { args.push(this.parseValue(0)); if (this.eatP(',')) continue; this.expP(')'); break; } }
      st.calls.push({name: m.v, args, off: m.off});
      if (st.calls.length === 1) { const dn = mgMethodDenied(st.calls[0]); if (dn) throw dn; }
      if (this.eatP('.')) continue;
      break;
    }
    while (this.eatP(';')) { /* 末尾分号 */ }
    const rest = this.pk();
    if (rest) { if (rest.k === 'id' && rest.v === 'db') throw this.fail(rest, '检测到多条语句，在线查询一次只能执行一条；请只保留一条（或选中其中一条再运行）'); throw this.fail(rest, `语句在这里应当结束，多余内容：${this.desc(rest)}（一次只能执行一条语句）`); }
    return st;
  }
  parseValue(depth) {
    if (depth > MG_MAX_NEST) throw this.fail(this.pk(), `嵌套层数超过 ${MG_MAX_NEST} 层`);
    const t = this.pk(); if (!t) throw this.fail(t, '此处应为一个值，实际是语句结尾');
    if (t.k === 'p' && t.v === '{') {
      this.i++; const o = {};
      if (this.eatP('}')) return o;
      for (;;) {
        const kt = this.pk(); if (!kt || !(kt.k === 'id' || kt.k === 'str' || kt.k === 'num')) throw this.fail(kt, `文档的键应为标识符或字符串，实际是${this.desc(kt)}`);
        this.i++; this.expP(':');
        o[kt.k === 'num' ? String(kt.v) : kt.v] = this.parseValue(depth + 1);
        if (this.eatP(',')) { if (this.pk() && this.pk().k === 'p' && this.pk().v === '}') { this.i++; break; } continue; }
        const c = this.pk(); if (c && c.k === 'p' && c.v === '}') { this.i++; break; }
        throw this.fail(c, `文档的键值对之间应为“,”或结束的“}”，实际是${this.desc(c)}`);
      }
      return o;
    }
    if (t.k === 'p' && t.v === '[') {
      this.i++; const a = [];
      if (this.eatP(']')) return a;
      for (;;) {
        a.push(this.parseValue(depth + 1));
        if (this.eatP(',')) { if (this.pk() && this.pk().k === 'p' && this.pk().v === ']') { this.i++; break; } continue; }
        const c = this.pk(); if (c && c.k === 'p' && c.v === ']') { this.i++; break; }
        throw this.fail(c, `数组元素之间应为“,”或结束的“]”，实际是${this.desc(c)}`);
      }
      return a;
    }
    if (t.k === 'str') { this.i++; return t.v; }
    if (t.k === 'num') { this.i++; return t.isInt && Math.abs(t.v) > 2147483647 ? (Math.abs(t.v) > MAX_SAFE ? mkLong(BigInt(t.text)) : mkLong(t.v)) : t.v; }
    if (t.k === 'p' && (t.v === '-' || t.v === '+')) { this.i++; const n = this.pk(); if (!n || (n.k !== 'num' && !(n.k === 'id' && /^(Infinity|NaN)$/.test(n.v)))) throw this.fail(n, `“${t.v}”后面应为数字`); const v = this.parseValue(depth); return t.v === '-' ? (isSp(v) ? mkLong(-Number(v.n)) : -v) : v; }
    if (t.k === 're') { this.i++; for (const f of t.flags) if (!'imxsul'.includes(f)) throw this.fail(t, `不支持的正则选项 "${f}"（只支持 i m x s u l）`); return mgS('re', {p: t.v, f: t.flags}); }
    if (t.k === 'id') {
      if (t.v === 'true') { this.i++; return true; } if (t.v === 'false') { this.i++; return false; } if (t.v === 'null' || t.v === 'undefined') { this.i++; return null; }
      if (t.v === 'NaN') { this.i++; return NaN; } if (t.v === 'Infinity') { this.i++; return Infinity; }
      let isNew = false; if (t.v === 'new') { this.i++; isNew = true; }
      const nt = isNew ? this.pk() : t;
      if (!nt || nt.k !== 'id') throw this.fail(nt, 'new 后面应为构造函数调用，例如 new Date(...)');
      if (nt.v === 'function' || nt.v === 'this') throw this.fail(nt, `不支持 JavaScript 函数 / 表达式（${nt.v}）`);
      const nx = this.pk(1);
      if (nx && nx.k === 'p' && nx.v === '(') return this.parseCtor(nt, depth);
      if (nt.v === 'function' || nt.v === 'this') throw this.fail(nt, `不支持 JavaScript 函数 / 表达式（${nt.v}）`);
      if (nt.v === 'MinKey') { this.i++; return mgS('min'); } if (nt.v === 'MaxKey') { this.i++; return mgS('max'); }
      throw this.fail(nt, `无法识别的标识符 ${nt.v}（字符串请加引号）`);
    }
    throw this.fail(t, `此处应为一个值，实际是${this.desc(t)}`);
  }
  parseCtor(nameTok, depth) {
    const name = nameTok.v; this.i += 2;
    const args = [];
    if (!this.eatP(')')) { for (;;) { args.push(this.parseValue(depth + 1)); if (this.eatP(',')) continue; this.expP(')'); break; } }
    const need = (lo, hi) => { if (args.length < lo || args.length > hi) throw this.fail(nameTok, `${name} 的参数个数不对`); };
    const str0 = () => { if (typeof args[0] !== 'string') throw this.fail(nameTok, `${name}：参数必须是字符串`); return args[0]; };
    switch (name) {
      case 'ObjectId': { need(0, 1); if (!args.length) return mkOid(oidHex(Math.floor(NOW_MS) % 1e8, NOW_MS, 'new')); const s = str0(); if (!/^[0-9a-fA-F]{24}$/.test(s)) throw this.fail(nameTok, `${name}：需要 24 位十六进制字符串`); return mkOid(s.toLowerCase()); }
      case 'ISODate': case 'Date': {
        if (!args.length) return mkDate(NOW_MS);
        if (typeof args[0] === 'number') return mkDate(args[0]);
        const ms = parseTs(/Z$|[+-]\d{2}:?\d{2}$/.test(String(args[0])) ? args[0] : String(args[0]) + (/\d:\d/.test(String(args[0])) ? 'Z' : 'Z'));
        const ms2 = /^\d{4}-\d{2}-\d{2}$/.test(String(args[0])) ? Date.UTC(+args[0].slice(0, 4), +args[0].slice(5, 7) - 1, +args[0].slice(8, 10)) : ms;
        if (isNaN(ms2)) throw this.fail(nameTok, `${name}：日期格式无法识别（支持 ISO-8601）`);
        return mkDate(ms2);
      }
      case 'NumberLong': case 'Long': { need(1, 1); const n = typeof args[0] === 'string' ? BigInt(args[0]) : BigInt(Math.trunc(Number(isSp(args[0]) ? args[0].n : args[0]))); return mkLong(n); }
      case 'NumberInt': case 'Int32': need(1, 1); return mgS('int', {n: Math.trunc(Number(args[0]))});
      case 'NumberDouble': case 'Double': need(1, 1); return Number(args[0]);
      case 'NumberDecimal': case 'Decimal128': need(1, 1); return mkDec(args[0]);
      case 'Timestamp': need(2, 2); return mgS('ts', {t: args[0], i: args[1]});
      case 'BinData': need(2, 2); return mgS('bin', {sub: args[0], hex: ''});
      case 'HexData': need(2, 2); return mgS('bin', {sub: args[0], hex: String(args[1])});
      case 'UUID': need(1, 1); return mgS('uuid', {s: str0()});
      case 'RegExp': need(1, 2); return mgS('re', {p: str0(), f: args[1] || ''});
      case 'MinKey': return mgS('min'); case 'MaxKey': return mgS('max');
    }
    throw this.fail(nameTok, `不支持的函数 / 构造函数 ${name}（只支持 ObjectId、ISODate、Date、NumberLong、NumberInt、NumberDouble、NumberDecimal、Timestamp、BinData、HexData、UUID、RegExp、MinKey、MaxKey）`);
  }
}

const MG_WRITE = setOf('insert', 'insertOne', 'insertMany', 'update', 'updateOne', 'updateMany', 'replaceOne', 'remove', 'deleteOne', 'deleteMany', 'save', 'findAndModify', 'findOneAndUpdate', 'findOneAndReplace', 'findOneAndDelete', 'bulkWrite', 'drop', 'dropIndex', 'dropIndexes',
  'createIndex', 'createIndexes', 'ensureIndex', 'reIndex', 'renameCollection', 'createCollection', 'dropDatabase', 'createView', 'convertToCapped', 'compact', 'validate', 'mapReduce', 'initializeUnorderedBulkOp', 'initializeOrderedBulkOp', 'copyTo', 'cloneCollection', 'dropCollection',
  'createUser', 'dropUser', 'updateUser', 'grantRolesToUser', 'revokeRolesFromUser', 'createRole', 'dropRole', 'setProfilingLevel', 'fsyncLock', 'fsyncUnlock', 'shutdownServer', 'killOp', 'killCursors', 'setLogLevel', 'setSlaveOk', 'setSecondaryOk', 'setReadPref');
const MG_EXEC = setOf('runCommand', 'adminCommand', 'eval', 'getSiblingDB', 'getMongo', 'auth', 'logout', 'getProfilingStatus', 'currentOp', 'serverStatus', 'hostInfo', 'getUsers', 'getRoles', 'getReplicationInfo', 'printShardingStatus', 'watch', 'explain', 'getPlanCache', 'getShardDistribution');
const MG_NOOP = setOf('pretty', 'toArray', 'batchSize', 'noCursorTimeout', 'allowDiskUse');
const MG_DENY_STAGE = {$out: '会写入集合', $merge: '会写入集合', $collStats: '请改用 db.<集合>.stats()', $indexStats: '请改用 db.<集合>.getIndexes()', $planCacheStats: '泄露执行计划缓存', $currentOp: '会列出服务端正在执行的操作（含其他人的语句）', $listSessions: '会列出服务端会话', $listLocalSessions: '会列出服务端会话',
  $listSampledQueries: '会读取其他人采样的查询', $listSearchIndexes: '属于管理操作', $changeStream: '是长连接监听，会占住游标', $changeStreamSplitLargeEvent: '是长连接监听，会占住游标', $search: 'Atlas Search 不支持', $searchMeta: 'Atlas Search 不支持', $vectorSearch: 'Atlas Search 不支持',
  $documents: '不支持无集合的 aggregate', $geoNear: '暂不支持', $queryStats: '会读取其他人的查询统计', $shardedDataDistribution: '属于管理操作'};
const MG_STAGES = setOf('$match', '$project', '$group', '$sort', '$limit', '$skip', '$unwind', '$count', '$addFields', '$set', '$unset', '$replaceRoot', '$replaceWith', '$lookup', '$facet', '$bucket', '$bucketAuto', '$sortByCount', '$sample', '$densify', '$fill', '$setWindowFields', '$redact', '$graphLookup', '$unionWith');
const MG_DENY_OP = {$where: '会在服务端执行 JavaScript', $function: '会在服务端执行 JavaScript', $accumulator: '会在服务端执行 JavaScript', $out: '会写入集合', $merge: '会写入集合', $eval: '会在服务端执行 JavaScript'};
const MG_SUPPORTED = '支持：find / findOne / countDocuments / count / estimatedDocumentCount / distinct / aggregate / getIndexes / stats，以及 db.getCollectionNames() / db.stats()';
const MG_RESERVED = setOf('find', 'findOne', 'count', 'countDocuments', 'aggregate', 'distinct', 'getIndexes', 'stats', 'estimatedDocumentCount', 'getCollection', 'getCollectionNames', 'getName', 'getDB', 'help', 'length', 'toString', 'constructor', 'valueOf', 'hasOwnProperty');
MG.validDb = function (name) {
  if (!name) throw mgErr('请先选择数据库');
  if (name.length > 63 || /[\/\\. "$*<>:|?\0]/.test(name)) throw mgErr(`库名不合法：${JSON.stringify(name)}`);
  if (['admin', 'local', 'config'].includes(name)) throw mgErr(`在线查询不允许访问系统库 ${name}`);
};
function mgValidColl(name) {
  if (name === '') throw mgErr('集合名不能为空');
  if (name.length > 255 || /[$\0]/.test(name)) throw mgErr(`集合名不合法：${JSON.stringify(name)}`);
  if (name.startsWith('system.')) throw mgErr(`不允许访问系统集合 ${name}`);
  if (name.startsWith('.') || name.endsWith('.') || name.includes('..')) throw mgErr(`集合名不合法：${JSON.stringify(name)}`);
}
function mgWalk(v, depth) {
  if (depth > 100) throw mgErr('文档嵌套层数过深');
  if (isSp(v)) return;
  if (Array.isArray(v)) { for (const x of v) mgWalk(x, depth + 1); return; }
  if (v && typeof v === 'object') for (const [k, x] of Object.entries(v)) {
    if (MG_DENY_OP[k]) throw mgErr(`${k} 已禁止：${MG_DENY_OP[k]}`);
    if (k.startsWith('$_')) throw mgErr(`内部操作符 ${k} 已禁止`);
    if (k === '$code' || k === '$scope') throw mgErr('不允许在查询里使用 JavaScript 代码');
    mgWalk(x, depth + 1);
  }
}
function mgMethodDenied(c) {
  if (MG_WRITE.has(c.name)) return mgErr(`${c.name} 是写操作 / 管理操作，在线查询只读，已禁止`);
  if (MG_EXEC.has(c.name)) return mgErr(`${c.name} 不在允许的方法清单里，已禁止${c.name === 'getSiblingDB' ? '；切换库请用页面顶部的库下拉' : c.name === 'explain' ? '（explain 会实际执行查询）' : ''}`);
  return null;
}
const isDoc = v => v && typeof v === 'object' && !Array.isArray(v) && !isSp(v);
const isIntArg = a => typeof a === 'number' && Number.isInteger(a) || (isSp(a) && (a.$$t === 'long' || a.$$t === 'int'));
const intVal = a => isSp(a) ? Number(a.n || a.n === 0 ? a.n : 0) : a;

/* 对外：检查一条语句。opts.maxRows 给出时缺省 / 超限的 limit 被收窄（并给提示） */
MG.check = function (text, opts) {
  opts = opts || {};
  text = String(text).trim();
  if (text === '') throw mgErr('语句为空');
  if (text.length > MG_MAX_STMT) throw mgErr(`语句过长（上限 ${MG_MAX_STMT >> 10} KB）`);
  const st = new MgParser(text).parseStatement();
  const c = {kind: '', coll: '', collections: [], dbLevel: false, notices: [], filter: null, projection: null, sort: null, skip: 0, limit: 0, field: '', pipeline: null, maxRows: opts.maxRows || 0, rawText: text};
  const notice = m => { if (!c.notices.includes(m)) c.notices.push(m); };
  if (st.hasColl) { mgValidColl(st.coll); c.coll = st.coll; }
  if (!st.calls.length) throw mgErr(`缺少方法调用，例如 db.${st.coll || '<集合>'}.find({})`);
  const head = st.calls[0], chain = st.calls.slice(1);
  const doc = (v, what) => { if (!isDoc(v)) throw mgErr(`${what}必须是文档，例如 {"status": "ok"}`); mgWalk(v, 0); return v; };
  const extra = new Set();
  const pipeline = (arr, depth) => {
    if (depth > 4) throw mgErr('pipeline 嵌套层数超过 4 层');
    if (arr.length > 50) throw mgErr('pipeline 的 stage 数超过 50 个');
    return arr.map((s, i) => {
      if (!isDoc(s) || Object.keys(s).length !== 1) throw mgErr(`pipeline 第 ${i + 1} 个 stage 必须是只含一个 $stage 键的文档`);
      const name = Object.keys(s)[0];
      if (MG_DENY_STAGE[name]) throw mgErr(`pipeline 第 ${i + 1} 个 stage ${name} 已禁止：${MG_DENY_STAGE[name]}`);
      if (!MG_STAGES.has(name)) throw mgErr(`pipeline 第 ${i + 1} 个 stage ${name} 不在允许的清单里（允许：${[...MG_STAGES].sort().join(' ')}）`);
      mgWalk(s[name], 0);
      const val = s[name];
      const fromColl = (v, stage) => { if (typeof v !== 'string') throw mgErr(`${stage} 的 from 必须是同库的集合名字符串（不支持跨库 / 对象写法）`); try { mgValidColl(v); } catch (e) { throw mgErr(`${stage} 的 from：${e.message}`); } extra.add(v); };
      const sub = (v, what) => { if (!Array.isArray(v)) throw mgErr(`${what} 的 pipeline 必须是数组`); return pipeline(v, depth + 1); };
      if (name === '$facet') { if (!isDoc(val)) throw mgErr('$facet 的参数必须是文档（名称 → pipeline）'); const o = {}; for (const [k, p] of Object.entries(val)) o[k] = sub(p, '$facet.' + k); return {[name]: o}; }
      if (name === '$lookup') {
        if (!isDoc(val)) throw mgErr('$lookup 的参数必须是文档'); const o = {}; let hasFrom = false;
        for (const [k, v] of Object.entries(val)) {
          if (k === 'from') { hasFrom = true; fromColl(v, '$lookup'); o[k] = v; } else if (k === 'pipeline') o[k] = sub(v, '$lookup'); else if (['localField', 'foreignField', 'as', 'let'].includes(k)) o[k] = v; else throw mgErr(`$lookup 不支持选项 ${k}`);
        }
        if (!hasFrom) throw mgErr('$lookup 必须指定 from（同库集合名）'); return {[name]: o};
      }
      if (name === '$graphLookup') {
        if (!isDoc(val)) throw mgErr('$graphLookup 的参数必须是文档'); const o = {}; let hasFrom = false, hasDepth = false;
        for (const [k, v] of Object.entries(val)) {
          if (k === 'from') { hasFrom = true; fromColl(v, '$graphLookup'); o[k] = v; }
          else if (k === 'maxDepth') { hasDepth = true; const n = intVal(v); if (!(Number.isInteger(n) && n >= 0)) throw mgErr('$graphLookup 的 maxDepth 必须是非负整数'); if (n > 10) { notice(`$graphLookup 的 maxDepth ${n} 超过上限，已改为 10`); o[k] = 10; } else o[k] = n; }
          else if (['startWith', 'connectFromField', 'connectToField', 'as', 'depthField', 'restrictSearchWithMatch'].includes(k)) o[k] = v; else throw mgErr(`$graphLookup 不支持选项 ${k}`);
        }
        if (!hasFrom) throw mgErr('$graphLookup 必须指定 from（同库集合名）');
        if (!hasDepth) { o.maxDepth = 10; notice('$graphLookup 未指定 maxDepth，已补 10（递归深度上限）'); }
        return {[name]: o};
      }
      if (name === '$unionWith') {
        if (typeof val === 'string') { fromColl(val, '$unionWith'); return s; }
        if (isDoc(val)) { const o = {}; let has = false; for (const [k, v] of Object.entries(val)) { if (k === 'coll') { has = true; fromColl(v, '$unionWith'); o[k] = v; } else if (k === 'pipeline') o[k] = sub(v, '$unionWith'); else throw mgErr(`$unionWith 不支持选项 ${k}`); } if (!has) throw mgErr('$unionWith 必须指定 coll（同库集合名；不支持 $documents）'); return {[name]: o}; }
        throw mgErr('$unionWith 的参数必须是集合名字符串或 {coll, pipeline} 文档');
      }
      return s;
    });
  };
  if (!st.hasColl) {
    const dn = mgMethodDenied(head); if (dn) throw dn;
    if (chain.length) throw mgErr(`db.${head.name}() 后面不能再接方法`);
    if (head.name === 'getCollectionNames') { if (head.args.length) throw mgErr('getCollectionNames 不接受参数'); c.kind = 'listCollections'; }
    else if (head.name === 'stats') { if (head.args.length) { if (head.args.length !== 1 || typeof head.args[0] !== 'number') throw mgErr('db.stats() 的参数应为数字（scale）'); notice('db.stats() 的 scale 参数已忽略，统一按字节返回'); } c.kind = 'dbStats'; }
    else throw mgErr(`不支持 db.${head.name}()。${MG_SUPPORTED}`);
  } else {
    const dn = mgMethodDenied(head); if (dn) throw dn;
    switch (head.name) {
      case 'find': case 'findOne': {
        if (head.args.length > 2) throw mgErr(`${head.name} 最多两个参数（过滤条件, 投影）`);
        c.kind = 'find';
        if (head.args.length >= 1) c.filter = doc(head.args[0], '过滤条件');
        if (head.args.length === 2) c.projection = doc(head.args[1], '投影');
        if (head.name === 'findOne') { if (chain.length) throw mgErr('findOne() 后面不能再接方法'); c.limit = 1; break; }
        const seen = new Set();
        chain.forEach((m, i) => {
          const d2 = mgMethodDenied(m); if (d2) throw d2;
          if (seen.has(m.name) && m.name !== 'pretty' && m.name !== 'toArray') throw mgErr(`游标方法 ${m.name} 重复出现`);
          seen.add(m.name);
          switch (m.name) {
            case 'sort': if (m.args.length !== 1) throw mgErr('sort() 需要一个文档参数，例如 sort({"_id": -1})'); c.sort = doc(m.args[0], 'sort 的参数'); break;
            case 'limit': case 'skip': { if (m.args.length !== 1 || !isIntArg(m.args[0]) || intVal(m.args[0]) < 0 || intVal(m.args[0]) > 2 ** 40) throw mgErr(`${m.name}() 的参数必须是非负整数`); if (m.name === 'limit') c.limit = intVal(m.args[0]); else c.skip = intVal(m.args[0]); break; }
            case 'count': case 'size': case 'itcount': if (i !== chain.length - 1) throw mgErr(`${m.name}() 必须放在链的最后`); c.kind = 'count'; if (c.sort || c.skip > 0 || c.limit > 0) notice(`${m.name}() 统计满足过滤条件的全部文档，链上的 sort / skip / limit 被忽略`); c.sort = null; c.skip = 0; c.limit = 0; c.projection = null; break;
            case 'maxTimeMS': notice('maxTimeMS 已忽略，执行时间上限由系统统一设置'); break;
            case 'hint': case 'collation': case 'comment': case 'min': case 'max': case 'returnKey': case 'showRecordId': case 'readPref': case 'readConcern': case 'tailable': case 'addOption': case 'next': case 'forEach': case 'map': throw mgErr(`游标方法 ${m.name} 不支持`);
            default: if (MG_NOOP.has(m.name)) { if (m.name === 'allowDiskUse') notice('allowDiskUse 已忽略：在线查询不允许使用磁盘临时文件'); break; } throw mgErr(`不支持的游标方法 ${m.name}（支持 sort / limit / skip / count / pretty / toArray）`);
          }
        });
        break;
      }
      case 'countDocuments': case 'count':
        if (chain.length) throw mgErr(`${head.name}() 后面不能再接方法`);
        if (head.args.length > 1) throw mgErr(head.name === 'countDocuments' ? 'countDocuments 只支持一个过滤条件参数（limit / skip 等选项请改用 aggregate 的 $skip / $limit / $count）' : 'count 只支持一个过滤条件参数');
        c.kind = 'count'; if (head.args.length === 1) c.filter = doc(head.args[0], '过滤条件'); break;
      case 'estimatedDocumentCount': if (chain.length) throw mgErr('estimatedDocumentCount() 后面不能再接方法'); if (head.args.length) throw mgErr('estimatedDocumentCount 不接受参数'); c.kind = 'estimated'; break;
      case 'distinct': {
        if (chain.length) throw mgErr('distinct() 后面不能再接方法');
        if (head.args.length < 1 || head.args.length > 2 || typeof head.args[0] !== 'string' || head.args[0] === '') throw mgErr('distinct 的第一个参数必须是字段名字符串，例如 distinct("status", {...})');
        c.kind = 'distinct'; c.field = head.args[0]; if (head.args.length === 2) c.filter = doc(head.args[1], '过滤条件'); break;
      }
      case 'aggregate': {
        for (const m of chain) { const d2 = mgMethodDenied(m); if (d2) throw d2; if (!MG_NOOP.has(m.name)) throw mgErr(`aggregate() 后面只能接 toArray() / pretty()，不支持 ${m.name}`); }
        c.kind = 'aggregate';
        const args = head.args; if (!args.length) throw mgErr('aggregate 需要 pipeline 数组，例如 aggregate([{"$match": {...}}])');
        let arr;
        if (Array.isArray(args[0])) {
          arr = args[0]; if (args.length > 2) throw mgErr('aggregate 最多两个参数（pipeline, 选项）');
          if (args.length === 2) {
            if (!isDoc(args[1])) throw mgErr('aggregate 的第二个参数（选项）必须是文档');
            for (const k of Object.keys(args[1])) { if (!['allowDiskUse', 'batchSize', 'cursor', 'maxTimeMS'].includes(k)) throw mgErr(`aggregate 选项 ${k} 不支持（只接受 allowDiskUse / batchSize / cursor / maxTimeMS，其中 allowDiskUse 与 maxTimeMS 会被忽略）`); if (k === 'allowDiskUse') notice('allowDiskUse 已忽略：在线查询不允许使用磁盘临时文件'); if (k === 'maxTimeMS') notice('maxTimeMS 已忽略，执行时间上限由系统统一设置'); }
          }
        } else { if (!args.every(isDoc)) throw mgErr('aggregate 的参数必须是 pipeline 数组，或依次列出的 stage 文档'); arr = args; }
        c.pipeline = pipeline(arr, 0); break;
      }
      case 'getIndexes': if (chain.length || head.args.length) throw mgErr('getIndexes() 不接受参数，也不能再接方法'); c.kind = 'listIndexes'; break;
      case 'stats': if (chain.length) throw mgErr('stats() 后面不能再接方法'); if (head.args.length) notice('stats() 的参数已忽略，统一按字节返回'); c.kind = 'collStats'; break;
      default: throw mgErr(`不支持 ${head.name}。${MG_SUPPORTED}`);
    }
  }
  const set = new Set(); if (c.coll) set.add(c.coll); for (const x of extra) set.add(x);
  c.collections = [...set].sort(); c.dbLevel = c.collections.length === 0;
  /* 行数上限：缺省补 / 收窄超限的 limit（提示） */
  if (c.kind === 'find' && c.maxRows > 0) {
    if (!c.limit) notice(`未指定 limit，结果最多返回 ${c.maxRows} 条（可用 skip / limit 分页）`);
    else if (c.limit > c.maxRows) { notice(`limit ${c.limit} 超过上限，已收窄为 ${c.maxRows}`); c.limit = c.maxRows; }
  }
  c.statement = mgRender(c);
  return c;
};
/* 规范化文本（Extended JSON，relaxed） */
function extJ(v) {
  if (isSp(v)) {
    switch (v.$$t) {
      case 'oid': return `{"$oid":"${v.hex}"}`; case 'date': return `{"$date":"${new Date(v.ms).toISOString()}"}`; case 'long': return String(v.n); case 'int': return String(v.n); case 'dec': return `{"$numberDecimal":"${v.s}"}`;
      case 're': return `{"$regularExpression":{"pattern":${JSON.stringify(v.p)},"options":${JSON.stringify(v.f)}}}`; case 'ts': return `{"$timestamp":{"t":${v.t},"i":${v.i}}}`; case 'min': return '{"$minKey":1}'; case 'max': return '{"$maxKey":1}';
      case 'uuid': return `{"$uuid":"${v.s}"}`; case 'bin': return `{"$binary":{"base64":"","subType":"${pad(v.sub, 2)}"}}`;
    }
  }
  if (Array.isArray(v)) return '[' + v.map(extJ).join(',') + ']';
  if (v && typeof v === 'object') return '{' + Object.entries(v).map(([k, x]) => JSON.stringify(k) + ':' + extJ(x)).join(',') + '}';
  if (typeof v === 'number') return Number.isFinite(v) ? String(v) : `{"$numberDouble":"${isNaN(v) ? 'NaN' : v > 0 ? 'Infinity' : '-Infinity'}"}`;
  return JSON.stringify(v);
}
function mgRender(c) {
  if (c.kind === 'listCollections') return 'db.getCollectionNames()';
  if (c.kind === 'dbStats') return 'db.stats()';
  const base = /^[A-Za-z_][A-Za-z0-9_]*$/.test(c.coll) && !MG_RESERVED.has(c.coll) ? 'db.' + c.coll : 'db.getCollection(' + JSON.stringify(c.coll) + ')';
  switch (c.kind) {
    case 'listIndexes': return base + '.getIndexes()'; case 'collStats': return base + '.stats()'; case 'estimated': return base + '.estimatedDocumentCount()';
    case 'count': return base + '.countDocuments(' + (c.filter ? extJ(c.filter) : '{}') + ')';
    case 'distinct': return base + '.distinct(' + JSON.stringify(c.field) + (c.filter ? ', ' + extJ(c.filter) : '') + ')';
    case 'aggregate': return base + '.aggregate([' + c.pipeline.map(extJ).join(', ') + '])';
  }
  let s = base + '.find(';
  if (c.filter || c.projection) { s += c.filter ? extJ(c.filter) : '{}'; if (c.projection) s += ', ' + extJ(c.projection); }
  s += ')';
  if (c.sort) s += '.sort(' + extJ(c.sort) + ')'; if (c.skip > 0) s += '.skip(' + c.skip + ')'; if (c.limit > 0) s += '.limit(' + c.limit + ')';
  return s;
}

/* ============================================================================
   八、MongoDB：文档生成 + 过滤 / 投影 / 排序 / 聚合（在样本文档上求值）+ 结果整形
   ========================================================================== */
const MGEN = {
  'mongo_order/orders': (i, total, ts, r, td) => {
    const items = Array.from({length: RI(r, 1, 4)}, () => ({skuId: mkLong(100000 + RI(r, 0, 5999) * 3), name: PK(r, SKUS), qty: PK(r, [1, 1, 2, 3]), price: mkDec((RI(r, 990, 59900) / 100).toFixed(2))}));
    const d = {_id: mkOid(oidHex(i, ts, td.n)), orderNo: 'O' + ymdOf(ts).replace(/-/g, '') + pad(10000000 + (Math.imul(i + 7, 2654435761) >>> 0) % 89999999, 8), userId: mkLong(10000 + h32('u' + i) % 9800000), status: PK(r, STATUS_W),
      amount: mkDec(items.reduce((a, x) => a + Number(x.price.s) * x.qty, 0).toFixed(2)), items};
    if (r() < 0.94) d.address = {city: PK(r, CITIES), detail: PK(r, ['中山路', '解放路', '文一西路', '科技园路', '天府大道']) + RI(r, 1, 999) + '号', zip: String(RI(r, 100000, 999999))};
    if (r() < 0.37) d.couponId = r() < 0.7 ? mkOid(oidHex(RI(r, 0, 1839), ts, 'coupons')) : null;
    if (r() < 0.92) d.payChannel = PK(r, ['alipay', 'wechat', 'wechat', 'card']);
    d.createdAt = mkDate(ts);
    if (r() < 0.88) d.paidAt = d.status >= 1 && d.status <= 3 ? mkDate(ts + RI(r, 20, 3600) * 1000) : null;
    return d;
  },
  'mongo_order/order_events': (i, total, ts, r, td) => {
    const ot = (SCHEMAS.mongo_order[0].tables.find(t => t.n === 'orders') || {rows: 1e7}).rows, j = Math.floor(i * ot / total);
    const d = {_id: mkOid(oidHex(i, ts, td.n)), orderId: mkOid(oidHex(j, tsAt(j, ot), 'orders')), type: PK(r, ['create', 'pay', 'pay', 'ship', 'sign', 'cancel'])};
    if (r() < 0.83) d.operator = r() < 0.9 ? PK(r, OPERATORS) : null;
    if (r() < 0.79) d.payload = PK(r, [{from: 0, to: 1}, {channel: 'wechat', amount: mkDec((RI(r, 990, 59900) / 100).toFixed(2))}, {express: 'SF' + RI(r, 100000000, 999999999)}, {reason: 'timeout'}]);
    d.ts = mkDate(ts); return d;
  },
  'mongo_order/carts': (i, total, ts, r, td) => {
    const d = {_id: mkOid(oidHex(i, ts, td.n)), userId: mkLong(10000 + (i * 2 + 1) % 9800000), skus: Array.from({length: RI(r, 1, 5)}, () => ({skuId: mkLong(100000 + RI(r, 0, 5999) * 3), qty: PK(r, [1, 1, 2, 5])})), updatedAt: mkDate(ts)};
    if (r() < 0.21) d.coupon = 'C' + RI(r, 1000, 9999); return d;
  },
  'mongo_order/coupons': (i, total, ts, r, td) => {
    const d = {_id: mkOid(oidHex(i, ts, td.n)), code: 'C' + (1000 + i), discount: PK(r, [0.95, 0.9, 0.85, 0.8, 0.75])};
    if (r() < 0.64) d.minAmount = PK(r, [50, 100, 200, 500]); d.validTo = mkDate(NOW_MS + RI(r, -30, 120) * DAY); d.enabled = r() < 0.8; return d;
  },
  'mongo_order/v_order_summary': (i, total, ts, r) => ({_id: ymdOf(NOW_MS - (29 - i) * DAY), cnt: RI(r, 8000, 21000), gmv: mkDec((RI(r, 800000, 2400000) + r()).toFixed(2))}),
  'mongo_order/api_logs': (i, total, ts, r, td) => ({_id: mkOid(oidHex(i, ts, td.n)), path: PK(r, ['/api/order/list', '/api/order/create', '/api/pay/notify', '/api/user/info', '/api/cart/add']), method: PK(r, ['GET', 'GET', 'POST']), status: PK(r, [200, 200, 200, 200, 304, 400, 500]), costMs: RI(r, 3, 900), traceId: hex(r, 16), ts: mkDate(ts)}),
  'mongo_order/error_events': (i, total, ts, r, td) => { const d = {_id: mkOid(oidHex(i, ts, td.n)), service: PK(r, ['order-api', 'pay-gateway', 'stock-job', 'coupon-svc']), level: PK(r, ['ERROR', 'ERROR', 'WARN']), message: PK(r, ['timeout calling downstream', 'duplicate key error', 'NullPointerException at OrderService.java:128', 'Redis connection reset'])}; if (r() < 0.58) d.stack = r() < 0.92 ? 'at com.example.order.' + PK(r, ['OrderService', 'PayClient', 'StockJob']) + '.run(…:' + RI(r, 20, 400) + ')' : null; d.ts = mkDate(ts); return d; },
  'mongo_user/users': (i, total, ts, r, td) => {
    const d = {_id: mkOid(oidHex(i, ts, td.n)), uid: mkLong(10000 + i), nickname: PK(r, NICKS) + RI(r, 1, 99)};
    if (r() < 0.91) d.phone = '1' + PK(r, [3, 5, 7, 8]) + RI(r, 100000000, 999999999);
    d.level = PK(r, [0, 0, 1, 1, 2, 3, 4, 5]);
    if (r() < 0.72) d.profile = Object.assign({gender: PK(r, ['M', 'F', 'U']), city: PK(r, CITIES)}, r() < 0.5 ? {birthday: mkDate(Date.UTC(RI(r, 1970, 2004), RI(r, 0, 11), RI(r, 1, 28)))} : {});
    if (r() < 0.66) d.tags = Array.from({length: RI(r, 1, 3)}, () => PK(r, ['vip', 'new', 'risk', 'buyer', 'student'])).filter((x, j, a) => a.indexOf(x) === j);
    d.regAt = mkDate(ts); return d;
  },
  'mongo_user/user_sessions': (i, total, ts, r, td) => { const d = {_id: mkOid(oidHex(i, ts, td.n)), uid: mkLong(10000 + h32('s' + i) % 5000000), ip: ipOf(r)}; if (r() < 0.96) d.ua = PK(r, UAS); d.loginAt = mkDate(ts); d.expireAt = mkDate(ts + 30 * DAY); return d; },
  'mongo_user/devices': (i, total, ts, r, td) => { const d = {_id: mkOid(oidHex(i, ts, td.n)), uid: mkLong(10000 + h32('d' + i) % 5000000), deviceId: hex(r, 16), os: PK(r, ['iOS 17', 'Android 13', 'Android 14', 'HarmonyOS 4'])}; if (r() < 0.74) d.pushToken = r() < 0.8 ? hex(r, 24) : null; d.lastSeen = mkDate(ts); return d; },
  'mongo_user/page_views': (i, total, ts, r, td) => { const d = {_id: mkOid(oidHex(i, ts, td.n))}; if (r() < 0.87) d.uid = mkLong(10000 + h32('p' + i) % 5000000); d.page = PK(r, PAGES); d.stayMs = RI(r, 200, 90000); d.ts = mkDate(ts); return d; },
  'mongo_user/search_history': (i, total, ts, r, td) => ({_id: mkOid(oidHex(i, ts, td.n)), uid: mkLong(10000 + h32('q' + i) % 5000000), keyword: PK(r, ['耳机', '机械键盘', '充电宝', '显示器', '台灯', '保温杯', '手环']), hits: RI(r, 0, 800), ts: mkDate(ts)}),
};
const MG_SORT_MEM = '服务端内存不足以完成排序 / 分组（在线查询禁止使用磁盘临时文件）：请给排序字段建索引、加更严格的过滤条件或先 $match / $limit 缩小数据量。';
/* 过滤条件“够不够窄”：只有低基数字段的等值 / 没有条件都算不够窄；带时间 / 其它字段条件的按够窄处理 */
const mgSelective = f => !!f && Object.keys(f).some(k => k.startsWith('$') || !MG_LOWCARD.has(k));
const MG_LOWCARD = setOf('status', 'level', 'type', 'method', 'enabled', 'os', 'service', 'payChannel', 'gender', 'coupon');
const MG_FK = {'mongo_order/order_events': {orderId: 'orders'}};
function mgDocAt(td, i) { const g = MGEN[td.key]; const ts = tsAt(i, td.total); return g ? g(i, td.total, ts, R(td.key + '#' + i), td) : {_id: mkOid(oidHex(i, ts, td.n))}; }
const MGPOOL = new Map();
function mgPool(td, want) {
  const total = td.total, n = Math.min(total, Math.min(20000, Math.max(POOL, want || 0))), ck = td.key + '@' + td.db + '#' + n;
  if (MGPOOL.has(ck)) return MGPOOL.get(ck);
  const out = []; for (let i = total - n; i < total; i++) out.push(mgDocAt(td, i));
  if (MGPOOL.size > 30) MGPOOL.clear(); MGPOOL.set(ck, out); return out;
}
/* ---- 路径 / 比较 ---- */
function mgGet(doc, path) {
  let cur = [doc];
  for (const p of String(path).split('.')) {
    const next = [];
    for (const c of cur) { if (c === null || c === undefined) continue; if (Array.isArray(c)) { if (/^\d+$/.test(p)) next.push(c[+p]); else for (const e of c) if (e && typeof e === 'object' && p in e) next.push(e[p]); } else if (typeof c === 'object' && !isSp(c) && p in c) next.push(c[p]); }
    cur = next;
  }
  return cur;
}
const mgGet1 = (doc, path) => { const v = mgGet(doc, path); return v.length ? v[0] : undefined; };
function mgCmp(a, b) {
  a = unwrap(a); b = unwrap(b);
  if (a === undefined || a === null) return (b === undefined || b === null) ? 0 : -1; if (b === undefined || b === null) return 1;
  if (typeof a === 'number' && typeof b === 'number') return a < b ? -1 : a > b ? 1 : 0;
  if (typeof a === 'string' && typeof b === 'string') return a < b ? -1 : a > b ? 1 : 0;
  if (typeof a === typeof b && typeof a === 'boolean') return a === b ? 0 : a ? 1 : -1;
  const sa = JSON.stringify(a), sb = JSON.stringify(b); return sa < sb ? -1 : sa > sb ? 1 : 0;
}
function mgEqVal(a, b) {
  if (isSp(b) && b.$$t === 're') { const re = new RegExp(b.p, b.f.replace(/[^imsu]/g, '')); return typeof a === 'string' && re.test(a); }
  if (Array.isArray(a)) return a.some(x => mgEqVal(x, b)) || (Array.isArray(b) && JSON.stringify(a) === JSON.stringify(b));
  if (a === undefined) return b === null;
  return mgCmp(a, b) === 0 && typeof unwrap(a) === typeof unwrap(b);
}
function mgFieldMatch(vals, cond) {
  const any = fn => vals.length ? vals.some(fn) : fn(undefined);
  if (isDoc(cond) && Object.keys(cond).length && Object.keys(cond).every(k => k.startsWith('$'))) {
    return Object.entries(cond).every(([op, arg]) => {
      switch (op) {
        case '$eq': return any(v => mgEqVal(v, arg)); case '$ne': return !any(v => mgEqVal(v, arg));
        case '$gt': return any(v => v !== undefined && v !== null && mgCmp(v, arg) > 0); case '$gte': return any(v => v !== undefined && v !== null && mgCmp(v, arg) >= 0);
        case '$lt': return any(v => v !== undefined && v !== null && mgCmp(v, arg) < 0); case '$lte': return any(v => v !== undefined && v !== null && mgCmp(v, arg) <= 0);
        case '$in': return any(v => arg.some(a => mgEqVal(v, a))); case '$nin': return !any(v => arg.some(a => mgEqVal(v, a)));
        case '$exists': return (vals.length > 0) === !!arg;
        case '$regex': { const re = new RegExp(isSp(arg) ? arg.p : arg, (cond.$options || (isSp(arg) ? arg.f : '')).replace(/[^imsu]/g, '')); return any(v => typeof v === 'string' && re.test(v)); }
        case '$options': return true;
        case '$not': return !mgFieldMatch(vals, arg);
        case '$size': return vals.some(v => Array.isArray(v) && v.length === arg);
        case '$all': return vals.some(v => Array.isArray(v) && arg.every(a => v.some(x => mgEqVal(x, a))));
        case '$elemMatch': return vals.some(v => Array.isArray(v) && v.some(e => isDoc(e) ? mgMatch(e, arg) : mgFieldMatch([e], arg)));
        case '$type': return vals.some(v => { const t = mgBsonType(v); return t === arg || ({string: 'string', number: 'double', int: 'int', long: 'long', bool: 'bool', date: 'date', objectId: 'objectId', array: 'array', object: 'object', null: 'null'})[arg] === t; });
        case '$mod': return any(v => typeof unwrap(v) === 'number' && unwrap(v) % arg[0] === arg[1]);
      }
      throw new NoSim('查询操作符 ' + op);
    });
  }
  return any(v => mgEqVal(v, cond));
}
function mgMatch(doc, f) {
  for (const [k, c] of Object.entries(f || {})) {
    if (k === '$and') { if (!c.every(x => mgMatch(doc, x))) return false; }
    else if (k === '$or') { if (!c.some(x => mgMatch(doc, x))) return false; }
    else if (k === '$nor') { if (c.some(x => mgMatch(doc, x))) return false; }
    else if (k === '$comment') continue;
    else if (k === '$expr') { const v = mgExpr(c, doc); if (!(v === true || (typeof v === 'number' && v !== 0))) return false; }
    else if (k.startsWith('$')) throw new NoSim('查询操作符 ' + k);
    else if (!mgFieldMatch(mgGet(doc, k), c)) return false;
  }
  return true;
}
function mgBsonType(v) {
  if (v === null || v === undefined) return 'null'; if (isSp(v)) return ({oid: 'objectId', date: 'date', long: 'long', int: 'int', dec: 'decimal', re: 'regex', ts: 'timestamp', min: 'minKey', max: 'maxKey', bin: 'binData', uuid: 'binData'})[v.$$t] || 'object';
  if (Array.isArray(v)) return 'array'; if (typeof v === 'string') return 'string'; if (typeof v === 'boolean') return 'bool'; if (typeof v === 'number') return Number.isInteger(v) ? 'int' : 'double'; return 'object';
}
/* ---- 表达式（聚合）---- */
function mgExpr(e, doc, vars) {
  if (typeof e === 'string' && e.startsWith('$$')) { const n = e.slice(2).split('.'); const base = n[0] === 'ROOT' || n[0] === 'CURRENT' ? doc : (vars && vars[n[0]]); return n.length > 1 ? mgGet1(base, n.slice(1).join('.')) : base; }
  if (typeof e === 'string' && e.startsWith('$')) { const v = mgGet(doc, e.slice(1)); return v.length > 1 ? v : v[0]; }
  if (Array.isArray(e)) return e.map(x => mgExpr(x, doc, vars));
  if (isSp(e)) return e;
  if (e && typeof e === 'object') {
    const ks = Object.keys(e);
    if (ks.length === 1 && ks[0].startsWith('$')) {
      const op = ks[0], a = e[op], ar = Array.isArray(a) ? a : [a], v = i => mgExpr(ar[i], doc, vars), n = i => { const x = unwrap(v(i)); return x === null || x === undefined ? null : Number(x); };
      switch (op) {
        case '$literal': return a; case '$multiply': return ar.reduce((p, _, i) => p === null ? null : (n(i) === null ? null : p * n(i)), 1); case '$add': { const vs = ar.map((_, i) => v(i)); if (vs.some(x => isSp(x) && x.$$t === 'date')) return mkDate(vs.reduce((s, x) => s + (isSp(x) ? x.ms : Number(x)), 0)); return vs.reduce((s, x) => s + Number(unwrap(x)), 0); }
        case '$subtract': { const x = v(0), y = v(1); if (isSp(x) && x.$$t === 'date') return isSp(y) ? x.ms - y.ms : mkDate(x.ms - Number(y)); return n(0) - n(1); }
        case '$divide': return n(1) ? n(0) / n(1) : null; case '$mod': return n(0) % n(1); case '$abs': return Math.abs(n(0)); case '$ceil': return Math.ceil(n(0)); case '$floor': return Math.floor(n(0)); case '$round': return r2(n(0), ar.length > 1 ? n(1) : 0); case '$sqrt': return Math.sqrt(n(0)); case '$pow': return Math.pow(n(0), n(1));
        case '$concat': return ar.map((_, i) => v(i)).some(x => x === null || x === undefined) ? null : ar.map((_, i) => String(v(i))).join(''); case '$toString': { const x = v(0); return x === null || x === undefined ? null : isSp(x) ? (x.$$t === 'oid' ? x.hex : x.$$t === 'date' ? new Date(x.ms).toISOString() : String(x.n !== undefined ? x.n : x.s)) : String(x); }
        case '$toLower': return String(v(0) ?? '').toLowerCase(); case '$toUpper': return String(v(0) ?? '').toUpperCase(); case '$strLenCP': return [...String(v(0) ?? '')].length; case '$substr': case '$substrCP': return [...String(v(0) ?? '')].slice(n(1), n(1) + n(2)).join('');
        case '$size': return Array.isArray(v(0)) ? v(0).length : null; case '$arrayElemAt': { const arr = v(0); const i = n(1); return Array.isArray(arr) ? arr[i < 0 ? arr.length + i : i] : null; } case '$first': return Array.isArray(v(0)) ? v(0)[0] : v(0); case '$last': return Array.isArray(v(0)) ? v(0)[v(0).length - 1] : v(0);
        case '$concatArrays': return [].concat(...ar.map((_, i) => v(i) || [])); case '$isArray': return Array.isArray(v(0)); case '$in': return Array.isArray(v(1)) && v(1).some(x => mgCmp(x, v(0)) === 0);
        case '$cond': { const c = Array.isArray(a) ? a : [a.if, a.then, a.else]; const t = mgExpr(c[0], doc, vars); return (t === true || (typeof t === 'number' && t !== 0)) ? mgExpr(c[1], doc, vars) : mgExpr(c[2], doc, vars); }
        case '$ifNull': { for (let i = 0; i < ar.length - 1; i++) { const x = v(i); if (x !== null && x !== undefined) return x; } return v(ar.length - 1); }
        case '$eq': return mgCmp(v(0), v(1)) === 0; case '$ne': return mgCmp(v(0), v(1)) !== 0; case '$gt': return mgCmp(v(0), v(1)) > 0; case '$gte': return mgCmp(v(0), v(1)) >= 0; case '$lt': return mgCmp(v(0), v(1)) < 0; case '$lte': return mgCmp(v(0), v(1)) <= 0;
        case '$and': return ar.every((_, i) => { const x = v(i); return x === true || (typeof x === 'number' && x !== 0); }); case '$or': return ar.some((_, i) => { const x = v(i); return x === true || (typeof x === 'number' && x !== 0); }); case '$not': { const x = v(0); return !(x === true || (typeof x === 'number' && x !== 0)); }
        case '$year': case '$month': case '$dayOfMonth': case '$hour': case '$dayOfWeek': { const x = v(0); const ms = isSp(x) ? x.ms : NaN; if (isNaN(ms)) return null; const p = lparts(ms + TZ * 0 - TZ); const q = new Date(ms); return ({'$year': q.getUTCFullYear(), '$month': q.getUTCMonth() + 1, '$dayOfMonth': q.getUTCDate(), '$hour': q.getUTCHours(), '$dayOfWeek': q.getUTCDay() + 1})[op]; }
        case '$dateToString': { const x = mgExpr(a.date, doc, vars); if (!isSp(x)) return null; const f = a.format || '%Y-%m-%dT%H:%M:%S.%LZ', q = new Date(x.ms); return String(f).replace(/%([YmdHMSLj%])/g, (m, c) => ({Y: q.getUTCFullYear(), m: pad(q.getUTCMonth() + 1), d: pad(q.getUTCDate()), H: pad(q.getUTCHours()), M: pad(q.getUTCMinutes()), S: pad(q.getUTCSeconds()), L: pad(q.getUTCMilliseconds(), 3), '%': '%'})[c]); }
        case '$toInt': case '$toLong': return Math.trunc(n(0)); case '$toDouble': case '$toDecimal': return n(0); case '$type': return mgBsonType(v(0)); case '$sum': { const x = v(0); return Array.isArray(x) ? x.reduce((s, y) => s + Number(unwrap(y)), 0) : Number(unwrap(x)); } case '$avg': { const x = v(0); return Array.isArray(x) && x.length ? x.reduce((s, y) => s + Number(unwrap(y)), 0) / x.length : null; } case '$max': { const x = v(0); return Array.isArray(x) ? x.reduce((m, y) => mgCmp(y, m) > 0 ? y : m) : x; } case '$min': { const x = v(0); return Array.isArray(x) ? x.reduce((m, y) => mgCmp(y, m) < 0 ? y : m) : x; }
      }
      throw new NoSim('表达式操作符 ' + op);
    }
    const o = {}; for (const [k, x] of Object.entries(e)) o[k] = mgExpr(x, doc, vars); return o;
  }
  return e;
}
function mgProject(doc, proj) {
  const keys = Object.keys(proj); if (!keys.length) return doc;
  const incl = keys.filter(k => k !== '_id' && (proj[k] === 1 || proj[k] === true || (typeof proj[k] === 'object' && !isSp(proj[k]) && false)));
  const excl = keys.filter(k => k !== '_id' && (proj[k] === 0 || proj[k] === false));
  const computed = keys.filter(k => k !== '_id' && !incl.includes(k) && !excl.includes(k));
  if (incl.length || computed.length) {
    const out = {}; if (proj._id !== 0 && proj._id !== false && '_id' in doc) out._id = doc._id;
    for (const k of Object.keys(proj)) {
      if (k === '_id') continue;
      if (incl.includes(k)) { const v = mgGet(doc, k); if (v.length) setPath(out, k, v[0]); } else { setPath(out, k, mgExpr(proj[k], doc)); }
    }
    return out;
  }
  const out = JSON.parse(JSON.stringify(doc, (k, v) => v), (k, v) => v); const o2 = Object.assign({}, doc);
  for (const k of keys) { if (k.includes('.')) { const ps = k.split('.'); let cur = o2; for (let i = 0; i < ps.length - 1; i++) { if (!isDoc(cur[ps[i]])) { cur = null; break; } cur[ps[i]] = Object.assign({}, cur[ps[i]]); cur = cur[ps[i]]; } if (cur) delete cur[ps[ps.length - 1]]; } else delete o2[k]; }
  return o2;
}
function setPath(o, path, v) { const ps = path.split('.'); let cur = o; for (let i = 0; i < ps.length - 1; i++) { if (!isDoc(cur[ps[i]])) cur[ps[i]] = {}; cur = cur[ps[i]]; } cur[ps[ps.length - 1]] = v; }
function mgSort(docs, spec) {
  const ks = Object.entries(spec);
  return docs.slice().sort((a, b) => { for (const [k, d] of ks) { const c = mgCmp(mgGet1(a, k), mgGet1(b, k)); if (c) return c * (d < 0 || d === 'desc' ? -1 : 1); } return 0; });
}
/* 按索引字段取文档（模拟索引查找）：_id（ObjectId）精确；FK（orderId→orders）；其它索引字段合成 1~6 个文档 */
function mgSynth(td, filter) {
  if (!filter) return null;
  for (const [k, c] of Object.entries(filter)) {
    if (k.startsWith('$')) continue;
    const idx = (td.t.idx || []).some(x => x[1][0] === k) && !MG_LOWCARD.has(k);
    const lits = isSp(c) || typeof c !== 'object' || c === null ? [c] : (isDoc(c) && Array.isArray(c.$in) ? c.$in : (isDoc(c) && c.$eq !== undefined ? [c.$eq] : null));
    if (!lits || !idx || lits.some(l => isSp(l) && l.$$t === 're')) continue;
    const out = [];
    for (const lit of lits.slice(0, 50)) {
      if (k === '_id' && isSp(lit) && lit.$$t === 'oid') { const i = parseInt(lit.hex.slice(8, 16), 16); if (i >= 0 && i < td.total) { const d = mgDocAt(td, i); d._id = lit; out.push(d); } continue; }
      if (k === '_id') continue;
      const key = String(unwrap(lit)), fk = (MG_FK[td.key] || {})[k];
      if (fk && isSp(lit) && lit.$$t === 'oid') {
        const pt = (SCHEMAS[td.schema][0].tables.find(t => t.n === fk) || {rows: 1e7}).rows, j = parseInt(lit.hex.slice(8, 16), 16);
        const lo = Math.ceil(j * td.total / pt), hi = Math.ceil((j + 1) * td.total / pt);
        for (let i = lo; i < hi && i < td.total; i++) { const d = mgDocAt(td, i); d[k] = lit; out.push(d); }
        continue;
      }
      const uniq = (td.t.idx || []).some(x => x[1][0] === k && x[2]);
      const cnt = uniq ? 1 : 1 + h32(td.key + k + key) % 6;
      for (let j = 0; j < cnt; j++) { const i = td.total - 1 - ((h32(key + 'x' + j) + j * 104729) % Math.min(td.total, POOL)); const d = mgDocAt(td, i); d[k] = lit; out.push(d); }
    }
    return out.sort((a, b) => mgCmp(a._id, b._id));
  }
  return null;
}
function mgIndexed(td, filter, sort) {
  const first = new Set((td.t.idx || []).map(x => x[1][0]));
  const fOk = filter && Object.keys(filter).some(k => first.has(k) || (k === '$and' || k === '$or'));
  const sOk = !sort || Object.keys(sort).length === 0 || first.has(Object.keys(sort)[0]);
  return {filterIdx: !!fOk, sortIdx: sOk};
}
function mgLookupTd(inst, db, coll) { return tdOf(inst, db, coll); }
function mgScan(td, filter, cx, want) {
  const syn = td.view && td.kind === 'view' ? null : mgSynth(td, filter);
  if (syn) { cx.synth = true; return syn.filter(d => mgMatch(d, filter)); }
  let rows = td.view && td.kind === 'view' ? Array.from({length: 30}, (_, i) => mgDocAt(td, i)) : mgPool(td, want);
  if (td.total > POOL && !(td.view)) cx.scale = Math.max(1, (filter && Object.keys(filter).length ? td.total * 30 / 730 : td.total) / rows.length);
  return filter && Object.keys(filter).length ? rows.filter(d => mgMatch(d, filter)) : rows;
}
const MG_TIMEOUT = () => new EErr('timeout', 'timeout');
function mgPipelineRun(td, pipe, cx, inst, db, want) {
  let docs = null, first = true, scaleOn = true;
  const stages = pipe.slice();
  /* 第一个 $match 用来取数 */
  let leading = null;
  if (stages.length && stages[0].$match) leading = stages[0].$match;
  const idx = mgIndexed(td, leading, null);
  if (td.total > 1e8 && !idx.filterIdx) throw MG_TIMEOUT();
  docs = mgScan(td, leading, cx, want); if (leading) stages.shift();
  const sc = () => cx.synth ? 1 : cx.scale;
  for (const s of stages) {
    const op = Object.keys(s)[0], a = s[op];
    switch (op) {
      case '$match': docs = docs.filter(d => mgMatch(d, a)); break;
      case '$sort': docs = mgSort(docs, a); break;
      case '$limit': docs = docs.slice(0, intVal(a)); break; case '$skip': docs = docs.slice(intVal(a)); break;
      case '$project': docs = docs.map(d => mgProject(d, a)); break;
      case '$addFields': case '$set': docs = docs.map(d => { const o = Object.assign({}, d); for (const [k, v] of Object.entries(a)) setPath(o, k, mgExpr(v, d)); return o; }); break;
      case '$unset': { const ks = Array.isArray(a) ? a : [a]; docs = docs.map(d => { const o = Object.assign({}, d); ks.forEach(k => delete o[k]); return o; }); break; }
      case '$count': docs = [{[a]: Math.round(docs.length * sc())}]; break;
      case '$sample': docs = docs.slice(0, intVal(a.size)); break;
      case '$replaceRoot': case '$replaceWith': docs = docs.map(d => mgExpr(op === '$replaceRoot' ? a.newRoot : a, d)); break;
      case '$unwind': { const path = (typeof a === 'string' ? a : a.path).replace(/^\$/, ''), keep = !(typeof a === 'string') && a.preserveNullAndEmptyArrays; const out = []; for (const d of docs) { const v = mgGet1(d, path); if (Array.isArray(v) && v.length) v.forEach(e => { const o = Object.assign({}, d); setPath(o, path, e); out.push(o); }); else if (v !== undefined && v !== null && !Array.isArray(v)) out.push(d); else if (keep) out.push(d); } docs = out; break; }
      case '$sortByCount': { const m = new Map(); for (const d of docs) { const k = mgExpr(a, d); const key = JSON.stringify(unwrap(k)); const e = m.get(key) || {_id: k, count: 0}; e.count++; m.set(key, e); } docs = [...m.values()].map(e => ({_id: e._id, count: Math.round(e.count * sc())})).sort((x, y) => y.count - x.count); break; }
      case '$group': {
        const m = new Map();
        for (const d of docs) {
          const id = mgExpr(a._id, d), key = JSON.stringify(unwrap(isDoc(id) ? Object.fromEntries(Object.entries(id).map(([k, v]) => [k, unwrap(v)])) : id));
          let g = m.get(key); if (!g) m.set(key, g = {id, docs: []}); g.docs.push(d);
        }
        if (!m.size && a._id === null) m.set('null', {id: null, docs: []});
        docs = [...m.values()].map(g => {
          const o = {_id: g.id};
          for (const [k, acc] of Object.entries(a)) {
            if (k === '_id') continue; const ao = Object.keys(acc)[0], arg = acc[ao]; const vals = () => g.docs.map(d => mgExpr(arg, d));
            switch (ao) {
              case '$sum': { const lit = typeof arg === 'number'; const s = lit ? g.docs.length * arg : vals().reduce((p, x) => p + (typeof unwrap(x) === 'number' ? Number(unwrap(x)) : 0), 0); o[k] = lit ? Math.round(s * sc()) : r2(s * sc(), 4); const anyDec = !lit && g.docs.length && vals().some(x => isSp(x) && x.$$t === 'dec'); if (anyDec) o[k] = mkDec(Number(o[k]).toFixed(2)); break; }
              case '$avg': { const vs = vals().map(unwrap).filter(x => typeof x === 'number'); o[k] = vs.length ? vs.reduce((p, x) => p + x, 0) / vs.length : null; break; }
              case '$min': case '$max': { const vs = vals().filter(x => x !== undefined && x !== null); o[k] = vs.length ? vs.reduce((mm, x) => (ao === '$min' ? mgCmp(x, mm) < 0 : mgCmp(x, mm) > 0) ? x : mm) : null; break; }
              case '$first': o[k] = g.docs.length ? mgExpr(arg, g.docs[0]) : null; break; case '$last': o[k] = g.docs.length ? mgExpr(arg, g.docs[g.docs.length - 1]) : null; break;
              case '$push': o[k] = vals().slice(0, 1000); break; case '$addToSet': { const seen = new Set(), out = []; for (const x of vals()) { const kk = JSON.stringify(unwrap(x)); if (!seen.has(kk)) { seen.add(kk); out.push(x); } } o[k] = out; break; }
              case '$count': o[k] = Math.round(g.docs.length * sc()); break;
              default: throw new NoSim('累加器 ' + ao);
            }
          }
          return o;
        });
        break;
      }
      case '$lookup': {
        const ftd = mgLookupTd(inst, db, a.from); const out = [];
        if (a.pipeline) throw new NoSim('$lookup 的 pipeline 写法');
        for (const d of docs) {
          const lv = mgGet1(d, a.localField); let matches = [];
          if (ftd && lv !== undefined) matches = mgSynth(ftd, {[a.foreignField]: lv}) || mgPool(ftd).filter(f => mgCmp(mgGet1(f, a.foreignField), lv) === 0);
          if (ftd && a.foreignField === '_id' && isSp(lv) && lv.$$t === 'oid') { const i = parseInt(lv.hex.slice(8, 16), 16); matches = i < ftd.total ? [Object.assign(mgDocAt(ftd, i), {_id: lv})] : []; }
          out.push(Object.assign({}, d, {[a.as]: matches.slice(0, 50)}));
        }
        cx.synth = true; docs = out; break;
      }
      default: throw new NoSim('聚合阶段 ' + op);
    }
  }
  return docs;
}

/* ---- 结果整形 ---- */
const MG_CELL = 64 << 10;
function mgFmtDate(ms) { return new Date(ms).toISOString(); }
function mgCell(v, st) {
  if (v === undefined || v === null) return null;
  if (isSp(v)) {
    switch (v.$$t) {
      case 'oid': return `ObjectId("${v.hex}")`; case 'date': return mgFmtDate(v.ms); case 'long': return String(v.n); case 'int': return v.n; case 'dec': return v.s; case 're': return mgCut('/' + v.p + '/' + v.f, st);
      case 'ts': return `Timestamp(${v.t}, ${v.i})`; case 'min': return 'MinKey'; case 'max': return 'MaxKey'; case 'uuid': return `UUID("${v.s}")`; case 'bin': st.binary++; return `BinData(${v.sub}, \\x${v.hex})`;
    }
  }
  if (typeof v === 'string') return mgCut(v, st);
  if (typeof v === 'boolean') return v;
  if (typeof v === 'number') return Number.isFinite(v) ? v : (isNaN(v) ? 'NaN' : v > 0 ? 'Infinity' : '-Infinity');
  if (Array.isArray(v) || typeof v === 'object') return mgCut(extJ(v), st);
  return mgCut(String(v), st);
}
function mgCut(s, st) { if (s.length <= MG_CELL) return s; st.cut++; return s.slice(0, MG_CELL) + `…[已截断，共 ${new TextEncoder().encode(s).length} 字节]`; }
function mgDocRows(docs, st) {
  const cols = [], idx = {}; let extra = false;
  for (const d of docs) for (const k of Object.keys(d)) { if (k in idx) continue; if (cols.length >= 200) { extra = true; continue; } idx[k] = cols.length; cols.push(k); }
  let en = '_extra'; while (en in idx) en += '_';
  const n = cols.length; if (extra) cols.push(en);
  const rows = docs.map(d => { const row = new Array(cols.length).fill(null); const rest = {}; let hasRest = false; for (const [k, v] of Object.entries(d)) { if (k in idx) row[idx[k]] = mgCell(v, st); else { rest[k] = v; hasRest = true; } } if (extra && hasRest) row[n] = mgCut(extJ(rest), st); return row; });
  return {cols, rows};
}
MG.run = function (inst, db, ck, lim) {
  MG.validDb(db);
  const maxRows = lim.maxRows, cx = {synth: false, scale: 1}, st = {cut: 0, binary: 0};
  const out = {warnings: ck.notices.slice(), notices: [], executed: ck.statement, stmtKind: ck.kind, truncated: false, truncatedBy: ''};
  const td = ck.coll ? tdOf(inst, db, ck.coll) : null;
  let cols = [], rows = [];
  const setDocs = docs => { const r = mgDocRows(docs, st); cols = r.cols; rows = r.rows; };
  switch (ck.kind) {
    case 'listCollections': {
      const d = findDb(inst, db); cols = ['collection', 'type'];
      rows = (d ? d.tables : []).slice().sort((a, b) => a.n < b.n ? -1 : 1).map(t => [t.n, t.view ? 'view' : 'collection']); break;
    }
    case 'dbStats': {
      const d = findDb(inst, db), objs = (d ? d.tables : []).reduce((a, t) => a + (t.view ? 0 : t.rows), 0), size = (d ? d.tables : []).reduce((a, t) => a + (t.size || 0), 0) * 1048576;
      setDocs([{db, collections: d ? d.tables.filter(t => !t.view).length : 0, views: d ? d.tables.filter(t => t.view).length : 0, objects: mkLong(objs), avgObjSize: size && objs ? Math.round(size / objs) : 0, dataSize: mkLong(Math.round(size)), storageSize: mkLong(Math.round(size * 0.46)), indexes: 14, indexSize: mkLong(Math.round(size * 0.07)), totalSize: mkLong(Math.round(size * 0.53)), ok: 1}]); break;
    }
    case 'collStats': {
      if (!td) { setDocs([{ns: `${db}.${ck.coll}`, size: 0, count: 0, storageSize: 0, nindexes: 0, ok: 1}]); break; }
      const sz = (td.t.size || 0) * 1048576; setDocs([{ns: `${db}.${ck.coll}`, size: mkLong(Math.round(sz)), count: mkLong(td.t.view ? 0 : td.total), avgObjSize: td.total ? Math.round(sz / td.total) : 0, storageSize: mkLong(Math.round(sz * 0.46)), freeStorageSize: mkLong(Math.round(sz * 0.01)), capped: false, nindexes: (td.t.idx || []).length, totalIndexSize: mkLong(Math.round(sz * 0.07)), totalSize: mkLong(Math.round(sz * 0.53)), indexSizes: Object.fromEntries((td.t.idx || []).map(x => [x[0], Math.round(sz * 0.07 / Math.max(1, (td.t.idx || []).length))])), scaleFactor: 1, ok: 1}]); break;
    }
    case 'listIndexes': {
      setDocs(td ? (td.t.idx || []).map(x => { const key = {}; const m = x[0] === '_id_' ? ['_id', '1'] : x[0].split(/_(?=-?1$|-?1_|text$|2dsphere$)/); if (x[0] === '_id_') key._id = 1; else for (let i = 0; i + 1 < m.length + 1; i += 2) { if (m[i] === undefined) break; const dv = m[i + 1]; key[m[i]] = dv === undefined ? 1 : /^-?\d+$/.test(dv) ? Number(dv) : dv; } const d = {v: 2, key, name: x[0]}; if (x[2] && x[0] !== '_id_') d.unique = true; return d; }) : []); break;
    }
    case 'estimated': cols = ['count']; rows = [[td ? (td.t.view ? 0 : td.total) : 0]]; break;
    case 'count': {
      if (td && td.total > 1e8 && !ck.filter) { cols = ['count']; rows = [[td.total]]; break; }
      if (td && td.total > 1e8 && !mgIndexed(td, ck.filter, null).filterIdx) throw MG_TIMEOUT();
      const ds = td ? mgScan(td, ck.filter, cx, 3000) : []; cols = ['count']; rows = [[ck.filter && Object.keys(ck.filter).length ? Math.round(ds.length * (cx.synth ? 1 : cx.scale)) : (td ? (td.t.view ? ds.length : td.total) : 0)]]; break;
    }
    case 'distinct': {
      const ds = td ? mgScan(td, ck.filter, cx, 3000) : []; const seen = new Set(), vals = [];
      for (const d of ds) { for (const v of mgGet(d, ck.field)) for (const x of (Array.isArray(v) ? v : [v])) { const k = JSON.stringify(unwrap(x)); if (x !== undefined && !seen.has(k)) { seen.add(k); vals.push(x); } } }
      vals.sort(mgCmp); cols = [ck.field]; rows = vals.slice(0, maxRows + 1).map(v => [mgCell(v, st)]); if (vals.length > maxRows) { out.truncated = true; out.truncatedBy = 'rows'; rows = rows.slice(0, maxRows); } break;
    }
    case 'find': {
      if (!td) { cols = []; rows = []; break; }
      const idx = mgIndexed(td, ck.filter, ck.sort);
      if (td.total > 1e6 && ck.sort && !idx.sortIdx && !mgSelective(ck.filter)) { if (ck.limit > 0 && ck.limit <= 1000) { if (td.total > 5e7) throw MG_TIMEOUT(); } else throw bad(MG_SORT_MEM); }
      if (td.total > 1e8 && ck.filter && Object.keys(ck.filter).length && !idx.filterIdx) throw MG_TIMEOUT();
      let ds = mgScan(td, ck.filter, cx, maxRows + 1 + (ck.skip || 0));
      if (ck.sort) ds = mgSort(ds, ck.sort);
      if (ck.skip) ds = ds.slice(ck.skip);
      const fetch = ck.limit > 0 && ck.limit <= maxRows ? ck.limit : maxRows + 1;
      ds = ds.slice(0, fetch);
      if (ds.length > maxRows) { ds = ds.slice(0, maxRows); out.truncated = true; out.truncatedBy = 'rows'; }
      if (ck.projection) ds = ds.map(d => mgProject(d, ck.projection));
      setDocs(ds); break;
    }
    case 'aggregate': {
      if (!td) { cols = []; rows = []; break; }
      const pipe = ck.pipeline.concat([{$limit: maxRows + 1}]);
      const hasSort = ck.pipeline.some(s => s.$sort), hasGroup = ck.pipeline.some(s => s.$group);
      const first = ck.pipeline[0] && ck.pipeline[0].$match;
      if (td.total > 1e6 && hasSort && !hasGroup && !mgSelective(first) && !mgIndexed(td, null, ck.pipeline.find(s => s.$sort).$sort).sortIdx && !ck.pipeline.some(s => s.$limit)) throw bad(MG_SORT_MEM);
      let ds = mgPipelineRun(td, pipe, cx, inst, db, maxRows + 1);
      if (ds.length > maxRows) { ds = ds.slice(0, maxRows); out.truncated = true; out.truncatedBy = 'rows'; }
      setDocs(ds); break;
    }
    default: throw bad(`不支持的语句种类 "${ck.kind}"`);
  }
  if (rows.length > maxRows) { rows = rows.slice(0, maxRows); out.truncated = true; out.truncatedBy = 'rows'; }
  if (out.truncated) out.warnings.push(`结果已按上限截断（最多 ${maxRows} 行 / 4 MB 原始数据）；请加过滤条件、投影或用 skip / limit 分页。`);
  if (st.cut) out.warnings.push(`${st.cut} 个值超过 ${MG_CELL >> 10} KB，已截断显示。`);
  if (st.binary) out.warnings.push(`${st.binary} 个二进制值已按 BinData(子类型, \\xHH...) 显示。`);
  return Object.assign(out, {columns: cols, types: null, rows, synth: cx.synth, scale: cx.scale});
};

/* ============================================================================
   九、对外接口：检查 + 授权对象 + 执行、库表树、结果区 HTML、数据字典、示例语句
   ========================================================================== */
const FAM = {PgSQL: 'pgsql', Mongo: 'mongo', ClickHouse: 'clickhouse'};
const LABEL = {pgsql: 'PostgreSQL', mongo: 'MongoDB', clickhouse: 'ClickHouse'};
const family = inst => (inst && FAM[inst.type]) || null;
const dbsOfInst = inst => (SCHEMAS[inst.schema] || []).map(d => ({db: d.db}));

/* 静态检查（不连服务端）+ 授权要看的对象。返回 {err} 或 {ck, targets:{objects, requireDb, structure, dbs}} */
function check(inst, db, text) {
  const fam = family(inst), t = String(text == null ? '' : text);
  if (!t.trim()) return {err: '语句为空'};
  try {
    if (fam === 'pgsql') {
      if (t.length > 128 << 10) return {err: '语句过长（上限 128 KB）'};
      const ck = PG.check(t, db), tg = {objects: [], requireDb: '', structure: ck.kind === 'show', dbs: []};
      if (ck.kind !== 'show' && ck.needAllPriv) tg.requireDb = `该语句在 FROM 里调用了自定义函数${ck.functions.length ? '（' + ck.functions.join('、') + '）' : ''}，无法确定实际访问的表，需要库 ${db} 的库级查询权限；请申请库级权限，或改用内置函数。`;
      for (const r of ck.tables) {
        if (isSystemSchema(r.schema)) { const ls = r.schema.toLowerCase(); if (ls === 'pg_catalog' || ls === 'information_schema') continue; return {err: `不允许查询系统 schema ${r.schema}`}; }
        tg.objects.push({db, name: r.schema + '.' + r.name});
      }
      return {ck, targets: tg, stmt: ck.sql};
    }
    if (fam === 'clickhouse') {
      if (t.length > 128 << 10) return {err: '语句过长（上限 128 KB）'};
      const ck = CH.check(t, db), tg = {objects: [], requireDb: '', structure: ['show', 'describe', 'exists'].includes(ck.kind), dbs: []};
      if (tg.structure) tg.dbs = ck.databases.filter(d => !chIsSystem(d) && !chIsInfo(d));
      for (const r of ck.tables) { if (chIsSystem(r.db) || chIsInfo(r.db)) continue; tg.objects.push({db: r.db, name: r.name}); }
      return {ck, targets: tg, stmt: ck.stmt};
    }
    if (fam === 'mongo') {
      if (t.length > 128 << 10) return {err: '语句过长（上限 128 KB）'};
      MG.validDb(db);
      const ck = MG.check(t), tg = {objects: ck.collections.map(c => ({db, name: c})), requireDb: ck.dbLevel ? `该语句查看整个库的信息，需要库 ${db} 的库级查询权限；请申请库级权限，或改查具体集合。` : '', structure: false, dbs: []};
      return {ck, targets: tg, stmt: ck.statement};
    }
  } catch (e) { if (e instanceof EErr) return {err: e.message}; throw e; }
  return {err: '该类型暂不支持在线查询'};
}
const msOfRun = (exec, n, extra) => Math.max(2, 3 + (h32(exec) % 24) + Math.floor(n / 250) + (extra || 0));
/* 执行已通过检查与授权的语句：返回 {ok, ...} 或 {err:{code,msg,title}} */
function run(inst, db, ck, maxRows) {
  const fam = family(inst), lim = {maxRows: Math.max(1, maxRows), maxBytes: 4 << 20};
  try {
    let r;
    if (fam === 'pgsql') r = PG.run(inst, db, ck, lim);
    else if (fam === 'clickhouse') r = CH.run(inst, db, ck, lim);
    else {
      const ck2 = MG.check(ck.rawText, {maxRows: lim.maxRows});
      r = MG.run(inst, db, ck2, lim);
    }
    const heavy = r.synth ? 0 : (r.scale > 1 ? Math.min(220, Math.round(Math.log10(r.scale) * 38)) : 0);
    r.ms = msOfRun(r.executed, r.rows.length, heavy);
    r.ok = true;
    return r;
  } catch (e) {
    if (e instanceof EErr) return {err: {code: e.ecode, msg: e.ecode === 'timeout' ? '' : e.message, title: e.etitle}};
    if (e instanceof SynErr || e instanceof NoSim) { const w = wrapSim(e, ck.sql || ck.stmt || ck.rawText || '', fam === 'clickhouse' ? 'ch' : 'pg'); return {err: {code: w.ecode, msg: w.message, title: w.etitle}}; }
    return {err: {code: 'engine_error', msg: '原型内部错误：' + e.message, title: '原型未模拟'}};
  }
}
const TRUNC_NOTE = (by, n, bytes) => by === 'bytes' ? `结果超过 ${nf(bytes || 4194304)} 字节上限，已截断。` : by === 'response' ? '服务端返回的数据量超过读取上限，已截断。' : `结果已按 ${nf(n)} 行上限截断。`;

/* ---- 库表树 ---- */
function treeData(inst, db) {
  const d = findDb(inst, db); if (!d) return null;
  const fam = family(inst), items = d.tables.map(t => ({n: t.n, name: fam === 'pgsql' ? t.n.split('.').slice(1).join('.') : t.n, schema: fam === 'pgsql' ? t.n.split('.')[0] : '', kind: fam === 'mongo' ? (t.view ? 'view' : 'collection') : (t.kind || (t.view ? 'view' : 'table')), view: !!t.view, rows: t.view && t.kind !== 'dict' ? -1 : t.rows, comment: t.c || '', cols: t.cols, eng: t.eng}));
  if (fam !== 'pgsql') return {groups: [{label: '', items}]};
  const m = new Map(); items.forEach(it => { if (!m.has(it.schema)) m.set(it.schema, []); m.get(it.schema).push(it); });
  const names = [...m.keys()].sort((a, b) => a === 'public' ? -1 : b === 'public' ? 1 : a < b ? -1 : 1);
  return {groups: names.map(s => ({label: s, items: m.get(s)}))};
}
const KIND_ICO = {table: '▦', view: '◈', matview: '◈', dictionary: '☷', dict: '☷', collection: '▦'};
const KIND_TXT = {table: '表', view: '视图', matview: '物化视图', dict: '字典', collection: '集合'};
/* 点表名 / 集合名生成的语句 */
function exampleFor(inst, db, it) {
  const fam = family(inst);
  if (fam === 'pgsql') return `select ${it.cols.map(c => c.n).join(', ')}\nfrom ${it.n}\nlimit 100;`;
  if (fam === 'clickhouse') return `select ${it.cols.map(c => c.n).join(', ')}\nfrom ${it.n}\nlimit 100`;
  return /^[A-Za-z_][A-Za-z0-9_]*$/.test(it.n) && !MG_RESERVED.has(it.n) ? `db.${it.n}.find({}).limit(10)` : `db.getCollection(${JSON.stringify(it.n)}).find({}).limit(10)`;
}

/* ---- 结果区 ---- */
function resultHtml(r, max) {
  const cell = (v, ci) => v == null ? '<span class="pq-null">NULL</span>' : typeof v === 'number' || typeof v === 'boolean' ? esc(v) : String(v).length > 160 ? `<span class="eq-long" title="${esc(String(v).slice(0, 400))}">${esc(v)}</span>` : esc(v);
  const shown = r.rows.slice(0, max), typed = r.types && r.types.some(Boolean);
  const warn = (r.warnings || []).length ? notice(`<ul style="margin:0;padding-left:18px" class="eq-warn">${r.warnings.map(w => `<li>${esc(w)}</li>`).join('')}</ul>`, 'warn', '⚠') : '';
  const notes = (r.notices || []).length ? notice(`<ul style="margin:0;padding-left:18px" class="eq-warn">${r.notices.map(w => `<li>${esc(w)}</li>`).join('')}</ul>`, '', 'ⓘ') : '';
  return `<div class="row" style="margin-bottom:6px"><b>${nf(r.n)} 行</b><span class="muted">· 耗时 ${r.ms} ms</span><span class="muted small">· ${esc(r.instName)} / ${esc(r.db)}</span>
      <span class="pq-chip warn">该类型的查询结果不做脱敏，请勿查看敏感数据</span><span class="sp"></span>
      <button class="btn sm" data-on="qryCopyRes">复制结果</button></div>
    ${r.limit ? `<div class="small muted" style="margin-bottom:6px">生效行数上限 ${nf(r.limit)}（${esc(r.limitSrc)}）${r.stmtKind ? ' · 语句种类 ' + esc(r.stmtKind) : ''}</div>` : ''}
    ${r.truncated ? notice(esc(TRUNC_NOTE(r.truncatedBy, r.limit, r.maxBytes)), 'warn', '✂') : ''}${warn}${notes}
    <div class="pq-grid"><table><thead><tr><th class="rn">#</th>${r.cols.map(c => `<th>${esc(c.n)}${typed && c.ty ? `<span class="eq-ty">${esc(c.ty)}</span>` : ''}</th>`).join('')}</tr></thead>
      <tbody>${shown.map((row, i) => `<tr><td class="rn">${i + 1}</td>${row.map((v, k) => `<td class="${typeof v === 'number' ? 'n' : ''}">${cell(v, k)}</td>`).join('')}</tr>`).join('') || `<tr><td colspan="${r.cols.length + 1}">${empty('查询成功，没有符合条件的数据')}</td></tr>`}</tbody></table></div>
    ${r.rows.length > max ? `<div class="small muted" style="margin-top:6px">为保证页面流畅，原型只渲染前 ${max} 行（共 ${nf(r.rows.length)} 行）。<a data-on="qryShowAll">显示全部</a></div>` : ''}
    <details class="pq-d" style="margin-top:10px"><summary>${r.fam === 'mongo' ? '实际执行的语句' : '实际执行的 SQL'}</summary><div><div class="pq-sqlv">${r.fam === 'mongo' ? esc(r.executed) : sqlHl(r.executed)}</div></div></details>`;
}

/* ---- 数据字典 ---- */
const DICT_NOTES = {
  pgsql: ['行数是 PostgreSQL 的统计估算（pg_class.reltuples），尚未统计过的表不显示；PostgreSQL 不记录创建/修改时间，因此没有这两项。'],
  mongo: ['MongoDB 没有固定 schema：字段与类型是对集合采样前 100 个文档推断的，没出现在样本里的字段不会列出；文档数是元数据计数，非精确。没有表注释，也不记录创建/修改时间。'],
  clickhouse: ['ClickHouse 不记录表的创建时间；“更新时间”是最近一次 DDL（元数据文件修改时间）。行数/大小对非 MergeTree 引擎可能为空，显示为“-”。'],
};
const dictKinds = inst => family(inst) === 'mongo' ? [['table', '集合'], ['view', '视图']] : [['table', '表'], ['view', '视图']];
function dictItems(inst, db) {
  const d = findDb(inst, db) || {tables: []}, fam = family(inst);
  const map = t => ({n: t.n, c: t.c || '', t, kindTxt: fam === 'mongo' ? (t.view ? 'view' : 'collection') : fam === 'clickhouse' ? (t.eng || (t.view ? 'View' : 'MergeTree')) : (t.kind === 'matview' ? '物化视图' : t.kind === 'view' ? '视图' : t.part ? '分区表' : '表')});
  return {table: d.tables.filter(t => !t.view || t.kind === 'dict').map(map), view: d.tables.filter(t => t.view && t.kind !== 'dict').map(map)};
}
const sizeTxt = mb => mb == null ? '-' : mb === 0 ? '< 1 MB' : mb >= 1024 * 1024 ? (mb / 1048576).toFixed(1) + ' TB' : mb >= 1024 ? (mb / 1024).toFixed(1) + ' GB' : mb + ' MB';
function dictHeads(inst, kind) { const fam = family(inst); return ['名称', '注释', fam === 'clickhouse' ? '引擎' : '类型', '估算行数', '大小']; }
function dictRow(inst, it) { const t = it.t; return [`<span class="mono">${esc(it.n)}</span>`, esc(it.c), esc(it.kindTxt), t.rows != null && !(t.view && t.kind !== 'dict' && family(inst) !== 'pgsql' && false) && !(t.view && t.kind === 'view') ? `≈ ${nf(t.rows)}` : '-', t.size != null && !t.view ? sizeTxt(t.size) : (t.size != null ? sizeTxt(t.size) : '-')]; }
function ddlOf2(inst, db, t) {
  const fam = family(inst);
  if (fam === 'clickhouse') return chDdl({db, t, cols: t.cols});
  if (fam === 'mongo') return JSON.stringify({note: 'MongoDB 没有 DDL：以下是集合选项与索引定义的 JSON 描述，不是可执行语句', database: db, collection: t.n, type: t.view ? 'view' : 'collection', options: t.view ? {viewOn: 'orders', pipeline: [{$group: {_id: {$dateToString: {format: '%Y-%m-%d', date: '$createdAt'}}, cnt: {$sum: 1}, gmv: {$sum: '$amount'}}}]} : {}, indexes: (t.idx || []).map(x => ({name: x[0], key: Object.fromEntries(x[1].map(c => [c, 1])), unique: !!x[2]}))}, null, 2);
  const [s, n] = t.n.split('.'), q = `${s}.${n}`;
  if (t.view) return `-- 近似 DDL：由 pg_catalog 重建，不保证与 pg_dump 的输出一致（不含属主、权限、表空间、触发器、规则、行级安全策略、统计目标等）。\nCREATE ${t.kind === 'matview' ? 'MATERIALIZED VIEW' : 'VIEW'} ${q} AS\n SELECT ${t.cols.map(c => c.n).join(', ')}\n   FROM ${s}.orders\n  GROUP BY 1;`;
  const pk = (t.idx || []).find(x => /_pkey$/.test(x[0]));
  const cons = (t.idx || []).filter(x => x[2]).map(x => /_pkey$/.test(x[0]) ? `    CONSTRAINT ${x[0]} PRIMARY KEY (${x[1].join(', ')})` : `    CONSTRAINT ${x[0]} UNIQUE (${x[1].join(', ')})`);
  let sql = `-- 近似 DDL：由 pg_catalog 重建，不保证与 pg_dump 的输出一致（不含属主、权限、表空间、触发器、规则、行级安全策略、统计目标等）。\nCREATE TABLE ${q} (\n${t.cols.map(c => `    ${c.n} ${c.t}${c.def === 'identity' ? ' GENERATED ALWAYS AS IDENTITY' : ''}${c.nul ? '' : ' NOT NULL'}${c.def && c.def !== 'identity' && c.def !== '' ? ' DEFAULT ' + c.def : ''}`).concat(cons).join(',\n')}\n)${t.part ? '\nPARTITION BY RANGE (created_at)' : ''};\n`;
  for (const x of (t.idx || []).filter(x => !x[2])) sql += `CREATE INDEX ${x[0]} ON ${q} USING ${/gin/.test(x[0]) ? 'gin' : 'btree'} (${x[1].join(', ')});\n`;
  if (t.c) sql += `COMMENT ON TABLE ${q} IS '${t.c}';\n`;
  for (const c of t.cols) if (c.cmt) sql += `COMMENT ON COLUMN ${q}.${c.n} IS '${c.cmt}';\n`;
  return sql.trimEnd();
}
function dictDetail(inst, db, t) {
  const fam = family(inst), kv = [];
  const sizeMb = t.size || 0;
  if (fam === 'pgsql') kv.push(['类型', t.kind === 'matview' ? '物化视图' : t.view ? '视图' : t.part ? '分区表' : '普通表'], ['属主', `<span class="mono">${esc(t.own || 'postgres')}</span>`], ['访问方法', t.view && t.kind !== 'matview' ? '-' : 'heap'], ['估算行数', t.rows != null && !(t.view && t.kind === 'view') ? `≈ ${nf(t.rows)}` : '-'], ['表大小', t.view && t.kind === 'view' ? '-' : sizeTxt(Math.round(sizeMb * 0.78))], ['索引大小', t.view && t.kind === 'view' ? '-' : sizeTxt(Math.round(sizeMb * 0.22))], ['注释', esc(t.c || '')]);
  else if (fam === 'mongo') kv.push(['类型', t.view ? 'view' : 'collection'], ['集合选项', t.view ? '<span class="mono">viewOn: orders</span>' : '{}'], ['文档数（估计）', t.view ? '-' : `≈ ${nf(t.rows)}`], ['数据大小', t.view ? '-' : sizeTxt(sizeMb)], ['存储大小', t.view ? '-' : sizeTxt(Math.round(sizeMb * 0.46))], ['索引数', t.view ? '-' : String((t.idx || []).length)], ['注释', esc(t.c || '')]);
  else kv.push(['引擎', esc(t.eng || (t.view ? 'View' : 'MergeTree'))], ['完整引擎定义', t.view ? '-' : `<span class="mono small">${esc(t.eng)}${t.part ? ' PARTITION BY ' + t.part : ''} ORDER BY (${t.sk || t.pk || 'tuple()'})</span>`], ['分区键', t.part || '-'], ['排序键', t.sk || '-'], ['主键', t.pk || '-'], ['采样键', '-'], ['行数', t.rows != null && !(t.view && t.kind === 'view') ? `≈ ${nf(t.rows)}` : '-'], ['大小', t.view && t.kind !== 'dict' ? '-' : sizeTxt(sizeMb)], ['最近一次 DDL', '2026-09-18 14:20:07'], ['注释', esc(t.c || '')]);
  const colsT = t.cols.map(c => {
    let extra = '';
    if (fam === 'pgsql') extra = c.def === 'identity' ? 'GENERATED ALWAYS AS IDENTITY' : '';
    if (fam === 'clickhouse') { const pk = String(t.pk || '').split(',').map(s => s.trim()), sk = String(t.sk || '').split(',').map(s => s.trim()); extra = [c.def ? 'DEFAULT' : '', pk.includes(c.n) ? '主键' : '', sk.includes(c.n) ? '排序键' : '', String(t.part || '').includes(c.n) ? '分区键' : ''].filter(Boolean).join('、'); }
    const def = fam === 'mongo' || c.def === 'identity' ? '' : (c.def || '');
    return [`<code>${esc(c.n)}</code>`, `<span class="mono small">${esc(c.t)}</span>`, c.nul || /^Nullable/.test(c.t) ? 'YES' : 'NO', def ? esc(def) : '<span class="muted">-</span>', c.key ? tag(c.key, c.key === 'PRI' ? 'brand' : '') : '', esc(extra), esc(c.cmt || '')];
  });
  const idxT = (t.idx || []).map(x => [`<code>${esc(x[0])}</code>`, x[1].map(c => `<code>${esc(c)}${fam === 'mongo' ? '(' + (/_-1/.test(x[0]) && c !== x[1][0] || /createdAt_-1|ts_-1/.test(x[0]) && x[1].indexOf(c) === x[1].length - 1 && x[1].length > 1 ? '-1' : '1') + ')' : ''}</code>`).join(' , '), x[2] ? '是' : '否', fam === 'pgsql' ? (/gin/.test(x[0]) ? 'gin' : 'btree') : fam === 'clickhouse' ? 'primary' : 'btree']);
  const heads = ['列名', '类型', '可空', '默认值', '键', '额外', '注释'];
  return `${kvList(kv, 'c2')}
    <details class="pq-d" open style="margin-top:10px"><summary>字段（${t.cols.length}）</summary><div>${tbl(heads, colsT)}</div></details>
    ${idxT.length ? `<details class="pq-d" open><summary>索引（${idxT.length}）</summary><div>${tbl(['索引名', '列', '唯一', '类型'], idxT)}</div></details>` : ''}
    <details class="pq-d" ${t.view ? 'open' : ''}><summary>建表语句</summary><div>${code(ddlOf2(inst, db, t), fam === 'mongo' ? '' : 'sql')}</div></details>`;
}
function dictHtmlExport(inst, db) {
  const d = findDb(inst, db); if (!d) return '';
  return `<h2>${esc(db)}</h2><ul>${d.tables.map((t, i) => `<li><a href="#${esc(db)}_${i}">${esc(t.n)}</a> ${esc(t.c || '')}</li>`).join('')}</ul>` + d.tables.map((t, i) =>
    `<h3 id="${esc(db)}_${i}">${esc(t.n)} <span class="s">${esc(t.c || '')}${t.view ? '（视图）' : ` · ≈${nf(t.rows)} 行`}</span></h3><table><tr><th>列名</th><th>类型</th><th>可空</th><th>默认值</th><th>键</th><th>注释</th></tr>${t.cols.map(c => `<tr><td>${esc(c.n)}</td><td>${esc(c.t)}</td><td>${c.nul ? 'YES' : 'NO'}</td><td>${esc(c.def && c.def !== 'identity' ? c.def : '')}</td><td>${esc(c.key)}</td><td>${esc(c.cmt || '')}</td></tr>`).join('')}</table>`).join('');
}

/* ============================================================================
   十一、示例语句（正常 / 规则演示 / 会被拦截）与编辑器提示
   放行 / 拒绝样例取自各引擎的 check_test.go；拒绝原因由上面的移植检查器给出（与引擎原文一致）
   ========================================================================== */
const HINTS = {
  pgsql: {
    placeholder: '在这里写 SQL',
    rule: '只读：SELECT / WITH / VALUES / TABLE / EXPLAIN / SHOW，一次一条',
  },
  mongo: {
    placeholder: '在这里写 mongo shell 语句',
    rule: '只读：shell 风格白名单方法（find / aggregate / count …），一次一条',
  },
  clickhouse: {
    placeholder: '在这里写 SQL',
    rule: '只读：SELECT / SHOW / DESCRIBE / EXISTS / EXPLAIN，一次一条',
  },
};
function samples(inst, db) {
  const fam = family(inst), L = [], add = (g, label, sql) => L.push({g, label, sql});
  const S = inst.schema;
  if (fam === 'pgsql') {
    if (db === 'order_core') {
      add('正常查询', '最近订单（带 schema）', "select id, order_no, user_id, status, amount, created_at\nfrom public.orders\nwhere status = 1\norder by id desc\nlimit 50;");
      add('正常查询', '订单状态分布（GROUP BY）', "select status, count(*) as cnt, sum(amount) as gmv\nfrom public.orders\nwhere created_at >= '2026-10-01'\ngroup by status\norder by cnt desc;");
      add('正常查询', '订单 + 明细联查', "select o.order_no, o.amount, i.sku_name, i.qty, i.price\nfrom public.orders o\njoin public.order_items i on i.order_id = o.id\nwhere o.user_id = 10086\norder by o.id desc\nlimit 20;");
      add('正常查询', 'jsonb 取值（->>）', "select ext->>'device' as device, count(*) as cnt\nfrom public.orders\nwhere created_at >= now() - interval '7 days'\ngroup by 1\norder by cnt desc;");
      add('正常查询', '数组 / 大整数（trace_no 超过 2^53 以字符串返回）', "select id, tags, trace_no\nfrom public.orders\nwhere tags @> array['vip']\nlimit 5;");
      add('正常查询', 'uuid / bytea（bytea 按 \\xHH 显示并提示）', "select id, order_id, filename, size_bytes, digest\nfrom public.attachments\nlimit 5;");
      add('正常查询', '其它 schema：audit.order_log', "select id, order_id, action, operator, created_at\nfrom audit.order_log\nwhere order_id = 18420000\norder by id desc\nlimit 20;");
      add('正常查询', 'CTE：近 14 天每日订单数', "with daily as (\n  select date_trunc('day', created_at) as day, count(*) as cnt\n  from public.orders\n  where created_at >= now() - interval '14 days'\n  group by 1\n)\nselect day, cnt from daily order by day desc;");
      add('正常查询', '视图：每日订单汇总', "select day, cnt, gmv from public.v_order_daily order by day desc limit 10;");
      add('正常查询', '系统视图 information_schema（免表权限）', "select * from information_schema.tables limit 10");
      add('正常查询', 'pg_stat_user_tables（未写 schema，按 public 校验，表级授权的用户会被拒）', "select schemaname, relname, n_live_tup from pg_stat_user_tables order by n_live_tup desc limit 20");
      add('正常查询', 'EXPLAIN（作为普通语句运行，没有执行计划页签）', "explain select * from public.orders where id = 1");
      add('正常查询', 'SHOW 参数', "show server_version");
      add('规则演示', '没带 LIMIT（自动补 LIMIT n+1）', "select id, order_no, amount\nfrom public.orders\nwhere created_at >= '2026-10-07';");
      add('规则演示', 'LIMIT 超过行数上限（不改写，客户端按上限截断）', "select id, order_no, amount from public.orders limit 100000;");
      add('规则演示', '无表查询（行数上限 100）', "select now(), version();");
      add('规则演示', '超长单元格（超过 64KB 截断）', "select repeat('x', 100000) as big;");
      add('规则演示', '调用自定义函数（FROM 里的非内置函数，要求库级权限）', "select * from public.my_report_fn(30) as r;");
      add('规则演示', '没有 public.attachments 的表权限时会被拒绝', "select * from public.attachments limit 5;");
      add('规则演示', '跨库引用（三段式名称，库不一致）', "select * from other_db.public.orders limit 1;");
      add('规则演示', '服务端报错：表不存在', "select * from public.no_such_table;");
      add('规则演示', '服务端报错：列不存在', "select idd from public.orders limit 1;");
      add('规则演示', '超时（statement_timeout）：generate_series 20 亿行', "select count(*)\nfrom generate_series(1, 2000000000);");
      add('规则演示', '超时：大表自连接（笛卡尔积）', "select count(*) from public.orders a, public.orders b;");
      add('会被拦截', 'INSERT（语句类型）', "insert into public.orders (order_no) values ('x');");
      add('会被拦截', 'UPDATE', "update public.orders set status = 4 where id = 1;");
      add('会被拦截', 'DROP TABLE（DDL）', "drop table public.orders;");
      add('会被拦截', 'pg_sleep（危险函数）', "select pg_sleep(5);");
      add('会被拦截', 'pg_read_file（读服务器文件）', "select pg_read_file('/etc/passwd');");
      add('会被拦截', 'set_config（改会话配置）', "select set_config('default_transaction_read_only', 'off', false);");
      add('会被拦截', 'pg_authid（敏感系统视图）', "select * from pg_authid;");
      add('会被拦截', 'EXPLAIN ANALYZE（会真正执行）', "explain analyze select 1;");
      add('会被拦截', '多语句', "select 1; select 2;");
      add('会被拦截', 'FOR UPDATE（行锁）', "select * from public.orders where id = 1 for update;");
      add('会被拦截', '可写 CTE', "with d as (delete from public.orders returning *) select * from d;");
      add('会被拦截', 'SELECT … INTO（建表）', "select * into public.orders_bak from public.orders;");
      add('会被拦截', 'COPY', "copy public.orders to '/tmp/orders.csv';");
      add('会被拦截', 'COMMIT（事务控制）', "commit;");
    } else if (db === 'order_report') {
      add('正常查询', '每日销售汇总', "select day, channel, orders, gmv, refund\nfrom report.daily_sales\norder by day desc, channel\nlimit 20;");
      add('正常查询', 'SKU 近 30 天销量 Top', "select sku_id, sku_name, sales_30d, stock\nfrom report.sku_stat\norder by sales_30d desc\nlimit 20;");
      add('正常查询', '按渠道汇总', "select channel, sum(orders) as orders, sum(gmv) as gmv\nfrom report.daily_sales\nwhere day >= date '2026-09-01'\ngroup by channel\norder by gmv desc;");
      add('规则演示', '没带 LIMIT（自动补）', "select day, channel, gmv from report.daily_sales;");
      add('会被拦截', 'TRUNCATE', "truncate table report.daily_sales;");
      add('会被拦截', 'pg_stat_activity（含其它会话的 SQL）', "select * from pg_stat_activity;");
    } else if (db === 'bi_dw') {
      add('正常查询', '每日订单数与成交额', "select date_key, count(*) as orders, sum(amount) as gmv\nfrom dw.fact_orders\nwhere date_key >= 20261001\ngroup by date_key\norder by date_key;");
      add('正常查询', '事实表 + 用户维度', "select u.city, count(*) as orders, sum(f.amount) as gmv\nfrom dw.fact_orders f\njoin dw.dim_user u on u.user_key = f.user_key\nwhere f.date_key >= 20261001\ngroup by u.city\norder by orders desc\nlimit 10;");
      add('正常查询', '日期维度（小表）', "select date_key, day, week_no, is_workday from dw.dim_date order by date_key desc limit 10;");
      add('规则演示', '没带 LIMIT（自动补）', "select order_id, user_key, amount from dw.fact_orders where date_key = 20261007;");
      add('会被拦截', 'DELETE', "delete from dw.fact_orders where date_key < 20250101;");
      add('会被拦截', 'pg_ls_dir（列服务器目录）', "select pg_ls_dir('/var/lib/postgresql');");
    } else if (db === 'bi_ods') {
      add('正常查询', '原始报文（jsonb）', "select id, payload->>'order_no' as order_no, payload->'amount' as amount, source, loaded_at\nfrom ods.orders_raw\norder by id desc\nlimit 10;");
      add('正常查询', '按来源统计', "select source, count(*) as cnt from ods.orders_raw where loaded_at >= now() - interval '1 day' group by source;");
      add('会被拦截', 'dblink（外部连接）', "select * from dblink('host=10.0.0.1', 'select 1') as t(a int);");
    }
  } else if (fam === 'mongo') {
    const dbk = db;
    if (db === 'order_mongo') {
      add('正常查询', '列出集合', 'db.getCollectionNames()');
      add('正常查询', '最近的已支付订单', 'db.orders.find({status: 1}).sort({_id: -1}).limit(20)');
      add('正常查询', '按用户查订单（含嵌套明细，嵌套文档显示为紧凑 JSON）', 'db.orders.find({userId: 10086}).sort({createdAt: -1}).limit(10)');
      add('正常查询', '按订单号查（findOne）', 'db.orders.findOne({orderNo: "O2026100612345678"})');
      add('正常查询', '投影：只取部分字段', 'db.orders.find({}, {orderNo: 1, amount: 1, createdAt: 1}).limit(10)');
      add('正常查询', '正则 + 日期条件', 'db.orders.find({orderNo: /^O20261006/, createdAt: {$gte: ISODate("2026-10-06")}}).limit(5)');
      add('正常查询', '统计：countDocuments', 'db.orders.countDocuments({status: 1})');
      add('正常查询', '去重：distinct', 'db.orders.distinct("payChannel")');
      add('正常查询', '聚合：各渠道成交额', 'db.orders.aggregate([{$match: {status: 1}}, {$group: {_id: "$payChannel", n: {$sum: 1}, gmv: {$sum: "$amount"}}}, {$sort: {n: -1}}, {$limit: 10}])');
      add('正常查询', '聚合：$lookup 同库集合（订单事件数）', 'db.orders.aggregate([{$match: {userId: 10086}}, {$lookup: {from: "order_events", localField: "_id", foreignField: "orderId", as: "events"}}, {$project: {orderNo: 1, amount: 1, events: {$size: "$events"}}}, {$limit: 5}])');
      add('正常查询', '订单事件：最近的支付事件', 'db.order_events.find({type: "pay"}).sort({_id: -1}).limit(10)');
      add('正常查询', '索引：getIndexes', 'db.orders.getIndexes()');
      add('正常查询', '集合统计：stats（库级之外的集合级信息）', 'db.orders.stats()');
      add('正常查询', '库统计：db.stats()（需要库级权限）', 'db.stats()');
      add('规则演示', '没带 limit（结果最多返回 N 条，给提示）', 'db.orders.find({status: 1})');
      add('规则演示', 'limit 超过行数上限（自动收窄）', 'db.orders.find({}).limit(100000)');
      add('规则演示', '被忽略的选项（allowDiskUse 提示）', 'db.orders.aggregate([{$match: {status: 1}}, {$limit: 5}], {allowDiskUse: true})');
      add('规则演示', 'find().count() 会忽略链上的 sort / limit', 'db.orders.find({status: 1}).sort({_id: -1}).limit(5).count()');
      add('规则演示', '没有索引的排序（服务端内存不足，禁止使用磁盘）', 'db.orders.find({status: 1}).sort({amount: -1})');
      add('规则演示', '无权限的集合（需要先申请）', 'db.coupons.find({enabled: true}).limit(10)');
      add('会被拦截', 'insertOne（写操作）', 'db.orders.insertOne({a: 1})');
      add('会被拦截', 'drop（管理操作）', 'db.orders.drop()');
      add('会被拦截', 'runCommand（原始命令）', 'db.runCommand({ping: 1})');
      add('会被拦截', '$where（服务端 JavaScript）', 'db.orders.find({$where: "this.a > 1"})');
      add('会被拦截', '$out（写集合）', 'db.orders.aggregate([{$out: "orders_copy"}])');
      add('会被拦截', '$merge（写集合）', 'db.orders.aggregate([{$merge: {into: "x"}}])');
      add('会被拦截', 'getSiblingDB（切库）', 'db.getSiblingDB("admin").x.find()');
      add('会被拦截', 'system.* 集合', 'db.system.users.find()');
      add('会被拦截', '$lookup 跨库写法', 'db.orders.aggregate([{$lookup: {from: {db: "other", coll: "x"}, localField: "a", foreignField: "b", as: "c"}}])');
      add('会被拦截', '不支持的游标方法 hint', 'db.orders.find({}).hint({_id: 1})');
      add('会被拦截', '多语句', 'db.orders.find({status: 1}).limit(1); db.carts.find({}).limit(1)');
    } else if (db === 'app_log') {
      add('正常查询', '最近的错误事件', 'db.error_events.find({level: "ERROR"}).sort({_id: -1}).limit(10)');
      add('正常查询', '按 traceId 查接口日志', 'db.api_logs.find({traceId: "9f3a1c52b7d84e06"}).limit(10)');
      add('正常查询', '聚合：各服务错误数（先 $match 缩小范围）', 'db.error_events.aggregate([{$match: {ts: {$gte: ISODate("2026-10-06")}}}, {$group: {_id: "$service", n: {$sum: 1}}}, {$sort: {n: -1}}])');
      add('规则演示', '4 亿文档的集合全量分组（超过执行时间上限）', 'db.api_logs.aggregate([{$group: {_id: "$path", n: {$sum: 1}}}])');
      add('规则演示', '估计文档数（元数据计数，不扫描）', 'db.api_logs.estimatedDocumentCount()');
      add('会被拦截', 'deleteMany', 'db.api_logs.deleteMany({})');
    } else if (db === 'user_mongo') {
      add('正常查询', '高等级用户（手机号等字段明文返回，不脱敏）', 'db.users.find({level: {$gte: 4}}).sort({_id: -1}).limit(10)');
      add('正常查询', '按 uid 查用户', 'db.users.find({uid: 10086})');
      add('正常查询', '用户最近的会话', 'db.user_sessions.find({uid: 10086}).sort({loginAt: -1}).limit(5)');
      add('正常查询', '聚合：各等级用户数', 'db.users.aggregate([{$group: {_id: "$level", n: {$sum: 1}}}, {$sort: {_id: 1}}])');
      add('正常查询', '集合列表', 'db.getCollectionNames()');
      add('规则演示', '没带 limit', 'db.users.find({level: 5})');
      add('会被拦截', 'updateMany', 'db.users.updateMany({}, {$set: {level: 5}})');
      add('会被拦截', 'admin 库的 system.users', 'db.getSiblingDB("admin").system.users.find()');
    } else if (db === 'user_behavior') {
      add('正常查询', '某用户最近的浏览', 'db.page_views.find({uid: 10086}).sort({ts: -1}).limit(10)');
      add('正常查询', '搜索词记录', 'db.search_history.find({uid: 10086}).sort({ts: -1}).limit(10)');
      add('规则演示', '9 亿文档全量分组（超时）', 'db.page_views.aggregate([{$group: {_id: "$page", n: {$sum: 1}}}])');
      add('规则演示', '没有索引的排序（内存不足）', 'db.page_views.find({}).sort({stayMs: -1})');
      add('会被拦截', 'dropIndex', 'db.page_views.dropIndex("uid_1_ts_-1")');
    }
  } else if (fam === 'clickhouse') {
    if (db === 'olap_order') {
      add('正常查询', '版本', 'select version()');
      add('正常查询', '近 7 天每日订单数与成交额', "select order_date, count() as orders, sum(amount) as gmv\nfrom fact_orders\nwhere order_date >= today() - 7\ngroup by order_date\norder by order_date");
      add('正常查询', '分时订单（toStartOfHour）', "select toStartOfHour(created_at) as hour, count() as orders\nfrom fact_orders\nwhere order_date = today()\ngroup by hour\norder by hour");
      add('正常查询', '按用户查订单（Decimal / UInt64 以字符串返回）', "select order_id, user_id, status, amount, channel, city, created_at, tags\nfrom fact_orders\nwhere user_id = 10086\norder by order_id desc\nlimit 20");
      add('正常查询', '事件类型分布（Enum8）', "select event_type, count() as cnt\nfrom order_events\nwhere event_date >= today() - 3\ngroup by event_type\norder by cnt desc");
      add('正常查询', 'Map 取值', "select props['channel'] as channel, count() as cnt\nfrom order_events\nwhere event_date = today()\ngroup by channel");
      add('正常查询', '订单 + 事件联查', "select o.order_id, o.amount, e.event_type, e.event_time\nfrom fact_orders o\njoin order_events e on e.order_id = o.order_id\nwhere o.user_id = 10086\norder by e.event_time desc\nlimit 20");
      add('正常查询', '字典函数 dictGet', "select dictGet('olap_order.dict_city', 'city_name', toUInt64(110000)) as city");
      add('正常查询', '视图：每日订单汇总', "select * from v_order_daily order by order_date desc limit 7");
      add('正常查询', 'SHOW TABLES', 'show tables from olap_order');
      add('正常查询', 'DESCRIBE', 'describe olap_order.fact_orders');
      add('正常查询', 'SHOW CREATE TABLE（真实 DDL）', 'show create table olap_order.fact_orders');
      add('正常查询', 'EXISTS', 'exists table olap_order.fact_orders');
      add('正常查询', 'numbers(N) 表函数', 'select * from numbers(10)');
      add('正常查询', '系统表：system.tables（只开放摘要类）', "select database, name, engine, total_rows\nfrom system.tables\nwhere database = 'olap_order'\norder by total_rows desc\nlimit 10");
      add('正常查询', 'EXPLAIN', 'explain select count() from olap_order.fact_orders');
      add('规则演示', '没带 LIMIT（不改写，服务端按行数上限截断）', "select order_id, amount, channel\nfrom fact_orders\nwhere order_date = today()");
      add('规则演示', '末尾 FORMAT 被忽略（给提示）', "select count() from fact_orders format JSON");
      add('规则演示', '读取行数超过限制（max_rows_to_read）', "select status, count() as cnt\nfrom fact_orders\nwhere status = 1\ngroup by status");
      add('规则演示', '服务端报错：表不存在', 'select * from no_such_table');
      add('规则演示', '服务端报错：未知列', 'select no_such_col from fact_orders limit 1');
      add('规则演示', '没有 olap_user.user_visits 表权限时被拒', 'select visit_date, count() from olap_user.user_visits where visit_date = today() group by visit_date');
      add('会被拦截', 'system.users（system 库只开放摘要类表）', 'select * from system.users');
      add('会被拦截', 'SETTINGS（覆盖只读 / 限额）', 'select 1 settings readonly = 0');
      add('会被拦截', 'remote（连其它服务器）', "select * from remote('host', db, t)");
      add('会被拦截', 'file（读服务器文件）', "select * from file('/etc/passwd', 'CSV', 'a String')");
      add('会被拦截', 'INSERT', 'insert into fact_orders values (1)');
      add('会被拦截', 'OPTIMIZE', 'optimize table fact_orders');
      add('会被拦截', 'sleep（危险函数）', 'select sleep(3)');
      add('会被拦截', 'merge()（无法静态确定涉及的表）', "select * from merge('olap_order', '^fact')");
      add('会被拦截', '外部引擎表（执行前引擎核对）', 'select * from ext_orders_mysql');
      add('会被拦截', 'USE（库在页面选）', 'use default');
      add('会被拦截', '多语句', 'select 1; select 2');
    } else if (db === 'olap_user') {
      add('正常查询', '某日访问量按页面', "select page, count() as pv, uniq(user_id) as uv\nfrom user_visits\nwhere visit_date = today()\ngroup by page\norder by pv desc");
      add('正常查询', '转化漏斗日汇总（小表）', "select stat_date, step, users, rate\nfrom user_funnel\norder by stat_date desc\nlimit 20");
      add('规则演示', '读取行数超过限制（88 亿行全表聚合）', 'select user_id, count() from user_visits group by user_id limit 10');
      add('会被拦截', 'DROP TABLE', 'drop table user_visits');
    } else if (db === 'dw_report') {
      add('正常查询', '某日事件统计（按分区键过滤）', "select name, count() as cnt\nfrom events_raw\nwhere event_date = today()\ngroup by name\norder by cnt desc");
      add('正常查询', '用户维度', "select user_id, level, city, reg_date from dim_user order by user_id desc limit 10");
      add('正常查询', '字典：渠道', 'select * from dict_channel');
      add('规则演示', '读取行数超过限制（124 亿行全表去重）', 'select uniqExact(user_id) from events_raw');
      add('会被拦截', 'SYSTEM 命令', 'system flush logs');
    }
  }
  if (!L.length) { const t = ((findDb(inst, db) || {}).tables || [])[0]; if (t) add('正常查询', '示例：' + t.n, exampleFor(inst, db, { n: t.n, cols: t.cols })); }
  return L;
}

/* ============================================================================
   十、导出
   ========================================================================== */
window.EngineQuery = {
  isExt: inst => !!family(inst), family, label: inst => LABEL[family(inst)] || '', dbs: dbsOfInst,
  check, run, treeData, exampleFor, resultHtml, TRUNC_NOTE,
  dictKinds, dictItems, dictHeads, dictRow, dictDetail, dictHtmlExport, DICT_NOTES, notes: DICT_NOTES, sizeTxt, ddlOf: ddlOf2,
  kindIco: KIND_ICO, kindTxt: KIND_TXT,
  samples: (inst, db) => samples(inst, db),
  hints: inst => HINTS[family(inst)],
  READ_VIEW_MSG: inst => `该类型（${LABEL[family(inst)]}）不支持只读视图模式，请联系管理员去掉实例的 read_view 标签，或改用 MySQL 系实例。`,
  BUSY_MSG: '你同时执行的 PostgreSQL / MongoDB / ClickHouse 查询已达 3 条，请等待其中一条完成后再试',
  UNMASKED: '该类型的查询结果不做脱敏，请勿查看敏感数据',
  /* 测试 / 调试用 */
  _PG: PG, _CH: CH, _MG: MG, _parse: (s, dia) => new Parser(s, dia).parseStatement(), _tdOf: tdOf,
};
})();
