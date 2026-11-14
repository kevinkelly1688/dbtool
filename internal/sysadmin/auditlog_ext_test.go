// @author: kevinkelly
// @license: Apache Licence
// @file: auditlog_ext_test.go
// @time: 2026/10/28

package sysadmin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// extFake 在 fakeRepo 之上实现审计扩展的可选接口（auditExtRepo / queryAuditRepo / sqlStatsRepo）。
type extFake struct {
	*fakeRepo
	extReady, rejectReady bool
	ext                   map[uint]AuditRowExt // audit id -> 扩展字段（只用 Result/IP/Category/Module/Code/Risky）
	rejects               []RejectRow
	overview              AuditOverviewRaw
	overviewCalls         int
}

func newExtFake() (*Service, *extFake) {
	s, f := setup()
	e := &extFake{fakeRepo: f, extReady: true, rejectReady: true, ext: map[uint]AuditRowExt{}}
	s2 := NewService(e)
	s2.Iterations = s.Iterations
	s2.now = s.now
	return s2, e
}

func (e *extFake) AuditExtReady() bool    { return e.extReady }
func (e *extFake) QueryRejectReady() bool { return e.rejectReady }

func (e *extFake) rows() []AuditRowExt {
	all, _, _ := e.fakeRepo.ListAuditLogs(AuditLogFilter{Limit: 1 << 20})
	out := make([]AuditRowExt, len(all))
	for i, a := range all {
		r := AuditRowExt{AuditEntry: a}
		if x, ok := e.ext[a.ID]; ok {
			r.Result, r.IP, r.Category, r.Module, r.Code, r.Risky = x.Result, x.IP, x.Category, x.Module, x.Code, x.Risky
		}
		out[i] = r
	}
	return out
}

func (e *extFake) ListAuditLogsExt(f AuditLogFilter, x AuditExtFilter) ([]AuditRowExt, int64, error) {
	base, _, _ := e.fakeRepo.ListAuditLogs(AuditLogFilter{Action: f.Action, User: f.User, Search: f.Search, Start: f.Start, End: f.End, Limit: 1 << 20})
	var out []AuditRowExt
	for _, a := range base {
		r := AuditRowExt{AuditEntry: a}
		if v, ok := e.ext[a.ID]; ok {
			r.Result, r.IP, r.Category, r.Module, r.Code, r.Risky = v.Result, v.IP, v.Category, v.Module, v.Code, v.Risky
		}
		if (x.Category != "" && r.Category != x.Category) || (x.Result != "" && r.Result != x.Result) || (x.Risky && !r.Risky) {
			continue
		}
		out = append(out, r)
	}
	return page(out, f.Offset, f.Limit), int64(len(out)), nil
}

func (e *extFake) GetAuditRowExt(id uint) (*AuditRowExt, error) {
	for _, r := range e.rows() {
		if r.ID == id {
			return &r, nil
		}
	}
	return nil, nil
}

func (e *extFake) RecentAuditsByUser(username string, excludeID uint, limit int) ([]AuditRowExt, error) {
	var out []AuditRowExt
	for _, r := range e.rows() {
		if r.UserName == username && r.ID != excludeID {
			out = append(out, r)
		}
	}
	return page(out, 0, limit), nil
}

func (e *extFake) AuditOverviewData(time.Time) (*AuditOverviewRaw, error) {
	e.overviewCalls++
	o := e.overview
	return &o, nil
}

func (e *extFake) ListQueryRejects(f QueryLogFilter, limit int) ([]RejectRow, int64, error) {
	var out []RejectRow
	for _, r := range e.rejects {
		if f.User != "" && r.UserName != f.User {
			continue
		}
		if f.Instance != "" && r.InstanceName != f.Instance {
			continue
		}
		if f.Search != "" && !strings.Contains(r.SQLText+r.Reason, f.Search) {
			continue
		}
		out = append(out, r)
	}
	n := int64(len(out))
	return page(out, 0, limit), n, nil
}

func (e *extFake) QueryAuditStats(f QueryLogFilter) (*QueryStatsRaw, error) {
	_, n, _ := e.fakeRepo.ListQueryLogs(QueryLogFilter{User: f.User, Instance: f.Instance, Search: f.Search, Limit: 1 << 20})
	_, rej, _ := e.ListQueryRejects(f, 1<<20)
	return &QueryStatsRaw{Total: n, Rejected: rej, AvgCost: 0.5, MaxCost: 2.5, MaxUser: "dba", UserCount: 2}, nil
}

