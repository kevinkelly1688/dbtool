// @author: kevinkelly
// @license: Apache Licence
// @file: localize.go
// @time: 2026/10/28

package sysadmin

import (
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
	"dbtool/internal/query"
)

// 界面多语言。4xx / 5xx 的错误消息由 HTTP 边缘统一翻译（含带 %s %d 的格式串），本包不用管；
// 这里处理边缘够不着的两类：成功响应里系统生成的文案（审批流告警、认证方式名等），以及“用顿号拼成清单”的错误消息。
// 中文请求原样返回，不改任何字段；资源组 / 权限组 / 用户名等用户数据一律不翻译（TP 只做精确匹配与格式串匹配）。

// reqLang 返回本次请求的界面语言（语言中间件放进 request context）。
func reqLang(c *gin.Context) string { return i18n.FromContext(c.Request.Context()) }

// tpJoined 翻译用 “；” 拼起来的多段系统文案：整串能匹配就整串翻译，否则逐段翻译后用英文分号拼回。
func tpJoined(lang, s string) string {
	if lang == i18n.ZH || s == "" {
		return s
	}
	if out := i18n.TP(lang, s); out != s {
		return out
	}
	parts := strings.Split(s, "；")
	if len(parts) < 2 {
		return s
	}
	for i, p := range parts {
		parts[i] = i18n.TP(lang, p)
	}
	return strings.Join(parts, i18n.T(lang, "；|sep"))
}

// localizeFlow 翻译审批流视图里系统生成的文案：类型名、流程文本、各级告警。
func localizeFlow(lang string, v *FlowView) {
	if v == nil || lang == i18n.ZH {
		return
	}
	v.TypeLabel = i18n.TP(lang, v.TypeLabel)
	v.Text = i18n.TP(lang, v.Text)
	for i := range v.Levels {
		v.Levels[i].Warning = i18n.TP(lang, v.Levels[i].Warning)
	}
	for i := range v.Warnings {
		v.Warnings[i] = i18n.TP(lang, v.Warnings[i])
	}
}

// localizeOverview 翻译总览矩阵里的文案。Types 是包级共享切片，先复制再改，不能动原值。
func localizeOverview(lang string, o *FlowOverview) {
	if o == nil || lang == i18n.ZH {
		return
	}
	types := make([]WorkflowTypeInfo, len(o.Types))
	copy(types, o.Types)
	for i := range types {
		types[i].Label = i18n.TP(lang, types[i].Label)
	}
	o.Types = types
	for i := range o.Matrix {
		for j := range o.Matrix[i].Cells {
			c := &o.Matrix[i].Cells[j]
			c.Text = i18n.TP(lang, c.Text)
			c.WarnText = tpJoined(lang, c.WarnText)
		}
	}
}

// localizeSaveResult 翻译保存审批流的结果。
func localizeSaveResult(lang string, r *SaveFlowResult) {
	if r == nil || lang == i18n.ZH {
		return
	}
	r.Before, r.After = i18n.TP(lang, r.Before), i18n.TP(lang, r.After)
	localizeFlow(lang, r.Flow)
}

// localizeResGroup 翻译资源组详情里各类工单审批流的说明。
func localizeResGroup(lang string, d *ResGroupDetail) {
	if d == nil || lang == i18n.ZH {
		return
	}
	for i := range d.Flows {
		d.Flows[i].Label = i18n.TP(lang, d.Flows[i].Label)
		d.Flows[i].Text = i18n.TP(lang, d.Flows[i].Text)
	}
}

// localizeFlowRefs 翻译权限组被引用的审批流里的工单类型名。
func localizeFlowRefs(lang string, refs []FlowRef) {
	if lang == i18n.ZH {
		return
	}
	for i := range refs {
		refs[i].TypeLabel = i18n.TP(lang, refs[i].TypeLabel)
	}
}

// authLabelIn 是认证方式列表在 lang 下的显示名，如 “Local + DingTalk”。
func authLabelIn(lang string, methods []string) string {
	labels := make([]string, len(methods))
	for i, m := range methods {
		labels[i] = i18n.TP(lang, AuthLabel(m))
	}
	return strings.Join(labels, " + ")
}

// localizeUsers 把用户列表里的认证方式显示名换成 lang 的写法。
func localizeUsers(lang string, rows []UserItem) {
	if lang == i18n.ZH {
		return
	}
	for i := range rows {
		rows[i].AuthLabel = authLabelIn(lang, rows[i].AuthMethods)
	}
}

// localizeErr 翻译边缘不能直接匹配的错误消息：由 Data 里的计数重新生成“引用清单”（顿号拼接的几段说明）。
// 返回副本，不改原错误。
func localizeErr(lang string, qe *query.Error) *query.Error {
	if lang == i18n.ZH || qe == nil {
		return qe
	}
	if m, ok := qe.Data["refs"].(map[string]int64); ok {
		r := ResGroupRefs{Users: m["users"], Instances: m["instances"], SQLWorkflows: m["sql_workflows"], AuditFlows: m["audit_flows"],
			PrivApplies: m["priv_applies"], PerfIssues: m["perf_issues"], NotifyChannels: m["notify_channels"]}
		if list := r.blocking(lang); len(list) > 0 {
			cp := *qe
			cp.Message = i18n.TP(lang, errf("资源组仍被引用，不能彻底删除：%s。有历史记录引用的资源组只能停用", strings.Join(list, i18n.T(lang, "、|sep"))))
			return &cp
		}
	}
	return qe
}
