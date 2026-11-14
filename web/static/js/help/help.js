/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: help.js
 * @time: 2026/10/28
 */
// 使用文档页：左侧目录 + 正文 + 右侧本页目录 + 全文搜索。
// 章节内容是 /static/help/<id>.md；地址 /help#help/<章节> 或 /help#<章节> 打开对应章节，正文里的站内链接 #help/<章节> 同样可跳转。
import { mdRender, mdInline, esc } from './md.js';
import { t } from '../i18n.js';

const root = document.getElementById('help');
if (root) init(root);

function init(root) {
  const base = root.dataset.base || '/static/help/';
  const body = document.getElementById('help-body');
  const tocBox = document.getElementById('help-toc');
  const nav = document.getElementById('help-nav');
  const input = document.getElementById('help-q');
  const topics = [...nav.querySelectorAll('a[data-topic]')].map((a) => ({ id: a.dataset.topic, title: a.textContent.trim() }));
  const known = new Set(topics.map((t) => t.id));
  const cache = new Map(); // id -> Promise<string>
  let q = '';
  let seq = 0; // 丢弃过期的异步渲染

  function load(id) {
    if (!cache.has(id)) {
      const p = fetch(base + encodeURIComponent(id) + '.md', { credentials: 'same-origin' }).then((res) => {
        if (!res.ok) throw new Error('HTTP ' + res.status);
        return res.text();
      });
      p.catch(() => cache.delete(id)); // 失败不缓存，下次可重试
      cache.set(id, p);
    }
    return cache.get(id);
  }

  function topicFromHash() {
    let h = location.hash.replace(/^#/, '');
    try { h = decodeURIComponent(h); } catch (_) { /* 保持原样 */ }
    h = h.replace(/^help\//, '');
    return known.has(h) ? h : 'quickstart';
  }

  function markNav(id) {
    nav.querySelectorAll('a[data-topic]').forEach((a) => {
      const on = a.dataset.topic === id;
      a.classList.toggle('on', on);
      if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    });
  }

  function error(msg) {
    body.innerHTML = '<div class="alert err" role="alert">' + esc(msg) + '</div>';
    tocBox.innerHTML = '';
  }

  async function showTopic() {
    const my = ++seq;
    const id = topicFromHash();
    markNav(id);
    let src;
    try {
      src = await load(id);
    } catch (_) {
      if (my === seq) error(t('文档内容加载失败，请刷新页面重试。'));
      return;
    }
    if (my !== seq) return;
    const r = mdRender(src);
    body.innerHTML = r.html;
    tocBox.innerHTML = r.toc.length
      ? '<div class="t">' + esc(t('本页目录')) + '</div>' + r.toc.map((it) => '<a data-id="' + it.id + '">' + esc(it.t) + '</a>').join('')
      : '';
    root.scrollTop = 0;
    const cur = topics.find((x) => x.id === id);
    document.title = (cur ? cur.title + ' · ' : '') + t('使用文档') + ' · dbtool';
  }

  const reEsc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

  // 全文搜索：逐行匹配（忽略大小写），命中处高亮；先转义再高亮，不会把输入当 HTML。
  async function showSearch() {
    const my = ++seq;
    markNav('');
    tocBox.innerHTML = '';
    const texts = await Promise.all(topics.map((tp) => load(tp.id).catch(() => '')));
    if (my !== seq) return;
    const ql = q.toLowerCase();
    const hits = [];
    const MAX = 80;
    topics.forEach((tp, k) => {
      texts[k].split('\n').forEach((l) => {
        if (hits.length < MAX && l.toLowerCase().includes(ql) && !/^```/.test(l)) hits.push({ t: tp, l });
      });
    });
    const re = new RegExp('(' + reEsc(esc(q)) + ')(?![^<]*>)', 'gi');
    const rows = hits.map((h) => {
      const text = h.l.replace(/^[#>|\-*\s]+/, '').replace(/\|/g, ' · ');
      return '<div class="hit"><a href="#help/' + esc(h.t.id) + '">' + esc(h.t.title) + '</a><div class="small">' +
        mdInline(text).replace(re, '<mark>$1</mark>') + '</div></div>';
    }).join('');
    const count = hits.length >= MAX ? t('{n} 条（仅显示前 {max} 条）', { n: hits.length, max: MAX }) : t('{n} 条', { n: hits.length });
    body.innerHTML = '<h1>' + esc(t('搜索「{q}」', { q })) + ' <span class="small muted">' + esc(count) +
      '</span> <button class="btn clear" type="button" id="help-clear">' + esc(t('清除搜索')) + '</button></h1>' +
      (rows || '<div class="muted">' + esc(t('没有找到相关内容，换个关键词试试。')) + '</div>');
    root.scrollTop = 0;
  }

  function refresh() { return q ? showSearch() : showTopic(); }

  function setQuery(v) {
    v = v.trim();
    if (v === q) return;
    q = v;
    refresh();
  }

  function clearSearch() {
    q = '';
    input.value = '';
    return showTopic();
  }

  let timer = 0;
  input.addEventListener('input', () => {
    clearTimeout(timer);
    timer = setTimeout(() => setQuery(input.value), 300);
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); clearTimeout(timer); setQuery(input.value); }
  });

  root.addEventListener('click', (e) => {
    const clear = e.target.closest('#help-clear');
    if (clear) { clearSearch(); return; }
    const toc = e.target.closest('#help-toc a[data-id]');
    if (toc) {
      const el = document.getElementById(toc.dataset.id);
      if (el) el.scrollIntoView({ behavior: 'smooth', block: 'start' });
      return;
    }
    const a = e.target.closest('a[href^="#"]');
    if (a && q) { // 点目录 / 搜索结果 / 正文链接：退出搜索态；地址没变时不会触发 hashchange，需要自己重绘
      const same = a.getAttribute('href') === location.hash;
      q = '';
      input.value = '';
      if (same) showTopic();
    }
  });

  window.addEventListener('hashchange', () => { if (!q) showTopic(); else clearSearch(); });
  showTopic();
}
