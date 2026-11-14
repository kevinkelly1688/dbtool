/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: wizard_model.js
 * @time: 2026/10/28
 */
// 向导的数据模型：工作状态 <-> TaskInput / TaskView 的转换、每一步的前端校验（服务端校验为准，这里只做即时提示）。
import { t, isZh, fmtNum } from '../i18n.js';
import { clone } from './util.js';
import { limits, policy } from './consts.js';
import { checkSchedule } from './sched.js';

const num = (v) => (v === '' || v === null || v === undefined ? NaN : Number(v));

export const STEPS = [t('来源'), t('条件'), t('模式与备份'), t('稳定性参数'), t('调度'), t('试运行并提交')];

// ---- 时间列与时间单位 ----
// time_unit：空 = date / datetime / timestamp 列；epoch_s / epoch_ms = int / bigint 里的 10 位秒 / 13 位毫秒 Unix 时间戳。
export const TIME_UNITS = [['epoch_s', t('10 位（Unix 秒）')], ['epoch_ms', t('13 位（Unix 毫秒）')]];
const UNIT_DIGITS = { epoch_s: 10, epoch_ms: 13 };
// “像时间戳”的取值区间：1980-01-01 至 2100-01-01（UTC）。与服务端口径一致，用来识别选错单位 / 选错列。
const SEC_MIN = 315532800;
const SEC_MAX = 4102444800;
const TIME_NAME = /(time|date|_at$|^at$|create|update|ctime|mtime|stamp|epoch|^ts$|_ts$|expire)/i;

export const colOf = (cols, name) => ((cols && cols.columns) || []).find((c) => c.name === name) || null;
export const isIntTime = (c) => !!c && !!c.is_int && !c.is_time;

// 可作时间列的候选：dt = date / datetime / timestamp 列；hinted = 名字像时间的 int / bigint 列；rest = 其余 int / bigint 列。
// all 的顺序即下拉的顺序（日期时间列在前，名字像时间的整型列其次）。
export function timeCandidates(cols) {
  const all = (cols && cols.columns) || [];
  const dt = all.filter((c) => c.is_time);
  const ints = all.filter(isIntTime);
  const hinted = ints.filter((c) => TIME_NAME.test(c.name));
  const rest = ints.filter((c) => !TIME_NAME.test(c.name));
  return { dt, hinted, rest, ints: [...hinted, ...rest], all: [...dt, ...hinted, ...rest] };
}

// 向导里默认选哪一列：日期时间列优先（created_at > 有索引 > 第一个）；没有就取名字像时间的整型列（有索引优先）；都没有则不选。
export function defaultTimeColumn(cols) {
  const c = timeCandidates(cols);
  return c.dt.find((x) => x.name === 'created_at') || c.dt.find((x) => x.indexed) || c.dt[0]
    || c.hinted.find((x) => x.indexed) || c.hinted[0] || null;
}

// 时间戳换算成 “2006-01-02 15:04:05”；offsetSec 是服务器时区相对 UTC 的偏移（秒）。超出可显示范围原样返回。
export function fmtEpoch(v, unit, offsetSec) {
  const n = Number(v);
  const sec = unit === 'epoch_ms' ? Math.floor(n / 1000) : n;
  if (!Number.isFinite(sec) || Math.abs(sec) > 253402300799) return String(v);
  const d = new Date((sec + (Number(offsetSec) || 0)) * 1000);
  const p = (x, w = 2) => String(x).padStart(w, '0');
  return `${p(d.getUTCFullYear(), 4)}-${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())} ${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}`;
}

