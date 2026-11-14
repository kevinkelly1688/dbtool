// @author: kevinkelly
// @license: Apache Licence
// @file: ext_test.go
// @time: 2026/10/28

package privapply_test

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/privapply"
	"dbtool/internal/privapply/memrepo"
)

// extSetup 在 setup 的场景上打开旁路表（申请理由、授予来源），并让用户都是启用状态。
func extSetup() (*privapply.Service, *memrepo.Repo) {
	s, r, _ := setup()
	r.ExtOn = true
	for id, u := range r.Users {
		u.IsActive = true
		u.Display = map[uint]string{1: "开发者", 2: "组长", 3: "DBA", 9: "外人"}[id]
		r.Users[id] = u
	}
	return s, r
}

func withReason(req privapply.CreateReq) privapply.CreateReq {
	req.Reason = "排查订单报表数据口径问题"
	return req
}

func approveAll(t *testing.T, s *privapply.Service, id uint) {
	t.Helper()
	ctx := context.Background()
	if err := s.Review(ctx, leader, id, audit.ActionPass, "同意，注意脱敏"); err != nil {
		t.Fatal(err)
	}
	if err := s.Review(ctx, dba, id, audit.ActionPass, ""); err != nil {
		t.Fatal(err)
	}
}

func TestApplyReasonRequiredWhenExtReady(t *testing.T) {
	s, r := extSetup()
	ctx := context.Background()
	if !s.ReasonRequired() {
		t.Fatal("旁路表就绪时应要求填写理由")
	}
	for _, bad := range []string{"", "  ", "太短了", strings.Repeat("长", 501)} {
		req := tableReq()
		req.Reason = bad
		if _, err := s.Create(ctx, dev, req); code(err) != "bad_request" {
			t.Errorf("理由 %q 应被拒绝: %v", bad, err)
		}
	}
	if len(r.Applies) != 0 {
		t.Fatal("被拒绝的申请不应落库")
	}
	a, err := s.Create(ctx, dev, withReason(tableReq()))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.ApplyExtRows[a.ApplyID].Reason; got != "排查订单报表数据口径问题" {
		t.Errorf("理由应写入旁路表: %q", got)
	}
	d, err := s.Detail(dev, a.ApplyID)
	if err != nil || d.Reason != "排查订单报表数据口径问题" {
		t.Errorf("详情应显示理由: %v %+v", err, d)
	}
	l, _ := s.ListApplies(dev, privapply.ListQuery{Limit: 20})
	if len(l.Rows) != 1 || l.Rows[0].Reason == "" {
		t.Errorf("列表应带理由: %+v", l.Rows)
	}
}

func TestApplyReasonDegradesWithoutExtTable(t *testing.T) {
	s, r, _ := setup() // 旁路表没创建
	if s.ReasonRequired() {
		t.Fatal("旁路表没创建时不应要求理由")
	}
	a := mustCreate(t, s, tableReq())
	if len(r.ApplyExtRows) != 0 {
		t.Error("没有旁路表时不应写入")
	}
	d, err := s.Detail(dev, a.ApplyID)
	if err != nil || d.Reason != "" || len(d.Timeline) == 0 {
		t.Errorf("详情应仍可打开：%v %+v", err, d)
	}
	approveAll(t, s, a.ApplyID)
	d, _ = s.Detail(dev, a.ApplyID)
	if len(d.Granted) != 0 {
		t.Errorf("没有旁路表就没有已授予链接: %+v", d.Granted)
	}
	if res, err := s.ListPrivileges(dev, privapply.PrivQuery{Limit: 20}); err != nil || res.Total != 2 || res.Rows[0].Source != "" {
		t.Errorf("没有旁路表时权限列表应照常（来源为空）：%v %+v", err, res)
	}
}

