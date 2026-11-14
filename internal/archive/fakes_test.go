// @author: kevinkelly
// @license: Apache Licence
// @file: fakes_test.go
// @time: 2026/10/28

package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/archive/engine/enginetest"
	"dbtool/internal/audit"
	ieng "dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/query"
)

// ---- 内存仓库 ----

type memRepo struct {
	mu       sync.Mutex
	notReady bool

	tasks   map[uint]*Task
	runs    map[uint]*Run
	events  map[uint][]Event
	stats   map[uint][]BatchStat
	stores  map[uint]*StoreRow
	policy  *PolicyRow
	checks  map[string]*CheckRow
	nextID  uint
	openWF  []OpenWorkflow
	audits  []*model.AuditEntry
	locks   map[string]bool
	failLog error

	// 审批与元数据
	settings map[[2]int]*model.WorkflowAuditSetting
	groups   map[uint]*model.AuthGroup
	members  map[uint]map[uint]bool // userID -> authGroupID
	userRGs  map[uint][]uint
	instRGs  map[uint][]uint
	rgs      map[uint]*model.ResourceGroup
	users    map[uint]model.User
	perms    map[uint]map[string]bool
	cfg      map[string]string
	wfAudits []*model.WorkflowAudit
	details  []*model.WorkflowAuditDetail
	wfLogs   []*model.WorkflowLog
}

func newMemRepo() *memRepo {
	return &memRepo{tasks: map[uint]*Task{}, runs: map[uint]*Run{}, events: map[uint][]Event{}, stats: map[uint][]BatchStat{}, stores: map[uint]*StoreRow{},
		checks: map[string]*CheckRow{}, locks: map[string]bool{}, settings: map[[2]int]*model.WorkflowAuditSetting{}, groups: map[uint]*model.AuthGroup{},
		members: map[uint]map[uint]bool{}, userRGs: map[uint][]uint{}, instRGs: map[uint][]uint{}, rgs: map[uint]*model.ResourceGroup{},
		users: map[uint]model.User{}, perms: map[uint]map[string]bool{}, cfg: map[string]string{}}
}

var _ Repo = (*memRepo)(nil)

func (r *memRepo) id() uint { r.nextID++; return r.nextID }

func (r *memRepo) Ready() bool { return !r.notReady }

func (r *memRepo) InTx(fn func(Repo) error) error {
	r.mu.Lock()
	tasks := map[uint]Task{}
	for k, v := range r.tasks {
		tasks[k] = *v
	}
	audits := make([]model.WorkflowAudit, len(r.wfAudits))
	for i, a := range r.wfAudits {
		audits[i] = *a
	}
	nd, nl := len(r.details), len(r.wfLogs)
	r.mu.Unlock()
	if err := fn(r); err != nil {
		r.mu.Lock()
		for k := range r.tasks {
			if old, ok := tasks[k]; ok {
				*r.tasks[k] = old
			} else {
				delete(r.tasks, k)
			}
		}
		for i := range r.wfAudits {
			if i < len(audits) {
				*r.wfAudits[i] = audits[i]
			}
		}
		if len(r.wfAudits) > len(audits) {
			r.wfAudits = r.wfAudits[:len(audits)]
		}
		r.details, r.wfLogs = r.details[:nd], r.wfLogs[:nl]
		r.mu.Unlock()
		return err
	}
	return nil
}

// ---- audit.Repo ----

