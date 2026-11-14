// @author: kevinkelly
// @license: Apache Licence
// @file: memrepo_test.go
// @time: 2026/10/28

package perf

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// memRepo 是 Repo 的内存假实现（测试用）。语义与 store.go 的 SQL 对齐：唯一键、乐观锁、列表筛选、排行榜聚合。
type memRepo struct {
	mu sync.Mutex

	issues map[uint]*Issue
	nextID uint
	ver    int64 // 递增的版本，模拟 update_time 的刷新

	logs    []IssueLog
	stats   map[string]*IssueStat // issueID|date
	users   map[uint]UserBrief
	groups  []GroupBrief
	ugroups map[uint][]uint // 用户 → 资源组
	igroups map[uint][]uint // 实例 → 资源组
	insts   map[uint]*model.Instance
	teams   []TeamConfig
	routes  []Route
	sla     []SLAPolicy
	collect []CollectConfig
	cursors []DigestCursor
	jobRuns map[string]bool
	jobsOK  bool
	cfg     map[string]string
	audits  []model.AuditEntry
	owners  map[string]string // "实例ID:库" → 负责人用户名
	dbaLoad map[uint]int

	slowRows []SlowLogRow // 按实例过滤用 slowInst
	slowInst map[uint]uint
	slowMD5  map[string]*SlowSample
	slowDays map[string][]Digest
	pt       []Digest
	ptErr    error

	// 测试钩子：UpdateIssue 真正比较版本之前调用，用来模拟别人在中间改了工单。
	beforeUpdate func(id uint)
	nextNowStart time.Time
	lockHeld     map[uint]bool
	failAudit    bool

	// 扩展（ext_memrepo_test.go）
	extIssue  map[uint]IssueExt
	extRoute  map[uint]string
	settings  map[string]string
	workflows map[uint]bool
	extTables map[string]bool // 已“创建”的旁路表；nil 表示全部可用
}

func newMemRepo() *memRepo {
	return &memRepo{issues: map[uint]*Issue{}, stats: map[string]*IssueStat{}, users: map[uint]UserBrief{}, ugroups: map[uint][]uint{},
		igroups: map[uint][]uint{}, insts: map[uint]*model.Instance{}, jobRuns: map[string]bool{}, cfg: map[string]string{}, owners: map[string]string{},
		dbaLoad: map[uint]int{}, slowInst: map[uint]uint{}, slowMD5: map[string]*SlowSample{},
		slowDays: map[string][]Digest{}, lockHeld: map[uint]bool{}, nextNowStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)}
}

var _ Repo = (*memRepo)(nil)

// ---- 工单 ----

func (m *memRepo) names(i *Issue) IssueRow {
	r := IssueRow{Issue: *i}
	if in := m.insts[i.InstanceID]; in != nil {
		r.InstanceName = in.InstanceName
	}
	if i.TeamID != nil {
		for _, g := range m.groups {
			if g.ID == *i.TeamID {
				r.TeamName = g.Name
			}
		}
	}
	if i.AssigneeID != nil {
		r.AssigneeName = m.users[*i.AssigneeID].Name()
	}
	if i.DBAID != nil {
		r.DBAName = m.users[*i.DBAID].Name()
	}
	return r
}

func (m *memRepo) GetIssue(id uint) (*Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i := m.issues[id]; i != nil {
		c := *i
		return &c, nil
	}
	return nil, nil
}

func (m *memRepo) GetIssueRow(id uint) (*IssueRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i := m.issues[id]; i != nil {
		r := m.names(i)
		return &r, nil
	}
	return nil, nil
}

func (m *memRepo) FindIssue(instanceID uint, db, checksum string) (*Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, i := range m.issues {
		if i.InstanceID == instanceID && i.DBName == db && i.Checksum == checksum {
			c := *i
			return &c, nil
		}
	}
	return nil, nil
}

func (m *memRepo) tick() time.Time {
	m.ver++
	return m.nextNowStart.Add(time.Duration(m.ver) * time.Microsecond)
}

