// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/model"
)

var (
	root = Principal{ID: 1, Username: "root", IsSuperuser: true}
	bob  = Principal{ID: 2, Username: "bob", Display: "鲍勃"}
	all  = Principal{ID: 3, Username: "alice"} // 有 query_all_instances
	nobo = Principal{ID: 4, Username: "nobody"}
)

func fixture() (*Service, *fakeRepo) {
	r := newFakeRepo()
	r.instances = []model.Instance{
		{ID: 1, InstanceName: "prod-mysql-01", DBType: "mysql", Host: "10.0.0.1", Port: 3306, IsEnable: true},
		{ID: 2, InstanceName: "prod-mysql-02", DBType: "mysql", Host: "10.0.0.2", Port: 3306, IsEnable: true},
		{ID: 3, InstanceName: "report-doris", DBType: "doris", Host: "10.0.0.3", Port: 9030, IsEnable: true},
		{ID: 4, InstanceName: "redis-01", DBType: "redis", Host: "10.0.0.4", Port: 6379, IsEnable: true},
		{ID: 5, InstanceName: "old-mysql", DBType: "mysql", Host: "10.0.0.5", Port: 3306, IsEnable: false},
		{ID: 6, InstanceName: "pg-1", DBType: "pgsql", Host: "10.0.0.6", Port: 5432, IsEnable: true},
		{ID: 7, InstanceName: "polar", DBType: "polardb", Host: "10.0.0.7", Port: 3306, IsEnable: true},
	}
	r.grant(2, "slowlogs.view", "slowquery.view", "slowquery.advise")
	r.grant(3, "slowlogs.view", "slowquery.view", permQueryAll)
	r.grant(4, "slowquery.advise") // 没有 slowlogs.view/slowquery.view
	r.userInsts[2] = map[uint]bool{1: true, 3: true}
	s := NewService(r, Options{Now: func() time.Time { return time.Date(2026, 10, 6, 15, 4, 5, 0, time.Local) }})
	return s, r
}

func wantErr(t *testing.T, err error, status int, contains string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("期望业务错误 %d %q，实际 %v", status, contains, err)
	}
	if e.HTTP != status || !strings.Contains(e.Message, contains) {
		t.Fatalf("期望 %d 含 %q，实际 %d %q", status, contains, e.HTTP, e.Message)
	}
}

func TestPermissionRequired(t *testing.T) {
	s, _ := fixture()
	if _, err := s.Instances(nobo, "query"); err == nil {
		t.Fatal("没有 slowlogs.view/slowquery.view 应被拒绝")
	} else {
		wantErr(t, err, 403, "没有权限")
	}
	if _, err := s.Groups(nobo, GroupParams{}); err == nil {
		t.Fatal("Groups 应校验权限")
	}
	// 只有优化权限、没有菜单权限：不能看数据，但“获取优化建议”的权限检查通过（再被实例范围拦下）
	only := Principal{ID: 4, Username: "nobody"}
	if _, err := s.Review(context.Background(), only, ReviewParams{InstanceID: 1}); err == nil {
		t.Fatal("Review 应校验权限")
	}
	if _, err := s.Advise(context.Background(), only, AdviseParams{InstanceID: 1, SQL: "select 1"}); err == nil {
		t.Fatal("没有实例范围应被拒绝")
	} else {
		wantErr(t, err, 403, "未关联")
	}
}

