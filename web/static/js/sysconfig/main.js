/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: main.js
 * @time: 2026/10/28
 */
// 系统管理 › 系统配置（仅超级用户）：本系统自己的设置中心。
// 数据与规则都在服务端 internal/settings：每项有类型、默认值、取值范围；这里只负责展示、收集修改、批量保存。
// 敏感项（密钥 / 密码）只显示“已设置”，留空 = 不修改。所有文字用 textContent 写入，不解析 HTML。
import { api, confirmRun, toast } from '../common.js';
import { t, tp, fmtDateTime } from '../i18n.js';

const $ = (id) => document.getElementById(id);

function h(tag, props, ...kids) {
  const e = document.createElement(tag);
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v === undefined || v === null || v === false) continue;
      if (k === 'text') e.textContent = v;
      else if (k === 'class') e.className = v;
      else e.setAttribute(k, v === true ? '' : v);
    }
  }
  for (const k of kids) if (k !== null && k !== undefined && k !== false) e.append(k);
  return e;
}

// 页面状态。drafts：键 → { v }，v 为新值；null / '' 表示恢复默认（敏感项 = 清除）。
const S = {
  data: null,
  byKey: new Map(),
  group: '',
  q: '',
  onlyMod: false,
  drafts: new Map(),
  errors: new Map(), // 键 → 服务端校验错误
  secretEdit: new Set(), // 正在输入新值的敏感项
};

const hist = { key: '', offset: 0, done: false };

// ---- 小工具 ----

function show(v) {
  return v === '' || v === undefined || v === null ? t('（空）') : String(v);
}

function typeLabel(it) {
  return it.unit ? ` ${tp(it.unit)}` : '';
}

function currentText(it) {
  if (it.type === 'secret') return it.secret_set ? t('（已设置，内容不显示）') : t('（未设置）');
  const v = it.custom && it.value !== '' ? it.value : it.default;
  const base = v === '' ? t('（空）') : v;
  return it.custom && it.value !== '' ? base : t('{value}（默认）', { value: base });
}

function optionLabel(it, v) {
  const o = (it.options || []).find((x) => x.value === v);
  return o ? tp(o.label) : v;
}

function displayValue(it, v) {
  if (it.type === 'bool') return v === 'true' ? t('开启') : v === 'false' ? t('关闭|off') : show(v);
  if (it.type === 'enum') return optionLabel(it, v);
  return show(v);
}

// 把用户当前选择换算成“保存后的生效值”，用于确认弹窗的差异。
function afterText(it, d) {
  if (it.type === 'secret') return d.v === null || d.v === '' ? t('（清除）') : t('（新值，不显示）');
  if (d.v === null || d.v === '') return t('{value}（默认）', { value: displayValue(it, it.default) });
  return displayValue(it, d.v);
}

function beforeText(it) {
  if (it.type === 'secret') return it.secret_set ? t('（已设置）') : t('（未设置）');
  const v = it.custom && it.value !== '' ? it.value : it.default;
  return it.custom && it.value !== '' ? displayValue(it, v) : t('{value}（默认）', { value: displayValue(it, v) });
}

// 草稿与服务端当前状态一致时就不算修改。
function sameAsServer(it, v) {
  if (it.type === 'secret') return false; // 敏感项只要提交了就算修改
  const explicit = it.custom ? it.value : '';
  if (v === null || v === '') return !it.custom || explicit === '';
  return explicit === v || (!it.custom && v === it.default);
}

function setDraft(key, v) {
  const it = S.byKey.get(key);
  if (!it) return;
  S.errors.delete(key);
  if (sameAsServer(it, v)) S.drafts.delete(key);
  else S.drafts.set(key, { v });
  const row = document.querySelector(`.st-row[data-key="${CSS.escape(key)}"]`);
  if (row) {
    row.classList.toggle('dirty', S.drafts.has(key));
    const err = row.querySelector('.st-err');
    if (err) err.remove();
  }
  renderBar();
  renderNav();
}

// ---- 提示区 ----

function renderAlerts() {
  const box = $('st-alerts');
  box.replaceChildren();
  const d = S.data;
  if (!d.ready) {
    box.append(h('div', { class: 'alert err', text: t('设置表 dbt_setting 不存在：请先在元数据库执行 deploy/sql/260_setting.sql（install.sh upgrade 会自动执行），在此之前所有设置按默认值运行。') }));
    return;
  }
  if (d.security.public_secret_key) {
    box.append(h('div', { class: 'alert warn', text: t('⚠ SECRET_KEY 仍是公开的缺省值，任何人都能伪造会话，请换成随机值（dbt-admin gen-secrets）。') }));
  }
  const bad = d.undecryptable || [];
  if (bad.length) {
    const names = bad.slice(0, 12).join(t('、|sep')) + (bad.length > 12 ? ' ' + t('等 {n} 项', { n: bad.length }) : '');
    box.append(h('div', { class: 'alert err', text: t('有 {n} 项敏感设置的密文用当前密钥（SECRET_KEY / MIRAGE_SECRET_KEY）解不开：{names}。它们被当成未设置，请在对应分组里重新填写（goInception 的连接信息在“审核引擎”页）。', { n: bad.length, names }) }));
  }
}

