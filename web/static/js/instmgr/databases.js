/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: databases.js
 * @time: 2026/10/28
 */
// 数据库管理：库清单 + 负责人/备注录入、创建数据库（二次确认）。
import { api, $, el, btn, setMsg, qs, emptyRow, cell, fmtNum, fmtBytes, openModal, closeModal, confirmBox, loadInstanceSelect, loadCaps, can, busy } from './ui.js';
import { t, tp } from '../i18n.js';

const msg = $('db-msg');
let choices = [];
let opts = null; // 创建对话框的选项：字符集、排序规则、负责人
let editing = null;
let caps = {};

const inst = () => Number($('db-inst').value);
const cur = () => choices.find((c) => c.id === inst());

async function load() {
  setMsg(msg, '');
  const c = cur();
  if (!c) { emptyRow($('db-rows'), 8, t('没有可用的实例')); return; }
  if (!c.supported) { emptyRow($('db-rows'), 8, '—'); return setMsg(msg, t('暂不支持 {type} 类型实例的数据库管理', { type: tp(c.db_type_label) }), 'warn'); }
  emptyRow($('db-rows'), 8, t('加载中…'));
  try {
    const rows = await api('GET', `/api/v1/instances/${c.id}/databases` + qs({ saved: $('db-saved').value, q: $('db-q').value.trim() }));
    render(rows);
  } catch (e) { emptyRow($('db-rows'), 8, t('加载失败')); setMsg(msg, e.message); }
}

function render(rows) {
  const tb = $('db-rows');
  if (!rows.length) return emptyRow(tb, 8, t('没有符合条件的数据库'));
  tb.replaceChildren();
  for (const r of rows) {
    const tr = el('tr');
    const gr = el('td', 'mono wrap');
    gr.textContent = r.grantees.length ? r.grantees.join('\n') : '—';
    const owner = r.saved ? cell(r.owner_display || r.owner) : cell(t('未录入'), 'muted');
    const ops = el('td', 'row-actions');
    if (can(caps, 'database.edit')) ops.append(btn(r.saved ? t('编辑') : t('录入'), '', () => openEdit(r)));
    const size = r.stats_ok ? cell(t('{n} 张 · {size}', { n: fmtNum(r.tables), size: fmtBytes(r.size_mb * 1024 * 1024) }), 'nw') : cell('—', 'muted');
    tr.append(cell(r.db_name, 'mono'), cell(r.charset), cell(r.collation), size, gr, owner, cell(r.remark), ops);
    tb.append(tr);
  }
}

function ownerOptions(select, selected) {
  select.replaceChildren(new Option(t('请选择数据库负责人'), ''), ...opts.owners.map((u) => new Option(t('{name}（{user}）', { name: u.display || u.username, user: u.username }), u.username)));
  select.value = selected || '';
}

async function ensureOpts(ownersOnly) {
  opts = await api('GET', `/api/v1/instances/${inst()}/databases/options` + (ownersOnly ? '?owners_only=1' : ''));
}

// ---- 创建 ----
function fillCollations() {
  const cs = opts.charsets.find((c) => c.name === $('c-charset').value);
  const sel = $('c-collation');
  sel.replaceChildren(new Option(t('默认'), ''));
  if (cs) for (const co of cs.collations) sel.append(new Option(co === cs.default_collation ? t('{name}（默认）', { name: co }) : co, co));
}

async function openCreate() {
  const c = cur();
  if (!c || !c.supported) return setMsg(msg, t('请先选择一个支持的实例'));
  setMsg($('db-create-msg'), '');
  try { await ensureOpts(false); } catch (e) { return setMsg(msg, e.message); }
  $('c-name').value = '';
  $('c-remark').value = '';
  ownerOptions($('c-owner'), '');
  const editable = opts.charset_editable;
  $('c-cs-box').hidden = $('c-co-box').hidden = !editable;
  $('c-hint').textContent = editable ? '' : t('{type} 实例不支持指定字符集和排序规则', { type: tp(c.db_type_label) });
  const cs = $('c-charset');
  cs.replaceChildren(new Option(t('实例默认'), ''), ...opts.charsets.map((x) => new Option(x.name, x.name)));
  cs.value = '';
  fillCollations();
  openModal('db-m-create');
}

async function submitCreate() {
  const body = { db_name: $('c-name').value.trim(), charset: $('c-charset').value, collation: $('c-collation').value, owner: $('c-owner').value, remark: $('c-remark').value };
  if (!body.db_name) return setMsg($('db-create-msg'), t('请填写数据库名称'));
  if (!body.owner) return setMsg($('db-create-msg'), t('请选择负责人'));
  const c = cur();
  let stmt = 'CREATE DATABASE `' + body.db_name + '`';
  if (body.charset) stmt += ' DEFAULT CHARACTER SET ' + body.charset;
  if (body.collation) stmt += ' COLLATE ' + body.collation;
  const ok = await confirmBox({ title: t('在 {name} 上创建数据库？', { name: c.instance_name }), lines: [t('数据库 {db}，负责人 {owner}。创建会写入操作日志。', { db: body.db_name, owner: body.owner })], sql: [stmt + ';'], okText: t('确定创建'), danger: false });
  if (!ok) return;
  try {
    await busy($('c-submit'), () => api('POST', `/api/v1/instances/${c.id}/databases`, { ...body, confirm: true }));
    closeModal('db-m-create');
    await load();
    setMsg(msg, t('已创建数据库 {db}', { db: body.db_name }), 'info');
  } catch (e) { setMsg($('db-create-msg'), e.message); }
}

// ---- 编辑 / 录入 ----
async function openEdit(r) {
  editing = r;
  setMsg($('db-edit-msg'), '');
  try { await ensureOpts(true); } catch (e) { return setMsg(msg, e.message); }
  $('e-name').value = r.db_name;
  ownerOptions($('e-owner'), r.owner);
  $('e-remark').value = r.remark || '';
  openModal('db-m-edit');
}

async function submitEdit() {
  if (!$('e-owner').value) return setMsg($('db-edit-msg'), t('请选择负责人'));
  try {
    await busy($('e-submit'), () => api('PUT', `/api/v1/instances/${inst()}/databases/info`, { db_name: editing.db_name, owner: $('e-owner').value, remark: $('e-remark').value }));
    closeModal('db-m-edit');
    await load();
    setMsg(msg, t('已保存'), 'info');
  } catch (e) { setMsg($('db-edit-msg'), e.message); }
}

$('db-filter').addEventListener('submit', (e) => { e.preventDefault(); load(); });
$('db-inst').addEventListener('change', load);
$('db-saved').addEventListener('change', load);
$('db-create').addEventListener('click', openCreate);
$('c-charset').addEventListener('change', fillCollations);
$('c-submit').addEventListener('click', submitCreate);
$('e-submit').addEventListener('click', submitEdit);

(async () => {
  try {
    caps = await loadCaps();
    $('db-create').hidden = !can(caps, 'database.create');
    choices = await loadInstanceSelect($('db-inst'), 'databases');
    await load();
  } catch (e) { setMsg(msg, e.message); }
})();