func (m *memRepo) InsertIssue(i *Issue) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.issues {
		if o.InstanceID == i.InstanceID && o.DBName == i.DBName && o.Checksum == i.Checksum {
			return ErrDuplicate
		}
	}
	m.nextID++
	i.ID = m.nextID
	i.UpdateTime = m.tick()
	c := *i
	m.issues[i.ID] = &c
	return nil
}

func (m *memRepo) UpdateIssue(i *Issue, prev time.Time) error {
	if m.beforeUpdate != nil {
		m.beforeUpdate(i.ID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.issues[i.ID]
	if cur == nil || !cur.UpdateTime.Equal(prev) {
		return ErrConflict
	}
	// 与 store.go 一致：影响指标由采集刷新维护，状态流转不覆盖它们
	keep := *cur
	c := *i
	c.ExecCount7d, c.TotalTime7d, c.AvgTimeMs, c.MaxTimeMs = keep.ExecCount7d, keep.TotalTime7d, keep.AvgTimeMs, keep.MaxTimeMs
	c.RowsExaminedAvg, c.RowsSentAvg, c.ImpactScore, c.PerDayCount = keep.RowsExaminedAvg, keep.RowsSentAvg, keep.ImpactScore, keep.PerDayCount
	c.LastSeen, c.LoadShare = keep.LastSeen, keep.LoadShare
	c.InstanceID, c.DBName, c.Checksum, c.Fingerprint, c.Tables, c.Source, c.FirstSeen, c.CreateTime = keep.InstanceID, keep.DBName, keep.Checksum, keep.Fingerprint, keep.Tables, keep.Source, keep.FirstSeen, keep.CreateTime
	c.UpdateTime = m.tick()
	m.issues[i.ID] = &c
	i.UpdateTime = c.UpdateTime
	return nil
}

func (m *memRepo) UpdateIssueMetrics(i *Issue) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.issues[i.ID]
	if cur == nil {
		return errors.New("no issue")
	}
	cur.ExecCount7d, cur.TotalTime7d, cur.AvgTimeMs, cur.MaxTimeMs = i.ExecCount7d, i.TotalTime7d, i.AvgTimeMs, i.MaxTimeMs
	cur.RowsExaminedAvg, cur.RowsSentAvg, cur.ImpactScore, cur.PerDayCount = i.RowsExaminedAvg, i.RowsSentAvg, i.ImpactScore, i.PerDayCount
	cur.LastSeen, cur.SampleSQL = i.LastSeen, i.SampleSQL
	return nil
}

func (m *memRepo) visible(i *Issue, q IssueQuery) bool {
	switch q.View {
	case ViewTodo:
		ok := (i.AssigneeID != nil && *i.AssigneeID == q.UserID && isSLARunning(i.Status)) ||
			(i.DBAID != nil && *i.DBAID == q.UserID && (i.Status == StFixed || i.Status == StWaivePending))
		if !ok {
			return false
		}
	case ViewTodoOverdue:
		if !(i.AssigneeID != nil && *i.AssigneeID == q.UserID && isSLARunning(i.Status) && i.ResolveDeadline != nil && i.ResolveDeadline.Before(q.Now)) {
			return false
		}
	case ViewOpen:
		if !isOpen(i.Status) {
			return false
		}
	case ViewOverdue:
		if !(isSLARunning(i.Status) && i.ResolveDeadline != nil && i.ResolveDeadline.Before(q.Now)) {
			return false
		}
	case ViewFixed:
		if i.Status != StFixed {
			return false
		}
	case ViewClosed:
		if i.Status != StClosed {
			return false
		}
	case ViewNew, ViewAssigned, ViewAccepted:
		if i.Status != q.View {
			return false
		}
	case ViewApprove:
		if !(i.Status == StWaivePending || (i.PendingExtendDate != nil && isOpen(i.Status))) {
			return false
		}
	}
	if q.Status != "" && i.Status != q.Status || q.Source != "" && i.Source != q.Source {
		return false
	}
	if q.Restrict {
		inTeam := false
		if i.TeamID != nil {
			for _, t := range q.TeamIDs {
				inTeam = inTeam || t == *i.TeamID
			}
		}
		if !inTeam && !(i.AssigneeID != nil && *i.AssigneeID == q.UserID) {
			return false
		}
	}
	if q.Severity != "" && i.Severity != q.Severity || q.OwnerSide != "" && i.OwnerSide != q.OwnerSide || q.Category != "" && i.Category != q.Category {
		return false
	}
	if q.TeamID != 0 && (i.TeamID == nil || *i.TeamID != q.TeamID) || q.InstanceID != 0 && i.InstanceID != q.InstanceID {
		return false
	}
	if q.Search != "" {
		s := strings.ToLower(strings.TrimPrefix(q.Search, "#"))
		if !strings.Contains(strings.ToLower(i.Fingerprint), s) && !strings.Contains(strings.ToLower(i.DBName), s) &&
			!strings.Contains(strings.ToLower(i.Tables), s) && fmt.Sprint(i.ID) != s {
			return false
		}
	}
	return true
}

func (m *memRepo) ListIssues(q IssueQuery) ([]IssueRow, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []*Issue
	for _, i := range m.issues {
		if m.visible(i, q) {
			list = append(list, i)
		}
	}
	less := func(a, b *Issue) int {
		switch q.Sort {
		case "severity":
			return strings.Compare(a.Severity, b.Severity)
		case "avg_time_ms":
			return cmpF(a.AvgTimeMs, b.AvgTimeMs)
		case "resolve_deadline":
			if a.ResolveDeadline == nil || b.ResolveDeadline == nil {
				return 0
			}
			return cmpF(float64(a.ResolveDeadline.Unix()), float64(b.ResolveDeadline.Unix()))
		case "id":
			return cmpF(float64(a.ID), float64(b.ID))
		}
		return cmpF(a.ImpactScore, b.ImpactScore)
	}
	sort.SliceStable(list, func(x, y int) bool {
		c := less(list[x], list[y])
		if c == 0 {
			return list[x].ID > list[y].ID
		}
		if q.Desc {
			return c > 0
		}
		return c < 0
	})
	total := len(list)
	if q.Offset > len(list) {
		list = nil
	} else {
		list = list[q.Offset:]
	}
	if q.Limit > 0 && len(list) > q.Limit {
		list = list[:q.Limit]
	}
	out := make([]IssueRow, 0, len(list))
	for _, i := range list {
		out = append(out, m.names(i))
	}
	return out, total, nil
}

func cmpF(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (m *memRepo) IssuesByStatus(statuses []string) ([]Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Issue
	for _, i := range m.issues {
		if contains(statuses, i.Status) {
			out = append(out, *i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func (m *memRepo) AbsentIssues(statuses []string, source string, before time.Time) ([]Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Issue
	for _, i := range m.issues {
		if contains(statuses, i.Status) && i.Source == source && i.LastSeen != nil && i.LastSeen.Before(before) {
			out = append(out, *i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func (m *memRepo) RefreshLoadShare(instanceID uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0.0
	for _, i := range m.issues {
		if i.InstanceID == instanceID && i.TotalTime7d > 0 {
			total += i.TotalTime7d
		}
	}
	if total == 0 {
		return nil
	}
	for _, i := range m.issues {
		if i.InstanceID == instanceID && i.TotalTime7d > 0 {
			i.LoadShare = round(i.TotalTime7d*100/total, 2)
		}
	}
	return nil
}

// ---- 日志与日指标 ----

func (m *memRepo) AddLog(l *IssueLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l.ID = uint(len(m.logs) + 1)
	m.logs = append(m.logs, *l)
	return nil
}

func (m *memRepo) ListLogs(issueID uint, limit int) ([]IssueLog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []IssueLog
	for k := len(m.logs) - 1; k >= 0; k-- {
		if m.logs[k].IssueID == issueID && len(out) < limit {
			out = append(out, m.logs[k])
		}
	}
	return out, nil
}

func (m *memRepo) logActions(issueID uint) []string {
	var out []string
	for _, l := range m.logs {
		if l.IssueID == issueID {
			out = append(out, l.Action)
		}
	}
	return out
}

func (m *memRepo) AddStat(issueID uint, day time.Time, d StatDelta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := fmt.Sprintf("%d|%s", issueID, day.Format(dateLayout))
	st := m.stats[k]
	if st == nil {
		st = &IssueStat{IssueID: issueID, StatDate: dateOf(day)}
		m.stats[k] = st
	}
	st.ExecCount += d.ExecCount
	st.TotalTime += d.TotalTime
	st.RowsExamined += d.RowsExamined
	st.RowsSent += d.RowsSent
	if d.MaxTimeMs > st.MaxTimeMs {
		st.MaxTimeMs = d.MaxTimeMs
	}
	return nil
}

func (m *memRepo) ListStats(issueID uint, since time.Time) ([]IssueStat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []IssueStat
	for _, s := range m.stats {
		if s.IssueID == issueID && !s.StatDate.Before(dateOf(since)) {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].StatDate.Before(out[b].StatDate) })
	return out, nil
}

func (m *memRepo) SumStats(issueID uint, since, until time.Time) (StatSum, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var r StatSum
	for _, s := range m.stats {
		if s.IssueID != issueID || s.StatDate.Before(dateOf(since)) || (!until.IsZero() && !s.StatDate.Before(dateOf(until))) {
			continue
		}
		r.ExecCount += s.ExecCount
		r.TotalTime += s.TotalTime
		r.RowsExamined += s.RowsExamined
		r.RowsSent += s.RowsSent
		if s.MaxTimeMs > r.MaxTimeMs {
			r.MaxTimeMs = s.MaxTimeMs
		}
	}
	return r, nil
}

// ---- 排行榜聚合 ----

func (m *memRepo) TeamAggregates(q BoardQuery) ([]TeamAgg, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	by := map[uint]*TeamAgg{}
	sumFix := map[uint]float64{}
	for _, i := range m.issues {
		if i.TeamID == nil || (q.OwnerSide != "" && i.OwnerSide != q.OwnerSide) {
			continue
		}
		a := by[*i.TeamID]
		if a == nil {
			a = &TeamAgg{TeamID: *i.TeamID}
			for _, g := range m.groups {
				if g.ID == a.TeamID {
					a.TeamName = g.Name
				}
			}
			by[*i.TeamID] = a
		}
		open := isOpen(i.Status)
		if open {
			a.Open++
			a.Waste += i.ImpactScore
			if i.Severity == SevP0 || i.Severity == SevP1 {
				a.P0P1Open++
			}
		}
		if i.Status == StNew || i.Status == StAssigned {
			a.NotAccepted++
		}
		if isSLARunning(i.Status) && i.ResolveDeadline != nil && i.ResolveDeadline.Before(q.Now) {
			a.Overdue++
		}
		if !i.CreateTime.Before(q.Start) && i.CreateTime.Before(q.End) {
			a.New++
		}
		if i.ClosedAt != nil && !i.ClosedAt.Before(q.Start) && i.ClosedAt.Before(q.End) && i.CloseReason == CloseVerified {
			a.Closed++
			a.FixDaysN++
			if !i.SLABreached {
				a.ClosedOnTime++
			}
			done := *i.ClosedAt
			if i.FixedAt != nil {
				done = *i.FixedAt
			}
			sumFix[a.TeamID] += done.Sub(i.CreateTime).Hours() / 24
		}
	}
	var out []TeamAgg
	for id, a := range by {
		if a.FixDaysN > 0 {
			a.FixDaysAvg = sumFix[id] / float64(a.FixDaysN)
		}
		out = append(out, *a)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].TeamID < out[b].TeamID })
	return out, nil
}

func (m *memRepo) OverallAggregate(q BoardQuery) (OverallAgg, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var o OverallAgg
	for _, i := range m.issues {
		if isOpen(i.Status) {
			o.Open++
			o.Waste += i.ImpactScore
			if i.OwnerSide == SideDBA {
				o.DBAOpen++
			}
		}
		if isSLARunning(i.Status) && i.ResolveDeadline != nil && i.ResolveDeadline.Before(q.Now) {
			o.Overdue++
		}
		if !i.CreateTime.Before(q.Start) && i.CreateTime.Before(q.End) {
			o.New++
		}
		if i.ClosedAt != nil && !i.ClosedAt.Before(q.Start) && i.ClosedAt.Before(q.End) && i.CloseReason == CloseVerified {
			o.Closed++
			o.Saved += i.BaselineDailySec
		}
	}
	return o, nil
}

func monday(t time.Time) string {
	d := dateOf(t)
	off := (int(d.Weekday()) + 6) % 7
	return d.AddDate(0, 0, -off).Format(dateLayout)
}

func (m *memRepo) WeeklyTrend(since time.Time) ([]WeekCount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := map[string]*WeekCount{}
	get := func(k string) *WeekCount {
		if w[k] == nil {
			w[k] = &WeekCount{Week: k}
		}
		return w[k]
	}
	for _, i := range m.issues {
		if !i.CreateTime.Before(since) {
			get(monday(i.CreateTime)).Created++
		}
		if i.ClosedAt != nil && !i.ClosedAt.Before(since) && i.CloseReason == CloseVerified {
			get(monday(*i.ClosedAt)).Closed++
		}
	}
	var out []WeekCount
	for _, c := range w {
		out = append(out, *c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Week < out[b].Week })
	return out, nil
}

func (m *memRepo) CategoryWaste() ([]CategoryAgg, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	by := map[string]*CategoryAgg{}
	for _, i := range m.issues {
		if !isOpen(i.Status) {
			continue
		}
		if by[i.Category] == nil {
			by[i.Category] = &CategoryAgg{Category: i.Category}
		}
		by[i.Category].Count++
		by[i.Category].Waste += i.ImpactScore
	}
	var out []CategoryAgg
	for _, c := range by {
		out = append(out, *c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Waste > out[b].Waste })
	return out, nil
}

// ---- 配置 ----

func (m *memRepo) ListTeams() ([]TeamConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]TeamConfig(nil), m.teams...), nil
}

func (m *memRepo) GetTeamByGroup(groupID uint) (*TeamConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.teams {
		if t.ResourceGroupID == groupID {
			c := t
			return &c, nil
		}
	}
	return nil, nil
}

func (m *memRepo) GetTeam(id uint) (*TeamConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.teams {
		if t.ID == id {
			c := t
			return &c, nil
		}
	}
	return nil, nil
}

func (m *memRepo) SaveTeam(t *TeamConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ID == 0 {
		t.ID = uint(len(m.teams) + 100)
		m.teams = append(m.teams, *t)
		return nil
	}
	for i := range m.teams {
		if m.teams[i].ID == t.ID {
			m.teams[i] = *t
		}
	}
	return nil
}

func (m *memRepo) DeleteTeam(id uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.teams {
		if m.teams[i].ID == id {
			m.teams = append(m.teams[:i], m.teams[i+1:]...)
			return nil
		}
	}
	return nil
}

func (m *memRepo) ListRoutes() ([]Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]Route(nil), m.routes...)
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Priority != out[b].Priority {
			return out[a].Priority < out[b].Priority
		}
		return out[a].ID < out[b].ID
	})
	return out, nil
}

func (m *memRepo) GetRoute(id uint) (*Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.routes {
		if r.ID == id {
			c := r
			return &c, nil
		}
	}
	return nil, nil
}

func (m *memRepo) SaveRoute(r *Route) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.ID == 0 {
		r.ID = uint(len(m.routes) + 1)
		m.routes = append(m.routes, *r)
		return nil
	}
	for i := range m.routes {
		if m.routes[i].ID == r.ID {
			m.routes[i] = *r
		}
	}
	return nil
}

