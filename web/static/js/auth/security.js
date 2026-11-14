/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: security.js
 * @time: 2026/10/28
 */
// 登录与安全设置（仅超级用户）：账号安全、会话与 API、页面水印、两步验证短信服务，以及登录方式的只读摘要。
// 保存前先列出变更差异和风险提示，二次确认后才提交；保存成功后水印立即按新配置重画（不用刷新页面）。
// 往页面写内容一律用 textContent / createElement，不拼 innerHTML。
import { api, confirmRun, toast, setButtonMeta } from '../common.js';
import { reload as reloadWatermark, preview as previewWatermark, buildTile, fillTemplate } from './watermark.js';
import { t, tp, isZh } from '../i18n.js';

const $ = (id) => document.getElementById(id);
const URL_SEC = '/api/v1/settings/security';

let data = null; // GET 的返回：settings / auth_groups / resource_groups / users / providers / twofa / me

// [元素 id, 设置键]。数字项的取值范围写在模板的 data-min / data-max 上
const NUMS = [
  ['sec-lock-cnt', 'lock_cnt_threshold'], ['sec-lock-time', 'lock_time_threshold'], ['sec-pw-min', 'password_min_len'],
  ['sec-session-min', 'session_minutes'], ['sec-jwt-hours', 'jwt_hours'],
  ['sec-wm-size', 'watermark_font_size'], ['sec-wm-angle', 'watermark_angle'],
];
const BOOLS = [
  ['sec-enforce-2fa', 'enforce_2fa'], ['sec-signup', 'sign_up_enabled'], ['sec-watermark', 'watermark_enabled'],
  ['sec-pw-complex', 'password_complexity'], ['sec-browser-close', 'session_browser_close'], ['sec-jwt-otp', 'api_jwt_require_otp'],
];
const TEXTS = [
  ['sec-ali-id', 'aliyun_access_key_id'], ['sec-ali-sign', 'aliyun_sign_name'], ['sec-ali-tpl', 'aliyun_template_code'],
  ['sec-ali-var', 'aliyun_variable_name'], ['sec-tc-id', 'tencent_secret_id'], ['sec-tc-sign', 'tencent_sign_name'],
  ['sec-tc-tpl', 'tencent_template_id'], ['sec-tc-appid', 'tencent_sdk_appid'],
  ['sec-wm-text', 'watermark_text'], ['sec-wm-extra', 'watermark_extra'],
];
// 水印的两个下拉与透明度滑块
const SELECTS = [['sec-wm-gap', 'watermark_gap'], ['sec-wm-color', 'watermark_color']];
// 短信密钥永不回显：留空提交表示不修改
const SECRETS = [['sec-ali-secret', 'aliyun_access_key_secret'], ['sec-tc-key', 'tencent_secret_key']];
setButtonMeta({ 'sec.edit': { name: t('修改登录与安全设置'), kind: 'd' } }); // 确认弹窗页脚显示
const STATUS = { enabled: [t('启用|status'), 'passed'], disabled: [t('停用|status'), 'aborted'], error: [t('异常'), 'rejected'] };

// 水印模板可用的变量：第一项是写进水印模板的变量名（存库，不能翻译），第二项是按钮提示
const WM_VARS = [['账号', t('登录账号')], ['姓名', t('姓名')], ['部门', t('所属部门')], ['IP', t('本次请求的 IP')], ['日期', t('当前日期')], ['时间', t('当前日期 + 时间（到分钟）')]]; // i18n-ignore
const WM_DEF = { text: '{姓名}（{账号}）{时间}', extra: '', opacity: 14, size: 14, angle: -22, gap: 'normal', color: 'auto' }; // i18n-ignore 默认水印模板（存库的变量名）

