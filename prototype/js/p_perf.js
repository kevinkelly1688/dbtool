/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_perf.js
 * @time: 2026/10/28
 */
/* 性能工单：工单列表 / 详情（perf）、团队排行榜（perfboard）、采集配置（perfcollect）、团队配置（perfteam）、SLA 策略（perfsla）
   状态机与规则以 internal/perf 为准：待分派 → 待受理 → 处理中 → 待验证 → 已关闭；豁免审批中；SLA 只在前三个状态计时。 */
(function () {
  'use strict';

  /* ---------- 时间：以“今天 = 2026-10-07、当前时分 = 打开页面时的时分”为基准 ---------- */
  const H = 3600e3, DAY = 24 * H;
  const T0 = Date.parse(`${TODAY}T${now().slice(11)}:00`), L0 = Date.now();
  const nowMs = () => T0 + (Date.now() - L0);
  const ago = h => T0 - h * H;
  const tsOf = ms => { const d = new Date(ms); return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`; };
  const dateOf = ms => tsOf(ms).slice(0, 10);
  const durText = ms => {
    const min = Math.max(1, Math.round(Math.abs(ms) / 60e3)), h = Math.floor(min / 60);
    if (min >= 48 * 60) return `${Math.floor(h / 24)} 天 ${h % 24} 小时`;
    if (h >= 1) return `${h} 小时 ${min % 60} 分钟`;
    return `${min} 分钟`;
  };
  const fmtSec = s => s >= 3600 ? (s / 3600).toFixed(1) + ' h' : s >= 60 ? (s / 60).toFixed(1) + ' min' : s + ' s';
  const DATE_ST = 'background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:6px 10px';

  /* ---------- 基础字典 ---------- */
  const ST = {new: ['待分派', 'warn'], assigned: ['待受理', 'info'], accepted: ['处理中', 'brand'], fixed: ['待验证', 'ok'], waive_pending: ['豁免审批中', 'warn'], closed: ['已关闭', '']};
  const RUNNING = ['new', 'assigned', 'accepted'];
  const LV_NAME = {P0: '紧急', P1: '高', P2: '中', P3: '低'}, LV_RANK = {P0: 0, P1: 1, P2: 2, P3: 3};
  const CAT = {
    noidx: {n: '缺少索引', c: 'var(--c5)'}, idxfail: {n: '索引失效', c: 'var(--c3)'}, likefront: {n: '前模糊 LIKE', c: 'var(--c4)'},
    deeppage: {n: '深分页', c: 'var(--c1)'}, bigres: {n: '返回结果集过大', c: 'var(--c2)'}, sortgrp: {n: '排序/分组/临时表', c: 'var(--c6)'},
    olap: {n: '分析型大查询', c: 'var(--c1)'}, subq: {n: '相关子查询', c: 'var(--c4)'}, inlist: {n: 'IN 列表过长', c: 'var(--c3)'}, orderrand: {n: 'ORDER BY RAND()', c: 'var(--c6)'},
  };
  const CAT_S = {noidx: '缺索引', idxfail: '索引失效', likefront: '前模糊', deeppage: '深分页', bigres: '大结果集', sortgrp: '排序分组', olap: '分析查询', subq: '子查询', inlist: 'IN 过长', orderrand: 'RAND'};
  const SRC = {slowlog: '平台慢日志', perf_schema: 'performance_schema', pt_digest: 'pt-query-digest', manual: '手工建单', slowpage: '慢日志页面建单'};
  const SIDE = {dev: '研发处理', dba: 'DBA 处理'};
  const sevTag = l => `<span class="tag ${l.toLowerCase()}">${l}</span>`;
  const stTag = i => { const [t, c] = ST[i.st]; return `<span class="tag ${c}">${t}</span>${i.pendExt ? ' <span class="tag warn">延期审批中</span>' : ''}`; };
  const uName = l => { const u = USERS.find(x => x.login === l); return u ? u.name : (l === 'system' ? '系统' : (l || '—')); };
  const uCell = l => l ? `${esc(uName(l))} <span class="muted small">${esc(l)}</span>` : '<span class="muted">—</span>';
  const userHas = (u, code) => u.super || (!BTNS[code].locked && !u.extraDeny.includes(code) && (u.extraGrant.includes(code) || u.groups.some(g => ((GROUPS.find(x => x.id === g) || {perms: []}).perms).includes(code))));
  const handlers = () => USERS.filter(u => u.status === 'active' && userHas(u, 'perf.handle'));
  const instName = id => instOf(id).name;
  const opts = (list, sel) => list.map(([v, t]) => `<option value="${esc(v)}" ${String(v) === String(sel) ? 'selected' : ''}>${esc(t)}</option>`).join('');
  const rgOpts = (sel, withEmpty) => (withEmpty ? `<option value="">${withEmpty}</option>` : '') + RGROUPS.filter(r => r.enabled).map(r => `<option value="${r.id}" ${r.id == sel ? 'selected' : ''}>${esc(r.name)}</option>`).join('');
  const isDba = () => ME.super || can('perf.assign');
  const hash8 = s => { let h = 2166136261; for (let k = 0; k < s.length; k++) h = Math.imul(h ^ s.charCodeAt(k), 16777619); return (h >>> 0).toString(16).padStart(8, '0') + (Math.imul(h, 2654435761) >>> 0).toString(16).padStart(8, '0'); };

  /* ---------- SLA 策略（dbt_perf_sla_policy）与升级规则 ---------- */
  let SLA = {
    P0: {accept: 4, resolve: 48, l3: 24, remind: 4, ext: 0}, P1: {accept: 24, resolve: 168, l3: 72, remind: 24, ext: 1},
    P2: {accept: 72, resolve: 336, l3: 168, remind: 48, ext: 1}, P3: {accept: 72, resolve: 720, l3: 336, remind: 72, ext: 2},
  };
  let ESC = [
    {lv: 'L1', name: '催办', when: '未受理超过「受理时限」', to: {owner: 1, tl: 0, dba: 0, director: 0, global: 0}},
    {lv: 'L2', name: '升级', when: '超过「解决时限」', to: {owner: 1, tl: 1, dba: 1, director: 0, global: 0}},
    {lv: 'L3', name: '升级总监', when: '超过解决时限 + 「超时后升级总监」小时', to: {owner: 1, tl: 1, dba: 1, director: 1, global: 1}},
  ];
  const TO_NAMES = {owner: '负责人', tl: '团队 TL', dba: '跟进 DBA', director: '总监', global: '全局渠道'};
  const SEV_RULES = [['P0', '≥ 3600 s', '≥ 30%', '≥ 10000 ms 且 ≥ 1000 次'], ['P1', '≥ 600 s', '≥ 10%', '≥ 5000 ms 且 ≥ 100 次'], ['P2', '≥ 60 s', '≥ 3%', '≥ 3000 ms 且 ≥ 10 次']];
  const evalLevel = m => {
    const w = m.avgMs * m.exec / 1000;
    if (w >= 3600 || m.pct >= 30 || (m.avgMs >= 10000 && m.exec >= 1000)) return 'P0';
    if (w >= 600 || m.pct >= 10 || (m.avgMs >= 5000 && m.exec >= 100)) return 'P1';
    if (w >= 60 || m.pct >= 3 || (m.avgMs >= 3000 && m.exec >= 10)) return 'P2';
    return 'P3';
  };

  /* ---------- 团队配置与路由规则 ---------- */
  const PTEAMS = {
    1: {owner: 'rd_zhao', tls: ['tl_wang'], dirs: ['admin'], rank: true, remark: '订单主站研发'},
    2: {owner: 'rd_zhou', tls: ['tl_wang'], dirs: ['admin'], rank: true, remark: '支付核心链路'},
    3: {owner: '', tls: ['dba_li'], dirs: ['admin'], rank: true, remark: '暂无在岗研发，由 DBA 兜底'},
    4: {owner: 'dba_li', tls: ['dba_li'], dirs: ['admin'], rank: true, remark: ''},
    5: {owner: '', tls: [], dirs: [], rank: false, remark: '资源组已停用，不参与排行与周报'},
  };
  let ROUTES = [
    {id: 1, inst: 1, db: 'order_.*', tbl: '.*', team: 1, owner: 'rd_zhao'},
    {id: 2, inst: 3, db: 'pay_.*', tbl: '(payments|refunds)', team: 2, owner: 'rd_zhou'},
    {id: 3, inst: 0, db: 'user_db', tbl: 'login_log|users', team: 3, owner: ''},
    {id: 4, inst: 0, db: 'dw|report.*|order_archive', tbl: '.*', team: 4, owner: 'dba_li'},
    {id: 5, inst: 7, db: '.*', tbl: '.*', team: 2, owner: 'rd_zhou'},
  ];
  let routeSeq = 5;
  const fullMatch = (pat, v) => { try { return new RegExp('^(?:' + pat + ')$').test(v); } catch (e) { return false; } };
  /* 路由顺序：路由规则（从上到下首个命中）→ 团队默认接口人；都没有则待分派 */
  function routeFor(inst, db, tbl) {
    const r = ROUTES.find(x => (!x.inst || x.inst == inst) && fullMatch(x.db, db || '') && fullMatch(x.tbl, tbl || ''));
    if (r) return {team: r.team, owner: r.owner || (PTEAMS[r.team] || {}).owner || '', rule: r};
    const team = (instOf(inst).rgs || [])[0] || 0;
    return {team, owner: (PTEAMS[team] || {}).owner || '', rule: null};
  }

  /* ---------- 诊断文案 / 执行计划 ---------- */
  function diagOf(i) {
    const t = '`' + i.table + '`', m = i.m, cols = i.idxCols || ['id'];
    const D = {
      noidx: {sum: [`${t} 的过滤 / 排序条件没有可用索引，执行计划为全表扫描，平均扫描 ${fmt(m.scan)} 行、返回 ${fmt(m.rows)} 行`, '扫描 / 返回比过高，属于“纯缺少索引”，由 DBA 评估后处理'],
        adv: [`新增联合索引 (${cols.join(', ')})：等值条件列在前，范围 / 排序列在后`, '建议低峰通过「SQL 上线」执行，大表使用在线改表（gh-ost / pt-osc）', '上线后观察 3 天，平均耗时应下降 50% 以上'],
        ddl: `ALTER TABLE \`${i.db}\`.\`${i.table}\` ADD INDEX \`idx_${cols.join('_')}\` (${cols.map(c => '`' + c + '`').join(', ')}), ALGORITHM=INPLACE, LOCK=NONE;`},
      idxfail: {sum: ['条件列上使用了函数 / 隐式类型转换 / OR 组合，已有索引无法生效，退化为全表扫描', `平均扫描 ${fmt(m.scan)} 行，仅返回 ${fmt(m.rows)} 行`], adv: ['把函数移到常量一侧，改写为范围条件（如 created_at >= ? AND created_at < ?）', '字符串列与数字比较时统一参数类型（加引号）', 'OR 条件可改为 UNION ALL 分别走索引']},
      likefront: {sum: ['LIKE 使用前导通配符（%xxx），无法使用 B+ 树索引，只能全表或全索引扫描', `平均扫描 ${fmt(m.scan)} 行`], adv: ['改为前缀匹配（xxx%），或把检索需求迁到 ES / 全文索引', '确需子串检索时增加 ngram 全文索引（FULLTEXT ... WITH PARSER ngram）']},
      deeppage: {sum: [`LIMIT 偏移量很大，需要先扫描并丢弃大量行再返回少量记录（平均扫描 ${fmt(m.scan)} 行，返回 ${fmt(m.rows)} 行）`], adv: ['改为游标分页：WHERE id > 上一页最大 id ORDER BY id LIMIT N', '或先用覆盖索引查出主键，再回表（延迟关联）', '前端限制最大页码']},
      bigres: {sum: [`单次返回 ${fmt(m.rows)} 行、SELECT * 取全部列，网络与内存开销大`], adv: ['只查询需要的列，增加时间范围与 LIMIT', '大批量导出请使用「数据导出」，不要走在线业务接口']},
      sortgrp: {sum: ['GROUP BY / ORDER BY 无法利用索引顺序，使用了临时表与文件排序', `平均扫描 ${fmt(m.scan)} 行`], adv: ['为分组 / 排序列建立联合索引，使其有序', '热点统计改为定时汇总表或异步计算']},
      olap: {sum: ['分析型大查询，一次扫描大表并聚合，占用主库 IO 与 CPU', `平均耗时 ${fmtMs(m.avgMs)}，平均扫描 ${fmt(m.scan)} 行`], adv: ['迁移到数仓 / 只读分析实例执行，错开业务高峰', '按时间分区裁剪扫描范围，或预聚合']},
      subq: {sum: ['相关子查询对外层每一行都执行一次，驱动表越大越慢'], adv: ['改写为 JOIN 或 EXISTS 半连接，并确认关联列上有索引']},
      inlist: {sum: ['IN 列表元素过多，优化器估算偏差，且 SQL 文本过长导致解析开销大'], adv: ['分批查询（每批 ≤ 500），或把条件写入临时表后 JOIN']},
      orderrand: {sum: ['ORDER BY RAND() 会对全表生成随机数并排序，无法使用索引'], adv: ['改为应用层随机 id，或 WHERE id >= 随机值 LIMIT 1']},
    };
    return D[i.cat];
  }
  function explainOf(i) {
    const tb = findTable(i.inst, i.db, i.table), rows = tb ? tb.rows : 1000000, e = (o) => Object.assign({id: 1, sel: 'SIMPLE', table: i.table, type: 'ALL', key: '', rows, filt: '10.00', extra: 'Using where'}, o);
    switch (i.cat) {
      case 'noidx': return [e({extra: /order by|group by/i.test(i.sql) ? 'Using where; Using filesort' : 'Using where'})];
      case 'idxfail': return [e({})];
      case 'likefront': return [e({type: 'index', key: 'PRIMARY', filt: '11.11'})];
      case 'deeppage': return [e({type: 'index', key: 'PRIMARY', rows: Math.min(rows, i.m.scan), filt: '100.00', extra: 'Backward index scan'})];
      case 'sortgrp': return [e({extra: 'Using where; Using temporary; Using filesort'})];
      case 'olap': return [e({extra: 'Using temporary'})];
      case 'subq': return [e({table: i.table, filt: '100.00', extra: 'Using where'}), e({id: 2, sel: 'DEPENDENT SUBQUERY', table: 'orders', type: 'ref', key: 'idx_status', rows: 5200, filt: '100.00', extra: 'Using where'})];
      case 'inlist': return [e({type: 'range', key: 'idx_pay', rows: i.m.scan, filt: '100.00', extra: 'Using index condition'})];
      case 'orderrand': return [e({extra: 'Using temporary; Using filesort'})];
      default: return [e({type: 'range', key: 'idx_created', rows: i.m.scan, filt: '100.00'})];
    }
  }
  const fingerprint = sql => sql.replace(/--[^\n]*|\/\*[\s\S]*?\*\//g, ' ').replace(/'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"/g, '?').replace(/\b\d+(?:\.\d+)?\b/g, '?')
    .replace(/\bin\s*\(\s*\?(?:\s*,\s*\?)*\s*\)/gi, 'in (?)').replace(/\s+/g, ' ').trim().replace(/;$/, '').toLowerCase();
  function classify(sql) {
    const s = sql.replace(/\s+/g, ' ');
    if (/like\s+['"]%/i.test(s)) return 'likefront';
    if (/order\s+by\s+rand\s*\(/i.test(s)) return 'orderrand';
    if (/limit\s+\d{4,}\s*,/i.test(s) || /offset\s+\d{4,}/i.test(s)) return 'deeppage';
    if (/\bin\s*\((\s*\d+\s*,){30,}/i.test(s)) return 'inlist';
    if (/\bwhere\b[^;]*\b(date|lower|upper|year|month|date_format|ifnull|substring)\s*\(\s*`?[a-z_]+/i.test(s) || /\bwhere\b[^;]*\bor\b/i.test(s)) return 'idxfail';
    if (/\bexists\s*\(\s*select|\bin\s*\(\s*select/i.test(s)) return 'subq';
    if (/group\s+by|order\s+by/i.test(s)) return 'sortgrp';
    if (/select\s+\*/i.test(s) && !/\bwhere\b/i.test(s)) return 'bigres';
    return 'noidx';
  }
  const tableOf = sql => { const m = /\bfrom\s+`?([a-z_][\w]*)`?/i.exec(sql); return m ? m[1] : ''; };

  /* ---------- 种子工单 ---------- */
  /* age/acc/fix/closed：距今小时数；plan：承诺完成日（距今天数）；m：[平均ms, 最大ms, 日均次数, 平均扫描行, 平均返回行, 占比%] */
  const SEED = [
    {id: 61, lv: 'P0', st: 'new', side: 'dba', team: 1, inst: 1, db: 'order_db', table: 'orders', cat: 'noidx', src: 'slowlog', idx: ['status', 'updated_at'], age: 2.5, m: [4120, 38200, 560, 2600000, 200, 34],
      sql: "SELECT id, order_no, user_id, amount FROM orders WHERE status = 2 AND updated_at < '2026-10-04 00:00:00' ORDER BY updated_at LIMIT 200"},
    {id: 60, lv: 'P1', st: 'assigned', side: 'dev', owner: 'rd_zhao', dba: 'dba_li', team: 1, inst: 1, db: 'order_db', table: 'orders', cat: 'idxfail', src: 'slowlog', age: 20, m: [1850, 9600, 380, 1100000, 36, 12],
      sql: "SELECT id, order_no, amount, status FROM orders WHERE DATE(created_at) = '2026-10-05' AND status = 1",
      cm: [[18, 'dba_li', '这条是昨天对账脚本带出来的，DATE(created_at) 让 idx_user_created 用不上，麻烦看下能否改成范围条件。']]},
    {id: 59, lv: 'P0', st: 'accepted', side: 'dev', owner: 'rd_zhao', dba: 'dba_li', team: 1, inst: 1, db: 'order_db', table: 'order_items', cat: 'likefront', src: 'slowlog', age: 60, acc: 55, plan: -1, m: [3210, 41200, 1340, 5100000, 20, 38.2],
      sql: "SELECT id, order_id, sku_id, qty, price FROM order_items WHERE sku_name LIKE '%无线耳机%' ORDER BY id DESC LIMIT 20",
      cm: [[30, 'rd_zhao', '已定位到商品搜索接口，正在评估改用 ES 检索，预计今天内给出方案。'], [2, 'dba_li', '已超过解决时限，请更新进展。注意 P0 不允许延期。']]},
    {id: 58, lv: 'P0', st: 'accepted', side: 'dba', owner: 'dba_li', dba: 'dba_li', team: 2, inst: 3, db: 'pay_db', table: 'payments', cat: 'noidx', src: 'slowlog', idx: ['channel', 'paid_at'], age: 30, acc: 28, plan: 1, m: [5600, 61000, 760, 8200000, 1, 31],
      sql: "SELECT COUNT(*), SUM(amount) FROM payments WHERE channel = 'alipay' AND paid_at >= '2026-10-06 00:00:00' AND paid_at < '2026-10-07 00:00:00'",
      cm: [[26, 'dba_li', '5.7 实例，ADD INDEX 走 INPLACE，预计 40 分钟；已和支付中心约了今晚 23:00 低峰执行。']]},
    {id: 57, lv: 'P1', st: 'accepted', side: 'dev', owner: 'rd_zhou', dba: 'dba_li', team: 2, inst: 3, db: 'pay_db', table: 'refunds', cat: 'deeppage', src: 'perf_schema', age: 160, acc: 150, plan: 0, m: [2900, 12500, 210, 180020, 20, 7.1],
      sql: 'SELECT id, refund_no, pay_no, amount, created_at FROM refunds ORDER BY id LIMIT 180000, 20',
      ext: [7, '深分页改造依赖运营后台翻页组件改版（改为游标分页），预计下周二随版本上线，申请延期到 10-14。', 'rd_zhou', 3]},
    {id: 56, lv: 'P0', st: 'assigned', side: 'dba', owner: 'dba_li', dba: 'dba_li', team: 3, inst: 4, db: 'user_db', table: 'login_log', cat: 'noidx', src: 'perf_schema', idx: ['ip', 'created_at'], age: 3, m: [12300, 80300, 320, 21000000, 14, 41],
      sql: "SELECT user_id, COUNT(*) AS cnt FROM login_log WHERE ip = '203.0.113.77' AND created_at >= '2026-10-01 00:00:00' GROUP BY user_id"},
    {id: 55, lv: 'P1', st: 'accepted', side: 'dev', owner: 'rd_zhao', dba: 'dba_li', team: 1, inst: 2, db: 'order_db', table: 'orders', cat: 'sortgrp', src: 'slowlog', age: 200, acc: 190, plan: -2, m: [1620, 8800, 420, 380000, 100, 9.4],
      sql: "SELECT user_id, SUM(amount) AS total FROM orders WHERE created_at >= '2026-09-01 00:00:00' GROUP BY user_id ORDER BY total DESC LIMIT 100",
      ext: [5, '该统计接口准备改为 T+1 汇总表，汇总表任务本周五上线，申请延期 5 天。', 'rd_zhao', 6]},
    {id: 54, lv: 'P1', st: 'fixed', side: 'dev', owner: 'rd_zhao', dba: 'dba_li', team: 1, inst: 1, db: 'order_db', table: 'order_log', cat: 'bigres', src: 'slowlog', age: 100, acc: 96, fix: 20, m: [2400, 15000, 300, 98000, 98000, 8],
      sql: "SELECT * FROM order_log WHERE created_at >= '2026-10-05 00:00:00'", fixText: '只查询 order_id/action/created_at 三列，增加 created_at 范围与 LIMIT 1000；已随 10-06 晚版本发布。', wf: 1038},
    {id: 53, lv: 'P2', st: 'fixed', side: 'dev', owner: 'rd_zhou', dba: 'dba_li', team: 2, inst: 3, db: 'pay_db', table: 'payments', cat: 'deeppage', src: 'slowlog', age: 140, acc: 130, fix: 76, m: [3300, 9800, 40, 90050, 50, 3.9],
      sql: 'SELECT id, pay_no, order_no, amount FROM payments WHERE status = 1 ORDER BY paid_at DESC LIMIT 90000, 50', fixText: '改为游标分页（WHERE paid_at < ? ORDER BY paid_at DESC LIMIT 50），已上线。', wf: 1031},
    {id: 52, lv: 'P2', st: 'assigned', side: 'dev', owner: 'dba_li', dba: 'dba_li', team: 4, inst: 5, db: 'order_archive', table: 'orders_2025', cat: 'olap', src: 'pt_digest', age: 360, m: [6400, 22000, 12, 41200000, 14, 1.2],
      sql: "SELECT DATE_FORMAT(created_at, '%Y-%m') AS m, COUNT(*), SUM(amount) FROM orders_2025 GROUP BY m"},
    {id: 51, lv: 'P2', st: 'waive_pending', side: 'dev', owner: 'rd_zhou', dba: 'dba_li', team: 2, inst: 3, db: 'pay_db', table: 'payments', cat: 'olap', src: 'slowlog', age: 120, acc: 100, plan: 3, m: [8200, 31000, 9, 2400000, 62, 2.1],
      sql: "SELECT channel, DATE(paid_at) AS d, COUNT(*) AS c, SUM(amount) AS s FROM payments WHERE paid_at >= '2026-09-01' AND paid_at < '2026-10-01' GROUP BY channel, DATE(paid_at)",
      waive: ['月度结算报表，每月 1 日凌晨由定时任务触发一次，业务窗口外执行且已限流，慢但可接受，申请豁免。', 'rd_zhou', 5]},
    {id: 50, lv: 'P1', st: 'closed', side: 'dev', owner: 'rd_zhao', dba: 'dba_li', team: 1, inst: 1, db: 'order_db', table: 'orders', cat: 'idxfail', src: 'slowlog', age: 300, acc: 290, fix: 200, closed: 120, kind: 'verified', m: [1900, 7800, 360, 840000, 1, 8],
      sql: 'SELECT id, status, amount FROM orders WHERE order_no = 202610070001', fixText: 'order_no 是 varchar，参数改为字符串，隐式转换消除。', wf: 1029},
    {id: 49, lv: 'P1', st: 'new', side: 'dev', owner: '', dba: '', team: 3, inst: 4, db: 'user_db', table: 'users', cat: 'likefront', src: 'perf_schema', age: 280, m: [5100, 18000, 150, 18000000, 3, 10.3],
      sql: "SELECT id, username, phone FROM users WHERE phone LIKE '%8801' LIMIT 20"},
    {id: 48, lv: 'P2', st: 'closed', side: 'dev', owner: 'rd_zhou', dba: 'dba_li', team: 2, inst: 3, db: 'pay_db', table: 'refunds', cat: 'inlist', src: 'slowlog', age: 400, acc: 380, fix: 290, closed: 250, kind: 'verified', m: [3100, 8000, 30, 2400, 2400, 3.2],
      sql: 'SELECT * FROM refunds WHERE pay_no IN (' + Array.from({length: 40}, (_, k) => `'P2026100${(k * 7) % 10}${100 + k}'`).join(', ') + ')', fixText: '拆成每批 200 个分批查询。', wf: 1020},
    {id: 47, lv: 'P3', st: 'accepted', side: 'dev', owner: 'rd_zhou', dba: 'dba_li', team: 2, inst: 7, db: 'order_db', table: 'user_addr', cat: 'subq', src: 'slowlog', age: 900, acc: 880, plan: -3, extCount: 1, dueAdd: 168, m: [2600, 7400, 10, 3200000, 8, 0.8],
      sql: 'SELECT * FROM user_addr a WHERE EXISTS (SELECT 1 FROM orders o WHERE o.user_id = a.user_id AND o.status = 3)'},
    {id: 46, lv: 'P2', st: 'fixed', side: 'dba', owner: 'dba_li', dba: 'dba_li', team: 4, inst: 5, db: 'order_db', table: 'order_items', cat: 'noidx', src: 'pt_digest', idx: ['price'], age: 90, acc: 80, fix: 5, m: [3900, 14000, 18, 71000000, 5200, 2.6],
      sql: 'SELECT sku_id, SUM(qty) AS q FROM order_items WHERE price > 5000 GROUP BY sku_id', fixText: '已在数仓只读库添加 idx_price（INPLACE，耗时 18 分钟）。', wf: 1040},
    {id: 45, lv: 'P3', st: 'closed', side: 'dev', owner: 'dba_li', dba: 'dba_li', team: 4, inst: 5, db: 'order_db', table: 'orders', cat: 'olap', src: 'pt_digest', age: 500, acc: 490, closed: 200, kind: 'waived', m: [2400, 6000, 8, 28000000, 100, 0.5],
      sql: 'SELECT * FROM orders WHERE amount > 100000 ORDER BY amount DESC', waive: ['数仓每周一次的大额订单核查，离线执行，无业务影响。', 'dba_li', 230]},
    {id: 44, lv: 'P3', st: 'waive_pending', side: 'dev', owner: 'rd_zhao', dba: 'dba_li', team: 1, inst: 1, db: 'order_db', table: 'order_log', cat: 'sortgrp', src: 'slowlog', age: 150, acc: 140, plan: 4, m: [2800, 9000, 12, 4600000, 9, 0.6],
      sql: "SELECT action, COUNT(*) AS cnt FROM order_log WHERE created_at >= '2026-10-01' GROUP BY action",
      waive: ['每月 1 日对账脚本执行一次，非实时链路，业务可接受 10 秒左右耗时，申请豁免。', 'rd_zhao', 20]},
    {id: 43, lv: 'P2', st: 'closed', side: 'dev', owner: 'rd_zhao', dba: 'dba_li', team: 1, inst: 1, db: 'order_db', table: 'orders', cat: 'orderrand', src: 'slowlog', age: 200, closed: 150, kind: 'duplicate', m: [3500, 9000, 40, 28000000, 1, 4],
      sql: 'SELECT * FROM orders ORDER BY RAND() LIMIT 1', dupReason: '压测脚本在预发库误连生产只读从库产生的流量，非业务 SQL。'},
    {id: 42, lv: 'P2', st: 'closed', side: 'dba', owner: 'dba_li', dba: 'dba_li', team: 3, inst: 4, db: 'user_db', table: 'users', cat: 'idxfail', src: 'perf_schema', age: 600, acc: 590, fix: 430, closed: 400, kind: 'verified', m: [1800, 6000, 60, 18000000, 1, 3.4],
      sql: "SELECT * FROM users WHERE LOWER(email) = 'zhang.wei@example.com'", fixText: '邮箱入库统一小写，查询去掉 LOWER()。', wf: 1011},
    {id: 41, lv: 'P1', st: 'closed', side: 'dev', owner: 'rd_zhou', dba: 'dba_li', team: 2, inst: 3, db: 'pay_db', table: 'payments', cat: 'idxfail', src: 'slowlog', age: 480, acc: 470, fix: 100, closed: 60, kind: 'verified', reopen: 1, m: [2200, 7700, 300, 33000000, 1, 11],
      sql: "SELECT * FROM payments WHERE order_no = 'SO202610010088' OR paid_at > '2026-10-01'", fixText: '拆成两条查询；第二次整改后改用 order_no 精确匹配。', wf: 1024},
    {id: 40, lv: 'P3', st: 'closed', side: 'dev', owner: 'rd_zhao', dba: 'dba_li', team: 1, inst: 1, db: 'order_db', table: 'user_addr', cat: 'subq', src: 'slowlog', age: 900, acc: 880, fix: 400, closed: 330, kind: 'verified', m: [1900, 5000, 20, 1800000, 12, 0.7],
      sql: 'SELECT * FROM user_addr WHERE user_id IN (SELECT user_id FROM orders WHERE status = 0)', fixText: '改写为 JOIN。', wf: 1005},
  ];

  const PERF_ISSUES = [];
  function mkIssue(s) {
    const p = SLA[s.lv], created = ago(s.age);
    const i = {
      id: s.id, level: s.lv, st: s.st, side: s.side, owner: s.owner || '', dba: s.dba || '', team: s.team, inst: s.inst, db: s.db, table: s.table, cat: s.cat, src: s.src,
      sql: s.sql, fp: fingerprint(s.sql), idxCols: s.idx || null,
      m: {avgMs: s.m[0], maxMs: s.m[1], exec: s.m[2], scan: s.m[3], rows: s.m[4], pct: s.m[5], waste: Math.round(s.m[0] * s.m[2] / 1000)},
      created, acceptDue: created + p.accept * H, dueAt: created + (p.resolve + (s.dueAdd || 0)) * H,
      acceptedAt: s.acc != null ? ago(s.acc) : 0, plan: s.plan != null ? dayOffset(s.plan) : '', fixedAt: s.fix != null ? ago(s.fix) : 0, fixPlan: s.fixText || '', wf: s.wf || 0,
      closedAt: s.closed != null ? ago(s.closed) : 0, closeKind: s.kind || '', extCount: s.extCount || 0, reopen: s.reopen || 0, locked: false,
      pendExt: s.ext ? {date: dayOffset(s.ext[0]), reason: s.ext[1], by: s.ext[2], at: ago(s.ext[3])} : null,
      pendWaive: s.waive ? {reason: s.waive[0], by: s.waive[1], at: ago(s.waive[2])} : null,
      baseline: null, after: null, log: [], seed: s,
      get overdue() { return isOverdue(this); }, get status() { return ST[this.st][0]; }, get title() { return `${CAT[this.cat].n} · ${this.db}.${this.table}`; },
    };
    if (i.fixedAt) i.baseline = {avgMs: i.m.avgMs, waste: i.m.waste};
    if (i.closeKind === 'verified') i.after = {avgMs: Math.max(9, Math.round(i.m.avgMs * 0.04)), waste: Math.max(1, Math.round(i.m.waste * 0.03))};
    seedLogs(i, s);
    return i;
  }
  function seedLogs(i, s) {
    const L = i.log, add = (at, who, act, text) => L.push({at, who, act, text});
    const t = nowMs(), run = RUNNING.includes(i.st), team = rgName(i.team);
    add(i.created, 'system', 'create', `系统自动建单（来源：${SRC[i.src]}），评估级别 ${i.level}，诊断：${CAT[i.cat].n}`);
    add(i.created + 60e3, 'system', 'route', i.owner ? `按路由规则指派给 ${uName(i.owner)}（团队：${team}）` : `团队「${team}」没有默认接口人，等待 DBA 分派`);
    (s.cm || []).forEach(([h, who, text]) => add(ago(h), who, 'comment', text));
    if (i.acceptedAt) add(i.acceptedAt, i.owner, 'accept', `受理，承诺完成日 ${i.plan || dateOf(i.acceptedAt + 2 * DAY)}`);
    if (run && !i.acceptedAt && t > i.acceptDue) add(i.acceptDue + 5 * 60e3, 'system', 'remind', `SLA L1 催办：已超过受理时限（${SLA[i.level].accept} 小时），已通知负责人`);
    if (run && t > i.dueAt) add(i.dueAt + 5 * 60e3, 'system', 'escalate', `SLA L2 升级：已超过解决时限，已通知团队 TL 与跟进 DBA`);
    if (run && t > i.dueAt + SLA[i.level].l3 * H) add(i.dueAt + SLA[i.level].l3 * H + 5 * 60e3, 'system', 'escalate', 'SLA L3 升级：已通知总监与全局渠道');
    if (s.extCount) add(i.created + 400 * H > t ? ago(300) : i.created + 400 * H, 'dba_li', 'extend_approve', `同意延期 1 次，解决截止顺延 7 天`);
    if (i.fixedAt) add(i.fixedAt, i.owner, 'fix', `标记修复：${i.fixPlan || '已优化'}${i.wf ? `（关联 SQL 上线工单 #${i.wf}）` : ''}`);
    if (i.pendExt) add(i.pendExt.at, i.pendExt.by, 'extend', `申请延期至 ${i.pendExt.date}：${i.pendExt.reason}`);
    if (i.pendWaive) add(i.pendWaive.at, i.pendWaive.by, 'waive', `申请豁免：${i.pendWaive.reason}`);
    if (s.reopen) { add(i.fixedAt - 200 * H, 'dba_li', 'verify_fail', '验证不通过：观察期内平均耗时仍 > 1200 ms，退回处理中'); }
    if (i.closeKind === 'verified') add(i.closedAt, 'dba_li', 'verify_pass', `验证通过：平均耗时 ${fmtMs(i.m.avgMs)} → ${fmtMs(i.after.avgMs)}，已关闭`);
    if (i.closeKind === 'waived') { add(i.closedAt - 20 * H, i.pendWaive ? i.pendWaive.by : 'dba_li', 'waive', `申请豁免：${s.waive[0]}`); add(i.closedAt, 'admin', 'waive_approve', '同意豁免，工单关闭（豁免）'); }
    if (i.closeKind === 'duplicate') add(i.closedAt, 'dba_li', 'close_dup', `误报关闭：${s.dupReason}（之后同一 SQL 再出现不会重开）`);
    L.sort((a, b) => a.at - b.at);
  }
  SEED.forEach(s => PERF_ISSUES.push(mkIssue(s)));
  const byId = id => PERF_ISSUES.find(x => x.id === id);

  /* ---------- SLA 计算 ---------- */
  const running = i => RUNNING.includes(i.st);
  const isOverdue = i => running(i) && nowMs() > i.dueAt;
  const accOverdue = i => (i.st === 'new' || i.st === 'assigned') && nowMs() > i.acceptDue;
  const escLevel = i => {
    if (!running(i)) return 0;
    const t = nowMs();
    if (t > i.dueAt + SLA[i.level].l3 * H) return 3;
    if (t > i.dueAt) return 2;
    return accOverdue(i) ? 1 : 0;
  };
  const remindCount = i => i.log.filter(l => l.act === 'remind' || l.act === 'escalate').length;
  function slaCell(i) {
    if (i.st === 'closed') return '<span class="muted">—</span>';
    if (i.st === 'fixed') return '<span class="muted">已暂停（待验证）</span>';
    if (i.st === 'waive_pending') return '<span class="muted">已暂停（豁免审批）</span>';
    const t = nowMs(), left = i.dueAt - t, lv = escLevel(i), unacc = i.st === 'new' || i.st === 'assigned';
    if (left < 0) return `<b style="color:var(--bad)">已超时 ${durText(-left)}</b> ${tag('L' + lv, 'bad')}`;
    const near = left < Math.max(4 * H, 0.2 * (i.dueAt - i.created));
    let h = `<span style="${near ? 'color:var(--warn);font-weight:600' : ''}">剩余 ${durText(left)}</span>${near ? ' ' + tag('即将超时', 'warn') : ''}`;
    if (unacc) h = (accOverdue(i) ? `<b style="color:var(--bad)">受理超时 ${durText(t - i.acceptDue)}</b> ${tag('L1', 'bad')}` : `<div class="small muted">受理剩余 ${durText(i.acceptDue - t)}</div>`) + h;
    return h;
  }

  /* ---------- 对外导出 ---------- */
  const todoList = login => PERF_ISSUES.filter(i => (running(i) && i.owner === login) || (i.dba === login && (i.st === 'fixed' || i.st === 'waive_pending')));
  window.PERF_ISSUES = PERF_ISSUES;
  window.perfTodoFor = u => { const l = typeof u === 'string' ? u : (u && u.login), list = todoList(l); return {n: list.length, overdue: list.filter(i => i.overdue).length}; };
  [NAV.perf, ...NAV_TREE.flatMap(g => g.items).filter(m => m.id === 'perf')].forEach(m => {
    m.badge = () => window.perfTodoFor(ME).n;
    Object.defineProperty(m, 'badgeWarn', {get: () => window.perfTodoFor(ME).overdue > 0, configurable: true});
  });

  /* ---------- 权限 / 范围 ---------- */
  const inScope = i => isDba() || i.owner === ME.login || i.dba === ME.login || ME.rgs.includes(i.team);
  const WHY = '需是负责人或团队成员';
  /* 带业务规则的按钮：有权限码但不在处理范围 / 状态不允许 → 灰显并提示原因；无权限码走 B() 的无权限样式 */
  function ab(code, label, i, op, o = {}) {
    if (!can(code)) return B(code, label, {cls: o.cls});
    const why = o.why || (o.scope === false ? '' : (!inScope(i) ? WHY : ''));
    if (why) return `<button class="btn ${o.cls || ''}" disabled title="${esc(why)}">${label}</button>`;
    return B(code, label, {cls: o.cls, data: {op, id: i.id}, title: o.title});
  }
  const errBox = '<div id="pf_err" class="small" style="color:var(--bad);min-height:18px;margin-top:6px"></div>';
  const fail = m => { const e = $('#pf_err'); if (e) e.textContent = m; toast(m); return false; };
  const val = id => { const e = $('#' + id); return e ? e.value.trim() : ''; };
  const addLog = (i, act, text) => { i.log.push({at: nowMs(), who: ME.login, act, text}); };
  const finish = (code, i, auditText, toastText) => { audit(code, `#${i.id} ${auditText}`, '成功', `${instName(i.inst)} / ${i.db}`); toast(toastText); buildNav(); rerender(); };

  /* =====================================================================
     perf：工单列表
     ===================================================================== */
  const PF = {tab: 'all', level: '', team: '', status: '', src: '', q: '', scope: '', sort: 'sla', dir: 1, page: 1, size: 14};
  const TABS = [
    ['all', '全部', () => true], ['todo', '我的待办', i => todoList(ME.login).includes(i)], ['new', '待分派', i => i.st === 'new'], ['assigned', '待受理', i => i.st === 'assigned'],
    ['accepted', '处理中', i => i.st === 'accepted'], ['fixed', '待验证', i => i.st === 'fixed'], ['approve', '审批中', i => i.st === 'waive_pending' || !!i.pendExt],
    ['overdue', '已超时', i => i.overdue], ['closed', '已关闭', i => i.st === 'closed'],
  ];
  const scopeOf = () => PF.scope || (isDba() ? 'all' : 'mine');
  const rowsFor = (tab = PF.tab, filters = true) => {
    const q = PF.q.trim().toLowerCase(), tp = TABS.find(t => t[0] === tab)[2];
    let list = PERF_ISSUES.filter(i => (scopeOf() === 'all' || inScope(i)) && tp(i));
    if (filters) list = list.filter(i => (!PF.level || i.level === PF.level) && (!PF.team || i.team == PF.team) && (!PF.status || i.st === PF.status) && (!PF.src || i.src === PF.src)
      && (!q || `#${i.id} ${i.sql} ${i.db} ${i.table} ${i.fp} ${CAT[i.cat].n} ${instName(i.inst)}`.toLowerCase().includes(q)));
    const key = {sla: i => running(i) ? i.dueAt - nowMs() : 9e15 + i.id, level: i => LV_RANK[i.level] * 1e6 - i.m.waste / 1000 + i.id / 1e4, waste: i => -i.m.waste, id: i => -i.id, avg: i => -i.m.avgMs}[PF.sort];
    return list.sort((a, b) => (key(a) - key(b)) * PF.dir);
  };
  function listTable() {
    const all = rowsFor(), pages = Math.max(1, Math.ceil(all.length / PF.size)); if (PF.page > pages) PF.page = pages;
    const rows = all.slice((PF.page - 1) * PF.size, PF.page * PF.size);
    const th = (k, t) => `<a data-on="pfSort" data-k="${k}" style="color:inherit;cursor:pointer">${t}${PF.sort === k ? (PF.dir > 0 ? ' ▲' : ' ▼') : ''}</a>`;
    return tbl(['#', th('level', '级别'), '状态', 'SQL / 诊断', th('waste', '日均浪费 DB 时间'), th('avg', '平均耗时'), '日均次数', '处理方', '团队 / 负责人', th('sla', 'SLA')],
      rows, {
        attrs: r => ` class="click" data-on="pfOpen" data-id="${r.id}"${r.overdue ? ' style="background:color-mix(in srgb, var(--bad) 8%, transparent)"' : ''}`,
        cells: r => [`<a href="#perf/detail/${r.id}" class="b">#${r.id}</a>`, sevTag(r.level), stTag(r),
          `<div class="mono small ellipsis" style="max-width:340px" title="${esc(r.sql)}">${esc(r.sql)}</div><div class="small muted">${esc(instName(r.inst))} / ${esc(r.db)}.${esc(r.table)} · ${CAT[r.cat].n}</div>`,
          `<span class="nowrap">${fmtSec(r.m.waste)}</span><div class="small muted">占比 ${r.m.pct}%</div>`, `<span class="nowrap">${fmtMs(r.m.avgMs)}</span>`, fmt(r.m.exec), SIDE[r.side],
          `${esc(rgName(r.team))}<div class="small">${uCell(r.owner)}</div>`, slaCell(r)],
      }) + (all.length > PF.size ? `<div class="pager"><span>共 ${all.length} 条</span>${Array.from({length: pages}, (_, k) => `<a class="${k + 1 === PF.page ? 'on' : ''}" data-on="pfPage" data-p="${k + 1}">${k + 1}</a>`).join('')}</div>` : `<div class="pager"><span>共 ${all.length} 条</span></div>`);
  }
  function renderList() {
    const my = window.perfTodoFor(ME), vis = PERF_ISSUES.filter(i => scopeOf() === 'all' || inScope(i)), wk = nowMs() - 7 * DAY;
    const kpi = (v, l, d, cls = '') => `<div class="card kpi"><div class="v ${cls}">${v}</div><div class="l">${l}</div>${d ? `<div class="d muted">${d}</div>` : ''}</div>`;
    return `<div class="grid g6" style="margin-bottom:14px">
      ${kpi(my.n, '我的待办', my.overdue ? `其中 <b class="up">${my.overdue} 张已超时</b>` : '无超时')}
      ${kpi(vis.filter(i => i.overdue).length, '已超时', '', 'up')}
      ${kpi(vis.filter(i => i.st === 'new').length, '待分派', '')}
      ${kpi(vis.filter(i => i.st === 'fixed').length, '待验证', '')}
      ${kpi(vis.filter(i => i.created >= wk).length, '本周新增', '')}
      ${kpi(vis.filter(i => i.closedAt >= wk).length, '本周完成', '', 'down')}</div>
    <div class="card">
      <div class="toolbar">
        <select data-onchange="pfFilter" data-k="scope" title="可见范围"><option value="all" ${scopeOf() === 'all' ? 'selected' : ''}>全部团队</option><option value="mine" ${scopeOf() === 'mine' ? 'selected' : ''}>我的团队与我负责的</option></select>
        <select data-onchange="pfFilter" data-k="level"><option value="">全部级别</option>${Object.keys(LV_RANK).map(l => `<option value="${l}" ${PF.level === l ? 'selected' : ''}>${l}-${LV_NAME[l]}</option>`).join('')}</select>
        <select data-onchange="pfFilter" data-k="team"><option value="">全部团队</option>${RGROUPS.filter(r => r.enabled).map(r => `<option value="${r.id}" ${PF.team == r.id ? 'selected' : ''}>${esc(r.name)}</option>`).join('')}</select>
        <select data-onchange="pfFilter" data-k="status"><option value="">全部状态</option>${Object.entries(ST).map(([k, [t]]) => `<option value="${k}" ${PF.status === k ? 'selected' : ''}>${t}</option>`).join('')}</select>
        <select data-onchange="pfFilter" data-k="src"><option value="">全部来源</option>${Object.entries(SRC).map(([k, t]) => `<option value="${k}" ${PF.src === k ? 'selected' : ''}>${t}</option>`).join('')}</select>
        <input type="search" placeholder="搜索指纹 / 库 / 表 / #工单号" value="${esc(PF.q)}" data-oninput="pfFilter" data-k="q" style="min-width:220px">
        <span class="sp"></span><a class="btn" href="#perfboard">团队排行榜 →</a>${B('perf.create', '＋ 手工建单', {cls: 'primary'})}</div>
      <div class="tabs">${TABS.map(([id, t, f]) => `<a data-on="pfTab" data-v="${id}" class="${PF.tab === id ? 'on' : ''}">${t} <span class="muted small">${rowsFor(id, false).length}</span></a>`).join('')}</div>
      <div id="pfTable">${listTable()}</div></div>`;
  }
  ON.pfTab = d => { PF.tab = d.v; PF.page = 1; rerender(); };
  ON.pfFilter = (d, el) => { PF[d.k] = el.value; PF.page = 1; if (d.k === 'scope') { rerender(); return; } $('#pfTable').innerHTML = listTable(); };
  ON.pfSort = d => { if (PF.sort === d.k) PF.dir = -PF.dir; else { PF.sort = d.k; PF.dir = 1; } PF.page = 1; $('#pfTable').innerHTML = listTable(); };
  ON.pfPage = d => { PF.page = +d.p; $('#pfTable').innerHTML = listTable(); };
  ON.pfOpen = (d, el, e) => { if (e.target.closest('a')) return; go('perf', 'detail/' + d.id); };

  /* =====================================================================
     perf：工单详情
     ===================================================================== */
  const trendOf = i => {
    const n = 30, fixIdx = i.fixedAt ? n - 1 - Math.round((nowMs() - i.fixedAt) / DAY) : -1;
    return series(i.id * 7 + 3, n, i.m.avgMs, i.m.avgMs * 0.25, [18, 22, i.m.avgMs * 0.15]).map((v, k) => Math.round(fixIdx >= 0 && k >= fixIdx ? v * 0.045 + 6 : v * (0.75 + k / 100)));
  };
  const LOG_CLS = {accept: 'ok', fix: 'ok', verify_pass: 'ok', extend_approve: 'ok', waive_approve: 'ok', verify_fail: 'bad', extend_reject: 'bad', waive_reject: 'bad', close_dup: 'bad', escalate: 'bad', remind: 'bad', extend: 'cur', waive: 'cur'};
  const LOG_NAME = {create: '建单', route: '路由', comment: '评论', accept: '受理', fix: '标记修复', extend: '申请延期', waive: '申请豁免', verify_pass: '验证通过', verify_fail: '验证不通过', extend_approve: '同意延期', extend_reject: '驳回延期',
    waive_approve: '同意豁免', waive_reject: '驳回豁免', assign: '指派', severity: '调级', side: '切换处理方', close_dup: '误报关闭', reopen: '重开', diagnose: '重新诊断', remind: 'L1 催办', escalate: 'SLA 升级'};
  function renderDetail(id) {
    const i = byId(id);
    if (!i) return `<div class="card">${empty('工单不存在或已被删除')}<div style="text-align:center"><a class="btn" href="#perf">← 返回列表</a></div></div>`;
    const dg = diagOf(i), m = i.m, p = SLA[i.level], lv = escLevel(i), pe = i.pendExt, pw = i.pendWaive, tr = trendOf(i);
    const mine = inScope(i), self = x => x && x.by === ME.login;
    const extWhy = !running(i) ? '当前状态不能申请延期' : pe ? '已有待审批的延期申请' : pw ? '已有待审批的豁免申请' : i.extCount >= p.ext ? (p.ext === 0 ? `${i.level} 级不允许延期` : `已达 ${i.level} 级延期次数上限（${p.ext} 次）`) : '';
    const waiveWhy = !running(i) ? '只有 SLA 计时中的工单可以申请豁免' : pe ? '已有待审批的延期申请，请先处理' : pw ? '已有待审批的豁免申请' : '';
    const handleRow = `<div class="row" style="gap:6px"><span class="muted small">研发 / 负责人：</span>
      ${ab('perf.handle', '受理', i, 'accept', {cls: 'primary', why: (i.st === 'new' || i.st === 'assigned') ? '' : '仅待分派 / 待受理可受理'})}
      ${ab('perf.handle', '标记修复', i, 'fix', {cls: i.st === 'accepted' ? 'primary' : '', why: (i.st === 'assigned' || i.st === 'accepted') ? '' : '仅待受理 / 处理中可标记修复'})}
      ${ab('perf.handle', '申请延期', i, 'extend', {why: mine ? extWhy : WHY})}
      ${ab('perf.handle', '申请豁免', i, 'waive', {why: mine ? waiveWhy : WHY})}
      ${ab('perf.comment', '评论', i, 'comment')}</div>`;
    const dbaRow = `<div class="row" style="gap:6px"><span class="muted small">DBA：</span>
      ${ab('perf.assign', '指派 / 改团队', i, 'assign', {scope: false, why: i.st === 'closed' ? '已关闭的工单请先重开' : ''})}
      ${i.st === 'fixed' ? ab('perf.verify', '验证通过', i, 'pass', {scope: false, cls: 'primary'}) + ab('perf.verify', '验证不通过', i, 'fail', {scope: false, cls: 'danger'}) : ''}
      ${pe ? ab('perf.approve', '同意延期', i, 'ext_ok', {scope: false, why: self(pe) ? '不能审批自己提交的申请' : ''}) + ab('perf.approve', '驳回延期', i, 'ext_no', {scope: false, why: self(pe) ? '不能审批自己提交的申请' : ''}) : ''}
      ${pw ? ab('perf.approve', '同意豁免', i, 'waive_ok', {scope: false, why: self(pw) ? '不能审批自己提交的申请' : ''}) + ab('perf.approve', '驳回豁免', i, 'waive_no', {scope: false, why: self(pw) ? '不能审批自己提交的申请' : ''}) : ''}
      ${ab('perf.adjust', '调整级别', i, 'sev', {scope: false, why: i.st === 'closed' ? '已关闭的工单不能调级' : ''})}
      ${ab('perf.adjust', '切换处理方', i, 'side', {scope: false, why: i.st === 'closed' ? '已关闭的工单不能切换' : ''})}
      ${ab('perf.close_dup', '误报关闭', i, 'dup', {scope: false, cls: 'danger', why: i.st === 'closed' ? '工单已关闭' : ''})}
      ${i.st === 'closed' ? ab('perf.reopen', '重开', i, 'reopen', {scope: false}) : ''}
      ${ab('perf.reopen', '重新诊断', i, 'diagnose', {scope: false})}</div>`;
    const banner = (pe ? notice(`<b>待审批的延期申请</b>（${uCell(pe.by)}，${tsOf(pe.at)}）：希望把解决截止延至 <b>${esc(pe.date)}</b>。<br>理由：${esc(pe.reason)}`, 'warn', '⏳') : '')
      + (pw ? notice(`<b>待审批的豁免申请</b>（${uCell(pw.by)}，${tsOf(pw.at)}）：${esc(pw.reason)}`, 'warn', '⏳') : '')
      + (i.st === 'fixed' ? notice(`研发已标记修复（${tsOf(i.fixedAt)}）。修复方案：${esc(i.fixPlan)}${i.wf ? `（<a href="#sqlworkflow/detail/${i.wf}">SQL 上线工单 #${i.wf}</a>）` : ''}`, 'ok', '✔') : '')
      + (i.closeKind === 'duplicate' ? notice('该工单已被标为<b>误报</b>关闭：之后同一 SQL 再出现不会重开。', '', 'ℹ') : '');
    const kpi = (v, l, d) => `<div class="card kpi"><div class="v">${v}</div><div class="l">${l}</div><div class="d muted">${d || '&nbsp;'}</div></div>`;
    const sparkScan = spark(series(i.id * 11, 14, m.scan, m.scan * 0.15), 'var(--c4)', 100, 24);
    const info = kvList([
      ['实例', `<a href="#instlist">${esc(instName(i.inst))}</a>`], ['库 / 表', `${esc(i.db)} / <b>${esc(i.table)}</b>`], ['来源', SRC[i.src]], ['处理方', SIDE[i.side]],
      ['团队', esc(rgName(i.team))], ['负责人', uCell(i.owner)], ['跟进 DBA', uCell(i.dba)], ['创建时间', tsOf(i.created)],
      ['受理时间', i.acceptedAt ? tsOf(i.acceptedAt) : ''], ['承诺完成日', i.plan], ['标记修复', i.fixedAt ? tsOf(i.fixedAt) : ''],
      ['修复前基线', i.baseline ? `平均 ${fmtMs(i.baseline.avgMs)}，日均浪费 ${fmtSec(i.baseline.waste)}` : ''],
      ['修复后效果', i.after ? `<span class="down">平均 ${fmtMs(i.after.avgMs)}，日均浪费 ${fmtSec(i.after.waste)}（改善 ${Math.round((1 - i.after.avgMs / i.m.avgMs) * 100)}%）</span>` : ''],
      ['延期 / 重开', `延期 ${i.extCount}/${p.ext} 次 · 重开 ${i.reopen} 次`], ['级别', `${sevTag(i.level)} ${i.locked ? tag('已手工调级 · 不再自动调整', 'warn') : ''}`]]);
    const timeline = tline(i.log.slice().sort((a, b) => b.at - a.at).map((l, k) => ({
      cls: k === 0 ? 'cur' : (LOG_CLS[l.act] === 'cur' ? '' : LOG_CLS[l.act] || ''),
      h: `${LOG_NAME[l.act] || l.act} <span class="muted small" style="font-weight:400">· ${esc(uName(l.who))} · ${tsOf(l.at)}</span>`, m: esc(l.text)})));
    return `<div class="row" style="margin-bottom:10px"><a class="btn" href="#perf">← 返回列表</a><span style="font-size:18px;font-weight:700">性能工单 #${i.id}</span>${sevTag(i.level)}${stTag(i)}${tag(SIDE[i.side], 'brand')}${i.overdue ? tag('已超时', 'bad') : ''}
      <span class="sp"></span><span class="muted small">${esc(instName(i.inst))} / ${esc(i.db)}.${esc(i.table)} · ${CAT[i.cat].n}</span></div>
    ${banner}
    <div class="card" style="margin-bottom:14px"><div class="row between" style="margin-bottom:8px;gap:20px"><div class="small">${slaDetail(i, lv, p)}</div></div>
      ${handleRow}<div style="height:6px"></div>${dbaRow}</div>
    <div class="grid g6" style="margin-bottom:14px">
      ${kpi(fmtSec(m.waste), '日均浪费 DB 时间')}${kpi(m.pct + '%', '占该实例慢 SQL 总耗时')}${kpi(fmt(m.exec), '日均执行次数')}
      ${kpi(fmtMs(m.avgMs), '平均耗时', '最大 ' + fmtMs(m.maxMs))}${kpi(fmt(m.scan), '平均扫描行', sparkScan)}${kpi(fmt(m.rows), '平均返回行', `扫描/返回比 ${m.rows ? fmt(Math.round(m.scan / m.rows)) : '—'}`)}</div>
    <div class="grid g-main">
      <div class="grid" style="align-content:start">
        <div class="card"><h2>近 30 天平均耗时（ms）</h2>${lineChart([{name: '平均耗时', color: 'var(--c1)', data: tr}], {xl: ['-30 天', '-22 天', '-15 天', '-7 天', '今天']})}</div>
        <div class="card"><h2>SQL 示例 <span class="sub">最近出现 ${tsOf(i.closedAt || ago(0.4))}</span><span class="right"><a class="btn sm" href="#sqladvisor">去优化工具</a><a class="btn sm" href="#slowlogs">查看慢日志</a></span></h2>
          ${code(i.sql, 'sql')}<div class="small muted">指纹（checksum <span class="mono">${hash8(i.fp)}</span>）：<span class="mono">${esc(i.fp)}</span></div></div>
        <div class="card"><h2>执行计划</h2>${tbl(['id', 'select_type', 'table', 'type', 'key', 'rows', 'filtered', 'Extra'], explainOf(i), {cells: r => [r.id, r.sel, esc(r.table), `<span class="${r.type === 'ALL' ? 'up' : ''}">${r.type}</span>`, r.key ? esc(r.key) : '<span class="muted">NULL</span>', fmt(r.rows), r.filt, esc(r.extra)]})}</div>
      </div>
      <div class="grid" style="align-content:start">
        <div class="card"><h2>诊断结论：${CAT[i.cat].n} ${tag(i.cat === 'noidx' ? '交给 DBA' : '研发处理', i.cat === 'noidx' ? 'brand' : '')}</h2>
          <ul style="margin:0 0 8px;padding-left:18px">${dg.sum.map(s => `<li>${esc(s)}</li>`).join('')}</ul><div class="b small">优化建议</div>
          <ol style="margin:4px 0 8px;padding-left:20px">${dg.adv.map(s => `<li>${esc(s)}</li>`).join('')}</ol>
          ${dg.ddl ? `<div class="b small">建议 DDL（需 DBA 评估）</div>${code(dg.ddl, 'sql')}` : ''}</div>
        <div class="card"><h2>处理信息</h2>${info}</div>
        <div class="card"><h2>处理时间线 <span class="sub">${i.log.length} 条</span></h2>${timeline}</div>
      </div>
    </div>`;
  }
  function slaDetail(i, lv, p) {
    const chips = [1, 2, 3].map(n => tag('L' + n, lv >= n ? 'bad' : '')).join(' ');
    if (!running(i)) return `<b>SLA：</b>${i.st === 'closed' ? '工单已关闭，SLA 结束' : `<span class="muted">已暂停（${ST[i.st][0]}）</span>`}　<span class="muted">受理时限 ${p.accept} h · 解决时限 ${p.resolve} h</span>`;
    return `<b>SLA（${i.level}）：</b>受理截止 <b>${tsOf(i.acceptDue)}</b>${i.acceptedAt ? ' <span class="muted">（已受理）</span>' : ''} ｜ 解决截止 <b>${tsOf(i.dueAt)}</b>${i.extCount ? ` <span class="muted">（含 ${i.extCount} 次延期）</span>` : ''} ｜ 倒计时 ${slaCell(i)} ｜ 升级级别 ${chips} ｜ 已催办 / 升级 ${remindCount(i)} 次`;
  }

  /* =====================================================================
     perf：动作（弹窗 + 状态流转；服务端规则：每次流转前再校验当前状态）
     ===================================================================== */
  const need = (i, states, msg) => { if (!i) { toast('工单不存在'); return false; } if (!states.includes(i.st)) { toast(msg || '工单状态已变化，请刷新'); rerender(); return false; } return true; };
  const form = html => `<div class="form">${html}</div>${errBox}`;
  const restoreSt = i => i.acceptedAt ? 'accepted' : (i.owner ? 'assigned' : 'new');
  ACTIONS['perf.handle'] = d => {
    const i = byId(+d.id); if (!i) return;
    if (d.op === 'accept') {
      if (!need(i, ['new', 'assigned'])) return;
      const dueD = dateOf(i.dueAt), maxD = dueD < TODAY ? TODAY : dueD, def = dayOffset(Math.min(3, Math.max(0, Math.round((Date.parse(maxD) - Date.parse(TODAY)) / DAY))));
      openModal({title: `受理工单 #${i.id}`, code: 'perf.handle', ok: '确认受理', body: form(
        fld('承诺完成日', `<input type="date" id="pf_date" min="${TODAY}" max="${maxD}" value="${def}" style="${DATE_ST};width:100%">`, {req: true})
        + fld('备注（选填）', '<textarea id="pf_remark" rows="2" placeholder="修复思路 / 排期"></textarea>')),
        onOk() {
          const v = val('pf_date');
          if (!v) return fail('请选择承诺完成日');
          if (v < TODAY) return fail('承诺完成日不能早于今天');
          if (v > maxD) return fail(`承诺完成日不能晚于 SLA 解决截止日 ${dueD}`);
          i.st = 'accepted'; i.acceptedAt = nowMs(); i.plan = v; if (!i.owner) i.owner = ME.login;
          addLog(i, 'accept', `受理，承诺完成日 ${v}${val('pf_remark') ? '：' + val('pf_remark') : ''}`);
          finish('perf.handle', i, `受理，承诺完成日 ${v}`, `已受理工单 #${i.id}`);
        }});
    } else if (d.op === 'fix') {
      if (!need(i, ['assigned', 'accepted'], '只有待受理 / 处理中的工单可以标记修复')) return;
      openModal({title: `标记修复 #${i.id}`, code: 'perf.handle', ok: '提交，申请验证', body: form(
        fld('修复方案', '<textarea id="pf_plan" rows="4" placeholder="例：改写为游标分页，已上线"></textarea>', {req: true})
        + fld('关联上线工单（选填）', '<input id="pf_wf" placeholder="如 1042">')),
        onOk() {
          const t = val('pf_plan'); if (t.length < 5) return fail('请填写修复方案（至少 5 个字）');
          i.st = 'fixed'; i.fixedAt = nowMs(); i.fixPlan = t; i.wf = parseInt(val('pf_wf'), 10) || 0; i.baseline = {avgMs: i.m.avgMs, waste: i.m.waste};
          if (!i.acceptedAt) i.acceptedAt = nowMs(); if (!i.owner) i.owner = ME.login;
          addLog(i, 'fix', `标记修复：${t}${i.wf ? `（关联 SQL 上线工单 #${i.wf}）` : ''}`);
          finish('perf.handle', i, '标记修复，申请验证', `已提交验证，工单 #${i.id} 进入待验证`);
        }});
    } else if (d.op === 'extend') {
      const p = SLA[i.level];
      if (!need(i, RUNNING) || i.pendExt || i.pendWaive) return toast('已有待审批的申请');
      if (i.extCount >= p.ext) return toast(`${i.level} 级延期次数已用完（上限 ${p.ext} 次）`);
      const lo = dayOffset(1, dateOf(i.dueAt)), hi = dayOffset(365, TODAY);
      openModal({title: `申请延期 #${i.id}`, code: 'perf.handle', ok: '提交申请', body: form(
        fld('延期至', `<input type="date" id="pf_date" min="${lo}" max="${hi}" value="${dayOffset(7, dateOf(Math.max(i.dueAt, nowMs())))}" style="${DATE_ST};width:100%">`, {req: true})
        + fld('延期理由', '<textarea id="pf_reason" rows="3" maxlength="1000"></textarea>', {req: true})),
        onOk() {
          const v = val('pf_date'), r = val('pf_reason');
          if (!v) return fail('请选择延期日期');
          if (v <= dateOf(i.dueAt)) return fail(`延期日期必须晚于当前解决截止日 ${dateOf(i.dueAt)}`);
          if (v > hi) return fail('延期日期不能超过一年');
          if (!r) return fail('请填写延期理由');
          i.pendExt = {date: v, reason: r, by: ME.login, at: nowMs()};
          addLog(i, 'extend', `申请延期至 ${v}：${r}`);
          finish('perf.handle', i, `申请延期至 ${v}`, '已提交延期申请，等待 DBA 审批');
        }});
    } else if (d.op === 'waive') {
      if (!need(i, RUNNING, '只有 SLA 计时中的工单可以申请豁免')) return;
      if (i.pendExt) return toast('已有待审批的延期申请，请先处理');
      openModal({title: `申请豁免 #${i.id}`, code: 'perf.handle', ok: '提交申请', body: form(
        fld('豁免理由', '<textarea id="pf_reason" rows="4" maxlength="1000" placeholder="说明为何可以接受（至少 10 字）"></textarea>', {req: true})),
        onOk() {
          const r = val('pf_reason'); if (r.length < 10) return fail(`豁免理由至少 10 个字（当前 ${r.length} 字）`);
          i.pendWaive = {reason: r, by: ME.login, at: nowMs()}; i.st = 'waive_pending';
          addLog(i, 'waive', `申请豁免：${r}`);
          finish('perf.handle', i, '申请豁免', '已提交豁免申请，等待 DBA 审批');
        }});
    }
  };
  ACTIONS['perf.comment'] = d => {
    const i = byId(+d.id); if (!i) return;
    openModal({title: `评论 #${i.id}`, code: 'perf.comment', ok: '发表评论', body: form(fld('内容', '<textarea id="pf_text" rows="4" placeholder="进展 / 疑问 / 需配合事项"></textarea>', {req: true})),
      onOk() { const t = val('pf_text'); if (!t) return fail('请输入评论内容'); addLog(i, 'comment', t); finish('perf.comment', i, '评论', '评论已发表'); }});
  };
  ACTIONS['perf.assign'] = d => {
    const i = byId(+d.id); if (!need(i, ['new', 'assigned', 'accepted', 'fixed', 'waive_pending'], '已关闭的工单不能指派')) return;
    openModal({title: `指派 / 改团队 #${i.id}`, code: 'perf.assign', ok: '确认指派', body: form(
      fld('负责人', `<select id="pf_owner">${opts(handlers().map(u => [u.login, `${u.name}（${u.login}）`]), i.owner)}</select>`, {req: true})
      + fld('团队', `<select id="pf_team">${rgOpts(i.team)}</select>`) + fld('备注（选填）', '<input id="pf_remark">')),
      onOk() {
        const o = val('pf_owner'), tm = +val('pf_team'); if (!o) return fail('请选择负责人');
        const old = i.owner, oldTeam = i.team; i.owner = o; i.team = tm; if (i.st === 'new') i.st = 'assigned'; if (!i.dba && isDba()) i.dba = ME.login;
        addLog(i, 'assign', `指派给 ${uName(o)}${old && old !== o ? `（原 ${uName(old)}）` : ''}${tm !== oldTeam ? `，团队 ${rgName(oldTeam)} → ${rgName(tm)}` : ''}${val('pf_remark') ? '：' + val('pf_remark') : ''}`);
        finish('perf.assign', i, `指派给 ${o}`, `已指派给 ${uName(o)}`);
      }});
  };
  ACTIONS['perf.verify'] = d => {
    const i = byId(+d.id); if (!need(i, ['fixed'], '只有待验证的工单可以验证')) return;
    const pass = d.op === 'pass', est = Math.max(9, Math.round(i.m.avgMs * 0.04));
    openModal({title: pass ? `验证通过 #${i.id}` : `验证不通过 #${i.id}`, code: 'perf.verify', ok: pass ? '验证通过并关闭' : '退回处理中', okCls: pass ? 'primary' : 'danger primary', body: form(
      (pass ? notice(`修复前基线：平均 ${fmtMs(i.baseline ? i.baseline.avgMs : i.m.avgMs)}；修复后观察：平均 <b>${fmtMs(est)}</b>（改善 ${Math.round((1 - est / i.m.avgMs) * 100)}%）。`, 'ok', '✔') : '')
      + fld(pass ? '备注（选填）' : '不通过原因', `<textarea id="pf_text" rows="3" placeholder="${pass ? '' : '例：线上仍有慢查询'}"></textarea>`, {req: !pass})),
      onOk() {
        const t = val('pf_text');
        if (pass) {
          i.st = 'closed'; i.closeKind = 'verified'; i.closedAt = nowMs(); i.after = {avgMs: est, waste: Math.max(1, Math.round(i.m.waste * 0.03))};
          addLog(i, 'verify_pass', `验证通过：平均耗时 ${fmtMs(i.m.avgMs)} → ${fmtMs(est)}${t ? '。' + t : ''}，已关闭`);
          finish('perf.verify', i, '验证通过，工单关闭', `工单 #${i.id} 验证通过，已关闭`);
        } else {
          if (!t) return fail('验证不通过必须填写原因');
          i.st = 'accepted'; i.reopen++; i.fixedAt = 0;
          addLog(i, 'verify_fail', `验证不通过：${t}。退回处理中（第 ${i.reopen} 次重开），SLA 继续计时`);
          finish('perf.verify', i, `验证不通过：${t}`, `已退回处理中，通知 ${uName(i.owner)}`);
        }
      }});
  };
  ACTIONS['perf.approve'] = d => {
    const i = byId(+d.id); if (!i) return;
    const isExt = d.op.startsWith('ext'), ok = d.op.endsWith('ok'), req = isExt ? i.pendExt : i.pendWaive;
    if (!req) { toast('该申请已被处理'); rerender(); return; }
    if (req.by === ME.login) return toast('不能审批自己提交的申请');
    openModal({title: `${ok ? '同意' : '驳回'}${isExt ? '延期' : '豁免'} #${i.id}`, code: 'perf.approve', ok: ok ? '同意' : '驳回', okCls: ok ? 'primary' : 'danger primary', body: form(
      notice(`${uCell(req.by)} 的${isExt ? `延期申请：延至 <b>${esc(req.date)}</b>` : '豁免申请'}<br>理由：${esc(req.reason)}`, 'warn', '⏳')
      + fld(ok ? '审批意见（选填）' : '驳回原因', '<textarea id="pf_text" rows="3"></textarea>', {req: !ok})),
      onOk() {
        const t = val('pf_text'); if (!ok && !t) return fail('驳回必须填写原因');
        if (isExt && ok) { i.dueAt = Date.parse(req.date + 'T23:59:59'); i.extCount++; addLog(i, 'extend_approve', `同意延期至 ${req.date} 23:59:59（第 ${i.extCount} 次）${t ? '。' + t : ''}`); }
        else if (isExt) addLog(i, 'extend_reject', `驳回延期申请：${t}`);
        else if (ok) { i.st = 'closed'; i.closeKind = 'waived'; i.closedAt = nowMs(); addLog(i, 'waive_approve', `同意豁免，工单关闭（豁免）${t ? '。' + t : ''}`); }
        else { i.st = restoreSt(i); addLog(i, 'waive_reject', `驳回豁免：${t}，工单回到「${ST[i.st][0]}」`); }
        if (isExt) i.pendExt = null; else i.pendWaive = null;
        finish('perf.approve', i, `${ok ? '同意' : '驳回'}${isExt ? '延期' : '豁免'}`, `已${ok ? '同意' : '驳回'}${isExt ? '延期' : '豁免'}申请`);
      }});
  };
  ACTIONS['perf.adjust'] = d => {
    const i = byId(+d.id); if (!need(i, ['new', 'assigned', 'accepted', 'fixed', 'waive_pending'], '已关闭的工单不能调整')) return;
    if (d.op === 'sev') {
      openModal({title: `调整级别 #${i.id}`, code: 'perf.adjust', ok: '确认调整', body: form(
        fld('新级别', `<select id="pf_lv">${Object.keys(LV_RANK).map(l => `<option value="${l}" ${l === i.level ? 'selected' : ''}>${l}-${LV_NAME[l]}</option>`).join('')}</select>`, {req: true, hint: '调整后级别锁定，不再自动调整'})
        + fld('原因（选填）', '<input id="pf_text">')),
        onOk() {
          const nl = val('pf_lv'); if (nl === i.level) return fail('级别没有变化');
          const old = i.level, t = nowMs(), p = SLA[nl], up = LV_RANK[nl] < LV_RANK[old];
          i.level = nl; i.locked = true;
          if (running(i)) { const base = up ? i.created : t; i.acceptDue = base + p.accept * H; i.dueAt = base + p.resolve * H; }
          addLog(i, 'severity', `调级 ${old} → ${nl}，已锁定不再自动调整${running(i) ? `，SLA 重算：解决截止 ${tsOf(i.dueAt)}` : ''}${val('pf_text') ? '。' + val('pf_text') : ''}`);
          finish('perf.adjust', i, `调级 ${old} → ${nl}`, `已调整为 ${nl}（已锁定）`);
        }});
    } else {
      const to = i.side === 'dev' ? 'dba' : 'dev';
      openModal({title: `切换处理方 #${i.id}`, code: 'perf.adjust', ok: '确认切换', body: form(notice(to === 'dba' ? '改为 <b>DBA 处理</b>' : '改回 <b>研发处理</b>', 'warn', '⇄') + fld('原因（选填）', '<input id="pf_text">')),
        onOk() {
          i.side = to;
          if (to === 'dba') { i.owner = i.dba || ME.login; if (!i.dba) i.dba = ME.login; if (i.st === 'new') i.st = 'assigned'; }
          else { const r = routeFor(i.inst, i.db, i.table); i.team = r.team || i.team; i.owner = r.owner; if (!i.owner && running(i) && i.st !== 'accepted') i.st = 'new'; else if (i.owner && i.st === 'new') i.st = 'assigned'; }
          addLog(i, 'side', `处理方 ${SIDE[to === 'dba' ? 'dev' : 'dba']} → ${SIDE[to]}，负责人：${i.owner ? uName(i.owner) : '待分派'}${val('pf_text') ? '。' + val('pf_text') : ''}`);
          finish('perf.adjust', i, `切换处理方为${SIDE[to]}`, `已切换为${SIDE[to]}`);
        }});
    }
  };
  ACTIONS['perf.close_dup'] = d => {
    const i = byId(+d.id); if (!need(i, ['new', 'assigned', 'accepted', 'fixed', 'waive_pending'], '工单已关闭')) return;
    openModal({title: `误报关闭 #${i.id}`, code: 'perf.close_dup', ok: '下一步：确认', okCls: 'danger primary', body: form(
      notice('关闭后，<b>同一 SQL 再次出现也不会重开工单</b>。', 'bad', '⚠') + fld('原因', '<textarea id="pf_text" rows="3"></textarea>', {req: true})),
      onOk() {
        const r = val('pf_text'); if (!r) return fail('误报关闭必须填写原因');
        setTimeout(() => risky('perf.close_dup', `误报关闭工单 #${i.id}`, `将关闭 <b>#${i.id}</b>（${esc(i.title)}），原因：${esc(r)}。<br>之后同一 SQL 再次出现不会再重开工单。`,
          `UPDATE dbt_perf_issue SET status='closed', close_reason='duplicate', remark='${r.replace(/'/g, "''")}' WHERE id=${i.id};`, () => {
            i.st = 'closed'; i.closeKind = 'duplicate'; i.closedAt = nowMs(); i.pendExt = null; i.pendWaive = null;
            addLog(i, 'close_dup', `误报关闭：${r}（之后同一 SQL 再出现不会重开）`);
            finish('perf.close_dup', i, `误报关闭：${r}`, `工单 #${i.id} 已误报关闭`);
          }, '#' + i.id), 0);
        return true;
      }});
  };
  ACTIONS['perf.reopen'] = d => {
    const i = byId(+d.id); if (!i) return;
    if (d.op === 'reopen') {
      if (!need(i, ['closed'], '只有已关闭的工单可以重开')) return;
      openModal({title: `重开 #${i.id}`, code: 'perf.reopen', ok: '确认重开', body: form(fld('原因（选填）', '<input id="pf_text" placeholder="例：同一 SQL 再次出现">')),
        onOk() {
          const old = i.level; if (!i.locked) i.level = evalLevel(i.m);
          const p = SLA[i.level], t = nowMs(); i.st = i.owner ? 'assigned' : 'new'; i.reopen++; i.closedAt = 0; i.closeKind = ''; i.fixedAt = 0; i.acceptedAt = 0; i.plan = ''; i.acceptDue = t + p.accept * H; i.dueAt = t + p.resolve * H;
          addLog(i, 'reopen', `重开（第 ${i.reopen} 次）${old !== i.level ? `，级别 ${old} → ${i.level}` : ''}${val('pf_text') ? '：' + val('pf_text') : ''}`);
          finish('perf.reopen', i, '重开', `工单 #${i.id} 已重开`);
        }});
    } else {
      openModal({title: `重新诊断 #${i.id}`, code: 'perf.reopen', ok: '重新诊断', wide: true, body: form(
        fld('真实 SQL', `<textarea class="sql-ed" id="pf_sql" rows="6" spellcheck="false">${esc(i.sql)}</textarea>`)),
        onOk() {
          const s = val('pf_sql'); if (!s) return fail('SQL 不能为空');
          if (!/^\s*(select|with|update|delete)\b/i.test(s)) return fail('只支持单条 SELECT / UPDATE / DELETE / WITH 语句');
          if (s.replace(/;\s*$/, '').includes(';')) return fail('只允许单条语句');
          const changed = s !== i.sql; if (changed) { i.sql = s; i.fp = fingerprint(s); const c = classify(s); i.cat = c; i.side = c === 'noidx' ? 'dba' : i.side; }
          addLog(i, 'diagnose', `重新诊断${changed ? '（已替换为真实 SQL）' : ''}：${CAT[i.cat].n}`);
          finish('perf.reopen', i, `重新诊断：${CAT[i.cat].n}`, '已重新诊断');
        }});
    }
  };
  /* 手工建单 */
  ON.pfCrInst = (d, el) => { $('#pf_db').innerHTML = dbsOf(el.value).map(x => `<option>${esc(x.db)}</option>`).join(''); };
  ACTIONS['perf.create'] = () => {
    const insts = visibleInstances().filter(x => x.type !== 'MsSQL');
    if (!insts.length) return toast('没有可用的实例');
    openModal({title: '手工创建性能工单', code: 'perf.create', ok: '创建', wide: true, body: form(
      fld('实例', `<select id="pf_inst" data-onchange="pfCrInst">${insts.map(x => `<option value="${x.id}">${esc(x.name)}（${x.type}${x.role === 'slave' ? ' · 从' : ''}）</option>`).join('')}</select>`, {req: true})
      + fld('库名', `<select id="pf_db">${dbsOf(insts[0].id).map(x => `<option>${esc(x.db)}</option>`).join('')}</select>`)
      + fld('级别', `<select id="pf_lv"><option value="">自动评估</option>${Object.keys(LV_RANK).map(l => `<option value="${l}">${l}-${LV_NAME[l]}</option>`).join('')}</select>`)
      + fld('负责人', `<select id="pf_owner"><option value="">自动（按路由规则）</option>${opts(handlers().map(u => [u.login, `${u.name}（${u.login}）`]), '')}</select>`)
      + fld('SQL', `<textarea class="sql-ed" id="pf_sql" rows="5" spellcheck="false" placeholder="粘贴一条慢 SQL"></textarea>`, {req: true})
      + fld('备注（选填）', '<input id="pf_remark">')),
      onOk() {
        const inst = +val('pf_inst'), db = val('pf_db'), sql = val('pf_sql');
        if (!sql) return fail('请粘贴 SQL');
        if (!/^\s*(select|with|update|delete)\b/i.test(sql)) return fail('只支持单条 SELECT / UPDATE / DELETE / WITH 语句');
        if (sql.replace(/;\s*$/, '').includes(';')) return fail('只允许单条语句');
        const fp = fingerprint(sql), dup = PERF_ISSUES.find(x => x.inst === inst && x.db === db && x.fp === fp);
        if (dup) return fail(`同一指纹已有工单 #${dup.id}（${dup.status}），不会重复创建`);
        const table = tableOf(sql), cat = classify(sql), r = routeFor(inst, db, table), owner = val('pf_owner') || (cat === 'noidx' ? 'dba_li' : r.owner), side = cat === 'noidx' ? 'dba' : 'dev';
        const lvl = val('pf_lv') || 'P2', p = SLA[lvl], t = nowMs(), id = Math.max(...PERF_ISSUES.map(x => x.id)) + 1;
        const m = {avgMs: 0, maxMs: 0, exec: 0, scan: 0, rows: 0, pct: 0, waste: 0};
        const i = {id, level: lvl, st: owner ? 'assigned' : 'new', side, owner, dba: isDba() ? ME.login : 'dba_li', team: r.team || (instOf(inst).rgs || [])[0] || 1, inst, db, table, cat, src: 'manual', sql, fp, idxCols: null, m,
          created: t, acceptDue: t + p.accept * H, dueAt: t + p.resolve * H, acceptedAt: 0, plan: '', fixedAt: 0, fixPlan: '', wf: 0, closedAt: 0, closeKind: '', extCount: 0, reopen: 0, locked: !!val('pf_lv'),
          pendExt: null, pendWaive: null, baseline: null, after: null, log: [],
          get overdue() { return isOverdue(this); }, get status() { return ST[this.st][0]; }, get title() { return `${CAT[this.cat].n} · ${this.db}.${this.table}`; }};
        i.m.avgMs = 0; i.log.push({at: t, who: ME.login, act: 'create', text: `手工建单（级别 ${lvl}${val('pf_lv') ? '' : '，无指标默认 P2'}），诊断：${CAT[cat].n}${val('pf_remark') ? '。备注：' + val('pf_remark') : ''}`});
        i.log.push({at: t + 1, who: 'system', act: 'route', text: owner ? `指派给 ${uName(owner)}（团队：${rgName(i.team)}）` : '未匹配到负责人，等待 DBA 分派'});
        PERF_ISSUES.unshift(i); PF.tab = 'all';
        audit('perf.create', `手工建单 #${id}（${CAT[cat].n}）`, '成功', `${instName(inst)} / ${db}`); toast(`已创建工单 #${id}`); buildNav(); go('perf', 'detail/' + id);
        return true;
      }});
  };

  pages.perf = {
    title: sub => /^detail\//.test(sub || '') ? `性能工单 #${sub.split('/')[1]}` : '工单列表',
    render(sub) { const m = /^detail\/(\d+)/.exec(sub || ''); return m ? renderDetail(+m[1]) : renderList(); },
  };

  /* =====================================================================
     perfboard：团队排行榜
     ===================================================================== */
  const PB = {days: 7, side: 'dev'};
  const BOARD = {1: {nw: 8, dn: 3, ot: 54, fix: 9.4, save: 2.1}, 2: {nw: 5, dn: 4, ot: 78, fix: 6.1, save: 1.2}, 3: {nw: 3, dn: 2, ot: 62, fix: 7.5, save: 0.6}, 4: {nw: 2, dn: 3, ot: 90, fix: 4.2, save: 0.4}};
  const WEEK_NEW = [9, 12, 8, 14, 11, 10, 13, 9, 12, 15, 11, 12], WEEK_DONE = [6, 9, 10, 9, 12, 11, 9, 12, 10, 11, 13, 9];
  function boardRows() {
    const k = PB.days === 7 ? 1 : PB.days === 30 ? 3.6 : 9, sideOk = i => PB.side === 'all' || i.side === PB.side;
    return RGROUPS.filter(r => r.enabled && (PTEAMS[r.id] || {}).rank).map(r => {
      const live = PERF_ISSUES.filter(i => i.team === r.id && sideOk(i)), open = live.filter(i => i.st !== 'closed'), b = BOARD[r.id] || {nw: 1, dn: 1, ot: 80, fix: 5, save: .2};
      const overdue = open.filter(i => i.overdue).length, unacc = open.filter(i => i.st === 'new' || i.st === 'assigned').length, waste = open.reduce((a, i) => a + i.m.waste, 0);
      const ot = Math.max(30, Math.min(100, Math.round(b.ot - (PB.days > 7 ? 4 : 0)))), score = Math.max(0, Math.min(100, Math.round(100 - overdue * 15 - unacc * 5 - waste / 3600 * 10 - (100 - ot) / 5)));
      return {r, open: open.length, overdue, unacc, p01: open.filter(i => i.level === 'P0' || i.level === 'P1').length, nw: Math.round(b.nw * k), dn: Math.round(b.dn * k), ot, fix: b.fix, waste, save: b.save * k, score};
    }).sort((a, b) => a.score - b.score);
  }
  pages.perfboard = {
    render() {
      const rows = boardRows(), sum = f => rows.reduce((a, r) => a + f(r), 0), sideOk = i => PB.side === 'all' || i.side === PB.side;
      const open = PERF_ISSUES.filter(i => i.st !== 'closed' && sideOk(i)), byCat = {};
      open.forEach(i => { byCat[i.cat] = (byCat[i.cat] || 0) + i.m.waste; });
      const dist = Object.entries(byCat).sort((a, b) => b[1] - a[1]).map(([c, v]) => [CAT_S[c], Math.max(1, Math.round(v / 60)), CAT[c].c]);
      const weeks = Array.from({length: 12}, (_, k) => dayOffset(-7 * (11 - k), '2026-10-05').slice(5));
      const kpi = (v, l, cls = '') => `<div class="card kpi"><div class="v ${cls}">${v}</div><div class="l">${l}</div></div>`;
      return `<div class="toolbar">
        <select data-onchange="pbFilter" data-k="days">${opts([[7, '近 7 天'], [30, '近 30 天'], [90, '近 90 天']], PB.days)}</select>
        <select data-onchange="pbFilter" data-k="side">${opts([['dev', '研发处理的问题'], ['dba', 'DBA 处理的问题'], ['all', '全部']], PB.side)}</select>
        <span class="sp"></span><a class="btn" href="#perf">工单列表 →</a></div>
      <div class="grid g6" style="margin-bottom:14px">${kpi(sum(r => r.open), '未关闭工单')}${kpi(sum(r => r.overdue), '已超时', 'up')}${kpi(sum(r => r.nw), '期间新增')}${kpi(sum(r => r.dn), '期间优化完成', 'down')}
        ${kpi((sum(r => r.waste) / 3600).toFixed(1) + ' h', '未关闭问题 每天浪费 DB 时间', 'up')}${kpi(sum(r => r.save).toFixed(1) + ' h', '期间优化 每天节省 DB 时间', 'down')}</div>
      <div class="card" style="margin-bottom:14px"><h2>团队健康度</h2>
        <div class="grid" style="grid-template-columns:repeat(auto-fit,minmax(270px,1fr));margin-bottom:14px">${rows.map(x => `<div class="card score" style="box-shadow:none;gap:14px">${ring(x.score)}<div><div class="b">${esc(x.r.name)}</div>
          <div class="small muted">超时 <b class="${x.overdue ? 'up' : ''}">${x.overdue}</b> · 未受理 ${x.unacc} · 未关闭 ${x.open}</div><div class="meter" style="margin-top:6px"><b class="${x.ot < 60 ? 'bad' : x.ot < 80 ? 'warn' : ''}" style="width:${x.ot}%"></b></div><div class="small muted">按时率 ${x.ot}%</div></div></div>`).join('')}</div>
        ${tbl(['排名', '团队', '健康分', '超时', '未受理', '未关闭', 'P0/P1 未关闭', '期间新增', '期间完成', '按时率', '平均修复天数', '每天浪费 DB 时间'], rows, {
          attrs: () => ' class="click" data-on="pbGo"', cells: (x, k) => [k + 1, `<b>${esc(x.r.name)}</b>`, tag(x.score, x.score >= 85 ? 'ok' : x.score >= 70 ? 'warn' : 'bad'), x.overdue ? `<b class="up">${x.overdue}</b>` : 0, x.unacc, x.open, x.p01, x.nw, x.dn, x.ot + '%', x.fix.toFixed(1), Math.round(x.waste / 60) + ' 分钟']})}</div>
      <div class="grid g2">
        <div class="card"><h2>近 12 周：新增 vs 优化完成</h2>${lineChart([{name: '新增', color: 'var(--c5)', data: WEEK_NEW}, {name: '优化完成', color: 'var(--c2)', data: WEEK_DONE}], {xl: [weeks[0], weeks[2], weeks[5], weeks[8], weeks[11]]})}</div>
        <div class="card"><h2>未关闭问题类型（浪费 DB 时间，分钟）</h2>${dist.length ? stackBar(dist) : empty('没有未关闭的问题')}
          <div style="margin-top:14px">${barChart(dist.slice(0, 6), {h: 150})}</div></div></div>`;
    },
  };
  ON.pbFilter = (d, el) => { PB[d.k] = d.k === 'days' ? +el.value : el.value; rerender(); };
  ON.pbGo = () => go('perf');

  /* =====================================================================
     perfcollect：采集配置
     ===================================================================== */
  const PRODUCTS = [['self', '自建 MySQL'], ['rds', '阿里云 RDS MySQL'], ['polardb', 'PolarDB MySQL'], ['aws', 'AWS RDS / Aurora'], ['polardbx', 'PolarDB-X'], ['doris', 'SelectDB / Doris'], ['adb', 'AnalyticDB MySQL'], ['other', '其他']];
  const METHODS = [['platform', '平台慢日志（dbt_slow_logs 表）'], ['ptdigest', 'pt-query-digest'], ['perfschema', 'performance_schema digest'], ['slowtable', 'mysql.slow_log 表'], ['doris', 'Doris / SelectDB audit_log'],
    ['pdx_sum', 'PolarDB-X statements_summary'], ['pdx_slow', 'PolarDB-X SHOW FULL SLOW'], ['custom', '自定义采集 SQL']];
  const optsOf = (L, v) => (L.find(x => x[0] === v) || [0, v])[1];
  const EX_RE = '^(show|set|use|commit|rollback|begin|start|xa|flush|analyze|optimize|kill)\\b', EX_DB = 'mysql,information_schema,performance_schema,sys,__internal_schema';
  const CUSTOM_SQL = 'SELECT db AS db_name, sql_text, COUNT(*) AS exec_count, SUM(query_time) * 1000 AS total_ms, MAX(query_time) * 1000 AS max_ms,\n       SUM(rows_examined) AS rows_examined, SUM(rows_sent) AS rows_sent, MAX(start_time) AS last_seen\nFROM mysql.slow_log\nWHERE start_time >= %(start)s AND start_time < %(end)s\nGROUP BY db, sql_text';
  const hourAgo = h => { const d = new Date(ago(h)); d.setMinutes(0, 0, 0); return d.getTime(); };
  let CCFG = [
    {id: 1, inst: 1, product: 'self', method: 'platform', avgMs: 1000, totalSec: 10, team: 1, on: true, exRe: EX_RE, exDb: EX_DB, sql: '', lastEnd: hourAgo(0.5), ok: true, res: '指纹 14 · 新建 2 · 更新 12'},
    {id: 2, inst: 2, product: 'self', method: 'platform', avgMs: 1000, totalSec: 10, team: 1, on: true, exRe: EX_RE, exDb: EX_DB, sql: '', lastEnd: hourAgo(0.5), ok: true, res: '指纹 9 · 新建 0 · 更新 9'},
    {id: 3, inst: 3, product: 'self', method: 'platform', avgMs: 800, totalSec: 8, team: 2, on: true, exRe: EX_RE, exDb: EX_DB, sql: '', lastEnd: hourAgo(0.5), ok: true, res: '指纹 11 · 新建 1 · 更新 10'},
    {id: 4, inst: 4, product: 'self', method: 'perfschema', avgMs: 1000, totalSec: 10, team: 3, on: true, exRe: EX_RE, exDb: EX_DB, sql: '', lastEnd: hourAgo(0.5), ok: true, res: '指纹 22 · 新建 3 · 更新 19（MySQL 8.0 含 QUERY_SAMPLE_TEXT）'},
    {id: 5, inst: 5, product: 'self', method: 'ptdigest', avgMs: 3000, totalSec: 30, team: 4, on: false, exRe: EX_RE, exDb: EX_DB, sql: '', lastEnd: hourAgo(26), ok: false, res: 'ERROR 该实例经 SSH 隧道连接，采集配置暂不支持（隧道尚未实现）'},
    {id: 6, inst: 7, product: 'rds', method: 'slowtable', avgMs: 1000, totalSec: 10, team: 0, on: false, exRe: EX_RE, exDb: EX_DB, sql: '', lastEnd: 0, ok: false, res: 'ERROR 未开启 log_output=TABLE，mysql.slow_log 无数据'},
    {id: 7, inst: 9, product: 'other', method: 'custom', avgMs: 1000, totalSec: 10, team: 4, on: false, exRe: EX_RE, exDb: EX_DB, sql: CUSTOM_SQL, lastEnd: hourAgo(30), ok: false, res: 'ERROR 实例连接失败：dial tcp 10.20.5.11:3306: i/o timeout'},
  ];
  let ccSeq = 7, CJOBS = {collect: true, sla: true, verify: true, report: false}, CWIN = 60;
  const PC = {product: '', on: ''};
  /* 自定义采集 SQL 只读校验：必须是单条 SELECT / WITH / SHOW，不允许 INTO OUTFILE、危险函数、DML / DDL */
  function checkSql(sql) {
    const src = sql || ''; if (!src.trim()) return {ok: false, msg: '自定义采集 SQL 不能为空'};
    if (src.length > 4000) return {ok: false, msg: 'SQL 过长（最多 4000 字符）'};
    const s = src.replace(/\/\*[\s\S]*?\*\//g, ' ').replace(/(--|#)[^\n]*/g, ' ').replace(/'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"/g, "''").trim().replace(/;\s*$/, '');
    if (s.includes(';')) return {ok: false, msg: '只允许单条语句（检测到多条语句，注释里夹带的第二条语句同样会被拦截）'};
    const first = (/^\(*\s*([a-z]+)/i.exec(s) || [])[1] || '';
    if (!['select', 'with', 'show'].includes(first.toLowerCase())) return {ok: false, msg: `只读校验不通过：必须以 SELECT / WITH / SHOW 开头（当前为 ${first ? first.toUpperCase() : '空'}）`};
    let m;
    if ((m = /\binto\s+(outfile|dumpfile)\b/i.exec(s))) return {ok: false, msg: `只读校验不通过：不允许 INTO ${m[1].toUpperCase()}`};
    if ((m = /\b(sleep|benchmark|load_file|get_lock|release_lock|master_pos_wait|pg_sleep)\s*\(/i.exec(s))) return {ok: false, msg: `只读校验不通过：不允许调用危险函数 ${m[1]}()`};
    if ((m = /\b(insert|update|delete|replace|drop|alter|create|truncate|grant|revoke|call|rename|handler|load)\b/i.exec(s))) return {ok: false, msg: `只读校验不通过：包含写操作关键字 ${m[1].toUpperCase()}`};
    if (/\bfor\s+update\b|\block\s+in\s+share\s+mode\b/i.test(s)) return {ok: false, msg: '只读校验不通过：不允许加锁读（FOR UPDATE / LOCK IN SHARE MODE）'};
    const w = []; if (!/%\(start\)s/.test(src) || !/%\(end\)s/.test(src)) w.push('未同时使用 %(start)s / %(end)s 占位符，每个窗口都会重复扫描全量数据');
    return {ok: true, msg: '只读校验通过：单条 SELECT，无危险函数', warn: w};
  }
  const compatErr = (c, instId) => {
    const t = instOf(instId).type;
    if (c.method === 'ptdigest' && t !== 'MySQL') return 'pt-query-digest 只适用自建 MySQL 实例';
    if (c.method === 'doris' && c.product !== 'doris') return 'Doris audit_log 只适用 SelectDB / Doris 产品类型';
    if (c.product === 'adb' && c.method !== 'custom') return 'AnalyticDB 只能使用自定义采集 SQL';
    if ((c.method === 'pdx_sum' || c.method === 'pdx_slow') && c.product !== 'polardbx') return 'PolarDB-X 采集方式只适用 PolarDB-X 产品类型';
    if (instOf(instId).ssh) return '该实例走 SSH 隧道，采集配置暂不支持';
    return '';
  };
  pages.perfcollect = {
    render() {
      const rows = CCFG.filter(c => (!PC.product || c.product === PC.product) && (PC.on === '' || String(c.on) === PC.on)), ed = can('perfcfg.collect');
      return `${ed ? '' : notice('你只有「查看配置」权限，以下配置为只读。', 'warn', '🔒')}${notice(`<b>后台任务</b>：${[['collect', '采集'], ['sla', 'SLA 巡检'], ['verify', '每日验证'], ['report', '周报']].map(([k, t]) => tag(t + (CJOBS[k] ? ' ✓' : ' ✗'), CJOBS[k] ? 'ok' : '')).join(' ')}
        　采集窗口 <b>${CWIN}</b> 分钟${ed ? `　${B('perfcfg.collect', '修改窗口', {cls: 'sm', data: {op: 'win'}})}` : ''}`, '', 'ℹ')}
      <div class="card"><div class="toolbar"><select data-onchange="pcFilter" data-k="product"><option value="">产品类型：全部</option>${opts(PRODUCTS, PC.product)}</select>
        <select data-onchange="pcFilter" data-k="on">${opts([['', '启用：全部'], ['true', '启用'], ['false', '停用']], PC.on)}</select><span class="sp"></span>
        ${B('perfcfg.collect', '批量生成默认配置', {data: {op: 'init'}})}${B('perfcfg.collect', '＋ 新增采集配置', {cls: 'primary', data: {op: 'new'}})}</div>
      ${tbl(['实例', '产品类型', '采集方式', '平均耗时阈值', '总耗时阈值', '默认团队', '启用', '上次窗口结束', '上次采集结果', '操作'], rows, {
        cells: c => [`<b class="nowrap">${esc(instName(c.inst))}</b>`, `<span class="nowrap">${optsOf(PRODUCTS, c.product)}</span>`, `<span class="nowrap">${optsOf(METHODS, c.method).replace(/（.*）/, '')}</span>`, `<span class="nowrap">${c.avgMs} ms</span>`, c.totalSec + ' s', c.team ? `<span class="nowrap">${esc(rgName(c.team))}</span>` : '<span class="muted nowrap">取实例资源组</span>',
          c.on ? tag('启用', 'ok') : tag('停用'), c.lastEnd ? `<span class="nowrap">${tsOf(c.lastEnd).slice(5)}</span>` : '<span class="muted">—</span>',
          `<span class="small ${c.ok ? '' : 'up'}">${c.ok ? 'OK ' : ''}${esc(c.res)}</span>`,
          `<div class="ops">${ed ? B('perfcfg.collect', '编辑', {cls: 'sm', data: {op: 'edit', id: c.id}}) : `<button class="btn sm" data-on="pcView" data-id="${c.id}">查看</button>`}${B('perfcfg.collect', '试采集', {cls: 'sm', data: {op: 'try', id: c.id}})}${B('perfcfg.collect', c.on ? '停用' : '启用', {cls: 'sm', data: {op: 'toggle', id: c.id}})}</div>`],
        attrs: c => c.ok ? '' : ' style="background:var(--bad-soft)"'})}
      </div>`;
    },
  };
  ON.pcFilter = (d, el) => { PC[d.k] = el.value; rerender(); };
  ON.pcView = d => collectForm(CCFG.find(c => c.id === +d.id), true);
  function collectForm(c, ro) {
    const isNew = !c.id, dis = ro ? 'disabled' : '', insts = INSTANCES.filter(x => (x.type === 'MySQL' || x.type === 'MariaDB') && x.enabled !== false);
    const body = `<div class="form">
      ${fld('实例', `<select id="pc_inst" ${isNew ? dis : 'disabled'}>${insts.map(x => `<option value="${x.id}" ${x.id === c.inst ? 'selected' : ''}>${esc(x.name)}（${x.type}）</option>`).join('')}</select>`, {req: true})}
      ${fld('产品类型', `<select id="pc_product" ${dis}>${opts(PRODUCTS, c.product)}</select>`)}
      ${fld('采集方式', `<select id="pc_method" data-onchange="pcMethod" ${dis}>${opts(METHODS, c.method)}</select>`, {req: true})}
      ${fld('默认归属团队', `<select id="pc_team" ${dis}>${rgOpts(c.team, '（取实例第一个资源组）')}</select>`)}
      ${fld('平均耗时阈值 (ms)', `<input type="number" id="pc_avg" value="${c.avgMs}" min="1" ${dis}>`, {req: true})}
      ${fld('窗口总耗时阈值 (s)', `<input type="number" id="pc_tot" value="${c.totalSec}" min="1" ${dis}>`)}
      ${fld('排除规则（正则）', `<input id="pc_exre" class="mono" value="${esc(c.exRe)}" ${dis}>`)}
      ${fld('排除库（逗号分隔）', `<input id="pc_exdb" class="mono" value="${esc(c.exDb)}" ${dis}>`)}
      ${fld('自定义采集 SQL', `<textarea id="pc_sql" class="sql-ed" rows="5" spellcheck="false" ${dis}${c.method === 'custom' ? '' : ' disabled'}>${esc(c.sql)}</textarea>
        <div class="row" style="margin-top:6px"><button class="btn sm" data-on="pcCheck" type="button">校验 SQL</button>${ro ? '' : '<button class="btn sm" data-on="pcSample" data-v="ok" type="button">填入合法示例</button><button class="btn sm" data-on="pcSample" data-v="bad" type="button">不合规示例 · INTO OUTFILE</button><button class="btn sm" data-on="pcSample" data-v="dml" type="button">不合规示例 · DELETE</button>'}</div>
        <div id="pc_chk" style="margin-top:6px"></div>`)}
      ${fld('启用', sw('pc_on', c.on) )}</div>
      ${ro ? '' : '<div class="row" style="margin-top:10px"><button class="btn" data-on="pcTry" type="button">试采集（只预览）</button></div>'}<div id="pc_preview" style="margin-top:8px"></div>`;
    openModal({title: ro ? '采集配置（只读）' : (isNew ? '新增采集配置' : '编辑采集配置'), body, wide: true, ok: ro ? '' : '保存', cancel: ro ? '关闭' : '取消', code: ro ? '' : 'perfcfg.collect',
      onOk() {
        const n = readCollect(c); if (typeof n === 'string') { toast(n); const e = $('#pc_chk'); e.innerHTML = notice(esc(n), 'bad', '✖'); return false; }
        if (isNew) { n.id = ++ccSeq; CCFG.push(n); } else Object.assign(c, n);
        audit('perfcfg.collect', `${isNew ? '新增' : '修改'}采集配置：${instName(n.inst)}（${optsOf(METHODS, n.method)}）`); toast('采集配置已保存'); rerender();
      }});
  }
  function readCollect(c) {
    const n = {inst: +val('pc_inst'), product: val('pc_product'), method: val('pc_method'), team: +val('pc_team') || 0, avgMs: +val('pc_avg'), totalSec: +val('pc_tot') || 10, exRe: val('pc_exre'), exDb: val('pc_exdb'), sql: val('pc_sql'), on: $('#pc_on').checked};
    if (!(n.avgMs > 0)) return '平均耗时阈值必须大于 0';
    try { new RegExp(n.exRe); } catch (e) { return '排除规则不是合法的正则表达式'; }
    if (!c.id && CCFG.some(x => x.inst === n.inst)) return `${instName(n.inst)} 已有采集配置，请直接编辑`;
    const e = compatErr(n, n.inst); if (e) return e;
    if (n.method === 'custom') { const r = checkSql(n.sql); if (!r.ok) return r.msg; } else if (n.sql && !checkSql(n.sql).ok) return '非自定义方式下也不允许保存危险 SQL：' + checkSql(n.sql).msg;
    return Object.assign({lastEnd: c.lastEnd || 0, ok: c.ok !== false, res: c.res || '尚未采集'}, n);
  }
  ON.pcMethod = (d, el) => { const t = $('#pc_sql'); t.disabled = el.value !== 'custom'; if (el.value === 'custom' && !t.value.trim()) t.value = CUSTOM_SQL; };
  const BAD_SQL = {bad: "SELECT db AS db_name, sql_text FROM mysql.slow_log\nWHERE start_time >= %(start)s INTO OUTFILE '/tmp/slow.csv'", dml: 'DELETE FROM mysql.slow_log WHERE start_time < %(start)s', sleep: "SELECT db AS db_name, sql_text FROM mysql.slow_log WHERE SLEEP(5) = 0 AND start_time >= %(start)s"};
  ON.pcSample = d => { $('#pc_sql').disabled = false; $('#pc_method').value = 'custom'; $('#pc_sql').value = d.v === 'ok' ? CUSTOM_SQL : BAD_SQL[d.v]; ON.pcCheck(); };
  ON.pcCheck = () => {
    const r = checkSql($('#pc_sql').value);
    $('#pc_chk').innerHTML = r.ok ? notice(`${esc(r.msg)}${(r.warn || []).map(w => `<div class="small">⚠ ${esc(w)}</div>`).join('')}`, (r.warn || []).length ? 'warn' : 'ok', '✔') : notice(esc(r.msg), 'bad', '✖');
  };
  /* 试采集：按总耗时排序的前 50 个指纹，标注 将建单 / 未达阈值 / 已排除 / 已有工单 */
  function previewRows(n) {
    const pool = PERF_ISSUES.filter(i => i.inst === n.inst).map(i => ({db: i.db, sql: i.sql, avg: i.m.avgMs, max: i.m.maxMs, exec: Math.max(1, Math.round(i.m.exec / 24)), scan: i.m.scan}));
    const d0 = dbsOf(n.inst)[0], t0 = d0 ? d0.tables.filter(t => !t.view)[0] : null, db = d0 ? d0.db : 'order_db', tb = t0 ? t0.n : 'orders';
    pool.push({db, sql: `SELECT id FROM ${tb} WHERE id = 10230912`, avg: 82, max: 410, exec: 4200, scan: 1}, {db, sql: `SELECT * FROM ${tb} WHERE id = 8812001`, avg: 60, max: 220, exec: 900, scan: 1},
      {db: 'mysql', sql: 'SELECT host, user FROM mysql.user WHERE user = ?', avg: 1200, max: 2300, exec: 12, scan: 80}, {db, sql: 'SHOW FULL PROCESSLIST', avg: 2400, max: 2400, exec: 3, scan: 0},
      {db, sql: 'SET autocommit = 0', avg: 1500, max: 3000, exec: 40, scan: 0}, {db, sql: `SELECT COUNT(*) FROM ${tb}`, avg: 640, max: 1500, exec: 90, scan: 2000000});
    const ex = (() => { try { return new RegExp(n.exRe, 'i'); } catch (e) { return null; } })(), exdbs = n.exDb.split(',').map(x => x.trim()).filter(Boolean);
    return pool.map(r => {
      const total = r.avg * r.exec, fp = fingerprint(r.sql), have = PERF_ISSUES.find(i => i.inst === n.inst && i.fp === fp);
      let st; if (exdbs.includes(r.db) || (ex && ex.test(r.sql.trim()))) st = ['已排除', '']; else if (have) st = [`已有工单 #${have.id}`, 'info']; else if (r.avg >= n.avgMs || total / 1000 >= n.totalSec) st = ['将建单', 'ok']; else st = ['未达阈值', ''];
      return Object.assign(r, {total, st});
    }).sort((a, b) => b.total - a.total).slice(0, 50);
  }
  function previewHtml(n) {
    if (n.method === 'custom') { const r = checkSql(n.sql); if (!r.ok) return notice(`自定义 SQL 未通过只读校验，已拒绝执行：${esc(r.msg)}`, 'bad', '✖'); }
    const e = compatErr(n, n.inst); if (e) return notice(esc(e), 'bad', '✖');
    const rows = previewRows(n), end = hourAgo(0), start = end - CWIN * 60e3;
    return notice(`预览窗口 <b>${tsOf(start)} ~ ${tsOf(end)}</b>（${optsOf(METHODS, n.method)}），共 ${rows.length} 个指纹，将建单 ${rows.filter(r => r.st[0] === '将建单').length} 个。`, 'ok', '✔')
      + tbl(['库', 'SQL', '次数', '平均 ms', '最大 ms', '总耗时', '结果'], rows, {cells: r => [esc(r.db), `<div class="mono small ellipsis" style="max-width:340px" title="${esc(r.sql)}">${esc(r.sql)}</div>`, fmt(r.exec), fmt(r.avg), fmt(r.max), fmtMs(r.total), tag(r.st[0], r.st[1])]});
  }
  ON.pcTry = () => { const n = readPreview(); $('#pc_preview').innerHTML = previewHtml(n); };
  const readPreview = () => ({inst: +val('pc_inst'), product: val('pc_product'), method: val('pc_method'), avgMs: +val('pc_avg') || 1000, totalSec: +val('pc_tot') || 10, exRe: val('pc_exre'), exDb: val('pc_exdb'), sql: val('pc_sql')});
  ACTIONS['perfcfg.collect'] = d => {
    const c = CCFG.find(x => x.id === +d.id);
    if (d.op === 'new') collectForm({id: 0, inst: (INSTANCES.find(x => (x.type === 'MySQL' || x.type === 'MariaDB') && x.enabled !== false && !CCFG.some(y => y.inst === x.id)) || INSTANCES[0]).id, product: 'self', method: 'platform', avgMs: 1000, totalSec: 10, team: 0, on: false, exRe: EX_RE, exDb: EX_DB, sql: ''});
    else if (d.op === 'edit') collectForm(c);
    else if (d.op === 'try') openModal({title: `试采集 · ${instName(c.inst)}`, wide: true, ok: '', cancel: '关闭', code: 'perfcfg.collect', body: previewHtml(c)});
    else if (d.op === 'toggle') { if (!c.on) { const e = compatErr(c, c.inst); if (e) return toast(e); } c.on = !c.on; audit('perfcfg.collect', `${c.on ? '启用' : '停用'}采集配置：${instName(c.inst)}`); toast(c.on ? '已启用' : '已停用'); rerender(); }
    else if (d.op === 'init') {
      const miss = INSTANCES.filter(x => (x.type === 'MySQL' || x.type === 'MariaDB') && x.enabled !== false && !CCFG.some(y => y.inst === x.id));
      if (!miss.length) return toast('所有支持的实例都已有采集配置');
      miss.forEach(x => CCFG.push({id: ++ccSeq, inst: x.id, product: 'self', method: x.type === 'MySQL' && x.slowlog ? 'platform' : 'perfschema', avgMs: 1000, totalSec: 10, team: 0, on: false, exRe: EX_RE, exDb: EX_DB, sql: '', lastEnd: 0, ok: true, res: '尚未采集（默认停用）'}));
      audit('perfcfg.collect', `批量生成默认采集配置（${miss.length} 个实例，默认停用）`); toast(`已为 ${miss.length} 个实例生成默认停用配置`); rerender();
    } else if (d.op === 'win') {
      openModal({title: '修改采集窗口', code: 'perfcfg.collect', body: form(fld('窗口（分钟）', `<input type="number" id="pc_win" value="${CWIN}" min="10" max="1440">`, {req: true})),
        onOk() { const v = +val('pc_win'); if (!(v >= 10 && v <= 1440)) return fail('窗口需在 10–1440 分钟之间'); CWIN = v; audit('perfcfg.collect', `修改采集窗口为 ${v} 分钟`); toast('已保存'); rerender(); }});
    }
  };

  /* =====================================================================
     perfteam：团队配置 / 路由规则
     ===================================================================== */
  const PT = {tab: 'teams'};
  pages.perfteam = {
    render() {
      const ed = can('perfcfg.team');
      const teams = `${tbl(['资源组（团队）', '默认接口人', '团队 TL（L2）', '总监（L3）', '群通知', '参与排行与周报', '成员', '备注', '操作'], RGROUPS, {cells: r => {
        const t = PTEAMS[r.id] || {owner: '', tls: [], dirs: [], rank: false, remark: ''};
        return [`<b>${esc(r.name)}</b>${r.enabled ? '' : ' ' + tag('已停用')}`, uCell(t.owner), t.tls.length ? t.tls.map(uName).join('、') : '<span class="muted">—</span>', t.dirs.length ? t.dirs.map(uName).join('、') : '<span class="muted">—</span>',
          r.bot ? tag(r.bot + ' ✓', 'ok') : '<span class="muted">未配置</span>', t.rank ? tag('是', 'ok') : tag('否'), USERS.filter(u => u.rgs.includes(r.id) && u.status === 'active').length + ' 人', esc(t.remark) || '<span class="muted">—</span>',
          ed ? B('perfcfg.team', '编辑', {cls: 'sm', data: {op: 'tedit', id: r.id}}) : `<button class="btn sm" data-on="ptView" data-id="${r.id}">查看</button>`];
      }})}`;
      const routes = `<div class="toolbar"><span class="sp"></span>${B('perfcfg.team', '＋ 新增路由规则', {cls: 'primary', data: {op: 'radd'}})}</div>
        ${tbl(['顺序', '实例', '库名（正则）', '表名（正则）', '团队', '负责人', '操作'], ROUTES, {cells: (r, k) => [`<span class="b">${k + 1}</span>`, r.inst ? esc(instName(r.inst)) : '<span class="muted">全部实例</span>', `<code>${esc(r.db)}</code>`, `<code>${esc(r.tbl)}</code>`, esc(rgName(r.team)), r.owner ? uCell(r.owner) : '<span class="muted">团队默认接口人</span>',
          `<div class="ops">${B('perfcfg.team', '↑', {cls: 'sm', data: {op: 'rup', id: r.id}, title: '上移'})}${B('perfcfg.team', '↓', {cls: 'sm', data: {op: 'rdown', id: r.id}, title: '下移'})}${B('perfcfg.team', '编辑', {cls: 'sm', data: {op: 'redit', id: r.id}})}${B('perfcfg.team', '删除', {cls: 'sm danger', data: {op: 'rdel', id: r.id}})}</div>`]})}
        <div class="card" style="box-shadow:none;margin-top:12px"><h2>路由测试</h2><div class="row">
          <select id="pt_ti">${opts(INSTANCES.filter(x => (x.type === 'MySQL' || x.type === 'MariaDB')).map(x => [x.id, x.name]), 3)}</select><input id="pt_tdb" placeholder="库名" value="pay_db" style="width:130px"><input id="pt_ttb" placeholder="表名" value="payments" style="width:130px"><button class="btn" data-on="ptTest">测试</button></div><div id="pt_res" style="margin-top:8px"></div></div>`;
      return `${ed ? '' : notice('你只有「查看配置」权限，以下配置为只读。', 'warn', '🔒')}<div class="card">${tabs('perfteam', [['teams', '团队'], ['routes', `路由规则 (${ROUTES.length})`]], PT.tab).replace(/href="#perfteam\/(\w+)"/g, 'data-on="ptTab" data-v="$1"')}${PT.tab === 'teams' ? teams : routes}</div>`;
    },
  };
  ON.ptTab = d => { PT.tab = d.v; rerender(); };
  ON.ptTest = () => {
    const r = routeFor(+val('pt_ti'), val('pt_tdb'), val('pt_ttb'));
    $('#pt_res').innerHTML = r.rule ? notice(`命中第 <b>${ROUTES.indexOf(r.rule) + 1}</b> 条规则（<code>${esc(r.rule.db)}</code> / <code>${esc(r.rule.tbl)}</code>）→ 团队 <b>${esc(rgName(r.team))}</b>，负责人 <b>${r.owner ? esc(uName(r.owner)) : '待分派'}</b>`, 'ok', '✔')
      : notice(`没有命中任何路由规则 → 回退到实例资源组 <b>${esc(rgName(r.team))}</b> 的默认接口人：<b>${r.owner ? esc(uName(r.owner)) : '无，工单将停在「待分派」'}</b>`, 'warn', '↪');
  };
  ON.ptView = d => teamForm(+d.id, true);
  const userOpts = (sel, withEmpty) => (withEmpty ? '<option value="">（无）</option>' : '') + USERS.filter(u => u.status === 'active').map(u => `<option value="${u.login}" ${[].concat(sel).includes(u.login) ? 'selected' : ''}>${esc(u.name)}（${u.login}）</option>`).join('');
  function teamForm(rid, ro) {
    const r = RGROUPS.find(x => x.id === rid), t = PTEAMS[rid] || (PTEAMS[rid] = {owner: '', tls: [], dirs: [], rank: true, remark: ''}), dis = ro ? 'disabled' : '';
    openModal({title: `${ro ? '团队配置（只读）' : '团队配置'} · ${r.name}`, wide: true, ok: ro ? '' : '保存', cancel: ro ? '关闭' : '取消', code: ro ? '' : 'perfcfg.team', body: `<div class="form">
      ${fld('资源组（团队）', `<select disabled><option>${esc(r.name)}</option></select>`)}${fld('默认接口人', `<select id="pt_owner" ${dis}>${userOpts(t.owner, true)}</select>`)}
      ${fld('团队 TL（L2，可多选）', `<select id="pt_tls" multiple size="4" ${dis}>${userOpts(t.tls)}</select>`)}${fld('总监（L3，可多选）', `<select id="pt_dirs" multiple size="3" ${dis}>${userOpts(t.dirs)}</select>`)}
      ${fld('参与排行与周报', sw('pt_rank', t.rank))}${fld('备注', `<input id="pt_remark" value="${esc(t.remark)}" ${dis}>`)}</div>`,
      onOk() {
        t.owner = val('pt_owner'); t.tls = [...$('#pt_tls').selectedOptions].map(o => o.value); t.dirs = [...$('#pt_dirs').selectedOptions].map(o => o.value); t.rank = $('#pt_rank').checked; t.remark = val('pt_remark');
        audit('perfcfg.team', `修改团队配置：${r.name}（默认接口人 ${t.owner || '无'}，参与排行 ${t.rank ? '是' : '否'}）`); toast('团队配置已保存'); rerender();
      }});
  }
  function routeForm(rt) {
    const isNew = !rt.id, insts = INSTANCES.filter(x => (x.type === 'MySQL' || x.type === 'MariaDB'));
    openModal({title: isNew ? '新增路由规则' : '编辑路由规则', code: 'perfcfg.team', ok: '保存', body: form(
      fld('实例', `<select id="pr_inst"><option value="0">全部实例</option>${insts.map(x => `<option value="${x.id}" ${x.id === rt.inst ? 'selected' : ''}>${esc(x.name)}</option>`).join('')}</select>`)
      + fld('库名（正则）', `<input id="pr_db" class="mono" value="${esc(rt.db)}" placeholder="pay_.*">`, {req: true}) + fld('表名（正则，可空）', `<input id="pr_tbl" class="mono" value="${esc(rt.tbl)}" placeholder="(payments|refunds)">`)
      + fld('团队', `<select id="pr_team">${rgOpts(rt.team)}</select>`, {req: true}) + fld('负责人', `<select id="pr_owner">${userOpts(rt.owner, true).replace('（无）', '（团队默认接口人）')}</select>`)),
      onOk() {
        const db = val('pr_db'), tbl_ = val('pr_tbl') || '.*';
        if (!db) return fail('库名正则不能为空');
        try { new RegExp(db); new RegExp(tbl_); } catch (e) { return fail('库名 / 表名不是合法的正则表达式'); }
        const n = {inst: +val('pr_inst'), db, tbl: tbl_, team: +val('pr_team'), owner: val('pr_owner')};
        if (isNew) ROUTES.push(Object.assign({id: ++routeSeq}, n)); else Object.assign(rt, n);
        audit('perfcfg.team', `${isNew ? '新增' : '修改'}路由规则：${db}/${tbl_} → ${rgName(n.team)}`); toast('路由规则已保存'); rerender();
      }});
  }
  ACTIONS['perfcfg.team'] = d => {
    const idx = ROUTES.findIndex(x => x.id === +d.id);
    if (d.op === 'tedit') teamForm(+d.id);
    else if (d.op === 'radd') routeForm({id: 0, inst: 0, db: '', tbl: '.*', team: 1, owner: ''});
    else if (d.op === 'redit') routeForm(ROUTES[idx]);
    else if (d.op === 'rup' || d.op === 'rdown') {
      const to = d.op === 'rup' ? idx - 1 : idx + 1; if (to < 0 || to >= ROUTES.length) return toast('已经在' + (to < 0 ? '最前' : '最后'));
      [ROUTES[idx], ROUTES[to]] = [ROUTES[to], ROUTES[idx]]; audit('perfcfg.team', `调整路由规则顺序：${ROUTES[to].db} 移到第 ${to + 1} 位`); rerender();
    } else if (d.op === 'rdel') {
      const r = ROUTES[idx]; openModal({title: '删除路由规则', code: 'perfcfg.team', okCls: 'danger primary', ok: '删除', body: notice(`确认删除规则：${esc(r.inst ? instName(r.inst) : '全部实例')} / <code>${esc(r.db)}</code> / <code>${esc(r.tbl)}</code> → ${esc(rgName(r.team))}？`, 'warn', '⚠'),
        onOk() { ROUTES.splice(idx, 1); audit('perfcfg.team', `删除路由规则：${r.db}/${r.tbl}`); toast('已删除'); rerender(); }});
    }
  };

  /* =====================================================================
     perfsla：SLA 策略
     ===================================================================== */
  const SLA_COLS = [['accept', '受理时限 (小时)'], ['resolve', '解决时限 (小时)'], ['l3', '超时后升级总监 (小时)'], ['remind', '重复催办间隔 (小时)'], ['ext', '最多延期次数']];
  const slaText = (sla, esc_) => Object.keys(sla).map(l => `${l}-${LV_NAME[l]}  受理 ${sla[l].accept}h  解决 ${sla[l].resolve}h  超时升级总监 ${sla[l].l3}h  催办间隔 ${sla[l].remind}h  最多延期 ${sla[l].ext} 次`).join('\n') + '\n' + esc_.map(e => `${e.lv} ${e.name}（${e.when}）→ ${Object.keys(TO_NAMES).filter(k => e.to[k]).map(k => TO_NAMES[k]).join('、') || '无'}`).join('\n');
  pages.perfsla = {
    render() {
      const ed = can('perfcfg.sla'), dis = ed ? '' : ' disabled';
      return `${ed ? '' : notice('你只有「查看配置」权限，SLA 策略为只读。', 'warn', '🔒')}<div class="card" style="margin-bottom:14px"><h2>各级别时限 <span class="right">${ed ? `<button class="btn" data-on="slaReset">重置</button>` : ''}${B('perfcfg.sla', '保存修改', {cls: 'primary'})}</span></h2>
        ${tbl(['级别', ...SLA_COLS.map(c => c[1])], Object.keys(SLA), {cells: l => [`${sevTag(l)} ${LV_NAME[l]}`, ...SLA_COLS.map(([k]) => `<input type="number" min="0" style="width:96px" id="sla_${l}_${k}" value="${SLA[l][k]}"${dis}>`)]})}
      </div>
      <div class="card" style="margin-bottom:14px"><h2>升级规则与通知对象</h2>
        ${tbl(['升级级别', '触发条件', ...Object.values(TO_NAMES)], ESC, {cells: (e, k) => [`${tag(e.lv, 'bad')} ${e.name}`, e.when, ...Object.keys(TO_NAMES).map(t => `<input type="checkbox" id="esc_${k}_${t}" ${e.to[t] ? 'checked' : ''}${dis}>`)]})}</div>
      <div class="card"><h2>级别评估规则</h2>
        ${tbl(['级别', '日均浪费 DB 时间', '占实例慢 SQL 总耗时', '平均耗时 且 日均次数'], [...SEV_RULES, ['P3', '其余（都不命中）', '—', '—']], {cells: r => [sevTag(r[0]), r[1], r[2], r[3]]})}</div>`;
    },
  };
  ON.slaReset = () => rerender();
  ACTIONS['perfcfg.sla'] = () => {
    const n = {}, nesc = ESC.map(e => Object.assign({}, e, {to: Object.assign({}, e.to)}));
    for (const l of Object.keys(SLA)) {
      n[l] = {};
      for (const [k, t] of SLA_COLS) { const v = $(`#sla_${l}_${k}`).value.trim(); if (v === '' || !/^\d+$/.test(v)) return toast(`${l} 的「${t}」需要填写非负整数`); n[l][k] = +v; }
      if (n[l].accept < 1 || n[l].resolve < n[l].accept) return toast(`${l}：解决时限需 ≥ 受理时限，且受理时限 ≥ 1 小时`);
      if (n[l].remind < 1) return toast(`${l}：催办间隔至少 1 小时`);
    }
    nesc.forEach((e, k) => Object.keys(TO_NAMES).forEach(t => { e.to[t] = $(`#esc_${k}_${t}`).checked ? 1 : 0; }));
    if (!nesc.every(e => e.to.owner)) return toast('每一级升级都必须通知负责人');
    const oldT = slaText(SLA, ESC), newT = slaText(n, nesc);
    if (oldT === newT) return toast('没有变更');
    const sqls = Object.keys(n).filter(l => JSON.stringify(n[l]) !== JSON.stringify(SLA[l])).map(l => `UPDATE dbt_perf_sla_policy SET accept_hours=${n[l].accept}, resolve_hours=${n[l].resolve}, l3_after_hours=${n[l].l3}, remind_hours=${n[l].remind}, max_extend=${n[l].ext} WHERE severity='${l}';`).join('\n');
    risky('perfcfg.sla', '保存 SLA 策略', `保存后对之后新建 / 重新计算的工单生效，请确认变更：${diffHtml(oldT, newT)}`,
      sqls || '-- 仅升级规则的通知对象有变化（写入 dbt_sql_config）', () => { SLA = n; ESC = nesc; audit('perfcfg.sla', 'SLA 策略变更：' + newT.split('\n').filter(l => !oldT.split('\n').includes(l)).join('；')); toast('SLA 策略已保存'); rerender(); });
  };
})();
