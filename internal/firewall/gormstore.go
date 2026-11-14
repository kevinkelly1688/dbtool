// @author: kevinkelly
// @license: Apache Licence
// @file: gormstore.go
// @time: 2026/10/28

package firewall

import (
	"errors"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"dbtool/internal/model"
)

// gormStore 是元数据库里的 Store 实现。
type gormStore struct{ db *gorm.DB }

// NewGormStore 创建元数据库存储。
func NewGormStore(db *gorm.DB) Store { return &gormStore{db: db} }

func noTable(err error) bool {
	var me *gomysql.MySQLError
	return errors.As(err, &me) && me.Number == 1146
}

func (s *gormStore) LoadRules() ([]Rule, error) {
	var rows []model.FirewallRule
	err := s.db.Where("enabled = 1").Order("priority, id").Limit(MaxRules).Find(&rows).Error
	if noTable(err) {
		return nil, nil // 还没执行 deploy/sql/240_firewall.sql：等同没有规则
	}
	if err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(rows))
	for _, m := range rows {
		out = append(out, fromModel(m))
	}
	return out, nil
}

// RuleFromModel 把库里的行转成领域对象（管理页用）。条件 JSON 解析失败时 Conditions 留空，Normalize 会把它当无效规则。
func RuleFromModel(m model.FirewallRule) Rule { return fromModel(m) }

// RuleToModel 把（已通过 Normalize 的）规则转成库里的行；ID / 创建时间 / 更新人由调用方填。
func RuleToModel(r Rule) model.FirewallRule {
	return model.FirewallRule{
		ID: r.ID, Name: r.Name, Description: r.Description, Priority: r.Priority, Enabled: r.Enabled, Mode: r.Mode,
		ObserveSince: r.ObserveSince, Scenes: strings.Join(r.Scenes, ","), Env: r.Env, InstanceIDs: joinUint(r.InstanceIDs),
		GroupMode: r.GroupMode, GroupIDs: joinUint(r.GroupIDs), MatchMode: r.Match, Conditions: ConditionsJSON(r.Conditions),
		Action: r.Action, ActionArg: r.ActionArg, Message: r.Message, Notify: r.Notify, UpdatedBy: r.UpdatedBy,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func joinUint(ids []uint) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(uint64(id), 10)
	}
	return strings.Join(parts, ",")
}

// fromModel 把库里的行转成领域对象。条件 JSON 解析失败时 Conditions 留空，Normalize 会把它当无效规则跳过。
func fromModel(m model.FirewallRule) Rule {
	conds, _ := ParseConditions(m.Conditions)
	return Rule{
		ID: m.ID, Name: m.Name, Description: m.Description, Priority: m.Priority, Enabled: m.Enabled, Mode: m.Mode,
		ObserveSince: m.ObserveSince, Scenes: splitCSV(m.Scenes), Env: m.Env, InstanceIDs: splitUintCSV(m.InstanceIDs),
		GroupMode: m.GroupMode, GroupIDs: splitUintCSV(m.GroupIDs), Match: m.MatchMode, Conditions: conds,
		Action: m.Action, ActionArg: m.ActionArg, Message: m.Message, Notify: m.Notify, UpdatedBy: m.UpdatedBy,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

func (s *gormStore) LoadExceptions(now time.Time) ([]Exception, error) {
	var rows []model.FirewallException
	err := s.db.Where("expires_at > ?", now).Order("id").Find(&rows).Error
	if noTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]Exception, 0, len(rows))
	for _, m := range rows {
		out = append(out, Exception{ID: m.ID, RuleID: m.RuleID, SubjectKind: m.SubjectKind, SubjectValue: m.SubjectValue,
			Reason: m.Reason, ExpiresAt: m.ExpiresAt, CreatedBy: m.CreatedBy})
	}
	return out, nil
}

func (s *gormStore) UserGroupIDs(userID uint) ([]uint, error) {
	var ids []uint
	err := s.db.Table("dbt_sql_users_groups").Where("users_id = ?", userID).Pluck("group_id", &ids).Error
	return ids, err
}

func (s *gormStore) UserGroupIDsByName(username string) ([]uint, error) {
	var ids []uint
	err := s.db.Table("dbt_sql_users AS u").Joins("JOIN dbt_sql_users_groups g ON g.users_id = u.id").
		Where("u.username = ?", username).Pluck("g.group_id", &ids).Error
	return ids, err
}

func (s *gormStore) RecordHit(h HitRow, window time.Duration) (uint, error) {
	now := time.Now()
	var prev model.FirewallHit
	err := s.db.Where("rule_id = ? AND user_name = ? AND fingerprint = ? AND effective = ? AND last_at >= ?",
		h.RuleID, h.UserName, h.Fingerprint, h.Effective, now.Add(-window)).Order("id DESC").Limit(1).Take(&prev).Error
	switch {
	case err == nil:
		// 窗口内合并：累加次数。原因文本以最新的为准（用户可能补充了说明）
		upd := map[string]any{"repeat_cnt": gorm.Expr("repeat_cnt + 1"), "last_at": now}
		if h.ReasonText != "" {
			upd["reason_text"] = h.ReasonText
		}
		if e := s.db.Model(&model.FirewallHit{}).Where("id = ?", prev.ID).Updates(upd).Error; e != nil {
			return 0, e
		}
		return prev.ID, nil
	case errors.Is(err, gorm.ErrRecordNotFound):
	default:
		return 0, err
	}
	row := model.FirewallHit{
		CreatedAt: now, LastAt: now, RuleID: h.RuleID, RuleName: h.RuleName, Scene: h.Scene, UserName: h.UserName,
		InstanceID: h.InstanceID, InstanceName: h.InstanceName, DBName: h.DB, Effective: h.Effective, WouldAction: h.WouldAction,
		Fingerprint: h.Fingerprint, SQLText: h.SQLText, ReasonText: h.ReasonText, Matched: h.Matched, RepeatCnt: 1,
	}
	if err := s.db.Create(&row).Error; err != nil {
		return 0, err
	}
	return row.ID, nil
}

func (s *gormStore) AttachQueryLog(ids []uint, queryLogID uint) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.Model(&model.FirewallHit{}).Where("id IN ?", ids).Update("query_log_id", queryLogID).Error
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitUintCSV(s string) []uint {
	var out []uint
	for _, p := range splitCSV(s) {
		if n, err := strconv.ParseUint(p, 10, 32); err == nil {
			out = append(out, uint(n))
		}
	}
	return out
}
