// @author: kevinkelly
// @license: Apache Licence
// @file: tuning.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SQLTuning：自实现（不依赖外部程序）。
//   - 系统参数用 SHOW GLOBAL VARIABLES 读取，不依赖 performance_schema，5.6 也能用；
//   - 表统计信息覆盖语句里出现的所有表，最多 10 张；
//   - 只接受 SELECT；PROFILE 会真实执行该查询，因此执行前设置只读事务与服务端超时，结果集直接丢弃不返回。

// sysParmFilter 是读取系统参数时要查询的变量名。
var sysParmFilter = []string{
	"binlog_cache_size", "bulk_insert_buffer_size", "have_partition_engine", "have_query_cache", "interactive_timeout",
	"join_buffer_size", "key_buffer_size", "key_cache_age_threshold", "key_cache_block_size", "key_cache_division_limit",
	"large_pages", "locked_in_memory", "long_query_time", "max_allowed_packet", "max_binlog_cache_size", "max_binlog_size",
	"max_connect_errors", "max_connections", "max_join_size", "max_length_for_sort_data", "max_seeks_for_key",
	"max_sort_length", "max_tmp_tables", "max_user_connections", "optimizer_prune_level", "optimizer_search_depth",
	"query_cache_size", "query_cache_type", "query_prealloc_size", "range_alloc_block_size", "read_buffer_size",
	"read_rnd_buffer_size", "sort_buffer_size", "sql_mode", "table_cache", "thread_cache_size", "tmp_table_size", "wait_timeout",
}

// SQLTuning 的选项（对应页面复选框）。
const (
	OptSysParm    = "sys_parm"
	OptSQLPlan    = "sql_plan"
	OptObjStat    = "obj_stat"
	OptSQLProfile = "sql_profile"
)

type ObjectStat struct {
	Table     string `json:"table"`
	Structure string `json:"structure,omitempty"`
	Info      *Table `json:"info,omitempty"`
	Indexes   *Table `json:"indexes,omitempty"`
	Error     string `json:"error,omitempty"`
}

type ProfileResult struct {
	ExecuteSeconds float64 `json:"execute_seconds"`
	Rows           int64   `json:"rows"`
	StatusDiff     *Table  `json:"status_diff,omitempty"`
	Detail         *Table  `json:"detail,omitempty"`
	Summary        *Table  `json:"summary,omitempty"`
	Error          string  `json:"error,omitempty"`
}

type TuningResult struct {
	SQL             string         `json:"sql"`
	Basic           *Table         `json:"basic,omitempty"`
	SysParams       *Table         `json:"sys_params,omitempty"`
	OptimizerSwitch *Table         `json:"optimizer_switch,omitempty"`
	Plan            *Table         `json:"plan,omitempty"`
	Rewrite         *Table         `json:"rewrite,omitempty"` // SHOW WARNINGS：优化器改写后的 SQL
	Objects         []ObjectStat   `json:"objects,omitempty"`
	Profile         *ProfileResult `json:"profile,omitempty"`
	Errors          []string       `json:"errors,omitempty"`
}

func (s *Service) runTuning(ctx context.Context, t *target, stmt string, opts map[string]bool) *TuningResult {
	r := &TuningResult{SQL: stmt}
	fail := func(what string, err error) {
		r.Errors = append(r.Errors, fmt.Sprintf("%s失败：%s", what, redact(err.Error(), t.secrets...)))
	}
	sess := t.sess
	var err error
	if r.Basic, err = sess.Query(ctx, "SELECT VERSION() AS version"); err != nil {
		fail("读取版本", err)
	}
	if opts[OptSysParm] {
		in := "'" + strings.Join(sysParmFilter, "','") + "'"
		if r.SysParams, err = sess.Query(ctx, "SHOW GLOBAL VARIABLES WHERE Variable_name IN ("+in+")"); err != nil {
			fail("读取系统参数", err)
		}
		if r.OptimizerSwitch, err = sess.Query(ctx, "SHOW GLOBAL VARIABLES LIKE 'optimizer_switch'"); err != nil {
			fail("读取 optimizer_switch", err)
		}
	}
	if opts[OptSQLPlan] {
		if r.Plan, err = sess.Query(ctx, "EXPLAIN "+stmt); err != nil {
			fail("获取执行计划", err)
		} else if r.Rewrite, err = sess.Query(ctx, "SHOW WARNINGS"); err != nil {
			fail("读取优化器改写后的 SQL", err)
		}
	}
	if opts[OptObjStat] {
		r.Objects = s.objectStats(ctx, t, stmt)
	}
	if opts[OptSQLProfile] {
		r.Profile = s.profile(ctx, t, stmt)
	}
	return r
}

