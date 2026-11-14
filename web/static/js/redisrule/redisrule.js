/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: redisrule.js
 * @time: 2026/10/28
 */
// Redis 上线规则页（仅超级用户）：规则列表、新增 / 编辑、启停、删除（都要二次确认）、从模板添加、规则测试。
// 所有往 DOM 写的内容都用 textContent，不拼 innerHTML。真正的校验与授权在服务端，这里的校验只是即时提示。
import { api } from '../common.js';
import { applyButtons } from '../btnperm.js';
import { t, isZh } from '../i18n.js';

const $ = (id) => document.getElementById(id);

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

const state = { rules: [], instances: [], supported: [], forbidden: [], templates: [], ready: true, defMax: 100, maxLimit: 10000 };
let editing = null; // 正在编辑的规则 id；null = 新增
let pending = null; // 等待二次确认的操作

function setMsg(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

// ---- 弹窗 ----
const stack = [];
function openModal(id) {
  const m = $(id);
  m.hidden = false;
  stack.push(id);
  const f = m.querySelector('input:not([type=hidden]):not([type=checkbox]), textarea, button.primary, button.danger');
  if (f) f.focus();
}
function closeModal(id) {
  const m = $(id);
  if (!m) return;
  m.hidden = true;
  const i = stack.lastIndexOf(id);
  if (i >= 0) stack.splice(i, 1);
}
document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-close]');
  if (!b) return;
  const mask = b.closest('.modal-mask');
  if (mask) closeModal(mask.id);
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && stack.length) closeModal(stack[stack.length - 1]);
});

// ---- 工具 ----
const instName = (id) => {
  const i = state.instances.find((x) => x.id === id);
  return i ? i.name : '#' + id;
};

function scopeText(r) {
  const sep = isZh ? '、' : ', ';
  const inst = r.instance_ids && r.instance_ids.length ? r.instance_ids.map(instName).join(sep) : t('全部实例');
  const db = r.dbs && r.dbs.length ? r.dbs.map((d) => 'db' + d).join(sep) : t('全部库');
  return inst + ' / ' + db;
}

