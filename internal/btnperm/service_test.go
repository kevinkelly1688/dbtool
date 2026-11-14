// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package btnperm

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func newSvc(f *fakeRepo) (*Service, *Resolver) {
	r := newRes(f)
	s := NewService(f, r)
	s.SetClock(fixedNow)
	return s, r
}

var admin = Actor{ID: 1, Username: "root", Display: "管理员"}

func baseFake() *fakeRepo {
	f := newFake()
	f.addUser(1, "root", true, true)
	f.addUser(2, "alice", true, false)
	f.addUser(3, "bob", true, false)
	f.addGroup(10, "RD", 2, 3)
	f.addGroup(11, "TL")
	return f
}

func wantErr(t *testing.T, err error, status int, contains string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("期望业务错误，得到 %v", err)
	}
	if e.HTTP != status || !strings.Contains(e.Message, contains) {
		t.Fatalf("错误 = %d %q，期望 %d 含 %q", e.HTTP, e.Message, status, contains)
	}
}

func TestSaveGroupDiffAuditAndCacheInvalidation(t *testing.T) {
	f := baseFake()
	s, r := newSvc(f)
	f.grant(10, "sqlworkflow.view_own", "sqlworkflow.submit")
	// 预热缓存：alice 此时没有 approve
	if ok(r.HasButton(2, "sqlworkflow.approve")) {
		t.Fatal("初始不应有")
	}

	res, err := s.SaveGroup(admin, 10, []string{"sqlworkflow.view_own", "sqlworkflow.approve", "sqlworkflow.approve", " "}, "补审批")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || strings.Join(res.Added, ",") != "sqlworkflow.approve" || strings.Join(res.Removed, ",") != "sqlworkflow.submit" || res.Members != 2 || res.Total != 2 {
		t.Fatalf("结果 = %+v", res)
	}
	if !ok(r.HasButton(2, "sqlworkflow.approve")) || ok(r.HasButton(2, "sqlworkflow.submit")) {
		t.Fatal("保存后缓存应已失效并立即生效")
	}
	if len(f.audits) != 1 {
		t.Fatalf("应写 1 条审计，实际 %d", len(f.audits))
	}
	a := f.audits[0]
	if a.Action != "修改按钮权限" || a.UserName != "root" || a.UserID != 1 ||
		!strings.Contains(a.ExtraInfo, "RD") || !strings.Contains(a.ExtraInfo, "新增 sqlworkflow.approve") || !strings.Contains(a.ExtraInfo, "移除 sqlworkflow.submit") || !strings.Contains(a.ExtraInfo, "原因：补审批") {
		t.Fatalf("审计 = %+v", a)
	}
	// 没有变化：不写库不写审计
	res, err = s.SaveGroup(admin, 10, []string{"sqlworkflow.approve", "sqlworkflow.view_own"}, "")
	if err != nil || res.Changed || len(f.audits) != 1 {
		t.Fatalf("无变化应是空操作: %+v %v audits=%d", res, err, len(f.audits))
	}
	// 只勾操作按钮、没有查看按钮：给出提示但保存成功
	res, err = s.SaveGroup(admin, 11, []string{"sqlworkflow.approve"}, "")
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "变更工单") {
		t.Fatalf("应有无查看按钮的提示: %+v %v", res, err)
	}
}

func TestSaveGroupValidation(t *testing.T) {
	f := baseFake()
	s, _ := newSvc(f)
	_, err := s.SaveGroup(admin, 10, []string{"no.such"}, "")
	wantErr(t, err, 400, "未知的按钮权限码")
	_, err = s.SaveGroup(admin, 10, []string{"user.edit"}, "")
	wantErr(t, err, 400, "仅限超级用户")
	_, err = s.SaveGroup(admin, 99, []string{"sqlworkflow.submit"}, "")
	wantErr(t, err, 404, "权限组不存在")
	_, err = s.SaveGroup(admin, 10, nil, strings.Repeat("长", 201))
	wantErr(t, err, 400, "原因过长")
	if len(f.audits) != 0 || len(f.groupBtns[10]) != 0 {
		t.Fatal("校验失败不应有任何写入")
	}
}

func TestSaveGroupRollbackWhenAuditFails(t *testing.T) {
	f := baseFake()
	s, _ := newSvc(f)
	f.failTx = errors.New("审计写失败")
	if _, err := s.SaveGroup(admin, 10, []string{"sqlworkflow.submit"}, ""); err == nil {
		t.Fatal("应失败")
	}
	if len(f.groupBtns[10]) != 0 || len(f.audits) != 0 {
		t.Fatal("事务失败应整体回滚")
	}
}

