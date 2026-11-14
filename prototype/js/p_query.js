/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_query.js
 * @time: 2026/10/28
 */
/* ============================================================================
   SQL 查询：在线查询 / 查慢日志 / 数据字典 / 分区维护
   规格：pg-query / pg-slowlogs / pg-dict / pg-partman；行为：user-guide 第 1/6/7/8 节与 internal/query、internal/slowlog、internal/dict、internal/partman。
   ON 名称前缀：qry（在线查询）、slg（查慢日志）、dic（数据字典）、ptm（分区维护）。
   ========================================================================== */
(function () {
  'use strict';

  /* ---------- 局部样式（只用 CSS 变量） ---------- */
  const CSS = `
.pq-top{display:flex;gap:10px;align-items:center;flex-wrap:wrap;margin-bottom:12px}
.pq-wb{display:grid;grid-template-columns:268px minmax(0,1fr);gap:14px;align-items:start}
@media(max-width:900px){.pq-wb{grid-template-columns:1fr}}
.pq-side .dbtree{max-height:600px;min-height:160px}
.pq-tabs{display:flex;gap:2px;align-items:flex-end;border-bottom:1px solid var(--border);flex-wrap:wrap;margin-bottom:10px}
.pq-tab{padding:5px 8px 5px 14px;border:1px solid var(--border);border-bottom:0;border-radius:8px 8px 0 0;background:var(--panel2);color:var(--muted);cursor:pointer;display:flex;gap:8px;align-items:center;font-size:13px;user-select:none}
.pq-tab.on{background:var(--panel);color:var(--text);font-weight:600;margin-bottom:-1px;padding-bottom:6px}
.pq-tab b{font-weight:400;opacity:.6;padding:0 5px;border-radius:4px}.pq-tab b:hover{background:var(--border);opacity:1}
.pq-tab.add{padding:5px 12px;color:var(--brand)}
.pq-chips{display:flex;gap:6px;flex-wrap:wrap;margin:10px 0 4px}
.pq-chip{display:inline-flex;gap:4px;align-items:center;padding:1px 9px;border-radius:999px;font-size:12px;background:var(--panel2);border:1px solid var(--border);color:var(--muted)}
.pq-chip.ok{background:var(--ok-soft);color:var(--ok);border-color:transparent}.pq-chip.warn{background:var(--warn-soft);color:var(--warn);border-color:transparent}.pq-chip.bad{background:var(--bad-soft);color:var(--bad);border-color:transparent}
.pq-grid{max-height:430px;overflow:auto;border:1px solid var(--border);border-radius:10px}
.pq-grid table{width:auto;min-width:100%}
.pq-grid th{position:sticky;top:0;z-index:1}
.pq-grid td{white-space:nowrap;font:12px/1.5 ui-monospace,Consolas,monospace;padding:4px 10px}
.pq-grid td.n{text-align:right}
.pq-grid td.rn,.pq-grid th.rn{color:var(--muted);text-align:right;background:var(--panel2)}
.pq-null{color:var(--muted);font-style:italic}
.pq-mk{color:var(--brand)}
.pq-rtabs{display:flex;gap:14px;border-bottom:1px solid var(--border);margin:8px 0 10px}
.pq-rtabs a{padding:4px 2px;color:var(--muted);border-bottom:2px solid transparent;cursor:pointer}
.pq-rtabs a.on{color:var(--brand);border-color:var(--brand);font-weight:600}
.pq-hrow{border:1px solid var(--border);border-radius:10px;padding:8px 10px;margin-bottom:8px;background:var(--panel)}
.pq-hrow pre{margin:6px 0;padding:6px 8px;background:var(--panel2);border-radius:6px;font:12px/1.5 ui-monospace,Consolas,monospace;white-space:pre-wrap;word-break:break-all;max-height:110px;overflow:auto}
.pq-hrow .m{display:flex;gap:10px;flex-wrap:wrap;align-items:center;font-size:12px;color:var(--muted)}
.pq-sqlv{font:12px/1.6 ui-monospace,Consolas,monospace;white-space:pre-wrap;word-break:break-all}
.pq-sub{font-size:12px;color:var(--muted)}
.pq-az{display:flex;gap:6px;flex-wrap:wrap;margin:0 0 10px}
.pq-az a{padding:0 7px;border:1px solid var(--border);border-radius:6px;background:var(--panel2);font-size:12px;color:var(--text)}
.pq-az a.dis{opacity:.35;pointer-events:none}
.pq-letter td{background:var(--panel2);font-weight:700;color:var(--brand)}
.pq-kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:12px;margin-bottom:14px}
.pq-kpis .card{padding:12px 14px}
.pq-exp td{background:var(--panel2)}
details.pq-d{border:1px solid var(--border);border-radius:10px;margin-bottom:10px;background:var(--panel)}
details.pq-d>summary{padding:8px 12px;cursor:pointer;font-weight:600;list-style:none}
details.pq-d>summary::before{content:"▸ ";color:var(--muted)}
details.pq-d[open]>summary::before{content:"▾ "}
details.pq-d>div{padding:0 12px 12px}
.pq-form2{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px 16px}
.pq-form2 label{display:block;font-size:12px;color:var(--muted);margin-bottom:2px}
.pq-form2 input,.pq-form2 select{width:100%}
.pq-form2 .full{grid-column:1/-1}
@media(max-width:700px){.pq-form2{grid-template-columns:1fr}}
.pq-stmt{font:12px/1.6 ui-monospace,Consolas,monospace;padding:4px 8px;border-radius:6px;background:var(--panel2);margin:3px 0;word-break:break-all}
.pq-stmt.add{border-left:3px solid var(--ok)}.pq-stmt.drop{border-left:3px solid var(--bad);background:var(--bad-soft)}.pq-stmt.reorg{border-left:3px solid var(--warn);background:var(--warn-soft)}
.pq-stmt.skipped{opacity:.55}.pq-stmt.failed{border-left:3px solid var(--bad)}
`;
  (function () {
    if (document.getElementById('pq-style')) return;
    const s = document.createElement('style'); s.id = 'pq-style'; s.textContent = CSS; document.head.appendChild(s);
  })();

  /* ---------- 公共小工具 ---------- */
  const hashOf = s => { let h = 2166136261; s = String(s); for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619); } return h >>> 0; };
  const nf = n => Number(n).toLocaleString('en-US');
  const rint = (r, a, b) => a + Math.floor(r() * (b - a + 1));
  const T0 = Date.UTC(2026, 9, 7, 10, 41, 0);              // “现在”（墙上时间）：2026-10-07 10:41
  const fmtDT = t => { const d = new Date(t); return `${d.getUTCFullYear()}-${pad2(d.getUTCMonth() + 1)}-${pad2(d.getUTCDate())} ${pad2(d.getUTCHours())}:${pad2(d.getUTCMinutes())}:${pad2(d.getUTCSeconds())}`; };
  const fmtD = t => fmtDT(t).slice(0, 10);
  const clip = (s, n) => (s = String(s)).length > n ? s.slice(0, n) + '…' : s;
  const oneLine = s => String(s).replace(/\s+/g, ' ').trim();
  const qsOf = sub => { const o = {}, i = String(sub || '').indexOf('?'); if (i < 0) return o; sub.slice(i + 1).split('&').forEach(kv => { if (!kv) return; const [k, v] = kv.split('='); try { o[decodeURIComponent(k)] = decodeURIComponent(v || ''); } catch (e) {} }); return o; };
  const opt = (v, text, sel) => `<option value="${esc(v)}" ${String(v) === String(sel) ? 'selected' : ''}>${esc(text)}</option>`;
  const isProd = inst => inst.env === '生产';
  /* 实例徽标：环境 / 角色 / 类型 */
  const instBadges = inst => `${tag(inst.env, isProd(inst) ? 'bad' : 'ok')} ${tag(inst.role === 'slave' ? '从库' : '主库')} ${tag(inst.type, 'info')} ${isReadView(inst) ? tag('read_view 只读视图', 'warn') : ''} <span class="pq-sub">${esc(inst.host)}:${inst.port} · ${esc(inst.ver)}</span>`;

  /* ============================================================================
     一、在线查询：规则与模拟引擎
     ========================================================================== */
  const ADMIN_LIMIT = 5000;                 // dbt_sql_config.admin_query_limit
  const QCFG = {disableStar: true, timeout: 60};

  /* ---------- 查询权限（授权关系）：优先读 p_priv.js 导出的 window.QPRIVS，缺失时用兜底 ---------- */
  const QP_FALLBACK = [
    {user: 'rd_zhao', inst: 1, db: 'order_db', tables: '*', limit: 1000, expire: '2026-12-31'},
    {user: 'rd_zhao', inst: 2, db: 'order_db', tables: '*', limit: 1000, expire: '2026-09-30'},          // 已过期
    {user: 'rd_zhao', inst: 7, db: 'order_db', tables: ['orders', 'order_items'], limit: 500, expire: '2026-10-20'},
    {user: 'rd_zhao', inst: 8, db: 'order_db', tables: '*', limit: 500, expire: '2026-12-31'},
    {user: 'tl_wang', inst: 1, db: 'order_db', tables: '*', limit: 2000, expire: '2027-03-31'},
    {user: 'tl_wang', inst: 3, db: 'pay_db', tables: '*', limit: 2000, expire: '2027-03-31'},
    {user: 'tl_wang', inst: 7, db: 'order_db', tables: '*', limit: 2000, expire: '2027-03-31'},
    {user: 'qa_sun', inst: 7, db: 'order_db', tables: '*', limit: 500, expire: '2026-11-30'},
    {user: 'qa_sun', inst: 4, db: 'user_db', tables: ['users', 'user_profile'], limit: 200, expire: '2026-10-31'},
    {user: 'rd_zhou', inst: 3, db: 'pay_db', tables: ['payments', 'refunds'], limit: 1000, expire: '2026-10-13'},
  ];
  const qprivs = () => (Array.isArray(window.QPRIVS) && window.QPRIVS.length ? window.QPRIVS : QP_FALLBACK);
  const pTables = p => {
    if (Array.isArray(p.tables)) return !p.tables.length || p.tables.some(t => /^(\*|全部|all|库级)$/i.test(String(t).trim())) ? '*' : p.tables;
    const s = String(p.tables == null ? '*' : p.tables).trim();
    return !s || /^(\*|全部|all|库级)$/i.test(s) ? '*' : s.split(/[,，\s]+/).filter(Boolean);
  };
  const pMatch = (p, inst, db) => p.user === ME.login && (p.inst == inst.id || p.inst === inst.name) && (p.db === db || p.db === '*' || (inst.type === 'Redis' && String(p.db).replace(/^db/i, '') === String(db).replace(/^db/i, '')));
  const pExpired = p => String(p.expire || '9999-12-31').slice(0, 10) < TODAY;
  const myInstGroup = inst => (inst.rgs || []).some(r => ME.rgs.includes(r));

  /* 当前用户在 实例/库 上的查询策略 */
  function qPolicy(inst, db) {
    if (ME.super || can('query.admin_run')) return {mode: 'admin', limit: ADMIN_LIMIT, src: '管理员（query.admin_run，不受库表授权限制）', all: true};
    if (can('query.group_run') && myInstGroup(inst)) return {mode: 'group', limit: ADMIN_LIMIT, src: '资源组内实例（query.group_run，按管理员行数上限）', all: true};
    const all = qprivs().filter(p => pMatch(p, inst, db)), live = all.filter(p => !pExpired(p));
    if (!live.length) return {mode: 'none', expired: all.length ? all.map(p => String(p.expire).slice(0, 10)).sort().pop() : ''};
    return {mode: 'priv', privs: live, limit: Math.max(...live.map(p => +p.limit || 0)), expire: live.map(p => String(p.expire).slice(0, 10)).sort().pop()};
  }
  /* 某张表的生效权限：表级优先于库级；无则 null */
  function tablePriv(pol, tbl) {
    if (pol.all) return {limit: pol.limit, src: pol.src};
    const hit = (pol.privs || []).filter(p => { const t = pTables(p); return t === '*' || t.map(x => x.toLowerCase()).includes(String(tbl).toLowerCase()); });
    if (!hit.length) return null;
    hit.sort((a, b) => (pTables(a) === '*') - (pTables(b) === '*'));
    const p = hit[0]; return {limit: +p.limit, src: `${p.db}${pTables(p) === '*' ? ' 库' : '.' + tbl + ' 表'}权限（到期 ${String(p.expire).slice(0, 10)}）`};
  }

  /* ---------- 词法工具：去注释、切语句、遮盖字符串 ---------- */
  function qLex(sql) {
    const stmts = []; let cur = '', i = 0; const n = sql.length;
    while (i < n) {
      const c = sql[i], c2 = sql.substr(i, 2);
      if ((c2 === '--' && /\s/.test(sql[i + 2] || ' ')) || c === '#') { while (i < n && sql[i] !== '\n') i++; cur += ' '; continue; }
      if (c2 === '/*') { const e = sql.indexOf('*/', i + 2); i = e < 0 ? n : e + 2; cur += ' '; continue; }
      if (c === "'" || c === '"' || c === '`') {
        let j = i + 1;
        while (j < n) { if (sql[j] === '\\' && c !== '`') { j += 2; continue; } if (sql[j] === c) { if (sql[j + 1] === c) { j += 2; continue; } break; } j++; }
        cur += sql.slice(i, j + 1); i = j + 1; continue;
      }
      if (c === ';') { if (cur.trim()) stmts.push(cur.trim()); cur = ''; i++; continue; }
      cur += c; i++;
    }
    if (cur.trim()) stmts.push(cur.trim());
    return stmts;
  }
  /* 等长遮盖：字符串内容 → x，反引号 → 空格（下标与原文一致，可用来在原文上取子串） */
  const qMask = (sql, mssql) => {
    let m = sql.replace(/'(?:[^'\\]|\\.|'')*'|"(?:[^"\\]|\\.|"")*"|`[^`]*`/g, s => s[0] === '`' ? ' ' + s.slice(1, -1) + ' ' : s[0] + 'x'.repeat(s.length - 2) + s[s.length - 1]);
    if (mssql) m = m.replace(/\[([^\]]*)\]/g, (s, a) => ' ' + a + ' ' + ' '.repeat(0));
    return m;
  };
  const KWSTOP = /^(where|on|using|group|order|limit|left|right|inner|outer|cross|natural|join|union|having|for|lock|into|straight_join|window|as|partition|force|use|ignore|set|top|offset|fetch)$/i;
  /* 取语句里的 库.表（含别名）；宁可多报不漏报；FROM / JOIN 后的逗号连接也算 */
  function qTables(m, curDb, mssql) {
    const out = [], seen = new Set(), re = /\b(?:from|join)\s+/gi; let x;
    const ctes = new Set(); let c; const cre = /(?:\bwith\s+(?:recursive\s+)?|,\s*)([A-Za-z_]\w*)\s+as\s*\(/gi;
    while ((c = cre.exec(m))) ctes.add(c[1].toLowerCase());
    while ((x = re.exec(m))) {
      let p = re.lastIndex;
      for (;;) {
        const t = /^\s*([A-Za-z_$][\w$]*)(?:\s*\.\s*([A-Za-z_$][\w$]*))?(?:\s*\.\s*([A-Za-z_$][\w$]*))?/.exec(m.slice(p));
        if (!t) break;
        p += t[0].length;
        let db = curDb, name = t[1];
        if (t[3]) { db = t[1]; name = t[3]; } else if (t[2]) { if (mssql && /^dbo$/i.test(t[1])) name = t[2]; else { db = t[1]; name = t[2]; } }
        const al = /^\s+(?:as\s+)?([A-Za-z_$][\w$]*)/i.exec(m.slice(p));
        let alias = '';
        if (al && !KWSTOP.test(al[1])) { alias = al[1]; p += al[0].length; }
        if (!(ctes.has(name.toLowerCase()) && !t[2]) && !/^dual$/i.test(name)) { const k = db + '.' + name + '/' + alias; if (!seen.has(k)) { seen.add(k); out.push({db, name, alias}); } }
        const cm = /^\s*,/.exec(m.slice(p));
        if (!cm) break;
        p += cm[0].length;
      }
    }
    return out;
  }
  /* 选择列表各项区间（只看第一个顶层 select） */
  function selectList(m) {
    const mm = /\bselect\b(?:\s+(?:all|distinct|distinctrow|sql_calc_found_rows|high_priority|straight_join|sql_no_cache))*(?:\s+top\s*\(?\s*\d+\s*\)?)?/i.exec(m);
    if (!mm) return [];
    let i = mm.index + mm[0].length, depth = 0, start = i; const items = [];
    for (; i < m.length; i++) {
      const c = m[i];
      if (c === '(') depth++;
      else if (c === ')') { if (depth === 0) break; depth--; }
      else if (depth === 0 && c === ',') { items.push([start, i]); start = i + 1; }
      else if (depth === 0 && /^from\b/i.test(m.slice(i, i + 5)) && !/[\w$]/.test(m[i - 1] || ' ')) break;
    }
    items.push([start, i]);
    return items;
  }
  /* 选择列表里是否有通配列（只判断顶层 *，COUNT(*) / a*b 不算） */
  function hasStar(m) {
    const re = /\bselect\b/gi; let x;
    while ((x = re.exec(m))) {
      const sub = m.slice(x.index);
      for (const [s, e] of selectList(sub)) { const t = sub.slice(s, e).trim(); if (t === '*' || /[\w$]\s*\.\s*\*$/.test(t)) return true; }
    }
    return false;
  }
  /* 括号 / 引号配平 + 几类常见语法错误（模拟引擎 1064） */
  function syntaxErr(sql, m) {
    const base = 'ERROR 1064 (42000): You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version for the right syntax to use near ';
    let d = 0; for (const c of m) { if (c === '(') d++; else if (c === ')') { d--; if (d < 0) break; } }
    if (d !== 0) return `${base}'${clip(sql.slice(-24), 24)}' —— 括号没有配平`;
    if ((m.match(/'/g) || []).length % 2) return `${base}'${clip(sql.slice(-20), 20)}' —— 字符串引号没有闭合`;
    let x = /\bselect\s+(?:distinct\s+)?from\b/i.exec(m); if (x) return `${base}'from ${clip(sql.slice(x.index + 11, x.index + 30), 18)}' at line 1 —— SELECT 后没有列`;
    x = /\b(where|and|or|by|on|having|from|join)\s*$/i.exec(m); if (x) return `${base}'' at line 1 —— 语句以 ${x[1].toUpperCase()} 结尾`;
    x = /\bwhere\s+(and|or)\b/i.exec(m); if (x) return `${base}'${x[1]}' —— WHERE 后多了 ${x[1].toUpperCase()}`;
    return '';
  }

  /* ---------- 校验流水线（对应 internal/query 的语句检查） ---------- */
  const FORBID_FN = /\b(sleep|benchmark|load_file|get_lock|release_lock|is_free_lock|is_used_lock|master_pos_wait|sys_eval|sys_exec|pg_sleep|xp_cmdshell)\s*\(/i;
  const SHOW_OK = /^\s*show\s+(?:full\s+)?(?:databases|schemas|tables|columns|fields|index|indexes|keys|create\s+table|create\s+view|table\s+status)\b/i;
  const err = (code, msg, extra) => Object.assign({code, msg}, extra || {});
  const WF_LINK = {href: '#sqlworkflow/new', text: '去提交 SQL 上线工单'};

  /* 返回 {err} 或 {an}；an 描述这条语句怎么执行 */
  function qCheck(raw, inst, db, pageLimit, wantPlan) {
    const pol = qPolicy(inst, db), admin = pol.mode === 'admin', mssql = inst.type === 'MsSQL';
    const notes = [];
    const stmts = qLex(raw);
    if (!stmts.length) return {err: err('bad_query', 'SQL 不能为空（只有注释不算语句）。')};
    if (stmts.length > 1) notes.push(`检测到 ${stmts.length} 条语句，本次只执行第 1 条；要执行其它语句请在编辑器里选中后再点“运行”（选中执行）。`);
    const sql = stmts[0], m = qMask(sql, mssql);
    const kw = ((m.match(/^[\s(]*([a-z_]+)/i) || [])[1] || '').toLowerCase();
    const kind = kw === 'with' ? 'select' : kw === 'desc' || kw === 'describe' ? 'show' : kw;

    /* 1) 语句类型 */
    if (!['select', 'with', 'show', 'explain', 'desc', 'describe'].includes(kw)) {
      const isWrite = /^(insert|update|delete|replace|create|alter|drop|truncate|rename|grant|revoke|call|set|load|lock|unlock|kill|flush|optimize|analyze|repair|merge|exec|execute|begin|start|commit|rollback)$/.test(kw);
      return {err: err('bad_query', kw === 'use' ? '不支持 USE：请在页面顶部选择库。' : `只能执行查询类语句（SELECT、结构类 SHOW、EXPLAIN）；当前语句是 ${kw ? kw.toUpperCase() : '未知'}。${isWrite ? '数据变更与结构变更请走「SQL 上线」工单。' : ''}`, {link: isWrite ? WF_LINK : null})};
    }
    if (kw === 'with' && /\b(insert\s+into|update\s+[\w$.]+\s+set|delete\s+from|replace\s+into)\b/i.test(m)) return {err: err('bad_query', 'WITH 语句里含有写操作，只允许纯查询。', {link: WF_LINK})};
    if (/\binto\s+(outfile|dumpfile)\b/i.test(m)) return {err: err('bad_query', '禁止 SELECT … INTO OUTFILE / DUMPFILE（写文件）。需要导出请走「数据导出」工单。', {link: {href: '#export/new', text: '去提交数据导出工单'}})};
    if (/\bfor\s+update\b|\block\s+in\s+share\s+mode\b|\bfor\s+share\b/i.test(m)) return {err: err('bad_query', '禁止加锁读（FOR UPDATE / LOCK IN SHARE MODE / FOR SHARE）。')};
    if (kw === 'explain' && /^\s*explain\s+analyze\b/i.test(m)) return {err: err('bad_query', 'EXPLAIN ANALYZE 会真正执行语句，已禁止；请使用普通 EXPLAIN。')};
    if (kw === 'show' && !SHOW_OK.test(m) && !admin) return {err: err('bad_query', 'SHOW 仅支持结构类：DATABASES / TABLES / COLUMNS / INDEX / CREATE TABLE / TABLE STATUS。SHOW PROCESSLIST、GRANTS、VARIABLES 等需要管理员权限。')};
    /* 2) 函数黑名单（管理员不受限）、系统库 */
    const fm = FORBID_FN.exec(m);
    if (fm && !admin) return {err: err('bad_query', `语句包含被禁止的函数 ${fm[1].toLowerCase()}()：sleep / benchmark / load_file / get_lock 等可能拖垮库或读取服务器文件，普通用户不可使用。`)};
    if (/\bmysql\s*\.\s*[a-z_]+/i.test(m)) return {err: err('bad_query', '禁止查询 mysql 系统库（如 mysql.user）。')};
    /* 3) 语法 */
    const se = syntaxErr(sql, m); if (se) return {err: err('engine_error', se)};
    if (mssql && /\blimit\s+\d+/i.test(m)) return {err: err('engine_error', "Msg 102, Level 15, State 1：'limit' 附近有语法错误。T-SQL 请使用 TOP n 或 OFFSET … FETCH。")};
    /* 4) SELECT * */
    if (QCFG.disableStar && (kw === 'select' || kw === 'with') && hasStar(m)) return {err: err('star_forbidden', '系统已禁止 SELECT *（disable_star）：请显式列出字段。可点左侧表名自动展开字段；COUNT(*) 不受影响。')};

    /* 5) 涉及的库表：存在性（引擎 EXPLAIN 校验）→ 权限 → read_view */
    let refs = qTables(m, db, mssql);
    const mm = m.match(/^\s*(?:show\s+(?:full\s+)?(?:columns|fields|index|indexes|keys)\s+(?:from|in)|show\s+create\s+(?:table|view)|desc|describe|show\s+table\s+status\s+(?:from|in))\s+([A-Za-z_$][\w$]*)(?:\s*\.\s*([A-Za-z_$][\w$]*))?/i);
    if (mm) refs = [{db: mm[2] ? mm[1] : db, name: mm[2] || mm[1], alias: ''}];
    const dbs = dbsOf(inst.id);
    for (const rf of refs) {
      const dobj = dbs.find(d => d.db.toLowerCase() === rf.db.toLowerCase());
      if (!dobj) return {err: err('engine_error', `ERROR 1049 (42000): Unknown database '${rf.db}'`)};
      const tb = dobj.tables.find(t => t.n.toLowerCase() === rf.name.toLowerCase());
      if (!tb) return {err: err('engine_error', `ERROR 1146 (42S02): Table '${dobj.db}.${rf.name}' doesn't exist`)};
      rf.t = tb; rf.db = dobj.db; rf.name = tb.n;
    }
    if (isReadView(inst)) {
      const bad = refs.find(rf => !/^v_/.test(rf.name));
      if (bad && kw !== 'show') return {err: err('read_view_only', `该实例带有 read_view（只读视图）标签：所有人（含管理员）只能查询以 v_ 开头的视图，${bad.db}.${bad.name} 不允许查询。`)};
    }
    let limit = ADMIN_LIMIT, limitSrc = '管理员上限';
    if ((kw === 'select' || kw === 'with') && !wantPlan) {      // EXPLAIN / 执行计划按原系统不校验表权限
      if (!admin) {
        if (!refs.length) { limit = 100; limitSrc = '无表查询上限（100 行）'; }
        else {
          limit = Infinity; limitSrc = '';
          for (const rf of refs) {
            const rp = qPolicy(inst, rf.db), tp = tablePriv(rp, rf.name);
            if (!tp) {
              const exp = rp.mode === 'none' && rp.expired;
              return {err: err('no_privilege', exp ? `你对 ${rf.db} 的查询权限已于 ${rp.expired} 过期，访问 ${rf.db}.${rf.name} 被拒绝。请重新申请。` : `你没有 ${rf.db}.${rf.name} 的查询权限。`, {db: rf.db, table: rf.name, inst: inst.id, link: {href: `#qapply/new?inst=${inst.id}&db=${encodeURIComponent(rf.db)}`, text: '去申请查询权限'}})};
            }
            if (tp.limit < limit) { limit = tp.limit; limitSrc = tp.src; }
          }
        }
      } else { limit = ADMIN_LIMIT; limitSrc = pol.src; }
    }
    if (inst.status === 'bad') return {err: err('engine_error', `ERROR 2003 (HY000): Can't connect to MySQL server on '${inst.host}:${inst.port}' (110) —— 实例当前不可达，请联系 DBA。`)};

    /* 6) LIMIT 改写 */
    let finalSql = sql.replace(/[\s;]+$/, ''), reqLimit = 0, offset = 0, explicitLimit = false, capped = false;
    if ((kw === 'select' || kw === 'with') && !wantPlan) {
      if (mssql) {
        const tm = /\bselect\s+(?:distinct\s+)?top\s*\(?\s*(\d+)/i.exec(m);
        if (tm) { explicitLimit = true; reqLimit = +tm[1]; }
      } else {
        const lm = /\blimit\s+(\d+)(?:\s*,\s*(\d+)|\s+offset\s+(\d+))?\s*$/i.exec(finalSql);
        if (lm) { explicitLimit = true; if (lm[2] != null) { offset = +lm[1]; reqLimit = +lm[2]; } else { reqLimit = +lm[1]; offset = +(lm[3] || 0); } }
      }
      const eff = Math.min(explicitLimit ? reqLimit : pageLimit, limit);
      capped = !explicitLimit || reqLimit > limit;
      if (explicitLimit && reqLimit > limit) {
        notes.push(`${mssql ? 'TOP' : 'LIMIT'} ${nf(reqLimit)} 超出你的行数上限 ${nf(limit)}（${limitSrc}），已改写为 ${mssql ? 'TOP' : 'LIMIT'} ${nf(limit)}。`);
        finalSql = mssql ? finalSql.replace(/(\bselect\s+(?:distinct\s+)?top\s*\(?\s*)\d+/i, '$1' + limit)
          : finalSql.replace(/(\blimit\s+)(\d+)(\s*,\s*\d+|\s+offset\s+\d+)?\s*$/i, (s, a, n1, rest) => rest && rest.indexOf(',') >= 0 ? `${a}${n1}, ${limit}` : `${a}${limit}${rest || ''}`);
      } else if (!explicitLimit) {
        finalSql = mssql ? finalSql.replace(/\bselect\b(\s+distinct\b)?/i, s => `${s} TOP ${eff}`) : finalSql + ` LIMIT ${eff}`;
        notes.push(`未指定 ${mssql ? 'TOP' : 'LIMIT'}，已自动补充 ${mssql ? 'TOP' : 'LIMIT'} ${nf(eff)}${eff < pageLimit ? `（受行数上限限制：${limitSrc}）` : '（页面“返回行数”）'}。`);
      }
      reqLimit = eff;
    }
    return {an: {kind, kw, sql, m, finalSql, refs, notes, limit, limitSrc, eff: reqLimit, capped, offset, explicitLimit, admin, mssql, plan: !!wantPlan || kw === 'explain', pol}};
  }

  /* ---------- 结果数据生成（按表结构造“像真的”数据） ---------- */
  const SUR = ['赵', '钱', '孙', '李', '周', '吴', '郑', '王', '冯', '陈', '蒋', '沈', '韩', '杨', '朱', '许'];
  const GIV = ['伟', '芳', '娜', '敏', '静', '磊', '洋', '勇', '艳', '杰', '涛', '明', '超', '秀英', '丽', '强'];
  const CITY = ['上海市浦东新区张江路', '北京市海淀区中关村大街', '杭州市余杭区文一西路', '深圳市南山区科技园路', '广州市天河区体育西路', '成都市高新区天府大道', '南京市鼓楼区中山路', '武汉市洪山区珞喻路'];
  const SKU = ['无线蓝牙耳机 Pro', '机械键盘 87 键', 'USB-C 快充线 1m', '智能手环 5', '便携充电宝 20000mAh', '4K 显示器 27 寸', '人体工学椅', '保温杯 500ml', '电动牙刷', '护眼台灯', '无线鼠标', '游戏手柄'];
  const PY = ['zhangwei', 'lina', 'wangjun', 'liuyang', 'chenjie', 'yangfan', 'zhaolei', 'sunmin', 'zhouting', 'wuqiang', 'xuyan', 'hejie'];
  const UAS = ['Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/118.0', 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0) Safari/604.1', 'Mozilla/5.0 (Linux; Android 13) Chrome/117.0 Mobile', 'Mozilla/5.0 (Macintosh; Intel Mac OS X 13_5) Safari/605.1'];
  const ENUMS = {status: [1, 3, 2, 0, 4], channel: ['alipay', 'wechat', 'card'], action: ['create', 'pay', 'ship', 'sign', 'cancel', 'refund'], level: [0, 1, 2, 3, 4, 5]};
  const pk = (r, list) => list[Math.floor(r() * list.length)];

  function maskVal(col, v) {
    const n = col.n.toLowerCase(); v = String(v);
    if (/phone|mobile/.test(n)) return v.replace(/^(\d{3})\d+(\d{4})$/, '$1****$2');
    if (/email/.test(n)) return v.replace(/^(.{2}).*(@.*)$/, '$1***$2');
    if (/id_card/.test(n)) return v.replace(/^(\d{4})\d+(\d{4}|\d{3}\w)$/, '$1**********$2');
    if (/card_no/.test(n)) return v.replace(/^(\d{4})\d+(\d{4})$/, '$1 **** **** $2');
    if (/addr|detail/.test(n)) return v.slice(0, 6) + '****';
    if (/name|holder/.test(n)) return v.slice(0, 1) + '*'.repeat(Math.max(1, v.length - 1));
    return '****';
  }
  /* 生成一个单元格的原始值；ctx：{n 行数, total 估计总行数, desc 是否倒序} */
  function genVal(col, i, r, ctx) {
    const n = col.n.toLowerCase(), t = col.t.toLowerCase();
    if (/^(id|sale_id|customer_key)$/.test(n)) return ctx.desc ? Math.max(1, ctx.total - i) : 1000 + i;
    if (col.nul && !/^(user_id|order_id)$/.test(n) && r() < 0.05) return null;
    if (/order_no$/.test(n)) return 'O' + fmtD(T0 - rint(r, 0, 20) * 86400000).replace(/-/g, '') + rint(r, 10000000, 99999999);
    if (n === 'pay_no') return 'P' + fmtD(T0 - rint(r, 0, 20) * 86400000).replace(/-/g, '') + rint(r, 10000000, 99999999);
    if (n === 'refund_no') return 'R' + fmtD(T0 - rint(r, 0, 20) * 86400000).replace(/-/g, '') + rint(r, 10000000, 99999999);
    if (n === 'user_id') return rint(r, 10000, 9800000);
    if (n === 'order_id') return Math.max(1, ctx.total - rint(r, 0, 4000000));
    if (n === 'sku_id') return rint(r, 1000, 99999);
    if (/_key$/.test(n)) return rint(r, 1, 99999);
    if (n === 'status') return pk(r, [1, 1, 1, 3, 3, 2, 0, 4]);
    if (n === 'level') return pk(r, [0, 0, 1, 1, 2, 3, 4, 5]);
    if (/amount|price|gmv|total/.test(n)) return (Math.pow(r(), 2) * 1800 + 9.9).toFixed(2);
    if (n === 'qty') return pk(r, [1, 1, 1, 2, 2, 3, 5]);
    if (/^(cnt|count|num)$/.test(n)) return rint(r, 1, 3000);
    if (n === 'channel') return pk(r, ['alipay', 'wechat', 'wechat', 'card']);
    if (n === 'action') return pk(r, ENUMS.action);
    if (n === 'operator') return pk(r, ['system', 'wang.lei', 'li.na', 'job:order-timeout', 'system']);
    if (n === 'sku_name') return pk(r, SKU);
    if (n === 'username') return pk(r, PY) + rint(r, 10, 9999);
    if (n === 'ip') return `${rint(r, 10, 223)}.${rint(r, 0, 255)}.${rint(r, 0, 255)}.${rint(r, 1, 254)}`;
    if (n === 'ua') return pk(r, UAS);
    if (/phone|mobile/.test(n)) return '1' + pk(r, [3, 5, 7, 8]) + rint(r, 100000000, 999999999);
    if (/email/.test(n)) return pk(r, PY) + rint(r, 1, 999) + '@' + pk(r, ['163.com', 'qq.com', 'gmail.com', '126.com']);
    if (/id_card/.test(n)) return '' + rint(r, 110000, 659000) + rint(r, 1960, 2003) + pad2(rint(r, 1, 12)) + pad2(rint(r, 1, 28)) + rint(r, 100, 999) + rint(r, 0, 9);
    if (/card_no/.test(n)) return '62' + rint(r, 10000000000000, 99999999999999);
    if (/addr|detail/.test(n)) return pk(r, CITY) + rint(r, 1, 999) + '号' + rint(r, 1, 30) + '栋' + rint(r, 101, 2808);
    if (/name|holder/.test(n) && /char/.test(t)) return pk(r, SUR) + pk(r, GIV);
    if (n === 'birthday') return fmtD(Date.UTC(rint(r, 1970, 2004), rint(r, 0, 11), rint(r, 1, 28)));
    if (/^(day|date)$/.test(n) || t === 'date') return fmtD(T0 - (ctx.desc ? i : ctx.n - 1 - i) * 86400000);
    if (/datetime|timestamp/.test(t) || /_at$/.test(n)) {
      const gap = 37 + (hashOf(n) % 11), k = ctx.desc ? i : Math.max(0, ctx.n - 1 - i);
      let off = 0; if (/^(updated_at|paid_at)$/.test(n)) off = rint(r, 20, 3600);
      return fmtDT(T0 - 20000 - k * gap * 1000 - rint(r, 0, 20000) + off * 1000);
    }
    if (/char|text/.test(t)) return clip(n, 6) + '_' + rint(r, 100, 9999);
    if (/decimal|double|float/.test(t)) return (r() * 1000).toFixed(2);
    if (/int/.test(t)) return rint(r, 0, 1000);
    return '—';
  }

  /* 解析选择列表 → 结果列描述 */
  function parseItems(an) {
    const {m, sql, refs} = an, items = [];
    const colOf = (q, name) => {
      for (const rf of refs) {
        if (!rf.t) continue;
        if (q && q.toLowerCase() !== (rf.alias || rf.name).toLowerCase() && q.toLowerCase() !== rf.name.toLowerCase()) continue;
        const c = (rf.t.cols || []).find(c => c.n.toLowerCase() === name.toLowerCase());
        if (c) return {col: c, tbl: rf.t};
      }
      return null;
    };
    for (const [s, e] of selectList(m)) {
      const mt = m.slice(s, e).trim(), ot = sql.slice(s, e).trim(); if (!mt) continue;
      let expr = mt, oexpr = ot, alias = '';
      let am = /^([\s\S]+?)\s+as\s+([\w$]+)$/i.exec(mt);
      if (!am) {
        am = /^([\s\S]*[\w$)'"])\s+([A-Za-z_][\w$]*)$/.exec(mt);
        if (am && /(^|\s)(case|when|then|else|and|or|not|is|in|like|between|interval|distinct|div|mod)$/i.test(am[1])) am = null;
      }
      if (am) { expr = am[1].trim(); alias = am[2]; oexpr = ot.slice(0, am[1].length).trim() || expr; }
      const it = {expr, alias, label: alias || oexpr};
      const rm = /^(?:([\w$]+)\s*\.\s*)?([\w$]+)$/.exec(expr);
      if (rm && !/^\d/.test(rm[2]) && !/^(null|true|false|current_date|current_timestamp)$/i.test(rm[2])) {
        const h = colOf(rm[1], rm[2]);
        if (!h && refs.length && refs.every(rf => rf.t)) it.unknown = rm[2];
        if (h) { it.col = h.col; it.tbl = h.tbl; it.label = alias || h.col.n; }
      } else if (/^(count|sum|avg|max|min|group_concat)\s*\(/i.test(expr)) {
        it.agg = expr.match(/^(\w+)/)[1].toLowerCase();
        for (const rf of refs) for (const c of (rf.t ? rf.t.cols : [])) if (!it.col && new RegExp('\\b' + c.n + '\\b', 'i').test(expr)) it.col = c;
      } else if (/^\d+(\.\d+)?$/.test(expr)) it.lit = ot;
      else if (/^'.*'$/.test(expr)) it.lit = ot.replace(/^'|'$/g, '');
      else {
        for (const rf of refs) for (const c of (rf.t ? rf.t.cols : [])) if (!it.col && new RegExp('\\b' + c.n + '\\b', 'i').test(expr)) it.col = c;
        it.fn = (expr.match(/^([a-z_]+)\s*\(/i) || [])[1] || '';
      }
      it.mask = !!(it.col && it.col.mask);
      items.push(it);
    }
    return items;
  }

  /* 估计命中行数 / 分组数 */
  function groupInfo(an) {
    const g = /\bgroup\s+by\s+([\s\S]+?)(?=\border\s+by\b|\bhaving\b|\blimit\b|$)/i.exec(an.m);
    if (!g) return null;
    const first = g[1].split(',')[0].trim(), nm = (/([\w$]+)\s*\)?\s*$/.exec(first) || [])[1] || '', low = first.toLowerCase();
    if (ENUMS[nm.toLowerCase()] && !/\(/.test(first)) return {n: ENUMS[nm.toLowerCase()].length, key: nm.toLowerCase(), enum: ENUMS[nm.toLowerCase()]};
    if (/%y-%m'/i.test(an.sql)) return {n: 12, key: 'month'};
    if (/date|day|month|hour|_at|created/.test(low)) return {n: 30, key: 'day'};
    return {n: 24, key: nm};
  }
  function estimate(an, items) {
    const gi = groupInfo(an), aggOnly = items.length && items.every(i => i.agg), total = Math.max(1, ...an.refs.map(rf => rf.t ? (rf.t.rows || (rf.t.view ? (/summary|day/.test(rf.t.n) ? 180 : 600000) : 1)) : 1));
    const wm = /\bwhere\b([\s\S]*?)(?=\bgroup\s+by\b|\border\s+by\b|\blimit\b|\bhaving\b|$)/i.exec(an.m);
    if (gi) return {n: gi.n, group: gi, total, where: !!wm};
    if (aggOnly) return {n: 1, agg: true, total, where: !!wm};
    if (wm && /\b(id|order_no|pay_no|refund_no|username|sale_id)\s*=\s*['\w]/i.test(wm[1])) return {n: 1, total, where: true};
    const im = wm && /\b(?:id|order_id|sku_id)\s+in\s*\(([^)]*)\)/i.exec(wm[1]);
    if (im) return {n: im[1].split(',').length, total, where: true};
    if (!an.refs.length) return {n: 1, total: 1};
    return {n: wm ? Math.max(1, Math.round(total * 0.02)) : total, total, where: !!wm};
  }

  /* 执行（模拟）：返回结果集 */
  function qExecute(an) {
    const items = parseItems(an);
    const un = items.find(i => i.unknown);
    if (un) return {err: err('engine_error', `ERROR 1054 (42S22): Unknown column '${un.unknown}' in 'field list'`)};
    const est = estimate(an, items);
    /* 超时：大表无条件统计 */
    if ((est.agg || est.group) && !est.where && est.total >= 5e7)
      return {err: err('timeout', `查询超过 ${QCFG.timeout} 秒仍未返回（${an.refs.map(r => r.name).join(', ')} 约 ${fmtN(est.total)} 行，无过滤条件的全表统计），已执行 KILL QUERY 终止。请加索引条件缩小范围，或走「数据导出」离线处理。`, {ms: QCFG.timeout * 1000})};
    const avail = Math.max(0, est.n - an.offset), n = Math.min(an.eff || est.n, avail);
    const hasOrder = /\border\s+by\b/i.test(an.m), ctx = {n, total: est.total, desc: !hasOrder || /\border\s+by\b[^;]*\bdesc\b/i.test(an.m)};
    const rows = [];
    for (let i = 0; i < n; i++) {
      const gi = i + an.offset, row = [];
      items.forEach((it, k) => {
        const r = rng(hashOf((it.tbl ? it.tbl.n : 'x') + '#' + gi + (est.group ? 'g' : '')) + k * 131);
        let v;
        if (it.lit != null) v = it.lit;
        else if (it.agg) {
          const base = est.group ? est.total / est.group.n : est.total, scale = est.group ? 0.4 + r() * 1.2 : (est.where ? 0.002 + r() * 0.05 : 1);
          if (it.agg === 'count') v = est.agg ? Math.round(est.total * (est.where ? 0.012 + r() * 0.03 : 1)) : Math.round(base * scale / 20 + r() * 50);
          else if (it.agg === 'avg') v = (it.col && /amount|price/.test(it.col.n) ? 120 + r() * 80 : 10 + r() * 90).toFixed(2);
          else if (it.agg === 'group_concat') v = 'a,b,c';
          else if ((it.agg === 'max' || it.agg === 'min') && it.col) v = genVal(it.col, it.agg === 'max' ? 0 : 5000, r, ctx);
          else v = (base * scale / 20 * (it.col && /amount|price|gmv/.test(it.col.n) ? 96 : 1.4)).toFixed(it.col && /int/.test(it.col.t) ? 0 : 2);
        } else if (est.group && it.col && est.group.enum && it.col.n.toLowerCase() === est.group.key) v = est.group.enum[i % est.group.enum.length];
        else if (est.group && est.group.key === 'month' && (it.fn || (it.col && /date|time/.test(it.col.t)))) v = fmtD(Date.UTC(/2025/.test(an.refs[0] ? an.refs[0].name : '') ? 2025 : 2026, (/2025/.test(an.refs[0] ? an.refs[0].name : '') ? 11 : 9) - i, 1)).slice(0, 7);
        else if (est.group && (/^(date|date_format|day|from_unixtime|cast)$/i.test(it.fn || '') || (it.col && /^(day|date)$/i.test(it.col.n)))) v = fmtD(T0 - i * 86400000);
        else if (it.col && !it.fn) v = genVal(it.col, gi, r, ctx);
        else if (/^(date|day)$/i.test(it.fn)) v = fmtD(T0 - gi * 3600000);
        else if (/^(now|current_timestamp|sysdate|getdate)$/i.test(it.fn) || /^now\s*\(/i.test(it.expr)) v = fmtDT(T0);
        else if (it.col) v = genVal(it.col, gi, r, ctx);
        else v = /^\d/.test(it.expr) ? it.expr : '1';
        if (it.mask && v != null) v = maskVal(it.col, v);
        row.push(v);
      });
      rows.push(row);
    }
    const masked = items.filter(i => i.mask).map(i => i.label);
    return {ok: true, kind: 'rows', cols: items.map(i => ({n: i.label, mask: i.mask, num: !!(i.agg || (i.col && !i.fn && /int|decimal|double|float/.test(i.col.t)))})), rows, n, masked,
      truncated: !!an.capped && n >= an.eff && est.n > n + an.offset, est: est.n,
      ms: Math.round(8 + (est.total > 1e6 && !est.where ? 40 + (hashOf(an.sql) % 120) : hashOf(an.sql) % 22) + n * 0.04)};
  }

  /* SHOW / DESC 类结果 */
  function qShow(an, inst, db) {
    const m = an.m, d = dbsOf(inst.id).find(x => x.db === db) || {tables: []}, rv = isReadView(inst);
    const mk = (cols, rows) => ({ok: true, kind: 'rows', cols: cols.map(n => ({n, mask: false})), rows, n: rows.length, masked: [], truncated: false, ms: 3 + rows.length});
    if (/^\s*show\s+(databases|schemas)/i.test(m)) {
      let list = dbsOf(inst.id).map(x => x.db);
      if (!an.admin) list = list.filter(x => qPolicy(inst, x).mode !== 'none');
      return mk(['Database'], list.sort().map(x => [x]));
    }
    if (/^\s*show\s+(?:full\s+)?tables/i.test(m)) { const list = d.tables.filter(t => !rv || /^v_/.test(t.n)); return /full/i.test(m) ? mk(['Tables_in_' + db, 'Table_type'], list.map(t => [t.n, t.view ? 'VIEW' : 'BASE TABLE'])) : mk(['Tables_in_' + db], list.map(t => [t.n])); }
    if (/^\s*show\s+table\s+status/i.test(m)) return mk(['Name', 'Engine', 'Rows', 'Data_length', 'Index_length', 'Create_time', 'Comment'], d.tables.filter(t => !t.view && (!rv || /^v_/.test(t.n))).map(t => [t.n, t.eng || 'InnoDB', t.rows, Math.round(t.size * 1048576 * 0.72), Math.round(t.size * 1048576 * 0.28), '2024-03-12 09:30:11', t.c]));
    const tb = an.refs[0] && an.refs[0].t;
    if (/^\s*(show\s+(full\s+)?(columns|fields)|desc|describe)\b/i.test(m)) {
      if (!tb) return {err: err('engine_error', 'ERROR 1146 (42S02): 表不存在')};
      return mk(['Field', 'Type', 'Null', 'Key', 'Default', 'Extra'], tb.cols.map(c => [c.n, c.t, c.nul ? 'YES' : 'NO', c.key, c.def === 'auto_increment' || c.def === '' ? null : c.def, c.def === 'auto_increment' ? 'auto_increment' : '']));
    }
    if (/^\s*show\s+(index|indexes|keys)/i.test(m)) {
      if (!tb) return {err: err('engine_error', 'ERROR 1146 (42S02): 表不存在')};
      const rows = []; (tb.idx || []).forEach(ix => ix[1].forEach((c, k) => rows.push([tb.n, ix[2] ? 0 : 1, ix[0], k + 1, c, 'A', Math.round(tb.rows / (ix[2] ? 1 : 12)), 'BTREE'])));
      return mk(['Table', 'Non_unique', 'Key_name', 'Seq_in_index', 'Column_name', 'Collation', 'Cardinality', 'Index_type'], rows);
    }
    if (/^\s*show\s+create\s+(table|view)/i.test(m)) { if (!tb) return {err: err('engine_error', 'ERROR 1146 (42S02): 表不存在')}; return mk([tb.view ? 'View' : 'Table', 'Create ' + (tb.view ? 'View' : 'Table')], [[tb.n, ddlOf(inst.id, db, tb)]]); }
    if (/processlist/i.test(m)) return mk(['Id', 'User', 'Host', 'db', 'Command', 'Time', 'State', 'Info'], [[88231, 'app_order', '10.20.8.14:51022', 'order_db', 'Query', 3, 'Sending data', 'select id, order_no from orders where …'], [88240, 'dbtool_ro', '10.20.9.3:40210', db, 'Query', 0, 'starting', 'show processlist'], [88102, 'app_order', '10.20.8.15:50133', 'order_db', 'Sleep', 41, '', null]]);
    return mk(['Variable_name', 'Value'], [['max_connections', '2000'], ['innodb_buffer_pool_size', '34359738368'], ['long_query_time', '1.000000'], ['read_only', inst.role === 'slave' ? 'ON' : 'OFF']]);
  }

  /* ---------- 执行计划 ---------- */
  function qExplain(an) {
    const m = an.m, refs = an.refs.filter(r => r.t);
    const wm = /\bwhere\b([\s\S]*?)(?=\bgroup\s+by\b|\border\s+by\b|\blimit\b|\bhaving\b|$)/i.exec(m);
    let wtxt = wm ? wm[1] : '';
    const fnCols = [], fnRe = /\b([a-z_]+)\s*\(\s*(?:[\w$]+\s*\.\s*)?([\w$]+)\s*\)\s*(?:=|>=|<=|>|<|<>|!=|\bbetween\b|\bin\b)/gi; let x;
    while ((x = fnRe.exec(wtxt))) if (!/^(count|sum|avg|max|min|in|and|or)$/i.test(x[1])) fnCols.push({fn: x[1], col: x[2].toLowerCase()});
    wtxt = wtxt.replace(fnRe, ' ');
    const preds = [], pre = /(?:([\w$]+)\s*\.\s*)?([\w$]+)\s*(=|>=|<=|<>|!=|>|<|\bnot\s+in\b|\bin\b|\bbetween\b|\blike\b)/gi;
    while ((x = pre.exec(wtxt))) if (!/^(and|or|not|where|select|null)$/i.test(x[2])) preds.push({q: x[1] || '', col: x[2].toLowerCase(), op: x[3].toLowerCase()});
    const joinEq = [], jre = /([\w$]+)\s*\.\s*([\w$]+)\s*=\s*([\w$]+)\s*\.\s*([\w$]+)/g, onTxt = m.replace(/\bwhere\b[\s\S]*$/i, '');
    while ((x = jre.exec(onTxt))) joinEq.push([[x[1], x[2].toLowerCase()], [x[3], x[4].toLowerCase()]]);
    const orderM = /\border\s+by\s+([\s\S]+?)(?=\blimit\b|$)/i.exec(m), hasGroup = /\bgroup\s+by\b/i.test(m);
    const tips = [], rows = [];
    refs.forEach((rf, idx) => {
      const t = rf.t, q = (rf.alias || rf.name).toLowerCase(), mine = c => t.cols.some(cc => cc.n.toLowerCase() === c);
      const my = preds.filter(p => (p.q ? p.q.toLowerCase() === q || p.q.toLowerCase() === rf.name.toLowerCase() : (refs.length === 1 || idx === 0)) && mine(p.col)).map(p => ({col: p.col, eq: p.op === '=' || p.op === 'in', range: /[<>]|between/.test(p.op)}));
      if (idx > 0) joinEq.forEach(([a, b]) => { [[a, b], [b, a]].forEach(([me, oth]) => { if (me[0].toLowerCase() === q && mine(me[1]) && oth[0].toLowerCase() !== q) my.push({col: me[1], eq: true, range: false}); }); });
      let best = null;
      (t.idx || []).forEach(ix => {
        const c0 = ix[1][0].toLowerCase(), p = my.find(p => p.col === c0 && (p.eq || p.range)); if (!p) return;
        const matched = ix[1].filter(c => my.some(pp => pp.col === c.toLowerCase() && pp.eq)).length, score = (p.eq ? 20 : 8) + matched * 3 + (ix[2] ? 2 : 0);
        if (!best || score > best.score) best = {ix, p, score, full: ix[2] && matched === ix[1].length};
      });
      const fnHit = fnCols.filter(f => mine(f.col));
      const possible = (t.idx || []).filter(ix => my.some(p => p.col === ix[1][0].toLowerCase()) || fnHit.some(f => f.col === ix[1][0].toLowerCase())).map(ix => ix[0]);
      let type, key = null, rowsE, filtered, extra = [], klen = '';
      if (best) {
        type = best.full ? (idx === 0 ? 'const' : 'eq_ref') : best.p.eq ? 'ref' : 'range'; key = best.ix[0];
        rowsE = type === 'const' || type === 'eq_ref' ? 1 : type === 'ref' ? Math.max(2, 3 + hashOf(t.n + key) % 180) : Math.max(100, Math.round(t.rows * 0.03));
        filtered = '100.00'; if (type === 'range') extra.push('Using index condition'); else if (my.length > 1 && type === 'ref') extra.push('Using where');
        klen = String(best.ix[1].reduce((s, c) => { const cc = t.cols.find(k => k.n === c), ty = cc ? cc.t : 'bigint'; const vm = /varchar\((\d+)\)/.exec(ty); return s + (/bigint/.test(ty) ? 8 : /datetime/.test(ty) ? 5 : vm ? 4 * +vm[1] + 2 : /tinyint/.test(ty) ? 1 : 4); }, 0));
      } else {
        type = 'ALL'; rowsE = t.rows; filtered = my.length || fnHit.length ? '10.00' : '100.00'; if (my.length || fnHit.length) extra.push('Using where');
        if (t.rows >= 1e5) tips.push({lv: 'bad', text: `对 <code>${esc(t.n)}</code> 做了全表扫描，预计扫描约 ${fmtN(t.rows)} 行（${fmtMB(t.size)}）${my.length ? '' : '，没有可用的过滤条件'}。`});
        const cand = [...new Set(my.filter(p => p.eq).map(p => p.col).concat(my.filter(p => p.range).map(p => p.col)))].filter(c => !fnHit.some(f => f.col === c));
        if (cand.length) tips.push({lv: 'info', text: `索引建议：<code>ALTER TABLE \`${esc(t.n)}\` ADD INDEX idx_${esc(cand.join('_'))}(${cand.map(c => '`' + esc(c) + '`').join(', ')});</code>（需走 SQL 上线工单；仅供参考，以 EXPLAIN 为准）`});
      }
      fnHit.forEach(f => tips.push({lv: 'warn', text: `WHERE 中对列 <code>${esc(f.col)}</code> 使用了函数 <code>${esc(f.fn)}()</code>，其上的索引无法使用；建议改写为范围条件，例如 <code>${esc(f.col)} &gt;= '2026-10-05' AND ${esc(f.col)} &lt; '2026-10-06'</code>。`}));
      if (orderM && idx === 0) {
        const oc = (orderM[1].split(',')[0].match(/([\w$]+)\s*(?:asc|desc)?\s*$/i) || [])[1];
        if (!(best && oc && best.ix[1].some(c => c.toLowerCase() === oc.toLowerCase())) && !(oc && oc.toLowerCase() === 'id' && t.cols[0].key === 'PRI')) { extra.push('Using filesort'); tips.push({lv: 'warn', text: 'ORDER BY 无法利用索引顺序，需要额外排序（Using filesort）。'}); }
      }
      if (hasGroup && idx === 0 && type === 'ALL') extra.push('Using temporary');
      rows.push([1, 'SIMPLE', t.n + (rf.alias ? ` (${rf.alias})` : ''), type, possible.join(',') || null, key, klen || null, rowsE, filtered, extra.join('; ') || null]);
    });
    if (/\blike\s+'%/i.test(an.sql)) tips.push({lv: 'warn', text: 'LIKE 以 % 开头（前导通配符），无法使用索引。'});
    const off = /\blimit\s+(\d+)\s*,|\boffset\s+(\d+)/i.exec(an.sql); if (off && +(off[1] || off[2]) >= 10000) tips.push({lv: 'warn', text: `深分页：偏移量 ${nf(off[1] || off[2])} 很大，建议改用“上一页最后一条的主键 &gt; ?”的游标方式或延迟关联。`});
    if (!rows.length) rows.push([1, 'SIMPLE', null, null, null, null, null, null, null, 'No tables used']);
    return {ok: true, kind: 'plan', cols: ['id', 'select_type', 'table', 'type', 'possible_keys', 'key', 'key_len', 'rows', 'filtered', 'Extra'], rows, tips, ms: 2 + (hashOf(an.sql) % 8)};
  }

  /* ---------- SQL 美化 ---------- */
  const KW2 = new Set('select from where and or not in is null like between group by order having limit offset join left right inner outer cross full on as distinct union all exists case when then else end desc asc show explain with top using interval'.split(' '));
  function qFormat(sql) {
    const toks = [], re = /(--[^\n]*|#[^\n]*|\/\*[\s\S]*?\*\/)|('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*")|(`[^`]*`)|([A-Za-z_][A-Za-z_0-9$]*)|(\s+)|([\s\S])/g; let x;
    while ((x = re.exec(sql))) toks.push(x[0]);
    const isWs = t => /^\s+$/.test(t), nextW = j => { j++; while (j < toks.length && isWs(toks[j])) j++; return (toks[j] || '').toLowerCase(); };
    let out = '', depth = 0, clause = '', prev = '', between = false;
    const nl = ind => { out = out.replace(/[ ]+$/, '') + '\n' + ' '.repeat(ind); };
    for (let i = 0; i < toks.length; i++) {
      const t = toks[i];
      if (isWs(t)) { if (out && !/\s$/.test(out)) out += ' '; continue; }
      if (/^[A-Za-z_]/.test(t)) {
        const lw = t.toLowerCase();
        if (depth === 0) {
          if (['from', 'where', 'having', 'limit', 'union'].includes(lw)) { nl(0); clause = lw; }
          else if ((lw === 'group' || lw === 'order') && nextW(i) === 'by') { nl(0); clause = lw; }
          else if (['left', 'right', 'inner', 'cross', 'full'].includes(lw) || (lw === 'join' && !['left', 'right', 'inner', 'cross', 'full', 'outer', 'natural'].includes(prev))) nl(0);
          else if (lw === 'select') clause = 'select';
          else if ((lw === 'and' || lw === 'or') && clause === 'where' && !between) nl(2);
        }
        if (lw === 'between') between = true; else if (lw === 'and' && between) between = false;
        out += KW2.has(lw) ? t.toUpperCase() : t; prev = lw; continue;
      }
      if (t === '(') depth++; else if (t === ')') depth = Math.max(0, depth - 1);
      out += t;
      if (t === ',' && depth === 0 && clause === 'select' && out.length - out.lastIndexOf('\n') > 72) nl(4);
      prev = t;
    }
    return out.replace(/[ ]+\n/g, '\n').trim();
  }

  /* ---------- 示例 SQL ---------- */
  function qSamples(inst, db) {
    if (isRedisInst(inst)) return RQ().samples(inst, db);
    if (isExtInst(inst)) return EQ().samples(inst, db);
    const S = inst.schema, L = [], add = (g, label, sql) => L.push({g, label, sql});
    if (S === 'order' && db === 'order_db') {
      add('正常查询', '最近订单（含脱敏手机号）', "SELECT id, order_no, user_id, status, amount, receiver_phone, created_at\nFROM orders\nWHERE status = 1\nORDER BY id DESC\nLIMIT 50;");
      add('正常查询', '订单状态分布（GROUP BY）', "SELECT status, COUNT(*) AS cnt, SUM(amount) AS gmv\nFROM orders\nWHERE created_at >= '2026-10-01'\nGROUP BY status\nORDER BY cnt DESC;");
      add('正常查询', '订单 + 明细联查', "SELECT o.order_no, o.amount, i.sku_name, i.qty, i.price\nFROM orders o\nJOIN order_items i ON i.order_id = o.id\nWHERE o.user_id = 10086\nORDER BY o.id DESC\nLIMIT 20;");
      add('正常查询', '用户收货地址（敏感列）', "SELECT user_id, name, phone, detail\nFROM user_addr\nWHERE user_id = 10086;");
      add('正常查询', '按订单号精确查', "SELECT id, order_no, user_id, status, amount, created_at\nFROM orders\nWHERE order_no = 'O2026100612345678';");
      add('正常查询', '结构：SHOW CREATE TABLE', "SHOW CREATE TABLE orders;");
      add('正常查询', '结构：DESC orders', "DESC orders;");
      add('正常查询', '执行计划：函数导致索引失效', "EXPLAIN SELECT id, order_no, amount\nFROM orders\nWHERE user_id = 10086 AND DATE(created_at) = '2026-10-06'\nORDER BY created_at DESC;");
      add('规则演示', '没带 LIMIT（自动补）', "SELECT id, order_no, amount\nFROM orders\nWHERE created_at >= '2026-10-07';");
      add('规则演示', 'LIMIT 超过上限（自动改写）', "SELECT id, order_no, amount\nFROM orders\nLIMIT 100000;");
      add('规则演示', '多语句（只执行第 1 条）', "SELECT COUNT(*) AS cnt FROM orders WHERE status = 0;\nSELECT COUNT(*) AS cnt FROM order_items;");
      add('规则演示', '无权限库（order_archive）', "SELECT id, order_no, amount\nFROM order_archive.orders_2025\nLIMIT 10;");
      add('规则演示', '大表全表统计（会超时）', "SELECT COUNT(*) FROM order_log;");
      add('会被拦截', 'UPDATE（写操作）', "UPDATE orders SET status = 4 WHERE order_no = 'O2026100612345678';");
      add('会被拦截', 'DROP TABLE（DDL）', "DROP TABLE order_log;");
      add('会被拦截', 'SELECT *', "SELECT * FROM orders LIMIT 10;");
      add('会被拦截', 'sleep() 函数', "SELECT sleep(30);");
      add('会被拦截', 'load_file()', "SELECT load_file('/etc/passwd');");
      add('会被拦截', '查 mysql.user', "SELECT host, user FROM mysql.user;");
      add('会被拦截', 'FOR UPDATE 加锁读', "SELECT id FROM orders WHERE id = 1 FOR UPDATE;");
    } else if (S === 'order' && db === 'order_archive') {
      add('正常查询', '2025 年订单抽样', "SELECT id, order_no, amount, created_at\nFROM orders_2025\nWHERE created_at >= '2025-12-01'\nORDER BY id DESC\nLIMIT 50;");
      add('正常查询', '按月汇总', "SELECT DATE_FORMAT(created_at, '%Y-%m') AS ym, COUNT(*) AS cnt, SUM(amount) AS gmv\nFROM orders_2025\nGROUP BY DATE_FORMAT(created_at, '%Y-%m')\nORDER BY ym;");
      add('会被拦截', 'DELETE（写操作）', "DELETE FROM orders_2025 WHERE created_at < '2025-02-01';");
    } else if (S === 'pay') {
      add('正常查询', '近期支付单', "SELECT id, pay_no, order_no, channel, amount, status, paid_at\nFROM payments\nWHERE status = 1\nORDER BY id DESC\nLIMIT 50;");
      add('正常查询', '各渠道支付汇总', "SELECT channel, COUNT(*) AS cnt, SUM(amount) AS total_amount\nFROM payments\nWHERE paid_at >= '2026-10-01'\nGROUP BY channel;");
      add('正常查询', '支付单关联退款', "SELECT p.pay_no, p.amount, r.refund_no, r.amount AS refund_amount\nFROM payments p\nJOIN refunds r ON r.pay_no = p.pay_no\nORDER BY r.id DESC\nLIMIT 20;");
      add('正常查询', '绑定银行卡（敏感列）', "SELECT user_id, card_no, holder, id_card\nFROM bank_cards\nWHERE user_id = 10086;");
      add('正常查询', '执行计划', "EXPLAIN SELECT pay_no, amount FROM payments WHERE order_no = 'O2026100612345678';");
      add('规则演示', '没带 LIMIT（自动补）', "SELECT pay_no, order_no, amount\nFROM payments\nWHERE channel = 'alipay';");
      add('会被拦截', 'UPDATE（写操作）', "UPDATE payments SET status = 2 WHERE pay_no = 'P2026100612345678';");
      add('会被拦截', 'SELECT *', "SELECT * FROM payments LIMIT 10;");
    } else if (S === 'user') {
      add('正常查询', '用户基本信息（含脱敏）', "SELECT id, username, phone, email, level, created_at\nFROM users\nWHERE level >= 3\nORDER BY id DESC\nLIMIT 50;");
      add('正常查询', '会员等级分布', "SELECT level, COUNT(*) AS cnt\nFROM users\nGROUP BY level\nORDER BY level;");
      add('正常查询', '用户近期登录', "SELECT user_id, ip, ua, created_at\nFROM login_log\nWHERE user_id = 10086\nORDER BY id DESC\nLIMIT 20;");
      add('正常查询', '实名资料（敏感列）', "SELECT user_id, real_name, id_card, birthday\nFROM user_profile\nWHERE user_id = 10086;");
      add('规则演示', '大表全表统计（会超时）', "SELECT COUNT(*) FROM login_log;");
      add('会被拦截', 'UPDATE（写操作）', "UPDATE users SET level = 5 WHERE id = 1;");
    } else if (S === 'mssql') {
      add('正常查询', '近期销售（T-SQL 用 TOP）', "SELECT TOP 50 sale_id, date_key, customer_key, amount\nFROM fact_sales\nORDER BY sale_id DESC;");
      add('正常查询', '客户维度（含脱敏）', "SELECT customer_key, name, phone, region\nFROM dim_customer\nWHERE region = N'华东';");
      add('规则演示', '没带 TOP（自动补）', "SELECT sale_id, amount\nFROM fact_sales\nWHERE date_key = 20261006;");
      add('会被拦截', 'LIMIT（T-SQL 不支持）', "SELECT sale_id, amount FROM fact_sales LIMIT 10;");
      add('会被拦截', 'DELETE（写操作）', "DELETE FROM fact_sales WHERE date_key < 20250101;");
    } else if (S === 'view') {
      add('正常查询', '订单只读视图', "SELECT id, order_no, status, amount, created_at\nFROM v_orders\nWHERE status = 1\nORDER BY id DESC\nLIMIT 50;");
      add('正常查询', '订单汇总视图', "SELECT day, cnt, gmv\nFROM v_order_summary\nORDER BY day DESC\nLIMIT 30;");
      add('会被拦截', '直接查底层表 orders（read_view）', "SELECT id, order_no, amount FROM orders LIMIT 10;");
    }
    if (!L.length) { const t = ((dbsOf(inst.id).find(d => d.db === db) || {}).tables || [])[0]; if (t) add('正常查询', '示例：' + t.n, `SELECT ${t.cols.slice(0, 5).map(c => c.n).join(', ')}\nFROM ${t.n}\nLIMIT 50;`); }
    return L;
  }

  /* ============================================================================
     二、在线查询：页面
     ========================================================================== */
  let HID = 200;
  const mkH = (user, inst, db, sql, at, ms, rows, st, msg) => ({id: ++HID, user, inst, db, sql, at, ms, rows, st, msg: msg || ''});
  const QHIST = [
    mkH('rd_zhao', 1, 'order_db', "SELECT id, order_no, user_id, status, amount, created_at FROM orders WHERE user_id = 8812094 ORDER BY id DESC LIMIT 20", '2026-10-07 09:48:12', 36, 20, '成功'),
    mkH('rd_zhao', 1, 'order_db', "SELECT status, COUNT(*) AS cnt, SUM(amount) AS gmv FROM orders WHERE created_at >= '2026-10-06' GROUP BY status", '2026-10-07 09:31:40', 412, 5, '成功'),
    mkH('rd_zhao', 1, 'order_db', "UPDATE orders SET status = 4 WHERE order_no = 'O2026100612345678'", '2026-10-07 09:29:03', 0, 0, '被拦截', '只能执行查询类语句（SELECT、结构类 SHOW、EXPLAIN）；当前语句是 UPDATE。'),
    mkH('rd_zhao', 1, 'order_db', "SELECT id, order_no, receiver_phone FROM orders WHERE order_no = 'O2026100611223344'", '2026-10-06 18:12:55', 12, 1, '成功'),
    mkH('rd_zhao', 7, 'order_db', "SELECT o.order_no, i.sku_name, i.qty FROM orders o JOIN order_items i ON i.order_id = o.id WHERE o.user_id = 10086 LIMIT 50", '2026-10-06 16:40:21', 88, 31, '成功'),
    mkH('rd_zhao', 1, 'order_archive', "SELECT id, order_no FROM orders_2025 LIMIT 10", '2026-10-06 15:02:09', 0, 0, '被拦截', '你没有 order_archive.orders_2025 的查询权限。'),
    mkH('rd_zhao', 8, 'order_db', "SELECT day, cnt, gmv FROM v_order_summary ORDER BY day DESC LIMIT 30", '2026-10-05 11:20:44', 9, 30, '成功'),
    mkH('rd_zhao', 1, 'order_db', "SELECT COUNT(*) FROM order_log", '2026-10-02 14:08:31', 60000, 0, '超时', '查询超过 60 秒仍未返回，已 KILL QUERY。'),
    mkH('tl_wang', 1, 'order_db', "SELECT DATE(created_at) AS d, COUNT(*) AS cnt FROM orders WHERE created_at >= '2026-09-30' GROUP BY DATE(created_at) ORDER BY d", '2026-10-07 10:12:07', 604, 8, '成功'),
    mkH('tl_wang', 3, 'pay_db', "SELECT channel, COUNT(*) AS cnt, SUM(amount) AS total_amount FROM payments WHERE paid_at >= '2026-10-01' GROUP BY channel", '2026-10-07 10:03:50', 1280, 3, '成功'),
    mkH('tl_wang', 3, 'pay_db', "SELECT pay_no, order_no, amount, status FROM payments WHERE status = 2 ORDER BY id DESC LIMIT 100", '2026-10-06 17:45:12', 95, 100, '成功'),
    mkH('dba_li', 4, 'user_db', "SELECT user_id, ip, ua, created_at FROM login_log WHERE user_id = 10086 ORDER BY id DESC LIMIT 20", '2026-10-07 09:55:18', 21, 20, '成功'),
    mkH('dba_li', 1, 'order_db', "SELECT id, order_no, amount FROM orders WHERE created_at >= '2026-10-07 08:00:00' AND status = 0 LIMIT 500", '2026-10-07 08:51:02', 233, 500, '成功'),
    mkH('dba_li', 3, 'pay_db', "SHOW CREATE TABLE payments", '2026-10-06 11:30:40', 4, 1, '成功'),
    mkH('admin', 1, 'order_db', "SELECT id, order_no, user_id, amount FROM orders WHERE user_id IN (10086, 10087) ORDER BY id DESC LIMIT 200", '2026-10-07 09:20:33', 58, 62, '成功'),
    mkH('admin', 2, 'order_db', "SELECT sleep(30)", '2026-10-05 20:11:09', 0, 0, '成功', ''),
    mkH('qa_sun', 7, 'order_db', "SELECT id, order_no, status, amount FROM orders WHERE order_no LIKE 'O20261006%' LIMIT 100", '2026-10-07 10:20:15', 142, 100, '成功'),
    mkH('qa_sun', 4, 'user_db', "SELECT id, username, phone, email FROM users WHERE level >= 3 LIMIT 50", '2026-10-06 14:00:27', 46, 50, '成功'),
    mkH('rd_zhao', 11, '0', "GET counter:order:today", '2026-10-07 10:05:41', 2, 1, '成功'),
    mkH('rd_zhao', 11, '0', "HGETALL order:detail:80123456", '2026-10-07 10:03:17', 2, 10, '成功'),
    mkH('rd_zhao', 11, '0', "KEYS order:*", '2026-10-07 09:58:02', 0, 0, '被拦截', 'KEYS 会在大库上长时间阻塞 Redis，已禁止；请改用 SCAN 0 MATCH 模式 COUNT 100 分批读取（或用左侧 key 浏览）'),
    mkH('rd_zhao', 11, '0', "HGETALL big:order:index", '2026-10-07 09:56:30', 3, 0, '被拦截', 'key 太大：共 182044 个元素，超过在线查询的全量读取阈值 5000；请改用 HSCAN big:order:index 0 COUNT 100 分页读取'),
    mkH('rd_zhao', 12, '0', "GET order:status:80123456", '2026-10-06 17:20:11', 0, 0, '被拦截', '你无 Redis db0 的查询权限，请先申请。'),
    mkH('tl_wang', 11, '0', "SCAN 0 MATCH order:status:* COUNT 100", '2026-10-07 10:09:48', 3, 31, '成功'),
    mkH('dba_li', 13, '0', "SCAN 0 MATCH session:* COUNT 100", '2026-10-07 09:41:26', 3, 68, '成功'),
    mkH('dba_li', 13, '1', "HGETALL user:profile:10086", '2026-10-07 09:40:03', 2, 6, '成功'),
    mkH('admin', 11, '0', "SET counter:order:today 0", '2026-10-06 21:15:09', 0, 0, '被拦截', 'SET 会修改数据，在线查询只允许只读命令；数据变更请通过「SQL 上线」提交 Redis 工单'),
    mkH('qa_sun', 13, '1', "GET ratelimit:api:10.20.8.14", '2026-10-06 15:12:44', 2, 1, '成功'),
    mkH('rd_zhao', 14, 'order_core', "select id, order_no, user_id, status, amount, created_at from public.orders where status = 1 order by id desc limit 50", '2026-10-07 10:12:33', 31, 50, '成功'),
    mkH('rd_zhao', 14, 'order_core', "select o.order_no, i.sku_name, i.qty from public.orders o join public.order_items i on i.order_id = o.id where o.user_id = 10086 limit 20", '2026-10-07 10:08:50', 44, 7, '成功'),
    mkH('rd_zhao', 14, 'order_core', "select pg_sleep(5)", '2026-10-07 10:05:12', 0, 0, '被拦截', '在线查询只允许只读语句（SELECT / WITH … SELECT / VALUES / TABLE / EXPLAIN / SHOW）：不允许调用函数 pg_sleep（会让会话睡眠并占住连接）。数据变更请通过「SQL 上线」提交工单。'),
    mkH('rd_zhao', 14, 'order_core', "select * from public.attachments limit 5", '2026-10-06 17:41:09', 0, 0, '被拦截', '你无 order_core.public.attachments 表的查询权限，请先申请。'),
    mkH('rd_zhao', 16, 'order_mongo', "db.orders.find({\"userId\":10086}).sort({\"createdAt\":-1}).limit(10)", '2026-10-07 09:50:21', 19, 4, '成功'),
    mkH('rd_zhao', 16, 'order_mongo', "db.orders.drop()", '2026-10-07 09:48:02', 0, 0, '被拦截', 'drop 是写操作 / 管理操作，在线查询只读，已禁止'),
    mkH('rd_zhao', 18, 'olap_order', "select order_date, count() as orders, sum(amount) as gmv from fact_orders where order_date >= today() - 7 group by order_date order by order_date", '2026-10-07 09:30:17', 412, 8, '成功'),
    mkH('rd_zhao', 18, 'olap_order', "select * from system.users", '2026-10-07 09:28:40', 0, 0, '被拦截', '不允许访问 system.users：system 库在线查询只开放库表结构类表（columns / data_skipping_indices / data_type_families / databases / one / parts / parts_columns / table_engines / tables / time_zones）；账号权限、他人 SQL、集群与凭据信息请通过 DBA 页面查看'),
    mkH('dba_li', 15, 'bi_dw', "select date_key, count(*) as orders, sum(amount) as gmv from dw.fact_orders where date_key >= 20261001 group by date_key order by date_key", '2026-10-07 10:15:44', 872, 7, '成功'),
    mkH('dba_li', 17, 'user_mongo', "db.users.find({\"level\":{\"$gte\":4}}).sort({\"_id\":-1}).limit(10)", '2026-10-07 09:22:09', 27, 10, '成功'),
    mkH('dba_li', 19, 'dw_report', "select uniqExact(user_id) from events_raw", '2026-10-06 16:12:30', 3, 0, '失败', 'ClickHouse 错误 158（超过读取行数 / 字节上限，请加过滤条件或缩小范围）：Limit for rows (controlled by \'max_rows_to_read\' setting) exceeded, max rows: 100.00 million, current rows: 12,480,330,990. (TOO_MANY_ROWS)'),
    mkH('admin', 18, 'olap_order', "select version()", '2026-10-06 21:02:11', 5, 1, '成功'),
    mkH('qa_sun', 17, 'user_mongo', "db.users.find({\"uid\":10086})", '2026-10-06 14:33:50', 12, 1, '成功'),
  ];
  QHIST.sort((a, b) => b.at.localeCompare(a.at));
  const ST_CLS = {'成功': 'ok', '被拦截': 'warn', '失败': 'bad', '超时': 'bad', '已取消': ''};

  const Q = {inst: 0, db: '', tabs: [], cur: 0, seq: 0, pageLimit: 500, treeKw: '', open: {}, running: null, showAll: false, lastSub: null};
  const QH = {kw: '', inst: '', from: '', to: '', user: ''};
  const qTab = () => Q.tabs.find(t => t.id === Q.cur) || Q.tabs[0];
  function qNewTab(sql) { const t = {id: ++Q.seq, name: '查询 ' + Q.seq, sql: sql || '', msg: null, res: null, plan: null, rtab: 'res'}; Q.tabs.push(t); Q.cur = t.id; return t; }
  function qEnsure(insts) {
    if (!insts.some(i => i.id == Q.inst)) { Q.inst = insts[0].id; Q.db = ''; }
    const dbs = qDbs(Q.inst);
    if (isRedisInst(instOf(Q.inst))) Q.db = String(Q.db).replace(/^db/i, ''); if (!dbs.some(d => d.db === Q.db)) Q.db = (dbs[0] || {}).db || '';
    if (!Q.tabs.length) { const s = qSamples(instOf(Q.inst), Q.db)[0]; qNewTab(s ? s.sql : ''); }
  }
  const qHostNow = () => TODAY + ' ' + now().slice(11);
  function qLog(inst, sql, st, ms, rows, msg) {
    QHIST.unshift(mkH(ME.login, inst.id, Q.db, sql, qHostNow(), ms, rows, st, msg));
  }

  /* ============================================================================
     Redis 实例：命令模式。命令检查 / 受限执行 / 结果整形 / key 浏览 / 示例在 p_redisq.js（window.RedisQuery）；
     这里只管页面状态与 MySQL / SQL Server 共用的权限、历史、审计、消息条。切回其它实例时本段完全不起作用。
     ========================================================================== */
  const RQ = () => window.RedisQuery;
  const isRedisInst = inst => !!inst && inst.type === 'Redis';
  const qDbs = instId => isRedisInst(instOf(instId)) ? RQ().dbs(instOf(instId)) : dbsOf(instId);
  const dbTxt = (instId, db) => isRedisInst(instOf(instId)) ? 'db' + String(db).replace(/^db/i, '') : db;
  const dbNum = db => String(db).replace(/^db/i, '');
  /* 限制提示条（对应 policybar.js 的 Redis 分支） */
  function qRedisChips(inst, pol) {
    const chips = [], lbl = 'db' + dbNum(Q.db);
    if (!can('query.run')) chips.push(['bad', '🔒 你没有「执行查询」权限（query.run），运行按钮已置灰']);
    if (pol.mode === 'admin') chips.push(['ok', `管理员 · 不受库表授权限制 · 最多 ${nf(pol.limit)} 行`]);
    else if (pol.mode === 'group') chips.push(['ok', `资源组内实例 · 免库表授权 · 最多 ${nf(pol.limit)} 行`]);
    else if (pol.mode === 'priv') {
      const days = Math.round((Date.parse(pol.expire) - Date.parse(TODAY)) / 86400000), lmin = Math.min(...pol.privs.map(p => +p.limit));
      chips.push(['ok', `查询权限：${lbl} · 最多 ${nf(lmin)}${lmin !== pol.limit ? '–' + nf(pol.limit) : ''} 行`]);
      chips.push([days <= 14 ? 'warn' : '', `权限到期 ${pol.expire}${days <= 14 ? `（${days} 天后）` : ''}`]);
    } else chips.push(['bad', `你在 ${lbl} 暂无查询权限${pol.expired ? `（权限已于 ${pol.expired} 过期）` : ''} <a href="#qapply/new?inst=${inst.id}&db=${lbl}">去申请</a>`]);
    chips.push(['warn', 'Redis 查询结果不做脱敏']);
    chips.push(['', `超时 ${QCFG.timeout}s`]);
    chips.push(['', '只读：白名单命令（GET / HGETALL / LRANGE / SCAN …），一次一条']);
    return `<div class="pq-chips">${chips.map(([c, h]) => `<span class="pq-chip ${c}">${h}</span>`).join('')}</div>`;
  }
  /* 选中文本优先；否则光标所在行；光标行是空行时用全部内容（一次只执行一条命令，多行会被拒绝并提示） */
  function qRedisText(ed) {
    const v = ed.value, a = ed.selectionStart, b = ed.selectionEnd;
    if (a !== b && v.slice(a, b).trim()) return v.slice(a, b);
    const s = a > 0 ? v.lastIndexOf('\n', a - 1) + 1 : 0, e = v.indexOf('\n', a), line = v.slice(s, e < 0 ? v.length : e);
    return line.trim() ? line : v;
  }
  /* 检查流水线：命令静态检查（不看权限）→ 库权限 → 实例可达 → 行数上限 */
  function qCheckRedis(text, inst, db, pageLimit) {
    const st = RQ().check(text);
    if (st.err) return {err: err('bad_query', st.err, {title: '命令被拦截'})};
    const n = dbNum(db), pol = qPolicy(inst, n);
    if (pol.mode === 'none') return {err: err('no_privilege', `你无 Redis db${n} 的查询权限，请先申请。`, {db: n, inst: inst.id, link: {href: `#qapply/new?inst=${inst.id}&db=db${n}`, text: '去申请查询权限'}})};
    if (inst.status === 'bad') return {err: err('engine_error', `dial tcp ${inst.host}:${inst.port}: i/o timeout —— 实例当前不可达，请联系 DBA。`, {title: 'Redis 连接失败'})};
    let limit = pol.limit; if (pageLimit > 0 && pageLimit < limit) limit = pageLimit;
    return {an: {cmd: st.cmd, line: st.line, db: n, limit, pol}};
  }
  function qRunRedis(inst, ed, tab) {
    tab.sql = ed.value;
    const text = qRedisText(ed);
    Q.showAll = false; Q.curPlan = false;
    if (!text.trim()) { tab.msg = {notes: ['先输入一条命令']}; qDrawMsg(); return; }
    const chk = qCheckRedis(text, inst, Q.db, Q.pageLimit);
    if (chk.err) { qFail(tab, text, inst, chk.err); qDrawMsg(); qDrawRes(); qDrawTabs(); return; }
    const an = chk.an, res = RQ().run(inst, an.db, an.cmd, an.limit);
    Q.running = {tab: tab.id, timer: setTimeout(() => {
      Q.running = null;
      if (res.err) qFail(tab, text, inst, Object.assign({title: res.err.code === 'engine_error' ? 'Redis 返回错误' : '命令被拦截'}, res.err), 0);
      else {
        tab.msg = {notes: res.notices};
        tab.res = {redis: true, cols: res.columns.map(n => ({n})), rows: res.rows, n: res.rows.length, ms: res.ms, instName: inst.name, db: an.db, executed: res.executed, cursor: res.cursor, nextCmd: res.nextCmd, srcText: text, masked: [], inst: inst.id, sql: res.executed};
        tab.rtab = 'res';
        qLog(inst, res.executed, '成功', res.ms, res.rows.length);
        audit('query.run', `${inst.name}/db${an.db} 执行命令（${res.rows.length} 行，${res.ms}ms）：${clip(oneLine(res.executed), 70)}`, '成功', inst.name + ' / db' + an.db);
      }
      qDrawTabs(); qDrawBar(); qDrawMsg(); qDrawRes();
    }, res.err ? 160 : Math.min(500, 120 + res.ms * 4))};
    qDrawTabs(); qDrawBar(); qDrawRes();
  }
  /* SCAN 系列的“下一页”：把游标换成新的再跑一次 */
  ON.qryRNext = () => {
    const t = qTab(), r = t.res, ed = $('#pqEd'); if (!r || !r.nextCmd || !ed) return;
    const i = r.srcText ? ed.value.indexOf(r.srcText) : -1;
    ed.value = i >= 0 ? ed.value.slice(0, i) + r.nextCmd + ed.value.slice(i + r.srcText.length) : r.nextCmd;
    const p = (i >= 0 ? i : 0) + r.nextCmd.length; ed.setSelectionRange(p, p); t.sql = ed.value;
    qRun(false);
  };
  /* 左侧 key 浏览：搜索框是匹配模式，回车才扫描（SCAN）；点 key 生成读取命令；“加载更多”继续 */
  const RK = {inst: 0, db: '', match: '*', keys: [], cursor: '0', done: true, loading: false, error: null, seq: 0};
  function rkDraw() {
    const el = $('#pqTree'); if (!el) return;
    if (RK.inst !== Q.inst || RK.db !== Q.db) { Object.assign(RK, {inst: Q.inst, db: Q.db, match: RQ().toMatch(Q.treeKw), keys: [], cursor: '0', done: true, error: null}); rkFetch(); }
    else el.innerHTML = RQ().keysHtml(RK);
  }
  function rkSearch() { Object.assign(RK, {match: RQ().toMatch(Q.treeKw), keys: [], cursor: '0', done: true, error: null}); rkFetch(); }
  function rkFetch() {
    const el = $('#pqTree'), inst = instOf(Q.inst), my = ++RK.seq; if (!el) return;
    const n = dbNum(Q.db);
    if (qPolicy(inst, n).mode === 'none') {      // key 浏览同样需要对该库有查询权限
      RK.loading = false; RK.error = {msg: `你无 Redis db${n} 的查询权限，请先申请。`, link: {href: `#qapply/new?inst=${inst.id}&db=db${n}`, text: '去申请查询权限'}};
      el.innerHTML = RQ().keysHtml(RK); return;
    }
    RK.loading = true; RK.error = null; el.innerHTML = RQ().keysHtml(RK);
    setTimeout(() => {
      if (my !== RK.seq) return;
      const r = RQ().browse(inst, n, RK.match, RK.cursor, 200);
      RK.loading = false;
      if (r.err) RK.error = {msg: r.err}; else { RK.keys = RK.keys.concat(r.keys); RK.cursor = r.cursor; RK.done = r.done; }
      const el2 = $('#pqTree'); if (el2) el2.innerHTML = RQ().keysHtml(RK);
    }, 120);
  }
  ON.rkMore = () => { if (!RK.loading && !RK.done) rkFetch(); };
  ON.rkPick = d => {
    const k = RK.keys[+d.i], ed = $('#pqEd'); if (!k || !ed) return;
    const cmd = RQ().commandFor(k.type, k.arg);
    if (ed.value.trim()) { qTab().sql = ed.value; qNewTab(cmd); qLoadEd(); qDrawTabs(); qDrawMsg(); qDrawRes(); }
    else { ed.value = cmd; qTab().sql = cmd; qDrawMsg(); }
  };
  /* ============================================================================
     PostgreSQL / MongoDB / ClickHouse 实例（EXT_TYPES）：语句检查 / 受限执行 / 结果整形 / 库表树 / 示例 / 数据字典在 p_engines.js（window.EngineQuery）；
     这里只管页面状态与三类共用的权限、历史、审计、消息条（流程同 internal/query/multi.go：引擎静态检查 → 库表授权 → 执行 → 日志）。切回其它实例时本段完全不起作用。
     ========================================================================== */
  const EQ = () => window.EngineQuery;
  const famOf = inst => isRedisInst(inst) ? 'redis' : isExtInst(inst) ? EQ().family(inst) : 'sql';
  const extSameTable = (inst, p, name) => p === name || (inst.type === 'PgSQL' && !String(p).includes('.') && 'public.' + p === name);
  /* 某张表（集合）的生效权限：库名 / 表名区分大小写，PostgreSQL 权限里不带点的表名视为 public 下的表（auth_multi.go 的 AuthorizeTargets）；返回 {limit, src} 或 null */
  function extObjPriv(inst, db, name) {
    const pol = qPolicy(inst, db);
    if (pol.all) return {limit: pol.limit, src: pol.src};
    let dbLim = null, tbLim = null;
    for (const p of pol.privs || []) {
      const t = pTables(p);
      if (t === '*') dbLim = dbLim == null ? +p.limit : Math.min(dbLim, +p.limit);
      else if (t.some(x => extSameTable(inst, x, name))) tbLim = tbLim == null ? +p.limit : Math.min(tbLim, +p.limit);
    }
    if (dbLim == null && tbLim == null) return null;
    return {limit: dbLim == null ? tbLim : tbLim == null ? dbLim : Math.min(dbLim, tbLim), src: dbLim != null && tbLim != null ? `${db}.${name} 的库/表权限` : tbLim != null ? `${db}.${name} 表权限` : `${db} 库权限`};
  }
  const extDbPriv = (inst, db) => { const pol = qPolicy(inst, db); if (pol.all) return {limit: pol.limit, src: pol.src}; const l = (pol.privs || []).filter(p => pTables(p) === '*'); return l.length ? {limit: Math.min(...l.map(p => +p.limit)), src: `${db} 库权限`} : null; };
  const extNoPriv = (inst, db, name, msg) => { const pol = qPolicy(inst, db); return err('no_privilege', msg + (pol.mode === 'none' && pol.expired ? `（你对 ${db} 的查询权限已于 ${pol.expired} 过期）` : ''), {db, table: name || '', inst: inst.id, link: {href: `#qapply/new?inst=${inst.id}&db=${encodeURIComponent(db)}${name ? '&table=' + encodeURIComponent(name) : ''}`, text: '去申请查询权限'}}); };
  const extConnFail = inst => {
    const f = famOf(inst), hp = `${inst.host}:${inst.port}`;
    return f === 'mongo' ? `无法连接 MongoDB 实例：server selection error: context deadline exceeded, current topology: { Type: Unknown, Servers: [{ Addr: ${hp}, Type: Unknown }] }`
      : f === 'clickhouse' ? `无法连接 ClickHouse HTTP 接口（${hp}）：dial tcp ${hp}: i/o timeout` : `dial tcp ${hp}: connect: connection timed out`;
  };
  /* 限制提示条（三类共用）：权限来源 / 到期 / 不做脱敏 / 超时 / 只读规则 */
  function extChips(inst, pol) {
    const chips = [], E = EQ(), fam = E.family(inst);
    if (!can('query.run')) chips.push(['bad', '🔒 你没有「执行查询」权限（query.run），运行按钮已置灰']);
    if (isReadView(inst)) chips.push(['bad', E.READ_VIEW_MSG(inst)]);
    if (pol.mode === 'admin') chips.push(['ok', `管理员 · 不受库表授权限制 · 最多 ${nf(pol.limit)} 行`]);
    else if (pol.mode === 'group') chips.push(['ok', `资源组内实例 · 免库表授权 · 最多 ${nf(pol.limit)} 行`]);
    else if (pol.mode === 'priv') {
      const all = pol.privs.some(p => pTables(p) === '*'), names = [...new Set([].concat(...pol.privs.map(p => pTables(p) === '*' ? [] : pTables(p))))];
      const days = Math.round((Date.parse(pol.expire) - Date.parse(TODAY)) / 86400000), lmin = Math.min(...pol.privs.map(p => +p.limit));
      chips.push(['ok', `查询权限：${all ? Q.db + ' 全部' + (fam === 'mongo' ? '集合' : '表') : names.join('、')} · 最多 ${nf(lmin)}${lmin !== pol.limit ? '–' + nf(pol.limit) : ''} 行`]);
      chips.push([days <= 14 ? 'warn' : '', `权限到期 ${pol.expire}${days <= 14 ? `（${days} 天后）` : ''}`]);
      if (!all) chips.push(['warn', `其余${fam === 'mongo' ? '集合' : '表'}无权限 <a href="#qapply/new?inst=${inst.id}&db=${encodeURIComponent(Q.db)}">去申请</a>`]);
    } else chips.push(['bad', `你在 ${esc(Q.db)} 暂无查询权限${pol.expired ? `（权限已于 ${pol.expired} 过期）` : ''} <a href="#qapply/new?inst=${inst.id}&db=${encodeURIComponent(Q.db)}">去申请</a>`]);
    chips.push(['warn', E.UNMASKED]);
    chips.push(['', `超时 ${QCFG.timeout}s`]);
    chips.push(['', E.hints(inst).rule]);
    return `<div class="pq-chips">${chips.map(([c, h]) => `<span class="pq-chip ${c}">${h}</span>`).join('')}</div>`;
  }
  /* 检查流水线：read_view 标签 → 引擎静态检查 → 库表授权 → 实例可达 → 行数上限 */
  function qCheckExt(text, inst, db, pageLimit) {
    const E = EQ();
    if (isReadView(inst)) return {err: err('read_view_only', E.READ_VIEW_MSG(inst))};
    const c = E.check(inst, db, text);
    if (c.err) return {err: err('bad_query', c.err, {title: '语句被拦截'})};
    const pol = qPolicy(inst, db), tg = c.targets;
    let limit, src;
    if (pol.all) { limit = pol.limit; src = pol.src; }
    else if (tg.structure) { limit = ADMIN_LIMIT; src = '结构查询'; }
    else if (tg.requireDb) { const dp = extDbPriv(inst, db); if (!dp) return {err: extNoPriv(inst, db, '', tg.requireDb)}; limit = dp.limit; src = dp.src; }
    else if (!tg.objects.length) { limit = 100; src = '无表查询'; }
    else {
      limit = Infinity; src = '';
      for (const o of tg.objects) {
        const op = extObjPriv(inst, o.db, o.name);
        if (!op) return {err: extNoPriv(inst, o.db, o.name, `你无 ${o.db}.${o.name} 表的查询权限，请先申请。`)};
        if (op.limit < limit) { limit = op.limit; src = op.src; }
      }
    }
    if (inst.status === 'bad') return {err: err('engine_error', extConnFail(inst) + ' —— 实例当前不可达，请联系 DBA。', {title: E.label(inst) + ' 连接失败'})};
    if (pageLimit > 0 && pageLimit < limit) limit = pageLimit;     // 页面“返回行数”只是进一步收窄，来源仍显示权限来源
    return {an: {ck: c.ck, stmt: c.stmt, db, limit, limitSrc: src, pol}};
  }
  /* 选中文本优先，否则运行编辑器全部内容（一次一条；多条会被引擎拒绝并提示选中其中一条） */
  const extText = ed => { const v = ed.value, a = ed.selectionStart, b = ed.selectionEnd; return a !== b && v.slice(a, b).trim() ? v.slice(a, b) : v; };
  function qRunExt(inst, ed, tab) {
    tab.sql = ed.value;
    const text = extText(ed);
    Q.showAll = false; Q.curPlan = false;
    if (!text.trim()) { tab.msg = {notes: ['先输入一条语句']}; qDrawMsg(); return; }
    const chk = qCheckExt(text, inst, Q.db, Q.pageLimit);
    if (chk.err) { qFail(tab, text, inst, chk.err); qDrawMsg(); qDrawRes(); qDrawTabs(); return; }
    const an = chk.an, E = EQ(), res = E.run(inst, an.db, an.ck, an.limit);
    Q.running = {tab: tab.id, timer: setTimeout(() => {
      Q.running = null;
      if (res.err) {
        const e = res.err, msg = e.code === 'timeout' ? `查询超过 ${QCFG.timeout} 秒已终止，请缩小范围或增加过滤条件。` : e.msg;
        const title = e.title || (e.code === 'engine_error' ? E.label(inst) + ' 返回错误' : /^PostgreSQL 报错/.test(msg) ? 'PostgreSQL 返回错误' : e.code === 'bad_query' ? '语句被拦截' : undefined);
        qFail(tab, text, inst, err(e.code, msg, {title}), e.code === 'timeout' ? QCFG.timeout * 1000 : 0);
      } else {
        tab.msg = {notes: []};
        tab.res = {ext: true, fam: E.family(inst), types: res.types, cols: res.columns.map((n, i) => ({n, ty: res.types ? res.types[i] : ''})), rows: res.rows, n: res.rows.length, ms: res.ms, instName: inst.name, db: an.db, executed: res.executed, stmtKind: res.stmtKind,
          truncated: res.truncated, truncatedBy: res.truncatedBy, limit: an.limit, limitSrc: an.limitSrc, warnings: res.warnings, notices: res.notices, inst: inst.id, sql: res.executed, masked: [], maxBytes: 4194304};
        tab.rtab = 'res';
        qLog(inst, res.executed, '成功', res.ms, res.rows.length);
        audit('query.run', `${inst.name}/${an.db} 执行查询（${res.rows.length} 行，${res.ms}ms）：${clip(oneLine(res.executed), 70)}`, '成功', inst.name + ' / ' + an.db);
      }
      qDrawTabs(); qDrawBar(); qDrawMsg(); qDrawRes();
    }, res.err ? (res.err.code === 'timeout' ? 1600 : 160) : Math.min(700, 150 + res.ms * 3))};
    qDrawTabs(); qDrawBar(); qDrawRes();
  }
  /* 库表树：PostgreSQL 按 schema 分组（表名 schema.table）；Mongo 是集合（字段来自采样）；ClickHouse 的视图 ◈ / 字典 ☷ 用图标区分 */
  function extDrawTree() {
    const el = $('#pqTree'); if (!el) return;
    const inst = instOf(Q.inst), E = EQ(), fam = E.family(inst);
    if (isReadView(inst)) { el.innerHTML = notice(esc(E.READ_VIEW_MSG(inst)), 'warn', '🔒'); return; }
    const td = E.treeData(inst, Q.db);
    if (!td) { el.innerHTML = empty('该实例没有可用的库'); return; }
    const kw = Q.treeKw.trim().toLowerCase(), total = td.groups.reduce((a, g) => a + g.items.length, 0);
    let h = `<div class="tn" style="font-weight:600;cursor:default">⛁ ${esc(Q.db)}<span class="n">${total} 个${fam === 'mongo' ? '集合 / 视图' : '对象'}</span></div>`, shown = 0;
    td.groups.forEach(g => {
      let list = g.items;
      if (kw) list = list.filter(it => it.n.toLowerCase().includes(kw) || it.comment.toLowerCase().includes(kw) || (fam !== 'mongo' && it.cols.some(c => c.n.toLowerCase().includes(kw))));
      if (!list.length) return;
      if (g.label) h += `<div class="eq-grp">▾ ${esc(g.label)}<span class="n">${list.length}</span></div>`;
      list.forEach(it => {
        shown++;
        const colHit = kw && fam !== 'mongo' && !it.n.toLowerCase().includes(kw) && it.cols.some(c => c.n.toLowerCase().includes(kw)), open = !!Q.open[it.n] || colHit, lock = !extObjPriv(inst, Q.db, it.n);
        const ico = it.kind === 'dict' ? '☷' : it.view ? '◈' : '▦', badge = it.rows >= 0 ? fmtN(it.rows) : it.view ? (it.kind === 'matview' ? '物化视图' : '视图') : '-';
        h += `<div class="tn l1" title="${esc((it.comment || '') + (it.kind === 'dict' ? '（字典）' : ''))}"><span data-on="qryTog" data-t="${esc(it.n)}" style="width:12px;color:var(--muted)">${open ? '▾' : '▸'}</span><span data-on="qryIns" data-t="${esc(it.n)}" style="flex:1;overflow:hidden;text-overflow:ellipsis"><span class="eq-ico ${it.view ? 'v' : ''}">${ico}</span> ${hlText(g.label ? it.name : it.n, kw)}${lock ? ' 🔒' : ''}</span><span class="n">${badge}</span></div>`
          + (open ? it.cols.map(c => `<div class="tn l2" data-on="qryInsCol" data-c="${esc(c.n)}" title="${esc(c.cmt || '')}"><span>${c.key === 'PRI' ? '🔑 ' : ''}${hlText(c.n, kw)}</span><span class="n">${esc(c.t)}</span></div>`).join('') : '');
      });
    });
    el.innerHTML = h + (shown ? '' : empty('没有匹配的对象'));
  }
  function extIns(inst, t, ed) {
    const E = EQ();
    if (!extObjPriv(inst, Q.db, t.n)) {
      const tab = qTab(); tab.msg = {err: extNoPriv(inst, Q.db, t.n, `你无 ${Q.db}.${t.n} 表的查询权限，请先申请。`), notes: []};
      qDrawMsg(); return;
    }
    if (!ed.value.trim()) { ed.value = E.exampleFor(inst, Q.db, {n: t.n, cols: t.cols}); qTab().sql = ed.value; ed.focus(); }
    else qInsert(t.n);
  }
  /* 在 Redis / PostgreSQL / MongoDB / ClickHouse 与 MySQL / SQL Server 实例之间切换时：编辑器里还是旧实例的示例（用户没改过）就换成新实例的第一条示例；用户自己写的内容不动 */
  function qSwapSample(newId) {
    const o = instOf(Q.inst), n = instOf(newId), t = qTab(), ed = $('#pqEd');
    if (famOf(o) === famOf(n)) return;
    const cur = ((ed ? ed.value : t.sql) || '').trim();
    if (!cur || !qSamples(o, Q.db).some(s => s.sql.trim() === cur)) return;
    const s = qSamples(n, ((qDbs(newId)[0]) || {}).db || '')[0];
    t.sql = s ? s.sql : '';
  }
  pages.query = {
    render(sub) {
      const insts = visibleInstances('all');
      if (!insts.length) return `<div class="card">${empty('你所在的资源组没有关联任何实例，无法查询。请联系管理员。', '⌘')}</div>`;
      if (sub !== Q.lastSub) {                 // #query/?inst=1&db=order_db 之类的预选，只在路由变化时生效一次
        Q.lastSub = sub; const qs = qsOf(sub);
        if (qs.inst && insts.some(i => i.id == qs.inst)) { Q.inst = +qs.inst; Q.db = qs.db || ''; }
      }
      qEnsure(insts); RK.inst = 0;
      const inst = instOf(Q.inst), rdx = isRedisInst(inst), ext = isExtInst(inst), dbs = qDbs(Q.inst), tab = qTab();
      return `<div class="pq-top">
        <b>实例</b><select id="pqInst" data-onchange="qryInst" style="min-width:270px">${instOpts(insts, Q.inst)}</select>
        <b>库</b><select id="pqDb" data-onchange="qryDb">${dbs.map(d => opt(d.db, rdx ? RQ().dbLabel(d) : d.db, Q.db)).join('')}</select>
        <span>${instBadges(inst)}</span><span class="sp"></span>
        <button class="btn" data-on="qryHist">🕘 历史</button>
      </div>
      <div class="pq-wb">
        <aside class="card pq-side" style="padding:10px">
          <input type="search" id="pqTreeKw" placeholder="${rdx ? 'key 匹配，如 user:*（回车）' : ext ? (EQ().family(inst) === 'mongo' ? '搜索集合' : '搜索表 / 字段') : '搜索表 / 字段'}" data-oninput="qryTreeKw" value="${esc(Q.treeKw)}" style="width:100%;margin-bottom:8px">
          <div class="dbtree" id="pqTree"></div>
        </aside>
        <section class="card" style="min-width:0">
          <div id="pqTabs"></div>
          <textarea class="sql-ed" id="pqEd" data-oninput="qryEd" spellcheck="false" rows="9" placeholder="${rdx ? esc(RQ().editorPlaceholder) : ext ? esc(EQ().hints(inst).placeholder) : '在这里写 SQL'}">${esc(tab.sql)}</textarea>
          <div id="pqBar"></div><div id="pqLimits"></div><div id="pqMsg"></div><div id="pqRes"></div>
        </section>
      </div>`;
    },
    init() {
      qDrawAll();
      const kw = $('#pqTreeKw'); if (kw) kw.addEventListener('keydown', e => { if (e.key === 'Enter' && isRedisInst(instOf(Q.inst))) { e.preventDefault(); Q.treeKw = kw.value; rkSearch(); } });   // Redis：回车才按模式 SCAN
      const ed = $('#pqEd'); if (!ed) return;
      ed.addEventListener('keydown', e => {
        if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') { e.preventDefault(); if (can('query.run')) qRun(false); else toast('无权限：需要「执行查询」（query.run）'); }
        else if (e.key === 'Tab' && !e.shiftKey) { e.preventDefault(); const a = ed.selectionStart; ed.value = ed.value.slice(0, a) + '    ' + ed.value.slice(ed.selectionEnd); ed.setSelectionRange(a + 4, a + 4); qTab().sql = ed.value; }
      });
    }
  };
  function qDrawAll() { qDrawTabs(); qDrawTree(); qDrawBar(); qDrawLimits(); qDrawMsg(); qDrawRes(); }
  function qLoadEd() { const ed = $('#pqEd'); if (ed) ed.value = qTab().sql; }

  /* ---------- 标签页 ---------- */
  function qDrawTabs() {
    const el = $('#pqTabs'); if (!el) return;
    el.innerHTML = `<div class="pq-tabs">${Q.tabs.map(t => `<span class="pq-tab ${t.id === Q.cur ? 'on' : ''}" data-on="qryTabSel" data-id="${t.id}">${esc(t.name)}${Q.running && Q.running.tab === t.id ? ' <span class="dot warn"></span>' : ''}<b data-on="qryTabClose" data-id="${t.id}" title="关闭标签">×</b></span>`).join('')}<span class="pq-tab add" data-on="qryTabNew">＋ 新建</span></div>`;
  }
  ON.qryTabSel = d => { qTab().sql = ($('#pqEd') || {}).value || qTab().sql; Q.cur = +d.id; qLoadEd(); qDrawTabs(); qDrawBar(); qDrawMsg(); qDrawRes(); };
  ON.qryTabNew = () => { const ed = $('#pqEd'); if (ed) qTab().sql = ed.value; qNewTab(''); qLoadEd(); qDrawTabs(); qDrawMsg(); qDrawRes(); if (ed) ed.focus(); };
  ON.qryTabClose = d => {
    const id = +d.id; if (Q.running && Q.running.tab === id) { toast('该标签正在运行，请先取消'); return; }
    const i = Q.tabs.findIndex(t => t.id === id); if (i < 0) return;
    if (Q.tabs.length === 1) { Q.tabs[0].sql = ''; Q.tabs[0].msg = Q.tabs[0].res = Q.tabs[0].plan = null; }
    else { Q.tabs.splice(i, 1); if (Q.cur === id) Q.cur = Q.tabs[Math.max(0, i - 1)].id; }
    qLoadEd(); qDrawTabs(); qDrawMsg(); qDrawRes();
  };
  ON.qryEd = (d, el) => { qTab().sql = el.value; };

  /* ---------- 实例 / 库 ---------- */
  ON.qryInst = (d, el) => { qSwapSample(+el.value); Q.inst = +el.value; Q.db = ''; Q.open = {}; Q.treeKw = ''; rerender(); };
  ON.qryDb = (d, el) => { Q.db = el.value; Q.open = {}; qDrawTree(); qDrawBar(); qDrawLimits(); };
  ON.qryPageLimit = (d, el) => { Q.pageLimit = +el.value; };

  /* ---------- 库表树 ---------- */
  const hlText = (t, kw) => { if (!kw) return esc(t); const i = t.toLowerCase().indexOf(kw); return i < 0 ? esc(t) : esc(t.slice(0, i)) + '<mark class="hl">' + esc(t.slice(i, i + kw.length)) + '</mark>' + esc(t.slice(i + kw.length)); };
  function qDrawTree() {
    const el = $('#pqTree'); if (!el) return;
    if (isRedisInst(instOf(Q.inst))) return rkDraw();
    if (isExtInst(instOf(Q.inst))) return extDrawTree();
    const inst = instOf(Q.inst), d = dbsOf(Q.inst).find(x => x.db === Q.db);
    if (!d) { el.innerHTML = empty('该实例没有可用的库'); return; }
    const pol = qPolicy(inst, d.db), rv = isReadView(inst), kw = Q.treeKw.trim().toLowerCase();
    let list = d.tables.filter(t => !rv || /^v_/.test(t.n));
    if (kw) list = list.filter(t => t.n.toLowerCase().includes(kw) || String(t.c || '').toLowerCase().includes(kw) || t.cols.some(c => c.n.toLowerCase().includes(kw)));
    el.innerHTML = `<div class="tn" style="font-weight:600;cursor:default">⛁ ${esc(d.db)}<span class="n">${list.length} 个${rv ? '视图' : '对象'}</span></div>` + (list.map(t => {
      const colHit = kw && !t.n.toLowerCase().includes(kw) && t.cols.some(c => c.n.toLowerCase().includes(kw)), open = !!Q.open[t.n] || colHit;
      const lock = !tablePriv(pol, t.n);
      return `<div class="tn l1" title="${esc(t.c || '')}"><span data-on="qryTog" data-t="${esc(t.n)}" style="width:12px;color:var(--muted)">${open ? '▾' : '▸'}</span><span data-on="qryIns" data-t="${esc(t.n)}" style="flex:1;overflow:hidden;text-overflow:ellipsis">${t.view ? '◈' : '▦'} ${hlText(t.n, kw)}${lock ? ' 🔒' : ''}</span><span class="n">${t.view ? '视图' : fmtN(t.rows)}</span></div>`
        + (open ? t.cols.map(c => `<div class="tn l2" data-on="qryInsCol" data-c="${esc(c.n)}" title="${esc(c.cmt || '')}"><span>${c.key === 'PRI' ? '🔑 ' : ''}${hlText(c.n, kw)}</span><span class="n">${esc(c.t)}${c.mask ? ' · 🛡' : ''}</span></div>`).join('') : '');
    }).join('') || empty('没有匹配的表'));
  }
  ON.qryTreeKw = (d, el) => { Q.treeKw = el.value; if (!isRedisInst(instOf(Q.inst))) qDrawTree(); };
  ON.qryTog = d => { Q.open[d.t] = !Q.open[d.t]; qDrawTree(); };
  function qInsert(txt) {
    const ed = $('#pqEd'); if (!ed) return;
    const a = ed.selectionStart || 0, b = ed.selectionEnd || 0;
    ed.value = ed.value.slice(0, a) + txt + ed.value.slice(b); const p = a + txt.length;
    ed.focus(); ed.setSelectionRange(p, p); qTab().sql = ed.value;
  }
  ON.qryIns = d => {
    const inst = instOf(Q.inst), t = findTable(Q.inst, Q.db, d.t), ed = $('#pqEd'); if (!t || !ed) return;
    if (isExtInst(inst)) return extIns(inst, t, ed);
    if (!tablePriv(qPolicy(inst, Q.db), t.n)) {   // 点带锁的表：直接给出“去申请”
      const tab = qTab(); tab.msg = {err: err('no_privilege', `你没有 ${Q.db}.${t.n} 的查询权限。`, {db: Q.db, table: t.n, inst: inst.id, link: {href: `#qapply/new?inst=${inst.id}&db=${encodeURIComponent(Q.db)}`, text: '去申请查询权限'}}), notes: []};
      qDrawMsg(); return;
    }
    if (!ed.value.trim()) { ed.value = `SELECT ${t.cols.map(c => c.n).join(', ')}\nFROM ${t.n}\nLIMIT 100;`; qTab().sql = ed.value; ed.focus(); }
    else qInsert(t.n);
  };
  ON.qryInsCol = d => qInsert(d.c);

  /* ---------- 工具栏 / 限制提示 ---------- */
  function qDrawBar() {
    const el = $('#pqBar'); if (!el) return;
    const inst = instOf(Q.inst), rdx = isRedisInst(inst), ext = isExtInst(inst), mg = ext && EQ().family(inst) === 'mongo', run = Q.running, samples = qSamples(inst, Q.db);
    const groups = []; samples.forEach((s, i) => { let g = groups.find(x => x.g === s.g); if (!g) groups.push(g = {g: s.g, items: []}); g.items.push([i, s.label]); });
    el.innerHTML = `<div class="toolbar" style="margin:8px 0 0">
      ${run ? `<button class="btn primary" disabled>⏳ 运行中…</button><button class="btn danger" data-on="qryCancel">取消</button>`
            : `${B('query.run', '▶ 运行', {cls: 'primary', data: {mode: 'run'}})}${rdx ? '<button class="btn" disabled title="Redis 没有执行计划">执行计划</button>' : ext ? `<button class="btn" disabled title="${mg ? 'MongoDB 不支持 explain' : EQ().label(inst) + ' 没有执行计划页签，请直接运行 EXPLAIN'}">执行计划</button>` : B('query.run', '执行计划', {data: {mode: 'explain'}})}`}
      ${rdx ? '<button class="btn" disabled title="Redis 命令无需格式化">格式化</button>' : mg ? '<button class="btn" disabled title="MongoDB 语句不做格式化">格式化</button>' : '<button class="btn" data-on="qryFmt">格式化</button>'}
      <select data-onchange="qrySample"><option value="">${rdx ? '示例命令…' : mg ? '示例语句…' : '示例 SQL…'}</option>${groups.map(g => `<optgroup label="${esc(g.g)}">${g.items.map(([i, l]) => `<option value="${i}">${esc(l)}</option>`).join('')}</optgroup>`).join('')}</select>
      <span class="sp"></span>
      <label class="small muted">返回行数 <select data-onchange="qryPageLimit">${[100, 500, 1000, 5000].map(n => opt(n, n, Q.pageLimit)).join('')}</select></label>
    </div>`;
  }
  function qDrawLimits() {
    const el = $('#pqLimits'); if (!el) return;
    const inst = instOf(Q.inst), pol = qPolicy(inst, Q.db), d = dbsOf(Q.inst).find(x => x.db === Q.db) || {tables: []};
    if (isRedisInst(inst)) { el.innerHTML = qRedisChips(inst, pol); return; }
    if (isExtInst(inst)) { el.innerHTML = extChips(inst, pol); return; }
    const chips = [];
    if (!can('query.run')) chips.push(['bad', '🔒 你没有「执行查询」权限（query.run），运行按钮已置灰']);
    if (pol.mode === 'admin') chips.push(['ok', `管理员 · 不受库表授权限制 · 最多 ${nf(pol.limit)} 行`]);
    else if (pol.mode === 'group') chips.push(['ok', `资源组内实例 · 免库表授权 · 最多 ${nf(pol.limit)} 行`]);
    else if (pol.mode === 'priv') {
      const all = pol.privs.some(p => pTables(p) === '*'), names = [...new Set([].concat(...pol.privs.map(p => pTables(p) === '*' ? [] : pTables(p))))];
      const days = Math.round((Date.parse(pol.expire) - Date.parse(TODAY)) / 86400000);
      const lmin = Math.min(...pol.privs.map(p => +p.limit)); chips.push(['ok', `查询权限：${all ? Q.db + ' 全部表' : names.join('、')} · 最多 ${nf(lmin)}${lmin !== pol.limit ? '–' + nf(pol.limit) : ''} 行`]);
      chips.push([days <= 14 ? 'warn' : '', `权限到期 ${pol.expire}${days <= 14 ? `（${days} 天后）` : ''}`]);
      if (!all) chips.push(['warn', `其余表无权限 <a href="#qapply/new?inst=${inst.id}&db=${encodeURIComponent(Q.db)}">去申请</a>`]);
    } else chips.push(['bad', `你在 ${esc(Q.db)} 暂无查询权限${pol.expired ? `（权限已于 ${pol.expired} 过期）` : ''} <a href="#qapply/new?inst=${inst.id}&db=${encodeURIComponent(Q.db)}">去申请</a>`]);
    const mk = d.tables.reduce((s, t) => s + t.cols.filter(c => c.mask).length, 0);
    if (mk) chips.push(['ok', `数据脱敏：开启（本库 ${mk} 个敏感列）`]);
    if (QCFG.disableStar) chips.push(['warn', '禁止 SELECT *']);
    chips.push(['', `超时 ${QCFG.timeout}s`]);
    if (isReadView(inst)) chips.push(['warn', '只读视图实例：所有人只能查 v_ 开头的视图']);
    el.innerHTML = `<div class="pq-chips">${chips.map(([c, h]) => `<span class="pq-chip ${c}">${h}</span>`).join('')}</div>`;
  }

  /* ---------- 消息条 / 结果区 ---------- */
  const ERR_TITLE = {busy: '并发已满', bad_query: '语句被拦截', star_forbidden: 'SELECT * 被禁用', no_privilege: '没有查询权限', read_view_only: 'read_view 实例限制', timeout: '查询超时，已终止', engine_error: '引擎返回错误', canceled: '已取消'};
  function qDrawMsg() {
    const el = $('#pqMsg'); if (!el) return;
    const m = qTab().msg; if (!m) { el.innerHTML = ''; return; }
    let h = '';
    if (m.err) {
      const e = m.err;
      h += notice(`<b>${e.title || ERR_TITLE[e.code] || '失败'}</b> <span class="muted small mono">${e.code}</span><br>${esc(e.msg)}${e.link ? `<div style="margin-top:6px"><a class="btn sm ${e.code === 'no_privilege' ? 'primary' : ''}" href="${esc(e.link.href)}" style="text-decoration:none">${esc(e.link.text)} →</a></div>` : ''}`, e.code === 'canceled' ? 'warn' : 'bad', e.code === 'timeout' ? '⏱' : e.code === 'canceled' ? '⏹' : '⛔');
    }
    (m.notes || []).forEach(n => { h += notice(esc(n), 'warn', 'ⓘ'); });
    el.innerHTML = h;
  }
  const cellHtml = (v, col) => v == null ? '<span class="pq-null">NULL</span>' : col.mask ? `<span class="pq-mk" title="已脱敏">${esc(v)}</span>` : esc(v);
  function qDrawRes() {
    const el = $('#pqRes'); if (!el) return;
    const t = qTab();
    if (Q.running && Q.running.tab === t.id) { el.innerHTML = `<div class="empty"><div class="ic">⏳</div><div>正在执行… 超时 ${QCFG.timeout} 秒</div></div>`; return; }
    if (!t.res && !t.plan) { el.innerHTML = empty('运行查询后，结果显示在这里', '⌘'); return; }
    const tabsH = t.res && t.plan ? `<div class="pq-rtabs"><a class="${t.rtab === 'res' ? 'on' : ''}" data-on="qryRtab" data-v="res">结果</a><a class="${t.rtab === 'plan' ? 'on' : ''}" data-on="qryRtab" data-v="plan">执行计划</a></div>` : '';
    if ((t.rtab === 'plan' && t.plan) || (!t.res && t.plan)) {
      const p = t.plan, tc = {ALL: 'bad', index: 'warn', range: 'warn', ref: 'ok', eq_ref: 'ok', const: 'ok'};
      el.innerHTML = `${tabsH}<div class="small muted" style="margin-bottom:6px">执行计划 · ${p.rows.length} 行 · ${fmtMs(p.ms)}</div>
        <div class="tbl-wrap"><table><thead><tr>${p.cols.map(c => `<th>${c}</th>`).join('')}</tr></thead><tbody>${p.rows.map(r => `<tr>${r.map((v, i) => `<td class="mono small">${v == null ? '<span class="pq-null">NULL</span>' : i === 3 ? tag(v, tc[v] || '') : i === 7 ? nf(v) : esc(v)}</td>`).join('')}</tr>`).join('')}</tbody></table></div>
        ${p.tips.map(x => notice(x.text, x.lv === 'bad' ? 'bad' : x.lv === 'warn' ? 'warn' : '', x.lv === 'info' ? '💡' : '⚠')).join('')}`;
      return;
    }
    if (t.res.redis) { el.innerHTML = tabsH + RQ().resultHtml(t.res, Q.showAll ? t.res.rows.length : Math.min(200, t.res.rows.length)); return; }
    if (t.res.ext) { el.innerHTML = tabsH + EQ().resultHtml(t.res, Q.showAll ? t.res.rows.length : Math.min(200, t.res.rows.length)); return; }
    const r = t.res, inst = instOf(r.inst), max = Q.showAll ? r.rows.length : Math.min(200, r.rows.length);
    el.innerHTML = `${tabsH}<div class="row" style="margin-bottom:6px"><b>${nf(r.n)} 行</b><span class="muted">· 耗时 ${fmtMs(r.ms)}</span><span class="muted small">· ${esc(inst.name)} / ${esc(r.db)}</span>
        ${r.masked.length ? `<span class="pq-chip ok">🛡 已脱敏：${r.masked.map(esc).join('、')}</span>` : ''}<span class="sp"></span>
        <button class="btn sm" data-on="qryCopyRes">复制结果</button><a class="btn sm" href="#export/new" style="text-decoration:none">需要导出？</a></div>
      ${r.truncated ? notice(`结果已按 <b>${nf(r.eff)}</b> 行截断（限制来源：${esc(r.limitSrc)}），估计共 ${fmtN(r.est)} 行。<a href="#export/new">去提交数据导出工单</a>`, 'warn', '✂') : ''}
      <div class="pq-grid"><table><thead><tr><th class="rn">#</th>${r.cols.map(c => `<th>${esc(c.n)}${c.mask ? ' <span class="pq-mk" title="敏感列，按脱敏规则显示">🛡</span>' : ''}</th>`).join('')}</tr></thead>
        <tbody>${r.rows.slice(0, max).map((row, i) => `<tr><td class="rn">${i + 1}</td>${row.map((v, k) => `<td class="${r.cols[k].num || typeof v === 'number' ? 'n' : ''}">${cellHtml(v, r.cols[k])}</td>`).join('')}</tr>`).join('') || `<tr><td colspan="${r.cols.length + 1}">${empty('查询成功，没有符合条件的数据')}</td></tr>`}</tbody></table></div>
      ${r.rows.length > max ? `<div class="small muted" style="margin-top:6px">为保证页面流畅，原型只渲染前 ${max} 行（共 ${nf(r.rows.length)} 行）。<a data-on="qryShowAll">显示全部</a></div>` : ''}
      <details class="pq-d" style="margin-top:10px"><summary>实际执行的 SQL</summary><div><div class="pq-sqlv">${sqlHl(r.sql)}</div></div></details>`;
  }
  ON.qryRtab = d => { qTab().rtab = d.v; qDrawRes(); };
  ON.qryShowAll = () => { Q.showAll = true; qDrawRes(); };
  ON.qryCopyRes = () => { const r = qTab().res; if (!r) return; copyText([r.cols.map(c => c.n).join('\t')].concat(r.rows.map(x => x.map(v => v == null ? 'NULL' : v).join('\t'))).join('\n')); };

  /* ---------- 运行 / 取消 / 格式化 / 示例 ---------- */
  ACTIONS['query.run'] = d => qRun(d.mode === 'explain');
  function qFail(tab, text, inst, e, ms) {
    const st = e.code === 'engine_error' ? '失败' : e.code === 'timeout' ? '超时' : e.code === 'canceled' ? '已取消' : '被拦截';
    tab.msg = {err: e, notes: []}; if (Q.curPlan) tab.plan = null; else tab.res = null;
    qLog(inst, text, st, ms || 0, 0, e.msg);
    audit('query.run', `${inst.name}/${dbTxt(inst.id, Q.db)} 查询未执行（${ERR_TITLE[e.code] || e.code}）：${clip(oneLine(text), 70)}`, '失败', inst.name + ' / ' + dbTxt(inst.id, Q.db));
  }
  function qRun(explain) {
    if (Q.running) return;
    const inst = instOf(Q.inst), ed = $('#pqEd'), tab = qTab(); if (!ed) return;
    tab.sql = ed.value;
    if (isRedisInst(inst)) { qRunRedis(inst, ed, tab); return; }
    if (isExtInst(inst)) { qRunExt(inst, ed, tab); return; }
    let text = ed.value, sel = false;
    if (ed.selectionStart !== ed.selectionEnd) { text = ed.value.slice(ed.selectionStart, ed.selectionEnd); sel = true; }
    Q.showAll = false; Q.curPlan = !!explain;
    const chk = qCheck(text, inst, Q.db, Q.pageLimit, explain);
    if (chk.err) { qFail(tab, text, inst, chk.err); qDrawMsg(); qDrawRes(); qDrawTabs(); return; }
    const an = chk.an;
    let res = an.plan ? qExplain(an) : an.kind === 'show' ? qShow(an, inst, Q.db) : qExecute(an);
    const delay = res.err ? (res.err.code === 'timeout' ? 1600 : 160) : Math.min(900, 180 + (res.ms || 20) * 2);
    Q.running = {tab: tab.id, timer: setTimeout(() => {
      Q.running = null;
      if (res.err) qFail(tab, text, inst, res.err, res.err.ms);
      else {
        tab.msg = {notes: (sel ? ['已执行选中的语句。'] : []).concat(an.notes)};
        if (an.plan) { tab.plan = res; tab.rtab = 'plan'; }
        else { tab.res = Object.assign({}, res, {sql: an.finalSql, inst: inst.id, db: Q.db, limitSrc: an.limitSrc, eff: an.eff}); tab.rtab = 'res'; }
        qLog(inst, text, '成功', res.ms, an.plan ? res.rows.length : res.n);
        audit('query.run', `${inst.name}/${dbTxt(inst.id, Q.db)} 执行${an.plan ? '执行计划' : '查询'}（${an.plan ? res.rows.length : res.n} 行，${res.ms}ms）：${clip(oneLine(an.sql), 70)}`, '成功', inst.name + ' / ' + dbTxt(inst.id, Q.db));
      }
      qDrawTabs(); qDrawBar(); qDrawMsg(); qDrawRes();
    }, delay)};
    qDrawTabs(); qDrawBar(); qDrawRes();
  }
  ON.qryCancel = () => {
    const run = Q.running; if (!run) return;
    clearTimeout(run.timer); Q.running = null;
    const tab = Q.tabs.find(t => t.id === run.tab), inst = instOf(Q.inst);
    if (tab) { tab.msg = {err: err('canceled', '已取消：已在另一条连接上发送 KILL QUERY，服务端无残留。'), notes: []}; }
    qLog(inst, (tab || {sql: ''}).sql, '已取消', 0, 0, '用户取消');
    audit('query.run', `${inst.name}/${dbTxt(inst.id, Q.db)} 取消正在运行的查询`, '成功', inst.name + ' / ' + dbTxt(inst.id, Q.db));
    qDrawTabs(); qDrawBar(); qDrawMsg(); qDrawRes();
  };
  ON.qryFmt = () => { const ed = $('#pqEd'); if (!ed || !ed.value.trim()) return toast('编辑器是空的'); if (isExtInst(instOf(Q.inst)) && EQ().family(instOf(Q.inst)) === 'mongo') return toast('MongoDB 语句不做 SQL 格式化'); ed.value = qFormat(ed.value); qTab().sql = ed.value; toast('已格式化'); };
  ON.qrySample = (d, el) => {
    const s = qSamples(instOf(Q.inst), Q.db)[+el.value]; el.value = ''; if (!s) return;
    const ed = $('#pqEd'); if (ed) qTab().sql = ed.value;
    if (qTab().sql.trim()) qNewTab(s.sql); else qTab().sql = s.sql;
    qLoadEd(); qDrawTabs(); qDrawMsg(); qDrawRes();
  };

  /* ---------- 历史（右侧抽屉） ---------- */
  const hlTerms = (text, terms) => {
    if (!terms.length) return esc(text);
    const low = text.toLowerCase(), rs = [];
    terms.forEach(t => { const lt = t.toLowerCase(); let i = 0; while ((i = low.indexOf(lt, i)) >= 0) { rs.push([i, i + lt.length]); i += lt.length; } });
    rs.sort((a, b) => a[0] - b[0]);
    let out = '', last = 0;
    rs.forEach(([s, e]) => { if (s < last) { if (e > last) { out += '<mark class="hl">' + esc(text.slice(last, e)) + '</mark>'; last = e; } return; } out += esc(text.slice(last, s)) + '<mark class="hl">' + esc(text.slice(s, e)) + '</mark>'; last = e; });
    return out + esc(text.slice(last));
  };
  function qHistFilter() {
    const terms = QH.kw.split(/\s+/).filter(Boolean), tl = terms.map(t => t.toLowerCase());
    const list = QHIST.filter(h => {
      if (!ME.super && h.user !== ME.login) return false;
      if (ME.super && QH.user && h.user !== QH.user) return false;
      if (QH.inst && h.inst != QH.inst) return false;
      const day = h.at.slice(0, 10);
      if (QH.from && day < QH.from) return false;
      if (QH.to && day > QH.to) return false;
      const hay = (h.sql + ' ' + instOf(h.inst).name + ' ' + h.db + ' ' + (ME.super ? h.user : '')).toLowerCase();
      return tl.every(t => hay.includes(t));          // 多个词需全部包含；% _ 按字面（没有通配符语义）
    });
    return {list, terms};
  }
  function qDrawHist() {
    const el = $('#qhList'); if (!el) return;
    const {list, terms} = qHistFilter();
    $('#qhCount').textContent = `共 ${list.length} 条${list.length > 60 ? '，仅显示最近 60 条' : ''}`;
    el.innerHTML = list.slice(0, 60).map(h => `<div class="pq-hrow"><div class="m"><b style="color:var(--text)">${esc(h.at)}</b>${ME.super ? tag(h.user, 'brand') : ''}<span>${esc(instOf(h.inst).name)} / ${esc(dbTxt(h.inst, h.db))}</span><span>${h.st === '成功' ? fmtMs(h.ms) : '—'}</span><span>${h.st === '成功' ? nf(h.rows) + ' 行' : ''}</span>${tag(h.st, ST_CLS[h.st])}</div>
      <pre>${hlTerms(h.sql, terms)}</pre>${h.msg ? `<div class="small muted">${esc(clip(h.msg, 120))}</div>` : ''}
      <div class="ops"><button class="btn sm" data-on="qryHistOpen" data-id="${h.id}">放到新标签</button><button class="btn sm" data-copy="${esc(h.sql)}">复制</button></div></div>`).join('') || empty('没有符合条件的历史记录', '🕘');
  }
  ON.qryHist = () => {
    const users = [...new Set(QHIST.map(h => h.user))];
    openDrawer('查询历史', `<div class="row" style="margin-bottom:10px">
        <input type="search" id="qhKw" placeholder="搜索语句" data-oninput="qryHistQ" value="${esc(QH.kw)}" style="flex:1;min-width:220px">
        <select data-onchange="qryHistInst"><option value="">全部实例</option>${INSTANCES.filter(i => QHIST.some(h => h.inst === i.id && (ME.super || h.user === ME.login))).map(i => opt(i.id, i.name, QH.inst)).join('')}</select>
        ${ME.super ? `<select data-onchange="qryHistUser"><option value="">全部用户</option>${users.map(u => opt(u, u, QH.user)).join('')}</select>` : ''}
      </div><div class="row" style="margin-bottom:10px"><span class="small muted">日期</span><input type="date" data-onchange="qryHistFrom" value="${esc(QH.from)}"><span class="small muted">至</span><input type="date" data-onchange="qryHistTo" value="${esc(QH.to)}">
        <button class="btn sm" data-on="qryHistReset">清除过滤</button></div>
      <div class="small muted" id="qhCount" style="margin-bottom:8px"></div><div id="qhList"></div>`, true);
    qDrawHist();
  };
  ON.qryHistQ = (d, el) => { QH.kw = el.value; qDrawHist(); };
  ON.qryHistInst = (d, el) => { QH.inst = el.value; qDrawHist(); };
  ON.qryHistUser = (d, el) => { QH.user = el.value; qDrawHist(); };
  ON.qryHistFrom = (d, el) => { QH.from = el.value; qDrawHist(); };
  ON.qryHistTo = (d, el) => { QH.to = el.value; qDrawHist(); };
  ON.qryHistReset = () => { Object.assign(QH, {kw: '', inst: '', from: '', to: '', user: ''}); ON.qryHist(); };
  ON.qryHistOpen = d => {
    const h = QHIST.find(x => x.id == d.id); if (!h) return;
    const ed = $('#pqEd'); if (ed) qTab().sql = ed.value;
    const vis = visibleInstances('all');
    if (vis.some(i => i.id === h.inst) && qDbs(h.inst).some(x => x.db === h.db)) { Q.inst = h.inst; Q.db = h.db; } else toast('该记录的实例 / 库你当前不可见，已放入当前实例');
    qNewTab(h.sql); closeDrawer(); rerender(); toast('已放到新标签');
  };

  /* ============================================================================
     三、查慢日志（dbt_slow_logs 聚合：实例 × 库 × SQL 指纹 × 天）
     ========================================================================== */
  const SLG_FPS = [
    {id: 'o1', schema: 'order', db: 'order_db', sql: 'select id, order_no, amount, status from orders where user_id = ? and date(created_at) = ? order by created_at desc limit ?', gens: ['uid', 'date', 'n'], cnt: 420, avg: 3.4, scan: 1.8e6, sent: 20, freq: .97, users: ['app_order@10.20.8.14', 'app_order@10.20.8.15', 'app_order@10.20.8.21']},
    {id: 'o2', schema: 'order', db: 'order_db', sql: 'select count(*) from orders where status = ? and created_at >= ?', gens: ['st', 'ts'], cnt: 90, avg: 8.6, scan: 9.2e6, sent: 1, freq: .9, users: ['report@10.20.2.3']},
    {id: 'o3', schema: 'order', db: 'order_db', sql: 'select o.order_no, i.sku_name, i.qty from orders o join order_items i on i.order_id = o.id where o.created_at between ? and ?', gens: ['ts', 'ts'], cnt: 35, avg: 14.2, scan: 2.6e7, sent: 48000, freq: .75, users: ['report@10.20.2.3', 'bi_etl@10.20.4.31']},
    {id: 'o4', schema: 'order', db: 'order_db', sql: 'update orders set status = ?, updated_at = now() where order_no = ?', gens: ['st', 'no'], cnt: 160, avg: 1.6, scan: 1, sent: 0, lock: 1.2, freq: .85, users: ['app_order@10.20.8.14', 'app_order@10.20.8.15']},
    {id: 'o5', schema: 'order', db: 'order_db', sql: 'select id, action, operator, created_at from order_log where order_id = ? order by id desc', gens: ['uid'], cnt: 260, avg: 2.1, scan: 3.5e5, sent: 12, freq: .95, users: ['app_order@10.20.8.14', 'app_order@10.20.8.21']},
    {id: 'o6', schema: 'order', db: 'order_db', sql: 'select sku_id, sum(qty) as qty from order_items where sku_id in (?) group by sku_id', gens: ['uid'], cnt: 55, avg: 5.2, scan: 4.8e6, sent: 300, freq: .8, users: ['bi_etl@10.20.4.31']},
    {id: 'a1', schema: 'order', db: 'order_archive', sql: 'select order_no, amount from orders_2025 where created_at >= ? and created_at < ? order by amount desc limit ?', gens: ['ts', 'ts', 'n'], cnt: 12, avg: 21.5, scan: 4.1e7, sent: 100, freq: .6, users: ['report@10.20.2.3']},
    {id: 'p1', schema: 'pay', db: 'pay_db', sql: 'select channel, count(*), sum(amount) from payments where paid_at >= ? and paid_at < ? group by channel', gens: ['ts', 'ts'], cnt: 40, avg: 11.8, scan: 6.8e6, sent: 3, freq: .9, users: ['report@10.20.2.7']},
    {id: 'p2', schema: 'pay', db: 'pay_db', sql: 'select p.pay_no, r.refund_no, r.amount from refunds r join payments p on p.pay_no = r.pay_no where r.created_at >= ? order by r.id desc', gens: ['ts'], cnt: 75, avg: 4.3, scan: 1.1e6, sent: 500, freq: .9, users: ['app_pay@10.20.7.12', 'report@10.20.2.7']},
    {id: 'p3', schema: 'pay', db: 'pay_db', sql: 'update payments set status = ? where pay_no = ? and status = ?', gens: ['st', 'no', 'st'], cnt: 120, avg: 1.4, scan: 1, sent: 0, lock: .9, freq: .85, users: ['app_pay@10.20.7.12', 'app_pay@10.20.7.13']},
    {id: 'p4', schema: 'pay', db: 'pay_db', sql: 'select card_no, holder from bank_cards where user_id = ? and card_no like ?', gens: ['uid', 'no'], cnt: 30, avg: 3.1, scan: 4e6, sent: 2, freq: .7, users: ['app_pay@10.20.7.13']},
    {id: 'u1', schema: 'user', db: 'user_db', sql: 'select id, username, phone from users where phone like ?', gens: ['no'], cnt: 300, avg: 6.7, scan: 1.82e7, sent: 1, freq: .98, users: ['app_user@10.20.6.8', 'app_user@10.20.6.9']},
    {id: 'u2', schema: 'user', db: 'user_db', sql: 'select count(*) from login_log where user_id = ? and created_at > ?', gens: ['uid', 'ts'], cnt: 520, avg: 1.9, scan: 8.6e5, sent: 1, freq: .98, users: ['app_user@10.20.6.8', 'app_user@10.20.6.9', 'risk@10.20.6.31']},
    {id: 'u3', schema: 'user', db: 'user_db', sql: 'select user_id, ip from login_log where ip = ? order by created_at desc limit ?', gens: ['no', 'n'], cnt: 85, avg: 12.4, scan: 3.9e8, sent: 50, freq: .85, users: ['risk@10.20.6.31']},
    {id: 'u4', schema: 'user', db: 'user_db', sql: 'select u.id, p.real_name from users u join user_profile p on p.user_id = u.id where u.level >= ? limit ?', gens: ['st', 'n'], cnt: 40, avg: 3.7, scan: 5.2e6, sent: 100, freq: .7, users: ['bi_etl@10.20.4.31']},
    {id: 'u5', schema: 'user', db: 'user_db', sql: 'update users set level = ? where id = ?', gens: ['st', 'uid'], cnt: 30, avg: 1.2, scan: 1, sent: 0, lock: .8, freq: .6, users: ['app_user@10.20.6.8']},
  ];
  const SLG_INST_F = {1: 1, 2: .45, 3: 1, 4: 1.3, 7: .08};
  const slgInsts = () => visibleInstances().filter(i => i.slowlog && SLG_INST_F[i.id]);
  const fpOf = id => SLG_FPS.find(f => f.id === id);
  const addDays = (ymd, n) => dayOffset(n, ymd);
  const dayDiff = (a, b) => Math.round((Date.parse(b) - Date.parse(a)) / 86400000);

  function slgCell(inst, fp, day) {
    const r = rng(hashOf(inst.id + fp.id + day));
    if (r() > fp.freq) return null;
    const spike = fp.id === 'o1' && inst.id === 1 && (day === addDays(TODAY, -2) || day === addDays(TODAY, -1)) ? 4.2 : fp.id === 'u3' && inst.id === 4 && day === addDays(TODAY, -4) ? 3 : 1;
    const wk = new Date(day + 'T00:00:00').getDay(), wf = wk === 0 || wk === 6 ? (/^(o2|o3|p1|a1|u4)$/.test(fp.id) ? .3 : .75) : 1;
    const cnt = Math.max(1, Math.round(fp.cnt * SLG_INST_F[inst.id] * (0.55 + r() * 0.9) * spike * wf));
    const avg = fp.avg * (0.85 + r() * 0.35) * (spike > 1 ? 1.3 : 1);
    return {key: `${inst.id}|${fp.id}|${day}`, inst: inst.id, fp, db: fp.db, day, cnt, avg, total: cnt * avg, max: avg * (2.2 + r() * 5), min: avg * (0.3 + r() * 0.3),
      scanAvg: Math.round(fp.scan * (0.7 + r() * 0.6)), scanMax: Math.round(fp.scan * (1.8 + r() * 2.5)), sentAvg: Math.round(fp.sent * (0.7 + r() * 0.6)), sentMax: Math.round(fp.sent * (1.6 + r() * 2)), user: fp.users[Math.floor(r() * fp.users.length)]};
  }
  const SL = {inst: '', db: '', kw: '', from: addDays(TODAY, -30), to: TODAY, sort: 'total', dir: -1, page: 1, open: {}};
  const SL_SIZE = 20;
  function slgRows() {
    if (!SL.from || !SL.to) return {err: '请选择起止日期。'};
    if (SL.from > SL.to) return {err: '开始日期不能晚于结束日期。'};
    const days = dayDiff(SL.from, SL.to) + 1;
    if (days > 93) return {err: `日期范围最长 93 天（当前选择了 ${days} 天），请缩小范围后再查。`};
    const insts = slgInsts().filter(i => !SL.inst || i.id == SL.inst), kws = SL.kw.trim().toLowerCase().split(/\s+/).filter(Boolean), rows = [];
    for (const inst of insts) for (const fp of SLG_FPS) {
      if (fp.schema !== inst.schema || !dbsOf(inst.id).some(d => d.db === fp.db)) continue;
      if (SL.db && fp.db !== SL.db) continue;
      if (kws.length && !kws.every(k => (fp.sql + ' ' + fp.db + ' ' + fp.users.join(' ')).toLowerCase().includes(k))) continue;
      for (let i = 0; i < days; i++) { const c = slgCell(inst, fp, addDays(SL.from, i)); if (c) rows.push(c); }
    }
    const k = {total: 'total', cnt: 'cnt', avg: 'avg', max: 'max', scan: 'scanAvg', day: 'day'}[SL.sort] || 'total';
    rows.sort((a, b) => (a[k] > b[k] ? 1 : a[k] < b[k] ? -1 : 0) * SL.dir || b.total - a.total);
    return {rows, days};
  }
  const sFmt = v => v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2);
  const sortTh = (label, k, right) => `<th class="nowrap" style="${right ? 'text-align:right;' : ''}cursor:pointer" data-on="slgSort" data-k="${k}">${label}${SL.sort === k ? (SL.dir < 0 ? ' ▼' : ' ▲') : ''}</th>`;

  pages.slowlogs = {
    render() {
      const insts = slgInsts(), dbs = [...new Set(SLG_FPS.filter(f => insts.some(i => i.schema === f.schema && (!SL.inst || i.id == SL.inst))).map(f => f.db))];
      if (SL.inst && !insts.some(i => i.id == SL.inst)) SL.inst = '';
      if (!insts.length) return `<div class="card">${empty('你所在资源组的实例都没有开启慢日志采集。', '◷')}</div>`;
      return `<div class="card"><div class="toolbar" style="margin:0">
        <select data-onchange="slgInst"><option value="">全部实例</option>${insts.map(i => opt(i.id, `${i.name}（${i.type}${i.role === 'slave' ? ' · 从' : ''}）`, SL.inst)).join('')}</select>
        <select data-onchange="slgDb"><option value="">全部库</option>${dbs.map(d => opt(d, d, SL.db)).join('')}</select>
        <input type="date" data-onchange="slgFrom" value="${SL.from}" aria-label="开始日期"><span class="muted">至</span><input type="date" data-onchange="slgTo" value="${SL.to}" aria-label="结束日期">
        <span class="subtabs">${[[7, '近 7 天'], [31, '近 31 天'], [93, '近 93 天']].map(([n, t]) => `<a data-on="slgRange" data-n="${n}" class="${dayDiff(SL.from, SL.to) + 1 === n && SL.to === TODAY ? 'on' : ''}">${t}</a>`).join('')}</span>
        <input type="search" id="slgKw" placeholder="搜索 SQL / 库 / 用户" data-oninput="slgKw" value="${esc(SL.kw)}" style="min-width:260px;flex:1">
        <button class="btn primary" data-on="slgGo">搜索</button></div></div>
      <div id="slgBody" style="margin-top:14px"></div>`;
    },
    init() { slgDraw(); }
  };
  function slgDraw() {
    const el = $('#slgBody'); if (!el) return;
    const res = slgRows();
    if (res.err) { el.innerHTML = notice(esc(res.err), 'bad', '⚠') + `<div class="card">${empty('请调整日期范围后再搜索', '◷')}</div>`; return; }
    const rows = res.rows, tot = rows.reduce((a, r) => a + r.cnt, 0), tsec = rows.reduce((a, r) => a + r.total, 0), fps = new Set(rows.map(r => r.fp.id + r.inst)).size;
    const worst = rows.reduce((m, r) => r.max > m.max ? r : m, {max: 0});
    const byDay = {}; rows.forEach(r => { const d = byDay[r.day] || (byDay[r.day] = {c: 0, t: 0}); d.c += r.cnt; d.t += r.total; });
    const days = Array.from({length: res.days}, (_, i) => addDays(SL.from, i)), cnts = days.map(d => (byDay[d] || {c: 0}).c), secs = days.map(d => Math.round((byDay[d] || {t: 0}).t));
    const xl = [0, 1, 2, 3, 4].map(i => days[Math.round((days.length - 1) * i / 4)].slice(5));
    const pages_ = Math.max(1, Math.ceil(rows.length / SL_SIZE)); if (SL.page > pages_) SL.page = pages_;
    const view = rows.slice((SL.page - 1) * SL_SIZE, SL.page * SL_SIZE);
    el.innerHTML = `<div class="pq-kpis">
        <div class="card kpi"><div class="l">慢 SQL 总次数</div><div class="v">${nf(tot)}</div></div>
        <div class="card kpi"><div class="l">慢 SQL 指纹数</div><div class="v">${fps}</div></div>
        <div class="card kpi"><div class="l">累计耗时</div><div class="v">${fmtMs(tsec * 1000)}</div></div>
        <div class="card kpi"><div class="l">最慢单条</div><div class="v">${worst.max ? worst.max.toFixed(1) + ' s' : '—'}</div><div class="d muted small">${worst.max ? esc(worst.day) + ' · ' + esc(clip(worst.fp.sql, 28)) : ''}</div></div></div>
      <div class="grid g2" style="margin-bottom:14px">
        <div class="card"><h2>每日慢 SQL 次数 <span class="sub">${days.length} 天</span></h2>${lineChart([{name: '次数', color: 'var(--c1)', data: cnts}], {xl, h: 150})}</div>
        <div class="card"><h2>每日累计耗时 <span class="sub">秒</span></h2>${lineChart([{name: '累计耗时(s)', color: 'var(--c3)', data: secs}], {xl, h: 150})}</div></div>
      <div class="card"><h2>聚合明细 <span class="sub">共 ${nf(rows.length)} 行</span></h2>
      ${rows.length ? `<div class="tbl-wrap"><table><thead><tr><th>实例</th><th>库</th><th>SQL 指纹</th>${sortTh('日期', 'day')}${sortTh('次数', 'cnt', 1)}${sortTh('总耗时(s)', 'total', 1)}${sortTh('平均(s)', 'avg', 1)}${sortTh('最大(s)', 'max', 1)}${sortTh('扫描行 均/最大', 'scan', 1)}<th style="text-align:right">返回行 均/最大</th><th>操作</th></tr></thead><tbody>
        ${view.map(r => `<tr class="click" data-on="slgRow" data-k="${r.key}"><td class="nowrap small">${esc(instOf(r.inst).name)}</td><td class="mono small">${esc(r.db)}</td><td class="mono small" style="max-width:340px" title="${esc(r.fp.sql)}"><div class="ellipsis" style="max-width:340px">${esc(r.fp.sql)}</div></td><td class="nowrap">${r.day}</td>
          <td style="text-align:right">${nf(r.cnt)}</td><td style="text-align:right"><b>${sFmt(r.total)}</b></td><td style="text-align:right">${sFmt(r.avg)}</td><td style="text-align:right;${r.max >= 30 ? 'color:var(--bad);font-weight:600' : ''}">${sFmt(r.max)}</td>
          <td style="text-align:right" class="small">${fmtN(r.scanAvg)} / ${fmtN(r.scanMax)}</td><td style="text-align:right" class="small">${fmtN(r.sentAvg)} / ${fmtN(r.sentMax)}</td><td><button class="btn sm" data-on="slgDetail" data-k="${r.key}">详细</button></td></tr>
          ${SL.open[r.key] ? `<tr class="pq-exp"><td colspan="11"><div class="small muted" style="margin-bottom:4px">代表 SQL · 用户@主机 ${esc(r.user)} · 指纹 ${hashOf(r.fp.id + r.fp.sql).toString(16)}</div>${code(slgSample(r.fp, rng(hashOf(r.key))), 'sql')}</td></tr>` : ''}`).join('')}</tbody></table></div>
        <div class="pager"><span>第 ${SL.page} / ${pages_} 页，每页 ${SL_SIZE} 条</span>${Array.from({length: pages_}, (_, i) => i + 1).filter(p => p === 1 || p === pages_ || Math.abs(p - SL.page) <= 2).map(p => `<a class="${p === SL.page ? 'on' : ''}" data-on="slgPage" data-p="${p}">${p}</a>`).join('')}</div>`
        : empty('该时间范围内没有慢日志', '◷')}</div>
      <details class="pq-d" style="margin-top:14px"><summary>采集器状态 <span class="muted small" style="font-weight:400">· 已启用 · 每 10 秒一轮</span></summary><div>${tbl(['实例', '状态', '最近成功', '本次写入', '累计写入', '耗时'], slgInsts().map(i => { const st = i.status === 'warn'; return [esc(i.name), st ? tag('退避重试', 'warn') : tag('正常', 'ok'), st ? '2026-10-07 10:36:10' : '2026-10-07 10:40:' + pad2(20 + i.id * 3), st ? 0 : 6 + i.id * 7, nf(1200000 + i.id * 173000), st ? '—' : (180 + i.id * 40) + ' ms']; }))}</div></details>`;
  }
  /* 把指纹里的 ? 换成像样的字面量 */
  function slgSample(fp, r) {
    let k = 0;
    const g = {uid: () => rint(r, 1000000, 9800000), date: () => `'${addDays(TODAY, -rint(r, 0, 6))}'`, n: () => pk(r, [10, 20, 50, 100, 200]), st: () => rint(r, 0, 4), ts: () => `'${addDays(TODAY, -rint(r, 0, 8))} ${pad2(rint(r, 0, 23))}:00:00'`, no: () => /like/.test(fp.sql) ? `'%${rint(r, 1000, 9999)}'` : `'O${addDays(TODAY, -rint(r, 0, 5)).replace(/-/g, '')}${rint(r, 10000000, 99999999)}'`};
    return fp.sql.replace(/\?/g, () => (g[fp.gens[k++] || 'n'] || g.n)());
  }
  ON.slgInst = (d, el) => { SL.inst = el.value; SL.db = ''; SL.page = 1; rerender(); };
  ON.slgDb = (d, el) => { SL.db = el.value; SL.page = 1; slgDraw(); };
  ON.slgFrom = (d, el) => { SL.from = el.value; SL.page = 1; slgDraw(); };
  ON.slgTo = (d, el) => { SL.to = el.value; SL.page = 1; slgDraw(); };
  ON.slgRange = d => { SL.to = TODAY; SL.from = addDays(TODAY, 1 - +d.n); SL.page = 1; rerender(); };
  ON.slgKw = (d, el) => { SL.kw = el.value; SL.page = 1; slgDraw(); };
  ON.slgGo = () => { SL.page = 1; slgDraw(); toast('已按条件重新查询'); };
  ON.slgSort = d => { if (SL.sort === d.k) SL.dir = -SL.dir; else { SL.sort = d.k; SL.dir = d.k === 'day' ? 1 : -1; } SL.page = 1; slgDraw(); };
  ON.slgPage = d => { SL.page = +d.p; slgDraw(); };
  ON.slgRow = d => { SL.open[d.k] = !SL.open[d.k]; slgDraw(); };
  ON.slgDetail = d => {
    const [iid, fid, day] = d.k.split('|'), inst = instOf(iid), fp = fpOf(fid), c = slgCell(inst, fp, day); if (!c) return;
    const n = Math.min(c.cnt, 100), r = rng(hashOf('d' + d.k)), lines = [];
    let sec = 23 * 3600 + 59 * 60 - rint(r, 0, 1800);
    for (let i = 0; i < n; i++) {
      sec -= rint(r, 20, Math.max(40, Math.floor(80000 / c.cnt)));
      const q = Math.max(1.02, c.avg * (0.5 + r() * 1.6)), lk = fp.lock ? fp.lock * (0.4 + r() * 1.4) : r() < .08 ? r() * 0.3 : 0;
      lines.push({t: `${pad2(Math.max(0, Math.floor(sec / 3600)))}:${pad2(Math.max(0, Math.floor(sec % 3600 / 60)))}:${pad2(Math.max(0, sec % 60))}`, user: pk(r, fp.users), q, lk, sent: Math.round(fp.sent * (0.5 + r())), scan: Math.round(fp.scan * (0.5 + r() * 1.2)), sql: slgSample(fp, r)});
    }
    openDrawer(`慢 SQL 明细 · ${day}`, `${kvList([['实例', esc(inst.name)], ['库', `<code>${esc(fp.db)}</code>`], ['日期', day], ['当天次数', `${nf(c.cnt)} 次`], ['平均 / 最大', `${sFmt(c.avg)} s / ${sFmt(c.max)} s`], ['扫描 / 返回行（均）', `${fmtN(c.scanAvg)} / ${fmtN(c.sentAvg)}`]])}
      <h3 style="font-size:14px;margin:14px 0 4px">SQL 指纹</h3>${code(fp.sql, 'sql')}
      ${canView('sqladvisor') ? `<div class="small"><a href="#sqladvisor">去优化工具 →</a></div>` : ''}
      <h3 style="font-size:14px;margin:14px 0 6px">最近 ${n} 条</h3>
      <div class="tbl-wrap" style="max-height:52vh"><table><thead><tr><th>执行时间</th><th>用户@主机</th><th>库名</th><th style="text-align:right">查询(s)</th><th style="text-align:right">锁定(s)</th><th style="text-align:right">返回行</th><th style="text-align:right">扫描行</th><th>SQL</th></tr></thead><tbody>
      ${lines.map(l => `<tr><td class="nowrap mono small">${l.t}</td><td class="mono small nowrap">${esc(l.user)}</td><td class="mono small">${esc(fp.db)}</td><td style="text-align:right">${l.q.toFixed(2)}</td><td style="text-align:right">${l.lk.toFixed(2)}</td><td style="text-align:right">${nf(l.sent)}</td><td style="text-align:right">${nf(l.scan)}</td><td class="mono small" style="min-width:260px;word-break:break-all">${esc(l.sql)}</td></tr>`).join('')}</tbody></table></div>`, true);
  };

  /* ============================================================================
     四、数据字典
     ========================================================================== */
  const DIC_KINDS = [['table', '表'], ['view', '视图'], ['trigger', '触发器'], ['proc', '存储过程'], ['func', '函数'], ['event', '定时任务']];
  const D = {inst: 0, db: '', kind: 'table', kw: '', exp: null};
  const DEFS = {
    sp_close_expired_orders: "BEGIN\n  DECLARE v_cnt INT DEFAULT 0;\n  UPDATE orders SET status = 4, updated_at = NOW()\n   WHERE status = 0 AND created_at < NOW() - INTERVAL 30 MINUTE;\n  SET v_cnt = ROW_COUNT();\n  INSERT INTO order_log(order_id, action, operator, created_at)\n  SELECT id, 'timeout_cancel', 'job:order-timeout', NOW() FROM orders\n   WHERE status = 4 AND updated_at >= NOW() - INTERVAL 1 MINUTE;\nEND",
    fn_order_fee: "BEGIN\n  RETURN ROUND(p_amount * 0.006 + 0.5, 2);\nEND",
    sp_daily_reconcile: "BEGIN\n  INSERT INTO reconcile_result(pay_date, channel, pay_cnt, pay_amount)\n  SELECT DATE(paid_at), channel, COUNT(*), SUM(amount)\n    FROM payments WHERE paid_at >= CURDATE() - INTERVAL 1 DAY AND paid_at < CURDATE() AND status = 1\n   GROUP BY DATE(paid_at), channel;\nEND",
    usp_load_fact_sales: "AS\nBEGIN\n  SET NOCOUNT ON;\n  INSERT INTO dbo.fact_sales(sale_id, date_key, customer_key, amount)\n  SELECT sale_id, date_key, customer_key, amount FROM stg.sales_delta;\n  TRUNCATE TABLE stg.sales_delta;\nEND",
    trg_orders_au: "BEGIN\n  IF NEW.status <> OLD.status THEN\n    INSERT INTO order_log(order_id, action, operator, created_at)\n    VALUES (NEW.id, CONCAT('status:', OLD.status, '->', NEW.status), 'trigger', NOW());\n  END IF;\nEND",
    ev_archive_order_log: "DO\nBEGIN\n  DELETE FROM order_log WHERE created_at < NOW() - INTERVAL 365 DAY LIMIT 50000;\nEND",
    ev_reconcile: "DO CALL sp_daily_reconcile()",
  };
  const dicItems = (instId, db) => {
    const d = dbsOf(instId).find(x => x.db === db) || {tables: [], procs: [], events: [], triggers: []}, rv = isReadView(instOf(instId));
    return {
      table: rv ? [] : d.tables.filter(t => !t.view).map(t => ({n: t.n, c: t.c, t})),
      view: d.tables.filter(t => t.view && (!rv || /^v_/.test(t.n))).map(t => ({n: t.n, c: t.c, t})),
      trigger: rv ? [] : (d.triggers || []).map(x => ({n: x.n, c: `${x.ev} · ${x.tbl}`, x})),
      proc: rv ? [] : (d.procs || []).filter(p => p.t === 'PROCEDURE').map(p => ({n: p.n, c: '存储过程', x: p})),
      func: rv ? [] : (d.procs || []).filter(p => p.t === 'FUNCTION').map(p => ({n: p.n, c: '函数', x: p})),
      event: rv ? [] : (d.events || []).map(e => ({n: e.n, c: `${e.sched} · ${e.st === 'ENABLED' ? '已启用' : '已停用'}`, x: e})),
    };
  };
  const dicEnsure = insts => {
    if (!insts.some(i => i.id == D.inst)) { D.inst = insts[0].id; D.db = ''; }
    const dbs = dbsOf(D.inst); if (!dbs.some(d => d.db === D.db)) D.db = (dbs[0] || {}).db || '';
    if (isExtInst(instOf(D.inst))) { if (D.kind !== 'table' && D.kind !== 'view') D.kind = 'table'; }
    else if (isReadView(instOf(D.inst))) D.kind = 'view';
  };

  pages.dict = {
    render() {
      const insts = visibleInstances('ext');
      if (!insts.length) return `<div class="card">${empty('你所在的资源组没有关联任何实例。', '☷')}</div>`;
      dicEnsure(insts);
      const inst = instOf(D.inst), dbs = dbsOf(D.inst), rv = isReadView(inst) && !isExtInst(inst);
      return `<div class="pq-top">
        <b>实例</b><select data-onchange="dicInst" style="min-width:270px">${instOpts(insts, D.inst)}</select>
        <b>库</b><select data-onchange="dicDb">${dbs.map(d => opt(d.db, d.db, D.db)).join('')}</select>
        <span>${instBadges(inst)}</span><span class="sp"></span>
        <select id="dicScope"><option value="db">导出范围：当前库</option><option value="inst" ${ME.super ? '' : 'disabled'}>导出范围：整个实例${ME.super ? '' : '（仅超级用户）'}</option></select>
        ${B('dict.export', '导出 HTML 字典')}</div>
      ${inst.status === 'bad' ? notice(`实例 ${esc(inst.name)} 当前不可达（${inst.host}:${inst.port}），无法读取元数据；下面显示的是上次缓存的结构。`, 'warn', '⚠') : ''}
      ${rv ? notice('read_view 实例：仅开放 <code>v_</code> 开头的视图，只展示列信息', 'warn', '🔒') : ''}
      <div class="card"><div class="toolbar" id="dicKinds"></div><div id="dicList"></div></div>`;
    },
    init() { dicDraw(); }
  };
  function dicDraw() {
    const kinds = $('#dicKinds'), list = $('#dicList'); if (!kinds || !list) return;
    if (isExtInst(instOf(D.inst))) return dicDrawExt(kinds, list);
    const inst = instOf(D.inst), items = dicItems(D.inst, D.db), rv = isReadView(inst), mssql = inst.type === 'MsSQL';
    kinds.innerHTML = `<span class="subtabs">${DIC_KINDS.map(([k, t]) => { const dis = rv && k !== 'view'; return `<a data-on="${dis ? 'noop' : 'dicKind'}" data-k="${k}" class="${D.kind === k ? 'on' : ''}" style="${dis ? 'opacity:.4;cursor:not-allowed' : ''}">${t} ${items[k].length}</a>`; }).join('')}</span>
      <input type="search" id="dicKw" placeholder="过滤名称 / 注释" data-oninput="dicKw" value="${esc(D.kw)}" style="min-width:220px">`;
    const kw = D.kw.trim().toLowerCase(), all = items[D.kind];
    if (D.kind === 'event' && mssql) { list.innerHTML = empty('SQL Server Agent 作业暂不支持（需读取 msdb）', '☷'); return; }
    const rows = all.filter(i => !kw || i.n.toLowerCase().includes(kw) || String(i.c || '').toLowerCase().includes(kw)).sort((a, b) => a.n.localeCompare(b.n));
    if (!rows.length) { list.innerHTML = empty(all.length ? '没有匹配的对象' : `该库没有${DIC_KINDS.find(k => k[0] === D.kind)[1]}`, '☷'); return; }
    const letters = [...new Set(rows.map(r => /^[a-z]/i.test(r.n) ? r.n[0].toUpperCase() : '#'))], isT = D.kind === 'table' || D.kind === 'view';
    const heads = D.kind === 'table' ? ['名称', '注释', '估算行数', '大小', '标记'] : D.kind === 'view' ? ['名称', '注释'] : ['名称', '说明'];
    let h = '', last = '';
    rows.forEach(r => {
      const L = /^[a-z]/i.test(r.n) ? r.n[0].toUpperCase() : '#';
      if (L !== last) { last = L; h += `<tr class="pq-letter" id="dl_${L === '#' ? 'x' : L}"><td colspan="${heads.length}">${L}</td></tr>`; }
      const t = r.t, mk = t && !t.view ? t.cols.filter(c => c.mask).length : 0;
      h += `<tr class="click" data-on="dicOpen" data-n="${esc(r.n)}"><td class="mono"><a>${esc(r.n)}</a></td><td>${esc(r.c || '')}</td>${D.kind === 'table' ? `<td>≈ ${fmtN(t.rows)}</td><td>${fmtMB(t.size)}</td><td>${t.part ? tag('分区表', 'info') + ' ' : ''}${mk ? tag(`敏感列 ${mk}`, 'warn') : ''}</td>` : ''}</tr>`;
    });
    list.innerHTML = `<div class="pq-az">${'ABCDEFGHIJKLMNOPQRSTUVWXYZ'.split('').map(c => `<a class="${letters.includes(c) ? '' : 'dis'}" data-on="dicJump" data-l="${c}">${c}</a>`).join('')}</div>${tbl(heads, []).replace(/<tbody>[\s\S]*<\/tbody>/, () => `<tbody>${h}</tbody>`)}`;
  }
  /* PostgreSQL / MongoDB / ClickHouse：表 / 视图（Mongo 为集合 / 视图）两个页签；行数、大小为估算值或元数据计数，空值显示“-” */
  function dicDrawExt(kinds, list) {
    const inst = instOf(D.inst), E = EQ();
    if (isReadView(inst)) { kinds.innerHTML = ''; list.innerHTML = notice(esc(E.READ_VIEW_MSG(inst)), 'warn', '🔒'); return; }
    const items = E.dictItems(inst, D.db), KD = E.dictKinds(inst);
    kinds.innerHTML = `<span class="subtabs">${KD.map(([k, t]) => `<a data-on="dicKind" data-k="${k}" class="${D.kind === k ? 'on' : ''}">${t} ${items[k].length}</a>`).join('')}</span>
      <input type="search" id="dicKw" placeholder="过滤名称 / 注释" data-oninput="dicKw" value="${esc(D.kw)}" style="min-width:220px">`;
    const kw = D.kw.trim().toLowerCase(), all = items[D.kind] || [];
    const rows = all.filter(i => !kw || i.n.toLowerCase().includes(kw) || String(i.c || '').toLowerCase().includes(kw)).sort((a, b) => a.n.localeCompare(b.n));
    if (!rows.length) { list.innerHTML = empty(all.length ? '没有匹配的对象' : `该库没有${(KD.find(k => k[0] === D.kind) || [0, '对象'])[1]}`, '☷'); return; }
    list.innerHTML = tbl(E.dictHeads(inst, D.kind), rows, {attrs: r => ` class="click" data-on="dicOpen" data-n="${esc(r.n)}"`, cells: r => E.dictRow(inst, r)});
  }
  ON.dicInst = (d, el) => { D.inst = +el.value; D.db = ''; D.kw = ''; rerender(); };
  ON.dicDb = (d, el) => { D.db = el.value; D.kw = ''; rerender(); };
  ON.dicKind = d => { D.kind = d.k; D.kw = ''; rerender(); };
  ON.dicKw = (d, el) => { D.kw = el.value; dicDraw(); };
  ON.dicJump = d => { const e = document.getElementById('dl_' + d.l); if (e) e.scrollIntoView({behavior: 'smooth', block: 'start'}); };

  /* 详情 */
  const yn = v => v ? '是' : '否';
  function dicTableDetail(inst, db, t) {
    const rv = isReadView(inst), mssql = inst.type === 'MsSQL', mb = t.size || 0;
    const info = t.view ? [['名称', `<code>${esc(t.n)}</code>`], ['类型', '视图'], ['注释', esc(t.c)]] : [['表名', `<code>${esc(t.n)}</code>`], ['引擎', esc(t.eng || 'InnoDB')], ['行格式', mssql ? '—' : 'Dynamic'], ['估算行数', `≈ ${nf(t.rows)}`],
      ['平均行长度', `${Math.round(mb * 1048576 / Math.max(1, t.rows))} B`], ['数据大小', fmtMB(Math.round(mb * 0.72))], ['索引大小', fmtMB(Math.round(mb * 0.28))], ['碎片大小', fmtMB(Math.round(mb * 0.04))],
      ['当前自增值', t.cols[0] && t.cols[0].def === 'auto_increment' ? nf(t.rows + 3118 + (hashOf(t.n) % 900)) : '—'], ['字符集 / 排序', mssql ? 'Chinese_PRC_CI_AS' : 'utf8mb4 / utf8mb4_0900_ai_ci'], ['创建时间', '2024-03-12 09:30:11'], ['表注释', esc(t.c)], ['分区', t.part ? '按时间 RANGE 分区（见「分区维护」）' : '否']];
    const mk = t.cols.filter(c => c.mask);
    return `${kvList(info, 'c2')}
      ${mk.length ? notice(`含 <b>${mk.length}</b> 个敏感列：${mk.map(c => `<code>${esc(c.n)}</code>`).join('、')}`, 'warn', '🛡') : ''}
      <details class="pq-d" open style="margin-top:10px"><summary>列信息（${t.cols.length}）</summary><div>${tbl(['列名', '类型', '可空', '默认值', '键', '额外', '注释', '敏感'], t.cols.map(c => [`<code>${esc(c.n)}</code>`, `<span class="mono small">${esc(c.t)}</span>`, c.nul ? 'YES' : 'NO', c.def && c.def !== 'auto_increment' ? esc(c.def) : '<span class="muted">—</span>', c.key ? tag(c.key, c.key === 'PRI' ? 'brand' : '') : '', c.def === 'auto_increment' ? 'auto_increment' : '', esc(c.cmt || ''), c.mask ? tag('敏感 · 脱敏', 'warn') : '']))}</div></details>
      ${t.view || !(t.idx || []).length ? '' : `<details class="pq-d" open><summary>索引信息（${t.idx.length}）</summary><div>${tbl(['索引名', '列', '唯一', '类型', '基数（估算）'], t.idx.map(ix => [`<code>${esc(ix[0])}</code>`, ix[1].map(c => `<code>${esc(c)}</code>`).join(' , '), yn(ix[2]), mssql ? 'CLUSTERED' : 'BTREE', fmtN(Math.round(t.rows / (ix[2] ? 1 : 12)))]))}</div></details>`}
      ${rv && t.view ? `<div class="small muted">read_view 实例：不展示视图定义。</div>` : mssql && !t.view ? `<details class="pq-d"><summary>建表语句</summary><div>${notice('SQL Server 暂不生成建表语句', '', 'ℹ')}</div></details>` : `<details class="pq-d" ${t.view ? 'open' : ''}><summary>${t.view ? '视图定义' : '建表语句'}</summary><div>${code(ddlOf(inst.id, db, t), 'sql')}</div></details>`}`;
  }
  ON.dicOpen = d => {
    if (isExtInst(instOf(D.inst))) { const t = findTable(D.inst, D.db, d.n); if (t) openDrawer(`${D.db}.${t.n}`, EQ().dictDetail(instOf(D.inst), D.db, t), true); return; }
    const inst = instOf(D.inst), it = dicItems(D.inst, D.db)[D.kind].find(x => x.n === d.n); if (!it) return;
    let body;
    if (it.t) body = dicTableDetail(inst, D.db, it.t);
    else if (D.kind === 'trigger') body = kvList([['触发器', `<code>${esc(it.n)}</code>`], ['触发时机', esc(it.x.ev)], ['所属表', `<code>${esc(it.x.tbl)}</code>`], ['定义者', 'dbadmin@%']]) + `<h3 style="font-size:14px;margin:14px 0 4px">语句</h3>` + code(`CREATE TRIGGER \`${it.n}\` ${it.x.ev} ON \`${it.x.tbl}\` FOR EACH ROW\n${DEFS[it.n] || 'BEGIN\n  -- …\nEND'}`, 'sql');
    else if (D.kind === 'event') body = kvList([['事件', `<code>${esc(it.n)}</code>`], ['调度', esc(it.x.sched)], ['状态', it.x.st === 'ENABLED' ? tag('已启用', 'ok') : tag('已停用')], ['下次执行', '2026-10-08 00:00:00'], ['上次执行', '2026-10-07 00:00:01']]) + `<h3 style="font-size:14px;margin:14px 0 4px">定义</h3>` + code(`CREATE EVENT \`${it.n}\`\nON SCHEDULE ${it.x.sched}\n${DEFS[it.n] || 'DO BEGIN END'}`, 'sql');
    else body = kvList([[D.kind === 'proc' ? '存储过程' : '函数', `<code>${esc(it.n)}</code>`], ['类型', it.x.t], ['定义者', 'dbadmin@%'], ['安全类型', 'DEFINER'], ['创建时间', '2025-11-20 16:08:42']]) + `<h3 style="font-size:14px;margin:14px 0 4px">定义</h3>` + code(`CREATE ${it.x.t} \`${it.n}\`(${it.x.t === 'FUNCTION' ? 'p_amount DECIMAL(12,2)) RETURNS DECIMAL(12,2) DETERMINISTIC' : ')'}\n${DEFS[it.n] || 'BEGIN\n  -- …\nEND'}`, 'sql');
    openDrawer(`${D.db}.${it.n}`, body, true);
  };

  /* 导出 HTML 字典：高危确认 → 生成 → 提供下载 */
  function dicHtml(inst, dbs) {
    if (isExtInst(inst)) {
      const sty0 = 'body{font:14px/1.6 -apple-system,"Segoe UI","Microsoft YaHei",sans-serif;margin:24px;color:#222}h1{font-size:22px}h2{margin-top:32px;border-bottom:2px solid #4b5bdc;padding-bottom:4px}h3{margin:20px 0 6px}table{border-collapse:collapse;width:100%;margin:6px 0 12px;font-size:13px}th,td{border:1px solid #ccd;padding:4px 8px;text-align:left}th{background:#eef0fb}.s{color:#667}';
      return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>${esc(inst.name)} 数据字典</title><style>${sty0}</style></head><body><h1>${esc(inst.name)} 数据字典</h1><p class="s">生成时间 ${TODAY} ${now().slice(11)} · 导出人 ${esc(ME.login)} · ${esc(EQ().notes[EQ().family(inst)].join(' '))}</p>${dbs.map(db => EQ().dictHtmlExport(inst, db)).join('')}</body></html>`;
    }
    const rv = isReadView(inst), sty = 'body{font:14px/1.6 -apple-system,"Segoe UI","Microsoft YaHei",sans-serif;margin:24px;color:#222}h1{font-size:22px}h2{margin-top:32px;border-bottom:2px solid #4b5bdc;padding-bottom:4px}h3{margin:20px 0 6px}table{border-collapse:collapse;width:100%;margin:6px 0 12px;font-size:13px}th,td{border:1px solid #ccd;padding:4px 8px;text-align:left}th{background:#eef0fb}.m{color:#c27c00;font-weight:600}.s{color:#667}';
    let h = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>${esc(inst.name)} 数据字典</title><style>${sty}</style></head><body><h1>${esc(inst.name)} 数据字典</h1><p class="s">生成时间 ${TODAY} ${now().slice(11)} · 导出人 ${esc(ME.login)} · 行数 / 大小为估算值${rv ? ' · 只读视图实例：仅含 v_ 视图的列信息' : ''}</p>`;
    dbs.forEach(db => {
      const d = dbsOf(inst.id).find(x => x.db === db); if (!d) return;
      const list = d.tables.filter(t => !rv || /^v_/.test(t.n));
      h += `<h2>${esc(db)}</h2><ul>${list.map((t, i) => `<li><a href="#${esc(db)}_${i}">${esc(t.n)}</a> ${esc(t.c || '')}</li>`).join('')}</ul>`;
      list.forEach((t, i) => {
        h += `<h3 id="${esc(db)}_${i}">${esc(t.n)} <span class="s">${esc(t.c || '')}${t.view ? '（视图）' : ` · ≈${nf(t.rows)} 行`}</span></h3><table><tr><th>列名</th><th>类型</th><th>可空</th><th>默认值</th><th>键</th><th>注释</th></tr>${t.cols.map(c => `<tr><td>${esc(c.n)}</td><td>${esc(c.t)}</td><td>${c.nul ? 'YES' : 'NO'}</td><td>${esc(c.def && c.def !== 'auto_increment' ? c.def : '')}</td><td>${esc(c.key)}</td><td>${esc(c.cmt || '')}${c.mask ? ' <span class="m">[敏感]</span>' : ''}</td></tr>`).join('')}</table>`;
        if (!rv && !t.view && (t.idx || []).length) h += `<table><tr><th>索引</th><th>列</th><th>唯一</th></tr>${t.idx.map(x => `<tr><td>${esc(x[0])}</td><td>${esc(x[1].join(', '))}</td><td>${x[2] ? '是' : '否'}</td></tr>`).join('')}</table>`;
      });
    });
    return h + '</body></html>';
  }
  ACTIONS['dict.export'] = () => {
    const inst = instOf(D.inst), scope = ($('#dicScope') || {}).value || 'db';
    if (scope === 'inst' && !ME.super) { toast('整个实例导出仅超级用户'); return; }
    const dbs = scope === 'inst' ? dbsOf(D.inst).map(d => d.db) : [D.db], target = scope === 'inst' ? inst.name : D.db;
    risky('dict.export', '导出 HTML 数据字典',
      `将导出 <b>${esc(inst.name)}</b> 的 <b>${scope === 'inst' ? '整个实例（' + dbs.length + ' 个库）' : '库 ' + esc(D.db)}</b> 的数据字典（表结构、索引、注释，不含数据）。<br>导出会先写入系统审计。`,
      null, () => setTimeout(() => {
        const html = dicHtml(inst, dbs), file = `${inst.name}_${scope === 'inst' ? 'all' : D.db}_dict_${TODAY.replace(/-/g, '')}.${scope === 'inst' ? 'zip' : 'html'}`;
        D.exp = {html, file};
        audit('dict.export', `导出数据字典：${inst.name} / ${scope === 'inst' ? '整个实例（' + dbs.length + ' 个库）' : D.db}`, '成功', inst.name);
        openModal({title: '数据字典已生成', ok: '', cancel: '关闭', body: `<div class="notice ok"><span>✓</span><div>已生成 <b>${esc(file)}</b>（约 ${Math.max(1, Math.round(html.length / 1024))} KB，${dbs.length} 个库，${dbs.reduce((a, db) => a + ((dbsOf(D.inst).find(x => x.db === db) || {tables: []}).tables.length), 0)} 个对象）。</div></div>
          <button class="btn primary" data-on="dicDownload">下载文件</button> <span class="small muted">原型：整实例导出合并为单个 HTML（代替 ZIP）</span>`});
      }, 0), target);
  };
  ON.dicDownload = () => {
    if (!D.exp) return;
    try { const a = document.createElement('a'); a.href = URL.createObjectURL(new Blob([D.exp.html], {type: 'text/html'})); a.download = D.exp.file.replace(/\.zip$/, '.html'); document.body.appendChild(a); a.click(); a.remove(); toast('已开始下载'); } catch (e) { toast('当前环境不支持下载'); }
  };

  /* ============================================================================
     五、分区维护（MySQL / PolarDB / SelectDB / Doris 的 RANGE 分区续建与清理；未分区的 MySQL 表可显式“初始化为分区表”；webhook 推送；运行输出）
     结构化配置，不接受手写 DDL。规则 / 字段 / 文案以真实实现（internal/partman）为准。
     ========================================================================== */
  const PM_PERIODS = [['minute', '分钟'], ['hour', '小时'], ['day', '天'], ['week', '周'], ['month', '月'], ['year', '年']];
  /* 时区：库列 timezone 是 varchar(20)；空 = 跟随目标库会话时区；Overview.timezones 是常用项（不含空值） */
  const PM_TZ_LEN = 20;
  const PM_TZS = ['Asia/Shanghai', 'UTC', 'Asia/Tokyo', 'Asia/Singapore', 'America/New_York', 'America/Los_Angeles', 'Europe/London', 'Europe/Berlin', '+08:00'];
  const PM_SESS_OFF = {20: 8, 21: 0};          // 目标库会话时区的固定偏移（小时）：原型里 doris-dw-prod 是 +08:00，selectdb-log-prod 是 UTC，其余实例按 +8
  const PM_AUTO = {run: false, drop: false, interval: 180, notifyOn: 'always'};      // dbt_sql_config：partman_auto_run / partman_auto_drop / partman_auto_interval / partman_notify_on（always | on_failure | never）
  const periodName = p => (PM_PERIODS.find(x => x[0] === p) || [0, p])[1];
  const pmOffName = off => { const s = off < 0 ? '-' : '+', a = Math.abs(Math.round(off * 60)); return `UTC${s}${pad2(Math.floor(a / 60))}:${pad2(a % 60)}`; };
  const pmSessOff = inst => (inst in PM_SESS_OFF) ? PM_SESS_OFF[inst] : 8;
  const pmDtf = {};
  /* IANA 名字在某个 UTC 时刻的偏移（小时）；Intl 不认识该名字时抛错 */
  function ianaOffAt(tz, utcMs) {
    const f = pmDtf[tz] || (pmDtf[tz] = new Intl.DateTimeFormat('en-US', {timeZone: tz, hourCycle: 'h23', year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric'}));
    const o = {}; f.formatToParts(new Date(utcMs)).forEach(p => { o[p.type] = +p.value; });
    return (Date.UTC(o.year, o.month - 1, o.day, o.hour, o.minute, o.second) - Math.floor(utcMs / 1000) * 1000) / 3600000;
  }
  /* 解析时区写法 → {err} | {follow, off, iana, name, label, warns}（契约 §2）：
     空 = 跟随目标库会话时区；固定偏移 ±HH:MM / ±HHMM / ±H / UTC+8 / GMT+08:00 / UTC / GMT（|偏移| ≤ 14:00，分钟 < 60）；
     IANA 名字区分大小写、不接受 Local；超过 20 个字符不接受；IANA 有夏令时给 warning */
  function pmTzParse(raw, inst) {
    const s = String(raw == null ? '' : raw).trim();
    if (s === '') { const off = pmSessOff(inst); return {follow: true, off, iana: '', name: pmOffName(off), label: `跟随目标库会话时区（当前 ${pmOffName(off)}）`, warns: []}; }
    if (s.length > PM_TZ_LEN) return {err: `时区 "${s}" 超过 20 个字符，库里的 timezone 列放不下，请改用固定偏移（如 -03:00）`};
    const unknown = `无法识别的时区 "${s}"（可填 IANA 名字如 Asia/Shanghai、固定偏移如 +08:00 / UTC+8，留空跟随目标库）`;
    const m = /^(?:(UTC|GMT)\s*)?(?:([+-])\s*(\d{1,2})(?::?(\d{2}))?)?$/i.exec(s);
    if (m) {
      let off = 0;
      if (m[2]) {
        const h = +m[3], mi = m[4] ? +m[4] : 0;
        if (mi >= 60 || h * 60 + mi > 14 * 60) return {err: `时区 "${s}" 的偏移超出范围（绝对值需 ≤ 14:00，分钟 < 60）`};
        off = (m[2] === '-' ? -1 : 1) * (h + mi / 60);
      }
      return {follow: false, off, iana: '', name: off === 0 ? 'UTC' : pmOffName(off), label: off === 0 ? 'UTC（UTC+00:00）' : `固定偏移 ${pmOffName(off)}`, warns: []};
    }
    if (/^local$/i.test(s) || !/^[A-Za-z][A-Za-z0-9_+\-]*(\/[A-Za-z0-9_+\-]+)*$/.test(s)) return {err: unknown};
    let off0, off1, off7;
    try {
      const canon = new Intl.DateTimeFormat('en-US', {timeZone: s}).resolvedOptions().timeZone;
      if (canon.toLowerCase() === s.toLowerCase() && canon !== s) return {err: `时区 "${s}" 区分大小写，请写成 ${canon}`};
      off0 = ianaOffAt(s, T0); off1 = ianaOffAt(s, Date.UTC(2026, 0, 1)); off7 = ianaOffAt(s, Date.UTC(2026, 6, 1));
    } catch (e) { return {err: unknown}; }
    const warns = [];
    if (off1 !== off7) {
      const std = Math.min(off1, off7), a = Math.abs(Math.round(std * 60));
      warns.push(`时区 ${s} 有夏令时：换季日前后按日的分区边界在 UTC 上会漂移 1 小时，按小时 / 分钟的分区还可能在换季时重复或缺失一个窗口。建议改用固定偏移（如 ${std < 0 ? '-' : '+'}${pad2(Math.floor(a / 60))}:${pad2(a % 60)}）`);
    }
    return {follow: false, off: off0, iana: s, name: s, label: `${s}（当前 ${pmOffName(off0)}）`, warns};
  }
  /* 配置在“墙上时间 wall”这一刻的偏移（小时）与对应的 UTC 毫秒；时区非法时按 +8 兜底（保存时已拦截） */
  function cfgOff(c, wall) {
    const r = pmTzParse(c.tz, c.inst); if (r.err) return 8; if (!r.iana) return r.off;
    const o1 = ianaOffAt(r.iana, wall - r.off * 3600000); return ianaOffAt(r.iana, wall - o1 * 3600000);
  }
  const epochOf = (c, wall) => wall - cfgOff(c, wall) * 3600000;
  function startOfPeriod(t, p) {
    const d = new Date(t);
    if (p === 'minute') d.setUTCSeconds(0, 0);
    else if (p === 'hour') d.setUTCMinutes(0, 0, 0);
    else { d.setUTCHours(0, 0, 0, 0); if (p === 'week') d.setUTCDate(d.getUTCDate() - ((d.getUTCDay() + 6) % 7)); else if (p === 'month') d.setUTCDate(1); else if (p === 'year') d.setUTCMonth(0, 1); }
    return d.getTime();
  }
  function addP(t, p, k) {
    const d = new Date(t);
    if (p === 'minute') d.setUTCMinutes(d.getUTCMinutes() + k); else if (p === 'hour') d.setUTCHours(d.getUTCHours() + k); else if (p === 'day') d.setUTCDate(d.getUTCDate() + k);
    else if (p === 'week') d.setUTCDate(d.getUTCDate() + 7 * k); else if (p === 'month') d.setUTCMonth(d.getUTCMonth() + k); else d.setUTCFullYear(d.getUTCFullYear() + k);
    return d.getTime();
  }
  /* 分区名：minute pYYYYMMDDHHmm / hour pYYYYMMDDHH / day pYYYYMMDD / month pYYYYMM / year pYYYY 取窗口起始时刻；
     week 取窗口最后一天 pYYYYMMDD（与原程序一致；旧版取周一，已改）。span = 周期跨度（week 需要它算窗口最后一天） */
  function pName(start, p, span) {
    const s = fmtDT(p === 'week' ? addP(start, 'week', span || 1) - 86400000 : start).replace(/[- :]/g, '');
    return 'p' + (p === 'minute' ? s.slice(0, 12) : p === 'hour' ? s.slice(0, 10) : p === 'month' ? s.slice(0, 6) : p === 'year' ? s.slice(0, 4) : s.slice(0, 8));
  }
  /* MySQL 边界字面量：datetime 墙上时间 / int 秒 / bigint 毫秒（按配置时区换算） */
  function boundLit(c, end) {
    if (c.ctype === 'datetime') return /00:00:00$/.test(fmtDT(end)) && ['day', 'week', 'month', 'year'].includes(c.period) ? `'${fmtD(end)}'` : `'${fmtDT(end)}'`;
    const epoch = epochOf(c, end);
    return String(c.ctype === 'int' ? Math.round(epoch / 1000) : epoch);
  }
  /* SelectDB / Doris 边界字面量：都是双引号字符串（datetime 完整时间、bigint 13 位毫秒、int 10 位秒） */
  const dorisLit = (c, t) => c.ctype === 'datetime' ? `"${fmtDT(t)}"` : `"${c.ctype === 'int' ? Math.round(epochOf(c, t) / 1000) : epochOf(c, t)}"`;
  const dorisRange = (c, p) => { const ty = c.ctype === 'datetime' ? 'DATETIME' : c.ctype === 'int' ? 'INT' : 'BIGINT', v = t => c.ctype === 'datetime' ? fmtDT(t) : String(c.ctype === 'int' ? Math.round(epochOf(c, t) / 1000) : epochOf(c, t)); return p.rng || `[types: [${ty}]; keys: [${v(addP(p.end, c.period, -c.span))}]; ..types: [${ty}]; keys: [${v(p.end)}]; )`; };
  /* 生成“现有分区”：hist 个历史 + 当前 + fut 个未来；withMax 时末尾带 pmax */
  function seedParts(period, span, hist, fut, withMax) {
    const cur = startOfPeriod(T0, period), parts = [], mk = end => ({name: pName(addP(end, period, -span), period, span), end});
    for (let k = hist; k >= 1; k--) parts.push(mk(addP(cur, period, -span * (k - 1))));
    let e = addP(cur, period, span); parts.push(mk(e));
    for (let j = 0; j < fut; j++) { e = addP(e, period, span); parts.push(mk(e)); }
    if (withMax) parts.push({name: 'pmax', end: null});
    return parts;
  }
  const ctypeOf = t => /^datetime/i.test(t) ? 'datetime' : /^bigint/i.test(t) ? 'bigint' : /^int/i.test(t) ? 'int' : '';

  /* ---------- 方言 / 计划 ---------- */
  const pmDialect = i => i && (i.type === 'Doris' || i.type === 'SelectDB') ? 'doris' : i && i.type === 'MySQL' ? 'mysql' : '';
  const PM_MAX_INIT = 5000, PM_WARN_INIT = 200;
  const pmTb = c => `\`${c.db}\`.\`${c.tbl}\``;
  /* 原程序网格对齐（alignDown，初始化用）：minute / hour 按日内取整；day 按 1970-01-01 起算的跨度倍数；week 按 1970-01-05（周一）；month 按 1970-01；year 按公元年的跨度倍数 */
  function alignGrid(t, p, span) {
    const D = 86400000, fd = (a, b) => Math.floor(a / b), d = new Date(t), day0 = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
    if (p === 'minute') return day0 + fd(d.getUTCHours() * 60 + d.getUTCMinutes(), span) * span * 60000;
    if (p === 'hour') return day0 + fd(d.getUTCHours(), span) * span * 3600000;
    if (p === 'day') return fd(fd(t, D), span) * span * D;
    if (p === 'week') { const idx = fd(t, D), mon = idx - ((idx + 3) % 7); return (4 + fd(fd(mon - 4, 7), span) * span * 7) * D; }
    if (p === 'month') { const mi = (d.getUTCFullYear() - 1970) * 12 + d.getUTCMonth(), f = fd(mi, span) * span; return Date.UTC(1970 + fd(f, 12), ((f % 12) + 12) % 12, 1); }
    return Date.UTC(fd(d.getUTCFullYear(), span) * span, 0, 1);
  }
  const pmInitCheckSort = {error: 0, warn: 1, ok: 2};

  /* 初始化计划（服务端 BuildPlan 对未分区 MySQL 表的 kind=init 计划）。st = {mode: current|earliest|date, date} */
  function ptmInitPlan(c, st, tz) {
    st = st || {mode: 'current', date: ''};
    const t = findTable(c.inst, c.db, c.tbl), tbn = `${c.db}.${c.tbl}`, checks = [], notes = [], warns = tz.warns.slice(), mode0 = st.mode || 'current';
    const chk = (code, level, message) => checks.push({code, level, message});
    const R = (error, code, extra) => Object.assign({kind: 'init', dia: 'mysql', tz, tbn, error, code, adds: [], drops: [], reorg: false, stmts: [], warns,
      init: {mode: mode0, date: st.date || '', start: '', count: 0, first: '', last: '', lastBound: '', earliest: '', canExec: false, checks, notes, confirm: null}}, extra || {});
    if (!t) return R(`表 ${tbn} 不存在`, 'not_found');
    chk('table_kind', t.view ? 'error' : 'ok', t.view ? `${tbn} 是视图，不能分区` : '是普通表（不是视图 / 临时表）');
    const eng = String(t.eng || '').toUpperCase();
    chk('engine', /^(INNODB|NDB|NDBCLUSTER)$/.test(eng) ? 'ok' : 'error', /^(INNODB|NDB|NDBCLUSTER)$/.test(eng) ? `存储引擎 ${t.eng} 支持分区` : `存储引擎 ${t.eng} 不支持分区（MySQL 8.0 只有 InnoDB / NDB 支持分区）`);
    chk('foreign_key', t.fk ? 'error' : 'ok', t.fk ? '表上有外键约束，MySQL 分区表不支持外键，请先删除外键' : '没有外键约束');
    const col = (t.cols || []).find(x => x.n === c.col), ct = col ? ctypeOf(col.t) : '';
    if (!col) chk('column', 'error', `分区字段 ${c.col} 在表里不存在`);
    else if (!ct || ct !== c.ctype) chk('column_type', 'error', `分区字段 ${c.col} 的类型是 ${col.t}，与配置的 ${c.ctype} 不一致（仅支持 bigint / int / datetime / date）`);
    else chk('column_type', 'ok', `分区字段 ${c.col} 存在，类型 ${col.t}，与配置的 ${c.ctype} 一致`);
    const uks = (t.idx || []).filter(ix => ix[2]), badUk = uks.filter(ix => !ix[1].includes(c.col));
    if (!uks.length) chk('unique_keys', 'ok', '表上没有唯一键');
    else if (badUk.length) chk('unique_keys', 'error', `以下唯一键不包含分区字段 ${c.col}：${badUk.map(ix => `${ix[0] === 'PRIMARY' ? '主键 ' : ''}${ix[0]}(${ix[1].join(', ')})`).join('、')}（MySQL 要求每个唯一键含主键都包含分区字段）`);
    else chk('unique_keys', 'ok', `唯一键均包含分区字段 ${c.col}（${uks.map(ix => (ix[0] === 'PRIMARY' ? '主键 ' : '') + ix[0]).join('、')}）`);

    const cur = alignGrid(T0, c.period, c.span), parseDT = s => Date.parse(s.replace(' ', 'T') + 'Z');
    let startT = cur, mode = mode0, earliest = '';
    if (!['current', 'earliest', 'date'].includes(mode)) return R(`start_mode "${mode}" 非法（可选 current / earliest / date）`, 'bad_init_option');
    if (mode === 'earliest') {
      if (t.minTimeout) return R(`读取最早数据超时：SELECT /*+ MAX_EXECUTION_TIME(20000) */ MIN(\`${c.col}\`) 超过 20 秒没有返回，请改用“指定起始日期”手工填写起点`, 'earliest_unavailable');
      if (!t.minAt) { notes.push('表为空（MIN 为 NULL），按“从当前窗口开始”处理'); mode = 'current'; }
      else { earliest = t.minAt; startT = Math.min(cur, alignGrid(parseDT(t.minAt), c.period, c.span)); }
    } else if (mode === 'date') {
      const ds = String(st.date || '').trim();
      if (!/^\d{4}-\d{2}-\d{2}( \d{2}:\d{2}:\d{2})?$/.test(ds) || isNaN(parseDT(ds.length === 10 ? ds + ' 00:00:00' : ds))) return R('起始日期格式不对，应为 YYYY-MM-DD（可带 HH:MM:SS）', 'bad_init_option');
      startT = alignGrid(parseDT(ds.length === 10 ? ds + ' 00:00:00' : ds), c.period, c.span);
      if (startT > cur) return R(`起始日期不能晚于当前窗口（当前窗口起点 ${fmtDT(cur)}）`, 'bad_init_option');
      notes.push(`起始日期 ${ds} 在配置时区下向下对齐到网格：${fmtDT(startT)}`);
    }
    const endT = addP(cur, c.period, c.span * c.pre), wins = [];
    for (let w = startT; w <= endT; w = addP(w, c.period, c.span)) { wins.push({name: pName(w, c.period, c.span), start: w, end: addP(w, c.period, c.span)}); if (wins.length > PM_MAX_INIT + 1) break; }
    const n = wins.length, first = wins[0], last = wins[wins.length - 1];
    if (c.ctype === 'int' && wins.some(w => (w.end - cfgOff(c, w.end) * 3600000) / 1000 > 2147483647)) chk('column_type', 'error', 'int 类型存秒级时间戳，分区边界超过 2038 年上限（2147483647），请改用 bigint');
    if (n > PM_MAX_INIT) chk('partition_count', 'error', `将创建的分区数超过上限 ${PM_MAX_INIT}（从 ${fmtDT(startT)} 到 ${fmtDT(endT)}），请缩小范围（指定起始日期）或加大周期 / 跨度`);
    else if (n > PM_WARN_INIT) chk('partition_count', 'warn', `将创建 ${n} 个分区（超过 ${PM_WARN_INIT}），ALTER 会很慢、元数据也会变大，请确认起点与周期`);
    else chk('partition_count', 'ok', `将创建 ${n} 个分区（${first.name} ~ ${last.name}）`);
    checks.sort((a, b) => pmInitCheckSort[a.level] - pmInitCheckSort[b.level]);
    const canExec = !checks.some(x => x.level === 'error');
    if (mode === 'current') warns.push(`“从当前窗口开始”不读存量数据：早于第一个分区上界 ${fmtDT(first.end)} 的行（含更早的存量数据）会全部落进第一个分区 ${first.name}（之后按保留数删除它会丢掉这些数据）；晚于最后一个分区上界 ${fmtDT(last.end)} 的行会让 ALTER 失败。表里已有更早的数据，可改选“从最早数据开始”或“指定起始日期”`);
    const init = {mode, date: st.date || '', start: fmtDT(startT), count: n, first: first.name, last: last.name, lastBound: fmtDT(last.end), earliest, canExec, checks, notes,
      confirm: {table: tbn, partition_count: n, first_partition: first.name, last_partition: last.name}};
    let stmts = [];
    if (canExec) {
      const lit = w => boundLit(c, w.end), pdef = w => `PARTITION \`${w.name}\` VALUES LESS THAN (${lit(w)})`, head = `ALTER TABLE ${pmTb(c)} PARTITION BY RANGE ${c.ctype === 'datetime' ? 'COLUMNS' : ''}(\`${c.col}\`) (`;
      const sql = head + wins.map(pdef).join(', ') + ')';
      const short = n > 6 ? head + '\n  ' + wins.slice(0, 3).map(pdef).join(',\n  ') + `,\n  …（省略 ${n - 5} 个分区，实际语句包含全部 ${n} 个）…,\n  ` + wins.slice(-2).map(pdef).join(',\n  ') + ')' : sql;
      stmts = [{kind: 'init', part: wins.map(w => w.name), bound: init.lastBound, danger: true, heavy: true, sql, short}];
    }
    return Object.assign(R('', ''), {error: '', init, adds: wins, stmts, warns});
  }

  /* 计划（服务端 BuildPlan 的原型版）：返回 {error, code?} 或 {kind, adds, drops, reorg, stmts, warns}；st 只对未分区的 MySQL 表有意义（初始化起点） */
  function ptmPlan(c, st) {
    const inst = instOf(c.inst), dia = pmDialect(inst), tbn = `${c.db}.${c.tbl}`;
    if (c.unsupported || !dia) return {error: `该类型（${inst.type}）的实例暂不支持分区维护，目前支持 MySQL / PolarDB / SelectDB / Doris`, code: 'unsupported_engine', kind: 'maintain', dia};
    const tz = pmTzParse(c.tz, c.inst);
    if (tz.err) return {error: tz.err, code: 'bad_request', kind: 'maintain', dia};
    if (!(c.span >= 1 && c.span <= 255)) return {error: '周期跨度须在 1–255 之间。', code: 'bad_request', kind: 'maintain', dia, tz};
    const E = (error, code) => ({error, code, kind: 'maintain', dia, tz});
    if (!c.parts) {
      if (dia === 'doris') return E(`SelectDB / Doris 不支持自动初始化未分区表，请先把 ${tbn} 建为 RANGE 分区表`, 'not_partitioned');
      return ptmInitPlan(c, st, tz);
    }
    const fin = c.parts.filter(p => p.end != null), pmax = c.parts.find(p => p.end == null && !p.unk), unk = c.parts.filter(p => p.unk), warns = tz.warns.slice();
    if (dia === 'doris' && pmax) return E(`${tbn} 含 MAXVALUE 分区 ${pmax.name}，SelectDB / Doris 不能在 MAXVALUE 之后追加分区`, 'max_partition');
    unk.forEach(p => warns.push(`分区 ${p.name} 的 Range 读不到上界（SHOW PARTITIONS 返回：${p.rng}），视为未知：不会被删除，也不参与续建起点的判断`));
    const hist = fin.filter(p => p.end <= T0), drops = c.retain > 0 ? hist.slice(0, Math.max(0, hist.length - c.retain)) : [];
    let fut = fin.filter(p => p.end > T0).length, e = fin.length ? fin[fin.length - 1].end : startOfPeriod(T0, c.period);
    const adds = [];
    while (fut < 1 + c.pre) {
      const start = e; e = addP(e, c.period, c.span); adds.push({name: pName(start, c.period, c.span), start, end: e}); if (e > T0) fut++;
      if (adds.length > 100) return E('一次最多创建 100 个分区，请检查周期 / 预创建数是否合理。', 'too_many');
    }
    if (c.ctype === 'int') { const bad = adds.find(a => (a.end - cfgOff(c, a.end) * 3600000) / 1000 > 2147483647); if (bad) return E(`int 类型存秒级时间戳，分区 ${bad.name} 的边界超过 2038 年上限（2147483647），请改用 bigint。`, 'int_overflow'); }
    const dup = adds.find(a => c.parts.some(p => p.name === a.name));
    if (dup) { const ex = c.parts.find(p => p.name === dup.name); return E(`将创建的分区 ${dup.name} 与现有分区重名但边界不同（现有上界 ${ex.unk ? '未知' : ex.end == null ? 'MAXVALUE' : fmtDT(ex.end)}，将创建的上界 ${fmtDT(dup.end)}），已拒绝`, 'name_conflict'); }
    const tb = pmTb(c), stmts = [];
    if (dia === 'doris') {
      adds.forEach(a => stmts.push({kind: 'add', part: a.name, sql: `ALTER TABLE ${tb} ADD PARTITION ${a.name} VALUES [(${dorisLit(c, a.start)}), (${dorisLit(c, a.end)}))`}));
    } else {
      const part = a => `PARTITION ${a.name} VALUES LESS THAN (${boundLit(c, a.end)})`;
      if (adds.length && pmax) stmts.push({kind: 'reorg', heavy: true, sql: `ALTER TABLE ${tb} REORGANIZE PARTITION ${pmax.name} INTO (\n  ${adds.map(part).join(',\n  ')},\n  PARTITION ${pmax.name} VALUES LESS THAN (MAXVALUE))`});
      else adds.forEach(a => stmts.push({kind: 'add', part: a.name, sql: `ALTER TABLE ${tb} ADD PARTITION (${part(a)})`}));
    }
    drops.forEach(p => stmts.push({kind: 'drop', part: p.name, sql: `ALTER TABLE ${tb} DROP PARTITION ${p.name}`}));
    return {kind: 'maintain', dia, tz, adds, drops, reorg: dia === 'mysql' && !!(adds.length && pmax), stmts, warns, unk};
  }
  function ptmApply(c, plan) {
    if (plan.kind === 'init') { c.parts = plan.adds.map(w => ({name: w.name, end: w.end})); return; }
    plan.drops.forEach(p => { const i = c.parts.findIndex(x => x.name === p.name); if (i >= 0) c.parts.splice(i, 1); });
    const mi = c.parts.findIndex(p => p.end == null && !p.unk);
    const ads = plan.adds.map(a => ({name: a.name, end: a.end}));
    if (mi >= 0) c.parts.splice(mi, 0, ...ads); else c.parts.push(...ads);
  }

  /* ---------- 种子数据 ---------- */
  let PM_ID = 18, PM_LID = 9000;
  const pmMk = (id, inst, db, tbl, col, ctype, period, span, retain, pre, enabled, remark, parts, extra) => Object.assign({id, inst, db, tbl, col, ctype, period, span, retain, pre, tz: 'Asia/Shanghai', enabled, remark, created: '2026-08-0' + id, parts}, extra || {});
  const PM_CFG = [
    pmMk(1, 1, 'order_db', 'order_log', 'created_at', 'datetime', 'month', 1, 12, 3, true, '订单流水日志（按月，保留 12 个月）', seedParts('month', 1, 13, 3, false)),
    pmMk(2, 4, 'user_db', 'login_log', 'created_at', 'datetime', 'day', 1, 90, 7, true, '登录日志（按天，保留 90 天）', seedParts('day', 1, 90, 6, false)),
    pmMk(3, 7, 'order_db', 'order_log', 'created_at', 'datetime', 'day', 1, 14, 3, false, '测试库订单流水（含 pmax，暂停维护）', seedParts('day', 1, 14, 1, true)),
    pmMk(4, 3, 'pay_db', 'payments', 'paid_at', 'datetime', 'day', 1, 30, 5, true, '支付单（误配：表尚未分区）', null),
    pmMk(5, 6, 'ReportDW', 'fact_sales', 'date_key', 'int', 'month', 1, 24, 2, false, '销售事实表（SQL Server，原系统遗留配置）', null, {unsupported: true}),
    /* —— 并入外部程序后新增的演示配置：MySQL 未分区表（初始化流程与各拒绝条件）/ SelectDB / Doris —— */
    pmMk(6, 1, 'order_events', 'order_event', 'event_time', 'datetime', 'day', 1, 30, 7, true, '订单事件（未分区，可初始化；时区留空 = 跟随目标库）', null, {tz: '', created: '2026-09-28'}),
    pmMk(7, 1, 'order_events', 'order_trace', 'ts_ms', 'bigint', 'hour', 1, 168, 6, true, '订单链路追踪（未分区，bigint 毫秒，按小时：从最早数据开始会超过 200 个分区）', null, {tz: 'UTC+8', created: '2026-09-28'}),
    pmMk(8, 1, 'order_events', 'order_audit_bin', 'ts', 'int', 'day', 1, 90, 5, true, '订单审计流水（未分区，12 亿行：读最早数据会超时）', null, {tz: '+08:00', created: '2026-09-28'}),
    pmMk(9, 1, 'order_events', 'order_snapshot', 'snap_at', 'datetime', 'week', 1, 8, 2, true, '订单快照（未分区；有不含分区字段的唯一键）', null, {created: '2026-09-28'}),
    pmMk(10, 1, 'order_events', 'order_ref_log', 'created_at', 'datetime', 'month', 1, 12, 2, true, '订单关联日志（未分区；有外键）', null, {created: '2026-09-28'}),
    pmMk(11, 1, 'order_events', 'order_memo_old', 'created_at', 'datetime', 'month', 1, 0, 2, false, '订单备注（旧表，MyISAM，不支持分区）', null, {created: '2026-09-28'}),
    pmMk(12, 1, 'order_events', 'order_legacy_ts', 'create_ts', 'bigint', 'day', 1, 30, 3, true, '旧时间戳表（字段实际是 varchar，配置写成了 bigint）', null, {created: '2026-09-28'}),
    pmMk(13, 20, 'dw_ads', 'ads_traffic_hour', 'stat_hour', 'datetime', 'hour', 1, 72, 6, true, '渠道流量小时汇总（Doris，datetime 双引号边界）', seedParts('hour', 1, 75, 3, false), {created: '2026-09-28'}),
    pmMk(14, 20, 'dw_ads', 'dwd_event_detail', 'event_ts', 'bigint', 'day', 1, 30, 7, true, '事件明细（Doris，bigint 13 位毫秒）', seedParts('day', 1, 30, 7, false), {created: '2026-09-28'}),
    pmMk(15, 20, 'dw_ads', 'ods_click_raw', 'click_ts', 'int', 'day', 1, 15, 4, true, '点击原始流水（Doris，int 10 位秒；2 个老分区的 Range 读不到上界）', (() => {
      const p = seedParts('day', 1, 20, 4, false);
      Object.assign(p[0], {end: null, unk: true, rng: '[types: [INT]; keys: [1.78971E9]; ..types: [INT]; keys: [1.78980E9]; )'});
      Object.assign(p[1], {end: null, unk: true, rng: '[types: [INT]; keys: [ ]; )'});
      return p;
    })(), {created: '2026-09-28'}),
    pmMk(16, 21, 'log_center', 'app_access_log', 'log_time', 'datetime', 'day', 1, 14, 3, true, '应用访问日志（SelectDB；表里有 MAXVALUE 分区，Doris 不能追加）', seedParts('day', 1, 14, 3, true), {created: '2026-09-28'}),
    pmMk(17, 21, 'log_center', 'audit_trail', 'op_time', 'datetime', 'week', 1, 12, 2, true, '操作审计（SelectDB，按周，分区名取窗口最后一天）', seedParts('week', 1, 12, 2, false), {created: '2026-09-28'}),
    pmMk(18, 20, 'dw_ads', 'dim_channel', 'created_at', 'datetime', 'day', 1, 30, 3, false, '渠道维表（Doris 未分区，不能初始化）', null, {created: '2026-09-28'}),
  ];
  const PM_LOGS = [];
  const pmLog = (cfg, at, source, operator, status, stmts) => PM_LOGS.push({id: ++PM_LID, cfg, at, source, operator, status, stmts});
  (function seedLogs() {
    const A = (db, tb, n, lt) => ({sql: `ALTER TABLE \`${db}\`.\`${tb}\` ADD PARTITION (PARTITION ${n} VALUES LESS THAN (${lt}))`, st: 'ok', ms: 120});
    const Dr = (db, tb, n) => ({sql: `ALTER TABLE \`${db}\`.\`${tb}\` DROP PARTITION ${n}`, st: 'ok', ms: 380});
    pmLog(2, '2026-10-07 00:05:01', '外部程序', 'partman-daemon', '成功', [A('user_db', 'login_log', 'p20261013', "'2026-10-14'"), Dr('user_db', 'login_log', 'p20260708')]);
    pmLog(2, '2026-10-06 00:05:02', '外部程序', 'partman-daemon', '成功', [A('user_db', 'login_log', 'p20261012', "'2026-10-13'"), Dr('user_db', 'login_log', 'p20260707')]);
    pmLog(2, '2026-09-22 00:05:04', '外部程序', 'partman-daemon', '部分失败', [A('user_db', 'login_log', 'p20260928', "'2026-09-29'"), {sql: 'ALTER TABLE `user_db`.`login_log` DROP PARTITION p20260623', st: 'failed', err: 'ERROR 1205 (HY000): Lock wait timeout exceeded; try restarting transaction（目标表上有长事务持有元数据锁）', ms: 15000}]);
    pmLog(1, '2026-10-01 00:05:02', '外部程序', 'partman-daemon', '成功', [A('order_db', 'order_log', 'p202701', "'2027-02-01'"), Dr('order_db', 'order_log', 'p202509')]);
    pmLog(1, '2026-09-01 00:05:01', '外部程序', 'partman-daemon', '成功', [A('order_db', 'order_log', 'p202612', "'2027-01-01'"), Dr('order_db', 'order_log', 'p202508')]);
    pmLog(3, '2026-09-28 15:20:41', '手工', 'dba_li', '成功', [{sql: "ALTER TABLE `order_db`.`order_log` REORGANIZE PARTITION pmax INTO (\n  PARTITION p20260929 VALUES LESS THAN ('2026-09-30'),\n  PARTITION p20260930 VALUES LESS THAN ('2026-10-01'),\n  PARTITION pmax VALUES LESS THAN (MAXVALUE))", st: 'ok', ms: 2310}]);
    pmLog(3, '2026-09-20 03:00:01', '定时', 'system', '成功', [A('order_db', 'order_log', 'p20260927', "'2026-09-28'")]);
    pmLog(4, '2026-10-05 00:05:03', '外部程序', 'partman-daemon', '失败', [{sql: '-- 读取 information_schema.PARTITIONS', st: 'failed', err: '表 pay_db.payments 不是分区表（PARTITION_NAME 为 NULL），无法生成计划', ms: 8}]);
    /* —— 并入后新增的演示日志：SelectDB / Doris（双引号边界、week 命名）、失败、被截断的追赶运行 —— */
    const DA = (db, tb, n, lo, hi, ms) => ({sql: `ALTER TABLE \`${db}\`.\`${tb}\` ADD PARTITION ${n} VALUES [("${lo}"), ("${hi}"))`, st: 'ok', ms: ms || 70});
    const epoch8 = (y, m, d, h) => Date.UTC(y, m - 1, d, h || 0) - 8 * 3600000;
    pmLog(13, '2026-10-07 09:00:03', '定时', 'system', '成功', [DA('dw_ads', 'ads_traffic_hour', 'p2026100713', '2026-10-07 13:00:00', '2026-10-07 14:00:00'), Dr('dw_ads', 'ads_traffic_hour', 'p2026100406')]);
    pmLog(14, '2026-10-06 16:20:41', '手工', 'dba_li', '成功', [DA('dw_ads', 'dwd_event_detail', 'p20261013', String(epoch8(2026, 10, 13)), String(epoch8(2026, 10, 14)), 95)]);
    pmLog(15, '2026-10-07 00:05:02', '定时', 'system', '成功', [DA('dw_ads', 'ods_click_raw', 'p20261011', String(epoch8(2026, 10, 11) / 1000), String(epoch8(2026, 10, 12) / 1000))]);
    pmLog(16, '2026-10-07 00:05:04', '定时', 'system', '失败', [{sql: '-- SHOW PARTITIONS FROM `log_center`.`app_access_log`', st: 'failed', err: 'log_center.app_access_log 含 MAXVALUE 分区 pmax，SelectDB / Doris 不能在 MAXVALUE 之后追加分区（max_partition）', ms: 41}]);
    pmLog(17, '2026-10-05 09:00:02', '定时', 'system', '成功', [DA('log_center', 'audit_trail', 'p20261025', '2026-10-19 00:00:00', '2026-10-26 00:00:00')]);
    pmLog(13, '2026-10-03 03:00:05', '定时', 'system', '成功', (() => {
      const o = [], base = Date.UTC(2026, 9, 3, 3);                      // 维护停了一阵，追赶 100 个小时分区（单次上限）：运行输出超过 200 行，会被截断
      for (let k = 0; k < 100; k++) { const lo = base + k * 3600000, hi = lo + 3600000; o.push(DA('dw_ads', 'ads_traffic_hour', 'p' + fmtDT(lo).replace(/[- :]/g, '').slice(0, 10), fmtDT(lo), fmtDT(hi), 55 + (k * 7) % 40)); }
      return o;
    })());
    /* 定时轮摘要：source=定时、cfg=0；items 逐个配置给出结果与原因 */
    const pmRound = (at, status, items) => PM_LOGS.push({id: ++PM_LID, cfg: 0, round: true, at, source: '定时', operator: 'system', status, stmts: [], items});
    pmRound('2026-10-07 09:00:00', '部分失败', [
      {cfg: 1, res: 'skip', why: '近期日志保护窗口：该配置 14 分钟前（08:46:02，来源 手工）已有维护日志，本轮跳过'},
      {cfg: 2, res: 'ok', why: '续建 1 个分区；partman_auto_drop=false，跳过删除 1 个过期分区（p20260708）'},
      {cfg: 5, res: 'skip', why: '实例类型 MsSQL 暂不支持分区维护，跳过'},
      {cfg: 6, res: 'skip', why: '表未分区：定时维护不会初始化（需在“预览并执行”里手工确认），只记服务日志'},
      {cfg: 13, res: 'ok', why: '续建 1 个分区；删除 1 个过期分区'},
      {cfg: 14, res: 'skip', why: '维护锁被占用：GET_LOCK(dbt_partman_14) 被另一个进程持有（409 busy），本轮跳过'},
      {cfg: 16, res: 'fail', why: '计划失败 max_partition：表含 MAXVALUE 分区 pmax，SelectDB / Doris 不能在 MAXVALUE 之后追加分区'},
    ]);
    pmRound('2026-10-06 09:00:00', '成功', [
      {cfg: 2, res: 'ok', why: '续建 1 个分区；partman_auto_drop=false，跳过删除 1 个过期分区（p20260707）'},
      {cfg: 13, res: 'ok', why: '分区已是最新，无变更（不推送）'},
      {cfg: 5, res: 'skip', why: '实例类型 MsSQL 暂不支持分区维护，跳过'},
    ]);
    PM_LOGS.sort((a, b) => b.at.localeCompare(a.at));
  })();
  const pmGet = id => PM_CFG.find(c => c.id == id);
  const pmLast = c => PM_LOGS.filter(l => l.cfg === c.id).sort((a, b) => b.at.localeCompare(a.at))[0];
  /* 状态：待续建 / 待清理 / 未分区（MySQL 可初始化，契约 state=unpartitioned）/ 异常 / 暂不支持 */
  function pmStatus(c) {
    if (c.unsupported || !pmDialect(instOf(c.inst))) return {t: '暂不支持', cls: ''};
    const p = ptmPlan(c);
    if (p.kind === 'init') {
      if (p.init.canExec) return {t: '未分区 · 可初始化', cls: 'info'};
      const er = p.init.checks.find(x => x.level === 'error'); return {t: '未分区 · 不可初始化', cls: 'warn', tip: '不能初始化：' + (er ? er.message : p.error)};
    }
    if (p.error) return {t: '异常', cls: 'bad', tip: p.error};
    if (p.drops.length && p.stmts.some(s => s.kind !== 'drop')) return {t: '待续建 + 待清理', cls: 'warn'};
    if (p.drops.length) return {t: `待清理 ${p.drops.length}`, cls: 'warn'};
    if (p.adds.length) return {t: `待续建 ${p.adds.length}`, cls: 'info'};
    return {t: '正常', cls: 'ok'};
  }
  const PS = {inst: '', en: '', ctype: '', period: '', kw: '', logCfg: '', logSrc: ''};
  const pmVisible = () => PM_CFG.filter(c => visibleInstances().some(i => i.id === c.inst));

  pages.partman = {
    render(sub) {
      const cur = sub === 'log' ? 'log' : 'cfg';
      return `${tabs('partman', [['cfg', '分区配置'], ['log', '维护日志']], cur)}${cur === 'log' ? ptmLogHtml() : ptmCfgHtml()}`;
    },
    init(sub) { if (sub !== 'log') ptmDraw(); }
  };
  function ptmCfgHtml() {
    const insts = [...new Set(pmVisible().map(c => c.inst))].map(instOf);
    return `<div class="notice ${PM_AUTO.run ? 'ok' : 'warn'}"><span>⏱</span><div><b>定时自动维护：${PM_AUTO.run ? '已开启' : '未开启'}</b> · 自动删除过期分区：${PM_AUTO.drop ? '开' : '关'} · 每 ${PM_AUTO.interval} 分钟一轮 · 推送时机 ${esc(PM_AUTO.notifyOn)}</div></div>
      <div id="ptmKpi"></div>
      <div class="toolbar">
        <select data-onchange="ptmFilter" data-k="inst"><option value="">全部实例</option>${insts.map(i => opt(i.id, i.name, PS.inst)).join('')}</select>
        <select data-onchange="ptmFilter" data-k="en"><option value="">自动维护：全部</option>${opt('1', '开', PS.en)}${opt('0', '关', PS.en)}</select>
        <select data-onchange="ptmFilter" data-k="ctype"><option value="">字段类型：全部</option>${['int', 'bigint', 'datetime'].map(t => opt(t, t, PS.ctype)).join('')}</select>
        <select data-onchange="ptmFilter" data-k="period"><option value="">周期：全部</option>${PM_PERIODS.map(([k, t]) => opt(k, t, PS.period)).join('')}</select>
        <input type="search" placeholder="搜索库名 / 表名 / 说明" data-oninput="ptmFilterKw" value="${esc(PS.kw)}" style="min-width:220px"><span class="sp"></span>
        ${B('partman.edit', '推送设置', {cls: '', data: {op: 'webhook'}})}${B('partman.add', '＋ 新增配置', {cls: 'primary'})}</div>
      <div class="card" id="ptmList"></div>`;
  }
  const diaTag = i => { const d = pmDialect(i); return d === 'doris' ? tag(i.type, 'info') : ''; };
  function ptmDraw() {
    const el = $('#ptmList'); if (!el) return;
    const kw = PS.kw.trim().toLowerCase();
    const all = pmVisible(), list = all.filter(c => (!PS.inst || c.inst == PS.inst) && (PS.en === '' || (c.enabled ? '1' : '0') === PS.en) && (!PS.ctype || c.ctype === PS.ctype) && (!PS.period || c.period === PS.period)
      && (!kw || (c.db + '.' + c.tbl + ' ' + c.remark).toLowerCase().includes(kw)));
    const sts = all.map(pmStatus), n = t => sts.filter(s => s.t.startsWith(t)).length;
    $('#ptmKpi').innerHTML = `<div class="pq-kpis"><div class="card kpi"><div class="l">配置数</div><div class="v">${all.length}</div></div><div class="card kpi"><div class="l">自动维护已开启</div><div class="v">${all.filter(c => c.enabled).length}</div></div>
      <div class="card kpi"><div class="l">待续建</div><div class="v">${n('待续建')}</div></div><div class="card kpi"><div class="l">待清理</div><div class="v">${sts.filter(s => /待清理|待续建 \+/.test(s.t)).length}</div></div>
      <div class="card kpi"><div class="l">未分区</div><div class="v">${n('未分区')}</div></div><div class="card kpi"><div class="l">异常</div><div class="v" style="color:${n('异常') ? 'var(--bad)' : 'inherit'}">${n('异常')}</div></div></div>`;
    el.innerHTML = `<div class="tbl-wrap"><table><thead><tr><th>#</th><th>实例 / 库.表</th><th>分区键 · 类型</th><th>周期</th><th>预创建</th><th>保留</th><th>时区</th><th>自动维护</th><th>上次执行</th><th>状态</th><th>操作</th></tr></thead><tbody>
      ${list.map(c => { const st = pmStatus(c), last = pmLast(c), inst = instOf(c.inst), tzp = pmTzParse(c.tz, c.inst); return `<tr><td>${c.id}</td><td><div class="nowrap small muted">${esc(inst.name)} ${diaTag(inst)}</div><code>${esc(c.db)}.${esc(c.tbl)}</code><div class="small muted">${esc(c.remark)}</div></td>
        <td><code>${esc(c.col)}</code> <span class="small muted">${c.ctype}${c.ctype === 'int' ? '（秒）' : c.ctype === 'bigint' ? '（毫秒）' : ''}</span></td><td>每 ${c.span} ${periodName(c.period)}</td><td>${c.pre}</td><td>${c.retain ? c.retain + ' 个' : '全部保留'}</td>
        <td class="nowrap small">${tzp.err ? tag('非法', 'bad') : tzp.follow ? `<span>跟随目标库</span><div class="muted">${esc(tzp.name)}</div>` : `${esc(c.tz)}${tzp.warns.length ? ' ' + `<span class="tag warn">夏令时</span>` : ''}`}</td>
        <td>${c.enabled ? tag('开', 'ok') : tag('关')}${c.enabled && !PM_AUTO.run ? '<div class="small muted">定时未开启</div>' : ''}</td>
        <td class="nowrap small">${last ? esc(last.at.slice(5, 16)) + `<div class="muted">${esc(last.source)} · ${last.status === '成功' ? '成功' : '<span style="color:var(--bad)">' + esc(last.status) + '</span>'}</div>` : '<span class="muted">—</span>'}</td>
        <td>${tag(st.t, st.cls)}${st.tip ? `<div class="small muted" style="max-width:220px" title="${esc(st.tip)}">${esc(clip(st.tip, 36))}</div>` : ''}</td>
        <td><div class="ops">${B('partman.run', '预览并执行', {cls: 'sm', data: {id: c.id}})}${B('partman.edit', '编辑', {cls: 'sm', data: {id: c.id}})}${B('partman.edit', c.enabled ? '停用' : '启用', {cls: 'sm', data: {id: c.id, tg: 1}})}<button class="btn sm" data-on="ptmLog" data-id="${c.id}">日志</button>${B('partman.delete', '删除', {cls: 'sm danger', data: {id: c.id}})}</div></td></tr>`; }).join('') || `<tr><td colspan="11">${empty(all.length ? '没有符合条件的配置' : '你可见的实例上还没有分区维护配置', '◫')}</td></tr>`}</tbody></table></div>`;
  }
  ON.ptmFilter = (d, el) => { PS[d.k] = el.value; ptmDraw(); };
  ON.ptmFilterKw = (d, el) => { PS.kw = el.value; ptmDraw(); };
  ON.ptmLog = d => { PS.logCfg = d.id; go('partman', 'log'); };

  /* ---------- 运行输出（原程序每张表的运行日志风格：毫秒时间戳 + INFO / WARN / ERROR） ---------- */
  const PM_OUT_CAP = 200, OUTS = {}, OUT_REG = {};
  const pmTs = ms => fmtDT(ms) + '.' + String(Math.floor(ms) % 1000).padStart(3, '0');
  const LV_RANK = {INFO: 0, WARN: 1, ERROR: 2};
  const stmtInfo = sql => {
    let m;
    if ((m = /ADD PARTITION \(PARTITION (\w+)/.exec(sql)) || (m = /ADD PARTITION (\w+) VALUES/.exec(sql))) return {k: 'add', n: m[1]};
    if ((m = /DROP PARTITION (\w+)/.exec(sql))) return {k: 'drop', n: m[1]};
    if (/REORGANIZE PARTITION/.test(sql)) return {k: 'reorg', n: (/REORGANIZE PARTITION (\w+)/.exec(sql) || [])[1] || ''};
    if (/PARTITION BY RANGE/.test(sql)) return {k: 'init', n: ''};
    return {k: 'other', n: ''};
  };
  /* 推送：按 partman_notify_on / 通道启用状态给出每个通道的结果（原程序 push.log 风格，地址脱敏） */
  function pmPushLines(failed, execN) {
    const out = [], pol = PM_AUTO.notifyOn, on = PM_WH.filter(w => w.isenabled);
    if (!execN && !failed) return out;
    if (pol === 'never') { out.push(['INFO', 'partman_notify_on=never，不推送']); return out; }
    if (pol === 'on_failure' && !failed) { out.push(['INFO', 'partman_notify_on=on_failure，本次全部成功，不推送']); return out; }
    if (!on.length) { out.push(['INFO', '没有启用的推送通道（dbt_partman_webhook.isenabled=1），不推送']); return out; }
    out.push(['INFO', `partman_notify_on=${pol}：本次${failed ? '有失败' : '执行成功'}，向 ${on.length} 个启用通道推送`]);
    on.forEach(w => {
      const r = pmWhSend(w, false), ment = w.mention.length && (w.mention_on === 'always' || (w.mention_on === 'on_failure' && failed)) ? `，@ ${w.mention.join(' ')}` : (w.mention.length ? `（mention_on=${w.mention_on}，本次不 @）` : '');
      out.push([r.ok ? 'INFO' : 'WARN', `推送 ${w.type} ${maskUrl(w.url)} → HTTP ${r.status || '-'} ${r.resp || r.message} ok=${r.ok}${ment}${r.ok ? '' : '（推送失败不影响维护结果，只记服务日志）'}`]);
    });
    return out;
  }
  /* 一次维护的运行输出。o = {at, source, dry, stmts:[{sql, st, ms, err}], plan?, nBefore?, wasPartitioned?, withPush}；没有 plan 时（历史日志）由语句反推 */
  function pmRunTrace(c, o) {
    const inst = instOf(c.inst), dia = pmDialect(inst), tz = pmTzParse(c.tz, c.inst), auto = o.source === '定时', tbn = `${c.db}.${c.tbl}`, stm = o.stmts || [], plan = o.plan || null;
    const L = [], one = s => String(s).replace(/\s+/g, ' ').trim();
    let t = Date.parse(String(o.at).replace(' ', 'T') + 'Z') + hashOf(c.id + String(o.at)) % 600;
    const P = (lv, msg, adv) => { L.push({t: pmTs(t), lv, msg}); t += adv == null ? 2 + hashOf(msg + L.length) % 11 : adv; };
    const t0 = t;
    let status = '成功';
    const fin = () => {
      P('INFO', `=== reconcile done ===  耗时 ${Math.round(t - t0)} ms`);
      return {lines: L, truncated: 0, status};
    };
    P('INFO', `=== reconcile start | env-table=${tbn} col=${c.col} coltype=${c.ctype} period=${c.period} interval=${c.span} retain=${c.retain} preCreate=${c.pre} tz=${tz.err ? c.tz : (tz.iana || tz.name)}(${tz.err ? '?' : pmOffName(tz.off)}) dialect=${dia || inst.type} source=${o.source}${o.dry ? ' dryRun=true' : ' dryRun=false'} ===`);
    if (!dia) { P('ERROR', `实例 ${inst.name} 的类型 ${inst.type} 暂不支持分区维护，目前支持 MySQL / PolarDB / SelectDB / Doris`); status = '失败'; return fin(); }
    if (tz.err) { P('ERROR', tz.err); status = '失败'; return fin(); }
    if (tz.follow) P('INFO', `时区留空：读取目标库会话时区 SELECT TIMESTAMPDIFF(SECOND, UTC_TIMESTAMP(), NOW()) = ${Math.round(tz.off * 3600)}，使用固定偏移 ${pmOffName(tz.off)}`, 14);
    (tz.warns || []).forEach(w => P('WARN', w));
    P('INFO', `检查表是否存在: ${tbn} 存在`, 9);
    const was = o.wasPartitioned != null ? o.wasPartitioned : !!c.parts, nB = o.nBefore != null ? o.nBefore : (c.parts ? c.parts.length : 0);
    const execAll = () => {
      let okN = 0, failN = 0, skipped = 0;
      stm.forEach(s => {
        if (o.dry) { P('INFO', `[DRY-RUN] 预检不上线，待执行 SQL: ${one(s.short || s.sql)}`); return; }
        if (s.st === 'skipped') { skipped++; return; }
        P('INFO', `执行 SQL: ${one(s.short || s.sql)}`, s.ms || 80);
        if (s.st === 'failed') { P('ERROR', `执行失败: ${s.err || '未知错误'}`, 3); failN++; } else { P('INFO', '执行成功', 2); okN++; }
      });
      if (skipped) P('WARN', `前一条语句失败，已停止；其余 ${skipped} 条未执行`);
      if (failN) status = okN ? '部分失败' : '失败';
      return {okN, failN};
    };
    if (!was) {
      P('INFO', dia === 'doris' ? `SHOW PARTITIONS FROM \`${c.db}\`.\`${c.tbl}\` → 0 行（未分区）` : `检查分区状态: ${tbn} 未分区（information_schema.PARTITIONS.PARTITION_NAME 为 NULL）`, 8);
      if (dia === 'doris') { P('ERROR', `SelectDB / Doris 不支持自动初始化未分区表，请先把 ${tbn} 建为 RANGE 分区表`); status = '失败'; return fin(); }
      if (auto) { P('WARN', 'partman_auto: 未分区，定时维护不会初始化（需在“预览并执行”里手工确认）'); status = '跳过'; return fin(); }
      const ip = plan && plan.kind === 'init' ? plan : null;
      if (ip) {
        P('INFO', `初始化起点: start_mode=${ip.init.mode}${ip.init.mode === 'date' ? ' start_date=' + ip.init.date : ''}`);
        ip.init.checks.forEach(ch => P(ch.level === 'error' ? 'ERROR' : ch.level === 'warn' ? 'WARN' : 'INFO', `前置检查 ${ch.code}: ${ch.message}`, 4));
        ip.init.notes.forEach(x => P('INFO', x));
        if (ip.error) { P('ERROR', `${ip.error}（${ip.code}）`); status = '失败'; return fin(); }
        if (ip.init.earliest) P('INFO', `表含存量数据，最早 ${ip.init.earliest}，从窗口 ${ip.init.start} 开始建分区`, 1200);
        if (!ip.init.canExec) { P('ERROR', '前置检查不通过，不发 ALTER'); status = '失败'; return fin(); }
        P('INFO', `表未分区，准备首次建分区，共 ${ip.init.count} 个分区: ${ip.init.first} ... ${ip.init.last}`);
      } else {
        P('INFO', '表未分区，准备首次建分区（显式初始化）');
      }
      const r = execAll();
      if (!o.dry && o.withPush !== false) pmPushLines(status !== '成功', r.okN + r.failN).forEach(([lv, m]) => P(lv, m, 30));
      return fin();
    }
    P('INFO', dia === 'doris' ? `SHOW PARTITIONS FROM \`${c.db}\`.\`${c.tbl}\` → ${nB} 行；取 Range 里最后一个 keys 作为上界` : `检查分区状态: 已分区（${c.ctype === 'datetime' ? 'RANGE COLUMNS' : 'RANGE'}(${c.col})），现有 ${nB} 个分区`, 8);
    if (plan && plan.error) { P('ERROR', `计划失败: ${plan.error}（${plan.code}）`); status = '失败'; return fin(); }
    ((plan && plan.warns) || []).filter(w => !(tz.warns || []).includes(w)).forEach(w => P('WARN', w));
    const adds = plan ? plan.adds.map(a => a.name) : stm.filter(s => stmtInfo(s.sql).k === 'add').map(s => stmtInfo(s.sql).n);
    const drops = plan ? plan.drops.map(a => a.name) : stm.filter(s => stmtInfo(s.sql).k === 'drop').map(s => stmtInfo(s.sql).n);
    const reorg = plan ? plan.reorg : stm.some(s => stmtInfo(s.sql).k === 'reorg');
    if (!plan && stm.length === 1 && /^--/.test(stm[0].sql)) { P('ERROR', `读取现有分区失败: ${stm[0].err}`); status = '失败'; return fin(); }
    if (adds.length || reorg) {
      if (reorg) P('WARN', '表存在 MAXVALUE 兜底分区，ADD PARTITION 会失败，改用 REORGANIZE PARTITION（会重写 MAXVALUE 分区的数据；定时维护永不自动执行）', 6);
      if (reorg && auto && !o.dry) P('WARN', 'partman_auto: 需要 REORGANIZE，定时维护不执行，请手工处理');
      else P('INFO', `准备新增分区: ${adds.join(', ') || '（REORGANIZE 内含）'}`);
    } else P('INFO', 'nothing to add (future partitions already present)');
    if (!c.retain) P('INFO', 'retain<=0，跳过删除（不删除任何历史分区）');
    else if (drops.length) {
      if (auto && !PM_AUTO.drop && !o.dry && !stm.some(s => stmtInfo(s.sql).k === 'drop')) P('INFO', `partman_auto_drop=false，跳过删除 ${drops.length} 个过期分区: ${drops.join(', ')}`);
      else P('INFO', `准备删除过期分区(保留最近 ${c.retain} 个历史分区): ${drops.join(', ')}`);
    } else P('INFO', 'nothing to drop (no partition older than the retain window)');
    (plan && plan.unk ? plan.unk : []).forEach(p => P('INFO', `分区 ${p.name} 上界未知，已排除在 DROP 之外`, 1));
    const r = execAll();
    if (!o.dry && o.withPush !== false) pmPushLines(status !== '成功', r.okN + r.failN).forEach(([lv, m]) => P(lv, m, 30));
    return fin();
  }
  /* 定时一轮的摘要输出 */
  function pmRoundTrace(l) {
    const L = [], ok = l.items.filter(i => i.res === 'ok').length, skip = l.items.filter(i => i.res === 'skip').length, fail = l.items.filter(i => i.res === 'fail').length;
    let t = Date.parse(l.at.replace(' ', 'T') + 'Z') + hashOf(l.id) % 300;
    const P = (lv, msg, adv) => { L.push({t: pmTs(t), lv, msg}); t += adv == null ? 3 + hashOf(msg) % 17 : adv; };
    P('INFO', `=== partman_auto round start | configs=${l.items.length} interval=${PM_AUTO.interval}min auto_drop=${PM_AUTO.drop} notify_on=${PM_AUTO.notifyOn} ===`);
    l.items.forEach(i => {
      const c = pmGet(i.cfg), tb = c ? `${c.db}.${c.tbl}` : '#' + i.cfg;
      P(i.res === 'fail' ? 'ERROR' : i.res === 'skip' ? 'WARN' : 'INFO', `[#${i.cfg} ${tb}] ${i.res === 'skip' ? '跳过：' : i.res === 'fail' ? '失败：' : ''}${i.why}`, 40 + hashOf(tb) % 400);
    });
    P('INFO', `=== partman_auto round done | 维护 ${ok} / 跳过 ${skip} / 失败 ${fail} ===`);
    return {lines: L, truncated: 0, status: l.status};
  }
  /* 超过 200 行只保留头 190 + 尾 10，中间一行“…已截断 N 行” */
  function pmCap(out) {
    if (out.lines.length <= PM_OUT_CAP) return out;
    const n = out.lines.length - PM_OUT_CAP, lines = out.lines.slice(0, PM_OUT_CAP - 10).concat([{t: '', lv: 'INFO', msg: `…已截断 ${n} 行`, mark: true}], out.lines.slice(-10));
    return Object.assign({}, out, {lines, truncated: n});
  }
  const pmOutOf = l => l.out || (l.out = pmCap(l.round ? pmRoundTrace(l) : (() => { const c = pmGet(l.cfg); return c ? pmRunTrace(c, {at: l.at, source: l.source, stmts: l.stmts, withPush: true}) : {lines: [], truncated: 0}; })()));
  const lvStyle = lv => lv === 'ERROR' ? 'color:var(--bad)' : lv === 'WARN' ? 'color:var(--warn)' : '';
  function outLinesHtml(out, S) {
    const q = (S.q || '').toLowerCase(), r = LV_RANK[S.lv] || 0;
    const rows = out.lines.filter(x => x.mark || ((LV_RANK[x.lv] || 0) >= r && (!q || (x.t + ' ' + x.msg).toLowerCase().includes(q))));
    return {n: rows.length, html: rows.map(x => x.mark ? `<div class="muted" style="font-style:italic">${esc(x.msg)}</div>` : `<div style="${lvStyle(x.lv)}">${esc(x.t)} <b>[${x.lv}]</b> ${esc(x.msg)}</div>`).join('') || '<div class="muted">没有符合条件的输出行</div>'};
  }
  function outHtml(key, out, title) {
    OUT_REG[key] = Object.assign({title: title || key}, out);
    const S = OUTS[key] || (OUTS[key] = {lv: '', q: ''}), v = outLinesHtml(out, S);
    return `<div id="pmOut_${esc(key)}" style="margin-top:8px"><div class="row" style="gap:6px;margin-bottom:6px;flex-wrap:wrap">
      <select data-onchange="ptmOutLv" data-k="${esc(key)}" aria-label="日志级别"><option value="">全部级别</option>${opt('WARN', 'WARN 及以上', S.lv)}${opt('ERROR', '仅 ERROR', S.lv)}</select>
      <input type="search" placeholder="搜索输出" data-oninput="ptmOutQ" data-k="${esc(key)}" value="${esc(S.q)}" style="min-width:150px">
      <button class="btn sm" data-on="ptmOutCopy" data-k="${esc(key)}">复制</button><button class="btn sm" data-on="ptmOutDl" data-k="${esc(key)}">下载 .log</button>
      <span class="small muted" id="pmOutN_${esc(key)}">${v.n} / ${out.lines.length} 行${out.truncated ? ` · 已截断 ${out.truncated} 行` : ''}</span></div>
      <div id="pmOutPre_${esc(key)}" style="background:var(--panel2);border:1px solid var(--border);border-radius:8px;padding:8px 10px;max-height:320px;overflow:auto;font:12px/1.55 ui-monospace,Consolas,monospace;white-space:pre-wrap;word-break:break-all">${v.html}</div></div>`;
  }
  function outDraw(key) {
    const out = OUT_REG[key], S = OUTS[key]; if (!out || !S) return;
    const v = outLinesHtml(out, S), pre = $('#pmOutPre_' + key), n = $('#pmOutN_' + key);
    if (pre) pre.innerHTML = v.html;
    if (n) n.textContent = `${v.n} / ${out.lines.length} 行${out.truncated ? ` · 已截断 ${out.truncated} 行` : ''}`;
  }
  const outText = out => out.lines.map(x => x.mark ? x.msg : `${x.t} [${x.lv}] ${x.msg}`).join('\n') + '\n';
  ON.ptmOutLv = (d, el) => { OUTS[d.k].lv = el.value; outDraw(d.k); };
  ON.ptmOutQ = (d, el) => { OUTS[d.k].q = el.value; outDraw(d.k); };
  ON.ptmOutCopy = d => {
    const out = OUT_REG[d.k]; if (!out) return; const txt = outText(out);
    try { navigator.clipboard.writeText(txt).then(() => toast('已复制运行输出'), () => toast('复制失败，请手动选择文本')); } catch (e) { toast('当前环境不支持复制'); }
  };
  ON.ptmOutDl = d => {
    const out = OUT_REG[d.k]; if (!out) return;
    try { const a = document.createElement('a'); a.href = URL.createObjectURL(new Blob([outText(out)], {type: 'text/plain'})); a.download = `partman_${String(out.title).replace(/[^0-9A-Za-z_.-]+/g, '_')}.log`; document.body.appendChild(a); a.click(); a.remove(); toast('已开始下载 .log'); } catch (e) { toast('当前环境不支持下载'); }
  };
  ON.ptmOut = d => { const s = OUTS['l' + d.id] || (OUTS['l' + d.id] = {lv: '', q: ''}); s.open = !s.open; rerender(); };

  /* ---------- 维护日志 ---------- */
  function ptmLogHtml() {
    const vis = pmVisible(), ids = vis.map(c => c.id);
    let list = PM_LOGS.filter(l => (l.round ? !PS.logCfg && (!PS.logSrc || l.source === PS.logSrc) : ids.includes(l.cfg) && (!PS.logCfg || l.cfg == PS.logCfg) && (!PS.logSrc || l.source === PS.logSrc)));
    const stClass = s => /DROP/.test(s.sql) ? 'drop' : /REORGANIZE|PARTITION BY RANGE/.test(s.sql) ? 'reorg' : s.st === 'failed' ? 'failed' : 'add';
    return `<div class="toolbar"><select data-onchange="ptmLogF" data-k="logCfg"><option value="">全部配置</option>${vis.map(c => opt(c.id, `#${c.id} ${c.db}.${c.tbl}（${instOf(c.inst).name}）`, PS.logCfg)).join('')}</select>
        <select data-onchange="ptmLogF" data-k="logSrc"><option value="">全部来源</option>${['手工', '定时', '外部程序'].map(s => opt(s, s, PS.logSrc)).join('')}</select><span class="sp"></span><span class="small muted">共 ${list.length} 条</span></div>
      ${list.map(l => {
        const S = OUTS['l' + l.id] || {}, key = 'l' + l.id, hasOut = true;
        if (l.round) {
          const out = pmOutOf(l);
          return `<div class="card" style="margin-bottom:10px"><div class="row" style="margin-bottom:6px"><b>${esc(l.at)}</b>${tag('定时轮摘要', 'info')}<span class="small muted">操作人 ${esc(l.operator)}</span>${tag(l.status, l.status === '成功' ? 'ok' : l.status === '部分失败' ? 'warn' : 'bad')}<span class="sp"></span><span class="small muted">日志 #${l.id}</span></div>
            ${l.items.map(i => { const c = pmGet(i.cfg); return `<div class="small" style="padding:2px 0">${i.res === 'ok' ? '<span style="color:var(--ok)">✓ 维护</span>' : i.res === 'skip' ? '<span style="color:var(--warn)">⤼ 跳过</span>' : '<span style="color:var(--bad)">✗ 失败</span>'} <code>#${i.cfg} ${c ? esc(c.db + '.' + c.tbl) : ''}</code> <span class="muted">${esc(i.why)}</span></div>`; }).join('')}
            <div style="margin-top:6px"><button class="btn sm" data-on="ptmOut" data-id="${l.id}">${S.open ? '收起运行输出 ▾' : '运行输出 ▸'}（${out.lines.length} 行）</button></div>${S.open ? outHtml(key, out, `auto_round_${l.at.replace(/[- :]/g, '')}`) : ''}</div>`;
        }
        const c = pmGet(l.cfg), out = pmOutOf(l);
        return `<div class="card" style="margin-bottom:10px"><div class="row" style="margin-bottom:6px"><b>${esc(l.at)}</b><code>${esc(c.db)}.${esc(c.tbl)}</code><span class="small muted">${esc(instOf(c.inst).name)}</span>${tag(l.source, l.source === '手工' ? 'brand' : l.source === '定时' ? 'info' : '')}<span class="small muted">操作人 ${esc(l.operator)}</span>${tag(l.status, l.status === '成功' ? 'ok' : l.status === '部分失败' ? 'warn' : 'bad')}<span class="sp"></span><span class="small muted">日志 #${l.id}</span></div>
        ${l.stmts.slice(0, 8).map(s => `<div class="pq-stmt ${stClass(s)} ${s.st}"><div style="white-space:pre-wrap">${sqlHl(clip(s.sql, 600))}</div><div class="small muted">${s.st === 'ok' ? '✓ 成功 · ' + s.ms + ' ms' : s.st === 'failed' ? '<span style="color:var(--bad)">✗ 失败：' + esc(s.err) + '</span>' : '— 未执行（前一条失败，已停止）'}</div></div>`).join('')}${l.stmts.length > 8 ? `<div class="small muted">… 共 ${l.stmts.length} 条语句，仅显示前 8 条</div>` : ''}
        <div style="margin-top:6px"><button class="btn sm" data-on="ptmOut" data-id="${l.id}">${S.open ? '收起运行输出 ▾' : '运行输出 ▸'}（${out.lines.length} 行${out.truncated ? ' · 已截断 ' + out.truncated + ' 行' : ''}）</button></div>${S.open ? outHtml(key, out, `${c.db}_${c.tbl}_${l.at.replace(/[- :]/g, '')}`) : ''}</div>`;
      }).join('') || `<div class="card">${empty('没有维护日志', '◫')}</div>`}`;
  }
  ON.ptmLogF = (d, el) => { PS[d.k] = el.value; rerender(); };

  /* ---------- 新增 / 编辑配置（结构化表单） ---------- */
  const pmInsts = () => visibleInstances().filter(i => pmDialect(i) && i.role === 'master' && !isReadView(i));
  function ptmFormHtml(c) {
    const insts = pmInsts(), edit = !!c.id, inst = c.inst || (insts[0] || {}).id;
    return `<div class="pq-form2">
        <div><label>数据库实例</label><select id="ptmInst" data-onchange="ptmFInst" ${edit ? 'disabled' : ''}>${instOpts(insts, inst)}</select></div>
        <div><label>说明</label><input id="ptmRemark" value="${esc(c.remark || '')}" placeholder="例：订单流水日志"></div>
        <div><label>库名</label><select id="ptmDb" data-onchange="ptmFDb" ${edit ? 'disabled' : ''}></select></div>
        <div><label>表名</label><select id="ptmTbl" data-onchange="ptmFTbl" ${edit ? 'disabled' : ''}></select></div>
        <div><label>分区字段</label><select id="ptmCol" data-onchange="ptmFCol"></select></div>
        <div><label>字段类型</label><input id="ptmCtype" readonly></div>
        <div><label>分区周期</label><select id="ptmPeriod">${PM_PERIODS.map(([k, t]) => opt(k, `${k}（${t}）`, c.period || 'day')).join('')}</select></div>
        <div><label>周期跨度</label><input type="number" id="ptmSpan" min="1" max="255" value="${c.span || 1}"></div>
        <div><label>保留历史分区数（0 = 全部保留）</label><input type="number" id="ptmRetain" min="0" max="3650" value="${c.retain == null ? 30 : c.retain}"></div>
        <div><label>预创建分区数</label><input type="number" id="ptmPre" min="1" max="100" value="${c.pre || 3}"></div>
        <div><label>时区（可留空）</label><input id="ptmTz" list="ptmTzList" maxlength="40" autocomplete="off" data-oninput="ptmFTz" value="${esc(c.id ? (c.tz || '') : (c.tz == null ? 'Asia/Shanghai' : c.tz))}" placeholder="留空 = 跟随目标库会话时区">
          <datalist id="ptmTzList">${PM_TZS.map(t => `<option value="${esc(t)}"></option>`).join('')}</datalist>
          <div class="small muted" id="ptmTzHint" style="margin-top:4px"></div></div>
        <div><label>自动维护</label><label class="sw"><input type="checkbox" id="ptmEn" ${c.enabled === false ? '' : 'checked'}><i></i>开启</label></div>
      </div>
      <div class="small muted" style="margin-top:6px">常用项：${PM_TZS.map(t => `<a data-on="ptmTzPick" data-v="${esc(t)}">${esc(t)}</a>`).join(' · ')} · <a data-on="ptmTzPick" data-v="">留空（跟随目标库）</a></div>
      <div class="row" style="margin-top:12px"><button class="btn" data-on="ptmPrev">预览将执行的语句</button></div><div id="ptmPrevOut" style="margin-top:8px"></div>`;
  }
  function ptmTzHint() {
    const el = $('#ptmTzHint'); if (!el) return;
    const r = pmTzParse($('#ptmTz').value, +$('#ptmInst').value);
    el.innerHTML = r.err ? `<span style="color:var(--bad)">⛔ ${esc(r.err)}</span>` : `<span style="color:var(--ok)">✓ ${esc(r.label)}</span>${r.warns.map(w => `<div style="color:var(--warn)">⚠ ${esc(w)}</div>`).join('')}`;
  }
  ON.ptmFTz = () => ptmTzHint();
  ON.ptmTzPick = d => { const el = $('#ptmTz'); if (el) { el.value = d.v; ptmTzHint(); } };
  function ptmFillDb(sel) { const inst = +$('#ptmInst').value; $('#ptmDb').innerHTML = dbsOf(inst).map(d => opt(d.db, d.db, sel)).join(''); ptmFillTbl(); }
  function ptmFillTbl(sel, selCol) {
    const inst = +$('#ptmInst').value, db = $('#ptmDb').value, dia = pmDialect(instOf(inst));
    $('#ptmTbl').innerHTML = tablesOf(inst, db).filter(t => !t.view).map(t => `<option value="${esc(t.n)}" ${t.n === sel ? 'selected' : ''}>${esc(t.n)}${t.part ? '（分区表）' : dia === 'doris' ? '（未分区，Doris 不能初始化）' : '（未分区，可初始化）'}</option>`).join('') || '<option value="">（该库没有表）</option>';
    ptmFillCol(selCol);
  }
  function ptmFillCol(sel) {
    const t = findTable(+$('#ptmInst').value, $('#ptmDb').value, $('#ptmTbl').value), cols = t ? t.cols.filter(c => ctypeOf(c.t)) : [];
    $('#ptmCol').innerHTML = cols.map(c => `<option value="${esc(c.n)}" ${c.n === sel ? 'selected' : ''}>${esc(c.n)}（${esc(c.t)}）</option>`).join('') || '<option value="">（没有 datetime / int / bigint 字段）</option>';
    if (!sel && cols.length) { const pref = cols.find(c => /created_at|paid_at|event_time|stat_hour|log_time|op_time|ts_ms|event_ts|click_ts/.test(c.n)) || cols[0]; $('#ptmCol').value = pref.n; }
    ptmFillType();
  }
  function ptmFillType() { const t = findTable(+$('#ptmInst').value, $('#ptmDb').value, $('#ptmTbl').value), c = t && t.cols.find(x => x.n === $('#ptmCol').value); $('#ptmCtype').value = c ? ctypeOf(c.t) + (ctypeOf(c.t) === 'int' ? '（10 位秒级时间戳）' : ctypeOf(c.t) === 'bigint' ? '（13 位毫秒时间戳）' : '（YYYY-MM-DD HH:MM:SS）') : ''; }
  ON.ptmFInst = () => { ptmFillDb(''); ptmTzHint(); };
  ON.ptmFDb = () => ptmFillTbl();
  ON.ptmFTbl = () => ptmFillCol();
  ON.ptmFCol = () => ptmFillType();
  /* 读表单 → 草稿配置 + 校验错误 + 时区 warning */
  function ptmRead(base) {
    const inst = +$('#ptmInst').value, db = $('#ptmDb').value, tbl = $('#ptmTbl').value, col = $('#ptmCol').value, t = findTable(inst, db, tbl), cc = t && t.cols.find(x => x.n === col);
    const c = {id: base.id || 0, inst, db, tbl, col, ctype: cc ? ctypeOf(cc.t) : '', period: $('#ptmPeriod').value, span: +$('#ptmSpan').value, retain: +$('#ptmRetain').value, pre: +$('#ptmPre').value, tz: $('#ptmTz').value.trim(), enabled: $('#ptmEn').checked, remark: $('#ptmRemark').value.trim()};
    const errs = [], tzr = pmTzParse(c.tz, inst);
    if (!tbl) errs.push('请选择表'); if (!c.ctype) errs.push('请选择 datetime / int / bigint 类型的分区字段');
    if (!(Number.isInteger(c.span) && c.span >= 1 && c.span <= 255)) errs.push('周期跨度须是 1–255 的整数');
    if (!(Number.isInteger(c.retain) && c.retain >= 0 && c.retain <= 3650)) errs.push('保留历史分区数须是 0–3650 的整数');
    if (!(Number.isInteger(c.pre) && c.pre >= 1 && c.pre <= 100)) errs.push('预创建分区数须是 1–100 的整数');
    if (tzr.err) errs.push(tzr.err);
    if (!/^[0-9A-Za-z_$]{1,64}$/.test(db + '') || !/^[0-9A-Za-z_$]{1,64}$/.test(tbl + '')) errs.push('库名 / 表名只允许字母、数字、下划线、$（1–64 位）');
    if (PM_CFG.some(x => x.id !== c.id && x.inst === inst && x.db === db && x.tbl === tbl)) errs.push(`${db}.${tbl} 已经有分区维护配置（#${PM_CFG.find(x => x.id !== c.id && x.inst === inst && x.db === db && x.tbl === tbl).id}）`);
    c.parts = base.id && base.tbl === tbl && base.db === db && base.inst === inst ? base.parts : (t && t.part ? seedParts(c.period, c.span || 1, c.retain > 0 ? c.retain + 2 : 5, 1, false) : null);
    return {c, errs, warns: tzr.err ? [] : tzr.warns};
  }
  ON.ptmPrev = () => {
    const base = PTM_BASE || {}, {c, errs} = ptmRead(base), out = $('#ptmPrevOut'); if (!out) return;
    if (errs.length) { out.innerHTML = notice(errs.map(esc).join('；'), 'bad', '⛔'); return; }
    out.innerHTML = ptmPlanHtml(c, ptmPlan(c));
  };
  const KIND_LABEL = {add: '创建分区', drop: '删除分区', reorg: '重组 pmax（重写数据）', init: '初始化为分区表（整表重建）'};
  const stmtHtml = s => `<div class="pq-stmt ${s.kind === 'init' ? 'reorg' : s.kind}"><span class="small muted">${KIND_LABEL[s.kind]}</span><div style="white-space:pre-wrap">${sqlHl(s.short || s.sql)}</div></div>`;
  const tzLine = (c, p) => { const r = p.tz || pmTzParse(c.tz, c.inst); return r && !r.err ? `时区 ${esc(r.follow ? '跟随目标库会话时区 → ' + r.name : r.label)}` : ''; };
  const checksHtml = ch => `<table style="width:100%;margin:6px 0"><tbody>${ch.map(x => `<tr><td style="width:28px;text-align:center;color:${x.level === 'ok' ? 'var(--ok)' : x.level === 'warn' ? 'var(--warn)' : 'var(--bad)'}">${x.level === 'ok' ? '✓' : x.level === 'warn' ? '⚠' : '✗'}</td><td class="small mono nowrap">${esc(x.code)}</td><td class="small">${esc(x.message)}</td></tr>`).join('')}</tbody></table>`;
  function ptmTraceHtml(c, p) {
    const tr = pmCap(pmRunTrace(c, {at: qHostNow().slice(0, 16) + ':00', source: '手工', dry: true, plan: p, stmts: p.stmts || [], withPush: false}));
    return `<details style="margin-top:8px"><summary class="small">检查过程（${tr.lines.length} 行）</summary>${outHtml('prev', tr, `preview_${c.db}_${c.tbl}`)}</details>`;
  }
  function ptmPlanHtml(c, p) {
    const warn = (p.warns || []).map(w => notice(esc(w), 'warn', '⚠')).join('');
    const exist = c.parts && p.dia === 'doris' ? `<details style="margin-top:6px"><summary class="small">现有分区（末尾 6 个及读不到上界的）</summary><div class="small mono" style="white-space:pre-wrap;margin-top:4px">${c.parts.filter((x, i) => x.unk || i >= c.parts.length - 6).map(x => esc(x.name + '  Range: ' + (x.end == null && !x.unk ? '[types: [..]; keys: [MAXVALUE]; )' : dorisRange(c, x)))).join('\n')}</div></details>` : '';
    if (p.kind === 'init') {
      const I = p.init;
      return `${tzLine(c, p) ? `<div class="small muted">${tzLine(c, p)}</div>` : ''}${warn}
        <div class="small" style="margin:6px 0 4px"><b>前置检查</b></div>${checksHtml(I.checks)}${I.notes.map(x => `<div class="small muted">ℹ ${esc(x)}</div>`).join('')}
        ${p.error ? notice(`<b>${esc(p.error)}</b>（${esc(p.code)}）`, 'bad', '⛔') : ''}
        ${!p.error && I.canExec ? `<div class="small" style="margin:6px 0">起点窗口 <b>${esc(I.start)}</b>${I.earliest ? `（最早数据 ${esc(I.earliest)}）` : ''}；将创建 <b>${I.count}</b> 个分区：<code>${esc(I.first)}</code> ~ <code>${esc(I.last)}</code>，最后边界 ${esc(I.lastBound)}：</div>${p.stmts.map(stmtHtml).join('')}` : !p.error ? notice('<b>前置检查未通过，不能初始化，也不会发出任何 ALTER。</b>', 'bad', '⛔') : ''}${ptmTraceHtml(c, p)}`;
    }
    if (p.error) return `${notice(`${esc(p.error)}${p.code ? `<span class="small muted">（${esc(p.code)}）</span>` : ''}`, 'bad', '⛔')}${ptmTraceHtml(c, p)}`;
    if (!p.stmts.length) return `${tzLine(c, p) ? `<div class="small muted">${tzLine(c, p)}</div>` : ''}${warn}${notice('<b>已是最新</b>，本次不会执行任何语句。', 'ok', '✓')}${exist}${ptmTraceHtml(c, p)}`;
    return `${tzLine(c, p) ? `<div class="small muted">${tzLine(c, p)}</div>` : ''}${warn}<div class="small" style="margin:4px 0">将创建 <b>${p.adds.length}</b> 个分区${p.drops.length ? `，删除 <b style="color:var(--bad)">${p.drops.length}</b> 个过期分区（${p.drops.map(x => esc(x.name)).join('、')}）` : ''}${p.reorg ? '；表里有 pmax，创建改用 REORGANIZE PARTITION（会重写 pmax 的数据）' : ''}：</div>${p.stmts.length > 12 ? p.stmts.slice(0, 6).map(stmtHtml).join('') + `<div class="small muted">… 省略 ${p.stmts.length - 12} 条 …</div>` + p.stmts.slice(-6).map(stmtHtml).join('') : p.stmts.map(stmtHtml).join('')}${exist}${ptmTraceHtml(c, p)}`;
  }
  let PTM_BASE = null;
  function ptmOpenForm(base) {
    PTM_BASE = base;
    const edit = !!base.id;
    openModal({title: edit ? `编辑分区配置 #${base.id}` : '新增分区配置', wide: true, code: edit ? 'partman.edit' : 'partman.add', ok: '保存', body: ptmFormHtml(base),
      onOk() {
        const {c, errs, warns} = ptmRead(base);
        if (errs.length) { toast(errs[0]); return false; }
        if (edit) { Object.assign(base, c, {parts: base.parts}); audit('partman.edit', `编辑分区配置 #${base.id} ${c.db}.${c.tbl}（${c.span} ${periodName(c.period)} / 保留 ${c.retain} / 预创建 ${c.pre} / 时区 ${c.tz || '跟随目标库'}）`, '成功', instOf(c.inst).name); }
        else { c.id = ++PM_ID; c.created = TODAY; PM_CFG.push(c); audit('partman.add', `新增分区配置 #${c.id} ${c.db}.${c.tbl}（${c.span} ${periodName(c.period)} / 保留 ${c.retain} / 预创建 ${c.pre} / 时区 ${c.tz || '跟随目标库'}）`, '成功', instOf(c.inst).name); }
        toast((edit ? '配置已保存' : '配置已新增') + (warns.length ? '；提示：' + warns.join('；') : '')); rerender();
      }});
    $('#ptmInst').value = base.inst || $('#ptmInst').value;
    ptmFillDb(base.db || '');
    if (base.tbl) { ptmFillTbl(base.tbl, base.col); }
    ptmTzHint();
  }
  ACTIONS['partman.add'] = () => { if (!pmInsts().length) { toast('没有可配置的实例（需要主库的 MySQL / SelectDB / Doris 实例）'); return; } ptmOpenForm({}); };
  ACTIONS['partman.edit'] = d => {
    if (d.op === 'webhook') { whOpen(); return; }
    const c = pmGet(d.id); if (!c) return;
    if (d.tg) { c.enabled = !c.enabled; audit('partman.edit', `${c.enabled ? '启用' : '停用'}分区配置 #${c.id} ${c.db}.${c.tbl} 的自动维护`, '成功', instOf(c.inst).name); toast(c.enabled ? '已启用自动维护' : '已停用自动维护'); rerender(); return; }
    if (c.unsupported || !pmDialect(instOf(c.inst))) { toast('该配置所属实例类型暂不支持编辑'); return; }
    ptmOpenForm(c);
  };

  /* ---------- 预览并执行 ---------- */
  let PTM_RUN = null;                                    // {c, st, plan, shot}
  const INIT_MODES = [['current', '从当前窗口开始（默认）'], ['earliest', '从最早数据所在窗口开始'], ['date', '指定起始日期']];
  function ptmInitPick(c, st) {
    return `<div class="card" style="padding:10px;margin:6px 0"><div class="small" style="margin-bottom:4px"><b>初始化起点</b></div>
      ${INIT_MODES.map(([k, t]) => `<label class="chk" style="display:block"><input type="radio" name="ptmInitMode" value="${k}" data-onchange="ptmInitMode" ${st.mode === k ? 'checked' : ''}> ${esc(t)}</label>`).join('')}
      <div style="margin:4px 0 0 22px"><input id="ptmInitDate" placeholder="YYYY-MM-DD" value="${esc(st.date || '')}" data-onchange="ptmInitDate" ${st.mode === 'date' ? '' : 'disabled'} style="min-width:240px"></div></div>`;
  }
  function ptmRunBody(c, p, rejected, st) {
    const needDrop = p.drops && p.drops.length > 0, isInit = p.kind === 'init', canExec = isInit ? p.init.canExec && !p.error : !p.error;
    const needDel = needDrop || isInit;
    return `${rejected ? notice(`<b>服务端重新核对：计划已变化，本次执行被拒绝（409 ${isInit && p.init && !p.init.canExec ? 'init_precheck_failed' : 'confirm_required'}），没有改动任何分区。</b><br>${rejected}以下是最新的计划，请重新确认后再执行。`, 'bad', '⛔') : ''}
      <div class="small muted" style="margin-bottom:6px"><code>${esc(instOf(c.inst).name)}</code>（${esc(instOf(c.inst).type)}）· <code>${esc(c.db)}.${esc(c.tbl)}</code> · 分区键 ${esc(c.col)}（${c.ctype}）· 每 ${c.span} ${periodName(c.period)} · 保留 ${c.retain || '全部'} · 预创建 ${c.pre}</div>
      ${isInit ? ptmInitPick(c, st || {mode: p.init.mode, date: p.init.date}) : ''}
      ${ptmPlanHtml(c, p)}
      ${needDrop ? `<div class="notice bad" style="margin-top:8px"><span>⚠</span><div><b>DROP PARTITION 会永久删除分区内的数据，且不可恢复。</b>${can('partman.delete') ? '' : '<br>含删除分区的计划需要额外的「删除配置」权限（partman.delete），你没有该权限，无法执行。'}</div></div>` : ''}
      ${isInit && canExec ? `<div class="notice bad" style="margin-top:8px"><span>⚠</span><div><b>初始化为分区表不可撤销，整表重建期间不可写，请在业务低峰执行。</b>${can('partman.delete') ? '' : '<br>初始化需要额外的「删除配置」权限（partman.delete），你没有该权限，无法执行。'}</div></div>` : ''}
      ${needDel && canExec ? `<div class="form" style="margin-top:6px"><label class="req">确认输入</label><input id="ptmConfirm" placeholder="请输入 ${esc(c.db)}.${esc(c.tbl)} 确认" autocomplete="off"></div>` : ''}
      ${p.reorg ? `<label class="chk" style="margin-top:8px"><input type="checkbox" id="ptmReorg"> 确认执行 REORGANIZE（会重写 pmax 分区的数据，耗时较长）</label>` : ''}
      ${needDel && canExec ? `<div style="margin-top:10px"><label class="chk small muted"><input type="checkbox" id="ptmSim"> 演示：模拟预览之后表被其他人修改</label></div>` : ''}`;
  }
  const sameDrops = (a, b) => a.join() === b.join();
  ACTIONS['partman.run'] = d => {
    const c = pmGet(d.id); if (!c) return;
    const st = {mode: 'current', date: ''};
    let plan = ptmPlan(c, st);
    const isInit0 = plan.kind === 'init';
    if (isInit0 ? !plan.init.canExec : (plan.error || !plan.stmts.length)) {
      openModal({title: `预览 · ${c.db}.${c.tbl}`, ok: '', cancel: '关闭', code: 'partman.run', wide: true, body: ptmPlanHtml(c, plan)}); return;
    }
    PTM_RUN = {c, st, plan, shot: ptmShot(plan)};
    openModal({title: `${isInit0 ? '初始化为分区表' : '预览并执行'} · ${c.db}.${c.tbl}`, wide: true, code: 'partman.run', ok: isInit0 ? '确认初始化' : '确认执行', okCls: (plan.drops && plan.drops.length) || isInit0 ? 'danger primary' : 'primary', body: ptmRunBody(c, plan, '', st),
      onOk() { return ptmRunOk(); }});
  };
  function ptmShot(p) { return p.kind === 'init' ? {init: true, confirm: p.init.confirm, ok: p.init.canExec && !p.error} : {drops: p.drops.map(x => x.name), reorg: p.reorg}; }
  function ptmReplan(rejected) {
    const R = PTM_RUN, typed = ($('#ptmConfirm') || {}).value || '';
    R.plan = ptmPlan(R.c, R.st); R.shot = ptmShot(R.plan);
    $('#modal .mbody').innerHTML = ptmRunBody(R.c, R.plan, rejected || '', R.st);
    if ($('#ptmConfirm')) $('#ptmConfirm').value = typed;
  }
  ON.ptmInitMode = (d, el) => { if (!PTM_RUN) return; PTM_RUN.st.mode = el.value; ptmReplan(''); };
  ON.ptmInitDate = (d, el) => { if (!PTM_RUN) return; PTM_RUN.st.date = el.value.trim(); ptmReplan(''); };
  function ptmRunOk() {
    const R = PTM_RUN, c = R.c, st = R.st;
    if (!c.enabled) { toast('409 disabled：该配置的维护开关是关闭的，请先在编辑里开启'); return false; }
    const isInit = R.plan.kind === 'init';
    if (isInit) {
      if (R.plan.error || !R.shot.ok) { toast(R.plan.error ? R.plan.error : '前置检查未通过，不能初始化'); return false; }
      if (!can('partman.delete')) { toast('403 forbidden：初始化与 DROP 一样需要额外的 partman.delete 权限'); return false; }
      if (($('#ptmConfirm').value || '').trim() !== `${c.db}.${c.tbl}`) { toast('输入不匹配，请输入 库.表 确认'); return false; }
    } else {
      if (R.shot.drops.length) {
        if (!can('partman.delete')) { toast('含 DROP PARTITION 需要额外的 partman.delete 权限'); return false; }
        if (($('#ptmConfirm').value || '').trim() !== `${c.db}.${c.tbl}`) { toast('输入不匹配，请输入 库.表 确认'); return false; }
      }
      if (R.shot.reorg && !$('#ptmReorg').checked) { toast('请先勾选确认 REORGANIZE'); return false; }
    }
    if ($('#ptmSim') && $('#ptmSim').checked) {          // 演示：别人在预览之后改了表
      if (isInit) c.parts = seedParts(c.period, c.span, 3, c.pre, false);                // 有人先把它初始化成了分区表
      else if (c.parts && R.shot.drops.length) { const i = c.parts.findIndex(p => p.name === R.shot.drops[0]); if (i >= 0) c.parts.splice(i, 1); }   // 先把最老的分区删掉了
    }
    const fresh = ptmPlan(c, st);
    let why = '';
    if (isInit) {
      if (fresh.kind !== 'init') why = `预览时表尚未分区；重新读取后表已经是分区表（${c.parts ? c.parts.length : 0} 个分区），本次不再初始化。`;
      else if (fresh.error || !fresh.init.canExec) why = `重新核对后前置检查不通过（${fresh.error || (fresh.init.checks.find(x => x.level === 'error') || {}).message}）。`;
      else if (JSON.stringify(fresh.init.confirm) !== JSON.stringify(R.shot.confirm)) why = `预览时 ${R.shot.confirm.partition_count} 个分区（${R.shot.confirm.first_partition} ~ ${R.shot.confirm.last_partition}），重新核对后 ${fresh.init.confirm.partition_count} 个（${fresh.init.confirm.first_partition} ~ ${fresh.init.confirm.last_partition}）。`;
    } else {
      const fd = fresh.error ? [] : fresh.drops.map(p => p.name);
      if (fresh.error || fresh.kind === 'init' || !sameDrops(fd, R.shot.drops) || fresh.reorg !== R.shot.reorg) why = `预览时的删除列表：${R.shot.drops.length ? esc(R.shot.drops.join('、')) : '（无）'}；重新核对后：${fd.length ? esc(fd.join('、')) : '（无）'}。`;
    }
    if (why) {
      audit('partman.run', `分区维护被拒绝（计划已变化）${c.db}.${c.tbl}：${why.replace(/<[^>]+>/g, '')}`, '拒绝', instOf(c.inst).name);
      if (!isInit && fresh.error) { $('#modal .mbody').innerHTML = notice(esc(fresh.error), 'bad', '⛔'); return false; }
      R.plan = fresh; R.shot = ptmShot(fresh);
      $('#modal .mbody').innerHTML = ptmRunBody(c, fresh, why + '<br>', st);
      toast('计划已变化，请重新确认'); return false;
    }
    if (!fresh.stmts.length) { toast('没有需要执行的语句'); return true; }
    const nBefore = c.parts ? c.parts.length : 0, was = !!c.parts;
    const stmts = fresh.stmts.map(s => ({sql: s.short || s.sql, st: 'ok', ms: s.kind === 'init' ? 4800 + (hashOf(s.sql) % 5200) : s.kind === 'reorg' ? 2200 + (hashOf(s.sql) % 900) : 90 + (hashOf(s.sql) % 400)}));
    const at = qHostNow().slice(0, 16) + ':00', whOn = PM_WH.filter(w => w.isenabled).length;
    if (isInit) audit('partman.run', `分区维护-执行初始化 ${c.db}.${c.tbl}：起点 ${fresh.init.start}（${fresh.init.mode}），${fresh.init.count} 个分区 ${fresh.init.first} ~ ${fresh.init.last}；语句：${fresh.stmts[0].sql}`, '成功', instOf(c.inst).name);
    else audit('partman.run', `手工分区维护 ${c.db}.${c.tbl}：创建 ${fresh.adds.length} 个${fresh.drops.length ? '、删除 ' + fresh.drops.map(p => p.name).join('/') : ''}${fresh.reorg ? '（REORGANIZE）' : ''}`, '成功', instOf(c.inst).name);
    const rec = {id: ++PM_LID, cfg: c.id, at, source: '手工', operator: ME.login, status: '成功', stmts};
    rec.out = pmCap(pmRunTrace(c, {at, source: '手工', plan: fresh, stmts, nBefore, wasPartitioned: was}));
    ptmApply(c, fresh);
    PM_LOGS.unshift(rec);
    const pushTxt = PM_AUTO.notifyOn === 'never' ? '；推送：partman_notify_on=never，不推送' : PM_AUTO.notifyOn === 'on_failure' ? '；推送：partman_notify_on=on_failure，本次成功不推送' : whOn ? `；推送：后台向 ${whOn} 个启用通道发送（失败不影响维护结果）` : '；推送：没有启用的通道';
    toast(isInit ? `已初始化为分区表：${fresh.init.count} 个分区（${fresh.init.first} ~ ${fresh.init.last}），已写入维护日志${pushTxt}` : `已执行 ${stmts.length} 条语句，已写入维护日志${pushTxt}`); rerender();
    return true;
  }

  ACTIONS['partman.delete'] = d => {
    const c = pmGet(d.id); if (!c) return;
    const n = PM_LOGS.filter(l => l.cfg === c.id).length;
    risky('partman.delete', '删除分区维护配置', `将删除配置 #${c.id}（<b>${esc(c.db)}.${esc(c.tbl)}</b>，${esc(instOf(c.inst).name)}）及其 <b>${n}</b> 条维护日志。<br>目标表上已有的分区<b>不会</b>被改动，但之后不再自动续建 / 清理。`, null, () => {
      PM_CFG.splice(PM_CFG.indexOf(c), 1); for (let i = PM_LOGS.length - 1; i >= 0; i--) if (PM_LOGS[i].cfg === c.id) PM_LOGS.splice(i, 1);
      audit('partman.delete', `删除分区配置 #${c.id} ${c.db}.${c.tbl}（连同 ${n} 条日志）`, '成功', instOf(c.inst).name); toast('配置已删除'); rerender();
    }, `${c.db}.${c.tbl}`);
  };

  /* ============================================================================
     推送设置（dbt_partman_webhook）：列表 / 新增 / 修改 / 删除 / 启停 / 测试发送；权限沿用 partman.edit（change_partmanconfig）
     webhook_url 只显示脱敏形式，secret 只显示“已设置”，写操作的审计里没有完整地址与密钥（契约 §5）
     ========================================================================== */
  const WH_TYPES = {
    slack: {name: 'Slack', hosts: ['hooks.slack.com'], ok: '正文 ok', secret: false, keyword: false, ment: 'Slack 的 <!subteam^…>、<@U…>'},
    wecom: {name: '企业微信', hosts: ['qyapi.weixin.qq.com'], ok: 'errcode = 0', secret: false, keyword: true, ment: '手机号 / userid / @all'},
    feishu: {name: '飞书', hosts: ['open.feishu.cn', 'open.larksuite.com'], ok: 'code = 0（旧版 StatusCode = 0）', secret: true, keyword: true, ment: 'user_id / open_id / all'},
    dingtalk: {name: '钉钉', hosts: ['oapi.dingtalk.com'], ok: 'errcode = 0', secret: true, keyword: true, ment: '手机号 / userid / all'},
  };
  const WH_MENTION_ON = [['always', '总是 @'], ['on_failure', '仅失败时 @（默认）'], ['never', '从不 @']];
  const PM_WH = [
    {id: 1, type: 'dingtalk', url: 'https://oapi.dingtalk.com/robot/send?access_token=3f9c1a7e5b2d4c60a8f1b7d2c9e4a613', isenabled: true, mention_on: 'on_failure', mention: ['13800000000', 'all'], keyword: '分区', secret: 'SEC9d1f…（演示）', sim: 'ok', created: '2026-09-02 10:12:44'},
    {id: 2, type: 'wecom', url: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=7a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d', isenabled: true, mention_on: 'always', mention: ['@all'], keyword: '', secret: '', sim: 'ok', created: '2026-09-02 10:15:20'},
    {id: 3, type: 'feishu', url: 'https://open.feishu.cn/open-apis/bot/v2/hook/9d8e7f6a-5b4c-3d2e-1f0a-b9c8d7e6f5a4', isenabled: false, mention_on: 'never', mention: [], keyword: 'partman', secret: 'FSK3c7a…（演示）', sim: 'sign', created: '2026-09-10 16:03:09'},
    {id: 4, type: 'slack', url: 'https://hooks.slack.com/services/T0A1B2C3D/B0E4F5G6H/Xk29sLw0pQ7vNn31', isenabled: true, mention_on: 'on_failure', mention: ['<!subteam^S0123ABC>'], keyword: '', secret: '', sim: 'timeout', created: '2026-09-18 09:40:31'},
  ];
  let WH_ID = 4;
  /* 与原程序 maskWebhook 一致：密钥段替换成 **** */
  function maskUrl(u) { for (const sep of ['key=', 'access_token=', '/hook/', '/services/']) { const i = u.indexOf(sep); if (i >= 0) return u.slice(0, i + sep.length) + '****'; } return u; }
  /* 地址校验（契约 §5.1）：http / https 完整地址；主机在白名单内且为 https；本机回环（localhost / 127.0.0.1 / ::1）仅为联调保留 */
  function whCheckUrl(type, url) {
    let u; try { u = new URL(url); } catch (e) { return {code: 'bad_request', msg: 'webhook_url 必须是 http:// 或 https:// 开头的完整地址'}; }
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return {code: 'bad_request', msg: 'webhook_url 必须是 http:// 或 https:// 开头的完整地址'};
    const h = u.hostname.replace(/^\[|\]$/g, '');
    if (['localhost', '127.0.0.1', '::1'].includes(h)) return null;
    if (u.protocol !== 'https:' || !WH_TYPES[type].hosts.includes(h)) return {code: 'webhook_host_not_allowed', msg: `${WH_TYPES[type].name} 通道的地址主机必须是 ${WH_TYPES[type].hosts.join(' / ')}（https）；当前是 ${u.protocol}//${h}`};
    return null;
  }
  /* 测试发送 / 推送的模拟结果：只返回脱敏信息，不带地址与密钥 */
  function pmWhSend(w) {
    const okResp = {slack: 'ok', wecom: '{"errcode":0,"errmsg":"ok"}', dingtalk: '{"errcode":0,"errmsg":"ok"}', feishu: '{"code":0,"msg":"success"}'}[w.type];
    if (w.sim === 'timeout') return {ok: false, status: 0, message: '请求失败：连接超时', resp: ''};
    if (w.sim === 'sign') return {ok: false, status: 200, message: '平台拒绝：code=19021 sign match fail or timestamp is not within one hour from current time', resp: '{"code":19021,"msg":"sign match fail or timestamp is not within one hour from current time"}'};
    if (w.sim === 'keyword') return {ok: false, status: 200, message: '平台拒绝：errcode=310000 keywords not in content', resp: '{"errcode":310000,"errmsg":"keywords not in content"}'};
    return {ok: true, status: 200, message: '发送成功', resp: okResp};
  }
  const whRow = w => `<tr><td>${w.id}</td><td>${tag(WH_TYPES[w.type].name, 'info')}</td><td class="mono small" style="word-break:break-all;max-width:260px">${esc(maskUrl(w.url))}</td>
    <td>${w.keyword ? esc(w.keyword) : '<span class="muted">—</span>'}</td><td>${w.secret ? tag('已设置', 'ok') : '<span class="muted">—</span>'}</td>
    <td class="small">${esc((WH_MENTION_ON.find(x => x[0] === w.mention_on) || [0, w.mention_on])[1])}<div class="muted">${w.mention.length ? esc(w.mention.join('、')) : '—'}</div></td>
    <td><label class="sw"><input type="checkbox" ${w.isenabled ? 'checked' : ''} data-onchange="whToggle" data-id="${w.id}"><i></i></label></td>
    <td><div class="ops"><button class="btn sm" data-on="whTest" data-id="${w.id}">测试发送</button><button class="btn sm" data-on="whEdit" data-id="${w.id}">编辑</button><button class="btn sm danger" data-on="whDel" data-id="${w.id}">删除</button></div></td></tr>`;
  let WH_RES = null;
  function whBody() {
    return `${notice(`全局推送时机：<b>${esc(PM_AUTO.notifyOn)}</b>`, '', 'ℹ')}
      <div class="toolbar"><button class="btn primary sm" data-on="whAdd">＋ 新增通道</button><button class="btn sm" data-on="whTest" data-id="0">测试发送（全部已启用）</button><span class="sp"></span><span class="small muted">共 ${PM_WH.length} 个通道，${PM_WH.filter(w => w.isenabled).length} 个启用</span></div>
      <div class="tbl-wrap"><table><thead><tr><th>#</th><th>类型</th><th>地址（脱敏）</th><th>关键词</th><th>密钥</th><th>@ 规则 / 对象</th><th>启用</th><th>操作</th></tr></thead><tbody>${PM_WH.map(whRow).join('') || `<tr><td colspan="8">${empty('还没有推送通道', '✉')}</td></tr>`}</tbody></table></div>
      <div id="whRes">${WH_RES || ''}</div>`;
  }
  function whOpen() {
    openModal({title: '推送设置 · 分区维护', wide: true, code: 'partman.edit', ok: '', cancel: '关闭', body: whBody()});
  }
  const whRefresh = () => { const m = $('#modal .mbody'); if (m && $('#modal header h3').textContent.startsWith('推送设置')) m.innerHTML = whBody(); };
  function whForm(w) {
    const e = !!w, t = e ? w.type : 'dingtalk', T = WH_TYPES[t];
    return `<div class="pq-form2">
      <div><label class="req">通道类型</label><select id="whType" data-onchange="whType">${Object.entries(WH_TYPES).map(([k, v]) => opt(k, `${v.name}（${k}）`, t)).join('')}</select></div>
      <div><label>启用</label><label class="sw"><input type="checkbox" id="whEn" ${!e || w.isenabled ? 'checked' : ''}><i></i>启用</label></div>
      <div style="grid-column:1/-1"><label ${e ? '' : 'class="req"'}>webhook 地址${e ? `（不改请留空；当前 <span class="mono">${esc(maskUrl(w.url))}</span>）` : ''}</label><input id="whUrl" class="mono" autocomplete="off" placeholder="${esc(T.hosts.length ? 'https://' + T.hosts[0] + '/…' : '')}" maxlength="500"></div>
      <div><label>@ 时机</label><select id="whMentionOn">${WH_MENTION_ON.map(([k, v]) => opt(k, v, e ? w.mention_on : 'on_failure')).join('')}</select></div>
      <div><label>@ 对象（逗号 / 空格分隔）</label><input id="whMention" value="${esc(e ? w.mention.join(', ') : '')}" placeholder="${esc(T.ment)}" maxlength="500"></div>
      <div><label>自定义关键词</label><input id="whKeyword" value="${esc(e ? w.keyword : '')}" maxlength="500" ${T.keyword ? '' : 'disabled'} placeholder="${T.keyword ? '机器人开启了关键词校验时填写' : 'Slack 不使用'}"></div>
      <div><label>加签密钥</label><input id="whSecret" type="password" autocomplete="new-password" maxlength="500" ${T.secret ? '' : 'disabled'} placeholder="${e && w.secret ? '已设置，不改请留空' : T.secret ? '机器人开启了加签时填写' : '该类型不使用'}">
        ${e && w.secret ? `<label class="chk small" style="margin-top:4px"><input type="checkbox" id="whClear"> 清空已设置的密钥</label>` : ''}</div>
    </div>`;
  }
  function whRead() {
    const tp = $('#whType').value;
    return {type: tp, url: $('#whUrl').value.trim(), isenabled: $('#whEn').checked, mention_on: $('#whMentionOn').value, mention: $('#whMention').value.split(/[,，\s]+/).map(s => s.trim()).filter(Boolean), keyword: WH_TYPES[tp].keyword ? $('#whKeyword').value.trim() : '', secret: WH_TYPES[tp].secret ? $('#whSecret').value : '', clear: !!($('#whClear') && $('#whClear').checked)};
  }
  function whOpenForm(w) {
    openModal({title: w ? `编辑推送通道 #${w.id}` : '新增推送通道', wide: true, code: 'partman.edit', ok: '保存', body: whForm(w),
      onOk() {
        const o = whRead(), T = WH_TYPES[o.type];
        if (!w && !o.url) { toast('webhook 地址必填'); return false; }
        if (o.url) {
          if (o.url.length > 500) { toast('400 bad_request：webhook_url 不能超过 500 个字符'); return false; }
          const er = whCheckUrl(o.type, o.url); if (er) { toast(`${er.code === 'webhook_host_not_allowed' ? '400 webhook_host_not_allowed：' : '400 bad_request：'}${er.msg}`); return false; }
          if (PM_WH.some(x => x.url === o.url && (!w || x.id !== w.id))) { toast('409 duplicate：该地址已存在'); return false; }
        } else if (w) {
          const er = whCheckUrl(o.type, w.url); if (er) { toast(`400 webhook_host_not_allowed：改类型后原地址不符合 ${T.name} 的主机白名单，请同时填写新地址`); return false; }
        }
        if (JSON.stringify(o.mention).length > 500) { toast('400 bad_request：@ 对象过长（序列化后不超过 500 个字符）'); return false; }
        if (w) {
          Object.assign(w, {type: o.type, isenabled: o.isenabled, mention_on: o.mention_on, mention: o.mention, keyword: o.keyword});
          if (o.url) w.url = o.url;
          if (o.clear) w.secret = ''; else if (o.secret) w.secret = '已设置';
          if (!T.secret) w.secret = '';
          audit('partman.edit', `分区维护-修改推送通道 #${w.id} ${T.name} ${maskUrl(w.url)}（${w.isenabled ? '启用' : '停用'} / mention_on=${w.mention_on}${o.url ? ' / 地址已更新' : ''}${o.clear ? ' / 密钥已清空' : o.secret ? ' / 密钥已更新' : ''}）`, '成功');
          toast('推送通道已保存');
        } else {
          const n = {id: ++WH_ID, type: o.type, url: o.url, isenabled: o.isenabled, mention_on: o.mention_on, mention: o.mention, keyword: o.keyword, secret: o.secret ? '已设置' : '', sim: 'ok', created: qHostNow().slice(0, 19)};
          PM_WH.push(n);
          audit('partman.edit', `分区维护-新增推送通道 #${n.id} ${T.name} ${maskUrl(n.url)}（${n.isenabled ? '启用' : '停用'} / mention_on=${n.mention_on}${n.secret ? ' / 已设置密钥' : ''}）`, '成功');
          toast('推送通道已新增');
        }
        WH_RES = null; setTimeout(whOpen, 0); return true;
      }});
  }
  ON.whType = () => {
    const w = (() => { const t = $('#modal header h3').textContent, m = /#(\d+)/.exec(t); return m ? PM_WH.find(x => x.id === +m[1]) : null; })(), tp = $('#whType').value, T = WH_TYPES[tp];
    $('#whKeyword').disabled = !T.keyword; $('#whSecret').disabled = !T.secret; $('#whMention').placeholder = T.ment;
    $('#whUrl').placeholder = 'https://' + T.hosts[0] + '/…';
    if (w && w.secret && !T.secret) toast('该类型不使用加签密钥，保存后原密钥会被清空');
  };
  ON.whAdd = () => whOpenForm(null);
  ON.whEdit = d => { const w = PM_WH.find(x => x.id == d.id); if (w) whOpenForm(w); };
  ON.whDel = d => {
    const w = PM_WH.find(x => x.id == d.id); if (!w) return;
    risky('partman.edit', '删除推送通道', `将删除推送通道 #${w.id}（${esc(WH_TYPES[w.type].name)}，<span class="mono">${esc(maskUrl(w.url))}</span>）。之后维护结果不再推送到该通道。`, null, () => {
      PM_WH.splice(PM_WH.indexOf(w), 1); audit('partman.edit', `分区维护-删除推送通道 #${w.id} ${WH_TYPES[w.type].name} ${maskUrl(w.url)}`, '成功'); toast('推送通道已删除'); WH_RES = null; setTimeout(whOpen, 0);
    });
  };
  ON.whToggle = (d, el) => {
    const w = PM_WH.find(x => x.id == d.id); if (!w) return;
    if (w.isenabled === el.checked) return;
    w.isenabled = el.checked; audit('partman.edit', `分区维护-${w.isenabled ? '启用' : '停用'}推送通道 #${w.id} ${WH_TYPES[w.type].name} ${maskUrl(w.url)}`, '成功'); toast(w.isenabled ? '已启用推送通道' : '已停用推送通道'); whRefresh();
  };
  ON.whTest = d => {
    const id = +d.id, list = id ? PM_WH.filter(x => x.id === id) : PM_WH.filter(x => x.isenabled);
    if (!list.length) { WH_RES = notice('<b>409 no_channel</b>：没有可发送的通道（没有已启用的通道）。', 'warn', '⚠'); whRefresh(); return; }
    const at = qHostNow().slice(0, 19);
    const rows = list.map(w => { const r = pmWhSend(w), ment = w.mention.length && w.mention_on === 'always' ? `@ ${w.mention.join(' ')}` : '不 @'; return {w, r, ment}; });
    audit('partman.edit', `分区维护-测试发送推送：${rows.map(x => `#${x.w.id} ${x.w.type} ${x.r.ok ? '成功' : '失败'}`).join('、')}`, rows.every(x => x.r.ok) ? '成功' : '部分失败');
    WH_RES = `<div class="card" style="margin-top:10px"><div class="small" style="margin-bottom:6px"><b>测试发送结果</b>（${esc(at)}）· 标题 <code>分区维护推送测试  ${esc(at)}</code>；正文“这是一条来自 dbtool 分区维护的测试消息，收到即表示该通道可用。”</div>
      <table><thead><tr><th>#</th><th>类型</th><th>地址（脱敏）</th><th>结果</th><th>状态码</th><th>说明</th><th>@</th></tr></thead><tbody>${rows.map(({w, r, ment}) => `<tr><td>${w.id}</td><td>${esc(WH_TYPES[w.type].name)}</td><td class="mono small" style="word-break:break-all">${esc(maskUrl(w.url))}</td><td>${r.ok ? tag('成功', 'ok') : tag('失败', 'bad')}</td><td>${r.status || '—'}</td><td class="small">${esc(clip(r.message, 200))}${w.isenabled ? '' : '<div class="muted">（该通道未启用，仅测试）</div>'}</td><td class="small muted">${esc(ment)}</td></tr>`).join('')}</tbody></table></div>`;
    whRefresh();
  };

})();
