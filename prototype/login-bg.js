/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: login-bg.js
 * @time: 2026/10/28
 */
// 登录页左侧“科学感”背景原型（不是正式页面）。
//
// 思路：把左侧品牌区做成一张“科学图版”——毫米坐标纸 + 标尺刻度 + 一种数据底图（三选一），
// 前景仍是原来的 SQL 流向拓扑。三种底图都是纯 SVG、固定随机种子（每次刷新一致）、没有内联样式，
// 颜色全部走 --dt-* 变量，三套皮肤自动适配；落地到正式页面时可以原样搬进 login_visual.js。
//
//   a 等值线  标量场（几个高斯峰叠加）的等高线，每隔几层一条粗线并标数值——像势场 / 地形图
//   b 延迟分布 SQL 耗时直方图 + 拟合曲线 + p50 / p99 标线——和产品主题（慢 SQL 治理）直接相关
//   c 点阵    六方晶格，节点大小与亮度随场强变化，强处连键——像分子结构 / 材料晶格
(function () {
  'use strict';
  var NS = 'http://www.w3.org/2000/svg';

  function el(name, attrs, parent) {
    var e = document.createElementNS(NS, name);
    for (var k in attrs) if (attrs[k] != null) e.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(e);
    return e;
  }
  function label(parent, attrs, s) { var t = el('text', attrs, parent); t.textContent = s; return t; }
  function rng(seed) {
    var s = seed >>> 0;
    return function () { s = (Math.imul(s, 1664525) + 1013904223) >>> 0; return s / 4294967296; };
  }
  function r1(n) { return Math.round(n * 10) / 10; }
  function gauss(t, mu, sig) { var d = (t - mu) / sig; return Math.exp(-d * d / 2); }

  // ---- 标量场：几个高斯峰 + 轻微纹波，归一化到 [0,1] ----
  function makeField(w, h) {
    var m = Math.min(w, h);
    var peaks = [[.80, .20, .20, 1], [.18, .52, .17, .78], [.58, .80, .19, .66], [.92, .62, .12, .5], [.36, .10, .13, .46]];
    function raw(x, y) {
      var v = 0;
      for (var i = 0; i < peaks.length; i++) {
        var p = peaks[i], dx = x - p[0] * w, dy = y - p[1] * h, s = p[2] * m;
        v += p[3] * Math.exp(-(dx * dx + dy * dy) / (2 * s * s));
      }
      return v + .035 * Math.sin(x / 41 + y / 57);
    }
    var lo = 1e9, hi = -1e9;
    for (var y = 0; y <= h; y += 20) for (var x = 0; x <= w; x += 20) { var v = raw(x, y); if (v < lo) lo = v; if (v > hi) hi = v; }
    return function (x, y) { return (raw(x, y) - lo) / (hi - lo); };
  }

  // ---- 公共：坐标纸 ----
  function paper(svg, w, h) {
    var defs = el('defs', {}, svg);
    var pm = el('pattern', { id: 'lb-pm', width: 16, height: 16, patternUnits: 'userSpaceOnUse' }, defs);
    el('path', { class: 'lb-minor', d: 'M.5 0V16M0 .5H16' }, pm);
    var pM = el('pattern', { id: 'lb-pM', width: 80, height: 80, patternUnits: 'userSpaceOnUse' }, defs);
    el('path', { class: 'lb-major', d: 'M.5 0V80M0 .5H80' }, pM);
    // 淡出：右上最清晰，左下（压在标题文字后面）最淡
    var lg = el('linearGradient', { id: 'lb-fg', x1: 1, y1: 0, x2: 0, y2: 1 }, defs);
    el('stop', { offset: 0, 'stop-color': '#fff' }, lg);
    el('stop', { offset: .55, 'stop-color': '#fff', 'stop-opacity': .7 }, lg);
    el('stop', { offset: 1, 'stop-color': '#fff', 'stop-opacity': .22 }, lg);
    var mask = el('mask', { id: 'lb-fade', maskUnits: 'userSpaceOnUse', x: 0, y: 0, width: w, height: h }, defs);
    el('rect', { x: 0, y: 0, width: w, height: h, fill: 'url(#lb-fg)' }, mask);
    var g = el('g', { mask: 'url(#lb-fade)' }, svg);
    el('rect', { x: 0, y: 0, width: w, height: h, fill: 'url(#lb-pm)' }, g);
    el('rect', { x: 0, y: 0, width: w, height: h, fill: 'url(#lb-pM)' }, g);
    return g;
  }

  // ---- 公共：上边 / 左边标尺，几处十字配准标记，图注 ----
  function frame(svg, w, h, rnd) {
    var d = '', x, y;
    for (x = 0; x <= w; x += 16) d += 'M' + (x + .5) + ' 0V' + (x % 80 === 0 ? 10 : 5);
    for (y = 0; y <= h; y += 16) d += 'M0 ' + (y + .5) + 'H' + (y % 80 === 0 ? 10 : 5);
    el('path', { class: 'lb-tick', d: d }, svg);
    for (x = 0; x <= w; x += 80) label(svg, { class: 'lb-num', x: x + 3, y: 20 }, String(Math.round(x * 1.25)));
    for (y = 80; y <= h; y += 80) label(svg, { class: 'lb-num', x: 14, y: y + 3 }, (y / 400).toFixed(1));
    label(svg, { class: 'lb-num lb-unit', x: 22, y: 34 }, 't / ms');

    // 十字配准标记：落在大格交点上
    var cr = '';
    for (var i = 0; i < 7; i++) {
      var cx = (1 + Math.floor(rnd() * Math.floor(w / 80 - 1))) * 80 + .5;
      var cy = (1 + Math.floor(rnd() * Math.floor(h / 80 - 1))) * 80 + .5;
      cr += 'M' + (cx - 7) + ' ' + cy + 'H' + (cx + 7) + 'M' + cx + ' ' + (cy - 7) + 'V' + (cy + 7);
      el('circle', { class: 'lb-cross-c', cx: cx, cy: cy, r: 3.2 }, svg);
    }
    el('path', { class: 'lb-cross', d: cr }, svg);

    // 图注（右下）
    label(svg, { class: 'lb-cap', x: w - 28, y: h - 30, 'text-anchor': 'end' }, 'FIG.01  SQL 审核流水线 · REVIEW → MASK → AUDIT');
    label(svg, { class: 'lb-cap lb-cap2', x: w - 28, y: h - 15, 'text-anchor': 'end' }, 'n = 8 engines   Δ = 0.00 ms   σ = 0.07');
  }

  // ---- a：等值线（marching squares）----
  var SEG = {
    1: [['L', 'B']], 2: [['B', 'R']], 3: [['L', 'R']], 4: [['T', 'R']], 5: [['T', 'L'], ['B', 'R']], 6: [['T', 'B']],
    7: [['T', 'L']], 8: [['T', 'L']], 9: [['T', 'B']], 10: [['T', 'R'], ['L', 'B']], 11: [['T', 'R']], 12: [['L', 'R']],
    13: [['B', 'R']], 14: [['L', 'B']]
  };
  function lerp(L, a, b) { return (L - a) / (b - a); }

  function contours(g, w, h, field) {
    var step = 8, nx = Math.ceil(w / step) + 1, ny = Math.ceil(h / step) + 1, v = [], i, j;
    for (j = 0; j < ny; j++) { var row = []; for (i = 0; i < nx; i++) row.push(field(i * step, j * step)); v.push(row); }
    var li = 0;
    for (var L = .08; L < .97; L += .06, li++) {
      var d = '', segs = [], idx = li % 4 === 0;
      for (j = 0; j < ny - 1; j++) {
        for (i = 0; i < nx - 1; i++) {
          var a = v[j][i], b = v[j][i + 1], c = v[j + 1][i + 1], dd = v[j + 1][i];
          var k = (a >= L ? 8 : 0) | (b >= L ? 4 : 0) | (c >= L ? 2 : 0) | (dd >= L ? 1 : 0);
          if (k === 0 || k === 15) continue;
          var x0 = i * step, y0 = j * step, P = {
            T: [x0 + step * lerp(L, a, b), y0], R: [x0 + step, y0 + step * lerp(L, b, c)],
            B: [x0 + step * lerp(L, dd, c), y0 + step], L: [x0, y0 + step * lerp(L, a, dd)]
          };
          var cs = SEG[k];
          for (var s = 0; s < cs.length; s++) {
            var p = P[cs[s][0]], q = P[cs[s][1]];
            d += 'M' + r1(p[0]) + ' ' + r1(p[1]) + 'L' + r1(q[0]) + ' ' + r1(q[1]);
            if (idx) segs.push([p, q]);
          }
        }
      }
      if (!d) continue;
      el('path', { class: idx ? 'lb-iso lb-iso-idx' : 'lb-iso', d: d }, g);
      if (idx && segs.length > 12) {
        var sg = segs[Math.floor(segs.length * .42)], mx = (sg[0][0] + sg[1][0]) / 2, my = (sg[0][1] + sg[1][1]) / 2;
        var ang = Math.atan2(sg[1][1] - sg[0][1], sg[1][0] - sg[0][0]) * 180 / Math.PI;
        if (ang > 90) ang -= 180; else if (ang < -90) ang += 180;
        label(g, { class: 'lb-isolbl', x: r1(mx), y: r1(my), 'text-anchor': 'middle', transform: 'rotate(' + r1(ang) + ' ' + r1(mx) + ' ' + r1(my) + ')' }, L.toFixed(2));
      }
    }
  }

  // ---- b：延迟分布直方图 ----
  // 只占右半幅、底边抬到“说明文字 / 引擎标签”之上：不压文字，也不和右下角图注重叠。
  function histogram(g, w, h, rnd) {
    var x0 = Math.round(w * .46 / 16) * 16, x1 = w - 32, span = x1 - x0;
    var n = 38, bw = span / n, base = h - 128, maxH = Math.min(190, h * .24), i, t;
    function pdf(t) { return gauss(t, .42, .1) + .5 * gauss(t, .62, .09) + .2 * gauss(t, .8, .07) + .05 * gauss(t, .15, .1); }
    var hs = [], top = 0;
    for (i = 0; i < n; i++) {
      t = (i + .5) / n;
      var v = pdf(t) * (.84 + .32 * rnd());
      hs.push(v); if (v > top) top = v;
    }
    var k = maxH / top, p99 = Math.round(n * .84);
    for (i = 0; i < n; i++) {
      var bh = hs[i] * k;
      if (bh < 1) continue;
      el('rect', { class: i >= p99 ? 'lb-bar lb-bar-tail' : 'lb-bar', x: r1(x0 + i * bw + 1), y: r1(base - bh), width: r1(bw - 2), height: r1(bh) }, g);
    }
    // 拟合曲线
    var d = '';
    for (i = 0; i <= 90; i++) {
      t = i / 90;
      d += (i ? 'L' : 'M') + r1(x0 + t * span) + ' ' + r1(base - pdf(t) * k * .98);
    }
    el('path', { class: 'lb-fit', d: d }, g);
    // 横轴（对数刻度）：一大格一个量级
    el('line', { class: 'lb-axis', x1: x0, y1: base + .5, x2: x1, y2: base + .5 }, g);
    ['1 ms', '10', '100', '1 s', '10 s'].forEach(function (s, j) {
      var x = r1(x0 + span * j / 4) + .5;
      el('line', { class: 'lb-axis', x1: x, y1: base, x2: x, y2: base + 6 }, g);
      label(g, { class: 'lb-num', x: x, y: base + 18, 'text-anchor': j === 0 ? 'start' : j === 4 ? 'end' : 'middle' }, s);
    });
    // p50 / p99 标线
    [[.5, 'p50'], [.84, 'p99']].forEach(function (m) {
      var x = r1(x0 + m[0] * span) + .5;
      el('line', { class: 'lb-mark', x1: x, y1: base, x2: x, y2: r1(base - maxH * 1.15) }, g);
      label(g, { class: 'lb-marklbl', x: x + 5, y: r1(base - maxH * 1.15) + 9 }, m[1]);
    });
    label(g, { class: 'lb-num lb-unit', x: x0, y: r1(base - maxH * 1.15) + 9 }, 'SQL 耗时分布 · 对数刻度');
  }

  // ---- c：六方点阵 ----
  function lattice(g, w, h, field) {
    var s = 30, dy = s * Math.sqrt(3) / 2, rows = Math.ceil(h / dy) + 1, cols = Math.ceil(w / s) + 1, pts = [], r, c;
    for (r = 0; r < rows; r++) {
      var rowPts = [];
      for (c = 0; c < cols; c++) {
        var x = c * s + (r % 2 ? s / 2 : 0), y = r * dy;
        rowPts.push({ x: x, y: y, v: field(x, y) });
      }
      pts.push(rowPts);
    }
    var bonds = ['', '', ''], i;
    function bond(p, q) {
      var m = Math.min(p.v, q.v);
      if (m < .3) return;
      var lv = m < .5 ? 0 : m < .72 ? 1 : 2;
      bonds[lv] += 'M' + r1(p.x) + ' ' + r1(p.y) + 'L' + r1(q.x) + ' ' + r1(q.y);
    }
    for (r = 0; r < rows; r++) {
      for (c = 0; c < cols; c++) {
        var p = pts[r][c];
        if (c + 1 < cols) bond(p, pts[r][c + 1]);
        if (r + 1 < rows) {
          var cc = r % 2 ? c + 1 : c - 1, down = pts[r + 1];
          if (c < cols) bond(p, down[c]);
          if (cc >= 0 && cc < cols) bond(p, down[cc]);
        }
      }
    }
    for (i = 0; i < 3; i++) if (bonds[i]) el('path', { class: 'lb-bond lb-bond' + i, d: bonds[i] }, g);
    var dots = [[], [], [], []];
    for (r = 0; r < rows; r++) for (c = 0; c < cols; c++) {
      var q = pts[r][c], lv = q.v < .22 ? 0 : q.v < .45 ? 1 : q.v < .7 ? 2 : 3;
      dots[lv].push(q);
    }
    var rad = [1.1, 1.6, 2.3, 3.2];
    for (i = 0; i < 4; i++) {
      var gg = el('g', { class: 'lb-dot lb-dot' + i }, g);
      dots[i].forEach(function (q) { el('circle', { cx: r1(q.x), cy: r1(q.y), r: rad[i] }, gg); });
    }
  }

  // ---- 数据库关键字层 ----
  // 填空白处：SQL 关键字（按语句类别着色）、几张和产品功能对应的小卡片（SQL / EXPLAIN / 三道关结果）、数据库概念标签，
  // 外加几个只有轮廓的超大幽灵字。位置由“拒绝采样”决定：避开 logo / 时钟 / 标题 / 说明 / 引擎标签 / 拓扑圆 / 图注，
  // 彼此也不重叠；种子固定，每次刷新一致。放不下的（窄屏）自动省略。
  var KW = {
    dql: ['SELECT', 'FROM', 'WHERE', 'JOIN', 'LEFT JOIN', 'GROUP BY', 'ORDER BY', 'HAVING', 'UNION', 'LIMIT 100', 'DISTINCT', 'EXISTS', 'EXPLAIN', 'COUNT(*)', 'WITH', 'IN (…)', 'LIKE'],
    dml: ['INSERT', 'UPDATE', 'DELETE', 'REPLACE', 'ON DUPLICATE KEY', 'UPSERT', 'LOAD DATA'],
    ddl: ['CREATE', 'ALTER', 'DROP', 'TRUNCATE', 'ADD INDEX', 'PRIMARY KEY', 'FOREIGN KEY', 'PARTITION BY'],
    dcl: ['BEGIN', 'COMMIT', 'ROLLBACK', 'SAVEPOINT', 'GRANT', 'REVOKE', 'FOR UPDATE'],
    cc: ['B+Tree', 'MVCC', 'WAL', 'binlog', 'redo log', 'undo log', 'ACID', 'Shard', 'Replica', 'Failover', 'Deadlock', 'Slow Query',
      'Index Scan', 'Full Scan', 'QPS', 'TPS', 'P99', 'buffer pool', 'cardinality', 'rows: 1284', 'type: range', 'key: idx_created']
  };
  // 估算文字宽度：等宽字体一个字符约 .6em，加上 letter-spacing（关键字 .08em、幽灵字 .06em），中文按 1em
  function tokenWidth(s, fs, spacing) {
    var n = 0, adv = .6 + (spacing || 0);
    for (var i = 0; i < s.length; i++) n += s.charCodeAt(i) > 255 ? 1 : adv;
    return n * fs;
  }
  function makePlacer(w, h, avoid, circle) {
    var placed = [];
    function hitRect(a, b, m) { return a.x < b.x + b.w + m && a.x + a.w + m > b.x && a.y < b.y + b.h + m && a.y + a.h + m > b.y; }
    function hitCircle(r, c, m) {
      var nx = Math.max(r.x, Math.min(c.x, r.x + r.w)), ny = Math.max(r.y, Math.min(c.y, r.y + r.h)), dx = nx - c.x, dy = ny - c.y;
      return dx * dx + dy * dy < (c.r + m) * (c.r + m);
    }
    return function (bw, bh, rnd, margin, tries) {
      var rangeX = w - 60 - bw, rangeY = h - 84 - bh;
      if (rangeX <= 0 || rangeY <= 0) return null;
      for (var t = 0; t < (tries || 90); t++) {
        var r = { x: 30 + rnd() * rangeX, y: 46 + rnd() * rangeY, w: bw, h: bh }, bad = false, i;
        for (i = 0; i < avoid.length && !bad; i++) if (hitRect(r, avoid[i], 10)) bad = true;
        if (!bad && circle && hitCircle(r, circle, 10)) bad = true;
        for (i = 0; i < placed.length && !bad; i++) if (hitRect(r, placed[i], margin == null ? 8 : margin)) bad = true;
        if (!bad) { placed.push(r); return r; }
      }
      return null;
    };
  }
  // 用 Range 取文字的“紧”外框（块级元素本身会撑满整行，不能直接用）
  function tightRect(elm, hostR) {
    var rg = document.createRange();
    rg.selectNodeContents(elm);
    var r = rg.getBoundingClientRect();
    return { x: r.left - hostR.left, y: r.top - hostR.top, w: r.width, h: r.height };
  }
  function miniCard(g, r, title, lines) {
    var grp = el('g', { class: 'lk-card' }, g);
    el('rect', { class: 'lk-card-bg', x: r.x, y: r.y, width: r.w, height: r.h, rx: 7 }, grp);
    el('line', { class: 'lk-card-hr', x1: r.x, y1: r.y + 24.5, x2: r.x + r.w, y2: r.y + 24.5 }, grp);
    [0, 1, 2].forEach(function (i) { el('circle', { class: 'lk-card-dot', cx: r.x + 13 + i * 11, cy: r.y + 12.5, r: 3 }, grp); });
    label(grp, { class: 'lk-card-title', x: r.x + r.w - 12, y: r.y + 16, 'text-anchor': 'end' }, title);
    lines.forEach(function (ln, i) {
      var t = el('text', { class: 'lk-code', x: r.x + 14, y: r.y + 45 + i * 18 }, grp);
      ln.forEach(function (seg) { var s = el('tspan', { class: 'lk-t-' + seg[1] }, t); s.textContent = seg[0]; });
    });
    return grp;
  }
  var CARDS = [
    { title: 'online query · 审核通过', lines: [
      [['SELECT ', 'kw'], ['id, status, amount', 'tx']],
      [['FROM   ', 'kw'], ['orders', 'id']],
      [['WHERE  ', 'kw'], ['created_at ', 'tx'], ['>= ', 'op'], ["'2026-10-01'", 'str']],
      [['ORDER BY ', 'kw'], ['id ', 'tx'], ['LIMIT ', 'kw'], ['100', 'num'], [';', 'op']],
      [['-- rows ≈ 1,284 · key: idx_created', 'cm']]] },
    { title: 'EXPLAIN', lines: [
      [['id  type   key          rows  Extra', 'cm']],
      [['1   ', 'num'], ['range  ', 'kw'], ['idx_created  ', 'id'], ['1284  ', 'num'], ['Using index', 'str']],
      [['2   ', 'num'], ['ref    ', 'kw'], ['uk_user_id   ', 'id'], ['1     ', 'num'], ['Using where', 'str']]] },
    { title: '三道关 · 全部通过', lines: [
      [['✓ ', 'ok'], ['审核  ', 'tx'], ['高危语句 0 · 规范告警 1', 'cm']],
      [['✓ ', 'ok'], ['脱敏  ', 'tx'], ['phone, id_card', 'id']],
      [['✓ ', 'ok'], ['审计  ', 'tx'], ['已记录 · 来源 10.2.8.31', 'cm']]] }
  ];
  function keywords(svg, w, h, rnd, host, pane) {
    var hostR = host.getBoundingClientRect(), avoid = [], circle = null, g = el('g', { class: 'lk' }, svg);
    if (pane) {
      var card = host.querySelector('.login-card'), tools = host.querySelector('.tools');
      [card, tools].forEach(function (e) { // 取元素盒子（含内边距和边框），不用文字外框，否则小词会被卡片边缘盖住一半
        if (!e) return;
        var b = e.getBoundingClientRect();
        avoid.push({ x: b.left - hostR.left, y: b.top - hostR.top, w: b.width, h: b.height });
      });
    } else {
      ['.lg', '.login-clock', '.kicker', 'h2', '.login-brand p', '.engines'].forEach(function (sel) {
        var e = host.querySelector(sel);
        if (e) avoid.push(tightRect(e, hostR));
      });
      var topo = host.querySelector('#login-topo');
      if (topo) {
        var t = topo.getBoundingClientRect(), s = Math.min(t.width, t.height) / 640;
        circle = { x: t.left - hostR.left + t.width / 2, y: t.top - hostR.top + t.height / 2, r: 292 * s + 16 };
      }
      avoid.push({ x: w - 440, y: h - 52, w: 420, h: 44 }); // 右下图注
    }
    var place = makePlacer(w, h, avoid, circle), i, r;

    // 层次：拓扑是主角。越靠近它的元素越淡（贴边处 40%，离开约一个半径后恢复满值），整体再乘 .lk 的总透明度。
    function wrap(cx, cy) {
      var o = 1;
      if (circle) {
        var dx = cx - circle.x, dy = cy - circle.y;
        o = .4 + .6 * Math.max(0, Math.min(1, (Math.sqrt(dx * dx + dy * dy) - circle.r) / (circle.r * .9)));
      }
      return el('g', { opacity: r1(o * 100) / 100 }, g);
    }
    if (circle) { // 拓扑后面一圈很柔的聚光，把视线留给它
      var defs = el('defs', {}, svg), spotR = r1(circle.r * 1.4);
      var rg = el('radialGradient', { id: 'lk-spot', gradientUnits: 'userSpaceOnUse', cx: r1(circle.x), cy: r1(circle.y), r: spotR }, defs);
      el('stop', { offset: 0, class: 'lk-spot0' }, rg);
      el('stop', { offset: 1, class: 'lk-spot1' }, rg);
      svg.insertBefore(el('circle', { cx: r1(circle.x), cy: r1(circle.y), r: spotR, fill: 'url(#lk-spot)' }), g);
    }

    // 0) 小卡片最先放（最需要整块空地）：SQL / EXPLAIN / 三道关；登录区不放
    if (!pane) CARDS.forEach(function (c) {
      var longest = 0;
      c.lines.forEach(function (ln) { var s = ''; ln.forEach(function (sg) { s += sg[0]; }); longest = Math.max(longest, tokenWidth(s, 11.5)); });
      var cw = Math.ceil(longest + 30), ch = 24 + 14 + c.lines.length * 18 + 4;
      r = place(cw, ch, rnd, 16, 400);
      if (r) miniCard(wrap(r.x + cw / 2, r.y + ch / 2), { x: r1(r.x), y: r1(r.y), w: cw, h: ch }, c.title, c.lines);
    });

    // 1) 幽灵大字：只有轮廓，给空旷处一点层次
    (pane ? [['SELECT', 70, 0], ['COMMIT', 56, 0]] : [['SELECT', 74, 0], ['INDEX', 60, 0], ['EXPLAIN', 44, 90], ['JOIN', 64, 0], ['COMMIT', 52, 0]]).forEach(function (gh) {
      var fs = gh[1], tw = tokenWidth(gh[0], fs, .06), vert = gh[2] === 90;
      r = place(vert ? fs * 1.1 : tw, vert ? tw : fs * 1.1, rnd, 18, 200);
      if (!r) return;
      var tx = vert ? r.x + fs * .85 : r.x, ty = vert ? r.y : r.y + fs * .85;
      label(wrap(r.x + r.w / 2, r.y + r.h / 2), { class: 'lk-ghost', x: r1(tx), y: r1(ty), 'font-size': fs, transform: vert ? 'rotate(90 ' + r1(tx) + ' ' + r1(ty) + ')' : null }, gh[0]);
    });
    if (pane) {
      // 登录区只放几个很淡的小词，不干扰表单
      for (i = 0; i < 9; i++) {
        var cat = ['dql', 'dml', 'ddl', 'dcl', 'cc'][i % 5], pool = KW[cat], word = pool[Math.floor(rnd() * pool.length)], fs = 11;
        r = place(tokenWidth(word, fs, cat === 'cc' ? .02 : .08) + 4, fs + 6, rnd, 30);
        if (r) label(g, { class: 'lk-w lk-c-' + cat + ' lk-o1', x: r1(r.x), y: r1(r.y + fs + 1), 'font-size': fs }, word);
      }
      return;
    }

    // 3) 概念标签（带框，和左下的引擎标签呼应）
    for (i = 0; i < 9; i++) {
      var word2 = KW.cc[Math.floor(rnd() * KW.cc.length)], fs2 = 11, bw = Math.ceil(tokenWidth(word2, fs2) + 18);
      r = place(bw, 22, rnd, 10);
      if (!r) continue;
      var cg = wrap(r.x + bw / 2, r.y + 11);
      el('rect', { class: 'lk-chip', x: r1(r.x) + .5, y: r1(r.y) + .5, width: bw, height: 21, rx: 4 }, cg);
      label(cg, { class: 'lk-chip-t', x: r1(r.x + bw / 2), y: r1(r.y + 15), 'text-anchor': 'middle', 'font-size': fs2 }, word2);
    }

    // 4) 关键字散点：按类别着色、三档透明度、字号错落
    var cats = ['dql', 'dql', 'dql', 'dml', 'ddl', 'dcl', 'dcl', 'cc'], sizes = [11, 12, 13, 15, 12, 11];
    for (i = 0; i < 70; i++) {
      var cat2 = cats[Math.floor(rnd() * cats.length)], pool2 = KW[cat2], word3 = pool2[Math.floor(rnd() * pool2.length)];
      var fs3 = sizes[Math.floor(rnd() * sizes.length)], tw3 = tokenWidth(word3, fs3, cat2 === 'cc' ? .02 : .08);
      r = place(tw3 + 4, fs3 + 6, rnd, 14, 40);
      if (!r) continue;
      var o = rnd(), e = label(wrap(r.x + r.w / 2, r.y + r.h / 2), { class: 'lk-w lk-c-' + cat2 + (o < .4 ? ' lk-o1' : o < .8 ? ' lk-o2' : ' lk-o3'), x: r1(r.x), y: r1(r.y + fs3 + 1), 'font-size': fs3 }, word3);
      e.style.animationDelay = (-rnd() * 8).toFixed(1) + 's';
    }
  }

  // ---- 入口 ----
  function build(svg, w, h, variant, withKw) {
    svg.textContent = '';
    svg.setAttribute('viewBox', '0 0 ' + w + ' ' + h);
    svg.setAttribute('width', w);
    svg.setAttribute('height', h);
    var rnd = rng(20261012);
    if (variant !== '0') {
      var field = makeField(w, h), g = paper(svg, w, h);
      if (variant === 'a') contours(g, w, h, field);
      else if (variant === 'b') histogram(g, w, h, rnd);
      else lattice(g, w, h, field);
      frame(svg, w, h, rng(7));
    }
    if (withKw) keywords(svg, w, h, rng(42), host, false);
  }
  function buildPane(svg2, pane, withKw) {
    var w = pane.clientWidth, h = pane.clientHeight;
    svg2.textContent = '';
    svg2.setAttribute('viewBox', '0 0 ' + w + ' ' + h);
    svg2.setAttribute('width', w);
    svg2.setAttribute('height', h);
    if (withKw) keywords(svg2, w, h, rng(99), pane, true);
  }

  var q = new URLSearchParams(location.search);
  var root = document.documentElement;
  var host = document.querySelector('.login-brand');
  var svg = document.getElementById('login-bg');
  var pane = document.querySelector('.login-pane');
  var svg2 = document.getElementById('login-bg2');
  var state = { v: q.get('v') || '0', skin: q.get('skin') || 'dark', k: q.get('k') !== '0' };

  function paint() {
    root.setAttribute('data-skin', state.skin);
    root.classList.toggle('has-paper', state.v !== '0');
    build(svg, host.clientWidth, host.clientHeight, state.v, state.k);
    if (svg2) buildPane(svg2, pane, state.k);
    document.querySelectorAll('[data-v]').forEach(function (b) { b.setAttribute('aria-pressed', String(b.dataset.v === state.v)); });
    document.querySelectorAll('[data-s]').forEach(function (b) { b.setAttribute('aria-pressed', String(b.dataset.s === state.skin)); });
    document.querySelectorAll('[data-k]').forEach(function (b) { b.setAttribute('aria-pressed', String(state.k)); });
  }
  document.querySelectorAll('[data-k]').forEach(function (b) { b.addEventListener('click', function () { state.k = !state.k; paint(); }); });
  document.querySelectorAll('[data-v]').forEach(function (b) { b.addEventListener('click', function () { state.v = b.dataset.v; paint(); }); });
  document.querySelectorAll('[data-s]').forEach(function (b) { b.addEventListener('click', function () { state.skin = b.dataset.s; paint(); }); });
  if (q.has('shot')) document.querySelector('.proto-bar').hidden = true;
  // 按容器尺寸重画（而不是 window.resize）：字体 / 布局稳定后尺寸才最终确定，背景必须与面板同尺寸，不然会被等比缩放出黑边。
  var timer = 0, lastW = 0, lastH = 0;
  new ResizeObserver(function () {
    if (host.clientWidth === lastW && host.clientHeight === lastH) return;
    lastW = host.clientWidth; lastH = host.clientHeight;
    clearTimeout(timer);
    timer = setTimeout(paint, 30);
  }).observe(host);
  var clock = document.getElementById('login-clock');
  if (clock) clock.textContent = '2026-10-12 19:27:20';
  paint();
})();