// 把服务端的探测结果（/sources/time-probe）与所选单位对照，返回 [{ level, text }]（level：ok / info / warn / bad）。
// bad 表示单位或列选错（试运行会判红）；这里只是提前提示，以服务端试运行为准。
export function probeNotes(pb, unit) {
  if (!pb) return [];
  if (!pb.sampled) return [{ level: 'warn', text: t('该列里没有大于 0 的值（表是空的，或全是 0 / NULL），无法核对时间单位') }];
  const gd = UNIT_DIGITS[pb.guess_unit] || 0;
  const d = UNIT_DIGITS[unit] || 0;
  if (!d) {
    return gd
      ? [{ level: 'info', text: t('抽样 {n} 行，值看起来是 {d} 位的 Unix 时间戳，请选择对应的时间单位', { n: pb.sampled, d: gd }) }]
      : [{ level: 'bad', text: t('抽样 {n} 行，值（例如 {v}）不像 10 位或 13 位的 Unix 时间戳，请确认选对了列', { n: pb.sampled, v: pb.sample }) }];
  }
  const inUnit = unit === 'epoch_s' ? pb.in_sec : pb.in_ms;
  if (inUnit * 2 < pb.sampled) {
    return [{
      level: 'bad',
      text: gd && gd !== d
        ? t('值（例如 {v}）看起来是 {g} 位的 Unix 时间戳，与所选的 {d} 位不符，请改选对应的时间单位', { v: pb.sample, g: gd, d })
        : t('值（例如 {v}）不像 {d} 位的 Unix 时间戳，请确认选对了列', { v: pb.sample, d }),
    }];
  }
  const when = (v) => fmtEpoch(v, unit, pb.offset_sec);
  const out = [];
  if (pb.has_range) {
    out.push({ level: 'ok', text: t('按 {d} 位解读：最早 {min}，最晚 {max}（服务器时区）', { d, min: when(pb.min), max: when(pb.max) }) });
    const lo = unit === 'epoch_ms' ? SEC_MIN * 1000 : SEC_MIN;
    const hi = unit === 'epoch_ms' ? SEC_MAX * 1000 : SEC_MAX;
    if (pb.min < lo || pb.max > hi) out.push({ level: 'warn', text: t('有值落在 1980–2100 年之外（最小 {min}，最大 {max}）：该列里可能混用了秒和毫秒，请先核对', { min: pb.min, max: pb.max }) });
  } else {
    out.push({ level: 'ok', text: t('按 {d} 位解读：抽样 {n} 行，例如 {v} 对应 {time}（服务器时区）', { d, n: pb.sampled, v: pb.sample, time: when(pb.sample) }) });
    if (pb.sampled > inUnit) out.push({ level: 'warn', text: t('抽样里有 {n} 行的值不在 {d} 位时间戳的合理区间内：该列里可能混用了秒和毫秒，请先核对', { n: pb.sampled - inUnit, d }) });
  }
  return out;
}

export function defaultParams() {
  const pol = policy();
  const d = pol.defaults || {};
  return {
    batch_size: d.batch_size || 1000,
    sleep_ms: d.sleep_ms === undefined ? 200 : d.sleep_ms,
    max_rows: Math.min(5000000, pol.max_rows_per_run || 5000000),
    max_threads_running: d.max_threads_running || 32,
    max_lag_seconds: d.max_lag_seconds || 5,
    min_disk_free_pct: d.min_disk_free_pct || 15,
    target_batch_ms: d.target_batch_ms || 500,
    max_wait_minutes: Math.round((d.max_wait_seconds || 600) / 60),
    max_consecutive_failures: d.max_consecutive_failures || 3,
    lock_wait_seconds: 0,
  };
}

export function blankTask() {
  return {
    name: '', resource_group_id: 0, instance_id: 0, db: '', table: '',
    time_column: '', time_unit: '', retention_days: Math.max(180, policy().min_retention_days || 30), extra_where: '',
    mode: 'move', purge_reason: '',
    dt_on: true, dt: { instance_id: 0, db: '', template: '{table}_{yyyy}' },
    df_on: false, df: { store_id: 0, format: 'csv', gzip: true, encrypt: false, path_template: '{db}/{table}/{yyyy}{mm}{dd}', part_mb: 256 },
    p: defaultParams(),
    win: { start: '01:00', end: '05:00' },
    sch: { kind: 'day', time: '02:00', weekdays: [1], month_day: 1, cron: '0 2 * * *' },
  };
}

