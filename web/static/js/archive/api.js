/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: api.js
 * @time: 2026/10/28
 */
// 数据归档：全部 JSON 接口的封装。
// 页面里不直接拼 URL，所有请求从这里走；错误统一抛出带 code / message / status / data 的 Error（common.js 的 api）。
import { api, csrfToken } from '../common.js';
import { t } from '../i18n.js';

// 所有 JSON 接口都挂在 /api/v1 下（会话 + CSRF + JWT 校验）
const B = '/api/v1/archive';
const qs = (o) => {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(o || {})) {
    if (v === undefined || v === null || v === '' || v === false) continue;
    p.set(k, v === true ? '1' : String(v));
  }
  const s = p.toString();
  return s ? '?' + s : '';
};

export const A = {
  meta: () => api('GET', `${B}/meta`),

  // 来源选择
  instances: () => api('GET', `${B}/sources/instances`),
  databases: (instanceId) => api('GET', `${B}/sources/databases${qs({ instance_id: instanceId })}`),
  tables: (instanceId, db) => api('GET', `${B}/sources/tables${qs({ instance_id: instanceId, db })}`),
  columns: (instanceId, db, table) => api('GET', `${B}/sources/columns${qs({ instance_id: instanceId, db, table })}`),
  // 整型列像不像 10 位秒 / 13 位毫秒时间戳（抽样 + 最早 / 最晚值），向导选了整型时间列后调用
  timeProbe: (instanceId, db, table, column) => api('GET', `${B}/sources/time-probe${qs({ instance_id: instanceId, db, table, column })}`),
  flow: (groupId) => api('GET', `${B}/flow${qs({ resource_group_id: groupId })}`),

  // 任务。列表接口响应带 total / kpi / instances，所以返回整个 payload（见 raw）。
  tasks: (params) => raw('GET', `${B}/tasks${qs(params)}`),
  task: (id) => api('GET', `${B}/tasks/${id}`),
  createTask: (body) => raw('POST', `${B}/tasks`, body),
  updateTask: (id, body) => raw('PUT', `${B}/tasks/${id}`, body),
  preview: (body) => api('POST', `${B}/preview`, body),
  check: (body) => api('POST', `${B}/check`, body),
  submitTask: (id, body) => api('POST', `${B}/tasks/${id}/submit`, body),
  approve: (id, remark) => api('POST', `${B}/tasks/${id}/approve`, { remark: remark || '' }),
  reject: (id, reason) => api('POST', `${B}/tasks/${id}/reject`, { reason }),
  pauseTask: (id, reason) => api('POST', `${B}/tasks/${id}/pause`, { reason: reason || '' }),
  resumeTask: (id) => api('POST', `${B}/tasks/${id}/resume`, {}),
  runTask: (id, ignoreWindow) => api('POST', `${B}/tasks/${id}/run`, { ignore_window: !!ignoreWindow, confirm: true }),
  disableTask: (id) => api('POST', `${B}/tasks/${id}/disable`, { confirm: true }),
  deleteTask: (id, name) => api('DELETE', `${B}/tasks/${id}${qs({ confirm_name: name })}`),

  // 运行
  runs: (params) => raw('GET', `${B}/runs${qs(params)}`),
  run: (id) => api('GET', `${B}/runs/${id}`),
  runEvents: (id, afterSeq) => raw('GET', `${B}/runs/${id}/events${qs({ after_seq: afterSeq || 0, limit: 200 })}`),
  manifestUrl: (id) => `${B}/runs/${id}/manifest`,
  progress: (taskId) => api('GET', `${B}/progress${qs({ task_id: taskId })}`),
  pauseRun: (id) => api('POST', `${B}/runs/${id}/pause`, {}),
  resumeRun: (id) => api('POST', `${B}/runs/${id}/resume`, {}),
  stopRun: (id) => api('POST', `${B}/runs/${id}/stop`, { confirm: true }),
  restoreRun: (id, name) => api('POST', `${B}/runs/${id}/restore`, { confirm_name: name }),

  // 备份存储
  stores: () => api('GET', `${B}/stores`),
  storePicker: () => api('GET', `${B}/stores${qs({ for: 'picker' })}`),
  createStore: (body) => api('POST', `${B}/stores`, body),
  updateStore: (id, body) => api('PUT', `${B}/stores/${id}`, body),
  testStore: (id) => api('POST', `${B}/stores/${id}/test`, {}),
  testStoreForm: (body) => api('POST', `${B}/stores/test`, body),
  enableStore: (id, enable) => api('POST', `${B}/stores/${id}/enable`, { enable: !!enable }),
  deleteStore: (id) => api('DELETE', `${B}/stores/${id}`),

  // 安全策略
  policy: () => api('GET', `${B}/policy`),
  policyDiff: (body) => api('POST', `${B}/policy/diff`, body),
  savePolicy: (body) => api('PUT', `${B}/policy`, { ...body, confirm: true }),
};

// 取整个响应体（含 total / kpi / outcome 等 data 之外的字段）。
async function raw(method, url, body) {
  const headers = { Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET') headers['X-CSRF-Token'] = csrfToken();
  let res;
  try {
    res = await fetch(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: 'same-origin' });
  } catch (_) {
    const err = new Error(t('无法连接服务器，请检查网络后重试'));
    err.code = 'network';
    throw err;
  }
  let payload = null;
  try { payload = await res.json(); } catch (_) { /* 非 JSON */ }
  if (!res.ok) {
    const err = new Error((payload && payload.message) || t('请求失败（HTTP {status}）', { status: res.status }));
    err.code = (payload && payload.code) || 'http_' + res.status;
    err.status = res.status;
    err.data = payload && payload.data;
    throw err;
  }
  return payload || {};
}


// 把校验失败的错误整理成 [{field, level, msg}]（非 validation 错误返回 null）。
export function issuesOf(err) {
  return err && err.code === 'validation' && err.data && Array.isArray(err.data.issues) ? err.data.issues : null;
}
