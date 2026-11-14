/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: policy.js
 * @time: 2026/10/28
 */
// 安全策略页签：查看全局硬约束；仅超级用户（archive.policy_edit，锁定码）可改，保存前先看差异与风险并二次确认。
import { applyButtons } from '../btnperm.js';
import { t, tp, isZh } from '../i18n.js';
import { A, issuesOf } from './api.js';
import { $, el, nf, pbutton, fmtTime, withNodes } from './util.js';
import { flash, notice, tag, openModal, modalMsg, kvList, bold, code, h4 } from './ui.js';
import { S, limits } from './consts.js';

const root = () => $('ar-view-policy');
let pol = null;
let built = false;

// [key, 标题, 单位]。key 带点表示在 defaults 里。
const META = [
  ['min_retention_days', t('最小保留天数'), t('天')],
  ['defaults.batch_size', t('默认批量'), t('行|unit')],
  ['defaults.sleep_ms', t('默认批间休眠'), 'ms'],
  ['defaults.max_threads_running', t('Threads_running 阈值'), ''],
  ['defaults.max_lag_seconds', t('复制延迟阈值'), t('秒')],
  ['defaults.min_disk_free_pct', t('磁盘剩余阈值'), '%'],
  ['defaults.max_wait_seconds', t('负载最长等待'), t('分钟')],
  ['defaults.target_batch_ms', t('批耗时目标'), 'ms'],
  ['defaults.max_consecutive_failures', t('连续失败暂停'), t('批|unit')],
  ['max_rows_per_run', t('单次运行最大行数'), t('行|unit')],
  ['max_concurrent_per_instance', t('每实例最大并发运行'), t('个|unit')],
  ['protected_patterns', t('受保护库表（永不归档）'), ''],
  ['allow_purge_no_backup', t('允许不备份清理'), ''],
];

const getv = (p, key) => key.split('.').reduce((a, k) => (a ? a[k] : undefined), p);

function show1(key, v) {
  if (key === 'allow_purge_no_backup') return v ? tag(t('允许（需填写理由并走审批）'), 'warn') : tag(t('不允许'), 'ok');
  if (key === 'protected_patterns') {
    const box = el('span');
    (v || []).forEach((x) => { box.append(code(x), ' '); });
    if (!(v || []).length) box.append(el('span', 'muted', t('（空）')));
    return box;
  }
  if (key === 'defaults.max_wait_seconds') return nf(Math.round(v / 60));
  return nf(v);
}

function build() {
  if (built) return;
  built = true;
  const r = root();
  const card = el('section', 'card');
  card.id = 'ar-p-card';
  r.append(card);
}

function draw() {
  const card = $('ar-p-card');
  card.replaceChildren();
  const h2 = el('h2', '', t('当前策略') + ' ');
  const sub = el('small', 'muted', pol.is_default ? t('尚未修改过（使用默认值）') : t('最后修改 {time} · {by}', { time: fmtTime(pol.updated_at, 16), by: pol.updated_by || '' }));
  h2.append(sub, el('span', 'spacer'), pbutton('archive.policy_edit', t('编辑策略'), 'primary', openEdit));
  card.append(h2);
  card.append(kvList(META.map(([k, title, u]) => [title, [show1(k, getv(pol, k)), u && !Array.isArray(getv(pol, k)) && typeof getv(pol, k) !== 'boolean' ? ' ' + u : '']]), true));
  applyButtons(root());
}

export async function load() {
  try {
    pol = await A.policy();
    build();
    draw();
  } catch (e) {
    if (e.code !== 'not_ready') flash(e.message, 'err');
  }
}

export function show() {
  build();
  root().hidden = false;
  load();
}
export function hide() {
  root().hidden = true;
}
export const refresh = () => load();

// ---- 编辑 ----
function field(label, input) {
  const l = el('label', 'fld');
  l.append(document.createTextNode(label), input);
  return l;
}
function num(val, min, max) {
  const i = el('input');
  i.type = 'number';
  i.step = '1';
  i.min = String(min);
  i.max = String(max);
  i.value = String(val);
  return i;
}

