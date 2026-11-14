// @author: kevinkelly
// @license: Apache Licence
// @file: review_ext.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/model"
)

// 本文件是“慢查日志”统计页新增的能力：
//   - 耗时下限筛选（min_seconds）；
//   - 顶部 KPI（指纹数 / 总次数 / 总耗时 / 最慢一次）和每行的逐日次数（行内趋势）；
//   - “全部实例”汇总（all=1）：可见的、有慢日志数据的实例一起统计，每行带实例名。
//
// KPI 与逐日次数由仓库的可选接口 ReviewExtRepo 提供：不实现它的仓库，统计页只是没有 KPI 和行内趋势，其余照常。

// MaxMinSeconds 是耗时下限的上限（一天）。
const MaxMinSeconds = 86400

// parseMinSeconds 解析耗时下限（秒，可带小数，最多 3 位有效小数）。空串表示不限。
func parseMinSeconds(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v != v || v < 0 || v > MaxMinSeconds {
		return 0, badReq("耗时下限需是 0 到 %d 之间的数字（秒）", MaxMinSeconds)
	}
	return float64(int64(v*1000+0.5)) / 1000, nil
}

// SparkKey 标识统计里的一行（逐日次数按它取）。
type SparkKey struct {
	InstanceID uint   // 平台采集来源
	HostKey    string // pt-query-digest 来源
	SQLID      string
}

// SparkPoint 是某行某天的慢查次数。
type SparkPoint struct {
	Key   SparkKey
	Day   string // YYYY-MM-DD
	Count int64
}

// ReviewExtRepo 是统计页 KPI 与行内趋势需要的仓库能力。
type ReviewExtRepo interface {
	// ReviewSummary 返回与统计列表相同过滤条件下（不分页）的汇总。source 为 SourceCollected 或 SourceDigest。
	ReviewSummary(source string, q ReviewQuery) (ReviewSummary, error)
	// ReviewSpark 返回给定各行在 [q.From, q.To) 内逐日的慢查次数。
	ReviewSpark(source string, q ReviewQuery, keys []SparkKey) ([]SparkPoint, error)
}

// ---- SQL ----

// reviewGroup 与 buildCollectedReview / buildDigestReview 的分组方式一致。
func reviewGroup(source string, q ReviewQuery) (group, having string) {
	if source == SourceDigest {
		group = "h.checksum"
		if len(q.Multi) > 0 {
			group = "h.hostname_max, h.checksum"
		}
		if q.MinSeconds > 0 {
			having = " HAVING COALESCE(SUM(h.`Query_time_sum`)/NULLIF(SUM(h.ts_cnt),0),0) >= ?"
		}
		return
	}
	group = "l.sql_md5"
	if len(q.Multi) > 0 {
		group = "l.instance_id, l.sql_md5"
	}
	if q.MinSeconds > 0 {
		having = " HAVING AVG(l.query_time_sec) >= ?"
	}
	return
}

// buildReviewSummary 汇总 SQL：先按列表同样的方式分组（含 HAVING），再对各组求和。
func buildReviewSummary(source string, q ReviewQuery) (string, []any) {
	group, having := reviewGroup(source, q)
	var inner string
	var args []any
	if source == SourceDigest {
		where, a := digestWhere(q)
		if q.Search != "" {
			where += " AND h.checksum IN (SELECT checksum FROM " + tblReview + " WHERE fingerprint LIKE ?)"
			a = append(a, likePattern(q.Search))
		}
		inner = "SELECT COALESCE(SUM(h.ts_cnt),0) AS cnt, COALESCE(SUM(h.`Query_time_sum`),0) AS sum_t, COALESCE(MAX(h.`Query_time_max`),0) AS max_t FROM " +
			tblReviewHist + " h WHERE " + where + " GROUP BY " + group + having
		args = a
	} else {
		where, a := collectedWhere(q, true)
		inner = "SELECT COUNT(*) AS cnt, COALESCE(SUM(l.query_time_sec),0) AS sum_t, COALESCE(MAX(l.query_time_sec),0) AS max_t FROM " +
			tblSlowLogs + " l WHERE " + where + " GROUP BY " + group + having
		args = a
	}
	if q.MinSeconds > 0 {
		args = append(append([]any{}, args...), q.MinSeconds)
	}
	return "SELECT COUNT(*) AS fps, COALESCE(SUM(t.cnt),0) AS cnt, COALESCE(SUM(t.sum_t),0) AS sum_t, COALESCE(MAX(t.max_t),0) AS max_t FROM (" + inner + ") t", args
}

