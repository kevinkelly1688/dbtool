// @author: kevinkelly
// @license: Apache Licence
// @file: source.go
// @time: 2026/10/28

package postgres

import (
	"context"
	"strconv"
	"strings"
)

// 结果列的来源：数据脱敏需要知道每个结果列直接来自哪张表的哪一列（含别名、JOIN 之后），不能只看列名。
// 服务端在语句的结果描述（RowDescription）里给出来源表 OID 与列号，再到 pg_class / pg_attribute 反查名字。

// ColumnSource 是结果列的来源。
type ColumnSource struct {
	Schema  string
	Table   string
	Column  string
	RelKind string // r 表 / p 分区表 / v 视图 / m 物化视图 / f 外部表 …（pg_class.relkind）
}

// FullTable 返回 架构.表。
func (c ColumnSource) FullTable() string { return c.Schema + "." + c.Table }

// IsView 视图类的来源：服务端报的是视图自己的列，不能当作底层表的列处理。
func (c ColumnSource) IsView() bool { return c.RelKind == "v" || c.RelKind == "m" }

type origin struct {
	rel uint32
	att uint16
}

// describeOrigins 用 Parse + Describe（不执行）取每个结果列的来源表 OID 与列号。
// 语句有错时返回与执行时相同的用户错误，调用方直接把它当作查询错误。
func (s *session) describeOrigins(ctx context.Context, sql string) ([]origin, error) {
	sd, err := s.pc.Prepare(ctx, "", sql, nil)
	if err != nil {
		s.fail(err)
		if ctx.Err() != nil {
			s.broken = true
		}
		return nil, s.e.wrapErr(ctx, err)
	}
	out := make([]origin, len(sd.Fields))
	for i, f := range sd.Fields {
		out[i] = origin{rel: f.TableOID, att: f.TableAttributeNumber}
	}
	return out, nil
}

// resolveSources 把来源表 OID / 列号反查成 架构.表.列。失败不影响查询结果：SourcesKnown 保持 false，由上层按列名保守处理。
// 必须在事务还可用时调用。
func (e *Engine) resolveSources(ctx context.Context, s *session, out *Shaped, origins []origin) {
	if len(origins) != len(out.Columns) {
		return
	}
	seen := map[uint32]bool{}
	var parts []string
	for _, o := range origins {
		if o.rel != 0 && !seen[o.rel] {
			seen[o.rel] = true
			parts = append(parts, strconv.FormatUint(uint64(o.rel), 10))
		}
	}
	if len(parts) == 0 {
		out.SourcesKnown = true // 全是表达式列：没有来源，也不需要反查
		return
	}
	rows, err := s.rows(ctx, "SELECT a.attrelid::text, a.attnum::text, n.nspname, c.relname, a.attname, c.relkind::text "+
		"FROM pg_catalog.pg_attribute a "+
		"JOIN pg_catalog.pg_class c ON c.oid = a.attrelid "+
		"JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace "+
		"WHERE a.attrelid = ANY($1::oid[]) AND a.attnum > 0 AND NOT a.attisdropped", "{"+strings.Join(parts, ",")+"}")
	if err != nil {
		return
	}
	byKey := map[string]ColumnSource{}
	for _, r := range rows {
		if len(r) < 6 || r[0] == nil || r[1] == nil || r[2] == nil || r[3] == nil || r[4] == nil {
			continue
		}
		kind := ""
		if r[5] != nil {
			kind = *r[5]
		}
		byKey[*r[0]+"/"+*r[1]] = ColumnSource{Schema: *r[2], Table: *r[3], Column: *r[4], RelKind: kind}
	}
	for i, o := range origins {
		if o.rel == 0 {
			continue
		}
		if cs, ok := byKey[strconv.FormatUint(uint64(o.rel), 10)+"/"+strconv.Itoa(int(o.att))]; ok {
			c := cs
			out.Columns[i].Source = &c
		}
	}
	out.SourcesKnown = true
}
