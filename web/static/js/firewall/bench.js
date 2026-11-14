/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: bench.js
 * @time: 2026/10/28
 */
// 语句测试台：把一条语句按当前已保存的全部规则走一遍，给出最终决定与每条规则的命中解释。
// 不写命中记录、不通知任何人；库表来自词法解析（线上有 goInception 时更准）。
import { api, toast } from '../common.js';
import { t, tp, isZh } from '../i18n.js';
import { h, clear, $, SCENES, ACTIONS, actionBadge, badge, highlightNodes, B, C, M, rich, envName } from './vocab.js';

const BASE = '/api/v1/settings/firewall';

// 示例语句：[名称, SQL, 场景, 环境, 时间, 扫描行数, 影响行数]
const PRESETS = [
  [t('整表删除（工单）'), 'DELETE FROM orders', 'workflow', 'prod', 'workday_day', '', '120000'],
  [t('带条件的更新（工单）'), 'UPDATE orders SET status = 3 WHERE id = 99', 'workflow', 'prod', 'workday_day', '', '1'],
  [t('查询敏感表的密码列'), 'SELECT UserId, UserName, Password FROM sys_users WHERE UserId = 20318', 'query', 'prod', 'workday_day', '', ''],
  [t('危险函数'), 'SELECT id, SLEEP(30) FROM orders LIMIT 1', 'query', 'prod', 'workday_day', '', ''],
  [t('笛卡尔积'), 'SELECT o.id, u.name FROM orders o, users u', 'query', 'prod', 'workday_day', '800000', ''],
  [t('夜间加索引（工单）'), 'ALTER TABLE orders ADD INDEX idx_user_created (user_id, created_at)', 'workflow', 'prod', 'workday_night', '', ''],
  [t('无条件全表读取'), 'SELECT * FROM order_item', 'query', 'prod', 'workday_day', '8200000', ''],
  [t('看别人在跑什么'), 'SELECT * FROM information_schema.processlist', 'query', 'prod', 'workday_day', '', ''],
];

const VERDICT = {
  pass: { t: t('放行'), cls: 'pass' }, deny: { t: t('拒绝'), cls: 'deny' }, reason: { t: t('要求填写原因'), cls: 'mid' },
  limit: { t: t('限制返回行数'), cls: 'mid' }, mask: { t: t('强制脱敏'), cls: 'mid' }, warn: { t: t('放行并告警'), cls: 'mid' },
};

