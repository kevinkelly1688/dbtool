/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: redis_keys.js
 * @time: 2026/10/28
 */
// Redis 实例的左侧“key 浏览”：按匹配模式用 SCAN 分页列出 key 与类型，点 key 在编辑器里生成对应的读取命令。
// 服务端每页最多扫描有限的轮数，所以对大库也不会拖慢 Redis；扫不满一页时给“加载更多”继续。
// key 名都用 textContent 写入，不会被当成 HTML。
import { api } from '../common.js';
import { t } from '../i18n.js';

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// 按类型生成读取命令。arg 是服务端给的、已按 redis-cli 规则转义好的 key 参数。
export function commandFor(type, arg) {
  switch (type) {
    case 'string': return `GET ${arg}`;
    case 'hash': return `HGETALL ${arg}`;
    case 'list': return `LRANGE ${arg} 0 99`;
    case 'set': return `SMEMBERS ${arg}`;
    case 'zset': return `ZRANGE ${arg} 0 99 WITHSCORES`;
    case 'stream': return `XRANGE ${arg} - + COUNT 100`;
    default: return `TYPE ${arg}`;
  }
}

// 输入没有通配符时按前缀匹配（自动补 *）；空输入匹配全部。
export function toMatch(input) {
  const s = String(input || '').trim();
  if (!s) return '*';
  return /[*?[]/.test(s) ? s : s + '*';
}

export function createKeyBrowser({ container, onPick }) {
  let ctx = { inst: '', db: '', match: '*' };
  let keys = [];
  let cursor = '0';
  let done = true;
  let loading = false;
  let error = '';
  let seq = 0;

  const url = (cur) => `/api/v1/query/instances/${encodeURIComponent(ctx.inst)}/databases/${encodeURIComponent(ctx.db)}/keys?match=${encodeURIComponent(ctx.match)}&cursor=${encodeURIComponent(cur)}&count=200`;

  function render() {
    const frag = document.createDocumentFragment();
    const head = el('div', 'tree-head');
    head.append(el('span', '', `⛁ db${ctx.db}`), el('span', 'n', t('匹配 {match} · {n} 个 key', { match: ctx.match, n: keys.length })));
    frag.append(head);
    if (error) frag.append(el('div', 'alert err', error));
    if (!error && !loading && !keys.length) {
      frag.append(el('div', 'tree-hint', done ? t('没有匹配的 key（或该库为空）') : t('这一批没有匹配的 key')));
    }
    for (const k of keys) {
      const row = el('div', 'tr-row rk-row');
      const b = el('button', 'name');
      b.type = 'button';
      b.title = commandFor(k.type, k.arg) + (k.binary ? '\n' + t('含不可见字符，已按 \\xHH 显示') : '');
      b.append(el('span', 'rk-type rk-' + (k.type || 'none'), k.type || '?'), el('span', 't', k.key));
      b.addEventListener('click', () => onPick(commandFor(k.type, k.arg), k));
      row.append(b);
      frag.append(row);
    }
    if (loading) frag.append(el('div', 'tree-hint', t('加载中…')));
    if (!loading && !done && !error) {
      const more = el('button', 'btn rk-more', t('加载更多'));
      more.type = 'button';
      more.addEventListener('click', () => fetchPage(cursor));
      frag.append(more);
    }
    if (!loading && done && keys.length && !error) frag.append(el('div', 'tree-hint', t('已扫描完整个库')));
    container.replaceChildren(frag);
  }

  async function fetchPage(cur) {
    const my = ++seq;
    loading = true;
    error = '';
    render();
    try {
      const r = await api('GET', url(cur));
      if (my !== seq) return;
      keys = keys.concat(r.keys || []);
      cursor = r.cursor || '0';
      done = !!r.done;
    } catch (e) {
      if (my !== seq) return;
      error = e.message;
    }
    loading = false;
    render();
  }

  return {
    // 切换实例 / 库：清空并从头扫描。inst 或 db 为空时只显示提示。
    load({ inst, db, match }) {
      ctx = { inst, db, match: match || '*' };
      keys = [];
      cursor = '0';
      done = true;
      error = '';
      if (!inst || !db) {
        seq++;
        loading = false;
        container.replaceChildren(el('div', 'tree-hint', t('选择实例和数据库后可浏览 key')));
        return Promise.resolve();
      }
      return fetchPage('0');
    },
    // 换匹配模式重新扫描。
    search(match) {
      if (!ctx.inst || !ctx.db) return Promise.resolve();
      ctx.match = match || '*';
      keys = [];
      cursor = '0';
      done = true;
      return fetchPage('0');
    },
    // 作废在途请求（切到 MySQL / SQL Server 实例时调用，避免晚到的结果覆盖对象树）。
    cancel() { seq++; loading = false; },
  };
}
