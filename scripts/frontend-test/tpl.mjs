/**
 * @author: kevinkelly
 * @license: Apache Licence
 * @file: tpl.mjs
 * @time: 2026/10/28
 */
// 页面测试直接读 web/templates 里的原始模板（只去掉 define 包装），而模板里的文案已经包成了翻译调用：
//   {{t "原文"}} / {{th "原文"}} / {{t "共 {n} 条" "n" .N}}
// 这里把它们还原成中文原文，测试里的中文断言才继续成立。带 {name} 占位的：参数是 Go 侧传进来的 HTML
// （带 id 的元素，脚本会去刷新它）时，用 GO_HTML 里登记的初值代入；其余占位保持原样。
// 用法：const src = untranslate(fs.readFileSync(file, 'utf8'));

// 模板参数表达式 → Go 侧提供的 HTML 初值（见 internal/instmgr/http.go、internal/binlog/http.go）。
const GO_HTML = {
  '.Data.SelCount': '<b id="il-sel-n">0</b>',
  '.Data.HardRows': '<span id="bl-hard-rows">0</span>',
  '.Data.HardMB': '<span id="bl-hard-mb">0</span>',
  '.Data.MaxTimeout': '<span id="bl-max-timeout">0</span>',
};

function fill(text, args) {
  // args 形如：'"n" .Data.SelCount "m" .Data.X'；只处理 "名字" 表达式 成对出现的情形
  const re = /"([A-Za-z_]\w*)"\s+(\.[\w.]+)/g;
  let m;
  while ((m = re.exec(args))) {
    const html = GO_HTML[m[2]];
    if (html) text = text.split('{' + m[1] + '}').join(html);
  }
  return text;
}

export function untranslate(src) {
  const unq = (s) => s.replace(/\\"/g, '"').replace(/\\\\/g, '\\');
  return src
    .replace(/\{\{-?\s*th?\s+"((?:[^"\\]|\\.)*)"([^}]*)\}\}/g, (_, s, args) => fill(unq(s), args))
    .replace(/\{\{-?\s*th?\s+`([^`]*)`([^}]*)\}\}/g, (_, s, args) => fill(s, args));
}
