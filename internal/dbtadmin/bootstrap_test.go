// @author: kevinkelly
// @license: Apache Licence
// @file: bootstrap_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sample 是一份小样本脚本：模拟 deploy/bootstrap/（基础表 + 基础数据，表名带 dbt_ 前缀）与 deploy/sql（扩展表脚本）。
func sample(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"bootstrap/002_base_data.sql": "INSERT INTO dbt_auth_permission (name) VALUES ('Can add user');\nINSERT INTO dbt_auth_permission (name) VALUES ('Can view; user');\n",
		"bootstrap/001_base_schema.sql": "-- 基础表\nCREATE TABLE IF NOT EXISTS dbt_sql_users (id INT PRIMARY KEY);\n" +
			"CREATE TABLE IF NOT EXISTS dbt_auth_permission (id INT PRIMARY KEY, name VARCHAR(50));\n" +
			"CREATE TABLE IF NOT EXISTS dbt_audit_log (id INT PRIMARY KEY);\n",
		"sql/001_notify_channel.sql": "CREATE TABLE IF NOT EXISTS dbt_notify_channel (id INT PRIMARY KEY);\n",
		"sql/002_sql_schedule.sql":   "CREATE TABLE IF NOT EXISTS dbt_sql_schedule (workflow_id INT PRIMARY KEY);\nCREATE TABLE IF NOT EXISTS dbt_x (id INT);\n",
		"sql/README.txt":             "不是 sql 文件",
	})
	return root
}

func TestBootstrapDryRunWritesNothing(t *testing.T) {
	st := newMemStore()
	te := newTestEnv(t, st)
	root := sample(t)
	code := te.run("bootstrap", "--sql-root", root)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	if len(st.execs) != 0 || len(st.tables) != 0 || len(st.applied) != 0 {
		t.Errorf("演练不应写任何东西: execs=%d tables=%v applied=%v", len(st.execs), st.tables, st.applied)
	}
	out := te.out.String()
	mustContain(t, "stdout", out, "库 db_tool @ 10.9.8.7:3306", "演练", "计划执行 4 个文件、", "bootstrap/001_base_schema.sql", "bootstrap/002_base_data.sql",
		"sql/001_notify_channel.sql", "sql/002_sql_schedule.sql", "加 --apply")
	// 顺序：bootstrap 在前，且文件名升序
	i1, i2, i3 := strings.Index(out, "bootstrap/001"), strings.Index(out, "bootstrap/002"), strings.Index(out, "sql/001")
	if !(i1 < i2 && i2 < i3) {
		t.Errorf("文件顺序不对:\n%s", out)
	}
	mustNotContain(t, "输出", te.all(), "S3cretDb", "Pw%40", "README.txt")
}

func TestBootstrapApplyNeedsYesWhenNotTTY(t *testing.T) {
	st := newMemStore()
	te := newTestEnv(t, st)
	code := te.run("bootstrap", "--sql-root", sample(t), "--apply")
	if code != ExitUsage {
		t.Fatalf("非终端且没有 --yes 应返回 2，得到 %d\n%s", code, te.all())
	}
	if len(st.execs) != 0 {
		t.Error("不应执行任何语句")
	}
	mustContain(t, "stderr", te.errb.String(), "--yes")
}