// 设置项的显示名（变更差异里用）
const LABELS = {
  lock_cnt_threshold: t('登录失败锁定次数'), lock_time_threshold: t('锁定时间（秒）'), enforce_2fa: t('强制两步验证'), sign_up_enabled: t('开放自助注册'),
  password_min_len: t('密码最小长度'), password_complexity: t('密码需含字母 / 数字 / 特殊字符'), session_minutes: t('会话有效期（分钟）'),
  session_browser_close: t('关闭浏览器即退出'), jwt_hours: t('JWT 有效期（小时）'), api_jwt_require_otp: t('JWT 登录要求 OTP'), api_user_whitelist: t('API 用户白名单（用户 ID）'),
  default_auth_group: t('新用户默认权限组'), default_resource_group: t('新用户默认资源组'), watermark_enabled: t('页面水印'), watermark_text: t('水印内容模板'),
  watermark_extra: t('水印附加文字'), watermark_opacity: t('水印透明度（%）'), watermark_font_size: t('水印字号（px）'), watermark_angle: t('水印倾斜角度（°）'),
  watermark_gap: t('水印疏密'), watermark_color: t('水印颜色'), sms_provider: t('短信服务商'), aliyun_access_key_id: t('阿里云 AccessKey ID'),
  aliyun_access_key_secret: t('阿里云 AccessKey Secret'), aliyun_sign_name: t('阿里云短信签名'), aliyun_template_code: t('阿里云模板编号'),
  aliyun_variable_name: t('阿里云模板变量名'), tencent_secret_id: t('腾讯云 SecretId'), tencent_secret_key: t('腾讯云 SecretKey'), tencent_sign_name: t('腾讯云短信签名'),
  tencent_template_id: t('腾讯云模板 ID'), tencent_sdk_appid: t('腾讯云 SDK AppID'),
};
const SHOWN = {
  gap: { tight: t('紧凑'), normal: t('标准'), loose: t('宽松') },
  color: { auto: t('跟随主题'), gray: t('灰色'), red: t('红色'), blue: t('蓝色') },
  sms: { disabled: t('关闭|off'), aliyun: t('阿里云'), tencent: t('腾讯云') },
};

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

function msgInto(box, text, kind = 'err') {
  box.replaceChildren();
  if (!text) return;
  const d = el('div', 'alert ' + kind, text);
  d.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  box.append(d);
}
const setMsg = (text, kind) => msgInto($('sec-msg'), text, kind);

async function busy(btn, fn) {
  if (btn) { btn.disabled = true; btn.setAttribute('aria-busy', 'true'); }
  try { return await fn(); } finally { if (btn) { btn.disabled = false; btn.removeAttribute('aria-busy'); } }
}

// 多选框。已保存但选项里没有的值（组已被删除、用户不在列表里）也保留为选中，避免一保存就被悄悄去掉
function fillMulti(sel, items, valueOf, textOf, chosen, missingText) {
  const want = new Set((chosen || []).map(String));
  const opts = (items || []).map((it) => {
    const o = new Option(textOf(it), valueOf(it));
    o.selected = want.delete(o.value);
    return o;
  });
  for (const v of want) {
    const o = new Option(missingText(v), v);
    o.selected = true;
    opts.push(o);
  }
  sel.replaceChildren(...opts);
}
const pickedValues = (sel) => [...sel.selectedOptions].map((o) => o.value);

// ---- 填表 ----

function syncSMS() {
  const v = $('sec-sms-provider').value;
  $('sec-sms-aliyun').hidden = v !== 'aliyun';
  $('sec-sms-tencent').hidden = v !== 'tencent';
  $('sec-sms-test-box').hidden = v === 'disabled';
}

