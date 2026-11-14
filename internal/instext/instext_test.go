// @author: kevinkelly
// @license: Apache Licence
// @file: instext_test.go
// @time: 2026/10/28

package instext

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 记录语句的假驱动：information_schema.tables 的计数由 have 决定，其余查询返回 rows 里预设的 instance_id 列。
type fakeDB struct {
	mu    sync.Mutex
	have  bool
	ids   []int64
	envs  map[int64]string // instance_id -> env（Envs 查询用）
	log   []string
	count int // HasTable 查询次数
}

type fakeConnector struct{ d *fakeDB }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{c.d}, nil }
func (c fakeConnector) Driver() driver.Driver                        { return nil }

type fakeConn struct{ d *fakeDB }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, io.ErrUnexpectedEOF }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, io.ErrUnexpectedEOF }
func (c *fakeConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.log = append(c.d.log, q)
	if strings.Contains(q, "information_schema.tables") {
		c.d.count++
		n := int64(0)
		if c.d.have {
			n = 1
		}
		return &fakeRows{cols: []string{"count(*)"}, rows: [][]driver.Value{{n}}}, nil
	}
	if strings.Contains(q, "instance_id, env") {
		var rows [][]driver.Value
		for id, e := range c.d.envs {
			rows = append(rows, []driver.Value{id, e})
		}
		return &fakeRows{cols: []string{"instance_id", "env"}, rows: rows}, nil
	}
	var rows [][]driver.Value
	for _, id := range c.d.ids {
		rows = append(rows, []driver.Value{id})
	}
	return &fakeRows{cols: []string{"instance_id"}, rows: rows}, nil
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func open(t *testing.T, d *fakeDB) *gorm.DB {
	t.Helper()
	sqlDB := sql.OpenDB(fakeConnector{d})
	t.Cleanup(func() { sqlDB.Close() })
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{Logger: logger.Discard, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func (d *fakeDB) stmts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.log...)
}

type inst struct {
	ID uint
}

func (inst) TableName() string { return "dbt_sql_instance" }

func TestExcludeDisabledAddsConditionOnlyWhenTableExists(t *testing.T) {
	Reset()
	defer Reset()
	d := &fakeDB{have: true}
	db := open(t, d)
	var out []inst
	if err := ExcludeDisabled(db, "dbt_sql_instance.id").Find(&out).Error; err != nil {
		t.Fatal(err)
	}
	last := d.stmts()[len(d.stmts())-1]
	if !strings.Contains(last, "dbt_sql_instance.id NOT IN (SELECT instance_id FROM dbt_instance_ext WHERE disabled = 1)") {
		t.Fatalf("应排除已停用实例: %s", last)
	}
	if c := DisabledCond(db, "i.id"); c != " AND i.id NOT IN (SELECT instance_id FROM dbt_instance_ext WHERE disabled = 1)" {
		t.Fatalf("%q", c)
	}

	// 表不存在：语句与引入本包之前完全一致
	Reset()
	d2 := &fakeDB{have: false}
	db2 := open(t, d2)
	if err := ExcludeDisabled(db2, "dbt_sql_instance.id").Find(&out).Error; err != nil {
		t.Fatal(err)
	}
	last = d2.stmts()[len(d2.stmts())-1]
	if strings.Contains(last, "dbt_instance_ext") || strings.Contains(last, "WHERE") {
		t.Fatalf("表不存在时不能加条件: %s", last)
	}
	if DisabledCond(db2, "i.id") != "" {
		t.Fatal("表不存在时片段为空")
	}
	if m, err := DisabledSet(db2); err != nil || len(m) != 0 {
		t.Fatalf("%v %v", m, err)
	}
}

func TestReadyCachesBothWays(t *testing.T) {
	Reset()
	defer Reset()
	d := &fakeDB{have: false}
	db := open(t, d)
	for i := 0; i < 5; i++ {
		if Ready(db) {
			t.Fatal("表不存在")
		}
	}
	if d.count != 1 {
		t.Fatalf("不存在时 30 秒内不重复检查，实际检查 %d 次", d.count)
	}
	// 建表后：缓存过期重查即生效；一旦存在就不再检查
	d.have = true
	mu.Lock()
	lastNot = lastNot.Add(-2 * notReadyRecheck)
	mu.Unlock()
	if !Ready(db) || !Ready(db) || !Ready(db) {
		t.Fatal("建表后应生效")
	}
	if d.count != 2 {
		t.Fatalf("存在后不再检查: %d", d.count)
	}
	if Ready(nil) {
		t.Fatal("nil 库不可用")
	}
}

func TestDisabledSet(t *testing.T) {
	Reset()
	defer Reset()
	d := &fakeDB{have: true, ids: []int64{3, 7}}
	m, err := DisabledSet(open(t, d))
	if err != nil || len(m) != 2 || !m[3] || !m[7] {
		t.Fatalf("%v %v", m, err)
	}
}

func TestEnvHelpers(t *testing.T) {
	for _, e := range []string{"", EnvProd, EnvTest} {
		if !ValidEnv(e) {
			t.Errorf("%q 应合法", e)
		}
	}
	if ValidEnv("dev") || ValidEnv("PROD") {
		t.Error("非法环境")
	}
	if EnvLabel(EnvProd) != "生产" || EnvLabel(EnvTest) != "测试" || EnvLabel("") != "" {
		t.Error("显示名")
	}
}

func TestEnvs(t *testing.T) {
	Reset()
	defer Reset()
	d := &fakeDB{have: true, envs: map[int64]string{1: "prod", 2: "test", 3: "  "}}
	db := open(t, d)
	m, err := Envs(db, []uint{1, 2, 3, 4})
	if err != nil || len(m) != 2 || m[1] != EnvProd || m[2] != EnvTest {
		t.Fatalf("空环境与没有记录的实例不进结果: %v %v", m, err)
	}
	last := d.stmts()[len(d.stmts())-1]
	if !strings.Contains(last, "FROM `dbt_instance_ext`") || !strings.Contains(last, "instance_id IN (") {
		t.Fatalf("一次 IN 查询: %s", last)
	}
	// 没有实例：不查库
	n := len(d.stmts())
	if m, err := Envs(db, nil); err != nil || len(m) != 0 || len(d.stmts()) != n {
		t.Fatalf("空 ID 不该查库: %v %v", m, err)
	}
}

func TestEnvsWithoutTable(t *testing.T) {
	Reset()
	defer Reset()
	d := &fakeDB{have: false, envs: map[int64]string{1: "prod"}}
	m, err := Envs(open(t, d), []uint{1})
	if err != nil || len(m) != 0 {
		t.Fatalf("表不存在返回空集合: %v %v", m, err)
	}
	for _, s := range d.stmts() {
		if strings.Contains(s, "instance_id, env") {
			t.Fatalf("表不存在时不该查旁路表: %s", s)
		}
	}
	if m, err := Envs(nil, []uint{1}); err != nil || len(m) != 0 {
		t.Fatalf("nil 库: %v %v", m, err)
	}
}
