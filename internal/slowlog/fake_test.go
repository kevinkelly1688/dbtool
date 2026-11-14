// @author: kevinkelly
// @license: Apache Licence
// @file: fake_test.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"errors"
	"sync"

	"gorm.io/gorm"

	"dbtool/internal/btnperm"
	"dbtool/internal/model"
)

// fakeRepo 是 Repo 的内存假实现：记录收到的查询，结果由测试直接设置。
type fakeRepo struct {
	mu sync.Mutex

	instances []model.Instance
	perms     map[uint]map[string]bool // 用户 ID -> 权限码
	userInsts map[uint]map[uint]bool   // 用户 ID -> 可见实例
	digestOK  bool
	// 表存在但历史表里没有这些实例（host:port）的数据：模拟“基础表已建、没部署 pt-query-digest 推送脚本”
	digestNoHost map[string]bool
	audits       []model.AuditEntry

	groupRows    []SlowLogGroup
	groupTotal   int64
	lastGroup    *GroupQuery
	groupCalls   int
	detailRows   []SlowLogRow
	lastDetail   *DetailQuery
	collReview   []ReviewRow
	digReview    []ReviewRow
	lastReview   *ReviewQuery
	collHist     []HistoryRow
	digHist      []HistoryRow
	lastHist     *ReviewQuery
	trend        *Trend
	lastTrend    *TrendQuery
	lastTrendSrc string
	dbs          []string
	lastDigDBs   string
	err          error // 非空时数据查询返回它

	// 采集器
	slowLogs  map[uint][]Entry // 已写入的行
	states    map[uint]CollectState
	stateOK   bool
	saveErr   error
	insertErr error
	watermark map[uint]int64
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{perms: map[uint]map[string]bool{}, userInsts: map[uint]map[uint]bool{},
		slowLogs: map[uint][]Entry{}, states: map[uint]CollectState{}, stateOK: true, watermark: map[uint]int64{}}
}

func (r *fakeRepo) grant(uid uint, codes ...string) {
	if r.perms[uid] == nil {
		r.perms[uid] = map[string]bool{}
	}
	for _, c := range codes {
		r.perms[uid][c] = true
	}
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
func (r *fakeRepo) HasButton(uid uint, code string) (bool, error) {
	return r.perms[uid][code] || btnperm.IsOpen(code), nil
}
func (r *fakeRepo) UserInstanceIDs(uid uint) (map[uint]bool, error) { return r.userInsts[uid], nil }
func (r *fakeRepo) InsertAudit(e *model.AuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits = append(r.audits, *e)
	return nil
}

func (r *fakeRepo) SlowLogGroups(q GroupQuery) ([]SlowLogGroup, int64, error) {
	r.groupCalls++
	r.lastGroup = &q
	return r.groupRows, r.groupTotal, r.err
}
func (r *fakeRepo) SlowLogDetail(q DetailQuery) ([]SlowLogRow, error) {
	r.lastDetail = &q
	return r.detailRows, r.err
}
func (r *fakeRepo) CollectedReview(q ReviewQuery) ([]ReviewRow, int64, error) {
	r.lastReview = &q
	return r.collReview, int64(len(r.collReview)), r.err
}
func (r *fakeRepo) CollectedHistory(q ReviewQuery) ([]HistoryRow, int64, error) {
	r.lastHist = &q
	return r.collHist, int64(len(r.collHist)), r.err
}
func (r *fakeRepo) CollectedTrend(q TrendQuery) (*Trend, error) {
	r.lastTrend, r.lastTrendSrc = &q, SourceCollected
	t := r.trend
	if t == nil {
		t = &Trend{}
	}
	return t, r.err
}
func (r *fakeRepo) CollectedDatabases(uint, string, string) ([]string, error) { return r.dbs, r.err }
func (r *fakeRepo) DigestAvailable() (bool, error)                            { return r.digestOK, nil }
func (r *fakeRepo) DigestHasHost(hostKey string) (bool, error) {
	return r.digestOK && !r.digestNoHost[hostKey], nil
}
func (r *fakeRepo) DigestReview(q ReviewQuery) ([]ReviewRow, int64, error) {
	r.lastReview = &q
	return r.digReview, int64(len(r.digReview)), r.err
}
func (r *fakeRepo) DigestHistory(q ReviewQuery) ([]HistoryRow, int64, error) {
	r.lastHist = &q
	return r.digHist, int64(len(r.digHist)), r.err
}
func (r *fakeRepo) DigestTrend(q TrendQuery) (*Trend, error) {
	r.lastTrend, r.lastTrendSrc = &q, SourceDigest
	t := r.trend
	if t == nil {
		t = &Trend{}
	}
	return t, r.err
}
func (r *fakeRepo) DigestDatabases(host, _, _ string) ([]string, error) {
	r.lastDigDBs = host
	return r.dbs, r.err
}

// ---- CollectRepo ----

func (r *fakeRepo) CollectInstances() ([]model.Instance, error) {
	var out []model.Instance
	for _, in := range r.instances {
		if in.IsEnable && collectTypes[in.DBType] {
			out = append(out, in)
		}
	}
	return out, nil
}

func (r *fakeRepo) Watermark(id uint) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.watermark[id]
	for _, e := range r.slowLogs[id] {
		if e.Micro > m {
			m = e.Micro
		}
	}
	return m, nil
}

