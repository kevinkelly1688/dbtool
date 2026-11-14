/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: detail.js
 * @time: 2026/10/28
 */
// SQL 工单详情：审批、终止、改可执行时间、立即/定时/手工执行、取消定时、标记执行异常、回滚语句、在线改表控制与进度、执行/导出进度条、状态轮询。
import { api, confirmRun, setButtonMeta, toast } from '../common.js';
import { t, tp } from '../i18n.js';

const root = document.getElementById('detail');
const id = root.dataset.id;
const base = `/api/v1/sqlworkflow/${id}`;
const msg = document.getElementById('detail-msg');
const $ = (x) => document.getElementById(x);

function setMsg(text, kind = 'err') {
  msg.replaceChildren();
  if (!text) return;
  const d = document.createElement('div');
  d.className = 'alert ' + kind;
  d.setAttribute('role', 'alert');
  d.textContent = text;
  msg.append(d);
}

const remark = () => ($('remark') ? $('remark').value.trim() : '');

async function act(path, body, button, done) {
  setMsg('');
  const all = document.querySelectorAll('.review-box button, #run-card button');
  all.forEach((b) => (b.disabled = true));
  if (button) button.setAttribute('aria-busy', 'true');
  try {
    await api('POST', `${base}/${path}`, body);
    if (done) done();
    else location.reload();
  } catch (e) {
    setMsg(e.message);
    all.forEach((b) => (b.disabled = false));
    if (button) button.removeAttribute('aria-busy');
  }
}

function on(elId, fn) {
  const el = $(elId);
  if (el) el.addEventListener('click', () => fn(el));
}

// ---- 统一确认：页脚显示权限码；高危操作红色标记；失败留在弹窗里可重试 ----
setButtonMeta({
  'sqlworkflow.approve': t('审批通过'),
  'sqlworkflow.reject': t('驳回 / 终止他人工单'),
  'sqlworkflow.cancel_own': t('终止自己的工单'),
  'sqlworkflow.execute_group': t('立即执行（资源组内）'),
  'sqlworkflow.schedule': t('定时执行'),
  'sqlworkflow.manual_finish': t('确认手工执行结束'),
  'sqlworkflow.osc': t('在线改表控制（暂停 / 恢复 / 终止）'),
  'sqlworkflow.batch_op': t('批量实例 重试 / 关闭'),
  'sqlworkflow.cancel_run': t('取消执行（执行中的工单）'),
  'export.download': t('下载导出文件'),
});

const isExport = () => root.dataset.export === 'true';
// 环境字段：详情数据里有 data-env（prod / 生产）才要求键入工单号；服务端没给就当作未知，不要求。
const isProd = () => root.dataset.envProd === 'true' || /^(prod|production|生产)$/i.test((root.dataset.env || '').trim());
const prodTyped = () => (isProd() ? { typed: String(id), typedLabel: t('目标是生产环境，请输入工单号确认：') } : {});
const sqlText = () => ($('wf-sql') ? $('wf-sql').textContent : '');
const resultRows = () => [...document.querySelectorAll('#result-table tbody tr')].filter((tr) => !tr.classList.contains('sum') && tr.querySelector('td.mono'));
const warnCount = () => document.querySelectorAll('#result-table tbody tr.lv-warn').length;

function baseDetails() {
  const d = [{ k: t('工单号'), v: '#' + id }, { k: t('实例'), v: root.dataset.instance }, { k: t('数据库'), v: root.dataset.db }];
  if (root.dataset.batch !== 'true') d.push({ k: t('语句数'), v: String(resultRows().length) });
  return d;
}

// 弹窗确认后调接口；成功后锁住页面按钮并刷新（或执行 after），失败留在弹窗内可重试，取消则不发请求。
async function riskAct(opts, path, body, after) {
  setMsg('');
  const r = await confirmRun(opts, () => api('POST', `${base}/${path}`, body));
  if (r.ok) {
    document.querySelectorAll('.review-box button, #run-card button, #stuck-box button').forEach((b) => (b.disabled = true));
    if (after) after(r.result);
    else location.reload();
  }
  return r;
}

