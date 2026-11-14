/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: vocab.js
 * @time: 2026/10/28
 */
// SQL 防火墙页面的词表与小工具。所有来自服务端 / 数据库的文字一律用 textContent 写入（不解析 HTML）。
import { t, tp, isZh, fmtNum as fmt } from '../i18n.js';

const SEP = isZh ? '、' : ', '; // 列举分隔符

export function h(tag, props, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') e.className = v;
    else if (k === 'text') e.textContent = v;
    else if (k === 'checked' || k === 'disabled' || k === 'value' || k === 'selected') e[k] = v;
    else if (k.startsWith('on') && typeof v === 'function') e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v === true ? '' : v);
  }
  for (const c of kids.flat()) if (c !== null && c !== undefined && c !== false) e.append(c.nodeType ? c : document.createTextNode(String(c)));
  return e;
}
export const clear = (n) => { n.replaceChildren(); return n; };
export const $ = (id) => document.getElementById(id);

// 整句翻译里嵌 <b> / <code> / <span class="mono">：占位值用 B() / C() / M() 包起来，译完再用 rich() 拆成节点（语序随语言，不用把句子拆碎）。
export const B = (s) => '\u0001' + s + '\u0002';
export const C = (s) => '\u0003' + s + '\u0004';
export const M = (s) => '\u0005' + s + '\u0006';
export function rich(s) {
  return String(s).split(/(\u0001.*?\u0002|\u0003.*?\u0004|\u0005.*?\u0006)/).filter((p) => p !== '').map((p) => {
    if (p[0] === '\u0001') return h('b', { text: p.slice(1, -1) });
    if (p[0] === '\u0003') return h('code', { text: p.slice(1, -1) });
    if (p[0] === '\u0005') return h('span', { class: 'mono', text: p.slice(1, -1) });
    return p;
  });
}
// 加粗的前导句后面紧跟正文时，中文不加空格，其它语言补一个
export const GAP = isZh ? '' : ' ';

export const SCENES = { query: t('在线查询'), workflow: t('SQL 工单'), export: t('数据导出') };

// 动作：由严到宽。cls 用在徽标与动作卡片上。
export const ACTIONS = {
  deny: { t: t('拒绝'), cls: 'crit' },
  reason: { t: t('要求填写原因'), cls: 'info' },
  limit: { t: t('限制返回行数'), cls: 'info' },
  mask: { t: t('强制脱敏'), cls: 'info' },
  warn: { t: t('仅告警'), cls: '' },
};
export const RESULTS = { ...ACTIONS, observe: { t: t('观察命中'), cls: 'warn' } };

