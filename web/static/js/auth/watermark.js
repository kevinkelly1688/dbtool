/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: watermark.js
 * @time: 2026/10/28
 */
// 页面水印：系统设置开启“页面水印”时，在整页盖一层淡色重复文字，用于截图 / 拍屏外泄时追溯来源。
// 内容与样式由超级用户在“登录与安全设置”里定义（内容模板、附加文字、透明度、字号、倾斜角度、疏密、颜色），
// 服务端 /api/v1/auth/ui 已把模板里的 {账号} {姓名} {部门} {IP} 按当前用户展开，{日期} {时间} 在这里每分钟刷新。
// 水印层不接收鼠标事件，不影响页面操作；被删除 / 隐藏 / 改样式会立刻恢复，并向服务端上报一次（审计“水印篡改”，见 reportTamper）。
// 设置页保存 / 预览时通过下面导出的 reload() / preview() 立即生效，不用刷新页面。
import { api } from '../common.js';

const ID = 'dbt-watermark';
// 疏密 → [横向留白, 纵向留白]（px，加在旋转后文字外框之外，保证相邻水印不重叠）
const GAPS = { tight: [36, 22], normal: [90, 60], loose: [160, 110] };
const COLORS = { gray: '#7d8799', red: '#d93025', blue: '#1a6fe0' };
const DEF = { opacity: 14, font_size: 14, angle: -22, gap: 'normal', color: 'auto' };

let cfg = null; // 已保存的配置（来自 /api/v1/auth/ui），关闭水印时为 null
let layer = null;
let observer = null;
let timer = null;
let previewTimer = null;
let previewing = false;
let tamperTimer = null; // 篡改上报的防抖定时器
let tamperKind = null; // 等待上报的类型
let lastReport = 0; // 上次上报的时间（ms）
const REPORT_GAP = 30 * 1000; // 两次上报至少间隔（服务端另有“每用户每分钟 3 次”的限流）

const pad = (n) => String(n).padStart(2, '0');

function stamps() {
  const d = new Date();
  const date = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  return { 日期: date, 时间: `${date} ${pad(d.getHours())}:${pad(d.getMinutes())}` };
}

// 把模板里的 {变量} 换成值；不认识的变量原样保留
export function fillTemplate(tpl, vars) {
  const v = { ...stamps(), ...(vars || {}) };
  return String(tpl || '').replace(/\{([^{}]*)\}/g, (m, k) => (Object.prototype.hasOwnProperty.call(v, k) ? v[k] : m));
}

const xmlEsc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// 估算一行文字的宽度（中文按 1 个字号宽，其余按 0.58）
function textWidth(s, size) {
  let w = 0;
  for (const ch of s) w += ch.charCodeAt(0) > 255 ? 1 : 0.58;
  return w * size;
}

function fillColor(color) {
  if (COLORS[color]) return COLORS[color];
  // auto：跟随皮肤的前景色（深色皮肤浅字，明亮 / 护眼皮肤深字）
  return getComputedStyle(document.documentElement).getPropertyValue('--dt-fg').trim() || '#808080';
}

// 生成平铺用的 SVG 瓦片：瓦片尺寸 = 旋转后的文字外框 + 留白，文字不会被切掉。
// c: {lines: [已替换好的文字行], opacity, font_size, angle, gap, color}；返回 {url, w, h}，没有文字时返回 null
export function buildTile(c) {
  const lines = (c.lines || []).filter(Boolean);
  if (!lines.length) return null;
  const size = Number(c.font_size) || DEF.font_size;
  const angle = Number.isFinite(Number(c.angle)) ? Number(c.angle) : DEF.angle;
  const opacity = (Number(c.opacity) || DEF.opacity) / 100;
  const lh = size * 1.5;
  const rad = (angle * Math.PI) / 180;
  const cs = Math.abs(Math.cos(rad));
  const sn = Math.abs(Math.sin(rad));
  const tw = Math.max(...lines.map((s) => textWidth(s, size)));
  const th = lh * lines.length;
  const [gx, gy] = GAPS[c.gap] || GAPS.normal;
  const w = Math.ceil(tw * cs + th * sn + gx);
  const h = Math.ceil(tw * sn + th * cs + gy);
  const cx = w / 2;
  const cy = h / 2;
  const text = lines.map((s, i) => `<text x="${cx}" y="${(cy - th / 2 + lh * (i + 0.5)).toFixed(1)}" text-anchor="middle" dominant-baseline="central">${xmlEsc(s)}</text>`).join('');
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${w}" height="${h}"><g transform="rotate(${angle} ${cx} ${cy})" font-size="${size}" font-family="'Microsoft YaHei','PingFang SC',sans-serif" fill="${fillColor(c.color)}" fill-opacity="${opacity}">${text}</g></svg>`;
  return { url: `url("data:image/svg+xml;utf8,${encodeURIComponent(svg)}")`, w, h };
}

