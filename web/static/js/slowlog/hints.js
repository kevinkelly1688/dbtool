/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: hints.js
 * @time: 2026/10/28
 */
// 慢查日志页：结果为空时，按数据源和实例的采集开关给出最可能的原因（纯函数，可直接用 node 单测）。
// kind: 'stat'（统计）/ 'det'（明细）；page.source: digest / collected / aliyun / all（汇总）；
// inst: 当前选中的具体实例（汇总模式或没选为 null），inst.collect_enabled 来自实例下拉接口。
// t 是翻译函数（页面传入 i18n.js 的 t）；这里保持纯函数、不导入 i18n.js，node 单测里默认原样输出中文。
const plain = (key, params) => (params ? key.replace(/\{(\w+)\}/g, (m, k) => (k in params ? String(params[k]) : m)) : key);

export function emptyHint(kind, page, inst, t = plain) {
  const what = kind === 'det' ? t('没有符合条件的慢日志明细') : t('该时间范围内没有慢日志统计');
  const src = page && page.source;
  if (src !== 'collected' && src !== 'all') return what; // pt-query-digest / 阿里云 RDS：数据由外部写入，这里说不出更多原因
  if (inst && inst.collect_enabled === false) {
    return t('{what}。这个实例没有开启“慢日志采集”，请在「实例管理」里打开。', { what });
  }
  const scope = src === 'all' ? t('汇总的实例里至少要有一个开启了“慢日志采集”') : t('请确认实例已开启“慢日志采集”');
  return t('{what}。{scope}，采集结果见「慢日志检索」页底部的“采集器状态”。', { what, scope });
}
