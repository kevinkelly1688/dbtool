/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: templates.js
 * @time: 2026/10/28
 */
// 通知消息模板（仅超级用户）：左侧选一类消息，中间编辑标题 / 状态色 / 内容块（字段表、文本、SQL 代码块、链接按钮、分隔线），
// 右侧是各渠道的预览（服务端按真实渲染逻辑生成）与可用变量。范围：全局 / 渠道专属；没有定制时沿用上一层。
// 保存 / 删除走统一确认组件 confirmRun；未保存的修改在切换前提示。
import { api, confirmRun, riskConfirm, toast } from '../common.js';
import { el } from '../sysadmin/ui.js';
import { t, tp, isZh } from '../i18n.js';

const $ = (id) => document.getElementById(id);
const BASE = '/api/v1/settings/notify/templates';

const BLOCK_NAME = { fields: t('字段表'), text: t('文本'), code: t('SQL / 代码块'), link: t('链接 / 按钮'), divider: t('分隔线') };
const MODES = [
  ['mask', t('隐藏字符串常量后取前 N 字')],
  ['head', t('只显示前 N 字')],
  ['full', t('完整显示')],
  ['none', t('不显示 SQL')],
];
const PTYPES = [['slack', 'Slack'], ['dingtalk', t('钉钉')], ['wecom', t('企业微信')], ['feishu', t('飞书 / Lark')], ['telegram', 'Telegram'], ['email', t('邮件')]];
const TONE_LABEL = { info: t('信息'), ok: t('成功'), warn: t('待处理'), crit: t('异常') };

let cat = null; // 目录：消息、变量、渠道、已保存模板摘要
let cur = null; // 正在编辑：{ key, scope, def, saved, tpl, parent, parentScope, dflt, base }
let focusEl = null; // 最近聚焦的“可插入变量”的输入框
let ptype = 'slack';
let pvTimer = 0;
let pvSeq = 0;
let lastIssues = [];

// “审批通过”在公共词表里是“审批”按钮（Approve）；这里是事件 / 消息名（一种状态），用带上下文的键译成 Approved
const tpName = (s) => (s === '审批通过' ? t('审批通过|state') : tp(s)); // i18n-ignore 与接口返回的名称比较

const clone = (o) => JSON.parse(JSON.stringify(o));
const norm = (x) => ({ name: x.name || '', enabled: !!x.enabled, title: x.title || '', tone: x.tone || 'info', icon: x.icon || '', blocks: x.blocks || [] });
const isDirty = () => !!cur && JSON.stringify(norm(cur.tpl)) !== cur.base;

function setMsg(text, kind = 'err') {
  const box = $('nt-msg');
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}

const chanOf = (id) => cat.channels.find((c) => String(c.id) === String(id));
const scopeLabel = (scope) => {
  if (scope === 'global') return t('全局（所有渠道）');
  const c = chanOf(scope.slice(2));
  return c ? t('渠道：{name}（{type}）', { name: c.name, type: tp(c.label) }) : scope;
};
const ptypeOf = (ty) => (ty === 'lark' ? 'feishu' : ty);

// ---- 左侧：消息列表 ----

function renderList() {
  const q = $('nt-q').value.trim().toLowerCase();
  const box = $('nt-events');
  box.replaceChildren();
  let lastGroup = '';
  for (const m of cat.messages) {
    // 搜索同时匹配中文原文与当前语言的译文
    if (q && !(m.name + tpName(m.name) + m.key + m.group + tp(m.group) + m.hint + tp(m.hint)).toLowerCase().includes(q)) continue;
    if (m.group !== lastGroup) {
      lastGroup = m.group;
      box.append(el('div', 'nt-grp', tp(m.group)));
    }
    const b = el('button', 'nt-ev' + (cur && cur.key === m.key ? ' on' : ''));
    b.type = 'button';
    b.setAttribute('role', 'listitem');
    b.append(document.createTextNode(tpName(m.name)));
    const tags = el('span', 'tags');
    const g = cat.templates.find((s) => s.event === m.key && s.scope === 'global');
    const n = cat.templates.filter((s) => s.event === m.key && s.scope !== 'global').length;
    if (g) tags.append(el('span', 'tag g', g.enabled ? t('全局') : t('全局·停用')));
    if (n) tags.append(el('span', 'tag c', t('渠道×{n}', { n })));
    b.append(tags);
    b.addEventListener('click', () => open(m.key, cur && cur.key === m.key ? cur.scope : 'global'));
    box.append(b);
  }
  if (!box.children.length) box.append(el('p', 'muted small', t('没有匹配的消息')));
}

