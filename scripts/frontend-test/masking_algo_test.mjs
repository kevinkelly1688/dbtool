/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: masking_algo_test.mjs
 * @time: 2026/10/28
 */
// 脱敏算法页面副本（web/static/js/maskadmin/algo.js）与服务端共用同一份测试向量：
//   node scripts/frontend-test/masking_algo_test.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import assert from 'node:assert/strict';
import { maskValue, ruleProblem, ruleText, sameRule } from '../../web/static/js/maskadmin/algo.js';

const here = dirname(fileURLToPath(import.meta.url));
const vectors = JSON.parse(readFileSync(join(here, '../../internal/masking/testdata/vectors.json'), 'utf8'));
let n = 0;
for (const v of vectors) {
  const rule = { kind: v.rule.kind, prefix: v.rule.prefix || 0, suffix: v.rule.suffix || 0, hide: v.rule.hide || 0, regex: v.rule.regex || '' };
  assert.equal(ruleProblem(rule), '', `${v.name}: 规则应合法`);
  assert.equal(maskValue(v.in, rule), v.out, v.name);
  n++;
}

// 空值原样；null 原样
assert.equal(maskValue('', { kind: 'whole' }), '');
assert.equal(maskValue(null, { kind: 'keep', prefix: 1, suffix: 1 }), null);

// 校验
assert.notEqual(ruleProblem({ kind: 'keep', prefix: -1, suffix: 0 }), '');
assert.notEqual(ruleProblem({ kind: 'keep', prefix: 0, suffix: 65 }), '');
assert.notEqual(ruleProblem({ kind: 'three', hide: 4 }), '');
assert.notEqual(ruleProblem({ kind: 'regex', regex: '(', hide: 1 }), '');
assert.notEqual(ruleProblem({ kind: 'regex', regex: '^(a)$', hide: 2 }), '');
assert.notEqual(ruleProblem({ kind: 'regex', regex: '', hide: 1 }), '');
assert.notEqual(ruleProblem({ kind: 'x' }), '');

// 文字与相等
assert.equal(ruleText({ kind: 'keep', prefix: 3, suffix: 4 }), '保留前 3 位 + *** + 后 4 位');
assert.ok(sameRule({ kind: 'keep', prefix: 3, suffix: 4 }, { kind: 'keep', prefix: 3, suffix: 4 }));
assert.ok(!sameRule({ kind: 'keep', prefix: 3, suffix: 4 }, { kind: 'keep', prefix: 3, suffix: 3 }));
assert.ok(!sameRule({ kind: 'whole' }, { kind: 'three', hide: 2 }));

console.log(`masking algo: ok（${n} 条共用向量 + 校验）`);
