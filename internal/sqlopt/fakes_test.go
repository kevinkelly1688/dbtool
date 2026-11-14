// @author: kevinkelly
// @license: Apache Licence
// @file: fakes_test.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/audit"
	"dbtool/internal/btnperm"
	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// ---- 假仓库 ----

type fakeRepo struct {
	instances []model.Instance
	config    map[string]string
	perms     map[uint]map[string]bool
	groupInst map[uint]map[uint]bool // 用户 → 实例（资源组关联）
}

func (r *fakeRepo) ListInstances() ([]model.Instance, error) { return r.instances, nil }
func (r *fakeRepo) GetInstance(id uint) (*model.Instance, error) {
	for i := range r.instances {
		if r.instances[i].ID == id {
			return &r.instances[i], nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}
func (r *fakeRepo) EngineConn(inst *model.Instance) engine.Conn {
	return engine.Conn{Host: inst.Host, Port: inst.Port, User: "dbuser", Password: "s3cretPW"}
}
func (r *fakeRepo) ConfigValue(k string) (string, bool) { v, ok := r.config[k]; return v, ok }
func (r *fakeRepo) HasButton(uid uint, code string) (bool, error) {
	return r.perms[uid][code] || btnperm.IsOpen(code), nil
}
func (r *fakeRepo) UserInstanceIDs(uid uint) (map[uint]bool, error) { return r.groupInst[uid], nil }

// ---- 假目标库 ----

type fakeTable struct {
	cols    [][3]string // 列名、data_type、column_type
	indexes [][4]string // 索引名、non_unique、seq、列名
	rows    string
}

type fakeDB struct {
	mu      sync.Mutex
	dbs     []string
	tables  map[string]fakeTable // 表名（小写）
	plan    *Table
	errs    map[string]error // 查询前缀 → 错误
	log     []string
	opened  int
	closed  int
	openErr error
	version string
	drained []string
	delay   time.Duration // 每次 Query 的耗时（测超时/取消）
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		dbs:     []string{"shop", "report", "secret", "information_schema", "mysql"},
		tables:  map[string]fakeTable{},
		errs:    map[string]error{},
		version: "8.0.36",
		plan: &Table{Columns: []string{"id", "select_type", "table", "type", "possible_keys", "key", "rows", "filtered", "Extra"},
			Rows: [][]any{{"1", "SIMPLE", "orders", "ALL", "idx_user", nil, "8214990", "10.00", "Using where; Using filesort"}}},
	}
}

func (f *fakeDB) Open(ctx context.Context, c engine.Conn, db string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.opened++
	return &fakeSession{db: f}, nil
}

func (f *fakeDB) queries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.log...)
}

type fakeSession struct{ db *fakeDB }

func (s *fakeSession) Close() error {
	s.db.mu.Lock()
	s.db.closed++
	s.db.mu.Unlock()
	return nil
}

func (s *fakeSession) Drain(ctx context.Context, q string) (int64, error) {
	s.db.mu.Lock()
	s.db.drained = append(s.db.drained, q)
	s.db.mu.Unlock()
	return 3, nil
}

func tbl(cols []string, rows ...[]any) *Table { return &Table{Columns: cols, Rows: rows} }

func (s *fakeSession) Query(ctx context.Context, q string, args ...any) (*Table, error) {
	f := s.db
	f.mu.Lock()
	f.log = append(f.log, q)
	delay := f.delay
	for p, err := range f.errs {
		if strings.HasPrefix(q, p) {
			f.mu.Unlock()
			return nil, err
		}
	}
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	str := func(i int) string {
		if i < len(args) {
			if s, ok := args[i].(string); ok {
				return strings.ToLower(s)
			}
		}
		return ""
	}
	switch {
	case q == "SHOW DATABASES":
		t := &Table{Columns: []string{"Database"}}
		for _, d := range f.dbs {
			t.Rows = append(t.Rows, []any{d})
		}
		return t, nil
	case strings.HasPrefix(q, "USE "):
		return tbl(nil), nil
	case strings.HasPrefix(q, "EXPLAIN "):
		return f.plan, nil
	case strings.Contains(q, "information_schema.columns"):
		t := &Table{Columns: []string{"column_name", "data_type", "column_type"}}
		for _, c := range f.tables[str(1)].cols {
			t.Rows = append(t.Rows, []any{c[0], c[1], c[2]})
		}
		return t, nil
	case strings.Contains(q, "information_schema.statistics") && strings.Contains(q, "index_name, non_unique"):
		t := &Table{Columns: []string{"index_name", "non_unique", "seq_in_index", "column_name"}}
		for _, ix := range f.tables[str(1)].indexes {
			t.Rows = append(t.Rows, []any{ix[0], ix[1], ix[2], ix[3]})
		}
		return t, nil
	case strings.Contains(q, "information_schema.tables") && strings.Contains(q, "IFNULL(table_rows"):
		return tbl([]string{"table_rows"}, []any{f.tables[str(1)].rows}), nil
	case strings.Contains(q, "information_schema.tables"), strings.Contains(q, "information_schema.statistics"):
		return tbl([]string{"table_name"}, []any{str(1)}), nil
	case strings.HasPrefix(q, "SELECT VERSION()"):
		return tbl([]string{"version"}, []any{f.version}), nil
	case q == "SHOW WARNINGS":
		return tbl([]string{"Level", "Code", "Message"}, []any{"Note", "1003", "/* select#1 */ select 1"}), nil
	case strings.HasPrefix(q, "SHOW CREATE TABLE"):
		return tbl([]string{"Table", "Create Table"}, []any{"orders", "CREATE TABLE `orders` (...)"}), nil
	case strings.HasPrefix(q, "SHOW GLOBAL VARIABLES"):
		return tbl([]string{"Variable_name", "Value"}, []any{"sort_buffer_size", "262144"}), nil
	case q == "SHOW SESSION STATUS":
		return tbl([]string{"Variable_name", "Value"}, []any{"Handler_read_rnd", "10"}, []any{"Threads_connected", "3"}), nil
	case q == "SHOW PROFILES":
		return tbl([]string{"Query_ID", "Duration", "Query"}, []any{"7", "0.1", "select 1"}), nil
	case strings.Contains(q, "information_schema.profiling"):
		return tbl([]string{"state", "duration"}, []any{"executing", "0.01"}), nil
	}
	return tbl(nil), nil
}

