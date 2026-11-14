/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_sql.js
 * @time: 2026/10/28
 */
/* ============================================================================
   SQL 上线（pages.sqlworkflow：列表 / 提交 / 详情）与 SQL 分析（pages.sqlanalyze）
   规格：docs/user-guide.md、internal/sqlflow、internal/sqlopt
   导出：window.WORKFLOWS / wfPendingFor(user) / wfPendingList(user)；设置 NAV.sqlworkflow.badge
   ========================================================================== */
(function () {
  'use strict';

  /* ---------- 状态名（与后端 workflow_* 状态一一对应） ---------- */
  const ST = {wait: '等待审批', pass: '审核通过', timing: '定时执行', run: '执行中', done: '已正常结束', exc: '执行有异常', abort: '人工终止流程', auto: '自动审批不通过'};
  const ST_LIST = [ST.wait, ST.pass, ST.timing, ST.run, ST.done, ST.exc, ST.abort, ST.auto];
  const ST_TAG = {[ST.wait]: 'warn', [ST.pass]: 'info', [ST.timing]: 'brand', [ST.run]: 'warn', [ST.done]: 'ok', [ST.exc]: 'bad', [ST.abort]: '', [ST.auto]: 'bad'};
  const ST_ICO = {[ST.wait]: '⏳', [ST.pass]: '✔', [ST.timing]: '⏱', [ST.run]: '⟳', [ST.done]: '✓', [ST.exc]: '!', [ST.abort]: '■', [ST.auto]: '✕'};
  const stHtml = s => `<span class="tag ${ST_TAG[s]}">${ST_ICO[s]} ${s}</span>`;

  /* 审批流：资源组 → 每一级的权限组（与「审批流程配置」页一致：订单 / 支付 组长 → DBA，其余仅 DBA） */
  const FLOW = {1: ['g_tl', 'g_dba'], 2: ['g_tl', 'g_dba'], 3: ['g_dba'], 4: ['g_dba'], 5: ['g_dba']};
  const LVNAME = {g_tl: '组长', g_dba: 'DBA'};
  const flowOf = rg => FLOW[rg] || ['g_dba'];

  /* 批量上线需要多台 MsSQL：data.js 里只有一台，这里补几台虚拟实例（id ≥ 61）；64 没有 ReportDW 库，用来演示预检失败 */
  const EXTRA_MSSQL = [61, 62, 63, 64].map((id, k) => ({id, name: k < 3 ? 'mssql-report-prod-0' + (k + 2) : 'mssql-shop-prod-01', type: 'MsSQL', ver: '2019 (15.0.4153)', role: 'master', host: '10.20.4.' + (32 + k), port: 1433, env: '生产', rgs: [4], tags: ['批量上线'], schema: 'mssql', dbs: k === 3 ? ['ShopDW'] : null, status: 'ok', enabled: true, flaky: id === 62}));
  const wfInst = id => INSTANCES.find(i => i.id == id) || EXTRA_MSSQL.find(i => i.id == id) || instOf(id);
  const dbList = inst => SCHEMAS[inst.schema] || [];
  const isRds = inst => inst.type === 'Redis';
  const REDIS_DBS = Array.from({length: 16}, (_, i) => String(i));   // Redis 的库是 db 编号（值是纯数字，显示成 db0、db1…）
  const dbNames = inst => isRds(inst) ? REDIS_DBS : inst.dbs || dbList(inst).map(d => d.db);
  const tblOf = (inst, db, n) => (((inst.dbs ? [] : dbList(inst)).find(d => d.db.toLowerCase() === String(db).toLowerCase()) || {}).tables || []).find(t => t.n.toLowerCase() === String(n).toLowerCase());
  const isMs = inst => inst.type === 'MsSQL';
  const hs = s => { let h = 2166136261; for (const c of String(s)) { h ^= c.charCodeAt(0); h = Math.imul(h, 16777619); } return h >>> 0; };
  const wfNow = () => TODAY + ' ' + now().slice(11);
  const uName = login => { const u = USERS.find(x => x.login === login); return u ? `${esc(u.login)}（${esc(u.name)}）` : esc(login); };

  /* ============================================================================
     模拟 goInception 检查（词法级规则；真实系统由 goInception + 站点规则完成）
     ========================================================================== */
  /* 拆分语句：忽略注释、字符串里的分号；记录起始行号；MsSQL 把独占一行的 GO 当分隔符 */
  function splitSql(text, ms) {
    if (ms) text = text.replace(/^[ \t]*GO[ \t]*$/gim, ';');
    const out = []; let cur = '', i = 0, q = null, line = 1, start = null;
    const push = () => { const t = cur.trim(); if (t) out.push({sql: t, line: start || 1}); cur = ''; start = null; };
    while (i < text.length) {
      const c = text[i], n = text[i + 1];
      if (q) { cur += c; if (c === '\\' && q !== '`') { cur += n || ''; i += 2; continue; } if (c === q) q = null; if (c === '\n') line++; i++; continue; }
      if (c === '-' && n === '-' && (text[i + 2] === undefined || /\s/.test(text[i + 2]))) { while (i < text.length && text[i] !== '\n') i++; continue; }
      if (c === '/' && n === '*') { const e = text.indexOf('*/', i + 2), end = e < 0 ? text.length : e + 2; line += (text.slice(i, end).match(/\n/g) || []).length; i = end; cur += ' '; continue; }
      if (c === "'" || c === '"' || c === '`') { q = c; if (start == null) start = line; cur += c; i++; continue; }
      if (c === ';') { push(); i++; continue; }
      if (c === '\n') line++;
      if (start == null && !/\s/.test(c)) start = line;
      cur += c; i++;
    }
    const unterminated = !!q;
    push();
    return {items: out, unterminated};
  }
  function topSplit(s) {
    const out = []; let d = 0, q = null, cur = '';
    for (let i = 0; i < s.length; i++) {
      const c = s[i];
      if (q) { cur += c; if (c === '\\') cur += s[++i] || ''; else if (c === q) q = null; continue; }
      if (c === "'" || c === '"' || c === '`') { q = c; cur += c; continue; }
      if (c === '(') d++;
      if (c === ')') d--;
      if (c === ',' && d === 0) { out.push(cur.trim()); cur = ''; continue; }
      cur += c;
    }
    if (cur.trim()) out.push(cur.trim());
    return out;
  }
  const ID = '(?:`[^`]+`|\\[[^\\]]+\\]|[\\w$]+)';
  const NAME = '(' + ID + '(?:\\s*\\.\\s*' + ID + '){0,2})';
  const unq = s => s.trim().replace(/^[`\[]|[`\]]$/g, '');
  function parseName(raw, ms) {
    const p = raw.split('.').map(unq), t = p[p.length - 1];
    return {t, db: ms ? (p.length === 3 ? p[0] : '') : (p.length === 2 ? p[0] : '')};
  }
  const noStr = c => c.replace(/'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|`[^`]*`/g, "''");
  const bigTable = t => t && (t.rows >= 5e6 || t.size >= 2000);

  function whereInfo(meta, w) {
    const names = new Set((meta.cols || []).map(c => c.n.toLowerCase()));
    const cols = [], eq = [], rng = []; let inCount = 1, m;
    const re = /`?([A-Za-z_]\w*)`?\s*(<=>|>=|<=|<>|!=|=|>|<|\s+not\s+in\s*\(|\s+in\s*\(|\s+like\s|\s+between\s|\s+is\s)/gi;
    while ((m = re.exec(w))) {
      const c = m[1].toLowerCase(); if (!names.has(c)) continue;
      const op = m[2].trim().toLowerCase(); if (!cols.includes(c)) cols.push(c);
      if (op === '=' || op === '<=>' || /^is/.test(op)) eq.push(c);
      else if (/^not|<>|!=/.test(op)) { /* 取反：不走索引 */ }
      else if (/^in/.test(op)) { eq.push(c); const mm = w.slice(re.lastIndex).match(/^([^)]*)\)/); if (mm) inCount = Math.max(inCount, topSplit(mm[1]).length); }
      else rng.push(c);
    }
    const fm = w.match(/\b(date|year|month|day|hour|lower|upper|substr|substring|concat|ifnull|from_unixtime|unix_timestamp|cast|trim|left|right)\s*\(\s*`?([A-Za-z_]\w*)`?/i);
    return {cols, eq, rng, inCount, lead: /like\s+n?'%/i.test(w), fnCol: fm && names.has(fm[2].toLowerCase()) ? fm[2] : ''};
  }
  function estimate(meta, wi, has) {
    const rows = meta.rows || 0, eqs = new Set(wi.eq), idx = meta.idx || [];
    if (!has) return {rows, indexed: true};
    if (idx.some(x => x[2] && x[1].every(c => eqs.has(c.toLowerCase())))) return {rows: Math.min(rows, wi.inCount), indexed: true, unique: true};
    const lead = idx.find(x => wi.cols.includes(x[1][0].toLowerCase()));
    if (lead) return {rows: Math.max(1, Math.round(rows * (eqs.has(lead[1][0].toLowerCase()) ? 0.002 : 0.05))), indexed: true, key: lead[0]};
    return {rows, indexed: false};
  }

  const DDL_KW = ['create', 'alter', 'drop', 'truncate', 'rename'], DML_KW = ['insert', 'replace', 'update', 'delete'];
  const LV_RANK = {ok: 0, warn: 1, bad: 2};

  /* inst 为 wfInst() 返回的实例对象，db 为目标库。返回 {items, type, warn, err, osc} */
  function wfCheck(sql, inst, db) { if (isRds(inst)) return redisCheck(sql, inst, db);
    const ms = isMs(inst), sp = splitSql(sql, ms), items = [];
    const ctx = {inst, db, created: {}, lastAlter: {}};
    sp.items.forEach((s, idx) => {
      const it = {n: idx + 1, sql: s.sql, line: s.line, msgs: [], rows: 0, osc: false, kind: '', tbl: ''};
      it.add = (lvl, text, rule) => it.msgs.push({lvl, text, rule: rule || ''});
      if (inst.status === 'bad' && idx === 0) it.add('bad', `实例 ${inst.name} 当前连接异常，无法完成检查，请联系 DBA`, 'inst_unreachable');
      try { (ms ? checkMs : checkMy)(ctx, it); } catch (e) { it.add('warn', '检查器异常：' + e.message); }
      delete it.add; items.push(it);
    });
    if (sp.unterminated && items.length) items[items.length - 1].msgs.push({lvl: 'bad', text: '语法错误：引号未闭合', rule: 'syntax'});
    const ddl = items.some(i => DDL_KW.includes(i.kind)), dml = items.some(i => DML_KW.includes(i.kind));
    if (ddl && dml) items.forEach(i => { if (DML_KW.includes(i.kind)) i.msgs.push({lvl: 'bad', text: 'DDL 与 DML 不能混在同一个工单提交（站点规则 ddl_dml_separation），请拆成两个工单', rule: 'ddl_dml_separation'}); });
    items.forEach(i => {
      i.lvl = i.msgs.reduce((a, m) => LV_RANK[m.lvl] > LV_RANK[a] ? m.lvl : a, 'ok');
      i.stage = i.lvl === 'bad' ? '规则检查未通过' : i.lvl === 'warn' ? '规则检查有警告' : 'Audit completed';
      i.ddl = DDL_KW.includes(i.kind) || i.kind === 'delete';
    });
    const anyDdl = items.some(i => i.ddl);
    return {items, ddl: anyDdl, type: anyDdl ? 'DDL' : 'DML', warn: items.filter(i => i.lvl === 'warn').length, err: items.filter(i => i.lvl === 'bad').length, osc: items.some(i => i.osc)};
  }
  /* 跳过语法检查：只拆语句，不给任何建议 */
  function wfNoCheck(sql, inst) {
    const sp = splitSql(sql, isMs(inst));
    const items = sp.items.map((s, i) => ({n: i + 1, sql: s.sql, line: s.line, msgs: [], rows: 0, osc: false, kind: (s.sql.match(/^\w+/) || [''])[0].toLowerCase(), tbl: '', lvl: 'ok', stage: '未检查（跳过）', ddl: /^(create|alter|drop|truncate|rename|delete)/i.test(s.sql)}));
    const anyDdl = items.some(i => i.ddl);
    return {items, ddl: anyDdl, type: anyDdl ? 'DDL' : 'DML', warn: 0, err: 0, osc: false, skipped: true};
  }

  /* ---------- MySQL / MariaDB 规则 ---------- */
  function checkMy(ctx, it) {
    const {inst} = ctx, add = it.add;
    const c = it.sql.replace(/\s+/g, ' ').trim(), l = c.toLowerCase(), kw = (l.match(/^[a-z]+/) || [''])[0];
    it.kind = kw;
    const plain = noStr(c);
    if ((plain.match(/\(/g) || []).length !== (plain.match(/\)/g) || []).length) { add('bad', `语法错误：括号不匹配 near '${c.slice(-24)}'`, 'syntax'); return; }
    if (!/^(create|alter|drop|truncate|rename|insert|replace|update|delete|select|use|set|grant|revoke|show|explain|call|begin|commit|rollback|flush|optimize|analyze)$/.test(kw)) { add('bad', `语法错误：无法识别的语句 near '${c.slice(0, 20)}'`, 'syntax'); return; }
    const look = raw => {
      const {t, db} = parseName(raw, false); let d = ctx.db; it.tbl = t;
      if (db && db.toLowerCase() !== ctx.db.toLowerCase()) { add('warn', `语句操作了其他库 \`${db}\`，与工单目标库 \`${ctx.db}\` 不一致，请确认`, 'cross_db'); d = db; }
      const v = ctx.created[t.toLowerCase()];
      if (v) return {t, db: d, meta: v, virtual: true};
      if (!dbNames(inst).some(x => x.toLowerCase() === d.toLowerCase())) { add('bad', `数据库 \`${d}\` 不存在`, 'db_not_exists'); return {t, db: d, meta: null, noDb: true}; }
      return {t, db: d, meta: tblOf(inst, d, t)};
    };
    const needTbl = r => { if (!r.meta && !r.noDb) { add('bad', `表 \`${r.db}\`.\`${r.t}\` 不存在`, 'table_not_exists'); return false; } return !!r.meta; };
    const chkCols = (meta, names, virtual) => { if (virtual) return; const set = new Set((meta.cols || []).map(x => x.n.toLowerCase())); names.forEach(n => { if (!set.has(n.toLowerCase())) add('bad', `列 \`${n}\` 在表 \`${it.tbl}\` 中不存在`, 'column_not_exists'); }); };
    const dmlWhere = (r, w, label, lim) => {
      const meta = r.meta;
      if (!w) { add('bad', `${label} 没有 WHERE 条件，会修改整张表（约 ${fmtN(meta.rows || 0)} 行），请补充条件（规则 dml_must_where）`, 'dml_must_where'); it.rows = meta.rows || 0; return; }
      const wi = whereInfo(meta, w), est = estimate(meta, wi, true);
      let rows = est.rows; if (lim) rows = Math.min(rows, lim);
      it.rows = rows;
      if (wi.fnCol) add('warn', `WHERE 对列 \`${wi.fnCol}\` 使用了函数，会使索引失效，请改写为范围条件`, 'where_func');
      if (wi.lead) add('warn', 'LIKE 以 % 开头，无法使用索引', 'like_leading');
      if (!est.indexed && (meta.rows || 0) >= 10000) add('warn', `WHERE 条件列${wi.cols.length ? ' `' + wi.cols.join('`, `') + '` ' : ''}没有可用索引，预计扫描约 ${fmtN(meta.rows)} 行（全表扫描），建议先建索引`, 'no_index');
      if (rows > 50000) add('warn', `预计影响约 ${fmtN(rows)} 行，超过 5 万行，建议拆成小批量（每批 ≤ 5000 行）`, 'affected_rows');
    };
    const addIdx = (r, name, cols, uniq) => {
      if (!needTbl(r)) return;
      if (!r.virtual) {
        const idx = r.meta.idx || [];
        if (idx.some(x => x[0].toLowerCase() === name.toLowerCase())) add('bad', `索引 \`${name}\` 已存在`, 'index_exists');
        chkCols(r.meta, cols, false);
        const dup = idx.find(x => x[1].length <= cols.length && x[1].every((cc, i) => cc.toLowerCase() === cols[i].toLowerCase()));
        if (dup) add('warn', `新索引的前缀与已有索引 \`${dup[0]}\`（${dup[1].join(', ')}）重复，可能是冗余索引`, 'redundant_index');
      }
      if (!/^(idx|uk|ix)_/i.test(name)) add('warn', `索引名 \`${name}\` 建议以 ${uniq ? 'uk_' : 'idx_'} 开头`, 'index_name');
      if (cols.length > 5) add('warn', `联合索引包含 ${cols.length} 列，超过 5 列，请评估必要性`, 'index_cols');
    };
    const alterClause = (r, cl) => {
      const lc = cl.toLowerCase().replace(/\s+/g, ' ');
      let mm;
      if ((mm = cl.match(/^add\s+(unique\s+)?(?:index|key)\s*(`?[\w$]*`?)\s*\(([^)]*)\)/i))) {
        addIdx(r, unq(mm[2]) || 'unnamed', topSplit(mm[3]).map(x => unq(x.split(/[\s(]/)[0])), !!mm[1]);
      } else if (/^add\s+(constraint\s+\S+\s+)?primary\s+key/i.test(cl)) {
        if (!r.virtual && (r.meta.idx || []).some(x => x[0] === 'PRIMARY')) add('bad', `表 \`${r.t}\` 已有主键`, 'pk_exists'); else add('warn', '添加主键会重建表', 'add_pk');
      } else if (/^add\s+(constraint|foreign|fulltext|unique|partition)/i.test(cl)) {
        add('warn', /foreign/i.test(cl) ? '不建议使用外键（规则 no_foreign_key）' : '该变更会重建表或加锁，请与 DBA 确认', 'table_rebuild');
      } else if ((mm = cl.match(/^add\s+(?:column\s+)?`?([A-Za-z_]\w*)`?\s+(\w+)/i))) {
        const n = mm[1];
        if (!r.virtual && (r.meta.cols || []).some(x => x.n.toLowerCase() === n.toLowerCase())) add('bad', `列 \`${n}\` 已存在`, 'column_exists');
        if (/not\s+null/i.test(cl) && !/\bdefault\b/i.test(cl) && !/auto_increment/i.test(cl)) add('bad', `NOT NULL 列 \`${n}\` 必须指定 DEFAULT（否则存量数据无法填充）`, 'col_not_null_default');
        if (!/\bcomment\s+'/i.test(cl)) add('warn', `列 \`${n}\` 缺少 COMMENT`, 'column_comment');
        if (/^(text|blob|json|longtext|mediumtext)$/i.test(mm[2])) add('warn', `列 \`${n}\` 使用 ${mm[2].toUpperCase()}，会影响查询性能`, 'blob_col');
      } else if ((mm = cl.match(/^drop\s+(?:column\s+)?`?([A-Za-z_]\w*)`?$/i)) && !/^(index|key|primary|foreign|partition)$/i.test(mm[1])) {
        if (!r.virtual && !(r.meta.cols || []).some(x => x.n.toLowerCase() === mm[1].toLowerCase())) add('bad', `列 \`${mm[1]}\` 不存在`, 'column_not_exists');
        else add('warn', `删除列 \`${mm[1]}\` 不可逆，请确认代码已不再读写该列；建议开启备份`, 'drop_column');
      } else if ((mm = cl.match(/^drop\s+(?:index|key)\s+`?(\w+)`?/i))) {
        if (!r.virtual && !(r.meta.idx || []).some(x => x[0].toLowerCase() === mm[1].toLowerCase())) add('bad', `索引 \`${mm[1]}\` 不存在`, 'index_not_exists'); else add('warn', '删除索引前请确认没有查询依赖它', 'drop_index');
      } else if ((mm = cl.match(/^(modify|change)\s+(?:column\s+)?`?([A-Za-z_]\w*)`?/i))) {
        if (!r.virtual && !(r.meta.cols || []).some(x => x.n.toLowerCase() === mm[2].toLowerCase())) add('bad', `列 \`${mm[2]}\` 不存在`, 'column_not_exists');
        else add('warn', `${mm[1].toUpperCase()} 列 \`${mm[2]}\` 可能触发表重建，且缩短类型 / 长度会截断数据；${mm[1].toLowerCase() === 'change' ? '重命名列会让旧代码报错，' : ''}请确认`, 'modify_column');
      } else if (/^rename/i.test(lc)) add('warn', '重命名会导致依赖旧名的应用报错', 'rename');
      else if (/^engine/i.test(lc)) add('warn', '变更存储引擎会重建表', 'engine');
    };
    let m;
    switch (kw) {
      case 'select':
        add('bad', '工单只处理变更语句：SELECT 请使用「在线查询」（规则 forbid_select）', 'forbid_select'); break;
      case 'use': add('warn', '目标库已在提交页选定，请去掉 USE（切库会让后续语句落到别的库）', 'disallow_use'); break;
      case 'set': case 'grant': case 'revoke': case 'show': case 'explain': case 'call': case 'begin': case 'commit': case 'rollback': case 'flush': case 'optimize': case 'analyze':
        add('bad', `不支持的语句类型 ${kw.toUpperCase()}：工单只允许 DDL / DML`, 'unsupported'); break;
      case 'insert': case 'replace': {
        m = c.match(new RegExp('^(?:insert|replace)\\s+(?:ignore\\s+)?(?:into\\s+)?' + NAME + '\\s*(\\(([^)]*)\\))?', 'i'));
        if (!m) { add('bad', '语法错误：INSERT 缺少表名', 'syntax'); break; }
        const r = look(m[1]); if (!needTbl(r)) break;
        if (!m[2] && !/^set\b/i.test(c.slice(m[0].length).trim())) add('warn', 'INSERT 没有指定列名，表结构变更后会写错列，请显式列出列（规则 insert_must_specify_column）', 'insert_columns');
        if (m[3]) chkCols(r.meta, topSplit(m[3]).map(unq), r.virtual);
        if (/\binsert\b.*\bselect\b/i.test(c)) { add('warn', 'INSERT … SELECT 会长时间读源表，建议分批', 'insert_select'); if (/select\s+\*/i.test(c)) add('warn', 'INSERT … SELECT * 依赖列顺序，请显式列出列（避免 SELECT *）', 'select_star'); it.rows = Math.min(r.meta.rows || 0, 100000); }
        else { const vs = (c.match(/\)\s*,\s*\(/g) || []).length + 1; it.rows = vs; if (vs > 1000) add('warn', `单条 INSERT 含 ${vs} 行，超过 1000 行，建议拆分`, 'insert_rows'); }
        break;
      }
      case 'update': {
        m = c.match(new RegExp('^update\\s+(?:low_priority\\s+)?(?:ignore\\s+)?' + NAME + '\\s+set\\s+(.+?)(?:\\s+where\\s+(.+?))?(?:\\s+order\\s+by\\s+.*?)?(?:\\s+limit\\s+(\\d+))?$', 'i'));
        if (!m) { add('bad', '语法错误：UPDATE 语句无法解析', 'syntax'); break; }
        const r = look(m[1]); if (!needTbl(r)) break;
        const cols = []; const re = /(?:^|,)\s*`?(\w+)`?\s*=/g; let mm; while ((mm = re.exec(m[2]))) cols.push(mm[1]);
        chkCols(r.meta, cols, r.virtual);
        dmlWhere(r, m[3], 'UPDATE', m[4] ? +m[4] : 0); break;
      }
      case 'delete': {
        m = c.match(new RegExp('^delete\\s+(?:low_priority\\s+|quick\\s+|ignore\\s+)*from\\s+' + NAME + '(?:\\s+where\\s+(.+?))?(?:\\s+order\\s+by\\s+.*?)?(?:\\s+limit\\s+(\\d+))?$', 'i'));
        if (!m) { add('bad', '语法错误：DELETE 语句无法解析', 'syntax'); break; }
        const r = look(m[1]); if (!needTbl(r)) break;
        dmlWhere(r, m[2], 'DELETE', m[3] ? +m[3] : 0);
        if (it.rows > 100000 && m[2]) add('warn', 'DELETE 影响行数很大，会产生大事务与主从延迟，建议带 LIMIT 分批删除', 'big_delete');
        break;
      }
      case 'create': {
        if ((m = c.match(new RegExp('^create\\s+(?:temporary\\s+)?table\\s+(?:if\\s+not\\s+exists\\s+)?' + NAME, 'i')))) {
          const r = look(m[1]);
          if (r.meta && !/if\s+not\s+exists/i.test(c)) add('bad', `表 \`${r.db}\`.\`${r.t}\` 已存在`, 'table_exists');
          const body = c.slice(c.indexOf('(') + 1, c.lastIndexOf(')')), tail = c.slice(c.lastIndexOf(')') + 1);
          const defs = topSplit(body), colDefs = defs.filter(d => !/^(primary|unique|key|index|constraint|foreign|fulltext|check)\b/i.test(d));
          if (!/primary\s+key/i.test(body)) add('bad', '表必须有主键（规则 table_must_have_pk）', 'table_must_have_pk');
          if (!/comment\s*=?\s*'/i.test(tail)) add('warn', '表缺少 COMMENT，请补充表注释', 'table_comment');
          const eng = (tail.match(/engine\s*=\s*(\w+)/i) || [])[1]; if (eng && eng.toLowerCase() !== 'innodb') add('warn', `存储引擎为 ${eng}，建议使用 InnoDB`, 'engine');
          const cs = (tail.match(/(?:charset|character\s+set)\s*=?\s*(\w+)/i) || [])[1]; if (cs && !/^utf8mb4/i.test(cs)) add('warn', `字符集 ${cs}，建议使用 utf8mb4`, 'charset');
          const noCmt = colDefs.filter(d => !/\bcomment\s+'/i.test(d)).map(d => unq(d.split(/\s+/)[0]));
          if (noCmt.length) add('warn', `${noCmt.length} 个列缺少 COMMENT：${noCmt.slice(0, 4).join(', ')}${noCmt.length > 4 ? ' …' : ''}`, 'column_comment');
          colDefs.forEach(d => { const nm = unq(d.split(/\s+/)[0]); if (/\b(float|double)\b/i.test(d) && /amount|price|fee|money|balance/i.test(nm)) add('warn', `金额列 \`${nm}\` 请使用 DECIMAL，避免浮点误差`, 'float_money'); if (/\b(text|blob|json)\b/i.test(d)) add('warn', `列 \`${nm}\` 使用 TEXT/BLOB/JSON，会影响查询性能，请确认必要性`, 'blob_col'); });
          ctx.created[r.t.toLowerCase()] = {cols: colDefs.map(d => ({n: unq(d.split(/\s+/)[0])})), rows: 0, idx: []};
        } else if ((m = c.match(new RegExp('^create\\s+(unique\\s+)?index\\s+(' + ID + ')\\s+on\\s+' + NAME + '\\s*\\(([^)]*)\\)', 'i')))) {
          addIdx(look(m[3]), unq(m[2]), topSplit(m[4]).map(x => unq(x.split(/[\s(]/)[0])), !!m[1]);
        } else add('warn', '不建议通过工单创建视图 / 存储过程 / 触发器 / 事件等对象，请与 DBA 确认', 'create_object');
        break;
      }
      case 'alter': {
        m = c.match(new RegExp('^alter\\s+(?:ignore\\s+)?table\\s+' + NAME + '\\s+(.+)$', 'i'));
        if (!m) { add('bad', '语法错误：ALTER 语句无法解析', 'syntax'); break; }
        const r = look(m[1]); if (!needTbl(r)) break;
        const k = r.t.toLowerCase();
        if (ctx.lastAlter[k]) add('warn', `同一张表 \`${r.t}\` 已有 ALTER（第 ${ctx.lastAlter[k]} 条），多条 ALTER 建议合并为一条，减少重建次数`, 'merge_alter');
        ctx.lastAlter[k] = it.n;
        topSplit(m[2]).forEach(cl => alterClause(r, cl));
        if (bigTable(r.meta) && !r.virtual) { add('warn', `表 \`${r.t}\` 约 ${fmtN(r.meta.rows)} 行 / ${fmtMB(r.meta.size)}，直接 ALTER 会长时间持有元数据锁并造成主从延迟，建议使用在线改表（gh-ost / pt-osc）。goInception 执行时将走 OSC`, 'osc'); it.osc = true; }
        break;
      }
      case 'drop': {
        if (/^drop\s+(database|schema)\b/i.test(c)) { add('bad', '高危：DROP DATABASE 命中站点规则 critical_ddl_regex，禁止通过工单执行', 'critical_ddl'); break; }
        if ((m = c.match(new RegExp('^drop\\s+(?:temporary\\s+)?table\\s+(?:if\\s+exists\\s+)?' + NAME, 'i')))) {
          const r = look(m[1]); if (!r.meta && !r.noDb && !/if\s+exists/i.test(c)) { add('bad', `表 \`${r.db}\`.\`${r.t}\` 不存在`, 'table_not_exists'); break; }
          add('warn', '高危：DROP TABLE 不可恢复。建议先 RENAME 为 `_del` 后缀观察一段时间，确认无业务访问后再删除（规则 critical_ddl_regex）', 'critical_ddl');
        } else if ((m = c.match(new RegExp('^drop\\s+index\\s+(' + ID + ')\\s+on\\s+' + NAME, 'i')))) {
          const r = look(m[2]);
          if (needTbl(r) && !r.virtual && !(r.meta.idx || []).some(x => x[0].toLowerCase() === unq(m[1]).toLowerCase())) add('bad', `索引 \`${unq(m[1])}\` 不存在`, 'index_not_exists'); else add('warn', '删除索引前请确认没有查询依赖它', 'drop_index');
        } else add('warn', '高危 DROP 语句，请与 DBA 确认', 'critical_ddl');
        break;
      }
      case 'truncate': add('bad', '高危：TRUNCATE 命中站点规则 critical_ddl_regex，禁止通过工单执行（需要清空请用带 LIMIT 的分批 DELETE）', 'critical_ddl'); break;
      case 'rename': add('warn', '重命名表会导致依赖该表名的应用报错，请确认发布顺序', 'rename'); break;
    }
  }

  /* ---------- SQL Server（T-SQL）规则：规则 ID 沿用 Bytebase 命名 ---------- */
  function checkMs(ctx, it) {
    const add = it.add;
    const c = it.sql.replace(/\s+/g, ' ').trim(), l = c.toLowerCase(), kw = (l.match(/^[a-z]+/) || [''])[0];
    it.kind = kw; const at = `（第 ${it.line} 行）`;
    if (/\b(drop\s+database|alter\s+database|create\s+database|backup\s+(database|log)|restore\s+(database|log)|dbcc\s|shutdown|reconfigure|kill\s+\d|xp_cmdshell|sp_configure|sp_addlinkedserver|sp_oacreate|openrowset|opendatasource|openquery|bulk\s+insert|(create|alter|drop)\s+login)\b/i.test(noStr(c))) { add('bad', `[statement.dangerous] 危险语句，禁止通过工单执行${at}`, 'statement.dangerous'); return; }
    if (!/^(select|insert|update|delete|merge|create|alter|drop|truncate|exec|execute|declare|set|use|if|begin|print|with|raiserror|commit|rollback|while)$/.test(kw)) { add('bad', `语法错误：'${c.slice(0, 20)}' 附近有语法错误${at}`, 'syntax'); return; }
    const plain = noStr(c);
    if ((plain.match(/\(/g) || []).length !== (plain.match(/\)/g) || []).length) { add('bad', `语法错误：括号不匹配${at}`, 'syntax'); return; }
    const nm = re => { const m = c.match(re); if (m) { const r = parseName(m[1], true); it.tbl = r.t; return r; } return null; };
    switch (kw) {
      case 'use': add('warn', `[statement.disallow-use] 目标库已在提交页选定，USE 切库会让后续语句落到别的库${at}`, 'statement.disallow-use'); break;
      case 'update':
        nm(new RegExp('^update\\s+(?:top\\s*\\(\\d+\\)\\s+)?' + NAME, 'i'));
        if (!/\bwhere\b/i.test(plain)) add('bad', `[statement.where.require.update-delete] UPDATE 没有 WHERE 条件，会修改整张表${at}`, 'statement.where.require.update-delete'); else it.rows = 1 + hs(c) % 480;
        if (/\blike\s+n?'%/i.test(c)) add('warn', `[statement.where.no-leading-wildcard-like] LIKE 前导通配符无法使用索引${at}`, 'like');
        break;
      case 'delete':
        nm(new RegExp('^delete\\s+(?:top\\s*\\(\\d+\\)\\s+)?(?:from\\s+)?' + NAME, 'i'));
        if (!/\bwhere\b/i.test(plain)) add('bad', `[statement.where.require.update-delete] DELETE 没有 WHERE 条件，会清空整张表${at}`, 'statement.where.require.update-delete'); else it.rows = 1 + hs(c) % 20000;
        break;
      case 'select':
        if (/select\s+(top\s*\(?\d+\)?\s+)?(distinct\s+)?\*/i.test(c)) add('warn', `[statement.select.no-select-all] 避免 SELECT *，请显式列出列${at}`, 'select_star');
        if (/\bfrom\b/i.test(plain) && !/\bwhere\b/i.test(plain)) add('warn', `[statement.where.require.select] SELECT 有 FROM 却没有 WHERE（工单里的 SELECT 只显示返回行数）${at}`, 'select_where');
        if (/\blike\s+n?'%/i.test(c)) add('warn', `[statement.where.no-leading-wildcard-like] LIKE 前导通配符无法使用索引${at}`, 'like');
        break;
      case 'insert': {
        const r = nm(new RegExp('^insert\\s+(?:into\\s+)?' + NAME, 'i'));
        if (r && !/^insert\s+(?:into\s+)?\S+\s*\(/i.test(c) && !/default\s+values|\bexec\b/i.test(c)) add('warn', `[statement.insert.must-specify-column] INSERT 要显式指定列名${at}`, 'insert_columns');
        it.rows = (c.match(/\)\s*,\s*\(/g) || []).length + 1; break;
      }
      case 'create': {
        const m = c.match(new RegExp('^create\\s+table\\s+' + NAME, 'i'));
        if (m) {
          const r = parseName(m[1], true); it.tbl = r.t;
          if (!/^[#@]/.test(r.t) && !/primary\s+key/i.test(c)) add('bad', `[table.require-pk] 表 ${r.t} 没有主键${at}`, 'table.require-pk');
          const defs = topSplit(c.slice(c.indexOf('(') + 1, c.lastIndexOf(')')));
          const nul = defs.filter(d => !/^(primary|constraint|unique|index|foreign|check)\b/i.test(d) && !/not\s+null|identity|primary key|\bas\b/i.test(d)).map(d => unq(d.split(/\s+/)[0]));
          if (nul.length) add('warn', `[column.no-null] 列 ${nul.slice(0, 3).join(', ')}${nul.length > 3 ? ' 等' : ''} 允许 NULL${at}`, 'column.no-null');
          if (/\bforeign\s+key\b/i.test(c)) add('warn', `[table.no-foreign-key] 不建议使用外键${at}`, 'fk');
        } else if (/^create\s+(unique\s+)?(clustered\s+|nonclustered\s+)?index/i.test(c)) {
          const m2 = c.match(new RegExp('\\bon\\s+' + NAME, 'i')); if (m2) it.tbl = parseName(m2[1], true).t;
          const t = it.tbl && tblOf(ctx.inst, ctx.db, it.tbl);
          if (t && bigTable(t) && !/online\s*=\s*on/i.test(c)) add('warn', `[schema.backward-compatibility] 在大表 ${it.tbl}（约 ${fmtN(t.rows)} 行）上建索引，建议加 WITH (ONLINE = ON) 并避开业务高峰${at}`, 'big_index');
        }
        break;
      }
      case 'alter': {
        const r = nm(new RegExp('^alter\\s+table\\s+' + NAME, 'i'));
        if (/\balter\s+column\b/i.test(c)) add('warn', `[column.disallow-change-type] ALTER COLUMN 会改变列类型 / 长度 / 可空性${at}`, 'alter_column');
        if (/\bdrop\s+column\b/i.test(c)) add('warn', `[schema.backward-compatibility] DROP COLUMN 不可逆，请确认代码已不再使用${at}`, 'drop_column');
        if (/\badd\b[^,]*\bnot\s+null\b/i.test(c) && !/\bdefault\b/i.test(c)) add('warn', `[schema.backward-compatibility] 添加 NOT NULL 列却没有 DEFAULT${at}`, 'not_null_default');
        const t = r && tblOf(ctx.inst, ctx.db, r.t);
        if (t && bigTable(t) && /\b(add|alter)\b/i.test(c)) add('warn', `[schema.backward-compatibility] ${r.t} 是大表（约 ${fmtN(t.rows)} 行），变更会持有 Sch-M 锁，请避开业务高峰${at}`, 'big_alter');
        break;
      }
      case 'drop': {
        const m = c.match(new RegExp('^drop\\s+table\\s+(?:if\\s+exists\\s+)?' + NAME, 'i'));
        if (m) { it.tbl = parseName(m[1], true).t; add('warn', `[schema.backward-compatibility] DROP TABLE ${it.tbl} 不可恢复，请确认${at}`, 'drop_table'); } else add('warn', `[schema.backward-compatibility] DROP 语句不可恢复，请确认${at}`, 'drop');
        break;
      }
      case 'truncate': add('warn', `[schema.backward-compatibility] TRUNCATE 不可恢复，且会重置 IDENTITY${at}`, 'truncate'); break;
      case 'exec': case 'execute': if (/^exec(ute)?\s+sp_rename/i.test(c)) add('warn', `[schema.backward-compatibility] sp_rename 会让依赖旧名的对象失效${at}`, 'rename'); break;
    }
  }

  /* ---------- 回滚语句（goInception 备份库里的 rollback_statement；原型按语句推导） ---------- */
  function wfRollback(wf) {
    const inst = wfInst(wf.inst), out = [];
    wf.chk.items.forEach(it => {
      const c = it.sql.replace(/\s+/g, ' ').trim(), r = {orig: it.sql, rb: []}, q = x => '`' + x + '`', tn = `${q(wf.db)}.${q(it.tbl || 'tbl')}`;
      const meta = tblOf(inst, wf.db, it.tbl), pk = meta && (meta.cols.find(x => x.key === 'PRI') || meta.cols[0]), pkn = pk ? pk.n : 'id', base = 20000000 + hs(c) % 9000000;
      let m;
      if (it.kind === 'update' && (m = c.match(/set\s+(.+?)\s+where/i))) {
        const col = (m[1].match(/`?(\w+)`?\s*=/) || [])[1] || 'status', cm = meta && meta.cols.find(x => x.n === col);
        const old = cm && /char|text|date/i.test(cm.t) ? "'旧值'" : String(Math.max(0, (parseInt((m[1].match(/=\s*(-?\d+)/) || [])[1], 10) || 1) - 1));
        const n = Math.min(3, Math.max(1, it.rows));
        for (let i = 0; i < n; i++) r.rb.push(`UPDATE ${tn} SET ${q(col)} = ${old} WHERE ${q(pkn)} = ${base + i * 17};`);
        if (it.rows > n) r.rb.push(`-- … 另有 ${fmtN(it.rows - n)} 行的回滚语句（共 ${fmtN(it.rows)} 行，已省略）`);
      } else if (it.kind === 'insert' || it.kind === 'replace') {
        for (let i = 0; i < Math.min(3, it.rows || 1); i++) r.rb.push(`DELETE FROM ${tn} WHERE ${q(pkn)} = ${base + i * 11};`);
      } else if (it.kind === 'delete') {
        const cols = meta ? meta.cols.slice(0, 4) : [];
        const val = (cc, i) => /int|decimal/i.test(cc.t) ? String(base + i * 7) : /date/i.test(cc.t) ? "'2026-07-01 08:12:33'" : `'${cc.n}_${base % 1000 + i}'`;
        for (let i = 0; i < Math.min(3, it.rows || 1); i++) r.rb.push(`INSERT INTO ${tn} (${cols.map(x => q(x.n)).join(', ')}) VALUES (${cols.map(cc => val(cc, i)).join(', ')});`);
        if (it.rows > 3) r.rb.push(`-- … 另有 ${fmtN(it.rows - 3)} 行的回滚语句（已省略）`);
      } else if (it.kind === 'alter') {
        c.replace(/^alter\s+table\s+\S+\s+/i, '').split(/\s*,\s*(?=add|drop|modify|change)/i).forEach(cl => {
          let mm;
          if ((mm = cl.match(/^add\s+(?:unique\s+)?(?:index|key)\s+`?(\w+)`?/i))) r.rb.push(`ALTER TABLE ${tn} DROP INDEX ${q(mm[1])};`);
          else if ((mm = cl.match(/^add\s+(?:column\s+)?`?(\w+)`?\s+\w+/i))) r.rb.push(`ALTER TABLE ${tn} DROP COLUMN ${q(mm[1])};`);
          else if ((mm = cl.match(/^modify\s+(?:column\s+)?`?(\w+)`?/i))) { const cm = meta && meta.cols.find(x => x.n === mm[1]); r.rb.push(`ALTER TABLE ${tn} MODIFY COLUMN ${q(mm[1])} ${cm ? cm.t : 'varchar(255)'}${cm && !cm.nul ? ' NOT NULL' : ''};`); }
          else r.rb.push(`-- 无法自动生成：${cl.slice(0, 50)}`);
        });
      } else if (it.kind === 'create') r.rb.push(`DROP TABLE IF EXISTS ${tn};`);
      else r.rb.push('-- 该语句无法自动生成回滚（DROP / TRUNCATE 需从备份恢复）');
      out.push(r);
    });
    return out;
  }

  /* ============================================================================
     权限 / 可见性 / 审批人（业务规则以 internal/sqlflow 为准）
     ========================================================================== */
  const WF = [];                       // 全部工单（内存）；window.WORKFLOWS 指向同一个数组
  const asUser = u => typeof u === 'string' ? USERS.find(x => x.login === u) : u;
  const ucodes = u => { const s = new Set(); (u.groups || []).forEach(gid => { const g = GROUPS.find(x => x.id === gid); if (g) g.perms.forEach(p => s.add(p)); }); (u.extraGrant || []).forEach(c => s.add(c)); (u.extraDeny || []).forEach(c => s.delete(c)); return s; };
  const uHas = (u, code) => u.super ? true : (!(BTNS[code] || {}).locked && ucodes(u).has(code));
  /* 某一级的审批人 = 属于该级权限组 且 属于工单所属资源组（且有审批权限）；超级用户另计 */
  const levelMembers = (rg, lv) => USERS.filter(u => u.status === 'active' && !u.super && u.groups.includes(flowOf(rg)[lv]) && u.rgs.includes(rg) && uHas(u, 'sqlworkflow.approve'));
  const isLevelApprover = (wf, u) => wf.status === ST.wait && (u.super || (u.groups.includes(flowOf(wf.rg)[wf.level]) && u.rgs.includes(wf.rg) && uHas(u, 'sqlworkflow.approve')));
  /* 不能自己审批自己（ban_self_audit）；超级用户不受限，同原项目 */
  const canApproveFor = (wf, u) => { u = asUser(u); return !!u && wf.status === ST.wait && uHas(u, 'sqlworkflow.approve') && (u.super || (u.login !== wf.submitter && isLevelApprover(wf, u))); };
  const approverList = (wf, lv) => levelMembers(wf.rg, lv).filter(u => u.login !== wf.submitter);
  const approverText = (wf, lv) => { const l = approverList(wf, lv).map(u => u.login); return l.length ? l.join('、') : 'admin（超级用户；本级无其他可审批人）'; };
  const wfVisible = wf => ME.super || (can('sqlworkflow.view_group') && ME.rgs.includes(wf.rg)) || (wf.submitter === ME.login && (can('sqlworkflow.view_own') || can('sqlworkflow.view_group')));
  const visibleWfs = () => WF.filter(wfVisible);
  /* 执行权限：资源组内的 execute_group，或提交人本人的 execute_own */
  function execCode(wf) {
    if (can('sqlworkflow.execute_group') && (ME.super || ME.rgs.includes(wf.rg))) return 'sqlworkflow.execute_group';
    if (can('sqlworkflow.execute_own') && wf.submitter === ME.login) return 'sqlworkflow.execute_own';
    return '';
  }
  const inWindow = (wf, t) => { t = t || wfNow(); const w = wf.window; return !w || ((!w.from || t >= w.from) && (!w.to || t <= w.to)); };
  const winText = w => w ? `${w.from || '不限'} ～ ${w.to || '不限'}` : '';
  const botOf = rg => (RGROUPS.find(r => r.id === rg) || {}).bot || '未配置';
  const flowText = rg => flowOf(rg).map(g => LVNAME[g]).join(' → ');

  window.WORKFLOWS = WF;
  window.wfPendingList = u => WF.filter(w => canApproveFor(w, u));
  window.wfPendingFor = u => window.wfPendingList(u).length;
  const badge = () => window.wfPendingFor(ME);
  NAV.sqlworkflow.badge = badge;                                   // NAV 是 NAV_TREE 条目的拷贝，侧栏读的是 NAV_TREE，所以两处都设
  NAV_TREE.forEach(g => g.items.forEach(m => { if (m.id === 'sqlworkflow') m.badge = badge; }));

  /* ============================================================================
     种子数据：20 个工单，覆盖 8 种状态，含 3 个批量上线（MsSQL）
     ========================================================================== */
  const A = (lv, user, at, note) => ({lv, user, at, note: note || '', action: 'approve'});
  const sha1of = (id, i) => (hs('wf' + id + ':' + i).toString(16) + hs('x' + id + ':' + i).toString(16) + hs('y' + id + i).toString(16) + hs('z' + i + id).toString(16) + hs('w' + id).toString(16)).padEnd(40, '0').slice(0, 40);
  const defaultFail = it => it.kind === 'insert' ? "Duplicate entry '10086' for key 'PRIMARY'" : 'Lock wait timeout exceeded; try restarting transaction';

  function seed(o) {
    const inst = wfInst(o.inst), insts = o.insts || [o.inst];
    const wf = Object.assign({type: 'sql', level: 0, approvals: [], log: [], window: null, schedule: null, scheduleBy: '', backup: false, executor: '', res: [], osc: null, bi: null, skip: false, manual: false, url: '', startedAt: '', endedAt: '', abort: null, ev: []}, o, {insts});
    wf.chk = o.rcmds ? redisSeedChk(o) : wfCheck(o.sql, inst, o.db); wf.sqlType = wf.chk.type;
    const fl = flowOf(wf.rg).map(g => LVNAME[g]).join(' → ');
    wf.log = [[wf.created, wf.submitter, `提交工单，进入审批流（${fl}）`], [wf.created, '系统', `机器人通知（${botOf(wf.rg)}）：[SQL上线申请] 新的工单申请 #${wf.id}，@ ${approverText(wf, 0)}`]];
    if (wf.status === ST.auto) {
      wf.log = [[wf.created, wf.submitter, '提交工单'], [wf.created, '系统', '服务端重新检查：存在错误级别的检查结果，自动审批不通过'], [wf.created, '系统', `机器人通知（${botOf(wf.rg)}）：[SQL上线申请] 工单 #${wf.id} 被系统自动驳回，@ ${wf.submitter}`]];
    }
    wf.approvals.forEach(a => {
      wf.log.push([a.at, a.user, `审批通过（第 ${a.lv + 1} 级 ${LVNAME[flowOf(wf.rg)[a.lv]]}）${a.note ? '：' + a.note : ''}`]);
      wf.log.push([a.at, '系统', `机器人通知（${botOf(wf.rg)}）：` + (a.lv + 1 < flowOf(wf.rg).length ? `工单 #${wf.id} 流转到第 ${a.lv + 2} 级，@ ${approverText(wf, a.lv + 1)}` : `工单 #${wf.id} 审批通过，@ ${wf.submitter}`)]);
    });
    if (wf.abort) wf.log.push([wf.abort.at, wf.abort.by, (wf.abort.kind === 'reject' ? '驳回工单：' : '撤回工单：') + wf.abort.note]);
    (o.ev || []).forEach(e => wf.log.push(e));
    if ((wf.status === ST.done || wf.status === ST.exc) && !wf.manual) wf.log.push([wf.endedAt, '系统', wf.status === ST.done ? '执行完成，工单已正常结束' : (wf.type === 'batch' ? '批量执行汇总：成功 2，失败 1，已关闭 1。已有实例的变更已生效，失败实例与其余实例状态不一致，请人工核对' : '执行有异常：第 2 条语句失败，其后语句未执行')]);
    if (o.finished && wf.type !== 'batch' && !wf.manual) {
      wf.res = wf.chk.items.map((it, i) => o.failAt >= 0 && i > o.failAt ? {st: 'skip', msg: '前序语句失败, 未执行', rows: 0, sec: 0}
        : i === o.failAt ? {st: 'bad', msg: o.failMsg || defaultFail(it), rows: 0, sec: 0.004}
        : {st: 'ok', msg: it.osc && o.executor === 'goinception' ? 'gh-ost 完成，已原子切换' : '', rows: it.rows || 0, sec: it.osc ? +(900 + hs(it.sql) % 900 + 0.4).toFixed(1) : +(0.012 + (hs(it.sql) % 900) / 1000).toFixed(3)});
      const oi = wf.chk.items.findIndex(x => x.osc);
      if (oi >= 0 && o.executor === 'goinception') wf.osc = {tool: 'gh-ost', idx: oi, pct: 100, state: 'done', sha1: sha1of(wf.id, oi), eta: '00:00:00'};
    }
    return wf;
  }
  const SEEDS = [
    {id: 1042, name: '订单表增加字段 refund_flag', inst: 1, db: 'order_db', submitter: 'rd_zhao', rg: 1, created: '2026-10-07 10:08', status: ST.wait, level: 0, backup: true, url: 'https://jira.example.com/browse/ORD-2381',
      sql: "ALTER TABLE orders\n  ADD COLUMN refund_flag tinyint NOT NULL DEFAULT 0 COMMENT '是否发生退款 0否 1是';"},
    {id: 1041, name: '测试环境清理超期未支付订单状态', inst: 7, db: 'order_db', submitter: 'qa_sun', rg: 1, created: '2026-10-07 09:40', status: ST.wait, level: 0, backup: true,
      sql: "UPDATE orders SET status = 4 WHERE status = 0 AND created_at < '2026-09-01 00:00:00';"},
    {id: 1040, name: '退款单增加创建时间索引', inst: 3, db: 'pay_db', submitter: 'rd_zhou', rg: 2, created: '2026-10-06 17:20', status: ST.wait, level: 1, backup: true, approvals: [A(0, 'tl_wang', '2026-10-06 18:05', '同意，注意避开对账时段')],
      sql: 'ALTER TABLE refunds ADD INDEX idx_created (created_at);'},
    {id: 1039, name: '支付单增加渠道+支付时间联合索引', inst: 3, db: 'pay_db', submitter: 'rd_zhou', rg: 2, created: '2026-10-06 10:30', status: ST.pass, level: 1, backup: true, approvals: [A(0, 'tl_wang', '2026-10-06 11:12', '查询慢，同意'), A(1, 'dba_li', '2026-10-06 14:20', '已确认，低峰期用 gh-ost 执行')],
      sql: 'ALTER TABLE payments ADD INDEX idx_channel_paid (channel, paid_at);'},
    {id: 1038, name: '订单明细增加 spu_id 冗余列与索引', inst: 1, db: 'order_db', submitter: 'rd_zhao', rg: 1, created: '2026-10-06 15:00', status: ST.pass, level: 1, backup: true, window: {from: '2026-10-07 22:00', to: '2026-10-08 05:00'}, approvals: [A(0, 'tl_wang', '2026-10-06 15:40', ''), A(1, 'dba_li', '2026-10-06 16:30', '今晚 22:00 后执行，大表走 OSC')],
      sql: "ALTER TABLE order_items\n  ADD COLUMN spu_id bigint unsigned NOT NULL DEFAULT 0 COMMENT 'SPU ID',\n  ADD INDEX idx_spu (spu_id);"},
    {id: 1037, name: '清理 90 天前订单流水日志', inst: 1, db: 'order_db', submitter: 'rd_zhao', rg: 1, created: '2026-10-06 09:20', status: ST.timing, level: 1, backup: true, executor: 'goinception', schedule: '2026-10-08 02:00', scheduleBy: 'dba_li', window: {from: '2026-10-08 01:00', to: '2026-10-08 05:00'},
      approvals: [A(0, 'tl_wang', '2026-10-06 10:05', ''), A(1, 'dba_li', '2026-10-06 11:30', '凌晨执行，分批删除')], ev: [['2026-10-07 09:30', 'dba_li', '设置定时执行：2026-10-08 02:00（goInception）']],
      sql: "DELETE FROM order_log WHERE created_at < '2026-07-09 00:00:00' LIMIT 200000;"},
    {id: 1036, name: '登录日志增加 IP+时间索引', inst: 4, db: 'user_db', submitter: 'qa_sun', rg: 3, created: '2026-10-07 08:40', status: ST.run, level: 0, backup: false, executor: 'goinception', startedAt: '2026-10-07 09:05', approvals: [A(0, 'dba_li', '2026-10-07 08:52', '低峰期 gh-ost 执行')],
      ev: [['2026-10-07 09:05', 'dba_li', '立即执行（goInception，在线改表 gh-ost）'], ['2026-10-07 09:05', '系统', `机器人通知（钉钉）：[SQL上线申请] 工单 #1036 开始执行`]],
      sql: 'ALTER TABLE login_log ADD INDEX idx_ip_created (ip, created_at);'},
    {id: 1035, name: '订单表增加状态+创建时间联合索引', inst: 1, db: 'order_db', submitter: 'rd_zhao', rg: 1, created: '2026-10-05 16:10', status: ST.done, level: 1, backup: true, executor: 'goinception', startedAt: '2026-10-06 22:05', endedAt: '2026-10-06 22:29', finished: true,
      approvals: [A(0, 'tl_wang', '2026-10-05 16:40', ''), A(1, 'dba_li', '2026-10-05 17:15', '')], ev: [['2026-10-06 22:05', 'dba_li', '立即执行（goInception，在线改表 gh-ost）']],
      sql: 'ALTER TABLE orders ADD INDEX idx_status_created (status, created_at);'},
    {id: 1034, name: '修正 6 笔订单状态（测试环境）', inst: 7, db: 'order_db', submitter: 'qa_sun', rg: 1, created: '2026-10-06 09:30', status: ST.done, level: 1, backup: true, executor: 'goinception', startedAt: '2026-10-06 11:02', endedAt: '2026-10-06 11:02', finished: true,
      approvals: [A(0, 'tl_wang', '2026-10-06 10:00', ''), A(1, 'dba_li', '2026-10-06 10:40', '')], ev: [['2026-10-06 11:02', 'dba_li', '立即执行（goInception，备份已开启）']],
      sql: "UPDATE orders SET status = 3 WHERE order_no IN ('E20260930001', 'E20260930002', 'E20260930005', 'E20260930011', 'E20260930012', 'E20260930019');"},
    {id: 1033, name: '补录 3 笔历史退款单', inst: 3, db: 'pay_db', submitter: 'rd_zhou', rg: 2, created: '2026-10-05 14:30', status: ST.exc, level: 1, backup: false, executor: 'native', startedAt: '2026-10-05 16:10', endedAt: '2026-10-05 16:10', finished: true, failAt: 1,
      failMsg: "Duplicate entry 'RF20261005002' for key 'refunds.uk_refund_no'",
      approvals: [A(0, 'tl_wang', '2026-10-05 15:00', ''), A(1, 'dba_li', '2026-10-05 15:40', '')], ev: [['2026-10-05 16:10', 'dba_li', '立即执行（原生执行：goInception 本次未启用）']],
      sql: "INSERT INTO refunds (refund_no, pay_no, amount, created_at) VALUES ('RF20261005001', 'P20260930118', 59.00, '2026-10-05 14:21:00');\nINSERT INTO refunds (refund_no, pay_no, amount, created_at) VALUES ('RF20261005002', 'P20260930207', 128.50, '2026-10-05 15:02:00');\nINSERT INTO refunds (refund_no, pay_no, amount, created_at) VALUES ('RF20261005003', 'P20260930341', 36.00, '2026-10-05 16:40:00');"},
    {id: 1032, name: '归档库删除 2025 年订单归档表', inst: 1, db: 'order_archive', submitter: 'rd_zhao', rg: 1, created: '2026-10-05 11:20', status: ST.abort, level: 0, backup: false,
      abort: {by: 'tl_wang', at: '2026-10-05 13:42', stage: 'review', lv: 0, kind: 'reject', note: '归档数据需保留 3 年（财务审计要求），不允许删除；如确需下线请先走归档审批。'},
      sql: 'DROP TABLE orders_2025;'},
    {id: 1031, name: '报表库 fact_sales 增加客户索引（批量 4 实例）', type: 'batch', inst: 6, insts: [6, 61, 62, 63], db: 'ReportDW', submitter: 'dba_li', rg: 4, created: '2026-10-04 19:20', status: ST.exc, level: 0, executor: 'native', startedAt: '2026-10-04 23:10', endedAt: '2026-10-04 23:12',
      approvals: [A(0, 'admin', '2026-10-04 19:50', '超级用户代审（提交人是本组唯一 DBA，不能自审）')], ev: [['2026-10-04 23:10', 'dba_li', '立即执行（原生执行，批量 4 个实例，并发 4 路）'], ['2026-10-04 23:11', 'dba_li', '关闭实例 mssql-report-prod-04（已下线检修，不再计入工单成败）']],
      bi: [{inst: 6, st: '成功', msg: '', sec: 2.8}, {inst: 61, st: '成功', msg: '', sec: 3.1}, {inst: 62, st: '失败', msg: '连接超时：dial tcp 10.20.4.33:1433: i/o timeout', sec: 30.0}, {inst: 63, st: '已关闭', msg: '实例下线检修，由 DBA 关闭，不再计入工单成败', sec: 0}],
      sql: 'CREATE NONCLUSTERED INDEX IX_fact_sales_customer ON dbo.fact_sales (customer_key) INCLUDE (amount) WITH (ONLINE = ON);'},
    {id: 1030, name: '批量补齐 dim_customer 空地区（3 实例）', type: 'batch', inst: 6, insts: [6, 61, 62], db: 'ReportDW', submitter: 'dba_li', rg: 4, created: '2026-10-03 15:00', status: ST.done, level: 0, executor: 'native', startedAt: '2026-10-03 20:00', endedAt: '2026-10-03 20:01',
      approvals: [A(0, 'admin', '2026-10-03 15:30', '超级用户代审')], ev: [['2026-10-03 20:00', 'dba_li', '立即执行（原生执行，批量 3 个实例）']],
      bi: [{inst: 6, st: '成功', msg: '影响 312 行', sec: 0.4}, {inst: 61, st: '成功', msg: '影响 298 行', sec: 0.5}, {inst: 62, st: '成功', msg: '影响 305 行', sec: 0.4}],
      sql: "UPDATE dbo.dim_customer SET region = N'未知' WHERE region IS NULL;"},
    {id: 1029, name: '批量清理 2024 年前销售明细（3 实例）', type: 'batch', inst: 6, insts: [6, 61, 62], db: 'ReportDW', submitter: 'dba_li', rg: 4, created: '2026-10-07 11:30', status: ST.wait, level: 0,
      bi: [{inst: 6, st: '待执行', msg: '', sec: 0}, {inst: 61, st: '待执行', msg: '', sec: 0}, {inst: 62, st: '待执行', msg: '', sec: 0}],
      sql: 'DELETE FROM dbo.fact_sales WHERE date_key < 20240101;'},
    {id: 1028, name: '用户优惠券状态批量置为已使用', inst: 4, db: 'user_db', submitter: 'qa_sun', rg: 3, created: '2026-10-06 14:12', status: ST.auto, level: 0, backup: true,
      sql: 'UPDATE user_coupons SET status = 1 WHERE user_id = 10086;'},
    {id: 1027, name: '三位内测用户升级为 VIP2', inst: 4, db: 'user_db', submitter: 'qa_sun', rg: 3, created: '2026-10-03 10:15', status: ST.done, level: 0, backup: true, executor: 'goinception', startedAt: '2026-10-03 11:20', endedAt: '2026-10-03 11:20', finished: true,
      approvals: [A(0, 'dba_li', '2026-10-03 11:00', '')], ev: [['2026-10-03 11:20', 'dba_li', '立即执行（goInception，备份已开启）']],
      sql: "UPDATE users SET level = 2 WHERE username IN ('lin_xiaoyu', 'chen_zhe', 'wu_ming');"},
    {id: 1026, name: '银行卡表增加绑定渠道字段', inst: 3, db: 'pay_db', submitter: 'rd_zhou', rg: 2, created: '2026-10-02 16:40', status: ST.abort, level: 1, backup: true, approvals: [A(0, 'tl_wang', '2026-10-02 17:10', '')],
      abort: {by: 'rd_zhou', at: '2026-10-03 09:15', stage: 'review', lv: 1, kind: 'cancel', note: '需求取消，改走新的绑卡服务，无需再变更。'},
      sql: "ALTER TABLE bank_cards ADD COLUMN bind_channel varchar(16) NOT NULL DEFAULT '' COMMENT '绑定渠道';"},
    {id: 1025, name: '收货地址表新增区县字段与联合索引', inst: 1, db: 'order_db', submitter: 'rd_zhao', rg: 1, created: '2026-10-07 11:05', status: ST.wait, level: 0, backup: true,
      sql: "ALTER TABLE user_addr ADD COLUMN district varchar(32) NOT NULL DEFAULT '' COMMENT '区县';\nALTER TABLE user_addr ADD INDEX idx_user_district (user_id, district);"},
    {id: 1024, name: '银行卡表增加持卡人索引', inst: 3, db: 'pay_db', submitter: 'tl_wang', rg: 2, created: '2026-10-07 11:40', status: ST.wait, level: 0, backup: true,
      sql: 'ALTER TABLE bank_cards ADD INDEX idx_holder (holder);'},
    {id: 1023, name: '收货地址详情列扩容（线下 pt-osc 执行）', inst: 1, db: 'order_db', submitter: 'rd_zhao', rg: 1, created: '2026-10-02 10:00', status: ST.done, level: 1, backup: false, manual: true, executor: 'manual', endedAt: '2026-10-02 23:40',
      approvals: [A(0, 'tl_wang', '2026-10-02 10:30', ''), A(1, 'dba_li', '2026-10-02 11:10', '线下用 pt-osc 执行，执行完在系统里确认结束')], ev: [['2026-10-02 23:40', 'dba_li', '确认手工执行结束（手工工单：系统未执行任何语句，线下已用 pt-osc 完成）']],
      sql: "ALTER TABLE user_addr MODIFY COLUMN detail varchar(512) NOT NULL COMMENT '详细地址';"},
  ];
  SEEDS.push(...redisSeeds()); SEEDS.forEach(o => WF.push(seed(o)));
  /* #1036 正在 gh-ost 在线改表：进度 37%，缓慢推进（约 4 秒 +1%），可演示暂停 / 恢复 / 终止 */
  (function () { const w = WF.find(x => x.id === 1036); w.res = [{st: 'run'}]; w.runIdx = 0; w.osc = {tool: 'gh-ost', idx: 0, pct: 37, state: 'running', sha1: sha1of(1036, 0), eta: '00:02:10', slow: true}; w._t = setTimeout(() => oscTick(w), 4000); })();
  /* 批量种子的首实例检查结果 / 单实例种子的备份说明已由 seed() 生成 */
  WF.sort((a, b) => b.id - a.id);

  /* ============================================================================
     状态机操作（审批 / 终止 / 执行 / 定时 / 手工 / OSC / 批量重试关闭）
     ========================================================================== */
  const wfLog = (wf, user, act) => wf.log.push([wfNow(), user, act]);
  const wfNotify = (wf, text, who) => wfLog(wf, '系统', `机器人通知（${botOf(wf.rg)}）：${text}${who ? '，@ ' + who : ''}`);
  const syncUi = () => { buildNav(); wfPaint(); };

  function opApprove(wf, note) {
    const lv = wf.level, fl = flowOf(wf.rg);
    wf.approvals.push({lv, user: ME.login, at: wfNow(), note, action: 'approve'});
    wfLog(wf, ME.login, `审批通过（第 ${lv + 1} 级 ${LVNAME[fl[lv]]}）${note ? '：' + note : ''}`);
    if (lv + 1 < fl.length) { wf.level = lv + 1; wfNotify(wf, `[SQL上线申请] 工单 #${wf.id} 流转到第 ${lv + 2} 级「${LVNAME[fl[lv + 1]]}」待处理`, approverText(wf, lv + 1)); }
    else { wf.status = ST.pass; wfNotify(wf, `[SQL上线申请] 工单 #${wf.id} 审批通过，可执行`, wf.submitter + '（提交人）'); }
  }
  function opAbort(wf, kind, note) {
    const stage = wf.status === ST.wait ? 'review' : 'exec';
    wf.abort = {by: ME.login, at: wfNow(), stage, lv: wf.level, kind, note};
    if (kind === 'reject') wf.approvals.push({lv: wf.level, user: ME.login, at: wfNow(), note, action: 'reject'});
    wf.status = ST.abort; wf.schedule = null;
    wfLog(wf, ME.login, (kind === 'reject' ? '驳回 / 终止工单：' : '撤回工单：') + (note || '（未填写原因）'));
    wfNotify(wf, `[SQL上线申请] 工单 #${wf.id} ${kind === 'reject' ? '被驳回' : '被提交人终止'}`, wf.submitter);
  }
  function startRun(wf, executor, failAt) {
    wf.status = ST.run; wf.executor = executor; wf.startedAt = wfNow(); wf.endedAt = ''; wf.schedule = null; wf.res = []; wf.runIdx = 0; wf.failAt = failAt; wf.osc = null;
    wfNotify(wf, `[SQL上线申请] 工单 #${wf.id} 开始执行`, wf.submitter);
    if (wf.type === 'batch') { wf.bi.forEach(b => { if (b.st !== '已关闭') { b.st = '待执行'; b.msg = ''; b.retried = false; } }); batchStep(wf); }
    else runStep(wf);
  }
  function runStep(wf) {
    clearTimeout(wf._t);
    if (wf.status !== ST.run) return;
    const i = wf.runIdx, it = wf.chk.items[i];
    if (!it) return finishRun(wf); if (wf.redis) return redisRunStep(wf, i, it);
    wf.res[i] = {st: 'run'};
    if (it.osc && wf.executor === 'goinception') { wf.osc = {tool: 'gh-ost', idx: i, pct: 0, state: 'running', sha1: sha1of(wf.id, i), eta: '--', slow: false}; wfPaint(); wf._t = setTimeout(() => oscTick(wf), 500); }
    else { wfPaint(); wf._t = setTimeout(() => stmtDone(wf, i), 650); }
  }
  function oscTick(wf) {
    clearTimeout(wf._t);
    const o = wf.osc;
    if (wf.status !== ST.run || !o) return;
    if (o.state === 'paused') { wf._t = setTimeout(() => oscTick(wf), 1500); return; }
    o.pct = Math.min(100, o.pct + (o.slow ? 1 : 6 + Math.floor(Math.random() * 8)));
    const left = Math.round((100 - o.pct) * (o.slow ? 4 : 0.65) * 1.2); o.eta = `00:${pad2(Math.floor(left / 60))}:${pad2(left % 60)}`;
    if (o.pct >= 100) { o.state = 'done'; o.eta = '00:00:00'; return stmtDone(wf, o.idx); }
    wfPaint();
    wf._t = setTimeout(() => oscTick(wf), o.slow ? 4000 : 650);
  }
  function stmtDone(wf, i, why) {
    clearTimeout(wf._t);
    const it = wf.chk.items[i], fail = why === 'killed' || wf.failAt === i;
    if (fail) {
      wf.res[i] = {st: 'bad', rows: 0, sec: 0.004, msg: why === 'killed' ? 'gh-ost 被人工终止（临时表 _gho 已清理），原表未变更' : (wf.failMsg || defaultFail(it))};
      for (let j = i + 1; j < wf.chk.items.length; j++) wf.res[j] = {st: 'skip', msg: '前序语句失败, 未执行', rows: 0, sec: 0};
      return finishRun(wf);
    }
    wf.res[i] = {st: 'ok', rows: it.rows || 0, msg: it.osc ? (wf.executor === 'goinception' ? 'gh-ost 完成，已原子切换' : '原生执行 ALTER（未使用在线改表，期间持有元数据锁）') : '',
      sec: it.osc ? +(900 + hs(it.sql) % 900 + 0.4).toFixed(1) : +(0.012 + (hs(it.sql) % 900) / 1000).toFixed(3)};
    wf.runIdx = i + 1; runStep(wf);
  }
  function finishRun(wf) {
    clearTimeout(wf._t);
    const bad = wf.res.some(r => r.st === 'bad');
    wf.status = bad ? ST.exc : ST.done; wf.endedAt = wfNow();
    const k = wf.res.findIndex(r => r.st === 'bad');
    wfLog(wf, '系统', wf.redis ? redisFinishLog(wf, bad, k) : bad ? `执行有异常：第 ${k + 1} 条语句失败，其后语句未执行` : '执行完成，工单已正常结束'); if (wf.redis) redisAuditExec(wf);
    wfNotify(wf, bad ? `[SQL上线申请] 工单 #${wf.id} 执行有异常` : `[SQL上线申请] 工单 #${wf.id} 已正常结束`, wf.submitter + '、DBA 组');
    syncUi(); toast(bad ? `工单 #${wf.id} 执行有异常` : `工单 #${wf.id} 执行完成`);
  }
  /* 批量上线：每个实例各执行一遍；单个实例失败不影响其余 */
  function batchStep(wf) {
    clearTimeout(wf._t);
    if (wf.status !== ST.run) return;
    const i = wf.bi.findIndex(b => b.st === '待执行');
    if (i < 0) return recomputeBatch(wf);
    wf.bi[i].st = '执行中'; wfPaint();
    wf._t = setTimeout(() => {
      const b = wf.bi[i], inst = wfInst(b.inst);
      if (inst.flaky && !b.retried) { b.st = '失败'; b.msg = `连接超时：dial tcp ${inst.host}:${inst.port}: i/o timeout`; b.sec = 30.0; }
      else { b.st = '成功'; b.msg = b.retried ? '重试成功' : `影响 ${wf.chk.items[0] ? (wf.chk.items[0].rows || 0) : 0} 行`; b.sec = +(0.4 + (hs(b.inst + wf.sql) % 30) / 10).toFixed(1); }
      batchStep(wf);
    }, 800);
  }
  function recomputeBatch(wf) {
    const b = wf.bi, fail = b.filter(x => x.st === '失败').length, ok = b.filter(x => x.st === '成功').length;
    if (b.some(x => x.st === '待执行' || x.st === '执行中')) { wf.status = ST.run; return; }
    wf.status = (!fail && ok) ? ST.done : ST.exc; wf.endedAt = wfNow();
    wfLog(wf, '系统', `批量执行汇总：共 ${b.length} 个实例，成功 ${ok}，失败 ${fail}，已关闭 ${b.filter(x => x.st === '已关闭').length}` + (fail ? '。已有实例的变更已生效，失败实例与其余实例状态不一致，请人工核对' : ''));
    wfNotify(wf, wf.status === ST.done ? `[SQL上线申请] 工单 #${wf.id} 已正常结束` : `[SQL上线申请] 工单 #${wf.id} 执行有异常（部分实例失败）`, wf.submitter);
    syncUi(); toast(wf.status === ST.done ? `批量工单 #${wf.id} 执行完成` : `批量工单 #${wf.id} 有实例失败`);
  }
  function batchOp(wf, i, op) {
    const b = wf.bi[i], name = wfInst(b.inst).name;
    if (op === 'retry') {
      b.st = '待执行'; b.retried = true; b.msg = ''; wf.status = ST.run; wf.endedAt = '';
      wfLog(wf, ME.login, `重试实例 ${name}（重新执行整段 SQL，并替换其原有结果）`); batchStep(wf);
    } else {
      b.st = '已关闭'; b.msg = '已由 ' + ME.login + ' 关闭，不再计入工单成败'; wfLog(wf, ME.login, `关闭实例 ${name}（不再计入工单成败，也不能再重试）`);
      recomputeBatch(wf);
    }
  }

  /* ============================================================================
     通用展示片段
     ========================================================================== */
  const lvTag = l => l === 'bad' ? '<span class="tag bad">错误</span>' : l === 'warn' ? '<span class="tag warn">警告</span>' : '<span class="tag ok">通过</span>';
  const instName = id => esc(wfInst(id).name);
  const wfTarget = wf => wf.type === 'batch' ? `${instName(wf.inst)} 等 ${wf.insts.length} 个实例` : instName(wf.inst);
  const wfLink = (id, t) => `<a href="#sqlworkflow/detail/${id}">${t || '#' + id}</a>`;
  const lvText = wf => { const fl = flowOf(wf.rg); return `第 ${wf.level + 1} / ${fl.length} 级 · ${LVNAME[fl[wf.level]]}`; };
  function lvCell(wf) {
    if (wf.status === ST.wait) return `<span class="tag warn">${lvText(wf)}</span><div class="small muted">${esc(approverList(wf, wf.level).map(u => u.login).join('、') || '仅超级用户可审')}</div>`;
    if (wf.status === ST.auto) return '<span class="muted">系统自动驳回</span>';
    if (wf.status === ST.abort) return `<span class="muted">第 ${wf.abort ? wf.abort.lv + 1 : wf.level + 1} 级终止</span>`;
    return `<span class="muted">✓ ${flowOf(wf.rg).length} 级审批完成</span>`;
  }
  const btnOff = (label, why, cls = '') => `<button class="btn ${cls}" disabled title="${esc(why)}">${label}</button>`;
  /* 带范围条件的权限按钮：没有权限码 → B() 置灰带锁；有码但不在范围内 → 置灰并说明原因 */
  const gate = (code, label, ok, why, o = {}) => (!can(code) || ok) ? B(code, label, o) : btnOff(label, why, o.cls);

  /* ============================================================================
     列表页
     ========================================================================== */
  const WS = {tab: 'all', status: '', inst: '', rg: '', type: '', kw: '', sort: 'created', dir: -1, page: 1};
  const PAGE_SIZE = 10;
  const tabMatch = w => WS.tab === 'mine' ? w.submitter === ME.login : WS.tab === 'approve' ? canApproveFor(w, ME) : WS.tab === 'exec' ? ((w.status === ST.pass || w.status === ST.timing) && !!execCode(w)) : true;
  function filtered() {
    const kw = WS.kw.trim().toLowerCase();
    const list = visibleWfs().filter(w => tabMatch(w) && (!WS.status || w.status === WS.status) && (!WS.inst || w.insts.includes(+WS.inst)) && (!WS.rg || w.rg === +WS.rg) && (!WS.type || w.type === WS.type)
      && (!kw || [w.name, w.submitter, '#' + w.id, String(w.id), w.db, wfInst(w.inst).name].some(x => String(x).toLowerCase().includes(kw))));
    const k = WS.sort, d = WS.dir;
    list.sort((a, b) => k === 'id' ? (a.id - b.id) * d : k === 'status' ? (ST_LIST.indexOf(a.status) - ST_LIST.indexOf(b.status)) * d || b.id - a.id : (a.created < b.created ? -1 : a.created > b.created ? 1 : a.id - b.id) * d);
    return list;
  }
  const sortHead = (k, t) => `<a data-on="wfSort" data-v="${k}" style="color:inherit">${t}${WS.sort === k ? (WS.dir < 0 ? ' ▼' : ' ▲') : ' ↕'}</a>`;
  function listTable() {
    const all = filtered(), pages = Math.max(1, Math.ceil(all.length / PAGE_SIZE));
    if (WS.page > pages) WS.page = pages;
    const rows = all.slice((WS.page - 1) * PAGE_SIZE, WS.page * PAGE_SIZE);
    const ops = w => `<div class="ops">${wfLink(w.id, '详情')}${canApproveFor(w, ME) ? wfLink(w.id, '<b>审批</b>') : ''}${(w.status === ST.pass || w.status === ST.timing) && execCode(w) ? wfLink(w.id, '<b>执行</b>') : ''}</div>`;
    const html = tbl(['工单号', sortHead('id', '名称'), '实例 / 库', '提交人', '资源组', sortHead('status', '状态'), '当前审批级', sortHead('created', '创建时间'), '操作'],
      rows.map(w => [
        `<b>${wfLink(w.id)}</b>`,
        `<div class="ellipsis" style="max-width:300px" title="${esc(w.name)}">${esc(w.name)}</div><div class="small muted">${w.sqlType}${w.type === 'batch' ? ` <span class="tag brand">批量 ${w.insts.length}</span>` : ''}${w.chk.osc ? ' <span class="tag info">OSC</span>' : ''}${w.skip ? ' <span class="tag warn">跳过检查</span>' : ''}${w.status === ST.timing ? ` <span class="small">⏱ ${esc(w.schedule)}</span>` : ''}</div>`,
        `<div class="nowrap">${wfTarget(w)}</div><div class="small muted mono">${esc(w.db)}</div>`,
        esc(w.submitter), `<span class="nowrap">${esc(rgName(w.rg))}</span>`, stHtml(w.status), lvCell(w), `<span class="nowrap">${esc(w.created)}</span>`, ops(w)],
      ), {attrs: (r, i) => ` class="click" data-on="wfOpen" data-id="${rows[i].id}"`});
    const pg = pages > 1 ? `<div class="pager"><span>共 ${all.length} 条</span>${Array.from({length: pages}, (_, i) => `<a data-on="wfPage" data-v="${i + 1}" class="${i + 1 === WS.page ? 'on' : ''}">${i + 1}</a>`).join('')}</div>` : `<div class="pager"><span>共 ${all.length} 条</span></div>`;
    return html + pg;
  }
  function listBody() {
    const vis = visibleWfs(), n = s => vis.filter(w => w.status === s).length;
    const mineN = vis.filter(w => w.submitter === ME.login).length, apN = vis.filter(w => canApproveFor(w, ME)).length, exN = vis.filter(w => (w.status === ST.pass || w.status === ST.timing) && execCode(w)).length;
    const insts = [...new Set(vis.flatMap(w => w.insts))].map(id => wfInst(id)), rgs = [...new Set(vis.map(w => w.rg))];
    const tabsDef = [['all', '全部', vis.length], ['mine', '我提交的', mineN], ['approve', '待我审批', apN], ['exec', '待我执行', exN]];
    return `
    <div class="grid" style="grid-template-columns:repeat(auto-fit,minmax(128px,1fr));gap:10px;margin-bottom:14px">${ST_LIST.map(s => `
      <div class="card kpi" data-on="wfKpi" data-v="${s}" style="padding:10px 12px;cursor:pointer;${WS.status === s ? 'border-color:var(--brand);background:var(--brand-soft)' : ''}"><div class="l">${ST_ICO[s]} ${s}</div><div class="v">${n(s)}</div></div>`).join('')}</div>
    <div class="card">
      <div class="tabs">${tabsDef.map(([id, t, c]) => `<a data-on="wfTab" data-v="${id}" class="${WS.tab === id ? 'on' : ''}" style="cursor:pointer">${t}${c ? ` <span class="tag ${id === 'approve' || id === 'exec' ? 'warn' : ''}">${c}</span>` : ''}</a>`).join('')}</div>
      <div class="toolbar">
        <select data-onchange="wfFilter" data-k="status"><option value="">全部状态</option>${ST_LIST.map(s => `<option ${WS.status === s ? 'selected' : ''}>${s}</option>`).join('')}</select>
        <select data-onchange="wfFilter" data-k="inst"><option value="">全部实例</option>${insts.map(i => `<option value="${i.id}" ${WS.inst == i.id ? 'selected' : ''}>${esc(i.name)}</option>`).join('')}</select>
        <select data-onchange="wfFilter" data-k="rg"><option value="">全部资源组</option>${rgs.map(r => `<option value="${r}" ${WS.rg == r ? 'selected' : ''}>${esc(rgName(r))}</option>`).join('')}</select>
        <select data-onchange="wfFilter" data-k="type"><option value="">单实例 + 批量</option><option value="sql" ${WS.type === 'sql' ? 'selected' : ''}>仅单实例工单</option><option value="batch" ${WS.type === 'batch' ? 'selected' : ''}>仅批量上线工单</option></select>
        <input type="search" id="wfKw" placeholder="搜索工单号 / 名称 / 提交人" value="${esc(WS.kw)}" data-oninput="wfKw" style="min-width:220px">
        <span class="sp"></span>
        ${B('sqlworkflow.submit', '＋ 提交工单', {cls: 'primary', data: {m: 'go'}})}
      </div>
      <div id="wfTblBox">${listTable()}</div>
    </div>`;
  }
  ON.wfTab = d => { WS.tab = d.v; WS.page = 1; wfPaint(true); };
  ON.wfKpi = d => { WS.status = WS.status === d.v ? '' : d.v; WS.page = 1; wfPaint(true); };
  ON.wfFilter = (d, el) => { WS[d.k] = el.value; WS.page = 1; wfPaint(true); };
  ON.wfKw = (d, el) => { WS.kw = el.value; WS.page = 1; const b = $('#wfTblBox'); if (b) b.innerHTML = listTable(); };
  ON.wfSort = d => { if (WS.sort === d.v) WS.dir = -WS.dir; else { WS.sort = d.v; WS.dir = -1; } const b = $('#wfTblBox'); if (b) b.innerHTML = listTable(); };
  ON.wfPage = d => { WS.page = +d.v; const b = $('#wfTblBox'); if (b) b.innerHTML = listTable(); };
  ON.wfOpen = d => go('sqlworkflow', 'detail/' + d.id);

  /* ============================================================================
     提交页
     ========================================================================== */
  const NS = {inst: 0, rg: 0, db: '', name: '', url: '', from: '', to: '', backup: true, batch: false, binsts: [], skip: false, sql: '', check: null};
  const SAMPLES = {
    order: [['ALTER 大表加字段（提示 OSC）', "ALTER TABLE orders\n  ADD COLUMN pay_channel varchar(16) NOT NULL DEFAULT '' COMMENT '支付渠道';"], ['建表缺主键', "CREATE TABLE order_tag (\n  order_id bigint unsigned NOT NULL COMMENT '订单ID',\n  tag varchar(32) NOT NULL COMMENT '标签'\n) ENGINE=InnoDB COMMENT='订单标签';"],
      ['UPDATE 无 WHERE', 'UPDATE orders SET status = 4;'], ['DROP 高危', 'DROP TABLE order_items;'], ['无索引的 DELETE', "DELETE FROM order_log WHERE created_at < '2026-01-01 00:00:00';"],
      ['正常 DML', "UPDATE orders SET status = 3 WHERE order_no = 'E20261007001';"],
      ['综合示例（多条）', "ALTER TABLE orders ADD INDEX amount_idx (amount);\nALTER TABLE orders ADD COLUMN remark varchar(255);\nCREATE TABLE order_coupon (\n  id bigint unsigned NOT NULL AUTO_INCREMENT COMMENT '主键',\n  order_id bigint unsigned NOT NULL COMMENT '订单ID',\n  coupon_code varchar(32) NOT NULL COMMENT '券码',\n  PRIMARY KEY (id),\n  KEY idx_order (order_id)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='订单优惠券';"]],
    pay: [['加索引（大表）', 'ALTER TABLE payments ADD INDEX idx_order_status (order_no, status);'], ['建表缺主键', "CREATE TABLE pay_notify_log (\n  pay_no varchar(40) NOT NULL COMMENT '支付流水',\n  body text COMMENT '回调报文'\n) ENGINE=InnoDB COMMENT='支付回调日志';"],
      ['UPDATE 无 WHERE', 'UPDATE refunds SET amount = 0;'], ['DROP 高危', 'DROP TABLE bank_cards;'], ['INSERT 无列名', "INSERT INTO refunds VALUES (1, 'RF20261007001', 'P20261006001', 12.00, '2026-10-07 10:00:00');"],
      ['正常 DML', "UPDATE payments SET status = 2 WHERE pay_no = 'P20261007001';"]],
    user: [['加索引（大表）', 'ALTER TABLE login_log ADD INDEX idx_ua (ua(64));'], ['加字段缺默认值', "ALTER TABLE users ADD COLUMN last_login_ip varchar(45) NOT NULL COMMENT '最后登录IP';"],
      ['DELETE 无 WHERE', 'DELETE FROM user_profile;'], ['TRUNCATE 高危', 'TRUNCATE TABLE login_log;'], ['正常 DML', "UPDATE users SET level = 2 WHERE username = 'lin_xiaoyu';"]],
    mssql: [['UPDATE 无 WHERE', "UPDATE dbo.dim_customer SET region = N'华东';"], ['SELECT * / INSERT 无列名', "SELECT * FROM dbo.fact_sales;\nINSERT INTO dbo.dim_customer VALUES (1, N'测试客户', '13800000000', N'华东');"],
      ['建表缺主键', "CREATE TABLE dbo.tmp_sales_snapshot (\n  sale_id bigint NOT NULL,\n  amount decimal(14,2) NULL\n);"], ['危险语句', "EXEC sp_configure 'xp_cmdshell', 1;\nRECONFIGURE;"],
      ['正常 DML', "UPDATE dbo.dim_customer SET region = N'未知' WHERE region IS NULL;"], ['在线建索引', 'CREATE NONCLUSTERED INDEX IX_fact_sales_customer ON dbo.fact_sales (customer_key) INCLUDE (amount) WITH (ONLINE = ON);']],
  };
  SAMPLES.redis = [['清理缓存键（通过）', 'DEL cache:order:1001 cache:order:1002\nUNLINK cache:order:detail:1001'], ['SET 不带过期（警告）', 'SET cache:order:summary ok'], ['设置会话键过期', 'EXPIRE session:u1001 3600'],
    ['修复用户哈希字段', 'HSET user:1001 status 1 level 3\nHDEL user:1001 temp_flag'], ['键不匹配规则（拒绝）', 'DEL order:1001'], ['硬禁止命令（拒绝）', 'FLUSHDB'],
    ['综合示例（多行）', 'DEL cache:a cache:b\n\nSET cache:x 1 EX 60\nGET cache:x\nKEYS *\nRENAME cache:old cache:new\nDEL "cache:unclosed']];
  const submitInsts = () => visibleInstances(true).filter(i => i.role !== 'slave' && !isReadView(i));
  const visibleExtra = () => EXTRA_MSSQL.filter(i => ME.super || can('query.admin_run') || i.rgs.some(r => ME.rgs.includes(r)));
  const rgsOf = inst => inst.rgs.filter(r => ME.super || ME.rgs.includes(r));
  const chkKey = () => `${NS.inst}|${NS.db}|${NS.batch ? NS.binsts.join(',') : ''}`;
  function nsFix() {
    const all = submitInsts().concat(NS.batch ? visibleExtra() : []);
    let inst = all.find(i => i.id === +NS.inst) || submitInsts()[0];
    if (!inst) return null;
    NS.inst = inst.id;
    const rgs = rgsOf(inst); if (!rgs.includes(NS.rg)) NS.rg = rgs[0] || 0;
    const dbs = dbNames(inst); if (!dbs.includes(NS.db)) NS.db = dbs[0] || '';
    if (!isMs(inst) || !can('sqlworkflow.submit_batch')) { NS.batch = false; NS.binsts = []; }
    if (NS.batch && !NS.binsts.includes(NS.inst)) NS.binsts = [NS.inst];
    if (NS.skip && (!can('sqlworkflow.skip_check') || isRds(inst))) NS.skip = false;
    return inst;
  }
  function flowPreview() {
    const fl = flowOf(NS.rg), fake = {rg: NS.rg, submitter: ME.login};
    return `<div class="row" style="gap:6px">${fl.map((g, i) => { const m = approverList(fake, i).map(u => u.login); return `<span class="tag ${m.length ? 'brand' : 'bad'}">第 ${i + 1} 级 · ${LVNAME[g]}：${m.length ? esc(m.join(' / ')) : '无人可审'}</span>${i + 1 < fl.length ? '<span class="muted">→</span>' : ''}`; }).join('')}</div>`;
  }
  function precheck() {
    const first = wfInst(NS.binsts[0] || NS.inst);
    return NS.binsts.map(id => { const i = wfInst(id); let why = ''; if (i.status !== 'ok') why = '无法连接实例'; else if (!dbNames(i).some(d => d.toLowerCase() === NS.db.toLowerCase())) why = `没有找到数据库 ${NS.db}`; return {inst: i, ok: !why, why}; });
  }
  function blockReasons() {
    const r = [], inst = wfInst(NS.inst), re = /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/;
    if (!NS.name.trim()) r.push('请填写工单名称'); else if (NS.name.trim().length > 50) r.push('工单名称不能超过 50 字');
    if (!NS.sql.trim()) r.push(isRds(inst) ? '请输入 Redis 命令' : '请输入 SQL 内容');
    if (NS.batch && NS.binsts.length < 2) r.push('批量上线请至少选择 2 个实例');
    if ((NS.from && !re.test(NS.from.trim())) || (NS.to && !re.test(NS.to.trim()))) r.push('可执行时间格式应为 YYYY-MM-DD HH:mm');
    else if (NS.from && NS.to && NS.to.trim() <= NS.from.trim()) r.push('可执行时间窗的结束时间必须晚于开始时间');
    if (!NS.skip && NS.sql.trim()) {
      if (!NS.check || NS.check.running) r.push('请先点击「检查」');
      else if (NS.check.sql !== NS.sql || NS.check.key !== chkKey()) r.push(isRds(inst) ? '命令或目标已修改，请重新检查' : 'SQL 或目标已修改，请重新检查');
      else if (NS.check.res.err) r.push(isRds(inst) ? `有 ${NS.check.res.err} 行被拒绝，修复后才能提交` : `检查结果有 ${NS.check.res.err} 条错误，修复后才能提交`);
      else if (NS.batch && precheck().some(p => !p.ok)) r.push('有实例预检失败，请取消勾选或先处理');
    }
    return r;
  }
  function chkHtml() {
    const c = NS.check;
    if (!c) return empty('尚未检查', '⌕');
    const res = c.res, rows = res.items.slice(0, c.shown), ms = isMs(wfInst(NS.inst)); if (res.redis) return redisChkHtml(c, res);
    const head = c.running ? `<span class="muted">goInception 检查中… ${c.shown} / ${res.items.length}</span>` : `类型：<b>${res.type}</b>　警告 <b style="color:var(--warn)">${res.warn}</b>　错误 <b style="color:var(--bad)">${res.err}</b>${res.osc ? '　<span class="tag info">含大表 DDL · 将走在线改表 OSC</span>' : ''}`;
    return `<div class="row between" style="margin-bottom:8px"><div>${head}</div><span class="small muted">${ms ? '已由 SQL Server 语法解析与上线规则检查' : '已由 goInception 检查'}</span></div>
      ${tbl(['#', '阶段', '级别', '信息 / 建议', 'SQL', '影响行数'], rows.map(it => [it.n, `<span class="small">${it.stage}</span>`, lvTag(it.lvl),
        it.msgs.length ? `<ul style="margin:0;padding-left:16px">${it.msgs.map(m => `<li class="small" style="color:${m.lvl === 'bad' ? 'var(--bad)' : 'var(--warn)'}">${esc(m.text)}</li>`).join('')}</ul>` : '<span class="muted">—</span>',
        `<div class="mono small" style="max-width:360px;white-space:pre-wrap;word-break:break-all">${esc(it.sql.length > 220 ? it.sql.slice(0, 220) + ' …' : it.sql)}</div>`, `<span class="mono">${it.rows ? fmtN(it.rows) : 0}</span>`]), {attrs: (r, i) => rows[i].lvl === 'bad' ? ' style="background:var(--bad-soft)"' : rows[i].lvl === 'warn' ? ' style="background:var(--warn-soft)"' : ''})}`;
  }
  function newBody() {
    const inst = nsFix();
    if (!inst) return `<div class="card">${empty('你所在的资源组没有可提交工单的实例（从库 / 只读视图实例不能提交变更）。', '∅')}</div>`;
    const rgs = rgsOf(inst), isM = isMs(inst), canBatch = isM && can('sqlworkflow.submit_batch');
    const samples = isRds(inst) ? SAMPLES.redis : SAMPLES[inst.schema] || [];
    const reasons = blockReasons(), insts = submitInsts();
    const backupOff = isM || NS.batch || NS.skip, rds = isRds(inst);
    const extra = NS.batch ? visibleExtra() : [], batchList = insts.filter(isMs).concat(extra);
    return `
    <div class="row" style="margin-bottom:12px"><a class="btn" href="#sqlworkflow">← 返回列表</a></div>
    <div class="card"><h2>工单信息</h2>
      <div class="form">
        ${fld('工单名称', `<input type="text" data-oninput="wfF" data-k="name" value="${esc(NS.name)}" placeholder="如：订单表增加字段" maxlength="60">`, {req: true})}
        ${fld('目标实例', `<select data-onchange="wfSel" data-k="inst">${insts.concat(NS.batch ? [] : []).map(i => `<option value="${i.id}" ${i.id === NS.inst || (NS.batch && NS.binsts[0] === i.id && i.id === NS.inst) ? 'selected' : ''}>${esc(i.name)}（${i.type}${i.status === 'bad' ? ' · 连接异常' : ''}）</option>`).join('')}${EXTRA_MSSQL.some(e => e.id === NS.inst) ? `<option value="${NS.inst}" selected>${esc(inst.name)}（MsSQL）</option>` : ''}</select>`, {req: true})}
        ${fld('资源组', `<select data-onchange="wfSel" data-k="rg">${rgs.map(r => `<option value="${r}" ${r === NS.rg ? 'selected' : ''}>${esc(rgName(r))}</option>`).join('')}</select>`, {req: true})}
        ${fld('审批流程', flowPreview())}
        ${fld('数据库', `<select data-onchange="wfSel" data-k="db">${dbNames(inst).map(d => `<option${rds ? ` value="${esc(d)}"` : ''} ${d === NS.db ? 'selected' : ''}>${rds ? 'db' + esc(d) : esc(d)}</option>`).join('')}</select>`, {req: true})}
        ${canBatch ? fld('批量上线', `<label class="sw"><input type="checkbox" data-onchange="wfBatch" ${NS.batch ? 'checked' : ''}><i></i>批量提交</label>`) : ''}
        ${NS.batch ? `<label>选择实例</label><div><div class="row" style="margin-bottom:6px"><button class="btn sm" data-on="wfBAll">全选</button><button class="btn sm" data-on="wfBClr">清空</button><span class="muted small">已选 ${NS.binsts.length} / 100</span></div>
          <div class="row" style="gap:14px">${batchList.map(i => `<label class="chk"><input type="checkbox" data-onchange="wfBInst" data-id="${i.id}" ${NS.binsts.includes(i.id) ? 'checked' : ''}>${esc(i.name)}</label>`).join('')}</div></div>` : ''}
        ${fld('需求链接（可不填）', `<input type="text" data-oninput="wfF" data-k="url" value="${esc(NS.url)}" placeholder="https://">`)}
        ${fld('可执行时间（可不填）', `<div class="row"><input type="text" data-oninput="wfF" data-k="from" value="${esc(NS.from)}" placeholder="开始 YYYY-MM-DD HH:mm" style="width:200px"> ～ <input type="text" data-oninput="wfF" data-k="to" value="${esc(NS.to)}" placeholder="结束 YYYY-MM-DD HH:mm" style="width:200px"></div>`)}
        ${rds ? '' : fld('执行前备份', `<label class="sw"><input type="checkbox" data-onchange="wfChkBox" data-k="backup" ${NS.backup && !backupOff ? 'checked' : ''} ${backupOff ? 'disabled' : ''}><i></i>执行前备份，以便生成回滚语句</label>`)}
        ${can('sqlworkflow.skip_check') && !NS.batch && !rds ? fld('跳过语法检查', `<label class="sw"><input type="checkbox" data-onchange="wfSkip" ${NS.skip ? 'checked' : ''}><i></i>仅超级用户</label>`) : ''}
      </div>
    </div>
    <div class="card" style="margin-top:14px"><h2>${rds ? 'Redis 命令' : 'SQL 内容'}
      <span class="right">${samples.map(([t], i) => `<button class="btn sm" data-on="wfSample" data-i="${i}">${esc(t)}</button>`).join('')}</span></h2>
      ${rds ? redisNote() : ''}<textarea class="sql-ed" id="wfSql" rows="10" spellcheck="false" data-oninput="wfSql" placeholder="${rds ? '每行一条命令' : '-- 输入变更 SQL'}">${esc(NS.sql)}</textarea>
      <div class="row between" style="margin-top:10px">
        <div class="small" id="wfStale" style="color:var(--warn)" ${NS.check && !NS.check.running && NS.check.sql !== NS.sql ? '' : 'hidden'}>⚠ ${rds ? '命令' : 'SQL'} 已修改，需要重新检查</div>
        <div class="row"><button class="btn" data-on="wfCheck">⌕ ${rds ? '检查命令' : '检查 SQL'}</button>${B(NS.batch ? 'sqlworkflow.submit_batch' : 'sqlworkflow.submit', '提交工单', {cls: 'primary', data: {m: 'do'}})}</div>
      </div>
      <div class="small" style="margin-top:8px">${reasons.length ? `<span class="muted">提交前还需：</span>${reasons.map(x => `<span class="tag warn" style="margin-right:4px">${esc(x)}</span>`).join('')}` : '<span class="tag ok">✓ 可以提交</span>'}${NS.skip ? ' <span class="tag bad">已跳过语法检查：只能原生执行</span>' : ''}</div>
    </div>
    ${NS.batch && NS.check && !NS.check.running ? `<div class="card" style="margin-top:14px"><h2>实例预检</h2>${tbl(['实例', '状态', '说明'], precheck().map(p => [esc(p.inst.name), p.ok ? '<span class="tag ok">可用</span>' : '<span class="tag bad">不可用</span>', esc(p.why)]))}</div>` : ''}
    <div class="card" style="margin-top:14px"><h2>检查结果</h2><div id="wfChkBox">${NS.skip ? notice('已勾选「跳过语法检查」：不逐条检查，只能原生执行。', 'warn', '⚠') : chkHtml()}</div></div>`;
  }
  ON.wfF = (d, el) => { NS[d.k] = el.value; };
  ON.wfSql = (d, el) => { NS.sql = el.value; const s = $('#wfStale'); if (s) s.hidden = !(NS.check && !NS.check.running && NS.check.sql !== NS.sql); };
  ON.wfSel = (d, el) => {
    if (d.k === 'inst') { NS.inst = +el.value; NS.rg = 0; NS.db = ''; if (NS.batch) NS.binsts = [NS.inst]; }
    else if (d.k === 'rg') NS.rg = +el.value; else NS.db = el.value;
    NS.check = null; wfPaint(true);
  };
  ON.wfChkBox = (d, el) => { NS[d.k] = el.checked; };
  ON.wfBatch = (d, el) => { NS.batch = el.checked; NS.binsts = el.checked ? [NS.inst] : []; if (el.checked) NS.skip = false; NS.check = null; wfPaint(true); };
  ON.wfBInst = (d, el) => {
    const id = +d.id; if (el.checked) { if (!NS.binsts.includes(id)) NS.binsts.push(id); } else NS.binsts = NS.binsts.filter(x => x !== id);
    if (NS.binsts.length) NS.inst = NS.binsts[0];
    NS.check = null; wfPaint(true);
  };
  ON.wfBAll = () => { NS.binsts = submitInsts().filter(isMs).concat(visibleExtra()).map(i => i.id); NS.inst = NS.binsts[0]; NS.check = null; wfPaint(true); };
  ON.wfBClr = () => { NS.binsts = []; NS.check = null; wfPaint(true); };
  ON.wfSkip = (d, el) => {
    if (!el.checked) { NS.skip = false; return wfPaint(true); }
    wfPaint(true);
    risky('sqlworkflow.skip_check', '跳过语法检查', '跳过检查后，系统<b>不会</b>发现缺主键、无 WHERE、表不存在等问题，该工单<b>只能原生执行</b>（不备份、无回滚语句、无在线改表）。', '', () => { NS.skip = true; NS.check = null; audit('sqlworkflow.skip_check', '提交页勾选「跳过语法检查」', '成功', wfInst(NS.inst).name); wfPaint(true); });
  };
  ON.wfSample = d => { const s = (SAMPLES[wfInst(NS.inst).schema] || [])[+d.i]; if (!s) return; NS.sql = s[1]; if (!NS.name) NS.name = s[0].replace(/（.*?）/g, ''); NS.check = null; wfPaint(true); };
  ON.wfCheck = () => {
    if (!NS.sql.trim()) return toast(isRds(wfInst(NS.inst)) ? '请先输入 Redis 命令' : '请先输入 SQL');
    const inst = wfInst(NS.inst), res = wfCheck(NS.sql, inst, NS.db);
    if (res.error) return toast(res.error); if (!res.items.length) return toast('没有识别到可检查的语句'); if (res.redis) redisAudit('检查', inst, res);
    NS.check = {sql: NS.sql, key: chkKey(), res, shown: 0, running: true};
    wfPaint(true);
    const step = () => {
      if (ROUTE.page !== 'sqlworkflow' || !/^new/.test(ROUTE.sub) || !NS.check || NS.check.res !== res) return;
      NS.check.shown = Math.min(res.items.length, NS.check.shown + (res.redis ? Math.ceil(res.items.length / 15) : 1)); const box = $('#wfChkBox'); if (box) box.innerHTML = chkHtml();
      if (NS.check.shown < res.items.length) setTimeout(step, 260); else { NS.check.running = false; wfPaint(true); toast(res.err ? `检查完成：${res.err} 条错误，${res.warn} 条警告` : res.warn ? `检查通过（${res.warn} 条警告）` : '检查通过'); }
    };
    setTimeout(step, 350);
  };
  function doSubmit() {
    const rs = blockReasons(); if (rs.length) return toast(rs[0]);
    const inst = wfInst(NS.inst), batch = NS.batch, sql = NS.sql.trim(); if (isRds(inst)) return redisSubmit(inst, NS.sql);
    const id = Math.max(...WF.map(w => w.id)) + 1;
    const wf = {id, name: NS.name.trim(), type: batch ? 'batch' : 'sql', inst: NS.inst, insts: batch ? NS.binsts.slice() : [NS.inst], db: NS.db, submitter: ME.login, rg: NS.rg, created: wfNow(), status: ST.wait, level: 0, approvals: [], log: [],
      window: (NS.from || NS.to) ? {from: NS.from.trim(), to: NS.to.trim()} : null, schedule: null, scheduleBy: '', backup: NS.backup && !isMs(inst) && !batch && !NS.skip, executor: '', res: [], osc: null,
      bi: batch ? NS.binsts.map(i => ({inst: i, st: '待执行', msg: '', sec: 0})) : null, skip: NS.skip, manual: false, url: NS.url.trim(), startedAt: '', endedAt: '', abort: null, sql};
    wf.chk = NS.skip ? wfNoCheck(sql, inst) : wfCheck(sql, inst, NS.db); wf.sqlType = wf.chk.type;
    const bad = !NS.skip && wf.chk.err > 0;       // 服务端提交时重新检查，不信任页面传来的结果
    wf.log.push([wf.created, ME.login, `提交工单${batch ? `（批量上线 ${wf.insts.length} 个实例）` : ''}${NS.skip ? '，已跳过语法检查' : ''}`]);
    if (bad) { wf.status = ST.auto; wf.log.push([wf.created, '系统', '服务端重新检查：存在错误级别的检查结果，自动审批不通过']); }
    else { wf.log.push([wf.created, '系统', `机器人通知（${botOf(wf.rg)}）：[SQL上线申请] 新的工单申请 #${id}，@ ${approverText(wf, 0)}`]); }
    WF.unshift(wf);
    audit(batch ? 'sqlworkflow.submit_batch' : 'sqlworkflow.submit', `提交工单 #${id}「${wf.name}」${batch ? `（批量 ${wf.insts.length} 实例）` : ''}`, '成功', `${inst.name} / ${wf.db}`);
    toast(bad ? `工单 #${id} 已提交，但被系统自动驳回` : `工单 #${id} 已提交，等待「${LVNAME[flowOf(wf.rg)[0]]}」审批`);
    Object.assign(NS, {name: '', url: '', from: '', to: '', sql: '', check: null, skip: false, batch: false, binsts: []});
    buildNav(); go('sqlworkflow', 'detail/' + id);
  }
  ACTIONS['sqlworkflow.submit'] = ACTIONS['sqlworkflow.submit_batch'] = d => {
    if (d.m === 'go') return go('sqlworkflow', 'new');
    if (d.m === 'copy') {
      const w = WF.find(x => x.id == d.id); if (!w) return;
      Object.assign(NS, {inst: w.inst, rg: w.rg, db: w.db, name: w.name + '（重新提交）', url: w.url || '', from: '', to: '', sql: w.sql, check: null, skip: false, batch: w.type === 'batch', binsts: w.type === 'batch' ? w.insts.slice() : [], backup: w.backup});
      return go('sqlworkflow', 'new');
    }
    doSubmit();
  };

  /* ============================================================================
     详情页
     ========================================================================== */
  /* ============================================================================
     Redis 工单（命令模式）：检查 / 提交 / 执行 / 详情。规则与引擎在 p_redisrule.js（window.RedisRules，加载顺序在本文件之后，运行时才读取）；
     规格：internal/redisrule 与 internal/sqlflow。MySQL / SQL Server 的路径完全不受影响：只在实例类型为 Redis 时进入这里。
     ========================================================================== */
  const EXEC_LABEL_REDIS = '原生执行';
  /* 一行命令的检查结果（形状与 MySQL 的检查项兼容，工单详情 / 列表的通用代码可直接使用）*/
  function rItem(n, text, lvl, status, message, keyCount, rule, before) {
    return {n, line: n, sql: text, msgs: message ? [{lvl: lvl === 'bad' ? 'bad' : 'warn', text: message, rule: ''}] : [], rows: keyCount, osc: false, kind: 'redis', tbl: '', lvl, stage: status, ddl: false, status, rule: rule || '', before: !!before};
  }
  function rTotals(items) { return {warn: items.filter(i => i.lvl === 'warn').length, err: items.filter(i => i.lvl === 'bad').length}; }
  /* 种子工单用：不依赖规则引擎，检查结果直接写死 */
  function redisSeedChk(o) {
    const items = o.rcmds.map(c => rItem(c.n, c.sql, c.warn ? 'warn' : 'ok', c.warn ? '警告' : '通过', c.warn || '', c.keys, c.rule, c.before));
    return Object.assign({items, ddl: false, type: 'Redis', osc: false, redis: true}, rTotals(items));
  }
  function redisSeeds() {
    const c1 = [
      {n: 1, sql: 'DEL cache:order:1001 cache:order:1002 cache:order:1003', keys: 3, rule: '清理缓存键'},
      {n: 2, sql: 'UNLINK cache:order:detail:1001', keys: 1, rule: '清理缓存键'},
      {n: 3, sql: 'HSET user:1001 status 1 level 3', keys: 1, rule: '修复用户哈希字段', before: true},
      {n: 4, sql: 'SET cache:order:summary:20261007 ok', keys: 1, rule: '写入缓存键', warn: 'SET 没有设置过期时间（EX / PX / EXAT / PXAT / KEEPTTL），键将永不过期'}];
    const c2 = [
      {n: 1, sql: 'DEL cache:promo:1001 cache:promo:1002', keys: 2, rule: '清理缓存键'},
      {n: 2, sql: 'EXPIRE session:u2001 3600', keys: 1, rule: '设置会话键过期'},
      {n: 3, sql: 'HDEL user:2001 temp_flag', keys: 1, rule: '修复用户哈希字段', before: true}];
    return [
      {id: 1044, name: '清理促销缓存并修正用户标记（Redis）', inst: 11, db: '0', submitter: 'rd_zhao', rg: 1, created: '2026-10-07 11:50', status: ST.wait, level: 0, redis: true, rcmds: c2, sql: c2.map(c => c.sql).join('\n')},
      {id: 1043, name: '清理订单缓存并修复用户哈希字段（Redis）', inst: 11, db: '0', submitter: 'rd_zhao', rg: 1, created: '2026-10-07 08:30', status: ST.done, level: 1, redis: true, executor: 'native', startedAt: '2026-10-07 09:40', endedAt: '2026-10-07 09:40',
        approvals: [A(0, 'tl_wang', '2026-10-07 08:50', ''), A(1, 'dba_li', '2026-10-07 09:20', '规则已核对，执行')], ev: [['2026-10-07 09:40', 'dba_li', '立即执行（原生执行：Redis 工单只支持原生执行；执行前按当时的规则整单复检通过）']],
        rcmds: c1, sql: c1.map(c => c.sql).join('\n'),
        res: [{st: 'ok', msg: '(integer) 3', rows: 3, sec: 0.002}, {st: 'ok', msg: '(integer) 1', rows: 1, sec: 0.001},
          {st: 'ok', msg: '(integer) 0', rows: 0, sec: 0.002, before: '键 "user:1001"：类型 hash，无过期时间，原字段值 "status"="0"，"level"="2"'},
          {st: 'ok', msg: 'OK（SET 没有设置过期时间（EX / PX / EXAT / PXAT / KEEPTTL），键将永不过期）', rows: 0, sec: 0.001}]}];
  }

  /* ---- 检查（提交页“检查命令”与提交时的服务端复检共用）---- */
  function redisCheck(sql, inst, db) {
    const RR = window.RedisRules, sp = RR.splitLines(sql);
    if (sp.error) return {items: [], error: sp.error, warn: 0, err: 0, type: 'Redis', osc: false, redis: true};
    const t = {instId: inst.id, db: parseInt(db, 10) || 0};
    const items = sp.lines.map(l => { const v = RR.evalLine(RR.rules, t, l.text); return rItem(l.no, l.text, ['ok', 'warn', 'bad'][v.level], v.status, v.message, v.keys.length, v.ruleName, v.recordBefore); });
    return Object.assign({items, ddl: false, type: 'Redis', osc: false, redis: true}, rTotals(items));
  }
  const rInfo = it => it.msgs.length ? it.msgs[0].text : it.rule ? `命中规则「${it.rule}」` : '';
  function redisChkHtml(c, res) {
    const rows = res.items.slice(0, c.shown);
    const head = c.running ? `<span class="muted">按 Redis 上线规则检查中… ${c.shown} / ${res.items.length}</span>` : `共 <b>${res.items.length}</b> 条命令　警告 <b style="color:var(--warn)">${res.warn}</b>　拒绝 <b style="color:var(--bad)">${res.err}</b>`;
    return `<div class="row between" style="margin-bottom:8px"><div>${head}</div><span class="small muted">已按 Redis 上线规则检查</span></div>
      ${!c.running && res.err ? notice(`有 <b>${res.err}</b> 行被拒绝，修改命令后才能提交。`, 'bad', '✕') : ''}
      ${tbl(['行', '命令', '涉及键数', '结果', '信息 / 建议'], rows.map(it => [it.n, `<div class="mono small" style="max-width:420px;white-space:pre-wrap;word-break:break-all">${esc(it.sql.length > 220 ? it.sql.slice(0, 220) + ' …' : it.sql)}</div>`, `<span class="mono">${it.rows}</span>`,
        `<span class="tag ${it.lvl === 'bad' ? 'bad' : it.lvl === 'warn' ? 'warn' : 'ok'}">${esc(it.status)}</span>`, `<span class="small" style="color:${it.lvl === 'bad' ? 'var(--bad)' : it.lvl === 'warn' ? 'var(--warn)' : 'inherit'}">${esc(rInfo(it))}</span>`]),
        {attrs: (r, i) => rows[i].lvl === 'bad' ? ' style="background:var(--bad-soft)"' : rows[i].lvl === 'warn' ? ' style="background:var(--warn-soft)"' : ''})}`;
  }
  /* 审计：Redis上线-检查 / Redis上线-提交检查（实例、库、条数、通过 / 警告 / 拒绝数、第一条拒绝原因）*/
  function redisAudit(kind, inst, res, result) {
    const ok = res.items.filter(i => i.lvl === 'ok').length, bad = res.items.find(i => i.lvl === 'bad');
    audit('sqlworkflow.submit', `Redis上线-${kind}：实例：${inst.name}，库：${NS.db}，共 ${res.items.length} 条命令：通过 ${ok}、警告 ${res.warn}、拒绝 ${res.err}${bad ? `，第 ${bad.n} 行：${bad.msgs[0].text.slice(0, 200)}` : ''}`, result || '成功', `${inst.name} / ${NS.db}`);
  }
  function redisNote() {
    const RR = window.RedisRules, on = RR ? RR.rules.filter(r => r.enabled).length : 0;
    return notice('<b>Redis 命令模式：</b>每行一条命令，遇错即停，<b>没有自动回滚</b>。', 'warn', '⚠')
      + (on ? '' : notice('<b>当前没有启用的 Redis 上线规则：</b>所有命令都会被拒绝。', 'bad', '✕'));
  }
  const redisDetailNote = () => notice('<b>Redis 工单：</b>Redis 没有事务回滚，已执行的命令无法自动撤销。', 'warn', '⚠');

  /* ---- 提交：服务端再检查一次（不信任页面的检查结果），有拒绝行直接失败，不产生工单，也不走“自动审批不通过” ---- */
  function redisSubmit(inst, sql) {
    const res = redisCheck(sql, inst, NS.db);
    if (res.error) return toast(res.error);
    redisAudit('提交检查', inst, res, res.err ? '拒绝' : '成功');
    if (res.err) {
      const bad = res.items.find(i => i.lvl === 'bad');
      NS.check = {sql: NS.sql, key: chkKey(), res, shown: res.items.length, running: false}; wfPaint(true);
      return toast(`服务端提交检查未通过：第 ${bad.n} 行被拒绝（${bad.msgs[0].text.slice(0, 40)}…），未创建工单`);
    }
    const id = Math.max(...WF.map(w => w.id)) + 1;
    const wf = {id, name: NS.name.trim(), type: 'sql', inst: NS.inst, insts: [NS.inst], db: NS.db, submitter: ME.login, rg: NS.rg, created: wfNow(), status: ST.wait, level: 0, approvals: [], log: [],
      window: (NS.from || NS.to) ? {from: NS.from.trim(), to: NS.to.trim()} : null, schedule: null, scheduleBy: '', backup: false, executor: '', res: [], osc: null, bi: null, skip: false, manual: false, url: NS.url.trim(),
      startedAt: '', endedAt: '', abort: null, sql: sql.replace(/\s+$/, ''), redis: true};
    wf.chk = res; wf.sqlType = res.type;
    wf.log.push([wf.created, ME.login, 'Redis 命令提交，服务端按当前规则复检通过'], [wf.created, '系统', `机器人通知（${botOf(wf.rg)}）：[SQL上线申请] 新的工单申请 #${id}，@ ${approverText(wf, 0)}`]);
    WF.unshift(wf);
    audit('sqlworkflow.submit', `提交工单 #${id}「${wf.name}」（Redis，${res.items.length} 条命令）`, '成功', `${inst.name} / ${wf.db}`);
    toast(`工单 #${id} 已提交，等待「${LVNAME[flowOf(wf.rg)[0]]}」审批`);
    Object.assign(NS, {name: '', url: '', from: '', to: '', sql: '', check: null, skip: false, batch: false, binsts: []});
    buildNav(); go('sqlworkflow', 'detail/' + id);
  }

  /* ---- 详情 ---- */
  function redisExcBanner(wf, k) {
    const r = wf.res[k];
    return notice(r && r.recheck
      ? `<b>执行有异常：规则复检未通过，整单拒绝执行，一条命令都没有发出。</b>${esc(r.msg)}。`
      : `<b>执行有异常：第 ${wf.chk.items[k].n} 行命令失败，其后的命令未执行。</b>前面已成功的命令<b>不会回退</b>。`, 'bad', '!');
  }
  function redisFinishLog(wf, bad, k) {
    if (!bad) return '执行完成，工单已正常结束';
    return wf.res[k] && wf.res[k].recheck ? '执行有异常：按当前规则复检未通过，整单未执行任何命令' : `执行有异常：第 ${wf.chk.items[k].n} 行命令失败，其后命令未执行（已执行的命令不回退）`;
  }
  function redisSqlCard(wf) {
    const items = wf.chk.items, showRes = wf.res.length > 0 || wf.status === ST.run;
    const resCell = i => {
      const r = wf.res[i]; if (!r) return '<span class="muted">—</span>';
      const bf = r.before ? `<details style="margin-top:4px"><summary class="small">执行前的原值</summary><pre class="mono small" style="margin:4px 0 0;white-space:pre-wrap;word-break:break-all;background:var(--panel2);border-radius:6px;padding:6px 8px">${esc(r.before)}</pre><div class="small" style="color:var(--warn)">⚠ 原值明文保存，不是备份，不能保证完整恢复。</div></details>` : '';
      if (r.st === 'ok') return `<span class="tag ok">成功</span><div class="small mono" style="word-break:break-all">${esc(r.msg)}</div>${bf}`;
      if (r.st === 'bad') return `<span class="tag bad">${r.recheck ? '规则复检未通过，拒绝执行' : 'Execute failed'}</span><div class="small" style="color:var(--bad)">${esc(r.msg)}</div>${bf}`;
      if (r.st === 'skip') return `<span class="tag">未执行</span><div class="small muted">${esc(r.msg)}</div>`;
      return '<span class="tag warn">⟳ 执行中…</span>';
    };
    const heads = ['行', '命令', '涉及键数', '检查结果', '检查信息'].concat(showRes ? ['执行结果', '耗时(s)'] : []);
    const rows = items.map((it, i) => [it.n, `<div class="mono small" style="min-width:200px;max-width:340px;white-space:pre-wrap;word-break:break-all">${esc(it.sql.length > 200 ? it.sql.slice(0, 200) + ' …' : it.sql)}</div>`, `<span class="mono">${it.rows}</span>`,
      `<span class="tag ${it.lvl === 'bad' ? 'bad' : it.lvl === 'warn' ? 'warn' : 'ok'}">${esc(it.status)}</span>`, `<span class="small">${esc(rInfo(it))}</span>`].concat(showRes ? [resCell(i), `<span class="mono">${wf.res[i] && wf.res[i].sec != null ? wf.res[i].sec : ''}</span>`] : []));
    return `<div class="card" style="margin-top:14px"><h2>Redis 命令（原文）与逐条结果 <span class="sub">共 ${items.length} 条 · 警告 ${wf.chk.warn} · 拒绝 ${wf.chk.err}</span></h2>
      ${code(wf.sql)}
      <div style="margin-top:8px">${tbl(heads, rows, {attrs: (r, i) => wf.res[i] && wf.res[i].st === 'bad' ? ' style="background:var(--bad-soft)"' : ''})}</div></div>`;
  }

  /* ---- 执行：先用“当前规则”整单复检（任一行不再通过则一条都不发），再逐条执行，遇错即停 ---- */
  function redisRunStep(wf, i, it) {
    const RR = window.RedisRules;
    if (i === 0) {
      const inst = wfInst(wf.inst), t = {instId: inst.id, db: parseInt(wf.db, 10) || 0};
      wf.rv = wf.chk.items.map(x => RR.evalLine(RR.rules, t, x.sql));
      const bad = wf.rv.findIndex(v => v.level === 2);
      if (bad >= 0) {
        wf.res = wf.chk.items.map((x, j) => j === bad ? {st: 'bad', recheck: true, msg: `执行前按当前规则复检未通过：${wf.rv[j].message}`, rows: 0, sec: 0} : {st: 'skip', msg: '有命令按当前规则不再通过，整单未执行', rows: 0, sec: 0});
        wfLog(wf, '系统', `执行前按当前规则整单复检：第 ${wf.chk.items[bad].n} 行不再通过（${wf.rv[bad].message}），一条命令都不发`);
        return finishRun(wf);
      }
      wfLog(wf, '系统', `执行前按当前规则整单复检通过（${wf.chk.items.length} 条）`);
    }
    wf.res[i] = {st: 'run'}; wfPaint();
    wf._t = setTimeout(() => redisStepDone(wf, i), 600);
  }
  function redisStepDone(wf, i) {
    clearTimeout(wf._t);
    if (wf.status !== ST.run) return;
    const RR = window.RedisRules, v = wf.rv[i], before = v.recordBefore ? RR.simSnapshot(v.args) : '';
    if (wf.failAt === i) {
      wf.res[i] = {st: 'bad', msg: wf.failMsg || 'WRONGTYPE Operation against a key holding the wrong kind of value', rows: 0, sec: 0.003, before};
      for (let j = i + 1; j < wf.chk.items.length; j++) wf.res[j] = {st: 'skip', msg: '前面的命令执行失败，已停止执行', rows: 0, sec: 0};
      return finishRun(wf);
    }
    const sim = RR.simExec(v.args);
    wf.res[i] = {st: 'ok', msg: sim.text + (v.level === 1 ? `（${v.message}）` : ''), rows: sim.rows, sec: sim.sec, before};
    wf.runIdx = i + 1; runStep(wf);
  }
  /* 审计：Redis上线-执行（含复检拒绝；定时执行的操作人是 system）：每条命令的摘要 + 结果，疑似密码的值参数记 *** */
  function redisAuditExec(wf) {
    const RR = window.RedisRules, inst = wfInst(wf.inst);
    const tk = s => { try { return RR.tokenize(s); } catch (e) { return [s.slice(0, 60)]; } };
    const parts = wf.chk.items.map((it, i) => { const r = wf.res[i] || {}; return `[${it.n}]${RR.redact(tk(it.sql))}→${r.st === 'skip' ? '未执行' : r.st === 'bad' ? '失败' : '成功'}`; });
    const ok = wf.res.filter(r => r.st === 'ok').length, bad = wf.res.filter(r => r.st === 'bad').length;
    audit('sqlworkflow.execute_group', `Redis上线-执行：工单id：${wf.id}，实例：${inst.name}，库：${wf.db}，共 ${wf.res.length} 条：成功 ${ok}、失败 ${bad}；${parts.join('；')}`, bad ? '失败' : '成功', `${inst.name} / ${wf.db}`);
    if (wf._sys) { AUDIT[0].user = 'system'; AUDIT[0].ip = '—'; delete wf._sys; }
  }

  const EXEC_LABEL = {goinception: 'goInception', native: '原生执行', manual: '手工执行'};
  const RB = {};                         // 展开回滚语句的工单
  const wfOf = d => WF.find(w => w.id == d.id);

  function banner(wf) {
    const fl = flowOf(wf.rg), a = wf.abort;
    switch (wf.status) {
      case ST.wait: {
        const none = !approverList(wf, wf.level).length;
        return notice(`<b>等待第 ${wf.level + 1} / ${fl.length} 级「${LVNAME[fl[wf.level]]}」审批。</b>可审批人：${esc(approverText(wf, wf.level))}。${none ? '<br>⚠ 本级没有可审批的人，仅超级用户可审批，请联系管理员。' : ''}`, none ? 'warn' : '', none ? '⚠' : '⏳');
      }
      case ST.pass: return notice(`<b>已通过全部 ${fl.length} 级审批，可以执行。</b>${wf.window ? `<br>可执行时间窗：${esc(winText(wf.window))}（${inWindow(wf) ? '当前在窗口内' : '<b style="color:var(--warn)">当前不在窗口内，不能执行</b>'}）。` : ''}`, 'ok', '✔');
      case ST.timing: return notice(`<b>已设置定时执行：${esc(wf.schedule)}</b>（${esc(EXEC_LABEL[wf.executor] || '')}，设置人 ${esc(wf.scheduleBy)}）。`, '', '⏱');
      case ST.run: return notice(`<b>正在执行（${esc((EXEC_LABEL[wf.executor] || '').split('（')[0])}）。</b>`, 'warn', '⟳');
      case ST.done: return notice(wf.manual ? '<b>手工工单：已确认结束。</b>系统没有执行任何语句，线下执行的结果由确认人负责。' : `<b>执行完成，工单已正常结束</b>（${esc(wf.endedAt)}）。`, 'ok', '✓');
      case ST.exc: {
        const k = wf.res.findIndex(r => r.st === 'bad');
        if (wf.redis) return redisExcBanner(wf, k); return notice(wf.type === 'batch' ? `<b>执行有异常：部分实例失败。</b>已有实例的变更已生效，请人工核对。` : `<b>执行有异常：第 ${k + 1} 条语句失败，其后的语句未执行。</b>前面已成功的语句不会自动回滚。`, 'bad', '!');
      }
      case ST.abort: return notice(`<b>${a && a.kind === 'cancel' ? '提交人已撤回' : '工单已被驳回 / 终止'}。</b>${a ? `${uName(a.by)} 于 ${esc(a.at)}：${esc(a.note || '（未填写原因）')}` : ''}`, '', '■');
      case ST.auto: { const e = wf.chk.items.flatMap(i => i.msgs.filter(m => m.lvl === 'bad')); return notice(`<b>系统自动审批不通过。</b>提交时服务端重新检查发现 ${wf.chk.err} 条错误：${esc((e[0] || {}).text || '')}${e.length > 1 ? ` 等 ${e.length} 项` : ''}。`, 'bad', '✕'); }
    }
    return '';
  }
  /* 步骤条：提交 → 各级审批 → 执行 → 结束，按状态着色（done / on / bad / skip） */
  function stepsHtml(wf) {
    const fl = flowOf(wf.rg), n = fl.length, ZH = ['一', '二', '三', '四'], execI = n + 1, endI = n + 2;
    const labels = ['提交', ...fl.map((g, i) => `${ZH[i]}级审批 · ${LVNAME[g]}`), '执行', '结束'];
    const s = labels.map(() => 'todo'); s[0] = 'done';
    const reviewDone = () => { for (let i = 1; i <= n; i++) s[i] = 'done'; };
    switch (wf.status) {
      case ST.wait: for (let i = 0; i < n; i++) s[1 + i] = i < wf.level ? 'done' : i === wf.level ? 'on' : 'todo'; break;
      case ST.pass: case ST.timing: case ST.run: reviewDone(); s[execI] = 'on'; break;
      case ST.done: reviewDone(); s[execI] = 'done'; s[endI] = 'done'; break;
      case ST.exc: reviewDone(); s[execI] = 'bad'; s[endI] = 'skip'; break;
      case ST.abort: {
        const lv = wf.abort ? wf.abort.lv : wf.level;
        if (wf.abort && wf.abort.stage === 'exec') { reviewDone(); s[execI] = 'bad'; s[endI] = 'skip'; }
        else { for (let i = 0; i < n; i++) s[1 + i] = i < lv ? 'done' : i === lv ? 'bad' : 'skip'; s[execI] = 'skip'; s[endI] = 'skip'; }
        break;
      }
      case ST.auto: s[1] = 'bad'; for (let i = 2; i < labels.length; i++) s[i] = 'skip'; break;
    }
    if (wf.status === ST.timing) labels[execI] = `执行 · 定时 ${wf.schedule}`;
    if (wf.status === ST.run) labels[execI] = '执行中…';
    if (wf.status === ST.pass) labels[execI] = '执行 · 待执行';
    return `<div class="steps" style="margin:0">${labels.map((t, i) => {
      const st = s[i], col = st === 'bad' ? 'var(--bad)' : '', sty = st === 'bad' ? `style="color:var(--bad);font-weight:600"` : st === 'skip' ? 'style="opacity:.45"' : '';
      return `<div class="st ${st === 'done' ? 'done' : st === 'on' ? 'on' : ''}" ${sty}><b ${col ? `style="border-color:${col};color:${col}"` : ''}>${st === 'done' ? '✓' : st === 'bad' ? '✕' : st === 'skip' ? '–' : i + 1}</b>${esc(t)}</div>`;
    }).join('')}</div>`;
  }
  function actionCard(wf) {
    const id = wf.id, sub = wf.submitter === ME.login, btns = [], hints = [], ec = execCode(wf), ecCode = ec || 'sqlworkflow.execute_group';
    const execWhy = '无该工单的执行权限';
    switch (wf.status) {
      case ST.wait: {
        const ok = canApproveFor(wf, ME), self = sub && !ME.super;
        const why = self ? '不能审批自己提交的工单' : `你不是当前审批级别（第 ${wf.level + 1} 级 · ${LVNAME[flowOf(wf.rg)[wf.level]]}）的审批人`;
        btns.push(gate('sqlworkflow.approve', '✓ 审批通过', ok, why, {cls: 'primary', data: {id}}));
        btns.push(gate('sqlworkflow.reject', '✕ 驳回', ok, why, {cls: 'danger', data: {id}}));
        btns.push(gate('sqlworkflow.alter_window', '修改可执行时间', isLevelApprover(wf, ME), '只有当前审批人可以修改可执行时间', {data: {id}}));
        if (sub) btns.push(B('sqlworkflow.cancel_own', '撤回工单', {cls: 'danger', data: {id}}));
        break;
      }
      case ST.pass: case ST.timing: {
        btns.push(gate(ecCode, '▶ 立即执行', !!ec, execWhy, {cls: 'primary', data: {id}}));
        btns.push(gate('sqlworkflow.schedule', wf.status === ST.timing ? '⏱ 修改定时' : '⏱ 定时执行', !!ec, execWhy, {data: {id}}));
        btns.push(gate('sqlworkflow.manual_finish', '✓ 确认手工执行结束', !!ec, execWhy, {data: {id}}));
        if (wf.status === ST.timing && ec) btns.push(`<button class="btn" data-on="wfFire" data-id="${id}" title="原型：模拟调度器到点触发">⏱ 原型：模拟到点触发</button>`);
        if (sub && can('sqlworkflow.cancel_own')) btns.push(B('sqlworkflow.cancel_own', '撤回 / 终止工单', {cls: 'danger', data: {id}}));
        else btns.push(gate('sqlworkflow.reject', '■ 终止工单', ME.super || ME.rgs.includes(wf.rg), '无终止该工单的权限', {cls: 'danger', data: {id}}));
        if (!inWindow(wf)) hints.push(`当前不在可执行时间窗（${esc(winText(wf.window))}）内，立即执行会被拒绝。`);
        break;
      }
      case ST.done: case ST.exc: case ST.abort: case ST.auto:
        if (wf.type !== 'batch' || wf.status !== ST.exc) btns.push(B('sqlworkflow.submit', '⎘ 复制为新工单', {data: {m: 'copy', id}}));
        break;
    }
    if (!btns.length && !hints.length) return '';
    return `<div class="card" style="margin-top:14px"><h2>操作</h2><div class="row">${btns.join('')}</div>${hints.length ? `<div class="small muted" style="margin-top:8px">${hints.map(h => `· ${h}`).join('<br>')}</div>` : ''}</div>`;
  }
  function infoCard(wf) {
    const inst = wfInst(wf.inst), ms = isMs(inst), rg = RGROUPS.find(r => r.id === wf.rg);
    const insts = wf.type === 'batch' ? wf.insts.map(i => `<span class="tag">${instName(i)}</span>`).join(' ') : `${esc(inst.name)} <span class="tag">${inst.type}${inst.role === 'slave' ? ' · 从' : ''}</span> <span class="small muted mono">${esc(inst.host)}:${inst.port}</span>`;
    return `<div class="card"><h2>${esc(wf.name)}</h2>${kvList([
      ['工单号', `<b>#${wf.id}</b>`], ['状态', stHtml(wf.status)],
      ['类型', `${wf.redis ? 'Redis 命令' : wf.sqlType}${wf.type === 'batch' ? ' · <span class="tag brand">批量上线</span>' : ''}${wf.chk.osc ? ' · <span class="tag info">含大表 DDL / OSC</span>' : ''}`], ['提交人', uName(wf.submitter)],
      ['资源组', `${esc(rgName(wf.rg))} <span class="small muted">审批流 ${esc(flowText(wf.rg))}</span>`], ['创建时间', esc(wf.created)],
      ['目标实例', insts], ['数据库', `<span class="mono">${esc(wf.db)}</span>`],
      [wf.redis ? '回滚' : '执行前备份', wf.redis ? '无自动回滚' : wf.backup ? '是' : '否'],
      ['可执行时间', wf.window ? esc(winText(wf.window)) : '不限'], ['语法检查', wf.redis ? 'Redis 上线规则' : wf.skip ? '<span class="tag warn">已跳过（只能原生执行）</span>' : ms ? '本地 T-SQL 解析 + 上线规则 + PARSEONLY' : 'goInception + 站点规则'],
      ['执行方式', wf.executor ? esc(wf.redis ? EXEC_LABEL_REDIS : EXEC_LABEL[wf.executor]) : '<span class="muted">待执行人选择</span>'], ['需求链接', wf.url ? `<a href="${esc(wf.url)}" target="_blank" rel="noopener">${esc(wf.url)}</a>` : ''],
      ['定时执行', wf.schedule ? `${esc(wf.schedule)}（${esc(wf.scheduleBy)}）` : ''], ['开始 / 结束', wf.startedAt || wf.endedAt ? `${esc(wf.startedAt || '—')} → ${esc(wf.endedAt || '进行中')}` : ''],
    ], 'c2')}</div>`;
  }
  function sqlCard(wf) { if (wf.redis) return redisSqlCard(wf);
    const showRes = wf.type !== 'batch' && (wf.res.length > 0 || wf.status === ST.run);
    const resCell = i => { const r = wf.res[i]; if (!r) return '<span class="muted">—</span>'; return r.st === 'ok' ? `<span class="tag ok">成功</span>${r.msg ? `<div class="small muted">${esc(r.msg)}</div>` : ''}` : r.st === 'bad' ? `<span class="tag bad">失败</span><div class="small" style="color:var(--bad)">${esc(r.msg)}</div>` : r.st === 'skip' ? `<span class="tag">未执行</span><div class="small muted">${esc(r.msg)}</div>` : '<span class="tag warn">⟳ 执行中…</span>'; };
    const heads = ['#', '阶段', 'SQL', '检查级别', '检查信息 / 建议', '影响行数'].concat(showRes ? ['执行结果', '耗时(s)'] : []);
    const rows = wf.chk.items.map((it, i) => [it.n, `<span class="small">${it.stage}</span>`, `<div class="mono small" style="min-width:200px;max-width:340px;white-space:pre-wrap;word-break:break-all">${esc(it.sql.length > 200 ? it.sql.slice(0, 200) + ' …' : it.sql)}</div>`, lvTag(it.lvl),
      it.msgs.length ? `<ul style="margin:0;padding-left:16px">${it.msgs.map(m => `<li class="small" style="color:${m.lvl === 'bad' ? 'var(--bad)' : 'var(--warn)'}">${esc(m.text)}</li>`).join('')}</ul>` : '<span class="muted">—</span>', `<span class="mono">${it.rows ? fmtN(it.rows) : 0}</span>`]
      .concat(showRes ? [resCell(i), `<span class="mono">${wf.res[i] && wf.res[i].sec != null ? wf.res[i].sec : ''}</span>`] : []));
    return `<div class="card" style="margin-top:14px"><h2>SQL 与逐条结果 <span class="sub">共 ${wf.chk.items.length} 条 · ${wf.chk.skipped ? '未检查（跳过）' : `警告 ${wf.chk.warn} · 错误 ${wf.chk.err}`}</span></h2>
      ${code(wf.sql, 'sql')}
      <div style="margin-top:8px">${tbl(heads, rows, {attrs: (r, i) => { const l = wf.chk.items[i].lvl; return l === 'bad' ? ' style="background:var(--bad-soft)"' : ''; }})}</div></div>`;
  }
  function batchCard(wf) {
    if (wf.type !== 'batch') return '';
    const okN = wf.bi.filter(b => b.st === '成功').length, failN = wf.bi.filter(b => b.st === '失败').length, closeN = wf.bi.filter(b => b.st === '已关闭').length;
    const ec = !!execCode(wf), why = '无该工单的执行权限';
    const tg = {'成功': 'ok', '失败': 'bad', '已关闭': '', '执行中': 'warn', '待执行': 'info'};
    return `<div class="card" style="margin-top:14px"><h2>各实例执行状态</h2>
      ${tbl(['实例', '版本 / 地址', '状态', '说明', '耗时(s)', '操作'], wf.bi.map((b, i) => { const ins = wfInst(b.inst); return [`<b>${esc(ins.name)}</b>`, `<span class="small muted">${esc(ins.ver)} · ${esc(ins.host)}:${ins.port}</span>`, `<span class="tag ${tg[b.st]}">${b.st === '执行中' ? '⟳ ' : ''}${b.st}</span>`, `<span class="small ${b.st === '失败' ? '' : 'muted'}" style="${b.st === '失败' ? 'color:var(--bad)' : ''}">${esc(b.msg)}</span>`, b.sec ? `<span class="mono">${b.sec}</span>` : '',
        b.st === '失败' ? `<div class="ops">${gate('sqlworkflow.batch_op', '重试', ec, why, {cls: 'sm', data: {id: wf.id, i, op: 'retry'}})}${gate('sqlworkflow.batch_op', '关闭', ec, why, {cls: 'sm danger', data: {id: wf.id, i, op: 'close'}})}</div>` : '']; }))}
      ${wf.status === ST.exc || wf.status === ST.done || okN + failN + closeN ? `<div class="notice ${failN ? 'warn' : 'ok'}" style="margin-top:10px"><span>Σ</span><div><b>批量执行汇总：</b>共 ${wf.bi.length} 个实例，成功 ${okN}，失败 ${failN}，已关闭 ${closeN}。${failN ? `已有 ${okN} 个实例的变更已生效，请人工核对。` : closeN === wf.bi.length ? '全部实例都被关闭，工单不会被置为成功。' : ''}</div></div>` : ''}</div>`;
  }
  function oscCard(wf) {
    const o = wf.osc; if (!o) return '';
    const it = wf.chk.items[o.idx], live = wf.status === ST.run, ec = !!execCode(wf), why = '需要执行权限且工单在执行中';
    const stMap = {running: ['info', '运行中'], paused: ['warn', '已暂停'], killed: ['bad', '已终止'], done: ['ok', '已完成']}, [cls, txt] = stMap[o.state];
    return `<div class="card" style="margin-top:14px"><h2>在线改表（${o.tool}） <span class="sub">sqlhash <span class="mono">${o.sha1.slice(0, 12)}…</span></span><span class="right"><span class="tag ${cls}">${txt}</span></span></h2>
      <div class="small mono" style="margin-bottom:8px;white-space:pre-wrap">${esc(it ? it.sql : '')}</div>
      <div class="row"><div class="meter" style="flex:1;height:12px"><b style="width:${o.pct}%;${o.state === 'killed' ? 'background:var(--bad)' : o.state === 'paused' ? 'background:var(--warn)' : ''}"></b></div><b>${o.pct}%</b><span class="muted small">剩余 ${esc(o.eta)}</span></div>
      ${live ? `<div class="row" style="margin-top:10px">${gate('sqlworkflow.osc', '⏸ 暂停', ec && o.state === 'running', why, {data: {id: wf.id, op: 'pause'}})}${gate('sqlworkflow.osc', '▶ 恢复', ec && o.state === 'paused', why, {data: {id: wf.id, op: 'resume'}})}${gate('sqlworkflow.osc', '■ 终止', ec, why, {cls: 'danger', data: {id: wf.id, op: 'kill'}})}</div>` : ''}</div>`;
  }
  function rollbackCard(wf) {
    if (wf.type === 'batch' || !(wf.status === ST.done || wf.status === ST.exc) || wf.manual || wf.redis) return '';
    if (!wf.backup) return `<div class="card" style="margin-top:14px"><h2>回滚语句</h2><div class="muted small">该工单没有开启备份（${isMs(wfInst(wf.inst)) ? 'SQL Server 不支持备份' : '原生执行 / 未勾选备份'}），没有回滚语句。</div></div>`;
    const open = !!RB[wf.id], list = open ? wfRollback(wf) : [];
    return `<div class="card" style="margin-top:14px"><h2>回滚语句<span class="right">${B('sqlworkflow.rollback', open ? '收起' : '查看回滚语句', {data: {id: wf.id, op: 'view'}})}${B('sqlworkflow.rollback', '⇩ 下载回滚 SQL', {data: {id: wf.id, op: 'dl'}})}</span></h2>
      ${open ? list.slice().reverse().map(r => `<div class="small muted mono" style="margin-top:8px;white-space:pre-wrap">-- 原语句：${esc(r.orig.replace(/\s+/g, ' ').slice(0, 160))}</div>${code(r.rb.join('\n'), 'sql')}`).join('') : ''}</div>`;
  }
  function timelineCard(wf) {
    const fl = flowOf(wf.rg), items = [{cls: 'ok', h: `提交 · ${uName(wf.submitter)}`, m: esc(wf.created) + (wf.skip ? ' · 跳过语法检查' : '')}];
    const a = wf.abort;
    fl.forEach((g, i) => {
      const ap = wf.approvals.filter(x => x.lv === i);
      const pass = ap.find(x => x.action === 'approve'), rej = ap.find(x => x.action === 'reject'), name = `第 ${i + 1} 级 · ${LVNAME[g]}`;
      if (pass) items.push({cls: 'ok', h: `${name} · 已通过`, m: `${uName(pass.user)} · ${esc(pass.at)}${pass.note ? ` · “${esc(pass.note)}”` : ''}`});
      else if (rej) items.push({cls: 'bad', h: `${name} · 已驳回 / 终止`, m: `${uName(rej.user)} · ${esc(rej.at)} · “${esc(rej.note)}”`});
      else if (a && a.kind === 'cancel' && a.stage === 'review' && a.lv === i) items.push({cls: 'bad', h: `${name} · 提交人已撤回`, m: `${uName(a.by)} · ${esc(a.at)} · “${esc(a.note)}”`});
      else if (wf.status === ST.wait && i === wf.level) items.push({cls: 'cur', h: `${name} · 审批中`, m: `可审批人：${esc(approverText(wf, i))}`});
      else if (wf.status === ST.auto && i === 0) items.push({cls: 'bad', h: `${name} · 系统自动驳回`, m: '提交时服务端重新检查有错误'});
      else if (wf.status === ST.auto || wf.status === ST.abort || (wf.status === ST.wait && i > wf.level)) items.push({cls: 'skip', h: `${name} · ${wf.status === ST.wait ? '待审批' : '未到达'}`, m: ''});
      else items.push({cls: 'ok', h: `${name} · 已通过`, m: ''});
    });
    const ex = {[ST.pass]: ['cur', '执行 · 待执行', '等待有执行权限的人执行'], [ST.timing]: ['cur', '执行 · 定时', `计划 ${esc(wf.schedule)}`], [ST.run]: ['cur', '执行中', `开始于 ${esc(wf.startedAt)}`],
      [ST.done]: ['ok', wf.manual ? '手工确认结束' : '已正常结束', esc(wf.endedAt)], [ST.exc]: ['bad', '执行有异常', esc(wf.endedAt)]}[wf.status];
    if (ex) items.push({cls: ex[0], h: ex[1], m: ex[2]});
    else if (a && a.stage === 'exec') items.push({cls: 'bad', h: '执行前被终止', m: `${uName(a.by)} · ${esc(a.at)} · “${esc(a.note)}”`});
    else items.push({cls: 'skip', h: '执行 / 结束 · 未开始', m: ''});
    return `<div class="card"><h2>审批进度</h2>${tline(items)}</div>`;
  }
  function logCard(wf) {
    return `<div class="card" style="margin-top:14px"><h2>操作日志 <span class="sub">${wf.log.length} 条</span></h2><div class="log" style="height:auto;max-height:340px">${wf.log.map(([at, u, act]) => `<div><span style="opacity:.6">${esc(at.slice(5))}</span> <b style="color:${u === '系统' ? '#8fb5ff' : '#e8f7ee'}">${esc(u)}</b> ${esc(act)}</div>`).join('')}</div></div>`;
  }
  function detailBody(id) {
    const wf = WF.find(w => w.id == id);
    const back = '<div class="row" style="margin-bottom:12px"><a class="btn" href="#sqlworkflow">← 返回列表</a></div>';
    if (!wf || !wfVisible(wf)) return back + `<div class="card">${empty('工单不存在，或你没有权限查看。', '🔒')}</div>`;
    return `${back}
    <div class="row" style="margin-bottom:10px"><b style="font-size:17px">SQL 工单 #${wf.id}</b>${stHtml(wf.status)}${wf.type === 'batch' ? `<span class="tag brand">批量上线 ${wf.insts.length}</span>` : ''}<span class="muted">${esc(wf.name)}</span></div>
    ${banner(wf)}${wf.redis ? redisDetailNote() : ''}
    <div class="card" style="padding:14px 16px">${stepsHtml(wf)}</div>
    ${actionCard(wf)}
    <div class="grid g-main" style="margin-top:14px;align-items:start">
      <div style="min-width:0">${infoCard(wf)}${batchCard(wf)}${oscCard(wf)}${sqlCard(wf)}${rollbackCard(wf)}</div>
      <div style="min-width:0">${timelineCard(wf)}${logCard(wf)}</div>
    </div>`;
  }

  /* ---------- 详情页动作 ---------- */
  const envOf = wf => wfInst(wf.inst).env;
  function chkState(wf, ok, msg) { if (ok) return true; toast(msg || '工单状态已变化，请刷新'); wfPaint(true); return false; }
  ACTIONS['sqlworkflow.approve'] = d => {
    const wf = wfOf(d); if (!wf) return;
    if (!canApproveFor(wf, ME)) return toast(wf.submitter === ME.login && !ME.super ? '不能审批自己提交的工单' : '你不是当前审批级别的审批人');
    const fl = flowOf(wf.rg), last = wf.level + 1 >= fl.length;
    openModal({title: `审批通过 · 工单 #${wf.id}`, code: 'sqlworkflow.approve', ok: '确认通过',
      body: `${notice(last ? '这是<b>最后一级</b>：通过后工单进入「审核通过」。' : `通过后流转到第 ${wf.level + 2} 级「${LVNAME[fl[wf.level + 1]]}」（${esc(approverText(wf, wf.level + 1))}）。`)}
        <div class="small muted" style="margin-bottom:6px">${esc(wf.name)} · ${wfTarget(wf)} / ${esc(wf.db)} · 检查：警告 ${wf.chk.warn}，错误 ${wf.chk.err}</div>
        <div class="form">${fld('审批意见（可不填）', '<textarea id="wfNote" rows="3"></textarea>')}</div>`,
      onOk() {
        if (!chkState(wf, canApproveFor(wf, ME))) return;
        opApprove(wf, $('#wfNote').value.trim());
        audit('sqlworkflow.approve', `审批通过工单 #${wf.id}（第 ${wf.approvals.length ? wf.approvals[wf.approvals.length - 1].lv + 1 : 1} 级）`, '成功', `${wfInst(wf.inst).name} / ${wf.db}`);
        toast(wf.status === ST.pass ? `工单 #${wf.id} 审批通过，可以执行` : `工单 #${wf.id} 已流转到下一级`); syncUi();
      }});
  };
  ACTIONS['sqlworkflow.reject'] = d => {
    const wf = wfOf(d); if (!wf) return;
    const review = wf.status === ST.wait, ok = review ? (isLevelApprover(wf, ME) && (ME.super || wf.submitter !== ME.login)) : ((wf.status === ST.pass || wf.status === ST.timing) && (ME.super || ME.rgs.includes(wf.rg)));
    if (!ok) return toast(review ? '只有当前审批级别的审批人可以驳回（不能驳回自己的工单，请使用撤回）' : '当前状态不能终止');
    openModal({title: `${review ? '驳回' : '终止'}工单 #${wf.id}`, code: 'sqlworkflow.reject', ok: review ? '确认驳回' : '确认终止', okCls: 'danger primary',
      body: `${notice(review ? '驳回后工单进入「人工终止流程」。' : '终止后不可恢复，已设置的定时计划会被取消。', 'warn', '⚠')}<div class="form">${fld('原因', '<textarea id="wfNote" rows="3"></textarea>', {req: true})}</div>`,
      onOk() {
        const note = $('#wfNote').value.trim(); if (!note) { toast('请填写原因'); return false; }
        if (!chkState(wf, wf.status === ST.wait || wf.status === ST.pass || wf.status === ST.timing)) return;
        opAbort(wf, 'reject', note); audit('sqlworkflow.reject', `${review ? '驳回' : '终止'}工单 #${wf.id}：${note}`, '成功', `${wfInst(wf.inst).name} / ${wf.db}`); toast(`工单 #${wf.id} 已${review ? '驳回' : '终止'}`); syncUi();
      }});
  };
  ACTIONS['sqlworkflow.cancel_own'] = d => {
    const wf = wfOf(d); if (!wf) return;
    if (wf.submitter !== ME.login) return toast('只能撤回自己提交的工单');
    if (![ST.wait, ST.pass, ST.timing].includes(wf.status)) return toast('当前状态不能撤回（执行中 / 已结束的工单不能终止）');
    openModal({title: `撤回工单 #${wf.id}`, code: 'sqlworkflow.cancel_own', ok: '确认撤回', okCls: 'danger primary',
      body: `${notice('撤回后工单进入「人工终止流程」，不可恢复。', 'warn', '⚠')}<div class="form">${fld('原因（可不填）', '<textarea id="wfNote" rows="3"></textarea>')}</div>`,
      onOk() { if (!chkState(wf, [ST.wait, ST.pass, ST.timing].includes(wf.status))) return; opAbort(wf, 'cancel', $('#wfNote').value.trim()); audit('sqlworkflow.cancel_own', `撤回工单 #${wf.id}`, '成功', `${wfInst(wf.inst).name} / ${wf.db}`); toast(`工单 #${wf.id} 已撤回`); syncUi(); }});
  };
  ACTIONS['sqlworkflow.alter_window'] = d => {
    const wf = wfOf(d); if (!wf) return;
    if (!isLevelApprover(wf, ME)) return toast('只有当前审批人可以修改可执行时间');
    const w = wf.window || {};
    openModal({title: `修改可执行时间 · 工单 #${wf.id}`, code: 'sqlworkflow.alter_window', ok: '保存',
      body: `<div class="form">${fld('开始时间（可不填）', `<input type="text" id="wfFrom" value="${esc(w.from || '')}" placeholder="YYYY-MM-DD HH:mm">`)}${fld('结束时间（可不填）', `<input type="text" id="wfTo" value="${esc(w.to || '')}" placeholder="YYYY-MM-DD HH:mm">`)}</div>`,
      onOk() {
        const f = $('#wfFrom').value.trim(), t = $('#wfTo').value.trim(), re = /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/;
        if ((f && !re.test(f)) || (t && !re.test(t))) { toast('时间格式应为 YYYY-MM-DD HH:mm'); return false; }
        if (f && t && t <= f) { toast('结束时间必须晚于开始时间'); return false; }
        wf.window = f || t ? {from: f, to: t} : null; wfLog(wf, ME.login, `修改可执行时间：${wf.window ? winText(wf.window) : '不限'}`);
        audit('sqlworkflow.alter_window', `修改工单 #${wf.id} 可执行时间：${wf.window ? winText(wf.window) : '不限'}`, '成功'); toast('可执行时间已更新'); syncUi();
      }});
  };
  /* 立即执行：先选执行方式（模拟结果）→ 再高危二次确认 */
  function openExec(d) {
    const wf = wfOf(d); if (!wf) return;
    const code = execCode(wf); if (!code) return toast('你没有该工单的执行权限');
    if (wf.status !== ST.pass && wf.status !== ST.timing) return toast('只有「审核通过」或「定时执行」的工单可以执行');
    if (!inWindow(wf)) return toast(`当前时间不在可执行时间窗（${winText(wf.window)}）内，不能执行`);
    const clash = WF.find(w => w !== wf && w.status === ST.run && w.sqlType === 'DDL' && wf.sqlType === 'DDL' && w.db === wf.db && w.insts.some(i => wf.insts.includes(i)));
    if (clash) return toast(`同实例同库已有 DDL 工单 #${clash.id} 正在执行（同库 DDL 串行），请等它结束`);
    const noGi = wf.skip || isMs(wfInst(wf.inst)) || wf.type === 'batch' || !!wf.redis;
    const rec = noGi ? 'native' : 'goinception';
    openModal({title: `执行工单 #${wf.id}`, code, ok: '下一步：确认执行', okCls: 'danger primary',
      body: `<div class="form">${fld('执行方式', `<select id="wfExec"><option value="goinception" ${noGi ? 'disabled' : ''} ${rec === 'goinception' ? 'selected' : ''}>goInception${rec === 'goinception' ? '（默认推荐）' : ''}</option><option value="native" ${rec === 'native' ? 'selected' : ''}>原生执行${rec === 'native' ? '（默认推荐）' : ''}</option></select>`)}
        ${wf.type === 'batch' ? '' : fld('原型 · 模拟结果', `<select id="wfFail"><option value="-1">全部成功</option>${wf.chk.items.map((it, i) => `<option value="${i}">${wf.redis ? `第 ${it.n} 行命令` : `第 ${i + 1} 条语句`}失败</option>`).join('')}</select>`)}</div>`,
      onOk() {
        const ex = $('#wfExec').value, failAt = wf.type === 'batch' ? -1 : +$('#wfFail').value;
        const ms = envOf(wf) === '生产', head = wf.sql.split('\n').slice(0, 3).join('\n') + (wf.sql.split('\n').length > 3 ? '\n…' : '');
        risky(code, `立即执行工单 #${wf.id}`, `将在 <b>${esc(wfTarget(wf))}</b> 的库 <b>${esc(wf.db)}</b> 上执行 ${wf.chk.items.length} 条语句（${wf.sqlType}${wf.chk.osc ? '，含大表 DDL' : ''}${wf.type === 'batch' ? `，批量 ${wf.insts.length} 个实例` : ''}）。<br>失败即停，已成功的语句<b>不会自动回滚</b>。${ms ? '<br>目标是<b>生产</b>实例，请键入工单号确认。' : ''}`, head,
          () => { startRun(wf, ex, failAt); audit(code, `立即执行工单 #${wf.id}（${ex}）`, '成功', `${wfInst(wf.inst).name} / ${wf.db}`); toast(`工单 #${wf.id} 开始执行`); syncUi(); }, ms ? String(wf.id) : '');
        return false;
      }});
  }
  ACTIONS['sqlworkflow.execute_group'] = ACTIONS['sqlworkflow.execute_own'] = openExec;
  ACTIONS['sqlworkflow.schedule'] = d => {
    const wf = wfOf(d); if (!wf) return;
    if (!execCode(wf)) return toast('你没有该工单的执行权限');
    if (wf.status !== ST.pass && wf.status !== ST.timing) return toast('只有「审核通过」或「定时执行」的工单可以设定时');
    const def = wf.schedule || (wf.window && wf.window.from > wfNow() ? wf.window.from : dayOffset(1) + ' 02:00'), noGi = wf.skip || isMs(wfInst(wf.inst)) || wf.type === 'batch' || !!wf.redis;
    openModal({title: `定时执行 · 工单 #${wf.id}`, code: 'sqlworkflow.schedule', ok: '下一步：确认', okCls: 'danger primary',
      body: `<div class="form">${fld(wf.window ? `执行时间（可执行时间窗：${esc(winText(wf.window))}）` : '执行时间', `<input type="text" id="wfAt" value="${esc(def)}" placeholder="YYYY-MM-DD HH:mm">`, {req: true})}${fld('执行方式', `<select id="wfExec"><option value="goinception" ${noGi ? 'disabled' : 'selected'}>goInception（推荐）</option><option value="native" ${noGi ? 'selected' : ''}>原生执行</option></select>`)}</div>`,
      onOk() {
        const at = $('#wfAt').value.trim(), ex = $('#wfExec').value;
        if (!/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/.test(at)) { toast('时间格式应为 YYYY-MM-DD HH:mm'); return false; }
        if (at <= wfNow()) { toast('定时执行时间必须晚于当前时间'); return false; }
        if (!inWindow(wf, at)) { toast(`设定的时间不在可执行时间窗（${winText(wf.window)}）内`); return false; }
        risky('sqlworkflow.schedule', `设定时执行工单 #${wf.id}`, `到 <b>${esc(at)}</b> 由调度器自动执行（${esc(EXEC_LABEL[ex].split('（')[0])}）。`, '', () => {
          wf.status = ST.timing; wf.schedule = at; wf.scheduleBy = ME.login; wf.executor = ex; wfLog(wf, ME.login, `设置定时执行：${at}（${ex}）`); wfNotify(wf, `[SQL上线申请] 工单 #${wf.id} 已设置定时执行 ${at}`, wf.submitter);
          audit('sqlworkflow.schedule', `设置工单 #${wf.id} 定时执行：${at}`, '成功', `${wfInst(wf.inst).name} / ${wf.db}`); toast(`已设置定时执行：${at}`); syncUi();
        }); return false;
      }});
  };
  ON.wfFire = d => {
    const wf = wfOf(d); if (!wf || wf.status !== ST.timing) return;
    wfLog(wf, '系统', `调度器到点触发（计划 ${wf.schedule}），认领计划并转入执行`);
    wf._sys = true; startRun(wf, wf.executor || 'goinception', -1); toast(`调度器已触发工单 #${wf.id}`); syncUi();
  };
  ACTIONS['sqlworkflow.manual_finish'] = d => {
    const wf = wfOf(d); if (!wf) return;
    if (!execCode(wf)) return toast('你没有该工单的执行权限');
    if (wf.status !== ST.pass && wf.status !== ST.timing) return toast('只有「审核通过」或「定时执行」的工单可以确认手工结束');
    risky('sqlworkflow.manual_finish', `确认手工执行结束 · 工单 #${wf.id}`, '请确认你<b>已在线下执行完</b>该工单的 SQL。工单将置为「已正常结束」，<b>系统不会执行任何语句</b>。', '', () => {
      wf.status = ST.done; wf.manual = true; wf.executor = 'manual'; wf.endedAt = wfNow(); wf.schedule = null; wf.res = [];
      wfLog(wf, ME.login, '确认手工执行结束（手工工单：系统未执行任何语句）'); wfNotify(wf, `[SQL上线申请] 工单 #${wf.id} 已正常结束（手工）`, wf.submitter);
      audit('sqlworkflow.manual_finish', `确认工单 #${wf.id} 手工执行结束`, '成功', `${wfInst(wf.inst).name} / ${wf.db}`); toast(`工单 #${wf.id} 已确认结束`); syncUi();
    });
  };
  ACTIONS['sqlworkflow.rollback'] = d => {
    const wf = wfOf(d); if (!wf) return;
    if (d.op === 'view') { RB[wf.id] = !RB[wf.id]; if (RB[wf.id]) audit('sqlworkflow.rollback', `查看工单 #${wf.id} 回滚语句`, '成功'); return wfPaint(true); }
    const text = `-- 工单 #${wf.id}「${wf.name}」回滚语句（按执行顺序倒序）\n-- 实例 ${wfInst(wf.inst).name} / 库 ${wf.db}\n\n` + wfRollback(wf).reverse().map(r => `-- 原语句：${r.orig.replace(/\s+/g, ' ').slice(0, 160)}\n${r.rb.join('\n')}\n`).join('\n');
    try { const a = document.createElement('a'); a.href = URL.createObjectURL(new Blob([text], {type: 'text/plain;charset=utf-8'})); a.download = `rollback_${wf.id}.sql`; document.body.appendChild(a); a.click(); a.remove(); } catch (e) { /* 原型环境下载失败时忽略 */ }
    audit('sqlworkflow.rollback', `下载工单 #${wf.id} 回滚语句 rollback_${wf.id}.sql`, '成功'); toast(`已下载 rollback_${wf.id}.sql`);
  };
  ACTIONS['sqlworkflow.osc'] = d => {
    const wf = wfOf(d); if (!wf || !wf.osc || wf.status !== ST.run) return toast('工单不在执行中');
    if (!execCode(wf)) return toast('你没有该工单的执行权限');
    const o = wf.osc, short = o.sha1.slice(0, 12);
    if (d.op === 'pause') { o.state = 'paused'; wfLog(wf, ME.login, `暂停在线改表（sqlhash ${short}…）`); }
    else if (d.op === 'resume') { o.state = 'running'; wfLog(wf, ME.login, `恢复在线改表（sqlhash ${short}…）`); oscTick(wf); }
    else {
      risky('sqlworkflow.osc', `终止在线改表 · 工单 #${wf.id}`, `终止后<b>原表不变</b>，该语句记为失败，其后语句不再执行，工单进入「执行有异常」。`, `-- gh-ost: kill ${o.sha1}`, () => {
        o.state = 'killed'; wfLog(wf, ME.login, `终止在线改表（sqlhash ${short}…）`); audit('sqlworkflow.osc', `终止工单 #${wf.id} 在线改表`, '成功'); stmtDone(wf, o.idx, 'killed'); toast('已终止在线改表');
      }); return;
    }
    audit('sqlworkflow.osc', `${d.op === 'pause' ? '暂停' : '恢复'}工单 #${wf.id} 在线改表`, '成功'); toast(d.op === 'pause' ? '已暂停' : '已恢复'); syncUi();
  };
  ACTIONS['sqlworkflow.batch_op'] = d => {
    const wf = wfOf(d); if (!wf || wf.type !== 'batch') return;
    if (!execCode(wf)) return toast('你没有该工单的执行权限');
    const i = +d.i, b = wf.bi[i]; if (!b || b.st !== '失败') return toast('该实例当前不是失败状态');
    const name = wfInst(b.inst).name, retry = d.op === 'retry';
    risky('sqlworkflow.batch_op', `${retry ? '重试' : '关闭'}实例 ${name}`, retry ? `将在 <b>${esc(name)}</b> 上<b>重新执行整段 SQL</b>，并用新结果替换原有结果。` : `关闭后该实例<b>不再计入工单成败，也不能再重试</b>。`, retry ? wf.sql.split('\n').slice(0, 2).join('\n') : '',
      () => { batchOp(wf, i, d.op); audit('sqlworkflow.batch_op', `${retry ? '重试' : '关闭'}工单 #${wf.id} 实例 ${name}`, '成功'); toast(retry ? `正在重试 ${name}` : `已关闭 ${name}`); syncUi(); });
  };

  /* ============================================================================
     页面入口 / 路由
     ========================================================================== */
  const parseSub = sub => { const p = String(sub || '').split('?')[0].split('/'); return {view: p[0] || 'list', arg: p[1] || ''}; };
  function bodyOf(r) {
    if (r.view === 'new') return newBody();
    if (r.view === 'detail') return detailBody(r.arg);
    return listBody();
  }
  function wfPaint(force) {
    const el = $('#wfBody'); if (!el) return;
    const a = document.activeElement;
    if (!force && a && el.contains(a) && /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName)) return;
    el.innerHTML = bodyOf(parseSub(ROUTE.sub));
  }
  pages.sqlworkflow = {
    title: sub => { const r = parseSub(sub); return r.view === 'new' ? '提交 SQL 工单' : r.view === 'detail' ? `SQL 工单 #${r.arg}` : 'SQL 上线'; },
    render(sub) { return `<div id="wfBody">${bodyOf(parseSub(sub))}</div>`; },
  };

  /* ============================================================================
     SQL 分析（pages.sqlanalyze）：上传 .sql / MyBatis Mapper XML / 慢日志 → 后台逐条分析（进度条、可取消）
     规格：internal/sqlopt。内置规则 R001–R016、S001/S002、P001–P006 为词法级建议。
     ========================================================================== */
  const SAMPLE_FILES = {
    sql: {name: 'order_queries.sql', text: `-- 订单服务 · 近期上线的查询
SELECT * FROM orders WHERE user_id = 10086 ORDER BY created_at DESC LIMIT 20;
SELECT order_no, amount FROM orders WHERE date(created_at) = '2026-10-01';
SELECT * FROM orders WHERE order_no LIKE '%2026100%';
SELECT user_id, COUNT(*) FROM orders WHERE receiver_phone = 13800138000 GROUP BY user_id;
SELECT o.id, i.sku_name FROM orders o JOIN order_items i ON i.order_id = o.id WHERE o.status IN (1, 2, 3) ORDER BY RAND() LIMIT 10;
SELECT id, order_no FROM orders ORDER BY id LIMIT 500000, 20;
UPDATE orders SET status = 4 WHERE status = 0 AND created_at < '2026-09-01 00:00:00';
DELETE FROM order_log WHERE created_at < '2026-07-01 00:00:00';
INSERT INTO order_items VALUES (1, 100, 200, '测试商品', 1, 9.90);
UPDATE user_addr SET detail = '待补充';`},
    xml: {name: 'OrderMapper.xml', text: `<?xml version="1.0" encoding="UTF-8"?>
<mapper namespace="com.example.order.mapper.OrderMapper">
  <select id="listByUser" resultType="Order">
    SELECT * FROM orders
    <where>
      <if test="userId != null"> AND user_id = #{userId} </if>
      <if test="status != null"> AND status = #{status} </if>
    </where>
    ORDER BY created_at DESC
  </select>
  <select id="countByStatus">SELECT status, COUNT(*) FROM orders WHERE created_at &gt;= #{begin} GROUP BY status</select>
  <select id="byIds">SELECT order_no, amount FROM orders WHERE id IN <foreach collection="ids" item="id" open="(" separator="," close=")">#{id}</foreach></select>
  <update id="cancelExpired">UPDATE orders SET status = 4 WHERE status = 0 AND created_at &lt; #{deadline}</update>
  <delete id="purgeLog">DELETE FROM order_log WHERE created_at &lt; #{d}</delete>
</mapper>`},
    slow: {name: 'mysql-slow.log', text: `# Time: 2026-10-06T22:14:07.331221Z
# User@Host: app_rw[app_rw] @  [10.20.1.8]  Id: 88231
# Query_time: 12.431221  Lock_time: 0.000132 Rows_sent: 20  Rows_examined: 28460310
use order_db;
SET timestamp=1791324847;
SELECT * FROM orders WHERE receiver_phone = '13800138000' ORDER BY created_at DESC LIMIT 20;
# Time: 2026-10-06T22:31:44.120110Z
# User@Host: report[report] @  [10.20.2.3]  Id: 88377
# Query_time: 41.208933  Lock_time: 0.000411 Rows_sent: 120  Rows_examined: 71230887
SET timestamp=1791325904;
SELECT sku_id, SUM(qty) FROM order_items WHERE price > 100 GROUP BY sku_id ORDER BY SUM(qty) DESC;
# Time: 2026-10-06T23:02:10.004000Z
# User@Host: app_rw[app_rw] @  [10.20.1.9]  Id: 88410
# Query_time: 5.884100  Lock_time: 0.000090 Rows_sent: 0  Rows_examined: 152300440
SET timestamp=1791327730;
DELETE FROM order_log WHERE created_at < '2026-07-01 00:00:00';
# Time: 2026-10-06T23:40:55.771300Z
# User@Host: app_rw[app_rw] @  [10.20.1.8]  Id: 88502
# Query_time: 3.120000  Lock_time: 0.000050 Rows_sent: 1  Rows_examined: 6120044
SET timestamp=1791330055;
SELECT * FROM user_addr WHERE phone = 13900139000;`},
  };
  function xmlText(n) {
    let s = '';
    n.childNodes.forEach(ch => {
      if (ch.nodeType === 3 || ch.nodeType === 4) s += ch.nodeValue;
      else if (ch.nodeType === 1) {
        const t = ch.tagName.toLowerCase(), inner = xmlText(ch);
        if (t === 'where') s += ' WHERE ' + inner.trim().replace(/^(and|or)\b/i, '');
        else if (t === 'set') s += ' SET ' + inner.trim().replace(/,\s*$/, '');
        else if (t === 'foreach') s += ' (?, ?, ?) ';
        else s += ' ' + inner + ' ';
      }
    });
    return s;
  }
  /* 解析上传内容：.sql（去注释、按分号拆）/ MyBatis3 Mapper XML（展开 if / where / set / foreach，#{} → ?）/ MySQL 慢日志 */
  function parseUpload(text) {
    let fmt = 'sql', items = [];
    if (/<mapper[\s>]/i.test(text)) {
      fmt = 'xml';
      try {
        const doc = new DOMParser().parseFromString(text, 'application/xml');
        if (doc.querySelector('parsererror')) throw new Error('xml');
        const ns = doc.documentElement.getAttribute('namespace') || '';
        doc.querySelectorAll('select,insert,update,delete').forEach(el => items.push({label: (ns ? ns.split('.').pop() + '.' : '') + (el.getAttribute('id') || '?'), sql: xmlText(el).replace(/#\{[^}]*\}/g, '?').replace(/\$\{([^}]*)\}/g, '$1').replace(/\s+/g, ' ').trim()}));
      } catch (e) { fmt = 'sql'; items = []; }
    } else if (/^# Query_time:/m.test(text)) {
      fmt = 'slow'; let cur = null;
      const push = () => { if (cur && cur.sql.trim()) items.push({label: `${cur.user || ''}${cur.db ? ' @ ' + cur.db : ''}`.trim(), sql: cur.sql.trim().replace(/;\s*$/, '').replace(/\s+/g, ' '), stats: cur.stats}); cur = null; };
      text.split('\n').forEach(line => {
        if (/^#/.test(line)) {
          if (/^# (Time|User@Host)/.test(line) && cur && cur.sql.trim()) push();
          cur = cur || {sql: '', stats: {}};
          let m;
          if ((m = line.match(/^# User@Host:\s*(\S+?)\[/))) cur.user = m[1];
          if ((m = line.match(/Query_time:\s*([\d.]+).*?Rows_sent:\s*(\d+)\s+Rows_examined:\s*(\d+)/))) cur.stats = {qt: +m[1], sent: +m[2], exam: +m[3]};
        } else if (/^use\s+/i.test(line)) { cur = cur || {sql: '', stats: {}}; cur.db = line.replace(/^use\s+|;\s*$/gi, ''); }
        else if (/^SET timestamp/i.test(line)) { /* 剔除 */ }
        else if (cur) cur.sql += line + '\n';
      });
      push();
    }
    if (fmt === 'sql') items = splitSql(text.replace(/^\s*#.*$/gm, ''), false).items.map(s => ({label: '', sql: s.sql.replace(/\s+/g, ' ')}));
    return {fmt, items: items.map((x, i) => Object.assign({n: i + 1}, x)), total: items.length};
  }
  const FMT_NAME = {sql: 'SQL 文件', xml: 'MyBatis3 Mapper XML', slow: 'MySQL 慢日志'};

  /* ---------- 内置规则 / 执行计划解读 / 索引建议 ---------- */
  const LV_N = {'高': 3, '中': 2, '低': 1, '提示': 0};
  const LV_CLS = {'高': 'bad', '中': 'warn', '低': 'info', '提示': ''};
  const SOAR_MAP = {R001: ['COL.001', '不建议使用 SELECT * 类型查询'], R002: ['CLA.001', '最外层 SELECT 未指定 WHERE 条件'], R003: ['FUN.001', '避免在 WHERE 条件中使用函数或其他运算符'], R004: ['ARG.001', '不建议使用前项通配符查找'], R005: ['CLA.003', '不建议使用 LIMIT 的 OFFSET 做分页'], R006: ['RES.001', 'ORDER BY RAND() 会产生临时表与文件排序'], R011: ['COL.002', 'INSERT 未指定列名'], S001: ['ARG.003', '隐式类型转换会使索引失效'], S002: ['IDX.001', '建议添加索引']};
  const findAny = (inst, db, name) => { if (!inst) return null; if (db && db !== '全部') return tblOf(inst, db, name); for (const d of dbList(inst)) { const t = d.tables.find(x => x.n.toLowerCase() === String(name).toLowerCase()); if (t) return t; } return null; };
  function fingerprint(sql) {
    let s = sql.replace(/--[^\n]*|\/\*[\s\S]*?\*\//g, ' ').replace(/\s+/g, ' ').trim();
    s = s.replace(/'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"/g, '?').replace(/\b0x[0-9a-f]+\b/gi, '?').replace(/\b\d+(?:\.\d+)?\b/g, '?');
    s = s.replace(/\(\s*\?(?:\s*,\s*\?)+\s*\)/g, '(?+)').replace(/(values)\s*\(\?\+?\)(?:\s*,\s*\(\?\+?\))+/i, '$1 (?+)').replace(/limit\s+\?\s*,\s*\?/i, 'limit ?').replace(/limit\s+\?\s+offset\s+\?/i, 'limit ?');
    return s.toLowerCase();
  }
  const fpId = fp => (hs(fp).toString(16).padStart(8, '0') + hs(fp + '#').toString(16).padStart(8, '0')).toUpperCase();
  function anaStmt(sql, inst, db) {
    const c = sql.replace(/\s+/g, ' ').trim(), lp = noStr(c).toLowerCase(), kind = (lp.match(/^\w+/) || [''])[0], F = [], add = (id, lvl, text, fix) => F.push({id, lvl, text, fix: fix || ''});
    const fp = fingerprint(c), tables = [];
    const addT = n => { n = unq(n.split('.').pop()); if (n && !tables.includes(n) && !/^(select|set|values|dual)$/i.test(n)) tables.push(n); };
    (c.match(/\b(?:from|join|update|into)\s+`?[\w.]+`?/gi) || []).forEach(m => addT(m.replace(/^\w+\s+/, '')));
    const fm = c.match(/\bfrom\s+(.+?)(?:\bwhere\b|\bgroup\b|\border\b|\blimit\b|\bhaving\b|$)/i);
    if (fm && fm[1].includes(',') && !/\(/.test(fm[1])) fm[1].split(',').forEach(x => addT(x.trim().split(/\s+/)[0]));
    const wm = c.match(/\bwhere\b(.*?)(?:\bgroup\s+by\b|\border\s+by\b|\blimit\b|\bhaving\b|$)/i), where = wm ? wm[1].trim() : '';
    const orderM = c.match(/\border\s+by\s+`?(?:\w+\.)?(\w+)`?/i), groupM = c.match(/\bgroup\s+by\s+`?(?:\w+\.)?(\w+)`?/i);
    const meta1 = inst && tables.length === 1 ? findAny(inst, db, tables[0]) : null;
    const hasAgg = /\b(count|sum|avg|max|min)\s*\(/i.test(c) || /\bgroup\s+by\b/i.test(c);
    if (/select\s+(?:distinct\s+)?(?:\w+\.)?\*/i.test(c)) add('R001', '低', '使用了 SELECT *，会多读无用列、阻碍覆盖索引', '显式列出需要的列');
    if (/^(update|delete)$/.test(kind) && !where) add('R002', '高', `${kind.toUpperCase()} 没有 WHERE 条件，会修改 / 删除整张表`, '补充 WHERE 条件，必要时分批');
    if (where && /\b(date|year|month|day|hour|lower|upper|substr|substring|concat|ifnull|from_unixtime|unix_timestamp|cast|trim)\s*\(\s*`?\w+/i.test(where)) add('R003', '中', 'WHERE 对列使用了函数 / 运算，索引无法使用', "改写成范围条件，如 created_at >= '2026-10-01' AND created_at < '2026-10-02'");
    if (/\blike\s+(concat\s*\(\s*)?'%/i.test(c)) add('R004', '中', 'LIKE 以 % 开头（前导通配符），无法使用索引', '改为后缀匹配，或使用全文索引 / 搜索引擎');
    const lim = c.match(/\blimit\s+(\d+)\s*,\s*(\d+)/i) || c.match(/\blimit\s+(\d+)\s+offset\s+(\d+)/i);
    const off = lim ? (/offset/i.test(lim[0]) ? +lim[2] : +lim[1]) : 0;
    if (off >= 10000) add('R005', off >= 100000 ? '高' : '中', `深分页：偏移量 ${fmtN(off)}，需要先扫描并丢弃前 ${fmtN(off)} 行`, '改用游标分页：WHERE id > 上次最大 id ORDER BY id LIMIT n');
    if (/order\s+by\s+rand\s*\(\s*\)/i.test(c)) add('R006', '高', 'ORDER BY RAND() 会产生临时表并全量排序', '在应用层随机取 id，或用 id 范围随机');
    if (where && /\bor\b/i.test(lp.replace(/\([^()]*\)/g, ''))) add('R007', '低', 'WHERE 顶层含 OR，可能无法使用单个索引', '改写为 UNION ALL，或为各分支分别建索引');
    if (where && /(!=|<>|\bnot\s+in\b|\bnot\s+like\b)/i.test(lp)) add('R008', '低', '含 != / <> / NOT IN / NOT LIKE，通常无法使用索引', '改写为正向条件');
    if (/\bin\s*\(\s*select\b/i.test(c)) add('R009', '低', 'IN (子查询) 在旧版本优化器下可能变成相关子查询', '改写为 JOIN');
    const inl = c.match(/\bin\s*\(([^()]*)\)/i); if (inl && topSplit(inl[1]).length >= 200) add('R010', '中', `IN 列表有 ${topSplit(inl[1]).length} 项`, '拆批或改用临时表 JOIN');
    if (/^(insert|replace)$/.test(kind) && !/^(insert|replace)\s+(?:ignore\s+)?(?:into\s+)?[\w.`]+\s*\(/i.test(c) && !/\bset\b/i.test(c)) add('R011', '低', 'INSERT 没有指定列清单，表结构变更会写错列', '显式列出列名');
    if (/\bjoin\b/i.test(c) && !/\b(on|using)\b/i.test(lp)) add('R012', '高', 'JOIN 缺少 ON / USING，会产生笛卡尔积', '补充连接条件');
    else if (fm && fm[1].includes(',') && !/\(/.test(fm[1]) && !where) add('R012', '高', '多表逗号连接且没有 WHERE，会产生笛卡尔积', '补充连接条件');
    if (/\bunion\b(?!\s+all)/i.test(c)) add('R013', '低', 'UNION 未指定 ALL，会做去重排序', '确认不需要去重时用 UNION ALL');
    if (/(=|!=|<>)\s*null\b/i.test(lp.replace(/<=>\s*null/g, '').replace(/\bis\s+(not\s+)?null/g, ''))) add('R014', '中', '= NULL / != NULL 永远不成立', '改用 IS NULL / IS NOT NULL');
    if (kind === 'select' && !where && !/\blimit\b/i.test(c) && !hasAgg && /\bfrom\b/i.test(c)) add('R015', '中', 'SELECT 既无 WHERE 也无 LIMIT，可能返回整张表', '加条件或 LIMIT');
    if (kind === 'select' && /\blimit\s+(\d+)\s*$/i.test(c) && +c.match(/\blimit\s+(\d+)\s*$/i)[1] > 1 && !/\border\s+by\b/i.test(c)) add('R016', '低', 'LIMIT 没有 ORDER BY，返回哪些行不确定', '加上 ORDER BY');
    const idx = [], plan = [], pnotes = [];
    if (inst) {
      const tm = tables.map(t => findAny(inst, db, t)).filter(Boolean);
      /* S001 隐式转换：字符串列与数字常量比较 */
      (where.match(/`?(\w+)`?\s*(?:=|in\s*\()\s*\d+\b/gi) || []).forEach(m => { const col = m.match(/^`?(\w+)`?/)[1]; tm.forEach(t => { const cc = (t.cols || []).find(x => x.n.toLowerCase() === col.toLowerCase()); if (cc && /char|text/i.test(cc.t)) add('S001', '中', `列 ${col}（${cc.t}）与数字常量比较，发生隐式类型转换，索引失效`, '把常量加引号，使类型一致'); }); });
      const onCols = [...c.matchAll(/(\w+)\.(\w+)\s*=\s*(\w+)\.(\w+)/g)].flatMap(m => [m[2].toLowerCase(), m[4].toLowerCase()]);
      const planFor = (t, meta, w, driven) => {
        const wi = whereInfo(meta, w || ''), est = estimate(meta, wi, !!w), extra = [], ixOf = n => (meta.idx || []).find(x => x[0] === n);
        let key = null, type = 'ALL', rows = meta.rows || 0;
        if (driven) {
          const lead = (meta.idx || []).find(x => onCols.includes(x[1][0].toLowerCase()));
          if (lead) { key = lead[0]; type = 'ref'; rows = Math.max(1, Math.round((meta.rows || 1) / 20000)); } else extra.push('Using join buffer (hash join)');
        } else if (w) {
          if (est.unique) { type = 'const'; key = ((meta.idx || []).find(x => x[2] && x[1].every(cc => wi.eq.includes(cc.toLowerCase()))) || [])[0] || null; rows = est.rows; }
          else if (est.key) { key = est.key; type = wi.eq.includes(ixOf(key)[1][0].toLowerCase()) ? 'ref' : 'range'; rows = est.rows; }
        } else if (orderM && ixOf('PRIMARY') && ixOf('PRIMARY')[1][0].toLowerCase() === orderM[1].toLowerCase()) { key = 'PRIMARY'; type = 'index'; }
        if (w && type !== 'const') extra.push('Using where');
        const keyCols = key ? ixOf(key)[1].map(x => x.toLowerCase()) : [], eqUsed = keyCols.filter(x => wi.eq.includes(x)).length;
        const oc = orderM && !/rand\s*\(/i.test(c) ? orderM[1].toLowerCase() : '';
        const orderOk = type === 'index' || (keyCols.length > 0 && keyCols[eqUsed] === oc);
        if (oc && !driven && type !== 'const' && !orderOk) extra.push('Using filesort');
        if (/order\s+by\s+rand\s*\(/i.test(c) && !driven) extra.push('Using temporary', 'Using filesort');
        if (groupM && !driven && keyCols[0] !== groupM[1].toLowerCase()) extra.push('Using temporary');
        return {id: 1, select_type: kind === 'update' ? 'UPDATE' : kind === 'delete' ? 'DELETE' : 'SIMPLE', table: t, type, possible_keys: (meta.idx || []).filter(x => wi.cols.includes(x[1][0].toLowerCase()) || (driven && onCols.includes(x[1][0].toLowerCase()))).map(x => x[0]).join(',') || 'NULL', key: key || 'NULL', rows, filtered: type === 'ALL' ? (w ? 10 : 100) : 100, Extra: [...new Set(extra)].join('; ') || 'NULL'};
      };
      if (/^(select|update|delete)$/.test(kind)) {
        tables.slice(0, 4).forEach((t, i) => { const meta = findAny(inst, db, t); if (meta && !meta.view) plan.push(planFor(t, meta, i === 0 ? where : '', i > 0)); });
        plan.forEach(p => {
          const big = p.rows >= 100000 ? '高' : p.rows >= 1000 ? '中' : '低';
          if (p.type === 'ALL') pnotes.push({id: 'P001', lvl: big, text: `表 ${p.table} 全表扫描，预计扫描 ${fmtN(p.rows)} 行`});
          if (p.type === 'index') pnotes.push({id: 'P002', lvl: '低', text: `表 ${p.table} 全索引扫描（${p.key}），随 OFFSET 增大会越来越慢`});
          if (/filesort/.test(p.Extra)) pnotes.push({id: 'P003', lvl: p.rows >= 10000 ? '中' : '低', text: `表 ${p.table} 需要 Using filesort（排序列不在所用索引的下一列）`});
          if (/temporary/.test(p.Extra)) pnotes.push({id: 'P004', lvl: '中', text: `表 ${p.table} 需要 Using temporary（GROUP BY / RAND 排序产生临时表）`});
          if (/join buffer/.test(p.Extra)) pnotes.push({id: 'P006', lvl: p.rows >= 100000 ? '高' : '中', text: `被驱动表 ${p.table} 的连接列没有索引（Using join buffer）`});
        });
      }
      /* S002 索引建议：单表、无 OR、等值列 → 范围列（无范围则 ORDER BY 列） */
      if (meta1 && !meta1.view && where && /^(select|update|delete)$/.test(kind) && !/\bor\b/i.test(lp)) {
        const wi = whereInfo(meta1, where), est = estimate(meta1, wi, true);
        const cols = [...new Set(wi.eq)], rg = wi.rng.filter(x => !cols.includes(x))[0];
        if (rg) cols.push(rg); else if (orderM && (meta1.cols || []).some(x => x.n.toLowerCase() === orderM[1].toLowerCase()) && !cols.includes(orderM[1].toLowerCase())) cols.push(orderM[1].toLowerCase());
        const bad = cols.some(cc => /text|blob|json/i.test(((meta1.cols || []).find(x => x.n.toLowerCase() === cc) || {t: ''}).t));
        const covered = (meta1.idx || []).some(x => x[1].length >= cols.length && cols.every((cc, i) => x[1][i].toLowerCase() === cc));
        if (cols.length && !covered && !bad && !est.unique) {
          const name = 'idx_' + cols.map(cc => cc.replace(/_at$|_id$/, '').slice(0, 14)).join('_'), ddl = `ALTER TABLE \`${tables[0]}\` ADD INDEX \`${name}\` (${cols.map(cc => '`' + cc + '`').join(', ')});`;
          add('S002', meta1.rows >= 1e5 ? '高' : meta1.rows >= 1000 ? '中' : '低', `表 ${tables[0]}（约 ${fmtN(meta1.rows || 0)} 行）缺少覆盖条件列 (${cols.join(', ')}) 的索引`, ddl);
          idx.push({ddl, why: `按「等值列 → 范围列${rg ? '' : '（无范围则 ORDER BY 列）'}」推导；${(meta1.rows || 0) >= 5e6 ? '大表请用 gh-ost / pt-osc 在线添加' : '表不大，可直接添加'}`, big: (meta1.rows || 0) >= 5e6});
        }
      }
    }
    const score = Math.max(0, 100 - F.concat(pnotes).reduce((a, f) => a + ({'高': 20, '中': 10, '低': 3}[f.lvl] || 0), 0));
    const top = F.concat(pnotes).reduce((a, f) => LV_N[f.lvl] > a ? LV_N[f.lvl] : a, -1);
    return {fp, fpId: fpId(fp), kind, tables, findings: F, plan, pnotes, idx, score, top};
  }

  /* ---------- 任务（只在内存；结束后保留约 30 分钟） ---------- */
  const SA = {src: null, parsed: null, inst: 0, db: '', jobs: [], cur: null, soar: false, nextId: 1};
  const RETAIN_MS = 30 * 60 * 1000;
  const hm = ts => { const d = new Date(ts); return pad2(d.getHours()) + ':' + pad2(d.getMinutes()); };
  const mysqlInsts = () => visibleInstances().filter(i => i.type === 'MySQL' || i.type === 'MariaDB');
  function makeJob(o) {
    const job = Object.assign({id: 'J' + (1000 + SA.nextId++), status: 'running', done: 0, startedTs: Date.now(), doneTs: 0, summary: null, demo: false}, o);
    job.items = o.parsed.items.map(x => ({n: x.n, sql: x.sql, label: x.label, stats: x.stats, st: 'waiting', res: null}));
    job.total = job.items.length; SA.jobs.unshift(job); return job;
  }
  function stepJob(job) {
    const it = job.items.find(x => x.st === 'waiting'); if (!it) return finishJob(job);
    const inst = job.inst ? wfInst(job.inst) : null;
    try { it.res = anaStmt(it.sql, inst && inst.status !== 'bad' ? inst : null, job.db); } catch (e) { it.res = {fp: '', fpId: '', kind: '', tables: [], findings: [{id: 'ERR', lvl: '提示', text: '分析异常：' + e.message}], plan: [], pnotes: [], idx: [], score: 100, top: 0}; }
    it.st = 'done'; job.done++;
    if (job.done >= job.total) finishJob(job);
  }
  function finishJob(job) {
    job.status = 'done'; job.doneTs = Date.now();
    const byType = {}, byLevel = {'高': 0, '中': 0, '低': 0, '无': 0}, byFp = {}, byTable = {};
    job.items.forEach(it => {
      const r = it.res, k = r.kind.toUpperCase() || '?'; byType[k] = (byType[k] || 0) + 1;
      byLevel[r.top >= 3 ? '高' : r.top === 2 ? '中' : r.top === 1 ? '低' : '无']++;
      const f = byFp[r.fpId] = byFp[r.fpId] || {fpId: r.fpId, fp: r.fp, count: 0, qt: 0, exam: 0, top: -1}; f.count++; f.qt += (it.stats || {}).qt || 0; f.exam += (it.stats || {}).exam || 0; f.top = Math.max(f.top, r.top);
      r.tables.forEach(t => { byTable[t] = (byTable[t] || 0) + 1; });
    });
    job.summary = {byType, byLevel, fps: Object.values(byFp).sort((a, b) => b.qt - a.qt || b.top - a.top || b.count - a.count), tables: Object.entries(byTable).sort((a, b) => b[1] - a[1]), avg: Math.round(job.items.reduce((a, x) => a + x.res.score, 0) / (job.items.length || 1))};
  }
  let saTimer = 0;
  function saTick() {
    clearTimeout(saTimer);
    const run = SA.jobs.filter(j => j.status === 'running'); if (!run.length) return;
    run.forEach(stepJob); saPaint();
    saTimer = setTimeout(saTick, 420);
  }
  /* 种子：两份最近报告（一份演示报告所有人可读，不过期） */
  (function () {
    const mk = (user, file, inst, db, minsAgo, demo) => { const p = parseUpload(SAMPLE_FILES[file].text), j = makeJob({user, name: SAMPLE_FILES[file].name, fmt: p.fmt, parsed: p, inst, db, demo}); while (j.status === 'running') stepJob(j); j.startedTs = Date.now() - (minsAgo + 1) * 60000; j.doneTs = Date.now() - minsAgo * 60000; return j; };
    mk('rd_zhao', 'xml', 0, '', 5, false);
    const d = mk('dba_li', 'sql', 1, 'order_db', 12, true); d.name = '示例报告 · order_queries.sql'; SA.cur = d.id;
  })();
  const jobVisible = j => (j.demo || ME.super || j.user === ME.login) && (j.demo || j.status === 'running' || Date.now() - j.doneTs < RETAIN_MS);
  const curJob = () => SA.jobs.find(j => j.id === SA.cur && jobVisible(j)) || null;

  function saBody() {
    const run = can('sqlanalyze.run'), insts = mysqlInsts(), job = curJob();
    if (SA.inst && !insts.some(i => i.id === +SA.inst)) { SA.inst = 0; SA.db = ''; }
    const dbs = SA.inst ? dbNames(wfInst(SA.inst)) : [];
    if (SA.inst && SA.db && !dbs.includes(SA.db)) SA.db = '';
    const p = SA.parsed, recent = SA.jobs.filter(jobVisible);
    return `
    ${run ? `
    <div class="grid g2" style="align-items:start">
      <div class="card"><h2>① 上传文件</h2>
        <div class="row"><input type="file" accept=".sql,.xml,.log,.txt" data-onchange="saFile"><span class="muted small">或</span>
          <button class="btn" data-on="saSample" data-k="sql">使用示例 .sql</button><button class="btn" data-on="saSample" data-k="xml">示例 Mapper XML</button><button class="btn" data-on="saSample" data-k="slow">示例慢日志</button></div>
        ${p ? `<div class="hr"></div><div class="row between"><div><b>${esc(SA.src.name)}</b> <span class="tag brand">${FMT_NAME[p.fmt]}</span> <span class="muted small">${(SA.src.size / 1024).toFixed(1)} KB · 已解析 <b>${p.total}</b> 条 SQL</span></div><button class="btn sm" data-on="saClear">移除</button></div>
          ${tbl(p.fmt === 'sql' ? ['#', 'SQL（预览）'] : ['#', p.fmt === 'xml' ? '语句 ID' : '用户 / 库', 'SQL（预览）'], p.items.slice(0, 6).map(x => [x.n].concat(p.fmt === 'sql' ? [] : [esc(x.label)]).concat([`<span class="mono small">${esc(x.sql.length > 110 ? x.sql.slice(0, 110) + ' …' : x.sql)}</span>`])))}
          ${p.total > 6 ? `<div class="small muted" style="margin-top:4px">仅预览前 6 条，共 ${p.total} 条</div>` : ''}` : '<div class="muted small" style="margin-top:10px">尚未选择文件</div>'}
      </div>
      <div class="card"><h2>② 分析选项</h2>
        <div class="form">
          ${fld('实例', `<select data-onchange="saInst"><option value="0">不选择（仅静态规则分析）</option>${insts.map(i => `<option value="${i.id}" ${+SA.inst === i.id ? 'selected' : ''}>${esc(i.name)}（${i.type}${i.role === 'slave' ? ' · 从' : ''}）</option>`).join('')}</select>`)}
          ${SA.inst ? fld('数据库', `<select data-onchange="saDb"><option value="">请选择</option>${dbs.map(d => `<option ${SA.db === d ? 'selected' : ''}>${esc(d)}</option>`).join('')}</select>`, {req: true}) : ''}
          ${fld('SOAR', SA.soar ? '<span class="tag ok">已配置</span> <span class="small muted">/opt/soar/soar</span>' : '<span class="tag">未配置</span>')}
        </div>
        <div class="row" style="margin-top:12px">${B('sqlanalyze.run', '▶ 开始分析', {cls: 'primary', data: {op: 'start'}})}<label class="sw small"><input type="checkbox" data-onchange="saSoar" ${SA.soar ? 'checked' : ''}><i></i>原型：模拟 SOAR 已配置</label></div>
      </div>
    </div>` : ''}
    ${job ? jobCard(job, run) : ''}
    <div class="card" style="margin-top:14px"><h2>最近报告</h2>
      ${tbl(['任务', '文件', '实例 / 库', '创建人', '进度', '状态', '剩余保留', ''], recent.map(j => [`<span class="mono">${j.id}</span>${j.demo ? ' <span class="tag info">演示</span>' : ''}`, `<span class="ellipsis" style="max-width:240px">${esc(j.name)}</span> <span class="tag">${FMT_NAME[j.fmt]}</span>`, j.inst ? `${instName(j.inst)} / ${esc(j.db || '全部')}` : '<span class="muted">静态分析</span>', esc(j.user), `${j.done} / ${j.total}`,
        j.status === 'running' ? '<span class="tag warn">⟳ 分析中</span>' : j.status === 'cancelled' ? '<span class="tag">已取消</span>' : '<span class="tag ok">已完成</span>', j.demo ? '<span class="muted">长期保留</span>' : j.status === 'running' ? '—' : `至 ${hm(j.doneTs + RETAIN_MS)}（约 ${Math.max(1, Math.round((j.doneTs + RETAIN_MS - Date.now()) / 60000))} 分钟）`,
        `<a data-on="saOpen" data-id="${j.id}">查看</a>`]))}</div>`;
  }
  function jobCard(job, run) {
    const s = job.summary, running = job.status === 'running', pct = Math.round(job.done / (job.total || 1) * 100);
    const levTag = r => ['高', '中', '低'].map(l => { const n = r.findings.concat(r.pnotes).filter(f => f.lvl === l).length; return n ? `<span class="tag ${LV_CLS[l]}">${l} ${n}</span>` : ''; }).join(' ') || '<span class="tag ok">无建议</span>';
    return `<div class="card" style="margin-top:14px"><h2>③ 分析结果 <span class="sub">${esc(job.name)} · ${job.inst ? `${instName(job.inst)} / ${esc(job.db || '全部')}` : '静态规则分析'}</span>
        <span class="right">${running && run && (job.user === ME.login || ME.super) ? B('sqlanalyze.run', '✕ 取消分析', {cls: 'danger', data: {op: 'cancel', id: job.id}}) : ''}</span></h2>
      <div class="row"><div class="meter" style="flex:1;height:10px"><b style="width:${pct}%;${job.status === 'cancelled' ? 'background:var(--muted)' : ''}"></b></div><b>${job.done} / ${job.total}</b><span class="muted small">${running ? '分析中…' : job.status === 'cancelled' ? '已取消，已分析的条目保留' : '已完成'}</span></div>
      ${job.status === 'done' && !job.demo ? notice(`<b>${hm(job.doneTs)}</b> 完成，约 <b>${hm(job.doneTs + RETAIN_MS)}</b> 后清理。`, '', '⏱') : ''}
      ${!SA.soar && !running ? notice('SOAR 未配置：本次只输出内置规则分析。', '', 'ℹ') : ''}
      ${s ? `<div class="grid g4" style="margin-top:12px">
        <div class="card kpi" style="padding:10px 12px"><div class="l">语句数</div><div class="v">${job.total}</div><div class="d muted">${Object.entries(s.byType).map(([k, v]) => `${k} ${v}`).join(' · ')}</div></div>
        <div class="card kpi" style="padding:10px 12px"><div class="l">平均评分</div><div class="v" style="color:${s.avg >= 85 ? 'var(--ok)' : s.avg >= 70 ? 'var(--warn)' : 'var(--bad)'}">${s.avg}</div></div>
        <div class="card kpi" style="padding:10px 12px"><div class="l">按最高建议级别</div><div class="row" style="margin-top:6px">${['高', '中', '低', '无'].map(l => `<span class="tag ${LV_CLS[l] || 'ok'}">${l} ${s.byLevel[l]}</span>`).join('')}</div></div>
        <div class="card kpi" style="padding:10px 12px"><div class="l">涉及的表</div><div class="d" style="margin-top:6px">${s.tables.slice(0, 5).map(([t, n]) => `<span class="tag">${esc(t)} × ${n}</span>`).join(' ') || '—'}</div></div></div>
      <h2 style="margin-top:14px">按指纹聚合</h2>
      ${tbl(['指纹 ID', '指纹', '次数', '最高级别'].concat(job.fmt === 'slow' ? ['总耗时', '扫描行数'] : []), s.fps.slice(0, 8).map(f => [`<span class="mono small">${f.fpId}</span>`, `<div class="mono small" style="max-width:420px;white-space:pre-wrap;word-break:break-all">${esc(f.fp.length > 130 ? f.fp.slice(0, 130) + ' …' : f.fp)}</div>`, f.count, f.top >= 0 ? `<span class="tag ${LV_CLS[['提示', '低', '中', '高'][f.top]]}">${['提示', '低', '中', '高'][f.top]}</span>` : '<span class="tag ok">无</span>'].concat(job.fmt === 'slow' ? [`${f.qt.toFixed(1)} s`, fmtN(f.exam)] : [])))}` : ''}
      <h2 style="margin-top:14px">逐条结果</h2>
      ${tbl(['#', 'SQL', '指纹 ID', '类型', '表', '评分', '建议', '状态', ''], job.items.map(it => { const r = it.res; return [it.n, `<div class="mono small" style="min-width:220px;max-width:380px;white-space:pre-wrap;word-break:break-all">${it.label ? `<span class="tag">${esc(it.label)}</span> ` : ''}${esc(it.sql.length > 130 ? it.sql.slice(0, 130) + ' …' : it.sql)}</div>`,
        r ? `<span class="mono small">${r.fpId.slice(0, 8)}…</span>` : '', r ? r.kind.toUpperCase() : '', r ? esc(r.tables.join(', ')) : '',
        r ? `<b style="color:${r.score >= 85 ? 'var(--ok)' : r.score >= 70 ? 'var(--warn)' : 'var(--bad)'}">${r.score}</b>` : '', r ? levTag(r) : '',
        it.st === 'done' ? '<span class="tag ok">已完成</span>' : `<span class="tag">${job.status === 'cancelled' ? '已取消' : '排队'}</span>`, it.st === 'done' ? `<a data-on="saView" data-id="${job.id}" data-n="${it.n}">查看</a>` : '']; }))}</div>`;
  }
  function saDetail(job, it) {
    const r = it.res, rows = a => tbl(['规则', '级别', '说明', '建议 / 改法'], a.map(f => [`<span class="mono">${f.id}</span>`, `<span class="tag ${LV_CLS[f.lvl]}">${f.lvl}</span>`, esc(f.text), f.fix ? `<span class="mono small">${esc(f.fix)}</span>` : '']));
    return `<div class="score" style="margin-bottom:12px">${ring(r.score, '评分')}<div><div class="b">${esc(it.label || '语句 #' + it.n)}</div><div class="small muted">指纹 ID <span class="mono">${r.fpId}</span> · 类型 ${r.kind.toUpperCase()} · 表 ${esc(r.tables.join(', ') || '—')}</div>${it.stats && it.stats.qt ? `<div class="small muted">慢日志：耗时 ${it.stats.qt}s · 扫描 ${fmtN(it.stats.exam)} 行 · 返回 ${fmtN(it.stats.sent)} 行</div>` : ''}</div></div>
      ${code(it.sql, 'sql')}
      <div class="small muted">SQL 指纹</div><div class="mono small" style="margin-bottom:12px;white-space:pre-wrap;word-break:break-all">${esc(r.fp)}</div>
      <h2>内置规则建议 <span class="sub">${r.findings.length} 条</span></h2>${r.findings.length ? rows(r.findings) : '<div class="notice ok"><span>✓</span><div>没有命中内置规则。</div></div>'}
      <h2 style="margin-top:16px">执行计划解读${job.inst ? ` <span class="sub">${instName(job.inst)}</span>` : ''}</h2>
      ${r.plan.length ? `${tbl(['id', 'select_type', 'table', 'type', 'possible_keys', 'key', 'rows', 'filtered', 'Extra'], r.plan.map(p => [p.id, p.select_type, p.table, `<b style="color:${p.type === 'ALL' ? 'var(--bad)' : 'inherit'}">${p.type}</b>`, esc(p.possible_keys), esc(p.key), fmtN(p.rows), p.filtered + '%', esc(p.Extra)]))}${r.pnotes.length ? `<div style="margin-top:8px">${rows(r.pnotes)}</div>` : '<div class="notice ok" style="margin-top:8px"><span>✓</span><div>执行计划没有明显问题。</div></div>'}` : job.inst ? '<div class="muted small">该语句类型 / 表不在所选库内，没有可解读的执行计划。</div>' : '<div class="muted small">需要选择实例。</div>'}
      <h2 style="margin-top:16px">索引建议</h2>${r.idx.length ? r.idx.map(x => `${code(x.ddl, 'sql')}<div class="small muted" style="margin-bottom:8px">${esc(x.why)}${x.big ? '。<span class="tag info">大表 · 建议走在线改表</span>' : ''}</div>`).join('') : `<div class="muted small">${job.inst ? '没有索引建议。' : '需要选择实例才能推导索引建议。'}</div>`}
      <details style="margin-top:16px"><summary style="cursor:pointer;font-weight:600">SOAR 报告 <span class="tag ${SA.soar ? 'ok' : ''}">${SA.soar ? '已配置' : '未配置'}</span></summary>
        <div style="margin-top:8px">${SA.soar ? code(soarReport(it, r, job), '') : notice('SOAR 未配置，只输出内置规则分析。', '', 'ℹ')}</div></details>`;
  }
  function soarReport(it, r, job) {
    const stars = n => '★'.repeat(Math.round(n / 20)) + '☆'.repeat(5 - Math.round(n / 20));
    return `# 优化建议\n\n##  ${stars(r.score)}  ${r.score} 分\n\n\`\`\`sql\n${it.sql}\n\`\`\`\n\n` + (r.findings.length ? r.findings.map(f => { const m = SOAR_MAP[f.id] || [f.id, f.text]; return `* **Item:** ${m[0]}\n* **Severity:** ${f.lvl === '高' ? 'L4' : f.lvl === '中' ? 'L3' : 'L2'}\n* **Content:** ${m[1]}\n* **Case:** ${it.sql.slice(0, 60)}${it.sql.length > 60 ? '…' : ''}\n`; }).join('\n') : '未发现明显问题。\n') + (job.inst ? `\n## 执行计划（online-dsn: ${wfInst(job.inst).name}）\n` + r.plan.map(p => `| ${p.table} | ${p.type} | ${p.key} | ${p.rows} | ${p.Extra} |`).join('\n') : '');
  }
  ON.saFile = (d, el) => {
    const f = el.files && el.files[0]; if (!f) return;
    if (f.size > 5 * 1024 * 1024) { toast('文件超过 5 MB，无法分析'); el.value = ''; return; }
    const rd = new FileReader();
    rd.onload = () => saLoad(f.name, String(rd.result), f.size);
    rd.onerror = () => toast('读取文件失败');
    rd.readAsText(f);
  };
  function saLoad(name, text, size) {
    if (text.includes('\u0000')) return toast('文件含 NUL 字符，无法分析');
    const p = parseUpload(text);
    if (!p.total) return toast('没有解析到任何 SQL');
    if (p.total > 1000) return toast(`一次最多 1000 条，当前 ${p.total} 条`);
    SA.src = {name, size: size || text.length}; SA.parsed = p; toast(`已解析 ${p.total} 条 SQL（${FMT_NAME[p.fmt]}）`); saPaint(true);
  }
  ON.saSample = d => { const s = SAMPLE_FILES[d.k]; saLoad(s.name, s.text, new Blob([s.text]).size); };
  ON.saClear = () => { SA.src = null; SA.parsed = null; saPaint(true); };
  ON.saInst = (d, el) => { SA.inst = +el.value; SA.db = SA.inst ? (dbNames(wfInst(SA.inst))[0] || '') : ''; saPaint(true); };
  ON.saDb = (d, el) => { SA.db = el.value; };
  ON.saSoar = (d, el) => { SA.soar = el.checked; saPaint(true); };
  ON.saOpen = d => { SA.cur = d.id; saPaint(true); window.scrollTo(0, 0); };
  ON.saView = d => { const job = SA.jobs.find(j => j.id === d.id), it = job && job.items.find(x => x.n == d.n); if (it && it.res) openDrawer(`分析报告 · #${it.n}`, saDetail(job, it), true); };
  ACTIONS['sqlanalyze.run'] = d => {
    if (d.op === 'cancel') {
      const j = SA.jobs.find(x => x.id === d.id); if (!j || j.status !== 'running') return toast('任务已结束，不能取消');
      if (!ME.super && j.user !== ME.login) return toast('只能取消自己的任务');
      j.status = 'cancelled'; j.doneTs = Date.now(); j.summary = null; audit('sqlanalyze.run', `取消 SQL 分析任务 ${j.id}`, '成功'); toast('已取消分析（终止运行中的外部程序），已分析的条目保留'); return saPaint(true);
    }
    if (!SA.parsed) return toast('请先选择文件');
    if (SA.inst && !SA.db) return toast('请选择数据库');
    if (SA.jobs.filter(j => j.status === 'running' && j.user === ME.login).length >= 2) return toast('你已有 2 个进行中的分析任务（每人最多 2 个），请等待或取消其中一个');
    const job = makeJob({user: ME.login, name: SA.src.name, fmt: SA.parsed.fmt, parsed: SA.parsed, inst: SA.inst, db: SA.db});
    SA.cur = job.id; audit('sqlanalyze.run', `提交 SQL 分析 ${job.id}：${job.name}（${job.total} 条）`, '成功', SA.inst ? `${wfInst(SA.inst).name} / ${SA.db}` : '静态分析');
    toast(`已提交分析任务 ${job.id}，后台逐条分析中`); saPaint(true); saTimer = setTimeout(saTick, 300);
  };
  function saPaint(force) {
    const el = $('#saBody'); if (!el) return;
    const a = document.activeElement;
    if (!force && a && el.contains(a) && /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName)) return;
    el.innerHTML = saBody();
  }
  pages.sqlanalyze = {title: 'SQL 分析', render() { return `<div id="saBody">${saBody()}</div>`; }};
})();
