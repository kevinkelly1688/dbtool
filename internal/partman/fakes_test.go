// @author: kevinkelly
// @license: Apache Licence
// @file: fakes_test.go
// @time: 2026/10/28

package partman

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/query"
)

// ---- 假仓库 ----

type fakeRepo struct {
	mu        sync.Mutex
	configs   []*Config
	logs      []Log
	audits    []model.AuditEntry
	nextID    uint
	failAudit bool
	lockBusy  map[string]bool // 名称 → 被别的进程占着
	locked    map[string]bool
	webhooks  []Webhook
	nextHook  uint
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{nextID: 1, lockBusy: map[string]bool{}, locked: map[string]bool{}}
}

func (r *fakeRepo) ListConfigs(f ConfigFilter) ([]Config, int64, error) {
	var out []Config
	for _, c := range r.configs {
		if !f.AllInstances {
			ok := false
			for _, id := range f.InstanceIDs {
				ok = ok || id == c.InstanceID
			}
			if !ok {
				continue
			}
		}
		if f.InstanceID != 0 && c.InstanceID != f.InstanceID {
			continue
		}
		if f.IsEnable != nil && c.IsEnable != *f.IsEnable {
			continue
		}
		if f.ColType != "" && c.ColType != f.ColType || f.Period != "" && c.Period != f.Period {
			continue
		}
		if f.Search != "" && !strings.Contains(c.DBName+c.TbName+c.Column+c.Env, f.Search) {
			continue
		}
		out = append(out, *c)
	}
	total := int64(len(out))
	if f.Limit > 0 {
		if f.Offset >= len(out) {
			return nil, total, nil
		}
		out = out[f.Offset:]
		if len(out) > f.Limit {
			out = out[:f.Limit]
		}
	}
	return out, total, nil
}