// buildReviewSpark 逐日次数 SQL：只取给定的这些行。
func buildReviewSpark(source string, q ReviewQuery, keys []SparkKey) (string, []any) {
	var args []any
	if source == SourceDigest {
		hosts, sums := map[string]bool{}, map[string]bool{}
		var hl, sl []any
		for _, k := range keys {
			if !hosts[k.HostKey] {
				hosts[k.HostKey] = true
				hl = append(hl, k.HostKey)
			}
			if !sums[k.SQLID] {
				sums[k.SQLID] = true
				sl = append(sl, k.SQLID)
			}
		}
		w := "h.hostname_max IN (" + placeholders(len(hl)) + ") AND h.checksum IN (" + placeholders(len(sl)) + ") AND h.ts_min >= ? AND h.ts_min < ?"
		args = append(append(append([]any{}, hl...), sl...), q.From, q.To)
		if q.DB != "" {
			w += " AND h.db_max = ?"
			args = append(args, q.DB)
		}
		return "SELECT h.hostname_max AS host_key, h.checksum AS sql_id, DATE_FORMAT(h.ts_min,'%Y-%m-%d') AS d, COALESCE(SUM(h.ts_cnt),0) AS c FROM " +
			tblReviewHist + " h WHERE " + w + " GROUP BY h.hostname_max, h.checksum, d", args
	}
	insts, sums := map[uint]bool{}, map[string]bool{}
	var il, sl []any
	for _, k := range keys {
		if !insts[k.InstanceID] {
			insts[k.InstanceID] = true
			il = append(il, k.InstanceID)
		}
		if !sums[k.SQLID] {
			sums[k.SQLID] = true
			sl = append(sl, k.SQLID)
		}
	}
	w := "l.instance_id IN (" + placeholders(len(il)) + ") AND l.sql_md5 IN (" + placeholders(len(sl)) + ") AND l.start_time >= ? AND l.start_time < ?"
	args = append(append(append([]any{}, il...), sl...), q.From, q.To)
	if q.DB != "" {
		w += " AND l.db_name = ?"
		args = append(args, q.DB)
	}
	return "SELECT l.instance_id AS instance_id, l.sql_md5 AS sql_id, DATE_FORMAT(l.start_time,'%Y-%m-%d') AS d, COUNT(*) AS c FROM " +
		tblSlowLogs + " l WHERE " + w + " GROUP BY l.instance_id, l.sql_md5, d", args
}

func (s *store) ReviewSummary(source string, q ReviewQuery) (ReviewSummary, error) {
	query, args := buildReviewSummary(source, q)
	var r struct {
		Fps int64   `gorm:"column:fps"`
		Cnt float64 `gorm:"column:cnt"`
		Sum float64 `gorm:"column:sum_t"`
		Max float64 `gorm:"column:max_t"`
	}
	if err := s.db.Raw(query, args...).Scan(&r).Error; err != nil {
		return ReviewSummary{}, err
	}
	return ReviewSummary{Fingerprints: r.Fps, ExecCount: int64(r.Cnt), ExecTimeSum: round(r.Sum, 3), QueryTimeMax: round(r.Max, 3)}, nil
}

