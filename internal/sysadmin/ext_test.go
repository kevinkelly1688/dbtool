// @author: kevinkelly
// @license: Apache Licence
// @file: ext_test.go
// @time: 2026/10/28

package sysadmin

import (
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/query"
)

func strp(s string) *string { return &s }

// 给 TL / DBA 权限组都授予工单审核按钮（TL 另有查询权限审核），并给研发组配置 SQL 上线审批流 TL → DBA。
func setupFlow(t *testing.T) (*Service, *fakeRepo) {
	t.Helper()
	s, f := setup()
	f.grant(10, "sqlworkflow.approve", "qpriv.review")
	f.grant(20, "sqlworkflow.approve")
	if _, err := s.SaveFlow(root, 1, WorkflowSQL, []uint{10, 20}); err != nil {
		t.Fatal(err)
	}
	return s, f
}

// ---- 审批流：按按钮权限计算“可审”、账号锁定、KPI ----

func TestFlowApproversByButtonEngine(t *testing.T) {
	s, f := setupFlow(t)
	// 默认引擎（夹具里 TL / DBA 都被授予了 sqlworkflow.approve）：两级都有人可审
	v, err := s.FlowDetail(1, WorkflowSQL)
	if err != nil || len(v.Warnings) != 0 {
		t.Fatalf("%v %v", err, v)
	}
	// alice 被单独禁用了审批按钮，bob 正常：第 1 级（TL=alice）无人可审，第 2 级正常
	s.SetButtonEngine(&fakeEngine{has: map[string]bool{"3/sqlworkflow.approve": true}})
	v, err = s.FlowDetail(1, WorkflowSQL)
	if err != nil {
		t.Fatal(err)
	}
	if v.Levels[0].Warning == "" || !strings.Contains(v.Levels[0].Warning, "sqlworkflow.approve") || v.Levels[0].Approvers[0].HasPerm {
		t.Fatalf("第 1 级应提示按钮权限缺失: %+v", v.Levels[0])
	}
	if v.Levels[1].Warning != "" || !v.Levels[1].Approvers[0].HasPerm || v.Levels[1].Approvers[0].Locked {
		t.Fatalf("第 2 级应正常: %+v", v.Levels[1])
	}
	// bob 登录失败达到阈值（默认 5 次 / 300 秒）而被锁定：第 2 级无人可审，且标出账号已锁定
	u := f.users[3]
	u.FailedLoginCount = 5
	last := fixedNow.Add(-time.Minute)
	u.LastLoginFailedAt = &last
	f.users[3] = u
	v, _ = s.FlowDetail(1, WorkflowSQL)
	lv := v.Levels[1]
	if !lv.Approvers[0].Locked || !strings.Contains(lv.Warning, "锁定") {
		t.Fatalf("锁定的审批人不计入: %+v", lv)
	}
	// 锁定期过后恢复
	last = fixedNow.Add(-time.Hour)
	f.users[3] = model.User{ID: u.ID, Username: u.Username, Display: u.Display, IsActive: true, FailedLoginCount: 5, LastLoginFailedAt: &last}
	v, _ = s.FlowDetail(1, WorkflowSQL)
	if v.Levels[1].Warning != "" {
		t.Fatalf("锁定期已过: %+v", v.Levels[1])
	}
	// 超级用户不受按钮引擎影响
	f.addUser(9, "boss", true, true)
	f.link(f.userGroups, 9, 10)
	f.link(f.userRGs, 9, 1)
	v, _ = s.FlowDetail(1, WorkflowSQL)
	if v.Levels[0].Warning != "" {
		t.Fatalf("超级用户总是可审: %+v", v.Levels[0])
	}
}

func TestFlowOverviewSummary(t *testing.T) {
	s, _ := setupFlow(t)
	if _, err := s.SaveFlow(root, 2, WorkflowQuery, []uint{20}); err != nil { // 数据组无人（没有成员）→ 无人可审
		t.Fatal(err)
	}
	ov, err := s.FlowOverview()
	if err != nil {
		t.Fatal(err)
	}
	// 2 个资源组 × 3 种类型 = 6；已配置 2；其中数据组·查询权限无人可审
	if ov.Summary.Total != 6 || ov.Summary.Configured != 2 || ov.Summary.Unconfigured != 4 || ov.Summary.NoApprover != 1 {
		t.Fatalf("%+v", ov.Summary)
	}
	for _, r := range ov.Matrix {
		switch r.GroupID {
		case 1:
			if r.Configured != 1 || r.Warns != 0 {
				t.Fatalf("研发组: %+v", r)
			}
		case 2:
			if r.Configured != 1 || r.Warns != 1 {
				t.Fatalf("数据组: %+v", r)
			}
			for _, c := range r.Cells {
				if c.Type == WorkflowQuery && !c.NoApprover {
					t.Fatalf("应标记无人可审: %+v", c)
				}
			}
		}
	}
}

