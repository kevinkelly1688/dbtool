/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: check_view.js
 * @time: 2026/10/28
 */
// 试运行结果展示（向导第六步、任务详情“试运行”弹窗共用）与“将执行的 SQL”块。
import { t, tp } from '../i18n.js';
import { el, nf, fmtDur } from './util.js';
import { tag, notice, h4, sqlBox, small } from './ui.js';

const LV = { ok: ['ok', t('通过|check')], warn: ['warn', t('警告')], error: ['bad', t('不通过')] };

function kpi(label, value, sub) {
  const d = el('div');
  d.append(el('span', '', label), el('b', '', value), el('small', '', sub || ' '));
  return d;
}

// “将执行的 SQL”：读 / 写目标 / 删源。sql = {select, insert, delete}；mode 决定空项的说明。
export function sqlBlocks(sql, mode) {
  const box = el('div');
  sql = sql || {};
  box.append(el('div', 'small muted', t('① 读')), sqlBox(sql.select || '—'));
  if (sql.insert) {
    box.append(el('div', 'small muted', t('② 写目标')), sqlBox(sql.insert));
  } else {
    box.append(el('div', 'small muted', t('② 写目标')),
      notice(mode === 'purge' ? t('仅清理模式：不备份，直接删除。') : t('没有库表备份。'), mode === 'purge' ? 'warn' : 'info'));
  }
  if (sql.delete) {
    box.append(el('div', 'small muted', t('③ 删源')), sqlBox(sql.delete));
  } else {
    box.append(el('div', 'small muted', t('③ 删源')), notice(t('仅备份模式：不删除源表数据。'), 'info'));
  }
  return box;
}

// chk = POST /check 的 data：{report, check_token, expires_at, need_ack_warn, need_ack_big, blocked}
export function checkNode(chk, mode, opt = {}) {
  const r = chk.report;
  const box = el('div');
  const kp = el('div', 'ar-kpis');
  const pre = r.capped ? '≥ ' : '';
  kp.append(
    kpi(t('预估命中行数'), pre + nf(r.est_rows), t('占表 {pct}%（表约 {rows} 行）', { pct: Number(r.percent || 0).toFixed(2), rows: nf(r.table_rows) }) + (r.capped ? t(' · 统计有上限') : '')),
    kpi(t('预计批数'), nf(r.est_batches)),
    kpi(t('预计耗时'), r.est_seconds ? fmtDur(r.est_seconds * 1000) : '—'),
    kpi(t('预计 binlog'), r.est_binlog_mb ? nf(r.est_binlog_mb) + ' MB' : '—'),
  );
  box.append(kp);

  const items = r.items || [];
  const errs = items.filter((i) => i.level === 'error').length;
  const warns = items.filter((i) => i.level === 'warn').length;
  const oks = items.length - errs - warns;
  box.append(h4(t('检查项'), t('{ok} 通过 · {warn} 警告 · {err} 不通过', { ok: oks, warn: warns, err: errs })));
  const list = el('div', 'ar-checks');
  for (const it of items) {
    const [cls, txt] = LV[it.level] || ['', it.level];
    const row = el('div', 'ar-chk ' + it.level);
    row.append(tag(it.key === 'large_batch' && it.level !== 'ok' ? t('需确认') : txt, cls), el('b', '', tp(it.title) || it.key), el('span', '', tp(it.detail) || ''));
    list.append(row);
  }
  box.append(list);

  const cols = r.sample_cols || [];
  const sample = r.sample || [];
  box.append(h4(t('最前 {n} 行样例', { n: sample.length })));
  if (sample.length) {
    const wrap = el('div', 'ar-sample');
    const tbl = el('table');
    const head = el('tr');
    cols.forEach((c) => head.append(el('th', '', c)));
    const thead = el('thead');
    thead.append(head);
    tbl.append(thead);
    const body = el('tbody');
    for (const row of sample) {
      const tr = el('tr');
      cols.forEach((c) => {
        const v = row[c];
        tr.append(el('td', '', v === undefined || v === null ? 'NULL' : v));
      });
      body.append(tr);
    }
    tbl.append(body);
    wrap.append(tbl);
    box.append(wrap);
  } else {
    box.append(small(t('没有符合条件的数据。')));
  }

  box.append(h4(t('将执行的 SQL')));
  box.append(sqlBlocks(r.sql, mode));
  if (!opt.noValidity) {
    if (chk.expires_at) box.append(el('div', 'small muted', t('有效期至 {time}', { time: String(chk.expires_at).slice(0, 16) })));
  }
  return box;
}