function fill(s) {
  for (const [id, k] of NUMS) $(id).value = s[k] === undefined || s[k] === null ? '' : String(s[k]);
  for (const [id, k] of BOOLS) $(id).checked = !!s[k];
  for (const [id, k] of TEXTS) $(id).value = s[k] || '';
  $('sec-wm-text').value = s.watermark_text || WM_DEF.text;
  for (const [id, k] of SELECTS) {
    $(id).value = s[k] || '';
    if ($(id).selectedIndex < 0) $(id).selectedIndex = 0;
  }
  $('sec-wm-op').value = String(s.watermark_opacity || WM_DEF.opacity);
  const has = s.has_secrets || {};
  for (const [id, k] of SECRETS) {
    $(id).value = '';
    $(id).placeholder = has[k] ? t('已配置（留空表示不修改）') : t('加密存储，不回显');
  }
  fillMulti($('sec-whitelist'), data.users, (u) => String(u.id), (u) => u.name, s.api_user_whitelist,
    (v) => t('用户 #{id}（不在列表中）', { id: v }));
  fillMulti($('sec-def-ag'), data.auth_groups, (g) => g.name, (g) => g.name, s.default_auth_group, (v) => t('{name}（已不存在）', { name: v }));
  fillMulti($('sec-def-rg'), data.resource_groups, (g) => g.name, (g) => g.name, s.default_resource_group, (v) => t('{name}（已不存在）', { name: v }));
  const sms = $('sec-sms-provider');
  sms.value = s.sms_provider || 'disabled';
  if (sms.selectedIndex < 0) sms.value = 'disabled';
  syncSMS();
  for (const [id] of NUMS) setErr($(id), '');
  wmLive();
}

function renderProvs(list) {
  const tb = $('sec-provs');
  tb.replaceChildren();
  if (!list || !list.length) {
    const tr = el('tr');
    const td = el('td', 'empty', t('还没有定义外部登录方式'));
    td.colSpan = 3;
    tr.append(td);
    tb.append(tr);
    return;
  }
  for (const p of list) {
    const tr = el('tr');
    const kind = el('td');
    kind.append(el('span', 'badge', tp(p.kind)));
    const st = el('td');
    const [text, cls] = STATUS[p.status] || [p.status, 'aborted'];
    st.append(el('span', 'st ' + cls, text));
    tr.append(el('td', '', p.name), kind, st);
    tb.append(tr);
  }
}

// ---- 概览 KPI（按已保存的设置）----

function renderKpis(s) {
  $('kpi-lock').textContent = t('{n} 次 / {m} 分钟', { n: s.lock_cnt_threshold, m: Math.round(s.lock_time_threshold / 60 * 10) / 10 });
  $('kpi-2fa').textContent = s.enforce_2fa ? t('强制') : t('自愿');
  $('kpi-2fa').className = 'kv' + (s.enforce_2fa ? ' ok' : '');
  const tf = data && data.twofa;
  $('kpi-2fa-d').textContent = tf ? t('{bound} / {total} 个账号已绑定', { bound: tf.bound, total: tf.total }) : (s.sms_provider && s.sms_provider !== 'disabled' ? t('TOTP + 短信') : t('仅 TOTP'));
  $('kpi-pw').textContent = t('≥ {n} 位', { n: s.password_min_len });
  $('kpi-pw-d').textContent = s.password_complexity ? t('需含字母 / 数字 / 特殊字符') : t('不要求复杂度');
  const h = (m) => (m % 60 === 0 ? `${m / 60}h` : `${m}m`);
  $('kpi-sess').textContent = `${h(s.session_minutes)} / ${s.jwt_hours}h`;
  $('kpi-sess-d').textContent = s.sign_up_enabled ? t('自助注册：开放') : t('自助注册：关闭');
}

function refresh(s) {
  renderKpis(s);
}

// ---- 校验与保存 ----

// 在输入框所在 label 末尾显示错误，并通过 aria-describedby 让读屏软件读到
function setErr(inp, text) {
  const label = inp.closest('label');
  if (inp.dataset.desc === undefined) inp.dataset.desc = inp.getAttribute('aria-describedby') || '';
  let s = label.querySelector('.field-err');
  if (!text) {
    if (s) s.remove();
    inp.removeAttribute('aria-invalid');
    if (inp.dataset.desc) inp.setAttribute('aria-describedby', inp.dataset.desc);
    else inp.removeAttribute('aria-describedby');
    return;
  }
  if (!s) {
    s = el('small', 'field-err');
    s.id = inp.id + '-err';
    label.append(s);
  }
  s.textContent = text;
  inp.setAttribute('aria-invalid', 'true');
  inp.setAttribute('aria-describedby', (inp.dataset.desc + ' ' + s.id).trim());
}

const runes = (s) => [...s].length;