func (s *store) ReviewSpark(source string, q ReviewQuery, keys []SparkKey) ([]SparkPoint, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	query, args := buildReviewSpark(source, q, keys)
	var rows []struct {
		InstanceID uint    `gorm:"column:instance_id"`
		HostKey    string  `gorm:"column:host_key"`
		SQLID      string  `gorm:"column:sql_id"`
		Day        string  `gorm:"column:d"`
		Count      float64 `gorm:"column:c"`
	}
	if err := s.db.Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]SparkPoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, SparkPoint{Key: SparkKey{InstanceID: r.InstanceID, HostKey: r.HostKey, SQLID: r.SQLID}, Day: r.Day, Count: int64(r.Count)})
	}
	return out, nil
}

// ---- 服务层 ----

func (s *Service) reviewExt() ReviewExtRepo {
	e, _ := s.repo.(ReviewExtRepo)
	return e
}

// dayIndex 返回 day 相对 startDay 的天数偏移，越界返回 -1。
func dayIndex(startDay, day string, n int) int {
	a, err1 := time.Parse(dayLayout, startDay)
	b, err2 := time.Parse(dayLayout, day)
	if err1 != nil || err2 != nil {
		return -1
	}
	i := int(b.Sub(a).Hours() / 24)
	if i < 0 || i >= n {
		return -1
	}
	return i
}

func daysIn(startDay, endDay string) int {
	a, err1 := time.Parse(dayLayout, startDay)
	b, err2 := time.Parse(dayLayout, endDay)
	if err1 != nil || err2 != nil || b.Before(a) {
		return 0
	}
	return int(b.Sub(a).Hours()/24) + 1
}

// enrichReview 给单实例的统计页补实例名、KPI 和逐日次数。补充信息取不到不影响统计本身。
func (s *Service) enrichReview(page *ReviewPage, rc *reviewCtx) {
	for i := range page.Rows {
		page.Rows[i].InstanceID, page.Rows[i].InstanceName = rc.inst.ID, rc.inst.InstanceName
	}
	s.fillExtras(page, rc.source, rc.q, rc.startDay, rc.endDay, nil)
}

// fillExtras 取 KPI 与逐日次数写进 page。targets 非空表示全部实例汇总（此时 source 是该来源下的一组实例）。
func (s *Service) fillExtras(page *ReviewPage, source string, q ReviewQuery, startDay, endDay string, merge *ReviewSummary) {
	ext := s.reviewExt()
	if ext == nil {
		return
	}
	sum, err := ext.ReviewSummary(source, q)
	if err != nil {
		slog.Warn("读取慢查日志统计汇总失败", "source", source, "err", err)
	} else if merge != nil {
		merge.Fingerprints += sum.Fingerprints
		merge.ExecCount += sum.ExecCount
		merge.ExecTimeSum += sum.ExecTimeSum
		if sum.QueryTimeMax > merge.QueryTimeMax {
			merge.QueryTimeMax = sum.QueryTimeMax
		}
	} else {
		page.Sum = &sum
	}
	if len(page.Rows) == 0 {
		return
	}
	n := daysIn(startDay, endDay)
	if n <= 0 || n > MaxSpanDays {
		return
	}
	keys := make([]SparkKey, 0, len(page.Rows))
	for _, r := range page.Rows {
		keys = append(keys, SparkKey{InstanceID: r.InstanceID, HostKey: r.HostKey, SQLID: r.SQLID})
	}
	pts, err := ext.ReviewSpark(source, q, keys)
	if err != nil {
		slog.Warn("读取慢查日志逐日次数失败", "source", source, "err", err)
		return
	}
	series := map[SparkKey][]int64{}
	for _, p := range pts {
		i := dayIndex(startDay, p.Day, n)
		if i < 0 {
			continue
		}
		if series[p.Key] == nil {
			series[p.Key] = make([]int64, n)
		}
		series[p.Key][i] += p.Count
	}
	for i := range page.Rows {
		r := &page.Rows[i]
		k := SparkKey{InstanceID: r.InstanceID, HostKey: r.HostKey, SQLID: r.SQLID}
		if source == SourceDigest {
			k.InstanceID = 0
		} else {
			k.HostKey = ""
		}
		if sp := series[k]; sp != nil {
			r.Spark = sp
		} else {
			r.Spark = make([]int64, n)
		}
	}
}

