/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: actions.js
 * @time: 2026/10/28
 */
// 任务 / 运行上的操作：审批、驳回、暂停 / 恢复、立即执行、终止、停用 / 删除、恢复数据、试运行。
// 列表和详情抽屉共用；每个操作成功后调用 done()，由调用方刷新界面。
// 约定：任务视图的参数一律叫 task，不叫 t（免得遮住翻译函数 t()）。
import { t, tp } from '../i18n.js';
import { A } from './api.js';
import { el, add, nf, bq, clone, button, pbutton, withNodes } from './util.js';
import { flash, notice, openModal, modalMsg, riskConfirm, kvList, code, bold } from './ui.js';
import { backupTags, modeTag, MODES, showIf, can } from './consts.js';
import { checkNode } from './check_view.js';

// TaskView -> TaskInput（编辑 / 试运行用）
export function taskToInput(task) {
  return {
    name: task.name,
    resource_group_id: task.resource_group_id,
    instance_id: task.instance_id, db: task.db, table: task.table,
    time_column: task.time_column, time_unit: task.time_unit || '', retention_days: task.retention_days, extra_where: task.extra_where || '',
    mode: task.mode, purge_reason: task.purge_reason || '',
    dest_table: task.dest_table ? clone(task.dest_table) : null,
    dest_file: task.dest_file ? clone(task.dest_file) : null,
    params: clone(task.params),
    schedule: clone(task.schedule),
  };
}

function field(labelText, control) {
  const l = el('label', 'fld');
  l.append(document.createTextNode(labelText), control);
  return l;
}

function approvalSummary(task) {
  return kvList([
    [t('来源'), [task.instance_name, ' · ', code(`${task.db}.${task.table}`)]],
    [t('条件'), code(task.where || '')],
    [t('模式 / 备份'), [modeTag(task.mode), ' ', ...backupTags(task)]],
    [t('调度'), t('{schedule} · 窗口 {window}', { schedule: tp(task.schedule_text || ''), window: tp(task.window_text || '') })],
  ]);
}

// ---- 审批 ----
export function approve(task, done) {
  const last = task.approval && task.approval.current_level === task.approval.level_count - 1;
  const note = el('input');
  note.type = 'text';
  note.maxLength = 100;
  note.placeholder = t('可选');
  const body = el('div');
  add(body, notice(t('当前：{text}', { text: tp((task.approval && task.approval.pending_text) || '') })), approvalSummary(task));
  if (task.mode === 'purge') body.append(notice([t('仅清理（不备份），理由：'), task.purge_reason || ''], 'warn', '!'));
  body.append(field(t('审批意见'), note));
  openModal({
    title: t('审批通过 · {name}', { name: task.name }), body, ok: last ? t('通过并启用任务') : t('通过'),
    onOk: async () => {
      await A.approve(task.id, note.value.trim());
      flash(last ? t('已通过最后一级，「{name}」已启用', { name: task.name }) : t('已通过，已流转到下一级'), 'ok');
      done();
    },
  });
}

export function reject(task, done) {
  const why = el('textarea');
  why.rows = 3;
  why.maxLength = 200;
  why.placeholder = t('例：时间列没有索引');
  const body = el('div');
  body.append(field(t('驳回原因（必填）'), why));
  openModal({
    title: t('驳回 · {name}', { name: task.name }), body, ok: t('驳回'), okCls: 'danger',
    onOk: async () => {
      if (why.value.trim().length < 2) { modalMsg(t('请填写驳回原因')); return false; }
      await A.reject(task.id, why.value.trim());
      flash(t('已驳回，已通知申请人'), 'ok');
      done();
    },
  });
}

// ---- 暂停 / 恢复 ----
export async function pauseTask(task, done) {
  try { await A.pauseTask(task.id); flash(t('任务已暂停，不再按调度运行'), 'ok'); done(); } catch (e) { flash(e.message, 'err'); }
}
export async function resumeTask(task, done) {
  try { await A.resumeTask(task.id); flash(t('已恢复，按调度继续'), 'ok'); done(); } catch (e) { flash(e.message, 'err'); }
}
export async function pauseRun(runId, paused, done) {
  try {
    if (paused) await A.resumeRun(runId); else await A.pauseRun(runId);
    flash(paused ? t('已继续运行') : t('已暂停，当前批做完后停止'), 'ok');
    done();
  } catch (e) { flash(e.message, 'err'); }
}

