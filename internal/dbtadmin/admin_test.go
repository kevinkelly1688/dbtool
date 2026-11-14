// @author: kevinkelly
// @license: Apache Licence
// @file: admin_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"strings"
	"testing"
	"time"

	"dbtool/internal/crypto"
	"dbtool/internal/settings"
	"dbtool/internal/sysadmin"
)

const goodPassword = "Tr0ub4dor&3-horse"

func adminEnv(t *testing.T) (*testEnv, *memStore) {
	t.Helper()
	st := newMemStore()
	st.tables[tblUsers] = true
	st.tables[tblAuditLog] = true
	st.tables[tblGroups] = true
	st.tables[tblUserProfile] = true
	st.groups["DBA"] = 1
	st.groups["QA"] = 2
	te := newTestEnv(t, st)
	te.env[PasswordEnv] = goodPassword
	return te, st
}

func TestIterationsMatchSysadmin(t *testing.T) {
	if got := sysadmin.NewService(nil).Iterations; got != DefaultIterations {
		t.Fatalf("dbt-admin 的迭代次数 %d 与 sysadmin.NewService 的 %d 不一致，新建的密码哈希参数会和页面里创建的用户不同", DefaultIterations, got)
	}
}

func TestAdminCreate(t *testing.T) {
	te, st := adminEnv(t)
	code := te.run("admin", "create", "--username", "ops.admin", "--email", "ops@example.com", "--display", "运维管理员", "--group", "DBA", "--group", "QA,DBA", "--yes")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	u := st.user("ops.admin")
	if u == nil {
		t.Fatal("用户没有创建")
	}
	if !u.IsSuperuser || !u.IsStaff || !u.IsActive {
		t.Errorf("应是启用的超级用户 + staff: %+v", u)
	}
	if u.Display != "运维管理员" || u.Email != "ops@example.com" {
		t.Errorf("资料不对: %+v", u)
	}
	// 哈希是 pbkdf2_sha256 格式，可被 crypto 校验函数验证；迭代次数与设置一致；盐为 22 位字母数字
	h := st.hashes[u.ID]
	parts := strings.Split(h, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" || parts[1] != "1000" || len(parts[2]) != 22 {
		t.Fatalf("哈希格式不对: %q", parts)
	}
	if !crypto.CheckDjangoPassword(goodPassword, h) {
		t.Error("哈希无法被 CheckDjangoPassword 验证")
	}
	if crypto.CheckDjangoPassword(goodPassword+"x", h) {
		t.Error("错误密码不应通过")
	}
	// 权限组去重
	if gs := st.userGroup[u.ID]; len(gs) != 2 {
		t.Errorf("权限组 %v", gs)
	}
	// 审计
	if len(st.audits) != 1 {
		t.Fatalf("审计 %d 条", len(st.audits))
	}
	a := st.audits[0]
	if a.Action != "CLI-创建超级用户" || a.UserName != "dbt-admin(alice)" {
		t.Errorf("审计: %+v", a)
	}
	mustContain(t, "审计", a.ExtraInfo, "ops.admin", "DBA")
	mustNotContain(t, "审计", a.ExtraInfo, goodPassword, "pbkdf2")
	// 输出不含密码 / 哈希 / 库密码；显示目标库名与主机
	mustNotContain(t, "输出", te.all(), goodPassword, "pbkdf2_sha256", h, "S3cretDb")
	mustContain(t, "stdout", te.out.String(), "db_tool", "10.9.8.7:3306", "已创建超级用户 ops.admin")
}

