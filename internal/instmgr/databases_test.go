// @author: kevinkelly
// @license: Apache Licence
// @file: databases_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func dbEnv() *env {
	e := newEnv()
	// 创建前的重名检查：只有 shop 已存在
	e.target.onFunc("WHERE schema_name = ?", func(_ string, args []any) (*Result, error) {
		if args[0] == "shop" {
			return rs([]string{"n"}, []any{int64(1)}), nil
		}
		return rs([]string{"n"}), nil
	})
	e.target.on("character_sets", rs([]string{"name", "def"}, []any{"latin1", "latin1_swedish_ci"}, []any{"utf8mb4", "utf8mb4_0900_ai_ci"}))
	e.target.on("information_schema.collations", rs([]string{"name", "cs_name"},
		[]any{"latin1_swedish_ci", "latin1"}, []any{"utf8mb4_0900_ai_ci", "utf8mb4"}, []any{"utf8mb4_general_ci", "utf8mb4"}))
	e.target.on("information_schema.schemata", rs([]string{"schema_name", "db_charset", "db_collation"},
		[]any{"shop", "utf8mb4", "utf8mb4_0900_ai_ci"}, []any{"tmp_migrate_0912", "utf8", "utf8_general_ci"}))
	e.target.on("schema_privileges", rs([]string{"db", "grantee"}, []any{"shop", "'app_rw'@'10.20.%'"}, []any{"shop", "'dba_ro'@'%'"}))
	return e
}

func TestListDatabasesMergesRegistration(t *testing.T) {
	e := dbEnv()
	e.repo.dbs[1] = map[string]DatabaseRecord{"shop": {DBName: "shop", Owner: "dev", OwnerDisplay: "演示开发者", Remark: "商城主库"}}
	list, err := e.svc.ListDatabases(context.Background(), root, 1, false, "")
	if err != nil || len(list) != 2 {
		t.Fatalf("%+v %v", list, err)
	}
	if !list[0].Saved || list[0].Owner != "dev" || list[0].Remark != "商城主库" || len(list[0].Grantees) != 2 {
		t.Fatalf("%+v", list[0])
	}
	if list[1].Saved || list[1].Grantees == nil || len(list[1].Grantees) != 0 {
		t.Fatalf("未录入的库: %+v", list[1])
	}
	if l, _ := e.svc.ListDatabases(context.Background(), root, 1, true, ""); len(l) != 1 || l[0].Name != "shop" {
		t.Fatalf("只看已录入: %+v", l)
	}
	if l, _ := e.svc.ListDatabases(context.Background(), root, 1, false, "TMP"); len(l) != 1 || l[0].Name != "tmp_migrate_0912" {
		t.Fatalf("搜索不区分大小写: %+v", l)
	}
	// 授权表读不出来（如 Doris）不影响库清单
	e2 := dbEnv()
	e2.target.handlers = append([]handler{{"schema_privileges", func(string, []any) (*Result, error) { return nil, errors.New("no table") }}}, e2.target.handlers...)
	if l, err := e2.svc.ListDatabases(context.Background(), root, 4, false, ""); err != nil || len(l) != 2 {
		t.Fatalf("%v %v", l, err)
	}
}

