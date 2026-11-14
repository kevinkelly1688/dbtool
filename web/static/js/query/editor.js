/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: editor.js
 * @time: 2026/10/28
 */
// ACE 编辑器封装。主题 "dbtool" 的颜色全部取自 CSS 变量，跟随皮肤自动切换。
/* global ace */
import { t } from '../i18n.js';
import { statementAt, runParts } from './statements.js';
import { aceMode } from './sqlutil.js';

function skinIsDark() {
  const s = document.documentElement.getAttribute('data-skin');
  return !(s === 'light' || s === 'sepia');
}

ace.define('ace/theme/dbtool', ['require', 'exports', 'module', 'ace/lib/dom'], function (require, exports) {
  exports.cssClass = 'ace-dbtool';
  exports.$id = 'ace/theme/dbtool';
  Object.defineProperty(exports, 'isDark', { get: skinIsDark });
  exports.cssText = [
    '.ace-dbtool { background-color: var(--dt-input); color: var(--dt-code-text); line-height: 1.7; }',
    '.ace-dbtool .ace_gutter { background: var(--dt-code-ln-bg); color: var(--dt-code-ln); border-right: 1px solid var(--dt-line); }',
    '.ace-dbtool .ace_gutter-active-line { background-color: rgba(var(--dt-cyan-rgb), .08); }',
    '.ace-dbtool .ace_print-margin { width: 1px; background: var(--dt-line); }',
    '.ace-dbtool .ace_cursor { color: var(--dt-cyan); border-left-width: 2px; }',
    '.ace-dbtool .ace_marker-layer .ace_selection { background: rgba(var(--dt-cyan-rgb), .22); }',
    '.ace-dbtool .ace_marker-layer .ace_bracket { margin: -1px 0 0 -1px; border: 1px solid rgba(var(--dt-cyan-rgb), .6); }',
    '.ace-dbtool .ace_marker-layer .ace_active-line { background: rgba(var(--dt-cyan-rgb), .05); }',
    '.ace-dbtool .ace_marker-layer .ace_selected-word { border: 1px solid rgba(var(--dt-cyan-rgb), .35); }',
    '.ace-dbtool .ace_keyword, .ace-dbtool .ace_storage { color: var(--dt-code-kw); }',
    '.ace-dbtool .ace_constant.ace_language, .ace-dbtool .ace_constant.ace_buildin { color: var(--dt-code-kw); }',
    '.ace-dbtool .ace_support.ace_function { color: var(--dt-code-fn); }',
    '.ace-dbtool .ace_string { color: var(--dt-code-str); }',
    '.ace-dbtool .ace_constant.ace_numeric { color: var(--dt-code-num); }',
    '.ace-dbtool .ace_comment { color: var(--dt-code-cm); font-style: italic; }',
    '.ace-dbtool .ace_keyword.ace_operator, .ace-dbtool .ace_paren { color: var(--dt-code-op); }',
    '.ace-dbtool .ace_invalid { color: #fff; background-color: var(--dt-crit); }',
    '.ace-dbtool .ace_placeholder { color: var(--dt-code-ln); }',
    '.ace_editor.ace_autocomplete { background: var(--dt-panel); color: var(--dt-fg); border: 1px solid var(--dt-line-hi); box-shadow: var(--dt-shadow); }',
    '.ace_editor.ace_autocomplete .ace_marker-layer .ace_active-line { background-color: var(--dt-cyan-soft); }',
  ].join('\n');
  require('../lib/dom').importCssString(exports.cssText, exports.cssClass, false);
});

