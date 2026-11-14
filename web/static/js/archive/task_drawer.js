/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: task_drawer.js
 * @time: 2026/10/28
 */
// 任务详情抽屉：配置、将执行的 SQL、审批进度、最近运行、变更历史；运行中实时进度（2 秒轮询）。
// 约定：任务视图的变量一律叫 task，不叫 t（免得遮住翻译函数 t()）。
import { applyButtons } from '../btnperm.js';
import { t, tp, isZh } from '../i18n.js';
import { A } from './api.js';
import { el, add, nf, link, go, notifyChanged, fmtTime, colon } from './util.js';
import { openDrawer, setDrawerBody, closeDrawer, notice, kvList, h4, timeline, empty, code, bold, flash } from './ui.js';
import { statusTag, modeTag, resultTag, backupTags, PAUSE_KIND, TRIGGER } from './consts.js';
import { taskButtons } from './actions.js';
import { sqlBlocks } from './check_view.js';
import { liveNode } from './live.js';
import { acquire, subscribe, byTask } from './poller.js';

let cur = null; // {id, task, release, unsub, liveBox}

function paramsText(p, w) {
  const lag = p.max_lag_seconds > 0 ? p.max_lag_seconds + ' s' : t('（未启用）');
  return t('批 {batch} 行 · 休眠 {sleep} ms · Threads_running ≤ {threads} · 复制延迟 ≤ {lag} · 磁盘剩余 ≥ {disk}% · 最长等待 {wait} 分钟 · 批耗时目标 {target} ms · 单次上限 {max} 行 · 连续失败 {fails} 批自动暂停', {
    batch: nf(p.batch_size), sleep: p.sleep_ms, threads: p.max_threads_running, lag, disk: p.min_disk_free_pct, wait: Math.round(p.max_wait_seconds / 60 * 10) / 10,
    target: p.target_batch_ms, max: nf(p.max_rows), fails: p.max_consecutive_failures,
  });
}

function destText(task) {
  const out = [];
  if (task.dest_table) out.push(t('库表：实例#{id} · {name}', { id: task.dest_table.instance_id, name: `${task.dest_table.db}.${task.dest_table.template}` }));
  if (task.dest_file) {
    const f = task.dest_file;
    out.push(t('文件：{format}{gzip}{enc} · 路径 {path} · 单文件 ≤ {part} MB', {
      format: f.format.toUpperCase(), gzip: f.gzip ? ' · gzip' : '', enc: f.encrypt ? t(' · AES-256-GCM 加密') : '', path: f.path_template, part: f.part_mb || 256,
    }));
  }
  return out;
}

function backupNode(task, dests) {
  const box = el('div');
  add(box, backupTags(task));
  for (const d of dests) box.append(el('div', 'small muted', d));
  if (!box.childNodes.length) box.append('—');
  return box;
}

function approvalNode(d, task) {
  if (!d) return empty(t('尚未提交审批'));
  const items = [{ cls: 'ok', h: t('{name} 提交申请', { name: d.applicant }), m: fmtTime(d.applied_at) + (d.purge_reason ? t(' · 不备份理由：{reason}', { reason: d.purge_reason }) : '') }];
  for (const s of d.steps || []) {
    const who = (s.users || []).join(isZh ? '、' : ', ');
    const lv = { n: s.level + 1, group: s.group_name };
    if (s.state === 'approved') items.push({ cls: 'ok', h: t('第 {n} 级 {group}：{by} 通过', { ...lv, by: s.by }), m: fmtTime(s.at) + (s.remark ? ' · ' + s.remark : '') });
    else if (s.state === 'rejected') items.push({ cls: 'bad', h: t('第 {n} 级 {group}：{by} 驳回', { ...lv, by: s.by }), m: fmtTime(s.at) + (s.remark ? ' · ' + s.remark : '') });
    else if (s.state === 'pending') items.push({ cls: 'cur', h: t('第 {n} 级 {group}：待审批', lv), m: who ? t('待 {who} 审批', { who }) : t('本级无其他可审批人，仅超级用户可审') });
    else items.push({ cls: 'skip', h: t('第 {n} 级 {group}', lv), m: d.reject ? t('流程已终止') : t('等待上一级通过') });
  }
  if (d.reject) items.push({ cls: 'bad', h: t('已驳回（{by}）', { by: d.reject.by }), m: d.reject.reason });
  if (task.status === 'enabled' && d.status === 'passed') items.push({ cls: 'ok', h: t('审批通过，任务已启用'), m: '' });
  return timeline(items);
}