func (m *memRepo) DeleteRoute(id uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.routes {
		if m.routes[i].ID == id {
			m.routes = append(m.routes[:i], m.routes[i+1:]...)
			return nil
		}
	}
	return nil
}

func (m *memRepo) ListSLA() ([]SLAPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SLAPolicy(nil), m.sla...), nil
}

func (m *memRepo) SaveSLA(ps []SLAPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range ps {
		found := false
		for i := range m.sla {
			if m.sla[i].Severity == p.Severity {
				id := m.sla[i].ID
				m.sla[i] = p
				m.sla[i].ID = id
				found = true
			}
		}
		if !found {
			p.ID = uint(len(m.sla) + 1)
			m.sla = append(m.sla, p)
		}
	}
	return nil
}

func (m *memRepo) ListCollectConfigs() ([]CollectConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]CollectConfig(nil), m.collect...), nil
}

func (m *memRepo) GetCollectConfig(id uint) (*CollectConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.collect {
		if c.ID == id {
			x := c
			return &x, nil
		}
	}
	return nil, nil
}

func (m *memRepo) GetCollectConfigByInstance(instanceID uint) (*CollectConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.collect {
		if c.InstanceID == instanceID {
			x := c
			return &x, nil
		}
	}
	return nil, nil
}