func TestGroupSaveWarnings(t *testing.T) {
	s, f := setupFlow(t)
	// 研发组 SQL 流：第 1 级 TL（alice）、第 2 级 DBA（bob）。bob 现在能审；若 DBA 组保存后他失去审批按钮 → 第 2 级无人可审
	eng := &fakeEngine{has: map[string]bool{"2/sqlworkflow.approve": true, "3/sqlworkflow.approve": true}, whatif: map[string]bool{"2/sqlworkflow.approve": true}}
	s.SetButtonEngine(eng)
	w := s.GroupSaveWarnings(20, "DBA", []string{"audit.view"})
	if len(w) != 1 || !strings.Contains(w[0], "研发组") || !strings.Contains(w[0], "第 2 级") || !strings.Contains(w[0], "无人可审") {
		t.Fatalf("应警告第 2 级将无人可审: %v", w)
	}
	// 保存后 bob 仍然能审（别的权限组给了他）→ 没有警告
	eng.whatif["3/sqlworkflow.approve"] = true
	if w := s.GroupSaveWarnings(20, "DBA", []string{"audit.view"}); len(w) != 0 {
		t.Fatalf("仍有人可审不应警告: %v", w)
	}
	// 这一级本来就没人可审：不重复警告
	eng.has["3/sqlworkflow.approve"] = false
	eng.whatif["3/sqlworkflow.approve"] = false
	if w := s.GroupSaveWarnings(20, "DBA", nil); len(w) != 0 {
		t.Fatalf("本来就无人可审不应警告: %v", w)
	}
	// 没有按钮引擎时什么也不做
	s.SetButtonEngine(nil)
	if w := s.GroupSaveWarnings(20, "DBA", nil); w != nil {
		t.Fatalf("%v", w)
	}
	_ = f
}

// ---- 资源组：负责人 / 备注、KPI、停用被拒、彻底删除、关联预览 ----

func TestResGroupLeaderRemarkAndSearch(t *testing.T) {
	s, f := setup()
	id, err := s.CreateResourceGroup(root, ResGroupInput{Name: "营销", Leader: strp("alice"), Remark: strp("增长与运营")})
	if err != nil {
		t.Fatal(err)
	}
	if e := f.xs().rg[id]; e.Leader != "alice" || e.Remark != "增长与运营" {
		t.Fatalf("%+v", e)
	}
	list, _ := s.ListResourceGroups("", false)
	var got ResGroupItem
	for _, g := range list {
		if g.ID == id {
			got = g
		}
	}
	if got.Leader != "alice" || got.LeaderDisplay != "alice名" || got.Remark != "增长与运营" {
		t.Fatalf("%+v", got)
	}
	// 搜索负责人 / 姓名 / 备注
	for _, q := range []string{"alice", "ALICE名", "增长", "营销"} {
		l, _ := s.ListResourceGroups(q, false)
		if len(l) != 1 || l[0].ID != id {
			t.Fatalf("搜索 %q: %+v", q, l)
		}
	}
	// 修改：不传字段 = 不变；传空串 = 清空
	if err := s.UpdateResourceGroup(root, id, ResGroupInput{Name: "营销"}); err != nil || f.xs().rg[id].Leader != "alice" {
		t.Fatalf("不传应保持: %v %+v", err, f.xs().rg[id])
	}
	if err := s.UpdateResourceGroup(root, id, ResGroupInput{Name: "营销", Leader: strp(""), Remark: strp("新备注")}); err != nil {
		t.Fatal(err)
	}
	if e := f.xs().rg[id]; e.Leader != "" || e.Remark != "新备注" {
		t.Fatalf("%+v", e)
	}
	last := f.audits[len(f.audits)-1]
	if last.Action != "修改资源组" || !strings.Contains(last.ExtraInfo, "负责人：alice → —") || !strings.Contains(last.ExtraInfo, "备注：已修改") {
		t.Fatalf("审计: %+v", last)
	}
	// 校验：负责人必须存在且启用；备注过长
	wantErr(t, s.UpdateResourceGroup(root, id, ResGroupInput{Name: "营销", Leader: strp("nobody")}), 400, "负责人")
	wantErr(t, s.UpdateResourceGroup(root, id, ResGroupInput{Name: "营销", Leader: strp("carol")}), 400, "已停用")
	wantErr(t, s.UpdateResourceGroup(root, id, ResGroupInput{Name: "营销", Remark: strp(strings.Repeat("字", 101))}), 400, "备注")
	// 详情
	d, err := s.GetResourceGroup(id)
	if err != nil || d.Remark != "新备注" {
		t.Fatalf("%v %+v", err, d)
	}
}

