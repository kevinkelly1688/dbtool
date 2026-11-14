// @author: kevinkelly
// @license: Apache Licence
// @file: multi_meta.go
// @time: 2026/10/28

package query

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/engine/dm"
	"dbtool/internal/engine/oracle"
	"dbtool/internal/model"
)

// PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦的库表树元数据（DBMeta / TableColumns / SearchColumns 的分支）。
// 表与列来自各引擎的 TableInfos / ColumnInfos（数据字典共用）；表上的敏感字段（🛡）按脱敏规则逐表取列算出（multiSensitive）。

// multiSystemDB 判断该库是否不允许在库表树里浏览（Mongo 的 admin / local / config，ClickHouse 的 system / information_schema，Oracle / 达梦的系统 schema）。
func multiSystemDB(dbType, db string) bool {
	l := strings.ToLower(db)
	switch dbType {
	case dbTypeMongo:
		return l == "admin" || l == "local" || l == "config"
	case dbTypeCH:
		return l == "system" || l == "information_schema"
	case dbTypeOracle:
		return oracle.IsSystemSchema(db)
	case dbTypeDM:
		return dm.IsSystemSchema(db)
	}
	return false
}

// checkMultiMetaDB 校验库名可用于元数据查询：符合实例的库名过滤规则，且不是系统库。
func (s *Service) checkMultiMetaDB(inst *model.Instance, db string) error {
	if strings.TrimSpace(db) == "" {
		return newErr(400, "bad_request", "请选择数据库")
	}
	if multiSystemDB(inst.DBType, db) {
		return newErr(403, "no_privilege", fmt.Sprintf("不允许访问系统库 %s", db))
	}
	ok, err := filterDBs([]string{db}, inst.ShowDBNameRegex, inst.DeniedDBNameRegex)
	if err != nil {
		return err
	}
	if len(ok) == 0 {
		return newErr(403, "no_privilege", "该数据库不在你可访问的范围内")
	}
	return nil
}

func (s *Service) multiMeta(ctx context.Context, p Principal, e engine.Engine, inst *model.Instance, db string) (*DBMeta, error) {
	if err := s.checkMultiMetaDB(inst, db); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var (
		tables []TableMeta
		err    error
	)
	switch inst.DBType {
	case dbTypePG:
		tables, err = s.pgTables(cctx, e, db)
	case dbTypeMongo:
		tables, err = s.mongoTables(cctx, e, db)
	case dbTypeCH:
		tables, err = s.chTables(cctx, e, db)
	case dbTypeOracle:
		tables, err = s.oraTables(cctx, e, db)
	case dbTypeDM:
		tables, err = s.dmTables(cctx, e, db)
	}
	if err != nil {
		return nil, err
	}
	out := &DBMeta{DB: db, Tables: tables, Unmasked: !s.Policy().DataMasking}
	if pp, ok := s.auth.(PolicyProvider); ok {
		if info, err := pp.PolicyInfo(ctx, p, inst, db, s.Policy().AdminLimit); err == nil {
			out.Policy = info
		}
	}
	if out.Policy.Mode == "" {
		out.Policy = PolicyInfo{Mode: "unknown", Tables: []string{}}
	}
	var sens map[int][]string
	if s.Policy().DataMasking { // 系统脱敏总开关关闭时查询结果不脱敏，也就没有敏感字段可标
		sens = s.multiSensitive(ctx, e, inst, db, out.Tables)
	}
	for i := range out.Tables {
		t := &out.Tables[i]
		t.Locked = !out.Policy.HasObject(inst.DBType, t.Name) && out.Policy.Mode != "unknown"
		if t.Locked {
			out.LockedTotal++
		}
		t.Sensitive = sens[i]
		if t.Sensitive == nil {
			t.Sensitive = []string{}
		}
		out.SensitiveTotal += len(t.Sensitive)
	}
	return out, nil
}

func (s *Service) multiColumns(ctx context.Context, e engine.Engine, inst *model.Instance, db, table string) ([]ColumnMeta, error) {
	if err := s.checkMultiMetaDB(inst, db); err != nil {
		return nil, err
	}
	if strings.TrimSpace(table) == "" {
		return nil, newErr(400, "bad_request", "请选择表")
	}
	cols, err := s.multiRawColumns(ctx, e, inst, db, table)
	if err == nil && s.Policy().DataMasking {
		m := s.maskMatcher(inst)
		for i := range cols {
			cols[i].Sensitive = m.match(db, table, cols[i].Name)
		}
	}
	return cols, err
}