func (s *Service) objectStats(ctx context.Context, t *target, stmt string) []ObjectStat {
	refs := parseStmt(stmt).tableRefs()
	var out []ObjectStat
	for i, ref := range refs {
		if i >= MaxTuningTables {
			out = append(out, ObjectStat{Table: "…", Error: fmt.Sprintf("语句涉及的表超过 %d 张，其余的没有展示", MaxTuningTables)})
			break
		}
		schema := ref.Schema
		if schema == "" {
			schema = t.db
		}
		name := ref.Name
		if schema != "" {
			name = schema + "." + ref.Name
		}
		o := ObjectStat{Table: name}
		if schema == "" {
			o.Error = "没有选择数据库，无法读取表信息"
			out = append(out, o)
			continue
		}
		if !t.visible[strings.ToLower(schema)] {
			o.Error = "无权访问该数据库"
			out = append(out, o)
			continue
		}
		ddl, err := t.sess.Query(ctx, "SHOW CREATE TABLE "+quoteIdent(schema)+"."+quoteIdent(ref.Name))
		if err != nil {
			o.Error = fmt.Sprintf("读取表结构失败（可能是 CTE、派生表或不存在的表）：%s", redact(err.Error(), t.secrets...))
			out = append(out, o)
			continue
		}
		if len(ddl.Rows) > 0 && len(ddl.Rows[0]) > 1 {
			o.Structure = cellString(ddl.Rows[0][1])
		}
		o.Info, _ = t.sess.Query(ctx, "SELECT table_name, engine, row_format AS format, table_rows, avg_row_length AS avg_row, ROUND((data_length + index_length) / 1024 / 1024, 2) AS total_mb, ROUND(data_length / 1024 / 1024, 2) AS data_mb, ROUND(index_length / 1024 / 1024, 2) AS index_mb FROM information_schema.tables WHERE table_schema = ? AND table_name = ?", schema, ref.Name)
		o.Indexes, _ = t.sess.Query(ctx, "SELECT table_name, index_name, non_unique, seq_in_index, column_name, collation, cardinality, nullable, index_type FROM information_schema.statistics WHERE table_schema = ? AND table_name = ? ORDER BY index_name, seq_in_index", schema, ref.Name)
		out = append(out, o)
	}
	return out
}

// statusMap 把 SHOW SESSION STATUS 的结果转成 map。
func statusMap(t *Table) map[string]string {
	m := map[string]string{}
	if t == nil {
		return m
	}
	for _, r := range t.Rows {
		if len(r) >= 2 {
			m[cellString(r[0])] = cellString(r[1])
		}
	}
	return m
}

func (s *Service) profile(ctx context.Context, t *target, stmt string) *ProfileResult {
	pr := &ProfileResult{}
	sess := t.sess
	// 双保险：只读事务 + 服务端执行超时（老版本不支持就忽略）
	_, _ = sess.Query(ctx, "SET SESSION TRANSACTION READ ONLY")
	_, _ = sess.Query(ctx, "SET SESSION max_execution_time = 60000")
	if _, err := sess.Query(ctx, "SET profiling = 1"); err != nil {
		pr.Error = fmt.Sprintf("当前 MySQL 不支持 profiling：%s", redact(err.Error(), t.secrets...))
		return pr
	}
	before, err := sess.Query(ctx, "SHOW SESSION STATUS")
	if err != nil {
		pr.Error = fmt.Sprintf("读取会话状态失败：%s", redact(err.Error(), t.secrets...))
		return pr
	}
	start := time.Now()
	n, err := sess.Drain(ctx, stmt)
	pr.ExecuteSeconds = float64(time.Since(start).Microseconds()) / 1e6
	pr.Rows = n
	if err != nil {
		pr.Error = t.dbError(ctx, err, "执行语句").Error()
		return pr
	}
	after, err := sess.Query(ctx, "SHOW SESSION STATUS")
	if err != nil {
		pr.Error = fmt.Sprintf("读取会话状态失败：%s", redact(err.Error(), t.secrets...))
		return pr
	}
	// 执行前后差异
	diff := &Table{Columns: []string{"status_name", "before", "after", "diff"}, Rows: [][]any{}}
	b, a := statusMap(before), statusMap(after)
	for _, r := range after.Rows {
		name := cellString(r[0])
		bv, av := b[name], a[name]
		if bv == av {
			continue
		}
		bf, e1 := strconv.ParseFloat(bv, 64)
		af, e2 := strconv.ParseFloat(av, 64)
		d := ""
		if e1 == nil && e2 == nil {
			d = strconv.FormatFloat(af-bf, 'f', -1, 64)
		}
		diff.Rows = append(diff.Rows, []any{name, bv, av, d})
	}
	pr.StatusDiff = diff
	// PROFILE：SHOW PROFILES 里编号最大的就是刚才执行的语句
	profiles, err := sess.Query(ctx, "SHOW PROFILES")
	if err != nil || len(profiles.Rows) == 0 {
		pr.Error = "没有取到 PROFILE 信息（profiling 可能被服务端禁用）"
		return pr
	}
	qid := int64(0)
	for _, r := range profiles.Rows {
		if id, err := strconv.ParseInt(cellString(r[0]), 10, 64); err == nil && id > qid {
			qid = id
		}
	}
	if pr.Detail, err = sess.Query(ctx, "SELECT state, duration, cpu_user, cpu_system, block_ops_in, block_ops_out, messages_sent, messages_received, page_faults_major, page_faults_minor, swaps FROM information_schema.profiling WHERE query_id = ? ORDER BY seq", qid); err != nil {
		pr.Error = fmt.Sprintf("读取 PROFILE 明细失败：%s", redact(err.Error(), t.secrets...))
		return pr
	}
	pr.Summary, _ = sess.Query(ctx, "SELECT state, SUM(duration) AS total_r, ROUND(100 * SUM(duration) / (SELECT SUM(duration) FROM information_schema.profiling WHERE query_id = ?), 2) AS pct_r, COUNT(*) AS calls, SUM(duration) / COUNT(*) AS r_call FROM information_schema.profiling WHERE query_id = ? GROUP BY state ORDER BY total_r DESC", qid, qid)
	return pr
}
