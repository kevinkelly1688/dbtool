// @author: kevinkelly
// @license: Apache Licence
// @file: fake_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"dbtool/internal/model"
)

// ---- 假目标库 ----

type call struct {
	sql  string
	args []any
	exec bool
}

type handler struct {
	match string
	fn    func(sql string, args []any) (*Result, error)
}

// fakeTarget 按 SQL 子串匹配返回预设结果，并记录所有调用。
type fakeTarget struct {
	mu       sync.Mutex
	calls    []call
	handlers []handler
	execErr  map[string]error // SQL 子串 -> Exec 返回的错误
	closed   bool
}

func newTarget() *fakeTarget { return &fakeTarget{execErr: map[string]error{}} }

// on 注册固定结果；先注册的先匹配。
func (t *fakeTarget) on(match string, res *Result) *fakeTarget {
	t.handlers = append(t.handlers, handler{match, func(string, []any) (*Result, error) { return res, nil }})
	return t
}

func (t *fakeTarget) onErr(match string, err error) *fakeTarget {
	t.handlers = append(t.handlers, handler{match, func(string, []any) (*Result, error) { return nil, err }})
	return t
}

func (t *fakeTarget) onFunc(match string, fn func(sql string, args []any) (*Result, error)) *fakeTarget {
	t.handlers = append(t.handlers, handler{match, fn})
	return t
}

func (t *fakeTarget) Query(_ context.Context, sql string, args ...any) (*Result, error) {
	t.mu.Lock()
	t.calls = append(t.calls, call{sql: sql, args: args})
	hs := append([]handler(nil), t.handlers...)
	t.mu.Unlock()
	for _, h := range hs {
		if strings.Contains(sql, h.match) {
			return h.fn(sql, args)
		}
	}
	return &Result{}, nil
}

func (t *fakeTarget) Exec(_ context.Context, sql string, args ...any) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, call{sql: sql, args: args, exec: true})
	for k, err := range t.execErr {
		if strings.Contains(sql, k) {
			return 0, err
		}
	}
	return 1, nil
}

func (t *fakeTarget) Close() error { t.closed = true; return nil }

// execs 返回所有 Exec 调用。
func (t *fakeTarget) execs() []call {
	var out []call
	for _, c := range t.calls {
		if c.exec {
			out = append(out, c)
		}
	}
	return out
}

// queries 返回包含子串的 Query 调用。
func (t *fakeTarget) queries(sub string) []call {
	var out []call
	for _, c := range t.calls {
		if !c.exec && strings.Contains(c.sql, sub) {
			out = append(out, c)
		}
	}
	return out
}

func rs(cols []string, rows ...[]any) *Result { return &Result{Columns: cols, Rows: rows} }

// ---- 假拨号器 ----

type fakeDialer struct {
	target   *fakeTarget
	dialErr  error
	failHost map[string]error // 只对某个 HOST 的连接失败
	probeVer string
	probeErr error
	dialed   []ConnInfo
	probed   []ConnInfo
}

func (d *fakeDialer) Dial(_ context.Context, c ConnInfo) (Target, error) {
	d.dialed = append(d.dialed, c)
	if err, ok := d.failHost[c.Host]; ok {
		return nil, err
	}
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	return d.target, nil
}

func (d *fakeDialer) Probe(_ context.Context, c ConnInfo) (string, error) {
	d.probed = append(d.probed, c)
	return d.probeVer, d.probeErr
}

// ---- 假仓库 ----

type fakeRepo struct {
	instances map[uint]*model.Instance
	scope     map[uint]map[uint]bool // 用户 -> 可见实例
	groups    []GroupOption
	tags      []TagOption
	tunnels   []TunnelOption
	tunnel    *TunnelInfo
	users     map[string]*model.User
	dbs       map[uint]map[string]DatabaseRecord
	accts     map[uint]map[string]AccountRecord
	acctPw    map[string]string // "实例id|user@host" -> 登记的密码
	tpls      []ParamTemplate
	hist      []ParamHistory
	audits    []model.AuditEntry
	refs      map[uint][]Ref
	writes    []InstanceWrite
	deleted   []uint
	nextID    uint
	failAudit bool
	saveErr   error // SaveInstance 返回的错误（模拟唯一索引冲突）
	failHist  bool
}

func newRepo() *fakeRepo {
	r := &fakeRepo{
		instances: map[uint]*model.Instance{}, scope: map[uint]map[uint]bool{}, users: map[string]*model.User{},
		dbs: map[uint]map[string]DatabaseRecord{}, accts: map[uint]map[string]AccountRecord{}, acctPw: map[string]string{},
		refs: map[uint][]Ref{}, nextID: 100,
		groups:  []GroupOption{{1, "研发组"}, {2, "财务组"}},
		tags:    []TagOption{{1, "can_read", "支持查询", true}, {2, "can_write", "支持上线", true}, {3, "old_tag", "旧标签", false}},
		tunnels: []TunnelOption{{1, "jump-prod"}},
	}
	r.users["dev"] = &model.User{ID: 5, Username: "dev", Display: "演示开发者", IsActive: true}
	r.users["wang"] = &model.User{ID: 6, Username: "wang", Display: "王工", IsActive: true}
	return r
}

