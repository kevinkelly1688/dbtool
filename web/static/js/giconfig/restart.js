/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: restart.js
 * @time: 2026/10/28
 */
// “保存并重启 / 回滚 / 仅重启”的确认弹窗，以及重启进度的轮询展示。
import { api } from '../common.js';
import { t, isZh } from '../i18n.js';
import { $, el, clear, openModal, closeModal, setMsg, confirmDialog, busy } from './ui.js';
import { BASE, state, reload } from './state.js';

const CANCELLED = Symbol('cancelled');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 重启类动作（保存并重启 / 回滚 / 仅重启）的前提：已设置合法的 Docker 容器名。返回不满足时的提示，满足则返回空串。
export function deployProblem(d) {
  if (!d || d.configured) return '';
  if (d.container) return d.preview_error ? t('已保存的 Docker 容器名不合法（{err}），请先到「部署与重启」里修正', { err: d.preview_error }) : t('已保存的 Docker 容器名不合法，请先到「部署与重启」里修正');
  return t('尚未设置 goInception 的 Docker 容器名，请先到「部署与重启」里填写');
}

// 开始重启类动作之前调用：不满足前提就给出提示并返回 false（按钮保持可点，点了只是提示，不发请求）。
export function ensureDeploy() {
  const p = deployProblem(state.ov && state.ov.deploy);
  if (!p) return true;
  setMsg(p, 'warn');
  return false;
}

function showErr(boxId, text) {
  const box = clear($(boxId));
  if (text) box.append(el('div', 'alert err', text));
}

// 提交请求；遇到“有警告 / 磁盘被改过”要先让用户确认，确认后带上确认标记重发。
async function submitWithConfirms(submit, opts) {
  for (;;) {
    try {
      return await submit(opts);
    } catch (e) {
      if (e.code === 'warnings') {
        const list = ((e.data && e.data.issues) || []).filter((i) => i.level === 'warn').map((i) => (i.line ? t('第 {line} 行：{msg}', { line: i.line, msg: i.message }) : i.message));
        if (!await confirmDialog({ title: t('配置文件有警告'), text: t('这些参数不是常规取值或不在已知列表里，确认无误再继续：'), list, okText: t('确认无误，继续') })) return CANCELLED;
        opts.ack_warnings = true;
      } else if (e.code === 'drift') {
        if (!await confirmDialog({ title: t('磁盘文件被人手工改过'), text: e.message, okText: t('覆盖并继续') })) return CANCELLED;
        opts.ack_drift = true;
      } else {
        throw e;
      }
    }
  }
}

function showTasks(data) {
  const n = data && typeof data.running === 'number' ? data.running : 0;
  if (n <= 0) { $('m-gi-restart-tasks').hidden = true; return; }
  const items = ((data && data.items) || []).map((tk) => (tk.id ? '#' + tk.id + ' ' : '') + (tk.info || tk.command || '')).filter(Boolean);
  $('m-gi-restart-taskmsg').textContent = items.length
    ? t('当前有 {n} 个任务正在通过 goInception 执行：{items}。立即重启会中断它们，被中断的工单会停在“执行有异常”，需要人工确认目标库状态。', { n, items: items.slice(0, 3).join(isZh ? '；' : '; ') })
    : t('当前有 {n} 个任务正在通过 goInception 执行。立即重启会中断它们，被中断的工单会停在“执行有异常”，需要人工确认目标库状态。', { n });
  $('m-gi-restart-tasks').hidden = false;
}

