// @author: kevinkelly
// @license: Apache Licence
// @file: notify_template.go
// @time: 2026/10/28

package store

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"dbtool/internal/model"
	"dbtool/internal/notify"
)

// 消息模板的存取（deploy/sql/260_notify_template.sql）。表不存在时 ActiveTemplates 返回空（通知按系统默认文案发送），
// 管理接口返回“未就绪”。

var (
	_ notify.TemplateSource = (*Store)(nil)
	_ notify.TemplateAdmin  = (*Store)(nil)
)

const tplHistKeep = 30 // 每个模板保留的历史条数

// TemplateTableReady 判断 dbt_notify_template 是否已创建。
func (s *Store) TemplateTableReady() bool {
	return s.hasTable("dbt_notify_template", &model.NotifyTemplate{})
}

func tplOf(r model.NotifyTemplate) notify.Template {
	blocks, err := notify.UnmarshalBlocks(r.Blocks)
	if err != nil {
		slog.Warn("消息模板的内容块无法解析，按空模板处理", "event", r.EventKey, "scope", r.ScopeKey, "err", err)
		blocks = []notify.Block{}
	}
	return notify.Template{EventKey: r.EventKey, Scope: r.ScopeKey, Name: r.Name, Enabled: r.Enabled, Title: r.Title, Tone: r.Tone,
		Icon: r.Icon, Blocks: blocks, UpdatedBy: r.UpdatedBy, UpdatedAt: r.UpdatedAt.Format("2006-01-02 15:04:05")}
}

// ActiveTemplates 实现 notify.TemplateSource：所有启用的模板。
func (s *Store) ActiveTemplates() ([]notify.Template, error) {
	if !s.TemplateTableReady() {
		return nil, nil
	}
	var rows []model.NotifyTemplate
	if err := s.DB.Where("enabled = ?", true).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]notify.Template, 0, len(rows))
	for _, r := range rows {
		out = append(out, tplOf(r))
	}
	return out, nil
}

// ListTemplates 返回全部模板（含停用的），按消息键、范围排序。
func (s *Store) ListTemplates() ([]notify.Template, error) {
	if !s.TemplateTableReady() {
		return nil, nil
	}
	var rows []model.NotifyTemplate
	if err := s.DB.Order("event_key, scope_key").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]notify.Template, 0, len(rows))
	for _, r := range rows {
		out = append(out, tplOf(r))
	}
	return out, nil
}

// GetTemplate 取一条模板；不存在返回 nil, nil。
func (s *Store) GetTemplate(eventKey, scope string) (*notify.Template, error) {
	if !s.TemplateTableReady() {
		return nil, nil
	}
	var r model.NotifyTemplate
	err := s.DB.Where("event_key = ? AND scope_key = ?", eventKey, scope).Take(&r).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := tplOf(r)
	return &t, nil
}

func snapshotOf(t notify.Template) string {
	t.UpdatedBy, t.UpdatedAt = "", ""
	b, _ := json.Marshal(t)
	return string(b)
}

// SaveTemplate 新建或覆盖一条模板，并记一条历史（同一事务）。
func (s *Store) SaveTemplate(t notify.Template, operator, note string) error {
	blocks, err := notify.MarshalBlocks(t.Blocks)
	if err != nil {
		return err
	}
	now := time.Now()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var cur model.NotifyTemplate
		err := tx.Where("event_key = ? AND scope_key = ?", t.EventKey, t.Scope).Take(&cur).Error
		switch {
		case err == gorm.ErrRecordNotFound:
			cur = model.NotifyTemplate{EventKey: t.EventKey, ScopeKey: t.Scope, CreatedAt: now}
		case err != nil:
			return err
		}
		cur.Name, cur.Enabled, cur.Title, cur.Tone, cur.Icon = t.Name, t.Enabled, t.Title, t.Tone, t.Icon
		cur.Blocks, cur.UpdatedBy, cur.UpdatedAt = string(blocks), cutRunes(operator, 150), now
		if err := tx.Save(&cur).Error; err != nil {
			return err
		}
		return s.addTemplateHist(tx, t.EventKey, t.Scope, "save", snapshotOf(t), note, operator, now)
	})
}

// DeleteTemplate 删除一条模板，并记一条历史（带删除前的内容）。
func (s *Store) DeleteTemplate(eventKey, scope, operator, note string) error {
	now := time.Now()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var cur model.NotifyTemplate
		err := tx.Where("event_key = ? AND scope_key = ?", eventKey, scope).Take(&cur).Error
		if err == gorm.ErrRecordNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.Delete(&cur).Error; err != nil {
			return err
		}
		return s.addTemplateHist(tx, eventKey, scope, "delete", snapshotOf(tplOf(cur)), note, operator, now)
	})
}

func (s *Store) addTemplateHist(tx *gorm.DB, event, scope, action, snapshot, note, operator string, at time.Time) error {
	h := model.NotifyTemplateHist{EventKey: event, ScopeKey: scope, Action: action, Snapshot: snapshot,
		Note: cutRunes(strings.TrimSpace(note), 200), Operator: cutRunes(operator, 150), CreatedAt: at}
	if err := tx.Create(&h).Error; err != nil {
		return err
	}
	// 只留最近 tplHistKeep 条
	var ids []uint
	if err := tx.Model(&model.NotifyTemplateHist{}).Where("event_key = ? AND scope_key = ?", event, scope).
		Order("id DESC").Offset(tplHistKeep).Pluck("id", &ids).Error; err != nil {
		return nil // 清理失败不影响保存
	}
	if len(ids) > 0 {
		_ = tx.Where("id IN ?", ids).Delete(&model.NotifyTemplateHist{}).Error
	}
	return nil
}

// TemplateHistory 返回一个模板最近的修改历史（新的在前）。
func (s *Store) TemplateHistory(eventKey, scope string, limit int) ([]notify.TemplateHist, error) {
	if !s.TemplateTableReady() {
		return nil, nil
	}
	if limit <= 0 || limit > tplHistKeep {
		limit = tplHistKeep
	}
	var rows []model.NotifyTemplateHist
	db := s.DB.Session(&gorm.Session{Logger: s.DB.Logger.LogMode(logger.Silent)})
	if err := db.Where("event_key = ? AND scope_key = ?", eventKey, scope).Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]notify.TemplateHist, 0, len(rows))
	for _, r := range rows {
		h := notify.TemplateHist{ID: r.ID, Action: r.Action, Note: r.Note, Operator: r.Operator, At: r.CreatedAt.Format("2006-01-02 15:04:05")}
		if r.Snapshot != "" {
			var t notify.Template
			if json.Unmarshal([]byte(r.Snapshot), &t) == nil {
				h.Snapshot = &t
			}
		}
		out = append(out, h)
	}
	return out, nil
}
