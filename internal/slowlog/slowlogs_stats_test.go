// @author: kevinkelly
// @license: Apache Licence
// @file: slowlogs_stats_test.go
// @time: 2026/10/28

package slowlog

import (
	"strings"
	"testing"
	"time"

	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// statsFake 在 fakeRepo 上加上 StatsRepo 能力。
type statsFake struct {
	*fakeRepo
	data  *StatsData
	last  *GroupQuery
	calls int
}

func (f *statsFake) SlowLogStats(q GroupQuery) (*StatsData, error) {
	f.calls++
	f.last = &q
	return f.data, f.fakeRepo.err
}

func statsFixture() (*Service, *statsFake) {
	_, base := fixture()
	f := &statsFake{fakeRepo: base}
	return NewService(f, Options{Now: func() time.Time { return time.Date(2026, 10, 6, 15, 4, 5, 0, time.Local) }}), f
}

func TestStatsFillsDaysAndFingerprint(t *testing.T) {
	s, f := statsFixture()
	f.data = &StatsData{Total: 7, Fingerprints: 3, TotalTime: 12.34567, MaxTime: 5.4321,
		Days:      []DayStat{{Day: "2026-10-05", Count: 3, TotalTime: 4.0004}, {Day: "2026-10-03", Count: 4, TotalTime: 8.3}},
		Slowest:   &SlowestEntry{InstanceID: 1, DBName: "shop", QueryTimeSec: 5.4321, Fingerprint: "SELECT * FROM orders WHERE id = 42 AND name = 'x'"},
		Databases: []string{"shop"}}
	st, err := s.Stats(root, GroupParams{Start: "2026-10-03", End: "2026-10-06"})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Days) != 4 || st.Days[0].Day != "2026-10-03" || st.Days[0].Count != 4 || st.Days[1].Count != 0 || st.Days[2].Count != 3 || st.Days[3].Day != "2026-10-06" || st.Days[3].TotalTime != 0 {
		t.Errorf("日期应补零且连续: %+v", st.Days)
	}
	if st.Total != 7 || st.Fingerprints != 3 || st.TotalTime != 12.346 || st.MaxTime != 5.432 {
		t.Errorf("KPI: %+v", st)
	}
	if st.Slowest == nil || st.Slowest.Fingerprint != "select * from orders where id=? and name=?" {
		t.Errorf("最慢单条应带 SQL 指纹: %+v", st.Slowest)
	}
	if f.last.From != "2026-10-03 00:00:00" || f.last.To != "2026-10-07 00:00:00" || f.last.InstanceIDs != nil {
		t.Errorf("查询条件: %+v", f.last)
	}
}

func TestStatsScopeAndValidation(t *testing.T) {
	s, f := statsFixture()
	f.data = &StatsData{}
	// 普通用户没选实例：只统计自己可见的实例
	if _, err := s.Stats(bob, GroupParams{DB: " shop ", Search: "orders"}); err != nil {
		t.Fatal(err)
	}
	q := f.last
	if len(q.InstanceIDs) != 2 || q.InstanceIDs[0] != 1 || q.InstanceIDs[1] != 3 || q.DB != "shop" || q.Search != "orders" {
		t.Errorf("范围: %+v", q)
	}
	// 选了不可见的实例：拒绝，不查库
	n := f.calls
	if _, err := s.Stats(bob, GroupParams{InstanceID: 2}); err == nil {
		t.Fatal("不可见的实例应被拒绝")
	} else {
		wantErr(t, err, 403, "未关联")
	}
	// 没有 slowlogs.view/slowquery.view
	if _, err := s.Stats(nobo, GroupParams{}); err == nil {
		t.Fatal("没有菜单权限应被拒绝")
	}
	// 参数校验
	for _, in := range []GroupParams{{Start: "2026-10-01"}, {Start: "2026-01-01", End: "2026-10-01"}, {DB: strings.Repeat("a", 65)}, {Search: strings.Repeat("a", MaxSearchLen+1)}} {
		if _, err := s.Stats(root, in); err == nil {
			t.Errorf("应拒绝 %+v", in)
		}
	}
	if f.calls != n {
		t.Errorf("校验失败不应查库: %d", f.calls-n)
	}
	// 没有任何可见实例：不查库，返回补零的空统计
	r := f.fakeRepo
	r.grant(4, "slowlogs.view", "slowquery.view")
	st, err := s.Stats(Principal{ID: 4, Username: "x"}, GroupParams{Start: "2026-10-05", End: "2026-10-06"})
	if err != nil || st.Total != 0 || len(st.Days) != 2 || f.calls != n {
		t.Errorf("无可见实例: %v %+v calls=%d", err, st, f.calls-n)
	}
	// 仓库报错：透传
	r.err = errBoom
	if _, err := s.Stats(root, GroupParams{}); err == nil {
		t.Error("仓库错误应透传")
	}
}

func TestStatsUnsupportedRepo(t *testing.T) {
	s, _ := fixture() // fakeRepo 没有实现 StatsRepo
	_, err := s.Stats(root, GroupParams{})
	wantErr(t, err, 501, "不支持")
}