// 审批通过前的说明：是否末级、下一级审批（时间线上只有级别和审批组，没有下一级具体的人）、检查警告数。
function approveLines() {
  const lines = [];
  const steps = [...document.querySelectorAll('.sf-steps .sf-step')];
  const cur = steps.findIndex((s) => s.classList.contains('current'));
  if (cur >= 0) {
    const title = (s) => (s.querySelector('.sf-step-body b') || s).textContent.trim();
    const next = steps.slice(cur + 1).find((s) => s.classList.contains('pending'));
    lines.push(next
      ? t('当前是「{cur}」，不是末级。通过后进入下一级「{next}」继续审批，工单还不能执行。', { cur: title(steps[cur]), next: title(next) })
      : t('当前是「{cur}」，已是末级审批。通过后工单进入「审核通过」，之后即可执行。', { cur: title(steps[cur]) }));
  }
  const w = warnCount();
  if (w > 0) lines.push(t('检查结果里有 {n} 条警告，请确认已评估过风险。', { n: w }));
  return lines;
}

on('act-pass', () => riskAct({
  title: t('审批通过 · 工单 #{id}', { id }), code: 'sqlworkflow.approve', okText: t('确认通过'), lines: approveLines(),
  details: [{ k: t('工单号'), v: '#' + id }, { k: t('实例'), v: root.dataset.instance }, { k: t('数据库'), v: root.dataset.db }, { k: t('备注'), v: remark() || t('同意') }],
}, 'pass', { remark: remark() || '同意' })); // i18n-ignore 默认审批意见是存库的备注
on('act-cancel', () => {
  if (!remark()) return setMsg(t('请填写驳回/终止原因'));
  const review = !!$('act-pass'); // 同时能审批：这是驳回；否则是终止
  riskAct({
    title: review ? t('驳回工单 #{id}', { id }) : t('终止工单 #{id}', { id }), level: 'high', code: review ? 'sqlworkflow.reject' : 'sqlworkflow.cancel_own',
    lines: [review ? t('驳回后工单进入「人工终止流程」，提交人需修改后重新提交。') : t('终止后不能恢复，需要重新提交。已设置的定时计划会被取消。')],
    details: [{ k: t('工单号'), v: '#' + id }, { k: t('实例'), v: root.dataset.instance }, { k: t('原因'), v: remark() }],
    okText: review ? t('确认驳回') : t('确认终止'),
  }, 'cancel', { remark: remark() });
});
on('act-rundate', (b) => act('run-date', { run_date_start: $('rd-start').value, run_date_end: $('rd-end').value }, b));