// ---- 打开一条模板 ----

async function guardDirty() {
  if (!isDirty()) return true;
  const r = await riskConfirm({ title: t('放弃未保存的修改？'), lines: [t('当前模板有还没保存的修改，切换后会丢失。')], okText: t('放弃修改'), cancelText: t('继续编辑'), level: 'high' });
  return r.ok;
}

async function open(key, scope) {
  if (!(await guardDirty())) {
    if (cur) $('nt-scope').value = cur.scope;
    return;
  }
  const def = cat.messages.find((m) => m.key === key);
  setMsg('');
  let r;
  try {
    r = await api('GET', `${BASE}/item?event=${encodeURIComponent(key)}&scope=${encodeURIComponent(scope)}`);
  } catch (e) { setMsg(e.message); return; }
  const tpl = clone(r.template || r.parent);
  tpl.event, tpl.scope = key, scope;
  tpl.event = key;
  tpl.scope = scope;
  if (!r.template) { tpl.name = tpl.name || ''; tpl.enabled = true; }
  cur = { key, scope, def, saved: !!r.template, tpl, parent: r.parent, parentScope: r.parent_scope, dflt: r.default, savedInfo: r.template };
  cur.base = JSON.stringify(norm(tpl));
  if (scope !== 'global') { const c = chanOf(scope.slice(2)); if (c) ptype = ptypeOf(c.type); }
  $('nt-editor').hidden = false;
  $('nt-side').hidden = false;
  renderList();
  renderEditor();
  schedulePreview(0);
}

// ---- 编辑器 ----

function renderEditor() {
  const tp0 = cur.tpl;
  $('nt-title').textContent = tpName(cur.def.name);
  $('nt-scope').value = cur.scope;
  $('nt-name').value = tp0.name || '';
  $('nt-tone').value = tp0.tone || 'info';
  $('nt-icon').value = tp0.icon || '';
  $('nt-enabled').checked = !!tp0.enabled;
  $('nt-ttl').value = tp0.title || '';
  $('nt-note').value = '';
  $('nt-reset').hidden = !cur.saved;
  $('nt-reset').textContent = cur.scope === 'global' ? t('恢复为系统默认') : t('恢复到上一层');
  $('nt-hist').hidden = true;
  $('nt-test-result').replaceChildren();
  renderSource();
  renderBlocks();
  renderVars();
  renderPtabs();
}

function renderSource() {
  const box = $('nt-source');
  box.replaceChildren();
  let text;
  let kind = 'info';
  const scope = scopeLabel(cur.scope);
  if (cur.saved) {
    const s = cur.savedInfo || {};
    text = s.updated_by ? t('「{scope}」已定制（{by} · {at}）。', { scope, by: s.updated_by, at: s.updated_at }) : t('「{scope}」已定制。', { scope });
    if (!s.enabled) { text += t('但已停用，发送时按上一层处理。'); kind = 'warn'; }
  } else {
    const up = cur.parentScope === 'global' ? t('全局模板') : t('系统默认文案');
    text = t('「{scope}」未定制，发送时用{up}。', { scope, up });
  }
  box.append(el('div', 'alert ' + kind, text));
}

function touch() {
  $('nt-save').disabled = false;
  schedulePreview(450);
}

function bindMeta() {
  const on = (id, key, kind) => $(id).addEventListener(kind || 'input', (e) => {
    cur.tpl[key] = e.target.type === 'checkbox' ? e.target.checked : e.target.value;
    touch();
  });
  on('nt-name', 'name');
  on('nt-tone', 'tone', 'change');
  on('nt-icon', 'icon');
  on('nt-enabled', 'enabled', 'change');
  on('nt-ttl', 'title');
}

// 可插入变量的输入框都打上 data-tpl，focusin 时记下来。
function tplInput(tag, value, onInput, attrs = {}) {
  const i = el(tag);
  if (tag === 'input') i.type = 'text';
  i.value = value || '';
  i.dataset.tpl = '1';
  i.autocomplete = 'off';
  for (const [k, v] of Object.entries(attrs)) i.setAttribute(k, v);
  i.addEventListener('input', () => { onInput(i.value); touch(); });
  return i;
}

