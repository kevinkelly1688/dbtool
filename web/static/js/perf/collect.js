/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: collect.js
 * @time: 2026/10/28
 */
// 采集配置：列表、新增/编辑/删除、试采集（只预览）、按实例类型批量生成默认（停用）配置。
import { api, confirmRun, setButtonMeta } from '../common.js';
import { t, tp } from '../i18n.js';
import { $, el, btn, fmtNum, setMsg, fillSelect, openModal, closeModal, wireModals, busy, applyButtons, loadButtons, can, setFormReadOnly, showReadOnlyNotice } from './util.js';

setButtonMeta({ 'perfcfg.collect': t('修改采集配置 / 试采集') });

const msg = $('pcc-msg');
let data = null; // 服务端返回：rows / instances / teams / products / collectors / jobs
let editing = null;
let ro = false; // 只有“查看配置”权限（没有 perfcfg.collect）：页面只读

// 上次采集结果是服务端写进库里的文本：“<时间> OK …” 按格式串翻译；“<时间> ERROR <原因>”只翻译原因。
function statusText(s) {
  const m = /^(\d\d-\d\d \d\d:\d\d ERROR )([\s\S]*)$/.exec(s);
  return m ? m[1] + tp(m[2]) : tp(s);
}

function renderJobs() {
  const box = $('pcc-jobs');
  box.replaceChildren();
  const j = data.jobs;
  const h = el('h2', '', t('后台任务（本进程）'));
  box.append(h);
  const chips = el('div', 'chips');
  for (const job of j.all) {
    const on = j.enabled.includes(job.value);
    chips.append(el('span', 'chip ' + (on ? 'ok' : ''), on ? t('已开启 · {label}', { label: tp(job.label) }) : t('未开启 · {label}', { label: tp(job.label) })));
  }
  box.append(chips);
  if (!j.job_run_table) box.append(el('div', 'alert warn', t('运行记录表 dbt_perf_job_run 还没有创建，周报任务不会执行。')));
}

function filtered() {
  const p = $('pcc-f-product').value;
  const e = $('pcc-f-enabled').value;
  return data.rows.filter((r) => (!p || r.product === p) && (e === '' || (e === '1') === r.enabled));
}

function renderRows() {
  const body = $('pcc-rows');
  body.replaceChildren();
  const rows = filtered();
  if (!rows.length) {
    const td = el('td', 'empty', data.rows.length ? t('没有符合条件的配置。') : t('还没有采集配置。'));
    td.colSpan = 10;
    const tr = el('tr');
    tr.append(td);
    body.append(tr);
    return;
  }
  for (const r of rows) {
    const failed = (r.last_status || '').includes('ERROR');
    const tr = el('tr', failed ? 'lv-err' : '');
    const en = el('td');
    en.append(el('span', 'st ' + (r.enabled ? 'passed' : 'aborted'), r.enabled ? t('启用|state') : t('停用|state')));
    const ops = el('td', 'row-actions');
    const edit = btn(t('编辑'), '', () => openForm(r));
    const tryIt = btn(t('试采集'), '', () => preview(r));
    const del = btn(t('删除'), 'ghost danger', () => remove(r));
    for (const b of [edit, tryIt, del]) b.dataset.btn = 'perfcfg.collect';
    ops.append(edit, document.createTextNode(' '), tryIt, document.createTextNode(' '), del);
    if (ro) ops.append(document.createTextNode(' '), btn(t('查看'), '', () => openForm(r)));
    const status = el('td', 'small' + (failed ? ' crit' : ''), statusText((r.last_status || '—').replace(/^RUNNING@.*/, t('采集中…'))));
    tr.append(el('td', '', r.instance), el('td', '', tp(r.product_label)), el('td', '', tp(r.collector_label)), el('td', 'num', fmtNum(r.min_avg_ms)), el('td', 'num', fmtNum(r.min_total_sec)),
      el('td', '', r.team || t('（取实例第一个资源组）')), en, el('td', '', r.last_collect_time_text ? r.last_collect_time_text.slice(5, 16) : '—'), status, ops);
    body.append(tr);
  }
}

function syncCustom() {
  $('mc-custom-wrap').hidden = $('mc-collector').value !== 'custom_sql';
}

