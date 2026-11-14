/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: stores.js
 * @time: 2026/10/28
 */
// 备份存储页签：列表、新建 / 编辑（SK 不回显、留空不改）、测试连接、启用 / 停用、删除。
// 编辑类操作需要 archive.store_edit（锁定码，服务端另外强制超级用户）；查看需要 archive.cfg_view。
import { applyButtons } from '../btnperm.js';
import { t, tp, isZh } from '../i18n.js';
import { A, issuesOf } from './api.js';
import { $, el, button, pbutton, fmtBytes, fmtTime, clip, labelCells, withNodes } from './util.js';
import { flash, notice, tag, openModal, modalMsg, riskConfirm, kvList, bold } from './ui.js';
import { S, STORE_STATUS, limits } from './consts.js';

const root = () => $('ar-view-stores');
const TYPES = [['local', t('本地目录')], ['nfs', t('NFS 挂载目录')], ['s3', t('S3 兼容（含 MinIO）')], ['oss', t('阿里云 OSS（S3 兼容端点）')]];
const typeText = (type) => (TYPES.find((x) => x[0] === type) || [type, type])[1];
const isFs = (type) => type === 'local' || type === 'nfs';
let items = [];
let built = false;

function build() {
  if (built) return;
  built = true;
  const r = root();
  const bad = el('div');
  bad.id = 'ar-s-bad';
  const tb = el('div', 'toolbar');
  const cnt = el('span', 'small muted');
  cnt.id = 'ar-s-count';
  tb.append(cnt, el('span', 'spacer'), pbutton('archive.store_edit', t('＋ 新建备份存储'), 'primary', () => openForm(null)));
  const card = el('section', 'card');
  const table = el('table', 'list ar-cards');
  const th = el('thead');
  const hr = el('tr');
  [t('名称'), t('类型'), t('地址 / 桶 / 前缀'), 'AK / SK', t('保留'), t('状态'), t('最近测试'), t('被引用'), t('操作')].forEach((title) => hr.append(el('th', '', title)));
  th.append(hr);
  const body = el('tbody');
  body.id = 'ar-s-rows';
  table.append(th, body);
  const wrap = el('div', 'ar-tbl-wrap');
  wrap.append(table);
  card.append(wrap);
  r.append(bad, tb, card);
}

function drawInfo() {
  const n = items.filter((s) => s.status === 'err').length;
  const bad = $('ar-s-bad');
  bad.replaceChildren();
  if (n) bad.append(notice(t('有 {n} 个备份存储连接异常，引用它的任务将被拒绝运行。', { n }), 'bad', '!'));
}

function maskAk(ak) {
  return ak || '—';
}

function row(s) {
  const tr = el('tr');
  const name = el('td');
  name.append(el('b', '', s.name));
  const addr = el('td');
  if (isFs(s.type)) addr.append(el('code', '', s.path || '—'));
  else {
    addr.append(el('code', '', s.endpoint || '—'));
    if (s.bucket) addr.append(el('div', 'small muted', t('桶 {name}', { name: s.bucket })));
  }
  addr.append(el('div', 'small muted', t('前缀 {name}', { name: s.prefix || '/' })));
  const ak = el('td');
  if (isFs(s.type)) ak.append(el('span', 'muted', '—'));
  else ak.append(el('span', 'small mono', maskAk(s.ak)), document.createTextNode(' / '), s.sk_set ? el('span', 'small', t('●●●●●●（已保存）')) : el('span', 'small muted', t('未设置')));
  const st = STORE_STATUS[s.status] || [s.status, ''];
  const stat = el('td');
  stat.append(tag(st[0], st[1]));
  if (s.status === 'ok' && s.free_bytes !== null && s.free_bytes !== undefined) stat.append(el('div', 'small muted', t('可用 {size}', { size: fmtBytes(s.free_bytes) })));
  const lt = el('td');
  if (s.last_test) {
    lt.append(el('span', 'small ar-nowrap', fmtTime(s.last_test.at, 16).slice(5)));
    const m = el('div', 'small ' + (s.last_test.ok ? 'muted' : ''), clip(tp(s.last_test.msg), 46));
    if (!s.last_test.ok) m.style.color = 'var(--dt-crit)';
    m.title = tp(s.last_test.msg) || '';
    lt.append(m);
  } else lt.append('—');
  const used = el('td', '', s.used_by ? t('{n} 个任务', { n: s.used_by }) : '0');
  const ops = el('td');
  const w = el('div', 'ar-ops');
  w.append(
    pbutton('archive.store_edit', t('编辑'), 'ghost', () => openForm(s)),
    pbutton('archive.store_edit', t('测试连接'), 'ghost', () => testSaved(s)),
    pbutton('archive.store_edit', s.enabled ? t('停用') : t('启用'), 'ghost', () => toggle(s)),
    pbutton('archive.store_edit', t('删除'), 'ghost danger', () => del(s)),
  );
  ops.append(w);
  tr.append(name, el('td', '', typeText(s.type)), addr, ak, el('td', '', s.retention_days ? t('{n} 天', { n: s.retention_days }) : t('永久')), stat, lt, used, ops);
  return tr;
}

