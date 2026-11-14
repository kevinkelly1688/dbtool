/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: util.js
 * @time: 2026/10/28
 */
// 分区维护页面共用的小工具（配置列表页与全局维护日志页都用）。
// 所有来自服务端 / 数据库的字符串一律用 textContent 写入，不拼 innerHTML。
import { t, tp, isZh } from '../i18n.js';

// 句中的冒号 / 分隔符：中文用全角，其它语言用半角。
export const colon = isZh ? '：' : ': ';
export const listSep = isZh ? '、' : ', ';
export const clauseSep = isZh ? '；' : '; ';

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

export function setMsg(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

// 日志来源：本系统手动执行（manual）、本系统定时维护（auto）、外部程序（external，日志里没有 source 字段）。
export const SOURCE_LABEL = { manual: t('手工'), auto: t('定时'), external: t('外部程序') };
// 日志状态：外部程序的日志没有执行状态，显示“未记录”而不是推定成功。
export const LOG_STATUS = {
  success: [t('成功'), 'passed'],
  partial: [t('部分失败'), 'waiting'],
  failed: [t('失败'), 'rejected'],
  unknown: [t('未记录'), 'aborted'],
};

export function sourceTag(source) {
  return el('span', 'st pm-src-' + (SOURCE_LABEL[source] ? source : 'external'), SOURCE_LABEL[source] || source || t('外部程序'));
}

export function logStatusTag(status) {
  const [text, cls] = LOG_STATUS[status] || LOG_STATUS.unknown;
  return el('span', 'st ' + cls, text);
}

// 一条日志里的全部语句与结果（失败原因红字）。
export function logItemsCell(l) {
  const box = el('div', 'pm-log-sql');
  for (const it of l.items || []) {
    const line = el('div', '', (it.sql || '').replaceAll('`', ''));
    if (it.status && it.status !== 'success' && it.status !== 'ok') line.append(el('span', 'pm-log-err', `  [${it.status}${it.error ? colon + tp(it.error) : ''}]`));
    else if (!it.sql && it.error) line.append(el('span', 'pm-log-err', tp(it.error)));
    box.append(line);
  }
  if (l.raw) box.append(el('div', '', l.raw));
  return box;
}

// 给一行的各单元格标上列名（窄屏时表格改为卡片式，用 CSS 的 attr(data-label) 显示）。
export function labelCells(tr, labels) {
  [...tr.children].forEach((c, i) => { if (labels[i]) c.dataset.label = labels[i]; });
}

// 系统记录的类型（kind）：定时维护轮次汇总 / 推送测试。
export const KIND_LABEL = { auto_round: t('定时轮次'), webhook_test: t('推送测试') };
const ROUND_RESULT = { executed: [t('已执行'), 'passed'], failed: [t('失败'), 'rejected'], skipped: [t('跳过'), 'aborted'], nothing: [t('无需维护'), 'aborted'] };

// 定时维护一轮的汇总（LogView.round）：计数 + 每个配置的结果与原因。
export function roundCell(round) {
  const box = el('div', 'pm-round');
  const sum = [t('检查 {n}', { n: round.checked }), t('已执行 {n}', { n: round.executed }), t('无需维护 {n}', { n: round.nothing }), t('跳过 {n}', { n: round.skipped }), t('失败 {n}', { n: round.failed })].join(' / ');
  box.append(el('div', 'pm-round-sum', sum));
  const meta = [];
  if (round.guard_minutes) meta.push(t('近期日志保护窗口 {n} 分钟', { n: round.guard_minutes }));
  meta.push(round.auto_drop ? t('允许自动清理') : t('只续建、不自动删除'));
  if (round.notify_on) meta.push(t('推送时机 {when}', { when: round.notify_on }));
  box.append(el('div', 'muted small', meta.join(clauseSep)));
  const items = round.items || [];
  if (items.length) {
    const ul = el('ul', 'pm-round-items');
    for (const it of items) {
      const [text, cls] = ROUND_RESULT[it.result] || [it.result, 'aborted'];
      const li = el('li');
      li.append(el('span', 'st ' + cls, text), el('span', 'mono small', ` #${it.cnf_id} ${it.table || ''} `), el('span', 'small', tp(it.reason || '')));
      ul.append(li);
    }
    box.append(ul);
  }
  if (round.items_cut) box.append(el('div', 'muted small', t('另有 {n} 项未列出', { n: round.items_cut })));
  return box;
}
