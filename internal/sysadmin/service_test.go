// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package sysadmin

import (
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/crypto"
	"dbtool/internal/model"
	"dbtool/internal/query"
	"dbtool/internal/settings"
)

var fixedNow = time.Date(2026, 10, 6, 9, 0, 0, 0, time.Local)

// 场景：
//
//	用户：1 root（超级用户）、2 alice、3 bob、4 carol（已停用）
//	权限组：10 TL、20 DBA、30 RD；资源组：1 研发组、2 数据组
//	bob 属于 DBA 与研发组；alice 属于 TL 与研发组
func setup() (*Service, *fakeRepo) {
	f := newFake()
	f.addUser(1, "root", true, true)
	f.addUser(2, "alice", true, false)
	f.addUser(3, "bob", true, false)
	f.addUser(4, "carol", false, false)
	f.authGroups = map[uint]string{10: "TL", 20: "DBA", 30: "RD"}
	f.rgs = map[uint]model.ResourceGroup{1: {GroupID: 1, GroupName: "研发组"}, 2: {GroupID: 2, GroupName: "数据组"}}
	f.link(f.userGroups, 3, 20)
	f.link(f.userGroups, 2, 10)
	f.link(f.userRGs, 3, 1)
	f.link(f.userRGs, 2, 1)
	f.instances = []InstanceRef{{1, "prod-1", "mysql", "master"}, {2, "prod-2", "mysql", "slave"}}
	s := NewService(f)
	s.Iterations = 1000
	s.now = func() time.Time { return fixedNow }
	s.SetButtonEngine(groupEngine{f})
	return s, f
}

var (
	root  = audit.Actor{ID: 1, Username: "root", Display: "root名", IsSuperuser: true}
	alice = audit.Actor{ID: 2, Username: "alice", Display: "alice名"}
)

func httpCode(t *testing.T, err error) int {
	t.Helper()
	var qe *query.Error
	if !errors.As(err, &qe) {
		t.Fatalf("期望业务错误，得到 %v", err)
	}
	return qe.HTTP
}

func wantErr(t *testing.T, err error, code int, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望失败（%d %s），实际成功", code, contains)
	}
	if got := httpCode(t, err); got != code {
		t.Fatalf("状态码 = %d，期望 %d（%v）", got, code, err)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("错误信息 %q 不含 %q", err.Error(), contains)
	}
}

func lastAudit(t *testing.T, f *fakeRepo) model.AuditEntry {
	t.Helper()
	if len(f.audits) == 0 {
		t.Fatal("没有写审计")
	}
	return f.audits[len(f.audits)-1]
}

// ---- 基础 ----

func TestAdminFreshCheck(t *testing.T) {
	s, f := setup()
	if a, err := s.Admin(1); err != nil || a.Display != "root名" || !a.IsSuperuser {
		t.Fatalf("%+v %v", a, err)
	}
	wantErr(t, func() error { _, err := s.Admin(2); return err }(), 403, "超级用户")
	wantErr(t, func() error { _, err := s.Admin(99); return err }(), 403, "超级用户")
	u := f.users[1]
	u.IsActive = false
	f.users[1] = u
	wantErr(t, func() error { _, err := s.Admin(1); return err }(), 403, "超级用户")
	// Operator 不要求超级用户，但要求启用
	if _, err := s.Operator(2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Operator(4); err == nil {
		t.Fatal("停用用户不应通过")
	}
}

func TestTxRollsBackWhenAuditFails(t *testing.T) {
	s, f := setup()
	f.failAudit = errBoom
	if _, err := s.CreateResourceGroup(root, ResGroupInput{Name: "新组"}); !errors.Is(err, errBoom) {
		t.Fatalf("应返回审计写入失败: %v", err)
	}
	if g, _ := f.ResourceGroupByName("新组"); g != nil {
		t.Fatal("审计写入失败时数据变更应回滚")
	}
}

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		pw, want string
	}{
		{"", "不能为空"}, {"   ", "不能为空"}, {"Ab1!", "太短"}, {"12345678901", "全是数字"}, {"Password123", "过于常见"},
		{"xxalicexx9", "不能包含用户名"}, {strings.Repeat("a", 129), "太长"},
	}
	for _, c := range cases {
		err := ValidatePassword(c.pw, "alice", "alice@example.com", "爱丽丝")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: 期望含 %q，得到 %v", c.pw, c.want, err)
		}
	}
	if err := ValidatePassword("T7#kd9-Qz", "alice", "alice@example.com", "爱丽丝"); err != nil {
		t.Errorf("强密码被拒绝: %v", err)
	}
}

// ---- 资源组 ----

func TestResourceGroupCreateUpdateDisable(t *testing.T) {
	s, f := setup()
	wantErr(t, func() error { _, err := s.CreateResourceGroup(root, ResGroupInput{Name: "  "}); return err }(), 400, "名称")
	wantErr(t, func() error {
		_, err := s.CreateResourceGroup(root, ResGroupInput{Name: "A", DingWebhook: "ftp://x"})
		return err
	}(), 400, "钉钉")
	wantErr(t, func() error {
		_, err := s.CreateResourceGroup(root, ResGroupInput{Name: "A", SlackWebhook: "https://hooks.slack.com/" + strings.Repeat("x", 300)})
		return err
	}(), 400, "过长")
	wantErr(t, func() error { _, err := s.CreateResourceGroup(root, ResGroupInput{Name: "研发组"}); return err }(), 409, "同名")

	const secret = "https://oapi.dingtalk.com/robot/send?access_token=SECRET-TOKEN"
	id, err := s.CreateResourceGroup(root, ResGroupInput{Name: " 财务组 ", DingWebhook: secret})
	if err != nil {
		t.Fatal(err)
	}
	if e := lastAudit(t, f); e.Action != "新建资源组" || e.UserID != 1 || e.UserName != "root" || strings.Contains(e.ExtraInfo, "SECRET") {
		t.Fatalf("审计不对（不能含 webhook）: %+v", e)
	}
	d, err := s.GetResourceGroup(id)
	if err != nil || d.Name != "财务组" || !d.DingSet || d.FeishuSet || d.SlackSet || len(d.Flows) != 3 {
		t.Fatalf("%+v %v", d, err)
	}

	// 已停用的同名组也占用名称
	if err := s.DisableResourceGroup(root, id); err != nil {
		t.Fatal(err)
	}
	wantErr(t, func() error { _, err := s.CreateResourceGroup(root, ResGroupInput{Name: "财务组"}); return err }(), 409, "已停用")
	wantErr(t, s.DisableResourceGroup(root, id), 409, "已经停用")
	if err := s.RestoreResourceGroup(root, id); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.RestoreResourceGroup(root, id), 409, "没有被停用")

	// 编辑：webhook 留空不修改，Clear 才清空
	if err := s.UpdateResourceGroup(root, id, ResGroupInput{Name: "财务组", SlackWebhook: "https://hooks.slack.com/services/T/B/X"}); err != nil {
		t.Fatal(err)
	}
	if g := f.rgs[id]; g.DingWebhook != secret || g.SlackWebhook == "" {
		t.Fatalf("留空不应修改钉钉 webhook: %+v", g)
	}
	if e := lastAudit(t, f); strings.Contains(e.ExtraInfo, "hooks.slack.com") || !strings.Contains(e.ExtraInfo, "Slack Webhook：已设置") {
		t.Fatalf("审计: %q", e.ExtraInfo)
	}
	if err := s.UpdateResourceGroup(root, id, ResGroupInput{Name: "财务组", ClearDing: true}); err != nil {
		t.Fatal(err)
	}
	if g := f.rgs[id]; g.DingWebhook != "" || g.SlackWebhook == "" {
		t.Fatalf("%+v", g)
	}
	// 改名：与他组重名被拒；成功时同步审批流配置里的组名
	wantErr(t, s.UpdateResourceGroup(root, id, ResGroupInput{Name: "数据组"}), 409, "同名")
	f.settings[[2]int{int(id), 2}] = model.WorkflowAuditSetting{GroupID: id, GroupName: "财务组", WorkflowType: 2, AuditAuthGroups: "10"}
	if err := s.UpdateResourceGroup(root, id, ResGroupInput{Name: "财务二组"}); err != nil {
		t.Fatal(err)
	}
	if f.settings[[2]int{int(id), 2}].GroupName != "财务二组" {
		t.Fatal("审批流里的组名应同步")
	}
	wantErr(t, s.UpdateResourceGroup(root, 999, ResGroupInput{Name: "x"}), 404, "不存在")

	// 有进行中的工单时不能停用
	f.openWF[id] = 2
	wantErr(t, s.DisableResourceGroup(root, id), 409, "2 个进行中")
	if f.rgs[id].IsDeleted != 0 {
		t.Fatal("不应被停用")
	}
}

