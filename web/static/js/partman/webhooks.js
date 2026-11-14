/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: webhooks.js
 * @time: 2026/10/28
 */
// 分区维护：推送设置（dbt_partman_webhook）——通道列表、新增 / 编辑、启停、删除、测试发送。
// 所有来自服务端的字符串一律用 textContent 写入。webhook 地址只显示服务端返回的脱敏形式；
// 加签密钥只写不读（只有“已设置”标志），保存后输入框清空。
import { api, confirmRun, toast } from '../common.js';
import { t, tp } from '../i18n.js';
import { el, setMsg, colon, listSep } from './util.js';

const $ = (id) => document.getElementById(id);

const TYPE_LABEL = { dingtalk: t('钉钉'), feishu: t('飞书'), wecom: t('企业微信'), slack: 'Slack' };
const MENTION_ON_LABEL = { always: t('每次推送都 @'), on_failure: t('仅失败时 @'), never: t('从不 @') };
const NOTIFY_ON_LABEL = {
  always: t('执行了语句或失败后推送'),
  on_failure: t('仅失败时推送'),
  never: t('不推送'),
};

let ov = null;
let state = { items: [], notify_on: 'always' };
let editing = null; // 正在编辑的通道；null 表示新增
let busy = false;

function show(id) { $(id).hidden = false; }
function hide(id) { $(id).hidden = true; }

function button(text, cls, fn) {
  const b = el('button', 'btn ' + cls, text);
  b.type = 'button';
  b.addEventListener('click', fn);
  return b;
}

function parseMention(text) {
  const out = [];
  for (const t of String(text).split(/[\s,，;；]+/)) if (t && !out.includes(t)) out.push(t);
  return out;
}

// ---- 列表 ----

function renderList() {
  const box = $('ph-list');
  box.replaceChildren();
  const rule = $('ph-rule');
  rule.textContent = t('全局推送时机：{when}', { when: NOTIFY_ON_LABEL[state.notify_on] || state.notify_on });
  if (!state.items.length) {
    box.append(el('p', 'empty muted', t('还没有推送通道。')));
    return;
  }
  for (const h of state.items) {
    const card = el('div', 'pm-hook' + (h.isenabled ? '' : ' off'));
    card.dataset.id = String(h.id);
    const head = el('div', 'pm-hook-head');
    head.append(el('b', '', TYPE_LABEL[h.type] || h.type), el('span', 'st ' + (h.isenabled ? 'passed' : 'aborted'), h.isenabled ? t('启用|state') : t('停用|state')),
      el('span', 'pm-hook-url mono', h.webhook_url));
    const kv = el('dl', 'pm-kv');
    const add = (k, v) => kv.append(el('dt', '', k), el('dd', '', v));
    add(t('@ 时机'), MENTION_ON_LABEL[h.mention_on] || h.mention_on);
    add(t('@ 对象'), (h.mention || []).length ? h.mention.join(listSep) : t('（无）'));
    add(t('关键词'), h.keyword || t('（无）'));
    add(t('加签密钥'), h.secret_set ? t('已设置') : t('未设置'));
    const ops = el('div', 'pm-hook-ops');
    ops.append(
      button(t('编辑'), 'ghost', () => openEdit(h)),
      button(h.isenabled ? t('停用') : t('启用'), 'ghost', () => toggle(h)),
      button(t('测试发送'), 'ghost', () => test(h.id)),
      button(t('删除'), 'ghost danger', () => remove(h)),
    );
    card.append(head, kv, ops);
    box.append(card);
  }
}

async function loadList() {
  try {
    const res = await api('GET', '/api/v1/partman/webhooks');
    state = { items: res.items || [], notify_on: res.notify_on || 'always' };
    renderList();
  } catch (e) {
    $('ph-list').replaceChildren();
    setMsg($('ph-msg'), e.message);
  }
}

function renderResults(res) {
  const box = $('ph-results');
  box.replaceChildren();
  const list = res.results || [];
  if (!list.length) return;
  box.append(el('div', 'muted small', t('测试结果（{n} 个通道）：', { n: list.length })));
  if (res.log_id) {
    const a = el('a', 'small', t('已记为系统记录 #{id}，到维护日志查看', { id: res.log_id }));
    a.href = '/partman/logs?source=system';
    box.append(a);
  }
  const ul = el('ul', 'pm-test-results');
  for (const r of list) {
    const li = el('li');
    li.append(el('span', 'st ' + (r.ok ? 'passed' : 'rejected'), r.ok ? t('成功') : t('失败')),
      el('span', '', ` ${TYPE_LABEL[r.type] || r.type} `), el('span', 'mono small', r.webhook_url || ''),
      el('div', 'small' + (r.ok ? ' muted' : ' pm-log-err'), (r.status ? `HTTP ${r.status}${colon}` : '') + tp(r.message || '')));
    ul.append(li);
  }
  box.append(ul);
}

async function test(id) {
  if (busy) return;
  busy = true;
  setMsg($('ph-msg'), '');
  $('ph-results').replaceChildren(el('p', 'muted', t('正在发送测试消息…')));
  try {
    const res = await api('POST', '/api/v1/partman/webhooks/test', { id: id || 0 });
    renderResults(res);
    const failed = (res.results || []).filter((r) => !r.ok).length;
    toast(failed ? t('测试发送完成，{n} 个通道失败', { n: failed }) : t('测试消息已发送'), failed ? 'warn' : 'ok');
  } catch (e) {
    $('ph-results').replaceChildren();
    setMsg($('ph-msg'), e.message);
  } finally {
    busy = false;
  }
}