func (r *memRepo) AuditSetting(g uint, t int) (*model.WorkflowAuditSetting, error) {
	return r.settings[[2]int{int(g), t}], nil
}
func (r *memRepo) AuthGroup(id uint) (*model.AuthGroup, error) { return r.groups[id], nil }
func (r *memRepo) UserInAuthGroup(u, g uint) (bool, error)     { return r.members[u][g], nil }
func (r *memRepo) AuthGroupUsers(names []string, rg uint) ([]model.User, error) {
	var out []model.User
	for uid, gs := range r.members {
		in := false
		for _, id := range r.userRGs[uid] {
			in = in || id == rg
		}
		if !in {
			continue
		}
		for gid := range gs {
			for _, n := range names {
				if g := r.groups[gid]; g != nil && g.Name == n {
					out = append(out, r.users[uid])
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (r *memRepo) InsertAudit(a *model.WorkflowAudit) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a.AuditID = r.id()
	r.wfAudits = append(r.wfAudits, a)
	return nil
}
func (r *memRepo) SaveAudit(a *model.WorkflowAudit) error { return nil }
func (r *memRepo) InsertDetail(d *model.WorkflowAuditDetail) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d.AuditDetailID = r.id()
	r.details = append(r.details, d)
	return nil
}
func (r *memRepo) InsertLog(l *model.WorkflowLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wfLogs = append(r.wfLogs, l)
	return nil
}
func (r *memRepo) HasButton(u uint, code string) (bool, error) { return r.perms[u][code], nil }
func (r *memRepo) ConfigValue(k string) (string, bool)         { v, ok := r.cfg[k]; return v, ok }

func (r *memRepo) UserResourceGroups(uid uint, all bool) ([]model.ResourceGroup, error) {
	var out []model.ResourceGroup
	if all {
		for _, g := range r.rgs {
			out = append(out, *g)
		}
	} else {
		for _, id := range r.userRGs[uid] {
			if g := r.rgs[id]; g != nil {
				out = append(out, *g)
			}
		}
	}
	return out, nil
}
func (r *memRepo) ResourceGroupByID(id uint) (*model.ResourceGroup, error) { return r.rgs[id], nil }
func (r *memRepo) InstanceResourceGroupIDs(inst uint) ([]uint, error)      { return r.instRGs[inst], nil }
func (r *memRepo) AuditByWorkflow(t int, id uint, _ bool) (*model.WorkflowAudit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.wfAudits {
		if a.WorkflowType == t && a.WorkflowID == id {
			return a, nil
		}
	}
	return nil, nil
}
func (r *memRepo) AuditLogs(auditID uint) ([]model.WorkflowLog, error) {
	var out []model.WorkflowLog
	for _, l := range r.wfLogs {
		if l.AuditID == auditID {
			out = append(out, *l)
		}
	}
	return out, nil
}
func (r *memRepo) AuditDetails(auditID uint) ([]model.WorkflowAuditDetail, error) {
	var out []model.WorkflowAuditDetail
	for _, d := range r.details {
		if d.AuditID == auditID {
			out = append(out, *d)
		}
	}
	return out, nil
}
func (r *memRepo) OpenWorkflows(uint, string, string) ([]OpenWorkflow, error) { return r.openWF, nil }
func (r *memRepo) WriteAudit(e *model.AuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failLog != nil {
		return r.failLog
	}
	r.audits = append(r.audits, e)
	return nil
}

func (r *memRepo) auditActions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, a := range r.audits {
		out = append(out, a.Action)
	}
	return out
}

// ---- 任务 ----

func (r *memRepo) InsertTask(t *Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t.ID = r.id()
	c := *t
	r.tasks[t.ID] = &c
	return nil
}

func (r *memRepo) SaveTask(t *Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.tasks[t.ID]
	if cur == nil {
		return errors.New("no task")
	}
	if cur.Version != t.Version {
		return ErrVersion
	}
	t.Version++
	c := *t
	r.tasks[t.ID] = &c
	return nil
}

func (r *memRepo) GetTask(id uint, _ bool) (*Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.tasks[id]; t != nil {
		c := *t
		return &c, nil
	}
	return nil, nil
}

func (r *memRepo) ListTasks(f TaskFilter) ([]Task, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Task
	for _, t := range r.tasks {
		if !f.AllGroups {
			ok := false
			for _, g := range f.ResourceGroupIDs {
				ok = ok || g == t.ResourceGroupID
			}
			if !ok {
				continue
			}
		}
		if len(f.Statuses) > 0 {
			ok := false
			for _, s := range f.Statuses {
				ok = ok || s == t.Status
			}
			if !ok {
				continue
			}
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	total := int64(len(out))
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, total, nil
}

func (r *memRepo) ActiveTaskOnTable(inst uint, db, table string, except uint) (*Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		if t.ID != except && t.InstanceID == inst && t.DBName == db && t.TblName == table && (t.Status == StPending || t.Status == StEnabled || t.Status == StPaused) {
			c := *t
			return &c, nil
		}
	}
	return nil, nil
}

func (r *memRepo) TaskNameTaken(name string, except uint) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		if t.Name == name && t.ID != except {
			return true, nil
		}
	}
	return false, nil
}

func (r *memRepo) DeleteTask(id uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tasks, id)
	return nil
}

