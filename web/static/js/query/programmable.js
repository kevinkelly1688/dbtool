/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: programmable.js
 * @time: 2026/10/28
 */
// 左侧“可编程对象”页签：当前库的视图 / 函数 / 存储过程 / 触发器 / 定时任务，按类别分组（点组标题展开 / 收起，带数量）。
// 双击对象（或聚焦后按 Enter）查看定义，右键出菜单；搜索框只按对象名过滤。名称、注释一律 textContent 写入。
import { api } from '../common.js';
import { t, tp } from '../i18n.js';
import { el } from './ui.js';

const GLYPH = { view: '◈', function: 'ƒ', procedure: '▤', trigger: '⚡', event: '◷' };
// 某类对象为空时的提示（按类别整句翻译，英文不用拼接名词）。
const NONE_TEXT = {
  view: t('这个库里没有视图'), function: t('这个库里没有函数'), procedure: t('这个库里没有存储过程'),
  trigger: t('这个库里没有触发器'), event: t('这个库里没有定时任务'),
};

// 回调：onOpen(kind, item, ctx)、onContext(kind, item, event, ctx)；item = { schema?, name, comment }，ctx = { inst, db, type }。
export function createProgrammables({ container, onOpen, onContext }) {
  let ctx = { inst: '', db: '', type: '' };
  let groups = null; // [{ kind, label, items, error }]
  let kw = '';
  let seq = 0;
  const open = new Set();

  const hint = (text, err) => container.replaceChildren(el('div', err ? 'alert err' : 'tree-hint', text));
  const fullName = (it) => (it.schema ? it.schema + '.' + it.name : it.name);

  function render() {
    if (!groups) return;
    if (!groups.length) { hint(t('这个实例类型暂不支持查看可编程对象')); return; }
    const frag = document.createDocumentFragment();
    let anyShown = false;
    for (const g of groups) {
      const items = kw ? g.items.filter((it) => fullName(it).toLowerCase().includes(kw)) : g.items;
      if (kw && !items.length) continue;
      anyShown = true;
      const expanded = kw ? true : open.has(g.kind);
      const gh = el('button', 'tr-group');
      gh.type = 'button';
      gh.dataset.kind = g.kind;
      gh.setAttribute('aria-expanded', String(expanded));
      gh.title = expanded ? t('收起') : t('展开');
      gh.append(el('span', 'arrow', expanded ? '▾' : '▸'), el('span', 'gn', tp(g.label)), el('span', 'n', `(${items.length})`));
      gh.addEventListener('click', () => {
        if (open.has(g.kind)) open.delete(g.kind); else open.add(g.kind);
        render();
      });
      frag.append(gh);
      if (!expanded) continue;
      if (g.error) frag.append(el('div', 'tree-hint', t('读取失败：{err}', { err: g.error })));
      else if (!items.length) frag.append(el('div', 'tree-hint', NONE_TEXT[g.kind] || t('这个库里没有{label}', { label: tp(g.label) })));
      for (const it of items) {
        const b = el('button', 'prog-item');
        b.type = 'button';
        b.dataset.kind = g.kind;
        b.dataset.name = fullName(it);
        if (it.comment) b.title = it.comment;
        const gl = el('span', 'pg', GLYPH[g.kind] || '·');
        gl.setAttribute('aria-hidden', 'true');
        b.append(gl, el('span', 'pn', fullName(it)));
        b.addEventListener('dblclick', () => onOpen(g.kind, it, { ...ctx }));
        b.addEventListener('keydown', (e) => {
          if (e.key === 'Enter') { e.preventDefault(); onOpen(g.kind, it, { ...ctx }); }
        });
        b.addEventListener('contextmenu', (e) => { e.preventDefault(); onContext(g.kind, it, e, { ...ctx }); });
        frag.append(b);
      }
    }
    if (!anyShown) frag.append(el('div', 'tree-hint', t('没有匹配的对象')));
    container.replaceChildren(frag);
  }

  return {
    // 换了实例 / 库：作废在途请求并清空（不发请求；页签被打开时再 load）。
    clear() {
      seq++;
      groups = null;
      open.clear();
      container.replaceChildren();
    },
    async load(next) {
      ctx = { ...ctx, ...next };
      const my = ++seq;
      groups = null;
      open.clear();
      if (!ctx.inst || !ctx.db) { hint(t('选择实例和数据库后显示可编程对象')); return; }
      hint(t('加载中…'));
      try {
        const base = `/api/v1/query/instances/${encodeURIComponent(ctx.inst)}/databases/${encodeURIComponent(ctx.db)}`;
        const r = await api('GET', `${base}/programmables`);
        if (my !== seq) return;
        groups = r.groups || [];
        render();
      } catch (e) {
        if (my === seq) hint(e.message || t('读取失败'), true);
      }
    },
    setFilter(text) {
      kw = String(text || '').trim().toLowerCase();
      render();
    },
  };
}