function render() {
  drawInfo();
  $('ar-s-count').textContent = t('共 {n} 个存储', { n: items.length });
  const body = $('ar-s-rows');
  body.replaceChildren();
  if (!items.length) {
    const tr = el('tr');
    const c = el('td', 'empty', t('还没有备份存储'));
    c.colSpan = 9;
    tr.append(c);
    body.append(tr);
  } else for (const s of items) body.append(row(s));
  labelCells(body.parentNode);
  applyButtons(root());
}

export async function load() {
  try {
    items = await A.stores();
    S.stores = items;
    render();
  } catch (e) {
    if (e.code !== 'not_ready') flash(e.message, 'err');
  }
}

export function show() {
  build();
  root().hidden = false;
  load();
}
export function hide() {
  root().hidden = true;
}
export const refresh = () => load();

// ---- 表单 ----
function field(label, input, hint) {
  const l = el('label', 'fld');
  l.append(document.createTextNode(label), input);
  if (hint) l.append(el('span', 'small muted', hint));
  return l;
}
function inp(type, val, ph) {
  const i = el('input');
  i.type = type;
  if (val !== undefined && val !== null) i.value = val;
  if (ph) i.placeholder = ph;
  i.autocomplete = type === 'password' ? 'new-password' : 'off';
  return i;
}

function openForm(s) {
  const n = !s;
  const f = {
    name: inp('text', n ? '' : s.name, t('例：阿里云 OSS 冷归档')),
    type: el('select'),
    path: inp('text', n ? '' : s.path, t('绝对路径')),
    endpoint: inp('text', n ? '' : s.endpoint, 'https://host:port'),
    region: inp('text', n ? '' : s.region, t('可选')),
    bucket: inp('text', n ? '' : s.bucket, ''),
    prefix: inp('text', n ? '' : s.prefix, 'dbtool/'),
    ak: inp('text', n ? '' : s.ak, ''),
    sk: inp('password', '', n || !s.sk_set ? t('密钥') : t('●●●●●●●●  已保存，留空表示不修改')),
    path_style: inp('checkbox'),
    keep: inp('number', n ? 90 : s.retention_days),
    enabled: inp('checkbox'),
  };
  f.name.maxLength = 50;
  f.keep.min = '0';
  f.keep.max = String(limits().store_retention_max || 36500);
  f.keep.style.maxWidth = '140px';
  TYPES.forEach(([v, label]) => f.type.append(new Option(label, v)));
  f.type.value = n ? 'nfs' : s.type;
  f.path_style.checked = !n && !!s.path_style;
  f.enabled.checked = n ? true : !!s.enabled;
  f.type.disabled = !n && s.used_by > 0;

  const body = el('div', 'ar-modal-form');
  const fsBox = el('div');
  fsBox.append(field(t('目录'), f.path));
  const objBox = el('div');
  const psLab = el('label', 'chk');
  psLab.style.margin = '8px 0';
  psLab.append(f.path_style, document.createTextNode(' ' + t('路径风格访问')));
  objBox.append(field('Endpoint', f.endpoint), field('Region', f.region), field(t('桶'), f.bucket), field('AK', f.ak), field('SK', f.sk), psLab);
  const enLab = el('label', 'chk');
  enLab.append(f.enabled, document.createTextNode(' ' + t('启用')));
  body.append(field(t('名称'), f.name), field(t('类型'), f.type, f.type.disabled ? t('已被任务引用，不能修改类型') : ''), fsBox, objBox, field(t('路径前缀'), f.prefix),
    field(t('保留天数（0 = 永久）'), f.keep));
  if (n) body.append(enLab);
  const testOut = el('div', 'small muted');
  const testBtn = button(t('测试连接'), '', async () => {
    testBtn.disabled = true;
    testOut.replaceChildren(document.createTextNode(t('测试中…')));
    try {
      const r = await A.testStoreForm(payload(true));
      testOut.replaceChildren(tag(r.ok ? t('连接成功') : r.kind === 'timeout' ? t('超时') : t('失败'), r.ok ? 'ok' : 'bad'), document.createTextNode(' ' + (tp(r.msg) || '')));
    } catch (e) {
      testOut.replaceChildren(tag(t('失败'), 'bad'), document.createTextNode(' ' + e.message));
    } finally {
      testBtn.disabled = false;
    }
  });
  const bar = el('div', 'toolbar');
  bar.style.marginTop = '10px';
  bar.append(testBtn, testOut);
  body.append(bar);

  const sync = () => {
    const fs = isFs(f.type.value);
    fsBox.hidden = !fs;
    objBox.hidden = fs;
  };
  f.type.addEventListener('change', sync);
  sync();

  function payload(forTest) {
    const o = {
      name: f.name.value.trim(), type: f.type.value, path: f.path.value.trim(), endpoint: f.endpoint.value.trim(), region: f.region.value.trim(),
      bucket: f.bucket.value.trim(), prefix: f.prefix.value.trim(), ak: f.ak.value.trim(), path_style: f.path_style.checked,
      retention_days: Number(f.keep.value), enabled: n ? f.enabled.checked : s.enabled,
    };
    if (isFs(o.type)) { o.endpoint = ''; o.bucket = ''; o.ak = ''; o.region = ''; o.path_style = false; }
    else o.path = '';
    if (f.sk.value) o.sk = f.sk.value;
    if (!n && forTest) o.id = s.id;
    return o;
  }

  openModal({
    title: n ? t('新建备份存储') : t('编辑备份存储 · {name}', { name: s.name }), body, size: 'wide', ok: t('保存'),
    onOk: async () => {
      const o = payload(false);
      const errs = [];
      if (o.name.length < 2 || o.name.length > 50) errs.push(t('名称需要 2–50 个字'));
      if (isFs(o.type) ? !o.path : !o.endpoint) errs.push(isFs(o.type) ? t('目录不能为空') : t('Endpoint 不能为空'));
      if (!isFs(o.type)) {
        if (!o.bucket) errs.push(t('S3 / OSS 必须填写桶'));
        if (!o.ak) errs.push(t('S3 / OSS 必须填写 AK'));
        if (!o.sk && (n || !s.sk_set)) errs.push(t('S3 / OSS 必须填写 SK'));
      }
      if (!Number.isInteger(o.retention_days) || o.retention_days < 0 || o.retention_days > (limits().store_retention_max || 36500)) errs.push(t('保留天数取 0–{max} 的整数（0 = 永久）', { max: limits().store_retention_max || 36500 }));
      if (errs.length) { modalMsg(errs[0]); return false; }
      try {
        if (n) await A.createStore(o);
        else await A.updateStore(s.id, o);
      } catch (e) {
        const is = issuesOf(e);
        modalMsg(is && is.length ? is.map((x) => tp(x.msg)).join(isZh ? '；' : '; ') : e.message);
        return false;
      }
      flash(n ? t('已新建备份存储') : t('已保存'), 'ok');
      await load();
    },
  });
}