func TestResourceGroupListAndNoWebhookLeak(t *testing.T) {
	s, f := setup()
	g := f.rgs[1]
	g.DingWebhook, g.FeishuWebhook = "https://a/x", ""
	f.rgs[1] = g
	f.link(f.rgInst, 1, 1)
	f.settings[[2]int{1, 1}] = model.WorkflowAuditSetting{GroupID: 1, WorkflowType: 1, AuditAuthGroups: "10"}
	f.settings[[2]int{1, 2}] = model.WorkflowAuditSetting{GroupID: 1, WorkflowType: 2, AuditAuthGroups: "10,20"}
	list, err := s.ListResourceGroups("", false)
	if err != nil || len(list) != 2 {
		t.Fatalf("%+v %v", list, err)
	}
	it := list[0]
	if it.ID != 1 || it.Users != 2 || it.Instances != 1 || it.FlowsConfigured != 2 || it.FlowsTotal != 3 || !it.DingSet || it.FeishuSet {
		t.Fatalf("%+v", it)
	}
	d, _ := s.GetResourceGroup(1)
	if !d.Flows[0].Configured || d.Flows[0].Text != "TL → DBA" || d.Flows[2].Configured {
		t.Fatalf("%+v", d.Flows)
	}
	// 停用的组默认不列出
	_ = s.DisableResourceGroup(root, 2)
	if list, _ = s.ListResourceGroups("", false); len(list) != 1 {
		t.Fatalf("%+v", list)
	}
	if list, _ = s.ListResourceGroups("", true); len(list) != 2 || !list[1].Deleted {
		t.Fatalf("%+v", list)
	}
	if list, _ = s.ListResourceGroups("研发", true); len(list) != 1 {
		t.Fatalf("%+v", list)
	}
}

func TestGroupRelations(t *testing.T) {
	s, f := setup()
	r, err := s.GroupRelations(1)
	if err != nil || len(r.Users.Linked) != 2 || len(r.Users.Unlinked) != 2 || len(r.Instances.Linked) != 0 || len(r.Instances.Unlinked) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	if err := s.ChangeGroupRelations(root, 1, "users", RelationChange{Add: []uint{1}, Remove: []uint{2}}); err != nil {
		t.Fatal(err)
	}
	if f.userRGs[1][1] != true || f.userRGs[2][1] {
		t.Fatalf("%v", f.userRGs)
	}
	if e := lastAudit(t, f); e.Action != "调整资源组关联用户" || !strings.Contains(e.ExtraInfo, "新增用户 root") || !strings.Contains(e.ExtraInfo, "移除用户 alice") {
		t.Fatalf("%+v", e)
	}
	if err := s.ChangeGroupRelations(root, 1, "instances", RelationChange{Add: []uint{1, 2, 1}}); err != nil {
		t.Fatal(err)
	}
	if len(f.rgInst[1]) != 2 {
		t.Fatalf("%v", f.rgInst)
	}
	wantErr(t, s.ChangeGroupRelations(root, 1, "instances", RelationChange{Add: []uint{99}}), 400, "不存在")
	wantErr(t, s.ChangeGroupRelations(root, 1, "users", RelationChange{Add: []uint{1}, Remove: []uint{1}}), 400, "同时")
	wantErr(t, s.ChangeGroupRelations(root, 1, "users", RelationChange{}), 400, "没有要调整")
	wantErr(t, s.ChangeGroupRelations(root, 1, "bogus", RelationChange{Add: []uint{1}}), 400, "类型")
	wantErr(t, s.ChangeGroupRelations(root, 77, "users", RelationChange{Add: []uint{1}}), 404, "不存在")
	_ = s.DisableResourceGroup(root, 2)
	wantErr(t, s.ChangeGroupRelations(root, 2, "users", RelationChange{Add: []uint{1}}), 409, "已停用")
	// 失败不应留下半截数据：新增成功但移除对象不存在时整体回滚
	n := len(f.userRGs[3])
	wantErr(t, s.ChangeGroupRelations(root, 1, "users", RelationChange{Add: []uint{4}, Remove: []uint{99}}), 400, "不存在")
	if f.userRGs[4][1] || len(f.userRGs[3]) != n {
		t.Fatal("应回滚")
	}
}

// ---- 用户 ----

func goodUser() UserInput {
	return UserInput{Username: "dave", Display: "戴夫", Email: "dave@example.com", Password: "T7#kd9-Qz", IsActive: true,
		AuthGroupIDs: []uint{30}, ResourceGroupIDs: []uint{1}}
}