// ---- 左侧导航 ----

function groupDirty(g) {
  for (const k of S.drafts.keys()) if (S.byKey.get(k)?.group === g) return true;
  return false;
}

function renderNav() {
  const nav = $('st-nav');
  nav.replaceChildren();
  for (const g of S.data.groups) {
    const on = !S.q && g.key === S.group;
    const b = h('button', { type: 'button', 'data-group': g.key, class: on ? 'on' : '', 'aria-current': on ? 'true' : false });
    b.append(h('span', { class: 'st-n-label', text: tp(g.label) }));
    if (groupDirty(g.key)) b.append(h('span', { class: 'st-n-dot', title: t('有未保存的修改') }));
    if (g.issues) b.append(h('span', { class: 'st-n-badge bad', text: String(g.issues), title: t('密文解不开') }));
    else if (g.modified) b.append(h('span', { class: 'st-n-badge mod', text: String(g.modified), title: t('{n} 项与默认值不同', { n: g.modified }) }));
    nav.append(b);
  }
}

// ---- 设置项 ----

function control(it) {
  const draft = S.drafts.get(it.key);
  const locked = !!it.locked;
  const wrap = h('div', { class: 'st-ctl' });
  const label = t('{name}（{code}）', { name: tp(it.label), code: it.key });
  // 当前展示的值：草稿 > 显式值 > 默认值
  let cur;
  if (draft) cur = draft.v === null || draft.v === '' ? it.default : draft.v;
  else cur = it.custom && it.value !== '' ? it.value : it.default;

  switch (it.type) {
    case 'bool': {
      const cb = h('input', { type: 'checkbox', 'aria-label': label });
      cb.checked = cur === 'true';
      cb.disabled = locked;
      cb.addEventListener('change', () => setDraft(it.key, cb.checked ? 'true' : 'false'));
      const txt = h('span', { text: cb.checked ? t('开启') : t('关闭|off') });
      cb.addEventListener('change', () => { txt.textContent = cb.checked ? t('开启') : t('关闭|off'); });
      wrap.append(h('label', { class: 'st-switch' }, cb, txt));
      break;
    }
    case 'enum': {
      const sel = h('select', { 'aria-label': label });
      for (const o of it.options || []) sel.append(new Option(tp(o.label), o.value));
      sel.value = cur;
      sel.disabled = locked;
      sel.addEventListener('change', () => setDraft(it.key, sel.value));
      wrap.append(sel);
      break;
    }
    case 'int':
    case 'float': {
      const inp = h('input', { type: 'number', step: it.type === 'int' ? '1' : 'any', inputmode: 'numeric', 'aria-label': label, placeholder: it.default || '' });
      if (it.ranged) { inp.min = String(it.min); inp.max = String(it.max); }
      inp.value = draft ? (draft.v === null ? '' : draft.v) : (it.custom ? it.value : '');
      inp.disabled = locked;
      inp.addEventListener('input', () => setDraft(it.key, inp.value.trim()));
      const line = h('div', { class: 'st-unit-wrap' }, inp);
      if (it.unit) line.append(h('span', { class: 'st-unit', text: tp(it.unit) }));
      wrap.append(line);
      break;
    }
    case 'text': {
      const ta = h('textarea', { 'aria-label': label, spellcheck: 'false', placeholder: it.default || tp(it.placeholder) || '' });
      ta.value = draft ? (draft.v === null ? '' : draft.v) : (it.custom ? it.value : '');
      ta.disabled = locked;
      ta.addEventListener('input', () => setDraft(it.key, ta.value));
      wrap.append(ta);
      break;
    }
    case 'secret': {
      const editing = S.secretEdit.has(it.key) || (!it.secret_set && !it.undecryptable);
      if (draft && (draft.v === null || draft.v === '')) {
        wrap.append(h('div', { class: 'st-secret-line' }, h('span', { class: 'st-tag bad', text: t('将清除') }),
          h('button', { type: 'button', class: 'st-link-btn', 'data-act': 'undo', text: t('撤销') })));
      } else if (editing && !locked) {
        const inp = h('input', { type: 'password', autocomplete: 'new-password', 'aria-label': t('{label} 的新值', { label }), placeholder: it.secret_set ? t('输入新值（留空不修改）') : t('输入值') });
        if (draft) inp.value = draft.v;
        inp.addEventListener('input', () => { if (inp.value === '') S.drafts.delete(it.key); else setDraft(it.key, inp.value); renderBar(); });
        const line = h('div', { class: 'st-secret-line' }, inp);
        if (it.secret_set) line.append(h('button', { type: 'button', class: 'st-link-btn', 'data-act': 'secret-cancel', text: t('取消') }));
        wrap.append(line);
      } else {
        const line = h('div', { class: 'st-secret-line' });
        line.append(h('span', { class: 'st-tag ' + (it.undecryptable ? 'bad' : 'set'), text: it.undecryptable ? t('密文解不开') : t('已设置') }));
        if (!locked) {
          line.append(h('button', { type: 'button', class: 'st-btn', 'data-act': 'secret-edit', text: t('修改') }));
          if (it.secret_set) line.append(h('button', { type: 'button', class: 'st-btn', 'data-act': 'secret-clear', text: t('清除') }));
        }
        wrap.append(line);
      }
      break;
    }
    default: {
      const inp = h('input', { type: 'text', spellcheck: 'false', autocomplete: 'off', 'aria-label': label, placeholder: it.default || tp(it.placeholder) || '' });
      inp.value = draft ? (draft.v === null ? '' : draft.v) : (it.custom ? it.value : '');
      inp.disabled = locked;
      inp.addEventListener('input', () => setDraft(it.key, inp.value.trim()));
      wrap.append(inp);
    }
  }
  const err = S.errors.get(it.key);
  if (err) wrap.append(h('div', { class: 'st-err', role: 'alert', text: tp(err) }));
  return wrap;
}