// 字段的值输入框：单行起步，输入换行（回车）后随内容增高。
function grow(area) {
  area.style.height = 'auto';
  area.style.height = Math.max(32, area.scrollHeight + 2) + 'px';
}

function renderBlocks() {
  const box = $('nt-blocks');
  box.replaceChildren();
  const blocks = cur.tpl.blocks;
  if (!blocks.length) box.append(el('p', 'muted small', t('还没有内容块')));
  blocks.forEach((b, i) => {
    const card = el('div', 'nt-blk');
    const h = el('div', 'nt-blk-h');
    h.append(el('b', '', `${i + 1}. ${BLOCK_NAME[b.t] || b.t}`), el('span', 'spacer'));
    const mk = (text, title, fn, dis) => {
      const x = el('button', 'btn', text);
      x.type = 'button';
      x.title = title;
      x.disabled = !!dis;
      x.addEventListener('click', fn);
      return x;
    };
    h.append(
      mk('▲', t('上移'), () => { [blocks[i - 1], blocks[i]] = [blocks[i], blocks[i - 1]]; renderBlocks(); touch(); }, i === 0),
      mk('▼', t('下移'), () => { [blocks[i + 1], blocks[i]] = [blocks[i], blocks[i + 1]]; renderBlocks(); touch(); }, i === blocks.length - 1),
      mk('✕', t('删除这个块'), () => { blocks.splice(i, 1); renderBlocks(); touch(); }),
    );
    card.append(h);
    if (b.t === 'fields') fieldsEditor(card, b);
    else if (b.t === 'text') card.append(tplInput('textarea', b.v, (v) => { b.v = v; }, { placeholder: t('文字，可含 {{变量}}'), 'aria-label': t('文本内容') }));
    else if (b.t === 'code') codeEditor(card, b);
    else if (b.t === 'link') linkEditor(card, b);
    box.append(card);
  });
}

function fieldsEditor(card, b) {
  const wrap = el('div');
  const draw = () => {
    wrap.replaceChildren();
    b.rows.forEach((r, j) => {
      const row = el('div', 'nt-row');
      row.append(tplInput('input', r.l, (v) => { r.l = v; }, { placeholder: t('名称'), 'aria-label': t('字段名称') }));
      const vi = tplInput('textarea', r.v, (v) => { r.v = v; grow(vi); }, { placeholder: t('值，如 {{title}}'), 'aria-label': t('字段的值'), rows: '1' });
      row.append(vi);
      requestAnimationFrame(() => grow(vi));
      const sh = el('label', 'mini');
      const cb = el('input');
      cb.type = 'checkbox';
      cb.checked = !!r.short;
      cb.addEventListener('change', () => { r.short = cb.checked; touch(); });
      sh.append(cb, document.createTextNode(t('占半行')));
      const del = el('button', 'btn', '✕');
      del.type = 'button';
      del.title = t('删除这一行');
      del.addEventListener('click', () => { b.rows.splice(j, 1); draw(); touch(); });
      row.append(sh, del);
      wrap.append(row);
    });
    const add = el('button', 'btn', t('+ 加一行'));
    add.type = 'button';
    add.addEventListener('click', () => {
      if (b.rows.length >= cat.limits.max_rows) { setMsg(t('一个字段表最多 {n} 行', { n: cat.limits.max_rows }), 'warn'); return; }
      b.rows.push({ l: '', v: '' });
      draw();
      touch();
    });
    wrap.append(add);
  };
  draw();
  card.append(wrap);
}