const executor = () => ($('run-executor') ? $('run-executor').value : '');
// 下拉里推荐项的文字带“（默认推荐）”后缀（服务端按界面语言渲染），确认框里只要执行方式本身。
const executorLabel = () => {
  const o = $('run-executor') && $('run-executor').selectedOptions[0];
  if (!o) return '';
  const suf = t('（默认推荐）');
  const s = o.textContent;
  return (s.endsWith(suf) ? s.slice(0, s.length - suf.length) : s).trim();
};
on('act-execute', () => {
  const exp = isExport();
  const lines = exp
    ? [t('将在目标实例上执行导出查询并生成文件。导出开始后无法撤回。')]
    : [t('将在目标实例的该库上按顺序执行本工单的 SQL，失败即停，其后语句标为未执行；已成功的语句不会自动回滚。执行开始后无法撤回。')];
  const w = warnCount();
  if (w > 0) lines.push(t('检查结果里有 {n} 条警告。', { n: w }));
  const details = baseDetails();
  details.push({ k: t('执行方式'), v: executorLabel() });
  riskAct({
    title: exp ? t('立即导出工单 #{id}', { id }) : t('立即执行工单 #{id}', { id }), level: 'high', code: 'sqlworkflow.execute_group', okText: exp ? t('确认导出') : t('确认执行'),
    focusOk: true, // 点“立即执行”就是要执行：初始焦点直接在确认按钮上（生产环境要键入工单号时先到键入框）
    lines, details, sql: sqlText(), ...prodTyped(),
  }, 'execute', { mode: 'auto', executor: executor() });
});
on('act-manual', () => {
  riskAct({
    title: t('确认手工执行结束 · 工单 #{id}', { id }), level: 'high', code: 'sqlworkflow.manual_finish', okText: t('确认已执行完'),
    lines: [t('请确认你已在线下自行执行完这些 SQL。点击确认后工单标记为已结束，系统不会执行任何语句。')],
    details: baseDetails(), ...prodTyped(),
  }, 'execute', { mode: 'manual' });
});
on('act-schedule', () => {
  const at = $('run-at').value;
  if (!at) return setMsg(t('请先选择定时执行时间'));
  const exp = isExport();
  const when = at.replace('T', ' ');
  riskAct({
    title: exp ? t('定时导出工单 #{id}', { id }) : t('定时执行工单 #{id}', { id }), level: 'high', code: 'sqlworkflow.schedule', okText: t('确认设定'),
    lines: [exp
      ? t('到 {at} 由调度器自动导出（{executor}）。到点前终止工单会取消计划。', { at: when, executor: executorLabel() })
      : t('到 {at} 由调度器自动执行（{executor}）。到点前终止工单会取消计划。', { at: when, executor: executorLabel() })],
    details: baseDetails(), sql: sqlText(), ...prodTyped(),
  }, 'schedule', { run_at: at, executor: executor() });
});
// 取消定时：工单回到“审核通过”（不是终止工单）
on('act-sched-cancel', () => {
  riskAct({
    title: t('取消定时 · 工单 #{id}', { id }), code: 'sqlworkflow.schedule', okText: t('取消定时'), cancelText: t('保留'),
    lines: [t('取消后工单回到「审核通过」，之后可以重新设置定时或立即执行。')],
    details: [{ k: t('工单号'), v: '#' + id }, { k: t('实例'), v: root.dataset.instance }],
  }, 'schedule/cancel', {});
});

// 取消执行：工单执行太久时，DBA / 超级用户主动中断。原因必填；服务端只接受“本进程正在执行”的工单。
on('act-cancel-run', async () => {
  const reason = $('cancel-reason') ? $('cancel-reason').value.trim() : '';
  if (!reason) return setMsg(t('请填写取消原因'));
  const lines = [
    t('确定取消这张工单的执行吗？正在执行的语句会被中断，其后的语句不再执行。'),
    t('已经成功执行的语句不会回滚；被中断的这一条由目标库回滚（大事务回滚本身也要一段时间）。'),
    t('工单会变为「执行有异常」，不会自动重试，请到目标库核对实际执行情况。'),
  ];
  if (root.dataset.batch === 'true') lines.push(t('这是批量工单：所有正在执行的实例都会被中断。'));
  lines.push(t('如果这张工单是用 goInception 执行的：取消只会断开与 goInception 的连接，已经下发到目标库的语句可能仍在执行，请再到「会话诊断」核对并终止。'));
  await riskAct({
    title: t('取消执行 · 工单 #{id}', { id }), level: 'high', code: 'sqlworkflow.cancel_run', okText: t('确认取消执行'), lines,
    details: [{ k: t('工单号'), v: '#' + id }, { k: t('实例'), v: root.dataset.instance }, { k: t('数据库'), v: root.dataset.db }, { k: t('原因'), v: reason }],
    ...prodTyped(),
  }, 'cancel-run', { remark: reason });
});

// 标记为执行异常：进程失联（被重启 / 被强杀）后工单停在排队中 / 执行中时用；服务端会检查执行锁，不会重试执行。
on('act-mark-exception', async (b) => {
  const reason = $('stuck-reason') ? $('stuck-reason').value.trim() : '';
  if (!reason) return setMsg(t('请填写标记原因'));
  await riskAct({
    title: t('标记为执行异常 · 工单 #{id}', { id }), level: 'high', code: 'sqlworkflow.reject', okText: t('确认标记'),
    lines: [t('确定把这张工单标记为「执行有异常」吗？'), t('不会重试执行，请务必到目标库核对哪些语句已经执行。')],
    details: [{ k: t('工单号'), v: '#' + id }, { k: t('实例'), v: root.dataset.instance }, { k: t('原因'), v: reason }],
  }, 'mark-exception', { remark: reason });
});

