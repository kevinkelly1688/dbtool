// @author: kevinkelly
// @license: Apache Licence
// @file: review_ext_test.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"strings"
	"testing"
)

func TestParseMinSeconds(t *testing.T) {
	for in, want := range map[string]float64{"": 0, " ": 0, "0": 0, "1": 1, "0.5": 0.5, "2.3456": 2.346, "86400": 86400} {
		got, err := parseMinSeconds(in)
		if err != nil || got != want {
			t.Errorf("%q → %v %v，期望 %v", in, got, err, want)
		}
	}
	for _, in := range []string{"-1", "abc", "NaN", "Inf", "86401", "1e9", "1,5"} {
		if _, err := parseMinSeconds(in); err == nil {
			t.Errorf("%q 应报错", in)
		}
	}
}

func TestBuildReviewMinSecondsAndMulti(t *testing.T) {
	base := ReviewQuery{InstanceID: 2, From: "f", To: "t", Sort: "exec_count", Desc: true, Limit: 20, Offset: 40}

	// 不带下限和多实例：与以前一致（没有 HAVING，计数仍是 COUNT(DISTINCT)）
	list, la, count, ca := buildCollectedReview(base)
	if strings.Contains(list, "HAVING") || !strings.Contains(count, "COUNT(DISTINCT l.sql_md5)") || countQ(list) != len(la) || countQ(count) != len(ca) {
		t.Errorf("默认应保持原样: %s | %s", list, count)
	}

	// 统计：平均耗时下限 → HAVING，计数改成对分组结果计数
	q := base
	q.MinSeconds = 1.5
	list, la, count, ca = buildCollectedReview(q)
	if !strings.Contains(list, "HAVING AVG(l.query_time_sec) >= ?") || !strings.Contains(count, "FROM (SELECT 1 FROM dbt_slow_logs l") || !strings.Contains(count, "HAVING AVG(l.query_time_sec) >= ?") {
		t.Errorf("collected HAVING: %s | %s", list, count)
	}
	if countQ(list) != len(la) || countQ(count) != len(ca) || la[len(la)-3] != 1.5 || la[len(la)-2] != 20 || la[len(la)-1] != 40 || ca[len(ca)-1] != 1.5 {
		t.Errorf("collected 参数: %v | %v", la, ca)
	}

	// 明细：单次耗时下限放在 WHERE
	list, la, count, ca = buildCollectedHistory(q)
	if !strings.Contains(list, "l.query_time_sec >= ?") || countQ(list) != len(la) || countQ(count) != len(ca) {
		t.Errorf("collected 明细: %s %v", list, la)
	}

	// 全部实例：IN + 按（实例, 指纹）分组 + 按分组计数
	q = base
	q.Multi = []ReviewTarget{{InstanceID: 1, HostKey: "h1:3306"}, {InstanceID: 3, HostKey: "h3:3306"}}
	list, la, count, ca = buildCollectedReview(q)
	if !strings.Contains(list, "l.instance_id IN (?,?)") || !strings.Contains(list, "GROUP BY l.instance_id, l.sql_md5") || !strings.Contains(count, "FROM (SELECT 1 FROM dbt_slow_logs l") {
		t.Errorf("collected multi: %s | %s", list, count)
	}
	if countQ(list) != len(la) || countQ(count) != len(ca) || la[0] != uint(1) || la[1] != uint(3) {
		t.Errorf("collected multi 参数: %v | %v", la, ca)
	}

	// digest
	dq := ReviewQuery{HostKey: "h:3306", From: "f", To: "t", Sort: "query_time_max", Desc: true, Limit: 30, MinSeconds: 2, Search: "x"}
	list, la, count, ca = buildDigestReview(dq)
	if !strings.Contains(list, "HAVING COALESCE(SUM(h.`Query_time_sum`)/NULLIF(SUM(h.ts_cnt),0),0) >= ?") || !strings.Contains(list, "ORDER BY query_time_max DESC") ||
		countQ(list) != len(la) || countQ(count) != len(ca) || ca[len(ca)-1] != 2.0 {
		t.Errorf("digest HAVING: %s %v | %s %v", list, la, count, ca)
	}
	dq.Multi = []ReviewTarget{{HostKey: "a:1"}, {HostKey: "b:2"}}
	dq.MinSeconds, dq.Search = 0, ""
	list, la, count, ca = buildDigestReview(dq)
	if !strings.Contains(list, "h.hostname_max IN (?,?)") || !strings.Contains(list, "GROUP BY h.hostname_max, h.checksum") || countQ(list) != len(la) || countQ(count) != len(ca) || la[0] != "a:1" {
		t.Errorf("digest multi: %s %v", list, la)
	}
	list, la, count, ca = buildDigestHistory(ReviewQuery{HostKey: "h", From: "f", To: "t", MinSeconds: 3, Search: "s", Limit: 10})
	if !strings.Contains(list, "h.sample LIKE ?") || !strings.Contains(list, "/NULLIF(h.ts_cnt,0),0) >= ?") || countQ(list) != len(la) || countQ(count) != len(ca) {
		t.Errorf("digest 明细: %s %v", list, la)
	}
}