function row(it) {
  const draft = S.drafts.has(it.key);
  const left = h('div');
  const lab = h('div', { class: 'st-label' }, tp(it.label));
  if (it.modified) lab.append(h('span', { class: 'st-tag mod', text: it.type === 'secret' ? t('已设置') : t('已修改') }));
  if (it.risk) lab.append(h('span', { class: 'st-tag risk', title: tp(it.risk), text: t('高危') }));
  if (it.locked) lab.append(h('span', { class: 'st-tag lock', text: t('只读') }));
  left.append(lab, h('div', { class: 'st-key', text: it.key }));
  if (it.help) left.append(h('p', { class: 'st-help', text: tp(it.help) }));
  const meta = h('div', { class: 'st-meta' });
  if (it.type !== 'secret') {
    const dv = it.default === '' ? t('（空）') : displayValue(it, it.default) + typeLabel(it);
    meta.append(h('span', { text: t('默认：{value}', { value: dv }) }));
  }
  if (it.ranged) meta.append(h('span', { text: t('范围：{min} ~ {max}{unit}', { min: it.min, max: it.max, unit: typeLabel(it) }) }));
  if (it.link) meta.append(h('a', { href: it.link.href, text: t('前往「{label}」页', { label: tp(it.link.label) }) }));
  if (it.custom && it.type !== 'secret' && it.modified && !(draft && S.drafts.get(it.key).v === '')) {
    meta.append(h('button', { type: 'button', class: 'st-link-btn', 'data-act': 'reset', text: t('恢复默认') }));
  }
  if (it.updated_at) meta.append(h('button', { type: 'button', class: 'st-link-btn', 'data-act': 'history', text: t('变更记录（{by}）', { by: it.updated_by || t('系统') }) }));
  left.append(meta);
  if (it.locked) left.append(h('p', { class: 'st-help', text: t('页面里不能修改：{reason}', { reason: tp(it.locked) }) }));
  if (it.risk) left.append(h('p', { class: 'st-help', text: '⚠ ' + tp(it.risk) }));

  const r = h('div', { class: 'st-row' + (draft ? ' dirty' : '') + (it.locked ? ' locked' : ''), 'data-key': it.key });
  r.append(left, control(it));
  return r;
}

function matches(it) {
  if (S.onlyMod && !(it.modified || S.drafts.has(it.key))) return false;
  const q = S.q.trim().toLowerCase();
  if (q) {
    const hay = `${it.key} ${tp(it.label)} ${tp(it.help) || ''} ${tp(it.section) || ''}`.toLowerCase();
    for (const w of q.split(/\s+/)) if (!hay.includes(w)) return false;
  }
  return true;
}