func TestGroupDiffIsDryRun(t *testing.T) {
	f := baseFake()
	f.grant(10, "sqlworkflow.submit")
	s, _ := newSvc(f)
	d, err := s.GroupDiff(10, []string{"sqlworkflow.view_own"})
	if err != nil || len(d.Added) != 1 || len(d.Removed) != 1 {
		t.Fatalf("%+v %v", d, err)
	}
	if !f.groupBtns[10]["sqlworkflow.submit"] || len(f.audits) != 0 {
		t.Fatal("diff 不应写库")
	}
	gv, err := s.Group(10)
	if err != nil || len(gv.Codes) != 1 || gv.Group.Name != "RD" {
		t.Fatalf("%+v %v", gv, err)
	}
	f.groupBtns[10]["legacy.removed"] = true // 登记表里已经没有的历史码：读取时忽略
	gv, _ = s.Group(10)
	if len(gv.Codes) != 1 {
		t.Fatalf("历史码应被忽略: %v", gv.Codes)
	}
}

func TestSetOverrides(t *testing.T) {
	f := baseFake()
	s, r := newSvc(f)
	f.grant(10, "export.download") // bob 所在的 RD 组授予了下载
	r.InvalidateAll()
	if !ok(r.HasButton(2, "export.download")) {
		t.Fatal("权限组授予应通过")
	}
	future := fixedNow().Add(48 * time.Hour)

	// 原因必填
	wantErr(t, s.SetOverrides(admin, 2, OverrideReq{Effect: EffectDeny, Codes: []string{"export.download"}}), 400, "原因")
	wantErr(t, s.SetOverrides(admin, 2, OverrideReq{Effect: "x", Codes: []string{"export.download"}, Reason: "r"}), 400, "方式")
	wantErr(t, s.SetOverrides(admin, 2, OverrideReq{Effect: EffectDeny, Reason: "r"}), 400, "至少一个")
	wantErr(t, s.SetOverrides(admin, 2, OverrideReq{Effect: EffectDeny, Codes: []string{"user.edit"}, Reason: "r"}), 400, "仅限超级用户")
	past := fixedNow().Add(-time.Hour)
	wantErr(t, s.SetOverrides(admin, 2, OverrideReq{Effect: EffectAllow, Codes: []string{"export.download"}, ExpireAt: &past, Reason: "r"}), 400, "到期时间")
	wantErr(t, s.SetOverrides(admin, 99, OverrideReq{Effect: EffectAllow, Codes: []string{"export.download"}, Reason: "r"}), 404, "用户不存在")
	wantErr(t, s.SetOverrides(admin, 1, OverrideReq{Effect: EffectAllow, Codes: []string{"export.download"}, Reason: "r"}), 400, "超级用户")
	if len(f.audits) != 0 {
		t.Fatal("失败不应有审计")
	}

	// 单独禁用（带到期）压过权限组授予
	if err := s.SetOverrides(admin, 2, OverrideReq{Effect: EffectDeny, Codes: []string{"export.download"}, ExpireAt: &future, Reason: "合规要求"}); err != nil {
		t.Fatal(err)
	}
	if ok(r.HasButton(2, "export.download")) {
		t.Fatal("禁用应立即生效")
	}
	a := f.audits[0]
	if a.Action != "单独禁用按钮" || !strings.Contains(a.ExtraInfo, "alice") || !strings.Contains(a.ExtraInfo, "export.download") || !strings.Contains(a.ExtraInfo, "合规要求") || !strings.Contains(a.ExtraInfo, "到期") {
		t.Fatalf("审计 = %+v", a)
	}
	// 改成单独授权：覆盖同码记录，审计里有变更前后
	if err := s.SetOverrides(admin, 2, OverrideReq{Effect: EffectAllow, Codes: []string{"export.download"}, Reason: "解除"}); err != nil {
		t.Fatal(err)
	}
	if !ok(r.HasButton(2, "export.download")) || len(f.overrides[2]) != 1 {
		t.Fatal("同码应被覆盖")
	}
	if !strings.Contains(f.audits[1].ExtraInfo, "单独禁用 ⇒ 单独授权") || f.audits[1].Action != "单独授权按钮" {
		t.Fatalf("审计 = %+v", f.audits[1])
	}

	// 视图：来源与到期标记
	v, err := s.User(2)
	if err != nil {
		t.Fatal(err)
	}
	var item *EffectiveItem
	for i := range v.Items {
		if v.Items[i].Code == "export.download" {
			item = &v.Items[i]
		}
	}
	if item == nil || !item.Allowed || item.Source != SrcAllow || len(v.Overrides) != 1 || v.Overrides[0].Label != "下载导出文件" {
		t.Fatalf("视图 = %+v %+v", item, v.Overrides)
	}

	// 撤销
	wantErr(t, s.RemoveOverrides(admin, 2, []string{"export.download"}, ""), 400, "原因")
	wantErr(t, s.RemoveOverrides(admin, 2, []string{"qpriv.manage"}, "r"), 409, "没有单独")
	if err := s.RemoveOverrides(admin, 2, []string{"export.download"}, "到期清理"); err != nil {
		t.Fatal(err)
	}
	if len(f.overrides[2]) != 0 || f.audits[2].Action != "撤销按钮单独授权/禁用" {
		t.Fatalf("撤销失败: %+v", f.audits)
	}
	if !ok(r.HasButton(2, "export.download")) { // 回到权限组授予
		t.Fatal("撤销后应回到权限组授予")
	}
}