func (r *memRepo) UpdateCheckpoint(id uint, cp string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.tasks[id]; t != nil {
		t.Checkpoint = cp
	}
	return nil
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func (r *memRepo) ClaimNextRun(id uint, old, nw *time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tasks[id]
	if t == nil || t.Status != StEnabled || !sameTime(t.NextRunAt, old) {
		return false, nil
	}
	t.NextRunAt = nw
	return true, nil
}

// ---- 运行 ----

func (r *memRepo) InsertRun(x *Run) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	x.ID = r.id()
	c := *x
	r.runs[x.ID] = &c
	return nil
}

func (r *memRepo) SaveRun(x *Run) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := *x
	r.runs[x.ID] = &c
	return nil
}

func (r *memRepo) GetRun(id uint) (*Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if x := r.runs[id]; x != nil {
		c := *x
		return &c, nil
	}
	return nil, nil
}

func (r *memRepo) allRuns() []Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Run
	for _, x := range r.runs {
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func (r *memRepo) ListRuns(f RunFilter) ([]Run, int64, error) {
	var out []Run
	for _, x := range r.allRuns() {
		if !f.AllGroups {
			ok := false
			for _, g := range f.ResourceGroupIDs {
				ok = ok || g == x.ResourceGroupID
			}
			if !ok {
				continue
			}
		}
		if (f.TaskID != 0 && x.TaskID != f.TaskID) || (f.Trigger != "" && x.Trigger != f.Trigger) || (f.Result != "" && x.Result != f.Result) {
			continue
		}
		out = append(out, x)
	}
	total := int64(len(out))
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, total, nil
}

func (r *memRepo) RecentRuns(taskID uint, n int) ([]Run, error) {
	var out []Run
	for _, x := range r.allRuns() {
		if x.TaskID == taskID && len(out) < n {
			out = append(out, x)
		}
	}
	return out, nil
}

func (r *memRepo) RunningRuns(inst uint, leaseAfter time.Time) ([]Run, error) {
	var out []Run
	for _, x := range r.allRuns() {
		if (x.Result == ResRunning || x.Result == ResPaused) && (inst == 0 || x.InstanceID == inst) {
			if !leaseAfter.IsZero() && (x.LeaseUntil == nil || !x.LeaseUntil.After(leaseAfter)) {
				continue
			}
			out = append(out, x)
		}
	}
	return out, nil
}

func (r *memRepo) RunsRestoredBy(id uint) ([]uint, error) {
	var out []uint
	for _, x := range r.allRuns() {
		if x.RestoreOf == id {
			out = append(out, x.ID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (r *memRepo) ExpireLeases(before, now time.Time) ([]Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Run
	for _, x := range r.runs {
		if (x.Result == ResRunning || x.Result == ResPaused) && (x.LeaseUntil == nil || x.LeaseUntil.Before(before)) {
			x.Result, x.FinishedAt = ResInterrupted, &now
			out = append(out, *x)
		}
	}
	return out, nil
}

func (r *memRepo) InsertEvents(evs []Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range evs {
		dup := false
		for _, o := range r.events[e.RunID] {
			dup = dup || o.Seq == e.Seq
		}
		if !dup {
			r.events[e.RunID] = append(r.events[e.RunID], e)
		}
	}
	return nil
}

func (r *memRepo) ListEvents(runID, after uint, limit int) ([]Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.events[runID] {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memRepo) TrimEvents(runID uint, keep int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	evs := r.events[runID]
	sort.Slice(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
	if len(evs) > keep {
		r.events[runID] = evs[len(evs)-keep:]
	}
	return nil
}

func (r *memRepo) InsertBatchStats(runID uint, rows []BatchStat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats[runID] = append(r.stats[runID], rows...)
	return nil
}

func (r *memRepo) ListBatchStats(runID uint) ([]BatchStat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]BatchStat(nil), r.stats[runID]...), nil
}

// ---- 存储 / 策略 / 试运行 ----

func (r *memRepo) ListStores() ([]StoreRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []StoreRow
	for _, s := range r.stores {
		c := *s
		c.SKSet, c.SK = c.SK != "", ""
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *memRepo) GetStore(id uint) (*StoreRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.stores[id]; s != nil {
		c := *s
		c.SKSet = c.SK != ""
		return &c, nil
	}
	return nil, nil
}

func (r *memRepo) InsertStore(s *StoreRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.ID = r.id()
	c := *s
	r.stores[s.ID] = &c
	return nil
}

func (r *memRepo) SaveStore(s *StoreRow, keepSK bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := *s
	if keepSK && r.stores[s.ID] != nil {
		c.SK = r.stores[s.ID].SK
	}
	r.stores[s.ID] = &c
	return nil
}

func (r *memRepo) DeleteStore(id uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stores, id)
	return nil
}

func (r *memRepo) StoreRefCount(id uint) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, t := range r.tasks {
		if c := t.cfg(); c.DestFile != nil && c.DestFile.StoreID == id {
			n++
		}
	}
	return n, nil
}

func (r *memRepo) GetPolicy() (*PolicyRow, error) { return r.policy, nil }
func (r *memRepo) SavePolicy(p *PolicyRow) error  { r.policy = p; return nil }
func (r *memRepo) InsertCheck(c *CheckRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.ID = r.id()
	cp := *c
	r.checks[c.Token] = &cp
	return nil
}
func (r *memRepo) GetCheckByToken(t string) (*CheckRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.checks[t]; c != nil {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}
func (r *memRepo) PurgeChecks(time.Time) error { return nil }

func (r *memRepo) TryLock(_ context.Context, name string) (func(), bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locks[name] {
		return nil, false, nil
	}
	r.locks[name] = true
	return func() { r.mu.Lock(); delete(r.locks, name); r.mu.Unlock() }, true, nil
}

// ---- 其他替身 ----

type fakeSource struct{ list []model.Instance }

func (f *fakeSource) ListInstances() ([]model.Instance, error) { return f.list, nil }
func (f *fakeSource) EngineConn(*model.Instance) ieng.Conn     { return ieng.Conn{} }

type fakeAccess struct{ deny map[string]map[uint]bool } // username -> instance ids

func (a fakeAccess) FilterInstances(p query.Principal, list []model.Instance) ([]model.Instance, error) {
	var out []model.Instance
	for _, in := range list {
		if !a.deny[p.Username][in.ID] {
			out = append(out, in)
		}
	}
	return out, nil
}
func (a fakeAccess) CanUseInstance(p query.Principal, in *model.Instance) error {
	if a.deny[p.Username][in.ID] {
		return query.NewError(403, "no_privilege", "你所在组未关联该实例")
	}
	return nil
}

type recNotifier struct {
	mu  sync.Mutex
	evs []notify.Event
}

func (n *recNotifier) Notify(_ context.Context, ev notify.Event) {
	n.mu.Lock()
	n.evs = append(n.evs, ev)
	n.mu.Unlock()
}
func (n *recNotifier) names() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, e := range n.evs {
		out = append(out, e.Name)
	}
	return out
}

// ---- 测试环境 ----

type env struct {
	t       *testing.T
	repo    *memRepo
	svc     *Service
	h       *enginetest.Harness
	notif   *recNotifier
	wg      sync.WaitGroup
	insts   []model.Instance
	tmpDir  string
	openErr error

	super, dba, rd, tl, viewer Caller
	testStoreErr               error
}

// 角色：rd 提申请（apply/dryrun/view）；tl 一级审批；dba 二级审批 + 运维（pause/run/stop/restore/delete/cfg_view）；viewer 只读。
func caller(id uint, name string, super bool, codes ...string) Caller {
	set := map[string]bool{}
	for _, c := range codes {
		set[c] = true
	}
	return Caller{Principal: query.Principal{ID: id, Username: name, Display: name, IsSuperuser: super}, Can: func(c string) bool { return set[c] }}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, repo: newMemRepo(), notif: &recNotifier{}, tmpDir: t.TempDir()}
	e.h = enginetest.New(enginetest.Options{Rows: 300})
	e.insts = []model.Instance{
		{ID: 1, InstanceName: "mysql-prod-m1", Type: "master", DBType: "mysql", IsEnable: true},
		{ID: 2, InstanceName: "mysql-prod-s1", Type: "slave", DBType: "mysql", IsEnable: true},
		{ID: 3, InstanceName: "mssql-prod", Type: "master", DBType: "mssql", IsEnable: true},
		{ID: 4, InstanceName: "mysql-test-m1", Type: "master", DBType: "mysql", IsEnable: true},
	}
	r := e.repo
	r.rgs[1] = &model.ResourceGroup{GroupID: 1, GroupName: "订单组"}
	r.rgs[2] = &model.ResourceGroup{GroupID: 2, GroupName: "支付组"}
	r.instRGs[1], r.instRGs[4] = []uint{1}, []uint{1, 2}
	r.groups[10] = &model.AuthGroup{ID: 10, Name: "团队负责人"}
	r.groups[11] = &model.AuthGroup{ID: 11, Name: "DBA"}
	r.settings[[2]int{1, audit.TypeArchive}] = &model.WorkflowAuditSetting{GroupID: 1, GroupName: "订单组", WorkflowType: audit.TypeArchive, AuditAuthGroups: "10,11"}
	for id, u := range map[uint]string{5: "rd_zhao", 6: "tl_wang", 7: "dba_li", 8: "viewer", 9: "root"} {
		r.users[id] = model.User{ID: id, Username: u, Display: u, IsActive: true, IsSuperuser: id == 9}
		r.userRGs[id] = []uint{1}
	}
	r.members[6] = map[uint]bool{10: true}
	r.members[7] = map[uint]bool{11: true}
	e.super = caller(9, "root", true)
	e.rd = caller(5, "rd_zhao", false, "archive.view", "archive.apply", "archive.dryrun")
	e.tl = caller(6, "tl_wang", false, "archive.view")
	e.dba = caller(7, "dba_li", false, "archive.view", "archive.apply", "archive.dryrun", "archive.pause", "archive.run", "archive.stop", "archive.restore", "archive.delete", "archive.cfg_view")
	e.viewer = caller(8, "viewer", false, "archive.view")
	// 本地存储
	r.stores[1] = &StoreRow{ID: 1, Name: "本地归档盘", Type: StoreLocal, Path: e.tmpDir, Enabled: true, Status: "ok"}
	r.nextID = 100

	e.svc = NewService(r, &fakeSource{list: e.insts}, fakeAccess{deny: map[string]map[uint]bool{"viewer": {4: true}}}, Options{
		Open: func(ctx context.Context, spec engine.Spec) (*engine.Env, error) {
			if e.openErr != nil {
				return nil, e.openErr
			}
			return e.h.EnvFor(spec, e.tmpDir), nil
		},
		Catalog: func(context.Context, *model.Instance) (Catalog, error) { return &fakeCatalog{}, nil },
		TestStore: func(context.Context, engine.StoreConfig) (time.Duration, int64, bool, error) {
			return 12 * time.Millisecond, 1 << 40, true, e.testStoreErr
		},
		Notifier: e.notif,
		NotifyGo: func(f func()) { f() },
		Now:      e.h.Clock.Now,
		Spawn:    func(f func()) { e.wg.Add(1); go func() { defer e.wg.Done(); f() }() },
		BaseURL:  "https://dba.example.com",
		FileKey:  func() ([]byte, error) { return e.h.FileKey, nil },
		Owner:    "test:1",
	})
	return e
}

// wait 等待所有后台运行结束。
func (e *env) wait() {
	e.t.Helper()
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		e.t.Fatal("后台运行超时未结束")
	}
	time.Sleep(10 * time.Millisecond)
}

// baseInput 是一个合法的归档任务输入（对应 enginetest 的 shop.orders）。
func (e *env) baseInput(name string) TaskInput {
	return TaskInput{Name: name, ResourceGroupID: 1, InstanceID: 1, DB: "shop", Table: "orders", TimeColumn: "created_at", RetentionDays: 90, ExtraWhere: "status IN (3,4)",
		Mode:      "move",
		DestTable: &engine.TableDest{InstanceID: 1, DB: "shop_arch", Template: "orders_bak"},
		Params:    engine.Params{BatchSize: 100, SleepMs: 50, Window: engine.Window{Start: "01:00", End: "05:00"}},
		Schedule:  Schedule{Kind: "day", Time: "02:00"}}
}

// checked 试运行并返回带 token 的输入。
func (e *env) checked(c Caller, in TaskInput) TaskInput {
	e.t.Helper()
	resp, err := e.svc.Check(context.Background(), c, in)
	if err != nil {
		e.t.Fatalf("试运行失败: %v", err)
	}
	if resp.Blocked {
		e.t.Fatalf("试运行被阻断: %+v", resp.Report.Items)
	}
	in.Submit, in.CheckToken = true, resp.CheckToken
	in.AckWarn, in.AckBig = true, true
	return in
}

// enabled 创建任务并走完两级审批，返回已启用的任务。
func (e *env) enabled(name string) *Task {
	e.t.Helper()
	in := e.checked(e.rd, e.baseInput(name))
	r, err := e.svc.CreateTask(e.rd, in)
	if err != nil {
		e.t.Fatalf("创建: %v", err)
	}
	id := r.Task.ID
	if _, err := e.svc.Approve(e.tl, id, "同意"); err != nil {
		e.t.Fatalf("一级审批: %v", err)
	}
	if _, err := e.svc.Approve(e.dba, id, "同意"); err != nil {
		e.t.Fatalf("二级审批: %v", err)
	}
	t, _ := e.repo.GetTask(id, false)
	if t.Status != StEnabled {
		e.t.Fatalf("应已启用: %s", t.Status)
	}
	return t
}

type fakeCatalog struct{}

func (*fakeCatalog) Databases(context.Context) ([]string, error) {
	return []string{"shop", "shop_arch", "mysql", "bad-name"}, nil
}
func (*fakeCatalog) Tables(_ context.Context, db string) ([]CatTable, error) {
	return []CatTable{{Name: "orders", Rows: 300}, {Name: "bad-name"}}, nil
}
func (*fakeCatalog) Describe(_ context.Context, db, table string) (*CatTableInfo, error) {
	if table == "nope" {
		return nil, errNoRows
	}
	return &CatTableInfo{Columns: []CatColumn{{Name: "id", Type: "bigint", IsInt: true}, {Name: "created_at", Type: "datetime", Indexed: true, IsTime: true},
		{Name: "created_ms", Type: "bigint(20)", Indexed: true, IsInt: true}, {Name: "note", Type: "varchar(20)"}}, PKCols: []string{"id"}, HasPK: true, Rows: 300}, nil
}
func (*fakeCatalog) ProbeTime(_ context.Context, db, table, col string, rangeOK bool) (engine.TimeProbe, error) {
	return engine.TimeProbe{Sampled: 3, InMs: 3, Sample: 1700000000000, HasRange: rangeOK, Min: 1600000000000, Max: 1760000000000, OffsetSec: 8 * 3600, GuessUnit: engine.UnitEpochMs}, nil
}
func (*fakeCatalog) Close() error { return nil }

var errNoRows = sql.ErrNoRows

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func hasPrefixAny(l []string, prefix string) bool {
	for _, x := range l {
		if strings.HasPrefix(x, prefix) {
			return true
		}
	}
	return false
}

func mustErrCode(t *testing.T, err error, status int, code string) *Error {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("期望业务错误 %d/%s，得到 %v", status, code, err)
	}
	if pe.Status != status || pe.Code != code {
		t.Fatalf("期望 %d/%s，得到 %d/%s: %s", status, code, pe.Status, pe.Code, pe.Message)
	}
	return pe
}

var _ = fmt.Sprint

func newSetting() *model.WorkflowAuditSetting {
	return &model.WorkflowAuditSetting{GroupID: 1, GroupName: "订单组", WorkflowType: audit.TypeArchive, AuditAuthGroups: "10,11"}
}