// multiRawColumns 取一张表 / 集合的列（不带敏感标记）。
func (s *Service) multiRawColumns(ctx context.Context, e engine.Engine, inst *model.Instance, db, table string) ([]ColumnMeta, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch inst.DBType {
	case dbTypePG:
		return s.pgColumns(cctx, e, db, table)
	case dbTypeMongo:
		return s.mongoColumns(cctx, e, db, table)
	case dbTypeOracle:
		return s.oraColumns(cctx, e, db, table)
	case dbTypeDM:
		return s.dmColumns(cctx, e, db, table)
	}
	return s.chColumns(cctx, e, db, table)
}

const (
	multiSensTables   = 60               // 库表树上最多逐表检查多少张表的字段
	multiSensParallel = 4                // 同时取几张表的列
	multiSensBudget   = 20 * time.Second // 整个检查的时间上限，超了就用已经查到的
)

// mayHit 判断脱敏规则里有没有可能命中这张表的字段（只看库 / 表，不看字段名）：wild 表示有库表都是 * 的通配规则，对所有表都可能命中。
func (m maskMatcher) mayHit(db, table string) (hit, wild bool) {
	for _, c := range m.cols {
		if c.TableSchema == "*" && c.TableName_ == "*" {
			wild = true
			continue
		}
		if !strings.EqualFold(c.TableSchema, db) {
			continue
		}
		if strings.EqualFold(c.TableName_, table) {
			hit = true
		} else if i := strings.LastIndex(table, "."); i >= 0 && !strings.Contains(c.TableName_, ".") && strings.EqualFold(c.TableName_, table[i+1:]) {
			hit = true
		}
	}
	return hit, wild
}

// multiSensitive 算出库里每张表命中脱敏规则的字段（下标 → 字段名），库表树上表名旁的 🛡 数字就来自它，和 MySQL 的 sensitiveInDB 同义。
// 这些引擎没有“一次查全库字段”的通用办法，所以只取规则可能命中的表的列：先是规则里点名的表，有通配规则时再补其它表，最多 multiSensTables 张；
// 取不到的表跳过（不影响页面）。
func (s *Service) multiSensitive(ctx context.Context, e engine.Engine, inst *model.Instance, db string, tables []TableMeta) map[int][]string {
	out := map[int][]string{}
	m := s.maskMatcher(inst)
	if len(m.cols) == 0 {
		return out
	}
	var named, others []int
	for i, t := range tables {
		hit, wild := m.mayHit(db, t.Name)
		switch {
		case hit:
			named = append(named, i)
		case wild:
			others = append(others, i)
		}
	}
	cand := append(named, others...)
	if len(cand) > multiSensTables {
		cand = cand[:multiSensTables]
	}
	if len(cand) == 0 {
		return out
	}
	cctx, cancel := context.WithTimeout(ctx, multiSensBudget)
	defer cancel()
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, multiSensParallel)
	)
	for _, idx := range cand {
		idx := idx
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if cctx.Err() != nil {
				return
			}
			cols, err := s.multiRawColumns(cctx, e, inst, db, tables[idx].Name)
			if err != nil {
				return
			}
			var hits []string
			for _, c := range cols {
				if m.match(db, tables[idx].Name, c.Name) {
					hits = append(hits, c.Name)
				}
			}
			if len(hits) > 0 {
				mu.Lock()
				out[idx] = hits
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

func (s *Service) multiSearch(ctx context.Context, e engine.Engine, inst *model.Instance, db, kw string) ([]ColumnHit, error) {
	if err := s.checkMultiMetaDB(inst, db); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	switch inst.DBType {
	case dbTypePG:
		return s.pgSearch(cctx, e, db, kw)
	case dbTypeCH:
		return s.chSearch(cctx, e, db, kw)
	case dbTypeOracle:
		return s.oraSearch(cctx, e, db, kw)
	case dbTypeDM:
		return s.dmSearch(cctx, e, db, kw)
	}
	return []ColumnHit{}, nil // MongoDB 的字段是采样得到的，不提供全库字段搜索
}
