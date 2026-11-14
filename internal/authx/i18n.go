// @author: kevinkelly
// @license: Apache Licence
// @file: i18n.go
// @time: 2026/10/28

package authx

import (
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
)

// 本文件是 authx 的界面多语言辅助。
//
// Service 层不知道请求语言，产出的提示都是中文原文（也会存进库、写进审计）；在 HTTP 边缘（本包的 handler）按请求语言翻译：
// 4xx / 5xx 的 JSON message 由 server 在边缘统一按目录翻译，这里补的是它翻译不到的几类内容——
// 成功响应里带出的文案（登录方式的“最近测试结果”、账号的认证方式）、纯文本响应、以及“缺少必填项：A、B”这种清单。

// reqLang 取请求语言（server 的语言中间件已把它放进 request.Context）。
func reqLang(c *gin.Context) string { return i18n.FromContext(c.Request.Context()) }

// trc 按请求语言翻译一条中文原文（带 {名字} 占位时成对传参）。
func trc(c *gin.Context, key string, kv ...any) string { return i18n.T(reqLang(c), key, kv...) }

// requiredPrefix 是 requireFields 拼出的清单提示的开头：缺少必填项：A、B。
const requiredPrefix = "缺少必填项：" // i18n-ignore 与 flow.go 里的 "缺少必填项：%s" 配套，清单逐项翻译

// localizeMsg 把业务层的中文提示翻译成 lang：整句精确匹配 / 带 %s %d 的格式串匹配（i18n.TP）；
// “缺少必填项：A、B”的清单先逐项翻译（TP 只能整串匹配），再整体匹配。
func localizeMsg(lang, msg string) string {
	if lang == i18n.ZH || msg == "" {
		return msg
	}
	if i := strings.Index(msg, requiredPrefix); i >= 0 {
		names := strings.Split(msg[i+len(requiredPrefix):], "、")
		for j := range names {
			names[j] = i18n.TP(lang, names[j])
		}
		msg = msg[:i] + requiredPrefix + strings.Join(names, ", ")
	}
	return i18n.TP(lang, msg)
}

// reportSyms 是测试报告每一行的状态符号前缀（见 TestReport.add）。
var reportSyms = []string{"✔ ", "✖ ", "ℹ ", "⚠ "}

// localizeLine 翻译测试报告的一行：状态符号保留，其余按 localizeMsg；“（空）”是 describeIdentity 拼进属性列表里的占位。
func localizeLine(lang, line string) string {
	if lang == i18n.ZH {
		return line
	}
	sym := ""
	for _, s := range reportSyms {
		if rest, ok := strings.CutPrefix(line, s); ok {
			sym, line = s, rest
			break
		}
	}
	out := localizeMsg(lang, line)
	if strings.Contains(out, "（空）") {
		out = strings.ReplaceAll(out, "（空）", i18n.T(lang, "（空）"))
	}
	return sym + out
}

// localizeTestMsg 翻译多行的测试结果文字（库里存的是中文原文，多行用换行分隔）。
func localizeTestMsg(lang, msg string) string {
	if lang == i18n.ZH || msg == "" {
		return msg
	}
	lines := strings.Split(msg, "\n")
	for i, l := range lines {
		lines[i] = localizeLine(lang, l)
	}
	return strings.Join(lines, "\n")
}

// localizedReport 返回行已翻译的测试报告副本（不改动原报告，原报告已写入库）。
func localizedReport(lang string, rep *TestReport) *TestReport {
	if rep == nil || lang == i18n.ZH {
		return rep
	}
	out := &TestReport{OK: rep.OK, Lines: make([]string, len(rep.Lines))}
	for i, l := range rep.Lines {
		out.Lines[i] = localizeLine(lang, l)
	}
	return out
}

// localizeView 翻译登录方式视图里随接口带出的“最近测试结果”（类型名 / 状态由前端按目录翻译）。
func localizeView(c *gin.Context, v *ProviderView) {
	v.TestMsg = localizeTestMsg(reqLang(c), v.TestMsg)
}

// localizeAccount 翻译账号的认证方式说明；外部认证时把已绑定的登录方式名用该语言的分隔符拼起来。
func localizeAccount(lang string, info *AccountInfo) {
	if lang == i18n.ZH || info == nil {
		return
	}
	if info.AuthMethod == "external" && len(info.Bound) > 0 {
		info.AuthMethodText = i18n.T(lang, "外部认证（{names}）", "names", strings.Join(info.Bound, ", "))
		return
	}
	info.AuthMethodText = i18n.TP(lang, info.AuthMethodText)
}