func TestBuildReviewSummaryAndSpark(t *testing.T) {
	q := ReviewQuery{InstanceID: 1, From: "f", To: "t", Search: evil, DB: evil, MinSeconds: 1}
	s, a := buildReviewSummary(SourceCollected, q)
	noEvil(t, "summary", s)
	if !strings.Contains(s, "HAVING AVG(l.query_time_sec) >= ?") || countQ(s) != len(a) || a[len(a)-1] != 1.0 {
		t.Errorf("collected 汇总: %s %v", s, a)
	}
	s, a = buildReviewSummary(SourceDigest, ReviewQuery{HostKey: "h", From: "f", To: "t", Search: evil, MinSeconds: 0})
	noEvil(t, "summary", s)
	if strings.Contains(s, "HAVING") || countQ(s) != len(a) {
		t.Errorf("digest 汇总: %s %v", s, a)
	}
	keys := []SparkKey{{InstanceID: 1, HostKey: "h1", SQLID: "a1"}, {InstanceID: 1, HostKey: "h1", SQLID: "b2"}, {InstanceID: 3, HostKey: "h3", SQLID: "a1"}}
	s, a = buildReviewSpark(SourceCollected, ReviewQuery{From: "f", To: "t", DB: evil}, keys)
	noEvil(t, "spark", s)
	if !strings.Contains(s, "l.instance_id IN (?,?)") || !strings.Contains(s, "l.sql_md5 IN (?,?)") || !strings.Contains(s, "l.db_name = ?") || countQ(s) != len(a) {
		t.Errorf("collected spark: %s %v", s, a)
	}
	s, a = buildReviewSpark(SourceDigest, ReviewQuery{From: "f", To: "t"}, keys)
	if !strings.Contains(s, "h.hostname_max IN (?,?)") || !strings.Contains(s, "h.checksum IN (?,?)") || countQ(s) != len(a) {
		t.Errorf("digest spark: %s %v", s, a)
	}
}

// extRepo 在假仓库上加上统计页的可选能力，并分来源记录收到的查询。
type extRepo struct {
	*fakeRepo
	collQ, digQ []ReviewQuery
	sums        map[string]ReviewSummary
	points      map[string][]SparkPoint
	sumErr      error
}

func (e *extRepo) CollectedReview(q ReviewQuery) ([]ReviewRow, int64, error) {
	e.collQ = append(e.collQ, q)
	return e.fakeRepo.collReview, int64(len(e.fakeRepo.collReview)), nil
}

func (e *extRepo) DigestReview(q ReviewQuery) ([]ReviewRow, int64, error) {
	e.digQ = append(e.digQ, q)
	return e.fakeRepo.digReview, int64(len(e.fakeRepo.digReview)), nil
}

