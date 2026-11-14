// @author: kevinkelly
// @license: Apache Licence
// @file: accounts_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// accEnv：mysql 8.0 实例，账号 app_rw@10.20.%、root@%（高危）、mysql.sys@localhost（内置）、dba_ro@10.0.0.2（平台自己用的账号）。
func accEnv() *env {
	e := newEnv().withVersion("8.0.36")
	exist := map[string]bool{"app_rw@10.20.%": true, "root@%": true, "mysql.sys@localhost": true, "dba_ro@10.0.0.2": true, "report@10.20.%": true}
	e.target.onFunc("WHERE user = ? AND host = ?", func(_ string, args []any) (*Result, error) {
		if exist[args[0].(string)+"@"+args[1].(string)] {
			return rs([]string{"n"}, []any{int64(1)}), nil
		}
		return rs([]string{"n"}), nil
	})
	e.target.on("CURRENT_USER()", rs([]string{"cu"}, []any{"dba_ro@10.0.0.2"}))
	e.target.on("ORDER BY user, host", rs([]string{"u", "h", "locked"},
		[]any{"app_rw", "10.20.%", "N"}, []any{"dba_ro", "10.0.0.2", "N"}, []any{"mysql.sys", "localhost", "Y"}, []any{"root", "%", "N"}))
	e.target.onFunc("SHOW GRANTS FOR", func(_ string, args []any) (*Result, error) {
		switch args[0] {
		case "root":
			return rs([]string{"g"}, []any{"GRANT ALL PRIVILEGES ON *.* TO `root`@`%` WITH GRANT OPTION"}), nil
		case "app_rw":
			return rs([]string{"g"}, []any{"GRANT USAGE ON *.* TO `app_rw`@`10.20.%`"}, []any{"GRANT SELECT, INSERT ON `shop`.* TO `app_rw`@`10.20.%`"}), nil
		}
		return rs([]string{"g"}, []any{"GRANT USAGE ON *.* TO `x`@`y`"}), nil
	})
	return e
}

func TestListAccounts(t *testing.T) {
	e := accEnv()
	e.repo.accts[1] = map[string]AccountRecord{"app_rw@10.20.%": {User: "app_rw", Host: "10.20.%", Remark: "商城应用读写"}}
	res, err := e.svc.ListAccounts(context.Background(), root, 1, false, "")
	if err != nil || len(res.Items) != 4 || !res.SupportsLock || res.Current != "dba_ro@10.0.0.2" {
		t.Fatalf("%+v %v", res, err)
	}
	by := map[string]AccountItem{}
	for _, a := range res.Items {
		by[a.User] = a
	}
	if a := by["app_rw"]; !a.Saved || a.Remark != "商城应用读写" || a.Risky || a.Protected || len(a.Privileges) != 2 || a.UserHost != "'app_rw'@'10.20.%'" {
		t.Fatalf("app_rw: %+v", a)
	}
	if a := by["root"]; !a.Risky || a.Saved || !strings.Contains(a.RiskNote, "任意主机") {
		t.Fatalf("root 应标高危: %+v", a)
	}
	if a := by["mysql.sys"]; !a.Protected || a.Locked == nil || !*a.Locked {
		t.Fatalf("内置账号受保护且已锁定: %+v", a)
	}
	if a := by["dba_ro"]; !a.Protected || !strings.Contains(a.ProtectReason, "平台连接") {
		t.Fatalf("平台自己的账号受保护: %+v", a)
	}
	if l, _ := e.svc.ListAccounts(context.Background(), root, 1, true, ""); len(l.Items) != 1 {
		t.Fatalf("只看已录入: %+v", l)
	}
	if l, _ := e.svc.ListAccounts(context.Background(), root, 1, false, "10.20"); len(l.Items) != 1 {
		t.Fatalf("按主机搜索: %+v", l)
	}
}