// ---- 下载导出文件：先确认；用 fetch 取，失败（文件已清理 / 无权限 / 状态不对）时在弹窗里给出可读说明，成功再交给浏览器保存 ----
const DOWNLOAD_BLOB_MAX = 256 * 1024 * 1024; // 超过这个大小不放进内存，改走浏览器直接下载（此时服务端会多记一条下载审计）

function downloadName(res) {
  const m = /filename\*?=(?:UTF-8'')?"?([^";]+)"?/i.exec(res.headers.get('Content-Disposition') || '');
  if (m) { try { return decodeURIComponent(m[1]); } catch (_) { return m[1]; } }
  return `export_${id}.zip`;
}

async function downloadExport(url) {
  let res;
  try {
    res = await fetch(url, { credentials: 'same-origin' });
  } catch (_) {
    throw new Error(t('无法连接服务器，请检查网络后重试'));
  }
  if (!res.ok) {
    let payload = null;
    try { payload = await res.json(); } catch (_) { /* 非 JSON 响应 */ }
    const m = payload && payload.message;
    if (res.status === 404 || res.status === 410) throw new Error(m || t('文件不存在，可能已按保留策略清理；需要的话请复制为新工单重新导出。'));
    if (res.status === 403) throw new Error(m || t('你没有下载这份导出文件的权限。'));
    if (res.status === 401) throw new Error(t('登录已过期，请重新登录后再下载。'));
    throw new Error(m || t('下载失败（HTTP {status}），请稍后重试或联系管理员。', { status: res.status }));
  }
  const size = Number(res.headers.get('Content-Length'));
  if (!Number.isFinite(size) || size <= 0 || size > DOWNLOAD_BLOB_MAX) {
    if (res.body) res.body.cancel().catch(() => {});
    location.href = url;
    return;
  }
  const blob = await res.blob();
  const a = document.createElement('a');
  const obj = URL.createObjectURL(blob);
  a.href = obj;
  a.download = downloadName(res);
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(obj), 30000);
}

document.querySelectorAll(`a[href="${base}/download"]`).forEach((a) => {
  a.addEventListener('click', async (e) => {
    if (e.ctrlKey || e.metaKey || e.shiftKey || e.button !== 0) return; // 新窗口 / 另存为等保持浏览器默认
    e.preventDefault();
    const dd = a.closest('dd.mono');
    const fileName = dd ? dd.textContent.replace(a.textContent, '').trim().split(/\s+/)[0] : '';
    const r = await confirmRun({
      title: t('下载导出文件 · 工单 #{id}', { id }), level: 'high', code: 'export.download', okText: t('确认下载'),
      lines: [t('导出文件是业务数据（已按数据脱敏配置处理，未配置脱敏的字段是原值），请妥善保管、勿外传。'), t('本次下载会写入审计日志（操作「离线下载」，含工单号与文件名）。')],
      details: [{ k: t('工单号'), v: '#' + id }, { k: t('文件'), v: fileName }],
    }, () => downloadExport(a.href));
    if (r.ok) toast(t('已开始下载'));
  });
});

// ---- 回滚语句 ----
on('act-rollback', async (b) => {
  const box = $('rollback-box');
  b.setAttribute('aria-busy', 'true');
  try {
    const items = await api('GET', `${base}/rollback`);
    box.replaceChildren();
    box.hidden = false;
    if (!items.length) {
      box.textContent = t('没有可回滚的语句（可能没有产生备份）。');
      return;
    }
    items.forEach((it) => {
      const h = document.createElement('div');
      h.className = 'muted mono';
      h.textContent = t('-- 原语句：{sql}', { sql: it.sql.replace(/\s+/g, ' ') });
      const pre = document.createElement('pre');
      pre.className = 'sqlbox mono';
      pre.textContent = it.rollback;
      box.append(h, pre);
    });
  } catch (e) {
    setMsg(e.message);
  } finally {
    b.removeAttribute('aria-busy');
  }
});

