/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: sched.js
 * @time: 2026/10/28
 */
// 调度 / 执行窗口的纯函数（向导里即时预览与校验用；以服务端校验为准）。
// 调度语义：每天 / 每周（星期）/ 每月（1–28 日）+ 开始时间，或 5 段 cron（分 时 日 月 周），最短间隔 1 小时；
// 窗口 HH:MM–HH:MM 至少 30 分钟，可跨零点。
import { t, isZh } from '../i18n.js';

export const WD = [t('日|wd'), t('一|wd'), t('二|wd'), t('三|wd'), t('四|wd'), t('五|wd'), t('六|wd')]; // 短名，用于“每周一、三”
export const WD_FULL = [t('周日'), t('周一'), t('周二'), t('周三'), t('周四'), t('周五'), t('周六')]; // 独立显示用
export const LIM_MIN_WINDOW = 30; // 分钟
export const LIM_CRON_GAP = 60; // 分钟

// "HH:MM" -> 当天第几分钟；非法返回 -1
export function hm(s) {
  const m = /^(\d{1,2}):(\d{2})$/.exec(String(s || '').trim());
  return m && +m[1] < 24 && +m[2] < 60 ? +m[1] * 60 + +m[2] : -1;
}

export function parseCron(str) {
  const f = String(str || '').trim().split(/\s+/);
  if (f.length !== 5) return { err: t('cron 需要 5 段：分 时 日 月 周') };
  const R5 = [[0, 59], [0, 23], [1, 31], [1, 12], [0, 7]];
  const out = [];
  for (let i = 0; i < 5; i++) {
    const set = new Set();
    const [lo, hi] = R5[i];
    for (const part of f[i].split(',')) {
      const m = /^(\*|\d+(?:-\d+)?)(?:\/(\d+))?$/.exec(part);
      if (!m) return { err: t('第 {n} 段「{part}」不是有效的 cron 表达式', { n: i + 1, part: f[i] }) };
      let a;
      let b;
      const step = m[2] ? +m[2] : 1;
      if (!step) return { err: t('步长不能为 0') };
      if (m[1] === '*') { a = lo; b = hi; } else if (m[1].includes('-')) { [a, b] = m[1].split('-').map(Number); } else { a = +m[1]; b = m[2] ? hi : a; }
      if (a < lo || b > hi || a > b) return { err: t('第 {n} 段「{part}」超出范围 {lo}–{hi}', { n: i + 1, part: f[i], lo, hi }) };
      for (let v = a; v <= b; v += step) set.add(i === 4 && v === 7 ? 0 : v);
    }
    out.push({ set, star: f[i].startsWith('*') });
  }
  return { f: out };
}

// 从 fromMs 起（不含）的后 n 次触发时间（毫秒数组）；按本机时区计算。
export function schedTimes(s, fromMs, n) {
  const out = [];
  if (s.kind === 'cron') {
    const p = parseCron(s.cron);
    if (p.err) return out;
    const [mi, ho, dm, mo, dw] = p.f;
    let t = Math.floor(fromMs / 60000) * 60000 + 60000;
    let guard = 0;
    while (out.length < n && guard++ < 60 * 24 * 400) {
      const d = new Date(t);
      let dayOk;
      if (dm.star && dw.star) dayOk = true;
      else if (dm.star) dayOk = dw.set.has(d.getDay());
      else if (dw.star) dayOk = dm.set.has(d.getDate());
      else dayOk = dm.set.has(d.getDate()) || dw.set.has(d.getDay());
      if (mo.set.has(d.getMonth() + 1) && dayOk && ho.set.has(d.getHours()) && mi.set.has(d.getMinutes())) out.push(t);
      t += 60000;
    }
    return out;
  }
  const m = hm(s.time);
  if (m < 0) return out;
  const d0 = new Date(fromMs);
  d0.setHours(0, 0, 0, 0);
  for (let k = 0; k < 800 && out.length < n; k++) {
    const d = new Date(d0.getTime());
    d.setDate(d.getDate() + k);
    const ok = s.kind === 'day' || (s.kind === 'week' && (s.weekdays || []).includes(d.getDay())) || (s.kind === 'month' && d.getDate() === +s.month_day);
    if (!ok) continue;
    const t = new Date(d.getFullYear(), d.getMonth(), d.getDate(), Math.floor(m / 60), m % 60, 0).getTime();
    if (t > fromMs) out.push(t);
  }
  return out;
}