func TestExpiredStatusIsDerived(t *testing.T) {
	s, r := extSetup()
	a, _ := s.Create(context.Background(), dev, withReason(tableReq()))
	approveAll(t, s, a.ApplyID)
	if d, _ := s.Detail(dev, a.ApplyID); d.EffStatus != audit.StatusPassed || d.StatusLabel != "审核通过" {
		t.Fatalf("未到期: %+v", d.StatusLabel)
	}
	// 有效期过去：显示“已过期”，数据库里的状态不变
	r.Applies[0].ValidDate = time.Now().AddDate(0, 0, -2)
	d, _ := s.Detail(dev, a.ApplyID)
	if d.EffStatus != privapply.StatusExpired || d.StatusLabel != "已过期" || r.Applies[0].Status != audit.StatusPassed {
		t.Errorf("过期: eff=%d label=%s db=%d", d.EffStatus, d.StatusLabel, r.Applies[0].Status)
	}
	if d.Reapply == "" {
		t.Error("已过期的申请应给出重新申请的预填参数")
	}
	last := d.Timeline[len(d.Timeline)-1]
	if !strings.Contains(last.Title, "到期") {
		t.Errorf("时间线应有到期节点: %+v", d.Timeline)
	}
	// 状态筛选：已过期 与 审核通过 互斥
	exp, _ := s.ListApplies(dev, privapply.ListQuery{Tab: "mine", Status: "expired", Limit: 20})
	ok, _ := s.ListApplies(dev, privapply.ListQuery{Tab: "mine", Status: "1", Limit: 20})
	if exp.Total != 1 || ok.Total != 0 || exp.Rows[0].StatusLabel != "已过期" {
		t.Errorf("筛选: expired=%d passed=%d", exp.Total, ok.Total)
	}
	old, _ := s.List(dev, "", 0, 20)
	if old.Rows[0].StatusLabel != "已过期" {
		t.Errorf("旧的列表接口也应显示已过期: %s", old.Rows[0].StatusLabel)
	}
}

