// @author: kevinkelly
// @license: Apache Licence
// @file: permcmd_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbtool/internal/btnperm"
)

func (m *memStore) PermMigrator(_ context.Context) (PermMigrator, error) {
	if m.permErr != nil {
		return nil, m.permErr
	}
	return m.perm, nil
}

// fakeMigrator 返回预设的计划，记录 Apply 的调用。
type fakeMigrator struct {
	plan     *btnperm.MigratePlan
	planErr  error
	applyErr error
	applied  int
	by       string
}

func (f *fakeMigrator) Plan() (*btnperm.MigratePlan, error) { return f.plan, f.planErr }
func (f *fakeMigrator) Apply(plan *btnperm.MigratePlan, by string, at time.Time) (*btnperm.MigrateResult, error) {
	f.applied++
	f.by = by
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	ga, ua := plan.Counts()
	return &btnperm.MigrateResult{GroupAdds: ga, UserAdds: ua, Strict: plan.SetStrict, Rollback: plan.RollbackSQL(by)}, nil
}

func samplePlan() *btnperm.MigratePlan {
	return &btnperm.MigratePlan{
		Mode: btnperm.ModeCompat, SetStrict: true, Baseline: []string{"qpriv.apply"},
		Groups: []btnperm.MigrateGroupPlan{
			{Group: btnperm.GroupInfo{ID: 3, Name: "DBA", Members: 6}, Legacy: []string{"sql_review", "menu_tools"}, Add: []string{"sqlworkflow.approve", "sqlworkflow.reject"}, Unmapped: []string{"menu_tools"}},
		},
		Users: []btnperm.MigrateUserPlan{
			{User: btnperm.UserInfo{ID: 31, Username: "ives"}, Legacy: []string{"query_all_instances"}, Add: []string{"query.admin_run"}},
		},
	}
}

func permEnv(t *testing.T, fm *fakeMigrator) *testEnv {
	t.Helper()
	st := newMemStore()
	st.perm = fm
	return newTestEnv(t, st)
}

func TestPermMigrateDryRun(t *testing.T) {
	fm := &fakeMigrator{plan: samplePlan()}
	te := permEnv(t, fm)
	if code := te.run("perm", "migrate"); code != ExitOK {
		t.Fatalf("退出码 %d：%s", code, te.all())
	}
	out := te.all()
	for _, want := range []string{"默认只演练", "DBA", "成员 6 人", "新增按钮 2 个", "丢弃：menu_tools", "ives", "query.admin_run", "合计：权限组新增按钮 2 条，单用户新增允许 1 条", "这是演练"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里缺少 %q:\n%s", want, out)
		}
	}
	if fm.applied != 0 {
		t.Error("演练不应写入")
	}
	if strings.Contains(out, "Pw@S3cret") || strings.Contains(out, "Pw%40S3cret") {
		t.Error("输出里不能出现密码")
	}
}

func TestPermMigrateApplyWritesRollbackFileFirst(t *testing.T) {
	fm := &fakeMigrator{plan: samplePlan()}
	te := permEnv(t, fm)
	path := filepath.Join(t.TempDir(), "sub", "rb.sql")
	if code := te.run("perm", "migrate", "--apply", "--yes", "--rollback-file", path); code != ExitOK {
		t.Fatalf("退出码 %d：%s", code, te.all())
	}
	if fm.applied != 1 || fm.by != "dbt-admin(alice)" {
		t.Fatalf("应执行一次，操作人 dbt-admin(alice): %+v", fm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"DELETE FROM dbt_group_perm WHERE group_id = 3 AND perm_code IN ('sqlworkflow.approve', 'sqlworkflow.reject');", "DELETE FROM dbt_user_perm WHERE user_id = 31 AND effect = 'allow' AND created_by = 'dbt-admin(alice)'", "DELETE FROM dbt_perm_setting WHERE skey = 'mode';"} {
		if !strings.Contains(text, want) {
			t.Errorf("撤销脚本里缺少 %q:\n%s", want, text)
		}
	}
	if !strings.Contains(te.all(), "已迁移：权限组新增按钮 2 条，单用户新增允许 1 条，标记为已迁移：true") {
		t.Errorf("结果提示不对:\n%s", te.all())
	}
}

func TestPermMigrateAlreadyDone(t *testing.T) {
	p := samplePlan()
	p.Mode, p.SetStrict = btnperm.ModeStrict, false
	p.Groups[0].Add, p.Users[0].Add = nil, nil
	fm := &fakeMigrator{plan: p}
	te := permEnv(t, fm)
	if code := te.run("perm", "migrate", "--apply", "--yes", "--rollback-file", filepath.Join(t.TempDir(), "rb.sql")); code != ExitOK {
		t.Fatalf("退出码 %d：%s", code, te.all())
	}
	if fm.applied != 0 || !strings.Contains(te.all(), "已经迁移过了") {
		t.Errorf("已迁移过：不写入、不存撤销脚本: applied=%d\n%s", fm.applied, te.all())
	}
}

func TestPermMigrateErrors(t *testing.T) {
	// 按钮权限表没创建
	te := permEnv(t, &fakeMigrator{planErr: ErrPermTables})
	if code := te.run("perm", "migrate"); code != ExitFailed || !strings.Contains(te.all(), "019_btnperm.sql") {
		t.Errorf("表没创建应失败并提示: %d %s", code, te.all())
	}
	// 撤销脚本存不下：不写库
	fm := &fakeMigrator{plan: samplePlan()}
	te = permEnv(t, fm)
	bad := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := te.run("perm", "migrate", "--apply", "--yes", "--rollback-file", filepath.Join(bad, "rb.sql")); code != ExitFailed || fm.applied != 0 {
		t.Errorf("存不下撤销脚本时不应写库: code=%d applied=%d\n%s", code, fm.applied, te.all())
	}
	// 写库失败
	fm = &fakeMigrator{plan: samplePlan(), applyErr: errors.New("boom")}
	te = permEnv(t, fm)
	if code := te.run("perm", "migrate", "--apply", "--yes", "--rollback-file", filepath.Join(t.TempDir(), "rb.sql")); code != ExitFailed || !strings.Contains(te.all(), "没有任何改动") {
		t.Errorf("写库失败应提示没有任何改动: %d %s", code, te.all())
	}
	// 未知子命令 / 缺子命令
	te = permEnv(t, &fakeMigrator{plan: samplePlan()})
	if code := te.run("perm", "bogus"); code != ExitUsage {
		t.Errorf("未知子命令: %d", code)
	}
	if code := te.run("perm"); code != ExitUsage {
		t.Errorf("缺子命令: %d", code)
	}
}
