// @author: kevinkelly
// @license: Apache Licence
// @file: migrate_test.go
// @time: 2026/10/28

package btnperm

import (
	"reflect"
	"strings"
	"testing"
)

// 生产库 2026-10-23 的权限组与原权限码快照（各组成员数：Default 1、RD 14、DBA 6、TL 2、DE 1），用来核对迁移前后每个人能用的按钮完全一致。
var prodGroups = []struct {
	id    uint
	name  string
	codes string
}{
	{1, "Default", "add_perfissue,menu_document,menu_instance,menu_mssqlinspection,menu_partman_list,menu_perf,menu_query,menu_queryapplylist,menu_slowquery,menu_sqlbatchworkflow,menu_sqlexportbatchworkflow,menu_sqlexportworkflow,menu_sqlquery,menu_sqlworkflow,offline_download,query_download,sqlbatch_submit,sqlexportbatch_submit,sqlexport_submit,view_perfissue"},
	{2, "RD", "archive_apply,change_perfissue,menu_archive,menu_dashboard,menu_document,menu_instance,menu_mssqlinspection,menu_partman_list,menu_perf,menu_query,menu_queryapplylist,menu_slowquery,menu_sqladvisor,menu_sqlanalyze,menu_sqlbatchworkflow,menu_sqlcheck,menu_sqlexportbatchworkflow,menu_sqlexportworkflow,menu_sqloptimize,menu_sqlquery,menu_sqlworkflow,menu_tools,offline_download,optimize_soar,optimize_sqladvisor,query_applypriv,query_download,query_resource_group_instance,query_submit,sqlbatch_submit,sqlexportbatch_submit,sqlexport_submit,sql_analyze,sql_submit,view_perfissue"},
	{3, "DBA", "archive_apply,archive_mgt,archive_review,instance_account_manage,menu_archive,menu_dashboard,menu_database,menu_dbdiagnostic,menu_document,menu_instance,menu_instance_account,menu_instance_list,menu_my2sql,menu_openapi,menu_param,menu_query,menu_queryapplylist,menu_schemasync,menu_slowquery,menu_sqladvisor,menu_sqlanalyze,menu_sqlbatchworkflow,menu_sqlcheck,menu_sqlexportbatchworkflow,menu_sqlexportworkflow,menu_sqloptimize,menu_sqlquery,menu_sqlworkflow,menu_system,menu_tools,optimize_soar,optimize_sqladvisor,optimize_sqltuning,param_edit,param_view,process_kill,process_view,query_all_instances,query_applypriv,query_mgtpriv,query_resource_group_instance,query_review,query_submit,sqlbatch_submit,sqlexportbatch_submit,sql_analyze,sql_execute,sql_execute_for_resource_group,sql_review,sql_submit,tablespace_view,trxandlocks_view,trx_view"},
	{4, "TL", "archive_apply,archive_review,change_perfissue,menu_archive,menu_dashboard,menu_document,menu_instance,menu_mssqlinspection,menu_partman_list,menu_query,menu_queryapplylist,menu_slowquery,menu_sqladvisor,menu_sqlanalyze,menu_sqlbatchworkflow,menu_sqlcheck,menu_sqlexportbatchworkflow,menu_sqlexportworkflow,menu_sqloptimize,menu_sqlquery,menu_sqlworkflow,menu_tools,optimize_soar,optimize_sqladvisor,query_applypriv,query_review,query_submit,sqlbatch_submit,sqlexportbatch_submit,sql_analyze,sql_review,sql_submit,view_perfissue"},
	{5, "DE", "menu_dashboard,menu_document,menu_partman_list,menu_query,menu_queryapplylist,menu_sqlcheck,menu_sqlexportworkflow,menu_sqlquery,menu_sqlworkflow,query_applypriv,query_submit,sql_review,sql_submit"},
}

// prodFake 搭出生产快照：每组一个用户（uid = 组号 + 10）、没有分组的用户 30、ives（单独授予 query_all_instances，不在任何组）= 31、
// 一个超级用户 1、一个已停用的用户 40（在 RD 组）。Default 组已有 query.run / query.objinfo 两条按钮授权。
func prodFake() *fakeRepo {
	f := newFake()
	f.addUser(1, "root", true, true)
	for _, g := range prodGroups {
		f.addUser(g.id+10, strings.ToLower(g.name), true, false)
		f.addGroup(g.id, g.name, g.id+10)
		for _, c := range strings.Split(g.codes, ",") {
			f.groupDj[g.id][c] = true
		}
	}
	f.grant(1, "query.run", "query.objinfo")
	f.addUser(30, "nogroup", true, false)
	f.addUser(31, "ives", true, false)
	f.giveDjango(31, "query_all_instances")
	f.addUser(40, "gone", false, false)
	f.members[40] = []uint{2}
	return f
}

