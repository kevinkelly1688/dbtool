// @author: kevinkelly
// @license: Apache Licence
// @file: fake_test.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/notify"
)

// ---- 假时钟 ----

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }
func (c *clock) Now() time.Time   { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func ts(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

// ---- 假目标库 ----

type fakeTarget struct {
	mu      sync.Mutex
	rules   []rule
	queries []string
	dbs     []string
	closed  bool
	block   chan struct{} // 非空时每次查询都等它关闭（或 ctx 取消）
	started chan struct{} // 第一次查询开始时关闭
	once    sync.Once
	hook    func(n int) // 每次查询（计数从 1 开始）开始时调用
}

type rule struct {
	sub string
	res *Result
	err error
}

func res(cols []string, rows ...[]any) *Result { return &Result{Cols: cols, Rows: rows} }

// kvRes 构造 SHOW STATUS / VARIABLES 样式的结果。
func kvRes(pairs ...string) *Result {
	r := &Result{Cols: []string{"Variable_name", "Value"}}
	for i := 0; i+1 < len(pairs); i += 2 {
		r.Rows = append(r.Rows, []any{pairs[i], pairs[i+1]})
	}
	return r
}

func (f *fakeTarget) on(sub string, r *Result) *fakeTarget {
	f.rules = append(f.rules, rule{sub: sub, res: r})
	return f
}

func (f *fakeTarget) onErr(sub string, err error) *fakeTarget {
	f.rules = append(f.rules, rule{sub: sub, err: err})
	return f
}

func (f *fakeTarget) Query(ctx context.Context, db, q string, args ...any) (*Result, error) {
	if err := CheckReadOnly(q); err != nil { // 假实现也强制只读：任何变更语句都会让测试失败
		return nil, err
	}
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.dbs = append(f.dbs, db)
	n := len(f.queries)
	f.mu.Unlock()
	if f.hook != nil {
		f.hook(n)
	}
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, r := range f.rules {
		if strings.Contains(q, r.sub) {
			if r.err != nil {
				return nil, r.err
			}
			return r.res, nil
		}
	}
	return nil, errors.New("fake: 没有这条查询的预设：" + truncate(q, 80))
}

func (f *fakeTarget) Close() error { f.closed = true; return nil }

// ---- 内存仓库 ----

type memReport struct {
	ReportDetail
	items []MSSQLItem
}

type memRepo struct {
	mu        sync.Mutex
	insts     []model.Instance
	tags      map[string][]uint
	reports   map[ReportRef]*memReport
	next      map[string]uint
	plans     map[uint]*Plan
	nextPlan  uint
	runs      map[uint]*Run
	nextRun   uint
	runItems  []RunItem
	audits    []model.AuditEntry
	config    map[string]string
	slow      map[string]any
	noTables  bool
	clk       *clock
	createErr error
}

func newMemRepo(clk *clock) *memRepo {
	return &memRepo{tags: map[string][]uint{}, reports: map[ReportRef]*memReport{}, next: map[string]uint{}, plans: map[uint]*Plan{}, runs: map[uint]*Run{},
		config: map[string]string{}, clk: clk}
}

func inst(id uint, name, dbType string) model.Instance {
	return model.Instance{ID: id, InstanceName: name, DBType: dbType, Host: "10.0.0." + fmt.Sprint(id), Port: 3306}
}

func (r *memRepo) ListInstances() ([]model.Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.Instance{}, r.insts...), nil
}

func (r *memRepo) GetInstance(id uint) (*model.Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.insts {
		if r.insts[i].ID == id {
			c := r.insts[i]
			return &c, nil
		}
	}
	return nil, nil
}

func (r *memRepo) InstanceIDsByTags(codes []string) ([]uint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []uint
	for _, c := range codes {
		out = append(out, r.tags[c]...)
	}
	return out, nil
}

func (r *memRepo) Conn(in *model.Instance) engine.Conn {
	return engine.Conn{Host: in.Host, Port: in.Port, User: "u", Password: "p"}
}

func (r *memRepo) CreateReport(kind string, instanceID uint, trigger, operator string, total int, now time.Time, staleAfter time.Duration) (uint, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return 0, false, r.createErr
	}
	for k, rep := range r.reports {
		if k.Kind == kind && rep.InstanceID == instanceID && rep.Status == StatusRunning {
			if rep.StartTime.Before(now.Add(-staleAfter)) {
				rep.Status, rep.Error = StatusFailed, "执行超时或任务进程中断"
				continue
			}
			return 0, true, nil
		}
	}
	r.next[kind]++
	id := r.next[kind]
	name, dbType := "", ""
	for _, in := range r.insts {
		if in.ID == instanceID {
			name, dbType = in.InstanceName, in.DBType
		}
	}
	rep := &memReport{}
	rep.ReportSummary = ReportSummary{Kind: kind, ID: id, InstanceID: instanceID, InstanceName: name, DBType: dbType, Status: StatusRunning, Trigger: trigger, Operator: operator, Total: total, StartTime: now}
	r.reports[ReportRef{kind, id}] = rep
	return id, false, nil
}