// ---- 全部实例汇总 ----

// reviewKeyOf 是排序字段到取值的映射（两个数据源的结果合并后重排用）。
func reviewSortLess(sortKey string, desc bool) func(a, b *ReviewRow) bool {
	num := map[string]func(r *ReviewRow) float64{
		"exec_count":        func(r *ReviewRow) float64 { return float64(r.ExecCount) },
		"exec_time_sum":     func(r *ReviewRow) float64 { return r.ExecTimeSum },
		"query_time_avg":    func(r *ReviewRow) float64 { return r.QueryTimeAvg },
		"query_time_max":    func(r *ReviewRow) float64 { return r.QueryTimeMax },
		"parse_total_rows":  func(r *ReviewRow) float64 { return float64(r.ParseTotalRows) },
		"return_total_rows": func(r *ReviewRow) float64 { return float64(r.ReturnTotal) },
		"parse_row_avg":     func(r *ReviewRow) float64 { return float64(r.ParseRowAvg) },
		"return_row_avg":    func(r *ReviewRow) float64 { return float64(r.ReturnRowAvg) },
	}
	str := map[string]func(r *ReviewRow) string{
		"create_time": func(r *ReviewRow) string { return r.CreateTime },
		"first_seen":  func(r *ReviewRow) string { return r.FirstSeen },
		"db_name":     func(r *ReviewRow) string { return r.DBName },
	}
	return func(a, b *ReviewRow) bool {
		c := 0
		if f, ok := num[sortKey]; ok {
			switch x, y := f(a), f(b); {
			case x < y:
				c = -1
			case x > y:
				c = 1
			}
		} else if f, ok := str[sortKey]; ok {
			c = strings.Compare(f(a), f(b))
		}
		if c != 0 {
			if desc {
				return c > 0
			}
			return c < 0
		}
		if a.SQLID != b.SQLID {
			return a.SQLID < b.SQLID
		}
		return a.InstanceID < b.InstanceID
	}
}