function openForm(r) {
  editing = r || null;
  setMsg($('mc-msg'), '');
  $('mc-title').textContent = r ? t('编辑采集配置：{instance}', { instance: r.instance }) : t('新增采集配置');
  const used = new Set(data.rows.map((x) => x.instance_id));
  const insts = r ? data.instances.filter((i) => i.id === r.instance_id) : data.instances.filter((i) => !used.has(i.id));
  fillSelect($('mc-inst'), insts.length ? insts : data.instances, (i) => t('{name}（{type}）', { name: i.name, type: i.db_type }), (i) => i.id);
  $('mc-inst').disabled = !!r;
  fillSelect($('mc-product'), data.products, (x) => tp(x.label), (x) => x.value);
  fillSelect($('mc-collector'), data.collectors, (x) => tp(x.label), (x) => x.value);
  fillSelect($('mc-team'), data.teams, (tm) => tm.name, (tm) => tm.id, ['0', t('（取实例第一个资源组）')]);
  $('mc-product').value = r ? r.product : 'mysql';
  $('mc-collector').value = r ? r.collector : 'platform_slowlogs';
  $('mc-team').value = r && r.team_id ? String(r.team_id) : '0';
  $('mc-avg').value = r ? r.min_avg_ms : 1000;
  $('mc-total').value = r ? r.min_total_sec : 10;
  $('mc-regex').value = r ? r.exclude_regex : '(?i)^\\s*(show|set|use|commit|rollback|begin|start|xa|flush|analyze|optimize|kill)\\b';
  $('mc-dbs').value = r ? r.exclude_dbs : 'mysql,information_schema,performance_schema,sys,__internal_schema';
  $('mc-sql').value = r ? r.custom_sql : '';
  $('mc-enabled').checked = r ? r.enabled : true;
  syncCustom();
  if (ro) setFormReadOnly('m-collect', true); // 只读模式：只能看，字段全部禁用（保存 / 试采集按钮由 data-btn 置灰）
  openModal('m-collect');
}

// ---- 采集窗口 ----

function renderWindow() {
  const w = data.window;
  if (!w) {
    $('pcc-window').hidden = true;
    return;
  }
  $('pcc-win-min').value = w.minutes;
  $('pcc-win-min').disabled = ro || !w.writable;
  $('pcc-win-note').textContent = w.writable ? '' : t('当前环境不能在页面里修改。');
  if (!w.writable) $('pcc-win-save').disabled = true;
}

async function saveWindow(button) {
  const v = Number($('pcc-win-min').value);
  if (!Number.isInteger(v)) return setMsg(msg, t('请填写整数分钟'));
  await busy(button, async () => {
    try {
      await api('PUT', '/api/v1/perf/config/collect-window', { minutes: v });
      setMsg(msg, t('采集窗口已改为 {n} 分钟。', { n: v }), 'info');
      await load();
    } catch (e) {
      setMsg(msg, e.message);
    }
  });
}

function formBody() {
  return {
    instance_id: Number($('mc-inst').value), product: $('mc-product').value, collector: $('mc-collector').value, custom_sql: $('mc-sql').value,
    min_avg_ms: Number($('mc-avg').value), min_total_sec: Number($('mc-total').value), exclude_regex: $('mc-regex').value, exclude_dbs: $('mc-dbs').value,
    team_id: Number($('mc-team').value), enabled: $('mc-enabled').checked,
  };
}

async function save(button) {
  await busy(button, async () => {
    setMsg($('mc-msg'), '');
    const b = formBody();
    if (!(b.min_avg_ms > 0) || !(b.min_total_sec > 0)) { // 阈值为 0 = 所有 SQL 都建单，服务端同样拒绝
      setMsg($('mc-msg'), t('平均耗时阈值和窗口总耗时阈值都必须大于 0'));
      return;
    }
    try {
      if (editing) await api('PUT', `/api/v1/perf/config/collect/${editing.id}`, formBody());
      else await api('POST', '/api/v1/perf/config/collect', formBody());
      closeModal('m-collect');
      setMsg(msg, t('已保存'), 'info');
      await load();
    } catch (e) {
      setMsg($('mc-msg'), e.message);
    }
  });
}

async function remove(r) {
  // 失败留在弹窗里可重试；取消不发请求
  const x = await confirmRun({
    title: t('删除采集配置 · {instance}', { instance: r.instance }), level: 'high', code: 'perfcfg.collect', okText: t('确认删除'),
    lines: [t('删除后该实例不再被自动采集；已经建好的工单不受影响。')],
    details: [{ k: t('实例'), v: r.instance }, { k: t('产品'), v: r.product }, { k: t('采集器'), v: r.collector }, { k: t('状态'), v: r.enabled ? t('启用|state') : t('停用|state') }],
  }, () => api('DELETE', `/api/v1/perf/config/collect/${r.id}`));
  if (!x.ok) return;
  setMsg(msg, t('已删除'), 'info');
  try { await load(); } catch (e) { setMsg(msg, e.message); }
}

