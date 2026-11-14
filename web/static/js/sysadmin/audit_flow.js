/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: audit_flow.js
 * @time: 2026/10/28
 */
// 审批流程配置：按组编辑（拖动排序、审批人预览、变更前后确认）、总览矩阵（含 KPI）与审批与通知链路。仅超级用户。
import { api, $, el, msgBox, busy, fillSelect, tabs, qs, hashParams, status, riskConfirm, confirmRun, toast } from './ui.js';
import { t, tp } from '../i18n.js';

const setMsg = msgBox($('af-msg'));
const setOk = msgBox($('af-ok'));
const els = {
  type: $('af-type'), group: $('af-group'), glist: $('af-glist'), cur: $('af-cur'), curN: $('af-cur-n'), chips: $('af-chips'), add: $('af-add'),
  levels: $('af-levels'), levelsGroup: $('af-levels-group'), save: $('af-save'), del: $('af-delete'),
};

let ov = null; // 总览数据：types / groups / auth_groups / matrix / summary
let original = []; // 当前已保存的权限组 ID 顺序
let draft = []; // 编辑中的顺序
let configured = false;
let previewSeq = 0;
let showTab = () => {};
let chartSeq = 0;
let lastFlow = null; // 最近一次审批人预览（对应当前草稿），保存前用来提示“无人可审”
let prevType = ''; // 当前编辑区对应的工单类型：切换被取消时还原下拉

const authName = (id) => (ov.auth_groups.find((g) => g.id === id) || { name: '#' + id }).name;
const sameList = (a, b) => a.length === b.length && a.every((x, i) => x === b[i]);
const textOf = (ids) => (ids.length ? ids.map(authName).join(' → ') : t('（空）'));
const typeLabel = (ty) => tp((ov.types.find((x) => x.type === Number(ty)) || { label: '#' + ty }).label);
const groupName = (id) => (ov.groups.find((g) => g.id === Number(id)) || { name: '#' + id }).name;

// 草稿与已保存的流程不同 = 有未保存的修改
const isDirty = () => !sameList(draft, original);

function updateDirty() {
  $('af-dirty').hidden = !isDirty();
}

// 有未保存的草稿时，切换资源组 / 工单类型前先确认；返回 true 才继续
async function discardOk() {
  if (!isDirty()) return true;
  const r = await riskConfirm({
    title: t('放弃未保存的修改？'), lines: [t('当前审批流程有未保存的修改，切换后这些修改会丢失。')],
    okText: t('放弃并切换'), cancelText: t('留在当前'), danger: true,
  });
  return r.ok;
}

function selection() {
  return { group_id: Number(els.group.value), workflow_type: Number(els.type.value) };
}

// ---- 左侧资源组列表（n/3 ⚠ 徽标）与工单类型标记 ----

function renderGroupList() {
  els.glist.replaceChildren();
  $('af-type-n').textContent = String(ov.types.length);
  for (const row of ov.matrix) {
    const b = el('button', Number(els.group.value) === row.group_id ? 'on' : '');
    b.type = 'button';
    b.setAttribute('role', 'option');
    const small = el('small', '', `${row.configured}/${ov.types.length}`);
    if (row.warns) small.append(el('span', 'warn-mark', ` ⚠${row.warns}`));
    b.append(el('span', '', row.group_name), small);
    b.addEventListener('click', async () => {
      if (Number(els.group.value) === row.group_id) return;
      if (!(await discardOk())) return;
      els.group.value = String(row.group_id);
      renderGroupList();
      renderTypeOptions();
      run(loadFlow);
    });
    els.glist.append(b);
  }
  if (!ov.matrix.length) els.glist.append(el('span', 'muted', t('还没有资源组')));
}

// 工单类型下拉的文字带上当前资源组的状态：✓ 已配置 / ⚠ 有级别无人可审 / ○ 未配置
function renderTypeOptions() {
  const row = ov.matrix.find((r) => String(r.group_id) === els.group.value);
  for (const opt of els.type.options) {
    const cell = row && row.cells.find((c) => String(c.type) === opt.value);
    opt.textContent = typeLabel(opt.value) + (cell ? (cell.configured ? (cell.warn ? ' ⚠' : ' ✓') : ' ○') : '');
  }
}

