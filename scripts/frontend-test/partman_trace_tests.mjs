/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: partman_trace_tests.mjs
 * @time: 2026/10/28
 */
// 运行输出（trace）面板相关用例，由 run_partman_merge.mjs 调用。
export async function registerTraceTests(h) {
  const { t, eq, truthy, has, lacks, ev, q, qa, exists, visible, click, setVal, until, sleep, ctl, reqsOf, load, openPage, rowBtn, send, noHScroll, base } = h;
  const J = JSON.stringify;
  const rowsIn = (sel) => ev(`return [...document.querySelectorAll(${J(sel)})].map(r=>r.textContent)`);
  const nthToggle = (scope, n) => ev(`const b=document.querySelectorAll(${J(scope + ' .pm-out-toggle')})[${n}]; if(!b) throw new Error('没有第 ${n} 个运行输出按钮'); b.click();`);
  const lineCount = (scope) => ev(`return document.querySelectorAll(${J(scope + ' .pm-out-line')}).length`);
  const lineRow = (scope, i) => ev(`const l=document.querySelectorAll(${J(scope + ' .pm-out-line')})[${i}]; return l ? l.className : null;`);

  // ---------- 配置的日志弹窗 ----------
  await openPage();
  await t('日志弹窗：有运行输出的记录显示“运行输出（N 行）”，没有的说明原因', async () => {
    await rowBtn('order_event', '日志');
    await until(() => ev("return document.querySelectorAll('#pl-rows tr:not(.pm-out-row)').length===3"), '三条日志');
    const rows = await rowsIn('#pl-rows tr:not(.pm-out-row)');
    has(rows[0], '运行输出（5 行）');
    has(rows[1], '无运行输出');
    has(rows[2], '运行输出（121 行）');
    eq(await lineCount('#pl-rows'), 0, '默认不展开（有语句项）');
  });
  await t('运行输出：展开后按需取明细，按级别着色，文本不被当 HTML 解析', async () => {
    await nthToggle('#pl-rows', 0);
    await until(() => exists('#pl-rows .pm-out-line'), '输出行');
    eq((await reqsOf('/logs/11')).length, 1);
    eq(await lineCount('#pl-rows'), 5);
    eq(await ev("return document.querySelector('#pl-rows .pm-out-toggle').getAttribute('aria-expanded')"), 'true');
    has(await lineRow('#pl-rows', 2), 'pm-lv-warn');
    has(await lineRow('#pl-rows', 4), 'pm-lv-error');
    has(await q('#pl-rows .pm-out-body'), '<img src=x onerror="window.__xss=1">', '原样显示为文本');
    eq(await ev("return document.querySelectorAll('#pl-rows .pm-out img').length"), 0, '不应生成 img 元素');
    eq(await ev('return window.__xss === undefined'), true);
    eq(await ev("return getComputedStyle(document.querySelector('#pl-rows .pm-lv-error .pm-out-l')).color !== getComputedStyle(document.querySelector('#pl-rows .pm-lv-info .pm-out-l')).color"), true, '级别颜色不同');
    has(await ev("return getComputedStyle(document.querySelector('#pl-rows .pm-out-body')).fontFamily"), 'mono', '等宽字体');
  });
  await t('运行输出：级别筛选 / 搜索（命中高亮）/ 计数', async () => {
    await ev("const s=document.querySelector('#pl-rows .pm-out-level'); s.value='ERROR'; s.dispatchEvent(new Event('change',{bubbles:true}));");
    eq(await lineCount('#pl-rows'), 1);
    has(await q('#pl-rows .pm-out-count'), '1 / 5 行');
    await ev("const s=document.querySelector('#pl-rows .pm-out-level'); s.value='WARN'; s.dispatchEvent(new Event('change',{bubbles:true}));");
    eq(await lineCount('#pl-rows'), 2);
    await ev("const s=document.querySelector('#pl-rows .pm-out-level'); s.value='INFO'; s.dispatchEvent(new Event('change',{bubbles:true}));");
    await setVal('#pl-rows .pm-out-search', '夏令时');
    eq(await lineCount('#pl-rows'), 1);
    eq(await ev("return document.querySelector('#pl-rows .pm-out-m mark').textContent"), '夏令时');
    await setVal('#pl-rows .pm-out-search', '不存在的词');
    has(await q('#pl-rows .pm-out-body'), '没有符合筛选条件');
    await setVal('#pl-rows .pm-out-search', '');
    eq(await lineCount('#pl-rows'), 5);
  });
  await t('运行输出：复制得到“时间 [级别] 消息”纯文本；下载链接指向导出接口', async () => {
    await ev("window.__copied = null; Object.defineProperty(navigator, 'clipboard', { value: { writeText: (x) => { window.__copied = x; return Promise.resolve(); } }, configurable: true });");
    await click('#pl-rows .pm-out-copy');
    await until(() => ev('return window.__copied !== null'), '复制');
    const txt = await ev('return window.__copied');
    has(txt, '2026-10-10 12:00:00.012 [INFO ] 开始维护 order_db.order_event');
    has(txt, '[ERROR] 执行失败');
    eq(txt.trim().split('\n').length, 5);
    eq(await q('#pl-rows .pm-out-dl', "getAttribute('href')"), '/api/v1/partman/logs/11/output');
    eq(await ev("return document.querySelector('#pl-rows .pm-out-dl').hasAttribute('download')"), true);
    const body = await ev("const r = await fetch('/api/v1/partman/logs/11/output'); return [r.headers.get('content-disposition'), await r.text()];");
    has(body[0], 'attachment');
    has(body[1], '[ERROR] 执行失败');
  });
  await t('运行输出：截断标记醒目，长输出在面板内部滚动，筛选时标记仍保留', async () => {
    await nthToggle('#pl-rows', 1);
    await until(async () => (await qa('#pl-rows .pm-out-line')).length > 100, '长输出');
    const P = "document.querySelectorAll('#pl-rows tr.pm-out-row')[1]";
    eq(await ev(`return !${P}.hidden`), true, '整行已显示');
    eq(await ev(`return !${P}.querySelector('.pm-out-cut-note').hidden`), true, '截断提示');
    has(await ev(`return ${P}.querySelector('.pm-out-cut-note').textContent`), '已截断 37 行');
    const sc = await ev(`const b=${P}.querySelector('.pm-out-body'); return [b.scrollHeight, b.clientHeight, getComputedStyle(b).overflowY];`);
    truthy(sc[0] > sc[1] && sc[2] === 'auto', '面板内部滚动 ' + J(sc));
    await ev(`const s=${P}.querySelector('.pm-out-level'); s.value='ERROR'; s.dispatchEvent(new Event('change',{bubbles:true}));`);
    has(await ev(`return ${P}.querySelector('.pm-out-body').textContent`), '已截断 37 行');
  });
  await t('日志列表项已内联 output.lines 时不再请求明细', async () => {
    await click('#pl-close');
    await openPage({ inlineOutput: true });
    await rowBtn('order_event', '日志');
    await until(() => ev("return document.querySelectorAll('#pl-rows tr:not(.pm-out-row)').length===3"), '日志');
    await nthToggle('#pl-rows', 0);
    await until(() => exists('#pl-rows .pm-out-line'), '输出行');
    eq(await lineCount('#pl-rows'), 5);
    eq((await reqsOf('/logs/11')).length, 0, '不应请求明细');
    await click('#pl-close');
    await openPage();
    await rowBtn('order_event', '日志');
    await until(() => ev("return document.querySelectorAll('#pl-rows tr:not(.pm-out-row)').length===3"), '日志');
  });
  await t('日志弹窗：Esc 关闭后再打开仍可用', async () => {
    await click('#pl-close');
    await rowBtn('order_event', '日志');
    await until(() => ev("return document.querySelectorAll('#pl-rows tr:not(.pm-out-row)').length===3"), '再次打开');
  });

  // ---------- 全局维护日志页 ----------
  await load('/partman/logs');
  await until(() => ev("return document.querySelectorAll('#pg-rows tr:not(.pm-out-row)').length===5"), '五条日志');
  await t('全局日志：定时轮次汇总可读（计数、每个配置的结果与原因），状态不推定', async () => {
    const rows = await rowsIn('#pg-rows tr:not(.pm-out-row)');
    const sys = rows.find((r) => r.includes('系统记录：定时轮次'));
    truthy(sys, '有定时轮次系统记录行');
    has(sys, '定时维护轮次');
    has(sys, '检查 3 / 已执行 1 / 无需维护 0 / 跳过 1 / 失败 1');
    has(sys, '近期日志保护窗口 30 分钟');
    has(sys, '只续建、不自动删除');
    has(sys, '#1002 log.access_log');
    has(sys, '表未分区：定时维护不会初始化');
    has(sys, '另有 2 项未列出');
    lacks(sys, '未记录');
    truthy(rows.some((r) => r.includes('系统记录：推送测试')), '推送测试记录');
  });
  await t('全局日志：轮次的运行输出默认折叠，点开后 WARN / ERROR 着色；推送测试没有汇总，输出直接展开', async () => {
    const open = await ev("return [...document.querySelectorAll('#pg-rows tr.pm-out-row')].filter(r=>!r.hidden).map(r=>r.textContent)");
    eq(open.length, 1, '只有推送测试默认展开');
    has(open[0], '推送测试开始');
    has(open[0], 'access_token=****');
    lacks(open[0], '定时维护开始');
    await ev("const r=[...document.querySelectorAll('#pg-rows tr:not(.pm-out-row)')].find(x=>x.textContent.includes('运行输出（6 行）')); r.querySelector('.pm-out-toggle').click();");
    await until(() => ev("return document.querySelectorAll('#pg-rows .pm-lv-error').length===1"), '轮次输出展开');
    eq(await ev("return document.querySelectorAll('#pg-rows .pm-lv-warn').length"), 1);
    eq(await ev("const r=document.querySelectorAll('#pg-rows tr.pm-out-row:not([hidden])')[0]; const w=r.getBoundingClientRect().width, t=document.querySelector('#pg-rows').getBoundingClientRect().width; return w >= t - 2"), true, '输出占满整行');
  });
  await t('全局日志：筛选“系统记录”发 scope=system，其它来源发 source', async () => {
    await setVal('#pg-source', 'system');
    await until(async () => (await reqsOf('/logs')).some((r) => r.query.scope === 'system'), 'scope 请求');
    const r = (await reqsOf('/logs')).filter((x) => x.query.scope === 'system').pop();
    eq(r.query.source, undefined);
    await until(() => ev("return document.querySelectorAll('#pg-rows tr:not(.pm-out-row)').length===2"), '两条系统记录');
    await setVal('#pg-source', 'manual');
    await until(async () => (await reqsOf('/logs')).some((x) => x.query.source === 'manual'), 'source 请求');
  });
  await t('全局日志：普通记录点开运行输出', async () => {
    await setVal('#pg-source', '');
    await until(() => ev("return document.querySelectorAll('#pg-rows tr:not(.pm-out-row)').length===5"), '全部');
    await ev("const r=[...document.querySelectorAll('#pg-rows tr:not(.pm-out-row)')].find(x=>x.textContent.includes('运行输出（5 行）')); r.querySelector('.pm-out-toggle').click();");
    await until(() => ev("return [...document.querySelectorAll('#pg-rows tr.pm-out-row')].some(r=>!r.hidden && r.querySelectorAll('.pm-out-line').length===5)"), '展开');
  });

  // ---------- 预览 / 执行里的检查过程 ----------
  await openPage();
  await t('预览：响应带 trace 时显示“检查过程”，可展开、可复制，没有下载（未落库）', async () => {
    await rowBtn('order_event', '预览 / 执行');
    await until(() => exists('#pr-plan .pm-trace-box'), '检查过程');
    has(await q('#pr-plan .pm-trace-box .pm-out-toggle'), '检查过程（3 行）');
    eq(await lineCount('#pr-plan'), 0, '默认折叠');
    await click('#pr-plan .pm-out-toggle');
    await until(() => exists('#pr-plan .pm-out-line'), '展开');
    eq(await lineCount('#pr-plan'), 3);
    eq(await exists('#pr-plan .pm-out-dl'), false, '预览输出不提供下载');
    truthy(await visible('#pr-plan .pm-out-copy'), '复制按钮');
    await click('#pr-close');
  });
  await t('Doris 预览：上界未知的分区单独标出“不会删除”', async () => {
    await rowBtn('ods_event', '预览 / 执行');
    await until(() => exists('#pr-plan .pm-part-unknown'), '未知上界分区');
    has(await q('#pr-plan .pm-part-unknown'), 'pbad');
    has(await q('#pr-plan .pm-part-unknown'), '不会删除');
    await click('#pr-close');
  });
  await t('初始化预览：很长的语句缩略显示，可展开 / 复制完整语句；确认框里也是缩略', async () => {
    await ctl({ longSql: true });
    await rowBtn('access_log', '预览 / 执行');
    await until(() => exists('#pr-plan .pm-stmt-text'), '缩略语句');
    const short = await q('#pr-plan .pm-stmt-text');
    truthy(short.length < 800 && short.includes('省略'), '缩略 ' + short.length);
    await ev("window.__copied=null; Object.defineProperty(navigator,'clipboard',{value:{writeText:(x)=>{window.__copied=x;return Promise.resolve();}},configurable:true});");
    await ev("[...document.querySelectorAll('#pr-plan .pm-stmt-ops button')].find(b=>b.textContent.includes('复制')).click()");
    await until(() => ev('return window.__copied !== null'), '复制');
    truthy((await ev('return window.__copied.length')) > 10000, '复制的是完整语句');
    await ev("[...document.querySelectorAll('#pr-plan .pm-stmt-ops button')].find(b=>b.textContent.includes('展开')).click()");
    truthy((await q('#pr-plan .pm-stmt-text')).length > 10000, '展开后是完整语句');
    await click('#pr-go');
    await until(() => exists('.cf-dialog'), '确认框');
    const shown = await q('.cf-dialog');
    has(shown, '语句较长');
    truthy(shown.length < 6000, '确认框里的语句已缩略 ' + shown.length);
    await click('.cf-foot .cf-btn:first-child');
    await click('#pr-close');
    await ctl({ longSql: false });
  });
  await t('配置未启用时：预览可看，执行按钮禁用并说明', async () => {
    await openPage({ disable1: true });
    await rowBtn('order_event', '预览 / 执行');
    await until(() => exists('#pr-plan .pm-stmt'), '预览');
    has(await q('#pr-msg'), '该配置已停用');
    has(await q('#pr-msg'), '只能预览');
    eq(await ev("return document.getElementById('pr-go').disabled"), true);
    await click('#pr-close');
  });
  await t('预览：响应没有 trace 时不显示检查过程', async () => {
    await ctl({ noTrace: true });
    await rowBtn('order_event', '预览 / 执行');
    await until(() => exists('#pr-plan .pm-stmt'), '预览');
    eq(await exists('#pr-plan .pm-trace-box'), false);
    await click('#pr-close');
    await ctl({ noTrace: false });
  });
  await t('编辑弹窗的预览同样显示检查过程', async () => {
    await click('#pm-new');
    await setVal('#pe-inst', '1');
    await setVal('#pe-db', 'order_db');
    await setVal('#pe-tb', 'order_event');
    await setVal('#pe-col', 'created_at');
    await click('#pe-preview');
    await until(() => exists('#pe-plan .pm-trace-box'), '检查过程');
    await click('#pe-cancel');
  });
  await t('初始化执行后：结果下方直接展示本次运行输出，可下载', async () => {
    await openPage();
    await rowBtn('access_log', '预览 / 执行');
    await until(() => visible('#pr-go'), '按钮');
    await click('#pr-go');
    await until(() => exists('.cf-dialog'), '确认框');
    await ev(`const i=document.querySelector('.cf-dialog input'); i.value='log.access_log'; i.dispatchEvent(new Event('input',{bubbles:true}));`);
    await click('.cf-foot .cf-btn:last-child');
    await until(() => exists('#pr-plan .pm-out-line'), '运行输出');
    eq(await lineCount('#pr-plan'), 2);
    eq(await q('#pr-plan .pm-out-dl', "getAttribute('href')"), '/api/v1/partman/logs/31/output');
    await click('#pr-close');
  });

  // ---------- 手机 / 平板 ----------
  for (const [label, w, hgt] of [['手机 375', 375, 812], ['平板 768', 768, 1024]]) {
    await send('Emulation.setDeviceMetricsOverride', { width: w, height: hgt, deviceScaleFactor: 1, mobile: w < 700 });
    await openPage();
    await t(`${label}：日志弹窗展开长输出，页面无横向滚动，面板内部滚动`, async () => {
      await rowBtn('order_event', '日志');
      await until(() => ev("return document.querySelectorAll('#pl-rows tr:not(.pm-out-row)').length===3"), '日志');
      await nthToggle('#pl-rows', 1);
      await until(async () => (await qa('#pl-rows .pm-out-line')).length > 100, '长输出');
      await noHScroll('日志弹窗');
      const box = await ev("const b=document.querySelectorAll('#pl-rows tr.pm-out-row')[1].querySelector('.pm-out'); const r=b.getBoundingClientRect(); const bd=b.querySelector('.pm-out-body'); return [r.left, r.right, innerWidth, bd.scrollHeight, bd.clientHeight, b.querySelector('.pm-out-dl').getBoundingClientRect().right];");
      truthy(box[0] >= 0 && box[1] <= box[2] + 1, '面板在视口内 ' + J(box));
      truthy(box[3] > box[4], '面板内部滚动 ' + J(box));
      truthy(box[5] <= box[2] + 1, '下载按钮在视口内');
      await click('#pl-close');
    });
    await load('/partman/logs');
    await until(() => ev("return document.querySelectorAll('#pg-rows tr:not(.pm-out-row)').length===5"), '日志');
    await t(`${label}：全局日志页无页面级横向滚动`, async () => {
      await noHScroll('全局日志');
      await ev("const r=[...document.querySelectorAll('#pg-rows tr:not(.pm-out-row)')].find(x=>x.textContent.includes('运行输出（121 行）')); r.querySelector('.pm-out-toggle').click();");
      await until(async () => (await qa('#pg-rows .pm-out-line')).length > 100, '长输出');
      await noHScroll('全局日志展开后');
    });
  }
  await send('Emulation.clearDeviceMetricsOverride');
}
