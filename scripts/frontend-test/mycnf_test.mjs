/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: mycnf_test.mjs
 * @time: 2026/10/28
 */
// my.cnf 片段生成函数的断言测试：node scripts/frontend-test/mycnf_test.mjs
import assert from 'node:assert/strict';
const B = String.fromCharCode(92), Q = String.fromCharCode(34);
import { cnfValue, myCnfSnippet } from '../../web/static/js/instmgr/mycnf.js';

assert.equal(myCnfSnippet('max_connections', 200), '[mysqld]\nmax_connections = 200');
assert.equal(myCnfSnippet('slow_query_log', 'ON'), '[mysqld]\nslow_query_log = ON');
assert.equal(myCnfSnippet('innodb_buffer_pool_size', '8G'), '[mysqld]\ninnodb_buffer_pool_size = 8G');
assert.equal(myCnfSnippet('sql_mode', 'STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION'), '[mysqld]\nsql_mode = "STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION"');
assert.equal(cnfValue('a b'), '"a b"');
assert.equal(cnfValue('a#b'), '"a#b"');
assert.equal(cnfValue('say ' + Q + 'hi' + Q), Q + 'say ' + B + Q + 'hi' + B + Q + Q);
assert.equal(cnfValue('C:' + B + 'data'), Q + 'C:' + B + B + 'data' + Q);
assert.equal(cnfValue(''), '""');
assert.equal(cnfValue(null), '""');
assert.equal(cnfValue('/var/lib/mysql'), '/var/lib/mysql');
// 参数名异常（含换行、等号、空格）不生成片段，避免把注入内容写进配置
assert.equal(myCnfSnippet('a\nb', 1), '');
assert.equal(myCnfSnippet('a=b', 1), '');
assert.equal(myCnfSnippet('', 1), '');
assert.equal(myCnfSnippet('1abc', 1), '');
// 值里的换行被引号包住，不会拆成新的一行配置
assert.equal(myCnfSnippet('init_connect', 'a\nb').split('\n').length, 3);
console.log('mycnf ok');
