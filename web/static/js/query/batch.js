/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: batch.js
 * @time: 2026/10/28
 */
// 在线查询：一次运行脚本里的多条语句（“运行全部”），一条语句一个结果页签，另有一个“汇总”页签。
//
// 每条语句各自走一次 /api/v1/query/execute：库表权限、SQL 防火墙、脱敏、行数上限、审计日志都按条处理，服务端不用改。
// 结果页签里的表格直接调用 grid.js 的 renderResult（悬停复制 / 预览、双击预览、方向键、列头复制整列、“实际执行的 SQL”都照旧）。
// 只有一个结果、又没有固定的页签时不出页签条，和没有这个功能时一样。
// app.js 只接这几处：drawResult 开头调 draw，execute 开头判断 v.batch，工具栏的两个按钮和 Ctrl+Shift+Enter。
import { t, isZh } from '../i18n.js';
import { api } from '../common.js';
import { toast } from '../confirm.js';
import { el, nf } from './ui.js';
import { splitRanges, isSetStatement } from './statements.js';
import { renderResult } from './grid.js';
import { renderPlan } from './plan.js';
import { openMenu } from './ctxmenu.js';
import { copyText } from './clip.js';
import { isRedis, isMongo, isMulti, isMSSQL } from './sqlutil.js';

export const MAX_BATCH = 20; // 一批最多几条（再多先提示，一条都不执行）
const PARALLEL = 3; // “同时执行”的并发数（等于服务端每人的并发上限）
const OPTS_KEY = 'dbtool.query.batch.v1';
const SEP = '\n;\n'; // 前置 SET 和语句之间的连接，同 app.js 的 execute

const STATUS_TEXT = {
  ok: t('成功'), failed: t('失败'), timeout: t('超时'), blocked: t('被拦截'),
  pending: t('排队中'), skipped: t('未执行'), canceled: t('已取消'), running: t('执行中'),
};
// 服务端归为“被拦截”的错误码（见 internal/query/events.go 的 statusOfError）
const BLOCKED = new Set(['bad_query', 'star_forbidden', 'no_privilege', 'readonly_violation', 'firewall_denied']);
const isBad = (s) => s === 'failed' || s === 'timeout' || s === 'blocked';
const isDone = (s) => s !== 'pending' && s !== 'running';
const mysqlFamily = (type) => !isRedis(type) && !isMongo(type) && !isMulti(type) && !isMSSQL(type);

function counts(items) {
  const c = { ok: 0, failed: 0, timeout: 0, blocked: 0, skipped: 0, canceled: 0, pending: 0, running: 0 };
  for (const r of items) c[r.status]++;
  return c;
}