function renderContent() {
  const box = $('st-content');
  box.replaceChildren();
  if (!S.data.ready) return;
  const items = S.data.items.filter((it) => matches(it));
  if (S.q.trim()) {
    if (!items.length) { box.append(h('div', { class: 'st-empty', text: t('没有符合条件的设置项') })); return; }
    let last = '';
    let sec = h('div', { class: 'st-sec' });
    for (const it of items) {
      if (it.group !== last) {
        last = it.group;
        const gl = S.data.groups.find((g) => g.key === it.group)?.label;
        box.append(h('div', { class: 'st-search-group', text: gl ? tp(gl) : it.group }));
        sec = h('div', { class: 'st-sec' });
        box.append(sec);
      }
      sec.append(row(it));
    }
    return;
  }
  const g = S.data.groups.find((x) => x.key === S.group) || S.data.groups[0];
  if (!g) return;
  box.append(h('div', { class: 'st-group-head' }, h('h2', { text: tp(g.label) }), g.desc ? h('p', { text: tp(g.desc) }) : null));
  const own = items.filter((it) => it.group === g.key);
  if (!own.length) { box.append(h('div', { class: 'st-empty', text: S.onlyMod ? t('这个分组里没有已修改的设置') : t('这个分组里没有设置项') })); return; }
  let curSec = null;
  let secBox = null;
  for (const it of own) {
    if (it.section !== curSec) {
      curSec = it.section;
      secBox = h('div', { class: 'st-sec' }, h('h3', { text: tp(it.section) }));
      box.append(secBox);
    }
    secBox.append(row(it));
  }
}

function renderBar() {
  const n = S.drafts.size;
  $('st-bar').hidden = n === 0;
  $('st-bar-text').textContent = n ? t('有 {n} 项修改尚未保存', { n }) : '';
}

function render() {
  renderAlerts();
  renderNav();
  renderContent();
  renderBar();
}

// ---- 保存 ----

async function save() {
  if (!S.drafts.size) return;
  const changes = {};
  const rows = [];
  const risks = [];
  for (const [key, d] of S.drafts) {
    const it = S.byKey.get(key);
    changes[key] = d.v === '' ? null : d.v;
    rows.push({ label: t('{name}（{code}）', { name: tp(it.label), code: key }), from: beforeText(it), to: afterText(it, d) });
    if (it.risk) risks.push(t('{label}：{risk}', { label: tp(it.label), risk: tp(it.risk) }));
  }
  const opts = { title: t('保存 {n} 项系统设置', { n: rows.length }), okText: t('保存'), rows,
    lines: [t('保存后立即生效（多实例部署最多延迟几秒），并写入变更记录与系统审计；敏感项加密保存，不记入内容。')] };
  if (risks.length) {
    opts.level = 'high';
    opts.typed = t('确认修改');
    opts.typedLabel = t('包含高危项：\n{risks}\n请原样输入以下文本确认：', { risks: risks.join('\n') });
  }
  const r = await confirmRun(opts, async () => {
    try {
      return await api('PUT', '/api/v1/settings/config', { changes });
    } catch (e) {
      const f = e.data && e.data.fields;
      if (f) {
        S.errors = new Map(Object.entries(f));
        renderContent();
      }
      throw e;
    }
  });
  if (r.noChange) {
    S.drafts.clear();
    renderBar();
    renderContent();
    return;
  }
  if (!r.ok) return;
  const n = (r.result && r.result.changes ? r.result.changes.length : 0);
  S.drafts.clear();
  S.errors.clear();
  S.secretEdit.clear();
  toast(n ? t('已保存 {n} 项', { n }) : t('没有变化'));
  await load();
}

// ---- 变更记录 ----

function histItem(x) {
  const top = h('div', { class: 'st-h-top' });
  top.append(h('b', { text: tp(x.label) || x.key }), h('span', { class: 'st-key', text: x.key }),
    h('span', { class: 'st-tag', text: { set: t('修改'), reset: t('恢复默认') }[x.action] || x.action }));
  const when = x.changed_at ? fmtDateTime(x.changed_at, { hour12: false }) : '';
  const meta = h('div', { class: 'st-h-top' }, h('span', { class: 'st-h-time', text: when }), h('span', { class: 'muted', text: x.by || t('系统') }));
  const diff = h('div', { class: 'st-h-diff' });
  if (x.secret) diff.append(document.createTextNode(t('敏感项：内容不记录')));
  else {
    if (x.old !== '') diff.append(h('s', { text: x.old }), document.createTextNode(' → '));
    else diff.append(document.createTextNode(t('（原为默认）→') + ' '));
    diff.append(x.action === 'reset' ? document.createTextNode(t('默认值')) : h('b', { text: x.new === '' ? t('（空）') : x.new }));
  }
  return h('div', { class: 'st-h-item' }, top, meta, diff);
}

