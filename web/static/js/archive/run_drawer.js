/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: run_drawer.js
 * @time: 2026/10/28
 */
// 运行详情抽屉：结果与统计、吞吐曲线、限流事件、校验结果、备份位置（库表 / 文件清单）、日志；库表备份可恢复数据。
import { applyButtons } from '../btnperm.js';
import { t, tp, isZh } from '../i18n.js';
import { A } from './api.js';
import { el, add, nf, fmtBytes, fmtDur, fmtTime, link, go, pbutton, notifyChanged, withNodes } from './util.js';
import { openDrawer, setDrawerBody, closeDrawer, notice, kvList, h4, code, bold, tag, flash, small } from './ui.js';
import { resultTag, TRIGGER, THROTTLE_KIND } from './consts.js';
import { throughputChart } from './chart.js';
import { liveNode } from './live.js';
import { acquire, subscribe, byRun } from './poller.js';
import { restoreRun } from './actions.js';

let cur = null;

function table(headers, rows) {
  const tbl = el('table', 'list');
  const th = el('thead');
  const hr = el('tr');
  headers.forEach((x) => hr.append(el('th', '', x)));
  th.append(hr);
  const tb = el('tbody');
  for (const r of rows) {
    const tr = el('tr');
    for (const c of r) {
      const td = el('td');
      add(td, c);
      tr.append(td);
    }
    tb.append(tr);
  }
  tbl.append(th, tb);
  return tbl;
}

function statNode(r) {
  const noLoad = r.trigger === 'dryrun' || r.trigger === 'restore';
  return kvList([
    [t('结果'), [resultTag(r.result), r.result === 'running' ? el('span', 'small muted', ' ' + t('进行中')) : '']],
    [t('扫描 / 备份 / 删除'), t('{scanned} / {backed} / {deleted} 行', { scanned: nf(r.scanned), backed: nf(r.backed), deleted: nf(r.deleted) })],
    [t('批数 / 耗时'), t('{n} 批 · {dur}', { n: nf(r.batches), dur: r.result === 'running' ? t('进行中') : fmtDur(r.duration_ms) })],
    [t('峰值 Threads_running'), noLoad ? '—' : String(r.peak_threads)],
    [t('峰值复制延迟'), noLoad ? '—' : (r.peak_lag_seconds || 0) + ' s'],
    [t('被限流次数'), noLoad ? '—' : String(r.throttle_count)],
    [t('处理的主键范围'), r.max_pk && r.max_pk.length ? `${(r.pk_cols || []).join(',')} ∈ [${(r.min_pk || []).join(',')}, ${(r.max_pk || []).join(',')}]` : '—'],
  ], true);
}

function backupsNode(r, d) {
  const box = el('div');
  const list = d.backups || [];
  if (!list.length) {
    box.append(small(r.trigger === 'restore' ? t('恢复运行本身不产生备份。') : t('本次运行没有产生备份数据。')));
    return box;
  }
  for (const b of list) {
    const card = el('div', 'card');
    card.style.cssText = 'margin-bottom:8px;padding:10px 12px';
    if (b.kind === 'table') {
      const row = el('div', 'toolbar');
      row.style.marginBottom = '0';
      add(row, tag(t('库表备份'), 'info'), code(b.location), el('span', 'small muted', t('{inst} · {rows} 行', { inst: b.instance_name, rows: nf(b.rows) })), el('span', 'spacer'));
      if (r.can_restore) row.append(pbutton('archive.restore', t('恢复数据'), 'danger', () => restoreRun(r, (res) => { notifyChanged(); go('#run/' + res.run_id); })));
      card.append(row);
      if (r.restored_by && r.restored_by.length) {
        const rs = el('div', 'small muted', t('已恢复过：'));
        r.restored_by.forEach((id, i) => { if (i) rs.append(isZh ? '、' : ', '); rs.append(link('#' + id, () => go('#run/' + id))); });
        card.append(rs);
      }
    } else {
      const row = el('div', 'toolbar');
      row.style.marginBottom = '4px';
      add(row, tag(t('文件备份'), 'brand'), el('span', '', b.store_name), code(b.path));
      card.append(row);
      const info = el('div', 'small muted');
      info.style.margin = '4px 0';
      add(info, t('{rows} 行 · {files} 个文件 · {bytes} · {enc} · 清单 sha256 ', {
        rows: nf(b.rows), files: (b.files || []).length, bytes: fmtBytes(b.bytes), enc: b.encrypted ? t('加密 {alg}', { alg: b.enc || 'AES-256-GCM' }) : t('未加密'),
      }), code((b.manifest_sha256 || '').slice(0, 16) + (b.manifest_sha256 ? '…' : '—')), ' ',
      b.manifest_ok ? tag(t('回读校验通过'), 'ok') : tag(t('未生成清单'), 'bad'));
      card.append(info);
      if ((b.files || []).length) {
        card.append(table([t('文件'), t('行数'), t('大小'), 'sha256'], b.files.map((f) => [code(f.key), nf(f.rows), fmtBytes(f.size), el('code', '', (f.sha256 || '').slice(0, 16) + (f.sha256 ? '…' : '')) ])));
      }
      if (d.manifest || b.manifest_ok) {
        const a = el('a', 'btn', t('下载清单'));
        a.href = A.manifestUrl(r.id);
        a.setAttribute('download', `manifest-${r.id}.json`);
        const bar = el('div', 'toolbar');
        bar.style.marginTop = '6px';
        bar.append(a);
        card.append(bar);
      }
    }
    box.append(card);
  }
  return box;
}

