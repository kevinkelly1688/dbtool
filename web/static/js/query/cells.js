/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: cells.js
 * @time: 2026/10/28
 */
// 结果表格里的纯函数：单元格文本、整列复制的几种格式、预览内容的类型判断。不碰 DOM，可以直接用 node 跑单测。

// 单元格里显示的文本（NULL 显示为 NULL）。
export function cellText(v) {
  if (v === null || v === undefined) return 'NULL';
  if (typeof v === 'boolean') return v ? 'true' : 'false';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

// 整列 / 整表复制用：NULL 复制成空，行与行保持对齐；值里的换行折成空格（和“复制”按钮的 TSV 一致）。
const flat = (v) => (v === null || v === undefined ? '' : cellText(v).replace(/[\r\n]+/g, ' '));

// 一列的值，每行一个。
export function columnValues(rows, k) {
  return rows.map((r) => flat(r[k])).join('\n');
}

// 列名 + 值，每行一个。
export function columnWithName(name, rows, k) {
  return [flat(name), ...rows.map((r) => flat(r[k]))].join('\n');
}

// SQL 字符串字面量：MySQL 系 / ClickHouse 的反斜杠是转义符，要双写；PostgreSQL / SQL Server / Oracle / 达梦只需要把单引号双写。
export function sqlLiteral(v, dbType) {
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  const s = cellText(v);
  const backslash = dbType !== 'pgsql' && dbType !== 'mssql' && dbType !== 'oracle' && dbType !== 'dm';
  return "'" + (backslash ? s.replace(/\\/g, '\\\\') : s).replace(/'/g, "''") + "'";
}

// 一列的值转成 IN 列表：('a','b',1)。去重（按值，数字 1 和字符串 '1' 算两个），跳过 NULL。
// 返回 { text, count, skipped }：count 是写进列表的个数，skipped 是 NULL 的个数。
export function inList(rows, k, dbType) {
  const seen = new Set();
  const out = [];
  let skipped = 0;
  for (const r of rows) {
    const v = r[k];
    if (v === null || v === undefined) { skipped++; continue; }
    const lit = sqlLiteral(v, dbType);
    if (seen.has(lit)) continue;
    seen.add(lit);
    out.push(lit);
  }
  return { text: out.length ? '(' + out.join(', ') + ')' : '', count: out.length, skipped };
}

// 预览时用哪种高亮：看起来像 SQL（以常见语句关键字或注释开头）→ 'sql'，其余 'text'。
const SQL_HEAD = /^(select|insert|update|delete|replace|with|set|create|alter|drop|truncate|explain|show|call|use|grant|revoke|begin|start|commit|rollback|merge|declare)\b/i;
export function previewKind(text) {
  const t = String(text).trimStart();
  if (t.startsWith('--') || t.startsWith('/*') || t.startsWith('#')) return /\b(select|insert|update|delete|set|create|alter)\b/i.test(t) ? 'sql' : 'text';
  return SQL_HEAD.test(t) ? 'sql' : 'text';
}

// 预览框的文本长度上限（字符）：再长的内容只预览前面这一段，复制按钮仍然复制完整内容。
export const PREVIEW_MAX = 500000;