// 打开重启确认弹窗。submit(opts) 发起真正的请求，返回服务端的保存结果。
//   steps：要展示给用户的流程说明；withNote：是否显示“变更说明”输入框。
export async function confirmAndRun({ title, steps, withNote, submit, okText = t('确认并重启') }) {
  $('m-gi-restart-title').textContent = title;
  const ol = clear($('m-gi-restart-steps'));
  for (const s of steps) ol.append(el('li', '', s));
  $('m-gi-restart-notebox').hidden = !withNote;
  $('m-gi-restart-note').value = '';
  $('m-gi-restart-ok').textContent = okText;
  showErr('m-gi-restart-err', '');
  showTasks(null);
  const wantWait = state.ov && state.ov.deploy ? state.ov.deploy.wait_tasks : true;
  document.querySelector('input[name="gi-when"][value="' + (wantWait ? 'wait' : 'force') + '"]').checked = true;
  openModal('m-gi-restart');
  api('GET', BASE + '/status').then((st) => { if (st && st.running > 0) showTasks({ running: st.running, items: st.tasks }); }).catch(() => {});

  const ok = $('m-gi-restart-ok');
  ok.onclick = async () => {
    const opts = { summary: $('m-gi-restart-note').value.trim() };
    if (!$('m-gi-restart-tasks').hidden) {
      const when = document.querySelector('input[name="gi-when"]:checked').value;
      opts.force = when === 'force';
      opts.wait_tasks = when === 'wait';
    }
    showErr('m-gi-restart-err', '');
    busy(ok, true);
    try {
      const res = await submitWithConfirms(submit, opts);
      if (res === CANCELLED) return;
      closeModal('m-gi-restart');
      await afterSubmit(res);
    } catch (e) {
      if (e.code === 'busy') {
        showTasks(e.data);
        showErr('m-gi-restart-err', t('有任务正在执行，请选择等待或强制重启后再确认。'));
      } else if (e.code === 'stale') {
        showErr('m-gi-restart-err', e.message);
        reload().catch(() => {});
      } else {
        showErr('m-gi-restart-err', e.message);
      }
    } finally {
      busy(ok, false);
    }
  };
}

// 仅保存的弹窗（不重启）。
export function confirmSave({ path, submit }) {
  $('m-gi-save-path').textContent = path || t('配置文件');
  $('m-gi-save-note').value = '';
  showErr('m-gi-save-err', '');
  openModal('m-gi-save');
  const ok = $('m-gi-save-ok');
  ok.onclick = async () => {
    showErr('m-gi-save-err', '');
    busy(ok, true);
    try {
      const res = await submitWithConfirms(submit, { summary: $('m-gi-save-note').value.trim() });
      if (res === CANCELLED) return;
      closeModal('m-gi-save');
      await afterSubmit(res);
    } catch (e) {
      showErr('m-gi-save-err', e.message);
      if (e.code === 'stale') reload().catch(() => {});
    } finally {
      busy(ok, false);
    }
  };
}

async function afterSubmit(res) {
  if (res && res.restarting) {
    watchJob();
    return;
  }
  setMsg(res ? res.message : t('已完成'), 'info');
  await reload();
}

// ---- 进度 ----

let watching = false;

function renderJob(job) {
  const ol = clear($('m-gi-progress-steps'));
  for (const s of job.steps) {
    const li = el('li', s.status, s.name);
    if (s.detail) li.append(el('small', '', s.detail));
    ol.append(li);
  }
  const box = clear($('m-gi-progress-result'));
  if (!job.done) {
    $('m-gi-progress-title').textContent = t('正在重启 goInception…');
    return;
  }
  const kind = job.ok ? 'info' : job.result === 'rolledback' ? 'warn' : 'err';
  $('m-gi-progress-title').textContent = job.ok ? t('重启完成') : job.result === 'rolledback' ? t('重启失败，已自动恢复旧配置') : t('重启失败');
  box.append(el('div', 'alert ' + kind, job.message || ''));
}

// 轮询后台重启流程，直到结束。
export async function watchJob() {
  if (watching) { openModal('m-gi-progress'); return; }
  watching = true;
  const close = $('m-gi-progress-close');
  close.disabled = true;
  clear($('m-gi-progress-steps'));
  clear($('m-gi-progress-result'));
  $('m-gi-progress-title').textContent = t('正在重启 goInception…');
  openModal('m-gi-progress');
  let fails = 0;
  try {
    for (;;) {
      await sleep(1200);
      let data;
      try {
        data = await api('GET', BASE + '/restart/status');
        fails = 0;
      } catch (e) {
        if (++fails >= 8) { clear($('m-gi-progress-result')).append(el('div', 'alert err', t('无法获取重启进度：{msg}。流程仍在后台进行，请稍后刷新页面查看结果。', { msg: e.message }))); break; }
        continue;
      }
      const job = data && data.job;
      if (!job) break;
      renderJob(job);
      if (job.done) break;
    }
  } finally {
    close.disabled = false;
    watching = false;
    await reload().catch(() => {});
  }
}