func (e *extRepo) ReviewSummary(source string, q ReviewQuery) (ReviewSummary, error) {
	return e.sums[source], e.sumErr
}

func (e *extRepo) ReviewSpark(source string, q ReviewQuery, keys []SparkKey) ([]SparkPoint, error) {
	return e.points[source], nil
}

func extFixture() (*Service, *extRepo) {
	s, r := fixture()
	e := &extRepo{fakeRepo: r, sums: map[string]ReviewSummary{}, points: map[string][]SparkPoint{}}
	s.repo = e
	return s, e
}

func TestReviewSingleInstanceKPIAndSpark(t *testing.T) {
	s, e := extFixture()
	e.collReview = []ReviewRow{{SQLID: "m1", SQLText: "select * from t where id = 42", SampleSQL: "select * from t where id = 42", ExecCount: 3, InstanceID: 1}}
	e.sums[SourceCollected] = ReviewSummary{Fingerprints: 9, ExecCount: 120, ExecTimeSum: 55.5, QueryTimeMax: 8.25}
	e.points[SourceCollected] = []SparkPoint{{Key: SparkKey{InstanceID: 1, SQLID: "m1"}, Day: "2026-10-02", Count: 2}, {Key: SparkKey{InstanceID: 1, SQLID: "m1"}, Day: "2026-10-04", Count: 1},
		{Key: SparkKey{InstanceID: 1, SQLID: "m1"}, Day: "2026-09-01", Count: 99}} // 范围外的点被忽略
	p, err := s.Review(context.Background(), root, ReviewParams{InstanceID: 1, Source: "collected", Start: "2026-10-01", End: "2026-10-05", MinSeconds: "1.5"})
	if err != nil {
		t.Fatal(err)
	}
	if e.collQ[0].MinSeconds != 1.5 {
		t.Fatalf("下限应传给仓库: %+v", e.collQ[0])
	}
	r := p.Rows[0]
	if r.InstanceName != "prod-mysql-01" || r.InstanceID != 1 || r.SampleSQL == "" {
		t.Fatalf("行: %+v", r)
	}
	if len(r.Spark) != 5 || r.Spark[1] != 2 || r.Spark[3] != 1 || r.Spark[0] != 0 {
		t.Fatalf("逐日次数: %v", r.Spark)
	}
	if p.Sum == nil || p.Sum.Fingerprints != 9 || p.Sum.QueryTimeMax != 8.25 {
		t.Fatalf("KPI: %+v", p.Sum)
	}
	// 汇总失败：统计照常，只是没有 KPI
	e.sumErr = context.DeadlineExceeded
	if p, err = s.Review(context.Background(), root, ReviewParams{InstanceID: 1, Source: "collected", Start: "2026-10-01", End: "2026-10-05"}); err != nil || len(p.Rows) != 1 {
		t.Fatalf("汇总失败不应影响列表: %v", err)
	}
	// 仓库不支持扩展接口：没有 KPI 也没有逐日次数
	s2, r2 := fixture()
	r2.collReview = []ReviewRow{{SQLID: "m1", SQLText: "select 1"}}
	p, err = s2.Review(context.Background(), root, ReviewParams{InstanceID: 1, Source: "collected", Start: "2026-10-01", End: "2026-10-05"})
	if err != nil || p.Sum != nil || p.Rows[0].Spark != nil || p.Rows[0].InstanceName != "prod-mysql-01" {
		t.Fatalf("降级: %+v %v", p, err)
	}
	// 非法下限
	if _, err = s.Review(context.Background(), root, ReviewParams{InstanceID: 1, MinSeconds: "-3"}); err == nil {
		t.Fatal("非法下限应报错")
	}
	if _, err = s.History(context.Background(), root, ReviewParams{InstanceID: 1, MinSeconds: "x"}); err == nil {
		t.Fatal("明细也应校验下限")
	}
}