func TestInstancesScopeAndFilter(t *testing.T) {
	s, _ := fixture()
	names := func(list []InstanceInfo) string {
		var n []string
		for _, i := range list {
			n = append(n, i.Name)
		}
		return strings.Join(n, ",")
	}
	// 查慢日志：开启了采集开关、类型受支持；管理员看全部，普通用户按资源组
	l, err := s.Instances(root, "logs")
	if err != nil || names(l) != "polar,prod-mysql-01,prod-mysql-02,report-doris" {
		t.Fatalf("root logs: %v %v", names(l), err)
	}
	l, _ = s.Instances(bob, "logs")
	if names(l) != "prod-mysql-01,report-doris" {
		t.Fatalf("bob logs: %v", names(l))
	}
	l, _ = s.Instances(all, "logs")
	if len(l) != 4 {
		t.Fatalf("query_all_instances 应看到全部: %v", names(l))
	}
	// 慢查日志：redis 带提示，pgsql 不出现，未开采集开关的 mysql 也出现
	q, _ := s.Instances(root, "query")
	got := map[string]InstanceInfo{}
	for _, i := range q {
		got[i.Name] = i
	}
	if _, ok := got["pg-1"]; ok {
		t.Error("pgsql 不应出现在慢查日志实例里")
	}
	if got["redis-01"].Hint == "" || got["redis-01"].Source != "" {
		t.Errorf("redis 应带“暂不支持”提示: %+v", got["redis-01"])
	}
	if got["old-mysql"].Name == "" || got["report-doris"].Source != SourceCollected || got["prod-mysql-01"].Source != "auto" {
		t.Errorf("实例数据源: %+v", got)
	}
	if _, err := s.Instances(root, "x"); err == nil {
		t.Error("未知页面类型应报错")
	}
}

func TestGroupsValidation(t *testing.T) {
	s, r := fixture()
	cases := []struct {
		name string
		in   GroupParams
		msg  string
	}{
		{"只有开始", GroupParams{Start: "2026-10-01"}, "同时提供"},
		{"日期格式", GroupParams{Start: "2026/10/01", End: "2026-10-02"}, "开始日期格式"},
		{"结束格式", GroupParams{Start: "2026-10-01", End: "x"}, "结束日期格式"},
		{"倒序", GroupParams{Start: "2026-10-05", End: "2026-10-01"}, "不能早于"},
		{"范围过长", GroupParams{Start: "2026-01-01", End: "2026-10-01"}, "最长 93 天"},
		{"每页过大", GroupParams{Limit: 201}, "每页条数"},
		{"翻页过深", GroupParams{Offset: MaxOffset + 1}, "翻页过深"},
		{"排序字段", GroupParams{Sort: "sql_text; drop table x"}, "不支持按"},
		{"排序方向", GroupParams{Order: "sideways"}, "排序方向"},
		{"搜索过长", GroupParams{Search: strings.Repeat("a", MaxSearchLen+1)}, "过长"},
		{"他人实例", GroupParams{InstanceID: 2}, "未关联"},
		{"实例不存在", GroupParams{InstanceID: 99}, "实例不存在"},
	}
	for _, c := range cases {
		_, err := s.Groups(bob, c.in)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: 期望含 %q，实际 %v", c.name, c.msg, err)
		}
	}
	if r.groupCalls != 0 {
		t.Errorf("校验失败不应查询数据库: %d", r.groupCalls)
	}
}