// ---- 编辑区 ----

function renderChips() {
  els.chips.replaceChildren();
  if (!draft.length) els.chips.append(el('span', 'hint', t('尚未选择审批权限组')));
  draft.forEach((id, i) => {
    if (i > 0) els.chips.append(el('span', 'arrow', '→'));
    const chip = el('span', 'chip');
    chip.draggable = true;
    chip.dataset.idx = String(i);
    chip.append(el('i', '', String(i + 1)), document.createTextNode(authName(id)));
    // 上移 / 下移：键盘和触屏也能调整顺序（拖动只适合鼠标）
    for (const [dir, txt, tip] of [[-1, '‹', t('前移')], [1, '›', t('后移')]]) {
      const mv = el('button', 'mv', txt);
      mv.type = 'button';
      mv.title = tip;
      mv.setAttribute('aria-label', `${tip} ${authName(id)}`);
      mv.disabled = i + dir < 0 || i + dir >= draft.length;
      mv.addEventListener('click', () => {
        [draft[i], draft[i + dir]] = [draft[i + dir], draft[i]];
        changed();
        // 重绘后把焦点放回被移动的那个芯片上同方向的按钮（到头了就放到另一个方向）
        const again = els.chips.querySelectorAll('.chip')[i + dir];
        const mvs = again ? [...again.querySelectorAll('.mv')] : [];
        const target = mvs[dir < 0 ? 0 : 1];
        const alt = mvs.find((x) => !x.disabled);
        if (target && !target.disabled) target.focus(); else if (alt) alt.focus();
      });
      chip.append(mv);
    }
    const rm = el('button', '', '×');
    rm.type = 'button';
    rm.title = t('移除');
    rm.setAttribute('aria-label', t('移除 {name}', { name: authName(id) }));
    rm.addEventListener('click', () => { draft.splice(i, 1); changed(); });
    chip.append(rm);
    chip.addEventListener('dragstart', (e) => { e.dataTransfer.setData('text/plain', String(i)); e.dataTransfer.effectAllowed = 'move'; chip.classList.add('drag'); });
    chip.addEventListener('dragend', () => chip.classList.remove('drag'));
    chip.addEventListener('dragover', (e) => { e.preventDefault(); e.dataTransfer.dropEffect = 'move'; });
    chip.addEventListener('drop', (e) => {
      e.preventDefault();
      const from = Number(e.dataTransfer.getData('text/plain'));
      if (Number.isNaN(from) || from === i) return;
      const [moved] = draft.splice(from, 1);
      draft.splice(i, 0, moved);
      changed();
    });
    els.chips.append(chip);
  });
  // 下拉：已选的不再出现
  els.add.replaceChildren(new Option(t('＋ 添加审批权限组…'), ''));
  for (const g of ov.auth_groups) if (!draft.includes(g.id)) els.add.append(new Option(g.name, String(g.id)));
  els.save.disabled = !draft.length || (configured && sameList(draft, original));
  updateDirty();
}

function approverChip(a) {
  const tags = [];
  if (a.locked) tags.push(t('账号已锁定'));
  if (!a.has_perm) tags.push(t('无审批权限'));
  const s = el('span', a.locked ? 'lock' : a.has_perm ? '' : 'nop', (a.locked ? '🔒 ' : '') + (a.display || a.username) + (tags.length ? t('（{tags}）', { tags: tags.join(t('、|sep')) }) : ''));
  s.title = a.username + (tags.length ? t('：{tags}', { tags: tags.join(t('、|sep')) }) : '');
  return s;
}