// ctx: { meta, openRuleDraft(draft), openException(prefill) }
export function initBench(ctx) {
  const { meta } = ctx;
  const sceneSel = $('fw-b-scene');
  clear(sceneSel).append(...Object.keys(SCENES).map((k) => h('option', { value: k, text: SCENES[k] })));
  const instSel = $('fw-b-inst');
  instSel.append(...(meta.instances || []).map((i) => h('option', { value: String(i.id), text: i.env ? t('{name}（{env}）', { name: i.name, env: envName(meta, i.env) }) : i.name })));
  const envSel = $('fw-b-env');
  envSel.prepend(...(meta.envs || []).map((e) => h('option', { value: e.code, text: tp(e.name) })));
  $('fw-b-group').append(...(meta.groups || []).map((g) => h('option', { value: String(g.id), text: g.name })));
  const pre = $('fw-b-pre');
  pre.append(...PRESETS.map((p, i) => h('option', { value: String(i), text: p[0] })));

  const syncEnv = () => { $('fw-b-env').disabled = instSel.value !== ''; };
  instSel.addEventListener('change', syncEnv);
  syncEnv();

  pre.addEventListener('change', () => {
    const p = PRESETS[Number(pre.value)];
    if (!p) return;
    $('fw-b-sql').value = p[1];
    sceneSel.value = p[2];
    instSel.value = '';
    syncEnv();
    $('fw-b-env').value = p[3];
    $('fw-b-when').value = p[4];
    $('fw-b-scan').value = p[5];
    $('fw-b-aff').value = p[6];
    run();
  });
  $('fw-b-run').addEventListener('click', run);
  $('fw-b-sql').addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) run(); });
  empty();

  function empty() {
    clear($('fw-b-res')).append(h('div', { class: 'fw-empty', text: t('输入语句后点“评估这条语句”，结果显示在这里') }));
  }

  const intOrNull = (id) => {
    const v = $(id).value.trim();
    return v === '' ? null : Math.max(0, Math.floor(Number(v)));
  };

  async function run() {
    const sql = $('fw-b-sql').value;
    if (!sql.trim()) { toast(t('请输入要测试的语句'), 'warn'); return; }
    const gid = $('fw-b-group').value;
    const body = {
      sql, scene: sceneSel.value, instance_id: Number(instSel.value) || 0, env: instSel.value ? '' : $('fw-b-env').value,
      group_ids: gid ? [Number(gid)] : [], when: $('fw-b-when').value, user: $('fw-b-user').value.trim(),
      scan_rows: intOrNull('fw-b-scan'), affect_rows: intOrNull('fw-b-aff'),
    };
    const out = $('fw-b-res');
    clear(out).append(h('div', { class: 'fw-empty', text: t('评估中…') }));
    try {
      render(await api('POST', BASE + '/evaluate', body), sql);
    } catch (e) {
      clear(out).append(h('div', { class: 'alert err', role: 'alert', text: e.message }));
    }
  }

  function verdictText(r) {
    switch (r.verdict) {
      case 'deny': {
        const name = r.rule ? r.rule.name : '';
        return r.message ? t('由规则「{name}」决定：{msg}', { name, msg: r.message }) : t('由规则「{name}」决定', { name });
      }
      case 'reason': return t('用户需要填写原因：{msgs}', { msgs: (r.reason_msgs || []).filter(Boolean).join(isZh ? '；' : '; ') });
      case 'limit': return t('最多返回 {n} 行（规则「{name}」）', { n: r.max_rows, name: r.limited_by });
      case 'mask': return t('强制脱敏的列：{cols}', { cols: (r.force_mask || []).join(isZh ? '、' : ', ') });
      case 'warn': return (r.warnings || []).join(isZh ? '；' : '; ') || t('放行，命中记录里会留下告警');
    }
    const obs = (r.hits || []).filter((x) => x.effective === 'observe').length;
    return obs ? t('没有会生效的规则命中；观察命中 {n} 条（仅记录，不影响执行）', { n: obs }) : t('没有规则命中');
  }

  function render(r, sql) {
    const out = clear($('fw-b-res'));
    const v = VERDICT[r.verdict] || VERDICT.pass;
    out.append(h('div', { class: 'fw-verdict ' + v.cls }, h('span', { class: 'big', text: v.t }), h('span', { text: verdictText(r) })));
    if (r.verdict === 'deny' || r.verdict === 'reason' || r.verdict === 'limit' || r.verdict === 'mask' || r.verdict === 'warn') {
      const extra = [];
      if (r.verdict !== 'reason' && r.need_reason) extra.push(t('另外：需要填写原因'));
      if (r.verdict !== 'limit' && r.max_rows > 0) extra.push(t('另外：最多返回 {n} 行', { n: r.max_rows }));
      if (r.verdict !== 'mask' && (r.force_mask || []).length) extra.push(t('另外：强制脱敏 {cols}', { cols: r.force_mask.join(isZh ? '、' : ', ') }));
      if (extra.length) out.append(h('div', { class: 'muted small', text: extra.join(isZh ? '；' : '; ') }));
    }
    if (r.failed) out.append(h('div', { class: 'alert warn', text: t('求值过程中出现内部错误，结果仅供参考。') }));
    if (!r.enabled_flag) out.append(h('div', { class: 'alert warn', text: t('总开关已关闭：线上规则不执行，这里按规则本身给出结果。') }));
    if (r.global_mode === 'observe') out.append(h('div', { class: 'alert warn', text: t('全局观察模式生效中：命中只记录，不拦截。') }));

    const f = r.facts || {};
    out.append(h('div', { class: 'muted small' }, ...rich(t('语句类型 {kind} · 涉及 {objects} · 指纹 {fp}{multi}', {
      kind: B(f.kind || '—'), objects: M((f.objects || []).join(isZh ? '、' : ', ') || '—'), fp: C(f.fingerprint || '—'), multi: f.multi ? t(' · 含多条语句') : '',
    }))));

    if ((r.hits || []).length) {
      out.append(...r.hits.map((x) => h('div', { class: 'fw-hit' },
        h('div', null, x.effective === 'observe' ? badge(t('观察，不执行'), 'warn', t('规则生效后将执行：{action}', { action: (ACTIONS[x.would_action] || {}).t || x.would_action })) : actionBadge(x.effective),
          ' ', h('b', { text: x.rule.name }), ' ', h('span', { class: 'muted small', text: t('优先级 {n}', { n: x.rule.priority }) }),
          x.downgraded ? h('span', { class: 'fw-badge warn', title: x.downgraded, text: t('已降级为告警') }) : null),
        h('div', { class: 'why', text: t('命中：{items}', { items: (x.matched || []).join(isZh ? '；' : '; ') || '—' }) }),
        (x.unknown || []).length ? h('div', { class: 'why unk', text: t('未评估：{items}', { items: x.unknown.join(isZh ? '；' : '; ') }) }) : null,
        x.downgraded ? h('div', { class: 'why unk', text: x.downgraded }) : null)));
    } else {
      out.append(h('div', { class: 'fw-empty', text: t('没有任何规则命中') }));
    }

    const skipped = r.skipped || [];
    if (skipped.length) {
      out.append(h('details', { class: 'fw-skipped' }, h('summary', { text: t('另有 {n} 条规则没有命中（范围不符 / 例外 / 已停用 / 条件未评估）', { n: skipped.length }) }),
        h('ul', null, skipped.map((s) => h('li', { text: t('{name}：{reason}', { name: s.rule.name, reason: s.reason }) })))));
    }

    out.append(h('div', null, h('div', { class: 'muted small fw-lbl', text: t('命中位置') }),
      h('pre', { class: 'fw-sql' }, ...highlightNodes(sql, r.highlight))));

    const fp = f.fingerprint || '';
    out.append(h('div', { class: 'fw-btnrow' },
      h('button', { class: 'btn', type: 'button', disabled: !fp, text: t('把这个指纹加入黑名单…'), onclick: () => ctx.openRuleDraft({
        name: t('指纹黑名单 {fp}', { fp }), description: t('来自语句测试台：{sql}', { sql: sql.replace(/\s+/g, ' ').slice(0, 120) }), priority: 5, mode: 'observe',
        scenes: ['query'], match: 'any', conditions: [{ t: 'fingerprint', v: fp }], action: 'deny', message: t('这条语句已被确认有风险，请联系 DBA。'),
      }) }),
      h('button', { class: 'btn', type: 'button', disabled: !fp, text: t('为这个指纹添加例外…'), onclick: () => ctx.openException({ subject_kind: 'fingerprint', subject_value: fp }) })));
    if ((r.notes || []).length) out.append(h('ul', { class: 'fw-notes' }, r.notes.map((n) => h('li', { text: n }))));
  }

  // 供外部（命中详情等）带入语句
  return {
    load(sql, opt = {}) {
      $('fw-b-sql').value = sql;
      if (opt.scene) sceneSel.value = opt.scene;
      if (opt.run) run();
    },
  };
}
