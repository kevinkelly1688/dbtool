/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: deploy.js
 * @time: 2026/10/28
 */
// “部署与重启”页签：配置文件路径、Docker 容器名（docker restart）、健康检查与回滚策略；以及进程状态卡片。
import { api } from '../common.js';
import { t, isZh } from '../i18n.js';
import { $, el, clear, setMsg, busy, fmtTime, confirmDialog } from './ui.js';
import { BASE, state, reload, restartLabel } from './state.js';
import { confirmAndRun, deployProblem, ensureDeploy } from './restart.js';

let filled = false;
let previewTimer = 0;

const num = (id) => Number($(id).value.trim());

function collect() {
  return {
    config_path: $('gi-d-path').value.trim(),
    container: $('gi-d-container').value.trim(), use_sudo: $('gi-d-sudo').checked,
    restart_timeout_sec: num('gi-d-rt'), health_timeout_sec: num('gi-d-ht'), wait_idle_max_sec: num('gi-d-wt'),
    auto_rollback: $('gi-d-auto').checked, wait_tasks: $('gi-d-wait').checked,
  };
}

function fill(d) {
  $('gi-d-path').value = d.config_path || '';
  $('gi-d-path').readOnly = d.config_path_source === 'env';
  $('gi-d-pathhint').textContent = d.config_path_source === 'env' ? t('由环境变量 GOINCEPTION_CONFIG_FILE 指定，不可修改') : '';
  $('gi-d-container').value = d.container || '';
  $('gi-d-sudo').checked = !!d.use_sudo;
  $('gi-d-rt').value = d.restart_timeout_sec;
  $('gi-d-ht').value = d.health_timeout_sec;
  $('gi-d-wt').value = d.wait_idle_max_sec;
  $('gi-d-auto').checked = !!d.auto_rollback;
  $('gi-d-wait').checked = !!d.wait_tasks;
  $('gi-d-preview').value = d.preview || d.preview_error || ''; // 容器名没填时留空，显示输入框里的占位模板
}

function schedulePreview() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(async () => {
    try {
      const r = await api('POST', BASE + '/deploy/preview', collect());
      // 容器名没填：留空显示模板；填了但不合法：显示原因
      $('gi-d-preview').value = r.preview || ($('gi-d-container').value.trim() ? r.problem || '' : '');
    } catch (_) { /* 预览失败不打扰 */ }
  }, 300);
}

// ---- 进程状态 ----

function dd(label, text, node) {
  const dl = $('gi-st');
  dl.append(el('dt', '', label));
  const d = el('dd');
  if (node) d.append(node); else d.textContent = text;
  dl.append(d);
}

function renderStatus(st, ov) {
  clear($('gi-st'));
  const badge = $('gi-status');
  badge.textContent = st ? st.state_label : t('未知');
  badge.className = 'st ' + (st && st.state === 'running' ? 'passed' : st && st.state === 'down' ? 'rejected' : 'aborted');
  const line = [];
  if (st && st.version) line.push(st.version);
  if (st && st.running != null) line.push(t('当前 {n} 个任务在执行', { n: st.running }));
  if (ov && ov.deploy) line.push(t('重启方式 {label}', { label: restartLabel(ov.deploy) }));
  $('gi-statusline').textContent = line.join(' · ');

  const stNode = el('span', badge.className, badge.textContent);
  dd(t('状态'), '', stNode);
  dd(t('版本'), st && st.version ? 'goInception ' + st.version : t('未知'));
  const conn = st && st.connections != null ? t('{conns} 个连接 · {tasks} 个任务', { conns: st.connections, tasks: st.running }) : t('未知');
  const taskText = st && st.tasks && st.tasks.length ? t('{conn}（{tasks}）', { conn, tasks: st.tasks.slice(0, 3).map((tk) => (tk.id ? '#' + tk.id + ' ' : '') + (tk.info || tk.command || '')).join(isZh ? '；' : '; ') }) : conn;
  dd(t('当前连接 / 任务'), st && st.task_note ? t('{text}（{note}）', { text: taskText, note: st.task_note }) : taskText);
  if (st && st.message) dd(t('说明'), st.message);
  if (ov && ov.deploy) dd(t('重启方式'), t('{label}：{cmd}', { label: restartLabel(ov.deploy), cmd: ov.deploy.preview || deployProblem(ov.deploy) }));
  const drift = ov ? ov.file.drift : st && st.drift;
  const cfgParts = [ov && ov.effective ? t('已生效版本 #{n}', { n: ov.effective.version }) : t('尚无生效版本')];
  if (ov && ov.pending) cfgParts.push(t('有待重启生效的 #{n}', { n: ov.pending.version }));
  if (drift) cfgParts.push(drift.message);
  dd(t('配置文件'), cfgParts.join(isZh ? '；' : '; '));
  const last = ov ? ov.last_restart : st && st.last_restart;
  if (last) {
    const res = { success: t('成功'), rolledback: t('失败，已自动恢复旧配置'), failed: t('失败') }[last.result] || last.result;
    const items = [res];
    if (last.duration_ms) items.push(t('耗时 {s} 秒', { s: (last.duration_ms / 1000).toFixed(1) }));
    items.push(last.operator);
    dd(t('最近一次重启'), fmtTime(last.at) + t('（{items}）', { items: items.join(isZh ? '，' : ', ') }) + (last.version ? t(' · 版本 #{n}', { n: last.version }) : ''));
  } else {
    dd(t('最近一次重启'), t('暂无记录'));
  }
}