function renderLevels(flow) {
  els.levels.replaceChildren();
  els.levelsGroup.textContent = flow ? flow.group_name : t('所选');
  if (!flow || !flow.levels.length) {
    const tr = el('tr');
    const td = el('td', 'empty', draft.length ? t('无法生成预览') : t('没有审批级别'));
    td.colSpan = 4;
    tr.append(td);
    els.levels.append(tr);
    return;
  }
  for (const lv of flow.levels) {
    const tr = el('tr');
    if (lv.warning) tr.className = 'lv-warn';
    const ap = el('td');
    const box = el('div', 'approvers');
    if (!lv.approvers.length) box.append(el('span', 'muted', t('（无）')));
    for (const a of lv.approvers) box.append(approverChip(a));
    ap.append(box);
    const st = el('td');
    if (lv.warning) st.append(status(t('⚠ 无人可审'), 'rejected'), el('div', 'small muted', tp(lv.warning)));
    else st.append(status(t('正常'), 'passed'));
    tr.append(el('td', 'num', String(lv.level)), el('td', '', lv.auth_group_name), ap, st);
    els.levels.append(tr);
  }
}

async function preview() {
  const seq = ++previewSeq;
  if (!draft.length) { lastFlow = null; renderLevels(null); return; }
  try {
    const flow = await api('POST', '/api/v1/settings/audit-flow/preview', { ...selection(), auth_group_ids: draft });
    if (seq === previewSeq) { lastFlow = flow; renderLevels(flow); }
  } catch (e) {
    if (seq === previewSeq) { lastFlow = null; renderLevels(null); setMsg(e.message); }
  }
}

function changed() {
  setOk('');
  renderChips();
  preview();
}

async function loadFlow() {
  setMsg('');
  setOk('');
  const flow = await api('GET', '/api/v1/settings/audit-flow/detail' + qs(selection()));
  configured = flow.configured;
  original = flow.auth_group_ids.slice();
  draft = flow.auth_group_ids.slice();
  prevType = els.type.value;
  lastFlow = flow;
  els.cur.textContent = flow.configured ? tp(flow.text) : t('未配置');
  els.curN.textContent = flow.configured ? t('（{n} 级）', { n: flow.auth_group_ids.length }) : '';
  els.del.hidden = !flow.configured;
  renderChips();
  renderLevels(flow);
}

async function save() {
  const sel = selection();
  const before = configured ? textOf(original) : t('（未配置）');
  const after = textOf(draft);
  const gname = groupName(sel.group_id);
  const tname = typeLabel(sel.workflow_type);
  // 草稿对应的预览里有“无人可审”的级别：保存会让该级的工单卡住，在确认框里明确警告
  await preview(); // 以当前草稿重新取一次审批人预览，避免用到过期结果
  const stuck = lastFlow ? lastFlow.levels.filter((lv) => lv.warning) : [];
  const lines = [t('新提交的工单立即按新流程流转，已在审批中的工单不受影响。')];
  if (stuck.length) {
    lines.unshift(t('⚠ {n} 个级别无人可审，保存后这些级别的工单会卡住：', { n: stuck.length }) + '\n'
      + stuck.map((lv) => t('第 {level} 级 {name}：{warning}', { level: lv.level, name: lv.auth_group_name, warning: tp(lv.warning) })).join('\n'));
  }
  const r = await confirmRun({
    title: t('变更审批流程'), code: 'flow.edit', level: 'high',
    lines,
    details: [{ k: t('资源组'), v: gname }, { k: t('工单类型'), v: tname }],
    rows: [{ label: t('审批流程'), from: before, to: after }],
    okText: stuck.length ? t('仍然变更') : t('确认变更'),
  }, () => api('PUT', '/api/v1/settings/audit-flow', { ...sel, auth_group_ids: draft }));
  if (r.noChange) { toast(t('没有变更'), 'warn'); return; }
  if (!r.ok) return;
  const res = r.result;
  await busy(els.save, async () => {
    try {
      await refresh();
      setOk(res.changed ? t('已变更：{before} ⇒ {after}。已写入系统审计。', { before: tp(res.before), after: tp(res.after) }) : t('审批流程没有变化。'), 'info');
      if (res.flow && res.flow.warnings.length) setMsg(t('注意：{warnings}', { warnings: res.flow.warnings.map((w) => tp(w)).join(t('；|sep')) }), 'warn');
    } catch (e) { setMsg(e.message); }
  });
}