async function loadHistory(reset) {
  const body = $('st-drawer-body');
  if (reset) { hist.offset = 0; hist.done = false; body.replaceChildren(); }
  try {
    const q = new URLSearchParams({ limit: '50', offset: String(hist.offset) });
    if (hist.key) q.set('key', hist.key);
    const res = await api('GET', '/api/v1/settings/config/history?' + q.toString());
    const rows = res.items || [];
    for (const x of rows) body.append(histItem(x));
    hist.offset += rows.length;
    hist.done = rows.length < 50;
    if (!body.children.length) body.append(h('div', { class: 'st-empty', text: t('还没有变更记录') }));
  } catch (e) {
    body.append(h('div', { class: 'alert err', text: e.message || t('加载失败') }));
  }
  $('st-drawer-more').hidden = hist.done;
}

function openHistory(key) {
  hist.key = key || '';
  const it = key ? S.byKey.get(key) : null;
  $('st-drawer-title').textContent = it ? t('变更记录：{name}', { name: tp(it.label) }) : t('变更记录（全部设置）');
  $('st-drawer').hidden = false;
  loadHistory(true);
  $('st-drawer-close').focus();
}

function closeHistory() { $('st-drawer').hidden = true; }

// ---- 加载 ----

async function load() {
  try {
    S.data = await api('GET', '/api/v1/settings/config');
  } catch (e) {
    $('st-alerts').replaceChildren(h('div', { class: 'alert err', text: e.message || t('加载失败') }));
    return;
  }
  S.byKey = new Map(S.data.items.map((it) => [it.key, it]));
  // 草稿里的键在服务端已经不存在（极少见）就丢掉
  for (const k of [...S.drafts.keys()]) if (!S.byKey.has(k)) S.drafts.delete(k);
  const fromHash = decodeURIComponent((location.hash || '').replace(/^#/, ''));
  if (!S.group) S.group = S.data.groups.some((g) => g.key === fromHash) ? fromHash : (S.data.groups[0] || {}).key || '';
  render();
}

// ---- 事件 ----

$('st-nav').addEventListener('click', (e) => {
  const b = e.target.closest('button[data-group]');
  if (!b) return;
  S.group = b.dataset.group;
  S.q = '';
  $('st-q').value = '';
  history.replaceState(null, '', '#' + encodeURIComponent(S.group));
  renderNav();
  renderContent();
});

$('st-content').addEventListener('click', (e) => {
  const b = e.target.closest('button[data-act]');
  if (!b) return;
  const rowEl = b.closest('.st-row');
  const key = rowEl && rowEl.dataset.key;
  if (!key) return;
  const it = S.byKey.get(key);
  switch (b.dataset.act) {
    case 'reset':
      setDraft(key, '');
      renderContent();
      break;
    case 'secret-edit':
      S.secretEdit.add(key);
      renderContent();
      document.querySelector(`.st-row[data-key="${CSS.escape(key)}"] input`)?.focus();
      break;
    case 'secret-cancel':
      S.secretEdit.delete(key);
      S.drafts.delete(key);
      renderBar();
      renderNav();
      renderContent();
      break;
    case 'secret-clear':
      S.drafts.set(key, { v: null });
      S.secretEdit.delete(key);
      renderBar();
      renderNav();
      renderContent();
      break;
    case 'undo':
      S.drafts.delete(key);
      renderBar();
      renderNav();
      renderContent();
      break;
    case 'history':
      openHistory(it.key);
      break;
    default:
  }
});

$('st-q').addEventListener('input', (e) => { S.q = e.target.value; if (S.data) { renderNav(); renderContent(); } });
$('st-only-mod').addEventListener('change', (e) => { S.onlyMod = e.target.checked; if (S.data) renderContent(); });
$('st-save').addEventListener('click', save);
$('st-discard').addEventListener('click', () => {
  S.drafts.clear();
  S.errors.clear();
  S.secretEdit.clear();
  render();
});
$('st-hist-all').addEventListener('click', () => openHistory(''));
$('st-drawer-close').addEventListener('click', closeHistory);
$('st-drawer-more').addEventListener('click', () => loadHistory(false));
$('st-drawer').addEventListener('click', (e) => { if (e.target === $('st-drawer')) closeHistory(); });
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !$('st-drawer').hidden) closeHistory(); });
window.addEventListener('beforeunload', (e) => {
  if (S.drafts.size) { e.preventDefault(); e.returnValue = ''; }
});

load();