func TestBootstrapApplyFresh(t *testing.T) {
	st := newMemStore()
	te := newTestEnv(t, st)
	root := sample(t)
	code := te.run("bootstrap", "--sql-root", root, "--apply", "--yes")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	// 语句数：3 + 2 + 1 + 2 = 8，按文件名顺序
	if len(st.execs) != 8 {
		t.Fatalf("执行了 %d 条语句: %q", len(st.execs), st.execs)
	}
	if !strings.Contains(st.execs[0], "dbt_sql_users") || !strings.Contains(st.execs[3], "Can add user") || !strings.Contains(st.execs[5], "dbt_notify_channel") {
		t.Errorf("执行顺序不对: %q", st.execs)
	}
	for _, n := range []string{"dbt_sql_users", "dbt_auth_permission", "dbt_audit_log", "dbt_notify_channel", "dbt_sql_schedule", TblApplied} {
		if !st.tables[n] {
			t.Errorf("缺表 %s", n)
		}
	}
	// 记录
	if len(st.applied) != 4 {
		t.Errorf("应记录 4 个文件: %v", st.applied)
	}
	if h := st.applied["bootstrap/001_base_schema.sql"]; len(h) != 64 {
		t.Errorf("sha256 记录有误: %q", h)
	}
	if st.appliedAt["sql/002_sql_schedule.sql"].IsZero() {
		t.Error("应记录 applied_at")
	}
	// 摘要
	out := te.out.String()
	mustContain(t, "stdout", out, "执行 4 个文件、8 条语句", "dbt_auth_permission 2 行", "本次新建 7 张表", "创建管理员", "dbt-admin admin create", "gen-secrets")
	// 审计（dbt_audit_log 被脚本创建）
	if len(st.audits) != 1 {
		t.Fatalf("审计 %d 条", len(st.audits))
	}
	a := st.audits[0]
	if a.UserName != "dbt-admin(alice)" || a.Action != "CLI-初始化基础表与数据" {
		t.Errorf("审计内容不对: %+v", a)
	}
	mustNotContain(t, "审计", a.ExtraInfo, "S3cretDb", "Can add user")
	mustNotContain(t, "输出", te.all(), "S3cretDb", "Pw%40")
}

func TestBootstrapRefusesWhenUsersExist(t *testing.T) {
	st := newMemStore()
	st.addUser("someone", false, true)
	te := newTestEnv(t, st)
	root := sample(t)
	code := te.run("bootstrap", "--sql-root", root, "--apply", "--yes")
	if code != ExitFailed {
		t.Fatalf("已有用户应拒绝，exit %d\n%s", code, te.all())
	}
	if len(st.execs) != 0 {
		t.Errorf("拒绝时不得执行任何语句: %q", st.execs)
	}
	mustContain(t, "stderr", te.errb.String(), "--only-sql", "dbt_sql_users", "扩展表脚本")

	// --only-sql：只执行 sql/*.sql，bootstrap/ 被忽略
	te.reset()
	code = te.run("bootstrap", "--sql-root", root, "--only-sql", "--apply", "--yes")
	if code != ExitOK {
		t.Fatalf("--only-sql exit %d\n%s", code, te.all())
	}
	if len(st.execs) != 3 {
		t.Errorf("--only-sql 应只执行 3 条语句: %q", st.execs)
	}
	for _, s := range st.execs {
		if strings.Contains(s, "dbt_auth_permission") {
			t.Errorf("--only-sql 不应执行 bootstrap 脚本: %s", s)
		}
	}
	for f := range st.applied {
		if strings.HasPrefix(f, "bootstrap/") {
			t.Errorf("不应记录 bootstrap 文件: %s", f)
		}
	}
	mustNotContain(t, "stdout", te.out.String(), "创建管理员") // 升级场景不提示创建管理员
}

func TestBootstrapEmptyUsersTableAllowed(t *testing.T) {
	st := newMemStore()
	st.tables[tblUsers] = true // 表存在但为空
	te := newTestEnv(t, st)
	if code := te.run("bootstrap", "--sql-root", sample(t), "--apply", "--yes"); code != ExitOK {
		t.Fatalf("空表应允许，exit %d\n%s", code, te.all())
	}
	// 已有表时提示：基础数据只应写进准备初始化的新库
	mustContain(t, "stdout", te.out.String(), "确认这个库确实是准备初始化的新库")
}

