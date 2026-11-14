/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: editor.js
 * @time: 2026/10/28
 */
// 规则编辑器（右侧抽屉）：基本信息 → 适用范围 → 命中条件 → 动作 → 上线前回放。
// 保存走 POST /rules（新建）/ PUT /rules/:id（修改）；校验以服务端为准，错误显示在抽屉顶部。
import { api, riskConfirm, toast } from '../common.js';
import { t, tp, isZh } from '../i18n.js';
import { h, clear, SCENES, ACTIONS, STMT, FEATURES, COND_TYPES, COND_DEFAULT, asList, fmtNum, badge, envName } from './vocab.js';
import { openDrawer } from './drawer.js';

const BASE = '/api/v1/settings/firewall';
const clone = (o) => JSON.parse(JSON.stringify(o));

export function blankRule() {
  return {
    id: 0, name: '', description: '', priority: 100, enabled: true, mode: 'observe', scenes: ['query'], env: 'all',
    instance_ids: [], group_mode: 'all', group_ids: [], match: 'all', conditions: [{ t: 'stmt', v: ['SELECT'] }],
    action: 'warn', action_arg: 0, message: '', notify: false,
  };
}

// 提交给服务端的形态
function inputOf(d) {
  return {
    name: d.name, description: d.description, priority: Number(d.priority) || 0, enabled: d.enabled, mode: d.mode,
    scenes: d.scenes, env: d.env, instance_ids: d.instance_ids, group_mode: d.group_mode,
    group_ids: d.group_mode === 'all' ? [] : d.group_ids, match: d.match,
    conditions: d.conditions.map((c) => ({ t: c.t, v: c.v })), action: d.action,
    action_arg: d.action === 'limit' ? Number(d.action_arg) || 0 : 0, message: d.message, notify: d.notify,
  };
}

// 条件取值规整成界面用的形态（库里读回来的数字 / 列表类型不一定一致）
function normConds(list) {
  return (list || []).map((c) => {
    const t = c.t;
    let v = c.v;
    if (t === 'stmt' || t === 'keyword' || t === 'object') v = asList(v);
    else if (t === 'scan_rows' || t === 'affect_rows') v = Number(v) || 0;
    else v = v === undefined || v === null ? '' : String(v);
    return { t, v };
  });
}