const pad = (n) => String(n).padStart(2, '0');
function fmtTime(iso) {
  const d = new Date(iso);
  if (!iso || Number.isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function parseCmds(text) {
  const seen = new Set();
  const out = [];
  for (const p of text.split(/[\s,，]+/)) {
    const n = p.trim().toUpperCase();
    if (n && !seen.has(n)) {
      seen.add(n);
      out.push(n);
    }
  }
  return out;
}

const forbiddenSet = () => new Set(state.forbidden.map((x) => x.split(/\s|…/)[0]));

function cmdProblems(cmds) {
  const bad = forbiddenSet();
  const ok = new Set(state.supported);
  const out = [];
  for (const c of cmds) {
    if (bad.has(c)) out.push(t('命令 {cmd} 属于硬禁止命令，任何规则都无法放行', { cmd: c }));
    else if (!ok.has(c)) out.push(t('命令 {cmd} 不在受支持的写命令清单里（无键命令、读命令与未知命令不能写入规则）', { cmd: c }));
  }
  return out;
}

// ---- 加载与渲染 ----
async function load() {
  try {
    const d = await api('GET', '/api/v1/settings/redis-rules');
    state.rules = d.rules || [];
    state.instances = d.instances || [];
    state.supported = d.supported_commands || [];
    state.forbidden = d.hard_forbidden || [];
    state.templates = d.templates || [];
    state.ready = d.table_ready !== false;
    state.defMax = d.default_max_keys || 100;
    state.maxLimit = d.max_keys_limit || 10000;
    $('rr-missing').hidden = state.ready;
    $('rr-maxkeys-limit').textContent = t('键数上限（1 ~ {n}）', { n: state.maxLimit });
    renderTemplates();
    renderChips();
    render();
  } catch (e) {
    setMsg($('rr-msg'), e.message);
  }
}

function renderTemplates() {
  const sel = $('rr-tpl');
  sel.replaceChildren(new Option(t('选择模板…'), ''));
  state.templates.forEach((tpl) => sel.append(new Option(tpl.title, tpl.key)));
}

function renderChips() {
  const f = $('rr-forbidden');
  f.replaceChildren();
  state.forbidden.forEach((c) => f.append(el('span', 'chip bad', c)));
  const s = $('rr-supported');
  s.replaceChildren();
  state.supported.forEach((c) => s.append(el('span', 'chip', c)));
}

function chips(list, empty) {
  const box = el('div', 'chips');
  if (!list || !list.length) {
    box.append(el('span', 'muted', empty));
    return box;
  }
  list.forEach((x) => box.append(el('span', 'chip', x)));
  return box;
}

function render() {
  $('rr-count').textContent = t('共 {total} 条，启用 {on} 条', { total: state.rules.length, on: state.rules.filter((r) => r.enabled).length });
  const tb = $('rr-table').tBodies[0];
  tb.replaceChildren();
  if (!state.rules.length) {
    const tr = el('tr');
    const td = el('td', 'empty', state.ready ? t('还没有规则（Redis 工单全部拒绝）。') : t('规则表未创建。'));
    td.colSpan = 10;
    tr.append(td);
    tb.append(tr);
    return;
  }
  for (const r of state.rules) {
    const tr = el('tr', r.enabled ? '' : 'rr-off');
    tr.append(el('td', '', r.name));
    const st = el('td');
    st.append(el('span', 'st ' + (r.enabled ? 'passed' : 'aborted'), r.enabled ? t('启用|status') : t('停用|status')));
    tr.append(st);
    const c = el('td', 'rr-cell');
    c.append(chips(r.commands, '—'));
    tr.append(c);
    const k = el('td', 'rr-cell');
    k.append(chips(r.key_patterns, '—'));
    tr.append(k);
    tr.append(el('td', 'rr-cell', scopeText(r)));
    tr.append(el('td', 'num', String(r.max_keys)));
    tr.append(el('td', '', r.record_before ? t('是') : t('否')));
    tr.append(el('td', 'rr-cell', r.description || ''));
    tr.append(el('td', 'small', `${r.updated_by || ''} ${fmtTime(r.updated_at)}`.trim()));
    const ops = el('td', 'rr-ops');
    const edit = el('button', 'btn', t('编辑'));
    edit.type = 'button';
    edit.dataset.btn = 'redisrule.edit';
    edit.addEventListener('click', () => openEdit(r));
    const tog = el('button', 'btn', r.enabled ? t('停用') : t('启用'));
    tog.type = 'button';
    tog.dataset.btn = 'redisrule.edit';
    tog.addEventListener('click', () => askToggle(r));
    const del = el('button', 'btn ghost danger', t('删除'));
    del.type = 'button';
    del.dataset.btn = 'redisrule.edit';
    del.addEventListener('click', () => askDelete(r));
    ops.append(edit, ' ', tog, ' ', del);
    tr.append(ops);
    tb.append(tr);
  }
  applyButtons(tb);
}

// ---- 编辑器 ----
function fillForm(r) {
  $('rr-f-name').value = r.name || '';
  $('rr-f-enabled').checked = r.enabled !== false;
  $('rr-f-cmds').value = (r.commands || []).join(',');
  $('rr-f-pats').value = (r.key_patterns || []).join('\n');
  const sel = $('rr-f-insts');
  sel.replaceChildren();
  state.instances.forEach((i) => {
    const o = new Option(i.name, String(i.id));
    o.selected = (r.instance_ids || []).includes(i.id);
    sel.append(o);
  });
  // 规则里限定了已不存在 / 已不是 Redis 的实例：保留，避免保存时悄悄丢掉范围
  (r.instance_ids || []).filter((id) => !state.instances.some((i) => i.id === id)).forEach((id) => {
    const o = new Option(t('#{id}（实例不存在或不是 Redis）', { id }), String(id));
    o.selected = true;
    sel.append(o);
  });
  $('rr-f-dbs').value = (r.dbs || []).join(',');
  $('rr-f-maxkeys').value = String(r.max_keys || state.defMax);
  $('rr-f-before').checked = !!r.record_before;
  $('rr-f-desc').value = r.description || '';
  $('rr-f-remark').value = r.remark || '';
  refreshCmdHint();
}

function openEdit(rule, preset) {
  editing = rule ? rule.id : null;
  $('rr-edit-title').textContent = rule ? t('编辑规则：{name}', { name: rule.name }) : preset ? t('新增规则（来自模板：{title}）', { title: preset.title }) : t('新增规则');
  setMsg($('rr-edit-msg'), preset ? t('已按模板填入，请核对后保存。') : '', 'info');
  fillForm(rule || (preset ? preset.rule : { enabled: true, max_keys: state.defMax }));
  openModal('rr-m-edit');
}

function readForm() {
  const dbs = $('rr-f-dbs').value.split(/[\s,，]+/).filter(Boolean);
  for (const d of dbs) {
    if (!/^\d+$/.test(d)) throw new Error(t('库编号 {db} 不合法，应为 0 ~ 1023 的整数', { db: d }));
  }
  const mk = $('rr-f-maxkeys').value.trim();
  if (mk !== '' && !/^\d+$/.test(mk)) throw new Error(t('键数上限应为整数'));
  return {
    name: $('rr-f-name').value.trim(),
    enabled: $('rr-f-enabled').checked,
    commands: parseCmds($('rr-f-cmds').value),
    key_patterns: $('rr-f-pats').value.split('\n').map((x) => x.trim()).filter(Boolean),
    instance_ids: [...$('rr-f-insts').selectedOptions].map((o) => Number(o.value)),
    dbs: dbs.map(Number),
    max_keys: mk === '' ? 0 : Number(mk),
    record_before: $('rr-f-before').checked,
    description: $('rr-f-desc').value.trim(),
    remark: $('rr-f-remark').value.trim(),
  };
}

function refreshCmdHint() {
  const probs = cmdProblems(parseCmds($('rr-f-cmds').value));
  $('rr-cmd-err').textContent = probs.join(isZh ? '；' : '; ');
  const pick = $('rr-cmd-pick');
  pick.replaceChildren();
  const have = new Set(parseCmds($('rr-f-cmds').value));
  state.supported.filter((c) => !have.has(c)).forEach((c) => {
    const b = el('button', 'chip pick', c);
    b.type = 'button';
    b.addEventListener('click', () => {
      const cur = $('rr-f-cmds').value.trim().replace(/[,，]\s*$/, '');
      $('rr-f-cmds').value = cur ? cur + ',' + c : c;
      refreshCmdHint();
    });
    pick.append(b);
  });
}

// ---- 差异（与服务端审计里的字段一致）----
function fieldsOf(r) {
  return [
    [t('名称'), r.name || ''], [t('启用'), r.enabled ? t('是') : t('否')], [t('命令'), (r.commands || []).join(',')], [t('键模式'), (r.key_patterns || []).join(' | ')],
    [t('实例'), (r.instance_ids || []).map(instName).join(',') || t('全部')], [t('库'), (r.dbs || []).join(',') || t('全部')], [t('键数上限'), String(r.max_keys || state.defMax)],
    [t('记录执行前值'), r.record_before ? t('是') : t('否')], [t('说明'), r.description || ''], [t('备注'), r.remark || ''],
  ];
}

function diffText(oldR, newR) {
  const a = fieldsOf(oldR || {});
  const b = fieldsOf(newR);
  const lines = [];
  b.forEach(([label, val], i) => {
    if (!oldR) {
      if (val !== '') lines.push(t('{label}：{val}', { label, val }));
    } else if (a[i][1] !== val) {
      lines.push(t('{label}：{from} → {to}', { label, from: a[i][1] || t('（空）'), to: val || t('（空）') }));
    }
  });
  return lines.length ? lines.join('\n') : t('没有字段变化');
}

// ---- 二次确认 ----
function askConfirm({ title, text, diff, run }) {
  pending = run;
  $('rr-confirm-title').textContent = title;
  $('rr-confirm-text').textContent = text;
  $('rr-confirm-diff').textContent = diff || '';
  $('rr-confirm-diff').hidden = !diff;
  $('rr-confirm-ok').disabled = false;
  openModal('rr-m-confirm');
}

$('rr-confirm-ok').addEventListener('click', async () => {
  if (!pending) return;
  const run = pending;
  const btn = $('rr-confirm-ok');
  btn.disabled = true;
  try {
    await run();
    pending = null;
    closeModal('rr-m-confirm');
  } catch (e) {
    closeModal('rr-m-confirm');
    // 校验类错误回到编辑器里显示；其他错误显示在页面顶部
    if (!$('rr-m-edit').hidden) setMsg($('rr-edit-msg'), e.message);
    else setMsg($('rr-msg'), e.message);
  } finally {
    btn.disabled = false;
  }
});

function onSaved(text) {
  closeModal('rr-m-edit');
  setMsg($('rr-msg'), text, 'info');
  return load();
}

$('rr-edit-save').addEventListener('click', () => {
  setMsg($('rr-edit-msg'), '');
  let body;
  try {
    body = readForm();
  } catch (e) {
    return setMsg($('rr-edit-msg'), e.message);
  }
  const probs = cmdProblems(body.commands);
  if (probs.length) return setMsg($('rr-edit-msg'), probs[0]);
  if (!body.name) return setMsg($('rr-edit-msg'), t('请填写规则名称'));
  if (!body.commands.length) return setMsg($('rr-edit-msg'), t('请至少选择一个命令'));
  if (!body.key_patterns.length) return setMsg($('rr-edit-msg'), t('请至少填写一个键匹配模式'));
  const old = editing ? state.rules.find((r) => r.id === editing) : null;
  askConfirm({
    title: old ? t('确认修改规则「{name}」', { name: old.name }) : t('确认新增规则「{name}」', { name: body.name }),
    text: old ? t('将按下面的差异修改规则：') : t('将新增下面的规则：'),
    diff: diffText(old, body),
    run: async () => {
      if (old) await api('PUT', `/api/v1/settings/redis-rules/${old.id}`, { ...body, confirm: true });
      else await api('POST', '/api/v1/settings/redis-rules', { ...body, confirm: true });
      await onSaved(old ? t('规则「{name}」已修改。', { name: body.name }) : t('规则「{name}」已新增。', { name: body.name }));
    },
  });
});

function askToggle(r) {
  const to = !r.enabled;
  askConfirm({
    title: to ? t('确认启用规则「{name}」', { name: r.name }) : t('确认停用规则「{name}」', { name: r.name }),
    text: to ? t('启用后，匹配这条规则的 Redis 命令将被允许提交和执行。') : t('停用后，只靠这条规则放行的命令将被拒绝（审批中的工单执行时也会按新规则复检）。'),
    diff: t('启用：{from} → {to}', { from: r.enabled ? t('是') : t('否'), to: to ? t('是') : t('否') }),
    run: async () => {
      await api('POST', `/api/v1/settings/redis-rules/${r.id}/enable`, { enabled: to, confirm: true });
      setMsg($('rr-msg'), to ? t('规则「{name}」已启用。', { name: r.name }) : t('规则「{name}」已停用。', { name: r.name }), 'info');
      await load();
    },
  });
}

function askDelete(r) {
  askConfirm({
    title: t('确认删除规则「{name}」', { name: r.name }),
    text: t('删除后无法恢复（审计里会保留删除前的内容）。只靠这条规则放行的命令将被拒绝。'),
    diff: fieldsOf(r).filter(([, v]) => v !== '').map(([k, v]) => t('{label}：{val}', { label: k, val: v })).join('\n'),
    run: async () => {
      await api('POST', `/api/v1/settings/redis-rules/${r.id}/delete`, { confirm: true });
      setMsg($('rr-msg'), t('规则「{name}」已删除。', { name: r.name }), 'info');
      await load();
    },
  });
}

// ---- 规则测试 ----
function openTest() {
  const sel = $('rr-t-inst');
  sel.replaceChildren(new Option(t('（任意实例）'), '0'));
  state.instances.forEach((i) => sel.append(new Option(i.name, String(i.id))));
  $('rr-t-table').hidden = true;
  openModal('rr-m-test');
}

$('rr-t-run').addEventListener('click', async () => {
  const db = $('rr-t-db').value.trim();
  if (!/^\d+$/.test(db)) return setMsg($('rr-msg'), t('db 编号应为 0 ~ 1023 的整数'));
  const btn = $('rr-t-run');
  btn.disabled = true;
  try {
    const out = await api('POST', '/api/v1/settings/redis-rules/test', { instance_id: Number($('rr-t-inst').value), db: Number(db), text: $('rr-t-text').value });
    const tb = $('rr-t-table').tBodies[0];
    tb.replaceChildren();
    out.forEach((r) => {
      const tr = el('tr', r.level === 2 ? 'lv-err' : r.level === 1 ? 'lv-warn' : '');
      tr.append(el('td', 'num', String(r.line)), el('td', '', r.status), el('td', 'mono', r.text), el('td', '', r.message || (r.rule_name ? t('命中规则「{name}」', { name: r.rule_name }) : '')));
      tb.append(tr);
    });
    $('rr-t-table').hidden = false;
  } catch (e) {
    setMsg($('rr-msg'), e.message);
    closeModal('rr-m-test');
  } finally {
    btn.disabled = false;
  }
});

// ---- 事件 ----
$('rr-new').addEventListener('click', () => openEdit(null));
$('rr-tpl-add').addEventListener('click', () => {
  const tpl = state.templates.find((x) => x.key === $('rr-tpl').value);
  if (!tpl) return setMsg($('rr-msg'), t('请先选择一个模板'));
  setMsg($('rr-msg'), '');
  openEdit(null, tpl);
});
$('rr-test').addEventListener('click', openTest);
$('rr-f-cmds').addEventListener('input', refreshCmdHint);

load().then(() => applyButtons(document));