func TestBootstrapIdempotentRerunAndChangedSHA(t *testing.T) {
	st := newMemStore()
	te := newTestEnv(t, st)
	root := sample(t)
	if code := te.run("bootstrap", "--sql-root", root, "--apply", "--yes"); code != ExitOK {
		t.Fatalf("第一次 exit %d\n%s", code, te.all())
	}
	first := st.applied["sql/001_notify_channel.sql"]
	nExec := len(st.execs)

	// 第二次：dbt_sql_users 仍为空（样本脚本不插用户）→ 允许；内容相同
	te.reset()
	if code := te.run("bootstrap", "--sql-root", root, "--apply", "--yes"); code != ExitOK {
		t.Fatalf("第二次 exit %d\n%s", code, te.all())
	}
	if len(st.execs) != nExec*2 {
		t.Errorf("重跑应再执行全部语句，执行了 %d 条（原 %d）", len(st.execs), nExec)
	}
	mustContain(t, "stdout", te.out.String(), "已应用过，内容相同")
	mustNotContain(t, "stdout", te.out.String(), "内容与上次记录不同")
	if st.applied["sql/001_notify_channel.sql"] != first {
		t.Error("内容没变，sha 记录不应变化")
	}

	// 修改文件内容：提示但允许重跑，并更新记录
	writeFiles(t, root, map[string]string{"sql/001_notify_channel.sql": "CREATE TABLE IF NOT EXISTS dbt_notify_channel (id INT PRIMARY KEY, x INT);\n"})
	te.reset()
	if code := te.run("bootstrap", "--sql-root", root, "--apply", "--yes"); code != ExitOK {
		t.Fatalf("第三次 exit %d\n%s", code, te.all())
	}
	mustContain(t, "stdout", te.out.String(), "内容与上次记录不同")
	if st.applied["sql/001_notify_channel.sql"] == first {
		t.Error("内容变了，sha 记录应更新")
	}
}

func TestBootstrapStopsOnFirstError(t *testing.T) {
	st := newMemStore()
	st.failOn = "dbt_x"
	st.failMsg = "Error 1050: Table 'dbt_x' already exists, duplicate entry 'TOPSECRET'"
	te := newTestEnv(t, st)
	root := sample(t)
	// 让出错语句里带一个“敏感值”
	writeFiles(t, root, map[string]string{
		"sql/002_sql_schedule.sql": "CREATE TABLE IF NOT EXISTS dbt_sql_schedule (workflow_id INT PRIMARY KEY);\nINSERT INTO dbt_x (secret) VALUES ('pbkdf2_sha256$1$salt$TOPSECRET');\nCREATE TABLE after_error (id INT);\n",
		"sql/003_after.sql":        "CREATE TABLE IF NOT EXISTS dbt_after (id INT);\n",
	})
	code := te.run("bootstrap", "--sql-root", root, "--apply", "--yes")
	if code != ExitFailed {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	errOut := te.errb.String()
	mustContain(t, "stderr", errOut, "sql/002_sql_schedule.sql", "第 2 条语句失败", "共 3 条", "第 2 行", "可直接重跑")
	mustNotContain(t, "输出", te.all(), "TOPSECRET", "pbkdf2_sha256")
	// 遇错即停：后面的语句与文件不执行；出错的文件不记录；之前的文件已记录
	for _, s := range st.execs {
		if strings.Contains(s, "after_error") || strings.Contains(s, "dbt_after") {
			t.Errorf("出错后不应继续执行: %s", s)
		}
	}
	if _, ok := st.applied["sql/002_sql_schedule.sql"]; ok {
		t.Error("出错的文件不应记录为已应用")
	}
	if _, ok := st.applied["sql/001_notify_channel.sql"]; !ok {
		t.Error("成功的文件应已记录")
	}
	if len(st.audits) != 0 {
		t.Error("失败时不写“成功”审计")
	}
}

func TestBootstrapValidatesAllFilesBeforeWriting(t *testing.T) {
	st := newMemStore()
	te := newTestEnv(t, st)
	root := sample(t)
	writeFiles(t, root, map[string]string{"sql/009_bad.sql": "DELIMITER $$\nCREATE PROCEDURE p() BEGIN SELECT 1; END$$\n"})
	code := te.run("bootstrap", "--sql-root", root, "--apply", "--yes")
	if code != ExitFailed {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	if len(st.execs) != 0 {
		t.Errorf("文件有问题时一条都不应执行: %q", st.execs)
	}
	mustContain(t, "stderr", te.errb.String(), "sql/009_bad.sql", "DELIMITER")
}

func TestBootstrapMissingDirs(t *testing.T) {
	st := newMemStore()
	te := newTestEnv(t, st)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"sql/001_a.sql": "SELECT 1;"})
	if code := te.run("bootstrap", "--sql-root", root); code != ExitFailed {
		t.Errorf("缺 bootstrap/ 应失败: %d", code)
	}
	mustContain(t, "stderr", te.errb.String(), "--only-sql")
	te.reset()
	if code := te.run("bootstrap", "--sql-root", root, "--only-sql"); code != ExitOK {
		t.Errorf("--only-sql 不需要 bootstrap/: %d\n%s", code, te.all())
	}
	te.reset()
	empty := t.TempDir()
	if code := te.run("bootstrap", "--sql-root", empty, "--only-sql"); code != ExitFailed {
		t.Errorf("缺 sql/ 应失败: %d", code)
	}
	te.reset()
	if code := te.run("bootstrap", "--sql-root", filepath.Join(root, "nope")); code != ExitUsage {
		t.Errorf("根目录不存在应是参数错误: %d", code)
	}
}