// ---- 批量上线：重试 / 关闭失败的实例 ----
document.querySelectorAll('[data-batch-op]').forEach((btn) => {
  btn.addEventListener('click', async () => {
    const tr = btn.closest('tr');
    const op = btn.dataset.batchOp;
    const name = tr.children[0].textContent.trim();
    const exp = isExport();
    const retry = op === 'retry';
    const lines = retry
      ? [exp ? t('将在 {name} 上重新导出，成功后文件合并进同一个压缩包，其余实例的文件不受影响。', { name }) : t('将在 {name} 上重新执行整段 SQL，并用新结果替换它原有的结果行；重试期间工单置为「执行中」。', { name })]
      : [t('关闭后 {name} 不再计入工单成败，也不能再重试；全部实例都被关闭时工单不会被置为成功。', { name })];
    await riskAct({
      title: retry ? t('重试实例 {name}', { name }) : t('关闭实例 {name}', { name }), level: 'high', code: 'sqlworkflow.batch_op', okText: retry ? t('确认重试') : t('确认关闭'),
      lines, details: [{ k: t('工单号'), v: '#' + id }, { k: t('实例'), v: name }, { k: t('数据库'), v: root.dataset.db }],
      sql: retry ? sqlText().split('\n').slice(0, 5).join('\n') : '', ...(retry ? prodTyped() : {}),
    }, `batch/${tr.dataset.instance}/${op}`, {});
  });
});

// ---- 批量实例表“数据库版本”列：打开详情页不同步连库，页面加载后逐行异步取实例健康探测（服务端 30 秒缓存、并发有上限；失败显示“未知”）----
async function loadBatchVersions() {
  const cells = Array.from(document.querySelectorAll('#batch-card tr[data-instance] [data-batch-version]'));
  let next = 0;
  const worker = async () => {
    while (next < cells.length) {
      const td = cells[next++];
      const iid = td.closest('tr').dataset.instance;
      try {
        const h = await api('GET', `/api/v1/instances/${encodeURIComponent(iid)}/health`);
        if (h && h.version && h.status !== 'unknown' && h.status !== 'disabled') {
          td.textContent = h.version;
        } else {
          td.textContent = t('未知');
          td.classList.add('muted');
          if (h && h.message) td.title = tp(h.message);
        }
      } catch (e) {
        td.textContent = t('未知');
        td.classList.add('muted');
        td.title = e.message || '';
      }
    }
  };
  await Promise.all(Array.from({ length: Math.min(3, cells.length) }, worker));
}
loadBatchVersions();

// ---- 在线改表：进度条 + 自动轮询（原来的“进度 / 暂停 / 恢复 / 终止”按钮保留）----
function oscShow(span, rows) {
  const out = span.nextElementSibling;
  const bar = span.querySelector('.sf-progress > i');
  const track = span.querySelector('.sf-progress');
  const r = rows && rows[0];
  const pct = r ? parseFloat(String(r.PERCENT ?? '').replace('%', '')) : NaN;
  if (bar && Number.isFinite(pct)) {
    bar.style.width = Math.max(0, Math.min(100, pct)) + '%';
    if (track) track.classList.remove('indet');
  }
  out.textContent = ' ' + (rows && rows.length ? rows.map((x) => `${x.PERCENT ?? ''}% ${x.REMAINTIME ?? ''} ${x.INFOMATION ?? ''}`.trim()).join(t('；')) : t('已发送'));
}

const oscSpans = [...document.querySelectorAll('.osc')];
oscSpans.forEach((span) => {
  span.querySelectorAll('button[data-osc]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const cmd = btn.dataset.osc;
      if (cmd === 'kill') {
        const r = await confirmRun({
          title: t('终止在线改表 · 工单 #{id}', { id }), level: 'high', code: 'sqlworkflow.osc', okText: t('确认终止'),
          lines: [t('终止后 gh-ost 会清理影子表，原表不变；该语句记为失败，其后语句不再执行，工单进入「执行有异常」。')],
          details: [{ k: t('工单号'), v: '#' + id }, { k: t('实例'), v: root.dataset.instance }, { k: t('数据库'), v: root.dataset.db }],
          sql: `-- gh-ost: kill ${span.dataset.sha}`,
        }, () => api('POST', `${base}/osc`, { command: cmd, sqlsha1: span.dataset.sha }));
        if (r.ok) oscShow(span, r.result);
        return;
      }
      btn.setAttribute('aria-busy', 'true');
      try {
        oscShow(span, await api('POST', `${base}/osc`, { command: cmd, sqlsha1: span.dataset.sha }));
      } catch (e) {
        span.nextElementSibling.textContent = ' ' + e.message;
      } finally {
        btn.removeAttribute('aria-busy');
      }
    });
  });
});