// 语句切分：规则同 statements.js（顶层分号，忽略引号与注释里的分号）。sql 是去掉首尾空白的原文（开头的注释保留，和“运行光标所在语句”一致）。
function parse(text) {
  return splitRanges(text).map((r) => {
    const raw = text.slice(r.start, r.end);
    const start = r.start + (raw.length - raw.trimStart().length);
    const sql = raw.trim();
    let body = sql;
    const comment = [];
    for (;;) {
      const m = body.match(/^(--[^\n]*|#[^\n]*|\/\*[\s\S]*?\*\/)\s*/);
      if (!m) break;
      comment.push(m[1]);
      body = body.slice(m[0].length);
    }
    return { start, end: start + sql.length, sql, body, comment: comment.join(' ') };
  });
}

// 页签 / 汇总里这条语句的简称：开头的注释，否则 “SELECT 表名”。
function labelOf(st) {
  const c = st.comment.replace(/^(--|#)\s*|^\/\*\s*|\s*\*\/$/g, '').replace(/^\d+\s*[.、)）]\s*/, '').trim();
  if (c) return c.length > 16 ? c.slice(0, 16) + '…' : c;
  const verb = (st.body.match(/^\w+/) || [''])[0].toUpperCase();
  const tbl = st.body.match(/\bfrom\s+`?([\w.]+)`?/i);
  return tbl ? `${verb} ${tbl[1]}` : verb;
}

function loadOpts() {
  try {
    const o = JSON.parse(localStorage.getItem(OPTS_KEY));
    if (o && typeof o === 'object') return { stop: o.stop !== false, parallel: o.parallel === true };
  } catch (_) { /* 隐私模式或数据损坏时用默认值 */ }
  return { stop: true, parallel: false };
}

// host（app.js 提供）：
//   editor, view() 当前标签的视图, tab() 当前标签, ctx() { type, instId, db, limit, instName }, busy() 是否有查询在跑,
//   begin(tabId, ctrl) / end() 开始 / 结束一次运行（置灰按钮、计时、刷新页面）, redraw(), setMsg(text, kind),
//   askReason(message) 防火墙要求原因时的弹窗, noPrivExtra(err) 无权限时的“申请”入口, resultOpts(res, item) renderResult 的附加参数。
export function createBatch(host) {
  const editor = host.editor;
  let seq = 0;
  let rseq = 0;
  const opts = loadOpts();
  let pop = null;
  const saveOpts = () => { try { localStorage.setItem(OPTS_KEY, JSON.stringify(opts)); } catch (_) { /* 忽略 */ } };

  // ---- 取要运行的语句 ----
  // 有选区：选区碰到的每条完整语句；否则编辑器里的全部语句。MySQL 系脚本开头连续的 SET 不单独成页签，随每条语句一起提交（同一会话里先执行）。
  function plan() {
    const type = host.ctx().type;
    let stmts = parse(editor.value);
    const sel = editor.selection();
    if (sel) stmts = stmts.filter((s) => s.end > sel.start && s.start < sel.end);
    let prelude = '';
    if (mysqlFamily(type)) {
      let n = 0;
      while (n < stmts.length && isSetStatement(stmts[n].sql)) n++;
      if (n > 0 && n < stmts.length) {
        prelude = stmts.slice(0, n).map((s) => s.sql).join(SEP);
        stmts = stmts.slice(n);
      }
    }
    return stmts.map((s, i) => ({ i, sql: s.sql, run: prelude ? prelude + SEP + s.sql : s.sql, label: labelOf(s) }));
  }

  // 工具栏“运行全部”按钮上的条数。
  function count() { return plan().length; }

  function runAll() {
    const list = plan();
    if (!list.length) return host.setMsg(t('先输入一条 SQL'), 'warn');
    if (list.length > MAX_BATCH) {
      return host.setMsg(t('一次最多运行 {max} 条语句（现在有 {n} 条）。请选中其中一部分再运行，或分几次运行；这次一条都没有执行。', { max: MAX_BATCH, n: list.length }), 'warn');
    }
    return start(host.view(), host.tab(), list);
  }

  // 上一次是“运行全部”且有固定的页签时，单条运行的结果并入同一排页签（没有固定的就回到单个结果）。
  const keepsPins = (v) => !!(v.batch && v.batch.items.some((r) => r.pinned));

  function runSingle(v, tab, one) {
    return start(v, tab, [{ i: 0, sql: one.sql, run: one.run, label: labelOf(parse(one.sql)[0] || { comment: '', body: one.sql }) }]);
  }

  // ---- 运行 ----
  async function start(v, tab, list) {
    const ctx = host.ctx();
    const id = ++seq;
    const fresh = list.map((s) => ({
      id: 'r' + (++rseq), gen: id, i: s.i, sql: s.sql, run: s.run, label: s.label, status: 'pending',
      res: null, err: null, ms: null, pinned: false, started: 0, ctx, n: 0,
    }));
    const pinned = v.batch ? v.batch.items.filter((r) => r.pinned) : [];
    const b = {
      id, v, tabId: tab.id, items: [...pinned, ...fresh], hasOv: fresh.length > 1, running: true, wall: 0, seg: performance.now(),
      stop: opts.stop, parallel: opts.parallel && fresh.length > 1, halted: 0, active: fresh.length > 1 ? 'ov' : fresh[0].id,
      reason: null, reasonP: null, ctrl: new AbortController(), stripLeft: 0, ovTop: 0, container: null, stripEl: null, bodyEl: null, bodyKey: '', revealed: '',
    };
    v.batch = b;
    v.res = null;
    v.plan = null;
    v.msg = null;
    v.rtab = 'res';
    host.begin(tab.id, b.ctrl);
    await runItems(b, fresh, { stop: b.stop, parallel: b.parallel });
    b.wall += performance.now() - b.seg;
    b.running = false;
    if (fresh.length > 1) {
      const c = counts(fresh);
      const parts = [t('成功 {n} 条', { n: c.ok })];
      if (c.failed + c.timeout) parts.push(t('失败 {n} 条', { n: c.failed + c.timeout }));
      if (c.blocked) parts.push(t('被拦截 {n} 条', { n: c.blocked }));
      if (c.skipped + c.canceled) parts.push(t('未执行 {n} 条', { n: c.skipped + c.canceled }));
      // 有汇总页签、又全部成功时，汇总页签已经说明一切，不再另外出一条提示
      if (c.ok !== fresh.length) {
        v.msg = {
          text: t('共 {total} 条：{parts}，用时 {s} 秒。', { total: fresh.length, parts: parts.join(isZh ? '，' : ', '), s: (b.wall / 1000).toFixed(1) }),
          kind: 'warn',
        };
      }
    }
    host.end();
  }

  // 重新运行其中几条：沿用这批的实例 / 库，依次执行，遇错不停。
  async function rerun(b, items) {
    if (host.busy()) return toast(t('正在运行，请稍等或先取消'), 'warn');
    for (const r of items) { r.status = 'pending'; r.res = null; r.err = null; r.n++; }
    b.running = true;
    b.halted = 0;
    if (!b.reason) b.reasonP = null; // 上次拒绝填原因的，这次重新问
    b.seg = performance.now();
    b.ctrl = new AbortController();
    b.v.msg = null;
    host.begin(b.tabId, b.ctrl);
    await runItems(b, items, { stop: false, parallel: false });
    b.wall += performance.now() - b.seg;
    b.running = false;
    host.end();
  }

  async function runItems(b, items, mode) {
    const signal = b.ctrl.signal;
    const timer = setInterval(() => tickLive(b), 250);
    const skip = (from) => {
      for (const r of items.slice(from)) {
        if (r.status !== 'pending') continue;
        r.status = signal.aborted ? 'canceled' : 'skipped';
        r.err = { message: signal.aborted ? t('已取消，这一条没有执行') : t('前面第 {n} 条没有成功，已按设置停止，这一条没有执行', { n: b.halted }) };
      }
    };
    const after = (r) => { if (mode.stop && isBad(r.status) && !b.halted) b.halted = r.i + 1; };
    try {
      if (!mode.parallel) {
        for (let k = 0; k < items.length; k++) {
          if (signal.aborted || b.halted) { skip(k); break; }
          await runOne(b, items[k]);
          after(items[k]);
        }
      } else {
        let next = 0;
        const worker = async () => {
          for (;;) {
            if (signal.aborted || b.halted) return;
            const k = next++;
            if (k >= items.length) return;
            await runOne(b, items[k]);
            after(items[k]);
          }
        };
        await Promise.all(Array.from({ length: PARALLEL }, worker));
        skip(0);
      }
    } finally {
      clearInterval(timer);
    }
    skip(0);
  }

  async function runOne(b, r) {
    r.status = 'running';
    r.started = performance.now();
    r.err = null;
    r.res = null;
    live(b);
    try {
      const res = await callExec(b, r);
      r.status = 'ok';
      r.res = res;
      r.ms = res.cost_ms;
    } catch (e) {
      r.status = e.name === 'AbortError' || e.code === 'canceled' ? 'canceled' : e.code === 'timeout' ? 'timeout' : BLOCKED.has(e.code) ? 'blocked' : 'failed';
      r.ms = Math.round(performance.now() - r.started);
      r.err = {
        message: e.name === 'AbortError' ? t('已取消。') : e.status === 401 ? t('登录已过期，请刷新页面重新登录。') : e.message,
        code: e.code && e.code !== 'canceled' ? e.code : '',
        extra: e.code === 'no_privilege' && e.data && e.data.database ? host.noPrivExtra(e) : null,
      };
      if (e.status === 401 && !b.halted) b.halted = r.i + 1; // 登录过期：后面的都会失败，不再发请求
    }
    live(b);
  }

  // 防火墙“要求填写原因”：整批只问一次，原因用于这批里需要原因的语句。
  function getReason(b, message) {
    if (b.reason) return Promise.resolve(b.reason);
    if (!b.reasonP) {
      b.reasonP = host.askReason(message).then((r) => {
        if (!r.ok) {
          const c = new Error(t('已取消：没有填写原因，语句没有执行。'));
          c.code = 'canceled';
          throw c;
        }
        b.reason = r.value;
        return r.value;
      });
    }
    return b.reasonP;
  }

  async function callExec(b, r) {
    const req = { instance_id: r.ctx.instId, db: r.ctx.db, sql: r.run, limit: r.ctx.limit };
    for (;;) {
      try {
        const res = await api('POST', '/api/v1/query/execute', req, { signal: b.ctrl.signal });
        return res;
      } catch (e) {
        if (e.code !== 'reason_required' || req.reason) throw e;
        req.reason = await getReason(b, e.message);
      }
    }
  }

  // ---- 结果区 ----
  // 页签：汇总（这批不止一条时）、各条结果（含固定的）、执行计划（单独点了“执行计划”之后）。
  function tabsOf(b) {
    const list = [];
    if (b.hasOv && b.items.some((r) => r.gen === b.id)) list.push({ id: 'ov' });
    list.push(...b.items);
    if (b.v.plan) list.push({ id: 'plan' });
    return list;
  }

  // 画结果区。container 是 #result，已由调用方清空前不要自己清（这里清）。返回 false 表示这个标签没有批量结果，走原来的画法。
  function draw(container, v) {
    sync(v);
    const b = v.batch;
    if (!b) return false;
    if (v.rtab === 'plan' && v.plan) b.active = 'plan'; // 刚点了“执行计划”
    container.replaceChildren();
    b.container = container;
    b.stripEl = null;
    b.bodyEl = null;
    b.bodyKey = '';
    paint(b);
    return true;
  }

  function bodyKeyOf(b, n) {
    if (b.active === 'ov') return `ov:${n}:${b.items.length}:${b.running}:${b.items.map((r) => r.status).join(',')}`;
    if (b.active === 'plan') return 'plan:' + n;
    const r = b.items.find((x) => x.id === b.active);
    return r ? `${r.id}:${r.status}:${r.n}:${n}` : '';
  }

  // 页签条总是重画（便宜）；内容区只在需要时重画，免得运行中别的语句状态一变就把正在看的结果表滚回顶部、选中的单元格也丢了。
  function paint(b, force) {
    const tabs = tabsOf(b);
    if (!tabs.some((x) => x.id === b.active)) b.active = tabs[0].id;
    const c = b.container;
    const strip = tabs.length > 1 ? buildStrip(b, tabs) : null;
    if (b.stripEl && b.stripEl.parentNode === c) {
      if (strip) b.stripEl.replaceWith(strip); else b.stripEl.remove();
    } else if (strip) {
      c.prepend(strip);
    }
    b.stripEl = strip;
    if (strip) scrollStripTo(b, strip);
    const key = bodyKeyOf(b, tabs.length > 1 ? 1 : 0);
    if (!force && b.bodyEl && b.bodyEl.parentNode === c && key === b.bodyKey) return;
    const body = el('div', 'res-body');
    body.setAttribute('role', 'tabpanel');
    if (b.bodyEl && b.bodyEl.parentNode === c) b.bodyEl.replaceWith(body); else c.append(body);
    b.bodyEl = body;
    b.bodyKey = key;
    fill(b, body, tabs.length > 1);
    sync(b.v);
  }

  // 运行中状态变化：这个标签正显示着才重画。
  function live(b) {
    sync(b.v);
    if (!b.container || !b.container.isConnected || !b.v.batch || b.v.batch !== b) return;
    if (!b.bodyEl || b.bodyEl.parentNode !== b.container) return; // 已经切到别的标签 / 被别的内容盖掉
    paint(b);
  }

  function tickLive(b) {
    if (!b.container || !b.container.isConnected) return;
    for (const x of b.container.querySelectorAll('[data-bt-live]')) {
      const r = b.items.find((y) => y.id === x.dataset.btLive);
      if (r && r.status === 'running') x.textContent = t('已用 {s} 秒', { s: ((performance.now() - r.started) / 1000).toFixed(1) });
    }
    const ov = b.container.querySelector('[data-bt-wall]');
    if (ov) ov.textContent = wallText(b);
  }

  const wallOf = (b) => (b.wall + (b.running ? performance.now() - b.seg : 0)) / 1000;
  const wallText = (b) => t('用时 {s} 秒', { s: wallOf(b).toFixed(1) });

  function select(b, id) {
    b.active = id;
    if (b.v.plan) b.v.rtab = id === 'plan' ? 'plan' : 'res';
    paint(b, true);
  }

  // ---- 页签条 ----
  function tabSub(r) {
    if (r.status === 'ok') return t('{n} 行 · {ms} ms', { n: nf(r.res.row_count), ms: nf(r.ms) });
    if (r.status === 'running') return t('执行中 {s}s', { s: ((performance.now() - r.started) / 1000).toFixed(1) });
    if (r.ms != null && isBad(r.status)) return `${STATUS_TEXT[r.status]} · ${nf(r.ms)} ms`;
    return STATUS_TEXT[r.status];
  }

  function statusDot(status) { return el('span', 'bt-si ' + status); }

  function buildStrip(b, tabs) {
    const strip = el('div', 'bt-strip');
    const prev = el('button', 'bt-arrow', '◀');
    const next = el('button', 'bt-arrow', '▶');
    prev.type = next.type = 'button';
    prev.hidden = next.hidden = true;
    prev.setAttribute('aria-label', t('向左滚动'));
    next.setAttribute('aria-label', t('向右滚动'));
    const sc = el('div', 'bt-scroll');
    sc.setAttribute('role', 'tablist');
    sc.setAttribute('aria-label', t('结果页签'));
    const ids = tabs.map((x) => x.id);
    const go = (id) => { select(b, id); focusTab(b, id); };
    for (const x of tabs) {
      const r = x.status ? x : null;
      const tab = el('div', 'bt-tab' + (r ? ' st-' + r.status + (r.pinned ? ' pinned' : '') : ' special'));
      tab.setAttribute('role', 'tab');
      tab.tabIndex = 0;
      tab.dataset.id = x.id;
      tab.setAttribute('aria-selected', String(b.active === x.id));
      if (!r) {
        tab.append(el('span', 'bt-t1', x.id === 'ov' ? t('汇总') : t('执行计划')));
        tab.title = x.id === 'ov' ? t('这一批每条语句的状态、行数、耗时') : t('执行计划');
      } else {
        if (r.status === 'running' || r.status === 'pending' || r.status === 'skipped' || r.status === 'canceled') tab.append(statusDot(r.status));
        tab.append(el('span', 'bt-t1', t('结果 {n}', { n: r.i + 1 })));
        const pin = el('span', 'bt-pin', '📌');
        pin.setAttribute('role', 'button');
        pin.setAttribute('aria-pressed', String(!!r.pinned));
        pin.title = r.pinned ? t('取消固定') : t('固定：下一次运行不会替换它');
        pin.addEventListener('click', (e) => { e.stopPropagation(); togglePin(b, r); });
        const close = el('span', 'bt-x', '×');
        close.setAttribute('role', 'button');
        close.title = t('关闭（中键也可以）');
        close.addEventListener('click', (e) => { e.stopPropagation(); closeItems(b, [r]); });
        tab.append(pin, close);
        tab.title = `${t('结果 {n}', { n: r.i + 1 })} · ${r.label}\n${tabSub(r)}\n\n${r.sql}`;
        tab.addEventListener('auxclick', (e) => { if (e.button === 1) { e.preventDefault(); closeItems(b, [r]); } });
        tab.addEventListener('contextmenu', (e) => { e.preventDefault(); tabMenu(b, r, e); });
      }
      tab.addEventListener('click', () => select(b, x.id));
      tab.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); go(x.id); }
        if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
          e.preventDefault();
          const k = ids.indexOf(x.id) + (e.key === 'ArrowRight' ? 1 : -1);
          if (k >= 0 && k < ids.length) go(ids[k]);
        }
      });
      sc.append(tab);
    }
    strip.append(prev, sc, next);
    if (b.running) {
      const done = b.items.filter((r) => r.gen === b.id && isDone(r.status)).length;
      const total = b.items.filter((r) => r.gen === b.id).length || 1;
      const p = el('div', 'bt-prog');
      p.style.width = Math.round((done / total) * 100) + '%';
      strip.append(p);
    }
    prev.addEventListener('click', () => sc.scrollBy({ left: -240, behavior: 'smooth' }));
    next.addEventListener('click', () => sc.scrollBy({ left: 240, behavior: 'smooth' }));
    sc.addEventListener('scroll', () => { b.stripLeft = sc.scrollLeft; });
    strip._sc = sc;
    strip._arrows = [prev, next];
    return strip;
  }

  // 条放进页面之后才量得出宽度：恢复上次的滚动位置，选中的页签露出来，放不下时显示左右箭头。
  function scrollStripTo(b, strip) {
    const sc = strip._sc;
    sc.scrollLeft = b.stripLeft || 0;
    const on = sc.querySelector('.bt-tab[aria-selected="true"]');
    if (on) {
      const l = on.offsetLeft;
      const r = l + on.offsetWidth;
      if (l < sc.scrollLeft) sc.scrollLeft = Math.max(0, l - 8);
      else if (r > sc.scrollLeft + sc.clientWidth) sc.scrollLeft = r - sc.clientWidth + 8;
    }
    b.stripLeft = sc.scrollLeft;
    const over = sc.scrollWidth > sc.clientWidth + 1;
    for (const a of strip._arrows) a.hidden = !over;
  }

  function focusTab(b, id) {
    requestAnimationFrame(() => {
      const tb = b.container && b.container.querySelector(`.bt-tab[data-id="${id}"]`);
      if (tb) tb.focus();
    });
  }

  // 固定只在“下一次运行”时才有效果，所以点完要明确告诉用户：页签上也画出固定状态（amber 顶边 + 图钉高亮）。
  function togglePin(b, r) {
    r.pinned = !r.pinned;
    paint(b, true);
    toast(r.pinned ? t('已固定“结果 {n}”：下一次运行会保留它', { n: r.i + 1 }) : t('已取消固定“结果 {n}”', { n: r.i + 1 }), 'ok');
  }

  // 关闭页签：运行中的批次里还没跑完的那几条不能关（关了也会被继续执行）。
  function closeItems(b, items) {
    if (items.some((r) => r.status === 'pending' || r.status === 'running')) return toast(t('这一条还没有执行完，先取消'), 'warn');
    const ids = new Set(items.map((r) => r.id));
    const k = b.items.findIndex((r) => ids.has(r.id));
    b.items = b.items.filter((r) => !ids.has(r.id));
    if (ids.has(b.active)) {
      const rest = b.items;
      b.active = rest.length ? rest[Math.min(k, rest.length - 1)].id : b.v.plan ? 'plan' : '';
    }
    if (!b.items.length && !b.v.plan) {
      b.v.batch = null;
      return host.redraw();
    }
    paint(b, true);
  }

  function tabMenu(b, r, e) {
    const idx = b.items.indexOf(r);
    const busy = host.busy();
    openMenu({ x: e.clientX, y: e.clientY }, [
      { label: t('复制 SQL'), run: async () => { const ok = await copyText(r.sql); toast(ok ? t('已复制 SQL') : t('复制失败'), ok ? 'ok' : 'err'); } },
      { label: t('定位到语句'), run: () => locate(b, r) },
      ...(busy ? [] : [{ label: t('只重新运行这一条'), run: () => rerun(b, [r]) }]),
      { label: r.pinned ? t('取消固定') : t('固定这个页签'), run: () => togglePin(b, r) },
      { label: t('关闭'), run: () => closeItems(b, [r]) },
      { label: t('关闭其他'), run: () => closeItems(b, b.items.filter((x) => x !== r)) },
      { label: t('关闭右侧'), run: () => closeItems(b, b.items.slice(idx + 1)) },
    ], { label: t('结果页签') });
  }

  // ---- 内容区 ----
  function fill(b, body, multi) {
    if (b.active === 'ov') return fillOverview(b, body);
    if (b.active === 'plan') return renderPlan(body, b.v.plan);
    const r = b.items.find((x) => x.id === b.active);
    if (!r) return;
    if (r.status === 'ok') {
      if (r.res.kind === 'explain') return renderPlan(body, r.res);
      return renderResult(body, r.res, { title: multi ? t('结果 {n}', { n: r.i + 1 }) : t('结果'), ...host.resultOpts(r.res, r) });
    }
    if (isBad(r.status)) return fillFail(b, body, r);
    fillWaiting(b, body, r);
  }

  function fillFail(b, body, r) {
    const box = el('div', 'bt-err');
    const warn = r.status === 'blocked' || r.status === 'timeout';
    const a = el('div', 'alert ' + (warn ? 'warn' : 'err'));
    a.append(el('b', '', r.status === 'blocked' ? t('被拦截：') : r.status === 'timeout' ? t('超时：') : t('执行失败：')), document.createTextNode(r.err.message));
    if (r.err.code) a.append(el('span', 'chip ' + (warn ? 'warn' : 'bad'), r.err.code));
    const x = r.err.extra;
    if (x && x.href) {
      const l = el('a', 'alert-act', x.text);
      l.href = x.href;
      l.target = '_blank';
      l.rel = 'noopener';
      a.append(l);
    } else if (x && x.plain) {
      a.append(document.createTextNode(x.plain));
    }
    box.append(a);
    const acts = el('div', 'bt-acts');
    const loc = el('button', 'btn', t('定位到语句'));
    loc.type = 'button';
    loc.addEventListener('click', () => locate(b, r));
    const again = el('button', 'btn', t('重新运行'));
    again.type = 'button';
    again.disabled = host.busy();
    again.addEventListener('click', () => rerun(b, [r]));
    acts.append(loc, again);
    box.append(acts);
    body.append(box);
  }

  function fillWaiting(b, body, r) {
    const box = el('div', 'bt-state');
    if (r.status === 'running') {
      const sp = el('span', 'bt-si running bt-spin');
      const sec = el('span', 'muted', t('已用 {s} 秒', { s: ((performance.now() - r.started) / 1000).toFixed(1) }));
      sec.dataset.btLive = r.id;
      box.append(sp, document.createTextNode(t('执行中…')), sec);
    } else if (r.status === 'pending') {
      const ahead = b.items.filter((x) => x.gen === r.gen && x.i < r.i && !isDone(x.status)).length;
      box.append(document.createTextNode(ahead ? t('排队中，前面还有 {n} 条', { n: ahead }) : t('排队中')));
    } else {
      box.append(document.createTextNode(r.err ? r.err.message : STATUS_TEXT[r.status]));
      const acts = el('div', 'bt-acts');
      const go = el('button', 'btn', t('运行这一条'));
      go.type = 'button';
      go.disabled = host.busy();
      go.addEventListener('click', () => rerun(b, [r]));
      acts.append(go);
      box.append(acts);
    }
    body.append(box);
  }

  function fillOverview(b, body) {
    const items = b.items.filter((r) => r.gen === b.id);
    const c = counts(items);
    const sum = el('div', 'bt-sum');
    const stat = (cls, label, n) => {
      const d = el('span', 'bt-s ' + cls);
      d.append(document.createTextNode(label + ' '), el('b', '', String(n)));
      return d;
    };
    sum.append(stat('', t('共'), items.length), stat('ok', t('成功'), c.ok));
    if (c.failed + c.timeout) sum.append(stat('bad', t('失败'), c.failed + c.timeout));
    if (c.blocked) sum.append(stat('wn', t('被拦截'), c.blocked));
    if (c.skipped + c.canceled) sum.append(stat('', t('未执行'), c.skipped + c.canceled));
    if (c.running + c.pending) sum.append(stat('', t('进行中'), c.running + c.pending));
    const how = el('span', 'bt-s muted');
    how.append(document.createTextNode(`${b.parallel ? t('同时执行') : t('依次执行')} · ${b.stop ? t('遇错停止') : t('遇错继续')} · `));
    const wall = el('span', '', wallText(b));
    wall.dataset.btWall = '1';
    how.append(wall);
    sum.append(how, el('span', 'spacer'));
    const bad = items.filter((r) => isBad(r.status) || r.status === 'skipped' || r.status === 'canceled');
    const mk = (label, fn, off) => {
      const x = el('button', 'btn sm', label);
      x.type = 'button';
      x.disabled = !!off;
      x.addEventListener('click', fn);
      sum.append(x);
    };
    const off = b.running || host.busy();
    mk(t('重试没成功的（{n}）', { n: bad.length }), () => rerun(b, bad), off || !bad.length);
    mk(t('全部重新运行'), () => rerun(b, items.slice()), off);
    mk(t('复制汇总'), async () => {
      const rows = items.map((r) => [`#${r.i + 1}`, STATUS_TEXT[r.status], r.res ? r.res.row_count : '', r.ms != null ? r.ms + 'ms' : '', r.sql.replace(/\s+/g, ' ')].join('\t'));
      const ok = await copyText(rows.join('\n'));
      toast(ok ? t('已复制汇总') : t('复制失败'), ok ? 'ok' : 'err');
    });
    body.append(sum);

    const wrap = el('div', 'bt-ovwrap');
    const table = el('table', 'bt-ov');
    const hr = el('tr');
    for (const [label, cls] of [['#', ''], [t('语句'), ''], [t('状态'), ''], [t('行数'), 'r'], [t('耗时'), 'r'], [t('说明'), '']]) hr.append(el('th', cls, label));
    const thead = el('thead');
    thead.append(hr);
    table.append(thead);
    const tb = el('tbody');
    for (const r of items) {
      const tr = el('tr', isBad(r.status) && r.status !== 'blocked' ? 'bad' : r.status === 'blocked' ? 'blk' : '');
      tr.tabIndex = 0;
      tr.append(el('td', 'n', String(r.i + 1)));
      const td = el('td');
      td.append(el('span', 'nm', r.label), el('span', 'sq', r.sql.replace(/\s+/g, ' ')));
      tr.append(td);
      const st = el('td', 'bt-stc');
      const badge = el('span', 'bt-stb ' + r.status);
      if (r.status !== 'ok') badge.append(statusDot(r.status));
      badge.append(document.createTextNode(STATUS_TEXT[r.status]));
      st.append(badge);
      tr.append(st, el('td', 'r', r.status === 'ok' ? nf(r.res.row_count) : '—'), el('td', 'r', r.ms != null ? nf(r.ms) + ' ms' : '—'));
      const why = el('td', 'why');
      const notes = [];
      if (r.status === 'ok') {
        if (r.res.masked_columns && r.res.masked_columns.length) notes.push(t('🛡 已脱敏 {cols}', { cols: r.res.masked_columns.join(', ') }));
        if (r.res.truncated) notes.push(t('已按 {n} 行截断', { n: nf(r.res.effective_limit) }));
      } else if (r.err) {
        notes.push(r.err.message);
      }
      why.textContent = notes.join('; ');
      tr.append(why);
      const open = () => { select(b, r.id); focusTab(b, r.id); };
      tr.addEventListener('click', open);
      tr.addEventListener('keydown', (e) => { if (e.key === 'Enter') open(); });
      tb.append(tr);
    }
    table.append(tb);
    wrap.append(table);
    wrap.addEventListener('scroll', () => { b.ovTop = wrap.scrollTop; });
    body.append(wrap);
    wrap.scrollTop = b.ovTop || 0;
  }

  // ---- 编辑器：行号槽里的状态圆点、当前页签对应语句的高亮 ----
  // 编辑器里的每条语句按“文本相同、先后顺序一致”对上这批的各条结果（改过的语句对不上就没有圆点）。
  function matchItems(b, stmts) {
    const map = new Map();
    let from = 0;
    for (const r of b.items) {
      if (r.gen !== b.id) continue;
      const k = stmts.findIndex((s, i) => i >= from && s.sql === r.sql);
      if (k >= 0) { map.set(r.id, stmts[k]); from = k + 1; }
    }
    return map;
  }

  function sync(v) {
    const b = v && v.batch;
    if (!b) {
      editor.setGutterMarks([]);
      editor.highlight(null);
      return;
    }
    const map = matchItems(b, parse(editor.value));
    editor.setGutterMarks([...map].map(([id, s]) => {
      const r = b.items.find((x) => x.id === id);
      return { row: editor.rowOf(s.start), n: r.i + 1, status: r.status };
    }));
    const act = map.get(b.active);
    editor.highlight(act ? { start: act.start, end: act.end } : null, b.revealed !== b.active && !!act);
    b.revealed = b.active;
  }

  function locate(b, r) {
    const s = matchItems(b, parse(editor.value)).get(r.id);
    if (!s) return toast(t('编辑器里已经找不到这条语句了'), 'warn');
    editor.select(s.start, s.end);
  }

  // 点行号槽里的圆点：跳到对应的结果页签。
  editor.onGutterMark((row) => {
    const v = host.view();
    const b = v && v.batch;
    if (!b) return false;
    const map = matchItems(b, parse(editor.value));
    for (const [id, s] of map) {
      if (editor.rowOf(s.start) === row) {
        if (!b.container || !b.container.isConnected) return false;
        select(b, id);
        return true;
      }
    }
    return false;
  });

  // ---- 批量选项 ----
  function closePop() {
    if (!pop) return;
    pop.remove();
    pop = null;
    document.removeEventListener('mousedown', onDocDown, true);
    document.removeEventListener('keydown', onDocKey, true);
    const btn = document.getElementById('batch-opts');
    if (btn) btn.setAttribute('aria-expanded', 'false');
  }
  const onDocDown = (e) => { if (pop && !pop.contains(e.target) && !e.target.closest('#batch-opts')) closePop(); };
  const onDocKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); closePop(); } };

  function togglePop(btn) {
    if (pop) return closePop();
    pop = el('div', 'bt-pop');
    pop.setAttribute('role', 'dialog');
    pop.setAttribute('aria-label', t('批量选项'));
    const radio = (name, value, checked, label) => {
      const l = el('label');
      const i = el('input');
      i.type = 'radio';
      i.name = name;
      i.value = value;
      i.checked = checked;
      l.append(i, document.createTextNode(label));
      return l;
    };
    pop.append(
      el('h5', '', t('遇到错误时')),
      radio('bt-stop', '1', opts.stop, t('停止，后面的语句不再执行（默认）')),
      radio('bt-stop', '0', !opts.stop, t('继续执行后面的语句')),
      el('h5', '', t('执行方式')),
      radio('bt-par', '0', !opts.parallel, t('依次执行（默认）')),
      radio('bt-par', '1', opts.parallel, t('同时执行，最多 {n} 条', { n: PARALLEL })),
      el('div', 'tip', t('一次最多 {max} 条。每条语句都单独做库表权限、防火墙、脱敏和行数限制，并各记一条审计日志。同时执行适合互不相干的查询，脚本里后面的语句依赖前面时请用“依次”。', { max: MAX_BATCH })),
    );
    pop.addEventListener('change', () => {
      opts.stop = pop.querySelector('input[name=bt-stop]:checked').value === '1';
      opts.parallel = pop.querySelector('input[name=bt-par]:checked').value === '1';
      saveOpts();
    });
    document.body.append(pop);
    const r = btn.getBoundingClientRect();
    pop.style.left = Math.max(8, Math.min(r.left, window.innerWidth - pop.offsetWidth - 8)) + 'px';
    pop.style.top = r.bottom + 4 + 'px';
    btn.setAttribute('aria-expanded', 'true');
    document.addEventListener('mousedown', onDocDown, true);
    document.addEventListener('keydown', onDocKey, true);
  }

  return { draw, sync, count, runAll, runSingle, keepsPins, togglePop };
}