// 水印内容 / 附加文字：与服务端同样的规则（以服务端为准，这里只是少一次往返）
function wmTextError(text, extra) {
  const known = WM_VARS.map((v) => v[0]);
  if (!text.trim()) return t('水印内容不能为空');
  if (runes(text) > 60) return t('水印内容最长 60 个字符');
  if (runes(extra) > 40) return t('附加文字最长 40 个字符');
  for (const part of [text, extra]) {
    const bad = (part.match(/\{[^{}]*\}/g) || []).find((v) => !known.includes(v.slice(1, -1)));
    if (bad) return t('不认识的变量 {name}，可用：{list}', { name: bad, list: known.map((k) => '{' + k + '}').join(' ') });
    if (/[{}]/.test(part.replace(/\{[^{}]*\}/g, ''))) return t('花括号不成对：变量要写成 {账号} 这样的形式');
  }
  if (!/\{(账号|姓名)\}/.test(text)) return t('水印内容必须包含 {账号} 或 {姓名}，否则截图无法追溯到具体的人');
  return '';
}

function validate() {
  const nums = {};
  let firstBad = null;
  for (const [id, k] of NUMS) {
    const inp = $(id);
    const s = inp.value.trim();
    const min = Number(inp.dataset.min);
    const max = Number(inp.dataset.max);
    let err = '';
    if (!(min < 0 ? /^-?\d+$/ : /^\d+$/).test(s)) err = min < 0 ? t('{name}必须是整数', { name: inp.dataset.name }) : t('{name}必须是正整数', { name: inp.dataset.name });
    else if (Number(s) < min || Number(s) > max) err = t('{name}应在 {min} 到 {max} 之间', { name: inp.dataset.name, min, max });
    setErr(inp, err);
    if (err && !firstBad) firstBad = inp;
    nums[k] = Number(s);
  }
  const tErr = wmTextError($('sec-wm-text').value, $('sec-wm-extra').value);
  setErr($('sec-wm-text'), tErr);
  if (tErr && !firstBad) firstBad = $('sec-wm-text');
  return { firstBad, nums };
}

function body(nums) {
  const b = { ...nums };
  for (const [id, k] of BOOLS) b[k] = $(id).checked;
  for (const [id, k] of TEXTS) b[k] = $(id).value.trim();
  for (const [id, k] of SELECTS) b[k] = $(id).value;
  b.watermark_opacity = Number($('sec-wm-op').value);
  for (const [id, k] of SECRETS) b[k] = $(id).value;
  b.api_user_whitelist = pickedValues($('sec-whitelist')).map(Number);
  b.default_auth_group = pickedValues($('sec-def-ag'));
  b.default_resource_group = pickedValues($('sec-def-rg'));
  b.sms_provider = $('sec-sms-provider').value;
  return b;
}

const listText = (v) => (Array.isArray(v) ? [...v].map(String).sort().join(isZh ? '、' : ', ') : String(v ?? ''));
function shown(k, v) {
  if (typeof v === 'boolean') return v ? t('开启') : t('关闭|off');
  if (k === 'watermark_gap') return SHOWN.gap[v] || String(v);
  if (k === 'watermark_color') return SHOWN.color[v] || String(v);
  if (k === 'sms_provider') return SHOWN.sms[v] || String(v);
  if (Array.isArray(v)) return v.length ? listText(v) : t('（空）');
  return v === '' || v === undefined || v === null ? t('（空）') : String(v);
}

// 对比已保存的设置与表单：返回 [{key, label, from, to}]；密钥只在填了新值时记为“已修改”
function diffOf(old, nw) {
  const out = [];
  for (const k of Object.keys(LABELS)) {
    if (SECRETS.some(([, sk]) => sk === k)) {
      if (nw[k]) out.push({ key: k, label: LABELS[k], from: old.has_secrets && old.has_secrets[k] ? t('已配置') : t('（未设置）'), to: t('已修改') });
      continue;
    }
    if (nw[k] === undefined) continue;
    const a = Array.isArray(nw[k]) ? listText(old[k] || []) : String(old[k] ?? '');
    const b = Array.isArray(nw[k]) ? listText(nw[k]) : String(nw[k]);
    if (a !== b) out.push({ key: k, label: LABELS[k], from: shown(k, old[k] ?? ''), to: shown(k, nw[k]) });
  }
  return out;
}

