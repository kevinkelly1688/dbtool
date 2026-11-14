/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: privileges.js
 * @time: 2026/10/28
 */
// 查询权限管理：修改有效期/行数上限、回收权限、直接授予（需要 qpriv.manage / 按钮 qpriv.manage）；筛选条件改变即刷新。
import { api, confirmRun, setButtonMeta } from '../common.js';
import { t } from '../i18n.js';

const msg = document.getElementById('priv-msg');

function setMsg(text, kind = 'err', box = msg) {
  box.replaceChildren();
  if (!text) return;
  const d = document.createElement('div');
  d.className = 'alert ' + kind;
  d.setAttribute('role', 'alert');
  d.textContent = text;
  box.append(d);
}

// ---- 筛选：下拉改变即提交；换实例时清掉库（库列表随实例变化） ----
const filter = document.getElementById('priv-filter');
if (filter) {
  filter.querySelectorAll('select').forEach((sel) => {
    sel.addEventListener('change', () => {
      if (sel.name === 'inst') {
        const db = filter.querySelector('select[name="db"]');
        if (db) db.value = '';
      }
      filter.submit();
    });
  });
}

setButtonMeta({ 'qpriv.manage': t('修改 / 回收他人权限') });

// 一行权限的说明（用户、实例、库、表、有效期），修改 / 回收 / 授予确认里共用
function rowDetails(tr) {
  const c = (i) => (tr.children[i] ? tr.children[i].textContent.trim() : '');
  return [{ k: t('权限编号'), v: '#' + tr.dataset.id }, { k: t('用户'), v: c(0) }, { k: t('实例'), v: c(1) }, { k: t('类型'), v: c(2) },
    { k: t('数据库'), v: c(3) }, { k: t('表'), v: c(4) }, { k: t('行数上限'), v: c(5) }, { k: t('有效期至'), v: tr.children[6] && tr.children[6].firstChild ? tr.children[6].firstChild.textContent.trim() : '' }];
}

// ---- 回收（高危确认，失败留在弹窗里可重试）/ 修改（弹出编辑框，保存前用差异预览确认）----
const editModal = document.getElementById('edit-modal');
const ed = editModal ? { limit: document.getElementById('ed-limit'), date: document.getElementById('ed-date'), msg: document.getElementById('edit-msg'), who: document.getElementById('edit-who') } : null;
let editing = null; // { tr, id, limit, date }

function closeEdit() {
  if (!editModal) return;
  editModal.hidden = true;
  const back = editing && editing.btn;
  editing = null;
  if (back && document.contains(back)) back.focus();
}

document.querySelectorAll('tr[data-id] [data-act]').forEach((btn) => {
  btn.addEventListener('click', async () => {
    const tr = btn.closest('tr');
    const id = tr.dataset.id;
    setMsg('');
    if (btn.dataset.act === 'delete') {
      const r = await confirmRun({
        title: t('回收查询权限 #{id}', { id }), level: 'high', code: 'qpriv.manage', okText: t('确认回收'),
        lines: [t('回收后该用户将无法再查询这些库表；已过期的权限记录会保留。')],
        details: rowDetails(tr),
      }, () => api('DELETE', `/api/v1/query/privileges/${id}`));
      if (r.ok) location.reload();
      return;
    }
    if (!editModal) return;
    editing = { tr, btn, id, limit: String(btn.dataset.limit), date: btn.dataset.date };
    setMsg('', 'err', ed.msg);
    ed.who.textContent = `${tr.children[0].textContent.trim()} · ${tr.children[1].textContent.trim()} · ${tr.children[3].textContent.trim()}${tr.children[4].textContent.trim() === t('（全部）') ? '' : '.' + tr.children[4].textContent.trim()}`;
    ed.limit.value = btn.dataset.limit;
    ed.date.value = btn.dataset.date;
    editModal.hidden = false;
    ed.limit.focus();
  });
});

if (editModal) {
  const submitEdit = async () => {
    if (!editing) return;
    setMsg('', 'err', ed.msg);
    const limit = ed.limit.value.trim();
    const date = ed.date.value.trim();
    if (!/^[1-9]\d*$/.test(limit) || Number(limit) > 1000000) return setMsg(t('行数上限需要是 1 ~ 1000000 的整数'), 'err', ed.msg);
    if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) return setMsg(t('请填写有效期至（YYYY-MM-DD）'), 'err', ed.msg);
    const cur = editing;
    const r = await confirmRun({
      title: t('修改查询权限 #{id}', { id: cur.id }), code: 'qpriv.manage', okText: t('确认修改'),
      lines: [t('修改后立即生效。请确认下面的变更：')],
      details: rowDetails(cur.tr).slice(0, 5),
      rows: [{ label: t('返回行数上限'), from: cur.limit, to: limit }, { label: t('有效期至'), from: cur.date, to: date }],
    }, () => api('PATCH', `/api/v1/query/privileges/${cur.id}`, { valid_date: date, limit_num: Number(limit) }));
    if (r.noChange) return setMsg(t('没有变更'), 'info', ed.msg);
    if (r.ok) location.reload();
  };
  document.getElementById('ed-submit').addEventListener('click', submitEdit);
  document.getElementById('ed-cancel').addEventListener('click', closeEdit);
  editModal.addEventListener('mousedown', (e) => { if (e.target === editModal) closeEdit(); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !editModal.hidden) closeEdit(); });
}

