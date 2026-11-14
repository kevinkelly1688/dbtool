/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: file.js
 * @time: 2026/10/28
 */
// “配置文件”页签：常用参数表单 + config.toml 源文件，双向同步；校验、对比、保存、保存并重启。
import { api } from '../common.js';
import { t } from '../i18n.js';
import { $, el, clear, setMsg, renderDiff, renderReport, busy, fmtTime } from './ui.js';
import { BASE, state, restartLabel } from './state.js';
import { confirmAndRun, confirmSave, ensureDeploy } from './restart.js';

const src = $('gi-src');
const controls = new Map(); // "段.参数名" → { item, input, badge, kind }
let built = false;
let dirty = false; // 编辑框里有尚未保存的修改
let secrets = {}; // 页面上新填的密码类参数
let baseValues = {}; // 载入时源文件里的参数值，用来标“已修改”
let baseVersion = 0;
let hasErrors = false;
const timers = {};

const idOf = (it) => it.section + '.' + it.key;

function later(name, fn, ms) {
  clearTimeout(timers[name]);
  timers[name] = setTimeout(fn, ms);
}

// ---- 表单 ----

function buildForm(groups) {
  const root = clear($('gi-form'));
  controls.clear();
  groups.forEach((g, gi) => {
    const det = el('details', 'acc');
    det.open = gi === 0;
    const sum = el('summary', '', g.title + ' ');
    sum.append(el('small', 'muted', g.tag));
    det.append(sum);
    const grid = el('div', 'form-grid');
    grid.style.padding = '10px 14px';
    for (const it of g.items) {
      const id = idOf(it);
      const badge = el('span', 'badge warn gi-modified', t('已修改'));
      badge.hidden = true;
      let input;
      let label;
      if (it.kind === 'bool') {
        label = el('label', 'check');
        input = document.createElement('input');
        input.type = 'checkbox';
        label.append(input, document.createTextNode(' ' + it.label), badge);
        input.addEventListener('change', () => { input.indeterminate = false; patch([{ section: it.section, key: it.key, value: input.checked }]); });
      } else if (it.kind === 'enum') {
        label = el('label', '', it.label);
        input = document.createElement('select');
        input.append(new Option(t('（未设置）'), ''));
        for (const v of it.enum) input.append(new Option(v, v));
        label.append(badge, input);
        input.addEventListener('change', () => { if (input.value) patch([{ section: it.section, key: it.key, value: input.value }]); });
      } else if (it.secret) {
        label = el('label', '', it.label);
        input = document.createElement('input');
        input.type = 'password';
        input.autocomplete = 'new-password';
        label.append(badge, input);
        input.addEventListener('input', () => {
          if (input.value) secrets[id] = input.value; else delete secrets[id];
          dirty = true;
          markModified();
          later('diff', refreshDiff, 500);
        });
      } else {
        label = el('label', '', it.label);
        input = document.createElement('input');
        input.type = 'text';
        input.className = 'mono';
        input.autocomplete = 'off';
        label.append(badge, input);
        input.addEventListener('change', () => commitText(it, input));
      }
      controls.set(id, { item: it, input, badge });
      grid.append(label);
    }
    det.append(grid);
    root.append(det);
  });
  built = true;
}

function commitText(it, input) {
  const v = input.value.trim();
  if (v === '') { later('params', refreshParams, 0); return; } // 清空不等于删除参数；还原显示
  let value = v;
  if (it.kind === 'int') {
    if (!/^-?\d+$/.test(v)) { setMsg(t('{id} 需要整数', { id: idOf(it) })); return; }
    value = Number(v);
  } else if (it.kind === 'float') {
    if (!Number.isFinite(Number(v))) { setMsg(t('{id} 需要数字', { id: idOf(it) })); return; }
    value = Number(v);
  }
  setMsg('');
  patch([{ section: it.section, key: it.key, value }]);
}

function applyValues(values, secretSet) {
  for (const [id, c] of controls) {
    if (document.activeElement === c.input) continue;
    const present = Object.prototype.hasOwnProperty.call(values, id);
    if (c.item.secret) {
      const set = secretSet.includes(id);
      c.input.placeholder = secrets[id] ? '' : set ? t('已设置，留空不改') : t('未设置');
      if (!secrets[id]) c.input.value = '';
    } else if (c.item.kind === 'bool') {
      c.input.indeterminate = !present;
      c.input.checked = present ? !!values[id] : false;
    } else if (c.item.kind === 'enum') {
      c.input.value = present ? String(values[id]) : '';
    } else {
      c.input.value = present ? String(values[id]) : '';
      c.input.placeholder = present ? '' : t('未设置（默认）');
    }
  }
  markModified(values);
}

let lastValues = {};
function markModified(values) {
  if (values) lastValues = values;
  for (const [id, c] of controls) {
    const changed = c.item.secret ? !!secrets[id] : Object.prototype.hasOwnProperty.call(lastValues, id) && String(lastValues[id]) !== String(baseValues[id]);
    const added = !c.item.secret && Object.prototype.hasOwnProperty.call(lastValues, id) && !Object.prototype.hasOwnProperty.call(baseValues, id);
    c.badge.hidden = !(changed || added);
  }
}

// ---- 与服务端同步 ----

async function patch(changes) {
  try {
    const out = await api('POST', BASE + '/file/patch', { content: src.value, changes });
    src.value = out.content;
    dirty = true;
    later('params', refreshParams, 0);
  } catch (e) {
    setMsg(e.message);
    later('params', refreshParams, 0);
  }
}