func (m *memRepo) SaveCollectConfig(c *CollectConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ID == 0 {
		for _, o := range m.collect {
			if o.InstanceID == c.InstanceID {
				return ErrDuplicate
			}
		}
		c.ID = uint(len(m.collect) + 1)
		m.collect = append(m.collect, *c)
		return nil
	}
	for i := range m.collect {
		if m.collect[i].ID == c.ID {
			lct, ls := m.collect[i].LastCollectTime, m.collect[i].LastStatus
			m.collect[i] = *c
			m.collect[i].LastCollectTime, m.collect[i].LastStatus = lct, ls
		}
	}
	return nil
}

func (m *memRepo) DeleteCollectConfig(id uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.collect {
		if m.collect[i].ID == id {
			m.collect = append(m.collect[:i], m.collect[i+1:]...)
			return nil
		}
	}
	return nil
}

func (m *memRepo) LockCollect(id uint, token, staleBefore string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.collect {
		if m.collect[i].ID == id {
			st := m.collect[i].LastStatus
			if strings.HasPrefix(st, runningPrefix) && st >= staleBefore {
				return false, nil
			}
			m.collect[i].LastStatus = token
			return true, nil
		}
	}
	return false, nil
}

func (m *memRepo) FinishCollect(id uint, end *time.Time, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.collect {
		if m.collect[i].ID == id {
			m.collect[i].LastStatus = status
			if end != nil {
				e := *end
				m.collect[i].LastCollectTime = &e
			}
		}
	}
	return nil
}