// ---- 假外部程序 ----

type fakeRunner struct {
	mu    sync.Mutex
	calls []ExecSpec
	res   *ExecResult
	err   error
	block chan struct{} // 非 nil 时 Run 会一直阻塞到被关闭或 ctx 结束
}

func (r *fakeRunner) Run(ctx context.Context, spec ExecSpec) (*ExecResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, spec)
	res, err, block := r.res, r.err, r.block
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if res == nil {
		return &ExecResult{Stdout: "ok"}, nil
	}
	c := *res
	return &c, nil
}

func (r *fakeRunner) last() ExecSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

func (r *fakeRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// ---- 假审计 ----

type fakeAudit struct {
	mu      sync.Mutex
	entries []model.AuditEntry
}

func (a *fakeAudit) Record(e *model.AuditEntry) error {
	a.mu.Lock()
	a.entries = append(a.entries, *e)
	a.mu.Unlock()
	return nil
}

func (a *fakeAudit) all() []model.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]model.AuditEntry{}, a.entries...)
}

// ---- 场景 ----

var (
	root  = audit.Actor{ID: 1, Username: "root", Display: "管理员", IsSuperuser: true}
	alice = audit.Actor{ID: 2, Username: "alice", Display: "爱丽丝"}
	bob   = audit.Actor{ID: 3, Username: "bob", Display: "鲍勃"}
)

// testToolPath 是一个必然存在的可执行文件（测试二进制自己），用来通过“程序存在”的检查。
var testToolPath = func() string {
	p, _ := testExecutable()
	return p
}()

type env struct {
	svc    *Service
	repo   *fakeRepo
	db     *fakeDB
	runner *fakeRunner
	audit  *fakeAudit
}

// newEnv：实例 1 prod-mysql（mysql，alice 可见）、2 other-mysql（mysql，alice 不可见）、3 report-doris（doris）、4 ora（oracle）。
func newEnv(t *testing.T) *env {
	t.Helper()
	repo := &fakeRepo{
		instances: []model.Instance{
			{ID: 1, InstanceName: "prod-mysql", DBType: "mysql", Host: "10.0.0.1", Port: 3306, Type: "master"},
			{ID: 2, InstanceName: "other-mysql", DBType: "mysql", Host: "10.0.0.2", Port: 3306},
			{ID: 3, InstanceName: "report-doris", DBType: "doris", Host: "10.0.0.3", Port: 9030},
			{ID: 4, InstanceName: "ora", DBType: "oracle", Host: "10.0.0.4", Port: 1521},
		},
		config:    map[string]string{},
		perms:     map[uint]map[string]bool{},
		groupInst: map[uint]map[uint]bool{alice.ID: {1: true, 3: true, 4: true}, bob.ID: {}},
	}
	e := &env{repo: repo, db: newFakeDB(), runner: &fakeRunner{}, audit: &fakeAudit{}}
	e.svc = NewService(repo, e.db, e.runner, e.audit)
	return e
}

func (e *env) configureTools() {
	e.repo.config[settings.KeyOptimizerSoarPath] = testToolPath
	e.repo.config[settings.KeyOptimizerSoarTestDSN] = "tester:tpass1234@10.9.9.9:3306/test"
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var be *Error
	if !errors.As(err, &be) {
		t.Fatalf("期望业务错误，得到 %v", err)
	}
	return be.Code
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误 %s，实际成功", code)
	}
	if got := codeOf(t, err); got != code {
		t.Fatalf("错误码 = %s（%v），期望 %s", got, err, code)
	}
}

// waitJob 轮询到任务结束。
func waitJob(t *testing.T, s *Service, actor audit.Actor, id string) *JobView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v, err := s.Job(actor, id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != StatusQueued && v.Status != StatusRunning {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("任务超时未结束")
	return nil
}

func engineConn() engine.Conn { return engine.Conn{Host: "h", Port: 3306} }