func TestCreateUser(t *testing.T) {
	s, f := setup()
	bads := []struct {
		mut  func(*UserInput)
		code int
		want string
	}{
		{func(u *UserInput) { u.Username = "" }, 400, "用户名"},
		{func(u *UserInput) { u.Username = "has space" }, 400, "用户名"},
		{func(u *UserInput) { u.Username = strings.Repeat("u", 31) }, 400, "用户名"},
		{func(u *UserInput) { u.Display = "" }, 400, "姓名"},
		{func(u *UserInput) { u.Display = "这个名字超过了十个汉字的长度" }, 400, "姓名"},
		{func(u *UserInput) { u.Email = "not-an-email" }, 400, "邮箱"},
		{func(u *UserInput) { u.Email = "A <a@b.com>" }, 400, "邮箱"},
		{func(u *UserInput) { u.Password = "" }, 400, "密码"},
		{func(u *UserInput) { u.Password = "short" }, 400, "密码"},
		{func(u *UserInput) { u.Password = "dave12345678" }, 400, "用户名"},
		{func(u *UserInput) { u.DingUserID = strings.Repeat("1", 21) }, 400, "钉钉"},
		{func(u *UserInput) { u.SlackID = "bad id!" }, 400, "Slack"},
		{func(u *UserInput) { u.AuthGroupIDs = []uint{999} }, 400, "权限组"},
		{func(u *UserInput) { u.ResourceGroupIDs = []uint{999} }, 400, "资源组"},
		{func(u *UserInput) { u.Username = "ALICE" }, 409, "已存在"},
	}
	for i, c := range bads {
		in := goodUser()
		c.mut(&in)
		_, err := s.CreateUser(root, in)
		if err == nil || httpCode(t, err) != c.code || !strings.Contains(err.Error(), c.want) {
			t.Errorf("#%d: 期望 %d/%q，得到 %v", i, c.code, c.want, err)
		}
	}
	if len(f.audits) != 0 {
		t.Fatalf("失败不应写审计: %+v", f.audits)
	}

	in := goodUser()
	in.IsSuperuser = true
	id, err := s.CreateUser(root, in)
	if err != nil {
		t.Fatal(err)
	}
	u := f.users[id]
	if !strings.HasPrefix(u.Password, "pbkdf2_sha256$1000$") || !crypto.CheckDjangoPassword("T7#kd9-Qz", u.Password) {
		t.Fatalf("密码应是 Django pbkdf2 编码: %q", u.Password)
	}
	if !u.IsStaff || !u.IsSuperuser || !u.IsActive || !u.DateJoined.Equal(fixedNow) || u.Display != "戴夫" {
		t.Fatalf("%+v", u)
	}
	if !f.userGroups[id][30] || !f.userRGs[id][1] {
		t.Fatal("权限组/资源组未写入")
	}
	e := lastAudit(t, f)
	if e.Action != "新建用户" || strings.Contains(e.ExtraInfo, "T7#kd9") || strings.Contains(e.ExtraInfo, "pbkdf2") || !strings.Contains(e.ExtraInfo, "RD") {
		t.Fatalf("审计不能含密码: %+v", e)
	}
}

func TestUpdateUserGuards(t *testing.T) {
	s, f := setup()
	cur := func(id uint) UserInput {
		u := f.users[id]
		return UserInput{Display: u.Display, Email: u.Email, IsActive: u.IsActive, IsSuperuser: u.IsSuperuser}
	}
	// 不能停用自己、不能降级自己
	in := cur(1)
	in.IsActive = false
	wantErr(t, s.UpdateUser(root, 1, in), 403, "停用自己")
	in = cur(1)
	in.IsSuperuser = false
	wantErr(t, s.UpdateUser(root, 1, in), 403, "降级")
	wantErr(t, s.SetUserActive(root, 1, false), 403, "停用自己")
	// 最后一个超级用户不能被停用或降级（由另一个“超级用户”操作的情形：alice 的旧会话里仍标着超级用户）
	asOther := audit.Actor{ID: 2, Username: "alice", Display: "alice名", IsSuperuser: true}
	in = cur(1)
	in.IsSuperuser = false
	wantErr(t, s.UpdateUser(asOther, 1, in), 409, "最后一个超级用户")
	wantErr(t, s.SetUserActive(asOther, 1, false), 409, "最后一个超级用户")
	// 有第二个超级用户后可以
	u := f.users[3]
	u.IsSuperuser = true
	f.users[3] = u
	if err := s.SetUserActive(asOther, 1, false); err != nil {
		t.Fatal(err)
	}
	// 非超级用户不能修改超级用户标记
	in = cur(2)
	in.IsSuperuser = true
	wantErr(t, s.UpdateUser(alice, 2, in), 403, "只有超级用户")
	// 用户名不可改
	in = cur(2)
	in.Username = "alice2"
	wantErr(t, s.UpdateUser(root, 2, in), 400, "不可修改")
	wantErr(t, s.UpdateUser(root, 77, cur(2)), 404, "不存在")
}

func TestUpdateUserFieldsAndGroups(t *testing.T) {
	s, f := setup()
	in := UserInput{Display: "爱丽丝", Email: "alice@example.com", SlackID: "U012AB3CD", IsActive: true, AuthGroupIDs: []uint{20, 30}, ResourceGroupIDs: []uint{2}}
	if err := s.UpdateUser(root, 2, in); err != nil {
		t.Fatal(err)
	}
	u := f.users[2]
	if u.Display != "爱丽丝" || u.Email != "alice@example.com" || u.SlackID != "U012AB3CD" || u.IsSuperuser {
		t.Fatalf("%+v", u)
	}
	if f.userGroups[2][10] || !f.userGroups[2][20] || !f.userGroups[2][30] || f.userRGs[2][1] || !f.userRGs[2][2] {
		t.Fatalf("groups=%v rgs=%v", f.userGroups[2], f.userRGs[2])
	}
	e := lastAudit(t, f)
	for _, w := range []string{"目标用户 alice", "姓名：alice名 → 爱丽丝", "新增权限组", "移除权限组 TL", "新增资源组 数据组", "Slack ID：已变更"} {
		if !strings.Contains(e.ExtraInfo, w) {
			t.Errorf("审计缺 %q: %q", w, e.ExtraInfo)
		}
	}
	// 再提交一次相同内容：无变化
	if err := s.UpdateUser(root, 2, in); err != nil {
		t.Fatal(err)
	}
	if e := lastAudit(t, f); !strings.Contains(e.ExtraInfo, "无变化") {
		t.Fatalf("%q", e.ExtraInfo)
	}
	// 已停用资源组里的关联不应被编辑清掉
	f.link(f.userRGs, 2, 1)
	_ = s.DisableResourceGroup(root, 1)
	if err := s.UpdateUser(root, 2, in); err != nil {
		t.Fatal(err)
	}
	if !f.userRGs[2][1] {
		t.Fatal("已停用资源组里的关联应保持不变")
	}
	// 引用不存在的组
	in.AuthGroupIDs = []uint{999}
	wantErr(t, s.UpdateUser(root, 2, in), 400, "权限组")
	in.AuthGroupIDs, in.ResourceGroupIDs = nil, []uint{1} // 已停用的资源组不能再分配
	wantErr(t, s.UpdateUser(root, 2, in), 400, "资源组")
}