func TestAdminCreateDefaultsAndDisplayTruncation(t *testing.T) {
	te, st := adminEnv(t)
	if code := te.run("admin", "create", "--username", "averyveryverylongname", "--yes"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	if d := st.user("averyveryverylongname").Display; d != "averyveryv" {
		t.Errorf("缺省姓名应取用户名前 10 个字符: %q", d)
	}
}

func TestAdminCreateRejectsWeakPassword(t *testing.T) {
	for name, pw := range map[string]string{
		"太短":    "Ab1!xyz",
		"全数字":   "1234567890123",
		"常见弱口令": "password123",
		"含用户名":  "xxopsadminxx1",
		"空白":    "   ",
	} {
		t.Run(name, func(t *testing.T) {
			te, st := adminEnv(t)
			te.env[PasswordEnv] = pw
			if code := te.run("admin", "create", "--username", "opsadmin", "--yes"); code != ExitUsage {
				t.Errorf("弱口令应返回 2，得到 %d\n%s", code, te.all())
			}
			if st.user("opsadmin") != nil || len(st.audits) != 0 {
				t.Error("弱口令不应创建用户或写审计")
			}
			mustNotContain(t, "输出", te.all(), pw)
			mustContain(t, "stderr", te.errb.String(), "密码不符合策略")
		})
	}
}

func TestAdminCreatePasswordPolicyFromDB(t *testing.T) {
	te, st := adminEnv(t)
	// goodPassword 是 17 位：最小长度设为 20 时应被拒绝
	st.settings[settings.KeySecurityPasswordMinLength] = "20"
	if code := te.run("admin", "create", "--username", "opsadmin", "--yes"); code != ExitUsage {
		t.Errorf("低于库里配置的最小长度应拒绝: %d\n%s", code, te.all())
	}
	mustContain(t, "stderr", te.errb.String(), "至少需要 20")
	// 最小长度放宽到 12 后通过
	st.settings[settings.KeySecurityPasswordMinLength] = "12"
	te.reset()
	if code := te.run("admin", "create", "--username", "opsadmin", "--yes"); code != ExitOK {
		t.Errorf("满足配置的最小长度应通过: %d\n%s", code, te.all())
	}
	// 复杂度要求：缺特殊字符的密码被拒
	st.settings[settings.KeySecurityPasswordComplexity] = "true"
	te.env[PasswordEnv] = "Abcdefghij1234"
	te.reset()
	if code := te.run("admin", "create", "--username", "second", "--yes"); code != ExitUsage {
		t.Errorf("开启复杂度后缺特殊字符应拒绝: %d", code)
	}
}

func TestAdminCreateDuplicateUser(t *testing.T) {
	te, st := adminEnv(t)
	st.addUser("root", true, true)
	before := st.hashes[1]
	code := te.run("admin", "create", "--username", "root", "--yes")
	if code != ExitFailed {
		t.Fatalf("重名应返回 1，得到 %d\n%s", code, te.all())
	}
	mustContain(t, "stderr", te.errb.String(), "已存在")
	if st.hashes[1] != before || len(st.audits) != 0 {
		t.Error("重名不应修改任何东西")
	}
	// 竞态：预检通过但插入时唯一键冲突
	te2, st2 := adminEnv(t)
	st2.dupOnIns = true
	if code := te2.run("admin", "create", "--username", "racer", "--yes"); code != ExitFailed {
		t.Errorf("唯一键冲突应返回 1: %d", code)
	}
	mustContain(t, "stderr", te2.errb.String(), "已存在")
}

func TestAdminCreateUsernameRules(t *testing.T) {
	te, st := adminEnv(t)
	for _, bad := range []string{"", "has space", "semi;colon", "a/b", strings.Repeat("x", 31), "引号'", "<script>"} {
		te.reset()
		args := []string{"admin", "create", "--yes"}
		if bad != "" {
			args = append(args, "--username", bad)
		}
		if code := te.run(args...); code != ExitUsage {
			t.Errorf("用户名 %q 应返回 2，得到 %d", bad, code)
		}
	}
	for _, good := range []string{"a", "ops.admin", "a@b.com", "x+y-z_1", "管理员01", strings.Repeat("x", 30)} {
		te.reset()
		if code := te.run("admin", "create", "--username", good, "--yes"); code != ExitOK {
			t.Errorf("用户名 %q 应通过，得到 %d\n%s", good, code, te.all())
		}
	}
	if len(st.users) != 6 {
		t.Errorf("用户数 %d", len(st.users))
	}
}

func TestAdminCreateEmailAndDisplayValidation(t *testing.T) {
	te, _ := adminEnv(t)
	if code := te.run("admin", "create", "--username", "u1", "--email", "not-an-email", "--yes"); code != ExitUsage {
		t.Errorf("坏邮箱: %d", code)
	}
	if code := te.run("admin", "create", "--username", "u1", "--display", "这个姓名超过十个字符了呀呀", "--yes"); code != ExitUsage {
		t.Errorf("姓名过长: %d", code)
	}
}

func TestAdminCreateGroupMissing(t *testing.T) {
	te, st := adminEnv(t)
	code := te.run("admin", "create", "--username", "u1", "--group", "NoSuchGroup", "--yes")
	if code != ExitFailed {
		t.Fatalf("exit %d", code)
	}
	mustContain(t, "stderr", te.errb.String(), "NoSuchGroup")
	if st.user("u1") != nil {
		t.Error("组不存在时不应创建用户")
	}
}

func TestAdminCreateEnvMissingNonTTY(t *testing.T) {
	te, st := adminEnv(t)
	delete(te.env, PasswordEnv)
	code := te.run("admin", "create", "--username", "u1", "--yes")
	if code != ExitUsage {
		t.Fatalf("非终端且没有 %s 应返回 2，得到 %d", PasswordEnv, code)
	}
	mustContain(t, "stderr", te.errb.String(), PasswordEnv)
	if st.user("u1") != nil {
		t.Error("不应创建用户")
	}
}

func TestAdminCreateInteractivePassword(t *testing.T) {
	te, st := adminEnv(t)
	delete(te.env, PasswordEnv)
	te.tty = true
	te.pwQueue = []string{"short", "short", goodPassword, goodPassword}
	// 第一次输入的弱口令被拒后重试
	code := te.run("admin", "create", "--username", "u1", "--yes")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	if len(te.pwPrompts) != 4 {
		t.Errorf("提示次数 %d", len(te.pwPrompts))
	}
	if !crypto.CheckDjangoPassword(goodPassword, st.hashes[st.user("u1").ID]) {
		t.Error("应使用第二次输入的合格密码")
	}
	mustNotContain(t, "输出", te.all(), goodPassword, "short")

	// 两次输入不一致
	te2, st2 := adminEnv(t)
	delete(te2.env, PasswordEnv)
	te2.tty = true
	te2.pwQueue = []string{goodPassword, goodPassword + "x"}
	if code := te2.run("admin", "create", "--username", "u2", "--yes"); code == ExitOK {
		t.Error("两次不一致不应成功")
	}
	if st2.user("u2") != nil {
		t.Error("不应创建用户")
	}
}

func TestAdminCreateDryRunAndNeedConfirm(t *testing.T) {
	te, st := adminEnv(t)
	if code := te.run("admin", "create", "--username", "u1", "--dry-run"); code != ExitOK {
		t.Fatalf("dry-run exit %d\n%s", code, te.all())
	}
	if st.user("u1") != nil || len(st.audits) != 0 {
		t.Error("dry-run 不应写入")
	}
	mustContain(t, "stdout", te.out.String(), "创建超级用户 u1", "db_tool")
	// 缺 --yes 且非终端
	te.reset()
	if code := te.run("admin", "create", "--username", "u1"); code != ExitUsage {
		t.Errorf("缺 --yes 应返回 2: %d", code)
	}
	if st.user("u1") != nil {
		t.Error("不应写入")
	}
	// dry-run 也会发现重名
	st.addUser("dup", true, true)
	te.reset()
	if code := te.run("admin", "create", "--username", "dup", "--dry-run"); code != ExitFailed {
		t.Errorf("dry-run 应发现重名: %d", code)
	}
}

func TestAdminCreateWithoutAuditTable(t *testing.T) {
	te, st := adminEnv(t)
	delete(st.tables, tblAuditLog)
	if code := te.run("admin", "create", "--username", "u1", "--yes"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	mustContain(t, "stdout", te.out.String(), "dbt_audit_log 不存在，未写审计")
	if st.user("u1") == nil {
		t.Error("没有 dbt_audit_log 也应创建用户")
	}
}

func TestAdminCreateNoUsersTable(t *testing.T) {
	te, st := adminEnv(t)
	delete(st.tables, tblUsers)
	if code := te.run("admin", "create", "--username", "u1", "--yes"); code != ExitFailed {
		t.Errorf("没有 dbt_sql_users 应失败: %d", code)
	}
	mustContain(t, "stderr", te.errb.String(), "bootstrap")
}

func TestAdminResetPassword(t *testing.T) {
	te, st := adminEnv(t)
	u := st.addUser("bob", false, true)
	u.FailedLoginCount = 7
	now := te.Now()
	u.LastFailedAt = &now
	old := st.hashes[u.ID]
	te.env[PasswordEnv] = "An0ther-Good-Pass!"
	code := te.run("admin", "reset-password", "--username", "bob", "--require-change", "--yes")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	if st.hashes[u.ID] == old || !crypto.CheckDjangoPassword("An0ther-Good-Pass!", st.hashes[u.ID]) {
		t.Error("密码应已重置且可验证")
	}
	if got := st.user("bob"); got.FailedLoginCount != 0 || got.LastFailedAt != nil {
		t.Errorf("失败计数应清零: %+v", got)
	}
	if !st.profile[u.ID].MustChange {
		t.Error("--require-change 应置位 must_change_password")
	}
	if len(st.audits) != 1 || st.audits[0].Action != "CLI-重置密码" {
		t.Fatalf("审计: %+v", st.audits)
	}
	mustNotContain(t, "输出与审计", te.all()+st.audits[0].ExtraInfo, "An0ther-Good-Pass!", "pbkdf2_sha256")

	// 不带 --require-change：清掉遗留标记
	te.reset()
	te.env[PasswordEnv] = "Yet-An0ther-Pass1"
	if code := te.run("admin", "reset-password", "--username", "bob", "--yes"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	if st.profile[u.ID].MustChange {
		t.Error("手输新密码且不要求改密时应清掉遗留的强制改密标记")
	}
}

func TestAdminResetPasswordErrors(t *testing.T) {
	te, st := adminEnv(t)
	if code := te.run("admin", "reset-password", "--username", "ghost", "--yes"); code != ExitFailed {
		t.Errorf("用户不存在: %d", code)
	}
	// LDAP 用户拒绝
	u := st.addUser("ldapuser", false, true)
	st.profile[u.ID] = AuthInfo{HasProfile: true, ProfileType: "ldap"}
	te.reset()
	if code := te.run("admin", "reset-password", "--username", "ldapuser", "--yes"); code != ExitFailed {
		t.Errorf("LDAP 用户应拒绝: %d", code)
	}
	mustContain(t, "stderr", te.errb.String(), "LDAP")
	// 弱口令
	st.addUser("bob", false, true)
	te.reset()
	te.env[PasswordEnv] = "12345678901"
	if code := te.run("admin", "reset-password", "--username", "bob", "--yes"); code != ExitUsage {
		t.Errorf("弱口令: %d", code)
	}
	// 要求改密但 dbt_user_profile 不存在：整体回滚，密码不变
	delete(st.tables, tblUserProfile)
	te.reset()
	te.env[PasswordEnv] = goodPassword
	bobID := st.user("bob").ID
	old := st.hashes[bobID]
	if code := te.run("admin", "reset-password", "--username", "bob", "--require-change", "--yes"); code != ExitFailed {
		t.Errorf("缺 dbt_user_profile: %d", code)
	}
	if st.hashes[bobID] != old {
		t.Error("失败时密码应回滚")
	}
	mustContain(t, "stderr", te.errb.String(), "dbt_user_profile")
}

func TestAdminUnlock(t *testing.T) {
	te, st := adminEnv(t)
	u := st.addUser("bob", false, true)
	u.FailedLoginCount = 5
	now := te.Now()
	u.LastFailedAt = &now
	if code := te.run("admin", "unlock", "--username", "bob", "--yes"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	if got := st.user("bob"); got.FailedLoginCount != 0 || got.LastFailedAt != nil {
		t.Errorf("应已解锁: %+v", got)
	}
	if len(st.audits) != 1 || st.audits[0].Action != "CLI-解锁账号" {
		t.Errorf("审计: %+v", st.audits)
	}
	// 无失败记录：无需解锁，不写审计
	te.reset()
	if code := te.run("admin", "unlock", "--username", "bob", "--yes"); code != ExitOK {
		t.Errorf("exit %d", code)
	}
	if len(st.audits) != 1 {
		t.Error("无变化不应再写审计")
	}
	mustContain(t, "stdout", te.out.String(), "无需解锁")
}

func TestAdminActivateDeactivate(t *testing.T) {
	te, st := adminEnv(t)
	st.addUser("root", true, true)
	st.addUser("bob", false, true)

	// 停用普通用户
	if code := te.run("admin", "deactivate", "--username", "bob", "--yes"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	if st.user("bob").IsActive {
		t.Error("bob 应已停用")
	}
	// 重复停用：无变化，成功退出
	te.reset()
	if code := te.run("admin", "deactivate", "--username", "bob", "--yes"); code != ExitOK {
		t.Errorf("重复停用: %d", code)
	}
	// 启用
	if code := te.run("admin", "activate", "--username", "bob", "--yes"); code != ExitOK {
		t.Fatal("activate")
	}
	if !st.user("bob").IsActive {
		t.Error("bob 应已启用")
	}
	if n := len(st.audits); n != 2 {
		t.Errorf("审计应有 2 条（停用、启用）: %d", n)
	}
	if st.audits[0].Action != "CLI-停用用户" || st.audits[1].Action != "CLI-启用用户" {
		t.Errorf("审计动作: %+v", st.audits)
	}
}

func TestAdminDeactivateLastSuperuserProtected(t *testing.T) {
	te, st := adminEnv(t)
	root := st.addUser("root", true, true)
	st.addUser("off", true, false) // 已停用的超级用户不算
	st.addUser("bob", false, true)
	te.reset()
	code := te.run("admin", "deactivate", "--username", "root", "--yes")
	if code != ExitFailed {
		t.Fatalf("不得停用最后一个启用的超级用户，exit %d\n%s", code, te.all())
	}
	if !root.IsActive || !st.user("root").IsActive {
		t.Error("root 必须仍是启用状态")
	}
	mustContain(t, "stderr", te.errb.String(), "最后一个")
	// 再启用一个超级用户后就可以停用
	if code := te.run("admin", "activate", "--username", "off", "--yes"); code != ExitOK {
		t.Fatal("activate off")
	}
	te.reset()
	if code := te.run("admin", "deactivate", "--username", "root", "--yes"); code != ExitOK {
		t.Errorf("有另一个启用的超级用户时应允许: %d\n%s", code, te.all())
	}
}

func TestAdminList(t *testing.T) {
	te, st := adminEnv(t)
	st.configs[settings.KeySecurityLoginLockAttempts] = "3"
	st.configs[settings.KeySecurityLoginLockSeconds] = "600"
	root := st.addUser("root", true, true)
	last := time.Date(2026, 10, 9, 8, 30, 0, 0, time.UTC)
	root.LastLogin = &last
	bob := st.addUser("bob", false, true)
	bob.FailedLoginCount = 3
	f := te.Now().Add(-100 * time.Second)
	bob.LastFailedAt = &f
	old := st.addUser("old", false, false)
	old.FailedLoginCount = 9
	of := te.Now().Add(-2 * time.Hour)
	old.LastFailedAt = &of

	if code := te.run("admin", "list"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	out := te.out.String()
	mustContain(t, "stdout", out, "用户名", "最近登录", "共 3 个用户", "2026-10-09 08:30:00", "从未登录", "锁定（剩余 501 秒）", "失败 9 次")
	// 输出里不能有哈希
	mustNotContain(t, "stdout", out, "pbkdf2")
	if len(st.audits) != 0 {
		t.Error("list 不写审计")
	}
}

func TestAdminCommandsNeverEchoSecrets(t *testing.T) {
	te, st := adminEnv(t)
	st.addUser("bob", false, true)
	te.env["SECRET_KEY"] = "SUPERSECRETKEYVALUE-SUPERSECRETKEYVALUE"
	cmds := [][]string{
		{"admin", "create", "--username", "n1", "--yes"},
		{"admin", "reset-password", "--username", "bob", "--yes"},
		{"admin", "unlock", "--username", "bob", "--yes"},
		{"admin", "deactivate", "--username", "bob", "--yes"},
		{"admin", "activate", "--username", "bob", "--yes"},
		{"admin", "list"},
		{"status"},
		{"admin", "create", "--username", "n2", "--dry-run"},
	}
	for _, c := range cmds {
		te.run(c...)
	}
	all := te.all()
	h := st.hashes[st.user("n1").ID]
	mustNotContain(t, "全部输出", all, goodPassword, "pbkdf2_sha256", h, "S3cretDb", "SUPERSECRETKEYVALUE")
	for _, a := range st.audits {
		mustNotContain(t, "审计", a.ExtraInfo+a.Action+a.UserName, goodPassword, "pbkdf2", "S3cretDb")
	}
}
