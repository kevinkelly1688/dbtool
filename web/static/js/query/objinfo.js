/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: objinfo.js
 * @time: 2026/10/28
 */
// 对象信息：表 / 视图 / 函数 / 存储过程 / 触发器 / 定时任务的基本信息与定义语句，只读。
// 打开方式：库表树里右键表名 →「对象信息」；“可编程对象”页签里双击对象。右上角“复制”复制定义语句。
// 内容取自后端 /api/v1/query/instances/{id}/databases/{db}/object-info（复用数据字典的对象详情）；
// 文本一律用 textContent / ACE setValue，不解析 HTML。
import { api } from '../common.js';
import { t, tp } from '../i18n.js';
import { copyText } from './clip.js';
import { openModal } from './modal.js';
import { codeView } from './preview.js';
import { el, iconButton, nf } from './ui.js';
import { aceMode } from './sqlutil.js';

export const KIND_LABEL = { table: t('表'), view: t('视图'), function: t('函数'), procedure: t('存储过程'), trigger: t('触发器'), event: t('定时任务') };

// o: { inst, db, kind: table|view|function|procedure|trigger|event, name, schema?: SQL Server / PostgreSQL 的架构, type: 实例类型 }
export function showObjectInfo(o) {
  const display = (o.schema ? o.schema + '.' : '') + o.name;
  const m = openModal({ label: t('对象信息：{name}', { name: display }), wide: true });
  const dlg = m.dlg;
  dlg.classList.add('oi-dialog'); // 比通用的 wide 更宽更高：列信息一行放得下

  const head = el('header', 'cv-head');
  const title = el('h2', 'cv-title', display);
  title.title = display;
  const meta = el('span', 'cv-meta', `${KIND_LABEL[o.kind] || t('对象')} · ${o.db}`);
  const status = el('span', 'cv-status');
  status.setAttribute('role', 'status');
  const copy = iconButton('cv-icon', t('复制定义语句'), 'copy');
  copy.disabled = true;
  const close = iconButton('cv-icon', t('关闭'), 'close');
  head.append(title, meta, el('span', 'spacer'), status, copy, close);
  dlg.append(head);
  const body = el('div', 'cv-body oi-body');
  body.append(el('p', 'oi-hint', t('加载中…')));
  dlg.append(body);

  let timer = 0;
  m.onClose(() => clearTimeout(timer));
  close.addEventListener('click', () => m.close());
  close.focus();

  let statement = '';
  copy.addEventListener('click', async () => {
    const ok = await copyText(statement);
    status.textContent = ok ? t('已复制（{n} 字符）', { n: nf(statement.length) }) : t('复制失败，请选中语句后按 Ctrl+C');
    status.classList.toggle('bad', !ok);
    copy.classList.toggle('done', ok);
    clearTimeout(timer);
    timer = setTimeout(() => { status.textContent = ''; copy.classList.remove('done'); }, 2000);
  });

  const p = new URLSearchParams({ name: o.name, kind: o.kind });
  if (o.schema) p.set('schema', o.schema);
  const url = `/api/v1/query/instances/${encodeURIComponent(o.inst)}/databases/${encodeURIComponent(o.db)}/object-info?${p}`;
  let closed = false;
  m.onClose(() => { closed = true; });
  api('GET', url).then((det) => {
    if (closed) return;
    body.replaceChildren();
    const code = render(body, det, o, m);
    if (code) {
      statement = code;
      copy.disabled = false;
    }
  }).catch((e) => {
    if (closed) return;
    body.replaceChildren(el('div', 'alert err', e.message || t('读取失败')));
  });
  return m;
}

// 说明类的列（注释 / 说明 / 备注 / Comment）：内容可能很长，只有这些列允许换行；其余列一律一行显示。
const WRAP_HEAD = /^(注释|说明|备注|表说明|comment|description|remarks?)$/i;
const PAIRS_SPLIT = 8; // 基本信息超过这么多行就分两栏
// 脱敏列：命中脱敏配置的字段写 Y。
const isMaskHead = (h) => h === t('脱敏');

