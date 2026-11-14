// @author: kevinkelly
// @license: Apache Licence
// @file: views_test.go
// @time: 2026/10/28

package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/engine"
)

func argStrings(args []driver.NamedValue) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i], _ = a.Value.(string)
	}
	return out
}

func TestViewDefs(t *testing.T) {
	e, srv := newFake(t, "mysql")
	srv.viewRows = [][]driver.Value{
		{"shop", "v_customers", "select `shop`.`customers`.`phone` AS `mobile` from `shop`.`customers`"},
		{"shop", "v_locked", nil}, // 没有 SHOW VIEW 权限：定义为空
	}
	defs, err := e.ViewDefs(context.Background(), "shop", []engine.ViewRef{{Name: "v_customers"}, {Schema: "Other", Name: "V2"}, {Name: "v_locked"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.ViewDef{
		{Schema: "shop", Name: "v_customers", Definition: "select `shop`.`customers`.`phone` AS `mobile` from `shop`.`customers`"},
		{Schema: "shop", Name: "v_locked"},
	}
	if !reflect.DeepEqual(defs, want) {
		t.Errorf("defs=%+v", defs)
	}
	// 没写库名的按当前库；名字与它的小写形式都带上；绑定参数，不拼进 SQL
	if want := []string{"shop", "v_customers", "Other", "V2", "other", "v2", "shop", "v_locked"}; !reflect.DeepEqual(argStrings(srv.viewArgs), want) {
		t.Errorf("args=%q", argStrings(srv.viewArgs))
	}
	var q string
	for _, s := range srv.stmts() {
		if strings.Contains(s, "information_schema.VIEWS") {
			q = s
		}
	}
	for _, sub := range []string{"VIEW_DEFINITION", "TABLE_SCHEMA = ? AND TABLE_NAME = ?", "NOT IN ('information_schema', 'performance_schema', 'mysql', 'sys')"} {
		if !strings.Contains(q, sub) {
			t.Errorf("SQL 缺少 %q: %s", sub, q)
		}
	}
	if strings.Contains(q, "v_customers") || strings.Contains(q, "Other") {
		t.Errorf("名字不应拼进 SQL: %s", q)
	}
}

func TestViewDefsNoQueryWhenNothingToAsk(t *testing.T) {
	e, srv := newFake(t, "mysql")
	// 没有当前库、名字又没写库名：MySQL 自己也解析不了，不查
	defs, err := e.ViewDefs(context.Background(), "", []engine.ViewRef{{Name: "v"}, {}})
	if err != nil || len(defs) != 0 {
		t.Fatalf("%v %v", defs, err)
	}
	for _, s := range srv.stmts() {
		if strings.Contains(s, "information_schema.VIEWS") {
			t.Errorf("不该查目录: %s", s)
		}
	}
	if q, args := viewDefsQuery("shop", nil); q != "" || args != nil {
		t.Errorf("空名单: %q %v", q, args)
	}
}

func TestViewDefsOversizedDefinitionIsUnreadable(t *testing.T) {
	e, srv := newFake(t, "mysql")
	srv.viewRows = [][]driver.Value{{"shop", "v_big", strings.Repeat("x", engine.MaxViewDefBytes+1)}}
	defs, err := e.ViewDefs(context.Background(), "shop", []engine.ViewRef{{Name: "v_big"}})
	if err != nil || len(defs) != 1 || defs[0].Definition != "" {
		t.Errorf("过大的定义当作读不到: %v %v", defs, err)
	}
}

// 目录表 / 列不存在（兼容协议的库）映射成 ErrViewDefsUnsupported；权限不足、连接错误等原样返回。
func TestViewDefsErrors(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		unsupported bool
	}{
		{"1146 表不存在", &drv.MySQLError{Number: 1146, Message: "Table 'information_schema.VIEWS' doesn't exist"}, true},
		{"1109 未知表", &drv.MySQLError{Number: 1109, Message: "Unknown table 'VIEWS' in information_schema"}, true},
		{"1054 未知列", &drv.MySQLError{Number: 1054, Message: "Unknown column 'VIEW_DEFINITION' in 'field list'"}, true},
		{"Doris 一类只报 1105 加文字", &drv.MySQLError{Number: 1105, Message: "errCode = 2, detailMessage = Unknown table 'views'"}, true},
		{"1142 没有 SELECT 权限", &drv.MySQLError{Number: 1142, Message: "SELECT command denied to user 'q'@'%' for table 'VIEWS'"}, false},
		{"连接错误", errors.New("connection reset by peer"), false},
	}
	for _, c := range cases {
		e, srv := newFake(t, "mysql")
		srv.viewErr = c.err
		_, err := e.ViewDefs(context.Background(), "shop", []engine.ViewRef{{Name: "v"}})
		if err == nil {
			t.Errorf("%s: 应返回错误", c.name)
			continue
		}
		if got := errors.Is(err, engine.ErrViewDefsUnsupported); got != c.unsupported {
			t.Errorf("%s: unsupported=%v，期望 %v（%v）", c.name, got, c.unsupported, err)
		}
	}
}