func TestGroupsDefaultsScopeAndShaping(t *testing.T) {
	s, r := fixture()
	long := strings.Repeat("x", maxTextRunes+10)
	r.groupRows = []SlowLogGroup{{InstanceID: 1, SQLText: "select * from shop.orders where a=1"}, {InstanceID: 1, SQLText: long}}
	r.groupTotal = 2

	// 没有给日期：默认最近 31 天（含今天）
	page, err := s.Groups(root, GroupParams{})
	if err != nil {
		t.Fatal(err)
	}
	if page.From != "2026-09-06" || page.To != "2026-10-06" {
		t.Errorf("默认范围: %s ~ %s", page.From, page.To)
	}
	q := r.lastGroup
	if q.From != "2026-09-06 00:00:00" || q.To != "2026-10-07 00:00:00" {
		t.Errorf("查询区间应为 [起始日 0 点, 结束日次日 0 点): %+v", q)
	}
	if q.InstanceIDs != nil || q.Limit != DefaultLimit || q.Sort != "exec_date" || !q.Desc {
		t.Errorf("管理员不限实例，默认分页/排序: %+v", q)
	}
	if page.Rows[0].TableName != "orders" || page.Rows[0].TextTruncated || !page.Rows[1].TextTruncated || len([]rune(page.Rows[1].SQLText)) != maxTextRunes {
		t.Errorf("主表提取与文本截断: %+v", page.Rows)
	}

	// 普通用户没选实例：只查自己可见的实例
	if _, err := s.Groups(bob, GroupParams{Search: "orders", Order: "asc", Sort: "count", Limit: 50, Offset: 100}); err != nil {
		t.Fatal(err)
	}
	q = r.lastGroup
	if len(q.InstanceIDs) != 2 || q.InstanceIDs[0] != 1 || q.InstanceIDs[1] != 3 || q.Desc || q.Sort != "count" || q.Limit != 50 || q.Offset != 100 || q.Search != "orders" {
		t.Errorf("范围/参数: %+v", q)
	}
	// 指定了实例：只查它
	if _, err := s.Groups(bob, GroupParams{InstanceID: 3}); err != nil || len(r.lastGroup.InstanceIDs) != 1 || r.lastGroup.InstanceIDs[0] != 3 {
		t.Errorf("指定实例: %v %+v", err, r.lastGroup)
	}
	// 没有任何可见实例：不查库，直接空页
	n := r.groupCalls
	page, err = s.Groups(Principal{ID: 4, Username: "x"}, GroupParams{})
	if err == nil {
		t.Fatalf("没有 slowlogs.view/slowquery.view 应被拒绝: %+v", page)
	}
	r.grant(4, "slowlogs.view", "slowquery.view")
	page, err = s.Groups(Principal{ID: 4, Username: "x"}, GroupParams{})
	if err != nil || page.Total != 0 || len(page.Rows) != 0 || r.groupCalls != n {
		t.Errorf("无可见实例应直接返回空页且不查库: %v %+v calls=%d", err, page, r.groupCalls-n)
	}
	// 仓库报错：原样返回（不吞）
	r.err = errBoom
	if _, err := s.Groups(root, GroupParams{}); !errors.Is(err, errBoom) {
		t.Errorf("仓库错误应透传: %v", err)
	}
}