func (r *memRepo) rep(kind string, id uint) *memReport { return r.reports[ReportRef{kind, id}] }

func (r *memRepo) ReportStatus(kind string, id uint) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if x := r.rep(kind, id); x != nil {
		return x.Status, nil
	}
	return "", errors.New("not found")
}

func (r *memRepo) UpdateProgress(kind string, id uint, done, total int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if x := r.rep(kind, id); x != nil && x.Status == StatusRunning {
		x.Done, x.Total = done, total
	}
	return nil
}

func (r *memRepo) InsertMSSQLItems(id uint, items []MSSQLItem) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	x := r.rep(KindMSSQL, id)
	if x == nil {
		return errors.New("not found")
	}
	x.Items = append(x.Items, items...)
	return nil
}

func (r *memRepo) FinishReport(kind string, id uint, f Finish) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x := r.rep(kind, id)
	if x == nil || x.Status != StatusRunning {
		return false, nil
	}
	x.Status, x.Error = f.Status, f.Error
	at := f.At
	x.FinishTime = &at
	if f.Status == StatusFinish {
		x.Alert, x.Attention, x.Failed, x.Total, x.Done = f.Alert, f.Attention, f.Failed, f.Total, f.Done
		x.Normal = f.Normal
		x.Result, x.Summary, x.ServerStart = f.Result, f.Summary, f.ServerStart
		if kind == KindMySQL {
			h := f.HealthScore
			x.HealthScore = &h
		}
	}
	return true, nil
}

func (r *memRepo) CancelReports(refs []ReportRef, by string, now time.Time) ([]ReportRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var done []ReportRef
	for _, ref := range refs {
		if x := r.reports[ref]; x != nil && x.Status == StatusRunning {
			x.Status, x.Error = StatusCancelled, by
			t := now
			x.FinishTime = &t
			done = append(done, ref)
		}
	}
	return done, nil
}

func (r *memRepo) DeleteReports(refs []ReportRef) (deleted, running []ReportRef, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ref := range refs {
		x := r.reports[ref]
		if x == nil {
			continue
		}
		if x.Status == StatusRunning {
			running = append(running, ref)
			continue
		}
		delete(r.reports, ref)
		deleted = append(deleted, ref)
	}
	return
}

func (r *memRepo) planOf(kind string, id uint) (uint, string) {
	for _, it := range r.runItems {
		if it.Kind == kind && it.ReportID == id {
			if run := r.runs[it.RunID]; run != nil {
				return run.PlanID, run.PlanName
			}
		}
	}
	return 0, ""
}

// isLatestDone 判断这份报告是不是该实例最近一次结束的（成功或失败）报告：同一种报告里 ID 最大的那份（调用方已持有 r.mu）。
func (r *memRepo) isLatestDone(kind string, id, instanceID uint) bool {
	var best uint
	for ref, x := range r.reports {
		if ref.Kind != kind || x.InstanceID != instanceID || (x.Status != StatusFinish && x.Status != StatusFailed) {
			continue
		}
		if ref.ID > best {
			best = ref.ID
		}
	}
	return best == id
}