// 风险提示：降低安全强度的修改（不阻止保存，只让管理员看清楚）
function risksOf(old, nw) {
  const w = [];
  if (nw.password_min_len < old.password_min_len) w.push(t('密码最小长度降低（{from} → {to}）：已有账号不受影响，新建 / 改密按新规则。', { from: old.password_min_len, to: nw.password_min_len }));
  if (old.enforce_2fa && !nw.enforce_2fa) w.push(t('关闭“强制两步验证”：未绑定两步验证的用户登录时不再被要求绑定。'));
  if (!old.sign_up_enabled && nw.sign_up_enabled) w.push(t('开放自助注册：任何能访问登录页的人都可以注册账号（按默认权限组入组）。'));
  if (nw.lock_cnt_threshold > old.lock_cnt_threshold || nw.lock_time_threshold < old.lock_time_threshold) w.push(t('放宽账号锁定策略（次数变多或锁定时间变短）会降低暴力破解的成本。'));
  if (old.api_jwt_require_otp && !nw.api_jwt_require_otp) w.push(t('关闭“JWT 登录要求 OTP”：已绑定两步验证的用户通过 API 登录将不再校验动态口令。'));
  if (old.watermark_enabled && !nw.watermark_enabled) w.push(t('关闭页面水印：此后截图 / 拍屏将无法追溯到具体的人。'));
  if (nw.watermark_enabled && nw.watermark_opacity < 6) w.push(t('水印透明度只有 {n}%：截图后很可能看不清，起不到溯源作用。', { n: nw.watermark_opacity }));
  if ((old.api_user_whitelist || []).length && !(nw.api_user_whitelist || []).length) w.push(t('清空 API 用户白名单：此后任何用户的 JWT 都能调用 API。'));
  if (nw.session_minutes > old.session_minutes && nw.session_minutes > 60 * 24) w.push(t('会话有效期超过 24 小时（{n} 分钟）：无人看管的浏览器保持登录的时间更长。', { n: nw.session_minutes }));
  return w;
}

$('sec-save').addEventListener('click', async () => {
  setMsg('');
  const v = validate();
  if (v.firstBad) {
    setMsg(t('有设置项不合法，请按提示修改后再保存。'));
    v.firstBad.focus();
    return;
  }
  const b = body(v.nums);
  const changes = diffOf(data.settings, b);
  if (!changes.length) {
    setMsg(t('没有变更。'), 'info');
    toast(t('没有变更'), 'warn');
    return;
  }
  const warns = risksOf(data.settings, b);
  // 变更差异 + 风险提示一起放进统一确认框；保存失败留在框里，可以改完重试
  const r = await confirmRun({
    title: t('确认保存登录与安全设置'), code: 'sec.edit', level: 'high',
    lines: [t('共 {n} 项变更，保存后立即生效（本实例立即，其他实例最多 10 秒后）。密钥类项只显示“已修改”，审计里也不记录密钥内容。', { n: changes.length }),
      ...(warns.length ? [t('请确认以下风险：'), ...warns.map((w) => '· ' + w)] : [])],
    rows: changes.map((c) => ({ label: c.label, from: c.from, to: c.to })),
    okText: warns.length ? t('我已了解风险，确认保存') : t('确认保存'),
  }, () => api('PUT', URL_SEC, b));
  if (!r.ok) return;
  try {
    const s = r.result;
    data.settings = s;
    fill(s);
    refresh(s);
    await reloadWatermark(); // 水印立即按新配置重画，不用刷新页面
    setMsg(t('已保存，立即生效'), 'info');
  } catch (err) {
    setMsg(err.message);
  }
});

// ---- 页面水印：变量按钮、实时预览、全页预览 ----

function buildVarButtons() {
  const box = $('sec-wm-vars');
  box.replaceChildren(...WM_VARS.map(([name, tip]) => {
    const b = el('button', 'btn mini', '{' + name + '}');
    b.type = 'button';
    b.title = tip;
    b.addEventListener('click', () => insertVar(name));
    return b;
  }));
}