func TestUserActiveUnlockReset(t *testing.T) {
	s, f := setup()
	if err := s.SetUserActive(root, 2, false); err != nil || f.users[2].IsActive {
		t.Fatal("停用失败", err)
	}
	wantErr(t, s.SetUserActive(root, 2, false), 409, "已经是该状态")
	if err := s.SetUserActive(root, 2, true); err != nil || !f.users[2].IsActive {
		t.Fatal("启用失败", err)
	}
	if e := lastAudit(t, f); e.Action != "启用用户" {
		t.Fatalf("%+v", e)
	}

	wantErr(t, s.UnlockUser(root, 3), 409, "无需解锁")
	u := f.users[3]
	u.FailedLoginCount = 5
	f.users[3] = u
	if err := s.UnlockUser(root, 3); err != nil || f.users[3].FailedLoginCount != 0 {
		t.Fatal("解锁失败", err)
	}
	if e := lastAudit(t, f); e.Action != "解锁账号" || !strings.Contains(e.ExtraInfo, "bob") {
		t.Fatalf("%+v", e)
	}
	wantErr(t, s.UnlockUser(root, 99), 404, "不存在")

	// 重置密码：需满足策略；新密码可用且不进审计
	wantErr(t, s.ResetPassword(root, 3, "123"), 400, "太短")
	wantErr(t, s.ResetPassword(root, 3, "bob-pass-1234"), 400, "用户名")
	wantErr(t, s.ResetPassword(root, 99, "T7#kd9-Qz"), 404, "不存在")
	if err := s.ResetPassword(root, 3, "N3w#Pass-77"); err != nil {
		t.Fatal(err)
	}
	if !crypto.CheckDjangoPassword("N3w#Pass-77", f.users[3].Password) {
		t.Fatal("新密码应生效")
	}
	if e := lastAudit(t, f); e.Action != "重置密码" || strings.Contains(e.ExtraInfo, "N3w") || strings.Contains(e.ExtraInfo, "pbkdf2") {
		t.Fatalf("%+v", e)
	}

	// 重置 2FA
	wantErr(t, s.ResetTwoFA(root, 3), 409, "没有绑定")
	f.twofa[3] = []string{"totp", "sms"}
	if err := s.ResetTwoFA(root, 3); err != nil || len(f.twofa[3]) != 0 {
		t.Fatal("重置 2FA 失败", err)
	}
	if e := lastAudit(t, f); e.Action != "重置 2FA" || !strings.Contains(e.ExtraInfo, "删除 2 条") {
		t.Fatalf("%+v", e)
	}
}