// 页面打开时就开始每 3 秒取一次进度，不用逐行手点；页面隐藏时暂停；出错（如权限不足）后停止，保留手动按钮。
if (oscSpans.length && root.dataset.pending === 'true') {
  let oscTimer = null;
  const poll = async () => {
    if (document.hidden) return;
    for (const span of oscSpans) {
      try {
        oscShow(span, await api('POST', `${base}/osc`, { command: 'get', sqlsha1: span.dataset.sha }));
      } catch (_) {
        clearInterval(oscTimer);
        return;
      }
    }
  };
  oscTimer = setInterval(poll, 3000);
  poll();
}

// ---- 执行 / 导出进度条 ----
const pgCard = $('progress-card');
if (pgCard) {
  const bar = $('pg-bar');
  const track = $('pg-track');
  const exp = root.dataset.export === 'true';
  const fmt = (s) => (s >= 60 ? t('{m} 分 {s} 秒', { m: Math.floor(s / 60), s: s % 60 }) : t('{s} 秒', { s }));
  const pgPoll = async () => {
    if (document.hidden) return;
    try {
      const p = await api('GET', `${base}/progress`);
      const queued = p.status === 'workflow_queuing';
      if (p.percent >= 0) {
        track.classList.remove('indet');
        bar.style.width = p.percent + '%';
        track.setAttribute('aria-valuenow', String(p.percent));
        $('pg-text').textContent = t('{label}：已处理 {done} / {total} 个实例（{percent}%）', { label: tp(p.status_label), done: p.done, total: p.total, percent: p.percent });
      } else {
        track.classList.add('indet');
        track.removeAttribute('aria-valuenow');
        $('pg-text').textContent = queued ? t('排队中，等待开始…') : exp ? t('正在导出…') : t('正在执行…');
      }
      $('pg-elapsed').textContent = p.active && !queued ? t('已用时 {time}', { time: fmt(p.elapsed_sec) }) : '';
    } catch (_) { /* 网络抖动：下一轮再试 */ }
  };
  pgPoll();
  setInterval(pgPoll, 2000);
}

// ---- 排队/执行中：轮询状态，变化后刷新页面 ----
if (root.dataset.pending === 'true') {
  const was = root.dataset.status;
  const timer = setInterval(async () => {
    try {
      const d = await api('GET', base);
      if (d.status !== was) {
        clearInterval(timer);
        location.reload();
      }
    } catch (_) { /* 网络抖动：下一轮再试 */ }
  }, 3000);
}