function codeEditor(card, b) {
  const g = el('div', 'nt-code');
  g.append(tplInput('input', b.l, (v) => { b.l = v; }, { placeholder: t('小标题，如 SQL 内容'), 'aria-label': t('代码块小标题') }));
  g.append(tplInput('textarea', b.v, (v) => { b.v = v; }, { placeholder: t('内容，如 {{sql}}'), 'aria-label': t('代码块内容'), rows: '3' }));
  const opt = el('div', 'opt full');
  const sel = el('select');
  sel.setAttribute('aria-label', t('SQL 显示方式'));
  for (const [v, label] of MODES) sel.append(new Option(label, v));
  sel.value = b.mode || 'mask';
  const n = el('input');
  n.type = 'number';
  n.min = cat.limits.code_min;
  n.max = cat.limits.code_max;
  n.value = b.n || cat.limits.code_default;
  n.setAttribute('aria-label', t('显示字数'));
  const nLab = el('label', 'mini', t('字数'));
  const sync = () => { const need = b.mode === 'mask' || b.mode === 'head'; n.hidden = !need; nLab.hidden = !need; };
  sel.addEventListener('change', () => { b.mode = sel.value; sync(); touch(); });
  n.addEventListener('input', () => { b.n = Number(n.value) || 0; touch(); });
  opt.append(el('span', 'muted small', t('显示方式')), sel, nLab, n);
  g.append(opt);
  sync();
  card.append(g);
}

function linkEditor(card, b) {
  const g = el('div', 'nt-code');
  g.append(tplInput('input', b.l, (v) => { b.l = v; }, { placeholder: t('链接文字，如 查看详情'), 'aria-label': t('链接文字') }));
  g.append(tplInput('input', b.v, (v) => { b.v = v; }, { placeholder: t('地址，如 {{url}}'), 'aria-label': t('链接地址') }));
  const opt = el('div', 'opt full');
  const sel = el('select');
  sel.setAttribute('aria-label', t('链接样式'));
  sel.append(new Option(t('按钮'), 'button'), new Option(t('文字链接'), 'text'));
  sel.value = b.as || 'button';
  sel.addEventListener('change', () => { b.as = sel.value; touch(); });
  opt.append(el('span', 'muted small', t('样式')), sel);
  g.append(opt);
  card.append(g);
}

function addBlock(kind) {
  if (cur.tpl.blocks.length >= cat.limits.max_blocks) { setMsg(t('内容块最多 {n} 个', { n: cat.limits.max_blocks }), 'warn'); return; }
  const b = { t: kind };
  // 新块的初始内容属于用户可编辑的模板内容（存库、按渠道接收方发送），不随界面语言翻译
  if (kind === 'fields') b.rows = [{ l: '工单名', v: '{{title}}' }]; // i18n-ignore
  else if (kind === 'text') b.v = '';
  else if (kind === 'code') Object.assign(b, { l: 'SQL 内容', v: '{{sql}}', mode: 'mask', n: cat.limits.code_default }); // i18n-ignore
  else if (kind === 'link') Object.assign(b, { l: '查看详情', v: '{{url}}', as: 'button' }); // i18n-ignore
  cur.tpl.blocks.push(b);
  renderBlocks();
  touch();
}

// ---- 变量面板 ----

function renderVars() {
  const box = $('nt-vars');
  box.replaceChildren();
  const have = new Set(cur.def.vars);
  const groups = [];
  for (const v of cat.vars) {
    if (!have.has(v.key)) continue;
    let g = groups.find((x) => x.name === v.group);
    if (!g) groups.push((g = { name: v.group, items: [] }));
    g.items.push(v);
  }
  for (const g of groups) {
    const d = el('div', 'nt-vg');
    d.append(el('span', '', tp(g.name)));
    for (const v of g.items) {
      const c = el('button', 'nt-chip', tp(v.name));
      c.type = 'button';
      c.title = t('{name}，样例：{example}', { name: tp(v.name), example: String(v.example).slice(0, 60) });
      c.append(el('code', '', `{{${v.key}}}`));
      c.addEventListener('mousedown', (e) => e.preventDefault()); // 不抢走输入框的焦点
      c.addEventListener('click', () => insertVar(v.key));
      d.append(c);
    }
    box.append(d);
  }
}

function insertVar(key) {
  const tag = `{{${key}}}`;
  const target = focusEl && document.body.contains(focusEl) ? focusEl : $('nt-ttl');
  const a = target.selectionStart ?? target.value.length;
  const b = target.selectionEnd ?? a;
  target.value = target.value.slice(0, a) + tag + target.value.slice(b);
  target.focus();
  target.setSelectionRange(a + tag.length, a + tag.length);
  target.dispatchEvent(new Event('input', { bubbles: true }));
}

// ---- 预览 ----

