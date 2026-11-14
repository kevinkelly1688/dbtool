/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: giconfig.js
 * @time: 2026/10/28
 */
// 系统管理 › goInception 配置（仅超级用户）：连接信息、配置文件、历史版本、部署与重启。
import { api } from '../common.js';
import { t, tp } from '../i18n.js';
import { $, el, clear, setMsg, openModal, renderDiff, busy, fmtTime } from './ui.js';
import { BASE, state, reload, restartLabel } from './state.js';
import { confirmAndRun, watchJob, deployProblem, ensureDeploy } from './restart.js';
import { initFile } from './file.js';
import { initDeploy, importDisk, restartOnly } from './deploy.js';

// ---- 页签 ----

function showTab(name) {
  document.querySelectorAll('#gi .tabbar .tab').forEach((b) => b.classList.toggle('on', b.dataset.tab === name));
  document.querySelectorAll('#gi [data-pane]').forEach((p) => { p.hidden = p.dataset.pane !== name; });
  try { history.replaceState(null, '', '#' + name); } catch (_) { /* ignore */ }
}

document.querySelectorAll('#gi .tabbar .tab').forEach((b) => b.addEventListener('click', () => showTab(b.dataset.tab)));
const initial = location.hash.replace('#', '');
if (['conn', 'file', 'hist', 'deploy'].includes(initial)) showTab(initial);

// ---- 连接信息 ----

const CONN = {
  main: { host: 'gi-c-host', port: 'gi-c-port', user: 'gi-c-user', pass: 'gi-c-pass', test: 'gi-c-test', save: 'gi-c-save', title: t('测试连接') },
  backup: { host: 'gi-b-host', port: 'gi-b-port', user: 'gi-b-user', pass: 'gi-b-pass', test: 'gi-b-test', save: 'gi-b-save', title: t('测试备份库') },
};

function fillConn(target, v) {
  const f = CONN[target];
  for (const k of ['host', 'port', 'user']) if (document.activeElement !== $(f[k])) $(f[k]).value = v[k] || '';
  $(f.pass).value = '';
  $(f.pass).placeholder = v.has_password ? t('已设置，留空不改') : t('未设置');
}

function connBody(target) {
  const f = CONN[target];
  return { target, host: $(f.host).value.trim(), port: $(f.port).value.trim(), user: $(f.user).value.trim(), password: $(f.pass).value };
}

for (const target of Object.keys(CONN)) {
  const f = CONN[target];
  $(f.save).addEventListener('click', async () => {
    const btn = $(f.save);
    busy(btn, true);
    try {
      await api('PUT', BASE + '/connection', connBody(target));
      setMsg(target === 'main' ? t('goInception 连接信息已保存，立即生效') : t('备份库连接信息已保存，立即生效'), 'info');
      await reload(true);
    } catch (e) {
      setMsg(e.message);
    } finally {
      busy(btn, false);
    }
  });
  $(f.test).addEventListener('click', async () => {
    const btn = $(f.test);
    busy(btn, true);
    try {
      const r = await api('POST', BASE + '/connection/test', connBody(target));
      $('m-gi-test-title').textContent = f.title;
      const body = clear($('m-gi-test-body'));
      const box = el('div', 'alert ' + (r.ok ? 'info' : 'err'));
      r.lines.forEach((l, i) => { if (i) box.append(el('br')); box.append(document.createTextNode((r.ok && i === 0 ? '✔ ' : !r.ok ? '✘ ' : '') + l)); });
      body.append(box);
      openModal('m-gi-test');
    } catch (e) {
      setMsg(e.message);
    } finally {
      busy(btn, false);
    }
  });
}

// ---- 历史版本 ----

const STATUS_CLASS = { applied: 'passed', draft: 'waiting', failed: 'rejected', rolledback: 'rejected' };

async function openDiff(v) {
  try {
    const d = await api('GET', BASE + '/versions/' + v.version + '/diff');
    $('m-gi-diff-title').textContent = (d.from ? '#' + d.from.version : t('空|none')) + ' → #' + d.to.version;
    renderDiff($('m-gi-diff-body'), d.diff);
    let res = '';
    if (d.to.status === 'applied') res = d.to.applied_at ? t('重启结果：成功，健康检查通过（{time}）。', { time: fmtTime(d.to.applied_at) }) : t('重启结果：成功，健康检查通过。');
    else if (d.to.status === 'draft') res = t('已写入磁盘，尚未重启生效。');
    else if (d.to.error) res = t('重启结果：{msg}', { msg: tp(d.to.error) });
    $('m-gi-diff-result').textContent = res;
    openModal('m-gi-diff');
  } catch (e) {
    setMsg(e.message);
  }
}

