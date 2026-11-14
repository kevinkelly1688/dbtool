// @author: kevinkelly
// @license: Apache Licence
// @file: dest.go
// @time: 2026/10/28

package enginetest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"dbtool/internal/archive/engine"
)

type destTable struct {
	info    engine.TableInfo
	numeric []bool
	pkIdx   []int
	rows    map[string]engine.Row // 主键(逗号连接) -> 行
}

// MemDest 是内存里的库表备份去向。
type MemDest struct {
	mu    sync.Mutex
	Log   *OpLog
	Clock *Clock

	Databases map[string]bool
	tables    map[string]*destTable

	// 钩子（call 从 1 开始计数）
	OnEnsure     func(db, table string) error
	OnInsert     func(call int, rows []engine.Row) error
	OnVerify     func(call int) error
	VerifyAdjust func(call int, found int64) int64 // 改写 VerifyPKs 的返回值，模拟回读不一致
	InsertCost   func(call int) time.Duration
	DiskPct      float64
	DiskOK       bool

	Ensures, Inserts, Verifies int
}

func NewMemDest(log *OpLog, clock *Clock, databases ...string) *MemDest {
	d := &MemDest{Log: log, Clock: clock, Databases: map[string]bool{}, tables: map[string]*destTable{}, DiskPct: 80, DiskOK: true}
	for _, db := range databases {
		d.Databases[db] = true
	}
	return d
}

func (d *MemDest) key(db, table string) string { return db + "." + table }

func (d *MemDest) newTable(db, table string, info engine.TableInfo) *destTable {
	t := &destTable{info: info, rows: map[string]engine.Row{}}
	t.info.DB, t.info.Name, t.info.Exists = db, table, true
	for _, p := range info.PrimaryKey {
		for i, c := range info.Columns {
			if strings.EqualFold(c.Name, p) {
				t.pkIdx = append(t.pkIdx, i)
				t.numeric = append(t.numeric, isNumericType(c.Type))
			}
		}
	}
	return t
}

// PreCreate 预先建好目标表（模拟“目标表已存在”）。
func (d *MemDest) PreCreate(db, table string, like engine.TableInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Databases[db] = true
	d.tables[d.key(db, table)] = d.newTable(db, table, like)
}

func (d *MemDest) pkOf(t *destTable, r engine.Row) []string {
	out := make([]string, len(t.pkIdx))
	for i, ix := range t.pkIdx {
		out[i] = pkStr(r[ix])
	}
	return out
}

