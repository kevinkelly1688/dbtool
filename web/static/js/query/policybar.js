/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: policybar.js
 * @time: 2026/10/28
 */
// 工具栏下方的限制策略提示条：当前用户在 实例 × 库 上的行数上限、权限到期天数、哪些表无权限（去申请）、本库敏感字段数、超时与语句规则。
// 数据来自 /meta 接口返回的 policy（服务端按真实授权计算）；全局开关来自页面 data- 属性。
import { t, isZh } from '../i18n.js';

function chip(cls, text) {
  const s = document.createElement('span');
  s.className = 'chip' + (cls ? ' ' + cls : '');
  s.textContent = text;
  return s;
}

function withLink(c, href, text) {
  if (!href) return c;
  const a = document.createElement('a');
  a.href = href;
  a.target = '_blank';
  a.rel = 'noopener';
  a.textContent = text;
  c.append(a);
  return c;
}

const nf = (n) => Number(n).toLocaleString('en-US');
const LIST_SEP = isZh ? '、' : ', '; // 表名列表的分隔符

// PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦：脱敏总开关关闭时的提示与只读范围（语句规则见使用文档与 docs/engine-*.md）。
const UNMASKED = t('数据脱敏总开关已关闭，该类型的查询结果不做脱敏');
const READONLY_TEXT = {
  pgsql: t('只读：SELECT / WITH / VALUES / TABLE / EXPLAIN（不含 ANALYZE）/ SHOW 参数，一次一条'),
  clickhouse: t('只读：SELECT / SHOW / DESCRIBE / EXISTS / EXPLAIN，一次一条'),
  mongo: t('只读：find / findOne / count / distinct / aggregate / getIndexes / stats，一次一条'),
  oracle: t('只读：SELECT / WITH / EXPLAIN PLAN FOR，函数只放行内置函数，一次一条'),
  dm: t('只读：SELECT / WITH / EXPLAIN，函数只放行内置函数，一次一条'),
};

// opts: { meta, db, dbType, global: {timeout, disableStar, masking}, applyHref }
// 提示条只显示出了问题、需要处理的：14 天内到期、其余表无权限（去申请）、暂无权限、脱敏开关没开却有敏感字段 / Redis 不脱敏。
// 正常状态的说明（权限范围与行数上限、到期日、脱敏已开启、禁止 SELECT *、超时、只读范围）不占版面，合成文字作为返回值，
// 由调用方放进“运行”按钮的悬停提示；没有要显示的就把整条隐藏。
export function renderPolicy(container, { meta, db, dbType, global, applyHref }) {
  const chips = [];
  const quiet = [];
  const redis = dbType === 'redis';
  const multi = dbType === 'pgsql' || dbType === 'mongo' || dbType === 'clickhouse' || dbType === 'oracle' || dbType === 'dm';
  const p = meta && meta.policy;
  if (p) {
    switch (p.mode) {
      case 'admin':
        quiet.push(t('管理员 · 不受库表授权限制 · 最多 {n} 行', { n: nf(p.max_rows) }));
        break;
      case 'group':
        quiet.push(t('资源组内实例 · 免库表授权 · 最多 {n} 行', { n: nf(p.max_rows) }));
        break;
      case 'priv': {
        const what = p.all_tables ? (redis ? `db${db}` : dbType === 'mongo' ? t('{db} 全部集合', { db }) : t('{db} 全部表', { db })) : (p.tables || []).join(LIST_SEP);
        const range = p.min_rows && p.min_rows !== p.max_rows ? `${nf(p.min_rows)}–${nf(p.max_rows)}` : nf(p.max_rows || p.min_rows);
        quiet.push(t('查询权限：{what} · 最多 {range} 行', { what, range }));
        if (p.expire) {
          if (p.expire_days <= 14) chips.push(chip('warn', t('权限到期 {date}（{n} 天后）', { date: p.expire, n: Math.max(p.expire_days, 0) })));
          else quiet.push(t('权限到期 {date}', { date: p.expire }));
        }
        if (!p.all_tables && meta.locked_total > 0) {
          const locked = dbType === 'mongo' ? t('其余 {n} 个集合无权限 · ', { n: meta.locked_total }) : t('其余 {n} 张表无权限 · ', { n: meta.locked_total });
          chips.push(withLink(chip('warn', locked), applyHref(), t('去申请')));
        }
        break;
      }
      case 'none': {
        const none = p.latest_expired ? t('你在 {db} 暂无查询权限（权限已于 {date} 过期） · ', { db, date: p.latest_expired }) : t('你在 {db} 暂无查询权限 · ', { db });
        chips.push(withLink(chip('bad', none), applyHref(), t('去申请')));
        break;
      }
      default:
    }
  }
  if (redis) {
    chips.push(chip('warn', t('Redis 查询结果不做脱敏')));
  } else if (multi) {
    if (global.masking) quiet.push(t('数据脱敏已启用')); else chips.push(chip('warn', UNMASKED));
  } else if (meta && meta.sensitive_total > 0) {
    if (global.masking) quiet.push(t('数据脱敏：开启（本库 {n} 个敏感字段）', { n: meta.sensitive_total }));
    else chips.push(chip('warn', t('本库有 {n} 个敏感字段，脱敏开关未开启', { n: meta.sensitive_total })));
  } else if (global.masking) {
    quiet.push(t('数据脱敏已启用'));
  }
  if (global.disableStar && !redis && !multi) quiet.push(t('禁止 SELECT *'));
  quiet.push(t('超时 {n} 秒', { n: global.timeout }));
  quiet.push(READONLY_TEXT[dbType] || (redis ? t('只读：白名单命令（GET / HGETALL / LRANGE / SCAN …），一次一条') : dbType === 'mssql' ? t('只读：SELECT / WITH（可 DECLARE @变量 打头）') : t('只读：SELECT / WITH / SHOW / DESC / EXPLAIN')));
  container.replaceChildren(...chips);
  container.hidden = chips.length === 0;
  return quiet.join('\n');
}