async function remove() {
  const sel = selection();
  const r = await confirmRun({
    title: t('删除审批流'), code: 'flow.edit', level: 'high',
    lines: [t('删除后该资源组不能再提交这类工单（提示“未配置审流”），直到重新配置。'), t('已在审批中的工单不受影响。')],
    details: [{ k: t('资源组'), v: groupName(sel.group_id) }, { k: t('工单类型'), v: typeLabel(sel.workflow_type) }, { k: t('当前流程'), v: textOf(original) }],
    okText: t('确认删除'),
  }, () => api('DELETE', '/api/v1/settings/audit-flow' + qs(sel)));
  if (!r.ok) return;
  await busy(els.del, async () => {
    try {
      await refresh();
      setOk(t('已删除该审批流，已写入系统审计。'), 'info');
    } catch (e) { setMsg(e.message); }
  });
}

// ---- 总览矩阵 ----

function kpi(label, value, sub, cls) {
  const d = el('div');
  d.append(el('b', cls || '', String(value)), el('span', '', label));
  if (sub) d.append(el('span', 'small', sub));
  return d;
}

function renderKpis() {
  const s = ov.summary || { total: 0, configured: 0, unconfigured: 0, no_approver: 0 };
  const box = $('af-kpis');
  box.replaceChildren(
    kpi(t('已配置'), s.configured, t('共 {total} 项（{groups} 个资源组 × {types} 种类型）', { total: s.total, groups: ov.groups.length, types: ov.types.length }), 'ok'),
    kpi(t('未配置'), s.unconfigured, '', s.unconfigured ? 'warn' : ''),
    kpi(t('无人可审'), s.no_approver, '', s.no_approver ? 'crit' : ''),
  );
}

function renderMatrix() {
  renderKpis();
  const head = $('af-matrix-head');
  head.replaceChildren(el('th', '', t('资源组')));
  for (const ty of ov.types) head.append(el('th', '', tp(ty.label)));
  const body = $('af-matrix-body');
  body.replaceChildren();
  if (!ov.matrix.length) {
    const tr = el('tr');
    const td = el('td', 'empty', t('还没有资源组。'));
    td.colSpan = ov.types.length + 1;
    tr.append(td);
    body.append(tr);
    return;
  }
  for (const row of ov.matrix) {
    const tr = el('tr');
    if (row.cells.some((c) => c.warn)) tr.className = 'lv-warn';
    tr.append(el('td', '', row.group_name));
    for (const c of row.cells) {
      const td = el('td', c.no_approver ? 'cell-bad' : '');
      const b = el('button', 'matrix-cell');
      b.type = 'button';
      if (!c.configured) { b.className += ' muted'; b.textContent = t('未配置'); }
      else b.textContent = c.empty ? t('（空）') : tp(c.text);
      b.addEventListener('click', async () => {
        const same = Number(els.group.value) === row.group_id && Number(els.type.value) === c.type;
        if (!same && !(await discardOk())) return;
        els.group.value = String(row.group_id);
        els.type.value = String(c.type);
        renderGroupList();
        renderTypeOptions();
        showTab('edit');
        run(loadFlow);
      });
      td.append(b);
      if (c.warn) { const w = el('div', 'small warn-text', '⚠ ' + tp(c.warn_text)); td.append(w); }
      tr.append(td);
    }
    body.append(tr);
  }
}

// ---- 审批与通知链路 ----