// reviewAll 统计页的“全部实例”汇总：用户可见的、有慢日志数据源的实例一起统计，每行带实例名。
// 两种数据源（平台采集 / pt-query-digest）各查一次，各取前 offset+limit 行后合并重排再分页，总数是两边之和。
// 阿里云 RDS 来源的实例没有对接，跳过并在 notes 里说明。
func (s *Service) reviewAll(ctx context.Context, p Principal, in ReviewParams) (*ReviewPage, error) {
	if err := s.requirePerm(p, viewButtons...); err != nil {
		return nil, err
	}
	if in.InstanceID != 0 {
		return nil, badReq("全部实例汇总不能同时指定实例")
	}
	from, to, startDay, endDay, err := s.parseRange(in.Start, in.End, 7)
	if err != nil {
		return nil, err
	}
	db, err := validDB(in.DB)
	if err != nil {
		return nil, err
	}
	search, err := validSearch(in.Search)
	if err != nil {
		return nil, err
	}
	minSec, err := parseMinSeconds(in.MinSeconds)
	if err != nil {
		return nil, err
	}
	sortBy, err := validSort(in.Sort, "exec_count", reviewSorts)
	if err != nil {
		return nil, err
	}
	desc, err := validOrder(in.Order)
	if err != nil {
		return nil, err
	}
	limit, offset, err := validPaging(in.Limit, in.Offset)
	if err != nil {
		return nil, err
	}
	sc, err := s.scopeOf(p)
	if err != nil {
		return nil, err
	}
	list, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	var coll, dig []ReviewTarget
	byID := map[uint]*model.Instance{}
	byHost := map[string]*model.Instance{}
	skipped := 0
	for i := range list {
		inst := &list[i]
		if !sc.allows(inst.ID) || !(inst.DBType == "mysql" || platformTypes[inst.DBType]) {
			continue
		}
		src, err := s.pickSource(inst, "")
		if err != nil {
			skipped++
			continue
		}
		byID[inst.ID] = inst
		if src == SourceDigest {
			if _, dup := byHost[hostKey(inst)]; !dup {
				byHost[hostKey(inst)] = inst
				dig = append(dig, ReviewTarget{InstanceID: inst.ID, HostKey: hostKey(inst)})
			}
		} else {
			coll = append(coll, ReviewTarget{InstanceID: inst.ID, HostKey: hostKey(inst)})
		}
	}
	page := &ReviewPage{Rows: []ReviewRow{}, Source: "all", Label: "全部实例汇总", From: startDay, To: endDay, All: true}
	if skipped > 0 {
		page.Notes = append(page.Notes, fmt.Sprintf("有 %d 个实例的慢日志来源暂不支持（阿里云 RDS 未对接 / 数据源不可用），未计入汇总。", skipped))
	}
	if len(coll) == 0 && len(dig) == 0 {
		return page, nil
	}
	base := ReviewQuery{DB: db, Search: search, From: from, To: to, Sort: sortBy, Desc: desc, MinSeconds: minSec}
	both := len(coll) > 0 && len(dig) > 0
	run := func(source string, targets []ReviewTarget) ([]ReviewRow, ReviewQuery, error) {
		q := base
		q.Multi = targets
		q.Limit, q.Offset = limit, offset
		if both { // 两边合并后重排再分页：每边都取前 offset+limit 行
			q.Limit, q.Offset = offset+limit, 0
		}
		var rows []ReviewRow
		var total int64
		var err error
		if source == SourceDigest {
			rows, total, err = s.repo.DigestReview(q)
		} else {
			rows, total, err = s.repo.CollectedReview(q)
		}
		if err != nil {
			return nil, q, err
		}
		page.Total += total
		for i := range rows {
			if source == SourceDigest {
				if in := byHost[rows[i].HostKey]; in != nil {
					rows[i].InstanceID = in.ID
				}
			} else if fp := Fingerprint(rows[i].SQLText); fp != "" {
				rows[i].SQLText = fp
			}
			if in := byID[rows[i].InstanceID]; in != nil {
				rows[i].InstanceName = in.InstanceName
			}
		}
		return rows, q, nil
	}
	var all []ReviewRow
	var sum ReviewSummary
	type part struct {
		source string
		q      ReviewQuery
		start  int
		end    int
	}
	var parts []part
	for _, g := range []struct {
		source  string
		targets []ReviewTarget
	}{{SourceCollected, coll}, {SourceDigest, dig}} {
		if len(g.targets) == 0 {
			continue
		}
		rows, q, err := run(g.source, g.targets)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part{g.source, q, len(all), len(all) + len(rows)})
		all = append(all, rows...)
	}
	if both {
		less := reviewSortLess(sortBy, desc)
		sort.SliceStable(all, func(i, j int) bool { return less(&all[i], &all[j]) })
		if offset >= len(all) {
			all = nil
		} else {
			all = all[offset:]
		}
		if len(all) > limit {
			all = all[:limit]
		}
	}
	if all == nil {
		all = []ReviewRow{}
	}
	for i := range all {
		all[i].SQLText, all[i].Truncated = clipText(all[i].SQLText)
	}
	page.Rows = all
	// KPI：两边汇总相加；逐日次数按来源分别取（key 里带来源标识，两边互不干扰）
	if ext := s.reviewExt(); ext != nil {
		for _, pt := range parts {
			sub := &ReviewPage{}
			for _, r := range all {
				if (pt.source == SourceDigest) == (r.HostKey != "") {
					sub.Rows = append(sub.Rows, r)
				}
			}
			s.fillExtras(sub, pt.source, pt.q, startDay, endDay, &sum)
			// 把逐日次数写回 page 里对应的行
			idx := 0
			for i := range page.Rows {
				if (pt.source == SourceDigest) == (page.Rows[i].HostKey != "") {
					page.Rows[i].Spark = sub.Rows[idx].Spark
					idx++
				}
			}
		}
		page.Sum = &sum
	}
	return page, nil
}
