/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: state.js
 * @time: 2026/10/28
 */
// 页面共享状态：服务端返回的概览。各模块监听 gi:overview 事件重新渲染。
import { api } from '../common.js';
import { t } from '../i18n.js';

export const BASE = '/api/v1/settings/goinception';
export const state = { ov: null };

// 重启方式只有 Docker（docker restart）：页面上统一显示成 Docker（容器名）；容器名没设置时写明。
export function restartLabel(d) {
  return d && d.container ? t('Docker（{name}）', { name: d.container }) : t('Docker（未设置容器名）');
}

// keepEditor 为真时，配置文件编辑框里尚未保存的修改不会被覆盖（例如只是保存了连接信息）。
export async function reload(keepEditor = false) {
  state.ov = await api('GET', BASE);
  document.dispatchEvent(new CustomEvent('gi:overview', { detail: { ov: state.ov, keepEditor } }));
  return state.ov;
}
