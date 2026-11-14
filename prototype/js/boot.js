/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: boot.js
 * @time: 2026/10/28
 */
/* 启动：主题、顶栏事件、路由监听 */
(function () {
  try { const t = localStorage.getItem('dbt_theme'); if (t) document.documentElement.dataset.theme = t; } catch (e) {}
  $('#helpBtn').onclick = () => go('help', (NAV[ROUTE.page] || {}).help || 'quickstart');
  $('#userSel').onchange = e => {   // 原型演示：不退出直接切换身份
    ME = USERS.find(u => u.id == e.target.value);
    try { sessionStorage.setItem('dbt_uid', ME.id); } catch (err) {}
    buildNav(); buildTop(); toast(`已切换为 ${ME.login}（${roleText(ME)}）`); render();
  };
  document.addEventListener('keydown', e => { if (e.key === 'Escape') { closeModal(); closeDrawer(); $('#umenu').classList.remove('open'); } });
  window.addEventListener('hashchange', () => { if ($('#login').hidden) { render(); buildNav(); } });
  loginInit();
})();