function openEdit() {
  if (!pol) return;
  const lim = limits();
  const R = (k, d) => lim[k] || d;
  const d = pol.defaults || {};
  const f = {
    min_retention_days: num(pol.min_retention_days, 1, R('retention_max_days', 3650)),
    batch_size: num(d.batch_size, ...R('batch_size', [100, 10000])),
    sleep_ms: num(d.sleep_ms, R('sleep_ms_min_when_delete', 50), (R('sleep_ms', [0, 5000]))[1]),
    max_threads_running: num(d.max_threads_running, ...R('max_threads_running', [4, 1000])),
    max_lag_seconds: num(d.max_lag_seconds, ...R('max_lag_seconds', [1, 3600])),
    min_disk_free_pct: num(d.min_disk_free_pct, ...R('min_disk_free_pct', [5, 50])),
    max_wait_minutes: num(Math.round((d.max_wait_seconds || 600) / 60), Math.ceil(R('max_wait_seconds', [60, 3600])[0] / 60), Math.floor(R('max_wait_seconds', [60, 3600])[1] / 60)),
    target_batch_ms: num(d.target_batch_ms, ...R('target_batch_ms', [100, 5000])),
    max_consecutive_failures: num(d.max_consecutive_failures, ...R('max_consecutive_failures', [1, 10])),
    max_rows_per_run: num(pol.max_rows_per_run, ...R('policy_max_rows', [1000, 2000000000])),
    max_concurrent_per_instance: num(pol.max_concurrent_per_instance, 1, 10),
    protected: el('textarea'),
    allow: el('input'),
  };
  f.protected.rows = 3;
  f.protected.value = (pol.protected_patterns || []).join('\n');
  f.allow.type = 'checkbox';
  f.allow.checked = !!pol.allow_purge_no_backup;

  const body = el('div', 'ar-modal-form grid');
  body.append(
    field(t('最小保留天数（天）'), f.min_retention_days),
    field(t('默认批量（行）'), f.batch_size), field(t('默认批间休眠（ms）'), f.sleep_ms),
    field(t('Threads_running 阈值'), f.max_threads_running), field(t('复制延迟阈值（秒）'), f.max_lag_seconds), field(t('磁盘剩余阈值（%）'), f.min_disk_free_pct),
    field(t('负载最长等待（分钟）'), f.max_wait_minutes), field(t('批耗时目标（ms）'), f.target_batch_ms), field(t('连续失败暂停（批）'), f.max_consecutive_failures),
    field(t('单次运行最大行数'), f.max_rows_per_run), field(t('每实例最大并发运行'), f.max_concurrent_per_instance),
    field(t('受保护库表（每行一个：库名 或 库.表）'), f.protected),
  );
  body.lastChild.classList.add('full');
  const lab = el('label', 'chk');
  lab.style.margin = '8px 0';
  lab.classList.add('full');
  lab.append(f.allow, document.createTextNode(' ' + t('允许“仅清理”模式（不备份直接删除）')));
  body.append(lab);

  openModal({
    title: t('编辑安全策略'), body, size: 'wide', ok: t('下一步：查看差异'),
    onOk: async () => {
      const g = (i) => Number(i.value);
      const next = {
        min_retention_days: g(f.min_retention_days),
        allow_purge_no_backup: f.allow.checked,
        protected_patterns: f.protected.value.split(/[\s,，;；]+/).map((x) => x.trim()).filter(Boolean),
        max_rows_per_run: g(f.max_rows_per_run),
        max_concurrent_per_instance: g(f.max_concurrent_per_instance),
        defaults: {
          batch_size: g(f.batch_size), sleep_ms: g(f.sleep_ms), max_threads_running: g(f.max_threads_running), max_lag_seconds: g(f.max_lag_seconds),
          min_disk_free_pct: g(f.min_disk_free_pct), target_batch_ms: g(f.target_batch_ms), max_wait_seconds: g(f.max_wait_minutes) * 60,
          max_consecutive_failures: g(f.max_consecutive_failures),
        },
      };
      for (const i of Object.values(f)) {
        if (i.type === 'number' && (!Number.isInteger(Number(i.value)) || Number(i.value) < Number(i.min) || Number(i.value) > Number(i.max))) {
          modalMsg(t('需要 {min}–{max} 之间的整数（当前输入：{value}）', { min: nf(i.min), max: nf(i.max), value: i.value }));
          i.focus();
          return false;
        }
      }
      let diff;
      try {
        diff = await A.policyDiff(next);
      } catch (e) {
        const is = issuesOf(e);
        modalMsg(is && is.length ? is.map((x) => tp(x.msg)).join(isZh ? '；' : '; ') : e.message);
        return false;
      }
      if (diff.errors && diff.errors.length) {
        modalMsg(diff.errors.map((x) => tp(x.msg)).join(isZh ? '；' : '; '));
        return false;
      }
      if (!diff.changes || !diff.changes.length) {
        modalMsg(t('策略没有变化'), 'info');
        return false;
      }
      // 先关掉表单，再打开差异确认（openModal 会重置弹窗内容）
      setTimeout(() => confirmDiff(next, diff), 0);
      return true;
    },
  });
}

function confirmDiff(next, diff) {
  const body = el('div');
  body.append(notice(withNodes(t('以下 {n} 项将被修改，{now}并写入系统审计。', { n: diff.changes.length }), { now: bold(t('立即生效')) }), 'bad', '⚠'));
  const tbl = el('table', 'list');
  const th = el('thead');
  const hr = el('tr');
  [t('项目'), t('原值'), t('新值')].forEach((x) => hr.append(el('th', '', x)));
  th.append(hr);
  const tb = el('tbody');
  for (const c of diff.changes) {
    const tr = el('tr');
    const label = el('td');
    label.append(tp(c.label) || c.field);
    if (c.risk) label.append(' ', tag(t('放宽'), 'warn'));
    tr.append(label, el('td', '', tp(c.from)), el('td', '', tp(c.to)));
    tb.append(tr);
  }
  tbl.append(th, tb);
  body.append(tbl);
  if ((diff.risks || []).length) {
    body.append(h4(t('风险提示')));
    const ul = el('ul', 'plain');
    diff.risks.forEach((x) => ul.append(el('li', '', tp(x))));
    body.append(ul);
  }
  openModal({
    title: t('⚠ 确认修改安全策略'), body, size: 'wide', ok: t('确认修改'), okCls: 'danger', crit: true,
    onOk: async () => {
      try {
        pol = await A.savePolicy(next);
      } catch (e) {
        const is = issuesOf(e);
        modalMsg(is && is.length ? is.map((x) => tp(x.msg)).join(isZh ? '；' : '; ') : e.message);
        return false;
      }
      S.meta = S.meta ? { ...S.meta, policy: { ...S.meta.policy, min_retention_days: pol.min_retention_days, allow_purge_no_backup: pol.allow_purge_no_backup, max_rows_per_run: pol.max_rows_per_run, protected_patterns: pol.protected_patterns, defaults: pol.defaults } } : S.meta;
      flash(t('安全策略已更新'), 'ok');
      draw();
    },
  });
}