func TestListTabsStatusSearchAndProgress(t *testing.T) {
	s, _ := extSetup()
	ctx := context.Background()
	a1, _ := s.Create(ctx, dev, withReason(tableReq()))
	r2 := withReason(tableReq())
	r2.TableList = []string{"refunds"}
	a2, _ := s.Create(ctx, dev, r2)

	// 申请人：默认页签“我的申请”，没有审批权限所以没有待办页签
	v, err := s.ListApplies(dev, privapply.ListQuery{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if v.Tab != privapply.TabMine || v.Total != 2 || v.CanTodo || v.CanAll || v.Counts.Mine != 2 {
		t.Errorf("申请人: tab=%s total=%d todo=%v all=%v counts=%+v", v.Tab, v.Total, v.CanTodo, v.CanAll, v.Counts)
	}
	if p := v.Rows[0].Progress; p == nil || p.Level != 1 || p.Total != 2 || p.Current != "组长" || !v.Rows[0].CanAbort {
		t.Errorf("审批进度/可撤回: %+v", v.Rows[0])
	}
	if v2, _ := s.ListApplies(dev, privapply.ListQuery{Tab: "todo", Limit: 20}); v2.Tab != privapply.TabMine {
		t.Errorf("没有审批权限请求待办页签应回落到我的申请: %s", v2.Tab)
	}

	// 组长：自己没有申请 → 默认落在待我审批
	v, _ = s.ListApplies(leader, privapply.ListQuery{Limit: 20})
	if v.Tab != privapply.TabTodo || v.Total != 2 || v.Counts.Todo != 2 || v.Counts.Mine != 0 || v.Counts.All != 2 || !v.CanTodo || !v.CanAll {
		t.Errorf("组长: tab=%s total=%d counts=%+v", v.Tab, v.Total, v.Counts)
	}
	if !v.Rows[0].CanReview || v.Rows[0].CanAbort {
		t.Errorf("待办行: %+v", v.Rows[0])
	}
	// 组长通过 a1 的第一级后，待办里只剩 a2；DBA 的待办是 a1
	if err := s.Review(ctx, leader, a1.ApplyID, audit.ActionPass, ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PendingReview(leader); n != 1 {
		t.Errorf("组长待办: %d", n)
	}
	if n, _ := s.PendingReview(dba); n != 1 {
		t.Errorf("DBA 待办: %d", n)
	}
	if n, _ := s.PendingReview(dev); n != 0 {
		t.Errorf("无审批权限: %d", n)
	}
	vd, _ := s.ListApplies(dba, privapply.ListQuery{Tab: "todo", Limit: 20})
	if vd.Total != 1 || vd.Rows[0].ApplyID != a1.ApplyID || vd.Rows[0].Progress.Level != 2 || vd.Rows[0].Progress.Current != "DBA" {
		t.Errorf("DBA 待办: %+v", vd.Rows)
	}
	// 全部页签：其他资源组的审批人看不到
	vo, _ := s.ListApplies(outsider, privapply.ListQuery{Tab: "all", Limit: 20})
	if vo.Total != 0 {
		t.Errorf("跨组: %d", vo.Total)
	}
	// 撤回 a2 后：状态筛选 / 搜索（编号、表名、实例）
	if err := s.Abort(ctx, dev, a2.ApplyID, "不要了"); err != nil {
		t.Fatal(err)
	}
	st, _ := s.ListApplies(dev, privapply.ListQuery{Tab: "mine", Status: "3", Limit: 20})
	if st.Total != 1 || st.Rows[0].ApplyID != a2.ApplyID || st.Rows[0].CanAbort || st.Rows[0].Progress != nil {
		t.Errorf("已撤回筛选: %+v", st.Rows)
	}
	for q, want := range map[string]int{"#" + itoa(a2.ApplyID): 1, "refunds": 1, "prod-mysql": 2, "orders": 1, "没有": 0, "dev": 2} {
		got, _ := s.ListApplies(dev, privapply.ListQuery{Tab: "mine", Search: q, Limit: 20})
		if int(got.Total) != want {
			t.Errorf("搜索 %q: 期望 %d 得到 %d", q, want, got.Total)
		}
	}
	// 非法状态值忽略
	if bad, _ := s.ListApplies(dev, privapply.ListQuery{Tab: "mine", Status: "x'; drop", Limit: 20}); bad.Total != 2 {
		t.Errorf("非法状态应被忽略: %d", bad.Total)
	}
	// 分页
	pg, _ := s.ListApplies(dev, privapply.ListQuery{Tab: "mine", Offset: 1, Limit: 1})
	if pg.Total != 2 || len(pg.Rows) != 1 {
		t.Errorf("分页: %d %d", pg.Total, len(pg.Rows))
	}
}

func itoa(n uint) string { return strconv.Itoa(int(n)) }

func TestDetailTimelineGrantedAndReapply(t *testing.T) {
	s, r := extSetup()
	r.UserDepts = map[string]string{"dev": "研发一部"}
	ctx := context.Background()
	a, _ := s.Create(ctx, dev, withReason(tableReq()))

	d, _ := s.Detail(leader, a.ApplyID)
	if d.Dept != "研发一部" {
		t.Errorf("部门: %q", d.Dept)
	}
	if len(d.Timeline) != 4 {
		t.Fatalf("提交 + 2 级审批 + 授予: %+v", d.Timeline)
	}
	states := func() string {
		var out []string
		for _, st := range d.Timeline {
			out = append(out, st.State)
		}
		return strings.Join(out, ",")
	}
	if states() != "done,current,pending,pending" || d.Timeline[1].Reviewers[0] != "组长" || d.Timeline[2].Reviewers[0] != "DBA" {
		t.Errorf("待审批时间线: %s %+v", states(), d.Timeline)
	}
	// 页面只显示审批组，鼠标悬停时显示这一级能审批的人；“提交 / 授予”不是审批节点，没有提示
	if d.Timeline[1].Tip() != "可审批人员（1）：组长" || d.Timeline[2].Tip() != "可审批人员（1）：DBA" || d.Timeline[0].Tip() != "" || d.Timeline[3].Tip() != "" {
		t.Errorf("悬停提示: %q %q %q %q", d.Timeline[0].Tip(), d.Timeline[1].Tip(), d.Timeline[2].Tip(), d.Timeline[3].Tip())
	}
	if d.Timeline[1].NoReviewer || d.Timeline[2].NoReviewer {
		t.Errorf("两级都有人，不应标无人可审: %+v", d.Timeline)
	}
	// 下一级的权限组没有任何成员：这一级标“无人可审”，提示改为原因；已经走完的级别不标
	delete(r.Members, 3)
	d, _ = s.Detail(leader, a.ApplyID)
	if !d.Timeline[2].NoReviewer || !strings.Contains(d.Timeline[2].Tip(), "没有符合条件的审批人") || d.Timeline[1].NoReviewer {
		t.Errorf("无人可审: %+v", d.Timeline)
	}
	r.Members[3] = map[uint]bool{20: true}
	approveAll(t, s, a.ApplyID)
	d, _ = s.Detail(dev, a.ApplyID)
	if states() != "done,done,done,done" {
		t.Errorf("通过后时间线: %s", states())
	}
	if d.Timeline[1].Actor != "组长" || d.Timeline[1].Note != "同意，注意脱敏" || d.Timeline[1].Time.IsZero() || d.Timeline[2].Actor != "DBA" || d.Timeline[2].Note != "" {
		t.Errorf("审批人/备注/时间: %+v", d.Timeline[:3])
	}
	if len(d.Granted) != 2 || d.Granted[0].DBName != "shop" || d.Granted[0].Table != "orders" || d.Granted[0].State != privapply.PrivSoon || d.Granted[1].Table != "customers" {
		t.Errorf("已授予: %+v", d.Granted)
	}
	// 回收后链接仍在，但标记已回收
	if err := s.DeletePrivilege(leader, d.Granted[0].PrivilegeID); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Detail(dev, a.ApplyID)
	if !d.Granted[0].Revoked || d.Granted[1].Revoked {
		t.Errorf("回收标记: %+v", d.Granted)
	}

	// 驳回：时间线停在被驳回的那一级，其后“跳过”；给出重新申请预填
	b, _ := s.Create(ctx, dev, withReason(dbReq()))
	if err := s.Review(ctx, leader, b.ApplyID, audit.ActionReject, "范围太大"); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Detail(dev, b.ApplyID)
	if states() != "done,rejected,skipped,rejected" || d.Timeline[1].Note != "范围太大" || d.Timeline[1].Actor != "组长" {
		t.Errorf("驳回时间线: %s %+v", states(), d.Timeline)
	}
	q, err := url.ParseQuery(d.Reapply)
	if err != nil || q.Get("new") != "1" || q.Get("instance") != "1" || q.Get("type") != "1" || q.Get("db") != "shop,report" || q.Get("limit") != "200" || q.Get("reason") == "" {
		t.Errorf("重新申请参数: %v %v", d.Reapply, err)
	}
	// 撤回
	cr := withReason(tableReq())
	cr.TableList = []string{"refunds"}
	c, err := s.Create(ctx, dev, cr)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Abort(ctx, dev, c.ApplyID, "写错了")
	d, _ = s.Detail(dev, c.ApplyID)
	last := d.Timeline[len(d.Timeline)-1]
	if last.State != privapply.StepAborted || last.Note != "写错了" || last.Actor != "开发者" || d.Reapply == "" {
		t.Errorf("撤回时间线: %+v", d.Timeline)
	}
	// 待审批的申请没有重新申请
	e, _ := s.Create(ctx, dev, withReason(dbReq()))
	if d, _ = s.Detail(dev, e.ApplyID); d.Reapply != "" {
		t.Errorf("待审批不应有重新申请: %q", d.Reapply)
	}
}

func seedPrivs(r *memrepo.Repo) {
	day := func(n int) time.Time { return time.Now().AddDate(0, 0, n) }
	r.Privs = []*model.QueryPrivilege{
		{PrivilegeID: 1, UserName: "dev", UserDisplay: "开发者", InstanceID: 1, PrivType: 2, DBName: "shop", TbName: "orders", ValidDate: day(30), LimitNum: 10},
		{PrivilegeID: 2, UserName: "dev", UserDisplay: "开发者", InstanceID: 1, PrivType: 1, DBName: "report", ValidDate: day(3), LimitNum: 10},
		{PrivilegeID: 3, UserName: "dev", UserDisplay: "开发者", InstanceID: 1, PrivType: 1, DBName: "old", ValidDate: day(-5), LimitNum: 10},
		{PrivilegeID: 4, UserName: "dba", UserDisplay: "DBA", InstanceID: 2, PrivType: 1, DBName: "x", ValidDate: day(7), LimitNum: 10},
		{PrivilegeID: 5, UserName: "dba", UserDisplay: "DBA", InstanceID: 2, PrivType: 1, DBName: "gone", ValidDate: day(30), LimitNum: 10, IsDeleted: 1},
		{PrivilegeID: 6, UserName: "far", UserDisplay: "远方", InstanceID: 3, PrivType: 1, DBName: "y", ValidDate: day(-1), LimitNum: 10},
	}
	r.PrivExtRows = map[uint]privapply.PrivExt{
		1: {PrivilegeID: 1, Source: privapply.SourceApply, ApplyID: 12},
		2: {PrivilegeID: 2, Source: privapply.SourceGrant, GrantedBy: "leader", Reason: "临时排障"},
	}
}

func TestPrivilegeListStatesFiltersAndStats(t *testing.T) {
	s, r := extSetup()
	seedPrivs(r)
	ids := func(l *privapply.PrivList) string {
		var out []string
		for _, p := range l.Rows {
			out = append(out, itoa(p.PrivilegeID))
		}
		return strings.Join(out, ",")
	}
	list := func(p audit.Actor, q privapply.PrivQuery) *privapply.PrivList {
		t.Helper()
		q.Limit = 50
		l, err := s.ListPrivileges(p, q)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	// 默认：未过期（有效 + 即将到期），旧行为不变；已回收永远不显示
	l := list(root, privapply.PrivQuery{})
	if ids(l) != "4,2,1" || l.Total != 3 {
		t.Errorf("默认应只含未过期: %s", ids(l))
	}
	if ids(list(root, privapply.PrivQuery{State: "expired"})) != "6,3" {
		t.Errorf("已过期: %s", ids(list(root, privapply.PrivQuery{State: "expired"})))
	}
	if ids(list(root, privapply.PrivQuery{State: "soon"})) != "4,2" {
		t.Errorf("即将到期（含第 7 天）: %s", ids(list(root, privapply.PrivQuery{State: "soon"})))
	}
	if ids(list(root, privapply.PrivQuery{State: "all"})) != "6,4,3,2,1" {
		t.Errorf("全部: %s", ids(list(root, privapply.PrivQuery{State: "all"})))
	}
	if ids(list(root, privapply.PrivQuery{State: "bogus"})) != "4,2,1" {
		t.Error("非法 state 回落到默认")
	}
	// 行上的状态 / 天数 / 来源
	byID := map[uint]privapply.PrivRow{}
	for _, p := range list(root, privapply.PrivQuery{State: "all"}).Rows {
		byID[p.PrivilegeID] = p
	}
	if byID[1].State != privapply.PrivValid || byID[1].DaysLeft != 30 || byID[1].Source != privapply.SourceApply || byID[1].ApplyID != 12 {
		t.Errorf("权限1: %+v", byID[1])
	}
	if byID[2].State != privapply.PrivSoon || byID[2].DaysLeft != 3 || byID[2].Source != privapply.SourceGrant || byID[2].GrantedBy != "leader" || byID[2].GrantNote != "临时排障" {
		t.Errorf("权限2: %+v", byID[2])
	}
	if byID[3].State != privapply.PrivExpired || byID[3].DaysLeft != -5 || byID[3].Source != "" {
		t.Errorf("权限3: %+v", byID[3])
	}
	// 用户 / 实例 / 库 / 搜索筛选
	if ids(list(root, privapply.PrivQuery{User: "dev", State: "all"})) != "3,2,1" ||
		ids(list(root, privapply.PrivQuery{InstanceID: 2, State: "all"})) != "4" ||
		ids(list(root, privapply.PrivQuery{DB: "REPORT"})) != "2" ||
		ids(list(root, privapply.PrivQuery{UserDisplay: "DBA"})) != "4" ||
		ids(list(root, privapply.PrivQuery{Search: "ord"})) != "1" {
		t.Error("筛选结果不对")
	}
	// KPI 与筛选项
	st := l.Stats
	if !l.HasStats || st.Valid != 3 || st.Soon != 2 || st.Expired != 2 || st.Users != 3 || st.Insts != 3 {
		t.Errorf("KPI: %+v", st)
	}
	if len(st.UserOpts) != 3 || len(st.InstOpts) != 3 || st.InstOpts[0].Name != "no-tag" && st.InstOpts[0].Name != "orphan" && st.InstOpts[0].Name != "prod-mysql-01" {
		t.Errorf("筛选项: %+v", st)
	}
	if got := list(root, privapply.PrivQuery{InstanceID: 1}).Stats.DBOpts; strings.Join(got, ",") != "old,report,shop" {
		t.Errorf("实例下的库: %v", got)
	}
	// 普通用户只看自己的，KPI 也只统计自己的
	own := list(dev, privapply.PrivQuery{State: "all"})
	if own.Total != 3 || own.Stats.Users != 1 || own.CanManage {
		t.Errorf("普通用户: %+v", own.Stats)
	}
	// 管理者：自己资源组关联的实例
	if got := list(leader, privapply.PrivQuery{State: "all"}); ids(got) != "4,3,2,1" || got.Stats.Insts != 2 {
		t.Errorf("管理者: %s %+v", ids(got), got.Stats)
	}
}

func grantReq() privapply.GrantReq {
	return privapply.GrantReq{UserName: "dev", InstanceID: 1, PrivType: 2, DBName: "shop", TableList: []string{"orders", "refunds"},
		ValidDate: tomorrow(), LimitNum: 300, Reason: "客诉排查，临时放开"}
}

func TestDirectGrant(t *testing.T) {
	s, r := extSetup()
	ctx := context.Background()

	// 权限：没有 query_mgtpriv 不行（含有审批权限的 dba）
	for _, p := range []audit.Actor{dev, dba} {
		if _, err := s.GrantPrivilege(ctx, p, grantReq()); code(err) != "forbidden" {
			t.Errorf("%s 不应能授予: %v", p.Username, err)
		}
	}
	if s.CanGrant(dev) || !s.CanGrant(leader) || !s.CanGrant(root) {
		t.Error("CanGrant 判断不对")
	}
	res, err := s.GrantPrivilege(ctx, leader, grantReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.PrivilegeIDs) != 2 || len(r.Privs) != 2 {
		t.Fatalf("应授予 2 张表: %+v privs=%d", res, len(r.Privs))
	}
	p := r.Privs[0]
	if p.UserName != "dev" || p.UserDisplay != "开发者" || p.DBName != "shop" || p.TbName != "orders" || p.LimitNum != 300 || p.PrivType != 2 || p.InstanceID != 1 {
		t.Errorf("授予记录: %+v", p)
	}
	// 来源与审计
	x := r.PrivExtRows[res.PrivilegeIDs[0]]
	if x.Source != privapply.SourceGrant || x.GrantedBy != "leader" || x.Reason != "客诉排查，临时放开" {
		t.Errorf("来源记录: %+v", x)
	}
	if len(r.AuditEntries) != 1 || r.AuditEntries[0].Action != "直接授予查询权限" || r.AuditEntries[0].UserName != "leader" ||
		!strings.Contains(r.AuditEntries[0].ExtraInfo, "dev") || !strings.Contains(r.AuditEntries[0].ExtraInfo, "客诉排查") || !strings.Contains(r.AuditEntries[0].ExtraInfo, "shop.orders,refunds") {
		t.Errorf("审计: %+v", r.AuditEntries)
	}
	// 立刻对在线查询生效
	privs, _ := r.ValidQueryPrivileges("dev", 1)
	if len(privs) != 2 {
		t.Errorf("授予后应有有效权限: %+v", privs)
	}
	// 授予的权限在列表里显示来源“直接授予”
	l, _ := s.ListPrivileges(leader, privapply.PrivQuery{Limit: 20})
	if l.Rows[0].Source != privapply.SourceGrant || l.Rows[0].GrantedBy != "leader" {
		t.Errorf("列表来源: %+v", l.Rows[0])
	}
	// 重复授予被拒绝
	if _, err := s.GrantPrivilege(ctx, leader, grantReq()); code(err) != "bad_request" {
		t.Errorf("重复授予: %v", err)
	}
	if len(r.Privs) != 2 || len(r.AuditEntries) != 1 {
		t.Error("被拒绝的授予不应落库或写审计")
	}
	// 库权限：多个库
	dbq := privapply.GrantReq{UserName: "dev", InstanceID: 1, PrivType: 1, DBList: []string{"report", "stats"}, ValidDate: tomorrow(), LimitNum: 50, Reason: "月度报表导出"}
	if res, err = s.GrantPrivilege(ctx, root, dbq); err != nil || len(res.PrivilegeIDs) != 2 || r.Privs[2].TbName != "" || r.Privs[2].PrivType != 1 {
		t.Errorf("库权限: %v %+v", err, res)
	}
}

func TestDirectGrantValidation(t *testing.T) {
	s, r := extSetup()
	ctx := context.Background()
	r.Users[5] = model.User{ID: 5, Username: "off", Display: "停用", IsActive: false}
	r.Users[6] = model.User{ID: 6, Username: "boss", Display: "老板", IsActive: true, IsSuperuser: true}
	r.Users[7] = model.User{ID: 7, Username: "nogroup", Display: "没资源组", IsActive: true}
	cases := []struct {
		name  string
		mod   func(*privapply.GrantReq)
		actor audit.Actor
		want  string
	}{
		{"权限类型", func(q *privapply.GrantReq) { q.PrivType = 0 }, leader, "bad_request"},
		{"没选实例", func(q *privapply.GrantReq) { q.InstanceID = 0 }, leader, "bad_request"},
		{"没选用户", func(q *privapply.GrantReq) { q.UserName = " " }, leader, "bad_request"},
		{"行数", func(q *privapply.GrantReq) { q.LimitNum = 0 }, leader, "bad_request"},
		{"行数过大", func(q *privapply.GrantReq) { q.LimitNum = 2_000_000 }, leader, "bad_request"},
		{"日期格式", func(q *privapply.GrantReq) { q.ValidDate = "x" }, leader, "bad_request"},
		{"过去的日期", func(q *privapply.GrantReq) { q.ValidDate = time.Now().AddDate(0, 0, -1).Format("2006-01-02") }, leader, "bad_request"},
		{"没有原因", func(q *privapply.GrantReq) { q.Reason = "" }, leader, "bad_request"},
		{"原因太短", func(q *privapply.GrantReq) { q.Reason = "急" }, leader, "bad_request"},
		{"没有表", func(q *privapply.GrantReq) { q.TableList = nil }, leader, "bad_request"},
		{"没有库", func(q *privapply.GrantReq) { q.DBName = "" }, leader, "bad_request"},
		{"库名含逗号", func(q *privapply.GrantReq) { q.DBName = "a,b" }, leader, "bad_request"},
		{"用户不存在", func(q *privapply.GrantReq) { q.UserName = "ghost" }, leader, "bad_request"},
		{"用户已停用", func(q *privapply.GrantReq) { q.UserName = "off" }, leader, "bad_request"},
		{"超级用户无需授予", func(q *privapply.GrantReq) { q.UserName = "boss" }, leader, "bad_request"},
		{"用户访问不了该实例", func(q *privapply.GrantReq) { q.UserName = "nogroup" }, leader, "bad_request"},
		{"管理者不能在别组的实例上授予", func(q *privapply.GrantReq) { q.InstanceID = 3 }, leader, "forbidden"},
		{"实例不存在", func(q *privapply.GrantReq) { q.InstanceID = 99 }, root, "forbidden"},
	}
	for _, c := range cases {
		req := grantReq()
		c.mod(&req)
		if _, err := s.GrantPrivilege(ctx, c.actor, req); code(err) != c.want {
			t.Errorf("%s: 期望 %s 得到 %v", c.name, c.want, err)
		}
	}
	if len(r.Privs) != 0 || len(r.AuditEntries) != 0 || len(r.PrivExtRows) != 0 {
		t.Errorf("所有被拒绝的授予都不应留下记录: privs=%d audit=%d ext=%d", len(r.Privs), len(r.AuditEntries), len(r.PrivExtRows))
	}
	// 拥有 query.admin_run 的用户即使资源组没关联实例也可授予
	r.Perms[7] = map[string]bool{"query.admin_run": true}
	req := grantReq()
	req.UserName = "nogroup"
	if _, err := s.GrantPrivilege(ctx, leader, req); err != nil {
		t.Errorf("query.admin_run: %v", err)
	}
}

func TestGrantUsersAndInstances(t *testing.T) {
	s, _ := extSetup()
	if _, err := s.GrantUsers(dev); code(err) != "forbidden" {
		t.Errorf("无权限: %v", err)
	}
	us, err := s.GrantUsers(leader)
	if err != nil || len(us) != 3 { // 与组长同资源组 1 的 dev/leader/dba，外人在资源组 2
		t.Errorf("管理者只能选同资源组的用户: %v %+v", err, us)
	}
	if all, _ := s.GrantUsers(root); len(all) != 4 {
		t.Errorf("超级用户可选全部启用用户: %+v", all)
	}
	insts, err := s.GrantInstances(leader)
	if err != nil || len(insts) != 2 { // 资源组 1 关联的实例 1、2（授予不要求 can_read 标签，只要求支持在线查询）
		t.Errorf("管理者可授予的实例: %v %+v", err, insts)
	}
	if all, _ := s.GrantInstances(root); len(all) != 3 {
		t.Errorf("超级用户: %+v", all)
	}
}
