/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: fuzzy_test.mjs
 * @time: 2026/10/28
 */
// 实例下拉模糊匹配的纯函数测试：node scripts/frontend-test/fuzzy_test.mjs
import assert from 'node:assert/strict';
import { fuzzyFilter } from '../../web/static/js/fuzzy.js';

const names = ['(生产) 商城Saas [PolarDB]-basic', '(生产) 商城Saas [PolarDB]-tenant(华东1)', '(生产) 商城Saas [ADB]-tenant', '(生产) 商城Saas [ADB]-shopsaas', 'prod-mysql-01', 'Test-MySQL-02'];
const f = (q) => fuzzyFilter(names, q).map((i) => names[i]);

assert.equal(fuzzyFilter(names, '').length, names.length, '空查询返回全部');
assert.equal(fuzzyFilter(names, '   ').length, names.length, '全空白等同空查询');
assert.deepEqual(f('adb'), [names[2], names[3]], '不区分大小写的子串');
assert.deepEqual(f('POLARDB tenant'), [names[1]], '多个词都要命中，顺序无关');
assert.deepEqual(f('tenant polardb'), [names[1]], '词序无关');
assert.deepEqual(f('华东'), [names[1]], '中文子串');
assert.deepEqual(f('mysql 02'), [names[5]], '数字与大小写');
assert.deepEqual(f('pdb'), [names[0], names[1]], '没有子串命中时按字符顺序（子序列）匹配');
assert.deepEqual(f('zzzz'), [], '没有匹配返回空');
assert.deepEqual(fuzzyFilter(names, 'prod').map((i) => i), [4], '返回的是原下标');
console.log('fuzzy: ok');