func TestGroupsDBFilterFingerprintAndTotalSort(t *testing.T) {
	s, r := fixture()
	r.groupRows = []SlowLogGroup{{InstanceID: 1, DBName: "shop", SQLText: "SELECT * FROM orders WHERE id = 42", QueryTimeSum: 12.5}}
	r.groupTotal = 1
	page, err := s.Groups(root, GroupParams{DB: " shop ", Sort: "total_time"})
	if err != nil {
		t.Fatal(err)
	}
	if r.lastGroup.DB != "shop" || r.lastGroup.Sort != "total_time" {
		t.Errorf("库筛选/总耗时排序应传给仓库: %+v", r.lastGroup)
	}
	if page.Rows[0].Fingerprint != "select * from orders where id=?" {
		t.Errorf("行内应带 SQL 指纹: %q", page.Rows[0].Fingerprint)
	}
	if _, err := s.Groups(root, GroupParams{DB: "a\nb"}); err == nil {
		t.Error("库名含换行应被拒绝")
	}
}

func TestBuildGroupsDBSearchAndTotalSort(t *testing.T) {
	q := GroupQuery{InstanceIDs: []uint{3}, DB: evil, Search: evil, From: "f", To: "t", Sort: "total_time", Desc: true, Limit: 10}
	list, la, count, ca := buildGroups(q)
	noEvil(t, "list", list)
	noEvil(t, "count", count)
	if countQ(list) != len(la) || countQ(count) != len(ca) {
		t.Fatalf("占位符与参数个数不一致: %d/%d %d/%d", countQ(list), len(la), countQ(count), len(ca))
	}
	// 参数顺序：时间范围、实例、库、搜索（一个参数覆盖 SQL/库/用户），然后 LIMIT/OFFSET
	if la[3] != evil || la[4] != likePattern(evil) || la[5] != 10 {
		t.Errorf("参数: %#v", la)
	}
	for _, want := range []string{"l.db_name = ?", "l.user_host", "SUM(l.query_time_sec)", "ORDER BY qt_sum DESC", "g.qt_sum"} {
		if !strings.Contains(list, want) {
			t.Errorf("缺少 %q:\n%s", want, list)
		}
	}
}

func TestBuildStats(t *testing.T) {
	q := GroupQuery{InstanceIDs: []uint{3, 5}, DB: evil, Search: evil, From: "2026-10-01 00:00:00", To: "2026-10-07 00:00:00"}
	for name, f := range map[string]func(GroupQuery) (string, []any){
		"summary": buildStatsSummary, "days": buildStatsDays, "slowest": buildStatsSlowest, "databases": buildStatsDatabases,
	} {
		sqlText, args := f(q)
		noEvil(t, name, sqlText)
		if countQ(sqlText) != len(args) {
			t.Errorf("%s 占位符 %d 与参数 %d 不一致:\n%s", name, countQ(sqlText), len(args), sqlText)
		}
		if !strings.Contains(sqlText, "FROM dbt_slow_logs l") {
			t.Errorf("%s: %s", name, sqlText)
		}
	}
	// 库下拉不带库与搜索条件：选了某个库后下拉里仍有其他库
	sqlText, args := buildStatsDatabases(q)
	if strings.Contains(sqlText, "db_name = ?") || strings.Contains(sqlText, "CONCAT_WS") || len(args) != 2+2+1 {
		t.Errorf("databases: %s %v", sqlText, args)
	}
	sqlText, args = buildStatsDays(q)
	if args[len(args)-1] != MaxSpanDays+1 || !strings.Contains(sqlText, "GROUP BY d") {
		t.Errorf("days: %s %v", sqlText, args)
	}
}

func TestHTTPStatsEndpoint(t *testing.T) {
	svc, f := statsFixture()
	f.data = &StatsData{Total: 2, Days: []DayStat{{Day: "2026-10-06", Count: 2, TotalTime: 3}}}
	f.grant(2, "slowlogs.view", "slowquery.view")
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) { register(s, rt, svc) },
	}})
	const path = "/api/v1/slowlog/slowlogs/stats?start=2026-10-05&end=2026-10-06&db_name=shop"
	if w := env.Anonymous("GET", path); w.Code != 401 {
		t.Errorf("未登录应 401: %d", w.Code)
	}
	grantServer(env, 2, "slowlogs.view", "slowquery.view")
	bob := env.Login("bob")
	var st SlowLogStats
	bob.GetJSON(path, &st)
	if st.Total != 2 || len(st.Days) != 2 || st.Days[1].Count != 2 {
		t.Errorf("统计: %+v", st)
	}
	if f.last == nil || f.last.DB != "shop" {
		t.Errorf("db_name 应传到仓库: %+v", f.last)
	}
	if code, _ := call(t, bob, "GET", "/api/v1/slowlog/slowlogs/stats?instance_id=-1", nil); code != 400 {
		t.Errorf("非法实例 ID 应 400: %d", code)
	}
}

func TestLogsPageOptimizeLink(t *testing.T) {
	env, _ := newEnv(t)
	root := env.Login("root")
	code, body := root.Page("/slowlogs")
	for _, want := range []string{`id="sl-db"`, `id="sl-kpis"`, `id="sl-chart-count"`, `id="sl-chart-time"`, `data-can-opt="1"`, "/static/css/slowlog_logs.css"} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("/slowlogs 缺少 %q (%d)", want, code)
		}
	}
	grantServer(env, 2, "slowlogs.view", "slowquery.view")
	bob := env.Login("bob")
	if code, body = bob.Page("/slowlogs"); code != 200 || !strings.Contains(body, `data-can-opt=""`) {
		t.Errorf("没有优化工具权限不应显示链接 (%d)", code)
	}
}