export async function refreshStatus() {
  const btn = $('gi-st-refresh');
  busy(btn, true);
  try {
    const st = await api('GET', BASE + '/status');
    renderStatus(st, state.ov);
  } catch (e) {
    renderStatus(null, state.ov);
    setMsg(e.message);
  } finally {
    busy(btn, false);
  }
}

// ---- 动作 ----

async function save() {
  const btn = $('gi-d-save');
  busy(btn, true);
  try {
    await api('PUT', BASE + '/deploy', collect());
    await reload(true);
    fill(state.ov.deploy); // 按服务端保存的值回填（容器名会去掉前后空白）
    if (state.ov.deploy.container) setMsg(t('部署设置已保存'), 'info');
    else setMsg(t('部署设置已保存，但尚未填写 Docker 容器名。'), 'warn');
  } catch (e) {
    setMsg(e.message);
  } finally {
    busy(btn, false);
  }
}

async function check() {
  const btn = $('gi-d-check');
  busy(btn, true);
  const box = clear($('gi-d-result'));
  try {
    const r = await api('POST', BASE + '/deploy/check');
    const wrap = el('div', 'alert ' + (r.ok ? 'info' : 'err'));
    for (const l of r.lines) wrap.append(el('div', '', l));
    box.append(wrap);
  } catch (e) {
    box.append(el('div', 'alert err', e.message));
  } finally {
    busy(btn, false);
  }
}

// 把磁盘上的文件导入为新版本。asApplied：goInception 已经用这份文件重启过。
export async function importDisk(asApplied) {
  const ok = await confirmDialog({
    title: t('把磁盘文件导入为新版本'),
    text: asApplied ? t('磁盘文件将记为新版本并标记“当前生效”（仅当 goInception 已用它重启过时选这个）。') : t('磁盘文件将记为新版本并标记“待重启生效”。'),
    okText: t('导入'),
  });
  if (!ok) return;
  try {
    const r = await api('POST', BASE + '/file/import', { as_applied: asApplied, base_version: state.ov.latest_version });
    setMsg(r.message, 'info');
    await reload();
  } catch (e) {
    setMsg(e.message);
  }
}

export function restartOnly() {
  if (!ensureDeploy()) return;
  const ov = state.ov;
  const steps = [];
  if (ov.pending) steps.push(t('把待生效的版本 #{n} 写入磁盘', { n: ov.pending.version }));
  steps.push(t('重启方式 {label}：执行 {cmd}', { label: restartLabel(ov.deploy), cmd: ov.deploy.preview }));
  steps.push(t('健康检查：等待 goInception 可连接，最长 {n} 秒', { n: ov.deploy.health_timeout_sec }));
  if (ov.pending && ov.deploy.auto_rollback) steps.push(t('失败则自动恢复到上一个生效版本并再次重启'));
  confirmAndRun({
    title: ov.pending ? t('重启 goInception 使 #{n} 生效', { n: ov.pending.version }) : t('重启 goInception'),
    steps, withNote: false,
    submit: (opts) => api('POST', BASE + '/restart', { force: !!opts.force, wait_tasks: !!opts.wait_tasks, ack_drift: !!opts.ack_drift }),
  });
}

export function initDeploy() {
  document.addEventListener('gi:overview', (e) => {
    const { ov, keepEditor } = e.detail;
    if (!filled || !keepEditor) { fill(ov.deploy); filled = true; }
    else $('gi-d-preview').value = ov.deploy.preview || ov.deploy.preview_error || '';
    renderStatus(null, ov);
    refreshStatus();
  });
  $('gi-d-container').addEventListener('input', schedulePreview);
  $('gi-d-sudo').addEventListener('change', schedulePreview);
  $('gi-d-save').addEventListener('click', save);
  $('gi-d-check').addEventListener('click', check);
  $('gi-st-refresh').addEventListener('click', refreshStatus);
  $('gi-st-restart').addEventListener('click', restartOnly);
}