func (r *fakeRepo) addInstance(id uint, name, dbType string) *model.Instance {
	in := &model.Instance{ID: id, InstanceName: name, Type: "master", DBType: dbType, Host: "10.0.0.1", Port: 3306, User: "dba_ro", Password: "secret-pw"}
	r.instances[id] = in
	return in
}

func (r *fakeRepo) ListInstances(f InstanceFilter, scope map[uint]bool) ([]model.Instance, int64, error) {
	var ids []int
	for id := range r.instances {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	var all []model.Instance
	for _, id := range ids {
		in := *r.instances[uint(id)]
		if scope != nil && !scope[in.ID] {
			continue
		}
		if f.Role != "" && in.Type != f.Role {
			continue
		}
		if f.DBType != "" && in.DBType != f.DBType {
			continue
		}
		if f.Enabled != nil && in.IsEnable != *f.Enabled {
			continue
		}
		if f.Q != "" && !strings.Contains(in.InstanceName, f.Q) && !strings.Contains(in.Host, f.Q) {
			continue
		}
		if f.Tag != "" {
			ok := false
			for _, t := range in.Tags {
				ok = ok || (t.TagCode == f.Tag && t.Active)
			}
			if !ok {
				continue
			}
		}
		all = append(all, in)
	}
	total := int64(len(all))
	if f.Offset >= len(all) {
		return nil, total, nil
	}
	end := f.Offset + f.Limit
	if end > len(all) {
		end = len(all)
	}
	return all[f.Offset:end], total, nil
}

func (r *fakeRepo) GetInstance(id uint) (*model.Instance, error) {
	if in, ok := r.instances[id]; ok {
		cp := *in
		return &cp, nil
	}
	return nil, nil
}

func (r *fakeRepo) BriefInstances(scope map[uint]bool) ([]model.Instance, error) {
	var out []model.Instance
	for _, in := range r.instances {
		if scope == nil || scope[in.ID] {
			out = append(out, *in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceName < out[j].InstanceName })
	return out, nil
}

func (r *fakeRepo) InstanceOptions() (*InstanceOptions, error) {
	return &InstanceOptions{Groups: r.groups, Tags: r.tags, Tunnels: r.tunnels}, nil
}

func (r *fakeRepo) TunnelInfo(id uint) (*TunnelInfo, error) {
	if id == 1 {
		return r.tunnel, nil
	}
	return nil, nil
}

func (r *fakeRepo) InstanceNameTaken(name string, except uint) (bool, error) {
	for _, in := range r.instances {
		if in.InstanceName == name && in.ID != except {
			return true, nil
		}
	}
	return false, nil
}

func (r *fakeRepo) SaveInstance(id uint, in InstanceWrite) (uint, error) {
	r.writes = append(r.writes, in)
	if r.saveErr != nil {
		return 0, r.saveErr
	}
	if id == 0 {
		r.nextID++
		id = r.nextID
		r.instances[id] = &model.Instance{ID: id}
	}
	cur := r.instances[id]
	cur.InstanceName, cur.Type, cur.DBType, cur.Host, cur.Port, cur.User = in.Name, in.Role, in.DBType, in.Host, in.Port, in.User
	cur.IsEnable = in.IsEnable
	if in.Password != nil {
		cur.Password = *in.Password
	}
	return id, nil
}

func (r *fakeRepo) SetInstanceEnable(id uint, enable bool) error {
	r.instances[id].IsEnable = enable
	return nil
}

func (r *fakeRepo) InstanceRefs(id uint) ([]Ref, error) { return r.refs[id], nil }

func (r *fakeRepo) DeleteInstance(id uint) error {
	r.deleted = append(r.deleted, id)
	delete(r.instances, id)
	return nil
}

func (r *fakeRepo) ConnInfo(inst *model.Instance) (ConnInfo, error) {
	ci := ConnInfo{DBType: inst.DBType, Host: inst.Host, Port: inst.Port, User: inst.User, Password: inst.Password}
	return ci, nil
}

func (r *fakeRepo) Decrypt(s string) string { return s }

func (r *fakeRepo) UserInstanceIDs(uid uint) (map[uint]bool, error) { return r.scope[uid], nil }

func (r *fakeRepo) SavedDatabases(id uint) ([]DatabaseRecord, error) {
	var out []DatabaseRecord
	for _, d := range r.dbs[id] {
		out = append(out, d)
	}
	return out, nil
}

func (r *fakeRepo) SaveDatabaseRecord(id uint, rec DatabaseRecord) error {
	if r.dbs[id] == nil {
		r.dbs[id] = map[string]DatabaseRecord{}
	}
	r.dbs[id][rec.DBName] = rec
	return nil
}

func (r *fakeRepo) ActiveUser(name string) (*model.User, error) {
	if u, ok := r.users[name]; ok && u.IsActive {
		return u, nil
	}
	return nil, nil
}

func (r *fakeRepo) ActiveUsers() ([]model.User, error) {
	var out []model.User
	for _, u := range r.users {
		if u.IsActive {
			out = append(out, *u)
		}
	}
	return out, nil
}

func (r *fakeRepo) SavedAccounts(id uint) ([]AccountRecord, error) {
	var out []AccountRecord
	for _, a := range r.accts[id] {
		out = append(out, a)
	}
	return out, nil
}

func (r *fakeRepo) SaveAccountRecord(id uint, rec AccountRecord) error {
	if r.accts[id] == nil {
		r.accts[id] = map[string]AccountRecord{}
	}
	k := rec.User + "@" + rec.Host
	old, had := r.accts[id][k]
	if had && rec.KeepRemark {
		rec.Remark = old.Remark
	}
	if rec.Password != nil {
		r.acctPw[fmt.Sprintf("%d|%s", id, k)] = *rec.Password
	}
	rec.Password = nil
	r.accts[id][k] = rec
	return nil
}

func (r *fakeRepo) DeleteAccountRecord(id uint, user, host string) error {
	delete(r.accts[id], user+"@"+host)
	return nil
}

func (r *fakeRepo) ListTemplates(dbType string) ([]ParamTemplate, error) {
	var out []ParamTemplate
	for _, t := range r.tpls {
		if t.DBType == dbType {
			out = append(out, t)
		}
	}
	return out, nil
}

func (r *fakeRepo) GetTemplate(dbType, name string) (*ParamTemplate, error) {
	for i := range r.tpls {
		if r.tpls[i].DBType == dbType && strings.EqualFold(r.tpls[i].VariableName, name) {
			cp := r.tpls[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *fakeRepo) GetTemplateByID(id uint) (*ParamTemplate, error) {
	for i := range r.tpls {
		if r.tpls[i].ID == id {
			cp := r.tpls[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *fakeRepo) SaveTemplate(t *ParamTemplate) error {
	if t.ID == 0 {
		t.ID = uint(len(r.tpls) + 1)
		r.tpls = append(r.tpls, *t)
		return nil
	}
	for i := range r.tpls {
		if r.tpls[i].ID == t.ID {
			r.tpls[i] = *t
		}
	}
	return nil
}

func (r *fakeRepo) InsertParamHistory(h *ParamHistory) error {
	if r.failHist {
		return errors.New("history table broken")
	}
	r.hist = append(r.hist, *h)
	return nil
}

func (r *fakeRepo) ListParamHistory(id uint, q string, offset, limit int) ([]ParamHistoryRow, int64, error) {
	var out []ParamHistoryRow
	for _, h := range r.hist {
		if h.InstanceID == id && (q == "" || strings.Contains(h.VariableName, q)) {
			out = append(out, ParamHistoryRow{ParamHistory: h, InstanceName: r.instances[id].InstanceName})
		}
	}
	total := int64(len(out))
	if offset >= len(out) {
		return nil, total, nil
	}
	end := offset + limit
	if end > len(out) {
		end = len(out)
	}
	return out[offset:end], total, nil
}

func (r *fakeRepo) InsertAudit(e *model.AuditEntry) error {
	if r.failAudit {
		return errors.New("audit table broken")
	}
	r.audits = append(r.audits, *e)
	return nil
}

// lastAudit 返回最后一条审计。
func (r *fakeRepo) lastAudit() model.AuditEntry {
	if len(r.audits) == 0 {
		return model.AuditEntry{}
	}
	return r.audits[len(r.audits)-1]
}

// ---- 测试公共场景 ----

var (
	root = Caller{ID: 1, Username: "root", Display: "管理员", IsSuperuser: true, AllInstances: true}
	bob  = Caller{ID: 2, Username: "bob", Display: "鲍勃"}
)

// env 是一个常用场景：实例 1 mysql（bob 可见）、2 mysql（bob 不可见）、3 mssql、4 doris、5 polardb。
type env struct {
	svc    *Service
	repo   *fakeRepo
	target *fakeTarget
	dialer *fakeDialer
}

func newEnv() *env {
	r := newRepo()
	r.addInstance(1, "prod-mysql-01", "mysql")
	r.addInstance(2, "prod-mysql-02", "mysql")
	r.addInstance(3, "mssql-01", "mssql")
	r.addInstance(4, "report-doris", "doris")
	r.addInstance(5, "polar-01", "polardb")
	r.scope[2] = map[uint]bool{1: true, 3: true, 4: true}
	t := newTarget()
	d := &fakeDialer{target: t, probeVer: "SQL Server 2019"}
	s := NewService(r, d)
	s.now = func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }
	return &env{svc: s, repo: r, target: t, dialer: d}
}

// withVersion 让目标库的 SELECT VERSION() 返回指定版本。
func (e *env) withVersion(v string) *env {
	e.target.on("SELECT VERSION()", rs([]string{"v"}, []any{v}))
	return e
}