func TestBootstrapInteractiveConfirmShowsTarget(t *testing.T) {
	st := newMemStore()
	te := newTestEnv(t, st)
	te.tty = true
	te.stdin.WriteString("no\n")
	root := sample(t)
	code := te.run("bootstrap", "--sql-root", root, "--apply")
	if code != ExitFailed {
		t.Fatalf("回答 no 应取消，exit %d\n%s", code, te.all())
	}
	if len(st.execs) != 0 {
		t.Error("取消后不得写入")
	}
	mustContain(t, "stdout", te.out.String(), "db_tool", "10.9.8.7:3306", "输入 yes 继续")

	te.reset()
	te.stdin.WriteString("yes\n")
	if code := te.run("bootstrap", "--sql-root", root, "--apply"); code != ExitOK {
		t.Fatalf("回答 yes 应执行，exit %d\n%s", code, te.all())
	}
	if len(st.execs) == 0 {
		t.Error("确认后应写入")
	}
}

func TestBootstrapNoAuditTableSkipsAudit(t *testing.T) {
	st := newMemStore()
	te := newTestEnv(t, st)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"bootstrap/001.sql": "CREATE TABLE IF NOT EXISTS dbt_sql_users (id INT);", "sql/001.sql": "CREATE TABLE IF NOT EXISTS dbt_a (id INT);"})
	if code := te.run("bootstrap", "--sql-root", root, "--apply", "--yes"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	if len(st.audits) != 0 {
		t.Error("没有 dbt_audit_log 不应写审计")
	}
	mustContain(t, "stdout", te.out.String(), "dbt_audit_log 不存在，未写审计", "dbt_auth_permission 不存在")
}

func TestStatusPorcelain(t *testing.T) {
	st := newMemStore()
	te := newTestEnv(t, st)
	te.run("status", "--porcelain")
	mustContain(t, "stdout", te.out.String(), "db=db_tool\n", "host=10.9.8.7:3306\n", "users_table=absent\n", "superusers=0\n", "bootstrap_applied=0\n")
	// 键名与表名无关：旧键 sql_users= 已改为 users_table=
	mustNotContain(t, "stdout", te.out.String(), "sql_users=", "dbt_sql_users=", "legacy_tables")

	st.tables[tblUsers] = true
	te.reset()
	te.run("status", "--porcelain")
	mustContain(t, "stdout", te.out.String(), "users_table=empty\n")

	st.addUser("root", true, true)
	st.addUser("bob", false, true)
	st.addUser("off", true, false)
	te.reset()
	te.run("status", "--porcelain")
	mustContain(t, "stdout", te.out.String(), "users_table=nonempty\n", "users=3\n", "superusers=1\n")
	te.reset()
	if code := te.run("status"); code != ExitOK {
		t.Fatal(code)
	}
	mustContain(t, "stdout", te.out.String(), "启用的超级用户 1 个")
}

func TestDatabaseMissingHint(t *testing.T) {
	te := newTestEnv(t, nil)
	te.Env.Open = func(context.Context, ConnParams) (Store, error) { return nil, ErrDatabaseMissing }
	code := te.run("bootstrap", "--sql-root", sample(t), "--apply", "--yes")
	if code != ExitFailed {
		t.Fatalf("exit %d", code)
	}
	mustContain(t, "stderr", te.errb.String(), "CREATE DATABASE `db_tool`", "不会自动建库", "10.9.8.7:3306")
	mustNotContain(t, "stderr", te.errb.String(), "S3cretDb")
}

func TestOpenErrorIsRedacted(t *testing.T) {
	te := newTestEnv(t, nil)
	te.Env.Open = func(context.Context, ConnParams) (Store, error) {
		return nil, os.ErrPermission
	}
	te.run("status")
	mustNotContain(t, "stderr", te.errb.String(), "S3cretDb")
}
