/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: mysql.js
 * @time: 2026/10/28
 */
// “我的 SQL”面板：收藏（每人自己的）+ 执行历史（用户看自己的；超级用户看所有人的，可模糊按用户和语句过滤）。
// 所有内容都用 textContent / DOM 节点拼出来，SQL 文本不会被当成 HTML。
import { api } from '../common.js';
import { t, tp } from '../i18n.js';

const $ = (id) => document.getElementById(id);
const PAGE = 20;
const STATUS_TEXT = { blocked: t('被拦截'), failed: t('失败'), timeout: t('超时'), canceled: t('已取消') };

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// 把关键词在 SQL 里标出来。
function highlight(target, text, keywords) {
  const ks = keywords.filter(Boolean).map((k) => k.toLowerCase());
  if (!ks.length) { target.textContent = text; return; }
  const lower = text.toLowerCase();
  const marks = [];
  ks.forEach((k) => {
    for (let i = lower.indexOf(k); i >= 0; i = lower.indexOf(k, i + k.length)) marks.push([i, i + k.length]);
  });
  marks.sort((a, b) => a[0] - b[0]);
  let pos = 0;
  marks.forEach(([s, e]) => {
    if (s < pos) return;
    if (s > pos) target.append(document.createTextNode(text.slice(pos, s)));
    const m = document.createElement('mark');
    m.textContent = text.slice(s, e);
    target.append(m);
    pos = e;
  });
  if (pos < text.length) target.append(document.createTextNode(text.slice(pos)));
}

// SQL 单元格：最多显示几行，长的可以展开；双击粘贴到编辑器。
function sqlCell(sql, keywords, onPaste) {
  const td = el('td', 'my-sql');
  const pre = el('pre', 'mono');
  highlight(pre, sql, keywords);
  td.append(pre);
  td.addEventListener('dblclick', () => { getSelection().removeAllRanges(); onPaste(); });
  if (sql.length > 160 || sql.split('\n').length > 3) {
    const more = el('button', 'btn ghost my-more', t('展开'));
    more.type = 'button';
    more.addEventListener('click', () => {
      const open = pre.classList.toggle('open');
      more.textContent = open ? t('收起') : t('展开');
    });
    td.append(more);
  }
  return td;
}

function actBtn(row, text, fn, cls = '') {
  const b = el('button', 'btn ghost my-act' + (cls ? ' ' + cls : ''), text);
  b.type = 'button';
  b.addEventListener('click', fn);
  row.append(b);
  return b;
}

function pagerText(total, offset) {
  const pages = Math.max(1, Math.ceil(total / PAGE));
  const page = Math.floor(offset / PAGE) + 1;
  return { text: t('共 {total} 条，第 {page} / {pages} 页', { total, page, pages }), first: page <= 1, last: page >= pages };
}

function emptyRow(cols, text) {
  const tr = el('tr', 'my-empty');
  const td = el('td', '', text);
  td.colSpan = cols;
  tr.append(td);
  return tr;
}

