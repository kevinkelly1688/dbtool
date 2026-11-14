/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: p_help.js
 * @time: 2026/10/28
 */
/* 使用手册：内容来自 help/*.md（唯一来源），由 tools/build_help.py 打包进 js/help_content.js，
   这样直接双击 index.html（file://）也能阅读。顶栏「? 帮助」打开当前页面对应章节（boot.js：go('help', NAV[页面].help)）。
   章节 id 与 perm.js 里 NAV_TREE 各菜单的 help 字段一一对应。 */
(function () {
  'use strict';

  const HELP_TOPICS = [
    {g: '入门', id: 'quickstart', t: '快速入门'},
    {g: '入门', id: 'login', t: '登录、两步验证与账号安全'},
    {g: 'SQL 审核与优化', id: 'sqlworkflow', t: 'SQL 上线（工单）'},
    {g: 'SQL 审核与优化', id: 'optimize', t: 'SQL 分析与优化工具'},
    {g: 'SQL 查询', id: 'query', t: '在线查询'},
    {g: 'SQL 查询', id: 'slowlog', t: '查慢日志与慢查日志'},
    {g: 'SQL 查询', id: 'partman', t: '分区维护'},
    {g: 'SQL 查询', id: 'archive', t: '数据归档'},
    {g: 'SQL 查询', id: 'export', t: '数据导出'},
    {g: 'SQL 查询', id: 'dict', t: '数据字典'},
    {g: 'SQL 查询', id: 'qpriv', t: '权限申请与查询权限'},
    {g: '性能与实例', id: 'perf', t: '性能工单'},
    {g: '性能与实例', id: 'instance', t: '实例管理'},
    {g: '性能与实例', id: 'inspect', t: '性能巡检'},
    {g: '系统管理', id: 'sysadmin', t: '系统管理'},
    {g: '系统管理', id: 'btnperm', t: '按钮级权限'},
    {g: '附录', id: 'permcodes', t: '权限码清单'},
    {g: '附录', id: 'faq', t: '常见问题'},
  ];

  const HP = {q: ''};
  const helpMd = () => window.HELP_MD || {};

  /* 行内：`code`、**粗体**、==高亮==、[文字](#help/章节) 与 http(s) 链接；其余一律转义 */
  function mdInline(s) {
    const codes = [];
    let h = s.replace(/`([^`]+)`/g, (m, c) => { codes.push(`<code>${esc(c)}</code>`); return '\u0000' + (codes.length - 1) + '\u0000'; });
    h = esc(h).replace(/\*\*(.+?)\*\*/g, '<b>$1</b>').replace(/==(.+?)==/g, '<mark>$1</mark>');
    h = h.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, t, u) => {
      u = u.replace(/&amp;/g, '&'); let x;
      if ((x = /^#(.+)$/.exec(u))) return `<a href="#${esc(x[1])}">${t}</a>`;
      if (/^https?:\/\//.test(u)) return `<a href="${esc(u)}" target="_blank" rel="noopener">${t}</a>`;
      return t;
    });
    return h.replace(/\u0000(\d+)\u0000/g, (m, i) => codes[+i]);
  }

  /* 块级：标题（## 进目录）、表格、列表、> 提示块（⚠ / 注意 开头为警告）、``` 代码块 */
  function mdRender(src) {
    const L = src.replace(/\r\n?/g, '\n').split('\n'), out = [], toc = [], para = [];
    let i = 0, hn = 0;
    const flush = () => { if (para.length) { out.push(`<p>${mdInline(para.join(' '))}</p>`); para.length = 0; } };
    const cells = l => { let s = l.trim(); if (s[0] === '|') s = s.slice(1); if (s.endsWith('|')) s = s.slice(0, -1); return s.split('|').map(c => c.trim()); };
    const isItem = l => /^\s*([-*]|\d+\.)\s+/.test(l);
    while (i < L.length) {
      const ln = L[i]; let m;
      if (/^```/.test(ln)) {
        flush(); const lang = ln.replace(/^```\s*/, '').trim().toLowerCase(), b = []; i++;
        while (i < L.length && !/^```/.test(L[i])) b.push(L[i++]);
        i++; out.push(code(b.join('\n'), lang === 'sql' ? 'sql' : undefined)); continue;
      }
      if (!ln.trim()) { flush(); i++; continue; }
      if ((m = /^(#{1,4})\s+(.*)$/.exec(ln))) {
        flush(); const lv = m[1].length, id = 'h' + (++hn);
        if (lv === 2) toc.push({id, t: m[2].replace(/[`*]/g, '')});
        out.push(lv === 1 ? `<h1>${mdInline(m[2])}</h1>` : `<h${lv + 1} id="${id}">${mdInline(m[2])}</h${lv + 1}>`); i++; continue;
      }
      if (/^\s*\|/.test(ln) && L[i + 1] && /^\s*\|?\s*:?-{2,}/.test(L[i + 1])) {
        flush(); const head = cells(ln); i += 2; const rows = [];
        while (i < L.length && /^\s*\|/.test(L[i])) rows.push(cells(L[i++]));
        out.push(`<div class="tbl-wrap" style="margin:8px 0"><table><thead><tr>${head.map(c => `<th>${mdInline(c)}</th>`).join('')}</tr></thead><tbody>${rows.map(r => `<tr>${head.map((_, k) => `<td>${mdInline(r[k] || '')}</td>`).join('')}</tr>`).join('')}</tbody></table></div>`);
        continue;
      }
      if (/^>/.test(ln)) {
        flush(); const b = []; while (i < L.length && /^>/.test(L[i])) b.push(L[i++].replace(/^>\s?/, ''));
        const t = b.join(' '); out.push(`<div class="tip ${/^(⚠|注意|警告)/.test(t) ? 'warn' : ''}">${mdInline(t)}</div>`); continue;
      }
      if (isItem(ln)) {
        /* 列表：支持一层缩进（行首 ≥ 2 个空格）的子项，挂在上一条顶层项下 */
        flush(); const raw = [];
        while (i < L.length && isItem(L[i])) raw.push(L[i++]);
        const its = raw.map(l => { const m = /^(\s*)([-*]|\d+\.)\s+(.*)$/.exec(l); return {ind: m[1].length, ol: /\d/.test(m[2]), t: m[3]}; });
        const base = Math.min(...its.map(x => x.ind)), top = [];
        its.forEach(x => { if (x.ind - base >= 2 && top.length) top[top.length - 1].kids.push(x); else top.push({...x, kids: []}); });
        const wrap = (a, h) => { const tag = a[0].ol ? 'ol' : 'ul'; return `<${tag}>${h}</${tag}>`; };
        out.push(wrap(top, top.map(x => `<li>${mdInline(x.t)}${x.kids.length ? wrap(x.kids, x.kids.map(k => `<li>${mdInline(k.t)}</li>`).join('')) : ''}</li>`).join(''))); continue;
      }
      para.push(ln.trim()); i++;
    }
    flush();
    return {html: out.join('\n'), toc};
  }

  const reEsc = s => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

  /* 全文搜索：逐行匹配（忽略大小写），命中处高亮；先转义再高亮，避免把用户输入当 HTML */
  function searchHits(q) {
    const ql = q.toLowerCase(), hits = [], md = helpMd();
    HELP_TOPICS.forEach(t => (md[t.id] || '').split('\n').forEach(l => {
      if (l.toLowerCase().includes(ql) && !/^```/.test(l) && hits.length < 80) hits.push({t, l});
    }));
    return hits;
  }
  function hitHtml(h, q) {
    const text = h.l.replace(/^[#>|\-*\s]+/, '').replace(/\|/g, ' · ');
    return mdInline(text).replace(new RegExp('(' + reEsc(esc(q)) + ')(?![^<]*>)', 'gi'), '<mark>$1</mark>');
  }

  const navHtml = cur => {
    let g = '';
    return HELP_TOPICS.map(t => {
      const s = (t.g !== g ? `<div class="g">${t.g}</div>` : '') + `<a href="#help/${t.id}" data-on="hpClear" class="${t.id === cur ? 'on' : ''}">${t.t}</a>`;
      g = t.g; return s;
    }).join('');
  };

  pages.help = {
    title: '使用手册',
    render(sub) {
      const id = HELP_TOPICS.some(t => t.id === sub) ? sub : 'quickstart';
      const nav = navHtml(HP.q ? '' : id);
      const box = `<input type="search" id="hpQ" placeholder="搜索手册（回车）" value="${esc(HP.q)}" style="width:100%;margin-bottom:8px">`;
      if (HP.q) {
        const hits = searchHits(HP.q);
        return `<div class="help"><div class="card topics">${box}${nav}</div>
          <div class="card hp"><h1>搜索「${esc(HP.q)}」 <span class="small muted">${hits.length} 条${hits.length >= 80 ? '（仅显示前 80 条）' : ''}</span>
            <button class="btn sm" data-on="hpClear" style="float:right">清除搜索</button></h1>
            ${hits.map(h => `<div style="padding:6px 0;border-bottom:1px solid var(--border)"><a href="#help/${h.t.id}" data-on="hpClear">${h.t.t}</a><div class="small">${hitHtml(h, HP.q)}</div></div>`).join('') || '<div class="muted">没有找到相关内容，换个关键词试试。</div>'}</div></div>`;
      }
      const src = helpMd()[id];
      const r = src ? mdRender(src) : {html: '<div class="notice warn"><span>!</span><div>未找到手册内容：请运行 <code>python tools/build_help.py</code> 生成 js/help_content.js。</div></div>', toc: []};
      return `<div class="help"><div class="card topics">${box}${nav}<div class="hr"></div><a href="docs/用户使用手册.md" target="_blank">⇩ 完整手册（Markdown）</a></div>
        <div class="card hp">${r.html}</div>
        <div class="toc">${r.toc.length ? '<div class="small b" style="margin-bottom:4px">本页目录</div>' + r.toc.map(t => `<a data-on="hpToc" data-id="${t.id}" style="cursor:pointer">${esc(t.t)}</a>`).join('') : ''}</div></div>`;
    },
    init() {
      const q = $('#hpQ');
      if (!q) return;
      q.onchange = () => { HP.q = q.value.trim(); rerender(); };
      if (HP.q) { q.focus(); q.setSelectionRange(q.value.length, q.value.length); }
    }
  };

  ON.hpToc = d => { const el = document.getElementById(d.id); if (el) el.scrollIntoView({behavior: 'smooth', block: 'start'}); };
  /* 点章节链接 / 搜索结果 / 清除按钮：退出搜索态；在当前章节上点则直接重绘 */
  ON.hpClear = () => { if (HP.q) { HP.q = ''; rerender(); } };
})();