// ---- 立即执行 ----
export function runNow(task, done) {
  const ign = el('input');
  ign.type = 'checkbox';
  const lab = el('label', 'chk');
  lab.style.marginTop = '10px';
  lab.append(ign, document.createTextNode(' ' + t('忽略执行窗口')));
  const m = MODES[task.mode] || [task.mode];
  const body = el('div');
  body.append(
    notice(withNodes(t('将{now}对 {table}（{inst}）执行一次归档：{mode}。单次最多处理 {rows} 行；{tail}', { inst: task.instance_name, mode: m[0], rows: nf(task.params.max_rows) }), {
      now: bold(t('立即')),
      table: code(`${task.db}.${task.table}`),
      tail: task.mode === 'copy' ? t('只备份，不删除。') : task.mode === 'purge' ? bold(t('不备份，删除后无法恢复。')) : t('先备份并校验，通过后才删源。'),
    }), 'bad', '⚠'),
    kvList([[t('条件'), code(task.where || '')], [t('执行窗口'), tp(task.window_text) || '—']]),
    lab,
  );
  openModal({
    title: t('⚠ 立即执行归档'), body, ok: t('确认执行'), okCls: 'danger', crit: true,
    onOk: async () => {
      const r = await A.runTask(task.id, ign.checked);
      flash(t('已开始运行 #{id}', { id: r.run_id }), 'ok');
      done(r);
    },
  });
}

// ---- 终止 ----
export function stopRun(run, done) {
  riskConfirm({
    title: t('终止运行'),
    body: notice(withNodes(t('终止「{name}」的运行 #{id}：当前批做完后结束，已处理的部分不会回滚，下次运行接着处理剩下的。', { id: run.id }), { name: bold(run.task_name) }), 'bad', '⚠'),
    ok: t('确认终止'),
    onOk: async () => {
      await A.stopRun(run.id);
      flash(t('运行 #{id} 已请求终止，当前批做完后结束', { id: run.id }), 'ok');
      done();
    },
  });
}

// ---- 停用 / 删除 ----
export function disableTask(task, done) {
  riskConfirm({
    title: t('停用归档任务'),
    body: notice(withNodes(task.status === 'pending'
      ? t('停用「{name}」后不再调度，进行中的审批作废；运行记录与已备份的数据保留。')
      : t('停用「{name}」后不再调度；运行记录与已备份的数据保留。'), { name: bold(task.name) }), 'warn', '!'),
    ok: t('确认停用'),
    onOk: async () => { await A.disableTask(task.id); flash(t('任务已停用'), 'ok'); done(); },
  });
}

export function deleteTask(task, done) {
  riskConfirm({
    title: t('删除归档任务'),
    body: notice(withNodes(t('将删除任务「{name}」的配置；历史运行记录与已备份的数据保留。'), { name: bold(task.name) }), 'warn', '!'),
    ok: t('确认删除'), nameToType: task.name,
    onOk: async () => { await A.deleteTask(task.id, task.name); flash(t('任务已删除'), 'ok'); done(true); },
  });
}

// ---- 恢复数据 ----
export function restoreRun(run, done) {
  const tb = run.dest_table || '';
  const [bd, bt] = tb.includes('.') ? tb.split('.') : ['', tb];
  const [sd, st] = String(run.table || '').split('.');
  const pk = (run.pk_cols && run.pk_cols[0]) || 'id';
  const again = run.restored_by && run.restored_by.length;
  const sql = `INSERT IGNORE INTO ${bq(sd)}.${bq(st)}\nSELECT * FROM ${bq(bd)}.${bq(bt)}\n WHERE ${bq(pk)} BETWEEN ${(run.min_pk || ['?'])[0]} AND ${(run.max_pk || ['?'])[0]};`;
  const body = el('div');
  body.append(notice(withNodes(t('按运行 #{id} 的主键范围把 {rows} 行从 {from} 以 {insert} 回灌到源表 {to}（{inst}）。{skip}已存在的行。{again}', { id: run.id, inst: run.instance_name }), {
    rows: bold(nf(run.backed)),
    from: code(tb),
    insert: code('INSERT IGNORE'),
    to: code(run.table),
    skip: bold(t('只补缺、不覆盖')),
    again: again ? bold(t('该运行已恢复过，再次恢复通常是空操作。')) : '',
  }), 'bad', '⚠'));
  riskConfirm({
    title: t('恢复数据（回灌到源表）'), body, sql, ok: t('确认恢复'), nameToType: run.task_name,
    onOk: async () => {
      const r = await A.restoreRun(run.id, run.task_name);
      flash(t('已开始恢复，恢复运行 #{id}', { id: r.run_id }), 'ok');
      done(r);
    },
  });
}

