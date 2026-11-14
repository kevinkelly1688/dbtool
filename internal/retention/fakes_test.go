// @author: kevinkelly
// @license: Apache Licence
// @file: fakes_test.go
// @time: 2026/10/28

package retention

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/model"
)

var baseNow = time.Date(2026, 10, 9, 3, 0, 0, 0, time.Local)

func daysAgo(n int) time.Time { return baseNow.AddDate(0, 0, -n) }

type fakeRow struct {
	ID   uint64
	T    time.Time
	Keep bool // 被固定条件排除（如收藏的查询、进行中的运行）
}

type batchCall struct {
	Table string
	Limit int
	Sel   int
	Del   int64
}

// fakeRepo 是内存 Repo：表是 map，DeleteBatch 按“早于 cutoff 且不被固定条件排除”的规则取最小的 limit 个 id。
type fakeRepo struct {
	mu       sync.Mutex
	cfg      map[string]string
	tables   map[string]*[]fakeRow // 不在 map 里 = 表不存在
	missing  map[string][]string   // 表 -> 缺的列
	childOf  map[string][]uint64   // 子表 -> 被连带删除的 id
	calls    []batchCall
	failOn   map[string]error // 表 -> DeleteBatch 返回的错误
	lockBusy bool
	lockErr  error
	locks    int

	audits []model.AuditEntry
	rounds []RoundRecord

	// 导出
	markerReady bool
	exports     []ExportFile
	purged      map[uint]ExportPurge
	markErr     error

	// 备份
	stores    []BackupStore
	runs      []BackupRun
	marked    map[uint]string
	markBkErr error
}

func newFake() *fakeRepo {
	return &fakeRepo{cfg: map[string]string{}, tables: map[string]*[]fakeRow{}, missing: map[string][]string{}, childOf: map[string][]uint64{},
		failOn: map[string]error{}, purged: map[uint]ExportPurge{}, marked: map[uint]string{}}
}

func (f *fakeRepo) set(k, v string) { f.cfg[k] = v }

// table 建表并填入行：ages 为每行的“几天前”。
func (f *fakeRepo) table(name string, ages ...int) *[]fakeRow {
	rows := make([]fakeRow, len(ages))
	for i, a := range ages {
		rows[i] = fakeRow{ID: uint64(i + 1), T: daysAgo(a)}
	}
	f.tables[name] = &rows
	return &rows
}

func (f *fakeRepo) ConfigValue(item string) (string, bool) {
	v, ok := f.cfg[item]
	return v, ok && v != ""
}

func (f *fakeRepo) TableState(table string, cols ...string) (TableState, error) {
	if _, ok := f.tables[table]; !ok {
		if _, ok2 := f.missing[table]; !ok2 && !f.extraTable(table) {
			return TableState{}, nil
		}
	}
	st := TableState{Exists: true}
	for _, c := range f.missing[table] {
		for _, w := range cols {
			if w == c {
				st.Missing = append(st.Missing, c)
			}
		}
	}
	return st, nil
}

// extraTable 是除目标表之外“存在”的表（子表、依赖表、归档表）。
func (f *fakeRepo) extraTable(name string) bool {
	switch name {
	case "dbt_audit_ext", "dbt_archive_event", "dbt_archive_batch_stat", "dbt_archive_task", "dbt_archive_run", "dbt_archive_store", "dbt_retention_run":
		return f.cfg["_no_"+name] == ""
	}
	return false
}

func (f *fakeRepo) CountOlder(_ context.Context, t Target, cutoff time.Time) (int64, error) {
	var n int64
	for _, r := range *f.tables[t.Table] {
		if r.T.Before(cutoff) && !r.Keep {
			n++
		}
	}
	return n, nil
}