func TestListAccountsLockFallbacks(t *testing.T) {
	// 5.6：没有锁定功能，SQL 里不带 account_locked
	e := accEnv()
	e.target.handlers = e.target.handlers[:0]
	e.target.on("SELECT VERSION()", rs([]string{"v"}, []any{"5.6.40-log"}))
	e.target.on("ORDER BY user, host", rs([]string{"u", "h"}, []any{"a", "%"}))
	res, err := e.svc.ListAccounts(context.Background(), root, 1, false, "")
	if err != nil || res.SupportsLock || res.Items[0].Locked != nil {
		t.Fatalf("%+v %v", res, err)
	}
	if q := e.target.queries("ORDER BY user, host")[0]; strings.Contains(q.sql, "account_locked") {
		t.Fatalf("5.6 不应查 account_locked: %s", q.sql)
	}
	// 8.0 但 mysql.user 没有该列：回退到不带锁定信息的查询
	e2 := accEnv()
	e2.target.handlers = append([]handler{{"account_locked", func(string, []any) (*Result, error) { return nil, errors.New("Unknown column 'account_locked'") }}}, e2.target.handlers...)
	res, err = e2.svc.ListAccounts(context.Background(), root, 1, false, "")
	if err != nil || res.SupportsLock || len(res.Items) != 4 || res.Items[0].Locked != nil {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestAccountsUnsupportedEngines(t *testing.T) {
	e := accEnv()
	for _, id := range []uint{3, 4} { // mssql、doris
		_, err := e.svc.ListAccounts(context.Background(), root, id, false, "")
		wantErr(t, err, 400, "暂不支持")
	}
	if _, err := e.svc.ListAccounts(context.Background(), root, 5, false, ""); err != nil { // polardb
		t.Fatal(err)
	}
	_, err := e.svc.ListAccounts(context.Background(), bob, 2, false, "")
	wantErr(t, err, 403, "未关联")
}

// TestAssessRisk 覆盖新的高危口径（对齐原型）。与旧口径（只看全局权限与 FILE）不同的用例：
//   - “% + 全局 SELECT, INSERT”旧口径不算高危，新口径算（任意主机 + 写权限）；
//   - “10.0.0.1 + 全局 SUPER, PROCESS”旧口径不算高危，新口径算（SUPER / PROCESS 本身是全局高危）。
func TestAssessRisk(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		grants []string
		risky  bool
		note   string // 原因里应包含的片段
	}{
		{"任意主机 ALL", "%", []string{"GRANT ALL PRIVILEGES ON *.* TO `r`@`%`"}, true, "任意主机"},
		{"指定主机 ALL", "10.0.0.1", []string{"GRANT ALL PRIVILEGES ON *.* TO `r`@`10.0.0.1`"}, true, "ALL PRIVILEGES"},
		{"库级 ALL 也算", "10.0.0.1", []string{"GRANT ALL PRIVILEGES ON `shop`.* TO `r`@`10.0.0.1`"}, true, "ALL PRIVILEGES"},
		{"任意主机只读", "%", []string{"GRANT SELECT ON `shop`.* TO `a`@`%`"}, false, ""},
		{"任意主机全局只读", "%", []string{"GRANT SELECT, SHOW VIEW ON *.* TO `a`@`%`"}, false, ""},
		{"任意主机全局写（旧口径不算）", "%", []string{"GRANT SELECT, INSERT ON *.* TO `a`@`%`"}, true, "写"},
		{"任意主机库级写", "%", []string{"GRANT SELECT, INSERT, UPDATE, DELETE ON `shop`.* TO `a`@`%`"}, true, "写"},
		{"任意主机表级 DROP", "%", []string{"GRANT DROP ON `shop`.`t` TO `a`@`%`"}, true, "写"},
		{"任意主机列级 UPDATE", "%", []string{"GRANT SELECT (`id`, `n`), UPDATE (`n`) ON `shop`.`t` TO `a`@`%`"}, true, "写"},
		{"指定主机库级写不算", "10.20.%", []string{"GRANT SELECT, INSERT, UPDATE, DELETE, ALTER ON `shop`.* TO `a`@`10.20.%`"}, false, ""},
		{"任意主机 SUPER", "%", []string{"GRANT SUPER ON *.* TO `a`@`%`"}, true, "SUPER"},
		{"指定主机 SUPER, PROCESS（旧口径不算）", "10.0.0.1", []string{"GRANT SUPER, PROCESS ON *.* TO `a`@`10.0.0.1`"}, true, "SUPER、PROCESS"},
		{"指定主机 PROCESS", "10.0.0.1", []string{"GRANT PROCESS ON *.* TO `a`@`10.0.0.1`"}, true, "PROCESS"},
		{"指定主机 FILE", "10.0.0.1", []string{"GRANT SELECT, FILE ON *.* TO `a`@`10.0.0.1`"}, true, "FILE"},
		{"WITH GRANT OPTION", "%", []string{"GRANT SELECT ON *.* TO `a`@`%` WITH GRANT OPTION"}, true, "GRANT OPTION"},
		{"库级 WITH GRANT OPTION", "10.0.0.1", []string{"GRANT SELECT ON `shop`.* TO `a`@`10.0.0.1` WITH GRANT OPTION"}, true, "GRANT OPTION"},
		{"空主机 REPLICATION SLAVE", "", []string{"GRANT REPLICATION SLAVE ON *.* TO `a`@``"}, true, "REPLICATION SLAVE"},
		{"REPLICATION CLIENT 不算", "10.0.0.1", []string{"GRANT REPLICATION CLIENT ON *.* TO `a`@`10.0.0.1`"}, false, ""},
		{"库名里带 file 不算 FILE", "10.0.0.1", []string{"GRANT SELECT ON `file_db`.* TO `a`@`h`"}, false, ""},
		{"库级 SUPER 样的库名不算全局", "10.0.0.1", []string{"GRANT SELECT ON `super`.* TO `a`@`h`"}, false, ""},
		{"USAGE", "%", []string{"GRANT USAGE ON *.* TO `a`@`%`"}, false, ""},
		{"读取失败的占位文字", "%", []string{"（读取权限失败：boom）"}, false, ""},
		{"多条原因", "%", []string{"GRANT ALL PRIVILEGES ON *.* TO `a`@`%` WITH GRANT OPTION"}, true, "；"},
	}
	for _, c := range cases {
		got, note := assessRisk(c.host, c.grants)
		if got != c.risky || (c.note != "" && !strings.Contains(note, c.note)) || (!c.risky && note != "") {
			t.Errorf("%s: 得到 %v %q，期望 %v 含 %q", c.name, got, note, c.risky, c.note)
		}
	}
}

// root 本机账号属于内置账号：受保护，不判定高危，且授权也不允许；其他主机的 root 不受影响。
func TestRootLocalAccountsProtected(t *testing.T) {
	for _, a := range [][2]string{{"root", "localhost"}, {"root", "127.0.0.1"}, {"root", "::1"}, {"ROOT", "LocalHost"}, {"mysql.sys", "localhost"}} {
		if p, why := protectOf(a[0], a[1], ""); !p || !strings.Contains(why, "内置") {
			t.Errorf("%v 应受保护: %v %q", a, p, why)
		}
	}
	for _, a := range [][2]string{{"root", "%"}, {"root", "10.0.0.5"}, {"rootx", "localhost"}, {"app", "localhost"}} {
		if p, _ := protectOf(a[0], a[1], ""); p {
			t.Errorf("%v 不应受保护", a)
		}
	}

	e := accEnv()
	e.target.handlers = nil
	e.target.on("SELECT VERSION()", rs([]string{"v"}, []any{"8.0.36"}))
	e.target.on("CURRENT_USER()", rs([]string{"cu"}, []any{"dba_ro@10.0.0.2"}))
	e.target.on("ORDER BY user, host", rs([]string{"u", "h", "locked"},
		[]any{"root", "localhost", "N"}, []any{"root", "%", "N"}, []any{"root", "::1", "N"}))
	e.target.onFunc("SHOW GRANTS FOR", func(string, []any) (*Result, error) {
		return rs([]string{"g"}, []any{"GRANT ALL PRIVILEGES ON *.* TO `root`@`x` WITH GRANT OPTION"}), nil
	})
	e.target.onFunc("WHERE user = ? AND host = ?", func(_ string, args []any) (*Result, error) {
		return rs([]string{"n"}, []any{int64(1)}), nil
	})
	res, err := e.svc.ListAccounts(context.Background(), root, 1, false, "")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]AccountItem{}
	for _, a := range res.Items {
		by[a.Host] = a
	}
	if a := by["localhost"]; !a.Protected || a.Risky {
		t.Fatalf("root@localhost 受保护且不判高危: %+v", a)
	}
	if a := by["::1"]; !a.Protected || a.Risky {
		t.Fatalf("root@::1 受保护且不判高危: %+v", a)
	}
	if a := by["%"]; a.Protected || !a.Risky {
		t.Fatalf("root@%% 不受保护且高危: %+v", a)
	}
	if res.Risky != 1 {
		t.Fatalf("高危计数只含 root@%%: %+v", res)
	}
	if l, _ := e.svc.ListAccountsFiltered(context.Background(), root, 1, AccountFilter{Risk: true}); len(l.Items) != 1 || l.Items[0].Host != "%" {
		t.Fatalf("只看高危: %+v", l)
	}

	// 锁定 / 删除 / 改密 / 授权 / 回收 全部拒绝，且不执行任何写语句
	wantErr(t, e.svc.SetAccountLock(context.Background(), root, 1, LockReq{User: "root", Host: "localhost", Lock: true, Confirm: true}), 403, "不能对账号")
	wantErr(t, e.svc.DropAccount(context.Background(), root, 1, DropReq{User: "root", Host: "127.0.0.1", Confirm: true}), 403, "不能对账号")
	wantErr(t, e.svc.ResetPassword(context.Background(), root, 1, PasswordReq{User: "root", Host: "::1", Password: "N3w-Passw0rd!", Password2: "N3w-Passw0rd!", Confirm: true}), 403, "不能对账号")
	for _, op := range []string{"grant", "revoke"} {
		req := GrantReq{User: "root", Host: "localhost", Op: op, Scope: "db", DBs: []string{"shop"}, Privs: []string{"SELECT"}, Confirm: true}
		_, err := e.svc.Grant(context.Background(), root, 1, req)
		wantErr(t, err, 403, "内置账号")
		_, err = e.svc.PreviewGrant(context.Background(), root, 1, req)
		wantErr(t, err, 403, "内置账号")
	}
	if n := len(e.target.execs()); n != 0 {
		t.Fatalf("不应执行任何写语句，实际 %d 条", n)
	}
	// 非内置的 root@% 不受影响
	if err := e.svc.SetAccountLock(context.Background(), root, 1, LockReq{User: "root", Host: "%", Lock: true, Confirm: true}); err != nil {
		t.Fatal(err)
	}
}