func (e *extFake) GetQueryLog(id uint) (*model.QueryLog, error) {
	for _, l := range e.fakeRepo.queryLogs {
		if l.ID == id {
			return &l, nil
		}
	}
	return nil, nil
}

func (e *extFake) GetQueryReject(id uint) (*RejectRow, error) {
	for _, r := range e.rejects {
		if r.ID == id {
			return &r, nil
		}
	}
	return nil, nil
}

func (e *extFake) SQLWorkflowStatusCounts(f SQLWorkflowFilter) (map[string]int64, error) {
	out := map[string]int64{}
	for _, w := range e.fakeRepo.workflows {
		if f.Status == "" || w.Status == f.Status {
			out[w.Status]++
		}
	}
	return out, nil
}

func seedAudits(f *fakeRepo) {
	mk := func(day int, user, action string) {
		_ = f.InsertAuditLog(&model.AuditEntry{UserID: 1, UserName: user, UserDisplay: user + "名", Action: action, ExtraInfo: "x",
			ActionTime: time.Date(2026, 10, day, 10, 0, 0, 0, time.Local)})
	}
	mk(1, "wang", "登入失败")
	mk(2, "dba1", "终止会话")
	mk(3, "dev", "登入")
	mk(4, "dba1", "修改审批流")
}

func TestAuditListWithExtFiltersAndFallback(t *testing.T) {
	s, e := newExtFake()
	seedAudits(e.fakeRepo)
	e.ext[1] = AuditRowExt{Result: audit.ResultFailed, IP: "10.0.0.1", Category: audit.CatLogin, Module: "登录认证", Code: "auth.login"}
	e.ext[2] = AuditRowExt{Result: audit.ResultOK, IP: "10.0.0.9", Category: audit.CatInst, Module: "会话管理", Code: "session.kill", Risky: true}
	// 3、4 是历史行：没有扩展信息

	p, err := s.ListAuditLogs(AuditQuery{})
	if err != nil || p.Total != 4 {
		t.Fatalf("%+v %v", p, err)
	}
	var kill AuditLogItem
	for _, r := range p.Rows {
		if r.ID == 2 {
			kill = r
		}
	}
	if kill.IP != "10.0.0.9" || kill.Result != "成功" || !kill.Risky || kill.CategoryLabel != "实例管理" || kill.CodeLabel != "终止会话" || kill.Module != "会话管理" {
		t.Fatalf("扩展字段应填充: %+v", kill)
	}
	if hist := p.Rows[0]; hist.ID != 4 || hist.Result != "" || hist.IP != "" || hist.Risky {
		t.Fatalf("历史行扩展字段应为空: %+v", hist)
	}
	for _, c := range []struct {
		q    AuditQuery
		want int64
	}{
		{AuditQuery{Category: audit.CatLogin}, 1},
		{AuditQuery{Result: "失败"}, 1},
		{AuditQuery{Risky: true}, 1},
		{AuditQuery{Category: audit.CatInst, Risky: true}, 1},
		{AuditQuery{Category: audit.CatInst, Result: "失败"}, 0},
	} {
		if p, err := s.ListAuditLogs(c.q); err != nil || p.Total != c.want {
			t.Errorf("%+v: total=%v err=%v 期望 %d", c.q, p, err, c.want)
		}
	}
	for _, q := range []AuditQuery{{Category: "nope"}, {Result: "挂了"}} {
		if _, err := s.ListAuditLogs(q); err == nil || httpCode(t, err) != 400 {
			t.Errorf("%+v 应 400: %v", q, err)
		}
	}

	// 扩展表不可用：不带扩展筛选时照常，带了得 409
	e.extReady = false
	if p, err := s.ListAuditLogs(AuditQuery{}); err != nil || p.Total != 4 || p.Rows[0].Result != "" {
		t.Fatalf("退化到基础行为: %+v %v", p, err)
	}
	if _, err := s.ListAuditLogs(AuditQuery{Risky: true}); err == nil || httpCode(t, err) != 409 {
		t.Fatalf("扩展表不可用时按高危筛选应 409: %v", err)
	}
	if o, _ := s.AuditOptions(); o.ExtReady || len(o.Categories) != len(audit.Categories) || len(o.Results) != 3 {
		t.Fatalf("%+v", o)
	}
}

