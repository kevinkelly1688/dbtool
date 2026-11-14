/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: slowlog_hints_test.mjs
 * @time: 2026/10/28
 */
// 慢查日志页“结果为空”提示的纯函数测试：node scripts/frontend-test/slowlog_hints_test.mjs
import assert from 'node:assert/strict';
import { emptyHint } from '../../web/static/js/slowlog/hints.js';

const base = '该时间范围内没有慢日志统计';
// 平台采集 + 实例没开采集开关：直接点名原因与去处
let h = emptyHint('stat', { source: 'collected' }, { id: 1, collect_enabled: false });
assert.ok(h.startsWith(base), h);
assert.ok(h.includes('没有开启“慢日志采集”') && h.includes('实例管理'), h);
// 平台采集 + 开关已开：给出采集器状态入口
h = emptyHint('stat', { source: 'collected' }, { id: 1, collect_enabled: true });
assert.ok(h.includes('请确认实例已开启“慢日志采集”') && h.includes('采集器状态'), h);
assert.ok(!h.includes('这个实例没有开启'), h);
// 汇总模式
h = emptyHint('stat', { source: 'all' }, null);
assert.ok(h.includes('至少要有一个开启了“慢日志采集”'), h);
// pt-query-digest / 阿里云：只给原提示
assert.equal(emptyHint('stat', { source: 'digest' }, { collect_enabled: false }), base);
assert.equal(emptyHint('stat', { source: 'aliyun' }, null), base);
// 明细
assert.ok(emptyHint('det', { source: 'collected' }, { collect_enabled: false }).startsWith('没有符合条件的慢日志明细。'));
assert.equal(emptyHint('det', { source: 'digest' }, null), '没有符合条件的慢日志明细');
// 没有 page（防御）
assert.equal(emptyHint('stat', null, null), base);
console.log('slowlog hints: ok');
