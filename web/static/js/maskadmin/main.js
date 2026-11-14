/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: main.js
 * @time: 2026/10/28
 */
// 系统管理 › 数据脱敏（仅超级用户）：批量选择多个库表字段，匹配同一条脱敏规则。
// 流程：① 选字段（跨库、跨表；按表 / 按字段名；疑似敏感；粘贴清单）→ ② 选规则（保留首尾 / 三段式 / 整体隐藏 / 正则）→ 暂存 → 确认差异后保存。
// 所有来自数据库的文字（库表字段名、注释）都用 textContent 写入。
import { api, confirmRun, toast } from '../common.js';
import { t, tp, isZh, fmtNum } from '../i18n.js';
import { maskValue, ruleText, sameRule, ruleProblem, setTranslator } from './algo.js';

setTranslator(t); // algo.js 不直接依赖 i18n.js（要被 node 测试 import），文案翻译函数由页面入口注入

const BASE = '/api/v1/settings/masking';
const $ = (id) => document.getElementById(id);
const SEP = '\u0001';
const MAX_TABLES = 300; // 树里一次最多画多少张表（继续筛选缩小范围）
const MAX_NAMES = 300;

function h(tag, props, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') e.className = v;
    else if (k === 'text') e.textContent = v;
    else if (k === 'checked' || k === 'disabled' || k === 'value' || k === 'indeterminate') e[k] = v;
    else if (k.startsWith('data-')) e.setAttribute(k, v);
    else e.setAttribute(k, v === true ? '' : v);
  }
  for (const c of kids.flat()) if (c !== null && c !== undefined && c !== false) e.append(c.nodeType ? c : document.createTextNode(String(c)));
  return e;
}
const clear = (n) => { n.replaceChildren(); return n; };
// 整句翻译后再把数字做成 <b>：占位值用 \u0001…\u0002 包起来，句子译完再拆（语序随语言，不用拆成碎片）
const bold = (n) => '\u0001' + n + '\u0002';
const richText = (s) => String(s).split(/\u0001(.*?)\u0002/).map((p, i) => (i % 2 ? h('b', { text: p }) : p));
const badge =(text, kind = '', title = '') => h('span', { class: 'mk-badge' + (kind ? ' ' + kind : ''), title, text });
const K = (db, table, field) => [db, table, field].join(SEP).toLowerCase();
const wild = (field) => K('*', '*', field);

// ---- 常量 ----
const SUS = [
  [/(phone|mobile|tel|手机|电话)/i, t('手机号|kind'), { kind: 'keep', prefix: 3, suffix: 4 }],
  [/(id_?card|idcard|cert_?no|id_?number|身份证)/i, t('身份证|kind'), { kind: 'keep', prefix: 3, suffix: 4 }],
  [/(bank_?card|card_?no|银行卡)/i, t('银行卡|kind'), { kind: 'keep', prefix: 6, suffix: 4 }],
  [/(e_?mail|邮箱)/i, t('邮箱|kind'), { kind: 'keep', prefix: 2, suffix: 0 }],
  [/(real_?name|full_?name|emp_?name|cust_?name|holder_?name|receiver_?name|contact_?name|user_?name|(^|\.)name$|姓名)/i, t('姓名|kind'), { kind: 'keep', prefix: 1, suffix: 0 }],
  [/(addr|address|地址)/i, t('地址|kind'), { kind: 'keep', prefix: 6, suffix: 0 }],
  [/(wechat|weixin|微信)/i, t('社交账号|kind'), { kind: 'keep', prefix: 2, suffix: 0 }],
];
const PRESETS = [
  { name: t('手机号 3+4'), rule: { kind: 'keep', prefix: 3, suffix: 4 }, sample: '13812345678' },
  { name: t('身份证 3+4'), rule: { kind: 'keep', prefix: 3, suffix: 4 }, sample: '110101199003071234' },
  { name: t('银行卡 6+4'), rule: { kind: 'keep', prefix: 6, suffix: 4 }, sample: '6222021234567890123' },
  { name: t('姓名 1+0'), rule: { kind: 'keep', prefix: 1, suffix: 0 }, sample: '张三丰' }, // i18n-ignore 试算用的示例原值
  { name: t('邮箱 2+0'), rule: { kind: 'keep', prefix: 2, suffix: 0 }, sample: 'zhangsan@corp.com' },
  { name: t('地址 6+0'), rule: { kind: 'keep', prefix: 6, suffix: 0 }, sample: '北京市海淀区中关村大街1号' }, // i18n-ignore 试算用的示例原值
];
const KINDS = [
  ['keep', t('保留首尾')],
  ['three', t('三段式')],
  ['whole', t('整体隐藏')],
  ['regex', t('自定义正则')],
];

const S = {
  instances: [], inst: null,
  dbs: [], db: '',
  catalogs: new Map(), // 库名 -> Catalog
  manual: [],          // 手动补充的 {db, table, field}
  tables: [],          // {db, name, comment, view, rows, cols: [entry]}
  byKey: new Map(),    // key -> entry
  cfg: null, cfgMap: new Map(), offNote: '',
  staged: new Map(),   // key -> {op, db, table, field, rule}
  sel: new Set(), open: new Set(),
  view: 'table', filter: 'all', q: '',
  rule: { kind: 'keep', prefix: 3, suffix: 4, hide: 2, regex: '^(\\d{3})(\\d+)(\\d{4})$' },
  scope: 'exact',
  samples: ['13812345678', '110101199003071234', '张三丰'], // i18n-ignore 试算用的示例原值
  tab: 'wiz', cfgSel: new Set(),
};