function render(r) {
  const box = el('div');
  const head = el('div', 'toolbar');
  add(head, resultTag(r.result), tag(TRIGGER[r.trigger] || r.trigger), el('span', 'small muted'));
  const info = head.lastChild;
  if (r.task_id) info.append(link(r.task_name, () => go('#task/' + r.task_id)));
  else info.append(r.task_name || t('（未提交的草稿）'));
  info.append(` · `);
  info.append(code(r.table));
  info.append(` · ${r.instance_name}`);
  box.append(head);

  if (r.err) box.append(notice([bold(t('错误：')), tp(r.err)], 'bad', '✕'));
  if (r.note) box.append(notice(tp(r.note), 'info'));
  if (r.auto_pause) box.append(notice(t('该运行触发了自动暂停，任务已置为“已暂停”。'), 'warn', '⏸'));
  if (r.restore_of) {
    box.append(notice(withNodes(t('这是对 {run} 的数据恢复，{skip}。'), {
      run: link(t('运行 #{id}', { id: r.restore_of }), () => go('#run/' + r.restore_of)),
      skip: bold(t('只补缺、不覆盖')),
    }), 'info', '↺'));
  }
  if (r.result === 'running' || r.result === 'paused') {
    const live = el('div');
    live.append(small(t('读取进度…')));
    cur.liveBox = live;
    box.append(live);
  } else if (cur) cur.liveBox = null;

  box.append(kvList([
    [t('操作人 / 触发'), `${r.by} · ${TRIGGER[r.trigger] || r.trigger}`],
    [t('开始'), fmtTime(r.started_at)],
    [t('结束'), r.result === 'running' || r.result === 'paused' ? t('进行中') : fmtTime(r.finished_at)],
  ], true));
  const stat = el('div');
  stat.style.marginTop = '8px';
  stat.append(statNode(r));
  box.append(stat);
  return box;
}

function renderDetail(r, d) {
  const box = render(r);
  if (r.trigger === 'schedule' || r.trigger === 'manual') {
    box.append(h4(t('批次吞吐曲线')));
    box.append(throughputChart(d.series));
    box.append(h4(t('限流事件')));
    const ts = d.throttles || [];
    if (ts.length) {
      box.append(table([t('时间'), t('触发项'), t('读数 / 阈值'), t('等待'), t('结果')], ts.map((e) => [
        el('span', 'ar-nowrap', fmtTime(e.at).slice(5)),
        THROTTLE_KIND[e.kind] || e.kind,
        `${e.value} / ${e.threshold}`,
        (e.waited_seconds || 0) + ' s',
        e.end === 'abort' ? tag(t('超过最长等待，中断'), 'warn') : e.end === 'wait' ? tag(t('等待中'), 'bad') : [tag(t('已恢复'), 'ok'), e.batch_to ? el('span', 'small muted', ' ' + t('批量降到 {n}', { n: e.batch_to })) : ''],
      ])));
    } else box.append(small(t('本次运行没有被限流。')));
    box.append(h4(t('校验结果')));
    const v = d.verify || [];
    if (v.length) {
      const list = el('div', 'ar-checks');
      for (const x of v) {
        const row = el('div', 'ar-chk ' + (x.ok ? 'ok' : 'error'));
        row.append(tag(x.ok ? t('通过|check') : t('失败'), x.ok ? 'ok' : 'bad'), el('b', '', tp(x.title) || x.key), el('span', '', tp(x.detail) || ''));
        list.append(row);
      }
      box.append(list);
    } else box.append(small(t('没有校验记录。')));
    box.append(h4(t('备份位置')));
    box.append(backupsNode(r, d));
  }
  box.append(h4(t('日志')));
  const pre = el('pre', 'ar-log', (d.log || []).join('\n') || t('（无）'));
  box.append(pre);
  return box;
}

async function reload() {
  if (!cur) return;
  const id = cur.id;
  try {
    const d = await A.run(id);
    if (!cur || cur.id !== id) return;
    const wasRunning = cur.running;
    cur.running = d.result === 'running' || d.result === 'paused';
    setDrawerBody(renderDetail(d, d), t('运行 #{id} · {name}', { id: d.id, name: d.task_name || t('试运行') }));
    applyButtons(document.getElementById('ar-drawer'));
    if (cur.running && !cur.release) cur.release = acquire();
    if (!cur.running && cur.release) {
      cur.release();
      cur.release = null;
    }
    if (cur.liveBox && cur.latest) paintLive(cur.latest);
    if (wasRunning && !cur.running) notifyChanged();
  } catch (e) {
    if (e.status === 404) {
      closeDrawer();
      flash(t('运行记录不存在或不在你的可见范围'), 'warn');
    } else flash(e.message, 'err');
  }
}

function paintLive(list) {
  if (!cur || !cur.liveBox) return;
  const p = byRun(list, cur.id);
  if (p) cur.liveBox.replaceChildren(liveNode(p, true));
}

export async function openRunDrawer(id, onClose) {
  cleanup();
  cur = { id, release: null, unsub: null, liveBox: null, latest: [], running: false, timer: 0 };
  openDrawer(t('运行 #{id}', { id }), el('div', 'muted', t('加载中…')), () => { cleanup(); if (onClose) onClose(); });
  cur.unsub = subscribe((list) => {
    if (!cur) return;
    cur.latest = list;
    paintLive(list);
  });
  // 运行中时每 2 秒刷新一次详情（曲线 / 统计 / 日志）；与进度轮询共用节拍，但详情是独立请求
  cur.timer = setInterval(() => { if (cur && cur.running && !document.hidden) reload(); }, 2000);
  await reload();
}

function cleanup() {
  if (!cur) return;
  clearInterval(cur.timer);
  if (cur.release) cur.release();
  if (cur.unsub) cur.unsub();
  cur = null;
}

