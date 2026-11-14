/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: confirm_page.js
 * @time: 2026/10/28
 */
// 测试页入口：把组件挂到 window，供 run_confirm.mjs 通过 CDP 调用。
import * as CF from '/static/js/confirm.js';
window.CF = CF;
window.__ready = true;