export function fromTask(tv) {
  const t = blankTask();
  Object.assign(t, {
    name: tv.name, resource_group_id: tv.resource_group_id, instance_id: tv.instance_id, db: tv.db, table: tv.table,
    time_column: tv.time_column, time_unit: tv.time_unit || '', retention_days: tv.retention_days, extra_where: tv.extra_where || '',
    mode: tv.mode, purge_reason: tv.purge_reason || '',
  });
  t.dt_on = !!tv.dest_table;
  if (tv.dest_table) t.dt = clone(tv.dest_table);
  t.df_on = !!tv.dest_file;
  if (tv.dest_file) t.df = { ...t.df, ...clone(tv.dest_file) };
  if (!t.df.part_mb) t.df.part_mb = 256;
  const p = tv.params || {};
  t.p = {
    ...t.p, batch_size: p.batch_size, sleep_ms: p.sleep_ms, max_rows: p.max_rows, max_threads_running: p.max_threads_running,
    // 复制延迟检查不能关闭（服务端把 0 当作默认值、负数才是“关闭”且有从库会被拒绝），所以前端只保留阈值
    max_lag_seconds: p.max_lag_seconds > 0 ? p.max_lag_seconds : t.p.max_lag_seconds,
    min_disk_free_pct: p.min_disk_free_pct, target_batch_ms: p.target_batch_ms, max_wait_minutes: Math.max(1, Math.round((p.max_wait_seconds || 600) / 60)),
    max_consecutive_failures: p.max_consecutive_failures, lock_wait_seconds: p.lock_wait_seconds || 0,
  };
  if (p.window) t.win = { start: p.window.start, end: p.window.end };
  if (tv.schedule) t.sch = { ...t.sch, ...clone(tv.schedule), weekdays: (tv.schedule.weekdays || t.sch.weekdays).slice() };
  return t;
}

export function toInput(t) {
  const p = t.p;
  const purge = t.mode === 'purge';
  return {
    name: String(t.name).trim(),
    resource_group_id: Number(t.resource_group_id) || 0,
    instance_id: Number(t.instance_id) || 0, db: t.db, table: t.table,
    time_column: t.time_column, time_unit: t.time_unit || '', retention_days: num(t.retention_days), extra_where: String(t.extra_where || '').trim(),
    mode: t.mode, purge_reason: purge ? String(t.purge_reason || '').trim() : '',
    dest_table: !purge && t.dt_on ? { instance_id: Number(t.dt.instance_id) || 0, db: t.dt.db, template: String(t.dt.template).trim() } : null,
    dest_file: !purge && t.df_on ? { store_id: Number(t.df.store_id) || 0, format: t.df.format, gzip: !!t.df.gzip, encrypt: !!t.df.encrypt, path_template: String(t.df.path_template).trim(), part_mb: num(t.df.part_mb) || 256 } : null,
    params: {
      batch_size: num(p.batch_size), sleep_ms: num(p.sleep_ms), max_rows: num(p.max_rows), max_threads_running: num(p.max_threads_running),
      max_lag_seconds: num(p.max_lag_seconds), min_disk_free_pct: num(p.min_disk_free_pct), target_batch_ms: num(p.target_batch_ms),
      max_wait_seconds: num(p.max_wait_minutes) * 60, max_consecutive_failures: num(p.max_consecutive_failures), lock_wait_seconds: p.lock_wait_seconds || 0,
      window: { start: t.win.start, end: t.win.end },
    },
    schedule: { kind: t.sch.kind, time: t.sch.time, weekdays: (t.sch.weekdays || []).slice(), month_day: num(t.sch.month_day) || 1, cron: t.sch.cron },
  };
}

// 试运行结果与哪些字段绑定：来源 / 条件 / 模式 / 备份去向 / 总量上限（与服务端 spec_hash 同口径）。
export function keyHash(t) {
  const i = toInput(t);
  return JSON.stringify([i.instance_id, i.db, i.table, i.time_column, i.time_unit, i.retention_days, i.extra_where, i.mode, i.purge_reason, i.dest_table, i.dest_file, i.params.max_rows]);
}

// 服务端 issue 的 field -> 向导步骤
export function stepOfField(field) {
  const f = String(field || '');
  if (/^(name|resource_group_id|instance_id|db|table)$/.test(f)) return 0;
  if (/^(time_column|time_unit|retention_days|extra_where)/.test(f)) return 1;
  if (/^(mode|purge_reason|dest_table|dest_file)/.test(f)) return 2;
  if (/^params\.window|^schedule/.test(f)) return 4;
  if (/^params/.test(f)) return 3;
  return -1;
}

