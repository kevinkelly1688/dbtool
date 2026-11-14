// @author: kevinkelly
// @license: Apache Licence
// @file: extras_test.go
// @time: 2026/10/28

package dm

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"dbtool/internal/engine"
)

func TestSearchColumns(t *testing.T) {
	s := &fakeScript{}
	s.add(fakeRule{match: "FROM all_tab_columns c", cols: []string{"TABLE_NAME", "COLUMN_NAME", "COMMENTS"}, rows: [][]driver.Value{
		{"EMPLOYEES", "PHONE", "手机号"}, {"EMPLOYEES", "PHONE2", nil},
	}})
	e := newFakeEngine(t, s)
	hits, err := e.SearchColumns(context.Background(), "HR", "  pho_ne%'x  ")
	if err != nil || len(hits) != 2 || hits[0] != (ColumnHit{"EMPLOYEES", "PHONE", "手机号"}) || hits[1].Comment != "" {
		t.Fatalf("%+v %v", hits, err)
	}
	q := s.log()[len(s.log())-1]
	// 通配符转义、单引号翻倍、关键字转大写、限定 owner、最多 500 条
	for _, sub := range []string{`'%PHO\_NE\%''X%'`, `ESCAPE '\'`, "c.owner = 'HR'", "ROWNUM <= 500"} {
		if !strings.Contains(q, sub) {
			t.Errorf("搜索语句缺少 %q：\n%s", sub, q)
		}
	}
	if hits, err := e.SearchColumns(context.Background(), "HR", "  "); err != nil || hits != nil {
		t.Errorf("空关键字: %v %v", hits, err)
	}
	if _, err := e.SearchColumns(context.Background(), `A"B`, "x"); err == nil {
		t.Error("非法 schema 名应拒绝")
	}
}

func TestIsSystemSchemaExported(t *testing.T) {
	for _, n := range []string{"SYS", "sys", "SYSAUDITOR", "SYSSSO", "CTISYS"} {
		if !IsSystemSchema(n) {
			t.Errorf("%s 应是系统 schema", n)
		}
	}
	for _, n := range []string{"HR", "SALES", "SYSDBA"} {
		if IsSystemSchema(n) {
			t.Errorf("%s 不是系统 schema", n)
		}
	}
}

func TestBigIntegersBecomeStrings(t *testing.T) {
	for _, c := range []struct {
		in   any
		want any
	}{
		{int64(1) << 53, "9007199254740992"},
		{-(int64(1) << 53), "-9007199254740992"},
		{int64(1<<53 - 1), int64(1<<53 - 1)},
		{int32(7), int64(7)},
		{int16(-3), int64(-3)},
		{7, int64(7)},
	} {
		if got, _, _ := shapeValue(c.in, "BIGINT", 100); got != c.want {
			t.Errorf("%v → %#v，期望 %#v", c.in, got, c.want)
		}
	}
}

func TestReadOnlyViolationIsWrapped(t *testing.T) {
	s := empScript()
	s.add(fakeRule{match: "from employees", err: errors.New("Error -6906: 事务只读，不能执行写操作")})
	e := newFakeEngine(t, s)
	_, err := runCheck(t, e, "select * from employees", QueryLimits{})
	if !errors.Is(err, engine.ErrReadOnlyViolation) || !IsReadOnlyViolation(err) {
		t.Fatalf("只读违规应包装成 engine.ErrReadOnlyViolation: %v", err)
	}
	s2 := empScript()
	s2.add(fakeRule{match: "from employees", err: errors.New("Error -2106: 无效的表或视图名")})
	e2 := newFakeEngine(t, s2)
	if _, err := runCheck(t, e2, "select * from employees", QueryLimits{}); errors.Is(err, engine.ErrReadOnlyViolation) {
		t.Errorf("普通错误不是只读违规: %v", err)
	}
}
