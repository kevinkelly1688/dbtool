// @author: kevinkelly
// @license: Apache Licence
// @file: fakes_test.go
// @time: 2026/10/28

package dict

import (
	"context"
	"strings"
	"sync"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/query"
)

// ---- 假查询连接：按“语句里包含的片段”返回预设结果，并记录每次调用 ----

type rule struct {
	contains string
	rows     *Rows
	err      error
}

type call struct {
	sql  string
	args []any
}

type fakeQ struct {
	mu     sync.Mutex
	rules  []rule
	calls  []call
	closed bool
}

func (f *fakeQ) Query(_ context.Context, q string, args ...any) (*Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{q, args})
	for _, r := range f.rules {
		if strings.Contains(q, r.contains) {
			if r.err != nil {
				return nil, r.err
			}
			if r.rows == nil {
				return &Rows{}, nil
			}
			return r.rows, nil
		}
	}
	return &Rows{}, nil
}

func (f *fakeQ) Close() error { f.closed = true; return nil }

func (f *fakeQ) on(contains string, data ...[]any) *fakeQ {
	f.rules = append(f.rules, rule{contains: contains, rows: &Rows{Data: data}})
	return f
}

func (f *fakeQ) fail(contains string, err error) *fakeQ {
	f.rules = append(f.rules, rule{contains: contains, err: err})
	return f
}

func (f *fakeQ) sqls() string {
	var b strings.Builder
	for _, c := range f.calls {
		b.WriteString(c.sql + "\n")
	}
	return b.String()
}

func row(v ...any) []any { return v }

// ---- 元数据、访问控制、审计 ----

type fakeSource struct{ instances []model.Instance }

func (f *fakeSource) ListInstances() ([]model.Instance, error) { return f.instances, nil }
func (f *fakeSource) EngineConn(*model.Instance) engine.Conn {
	return engine.Conn{Host: "h", Port: 3306, User: "u", Password: "s3cret"}
}

type fakeAccess struct{ allowed map[uint]map[uint]bool }

func (a *fakeAccess) FilterInstances(p query.Principal, list []model.Instance) ([]model.Instance, error) {
	if p.IsSuperuser {
		return list, nil
	}
	var out []model.Instance
	for _, i := range list {
		if a.allowed[p.ID][i.ID] {
			out = append(out, i)
		}
	}
	return out, nil
}

func (a *fakeAccess) CanUseInstance(p query.Principal, inst *model.Instance) error {
	if p.IsSuperuser || a.allowed[p.ID][inst.ID] {
		return nil
	}
	return &query.Error{Code: "no_privilege", Message: "你所在组未关联该实例", HTTP: 403}
}

// ---- 测试环境 ----

type opened struct {
	dbType, db string
	conn       engine.Conn
}

type env struct {
	svc    *Service
	src    *fakeSource
	access *fakeAccess
	q      *fakeQ            // 默认连接
	qs     map[string]*fakeQ // 按库名定制的连接（整实例导出时每库一个）
	opens  []opened
	openEr error
}

// rvTag 是旧标签 read_view：数据字典不再据此限制对象类别、视图名或视图定义，保留它用来验证“带旧标签也不受限”。
func rvTag() []model.InstanceTag {
	return []model.InstanceTag{{TagCode: "read_view", TagName: "只读视图", Active: true}}
}

func newEnv() *env {
	e := &env{q: &fakeQ{}, qs: map[string]*fakeQ{}}
	e.src = &fakeSource{instances: []model.Instance{
		{ID: 1, InstanceName: "prod-mysql-01", DBType: "mysql"},
		{ID: 2, InstanceName: "mssql-01", DBType: "mssql"},
		{ID: 3, InstanceName: "cass-01", DBType: "cassandra"},
		{ID: 4, InstanceName: "rv-mysql", DBType: "mysql", Tags: rvTag()},
		{ID: 5, InstanceName: "filtered", DBType: "mysql", ShowDBNameRegex: "^app_", DeniedDBNameRegex: "^app_secret"},
		{ID: 6, InstanceName: "other-mysql", DBType: "mysql"},
		{ID: 7, InstanceName: "doris-01", DBType: "doris"},
		{ID: 8, InstanceName: "rv-mssql", DBType: "mssql", Tags: rvTag()},
	}}
	e.access = &fakeAccess{allowed: map[uint]map[uint]bool{2: {1: true, 2: true, 3: true, 4: true, 5: true, 7: true, 8: true}}}
	open := func(_ context.Context, dbType string, c engine.Conn, db string) (Querier, error) {
		e.opens = append(e.opens, opened{dbType, db, c})
		if e.openEr != nil {
			return nil, e.openEr
		}
		if q, ok := e.qs[db]; ok {
			return q, nil
		}
		return e.q, nil
	}
	e.svc = NewService(e.src, e.access, open)
	return e
}

var (
	root = query.Principal{ID: 1, Username: "root", Display: "管理员", IsSuperuser: true}
	bob  = query.Principal{ID: 2, Username: "bob", Display: "鲍勃"}
)