// 带序号列的表格：heads 是表头（不含序号），rows 是字符串行；start 是第一行的序号偏移（分栏显示时右栏接着左栏编号）。
function tableOf(heads, rows, cls, start = 0) {
  const tbl = el('table', 'oi-kv ' + cls);
  const wrapCol = heads.map((h) => WRAP_HEAD.test(String(h).trim()));
  const maskCol = heads.findIndex(isMaskHead);
  const thead = el('thead');
  const hr = el('tr');
  hr.append(el('th', 'rn', '#'), ...heads.map((h, i) => el('th', (wrapCol[i] ? 'wrap' : '') + (i === maskCol ? ' mk' : ''), h)));
  thead.append(hr);
  const tb = el('tbody');
  rows.forEach((r, i) => {
    const tr = el('tr');
    if (maskCol >= 0 && r[maskCol] === 'Y') tr.className = 'oi-sens'; // 配置了脱敏的字段整行淡色突出
    tr.append(el('td', 'rn', String(start + i + 1)), ...r.map((v, j) => {
      const td = el('td', (wrapCol[j] ? 'wrap' : '') + (j === maskCol ? ' mk' : ''), v === null || v === undefined ? '' : String(v));
      if (j === maskCol && v === 'Y') td.title = t('该字段配置了数据脱敏');
      return td;
    }));
    tb.append(tr);
  });
  tbl.append(thead, tb);
  const wrap = el('div', 'oi-kvwrap');
  wrap.append(tbl);
  return wrap;
}

// 把详情画进 body；返回要复制的定义语句（第一个 code 小节的文本，没有则空串）。
// 小节标题 / 属性名 / 表头 / 提示由服务端按请求语言翻译；这里的 tp 兜底（精确匹配目录）。
function render(body, det, o, m) {
  let statement = '';
  const emptyOf = { [t('列信息')]: t('没有列'), [t('索引信息')]: t('没有索引') }; // 表格小节没有数据时的提示（键是已翻译的小节标题）
  for (const n of det.notes || []) body.append(el('div', 'alert info oi-alert', tp(n)));
  let firstKV = true;
  const views = [];
  for (const s of det.sections || []) {
    if (s.type === 'kv') {
      body.append(el('h3', 'oi-h', firstKV ? t('基本信息') : tp(s.title)));
      const pairs = (s.pairs || []).slice();
      if (firstKV && !pairs.some((p) => p.key === t('数据库'))) pairs.unshift({ key: t('数据库'), value: o.db });
      firstKV = false;
      const kv = pairs.map((p) => [tp(p.key), p.value]);
      const heads = [t('属性名'), t('属性值')];
      if (kv.length > PAIRS_SPLIT) { // 属性多：分成左右两栏（先左后右，序号连续），宽窗口里右边不会空着；窄屏自动叠成一栏
        const half = Math.ceil(kv.length / 2);
        const cols = el('div', 'oi-kvcols');
        cols.append(tableOf(heads, kv.slice(0, half), 'oi-pairs'), tableOf(heads, kv.slice(half), 'oi-pairs', half));
        body.append(cols);
      } else {
        body.append(tableOf(heads, kv, 'oi-pairs'));
      }
    } else if (s.type === 'table') { // 列信息 / 索引信息：自增列在“额外”里标注，索引带主键 / 唯一标记
      body.append(el('h3', 'oi-h', tp(s.title)));
      if ((s.rows || []).length) body.append(tableOf((s.columns || []).map(tp), s.rows, 'oi-grid'));
      else body.append(el('p', 'oi-hint', emptyOf[s.title] || t('没有{name}', { name: String(s.title).replace(/信息$/, '') })));
    } else if (s.type === 'code') {
      body.append(el('h3', 'oi-h', tp(String(s.title)).replace(/^建表语句/, t('创建语句'))));
      const text = s.text || '';
      if (!statement) statement = text;
      const v = codeView(text, aceMode(o.type), 420);
      const box = el('div', 'oi-code');
      box.append(v.node);
      body.append(box);
      views.push(v);
      if (v.ed) m.onClose(() => v.ed.destroy());
      if (s.note) body.append(el('p', 'oi-hint', tp(s.note)));
    } else {
      body.append(el('h3', 'oi-h', tp(s.title)), el('div', 'alert warn oi-alert', s.text || ''));
    }
  }
  if (!(det.sections || []).length) body.append(el('p', 'oi-hint', t('没有可显示的信息')));
  for (const v of views) v.resize(); // 节点已在文档里：按换行后的行数定高度
  return statement;
}
