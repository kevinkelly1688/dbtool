// @author: kevinkelly
// @license: Apache Licence
// @file: userstatus_db_test.go
// @time: 2026/10/28

package server

import (
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"dbtool/internal/crypto"
)

// 假驱动：记录语句，按预置结果返回 is_active / is_superuser，不连任何数据库。
type statusDriver struct {
	mu   sync.Mutex
	logs []string
	rows [][]driver.Value
}

func (d *statusDriver) Open(string) (driver.Conn, error) { return &statusConn{d}, nil }

type statusConn struct{ d *statusDriver }

func (c *statusConn) Prepare(q string) (driver.Stmt, error) { return &statusStmt{c.d, q}, nil }
func (c *statusConn) Close() error                          { return nil }
func (c *statusConn) Begin() (driver.Tx, error)             { return nil, io.ErrUnexpectedEOF }

type statusStmt struct {
	d *statusDriver
	q string
}

func (s *statusStmt) Close() error  { return nil }
func (s *statusStmt) NumInput() int { return -1 }
func (s *statusStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, io.ErrUnexpectedEOF
}
func (s *statusStmt) Query([]driver.Value) (driver.Rows, error) {
	s.d.mu.Lock()
	s.d.logs = append(s.d.logs, strings.Join(strings.Fields(s.q), " "))
	rows := s.d.rows
	s.d.mu.Unlock()
	return &statusRows{rows: rows}, nil
}

type statusRows struct {
	rows [][]driver.Value
	i    int
}

func (r *statusRows) Columns() []string { return []string{"is_active", "is_superuser"} }
func (r *statusRows) Close() error      { return nil }
func (r *statusRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func TestGormStatusSource(t *testing.T) {
	d := &statusDriver{rows: [][]driver.Value{{int64(1), int64(0)}}}
	sql.Register("status-driver-test", d)
	sqlDB, err := sql.Open("status-driver-test", "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}),
		&gorm.Config{Logger: logger.Discard, NamingStrategy: schema.NamingStrategy{SingularTable: true}})
	if err != nil {
		t.Fatal(err)
	}
	src := gormStatusSource{db}
	st, err := src.UserStatus(5)
	if err != nil || !st.Exists || !st.Active || st.Superuser {
		t.Fatalf("启用的普通用户: %+v %v", st, err)
	}
	d.mu.Lock()
	q := d.logs[len(d.logs)-1]
	d.rows = [][]driver.Value{{int64(0), int64(1)}}
	d.mu.Unlock()
	if !strings.Contains(q, "FROM `dbt_sql_users`") || !strings.Contains(q, "is_active") || !strings.Contains(q, "is_superuser") || !strings.Contains(q, "WHERE id = ?") {
		t.Errorf("应只读 dbt_sql_users 的两列: %s", q)
	}
	if st, _ = src.UserStatus(5); !st.Exists || st.Active || !st.Superuser {
		t.Errorf("已停用的超级用户: %+v", st)
	}
	d.mu.Lock()
	d.rows = nil
	d.mu.Unlock()
	if st, err = src.UserStatus(6); err != nil || st.Exists {
		t.Errorf("不存在的用户: %+v %v", st, err)
	}
}

// 仓库提供了元数据库句柄（store.Store 的形态）而没有自己实现 UserStatusSource 时，改用 dbt_sql_users。
type dbProviderRepo struct {
	*fakeRepo
	db *gorm.DB
}

func (r dbProviderRepo) GormDB() *gorm.DB             { return r.db }
func (r dbProviderRepo) MirageCrypto() *crypto.Mirage { return nil }

func TestStatusSourceFallsBackToDBProvider(t *testing.T) {
	s := &Server{repo: dbProviderRepo{fakeRepo: &fakeRepo{}, db: &gorm.DB{}}}
	if _, ok := s.statusSource().(gormStatusSource); !ok {
		t.Error("DBProvider 应回退到 dbt_sql_users 查询")
	}
	s = &Server{repo: &fakeRepo{}}
	if s.statusSource() != nil {
		t.Error("两者都没有时不做校验")
	}
	s = &Server{repo: &statusRepo{fakeRepo: &fakeRepo{}}}
	if _, ok := s.statusSource().(*statusRepo); !ok {
		t.Error("仓库自己实现时优先用它")
	}
}