const eng = () => (S.inst && S.inst.engine) || { table: '表', table_n: '张表', field: '字段', row_unit: '行', path: '库.表.字段', hier: '', label: '' }; // i18n-ignore 接口缺省的引擎词，显示时经 words() 翻译
// 引擎词（接口给的是中文：表 / 集合 / 字段 / 列 / 张表 …）：中文界面原样；其它语言取译文并转小写，嵌进句子里（复数词用 张表 / 个集合 的译文）。
const WORD = { // 接口给出的中文引擎词 → 句子里用的小写英文词（键是接口值，不是文案）
  '表': t('表|word'), '集合': t('集合|word'), '字段': t('字段|word'), '列': t('列|word'), // i18n-ignore
  '张表': t('张表|word'), '个集合': t('个集合|word'), '行': t('行|word'), '文档': t('文档|word'), // i18n-ignore
};
const lc = (s) => (isZh ? s : (WORD[s] || tp(s)).toLowerCase());
function words() {
  const e = eng();
  return { table: lc(e.table), tables: isZh ? e.table : lc(e.table_n), field: lc(e.field), rows: lc(e.row_unit), tableN: lc(e.table_n), path: tp(e.path) };
}
const suspect = (f) => SUS.find((s) => s[0].test(f)) || null;
const isNum = (t) => /^(nullable\()?(u?int|bigint|decimal|tinyint|smallint|mediumint|double|float|numeric|number|money|real|long)/i.test(t || '') && !/\|/.test(t || '');
const isMixed = (t) => /\|/.test(t || '');
const isDoc = (t) => /^(json|jsonb|object|array|map|tuple|nested)/i.test(t || '');
const terms = (q) => q.toLowerCase().split(/\s+/).filter(Boolean);
const hay = (s, ts) => { s = s.toLowerCase(); return ts.every((t) => s.includes(t)); };
// 行数的简写：中文用“万 / 亿”，其它语言直接用千分位数字
const fmtRows = (n) => (!isZh ? fmtNum(n) : n >= 1e8 ? (n / 1e8).toFixed(1) + ' 亿' : n >= 1e4 ? (n / 1e4).toFixed(n >= 1e6 ? 0 : 1) + ' 万' : String(n)); // i18n-ignore 仅中文界面走这一支
const pathOf = (db, table, field) => (db === '*' ? t('任意库表 · {field}', { field }) : `${db}.${table}.${field}`);

// ---- 目录与有效状态 ----
function rebuild() {
  S.tables = []; S.byKey = new Map();
  const add = (db, t) => {
    const tb = { db, name: t.name, comment: t.comment || '', view: !!t.view, rows: t.rows, cols: [] };
    for (const c of t.columns || []) {
      const e = { db, table: t.name, field: c.name, type: c.type || '', comment: c.comment || '', key: K(db, t.name, c.name), tb };
      tb.cols.push(e); S.byKey.set(e.key, e);
    }
    S.tables.push(tb);
    return tb;
  };
  const idx = new Map();
  for (const [db, cat] of S.catalogs) for (const t of cat.tables || []) idx.set(db + SEP + t.name, add(db, t));
  for (const m of S.manual) {
    let tb = idx.get(m.db + SEP + m.table);
    if (!tb) { tb = add(m.db, { name: m.table, comment: t('（手动添加）'), rows: 0, columns: [] }); idx.set(m.db + SEP + m.table, tb); }
    if (!tb.cols.some((c) => c.field.toLowerCase() === m.field.toLowerCase())) {
      const e = { db: m.db, table: m.table, field: m.field, type: t('手动添加'), comment: '', key: K(m.db, m.table, m.field), tb };
      tb.cols.push(e); S.byKey.set(e.key, e);
    }
  }
  S.tables.sort((a, b) => (a.db === b.db ? a.name.localeCompare(b.name) : a.db.localeCompare(b.db)));
}

function rebuildCfg() {
  S.cfgMap = new Map();
  for (const it of (S.cfg && S.cfg.items) || []) S.cfgMap.set(K(it.scope_db, it.scope_table, it.field), it);
}

// 某字段当前有效的脱敏状态：先看暂存，再看已配置；精确键优先，其次同名通配。
function effective(e) {
  for (const k of [e.key, wild(e.field)]) {
    const st = S.staged.get(k);
    const cf = S.cfgMap.get(k);
    if (st) {
      if (st.op === 'remove') return { none: true, staged: true };
      if (st.op === 'set') return { rule: st.rule, active: true, staged: true, source: 'policy' };
      if (cf) return { rule: cf.rule, active: st.op === 'enable', staged: true, source: cf.source, legacy: cf.legacy_desc };
    }
    if (cf) return { rule: cf.rule, active: cf.active, staged: false, source: cf.source, legacy: cf.legacy_desc };
  }
  return null;
}
const masked = (e) => { const ef = effective(e); return !!(ef && ef.rule); };

function scopeTables() {
  return S.tables.filter((t) => S.db === '*' || t.db === S.db);
}
function colPass(e, ts) {
  const ef = effective(e), m = !!(ef && ef.rule);
  if (S.filter === 'ok' && !m) return false;
  if (S.filter === 'un' && m) return false;
  if (S.filter === 'sus' && !(suspect(e.field) && !m)) return false;
  if (ts.length && !hay(`${e.db}.${e.table} ${e.tb.comment} ${e.field} ${e.comment}`, ts)) return false;
  return true;
}
function visibleEntries() {
  const ts = terms(S.q), out = [];
  for (const t of scopeTables()) for (const c of t.cols) if (colPass(c, ts)) out.push(c);
  return out;
}

// ---- 渲染：引擎说明、提示、左侧树 ----
function renderEngine() {
  const box = $('mk-engine');
  const e = S.inst && S.inst.engine;
  if (!e) { box.hidden = true; return; }
  box.hidden = false;
  clear(box);
  const cls = { exact: 'ok', name: 'warn', path: 'info' }[e.locate], txt = { exact: t('精确定位'), name: t('保守匹配'), path: t('字段路径匹配') }[e.locate];
  box.append(
    badge(e.label, 'info'),
    h('span', { class: 'item' }, h('span', { class: 'k', text: t('结果列定位') }), badge(txt, cls)),
  );
}

function renderAlerts() {
  const box = clear($('mk-alerts'));
  const c = S.cfg;
  if (!c) { if (S.offNote) box.append(h('div', { class: 'muted small', text: S.offNote })); return; }
  if (!c.policy_ok) box.append(h('div', { class: 'alert err', text: t('脱敏策略表还没建（deploy/sql/230_masking_policy.sql）：只能查看旧配置，不能保存。') }));
  if (!c.masking_on) {
    // 整个页面只有超级用户进得来，所以按钮直接给；后端同样只认超级用户
    const on = h('button', { class: 'btn primary', type: 'button', text: t('开启总开关…') });
    on.addEventListener('click', enableMasking);
    box.append(h('div', { class: 'alert warn mk-alert-act' },
      h('span', { text: t('系统脱敏总开关已关闭：保存的配置暂不生效，查询与导出返回的都是原值。') }), on));
  } else if (!c.export_on) box.append(h('div', { class: 'alert warn', text: t('SQL 导出工单的脱敏已关闭：导出的是原值。') }));
  if (S.offNote) box.append(h('div', { class: 'muted small', text: S.offNote }));
}

// 打开系统脱敏总开关：确认后写系统设置并记系统审计，然后刷新配置（横幅随之变成“已开启”）
function enableMasking() {
  const c = S.cfg || {};
  const lines = [
    t('开启后，所有实例的在线查询结果立即按已启用的脱敏配置脱敏；SQL 导出工单默认同样脱敏（脱敏失败时导出会中止，不会导出原值）。'),
    t('依赖原值的导出（对账、迁移）请先评估：可把 export_data_masking 设为 false 单独关闭导出脱敏。'),
  ];
  if (!c.query_check) lines.push(t('query_check 当前是关闭的：脱敏出错时在线查询会放行未脱敏的结果并提示；敏感库建议同时开启。'));
  confirmRun({
    title: t('开启系统脱敏总开关'), lines, okText: t('确认开启'), code: 'maskadmin.enable', codeName: t('开启脱敏总开关'),
  }, () => api('POST', BASE + '/enable', {})).then(async (r) => {
    if (!r.ok) return;
    await loadConfig();
    refresh();
    toast(r.result && r.result.changed ? t('系统脱敏总开关已开启，立即生效') : t('系统脱敏总开关本来就是开启的'));
  });
}

function applyWords() {
  const w = words();
  $('mk-v-table').textContent = t('按{table}', { table: w.table });
  $('mk-q').placeholder = t('筛选{table} / {field} / 注释', { table: w.tables, field: w.field });
  $('mk-sc-exact').textContent = t('只对所选的“{path}”', { path: w.path });
  $('mk-sc-name').textContent = t('该实例所有{table}里的同名{field}', { table: w.tables, field: w.field });
  $('mk-addpath').hidden = !(S.inst && S.inst.db_type === 'mongo');
}

function stateBadges(e) {
  const ef = effective(e);
  if (ef && ef.rule) {
    const kind = ef.active === false ? '' : 'ok';
    const tags = [ruleText(ef.rule)];
    if (ef.staged) tags.push(t('待保存'));
    if (ef.active === false) tags.push(t('已停用'));
    return [badge(tags.join(' · '), kind, ef.legacy || ''), ...(ef.source === 'legacy' && !ef.staged ? [badge(t('旧配置'), 'info')] : [])];
  }
  if (ef && ef.none) return [badge(t('待移除'), 'warn')];
  const sus = suspect(e.field);
  return sus ? [badge(t('疑似{kind}', { kind: sus[1] }), 'warn')] : [];
}

function colRow(e, label) {
  const cb = h('input', { type: 'checkbox', checked: S.sel.has(e.key), 'aria-label': t('选择 {field}', { field: e.field }), 'data-act': 'csel' });
  return h('div', { class: 'mk-col-row' + (S.sel.has(e.key) ? ' sel' : ''), 'data-k': e.key },
    cb,
    h('span', { class: 'mono', text: label || e.field }),
    h('span', { class: 'muted small ty mono', title: e.type, text: e.type }),
    h('span', { class: 'cm muted small', title: e.comment, text: e.comment }),
    h('span', {}, stateBadges(e)));
}

function renderTree() {
  const box = clear($('mk-tree'));
  if (!S.db) { box.append(h('div', { class: 'mk-empty', text: t('请先选择数据库') })); return; }
  const ts = terms(S.q);
  if (S.view === 'name') return renderByName(box, ts);
  const w = words();
  let shown = 0, hidden = 0, lastDb = null;
  for (const tb of scopeTables()) {
    const cols = tb.cols.filter((c) => colPass(c, ts));
    if (!cols.length) continue;
    if (shown >= MAX_TABLES) { hidden++; continue; }
    shown++;
    if (S.db === '*' && tb.db !== lastDb) { box.append(h('div', { class: 'mk-db-head mono', text: tb.db })); lastDb = tb.db; }
    const tk = tb.db + SEP + tb.name;
    const selN = cols.filter((c) => S.sel.has(c.key)).length;
    const maskedN = tb.cols.filter(masked).length;
    const open = S.open.has(tk) || ts.length > 0;
    const tcb = h('input', { type: 'checkbox', checked: selN > 0 && selN === cols.length, indeterminate: selN > 0 && selN < cols.length, 'aria-label': t('选择 {name} 的全部{field}', { name: tb.name, field: w.field }), 'data-act': 'tsel' });
    const caret = h('button', { class: 'mk-caret', type: 'button', 'aria-label': t('展开'), 'data-act': 'toggle', text: '▸' });
    const row = h('div', { class: 'mk-tbl-row' }, caret, tcb,
      h('span', { class: 'name mono', text: tb.name }), h('span', { class: 'muted small', text: tb.comment }),
      h('span', { class: 'cnt' },
        tb.view ? badge(t('视图'), 'info') : null,
        tb.rows > 0 ? h('span', { class: 'muted small', text: `≈ ${fmtRows(tb.rows)} ${w.rows}` }) : null,
        maskedN ? badge(t('已脱敏 {n}/{total}', { n: maskedN, total: tb.cols.length }), 'ok') : null,
        selN ? badge(t('已选 {n}', { n: selN }), 'info') : null));
    box.append(h('div', { class: 'mk-tbl' + (open ? ' open' : ''), 'data-t': tk },
      row, h('div', { class: 'mk-cols' }, cols.map((c) => colRow(c)))));
  }
  if (hidden) box.append(h('div', { class: 'mk-more', text: t('还有 {n} 张{table}没有显示，请输入关键字缩小范围。', { n: hidden, table: isZh ? eng().table.replace('张', '') : w.tables }) })); // i18n-ignore 与引擎词“张表”比较
  if (!shown) box.append(h('div', { class: 'mk-empty', text: t('没有符合条件的{table}或{field}', { table: w.tables, field: w.field }) }));
}

function renderByName(box, ts) {
  const map = new Map();
  for (const t of scopeTables()) for (const c of t.cols) {
    if (!colPass(c, ts)) continue;
    const k = c.field;
    if (!map.has(k)) map.set(k, { n: k, types: new Set(), items: [] });
    const g = map.get(k); g.types.add(c.type); g.items.push(c);
  }
  const names = [...map.values()].sort((a, b) => b.items.length - a.items.length || a.n.localeCompare(b.n));
  const w = words();
  let shown = 0;
  for (const g of names) {
    if (shown++ >= MAX_NAMES) break;
    const selN = g.items.filter((i) => S.sel.has(i.key)).length;
    const maskedN = g.items.filter(masked).length;
    const sus = suspect(g.n);
    const nk = 'n:' + g.n;
    const open = S.open.has(nk) || ts.length > 0;
    const cb = h('input', { type: 'checkbox', checked: selN > 0 && selN === g.items.length, indeterminate: selN > 0 && selN < g.items.length, 'aria-label': t('选择所有名为 {name} 的{field}', { name: g.n, field: w.field }), 'data-act': 'nsel' });
    const caret = h('button', { class: 'mk-caret', type: 'button', 'aria-label': t('展开'), 'data-act': 'ntoggle', text: '▸' });
    box.append(h('div', { class: 'mk-tbl' + (open ? ' open' : ''), 'data-n': g.n },
      h('div', { class: 'mk-tbl-row' }, caret, cb, h('span', { class: 'name mono', text: g.n }), h('span', { class: 'muted small mono', text: [...g.types].slice(0, 3).join(' / ') }),
        h('span', { class: 'cnt' }, sus ? badge(t('疑似{kind}', { kind: sus[1] }), 'warn') : null,
          h('span', { class: 'muted small', text: t('出现在 {n} {tableN}', { n: g.items.length, tableN: w.tableN }) }),
          maskedN ? badge(t('已脱敏 {n}', { n: maskedN }), 'ok') : null, selN ? badge(t('已选 {n}', { n: selN }), 'info') : null)),
      h('div', { class: 'mk-cols' }, g.items.map((i) => colRow(i, `${i.db}.${i.table}`)))));
  }
  if (names.length > MAX_NAMES) box.append(h('div', { class: 'mk-more', text: t('还有 {n} 个{field}名没有显示，请输入关键字缩小范围。', { n: names.length - MAX_NAMES, field: w.field }) }));
  if (!names.length) box.append(h('div', { class: 'mk-empty', text: t('没有符合条件的{field}', { field: w.field }) }));
}

// ---- 渲染：右侧规则 ----
function stepper(key, v) {
  const minus = h('button', { type: 'button', 'data-step': key, 'data-d': '-1', 'aria-label': t('减一'), text: '−' });
  const plus = h('button', { type: 'button', 'data-step': key, 'data-d': '1', 'aria-label': t('加一'), text: '+' });
  return h('span', { class: 'mk-num' }, minus, h('input', { type: 'text', 'data-num': key, value: String(v), inputmode: 'numeric', 'aria-label': key === 'group' ? t('分组序号') : key === 'prefix' ? t('保留前几位') : t('保留后几位') }), plus);
}

function renderRule() {
  const kinds = clear($('mk-kinds'));
  const whole = S.inst && S.inst.db_type === 'mongo' ? t('整字段隐藏') : t('整列隐藏');
  for (const [k, name] of KINDS) {
    kinds.append(h('button', { class: 'mk-kind', type: 'button', 'data-kind': k, 'aria-pressed': String(S.rule.kind === k) }, h('b', { text: k === 'whole' ? whole : name })));
  }
  const r = S.rule, box = clear($('mk-params'));
  if (r.kind === 'keep') {
    box.append(h('div', { class: 'mk-formula' }, h('span', { text: t('保留前') }), stepper('prefix', r.prefix), h('span', { text: t('位') }),
      h('span', { class: 'mid mono', text: '***' }), h('span', { text: t('保留后') }), stepper('suffix', r.suffix), h('span', { text: t('位') })));
  } else if (r.kind === 'three') {
    const seg = h('div', { class: 'mk-seg', role: 'group' }, [1, 2, 3].map((g) => h('button', { type: 'button', 'data-hide': String(g), class: r.hide === g ? 'on' : '', text: [t('前段'), t('中段'), t('后段')][g - 1] })));
    box.append(h('div', { class: 'mk-row' }, h('span', { text: t('按长度三等分，隐藏') }), seg));
  } else if (r.kind === 'whole') {
    box.append(h('div', { class: 'mk-formula' }, h('span', { text: t('所有值显示为') }), h('span', { class: 'mid mono', text: '***' })));
  } else {
    box.append(h('div', { class: 'mk-row' }, h('input', { id: 'mk-rx', class: 'mono mk-grow', type: 'text', value: r.regex, 'aria-label': t('正则'), autocomplete: 'off' }),
      h('span', { text: t('隐藏第') }), stepper('group', r.hide), h('span', { text: t('组|regex') })),
    h('div', { class: 'muted small', text: t('正则不匹配时原值原样返回。') }));
  }
  const pre = clear($('mk-presets'));
  PRESETS.forEach((p, i) => pre.append(h('button', { class: 'mk-chip', type: 'button', 'data-preset': String(i), text: p.name })));
  renderPreview();
}

function renderPreview() {
  const tbl = clear($('mk-prev'));
  tbl.append(h('tr', {}, h('th', { text: t('原值') }), h('th', { text: t('脱敏后') })));
  S.samples.forEach((s, i) => {
    tbl.append(h('tr', {}, h('td', {}, h('input', { type: 'text', class: 'mono', value: s, 'data-sample': String(i), 'aria-label': t('示例原值 {n}', { n: i + 1 }) })),
      h('td', { class: 'out mono', text: String(maskValue(s, S.rule)) })));
  });
}

function targets() {
  const out = [];
  for (const k of S.sel) { const e = S.byKey.get(k); if (e) out.push(e); }
  return out;
}

function renderImpact() {
  const list = targets(), box = clear($('mk-impact')), btn = $('mk-apply');
  const hint = $('mk-sel-hint');
  const w = words();
  if (!list.length) {
    box.append(h('span', { class: 'muted', text: t('尚未选择{field}', { field: w.field }) }));
    btn.disabled = true; btn.textContent = t('应用到所选{field}', { field: w.field });
    hint.textContent = '';
    return;
  }
  const dbs = new Set(list.map((e) => e.db)), tabs = new Set(list.map((e) => e.db + SEP + e.table));
  hint.textContent = t('已选 {n} 个{field}（{tables} {tableN}，{dbs} 个库）', { n: list.length, field: w.field, tables: tabs.size, tableN: w.tableN, dbs: dbs.size });
  const useName = S.scope === 'name';
  let over = 0, same = 0, num = 0, mixed = 0, doc = 0;
  for (const e of list) {
    const ef = effective(useName ? { key: wild(e.field), field: e.field } : e);
    if (ef && ef.rule) { if (sameRule(ef.rule, S.rule) && ef.active !== false) same++; else over++; }
    if (isNum(e.type)) num++;
    if (isMixed(e.type)) mixed++;
    if (isDoc(e.type)) doc++;
  }
  const ul = h('ul', {});
  ul.append(h('li', {}, t('规则：'), h('b', { text: ruleText(S.rule) }), useName ? t('；范围：该实例所有{table}的同名{field}', { table: w.tables, field: w.field }) : ''));
  const problem = ruleProblem(S.rule);
  if (problem) ul.append(h('li', { class: 'warn', text: problem }));
  if (over) ul.append(h('li', { class: 'warn', text: t('{n} 个{field}已有别的规则（或已停用），应用后会被覆盖', { n: over, field: w.field }) }));
  if (same) ul.append(h('li', { class: 'muted', text: t('{n} 个{field}已是同一规则，无变化', { n: same, field: w.field }) }));
  if (num) ul.append(h('li', { class: 'warn', text: t('{n} 个是数值类型：脱敏后以文本返回（金额 / 数字列通常不需要脱敏）', { n: num }) }));
  if (mixed) ul.append(h('li', { class: 'warn', text: t('{n} 个{field}的类型不统一（如部分文档里是数字）：数字会先转成文本再脱敏，结果一律按文本返回', { n: mixed, field: w.field }) }));
  if (doc) ul.append(h('li', { class: 'warn', text: t('{n} 个是 JSON / Map 类型：对其中所有文本值逐个按规则处理，不区分键名', { n: doc }) }));
  const kinds = new Map();
  for (const e of list) { const u = suspect(e.field); if (u) kinds.set(u[1], (kinds.get(u[1]) || 0) + 1); }
  box.append(h('span', {}, ...richText(t('将对 {tables} {tableN}的 {n} 个{field}设置脱敏', { tables: bold(tabs.size), tableN: w.tableN, n: bold(list.length), field: w.field }))), ul);
  if (kinds.size > 1) {
    ul.append(h('li', { class: 'warn', text: t('所选{field}含 {n} 类疑似敏感数据（{kinds}），同一个规则不一定都合适', { field: w.field, n: kinds.size, kinds: [...kinds].map(([n, c]) => n + ' ' + c).join(isZh ? '、' : ', ') }) }));
    const sb = h('button', { class: 'btn', type: 'button', id: 'mk-smart', text: t('改为：每类用各自的推荐规则（手机号 3+4、姓名 1+0 …）') });
    sb.addEventListener('click', applySmart);
    box.append(h('div', { class: 'mk-top' }, sb));
  }
  btn.disabled = !!problem;
  btn.textContent = t('应用到所选 {n} 个{field}', { n: list.length, field: w.field });
}

// ---- 暂存 / 底栏 ----
function stageSet(e, rule) {
  const useName = S.scope === 'name';
  const key = useName ? wild(e.field) : e.key;
  const cur = effective(useName ? { key, field: e.field } : e);
  if (cur && cur.rule && sameRule(cur.rule, rule) && cur.active !== false) return false;
  S.staged.set(key, { op: 'set', db: useName ? '*' : e.db, table: useName ? '*' : e.table, field: e.field, rule: { ...rule } });
  return true;
}

function applyAll() {
  const list = targets();
  if (!list.length) return;
  const problem = ruleProblem(S.rule);
  if (problem) { toast(problem, 'warn'); return; }
  let n = 0;
  for (const e of list) if (stageSet(e, S.rule)) n++;
  S.sel.clear();
  refresh();
  toast(t('已暂存 {n} 项变更，点“保存并生效”后才会真正生效', { n }));
}

function applySmart() {
  const list = targets();
  let n = 0, skip = 0;
  for (const e of list) {
    const u = suspect(e.field);
    if (!u) { skip++; continue; }
    if (stageSet(e, u[2])) n++;
  }
  S.sel.clear();
  refresh();
  toast(skip ? t('已按类型暂存 {n} 项，{skip} 个非疑似{field}未处理', { n, skip, field: words().field }) : t('已按类型暂存 {n} 项', { n }));
}

function stagedCounts() {
  let add = 0, over = 0, rm = 0, tog = 0;
  for (const [k, st] of S.staged) {
    const cf = S.cfgMap.get(k);
    if (st.op === 'remove') rm++;
    else if (st.op === 'set') { if (cf) over++; else add++; } else tog++;
  }
  return { add, over, rm, tog, n: S.staged.size };
}

function renderBar() {
  const c = stagedCounts(), bar = clear($('mk-bar'));
  const dis = h('button', { class: 'btn', type: 'button', text: t('放弃变更'), disabled: !c.n });
  const save = h('button', { class: 'btn primary', type: 'button', text: t('保存并生效…'), disabled: !c.n || !(S.cfg && S.cfg.policy_ok) });
  dis.addEventListener('click', () => { S.staged.clear(); refresh(); toast(t('已放弃未保存的变更')); });
  save.addEventListener('click', openSave);
  bar.append(h('div', { class: 'sum' }, h('span', {}, ...richText(t('已选 {n} 个{field}', { n: bold(S.sel.size), field: words().field }))), h('span', { class: 'dim', text: '|' }),
    c.n ? [badge(t('待保存 {n} 项', { n: c.n }), 'warn'), h('span', { class: 'muted small', text: t('新增 {add} · 覆盖 {over} · 移除 {rm} · 启停 {tog}', { add: c.add, over: c.over, rm: c.rm, tog: c.tog }) })] : h('span', { class: 'muted small', text: t('没有未保存的变更') })),
  h('span', { class: 'spacer' }), dis, save);
  $('mk-cfg-count').textContent = String(((S.cfg && S.cfg.items) || []).length);
}

// 差异表里的规则文字：正则规则的“隐藏第 N 组”不含表达式本身，只改表达式时前后文字会一样，被确认框当成“无变化”吞掉，所以把表达式带上。
const ruleDiffText = (r) => ruleText(r) + (r.kind === 'regex' && r.regex ? ' ' + r.regex : '');

function openSave() {
  const rows = [], changes = [];
  const items = [...S.staged.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  for (const [k, st] of items) {
    const cf = S.cfgMap.get(k);
    const label = pathOf(st.db, st.table, st.field);
    const from = cf ? ruleDiffText(cf.rule) + (cf.active ? '' : t('（已停用）')) : t('（未配置）');
    let to;
    if (st.op === 'set') { to = ruleDiffText(st.rule); changes.push({ op: 'set', scope_db: st.db, scope_table: st.table, field: st.field, rule: st.rule }); }
    else if (st.op === 'remove') { to = t('（移除）'); changes.push({ op: 'remove', scope_db: st.db, scope_table: st.table, field: st.field }); }
    else { to = st.op === 'enable' ? ruleDiffText(cf.rule) + t('（启用）') : ruleDiffText(cf.rule) + t('（停用）'); changes.push({ op: st.op, scope_db: st.db, scope_table: st.table, field: st.field }); }
    rows.push({ label, from, to });
  }
  confirmRun({
    title: t('确认保存脱敏配置'),
    lines: [t('实例 {name}，共 {n} 项变更。保存后在线查询与 SQL 导出工单立即按新配置脱敏，并写入系统审计。', { name: S.inst.name, n: changes.length })],
    rows, okText: t('确认并生效'), code: 'maskadmin.save', codeName: t('保存脱敏配置'),
  }, () => api('POST', BASE + '/save', { instance_id: S.inst.id, changes })).then(async (r) => {
    if (r.noChange) { // 暂存项与当前配置逐项相同：确认框不会弹出，必须给出反馈并清掉这些无效暂存，否则底栏一直亮着“待保存”
      S.staged.clear();
      refresh();
      toast(t('暂存的变更与当前配置完全一致，没有需要保存的内容，已清除暂存'), 'warn');
      return;
    }
    if (!r.ok) return;
    S.staged.clear();
    await loadConfig();
    refresh();
    const x = r.result || {};
    toast(t('已保存并生效：新增 {added}、覆盖 {updated}、移除 {removed}、启停 {toggled}', { added: x.added || 0, updated: x.updated || 0, removed: x.removed || 0, toggled: x.toggled || 0 }));
  });
}

// ---- 已配置 ----
function renderCfg() {
  const items = ((S.cfg && S.cfg.items) || []);
  const q = terms($('mk-cq').value), rf = $('mk-cf-rule').value, sf = $('mk-cf-state').value;
  const list = items.filter((it) => {
    const key = K(it.scope_db, it.scope_table, it.field);
    const st = S.staged.get(key);
    const active = st && (st.op === 'enable' || st.op === 'disable') ? st.op === 'enable' : it.active;
    if (q.length && !hay(`${it.scope_db}.${it.scope_table}.${it.field} ${it.rule_text}`, q)) return false;
    if (rf && it.rule.kind !== rf) return false;
    if (sf !== '' && String(active ? 1 : 0) !== sf) return false;
    return true;
  });
  const tbl = clear($('mk-ctable'));
  const all = h('input', { type: 'checkbox', 'aria-label': t('全选'), 'data-act': 'call' });
  tbl.append(h('thead', {}, h('tr', {}, h('th', {}, all), ...[t('字段'), t('范围'), t('脱敏方式'), t('来源'), t('示例'), t('启用'), t('更新')].map((x) => h('th', { text: x })))));
  const tb = h('tbody', {});
  for (const it of list) {
    const key = K(it.scope_db, it.scope_table, it.field);
    const st = S.staged.get(key);
    const removed = st && st.op === 'remove';
    const rule = st && st.op === 'set' ? st.rule : it.rule;
    const active = st && (st.op === 'enable' || st.op === 'disable') ? st.op === 'enable' : (st && st.op === 'set' ? true : it.active);
    const cb = h('input', { type: 'checkbox', checked: S.cfgSel.has(key), 'aria-label': t('选择'), 'data-act': 'ccsel' });
    const sw = h('button', { class: 'mk-switch', type: 'button', role: 'switch', 'aria-checked': String(active), 'aria-label': t('启用'), 'data-act': 'cswitch' });
    const tr = h('tr', { 'data-k': key, class: active ? '' : 'off' },
      h('td', {}, cb),
      h('td', { class: 'mono', text: it.wildcard ? it.field : `${it.scope_db}.${it.scope_table}.${it.field}`, }),
      h('td', {}, it.wildcard ? badge(t('任意库表的同名{field}', { field: words().field }), 'info') : badge(t('精确'))),
      h('td', {}, removed ? badge(t('待移除'), 'warn') : [ruleText(rule), st && st.op === 'set' ? badge(t('待保存'), 'warn') : null]),
      h('td', {}, it.source === 'legacy' ? badge(t('旧配置'), 'info', tp(it.legacy_desc)) : badge(t('新策略'))),
      h('td', { class: 'mono muted', text: rule.kind === 'legacy' ? '—' : String(maskValue('13812345678', rule)) }),
      h('td', {}, sw),
      h('td', { class: 'muted small', text: [it.updated_by, it.updated_at].filter(Boolean).join(' · ') }));
    tb.append(tr);
  }
  if (!list.length) tb.append(h('tr', {}, h('td', { colspan: '8', class: 'mk-empty', text: t('没有符合条件的配置') })));
  tbl.append(tb);
}

function stageToggle(keys, on) {
  for (const k of keys) {
    const it = S.cfgMap.get(k);
    if (!it) continue;
    const st = S.staged.get(k);
    if (st && st.op === 'set') continue; // 已暂存新规则的字段，启停跟随新规则（保存后是启用）
    S.staged.set(k, { op: on ? 'enable' : 'disable', db: it.scope_db, table: it.scope_table, field: it.field });
  }
}

// ---- 粘贴清单匹配 ----
function matchLines(lines) {
  const hit = new Set(), miss = [];
  const all = [...S.byKey.values()];
  for (const l of lines) {
    const tail = '.' + l.replace(/[`"']/g, '').toLowerCase();
    let any = false;
    for (const e of all) {
      const k = '.' + `${e.db}.${e.table}.${e.field}`.toLowerCase();
      if (k.length >= tail.length && k.endsWith(tail)) { hit.add(e.key); any = true; }
    }
    if (!any) miss.push(l);
  }
  return { hit, miss };
}

// 把“库.表.字段”拆成三段：PostgreSQL / SQL Server 的表带架构（库.架构.表.字段），MongoDB 的字段路径可以带点。
function parseFull(line) {
  const p = line.replace(/[`"']/g, '').split('.').map((x) => x.trim()).filter(Boolean);
  const t = S.inst && S.inst.db_type;
  if (t === 'mssql' || t === 'pgsql') return p.length >= 4 ? { db: p[0], table: p[1] + '.' + p[2], field: p.slice(3).join('.') } : null;
  if (t === 'mongo') return p.length >= 3 ? { db: p[0], table: p[1], field: p.slice(2).join('.') } : null;
  return p.length === 3 ? { db: p[0], table: p[1], field: p[2] } : null;
}

function addManual(db, table, field) {
  if (!S.manual.some((m) => m.db === db && m.table === table && m.field === field)) S.manual.push({ db, table, field });
  if (!S.catalogs.has(db)) S.catalogs.set(db, { db, tables: [] });
  rebuild();
  const e = S.byKey.get(K(db, table, field));
  if (e) { S.sel.add(e.key); S.open.add(db + SEP + table); }
  return e;
}

function pasteLines() {
  return $('mk-paste').value.split(/\r?\n/).map((s) => s.trim()).filter(Boolean);
}
function renderPasteResult() {
  const lines = pasteLines(), box = $('mk-paste-res');
  if (!lines.length) { box.textContent = ''; return; }
  const r = matchLines(lines);
  let extra = 0;
  if ($('mk-paste-add').checked) extra = r.miss.filter((l) => parseFull(l)).length;
  const left = r.miss.length - extra;
  const field = words().field;
  let msg = t('共 {lines} 行，匹配到 {hit} 个{field}', { lines: lines.length, hit: r.hit.size, field });
  if (extra) msg += t('，另有 {extra} 行按完整路径添加', { extra });
  if (left) {
    const missed = r.miss.filter((l) => !($('mk-paste-add').checked && parseFull(l))).slice(0, 5).join(isZh ? '、' : ', ') + (left > 5 ? '…' : '');
    msg += t('；未匹配 {left} 行：{missed}', { left, missed });
  }
  box.textContent = msg;
}

// ---- 数据加载 ----
async function loadConfig() {
  S.cfg = await api('GET', `${BASE}/config?instance_id=${S.inst.id}`);
  rebuildCfg();
  renderAlerts();
}

async function loadCatalog(db) {
  if (S.catalogs.has(db) && S.catalogs.get(db).tables.length) return;
  const cat = await api('GET', `${BASE}/catalog?instance_id=${S.inst.id}&db=${encodeURIComponent(db)}`);
  S.catalogs.set(db, cat);
  if (cat.truncated && cat.note) toast(t('{db}：{note}', { db, note: cat.note }), 'warn');
}

async function chooseDb(db) {
  S.db = db;
  const tree = $('mk-tree');
  if (!db) { rebuild(); refresh(); return; }
  clear(tree).append(h('div', { class: 'mk-empty', text: t('正在读取字段清单…') }));
  try {
    if (db === '*') {
      const todo = S.dbs.slice();
      let done = 0;
      const worker = async () => {
        while (todo.length) {
          const d = todo.shift();
          try { await loadCatalog(d); } catch (e) { toast(t('{db}：{note}', { db: d, note: e.message }), 'warn'); }
          done++;
          tree.firstChild && (tree.firstChild.textContent = t('正在读取字段清单… {done}/{total}', { done, total: S.dbs.length }));
        }
      };
      await Promise.all([worker(), worker(), worker()]);
    } else {
      await loadCatalog(db);
    }
  } catch (e) {
    clear(tree).append(h('div', { class: 'alert err', text: e.message }));
    return;
  }
  rebuild();
  refresh();
}

async function chooseInstance(id) {
  S.inst = S.instances.find((i) => i.id === id) || null;
  S.dbs = []; S.db = ''; S.catalogs = new Map(); S.manual = []; S.sel.clear(); S.open.clear(); S.staged.clear(); S.cfgSel.clear(); S.cfg = null; S.cfgMap = new Map();
  rebuild();
  renderEngine(); applyWords(); renderRule();
  const dbSel = $('mk-db');
  if (!S.inst) { renderAlerts(); refresh(); return; }
  clear(dbSel).append(h('option', { value: '', text: t('读取中…') }));
  try {
    await loadConfig();
    S.dbs = (await api('GET', `${BASE}/databases?instance_id=${S.inst.id}`)) || [];
  } catch (e) {
    clear(dbSel).append(h('option', { value: '', text: t('（读取失败）') }));
    $('mk-alerts').append(h('div', { class: 'alert err', text: e.message }));
    refresh();
    return;
  }
  clear(dbSel).append(h('option', { value: '', text: S.dbs.length ? t('请选择数据库（共 {n} 个）', { n: S.dbs.length }) : t('没有可用的库') }));
  if (S.dbs.length > 1 && S.dbs.length <= 30) dbSel.append(h('option', { value: '*', text: t('全部库（{n}）', { n: S.dbs.length }) }));
  for (const d of S.dbs) dbSel.append(h('option', { value: d, text: d }));
  if (S.dbs.length === 1) { dbSel.value = S.dbs[0]; await chooseDb(S.dbs[0]); return; }
  refresh();
}

function refresh() {
  renderTree(); renderImpact(); renderBar();
  if (S.tab === 'cfg') renderCfg();
}

// ---- 事件 ----
function setTab(t) {
  S.tab = t;
  $('mk-wiz').hidden = t !== 'wiz'; $('mk-cfgsec').hidden = t !== 'cfg';
  $('mk-t-wiz').setAttribute('aria-selected', String(t === 'wiz')); $('mk-t-cfg').setAttribute('aria-selected', String(t === 'cfg'));
  if (t === 'cfg') renderCfg();
}
$('mk-t-wiz').addEventListener('click', () => setTab('wiz'));
$('mk-t-cfg').addEventListener('click', () => setTab('cfg'));

$('mk-inst').addEventListener('change', () => chooseInstance(Number($('mk-inst').value)));
$('mk-db').addEventListener('change', () => chooseDb($('mk-db').value));
let qTimer = 0;
$('mk-q').addEventListener('input', () => { S.q = $('mk-q').value; clearTimeout(qTimer); qTimer = setTimeout(renderTree, 150); });
$('mk-v-table').addEventListener('click', () => { S.view = 'table'; $('mk-v-table').setAttribute('aria-pressed', 'true'); $('mk-v-name').setAttribute('aria-pressed', 'false'); renderTree(); });
$('mk-v-name').addEventListener('click', () => { S.view = 'name'; $('mk-v-name').setAttribute('aria-pressed', 'true'); $('mk-v-table').setAttribute('aria-pressed', 'false'); renderTree(); });
document.querySelectorAll('#mk [data-f]').forEach((b) => b.addEventListener('click', () => {
  S.filter = b.dataset.f;
  document.querySelectorAll('#mk [data-f]').forEach((x) => x.setAttribute('aria-pressed', String(x === b)));
  renderTree();
}));
$('mk-b-all').addEventListener('click', () => { for (const e of visibleEntries()) S.sel.add(e.key); refresh(); });
$('mk-b-none').addEventListener('click', () => { S.sel.clear(); refresh(); });
$('mk-b-sus').addEventListener('click', () => {
  let n = 0;
  for (const t of scopeTables()) for (const c of t.cols) if (suspect(c.field) && !masked(c)) { S.sel.add(c.key); n++; }
  refresh();
  toast(t('已选中 {n} 个疑似敏感且还没脱敏的{field}', { n, field: words().field }));
});

$('mk-tree').addEventListener('click', (ev) => {
  const act = ev.target.dataset && ev.target.dataset.act;
  const row = ev.target.closest('.mk-tbl');
  if (!row) return;
  if (act === 'toggle') { const k = row.dataset.t; if (S.open.has(k)) S.open.delete(k); else S.open.add(k); row.classList.toggle('open'); }
  if (act === 'ntoggle') { const k = 'n:' + row.dataset.n; if (S.open.has(k)) S.open.delete(k); else S.open.add(k); row.classList.toggle('open'); }
});
$('mk-tree').addEventListener('change', (ev) => {
  const act = ev.target.dataset && ev.target.dataset.act;
  const row = ev.target.closest('.mk-tbl');
  const on = ev.target.checked;
  const set = (keys) => keys.forEach((k) => (on ? S.sel.add(k) : S.sel.delete(k)));
  if (act === 'csel') set([ev.target.closest('.mk-col-row').dataset.k]);
  else if (act === 'tsel' && row) {
    const ts = terms(S.q);
    const t = S.tables.find((x) => x.db + SEP + x.name === row.dataset.t);
    if (t) set(t.cols.filter((c) => colPass(c, ts)).map((c) => c.key));
  } else if (act === 'nsel' && row) set(visibleEntries().filter((e) => e.field === row.dataset.n).map((e) => e.key));
  else return;
  refresh();
});

// 右侧规则
$('mk-kinds').addEventListener('click', (ev) => { const b = ev.target.closest('[data-kind]'); if (!b) return; S.rule.kind = b.dataset.kind; renderRule(); renderImpact(); });
$('mk-params').addEventListener('click', (ev) => {
  const st = ev.target.closest('[data-step]'), hd = ev.target.closest('[data-hide]');
  if (st) {
    const key = st.dataset.step === 'group' ? 'hide' : st.dataset.step;
    const max = key === 'hide' ? 9 : 64, min = key === 'hide' ? 1 : 0;
    S.rule[key] = Math.max(min, Math.min(max, S.rule[key] + Number(st.dataset.d)));
    renderRule(); renderImpact();
  }
  if (hd) { S.rule.hide = Number(hd.dataset.hide); renderRule(); renderImpact(); }
});
$('mk-params').addEventListener('input', (ev) => {
  const num = ev.target.dataset && ev.target.dataset.num;
  if (num) {
    const key = num === 'group' ? 'hide' : num;
    const v = parseInt(ev.target.value, 10);
    if (!Number.isNaN(v)) { S.rule[key] = Math.max(key === 'hide' ? 1 : 0, Math.min(key === 'hide' ? 9 : 64, v)); renderPreview(); renderImpact(); }
  }
  if (ev.target.id === 'mk-rx') { S.rule.regex = ev.target.value; renderPreview(); renderImpact(); }
});
$('mk-params').addEventListener('change', (ev) => { if (ev.target.dataset && ev.target.dataset.num) renderRule(); });
$('mk-presets').addEventListener('click', (ev) => {
  const b = ev.target.closest('[data-preset]');
  if (!b) return;
  const p = PRESETS[Number(b.dataset.preset)];
  S.rule = { ...S.rule, ...p.rule };
  S.samples = [p.sample, ...S.samples.filter((x) => x !== p.sample)].slice(0, 3);
  renderRule(); renderImpact();
});
$('mk-prev').addEventListener('input', (ev) => {
  const i = ev.target.dataset && ev.target.dataset.sample;
  if (i === undefined) return;
  S.samples[Number(i)] = ev.target.value;
  ev.target.closest('tr').querySelector('.out').textContent = String(maskValue(ev.target.value, S.rule));
});
document.querySelectorAll('input[name=mk-scope]').forEach((r) => r.addEventListener('change', () => { S.scope = r.value; renderImpact(); }));
$('mk-apply').addEventListener('click', applyAll);

// 已配置
for (const id of ['mk-cq', 'mk-cf-rule', 'mk-cf-state']) $(id).addEventListener('input', renderCfg);
$('mk-ctable').addEventListener('change', (ev) => {
  const act = ev.target.dataset && ev.target.dataset.act;
  if (act === 'call') {
    ev.target.closest('table').querySelectorAll('tbody tr[data-k]').forEach((tr) => { if (ev.target.checked) S.cfgSel.add(tr.dataset.k); else S.cfgSel.delete(tr.dataset.k); });
    renderCfg();
  } else if (act === 'ccsel') {
    const k = ev.target.closest('tr').dataset.k;
    if (ev.target.checked) S.cfgSel.add(k); else S.cfgSel.delete(k);
  }
});
$('mk-ctable').addEventListener('click', (ev) => {
  if (!(ev.target.dataset && ev.target.dataset.act === 'cswitch')) return;
  const k = ev.target.closest('tr').dataset.k;
  stageToggle([k], ev.target.getAttribute('aria-checked') !== 'true');
  renderCfg(); renderBar();
});
$('mk-c-on').addEventListener('click', () => { stageToggle([...S.cfgSel], true); renderCfg(); renderBar(); });
$('mk-c-off').addEventListener('click', () => { stageToggle([...S.cfgSel], false); renderCfg(); renderBar(); });
$('mk-c-del').addEventListener('click', () => {
  if (!S.cfgSel.size) return;
  for (const k of S.cfgSel) { const it = S.cfgMap.get(k); if (it) S.staged.set(k, { op: 'remove', db: it.scope_db, table: it.scope_table, field: it.field }); }
  const n = S.cfgSel.size;
  S.cfgSel.clear();
  renderCfg(); renderBar();
  toast(t('已把 {n} 项加入“待移除”，保存后生效', { n }));
});
$('mk-c-edit').addEventListener('click', () => {
  const its = [...S.cfgSel].map((k) => S.cfgMap.get(k)).filter((it) => it && !it.wildcard);
  if (!its.length) { toast(t('请先勾选要改规则的字段（通配项请在向导里按字段名选）'), 'warn'); return; }
  S.sel.clear();
  for (const it of its) addManual(it.scope_db, it.scope_table, it.field);
  if (!S.db) { S.db = '*'; if ([...$('mk-db').options].some((o) => o.value === '*')) $('mk-db').value = '*'; }
  S.filter = 'all';
  setTab('wiz');
  refresh();
  toast(t('已把 {n} 个{field}带回向导，选好新规则后应用', { n: its.length, field: words().field }));
});

// 手动补充路径（MongoDB）
$('mk-ap-add').addEventListener('click', () => {
  const v = $('mk-ap-in').value.trim(), i = v.indexOf('.');
  if (i < 1 || i === v.length - 1) { toast(t('格式：集合.字段路径，例如 users.profile.wechat'), 'warn'); return; }
  const db = S.db && S.db !== '*' ? S.db : S.dbs[0];
  if (!db) { toast(t('请先选择数据库'), 'warn'); return; }
  addManual(db, v.slice(0, i), v.slice(i + 1));
  $('mk-ap-in').value = '';
  refresh();
  toast(t('已添加 {path} 并选中', { path: `${db}.${v}` }));
});

// 粘贴清单
$('mk-paste-open').addEventListener('click', () => { $('mk-m-paste').hidden = false; $('mk-paste').focus(); });
$('mk-p-close').addEventListener('click', () => { $('mk-m-paste').hidden = true; });
$('mk-paste').addEventListener('input', renderPasteResult);
$('mk-paste-add').addEventListener('change', renderPasteResult);
$('mk-p-ok').addEventListener('click', () => {
  const lines = pasteLines();
  const r = matchLines(lines);
  for (const k of r.hit) S.sel.add(k);
  let extra = 0;
  if ($('mk-paste-add').checked) {
    for (const l of r.miss) {
      const f = parseFull(l);
      if (f && S.db && addManual(f.db, f.table, f.field)) extra++;
    }
  }
  $('mk-m-paste').hidden = true;
  setTab('wiz');
  refresh();
  const field = words().field;
  toast(r.miss.length - extra
    ? t('已选中 {n} 个{field}，{left} 行没匹配到', { n: r.hit.size + extra, field, left: r.miss.length - extra })
    : t('已选中 {n} 个{field}', { n: r.hit.size + extra, field }));
});
document.addEventListener('keydown', (ev) => { if (ev.key === 'Escape') $('mk-m-paste').hidden = true; });
window.addEventListener('beforeunload', (ev) => { if (S.staged.size) { ev.preventDefault(); ev.returnValue = ''; } });

// ---- 初始化 ----
(async function init() {
  try {
    S.instances = (await api('GET', BASE + '/instances')) || [];
  } catch (e) {
    $('mk-alerts').append(h('div', { class: 'alert err', text: e.message }));
    return;
  }
  const sel = clear($('mk-inst'));
  const ok = S.instances.filter((i) => i.supported);
  if (!ok.length) {
    sel.append(h('option', { value: '', text: t('（无）') }));
    $('mk-alerts').append(h('div', { class: 'alert warn', text: t('没有可配置脱敏的实例。') }));
    renderRule(); renderBar();
    return;
  }
  for (const i of ok) sel.append(h('option', { value: String(i.id), text: t('{name}（{label}）', { name: i.name, label: i.label }) + (i.count ? t(' · 已配置 {n}', { n: i.count }) : '') }));
  const off = S.instances.length - ok.length;
  if (off) S.offNote = t('另有 {n} 个实例不在脱敏范围内（无表字段结构的类型）。', { n: off });
  renderRule(); applyWords();
  await chooseInstance(ok[0].id);
})();