async function testSaved(s) {
  flash(t('正在测试连接…'), 'info', true);
  try {
    const r = await A.testStore(s.id);
    flash('');
    const body = el('div');
    body.append(notice([bold(r.ok ? t('连接成功') : r.kind === 'timeout' ? t('连接超时') : t('连接失败')), el('br'), tp(r.msg) || ''], r.ok ? 'ok' : 'bad'));
    body.append(kvList([[t('存储'), s.name], [t('耗时'), r.ms ? r.ms + ' ms' : '—'], [t('可用空间'), r.free_bytes === null || r.free_bytes === undefined ? '—' : fmtBytes(r.free_bytes)]]));
    openModal({ title: t('测试连接 · {name}', { name: s.name }), body, ok: '', cancel: t('关闭') });
    await load();
  } catch (e) {
    flash(e.message, 'err');
  }
}

function toggle(s) {
  const act = async () => {
    await A.enableStore(s.id, !s.enabled);
    flash(s.enabled ? t('已停用') : t('已启用'), 'ok');
    await load();
  };
  if (s.enabled && s.used_by) {
    riskConfirm({
      title: t('停用备份存储'),
      body: notice(withNodes(t('「{name}」被 {n} 个任务引用，停用后这些任务将无法运行。', { n: s.used_by }), { name: bold(s.name) }), 'warn', '!'),
      ok: t('确认停用'), onOk: act,
    });
  } else act().catch((e) => flash(e.message, 'err'));
}

function del(s) {
  if (s.used_by) {
    flash(t('被 {n} 个任务引用，不能删除', { n: s.used_by }), 'warn');
    return;
  }
  riskConfirm({
    title: t('删除备份存储'), nameToType: s.name,
    body: notice(withNodes(t('删除「{name}」的配置（不会删除对象存储里已有的备份文件）。'), { name: bold(s.name) }), 'warn', '!'),
    ok: t('确认删除'),
    onOk: async () => { await A.deleteStore(s.id); flash(t('已删除'), 'ok'); await load(); },
  });
}