func allowedOf(t *testing.T, r *Resolver, uid uint) map[string]bool {
	t.Helper()
	m, err := r.AllowedButtons(uid)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// compatAllowed 是早期“兼容”模式的判定规则，运行时早已不用，只在这里留作迁移的对照基准（“迁移前每个人能用什么”）：
// 用户停用 / 单用户禁用 / 锁定按钮 → 无；单独授权、权限组授予、基础按钮 → 有；否则按钮的 Legacy 里有任何一个原权限码被用户持有（直接 + 权限组）→ 有。
func compatAllowed(f *fakeRepo, uid uint) map[string]bool {
	p, _ := f.Profile(uid)
	held := map[string]bool{}
	for c := range f.django[uid] {
		held[c] = true
	}
	for _, g := range f.members[uid] {
		for c := range f.groupDj[g] {
			held[c] = true
		}
	}
	now := fixedNow()
	out := map[string]bool{}
	for _, c := range Codes() {
		b, _ := Lookup(c)
		if EvalButton(b, p, now).Allowed {
			out[c] = true
			continue
		}
		if !p.Active || b.Locked || p.denied(c, now) {
			continue
		}
		for _, l := range b.Legacy {
			if held[l] {
				out[c] = true
				break
			}
		}
	}
	return out
}

// 核心：迁移前（按早期兼容规则）与迁移后（只认按钮）每个用户能用的按钮完全一致。
func TestMigrateKeepsEveryonesButtons(t *testing.T) {
	f := prodFake()
	r := newRes(f)
	uids := []uint{1, 11, 12, 13, 14, 15, 30, 31, 40}
	before := map[uint]map[string]bool{}
	for _, uid := range uids {
		before[uid] = compatAllowed(f, uid)
	}
	if len(before[13]) < 60 {
		t.Fatalf("DBA 迁移前能用的按钮应有几十个，实际 %d", len(before[13]))
	}

	plan, err := PlanMigrate(f)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != ModeCompat || !plan.SetStrict || plan.Empty() {
		t.Fatalf("现在是兼容模式，应有要写的内容: %+v", plan)
	}
	res, err := ApplyMigrate(f, plan, "tester", fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Strict || res.GroupAdds == 0 || res.UserAdds != 1 {
		t.Fatalf("结果不对: %+v", res)
	}
	r.InvalidateAll()
	if set, _ := r.Setting(); set.Mode != ModeStrict {
		t.Fatalf("迁移后应是严格模式: %+v", set)
	}
	for _, uid := range uids {
		after := allowedOf(t, r, uid)
		if !reflect.DeepEqual(before[uid], after) {
			var lost, gained []string
			for c := range before[uid] {
				if !after[c] {
					lost = append(lost, c)
				}
			}
			for c := range after {
				if !before[uid][c] {
					gained = append(gained, c)
				}
			}
			t.Errorf("用户 %d（%s）迁移前后按钮不一致：少了 %v，多了 %v", uid, f.users[uid].Username, lost, gained)
		}
	}
}

// 展开规则：用按钮的 Legacy（NoFeed 的按钮也算）、丢弃没有按钮的原权限码、基础按钮不写库、已有授权不动。
func TestMigratePlanDetails(t *testing.T) {
	f := prodFake()
	plan, err := PlanMigrate(f)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]MigrateGroupPlan{}
	for _, g := range plan.Groups {
		by[g.Group.Name] = g
	}
	has := func(list []string, c string) bool {
		for _, x := range list {
			if x == c {
				return true
			}
		}
		return false
	}
	dba := by["DBA"]
	for _, c := range []string{"sqlworkflow.alter_window", "sqlworkflow.schedule", "sqlworkflow.osc", "sqlworkflow.cancel_run", "database.edit", "binlog.parse", "query.admin_run"} {
		if !has(dba.Add, c) {
			t.Errorf("DBA 应该拿到 %s（兼容模式下由原权限码放行，含 NoFeed 的按钮）", c)
		}
	}
	for _, c := range BaselineButtons() {
		for _, g := range plan.Groups {
			if has(g.Add, c) {
				t.Errorf("基础按钮 %s 不写库（%s）", c, g.Group.Name)
			}
		}
	}
	if has(dba.Add, "inspect.view") || has(dba.Add, "perf.view") || has(dba.Add, "export.submit") {
		t.Error("DBA 组本来没有巡检 / 性能工单 / 提交导出的原权限码，迁移不应凭空多给")
	}
	de := by["DE"]
	if !reflect.DeepEqual(de.Unmapped, []string{"menu_dashboard", "menu_document", "menu_sqlcheck", "menu_sqlquery"}) {
		t.Errorf("DE 丢弃的原权限码 = %v", de.Unmapped)
	}
	if !has(by["TL"].Unmapped, "archive_review") {
		t.Errorf("archive_review 没有对应按钮（归档审批不需要审批码），迁移时应丢弃: %+v", by["TL"])
	}
	// Default 已有的两条按钮不动，也不重复写
	if def := by["Default"]; has(def.Add, "query.run") || has(def.Add, "query.objinfo") {
		t.Errorf("Default 已有的按钮不应再出现在新增里: %v", def.Add)
	}
	// ives 单独授予的 query_all_instances → 单用户允许 query.admin_run
	var ives MigrateUserPlan
	for _, u := range plan.Users {
		if u.User.Username == "ives" {
			ives = u
		}
	}
	if !reflect.DeepEqual(ives.Add, []string{"query.admin_run"}) {
		t.Errorf("ives 应得到单用户允许 query.admin_run: %+v", ives)
	}
}

// 重复执行：第二次没有任何新增，也不再切模式；回滚语句只包含这次新增的行。
func TestMigrateIsIdempotentAndRollbackIsExact(t *testing.T) {
	f := prodFake()
	plan, _ := PlanMigrate(f)
	res, err := ApplyMigrate(f, plan, "tester", fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(res.Rollback, "\n")
	for _, want := range []string{"DELETE FROM dbt_group_perm WHERE group_id = 3 AND perm_code IN (", "'binlog.parse'", "DELETE FROM dbt_user_perm WHERE user_id = 31 AND effect = 'allow' AND created_by = 'tester' AND perm_code IN ('query.admin_run');", "DELETE FROM dbt_perm_setting WHERE skey = 'mode';"} {
		if !strings.Contains(joined, want) {
			t.Errorf("回滚语句里缺少 %q:\n%s", want, joined)
		}
	}
	for _, line := range res.Rollback {
		if strings.HasPrefix(line, "DELETE FROM dbt_group_perm WHERE group_id = 1 ") && (strings.Contains(line, "'query.run'") || strings.Contains(line, "'query.objinfo'")) {
			t.Errorf("Default 组本来就有 query.run / query.objinfo，回滚语句不能删它们: %s", line)
		}
	}
	if len(f.audits) != 1 || f.audits[0].Action != "迁移原权限到按钮权限（全部）" {
		t.Errorf("应写一条审计: %+v", f.audits)
	}
	plan2, _ := PlanMigrate(f)
	if !plan2.Empty() || plan2.SetStrict || plan2.Mode != ModeStrict {
		t.Fatalf("第二次应没有任何要写的: %+v", plan2)
	}
	res2, err := ApplyMigrate(f, plan2, "tester", fixedNow())
	if err != nil || res2.GroupAdds != 0 || res2.UserAdds != 0 || res2.Strict || len(res2.Rollback) != 0 {
		t.Fatalf("第二次执行不应有任何写入: %+v %v", res2, err)
	}
}

// 已有的单用户覆盖（包括“禁止”）不被覆盖；用户单独授予的码已被所在权限组覆盖时不重复写。
func TestMigrateRespectsExistingOverridesAndGroupCoverage(t *testing.T) {
	f := newFake()
	f.addUser(1, "a", true, false)
	f.addUser(2, "b", true, false)
	f.addGroup(10, "G", 2)
	f.groupDj[10]["query_all_instances"] = true
	f.giveDjango(1, "query_all_instances", "sql_review")
	f.giveDjango(2, "query_all_instances")
	f.overrides[1] = []Override{{Code: "query.admin_run", Effect: EffectDeny, Reason: "测试"}}
	r := newRes(f)
	b1, b2 := compatAllowed(f, 1), compatAllowed(f, 2)

	plan, err := PlanMigrate(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range plan.Users {
		switch u.User.ID {
		case 1:
			if !reflect.DeepEqual(u.Existing, []string{"query.admin_run"}) || contains(u.Add, "query.admin_run") {
				t.Errorf("a 已有对 query.admin_run 的禁止，不能被覆盖: %+v", u)
			}
		case 2:
			if len(u.Add) != 0 || !reflect.DeepEqual(u.Covered, []string{"query.admin_run"}) {
				t.Errorf("b 所在 G 组迁移后已有 query.admin_run，不重复写: %+v", u)
			}
		}
	}
	if _, err := ApplyMigrate(f, plan, "t", fixedNow()); err != nil {
		t.Fatal(err)
	}
	if f.overrides[1][0].Effect != EffectDeny {
		t.Errorf("禁止被改了: %+v", f.overrides[1])
	}
	r.InvalidateAll()
	if !reflect.DeepEqual(b1, allowedOf(t, r, 1)) || !reflect.DeepEqual(b2, allowedOf(t, r, 2)) {
		t.Error("迁移前后按钮不一致")
	}
}

// 写入失败整体回滚：权限组按钮、单用户授权、迁移状态都保持原样。
func TestMigrateIsAtomic(t *testing.T) {
	f := prodFake()
	plan, _ := PlanMigrate(f)
	f.failTx = errBoom
	if _, err := ApplyMigrate(f, plan, "t", fixedNow()); err == nil {
		t.Fatal("应当失败")
	}
	if f.setting.Mode != ModeCompat || len(f.groupBtns[3]) != 0 || len(f.overrides[31]) != 0 || len(f.audits) != 0 {
		t.Errorf("失败后应保持原样: mode=%s dba=%d ives=%d audits=%d", f.setting.Mode, len(f.groupBtns[3]), len(f.overrides[31]), len(f.audits))
	}
}