function insertVar(name) {
  const inp = $('sec-wm-text');
  const v = '{' + name + '}';
  const a = inp.selectionStart === null ? inp.value.length : inp.selectionStart;
  const e = inp.selectionEnd === null ? a : inp.selectionEnd;
  inp.value = inp.value.slice(0, a) + v + inp.value.slice(e);
  inp.focus();
  inp.setSelectionRange(a + v.length, a + v.length);
  wmLive();
}

const clampInt = (s, lo, hi, def) => {
  const n = parseInt(s, 10);
  return n >= lo && n <= hi ? n : def;
};

// 从表单取水印配置（取不到合法值就用默认值，实时预览不报错）
function wmForm() {
  const vars = (data && data.me) || {};
  return {
    lines: [fillTemplate($('sec-wm-text').value, vars), $('sec-wm-extra').value ? fillTemplate($('sec-wm-extra').value, vars) : ''],
    opacity: clampInt($('sec-wm-op').value, 3, 30, WM_DEF.opacity),
    font_size: clampInt($('sec-wm-size').value, 12, 28, WM_DEF.size),
    angle: clampInt($('sec-wm-angle').value, -60, 0, WM_DEF.angle),
    gap: $('sec-wm-gap').value || WM_DEF.gap,
    color: $('sec-wm-color').value || WM_DEF.color,
  };
}

function wmLive() {
  const c = wmForm();
  $('sec-wm-opv').textContent = c.opacity + '%';
  const tile = buildTile(c);
  const p = $('sec-wm-prev');
  p.style.backgroundImage = tile ? tile.url : '';
  p.style.backgroundSize = tile ? `${tile.w}px ${tile.h}px` : '';
}

for (const id of ['sec-wm-text', 'sec-wm-extra', 'sec-wm-op', 'sec-wm-size', 'sec-wm-angle']) $(id).addEventListener('input', wmLive);
for (const id of ['sec-wm-gap', 'sec-wm-color']) $(id).addEventListener('change', wmLive);
document.addEventListener('skinchange', wmLive); // “跟随主题”的颜色随皮肤变

$('sec-wm-full').addEventListener('click', () => {
  const err = wmTextError($('sec-wm-text').value, $('sec-wm-extra').value);
  if (err) { msgInto($('sec-wm-msg'), err); return; }
  msgInto($('sec-wm-msg'), t('已临时显示水印 5 秒（未保存）'), 'info');
  previewWatermark(wmForm(), 5000);
  setTimeout(() => msgInto($('sec-wm-msg'), ''), 5200);
});

// ---- 短信 ----
$('sec-sms-provider').addEventListener('change', syncSMS);

$('sec-sms-test').addEventListener('click', (e) => busy(e.currentTarget, async () => {
  const box = $('sec-sms-msg');
  const phone = $('sec-sms-phone').value.trim();
  if (!phone) {
    msgInto(box, t('请填写测试手机号'));
    $('sec-sms-phone').focus();
    return;
  }
  try {
    await api('POST', URL_SEC + '/sms-test', { phone });
    const saved = (data.settings && data.settings.sms_provider) || 'disabled';
    const unsaved = saved !== $('sec-sms-provider').value;
    msgInto(box, unsaved
      ? t('测试短信已发送到 {phone}，请查收（注意：服务商的修改还没保存，本次用的是已保存的配置）', { phone })
      : t('测试短信已发送到 {phone}，请查收', { phone }), 'info');
  } catch (err) {
    msgInto(box, err.message);
  }
}));

async function load() {
  data = await api('GET', URL_SEC);
  buildVarButtons();
  fill(data.settings || {});
  renderProvs(data.providers);
  refresh(data.settings || {});
  setMsg('');
  $('sec-save').disabled = false; // 加载成功前不允许保存，免得把空表单提交上去
}

load().catch((e) => {
  setMsg(t('加载失败：') + e.message);
  const tr = el('tr');
  const td = el('td', 'empty', t('加载失败'));
  td.colSpan = 3;
  tr.append(td);
  $('sec-provs').replaceChildren(tr);
});