export function createEditor(el, { onRun, onChange, onRunAll }) {
  ace.config.set('basePath', '/static/vendor/ace');
  const editor = ace.edit(el);
  editor.setTheme('ace/theme/dbtool');
  editor.session.setMode('ace/mode/mysql');
  editor.setFontSize(13);
  editor.setShowPrintMargin(false);
  editor.session.setUseWrapMode(true);
  editor.setOptions({ enableBasicAutocompletion: true, enableLiveAutocompletion: true });
  editor.renderer.setScrollMargin(6, 6);

  let gutterMarks = []; // 行号槽里的语句状态圆点 [{ row, cls }]
  let hlMarker = null; // 当前结果页签对应的语句高亮
  let names = []; // 表名补全（Redis 实例时是命令名）
  let metaLabel = t('表');
  const completer = {
    getCompletions(_ed, _session, _pos, prefix, cb) {
      if (!prefix) return cb(null, []);
      cb(null, names.map((n) => ({ caption: n, value: n, meta: metaLabel })));
    },
  };
  ace.require('ace/ext/language_tools').addCompleter(completer);

  editor.commands.addCommand({
    name: 'runQuery',
    bindKey: { win: 'Ctrl-Enter', mac: 'Command-Enter' },
    exec: () => onRun(),
  });
  if (onRunAll) {
    editor.commands.addCommand({
      name: 'runAllQueries',
      bindKey: { win: 'Ctrl-Shift-Enter', mac: 'Command-Shift-Enter' },
      exec: () => onRunAll(),
    });
  }
  editor.session.on('change', () => onChange());
  document.addEventListener('skinchange', () => editor.setTheme('ace/theme/dbtool'));

  return {
    get value() { return editor.getValue(); },
    set value(v) { editor.setValue(v, 1); },
    setMode(dbType) { editor.session.setMode(aceMode(dbType)); },
    setTables(list, label = t('表')) { names = list; metaLabel = label; },
    focus() { editor.focus(); },
    resize() { editor.resize(); },
    // 选中文本优先；否则光标所在语句；否则全部内容。
    sqlToRun() {
      const sel = editor.getSelectedText();
      if (sel.trim()) return sel;
      const text = editor.getValue();
      const off = editor.session.doc.positionToIndex(editor.getCursorPosition());
      return statementAt(text, off) ?? text;
    },
    // MySQL 系：{ prelude, stmt }。选中文本原样作为 stmt（要用变量就把 SET 一起选上）；否则取光标所在语句，
    // 并自动带上脚本开头连续的 SET 语句作为 prelude（服务端先在同一会话里执行它们）。见 statements.js 的 runParts。
    partsToRun() {
      const sel = editor.getSelectedText();
      if (sel.trim()) return { prelude: '', stmt: sel };
      const text = editor.getValue();
      const off = editor.session.doc.positionToIndex(editor.getCursorPosition());
      return runParts(text, off);
    },
    // Redis：选中文本优先；否则光标所在行；光标行是空行时用全部内容（服务端一次只执行一条命令，多行会被拒绝并提示）。
    lineToRun() {
      const sel = editor.getSelectedText();
      if (sel.trim()) return sel;
      const line = editor.session.getLine(editor.getCursorPosition().row);
      return line.trim() ? line : editor.getValue();
    },
    // MongoDB：选中文本优先；否则整个编辑器内容（语句里的字符串 / 对象可含分号和空行，不能按它们切分；服务端一次只执行一条）。
    allToRun() {
      const sel = editor.getSelectedText();
      return sel.trim() ? sel : editor.getValue();
    },
    // 把编辑器里第一处 old 换成 next，并把光标放到替换后的末尾；找不到返回 false。
    replaceText(old, next) {
      const text = editor.getValue();
      const i = old ? text.indexOf(old) : -1;
      if (i < 0) return false;
      editor.setValue(text.slice(0, i) + next + text.slice(i + old.length), 1);
      editor.moveCursorToPosition(editor.session.doc.indexToPosition(i + next.length));
      editor.clearSelection();
      return true;
    },
    // 在光标处（或替换选中内容）插入文本，用于点字段名。
    insert(text) {
      editor.insert(text);
      editor.focus();
    },
    append(sql) {
      const cur = editor.getValue().trim();
      editor.setValue(cur ? cur + '\n\n' + sql : sql, 1);
      editor.focus();
    },

    // ---- 一次运行多条语句（js/query/batch.js）用 ----
    // 选区的字符偏移 { start, end }；没选中返回 null。
    selection() {
      if (editor.selection.isEmpty()) return null;
      const r = editor.getSelectionRange();
      const doc = editor.session.doc;
      return { start: doc.positionToIndex(r.start), end: doc.positionToIndex(r.end) };
    },
    // 光标 / 选区变了：回调（运行全部按钮上的条数用）。
    onCursor(fn) {
      editor.selection.on('changeCursor', fn);
      editor.selection.on('changeSelection', fn);
    },
    // 字符偏移所在的行号（从 0 起）。
    rowOf(offset) { return editor.session.doc.indexToPosition(Math.max(0, offset)).row; },
    // 给每条语句第一行的行号槽画一个带序号的状态圆点：marks = [{ row, n, status }]；传空数组清掉。样式见 query.css 的 .dbt-gm*。
    setGutterMarks(marks) {
      const s = editor.session;
      for (const g of gutterMarks) s.removeGutterDecoration(g.row, g.cls);
      gutterMarks = marks.filter((m) => m.row >= 0 && m.row < s.getLength()).map((m) => ({ row: m.row, cls: `dbt-gm dbt-gm-${m.status} dbt-gm-n${Math.min(m.n, 20)}` }));
      for (const g of gutterMarks) s.addGutterDecoration(g.row, g.cls);
    },
    // 点了带圆点的行号槽：回调该行号（返回 false 表示没处理）。
    onGutterMark(fn) {
      editor.on('gutterclick', (e) => {
        const row = e.getDocumentPosition().row;
        if (gutterMarks.some((g) => g.row === row) && fn(row) !== false) e.stop();
      });
    },
    // 高亮 [start, end] 字符偏移所在的整行（当前结果页签对应的语句）；range 为 null 取消；reveal 为真时滚进可视区。
    highlight(range, reveal) {
      const s = editor.session;
      if (hlMarker != null) { s.removeMarker(hlMarker); hlMarker = null; }
      if (!range) return;
      const p0 = s.doc.indexToPosition(range.start);
      const p1 = s.doc.indexToPosition(range.end);
      const Range = ace.require('ace/range').Range;
      hlMarker = s.addMarker(new Range(p0.row, 0, p1.row, 1), 'dbt-stmt-act', 'fullLine');
      if (reveal) {
        const r = editor.renderer;
        if (p0.row < r.getFirstFullyVisibleRow() || p1.row > r.getLastFullyVisibleRow()) editor.scrollToLine(p0.row, true, true, () => {});
      }
    },
    // 选中 [start, end] 字符偏移之间的文本并聚焦（“定位到语句”）。
    select(start, end) {
      const doc = editor.session.doc;
      const p0 = doc.indexToPosition(start);
      const p1 = doc.indexToPosition(end);
      const Range = ace.require('ace/range').Range;
      editor.selection.setRange(new Range(p0.row, p0.column, p1.row, p1.column));
      editor.scrollToLine(p0.row, true, true, () => {});
      editor.focus();
    },
  };
}
