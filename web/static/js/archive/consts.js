/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: consts.js
 * @time: 2026/10/28
 */
// 数据归档：枚举文案、公共状态（meta、按钮权限）、标签构造。
import { t } from '../i18n.js';
import { loadButtons, can as canBtn } from '../btnperm.js';
import { tag } from './ui.js';

export const MODES = {
  move: [t('归档并删除'), 'ok', t('备份并校验后删除源数据')],
  copy: [t('仅备份'), 'info', t('只备份，不删除源数据')],
  purge: [t('仅清理'), 'warn', t('不备份，直接删除')],
};

export const TASK_STATUS = {
  draft: [t('草稿'), ''],
  pending: [t('待审批'), 'warn'],
  rejected: [t('已驳回'), 'bad'],
  enabled: [t('已启用'), 'ok'],
  paused: [t('已暂停'), 'warn'],
  disabled: [t('已停用'), ''],
};

export const RUN_RESULT = {
  running: [t('运行中'), 'brand'],
  paused: [t('已暂停'), 'warn'],
  success: [t('成功'), 'ok'],
  window_end: [t('窗口结束中断'), 'info'],
  load_abort: [t('负载中断'), 'warn'],
  limit_reached: [t('已达上限'), 'info'],
  failed: [t('失败'), 'bad'],
  verify_failed: [t('校验失败'), 'bad'],
  stopped: [t('已终止'), ''],
  structure_changed: [t('结构变更暂停'), 'warn'],
  interrupted: [t('中断'), 'warn'],
  skipped: [t('已跳过'), ''],
};

export const TRIGGER = { schedule: t('定时'), manual: t('手动'), restore: t('恢复|restore'), dryrun: t('试运行') };
export const STORE_STATUS = { ok: [t('正常'), 'ok'], err: [t('异常'), 'bad'], off: [t('已停用'), ''], unknown: [t('未测试'), 'warn'] };
export const PAUSE_KIND = { manual: t('人工暂停'), auto: t('自动暂停'), structure: t('结构变更暂停') };
export const PROGRESS_STATE = { running: [t('运行中'), 'brand'], paused: [t('已暂停'), 'warn'], waiting_load: [t('等待负载'), 'bad'], finished: [t('即将结束'), ''] };
export const THROTTLE_KIND = { threads: 'Threads_running', lag: t('从库复制延迟'), disk: t('磁盘剩余') };

// 页面共享状态：meta（/meta）、按钮权限、备份存储名称缓存。
export const S = {
  meta: null,
  buttons: null,
  stores: [], // 备份存储（精简或完整）
};

export async function initShared(meta) {
  S.meta = meta;
  S.buttons = await loadButtons();
}

// 没拿到权限信息时按“有”处理（服务端会兜底拒绝）。
export const can = (code) => !S.buttons || canBtn(S.buttons, code);
export const isSuper = () => !!(S.meta && S.meta.me && S.meta.me.is_super);
export const limits = () => (S.meta && S.meta.limits) || {};
export const policy = () => (S.meta && S.meta.policy) || {};

export function statusTag(status) {
  const s = TASK_STATUS[status] || [status, ''];
  return tag(s[0], s[1]);
}
export function resultTag(result) {
  const s = RUN_RESULT[result] || [result, ''];
  return tag(s[0], s[1]);
}
export function modeTag(mode) {
  const m = MODES[mode] || [mode, ''];
  return tag(m[0], m[1], m[2]);
}

export const storeName = (id) => {
  const s = S.stores.find((x) => x.id === id);
  return s ? s.name : t('存储#{id}', { id });
};

// 备份去向标签（任务列表 / 详情共用）。参数 task 是任务视图；不叫 t，免得遮住翻译函数 t()。
export function backupTags(task) {
  const out = [];
  if (task.mode === 'purge') return [tag(t('不备份'), 'warn', t('不备份，删除后无法恢复'))];
  if (task.dest_table) out.push(tag(t('库表 {name}', { name: `${task.dest_table.db}.${task.dest_table.template}` }), 'info', t('实例 {id}', { id: task.dest_table.instance_id })));
  if (task.dest_file) {
    const st = S.stores.find((x) => x.id === task.dest_file.store_id);
    const bad = st && (st.enabled === false || st.status === 'err' || st.status === 'off');
    const name = st ? st.name : t('存储#{id}', { id: task.dest_file.store_id });
    out.push(tag(t('文件 {name}', { name }) + (task.dest_file.gzip ? ' · gzip' : '') + (task.dest_file.encrypt ? ' · ' + t('加密') : ''), bad ? 'bad' : 'brand', st ? (bad ? t('备份存储当前不可用') : '') : t('存储已删除或无权查看')));
  }
  return out;
}

// 按钮显示规则：状态机允许（stateOk）时才考虑；服务端 allow 为真则显示；allow 为假但调用者本来就没有该按钮权限时，
// 交给 btnperm 按“置灰 / 隐藏”设置处理（显示出来，data-btn 生效）；有权限却 allow 为假（不是审批人等）则不显示。
export const showIf = (stateOk, allowed, code) => !!stateOk && (!!allowed || !can(code));