func TestResGroupExtTableMissing(t *testing.T) {
	s, f := setup()
	f.xs().missing = true
	// 不填扩展字段：照常工作
	id, err := s.CreateResourceGroup(root, ResGroupInput{Name: "无扩展"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListResourceGroups("", false); err != nil {
		t.Fatal(err)
	}
	// 填了负责人：给出建表提示，且整体回滚（资源组不会被创建）
	n := len(f.rgs)
	_, err = s.CreateResourceGroup(root, ResGroupInput{Name: "带负责人", Leader: strp("alice")})
	wantErr(t, err, 409, "080_sysadmin_ext.sql")
	if len(f.rgs) != n {
		t.Fatal("扩展表缺失时不应留下半个资源组")
	}
	wantErr(t, s.UpdateResourceGroup(root, id, ResGroupInput{Name: "无扩展", Remark: strp("x")}), 409, "080_sysadmin_ext.sql")
}

func TestResGroupSummary(t *testing.T) {
	s, f := setupFlow(t)
	g := f.rgs[2]
	g.IsDeleted = 1
	f.rgs[2] = g
	g1 := f.rgs[1]
	g1.DingWebhook = "https://x"
	f.rgs[1] = g1
	sum, err := s.ResGroupSummary()
	if err != nil {
		t.Fatal(err)
	}
	// 研发组启用：没有实例（fake 的 rgInst 为空）、审批流 1/3、已配钉钉；数据组已停用
	if sum.Enabled != 1 || sum.Disabled != 1 || sum.NoInstance != 1 || sum.FlowIncomplete != 1 || sum.NoBot != 0 {
		t.Fatalf("%+v", sum)
	}
}

// “通知群”列与“未配置机器人”统计要把“通知渠道”也算进去：只配了通知渠道（没配资源组上的旧版 Webhook）的组不是“没有机器人”；
// 全局渠道对所有资源组生效；邮件不算机器人；渠道类型按个数列出。
func TestResGroupChannelsCountAsConfigured(t *testing.T) {
	s, f := setupFlow(t)
	f.rgs[3] = model.ResourceGroup{GroupID: 3, GroupName: "测试组"}
	f.rgs[4] = model.ResourceGroup{GroupID: 4, GroupName: "邮件组"}
	f.rgs[5] = model.ResourceGroup{GroupID: 5, GroupName: "空白组"}
	g2 := f.rgs[2]
	g2.IsDeleted = 0
	f.rgs[2] = g2
	// 1 号组：没有旧版 Webhook，只有一个钉钉渠道；2 号组：两个飞书渠道；3 号组：只有全局的 Slack 渠道；
	// 4 号组：只有邮件渠道；5 号组：什么都没有（全局的 Slack 渠道对它也生效，所以也算有机器人）
	f.xs().chans = map[uint]map[string]int{1: {"dingtalk": 1}, 2: {"feishu": 2}, 4: {"email": 1}, 0: {"slack": 1}}
	list, err := s.ListResourceGroups("", false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint]map[string]int{}
	for _, it := range list {
		got[it.ID] = it.Channels
	}
	if got[1]["dingtalk"] != 1 || got[1]["slack"] != 1 || got[2]["feishu"] != 2 || got[3]["slack"] != 1 || got[4]["email"] != 1 || got[5]["slack"] != 1 {
		t.Fatalf("各组生效的渠道（含全局）: %+v", got)
	}
	if got[2]["dingtalk"] != 0 || got[4]["dingtalk"] != 0 {
		t.Fatalf("没有关联的渠道不应出现: %+v", got)
	}
	sum, err := s.ResGroupSummary()
	if err != nil {
		t.Fatal(err)
	}
	if sum.NoBot != 0 {
		t.Fatalf("有全局 Slack 渠道，所有启用的组都有机器人: %+v", sum)
	}
	// 去掉全局渠道：4 号组只有邮件、5 号组什么都没有，才是“没有机器人”；1、2 号组有渠道，3 号组没有
	f.xs().chans = map[uint]map[string]int{1: {"dingtalk": 1}, 2: {"feishu": 2}, 4: {"email": 1}}
	sum, err = s.ResGroupSummary()
	if err != nil {
		t.Fatal(err)
	}
	if sum.NoBot != 3 {
		t.Fatalf("3、4、5 号组没有机器人（邮件不算）: %+v", sum)
	}
	// 企业微信渠道也是机器人：3 号组只配了企业微信渠道，就不再算“没有机器人”
	f.xs().chans = map[uint]map[string]int{1: {"dingtalk": 1}, 2: {"feishu": 2}, 3: {"wecom": 1}, 4: {"email": 1}}
	sum, err = s.ResGroupSummary()
	if err != nil {
		t.Fatal(err)
	}
	if sum.NoBot != 2 {
		t.Fatalf("只有 4、5 号组没有机器人（企业微信算机器人）: %+v", sum)
	}
	if list, err = s.ListResourceGroups("", false); err != nil {
		t.Fatal(err)
	}
	for _, it := range list {
		if it.ID == 3 && it.Channels["wecom"] != 1 {
			t.Fatalf("3 号组的渠道里应有企业微信: %+v", it.Channels)
		}
	}
	// 详情里也带着渠道
	d, err := s.GetResourceGroup(2)
	if err != nil || d.Channels["feishu"] != 2 {
		t.Fatalf("详情: %+v %v", d, err)
	}
}

func TestDisableRefusedListsOpenWorkflows(t *testing.T) {
	s, f := setup()
	f.openWF[1] = 2
	f.xs().open[1] = []OpenWorkflow{{Kind: "sql", Label: "SQL 上线", ID: 7, Name: "加索引", Status: "待审批", Href: "/sqlworkflow/7"}}
	err := s.DisableResourceGroup(root, 1)
	wantErr(t, err, 409, "2 个进行中的工单")
	var qe *query.Error
	if !errors.As(err, &qe) || qe.Data == nil {
		t.Fatalf("应附带工单明细: %v", err)
	}
	list, ok := qe.Data["open_workflows"].([]OpenWorkflow)
	if !ok || len(list) != 1 || list[0].Href != "/sqlworkflow/7" || qe.Data["total"] != int64(2) {
		t.Fatalf("%+v", qe.Data)
	}
}

func TestPurgeResourceGroup(t *testing.T) {
	s, f := setupFlow(t)
	// 未停用：拒绝
	wantErr(t, s.PurgeResourceGroup(root, 2, "数据组"), 409, "先停用")
	if err := s.DisableResourceGroup(root, 2); err != nil {
		t.Fatal(err)
	}
	// 组名不对
	wantErr(t, s.PurgeResourceGroup(root, 2, "错的"), 400, "数据组")
	// 有引用：拒绝并列出
	f.xs().refs[2] = ResGroupRefs{Users: 1, SQLWorkflows: 3}
	err := s.PurgeResourceGroup(root, 2, "数据组")
	wantErr(t, err, 409, "1 个关联用户")
	wantErr(t, err, 409, "3 个 SQL 上线")
	if _, ok := f.rgs[2]; !ok || len(f.xs().purged) != 0 {
		t.Fatal("被引用时不能删除")
	}
	// 没有引用：删除，审批流配置一并删除，写审计
	f.xs().refs[2] = ResGroupRefs{FlowSettings: 1}
	if _, err := s.SaveFlow(root, 2, WorkflowQuery, []uint{20}); err == nil {
		t.Fatal("已停用的资源组不应能再保存审批流")
	}
	if err := s.PurgeResourceGroup(root, 2, " 数据组 "); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.rgs[2]; ok {
		t.Fatal("资源组应被删除")
	}
	last := f.audits[len(f.audits)-1]
	if last.Action != "彻底删除资源组" || !strings.Contains(last.ExtraInfo, "数据组") {
		t.Fatalf("%+v", last)
	}
	wantErr(t, s.PurgeResourceGroup(root, 99, "x"), 404, "")
	wantErr(t, s.PurgeResourceGroup(alice, 99, "x"), 404, "")
}

func TestPreviewRelationChange(t *testing.T) {
	s, f := setup()
	f.grant(10, "sqlworkflow.approve")
	if _, err := s.SaveFlow(root, 1, WorkflowSQL, []uint{10}); err != nil { // 第 1 级 TL：目前只有 alice
		t.Fatal(err)
	}
	// 移除 alice → 第 1 级无人可审
	p, err := s.PreviewRelationChange(1, "users", RelationChange{Remove: []uint{2}})
	if err != nil || len(p.NewBreaks) != 1 || !strings.Contains(p.NewBreaks[0], "SQL上线申请 第 1 级") || p.Removed != 1 {
		t.Fatalf("%v %+v", err, p)
	}
	// 同时加入另一位 TL 成员 dave（有审核权限）→ 没有新断点
	f.addUser(5, "dave", true, false)
	f.link(f.userGroups, 5, 10)
	p, _ = s.PreviewRelationChange(1, "users", RelationChange{Remove: []uint{2}, Add: []uint{5}})
	if len(p.NewBreaks) != 0 || p.Added != 1 {
		t.Fatalf("%+v", p)
	}
	// dave 不在 TL 组里 → 加了也补不上
	f.userGroups[5] = map[uint]bool{}
	p, _ = s.PreviewRelationChange(1, "users", RelationChange{Remove: []uint{2}, Add: []uint{5}})
	if len(p.NewBreaks) != 1 {
		t.Fatalf("%+v", p)
	}
	// 本来就无人可审的级别不算“新”断点；实例变更不影响审批流
	f.users[2] = model.User{ID: 2, Username: "alice", Display: "alice名", IsActive: false}
	p, _ = s.PreviewRelationChange(1, "users", RelationChange{Remove: []uint{3}})
	if len(p.NewBreaks) != 0 {
		t.Fatalf("%+v", p)
	}
	p, _ = s.PreviewRelationChange(1, "instances", RelationChange{Remove: []uint{1}})
	if len(p.NewBreaks) != 0 || p.Removed != 1 {
		t.Fatalf("%+v", p)
	}
	_, err = s.PreviewRelationChange(1, "bogus", RelationChange{})
	wantErr(t, err, 400, "类型")
	_, err = s.PreviewRelationChange(99, "users", RelationChange{})
	wantErr(t, err, 404, "")
	// 预览不写库
	if len(f.audits) != 1 {
		t.Fatalf("预览不应写审计: %d", len(f.audits))
	}
}

// ---- 用户：部门、认证方式、重置密码、KPI ----

func TestUserDeptAndSearch(t *testing.T) {
	s, f := setup()
	id, err := s.CreateUser(root, UserInput{Username: "erin", Display: "艾琳", Email: "erin@example.com", Password: "Str0ng-Pass-9", IsActive: true, Dept: strp("数据平台部")})
	if err != nil {
		t.Fatal(err)
	}
	if f.xs().profiles[id].Dept != "数据平台部" {
		t.Fatalf("%+v", f.xs().profiles[id])
	}
	pg, err := s.ListUsers(UserQuery{Search: "数据平台"}) // 搜索部门
	if err != nil || len(pg.Rows) != 1 || pg.Rows[0].Username != "erin" || pg.Rows[0].Dept != "数据平台部" {
		t.Fatalf("%v %+v", err, pg)
	}
	// 编辑：不传 dept 不变；传空串清空；改动写审计
	in := UserInput{Display: "艾琳", Email: "erin@example.com", IsActive: true}
	if err := s.UpdateUser(root, id, in); err != nil || f.xs().profiles[id].Dept != "数据平台部" {
		t.Fatalf("%v %+v", err, f.xs().profiles[id])
	}
	in.Dept = strp("质量部")
	if err := s.UpdateUser(root, id, in); err != nil || f.xs().profiles[id].Dept != "质量部" {
		t.Fatalf("%v", err)
	}
	if last := f.audits[len(f.audits)-1]; !strings.Contains(last.ExtraInfo, "部门：数据平台部 → 质量部") {
		t.Fatalf("%+v", last)
	}
	in.Dept = strp(strings.Repeat("部", 51))
	wantErr(t, s.UpdateUser(root, id, in), 400, "部门")
	// 详情带最近登录 IP
	f.xs().loginIP["erin"] = "10.1.2.3"
	d, err := s.GetUser(id)
	if err != nil || d.LastLoginIP != "10.1.2.3" || d.Dept != "质量部" {
		t.Fatalf("%v %+v", err, d)
	}
}

func TestUserExtTableMissing(t *testing.T) {
	s, f := setup()
	f.xs().missing = true
	n := len(f.users)
	_, err := s.CreateUser(root, UserInput{Username: "erin", Display: "艾琳", Password: "Str0ng-Pass-9", IsActive: true, Dept: strp("部门")})
	wantErr(t, err, 409, "080_sysadmin_ext.sql")
	if len(f.users) != n {
		t.Fatal("扩展表缺失时设置部门应整体回滚")
	}
	// 不填部门照常创建，列表也正常
	if _, err := s.CreateUser(root, UserInput{Username: "erin", Display: "艾琳", Password: "Str0ng-Pass-9", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListUsers(UserQuery{}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateLDAPUserAndNoPasswordReset(t *testing.T) {
	s, f := setup()
	// LDAP 用户不需要密码，库里存不可用密码（"!" 开头，永远校验失败）
	id, err := s.CreateUser(root, UserInput{Username: "ldapu", Display: "外部", IsActive: true, AuthType: "ldap"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.users[id].Password, "!") || f.xs().profiles[id].AuthType != "ldap" {
		t.Fatalf("%q %+v", f.users[id].Password, f.xs().profiles[id])
	}
	it, _ := s.GetUser(id)
	if it.AuthLabel != "LDAP" || it.CanResetPassword || len(it.AuthMethods) != 1 || it.AuthMethods[0] != "ldap" {
		t.Fatalf("%+v", it.UserItem)
	}
	before := f.users[id].Password
	_, err = s.ResetPasswordEx(root, id, ResetPasswordOpts{Generate: true})
	wantErr(t, err, 409, "LDAP")
	wantErr(t, s.ResetPassword(root, id, "Str0ng-Pass-9"), 409, "外部系统")
	if f.users[id].Password != before {
		t.Fatal("LDAP 用户的密码不能被改")
	}
	// 本地用户不受影响；认证方式非法
	_, err = s.CreateUser(root, UserInput{Username: "x1", Display: "x", Password: "Str0ng-Pass-9", AuthType: "kerberos"})
	wantErr(t, err, 400, "认证方式")
	// 本地用户新建时必须有合规密码
	_, err = s.CreateUser(root, UserInput{Username: "x2", Display: "x", AuthType: "local"})
	wantErr(t, err, 400, "密码")
	// 外部身份绑定：只有 LDAP 绑定的老用户同样不能重置；同时有“本地”标记则可以
	f.xs().kinds[3] = []string{"ldap"}
	wantErr(t, s.ResetPassword(root, 3, "Str0ng-Pass-9"), 409, "LDAP")
	f.xs().profiles[3] = UserProfile{AuthType: "local"}
	if err := s.ResetPassword(root, 3, "Str0ng-Pass-9"); err != nil {
		t.Fatalf("显式本地认证的用户可以重置: %v", err)
	}
	f.xs().kinds[2] = []string{"dingtalk"}
	if err := s.ResetPassword(root, 2, "Str0ng-Pass-9"); err != nil {
		t.Fatalf("钉钉等其他外部登录方式不限制重置: %v", err)
	}
}

func TestResetPasswordGenerateAndForceChange(t *testing.T) {
	s, f := setup()
	old := f.users[3].Password
	res, err := s.ResetPasswordEx(root, 3, ResetPasswordOpts{Generate: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Password == "" || !res.ForceChange || !f.xs().profiles[3].MustChangePassword {
		t.Fatalf("%+v %+v", res, f.xs().profiles[3])
	}
	if err := s.policy().Validate(res.Password, "bob", "", "bob名"); err != nil {
		t.Fatalf("临时密码应满足密码策略: %v", err)
	}
	if f.users[3].Password == old || !strings.HasPrefix(f.users[3].Password, "pbkdf2_sha256$") || strings.Contains(f.users[3].Password, res.Password) {
		t.Fatal("应保存新的 pbkdf2 哈希，且不含明文")
	}
	last := f.audits[len(f.audits)-1]
	if last.Action != "重置密码" || strings.Contains(last.ExtraInfo, res.Password) || !strings.Contains(last.ExtraInfo, "系统生成临时密码") {
		t.Fatalf("审计里不能有密码: %+v", last)
	}
	// 生成与手输不能同时给
	_, err = s.ResetPasswordEx(root, 3, ResetPasswordOpts{Generate: true, Password: "Str0ng-Pass-9"})
	wantErr(t, err, 400, "不能同时")
	// 管理员手输且不强制改密：清掉遗留的标记
	if _, err := s.ResetPasswordEx(root, 3, ResetPasswordOpts{Password: "Another-Pass-77"}); err != nil {
		t.Fatal(err)
	}
	if f.xs().profiles[3].MustChangePassword {
		t.Fatal("手输密码不强制时应清除旧标记")
	}
	// 手输 + 强制改密
	res, err = s.ResetPasswordEx(root, 3, ResetPasswordOpts{Password: "Another-Pass-88", ForceChange: true})
	if err != nil || res.Password != "" || !res.ForceChange || !f.xs().profiles[3].MustChangePassword {
		t.Fatalf("%v %+v", err, res)
	}
	// 扩展表缺失：强制改密给出提示且密码不变；普通重置不受影响
	f.xs().missing = true
	cur := f.users[3].Password
	_, err = s.ResetPasswordEx(root, 3, ResetPasswordOpts{Generate: true})
	wantErr(t, err, 409, "080_sysadmin_ext.sql")
	if f.users[3].Password != cur {
		t.Fatal("强制改密写不进去时密码不应被改")
	}
	if err := s.ResetPassword(root, 3, "Plain-Reset-55"); err != nil {
		t.Fatalf("旧的重置方式在没有扩展表时照常可用: %v", err)
	}
	// 弱密码仍被策略拒绝
	wantErr(t, s.ResetPassword(root, 3, "123"), 400, "")
}

func TestGenerateTempPasswordMeetsPolicy(t *testing.T) {
	p := PasswordPolicy{MinLen: 12, Complexity: true}
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pw, err := generateTempPassword(p, "bob", "bob@example.com", "鲍勃")
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) < 16 || seen[pw] {
			t.Fatalf("长度或重复: %q", pw)
		}
		seen[pw] = true
		if err := p.Validate(pw, "bob", "bob@example.com", "鲍勃"); err != nil {
			t.Fatalf("%q: %v", pw, err)
		}
		if strings.ContainsAny(pw, "0O1lI") {
			t.Fatalf("不应含易混字符: %q", pw)
		}
	}
	// 长度上限 32：策略要求更长时无法生成，提示改为手输
	_, err := generateTempPassword(PasswordPolicy{MinLen: 40})
	wantErr(t, err, 400, "手输")
}

func TestUserSortAndSuperFilterAndSummary(t *testing.T) {
	s, f := setup()
	if _, err := s.ListUsers(UserQuery{Sort: "bogus"}); err == nil {
		t.Fatal("非法排序应 400")
	}
	for _, sort := range []string{"", SortUsername, SortLastLogin, SortStatus} {
		if _, err := s.ListUsers(UserQuery{Sort: sort}); err != nil {
			t.Fatalf("%q: %v", sort, err)
		}
	}
	pg, err := s.ListUsers(UserQuery{Status: StatusSuper})
	if err != nil || len(pg.Rows) != 1 || pg.Rows[0].Username != "root" {
		t.Fatalf("%v %+v", err, pg)
	}
	u := f.users[3]
	u.FailedLoginCount = 5
	last := fixedNow.Add(-time.Minute)
	u.LastLoginFailedAt = &last
	f.users[3] = u
	f.twofa[2] = []string{"totp"}
	sum, err := s.UserSummary()
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 4 || sum.Active != 3 || sum.Inactive != 1 || sum.Super != 1 || sum.Locked != 1 || sum.No2FA != 3 {
		t.Fatalf("%+v", sum)
	}
}

// ---- 权限组说明 ----

func TestAuthGroupDescription(t *testing.T) {
	s, f := setup()
	id, err := s.CreateAuthGroup(root, AuthGroupInput{Name: "SRE", Description: strp("值班同学")})
	if err != nil || f.xs().descs[id] != "值班同学" {
		t.Fatalf("%v %+v", err, f.xs().descs)
	}
	list, _ := s.ListAuthGroups()
	found := false
	for _, g := range list {
		if g.ID == id && g.Description == "值班同学" {
			found = true
		}
	}
	if !found {
		t.Fatalf("%+v", list)
	}
	d, _ := s.GetAuthGroup(id)
	if d.Description != "值班同学" {
		t.Fatalf("%+v", d)
	}
	// 不传 = 不变；传空 = 清空；改动写审计
	if err := s.UpdateAuthGroup(root, id, AuthGroupInput{Name: "SRE"}); err != nil || f.xs().descs[id] != "值班同学" {
		t.Fatalf("%v", err)
	}
	if err := s.UpdateAuthGroup(root, id, AuthGroupInput{Name: "SRE", Description: strp("")}); err != nil || f.xs().descs[id] != "" {
		t.Fatalf("%v", err)
	}
	if last := f.audits[len(f.audits)-1]; !strings.Contains(last.ExtraInfo, "说明：值班同学 → —") {
		t.Fatalf("%+v", last)
	}
	wantErr(t, s.UpdateAuthGroup(root, id, AuthGroupInput{Name: "SRE", Description: strp(strings.Repeat("说", 201))}), 400, "说明")
	wantErr(t, s.UpdateAuthGroup(root, id, AuthGroupInput{Name: "SRE", Description: strp("a\nb")}), 400, "换行")
	// 删除权限组时说明一起删
	f.xs().descs[id] = "x"
	if err := s.DeleteAuthGroup(root, id); err != nil || len(f.xs().descs) != 0 {
		t.Fatalf("%v %+v", err, f.xs().descs)
	}
	// 扩展表缺失：不填说明照常；填了提示建表
	f.xs().missing = true
	if _, err := s.CreateAuthGroup(root, AuthGroupInput{Name: "A1"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateAuthGroup(root, AuthGroupInput{Name: "A2", Description: strp("x")})
	wantErr(t, err, 409, "080_sysadmin_ext.sql")
}

// ---- 纯函数 ----

func TestAuthMethods(t *testing.T) {
	cases := []struct {
		profile string
		kinds   []string
		want    []string
		ext     bool // 密码由外部管理
	}{
		{"", nil, []string{"local"}, false},
		{"local", nil, []string{"local"}, false},
		{"ldap", nil, []string{"ldap"}, true},
		{"", []string{"ldap", "ldap"}, []string{"ldap"}, true},
		{"local", []string{"ldap"}, []string{"local", "ldap"}, false},
		{"", []string{"oidc", "dingtalk"}, []string{"oidc", "dingtalk"}, false},
		{"LDAP", []string{"ldap"}, []string{"ldap"}, true},
	}
	for _, c := range cases {
		got := authMethods(c.profile, c.kinds)
		if strings.Join(got, ",") != strings.Join(c.want, ",") || passwordManagedExternally(got) != c.ext {
			t.Errorf("%+v => %v ext=%v", c, got, passwordManagedExternally(got))
		}
	}
	if AuthLabel("ldap") != "LDAP" || AuthLabel("local") != "本地" || AuthLabel("zzz") != "zzz" {
		t.Error("AuthLabel")
	}
}

func TestIPFromLoginExtra(t *testing.T) {
	for in, want := range map[string]string{
		"10.1.2.3（本地）": "10.1.2.3", "10.1.2.3（API）": "10.1.2.3", "2001:db8::1（LDAP:公司）": "2001:db8::1",
		"本地": "", "": "", "unknown（本地）": "",
	} {
		if got := ipFromLoginExtra(in); got != want {
			t.Errorf("%q => %q, 期望 %q", in, got, want)
		}
	}
}

func TestResGroupRefsBlocking(t *testing.T) {
	if len((ResGroupRefs{FlowSettings: 5}).Blocking()) != 0 {
		t.Fatal("审批流配置不阻止彻底删除")
	}
	b := (ResGroupRefs{Users: 2, PerfIssues: 1}).Blocking()
	if len(b) != 2 || b[0] != "2 个关联用户" || b[1] != "1 个性能工单" {
		t.Fatalf("%v", b)
	}
}

func TestUserOrderWhitelist(t *testing.T) {
	if userOrder("") != "dbt_sql_users.username" || userOrder("x; DROP TABLE") != "dbt_sql_users.username" {
		t.Fatal("不认识的排序必须回到用户名")
	}
	if !strings.Contains(userOrder(SortLastLogin), "last_login DESC") || !strings.Contains(userOrder(SortStatus), "is_active ASC") {
		t.Fatal("排序语句不对")
	}
}