func TestReviewAllInstances(t *testing.T) {
	s, e := extFixture()
	ctx := context.Background()
	in := ReviewParams{All: true, Start: "2026-10-01", End: "2026-10-04", Limit: 2, Offset: 1, Sort: "exec_count", Order: "desc", MinSeconds: "1"}

	// 只有平台采集来源（没有 pt-query-digest 表）：真实分页下推给仓库
	e.collReview = []ReviewRow{
		{SQLID: "a", SQLText: "select * from t where id = 1", ExecCount: 5, InstanceID: 1},
		{SQLID: "b", SQLText: "select * from u where id = 2", ExecCount: 4, InstanceID: 3},
	}
	e.sums[SourceCollected] = ReviewSummary{Fingerprints: 2, ExecCount: 9, ExecTimeSum: 20, QueryTimeMax: 7}
	e.points[SourceCollected] = []SparkPoint{{Key: SparkKey{InstanceID: 3, SQLID: "b"}, Day: "2026-10-03", Count: 4}}
	p, err := s.Review(ctx, root, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.collQ) != 1 || len(e.digQ) != 0 {
		t.Fatalf("查询次数: %d/%d", len(e.collQ), len(e.digQ))
	}
	q := e.collQ[0]
	ids := []uint{}
	for _, tg := range q.Multi {
		ids = append(ids, tg.InstanceID)
	}
	// root 可见全部；mysql×3(1,2,5) + doris(3) + polardb(7)；redis/pgsql 不在内
	if len(ids) != 5 || q.Limit != 2 || q.Offset != 1 || q.MinSeconds != 1 {
		t.Fatalf("目标与分页: %v %+v", ids, q)
	}
	for _, id := range ids {
		if id == 4 || id == 6 {
			t.Fatalf("redis / pgsql 不应参与汇总: %v", ids)
		}
	}
	if !p.All || p.Source != "all" || p.Total != 2 || len(p.Rows) != 2 || p.Rows[0].InstanceName != "prod-mysql-01" || p.Rows[1].InstanceName != "report-doris" {
		t.Fatalf("结果: %+v", p)
	}
	if p.Rows[0].SQLText != "select * from t where id=?" {
		t.Fatalf("应显示指纹: %q", p.Rows[0].SQLText)
	}
	if p.Sum == nil || p.Sum.ExecCount != 9 || p.Rows[1].Spark[2] != 4 || len(p.Rows[1].Spark) != 4 {
		t.Fatalf("KPI/趋势: %+v %v", p.Sum, p.Rows[1].Spark)
	}

	// 两种来源并存：各取前 offset+limit 行，合并后按排序重排再分页，总数相加
	e.digestOK = true
	e.collQ, e.digQ = nil, nil
	e.collReview = []ReviewRow{{SQLID: "c1", SQLText: "select 1", ExecCount: 10, InstanceID: 3}, {SQLID: "c2", SQLText: "select 2", ExecCount: 2, InstanceID: 3}}
	e.digReview = []ReviewRow{{SQLID: "d1", SQLText: "select d1", ExecCount: 7, HostKey: "10.0.0.1:3306"}, {SQLID: "d2", SQLText: "select d2", ExecCount: 1, HostKey: "10.0.0.2:3306"}}
	e.sums[SourceDigest] = ReviewSummary{Fingerprints: 4, ExecCount: 11, ExecTimeSum: 5, QueryTimeMax: 9}
	e.points[SourceDigest] = []SparkPoint{{Key: SparkKey{HostKey: "10.0.0.1:3306", SQLID: "d1"}, Day: "2026-10-01", Count: 7}}
	in.Offset, in.Limit = 1, 2
	p, err = s.Review(ctx, root, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.collQ) != 1 || len(e.digQ) != 1 || e.collQ[0].Limit != 3 || e.collQ[0].Offset != 0 || e.digQ[0].Limit != 3 {
		t.Fatalf("合并分页应各取前 offset+limit 行: %+v %+v", e.collQ, e.digQ)
	}
	got := []string{}
	for _, r := range p.Rows {
		got = append(got, r.SQLID)
	}
	// 全部降序：c1(10) d1(7) c2(2) d2(1)；跳过 1 条取 2 条 → d1 c2
	if strings.Join(got, ",") != "d1,c2" || p.Total != 4 {
		t.Fatalf("合并重排: %v total=%d", got, p.Total)
	}
	if p.Rows[0].InstanceName != "prod-mysql-01" || p.Rows[0].InstanceID != 1 || p.Rows[0].Spark[0] != 7 {
		t.Fatalf("digest 行应由 host:port 映射到实例并带逐日次数: %+v", p.Rows[0])
	}
	if p.Sum == nil || p.Sum.Fingerprints != 6 || p.Sum.ExecCount != 20 || p.Sum.QueryTimeMax != 9 {
		t.Fatalf("两个来源的 KPI 应相加: %+v", p.Sum)
	}

	// 升序时 offset 越界得到空页
	in.Order, in.Offset = "asc", 10
	if p, err = s.Review(ctx, root, in); err != nil || len(p.Rows) != 0 || p.Total != 4 {
		t.Fatalf("越界: %+v %v", p, err)
	}
}