// ---- 授权语句生成 ----

func TestBuildGrantStatements(t *testing.T) {
	base := GrantReq{User: "app_rw", Host: "10.20.%", Op: "grant"}
	cases := []struct {
		name string
		req  func(r *GrantReq)
		want []string
	}{
		{"全局", func(r *GrantReq) { r.Scope, r.Privs = "global", []string{"insert", "select"} }, []string{"GRANT SELECT, INSERT ON *.* TO 'app_rw'@'10.20.%';"}},
		{"库级多库", func(r *GrantReq) {
			r.Scope, r.DBs, r.Privs = "db", []string{"shop", "report"}, []string{"SELECT", "UPDATE", "INSERT"}
		},
			[]string{"GRANT SELECT, INSERT, UPDATE ON `shop`.* TO 'app_rw'@'10.20.%';", "GRANT SELECT, INSERT, UPDATE ON `report`.* TO 'app_rw'@'10.20.%';"}},
		{"GRANT 别名", func(r *GrantReq) { r.Scope, r.DBs, r.Privs = "db", []string{"shop"}, []string{"select", "grant"} },
			[]string{"GRANT SELECT, GRANT OPTION ON `shop`.* TO 'app_rw'@'10.20.%';"}},
		{"表级", func(r *GrantReq) {
			r.Scope, r.DBs, r.Tables, r.Privs = "table", []string{"shop"}, []string{"orders", "items"}, []string{"SELECT"}
		},
			[]string{"GRANT SELECT ON `shop`.`orders` TO 'app_rw'@'10.20.%';", "GRANT SELECT ON `shop`.`items` TO 'app_rw'@'10.20.%';"}},
		{"列级", func(r *GrantReq) {
			r.Scope, r.DBs, r.Tables, r.Columns, r.Privs = "column", []string{"shop"}, []string{"orders"}, []string{"id", "price"}, []string{"UPDATE", "SELECT"}
		}, []string{"GRANT SELECT (`id`, `price`), UPDATE (`id`, `price`) ON `shop`.`orders` TO 'app_rw'@'10.20.%';"}},
		{"回收", func(r *GrantReq) {
			r.Op, r.Scope, r.DBs, r.Privs = "revoke", "db", []string{"shop"}, []string{"DELETE"}
		},
			[]string{"REVOKE DELETE ON `shop`.* FROM 'app_rw'@'10.20.%';"}},
		{"库名含反引号", func(r *GrantReq) { r.Scope, r.DBs, r.Privs = "db", []string{"a`b"}, []string{"SELECT"} },
			[]string{"GRANT SELECT ON `a``b`.* TO 'app_rw'@'10.20.%';"}},
		{"账号名含引号只在展示里转义", func(r *GrantReq) { r.User, r.Scope, r.Privs = "o'brien", "global", []string{"SELECT"} },
			[]string{"GRANT SELECT ON *.* TO 'o\\'brien'@'10.20.%';"}},
		{"重复的库与权限", func(r *GrantReq) {
			r.Scope, r.DBs, r.Privs = "db", []string{"shop", "shop"}, []string{"SELECT", "select"}
		},
			[]string{"GRANT SELECT ON `shop`.* TO 'app_rw'@'10.20.%';"}},
	}
	for _, c := range cases {
		req := base
		c.req(&req)
		plan, err := buildGrant(req)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if strings.Join(plan.Statements, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s:\n got %v\nwant %v", c.name, plan.Statements, c.want)
		}
		for _, st := range plan.stmts {
			if len(st.args) != 2 || st.args[0] != req.User || st.args[1] != req.Host || strings.Contains(st.sql, req.User+"'") {
				t.Errorf("%s：账号必须走参数占位符: %+v", c.name, st)
			}
		}
	}
}