// ---- 游标 ----

func (m *memRepo) GetCursor(instanceID uint, schema, digest string) (*DigestCursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.cursors {
		if c.InstanceID == instanceID && c.SchemaName == schema && c.Digest == digest {
			x := c
			return &x, nil
		}
	}
	return nil, nil
}

func (m *memRepo) SetCursor(c *DigestCursor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.cursors {
		o := &m.cursors[i]
		if o.InstanceID == c.InstanceID && o.SchemaName == c.SchemaName && o.Digest == c.Digest {
			o.CountStar = c.CountStar
			return nil
		}
	}
	x := *c
	x.ID = uint(len(m.cursors) + 1)
	m.cursors = append(m.cursors, x)
	return nil
}

func (m *memRepo) ListCursors(instanceID uint) ([]DigestCursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DigestCursor
	for _, c := range m.cursors {
		if c.InstanceID == instanceID && c.SchemaName != slowCursorSchema {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memRepo) SaveCursors(creates, updates []DigestCursor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range creates {
		c.ID = uint(len(m.cursors) + 1)
		m.cursors = append(m.cursors, c)
	}
	for _, u := range updates {
		for i := range m.cursors {
			if m.cursors[i].ID == u.ID {
				m.cursors[i] = u
			}
		}
	}
	return nil
}

// ---- 任务运行记录 ----

func (m *memRepo) JobRunsReady() bool { return m.jobsOK }

func (m *memRepo) ClaimJobRun(job, key string, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := job + "|" + key
	if m.jobRuns[k] {
		return false, nil
	}
	m.jobRuns[k] = true
	return true, nil
}

func (m *memRepo) FinishJobRun(string, string, time.Time, string) error { return nil }

// ---- 目录 ----

func (m *memRepo) GetUsers(ids []uint) (map[uint]UserBrief, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uint]UserBrief{}
	for _, id := range ids {
		if u, ok := m.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (m *memRepo) ListActiveUsers() ([]UserBrief, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []UserBrief
	for _, u := range m.users {
		if u.Active {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func (m *memRepo) UsersByNames(names []string) ([]UserBrief, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []UserBrief
	for _, u := range m.users {
		if u.Active && contains(names, u.Username) {
			out = append(out, u)
		}
	}
	return out, nil
}

func (m *memRepo) UserGroupIDs(userID uint) ([]uint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]uint(nil), m.ugroups[userID]...), nil
}

func (m *memRepo) ListGroups() ([]GroupBrief, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]GroupBrief(nil), m.groups...), nil
}

func (m *memRepo) InstanceGroupIDs(instanceID uint) ([]uint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]uint(nil), m.igroups[instanceID]...), nil
}