export const STMT = ['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'CREATE', 'ALTER', 'DROP', 'TRUNCATE'];
export const FEATURES = {
  no_where: t('没有 WHERE'), no_limit: t('没有 LIMIT'), cartesian: t('笛卡尔积'), multi_stmt: t('一次提交多条语句'),
  select_star: 'SELECT *', leading_wildcard: t('LIKE 以 % 开头'), big_in: t('IN 列表过长（超过 1000 项）'),
};
export const COND_TYPES = {
  stmt: t('语句类型'), feature: t('结构特征'), keyword: t('关键字 / 函数'), regex: t('正则（SQL 文本）'), object: t('涉及库表（列）'),
  scan_rows: t('预估扫描行数 ≥'), affect_rows: t('预估影响行数 ≥'), time: t('时间'), fingerprint: t('SQL 指纹 ='),
};
// 新增条件时的缺省取值
export const COND_DEFAULT = {
  stmt: () => ['SELECT'], feature: () => 'no_where', keyword: () => [], regex: () => '', object: () => [],
  scan_rows: () => 1000000, affect_rows: () => 100000, time: () => 'offhours', fingerprint: () => '',
};

export const asList = (v) => (Array.isArray(v) ? v.map(String) : v === undefined || v === null || v === '' ? [] : [String(v)]);
const num = (v) => fmt(Number(v));

// 一个条件的展示片段：{k: 标签, v: 内容, mono: 是否等宽}
export function condParts(c) {
  const v = c.v;
  switch (c.t) {
    case 'stmt': return { k: t('语句'), v: asList(v).join(' / ') };
    case 'feature': return { v: FEATURES[v] || String(v) };
    case 'keyword': return { k: t('含'), v: asList(v).join(' / ') };
    case 'regex': return { k: t('正则'), v: String(v), mono: true };
    case 'object': return { k: t('涉及'), v: asList(v).join(SEP), mono: true };
    case 'scan_rows': return { k: t('预估扫描 ≥'), v: t('{n} 行', { n: num(v) }) };
    case 'affect_rows': return { k: t('预估影响 ≥'), v: t('{n} 行', { n: num(v) }) };
    case 'time': return { v: v === 'offhours' ? t('非工作时间') : t('工作时间') };
    case 'fingerprint': return { k: t('指纹'), v: String(v), mono: true };
  }
  return { v: String(c.t) };
}

export function condChip(c, pendingTypes = []) {
  const p = condParts(c);
  const pending = pendingTypes.includes(c.t);
  return h('span', { class: 'fw-chip' + (p.mono ? ' mono' : '') + (pending ? ' pending' : ''), title: pending ? t('暂未接入') : null },
    p.k ? h('span', { class: 'k', text: p.k }) : null, p.v);
}

export function badge(text, kind = '', title = '') {
  return h('span', { class: 'fw-badge' + (kind ? ' ' + kind : ''), title: title || null, text });
}

export function actionBadge(a, arg) {
  const x = ACTIONS[a] || { t: a, cls: '' };
  return badge(x.t + (a === 'limit' && arg ? ' ' + arg : ''), x.cls);
}

export function resultBadge(effective, would) {
  if (effective === 'observe') {
    const w = ACTIONS[would];
    return badge(t('观察命中'), 'warn', w ? t('规则生效后将执行：{action}', { action: w.t }) : '');
  }
  return actionBadge(effective);
}

// 把 text 里出现的 terms（不区分大小写，长的优先）包成 <mark>，返回节点数组；不使用 innerHTML。
export function highlightNodes(text, terms) {
  const list = [...new Set((terms || []).map((t) => String(t)).filter(Boolean))].sort((a, b) => b.length - a.length);
  if (!list.length) return [document.createTextNode(text)];
  const low = text.toLowerCase();
  const marks = [];
  for (const t of list) {
    const lt = t.toLowerCase();
    for (let i = low.indexOf(lt); i >= 0; i = low.indexOf(lt, i + lt.length)) {
      if (!marks.some((m) => i < m[1] && i + lt.length > m[0])) marks.push([i, i + lt.length]);
    }
  }
  marks.sort((a, b) => a[0] - b[0]);
  const out = [];
  let pos = 0;
  for (const [s, e] of marks) {
    if (s > pos) out.push(document.createTextNode(text.slice(pos, s)));
    out.push(h('mark', { text: text.slice(s, e) }));
    pos = e;
  }
  if (pos < text.length) out.push(document.createTextNode(text.slice(pos)));
  return out;
}

// 环境显示名：取 meta.envs（环境字典）里的名称，字典里没有就显示编码。
export function envName(meta, code) {
  const e = ((meta && meta.envs) || []).find((x) => x.code === code);
  return tp(e ? e.name : code);
}

// 规则的“适用范围”两行说明
export function scopeText(r, meta) {
  const scenes = (r.scenes || []).map((s) => SCENES[s] || s).join(' · ');
  const env = !r.env || r.env === 'all' ? t('全部实例') : t('仅{name}实例', { name: envName(meta, r.env) });
  const parts = [env];
  if ((r.instance_ids || []).length) {
    const names = (r.instance_ids || []).map((id) => (meta && meta.instances.find((i) => i.id === id) || {}).name || '#' + id);
    parts.push(names.length <= 2 ? t('限定 {names}', { names: names.join(SEP) }) : t('限定 {names} 等 {n} 个', { names: names.slice(0, 2).join(SEP), n: names.length }));
  }
  if (r.group_mode !== 'all' && (r.group_ids || []).length) {
    const names = (r.group_ids || []).map((id) => (meta && meta.groups.find((g) => g.id === id) || {}).name || '#' + id);
    const list = names.join(SEP);
    parts.push(r.group_mode === 'only' ? t('仅 {names}', { names: list }) : t('排除 {names}', { names: list }));
  }
  return { scenes, sub: parts.join(' · ') };
}

export function fmtNum(n) { return fmt(Number(n || 0)); }
