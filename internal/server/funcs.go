// @author: kevinkelly
// @license: Apache Licence
// @file: funcs.go
// @time: 2026/10/28

package server

import (
	"fmt"
	"html"
	"html/template"
	"strings"

	"dbtool/internal/i18n"
	"dbtool/internal/review"
)

// funcsFor 返回绑定了界面语言的模板函数：在 tplFuncs 之上覆盖 t / lang，以及需要翻译的辅助函数。
// 页面模板按语言各编译一份（见 loadTemplates），所以模板里写 {{t "查询权限"}} 就够了，range 里面也能用。
func funcsFor(lang string) template.FuncMap {
	m := make(template.FuncMap, len(tplFuncs)+4)
	for k, v := range tplFuncs {
		m[k] = v
	}
	m["t"] = func(key string, kv ...any) string { return i18n.T(lang, key, kv...) }
	m["lang"] = func() string { return lang }
	m["tp"] = func(s string) string { return i18n.TP(lang, s) }
	m["th"] = func(key string, kv ...any) template.HTML { return transHTML(lang, key, kv...) }
	m["privType"] = func(t int) string {
		if t == 1 {
			return i18n.T(lang, "库权限")
		}
		return i18n.T(lang, "表权限")
	}
	return m
}

// htmlTags 是 th 允许保留的行内标签（无属性）；译文里其余的 < > & 一律转义。
var htmlTags = strings.NewReplacer(
	"&lt;br&gt;", "<br>", "&lt;br/&gt;", "<br>",
	"&lt;em&gt;", "<em>", "&lt;/em&gt;", "</em>",
	"&lt;b&gt;", "<b>", "&lt;/b&gt;", "</b>",
	"&lt;strong&gt;", "<strong>", "&lt;/strong&gt;", "</strong>",
	"&lt;code&gt;", "<code>", "&lt;/code&gt;", "</code>",
	"&lt;small&gt;", "<small>", "&lt;/small&gt;", "</small>",
	"&lt;kbd&gt;", "<kbd>", "&lt;/kbd&gt;", "</kbd>",
)

// transHTML 翻译带行内标签的整句：译文先整体转义，再还原 htmlTags 里的标签；参数值默认转义，
// 传 template.HTML 的（如一个链接）原样保留。这样含 <b>/<em>/<br> 的句子可以整句翻译，不必拆成碎片。
func transHTML(lang, key string, kv ...any) template.HTML {
	s := htmlTags.Replace(html.EscapeString(i18n.T(lang, key)))
	args := make([]any, len(kv))
	for i, v := range kv {
		args[i] = v
		if i%2 == 1 {
			if h, ok := v.(template.HTML); ok {
				args[i] = string(h)
			} else {
				args[i] = html.EscapeString(fmt.Sprint(v))
			}
		}
	}
	return template.HTML(i18n.Fill(s, args...)) // #nosec G203 -- 译文与参数均已转义，仅放行白名单标签
}

// tplFuncs 是模板里可用的辅助函数。其中 t 是恒等版本（中文原样输出），仅供没有语言的场合（测试）解析模板用；
// 真正渲染用 funcsFor(lang)。
var tplFuncs = template.FuncMap{
	"t":    func(key string, kv ...any) string { return i18n.T(i18n.ZH, key, kv...) },
	"th":   func(key string, kv ...any) template.HTML { return transHTML(i18n.ZH, key, kv...) },
	"tp":   func(s string) string { return s },
	"lang": func() string { return i18n.ZH },
	// statusClass 把审批状态映射成样式类名。
	"statusClass": func(s int) string {
		switch s {
		case 0:
			return "waiting"
		case 1:
			return "passed"
		case 2:
			return "rejected"
		default:
			return "aborted"
		}
	},
	"sqlStatusClass": sqlStatusClass,
	// initial 取字符串的第一个字符（按 rune，中文名不会被截坏），用作头像文字；空串返回 "?"。
	"initial": func(ss ...string) string {
		for _, x := range ss {
			if r := []rune(strings.TrimSpace(x)); len(r) > 0 {
				return strings.ToUpper(string(r[0]))
			}
		}
		return "?"
	},
	// lvl 把检查/执行结果的错误级别映射成样式类名。
	"lvl": func(l review.Flex) string {
		switch l {
		case review.LevelWarning:
			return "lv-warn"
		case review.LevelError:
			return "lv-err"
		}
		return ""
	},
	// can 判断按钮权限：{{if can . "sqlworkflow.approve"}}（. 为页面数据；等价于 .Btn.Can）。
	"can": func(d pageData, code string) bool { return d.Btn.Can(code) },
	"privType": func(t int) string {
		if t == 1 {
			return "库权限"
		}
		return "表权限"
	},
}