func (r *fakeRepo) GetConfig(id uint) (*Config, error) {
	for _, c := range r.configs {
		if c.ID == id {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *fakeRepo) FindByKey(inst uint, db, tb string) (*Config, error) {
	for _, c := range r.configs {
		if c.InstanceID == inst && c.DBName == db && c.TbName == tb {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *fakeRepo) InsertConfig(c *Config) error {
	c.ID = r.nextID
	r.nextID++
	max := int64(0)
	for _, x := range r.configs {
		if x.CnfID > max {
			max = x.CnfID
		}
	}
	c.CnfID = max + 1
	cp := *c
	r.configs = append(r.configs, &cp)
	return nil
}

func (r *fakeRepo) UpdateConfig(c *Config) error {
	for i, x := range r.configs {
		if x.ID == c.ID {
			cp := *c
			r.configs[i] = &cp
			return nil
		}
	}
	return errors.New("not found")
}

func (r *fakeRepo) DeleteConfig(id uint) error {
	for i, x := range r.configs {
		if x.ID == id {
			r.configs = append(r.configs[:i], r.configs[i+1:]...)
			var keep []Log
			for _, l := range r.logs {
				if l.CnfID != x.CnfID {
					keep = append(keep, l)
				}
			}
			r.logs = keep
			return nil
		}
	}
	return errors.New("not found")
}

func (r *fakeRepo) InsertLog(cnfID int64, result string, at time.Time) (uint, error) {
	id := uint(len(r.logs) + 100)
	r.logs = append(r.logs, Log{ID: id, CnfID: cnfID, ExecResult: result, CreateTime: at})
	return id, nil
}

func (r *fakeRepo) ListLogs(cnfID int64, limit int) ([]Log, error) {
	var out []Log
	for i := len(r.logs) - 1; i >= 0 && len(out) < limit; i-- {
		if r.logs[i].CnfID == cnfID {
			out = append(out, r.logs[i])
		}
	}
	return out, nil
}

func (r *fakeRepo) LastLogTimes(ids []int64) (map[int64]time.Time, error) {
	out := map[int64]time.Time{}
	for _, l := range r.logs {
		if l.CreateTime.After(out[l.CnfID]) {
			out[l.CnfID] = l.CreateTime
		}
	}
	return out, nil
}

func (r *fakeRepo) GetLog(id uint) (*Log, error) {
	for i := range r.logs {
		if r.logs[i].ID == id {
			cp := r.logs[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *fakeRepo) ListWebhooks(onlyEnabled bool) ([]Webhook, error) {
	var out []Webhook
	for _, w := range r.webhooks {
		if !onlyEnabled || w.IsEnabled {
			out = append(out, w)
		}
	}
	return out, nil
}

func (r *fakeRepo) InsertWebhook(w *Webhook) error {
	for _, x := range r.webhooks {
		if x.URL == w.URL {
			return errors.New("dup")
		}
	}
	r.nextHook++
	w.ID = r.nextHook
	r.webhooks = append(r.webhooks, *w)
	return nil
}

func (r *fakeRepo) UpdateWebhook(w *Webhook) error {
	for i := range r.webhooks {
		if r.webhooks[i].ID == w.ID {
			r.webhooks[i] = *w
			return nil
		}
	}
	return errors.New("not found")
}

func (r *fakeRepo) DeleteWebhook(id uint) error {
	for i := range r.webhooks {
		if r.webhooks[i].ID == id {
			r.webhooks = append(r.webhooks[:i], r.webhooks[i+1:]...)
			return nil
		}
	}
	return errors.New("not found")
}

func (r *fakeRepo) InsertAudit(e *model.AuditEntry) error {
	if r.failAudit {
		return errors.New("audit down")
	}
	r.audits = append(r.audits, *e)
	return nil
}

func (r *fakeRepo) TryLock(_ context.Context, name string) (func(), bool, error) {
	if r.lockBusy[name] || r.locked[name] {
		return nil, false, nil
	}
	r.locked[name] = true
	return func() { delete(r.locked, name) }, true, nil
}

func (r *fakeRepo) auditActions() []string {
	var out []string
	for _, a := range r.audits {
		out = append(out, a.Action)
	}
	return out
}

// ---- 假元数据与访问控制 ----

type fakeSource struct {
	instances []model.Instance
	cfg       map[string]string
}

func (f *fakeSource) ListInstances() ([]model.Instance, error) { return f.instances, nil }
func (f *fakeSource) EngineConn(*model.Instance) engine.Conn   { return engine.Conn{} }
func (f *fakeSource) ConfigValue(k string) (string, bool)      { v, ok := f.cfg[k]; return v, ok }

type fakeAccess struct{ allowed map[uint]map[uint]bool } // 用户 ID → 可用实例 ID

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

// ---- 假目标库：把 ADD / DROP / REORGANIZE 应用到内存里的分区列表 ----

type fakeTarget struct {
	mu        sync.Mutex
	tables    map[string]*TableState // "db.tb"
	executed  []string
	failOn    string // 语句包含它时失败
	dialErr   error
	inspectEr error
	dials     int
	minVal    *time.Time // MinTime 的返回（nil = 空表）
	minErr    error
	minCalls  int
}

// MinTime 满足 MinReader。
func (f *fakeTarget) MinTime(_ context.Context, _, _, _, _ string, _ *time.Location) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minCalls++
	if f.minErr != nil {
		return time.Time{}, false, f.minErr
	}
	if f.minVal == nil {
		return time.Time{}, false, nil
	}
	return *f.minVal, true, nil
}

func newFakeTarget() *fakeTarget { return &fakeTarget{tables: map[string]*TableState{}} }

func (f *fakeTarget) dialer() Dialer {
	return func(_ context.Context, _ *model.Instance, _ engine.Conn) (Target, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.dials++
		if f.dialErr != nil {
			return nil, f.dialErr
		}
		return f, nil
	}
}

func (f *fakeTarget) Close() error { return nil }

func (f *fakeTarget) Inspect(_ context.Context, db, table, column string) (TableState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectEr != nil {
		return TableState{}, f.inspectEr
	}
	st, ok := f.tables[db+"."+table]
	if !ok {
		return TableState{}, nil
	}
	cp := *st
	cp.Partitions = append([]RawPartition(nil), st.Partitions...)
	return cp, nil
}

var (
	reAdd   = regexp.MustCompile("^ALTER TABLE `(\\w+)`\\.`(\\w+)` ADD PARTITION \\(PARTITION `(\\w+)` VALUES LESS THAN \\((.+)\\)\\)$")
	reDrop  = regexp.MustCompile("^ALTER TABLE `(\\w+)`\\.`(\\w+)` DROP PARTITION `?(\\w+)`?$")
	reReorg = regexp.MustCompile("^ALTER TABLE `(\\w+)`\\.`(\\w+)` REORGANIZE PARTITION `(\\w+)` INTO \\((.+)\\)$")
	// SelectDB / Doris：ADD PARTITION p VALUES [("lo"), ("hi"))
	reDorisAdd = regexp.MustCompile("^ALTER TABLE `(\\w+)`\\.`(\\w+)` ADD PARTITION (\\w+) VALUES \\[\\(\"(.+)\"\\), \\(\"(.+)\"\\)\\)$")
	// 初始化：PARTITION BY RANGE [COLUMNS](`col`) (PARTITION `p` VALUES LESS THAN (…), …)
	reInit = regexp.MustCompile("^ALTER TABLE `(\\w+)`\\.`(\\w+)` PARTITION BY (RANGE(?: COLUMNS)?) ?\\((`\\w+`)\\) \\((.+)\\)$")
	rePart = regexp.MustCompile("PARTITION `(\\w+)` VALUES LESS THAN \\(([^)]+)\\)")
)

func (f *fakeTarget) Exec(_ context.Context, stmts []string) []ExecResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ExecResult, len(stmts))
	failed := false
	for i, s := range stmts {
		out[i].SQL = s
		if failed {
			out[i].Skipped = true
			continue
		}
		if f.failOn != "" && strings.Contains(s, f.failOn) {
			out[i].Err = "ERROR 1517 (HY000): Duplicate partition name"
			failed = true
			continue
		}
		f.executed = append(f.executed, s)
		if m := reInit.FindStringSubmatch(s); m != nil {
			st := f.tables[m[1]+"."+m[2]]
			st.Method, st.Expression = m[3], m[4]
			st.Partitions = nil
			for _, pm := range rePart.FindAllStringSubmatch(m[5], -1) {
				st.Partitions = append(st.Partitions, RawPartition{Name: pm[1], Desc: pm[2]})
			}
		} else if m := reDorisAdd.FindStringSubmatch(s); m != nil {
			st := f.tables[m[1]+"."+m[2]]
			st.Partitions = append(st.Partitions, RawPartition{Name: m[3], Desc: "[types: [X]; keys: [" + m[4] + "]; ..types: [X]; keys: [" + m[5] + "]; )"})
		} else if m := reAdd.FindStringSubmatch(s); m != nil {
			st := f.tables[m[1]+"."+m[2]]
			st.Partitions = append(st.Partitions, RawPartition{Name: m[3], Desc: m[4]})
		} else if m := reDrop.FindStringSubmatch(s); m != nil {
			st := f.tables[m[1]+"."+m[2]]
			var keep []RawPartition
			for _, p := range st.Partitions {
				if p.Name != m[3] {
					keep = append(keep, p)
				}
			}
			st.Partitions = keep
		} else if m := reReorg.FindStringSubmatch(s); m != nil {
			st := f.tables[m[1]+"."+m[2]]
			st.Partitions = st.Partitions[:len(st.Partitions)-1] // 去掉 pmax，按 INTO 列表重建
			for _, part := range strings.Split(m[4], "), PARTITION ") {
				part = strings.TrimPrefix(part, "PARTITION ")
				name := part[1 : strings.Index(part[1:], "`")+1]
				desc := part[strings.Index(part, "LESS THAN (")+len("LESS THAN ("):]
				desc = strings.TrimSuffix(desc, ")")
				st.Partitions = append(st.Partitions, RawPartition{Name: name, Desc: desc})
			}
		} else {
			panic(fmt.Sprintf("假目标库不认识的语句: %s", s))
		}
	}
	return out
}

func (f *fakeTarget) names(key string) string {
	var out []string
	for _, p := range f.tables[key].Partitions {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// ---- 测试环境 ----

var fixedNow = time.Date(2026, 10, 6, 12, 0, 0, 0, cst)

type env struct {
	svc    *Service
	repo   *fakeRepo
	src    *fakeSource
	access *fakeAccess
	tgt    *fakeTarget
}

func newEnv() *env {
	e := &env{repo: newFakeRepo(), tgt: newFakeTarget()}
	e.src = &fakeSource{cfg: map[string]string{}, instances: []model.Instance{
		{ID: 1, InstanceName: "prod-mysql-01", DBType: "mysql"},
		{ID: 2, InstanceName: "sit-polardb", DBType: "polardb", DeniedDBNameRegex: "^secret"},
		{ID: 3, InstanceName: "sdb-01", DBType: "selectdb"},
		{ID: 4, InstanceName: "other-mysql", DBType: "mysql"},
		{ID: 5, InstanceName: "mssql-01", DBType: "mssql"},
		{ID: 6, InstanceName: "doris-01", DBType: "doris"},
	}}
	e.access = &fakeAccess{allowed: map[uint]map[uint]bool{2: {1: true, 2: true, 3: true, 5: true, 6: true}}}
	e.svc = NewService(e.repo, e.src, e.access, e.tgt.dialer())
	e.svc.now = func() time.Time { return fixedNow }
	return e
}

var (
	root = Caller{Principal: query.Principal{ID: 1, Username: "root", Display: "管理员", IsSuperuser: true}}
)

// user 返回普通用户（ID 2），perms 是它拥有的权限码。
func user(perms ...string) Caller {
	set := map[string]bool{}
	for _, p := range perms {
		set[p] = true
	}
	return Caller{Principal: query.Principal{ID: 2, Username: "bob", Display: "鲍勃"}, Can: func(c string) bool { return set[c] }}
}

func dailyTable(boundsUntil ...string) *TableState {
	st := &TableState{Exists: true, IsBase: true, ColumnType: "datetime", ColumnTypeFull: "datetime", Method: "RANGE COLUMNS", Expression: "`created_at`"}
	for _, b := range boundsUntil { // b 形如 2026-10-07：分区名按“起始日”命名
		t, _ := time.Parse("2006-01-02", b)
		st.Partitions = append(st.Partitions, RawPartition{Name: "p" + t.AddDate(0, 0, -1).Format("20060102"), Desc: "'" + b + " 00:00:00'"})
	}
	return st
}

func goodInput() ConfigInput {
	return ConfigInput{InstanceID: 1, DBName: "log", TbName: "access_log", Column: "created_at", ColType: ColDatetime, Period: "day",
		Interval: 1, Retain: 30, PreCreate: 3, Timezone: "Asia/Shanghai", IsEnable: true, Env: "线上日志"}
}

// addConfig 直接放进仓库（绕过校验），返回配置。
func (e *env) addConfig(in ConfigInput) *Config {
	c := Config{InstanceID: in.InstanceID, DBName: in.DBName, TbName: in.TbName, Column: in.Column, ColType: in.ColType, Period: in.Period,
		Interval: in.Interval, Retain: in.Retain, PreCreate: in.PreCreate, Timezone: in.Timezone, IsEnable: in.IsEnable, Env: in.Env,
		CreateTime: fixedNow, LastestTime: fixedNow}
	_ = e.repo.InsertConfig(&c)
	return &c
}