func TestBuildGrantRejects(t *testing.T) {
	ok := GrantReq{User: "u", Host: "h", Op: "grant", Scope: "db", DBs: []string{"shop"}, Privs: []string{"SELECT"}}
	cases := []struct {
		name string
		mod  func(r *GrantReq)
		want string
	}{
		{"操作", func(r *GrantReq) { r.Op = "drop" }, "操作类型"},
		{"范围", func(r *GrantReq) { r.Scope = "everything" }, "权限类型"},
		{"没有权限", func(r *GrantReq) { r.Privs = nil }, "请选择"},
		{"注入权限", func(r *GrantReq) { r.Privs = []string{"SELECT ON *.* TO evil@'%'; --"} }, "不支持权限"},
		// ALL PRIVILEGES 只在全局 / 库级白名单里（见 TestBuildGrantAllPrivileges），表级、列级仍然拒绝
		{"表级不能给 ALL PRIVILEGES", func(r *GrantReq) { r.Scope, r.Tables, r.Privs = "table", []string{"t"}, []string{"ALL PRIVILEGES"} }, "不支持权限"},
		{"列级不能给 ALL", func(r *GrantReq) {
			r.Scope, r.Tables, r.Columns, r.Privs = "column", []string{"t"}, []string{"c"}, []string{"ALL"}
		}, "不支持权限"},
		{"库级不能给 FILE", func(r *GrantReq) { r.Privs = []string{"FILE"} }, "不支持权限"},
		{"表级不能给 EXECUTE", func(r *GrantReq) { r.Scope, r.Tables, r.Privs = "table", []string{"t"}, []string{"EXECUTE"} }, "不支持权限"},
		{"列级不能给 DELETE", func(r *GrantReq) {
			r.Scope, r.Tables, r.Columns, r.Privs = "column", []string{"t"}, []string{"c"}, []string{"DELETE"}
		}, "不支持权限"},
		{"没选库", func(r *GrantReq) { r.DBs = nil }, "请选择数据库"},
		{"库名为空", func(r *GrantReq) { r.DBs = []string{""} }, "不合法"},
		{"库名过长", func(r *GrantReq) { r.DBs = []string{strings.Repeat("x", 65)} }, "不合法"},
		{"库名含换行", func(r *GrantReq) { r.DBs = []string{"a\nb"} }, "不合法"},
		{"表级选了两个库", func(r *GrantReq) { r.Scope, r.DBs, r.Tables = "table", []string{"a", "b"}, []string{"t"} }, "最多选择 1"},
		{"表级没选表", func(r *GrantReq) { r.Scope, r.Tables = "table", nil }, "请选择表"},
		{"列级没选列", func(r *GrantReq) { r.Scope, r.Tables, r.Columns = "column", []string{"t"}, nil }, "请选择列"},
		{"列级选了两张表", func(r *GrantReq) { r.Scope, r.Tables, r.Columns = "column", []string{"t1", "t2"}, []string{"c"} }, "最多选择 1"},
		{"没有账号", func(r *GrantReq) { r.User, r.Host = "", "" }, "缺少账号"},
	}
	for _, c := range cases {
		r := ok
		c.mod(&r)
		_, err := buildGrant(r)
		if err == nil {
			t.Errorf("%s：应该被拒绝", c.name)
			continue
		}
		wantErr(t, err, 400, c.want)
	}
}