// ---- 直接授予 ----
const open = document.getElementById('grant-open');
if (open) {
  const $ = (id) => document.getElementById(id);
  const modal = $('grant-modal');
  const gmsg = $('grant-msg');
  const els = {
    user: $('gm-user'), inst: $('gm-inst'), dbOne: $('gm-db-one'), db: $('gm-db'), dbsMany: $('gm-dbs-many'), dbChecks: $('gm-db-checks'),
    tablesWrap: $('gm-tables-wrap'), tableChecks: $('gm-table-checks'), date: $('gm-date'), limit: $('gm-limit'), reason: $('gm-reason'),
    submit: $('gm-submit'),
  };
  const pickedDBs = new Set();
  const pickedTables = new Set();
  let opts = null;
  let tables = [];

  const gtype = () => Number(document.querySelector('input[name="gm-type"]:checked').value);

  function fill(sel, items, text, value) {
    sel.replaceChildren();
    for (const it of items) sel.append(new Option(text(it), value(it)));
  }

  function checks(container, items, picked) {
    container.replaceChildren();
    if (!items.length) {
      const h = document.createElement('div');
      h.className = 'hint';
      h.textContent = t('没有可选项');
      container.append(h);
      return;
    }
    for (const name of items) {
      const label = document.createElement('label');
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.value = name;
      cb.checked = picked.has(name);
      cb.addEventListener('change', () => { if (cb.checked) picked.add(name); else picked.delete(name); });
      label.append(cb, document.createTextNode(name));
      container.append(label);
    }
  }

  function syncType() {
    const gt = gtype();
    els.dbOne.hidden = gt === 1;
    els.tablesWrap.hidden = gt === 1;
    els.dbsMany.hidden = gt === 2;
  }

  async function loadDBs() {
    pickedDBs.clear();
    pickedTables.clear();
    tables = [];
    els.db.replaceChildren();
    els.dbChecks.replaceChildren();
    els.tableChecks.replaceChildren();
    if (!els.inst.value) return;
    try {
      const dbs = await api('GET', `/api/v1/query/instances/${encodeURIComponent(els.inst.value)}/databases`);
      fill(els.db, dbs, (x) => x, (x) => x);
      checks(els.dbChecks, dbs, pickedDBs);
      await loadTables();
    } catch (e) {
      setMsg(e.message, 'err', gmsg);
    }
  }

  async function loadTables() {
    pickedTables.clear();
    tables = [];
    if (els.inst.value && els.db.value) {
      try {
        tables = await api('GET', `/api/v1/query/instances/${encodeURIComponent(els.inst.value)}/databases/${encodeURIComponent(els.db.value)}/tables`);
      } catch (e) {
        setMsg(e.message, 'err', gmsg);
      }
    }
    checks(els.tableChecks, tables, pickedTables);
  }

  async function showModal() {
    setMsg('', 'err', gmsg);
    modal.hidden = false;
    try {
      if (!opts) opts = await api('GET', '/api/v1/query/privileges/grant-options');
    } catch (e) {
      return setMsg(e.message, 'err', gmsg);
    }
    if (!opts.users.length) setMsg(t('没有可授予的用户'), 'err', gmsg);
    fill(els.user, opts.users, (u) => t('{display}（{user}）', { display: u.display || u.username, user: u.username }), (u) => u.username);
    fill(els.inst, opts.instances, (i) => i.name, (i) => String(i.id));
    if (!els.date.value) {
      const d = new Date();
      d.setDate(d.getDate() + 30);
      els.date.value = d.toISOString().slice(0, 10);
    }
    syncType();
    await loadDBs();
  }

  async function submit() {
    setMsg('', 'err', gmsg);
    const gt = gtype();
    const body = {
      username: els.user.value, instance_id: Number(els.inst.value), priv_type: gt, valid_date: els.date.value,
      limit_num: Number(els.limit.value), reason: els.reason.value.trim(),
    };
    if (gt === 1) {
      body.db_list = [...pickedDBs];
      if (!body.db_list.length) return setMsg(t('请至少选择一个数据库'), 'err', gmsg);
    } else {
      body.db_name = els.db.value;
      body.table_list = [...pickedTables];
      if (!body.db_name || !body.table_list.length) return setMsg(t('请选择数据库，并至少勾选一张表'), 'err', gmsg);
    }
    if ([...body.reason].length < 5) return setMsg(t('请填写授予原因（至少 5 个字）'), 'err', gmsg);
    const sep = t('、');
    const objs = gt === 1 ? body.db_list.join(sep) : `${body.db_name} / ${body.table_list.join(sep)}`;
    // 失败（如用户已有同样权限）时错误留在确认弹窗里，可以直接重试；取消不发请求
    const r = await confirmRun({
      title: t('直接授予查询权限'), level: 'high', code: 'qpriv.manage', okText: t('确认授予'),
      lines: [t('管理员直接授予不走审批，来源记为「管理员直接授予」，并写入系统审计。')],
      details: [
        { k: t('用户'), v: els.user.selectedOptions[0].textContent }, { k: t('实例'), v: els.inst.selectedOptions[0].textContent },
        { k: t('权限类型'), v: gt === 1 ? t('库权限（整个库）') : t('表权限（指定表）') }, { k: gt === 1 ? t('数据库') : t('库 / 表'), v: objs },
        { k: t('行数上限'), v: String(body.limit_num) }, { k: t('有效期至'), v: body.valid_date }, { k: t('授予原因'), v: body.reason },
      ],
    }, () => api('POST', '/api/v1/query/privileges', body));
    if (r.ok) location.href = '/query/privileges?state=all&user=' + encodeURIComponent(body.username);
  }

  open.addEventListener('click', showModal);
  $('gm-cancel').addEventListener('click', () => { modal.hidden = true; });
  modal.addEventListener('click', (e) => { if (e.target === modal) modal.hidden = true; });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') modal.hidden = true; });
  els.submit.addEventListener('click', submit);
  els.inst.addEventListener('change', loadDBs);
  els.db.addEventListener('change', loadTables);
  document.querySelectorAll('input[name="gm-type"]').forEach((r) => r.addEventListener('change', syncType));
}