func TestDetail(t *testing.T) {
	s, r := fixture()
	r.detailRows = []SlowLogRow{{ID: 1, SQLText: "select 1"}}
	rows, err := s.Detail(bob, DetailParams{InstanceID: 1, DB: "shop", MD5: "abc123", Day: "2026-10-06"})
	if err != nil || len(rows) != 1 {
		t.Fatal(err, rows)
	}
	q := r.lastDetail
	if q.From != "2026-10-06 00:00:00" || q.To != "2026-10-07 00:00:00" || q.Limit != DetailRows || q.DB != "shop" || q.MD5 != "abc123" {
		t.Errorf("明细查询: %+v", q)
	}
	for name, in := range map[string]DetailParams{
		"缺日期":     {InstanceID: 1, MD5: "abc"},
		"日期非法":    {InstanceID: 1, MD5: "abc", Day: "2026-13-40"},
		"指纹非法":    {InstanceID: 1, MD5: "a'b", Day: "2026-10-06"},
		"库名含控制字符": {InstanceID: 1, DB: "a\nb", Day: "2026-10-06"},
		"无实例":     {Day: "2026-10-06"},
		"越权实例":    {InstanceID: 2, Day: "2026-10-06"},
	} {
		if _, err := s.Detail(bob, in); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
	// 指纹/库为空是合法的（对应 NULL 分组）
	if _, err := s.Detail(bob, DetailParams{InstanceID: 1, Day: "2026-10-06"}); err != nil {
		t.Error(err)
	}
}

func TestPickSource(t *testing.T) {
	s, r := fixture()
	ctx := context.Background()
	review := func(inst uint, source string) (*ReviewPage, error) {
		return s.Review(ctx, root, ReviewParams{InstanceID: inst, Source: source, Start: "2026-10-01", End: "2026-10-06"})
	}
	r.collReview = []ReviewRow{{SQLID: "m1", SQLText: "SELECT * FROM t WHERE id = 42 AND name = 'x'", ExecCount: 3}}
	r.digReview = []ReviewRow{{SQLID: "c1", SQLText: "select * from t where id=?"}}

	// redis / 未知类型
	_, err := review(4, "")
	wantErr(t, err, 400, "Redis")
	_, err = review(6, "")
	wantErr(t, err, 400, "暂不支持 pgsql")
	// 平台类型只能用平台采集
	p, err := review(3, "")
	if err != nil || p.Source != SourceCollected || p.Label == "" {
		t.Fatalf("doris: %v %+v", err, p)
	}
	_, err = review(3, "digest")
	wantErr(t, err, 400, "只有平台采集")
	// 非法数据源
	_, err = review(1, "aliyun")
	wantErr(t, err, 400, "不支持的数据源")
	// mysql 自动：没有 digest 表 -> 平台采集；有 -> digest
	p, _ = review(1, "")
	if p.Source != SourceCollected {
		t.Errorf("无 digest 表应回退到平台采集: %s", p.Source)
	}
	if p.Rows[0].SQLText != "select * from t where id=? and name=?" {
		t.Errorf("平台采集的统计应展示指纹: %q", p.Rows[0].SQLText)
	}
	r.digestOK = true
	p, _ = review(1, "")
	if p.Source != SourceDigest || p.Rows[0].SQLID != "c1" || r.lastReview.HostKey != "10.0.0.1:3306" {
		t.Errorf("有 digest 表应使用它，并按 host:port 过滤: %+v %+v", p, r.lastReview)
	}
	p, _ = review(1, "collected")
	if p.Source != SourceCollected {
		t.Errorf("手动指定平台采集: %s", p.Source)
	}
	r.digestOK = false
	_, err = review(1, "digest")
	wantErr(t, err, 409, "dbt_mysql_slow_query_review")
}

func TestReviewValidationAndParams(t *testing.T) {
	s, r := fixture()
	ctx := context.Background()
	base := ReviewParams{InstanceID: 1, Start: "2026-10-01", End: "2026-10-06"}
	bad := map[string]func(*ReviewParams){
		"排序":    func(p *ReviewParams) { p.Sort = "password" },
		"方向":    func(p *ReviewParams) { p.Order = "x" },
		"每页":    func(p *ReviewParams) { p.Limit = 5000 },
		"偏移":    func(p *ReviewParams) { p.Offset = -1 },
		"指纹":    func(p *ReviewParams) { p.SQLID = "x y" },
		"库名":    func(p *ReviewParams) { p.DB = strings.Repeat("d", 65) },
		"范围":    func(p *ReviewParams) { p.Start = "2025-01-01" },
		"搜索":    func(p *ReviewParams) { p.Search = strings.Repeat("s", 201) },
		"无实例":   func(p *ReviewParams) { p.InstanceID = 0 },
		"越权":    func(p *ReviewParams) { p.InstanceID = 2 },
		"不存在":   func(p *ReviewParams) { p.InstanceID = 42 },
		"只给了开始": func(p *ReviewParams) { p.End = "" },
	}
	for name, mod := range bad {
		p := base
		mod(&p)
		if _, err := s.Review(ctx, bob, p); err == nil {
			t.Errorf("%s 应报错", name)
		}
		if _, err := s.History(ctx, bob, p); err == nil {
			t.Errorf("%s（明细）应报错", name)
		}
	}
	in := base
	in.DB, in.Search, in.Sort, in.Order, in.Limit, in.Offset = "shop", "orders", "query_time_avg", "asc", 100, 200
	if _, err := s.Review(ctx, bob, in); err != nil {
		t.Fatal(err)
	}
	q := r.lastReview
	if q.DB != "shop" || q.Search != "orders" || q.Sort != "query_time_avg" || q.Desc || q.Limit != 100 || q.Offset != 200 || q.From != "2026-10-01 00:00:00" || q.To != "2026-10-07 00:00:00" {
		t.Errorf("统计查询参数: %+v", q)
	}
	// 默认：统计按执行次数倒序，明细按解析行数倒序；没给日期取最近 7 天
	if _, err := s.Review(ctx, bob, ReviewParams{InstanceID: 1}); err != nil {
		t.Fatal(err)
	}
	if q = r.lastReview; q.Sort != "exec_count" || !q.Desc || q.Limit != DefaultLimit || q.From != "2026-09-30 00:00:00" {
		t.Errorf("默认统计参数: %+v", q)
	}
	r.collHist = []HistoryRow{{ID: 1, SQLText: strings.Repeat("y", maxTextRunes+1)}}
	h, err := s.History(ctx, bob, ReviewParams{InstanceID: 1, SQLID: "ab12"})
	if err != nil {
		t.Fatal(err)
	}
	if q = r.lastHist; q.Sort != "parse_rows" || q.SQLID != "ab12" || !h.Rows[0].Truncated {
		t.Errorf("默认明细参数: %+v %+v", q, h.Rows[0])
	}
	if _, err := s.History(ctx, bob, ReviewParams{InstanceID: 1, Sort: "bogus"}); err == nil {
		t.Error("明细不支持的排序应报错")
	}
	// 统计的排序名不能用在明细上，反之亦然
	if _, err := s.History(ctx, bob, ReviewParams{InstanceID: 1, Sort: "exec_time_sum"}); err == nil {
		t.Error("exec_time_sum 不是明细的排序字段")
	}
}

func TestTrend(t *testing.T) {
	s, r := fixture()
	ctx := context.Background()
	r.digestOK = true
	r.trend = &Trend{Points: []TrendPoint{{T: "2026-10-05", Count: 4, Pct95: 1.5}}}
	tr, err := s.Trend(ctx, bob, ReviewParams{InstanceID: 1, SQLID: "abc", Start: "2026-10-01", End: "2026-10-06"})
	if err != nil || tr.Bucket != "day" || tr.Source != SourceDigest || len(tr.Points) != 1 || r.lastTrendSrc != SourceDigest {
		t.Fatalf("%v %+v", err, tr)
	}
	if r.lastTrend.HostKey != "10.0.0.1:3306" || r.lastTrend.From != "2026-10-01 00:00:00" || r.lastTrend.To != "2026-10-07 00:00:00" {
		t.Errorf("趋势查询必须带实例与时间范围: %+v", r.lastTrend)
	}
	if _, err := s.Trend(ctx, bob, ReviewParams{InstanceID: 1, Start: "2026-10-01", End: "2026-10-06"}); err == nil {
		t.Error("没有 SQL 指纹应报错")
	}
	if _, err := s.Trend(ctx, bob, ReviewParams{InstanceID: 1, SQLID: "abc", Bucket: "minute"}); err == nil {
		t.Error("未知粒度应报错")
	}
	if _, err := s.Trend(ctx, bob, ReviewParams{InstanceID: 1, SQLID: "abc", Bucket: "hour", Start: "2026-10-01", End: "2026-10-09"}); err == nil {
		t.Error("按小时统计超过 7 天应报错")
	}
	if tr, err = s.Trend(ctx, bob, ReviewParams{InstanceID: 1, SQLID: "abc", Bucket: "hour", Start: "2026-10-01", End: "2026-10-07"}); err != nil || tr.Bucket != "hour" {
		t.Errorf("7 天内按小时: %v", err)
	}
	r.trend = nil
	if tr, _ = s.Trend(ctx, bob, ReviewParams{InstanceID: 3, SQLID: "abc"}); tr.Points == nil || r.lastTrendSrc != SourceCollected {
		t.Errorf("无数据时返回空数组而不是 null: %+v", tr)
	}
}

func TestDatabases(t *testing.T) {
	s, r := fixture()
	r.dbs = []string{"shop", "report"}
	r.digestOK = true
	list, err := s.Databases(bob, ReviewParams{InstanceID: 1})
	if err != nil || len(list) != 2 || r.lastDigDBs != "10.0.0.1:3306" {
		t.Fatalf("%v %v %q", err, list, r.lastDigDBs)
	}
	if _, err := s.Databases(bob, ReviewParams{InstanceID: 2}); err == nil {
		t.Error("越权实例应报错")
	}
	r.dbs = nil
	if list, _ = s.Databases(bob, ReviewParams{InstanceID: 3}); list == nil {
		t.Error("应返回空数组")
	}
}

func TestAdvise(t *testing.T) {
	s, r := fixture()
	ctx := context.Background()
	fa := &fakeAdvisor{adv: &Advice{Source: "fake", Label: "假建议", Items: []AdviceItem{{Level: "info", Title: "t"}}}}
	s.advisor = fa

	adv, err := s.Advise(ctx, bob, AdviseParams{InstanceID: 1, DB: "shop", SQL: "  select * from t where id = 1  ", RowsExamined: 100, RowsSent: 1, QueryTime: 2.5})
	if err != nil || adv.Source != "fake" {
		t.Fatal(err, adv)
	}
	if fa.req.SQL != "select * from t where id = 1" || fa.req.InstanceName != "prod-mysql-01" || fa.req.DBType != "mysql" || fa.req.DB != "shop" || fa.req.RowsExamined != 100 {
		t.Errorf("传给 Advisor 的请求: %+v", fa.req)
	}
	if len(r.audits) != 1 || r.audits[0].UserName != "bob" || r.audits[0].Action != actionAdviseName {
		t.Fatalf("应写审计: %+v", r.audits)
	}
	if strings.Contains(r.audits[0].ExtraInfo, "select") || !strings.Contains(r.audits[0].ExtraInfo, "prod-mysql-01") {
		t.Errorf("审计里不能带 SQL 原文，但要有实例: %q", r.audits[0].ExtraInfo)
	}

	for name, c := range map[string]struct {
		p      Principal
		in     AdviseParams
		status int
		msg    string
	}{
		"无优化权限": {Principal{ID: 3, Username: "alice"}, AdviseParams{InstanceID: 1, SQL: "select 1"}, 403, "没有权限"},
		"越权实例":  {bob, AdviseParams{InstanceID: 2, SQL: "select 1"}, 403, "未关联"},
		"doris": {bob, AdviseParams{InstanceID: 3, SQL: "select 1"}, 400, "暂不支持优化建议"},
		"空 SQL": {bob, AdviseParams{InstanceID: 1, SQL: "  "}, 400, "选择需要优化"},
		"超长":    {bob, AdviseParams{InstanceID: 1, SQL: strings.Repeat("a", MaxAdviseSQL+1)}, 400, "过长"},
		"负数":    {bob, AdviseParams{InstanceID: 1, SQL: "select 1", RowsExamined: -1}, 400, "不能为负"},
		"库名非法":  {bob, AdviseParams{InstanceID: 1, SQL: "select 1", DB: "a\x00b"}, 400, "数据库名"},
		"实例不存在": {bob, AdviseParams{InstanceID: 99, SQL: "select 1"}, 404, "不存在"},
		"没选实例":  {bob, AdviseParams{SQL: "select 1"}, 400, "选择实例"},
	} {
		_, err := s.Advise(ctx, c.p, c.in)
		if err == nil {
			t.Errorf("%s 应报错", name)
			continue
		}
		wantErr(t, err, c.status, c.msg)
	}
	if len(r.audits) != 1 {
		t.Errorf("失败的请求不应写审计: %d", len(r.audits))
	}
	// 只要有 slowquery.advise 就可以，不要求同时有查看按钮
	r.grant(4, "slowquery.advise")
	r.userInsts[4] = map[uint]bool{1: true}
	if _, err := s.Advise(ctx, Principal{ID: 4, Username: "x"}, AdviseParams{InstanceID: 1, SQL: "select 1"}); err != nil {
		t.Errorf("slowquery.advise 应可用: %v", err)
	}
	// Advisor 失败：包装成 502；业务错误透传
	fa.err = errBoom
	_, err = s.Advise(ctx, bob, AdviseParams{InstanceID: 1, SQL: "select 1"})
	wantErr(t, err, 502, "生成优化建议失败")
	fa.err = badReq("某个业务错误")
	_, err = s.Advise(ctx, bob, AdviseParams{InstanceID: 1, SQL: "select 1"})
	wantErr(t, err, 400, "某个业务错误")
	// 默认 Advisor 是规则分析
	d, _ := fixture()
	adv, err = d.Advise(ctx, root, AdviseParams{InstanceID: 1, SQL: "select * from t"})
	if err != nil || adv.Source != "rule" || adv.Items[0].Code != "select_star" && adv.Items[0].Code != "no_where" {
		t.Errorf("默认规则建议: %v %+v", err, adv)
	}
}

func TestCollectorOverview(t *testing.T) {
	s, r := fixture()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)
	r.states[1] = CollectState{InstanceID: 1, Status: "ok", LastRunAt: now, LastRows: 7}
	r.states[3] = CollectState{InstanceID: 3, Status: "error", Error: "boom", LastRunAt: now}
	ov, err := s.CollectorOverview(bob)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Enabled || !ov.StateTable || len(ov.Items) != 2 || ov.Items[0].InstanceName != "prod-mysql-01" || ov.Items[0].LastRows != 7 || !strings.Contains(ov.Items[1].Error, "仅管理员可见") {
		t.Errorf("%+v", ov)
	}
	ov, _ = s.CollectorOverview(root)
	if len(ov.Items) != 4 {
		t.Fatalf("管理员看到全部开启采集的实例: %d", len(ov.Items))
	}
	for _, it := range ov.Items {
		if it.InstanceID == 2 && it.Status != "none" {
			t.Errorf("没有记录的实例状态应为 none: %+v", it)
		}
		if it.InstanceID == 3 && it.Error != "boom" {
			t.Errorf("管理员能看到完整错误: %+v", it)
		}
	}
	// 状态表不存在：回退到进程内状态；进程内更新的覆盖库里的
	r.stateOK = false
	c := NewCollector(CollectorConfig{Enabled: true, Interval: 30 * time.Second, Workers: 3}, r, newFakeSource(), nil)
	c.states[2] = CollectState{InstanceID: 2, Status: "ok", LastRunAt: now, LastRows: 1}
	s.collector = c
	ov, _ = s.CollectorOverview(root)
	if ov.StateTable || !ov.Enabled || ov.IntervalSec != 30 || ov.Workers != 3 {
		t.Errorf("采集器信息: %+v", ov)
	}
	for _, it := range ov.Items {
		if it.InstanceID == 2 && it.LastRows != 1 {
			t.Errorf("应使用进程内状态: %+v", it)
		}
	}
	if _, err := s.CollectorOverview(nobo); err == nil {
		t.Error("没有权限应报错")
	}
}