function renderPtabs() {
  const box = $('nt-ptabs');
  box.replaceChildren();
  for (const [v, label] of PTYPES) {
    const b = el('button', 'tab' + (ptype === v ? ' on' : ''), label);
    b.type = 'button';
    b.setAttribute('role', 'tab');
    b.setAttribute('aria-selected', String(ptype === v));
    b.addEventListener('click', () => { ptype = v; renderPtabs(); schedulePreview(0); });
    box.append(b);
  }
}

function draft() {
  const d = cur.tpl;
  return { event: cur.key, scope: cur.scope, name: d.name || '', enabled: !!d.enabled, title: d.title || '', tone: d.tone || 'info', icon: d.icon || '', blocks: d.blocks };
}

function schedulePreview(delay) {
  clearTimeout(pvTimer);
  pvTimer = setTimeout(runPreview, delay);
}

async function runPreview() {
  if (!cur) return;
  const seq = ++pvSeq;
  let r;
  try {
    r = await api('POST', `${BASE}/preview`, { ...draft(), channel_type: ptype });
  } catch (e) {
    if (seq !== pvSeq) return;
    $('nt-pv').replaceChildren(el('div', 'alert err', e.message));
    return;
  }
  if (seq !== pvSeq) return;
  renderIssues(r.issues || []);
  const box = $('nt-pv');
  box.replaceChildren();
  if (!r.preview) {
    box.append(el('p', 'muted small', t('模板有待修正的问题，暂无预览。')));
    return;
  }
  const pv = r.preview;
  const m = pv.message;
  if (!m) { box.append(el('p', 'muted small', t('没有套用模板。'))); return; }
  if (ptype === 'email') {
    let subject = '';
    try { subject = JSON.parse(pv.payload).subject || ''; } catch (_) { /* 非 JSON */ }
    box.append(el('div', 'nt-pv-meta', t('主题：{subject}', { subject })));
  }
  box.append(renderMsg(m, ptype));
  const meta = el('div', 'nt-pv-meta');
  meta.append(el('span', '', t('请求体 {n} 字节', { n: new Blob([pv.payload]).size })), el('span', '', t('状态色：{tone}', { tone: TONE_LABEL[m.tone] || m.tone })));
  box.append(meta);
  const det = el('details');
  det.append(el('summary', '', t('请求体（JSON）')));
  let pretty = pv.payload;
  try { pretty = JSON.stringify(JSON.parse(pv.payload), null, 2); } catch (_) { /* 原样 */ }
  det.append(el('pre', '', pretty));
  box.append(det);
}

// 预览里的消息是模板渲染出来的实际内容（语言属于接收方），不随界面语言翻译；这里只画布局
function renderMsg(m, type) {
  const root = el('div', `nt-pv-msg ${type} ${m.tone}`);
  root.append(el('div', 'hd', (m.icon ? m.icon + ' ' : '') + m.title));
  for (const b of m.blocks || []) {
    if (b.t === 'fields') {
      if (type === 'slack' || type === 'feishu') {
        const g = el('div', 'flds');
        for (const r of b.rows) {
          const d = el('div', r.short ? '' : 'full');
          d.append(el('b', '', r.l), document.createTextNode(r.v));
          g.append(d);
        }
        root.append(g);
      } else {
        const ul = el('ul', 'ln');
        for (const r of b.rows) {
          const li = el('li');
          li.append(el('b', '', r.l), document.createTextNode('：' + r.v));
          ul.append(li);
        }
        root.append(ul);
      }
    } else if (b.t === 'text') {
      root.append(el('div', 'pre', b.v));
    } else if (b.t === 'code') {
      if (b.l) root.append(el('div', 'cap', b.l));
      root.append(el('pre', '', b.v));
    } else if (b.t === 'link') {
      if (b.as === 'button') {
        const d = el('div');
        d.append(el('span', 'btnx', b.l));
        root.append(d);
      } else {
        const d = el('div');
        d.append(document.createTextNode(b.l + '：'), el('span', 'lnk', b.v));
        root.append(d);
      }
    } else root.append(document.createElement('hr'));
  }
  return root;
}

// 校验问题的文字：整串能匹配目录就直接翻译；“位置：原因”这种拼出来的串按第一个“：”拆开，分别翻译
function issueText(s) {
  const out = tp(s);
  if (out !== s || isZh) return out;
  const i = s.indexOf('：');
  return i > 0 ? tp(s.slice(0, i)) + ': ' + tp(s.slice(i + 1)) : s;
}