function rollback(v) {
  if (!ensureDeploy()) return;
  const ov = state.ov;
  const steps = [t('以 #{from} 的内容生成新版本 #{to}（变更：{summary}）', { from: v.version, to: ov.latest_version + 1, summary: v.change_summary || t('见版本对比') }), t('备份当前文件并写入')];
  steps.push(t('重启方式 {label}：执行 {cmd}', { label: restartLabel(ov.deploy), cmd: ov.deploy.preview }), t('健康检查：等待 goInception 可连接，最长 {n} 秒', { n: ov.deploy.health_timeout_sec }));
  if (ov.deploy.auto_rollback) steps.push(t('失败则自动恢复到回滚前的生效版本并再次重启'));
  confirmAndRun({
    title: t('回滚到 #{n}', { n: v.version }), steps, withNote: false, okText: t('确认回滚并重启'),
    submit: (opts) => api('POST', BASE + '/versions/' + v.version + '/rollback', { base_version: ov.latest_version, force: !!opts.force, wait_tasks: !!opts.wait_tasks, ack_drift: !!opts.ack_drift }),
  });
}

function renderHistory(ov) {
  const rows = clear($('gi-h-rows'));
  if (!ov.table_ready) {
    const td = el('td', 'empty', t('版本历史表未创建，请先执行 deploy/sql/011_gi_config.sql。'));
    td.colSpan = 6;
    rows.append(el('tr')).append(td);
    return;
  }
  if (!ov.versions.length) {
    const td = el('td', 'empty', t('还没有版本。'));
    td.colSpan = 6;
    rows.append(el('tr')).append(td);
    return;
  }
  for (const v of ov.versions) {
    const tr = el('tr', v.status === 'failed' || v.status === 'rolledback' ? 'gi-err' : '');
    const st = el('td');
    st.append(el('span', 'st ' + (v.label === '已被替换' ? 'aborted' : STATUS_CLASS[v.status] || 'aborted'), tp(v.label))); // i18n-ignore 与服务端返回的状态名比较
    const ops = el('td', 'row-actions');
    const view = el('button', 'btn', v.status === 'failed' || v.status === 'rolledback' ? t('查看原因') : t('查看'));
    view.type = 'button';
    view.addEventListener('click', () => openDiff(v));
    ops.append(view);
    if (!v.current && v.status !== 'failed' && v.status !== 'rolledback' && v.label !== '已保存，待重启生效') { // i18n-ignore 与服务端返回的状态名比较
      ops.append(document.createTextNode(' '));
      const rb = el('button', 'btn', t('回滚到此版本'));
      rb.type = 'button';
      rb.addEventListener('click', () => rollback(v));
      ops.append(rb);
    }
    tr.append(el('td', '', '#' + v.version), el('td', '', fmtTime(v.created_at)), el('td', '', v.operator), el('td', 'mono small', v.change_summary), st, ops);
    rows.append(tr);
  }
}

// ---- 横幅（表缺失 / 容器名未设置 / 文件漂移 / 待重启 / 流程进行中） ----

function banner(kind, text, actions = []) {
  const d = el('div', 'alert ' + kind);
  d.append(el('div', '', text));
  if (actions.length) {
    const box = el('div', 'gi-banner-actions');
    for (const [label, fn] of actions) {
      const b = el('button', 'btn', label);
      b.type = 'button';
      b.addEventListener('click', fn);
      box.append(b);
    }
    d.append(box);
  }
  $('gi-banner').append(d);
}

// 切到“部署与重启”页签并聚焦容器名输入框。
function goDeploy() {
  showTab('deploy');
  const c = $('gi-d-container');
  if (c) c.focus();
}

function renderBanner(ov) {
  clear($('gi-banner'));
  if (!ov.table_ready) {
    banner('err', t('版本历史表未创建，请先执行 deploy/sql/011_gi_config.sql。'));
  } else {
    const dp = deployProblem(ov.deploy);
    if (dp) banner('warn', dp, [[t('到「部署与重启」设置容器名'), goDeploy]]);
  }
  const f = ov.file;
  if (!f.path) {
    banner('warn', t('尚未配置 goInception 配置文件路径，请到“部署与重启”填写。'));
  } else if (f.read_error) {
    banner('err', t('读取配置文件失败：{msg}', { msg: f.read_error }));
  } else if (f.drift.kind === 'modified') {
    banner('warn', t('{msg}；保存会覆盖手工修改（留一份备份）。', { msg: f.drift.message }), [
      [t('导入磁盘文件（goInception 已用它重启过）'), () => importDisk(true)],
      [t('导入磁盘文件（待重启生效）'), () => importDisk(false)],
    ]);
  } else if (f.drift.kind === 'unmanaged' || f.drift.kind === 'missing') {
    const acts = f.drift.kind === 'unmanaged' ? [[t('把磁盘文件导入为第一个版本'), () => importDisk(true)]] : [];
    banner('info', f.drift.message, acts);
  }
  if (ov.pending) {
    banner('warn', t('有未生效的配置（#{n}），goInception 重启后才会使用。', { n: ov.pending.version }), [[t('仅重启使其生效'), restartOnly]]);
  }
  if (ov.job && !ov.job.done) banner('info', t('保存并重启流程正在进行。'), [[t('查看进度'), watchJob]]);
}

document.addEventListener('gi:overview', (e) => {
  const { ov } = e.detail;
  fillConn('main', ov.conn);
  fillConn('backup', ov.backup);
  renderHistory(ov);
  renderBanner(ov);
});

initFile();
initDeploy();

reload().then((ov) => {
  if (ov.job && !ov.job.done) watchJob();
}).catch((e) => setMsg(e.message));