function render(task) {
  const box = el('div');
  const meta = el('div', 'toolbar');
  add(meta, statusTag(task.status), modeTag(task.mode), el('span', 'small muted', t('{group} · 申请人 {creator} · {time}', { group: task.resource_group_name || '', creator: task.creator, time: fmtTime(task.created_at) })));
  box.append(meta);

  if (task.status === 'paused' && task.pause) {
    box.append(notice([bold(PAUSE_KIND[task.pause.kind] || t('已暂停')), task.pause.at ? t('（{time}）', { time: fmtTime(task.pause.at) }) : '', colon, tp(task.pause.reason) || '—'], 'warn', '⏸'));
  }
  if (task.status === 'rejected' && task.reject_reason) box.append(notice([bold(t('已驳回')), colon, task.reject_reason], 'bad', '✕'));
  if (task.status === 'pending' && task.approval) box.append(notice([t('审批中：'), tp(task.approval.pending_text || '')], 'warn', '⏳'));
  if (task.mode === 'purge') box.append(notice([bold(t('仅清理：不备份，删除后无法恢复。')), t('理由：'), task.purge_reason || '—'], 'warn', '!'));
  if (task.fingerprint_drift && task.status === 'enabled') box.append(notice(t('源表结构与审批时不一致，下次运行将被暂停。'), 'warn', '!'));

  if (task.running) {
    box.append(h4(t('运行中')));
    const live = el('div');
    live.append(el('div', 'small muted', t('读取进度…')));
    cur.liveBox = live;
    box.append(live);
  } else if (cur) {
    cur.liveBox = null;
  }

  const tools = el('div', 'toolbar');
  tools.style.margin = '10px 0';
  add(tools, taskButtons(task, { compact: false, full: true, done: (gone) => { if (gone === true) { closeDrawer(); notifyChanged(); } else reload(true); }, goto: go }));
  box.append(tools);

  const dests = destText(task);
  box.append(kvList([
    [t('实例 / 库表'), [task.instance_name, el('br'), code(`${task.db}.${task.table}`)]],
    [t('归档条件'), code(task.where || '')],
    [t('模式'), modeTag(task.mode)],
    [t('备份去向'), backupNode(task, dests)],
    [t('调度'), [tp(task.schedule_text) || '—', el('span', 'small muted', task.next_run_at ? '  ' + t('下次 {time}', { time: String(task.next_run_at).slice(0, 16) }) : '')]],
    [t('执行窗口'), tp(task.window_text) || '—'],
    [t('稳定性参数'), paramsText(task.params)],
    // 断点只有“仅备份”（或主键含字符串列、仍按主键排序读取的表）才有；归档并删除 / 仅清理不排序读取，靠“删掉的行不再命中”推进，没有断点
    [t('累计'), task.mode === 'copy' || (task.checkpoint || []).length
      ? t('{rows} 行 / {runs} 次运行 · 断点 {pk} = {cp}', { rows: nf(task.cum_rows), runs: task.cum_runs, pk: (task.pk_cols || []).join(',') || 'id', cp: (task.checkpoint || []).join(',') || t('（从头）') })
      : t('{rows} 行 / {runs} 次运行', { rows: nf(task.cum_rows), runs: task.cum_runs })],
  ]));

  box.append(h4(t('将执行的 SQL')));
  box.append(sqlBlocks(task.sql, task.mode));

  box.append(h4(t('审批进度')));
  box.append(approvalNode(task.approval_detail, task));

  const rh = h4(t('最近运行'));
  rh.append(link(t('查看全部'), () => go('#runs?task=' + task.id)));
  box.append(rh);
  const runs = task.recent_runs || [];
  if (runs.length) {
    const tbl = el('table', 'list');
    const hr = el('tr');
    [t('运行'), t('开始'), t('触发'), t('结果'), t('删除行数')].forEach((x) => hr.append(el('th', '', x)));
    const th = el('thead');
    th.append(hr);
    const tb = el('tbody');
    for (const r of runs) {
      const tr = el('tr');
      const a = el('td');
      a.append(link('#' + r.id, () => go('#run/' + r.id)));
      const c2 = el('td', 'ar-nowrap small', fmtTime(r.started_at, 16).slice(5));
      const c3 = el('td', '', TRIGGER[r.trigger] || r.trigger);
      const c4 = el('td');
      c4.append(resultTag(r.result));
      tr.append(a, c2, c3, c4, el('td', '', nf(r.deleted)));
      tb.append(tr);
    }
    tbl.append(th, tb);
    box.append(tbl);
  } else box.append(empty(t('还没有运行记录')));

  box.append(h4(t('变更历史')));
  const hist = task.history || [];
  box.append(hist.length ? timeline(hist.slice().reverse().map((x) => ({ cls: 'ok', h: tp(x.text), m: `${fmtTime(x.at)} · ${x.user}` }))) : empty(t('暂无')));
  return box;
}

function paintLive(list) {
  if (!cur || !cur.liveBox) return;
  const p = byTask(list, cur.id);
  if (p) {
    cur.liveBox.replaceChildren(liveNode(p, true));
  } else if (cur.task && cur.task.running && Date.now() - cur.lastReload > 4000) {
    // 进度里没有了：运行已结束，重新加载详情
    reload(false);
  }
}

async function reload(changed) {
  if (!cur) return;
  const id = cur.id;
  cur.lastReload = Date.now();
  try {
    const task = await A.task(id);
    if (!cur || cur.id !== id) return;
    cur.task = task;
    setDrawerBody(render(task), t('归档任务 · {name}', { name: task.name }));
    applyButtons(document.getElementById('ar-drawer'));
    syncPoll();
    if (cur.liveBox) paintLive(cur.latest || []);
  } catch (e) {
    if (e.status === 404) {
      closeDrawer();
      flash(t('任务不存在或已被删除'), 'warn');
    } else flash(e.message, 'err');
  }
  if (changed) notifyChanged();
}

function syncPoll() {
  if (!cur) return;
  const want = !!(cur.task && cur.task.running);
  if (want && !cur.release) cur.release = acquire();
  if (!want && cur.release) {
    cur.release();
    cur.release = null;
  }
}

export async function openTaskDrawer(id, onClose) {
  cleanup();
  cur = { id, task: null, release: null, unsub: null, liveBox: null, latest: [], lastReload: 0 };
  openDrawer(t('归档任务 · 加载中…'), el('div', 'muted', t('加载中…')), () => { cleanup(); if (onClose) onClose(); });
  cur.unsub = subscribe((list) => { if (cur) { cur.latest = list; paintLive(list); } });
  await reload(false);
}

function cleanup() {
  if (!cur) return;
  if (cur.release) cur.release();
  if (cur.unsub) cur.unsub();
  cur = null;
}

