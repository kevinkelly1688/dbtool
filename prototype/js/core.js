/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: core.js
 * @time: 2026/10/28
 */
/* ============================================================================
   通用工具：DOM / 图表 / 弹窗 / 抽屉 / toast / 路由 / 按钮动作分发
   页面开发约定见 ../README.md「开发约定」。
   ========================================================================== */
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];
const esc = s => String(s == null ? '' : s).replace(/[&<>"]/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;'}[c]));
const rng = seed => { let s = seed >>> 0; return () => (s = (s * 1664525 + 1013904223) >>> 0) / 4294967296; };
const pick = (r, list) => list[Math.floor(r() * list.length)];
const pad2 = x => String(x).padStart(2, '0');
const now = () => { const d = new Date(); return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`; };
const fmt = n => n >= 1e8 ? (n / 1e8).toFixed(1) + ' 亿' : n >= 1e4 ? (n / 1e4).toFixed(1) + ' 万' : String(Math.round(n * 100) / 100);
const fmtMs = ms => ms >= 60000 ? (ms / 60000).toFixed(1) + ' min' : ms >= 1000 ? (ms / 1000).toFixed(2) + ' s' : Math.round(ms) + ' ms';
const sleep = ms => new Promise(r => setTimeout(r, ms));
/* 以 2026-10-07 为“今天”的相对日期：dayOffset(-3) → '2026-10-04' */
const dayOffset = (n, base = '2026-10-07') => { const d = new Date(base + 'T00:00:00'); d.setDate(d.getDate() + n); return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`; };

const stTag = {ok: ['ok', '正常'], warn: ['warn', '警告'], bad: ['bad', '异常'], info: ['info', '建议']};
const tagOf = st => `<span class="tag ${stTag[st][0]}">${stTag[st][1]}</span>`;
const tag = (text, cls = '') => `<span class="tag ${cls}">${esc(text)}</span>`;
const dot = (cls, text) => `<span class="nowrap"><span class="dot ${cls}"></span>${text}</span>`;
const meter = (v, w = 70, b = 85) => `<div class="meter" title="${v}%"><b class="${v >= b ? 'bad' : v >= w ? 'warn' : ''}" style="width:${v}%"></b></div>`;
const kvList = (rows, cls = '') => `<div class="kv ${cls}">${rows.map(([k, v]) => `<div>${k}</div><div>${v == null || v === '' ? '<span class="muted">—</span>' : v}</div>`).join('')}</div>`;
const empty = (text = '暂无数据', ic = '∅') => `<div class="empty"><div class="ic">${ic}</div><div>${text}</div></div>`;
const notice = (html, cls = '', ic = 'ℹ') => `<div class="notice ${cls}"><span>${ic}</span><div>${html}</div></div>`;
const sw = (id, on, label = '') => `<label class="sw"><input type="checkbox" id="${id}" ${on ? 'checked' : ''}><i></i>${label}</label>`;
/* 简单表格：heads 为表头数组，rows 为「行 → 单元格 HTML 数组」，opts.attrs(row,i) 返回 <tr> 属性串 */
function tbl(heads, rows, opts = {}) {
  return `<div class="tbl-wrap"><table${opts.cls ? ` class="${opts.cls}"` : ''}><thead><tr>${heads.map(h => `<th>${h}</th>`).join('')}</tr></thead><tbody>${
    rows.length ? rows.map((r, i) => `<tr${opts.attrs ? opts.attrs(r, i) : ''}>${(opts.cells ? opts.cells(r, i) : r).map(c => `<td>${c}</td>`).join('')}</tr>`).join('') : `<tr><td colspan="${heads.length}">${empty()}</td></tr>`}</tbody></table></div>`;
}
/* 表单行：fld('名称', '<input>', {req, hint}) —— 放在 <div class="form"> 里 */
const fld = (label, input, o = {}) => `<label class="${o.req ? 'req' : ''}">${label}</label><div>${input}</div>${o.hint ? `<div class="hint">${o.hint}</div>` : ''}`;
const pager = (total, page = 1, size = 20) => { const n = Math.max(1, Math.ceil(total / size)); return `<div class="pager"><span>共 ${total} 条</span>${Array.from({length: Math.min(n, 5)}, (_, i) => `<a class="${i + 1 === page ? 'on' : ''}" data-on="noop">${i + 1}</a>`).join('')}${n > 5 ? '<span>…</span>' : ''}</div>`; };

let toastTimer;
function toast(msg) {
  const t = $('#toast'); t.textContent = msg; t.classList.add('show');
  clearTimeout(toastTimer); toastTimer = setTimeout(() => t.classList.remove('show'), 1900);
}
function copyText(text) {
  const done = () => toast('已复制到剪贴板');
  if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done, fb); else fb();
  function fb() { const ta = document.createElement('textarea'); ta.value = text; document.body.appendChild(ta); ta.select(); try { document.execCommand('copy'); } catch (e) {} ta.remove(); done(); }
}

/* ---------- SQL 高亮 / 代码块 ---------- */
const SQL_KW = new Set(('select from where and or not in is null like between insert into values update set delete create alter drop table index database schema add column modify change primary key unique default auto_increment comment engine charset collate group by order having limit offset join left right inner outer on as distinct union all exists case when then else end desc asc count sum avg max min show explain use grant revoke to with option identified kill if truncate rename partition less than maxvalue begin commit rollback full text constraint foreign references view procedure function trigger event using force ignore for lock tables describe exec go top over row_number').split(' '));
function sqlHl(s) {
  const re = /(--[^\n]*|#[^\n]*|\/\*[\s\S]*?\*\/)|('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*")|(\b\d+(?:\.\d+)?\b)|(\b[A-Za-z_][A-Za-z_0-9]*\b)/g;
  let out = '', last = 0, m;
  while ((m = re.exec(s))) {
    out += esc(s.slice(last, m.index)); last = re.lastIndex;
    if (m[1]) out += `<span class="cmt">${esc(m[1])}</span>`;
    else if (m[2]) out += `<span class="str">${esc(m[2])}</span>`;
    else if (m[3]) out += `<span class="num">${m[3]}</span>`;
    else out += SQL_KW.has(m[4].toLowerCase()) ? `<span class="kw">${m[4]}</span>` : m[4];
  }
  return out + esc(s.slice(last));
}
/* code(text)：代码块 + 复制按钮；code(text, 'sql') 带 SQL 高亮 */
const code = (text, lang) => `<pre class="code"><button class="cp" data-copy="${esc(text)}">复制</button>${lang === 'sql' ? sqlHl(text) : esc(text)}</pre>`;
/* diff(旧文本, 新文本)：按行比较，红绿高亮 */
function diffHtml(a, b) {
  const A = String(a).split('\n'), Bn = String(b).split('\n'), sa = new Set(A), sb = new Set(Bn);
  return `<pre class="diff">${A.filter(l => !sb.has(l)).map(l => `<span class="del">- ${esc(l)}</span>`).join('')}${Bn.map(l => sa.has(l) ? `<span>  ${esc(l)}</span>` : `<span class="add">+ ${esc(l)}</span>`).join('')}</pre>`;
}

/* ---------- 迷你图表（SVG） ---------- */
function series(seed, n, base, amp, spike) {
  const r = rng(seed); const out = [];
  for (let i = 0; i < n; i++) {
    let v = base + Math.sin(i / 6) * amp * .5 + (r() - .5) * amp;
    if (spike && i >= spike[0] && i <= spike[1]) v += spike[2];
    out.push(Math.max(0, v));
  }
  return out;
}
function lineChart(list, {w = 560, h = 170, unit = '', xl = ['-60m', '-45m', '-30m', '-15m', '现在']} = {}) {
  const pad = {l: 46, r: 10, t: 10, b: 22};
  const max = (Math.max(...list.flatMap(s => s.data)) * 1.1) || 1, n = list[0].data.length;
  const X = i => pad.l + (w - pad.l - pad.r) * i / (n - 1 || 1), Y = v => pad.t + (h - pad.t - pad.b) * (1 - v / max);
  let g = '';
  for (let k = 0; k <= 4; k++) { const v = max * k / 4, y = Y(v); g += `<line x1="${pad.l}" x2="${w - pad.r}" y1="${y}" y2="${y}" stroke="var(--border)"/><text x="${pad.l - 6}" y="${y + 4}" text-anchor="end" font-size="10" fill="var(--muted)">${fmt(v >= 10 ? Math.round(v) : v)}${unit}</text>`; }
  xl.forEach((t, i) => g += `<text x="${pad.l + (w - pad.l - pad.r) * i / (xl.length - 1 || 1)}" y="${h - 6}" text-anchor="middle" font-size="10" fill="var(--muted)">${t}</text>`);
  const paths = list.map(s => `<path d="${s.data.map((v, i) => (i ? 'L' : 'M') + X(i).toFixed(1) + ' ' + Y(v).toFixed(1)).join('')}" fill="none" stroke="${s.color}" stroke-width="1.8"/>`).join('');
  const lg = list.length > 1 ? `<div class="legend">${list.map(s => `<span><i style="background:${s.color}"></i>${s.name}</span>`).join('')}</div>` : '';
  return `<svg viewBox="0 0 ${w} ${h}" role="img" style="width:100%;height:auto">${g}${paths}</svg>${lg}`;
}
/* 柱状图：items = [[标签, 值, 颜色?]]，可传 items2 画并列第二组 */
function barChart(items, {w = 560, h = 170, color = 'var(--c1)', unit = ''} = {}) {
  const pad = {l: 40, r: 10, t: 10, b: 24}, max = (Math.max(...items.map(i => i[1])) * 1.15) || 1, n = items.length;
  const bw = (w - pad.l - pad.r) / n, Y = v => pad.t + (h - pad.t - pad.b) * (1 - v / max);
  let g = '';
  for (let k = 0; k <= 3; k++) { const v = max * k / 3, y = Y(v); g += `<line x1="${pad.l}" x2="${w - pad.r}" y1="${y}" y2="${y}" stroke="var(--border)"/><text x="${pad.l - 6}" y="${y + 4}" text-anchor="end" font-size="10" fill="var(--muted)">${fmt(v >= 10 ? Math.round(v) : v)}${unit}</text>`; }
  items.forEach(([l, v, c], i) => { const x = pad.l + bw * i + bw * .2; g += `<rect x="${x}" y="${Y(v)}" width="${bw * .6}" height="${h - pad.b - Y(v)}" rx="3" fill="${c || color}"><title>${esc(l)}：${v}${unit}</title></rect><text x="${x + bw * .3}" y="${h - 8}" text-anchor="middle" font-size="10" fill="var(--muted)">${esc(l)}</text>`; });
  return `<svg viewBox="0 0 ${w} ${h}" role="img" style="width:100%;height:auto">${g}</svg>`;
}
function stackBar(items) {
  const total = items.reduce((a, b) => a + b[1], 0) || 1;
  return `<div class="bar">${items.map(([n, v, c]) => `<span style="width:${v / total * 100}%;background:${c}" title="${esc(n)} ${v}">${v / total >= .08 ? `${esc(n)} ${v}` : ''}</span>`).join('')}</div>
  <div class="legend">${items.map(([n, v, c]) => `<span><i style="background:${c}"></i>${esc(n)} ${v}</span>`).join('')}</div>`;
}
function ring(score, label = '健康分') {
  const c = score >= 85 ? 'var(--ok)' : score >= 70 ? 'var(--warn)' : 'var(--bad)';
  return `<div class="ring" style="background:conic-gradient(${c} ${score * 3.6}deg,var(--panel2) 0)"><div><span><b style="color:${c}">${score}</b><br><span class="small muted">${label}</span></span></div></div>`;
}
const spark = (data, color = 'var(--c1)', w = 90, h = 26) => { const mx = Math.max(...data), mn = Math.min(...data), s = (mx - mn) || 1; return `<svg width="${w}" height="${h}" viewBox="0 0 ${w} ${h}"><path d="${data.map((v, i) => (i ? 'L' : 'M') + (i * w / (data.length - 1 || 1)).toFixed(1) + ' ' + (h - 2 - (h - 4) * (v - mn) / s).toFixed(1)).join('')}" fill="none" stroke="${color}" stroke-width="1.5"/></svg>`; };
/* 步骤条：stepsBar(['选择实例','检查','提交'], 1) —— cur 为当前步骤下标 */
const stepsBar = (list, cur) => `<div class="steps">${list.map((t, i) => `<div class="st ${i < cur ? 'done' : i === cur ? 'on' : ''}"><b>${i < cur ? '✓' : i + 1}</b>${t}</div>`).join('')}</div>`;
/* 时间线：tline([{cls:'ok|cur|bad|skip', h:'标题', m:'说明'}]) */
const tline = items => `<div class="tl">${items.map(i => `<div class="it ${i.cls || ''}"><div class="h">${i.h}</div>${i.m ? `<div class="m">${i.m}</div>` : ''}</div>`).join('')}</div>`;

/* ---------- 抽屉 / 弹窗 ---------- */
function openDrawer(title, html, wide) { $('#drTitle').textContent = title; $('#drBody').innerHTML = html; $('#drawer').classList.toggle('wide', !!wide); $('#drawer').classList.add('open'); }
function closeDrawer() { $('#drawer').classList.remove('open', 'wide'); }
let MODAL_OK = null;
/* openModal({title, body, ok, okCls, wide, code: 权限码, onOk(): 返回 false 则不关闭, cancel}) */
function openModal({title, body, ok = '确定', okCls = 'primary', wide = false, code: pcode = '', onOk = null, cancel = '取消'}) {
  const m = $('#modal'), meta = BTNS[pcode];
  m.className = 'modal' + (wide ? ' wide' : '');
  m.innerHTML = `<header><h3>${title}</h3><button class="btn sm" data-mclose="1">✕</button></header><div class="mbody">${body}</div>
    <footer>${pcode ? `<span class="perm-code">权限码：${pcode}${meta && meta.kind === 'd' ? ' · <b style="color:var(--bad)">高危</b>' : ''}</span>` : ''}
    ${cancel ? `<button class="btn" data-mclose="1">${cancel}</button>` : ''}${ok ? `<button class="btn ${okCls}" id="mOk">${ok}</button>` : ''}</footer>`;
  MODAL_OK = onOk; $('#mask').classList.add('open');
  const f = m.querySelector('input:not([type=checkbox]):not([type=radio]),select,textarea'); if (f) setTimeout(() => f.focus(), 30);
}
function closeModal() { $('#mask').classList.remove('open'); MODAL_OK = null; }

/* 高危操作确认：risky(权限码, 标题, 说明HTML, 将执行的SQL/命令, 确认后回调, 需键入的确认文本)
   confirmText 非空时（如 `库.表`、实例名）必须原样输入才能确认。 */
function risky(pcode, title, detail, sql, done, confirmText) {
  openModal({title: '⚠ ' + title, okCls: 'danger primary', ok: '确认执行', code: pcode,
    body: `<div class="notice bad"><span>⚠</span><div>${detail}</div></div>
      ${sql ? `<div class="small muted">将执行：</div>${code(sql, 'sql')}` : ''}
      ${confirmText ? `<div class="form" style="margin-top:8px"><label class="req">确认输入</label><input id="riskInput" placeholder="请输入 ${esc(confirmText)}"></div>` : ''}`,
    onOk() {
      if (confirmText && $('#riskInput').value.trim() !== confirmText) { toast('输入不匹配'); return false; }
      done && done(); return true;
    }});
}

/* ---------- 路由 ---------- */
const pages = {};            // pages[菜单id] = {render(sub) → HTML, init(sub)?, title?: string | (sub)=>string}
const ACTIONS = {};          // 权限码 → 处理函数 (dataset, el)；由 B() 渲染的按钮点击后分发
const ON = {};               // 普通交互（不需要权限码）：<tag data-on="name"> → ON.name(dataset, el, event)
let ROUTE = {page: 'home', sub: ''};
function go(page, sub = '') { const h = page + (sub ? '/' + sub : ''); if (location.hash.slice(1) === h) rerender(); else location.hash = h; }
function render(route) {
  const raw = (typeof route === 'string' ? route : location.hash.slice(1)) || 'home';
  const i = raw.indexOf('/'), page = i < 0 ? raw : raw.slice(0, i), sub = i < 0 ? '' : raw.slice(i + 1);
  const id = pages[page] && NAV[page] ? page : 'home';
  ROUTE = {page: id, sub: id === page ? sub : ''};
  const p = pages[id], view = $('#view'), nav = NAV[id];
  const t = p.title ? (typeof p.title === 'function' ? p.title(ROUTE.sub) : p.title) : nav.name;
  $('#pageTitle').textContent = t; $('#crumb').textContent = nav.group; document.title = t + ' · DB Tool';
  $$('.nav a').forEach(a => a.classList.toggle('active', a.dataset.go === id));
  if (!canView(id)) {
    view.innerHTML = `<div class="card" style="text-align:center;padding:60px 20px"><div style="font-size:48px">🔒</div><h2 style="justify-content:center">无权访问「${nav.name}」</h2>
      <p class="muted">当前用户 <b>${esc(ME.login)}</b> 没有该页面的查看权限（需要 ${nav.perm.map(c => `<code>${c}</code>`).join(' 或 ')}）。</p></div>`;
    return;
  }
  closeDrawer();
  view.innerHTML = p.render(ROUTE.sub);
  p.init && p.init(ROUTE.sub);
  window.scrollTo(0, 0);
}
const rerender = () => render(ROUTE.page + (ROUTE.sub ? '/' + ROUTE.sub : ''));
/* 页内页签：tabs('sqlworkflow', [['list','工单列表'],...], 当前) → 链接形如 #sqlworkflow/list */
const tabs = (page, list, cur) => `<div class="tabs">${list.map(([id, t]) => `<a href="#${page}/${id}" class="${cur === id ? 'on' : ''}">${t}</a>`).join('')}</div>`;
const subtabs = (list, cur, on) => `<div class="subtabs">${list.map(([id, t]) => `<a data-on="${on}" data-v="${id}" class="${cur === id ? 'on' : ''}">${t}</a>`).join('')}</div>`;

/* ---------- 侧栏 / 顶栏 ---------- */
const NAV_FOLD = {};
function buildNav() {
  let h = '';
  NAV_TREE.forEach(g => {
    const its = g.items.filter(m => canView(m.id));
    if (!its.length) return;
    const fold = !!NAV_FOLD[g.group];
    h += `<div class="nav-group ${fold ? 'fold' : ''}" data-on="navFold" data-g="${g.group}">${g.group}</div><div class="nav-items ${fold ? 'fold' : ''}">` + its.map(m => {
      const n = m.badge ? m.badge() : 0;
      return `<a data-go="${m.id}" href="#${m.id}"><span class="ico">${m.ico}</span>${m.name}${n ? `<span class="badge ${m.badgeWarn ? 'warn' : ''}">${n}</span>` : ''}</a>`;
    }).join('') + '</div>';
  });
  $('#nav').innerHTML = h;
  $$('.nav a').forEach(a => a.classList.toggle('active', a.dataset.go === ROUTE.page));
}
ON.navFold = d => { NAV_FOLD[d.g] = !NAV_FOLD[d.g]; buildNav(); };
function buildTop() {
  $('#userSel').innerHTML = USERS.filter(u => u.status === 'active').map(u => `<option value="${u.id}" ${u.id === ME.id ? 'selected' : ''}>${u.login}（${u.super ? '超级用户' : u.groups.map(groupName).join('/')}）</option>`).join('');
  $('#avatar').textContent = ME.name.slice(0, 1);
  applyWatermark();
}
/* ---------- 页面水印 ----------
   开关、内容模板和样式只有超级用户能在「登录与安全设置」里定义（权限码 sec.edit 是锁定码）；其他人只会看到水印，改不了也关不掉。
   水印层被删除 / 隐藏 / 改样式时自动恢复，并记一条审计。 */
const WM_VARS = [['账号', '登录账号'], ['姓名', '姓名'], ['部门', '所属部门'], ['IP', '本次登录 IP'], ['日期', '当前日期'], ['时间', '当前日期 + 时间（到分钟）']];
const WM_GAPS = {tight: ['紧凑', 36, 22], normal: ['标准', 90, 60], loose: ['宽松', 160, 110]};   // [名称, 横向留白, 纵向留白]
const WM_COLORS = {auto: '跟随主题', gray: '灰色', red: '红色', blue: '蓝色'};
const WM_DEF = {text: '{姓名}（{账号}）{时间}', extra: '', opacity: 14, size: 14, angle: -22, gap: 'normal', color: 'auto'};
let WM_PREV = false, WM_OBS = null, WM_EL = null, WM_GOOD = null;
const wmCfg = () => Object.assign({}, WM_DEF, SETTINGS.wmCfg);
const wmSig = el => [el.id, el.className, el.getAttribute('style') || '', el.hidden ? 'h' : ''].join('|');
/* 把模板里的 {账号} {姓名} … 换成当前用户的值；不认识的变量原样保留（保存前会被校验拦住） */
function wmText(tpl, u = ME) {
  const t = now(), v = {账号: u.login, 姓名: u.name, 部门: u.dept, IP: u.ip || SELF_IP, 日期: t.slice(0, 10), 时间: t};
  return String(tpl || '').replace(/\{([^{}]*)\}/g, (m, k) => Object.prototype.hasOwnProperty.call(v, k) ? v[k] : m);
}
/* 按配置生成平铺用的 SVG 瓦片：瓦片尺寸 = 旋转后的文字外框 + 留白，文字不会被切掉 */
function wmTile(cfg) {
  const lines = [wmText(cfg.text), cfg.extra ? wmText(cfg.extra) : ''].filter(Boolean);
  if (!lines.length) return null;
  const size = cfg.size, lh = size * 1.5, rad = cfg.angle * Math.PI / 180, cs = Math.abs(Math.cos(rad)), sn = Math.abs(Math.sin(rad));
  const tw = Math.max(...lines.map(s => [...s].reduce((a, ch) => a + (ch.charCodeAt(0) > 255 ? 1 : .58), 0) * size)), th = lh * lines.length;
  const [, gx, gy] = WM_GAPS[cfg.gap] || WM_GAPS.normal;
  const W = Math.ceil(tw * cs + th * sn + gx), H = Math.ceil(tw * sn + th * cs + gy), cx = W / 2, cy = H / 2;
  const dark = document.documentElement.dataset.theme === 'dark';
  const fill = {auto: dark ? '#dfe6f5' : '#3a4456', gray: '#7d8799', red: '#d93025', blue: '#1a6fe0'}[cfg.color] || '#7d8799';
  const text = lines.map((s, i) => `<text x="${cx}" y="${(cy - th / 2 + lh * (i + .5)).toFixed(1)}" text-anchor="middle" dominant-baseline="central">${esc(s)}</text>`).join('');
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}"><g transform="rotate(${cfg.angle} ${cx} ${cy})" font-size="${size}" font-family="'Microsoft YaHei','PingFang SC',sans-serif" fill="${fill}" fill-opacity="${cfg.opacity / 100}">${text}</g></svg>`;
  return {W, H, url: `url("data:image/svg+xml;utf8,${encodeURIComponent(svg)}")`};
}
/* 重画水印。preview 非空时临时按它显示（保存前预览）；省略则按已保存的设置，未启用或未登录时隐藏 */
function applyWatermark(preview) {
  const w = $('#wm'); if (!w) return;
  WM_PREV = !!preview;
  const cfg = preview || (SETTINGS.watermark ? wmCfg() : null), tile = cfg && wmTile(cfg), on = !!tile && !$('#app').hidden;
  if (WM_OBS) WM_OBS.disconnect();                       // 自己改自己，不能触发防删
  w.classList.toggle('on', on);
  w.style.backgroundImage = tile ? tile.url : '';
  w.style.backgroundSize = tile ? `${tile.W}px ${tile.H}px` : '';
  WM_EL = w; WM_GOOD = {cls: w.className, style: w.getAttribute('style') || '', sig: wmSig(w)};
  if (on) wmWatch();
}
/* 防删：水印层被移除、隐藏、改 id / class / style 时立即恢复，并写一条审计 */
function wmWatch() {
  if (!WM_OBS) WM_OBS = new MutationObserver(() => {
    const el = WM_EL;
    if (!el || (el.isConnected && wmSig(el) === WM_GOOD.sig)) return;
    WM_OBS.disconnect();
    el.id = 'wm'; el.hidden = false; el.className = WM_GOOD.cls; el.setAttribute('style', WM_GOOD.style);
    if (!el.isConnected) document.body.append(el);
    audit('sec.view', '检测到页面水印层被删除或改动，已自动恢复', '拒绝');
    wmWatch();
  });
  WM_OBS.observe(document.body, {childList: true});
  WM_OBS.observe(WM_EL, {attributes: true});
}
setInterval(() => { if (SETTINGS.watermark && !WM_PREV && !$('#app').hidden) applyWatermark(); }, 60 * 1000);   // 每分钟刷新水印里的时间

/* ---------- 全局事件 ---------- */
document.addEventListener('click', e => {
  const t = e.target;
  const cp = t.closest('[data-copy]'); if (cp) { copyText(cp.dataset.copy); return; }
  if (t.closest('[data-close]')) { closeDrawer(); return; }
  if (t.closest('[data-mclose]') || t.id === 'mask') { closeModal(); return; }
  if (t.closest('#mOk')) { const cb = MODAL_OK, html = $('#modal').innerHTML; if (!cb) { closeModal(); return; } const r = cb(); if (r !== false && $('#modal').innerHTML === html) closeModal(); return; }   // onOk 里又 openModal 时不要关掉新弹窗
  if (t.closest('[data-print]')) { window.print(); return; }
  const a = t.closest('[data-act]');
  if (a) {
    const pc = a.dataset.act;
    if (!can(pc)) { audit(pc, '尝试操作：' + (BTNS[pc] || {}).label, '拒绝（无权限）'); toast('无权限'); return; }
    if (ACTIONS[pc]) ACTIONS[pc](a.dataset, a, e); else toast('原型：' + ((BTNS[pc] || {}).label || pc));
    return;
  }
  const h = t.closest('[data-on]');
  if (h && ON[h.dataset.on]) { ON[h.dataset.on](h.dataset, h, e); return; }
});
ON.noop = () => {};
/* 表单控件：<select data-onchange="x"> / <input data-oninput="x"> → ON.x(dataset, el, event)；
   页面刷新（rerender）会丢失输入框焦点，输入类过滤请优先用 data-oninput 配合局部更新，或在 init() 里绑定 */
document.addEventListener('change', e => { const h = e.target.closest('[data-onchange]'); if (h && ON[h.dataset.onchange]) ON[h.dataset.onchange](h.dataset, h, e); });
document.addEventListener('input', e => { const h = e.target.closest('[data-oninput]'); if (h && ON[h.dataset.oninput]) ON[h.dataset.oninput](h.dataset, h, e); });
