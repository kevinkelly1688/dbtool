/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: detail.js
 * @time: 2026/10/28
 */
// 权限申请详情：审批（通过/驳回）与撤回。
import { api, confirmRun, setButtonMeta } from '../common.js';
import { t } from '../i18n.js';

const id = document.getElementById('detail').dataset.applyId;
const msg = document.getElementById('detail-msg');
const remark = document.getElementById('remark');

function setMsg(text) {
  msg.replaceChildren();
  if (!text) return;
  const d = document.createElement('div');
  d.className = 'alert err';
  d.setAttribute('role', 'alert');
  d.textContent = text;
  msg.append(d);
}

async function act(path, body, button) {
  setMsg('');
  document.querySelectorAll('.review-box button').forEach((b) => (b.disabled = true));
  button.setAttribute('aria-busy', 'true');
  try {
    await api('POST', `/api/v1/query/privilege-applies/${id}/${path}`, body);
    location.reload();
  } catch (e) {
    setMsg(e.message);
    document.querySelectorAll('.review-box button').forEach((b) => (b.disabled = false));
    button.removeAttribute('aria-busy');
  }
}

const value = () => (remark ? remark.value.trim() : '');

const pass = document.getElementById('act-pass');
const reject = document.getElementById('act-reject');
const abort = document.getElementById('act-abort');
setButtonMeta({ 'qpriv.review': t('审批查询权限申请'), 'qpriv.apply': t('申请 / 撤回查询权限') });
// 按左侧标签文字取 dd 的内容；标签文字是服务端按界面语言渲染的，这里用同一个 t() 取同一份译文来对应
const kv = (label) => {
  for (const dt of document.querySelectorAll('dl.kv dt')) if (dt.textContent.trim() === label) return dt.nextElementSibling ? dt.nextElementSibling.textContent.trim() : '';
  return '';
};
const applyDetails = () => [
  { k: t('申请号'), v: '#' + id }, { k: t('申请人'), v: kv(t('申请人')) }, { k: t('实例'), v: kv(t('目标实例')) }, { k: t('权限类型'), v: kv(t('权限类型')) },
  { k: t('数据库'), v: kv(t('数据库')) }, { k: t('表'), v: kv(t('表')) || t('（整个库）') }, { k: t('行数上限'), v: kv(t('返回行数上限')) }, { k: t('有效期至'), v: kv(t('有效期至')) },
];
const stepTitle = (li) => ((li.querySelector('.tl-head b') || li).textContent || '').trim();
// 通过前说明：当前是第几级、是否末级、下一级是哪个审批组（时间线上只有审批组，没有具体的人）
function passLines() {
  const lines = [];
  const items = [...document.querySelectorAll('ol.tl > li')];
  const cur = items.findIndex((li) => li.classList.contains('current'));
  if (cur >= 0) {
    const next = items.slice(cur + 1).find((li) => li.classList.contains('pending'));
    if (next) {
      lines.push(t('当前是「{cur}」，不是末级。通过后进入下一级「{next}」继续审批，权限还不会授予。', { cur: stepTitle(items[cur]), next: stepTitle(next) }));
    } else {
      lines.push(t('当前是「{cur}」，已是末级审批。通过后会立即授予申请人上述库表的查询权限。', { cur: stepTitle(items[cur]) }));
    }
  }
  return lines;
}
if (pass) {
  pass.addEventListener('click', async () => {
    setMsg('');
    const r = await confirmRun({
      title: t('通过权限申请 #{id}', { id }), code: 'qpriv.review', okText: t('确认通过'), lines: passLines(),
      details: [...applyDetails(), { k: t('备注'), v: value() }],
    }, () => api('POST', `/api/v1/query/privilege-applies/${id}/audit`, { action: 'pass', remark: value() }));
    if (r.ok) { document.querySelectorAll('.review-box button').forEach((b) => (b.disabled = true)); location.reload(); }
  });
}
if (reject) {
  reject.addEventListener('click', () => {
    if (!value()) return setMsg(t('驳回时请填写驳回原因'));
    act('audit', { action: 'reject', remark: value() }, reject);
  });
}
if (abort) {
  abort.addEventListener('click', async () => {
    setMsg('');
    const r = await confirmRun({
      title: t('撤回权限申请 #{id}', { id }), code: 'qpriv.apply', okText: t('确认撤回'),
      lines: [t('撤回后流程终止，不能恢复；需要重新提交申请。')], details: applyDetails(),
    }, () => api('POST', `/api/v1/query/privilege-applies/${id}/abort`, { remark: value() || '申请人主动撤回' })); // i18n-ignore 撤回备注存库（审批日志）
    if (r.ok) { document.querySelectorAll('.review-box button').forEach((b) => (b.disabled = true)); location.reload(); }
  });
}