// ctx 由 app.js 提供：
//   context()        → { sql, instance, db }  当前编辑器里选中的文本 / 光标所在语句，以及当前实例名和库
//   paste(sql)       把 SQL 插入当前编辑器光标处
//   newTab(sql, instance, db)  放到新标签（尽量带上当时的实例和库）
export function initMySQL(ctx) {
  const panel = $('mysql');
  if (!panel) return;

  let tab = 'fav';
  let isAdmin = false;
  let usersLoaded = false;

  // ---------------- 收藏 ----------------
  const fav = { offset: 0, seq: 0, sort: 'used', asc: false, editing: 0 };

  function favForm(show, init) {
    const f = $('fav-edit');
    f.hidden = !show;
    if (!show) return;
    fav.editing = init.id || 0;
    $('fav-edit-title').textContent = init.id ? t('编辑收藏') : t('添加收藏');
    $('fav-edit-comment').value = init.comment || '';
    $('fav-edit-sql').value = init.sql || '';
    f.dataset.instance = init.instance || '';
    f.dataset.db = init.db || '';
    $('fav-edit-msg').textContent = '';
    (init.sql ? $('fav-edit-comment') : $('fav-edit-sql')).focus();
  }

  async function loadFav(reset) {
    if (reset) fav.offset = 0;
    const my = ++fav.seq;
    const q = $('fav-q').value.trim();
    const params = new URLSearchParams({ offset: fav.offset, limit: PAGE, sort: fav.sort, order: fav.asc ? 'asc' : 'desc' });
    if (q) params.set('q', q);
    const body = $('fav-rows');
    body.replaceChildren(emptyRow(5, t('加载中…')));
    try {
      const pg = await api('GET', '/api/v1/query/favorites?' + params.toString());
      if (my !== fav.seq) return;
      body.replaceChildren();
      if (!pg.items.length) body.append(emptyRow(5, q ? t('没有匹配的收藏') : t('没有数据')));
      const kws = q.split(/\s+/);
      pg.items.forEach((it) => body.append(favRow(it, kws)));
      const p = pagerText(pg.total, fav.offset);
      $('fav-count').textContent = p.text;
      $('fav-prev').disabled = p.first;
      $('fav-next').disabled = p.last;
    } catch (e) {
      if (my !== fav.seq) return;
      const tr = el('tr', 'my-empty');
      const td = el('td', 'alert err', e.message);
      td.colSpan = 5;
      tr.append(td);
      body.replaceChildren(tr);
    }
  }

  async function useFav(it) {
    ctx.paste(it.sql);
    try { await api('POST', `/api/v1/query/favorites/${it.id}/use`); loadFav(false); } catch (_) { /* 记次数失败不影响使用 */ }
  }

  function favRow(it, kws) {
    const tr = el('tr');
    tr.append(el('td', 'my-note', it.comment || '—'));
    tr.append(sqlCell(it.sql, kws, () => useFav(it)));
    tr.append(el('td', 'my-time mono', it.created_at));
    tr.append(el('td', 'my-time mono', it.last_used_at || '—'));
    const td = el('td', 'my-ops');
    actBtn(td, t('使用'), () => useFav(it));
    actBtn(td, t('编辑'), () => favForm(true, { id: it.id, comment: it.comment, sql: it.sql, instance: it.instance, db: it.db }));
    const del = actBtn(td, t('删除'), async () => {
      if (!del.dataset.sure) {
        del.dataset.sure = '1';
        del.textContent = t('确认删除');
        setTimeout(() => { delete del.dataset.sure; del.textContent = t('删除'); }, 3000);
        return;
      }
      try { await api('DELETE', `/api/v1/query/favorites/${it.id}`); loadFav(false); } catch (e) { del.textContent = e.message; }
    }, 'danger');
    tr.append(td);
    return tr;
  }

  $('fav-add').addEventListener('click', () => {
    const c = ctx.context();
    favForm(true, { sql: c.sql, instance: c.instance, db: c.db });
  });
  $('fav-edit-cancel').addEventListener('click', () => favForm(false));
  $('fav-edit').addEventListener('submit', async (e) => {
    e.preventDefault();
    const f = $('fav-edit');
    const body = { comment: $('fav-edit-comment').value, sql: $('fav-edit-sql').value, instance: f.dataset.instance || '', db: f.dataset.db || '' };
    if (!body.sql.trim()) { $('fav-edit-msg').textContent = t('SQL 不能为空'); return; }
    try {
      if (fav.editing) await api('PUT', `/api/v1/query/favorites/${fav.editing}`, body);
      else {
        const r = await api('POST', '/api/v1/query/favorites', body);
        if (r.existed) {
          const q = $('fav-q');
          const ph = q.placeholder;
          q.placeholder = t('已收藏过，已更新注释');
          setTimeout(() => { q.placeholder = ph; }, 4000);
        }
      }
      favForm(false);
      loadFav(true);
    } catch (err) { $('fav-edit-msg').textContent = err.message; }
  });
  $('fav-form').addEventListener('submit', (e) => { e.preventDefault(); loadFav(true); });
  $('fav-prev').addEventListener('click', () => { fav.offset = Math.max(0, fav.offset - PAGE); loadFav(false); });
  $('fav-next').addEventListener('click', () => { fav.offset += PAGE; loadFav(false); });
  document.querySelectorAll('[data-fav-sort]').forEach((th) => th.addEventListener('click', () => {
    const key = th.dataset.favSort;
    if (fav.sort === key) fav.asc = !fav.asc; else { fav.sort = key; fav.asc = false; }
    document.querySelectorAll('[data-fav-sort] .arrow').forEach((a) => { a.textContent = ''; });
    th.querySelector('.arrow').textContent = fav.asc ? '↑' : '↓';
    loadFav(true);
  }));

  // ---------------- 执行历史 ----------------
  const hist = { offset: 0, seq: 0 };

  async function loadUsers() {
    if (usersLoaded) return;
    usersLoaded = true;
    try {
      const us = await api('GET', '/api/v1/query/history/users');
      const dl = $('hist-users');
      us.forEach((u) => {
        const o = document.createElement('option');
        o.value = u.username;
        o.label = t('{name} · {n} 条', { name: u.display || u.username, n: u.count });
        dl.append(o);
      });
    } catch (_) { usersLoaded = false; }
  }

  async function loadHist(reset) {
    if (reset) hist.offset = 0;
    const my = ++hist.seq;
    const q = $('hist-q').value.trim();
    const params = new URLSearchParams({ offset: hist.offset, limit: PAGE });
    if (q) params.set('q', q);
    if ($('hist-inst').value) params.set('instance', $('hist-inst').value);
    if (!$('hist-user').hidden && $('hist-user').value.trim()) params.set('user', $('hist-user').value.trim());
    if (!$('hist-status').hidden && $('hist-status').value) params.set('status', $('hist-status').value);
    if ($('hist-from').value) params.set('start', $('hist-from').value);
    if ($('hist-to').value) params.set('end', $('hist-to').value);
    const body = $('hist-rows');
    body.replaceChildren(emptyRow(8, t('加载中…')));
    try {
      const pg = await api('GET', '/api/v1/query/history?' + params.toString());
      if (my !== hist.seq) return;
      if (pg.admin && !isAdmin) { isAdmin = true; applyAdmin(); loadUsers(); }
      $('hist-status').hidden = !pg.events;
      $('hist-scope').textContent = tp(pg.scope);
      body.replaceChildren();
      if (!pg.items.length) body.append(emptyRow(8, q ? t('没有匹配的执行记录') : t('没有数据')));
      const kws = q.split(/\s+/);
      pg.items.forEach((it) => body.append(histRow(it, kws, pg.admin)));
      const p = pagerText(pg.total, hist.offset);
      $('hist-count').textContent = p.text;
      $('hist-prev').disabled = p.first;
      $('hist-next').disabled = p.last;
    } catch (e) {
      if (my !== hist.seq) return;
      const tr = el('tr', 'my-empty');
      const td = el('td', 'alert err', e.message);
      td.colSpan = 8;
      tr.append(td);
      body.replaceChildren(tr);
    }
  }

  function applyAdmin() {
    $('hist-user').hidden = false;
    $('hist-user-th').hidden = false;
    document.querySelectorAll('#hist-rows td.my-user-cell').forEach((c) => { c.hidden = false; });
  }

  function histRow(it, kws, admin) {
    const status = it.status || 'ok';
    const tr = el('tr', status !== 'ok' ? 'bad' : '');
    tr.append(el('td', 'my-time mono', it.time));
    const u = el('td', 'my-user-cell', it.display ? t('{name}（{user}）', { name: it.display, user: it.username }) : it.username);
    u.hidden = !admin;
    tr.append(u);
    tr.append(el('td', 'my-inst', `${it.instance}${it.db ? ' / ' + it.db : ''}`));
    const sqlTd = sqlCell(it.sql, kws, () => ctx.paste(it.sql));
    if (it.reason) sqlTd.append(el('div', 'my-reason', t('原因：{reason}', { reason: tp(it.reason) })));
    tr.append(sqlTd);
    tr.append(el('td', 'num', status === 'ok' ? String(it.rows) : '—'));
    tr.append(el('td', 'num', `${it.cost_sec} s`));
    const st = el('td');
    st.append(el('span', 'st-badge ' + status, status === 'ok' ? t('成功') : STATUS_TEXT[status] || status));
    tr.append(st);
    const ops = el('td', 'my-ops');
    actBtn(ops, t('粘贴'), () => ctx.paste(it.sql));
    actBtn(ops, t('新标签'), () => ctx.newTab(it.sql, it.instance, it.db));
    actBtn(ops, t('收藏|action'), () => {
      setTab('fav');
      favForm(true, { sql: it.sql, instance: it.instance, db: it.db });
    });
    actBtn(ops, t('复制'), async (ev) => {
      try { await navigator.clipboard.writeText(it.sql); ev.target.textContent = t('已复制'); } catch (_) { ev.target.textContent = t('复制失败'); }
      setTimeout(() => (ev.target.textContent = t('复制')), 1500);
    });
    tr.append(ops);
    return tr;
  }

  $('hist-form').addEventListener('submit', (e) => { e.preventDefault(); loadHist(true); });
  ['hist-inst', 'hist-status', 'hist-from', 'hist-to'].forEach((id) => $(id).addEventListener('change', () => loadHist(true)));
  $('hist-reset').addEventListener('click', () => { $('hist-form').reset(); loadHist(true); });
  $('hist-prev').addEventListener('click', () => { hist.offset = Math.max(0, hist.offset - PAGE); loadHist(false); });
  $('hist-next').addEventListener('click', () => { hist.offset += PAGE; loadHist(false); });

  // ---------------- 标签与面板 ----------------
  function setTab(t) {
    tab = t;
    ['fav', 'hist'].forEach((k) => {
      const on = k === t;
      $('my-tab-' + k).classList.toggle('on', on);
      $('my-tab-' + k).setAttribute('aria-selected', String(on));
      $('my-pane-' + k).hidden = !on;
    });
    $('my-scope').textContent = t === 'fav' ? '' : $('hist-scope').textContent;
    if (t === 'fav') loadFav(true); else loadHist(true);
  }

  function open(t) {
    panel.hidden = false;
    $('hist-btn').setAttribute('aria-expanded', 'true');
    setTab(t || tab);
  }
  function close() {
    panel.hidden = true;
    $('hist-btn').setAttribute('aria-expanded', 'false');
  }

  $('hist-btn').addEventListener('click', () => (panel.hidden ? open() : close()));
  $('my-close').addEventListener('click', close);
  $('my-tab-fav').addEventListener('click', () => setTab('fav'));
  $('my-tab-hist').addEventListener('click', () => setTab('hist'));
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !panel.hidden) close(); });
  // 历史范围说明在拿到数据后才有：更新顶部的范围提示
  new MutationObserver(() => { if (tab === 'hist') $('my-scope').textContent = $('hist-scope').textContent; })
    .observe($('hist-scope'), { childList: true, characterData: true, subtree: true });
}