func TestListUsersFiltersAndLock(t *testing.T) {
	s, f := setup()
	recent := fixedNow.Add(-2 * time.Minute)
	old := fixedNow.Add(-10 * time.Minute)
	u := f.users[2]
	u.FailedLoginCount, u.LastLoginFailedAt = 5, &recent
	f.users[2] = u
	u = f.users[3]
	u.FailedLoginCount, u.LastLoginFailedAt = 7, &old // 锁定窗口（默认 300 秒）已过
	f.users[3] = u
	f.twofa[3] = []string{"totp"}

	page, err := s.ListUsers(UserQuery{})
	if err != nil || page.Total != 4 || page.Pages != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	by := map[string]UserItem{}
	for _, r := range page.Rows {
		by[r.Username] = r
	}
	if a := by["alice"]; !a.Locked || a.LockRemaining < 170 || a.LockRemaining > 181 || len(a.AuthGroups) != 1 || a.AuthGroups[0].Name != "TL" || a.ResourceGroups[0].Name != "研发组" {
		t.Fatalf("%+v", a)
	}
	if b := by["bob"]; b.Locked || len(b.TwoFA) != 1 || b.TwoFA[0] != "totp" {
		t.Fatalf("锁定窗口外不应锁定: %+v", b)
	}
	names := func(q UserQuery) string {
		p, err := s.ListUsers(q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range p.Rows {
			out = append(out, r.Username)
		}
		return strings.Join(out, ",")
	}
	if got := names(UserQuery{Status: StatusLocked}); got != "alice" {
		t.Errorf("locked = %q", got)
	}
	if got := names(UserQuery{Status: StatusInactive}); got != "carol" {
		t.Errorf("inactive = %q", got)
	}
	if got := names(UserQuery{Status: StatusNo2FA}); got != "alice,carol,root" {
		t.Errorf("no2fa = %q", got)
	}
	if got := names(UserQuery{AuthGroupID: 20}); got != "bob" {
		t.Errorf("auth group = %q", got)
	}
	if got := names(UserQuery{ResourceGroupID: 1}); got != "alice,bob" {
		t.Errorf("resource group = %q", got)
	}
	if got := names(UserQuery{Search: "ali"}); got != "alice" {
		t.Errorf("search = %q", got)
	}
	if got := names(UserQuery{PageSize: 2, Page: 2}); got != "carol,root" {
		t.Errorf("page 2 = %q", got)
	}
	if _, err := s.ListUsers(UserQuery{Status: "bogus"}); err == nil {
		t.Error("未知状态应报错")
	}
	// 锁定策略读 dbt_sql_config
	f.cfg[settings.KeySecurityLoginLockAttempts] = "8"
	if got := names(UserQuery{Status: StatusLocked}); got != "" {
		t.Errorf("阈值 8 时不应有人被锁: %q", got)
	}
	f.cfg[settings.KeySecurityLoginLockAttempts] = "abc" // 非法值回退默认 5
	if got := names(UserQuery{Status: StatusLocked}); got != "alice" {
		t.Errorf("非法配置应回退默认: %q", got)
	}
}

func TestGetUserHidesSecrets(t *testing.T) {
	s, f := setup()
	u := f.users[3]
	u.Password, u.DingUserID, u.FeishuOpenID = "pbkdf2_sha256$1$salt$hash", "ding1", "ou_x"
	f.users[3] = u
	d, err := s.GetUser(3)
	if err != nil || d.DingUserID != "ding1" || d.FeishuOpenID != "ou_x" || len(d.AuthGroupIDs) != 1 || d.AuthGroupIDs[0] != 20 || !d.HasDing || !d.HasFeishu || d.HasSlack {
		t.Fatalf("%+v %v", d, err)
	}
	wantErr(t, func() error { _, err := s.GetUser(99); return err }(), 404, "不存在")
}

// ---- 权限组 ----

func TestAuthGroupLifecycle(t *testing.T) {
	s, f := setup()
	wantErr(t, func() error { _, err := s.CreateAuthGroup(root, AuthGroupInput{Name: " "}); return err }(), 400, "名称")
	wantErr(t, func() error { _, err := s.CreateAuthGroup(root, AuthGroupInput{Name: "a,b"}); return err }(), 400, "逗号")
	wantErr(t, func() error { _, err := s.CreateAuthGroup(root, AuthGroupInput{Name: "dba"}); return err }(), 409, "同名")
	wantErr(t, func() error {
		_, err := s.CreateAuthGroup(root, AuthGroupInput{Name: "QA", Description: strp(strings.Repeat("长", 201))})
		return err
	}(), 400, "200")
	if len(f.authGroups) != 3 {
		t.Fatal("失败不应创建")
	}
	id, err := s.CreateAuthGroup(root, AuthGroupInput{Name: "QA", Description: strp("测试组")})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetAuthGroup(id)
	if d.Name != "QA" || d.Description != "测试组" || len(d.Members) != 0 {
		t.Fatalf("%+v", d)
	}
	if e := lastAudit(t, f); e.Action != "新建权限组" || !strings.Contains(e.ExtraInfo, "「QA」") || !strings.Contains(e.ExtraInfo, "说明：测试组") {
		t.Fatalf("%+v", e)
	}
	// 改名 + 改说明
	if err := s.UpdateAuthGroup(root, id, AuthGroupInput{Name: "QA2", Description: strp("新说明")}); err != nil {
		t.Fatal(err)
	}
	e := lastAudit(t, f)
	if e.Action != "修改权限组" || !strings.Contains(e.ExtraInfo, "QA → QA2") || !strings.Contains(e.ExtraInfo, "测试组 → 新说明") {
		t.Fatalf("%+v", e)
	}
	wantErr(t, s.UpdateAuthGroup(root, id, AuthGroupInput{Name: "DBA"}), 409, "同名")
	wantErr(t, s.UpdateAuthGroup(root, 999, AuthGroupInput{Name: "x"}), 404, "不存在")
	// 只改大小写（同一个组）允许
	if err := s.UpdateAuthGroup(root, id, AuthGroupInput{Name: "qa2"}); err != nil {
		t.Fatal(err)
	}

	// 说明缺省（nil）= 不动；什么都没变也留痕，写明无变化。空串才是清空
	if err := s.UpdateAuthGroup(root, id, AuthGroupInput{Name: "qa2"}); err != nil {
		t.Fatal(err)
	}
	if e := lastAudit(t, f); !strings.Contains(e.ExtraInfo, "无变化") {
		t.Fatalf("%+v", e)
	}
	if d, _ := s.GetAuthGroup(id); d.Description != "新说明" {
		t.Fatalf("没带 description 时说明应保持不变: %+v", d)
	}
	if err := s.UpdateAuthGroup(root, id, AuthGroupInput{Name: "qa2", Description: strp("")}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetAuthGroup(id); d.Description != "" {
		t.Fatalf("空串应清空说明: %+v", d)
	}

	// 删除：被审批流引用时拒绝
	f.settings[[2]int{1, 1}] = model.WorkflowAuditSetting{GroupID: 1, WorkflowType: 1, AuditAuthGroups: "10," + itoa(id)}
	wantErr(t, s.DeleteAuthGroup(root, id), 409, "审批流引用")
	delete(f.settings, [2]int{1, 1})
	f.pending[id] = 3
	wantErr(t, s.DeleteAuthGroup(root, id), 409, "待审批")
	f.pending[id] = 0
	f.link(f.userGroups, 3, id)
	f.grant(id, "sqlworkflow.approve")
	if err := s.DeleteAuthGroup(root, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.authGroups[id]; ok || f.userGroups[3][id] {
		t.Fatal("应删除权限组并解除成员")
	}
	if e := lastAudit(t, f); !strings.Contains(e.ExtraInfo, "解除 1 个成员") {
		t.Fatalf("%q", e.ExtraInfo)
	}
	wantErr(t, s.DeleteAuthGroup(root, id), 404, "不存在")
}

func itoa(n uint) string { return formatFlowIDs([]uint{n}) }

func TestAuthGroupList(t *testing.T) {
	s, f := setup()
	f.settings[[2]int{1, 2}] = model.WorkflowAuditSetting{GroupID: 1, WorkflowType: 2, AuditAuthGroups: "10,20"}
	f.settings[[2]int{2, 2}] = model.WorkflowAuditSetting{GroupID: 2, WorkflowType: 2, AuditAuthGroups: "20"}
	list, err := s.ListAuthGroups()
	if err != nil || len(list) != 3 {
		t.Fatalf("%+v %v", list, err)
	}
	by := map[string]AuthGroupItem{}
	for _, g := range list {
		by[g.Name] = g
	}
	if g := by["DBA"]; g.Members != 1 || g.Flows != 2 {
		t.Fatalf("%+v", g)
	}
	if g := by["TL"]; g.Flows != 1 {
		t.Fatalf("%+v", g)
	}
	d, _ := s.GetAuthGroup(20)
	if len(d.Members) != 1 || d.Members[0].Username != "bob" || len(d.Flows) != 2 || d.Flows[0].Level != 2 || d.Flows[0].GroupName != "研发组" {
		t.Fatalf("%+v", d)
	}
}

// ---- 审批流 ----

func TestSaveFlowValidation(t *testing.T) {
	s, f := setup()
	save := func(g uint, wt int, ids ...uint) error { _, err := s.SaveFlow(root, g, wt, ids); return err }
	wantErr(t, save(1, 2), 400, "不能为空")
	wantErr(t, save(1, 2, 10, 20, 10), 400, "不能在同一条审批流里选两次：TL")
	wantErr(t, save(1, 2, 10, 999), 400, "#999 不存在")
	wantErr(t, save(1, 9, 10), 400, "工单类型")
	wantErr(t, save(77, 2, 10), 404, "不存在或已停用")
	_ = s.DisableResourceGroup(root, 2)
	wantErr(t, save(2, 2, 10), 404, "不存在或已停用")
	many := make([]uint, 11)
	for i := range many {
		many[i] = uint(i + 1)
		f.authGroups[uint(i+1)] = "G" + itoa(uint(i+1))
	}
	wantErr(t, save(1, 2, many...), 400, "最多")
	if len(f.settings) != 0 {
		t.Fatal("校验失败不应保存")
	}
}

func TestSaveFlowAndAuditFormat(t *testing.T) {
	s, f := setup()
	res, err := s.SaveFlow(root, 1, WorkflowSQL, []uint{10, 20})
	if err != nil || !res.Changed || res.Before != "（未配置）" || res.After != "TL → DBA" {
		t.Fatalf("%+v %v", res, err)
	}
	st := f.settings[[2]int{1, 2}]
	if st.AuditAuthGroups != "10,20" || st.GroupName != "研发组" {
		t.Fatalf("存库格式必须是逗号分隔 ID（无空格）: %+v", st)
	}
	if e := lastAudit(t, f); e.Action != "修改审批流" || e.ExtraInfo != "研发组 · SQL上线申请：（未配置） ⇒ TL → DBA" {
		t.Fatalf("%+v", e)
	}
	// 修改：加一级
	res, err = s.SaveFlow(root, 1, WorkflowSQL, []uint{10, 20, 30})
	if err != nil || res.Before != "TL → DBA" || res.After != "TL → DBA → RD" || f.settings[[2]int{1, 2}].AuditAuthGroups != "10,20,30" {
		t.Fatalf("%+v %v", res, err)
	}
	if e := lastAudit(t, f); e.ExtraInfo != "研发组 · SQL上线申请：TL → DBA ⇒ TL → DBA → RD" {
		t.Fatalf("%q", e.ExtraInfo)
	}
	// 相同内容：无变化，不改库
	n := len(f.audits)
	res, err = s.SaveFlow(root, 1, WorkflowSQL, []uint{10, 20, 30})
	if err != nil || res.Changed {
		t.Fatalf("%+v %v", res, err)
	}
	if len(f.audits) != n+1 || !strings.Contains(lastAudit(t, f).ExtraInfo, "无变化") {
		t.Fatal("无变化也应记一条审计")
	}
	// 兼容：旧数据里带空格的格式也能识别为“相同”
	f.settings[[2]int{1, 2}] = model.WorkflowAuditSetting{AuditSettingID: 1, GroupID: 1, WorkflowType: 2, AuditAuthGroups: "10,20,30"}
	// 删除
	if err := s.DeleteFlow(root, 1, WorkflowSQL); err != nil {
		t.Fatal(err)
	}
	if e := lastAudit(t, f); e.Action != "删除审批流" || !strings.Contains(e.ExtraInfo, "⇒ （未配置）") {
		t.Fatalf("%+v", e)
	}
	wantErr(t, s.DeleteFlow(root, 1, WorkflowSQL), 404, "没有配置")
	wantErr(t, s.DeleteFlow(root, 1, 9), 400, "工单类型")
}

// auditCompat 把 fakeRepo 包装成 internal/audit 的 Repo，用来验证本包保存的审批流能被审批引擎正确使用。
type auditCompat struct {
	*fakeRepo
	created []*model.WorkflowAudit
	logs    []*model.WorkflowLog
}

func (a *auditCompat) AuditSetting(g uint, wt int) (*model.WorkflowAuditSetting, error) {
	return a.GetAuditSetting(g, wt)
}
func (a *auditCompat) AuthGroup(id uint) (*model.AuthGroup, error) { return a.GetAuthGroup(id) }
func (a *auditCompat) UserInAuthGroup(u, g uint) (bool, error)     { return a.userGroups[u][g], nil }
func (a *auditCompat) AuthGroupUsers(names []string, rg uint) ([]model.User, error) {
	var out []model.User
	for uid, u := range a.users {
		for _, n := range names {
			for gid := range a.userGroups[uid] {
				if a.authGroups[gid] == n && a.userRGs[uid][rg] && u.IsActive {
					out = append(out, u)
				}
			}
		}
	}
	return out, nil
}
func (a *auditCompat) InsertAudit(w *model.WorkflowAudit) error {
	w.AuditID = uint(len(a.created) + 1)
	a.created = append(a.created, w)
	return nil
}
func (a *auditCompat) SaveAudit(*model.WorkflowAudit) error          { return nil }
func (a *auditCompat) InsertDetail(*model.WorkflowAuditDetail) error { return nil }
func (a *auditCompat) InsertLog(l *model.WorkflowLog) error          { a.logs = append(a.logs, l); return nil }
func (a *auditCompat) HasButton(u uint, code string) (bool, error) {
	return a.hasButton(u, code), nil
}

func TestSavedFlowIsUsableByAuditEngine(t *testing.T) {
	s, f := setup()
	if _, err := s.SaveFlow(root, 1, WorkflowQuery, []uint{20, 10, 30}); err != nil {
		t.Fatal(err)
	}
	ac := &auditCompat{fakeRepo: f}
	a, err := audit.Create(ac, fixedNow, audit.Spec{WorkflowType: audit.TypeQueryPrivilege, WorkflowID: 7, GroupID: 1, GroupName: "研发组", Title: "t", CreateUser: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if a.AuditAuthGroups != "20,10,30" || a.CurrentAudit != "20" || a.NextAudit != "10" {
		t.Fatalf("审批引擎应按保存的顺序逐级流转: %+v", a)
	}
	if len(ac.logs) != 1 || !strings.Contains(ac.logs[0].OperationInfo, "DBA->TL->RD") {
		t.Fatalf("%+v", ac.logs)
	}
	// 引擎里的类型常量与本包一致
	if audit.TypeQueryPrivilege != WorkflowQuery || audit.TypeSQLReview != WorkflowSQL || audit.TypeArchive != WorkflowArchive {
		t.Fatal("工单类型数值必须与 internal/audit 一致")
	}
}

func TestFlowDetailWarnings(t *testing.T) {
	s, f := setup()
	f.grant(20, "sqlworkflow.approve")
	// 研发组：TL（alice，但 TL 没有 sql_review）→ DBA（bob，有）→ RD（没有成员）
	f.settings[[2]int{1, 2}] = model.WorkflowAuditSetting{GroupID: 1, WorkflowType: 2, AuditAuthGroups: "10,20,30"}
	v, err := s.FlowDetail(1, WorkflowSQL)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Configured || v.Text != "TL → DBA → RD" || len(v.Levels) != 3 {
		t.Fatalf("%+v", v)
	}
	if l := v.Levels[0]; len(l.Approvers) != 1 || l.Approvers[0].Username != "alice" || l.Approvers[0].HasPerm || !strings.Contains(l.Warning, "sqlworkflow.approve") {
		t.Fatalf("TL 级应警告成员没有审核权限: %+v", l)
	}
	if l := v.Levels[1]; l.Warning != "" || len(l.Approvers) != 1 || !l.Approvers[0].HasPerm {
		t.Fatalf("%+v", l)
	}
	if l := v.Levels[2]; !strings.Contains(l.Warning, "该级无人可审") || len(l.Approvers) != 0 {
		t.Fatalf("%+v", l)
	}
	if len(v.Warnings) != 2 {
		t.Fatalf("%v", v.Warnings)
	}
	// 未配置
	v, _ = s.FlowDetail(1, WorkflowQuery)
	if v.Configured || len(v.Levels) != 0 {
		t.Fatalf("%+v", v)
	}
	// 权限组被删除 / 配置里有垃圾内容：给出警告而不是报错
	f.settings[[2]int{1, 3}] = model.WorkflowAuditSetting{GroupID: 1, WorkflowType: 3, AuditAuthGroups: " 10, x ,,999"}
	v, err = s.FlowDetail(1, WorkflowArchive)
	if err != nil || len(v.Levels) != 2 || v.Levels[1].Exists || !strings.Contains(v.Levels[1].Warning, "不存在") || v.Levels[1].AuthGroupName != "#999" {
		t.Fatalf("%+v %v", v, err)
	}
	foundInvalid := false
	for _, w := range v.Warnings {
		foundInvalid = foundInvalid || strings.Contains(w, "x")
	}
	if !foundInvalid {
		t.Fatalf("应提示无法识别的内容: %v", v.Warnings)
	}
	wantErr(t, func() error { _, err := s.FlowDetail(1, 9); return err }(), 400, "工单类型")
	wantErr(t, func() error { _, err := s.FlowDetail(88, 1); return err }(), 404, "不存在")
}

func TestPreviewFlow(t *testing.T) {
	s, f := setup()
	v, err := s.PreviewFlow(1, WorkflowSQL, []uint{20, 10})
	if err != nil || v.Text != "DBA → TL" || len(v.Warnings) != 2 {
		t.Fatalf("没有授予审核权限，两级都应警告: %+v %v", v, err)
	}
	if len(f.settings) != 0 || len(f.audits) != 0 {
		t.Fatal("预览不应写库")
	}
	wantErr(t, func() error { _, err := s.PreviewFlow(1, WorkflowSQL, []uint{10, 10}); return err }(), 400, "两次")
}

func TestFlowOverview(t *testing.T) {
	s, f := setup()
	f.grant(20, "sqlworkflow.approve")
	f.settings[[2]int{1, 2}] = model.WorkflowAuditSetting{GroupID: 1, WorkflowType: 2, AuditAuthGroups: "20"}
	f.settings[[2]int{1, 1}] = model.WorkflowAuditSetting{GroupID: 1, WorkflowType: 1, AuditAuthGroups: "30"} // RD 没有成员
	f.settings[[2]int{2, 2}] = model.WorkflowAuditSetting{GroupID: 2, WorkflowType: 2, AuditAuthGroups: ""}   // 空流程
	f.settings[[2]int{2, 1}] = model.WorkflowAuditSetting{GroupID: 2, WorkflowType: 1, AuditAuthGroups: ""}
	o, err := s.FlowOverview()
	if err != nil || len(o.Matrix) != 2 || len(o.Types) != 3 || len(o.AuthGroups) != 3 {
		t.Fatalf("%+v %v", o, err)
	}
	cell := func(g, wt int) MatrixCell {
		for _, r := range o.Matrix {
			if int(r.GroupID) == g {
				for _, c := range r.Cells {
					if c.Type == wt {
						return c
					}
				}
			}
		}
		t.Fatalf("没有格子 %d/%d", g, wt)
		return MatrixCell{}
	}
	if c := cell(1, 2); !c.Configured || c.Text != "DBA" || c.Warn {
		t.Fatalf("%+v", c)
	}
	if c := cell(1, 1); !c.Configured || !c.Warn || !strings.Contains(c.WarnText, "无人可审") {
		t.Fatalf("%+v", c)
	}
	if c := cell(1, 3); c.Configured || c.Warn {
		t.Fatalf("%+v", c)
	}
	if c := cell(2, 2); !c.Configured || !c.Empty || c.Warn {
		t.Fatalf("SQL 上线的空流程是合法的（自动审核规则决定）: %+v", c)
	}
	if c := cell(2, 1); !c.Empty || !c.Warn {
		t.Fatalf("查询权限申请的空流程应警告: %+v", c)
	}
	// 已停用的资源组不在矩阵里
	_ = s.DisableResourceGroup(root, 2)
	if o, _ = s.FlowOverview(); len(o.Matrix) != 1 {
		t.Fatalf("%+v", o.Matrix)
	}
}

// ---- 系统审计 ----

func TestAuditLogListAndFilters(t *testing.T) {
	s, f := setup()
	mk := func(day int, user, action, extra string) {
		_ = f.InsertAuditLog(&model.AuditEntry{UserID: 1, UserName: user, UserDisplay: user + "名", Action: action, ExtraInfo: extra,
			ActionTime: time.Date(2026, 10, day, 10, 0, 0, 0, time.Local)})
	}
	mk(1, "wang", "登入失败", "IP 10.0.0.1")
	mk(2, "dba1", "修改审批流", "研发组 · SQL上线申请")
	mk(3, "dev", "登入", "IP 10.0.0.2")
	mk(4, "dev", "下载导出文件", "工单 #130")
	p, err := s.ListAuditLogs(AuditQuery{})
	if err != nil || p.Total != 4 || p.Rows[0].Action != "下载导出文件" || p.Rows[0].ActionTime != "2026-10-04 10:00:00" {
		t.Fatalf("%+v %v", p, err)
	}
	cases := []struct {
		q    AuditQuery
		want int64
	}{
		{AuditQuery{Action: "登入"}, 1},
		{AuditQuery{User: "dev"}, 2},
		{AuditQuery{Search: "审批流"}, 1},
		{AuditQuery{StartDate: "2026-10-02", EndDate: "2026-10-03"}, 2}, // 含结束当天
		{AuditQuery{StartDate: "2026-10-04"}, 1},
		{AuditQuery{EndDate: "2026-10-01"}, 1},
	}
	for i, c := range cases {
		if p, err := s.ListAuditLogs(c.q); err != nil || p.Total != c.want {
			t.Errorf("#%d: total=%v err=%v 期望 %d", i, p, err, c.want)
		}
	}
	for _, q := range []AuditQuery{{StartDate: "bad"}, {EndDate: "2026/10/01"}, {StartDate: "2026-10-05", EndDate: "2026-10-01"}} {
		if _, err := s.ListAuditLogs(q); err == nil || httpCode(t, err) != 400 {
			t.Errorf("%+v 应 400: %v", q, err)
		}
	}
	p, _ = s.ListAuditLogs(AuditQuery{PageSize: 3, Page: 2})
	if len(p.Rows) != 1 || p.Pages != 2 || p.Page != 2 {
		t.Fatalf("%+v", p)
	}
	// 页大小上限
	if p, _ = s.ListAuditLogs(AuditQuery{PageSize: 100000}); p.PageSize != 200 {
		t.Fatalf("page size = %d", p.PageSize)
	}
	// 动作列表有缓存
	acts, _ := s.AuditActions()
	if len(acts) != 4 {
		t.Fatalf("%v", acts)
	}
	mk(5, "x", "新动作", "")
	if acts, _ = s.AuditActions(); len(acts) != 4 {
		t.Fatal("一分钟内应走缓存")
	}
	s.now = func() time.Time { return fixedNow.Add(2 * time.Minute) }
	if acts, _ = s.AuditActions(); len(acts) != 5 {
		t.Fatalf("缓存过期后应刷新: %v", acts)
	}
}

func TestExportAuditLogs(t *testing.T) {
	s, f := setup()
	_ = f.InsertAuditLog(&model.AuditEntry{UserName: "a", Action: "登入", ActionTime: fixedNow})
	_ = f.InsertAuditLog(&model.AuditEntry{UserName: "b", Action: "登出", ActionTime: fixedNow})
	rows, truncated, err := s.ExportAuditLogs(alice, AuditQuery{Action: "登入"})
	if err != nil || truncated || len(rows) != 1 {
		t.Fatalf("%+v %v %v", rows, truncated, err)
	}
	if e := lastAudit(t, f); e.Action != "导出审计日志" || e.UserName != "alice" || !strings.Contains(e.ExtraInfo, "导出 1 行") {
		t.Fatalf("导出本身要写审计: %+v", e)
	}
	if _, _, err := s.ExportAuditLogs(alice, AuditQuery{StartDate: "x"}); err == nil {
		t.Fatal("日期非法应失败")
	}
}

func TestQueryLogAndSQLWorkflowAudit(t *testing.T) {
	s, f := setup()
	long := strings.Repeat("x", 1500)
	f.queryLogs = []model.QueryLog{
		{ID: 2, InstanceName: "prod-1", DBName: "shop", SQLLog: "select 1", Username: "dev", UserDisplay: "开发", CreateTime: fixedNow, EffectRow: 1, CostTime: "0.01"},
		{ID: 1, InstanceName: "prod-2", DBName: "shop", SQLLog: long, Username: "dba", UserDisplay: "DBA", CreateTime: fixedNow},
	}
	p, err := s.ListQueryLogs(QueryAuditQuery{})
	if err != nil || p.Total != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	if r := p.Rows[1]; !r.SQLTruncated || len([]rune(r.SQL)) != 1000 {
		t.Fatalf("长 SQL 应截断: %v %d", r.SQLTruncated, len(r.SQL))
	}
	if p.Rows[0].SQLTruncated || p.Rows[0].CreateTime != "2026-10-06 09:00:00" {
		t.Fatalf("%+v", p.Rows[0])
	}
	if p, _ = s.ListQueryLogs(QueryAuditQuery{User: "dba"}); p.Total != 1 {
		t.Fatalf("%+v", p)
	}
	if p, _ = s.ListQueryLogs(QueryAuditQuery{Instance: "prod-1", Search: "select"}); p.Total != 1 {
		t.Fatalf("%+v", p)
	}
	if _, err = s.ListQueryLogs(QueryAuditQuery{StartDate: "nope"}); err == nil {
		t.Fatal("日期非法应失败")
	}

	fin := fixedNow.Add(time.Hour)
	f.workflows = []fakeWorkflow{
		{SQLWorkflowRow{ID: 5, Name: "加索引", Engineer: "dev", EngineerDisplay: "开发", Status: model.WfFinish, GroupName: "研发组", InstanceName: "prod-1", DBName: "shop", SyntaxType: 1, CreateTime: fixedNow, FinishTime: &fin}, 1},
		{SQLWorkflowRow{ID: 6, Name: "导数据", Engineer: "dev", EngineerDisplay: "开发", Status: model.WfManReviewing, GroupName: "数据组", SyntaxType: 3, CreateTime: fixedNow}, 2},
	}
	w, err := s.ListSQLWorkflows(SQLWorkflowQuery{})
	if err != nil || w.Total != 2 {
		t.Fatalf("%+v %v", w, err)
	}
	if r := w.Rows[0]; r.StatusLabel != "已正常结束" || r.SyntaxType != "DDL" || r.FinishTime != "2026-10-06 10:00:00" || r.InstanceName != "prod-1" {
		t.Fatalf("%+v", r)
	}
	if r := w.Rows[1]; r.SyntaxType != "离线导出" || r.FinishTime != "" || r.StatusLabel != "等待审批" {
		t.Fatalf("%+v", r)
	}
	if w, _ = s.ListSQLWorkflows(SQLWorkflowQuery{Status: model.WfManReviewing}); w.Total != 1 {
		t.Fatalf("%+v", w)
	}
	if w, _ = s.ListSQLWorkflows(SQLWorkflowQuery{GroupID: 1, Search: "索引"}); w.Total != 1 {
		t.Fatalf("%+v", w)
	}
	if _, err = s.ListSQLWorkflows(SQLWorkflowQuery{Status: "bogus"}); err == nil || httpCode(t, err) != 400 {
		t.Fatalf("%v", err)
	}
	o, err := s.AuditOptions()
	if err != nil || len(o.SQLStatuses) != 9 || len(o.ResGroups) != 2 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestPasswordPolicyFromAuthSettings(t *testing.T) {
	s, f := setup()
	// 默认：9 位
	if _, err := s.CreateUser(root, func() UserInput { u := goodUser(); u.Password = "Ab#12345"; return u }()); err == nil || !strings.Contains(err.Error(), "至少需要 9") {
		t.Fatalf("默认应要求 9 位: %v", err)
	}
	// 配置了更长的最小长度与复杂度
	f.authCfg[settings.KeySecurityPasswordMinLength] = "12"
	f.authCfg[settings.KeySecurityPasswordComplexity] = "true"
	in := goodUser()
	in.Password = "T7#kd9-Qz" // 9 位，满足默认但不满足 12 位
	if _, err := s.CreateUser(root, in); err == nil || !strings.Contains(err.Error(), "至少需要 12") {
		t.Fatalf("应按配置要求 12 位: %v", err)
	}
	in.Password = "Tkd9Qzabcdefg" // 13 位，没有特殊字符
	if _, err := s.CreateUser(root, in); err == nil || !strings.Contains(err.Error(), "特殊字符") {
		t.Fatalf("应要求复杂度: %v", err)
	}
	in.Password = "T7#kd9-Qz-abc"
	if _, err := s.CreateUser(root, in); err != nil {
		t.Fatal(err)
	}
	// 重置密码同样走策略；配置值非法时回退默认
	if err := s.ResetPassword(root, 3, "T7#kd9-Qz"); err == nil {
		t.Fatal("重置密码也应按策略校验")
	}
	f.authCfg[settings.KeySecurityPasswordMinLength] = "abc"
	f.authCfg[settings.KeySecurityPasswordComplexity] = "false"
	if err := s.ResetPassword(root, 3, "T7#kd9-Qz"); err != nil {
		t.Fatalf("非法配置应回退默认策略: %v", err)
	}
}