// ---- 对已有任务试运行（结果在弹窗里看；提交审批要在“编辑 / 重新提交”向导里走） ----
export async function checkTask(task) {
  flash(t('正在试运行…'), 'info', true);
  try {
    const input = { ...taskToInput(task), task_id: task.id };
    const chk = await A.check(input);
    flash('');
    const body = el('div');
    if (chk.blocked) body.append(notice(t('有红色检查项未通过，不能提交审批。'), 'bad', '✕'));
    body.append(checkNode(chk, task.mode));
    openModal({ title: t('试运行结果 · {name}', { name: task.name }), body, ok: '', cancel: t('关闭'), size: 'xwide' });
  } catch (e) {
    flash(e.message, 'err');
  }
}


// ---- 任务按钮（列表与详情抽屉共用）。opt: {compact, done(refreshAll), edit(true=含编辑/试运行), goto} ----
export function taskButtons(task, opt) {
  const cls = opt.compact ? 'ghost' : '';
  const al = task.allow || {};
  const out = [];
  const add1 = (b) => { out.push(b); return b; };
  const run = task.running; // {run_id, state}
  const done = opt.done;
  if (al.approve) add1(button(t('通过'), 'primary', () => approve(task, done)));
  if (al.reject) add1(button(t('驳回'), 'danger', () => reject(task, done)));
  if (opt.full) {
    if (showIf(!run, al.edit, 'archive.apply')) add1(pbutton('archive.apply', t('编辑 / 重新提交'), cls, () => { opt.goto('#wizard/' + task.id); }));
    if (showIf(true, al.check, 'archive.dryrun')) add1(pbutton('archive.dryrun', t('试运行'), cls, () => checkTask(task)));
  }
  if (showIf(task.status === 'enabled' && !run, al.run, 'archive.run')) add1(pbutton('archive.run', t('立即执行'), cls, () => runNow(task, done)));
  if (showIf(task.status === 'enabled' && !run, al.pause, 'archive.pause')) add1(pbutton('archive.pause', opt.full ? t('暂停任务') : t('暂停'), cls, () => pauseTask(task, done)));
  if (run) {
    const paused = run.state === 'paused';
    add1(pbutton('archive.pause', paused ? (opt.full ? t('继续运行') : t('继续')) : (opt.full ? t('暂停运行') : t('暂停')), cls, () => pauseRun(run.run_id, paused, done)));
    add1(pbutton('archive.stop', opt.full ? t('终止运行') : t('终止'), 'danger ' + cls, () => stopRun({ id: run.run_id, task_name: task.name }, done)));
  }
  if (showIf(task.status === 'paused' && !(task.pause && task.pause.kind === 'structure') && !run, al.resume, 'archive.pause')) add1(pbutton('archive.pause', opt.full ? t('恢复任务') : t('恢复|resume'), cls, () => resumeTask(task, done)));
  if (opt.full) {
    if (showIf(['enabled', 'paused', 'pending'].includes(task.status) && !run, al.disable, 'archive.delete')) add1(pbutton('archive.delete', t('停用任务'), 'danger', () => disableTask(task, done)));
    if (showIf(['draft', 'rejected', 'disabled'].includes(task.status), al.delete, 'archive.delete')) add1(pbutton('archive.delete', t('删除任务'), 'danger', () => deleteTask(task, done)));
  }
  return out;
}
