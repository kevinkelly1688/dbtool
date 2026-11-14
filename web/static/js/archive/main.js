/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: main.js
 * @time: 2026/10/28
 */
// 数据归档页面入口：初始化、页签 / hash 路由、详情抽屉（#task/ID、#run/ID）、向导（#wizard、#wizard/ID）。
// 所有来自服务端 / 数据库的字符串一律用 textContent 写入，不拼 innerHTML。
import { applyButtons } from '../btnperm.js';
import { t } from '../i18n.js';
import { A } from './api.js';
import { $ } from './util.js';
import { initUi, closeDrawer, drawerOpen, flash } from './ui.js';
import { S, initShared, can } from './consts.js';
import * as tasks from './tasks.js';
import * as runs from './runs.js';
import * as stores from './stores.js';
import * as policy from './policy.js';
import * as wizard from './wizard.js';
import { openTaskDrawer } from './task_drawer.js';
import { openRunDrawer } from './run_drawer.js';

const VIEWS = { tasks, runs, stores, policy, wizard };
let curView = '';
let suppressReplace = false;
let ready = false;

function parseHash() {
  const h = location.hash.replace(/^#/, '');
  const [path, qs] = h.split('?');
  const [name, arg] = path.split('/');
  const query = {};
  new URLSearchParams(qs || '').forEach((v, k) => { query[k] = v; });
  return { name: name || 'tasks', arg: arg || '', query };
}

function setTabsActive(name) {
  document.querySelectorAll('#ar-tabs .tab').forEach((a) => {
    const on = a.dataset.tab === name;
    a.classList.toggle('on', on);
    if (on) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
}

function switchView(name, query, arg) {
  if (curView && curView !== name) VIEWS[curView].hide();
  curView = name;
  setTabsActive(name === 'wizard' ? '' : name);
  if (name === 'wizard') return wizard.show(arg ? Number(arg) : 0);
  return VIEWS[name].show(query);
}

function closeDrawerQuietly() {
  if (!drawerOpen()) return;
  suppressReplace = true;
  closeDrawer();
  suppressReplace = false;
}

// 抽屉关闭后把地址栏从 #task/ID 改回当前页签（不触发路由）
function drawerClosed() {
  if (suppressReplace) return;
  const { name } = parseHash();
  if (name === 'task' || name === 'run') history.replaceState(null, '', '#' + (curView === 'wizard' || !curView ? 'tasks' : curView));
}

function route() {
  if (!ready) return;
  const { name, arg, query } = parseHash();
  if (name === 'task' || name === 'run') {
    if (!curView || curView === 'wizard') switchView(name === 'task' ? 'tasks' : 'runs', {});
    closeDrawerQuietly();
    const id = Number(arg);
    if (!id) return;
    if (name === 'task') openTaskDrawer(id, drawerClosed);
    else openRunDrawer(id, drawerClosed);
    return;
  }
  closeDrawerQuietly();
  let view = VIEWS[name] ? name : 'tasks';
  if ((view === 'stores' || view === 'policy') && !can('archive.cfg_view')) {
    view = 'tasks';
    history.replaceState(null, '', '#tasks');
  }
  switchView(view, query, arg);
}

async function init() {
  initUi();
  let meta;
  try {
    meta = await A.meta();
  } catch (e) {
    flash(e.message, 'err', true);
    return;
  }
  if (!meta.ready) {
    const box = $('ar-notready');
    box.hidden = false;
    box.textContent = t('数据归档尚未初始化，请先在元数据库执行 deploy/sql/180_archive.sql。');
    $('ar-tabs').hidden = true;
    return;
  }
  await initShared(meta);
  try {
    S.stores = can('archive.cfg_view') ? await A.stores() : await A.storePicker();
  } catch (_) {
    S.stores = [];
  }
  if (can('archive.cfg_view')) document.querySelectorAll('#ar-tabs .tab[data-tab="stores"], #ar-tabs .tab[data-tab="policy"]').forEach((a) => { a.hidden = false; });
  const me = meta.me || {};
  $('ar-tabnote').textContent = me.username ? t('当前用户：{name}', { name: me.display || me.username }) + (me.is_super ? t('（超级用户）') : '') : '';
  ready = true;
  applyButtons(document);
  $('ar-refresh').addEventListener('click', () => {
    const v = VIEWS[curView];
    if (v && v.refresh) v.refresh();
  });
  window.addEventListener('hashchange', route);
  route();
}

init();