function renderIssues(list) {
  lastIssues = list;
  const box = $('nt-issues');
  box.replaceChildren();
  const crit = list.filter((i) => i.level === 'crit');
  const warn = list.filter((i) => i.level !== 'crit');
  const add = (items, kind, head) => {
    if (!items.length) return;
    const d = el('div', 'alert ' + kind);
    d.append(el('b', '', head));
    const ul = el('ul');
    for (const i of items) ul.append(el('li', '', issueText(i.text)));
    d.append(ul);
    box.append(d);
  };
  add(crit, 'err', t('这些问题要先改掉才能保存：'));
  add(warn, 'warn', t('提示：'));
  $('nt-save').disabled = crit.length > 0;
}

// ---- 保存 / 恢复 / 测试 / 历史 ----

async function save() {
  const body = { ...draft(), note: $('nt-note').value.trim() };
  const def = cur.def;
  const r = await confirmRun({
    title: t('保存「{name}」的模板？', { name: tpName(def.name) }), level: 'normal', code: 'notify.edit', codeName: t('新建 / 编辑渠道与 @ 规则'),
    lines: [t('保存后，这个范围下该类消息按新模板发送（最多 20 秒内生效）。')],
    details: [{ k: t('消息'), v: tpName(def.name) }, { k: t('范围'), v: scopeLabel(cur.scope) }, { k: t('标题'), v: body.title }, { k: t('状态'), v: body.enabled ? t('启用|status') : t('停用|status') }],
    okText: t('保存'),
  }, () => api('PUT', `${BASE}/item`, body));
  if (!r.ok) return;
  cur.saved = true;
  cur.base = JSON.stringify(norm(cur.tpl));
  try { cat = { ...cat, ...(await api('GET', `${BASE}/catalog`)) }; } catch (_) { /* 摘要刷新失败不影响保存结果 */ }
  const w = r.result && r.result.issues ? r.result.issues.filter((i) => i.level !== 'crit') : [];
  renderList();
  cur.savedInfo = { ...norm(cur.tpl), updated_by: t('我'), updated_at: new Date().toLocaleString() };
  renderSource();
  $('nt-reset').hidden = false;
  toast(w.length ? t('已保存（有提示，见页面）') : t('已保存'));
}

async function reset() {
  const up = cur.scope === 'global' ? t('系统默认文案') : t('全局模板（没有则是系统默认文案）');
  const r = await confirmRun({
    title: t('删除这个范围的模板？'), level: 'high', code: 'notify.edit', codeName: t('新建 / 编辑渠道与 @ 规则'),
    lines: [t('删除后「{scope}」下的「{name}」恢复为{up}。删除前的内容保留在历史里，可以载入回来。', { scope: scopeLabel(cur.scope), name: tpName(cur.def.name), up })],
    details: [{ k: t('消息'), v: tpName(cur.def.name) }, { k: t('范围'), v: scopeLabel(cur.scope) }],
    okText: t('确认删除'),
  }, () => api('DELETE', `${BASE}/item?event=${encodeURIComponent(cur.key)}&scope=${encodeURIComponent(cur.scope)}&note=${encodeURIComponent($('nt-note').value.trim())}`));
  if (!r.ok) return;
  cat = { ...cat, ...(await api('GET', `${BASE}/catalog`)) };
  cur.base = JSON.stringify(norm(cur.tpl)); // 已经删除：当前内容视为已处理，直接重新打开不提示放弃修改
  await open(cur.key, cur.scope);
  toast(t('已恢复'));
}

async function sendTest() {
  const id = Number($('nt-test-ch').value);
  const res = $('nt-test-result');
  res.replaceChildren();
  if (!id) { res.append(el('div', 'alert warn', t('请先选择渠道。'))); return; }
  const c = chanOf(id);
  const btn = $('nt-test');
  btn.disabled = true;
  try {
    const r = await api('POST', `${BASE}/test`, { ...draft(), channel_id: id, test_to: $('nt-test-to').value.trim() });
    if (r && r.stages) { // 邮件
      res.append(el('div', 'alert ' + (r.ok ? 'info' : 'err'), r.ok ? t('已发送到 {to}。', { to: r.to || t('渠道配置的收件人') }) : t('发送失败：') + tp(r.error || '')));
    } else {
      res.append(el('div', 'alert info', t('已发送到「{name}」，请到对应群 / 邮箱查看效果。', { name: c ? c.name : id })));
    }
  } catch (e) {
    res.append(el('div', 'alert err', e.message));
  } finally {
    btn.disabled = false;
  }
}