func TestUserViewSuperAndExpired(t *testing.T) {
	f := baseFake()
	s, _ := newSvc(f)
	past := fixedNow().Add(-time.Hour)
	f.overrides[2] = []Override{{Code: "perf.create", Effect: EffectAllow, ExpireAt: &past, Reason: "旧的"}}
	v, err := s.User(2)
	if err != nil || len(v.Overrides) != 1 || !v.Overrides[0].Expired {
		t.Fatalf("到期标记: %+v %v", v, err)
	}
	for _, it := range v.Items {
		if it.Code == "perf.create" && it.Allowed {
			t.Fatal("到期的授权不应生效")
		}
	}
	sv, _ := s.User(1)
	for _, it := range sv.Items {
		if !it.Allowed || it.Source != SrcSuper {
			t.Fatalf("超级用户应全部放行: %+v", it)
		}
	}
	if _, err := s.User(99); err == nil {
		t.Fatal("不存在的用户应报错")
	}
}

func TestRegistryView(t *testing.T) {
	f := baseFake()
	s, _ := newSvc(f)
	v, err := s.Registry()
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Modules) == 0 || len(v.Presets["DBA"]) == 0 || v.Total == 0 || v.Setting.Mode != ModeCompat || !v.TablesReady {
		t.Fatalf("%+v", v)
	}
	if ok2, _ := s.IsAdmin(1); !ok2 {
		t.Fatal("root 是超级用户")
	}
	if ok2, _ := s.IsAdmin(2); ok2 {
		t.Fatal("alice 不是")
	}
}

// 没有任何按钮的权限组：Codes 必须是空切片（JSON 里是 [] 而不是 null，前端按数组遍历）。
func TestGroupEmptyCodesIsNotNil(t *testing.T) {
	s, _ := newSvc(baseFake())
	gv, err := s.Group(11)
	if err != nil || gv.Codes == nil || len(gv.Codes) != 0 {
		t.Fatalf("%#v %v", gv, err)
	}
}

// 设置：只能改“无权限按钮的呈现方式”（迁移状态 mode 由 dbt-admin perm migrate 写入，接口不接受）；写审计、使缓存失效；无变化是空操作。
func TestSetSettingDenyDisplay(t *testing.T) {
	f := baseFake()
	s, r := newSvc(f)
	set, err := s.SetSetting(admin, SettingReq{DenyDisplay: DenyDisable, Reason: "给研发看提示"})
	if err != nil || set.DenyDisplay != DenyDisable || f.setting.DenyDisplay != DenyDisable || f.setting.Mode != ModeCompat {
		t.Fatalf("%+v %v %+v", set, err, f.setting)
	}
	if a := f.audits[len(f.audits)-1]; a.Action != "修改按钮权限设置" || !strings.Contains(a.ExtraInfo, "hide ⇒ disable") || !strings.Contains(a.ExtraInfo, "原因：给研发看提示") {
		t.Fatalf("审计 = %+v", a)
	}
	if got := ok(r.Setting()); got.DenyDisplay != DenyDisable {
		t.Fatalf("缓存应已失效: %+v", got)
	}
	_, err = s.SetSetting(admin, SettingReq{DenyDisplay: "x"})
	wantErr(t, err, 400, "hide 或 disable")
	n := len(f.audits)
	if _, err := s.SetSetting(admin, SettingReq{DenyDisplay: DenyDisable}); err != nil || len(f.audits) != n {
		t.Fatal("无变化应是空操作")
	}
	if _, err := s.SetSetting(admin, SettingReq{}); err != nil || len(f.audits) != n {
		t.Fatal("空请求应是空操作")
	}
}