func TestReviewAllScopeAndValidation(t *testing.T) {
	s, e := extFixture()
	ctx := context.Background()
	// bob(2) 只关联实例 1、3
	if _, err := s.Review(ctx, bob, ReviewParams{All: true}); err != nil {
		t.Fatal(err)
	}
	var ids []uint
	for _, tg := range e.collQ[0].Multi {
		ids = append(ids, tg.InstanceID)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Fatalf("汇总只能包含用户可见的实例: %v", ids)
	}
	// 没有 slowlogs.view/slowquery.view
	if _, err := s.Review(ctx, Principal{ID: 4, Username: "eve"}, ReviewParams{All: true}); err == nil {
		t.Fatal("没有权限应拒绝")
	}
	// 指定了实例又要汇总
	_, err := s.Review(ctx, root, ReviewParams{All: true, InstanceID: 1})
	wantErr(t, err, 400, "不能同时指定实例")
	for name, p := range map[string]ReviewParams{
		"排序": {All: true, Sort: "password"}, "方向": {All: true, Order: "x"}, "每页": {All: true, Limit: 9999}, "下限": {All: true, MinSeconds: "abc"},
		"范围": {All: true, Start: "2025-01-01", End: "2026-10-01"}, "搜索": {All: true, Search: strings.Repeat("s", 201)}, "库": {All: true, DB: "a\nb"},
	} {
		if _, err := s.Review(ctx, root, p); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
	// 一个可见实例都没有：空页，不查库
	e.collQ = nil
	e.grant(9, "slowlogs.view", "slowquery.view")
	if p, err := s.Review(ctx, Principal{ID: 9, Username: "nobody"}, ReviewParams{All: true}); err != nil || len(e.collQ) > 0 || len(p.Rows) != 0 {
		t.Fatalf("没有可见实例应得到空页且不查询: %+v %v", p, err)
	}
}

func TestReviewSortLess(t *testing.T) {
	a := &ReviewRow{SQLID: "a", ExecCount: 5, QueryTimeAvg: 1.5, DBName: "x", CreateTime: "2026-10-02 00:00:00"}
	b := &ReviewRow{SQLID: "b", ExecCount: 5, QueryTimeAvg: 3, DBName: "y", CreateTime: "2026-10-01 00:00:00"}
	if reviewSortLess("exec_count", true)(a, b) != true || reviewSortLess("exec_count", true)(b, a) != false {
		t.Error("次数相同按指纹升序稳定")
	}
	if !reviewSortLess("query_time_avg", true)(b, a) || !reviewSortLess("query_time_avg", false)(a, b) {
		t.Error("按平均耗时")
	}
	if !reviewSortLess("create_time", true)(a, b) || !reviewSortLess("db_name", false)(a, b) {
		t.Error("字符串字段")
	}
}
