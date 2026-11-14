/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: flowprev.js
 * @time: 2026/10/28
 */
// 提交页（SQL 上线、数据导出共用）的审批流预览：按所选资源组读出审批流，逐级只显示审批组，鼠标悬停时显示可审批的人员；某级无人可审时标红。
// 传了 note（提示区）时，芯片只放在 box 里，“没配置 / 流程为空 / 无人可审 / 读取失败”这类提示条放到 note 里
// （提交 SQL 工单页、数据导出提交页：芯片在页头，提示条在页头下面整行）；不传 note 时提示条还是和芯片放在一起。
import { api } from '../common.js';
import { t, tp } from '../i18n.js';

let seq = 0; // 只认最后一次请求的结果（连续切换资源组时旧响应可能后到）

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function render(box, f, note) {
  const out = note || box;
  box.replaceChildren();
  if (note) note.replaceChildren();
  if (!f.configured) {
    const first = f.warnings && f.warnings[0];
    out.append(el('div', 'alert err', first ? tp(first) : t('该资源组还没有配置 SQL 上线审批流，提交会被拒绝。')));
    return;
  }
  if (f.auto) {
    out.append(el('div', 'alert info', t('资源组「{name}」的审批流为空：提交后系统直接审核通过，不需要人工审批。', { name: f.group_name })));
    return;
  }
  const line = el('div', 'sf-flowprev');
  line.append(el('span', 'lv ok start', t('提交')));
  f.levels.forEach((lv) => {
    line.append(el('span', 'arrow', '→'));
    // 只显示审批组；可审批人员放在鼠标悬停提示里；某一级无人可审时标红
    const node = el('span', 'lv ' + (lv.warning ? 'bad' : 'ok'));
    node.append(el('b', '', t('第 {level} 级 · {name}', { level: lv.level, name: lv.group_name })));
    if (lv.warning) node.append(el('span', 'who', t('：无人可审')));
    const names = (lv.approvers || []).map((a) => a.display + (a.super ? t('（超级用户）') : ''));
    const tip = lv.warning ? tp(lv.warning) : names.length ? t('可审批人员（{n}）：{names}', { n: names.length, names: names.join(t('、')) }) : '';
    if (tip) {
      node.title = tip;
      node.tabIndex = 0; // 键盘聚焦时同样能看到提示
      node.setAttribute('aria-label', t('第 {level} 级 {name}。{tip}', { level: lv.level, name: lv.group_name, tip }));
    }
    line.append(node);
  });
  line.append(el('span', 'arrow', '→'));
  line.append(el('span', 'lv ok end', t('审核通过，可执行')));
  box.append(line);
  (f.warnings || []).forEach((w) => out.append(el('div', 'alert warn', tp(w))));
}

export async function showFlow(box, groupId, note) {
  const mine = ++seq;
  box.replaceChildren();
  if (note) note.replaceChildren();
  if (!groupId) return;
  box.append(el('span', 'muted', t('读取审批流…')));
  try {
    const f = await api('GET', `/api/v1/sqlworkflow/flow-preview?group_id=${encodeURIComponent(groupId)}`);
    if (mine === seq) render(box, f, note);
  } catch (e) {
    if (mine === seq) {
      const a = el('div', 'alert warn', t('审批流读取失败：{msg}', { msg: e.message }));
      if (note) { box.replaceChildren(); note.replaceChildren(a); } else box.replaceChildren(a);
    }
  }
}

// 复制为新工单：取 ?copy=<工单号> 指向的工单内容；没有该参数返回 null，失败时抛出错误。
export async function loadCopySource() {
  const id = new URLSearchParams(location.search).get('copy');
  if (!id || !/^\d+$/.test(id)) return null;
  const c = await api('GET', `/api/v1/sqlworkflow/${id}/copy`);
  c.source_id = id;
  return c;
}