const RE2_UNSUPPORTED = /\(\?<?[=!]|\\[1-9]/;

// opts: { meta, rule?: RuleView（修改）, draft?: 新建时的初始值, replay?: 打开后立即回放, onSaved(rule, isNew) }
export function openEditor(opts) {
  const { meta, rule, onSaved } = opts;
  const isNew = !rule;
  const d = rule ? clone(rule) : Object.assign(blankRule(), clone(opts.draft || {}));
  d.conditions = normConds(d.conditions);
  if (!d.conditions.length) d.conditions = [{ t: 'stmt', v: ['SELECT'] }];
  d.scenes = d.scenes || [];
  d.instance_ids = d.instance_ids || [];
  d.group_ids = d.group_ids || [];
  const snapshot = JSON.stringify(inputOf(d));
  const pending = meta.pending_conditions || [];
  const sceneInfo = Object.fromEntries((meta.scenes || []).map((s) => [s.key, s]));
  let saved = false;

  const err = h('div', { class: 'alert err', role: 'alert', hidden: true });
  const showErr = (m) => { err.textContent = m || ''; err.hidden = !m; if (m) err.scrollIntoView({ block: 'nearest' }); };
  const modeBadge = badge('', '');
  const syncModeBadge = () => {
    modeBadge.className = 'fw-badge ' + (d.mode === 'observe' ? 'warn' : 'ok');
    modeBadge.textContent = d.mode === 'observe' ? t('观察模式') : t('已生效');
  };
  syncModeBadge();

  // ---- 1 基本信息 ----
  const inName = h('input', { type: 'text', value: d.name, maxlength: '100', placeholder: t('如：禁止无条件 DELETE'), 'aria-label': t('规则名称'),
    oninput: () => { d.name = inName.value; } });
  const inDesc = h('input', { type: 'text', value: d.description, maxlength: '500', placeholder: t('说明（可选）'), 'aria-label': t('说明'),
    oninput: () => { d.description = inDesc.value; } });
  const inPrio = h('input', { type: 'number', min: '0', max: '100000', value: String(d.priority), 'aria-label': t('优先级'),
    oninput: () => { d.priority = inPrio.value; } });
  inPrio.className = 'w90';
  const modeBtns = ['observe', 'enforce'].map((m) => h('button', {
    type: 'button', 'data-m': m, 'aria-pressed': String(d.mode === m), text: m === 'observe' ? t('观察：只记录，不拦截') : t('生效：执行动作'),
    onclick: () => { d.mode = m; modeBtns.forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.m === m))); syncModeBadge(); },
  }));
  const sec1 = h('div', { class: 'fw-sec' }, h('h4', null, h('span', { class: 'step', text: '1' }), t('基本信息')),
    h('div', { class: 'fw-fgrid' },
      h('label', { text: t('名称') }), inName,
      h('label', { text: t('说明') }), inDesc,
      h('label', { text: t('优先级（小的先求值）') }), h('div', null, inPrio),
      h('label', { text: t('模式') }), h('div', { class: 'fw-seg warn mode' }, modeBtns)));

  // ---- 2 适用范围 ----
  const scenesBox = h('div', { class: 'fw-checks' }, Object.keys(SCENES).map((k) => {
    const info = sceneInfo[k] || { active: true };
    return h('label', null,
      h('input', { type: 'checkbox', value: k, checked: d.scenes.includes(k), onchange: (e) => {
        d.scenes = e.target.checked ? [...new Set([...d.scenes, k])] : d.scenes.filter((x) => x !== k);
      } }), SCENES[k], info.active ? null : h('span', { class: 'tag', text: t('尚未接入') }));
  }));
  const envSel = h('select', { 'aria-label': t('实例环境'), onchange: () => { d.env = envSel.value; } },
    [['all', t('全部实例')], ...(meta.envs || []).map((e) => [e.code, t('仅{name}实例', { name: tp(e.name) })])].map(([v, label]) => h('option', { value: v, selected: d.env === v, text: label })));
  const instBox = h('div', { class: 'fw-instbox' }, (meta.instances || []).map((i) => h('label', null,
    h('input', { type: 'checkbox', value: String(i.id), checked: d.instance_ids.includes(i.id), onchange: (e) => {
      d.instance_ids = e.target.checked ? [...new Set([...d.instance_ids, i.id])] : d.instance_ids.filter((x) => x !== i.id);
    } }), i.name, i.env ? h('span', { class: 'muted small', text: envName(meta, i.env) }) : null)));
  const instDetails = h('details', { open: d.instance_ids.length > 0 },
    h('summary', { class: 'muted', text: t('限定到指定实例（可选）') }), instBox);
  const groupBox = h('div', { class: 'fw-checks', hidden: d.group_mode === 'all' }, (meta.groups || []).map((g) => h('label', null,
    h('input', { type: 'checkbox', value: String(g.id), checked: d.group_ids.includes(g.id), onchange: (e) => {
      d.group_ids = e.target.checked ? [...new Set([...d.group_ids, g.id])] : d.group_ids.filter((x) => x !== g.id);
    } }), g.name)));
  const gmodeSel = h('select', { 'aria-label': t('用户范围'), onchange: () => { d.group_mode = gmodeSel.value; groupBox.hidden = d.group_mode === 'all'; } },
    [['all', t('全部用户')], ['only', t('仅这些权限组')], ['except', t('排除这些权限组')]].map(([v, label]) => h('option', { value: v, selected: d.group_mode === v, text: label })));
  const sec2 = h('div', { class: 'fw-sec' }, h('h4', null, h('span', { class: 'step', text: '2' }), t('适用范围')),
    h('div', { class: 'fw-fgrid' },
      h('label', { class: 'top', text: t('场景') }), h('div', null, scenesBox),
      h('label', { class: 'top', text: t('实例') }), h('div', null, envSel, h('div', { class: 'fw-gap' }, instDetails)),
      h('label', { class: 'top', text: t('用户') }), h('div', null, gmodeSel, h('div', { class: 'fw-gap' }, groupBox))));

  // ---- 3 条件 ----
  const condsBox = h('div');
  const matchBtns = ['all', 'any'].map((m) => h('button', {
    type: 'button', 'data-m': m, 'aria-pressed': String(d.match === m), text: m === 'all' ? t('全部满足') : t('任一满足'),
    onclick: () => { d.match = m; matchBtns.forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.m === m))); },
  }));
  const listInput = (i, mono, placeholder) => h('input', {
    type: 'text', class: mono ? 'mono' : null, value: asList(d.conditions[i].v).join(', '), placeholder,
    oninput: (e) => { d.conditions[i].v = e.target.value.split(/[,，\n]/).map((x) => x.trim()).filter(Boolean); },
  });
  function paramEditor(c, i) {
    switch (c.t) {
      case 'stmt':
        return h('div', { class: 'fw-checks' }, STMT.map((k) => h('label', null,
          h('input', { type: 'checkbox', value: k, checked: asList(c.v).includes(k), onchange: (e) => {
            const cur = asList(d.conditions[i].v);
            d.conditions[i].v = e.target.checked ? [...new Set([...cur, k])] : cur.filter((x) => x !== k);
          } }), k)));
      case 'feature':
        return h('select', { 'aria-label': t('结构特征'), onchange: (e) => { d.conditions[i].v = e.target.value; } },
          Object.keys(FEATURES).map((k) => h('option', { value: k, selected: c.v === k, text: FEATURES[k] })));
      case 'keyword':
        return listInput(i, false, t('逗号分隔，如 SLEEP, BENCHMARK'));
      case 'regex': {
        const hint = h('div', { class: 'hint', hidden: true });
        const inp = h('input', { type: 'text', class: 'mono', value: String(c.v || ''), placeholder: t('如 information_schema\\.processlist'),
          oninput: (e) => {
            d.conditions[i].v = e.target.value;
            const v = e.target.value;
            let bad = '';
            if (RE2_UNSUPPORTED.test(v)) bad = t('含有 RE2 不支持的写法（反向引用 / 环视），保存时会被拒绝。');
            else { try { new RegExp(v); } catch (ex) { bad = t('正则可能有语法错误：{msg}', { msg: ex.message }); } }
            hint.className = 'hint bad';
            hint.textContent = bad;
            hint.hidden = !bad;
          } });
        return h('div', null, inp, hint);
      }
      case 'object':
        return listInput(i, true, 'shop.orders, *.*.phone');
      case 'scan_rows':
      case 'affect_rows':
        return h('div', { class: 'fw-row' },
          h('input', { type: 'number', min: '1', value: String(c.v), 'aria-label': t('行数'), oninput: (e) => { d.conditions[i].v = Number(e.target.value) || 0; } }), t('行|unit'));
      case 'time':
        return h('select', { 'aria-label': t('时间'), onchange: (e) => { d.conditions[i].v = e.target.value; } },
          [['offhours', t('非工作时间')], ['workhours', t('工作时间')]]
            .map(([v, label]) => h('option', { value: v, selected: c.v === v, text: label })));
      case 'fingerprint':
        return h('input', { type: 'text', class: 'mono', value: String(c.v || ''), placeholder: t('8 位十六进制'), maxlength: '8', 'aria-label': t('SQL 指纹'),
          oninput: (e) => { d.conditions[i].v = e.target.value.trim().toLowerCase(); } });
    }
    return h('span');
  }
  function renderConds() {
    clear(condsBox);
    d.conditions.forEach((c, i) => {
      const typeSel = h('select', { 'aria-label': t('条件类型'), onchange: (e) => {
        d.conditions[i] = { t: e.target.value, v: COND_DEFAULT[e.target.value]() };
        renderConds();
      } }, Object.keys(COND_TYPES).map((k) => h('option', { value: k, selected: c.t === k,
        text: pending.includes(k) ? t('{type}（暂未接入）', { type: COND_TYPES[k] }) : COND_TYPES[k] })));
      condsBox.append(h('div', { class: 'fw-crow' }, typeSel, paramEditor(c, i),
        h('button', { class: 'fw-link danger', type: 'button', 'aria-label': t('删除条件'), text: '✕', onclick: () => { d.conditions.splice(i, 1); renderConds(); } })));
    });
  }
  renderConds();
  const sec3 = h('div', { class: 'fw-sec' },
    h('h4', null, h('span', { class: 'step', text: '3' }), t('命中条件'), h('span', { class: 'fw-seg' }, matchBtns)),
    condsBox,
    h('div', { class: 'fw-gap' }, h('button', { class: 'btn', type: 'button', text: t('＋ 添加条件'), onclick: () => {
      if (d.conditions.length >= (meta.max_conditions || 20)) { toast(t('一条规则最多 {n} 个条件', { n: meta.max_conditions || 20 }), 'warn'); return; }
      d.conditions.push({ t: 'keyword', v: [] });
      renderConds();
    } })));

  // ---- 4 动作 ----
  const actsBox = h('div', { class: 'fw-acts' });
  const limitRow = h('div', { hidden: d.action !== 'limit' },
    h('input', { type: 'number', min: '1', max: '1000000', value: String(d.action_arg || 1000), 'aria-label': t('最多返回行数'),
      oninput: (e) => { d.action_arg = Number(e.target.value) || 0; } }), t(' 行|unit'));
  if (!d.action_arg) d.action_arg = 1000;
  const renderActs = () => {
    clear(actsBox);
    for (const k of Object.keys(ACTIONS)) {
      actsBox.append(h('button', { type: 'button', class: 'fw-act ' + ACTIONS[k].cls, 'aria-pressed': String(d.action === k), onclick: () => {
        d.action = k;
        limitRow.hidden = k !== 'limit';
        renderActs();
      } }, h('b', { text: ACTIONS[k].t })));
    }
  };
  renderActs();
  const inMsg = h('input', { type: 'text', value: d.message, maxlength: '500', placeholder: t('被拦截时用户看到的说明'), 'aria-label': t('提示文案'),
    oninput: () => { d.message = inMsg.value; } });
  const sec4 = h('div', { class: 'fw-sec' }, h('h4', null, h('span', { class: 'step', text: '4' }), t('命中后的动作')), actsBox,
    h('div', { class: 'fw-fgrid fw-gap' },
      h('label', { text: t('最多返回') }), limitRow,
      h('label', { text: t('提示文案') }), inMsg));

  // ---- 5 回放 ----
  const replayBox = h('div', { class: 'fw-replay', id: 'fw-replay' },
    h('button', { class: 'btn', type: 'button', text: t('开始回放'), onclick: () => runReplay() }));
  async function runReplay() {
    clear(replayBox).append(h('span', { class: 'muted', text: t('回放中…') }));
    try {
      const r = await api('POST', BASE + '/replay', { rule: inputOf(d), days: 7 });
      renderReplay(r);
    } catch (e) {
      clear(replayBox).append(h('div', { class: 'alert err', text: t('回放失败：{msg}', { msg: e.message }) }),
        h('button', { class: 'btn', type: 'button', text: t('重试'), onclick: () => runReplay() }));
    }
  }
  function renderReplay(r) {
    const box = clear(replayBox);
    if (!r.applicable) {
      box.append(h('div', { class: 'muted', text: (r.notes || [])[0] || t('这条规则没有可回放的数据。') }));
      return;
    }
    const pct = (r.rate * 100).toFixed(1);
    const bs = r.scanned_by_scene || {};
    const src = Object.keys(bs).map((k) => (SCENES[k] || k) + ' ' + fmtNum(bs[k])).join(isZh ? '、' : ', ');
    box.append(h('div', null,
      h('b', { text: t('最近 {days} 天 {scanned} 条语句（{src}）中，命中 {hits} 条（{pct}%），涉及 {users} 位用户', {
        days: r.days, scanned: fmtNum(r.scanned), src: src || t('无'), hits: fmtNum(r.hits), pct, users: fmtNum(r.users),
      }) }),
      r.hits ? (r.dba_known ? t('，其中 {n} 条来自 DBA 组。', { n: fmtNum(r.dba_hits) }) : (isZh ? '。' : '.')) : t('：不会影响任何人。')));
    if ((r.samples || []).length) {
      box.append(h('ul', null, r.samples.map((s) => h('li', null,
        h('span', { class: 'mono small', text: s.sql }),
        h('div', { class: 'muted small', text: `${SCENES[s.scene] || ''} · ${s.user} · ${s.instance || '—'}${s.db ? ' / ' + s.db : ''} · ${s.at}${s.why && s.why.length ? ' · ' + s.why.join(isZh ? '；' : '; ') : ''}` })))));
    }
    if (r.advice) box.append(h('div', { class: 'alert ' + (r.rate > 0.01 ? 'warn' : 'info'), text: r.advice }));
    if ((r.notes || []).length) box.append(h('ul', { class: 'notes' }, r.notes.map((n) => h('li', { text: n }))));
    box.append(h('div', { class: 'fw-gap' }, h('button', { class: 'btn', type: 'button', text: t('重新回放'), onclick: () => runReplay() })));
  }
  const sec5 = h('div', { class: 'fw-sec' }, h('h4', null, h('span', { class: 'step', text: '5' }), t('上线前：用历史语句回放')), replayBox);

  // ---- 保存 ----
  const saveBtn = h('button', { class: 'btn primary', type: 'button', text: t('保存'), onclick: () => save() });
  const cancelBtn = h('button', { class: 'btn ghost', type: 'button', text: t('取消'), onclick: () => ui.requestClose() });
  const dirty = () => !saved && JSON.stringify(inputOf(d)) !== snapshot;
  async function save() {
    showErr('');
    const body = inputOf(d);
    if (!body.name.trim()) return showErr(t('请填写规则名称'));
    if (!body.scenes.length) return showErr(t('至少选择一个适用场景'));
    if (!body.conditions.length) return showErr(t('至少需要一个命中条件'));
    if (body.mode === 'enforce' && body.enabled && (isNew || rule.mode !== 'enforce')) {
      const r = await riskConfirm({
        title: t('以“生效”模式保存？'), level: 'high', okText: t('保存并生效'),
        lines: [t('保存后，规则「{name}」会立即对命中的语句执行「{action}」。', { name: body.name.trim(), action: ACTIONS[body.action].t }),
          t('建议先用“观察”模式跑一段时间，看过命中记录和历史回放再切到生效。')],
      });
      if (!r.ok) return;
    }
    saveBtn.disabled = true;
    saveBtn.setAttribute('aria-busy', 'true');
    try {
      const res = await api(isNew ? 'POST' : 'PUT', isNew ? BASE + '/rules' : `${BASE}/rules/${rule.id}`, body);
      saved = true;
      ui.close();
      onSaved(res, isNew);
    } catch (e) {
      showErr(e.message);
    } finally {
      saveBtn.disabled = false;
      saveBtn.removeAttribute('aria-busy');
    }
  }

  const ui = openDrawer({
    title: isNew ? t('新建规则') : t('编辑规则'), badge: modeBadge,
    body: [err, sec1, sec2, sec3, sec4, sec5], footer: [cancelBtn, saveBtn],
    canClose: async () => {
      if (!dirty()) return true;
      const r = await riskConfirm({ title: t('放弃未保存的修改？'), lines: [t('离开后这次的修改会丢失。')], okText: t('放弃修改'), danger: true });
      return r.ok;
    },
  });
  if (opts.replay) {
    runReplay().then(() => replayBox.scrollIntoView({ block: 'center' }));
  }
  return ui;
}
