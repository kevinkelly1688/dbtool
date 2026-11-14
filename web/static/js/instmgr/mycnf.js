/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: mycnf.js
 * @time: 2026/10/28
 */
// 改参数成功后给出可粘贴到 my.cnf 的 [mysqld] 片段（纯函数，无 DOM 依赖，node 可直接测试）。
// 在线 SET GLOBAL 只对运行中的进程生效，重启后会丢；要永久生效需手工写进配置文件。

const BACKSLASH = String.fromCharCode(92);
const QUOTE = String.fromCharCode(34);

// 选项文件里值含空白、# 、; 、引号或反斜杠时需要加引号。
export function cnfValue(v) {
  const s = String(v ?? '');
  if (s === '') return QUOTE + QUOTE;
  if (/^[A-Za-z0-9_.:+\-/]+$/.test(s)) return s;
  const esc = s.split(BACKSLASH).join(BACKSLASH + BACKSLASH).split(QUOTE).join(BACKSLASH + QUOTE);
  return QUOTE + esc + QUOTE;
}

// 参数名只允许字母、数字、下划线、连字符；其它字符返回 ''（不生成片段，避免把异常内容写进配置）。
export function myCnfSnippet(name, value) {
  const n = String(name ?? '').trim();
  if (!/^[A-Za-z][A-Za-z0-9_-]*$/.test(n)) return '';
  return '[mysqld]\n' + n + ' = ' + cnfValue(value);
}
