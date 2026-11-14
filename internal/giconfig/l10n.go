// @author: kevinkelly
// @license: Apache Licence
// @file: l10n.go
// @time: 2026/10/28

package giconfig

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
)

// 界面多语言：服务层只产出中文原文（审计、版本记录里存的就是它），接口响应里面向用户的提示在 HTTP 边缘
// 按请求语言翻译（中文请求原样返回）。goInception 返回的原文、配置文件内容、用户填写的说明不翻译。

func langOf(c *gin.Context) string { return i18n.FromContext(c.Request.Context()) }

func tp(lang, s string) string { return i18n.TP(lang, s) }

// tpLine 翻译带符号前缀的一行（“✘ ” / “✔ ” / “ℹ ”）：前缀原样保留，后面的整句按目录翻译。
func tpLine(lang, s string) string {
	if lang == i18n.ZH {
		return s
	}
	for _, p := range []string{"✘ ", "✔ ", "ℹ "} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return p + tp(lang, rest)
		}
	}
	return tp(lang, s)
}

func tpLines(lang string, ss []string) []string {
	if lang == i18n.ZH {
		return ss
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = tpLine(lang, s)
	}
	return out
}

func localizeReport(lang string, r *Report) {
	if lang == i18n.ZH {
		return
	}
	for i := range r.Issues {
		r.Issues[i].Message = tp(lang, r.Issues[i].Message)
	}
}

func localizeDrift(lang string, d *Drift) { d.Message = tp(lang, d.Message) }

func localizeForm(lang string, groups []FormGroup) {
	if lang == i18n.ZH {
		return
	}
	for gi := range groups {
		g := &groups[gi]
		g.Title = tp(lang, g.Title)
		for i := range g.Items {
			g.Items[i].Label = tp(lang, g.Items[i].Label)
		}
	}
}

func localizeRestartRecord(lang string, r *RestartRecord) {
	if r != nil {
		r.Message = tp(lang, r.Message)
	}
}

func localizeJob(lang string, j *JobView) {
	if j == nil || lang == i18n.ZH {
		return
	}
	j.Message = tp(lang, j.Message)
	for i := range j.Steps {
		j.Steps[i].Name = tp(lang, j.Steps[i].Name)
		j.Steps[i].Detail = tp(lang, j.Steps[i].Detail)
	}
}

// localizeOverview 翻译概览里面向用户的文案。版本列表的 Label 不在这里翻译：页面要拿它与原文比较来决定样式与按钮，
// 显示时由前端用 tp() 翻译；变更摘要与失败原因是存库的历史文本，只做展示时的翻译（匹配得上才换）。
func localizeOverview(lang string, o *Overview) {
	if lang == i18n.ZH {
		return
	}
	localizeForm(lang, o.Catalog)
	localizeDrift(lang, &o.File.Drift)
	o.File.ReadError = tp(lang, o.File.ReadError)
	o.File.EditorFrom = tp(lang, o.File.EditorFrom)
	o.Deploy.PreviewError = tp(lang, o.Deploy.PreviewError)
	for i := range o.Versions {
		o.Versions[i].ChangeSummary = tp(lang, o.Versions[i].ChangeSummary)
		o.Versions[i].Error = tp(lang, o.Versions[i].Error)
	}
	if o.Effective != nil {
		o.Effective.ChangeSummary = tp(lang, o.Effective.ChangeSummary)
	}
	if o.Pending != nil {
		o.Pending.ChangeSummary = tp(lang, o.Pending.ChangeSummary)
	}
	localizeRestartRecord(lang, o.LastResult)
	localizeJob(lang, o.Job)
}

func localizeStatus(lang string, st *Status) {
	if lang == i18n.ZH {
		return
	}
	st.StateLabel = tp(lang, st.StateLabel)
	st.Message = tp(lang, st.Message)
	st.TaskNote = tp(lang, st.TaskNote)
	localizeDrift(lang, &st.Drift)
	localizeRestartRecord(lang, st.LastResult)
	localizeJob(lang, st.Job)
}

// wrapped 是带原因的错误：文字与 fmt.Errorf("…%w") 一致，保留 Unwrap；
// 格式串用 %s 写，是因为 %w 不在按目录翻译时匹配的动词里。
type wrapped struct {
	msg string
	err error
}

func (e *wrapped) Error() string { return e.msg }
func (e *wrapped) Unwrap() error { return e.err }

// wrapf(err, "命令执行失败：%s")：最后一个 %s 对应 err，前面的参数照常放在 args 里。
func wrapf(err error, format string, args ...any) error {
	return &wrapped{msg: fmt.Sprintf(format, append(args, err)...), err: err}
}

func localizeMeta(lang string, m *VersionMeta) {
	if m == nil || lang == i18n.ZH {
		return
	}
	// Label 保持原文：页面要拿它与原文比较（见 localizeOverview）；摘要与失败原因是存库的历史文本，匹配得上才换。
	m.ChangeSummary = tp(lang, m.ChangeSummary)
	m.Error = tp(lang, m.Error)
}

func localizeVersion(lang string, v *VersionDetail) { localizeMeta(lang, &v.VersionMeta) }

func localizeDiff(lang string, d *DiffResult) {
	if lang == i18n.ZH {
		return
	}
	for i := range d.Lines {
		if d.Lines[i].Op == "~" { // “… 省略 N 行未变内容 …”；其余行是配置文件原文
			d.Lines[i].Text = tp(lang, d.Lines[i].Text)
		}
	}
}

func localizeDiffResponse(lang string, d *DiffResponse) {
	localizeMeta(lang, d.From)
	localizeMeta(lang, &d.To)
	localizeDiff(lang, &d.Diff)
}

func localizeSave(lang string, r *SaveResult) {
	if lang == i18n.ZH {
		return
	}
	r.Message = tp(lang, r.Message)
	for i := range r.Warnings {
		r.Warnings[i].Message = tp(lang, r.Warnings[i].Message)
	}
}

// localizeData 翻译错误响应里附带的结构化数据（校验报告、任务列表）。
func localizeData(lang string, data any) any {
	if lang == i18n.ZH {
		return data
	}
	switch d := data.(type) {
	case Report:
		localizeReport(lang, &d)
		return d
	case Tasks:
		d.Note = tp(lang, d.Note)
		return d
	}
	return data
}
