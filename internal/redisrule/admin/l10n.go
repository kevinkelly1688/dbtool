// @author: kevinkelly
// @license: Apache Licence
// @file: l10n.go
// @time: 2026/10/28

package admin

import (
	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
	"dbtool/internal/redisrule"
)

// 界面多语言：规则引擎只产出中文原文（审计、工单检查结果里存的就是它），接口响应里面向用户的提示在这里
// 按请求语言翻译（中文请求原样返回）。规则名、说明、备注等用户数据不翻译。

func langOf(c *gin.Context) string { return i18n.FromContext(c.Request.Context()) }

// 试检查结果的状态按级别映射，避免依赖中文原文比较；“通过 / 警告”带上下文，与审批流里的同名词区分。
func statusLabel(lang string, level int, fallback string) string {
	switch level {
	case redisrule.LevelOK:
		return i18n.T(lang, "通过|check")
	case redisrule.LevelWarn:
		return i18n.T(lang, "警告|check")
	case redisrule.LevelError:
		return i18n.T(lang, "拒绝")
	}
	return fallback
}

func localizeTemplates(lang string, list []redisrule.Template) {
	if lang == i18n.ZH {
		return
	}
	for i := range list {
		t := &list[i]
		t.Title = i18n.TP(lang, t.Title)
		t.Description = i18n.TP(lang, t.Description)
		t.Rule.Name = i18n.TP(lang, t.Rule.Name)
		t.Rule.Description = i18n.TP(lang, t.Rule.Description)
	}
}

func localizeVerdicts(lang string, list []redisrule.LineVerdict) {
	if lang == i18n.ZH {
		return
	}
	for i := range list {
		v := &list[i]
		v.Status = statusLabel(lang, v.Level, v.Status)
		v.Message = i18n.TP(lang, v.Message)
	}
}