func TestBuildGrantWarnings(t *testing.T) {
	plan, err := buildGrant(GrantReq{User: "u", Host: "h", Op: "grant", Scope: "global", Privs: []string{"SELECT", "FILE", "SUPER"}})
	if err != nil || len(plan.Warnings) != 2 {
		t.Fatalf("高危权限要警告: %+v %v", plan, err)
	}
	plan, _ = buildGrant(GrantReq{User: "u", Host: "h", Op: "grant", Scope: "db", DBs: []string{"my_db", "plain"}, Privs: []string{"SELECT"}})
	if !strings.Contains(plan.Statements[0], "`my\\_db`.*") || strings.Contains(plan.Statements[1], "\\") {
		t.Fatalf("含下划线的库名在库级授权里要转义: %v", plan.Statements)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "通配符") {
		t.Fatalf("%v", plan.Warnings)
	}
	plan, _ = buildGrant(GrantReq{User: "u", Host: "h", Op: "revoke", Scope: "db", DBs: []string{"my_db"}, Privs: []string{"SELECT"}})
	if plan.stmts[0].alt == nil || !strings.Contains(plan.stmts[0].alt.sql, "`my_db`.*") {
		t.Fatalf("回收要带上未转义的回退语句: %+v", plan.stmts[0])
	}
	plan, _ = buildGrant(GrantReq{User: "u", Host: "h", Op: "grant", Scope: "db", DBs: []string{"my_db"}, Privs: []string{"SELECT"}})
	if plan.stmts[0].alt != nil {
		t.Fatal("授权不需要回退语句")
	}
}

func TestPreviewAndExecuteGrant(t *testing.T) {
	e := accEnv()
	req := GrantReq{User: "app_rw", Host: "10.20.%", Op: "grant", Scope: "db", DBs: []string{"shop", "report"}, Privs: []string{"SELECT", "INSERT"}}
	plan, err := e.svc.PreviewGrant(context.Background(), root, 1, req)
	if err != nil || len(plan.Statements) != 2 || plan.Warnings == nil {
		t.Fatalf("%+v %v", plan, err)
	}
	if len(e.target.execs()) != 0 {
		t.Fatal("预览不能执行")
	}
	_, err = e.svc.Grant(context.Background(), root, 1, req)
	wantErr(t, err, 400, "确认")
	req.Confirm = true
	if _, err := e.svc.Grant(context.Background(), root, 1, req); err != nil {
		t.Fatal(err)
	}
	ex := e.target.execs()
	if len(ex) != 2 || ex[0].sql != "GRANT SELECT, INSERT ON `shop`.* TO ?@?" || ex[0].args[0] != "app_rw" || ex[0].args[1] != "10.20.%" {
		t.Fatalf("%+v", ex)
	}
	a := e.repo.lastAudit()
	if a.Action != "账号授权" || !strings.Contains(a.ExtraInfo, "GRANT SELECT, INSERT ON `shop`.* TO 'app_rw'@'10.20.%'") || !strings.Contains(a.ExtraInfo, "成功") {
		t.Fatalf("%+v", a)
	}
	// 账号不存在
	req.User = "ghost"
	_, err = e.svc.PreviewGrant(context.Background(), root, 1, req)
	wantErr(t, err, 404, "不存在")
	// 请求本身不合法，不连目标库
	n := len(e.dialer.dialed)
	req.Privs = []string{"FILE"}
	_, err = e.svc.Grant(context.Background(), root, 1, req)
	wantErr(t, err, 400, "不支持权限")
	if len(e.dialer.dialed) != n {
		t.Fatal("请求不合法时不应连目标库")
	}
}

