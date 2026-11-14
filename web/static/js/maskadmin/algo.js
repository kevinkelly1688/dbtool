/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: algo.js
 * @time: 2026/10/28
 */
// 脱敏算法的页面副本：用于“试算”，语义与服务端 internal/masking 一致（共用测试向量 internal/masking/testdata/vectors.json）。
// 正则用浏览器的 RegExp，服务端用 Go RE2，少数高级写法（回溯引用、前后断言）在这里能跑、服务端会拒绝，保存时以服务端校验为准。

// 文案翻译：默认原样输出（本文件会被 node 测试直接 import，不依赖浏览器里的 i18n.js）；页面入口用 setTranslator(t) 接上真正的翻译函数。
let t = (key, params) => (params ? key.replace(/\{(\w+)\}/g, (m, k) => (k in params ? String(params[k]) : m)) : key);
export function setTranslator(fn) { t = fn; }

export const MASK = '***';

const runes = (s) => Array.from(String(s));

// 保留前 n 位和后 m 位（按字符计），中间固定 ***；值太短时少保留，保证至少隐藏 1 个字符，先保前缀、再缩后缀。
export function keepEnds(v, n, m) {
  const a = runes(v);
  const l = a.length;
  if (!l) return String(v);
  const p = Math.max(0, Math.min(n, l - 1));
  const s = Math.max(0, Math.min(m, l - 1 - p));
  return a.slice(0, p).join('') + MASK + (s ? a.slice(l - s).join('') : '');
}

// 三段式：按长度三等分（余数依次给中段、后段），隐藏 hide 指定的一段（1 前 2 中 3 后）。
export function threeSegment(v, hide) {
  const a = runes(v);
  const n = a.length;
  const avg = Math.floor(n / 3);
  const rem = n % 3;
  const lens = [avg, avg + (rem > 0 ? 1 : 0), avg + (rem > 1 ? 1 : 0)];
  let pos = 0;
  let out = '';
  for (let g = 1; g <= 3; g++) {
    const seg = a.slice(pos, pos + lens[g - 1]);
    pos += lens[g - 1];
    out += g === hide ? '*'.repeat(seg.length) : seg.join('');
  }
  return out;
}

// 正则：命中时把“隐藏分组”换成等长的 *，其余分组原样；输出只含各分组（与服务端一致）；不匹配原样返回。
function regexMask(v, regex, hide) {
  let m;
  try { m = new RegExp(regex, 'i').exec(String(v)); } catch (_) { return String(v); }
  if (!m) return String(v);
  let last = 0;
  for (let g = 1; g < m.length; g++) if (m[g] !== undefined) last = g;
  let out = '';
  for (let g = 1; g <= last; g++) out += g === hide ? '*'.repeat(runes(m[g] || '').length) : (m[g] || '');
  return out;
}

// rule: {kind, prefix, suffix, hide, regex}。空值原样返回。
export function maskValue(v, rule) {
  if (v === '' || v === null || v === undefined) return v;
  switch (rule.kind) {
    case 'keep': return keepEnds(v, rule.prefix | 0, rule.suffix | 0);
    case 'three': return threeSegment(v, rule.hide | 0);
    case 'whole': return MASK;
    case 'regex': return regexMask(v, rule.regex || '', rule.hide | 0);
    default: return String(v);
  }
}

export function ruleText(r) {
  switch (r.kind) {
    case 'keep': return t('保留前 {prefix} 位 + *** + 后 {suffix} 位', { prefix: r.prefix | 0, suffix: r.suffix | 0 });
    case 'three': return [t('三段式：隐藏前段'), t('三段式：隐藏中段'), t('三段式：隐藏后段')][Math.min(Math.max(r.hide | 0, 1), 3) - 1];
    case 'whole': return t('整体显示 ***');
    case 'regex': return t('正则：隐藏第 {hide} 组', { hide: r.hide | 0 });
    default: return t('旧规则');
  }
}

export function sameRule(a, b) {
  if (!a || !b || a.kind !== b.kind) return false;
  switch (a.kind) {
    case 'whole': return true;
    case 'keep': return (a.prefix | 0) === (b.prefix | 0) && (a.suffix | 0) === (b.suffix | 0);
    case 'three': return (a.hide | 0) === (b.hide | 0);
    case 'regex': return a.regex === b.regex && (a.hide | 0) === (b.hide | 0);
    default: return false;
  }
}

// 页面里的轻量校验（服务端还会再校验一遍）；返回错误文字，合法返回空串。
export function ruleProblem(r) {
  switch (r.kind) {
    case 'keep':
      if (!(r.prefix >= 0 && r.prefix <= 64 && r.suffix >= 0 && r.suffix <= 64)) return t('保留位数需在 0～64 之间');
      return '';
    case 'three':
      return r.hide >= 1 && r.hide <= 3 ? '' : t('三段式要隐藏的段需是 1、2 或 3');
    case 'whole':
      return '';
    case 'regex': {
      if (!r.regex || r.regex.length > 255) return t('请填写正则（不超过 255 个字符）');
      let n;
      try { n = new RegExp(r.regex + '|').exec('').length - 1; } catch (e) { return t('正则不合法：{msg}', { msg: e.message }); }
      return r.hide >= 1 && r.hide <= n ? '' : t('要隐藏的分组需在 1～{max} 之间（正则里共有 {n} 个分组）', { max: Math.max(n, 1), n });
    }
    default:
      return t('请选择脱敏方式');
  }
}