func TestCreateDatabase(t *testing.T) {
	e := dbEnv()
	in := DBCreate{Name: "new_shop", Charset: "UTF8MB4", Collation: "utf8mb4_general_ci", Owner: "dev", Remark: "新库", Confirm: true}
	if err := e.svc.CreateDatabase(context.Background(), root, 1, in); err != nil {
		t.Fatal(err)
	}
	ex := e.target.execs()
	if len(ex) != 1 || ex[0].sql != "CREATE DATABASE `new_shop` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci" {
		t.Fatalf("%+v", ex)
	}
	rec := e.repo.dbs[1]["new_shop"]
	if rec.Owner != "dev" || rec.OwnerDisplay != "演示开发者" || rec.Remark != "新库" {
		t.Fatalf("应登记负责人: %+v", rec)
	}
	a := e.repo.lastAudit()
	if a.Action != "创建数据库" || !strings.Contains(a.ExtraInfo, "new_shop") || !strings.Contains(a.ExtraInfo, "成功") {
		t.Fatalf("%+v", a)
	}
	// 不指定字符集：不带子句
	e2 := dbEnv()
	if err := e2.svc.CreateDatabase(context.Background(), root, 1, DBCreate{Name: "plain", Owner: "dev", Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if e2.target.execs()[0].sql != "CREATE DATABASE `plain`" {
		t.Fatalf("%+v", e2.target.execs())
	}
	// 只给排序规则时反推字符集
	e3 := dbEnv()
	if err := e3.svc.CreateDatabase(context.Background(), root, 1, DBCreate{Name: "co_only", Collation: "latin1_swedish_ci", Owner: "dev", Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if e3.target.execs()[0].sql != "CREATE DATABASE `co_only` DEFAULT CHARACTER SET latin1 COLLATE latin1_swedish_ci" {
		t.Fatalf("%+v", e3.target.execs())
	}
}

func TestCreateDatabaseValidation(t *testing.T) {
	cases := []struct {
		name string
		mod  func(in *DBCreate)
		code int
		want string
	}{
		{"库名带反引号", func(in *DBCreate) { in.Name = "a`; drop database x; --" }, 400, "数据库名"},
		{"库名带空格", func(in *DBCreate) { in.Name = "my db" }, 400, "数据库名"},
		{"数字开头", func(in *DBCreate) { in.Name = "1abc" }, 400, "数据库名"},
		{"下划线结尾", func(in *DBCreate) { in.Name = "abc_" }, 400, "数据库名"},
		{"中文", func(in *DBCreate) { in.Name = "商城" }, 400, "数据库名"},
		{"过长", func(in *DBCreate) { in.Name = "a" + strings.Repeat("b", 64) }, 400, "数据库名"},
		{"系统库", func(in *DBCreate) { in.Name = "mysql" }, 400, "系统库"},
		{"缺负责人", func(in *DBCreate) { in.Owner = "" }, 400, "负责人"},
		{"负责人不存在", func(in *DBCreate) { in.Owner = "ghost" }, 400, "负责人不存在"},
		{"备注过长", func(in *DBCreate) { in.Remark = strings.Repeat("x", 256) }, 400, "备注"},
		{"没有确认", func(in *DBCreate) { in.Confirm = false }, 400, "确认"},
		{"字符集不存在", func(in *DBCreate) { in.Charset = "evil;drop" }, 400, "不支持的字符集"},
		{"字符集与排序规则不匹配", func(in *DBCreate) { in.Charset, in.Collation = "latin1", "utf8mb4_general_ci" }, 400, "不属于"},
		{"排序规则不存在", func(in *DBCreate) { in.Collation = "x_ci" }, 400, "不支持的排序规则"},
	}
	for _, c := range cases {
		e := dbEnv()
		in := DBCreate{Name: "good_db", Owner: "dev", Confirm: true}
		c.mod(&in)
		err := e.svc.CreateDatabase(context.Background(), root, 1, in)
		if err == nil {
			t.Errorf("%s：应该失败", c.name)
			continue
		}
		wantErr(t, err, c.code, c.want)
		if len(e.target.execs()) != 0 || len(e.repo.audits) != 0 {
			t.Errorf("%s：校验失败不能执行也不用审计", c.name)
		}
	}
}

func TestCreateDatabaseConflictAndFailure(t *testing.T) {
	e := dbEnv()
	err := e.svc.CreateDatabase(context.Background(), root, 1, DBCreate{Name: "shop", Owner: "dev", Confirm: true})
	wantErr(t, err, 409, "已存在")

	e2 := dbEnv()
	e2.target.execErr["CREATE DATABASE"] = errors.New("Error 1044: Access denied for user")
	err = e2.svc.CreateDatabase(context.Background(), root, 1, DBCreate{Name: "newdb", Owner: "dev", Confirm: true})
	wantErr(t, err, 502, "1044")
	if _, ok := e2.repo.dbs[1]["newdb"]; ok {
		t.Fatal("创建失败不能登记")
	}
	if a := e2.repo.lastAudit(); a.Action != "创建数据库" || !strings.Contains(a.ExtraInfo, "失败") {
		t.Fatalf("失败也要记审计: %+v", a)
	}
}

func TestCreateDatabaseDorisNoCharset(t *testing.T) {
	e := dbEnv()
	err := e.svc.CreateDatabase(context.Background(), root, 4, DBCreate{Name: "dw", Charset: "utf8mb4", Owner: "dev", Confirm: true})
	wantErr(t, err, 400, "暂不支持")
	if err := e.svc.CreateDatabase(context.Background(), root, 4, DBCreate{Name: "dw", Owner: "dev", Confirm: true}); err != nil {
		t.Fatalf("Doris 不指定字符集可以创建: %v", err)
	}
	if e.target.execs()[0].sql != "CREATE DATABASE `dw`" {
		t.Fatalf("%+v", e.target.execs())
	}
	// 不可见 / 不支持
	wantErr(t, e.svc.CreateDatabase(context.Background(), bob, 2, DBCreate{Name: "x", Owner: "dev", Confirm: true}), 403, "未关联")
	wantErr(t, e.svc.CreateDatabase(context.Background(), root, 3, DBCreate{Name: "x", Owner: "dev", Confirm: true}), 400, "暂不支持")
}

func TestDatabaseOptions(t *testing.T) {
	e := dbEnv()
	o, err := e.svc.DatabaseOptions(context.Background(), root, 1, false)
	if err != nil || !o.CharsetEditable || len(o.Charsets) != 2 || len(o.Owners) != 2 {
		t.Fatalf("%+v %v", o, err)
	}
	if o.Charsets[1].Name != "utf8mb4" || len(o.Charsets[1].Collations) != 2 || o.Charsets[1].Default != "utf8mb4_0900_ai_ci" {
		t.Fatalf("%+v", o.Charsets)
	}
	o, err = e.svc.DatabaseOptions(context.Background(), root, 1, true)
	if err != nil || len(o.Owners) != 2 || len(o.Charsets) != 0 {
		t.Fatalf("%+v %v", o, err)
	}
	if len(e.dialer.dialed) != 1 {
		t.Fatalf("owners_only 不应连目标库: %d", len(e.dialer.dialed))
	}
	o, _ = e.svc.DatabaseOptions(context.Background(), root, 4, false)
	if o.CharsetEditable || len(o.Charsets) != 0 {
		t.Fatalf("Doris 不支持指定字符集: %+v", o)
	}
}

func TestSaveDatabaseInfo(t *testing.T) {
	e := dbEnv()
	if err := e.svc.SaveDatabaseInfo(bob, 1, DBInfo{Name: "shop", Owner: "wang", Remark: "报表库"}); err != nil {
		t.Fatal(err)
	}
	if r := e.repo.dbs[1]["shop"]; r.Owner != "wang" || r.OwnerDisplay != "王工" || r.Remark != "报表库" {
		t.Fatalf("%+v", r)
	}
	if e.repo.lastAudit().Action != "登记数据库信息" {
		t.Fatalf("%+v", e.repo.lastAudit())
	}
	wantErr(t, e.svc.SaveDatabaseInfo(bob, 1, DBInfo{Name: "shop", Owner: "ghost"}), 400, "负责人不存在")
	wantErr(t, e.svc.SaveDatabaseInfo(bob, 1, DBInfo{Name: "", Owner: "dev"}), 400, "数据库名")
	wantErr(t, e.svc.SaveDatabaseInfo(bob, 2, DBInfo{Name: "x", Owner: "dev"}), 403, "未关联")
	wantErr(t, e.svc.SaveDatabaseInfo(bob, 3, DBInfo{Name: "x", Owner: "dev"}), 400, "暂不支持")
	if len(e.dialer.dialed) != 0 {
		t.Fatal("登记信息不连目标库")
	}
}