async function renderChart() {
  const seq = ++chartSeq;
  const box = $('af-timeline');
  const gid = Number($('af-chart-group').value);
  const wt = Number($('af-chart-type').value);
  if (!gid) { box.replaceChildren(el('li', 'muted', t('还没有资源组'))); return; }
  box.replaceChildren(el('li', 'muted', t('加载中…')));
  try {
    const [flow, rg] = await Promise.all([
      api('GET', '/api/v1/settings/audit-flow/detail' + qs({ group_id: gid, workflow_type: wt })),
      api('GET', `/api/v1/settings/resource-groups/${gid}`).catch(() => null),
    ]);
    if (seq !== chartSeq) return;
    const items = [];
    const bots = rg ? [[t('钉钉'), rg.ding_set], [t('飞书'), rg.feishu_set], ['Slack', rg.slack_set]].filter((x) => x[1]).map((x) => x[0]) : [];
    const botText = rg ? (bots.length ? t('机器人通知：{bots} 群（提交人 + 资源组群）', { bots: bots.join(' / ') }) : t('该资源组未配置机器人，只有通知渠道里的规则会发')) : '';
    const typeName = tp(flow.type_label);
    if (!flow.configured) {
      items.push({ cls: 'bad', head: t('未配置审批流 → 提交被拒绝'), sub: t('资源组「{group}」不能提交「{type}」', { group: flow.group_name, type: typeName }) });
    } else {
      items.push({ cls: 'ok', head: t('提交人提交「{type}」', { type: typeName }), sub: t('资源组 {group} · 读取审批流：{text}', { group: flow.group_name, text: tp(flow.text) }) });
      if (!flow.levels.length) items.push({ cls: 'cur', head: t('空审批流 → 直接审核通过'), sub: t('没有审批级别') });
      for (const lv of flow.levels) {
        const names = lv.approvers.filter((a) => a.has_perm && !a.locked).map((a) => a.display || a.username);
        items.push({
          cls: lv.warning ? 'bad' : 'cur',
          head: t('第 {level} 级：{name}', { level: lv.level, name: lv.auth_group_name }),
          sub: lv.warning ? '⚠ ' + tp(lv.warning) : t('机器人 @ {names}', { names: names.join(t('、|sep')) }),
        });
      }
      items.push({ cls: 'ok', head: t('全部通过 → 工单生效 / 可执行'), sub: botText });
    }
    box.replaceChildren();
    for (const it of items) {
      const li = el('li', it.cls);
      li.append(el('b', '', it.head), el('div', 'small muted', it.sub));
      box.append(li);
    }
  } catch (e) {
    if (seq === chartSeq) box.replaceChildren(el('li', 'warn-text', e.message));
  }
}

async function run(fn) { try { await fn(); } catch (e) { setMsg(e.message); } }

async function refresh() {
  ov = await api('GET', '/api/v1/settings/audit-flow');
  renderMatrix();
  renderGroupList();
  renderTypeOptions();
  await loadFlow();
}

async function init() {
  ov = await api('GET', '/api/v1/settings/audit-flow');
  fillSelect(els.type, ov.types, (ty) => tp(ty.label), (ty) => ty.type);
  fillSelect(els.group, ov.groups, (g) => g.name, (g) => g.id);
  fillSelect($('af-chart-type'), ov.types, (ty) => tp(ty.label), (ty) => ty.type);
  fillSelect($('af-chart-group'), ov.groups, (g) => g.name, (g) => g.id);
  const h = hashParams();
  if (h.g && ov.groups.some((g) => String(g.id) === h.g)) els.group.value = h.g;
  if (h.t && ov.types.some((ty) => String(ty.type) === h.t)) els.type.value = h.t;
  renderMatrix();
  renderGroupList();
  renderTypeOptions();
  if (!ov.groups.length) { setMsg(t('还没有资源组，请先到“资源组”新建。'), 'warn'); renderChips(); renderLevels(null); return; }
  await loadFlow();
}

showTab = tabs($('af-tabs'), (name) => {
  if (name !== 'chart' || !ov) return;
  // 链路页默认跟随“按组配置”里选中的资源组 / 类型
  $('af-chart-group').value = els.group.value;
  $('af-chart-type').value = els.type.value;
  run(renderChart);
});
els.type.addEventListener('change', async () => {
  if (!(await discardOk())) { els.type.value = prevType; return; }
  run(loadFlow);
});
window.addEventListener('beforeunload', (e) => { if (isDirty()) { e.preventDefault(); e.returnValue = ''; } });
els.add.addEventListener('change', () => {
  const v = Number(els.add.value);
  if (v) { draft.push(v); changed(); }
});
$('af-reset').addEventListener('click', () => { draft = original.slice(); changed(); });
$('af-clear').addEventListener('click', () => { draft = []; changed(); });
$('af-chart-group').addEventListener('change', () => run(renderChart));
$('af-chart-type').addEventListener('change', () => run(renderChart));
els.save.addEventListener('click', save);
els.del.addEventListener('click', remove);

init().catch((e) => setMsg(e.message));