func (r *memRepo) ListReports(f ReportFilter) ([]ReportSummary, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	vis := map[uint]bool{}
	for _, id := range f.InstanceIDs {
		vis[id] = true
	}
	var all []ReportSummary
	for ref, x := range r.reports {
		s := x.ReportSummary
		if !f.AllInst && !vis[s.InstanceID] {
			continue
		}
		if f.InstanceID != 0 && s.InstanceID != f.InstanceID {
			continue
		}
		if f.Trigger != "" && s.Trigger != f.Trigger {
			continue
		}
		if f.Running && s.Status != StatusRunning {
			continue
		}
		if f.Latest && !r.isLatestDone(ref.Kind, ref.ID, s.InstanceID) {
			continue
		}
		switch f.Level {
		case "alert":
			if s.Alert == 0 {
				continue
			}
		case "attention":
			if s.Alert != 0 || s.Attention == 0 {
				continue
			}
		case "clean":
			if s.Status != StatusFinish || s.Alert != 0 || s.Attention != 0 {
				continue
			}
		case "failed":
			if s.Status != StatusFailed {
				continue
			}
		case "cancelled":
			if s.Status != StatusCancelled {
				continue
			}
		}
		if f.Search != "" && !strings.Contains(s.InstanceName, f.Search) {
			continue
		}
		if f.Start != nil && s.StartTime.Before(*f.Start) {
			continue
		}
		if f.End != nil && !s.StartTime.Before(*f.End) {
			continue
		}
		s.PlanID, s.PlanName = r.planOf(ref.Kind, ref.ID)
		if f.PlanID != 0 && s.PlanID != f.PlanID {
			continue
		}
		if f.RunID != 0 {
			found := false
			for _, it := range r.runItems {
				if it.RunID == f.RunID && it.Kind == ref.Kind && it.ReportID == ref.ID {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		all = append(all, s)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].StartTime.Equal(all[j].StartTime) {
			return all[i].StartTime.After(all[j].StartTime)
		}
		if all[i].Kind != all[j].Kind {
			return all[i].Kind > all[j].Kind
		}
		return all[i].ID > all[j].ID
	})
	total := int64(len(all))
	if f.Offset >= len(all) {
		return []ReportSummary{}, total, nil
	}
	all = all[f.Offset:]
	if f.Limit > 0 && len(all) > f.Limit {
		all = all[:f.Limit]
	}
	return all, total, nil
}

func (r *memRepo) GetReport(kind string, id uint) (*ReportDetail, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x := r.rep(kind, id)
	if x == nil {
		return nil, nil
	}
	d := x.ReportDetail
	d.PlanID, d.PlanName = r.planOf(kind, id)
	return &d, nil
}

func (r *memRepo) ReportBrief(kind string, id uint) (*ReportSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if x := r.rep(kind, id); x != nil {
		s := x.ReportSummary
		return &s, nil
	}
	return nil, nil
}

func (r *memRepo) SlowTop(instanceID uint, since time.Time, limit int) (map[string]any, bool) {
	if r.slow == nil {
		return nil, false
	}
	return r.slow, true
}

func (r *memRepo) PrevMSSQLRaw(instanceID, excludeID uint) (*PrevRaw, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *memReport
	for ref, x := range r.reports {
		if ref.Kind == KindMSSQL && x.InstanceID == instanceID && x.Status == StatusFinish && x.ID != excludeID && (best == nil || x.ID > best.ID) {
			best = x
		}
	}
	if best == nil {
		return nil, nil
	}
	out := &PrevRaw{ReportID: best.ID, StartTime: best.StartTime, ServerStart: best.ServerStart, Raw: map[string]string{}}
	for _, it := range best.Items {
		if it.RawCounter != "" {
			out.Raw[it.Key+"|"+it.DB] = it.RawCounter
		}
	}
	return out, nil
}

func (r *memRepo) PlanReportRefsBefore(planID uint, cutoff time.Time, limit int) ([]ReportRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ReportRef
	for _, it := range r.runItems {
		if run := r.runs[it.RunID]; run != nil && run.PlanID == planID && it.ReportID != 0 {
			if x := r.rep(it.Kind, it.ReportID); x != nil && x.StartTime.Before(cutoff) && x.Status != StatusRunning {
				out = append(out, ReportRef{it.Kind, it.ReportID})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memRepo) UnlinkedReportRefsBefore(kind string, cutoff time.Time, limit int) ([]ReportRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	linked := map[ReportRef]bool{}
	for _, it := range r.runItems {
		linked[ReportRef{it.Kind, it.ReportID}] = true
	}
	var out []ReportRef
	for ref, x := range r.reports {
		if ref.Kind == kind && !linked[ref] && x.StartTime.Before(cutoff) && x.Status != StatusRunning {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memRepo) PlanTablesReady() bool { return !r.noTables }

func clonePlan(p Plan) Plan {
	p.InstanceIDs = append([]uint{}, p.InstanceIDs...)
	p.TagCodes = append([]string{}, p.TagCodes...)
	p.Weekdays = append([]int{}, p.Weekdays...)
	p.Databases = append([]string{}, p.Databases...)
	p.NotifyChannels = append([]uint{}, p.NotifyChannels...)
	return p
}

func (r *memRepo) ListPlans() ([]Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Plan
	for _, p := range r.plans {
		out = append(out, clonePlan(*p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *memRepo) GetPlan(id uint) (*Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.plans[id]; p != nil {
		c := clonePlan(*p)
		return &c, nil
	}
	return nil, nil
}

func (r *memRepo) PlanByName(name string) (*Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.plans {
		if p.Name == name {
			c := clonePlan(*p)
			return &c, nil
		}
	}
	return nil, nil
}

func (r *memRepo) SavePlan(p *Plan) (uint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := clonePlan(*p)
	if c.ID == 0 {
		r.nextPlan++
		c.ID = r.nextPlan
	}
	r.plans[c.ID] = &c
	return c.ID, nil
}

func (r *memRepo) DeletePlan(id uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.plans, id)
	for rid, run := range r.runs {
		if run.PlanID == id {
			delete(r.runs, rid)
		}
	}
	return nil
}

func (r *memRepo) SetPlanEnabled(id uint, enabled bool, next *time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.plans[id]; p != nil {
		p.Enabled, p.NextRunAt = enabled, next
	}
	return nil
}

func (r *memRepo) DuePlans(now time.Time, limit int) ([]Plan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Plan
	for _, p := range r.plans {
		if p.Enabled && (p.NextRunAt == nil || !p.NextRunAt.After(now)) {
			out = append(out, clonePlan(*p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memRepo) ClaimPlan(id uint, expected time.Time, next *time.Time, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.plans[id]
	if p == nil || !p.Enabled || p.NextRunAt == nil || !p.NextRunAt.Equal(expected) {
		return false, nil
	}
	p.NextRunAt = next
	return true, nil
}

func (r *memRepo) MarkPlanRun(id uint, runID uint, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.plans[id]; p != nil {
		p.LastRunID = runID
		t := at
		p.LastRunAt = &t
	}
	return nil
}

func (r *memRepo) CreateRun(x *Run) (uint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextRun++
	c := *x
	c.ID = r.nextRun
	r.runs[c.ID] = &c
	return c.ID, nil
}

func (r *memRepo) UpdateRun(x *Run) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := *x
	r.runs[x.ID] = &c
	return nil
}

func (r *memRepo) TouchRun(id uint, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if x := r.runs[id]; x != nil && x.Status == RunRunning {
		x.Heartbeat = now
	}
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

func (r *memRepo) ListRuns(planID uint, limit int) ([]Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Run
	for _, x := range r.runs {
		if x.PlanID == planID {
			out = append(out, *x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memRepo) ActiveRun(planID uint, freshAfter time.Time) (*Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.runs {
		if x.PlanID == planID && x.Status == RunRunning && !x.Heartbeat.Before(freshAfter) {
			c := *x
			return &c, nil
		}
	}
	return nil, nil
}

func (r *memRepo) FailStaleRuns(before time.Time, now time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.runs {
		if x.Status == RunRunning && x.Heartbeat.Before(before) {
			x.Status, x.Note = RunFailed, "运行中断（进程退出或失联）"
			t := now
			x.FinishedAt = &t
			n++
		}
	}
	return n, nil
}

func (r *memRepo) AddRunItem(it *RunItem) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := *it
	c.ID = uint(len(r.runItems) + 1)
	r.runItems = append(r.runItems, c)
	return nil
}

func (r *memRepo) RunItems(runID uint) ([]RunItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []RunItem
	for _, it := range r.runItems {
		if it.RunID == runID {
			out = append(out, it)
		}
	}
	return out, nil
}

func (r *memRepo) DeleteRunsBefore(planID uint, cutoff time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, x := range r.runs {
		if x.PlanID == planID && x.StartedAt.Before(cutoff) && x.Status != RunRunning {
			delete(r.runs, id)
		}
	}
	return nil
}

func (r *memRepo) ConfigValue(item string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.config[item]
	return v, ok
}

func (r *memRepo) InsertAudit(e *model.AuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits = append(r.audits, *e)
	return nil
}

func (r *memRepo) auditActions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.audits))
	for i, a := range r.audits {
		out[i] = a.Action
	}
	return out
}

func (r *memRepo) report(kind string, id uint) *ReportDetail {
	d, _ := r.GetReport(kind, id)
	return d
}

// ---- 假通知器 ----

type fakeNotifier struct {
	mu       sync.Mutex
	channels []ChannelOption
	sent     []notify.Event
	sentIDs  [][]uint
	err      error
	n        int // Send 返回的成功数；-1 表示 = len(ids)
}

func (f *fakeNotifier) Channels() ([]ChannelOption, error) { return f.channels, nil }
func (f *fakeNotifier) Send(ctx context.Context, ids []uint, ev notify.Event) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, ev)
	f.sentIDs = append(f.sentIDs, ids)
	if f.err != nil {
		return 0, f.err
	}
	if f.n < 0 {
		return len(ids), nil
	}
	return len(ids), nil
}

// ---- 构造服务 ----

type env struct {
	svc  *Service
	repo *memRepo
	clk  *clock
	ft   func(inst model.Instance) *fakeTarget // 按实例给目标库；默认返回一个能跑通 MySQL 的假库
	nt   *fakeNotifier

	mu      sync.Mutex
	opened  []uint
	sleeps  []time.Duration
	openErr map[uint]error
}

func newEnv() *env {
	clk := newClock(ts("2026-10-06 08:00:00"))
	repo := newMemRepo(clk)
	repo.insts = []model.Instance{inst(1, "prod-mysql-01", "mysql"), inst(2, "prod-mysql-02", "mysql"), inst(3, "mssql-01", "mssql"), inst(4, "redis-01", "redis")}
	e := &env{repo: repo, clk: clk, nt: &fakeNotifier{channels: []ChannelOption{{ID: 7, Name: "研发群", Type: "dingtalk", Label: "钉钉", Enabled: true}, {ID: 8, Name: "DBA 群", Type: "slack", Label: "Slack", Enabled: true}}, n: -1}, openErr: map[uint]error{}}
	e.ft = func(in model.Instance) *fakeTarget {
		if in.DBType == "mssql" {
			return mssqlHappyTarget()
		}
		return mysqlHappyTarget()
	}
	e.svc = NewService(repo, Options{
		Now: clk.Now, Notifier: e.nt, BaseURL: "https://dba.example.com/",
		Open: func(ctx context.Context, dbType string, c engine.Conn) (Target, error) {
			var in model.Instance
			for _, x := range repo.insts {
				if x.Host == c.Host {
					in = x
				}
			}
			e.mu.Lock()
			e.opened = append(e.opened, in.ID)
			err := e.openErr[in.ID]
			e.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return e.ft(in), nil
		},
		Sleep: func(ctx context.Context, d time.Duration) {
			e.mu.Lock()
			e.sleeps = append(e.sleeps, d)
			e.mu.Unlock()
			clk.Add(d)
		},
		Manual: 4, BusyPoll: time.Second, RetryDelay: time.Minute,
	})
	return e
}

func (e *env) sleepList() []time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]time.Duration{}, e.sleeps...)
}

// waitIdle 等待所有后台巡检结束。
func (e *env) waitIdle() { e.svc.wg.Wait() }

// ---- 一个“能跑通”的 MySQL 假库 ----

func mysqlHappyTarget() *fakeTarget {
	return (&fakeTarget{}).
		on("SELECT VERSION()", res([]string{"v"}, []any{"8.0.36"})).
		on("SELECT @@version_comment", res([]string{"c"}, []any{"MySQL Community Server - GPL"})).
		on("Uptime", kvRes("Uptime", "3600")).
		on("variable_name IN ('Threads_connected'", kvRes("Threads_connected", "38", "Threads_running", "5", "Threads_cached", "3", "Aborted_connections", "1", "Aborted_clients", "2", "Connections", "9999")).
		on("variable_name = 'max_connections'", kvRes("max_connections", "100")).
		on("variable_name = 'innodb_buffer_pool_size'", kvRes("innodb_buffer_pool_size", "8589934592")).
		on("LIKE 'Innodb_buffer_pool%'", kvRes("Innodb_buffer_pool_read_requests", "1000000", "Innodb_buffer_pool_reads", "10000", "Innodb_buffer_pool_pages_total", "1000", "Innodb_buffer_pool_pages_free", "10", "Innodb_buffer_pool_pages_dirty", "20")).
		on("'sort_buffer_size'", kvRes("sort_buffer_size", "262144", "tmp_table_size", "16777216")).
		on("LIKE 'Innodb_data_%'", kvRes("Innodb_data_reads", "10", "Innodb_data_writes", "20", "Innodb_log_waits", "0", "Innodb_log_writes", "5")).
		on("'slow_query_log','long_query_time'", kvRes("slow_query_log", "ON", "long_query_time", "1.000000")).
		on("'Slow_queries'", kvRes("Slow_queries", "3", "Questions", "100", "Queries", "110", "Com_select", "60", "Com_insert", "20", "Com_update", "10", "Com_delete", "5")).
		on("Innodb_%lock%", kvRes("Innodb_row_lock_waits", "2", "Innodb_row_lock_time", "100", "Innodb_row_lock_time_avg", "50", "Innodb_trx_rseg_history_len", "20")).
		on("INFORMATION_SCHEMA.SCHEMATA", res([]string{"n"}, []any{int64(5)})).
		on("SUM(DATA_LENGTH + INDEX_LENGTH)", res([]string{"s"}, []any{"512.50"})).
		on("FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA NOT IN ('information_schema','mysql','performance_schema','sys')", res([]string{"n"}, []any{int64(100)})).
		on("information_schema.innodb_trx t", res([]string{"a"})).
		on("FROM information_schema.innodb_trx WHERE", res([]string{"a", "b"}, []any{int64(0), int64(0)})).
		on("information_schema.processlist WHERE STATE", res([]string{"a"})).
		on("c.EXTRA LIKE '%auto_increment%'", res([]string{"s", "t", "c", "dt", "ct", "ai"}, []any{"shop", "order_items", "id", "int", "int(11)", "1932735283"})).
		on("TABLE_CONSTRAINTS", res([]string{"s", "t", "e", "r"}, []any{"log", "tmp_a", "InnoDB", int64(10)}, []any{"log", "big", "InnoDB", int64(5000000)})).
		on("SELECT NOW()", res([]string{"n"}, []any{ts("2026-10-06 08:00:00")})).
		on("innodb_print_all_deadlocks", res([]string{"a", "b"}, []any{"innodb_print_all_deadlocks", "OFF"})).
		on("lock_deadlocks", res([]string{"c"}, []any{int64(0)})).
		on("SHOW ENGINE INNODB STATUS", res([]string{"t", "n", "s"}, []any{"InnoDB", "", "no deadlock"})).
		onErr("performance_schema.error_log", errors.New("Table 'performance_schema.error_log' doesn't exist")).
		on("SHOW VARIABLES LIKE 'long_query_time'", kvRes("long_query_time", "1")).
		on("events_statements_summary_by_digest", res([]string{"a", "b", "c", "d", "e", "f", "g", "h"},
			[]any{"shop", "select * from orders where status = ?", int64(8412), 26921.5, 3.2, 41.0, 1000.0, "2026-10-05 22:00:00"})).
		on("IFNULL(DATA_FREE, 0)", res([]string{"s", "t", "e", "r", "d", "i", "f"})).
		on("TABLE_COLLATION", res([]string{"s", "t", "c"}, []any{"shop", "orders", "utf8mb4_0900_ai_ci"}, []any{"log", "tmp_a", "utf8mb4_general_ci"})).
		on("CHARACTER_SET_NAME IS NOT NULL", res([]string{"s", "t", "c", "cs", "co"}, []any{"shop", "orders", "note", "utf8mb4", "utf8mb4_0900_ai_ci"})).
		on("information_schema.STATISTICS s", res([]string{"s", "t", "i", "u", "q", "c", "sub", "ty"},
			[]any{"shop", "orders", "idx_user", int64(1), int64(1), "user_id", int64(0), "BTREE"},
			[]any{"shop", "orders", "idx_user_created", int64(1), int64(1), "user_id", int64(0), "BTREE"},
			[]any{"shop", "orders", "idx_user_created", int64(1), int64(2), "created_at", int64(0), "BTREE"})).
		on("SHOW VARIABLES LIKE 'performance_schema'", kvRes("performance_schema", "OFF")).
		on("character_set_server", kvRes("character_set_server", "utf8mb4", "collation_server", "utf8mb4_0900_ai_ci")).
		on("SHOW GLOBAL VARIABLES", kvRes("innodb_flush_log_at_trx_commit", "2", "log_bin", "ON", "sync_binlog", "1", "binlog_format", "ROW", "binlog_row_image", "FULL", "transaction_isolation", "REPEATABLE-READ",
			"slow_query_log", "ON", "long_query_time", "1", "innodb_print_all_deadlocks", "OFF", "performance_schema", "OFF", "sql_mode", "STRICT_TRANS_TABLES", "character_set_server", "utf8mb4",
			"innodb_lock_wait_timeout", "50", "local_infile", "OFF", "innodb_file_per_table", "ON", "max_connections", "100")).
		on("FROM mysql.user", res([]string{"User", "Host", "authentication_string", "plugin", "Super_priv", "Grant_priv", "File_priv", "account_locked"},
			[]any{"root", "localhost", "*ABC", "caching_sha2_password", "Y", "Y", "Y", "N"},
			[]any{"app", "%", "", "mysql_native_password", "N", "N", "N", "N"})).
		on("SHOW REPLICA STATUS", res([]string{})).
		on("SHOW BINARY LOGS", res([]string{"n", "s"}, []any{"binlog.000001", int64(1073741824)}))
}

// ---- 一个“能跑通”的 SQL Server 假库 ----

func mssqlHappyTarget() *fakeTarget {
	return (&fakeTarget{}).
		on("SERVERPROPERTY('ProductVersion')", res([]string{"version", "level", "edition", "cu", "server_name", "sqlserver_start_time", "cpu_count", "memory_mb"},
			[]any{"15.0.4153.1", "RTM", "Enterprise Edition", "CU18", "SQL01", ts("2026-09-12 02:10:00"), int64(16), int64(131072)})).
		on("sys.configurations", res([]string{"name", "v"}, []any{"max degree of parallelism", int64(0)}, []any{"cost threshold for parallelism", int64(5)}, []any{"max server memory (MB)", int64(110000)})).
		on("FROM sys.databases WHERE database_id > 4", res([]string{"name"}, []any{"ShopDB"}, []any{"Report"})).
		on("wait_type NOT LIKE 'SLEEP_%'", res([]string{"wait_type", "wait_time_ms", "signal_wait_time_ms", "waiting_tasks_count"}, []any{"PAGEIOLATCH_SH", int64(9000), int64(100), int64(50)}, []any{"CXPACKET", int64(1000), int64(900), int64(20)})).
		on("sys.dm_os_wait_stats WHERE wait_type NOT IN", res([]string{"signal_ms", "wait_ms"}, []any{int64(500), int64(1000)})).
		on("sys.dm_io_virtual_file_stats", res([]string{"database_id", "file_id", "db_name", "file_name", "type_desc", "num_of_reads", "io_stall_read_ms", "num_of_writes", "io_stall_write_ms", "num_of_bytes_read", "num_of_bytes_written"},
			[]any{int64(5), int64(2), "ShopDB", "ShopDB_log", "LOG", int64(1000), int64(500), int64(5000), int64(40000), int64(10485760), int64(20971520)})).
		on("sys.dm_exec_query_stats", res([]string{"execution_count", "total_cpu_ms", "total_elapsed_ms", "total_logical_reads", "last_execution_time", "stmt", "db_name"})).
		on("sys.dm_os_buffer_descriptors", res([]string{"db_name", "mb"})).
		on("sys.dm_db_missing_index_groups", res([]string{"table_name"})).
		on("sys.dm_db_index_usage_stats", res([]string{"table_name"})).
		on("sys.database_files", res([]string{"name"})).
		on("log_reuse_wait_desc", res([]string{"log_reuse_wait_desc", "recovery_model_desc"}, []any{"NOTHING", "FULL"})).
		on("sys.dm_db_log_info", res([]string{"n"}, []any{int64(120)})).
		on("is_auto_shrink_on", res([]string{"is_auto_shrink_on", "page_verify_option_desc", "is_auto_create_stats_on", "is_auto_update_stats_on", "compatibility_level", "is_read_only"}, []any{int64(0), "CHECKSUM", int64(1), int64(1), int64(150), int64(0)})).
		on("Page life expectancy", res([]string{"name", "cntr_value"}, []any{"Page life expectancy", int64(180)}, []any{"Database pages", int64(2097152)})).
		on("Number of Deadlocks/sec", res([]string{"cntr_value"}, []any{int64(7)})).
		on("dm_exec_requests r JOIN", res([]string{"session_id"})).
		on("dm_tran_active_transactions", res([]string{"session_id"})).
		on("sys.dm_os_schedulers", res([]string{"avg_r", "max_r", "n"}, []any{0.5, int64(2), int64(16)})).
		on("dm_exec_query_memory_grants WHERE grant_time IS NULL", res([]string{"n"}, []any{int64(0)})).
		on("dm_exec_query_memory_grants WHERE grant_time IS NOT NULL", res([]string{"n"}, []any{int64(3)})).
		on("dm_io_pending_io_requests", res([]string{"n"}, []any{int64(0)})).
		on("database_id = 2 AND type = 0", res([]string{"files", "min_mb", "max_mb"}, []any{int64(8), int64(1024), int64(1024)})).
		on("msdb.dbo.sysjobhistory", res([]string{"job_name"})).
		on("sys.dm_server_memory_dumps", res([]string{"filename"})).
		on("msdb.dbo.backupset", res([]string{"type", "last_finish"})).
		on("recovery_model_desc FROM sys.databases WHERE database_id = DB_ID()", res([]string{"recovery_model_desc"}, []any{"SIMPLE"}))
}

func ids(l ...uint) []uint { return l }
