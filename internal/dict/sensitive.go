// @author: kevinkelly
// @license: Apache Licence
// @file: sensitive.go
// @time: 2026/10/28

package dict

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"dbtool/internal/masking"
	"dbtool/internal/model"
)

// MaskSource 提供某实例激活的脱敏字段配置（dbt_masking_policy）。
// 与在线查询（internal/query/mask.go）用的是同一份配置：store.Store / server.Repo 已实现它，这里只读调用。
type MaskSource interface {
	ActiveMaskingColumns(instanceID uint) ([]model.DataMaskingColumn, error)
}

// WithMasking 设置敏感列规则来源。不设置、规则读取失败或实例没有配置脱敏字段时，数据字典不标敏感列，也不报错。
func (s *Service) WithMasking(m MaskSource) *Service {
	s.mask = m
	return s
}

// colRef 是库里的一列（SQL Server 带架构名，MySQL 的 Schema 为空）。
type colRef struct{ Schema, Table, Column string }

// columnFinder 是可选能力：按列名批量找出库里的列，用来统计每张表的敏感列数。
type columnFinder interface {
	ColumnsNamed(ctx context.Context, q Querier, db string, names []string) ([]colRef, error)
}

// maxSensitiveNames 是一次按列名查找的上限（SQL Server 单条语句最多 2100 个参数）。
const maxSensitiveNames = 500

// sensitiveSet 是某实例某库的敏感列规则。nil 的所有方法都可安全调用，表示“没有规则”。
type sensitiveSet struct {
	db   string
	cols []masking.Column
}

// sensitiveFor 读出实例的激活脱敏字段，只保留与 db 相关的（库名相同，或任意库表通配）。
// 没有规则来源、读取失败（只记日志）或没有相关规则时返回 nil。
func (s *Service) sensitiveFor(inst *model.Instance, db string) *sensitiveSet {
	if s.mask == nil || inst == nil {
		return nil
	}
	list, err := s.mask.ActiveMaskingColumns(inst.ID)
	if err != nil {
		slog.Warn("数据字典读取脱敏配置失败，已跳过敏感列标记", "instance", inst.ID, "err", err)
		return nil
	}
	ss := &sensitiveSet{db: db}
	for _, c := range list {
		if strings.TrimSpace(c.ColumnName) == "" {
			continue
		}
		mc := masking.Column{Schema: c.TableSchema, Table: c.TableName_, Column: c.ColumnName, RuleType: c.RuleType}
		// SQL Server 配置里的“库”可能写成架构名（如 dbo），所以不按库名预过滤。
		if inst.DBType == "mssql" || inst.DBType == "pgsql" || mc.Schema == "*" || strings.EqualFold(mc.Schema, db) {
			ss.cols = append(ss.cols, mc)
		}
	}
	if len(ss.cols) == 0 {
		return nil
	}
	return ss
}

// Has 判断（架构, 表, 列）是否命中脱敏规则。规则与 masking.Analyze 一致：库.表.字段精确匹配（不区分大小写），
// 或配置里库与表都是 * 的同名字段通配。schema 用于 SQL Server / PostgreSQL：配置里的表写成 架构.表，旧配置只写表名也算命中。
// MongoDB 的字段是点号路径，配置里的数组标记 [] 在比较时去掉。
func (ss *sensitiveSet) Has(schema, table, col string) bool {
	if ss == nil {
		return false
	}
	qualified := ""
	if schema != "" {
		qualified = schema + "." + table
	}
	for _, c := range ss.cols {
		if !strings.EqualFold(strings.ReplaceAll(c.Column, "[]", ""), col) {
			continue
		}
		if c.Schema == "*" && c.Table == "*" {
			return true
		}
		if !(strings.EqualFold(c.Schema, ss.db) || (schema != "" && strings.EqualFold(c.Schema, schema))) {
			continue
		}
		if strings.EqualFold(c.Table, table) || (qualified != "" && strings.EqualFold(c.Table, qualified)) {
			return true
		}
	}
	return false
}

// names 返回去重后的配置列名（小写，最多 maxSensitiveNames 个）。
func (ss *sensitiveSet) names() []string {
	if ss == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range ss.cols {
		n := strings.ToLower(strings.ReplaceAll(c.Column, "[]", ""))
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
		if len(out) >= maxSensitiveNames {
			break
		}
	}
	return out
}

// annotateSensitive 给对象列表补“敏感列数”。任何一步失败都只是不标记。
func (s *Service) annotateSensitive(ctx context.Context, q Querier, inst *model.Instance, d dialect, db string, items []Item) {
	if len(items) == 0 {
		return
	}
	cf, ok := d.(columnFinder)
	if !ok {
		return
	}
	ss := s.sensitiveFor(inst, db)
	if ss == nil {
		return
	}
	refs, err := cf.ColumnsNamed(ctx, q, db, ss.names())
	if err != nil {
		slog.Warn("数据字典统计敏感列失败，已跳过", "instance", inst.ID, "db", db, "err", err)
		return
	}
	counts := map[string]int{}
	for _, r := range refs {
		if ss.Has(r.Schema, r.Table, r.Column) {
			counts[r.Schema+"\x01"+r.Table]++
		}
	}
	for i := range items {
		items[i].Sensitive = counts[items[i].Schema+"\x01"+items[i].Name]
	}
}

// sensitiveHead 是详情“列信息”表里追加的列名；sensitiveMark 是命中脱敏配置时该列的值（没命中为空）。
const (
	sensitiveHead = "脱敏"
	sensitiveMark = "Y"
)

// annotateDetail 在表 / 视图详情的“列信息”里追加“脱敏”列（配置了脱敏的字段写 Y），并记录敏感列名。没有规则时原样返回。
func (s *Service) annotateDetail(inst *model.Instance, r DetailRequest, det *Detail) {
	if det == nil || (r.Kind != KindTable && r.Kind != KindView) {
		return
	}
	ss := s.sensitiveFor(inst, r.DB)
	if ss == nil {
		return
	}
	for i := range det.Sections {
		sec := &det.Sections[i]
		if sec.Type != "table" || sec.Title != "列信息" {
			continue
		}
		// columnHeads 是包级共享切片，必须复制后再追加。
		sec.Columns = append(append([]string(nil), sec.Columns...), sensitiveHead)
		rows := make([][]string, 0, len(sec.Rows))
		for _, row := range sec.Rows {
			nr := append([]string(nil), row...)
			if len(row) > 0 && ss.Has(r.Schema, r.Name, row[0]) {
				nr = append(nr, sensitiveMark)
				det.SensitiveColumns = append(det.SensitiveColumns, row[0])
			} else {
				nr = append(nr, "")
			}
			rows = append(rows, nr)
		}
		sec.Rows = rows
		return
	}
}

// placeholders 生成 n 个以逗号分隔的占位符：prefix 为 "?" 时是 ?, ?, ?；为 "@p" 时是 @p1, @p2…（placeholdersFrom 可指定起始序号）。
func placeholders(prefix string, n int) string {
	return placeholdersFrom(prefix, 1, n)
}

func placeholdersFrom(prefix string, start, n int) string {
	parts := make([]string, n)
	for i := range parts {
		if prefix == "?" {
			parts[i] = "?"
		} else {
			parts[i] = prefix + strconv.Itoa(start+i)
		}
	}
	return strings.Join(parts, ", ")
}