func (f *fakeRepo) DeleteBatch(_ context.Context, t Target, cutoff time.Time, limit int) (int, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn[t.Table]; err != nil {
		return 0, 0, err
	}
	rows := f.tables[t.Table]
	var ids []uint64
	for _, r := range *rows {
		if r.T.Before(cutoff) && !r.Keep {
			ids = append(ids, r.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	del := map[uint64]bool{}
	for _, id := range ids {
		del[id] = true
	}
	kept := (*rows)[:0:0]
	for _, r := range *rows {
		if !del[r.ID] {
			kept = append(kept, r)
		}
	}
	*rows = kept
	for _, c := range t.Children {
		f.childOf[c.Table] = append(f.childOf[c.Table], ids...)
	}
	f.calls = append(f.calls, batchCall{Table: t.Table, Limit: limit, Sel: len(ids), Del: int64(len(ids))})
	return len(ids), int64(len(ids)), nil
}

func (f *fakeRepo) TryLock(context.Context, string) (func(), bool, error) {
	if f.lockErr != nil {
		return nil, false, f.lockErr
	}
	if f.lockBusy {
		return nil, false, nil
	}
	f.locks++
	return func() {}, true, nil
}

func (f *fakeRepo) ExportMarkerReady() (bool, error) { return f.markerReady, nil }

func (f *fakeRepo) ExpiredExports(_ context.Context, cutoff time.Time, afterID uint, limit int) ([]ExportFile, error) {
	var out []ExportFile
	for _, e := range f.exports {
		if e.WorkflowID <= afterID || !e.RefTime.Before(cutoff) {
			continue
		}
		if _, done := f.purged[e.WorkflowID]; done {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkflowID < out[j].WorkflowID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRepo) MarkExportPurged(_ context.Context, p ExportPurge) error {
	if f.markErr != nil {
		return f.markErr
	}
	f.purged[p.WorkflowID] = p
	return nil
}

func (f *fakeRepo) BackupStores(context.Context) ([]BackupStore, error) { return f.stores, nil }

func (f *fakeRepo) BackupRuns(_ context.Context, before time.Time, afterID uint, limit int) ([]BackupRun, error) {
	var out []BackupRun
	for _, r := range f.runs {
		if r.ID <= afterID || !r.RefTime.Before(before) {
			continue
		}
		if _, done := f.marked[r.ID]; done {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRepo) MarkBackupPurged(_ context.Context, id uint, note string) error {
	if f.markBkErr != nil {
		return f.markBkErr
	}
	f.marked[id] = note
	return nil
}

func (f *fakeRepo) InsertAudit(e *model.AuditEntry) error {
	f.audits = append(f.audits, *e)
	return nil
}

func (f *fakeRepo) SaveRound(r *RoundRecord) error {
	f.rounds = append(f.rounds, *r)
	return nil
}

func (f *fakeRepo) ListRounds(limit int) ([]RoundRecord, error) { return f.rounds, nil }

// ---- 假 FileStore ----

type fakeFS struct {
	mu      sync.Mutex
	objects map[string]bool
	deleted []string
	failKey string
}

func newFakeFS(keys ...string) *fakeFS {
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return &fakeFS{objects: m}
}

func (f *fakeFS) Put(context.Context, string, io.Reader, int64) error { return errors.New("not used") }
func (f *fakeFS) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}
func (f *fakeFS) Stat(context.Context, string) (engine.FileInfo, error) {
	return engine.FileInfo{}, errors.New("not used")
}
func (f *fakeFS) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key == f.failKey {
		return fmt.Errorf("拒绝删除 %s", key)
	}
	delete(f.objects, key)
	f.deleted = append(f.deleted, key)
	return nil
}
func (f *fakeFS) Test(context.Context) (time.Duration, error) { return 0, nil }
func (f *fakeFS) Free(context.Context) (int64, bool, error)   { return 0, false, nil }

// newTestService 返回时钟固定的服务；sleep 记录每次休眠并可推进时钟。
func newTestService(t *testing.T, repo *fakeRepo) (*Service, *[]time.Duration, *time.Time) {
	t.Helper()
	clock := baseNow
	var sleeps []time.Duration
	s := NewService(repo, t.TempDir())
	s.now = func() time.Time { return clock }
	s.sleep = func(_ context.Context, d time.Duration) { sleeps = append(sleeps, d) }
	return s, &sleeps, &clock
}
