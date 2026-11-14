// @author: kevinkelly
// @license: Apache Licence
// @file: l10n.go
// @time: 2026/10/28

package firewalladmin

import (
	"github.com/gin-gonic/gin"

	"dbtool/internal/firewall"
	"dbtool/internal/i18n"
)

// 界面多语言：服务层只产出中文原文（审计、命中记录、测试里的断言都依赖它），接口响应里面向用户的提示在这里
// 按请求语言翻译（中文请求原样返回）。带参数的整句靠 i18n.TP 按格式串匹配，所以文案要用整句 + %s / %d 写成。
// 用户数据（规则名、说明、提示文案、实例名等）与存库的审计文本不翻译；状态枚举值（effective 等）本来就不是文案。

// langOf 返回本次请求的界面语言。
func langOf(c *gin.Context) string { return i18n.FromContext(c.Request.Context()) }

func tp(lang, s string) string { return i18n.TP(lang, s) }

func tpAll(lang string, ss []string) []string {
	if lang == i18n.ZH {
		return ss
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = tp(lang, s)
	}
	return out
}

func tpSkips(lang string, in []firewall.Skip) []firewall.Skip {
	if lang == i18n.ZH {
		return in
	}
	out := make([]firewall.Skip, len(in))
	for i, s := range in {
		s.Reason = tp(lang, s.Reason)
		out[i] = s
	}
	return out
}

func localizeMeta(lang string, m *Meta) {
	for i := range m.Scenes {
		m.Scenes[i].Label = tp(lang, m.Scenes[i].Label)
		m.Scenes[i].Note = tp(lang, m.Scenes[i].Note)
	}
	for i := range m.Features {
		m.Features[i].Label = tp(lang, m.Features[i].Label)
	}
}

func localizeRule(lang string, v *RuleView) { v.Invalid = tp(lang, v.Invalid) }

func localizeRuleList(lang string, l *RuleList) {
	for i := range l.Rules {
		localizeRule(lang, &l.Rules[i])
	}
}

func localizeMode(lang string, r *ModeResult) {
	localizeRule(lang, &r.Rule)
	r.Warning = tp(lang, r.Warning)
}

func localizeHit(lang string, h *HitView) {
	h.Matched = tpAll(lang, h.Matched)
	h.Unknown = tpAll(lang, h.Unknown)
	h.Downgraded = tp(lang, h.Downgraded)
}

func localizeHits(lang string, l *HitList) {
	for i := range l.Items {
		localizeHit(lang, &l.Items[i])
	}
}

func localizeException(lang string, e *ExceptionView) {
	e.RuleName = tp(lang, e.RuleName) // “（规则已删除）”占位；真实规则名是用户数据，不在目录里
	e.SubjectText = tp(lang, e.SubjectText)
}

func localizeExceptions(lang string, l []ExceptionView) {
	for i := range l {
		localizeException(lang, &l[i])
	}
}

func localizeTemplates(lang string, l []TemplateView) {
	for i := range l {
		l[i].Category = tp(lang, l[i].Category)
		l[i].Name = tp(lang, l[i].Name)
		l[i].Description = tp(lang, l[i].Description)
	}
}

func localizeEval(lang string, r *EvalResult) {
	r.Message = tp(lang, r.Message)
	r.ReasonMsgs = tpAll(lang, r.ReasonMsgs)
	r.Warnings = tpAll(lang, r.Warnings)
	r.Notes = tpAll(lang, r.Notes)
	r.Skipped = tpSkips(lang, r.Skipped)
	for i := range r.Hits {
		r.Hits[i].Matched = tpAll(lang, r.Hits[i].Matched)
		r.Hits[i].Unknown = tpAll(lang, r.Hits[i].Unknown)
		r.Hits[i].Downgraded = tp(lang, r.Hits[i].Downgraded)
	}
}

func localizeReplay(lang string, r *ReplayResult) {
	r.Advice = tp(lang, r.Advice)
	r.Notes = tpAll(lang, r.Notes)
	for i := range r.Samples {
		r.Samples[i].Why = tpAll(lang, r.Samples[i].Why)
	}
}