async function toggle(h) {
  try {
    await api('POST', `/api/v1/partman/webhooks/${h.id}/enable`, { enable: !h.isenabled });
    setMsg($('ph-msg'), h.isenabled ? t('已停用该通道') : t('已启用该通道'), 'info');
    await loadList();
  } catch (e) {
    setMsg($('ph-msg'), e.message);
  }
}

async function remove(h) {
  const cr = await confirmRun({
    title: t('删除推送通道'), level: 'high', code: 'partman.edit', codeName: t('编辑配置'),
    lines: [t('删除后，分区维护的结果不再推送到这个通道。')],
    details: [{ k: t('类型'), v: TYPE_LABEL[h.type] || h.type }, { k: t('地址'), v: h.webhook_url }, { k: t('状态'), v: h.isenabled ? t('启用|state') : t('停用|state') }],
    okText: t('确认删除'),
  }, () => api('DELETE', `/api/v1/partman/webhooks/${h.id}`));
  if (!cr.ok) return;
  setMsg($('ph-msg'), t('已删除'), 'info');
  await loadList();
}

// ---- 新增 / 编辑 ----

function openEdit(h) {
  editing = h || null;
  $('phe-title').textContent = h ? t('编辑推送通道：{type}', { type: TYPE_LABEL[h.type] || h.type }) : t('新增推送通道');
  setMsg($('phe-msg'), '');
  $('phe-type').value = h ? h.type : 'dingtalk';
  $('phe-mention-on').value = h ? h.mention_on : 'on_failure';
  const url = $('phe-url');
  url.value = '';
  url.placeholder = h ? t('留空表示不修改（当前：{url}）', { url: h.webhook_url }) : 'https://…';
  $('phe-keyword').value = h ? h.keyword || '' : '';
  const sec = $('phe-secret');
  sec.value = '';
  sec.placeholder = h && h.secret_set ? t('已设置，留空表示不修改') : t('安全设置里的加签密钥');
  $('phe-clear-secret').checked = false;
  $('phe-clear-box').hidden = !(h && h.secret_set);
  $('phe-mention').value = h ? (h.mention || []).join(', ') : '';
  $('phe-enable').checked = h ? h.isenabled : true;
  $('phe-test').hidden = !h;
  show('pm-hook-edit');
}

function closeEdit() {
  $('phe-secret').value = '';
  $('phe-url').value = '';
  hide('pm-hook-edit');
}

async function save() {
  setMsg($('phe-msg'), '');
  const url = $('phe-url').value.trim();
  if (!editing && !url) { setMsg($('phe-msg'), t('请填写 Webhook 地址')); return; }
  const body = {
    type: $('phe-type').value,
    webhook_url: url,
    isenabled: $('phe-enable').checked,
    mention_on: $('phe-mention-on').value,
    mention: parseMention($('phe-mention').value),
    keyword: $('phe-keyword').value.trim(),
    secret: $('phe-secret').value,
  };
  if (editing && $('phe-clear-secret').checked) {
    if (body.secret) { setMsg($('phe-msg'), t('不能同时填写新密钥和勾选“清空已保存的加签密钥”')); return; }
    body.clear_secret = true;
  }
  const btn = $('phe-save');
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  try {
    if (editing) await api('PUT', `/api/v1/partman/webhooks/${editing.id}`, body);
    else await api('POST', '/api/v1/partman/webhooks', body);
    closeEdit();
    setMsg($('ph-msg'), t('已保存'), 'info');
    await loadList();
  } catch (e) {
    setMsg($('phe-msg'), e.message);
  } finally {
    btn.disabled = false;
    btn.removeAttribute('aria-busy');
  }
}

async function testSaved() {
  if (!editing) return;
  setMsg($('phe-msg'), '');
  try {
    const res = await api('POST', '/api/v1/partman/webhooks/test', { id: editing.id });
    const r = (res.results || [])[0];
    if (!r) setMsg($('phe-msg'), t('没有返回测试结果'));
    else setMsg($('phe-msg'), (r.ok ? t('测试发送成功：{msg}', { msg: tp(r.message || '') }) : t('测试发送失败：{msg}', { msg: tp(r.message || '') })), r.ok ? 'info' : 'err');
  } catch (e) {
    setMsg($('phe-msg'), e.message);
  }
}

// ---- 入口 ----

export function initWebhooks(overview) {
  ov = overview;
  $('pm-hooks').hidden = !ov.can_webhook;
  if (!ov.can_webhook) return;
  $('pm-hooks').addEventListener('click', () => {
    setMsg($('ph-msg'), '');
    $('ph-results').replaceChildren();
    show('pm-hook');
    loadList();
  });
  $('ph-close').addEventListener('click', () => hide('pm-hook'));
  $('ph-new').addEventListener('click', () => openEdit(null));
  $('ph-test-all').addEventListener('click', () => test(0));
  $('phe-cancel').addEventListener('click', closeEdit);
  $('phe-save').addEventListener('click', save);
  $('phe-test').addEventListener('click', testSaved);
}
