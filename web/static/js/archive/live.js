/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: live.js
 * @time: 2026/10/28
 */
// 运行中的实时面板：进度条、速度、预计剩余、限流状态；full 模式多一组 Threads_running / 复制延迟仪表。
// 只含进度 / 仪表，不含按钮，所以每个轮询周期整块重绘不会“点不中”。
import { t, tp } from '../i18n.js';
import { el, add, nf, fmtDur } from './util.js';
import { tag, notice } from './ui.js';
import { PROGRESS_STATE } from './consts.js';

function gauge(label, value, threshold, unit, text) {
  const g = el('div', 'ar-gauge');
  g.append(el('span', '', label));
  if (text) {
    g.append(el('span', 'muted', text));
    return g;
  }
  const bar = el('div', 'ar-bar');
  const b = el('b', value > threshold ? 'bad' : value > threshold * 0.75 ? 'warn' : '');
  b.style.width = Math.min(100, (value / (threshold * 1.5 || 1)) * 100) + '%';
  bar.append(b, el('i'));
  g.append(bar, el('b', '', t('{value}{unit} / 阈值 {threshold}{unit}', { value, unit, threshold })));
  return g;
}

function stateOf(p) {
  if (p.state === 'waiting_load' || (p.throttled && p.state !== 'paused')) {
    return [t('等待负载') + (p.throttle_waited_seconds ? ` ${p.throttle_waited_seconds}s` : ''), 'bad'];
  }
  return PROGRESS_STATE[p.state] || [p.state, ''];
}

// 构造实时面板节点；p 为 /progress 里的一项。
export function liveNode(p, full) {
  const box = el('div', 'ar-live');
  // 当前休眠 / 是否检测复制延迟：接口里没有这两个字段时按设定值 / 阈值是否启用回退
  const sleep = p.sleep_ms !== undefined ? p.sleep_ms : p.set_sleep_ms;
  const lagKnown = p.lag_checked !== undefined ? p.lag_checked : p.max_lag_seconds > 0;
  const known = p.total_est > 0;
  const pct = known ? Math.min(100, Number(p.percent) || 0) : 0;
  const bar = el('div', 'ar-prog' + (known ? '' : ' busy'));
  const fill = el('b');
  if (known) fill.style.width = pct.toFixed(1) + '%';
  bar.append(fill);
  box.append(bar);

  const [stText, stCls] = stateOf(p);
  const line = el('div', 'small');
  add(line, tag(stText, stCls), ' ');
  if (known) line.append(t('{pct}% · {done} / {total} 行 · ', { pct: pct.toFixed(1), done: nf(p.scanned), total: nf(p.total_est) }));
  else line.append(t('已处理 {n} 行 · ', { n: nf(p.scanned) }));
  line.append(t('{rate} 行/s · 预计剩余 {eta}', { rate: nf(Math.round(p.rows_per_sec || 0)), eta: p.eta_seconds > 0 ? fmtDur(p.eta_seconds * 1000) : '—' }));
  box.append(line);

  if (!full) {
    box.append(el('div', 'small muted', t('批 {batch} · 休眠 {sleep} ms · 限流 {n} 次', { batch: nf(p.batch_size), sleep, n: p.throttle_count || 0 })));
    return box;
  }

  const gauges = el('div', 'ar-gauges');
  gauges.append(gauge('Threads_running', p.threads, p.max_threads, ''));
  if (lagKnown) gauges.append(gauge(t('复制延迟'), p.lag_seconds, p.max_lag_seconds, 's'));
  else gauges.append(gauge(t('复制延迟'), 0, 0, 's', t('未启用检查')));
  box.append(gauges);
  box.append(el('div', 'small muted',
    t('当前批大小 {batch}（设定 {setBatch}）· 批间休眠 {sleep} ms（设定 {setSleep}）· 已用时 {elapsed} · 本次限流 {n} 次 · 峰值 Threads_running {peak}', {
      batch: nf(p.batch_size), setBatch: nf(p.set_batch_size), sleep, setSleep: p.set_sleep_ms, elapsed: fmtDur(p.elapsed_ms), n: p.throttle_count || 0, peak: p.peak_threads || 0,
    })));
  if (p.throttled) {
    const waited = p.throttle_waited_seconds || 0;
    const msg = p.throttle_reason
      ? t('负载超限，已暂停等待：{reason}，已等 {waited}s（最长 {max}s）。', { reason: tp(p.throttle_reason), waited, max: p.max_wait_seconds })
      : t('负载超限，已暂停等待，已等 {waited}s（最长 {max}s）。', { waited, max: p.max_wait_seconds });
    box.append(notice(msg, 'bad', '⏸'));
  }
  return box;
}
