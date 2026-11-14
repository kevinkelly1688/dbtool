/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: statements_test.mjs
 * @time: 2026/10/28
 */
// 在线查询的语句定位 / 前置 SET：node scripts/frontend-test/statements_test.mjs
import assert from 'node:assert/strict';
import { statementAt, runParts, isSetStatement } from '../../web/static/js/query/statements.js';

assert.equal(isSetStatement('SET @a := 1'), true);
assert.equal(isSetStatement('  set block_encryption_mode = \'aes-256-cbc\''), true);
assert.equal(isSetStatement('-- 先设置\nSET @a := 1'), true, '开头的注释不影响判断');
assert.equal(isSetStatement('/* x */ set @a = 1'), true);
assert.equal(isSetStatement('select @a'), false);
assert.equal(isSetStatement('settings_table'), false, '只认 SET 这个词');
assert.equal(isSetStatement('SET'), true);

const script = [
  "SET block_encryption_mode = 'aes-256-cbc';",
  "SET @SiteId := 1008;",
  "SET @SrcCurrency := 'USD';  -- V1 源站点币种；勿改",
  '',
  '-- ① 加密模式校验',
  'SELECT @@session.block_encryption_mode AS m;',
  '',
  'SELECT @SiteId, @SrcCurrency;',
].join('\n');
const at = (needle) => script.indexOf(needle) + 1;
// 语句片段里可能带着它前面的注释（服务端切分时会去掉）；比较时去掉行注释
const bare = (x) => x.replace(/--[^\n]*/g, '').replace(/\s+/g, ' ').trim();

// 光标在后面的查询上：自动带上开头的 3 条 SET
let r = runParts(script, at('SELECT @SiteId'));
assert.equal(bare(r.stmt), 'SELECT @SiteId, @SrcCurrency');
assert.equal(r.prelude.split('\n;\n').length, 3);
assert.ok(r.prelude.startsWith('SET block_encryption_mode'));
assert.ok(r.prelude.includes('SET @SiteId := 1008'));

// 光标在第一条查询（前面隔着注释）上
r = runParts(script, at('SELECT @@session'));
assert.equal(bare(r.stmt), 'SELECT @@session.block_encryption_mode AS m');
assert.equal(r.prelude.split('\n;\n').length, 3);

// 光标就在某条 SET 上：运行 全部开头的 SET + 紧随其后的第一条查询
r = runParts(script, at('SET @SiteId'));
assert.equal(bare(r.stmt), 'SELECT @@session.block_encryption_mode AS m');
assert.equal(r.prelude.split('\n;\n').length, 3);

// 没有开头的 SET：与 statementAt 一致，没有 prelude
const plain = 'select 1;\nselect 2;\nset @a = 1;';
r = runParts(plain, plain.indexOf('select 2'));
assert.deepEqual(r, { prelude: '', stmt: 'select 2' });
assert.equal(statementAt(plain, plain.indexOf('select 2')), 'select 2');
// 光标在后面的 SET 上：不是开头的 SET，原样运行这一条（服务端会提示 SET 不能单独运行）
r = runParts(plain, plain.indexOf('set @a'));
assert.deepEqual(r, { prelude: '', stmt: 'set @a = 1' });

// 全是 SET：整份交给服务端
r = runParts('set @a = 1;\nset @b = 2;', 3);
assert.deepEqual(r, { prelude: '', stmt: 'set @a = 1\n;\nset @b = 2' });

// 空文本 / 只有注释
assert.deepEqual(runParts('', 0), { prelude: '', stmt: '' });
assert.deepEqual(runParts('-- nothing', 3), { prelude: '', stmt: '-- nothing' });

// 引号里的分号不切分，SET 的值可以含分号
r = runParts("set @s := 'a;b';\nselect @s;", 20);
assert.deepEqual(r, { prelude: "set @s := 'a;b'", stmt: 'select @s' });

console.log('statements_test ok');