// 试采集：用表单里的配置（行上的“试采集”用已保存的配置），只预览不建单
async function preview(r) {
  const body = r ? { id: r.id, instance_id: r.instance_id, product: r.product, collector: r.collector, custom_sql: r.custom_sql, min_avg_ms: r.min_avg_ms,
    min_total_sec: r.min_total_sec, exclude_regex: r.exclude_regex, exclude_dbs: r.exclude_dbs, team_id: r.team_id || 0, enabled: r.enabled } : formBody();
  setMsg($('mp-msg'), '');
  $('mp-rows').replaceChildren();
  $('mp-sub').textContent = t('采集中…');
  openModal('m-preview');
  try {
    const res = await api('POST', '/api/v1/perf/config/collect/preview', body);
    $('mp-sub').textContent = t('窗口 {start} ~ {end}，采到 {total} 个指纹，其中 {matched} 个达到建单阈值', { start: res.window_start, end: res.window_end, total: res.total, matched: res.matched })
      + (res.total > res.rows.length ? t('（只显示总耗时最高的 {n} 条）', { n: res.rows.length }) : '');
    if (res.note) setMsg($('mp-msg'), tp(res.note), 'info');
    if (!res.rows.length) {
      const td = el('td', 'empty', t('这个窗口里没有采集到慢 SQL。'));
      td.colSpan = 7;
      const tr = el('tr');
      tr.append(td);
      $('mp-rows').append(tr);
    }
    for (const x of res.rows) {
      const tr = el('tr');
      const sql = el('td', 'mono wrap', x.sql);
      const out = el('td');
      if (x.issue_id) out.append(el('a', '', t('已有工单 #{id}', { id: x.issue_id })));
      else out.append(el('span', 'st ' + (x.would_create ? 'running' : 'aborted'), x.would_create ? t('将建单') : t('未达阈值/已排除')));
      if (x.issue_id) out.firstChild.href = '/perf/' + x.issue_id;
      tr.append(el('td', '', x.db_name || '-'), sql, el('td', 'num', fmtNum(x.exec_count)), el('td', 'num', fmtNum(x.avg_ms, 1)), el('td', 'num', fmtNum(x.max_ms, 1)), el('td', 'num', fmtNum(x.total_sec, 2)), out);
      $('mp-rows').append(tr);
    }
  } catch (e) {
    $('mp-sub').textContent = '';
    setMsg($('mp-msg'), e.message);
  }
}

async function initDefaults() {
  const x = await confirmRun({
    title: t('批量生成默认采集配置'), code: 'perfcfg.collect', okText: t('确认生成'),
    lines: [t('为还没有采集配置的实例生成默认配置，默认停用；已有配置的实例保持不变。')],
  }, () => api('POST', '/api/v1/perf/config/collect/init'));
  if (!x.ok) return;
  setMsg(msg, t('已生成 {created} 条（默认停用），{skipped} 个实例已有配置、保持不变。', { created: x.result.created, skipped: x.result.skipped }), 'info');
  try { await load(); } catch (e) { setMsg(msg, e.message); }
}

async function load() {
  data = await api('GET', '/api/v1/perf/config/collect');
  const b = await loadButtons();
  ro = !!b && !can(b, 'perfcfg.collect');
  if (ro) showReadOnlyNotice($('pcc-ro'), t('采集配置|ro'));
  renderJobs();
  renderWindow();
  renderRows();
  await applyButtons(document); // 无“修改采集配置”权限的按钮置灰
}

wireModals();
fillSelect($('pcc-f-product'), [], () => '', () => '', ['', t('产品类型：全部')]);
$('pcc-f-product').addEventListener('change', renderRows);
$('pcc-f-enabled').addEventListener('change', renderRows);
$('pcc-new').addEventListener('click', () => openForm(null));
$('pcc-init').addEventListener('click', () => initDefaults());
$('mc-ok').addEventListener('click', (e) => save(e.currentTarget));
$('pcc-win-save').addEventListener('click', (e) => saveWindow(e.currentTarget));
$('mc-preview').addEventListener('click', () => preview(null));
$('mc-collector').addEventListener('change', syncCustom);
load().then(() => {
  fillSelect($('pcc-f-product'), data.products, (x) => tp(x.label), (x) => x.value, ['', t('产品类型：全部')]);
}).catch((e) => setMsg(msg, e.message));