func (d *MemDest) EnsureTable(ctx context.Context, db, table string, src engine.TableInfo) error {
	d.mu.Lock()
	d.Ensures++
	hook := d.OnEnsure
	d.mu.Unlock()
	if hook != nil {
		if err := hook(db, table); err != nil {
			return err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.Databases[db] {
		return fmt.Errorf("目标库 %s 不存在", db)
	}
	if t, ok := d.tables[d.key(db, table)]; ok {
		have := map[string]bool{}
		for _, c := range t.info.Columns {
			have[strings.ToLower(c.Name)] = true
		}
		for _, c := range src.Columns {
			if !have[strings.ToLower(c.Name)] {
				return fmt.Errorf("目标表缺少列 %s，结构不兼容", c.Name)
			}
		}
		return nil
	}
	d.tables[d.key(db, table)] = d.newTable(db, table, src)
	d.Log.add("dst.ensure", nil, 0)
	return nil
}

func rowSig(r engine.Row) string {
	parts := make([]string, len(r))
	for i, v := range r {
		parts[i] = pkStr(v)
		if v == nil {
			parts[i] = "\x00NULL"
		}
	}
	return strings.Join(parts, "\x1f")
}

func (d *MemDest) Insert(ctx context.Context, db, table string, cols []string, rows []engine.Row, pkCols []string) (int64, int64, error) {
	d.mu.Lock()
	d.Inserts++
	call := d.Inserts
	hook, cost := d.OnInsert, d.InsertCost
	d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if hook != nil {
		if err := hook(call, rows); err != nil {
			return 0, 0, err
		}
	}
	if cost != nil && d.Clock != nil {
		d.Clock.Advance(cost(call))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.tables[d.key(db, table)]
	if t == nil {
		return 0, 0, fmt.Errorf("目标表 %s.%s 不存在", db, table)
	}
	// 先全部检查（事务语义：要么全成功，要么全不写）
	var fresh []engine.Row
	var existed int64
	for _, r := range rows {
		k := strings.Join(d.pkOf(t, r), ",")
		if old, ok := t.rows[k]; ok {
			if rowSig(old) != rowSig(r) {
				return 0, 0, fmt.Errorf("%w：目标表已有主键 %s 但内容不一致", engine.ErrMismatch, k)
			}
			existed++
			continue
		}
		fresh = append(fresh, r)
	}
	var pks [][]string
	for _, r := range fresh {
		pk := d.pkOf(t, r)
		t.rows[strings.Join(pk, ",")] = append(engine.Row(nil), r...)
		pks = append(pks, pk)
	}
	d.Log.add("dst.insert", pks, len(fresh))
	return int64(len(fresh)), existed, nil
}

func (d *MemDest) VerifyPKs(ctx context.Context, db, table string, pkCols []string, pks [][]string) (int64, error) {
	d.mu.Lock()
	d.Verifies++
	call := d.Verifies
	hook, adj := d.OnVerify, d.VerifyAdjust
	d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if hook != nil {
		if err := hook(call); err != nil {
			return 0, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.tables[d.key(db, table)]
	if t == nil {
		return 0, fmt.Errorf("目标表 %s.%s 不存在", db, table)
	}
	var found int64
	var ok [][]string
	for _, pk := range pks {
		if _, has := t.rows[strings.Join(pk, ",")]; has {
			found++
			ok = append(ok, pk)
		}
	}
	if adj != nil {
		found = adj(call, found)
		if found < int64(len(ok)) { // 被改写成“查到更少”时，日志里只记录真正校验到的前 found 个
			ok = ok[:found]
		}
	}
	d.Log.add("dst.verify", ok, int(found))
	return found, nil
}

func (d *MemDest) DiskFreePct(ctx context.Context) (float64, bool, error) {
	return d.DiskPct, d.DiskOK, nil
}

func (d *MemDest) Describe(ctx context.Context, db, table string) (engine.TableInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t := d.tables[d.key(db, table)]; t != nil {
		info := t.info
		info.Rows = int64(len(t.rows))
		return info, nil
	}
	return engine.TableInfo{DB: db, Name: table}, nil
}

func (d *MemDest) DBExists(ctx context.Context, db string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.Databases[db], nil
}

func (d *MemDest) ReadRange(ctx context.Context, req engine.RangeReq) (engine.Batch, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.tables[d.key(req.DB, req.Table)]
	if t == nil {
		return engine.Batch{}, fmt.Errorf("备份表 %s.%s 不存在", req.DB, req.Table)
	}
	type kv struct {
		pk  []string
		row engine.Row
	}
	var all []kv
	for _, r := range t.rows {
		pk := d.pkOf(t, r)
		if ComparePK(t.numeric, pk, req.Min) < 0 || ComparePK(t.numeric, pk, req.Max) > 0 {
			continue
		}
		if len(req.After) > 0 && ComparePK(t.numeric, pk, req.After) <= 0 {
			continue
		}
		all = append(all, kv{pk, r})
	}
	sort.Slice(all, func(i, j int) bool { return ComparePK(t.numeric, all[i].pk, all[j].pk) < 0 })
	b := engine.Batch{}
	for _, c := range t.info.Columns {
		b.Cols = append(b.Cols, c.Name)
	}
	for _, e := range all {
		if len(b.Rows) >= req.Limit {
			break
		}
		b.Rows = append(b.Rows, append(engine.Row(nil), e.row...))
		b.PKs = append(b.PKs, e.pk)
	}
	return b, nil
}

func (d *MemDest) Close() error { return nil }

// Table 返回目标表里的所有行（按主键排序）；表不存在返回 nil。
func (d *MemDest) Table(db, table string) []engine.Row {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.tables[d.key(db, table)]
	if t == nil {
		return nil
	}
	rows := make([]engine.Row, 0, len(t.rows))
	for _, r := range t.rows {
		rows = append(rows, append(engine.Row(nil), r...))
	}
	sort.Slice(rows, func(i, j int) bool { return ComparePK(t.numeric, d.pkOf(t, rows[i]), d.pkOf(t, rows[j])) < 0 })
	return rows
}

// Exists 判断目标表是否存在。
func (d *MemDest) Exists(db, table string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tables[d.key(db, table)] != nil
}

// Tamper 直接改目标表里某行的某列（模拟“已有主键相同但内容不一致的行”）。
func (d *MemDest) Tamper(db, table, pk string, col int, v any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t := d.tables[d.key(db, table)]; t != nil {
		if r, ok := t.rows[pk]; ok {
			r[col] = v
		}
	}
}

// Remove 直接从目标表删一行（模拟备份丢失）。
func (d *MemDest) Remove(db, table, pk string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t := d.tables[d.key(db, table)]; t != nil {
		delete(t.rows, pk)
	}
}

var _ engine.DestDB = (*MemDest)(nil)