// 基础表结构安装时就会建出 dbt_mysql_slow_query_review(_history)：表在不代表有数据。
// 自动选数据源时，历史表里没有该实例（host:port）的数据就应该用平台采集，否则“慢查日志”恒为空而“查慢日志”有数据。
func TestAutoSourceFallsBackToCollectedWhenDigestHasNoData(t *testing.T) {
	s, r := fixture()
	ctx := context.Background()
	r.digestOK = true // 表存在
	r.digestNoHost = map[string]bool{"10.0.0.1:3306": true, "10.0.0.2:3306": true}
	r.collReview = []ReviewRow{{SQLID: "c1", SQLText: "select * from t where id=1", ExecCount: 3}}
	r.digReview = []ReviewRow{{SQLID: "d1", SQLText: "select 1"}}
	in := ReviewParams{InstanceID: 1, Start: "2026-10-01", End: "2026-10-06"}

	p, err := s.Review(ctx, root, in)
	if err != nil || p.Source != SourceCollected || len(p.Rows) != 1 || p.Rows[0].SQLID != "c1" {
		t.Fatalf("digest 表存在但没有该实例的数据：应回退到平台采集: %v %+v", err, p)
	}
	// 手动指定 digest 仍然尊重
	in.Source = SourceDigest
	if p, err = s.Review(ctx, root, in); err != nil || p.Source != SourceDigest {
		t.Fatalf("手动指定 digest: %v %+v", err, p)
	}
	// 推送脚本开始写入后（历史表里有了这个实例）：自动选 digest
	in.Source = ""
	r.digestNoHost = map[string]bool{"10.0.0.2:3306": true}
	if p, err = s.Review(ctx, root, in); err != nil || p.Source != SourceDigest || p.Rows[0].SQLID != "d1" {
		t.Fatalf("历史表里有该实例的数据：应使用 digest: %v %+v", err, p)
	}
	// 其它端点（趋势、库列表）同一个判定
	r.digestNoHost = map[string]bool{"10.0.0.1:3306": true}
	if _, err := s.Trend(ctx, root, ReviewParams{InstanceID: 1, SQLID: "abc"}); err != nil || r.lastTrendSrc != SourceCollected {
		t.Fatalf("趋势应跟随回退到平台采集: %v %q", err, r.lastTrendSrc)
	}
}