export function schedText(s) {
  if (!s) return '—';
  if (s.kind === 'day') return t('每天 {time}', { time: s.time });
  if (s.kind === 'week') return t('每周{days} {time}', { days: (s.weekdays || []).slice().sort((a, b) => a - b).map((d) => WD[d]).join(isZh ? '、' : ', '), time: s.time });
  if (s.kind === 'month') return t('每月 {day} 日 {time}', { day: s.month_day, time: s.time });
  return `cron ${s.cron}`;
}

// 窗口时长（分钟）；start == end 视为不限制，返回 1440。
export function winDur(w) {
  const a = hm(w.start);
  const b = hm(w.end);
  if (a < 0 || b < 0) return -1;
  if (a === b) return 1440;
  return b > a ? b - a : b + 1440 - a;
}

export function inWin(min, w) {
  const a = hm(w.start);
  const b = hm(w.end);
  if (a === b) return true;
  return a < b ? min >= a && min < b : min >= a || min < b;
}

export function winText(w) {
  const a = hm(w.start);
  const b = hm(w.end);
  if (a === b && a >= 0) return t('不限制');
  const d = winDur(w);
  const dur = d >= 60 ? t('{n} 小时', { n: d % 60 ? (d / 60).toFixed(1) : d / 60 }) : t('{n} 分钟', { n: d });
  return t('{start} – {end}{cross}（{dur}）', { start: w.start, end: w.end, cross: b < a ? t('（跨零点）') : '', dur });
}

export const minOf = (ms) => {
  const d = new Date(ms);
  return d.getHours() * 60 + d.getMinutes();
};

const pad2 = (n) => String(n).padStart(2, '0');
export function fmtAt(ms) {
  const d = new Date(ms);
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

// 调度 + 窗口的前端校验，返回错误文案数组（空 = 通过）。
export function checkSchedule(s, w, now = Date.now()) {
  const e = [];
  if (s.kind === 'cron') {
    const p = parseCron(s.cron);
    if (p.err) e.push(p.err);
    else {
      const ts = schedTimes(s, now, 24);
      if (ts.length < 2) e.push(t('该 cron 在未来一段时间内触发不足 2 次'));
      else {
        let gap = Infinity;
        for (let i = 1; i < ts.length; i++) gap = Math.min(gap, ts[i] - ts[i - 1]);
        if (gap < LIM_CRON_GAP * 60000) e.push(t('cron 最短间隔 {min} 分钟（当前最短 {cur} 分钟）', { min: LIM_CRON_GAP, cur: Math.round(gap / 60000) }));
      }
    }
  } else {
    if (hm(s.time) < 0) e.push(t('开始时间格式应为 HH:MM'));
    if (s.kind === 'week' && !(s.weekdays || []).length) e.push(t('请至少选择一个星期'));
    if (s.kind === 'month' && !(Number.isInteger(+s.month_day) && +s.month_day >= 1 && +s.month_day <= 28)) e.push(t('每月的日期取 1–28'));
  }
  if (hm(w.start) < 0 || hm(w.end) < 0) e.push(t('执行窗口格式应为 HH:MM'));
  else if (hm(w.start) !== hm(w.end) && winDur(w) < LIM_MIN_WINDOW) e.push(t('执行窗口至少 {min} 分钟（当前 {cur} 分钟）', { min: LIM_MIN_WINDOW, cur: winDur(w) }));
  else if (s.kind !== 'cron' && hm(s.time) >= 0 && !inWin(hm(s.time), w)) e.push(t('开始时间 {time} 不在执行窗口 {start}–{end} 内，任务永远不会被调度', { time: s.time, start: w.start, end: w.end }));
  else if (s.kind === 'cron' && !parseCron(s.cron).err) {
    const ts = schedTimes(s, now, 5);
    if (ts.length && ts.every((x) => !inWin(minOf(x), w))) e.push(t('cron 未来 5 次触发都落在执行窗口之外，任务会一直被跳过'));
  }
  return e;
}