async function refreshParams() {
  try {
    const res = await api('POST', BASE + '/file/params', { content: src.value });
    hasErrors = res.report.errors > 0;
    renderReport($('gi-valid'), res.report);
    applyValues(res.values, res.secret_set);
    if (!Object.keys(baseValues).length && !dirty) baseValues = res.values;
    markModified(res.values);
    updateButtons();
  } catch (e) {
    clear($('gi-valid')).append(el('div', 'alert err', e.message));
  }
  later('diff', refreshDiff, 200);
}

async function refreshDiff() {
  try {
    const res = await api('POST', BASE + '/file/diff', { content: src.value, secrets });
    const from = res.from ? (res.from.current ? t('当前生效 #{n}', { n: res.from.version }) : t('当前版本 #{n}', { n: res.from.version })) : t('磁盘上的文件');
    $('gi-diff-title').textContent = t('{from} → 待保存', { from });
    renderDiff($('gi-diff'), res.diff);
  } catch (e) {
    clear($('gi-diff')).append(el('span', 'ctx', t('无法生成对比：{msg}', { msg: e.message })));
  }
}

function updateButtons() {
  const ov = state.ov;
  const off = !ov || !ov.table_ready || !ov.file.path || hasErrors;
  $('gi-f-save').disabled = off;
  $('gi-f-saverestart').disabled = off;
}

// ---- 概览渲染 ----

function onOverview(e) {
  const { ov, keepEditor } = e.detail;
  if (!built) buildForm(ov.catalog);
  $('gi-f-path').textContent = ov.file.path || t('（未配置路径）');
  const eff = $('gi-f-eff');
  if (ov.effective) {
    eff.textContent = t('当前生效 #{n}', { n: ov.effective.version });
    eff.className = 'badge ok';
  } else {
    eff.textContent = t('尚无生效版本');
    eff.className = 'badge';
  }
  const parts = [];
  if (ov.file.exists) parts.push(t('文件最后修改 {time}', { time: fmtTime(ov.file.mod_time) }) + (ov.file.mode ? t(' · 权限 {mode}', { mode: ov.file.mode }) : ''));
  if (ov.effective) parts.push(t('最近版本 {time} · {operator}', { time: fmtTime(ov.effective.created_at), operator: ov.effective.operator }));
  $('gi-f-meta').textContent = parts.join('　');

  if (keepEditor && dirty) {
    updateButtons();
    return;
  }
  src.value = ov.file.editor;
  $('gi-f-from').textContent = t('内容来源：{src}', { src: ov.file.editor_from });
  baseVersion = ov.latest_version;
  secrets = {};
  dirty = false;
  baseValues = {};
  lastValues = {};
  updateButtons();
  later('params', refreshParams, 0);
}

// ---- 动作 ----

function flowSteps() {
  const ov = state.ov;
  const d = ov.deploy;
  const next = ov.latest_version + 1;
  const steps = [t('校验配置文件（语法 / 参数名 / 取值范围）'), t('备份当前文件并写入新版本 #{n}', { n: next })];
  steps.push(t('重启方式 {label}：执行 {cmd}', { label: restartLabel(d), cmd: d.preview }));
  steps.push(t('健康检查：等待 goInception 可连接，最长 {n} 秒', { n: d.health_timeout_sec }));
  if (d.auto_rollback) steps.push(t('失败则自动恢复到上一个生效版本并再次重启，结果记录在版本历史里'));
  return steps;
}

function saveBody(restart, opts) {
  return { content: src.value, secrets, base_version: baseVersion, restart, ...opts };
}

async function check() {
  const btn = $('gi-f-check');
  busy(btn, true);
  try {
    const rep = await api('POST', BASE + '/file/validate', { content: src.value });
    hasErrors = rep.errors > 0;
    renderReport($('gi-valid'), rep);
    updateButtons();
  } catch (e) {
    setMsg(e.message);
  } finally {
    busy(btn, false);
  }
}

async function restore() {
  const ov = state.ov;
  try {
    if (ov.effective) {
      const v = await api('GET', BASE + '/versions/' + ov.effective.version);
      src.value = v.content;
      $('gi-f-from').textContent = t('内容来源：当前生效的 #{n}', { n: v.version });
    } else {
      src.value = ov.file.editor;
    }
    secrets = {};
    dirty = true;
    later('params', refreshParams, 0);
  } catch (e) {
    setMsg(e.message);
  }
}

function saveOnly() {
  confirmSave({
    path: state.ov.file.path,
    submit: (opts) => api('POST', BASE + '/file/save', saveBody(false, opts)),
  });
}

function saveAndRestart() {
  if (!ensureDeploy()) return;
  confirmAndRun({
    title: t('保存并重启 goInception'),
    steps: flowSteps(),
    withNote: true,
    submit: (opts) => api('POST', BASE + '/file/save', saveBody(true, opts)),
  });
}

export function initFile() {
  document.addEventListener('gi:overview', onOverview);
  src.addEventListener('input', () => { dirty = true; later('params', refreshParams, 400); });
  $('gi-f-check').addEventListener('click', check);
  $('gi-f-restore').addEventListener('click', restore);
  $('gi-f-save').addEventListener('click', saveOnly);
  $('gi-f-saverestart').addEventListener('click', saveAndRestart);
  const view = $('gi-f-view');
  try { view.value = localStorage.getItem('gi-view') === 'src' ? 'src' : 'both'; } catch (_) { /* 无存储时用默认 */ }
  const applyView = () => $('gi-f-cols').classList.toggle('src-only', view.value === 'src');
  view.addEventListener('change', () => { applyView(); try { localStorage.setItem('gi-view', view.value); } catch (_) { /* ignore */ } });
  applyView();
  window.addEventListener('beforeunload', (e) => { if (dirty) { e.preventDefault(); e.returnValue = ''; } });
}
