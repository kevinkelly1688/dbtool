/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: main.js
 * @time: 2026/10/28
 */
// 环境管理页（仅超级用户）：环境字典的列表、新增、修改、删除（都要二次确认）。
// 所有往 DOM 写的内容都用 textContent；真正的校验与授权在服务端。
import { api, riskConfirm } from '../common.js';
import { applyButtons } from '../btnperm.js';
import { t, tp } from '../i18n.js';

const $ = (id) => document.getElementById(id);

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function setMsg(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

let items = [];
let editing = null; // 正在修改的环境编码；null 表示新增
let canEdit = true;

async function load() {
  try {
    const d = await api('GET', '/api/v1/settings/environments');
    items = d.items || [];
    $('env-missing').hidden = d.table_ready !== false;
    render();
  } catch (e) {
    setMsg($('env-msg'), tp(e.message));
  }
}

function btn(text, cls, fn) {
  const b = el('button', 'btn ' + cls, text);
  b.type = 'button';
  b.addEventListener('click', fn);
  return b;
}

function render() {
  const tb = $('env-table').tBodies[0];
  tb.replaceChildren();
  for (const it of items) {
    const tr = el('tr');
    const code = el('td', 'env-code', it.code);
    if (it.builtin) code.append(el('small', 'muted', t('内置')));
    tr.append(code, el('td', '', tp(it.name)));
    const prod = el('td');
    if (it.is_prod) prod.append(el('span', 'badge prod', t('是')));
    else prod.append(el('span', 'muted', '—'));
    tr.append(prod, el('td', 'num', String(it.sort)), el('td', 'num', String(it.instances)), el('td', 'num', String(it.rules)),
      el('td', 'muted small', it.updated_at ? `${it.updated_at} ${it.updated_by || ''}`.trim() : '—'));
    const ops = el('td', 'env-ops');
    if (canEdit) {
      ops.append(btn(t('编辑'), '', () => openEdit(it)), ' ');
      if (!it.builtin) {
        const del = btn(t('删除'), 'ghost danger', () => remove(it));
        if (it.instances > 0 || it.rules > 0) {
          del.disabled = true;
          del.title = t('仍有实例或防火墙规则在使用，不能删除');
        }
        ops.append(del);
      }
    }
    tr.append(ops);
    tb.append(tr);
  }
}

function openEdit(it) {
  editing = it ? it.code : null;
  $('env-m-title').textContent = it ? t('编辑环境') : t('新增环境');
  $('env-f-code').value = it ? it.code : '';
  $('env-f-code').disabled = !!it;
  $('env-f-name').value = it ? it.name : '';
  $('env-f-prod').checked = it ? !!it.is_prod : false;
  $('env-f-prod').disabled = !!(it && it.builtin);
  $('env-f-sort').value = it ? it.sort : 100;
  setMsg($('env-m-msg'), '');
  $('env-m').hidden = false;
  (it ? $('env-f-name') : $('env-f-code')).focus();
}

function closeEdit() { $('env-m').hidden = true; }

async function save() {
  const body = {
    code: $('env-f-code').value.trim(),
    name: $('env-f-name').value.trim(),
    is_prod: $('env-f-prod').checked,
    sort: Number($('env-f-sort').value) || 0,
    confirm: true,
  };
  if (!body.name) return setMsg($('env-m-msg'), t('请填写显示名称'));
  if (editing === null && !body.code) return setMsg($('env-m-msg'), t('请填写编码'));
  const r = await riskConfirm({
    title: editing === null ? t('新增环境？') : t('保存环境「{name}」的修改？', { name: body.name }),
    details: [{ k: t('编码'), v: editing === null ? body.code : editing }, { k: t('显示名称'), v: body.name }, { k: t('按生产对待'), v: body.is_prod ? t('是') : t('否') }],
    lines: body.is_prod ? [t('按生产对待的环境，实例会显示红色徽标，工单详情的生产确认也会生效。')] : [],
    okText: t('确定保存'),
  });
  if (!r.ok) return;
  try {
    if (editing === null) await api('POST', '/api/v1/settings/environments', body);
    else await api('PUT', '/api/v1/settings/environments/' + encodeURIComponent(editing), body);
    closeEdit();
    setMsg($('env-msg'), t('已保存'), 'info');
    await load();
  } catch (e) {
    setMsg($('env-m-msg'), tp(e.message));
  }
}

async function remove(it) {
  const r = await riskConfirm({
    title: t('删除环境「{name}」？', { name: it.name }),
    lines: [t('只有没有实例使用、也没有防火墙规则引用的环境才能删除。')],
    level: 'high',
    typed: it.code,
    okText: t('确定删除'),
  });
  if (!r.ok) return;
  try {
    await api('POST', '/api/v1/settings/environments/' + encodeURIComponent(it.code) + '/delete', { confirm: true });
    setMsg($('env-msg'), t('已删除'), 'info');
    await load();
  } catch (e) {
    setMsg($('env-msg'), tp(e.message));
  }
}

$('env-new').addEventListener('click', () => openEdit(null));
$('env-m-cancel').addEventListener('click', closeEdit);
$('env-m-ok').addEventListener('click', save);
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !$('env-m').hidden) closeEdit(); });

await applyButtons(document);
await load();