func (r *fakeRepo) ExistingKeys(id uint, lo, hi int64) (map[string]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for _, e := range r.slowLogs[id] {
		if e.Micro >= lo && e.Micro <= hi {
			out[entryKey(e.Micro, e.SQLMD5, e.UserHost)] = true
		}
	}
	return out, nil
}

func (r *fakeRepo) InsertSlowLogs(id uint, rows []Entry) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.insertErr != nil {
		return 0, r.insertErr
	}
	r.slowLogs[id] = append(r.slowLogs[id], rows...)
	return len(rows), nil
}

func (r *fakeRepo) SaveState(st CollectState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveErr != nil {
		return r.saveErr
	}
	r.states[st.InstanceID] = st
	return nil
}

func (r *fakeRepo) LoadStates() (map[uint]CollectState, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.stateOK {
		return nil, false, nil
	}
	out := map[uint]CollectState{}
	for k, v := range r.states {
		out[k] = v
	}
	return out, true, nil
}

// ---- 假 Source / Advisor ----

// fakeSource 按实例返回预置的行（只返回游标大于 after 的，最多 limit 条），并可注入错误、panic、阻塞。
type fakeSource struct {
	mu      sync.Mutex
	rows    map[uint][]Entry
	errs    map[uint]error
	panics  map[uint]bool
	calls   map[uint][]int64 // 每次调用收到的 after
	block   chan struct{}    // 非空时 Fetch 先等待它被关闭
	running int
	maxRun  int
}

func newFakeSource() *fakeSource {
	return &fakeSource{rows: map[uint][]Entry{}, errs: map[uint]error{}, panics: map[uint]bool{}, calls: map[uint][]int64{}}
}

func (s *fakeSource) Fetch(ctx context.Context, inst *model.Instance, after int64, limit int) ([]Entry, error) {
	s.mu.Lock()
	s.calls[inst.ID] = append(s.calls[inst.ID], after)
	s.running++
	if s.running > s.maxRun {
		s.maxRun = s.running
	}
	block := s.block
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.running--; s.mu.Unlock() }()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.panics[inst.ID] {
		panic("boom")
	}
	if err := s.errs[inst.ID]; err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range s.rows[inst.ID] {
		if e.Micro > after {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

type fakeAdvisor struct {
	req *AdviseRequest
	adv *Advice
	err error
}

func (a *fakeAdvisor) Advise(_ context.Context, req AdviseRequest) (*Advice, error) {
	a.req = &req
	return a.adv, a.err
}

var errBoom = errors.New("connection refused")
