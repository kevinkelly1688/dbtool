// @author: kevinkelly
// @license: Apache Licence
// @file: notify_template.go
// @time: 2026/10/28

package memrepo

import (
	"sort"
	"time"

	"dbtool/internal/notify"
)

var (
	_ notify.TemplateSource = (*Repo)(nil)
	_ notify.TemplateAdmin  = (*Repo)(nil)
)

func tplKey(event, scope string) string { return event + "|" + scope }

// TemplateTableReady 实现 notify.TemplateAdmin：内存仓库始终可用。
func (r *Repo) TemplateTableReady() bool { return true }

// ActiveTemplates 实现 notify.TemplateSource。
func (r *Repo) ActiveTemplates() ([]notify.Template, error) {
	var out []notify.Template
	for _, t := range r.templates {
		if t.Enabled {
			out = append(out, t.Clone())
		}
	}
	return out, nil
}

// ListTemplates 实现 notify.TemplateAdmin。
func (r *Repo) ListTemplates() ([]notify.Template, error) {
	out := make([]notify.Template, 0, len(r.templates))
	for _, t := range r.templates {
		out = append(out, t.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EventKey != out[j].EventKey {
			return out[i].EventKey < out[j].EventKey
		}
		return out[i].Scope < out[j].Scope
	})
	return out, nil
}

// GetTemplate 实现 notify.TemplateAdmin。
func (r *Repo) GetTemplate(event, scope string) (*notify.Template, error) {
	if t, ok := r.templates[tplKey(event, scope)]; ok {
		c := t.Clone()
		return &c, nil
	}
	return nil, nil
}

// SaveTemplate 实现 notify.TemplateAdmin。
func (r *Repo) SaveTemplate(t notify.Template, operator, note string) error {
	if r.templates == nil {
		r.templates = map[string]notify.Template{}
	}
	t.UpdatedBy, t.UpdatedAt = operator, time.Now().Format("2006-01-02 15:04:05")
	r.templates[tplKey(t.EventKey, t.Scope)] = t.Clone()
	r.addTplHist(t.EventKey, t.Scope, "save", &t, note, operator)
	return nil
}

// DeleteTemplate 实现 notify.TemplateAdmin。
func (r *Repo) DeleteTemplate(event, scope, operator, note string) error {
	old, ok := r.templates[tplKey(event, scope)]
	if !ok {
		return nil
	}
	delete(r.templates, tplKey(event, scope))
	r.addTplHist(event, scope, "delete", &old, note, operator)
	return nil
}

func (r *Repo) addTplHist(event, scope, action string, snap *notify.Template, note, operator string) {
	c := snap.Clone()
	r.tplHist = append(r.tplHist, tplHistRow{event: event, scope: scope, h: notify.TemplateHist{
		ID: uint(len(r.tplHist) + 1), Action: action, Note: note, Operator: operator, At: time.Now().Format("2006-01-02 15:04:05"), Snapshot: &c}})
}

// TemplateHistory 实现 notify.TemplateAdmin（新的在前）。
func (r *Repo) TemplateHistory(event, scope string, limit int) ([]notify.TemplateHist, error) {
	var out []notify.TemplateHist
	for i := len(r.tplHist) - 1; i >= 0; i-- {
		if x := r.tplHist[i]; x.event == event && x.scope == scope {
			out = append(out, x.h)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type tplHistRow struct {
	event, scope string
	h            notify.TemplateHist
}

// NotifyTemplates 返回当前保存的模板（测试用）。
func (r *Repo) NotifyTemplates() []notify.Template {
	out, _ := r.ListTemplates()
	return out
}
