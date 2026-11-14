/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: preview.js
 * @time: 2026/10/28
 */
// 单元格内容预览：双击单元格 / 悬停工具里的“预览”打开。完整内容带行号、长行自动换行，
// 看起来像 SQL 的内容用和编辑器同一套高亮；右上角“复制”复制当前预览的完整内容，Esc / 遮罩 / ✕ 关闭。
// 所有文本都用 textContent / ACE 的 setValue 写入，不解析 HTML。
/* global ace */
import { t } from '../i18n.js';
import { copyText } from './clip.js';
import { PREVIEW_MAX } from './cells.js';
import { openModal } from './modal.js';
import { el, iconButton, nf } from './ui.js';

// 用 ACE 做只读的代码视图（行号、换行、高亮）；ACE 没加载出来时退回带行号的 <pre>。
// 返回 { node, ed, focus(), resize() }：node 放进文档后再调用 resize()（按换行后的行数定高度，maxPx 封顶）。
export function codeView(text, mode, maxPx) {
  if (typeof ace === 'undefined' || !ace.edit) {
    const box = el('div', 'cv-fallback');
    box.append(el('div', 'cv-gutter', text.split('\n').map((_, i) => String(i + 1)).join('\n')));
    const pre = el('pre', 'cv-pre', text);
    pre.tabIndex = 0;
    pre.setAttribute('aria-label', t('内容'));
    box.append(pre);
    return { node: box, ed: null, focus: () => pre.focus(), resize() {} };
  }
  const host = el('div', 'cv-ace');
  const ed = ace.edit(host);
  ed.setTheme('ace/theme/dbtool');
  ed.session.setMode(mode || 'ace/mode/text');
  ed.session.setUseWrapMode(true);
  ed.setReadOnly(true);
  ed.setOptions({ showPrintMargin: false, highlightActiveLine: false, highlightGutterLine: false, showFoldWidgets: false, fontSize: 13 });
  ed.renderer.$cursorLayer.element.style.display = 'none'; // 只读：不显示光标
  ed.setValue(text, -1);
  ed.session.getUndoManager().reset();
  return {
    node: host,
    ed,
    focus: () => ed.focus(),
    // 内容高度跟着走：先让 ACE 按当前宽度算出换行后的行数，再设置容器高度
    resize() {
      ed.resize(true);
      const lh = ed.renderer.lineHeight || 22;
      host.style.height = Math.min(Math.max(ed.session.getScreenLength() * lh + 20, 80), maxPx) + 'px';
      ed.resize(true);
    },
  };
}

// o: { title: 列名, sub: '第 3 行 · varchar(255)'（可空）, text: 完整内容, mode: ACE 模式 id（空 = 纯文本）, isNull: 值是否为 NULL }
export function openPreview(o) {
  const text = String(o.text);
  const shown = text.length > PREVIEW_MAX ? text.slice(0, PREVIEW_MAX) : text;
  const lineCount = text.split('\n').length;
  const m = openModal({ label: t('内容预览：{name}', { name: o.title }) });
  const dlg = m.dlg;

  const head = el('header', 'cv-head');
  const title = el('h2', 'cv-title', o.title);
  title.title = o.title;
  const meta = el('span', 'cv-meta', [o.sub, o.isNull ? t('值为 NULL') : t('{chars} 字符 · {lines} 行', { chars: nf(text.length), lines: nf(lineCount) })].filter(Boolean).join(' · '));
  // 复制结果就地提示：全局 toast 出现在右上角，会盖住这里的复制 / 关闭按钮
  const status = el('span', 'cv-status');
  status.setAttribute('role', 'status');
  const copy = iconButton('cv-icon', t('复制'), 'copy');
  copy.setAttribute('aria-label', t('复制全部内容'));
  const close = iconButton('cv-icon', t('关闭'), 'close');
  head.append(title, meta, el('span', 'spacer'), status, copy, close);
  dlg.append(head);
  if (shown.length < text.length) {
    dlg.append(el('div', 'cv-note', t('仅预览前 {max} 个字符（共 {total}）', { max: nf(PREVIEW_MAX), total: nf(text.length) })));
  }

  const body = el('div', 'cv-body');
  dlg.append(body);
  const view = codeView(shown, o.mode, Math.round(window.innerHeight * 0.88) - 110);
  body.append(view.node);
  if (view.ed) m.onClose(() => view.ed.destroy());

  let timer = 0;
  m.onClose(() => clearTimeout(timer));
  copy.addEventListener('click', async () => {
    const ok = await copyText(text);
    status.textContent = ok ? t('已复制（{n} 字符）', { n: nf(text.length) }) : t('复制失败，请选中文本后按 Ctrl+C');
    status.classList.toggle('bad', !ok);
    copy.classList.toggle('done', ok);
    clearTimeout(timer);
    timer = setTimeout(() => { status.textContent = ''; copy.classList.remove('done'); }, 2000);
  });
  close.addEventListener('click', () => m.close());

  view.resize();
  view.focus();
  return m;
}
