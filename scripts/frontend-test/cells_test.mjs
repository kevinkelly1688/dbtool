/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: cells_test.mjs
 * @time: 2026/10/28
 */
// 结果表格纯函数测试：node scripts/frontend-test/cells_test.mjs
import assert from 'node:assert/strict';
import { cellText, columnValues, columnWithName, sqlLiteral, inList, previewKind } from '../../web/static/js/query/cells.js';

assert.equal(cellText(null), 'NULL');
assert.equal(cellText(undefined), 'NULL');
assert.equal(cellText(true), 'true');
assert.equal(cellText(0), '0');
assert.equal(cellText('a\nb'), 'a\nb', '字符串原样');
assert.equal(cellText({ a: 1 }), '{"a":1}', '对象不显示 [object Object]');

const rows = [[1, 'a b'], [2, null], [3, "it's"], [4, 'x\ny'], [5, 'a b']];
assert.equal(columnValues(rows, 0), '1\n2\n3\n4\n5');
assert.equal(columnValues(rows, 1), 'a b\n\nit\'s\nx y\na b', 'NULL 为空行、值里的换行折成空格，行数与结果一致');
assert.equal(columnValues(rows, 1).split('\n').length, rows.length);
assert.equal(columnWithName('name', rows, 1).split('\n')[0], 'name');
assert.equal(columnWithName('name', rows, 1).split('\n').length, rows.length + 1);
assert.equal(columnValues([], 0), '', '没有行时为空串');

// 字符串字面量：MySQL 系 / ClickHouse 双写反斜杠，PG / SQL Server 不动
assert.equal(sqlLiteral("it's", 'mysql'), "'it''s'");
assert.equal(sqlLiteral('a\\b', 'mysql'), "'a\\\\b'");
assert.equal(sqlLiteral('a\\b', ''), "'a\\\\b'", '类型未知按 MySQL 处理');
assert.equal(sqlLiteral('a\\b', 'clickhouse'), "'a\\\\b'");
assert.equal(sqlLiteral('a\\b', 'pgsql'), "'a\\b'");
assert.equal(sqlLiteral('a\\b', 'mssql'), "'a\\b'");
assert.equal(sqlLiteral(12, 'mysql'), '12');
assert.equal(sqlLiteral(true, 'pgsql'), 'true');
assert.equal(sqlLiteral('12', 'mysql'), "'12'", '字符串形式的数字保持引号');

const il = inList(rows, 1, 'mysql');
assert.deepEqual(il, { text: "('a b', 'it''s', 'x\ny')", count: 3, skipped: 1 }, '去重（a b 出现两次）、跳过 NULL');
assert.deepEqual(inList(rows, 0, 'pgsql'), { text: '(1, 2, 3, 4, 5)', count: 5, skipped: 0 });
assert.deepEqual(inList([[null], [null]], 0, 'mysql'), { text: '', count: 0, skipped: 2 }, '全是 NULL');
assert.deepEqual(inList([[1], ['1']], 0, 'mysql'), { text: "(1, '1')", count: 2, skipped: 0 }, '数字 1 与字符串 \'1\' 是两个值');

// 预览的高亮类型
assert.equal(previewKind('SELECT 1'), 'sql');
assert.equal(previewKind('  \n select * from t'), 'sql');
assert.equal(previewKind('SET block_encryption_mode = \'aes-256-cbc\';\nSELECT 1'), 'sql');
assert.equal(previewKind('-- 注释\nselect 1'), 'sql');
assert.equal(previewKind('/* c */ update t set a=1'), 'sql');
assert.equal(previewKind('-- 只有注释，没有语句'), 'text');
assert.equal(previewKind('hello world'), 'text');
assert.equal(previewKind('{"a": 1}'), 'text');
assert.equal(previewKind('115'), 'text');
assert.equal(previewKind('selected rows'), 'text', '关键字后必须是词边界');
console.log('cells: ok');