func TestGrantStopsOnFailureAndFallsBack(t *testing.T) {
	e := accEnv()
	e.target.execErr["`report`"] = errors.New("Error 1044: Access denied")
	req := GrantReq{User: "app_rw", Host: "10.20.%", Op: "grant", Scope: "db", DBs: []string{"shop", "report", "log"}, Privs: []string{"SELECT"}, Confirm: true}
	_, err := e.svc.Grant(context.Background(), root, 1, req)
	wantErr(t, err, 502, "1044")
	if n := len(e.target.execs()); n != 2 {
		t.Fatalf("第二条失败后应停止，实际执行 %d 条", n)
	}
	a := e.repo.lastAudit()
	if !strings.Contains(a.ExtraInfo, "`shop`.* TO 'app_rw'@'10.20.%'; 成功") || !strings.Contains(a.ExtraInfo, "失败") {
		t.Fatalf("审计要记录已成功与失败的语句: %s", a.ExtraInfo)
	}

	// 已授出的库级权限库名可能没有转义：转义形式回收报 1141 时回退
	e2 := accEnv()
	e2.target.execErr["`my\\_db`"] = errors.New("Error 1141 (42000): There is no such grant defined for user 'app_rw' on host '10.20.%'")
	_, err = e2.svc.Grant(context.Background(), root, 1, GrantReq{User: "app_rw", Host: "10.20.%", Op: "revoke", Scope: "db", DBs: []string{"my_db"}, Privs: []string{"SELECT"}, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	ex := e2.target.execs()
	if len(ex) != 2 || !strings.Contains(ex[1].sql, "`my_db`.*") {
		t.Fatalf("应回退到未转义的库名: %+v", ex)
	}
}

func TestRevokeOnProtectedAccount(t *testing.T) {
	e := accEnv()
	req := GrantReq{User: "dba_ro", Host: "10.0.0.2", Op: "revoke", Scope: "global", Privs: []string{"SELECT"}, Confirm: true}
	_, err := e.svc.Grant(context.Background(), root, 1, req)
	wantErr(t, err, 403, "平台连接")
	if len(e.target.execs()) != 0 {
		t.Fatal("不能回收平台自己账号的权限")
	}
	req.Op = "grant" // 授权是允许的
	if _, err := e.svc.Grant(context.Background(), root, 1, req); err != nil {
		t.Fatal(err)
	}
}

// ---- 创建账号 ----

func TestCreateAccounts(t *testing.T) {
	e := accEnv()
	in := AccountCreate{User: "new_app", Host: "10.1.%|10.2.% | 10.1.%", Password: "Passw0rd!x", Password2: "Passw0rd!x", Remark: "新应用", Confirm: true}
	res, err := e.svc.CreateAccounts(context.Background(), root, 1, in)
	if err != nil || len(res.Created) != 2 || len(res.Failed) != 0 {
		t.Fatalf("多个主机各建一个账号并去重: %+v %v", res, err)
	}
	ex := e.target.execs()
	if len(ex) != 2 || ex[0].sql != "CREATE USER ?@? IDENTIFIED BY ?" || ex[0].args[0] != "new_app" || ex[0].args[1] != "10.1.%" || ex[0].args[2] != "Passw0rd!x" {
		t.Fatalf("密码应以明文参数交给驱动（驱动负责转义）: %+v", ex)
	}
	if _, isSecret := ex[0].args[2].(secret); isSecret {
		t.Fatal("交给驱动前必须还原成普通字符串")
	}
	if e.repo.accts[1]["new_app@10.1.%"].Remark != "新应用" || e.repo.acctPw["1|new_app@10.2.%"] != "Passw0rd!x" {
		t.Fatalf("应在平台登记备注和密码: %+v %+v", e.repo.accts, e.repo.acctPw)
	}
	for _, a := range e.repo.audits {
		if strings.Contains(a.ExtraInfo, "Passw0rd") {
			t.Fatalf("审计里不能有密码: %s", a.ExtraInfo)
		}
	}
	if a := e.repo.lastAudit(); a.Action != "创建数据库账号" || !strings.Contains(a.ExtraInfo, "'******'") {
		t.Fatalf("%+v", a)
	}
}

func TestCreateAccountsValidation(t *testing.T) {
	good := AccountCreate{User: "new_app", Host: "10.1.%", Password: "Passw0rd!x", Password2: "Passw0rd!x", Confirm: true}
	cases := []struct {
		name string
		mod  func(in *AccountCreate)
		want string
	}{
		{"账号含引号", func(in *AccountCreate) { in.User = "a'b" }, "账号名"},
		{"账号为空", func(in *AccountCreate) { in.User = "" }, "账号名"},
		{"账号过长", func(in *AccountCreate) { in.User = strings.Repeat("a", 33) }, "账号名"},
		{"主机含引号", func(in *AccountCreate) { in.Host = "x'@'y" }, "主机"},
		{"主机含空格", func(in *AccountCreate) { in.Host = "a b" }, "主机"},
		{"主机为空", func(in *AccountCreate) { in.Host = " | " }, "主机不能为空"},
		{"两次密码不一致", func(in *AccountCreate) { in.Password2 = "other" }, "不一致"},
		{"密码太短", func(in *AccountCreate) { in.Password, in.Password2 = "Ab1!", "Ab1!" }, "8 到 32"},
		{"密码太长", func(in *AccountCreate) { p := strings.Repeat("Ab1!", 9); in.Password, in.Password2 = p, p }, "8 到 32"},
		{"密码缺特殊符号", func(in *AccountCreate) { in.Password, in.Password2 = "Passw0rdxx", "Passw0rdxx" }, "英文、数字和特殊符号"},
		{"密码缺数字", func(in *AccountCreate) { in.Password, in.Password2 = "Password!!", "Password!!" }, "英文、数字和特殊符号"},
		{"密码缺字母", func(in *AccountCreate) { in.Password, in.Password2 = "12345678!!", "12345678!!" }, "英文、数字和特殊符号"},
		{"密码有空格", func(in *AccountCreate) { in.Password, in.Password2 = "Pass w0rd!", "Pass w0rd!" }, "空格"},
		{"密码有中文", func(in *AccountCreate) { in.Password, in.Password2 = "密码Passw0rd!", "密码Passw0rd!" }, "空格或中文"},
		{"备注过长", func(in *AccountCreate) { in.Remark = strings.Repeat("x", 256) }, "备注"},
		{"没有确认", func(in *AccountCreate) { in.Confirm = false }, "确认"},
	}
	for _, c := range cases {
		e := accEnv()
		in := good
		c.mod(&in)
		_, err := e.svc.CreateAccounts(context.Background(), root, 1, in)
		if err == nil {
			t.Errorf("%s：应该失败", c.name)
			continue
		}
		wantErr(t, err, 400, c.want)
		if len(e.dialer.dialed) != 0 || len(e.repo.audits) != 0 {
			t.Errorf("%s：校验失败不应连库或审计", c.name)
		}
	}
	e := accEnv()
	if _, err := e.svc.CreateAccounts(context.Background(), root, 4, good); err == nil { // doris
		t.Fatal("doris 不支持账号管理")
	}
}

func TestCreateAccountsPartialFailure(t *testing.T) {
	e := accEnv()
	ft := &failNth{fakeTarget: e.target, failAt: 2}
	svc := NewService(e.repo, dialerFunc(func() Target { return ft }))
	res, err := svc.CreateAccounts(context.Background(), root, 1, AccountCreate{User: "new_app", Host: "a|b|c", Password: "Passw0rd!x", Password2: "Passw0rd!x", Confirm: true})
	if err != nil || len(res.Created) != 2 || len(res.Failed) != 1 || !strings.Contains(res.Failed[0], "'b'") {
		t.Fatalf("部分成功: %+v %v", res, err)
	}
	if _, ok := e.repo.accts[1]["new_app@b"]; ok {
		t.Fatal("失败的主机不能登记")
	}
	// 全部失败：报错
	ft2 := &failNth{fakeTarget: newTarget(), all: true}
	svc2 := NewService(e.repo, dialerFunc(func() Target { return ft2 }))
	_, err = svc2.CreateAccounts(context.Background(), root, 1, AccountCreate{User: "x_app", Host: "a", Password: "Passw0rd!x", Password2: "Passw0rd!x", Confirm: true})
	wantErr(t, err, 502, "创建失败")
}

// failNth 让第 N 次 Exec 失败。
type failNth struct {
	*fakeTarget
	failAt int
	all    bool
	n      int
}

func (f *failNth) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	f.n++
	if f.all || f.n == f.failAt {
		return 0, errors.New("Error 1396: Operation CREATE USER failed")
	}
	return f.fakeTarget.Exec(ctx, sql, args...)
}

type dialerFunc func() Target

func (d dialerFunc) Dial(context.Context, ConnInfo) (Target, error) { return d(), nil }
func (d dialerFunc) Probe(context.Context, ConnInfo) (string, error) {
	return "", nil
}

// ---- 录入 / 改密 / 锁定 / 删除 ----

func TestSaveAccountInfo(t *testing.T) {
	e := accEnv()
	in := AccountInfo{User: "app_rw", Host: "10.20.%", Password: "registered-pw", Remark: "商城应用读写"}
	if err := e.svc.SaveAccountInfo(bob, 1, in); err != nil {
		t.Fatal(err)
	}
	if e.repo.accts[1]["app_rw@10.20.%"].Remark != "商城应用读写" || e.repo.acctPw["1|app_rw@10.20.%"] != "registered-pw" {
		t.Fatalf("%+v", e.repo.accts)
	}
	a := e.repo.lastAudit()
	if a.Action != "登记账号信息" || strings.Contains(a.ExtraInfo, "registered-pw") {
		t.Fatalf("审计不能有密码: %+v", a)
	}
	// 密码留空：不改已登记的密码
	in.Password, in.Remark = "", "改备注"
	if err := e.svc.SaveAccountInfo(bob, 1, in); err != nil || e.repo.acctPw["1|app_rw@10.20.%"] != "registered-pw" {
		t.Fatalf("%v %+v", err, e.repo.acctPw)
	}
	if len(e.dialer.dialed) != 0 {
		t.Fatal("录入只改平台信息，不连目标库")
	}
	wantErr(t, e.svc.SaveAccountInfo(bob, 1, AccountInfo{Host: "h"}), 400, "账号")
	wantErr(t, e.svc.SaveAccountInfo(bob, 2, in), 403, "未关联")
	wantErr(t, e.svc.SaveAccountInfo(bob, 3, in), 400, "暂不支持")
}

func TestResetPassword(t *testing.T) {
	e := accEnv()
	e.repo.accts[1] = map[string]AccountRecord{"app_rw@10.20.%": {User: "app_rw", Host: "10.20.%", Remark: "keep-me"}}
	in := PasswordReq{User: "app_rw", Host: "10.20.%", Password: "N3w-Passw0rd!", Password2: "N3w-Passw0rd!", Confirm: true}
	if err := e.svc.ResetPassword(context.Background(), root, 1, in); err != nil {
		t.Fatal(err)
	}
	ex := e.target.execs()
	if len(ex) != 1 || ex[0].sql != "ALTER USER ?@? IDENTIFIED BY ?" || ex[0].args[2] != "N3w-Passw0rd!" {
		t.Fatalf("%+v", ex)
	}
	if e.repo.acctPw["1|app_rw@10.20.%"] != "N3w-Passw0rd!" || e.repo.accts[1]["app_rw@10.20.%"].Remark != "keep-me" {
		t.Fatalf("应更新登记的密码但不动备注: %+v %+v", e.repo.acctPw, e.repo.accts)
	}
	a := e.repo.lastAudit()
	if a.Action != "修改数据库账号密码" || strings.Contains(a.ExtraInfo, "N3w-Passw0rd") || !strings.Contains(a.ExtraInfo, "'******'") {
		t.Fatalf("%+v", a)
	}
	for name, mod := range map[string]func(*PasswordReq){
		"不一致": func(r *PasswordReq) { r.Password2 = "x" },
		"太弱":  func(r *PasswordReq) { r.Password, r.Password2 = "weakweak", "weakweak" },
		"未确认": func(r *PasswordReq) { r.Confirm = false },
	} {
		bad := in
		mod(&bad)
		if err := e.svc.ResetPassword(context.Background(), root, 1, bad); err == nil {
			t.Errorf("%s：应该失败", name)
		}
	}
	in.User, in.Host = "ghost", "%"
	wantErr(t, e.svc.ResetPassword(context.Background(), root, 1, in), 404, "不存在")
	// 平台自己的账号、内置账号不能改密
	for _, a := range [][2]string{{"dba_ro", "10.0.0.2"}, {"mysql.sys", "localhost"}} {
		in.User, in.Host = a[0], a[1]
		wantErr(t, e.svc.ResetPassword(context.Background(), root, 1, in), 403, "不能对账号")
	}
	// 执行失败：不更新登记
	e2 := accEnv()
	e2.target.execErr["ALTER USER"] = errors.New("Error 1819: Your password does not satisfy the current policy")
	in = PasswordReq{User: "app_rw", Host: "10.20.%", Password: "N3w-Passw0rd!", Password2: "N3w-Passw0rd!", Confirm: true}
	wantErr(t, e2.svc.ResetPassword(context.Background(), root, 1, in), 502, "1819")
	if len(e2.repo.acctPw) != 0 {
		t.Fatal("失败时不能更新登记的密码")
	}
}

func TestSetAccountLock(t *testing.T) {
	e := accEnv()
	wantErr(t, e.svc.SetAccountLock(context.Background(), root, 1, LockReq{User: "app_rw", Host: "10.20.%", Lock: true}), 400, "确认")
	if err := e.svc.SetAccountLock(context.Background(), root, 1, LockReq{User: "app_rw", Host: "10.20.%", Lock: true, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if ex := e.target.execs(); len(ex) != 1 || ex[0].sql != "ALTER USER ?@? ACCOUNT LOCK" {
		t.Fatalf("%+v", ex)
	}
	if err := e.svc.SetAccountLock(context.Background(), root, 1, LockReq{User: "app_rw", Host: "10.20.%", Lock: false, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if ex := e.target.execs(); ex[1].sql != "ALTER USER ?@? ACCOUNT UNLOCK" || e.repo.lastAudit().Action != "解锁数据库账号" {
		t.Fatalf("%+v %+v", ex, e.repo.lastAudit())
	}
	wantErr(t, e.svc.SetAccountLock(context.Background(), root, 1, LockReq{User: "dba_ro", Host: "10.0.0.2", Lock: true, Confirm: true}), 403, "平台连接")
	// 5.6 不支持
	e2 := accEnv()
	e2.target.handlers = append([]handler{{"SELECT VERSION()", func(string, []any) (*Result, error) { return rs([]string{"v"}, []any{"5.6.40"}), nil }}}, e2.target.handlers...)
	wantErr(t, e2.svc.SetAccountLock(context.Background(), root, 1, LockReq{User: "app_rw", Host: "10.20.%", Lock: true, Confirm: true}), 400, "不支持锁定")
}

func TestDropAccount(t *testing.T) {
	e := accEnv()
	e.repo.accts[1] = map[string]AccountRecord{"report@10.20.%": {User: "report", Host: "10.20.%"}}
	wantErr(t, e.svc.DropAccount(context.Background(), root, 1, DropReq{User: "report", Host: "10.20.%"}), 400, "确认")
	if err := e.svc.DropAccount(context.Background(), root, 1, DropReq{User: "report", Host: "10.20.%", Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if ex := e.target.execs(); len(ex) != 1 || ex[0].sql != "DROP USER ?@?" || ex[0].args[0] != "report" {
		t.Fatalf("%+v", ex)
	}
	if _, ok := e.repo.accts[1]["report@10.20.%"]; ok {
		t.Fatal("应清理平台登记")
	}
	if a := e.repo.lastAudit(); a.Action != "删除数据库账号" || !strings.Contains(a.ExtraInfo, "DROP USER 'report'@'10.20.%'") {
		t.Fatalf("%+v", a)
	}
	for _, a := range [][2]string{{"dba_ro", "10.0.0.2"}, {"mysql.sys", "localhost"}} {
		wantErr(t, e.svc.DropAccount(context.Background(), root, 1, DropReq{User: a[0], Host: a[1], Confirm: true}), 403, "不能对账号")
	}
	wantErr(t, e.svc.DropAccount(context.Background(), root, 1, DropReq{User: "ghost", Host: "%", Confirm: true}), 404, "不存在")
	wantErr(t, e.svc.DropAccount(context.Background(), root, 1, DropReq{Confirm: true}), 400, "缺少账号")
}

func TestListObjects(t *testing.T) {
	e := accEnv()
	e.target.on("information_schema.schemata", rs([]string{"n"}, []any{"shop"}, []any{"report"}))
	e.target.on("information_schema.tables WHERE table_schema = ?", rs([]string{"n"}, []any{"orders"}))
	e.target.on("information_schema.columns", rs([]string{"n"}, []any{"id"}, []any{"price"}))
	dbs, err := e.svc.ListObjects(context.Background(), root, 1, "", "")
	if err != nil || len(dbs) != 2 {
		t.Fatalf("%v %v", dbs, err)
	}
	tbs, _ := e.svc.ListObjects(context.Background(), root, 1, "shop", "")
	cols, _ := e.svc.ListObjects(context.Background(), root, 1, "shop", "orders")
	if len(tbs) != 1 || len(cols) != 2 {
		t.Fatalf("%v %v", tbs, cols)
	}
	if q := e.target.queries("information_schema.columns")[0]; q.args[0] != "shop" || q.args[1] != "orders" {
		t.Fatalf("库名表名要走参数: %+v", q)
	}
}