func (m *memRepo) UserInstanceIDs(userID uint) (map[uint]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uint]bool{}
	for _, g := range m.ugroups[userID] {
		for inst, gs := range m.igroups {
			for _, x := range gs {
				if x == g {
					out[inst] = true
				}
			}
		}
	}
	return out, nil
}

func (m *memRepo) ListInstances(dbTypes []string) ([]InstanceBrief, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []InstanceBrief
	for _, in := range m.insts {
		if contains(dbTypes, in.DBType) {
			out = append(out, InstanceBrief{ID: in.ID, Name: in.InstanceName, DBType: in.DBType})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

func (m *memRepo) GetInstance(id uint) (*model.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if in := m.insts[id]; in != nil {
		c := *in
		return &c, nil
	}
	return nil, nil
}

func (m *memRepo) EngineConn(*model.Instance) engine.Conn { return engine.Conn{} }

func (m *memRepo) DatabaseOwner(instanceID uint, db string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.owners[fmt.Sprintf("%d:%s", instanceID, db)], nil
}

func (m *memRepo) DBALoads(usernames []string) ([]UserLoad, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []UserLoad
	for _, u := range m.users {
		if u.Active && contains(usernames, u.Username) {
			n := 0
			for _, i := range m.issues {
				if i.DBAID != nil && *i.DBAID == u.ID && isOpen(i.Status) {
					n++
				}
			}
			out = append(out, UserLoad{User: u, Open: n})
		}
	}
	return out, nil
}

func (m *memRepo) ConfigValue(item string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.cfg[item]
	return v, ok && strings.TrimSpace(v) != ""
}

func (m *memRepo) InsertAudit(e *model.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAudit {
		return errors.New("audit down")
	}
	m.audits = append(m.audits, *e)
	return nil
}

func (m *memRepo) auditActions() []string {
	var out []string
	for _, a := range m.audits {
		out = append(out, a.Action)
	}
	return out
}

// ---- 慢日志 ----

func (m *memRepo) SlowLogsAfter(instanceID, afterID uint, limit int) ([]SlowLogRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SlowLogRow
	for _, r := range m.slowRows {
		if m.slowInst[r.ID] == instanceID && r.ID > afterID && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memRepo) SlowLogsCreatedBetween(instanceID uint, start, end time.Time, limit int) ([]SlowLogRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SlowLogRow
	for _, r := range m.slowRows {
		if m.slowInst[r.ID] == instanceID && r.StartTime != nil && !r.StartTime.Before(start) && r.StartTime.Before(end) && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memRepo) MaxSlowLogID(instanceID uint, before time.Time) (uint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var mx uint
	for _, r := range m.slowRows {
		if m.slowInst[r.ID] == instanceID && r.StartTime != nil && r.StartTime.Before(before) && r.ID > mx {
			mx = r.ID
		}
	}
	return mx, nil
}

func (m *memRepo) PtDigests(string, time.Time, time.Time) ([]Digest, error) {
	return append([]Digest(nil), m.pt...), m.ptErr
}

func (m *memRepo) SlowLogByMD5(instanceID uint, md5, db string, since time.Time) (*SlowSample, []Digest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := fmt.Sprintf("%d|%s", instanceID, md5)
	if s := m.slowMD5[k]; s != nil {
		return s, m.slowDays[k], nil
	}
	return nil, nil, nil
}