// ---- 结果表：所有列超出定义的列宽就折叠成一行，点开后在同一列宽里换行 ----
// 每列的宽度在 sqlflow.css 里定义（固定列宽，SQL 列拿走其余的宽度）。单元格里的文字在列宽里放得下就原样显示；放不下（或本身带换行）
// 才折成一行（末尾省略），前面带 ▸，点 ▸ 或这一行展开，点 ▾ 折叠。一列里只要有折叠的，这一列放得下的格子也留出 ▸ 的位置（整列左对齐）。
// 有折叠的列，表头旁有 ▸ 一键展开 / 折叠整列；卡片标题旁的“展开全部 / 折叠全部”管所有列。
// 汇总行的“信息”（整句汇总）不折叠，“在线改表”列（按钮）不折叠。窗口宽度变了按新的宽度重新判断。
// 没有脚本时仍然是完整显示（渐进增强：模板 head 里的 <noscript> 把表格改回自动布局）。
(function foldResultTable() {
  const table = $('result-table');
  if (!table) return;
  const FOLD = new Set(['id', 'inst', 'stage', 'lv', 'msg', 'sql', 'aff', 'time']); // th / td 的 data-c；osc（按钮）不折叠
  const NARROW = { id: 4, lv: 7, aff: 6, time: 6 }; // 这几列窄而且列宽固定：不超过这么多个字符的内容不可能放不下，不建折叠结构
  const CARET = 26; // ▸ 按钮 20px + 间距 6px，和 sqlflow.css 的 .rf-t / .rf 一致
  // 汇总行的“保存文件: xxx.zip。”单独占一行：返回拆好的各行；没有这一句（或它在开头）就原样返回一行。
  // 文件名里有“.”，英文只在后面跟空白或结尾的“.”处断句；文案取自目录，中英文都认。
  const savedKey = t('保存文件: {name}', { name: '' });
  const savedLines = (txt) => {
    const i = txt.indexOf(savedKey);
    if (i <= 0 || !/[。.]\s*$/.test(txt.slice(0, i))) return [txt];
    const rest = txt.slice(i);
    const end = /。|\.(?=\s|$)/.exec(rest);
    const cut = end ? end.index + end[0].length : rest.length;
    return [txt.slice(0, i), rest.slice(0, cut), rest.slice(cut)].map((s) => s.trim()).filter(Boolean);
  };
  const mk = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  };

  // ---- 把每个要折叠的单元格拆成：▸ 按钮 + 一行省略的文字 + 展开后的全文（是否真的折叠由 apply 按宽度决定）----
  const cells = [];
  for (const tr of table.querySelectorAll('tbody tr')) {
    const sum = tr.classList.contains('sum');
    for (const td of tr.querySelectorAll('td[data-c]')) {
      const col = td.dataset.c;
      if (!FOLD.has(col) || (col === 'msg' && sum)) continue; // 汇总行的整句汇总不折叠
      const host = col === 'msg' ? td.querySelector('.msgcell') : td; // 信息列只动说明本身，“执行前的原值”（details）不动
      if (!host) continue;
      const extra = [...host.querySelectorAll('small')]; // 实例名后面的“已关闭”标记留在外面，折叠时也看得见
      const text = [...host.childNodes].filter((n) => !extra.includes(n)).map((n) => n.textContent).join('').trim();
      if (!text || text.length <= (NARROW[col] || 0)) continue;
      const line = text.replace(/\s+/g, ' ');
      const box = mk('div', 'rf');
      const btn = mk('button', 'rf-t', '▸');
      btn.type = 'button';
      btn.hidden = true;
      const one = mk('span', 'rf-one', line);
      const full = mk('div', 'rf-full', col === 'msg' ? savedLines(text).join('\n') : text);
      full.hidden = true;
      box.append(btn, one, full);
      let x = null;
      if (extra.length) {
        x = mk('span', 'rf-x');
        x.append(' ', ...extra);
        box.append(x);
      }
      host.replaceChildren(box);
      const c = { col, sum, host, box, btn, one, full, x, text, line, multi: text.includes('\n'), open: false, over: false, tw: 0, base: 0 };
      btn.addEventListener('click', () => { c.open = !c.open; apply(); });
      one.addEventListener('click', () => { if (c.over) { c.open = true; apply(); } });
      cells.push(c);
    }
  }
  // 汇总行的“信息”不折叠：整句显示，“保存文件”另起一行
  for (const box of table.querySelectorAll('tr.sum .msgcell')) {
    const lines = savedLines(box.textContent);
    if (lines.length < 2) continue;
    box.replaceChildren(...lines.map((s) => mk('div', '', s)));
  }

  // ---- 表头旁的 ▸（这一列有折叠的才出现）和卡片标题旁的“展开全部 / 折叠全部” ----
  const heads = new Map();
  for (const th of table.querySelectorAll('thead th[data-c]')) {
    const col = th.dataset.c;
    if (!FOLD.has(col)) continue;
    const b = mk('button', 'rf-th', '▸');
    b.type = 'button';
    b.hidden = true;
    b.addEventListener('click', () => setAll(cells.filter((c) => c.col === col && c.over)));
    th.append(b);
    heads.set(col, b);
  }
  const h2 = table.closest('section') && table.closest('section').querySelector('h2');
  const allBtn = mk('button', 'btn', t('展开全部'));
  allBtn.type = 'button';
  allBtn.id = 'result-fold-all';
  allBtn.hidden = true;
  allBtn.addEventListener('click', () => setAll(cells.filter((c) => c.over)));
  if (h2 && cells.length) h2.append(allBtn);
  // 有没展开的 → 全部展开；全都展开了 → 全部折叠
  function setAll(list) {
    const open = list.some((c) => !c.open);
    list.forEach((c) => { c.open = open; });
    apply();
  }

  // ---- 量一行文字有多宽：用一个看不见的量尺，不动页面上的元素（按钮的键盘焦点也就不会丢）；同样的字体 + 文字只量一次 ----
  const ruler = mk('span', 'rf-ruler');
  ruler.setAttribute('aria-hidden', 'true');
  document.body.append(ruler);
  let fonts = new Map();
  function widthOf(c) {
    const fk = c.col + (c.sum ? '#' : '');
    let f = fonts.get(fk);
    if (!f) {
      const cs = getComputedStyle(c.host);
      f = { family: cs.fontFamily, size: cs.fontSize, weight: cs.fontWeight, spacing: cs.letterSpacing, widths: new Map() };
      fonts.set(fk, f);
    }
    let w = f.widths.get(c.line);
    if (w == null) {
      ruler.style.fontFamily = f.family;
      ruler.style.fontSize = f.size;
      ruler.style.fontWeight = f.weight;
      ruler.style.letterSpacing = f.spacing;
      ruler.textContent = c.line;
      w = ruler.getBoundingClientRect().width;
      f.widths.set(c.line, w);
    }
    return w;
  }

  // ---- 按现在的列宽逐格判断：放得下就是普通文字；放不下（或本身带换行）才出现 ▸，并按 open 决定折叠还是展开 ----
  function apply() {
    for (const c of cells) {
      c.tw = widthOf(c);
      c.base = c.box.clientWidth - (c.x ? c.x.offsetWidth + 6 : 0); // 这一格能用的全部宽度
    }
    const has = {};
    for (const c of cells) if (c.multi || c.tw > c.base + 1) has[c.col] = true;
    for (const c of cells) {
      const pad = !!has[c.col]; // 这一列有折叠的：放得下的格子也留出 ▸ 的位置
      c.over = c.multi || c.tw > c.base - (pad ? CARET : 0) + 1;
      c.box.classList.toggle('over', c.over);
      c.box.classList.toggle('pad', pad && !c.over);
      c.one.removeAttribute('title');
      if (!c.over) {
        c.btn.hidden = true;
        c.one.hidden = false;
        c.full.hidden = true;
        continue;
      }
      c.btn.hidden = false;
      c.btn.textContent = c.open ? '▾' : '▸';
      c.btn.title = c.open ? t('折叠|act') : t('展开全文');
      c.btn.setAttribute('aria-expanded', String(c.open));
      c.one.hidden = c.open;
      c.full.hidden = !c.open;
      if (!c.open) c.one.title = c.text; // 鼠标放上去直接看全文
    }
    for (const [col, b] of heads) {
      const list = cells.filter((c) => c.col === col && c.over);
      const all = list.length > 0 && list.every((c) => c.open);
      b.hidden = !list.length;
      b.textContent = all ? '▾' : '▸';
      b.title = all ? t('折叠这一列') : t('展开这一列');
    }
    const folded = cells.filter((c) => c.over);
    allBtn.hidden = !folded.length;
    allBtn.textContent = folded.length > 0 && folded.every((c) => c.open) ? t('折叠全部') : t('展开全部');
  }

  if (!cells.length) return;
  apply();
  // 窗口宽度变了（SQL 列拿走其余的宽度）、字体加载完了，哪些放得下、哪些放不下也跟着变
  let raf = 0;
  const recheck = () => { cancelAnimationFrame(raf); raf = requestAnimationFrame(apply); };
  window.addEventListener('resize', recheck);
  if (window.ResizeObserver) new ResizeObserver(recheck).observe(table);
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(() => { fonts = new Map(); apply(); });
})();