func TestAuditExportCarriesExtAndFilters(t *testing.T) {
	s, e := newExtFake()
	seedAudits(e.fakeRepo)
	e.ext[2] = AuditRowExt{Result: audit.ResultOK, IP: "10.0.0.9", Category: audit.CatInst, Code: "session.kill", Risky: true}
	rows, _, err := s.ExportAuditLogs(alice, AuditQuery{Risky: true})
	if err != nil || len(rows) != 1 || rows[0].IP != "10.0.0.9" {
		t.Fatalf("%+v %v", rows, err)
	}
	if a := lastAudit(t, e.fakeRepo); !strings.Contains(a.ExtraInfo, "仅高危=true") {
		t.Fatalf("导出审计应记录扩展筛选条件: %q", a.ExtraInfo)
	}
}

func TestAuditOverviewAndDetail(t *testing.T) {
	s, e := newExtFake()
	seedAudits(e.fakeRepo)
	e.ext[2] = AuditRowExt{Result: audit.ResultOK, IP: "10.0.0.9", Category: audit.CatInst, Code: "session.kill", Risky: true}
	e.overview = AuditOverviewRaw{Days: []DayCount{{"2026-10-05", 3}}, Events7: 9, LoginFailed: 2, Lockouts: 1, Risky7: 4, Denied7: -1,
		TopUsers: []UserCount{{"dba1", "DBA", 5}}}
	o, err := s.AuditOverview()
	if err != nil || !o.ExtReady || o.Events7 != 9 || o.RiskyCount == nil || *o.RiskyCount != 4 || o.DeniedCount != nil || len(o.TopUsers) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
	if _, _ = s.AuditOverview(); e.overviewCalls != 1 {
		t.Fatalf("概览应缓存 1 分钟: %d 次", e.overviewCalls)
	}
	s.now = func() time.Time { return fixedNow.Add(2 * time.Minute) }
	if _, _ = s.AuditOverview(); e.overviewCalls != 2 {
		t.Fatalf("缓存过期后应重新统计: %d", e.overviewCalls)
	}

	d, err := s.AuditDetail(2)
	if err != nil || d.Item.Code != "session.kill" || len(d.Recent) != 1 || d.Recent[0].ID != 4 || d.Hint != "" {
		t.Fatalf("%+v %v", d, err)
	}
	if d, _ := s.AuditDetail(1); d.Hint == "" {
		t.Fatal("登录失败应带说明")
	}
	if _, err := s.AuditDetail(999); err == nil || httpCode(t, err) != 404 {
		t.Fatalf("%v", err)
	}
	// 不支持扩展的仓库：概览为空，不报错
	plain, _ := setup()
	if o, err := plain.AuditOverview(); err != nil || o.ExtReady || len(o.Days) != 0 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestQueryAuditMergesRejectedStatements(t *testing.T) {
	s, e := newExtFake()
	t0 := fixedNow
	e.fakeRepo.queryLogs = []model.QueryLog{
		{ID: 2, InstanceName: "prod-1", DBName: "shop", SQLLog: "select 2", Username: "dev", UserDisplay: "开发", CreateTime: t0.Add(-1 * time.Minute), CostTime: "0.1", Masking: true},
		{ID: 1, InstanceName: "prod-1", DBName: "shop", SQLLog: "select 1", Username: "dba", UserDisplay: "DBA", CreateTime: t0.Add(-5 * time.Minute), CostTime: "0.2"},
	}
	long := strings.Repeat("y", 1500)
	e.rejects = []RejectRow{
		{ID: 11, UserName: "bob", InstanceName: "prod-1", DBName: "shop", SQLText: "delete from t", Reason: "仅允许只读查询", CreatedAt: t0.Add(-2 * time.Minute)},
		{ID: 10, UserName: "alice", InstanceName: "prod-2", DBName: "d", SQLText: long, Reason: "无库表权限", CreatedAt: t0.Add(-9 * time.Minute)},
	}
	p, err := s.ListQueryLogs(QueryAuditQuery{})
	if err != nil || p.Total != 4 || !p.RejectReady || len(p.Rows) != 4 {
		t.Fatalf("%+v %v", p, err)
	}
	var got []string
	for _, r := range p.Rows {
		got = append(got, r.Kind+":"+itoaU(r.ID))
	}
	if strings.Join(got, ",") != "ok:2,rejected:11,ok:1,rejected:10" {
		t.Fatalf("应按时间倒序合并: %v", got)
	}
	if r := p.Rows[0]; r.Status != "masked" || !r.Masking {
		t.Fatalf("脱敏记录: %+v", r)
	}
	if r := p.Rows[1]; r.Status != "rejected" || r.Reason != "仅允许只读查询" || r.UserDisplay != "bob名" || r.CostTime != "" {
		t.Fatalf("拒绝记录: %+v", r)
	}
	if r := p.Rows[3]; !r.SQLTruncated || len([]rune(r.SQL)) != 1000 {
		t.Fatalf("长 SQL 应截断: %+v", r.SQLTruncated)
	}
	// 分页：第二页只剩两条
	p, _ = s.ListQueryLogs(QueryAuditQuery{Page: 2, PageSize: 2})
	if len(p.Rows) != 2 || p.Rows[0].Kind != "ok" || p.Rows[0].ID != 1 || p.Pages != 2 {
		t.Fatalf("%+v", p)
	}
	// 仅被拒绝 / 筛选
	if p, _ = s.ListQueryLogs(QueryAuditQuery{OnlyRejected: true}); p.Total != 2 || p.Rows[0].Kind != "rejected" {
		t.Fatalf("%+v", p)
	}
	if p, _ = s.ListQueryLogs(QueryAuditQuery{Instance: "prod-2"}); p.Total != 1 || p.Rows[0].ID != 10 {
		t.Fatalf("%+v", p)
	}
	if _, err := s.ListQueryLogs(QueryAuditQuery{Page: 600, PageSize: 20}); err == nil || httpCode(t, err) != 400 {
		t.Fatalf("翻页过深应 400: %v", err)
	}

	// 详情：完整 SQL 不截断
	d, err := s.QueryAuditDetail("rejected", 10)
	if err != nil || d.SQLTruncated || len([]rune(d.SQLFull)) != 1500 || d.Reason != "无库表权限" {
		t.Fatalf("%+v %v", d, err)
	}
	if d, err = s.QueryAuditDetail("ok", 1); err != nil || d.SQLFull != "select 1" {
		t.Fatalf("%+v %v", d, err)
	}
	for _, c := range []struct {
		kind string
		id   uint
		code int
	}{{"ok", 99, 404}, {"rejected", 99, 404}, {"weird", 1, 400}} {
		if _, err := s.QueryAuditDetail(c.kind, c.id); err == nil || httpCode(t, err) != c.code {
			t.Errorf("%+v: %v", c, err)
		}
	}
	// 汇总
	st, err := s.QueryAuditStats(QueryAuditQuery{})
	if err != nil || st.Total != 4 || st.Rejected != 2 || !st.RejectReady || st.MaxUser != "dba" {
		t.Fatalf("%+v %v", st, err)
	}

	// 拒绝记录表不存在：只显示成功记录，“仅被拒绝”得到空列表
	e.rejectReady = false
	if p, err = s.ListQueryLogs(QueryAuditQuery{}); err != nil || p.Total != 2 || p.RejectReady {
		t.Fatalf("%+v %v", p, err)
	}
	if p, _ = s.ListQueryLogs(QueryAuditQuery{OnlyRejected: true}); p.Total != 0 || len(p.Rows) != 0 || p.RejectReady {
		t.Fatalf("%+v", p)
	}
	if _, err := s.QueryAuditDetail("rejected", 10); err == nil || httpCode(t, err) != 404 {
		t.Fatalf("%v", err)
	}
}

func itoaU(n uint) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestSQLWorkflowStats(t *testing.T) {
	s, e := newExtFake()
	e.fakeRepo.workflows = []fakeWorkflow{
		{SQLWorkflowRow{ID: 1, Status: model.WfFinish}, 1}, {SQLWorkflowRow{ID: 2, Status: model.WfFinish}, 1},
		{SQLWorkflowRow{ID: 3, Status: model.WfManReviewing}, 1}, {SQLWorkflowRow{ID: 4, Status: "weird_status"}, 1},
	}
	list, err := s.SQLWorkflowStats(SQLWorkflowQuery{})
	if err != nil || len(list) != 3 {
		t.Fatalf("%+v %v", list, err)
	}
	if list[0].Status != model.WfManReviewing || list[0].Label != "等待审批" || list[1].Count != 2 || list[2].Status != "weird_status" {
		t.Fatalf("应按流转顺序排列、未知状态排最后: %+v", list)
	}
	if _, err := s.SQLWorkflowStats(SQLWorkflowQuery{Status: "bogus"}); err == nil || httpCode(t, err) != 400 {
		t.Fatalf("%v", err)
	}
}

func TestAuditExtHTTP(t *testing.T) {
	svc, e := newExtFake()
	seedAudits(e.fakeRepo)
	e.ext[2] = AuditRowExt{Result: audit.ResultOK, IP: "10.0.0.9", Category: audit.CatInst, Module: "会话管理", Code: "session.kill", Risky: true}
	e.overview = AuditOverviewRaw{Events7: 7, Risky7: 1, Denied7: 0}
	e.fakeRepo.queryLogs = []model.QueryLog{{ID: 1, InstanceName: "prod-1", SQLLog: "select 1", Username: "dba", CreateTime: fixedNow}}
	e.rejects = []RejectRow{{ID: 5, UserName: "bob", SQLText: "drop table t", Reason: "非只读", CreatedAt: fixedNow}}
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){registerWith(svc)}})
	root, bob := env.Login("root"), env.Login("bob")

	if code, _, _ := call(t, bob, "GET", "/api/v1/audit/overview", nil); code != 403 {
		t.Fatalf("普通用户不能读审计概览: %d", code)
	}
	code, _, body := call(t, root, "GET", "/api/v1/audit/overview", nil)
	if code != 200 || !strings.Contains(body, `"events_7d":7`) || !strings.Contains(body, `"risky_7d":1`) || !strings.Contains(body, `"denied_7d":0`) {
		t.Fatalf("概览: %d %s", code, body)
	}
	code, _, body = call(t, root, "GET", "/api/v1/audit/log?risky=1&category=inst", nil)
	if code != 200 || !strings.Contains(body, `"total":1`) || !strings.Contains(body, `"ip":"10.0.0.9"`) || !strings.Contains(body, `"code_label":"终止会话"`) {
		t.Fatalf("按扩展筛选: %d %s", code, body)
	}
	if code, _, _ = call(t, root, "GET", "/api/v1/audit/log?category=bad", nil); code != 400 {
		t.Fatalf("非法类别应 400: %d", code)
	}
	if code, _, body = call(t, root, "GET", "/api/v1/audit/log/2", nil); code != 200 || !strings.Contains(body, `"recent"`) {
		t.Fatalf("详情: %d %s", code, body)
	}
	if code, _, _ = call(t, root, "GET", "/api/v1/audit/log/abc", nil); code != 400 {
		t.Fatalf("详情 id 非法: %d", code)
	}
	if code, _, _ = call(t, root, "GET", "/api/v1/audit/log/999", nil); code != 404 {
		t.Fatalf("详情不存在: %d", code)
	}
	// 导出仍是 /log/export，不被 /log/:id 吃掉；CSV 带扩展列
	w := root.Do("GET", "/api/v1/audit/log/export?risky=1", nil)
	csv := w.Body.String()
	if w.Code != 200 || !strings.HasPrefix(csv, "\xEF\xBB\xBF用户名,") || !strings.Contains(csv, "来源IP,类别,模块,权限码,动作,结果,高危,额外信息,操作时间") ||
		!strings.Contains(csv, "10.0.0.9,实例管理,会话管理,session.kill,终止会话,成功,是,") {
		t.Fatalf("CSV: %d %q", w.Code, csv)
	}
	// 查询审计
	code, _, body = call(t, root, "GET", "/api/v1/audit/query-logs?only_rejected=1", nil)
	if code != 200 || !strings.Contains(body, `"total":1`) || !strings.Contains(body, `"kind":"rejected"`) || !strings.Contains(body, `"reject_ready":true`) {
		t.Fatalf("仅被拒绝: %d %s", code, body)
	}
	if code, _, body = call(t, root, "GET", "/api/v1/audit/query-logs/stats", nil); code != 200 || !strings.Contains(body, `"rejected":1`) || !strings.Contains(body, `"total":2`) {
		t.Fatalf("汇总: %d %s", code, body)
	}
	if code, _, body = call(t, root, "GET", "/api/v1/audit/query-logs/rejected/5", nil); code != 200 || !strings.Contains(body, `"sql_full":"drop table t"`) {
		t.Fatalf("拒绝详情: %d %s", code, body)
	}
	if code, _, _ = call(t, root, "GET", "/api/v1/audit/query-logs/rejected/0", nil); code != 400 {
		t.Fatalf("id 非法: %d", code)
	}
	if code, _, body = call(t, root, "GET", "/api/v1/audit/sql-workflows/stats", nil); code != 200 {
		t.Fatalf("状态分布: %d %s", code, body)
	}
}
