// @author: kevinkelly
// @license: Apache Licence
// @file: sqlmask.go
// @time: 2026/10/28

package sqlflow

import (
	"fmt"
	"log/slog"
	"strings"

	"dbtool/internal/audit"
	"dbtool/internal/masking"
	"dbtool/internal/model"
	"dbtool/internal/review"
)

// 工单 SQL 文本脱敏：工单里的 UPDATE / INSERT / REPLACE / DELETE 及其 WHERE，凡是绑定到“数据脱敏”里已配置字段的值，
// 对非超级用户一律显示为脱敏后的值（提交人、审批人、执行人都一样，审批期间也一样）；超级用户看原文。
// 库里保存的仍是原文（执行用它），遮盖发生在读取时：工单详情、检查 / 执行结果、回滚语句、复制、通知。
// 规则来自“数据脱敏”配置（与在线查询共用），语法解析与匹配见 internal/masking/sqlstmt.go。

// MaskColumnLoader 是仓库可选实现：读取实例的脱敏字段配置（store.Store 已实现）。没有实现时工单文本不脱敏。
type MaskColumnLoader interface {
	ActiveMaskingColumns(instanceID uint) ([]model.DataMaskingColumn, error)
}

// wfMask 是针对一张工单的文本脱敏器。nil 表示不需要遮盖（超级用户、没有配置、类型不支持）。
type wfMask struct {
	m       *masking.StmtMasker
	secrets []string
	changed bool
}

// maskDialect 返回类型对应的词法方言；不支持的类型 ok=false。
func maskDialect(dbType string) (masking.Dialect, bool) {
	switch {
	case dbType == dbTypeMSSQL:
		return masking.DialectMSSQL, true
	case mysqlProtocolTypes[dbType] || dbType == "mariadb":
		return masking.DialectMySQL, true
	}
	return 0, false
}

// maskerFor 为查看人 p 构造工单的文本脱敏器；p 为 nil 表示“按非超级用户”（通知等不面向具体查看人的场景）。
// 读脱敏配置失败时返回错误（与在线查询一致：宁可打不开，也不把原文给出去）。
func (s *Service) maskerFor(p *audit.Actor, w *model.SqlWorkflow) (*wfMask, error) {
	if p != nil && p.IsSuperuser {
		return nil, nil
	}
	loader, ok := s.repo.(MaskColumnLoader)
	if !ok || w == nil {
		return nil, nil
	}
	var insts []*model.Instance
	if w.IsBatch() {
		insts, _ = s.batchInstances(w)
	} else if w.Instance != nil {
		insts = []*model.Instance{w.Instance}
	} else if in, err := s.repo.GetInstance(w.InstanceID); err == nil && in != nil {
		insts = []*model.Instance{in}
	}
	var dialect masking.Dialect
	var cols []masking.Column
	have := false
	for _, in := range insts {
		d, ok := maskDialect(in.DBType)
		if !ok {
			continue
		}
		list, err := loader.ActiveMaskingColumns(in.ID)
		if err != nil {
			return nil, fmt.Errorf("读取脱敏配置失败: %w", err)
		}
		if len(list) == 0 {
			continue
		}
		dialect, have = d, true
		for _, c := range list {
			col := masking.Column{Schema: c.TableSchema, Table: c.TableName_, Column: c.ColumnName, RuleType: c.RuleType}
			if c.Kind != "" {
				col.Rule = &masking.Rule{Kind: c.Kind, Prefix: c.KeepPrefix, Suffix: c.KeepSuffix, HideGroup: c.HideGroup, Regex: c.RuleRegex}
			}
			cols = append(cols, col)
		}
	}
	if !have {
		return nil, nil
	}
	return &wfMask{m: masking.NewStmtMasker(dialect, w.DBName, cols, nil)}, nil
}

// SQL 遮盖一段 SQL，并记下被遮盖的原值（之后 Text 用它清除错误信息里的回显）。nil 安全。
func (k *wfMask) SQL(sql string) string {
	if k == nil || sql == "" {
		return sql
	}
	out, secrets := k.m.Mask(sql)
	if out != sql {
		k.changed = true
	}
	k.secrets = append(k.secrets, secrets...)
	return out
}

// Text 清除文本里出现的、已被遮盖的原值。nil 安全。
func (k *wfMask) Text(s string) string {
	if k == nil || len(k.secrets) == 0 || s == "" {
		return s
	}
	return masking.Scrub(s, k.secrets)
}

// Changed 报告是否真的遮盖过东西（用来决定是否禁用复制 / 下载回滚）。
func (k *wfMask) Changed() bool { return k != nil && k.changed }

// Rows 返回遮盖后的结果行（拷贝，不改原切片）：先遮盖所有语句收集原值，再清除错误信息里的回显。
func (k *wfMask) Rows(rows []review.Row) []review.Row {
	if k == nil || len(rows) == 0 {
		return rows
	}
	out := make([]review.Row, len(rows))
	copy(out, rows)
	for i := range out {
		out[i].SQL = k.SQL(out[i].SQL)
	}
	for i := range out {
		out[i].ErrorMessage = k.Text(out[i].ErrorMessage)
	}
	return out
}

// maskDetail 对详情里的文本就地遮盖。Content 换成拷贝（不能改到仓库里的对象）。
func (s *Service) maskDetail(k *wfMask, d *Detail) {
	if k == nil {
		return
	}
	if d.Content != nil {
		cc := *d.Content
		cc.SQLContent = k.SQL(cc.SQLContent)
		cc.ReviewContent, cc.ExecuteResult = "", "" // 原始 JSON 含明文，已拆成下面的 ReviewRows / ExecRows
		d.Content = &cc
	}
	d.ReviewRows = k.Rows(d.ReviewRows)
	d.ExecRows = k.Rows(d.ExecRows)
	for i := range d.Logs {
		d.Logs[i].OperationInfo = k.Text(d.Logs[i].OperationInfo)
	}
}

// notifyMask 构造通知用的脱敏器：通知发到群里，一律按非超级用户处理。取配置失败时记日志并返回 nil，
// 由调用方不放 SQL 内容（见 previewSQL）。
func (s *Service) notifyMask(w *model.SqlWorkflow) (*wfMask, bool) {
	k, err := s.maskerFor(nil, w)
	if err != nil {
		slog.Warn("通知里的 SQL 脱敏失败，已省略 SQL 内容", "workflow", w.ID, "err", err)
		return nil, false
	}
	return k, true
}

// maskedSQLForNotify 返回通知里可以显示的 SQL（已脱敏）；脱敏配置读不出来时返回空串。
func (s *Service) maskedSQLForNotify(w *model.SqlWorkflow, sql string) string {
	k, ok := s.notifyMask(w)
	if !ok {
		return ""
	}
	return strings.ReplaceAll(k.SQL(sql), "\r", "")
}