async function showHistory() {
  const box = $('nt-hist');
  box.hidden = false;
  box.replaceChildren(el('p', 'muted small', t('加载中…')));
  let r;
  try {
    r = await api('GET', `${BASE}/history?event=${encodeURIComponent(cur.key)}&scope=${encodeURIComponent(cur.scope)}`);
  } catch (e) { box.replaceChildren(el('div', 'alert err', e.message)); return; }
  box.replaceChildren();
  const list = r.history || [];
  if (!list.length) { box.append(el('p', 'muted small', t('这个范围还没有修改历史。'))); return; }
  const tb = el('table');
  const hr = el('tr');
  for (const h of [t('时间'), t('操作人'), t('动作'), t('说明'), '']) hr.append(el('th', '', h));
  tb.append(hr);
  for (const h of list) {
    const tr = el('tr');
    tr.append(el('td', '', h.at), el('td', '', h.operator), el('td', '', h.action === 'delete' ? t('删除') : t('保存')), el('td', '', h.note || ''));
    const td = el('td');
    if (h.snapshot) {
      const b = el('button', 'btn', t('载入这个版本'));
      b.type = 'button';
      b.addEventListener('click', () => {
        const s = h.snapshot;
        cur.tpl = { ...cur.tpl, name: s.name || '', enabled: s.enabled !== false, title: s.title || '', tone: s.tone || 'info', icon: s.icon || '', blocks: clone(s.blocks || []) };
        renderEditor();
        touch();
        toast(t('已载入到编辑器（还没保存）'));
      });
      td.append(b);
    }
    tr.append(td);
    tb.append(tr);
  }
  box.append(tb);
}

// ---- 初始化 ----

function fillScope() {
  const sel = $('nt-scope');
  const off = ' · ' + t('已停用');
  sel.replaceChildren(new Option(scopeLabel('global'), 'global'));
  for (const c of cat.channels) sel.append(new Option(t('渠道：{name}（{type}）', { name: c.name, type: tp(c.label) }) + (c.enabled ? '' : off), 'c:' + c.id));
  const ch = $('nt-test-ch');
  ch.replaceChildren(new Option(t('选择渠道…'), '0'));
  for (const c of cat.channels) ch.append(new Option(`${c.name}${isZh ? '（' : ' ('}${tp(c.label)}${isZh ? '）' : ')'}${c.enabled ? '' : off}`, String(c.id)));
}

async function init() {
  try {
    cat = await api('GET', `${BASE}/catalog`);
  } catch (e) { setMsg(e.message); return; }
  if (!cat.ready) {
    setMsg(t('消息模板表未创建，请先执行 deploy/sql/260_notify_template.sql。'), 'warn');
  }
  fillScope();
  renderList();
  bindMeta();
  $('nt-q').addEventListener('input', renderList);
  $('nt-scope').addEventListener('change', (e) => open(cur.key, e.target.value));
  $('nt-add').addEventListener('click', (e) => { const btn = e.target.closest('[data-add]'); if (btn) addBlock(btn.dataset.add); });
  $('nt-editor').addEventListener('focusin', (e) => { if (e.target.dataset && e.target.dataset.tpl) focusEl = e.target; });
  $('nt-save').addEventListener('click', save);
  $('nt-reset').addEventListener('click', reset);
  $('nt-test').addEventListener('click', sendTest);
  $('nt-history').addEventListener('click', showHistory);
  $('nt-test-ch').addEventListener('change', (e) => {
    const c = chanOf(e.target.value);
    $('nt-test-to').hidden = !(c && c.type === 'email');
  });
  window.addEventListener('beforeunload', (e) => { if (isDirty()) { e.preventDefault(); e.returnValue = ''; } });
  // 地址里带 #ev=sql.finished&sc=global 时直接打开（从别处跳转过来用）
  const h = new URLSearchParams(location.hash.slice(1));
  if (h.get('ev') && cat.messages.find((m) => m.key === h.get('ev'))) await open(h.get('ev'), h.get('sc') || 'global');
}

init();