const inRange = (v, [lo, hi]) => Number.isInteger(v) && v >= lo && v <= hi;

// 批间休眠下限：会删除源数据的模式（归档并删除 / 仅清理）取服务端公布的 50 ms；仅备份最低 1。
// 0 不可用：服务端把 0 当作“使用安全策略默认值”，而不是不休眠，静默改值会让人误以为已经是最快。
export function sleepMin(mode) {
  const lim = limits();
  return mode === 'copy' ? Math.max(1, (lim.sleep_ms || [0, 5000])[0]) : (lim.sleep_ms_min_when_delete || 50);
}

// 第 step 步的前端校验，返回文案数组。ctx: {edit(TaskView|null), cols(sources/columns), instances}
// 参数 task 即向导工作状态（blankTask 的结构）；不叫 t，免得遮住翻译函数 t()。
export function stepErrors(task, step, ctx) {
  const e = [];
  const lim = limits();
  const pol = policy();
  const cols = ctx.cols;
  if (step === 0) {
    const nm = String(task.name || '').trim();
    const nl = lim.name_len || [2, 50];
    if ([...nm].length < nl[0] || [...nm].length > nl[1]) e.push(t('任务名称需要 {min}–{max} 个字（当前 {n}）', { min: nl[0], max: nl[1], n: [...nm].length }));
    if (!task.instance_id) e.push(t('请选择实例'));
    else if (!task.db) e.push(t('请选择库'));
    else if (!task.table) e.push(t('请选择表'));
    else if (cols) {
      if (cols.protected) e.push(t('{table} 属于受保护库表（安全策略），永不允许归档', { table: `${task.db}.${task.table}` }));
      if (!cols.has_pk) e.push(t('该表没有主键（含唯一非空键），不允许建任务'));
      if ((cols.fk_parents || []).length) e.push(t('表被外键子表引用：{tables}，不能归档', { tables: cols.fk_parents.join(isZh ? '、' : ', ') }));
      if (!(cols.columns || []).some((c) => c.is_time || c.is_int)) e.push(t('该表没有可作时间列的列（需要 datetime / timestamp / date 列，或存着 10 位秒 / 13 位毫秒时间戳的 int / bigint 列），不能按时间归档'));
      const at = cols.active_task;
      if (at && !(ctx.edit && ctx.edit.id === at.id)) e.push(t('这张表已有进行中的任务「{name}」（{status}）', { name: at.name, status: at.status }));
    }
    if (task.instance_id) {
      if (!task.resource_group_id) e.push(t('请选择所属资源组'));
      else {
        const inst = (ctx.instances || []).find((i) => i.id === Number(task.instance_id));
        const rg = inst && (inst.resource_groups || []).find((r) => r.id === Number(task.resource_group_id));
        if (rg && !rg.has_flow) e.push(t('资源组「{name}」没有配置“数据归档申请”审批流，不能提交', { name: rg.name }));
      }
    }
  } else if (step === 1) {
    if (!task.time_column) e.push(t('请选择时间列'));
    else if (isIntTime(colOf(cols, task.time_column)) && !task.time_unit) e.push(t('请选择时间单位（10 位秒 / 13 位毫秒）'));
    const r = num(task.retention_days);
    const min = pol.min_retention_days || 1;
    const max = lim.retention_max_days || 3650;
    if (!Number.isInteger(r) || r < 1) e.push(t('保留天数必须是正整数'));
    else if (r < min) e.push(t('保留天数不得小于安全策略的最小保留天数 {min} 天', { min }));
    else if (r > max) e.push(t('保留天数最大 {max} 天', { max }));
    const xl = lim.extra_where_max || 500;
    if ([...String(task.extra_where || '')].length > xl) e.push(t('附加条件最长 {max} 字', { max: xl }));
  } else if (step === 2) {
    if (!['move', 'copy', 'purge'].includes(task.mode)) e.push(t('请选择模式'));
    if (task.mode === 'purge') {
      if (!pol.allow_purge_no_backup) e.push(t('安全策略当前不允许“不备份清理”'));
      const pr = lim.purge_reason || [5, 500];
      const prn = [...String(task.purge_reason || '').trim()].length;
      if (prn < pr[0]) e.push(t('仅清理必须填写“不备份理由”（至少 {min} 个字）', { min: pr[0] }));
      else if (prn > pr[1]) e.push(t('不备份理由最多 {max} 字（当前 {n}）', { max: pr[1], n: prn }));
    } else {
      if (!task.dt_on && !task.df_on) e.push(t('请至少选择一个备份去向（归档库表 / 文件存储）'));
      if (task.dt_on) {
        if (!task.dt.instance_id) e.push(t('请选择归档库表所在实例'));
        else if (!task.dt.db) e.push(t('请选择归档库'));
        if (!/^[A-Za-z0-9_{}]{1,64}$/.test(String(task.dt.template || ''))) e.push(t('目标表名模板只能含字母、数字、下划线与 {table} {yyyy} {mm} {dd} {yyyymm}，且不超过 64 位'));
        else if (Number(task.dt.instance_id) === Number(task.instance_id) && task.dt.db === task.db && String(task.dt.template) === task.table) e.push(t('目标表不能就是源表'));
      }
      if (task.df_on) {
        if (!task.df.store_id) e.push(t('请选择备份存储'));
        if (!/^[A-Za-z0-9_{}/.-]{1,128}$/.test(String(task.df.path_template || '')) || String(task.df.path_template).includes('..')) e.push(t('路径模板只能含字母、数字、_ - . / 与 {inst} {db} {table} {yyyy} {mm} {dd} {run_id}，且不得含 ..'));
        const pm = lim.part_mb || [16, 4096];
        if (!inRange(num(task.df.part_mb), pm)) e.push(t('单文件上限需要在 {min}–{max} MB 之间的整数', { min: pm[0], max: pm[1] }));
      }
    }
  } else if (step === 3) {
    const p = task.p;
    const chk = (v, rng, name, unit) => { if (!inRange(num(v), rng)) e.push(t('{name}需要在 {min}–{max}{unit} 之间的整数', { name, min: fmtNum(rng[0]), max: fmtNum(rng[1]), unit: unit || '' })); };
    chk(p.batch_size, lim.batch_size || [100, 10000], t('批大小'), t(' 行'));
    if (num(p.sleep_ms) === 0) e.push(t('批间休眠不能填 0，最短请填 1'));
    else chk(p.sleep_ms, [sleepMin(task.mode), (lim.sleep_ms || [0, 5000])[1]], task.mode === 'copy' ? t('批间休眠') : t('批间休眠（会删除源数据的模式不得低于下限）'), ' ms');
    chk(p.max_threads_running, lim.max_threads_running || [4, 1000], t('Threads_running 阈值'));
    chk(p.max_lag_seconds, lim.max_lag_seconds || [1, 3600], t('复制延迟阈值'), t(' 秒'));
    chk(p.min_disk_free_pct, lim.min_disk_free_pct || [5, 50], t('磁盘剩余阈值'), ' %');
    const mw = lim.max_wait_seconds || [60, 3600];
    chk(p.max_wait_minutes, [Math.ceil(mw[0] / 60), Math.floor(mw[1] / 60)], t('最长等待'), t(' 分钟'));
    chk(p.target_batch_ms, lim.target_batch_ms || [100, 5000], t('批耗时目标'), ' ms');
    chk(p.max_consecutive_failures, lim.max_consecutive_failures || [1, 10], t('连续失败暂停阈值'), t(' 批'));
    // 服务端：任务级下限 run_max_rows[0]；超过安全策略上限只是按上限截断（试运行里给黄色），不拒绝
    chk(p.max_rows, lim.run_max_rows || [100, 2000000000], t('单次运行最多处理行数'), t(' 行'));
  } else if (step === 4) {
    const s = { kind: task.sch.kind, time: task.sch.time, weekdays: task.sch.weekdays, month_day: task.sch.month_day, cron: task.sch.cron };
    e.push(...checkSchedule(s, task.win));
  }
  return e;
}