// 由服务端配置（模板已展开用户变量）生成当前时刻的瓦片
function tileOf(conf, vars) {
  return buildTile({
    lines: [fillTemplate(conf.watermark_text, vars), conf.watermark_extra ? fillTemplate(conf.watermark_extra, vars) : ''],
    opacity: conf.watermark_opacity, font_size: conf.watermark_font_size, angle: conf.watermark_angle, gap: conf.watermark_gap, color: conf.watermark_color,
  });
}

function styleLayer(d, t) {
  const s = d.style;
  s.position = 'fixed';
  s.inset = '0';
  s.pointerEvents = 'none';
  s.zIndex = '9999';
  s.backgroundRepeat = 'repeat';
  s.backgroundSize = `${t.w}px ${t.h}px`;
  s.backgroundImage = t.url;
  s.display = 'block';
  s.visibility = 'visible';
}

// 判断水印层现在的状态：返回 'removed'（被删除）/ 'hidden'（被隐藏）/ 'style'（被改了样式或属性）；一切正常返回 ''。
// expected 是我们自己设置时记下的 style 属性值。
export function classifyTamper(l, expected) {
  if (!l || !l.isConnected) return 'removed';
  const st = l.style || {};
  if (l.hidden || st.display === 'none' || st.visibility === 'hidden' || st.opacity === '0') return 'hidden';
  if (l.getAttribute('style') !== expected) return 'style';
  return '';
}

// 样式表规则把水印层藏起来（display:none / visibility:hidden / opacity:0）：行内样式没变，所以靠计算样式发现。
function hiddenByCss(l) {
  try {
    const cs = getComputedStyle(l);
    return cs.display === 'none' || cs.visibility === 'hidden' || parseFloat(cs.opacity) === 0;
  } catch (_) {
    return false;
  }
}

// 上报水印被篡改。防抖：同一时间窗里只发一次（取最先发现的类型），两次上报至少隔 REPORT_GAP；失败静默（不能影响页面）。
// 服务端只认 kind 这个枚举，用户与 IP 来自登录会话。
export function reportTamper(kind) {
  if (kind !== 'removed' && kind !== 'hidden' && kind !== 'style') return;
  if (!tamperKind) tamperKind = kind;
  if (tamperTimer) return;
  const wait = Math.max(800, REPORT_GAP - (Date.now() - lastReport));
  tamperTimer = setTimeout(async () => {
    const k = tamperKind;
    tamperTimer = null;
    tamperKind = null;
    lastReport = Date.now();
    try {
      await api('POST', '/api/v1/auth/watermark-tamper', { kind: k });
    } catch (_) { /* 未登录 / 被限流 / 网络问题：忽略 */ }
  }, wait);
}

function removeLayer() {
  if (observer) observer.disconnect();
  if (layer) layer.remove();
  layer = null;
}

// 重建 / 刷新时先停掉监听，免得自己的修改触发自己
function render(rebuild = false) {
  const conf = previewing ? previewing : cfg;
  const t = conf && (previewing ? buildTile(previewing) : tileOf(conf));
  if (!t) { removeLayer(); return; }
  if (observer) observer.disconnect();
  if (rebuild || !layer || !layer.isConnected) {
    if (layer) layer.remove();
    layer = document.createElement('div');
    layer.id = ID;
    layer.setAttribute('aria-hidden', 'true');
    document.body.append(layer);
  }
  styleLayer(layer, t);
  watch();
}

// 防删：水印层被移除或样式 / 属性被改动时立即重建
function watch() {
  if (!observer) {
    observer = new MutationObserver(() => {
      const kind = classifyTamper(layer, layer && layer.dataset.style);
      if (kind) {
        reportTamper(kind);
        render(true);
      }
    });
  }
  layer.dataset.style = layer.getAttribute('style') || '';
  observer.observe(document.body, { childList: true });
  observer.observe(layer, { attributes: true });
}

function start() {
  if (timer) clearInterval(timer);
  timer = setInterval(() => { // 每分钟刷新水印里的时间；顺便查一下有没有被样式表规则藏起来
    if (!cfg || previewing) return;
    if (layer && layer.isConnected && hiddenByCss(layer)) reportTamper('hidden');
    render();
  }, 60 * 1000);
}

// 取服务端配置并按它重画；保存设置后调用，立即生效。返回当前配置（关闭时为 null）。
export async function reload() {
  let ui;
  try {
    ui = await api('GET', '/api/v1/auth/ui');
  } catch (_) {
    return cfg; // 未登录或接口不可用时保持现状，不打扰页面
  }
  cfg = ui && ui.watermark ? ui : null;
  if (cfg) cfg = { watermark_opacity: DEF.opacity, watermark_font_size: DEF.font_size, watermark_angle: DEF.angle, watermark_gap: DEF.gap, watermark_color: DEF.color, ...cfg };
  if (!previewing) render(true);
  return cfg;
}

// 全页预览：按表单里还没保存的配置临时显示 ms 毫秒。conf: {lines, opacity, font_size, angle, gap, color}
export function preview(conf, ms = 5000) {
  previewing = conf;
  render(true);
  clearTimeout(previewTimer);
  previewTimer = setTimeout(() => { previewing = false; render(true); }, ms);
}

// 皮肤切换后文字颜色（auto）要跟着变
document.addEventListener('skinchange', () => render());

reload().then(start);
